//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// wrapArgv: 자기 재실행 런처로 감싼다 — 런처가 landlock을 건 뒤 실제 argv로 대체한다.
// landlock은 자식 프로세스 자신이 걸어야 하므로 부모가 argv를 직접 감쌀 수 없다(D59).
func wrapArgv(scratch string, argv []string, selfExe string) []string {
	out := []string{selfExe, "__exec-launcher", scratch, "--"}
	return append(out, argv...)
}

// applyLauncherRestriction: 쓰기=scratch·읽기=전역(BestEffort). 미지원 커널은 통과한다.
// /dev/null 쓰기는 허용(셸 리다이렉트 등 일상 유틸 경로 — 검토 반영).
func applyLauncherRestriction(scratch string) error {
	return landlock.V5.BestEffort().RestrictPaths(
		landlock.RWDirs(scratch),
		landlock.RODirs("/"),
		landlock.RWFiles("/dev/null"),
	)
}

// RunLauncher: `__exec-launcher <scratch> -- <argv...>` 진입점(main.go가 분기). 제한을 건 뒤
// syscall.Exec로 실제 argv로 프로세스 이미지를 대체한다(성공 시 반환 없음).
func RunLauncher(args []string) error {
	if len(args) < 2 || args[1] != "--" {
		return fmt.Errorf("exec-launcher: 잘못된 인자")
	}
	scratch, argv := args[0], args[2:]
	if err := applyLauncherRestriction(scratch); err != nil {
		return fmt.Errorf("exec-launcher: 제한 적용 실패: %w", err)
	}
	if len(argv) == 0 {
		return fmt.Errorf("exec-launcher: 빈 argv")
	}
	return syscall.Exec(argv[0], argv, os.Environ()) // 성공 시 반환 없음
}
