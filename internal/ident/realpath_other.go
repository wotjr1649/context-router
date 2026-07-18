//go:build !windows

package ident

import "path/filepath"

// RealPath: unix에는 junction 개념이 없고 filepath.EvalSymlinks가 심링크 체인을 완전히
// 해석한다 — windows 전용 realpath_windows.go와 대칭(게이트3 심층 P2-1a). Export됨(계획3
// 최종리뷰 F1) — ident.Canonicalize 외에 ingest.canonicalPath·main의 canonicalize 헬퍼도
// 이 함수를 공유한다.
func RealPath(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}
