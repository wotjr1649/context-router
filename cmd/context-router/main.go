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

	"github.com/wotjr1649/context-router/internal/buildinfo"
	"github.com/wotjr1649/context-router/internal/cli"
	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/mcp"
	"github.com/wotjr1649/context-router/internal/sandbox"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

var version = buildinfo.ProductVersion()

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
	fs.StringVar(&enable, "enable", "", "opt-in profiles: ingest,net,exec; none = zero profiles")
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
	// D101 계약 1: --enable이 빈 문자열일 때만(미지정·"--enable="로 명시한 빈 값 둘 다 — flag
	// 패키지에는 그 둘을 구분할 수단이 없다) CTR_ENABLE을 대신 읽는다. 플래그가 주어지면
	// 플래그가 이긴다. storeRootFor의 CTR_STORE_ROOT 관례(직접 os.Getenv, 테스트는 t.Setenv)를
	// 그대로 따른다 — 이 패키지에 이미 있는 관례라 별도 getenv 파라미터를 두지 않는다.
	// D101 계약 2(설계 v0.19): "기본값은 서버가 갖는다. 환경 변수가 없으면 현행 기본 프로필로
	// 돈다 — Codex 사용자가 아무것도 안 해도 동작해야 한다." 플러그인의 plugin/mcp.json은
	// args를 고정하지 않으므로(옛 --enable ingest,net 고정이 CTR_ENABLE을 영구히 이겨 그 환경
	// 변수를 무의미하게 만들었다 — v0.19 리뷰 C1) 서버 자신이 이 기본값을 갖는다. 둘 다 없을
	// 때만 적용되고, --enable·CTR_ENABLE 어느 쪽이든 있으면 그 값이 그대로 이긴다(우선순위
	// 불변). 이 변경으로 인자 없는 맨 context-router 실행이 이제 ctr_index·ctr_fetch_and_index를
	// 등록한다 — 이전에는 --enable/CTR_ENABLE 없이는 둘 다 꺼져 있었다. net은 아웃바운드 HTTP를
	// 여는 프로필이라 이것은 문서화되지 않은 맨 실행의 자세(posture) 변화다 — 이미 문서화된 설치
	// 절차는 전부 ingest,net을 명시로 전달하므로 그 경로는 영향이 없다(v0.19 리뷰 C1, 소유자
	// 결정).
	// 재검토 리뷰 2: 공백뿐이거나 쉼표뿐인 값(예: CTR_ENABLE="   "나 ",")은 원본 문자열이
	// 비어 있지 않아 위 "빈 문자열일 때만" 이관·기본값 대체 둘 다 건너뛰지만, parseEnableList가
	// 그런 값을 오류 없이 빈 슬라이스로 돌려주므로(항목별 트림 뒤 전부 공백이라 스킵) 결과가
	// 조용히 "프로필 0개"가 된다 — parseEnableList 자신의 doc 주석이 막으려는 바로 그 조용한
	// 오설정이다. 그래서 각 단계를 "원본이 비었는가"가 아니라 "파싱 결과가 비었는가"로 넘긴다:
	// 플래그가 쓸모없으면(빈 값이든 공백/쉼표뿐이든) 환경 변수로, 환경 변수도 쓸모없으면
	// 기본값으로. 오류(모르는 이름)는 그 자리에서 바로 반환하고 다음 단계로 넘기지 않는다 —
	// 오탈자를 조용히 삼키면 안 된다는 것은 기존 계약 그대로다.
	// 릴리스 리뷰 F2: 그 "파싱 결과가 비었는가" 판정은 프로필 0개를 요청할 자리를 남기지
	// 않았다 — `--enable=`도 `CTR_ENABLE=""`도 "이 단계는 아무것도 안 줬다"로 읽혀 기본값
	// ingest,net으로 떨어졌고, 아웃바운드 HTTP를 여는 net이 명시 지시와 반대 방향으로 켜진 채
	// 그 사실을 알리는 곳도 없었다. 그래서 각 단계의 반환을 (목록, 값을 주었는가) 둘로 나눠
	// "0개 요청"과 "무입력"이 서로 다른 모양이 되게 하고, 0개는 이름 none으로만 표현한다.
	// 판정을 빈 문자열 유무로 되돌리는 방향(fs.Visit·os.LookupEnv)은 두지 않는다: 미설정 변수를
	// 빈 값으로 펼치는 셸이 opt-in 전부를 조용히 끄게 되고, 우발적 빈 값이 의도적 빈 값보다
	// 훨씬 흔하다. 이름은 우발적으로 도착하지 않는다.
	enableList, supplied, err := parseEnableList(enable)
	if err != nil {
		return serverFlags{}, err
	}
	if !supplied {
		if enableList, supplied, err = parseEnableList(os.Getenv("CTR_ENABLE")); err != nil {
			return serverFlags{}, err
		}
	}
	if !supplied {
		if enableList, _, err = parseEnableList(defaultEnableProfile); err != nil {
			return serverFlags{}, err // 상수라 사실상 도달하지 않는다
		}
	}
	f.Enable = enableList
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

