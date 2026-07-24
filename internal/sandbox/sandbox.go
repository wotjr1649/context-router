// Package sandbox — exec 3종의 OS 격리 실행 기반(설계 v0.11 D59).
// 자기정의 인터페이스 0: Run/Probe는 per-OS 파일(build tag)이 동일 시그니처로 구현.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ErrSetup — Job/landlock/스크래치 등 격리 준비 실패 sentinel(fail-closed, D61:
// 사용자 코드 실패와 구분되어 mcp toToolError에서 SANDBOX_UNAVAILABLE로 매핑).
var ErrSetup = errors.New("sandbox: 격리 준비 실패")

type Spec struct {
	Argv      []string      // argv[0]은 LookPath 완료된 실행 파일
	Dir       string        // cwd = 스크래치(D58)
	Env       []string      // 완성 env(BaseEnv+러너 재지정 — internal/exec가 조립)
	Timeout   time.Duration // 호출 계층에서 클램프 완료(120s 기본·1800s 상한)
	StdoutCap int           // 32768 (D61)
	StderrCap int           // 8192 (D61)
}

type Result struct {
	Stdout, Stderr           []byte
	StdoutTrunc, StderrTrunc bool
	ExitCode                 int // TimedOut=true면 -1(무의미 — MCP 계층이 null로 변환)
	TimedOut                 bool
	Duration                 time.Duration
}

// NewScratch: 실행별 고유 스크래치(0700). 이름 충돌은 MkdirTemp가 회피.
func NewScratch(parent string) (string, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("%w: 스크래치 부모 생성 실패", ErrSetup)
	}
	dir, err := os.MkdirTemp(parent, "run-")
	if err != nil {
		return "", fmt.Errorf("%w: 스크래치 생성 실패", ErrSetup)
	}
	return dir, nil
}

// SweepStale: 기동 시 24h+ 스테일 스윕(D61 — best-effort, 실패 무시).
func SweepStale(parent string, ttl time.Duration) {
	ents, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cut := time.Now().Add(-ttl)
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		if info.ModTime().Before(cut) {
			os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
}

// baseKeys: D59 env allowlist 닫힌 표 — 열거 외 전부 차단.
func baseKeys() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"PATH", "PATHEXT", "COMSPEC", "SystemRoot", "SystemDrive",
			"TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
			"LOCALAPPDATA", "APPDATA", "ProgramFiles", "ProgramData", "PSModulePath",
		}
	}
	return []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}
}

// BaseEnv: 닫힌 표에 있는 변수만 현재 프로세스에서 복사(값 존재분만).
func BaseEnv() []string {
	var out []string
	for _, k := range baseKeys() {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}
