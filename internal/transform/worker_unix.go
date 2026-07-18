//go:build !windows

package transform

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// applyMemLimit: unix — RLIMIT_AS는 setrlimit(2) 시스템콜 특성상 부모가 아니라 **자식
// 프로세스 자신**이 걸어야 한다. 그래서 여기서는 직접 상한을 걸지 않고 환경변수
// CTR_WORKER_MEM=<bytes>를 자식에 전달만 한다 — RunWorker 진입 시 selfApplyMemLimit()이
// 이를 읽어 자기 프로세스에 Setrlimit(RLIMIT_AS)를 적용한다(self-apply 방식).
// Setpgid로 자식을 독립 프로세스 그룹으로 분리해, cleanup/cmd.Cancel에서 그룹 전체를
// kill(-pgid, SIGKILL)해 트리킬한다(설계 §4.3 kill(-pgid) 요구사항).
// CI 이월: 이 파일은 darwin/linux 공용(!windows) — 로컬 실측은 linux/darwin 미보유로
// `GOOS=linux go build ./...` 컴파일 확인까지만 수행했다(Windows 머신, task-2 브리프 §협업).
func applyMemLimit(cmd *exec.Cmd, bytes int64) (func(), error) {
	cmd.Env = append(os.Environ(), "CTR_WORKER_MEM="+strconv.FormatInt(bytes, 10))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	pgid := cmd.Process.Pid
	kill := func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL) // 이미 종료된 그룹이면 ESRCH — 무시(idempotent)
	}
	cmd.Cancel = func() error {
		kill()
		return nil
	}
	return kill, nil
}

// selfApplyMemLimit: 부모(applyMemLimit)가 설정한 CTR_WORKER_MEM 환경변수를 읽어 자기
// 프로세스에 RLIMIT_AS를 적용한다(self-apply 방식). 값이 없거나 파싱 실패면 조용히 skip —
// 이 경우 부모 쪽 프로세스 그룹 kill(timeout)만 방어선으로 남는다.
func selfApplyMemLimit() {
	v := os.Getenv("CTR_WORKER_MEM")
	if v == "" {
		return
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return
	}
	lim := syscall.Rlimit{Cur: n, Max: n}
	_ = syscall.Setrlimit(syscall.RLIMIT_AS, &lim)
}
