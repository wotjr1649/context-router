// Package exec — ctr_execute/execute_file의 러너 테이블(설계 v0.11 D60).
// sandbox 위에서 언어별 toolchain을 감지·실행한다. 내장 인터프리터 없음.
package exec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wotjr1649/context-router/internal/sandbox"
)

var (
	ErrUnsupportedLang  = errors.New("exec: 지원하지 않는 언어")
	ErrToolchainMissing = errors.New("exec: toolchain 미설치")
	ErrVersionGate      = errors.New("exec: toolchain 버전 미달")
	ErrInvalidPath      = errors.New("exec: 잘못된 파일 경로")
)

const (
	defaultTimeout = 120_000
	maxTimeout     = 1_800_000
	stdoutCap      = 32768
	stderrCap      = 8192
)

type Request struct {
	Language, Code, FilePath string
	WorktreeRoot             string // CTR_WORKTREE로 노출(D58 — cwd는 스크래치, 워크트리 접근은 env로)
	TimeoutMS                int
}

type Response struct {
	Stdout, Stderr string
	ExitCode       *int // timed_out 시 nil (D61)
	TimedOut       bool
	StdoutTrunc    bool
	StderrTrunc    bool
	DurationMS     int64
	Runner         string
}

func clampTimeout(ms int) time.Duration {
	if ms <= 0 {
		ms = defaultTimeout
	} else if ms > maxTimeout {
		ms = maxTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// runner: 한 언어의 실행 계약.
type runner struct {
	file   string // 스니펫 파일명
	detect func() (bin, version string, err error)
	argv   func(bin, file string) []string
	// extra: 러너별 재지정/강제 env(nil 가능). env 값이 가리키는 스크래치 하위 준비(파일
	// 기록·디렉터리 생성)를 함께 하므로 오류를 반환한다 — 격리 준비 실패는 fail-closed다
	// (설계 정정: 이전의 "전부 best-effort" 서술을 대체한다. 경고만 남기고 계속하면 실재하지
	// 않는 경로를 가리키는 env로 실행되고, 그 상태가 변수를 주지 않은 것보다 나쁜 지점이
	// 있다 — pip은 PIP_CONFIG_FILE이 실재할 때만 사용자 구성을 빼기 때문이다).
	//
	// 지점별 분류 기준은 하나다: **부재가 호스트 무관 기본으로 안전하게 퇴화하는가.**
	//   · 시행점 — 부재가 호스트 사용자 구성을 다시 들이거나(pip.conf·nuget.config), 소비자가
	//     실재를 요구해 실행이 깨진다(psmodules·GOTMPDIR·Application Support·NuGet sentinel).
	//     실패 시 sandbox.ErrSetup을 감아 반환하고 env는 nil로 둔다 — 격리가 빠진 env를
	//     호출부에 넘기지 않는다.
	//   · 이중 안전장치 — 부재가 도구 내장 기본으로 퇴화한다(npmrc·스크래치 홈·pycache·
	//     go-env). 경고만 남기고 계속하며, 왜 benign인지 각 지점에 적어 둔다.
	extra func(scratch string) ([]string, error)
}

func shellName() string {
	if runtime.GOOS == "windows" {
		return "pwsh"
	}
	return "sh"
}

// lookVersioned: bins를 우선순위로 찾는다(버전 게이트가 없는 언어용 — 첫 발견 반환).
func lookVersioned(bins []string) (string, error) {
	for _, b := range bins {
		if p, err := exec.LookPath(b); err == nil {
			return p, nil
		}
	}
	return "", ErrToolchainMissing
}

func table() map[string]runner {
	return map[string]runner{
		"shell":      shellRunner(),
		"javascript": {file: "snippet.js", detect: detectJS, argv: fileArgv, extra: jsEnv},
		"typescript": tsRunner(),
		"python":     {file: "snippet.py", detect: detectPy, argv: fileArgv, extra: pyEnv},
		"go":         {file: "snippet.go", detect: detectGo, argv: goArgv, extra: goEnv},
		"csharp":     {file: "snippet.cs", detect: detectCS, argv: csArgv, extra: csEnv},
	}
}

func Run(ctx context.Context, scratchParent, selfExe string, req Request) (Response, error) {
	r, ok := table()[req.Language]
	if !ok {
		return Response{}, ErrUnsupportedLang // I3: 입력 원문 에코 금지(오류 규칙·로그 인젝션)
	}
	bin, version, err := r.detect()
	if err != nil {
		return Response{}, err // ErrToolchainMissing / ErrVersionGate
	}
	scratch, err := sandbox.NewScratch(scratchParent)
	if err != nil {
		return Response{}, err // ErrSetup
	}
	defer func() { // D61: 삭제 실패는 warning — 회수는 기동 시 SweepStale
		if rmErr := os.RemoveAll(scratch); rmErr != nil {
			slog.Warn("exec: 스크래치 삭제 실패 — 기동 스윕이 회수", "error", rmErr)
		}
	}()

	env := append(sandbox.BaseEnv(), tmpEnv(scratch)...) // 공통 temp 재지정(중복 키는 마지막 값 승리)
	if r.extra != nil {
		ex, err := r.extra(scratch)
		if err != nil {
			return Response{}, err // ErrSetup — 격리 시행점 준비 실패는 실행 없이 반환(fail-closed)
		}
		env = append(env, ex...)
	}
	// execute_file: 파일 경로를 CTR_FILE로 노출(스니펫이 읽음)
	if req.FilePath != "" {
		env = append(env, "CTR_FILE="+req.FilePath)
	}
	if req.WorktreeRoot != "" {
		env = append(env, "CTR_WORKTREE="+req.WorktreeRoot) // D58·검수 ⑩
	}
	env = append(env, "CTR_SCRATCH="+scratch)

	file := filepath.Join(scratch, r.file)
	if err := os.WriteFile(file, snippetContent(r.file, req.Code), 0o600); err != nil {
		return Response{}, fmt.Errorf("%w: 스니펫 기록 실패", sandbox.ErrSetup)
	}
	spec := sandbox.Spec{
		Argv: r.argv(bin, file), Dir: scratch, Env: env, SelfExe: selfExe,
		Timeout: clampTimeout(req.TimeoutMS), StdoutCap: stdoutCap, StderrCap: stderrCap,
	}
	res, err := sandbox.Run(ctx, spec)
	if err != nil {
		return Response{}, err // ErrSetup 계열 또는 pre-Start ctx 오류
	}
	resp := Response{
		Stdout: string(res.Stdout), Stderr: string(res.Stderr),
		StdoutTrunc: res.StdoutTrunc, StderrTrunc: res.StderrTrunc,
		DurationMS: res.Duration.Milliseconds(), Runner: runnerLabel(req.Language, bin, version),
	}
	// 취소/타임아웃 판정: res.TimedOut 단독으로 부족하다 — unix는 인플라이트 부모 deadline을
	// ctx.Done() 경로로 처리해 TimedOut=false·ExitCode=-1로 반환하는 알려진 미세 불일치가
	// 있다(태스크 계약). ctx.Err()를 함께 봐서 살해된 프로세스의 -1을 정상 종료로 오분류하지
	// 않는다.
	switch {
	case res.TimedOut || errors.Is(ctx.Err(), context.DeadlineExceeded):
		resp.TimedOut = true // ExitCode는 nil로 남긴다(D61)
	case errors.Is(ctx.Err(), context.Canceled):
		return Response{}, ctx.Err() // 부모 취소 — 오류로 전파(정상 결과로 위장 금지)
	default:
		ec := res.ExitCode
		resp.ExitCode = &ec
	}
	return resp, nil
}

func runnerLabel(lang, bin, version string) string {
	name := filepath.Base(bin)
	if version != "" {
		return name + " " + version
	}
	return name
}

func fileArgv(bin, file string) []string { return []string{bin, file} }
func goArgv(bin, file string) []string   { return []string{bin, "run", file} }
func csArgv(bin, file string) []string   { return []string{bin, "run", file} }

// snippetContent: 스니펫 파일에 기록할 바이트. .ps1은 UTF-8 BOM을 선두에 붙인다(I2) —
// powershell.exe 5.1이 BOM 없는 .ps1을 시스템 ANSI 코드페이지로 디코딩해 비ASCII를 손상시키기
// 때문(한국어 Windows에서 관측). pwsh 7은 BOM을 허용하므로 fallback 양쪽 모두 안전하다.
func snippetContent(fileName, code string) []byte {
	if strings.HasSuffix(fileName, ".ps1") {
		return append([]byte{0xEF, 0xBB, 0xBF}, code...)
	}
	return []byte(code)
}

func shellRunner() runner {
	if runtime.GOOS == "windows" {
		var psHome, pfDir string // detect가 채우고 extra가 읽는다(Run: detect 115행 → extra 131행)
		return runner{
			file: "snippet.ps1",
			detect: func() (string, string, error) {
				if p, err := exec.LookPath("pwsh"); err == nil {
					psHome, pfDir = filepath.Dir(p), "PowerShell"
					return p, "7", nil
				}
				if p, err := exec.LookPath("powershell"); err == nil {
					psHome, pfDir = filepath.Dir(p), "WindowsPowerShell"
					return p, "5.1", nil // runner 필드로 5.1 가시화 (D60)
				}
				return "", "", ErrToolchainMissing
			},
			argv: func(bin, file string) []string {
				return []string{bin, "-NoProfile", "-NonInteractive", "-File", file}
			},
			// D65: 모듈 경로를 스크래치 빈 디렉터리 + 인터프리터 설치본 + 머신 전역으로 고정한다
			// (값·순서와 스크래치가 첫 항목인 이유는 psModulePath).
			// PSModulePath를 명시해도 pwsh는 사용자 모듈 디렉터리가 실재하면 USERPROFILE 유도 경로를
			// 앞에 덧붙이므로 USERPROFILE도 스크래치로 돌린다 — 호스트 사용자 모듈을 실제로 떼어내는
			// 레버는 이쪽이다(실측: TestRunShellScratchModulePathWins의 격리 없는 절반이 그 경로로 로드).
			// extra는 BaseEnv 뒤에 붙어 마지막 값이 이기므로(exec.go:129-136) 이 재지정은 shell
			// 러너에만 적용된다 — csharp의 USERPROFILE(D60)은 그대로다.
			// psHome·pfDir 공유는 안전하다: table()이 호출마다 새 클로저 쌍을 만들어(exec.go:99) 동시
			// Run끼리 변수를 공유하지 않고, extra를 부르는 유일한 경로인 Run이 detect 성공 뒤에만
			// 도달한다(detect 오류는 117행에서 조기 반환). 표에서 직접 extra를 부르는 코드는
			// detect를 먼저 불러야 한다.
			extra: func(scratch string) ([]string, error) {
				mods := filepath.Join(scratch, "psmodules")
				// 시행점: 첫 항목이 **실재**해야 5.1 프로바이더 cmdlet 영구 정지를 피한다(아래
				// psModulePath 주석의 실측 — 정지를 푸는 조건이 "첫 항목에 실재하는 경로"였다).
				// 부재 경로를 첫 항목으로 주면 그 정지가 그대로 돌아오고, 정지는 오류도 아니라
				// 타임아웃까지 매달린다 — 준비가 깨졌으면 실행하지 않는 쪽이 계약이다.
				if err := os.MkdirAll(mods, 0o700); err != nil {
					slog.Error("exec: 스크래치 모듈 디렉터리 생성 실패", "error", err) // 경로는 로그에만
					return nil, fmt.Errorf("%w: 모듈 디렉터리 생성 실패", sandbox.ErrSetup)
				}
				return []string{
					"PSModulePath=" + psModulePath(mods, psHome, pfDir),
					"USERPROFILE=" + scratch,
				}, nil
			},
		}
	}
	return runner{
		file: "snippet.sh",
		detect: func() (string, string, error) {
			p, err := exec.LookPath("sh")
			if err != nil {
				return "", "", ErrToolchainMissing
			}
			return p, "", nil
		},
		argv: func(bin, file string) []string { return []string{bin, file} },
	}
}

// psModulePath: shell 러너가 주입할 PSModulePath 값 — 세 항목, 이 순서다. 호스트
// PSModulePath는 읽지 않는다.
//
//  1. scratchMods — 스크래치 하위의 빈 모듈 디렉터리. PowerShell이 사용자 모듈 디렉터리를 두는
//     첫 항목 자리를 우리 것으로 채운다. 호스트 사용자 경로가 아니라 우리가 만든 빈 디렉터리라
//     자동 로드될 모듈이 없다 — D65 계약(호스트 설치 모듈 상속 차단)은 그대로다.
//  2. <PSHOME>\Modules — 인터프리터 설치본.
//  3. <ProgramFiles>\<pfDir>\Modules — 머신 전역(pfDir은 버전별로 PowerShell(7) /
//     WindowsPowerShell(5.1)). 사용자 구성이 아니므로 D65가 닫은 대상이 아니다.
//
// 첫 항목이 스크래치인 이유는 CI(windows-latest) 5.1의 프로바이더 cmdlet 영구 정지다(실측):
// Set-Content가 예외도 오류 레코드도 없이 돌아오지 않고, 파이프라인과 [IO.File]::WriteAllText는
// 정상 반환한다. 상속 PSModulePath를 항목 단위로 이분탐색한 결과 정지를 푸는 항목은 단 하나 —
// 호스트 **사용자** 모듈 경로(<USERPROFILE>\Documents\PowerShell\Modules)였다(그 항목이 있으면
// exit 0·1.32s, 없으면 값이 더 넓어도 정지). 그 경로를 주입하는 교정은 D65가 금지하므로 "첫 항목에
// 경로가 있어야 한다"는 요건만 스크래치의 빈 디렉터리로 만족시킨다. 이 교정으로 3-OS CI가 초록이
// 됐고, 재발 판별 계측은 internal/sandbox/run_windows_test.go의 TestRunWindowsTimeoutKillsTree가
// 실패 경로에서 낸다(읽는 법은 그 파일 probeEnvVariants 주석).
//
// ProgramFiles가 비면 그 항목을 빼고 준다 — 빈 값으로 조립한 상대 경로 항목은 자식의 cwd
// (스크래치) 기준으로 해석되므로 넣지 않는다.
func psModulePath(scratchMods, psHome, pfDir string) string {
	out := []string{scratchMods, filepath.Join(psHome, "Modules")}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		out = append(out, filepath.Join(pf, pfDir, "Modules"))
	}
	return strings.Join(out, ";")
}

func detectJS() (string, string, error) {
	p, err := lookVersioned([]string{"bun", "node"})
	return p, "", err
}

// jsEnv: node/bun의 사용자 레벨 구성을 스크래치로 가둔다(D65). 두 갈래다.
//
// ① npmrc — 인터프리터 자체는 npmrc를 읽지 않으므로(`node file.js`·`bun file.js`) 스니펫 실행
// 에는 반영 지점이 없지만, 스니펫이 npm·bun install을 호출하면 호스트 사용자 구성의 레지스트리·
// 프록시·스크립트 설정이 그대로 적용된다 — env 닫힌 표가 통과시키는 USERPROFILE/HOME이
// ~/.npmrc로 가는 포인터이기 때문이다. npm과 bun 양쪽이 이 변수를 존중한다(bun 1.3.14 실측 —
// 이 변수만 바꿔도 install의 레지스트리 해석이 바뀐다). 파일 사전 생성은 격리의 시행점이 아니라
// 이중 안전장치다: 경로가 부재해도 npm은 호스트 파일로 되돌아가지 않고 내장 기본(공개
// 레지스트리)으로 간다(실측). pyEnv의 pip.conf는 반대로 파일 실재 여부가 시행점이다.
//
// ② bunfig — bun은 `$HOME/.bunfig.toml`·`$XDG_CONFIG_HOME/.bunfig.toml`을 사용자 레벨 구성으로
// 읽는다(bun 문서). 그중 preload는 스니펫보다 먼저 실행되므로 이 상속은 사용자 코드 앞에 임의
// 모듈이 끼어드는 경로이고, detectJS가 bun을 먼저 고르니 러너의 기본 경로에서 열려 있다.
// 스크래치 로컬 bunfig로 덮는 방법은 불완전하다: 얕은 병합은 열거한 키만 덮고, 전역 preload를
// 지우려는 `preload = []`는 bun이 구성 오류로 거부한다("Expected preload to be an array" —
// 1.3.14 실측). 그래서 키에 무관한 레버인 홈 포인터 자체를 스크래치로 돌린다(csEnv 선례).
// 검증 한계를 밝혀 둔다: windows 1.3.14에서는 어떤 홈 포인터(HOME·XDG_CONFIG_HOME·USERPROFILE·
// HOMEDRIVE+HOMEPATH)로도 전역 bunfig가 적용되지 않아(실측) 이 갈래가 닫는 것은 문서상 unix
// 경로다. CI는 이제 3-OS 전부에 bun 1.3.14를 설치하므로(ci.yml) 그 unix 경로에서 격리 단정 절반이
// ubuntu·macos에서 처음 실행되고, windows는 픽스처 무효를 Skip 문면으로 남긴다
// (TestRunJSScratchBunfigWins). USERPROFILE은 돌리지 않는다 — windows에서
// 효과가 없고(실측) node/npm 쪽 부작용만 크다.
func jsEnv(scratch string) ([]string, error) {
	rc := filepath.Join(scratch, "npmrc")
	// benign(이중 안전장치): 레버는 NPM_CONFIG_USERCONFIG 값 자체다 — 경로가 부재해도 npm은
	// 호스트 사용자 파일로 되돌아가지 않고 내장 기본으로 간다(위 ① 실측). 부재가 호스트 무관
	// 기본으로 퇴화하므로 시행점이 아니다.
	if err := os.WriteFile(rc, nil, 0o600); err != nil {
		slog.Warn("exec: npmrc 기록 실패 — 격리는 유지(호스트 사용자 구성 미적용), npm 내장 기본으로 실행", "error", err)
	}
	home := filepath.Join(scratch, "home")
	xdg := filepath.Join(home, ".config") // XDG 기본 배치 — MkdirAll이 home까지 만든다
	// benign(이중 안전장치): bunfig 차단의 레버도 홈 포인터 **값**이다(위 ②) — 디렉터리가 없으면
	// 자식이 찾을 사용자 bunfig가 없는 것이고, 호스트 홈으로 되돌아가지도 않는다(부재가 곧
	// 격리된 결과다). 인터프리터 실행 자체는 이 디렉터리를 요구하지 않고, install 계열이 쓸 때는
	// 자신이 만든다.
	if err := os.MkdirAll(xdg, 0o700); err != nil {
		slog.Warn("exec: 스크래치 홈 생성 실패 — 격리는 유지(홈 포인터가 호스트 사용자 구성 밖을 가리킨다)", "error", err)
	}
	return []string{
		"NPM_CONFIG_USERCONFIG=" + rc,
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + xdg,
	}, nil
}

func tsRunner() runner {
	return runner{
		file:   "snippet.ts",
		detect: detectTS,
		extra:  jsEnv, // javascript와 같은 러너(node/bun)라 사용자 구성 격리도 같다(D65)
		argv: func(bin, file string) []string {
			if strings.HasPrefix(filepath.Base(bin), "bun") {
				return []string{bin, file}
			}
			return []string{bin, "--experimental-transform-types", file}
		},
	}
}

// 버전 게이트는 프로세스 실행(node --version / dotnet --list-sdks)이 필요하다. 프로브가
// 성공해 확정한 결과(통과/버전미달)만 서버 수명 캐시하고(검토 반영 — 매 호출 재실행 금지),
// 프로브 자체 실패(타임아웃/exec 오류)는 캐시하지 않아 다음 호출에서 재프로브한다(I1 —
// sync.Once는 일시 실패도 영구 고정해 부적합). LookPath는 매 호출 유지.
// ponytail: 서버 실행 중 toolchain이 교체되는 경우는 재기동으로 갱신(D60 문면 — 서버 수명 캐시).
const gateProbeTimeout = 5 * time.Second

var (
	nodeGateState   gate
	dotnetGateState gate
)

// gate: 성공 프로브가 확정한 결과만 캐시하는 잠금(sync.Once 대체).
type gate struct {
	mu     sync.Mutex
	cached bool
	err    error
}

// do: probe가 (definitive, err)를 반환한다 — definitive=false(프로브 자체 실패)면 캐시하지
// 않고 그대로 반환해 다음 호출에서 재시도한다.
func (g *gate) do(probe func() (bool, error)) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cached {
		return g.err
	}
	definitive, err := probe()
	if definitive {
		g.cached, g.err = true, err
	}
	return err
}

