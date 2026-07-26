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

// TestShellExitCodeIsLastCommand: 중간 명령이 실패해도 마지막 명령이 성공하면 exit 0이고,
// 실패 흔적은 stderr에만 남는다. 러너가 엄격 모드를 주입하지 않는다는 계약의 회귀 방지다
// (설계 v0.12 D66 — 셸 일반의 정의된 동작이며 언어별로 다르게 굴지 않는다).
func TestShellExitCodeIsLastCommand(t *testing.T) {
	requireLang(t, "shell")
	var code string
	if runtime.GOOS == "windows" {
		// 존재하지 않는 경로 접근 = 비종결 오류. 그 뒤 정상 출력으로 끝난다.
		code = "Get-Content 'Z:\\ctr-no-such-file.txt'\nWrite-Output 'done'"
	} else {
		code = "cat /ctr-no-such-file.txt\necho done"
	}
	resp := run(t, Request{Language: "shell", Code: code})
	if resp.ExitCode == nil {
		t.Fatalf("ExitCode=nil (timed_out=%v)", resp.TimedOut)
	}
	if *resp.ExitCode != 0 {
		t.Fatalf("exit=%d want 0 — 러너가 엄격 모드를 주입했을 수 있다. stderr=%q", *resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "done") {
		t.Errorf("마지막 명령이 실행되지 않았다: stdout=%q", resp.Stdout)
	}
	if strings.TrimSpace(resp.Stderr) == "" {
		t.Errorf("실패 흔적이 stderr에 없다 — 호출자가 판정할 근거가 사라진다")
	}
}

// TestShellNativeExitNotPropagated: 계약의 안전망(exit_code + stderr를 함께 보기)이 잡지
// 못하는 부류를 고정한다 — windows 인터프리터는 -File 실행에서 exit이나 종결 오류가 없으면 0을
// 돌려주므로, 마지막 줄이 비 0으로 죽은 네이티브 명령이어도 exit_code 0이고 stderr까지 비어
// 실패가 두 채널 어디에도 남지 않는다($ErrorActionPreference = 'Stop'도 이 부류는 멈추지
// 못한다 — pwsh 7.6.0·powershell 5.1 실측). 안내가 PowerShell에 exit $LASTEXITCODE 전파를
// 지시하는 근거이고, 러너가 언젠가 네이티브 종료를 전파하기 시작하면 안내 문면을 고쳐야
// 하므로 이 테스트가 먼저 깨져 알린다. sh는 반대로 마지막 명령의 상태를 문자 그대로
// 전파하며(안내의 sh 문면 근거) 두 절반이 그 대비를 함께 고정한다(설계 v0.12 D66).
func TestShellNativeExitNotPropagated(t *testing.T) {
	requireLang(t, "shell")
	if runtime.GOOS != "windows" {
		resp := run(t, Request{Language: "shell", Code: "sh -c 'exit 5'"})
		if resp.ExitCode == nil {
			t.Fatalf("ExitCode=nil (timed_out=%v)", resp.TimedOut)
		}
		if *resp.ExitCode != 5 {
			t.Fatalf("exit=%d want 5 — sh가 마지막 명령의 상태를 전파하지 않는다. stderr=%q",
				*resp.ExitCode, resp.Stderr)
		}
		return
	}
	resp := run(t, Request{Language: "shell", Code: "cmd /c exit 5"})
	if resp.ExitCode == nil {
		t.Fatalf("ExitCode=nil (timed_out=%v)", resp.TimedOut)
	}
	if *resp.ExitCode != 0 {
		t.Fatalf("exit=%d want 0 — 네이티브 종료가 전파되기 시작했다면 안내 문면을 고친다. stderr=%q",
			*resp.ExitCode, resp.Stderr)
	}
	if s := strings.TrimSpace(resp.Stderr); s != "" {
		t.Errorf("stderr가 비어 있지 않다 — 실패가 관측 가능해졌다면 안내 문면을 고친다: %q", s)
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

// TestJSEnvIsolatesUserConfig: npm 사용자 구성과 bun 사용자 레벨 bunfig 탐색이 스크래치로
// 고정된다. npmrc만 덮으면 절반이다 — bun은 홈 포인터로 자기 사용자 구성(preload 포함)을
// 찾는다(jsEnv 주석 ②).
func TestJSEnvIsolatesUserConfig(t *testing.T) {
	want := func(root string) map[string]string {
		return map[string]string{
			"NPM_CONFIG_USERCONFIG": filepath.Join(root, "npmrc"),
			"HOME":                  filepath.Join(root, "home"),
			"XDG_CONFIG_HOME":       filepath.Join(root, "home", ".config"),
		}
	}
	scratch := t.TempDir()
	m := map[string]string{}
	for _, kv := range jsEnv(scratch) {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for k, w := range want(scratch) {
		if got := m[k]; got != w {
			t.Errorf("%s=%q want %q", k, got, w)
		}
	}
	// env에 할당한 경로는 전부 사전 생성한다(goEnv GOTMPDIR 규칙) — npmrc는 파일, 홈·XDG는 디렉터리.
	if _, err := os.Stat(m["NPM_CONFIG_USERCONFIG"]); err != nil {
		t.Errorf("npmrc 사전 생성 안 됨: %v", err)
	}
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME"} {
		if fi, err := os.Stat(m[k]); err != nil || !fi.IsDir() {
			t.Errorf("%s 디렉터리 사전 생성 안 됨: %v", k, err)
		}
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
		sm := map[string]string{}
		for _, kv := range r.extra(sub) {
			k, v, _ := strings.Cut(kv, "=")
			sm[k] = v
		}
		for k, w := range want(sub) {
			if got := sm[k]; got != w {
				t.Errorf("%s: %s=%q want %q", lang, k, got, w)
			}
		}
	}
}

// TestRunJSIsolatesUserConfig: 러너 안에서 관측한 사용자 구성 포인터 셋이 전부 스크래치 하위다
// (적용 실증의 전파 절반 — npm 구성 해석은 TestNpmrcScratchConfigWins, bun preload는
// TestRunJSScratchBunfigWins가 맡는다). javascript·typescript 양쪽을 본다: 한쪽만 배선되면 그
// 서브테스트에서 갈린다. 실패 시 어느 변수가 새는지 출력에 남긴다.
func TestRunJSIsolatesUserConfig(t *testing.T) {
	for _, lang := range []string{"javascript", "typescript"} {
		t.Run(lang, func(t *testing.T) {
			requireLang(t, lang)
			resp := run(t, Request{
				Language: lang,
				Code: "const s = String(process.env.CTR_SCRATCH)\n" +
					"const keys = ['NPM_CONFIG_USERCONFIG', 'HOME', 'XDG_CONFIG_HOME']\n" +
					"const leak = keys.filter((k) => !String(process.env[k]).startsWith(s))\n" +
					"console.log(leak.length === 0 ? 'ALL-IN-SCRATCH' : 'LEAK ' + leak.map((k) => k + '=' + process.env[k]).join(' '))\n",
			})
			if resp.ExitCode == nil || *resp.ExitCode != 0 {
				t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
			}
			if !strings.Contains(resp.Stdout, "ALL-IN-SCRATCH") {
				t.Errorf("사용자 구성 포인터가 스크래치 하위가 아니다: %q", resp.Stdout)
			}
		})
	}
}

// TestRunJSScratchBunfigWins: bun 사용자 레벨 bunfig(`$HOME/.bunfig.toml`)에 preload를 심어
// 격리가 이긴다는 것을 확인한다. preload는 스니펫보다 먼저 실행되므로 상속되면 사용자 코드 앞에
// 임의 모듈이 끼어들고, detectJS가 bun을 먼저 고르니 러너의 기본 경로다.
// 이 픽스처는 플랫폼·bun 버전에 따라 무효일 수 있다 — windows 1.3.14에서는 어떤 홈 포인터로도
// 전역 bunfig가 적용되지 않는 것을 실측했다. 그래서 격리 없는 절반으로 픽스처 실효성을 먼저
// 확인하고, 무효면 그 사실을 Skip 문면에 남긴다(조용한 통과 금지). 실효적이면 러너 절반을
// 단정한다. bun이 없으면 읽는 주체가 없다 — node는 bunfig를 쓰지 않는다.
func TestRunJSScratchBunfigWins(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun 미설치 — bunfig를 읽는 주체가 없다(node는 bunfig를 쓰지 않는다)")
	}
	// 픽스처를 심기 전에 sync.Once 빌드를 끝낸다 — 심은 뒤면 그 go build가 깨지고 실패가
	// testExeErr에 캐시돼 패키지 전체가 오염된다.
	selfExe(t)

	fake := t.TempDir()
	t.Setenv("HOME", fake) // unix 닫힌 표가 통과시키는 포인터 — 격리가 없으면 자식이 이걸 본다
	t.Setenv("XDG_CONFIG_HOME", fake)
	preload := filepath.Join(fake, "host-preload.js")
	if err := os.WriteFile(preload, []byte("console.log('HOST-PRELOAD')\n"), 0o600); err != nil {
		t.Fatalf("preload 픽스처 기록 실패: %v", err)
	}
	// TOML 문자열이라 경로는 슬래시로 — windows 백슬래시 이스케이프를 피한다.
	cfg := []byte("preload = [\"" + filepath.ToSlash(preload) + "\"]\n")
	if err := os.WriteFile(filepath.Join(fake, ".bunfig.toml"), cfg, 0o600); err != nil {
		t.Fatalf("호스트 bunfig 픽스처 기록 실패: %v", err)
	}
	code := "console.log('snippet-ok')\n"

	// ① 격리 없음 — jsEnv에서 홈 포인터만 뺀 환경으로 직접 실행하고, 픽스처 홈을 명시로 얹는다
	// (windows 닫힌 표에는 HOME이 없어 BaseEnv 경유로는 실효성 자체를 확인할 수 없다).
	scratch := t.TempDir()
	file := filepath.Join(scratch, "snippet.js")
	if err := os.WriteFile(file, []byte(code), 0o600); err != nil {
		t.Fatalf("스니펫 기록 실패: %v", err)
	}
	env := sandbox.BaseEnv()
	for _, kv := range jsEnv(scratch) {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+fake, "XDG_CONFIG_HOME="+fake)
	cmd := exec.Command(bun, file)
	cmd.Dir, cmd.Env = scratch, env
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("격리 없는 절반이 실행 자체에 실패했다: %v: %s", err, raw)
	}
	if !strings.Contains(string(raw), "HOST-PRELOAD") {
		t.Skipf("이 플랫폼·bun 버전에서 전역 bunfig가 적용되지 않아 픽스처가 무효다 — 격리가 닫는 것은 문서상 unix 경로다: %s", raw)
	}

	// ② 격리 있음 — 같은 픽스처가 러너에서 preload되지 않는다.
	resp := run(t, Request{Language: "javascript", Code: code})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "HOST-PRELOAD") {
		t.Errorf("호스트 bunfig의 preload가 러너에서 실행됐다: %q", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "snippet-ok") {
		t.Errorf("골든 출력 부재: %q", resp.Stdout)
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
	switch got := registry(jsEnv(t.TempDir())); got {
	case marker:
		t.Errorf("스크래치 npmrc가 호스트 사용자 구성에 지고 있다: %q", got)
	case "":
		t.Errorf("registry 값을 못 읽었다 — 격리 판정 불가")
	}
}

// TestRunShellPSModulePathHasNoHostUserPath: 러너 안에서 관측한 유효 PSModulePath에 호스트
// 사용자 모듈 경로가 없고 인터프리터 설치본의 Modules가 들어 있다(적용 실증). 기본 cmdlet도
// 그대로 동작한다 — 격리가 러너를 망가뜨리지 않았다는 증거를 같은 왕복에서 얻는다.
func TestRunShellPSModulePathHasNoHostUserPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PSModulePath는 Windows 전용 키")
	}
	requireLang(t, "shell")
	resp := run(t, Request{
		Language: "shell",
		Code:     "$env:PSModulePath\n\"YEAR=\" + (Get-Date -Format yyyy)\n",
	})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "YEAR=") {
		t.Errorf("기본 cmdlet이 동작하지 않는다: stdout=%q stderr=%q", resp.Stdout, resp.Stderr)
	}
	bin, _, err := table()["shell"].detect()
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if want := filepath.Join(filepath.Dir(bin), "Modules"); !strings.Contains(resp.Stdout, want) {
		t.Errorf("인터프리터 설치본 Modules(%q)가 유효 경로에 없다:\n%s", want, resp.Stdout)
	}
	// 호스트 사용자 모듈 경로가 남아 있으면 호스트 설치 모듈이 스니펫에서 자동 로드된다.
	// 스크래치는 %LOCALAPPDATA%\Temp 하위라 <USERPROFILE>\Documents 접두와 겹치지 않는다.
	hostDocs := filepath.Join(os.Getenv("USERPROFILE"), "Documents")
	if strings.Contains(resp.Stdout, hostDocs) {
		t.Errorf("호스트 사용자 모듈 경로(%q)가 남아 있다:\n%s", hostDocs, resp.Stdout)
	}
}

