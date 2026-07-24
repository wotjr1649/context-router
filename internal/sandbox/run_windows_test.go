//go:build windows

package sandbox

import (
	"context"
	"testing"
	"time"
)

func TestRunWindowsEchoAndExit(t *testing.T) {
	dir := t.TempDir()
	s := Spec{
		Argv:      []string{"cmd.exe", "/c", "echo hi& exit 3"},
		Dir:       dir,
		Env:       BaseEnv(),
		Timeout:   30 * time.Second,
		StdoutCap: 32768, StderrCap: 8192,
	}
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.ExitCode != 3 {
		t.Fatalf("exit=%d want 3", r.ExitCode)
	}
	if got := string(r.Stdout); got == "" || got[:2] != "hi" {
		t.Fatalf("stdout=%q", got)
	}
}

func TestRunWindowsTimeoutKillsTree(t *testing.T) {
	dir := t.TempDir()
	s := Spec{
		Argv:      []string{"cmd.exe", "/c", "ping -n 30 127.0.0.1 >NUL"},
		Dir:       dir,
		Env:       BaseEnv(),
		Timeout:   1 * time.Second,
		StdoutCap: 32768, StderrCap: 8192,
	}
	start := time.Now()
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !r.TimedOut {
		t.Fatalf("TimedOut=false")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("트리 미종료 — 지연 %v", time.Since(start))
	}
}

func TestProbeWindows(t *testing.T) {
	if err := Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

// TestRunWindowsMemLimitCap: 스펙 §2 캡 실증 — 작은 MemLimitBytes(256MB)를 주입하면
// 러너 시작은 되지만 대량 할당(1.5GB)이 Job 메모리 캡에 막혀 비정상 종료(비 0 exit)한다.
// 캡 미적용이면 할당이 성공해 exit 0 → 이 테스트가 실패한다.
func TestRunWindowsMemLimitCap(t *testing.T) {
	dir := t.TempDir()
	s := Spec{
		Argv: []string{
			"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"try { $a = New-Object byte[] 1500000000; [void]$a.Length } catch { exit 42 }",
		},
		Dir:           dir,
		Env:           BaseEnv(),
		Timeout:       30 * time.Second,
		StdoutCap:     32768,
		StderrCap:     8192,
		MemLimitBytes: 256 << 20, // PS 시작에는 충분, 1.5GB 할당은 초과 → 실패
	}
	r, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.TimedOut {
		t.Fatalf("예기치 않은 타임아웃 — 캡 실증 불가")
	}
	if r.ExitCode == 0 {
		t.Fatalf("메모리 캡 미적용 — exit=0 (대량 할당이 성공함)")
	}
}
