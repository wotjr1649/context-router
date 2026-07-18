// Command context-router — MCP 서버 진입점·플래그·배선. 설계서 §2.2, §8.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/wotjr1649/context-router/internal/cli"
	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/mcp"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

const version = "0.0.1-dev"

type serverFlags struct {
	Root, StoreRoot, LogLevel   string
	Profile, Enable, AllowPaths []string
	Projects                    []string // --projects: global-search 전용 allowlist (설계 §8)
	NetAllowLocal               bool
	NetPorts                    []int
}

func parseFlags(args []string) (serverFlags, error) {
	fs := flag.NewFlagSet("context-router", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f serverFlags
	var profile, enable string
	fs.StringVar(&f.Root, "root", "", "project root (default: cwd)")
	fs.StringVar(&f.StoreRoot, "store-root", "", "store root override")
	fs.StringVar(&profile, "profile", "search,fetch,transform", "tool profile")
	fs.StringVar(&enable, "enable", "", "opt-in profiles: ingest,net")
	fs.StringVar(&f.LogLevel, "log-level", "info", "log level")
	fs.Func("allow-path", "extra ingest root (repeatable)", func(v string) error {
		f.AllowPaths = append(f.AllowPaths, v)
		return nil
	})
	fs.BoolVar(&f.NetAllowLocal, "net-allow-local", false, "allow 127.0.0.1/::1 destinations for fetch_and_index")
	var netPorts string
	fs.StringVar(&netPorts, "net-ports", "", "extra allowed ports for fetch_and_index (comma-separated)")
	var projects string
	fs.StringVar(&projects, "projects", "", "global-search project allowlist (comma-separated paths or IDs, required for --profile global-search)")
	if err := fs.Parse(args); err != nil {
		return serverFlags{}, err
	}
	f.Profile = strings.Split(profile, ",")
	if enable != "" {
		f.Enable = strings.Split(enable, ",")
	}
	for _, p := range strings.Split(projects, ",") {
		if p = strings.TrimSpace(p); p != "" {
			f.Projects = append(f.Projects, p)
		}
	}
	if netPorts != "" {
		for _, p := range strings.Split(netPorts, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return serverFlags{}, fmt.Errorf("ctr: net-ports: %w", err)
			}
			f.NetPorts = append(f.NetPorts, n)
		}
	}
	return f, nil
}

func banner(f serverFlags, root string) string {
	onoff := func(name string) string {
		if slices.Contains(f.Enable, name) {
			return "on"
		}
		return "off"
	}
	return fmt.Sprintf("[ctr] v%s profile=%s ingest=%s net=%s root=%s",
		version, strings.Join(f.Profile, ","), onoff("ingest"), onoff("net"), root)
}

// errAllowPathViolation: --allow-path가 store-root 하위를 가리킬 때(자기 재귀 색인 방지,
// 설계 §4.4·Task6 이관).
var errAllowPathViolation = errors.New("ctr: allow-path가 store-root 하위 경로입니다")

// defaultStoreRoot: OS별 저장 루트 기본값 (설계 §3.1).
func defaultStoreRoot() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("ctr: store-root: %%LOCALAPPDATA%% 미설정")
		}
		return filepath.Join(base, "context-router"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("ctr: store-root: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "context-router"), nil
	default:
		if base := os.Getenv("XDG_DATA_HOME"); base != "" {
			return filepath.Join(base, "context-router"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("ctr: store-root: %w", err)
		}
		return filepath.Join(home, ".local", "share", "context-router"), nil
	}
}

// storeRootFor: 우선순위 --store-root > CTR_STORE_ROOT > OS 기본값 (설계 §3.1).
func storeRootFor(f serverFlags) (string, error) {
	if f.StoreRoot != "" {
		return f.StoreRoot, nil
	}
	if v := os.Getenv("CTR_STORE_ROOT"); v != "" {
		return v, nil
	}
	return defaultStoreRoot()
}

// withinRoot: p(ident.Fold됨)가 root(ident.Fold됨) 하위인지 — 문자열 접두사 매칭 금지,
// filepath.Rel 기반(ingest.withinRoot와 동형, 설계 §4.4).
func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonicalizeStoreRoot: storeRoot를 allow-path와 동일한 기준(Abs→EvalSymlinks→Fold)으로
// canonicalize한다 — 그래야 canonicalizeAllowPaths의 store-root 하위 거부가 심링크로
// 우회되지 않는다(§4.4). storeRoot는 아직 생성 전일 수 있어 EvalSymlinks의 미존재 오류는
// 관용 처리(Abs+Fold만 사용).
func canonicalizeStoreRoot(storeRoot string) (string, error) {
	abs, err := filepath.Abs(storeRoot)
	if err != nil {
		return "", fmt.Errorf("ctr: store-root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ident.Fold(abs), nil
		}
		return "", fmt.Errorf("ctr: store-root: %w", err)
	}
	return ident.Fold(real), nil
}

// canonicalizeAllowPaths: 각 --allow-path를 canonicalize(Abs→EvalSymlinks→Fold)하고
// store-root 하위면 시작을 거부한다(Task6 이관, 설계 §4.4).
func canonicalizeAllowPaths(paths []string, storeRoot string) ([]string, error) {
	foldedStoreRoot := ident.Fold(storeRoot)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("ctr: allow-path: %w", err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("ctr: allow-path: %w", err)
		}
		if withinRoot(foldedStoreRoot, ident.Fold(real)) {
			return nil, fmt.Errorf("ctr: allow-path: %w", errAllowPathViolation)
		}
		out = append(out, real)
	}
	return out, nil
}

