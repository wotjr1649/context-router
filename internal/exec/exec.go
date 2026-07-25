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
	extra  func(scratch string) []string // 러너별 재지정/강제 env (nil 가능)
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
		"javascript": {file: "snippet.js", detect: detectJS, argv: fileArgv},
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
		env = append(env, r.extra(scratch)...)
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
		return runner{
			file: "snippet.ps1",
			detect: func() (string, string, error) {
				if p, err := exec.LookPath("pwsh"); err == nil {
					return p, "7", nil
				}
				if p, err := exec.LookPath("powershell"); err == nil {
					return p, "5.1", nil // runner 필드로 5.1 가시화 (D60)
				}
				return "", "", ErrToolchainMissing
			},
			argv: func(bin, file string) []string {
				return []string{bin, "-NoProfile", "-NonInteractive", "-File", file}
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

func detectJS() (string, string, error) {
	p, err := lookVersioned([]string{"bun", "node"})
	return p, "", err
}

func tsRunner() runner {
	return runner{
		file:   "snippet.ts",
		detect: detectTS,
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

func pyEnv(scratch string) []string {
	cache := filepath.Join(scratch, "pycache")
	_ = os.MkdirAll(cache, 0o700)
	return []string{"PYTHONPYCACHEPREFIX=" + cache}
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
func goEnv(scratch string) []string {
	build := filepath.Join(scratch, "go-build")
	gopath := filepath.Join(scratch, "go")
	gotmp := filepath.Join(scratch, "go-tmp")
	for _, d := range []string{build, gopath, gotmp} {
		_ = os.MkdirAll(d, 0o700)
	}
	return []string{
		"GOCACHE=" + build,
		"GOPATH=" + gopath,
		"GOTMPDIR=" + gotmp,
	}
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
func csEnv(scratch string) []string {
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
	for _, d := range []string{home, nuget, nugetHTTP, nugetPlugins, migrations, appSupport} {
		_ = os.MkdirAll(d, 0o700)
	}
	_ = os.WriteFile(filepath.Join(migrations, "1"), nil, 0o600)
	// 이 파일이 호스트 구성 차단의 유일한 시행점이라 조용한 실패는 격리 없이 실행되는 것과
	// 같다 — 러너 오류로 올리지는 않되(다른 재지정과 동일한 best-effort 계약) 관측은 남긴다.
	if err := os.WriteFile(filepath.Join(scratch, "nuget.config"), []byte(nugetConfig), 0o600); err != nil {
		slog.Warn("exec: NuGet 구성 기록 실패 — 호스트 구성이 병합된 채로 실행", "error", err)
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
	}
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
