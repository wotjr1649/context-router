package exec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/sandbox"
)

// testSelfExe: 실 ctr 바이너리를 1회만 빌드해(sync.Once) 재사용한다 — transform 패키지의
// worker_test.go 선례를 이 패키지에 복제한다(패키지별 복제 관례). Linux sandbox.Run은
// selfExe로 자기 재실행(__exec-launcher)하므로 테스트 바이너리(os.Executable)를 쓰면
// 자기재실행 재귀가 된다 — 실 바이너리를 넘겨야 한다.
var (
	testExeOnce sync.Once
	testExePath string
	testExeErr  error
)

func testSelfExe(t *testing.T) string {
	t.Helper()
	testExeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ctr-exec-test-*")
		if err != nil {
			testExeErr = err
			return
		}
		bin := filepath.Join(dir, "ctr-test")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "github.com/wotjr1649/context-router/cmd/context-router")
		if out, err := cmd.CombinedOutput(); err != nil {
			testExeErr = &buildErr{err: err, out: string(out)}
			return
		}
		testExePath = bin
	})
	if testExeErr != nil {
		t.Fatalf("selfExe 빌드 실패: %v", testExeErr)
	}
	return testExePath
}

type buildErr struct {
	err error
	out string
}

func (e *buildErr) Error() string { return e.err.Error() + ": " + e.out }

func TestMain(m *testing.M) {
	code := m.Run()
	if testExePath != "" {
		os.RemoveAll(filepath.Dir(testExePath))
	}
	os.Exit(code)
}

func selfExe(t *testing.T) string { return testSelfExe(t) }

func TestRunShellEcho(t *testing.T) {
	if _, err := exec.LookPath(shellName()); err != nil {
		t.Skip("shell 미설치")
	}
	resp, err := Run(context.Background(), t.TempDir(), selfExe(t),
		Request{Language: "shell", Code: "echo hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.Stdout, "hello") {
		t.Fatalf("stdout=%q", resp.Stdout)
	}
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("exit=%v", resp.ExitCode)
	}
	if resp.Runner == "" {
		t.Fatalf("runner 미기록")
	}
}

// TestSnippetPS1BOM: I2 — .ps1 스니펫은 선두에 UTF-8 BOM(EF BB BF)이 붙어야 powershell.exe
// 5.1이 비ASCII를 ANSI 코드페이지로 오독하지 않는다. 다른 확장자는 BOM이 붙지 않는다.
func TestSnippetPS1BOM(t *testing.T) {
	got := snippetContent("snippet.ps1", "Write-Output '한글'")
	if !bytes.HasPrefix(got, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf(".ps1 UTF-8 BOM 미기록: 선두=%x", got[:min(3, len(got))])
	}
	if !bytes.HasSuffix(got, []byte("Write-Output '한글'")) {
		t.Fatalf("BOM 뒤 코드 원문 손상")
	}
	for _, f := range []string{"snippet.sh", "snippet.js", "snippet.py", "snippet.go", "snippet.cs"} {
		if bytes.HasPrefix(snippetContent(f, "x"), []byte{0xEF, 0xBB, 0xBF}) {
			t.Errorf("%s에 BOM 오부착", f)
		}
	}
}

func TestUnsupportedLang(t *testing.T) {
	const lang = "brainfuck-마커-XYZ" // 원문 에코 여부를 구분할 유일 표식
	_, err := Run(context.Background(), t.TempDir(), selfExe(t),
		Request{Language: lang, Code: "+"})
	if err == nil || !strings.Contains(err.Error(), "지원") {
		t.Fatalf("미지원 언어 오류 아님: %v", err)
	}
	// I3: 오류는 sentinel만 담고 사용자 입력 원문을 에코하지 않는다(오류 규칙·로그 인젝션).
	if !errors.Is(err, ErrUnsupportedLang) {
		t.Fatalf("sentinel(ErrUnsupportedLang) 아님: %v", err)
	}
	if strings.Contains(err.Error(), lang) {
		t.Fatalf("오류에 입력 원문 에코: %v", err)
	}
}

func TestClampTimeout(t *testing.T) {
	if clampTimeout(0).Milliseconds() != 120000 {
		t.Fatalf("기본 120s 아님")
	}
	if clampTimeout(9_000_000).Milliseconds() != 1_800_000 {
		t.Fatalf("상한 1800s 아님")
	}
}

// requireLang: 러너 미감지면 Skip(구속 제약 — toolchain 감지 실패 시 t.Skip).
func requireLang(t *testing.T, lang string) {
	t.Helper()
	if _, _, err := table()[lang].detect(); err != nil {
		t.Skipf("%s 러너 미감지: %v", lang, err)
	}
}

func run(t *testing.T, req Request) Response {
	t.Helper()
	resp, err := Run(context.Background(), t.TempDir(), selfExe(t), req)
	if err != nil {
		t.Fatalf("Run(%s): %v", req.Language, err)
	}
	return resp
}

