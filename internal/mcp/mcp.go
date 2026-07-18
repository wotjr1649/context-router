// Package mcp — 도구 스키마·등록·핸들러·오류 변환. 설계서 §4.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/netfetch"
	"github.com/wotjr1649/context-router/internal/search"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

const serverVersion = "0.0.1-dev"

// Config — Serve/NewServer 입력 (설계 §4, §8).
type Config struct {
	Canon         ident.Canon
	Store         *store.Store
	SelfExe       string   // transform worker 재실행 경로(os.Executable(), §4.3) — 격리 프로브·Spawn에 사용
	Profile       []string // 예약: transform/global-search 게이팅용 — v0.0.1은 미분기(§8)
	Enable        []string // opt-in: "ingest"·"net"
	AllowPaths    []string // 이미 canonicalize된 ctr_index 허용 root (cmd가 검증 — §4.4)
	NetAllowLocal bool     // --net-allow-local (§4.5, ctr_fetch_and_index)
	NetPorts      []int    // --net-ports 추가 허용 포트 (§4.5)
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
	srv := mcp.NewServer(&mcp.Implementation{Name: "context-router", Version: serverVersion}, nil)
	// 경로 허용(ingest root)·상대화(search/fetch relativize) 기준 = WorktreeRoot — linked git
	// worktree에서 ProjectRoot(주 checkout)를 쓰면 현재 worktree 파일이 WORKSPACE_VIOLATION이
	// 된다(저장소 디렉터리 명명 ProjectID는 ProjectRoot 기반 그대로, main.go 참조).
	registerSearch(srv, cfg.Store, cfg.Canon.WorktreeRoot)
	registerFetch(srv, cfg.Store, cfg.Canon.WorktreeRoot)
	// ProbeIsolation: OS 메모리 격리가 안 되는 환경에서는 ctr_transform 자체를 미등록한다
	// (in-process fallback 금지, 설계 §4.3/§5.3) — 첫 실제 호출에서야 실패를 알리지 않는다.
	if err := transform.ProbeIsolation(cfg.SelfExe); err != nil {
		slog.Warn("mcp: transform 격리 프로브 실패 — ctr_transform 비활성화", "error", err)
	} else {
		registerTransform(srv, cfg.Store, cfg.SelfExe)
	}
	if slices.Contains(cfg.Enable, "ingest") {
		registerIndex(srv, cfg.Store, cfg.Canon.WorktreeRoot, cfg.AllowPaths)
	}
	if slices.Contains(cfg.Enable, "net") {
		registerFetchAndIndex(srv, cfg.Store, cfg.NetAllowLocal, cfg.NetPorts)
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
	switch {
	case errors.Is(err, store.ErrNotFound):
		return toolErr(codeNotFound, "대상을 찾을 수 없습니다")
	case errors.Is(err, store.ErrInvalidSelector):
		return toolErr(codeInvalidArgument, "잘못된 선택자입니다")
	case errors.Is(err, store.ErrUnavailable):
		return toolErr(codeStorageUnavailable, "저장소를 사용할 수 없습니다")
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
	case errors.Is(err, fs.ErrNotExist):
		return toolErr(codeNotFound, "대상을 찾을 수 없습니다")
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

type searchQueryResult struct {
	Query     string      `json:"query"`
	Hits      []searchHit `json:"hits"`
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

func registerSearch(srv *mcp.Server, st *store.Store, worktreeRoot string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_search",
		Description: "프로젝트 색인을 BM25+RRF로 검색해 스니펫을 반환한다.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
		start := time.Now()
		if len(in.Queries) < 1 || len(in.Queries) > 8 {
			return nil, SearchOutput{}, toolErr(codeInvalidArgument, "queries는 1~8개여야 합니다")
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
		qrs, err := search.Query(ctx, st, worktreeRoot, in.Queries, limit, budget)
		if err != nil {
			return nil, SearchOutput{}, toToolError(err)
		}
		out := SearchOutput{Untrusted: true, Results: make([]searchQueryResult, len(qrs))}
		for i, qr := range qrs {
			hits := make([]searchHit, len(qr.Hits))
			for j, h := range qr.Hits {
				hits[j] = toSearchHit(h)
			}
			out.Results[i] = searchQueryResult{Query: qr.Query, Hits: hits, Truncated: qr.Truncated}
		}
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
		Name:        "ctr_fetch",
		Description: "artifact 저장본에서 선택자 범위를 그대로 회수한다.",
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
	"스크립트는 starlark: 최상위 for/while/재귀는 비활성이며 def f(): ... 안에서 " +
	"정의하고 호출해야 한다. inputs[i].text()/.lines()/.json(), args, emit(x)로 " +
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

func registerTransform(srv *mcp.Server, st *store.Store, selfExe string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_transform",
		Description: transformDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TransformInput) (*mcp.CallToolResult, TransformOutput, error) {
		start := time.Now()
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
}

const maxSnippetBytes = 1024

// snippetOf: body 앞부분을 UTF-8 경계로 스냅해 미리보기로 반환한다(설계 §4.5 "snippet(≤1KB)").
func snippetOf(body []byte) string {
	if len(body) <= maxSnippetBytes {
		return string(body)
	}
	n := maxSnippetBytes
	for n > 0 && !utf8.RuneStart(body[n]) {
		n--
	}
	return string(body[:n])
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
		rep, err := ingest.RunWeb(ctx, st, res.FinalURL, res.RawHTML, res.Body, res.MediaType, res.Extraction)
		if err != nil {
			return nil, FetchAndIndexOutput{}, toToolError(err)
		}
		out := FetchAndIndexOutput{
			ArtifactID: rep.ArtifactID, Title: res.FinalURL, ByteLength: rep.ByteLength,
			Extraction: res.Extraction, IndexedChunks: rep.IndexedChunks, Snippet: snippetOf(res.Body),
		}
		st.LedgerAppend("ctr_fetch_and_index", rep.ByteLength, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}