// enableProfileNames — --enable·CTR_ENABLE이 받는 프로필 이름 집합(D101). internal/cli가 들던
// 같은 이름 셋(mcpProfileNames)은 기입 경로와 함께 지워졌으므로 이제 이 목록이 유일한 자리다.
var enableProfileNames = []string{"ingest", "net", "exec"}

// enableProfileNone — 빈 집합을 뜻하는 이름(릴리스 리뷰 F2). 값 전체일 때만 유효하다: 다른
// 이름과 섞이면 두 해석(0개 / 그 이름만)이 다 말이 되므로 오류로 돌린다. 이 이름 하나가
// v0.19 이전 자세(search/fetch/transform만, 색인 쓰기도 아웃바운드 HTTP도 없음)를 되돌리는
// 유일한 표현이다.
const enableProfileNone = "none"

// enableNamesMsg — 두 오류 문면이 나열하는 허용 값 목록. 한 벌만 두어 이름이 늘 때 한쪽만
// 갱신되는 일을 없앤다(D13 반파편화).
var enableNamesMsg = strings.Join(enableProfileNames, ",") + "," + enableProfileNone

// defaultEnableProfile — --enable도 CTR_ENABLE도 없을 때 서버가 갖는 기본값(D101 계약 2,
// v0.19 리뷰 C1). exec는 여전히 opt-in이다 — 이 기본값이 켜는 것은 실행 도구가 아니라
// 색인·네트워크뿐이다.
const defaultEnableProfile = "ingest,net"

// parseEnableList — --enable/CTR_ENABLE 공통 문법(쉼표 구분·항목별 공백 트림·빈 항목 무시)을
// 판다. 모르는 이름은 오류로 거부한다: mcp.NewServer(internal/mcp/mcp.go)는 cfg.Enable에
// slices.Contains(…, "ingest"/"net"/"exec")로만 반응하므로 그 셋에 없는 이름은 오류도 경고도
// 없이 그냥 아무 도구도 켜지 않는다 — CTR_ENABLE은 플래그보다 오타가 눈에 덜 띄어 그 침묵이
// "프로필이 켜졌다"는 오인으로 이어지기 쉽다. 오류 문면에 입력 원문을 담지 않는다(규약 §6
// 사용자 입력 에코 금지) — 허용 이름만 나열한다.
// supplied(릴리스 리뷰 F2): 이 단계가 값을 주었는지. 이름 하나라도 남으면 true이고, none
// 단독도 true다(빈 목록을 명시로 요청한 것이라 다음 단계로 넘어가지 않는다). 빈 문자열·공백
// 뿐·쉼표뿐은 false라 호출자가 다음 단계로 넘긴다 — 그 갈래는 종전 그대로다.
func parseEnableList(raw string) (out []string, supplied bool, err error) {
	sawNone := false
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if name == enableProfileNone {
			sawNone = true
			continue
		}
		if !slices.Contains(enableProfileNames, name) {
			return nil, false, fmt.Errorf("ctr: --enable/CTR_ENABLE에 모르는 프로필 이름이 있습니다(가능: %s)", enableNamesMsg)
		}
		out = append(out, name)
	}
	if sawNone {
		if len(out) > 0 {
			return nil, false, fmt.Errorf("ctr: --enable/CTR_ENABLE의 %s은 값 전체일 때만 유효합니다(가능: %s)", enableProfileNone, enableNamesMsg)
		}
		return nil, true, nil // 빈 집합을 명시로 요청 — 다음 단계(환경 변수·기본값)로 넘기지 않는다
	}
	return out, len(out) > 0, nil
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
	rep, err := session.Sweep(ctx, d, now)
	if err != nil {
		fmt.Fprintf(stderr, "ctr: session retention sweep 실패(계속 진행): %v\n", err)
		return
	}
	fmt.Fprintf(stderr, "ctr: session retention sweep: %d개 이벤트 삭제, empty-session GC %d건\n",
		rep.EventsDeleted, rep.EmptySessionsDeleted)
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

