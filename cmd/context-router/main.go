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

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/mcp"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

const version = "0.0.1-dev"

type serverFlags struct {
	Root, StoreRoot, LogLevel   string
	Profile, Enable, AllowPaths []string
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
	if err := fs.Parse(args); err != nil {
		return serverFlags{}, err
	}
	f.Profile = strings.Split(profile, ",")
	if enable != "" {
		f.Enable = strings.Split(enable, ",")
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
	root := f.Root
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	fmt.Fprintln(stderr, banner(f, root))

	canon, err := ident.Canonicalize(root)
	if err != nil {
		return err
	}
	storeRoot, err := storeRootFor(f)
	if err != nil {
		return err
	}
	storeRoot, err = canonicalizeStoreRoot(storeRoot)
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
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ctr:", err)
		os.Exit(1)
	}
}
