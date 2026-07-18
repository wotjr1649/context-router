//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile: content.db.rebuild.lock에 대한 논블로킹 exclusive flock 시도(unix, lockStore
// 계약 §설계 3.5). 경합 실패(EWOULDBLOCK)는 errLockBusy로 변환해 lockStore의 백오프 재시도
// 대상임을 알린다 — 그 외 실패(권한 등)는 그대로 반환해 무한 재시도를 피한다. flock은 open
// file description에 결부되므로 release()의 f.Close()가 곧 잠금 해제다(파일 자체는 유지).
func tryLockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, sanitizeIOErr("lock open", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLockBusy
		}
		return nil, sanitizeIOErr("lock flock", err)
	}
	return func() { f.Close() }, nil
}