// TestNodeAtLeast: TS 버전 게이트 판정 — 미달(≥22.7 미만)은 거부, 충족은 통과.
func TestNodeAtLeast(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"v22.7.0", true},
		{"v22.19.0", true},
		{"v23.0.0", true},
		{"v22.6.9", false},
		{"v20.11.1", false},
		{"v18.0.0", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := nodeAtLeast(c.v, 22, 7); got != c.want {
			t.Errorf("nodeAtLeast(%q)=%v want %v", c.v, got, c.want)
		}
	}
}

// TestDotnetHasMajor: C# 버전 게이트 판정 — SDK 목록에 major≥10이 있어야 통과.
func TestDotnetHasMajor(t *testing.T) {
	cases := []struct {
		list string
		want bool
	}{
		{"10.0.301 [C:\\Program Files\\dotnet\\sdk]\n", true},
		{"8.0.100 [x]\n9.0.100 [x]\n10.0.100 [x]\n", true},
		{"8.0.100 [x]\n9.0.100 [x]\n", false},
		{"", false},
	}
	for _, c := range cases {
		if got := dotnetHasMajor(c.list, 10); got != c.want {
			t.Errorf("dotnetHasMajor(%q)=%v want %v", c.list, got, c.want)
		}
	}
}

// TestGateCache: I1 — 프로브 자체 실패(definitive=false)는 캐시하지 않아 재프로브하고,
// 확정 결과(definitive=true)는 서버 수명 캐시해 재프로브하지 않는다.
func TestGateCache(t *testing.T) {
	var g gate
	calls := 0
	transient := func() (bool, error) { calls++; return false, ErrToolchainMissing }
	if err := g.do(transient); !errors.Is(err, ErrToolchainMissing) {
		t.Fatalf("transient 1차: %v", err)
	}
	if err := g.do(transient); !errors.Is(err, ErrToolchainMissing) {
		t.Fatalf("transient 2차: %v", err)
	}
	if calls != 2 {
		t.Fatalf("transient 캐시됨(재프로브 안 함): calls=%d want 2", calls)
	}
	definitive := func() (bool, error) { calls++; return true, ErrVersionGate }
	if err := g.do(definitive); !errors.Is(err, ErrVersionGate) {
		t.Fatalf("definitive 1차: %v", err)
	}
	before := calls
	if err := g.do(definitive); !errors.Is(err, ErrVersionGate) {
		t.Fatalf("definitive 2차: %v", err)
	}
	if calls != before {
		t.Fatalf("definitive 재프로브됨(캐시 미작동): calls=%d want %d", calls, before)
	}
}

// TestProbeVersionTimeout: I1 — 프로브에 시한이 걸려 웜지 않은(sleeping) 프로세스가 무기한
// 블록하지 않고 ok=false로 유한시간에 반환한다. 셸 미설치면 Skip(러너 의존).
func TestProbeVersionTimeout(t *testing.T) {
	sh, err := exec.LookPath(shellName())
	if err != nil {
		t.Skip("shell 미설치")
	}
	var args []string
	if runtime.GOOS == "windows" {
		args = []string{"-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 5"}
	} else {
		args = []string{"-c", "sleep 5"}
	}
	start := time.Now()
	if out, ok := probeVersion(sh, 100*time.Millisecond, args...); ok {
		t.Fatalf("시한 안에 안 끝났어야: out=%q", out)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("프로브 시한 미적용(5s sleep이 완주): %v", d)
	}
}

// TestCTRFileRoundtrip: execute_file 경로 — FilePath가 CTR_FILE로 노출되어 스니펫이 읽는다.
func TestCTRFileRoundtrip(t *testing.T) {
	requireLang(t, "javascript")
	resp := run(t, Request{
		Language: "javascript",
		Code:     "console.log(process.env.CTR_FILE)",
		FilePath: "roundtrip-marker.txt",
	})
	if !strings.Contains(resp.Stdout, "roundtrip-marker.txt") {
		t.Fatalf("CTR_FILE 미왕복: stdout=%q", resp.Stdout)
	}
}

