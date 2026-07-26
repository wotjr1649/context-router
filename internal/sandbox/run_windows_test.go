//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestRunWindowsEchoAndExit(t *testing.T) {
	dir := t.TempDir()
	s := Spec{
		Argv:      []string{"cmd.exe", "/c", "echo hi& exit 3"},
		Dir:       dir,
		Env:       BaseEnv(),
		Timeout:   30 * time.Second,
		StdoutCap: 32768, StderrCap: 8192,
	}
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.ExitCode != 3 {
		t.Fatalf("exit=%d want 3", r.ExitCode)
	}
	if got := string(r.Stdout); got == "" || got[:2] != "hi" {
		t.Fatalf("stdout=%q", got)
	}
}

// TestRunWindowsTimeoutKillsTree: 타임아웃 시 트리 전체가 종료되는지(D59 잔존 0) 검증한다.
// 루트(powershell)가 손자(ping)를 잡 안에서 스폰(UseShellExecute=false → CreateProcess →
// 잡 상속)하고 그 PID를 파일에 남긴다. Run 반환 후 "지연 < 임계"만이 아니라 손자 PID가
// 실제로 소멸했는지까지 확인한다 — WaitDelay(5s)가 루트만 회수해도 통과하던 약한 검사를
// 막는다.
//
// Env는 닫힌 표에 PSModulePath를 명시로 덧붙인다(psModulePath — exec.go shellRunner가 주입하는
// 값과 같은 유도) — 값이 없으면 PowerShell이 유효 모듈 경로를 호스트 상태(HKLM/HKCU 환경값·셸
// 폴더)에서 스스로 재구성하므로, 명시 주입이 자식의 모듈 경로를 호스트와 무관하게 고정한다.
// bare BaseEnv()로 "단순화"하지 말 것 — 표에 되돌려 넣는 것도 금지다(그건 D65가 닫은 호스트
// 상속이다).
//
// 첫 항목이 스크래치의 빈 디렉터리인 것은 CI(windows-latest) 5.1 정지의 교정이고, 그 항목을 빼면
// 이 테스트는 아무 오류 출력 없이 무한 정지로 되돌아간다 — 실측·계약은 아래 "실패 경로 진단 계측"
// 주석과 docs/context-router-design-v0.12-ko.md D65, 유도는 internal/exec/exec.go psModulePath.
func TestRunWindowsTimeoutKillsTree(t *testing.T) {
	psExe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatalf("powershell.exe 미해석 — 트리 종료 검증 불가: %v", err)
	}
	dir := t.TempDir()
	bcPath := filepath.Join(dir, "root.bc")
	pidFile := filepath.Join(dir, "gc.pid")
	s := Spec{
		Argv: []string{
			psExe, "-NoProfile", "-NonInteractive", "-Command",
			treeKillScript(bcPath, pidFile, 60),
		},
		Dir:       dir,
		Env:       append(BaseEnv(), "PSModulePath="+psModulePath(psExe, scratchModuleDir(t, dir))),
		Timeout:   3 * time.Second,
		StdoutCap: 32768, StderrCap: 8192,
	}

	// 진단 계측: 실패한 경우에만 빵가루·env·러너 신원·판별 프로브를 남긴다 — 통과 경로의 출력과
	// 소요는 그대로다. Cleanup은 t.Fatalf로 끝난 실행에서도 돌고, t.TempDir의 삭제 Cleanup보다
	// 나중에 등록돼 LIFO로 그보다 먼저 돈다(스크래치가 아직 살아 있다).
	var r Result
	var start time.Time
	t.Cleanup(func() {
		if t.Failed() {
			dumpTreeKillDiag(t, s, dir, psExe, bcPath, r, start)
		}
	})

	start = time.Now()
	r, err = Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !r.TimedOut {
		t.Fatalf("TimedOut=false%s", rootOutput(r))
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("트리 미종료 — 지연 %v%s", time.Since(start), rootOutput(r))
	}

	gcPid := readPidFile(t, pidFile, r)
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(gcPid) {
		if time.Now().After(deadline) {
			t.Fatalf("손자 프로세스(pid=%d)가 Run 반환 후에도 생존 — 트리 미종료", gcPid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// psModulePath: exec.shellRunner의 extra가 주입하는 PSModulePath와 같은 유도 — 스크래치 빈 모듈
// 디렉터리 + <PSHOME>\Modules + 머신 전역 <ProgramFiles>\WindowsPowerShell\Modules, 이 순서. 이
// 테스트는 powershell.exe(5.1)만 띄우므로 5.1 갈래만 조립한다(pwsh 7이면 PowerShell\Modules).
//
// 몇 줄을 복제하는 이유: 이 파일은 인패키지 테스트(package sandbox)이고 internal/exec는
// internal/sandbox를 임포트하므로, 러너의 헬퍼를 가져오면 임포트 순환이 된다. 러너 쪽 유도를
// 바꾸면 여기도 같이 바꿀 것 — internal/exec/exec.go의 psModulePath.
func psModulePath(psExe, scratchMods string) string {
	out := []string{scratchMods, filepath.Join(filepath.Dir(psExe), "Modules")}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		out = append(out, filepath.Join(pf, "WindowsPowerShell", "Modules"))
	}
	return strings.Join(out, ";")
}

// scratchModuleDir: 러너가 스크래치 하위에 만드는 빈 모듈 디렉터리의 대역(exec.shellRunner의
// extra와 같은 배치·같은 이름). 실재하는 빈 디렉터리인 것이 주입 값의 전제이므로 생성까지 여기서
// 하고, 실패하면 실험 자체가 성립하지 않으므로 즉시 멈춘다(러너 쪽은 경고만 남기고 계속한다 —
// 그쪽 실패 경로는 probe-env psmod-scratch-noexist가 모형화한다).
func scratchModuleDir(t *testing.T, scratch string) string {
	t.Helper()
	d := filepath.Join(scratch, "psmodules")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatalf("스크래치 모듈 디렉터리 생성 실패: %v", err)
	}
	return d
}

func TestProbeWindows(t *testing.T) {
	if err := Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

// TestRunWindowsPreDoneContext: Start 전 ctx가 이미 종료된 경우 격리 준비 실패로 오분류하지
// 않는다 — 취소는 context 오류로 전파, deadline 만료는 타임아웃 Result로 변환.
func TestRunWindowsPreDoneContext(t *testing.T) {
	dir := t.TempDir()
	mk := func() Spec {
		return Spec{
			Argv:      []string{"cmd.exe", "/c", "echo hi"},
			Dir:       dir,
			Env:       BaseEnv(),
			Timeout:   30 * time.Second,
			StdoutCap: 32768, StderrCap: 8192,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 이미 취소
	if _, err := Run(ctx, mk()); !errors.Is(err, context.Canceled) || errors.Is(err, ErrSetup) {
		t.Fatalf("취소 전파 실패: err=%v (want context.Canceled, ErrSetup 아님)", err)
	}

	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second)) // 이미 만료
	defer dcancel()
	r, err := Run(dctx, mk())
	if err != nil {
		t.Fatalf("deadline 만료는 err 없이 TimedOut Result여야: err=%v", err)
	}
	if !r.TimedOut || r.ExitCode != -1 {
		t.Fatalf("deadline 만료가 TimedOut Result가 아님: %+v", r)
	}
}

// TestNewJobConfiguresCaps: 스펙 §2 캡 실증 — newJob이 주입값(및 0=기본값)을 잡에 실제로
// 설정하는지 QueryInformationJobObject로 직접 조회해 확인한다. 대량 할당을 하지 않으므로
// (프로젝트 메모리 규율) OOM 위험이 없고, 캡 누락이 우연히 통과할 여지도 없다.
func TestNewJobConfiguresCaps(t *testing.T) {
	assertJobLimits(t, Spec{MemLimitBytes: 128 << 20, ProcLimit: 8}, 128<<20, 8)
	assertJobLimits(t, Spec{}, defaultJobMemoryBytes, defaultProcLimit) // 0 → 기본값
}

func assertJobLimits(t *testing.T, s Spec, wantMem uint64, wantProc uint32) {
	t.Helper()
	h, err := newJob(s)
	if err != nil {
		t.Fatalf("newJob: %v", err)
	}
	defer windows.CloseHandle(h)

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var retlen uint32
	if err := windows.QueryInformationJobObject(
		h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &retlen,
	); err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}

	flags := info.BasicLimitInformation.LimitFlags
	for _, f := range []struct {
		name string
		bit  uint32
	}{
		{"KILL_ON_JOB_CLOSE", windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
		{"JOB_MEMORY", windows.JOB_OBJECT_LIMIT_JOB_MEMORY},
		{"ACTIVE_PROCESS", windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS},
	} {
		if flags&f.bit == 0 {
			t.Errorf("LimitFlags에 %s 미설정 (flags=%#x)", f.name, flags)
		}
	}
	if info.JobMemoryLimit != uintptr(wantMem) {
		t.Errorf("JobMemoryLimit=%d want %d", info.JobMemoryLimit, wantMem)
	}
	if info.BasicLimitInformation.ActiveProcessLimit != wantProc {
		t.Errorf("ActiveProcessLimit=%d want %d", info.BasicLimitInformation.ActiveProcessLimit, wantProc)
	}
}

// rootOutput: 실패 메시지에 붙일 루트 출력. Run이 상한 버퍼로 이미 회수해 둔 것을 버리지
// 않고 남긴다 — 루트가 왜 기록에 실패했는지가 CI 로그에서 바로 읽히게(진단 없는 실패는
// 원격에서만 재현되는 회귀를 추적 불가로 만든다).
func rootOutput(r Result) string {
	return fmt.Sprintf(" (루트 stdout=%q stderr=%q)", r.Stdout, r.Stderr)
}

// ── 실패 경로 진단 계측 ────────────────────────────────────────────────────────
// 2026-07-25, TestRunWindowsTimeoutKillsTree가 CI(windows-latest)에서만 결정적으로 멈췄고 개발기에서는
// 재현되지 않았다. 실패 서명은 "TimedOut=true, 두 스트림 모두 빈 값, 손자 PID 파일 부재" — 예외도
// 오류 레코드도 없어 스트림만으로는 아무것도 읽히지 않는다. 아래 계측이 그 침묵을 이름 있는 원인으로
// 바꿨고, 실패한 경우에만 돌기 때문에 통과 런의 출력·소요는 그대로다. 남긴 이유는 재발 대비다:
// 계측 없는 이 실패는 원격에서만 재현되는 무한 정지이고, 여기까지 오는 데 CI 왕복 여러 번이 들었다.
//
// 확정된 원인(CI 빵가루 실측. 전체 기록·계약은 docs/context-router-design-v0.12-ko.md D65,
// 주입 값의 유도는 internal/exec/exec.go psModulePath):
//   - 정지 지점은 프로바이더 cmdlet이다. 격리 실행이 01(진입)·02(파이프라인)·03([IO.File] 직접
//     쓰기)까지 남기고 .provider.cmdlet 동반 파일이 없다 = Set-Content가 돌아오지 않는다.
//     인터프리터는 정상 기동한다(170~194ms, FullLanguage, ConsoleHost).
//   - 원인은 PSModulePath의 **값**이고, 상속 값 8항목의 이분탐색이 정지를 푸는 항목을 하나로
//     좁혔다: 호스트 **사용자** 모듈 경로다. 그 슬롯이 채워져 있으면 exit 0·1.3s로 완주하고,
//     없으면 값이 더 좁아도 더 넓어도 정지한다. 교정은 그 슬롯을 스크래치의 빈 디렉터리로 채우는
//     것이다 — 호스트 값을 주입하는 것은 D65가 금지한다.
//
// 반증된 가설 — 되살리지 말 것: 3s 예산 부족(20s 프로브도 같은 지점), PATH(2항목 62B·합성 81항목
// 2380B·상속 81항목 3018B 세 변종 모두 동일하게 정지), 표가 떨어뜨린 표준 머신 변수 19개(얹어도
// 동일). 주입 값을 넓히는 것도 교정이 아니다 — 인터프리터가 머신 전역 항목을 스스로 앞에 덧붙이므로
// (실측) 자식의 유효 값은 이미 주입 값보다 넓다.
//
// 남긴 것은 빵가루와 그 무조건 덤프, 그리고 이 정지를 판별하는 프로브 둘(probe-env psmod-shipped·
// psmod-inherited)이다. 재발 시 읽는 법은 treeKillScript의 판별 3단계 주석과 probeEnvVariants 주석.

// treeKillScript: 손자를 스폰·기록하고 종료를 기다리는 루트 스크립트. 검증 경로
// (New-Object → Process::Start → Set-Content → WaitForExit)는 v0.11과 동일하게 두고 그 앞에
// 빵가루와 판별 3단계만 끼워 넣는다 — 실패 서명을 바꾸지 않기 위해서다. 규칙 둘:
//   - 빵가루는 [IO.File]::AppendAllText만 쓴다. cmdlet·모듈 해석에 의존하지 않는 순수 .NET
//     경로이고 호출마다 파일을 닫으므로, 명령 해석이 깨졌거나 잡이 트리를 끊어도 기록이 남는다
//     (두 스트림이 빈 값으로 돌아오는 실패라 스트림에 다시 의존할 수 없다).
//   - 큰따옴표를 쓰지 않는다 — Go EscapeArg → CreateProcess → powershell의 3중 인용을 피한다.
func treeKillScript(bcPath, pidPath string, pingCount int) string {
	return strings.Join([]string{
		"$t0=[DateTime]::UtcNow",
		"$b='" + bcPath + "'",
		"$nl=[Environment]::NewLine",
		"function bc($m){[IO.File]::AppendAllText($b,[DateTime]::UtcNow.ToString('HH:mm:ss.fff')+' '+$m+$nl)}",
		// startup-ms = CreateProcess(created)부터 첫 문장까지. 잡 배정·재개 대기와 인터프리터
		// 기동을 함께 담으므로 "기동까지가 늦었나 / 그 뒤가 멈췄나"가 이 한 값으로 갈린다.
		// 그래서 01이 첫 빵가루다 — 일찍 죽는 실행에서 가장 값진 한 줄이라 다른 무엇보다 앞에 온다.
		"$sp=[System.Diagnostics.Process]::GetCurrentProcess().StartTime.ToUniversalTime()",
		// 01에 자식이 실제로 본 PATH·PSModulePath의 크기를 함께 싣는다 — 정지하는 실행에서도
		// 남는 데 성공한 줄이라, env가 실제로 자식에 도달했는지가 이 한 줄로 확인된다. 값 전체가
		// 아니라 항목 수·크기만 남긴다(로그 폭발 방지). psmod-n은 인터프리터가 주입 값에 항목을
		// 덧붙였는지를 드러낸다 — 프로브의 판정을 "주입 값"이 아니라 "자식이 실제로 본 값"으로
		// 읽게 하는 단서다.
		"bc('01 enter pid=' + $PID + ' created=' + $sp.ToString('HH:mm:ss.fff') + ' startup-ms=' + [int](($t0 - $sp).TotalMilliseconds) + ' ps=' + $PSVersionTable.PSVersion + ' clr=' + $PSVersionTable.CLRVersion + ' lang=' + $ExecutionContext.SessionState.LanguageMode + ' host=' + $Host.Name + ' path-n=' + ($env:PATH -split ';').Count + ' path-len=' + $env:PATH.Length + ' psmod-n=' + ($env:PSModulePath -split ';').Count + ' psmod-len=' + $env:PSModulePath.Length)",
		// ── 판별 3단계 ────────────────────────────────────────────────────────────
		// 01 이후를 "계측"에서 "판별"로 바꾼다. 앞선 CI 런에서 01([IO.File]::AppendAllText)은
		// 남았고 그 다음이 예외·오류 레코드·stdout·stderr 전부 없이 멈췄다. 예외 없는 정지는
		// "OS 호출이 돌아오지 않았다"는 뜻이므로, 돌아오지 않는 호출을 세 갈래로 나눠 가둔다:
		// 순수 파이프라인 → .NET 직접 파일 쓰기 → 프로바이더 경유 파일 쓰기.
		//
		// 다음 CI 로그를 읽는 법(다시 유도하지 말 것):
		//   02 있음·03 없음 → 파이프라인은 되고 직접 파일 열기가 멈춘다. 파일시스템 필터 가설 강화.
		//   03 있음·04 없음 → 일반 스캔 적체 가설은 약하고 Set-Content 프로바이더 경로가 문제다.
		//   01만 있음       → 파일 쓰기 가설 반증. 정지는 첫 cmdlet·파이프라인 활성화 안에 있다.
		//   04까지 있고 12 없음 → 같은 프로바이더 호출이 한 번은 돌아왔으므로 결정적 차단이 아니다.
		//     그 경우 10~13 중 어디가 마지막인지가 다음 신호다.
		"1 | Out-Null",
		"bc('02 pipeline-returned')",
		"[IO.File]::WriteAllText($b + '.dotnet.cmdlet', 'x')",
		"bc('03 dotnet-write-returned')",
		"'x' | Set-Content -LiteralPath ($b + '.provider.cmdlet')",
		"bc('04 set-content-returned')",
		// Process::Start·Set-Content를 try로 감싼다 — 던진 예외가 빈 스트림이 아니라 파일에 남게.
		"try{$psi=New-Object System.Diagnostics.ProcessStartInfo",
		"$psi.FileName='ping'",
		"$psi.Arguments='-n " + strconv.Itoa(pingCount) + " 127.0.0.1'",
		"$psi.UseShellExecute=$false",
		"bc('10 pre-start')",
		"$p=[System.Diagnostics.Process]::Start($psi)",
		"bc('11 started gc=' + $p.Id)",
		"Set-Content -LiteralPath '" + pidPath + "' -Value $p.Id",
		"bc('12 set-content-returned exists=' + [IO.File]::Exists('" + pidPath + "'))",
		"$p.WaitForExit()",
		"bc('13 gc-exited')",
		"}catch{bc('90 threw ' + $_.Exception.GetType().FullName + ' :: ' + $_.Exception.Message)}",
		// 비종료 오류(Set-Content 실패 등)는 catch에 걸리지 않으므로 $Error로 따로 건진다.
		"bc('91 errors=' + $Error.Count + ' first=[' + $(if($Error.Count -gt 0){$Error[0].ToString()}else{''}) + ']')",
	}, ";")
}

// dumpTreeKillDiag: 실패한 경우에만 부르는 진단 덤프. 조건 없이 전부 남긴다 — 어느 가설이
// 맞는지 모르는 상태에서 "추측이 맞을 때만 찍는 로그"는 아무것도 가려내지 못한다.
func dumpTreeKillDiag(t *testing.T, s Spec, dir, psExe, bcPath string, r Result, start time.Time) {
	t.Helper()
	t.Logf("[diag] 러너 신원: ImageOS=%q ImageVersion=%q RUNNER_OS=%q GOARCH=%s NumCPU=%d os.TempDir=%q",
		os.Getenv("ImageOS"), os.Getenv("ImageVersion"), os.Getenv("RUNNER_OS"),
		runtime.GOARCH, runtime.NumCPU(), os.TempDir())
	// Run 진입 시각(UTC). 빵가루의 created와 나란히 놓으면 Start()까지의 비용이 나온다.
	t.Logf("[diag] Run 진입=%s (UTC)", start.UTC().Format("15:04:05.000"))
	if fi, err := os.Stat(psExe); err == nil {
		t.Logf("[diag] 인터프리터: %s size=%d mtime=%s", psExe, fi.Size(), fi.ModTime().UTC().Format(time.RFC3339))
	} else {
		t.Logf("[diag] 인터프리터 stat 실패: %s: %v", psExe, err)
	}
	t.Logf("[diag] Go가 자식에 넘긴 Spec.Env(%d개):", len(s.Env))
	for _, kv := range s.Env {
		t.Logf("[diag]   %s", kv)
	}
	t.Logf("[diag] Result: TimedOut=%v ExitCode=%d Duration=%v stdout(%dB,trunc=%v)=%q stderr(%dB,trunc=%v)=%q",
		r.TimedOut, r.ExitCode, r.Duration,
		len(r.Stdout), r.StdoutTrunc, r.Stdout, len(r.Stderr), r.StderrTrunc, r.Stderr)
	// 루트 빵가루 3종을 이름으로 못 박아 먼저 남긴다 — 열거(dumpScratch)만으로는 "없는 파일"이
	// 눈에 띄지 않고, 이 실험에서 부재가 곧 결론이다.
	logBreadcrumbs(t, bcPath)
	dumpScratch(t, dir)
	// 판별 프로브: 이 실패가 알려진 모듈 경로 정지인지를 같은 런에서 가른다(읽는 법은 그 함수
	// 주석). 손자 ping은 스스로 끝나 잔존 프로세스를 남기지 않는다.
	probeEnvVariants(t, s, dir)
}

// dumpScratch: 스크래치의 모든 항목과 내용. 없는 빵가루는 있는 빵가루만큼의 정보다.
func dumpScratch(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Logf("[diag] 스크래치 읽기 실패 %s: %v", dir, err)
		return
	}
	if len(ents) == 0 {
		t.Logf("[diag] 스크래치 비어 있음(%s) — 자식이 파일을 하나도 만들지 못했다", dir)
	}
	for _, e := range ents {
		logFileIfAny(t, filepath.Join(dir, e.Name()))
	}
}

func logFileIfAny(t *testing.T, path string) {
	t.Helper()
	// 스크래치에는 파일 아닌 항목도 있다(주입 값의 첫 항목인 psmodules 디렉터리). ReadFile로
	// 읽으면 "부재/읽기 실패"로 찍혀 부재가 결론인 이 실험의 판독을 흐린다 — 항목 수로 남긴다.
	// 그 수가 0인 것 자체가 D65 계약(우리 슬롯은 비어 있다)의 확인이다.
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		ents, rerr := os.ReadDir(path)
		t.Logf("[diag] --- %s\\ (디렉터리, 항목 %d개, err=%v) ---", filepath.Base(path), len(ents), rerr)
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Logf("[diag] %s 부재/읽기 실패: %v", filepath.Base(path), err)
		return
	}
	t.Logf("[diag] --- %s (%dB) ---\n%s", filepath.Base(path), len(b), b)
}

