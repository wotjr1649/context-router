// Package mcp — 도구 스키마·등록·핸들러·오류 변환. 설계서 §4.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wotjr1649/context-router/internal/buildinfo"
	"github.com/wotjr1649/context-router/internal/exec"
	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/netfetch"
	"github.com/wotjr1649/context-router/internal/sandbox"
	"github.com/wotjr1649/context-router/internal/search"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

// Config — Serve/NewServer 입력 (설계 §4, §8).
type Config struct {
	Canon         ident.Canon
	Store         *store.Store
	SelfExe       string   // transform worker 재실행 경로(os.Executable(), §4.3) — 격리 프로브·Spawn에 사용
	ScratchRoot   string   // exec 스크래치 부모(OS temp 하위, D58) — T6이 main.go에서 채운다. 빈 값이면 exec.Run이 ErrSetup
	Profile       []string // 예약: transform/global-search 게이팅용 — v0.0.1은 미분기(§8)
	Enable        []string // opt-in: "ingest"·"net"·"exec"
	AllowPaths    []string // 이미 canonicalize된 ctr_index 허용 root (cmd가 검증 — §4.4)
	NetAllowLocal bool     // --net-allow-local (§4.5, ctr_fetch_and_index)
	NetPorts      []int    // --net-ports 추가 허용 포트 (§4.5)
	// Session: 이미 연 worktree Session DB(T2 session.Open). nil이면 ctr_record_event를
	// 등록하지 않는다(Enable 플래그 불요 — nil 자체가 게이팅 신호, main.go 배선은 T10).
	Session *session.DB
	// TransformTimeout: ctr_transform 핸들러가 Spawn에 씌우는 마감(0이면 NewServer가
	// 10s로 채운다). transform.go의 defaultWorkerTimeout 안전망(호출자 ctx에 deadline이
	// 없을 때만 적용)과 별개로, registerTransform은 항상 이 값으로 WithTimeout한다.
	TransformTimeout time.Duration
}

// Serve builds the tool server per cfg and runs it over stdio until ctx가 끝나거나
// stdin이 닫힐 때까지 블록한다.
func Serve(ctx context.Context, cfg Config) error {
	srv, err := NewServer(cfg)
	if err != nil {
		return err
	}
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// NewServer builds the server and registers tools per cfg.Enable(서빙 없음 — 테스트용 분리).
func NewServer(cfg Config) (*mcp.Server, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("mcp: nil store")
	}
	if cfg.TransformTimeout == 0 {
		cfg.TransformTimeout = 10 * time.Second
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "context-router", Version: buildinfo.ProductVersion()}, nil)
	// 경로 허용(ingest root)·상대화(search/fetch relativize) 기준 = WorktreeRoot — linked git
	// worktree에서 ProjectRoot(주 checkout)를 쓰면 현재 worktree 파일이 WORKSPACE_VIOLATION이
	// 된다(저장소 디렉터리 명명 ProjectID는 ProjectRoot 기반 그대로, main.go 참조).
	registerSearch(srv, cfg.Store, cfg.Canon.WorktreeRoot, cfg.Session)
	registerFetch(srv, cfg.Store, cfg.Canon.WorktreeRoot)
	// ProbeIsolation: OS 메모리 격리가 안 되는 환경에서는 ctr_transform 자체를 미등록한다
	// (in-process fallback 금지, 설계 §4.3/§5.3) — 첫 실제 호출에서야 실패를 알리지 않는다.
	if err := transform.ProbeIsolation(cfg.SelfExe); err != nil {
		slog.Warn("mcp: transform 격리 프로브 실패 — ctr_transform 비활성화", "error", err)
	} else {
		registerTransform(srv, cfg.Store, cfg.SelfExe, cfg.TransformTimeout)
	}
	if slices.Contains(cfg.Enable, "ingest") {
		registerIndex(srv, cfg.Store, cfg.Canon.WorktreeRoot, cfg.AllowPaths)
	}
	if slices.Contains(cfg.Enable, "net") {
		registerFetchAndIndex(srv, cfg.Store, cfg.NetAllowLocal, cfg.NetPorts)
	}
	// exec: 삼중 게이트 중 서버측 2개 — Enable "exec"(프로필) + sandbox.Probe(OS 격리 가능
	// 여부, transform ProbeIsolation 선례와 동형). 프로브 실패 시 slog.Warn + 미등록(첫 실제
	// 호출에서야 실패를 알리지 않는다, fail-closed).
	if slices.Contains(cfg.Enable, "exec") {
		if err := sandbox.Probe(context.Background()); err != nil {
			slog.Warn("mcp: exec 격리 프로브 실패 — ctr_execute 비활성화", "error", err)
		} else {
			registerExecute(srv, cfg.Store, cfg.ScratchRoot, cfg.SelfExe, cfg.Canon.WorktreeRoot)
		}
	}
	if cfg.Session != nil {
		registerRecordEvent(srv, cfg.Store, cfg.Session)
		registerSessionSummary(srv, cfg.Store, cfg.Session)
		registerExportEvents(srv, cfg.Store, cfg.Session)
	}
	return srv, nil
}

// --- 오류 변환 (규약 §6: mcp의 toToolError 하나가 sentinel→코드 매핑을 전담) ---

const (
	codeInvalidArgument     = "INVALID_ARGUMENT"
	codeNotFound            = "NOT_FOUND"
	codeWorkspaceViolation  = "WORKSPACE_VIOLATION"
	codeUnsupportedFile     = "UNSUPPORTED_FILE"
	codeStorageUnavailable  = "STORAGE_UNAVAILABLE"
	codeBudgetExceeded      = "BUDGET_EXCEEDED"
	codeOutputLimitExceeded = "OUTPUT_LIMIT_EXCEEDED"
	codeNetworkDenied       = "NETWORK_DENIED"
	codeInternal            = "INTERNAL"
)

