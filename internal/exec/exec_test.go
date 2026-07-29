package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// mustEnv: extra류(격리 준비) 함수의 (env, error)를 호출부 잡음 없이 펼친다. 준비 실패는
// 테스트 픽스처가 깨진 것이므로 즉시 끊는다 — t를 받는 헬퍼로는 f(g()) 형태로 다중값을
// 전달할 수 없어(Go 호출 규칙) panic으로 끊는다. panic도 해당 테스트만 실패시킨다.
func mustEnv(kv []string, err error) []string {
	if err != nil {
		panic("격리 준비 실패: " + err.Error())
	}
	return kv
}

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
	side := filepath.Join(t.TempDir(), "side")
	got := snippetContent("snippet.ps1", "Write-Output '한글'", side)
	if !bytes.HasPrefix(got, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf(".ps1 UTF-8 BOM 미기록: 선두=%x", got[:min(3, len(got))])
	}
	// 신호(D76 tail 또는 D78 등록 문)가 붙으므로 접두·접미가 아니라 포함으로 본다.
	if !bytes.Contains(got, []byte("Write-Output '한글'")) {
		t.Fatalf("코드 본문이 없다: %q", got)
	}
	// D78: 이 스니펫은 승격 대상이라 BOM 직후는 등록 문이고 코드는 그 뒤에 **같은 줄로**
	// 이어진다(구조 단정 본체는 TestSnippetContentRegistration). 선두 BOM이라는 I2 계약은
	// 위 단정이 지킨다 — 여기서는 그 사이에 들어간 것이 등록 문뿐이고 코드가 손상 없이
	// 끝까지 남는지만 본다.
	if !bytes.HasSuffix(got, []byte("; Write-Output '한글'")) {
		t.Fatalf("BOM 다음 등록 문 뒤로 코드가 그대로 이어지지 않는다: %q", got)
	}
	for _, f := range []string{"snippet.sh", "snippet.js", "snippet.py", "snippet.go", "snippet.cs"} {
		out := snippetContent(f, "x", side)
		if bytes.HasPrefix(out, []byte{0xEF, 0xBB, 0xBF}) {
			t.Errorf("%s에 BOM 오부착", f)
		}
		// D76: tail은 .ps1 전용이다 — 다른 확장자는 원문 그대로 나와야 한다(tail 미부착).
		if string(out) != "x" {
			t.Errorf("%s에 tail이 붙었다: %q", f, out)
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
// 돌려주므로, 마지막 줄이 비 0으로 죽은 네이티브 명령이어도 exit_code는 0이다
// ($ErrorActionPreference = 'Stop'도 이 부류는 멈추지 못한다 — pwsh 7.6.0·powershell 5.1
// 실측). 안내가 PowerShell에 exit $LASTEXITCODE 전파를 지시하는 근거이고, 러너가 언젠가
// 네이티브 종료를 exit_code로 전파하기 시작하면 안내 문면을 고쳐야 하므로 이 테스트가 먼저
// 깨져 알린다. D76부터 stderr는 더 이상 비어 있지 않다 — 원래 무신호였던 이 자리를 채우는
// 보강 줄 정확히 하나를 문면 그대로 고정한다(아래 wantStderr) — 부분일치로 완화하면 문면이
// 바뀌거나 엉뚱한 케이스로 새는 것을 이 테스트가 놓친다. sh는 반대로 마지막 명령의 상태를
// 문자 그대로 전파하며(안내의 sh 문면 근거) 두 절반이 그 대비를 함께 고정한다(설계 v0.12
// D66 · v0.13 D76).
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
	// D76: 이 자리는 이제 비어 있지 않다 — 마지막 외부 명령(cmd /c exit 5)의 종료 상황을 알리는
	// 보강 줄 정확히 하나가 들어간다. 문면 한 글자, 줄 수 어느 쪽이 바뀌어도 여기서 깨져야 한다 —
	// 부분일치로 완화하면 이 감시선이 다시 무력해진다(D76 이전엔 stderr가 비어야 통과했다).
	const wantStderr = "context-router: 마지막 외부 명령이 종료 코드 5번으로 끝났습니다(exit_code는 스니펫의 종료 상태입니다).\n"
	if resp.Stderr != wantStderr {
		t.Fatalf("stderr가 보강 줄과 정확히 일치해야 한다: got=%q want=%q", resp.Stderr, wantStderr)
	}
}

// TestShellNativeExitAugmentation — D76: exit_code 0에 stderr가 빈 상황에서만 마지막 외부
// 명령의 종료 상황을 stderr 한 줄로 낸다. exit_code의 의미는 바뀌지 않는다.
//
// D78이 신호 생성 형태를 tail에서 등록 문으로 바꾼 뒤에도 이 보강 조건은 그대로다 —
// 바뀐 것은 어떤 스니펫에서 신호가 살아남느냐이고, 아래 세 케이스(인수 없는 exit ·
// Get-Variable 재정의 · ConstrainedLanguage)가 그 변화의 결과를 잠근다.
func TestShellNativeExitAugmentation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows shell 갈래 전용(PowerShell -File 계약)")
	}
	requireLang(t, "shell")
	const marker = "마지막 외부 명령이 종료 코드"
	// 사이드 파일에 진짜 값(ctr-exit-code:5)이 남았을 때 나가는 보강 줄 전문. 정확히
	// 일치를 요구하는 것은 값이 5라는 것과 **스니펫 자신의 stderr가 비어 있었다**는 것을
	// 한 번에 잠그기 때문이다 — 보강은 stderr가 빈 경우에만 들어간다(exec.go의 D76 조건).
	const wantStderr5 = "context-router: 마지막 외부 명령이 종료 코드 5번으로 끝났습니다(exit_code는 스니펫의 종료 상태입니다).\n"

	t.Run("마지막_줄이_비0_네이티브", func(t *testing.T) {
		resp := run(t, Request{Language: "shell", Code: "cmd /c exit 5\n"})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("exit_code=%v want 0(계약 불변)", resp.ExitCode)
		}
		if !strings.Contains(resp.Stderr, marker) {
			t.Fatalf("보강 줄 없음: stderr=%q", resp.Stderr)
		}
	})

	t.Run("성공한_cmdlet_뒤_거짓실패_금지", func(t *testing.T) {
		resp := run(t, Request{Language: "shell", Code: "cmd /c exit 5\nWrite-Output 'ok'\n"})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("exit_code=%v want 0 — 거짓 실패를 만들지 않는다", resp.ExitCode)
		}
		// 잠긴 값이므로 이 케이스에도 보강 줄이 나온다(의미는 마지막 외부 명령 한정이다).
		if !strings.Contains(resp.Stderr, marker) {
			t.Fatalf("보강 줄 없음: stderr=%q", resp.Stderr)
		}
	})

	t.Run("인수없는_exit도_보강된다", func(t *testing.T) {
		// v0.13 §1.2의 공백 ②를 D78이 닫았다. 인수 없는 exit은 0을 반환해 그 앞의 네이티브
		// 실패(5)를 가렸고, D76 tail은 exit 뒤에 실행되지 않아 신호까지 사라졌다 — 단정이
		// 그 공백을 "보강되지 않는다"로 굳혀 놓았던 자리다. 등록 문은 엔진 종료 시점에
		// 실행되므로 그 5가 남는다. 단정을 뒤집는 것이 D78의 계약이다.
		resp := run(t, Request{Language: "shell", Code: "cmd /c exit 5\nexit\n"})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("exit_code=%v want 0 — 인수 없는 exit의 계약은 바뀌지 않았다", resp.ExitCode)
		}
		if resp.Stderr != wantStderr5 {
			t.Fatalf("보강 줄이 정확히 하나 나와야 한다: got=%q want=%q", resp.Stderr, wantStderr5)
		}
	})

	t.Run("코드를_명시한_exit", func(t *testing.T) {
		resp := run(t, Request{Language: "shell", Code: "cmd /c exit 5\nexit 3\n"})
		if resp.ExitCode == nil || *resp.ExitCode != 3 {
			t.Fatalf("exit_code=%v want 3 — 스니펫이 명시한 코드를 그대로 낸다", resp.ExitCode)
		}
	})

	t.Run("stderr가_비어있지_않으면_넣지_않는다", func(t *testing.T) {
		resp := run(t, Request{Language: "shell", Code: "cmd /c exit 5\n[Console]::Error.WriteLine('boom')\n"})
		if strings.Count(resp.Stderr, marker) != 0 {
			t.Fatalf("stderr가 이미 비어 있지 않은데 보강했다: %q", resp.Stderr)
		}
	})

	t.Run("strict_mode에_네이티브_명령_없음", func(t *testing.T) {
		// 이 스니펫은 최상단 전용 구문으로 시작하지 않아 **승격**된다(promote/strictmode-first).
		// 그래서 여기서 $LASTEXITCODE를 직접 참조하는 것은 등록 문의 핸들러이고, 그런데도
		// 미초기화 변수 오류가 stderr로 나가지 않아야 한다 — 핸들러가 스니펫의 strict mode
		// 스코프 밖에서 돌기 때문이다(근거 실측은 exec.go의 nativeExitTail 주석). D76에서는
		// 같은 자리를 tail의 Get-Variable 경유가 지켰다 — 아래 Fatalf 문면의 "tail"은 그때
		// 표현이 남은 것이다.
		resp := run(t, Request{Language: "shell", Code: "Set-StrictMode -Version Latest\nWrite-Output 'ok'\n"})
		if resp.Stderr != "" {
			t.Fatalf("stderr가 비어야 한다 — tail이 오류를 내면 안 된다: %q", resp.Stderr)
		}
	})

	t.Run("여러줄_문자열로_끝나는_스니펫", func(t *testing.T) {
		// here-string으로 끝나고 마지막 개행이 없는 형태. 이 스니펫은 `cmd`로 시작해 **승격**되므로
		// tail이 붙지 않는다 — 등록 문이 첫 줄 **앞에** 인라인으로 들어갈 뿐 끝에는 아무것도
		// 덧붙지 않는다. 그래서 여기서 보는 것은 "끝에 붙이기"가 아니라 "앞에 붙이기"가 구문을
		// 깨지 않는다는 것이다(아래 Fatalf 문면의 "tail"은 D76 시절 표현이 남은 것이다).
		// 폴백 tail의 선행 개행 계약은 TestSnippetContentFallbackKeepsD76Form의 골든 리터럴이
		// 바이트 단위로 잠근다.
		resp := run(t, Request{Language: "shell", Code: "cmd /c exit 5\n$x = @'\nline\n'@"})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("tail이 구문을 깼다: exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
		}
		if !strings.Contains(resp.Stderr, marker) {
			t.Fatalf("보강 줄 없음: stderr=%q", resp.Stderr)
		}
	})

	t.Run("Get-Variable_재정의에도_신호가_살아남는다", func(t *testing.T) {
		// F2 리뷰가 막던 회귀(스니펫이 Get-Variable을 같은 이름의 함수로 가리면 tail의 호출이
		// 그 함수로 해석돼 throw가 stderr로 새는 것)는 **대상 자체와 함께 사라졌다** — D78
		// 등록 문은 Get-Variable을 쓰지 않고 $LASTEXITCODE를 직접 읽는다. 그래서 이 스니펫은
		// 신호가 소실되던 케이스에서 살아남는 케이스로 바뀐다(설계 v0.14 §3 표5의 긍정 결과).
		// 정확히 일치 단정이 원래의 보호 의도도 함께 유지한다 — 오류가 샜다면 stderr가 비지
		// 않아 보강 자체가 들어가지 않았을 것이다.
		resp := run(t, Request{Language: "shell", Code: "function Get-Variable { throw 'boom' }\ncmd /c exit 5\n"})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("exit_code=%v want 0 — 종료 신호가 스니펫 실행에 영향을 주면 안 된다", resp.ExitCode)
		}
		if resp.Stderr != wantStderr5 {
			t.Fatalf("사이드 파일의 진짜 값(5)이 보강 줄 하나로 나와야 한다: got=%q want=%q",
				resp.Stderr, wantStderr5)
		}
	})

	t.Run("스니펫이_사이드파일_이름에_직접_써도_오인되지_않는다", func(t *testing.T) {
		// F3 리뷰(회귀 방지): 스크래치가 스니펫의 작업 디렉터리이기도 해 상대 경로
		// "ctr-native-exit"가 tail의 사이드 파일과 같은 절대 경로로 닿는다. 마커 없이 직접
		// 쓴 값(9)을 tail이 건너뛴 채로(top-level return) 남기면, 그 값이 외부 명령 종료
		// 코드로 오인되면 안 된다.
		resp := run(t, Request{Language: "shell", Code: "[System.IO.File]::WriteAllText('ctr-native-exit', '9')\nreturn\n"})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("exit_code=%v want 0", resp.ExitCode)
		}
		if resp.Stderr != "" {
			t.Fatalf("스니펫이 직접 쓴 값이 외부 명령 종료 코드로 오인됐다: %q", resp.Stderr)
		}
	})

	t.Run("사이드파일이_상한을_넘으면_보강하지_않는다", func(t *testing.T) {
		// G1(최종 게이트 리뷰): os.ReadFile은 마커 검사 이전에 파일 전체를 서버 프로세스에
		// 할당했다 — 스크래치가 스니펫의 작업 디렉터리이기도 해 스니펫이 이 값을 직접 고를 수
		// 있었다. 마커 뒤에 상한(maxNativeExitSideFileBytes)을 정확히 1바이트 넘기는 자릿수의
		// 숫자를 쓴다 — 그 자릿수는 int64 오버플로 없이 strconv.Atoi가 그대로 파싱할 크기다.
		// 상한이 없다면(또는 한 바이트라도 새면) 이 값은 파싱에 성공해 보강 줄을 냈을 것이므로,
		// 이 케이스가 실패하면 상한이 아니라 다른 이유(파싱 실패 등)로 막혔다는 혼동이 없다.
		digits := maxNativeExitSideFileBytes - len(ctrNativeExitMarker) + 1
		overLimit := ctrNativeExitMarker + strings.Repeat("1", digits)
		resp := run(t, Request{
			Language: "shell",
			Code:     "[System.IO.File]::WriteAllText('ctr-native-exit', '" + overLimit + "')\nreturn\n",
		})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("exit_code=%v want 0", resp.ExitCode)
		}
		if resp.Stderr != "" {
			t.Fatalf("상한을 넘는 사이드 파일이 보강을 만들었다: %q", resp.Stderr)
		}
	})

	t.Run("ConstrainedLanguage에서도_신호가_살아남는다", func(t *testing.T) {
		// 스니펫이 자신을 ConstrainedLanguage로 낮추면(자기 자신에 대한 강등은 FullLanguage에서
		// 허용된다) 그 뒤에 실행되는 D76 tail의 WriteAllText(.NET 정적 메서드 직접 호출)가
		// 막혔다 — "Cannot invoke method. Method invocation is supported only on core types in
		// this language mode."(pwsh 7·powershell 5.1 양쪽 실측). try/catch가 오류를 삼켜
		// 안전하기는 했지만 신호는 사라졌다.
		//
		// D78에서는 등록 문이 스니펫보다 **먼저** 실행되므로 핸들러 스크립트블록이
		// FullLanguage에서 생성되고, 강등 뒤에 열리는 종료 시점에도 기록에 성공한다(이 dev
		// host 실측: 사이드 파일 ctr-exit-code:5). 네이티브 명령(cmd) 호출 자체는 언어 모드
		// 제약을 받지 않는다는 전제는 그대로다.
		resp := run(t, Request{Language: "shell", Code: "$ExecutionContext.SessionState.LanguageMode = 'ConstrainedLanguage'\ncmd /c exit 5\n"})
		if resp.ExitCode == nil || *resp.ExitCode != 0 {
			t.Fatalf("exit_code=%v want 0 — 종료 신호가 스니펫 실행에 영향을 주면 안 된다", resp.ExitCode)
		}
		if resp.Stderr != wantStderr5 {
			t.Fatalf("사이드 파일의 진짜 값(5)이 보강 줄 하나로 나와야 한다: got=%q want=%q",
				resp.Stderr, wantStderr5)
		}
	})
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
	for _, kv := range mustEnv(goEnv(scratch)) {
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
	for _, kv := range mustEnv(goEnv(scratch)) {
		if !strings.HasPrefix(kv, "GOENV=") {
			env = append(env, kv)
		}
	}
	// 시한·정리는 runPS1Shell(아래)이 확립한 관용구를 그대로 쓴다 — CommandContext가 예산
	// 만료·테스트 종료에 자식을 끝내고, WaitDelay가 파이프를 쥔 손자(go run이 만든 바이너리)
	// 때문에 Wait이 돌아오지 않는 것을 막는다. 예산은 러너 절반이 받는 값과 같은
	// clampTimeout(0)=120s라 기존 여유가 줄지 않는다(아래 대조군 스폰 전부 동일).
	ctx, cancel := context.WithTimeout(t.Context(), clampTimeout(0))
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", file)
	cmd.Dir, cmd.Env = scratch, env
	cmd.WaitDelay = time.Second
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
	for _, kv := range mustEnv(pyEnv(scratch)) {
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
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), clampTimeout(0))
	defer cancelProbe()
	probe := exec.CommandContext(probeCtx, py, "-c", "import site; print(site.getusersitepackages())")
	probe.Env = sandbox.BaseEnv() // 자식과 같은 표로 물어야 자식이 볼 경로가 나온다
	probe.WaitDelay = time.Second
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
	for _, kv := range mustEnv(pyEnv(scratch)) {
		if !strings.HasPrefix(kv, "PYTHONNOUSERSITE=") {
			env = append(env, kv)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), clampTimeout(0))
	defer cancel()
	cmd := exec.CommandContext(ctx, py, file)
	cmd.Dir, cmd.Env = scratch, env
	cmd.WaitDelay = time.Second
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
	for _, kv := range mustEnv(jsEnv(scratch)) {
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
		for _, kv := range mustEnv(r.extra(sub)) {
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
	for _, kv := range mustEnv(jsEnv(scratch)) {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+fake, "XDG_CONFIG_HOME="+fake)
	ctx, cancel := context.WithTimeout(t.Context(), clampTimeout(0))
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, file)
	cmd.Dir, cmd.Env = scratch, env
	cmd.WaitDelay = time.Second
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
		ctx, cancel := context.WithTimeout(t.Context(), clampTimeout(0))
		defer cancel()
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			// npm은 .cmd 래퍼라 CreateProcess로 직접 실행되지 않는다 — cmd.exe 경유.
			// 이 갈래에서 ctx가 끝내는 것은 cmd.exe까지다(그 아래 node는 Job이 없어 남을 수
			// 있다) — WaitDelay가 파이프를 회수해 Output이 돌아오는 것을 보장한다.
			cmd = exec.CommandContext(ctx, "cmd", "/c", "npm", "config", "get", "registry")
		} else {
			cmd = exec.CommandContext(ctx, "npm", "config", "get", "registry")
		}
		cmd.Dir, cmd.Env = t.TempDir(), append(sandbox.BaseEnv(), extra...)
		cmd.WaitDelay = time.Second
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
	switch got := registry(mustEnv(jsEnv(t.TempDir()))); got {
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
	if err := os.WriteFile(file, snippetContent("snippet.ps1", code, filepath.Join(scratch, ctrNativeExitFile)), 0o600); err != nil {
		t.Fatalf("스니펫 기록 실패: %v", err)
	}
	env := sandbox.BaseEnv() // 닫힌 표 그대로 — PSModulePath는 이미 표 밖이다
	for _, kv := range mustEnv(r.extra(scratch)) {
		if !strings.HasPrefix(kv, "USERPROFILE=") {
			env = append(env, kv)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), clampTimeout(0))
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-NoProfile", "-NonInteractive", "-File", file)
	cmd.Dir, cmd.Env = scratch, env
	cmd.WaitDelay = time.Second
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
	if err := os.WriteFile(file, snippetContent("snippet.ps1", code, filepath.Join(scratch, ctrNativeExitFile)), 0o600); err != nil {
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
	for _, kv := range mustEnv(csEnv(scratch)) {
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
// TestExtraSetupFailureIsErrSetup: 격리 시행점의 준비 실패는 러너 오류(sandbox.ErrSetup)로
// 오르고 env는 비어서 돌아온다 — 경고만 남기고 계속하면 실재하지 않는 경로를 가리키는 env로
// 실행되고, pip·NuGet은 그 상태에서 호스트 사용자 구성을 다시 로드한다(exec.go runner.extra
// 주석의 분류 기준).
//
// 픽스처는 프로덕션에 훅을 심지 않고 실제 OS 오류를 만든다: 파일이 놓일 자리에 디렉터리를 미리
// 만들면 os.WriteFile이(unix EISDIR / windows 액세스 거부), 디렉터리가 놓일 자리에 파일을 미리
// 만들면 os.MkdirAll이(ENOTDIR) 실패한다. 각 케이스는 같은 함수가 블로커 없는 스크래치에서
// 정상 반환하는 것까지 확인해 블로커가 실패의 원인임을 고정한다.
func TestExtraSetupFailureIsErrSetup(t *testing.T) {
	cases := []struct {
		name    string                         // 시행점
		blocker string                         // 스크래치 하위에 미리 만들 상대 경로
		asDir   bool                           // true=디렉터리로 만든다(파일 기록을 막음), false=파일(디렉터리 생성을 막음)
		env     func(string) ([]string, error) // 대상 extra
	}{
		{"pip.conf", "pip.conf", true, pyEnv},
		{"nuget.config", "nuget.config", true, csEnv},
		{"nuget-migrations-sentinel", filepath.Join("xdg", "NuGet", "Migrations", "1"), true, csEnv},
		{"dotnet-cli-home", "dotnet", false, csEnv},
		{"gotmpdir", "go-tmp", false, goEnv},
		{"js-home", "home", false, jsEnv},
		{"cs-home", "home", false, csEnv},
		// unix 갈래만 헬퍼를 부른다 — windows 갈래는 PSModulePath·USERPROFILE을 유지한다.
		{"shell-home", "home", false, func(scratch string) ([]string, error) {
			if runtime.GOOS == "windows" {
				return nil, fmt.Errorf("%w: windows 갈래는 이 축의 대상이 아니다", sandbox.ErrSetup)
			}
			return table()["shell"].extra(scratch)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// shell-home은 unix 갈래만 대상이다 — windows 갈래는 이 축의 대상이 아니므로(위 표
			// 행의 windows 분기) 해피 패스 대조까지 포함해 이 서브테스트 전체를 건너뛴다.
			if tc.name == "shell-home" && runtime.GOOS == "windows" {
				t.Skip("windows 갈래는 홈 격리 축의 대상이 아니다 — PSModulePath·USERPROFILE을 유지한다")
			}
			scratch := t.TempDir()
			blocker := filepath.Join(scratch, tc.blocker)
			if tc.asDir {
				if err := os.MkdirAll(blocker, 0o700); err != nil {
					t.Fatalf("블로커 디렉터리 생성 실패: %v", err)
				}
			} else if err := os.WriteFile(blocker, nil, 0o600); err != nil {
				t.Fatalf("블로커 파일 기록 실패: %v", err)
			}
			kv, err := tc.env(scratch)
			if !errors.Is(err, sandbox.ErrSetup) {
				t.Fatalf("격리 준비 실패가 ErrSetup으로 오르지 않았다: err=%v", err)
			}
			if kv != nil {
				t.Errorf("실패 시 env를 돌려주면 격리가 빠진 채로 실행된다: %v", kv)
			}
			// 블로커가 없으면 같은 함수가 정상 반환한다(해피 패스 대조).
			if kv, err := tc.env(t.TempDir()); err != nil || len(kv) == 0 {
				t.Fatalf("깨끗한 스크래치에서 준비 실패: kv=%v err=%v", kv, err)
			}
		})
	}
}

// TestShellExtraModuleDirFailureIsErrSetup: windows shell 러너의 psmodules 생성 실패도 시행점이다.
// PSModulePath 첫 항목이 실재해야 5.1 프로바이더 cmdlet 영구 정지를 피한다(psModulePath 주석의
// 실측) — 부재 경로를 첫 항목으로 주고 실행하면 그 정지가 타임아웃까지 매달린다.
func TestShellExtraModuleDirFailureIsErrSetup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows 갈래의 psmodules 시행점 전용 — unix 갈래는 홈 격리 축을 shell-home 행이 덮는다")
	}
	r := table()["shell"]
	if _, _, err := r.detect(); err != nil { // extra가 읽는 psHome을 detect가 채운다(Run과 같은 순서)
		t.Skipf("pwsh/powershell 미설치: %v", err)
	}
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "psmodules"), nil, 0o600); err != nil {
		t.Fatalf("블로커 파일 기록 실패: %v", err)
	}
	kv, err := r.extra(scratch)
	if !errors.Is(err, sandbox.ErrSetup) {
		t.Fatalf("모듈 디렉터리 생성 실패가 ErrSetup으로 오르지 않았다: err=%v", err)
	}
	if kv != nil {
		t.Errorf("실패 시 env를 돌려주면 부재 경로가 첫 항목인 PSModulePath로 실행된다: %v", kv)
	}
	if kv, err := r.extra(t.TempDir()); err != nil || len(kv) == 0 {
		t.Fatalf("깨끗한 스크래치에서 준비 실패: kv=%v err=%v", kv, err)
	}
}

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

