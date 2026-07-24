//go:build darwin

package sandbox

import (
	"fmt"
	"path/filepath"
)

// wrapArgv: sandbox-exec 프로필로 argv를 감싼다(쓰기=scratch·읽기 허용). 프로필을 부모가
// 직접 입힐 수 있어 재실행이 불필요하다. /dev/null 쓰기는 허용(검토 반영 — linux 런처와 동일 취지).
func wrapArgv(scratch string, argv []string, _ string) []string {
	// SBPL subpath는 커널 canonical(심링크 해석) 경로로 매칭한다 — /var/folders는 실제로
	// /private/var/folders의 심링크라, 정규화하지 않으면 실제 쓰기가 매칭 안 돼 전면 deny된다.
	// EvalSymlinks 실패 시 원본 유지(격리 fail-closed — 최악의 경우 deny).
	canonical := scratch
	if resolved, err := filepath.EvalSymlinks(scratch); err == nil {
		canonical = resolved
	}
	profile := fmt.Sprintf(`(version 1)(allow default)(deny file-write*)(allow file-write* (subpath %q))(allow file-write* (literal "/dev/null"))`, canonical)
	return append([]string{"/usr/bin/sandbox-exec", "-p", profile}, argv...)
}

// RunLauncher: darwin은 런처 재실행 경로 미사용(wrapArgv가 sandbox-exec로 직접 감쌈).
func RunLauncher([]string) error { return fmt.Errorf("exec-launcher: darwin 미사용") }