func toolErr(code, msg string) error { return fmt.Errorf("[%s] %s", code, msg) }

// toToolError: sentinel→MCP 코드 단일 변환 지점. 매핑 없는 오류는 INTERNAL로 뭉개고
// 상세는 stderr slog에만 남긴다(원문·절대경로는 이미 생성 시점에 위생 처리됨, §6).
func toToolError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err // SDK가 취소/데드라인을 직접 처리하도록 원본 그대로 반환(§6, INTERNAL/slog 소음 방지)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return toolErr(codeNotFound, "대상을 찾을 수 없습니다")
	case errors.Is(err, store.ErrInvalidSelector):
		return toolErr(codeInvalidArgument, "잘못된 선택자입니다")
	case errors.Is(err, store.ErrUnavailable):
		return toolErr(codeStorageUnavailable, "저장소를 사용할 수 없습니다")
	case errors.Is(err, session.ErrLeaseHeld), errors.Is(err, session.ErrRecoverPending), errors.Is(err, session.ErrCorrupt):
		return toolErr(codeStorageUnavailable, "세션 저장소를 사용할 수 없습니다")
	case errors.Is(err, transform.ErrNoIsolation):
		return toolErr(codeStorageUnavailable, "격리 실행을 사용할 수 없습니다")
	case errors.Is(err, transform.ErrBudget):
		return toolErr(codeBudgetExceeded, "실행 스텝 상한을 초과했습니다")
	case errors.Is(err, transform.ErrOutputLimit):
		return toolErr(codeOutputLimitExceeded, "출력 크기 상한을 초과했습니다")
	case errors.Is(err, ingest.ErrWorkspace):
		return toolErr(codeWorkspaceViolation, "작업 영역 밖 경로입니다")
	case errors.Is(err, ingest.ErrUnsupported):
		return toolErr(codeUnsupportedFile, "지원하지 않는 파일입니다")
	case errors.Is(err, netfetch.ErrDenied):
		return toolErr(codeNetworkDenied, "네트워크 목적지가 거부되었습니다")
	case errors.Is(err, netfetch.ErrBodyTooLarge):
		return toolErr(codeOutputLimitExceeded, "응답 본문이 상한을 초과했습니다")
	case errors.Is(err, netfetch.ErrTooManyRedirects):
		return toolErr(codeNetworkDenied, "리다이렉트 상한을 초과했습니다")
	case errors.Is(err, netfetch.ErrUnsupportedMedia):
		return toolErr(codeUnsupportedFile, "지원하지 않는 미디어 타입입니다")
	case errors.Is(err, fs.ErrNotExist):
		return toolErr(codeNotFound, "대상을 찾을 수 없습니다")
	case errors.Is(err, exec.ErrUnsupportedLang), errors.Is(err, exec.ErrInvalidPath):
		return toolErr(codeInvalidArgument, "잘못된 실행 요청입니다")
	case errors.Is(err, exec.ErrToolchainMissing), errors.Is(err, exec.ErrVersionGate):
		return toolErr(codeUnsupportedFile, "요청한 실행 환경을 사용할 수 없습니다")
	case errors.Is(err, sandbox.ErrSetup):
		return toolErr(codeStorageUnavailable, "격리 실행을 준비할 수 없습니다")
	default:
		slog.Error("mcp: internal tool error", "error", err)
		return toolErr(codeInternal, "내부 오류가 발생했습니다")
	}
}

