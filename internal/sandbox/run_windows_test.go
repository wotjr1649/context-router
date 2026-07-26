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
	"slices"
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
// 이 주입은 아래 CI 정지의 교정이 아니다. 정지한 실행의 유효 값은 이미 머신 전역 + <PSHOME>
// 2항목이었다(인터프리터가 스스로 덧붙인다 — 빵가루 psmod-len=93). 명시 주입은 그 값을 고정할
// 뿐이라 정지 여부를 바꾸지 않는다. 원인 미확정 상태이므로 임시 진단 계측(treeKillScript 빵가루·
// dumpTreeKillDiag·probe-env psmod-shipped·bisectModulePath)을 유지한다 — 자세한 실측표는 아래
// "임시 진단 계측" 주석. 원인이 확정되면 계측을 제거한다.
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
		Env:       append(BaseEnv(), "PSModulePath="+psModulePath(psExe)),
		Timeout:   3 * time.Second,
		StdoutCap: 32768, StderrCap: 8192,
	}

	// 진단 계측(임시): 실패한 경우에만 빵가루·env·러너 신원·통제 실행을 남긴다 — 통과 경로의
	// 출력과 소요는 그대로다. Cleanup은 t.Fatalf로 끝난 실행에서도 돌고, t.TempDir의 삭제
	// Cleanup보다 나중에 등록돼 LIFO로 그보다 먼저 돈다(스크래치가 아직 살아 있다).
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

// psModulePath: exec.shellRunner의 extra가 주입하는 PSModulePath와 같은 유도 — 머신 전역
// <ProgramFiles>\WindowsPowerShell\Modules + <PSHOME>\Modules, 이 순서. 이 테스트는
// powershell.exe(5.1)만 띄우므로 5.1 갈래만 조립한다(pwsh 7이면 PowerShell\Modules).
//
// 세 줄을 복제하는 이유: 이 파일은 인패키지 테스트(package sandbox)이고 internal/exec는
// internal/sandbox를 임포트하므로, 러너의 헬퍼를 가져오면 임포트 순환이 된다. 러너 쪽 유도를
// 바꾸면 여기도 같이 바꿀 것 — internal/exec/exec.go의 psModulePath.
func psModulePath(psExe string) string {
	home := filepath.Join(filepath.Dir(psExe), "Modules")
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		return home
	}
	return filepath.Join(pf, "WindowsPowerShell", "Modules") + ";" + home
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

