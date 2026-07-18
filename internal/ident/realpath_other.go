//go:build !windows

package ident

import "path/filepath"

// realPath: unix에는 junction 개념이 없고 filepath.EvalSymlinks가 심링크 체인을 완전히
// 해석한다 — windows 전용 realpath_windows.go와 대칭(게이트3 심층 P2-1a).
func realPath(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}