// TestRunnerLabelNodeLeg — D70: bun이 없는 환경에서 javascript·typescript가 node 레그를 탄다.
// 라벨은 실행 파일 basename이라 windows에서 ".exe"가 붙으므로 확장자를 제거하고 비교한다.
// bun이 설치된 환경에서는 detectJS가 bun을 고르므로 이 단정이 성립하지 않는다 — SKIP한다
// (거짓 실패를 만들지 않는다). CI의 bun 없는 잡이 이 테스트의 실행 지점이다.
func TestRunnerLabelNodeLeg(t *testing.T) {
	if _, err := exec.LookPath("bun"); err == nil {
		t.Skip("bun 설치됨 — detectJS가 bun을 고르므로 node 레그가 아니다")
	}
	for _, lang := range []string{"javascript", "typescript"} {
		t.Run(lang, func(t *testing.T) {
			requireLang(t, lang) // exec_test.go:157 — 미설치 호스트는 Skip(Fatal 금지, 저장소 관례)
			// run(t, req)(exec_test.go:164)은 Run(ctx, t.TempDir(), selfExe(t), req)를 부른다.
			// selfExe를 빈 문자열로 넘기면 linux의 sandbox.Run이 ErrSetup으로 즉시 반환하므로
			// (run_unix.go:34) 이 잡(ubuntu)에서 Run을 직접 부르면 안 된다.
			resp := run(t, Request{Language: lang, Code: "console.log(1)"})
			// runnerLabel은 filepath.Base(bin) + (version이 있으면) " " + version이다
			// (internal/exec/exec.go:237-243) — 첫 토큰을 떼고 확장자를 제거해 비교한다.
			first := strings.Fields(resp.Runner)[0]
			base := strings.TrimSuffix(first, filepath.Ext(first))
			if base != "node" {
				t.Fatalf("runner=%q base=%q want node", resp.Runner, base)
			}
		})
	}
}

