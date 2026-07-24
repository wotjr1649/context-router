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

// applyLauncherRestriction: 쓰기=scratch·읽기=전역. /dev/null 쓰기 허용(셸 리다이렉트 등
// 일상 유틸 경로). scratch에는 REFER를 부여해 스크래치 내부 rename/link(빌드·캐시·원자적
// 쓰기 패턴)를 허용한다.
//
// ABI 게이트(#3 fix round 2): WithRefer는 go-landlock BestEffort에서 landlock ABI-1
// 커널(5.13–5.18, 예: Ubuntu 22.04/5.15)일 때 refer 룰을 다운그레이드 못 해 룰셋 전체를
// no-op으로 폐기한다(FSRule.downgrade→false ⇒ restrict.go가 v0 반환) — FS 격리가 조용히
// 사라지는 회귀다. 그래서 refer는 하드(비-BestEffort) 구성으로 시도한다: 하드 구성은 커널
// ABI 미달 시 landlock syscall 전에 compatibleWithABI 게이트가 error만 반환하고 아무 제한도
// 걸지 않으므로, 실패를 안전한 분기점으로 쓸 수 있다.
//   - ABI 2+ 커널: 최고 ABI부터 하드 시도 → 첫 성공이 이 커널의 최대 FS 격리 수준(+refer).
//     상한은 V5(라운드 1 천장 유지 — V6+ RestrictPaths는 FS 동일, V9만 유닉스소켓 권한을
//     추가로 걸어 러너 동작을 바꿀 수 있어 제외).
//   - ABI 1 커널: 위 하드 시도가 모두 실패 → refer 없는 BestEffort로 격리 유지(폐기 회피).
//   - landlock 미지원 커널: BestEffort no-op(정상 적용 경로 — ErrSetup 아님).
func applyLauncherRestriction(scratch string) error {
	referRules := []landlock.Rule{
		landlock.RWDirs(scratch).WithRefer(),
		landlock.RODirs("/"),
		landlock.RWFiles("/dev/null"),
	}
	for _, cfg := range []landlock.Config{landlock.V5, landlock.V4, landlock.V3, landlock.V2} {
		if err := cfg.RestrictPaths(referRules...); err == nil {
			return nil
		}
	}
	return landlock.V5.BestEffort().RestrictPaths(
		landlock.RWDirs(scratch),
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
