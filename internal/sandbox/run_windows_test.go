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
// Env는 닫힌 표에 PSModulePath를 명시로 덧붙인다(exec.go shellRunner의 재지정과 같은 모양).
// D65에서 이 키가 표에서 빠진 뒤로, 값이 없으면 PowerShell이 유효 모듈 경로를 호스트 상태
// (HKLM/HKCU 환경값·셸 폴더)에서 스스로 재구성한다 — 개발기에서는 시스템 모듈 경로가 되살아나
// 통과하지만 CI(windows-latest)에서는 루트가 손자 PID를 남기지 못했다(v0.11 3.07s 초록 →
// v0.12 6.04s 실패, 이 패키지의 유일한 변경이 그 키의 제거였다). 즉 이 테스트는 호스트의
// PSModulePath에 암묵적으로 기대고 있었고, 명시 주입이 자식의 유효 모듈 경로를 호스트와
// 무관하게 고정한다. bare BaseEnv()로 "단순화"하지 말 것 — 표에 되돌려 넣는 것도 금지다
// (그건 D65가 닫은 호스트 상속이다).
func TestRunWindowsTimeoutKillsTree(t *testing.T) {
	psExe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatalf("powershell.exe 미해석 — 트리 종료 검증 불가: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "gc.pid")
	script := "$psi=New-Object System.Diagnostics.ProcessStartInfo;" +
		"$psi.FileName='ping';$psi.Arguments='-n 60 127.0.0.1';$psi.UseShellExecute=$false;" +
		"$p=[System.Diagnostics.Process]::Start($psi);" +
		"Set-Content -LiteralPath '" + pidFile + "' -Value $p.Id;" +
		"$p.WaitForExit()"
	s := Spec{
		Argv:      []string{psExe, "-NoProfile", "-NonInteractive", "-Command", script},
		Dir:       dir,
		Env:       append(BaseEnv(), "PSModulePath="+filepath.Join(filepath.Dir(psExe), "Modules")),
		Timeout:   3 * time.Second,
		StdoutCap: 32768, StderrCap: 8192,
	}
	start := time.Now()
	r, err := Run(context.Background(), s)
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
