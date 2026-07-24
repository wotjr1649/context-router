//go:build !windows && !linux && !darwin

package sandbox

import "fmt"

// wrapArgv: FS 제한 미지원 OS — BestEffort 통과(제한 없이 그대로).
func wrapArgv(_ string, argv []string, _ string) []string { return argv }

// RunLauncher: 런처 재실행 경로 미사용.
func RunLauncher([]string) error { return fmt.Errorf("exec-launcher: 미지원 OS") }