// TestClosedEnvTable: env 닫힌 표 — 부모에 심은 표 밖 표식 변수는 스니펫 환경에 나타나지
// 않고, 의도 주입(CTR_SCRATCH)은 나타난다(env 배선이 죽어 빈 게 아님을 함께 확인).
func TestClosedEnvTable(t *testing.T) {
	requireLang(t, "javascript")
	t.Setenv("CTR_LEAK_MARKER_XYZ", "should-not-propagate")
	resp := run(t, Request{
		Language: "javascript",
		Code:     "console.log(Object.keys(process.env).join('\\n'))",
	})
	if strings.Contains(resp.Stdout, "CTR_LEAK_MARKER_XYZ") {
		t.Fatalf("표 밖 변수가 스니펫 환경에 누출: %q", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "CTR_SCRATCH") {
		t.Fatalf("의도 주입 CTR_SCRATCH 부재(env 배선 죽음 의심): %q", resp.Stdout)
	}
}

// TestNonZeroExit: 비 0 종료 코드가 그대로 회수되고 timed_out이 아니다.
func TestNonZeroExit(t *testing.T) {
	requireLang(t, "javascript")
	resp := run(t, Request{Language: "javascript", Code: "process.exit(3)"})
	if resp.TimedOut {
		t.Fatalf("timed_out이면 안 됨")
	}
	if resp.ExitCode == nil || *resp.ExitCode != 3 {
		t.Fatalf("exit=%v want 3", resp.ExitCode)
	}
}

// TestTruncation: stdout이 상한(32768)을 넘으면 잘리고 StdoutTrunc가 선다.
func TestTruncation(t *testing.T) {
	requireLang(t, "javascript")
	resp := run(t, Request{
		Language: "javascript",
		Code:     "process.stdout.write('x'.repeat(40000))",
	})
	if !resp.StdoutTrunc {
		t.Fatalf("StdoutTrunc 미설정(len=%d)", len(resp.Stdout))
	}
	if len(resp.Stdout) != stdoutCap {
		t.Fatalf("len(stdout)=%d want %d", len(resp.Stdout), stdoutCap)
	}
}

// TestTimeout: 짧은 타임아웃 안에 안 끝나면 트리킬되어 TimedOut=true·ExitCode=nil.
func TestTimeout(t *testing.T) {
	requireLang(t, "shell")
	code := "sleep 30"
	if runtime.GOOS == "windows" {
		code = "Start-Sleep -Seconds 30"
	}
	resp := run(t, Request{Language: "shell", Code: code, TimeoutMS: 800})
	if !resp.TimedOut {
		t.Fatalf("TimedOut 미설정")
	}
	if resp.ExitCode != nil {
		t.Fatalf("timed_out인데 ExitCode 비-nil: %v", *resp.ExitCode)
	}
	if resp.DurationMS > 10_000 {
		t.Fatalf("트리킬 지연 의심: %dms", resp.DurationMS)
	}
}

// TestRunGo: go 골든 + cold-cache 재지정 실증 — 매 실행 새 스크래치의 빈 GOCACHE로 실제
// 컴파일이 성공하고(재지정 env가 살아있음), 스니펫이 읽은 GOCACHE가 스크래치 하위를 가리킨다.
func TestRunGo(t *testing.T) {
	requireLang(t, "go")
	resp := run(t, Request{
		Language: "go",
		Code:     "package main\nimport \"os\"\nfunc main() { os.Stdout.WriteString(os.Getenv(\"GOCACHE\") + \"\\ngo-ok\\n\") }\n",
	})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "go-ok") {
		t.Fatalf("골든 출력 부재: %q", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "go-build") {
		t.Fatalf("GOCACHE 재지정 미확인(스크래치 하위 아님): %q", resp.Stdout)
	}
}

// TestGoEnvIsolatesGOENV: go env 파일 경로가 스크래치 안으로 고정되고 그 파일이 실재한다.
// 호스트 go env 파일이 GOFLAGS·GOPROXY·GOTOOLCHAIN을 공급하지 못하게 하는 계약이다.
func TestGoEnvIsolatesGOENV(t *testing.T) {
	scratch := t.TempDir()
	m := map[string]string{}
	for _, kv := range goEnv(scratch) {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	want := filepath.Join(scratch, "go-env")
	if got := m["GOENV"]; got != want {
		t.Errorf("GOENV=%q want %q", got, want)
	}
	// go는 GOENV 파일을 스스로 만들지 않으므로 사전 생성한다(GOTMPDIR 사전 생성과 같은 규율).
	// 빈 파일이면 "설정 없음"이 아니라 "go 내장 기본"이 된다 — 공개 모듈 프록시와 툴체인
	// 자동 다운로드가 그 기본에 포함되므로, 의도한 값을 명시적으로 적는다.
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("GOENV 파일 미생성: %v", err)
	}
	for _, line := range []string{"GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off"} {
		if !strings.Contains(string(b), line) {
			t.Errorf("go env 파일에 %q 없음:\n%s", line, b)
		}
	}
}

// TestRunGoIgnoresHostGoEnv: 러너 안에서 관측한 GOENV가 스크래치 하위다(적용 실증).
// 파일 생성만이 아니라 go 툴체인이 실제로 그 경로를 쓰는지 확인한다.
func TestRunGoIgnoresHostGoEnv(t *testing.T) {
	requireLang(t, "go")
	resp := run(t, Request{
		Language: "go",
		Code: "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\n" +
			"func main() { fmt.Println(\"GOENV=\" + os.Getenv(\"GOENV\")) }\n",
	})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "go-env") {
		t.Errorf("GOENV가 스크래치 하위가 아니다: %q", resp.Stdout)
	}
}

