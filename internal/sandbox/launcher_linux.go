//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"strings"
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
// /dev/null 쓰기는 허용(셸 리다이렉트 등 일상 유틸 경로 — 검토 반영). scratch에는 REFER를
// 부여해 스크래치 내부 rename/link(빌드·캐시·원자적 쓰기 패턴)를 ABI 2+ 커널에서 허용한다
// — WithRefer 없이는 modern 커널에서 이 정당한 연산이 거부된다(#3).
func applyLauncherRestriction(scratch string) error {
	return landlock.V5.BestEffort().RestrictPaths(
		landlock.RWDirs(scratch).WithRefer(),
		landlock.RODirs("/"),
		landlock.RWFiles("/dev/null"),
	)
}

// statusFD: 부모가 ExtraFiles로 넘긴 상태 파이프(fd 3). 격리 준비 실패를 부모에 알린다.
const statusFD = 3

// RunLauncher: `__exec-launcher <scratch> -- <argv...>` 진입점(main.go가 분기). 제한을 건 뒤
// syscall.Exec로 실제 argv로 프로세스 이미지를 대체한다(성공 시 반환 없음). 성공 시 fd 3은
// CLOEXEC로 닫혀 부모가 EOF(=정상)를 보고, 실패 시 fd 3에 1바이트를 써 부모 Run이 exit
// code 1을 정상 종료로 오분류하지 않고 ErrSetup으로 매핑하게 한다(#2 fail-closed).
func RunLauncher(args []string) error {
	syscall.CloseOnExec(statusFD) // exec 성공 시 상태 파이프 자동 닫힘 → 부모는 EOF를 관측
	if err := runLauncher(args); err != nil {
		_, _ = syscall.Write(statusFD, []byte{1}) // setup 실패 신호(fd 미개방이면 무시)
		return err
	}
	return nil // syscall.Exec 성공 시 도달하지 않음
}

func runLauncher(args []string) error {
	if len(args) < 2 || args[1] != "--" {
		return fmt.Errorf("exec-launcher: 잘못된 인자")
	}
	scratch, argv := args[0], args[2:]
	if err := applyLauncherRestriction(scratch); err != nil {
		// landlock 오류에 스크래치 절대경로가 섞여 부모 Result.Stderr로 새지 않게 가린다(#6).
		return fmt.Errorf("exec-launcher: 제한 적용 실패: %s", strings.ReplaceAll(err.Error(), scratch, "<scratch>"))
	}
	if len(argv) == 0 {
		return fmt.Errorf("exec-launcher: 빈 argv")
	}
	return syscall.Exec(argv[0], argv, os.Environ()) // 성공 시 반환 없음
}