// TestRunShellScratchModulePathWins: 호스트 사용자 모듈 경로에 임포트 가능한 모듈이 실재하는
// 픽스처를 만들어 스크래치 구성이 이긴다는 것을 확인한다(D65 검증 계약 — 형제 태스크 T7·T8과
// 같은 양방향 형태). 위 테스트는 유효 경로 문자열을 보지만 이 테스트는 로드 가능성 자체를 본다.
// 픽스처는 t.Setenv로 호스트 USERPROFILE 자체를 임시 디렉터리로 돌려 심는다 — 사용자의 실제
// Documents에 쓰지 않는다. pwsh는 USERPROFILE 유도 사용자 모듈 경로를 PSModulePath 앞에
// 덧붙이므로(실측) 격리 없는 절반에서 픽스처가 실제로 도달한다.
func TestRunShellScratchModulePathWins(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PSModulePath는 Windows 전용 키")
	}
	requireLang(t, "shell")
	// 픽스처를 심기 전에 sync.Once 빌드를 끝낸다 — 심은 뒤면 그 go build가 USERPROFILE 유도
	// GOPATH를 잃고 깨지며 실패가 testExeErr에 캐시돼 패키지 전체가 오염된다.
	selfExe(t)

	fake := t.TempDir()
	// pwsh 사용자 모듈 경로 규약: <USERPROFILE>\Documents\PowerShell\Modules\<Name>\<Name>.psm1
	const modName = "CtrHostCanary"
	modDir := filepath.Join(fake, "Documents", "PowerShell", "Modules", modName)
	if err := os.MkdirAll(modDir, 0o700); err != nil {
		t.Fatalf("픽스처 모듈 디렉터리 생성 실패: %v", err)
	}
	psm1 := []byte("function Get-CtrHostCanary { 'HOST-MODULE-LOADED' }\n")
	if err := os.WriteFile(filepath.Join(modDir, modName+".psm1"), psm1, 0o600); err != nil {
		t.Fatalf("픽스처 모듈 기록 실패: %v", err)
	}
	t.Setenv("USERPROFILE", fake) // 닫힌 표가 통과시키는 포인터 — 격리가 없으면 자식이 이걸 본다

	// 임포트 성공/실패가 stdout 표식으로 갈린다 — 종료코드는 양쪽 다 0으로 둔다.
	code := "try { Import-Module " + modName + " -ErrorAction Stop; \"CANARY=\" + (Get-CtrHostCanary) }" +
		" catch { \"IMPORT_FAILED\" }\n"

	// ① 격리 없음 — shellRunner의 extra에서 USERPROFILE 재지정만 뺀 환경으로 직접 실행한다
	// (프로덕션에 토글을 심지 않기 위해 이 절반만 테스트가 환경을 조립한다). detect → extra
	// 순서는 Run과 같게 유지한다 — extra가 읽는 psHome을 detect가 채운다.
	r := table()["shell"]
	bin, _, err := r.detect()
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	scratch := t.TempDir()
	file := filepath.Join(scratch, "snippet.ps1")
	if err := os.WriteFile(file, snippetContent("snippet.ps1", code), 0o600); err != nil {
		t.Fatalf("스니펫 기록 실패: %v", err)
	}
	env := sandbox.BaseEnv() // 닫힌 표 그대로 — PSModulePath는 이미 표 밖이다
	for _, kv := range r.extra(scratch) {
		if !strings.HasPrefix(kv, "USERPROFILE=") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bin, "-NoProfile", "-NonInteractive", "-File", file)
	cmd.Dir, cmd.Env = scratch, env
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("격리 없는 절반이 실행 자체에 실패했다: %v: %s", err, raw)
	}
	if !strings.Contains(string(raw), "HOST-MODULE-LOADED") {
		t.Fatalf("픽스처 무효 — USERPROFILE 재지정 없이도 호스트 모듈이 로드되지 않았다: %s", raw)
	}

	// ② 격리 있음 — 같은 픽스처가 러너에서 임포트되지 않는다.
	resp := run(t, Request{Language: "shell", Code: code})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "HOST-MODULE-LOADED") {
		t.Errorf("호스트 사용자 모듈이 러너에서 로드됐다: %q", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "IMPORT_FAILED") {
		t.Errorf("임포트 실패 표식 부재 — 관측이 성립하지 않았다: stdout=%q stderr=%q",
			resp.Stdout, resp.Stderr)
	}
}