// TestRunGoScratchGoEnvWins: 호스트 go env 파일이 실재하는 픽스처를 만들어 스크래치 구성이
// 이긴다는 것을 확인한다(D65 검증 계약). 양방향을 다 본다 — 격리를 뺀 절반은 호스트 파일의
// GOFLAGS가 툴체인까지 도달해 빌드가 깨지고(픽스처가 실효적이라는 증거), 격리를 넣은 절반은
// 같은 스니펫이 정상 종료한다. 한 방향만 보면 파일이 비었거나 도달하지 않아도 통과하는 공허한
// 테스트가 된다(TestRunGoIgnoresHostGoEnv는 전파만 고정하므로 이 테스트가 반영을 맡는다).
// 호스트 config 위치는 os.UserConfigDir() 규칙상 OS별로 달라 분기가 필요하다 — 자식 env
// allowlist가 통과시키는 포인터가 windows APPDATA · unix HOME이므로 unix는 HOME 경유로
// 고정한다(XDG_CONFIG_HOME은 allowlist 밖이라 자식이 못 본다). unix 실행은 3-OS CI가 검증한다.
func TestRunGoScratchGoEnvWins(t *testing.T) {
	requireLang(t, "go")
	// 픽스처를 심기 전에 sync.Once 빌드를 끝낸다 — 심은 뒤면 그 go build가 깨지고 실패가
	// testExeErr에 캐시돼 패키지 전체가 오염된다.
	selfExe(t)

	fake := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", fake)
	} else {
		t.Setenv("XDG_CONFIG_HOME", "") // 자식과 같은 해석 경로(HOME)로 통일
		t.Setenv("HOME", fake)
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	hostEnv := filepath.Join(cfgDir, "go", "env")
	if err := os.MkdirAll(filepath.Dir(hostEnv), 0o700); err != nil {
		t.Fatalf("호스트 config 디렉터리 생성 실패: %v", err)
	}
	// 반영되면 go가 플래그 파싱에서 즉시 죽는 값 — 관측이 종료코드로 갈리고 네트워크가 필요 없다.
	if err := os.WriteFile(hostEnv, []byte("GOFLAGS=-bogusflag\n"), 0o600); err != nil {
		t.Fatalf("호스트 go env 픽스처 기록 실패: %v", err)
	}

	code := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"go-ok\") }\n"

	// ① 격리 없음 — goEnv에서 GOENV만 뺀 환경으로 직접 실행한다(프로덕션에 토글을 심지 않기
	// 위해 이 절반만 테스트가 환경을 조립한다). 나머지 재지정은 그대로여서 차이는 GOENV뿐이다.
	scratch := t.TempDir()
	file := filepath.Join(scratch, "snippet.go")
	if err := os.WriteFile(file, []byte(code), 0o600); err != nil {
		t.Fatalf("스니펫 기록 실패: %v", err)
	}
	env := sandbox.BaseEnv() // 닫힌 표 그대로 — 개발자 셸의 GOFLAGS/GOPROXY가 실험을 흐리지 않게
	for _, kv := range goEnv(scratch) {
		if !strings.HasPrefix(kv, "GOENV=") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command("go", "run", file)
	cmd.Dir, cmd.Env = scratch, env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("픽스처 무효 — GOENV 없이도 성공했다(호스트 파일이 툴체인에 도달하지 않음): %s", out)
	}
	if !strings.Contains(string(out), "bogusflag") {
		t.Fatalf("호스트 GOFLAGS가 실패 원인이 아니다(다른 이유로 실패): %v: %s", err, out)
	}

	// ② 격리 있음 — 같은 픽스처·같은 스니펫이 러너에서 정상 종료한다.
	resp := run(t, Request{Language: "go", Code: code})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("스크래치 go env가 호스트에 지고 있다: timedout=%v stdout=%q stderr=%q",
			resp.TimedOut, resp.Stdout, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "go-ok") {
		t.Errorf("골든 출력 부재: %q", resp.Stdout)
	}
}

func TestRunPython(t *testing.T) {
	requireLang(t, "python")
	resp := run(t, Request{Language: "python", Code: "print('py-ok')"})
	if resp.ExitCode == nil || *resp.ExitCode != 0 || !strings.Contains(resp.Stdout, "py-ok") {
		t.Fatalf("exit=%v stdout=%q stderr=%q", resp.ExitCode, resp.Stdout, resp.Stderr)
	}
}