// TestHomeIsolationEnvAnchors — D75: 헬퍼가 세 앵커와 준비물을 소유한다. 플랫폼 분기가 없다.
func TestHomeIsolationEnvAnchors(t *testing.T) {
	scratch := t.TempDir()
	env, err := homeIsolationEnv(scratch)
	if err != nil {
		t.Fatalf("homeIsolationEnv: %v", err)
	}
	home := filepath.Join(scratch, "home")
	xdg := filepath.Join(scratch, "xdg")
	for _, want := range []string{"HOME=" + home, "CFFIXED_USER_HOME=" + home, "XDG_DATA_HOME=" + xdg} {
		if !slices.Contains(env, want) {
			t.Fatalf("env에 %q 없음: %v", want, env)
		}
	}
	for _, p := range []string{home, filepath.Join(home, "Library", "Application Support"), filepath.Join(xdg, "NuGet", "Migrations")} {
		if fi, statErr := os.Stat(p); statErr != nil || !fi.IsDir() {
			t.Fatalf("준비물 디렉터리 부재: %s (%v)", p, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(xdg, "NuGet", "Migrations", "1")); statErr != nil {
		t.Fatalf("NuGet sentinel 부재: %v", statErr)
	}
}

// TestHomeIsolationEnvFailClosed — D75: 준비물 생성이 실패하면 실행을 거부한다. 홈이 없으면
// 재지정이 성립하지 않고 그 상태로 실행하면 호스트 홈으로 되돌아갈 수 있다.
func TestHomeIsolationEnvFailClosed(t *testing.T) {
	scratch := t.TempDir()
	// 파일 자리에 디렉터리를 만들 수 없게 해 실제 OS 오류를 낸다(v0.12의 extra fail-closed
	// 픽스처와 같은 방식).
	if err := os.WriteFile(filepath.Join(scratch, "home"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := homeIsolationEnv(scratch)
	if !errors.Is(err, sandbox.ErrSetup) {
		t.Fatalf("err=%v want sandbox.ErrSetup", err)
	}
	if env != nil {
		t.Fatalf("env=%v want nil — 격리가 빠진 env를 호출부에 넘기지 않는다", env)
	}
}

// TestRunnerIsolationKeySets — D75: 통합 후 각 러너의 유효 격리 키 집합을 고정한다. js·shell이
// 얻는 키가 늘어나는 것은 의도된 변경이므로 "기존과 동일"을 요구하지 않는다 — 변화 없음을
// 단정하는 대상은 csharp(이미 세 앵커를 전부 준다)과 windows shell(헬퍼를 부르지 않는다)이다.
func TestRunnerIsolationKeySets(t *testing.T) {
	scratch := t.TempDir()
	anchors := []string{"HOME", "CFFIXED_USER_HOME", "XDG_DATA_HOME"}
	tbl := table()
	var csharpEnv []string
	for _, lang := range []string{"javascript", "typescript", "csharp"} {
		env, err := tbl[lang].extra(scratch)
		if err != nil {
			t.Fatalf("%s extra: %v", lang, err)
		}
		for _, k := range anchors {
			if !hasEnvKeyT(env, k) {
				t.Fatalf("%s에 앵커 %s 없음", lang, k)
			}
		}
		if lang == "csharp" {
			csharpEnv = env
		}
	}
	// csharp은 §2⑤가 "변화 없음"을 요구하는 대상이다 — 키 존재만으로는 값이 바뀌어도(예: 헬퍼가
	// 다른 하위 경로를 쓰게 되어도) 통과하므로 값까지 고정한다. csEnv는 D75 이전에도 이미 이
	// 경로였다(homeIsolationEnv와 같은 산식) — 여기서 값이 달라지면 D75가 csharp에 대해 "무변화"를
	// 지키지 못했다는 뜻이다.
	wantHome, wantXDG := filepath.Join(scratch, "home"), filepath.Join(scratch, "xdg")
	wantVals := map[string]string{"HOME": wantHome, "CFFIXED_USER_HOME": wantHome, "XDG_DATA_HOME": wantXDG}
	for _, k := range anchors {
		if got := envValueT(csharpEnv, k); got != wantVals[k] {
			t.Fatalf("csharp %s=%q want %q", k, got, wantVals[k])
		}
	}
	shell := tbl["shell"]
	if runtime.GOOS == "windows" {
		// windows 갈래의 extra는 detect가 채우는 psHome·pfDir를 읽는다(exec.go:335-338의 계약) —
		// detect를 먼저 부르지 않으면 값이 빈 채로 조립된다. 미설치면 Skip한다
		// (TestShellExtraModuleDirFailureIsErrSetup:1189-1191 선례).
		if _, _, err := shell.detect(); err != nil {
			t.Skipf("windows shell 미설치: %v", err)
		}
		env, err := shell.extra(scratch)
		if err != nil {
			t.Fatalf("windows shell extra: %v", err)
		}
		if !hasEnvKeyT(env, "PSModulePath") || !hasEnvKeyT(env, "USERPROFILE") {
			t.Fatalf("windows shell 기존 2키가 유지되지 않았다: %v", env)
		}
		// 키 존재만 보면 detect 누락이 드러나지 않는다 — 값까지 단정한다.
		if got := envValueT(env, "PSModulePath"); !strings.HasPrefix(got, filepath.Join(scratch, "psmodules")) {
			t.Fatalf("PSModulePath 첫 항목이 스크래치가 아니다: %q", got)
		}
		if got := envValueT(env, "USERPROFILE"); got != scratch {
			t.Fatalf("USERPROFILE=%q want %q", got, scratch)
		}
		for _, k := range anchors {
			if hasEnvKeyT(env, k) {
				t.Fatalf("windows shell이 헬퍼 앵커 %s를 얻었다 — 호출 지점 규정 위반", k)
			}
		}
		return
	}
	env, err := shell.extra(scratch)
	if err != nil {
		t.Fatalf("unix shell extra: %v", err)
	}
	for _, k := range anchors {
		if !hasEnvKeyT(env, k) {
			t.Fatalf("unix shell에 앵커 %s 없음", k)
		}
	}
}

func hasEnvKeyT(env []string, key string) bool {
	return envValueT(env, key) != ""
}

// envValueT — 마지막 값이 이긴다(Run이 BaseEnv 뒤에 extra를 붙이는 규칙과 같다). 부재는 "".
func envValueT(env []string, key string) string {
	out := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			out = v
		}
	}
	return out
}

// TestRunShellHomeToolchainStillWorks — D75: 홈 재지정이 sh 스니펫의 홈 유도 툴체인 호출을
// 깨뜨리지 않는다. sentinel 준비가 빠지면 실패하는 경로를 덮는다. 리뷰 F1: exit code만으로는
// 냉각된 홈의 first-run 배너가 stdout에 섞여도 통과하므로 stdout 형태까지 본다 — 단, 이
// 단정은 이 Windows 호스트에서 실행되지 않는다(위 Skip). dotnet을 설치한 CI ubuntu·macos가
// 실행 지점이며, 로컬에서 검증되었다고 적지 않는다.
func TestRunShellHomeToolchainStillWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shell 갈래 전용")
	}
	if _, err := exec.LookPath("dotnet"); err != nil { // 이 파일에서 exec는 os/exec다(별칭 없음)
		t.Skip("dotnet 없음")
	}
	// run(t, req)(exec_test.go:164)은 Run(ctx, t.TempDir(), selfExe(t), req)를 부른다 — selfExe에
	// 빈 문자열을 넘기면 linux의 sandbox.Run이 ErrSetup으로 즉시 반환한다(run_unix.go:34).
	resp := run(t, Request{Language: "shell", Code: "set -e\ndotnet --version\n"})
	if resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("dotnet --version 실패: exit=%v stderr=%q", resp.ExitCode, resp.Stderr)
	}
	// 리뷰 F1: dotnetFirstRunEnv(shellEnv 경유)가 빠지면 냉각된 HOME이 first-run으로 보여 배너·
	// 텔레메트리 안내가 stdout에 섞인다. 정확한 배너 문구는 SDK 버전마다 달라 문자열 매칭 대신
	// 형태로 본다 — 정상 출력은 버전 번호 한 줄뿐이므로, 트레일링 개행을 뗀 뒤 그 안에 개행이
	// 더 있으면(또는 아예 비어 있으면) 배너 유입 의심으로 실패시킨다.
	got := strings.TrimRight(resp.Stdout, "\n")
	if got == "" || strings.Contains(got, "\n") {
		t.Fatalf("dotnet --version stdout이 버전 한 줄이 아니다(first-run 배너 유입 의심): %q", resp.Stdout)
	}
}