// ── 임시 진단 계측 ─────────────────────────────────────────────────────────────
// TestRunWindowsTimeoutKillsTree가 CI(windows-latest)에서만 결정적으로 실패하고 개발기에서는
// 재현되지 않는다. 실패 서명은 "TimedOut=true, 두 스트림 모두 빈 값, 손자 PID 파일 부재"다.
//
// 반증된 가설 — 되살리지 말 것:
//  1. 3s 예산 부족: 20s 프로브도 같은 지점에서 멈춘다.
//  2. "probe-noiso가 env 격리 없이 돈다"는 근거로 닫힌 표를 배제한 결론: 그 판본은 cmd.Env를
//     s.Env로 고정하고 있었으므로 env를 갈라낸 적이 없다.
//  3. PATH: 2항목(62B)·합성 81항목(2380B)·상속 81항목(3018B) 세 변종이 모두 동일하게 멈췄다.
//  4. 표가 떨어뜨린 표준 머신 변수 19개(windir·PROCESSOR_*·ALLUSERSPROFILE·PUBLIC·USERNAME 등):
//     얹어도 같은 지점에서 멈춘다(plus-omitted).
//
// 확정된 사실(앞선 CI 런들의 빵가루):
//   - 정지 지점은 프로바이더 cmdlet이다. 격리 실행이 01(진입)·02(파이프라인)·03([IO.File] 직접
//     쓰기)까지 남기고 .provider.cmdlet 동반 파일이 없다 = Set-Content가 돌아오지 않는다.
//     인터프리터는 정상 기동한다(170~194ms, FullLanguage, ConsoleHost). 예외도 오류 레코드도 없다.
//   - 원인은 PSModulePath의 **값**이다. 같은 런의 네 변종 중 상속 값(8항목 359B)만 04를 남기고
//     exit 0으로 완주했고(1.33s), 단일 항목(유효 93B)·값 없음(유효 292B)·표준 머신 변수 추가는
//     모두 정지했다. probe-noiso(부모 환경 전체 상속)도 완주한다.
//
// 반증 5 — "단일 항목이라 부족했다"는 읽기. 자식의 유효 값은 주입 값이 아니다: 인터프리터가
// 머신 전역 모듈 경로를 스스로 앞에 덧붙인다(로컬 실측 — 5.1: 50B 주입 → 유효 93B =
// <ProgramFiles>\WindowsPowerShell\Modules;<PSHOME>\Modules / pwsh 7: 37B → 73B, 같은 모양).
// 빵가루의 psmod-len=93이 정확히 그 둘이므로, 정지한 실행은 이미 2항목을 보고 있었다. 따라서
// 주입 값을 그 2항목으로 넓히는 것은 유효 값을 바꾸지 않는다 — psModulePath의 명시는 값을
// 고정하는 것이고 이 정지의 교정이 아니다.
//
// 남은 질문은 "상속 값(359B)의 무엇이 정지를 푸는가"다. 값 없음(292B)이 2항목의 상위집합인데도
// 정지했으므로 답은 항목 수·머신 전역이 아니라 상속 값에만 있는 항목 쪽에 있다. probeEnvVariants가
// psmod-shipped(지금 주입하는 값)로 정지를 다시 못 박고, 상속 값이 풀고 주입 값이 못 풀면
// bisectModulePath가 상속 값을 항목 단위로 갈라 최소 집합을 남긴다 — 그 항목이 무엇인지가
// 다음 결정의 입력이다(호스트 값을 주입하는 교정은 D65가 금지하므로, 답에 따라 러너 쪽이 아닌
// 테스트 쪽 교정이 될 수도 있다: 빵가루 03이 [IO.File]::WriteAllText는 모든 변종에서 돌아옴을 보인다).
// 원인이 확정되면 판별 3단계·빵가루 문장과 dumpTreeKillDiag/probe*/synthPath/logBreadcrumbs/
// logDroppedEnv/bisect*를 함께 제거하고 스크립트를 원래 5문장으로 되돌린다.

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
		// 기동을 함께 담으므로, probe-noiso(격리 없음)·probe-long(격리 있음)의 같은 값과 나란히
		// 놓으면 "3s 예산이 짧았나 / 격리 경로가 늦췄나"가 갈린다. 그래서 01이 첫 빵가루다 —
		// 일찍 죽는 실행에서 가장 값진 한 줄이라 다른 무엇보다 앞에 온다.
		"$sp=[System.Diagnostics.Process]::GetCurrentProcess().StartTime.ToUniversalTime()",
		// 01에 자식이 실제로 본 PATH·PSModulePath의 크기를 함께 싣는다 — 지금까지 유일하게
		// 남는 데 성공한 줄이고, 아래 PATH 변종(probePathVariants)이 실제로 자식에 도달했는지를
		// 이 한 줄로 확인할 수 있다. 값 전체가 아니라 크기만 남긴다(로그 폭발 방지).
		"bc('01 enter pid=' + $PID + ' created=' + $sp.ToString('HH:mm:ss.fff') + ' startup-ms=' + [int](($t0 - $sp).TotalMilliseconds) + ' ps=' + $PSVersionTable.PSVersion + ' clr=' + $PSVersionTable.CLRVersion + ' lang=' + $ExecutionContext.SessionState.LanguageMode + ' host=' + $Host.Name + ' path-n=' + ($env:PATH -split ';').Count + ' path-len=' + $env:PATH.Length + ' psmod-len=' + $env:PSModulePath.Length)",
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
	logDroppedEnv(t)
	t.Logf("[diag] Result: TimedOut=%v ExitCode=%d Duration=%v stdout(%dB,trunc=%v)=%q stderr(%dB,trunc=%v)=%q",
		r.TimedOut, r.ExitCode, r.Duration,
		len(r.Stdout), r.StdoutTrunc, r.Stdout, len(r.Stderr), r.StderrTrunc, r.Stderr)
	// 루트 빵가루 3종을 이름으로 못 박아 먼저 남긴다 — 열거(dumpScratch)만으로는 "없는 파일"이
	// 눈에 띄지 않고, 이 실험에서 부재가 곧 결론이다.
	logBreadcrumbs(t, bcPath)
	dumpScratch(t, dir)
	// 통제 실행: 빵가루가 아예 없을 때 "격리 경로가 자식을 못 돌렸다 / 3s 예산이 이 러너에서
	// 부족했다 / 인터프리터가 이 환경에서 아무것도 못 한다"를 갈라내고(2종), 닫힌 표의 PATH가
	// 원인인지를 3변종으로 다시 나눈다. 손자 ping은 스스로 끝나 잔존 프로세스를 남기지 않는다.
	probeNoIsolation(t, s, dir)
	probeLongBudget(t, s, dir)
	probePathVariants(t, s, psExe, dir)
	probeEnvVariants(t, s, dir)
}

