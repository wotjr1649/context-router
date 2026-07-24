//go:build !windows

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// testSelfExe: 실 ctr 바이너리를 1회만 빌드해(sync.Once) 재사용한다 — Linux wrapArgv가
// selfExe를 `__exec-launcher`로 재실행하므로 실 바이너리(분기 보유)를 넘겨야 재귀가 구조적으로
// 불가능하다. worker_test.go:26 선례의 패키지별 복제 관례(검토 반영 note 2).
var (
	testExeOnce sync.Once
	testExePath string
	testExeErr  error
)

func testSelfExe(t *testing.T) string {
	t.Helper()
	testExeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ctr-sandbox-test-*")
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

// selfExe: 검토 반영 — os.Executable()(테스트 바이너리) 금지: Linux wrapArgv가 그걸
// 재실행하면 테스트 스위트가 재귀한다. testSelfExe(실 ctr 바이너리 1회 빌드)를 재사용.
func selfExe(t *testing.T) string { return testSelfExe(t) }

func TestRunUnixEchoExit(t *testing.T) {
	dir := t.TempDir()
	s := Spec{
		Argv: []string{"/bin/sh", "-c", "printf hi; exit 5"},
		Dir:  dir, Env: BaseEnv(), SelfExe: selfExe(t),
		Timeout: 30 * time.Second, StdoutCap: 32768, StderrCap: 8192,
	}
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.ExitCode != 5 || string(r.Stdout) != "hi" {
		t.Fatalf("exit=%d out=%q", r.ExitCode, r.Stdout)
	}
}

func TestRunUnixTimeoutKillsGroup(t *testing.T) {
	dir := t.TempDir()
	s := Spec{
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
		Dir:  dir, Env: BaseEnv(), SelfExe: selfExe(t),
		Timeout: 1 * time.Second, StdoutCap: 32768, StderrCap: 8192,
	}
	start := time.Now()
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !r.TimedOut || time.Since(start) > 10*time.Second {
		t.Fatalf("그룹 미종료 TimedOut=%v dur=%v", r.TimedOut, time.Since(start))
	}
}

func TestStdoutCapTruncates(t *testing.T) {
	dir := t.TempDir()
	s := Spec{
		Argv: []string{"/bin/sh", "-c", "yes x | head -c 100000"},
		Dir:  dir, Env: BaseEnv(), SelfExe: selfExe(t),
		Timeout: 30 * time.Second, StdoutCap: 32768, StderrCap: 8192,
	}
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !r.StdoutTrunc || len(r.Stdout) != 32768 {
		t.Fatalf("cap 미적용 trunc=%v len=%d", r.StdoutTrunc, len(r.Stdout))
	}
}

// TestRunUnixPreDoneContext: Start 전 ctx가 이미 종료된 경우 격리 준비 실패로 오분류하지
// 않는다 — 취소는 context 오류로 전파, deadline 만료는 타임아웃 Result로 변환(run_windows.go
// 선례와 동일 계약). SelfExe 미설정: ctx 분류가 fail-closed SelfExe 검사보다 먼저임을 겸해 가드.
func TestRunUnixPreDoneContext(t *testing.T) {
	dir := t.TempDir()
	mk := func() Spec {
		return Spec{
			Argv: []string{"/bin/sh", "-c", "printf hi"},
			Dir:  dir, Env: BaseEnv(),
			Timeout: 30 * time.Second, StdoutCap: 32768, StderrCap: 8192,
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
