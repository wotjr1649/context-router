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

	if err := cmd.Start(); err != nil {
		// 원본 err는 해석된 실행 파일 경로를 담을 수 있어 로그로만 남기고, 반환 메시지는
		// 경로 없이 유지한다(run_windows.go 선례와 동일).
		slog.Error("sandbox: 자식 프로세스 시작 실패", "err", err)
		return Result{}, fmt.Errorf("%w: 프로세스 시작", ErrSetup)
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