// logDroppedEnv: 부모에는 있는데 닫힌 표가 떨어뜨리는 변수의 이름과 값 길이. 값은 남기지
// 않는다 — 러너 환경에는 토큰류가 섞여 있어 값을 CI 로그에 찍는 것 자체가 유출이다. 아래
// plus-omitted 묶음이 러너에서 실제로 무엇을 놓치는지는 이 목록과 나란히 놓아야 읽힌다.
func logDroppedEnv(t *testing.T) {
	t.Helper()
	kept := make(map[string]bool, len(baseKeys()))
	for _, k := range baseKeys() {
		kept[strings.ToLower(k)] = true
	}
	var names []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		// Windows는 =C:=... 형태의 빈 이름 항목을 실을 수 있다 — 표 밖이지만 신호가 아니다.
		if k == "" || kept[strings.ToLower(k)] {
			continue
		}
		names = append(names, fmt.Sprintf("%s(%dB)", k, len(v)))
	}
	slices.Sort(names)
	t.Logf("[diag] 닫힌 표가 떨어뜨리는 부모 변수 %d개(이름·길이만): %s", len(names), strings.Join(names, " "))
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

// probeNoIsolation: 잡·CREATE_SUSPENDED 없이, 그리고 닫힌 env 표 없이(부모 환경 상속) 같은
// argv·cwd로 같은 스크립트를 돌린다 — 이름이 주장하는 "격리 없음"이 실제로 성립하는 유일한
// 실행이다. cmd.Env=s.Env로 표를 고정했던 앞선 판본은 env 격리를 하나도 갈라내지 못했고, 그
// 판본을 근거로 닫힌 표를 후보에서 뺀 결론은 틀렸다. cmd.Env를 비워 두면 exec.Cmd가 부모
// 환경 전체를 물려준다(BaseEnv 주석의 "nil Env = 전체 상속"과 같은 성질을 여기서는 의도로 쓴다).
// 빵가루가 여기서 남고 격리 실행에서 안 남으면 원인은 잡·재개 경로 또는 닫힌 표 중 하나이고,
// PATH 변종(probe-path-*)이 그 둘을 다시 나눈다. 여기서도 안 남으면 인터프리터·러너 쪽이며,
// 이 실행은 킬 경로가 아니라 스트림으로 예외를 볼 수 있다.
func probeNoIsolation(t *testing.T, s Spec, dir string) {
	t.Helper()
	bc := filepath.Join(dir, "probe-noiso.bc")
	argv := probeArgv(s.Argv, treeKillScript(bc, filepath.Join(dir, "probe-noiso.pid"), 5))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir // cmd.Env 미설정 = 부모 환경 상속. 여기에 s.Env를 되돌려 넣지 말 것.
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	t.Logf("[diag] probe-noiso(격리 없음·env 상속·20s): 소요=%v err=%v stdout=%q stderr=%q",
		time.Since(start), err, out.String(), errb.String())
	logBreadcrumbs(t, bc)
}

