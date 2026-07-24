// Package sandbox — exec 3종의 OS 격리 실행 기반(설계 v0.11 D59).
// 자기정의 인터페이스 0: Run/Probe는 per-OS 파일(build tag)이 동일 시그니처로 구현.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

	// Windows Job Object 캡(D59, 0=기본값). unix 구현은 무시한다.
	MemLimitBytes uint64 // 잡 메모리 상한(0=4GiB)
	ProcLimit     uint32 // 활성 프로세스 상한(0=64)
}

type Result struct {
	Stdout, Stderr           []byte
	StdoutTrunc, StderrTrunc bool
	ExitCode                 int // TimedOut=true면 -1(무의미 — MCP 계층이 null로 변환)
	TimedOut                 bool
	Duration                 time.Duration
}

// scratchPrefix: NewScratch가 만드는 스크래치 이름 접두 — SweepStale은 이 접두 항목만
// 스윕 대상으로 삼아 공유 OS temp 하위 타 항목을 건드리지 않는다.
const scratchPrefix = "run-"

// NewScratch: 실행별 고유 스크래치(0700). 이름 충돌은 MkdirTemp가 회피.
func NewScratch(parent string) (string, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("%w: 스크래치 부모 생성 실패", ErrSetup)
	}
	dir, err := os.MkdirTemp(parent, scratchPrefix)
	if err != nil {
		return "", fmt.Errorf("%w: 스크래치 생성 실패", ErrSetup)
	}
	return dir, nil
}

// SweepStale: 기동 시 24h+ 스테일 스윕(D61 — best-effort, 실패 무시).
func SweepStale(parent string, ttl time.Duration) {
	// 부모가 실제 디렉터리가 아니면(심링크/정션 등 리파스 포인트) 스윕 생략 — 공유 OS
	// temp 하위라 링크를 따라가 표적 밖을 지우지 않게(Lstat은 링크 자신을 보고, IsDir는
	// 리파스 포인트에 대해 false).
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() {
		return
	}
	ents, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cut := time.Now().Add(-ttl)
	for _, e := range ents {
		// NewScratch가 만든 접두 항목만 대상 — 타 항목은 스윕 밖.
		if !e.IsDir() || !strings.HasPrefix(e.Name(), scratchPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
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

// BaseEnv: 닫힌 표에 있는 변수만 현재 프로세스에서 복사(값 존재분만). 표 밖 변수가
// 하나도 안 잡혀도 nil이 아닌 빈 슬라이스를 반환한다 — nil Env는 exec.Cmd에서 부모
// 환경 전체 상속을 뜻해 닫힌 표를 무력화하므로.
func BaseEnv() []string {
	keys := baseKeys()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}
