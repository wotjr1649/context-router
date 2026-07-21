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
	"time"

	"github.com/wotjr1649/context-router/internal/cli"
	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/mcp"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

const version = "0.4.0"

type serverFlags struct {
	Root, StoreRoot, LogLevel   string
	Profile, Enable, AllowPaths []string
	Projects                    []string // --projects: global-search 전용 allowlist (설계 §8)
	NetAllowLocal               bool
	NetPorts                    []int
	RetentionEvents             time.Duration // --retention-events: 0 = off/무기한 (설계 §5, D17)
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
	var retentionEvents string
	fs.StringVar(&retentionEvents, "retention-events", "", "session event retention (time.ParseDuration format, e.g. 720h); default off (unlimited, 설계 §5)")
	if err := fs.Parse(args); err != nil {
		return serverFlags{}, err
	}
	if n := fs.NArg(); n > 0 {
		// 위치 인자는 허용하지 않는다(최종리뷰 F4) — 그러지 않으면 서브커맨드가 플래그보다
		// 앞이 아니라 뒤에 오타로 붙은 호출(예: "--store-root X doctor")이 dispatchCLI를
		// 그냥 통과해(args[0]가 "-"로 시작) MCP 서버가 조용히 기동해버린다("미지 서브커맨드
		// 거부" 원칙과 반대되는 구멍). 개수만 밝히고 사용자 입력 원문은 에코하지 않는다(§6).
		return serverFlags{}, fmt.Errorf("ctr: 위치 인자는 허용되지 않습니다 (받은 개수: %d)", n)
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
	d, err := parseRetentionEventsFlag(retentionEvents)
	if err != nil {
		return serverFlags{}, err
	}
	f.RetentionEvents = d
	return f, nil
}

// parseRetentionEventsFlag — --retention-events 값을 검증한다(설계 §5, D17). 빈 문자열(플래그
// 미지정, 기본값)은 0(=off=무기한)으로 통과한다. time.ParseDuration 표준 동작을 그대로 쓴다 —
// 커스텀 단위(예: "d")를 추가하지 않는다("30d"는 표준대로 오류, "720h"처럼 시간 단위로
// 환산해서 써야 한다 — 기존 --older-than 선례와 동일 관례). 파싱 오류·음수 기간은 기동
// 거부(사용자 입력 원문은 %w로 감싸지 않는다, cli 패키지 --older-than과 동일한 위생 관례).
func parseRetentionEventsFlag(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, errors.New("ctr: --retention-events 값이 유효한 기간이 아닙니다")
	}
	// D4(Codex P2): 음수는 물론, 1초 미만 양수도 거부한다 — retentionSecFromDuration이 초 단위로
	// 절삭해 500ms 같은 양수 기간이 0(무기한)으로 조용히 뭉개지는 경로를 차단한다(0=off는 빈
	// 문자열로만 표현, 위에서 이미 처리).
	if d < 0 || (d > 0 && d < time.Second) {
		return 0, errors.New("ctr: --retention-events 값이 유효한 기간이 아닙니다 (1초 미만 양수 금지)")
	}
	return d, nil
}

// retentionSecFromDuration — 파싱된 --retention-events → session.Options.RetentionSec(초)
// 변환(설계 §5). 0 Duration은 그대로 0(무기한/정책 미표명)으로 매핑된다.
func retentionSecFromDuration(d time.Duration) int64 {
	return int64(d / time.Second)
}

// sweepSessionRetentionAtStart — 서버 시작 시 1회 retention 스윕을 수행하는 헬퍼(설계 §5:
// "시작 시 1회 트랜잭션 + 삭제 건수 stderr 1줄 고지, 실패는 log-and-continue"). now는 값
// 주입(G7 결정론). run()이 openSessionDB로 session.Open을 배선한 직후(sessDB!=nil일 때만)
// 호출한다(T10, task-8-report.md의 구조 결정 그대로 이 자리에 합류).
func sweepSessionRetentionAtStart(ctx context.Context, d *session.DB, now time.Time, stderr io.Writer) {
	deleted, err := session.Sweep(ctx, d, now)
	if err != nil {
		fmt.Fprintf(stderr, "ctr: session retention sweep 실패(계속 진행): %v\n", err)
		return
	}
	fmt.Fprintf(stderr, "ctr: session retention sweep: %d개 이벤트 삭제\n", deleted)
}