// TestRunShellScratchHomeWins — D75 §2 ①: 격리 없는 절반은 호스트 홈 유도 구성을 읽고, 격리된
// 절반은 읽지 않는다. windows의 같은 축(USERPROFILE·PSModulePath)은
// TestRunShellScratchModulePathWins(exec_test.go:1052)가 이미 덮으므로 그 A/B 구성 방식을
// unix HOME 축으로 복제한다 — **그 테스트를 먼저 읽고 절반을 만드는 형태를 그대로 따른다**
// (격리 없는 절반을 어떻게 조립하는지가 거기에 있다).
func TestRunShellScratchHomeWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shell 갈래 전용 — windows 축은 exec_test.go:1052가 덮는다")
	}
	requireLang(t, "shell")
	// 픽스처를 심기 전에 sync.Once 빌드를 끝낸다 — 심은 뒤면 그 go build가 HOME 유도 GOPATH/
	// GOCACHE 기본값을 잃고 깨지며 실패가 testExeErr에 캐시돼 패키지 전체가 오염된다(다른 HOME·
	// USERPROFILE 픽스처 테스트와 같은 선례 — exec_test.go의 selfExe(t) 선호출 관례).
	selfExe(t)

	fakeHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeHome, "ctr-marker"), []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome) // BaseEnv의 허용 표가 이 값을 복사한다(sandbox.go:131)

	const code = "test -f \"$HOME/ctr-marker\" && echo SEEN || echo ABSENT\n"

	// 격리된 절반: 실제 러너 경로 — extra가 홈을 스크래치로 돌린다.
	got := run(t, Request{Language: "shell", Code: code})
	if !strings.Contains(got.Stdout, "ABSENT") {
		t.Fatalf("격리된 절반이 호스트 홈 마커를 봤다: stdout=%q stderr=%q", got.Stdout, got.Stderr)
	}

	// 격리 없는 절반: extra를 적용하지 않고 같은 스니펫을 직접 돌려 SEEN을 본다(:890과 같은
	// 방식 — 프로덕션에 토글을 심지 않기 위해 이 절반만 테스트가 환경을 조립한다). 이 대조가
	// 없으면 위 단정은 마커 생성이 실패해도 통과한다(거짓 clean).
	r := table()["shell"]
	bin, _, err := r.detect()
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	scratch := t.TempDir()
	file := filepath.Join(scratch, "snippet.sh")
	if err := os.WriteFile(file, []byte(code), 0o600); err != nil {
		t.Fatalf("스니펫 기록 실패: %v", err)
	}
	env := sandbox.BaseEnv() // 닫힌 표가 HOME=fakeHome을 그대로 복사한다 — extra 미적용
	ctx, cancel := context.WithTimeout(t.Context(), clampTimeout(0))
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, file)
	cmd.Dir, cmd.Env = scratch, env
	cmd.WaitDelay = time.Second
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("격리 없는 절반이 실행 자체에 실패했다: %v: %s", err, raw)
	}
	if !strings.Contains(string(raw), "SEEN") {
		t.Fatalf("픽스처 무효 — 격리 없이도 호스트 홈 마커가 보이지 않았다: %s", raw)
	}
}