// jsonLen: out을 마샬링한 바이트 길이(ledger bytes_returned 근사치 — 실패 시 0).
func jsonLen(out any) int64 {
	b, err := json.Marshal(out)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// --- ctr_search (설계 §4.1) ---

type SearchInput struct {
	Queries        []string `json:"queries" jsonschema:"검색 질의 1~8개"`
	Limit          int      `json:"limit,omitempty" jsonschema:"질의당 최대 히트 수, 기본 3, 최대 10"`
	MaxReturnBytes int      `json:"max_return_bytes,omitempty" jsonschema:"스니펫 바이트 예산, 기본 8192"`
	Scope          string   `json:"scope,omitempty" jsonschema:"검색 범위 content(기본)|events|all — events/all은 세션 이벤트 포함, 세션 저장소 불용 시 오류"`
}

type searchHit struct {
	ArtifactID        int64   `json:"artifact_id"`
	ChunkID           int64   `json:"chunk_id"`
	Source            string  `json:"source"`
	LineStart         int     `json:"line_start"`
	LineEnd           int     `json:"line_end"`
	Snippet           string  `json:"snippet"`
	Score             float64 `json:"score"`
	Stale             bool    `json:"stale"`
	Redacted          bool    `json:"redacted"`
	SourceCoordsExact bool    `json:"source_coords_exact"`
}

// eventHit — search.EventHit의 mcp wire 표현(snake_case, 설계 §3.4). 필드 그대로 직렬화 —
// EventID/SessionID/EventType/TS/Summary/Superseded.
type eventHit struct {
	EventID    string `json:"event_id"`
	SessionID  string `json:"session_id"`
	EventType  string `json:"event_type"`
	TS         int64  `json:"ts"`
	Summary    string `json:"summary"`
	Superseded bool   `json:"superseded"`
}

type searchQueryResult struct {
	Query     string      `json:"query"`
	Hits      []searchHit `json:"hits"`
	Events    []eventHit  `json:"events,omitempty"`
	Truncated bool        `json:"truncated"`
}

type SearchOutput struct {
	Results   []searchQueryResult `json:"results"`
	Untrusted bool                `json:"untrusted"`
}

func toSearchHit(h search.Hit) searchHit {
	return searchHit{
		ArtifactID: h.ArtifactID, ChunkID: h.ChunkID, Source: h.Source,
		LineStart: h.LineStart, LineEnd: h.LineEnd, Snippet: h.Snippet,
		Score: h.Score, Stale: h.Stale, Redacted: h.Redacted,
		SourceCoordsExact: h.SourceCoordsExact,
	}
}

func toEventHit(h search.EventHit) eventHit {
	return eventHit{
		EventID: h.EventID, SessionID: h.SessionID, EventType: h.EventType,
		TS: h.TS, Summary: h.Summary, Superseded: h.Superseded,
	}
}

const (
	scopeContent = "content"
	scopeEvents  = "events"
	scopeAll     = "all"
)

// normalizeSearchScope: 생략("")은 content(후방 호환, 설계 §3.4). 그 외 미지 값은 INVALID_ARGUMENT
// (신규 오류 코드 없음 — 기존 코드 재사용).
func normalizeSearchScope(s string) (string, error) {
	switch s {
	case "":
		return scopeContent, nil
	case scopeContent, scopeEvents, scopeAll:
		return s, nil
	default:
		return "", toolErr(codeInvalidArgument, "scope는 content|events|all 중 하나여야 합니다")
	}
}

// queryEventSection: in.Queries 각각에 search.QueryEvents(porter+trigram RRF, content과 동일
// 패턴)를 호출해 wire 타입으로 변환한다. sess는 호출 전 nil이 아님이 registerSearch에서 이미
// 보장된다(scope!=content 진입 조건). C3(Codex P2): budget(content 소진 후 남은 바이트) 안에서
// 이벤트 summary 바이트를 앞에서부터 채우고, 넘치면 그 질의를 truncated로 표시한다 — content과
// events가 하나의 예산을 공유한다(공유 pool, 질의 순서대로 소진). 측정 단위는 content
// applyBudget의 snippet-only 관례와 동형(summary 길이). budget이 소진돼도 오류는 아니다.
func queryEventSection(ctx context.Context, sess *session.DB, queries []string, limit, budget int) ([][]eventHit, []bool, error) {
	out := make([][]eventHit, len(queries))
	truncated := make([]bool, len(queries))
	remaining := budget
	for i, q := range queries {
		hits, err := search.QueryEvents(ctx, sess.Reader(), q, limit)
		if err != nil {
			return nil, nil, err
		}
		ev := make([]eventHit, 0, len(hits))
		for _, h := range hits {
			e := toEventHit(h)
			if len(e.Summary) > remaining {
				truncated[i] = true
				break
			}
			ev = append(ev, e)
			remaining -= len(e.Summary)
		}
		out[i] = ev
	}
	return out, truncated, nil
}

// contentBudgetUsed: content 검색이 실제 소진한 스니펫 바이트 합(search.applyBudget과 동일 단위).
// events 섹션이 content와 예산을 공유하도록 남은 예산 계산에 쓴다(C3). qrs가 nil(scope=events)
// 이면 0 — events가 전체 예산을 받는다.
func contentBudgetUsed(qrs []search.QueryResult) int {
	used := 0
	for _, qr := range qrs {
		for _, h := range qr.Hits {
			used += len(h.Snippet)
		}
	}
	return used
}

// buildSearchOutput: content 결과(qrs, scope=events면 nil)와 이벤트 결과(evs, scope=content면
// nil)를 질의별로 합친다. Hits는 기존 계약대로 항상 빈 슬라이스 이상(null 금지), Events는
// omitempty라 비어 있으면 생략된다.
func buildSearchOutput(queries []string, qrs []search.QueryResult, evs [][]eventHit, evTrunc []bool) SearchOutput {
	out := SearchOutput{Untrusted: true, Results: make([]searchQueryResult, len(queries))}
	for i, q := range queries {
		r := searchQueryResult{Query: q, Hits: []searchHit{}}
		if qrs != nil {
			hits := make([]searchHit, len(qrs[i].Hits))
			for j, h := range qrs[i].Hits {
				hits[j] = toSearchHit(h)
			}
			r.Hits, r.Truncated = hits, qrs[i].Truncated
		}
		if evs != nil {
			r.Events = evs[i]
			if evTrunc[i] { // C3: 이벤트 절단도 기존 content truncated 관례로 동일 신호에 합류
				r.Truncated = true
			}
		}
		out.Results[i] = r
	}
	return out
}

func registerSearch(srv *mcp.Server, st *store.Store, worktreeRoot string, sess *session.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ctr_search",
		Description: "프로젝트 색인을 BM25+RRF로 검색해 스니펫을 반환한다. scope로 content(기본)/" +
			"events/all을 선택한다 — events/all은 세션 이벤트도 함께 검색한다(세션 저장소 불용 시 STORAGE_UNAVAILABLE).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
		start := time.Now()
		if len(in.Queries) < 1 || len(in.Queries) > 8 {
			return nil, SearchOutput{}, toolErr(codeInvalidArgument, "queries는 1~8개여야 합니다")
		}
		scope, err := normalizeSearchScope(in.Scope)
		if err != nil {
			return nil, SearchOutput{}, err
		}
		if scope != scopeContent && sess == nil {
			return nil, SearchOutput{}, toolErr(codeStorageUnavailable, "세션 저장소를 사용할 수 없습니다")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 3
		} else if limit > 10 {
			limit = 10
		}
		budget := in.MaxReturnBytes
		if budget <= 0 {
			budget = 8192
		}
		var qrs []search.QueryResult
		if scope != scopeEvents {
			if qrs, err = search.Query(ctx, st, worktreeRoot, in.Queries, limit, budget); err != nil {
				return nil, SearchOutput{}, toToolError(err)
			}
		}
		var evs [][]eventHit
		var evTrunc []bool
		if scope != scopeContent {
			// C3: content가 소진하고 남은 예산을 events가 이어받는다(결합 예산).
			eventsBudget := max(0, budget-contentBudgetUsed(qrs))
			if evs, evTrunc, err = queryEventSection(ctx, sess, in.Queries, limit, eventsBudget); err != nil {
				return nil, SearchOutput{}, toToolError(session.ClassifyStorageErr(err)) // C2: 런타임 훼손 → STORAGE_UNAVAILABLE
			}
		}
		out := buildSearchOutput(in.Queries, qrs, evs, evTrunc)
		st.LedgerAppend("ctr_search", 0, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}

// --- ctr_fetch (설계 §4.2) ---

type FetchInput struct {
	ArtifactID     int64  `json:"artifact_id" jsonschema:"artifact ID"`
	ChunkID        *int64 `json:"chunk_id,omitempty" jsonschema:"청크 선택자"`
	LineStart      *int   `json:"line_start,omitempty" jsonschema:"라인 선택자 시작(1-기반, 포함)"`
	LineEnd        *int   `json:"line_end,omitempty" jsonschema:"라인 선택자 끝(포함)"`
	ByteStart      *int64 `json:"byte_start,omitempty" jsonschema:"바이트 선택자 시작"`
	ByteEnd        *int64 `json:"byte_end,omitempty" jsonschema:"바이트 선택자 끝"`
	MaxReturnBytes int    `json:"max_return_bytes,omitempty" jsonschema:"기본 16384, 최대 65536"`
}

// fetchProvenance: 설계 §4.2 provenance 전체 계약. src_hash/source/source_kind/stale은
// store.ReadRange가 채운 RangeResult.Source/HasSource/Stale에서 온다 — HasSource=false면
// (소스 없는 artifact) 4필드 모두 zero value(""/false)로 남긴다.
type fetchProvenance struct {
	ContentHash string `json:"content_hash"`
	Redaction   string `json:"redaction"`
	CreatedAt   int64  `json:"created_at"`
	SrcHash     string `json:"src_hash"`
	Source      string `json:"source"`
	SourceKind  string `json:"source_kind"`
	Stale       bool   `json:"stale"`
}

type FetchOutput struct {
	Content           string          `json:"content"`
	ByteStart         int64           `json:"byte_start"`
	ByteEnd           int64           `json:"byte_end"`
	LineStart         int             `json:"line_start"`
	LineEnd           int             `json:"line_end"`
	Truncated         bool            `json:"truncated"`
	ExactScope        string          `json:"exact_scope"`
	Representation    string          `json:"representation"`
	SourceCoordsExact bool            `json:"source_coords_exact"`
	Provenance        fetchProvenance `json:"provenance"`
	Untrusted         bool            `json:"untrusted"`
}

// selectorFromInput: chunk_id | line_start&line_end | byte_start&byte_end 중 정확히
// 1조합만 허용한다(설계 §4.2) — 0개·2개 이상은 INVALID_ARGUMENT.
func selectorFromInput(in FetchInput) (store.Selector, error) {
	n := 0
	var sel store.Selector
	if in.ChunkID != nil {
		n++
		sel = store.Selector{Kind: "chunk", ChunkID: *in.ChunkID}
	}
	if in.LineStart != nil || in.LineEnd != nil {
		if in.LineStart == nil || in.LineEnd == nil {
			return store.Selector{}, toolErr(codeInvalidArgument, "line_start/line_end는 함께 지정해야 합니다")
		}
		n++
		sel = store.Selector{Kind: "line", LineStart: *in.LineStart, LineEnd: *in.LineEnd}
	}
	if in.ByteStart != nil || in.ByteEnd != nil {
		if in.ByteStart == nil || in.ByteEnd == nil {
			return store.Selector{}, toolErr(codeInvalidArgument, "byte_start/byte_end는 함께 지정해야 합니다")
		}
		n++
		sel = store.Selector{Kind: "byte", ByteStart: *in.ByteStart, ByteEnd: *in.ByteEnd}
	}
	if n != 1 {
		return store.Selector{}, toolErr(codeInvalidArgument, "선택자는 정확히 1개여야 합니다")
	}
	return sel, nil
}

// sourceCoordsExact: search 의미론(§4.0)과 통일 — file/inline이고 extraction을 거치지 않은
// 무편집 소스일 때만 좌표가 원문을 그대로 가리킨다.
func sourceCoordsExact(res store.RangeResult) bool {
	return res.HasSource && res.Source.Extraction == "" &&
		(res.Source.Kind == "file" || res.Source.Kind == "inline") &&
		res.Artifact.Redaction == "none"
}

// representationOf: sourceKind(res.Source.Kind)가 "inline"이면 최우선으로 "inline"을
// 반환한다. 그 외는 media_type 근사(markdown/file).
func representationOf(mediaType, sourceKind string) string {
	if sourceKind == "inline" {
		return "inline"
	}
	if mediaType == "text/markdown" {
		return "markdown"
	}
	return "file"
}

// applyFetchBudget: res.Text가 maxBytes를 넘으면 UTF-8 경계로 잘라 truncated=true를 표시하고
// byte_end/line_end를 실제 반환분에 맞춰 재계산한다(설계 §4.2 "실제 반환 범위").
func applyFetchBudget(res store.RangeResult, maxBytes int) (text []byte, byteEnd int64, lineEnd int, truncated bool) {
	if len(res.Text) <= maxBytes {
		return res.Text, res.ByteEnd, res.LineEnd, false
	}
	n := maxBytes
	for n > 0 && !utf8.RuneStart(res.Text[n]) {
		n--
	}
	cut := res.Text[:n]
	lineEnd = res.LineEnd
	if res.LineStart > 0 { // line/chunk 선택자만 라인 정보를 갖는다(byte는 0 유지)
		lineEnd = res.LineStart + strings.Count(string(cut), "\n")
		if len(cut) > 0 && cut[len(cut)-1] == '\n' {
			lineEnd-- // 개행으로 정확히 끝나면 다음 줄은 아직 포함되지 않음 — 과계산 보정
		}
	}
	return cut, res.ByteStart + int64(n), lineEnd, true
}

func registerFetch(srv *mcp.Server, st *store.Store, worktreeRoot string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ctr_fetch",
		Description: "artifact 저장본에서 선택자 범위를 그대로 회수한다 — 저장된 artifact의 " +
			"byte-exact 조회이며 웹 fetch가 아니다(웹은 ctr_fetch_and_index).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in FetchInput) (*mcp.CallToolResult, FetchOutput, error) {
		start := time.Now()
		sel, err := selectorFromInput(in)
		if err != nil {
			return nil, FetchOutput{}, err
		}
		res, err := st.ReadRange(ctx, in.ArtifactID, sel)
		if err != nil {
			return nil, FetchOutput{}, toToolError(err)
		}
		maxBytes := in.MaxReturnBytes
		if maxBytes <= 0 {
			maxBytes = 16384
		} else if maxBytes > 65536 {
			maxBytes = 65536
		}
		text, byteEnd, lineEnd, truncated := applyFetchBudget(res, maxBytes)
		prov := fetchProvenance{
			ContentHash: res.Artifact.ContentHash, Redaction: res.Artifact.Redaction,
			CreatedAt: res.Artifact.CreatedAt,
		}
		if res.HasSource {
			prov.SrcHash = res.Source.SrcHash
			prov.Source = search.RelativizeSource(worktreeRoot, res.Source.URI)
			prov.SourceKind = res.Source.Kind
			prov.Stale = res.Stale
		}
		out := FetchOutput{
			Content: string(text), ByteStart: res.ByteStart, ByteEnd: byteEnd,
			LineStart: res.LineStart, LineEnd: lineEnd, Truncated: truncated,
			ExactScope: "artifact", Representation: representationOf(res.Artifact.MediaType, res.Source.Kind),
			SourceCoordsExact: sourceCoordsExact(res),
			Provenance:        prov,
			Untrusted:         true,
		}
		st.LedgerAppend("ctr_fetch", 0, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}

// --- ctr_execute / ctr_execute_file (설계 v0.11 D58, Enable "exec" + 프로브 게이트) ---

type ExecuteInput struct {
	Language  string `json:"language" jsonschema:"실행 언어: shell|javascript|typescript|python|go|csharp"`
	Code      string `json:"code" jsonschema:"실행할 코드 — 파생 답만 print(대형 원문은 코드 내에서 집계·필터). 저장본의 무 I/O 변환은 ctr_transform"`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"실행 시간 상한(ms), 기본 120000, 최대 1800000"`
}

type ExecuteFileInput struct {
	Language  string `json:"language" jsonschema:"실행 언어: shell|javascript|typescript|python|go|csharp"`
	Path      string `json:"path" jsonschema:"CTR_FILE로 스니펫에 전달할 파일 경로(절대·존재)"`
	Code      string `json:"code" jsonschema:"CTR_FILE을 처리하는 코드"`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"실행 시간 상한(ms), 기본 120000, 최대 1800000"`
}

type ExecuteOutput struct {
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	ExitCode    *int   `json:"exit_code"` // timed_out 시 null (D61)
	TimedOut    bool   `json:"timed_out"`
	StdoutTrunc bool   `json:"stdout_truncated"`
	StderrTrunc bool   `json:"stderr_truncated"`
	DurationMS  int64  `json:"duration_ms"`
	Runner      string `json:"runner"`
}

func toExecuteOutput(r exec.Response) ExecuteOutput {
	return ExecuteOutput{
		Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode, TimedOut: r.TimedOut,
		StdoutTrunc: r.StdoutTrunc, StderrTrunc: r.StderrTrunc,
		DurationMS: r.DurationMS, Runner: r.Runner,
	}
}

// validateExecPath: 절대·존재·일반 파일 검증(원문 에코 없음, §6 — sentinel 문구만).
func validateExecPath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", exec.ErrInvalidPath
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return "", exec.ErrInvalidPath
	}
	return p, nil
}

func registerExecute(srv *mcp.Server, st *store.Store, scratchRoot, selfExe, worktreeRoot string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ctr_execute",
		Description: "샌드박스에서 코드를 실행하고 stdout만 반환한다(think-in-code — 큰 데이터는 " +
			"코드 안에서 집계해 파생 답만 print; 출력 초과 시 ctr_search 연계). 저장된 artifact의 " +
			"무 I/O·결정적 변환은 ctr_transform, 툴체인·파일시스템이 필요한 실행은 이 도구.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ExecuteInput) (*mcp.CallToolResult, ExecuteOutput, error) {
		start := time.Now()
		resp, err := exec.Run(ctx, scratchRoot, selfExe,
			exec.Request{Language: in.Language, Code: in.Code, WorktreeRoot: worktreeRoot, TimeoutMS: in.TimeoutMS})
		if err != nil {
			return nil, ExecuteOutput{}, toToolError(err)
		}
		// post-Start 부모 취소/데드라인: exec.Run이 nil 오류로 부분 출력을 돌려줘도 정상 결과로
		// 반환·원장 기록하지 않고 ctx.Err()를 그대로 전파한다(SDK가 취소 처리 — toToolError 최상단
		// passthrough와 동일 취급). 도구 자체 TimeoutMS(부모 ctx에 데드라인 없음)는 res.TimedOut로
		// 정상 결과가 되고 이 분기에 걸리지 않는다.
		if ctx.Err() != nil {
			return nil, ExecuteOutput{}, ctx.Err()
		}
		out := toExecuteOutput(resp)
		st.LedgerAppend("ctr_execute", 0, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
	// ctr_execute_file: path 검증 후 CTR_FILE로 전달(동일 계약).
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_execute_file",
		Description: "ctr_execute와 동일하되 CTR_FILE 환경변수로 파일 경로를 스니펫에 전달한다.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ExecuteFileInput) (*mcp.CallToolResult, ExecuteOutput, error) {
		start := time.Now()
		abs, verr := validateExecPath(in.Path)
		if verr != nil {
			return nil, ExecuteOutput{}, toToolError(verr)
		}
		resp, err := exec.Run(ctx, scratchRoot, selfExe,
			exec.Request{Language: in.Language, Code: in.Code, FilePath: abs, WorktreeRoot: worktreeRoot, TimeoutMS: in.TimeoutMS})
		if err != nil {
			return nil, ExecuteOutput{}, toToolError(err)
		}
		if ctx.Err() != nil {
			return nil, ExecuteOutput{}, ctx.Err()
		}
		out := toExecuteOutput(resp)
		st.LedgerAppend("ctr_execute_file", 0, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}

// --- ctr_index (설계 §4.4, Enable에 "ingest" 있을 때만 등록) ---

type IndexInput struct {
	Path         string   `json:"path,omitempty" jsonschema:"색인할 파일 또는 디렉터리 경로"`
	Content      string   `json:"content,omitempty" jsonschema:"인라인 콘텐츠(경로 대신 title과 함께 사용)"`
	Title        string   `json:"title,omitempty" jsonschema:"인라인 콘텐츠 제목"`
	Include      []string `json:"include,omitempty" jsonschema:"포함 glob 패턴"`
	Exclude      []string `json:"exclude,omitempty" jsonschema:"제외 glob 패턴"`
	MaxFileBytes int64    `json:"max_file_bytes,omitempty" jsonschema:"파일당 최대 바이트, 기본 5MB"`
}

type indexSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type IndexOutput struct {
	Indexed     int         `json:"indexed"`
	BytesStored int64       `json:"bytes_stored"`
	Skipped     []indexSkip `json:"skipped"`
}

// validateIndexInput: path/content는 XOR(둘 다 또는 둘 다 아님은 오류)이고, content를 쓸
// 때는 title이 필수다(설계 §4.4).
func validateIndexInput(in IndexInput) error {
	switch {
	case in.Path == "" && in.Content == "":
		return toolErr(codeInvalidArgument, "path 또는 content가 필요합니다")
	case in.Path != "" && in.Content != "":
		return toolErr(codeInvalidArgument, "path와 content는 동시에 지정할 수 없습니다")
	case in.Content != "" && in.Title == "":
		return toolErr(codeInvalidArgument, "content 지정 시 title이 필요합니다")
	}
	return nil
}

func registerIndex(srv *mcp.Server, st *store.Store, worktreeRoot string, allowPaths []string) {
	destructive := false
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_index",
		Description: "파일·디렉터리·인라인 텍스트를 색인에 등록한다.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in IndexInput) (*mcp.CallToolResult, IndexOutput, error) {
		start := time.Now()
		if err := validateIndexInput(in); err != nil {
			return nil, IndexOutput{}, err
		}
		rep, err := ingest.Run(ctx, st, worktreeRoot, allowPaths, ingest.Request{
			Path: in.Path, Content: in.Content, Title: in.Title,
			Include: in.Include, Exclude: in.Exclude, MaxFileBytes: in.MaxFileBytes,
		})
		if err != nil {
			return nil, IndexOutput{}, toToolError(err)
		}
		skipped := make([]indexSkip, len(rep.Skipped))
		for i, sk := range rep.Skipped {
			skipped[i] = indexSkip{Path: sk.Path, Reason: sk.Reason}
		}
		out := IndexOutput{Indexed: rep.Indexed, BytesStored: rep.BytesStored, Skipped: skipped}
		st.LedgerAppend("ctr_index", rep.BytesStored, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}

// --- ctr_transform (설계 §4.2.3, §4.3) ---

const (
	maxTransformScriptBytes = 64 * 1024
	maxTransformInputs      = 8
	maxTransformInputBytes  = 8 * 1024 * 1024
	maxTransformTotalBytes  = 16 * 1024 * 1024
	maxTransformOutputBytes = 262144
)

// transformDescription: starlark의 def 래핑 제약을 명시한다 — 모르면 자연스러운 top-level
// for/while/재귀 스크립트가 실패하므로 필수(T1/T2 승계 계약 (b)).
const transformDescription = "artifact 텍스트를 starlark 스크립트로 변환한다. " +
	"최상위 for는 def 함수 안에서 사용. while·재귀는 starlark 기본 설정상 지원 안 됨(def 안에서도). " +
	"예: def f(): ... 안에서 정의하고 호출해야 한다. inputs[i].text()/.lines()/.json(), args, emit(x)로 " +
	"출력한다. 내장: regex_extract/json_project/line_window/head/tail/count/sort/dedupe."

type TransformInput struct {
	Script         string            `json:"script" jsonschema:"starlark 스크립트, 최대 64KB"`
	Inputs         []int64           `json:"inputs,omitempty" jsonschema:"입력 artifact ID 목록, 최대 8개"`
	Args           map[string]string `json:"args,omitempty" jsonschema:"스크립트 args로 전달할 키/값"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"출력 상한, 기본 32768, 최대 262144"`
}

type TransformOutput struct {
	Result    string `json:"result"`
	StepsUsed int64  `json:"steps_used"`
	Truncated bool   `json:"truncated"`
}

// transformResultErr: Eval의 ErrKind→도구 오류. budget/output_limit은 toToolError의 sentinel
// 매핑을 그대로 재사용(§6 단일 지점), script는 ErrSummary가 이미 안전(원문·데이터 미포함).
func transformResultErr(res transform.Result) error {
	switch res.ErrKind {
	case "budget":
		return toToolError(transform.ErrBudget)
	case "output_limit":
		return toToolError(transform.ErrOutputLimit)
	case "script":
		return toolErr(codeInvalidArgument, res.ErrSummary)
	}
	return nil
}

func registerTransform(srv *mcp.Server, st *store.Store, selfExe string, timeout time.Duration) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_transform",
		Description: transformDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TransformInput) (*mcp.CallToolResult, TransformOutput, error) {
		start := time.Now()
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		if len(in.Script) > maxTransformScriptBytes {
			return nil, TransformOutput{}, toolErr(codeInvalidArgument, "script가 상한(64KB)을 초과했습니다")
		}
		if len(in.Inputs) > maxTransformInputs {
			return nil, TransformOutput{}, toolErr(codeInvalidArgument, "inputs는 최대 8개입니다")
		}
		if in.MaxOutputBytes > maxTransformOutputBytes {
			return nil, TransformOutput{}, toolErr(codeInvalidArgument, "max_output_bytes가 상한(262144)을 초과했습니다")
		}
		inputs := make([]transform.Input, len(in.Inputs))
		var total int64
		for i, id := range in.Inputs {
			text, err := st.ArtifactText(ctx, id, maxTransformInputBytes)
			if err != nil {
				return nil, TransformOutput{}, toToolError(err)
			}
			if total += int64(len(text)); total > maxTransformTotalBytes {
				return nil, TransformOutput{}, toolErr(codeInvalidArgument, "inputs 총합이 상한(16MB)을 초과했습니다")
			}
			inputs[i] = transform.Input{ID: id, Text: text}
		}
		res, err := transform.Spawn(ctx, selfExe, transform.Request{
			Script: in.Script, Inputs: inputs, Args: in.Args,
			Caps: transform.Caps{MaxOutputBytes: in.MaxOutputBytes}, // MaxSteps=0 → transform 기본 5_000_000
		})
		if err != nil {
			return nil, TransformOutput{}, toToolError(err)
		}
		if terr := transformResultErr(res); terr != nil {
			return nil, TransformOutput{}, terr
		}
		out := TransformOutput{Result: res.Output, StepsUsed: res.StepsUsed, Truncated: res.Truncated}
		st.LedgerAppend("ctr_transform", 0, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}

// --- ctr_fetch_and_index (설계 §4.5, Enable에 "net" 있을 때만 등록) ---

type FetchAndIndexInput struct {
	URL      string `json:"url" jsonschema:"가져올 URL(http/https)"`
	MaxBytes int64  `json:"max_bytes,omitempty" jsonschema:"응답 본문 상한 바이트, 기본 10MB"`
}

type FetchAndIndexOutput struct {
	ArtifactID    int64  `json:"artifact_id"`
	Title         string `json:"title"`
	ByteLength    int64  `json:"byte_length"`
	Extraction    string `json:"extraction"`
	IndexedChunks int    `json:"indexed_chunks"`
	Snippet       string `json:"snippet"`
	Untrusted     bool   `json:"untrusted"`
}

// registerFetchAndIndex: 핸들러의 구체 호출은 netfetch.Fetch→ingest.RunWeb 2개뿐(규약 §2 —
// mcp만 netfetch를 import하고, ingest에는 원시 인자로 배선해 ingest→netfetch 의존을 피한다).
func registerFetchAndIndex(srv *mcp.Server, st *store.Store, allowLocal bool, extraPorts []int) {
	destructive := false
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_fetch_and_index",
		Description: "URL을 SSRF 안전 정책으로 가져와 색인에 등록한다.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in FetchAndIndexInput) (*mcp.CallToolResult, FetchAndIndexOutput, error) {
		start := time.Now()
		if in.URL == "" {
			return nil, FetchAndIndexOutput{}, toolErr(codeInvalidArgument, "url이 필요합니다")
		}
		res, err := netfetch.Fetch(ctx, netfetch.Config{AllowLocal: allowLocal, ExtraPorts: extraPorts, MaxBytes: in.MaxBytes}, in.URL)
		if err != nil {
			return nil, FetchAndIndexOutput{}, toToolError(err)
		}
		rep, err := ingest.RunWeb(ctx, st, res.FinalURL, res.RawHTML, res.Body, res.MediaType, res.Extraction, res.Title)
		if err != nil {
			return nil, FetchAndIndexOutput{}, toToolError(err)
		}
		out := FetchAndIndexOutput{
			ArtifactID: rep.ArtifactID, Title: res.FinalURL, ByteLength: rep.ByteLength,
			Extraction: res.Extraction, IndexedChunks: rep.IndexedChunks, Snippet: rep.Snippet,
			Untrusted: true,
		}
		st.LedgerAppend("ctr_fetch_and_index", rep.ByteLength, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}

// --- ctr_global_search (설계 §4.6, §5.4, global-search 프로필 전용 등록) ---

// GlobalProject: global-search 프로필이 read-only로 여는 프로젝트 1개. Root가 ""면
// --projects에 경로가 아닌 ID 문자열을 준 경우다 — search.Query에 projectRoot=""를
// 넘기므로 Hit.Source가 project-relative화되지 못하고 절대경로로 반환된다(도구
// 설명에 명시, RelativizeSource 참조).
type GlobalProject struct {
	ID    string
	Root  string
	Store *store.Store
}

// GlobalConfig — NewGlobalServer/ServeGlobal 입력. store 열기/닫기는 호출자(cmd) 책임.
type GlobalConfig struct {
	Projects []GlobalProject
}

// globalHit: searchHit + project 라벨(설계 §4.6 "반환에 project 라벨 추가"). wire
// 타입은 mcp 소유(규약 §3) — search.Hit 등 internal 타입에는 태그를 얹지 않는다.
type globalHit struct {
	searchHit
	Project string `json:"project"`
}

type globalQueryResult struct {
	Query     string      `json:"query"`
	Hits      []globalHit `json:"hits"`
	Truncated bool        `json:"truncated"`
}

type GlobalSearchOutput struct {
	Results   []globalQueryResult `json:"results"`
	Untrusted bool                `json:"untrusted"`
}

const globalSearchDescription = "여러 프로젝트 색인을 read-only로 동시 검색해 project 라벨을 붙여 " +
	"병합 반환한다(BM25+RRF, score는 rank 기반이라 프로젝트 간 직접 비교 가능해 병합 정렬에 " +
	"쓴다). --projects에 경로 대신 ID 문자열로 지정된 프로젝트는 원본 대조·경로 상대화가 " +
	"불가해 source가 절대경로로 반환된다."

// mergeGlobalHits: 쿼리별로 모든 프로젝트의 hit에 project 라벨을 붙여 RRF score
// 내림차순(동점이면 project→artifact_id 오름차순, 결정적)으로 병합하고 limit으로
// 절단한다. truncated는 어느 한 프로젝트의 search.Query truncated거나 이 병합
// 절단이 발생하면 true(설계 §4.6 병합 계약).
func mergeGlobalHits(projects []GlobalProject, perProject [][]search.QueryResult, queries []string, limit int) []globalQueryResult {
	out := make([]globalQueryResult, len(queries))
	for qi, q := range queries {
		var hits []globalHit
		truncated := false
		for pi, qrs := range perProject {
			qr := qrs[qi]
			truncated = truncated || qr.Truncated
			for _, h := range qr.Hits {
				hits = append(hits, globalHit{searchHit: toSearchHit(h), Project: projects[pi].ID})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].Score != hits[j].Score {
				return hits[i].Score > hits[j].Score
			}
			if hits[i].Project != hits[j].Project {
				return hits[i].Project < hits[j].Project
			}
			return hits[i].ArtifactID < hits[j].ArtifactID
		})
		if len(hits) > limit {
			hits = hits[:limit]
			truncated = true
		}
		out[qi] = globalQueryResult{Query: q, Hits: hits, Truncated: truncated}
	}
	return out
}

func registerGlobalSearch(srv *mcp.Server, projects []GlobalProject) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_global_search",
		Description: globalSearchDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, GlobalSearchOutput, error) {
		if len(in.Queries) < 1 || len(in.Queries) > 8 {
			return nil, GlobalSearchOutput{}, toolErr(codeInvalidArgument, "queries는 1~8개여야 합니다")
		}
		// E1(fable): 공유 SearchInput의 scope는 global-search에서 미지원 — 조용히 무시하지 않고
		// 명시 거부한다(events/all은 세션 저장소 개념이 없는 global 표면에서 의미 없음).
		if in.Scope != "" {
			return nil, GlobalSearchOutput{}, toolErr(codeInvalidArgument, "global-search는 scope를 지원하지 않습니다")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 3
		} else if limit > 10 {
			limit = 10
		}
		budget := in.MaxReturnBytes
		if budget <= 0 {
			budget = 8192
		}
		perProject := make([][]search.QueryResult, len(projects))
		for i, p := range projects {
			qrs, err := search.Query(ctx, p.Store, p.Root, in.Queries, limit, budget)
			if err != nil {
				return nil, GlobalSearchOutput{}, toToolError(err)
			}
			perProject[i] = qrs
		}
		out := GlobalSearchOutput{Untrusted: true, Results: mergeGlobalHits(projects, perProject, in.Queries, limit)}
		return nil, out, nil
	})
}

// NewGlobalServer builds a global-search-only server: cfg.Projects는 이미 read-only로
// 연 상태로 받아 ctr_global_search 하나만 등록한다(설계 §4.6 금지 조항 — 다른 도구
// 절대 미등록).
func NewGlobalServer(cfg GlobalConfig) (*mcp.Server, error) {
	if len(cfg.Projects) == 0 {
		return nil, fmt.Errorf("mcp: global-search에는 최소 1개 프로젝트가 필요합니다")
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ctr-global", Version: buildinfo.ProductVersion()}, nil)
	registerGlobalSearch(srv, cfg.Projects)
	return srv, nil
}

// ServeGlobal builds the global-search server and runs it over stdio(Serve와 동형, 설계 §4.6).
func ServeGlobal(ctx context.Context, cfg GlobalConfig) error {
	srv, err := NewGlobalServer(cfg)
	if err != nil {
		return err
	}
	return srv.Run(ctx, &mcp.StdioTransport{})
}
