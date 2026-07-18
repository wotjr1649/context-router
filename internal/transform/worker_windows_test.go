//go:build windows

package transform

import (
	"os/exec"
	"testing"
	"time"
)

// TestKillOrphan_KillsStartedProcess: 리뷰 Critical 회귀 가드. applyMemLimit이 Start() 이후
// OpenProcess/AssignProcessToJobObject 실패 분기에서 호출하는 killOrphan이 실제로 자식을
// kill+reap하는지 검증한다. 실환경에서 Assign 자체를 실패시키긴 어려워(권한 상승 없이 재현
// 불가 — nested job 등은 CI 이월) killOrphan을 "Start된 cmd + 강제 호출"로 직접 단위 테스트한다.
// applyMemLimit 쪽 실패 분기 2곳이 killOrphan을 호출하는지는 코드 리뷰로 보장한다.
func TestKillOrphan_KillsStartedProcess(t *testing.T) {
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1") // ~30s 대기, stdin 불필요
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		killOrphan(cmd) // 내부에서 Kill()+Process.Wait()까지 수행
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("killOrphan 이후에도 프로세스가 살아있음(자식 leak 의심)")
	}
}
