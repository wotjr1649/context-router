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

func TestRunPython(t *testing.T) {
	requireLang(t, "python")
	resp := run(t, Request{Language: "python", Code: "print('py-ok')"})
	if resp.ExitCode == nil || *resp.ExitCode != 0 || !strings.Contains(resp.Stdout, "py-ok") {
		t.Fatalf("exit=%v stdout=%q stderr=%q", resp.ExitCode, resp.Stdout, resp.Stderr)
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
}

// TestRunCS: C# 골든 + env 주입 런타임 왕복 — file-based 앱이 실행되고 스니펫이 읽은
// 재지정 env가 스크래치 하위를 담는다. dotnet 복원이 환경(예: 머신 NuGet.Config 소스 부재)
// 때문에 실패하면 Skip한다 — 러너 미감지 Skip과 동류의 "toolchain 비가용" 처리(CI 검증).
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
