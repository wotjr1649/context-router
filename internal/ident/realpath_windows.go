//go:build windows

package ident

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// realPath: windows에서 filepath.EvalSymlinks는 심링크(IO_REPARSE_TAG_SYMLINK)만 인식하고
// NTFS junction(IO_REPARSE_TAG_MOUNT_POINT)은 인식하지 못한다 — os.Lstat이 junction에
// os.ModeSymlink를 세우지 않아 EvalSymlinks의 내부 워크가 이를 건너뛴다(golang.org/x/sys/windows
// 조사로 실측 확인, 게이트3 심층 P2-1a, Codex 교차리뷰). junction 경유 경로가 실경로로
// canonicalize되지 않으면 §4.4 경계 판정(allow-path/store-root 등)이 junction으로 우회될 수
// 있다. GetFinalPathNameByHandle은 심링크·junction 등 모든 reparse point 체인을 최종 실경로
// (`\\?\...` 확장 접두 포함)로 풀어준다 — 그 접두는 Fold()가 이미 벗겨낸다(UNC 확장경로 처리
// 참조).
func realPath(p string) (string, error) {
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", fmt.Errorf("realpath: %w", err)
	}
	h, err := windows.CreateFile(ptr, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("realpath: open: %w", err)
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, 260)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", fmt.Errorf("realpath: GetFinalPathNameByHandle: %w", err)
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1) // n=필요 버퍼 크기(널 미포함) — 재시도
	}
}