// startupPurgeBudget — 기동 회수 고루틴의 폭주 가드. 회수는 mcp.Serve를 막지 않는 별도 고루틴이라
// 이 값은 세션 시작 지연이 아니고, 정상적으로 느린 회수를 끊지 않을 만큼 넉넉해야 한다: 실측
// 배치 보유가 100해시 약 0.63s(6~9ms/hash, 210MiB store)이고 거기에 txRetry BUSY 백오프(50/200/800ms)와
// 문장마다 최악 busy_timeout(5s) 대기가 겹칠 수 있다. 60s는 그 합보다 크게 잡은 값이다 — 소진은
// 정상 경로가 아니라 이상 신호이므로 경고로 남긴다(짧은 예산을 걸었을 때의 실측 결과는 정반대였다:
// 예산을 넘긴 배치는 단일 tx라 통째 롤백돼 회수가 전혀 진행되지 않았다).
//
// 강제되는 범위(하드 바운드가 아니다): 행 삭제 tx는 드라이버가 ctx 취소를 sqlite3_interrupt로
// 전달해 촘촘히 끊긴다(실측 5/50/150ms 예산 → 소요 5.5/50.2/154ms). 파일 회수 루프도 D74부터
// 매 hash 선두에서 ctx.Err()를 관측해 예산 소진 시 조기 반환한다(store.reclaimHookBlobs) — 그래도
// 취소 없이 정상 완주할 때의 상한은 시간이 아니라 startupPurgeMaxHashes다. 또 SQLite busy handler는
// interrupt 플래그를 보지 않아 다른 프로세스가 쥔 락을 기다리는 busy_timeout(5s)은 이 예산을 넘길
// 수 있다(잠금 대기 자체는 D74의 lockStoreCtx가 ctx도 함께 관측한다 — store.go:67).
const startupPurgeBudget = 60 * time.Second