// probeVersion: 버전 프로브에 고정 시한을 걸어 웜지 않은 toolchain이 exec·doctor를 무기한
// 블록하지 못하게 한다(I1). ok=false는 프로브 자체 실패(타임아웃/exec 오류) — 캐시 금지.
func probeVersion(bin string, timeout time.Duration, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	// WaitDelay: ctx 취소 후 자손 프로세스(예: dash가 fork한 sleep)가 stdout 파이프를 계속
	// 점유해도 1s 안에 파이프를 강제로 닫아 유한시간에 반환한다(프로브 전용 — sandbox 5s와 별개).
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// detectTS: bun 우선, 없으면 node ≥22.7 게이트.
func detectTS() (string, string, error) {
	if p, err := exec.LookPath("bun"); err == nil {
		return p, "", nil
	}
	p, err := exec.LookPath("node")
	if err != nil {
		return "", "", ErrToolchainMissing
	}
	if err := nodeGate(p); err != nil {
		return "", "", err
	}
	return p, "", nil
}

func nodeGate(node string) error {
	return nodeGateState.do(func() (bool, error) {
		out, ok := probeVersion(node, gateProbeTimeout, "--version") // "v22.7.0\n"
		if !ok {
			return false, ErrToolchainMissing // transient — 캐시 금지
		}
		if !nodeAtLeast(strings.TrimSpace(out), 22, 7) {
			return true, fmt.Errorf("%w: node ≥22.7 필요", ErrVersionGate)
		}
		return true, nil
	})
}

// nodeAtLeast: "vMAJOR.MINOR.PATCH"가 (major,minor) 이상인가.
func nodeAtLeast(v string, major, minor int) bool {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, _ := strconv.Atoi(parts[0])
	min, _ := strconv.Atoi(parts[1])
	if maj != major {
		return maj > major
	}
	return min >= minor
}

func detectPy() (string, string, error) {
	p, err := lookVersioned([]string{"python3", "python"})
	return p, "", err
}

// pyEnv: 바이트코드 캐시를 스크래치로 돌리고(기존), user site-packages와 pip 사용자 구성을
// 차단한다(D65 — env 닫힌 표가 통과시키는 APPDATA/HOME이 그 두 경로로 가는 포인터다).
// pip.conf 사전 생성은 jsEnv의 npmrc와 달리 이중 안전장치가 아니라 격리의 시행점이다: pip은
// PIP_CONFIG_FILE 경로가 실재할 때만 사용자 구성을 로드 목록에서 뺀다(pip 내부
// iter_config_files의 os.path.exists 조건 — 부재 경로면 호스트 사용자 구성이 그대로 로드되는
// 것을 실측). 머신 전역 구성(/etc/pip.conf · %ProgramData%\pip\pip.ini)은 이 레버 밖이며,
// 여기서 끊는 것은 사용자 구성 상속이다.
func pyEnv(scratch string) ([]string, error) {
	cache := filepath.Join(scratch, "pycache")
	// benign(캐시 재지정, 격리 레버 아님): CPython이 .pyc를 쓰는 시점에 상위 디렉터리를 스스로
	// 만들고 그마저 실패하면 바이트코드 기록만 조용히 건너뛴다 — 부재가 호스트 경로로 새지 않는다.
	if err := os.MkdirAll(cache, 0o700); err != nil {
		slog.Warn("exec: 바이트코드 캐시 디렉터리 생성 실패 — 바이트코드 기록만 생략된다", "error", err)
	}
	conf := filepath.Join(scratch, "pip.conf")
	// 시행점: pip은 PIP_CONFIG_FILE 경로가 **실재**할 때만 사용자 구성을 로드 목록에서 뺀다(위
	// 주석의 실측). 조용히 넘기면 호스트 pip 사용자 구성이 그대로 로드된 채 실행되므로, 변수를
	// 주지 않은 것보다 나쁜 상태다 — 실행하지 않는다.
	if err := os.WriteFile(conf, nil, 0o600); err != nil {
		slog.Error("exec: pip 구성 기록 실패", "error", err) // 경로는 로그에만
		return nil, fmt.Errorf("%w: pip 구성 기록 실패", sandbox.ErrSetup)
	}
	return []string{
		"PYTHONPYCACHEPREFIX=" + cache,
		"PYTHONNOUSERSITE=1",
		"PIP_CONFIG_FILE=" + conf,
	}, nil
}

func detectGo() (string, string, error) {
	p, err := exec.LookPath("go")
	if err != nil {
		return "", "", ErrToolchainMissing
	}
	return p, "", nil
}

// goEnv: 빌드 캐시·모듈·임시를 스크래치 하위로 재지정하고 그 디렉터리를 생성한다.
// GOTMPDIR은 go가 스스로 만들지 않으므로(내부적으로 그 안에 임시 하위만 생성) 사전 생성이
// 필수다 — 없으면 `go run`이 work dir 생성에 실패한다.
// GOENV는 호스트 go env 파일(os.UserConfigDir()/go/env)이 GOFLAGS·GOPROXY·GOTOOLCHAIN을
// 샌드박스 빌드에 공급하는 경로를 끊는다(D65) — env allowlist가 통과시키는 APPDATA/HOME이
// 그 파일로 가는 포인터이기 때문이다. go는 이 파일을 만들지 않으므로 사전 생성한다. 빈 파일은
// "설정 없음"이 아니라 go 내장 기본(공개 모듈 프록시 + 툴체인 자동 다운로드)이므로 의도한
// 값을 적는다: GOFLAGS 없음 · 툴체인은 설치본 고정 · 모듈 다운로드 없음. 스니펫은 단일 파일
// stdlib 실행이라 이 값들로 정상 동작한다(GOPATH가 스크래치라 어차피 모듈 캐시가 비어 있다).
const goEnvFile = "GOFLAGS=\nGOTOOLCHAIN=local\nGOPROXY=off\n"

func goEnv(scratch string) ([]string, error) {
	build := filepath.Join(scratch, "go-build")
	gopath := filepath.Join(scratch, "go")
	gotmp := filepath.Join(scratch, "go-tmp")
	// 시행점(gotmp): go는 GOTMPDIR를 스스로 만들지 않아 부재면 `go run`이 work dir 생성에서
	// 죽는다(위 주석의 사전 생성 필수 근거) — 사용자 코드 오류로 오분류될 실패다. build·gopath는
	// go가 없으면 스스로 만드는 쪽이지만 같은 루프에서 함께 fail-closed로 둔다: 스크래치 하위
	// 생성이 막힌 상황이면 go의 자체 생성도 같은 이유로 실패하므로 나눠서 얻는 것이 없다.
	for _, d := range []string{build, gopath, gotmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			slog.Error("exec: go 캐시·임시 디렉터리 생성 실패", "error", err) // 경로는 로그에만
			return nil, fmt.Errorf("%w: go 디렉터리 생성 실패", sandbox.ErrSetup)
		}
	}
	goenv := filepath.Join(scratch, "go-env")
	// benign(이중 안전장치): 호스트 go env 파일을 끊는 레버는 GOENV 값 자체다 — 부재 파일이면
	// go는 호스트 파일로 되돌아가지 않고 내장 기본으로 간다. 잃는 것은 의도한 값(GOPROXY=off·
	// GOTOOLCHAIN=local)뿐이며 그 기본도 호스트 구성이 아니므로 시행점이 아니다.
	if err := os.WriteFile(goenv, []byte(goEnvFile), 0o600); err != nil {
		slog.Warn("exec: go env 파일 기록 실패 — go 내장 기본(공개 모듈 프록시·툴체인 자동 다운로드)으로 실행", "error", err)
	}
	return []string{
		"GOCACHE=" + build,
		"GOPATH=" + gopath,
		"GOTMPDIR=" + gotmp,
		"GOENV=" + goenv,
	}, nil
}