// TestPyEnvIsolatesUserSite: user site-packages와 pip 구성이 차단된다.
func TestPyEnvIsolatesUserSite(t *testing.T) {
	scratch := t.TempDir()
	m := map[string]string{}
	for _, kv := range pyEnv(scratch) {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	if m["PYTHONNOUSERSITE"] != "1" {
		t.Errorf("PYTHONNOUSERSITE 미주입")
	}
	want := filepath.Join(scratch, "pip.conf")
	if got := m["PIP_CONFIG_FILE"]; got != want {
		t.Errorf("PIP_CONFIG_FILE=%q want %q", got, want)
	}
	// 사전 생성이 pip 격리의 시행점이다 — pip은 PIP_CONFIG_FILE 경로가 실재할 때만 사용자
	// 구성을 로드 목록에서 뺀다(pyEnv 주석). 파일이 없으면 경로만 맞고 격리는 없다.
	if _, err := os.Stat(want); err != nil {
		t.Errorf("pip 구성 사전 생성 안 됨: %v", err)
	}
	if got := m["PYTHONPYCACHEPREFIX"]; got != filepath.Join(scratch, "pycache") {
		t.Errorf("기존 PYTHONPYCACHEPREFIX 계약이 깨졌다: %q", got)
	}
}

// TestRunPyIgnoresUserSite: 러너 안에서 user site가 비활성임을 실증한다.
func TestRunPyIgnoresUserSite(t *testing.T) {
	requireLang(t, "python")
	resp := run(t, Request{
		Language: "python",
		Code:     "import site\nprint('USER_SITE=' + str(site.ENABLE_USER_SITE))\n",
	})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "USER_SITE=False") {
		t.Errorf("user site가 비활성이 아니다: %q", resp.Stdout)
	}
}

// TestRunPyScratchUserSiteWins: 호스트 user site-packages에 임포트 가능한 모듈이 실재하는
// 픽스처를 만들어 격리가 이긴다는 것을 확인한다(D65 검증 계약). 양방향을 다 본다 — 격리를 뺀
// 절반은 그 모듈이 임포트되고(픽스처가 실효적이라는 증거), 넣은 절반은 같은 스니펫이
// ModuleNotFoundError로 죽는다. 한 방향만 보면 경로 계산이 틀려 아무도 안 보는 디렉터리에
// 심어도 통과하는 공허한 테스트가 된다(TestRunPyIgnoresUserSite는 플래그 관측만 맡는다).
// user site 경로 규칙은 플랫폼·버전에 달려 있으므로(windows %APPDATA%\Python\PythonXY,
// unix ~/.local/lib/pythonX.Y, macOS 프레임워크 빌드 ~/Library/Python/X.Y) 인터프리터에게 직접
// 묻는다. 자식 env 닫힌 표가 통과시키는 홈 포인터가 windows APPDATA · unix HOME이라 그쪽을
// 돌려 픽스처를 심는다. unix 실행은 3-OS CI가 검증한다.
func TestRunPyScratchUserSiteWins(t *testing.T) {
	requireLang(t, "python")
	// 픽스처를 심기 전에 sync.Once 빌드를 끝낸다 — 심은 뒤면 그 go build가 깨지고 실패가
	// testExeErr에 캐시돼 패키지 전체가 오염된다.
	selfExe(t)

	py, _, err := detectPy()
	if err != nil {
		t.Fatalf("python 미감지: %v", err)
	}
	fake := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", fake) // site._getuserbase(): nt는 %APPDATA%\Python
	} else {
		t.Setenv("HOME", fake)
	}
	probe := exec.Command(py, "-c", "import site; print(site.getusersitepackages())")
	probe.Env = sandbox.BaseEnv() // 자식과 같은 표로 물어야 자식이 볼 경로가 나온다
	out, err := probe.Output()
	if err != nil {
		t.Fatalf("user site 경로 조회 실패: %v", err)
	}
	userSite := strings.TrimSpace(string(out))
	if !strings.HasPrefix(userSite, fake) {
		t.Fatalf("픽스처가 user site 경로를 못 잡았다(호스트 홈을 가리킨다) — 심지 않고 중단: %q", userSite)
	}
	if err := os.MkdirAll(userSite, 0o700); err != nil {
		t.Fatalf("user site 디렉터리 생성 실패: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userSite, "ctr_host_site_marker.py"),
		[]byte("VALUE = \"host-user-site\"\n"), 0o600); err != nil {
		t.Fatalf("user site 픽스처 기록 실패: %v", err)
	}
	code := "import ctr_host_site_marker\nprint(ctr_host_site_marker.VALUE)\n"

	// ① 격리 없음 — pyEnv에서 PYTHONNOUSERSITE만 뺀 환경으로 직접 실행한다(프로덕션에 토글을
	// 심지 않기 위해 이 절반만 테스트가 환경을 조립한다). 나머지 재지정은 그대로다.
	scratch := t.TempDir()
	file := filepath.Join(scratch, "snippet.py")
	if err := os.WriteFile(file, []byte(code), 0o600); err != nil {
		t.Fatalf("스니펫 기록 실패: %v", err)
	}
	env := sandbox.BaseEnv()
	for _, kv := range pyEnv(scratch) {
		if !strings.HasPrefix(kv, "PYTHONNOUSERSITE=") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(py, file)
	cmd.Dir, cmd.Env = scratch, env
	raw, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(raw), "host-user-site") {
		t.Fatalf("픽스처 무효 — 격리 없이도 호스트 user site 모듈이 임포트되지 않았다: %v: %s", err, raw)
	}

	// ② 격리 있음 — 같은 픽스처·같은 스니펫이 러너에서 임포트에 실패한다.
	resp := run(t, Request{Language: "python", Code: code})
	if resp.TimedOut || resp.ExitCode == nil {
		t.Fatalf("러너가 종료코드를 남기지 않았다: timedout=%v stderr=%q", resp.TimedOut, resp.Stderr)
	}
	if *resp.ExitCode == 0 {
		t.Fatalf("호스트 user site 모듈이 러너에서 임포트됐다: stdout=%q", resp.Stdout)
	}
	if !strings.Contains(resp.Stderr, "ModuleNotFoundError") {
		t.Errorf("임포트 실패가 아닌 다른 이유로 죽었다(격리 확인 불가): stderr=%q", resp.Stderr)
	}
}

