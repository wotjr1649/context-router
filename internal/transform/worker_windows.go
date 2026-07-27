//go:build windows

package transform

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyMemLimit: Job Object 기반 OS 메모리 상한(설계 §4.3). exec.CommandContext로 만든 cmd는
// CREATE_SUSPENDED로 시작할 수 없고 Go의 os/exec는 CreateProcess의 스레드 핸들을 노출하지
// 않아 ResumeThread 경로를 탈 수 없다 — 대신 허용된 대안을 쓴다: Start() 직후 즉시
// AssignProcessToJobObject. Start~Assign 사이의 경합창(수십 마이크로초)에 할당된 메모리는
// 상한 밖이지만, 그 창은 cmd.Process.Kill 기본 취소 로직이 여전히 커버하고(아래 참고) 이후
// 모든 할당에는 상한이 적용된다.
//
// cmd.Cancel: 일부러 건드리지 않는다. exec.CommandContext(Spawn에서 cmd 생성 시점)가 이미
// `func() error { return cmd.Process.Kill() }`를 Start() **전에** 심어둔다 — Start() 내부에서
// 띄우는 watchCtx 고루틴이 c.Cancel을 동기화 없이 읽으므로(Go stdlib os/exec.go), Start() 후
// 재대입하면 그 읽기와 unsynchronized 데이터 레이스가 된다(-race가 잡는 실제 레이스, 리뷰
// Imp2/B3 — 이전 코드는 여기서 closeJob으로 교체했었다). 이 worker는 자식을 스폰하지 않으므로
// (starlark 샌드박스에 서브프로세스 실행 수단이 없다) 기본 Cancel(단일 프로세스 kill)만으로
// 트리킬에 충분하다 — closeJob은 아래 Job 핸들의 **정상 경로 자원 해제**(cleanup 반환값)
// 전용으로만 쓴다.
func applyMemLimit(cmd *exec.Cmd, bytes int64) (func(), error) {
	// D19-a: GOMEMLIMIT(소프트) 주입 — Job 캡(하드)은 그대로 유지. 현재 cmd.Env는 미설정
	// (nil=상속)이므로 os.Environ()에 이어붙인다.
	cmd.Env = append(os.Environ(), "GOMEMLIMIT="+strconv.FormatInt(gomemlimitBytes(), 10))

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
		ProcessMemoryLimit: uintptr(bytes),
	}
	if _, err := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	procHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		killOrphan(cmd)
		_ = windows.CloseHandle(job)
		return nil, err
	}
	assignErr := windows.AssignProcessToJobObject(job, procHandle)
	_ = windows.CloseHandle(procHandle)
	if assignErr != nil {
		killOrphan(cmd)
		_ = windows.CloseHandle(job)
		return nil, assignErr
	}

	var once sync.Once
	// closeJob: 마지막 Job 핸들 close 시 KILL_ON_JOB_CLOSE가 배정된 프로세스(트리)를 종료한다.
	// Spawn의 defer cleanup()(정상/타임아웃 후 공통 경로)에서만 호출된다 — cmd.Cancel에는
	// 더 이상 배선하지 않는다(위 주석, 리뷰 B3). 재호출돼도 sync.Once로 안전.
	closeJob := func() { once.Do(func() { _ = windows.CloseHandle(job) }) }
	return closeJob, nil
}

// selfApplyMemLimit: windows는 부모가 Job Object로 이미 상한을 적용하므로 자식 self-apply가
// 불필요하다 — 항상 성공(nil)을 반환한다(unix는 자식이 자기 자신에게 Setrlimit을 걸어야 해서
// 실패할 수 있다, 리뷰 B2).
func selfApplyMemLimit() error { return nil }

// killOrphan: Start() 이후 Job 배정(OpenProcess/AssignProcessToJobObject) 실패 시 호출한다.
// 이 시점엔 자식이 아직 job에 assign되지 않아 TerminateJobObject(job,*)로는 죽지 않으므로
// (리뷰 Critical: 이전 코드는 이걸로 착각해 자식을 leak했다) cmd.Process를 직접 kill하고
// Wait()로 reap해 좀비를 방지한다.
func killOrphan(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}