// openSessionDB — session.Open을 배선한다(설계 §6.2 fail-closed, T10). sentinel 3종
// (ErrCorrupt·ErrRecoverPending·ErrLeaseHeld) 및 그 외 예기치 못한 오류 전부를 동일하게
// 처리한다: nil을 반환하고 stderr에 1줄 경고만 남긴 뒤 서버 시작은 계속 진행한다(§6.2
// "가용성 트레이드오프 수용" — content 도구는 정상 서빙, 세션 표면만 미등록이 NewServer의
// cfg.Session==nil 분기로 이어진다). ErrCorrupt·ErrRecoverPending은 수동 복구 CLI 안내를
// 추가로 남긴다(§6.3 "recover 재실행을 stderr로 안내").
func openSessionDB(dir string, opts session.Options, stderr io.Writer) *session.DB {
	d, err := session.Open(dir, opts)
	if err != nil {
		fmt.Fprintf(stderr, "ctr: session.db 사용 불가 — 세션 도구 비활성(content 도구는 정상 진행): %v\n", err)
		if errors.Is(err, session.ErrCorrupt) || errors.Is(err, session.ErrRecoverPending) {
			fmt.Fprintln(stderr, "ctr: 복구하려면 `context-router session recover`를 실행하세요")
		}
		return nil
	}
	return d
}

// validProfile: v0.0.1이 실제로 지원하는 두 형태뿐(mcp.NewServer가 Profile로 도구를
// 게이팅하지 않으므로, 임의 부분집합을 받아주면 사용자가 오인한다 — 설계 §8).
func validProfile(p []string) bool {
	return slices.Equal(p, []string{"search", "fetch", "transform"}) || slices.Equal(p, []string{"global-search"})
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

// canonicalizeStoreRoot: storeRoot를 allow-path와 동일한 기준(Abs→ident.RealPath→Fold)으로
// canonicalize한다 — 그래야 canonicalizeAllowPaths의 store-root 하위 거부가 심링크·windows
// junction으로 우회되지 않는다(§4.4, 최종리뷰 F1 — ident.RealPath는 filepath.EvalSymlinks와
// 달리 NTFS junction도 실경로로 해석한다). storeRoot는 아직 생성 전일 수 있어 RealPath의
// 미존재 오류는 관용 처리(Abs+Fold만 사용).
func canonicalizeStoreRoot(storeRoot string) (string, error) {
	abs, err := filepath.Abs(storeRoot)
	if err != nil {
		return "", fmt.Errorf("ctr: store-root: %w", err)
	}
	real, err := ident.RealPath(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ident.Fold(abs), nil
		}
		return "", fmt.Errorf("ctr: store-root: %w", err)
	}
	return ident.Fold(real), nil
}

// canonicalizeAllowPaths: 각 --allow-path를 canonicalize(Abs→ident.RealPath→Fold)하고
// store-root 하위면 시작을 거부한다(Task6 이관, 설계 §4.4). storeRoot 인자 자체도
// (호출자가 canonicalizeStoreRoot를 이미 거쳤든 raw든) 여기서 동일 기준으로
// 다시 canonicalize한다 — macOS `/var`→`/private/var` 같은 심링크 낀 경로에서
// storeRoot만 미해석 상태로 비교하면 실제로는 하위인 allow-path를 "무관"으로 오판해
// store-root 하위 거부(§4.4)가 심링크로 우회된다(3-OS CI 최초 실측 발견).
func canonicalizeAllowPaths(paths []string, storeRoot string) ([]string, error) {
	foldedStoreRoot, err := canonicalizeStoreRoot(storeRoot) // 이미 Abs→RealPath→Fold 반환
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("ctr: allow-path: %w", err)
		}
		real, err := ident.RealPath(abs)
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