// probeArgv: 같은 argv에서 스크립트(마지막 인자)만 바꾼 사본.
func probeArgv(argv []string, script string) []string {
	out := append([]string(nil), argv...)
	out[len(out)-1] = script
	return out
}

// probeBudget: 프로브 1회 예산. 손자 ping 2회(≈1s)+인터프리터 기동(≈0.2s)이면 건강한 실행은
// ≈1.5s에 끝나므로 6s는 넉넉하고, 둘이 모두 멈춰도 12s로 끝난다 — 실패 경로의 계측 전체가 job의
// timeout-minutes: 20 안쪽에 크게 남는다.
const probeBudget = 6 * time.Second

// envWith: 닫힌 표 사본에 재지정을 덧붙인다. exec.Cmd는 중복 키에서 마지막 값을 쓰므로
// (os/exec Cmd.Env 계약, Windows는 키 대소문자 무시) 덧붙임이 표의 값을 대체한다. base를 직접
// append하면 백업 배열을 공유해 변종끼리 서로를 덮으므로 반드시 복제한다.
func envWith(base []string, kv ...string) []string {
	return append(append([]string(nil), base...), kv...)
}

// breadcrumbHas: 빵가루에 해당 단계 표식이 남았는지("HH:mm:ss.fff NN ..." 형식). 파일 부재·읽기
// 실패는 미도달로 본다 — 이 실험에서 부재가 곧 판정이다.
func breadcrumbHas(bc, step string) bool {
	b, err := os.ReadFile(bc)
	return err == nil && bytes.Contains(b, []byte(" "+step+" "))
}