// startupPurgeMaxHashes — 1회 회수 배치 상한. 이 값이 묶는 것은 기동당 작업량이 아니라 **잠금 보유
// 시간**이다: 배치는 행 삭제 tx로 content.db 쓰기 락을, 이어서 파일 회수로 lockStore를 잡는데, 그
// 창에서 훅의 Read 가드가 writable Open에 실패하면 색인을 포기하고 가드를 조용히 통과시킨다(훅 총예산
// 2000ms). 그래서 보유 시간이 그 예산의 일부에 머무는 값을 실측으로 골랐다 — 100해시 배치의 보유는
// 632·633ms(6.3ms/hash), 80해시는 734ms(같은 노이즈 대역), 아티팩트 1254개 시절의 7.9ms/hash로는
// 100해시가 약 790ms다. 최악에도 훅 예산의 1/3~2/5만 쓰고 1.2s 이상을 남긴다(경합 프로세스의
// writable Open 자체는 실측 약 31ms만 지연됐다 — 무경합 78ms vs 109ms).
// 더 내리지 않는 이유: 정상상태 유입이 세션당 shadow 약 67해시라 그보다 낮으면 회수가 유입을 못
// 따라간다. 더 올리지 않는 이유: 보유 시간이 훅 예산을 잠식하고, 행 삭제가 단일 tx라 배치가 클수록
// 롤백 시 잃는 진행분도 커진다(예산을 넘긴 배치는 통째 롤백 — 부분 진행이 없다).
//
// **이 상한이 묶지 못하는 것**(2026-07-26 머지 전 코드리뷰; D73 실측으로 갱신 2026-07-27,
// 2026-07-27 재갱신): `LIMIT`은 반환 행만 묶고 shadow 술어 평가는 묶지 않는다. v0.13 D73이 이
// 술어가 쓰는 `sources.raw_blob_hash`·`artifact_id` 조회에 복합 색인 3개(shadowIndexDDL)를
// 추가했다. 실측(설계 v0.13 §3, D73 §2⑧ — artifacts 여러 행에 sources를 나눠 담는 픽스처)은
// 색인이 있을 때 행 수 10배(2천→2만)에 술어 소요가 ×10.0(거의 선형)로 자라고, 없을 때는
// ×126.0(거의 제곱)로 자람을 보였다 — 색인이 이 술어를 크게 완화하지만 없애지는 않는다.
// (D73 §3의 실측은 술어 SELECT만 격리한 값이라 보유 시간이 아니다.) 배치 크기를 고정해도
// shadow 아티팩트가 늘면 술어 비용이 함께 자란다는 결론 자체는 색인 유무와 무관하게
// (정도만 다르게) 유효하다.
//
// D77(v0.14): 색인 도입 이후 합성 픽스처로 재측정했다 — 20해시 19.8ms(해시당 약 1.0ms,
// -count 5 재실행 대역 13.0~23.7ms), 회수 경로 확인(ReclaimedB>0·DeferredFiles=0).
// 자동 게이트는 store_test.go의 TestPurgeHookOnlyLockHoldBudget이며 임계는 훅 예산
// 2000ms의 50%인 1000ms다. 위 632·633ms는 **D73 색인 도입 이전**의 실 도그푸딩
// 저장소(색인 없음, 아티팩트 1254개·210MiB) 값이라 색인 유무도 분포도 달라 절대값
// 비교는 성립하지 않는다 — 게이트가 잡는 것은 해시당 비용의 회귀다.
const startupPurgeMaxHashes = 100

// defaultFTSMergeInterval — D102 계약 2. 세그먼트 축적이 하루 약 36 MB이고 정상상태 병합이
// 약 1.2초 쓰기 락을 잡는다(설계 v0.20 관측 B). 그 보유가 훅 Read 가드의 2000 ms 총예산과
// 겹치면 그 훅의 포착이 버려지므로(계약 9 — 수용 위험, doctor [12]의 shadow-store drop으로
// 사후 관측된다) 하루 한 번으로 묶는다.
const defaultFTSMergeInterval = 24 * time.Hour

// ftsMergeStartDelay — 기동 첨두를 비켜나는 지연. 기동 직후에는 퍼지 배치가 쓰기 락을 잡고
// (startupPurgeMaxHashes 주석의 실측 보유) 세션 시작 훅이 몰린다 — 그 위에 병합을 얹지 않는다.
// **이 지연보다 짧은 세션은 병합하지 않는다**(의도된 성질): 스탬프는 벽시계라 다음에 이 지연을
// 넘긴 세션 하나가 밀린 몫을 한 번에 걷는다.
const ftsMergeStartDelay = 30 * time.Second