// probeLongBudget: 같은 격리 경로(잡·CREATE_SUSPENDED·재개)를 예산만 20s로 늘려 돌린다.
// 손자가 기록되면 3s 예산이 이 러너에서 부족했다는 뜻이고, 여기서도 빵가루가 없으면 격리
// 경로 자체가 자식을 돌리지 못한 것이다. ping 5회로 스스로 끝나므로 이 실행은 타임아웃이
// 아닌 정상 종료 경로를 지나며, 그 stdout에 표식이 있는데 3s 실행에는 없다면 출력 손실이
// 킬 경로 고유임을 뜻한다.
func probeLongBudget(t *testing.T, s Spec, dir string) {
	t.Helper()
	bc := filepath.Join(dir, "probe-long.bc")
	p := s
	p.Argv = probeArgv(s.Argv, treeKillScript(bc, filepath.Join(dir, "probe-long.pid"), 5))
	p.Timeout = 20 * time.Second
	start := time.Now()
	r, err := Run(context.Background(), p)
	t.Logf("[diag] probe-long(격리 있음·20s): 소요=%v err=%v TimedOut=%v ExitCode=%d stdout=%q stderr=%q",
		time.Since(start), err, r.TimedOut, r.ExitCode, r.Stdout, r.Stderr)
	logBreadcrumbs(t, bc)
}

// pathProbeBudget: PATH 변종 1회 예산. 손자 ping 2회(≈1s)+인터프리터 기동(≈0.2s)이면 건강한
// 실행은 ≈1.5s에 끝나므로 6s는 넉넉하고, 셋이 모두 멈춰도 18s로 끝난다 — 계측 전체(실패 시)를
// 러너에서 ≈120s(항목 이분탐색 3라운드까지 포함한 최악)에 묶어 job의 timeout-minutes: 20 안쪽에
// 크게 남긴다.
const pathProbeBudget = 6 * time.Second

// probePathVariants: 같은 격리 실행을 PATH만 바꿔 3번 돌린다. 닫힌 env 표가 다시 후보로 돌아왔고
// (probe-noiso의 env 고정이 잘못된 배제를 낳았다) PATH는 표에서 호스트 상태를 가장 크게 실어
// 나르는 항목이다. 읽는 법:
//   - (c)만 멈춘다      → 상속 PATH의 특정 항목이 원인
//   - (b)도 함께 멈춘다 → 특정 항목이 아니라 길이·항목 수가 원인
//   - 셋 다 멈춘다      → PATH 가설 반증(원인은 PATH 밖)
//
// 세 변종의 항목 수·바이트 수를 로그에 함께 남기므로 "비슷한 길이였나"는 로그에서 직접 읽힌다.
func probePathVariants(t *testing.T, s Spec, psExe, dir string) {
	t.Helper()
	inherited := os.Getenv("PATH")
	sys32 := filepath.Join(os.Getenv("SystemRoot"), "System32")
	for _, v := range []struct{ name, path string }{
		{"a-minimal", sys32 + ";" + filepath.Dir(psExe)},
		{"b-synthetic", synthPath(t, inherited)},
		{"c-inherited", inherited},
	} {
		bc := filepath.Join(dir, "probe-path-"+v.name+".bc")
		p := s
		p.Argv = probeArgv(s.Argv, treeKillScript(bc, filepath.Join(dir, "probe-path-"+v.name+".pid"), 2))
		p.Env = envWith(s.Env, "PATH="+v.path)
		p.Timeout = pathProbeBudget
		start := time.Now()
		r, err := Run(context.Background(), p)
		t.Logf("[diag] probe-path %s(격리 있음·%v, %d항목 %dB): 소요=%v err=%v TimedOut=%v ExitCode=%d stdout=%q stderr=%q",
			v.name, pathProbeBudget, len(strings.Split(v.path, ";")), len(v.path),
			time.Since(start), err, r.TimedOut, r.ExitCode, r.Stdout, r.Stderr)
		logBreadcrumbs(t, bc)
	}
}