// envProbe: 같은 격리 경로를 env만 바꿔 1회 돌리고, 프로바이더 쓰기가 돌아왔는지(빵가루 04)를
// 반환한다. 아래 두 변종의 판정은 이 한 값뿐이다 — 두 스트림이 빈 값으로 오는 실패라 출력에
// 다시 의존할 수 없다.
func envProbe(t *testing.T, s Spec, dir, name string, env []string) bool {
	t.Helper()
	bc := filepath.Join(dir, "probe-env-"+name+".bc")
	p := s
	p.Argv = probeArgv(s.Argv, treeKillScript(bc, filepath.Join(dir, "probe-env-"+name+".pid"), 2))
	p.Env = env
	p.Timeout = probeBudget
	start := time.Now()
	r, err := Run(context.Background(), p)
	ok := breadcrumbHas(bc, "04")
	t.Logf("[diag] probe-env %s(격리 있음·%v, env %d개): provider-write-returned=%v 소요=%v err=%v TimedOut=%v ExitCode=%d stdout=%q stderr=%q",
		name, probeBudget, len(env), ok, time.Since(start), err, r.TimedOut, r.ExitCode, r.Stdout, r.Stderr)
	logBreadcrumbs(t, bc)
	return ok
}

// probeEnvVariants: 이 실패가 알려진 모듈 경로 정지인지를 판별한다. 판정은 빵가루 04(Set-Content
// 반환) 하나이고, 두 변종은 무조건 다 돌린다 — 출하 값과 알려진 양성 통제(부모 상속 값)의 대비가
// 곧 판정이다. 읽는 법(다시 유도하지 말 것):
//
//	shipped 04 있음      → 이 정지는 재발하지 않았다. 실패 원인은 모듈 경로 밖이고, 빵가루의
//	                      마지막 단계가 다음 신호다(treeKillScript의 판별 3단계 주석).
//	shipped 정지 + 통제 04 → 정지 재발이고 출하 값이 요건을 잃었다. 사용자 모듈 슬롯을 스크래치
//	                      빈 디렉터리로 채우는 교정이 이 러너에서 더는 통하지 않는다는 뜻이므로,
//	                      값을 더 만들어 보는 것이 아니라 D65를 다시 결정해야 한다(호스트 값
//	                      주입은 D65가 닫았다 — exec.go psModulePath).
//	둘 다 정지           → 원인은 PSModulePath 밖이다. 부모 상속 값으로도 멈추므로 다음 후보는
//	                      닫힌 표의 나머지가 아니라 러너·인터프리터 쪽이다.
func probeEnvVariants(t *testing.T, s Spec, dir string) {
	t.Helper()

	// (1) 지금 러너가 주입하는 값 그대로(s.Env = 닫힌 표 + psModulePath) 자기 완결 실행. 본
	// 테스트는 타임아웃·킬 경로를 지나므로 실패가 "모듈 경로 정지"인지 "킬 경로"인지 갈리지
	// 않는다 — 이 프로브는 손자 ping 2회로 스스로 끝나므로 그 둘을 나눈다.
	shipped := psModulePath(s.Argv[0], scratchModuleDir(t, dir))
	t.Logf("[diag] 주입 PSModulePath: %d항목 %dB", len(strings.Split(shipped, ";")), len(shipped))
	envProbe(t, s, dir, "psmod-shipped", s.Env)

	// (2) 알려진 양성 통제 — 부모의 상속 값. 정지를 푸는 항목(호스트 사용자 모듈 경로)이 그 안에
	// 있으므로 이 값은 완주한다. D65가 주입을 금지하는 값이라 통제로만 쓴다 — 본 테스트나 러너를
	// 이 값으로 "고치지" 말 것.
	inherited := os.Getenv("PSModulePath")
	t.Logf("[diag] 상속 PSModulePath: %d항목 %dB", len(strings.Split(inherited, ";")), len(inherited))
	envProbe(t, s, dir, "psmod-inherited", envWith(s.Env, "PSModulePath="+inherited))
}

// logBreadcrumbs: 빵가루 파일과 판별 3단계가 만드는 동반 파일 둘을 조건 없이 남긴다. 없는
// 파일이 이 실험의 신호이므로 부재도 한 줄로 찍혀야 한다(logFileIfAny가 부재를 남긴다).
func logBreadcrumbs(t *testing.T, bc string) {
	t.Helper()
	for _, p := range []string{bc, bc + ".dotnet.cmdlet", bc + ".provider.cmdlet"} {
		logFileIfAny(t, p)
	}
}

// readPidFile: 루트가 손자를 스폰·기록할 때까지 잠깐 폴링한 뒤 PID를 읽는다.
func readPidFile(t *testing.T, path string, r Result) int {
	t.Helper()
	var b []byte
	deadline := time.Now().Add(3 * time.Second)
	for {
		var err error
		b, err = os.ReadFile(path)
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("손자 PID 파일 미기록: %v%s", err, rootOutput(r))
		}
		time.Sleep(50 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("PID 파싱: %q: %v", b, err)
	}
	return pid
}

// processAlive: pid가 아직 실행 중인지(STILL_ACTIVE). 열 수 없으면 소멸로 본다.
func processAlive(pid int) bool {
	const stillActive = 259 // STILL_ACTIVE
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