// runFTSMergeLoop — D102 계약 2의 자동 경로. ctx가 죽을 때까지 delay 뒤 한 번, 그 뒤 interval
// 마다 조건을 다시 본다. **기동 퍼지 고루틴에 얹지 않는 이유가 셋이다**(설계 v0.20 D102 계약 2):
// 그 고루틴은 기동당 1회만 돌아 오래 사는 서버에서 병합이 영영 안 오고, purgeErr는 정상 종료
// (purgeCtx 취소)에서도 non-nil이며, 60초 예산을 나눠 쓰면 optimize가 단일 암시 트랜잭션이라
// 중단 시 통째 롤백돼 매번 0의 진전을 낸다. 그래서 여기는 purgeErr를 보지 않고 purgeCtx도
// 쓰지 않는다. 실패는 로그만 남긴다 — 스탬프가 안 찍히므로 다음 주기가 그대로 재시도한다.
func runFTSMergeLoop(ctx context.Context, st *store.Store, delay, interval time.Duration) {
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		if ran, err := st.MergeFTSIfDue(ctx, interval, time.Now()); errors.Is(err, context.Canceled) {
			// 정상 종료(cancelMerge)도 진행 중 병합을 이 취소로 만든다 — D102 계약 2가 퍼지 고루틴을
			// 배제한 이유 ②와 같은 형태라 같은 파일의 퍼지 선례(729행)와 강등을 맞춘다: 깨끗한
			// 종료를 실패로 찍지 않는다.
			slog.Debug("FTS 병합 중단 — 서버 종료", "error", err)
		} else if err != nil {
			slog.Warn("FTS 병합 실패 — 다음 주기에 재시도", "error", err)
		} else if ran {
			// took: 설계 §4-1이 배포 후 반드시 잴 것 1순위로 못박은 optimize 소요 시간 — 실측 1.2초는
			// 하한이고 훅 예산 잠식(계약 9) 논거가 그 위에 선다.
			slog.Info("FTS 병합 완료", "took", time.Since(start))
		}
		t.Reset(interval)
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
	scratchRoot := filepath.Join(os.TempDir(), "ctr-exec-"+canon.ProjectID) // D58: OS temp 하위
	if slices.Contains(f.Enable, "exec") {
		sandbox.SweepStale(scratchRoot, 24*time.Hour) // D61: 기동 시 24h+ 스테일 스윕
	}
	// D67: shadow 귀속 보존 기간 초과분을 기동 시 1회 회수한다. exec 프로필과 무관해 그 if 밖이다.
	// mcp.Serve를 막지 않는 별도 고루틴으로 돌린다 — 인라인이면 상시 로드(alwaysLoad, D63③)에서
	// 호스트가 서버 연결까지 세션 시작 자체를 막으므로 회수 시간이 그대로 사용자 대기가 되고, 그
	// 제약에 맞춘 짧은 예산에서는 실측 1.57s 배치(당시 상한 200해시)가 통째 롤백돼(단일 tx) 회수가 전혀 진행되지
	// 않았다. VACUUM은 하지 않는다 — free page 재사용으로 파일이 정상상태에 머무는 것이 목표이고
	// 200MiB급 VACUUM은 기동 지연이 크다. 어느 실패든 기동을 막지 않는다(로그만 남긴다): 행 삭제가
	// 실패하면 다음 기동이 같은 배치를 다시 시도하고, 커밋 뒤 파일 회수에서 유예·실패한 몫은 행이
	// 이미 없어 다음 기동 술어에 잡히지 않으므로 purge --gc가 회수한다(reclaimHookBlobs 계약).
	//
	// 동시성(st를 요청 핸들러들과 공유한다): Store는 가변 상태가 없고(dir + *sql.DB 3개) *sql.DB는
	// 다중 고루틴 사용이 계약이다. 쓰기는 writer SetMaxOpenConns(1)로 직렬화돼 이 tx와 핸들러의
	// Register tx가 섞이지 않으며(뒤엣것이 풀에서 대기), 프로세스 간 BUSY는 txRetry+busy_timeout이
	// 흡수한다. 직렬화 밖인 blob 물리 삭제는 rename 격리 프로토콜이 Register.writeBlob 경합을 닫고
	// 의심되면(mtime 1h 이내·참조 재확인 실패) 삭제 대신 유예한다.
	//
	// 잠금 보유자는 프로세스 안팎을 함께 봐야 한다: lockStore를 잡는 코드는 store.Open(writable)·
	// GCOrphanBlobs·reclaimHookBlobs이고, 이 서버 프로세스에서 그것을 부르는 것은 이 고루틴뿐이다
	// (internal/mcp는 셋 다 부르지 않아 요청 경로가 막히지 않는다). **다른 프로세스**로는 CLI purge
	// 계열과 훅이 있는데, 훅의 Read 가드는 총예산 2000ms 안에서 writable store.OpenContext를 하고
	// 실패하면 색인을 포기해 가드를 조용히 통과시킨다(internal/hook guardRead의 guard-store drop).
	// 그래서 이 배치의 잠금·쓰기 락 보유 시간은 그 예산의 일부만 쓰도록 건수로 묶는다
	// (startupPurgeMaxHashes 주석의 실측 근거).
	cutoff := store.ShadowCutoff(time.Now(), store.ShadowRetention(os.Getenv))
	purgeCtx, cancelPurge := context.WithTimeout(ctx, startupPurgeBudget)
	purgeDone := make(chan struct{})
	go func() {
		defer close(purgeDone)
		if cutoff <= 0 {
			// 보존 기간이 epoch 경과분(약 56년)보다 크면 cutoff가 0 이하로 내려가는데, store는 그 값을
			// "나이 필터 없음"으로 읽는다(shadowOwnedFilter) — 보존을 늘리려는 설정이 나이 무관 전량
			// 삭제로 반전된다. 클램프하면 그 반전은 막아도 무의미한 배치를 한 번 돌리므로(쓰기 락을
			// 잡는다) 회수 자체를 건너뛴다. 그 설정의 의도된 결과와도 같다 — 적격 대상이 없다.
			// 경계 자체는 store.ShadowCutoff의 표 테스트(TestShadowCutoffBoundary, internal/store)가 고정한다.
			return
		}
		rep, purgeErr := st.PurgeHookOnlyOlderThan(purgeCtx, cutoff, startupPurgeMaxHashes)
		// 결과 분류를 오류 모양이 아니라 ctx의 종료 사유로 하는 이유: 예산 소진의 관측된 모양은
		// context.DeadlineExceeded였지만 드라이버가 문장 중간 취소를 sqlite3_interrupt로 처리하므로
		// SQLITE_INTERRUPT로 올라올 수도 있고, D74부터 파일 회수 단계도 자체적으로 오류를 반환한다
		// (store.reclaimHookBlobs, 루프 선두 ctx.Err()). 그래서 이 오류 하나만으로는 행 삭제 tx가
		// 커밋됐는지 알 수 없다 — rep.Hashes(store.purgeHookRows는 실패 시 항상 빈 리포트를 반환하므로
		// >0이면 커밋이 확정)로 아래에서 그 둘을 구분한다. 한 번만 읽어 모든 분기가 같은 값을 보게
		// 한다 — ctx.Err()는 최초 종료 사유로 latch되므로 취소가 deadline을 덮어쓰지 못하고, 취소는
		// 종료 경로만 의미한다(시그널 또는 아래 defer의 cancelPurge).
		purgeCtxErr := purgeCtx.Err()
		budgetSpent := errors.Is(purgeCtxErr, context.DeadlineExceeded)
		switch {
		case purgeErr != nil && rep.Hashes > 0:
			// F1 확장(G2 — 최종 게이트 리뷰): rep.Hashes>0이면 행 삭제 tx는 이미 커밋되었고
			// (store.purgeHookRows는 실패 시 항상 빈 리포트를 반환하므로 이 값 자체가 커밋의
			// 증거다) 이후 파일 회수 단계에서 실패했다는 뜻이다 — 원인은 예산 소진·서버 종료
			// 취소·lockStoreCtx 실패 등 무엇이든 상관없다. 행이 이미 삭제돼 다음 기동 술어에
			// 다시 안 잡히므로 남은 파일의 유일한 회수 경로는 purge --gc다 — 이 case를 아래
			// 종료-취소 case보다 먼저 두는 이유가 그것이다: 순서가 뒤집히면 종료 취소로 중단된
			// 커밋-후 실패까지 "남은 배치는 다음 기동이 다시 집는다"(행이 이미 없어 거짓인
			// 안내)로 흡수된다. D67 튜닝 입력이 소실되지 않도록 카운트를 그대로 싣고,
			// budget_spent로 예산 소진과 종료 취소를 구분한다(ctx.Err()는 단일 값이라 둘은
			// 배타적이다).
			slog.Warn("기동 shadow 회수 중단 — 행 삭제 완료, 파일 회수 잔여분은 purge --gc",
				"hashes", rep.Hashes, "bytes", rep.ReclaimedB, "deferred", rep.DeferredFiles,
				"capped", rep.Hashes == startupPurgeMaxHashes, "budget_spent", budgetSpent)
		case purgeErr != nil && errors.Is(purgeCtxErr, context.Canceled):
			// 종료로 중단된 것은 실패가 아니다 — 위 case가 커밋된 사례(rep.Hashes>0)를 먼저
			// 가져가므로 이 분기는 행 삭제 tx 자체가 커밋 전에 취소된 rep.Hashes==0 사례만
			// 남는다(그래서 "남은 배치는 다음 기동이 그대로 다시 집는다"가 참이다). stdio
			// 서버는 호스트가 stdin을 닫으면 mcp.Serve가 반환하므로(시그널 없이도) 이 경로를
			// 탄다.
			slog.Debug("기동 shadow 회수 중단 — 서버 종료", "error", purgeErr)
		case purgeErr != nil && budgetSpent:
			// 예산은 폭주 가드이므로 소진은 정상 경로가 아니다 — 경고로 올린다. 건수는 싣지 않는다:
			// 위 case가 커밋된 사례를 먼저 가져가므로 이 분기는 rep.Hashes==0(행 삭제 tx 자체가
			// 예산에 걸려 커밋 전 롤백, store.purgeHookRows)만 남는다. 남은 파일은 참조 재확인이
			// 실패하면 보수적으로 유예되므로(stillReferenced가 오류를 "참조 있음"으로 취급) 오삭제는
			// 없다.
			slog.Warn("기동 shadow 회수 예산 소진 — 행 삭제 미완료, 다음 기동에서 재시도")
		case purgeErr != nil:
			// rep.Hashes==0인 행 삭제 자체의 실패만 이 분기로 온다 — 커밋 뒤 실패(파일 회수
			// 실패 포함, 원인 무관)는 맨 위 case가 전부 먼저 가져간다. 다음 기동이 같은 배치를
			// 다시 집는다.
			slog.Warn("기동 shadow 회수 실패 — 다음 기동에서 재시도", "error", purgeErr)
		case rep.Hashes > 0:
			// capped·budget_spent: 어느 예산이 이번 배치를 끊었는지 관측용(D67 임계값 재설정 입력).
			slog.Info("기동 shadow 회수", "hashes", rep.Hashes, "bytes", rep.ReclaimedB,
				"deferred", rep.DeferredFiles, "capped", rep.Hashes == startupPurgeMaxHashes,
				"budget_spent", budgetSpent)
		}
	}()
	// 회수를 st.Close()·sessDB.Close()보다 먼저 끝낸다(defer LIFO — 이 defer가 그 둘보다 나중에
	// 등록되어 먼저 돈다): 닫힌 DB 접근과 rename 격리 중간 상태(*.purging)로 프로세스가 끝나는 것을
	// 막는다. D74부터 루프가 매 hash 선두에서 ctx.Err()를 관측하고 lockStoreCtx도 잠금 대기 중
	// ctx를 함께 관측하므로(store.go:67), 취소 뒤 남는 대기는 이미 진행 중인 단일 hash 처리분뿐이다
	// — 더 이상 잔여 배치 전체(≤startupPurgeMaxHashes)나 잠금 대기 최대 5초를 기다리지 않는다.
	// 고루틴이 프로세스보다 오래 살지 않는다.
	defer func() { cancelPurge(); <-purgeDone }()

	// D102 계약 2: 병합은 퍼지와 별개 고루틴이고 서버 생존 ctx에 묶인다 — 퍼지의 60초 예산도
	// purgeErr도 cutoff<=0 건너뛰기도 공유하지 않는다(그 셋 중 어느 것에 걸려도 병합이 영영
	// 안 도는 구간이 생긴다). defer 등록이 st.Close()(636행)보다 뒤라 LIFO로 먼저 돌아, 고루틴이
	// 닫힌 DB를 만지거나 프로세스보다 오래 사는 일이 없다.
	mergeCtx, cancelMerge := context.WithCancel(ctx)
	mergeDone := make(chan struct{})
	go func() {
		defer close(mergeDone)
		runFTSMergeLoop(mergeCtx, st, ftsMergeStartDelay, defaultFTSMergeInterval)
	}()
	defer func() { cancelMerge(); <-mergeDone }()
	return mcp.Serve(ctx, mcp.Config{
		Canon: canon, Store: st, SelfExe: selfExe, ScratchRoot: scratchRoot,
		Profile: f.Profile, Enable: f.Enable, AllowPaths: allowPaths,
		NetAllowLocal: f.NetAllowLocal, NetPorts: f.NetPorts,
		Session: sessDB,
	})
}