// synthPath: 상속 PATH와 항목 수가 같고, 확실히 존재하는 로컬 디렉터리만으로 이뤄진 대조 PATH.
// %SystemRoot%\System32(모자라면 %SystemRoot%)의 실제 하위 디렉터리를 열거해 쓴다 — 열거했으므로
// 존재가 확실하고, 시스템 드라이브라 네트워크·마운트 경유가 아니며, 항목이 서로 달라 "같은 경로
// 반복"으로 항목 수 대조가 무의미해지는 일도 없다. 항목 수는 정확히 맞추고 길이는 근사한다
// (개발기 테스트 프로세스 실측: 상속 66항목 2700B ↔ 대조 66항목 1944B). 대조가 상속보다 짧게
// 나오는 쪽이라, (b)가 멈추면 "길이·항목 수" 해석은 더 짧은 PATH로도 재현됐다는 뜻이 된다.
func synthPath(t *testing.T, inherited string) string {
	t.Helper()
	want := len(strings.Split(inherited, ";"))
	root := os.Getenv("SystemRoot")
	out := make([]string, 0, want)
	for _, base := range []string{filepath.Join(root, "System32"), root} {
		ents, err := os.ReadDir(base)
		if err != nil {
			t.Logf("[diag] synth PATH 열거 실패 %s: %v", base, err)
			continue
		}
		for _, e := range ents {
			if len(out) == want {
				return strings.Join(out, ";")
			}
			if e.IsDir() {
				out = append(out, filepath.Join(base, e.Name()))
			}
		}
	}
	// 하위 디렉터리가 상속 항목 수보다 적으면 있는 만큼으로 간다 — 로그의 항목 수가 그 사실을
	// 그대로 드러내므로 조용히 왜곡되지 않는다.
	return strings.Join(out, ";")
}

// envWith: 닫힌 표 사본에 재지정을 덧붙인다. exec.Cmd는 중복 키에서 마지막 값을 쓰므로
// (os/exec Cmd.Env 계약, Windows는 키 대소문자 무시) 덧붙임이 표의 값을 대체한다. base를 직접
// append하면 백업 배열을 공유해 변종끼리 서로를 덮으므로 반드시 복제한다.
func envWith(base []string, kv ...string) []string {
	return append(append([]string(nil), base...), kv...)
}

// omittedWindowsKeys: 닫힌 표가 떨어뜨리는 것 중 Windows 구성요소가 통상 요구하는 표준 머신
// 변수. 러너·툴체인·사용자 앱 변수는 넣지 않는다 — 거기까지 넣으면 probe-noiso(부모 환경 전체
// 상속)와 같아져 아무것도 가려내지 못하고, CI 토큰류 값이 자식과 로그로 흘러간다. 순서는
// 이분탐색의 첫 반쪽이 "시스템 경로·아키텍처", 둘째 반쪽이 "계정·세션 신원"으로 갈리게 둔다.
func omittedWindowsKeys() []string {
	return []string{
		"windir", "OS", "DriverData", "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS",
		"CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432",
		"ProgramFiles(x86)", "ProgramW6432",
		"ALLUSERSPROFILE", "PUBLIC", "USERNAME", "USERDOMAIN", "COMPUTERNAME",
		"SESSIONNAME", "LOGONSERVER",
		"PROCESSOR_IDENTIFIER", "PROCESSOR_LEVEL", "PROCESSOR_REVISION",
	}
}

// envOf: 주어진 키 중 부모에 값이 있는 것만 KEY=VALUE로. 부재 키는 조용히 빠지지 않고 로그의
// 개수·이름으로 드러난다.
func envOf(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func envKeys(kvs []string) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i], _, _ = strings.Cut(kv, "=")
	}
	return out
}

// breadcrumbHas: 빵가루에 해당 단계 표식이 남았는지("HH:mm:ss.fff NN ..." 형식). 파일 부재·읽기
// 실패는 미도달로 본다 — 이 실험에서 부재가 곧 판정이다.
func breadcrumbHas(bc, step string) bool {
	b, err := os.ReadFile(bc)
	return err == nil && bytes.Contains(b, []byte(" "+step+" "))
}