// TestPromotesNativeExitSignal — D78 미니 파서. 최상단 전용 구문으로 시작하는 스니펫은
// 승격하지 않는다(설계 v0.14 D78의 4단계 절차). 미탐만이 회귀이고 과잉 폴백은 기존 동작
// 유지이므로, 판정을 넓게 잡는 쪽이 안전 방향이다.
func TestPromotesNativeExitSignal(t *testing.T) {
	fallback := []struct{ name, code string }{
		{"param", "param($x)\ncmd /c exit 74"},
		{"param-with-default", "param($x = \"dflt\")\nWrite-Output $x"},
		{"param-uppercase", "PARAM ($x)\ncmd /c exit 74"},
		{"block-comment-then-param", "<# doc #> param($x)\ncmd /c exit 74"},
		{"multiline-block-comment-then-param", "<#\n doc\n line2\n#>\nparam($x)\ncmd /c exit 74"},
		{"attribute-then-param", "[CmdletBinding()] param($x)\ncmd /c exit 74"},
		{"using", "using namespace System.Text\ncmd /c exit 78"},
		{"bom-then-param", "\uFEFFparam($x)\ncmd /c exit 74"},
		{"backtick-continuation-then-using", "`\nusing namespace System.Text\ncmd /c exit 78"},
		{"dynamicparam", "dynamicparam { }\nend { cmd /c exit 79 }"},
		{"begin", "begin { }\nprocess { }\nend { cmd /c exit 79 }"},
		{"process", "process { }\nend { cmd /c exit 79 }"},
		{"end", "end { cmd /c exit 79 }"},
		{"clean", "clean { }\nend { cmd /c exit 79 }"},
		{"type-cast-overfallback", "[int]$x = 5\ncmd /c exit 70"},
		// PowerShell은 이 줄 전체를 라인 주석으로 처리하므로 다음 줄의 param이 살아 있다.
		// 좌→우 우선순위(현재 위치 첫 문자로만 판정)를 따르면 라인 주석을 소비한 뒤
		// param을 만나 폴백한다. `<#`를 먼저 찾는 구현은 짝 없는 `#>`를 찾아 남은 입력을
		// 전부 삼키고 미탐을 낸다 — 그것을 막는 것이 이 케이스다.
		{"line-comment-containing-block-open", "# see <# note\nparam($x)\ncmd /c exit 74"},
		// PowerShell 언어 사양의 공백은 " \t\r\n" 넷보다 넓다 — form feed·vertical tab·
		// Unicode Zs(NBSP)도 공백이다. 검수 실측(pwsh 7·powershell 5.1 양쪽): 아래 셋은
		// 현재 형태에서 정상 실행(exit 0 · side :78 · stdout USING-OK)인데 승격하면
		// ParserError로 exit 1 · 사이드파일 없음 · stdout 빈 값이 된다.
		// nbsp 케이스의 선두 문자는 U+00A0이라 **화면에 보이지 않는다** — 복사·편집에서
		// 일반 공백(U+0020)으로 바뀌면 그 케이스가 무효가 되므로 바이트로 확인할 것.
		{"formfeed-then-using", "\x0Cusing namespace System.Text\nWrite-Output 'ok'\ncmd /c exit 78"},
		{"verttab-then-using", "\x0Busing namespace System.Text\nWrite-Output 'ok'\ncmd /c exit 78"},
		{"nbsp-then-using", " using namespace System.Text\nWrite-Output 'ok'\ncmd /c exit 78"},
		// PowerShell은 **단독 CR도 줄 종료**로 처리한다(실측 확인). LF만 찾는 구현은
		// 주석이 다음 LF까지 이어진다고 보고 using을 지나쳐 승격한다 — 같은 ParserError.
		{"cr-line-comment-then-using", "# note\rusing namespace System.Text\nWrite-Output 'ok'\ncmd /c exit 78"},
	}
	for _, tc := range fallback {
		t.Run("fallback/"+tc.name, func(t *testing.T) {
			if promotesNativeExitSignal(tc.code) {
				t.Fatalf("승격됐다 — 최상단 전용 구문이므로 폴백해야 한다\ncode=%q", tc.code)
			}
		})
	}

	promote := []struct{ name, code string }{
		{"plain", "cmd /c exit 71\nexit 3"},
		{"line-comment-first", "# just a comment\ncmd /c exit 82"},
		{"requires-first", "#requires -Version 5\ncmd /c exit 81"},
		{"strictmode-first", "Set-StrictMode -Version Latest\n\"hello\""},
		{"variable-first", "$x = 1\ncmd /c exit 77"},
		{"param-inside-function", "function f { param($y) }\nf\ncmd /c exit 70"},
		// 선두에 `$`를 두지 않는 것이 이 케이스의 전부다 — `$`로 시작하면 토큰 스캔이
		// j=0에서 끝나 **빈 토큰**을 판정하므로, `strings.HasPrefix(s, "param")` 같은
		// 접두어 구현도 똑같이 승격시켜 "전체 토큰 일치"와 "접두어 일치"가 구분되지
		// 않는다(최종 리뷰 M3). 식별자 그대로 두면 접두어 구현에서만 폴백으로 뒤집힌다.
		{"paramx-identifier", "paramX 1\ncmd /c exit 70"},
		{"unterminated-block-comment", "<# never closed\ncmd /c exit 70"},
		{"empty", ""},
	}
	for _, tc := range promote {
		t.Run("promote/"+tc.name, func(t *testing.T) {
			if !promotesNativeExitSignal(tc.code) {
				t.Fatalf("폴백됐다 — 승격 대상이다\ncode=%q", tc.code)
			}
		})
	}
}

// TestSnippetContentRegistration — D78: 승격 대상은 BOM 다음에 등록 문이 오고 그 뒤에
// 스니펫이 **같은 줄로** 이어진다. 앞 줄에 두면 모든 오류의 줄 번호가 +1 밀린다.
func TestSnippetContentRegistration(t *testing.T) {
	const side = `C:\tmp\ctr-native-exit`
	got := string(snippetContent("snippet.ps1", "cmd /c exit 71\nexit 3", side))

	if !strings.HasPrefix(got, "\xEF\xBB\xBF") {
		t.Fatal("BOM이 없다 — powershell 5.1이 비ASCII를 손상시킨다(I2 계약)")
	}
	body := strings.TrimPrefix(got, "\xEF\xBB\xBF")
	if !strings.HasPrefix(body, "Register-EngineEvent ") {
		t.Fatalf("등록 문이 선두가 아니다: %.80q", body)
	}
	if !strings.Contains(body, "-SupportEvent") {
		t.Fatal("-SupportEvent가 없다 — 세션 작업표에 PSEventJob이 남아 Get-Job | Wait-Job이 끝나지 않는다")
	}
	if !strings.Contains(body, "$LASTEXITCODE") {
		t.Fatal("$LASTEXITCODE를 읽지 않는다")
	}
	// 인라인 결합: 등록 문과 스니펫 첫 줄 사이에 개행이 없어야 한다.
	head, _, ok := strings.Cut(body, "\n")
	if !ok {
		t.Fatal("개행이 없다")
	}
	if !strings.Contains(head, "; cmd /c exit 71") {
		t.Fatalf("스니펫 첫 줄이 등록 문과 같은 줄에 있지 않다: %.200q", head)
	}
}

// TestSnippetContentEscapesSidePath — D78: sidePath의 작은따옴표는 이중화한다. 빠지면
// 사용자명에 '가 있는 호스트에서 스크립트 자체가 파싱되지 않아 모든 shell 스니펫이
// 실패한다(D76 tail이 nativeExitTail에서 하는 처리와 같다).
func TestSnippetContentEscapesSidePath(t *testing.T) {
	const side = `C:\Users\o'brien\tmp\ctr-native-exit`
	got := string(snippetContent("snippet.ps1", "cmd /c exit 71", side))
	if !strings.Contains(got, `o''brien`) {
		t.Fatalf("작은따옴표가 이중화되지 않았다: %.240q", got)
	}
	if strings.Contains(got, `'C:\Users\o'brien`) {
		t.Fatal("원본 작은따옴표가 그대로 남아 문자열 리터럴이 조기 종료된다")
	}
	// 폴백 경로(nativeExitTail)도 같은 요건이다. 위 스니펫은 승격 대상이라 tail이 없으므로
	// 이 단정 없이는 tail 쪽 이스케이프를 보는 곳이 하나도 남지 않는다 —
	// TestSnippetContentFallbackKeepsD76Form의 골든은 작은따옴표가 없는 sidePath를 쓴다.
	fb := string(snippetContent("snippet.ps1", "param($x)\ncmd /c exit 71", side))
	if !strings.Contains(fb, `o''brien`) || strings.Contains(fb, `'C:\Users\o'brien`) {
		t.Fatalf("폴백 경로의 작은따옴표가 이중화되지 않았다: %.240q", fb)
	}
}