// detectCS: dotnet + SDK ≥10 게이트.
func detectCS() (string, string, error) {
	p, err := exec.LookPath("dotnet")
	if err != nil {
		return "", "", ErrToolchainMissing
	}
	if err := dotnetGate(p); err != nil {
		return "", "", err
	}
	return p, "10+", nil
}

func dotnetGate(dotnet string) error {
	return dotnetGateState.do(func() (bool, error) {
		out, ok := probeVersion(dotnet, gateProbeTimeout, "--list-sdks")
		if !ok {
			return false, ErrToolchainMissing // transient — 캐시 금지
		}
		if !dotnetHasMajor(out, 10) {
			return true, fmt.Errorf("%w: .NET SDK ≥10 필요", ErrVersionGate)
		}
		return true, nil
	})
}

// dotnetHasMajor: --list-sdks 출력에 major 이상 SDK가 하나라도 있는가.
func dotnetHasMajor(list string, major int) bool {
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		maj, _ := strconv.Atoi(strings.SplitN(line, ".", 2)[0])
		if maj >= major {
			return true
		}
	}
	return false
}

// nugetConfig: 스크래치 로컬 NuGet 구성 — 호스트 설정 상속을 끊는다.
// NUGET_* env는 캐시·패키지 "경로"만 재지정할 뿐 구성 파일 검색 경로는 격리하지 않아, 사용자
// 전역(Windows %APPDATA%\NuGet\NuGet.Config, unix ~/.nuget/NuGet/NuGet.Config)과 머신 레벨
// 구성이 그대로 병합된다. 등록된 로컬 소스 디렉터리가 없으면 복원이 NU1301로 죽고(로컬
// 도그푸딩에서 관측), config 섹션의 globalPackagesFolder는 NUGET_PACKAGES env보다 우선해
// 패키지를 스크래치 밖에 쓴다 — 격리 계약과 D61 정리 대상 밖 잔존물 양쪽에 걸린다.
// unix는 HOME 재지정이 사용자 구성을 우연히 가렸으므로 CI 3-OS가 이 경로를 보지 못했다.
// 스크래치는 cwd이자 file-based 프로젝트 디렉터리라 여기 둔 구성이 탐색 체인의 말단이 되고,
// 각 섹션의 <clear />가 상위에서 병합된 값을 전부 지운다 — 호스트와 무관하게 결정적이다.
const nugetConfig = `<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <config>
    <clear />
  </config>
  <packageSources>
    <clear />
    <add key="nuget.org" value="https://api.nuget.org/v3/index.json" protocolVersion="3" />
  </packageSources>
  <disabledPackageSources>
    <clear />
  </disabledPackageSources>
  <packageSourceMapping>
    <clear />
  </packageSourceMapping>
  <fallbackPackageFolders>
    <clear />
  </fallbackPackageFolders>
</configuration>
`