// resolveProjectEntry: --projects 엔트리 1개를 판별한다. 경로 구분자(/,\)를 포함하거나
// 존재하는 디렉터리면 경로로 취급해 ident.Canonicalize로 ProjectID·WorktreeRoot(=root)를
// 구한다. 그 외는 ProjectID 문자열 그대로 취급하고 root=""를 반환한다(설계 §4.6 — ID
// 엔트리는 원본 대조·경로 상대화가 제한됨, mcp.GlobalProject.Root 주석 참조).
func resolveProjectEntry(entry string) (id, root string, err error) {
	looksLikePath := strings.ContainsAny(entry, `/\`)
	if !looksLikePath {
		if fi, statErr := os.Stat(entry); statErr == nil && fi.IsDir() {
			looksLikePath = true
		}
	}
	if !looksLikePath {
		return entry, "", nil
	}
	canon, err := ident.Canonicalize(entry)
	if err != nil {
		return "", "", err
	}
	return canon.ProjectID, canon.WorktreeRoot, nil
}

// buildGlobalProjects: entries 각각을 resolveProjectEntry로 판별해 read-only store로 연다.
// store.Open(dir, true)는 실제 DB 파일을 지연 연결하므로(디렉터리/DB 없음이어도 즉시
// 에러가 안 남) PingContext로 강제 연결해 열기 실패를 즉시 드러낸다. 하나라도 실패하면
// 이미 연 store를 모두 Close하고 시작을 거부한다(fail-closed, 설계 §4.6/§5.4).
func buildGlobalProjects(ctx context.Context, storeRoot string, entries []string) ([]mcp.GlobalProject, error) {
	projects := make([]mcp.GlobalProject, 0, len(entries))
	closeAll := func() {
		for _, p := range projects {
			p.Store.Close()
		}
	}
	for _, entry := range entries {
		id, root, err := resolveProjectEntry(entry)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("ctr: global-search: %w", err)
		}
		st, err := store.Open(filepath.Join(storeRoot, "projects", id), true)
		if err == nil {
			err = st.Reader().PingContext(ctx)
		}
		if err != nil {
			if st != nil {
				st.Close()
			}
			closeAll()
			return nil, fmt.Errorf("ctr: global-search: 프로젝트 %q 열기 실패: %w", id, err)
		}
		projects = append(projects, mcp.GlobalProject{ID: id, Root: root, Store: st})
	}
	return projects, nil
}

// parseLogLevel: --log-level 문자열→slog.Level. 미지 값은 info로 뭉갠다.
func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: parseLogLevel(f.LogLevel)})))

	// global-search 프로필 ⇄ --projects는 1:1 필수 대응(모호성 차단, 설계 §4.6/§8).
	isGlobal := slices.Contains(f.Profile, "global-search")
	if isGlobal && len(f.Projects) == 0 {
		return errors.New("ctr: --profile global-search은 --projects가 필수입니다")
	}
	if !isGlobal && len(f.Projects) > 0 {
		return errors.New("ctr: --projects는 --profile global-search에서만 사용할 수 있습니다")
	}

	root := f.Root
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	fmt.Fprintln(stderr, banner(f, root))

	storeRoot, err := storeRootFor(f)
	if err != nil {
		return err
	}
	storeRoot, err = canonicalizeStoreRoot(storeRoot)
	if err != nil {
		return err
	}

	if isGlobal {
		// global 분기: cwd store.Open/transform probe/ingest·net 게이팅 전부 미수행 —
		// 오직 --projects의 read-only store만 열어 mcp.ServeGlobal에 넘긴다(설계 §5.4).
		projects, err := buildGlobalProjects(ctx, storeRoot, f.Projects)
		if err != nil {
			return err
		}
		defer func() {
			for _, p := range projects {
				p.Store.Close()
			}
		}()
		return mcp.ServeGlobal(ctx, mcp.GlobalConfig{Projects: projects})
	}

	canon, err := ident.Canonicalize(root)
	if err != nil {
		return err
	}
	allowPaths, err := canonicalizeAllowPaths(f.AllowPaths, storeRoot)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(storeRoot, "projects", canon.ProjectID), false)
	if err != nil {
		return err
	}
	defer st.Close()

	selfExe, err := os.Executable()
	if err != nil {
		return err
	}
	return mcp.Serve(ctx, mcp.Config{
		Canon: canon, Store: st, SelfExe: selfExe,
		Profile: f.Profile, Enable: f.Enable, AllowPaths: allowPaths,
		NetAllowLocal: f.NetAllowLocal, NetPorts: f.NetPorts,
	})
}

// transformWorkerArg: worker 프로세스 재실행 숨김 모드 인자(설계 §4.3). 플래그 파싱보다
// 먼저 분기해야 한다 — stdout은 Result JSON 1건이어야 하고 배너·로그가 섞이면 안 된다.
const transformWorkerArg = "__transform-worker"

// cliSubcommands: internal/cli가 처리하는 4개 서브커맨드 이름(설계 §7). 이 중 하나가 아닌
// 첫 인자는 dispatchCLI의 관심사가 아니다 — MCP 서버 플래그로 그대로 흘려보낸다.
var cliSubcommands = map[string]bool{"doctor": true, "stats": true, "purge": true, "upgrade": true}

// prescanRootFlags: cli 서브커맨드 args에서 --root/--store-root(단대시 -root/-store-root,
// "--f v"·"--f=v" 두 형태 모두)만 수동으로 뽑아내고 그 토큰을 제거한 나머지를 반환한다.
// 서버 전체 flagset(parseFlags)을 서브커맨드에 재사용하지 않는 이유: stats(--provider),
// 이후 purge(--project/--older-than/--gc/--force) 같은 서브커맨드 전용 플래그는 그 flagset에
// 없어 항상 "flag provided but not defined"로 실패했다(Task4 리뷰 지적, 설계 §7 계약 —
// `context-router stats --provider <path>`가 실제로 동작해야 한다). 나머지 플래그의 유효성
// 검사는 여기서 하지 않는다 — 각 서브커맨드가 소유한 cli 쪽 flagset이 미지 플래그를 오류로
// 낸다. "--f v" 형태에서 다음 토큰이 없거나 "-"로 시작하면(다른 플래그처럼 보이면) 값으로
// 삼키지 않고 오류를 반환한다 — 그렇지 않으면 `stats --root --provider p` 같은 오타가
// --provider를 --root의 값으로 조용히 삼켜버린다(리뷰 Fix Round 2, Important-1).
func prescanRootFlags(args []string) (root, storeRoot string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := strings.TrimLeft(a, "-")
		if name == a { // "-" 접두사가 없는 토큰(위치 인자)은 그대로 통과
			rest = append(rest, a)
			continue
		}
		key, val, hasEq := strings.Cut(name, "=")
		var target *string
		switch key {
		case "root":
			target = &root
		case "store-root":
			target = &storeRoot
		default:
			rest = append(rest, a)
			continue
		}
		if hasEq {
			*target = val // ponytail: "--root=" 빈 값은 의도적으로 미지정과 동일하게 동작(아래 root=="" 분기가 cwd로 채움)
			continue
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
			return "", "", nil, fmt.Errorf("ctr: --%s: 값 누락", key)
		}
		*target = args[i+1]
		i++
	}
	return root, storeRoot, rest, nil
}

// dispatchCLI: args[1](=os.Args[1])이 cli 서브커맨드 4개 중 하나면 storeRoot·projectRoot를
// prescanRootFlags + 기존 storeRootFor+canonicalizeStoreRoot로 결정해 cli.Run에 위임한다.
// handled=false면 호출자가 평소대로 MCP 서버 경로(run)로 진행해야 한다 — cli는 storeRoot를
// 재도출하지 않는다(설계 §7 Produces). --root/--store-root를 제외한 나머지 args는 그대로
// cli.Run에 넘겨 서브커맨드 전용 flagset(stats의 --provider 등)이 스스로 파싱한다.
func dispatchCLI(ctx context.Context, args []string) (handled bool, err error) {
	if len(args) < 2 {
		return false, nil
	}
	sub := args[1]
	if !cliSubcommands[sub] {
		if strings.HasPrefix(sub, "-") {
			return false, nil // MCP 서버 플래그(예: --profile) — run()으로 진행
		}
		// "-"로 시작하지 않는데 4개 서브커맨드도 아니다 — `context-router stat` 같은 오타를
		// 조용히 MCP 서버로 흘려보내면 안 된다(리뷰 Fix Round 3, item 1). 명시 거부.
		return true, fmt.Errorf("ctr: 미지 서브커맨드: %s", sub)
	}
	subArgs := args[2:]

	root, storeRootRaw, rest, err := prescanRootFlags(subArgs)
	if err != nil {
		return true, err
	}
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return true, err
		}
	}
	storeRoot, err := storeRootFor(serverFlags{StoreRoot: storeRootRaw})
	if err != nil {
		return true, err
	}
	storeRoot, err = canonicalizeStoreRoot(storeRoot)
	if err != nil {
		return true, err
	}
	return true, cli.Run(ctx, sub, rest, storeRoot, root, version, os.Stdout, os.Stderr)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == transformWorkerArg {
		if err := transform.RunWorker(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "ctr:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if handled, err := dispatchCLI(ctx, os.Args); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "ctr:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ctr:", err)
		os.Exit(1)
	}
}