// TestSnippetContentFallbackKeepsD76Form — D78: 폴백 대상은 현재 D76 형태와 **바이트
// 동일**해야 한다. §2.5·§2.6의 대조 논거 전체가 이 동일성에 걸려 있다 — runPS1의
// promote=false 대조군이 "현재 형태"라는 전제가 여기서 무너지면 이후 모든 대조가
// 조용히 무의미해진다. 그래서 부분 문자열이 아니라 골든 리터럴로 잠근다(선행 개행
// 하나, 줄 순서 하나가 달라져도 잡힌다).
func TestSnippetContentFallbackKeepsD76Form(t *testing.T) {
	const side = `C:\tmp\ctr-native-exit`
	const code = "param($x)\ncmd /c exit 74"
	// D76 tail 골든(216B). 마커를 상수 대신 리터럴로 적는 것은 의도적이다 —
	// ctrNativeExitMarker가 바뀌면 이 테스트가 먼저 깨져 D76 무회귀를 알린다.
	const wantTail = "\ntry {\n" +
		"$ctrNativeExitCode = Get-Variable -Name LASTEXITCODE -ValueOnly -ErrorAction SilentlyContinue\n" +
		"[System.IO.File]::WriteAllText('" + `C:\tmp\ctr-native-exit` + "', '" +
		"ctr-exit-code:" + "' + [string]$ctrNativeExitCode)\n" +
		"} catch {}\n"

	got := string(snippetContent("snippet.ps1", code, side))
	if strings.Contains(got, "Register-EngineEvent") {
		t.Fatal("폴백 대상에 등록 문이 붙었다")
	}
	if want := "\xEF\xBB\xBF" + code + wantTail; got != want {
		t.Fatalf("폴백 산출물이 D76 형태와 바이트 동일하지 않다\ngot  %q\nwant %q", got, want)
	}
}

// TestSnippetContentNonPS1Unchanged — .ps1이 아니면 원본 그대로다(기존 계약).
func TestSnippetContentNonPS1Unchanged(t *testing.T) {
	const code = "echo hi"
	if got := string(snippetContent("snippet.sh", code, "/tmp/x")); got != code {
		t.Fatalf("got %q want %q", got, code)
	}
}

// psBin — 셸을 고른다. name이 빈 문자열이면 러너와 같은 순서(pwsh → powershell)로
// 고른다(shellRunner의 detect 순서와 일치 — exec.go:401-411). name을 주면 그 셸만 쓰고
// 부재 시 Skip한다 — 5.1 레그를 명시적으로 밟는 관용구는 위
// TestRunShellPS51ProviderWriteReturns가 선례다.
func psBin(t *testing.T, name string) string {
	t.Helper()
	if name != "" {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s 미해석: %v", name, err)
		}
		return p
	}
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p
	}
	p, err := exec.LookPath("powershell")
	if err != nil {
		t.Skipf("pwsh/powershell 미설치: %v", err)
	}
	return p
}

// runPS1Shell — .ps1 스니펫을 러너와 같은 형태로 돌리고 (종료 코드, stdout, stderr,
// 사이드파일)을 준다.
//
// dir을 인자로 받는 이유: PowerShell 오류 메시지에는 스크립트의 절대 경로가 박힌다.
// 승격본과 대조군을 서로 다른 t.TempDir()에서 돌리면 stderr가 경로에서 갈려 §2.6의
// 바이트 비교가 승격 여부와 무관하게 **항상** 실패한다(검수 실측: 같은 dir이면 바이트
// 동일, 다른 dir이면 항상 불일치).
//
// promote=false면 등록 문 없이 D76 tail만 붙인 대조군을 만든다 — 승격 대상은 트리 안에
// 등록 문 없는 경로가 없으므로 대조군은 테스트가 직접 조립해야 한다(§2.6).
//
// 타임아웃: -SupportEvent 회귀가 나면 `Get-Job | Wait-Job`이 끝나지 않는다(설계 §3 표6).
// 그 감시선이 물리는 순간 무한 대기가 되고 go test 기본 10분 패닉으로만 끝난다. 그래서
// probeVersion(exec.go:639-646)의 CommandContext + WaitDelay 관용구를 그대로 쓰고,
// 타임아웃을 일반 실패와 구분해 진단 가능하게 만든다. 예산 근거: 5.1 기동 실측
// 170~194ms, 건강한 실행은 1s 안쪽이다(TestRunShellPS51ProviderWriteReturns 선례).
func runPS1Shell(t *testing.T, shell, dir, code string, promote bool) (exitCode int, stdout, stderr, side string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("windows shell 갈래 전용(PowerShell -File 계약)")
	}
	bin := psBin(t, shell)
	// 환경도 러너가 만드는 것을 그대로 재현한다(exec.go:133 + shellRunner의 extra). cmd.Env를
	// nil로 두면 호스트 환경 **전체**가 상속되어(sandbox.go:134-136이 경고하는 그 경로) D65가
	// 일부러 떼어낸 호스트 PSModulePath가 스니펫에 닿고, 5.1의 실행 정책이 테스트를 띄운
	// 세션의 상속 값에 따라 갈린다. 조립을 손으로 하는 것은 detect가 pwsh를 먼저 고르기
	// 때문이다 — 5.1을 명시하는 레그는 직접 조립해야 한다(TestRunShellPS51ProviderWriteReturns
	// 와 같은 방식).
	pfDir := "WindowsPowerShell" // detect: pwsh → PowerShell, powershell → WindowsPowerShell
	if strings.HasPrefix(strings.ToLower(filepath.Base(bin)), "pwsh") {
		pfDir = "PowerShell"
	}
	mods := filepath.Join(dir, "psmodules") // extra와 같은 배치·같은 이름
	if err := os.MkdirAll(mods, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(sandbox.BaseEnv(), tmpEnv(dir)...)
	env = append(env, "PSModulePath="+psModulePath(mods, filepath.Dir(bin), pfDir), "USERPROFILE="+dir)

	sidePath := filepath.Join(dir, ctrNativeExitFile)
	_ = os.Remove(sidePath) // 같은 dir을 재사용하는 호출(§2.6 대조)에서 이전 값이 남지 않게 한다
	var content []byte
	if promote {
		content = snippetContent("snippet.ps1", code, sidePath)
	} else {
		buf := append([]byte{0xEF, 0xBB, 0xBF}, code...)
		content = append(buf, nativeExitTail(sidePath)...)
	}
	file := filepath.Join(dir, "snippet.ps1")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-NoProfile", "-NonInteractive", "-File", file)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = time.Second
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("스니펫이 10s 예산 안에 끝나지 않았다 — -SupportEvent 회귀 의심"+
			"(Get-Job | Wait-Job이 이벤트 작업을 기다린다): %v", ctx.Err())
	}
	if err != nil && cmd.ProcessState == nil {
		t.Fatalf("인터프리터를 시작하지 못했다: %v", err) // 비영 종료는 정상 경로이므로 err 자체는 무시한다
	}
	if b, rerr := os.ReadFile(sidePath); rerr == nil {
		side = string(b)
	}
	return cmd.ProcessState.ExitCode(), so.String(), se.String(), side
}

// runPS1 — 대부분의 케이스용 래퍼. 셸은 러너 순서로 고르고 스크래치는 새로 만든다.
// stderr를 대조하는 테스트는 이 래퍼를 쓰지 말고 runPS1Shell에 같은 dir을 넘긴다.
func runPS1(t *testing.T, code string, promote bool) (int, string, string, string) {
	t.Helper()
	return runPS1Shell(t, "", t.TempDir(), code, promote)
}

// TestD78SignalOnExit — D78이 닫는 공백 ①②. exit로 끝나는 스니펫에서도 사이드파일이
// 생기고 마지막 외부 명령의 코드가 담긴다.
func TestD78SignalOnExit(t *testing.T) {
	cases := []struct {
		name, code, want string
	}{
		{"explicit-exit", "cmd /c exit 71\nexit 3", ctrNativeExitMarker + "71"},
		{"bare-exit", "cmd /c exit 76\nexit", ctrNativeExitMarker + "76"},
		// exit 0은 인수 없는 exit과 별개 사례로 남긴다 — 보강의 트리거는 "인수 없는
		// exit"이 아니라 exit_code == 0(exec.go:189)이라, 사용자에게 실제로 새 줄이
		// 붙는 자리가 이 둘 **모두**다(CHANGELOG 0.14.0). 인수 없는 exit만 태우면
		// 릴리스 문면이 약속한 절반에 감시선이 없다(최종 리뷰).
		{"exit-zero", "cmd /c exit 71\nexit 0", ctrNativeExitMarker + "71"},
		{"throw", "cmd /c exit 73\nthrow 'boom'", ctrNativeExitMarker + "73"},
		{"normal", "cmd /c exit 72", ctrNativeExitMarker + "72"},
	}
	// 양쪽 셸을 명시적으로 돈다. runPS1은 pwsh를 먼저 잡으므로 그것만 쓰면 pwsh와 5.1이
	// 모두 있는 GitHub windows 러너에서 제품의 5.1 폴백 갈래(shellRunner detect, runner
	// 라벨 `5.1`)를 새 테스트가 한 번도 밟지 않는다 — D78이 바꾸는 것이 이벤트 서브시스템·
	// -SupportEvent·핸들러 실행 시점이라 셸 버전에 민감한 표면이다. 부재 셸은 psBin이
	// Skip한다(TestRunShellPS51ProviderWriteReturns 선례).
	//
	// 5.1 갈래가 다섯 사례 **모두** side=""로 실패하면 그것은 D78이 아니라 호스트의
	// ExecutionPolicy다 — 스니펫을 파싱하기도 전에 load-time UnauthorizedAccess로 끝나고,
	// 그 사유는 stderr에만 있다(아래 Fatalf가 함께 낸다). 5.1은 pwsh와 정책 레지스트리 키가
	// 별개이며, 미설정 시 기본값은 **SKU에 따라 다르다**: 클라이언트 SKU는 Restricted라
	// 이 차단이 나고, CI windows 러너가 쓰는 서버 SKU는 LocalMachine 기본값이 RemoteSigned라
	// 서명 없는 로컬 .ps1이 그대로 돈다 — 개발기가 빨간불이어도 CI는 이 갈래를 실제로 밟는다.
	//
	// D78 부재는 그 서명을 내지 않는다: 폴백 tail이 그대로 도니 normal은 통과하고
	// exit·throw로 끝나는 넷만 실패한다(Task 3 · 최종 리뷰 유도 FAIL 실측). 정책 차단을
	// Skip으로 바꾸는 감시선은 넣지 않는다 — Get-ExecutionPolicy는 5.1에서
	// Microsoft.PowerShell.Security 자동 로드에 실패할 수 있어(실측:
	// CouldNotAutoloadMatchingModule) 정책과 무관하게 이 갈래를 통째로 건너뛰게 만든다.
	// 5.1 갈래의 실검증 권위는 CI windows 잡이다.
	for _, shell := range []string{"pwsh", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					_, _, errOut, side := runPS1Shell(t, shell, t.TempDir(), tc.code, true)
					if side != tc.want {
						// stderr를 함께 낸다 — 이 갈래의 흔한 실패 원인(로드 단계 정책 차단)은
						// 사이드파일 값만으로는 보이지 않고 거기에만 적혀 있다.
						t.Fatalf("side=%q want %q — stderr=%q", side, tc.want,
							errOut[:min(len(errOut), 400)])
					}
				})
			}
		})
	}
}

