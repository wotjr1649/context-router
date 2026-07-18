//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile: content.db.rebuild.lock의 1바이트([0,1)) 영역에 대한 논블로킹 exclusive
// LockFileEx 시도(windows, lockStore 계약 §설계 3.5). 경합 실패(ERROR_LOCK_VIOLATION)는
// errLockBusy로 변환해 lockStore의 백오프 재시도 대상임을 알린다 — 그 외 실패는 그대로
// 반환해 무한 재시도를 피한다. release()의 f.Close()가 핸들과 함께 잠금을 해제한다(파일
// 자체는 유지).
func tryLockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, sanitizeIOErr("lock open", err)
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockBusy
		}
		return nil, sanitizeIOErr("lock LockFileEx", err)
	}
	return func() { f.Close() }, nil
}