// resolveProjectEntry: --projects 엔트리 1개를 판별한다. 경로 구분자가 없고
// <storeRoot>/projects/<entry>가 실재하는 디렉터리면 그 자체로 이미 store ID이므로 확정하고
// 경로 해석을 하지 않는다(최종리뷰 F5 — cli.purgeProjectID의 "ID 우선" 판별과 동형, D13상
// 서로 다른 패키지라 자체 인터페이스 없이 각자 소유) — 그러지 않으면 cwd에 우연히 동명
// 디렉터리가 있을 때 store ID가 경로로 오인되어 엉뚱한 프로젝트가 열린다. 그 외에는 기존
// 로직: 경로 구분자를 포함하거나 존재하는 디렉터리면 경로로 취급해 ident.Canonicalize로
// ProjectID·WorktreeRoot(=root)를 구한다. 그 외는 ProjectID 문자열 그대로 취급하고 root=""를
// 반환한다(설계 §4.6 — ID 엔트리는 원본 대조·경로 상대화가 제한됨, mcp.GlobalProject.Root
// 주석 참조).
func resolveProjectEntry(storeRoot, entry string) (id, root string, err error) {
	hasSep := strings.ContainsAny(entry, `/\`)
	if !hasSep {
		if fi, statErr := os.Stat(filepath.Join(storeRoot, "projects", entry)); statErr == nil && fi.IsDir() {
			return entry, "", nil // 이미 store ID로 확정 — 경로 해석 생략
		}
	}
	looksLikePath := hasSep
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
// ProjectID 기준으로 중복 엔트리를 먼저 걸러낸다(최종리뷰 F5) — 그러지 않으면 같은
// 프로젝트를 --projects에 두 번 주었을 때 store가 두 번 열리고 global-search 결과에도
// 같은 프로젝트의 hit이 중복 출현한다. store.Open(dir, true)는 실제 DB 파일을 지연
// 연결하므로(디렉터리/DB 없음이어도 즉시 에러가 안 남) PingContext로 강제 연결해 열기
// 실패를 즉시 드러낸다. 하나라도 실패하면 이미 연 store를 모두 Close하고 시작을
// 거부한다(fail-closed, 설계 §4.6/§5.4).
func buildGlobalProjects(ctx context.Context, storeRoot string, entries []string) ([]mcp.GlobalProject, error) {
	projects := make([]mcp.GlobalProject, 0, len(entries))
	closeAll := func() {
		for _, p := range projects {
			p.Store.Close()
		}
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id, root, err := resolveProjectEntry(storeRoot, entry)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("ctr: global-search: %w", err)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
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

	// mcp.NewServer는 Profile을 아직 미분기(v0.0.1 예약, 설계 §8 주석) — 그런데도 임의
	// 부분집합을 조용히 받으면 사용자가 실제로는 전체 도구가 등록되는데도 일부만 켠
	// 것으로 오인한다(설계 §2.1 "등록됨 = 보안 경계"). 침묵 대신 시작 시점에 거부한다.
	if !validProfile(f.Profile) {
		return fmt.Errorf("ctr: --profile: v0.0.1은 기본값 \"search,fetch,transform\" 또는 \"global-search\" 단독만 지원합니다 (받은 값: %q)", strings.Join(f.Profile, ","))
	}

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

	// session.db 배선(설계 §2.1 경로 예약: projects/<pid>/worktrees/<wid>) — 실패는
	// fail-closed(openSessionDB가 nil+stderr 경고로 흡수, content 도구는 영향 없음).
	sessDB := openSessionDB(
		filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID),
		session.Options{
			RetentionSec: retentionSecFromDuration(f.RetentionEvents),
			Producer:     fmt.Sprintf("context-router/%s", version),
			WorktreeRoot: canon.WorktreeRoot, // D3: session_start payload에 사용자 worktree 경로 주입(설계 §2.2)
		},
		stderr,
	)
	if sessDB != nil {
		defer sessDB.Close() // lease 해제 — release 비멱등 1회(session.DB.Close 계약)
		sweepSessionRetentionAtStart(ctx, sessDB, time.Now(), stderr)
	}

	selfExe, err := os.Executable()
	if err != nil {
		return err
	}
	return mcp.Serve(ctx, mcp.Config{
		Canon: canon, Store: st, SelfExe: selfExe,
		Profile: f.Profile, Enable: f.Enable, AllowPaths: allowPaths,
		NetAllowLocal: f.NetAllowLocal, NetPorts: f.NetPorts,
		Session: sessDB,
	})
}

// transformWorkerArg: worker 프로세스 재실행 숨김 모드 인자(설계 §4.3). 플래그 파싱보다
// 먼저 분기해야 한다 — stdout은 Result JSON 1건이어야 하고 배너·로그가 섞이면 안 된다.
const transformWorkerArg = "__transform-worker"

// cliSubcommands: internal/cli가 처리하는 서브커맨드 이름(설계 §7). 이 중 하나가 아닌 첫
// 인자는 dispatchCLI의 관심사가 아니다 — MCP 서버 플래그로 그대로 흘려보낸다. "session"은
// v0.1 태스크9 추가(§6.3·§7) — export(9a)·recover(9b) 두 하위 서브커맨드를 cli.Run이 내부
// 디스패치한다(이 맵은 최상위 이름 1개만 안다, T4-plan3 미지 서브커맨드 MCP 오기동 차단 정합).
// "hook"은 v0.2 추가(설계 §2) — Claude Code 훅 서브프로세스(stdin JSON 1건→cc: 세션 append).
// "usage"는 v0.2 추가(설계 §6) — 로컬 transcript 세션별 토큰 집계 + cc: 스트림 대조(읽기 전용).
// "codex-hook"은 v0.4 추가(설계 §2 D35) — Codex 러닝 훅 전용, 구버전 바이너리 오귀속 차단 게이트(§11.2 F3).
var cliSubcommands = map[string]bool{"doctor": true, "stats": true, "purge": true, "upgrade": true, "session": true, "hook": true, "usage": true, "codex-hook": true}

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
// absorbHookPreprocErr — 실행 훅(install/uninstall이 아닌 `hook`)의 전처리 오류를 fail-open으로
// 흡수한다(설계 §2 always-exit-0): stdin을 EOF까지 소비(broken pipe 방지)하고 stderr 1줄
// (절대경로·비밀 미포함)만 남긴 뒤 nil(exit 0)을 돌려준다. install/uninstall·그 외 서브커맨드는
// 원래 오류를 그대로 전파해 기존 exit 1 동작을 유지한다.
func absorbHookPreprocErr(sub string, rest []string, err error) error {
	isRunningHook := sub == "codex-hook" ||
		(sub == "hook" && (len(rest) == 0 || (rest[0] != "install" && rest[0] != "uninstall")))
	if !isRunningHook {
		return err
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	fmt.Fprintln(os.Stderr, "ctr: hook 전처리 실패 — 이벤트 무시(exit 0)")
	return nil
}

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
		// 실행 훅의 root 플래그 파싱 실패(예: 값 없는 --store-root가 settings에 잔존)도 fail-open으로
		// 흡수한다(D23 — 최종 리뷰 C3). rest는 파싱 실패로 비어 있을 수 있으므로 원본 subArgs로
		// install/uninstall(오류 전파 유지)을 판별한다.
		return true, absorbHookPreprocErr(sub, subArgs, err)
	}
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return true, absorbHookPreprocErr(sub, rest, err)
		}
	}
	storeRoot, err := storeRootFor(serverFlags{StoreRoot: storeRootRaw})
	if err != nil {
		return true, absorbHookPreprocErr(sub, rest, err)
	}
	storeRoot, err = canonicalizeStoreRoot(storeRoot)
	if err != nil {
		return true, absorbHookPreprocErr(sub, rest, err)
	}
	// storeRootExplicit: prescanRootFlags가 --store-root 토큰을 소비하므로 cli는 명시/기본을
	// 구분할 수 없다 — 여기서 판별해(원시값 비어있지 않음) 넘긴다. hook install이 명시된 경우에만
	// 훅 명령 args에 --store-root <원시값>을 주입한다(설계 §7).
	storeRootExplicit := storeRootRaw != ""
	return true, cli.Run(ctx, sub, rest, storeRoot, root, version, storeRootExplicit, storeRootRaw, os.Stdout, os.Stderr)
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