// TestD78ExitCodeUnchanged — 승격 대상에서 종료 코드는 바뀌지 않는다(설계 §2.2).
func TestD78ExitCodeUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"explicit-exit", "cmd /c exit 71\nexit 3"},
		{"normal", "cmd /c exit 72"},
		{"throw", "cmd /c exit 73\nthrow 'boom'"},
		{"bare-exit", "cmd /c exit 76\nexit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, _, _, _ := runPS1(t, tc.code, false)
			got, _, _, _ := runPS1(t, tc.code, true)
			if got != base {
				t.Fatalf("종료 코드가 바뀌었다: 승격 %d vs 대조군 %d", got, base)
			}
		})
	}
}

// TestD78BodyRunsToCompletion — 설계 §2.3. 문 종료 오류가 있어도 마지막 문장이 실행된다.
// 종료 코드는 양쪽 0이라 §2.2 단정으로는 검출되지 않는다 — 이 단정이 try/finally 회귀를
// 막는 감시선이다(래핑하면 사이드파일이 41이 된다).
func TestD78BodyRunsToCompletion(t *testing.T) {
	const code = "cmd /c exit 41\nNotACmd999\ncmd /c exit 69"
	_, _, _, side := runPS1(t, code, true)
	if want := ctrNativeExitMarker + "69"; side != want {
		t.Fatalf("side=%q want %q — 오류 뒤 마지막 문장이 실행되지 않았다(try/finally 회귀)", side, want)
	}
}

// TestD78StderrNoDrift — 설계 §2.6. 오류가 2행 이상인 스니펫에서 stderr가 대조군과
// 바이트 동일해야 한다. 등록 문을 앞 줄에 두면 여기서 줄 번호 +1 드리프트가 잡힌다.
// param을 사례로 쓰지 않는다 — 승격해도 stderr가 0B라 비교할 오류가 없다.
func TestD78StderrNoDrift(t *testing.T) {
	const code = "cmd /c exit 41\nthrow 'boom'" // 오류가 2행에서 난다(검수 실측 확인)
	// 같은 스크래치를 재사용한다 — 다른 TempDir이면 stderr에 박히는 스크립트 절대 경로가
	// 달라 승격 여부와 무관하게 항상 불일치하고, 이 감시선이 무조건 FAIL한다.
	dir := t.TempDir()
	_, _, baseErr, _ := runPS1Shell(t, "", dir, code, false)
	_, _, gotErr, _ := runPS1Shell(t, "", dir, code, true)
	// 픽스처 유효성 먼저 — 대조군 stderr가 비면 비교할 오류가 아예 없어 아래 단정이
	// "둘 다 빈 값"으로 공허하게 통과한다(최종 리뷰 M1). TestD78RequiresStillApplies의
	// `base == 0` 검사와 같은 관용구다. 이 가드가 닫는 것은 **빈 stderr**뿐이다 —
	// 셸이 스크립트를 로드조차 못 하는 호스트(5.1 정책 차단 등)는 양쪽 다리가 같은
	// 비어 있지 않은 문면을 내므로 여기서 걸리지 않는다. 그 갈래의 권위는 CI windows
	// 잡이다(runPS1Shell 주석).
	if baseErr == "" {
		t.Fatal("대조군 stderr가 비었다 — 픽스처가 무효다(2행 오류를 내는 스니펫이어야 한다)")
	}
	if gotErr != baseErr {
		t.Fatalf("stderr가 달라졌다 — 등록 문을 앞 줄에 두면 여기서 줄 번호 +1 드리프트가 "+
			"잡힌다\n승격: %q\n대조: %q", gotErr, baseErr)
	}
}

// TestD78QuotedSidePathRuns — 설계 §2.7의 나머지 절반. 생성 바이트에서 작은따옴표가
// 이중화되는 것은 TestSnippetContent…가 단정한다. 여기서는 그 스크립트가 실제로 파싱되어
// 사이드파일이 기록되는지를 본다 — 따옴표 짝이 어긋나거나 `"; "`가 빠지는 회귀는 바이트
// 단정을 통과하지만 이 단정은 통과하지 못한다.
func TestD78QuotedSidePathRuns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "o'brien") // 작은따옴표는 Windows 파일명에 허용된다
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, _, side := runPS1Shell(t, "", dir, "cmd /c exit 62", true)
	if want := ctrNativeExitMarker + "62"; side != want {
		t.Fatalf("side=%q want %q — 경로의 작은따옴표가 이중화되지 않으면 스크립트가 "+
			"파싱되지 않는다", side, want)
	}
}

// TestD78FirstLineErrorKeepsLineNumber — 설계 §2.6. 1행 오류는 줄 번호만 단정하고
// 열 번호·소스 줄 에코 차이는 허용한다(§1.2의 알려진 한계).
func TestD78FirstLineErrorKeepsLineNumber(t *testing.T) {
	const code = "throw 'boom'"
	_, _, gotErr, _ := runPS1(t, code, true)
	if !strings.Contains(gotErr, "snippet.ps1:1") && !strings.Contains(gotErr, "snippet.ps1: 1") {
		t.Fatalf("1행 오류의 줄 번호가 1이 아니다: %q", gotErr)
	}
}

// TestD78RequiresStillApplies — 설계 §2.6. 승격돼도 #requires 지시자가 그대로 작동한다.
func TestD78RequiresStillApplies(t *testing.T) {
	const code = "#requires -Version 99\ncmd /c exit 61"
	base, _, _, _ := runPS1(t, code, false)
	got, _, _, _ := runPS1(t, code, true)
	if base == 0 {
		t.Fatalf("대조군이 이미 통과했다 — 픽스처가 무효다(요구 버전을 올려라)")
	}
	if got != base {
		t.Fatalf("승격 시 #requires가 무력화됐다: %d vs 대조군 %d", got, base)
	}
}

// TestD78NoObservableSideEffects — 설계 §2.4. -SupportEvent 회귀 감시선.
// 빠지면 Get-Job이 1이 되고 Wait-Job이 끝나지 않으며 작업 표가 stdout에 찍힌다.
func TestD78NoObservableSideEffects(t *testing.T) {
	// TestD78SignalOnExit과 같은 이유로 양쪽 셸을 돈다 — 작업표·구독표는 셸 버전에
	// 민감한 표면이고, 이 테스트가 -SupportEvent 회귀의 1차 감시선이다.
	for _, shell := range []string{"pwsh", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			t.Run("job-and-subscriber-tables-clean", func(t *testing.T) {
				const code = `Write-Output ("jobs=" + (Get-Job).Count + " subs=" + (Get-EventSubscriber).Count)`
				_, out, _, _ := runPS1Shell(t, shell, t.TempDir(), code, true)
				if got := strings.TrimSpace(out); got != "jobs=0 subs=0" {
					t.Fatalf("stdout=%q want \"jobs=0 subs=0\" — -SupportEvent가 빠지면 1/1이 되고 작업 표가 stdout에도 찍힌다", got)
				}
			})
			t.Run("wait-job-terminates", func(t *testing.T) {
				const code = "cmd /c exit 71\nGet-Job | Wait-Job | Out-Null"
				_, _, _, side := runPS1Shell(t, shell, t.TempDir(), code, true)
				if want := ctrNativeExitMarker + "71"; side != want {
					t.Fatalf("side=%q want %q — Wait-Job이 이벤트 작업을 기다리다 멈췄을 수 있다", side, want)
				}
			})
			t.Run("remove-job-keeps-sidefile", func(t *testing.T) {
				const code = "cmd /c exit 71\nGet-Job | Remove-Job -Force"
				_, _, _, side := runPS1Shell(t, shell, t.TempDir(), code, true)
				if want := ctrNativeExitMarker + "71"; side != want {
					t.Fatalf("side=%q want %q — 통상적인 정리 관용구가 신호를 지웠다", side, want)
				}
			})
		})
	}
}

