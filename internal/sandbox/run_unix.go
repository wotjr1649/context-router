//go:build !windows

// run_unix.go — Unix 격리 실행(설계 v0.11 D59). 부모는 Setpgid로 자식을 새 프로세스 그룹의
// 리더로 만들고, 타임아웃/취소 시 kill(-pgid)로 그룹째 종료해 손자 잔존을 막는다. FS 제한은
// OS별 wrapArgv가 입힌다(linux=landlock 런처 재실행, darwin=sandbox-exec, 기타=미제한).
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// Probe: unix는 landlock/sandbox-exec가 BestEffort라 등록 프로브 비대상(항상 nil).
func Probe(context.Context) error { return nil }

func Run(ctx context.Context, s Spec) (Result, error) {
	// pre-Start: ctx가 이미 종료됐으면 격리 준비 실패로 오분류하지 않는다(run_windows.go
	// 선례와 동일 계약) — 취소는 context 오류로, deadline 만료는 타임아웃 Result로 전파.
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{TimedOut: true, ExitCode: -1}, nil
		}
		return Result{}, err
	}
	// Linux는 런처 재실행에 자기 바이너리 경로가 필수 — 없으면 fail-closed(검토 반영).
	// darwin/other는 selfExe 미사용이라 무관.
	if runtime.GOOS == "linux" && s.SelfExe == "" {
		return Result{}, fmt.Errorf("%w: selfExe 미지정", ErrSetup)
	}

	argv := wrapArgv(s.Dir, s.Argv, s.SelfExe) // OS별 FS 제한 입힘
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &capWriter{cap: s.StdoutCap}
	errw := &capWriter{cap: s.StderrCap}
	cmd.Stdout, cmd.Stderr = out, errw
	// WaitDelay: 킬 후 파이프 회수 시한(전 OS 공통, D59). 반드시 Start 전에 설정한다.
	// 그룹 kill이 못 미친 자손(예: setsid로 그룹 이탈)이 stdout 파이프를 붙들면 cmd.Wait의
	// I/O 대기가 무한 블록하는데, WaitDelay 초과 시 os/exec가 파이프를 강제 회수해 Wait가
	// 부분 출력으로 반환한다(run_windows.go 선례와 동형).
	cmd.WaitDelay = waitDelay

	// statusR: Linux 런처가 격리 준비 실패를 알리는 상태 파이프(자식 fd 3). 런처는 성공 시
	// exec 직전 CLOEXEC로 이 fd를 닫아 부모가 EOF(=정상)를 보고, 실패 시 1바이트를 써 부모가
	// ErrSetup으로 매핑한다(D61 fail-closed). 대상의 정당한 exit code와 겹치지 않는다 —
	// 신호는 런처 실패 경로에서만 나오고, exec 성공 후 대상 프로세스는 fd 3을 갖지 않는다.
	var statusR, statusW *os.File
	if runtime.GOOS == "linux" {
		r, w, perr := os.Pipe()
		if perr != nil {
			return Result{}, fmt.Errorf("%w: 상태 파이프", ErrSetup)
		}
		statusR, statusW = r, w
		cmd.ExtraFiles = []*os.File{w} // 자식에서 fd 3
		defer func() { _ = statusR.Close() }()
		defer func() { _ = statusW.Close() }() // Start 실패 경로 안전망(성공 경로는 아래서 즉시 닫음)
	}

	if err := cmd.Start(); err != nil {
		// 원본 err는 해석된 실행 파일 경로를 담을 수 있어 로그로만 남기고, 반환 메시지는
		// 경로 없이 유지한다(run_windows.go 선례와 동일).
		slog.Error("sandbox: 자식 프로세스 시작 실패", "err", err)
		return Result{}, fmt.Errorf("%w: 프로세스 시작", ErrSetup)
	}
	// 부모 쪽 write end를 즉시 닫아 자식 종료 시 read가 EOF를 관측하게 한다(안 닫으면 블록).
	if statusW != nil {
		_ = statusW.Close()
		statusW = nil
	}
	pgid := cmd.Process.Pid // Setpgid로 pgid == pid
	kill := func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(s.Timeout)
	defer timer.Stop()

	var res Result
	select {
	case werr := <-done:
		// 런처 setup 실패 신호 확인 — 자식이 종료해 write end가 닫혔으므로 read는 즉시
		// 반환한다(1바이트+=실패, EOF=정상). 실패면 exit code 1을 정상 종료로 오분류하지
		// 않고 ErrSetup으로 매핑한다(#2 fail-closed).
		if statusR != nil {
			var b [1]byte
			if n, _ := statusR.Read(b[:]); n > 0 {
				slog.Error("sandbox: 런처 격리 준비 실패 신호")
				return Result{}, fmt.Errorf("%w: 런처 제한 적용", ErrSetup)
			}
		}
		res.ExitCode = exitCodeOf(werr, cmd)
	case <-timer.C:
		res.TimedOut = true
		kill()
		<-done
		res.ExitCode = -1
	case <-ctx.Done():
		// 부모 취소 — 그룹째 종료하고 회수. TimedOut=false로 두어 타임아웃과 구분한다(T2 계약).
		kill()
		<-done
		res.ExitCode = -1
	}
	res.Stdout, res.StdoutTrunc = out.buf.Bytes(), out.trunc
	res.Stderr, res.StderrTrunc = errw.buf.Bytes(), errw.trunc
	res.Duration = time.Since(start)
	return res, nil
}

// exitCodeOf: 정상 종료면 exit code, 그 외 -1.
func exitCodeOf(werr error, cmd *exec.Cmd) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if werr != nil {
		return -1
	}
	return 0
}