// TestRunTS: TypeScript 골든 — 타입 표기가 스트립되고 실행된다(bun 또는 node ≥22.7).
func TestRunTS(t *testing.T) {
	requireLang(t, "typescript")
	resp := run(t, Request{
		Language: "typescript",
		Code:     "const x: number = 41\nconsole.log('ts-' + (x + 1))\n",
	})
	if resp.ExitCode == nil || *resp.ExitCode != 0 || !strings.Contains(resp.Stdout, "ts-42") {
		t.Fatalf("exit=%v stdout=%q stderr=%q", resp.ExitCode, resp.Stdout, resp.Stderr)
	}
}

// TestJSEnvIsolatesUserConfig: npm 사용자 구성 경로가 스크래치로 고정된다.
func TestJSEnvIsolatesUserConfig(t *testing.T) {
	scratch := t.TempDir()
	m := map[string]string{}
	for _, kv := range jsEnv(scratch) {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	want := filepath.Join(scratch, "npmrc")
	if got := m["NPM_CONFIG_USERCONFIG"]; got != want {
		t.Errorf("NPM_CONFIG_USERCONFIG=%q want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("npmrc 사전 생성 안 됨: %v", err)
	}
	// 양쪽 배선을 표에서 직접 본다 — typescript는 tsRunner()가 따로 조립하므로 한쪽만 배선해도
	// 위 검사는 통과한다. 러너 미설치 환경에서도(런타임 테스트가 Skip돼도) 누락을 잡는다.
	for _, lang := range []string{"javascript", "typescript"} {
		r := table()[lang]
		if r.extra == nil {
			t.Errorf("%s: extra 미배선", lang)
			continue
		}
		sub := t.TempDir()
		got := ""
		for _, kv := range r.extra(sub) {
			if k, v, _ := strings.Cut(kv, "="); k == "NPM_CONFIG_USERCONFIG" {
				got = v
			}
		}
		if want := filepath.Join(sub, "npmrc"); got != want {
			t.Errorf("%s: NPM_CONFIG_USERCONFIG=%q want %q", lang, got, want)
		}
	}
}

// TestRunJSIgnoresHostNpmrc: 러너 안에서 관측한 NPM_CONFIG_USERCONFIG가 스크래치 하위다
// (적용 실증의 전파 절반 — 구성 해석 절반은 TestNpmrcScratchConfigWins가 맡는다).
// javascript·typescript 양쪽을 본다: 한쪽만 배선되면 그 서브테스트에서 갈린다.
func TestRunJSIgnoresHostNpmrc(t *testing.T) {
	for _, lang := range []string{"javascript", "typescript"} {
		t.Run(lang, func(t *testing.T) {
			requireLang(t, lang)
			resp := run(t, Request{
				Language: lang,
				Code: "const rc = String(process.env.NPM_CONFIG_USERCONFIG)\n" +
					"console.log(rc.startsWith(String(process.env.CTR_SCRATCH)) ? 'RC-IN-SCRATCH' : 'RC=' + rc)\n",
			})
			if resp.ExitCode == nil || *resp.ExitCode != 0 {
				t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
			}
			if !strings.Contains(resp.Stdout, "RC-IN-SCRATCH") {
				t.Errorf("NPM_CONFIG_USERCONFIG가 스크래치 하위가 아니다: %q", resp.Stdout)
			}
		})
	}
}

// TestNpmrcScratchConfigWins: 호스트 사용자 npmrc가 실재하는 픽스처를 만들어 스크래치 구성이
// 이긴다는 것을 확인한다(D65 검증 계약). npmrc를 읽는 주체는 인터프리터가 아니라 패키지
// 관리자다 — `node file.js`/`bun file.js`는 npmrc를 전혀 읽지 않으므로 스니펫 실행만으로는
// 반영을 관측할 수 없고, 반영은 스니펫이 npm/bun install을 호출할 때 일어난다. 그래서 적용
// 실증은 그 구성을 실제로 해석하는 npm으로 한다(`config get`은 네트워크를 쓰지 않는다).
// 양방향을 다 본다 — 격리를 뺀 절반은 호스트 registry가 나오고(픽스처가 실효적이라는 증거),
// 넣은 절반은 그 값이 사라진다. 자식 env 닫힌 표가 통과시키는 홈 포인터가 windows USERPROFILE ·
// unix HOME이라(npm은 os.homedir()로 ~를 정한다) 픽스처를 그쪽에 심는다. 상위 우선순위인
// 프로젝트 구성이 실험을 흐리지 않게 cwd는 빈 디렉터리로 둔다. unix 실행은 3-OS CI가 검증한다.
func TestNpmrcScratchConfigWins(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm 미설치 — npmrc를 해석하는 주체가 없다")
	}
	fake := t.TempDir()
	t.Setenv("HOME", fake)
	t.Setenv("USERPROFILE", fake)
	const marker = "http://ctr-host-npmrc-marker.invalid/"
	if err := os.WriteFile(filepath.Join(fake, ".npmrc"), []byte("registry="+marker+"\n"), 0o600); err != nil {
		t.Fatalf("호스트 npmrc 픽스처 기록 실패: %v", err)
	}
	registry := func(extra []string) string {
		t.Helper()
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			// npm은 .cmd 래퍼라 CreateProcess로 직접 실행되지 않는다 — cmd.exe 경유.
			cmd = exec.Command("cmd", "/c", "npm", "config", "get", "registry")
		} else {
			cmd = exec.Command("npm", "config", "get", "registry")
		}
		cmd.Dir, cmd.Env = t.TempDir(), append(sandbox.BaseEnv(), extra...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("npm config get registry: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := registry(nil); got != marker {
		t.Fatalf("픽스처 무효 — 호스트 사용자 npmrc가 npm에 반영되지 않는다: %q", got)
	}
	// 내장 기본으로 떨어지는지가 아니라 "호스트 사용자 구성 값이 아닌지"를 본다 — 머신 레벨
	// npmrc가 registry를 정한 호스트에서도 판정이 흔들리지 않게(사용자 구성 상속만이 계약이다).
	// 빈 출력은 통과가 아니다 — 값을 못 읽은 것과 격리된 것을 구분한다.
	switch got := registry(jsEnv(t.TempDir())); {
	case got == marker:
		t.Errorf("스크래치 npmrc가 호스트 사용자 구성에 지고 있다: %q", got)
	case got == "":
		t.Errorf("registry 값을 못 읽었다 — 격리 판정 불가")
	}
}

// TestCSEnvInjection: C# env 재지정이 스크래치 하위 경로·강제 플래그로 주입되고 재지정
// 디렉터리가 생성됨을 결정적으로 검증한다(dotnet 실행/복원 불필요 — 배선 자체 확인).
// 런타임 왕복은 TestRunCS(CI)가 겸한다.
func TestCSEnvInjection(t *testing.T) {
	scratch := t.TempDir()
	m := map[string]string{}
	for _, kv := range csEnv(scratch) {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	if got := m["DOTNET_CLI_HOME"]; got != filepath.Join(scratch, "dotnet") {
		t.Errorf("DOTNET_CLI_HOME=%q", got)
	}
	if got := m["NUGET_PACKAGES"]; got != filepath.Join(scratch, "nuget") {
		t.Errorf("NUGET_PACKAGES=%q", got)
	}
	if m["DOTNET_CLI_DO_NOT_USE_MSBUILD_SERVER"] != "1" {
		t.Errorf("MSBUILD 서버 비활성 플래그 미주입")
	}
	// I4: cold DOTNET_CLI_HOME first-run 배너/텔레메트리가 stdout을 오염하지 않게 억제.
	if m["DOTNET_NOLOGO"] != "1" {
		t.Errorf("DOTNET_NOLOGO 미주입")
	}
	if m["DOTNET_CLI_TELEMETRY_OPTOUT"] != "1" {
		t.Errorf("DOTNET_CLI_TELEMETRY_OPTOUT 미주입")
	}
	// I5: NuGet http/plugins 캐시도 스크래치 하위여야 한다(안 그러면 HOME/LOCALAPPDATA로 이탈).
	if got := m["NUGET_HTTP_CACHE_PATH"]; got != filepath.Join(scratch, "nuget-http") {
		t.Errorf("NUGET_HTTP_CACHE_PATH=%q (스크래치 하위 아님)", got)
	}
	if got := m["NUGET_PLUGINS_CACHE_PATH"]; got != filepath.Join(scratch, "nuget-plugins") {
		t.Errorf("NUGET_PLUGINS_CACHE_PATH=%q (스크래치 하위 아님)", got)
	}
	// 쓰기 계약 — 러너가 재지정 디렉터리 존재를 전제(NuGet aux 캐시 포함)
	for _, sub := range []string{"dotnet", "nuget", "nuget-http", "nuget-plugins"} {
		if fi, err := os.Stat(filepath.Join(scratch, sub)); err != nil || !fi.IsDir() {
			t.Errorf("재지정 디렉터리 %q 미생성: %v", sub, err)
		}
	}
	// 호스트 NuGet 구성 상속 차단 — NUGET_* env는 캐시·패키지 경로만 재지정하고 구성 파일
	// 검색 경로는 격리하지 않는다(부재 로컬 소스 → 복원 NU1301, config globalPackagesFolder는
	// NUGET_PACKAGES env보다 우선해 패키지를 스크래치 밖에 쓴다). 스크래치 로컬 구성이 탐색
	// 체인 말단에서 섹션별 <clear />로 상위 병합분을 지운다.
	cfg, err := os.ReadFile(filepath.Join(scratch, "nuget.config"))
	if err != nil {
		t.Fatalf("nuget.config 미생성: %v", err)
	}
	if n := strings.Count(string(cfg), "<clear />"); n != 5 {
		t.Errorf("<clear /> %d개 — config·packageSources·disabledPackageSources·packageSourceMapping·fallbackPackageFolders 5개 필요", n)
	}
	if !strings.Contains(string(cfg), "https://api.nuget.org/v3/index.json") {
		t.Errorf("clear 후 복원 가능한 소스가 없다 — nuget.org 미등록")
	}
}

// TestRunCS: C# 골든 + env 주입 런타임 왕복 — file-based 앱이 실행되고 스니펫이 읽은
// 재지정 env가 스크래치 하위를 담는다. 호스트 NuGet 구성 상속은 csEnv의 스크래치 로컬
// nuget.config가 끊으므로 더 이상 Skip 사유가 아니다 — 그 전에는 부재 로컬 소스로 인한
// NU1301이 이 Skip에 흡수돼 실사용 실패가 테스트에 드러나지 않았다. 남은 Skip은 소스 도달
// 불가 등 진짜 환경 비가용이며, 러너 미감지 Skip과 동류로 처리한다(CI 검증).
func TestRunCS(t *testing.T) {
	requireLang(t, "csharp")
	resp := run(t, Request{
		Language: "csharp",
		Code: "System.Console.WriteLine(\"cs-ok\");\n" +
			"System.Console.WriteLine(System.Environment.GetEnvironmentVariable(\"DOTNET_CLI_HOME\"));\n" +
			"System.Console.WriteLine(System.Environment.GetEnvironmentVariable(\"NUGET_PACKAGES\"));\n",
	})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		// NuGet 오류(NU1xxx)는 MSBuild가 stdout으로도 낸다 — 두 스트림 다 본다.
		if strings.Contains(resp.Stdout+resp.Stderr, "NU1") { // NuGet 복원/소스 오류 = 환경 문제
			t.Skipf("dotnet 복원 환경 문제로 실행 불가(CI에서 검증): %.100q", resp.Stdout)
		}
		t.Fatalf("csharp 실패: timedout=%v stdout=%q stderr=%q", resp.TimedOut, resp.Stdout, resp.Stderr)
	}
	for _, want := range []string{"cs-ok", "dotnet", "nuget"} {
		if !strings.Contains(resp.Stdout, want) {
			t.Fatalf("env 주입/골든 부재 %q: stdout=%q", want, resp.Stdout)
		}
	}
}

// TestRunnerStatus: doctor용 — 6언어 모두 항목이 나오고 현재 환경의 러너들이 OK다.
func TestRunnerStatus(t *testing.T) {
	st := RunnerStatus()
	if len(st) != 6 {
		t.Fatalf("언어 수=%d want 6", len(st))
	}
	byLang := map[string]LangStatus{}
	for _, s := range st {
		byLang[s.Lang] = s
	}
	for _, l := range []string{"shell", "javascript", "typescript", "python", "go", "csharp"} {
		if _, ok := byLang[l]; !ok {
			t.Fatalf("언어 %q 상태 부재", l)
		}
	}
	// 감지된 러너는 Runner 라벨이 채워져야 한다(부재 시엔 OK=false라 검사 생략).
	for _, s := range st {
		if s.OK && s.Runner == "" {
			t.Errorf("%s OK인데 Runner 라벨 비어있음", s.Lang)
		}
	}
}