// TestD78FallbackMatchesBaseline — 설계 §2.5. 폴백 대상은 대조군과 완전히 같아야 한다.
// 폴백이 만드는 스크립트는 D76 형태와 바이트 동일하므로 이 대조는 구성상 성립한다.
// 명명 블록은 토큰마다 따로 단정한다 — dynamicparam이 3라운드에서, clean이 5라운드에서
// 빠졌던 자리다(clean은 7.x에서만 명명 블록이라 5.1에서는 과잉 폴백으로 무해하다).
func TestD78FallbackMatchesBaseline(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"param", "param($x = \"dflt\")\nWrite-Output \"x=$x\"\ncmd /c exit 80"},
		{"block-comment-then-param", "<# doc #> param($x)\ncmd /c exit 74"},
		{"multiline-block-comment-then-param", "<#\n doc\n line2\n#>\nparam($x)\ncmd /c exit 74"},
		{"attribute-then-param", "[CmdletBinding()] param($x)\ncmd /c exit 74"},
		{"using", "using namespace System.Text\nWrite-Output 'ok'\ncmd /c exit 78"},
		{"backtick-then-using", "`\nusing namespace System.Text\nWrite-Output 'ok'\ncmd /c exit 78"},
		{"bom-then-param", "\uFEFFparam($x)\ncmd /c exit 74"},
		// 명명 블록은 **토큰마다 따로** 태운다 — begin/process/end를 한 스니펫에 합치면
		// begin 토큰만 판정을 거친다. dynamicparam이 3라운드에서, clean이 5라운드에서
		// 빠졌던 자리가 정확히 이 자리다.
		{"dynamicparam", "dynamicparam { }\nend { cmd /c exit 79 }"},
		{"begin", "begin { }\nend { cmd /c exit 79 }"},
		{"process", "process { }\nend { cmd /c exit 79 }"},
		{"end", "end { cmd /c exit 79 }"},
		{"clean", "clean { }\nend { cmd /c exit 79 }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if promotesNativeExitSignal(tc.code) {
				t.Fatalf("승격 대상으로 판정됐다 — 폴백해야 한다")
			}
			bc, bo, _, bs := runPS1(t, tc.code, false)
			gc, go_, _, gs := runPS1(t, tc.code, true)
			if gc != bc || go_ != bo || gs != bs {
				t.Fatalf("폴백이 대조군과 다르다\n승격: exit=%d out=%q side=%q\n대조: exit=%d out=%q side=%q",
					gc, go_, gs, bc, bo, bs)
			}
		})
	}
}

// TestD78Limits — 설계 §2.9. 경로마다 결과가 다르므로 따로 단정한다.
func TestD78Limits(t *testing.T) {
	// 아래 두 케이스는 종료 코드를 함께 단정한다 — side == ""는 "본문이 여기까지 갔지만
	// 신호가 남지 않았다"뿐 아니라 "스크립트가 파싱조차 안 됐다"·"인터프리터가 뜨지
	// 않았다"에서도 성립해, side만 보면 아무것도 돌지 않아도 통과한다(최종 리뷰 M2).
	t.Run("environment-exit-no-signal", func(t *testing.T) {
		exitCode, _, _, side := runPS1(t, "cmd /c exit 75\n[Environment]::Exit(5)", true)
		if side != "" {
			t.Fatalf("side=%q want 없음 — [Environment]::Exit은 이벤트를 발화시키지 않는다", side)
		}
		if exitCode != 5 {
			t.Fatalf("exit=%d want 5 — [Environment]::Exit(5)가 실행되지 않았다(본문 미실행)", exitCode)
		}
	})
	t.Run("fallback-throw-no-signal", func(t *testing.T) {
		// 폴백 경로는 D76 tail이라 처리되지 않은 종료 오류에서 tail이 실행되지 않는다.
		exitCode, _, _, side := runPS1(t, "param($x)\ncmd /c exit 73\nthrow 'x'", true)
		if side != "" {
			t.Fatalf("side=%q want 없음 — 폴백 경로의 알려진 공백이다", side)
		}
		if exitCode != 1 {
			t.Fatalf("exit=%d want 1 — throw가 실행되지 않았다(본문 미실행)", exitCode)
		}
	})
	t.Run("fallback-normal-records", func(t *testing.T) {
		_, _, _, side := runPS1(t, "param($x)\ncmd /c exit 74", true)
		if want := ctrNativeExitMarker + "74"; side != want {
			t.Fatalf("side=%q want %q — 폴백 경로도 정상 종료에서는 기록한다", side, want)
		}
	})
	// 아래 두 케이스의 첫 줄이 `$ctrProbe = Join-Path …`인 것은 필수다(계획 초안에서 정정).
	// 초안은 `[System.IO.File]::WriteAllText($PSScriptRoot + …)`로 시작했는데 선두 `[`는
	// promotesFirstToken이 최상단 속성 선언으로 보고 과잉 폴백시키는 문자다(기존 fallback
	// 케이스 type-cast-overfallback과 같은 자리). 그러면 등록 문이 아예 붙지 않아 두 단정이
	// 승격 경로를 밟지 못한다 — 실측 4형태 비교:
	//   폴백형 -Force / -Force 없음 → 둘 다 side=":8888"
	//   승격형 -Force → ":8888",  승격형 -Force 없음 → ":45"
	// 즉 초안 형태에서는 without-force가 통과 불가능하고, force-wins는 무엇을 지우든 통과하는
	// 거짓 초록이 된다. 그 재발을 막으려고 두 케이스 모두 승격 판정을 먼저 단정한다.
	t.Run("unregister-force-wins", func(t *testing.T) {
		code := "$ctrProbe = Join-Path $PSScriptRoot '" + ctrNativeExitFile + "'\n" +
			"[System.IO.File]::WriteAllText($ctrProbe, '" + ctrNativeExitMarker + "8888')\n" +
			"Unregister-Event -SourceIdentifier PowerShell.Exiting -Force -ErrorAction SilentlyContinue\n" +
			"cmd /c exit 45\nexit"
		if !promotesNativeExitSignal(code) {
			t.Fatal("픽스처가 폴백으로 판정됐다 — 등록 문이 없으면 이 단정은 아무것도 보지 못한다")
		}
		_, _, _, side := runPS1(t, code, true)
		if want := ctrNativeExitMarker + "8888"; side != want {
			t.Fatalf("side=%q want %q — -Force로는 조작이 통한다(알려진 한계)", side, want)
		}
	})
	t.Run("unregister-without-force-fails", func(t *testing.T) {
		// -SupportEvent 구독은 숨김이라 -Force 없는 Unregister-Event는 찾지 못하고 실패한다.
		// 이 대비가 깨지면 -SupportEvent가 사라진 것이다.
		code := "$ctrProbe = Join-Path $PSScriptRoot '" + ctrNativeExitFile + "'\n" +
			"[System.IO.File]::WriteAllText($ctrProbe, '" + ctrNativeExitMarker + "8888')\n" +
			"Unregister-Event -SourceIdentifier PowerShell.Exiting -ErrorAction SilentlyContinue\n" +
			"cmd /c exit 45\nexit"
		if !promotesNativeExitSignal(code) {
			t.Fatal("픽스처가 폴백으로 판정됐다 — 등록 문이 없으면 이 단정은 아무것도 보지 못한다")
		}
		_, _, _, side := runPS1(t, code, true)
		if want := ctrNativeExitMarker + "45"; side != want {
			t.Fatalf("side=%q want %q — -Force 없는 해제가 성공했다면 -SupportEvent가 빠진 것이다", side, want)
		}
	})
	// Task 2 리뷰가 지목한 구분. 승격 경로의 등록 문은 $LASTEXITCODE를 **직접** 읽는데,
	// nativeExitTail의 주석이 그 직접 참조가 strict mode 스코프 안에서는 미초기화 변수
	// 오류를 낸다고 문서화하고 있다. 기존 서브테스트 `strict_mode에_네이티브_명령_없음`은
	// stderr == ""만 단정하므로 "핸들러가 빈 값을 기록했다"와 "핸들러가 throw했고 내부
	// catch가 삼켰다"를 구분하지 못한다 — 둘 다 통과한다. 사이드파일 값이 그 둘을 가른다:
	// 기록에 성공하면 마커만 남은 빈 값이고, 핸들러가 throw했다면 파일 자체가 없다.
	// 설계 §3 표4가 이 조합의 관측값을 "빈 값"으로 기록하고 있다.
	t.Run("strict-mode-no-native-command", func(t *testing.T) {
		_, _, _, side := runPS1(t, "Set-StrictMode -Version Latest\nWrite-Output 'ok'", true)
		if want := ctrNativeExitMarker; side != want {
			t.Fatalf("side=%q want %q — 빈 문자열이면 핸들러가 throw하고 내부 catch가 삼킨 것이다", side, want)
		}
	})
	t.Run("lastexitcode-assign", func(t *testing.T) {
		// 설계 §3 표5: 스크립트 최상단 대입만으로 값이 바뀐다(global: 한정자 불필요).
		_, _, _, side := runPS1(t, "cmd /c exit 5\n$LASTEXITCODE = 7777\nexit", true)
		if want := ctrNativeExitMarker + "7777"; side != want {
			t.Fatalf("side=%q want %q", side, want)
		}
	})
	t.Run("own-handler-wins", func(t *testing.T) {
		// 설계 §3 표5: 같은 소스 식별자로 두 번 등록해도 충돌 오류 없이 공존하고
		// 스니펫 쪽 기록이 이긴다. 핸들러는 별도 스코프에서 돌므로 경로를 env로 넘긴다
		// (프로세스 전역이라 핸들러에서 보인다 — 러너의 등록 문도 같은 방식으로 읽는다).
		code := "$env:CTR_PROBE = Join-Path $PSScriptRoot '" + ctrNativeExitFile + "'\n" +
			"Register-EngineEvent -SourceIdentifier PowerShell.Exiting -SupportEvent -Action { " +
			"[System.IO.File]::WriteAllText($env:CTR_PROBE, '" + ctrNativeExitMarker + "9999') }\n" +
			"cmd /c exit 5\nexit"
		_, _, _, side := runPS1(t, code, true)
		if want := ctrNativeExitMarker + "9999"; side != want {
			t.Fatalf("side=%q want %q — 스니펫 자신의 핸들러가 이기는 것이 관측된 동작이다(위조 무력화를 주장하지 않는 근거)", side, want)
		}
	})
}