// envProbe: 같은 격리 경로를 env만 바꿔 1회 돌리고, 프로바이더 쓰기가 돌아왔는지(빵가루 04)를
// 반환한다. 아래 변종과 이분탐색의 판정은 이 한 값뿐이다 — 두 스트림이 빈 값으로 오는 실패라
// 출력에 다시 의존할 수 없다.
func envProbe(t *testing.T, s Spec, dir, name string, env []string) bool {
	t.Helper()
	bc := filepath.Join(dir, "probe-env-"+name+".bc")
	p := s
	p.Argv = probeArgv(s.Argv, treeKillScript(bc, filepath.Join(dir, "probe-env-"+name+".pid"), 2))
	p.Env = env
	p.Timeout = pathProbeBudget
	start := time.Now()
	r, err := Run(context.Background(), p)
	ok := breadcrumbHas(bc, "04")
	t.Logf("[diag] probe-env %s(격리 있음·%v, env %d개): provider-write-returned=%v 소요=%v err=%v TimedOut=%v ExitCode=%d stdout=%q stderr=%q",
		name, pathProbeBudget, len(env), ok, time.Since(start), err, r.TimedOut, r.ExitCode, r.Stdout, r.Stderr)
	logBreadcrumbs(t, bc)
	return ok
}

// probeEnvVariants: 남은 단 하나의 후보 — "닫힌 표가 어느 변수를 넘기는가" — 를 네 갈래로
// 가른다. 판정은 빵가루 04(Set-Content 반환) 하나이고, 넷 다 무조건 돌린다(하나가 풀려도 나머지
// 답은 독립된 정보다). 읽는 법:
//
//	psmod-shipped   04 없음 → 예상대로다(유효 값이 정지 실행과 같은 2항목이므로). 정지가 킬 경로가
//	                        아니라 모듈 경로 쪽임을 자기완결 실행으로 다시 못 박는다.
//	psmod-shipped   04 있음 → 놀라운 결과. 유효 항목 집합이 같으므로 남는 차이는 **항목 순서**뿐이다
//	                        (명시 주입은 머신 전역·PSHOME 순서를 고정한다). 그때는 순서가 신호다.
//	psmod-inherited 만 04 → PSModulePath 값이 원인. 이어서 bisectModulePath가 항목까지 좁힌다.
//	psmod-absent    도 04 → 명시 주입이 문제이고 PowerShell의 재구성값은 충분하다.
//	plus-omitted    만 04 → 원인은 PSModulePath 밖. 이어지는 이분탐색이 어느 변수인지 좁힌다.
//	넷 다 정지            → 원인은 이 축들 밖. 닫힌 표가 떨어뜨리는 나머지(logDroppedEnv 목록)
//	                        중 표준 머신 변수가 아닌 항목이 다음 후보다.
func probeEnvVariants(t *testing.T, s Spec, dir string) {
	t.Helper()

	// (0) 지금 러너가 주입하는 값 그대로(s.Env = 닫힌 표 + psModulePath) 자기 완결 실행. 본
	// 테스트는 타임아웃·킬 경로를 지나므로 실패가 "모듈 경로 정지"인지 "킬 경로"인지 갈리지
	// 않는다 — 이 프로브는 손자 ping 2회로 스스로 끝나므로 그 둘을 나눈다.
	shipped := psModulePath(s.Argv[0]) // s.Argv[0] = 이 테스트가 해석한 powershell.exe
	t.Logf("[diag] 주입 PSModulePath: %d항목 %dB", len(strings.Split(shipped, ";")), len(shipped))
	shippedOK := envProbe(t, s, dir, "psmod-shipped", s.Env)

	// (1) 주입 값 대신 부모의 상속 값. 이 브랜치 전의 자식이 보던 값이 바로 이것이다.
	inherited := os.Getenv("PSModulePath")
	t.Logf("[diag] 상속 PSModulePath: %d항목 %dB", len(strings.Split(inherited, ";")), len(inherited))
	if envProbe(t, s, dir, "psmod-inherited", envWith(s.Env, "PSModulePath="+inherited)) && !shippedOK {
		entries := slices.DeleteFunc(strings.Split(inherited, ";"), func(e string) bool { return e == "" })
		bisectModulePath(t, s, dir, entries)
	}

	// (2) PSModulePath를 아예 넘기지 않는다 — PowerShell이 스스로 재구성한다. 표에서 뺀 직후
	// (주입 전) 판본이 사실상 이 모양이었고 동일 정지였으므로, 같은 런 안에서 빵가루로 다시
	// 확인하는 대조다. bare BaseEnv()는 여기서만 의도다 — 본 테스트를 이렇게 바꾸지 말 것.
	envProbe(t, s, dir, "psmod-absent", BaseEnv())

	// (3) 표가 떨어뜨린 표준 머신 변수 묶음을 한 번에 얹는다.
	omitted := envOf(omittedWindowsKeys())
	t.Logf("[diag] plus-omitted 묶음 %d/%d개: %v", len(omitted), len(omittedWindowsKeys()), envKeys(omitted))
	if envProbe(t, s, dir, "plus-omitted", envWith(s.Env, omitted...)) {
		bisectEnvGroup(t, s, dir, omitted)
	}
}