// csEnv: dotnet CLI/NuGet 홈을 스크래치 하위로 재지정(그 디렉터리 생성)하고, in-job 실행에
// 맞춰 MSBuild 서버/노드 재사용·공유 컴파일을 끈다(스크래치 수명 밖 프로세스 잔존 방지).
// NUGET_PACKAGES는 global-packages만 재지정하므로 http/plugins 캐시도 스크래치 하위로 옮긴다
// (I5 — 안 하면 HOME/LOCALAPPDATA로 이탈해 landlock write-deny로 실패하거나 D61 잔존물이 남음).
// cold DOTNET_CLI_HOME는 매 실행 first-run이라 배너/텔레메트리가 stdout을 오염시킨다 — 끈다(I4).
func csEnv(scratch string) ([]string, error) {
	home := filepath.Join(scratch, "dotnet")
	nuget := filepath.Join(scratch, "nuget")
	nugetHTTP := filepath.Join(scratch, "nuget-http")
	nugetPlugins := filepath.Join(scratch, "nuget-plugins")
	// XDG_DATA_HOME 밑 NuGet/Migrations/1 sentinel을 미리 만들어 MigrationRunner를 단락시킨다 —
	// 없으면 NuGet-Migrations named mutex가 /tmp/.dotnet(TMPDIR 무시 하드코딩, dotnet/runtime#49822)에
	// shared-memory를 열다 landlock RO /tmp에서 EACCES로 실패한다.
	xdg := filepath.Join(scratch, "xdg")
	migrations := filepath.Join(xdg, "NuGet", "Migrations")
	// macOS는 ~/Library/Application Support/dotnet(Cocoa NSApplicationSupportDirectory)를 쓰는데
	// DOTNET_CLI_HOME으로 못 옮긴다 — Foundation이 존중하는 CFFIXED_USER_HOME(+HOME)을 scratch
	// 하위로 돌려 SBPL subpath 안에 넣고, first-run 워크로드 무결성 검사(네트워크)를 스킵한다.
	userHome := filepath.Join(scratch, "home")
	// file-based dotnet run의 artifacts temp는 LocalApplicationData(macOS=<home>/Library/Application
	// Support)에서 결정되는데 NSSearchPath는 경로를 만들지 않아, 미존재 시 빈 문자열→temp 경로 결정 실패로
	// 죽는다 — env에 할당·파생한 경로는 전부 사전 생성한다(goEnv GOTMPDIR 규칙). SBPL scratch subpath 안.
	appSupport := filepath.Join(userHome, "Library", "Application Support")
	// 시행점: migrations·appSupport는 부재 자체가 실행을 깨는 쪽이고(위 두 주석의 근거 —
	// landlock RO /tmp EACCES · NSSearchPath 빈 문자열로 temp 경로 결정 실패), 나머지 넷도
	// env가 가리키는 경로라 만들 수 없는 상황에서는 dotnet/NuGet의 자체 생성도 같은 이유로
	// 실패한다. 한 루프이므로 함께 fail-closed로 둔다.
	for _, d := range []string{home, nuget, nugetHTTP, nugetPlugins, migrations, appSupport} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			slog.Error("exec: dotnet·NuGet 스크래치 디렉터리 생성 실패", "error", err) // 경로는 로그에만
			return nil, fmt.Errorf("%w: dotnet 디렉터리 생성 실패", sandbox.ErrSetup)
		}
	}
	// 시행점: sentinel이 없으면 MigrationRunner가 돌아 위 주석의 EACCES 실패 경로로 들어간다.
	if err := os.WriteFile(filepath.Join(migrations, "1"), nil, 0o600); err != nil {
		slog.Error("exec: NuGet 마이그레이션 sentinel 기록 실패", "error", err) // 경로는 로그에만
		return nil, fmt.Errorf("%w: NuGet sentinel 기록 실패", sandbox.ErrSetup)
	}
	// 시행점: 이 파일이 호스트 구성 차단의 유일한 시행점이라 조용한 실패는 격리 없이 실행되는
	// 것과 같다 — 그래서 러너 오류로 올린다(설계 정정: 이전의 best-effort 계약을 대체한다).
	if err := os.WriteFile(filepath.Join(scratch, "nuget.config"), []byte(nugetConfig), 0o600); err != nil {
		slog.Error("exec: NuGet 구성 기록 실패", "error", err) // 경로는 로그에만
		return nil, fmt.Errorf("%w: NuGet 구성 기록 실패", sandbox.ErrSetup)
	}
	return []string{
		"DOTNET_CLI_HOME=" + home,
		"NUGET_PACKAGES=" + nuget,
		"NUGET_HTTP_CACHE_PATH=" + nugetHTTP,
		"NUGET_PLUGINS_CACHE_PATH=" + nugetPlugins,
		"XDG_DATA_HOME=" + xdg,
		"HOME=" + userHome,
		"CFFIXED_USER_HOME=" + userHome,
		"DOTNET_SKIP_WORKLOAD_INTEGRITY_CHECK=1",
		"DOTNET_GENERATE_ASPNET_CERTIFICATE=false", // macOS first-run dev-cert가 SBPL 밖 로그인 키체인 접근 예방
		"DOTNET_NOLOGO=1",
		"DOTNET_CLI_TELEMETRY_OPTOUT=1",
		"DOTNET_CLI_DO_NOT_USE_MSBUILD_SERVER=1",
		"MSBUILDDISABLENODEREUSE=1",
		"UseSharedCompilation=false",
	}, nil
}

