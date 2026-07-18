//go:build windows

package transform

import (
	"os/exec"
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
// cmd.Cancel: exec.CommandContext는 Start() 호출 시점에 "watcher" 고루틴을 이미 띄우고,
// 그 고루틴은 ctx.Done() 시점에 c.Cancel 필드를 **그때 값으로** 읽는다(Go 1.20+
// os/exec.watchCtx). 즉 Start() 이후 아무 때나 cmd.Cancel을 재설정해도 안전하다 — Job
// 배정 이전에 취소되면 CommandContext의 기본 Cancel(Process.Kill)이 이미 유효하고, 배정
// 이후에는 아래에서 closeJob으로 교체해 Job 전체(KILL_ON_JOB_CLOSE)를 트리킬한다.
func applyMemLimit(cmd *exec.Cmd, bytes int64) (func(), error) {
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
		windows.CloseHandle(job)
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	procHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		killOrphan(cmd)
		windows.CloseHandle(job)
		return nil, err
	}
	assignErr := windows.AssignProcessToJobObject(job, procHandle)
	windows.CloseHandle(procHandle)
	if assignErr != nil {
		killOrphan(cmd)
		windows.CloseHandle(job)
		return nil, assignErr
	}

	var once sync.Once
	// closeJob: 마지막 Job 핸들 close 시 KILL_ON_JOB_CLOSE가 배정된 프로세스 트리 전체를
	// 종료한다. cleanup(정상 완료 후)과 cmd.Cancel(timeout/ctx 취소) 양쪽에서 공유 — 정상
	// 종료 후 재호출돼도 sync.Once로 안전.
	closeJob := func() { once.Do(func() { windows.CloseHandle(job) }) }
	cmd.Cancel = func() error {
		closeJob()
		return nil
	}
	return closeJob, nil
}

// selfApplyMemLimit: windows는 부모가 Job Object로 이미 상한을 적용하므로 자식 self-apply가
// 불필요하다.
func selfApplyMemLimit() {}

// killOrphan: Start() 이후 Job 배정(OpenProcess/AssignProcessToJobObject) 실패 시 호출한다.
// 이 시점엔 자식이 아직 job에 assign되지 않아 TerminateJobObject(job,*)로는 죽지 않으므로
// (리뷰 Critical: 이전 코드는 이걸로 착각해 자식을 leak했다) cmd.Process를 직접 kill하고
// Wait()로 reap해 좀비를 방지한다.
func killOrphan(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}