// TestRunShellPS51ProviderWriteReturns: 출하 유도(psModulePath)로 조립한 값이 **5.1**
// 인터프리터에서 프로바이더 cmdlet 쓰기를 돌아오게 하는지 본다. shellRunner의 detect는 pwsh 7이
// 있으면 그것을 먼저 고르므로 Run 경유로는 5.1 갈래가 CI에서 한 번도 실행되지 않는데, 측정된
// 정지는 그 갈래에서만 났다(CI windows-latest + Windows PowerShell 5.1, 2026-07-25). 그래서
// detect만 5.1로 대체하고 값 유도·env 조립·격리 경로는 출하와 같게 둔다 — 프로덕션에 토글을
// 심지 않기 위해 이 테스트가 argv를 직접 조립한다(위 TestRunShellScratchModulePathWins와 같은
// 방식). 5.1만 있는 호스트에서 ctr_execute{language:"shell"}가 실제로 받는 구성이 이것이다.
//
// 판정 둘이다.
//
//	① 구조 요건 — 주입 값의 첫 항목이 인자로 준(=우리가 만든) 실재 디렉터리다. 이분탐색이
//	   지목한 유일한 해제 조건이라 유도가 이 성질을 잃으면 정지 조건이 돌아온다. 첫 항목의
//	   신원까지 보는 이유: <PSHOME>\Modules로 "단순화"해도 그 경로는 실재하므로 존재 검사만으로는
//	   그 회귀가 통과한다.
//	② 동작 요건 — 같은 값으로 띄운 5.1에서 Set-Content가 돌아온다. 정지가 재발하면 여기서 잡힌다.
//
// 예산 10s로 묶는다: 5.1 기동은 실측 170~194ms이고 이 스니펫은 손자를 스폰하지 않으므로 건강한
// 실행은 1s 안쪽이다. 예산이 만료되면 sandbox.Run이 잡을 종료하므로(+WaitDelay 5s) 정지가
// 재발해도 패키지가 매달리지 않는다. 재발 시 판별 계측은 internal/sandbox/run_windows_test.go
// TestRunWindowsTimeoutKillsTree의 실패 경로에 있다.
func TestRunShellPS51ProviderWriteReturns(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell 5.1 갈래는 Windows 전용")
	}
	psExe, err := exec.LookPath("powershell")
	if err != nil {
		t.Skipf("powershell.exe(5.1) 미해석 — 5.1 갈래 검증 불가: %v", err)
	}

	scratch := t.TempDir()
	mods := filepath.Join(scratch, "psmodules") // shellRunner의 extra와 같은 배치·같은 이름
	if err := os.MkdirAll(mods, 0o700); err != nil {
		t.Fatalf("스크래치 모듈 디렉터리 생성 실패 — 주입 값의 전제가 깨진다: %v", err)
	}
	psmod := psModulePath(mods, filepath.Dir(psExe), "WindowsPowerShell")

	first, _, _ := strings.Cut(psmod, ";")
	fi, statErr := os.Stat(first)
	if first != mods || statErr != nil || !fi.IsDir() {
		t.Fatalf("주입 값의 첫 항목이 우리가 만든 실재 디렉터리가 아니다 — CI 5.1 프로바이더 "+
			"cmdlet 정지의 유일한 해제 조건이다(D65): first=%q want=%q stat=%v", first, mods, statErr)
	}

	// 표식은 Set-Content **뒤에** 찍는다 — 파일 존재만 보면 "쓰기는 됐는데 호출이 돌아오지
	// 않았다"를 구분하지 못하고, 정지의 서명이 정확히 그 자리다.
	const marker = "PROVIDER-WRITE-RETURNED"
	out := filepath.Join(scratch, "provider.out")
	code := "'x' | Set-Content -LiteralPath '" + out + "'\nWrite-Output '" + marker + "'\n"
	file := filepath.Join(scratch, "snippet.ps1")
	if err := os.WriteFile(file, snippetContent("snippet.ps1", code), 0o600); err != nil {
		t.Fatalf("스니펫 기록 실패: %v", err)
	}
	env := append(sandbox.BaseEnv(), tmpEnv(scratch)...)
	env = append(env, "PSModulePath="+psmod, "USERPROFILE="+scratch) // extra가 주입하는 그대로
	res, err := sandbox.Run(context.Background(), sandbox.Spec{
		Argv: []string{psExe, "-NoProfile", "-NonInteractive", "-File", file},
		Dir:  scratch, Env: env, Timeout: 10 * time.Second,
		StdoutCap: stdoutCap, StderrCap: stderrCap,
	})
	if err != nil {
		t.Fatalf("sandbox.Run: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("5.1에서 프로바이더 cmdlet이 돌아오지 않았다(정지 재발): stdout=%q stderr=%q",
			res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), marker) {
		t.Fatalf("exit=%d stdout=%q stderr=%q — 표식 부재는 Set-Content 뒤가 실행되지 않았다는 뜻",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	if b, err := os.ReadFile(out); err != nil || string(bytes.TrimSpace(b)) != "x" {
		t.Fatalf("프로바이더 쓰기 결과가 없다: %q err=%v", b, err)
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
