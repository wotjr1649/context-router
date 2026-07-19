//go:build !windows

package transform

import (
	"fmt"
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
//
// cmd.Cancel: 반드시 cmd.Start() **전에** 설정한다. exec.CommandContext가 Start() 내부에서
// 띄우는 watchCtx 고루틴은 c.Cancel 필드를 동기화 없이 읽는다(Go stdlib os/exec.go
// watchCtx: `if c.Cancel != nil { c.Cancel() }`) — Start() 후 재대입하면 그 읽기와
// unsynchronized 데이터 레이스가 된다(-race가 잡는 실제 레이스, 리뷰 Imp2/B3). kill 클로저는
// cmd.Process.Pid를 호출 시점에 지연 읽어 pgid를 얻는다 — watchCtx는 Start()가 c.Process를
// 채운 뒤에야 띄워지므로(Start() 내부 순서: StartProcess → watcher goroutine 기동) 안전하다.
func applyMemLimit(cmd *exec.Cmd, bytes int64) (func(), error) {
	cmd.Env = append(
		os.Environ(),
		"CTR_WORKER_MEM="+strconv.FormatInt(bytes, 10),
		"GOMEMLIMIT="+strconv.FormatInt(gomemlimitBytes(), 10), // D19-a: soft limit, RLIMIT_AS 하드 캡은 그대로
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	kill := func() {
		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // 이미 종료된 그룹이면 ESRCH — 무시(idempotent)
	}
	cmd.Cancel = func() error {
		kill()
		return nil
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return kill, nil
}

// selfApplyMemLimit: 부모(applyMemLimit)가 설정한 CTR_WORKER_MEM 환경변수를 읽어 자기
// 프로세스에 RLIMIT_AS를 적용한다(self-apply 방식). 값이 없으면 격리가 요구되지 않은
// 호출이므로 nil(성공)을 반환한다. 값이 있는데 파싱 실패거나 Setrlimit(2)가 거부되면
// 격리를 보장할 수 없으므로 error를 반환한다 — 호출자(RunWorker)가 이를 "no_isolation"
// 신호로 바꿔 무제한 실행을 막는다(리뷰 B2: 종전에는 실패를 버리고 조용히 무제한 계속
// 실행해 ProbeIsolation이 격리 불가 환경도 통과시켰다).
func selfApplyMemLimit() error {
	v := os.Getenv("CTR_WORKER_MEM")
	if v == "" {
		return nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return fmt.Errorf("transform: CTR_WORKER_MEM 파싱 실패: %w", err)
	}
	// hard limit(Max)은 기존 값을 그대로 물려받고 soft limit(Cur)만 낮춘다 — Max를 올리는
	// setrlimit(2) 호출은 POSIX상 비-superuser에게 EPERM일 수 있다(3-OS CI 최초 실측: macOS
	// 러너에서 Cur=Max=n으로 둘 다 설정하면 매번 실패해 ctr_transform이 자기격리 못 함으로
	// 전체 비활성됐다 — darwin RLIMIT_AS 슬롯은 RLIMIT_RSS와 공유돼 Linux의 진짜 가상주소공간
	// 상한과 기존 hard ceiling 취급이 다르다). 자기 자신에게 상한을 씌우는 목적상 Max는
	// 건드릴 필요가 없다.
	var existing syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &existing); err != nil {
		return fmt.Errorf("transform: Getrlimit(RLIMIT_AS) 실패: %w", err)
	}
	// D19-b: RLIMIT_AS는 n(순수 캡) 그대로가 아니라 rlimitASBytes(n)(n+vaHeadroomBytes)로
	// 건다 — Go 런타임이 시동 시 예약하는 VA가 256MB 캡을 넘어 조기 사망하던 문제(ubuntu
	// 실측: 할당 0, 8.4ms 사망)의 backstop. 실질 메모리 제어는 이제 GOMEMLIMIT(T2)이
	// 담당하므로 여기서 RLIMIT_AS를 넉넉히 잡아도 메모리 안전성은 그대로 유지된다.
	lim := syscall.Rlimit{Cur: uint64(rlimitASBytes(int64(n))), Max: existing.Max}
	if lim.Cur > lim.Max { // 기존 hard limit이 요청값(headroom 포함)보다 낮으면 그 한도까지만
		lim.Cur = lim.Max
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &lim); err != nil {
		return fmt.Errorf("transform: Setrlimit(RLIMIT_AS) 실패: %w", err)
	}
	return nil
}
