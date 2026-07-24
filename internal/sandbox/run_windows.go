//go:build windows

// run_windows.go — Windows 격리 실행(설계 v0.11 D59). Job Object로 프로세스 트리를
// 묶어 kill-on-job-close로 트리째 종료하고, 메모리·활성 프로세스 수 캡을 건다. 자식은
// CREATE_SUSPENDED로 만든 뒤 잡에 배정하고서야 재개해 배정 전 손자 스폰을 막는다.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job 캡 기본값(D59, 구현 계획에서 확정): 잡 메모리 4GiB·활성 프로세스 64.
// (WaitDelay는 sandbox.go의 공통 waitDelay 상수 — 전 OS 동일.)
const (
	defaultJobMemoryBytes uint64 = 4 << 30
	defaultProcLimit      uint32 = 64
)

// newJob: kill-on-job-close + 메모리·활성 프로세스 캡을 건 Job Object를 만든다.
func newJob(s Spec) (windows.Handle, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	memBytes := s.MemLimitBytes
	if memBytes == 0 {
		memBytes = defaultJobMemoryBytes
	}
	procLimit := s.ProcLimit
	if procLimit == 0 {
		procLimit = defaultProcLimit
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
				windows.JOB_OBJECT_LIMIT_JOB_MEMORY |
				windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS,
			ActiveProcessLimit: procLimit,
		},
		// ponytail: uintptr은 386에서 32비트라 4GiB가 잘리지만 386은 타깃 아님(amd64/arm64는 온전).
		JobMemoryLimit: uintptr(memBytes),
	}
	if _, err := windows.SetInformationJobObject(
		h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

// Probe: 이 환경에서 Job Object 생성/캡 설정이 가능한지 최소 확인(fail-closed 등록용).
func Probe(context.Context) error {
	h, err := newJob(Spec{})
	if err != nil {
		return fmt.Errorf("%w: Job Object 준비 불가: %v", ErrSetup, err)
	}
	windows.CloseHandle(h)
	return nil
}

// RunLauncher: windows는 Job Object 모델이라 런처 재실행 경로 미사용(darwin 선례와 동일 스텁).
// main.go의 __exec-launcher 분기가 모든 OS에서 이 심볼을 참조하므로 정의가 필요하다.
func RunLauncher([]string) error { return fmt.Errorf("exec-launcher: windows 미사용") }

// assignToJob: pid의 프로세스 핸들을 열어 잡에 배정한다(least-privilege 권한).
func assignToJob(job windows.Handle, pid int) error {
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	err = windows.AssignProcessToJobObject(job, ph)
	windows.CloseHandle(ph)
	return err
}

// resumeMainThread: CREATE_SUSPENDED로 만든 프로세스의 (유일한) 주 스레드를 재개한다.
// os/exec가 CreateProcess의 스레드 핸들을 노출하지 않으므로 Toolhelp 스냅샷으로 pid의
// 첫 스레드를 찾아 ResumeThread한다.
func resumeMainThread(pid int) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)

	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	if err := windows.Thread32First(snap, &te); err != nil {
		return err
	}
	for {
		if te.OwnerProcessID == uint32(pid) {
			th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
			if err != nil {
				return err
			}
			_, err = windows.ResumeThread(th)
			windows.CloseHandle(th)
			return err
		}
		if err := windows.Thread32Next(snap, &te); err != nil {
			return err // ERROR_NO_MORE_FILES = pid 스레드 미발견
		}
	}
}