// transformWorkerArg: worker 프로세스 재실행 숨김 모드 인자(설계 §4.3). 플래그 파싱보다
// 먼저 분기해야 한다 — stdout은 Result JSON 1건이어야 하고 배너·로그가 섞이면 안 된다.
const transformWorkerArg = "__transform-worker"

// execLauncherArg: sandbox 런처 재실행 숨김 모드 인자(설계 v0.11 D59). Linux는 landlock을
// 자식 자신이 걸어야 해 `__exec-launcher <scratch> -- <argv...>`로 재실행한다 — 이 분기가
// 제한을 건 뒤 syscall.Exec로 실제 argv로 대체한다. transformWorkerArg와 마찬가지로 플래그
// 파싱보다 먼저 분기해야 한다.
const execLauncherArg = "__exec-launcher"

// cliSubcommands: internal/cli가 처리하는 서브커맨드 이름(설계 §7). 이 중 하나가 아닌 첫
// 인자는 dispatchCLI의 관심사가 아니다 — MCP 서버 플래그로 그대로 흘려보낸다. "session"은
// v0.1 태스크9 추가(§6.3·§7) — export(9a)·recover(9b) 두 하위 서브커맨드를 cli.Run이 내부
// 디스패치한다(이 맵은 최상위 이름 1개만 안다, T4-plan3 미지 서브커맨드 MCP 오기동 차단 정합).
// "hook"은 v0.2 추가(설계 §2) — Claude Code 훅 서브프로세스(stdin JSON 1건→cc: 세션 append).
// "usage"는 v0.2 추가(설계 §6) — 로컬 transcript 세션별 토큰 집계 + cc: 스트림 대조(읽기 전용).
// "codex-hook"은 v0.4 추가(설계 §2 D35) — Codex 러닝 훅 전용, 구버전 바이너리 오귀속 차단 게이트(§11.2 F3).
var cliSubcommands = map[string]bool{"doctor": true, "stats": true, "purge": true, "upgrade": true, "session": true, "hook": true, "usage": true, "codex-hook": true, "version": true}

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
	// version은 CI/패키징 메타데이터 명령 — cwd/store-root/env 해석 이전에 조기 처리한다(F1).
	// 최소 환경(삭제된 cwd·미설정 HOME/LOCALAPPDATA)에서도 storeRootFor 실패로 죽지 않고 버전만
	// 출력해야 한다. cli.Run "version" 케이스는 storeRoot/projDir을 쓰지 않으므로(내부: len(args)
	// 검증 + version 1줄 출력뿐) 빈 값을 넘겨 출력·잉여 인자 검증을 단일 소스로 재사용한다.
	if sub == "version" {
		return true, cli.Run(ctx, sub, args[2:], "", "", version, os.Stdout, os.Stderr)
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
	// storeRootRaw는 여기서 끝난다 — 명시 여부와 원시값을 cli까지 나르던 이유가 `hook install`의
	// 훅 명령 --store-root 주입 하나였고, 그 등록 경로가 v0.19에서 사라졌다(D96). 정규화된
	// storeRoot만 넘긴다.
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

	if len(os.Args) > 1 && os.Args[1] == execLauncherArg {
		if err := sandbox.RunLauncher(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctr:", err)
			os.Exit(1)
		}
		return // 성공 시 syscall.Exec로 대체되어 도달 안 함(darwin/other는 오류)
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
