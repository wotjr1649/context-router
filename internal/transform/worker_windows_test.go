//go:build windows

package transform

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestApplyMemLimit_SetsGOMEMLIMIT: D19-a — applyMemLimit이 자식 env에
// GOMEMLIMIT=<Job캡의 80%>(바이트 정수 문자열)를 주입하는지 검증한다. windows/unix
// applyMemLimit 모두 이 값을 공유하므로(gomemlimitBytes()) 공통 로직 검증을 windows 테스트
// 파일에서 cmd.Env 직접 검사로 수행한다.
func TestApplyMemLimit_SetsGOMEMLIMIT(t *testing.T) {
	cmd := exec.Command("ping", "-n", "2", "127.0.0.1")
	cleanup, err := applyMemLimit(cmd, defaultMemLimitBytes)
	if err != nil {
		t.Fatalf("applyMemLimit: %v", err)
	}
	defer cleanup()
	defer cmd.Wait()

	const want = "GOMEMLIMIT=214748364" // 256MB * 0.8
	for _, e := range cmd.Env {
		if e == want {
			return
		}
	}
	t.Fatalf("cmd.Env want contains %q, got %v", want, cmd.Env)
}

// TestSpawn_Churn20x_NoMemKill: D19-a 실측 — session-04 churn 레시피(1MB 문자열 120개
// 누적)를 20회 반복 Spawn한다. GOMEMLIMIT 주입 전에는 GC의 commit 반환 지연으로 Job 캡
// (256MB) 도달 전에 커밋이 쌓여 통계적 mem-kill("worker killed")이 관찰됐다(브리프 Step2:
// 7/20 수준). GOMEMLIMIT=Job캡의 80%에서 선제 GC를 발동시켜 이 죽음 모드를 없애는 게 목표
// — 20회 전부 "worker killed" 부재를 요구한다(스크립트 자체 오류(script/budget 등)는 허용,
// mem-kill·timeout-kill만 배제). 반복 프로세스 스폰 비용이 커 -short에서는 스킵한다.
func TestSpawn_Churn20x_NoMemKill(t *testing.T) {
	if testing.Short() {
		t.Skip("churn 20x는 반복 프로세스 스폰 비용이 커 -short에서 스킵")
	}
	exe := testSelfExe(t)

	script := "def churn():\n" +
		"\ts = \"A\" * 1048576\n" +
		"\tout = []\n" +
		"\tfor i in range(120):\n" +
		"\t\tout.append(s + str(i))\n" +
		"\treturn len(out)\n\n" +
		"churn()\n"

	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		res, err := Spawn(ctx, exe, Request{Script: script})
		cancel()
		if err != nil {
			t.Fatalf("run %d: Spawn returned Go error (parent must survive): %v", i, err)
		}
		if strings.Contains(res.ErrSummary, "worker killed") {
			t.Fatalf("run %d: ErrSummary=%q want no \"worker killed\" (GOMEMLIMIT 선제 GC 미작동 의심)", i, res.ErrSummary)
		}
	}
}

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