// Run: Spec을 Job Object 안에서 실행하고 상한 버퍼로 출력을 회수한다. 격리 준비 실패만
// ErrSetup으로 반환하고, 사용자 코드의 종료·타임아웃은 Result로 담아 err=nil을 반환한다.
func Run(ctx context.Context, s Spec) (Result, error) {
	job, err := newJob(s)
	if err != nil {
		return Result{}, fmt.Errorf("%w: Job 준비: %v", ErrSetup, err)
	}
	var once sync.Once
	// terminateJob: 타임아웃/취소·정상 정리 공통의 1차 teardown. TerminateJobObject로 잡의
	// 전 프로세스를 결정적으로 종료한 뒤 핸들을 닫는다 — 핸들 수와 무관하게 트리를 끝내므로
	// KILL_ON_JOB_CLOSE의 "마지막 핸들" 전제가 무너져도(핸들이 어디선가 살아있어도) 잔존이
	// 없다. KILL_ON_JOB_CLOSE는 정리 없이 죽는 크래시 경로의 백스톱으로만 남긴다. Cancel과
	// defer 양쪽에서 불려 sync.Once로 1회화.
	terminateJob := func() error {
		var e error
		once.Do(func() {
			_ = windows.TerminateJobObject(job, 1)
			e = windows.CloseHandle(job)
		})
		return e
	}
	defer terminateJob()

	timeoutCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	out := &capWriter{cap: s.StdoutCap}
	errw := &capWriter{cap: s.StderrCap}
	cmd.Stdout, cmd.Stderr = out, errw
	// Cancel/WaitDelay는 반드시 Start() 전에 설정한다 — Start() 내부 watchCtx 고루틴이 이
	// 필드를 동기화 없이 읽으므로 Start() 후 재대입은 데이터 레이스(transform worker 리뷰
	// 선례). ctx 만료 시 잡을 명시적으로 종료해 트리째 끝내고, WaitDelay 내 파이프가 안
	// 닫히면 os/exec가 강제 회수해 Wait가 부분 출력으로 반환한다(D59).
	cmd.Cancel = terminateJob
	cmd.WaitDelay = waitDelay

	start := time.Now()
	if err := cmd.Start(); err != nil {
		// ctx가 Start 전/중에 이미 만료·취소된 경우는 격리 준비 실패가 아니다: deadline은
		// 타임아웃 Result로, cancel은 context 오류로 전파한다(ErrSetup으로 감싸지 않음).
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return Result{TimedOut: true, ExitCode: -1, Duration: time.Since(start)}, nil
		case errors.Is(err, context.Canceled):
			return Result{}, context.Canceled
		}
		// 원본 err는 해석된 실행 파일 경로를 담을 수 있어 로그로만 남기고, 반환 메시지는
		// 경로 없이 유지한다.
		slog.Error("sandbox: 자식 프로세스 시작 실패", "err", err)
		return Result{}, fmt.Errorf("%w: 프로세스 시작", ErrSetup)
	}

	// CREATE_SUSPENDED → 배정 → 재개: 자식이 잡 배정 전에 손자를 만들지 못하게 한다.
	if err := assignToJob(job, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill() // 아직 잡 미배정 — 직접 kill(배정 후면 terminateJob이 커버)
		_ = cmd.Wait()         // reap + watchCtx 고루틴 회수
		return Result{}, fmt.Errorf("%w: Job 배정: %v", ErrSetup, err)
	}
	if err := resumeMainThread(cmd.Process.Pid); err != nil {
		_ = terminateJob() // 자식은 잡에 배정됨 → 잡 종료로 (미재개 상태라도) 정리
		_ = cmd.Wait()     // reap
		return Result{}, fmt.Errorf("%w: 스레드 재개: %v", ErrSetup, err)
	}

	werr := cmd.Wait()

	var res Result
	res.Stdout, res.StdoutTrunc = out.buf.Bytes(), out.trunc
	res.Stderr, res.StderrTrunc = errw.buf.Bytes(), errw.trunc
	res.Duration = time.Since(start)
	if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1 // TimedOut이면 무의미 — MCP 계층이 null로 변환
		return res, nil
	}
	res.ExitCode = exitCodeOf(werr, cmd)
	return res, nil
}

// exitCodeOf: 정상/비정상 종료의 exit code를 반환한다. Wait가 *exec.ExitError가 아닌
// non-nil 에러를 낸 경우(비정상 회수)는 -1.
func exitCodeOf(werr error, cmd *exec.Cmd) int {
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return -1
		}
	}
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return -1
}
