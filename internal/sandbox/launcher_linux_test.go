//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWrapArgvLinux: 런처 재실행 argv 형태 단위 검증 — `selfExe __exec-launcher <scratch> -- <argv...>`.
func TestWrapArgvLinux(t *testing.T) {
	got := wrapArgv("/scratch", []string{"/bin/echo", "hi"}, "/self/ctr")
	want := []string{"/self/ctr", "__exec-launcher", "/scratch", "--", "/bin/echo", "hi"}
	if len(got) != len(want) {
		t.Fatalf("argv len=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

// landlockSupported: 커널 LSM 목록에 landlock이 있는지(/sys/kernel/security/lsm).
func landlockSupported() bool {
	b, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return false
	}
	for _, name := range strings.Split(strings.TrimSpace(string(b)), ",") {
		if name == "landlock" {
			return true
		}
	}
	return false
}

// TestLandlockRestrictsWrites: 스펙 §2 실증 — 스크래치 밖 경로 쓰기가 실패한다. 커널 미지원이면
// BestEffort 통과가 정상이므로 t.Skip(스펙 §2 문면). ubuntu-latest는 지원 커널이라 CI에서 실경로.
func TestLandlockRestrictsWrites(t *testing.T) {
	if !landlockSupported() {
		t.Skip("커널 landlock 미지원 — BestEffort 통과(스펙 §2)")
	}
	scratch := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	s := Spec{
		Argv: []string{"/bin/sh", "-c", "echo x > " + outside},
		Dir:  scratch, Env: BaseEnv(), SelfExe: testSelfExe(t),
		Timeout: 30 * time.Second, StdoutCap: 32768, StderrCap: 8192,
	}
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.ExitCode == 0 {
		t.Fatalf("스크래치 밖 쓰기가 성공함 — 제한 미적용 (exit=%d)", r.ExitCode)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("스크래치 밖 파일이 생성됨 — 제한 미적용")
	}
}
