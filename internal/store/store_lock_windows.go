//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile: path의 1바이트([0,1)) 영역에 대한 논블로킹 LockFileEx 시도(windows,
// lockStore 계약 §설계 3.5 + AcquireLock 공개 API §설계 8). shared=false는
// LOCKFILE_EXCLUSIVE_LOCK, shared=true는 flags 0(shared) — 둘 다 LOCKFILE_FAIL_IMMEDIATELY
// 병기. 경합 실패(ERROR_LOCK_VIOLATION)는 errLockBusy로 변환해 lockStore의 백오프 재시도
// 대상임을 알린다 — 그 외 실패는 그대로 반환해 무한 재시도를 피한다. release()의
// f.Close()가 핸들과 함께 잠금을 해제한다(파일 자체는 유지). 같은 프로세스에서도
// os.OpenFile을 별도로 호출하면 핸들이 갈라지므로 shared+shared는 정상적으로 공존한다.
func tryLockFile(path string, shared bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, sanitizeIOErr("lock open", err)
	}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol); err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockBusy
		}
		return nil, sanitizeIOErr("lock LockFileEx", err)
	}
	return func() { f.Close() }, nil
}