// tmpEnv: 공통 temp 재지정 — Unix는 TMPDIR, Windows는 TEMP·TMP를 스크래치 하위 tmp로
// 돌리고 그 디렉터리를 생성한다(검토 반영). dotnet file-based 앱 산출물 등 temp 기록이
// 스크래치 수명 안에 들어와 러너의 쓰기 계약과 충돌하지 않게 한다.
func tmpEnv(scratch string) []string {
	tmp := filepath.Join(scratch, "tmp")
	_ = os.MkdirAll(tmp, 0o700)
	if runtime.GOOS == "windows" {
		return []string{"TEMP=" + tmp, "TMP=" + tmp}
	}
	return []string{"TMPDIR=" + tmp}
}

// RunnerStatus: doctor [18]용 — 각 언어의 감지 결과(실행 없음).
type LangStatus struct {
	Lang, Runner, Version string
	OK                    bool
}

func RunnerStatus() []LangStatus {
	langs := []string{"shell", "javascript", "typescript", "python", "go", "csharp"}
	out := make([]LangStatus, 0, len(langs))
	for _, l := range langs {
		r := table()[l]
		bin, ver, err := r.detect()
		st := LangStatus{Lang: l, OK: err == nil, Version: ver}
		if err == nil {
			st.Runner = filepath.Base(bin)
		}
		out = append(out, st)
	}
	return out
}