// bisectModulePath: 상속 PSModulePath가 정지를 풀고 주입 값이 못 풀 때만 부른다 — "상속 값의 어느
// 항목이 필요한가"를 항목 단위 이분탐색으로 좁힌다. 판정은 빵가루 04 하나이고, 반쪽 둘이 모두
// 멈추면 단일 항목 가정이 깨진 것이므로 단독 결론을 내지 않고 그 사실만 남긴다.
//
// 남는 항목이 무엇인지가 다음 결정의 입력이다: 사용자 경로면 D65 계약과 정면으로 부딪히고,
// 러너 이미지 고유 경로(C:\Modules\... 류)면 러너 쪽 교정이 아니라 테스트 쪽 교정이 답이다.
// 어느 쪽이든 호스트 값을 그대로 주입하는 교정은 D65가 닫았으므로 이 결과를 사람이 읽고 정한다.
func bisectModulePath(t *testing.T, s Spec, dir string, entries []string) {
	t.Helper()
	for round := 1; len(entries) > 1; round++ {
		lo, hi := entries[:len(entries)/2], entries[len(entries)/2:]
		name := fmt.Sprintf("psmod-bisect%d", round)
		if envProbe(t, s, dir, name+"-lo", envWith(s.Env, "PSModulePath="+strings.Join(lo, ";"))) {
			entries = lo
			continue
		}
		if envProbe(t, s, dir, name+"-hi", envWith(s.Env, "PSModulePath="+strings.Join(hi, ";"))) {
			entries = hi
			continue
		}
		t.Logf("[diag] PSModulePath 이분탐색 중단(라운드 %d): 반쪽 둘 다 정지 — 단일 항목이 아니다. 최소 집합은 %v의 조합",
			round, entries)
		return
	}
	t.Logf("[diag] PSModulePath 이분탐색 결과: 단독으로 정지를 푸는 항목 = %v", entries)
}

// bisectEnvGroup: 묶음이 정지를 풀었을 때 최소 원인을 이분탐색으로 좁힌다(단일 원인 가정).
// 반쪽 둘이 모두 멈추면 그 가정이 깨진 것이므로 단독 결론을 내지 않고 그 사실을 남기고 멈춘다 —
// 틀린 단일 결론이 다음 CI 왕복을 낭비하는 것보다 낫다. 라운드당 최대 2회(≈7.5s)·최대 5라운드.
func bisectEnvGroup(t *testing.T, s Spec, dir string, kvs []string) {
	t.Helper()
	for round := 1; len(kvs) > 1; round++ {
		lo, hi := kvs[:len(kvs)/2], kvs[len(kvs)/2:]
		if envProbe(t, s, dir, fmt.Sprintf("bisect%d-lo", round), envWith(s.Env, lo...)) {
			kvs = lo
			continue
		}
		if envProbe(t, s, dir, fmt.Sprintf("bisect%d-hi", round), envWith(s.Env, hi...)) {
			kvs = hi
			continue
		}
		t.Logf("[diag] 이분탐색 중단(라운드 %d): 반쪽 둘 다 정지 — 단일 원인이 아니다. 최소 집합은 %v의 조합",
			round, envKeys(kvs))
		return
	}
	// 슬라이스째로 찍는다 — 비었을 때도 panic 없이 그 사실이 그대로 남는다.
	t.Logf("[diag] 이분탐색 결과: 단독으로 정지를 푸는 변수 = %v", kvs)
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
