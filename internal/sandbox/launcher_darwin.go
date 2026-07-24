//go:build darwin

package sandbox

import "fmt"

// wrapArgv: sandbox-exec 프로필로 argv를 감싼다(쓰기=scratch·읽기 허용). 프로필을 부모가
// 직접 입힐 수 있어 재실행이 불필요하다. /dev/null 쓰기는 허용(검토 반영 — linux 런처와 동일 취지).
func wrapArgv(scratch string, argv []string, _ string) []string {
	profile := fmt.Sprintf(`(version 1)(allow default)(deny file-write*)(allow file-write* (subpath %q))(allow file-write* (literal "/dev/null"))`, scratch)
	return append([]string{"/usr/bin/sandbox-exec", "-p", profile}, argv...)
}

// RunLauncher: darwin은 런처 재실행 경로 미사용(wrapArgv가 sandbox-exec로 직접 감쌈).
func RunLauncher([]string) error { return fmt.Errorf("exec-launcher: darwin 미사용") }
