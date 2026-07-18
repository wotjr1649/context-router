package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/netfetch"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

// testSelfExe: 실 ctr 바이너리를 1회만 빌드해(sync.Once) 재사용한다 — ctr_transform은
// 실제 "__transform-worker" 프로세스 경계를 타므로(internal/transform/worker_test.go와
// 동형 패턴), NewServer의 ProbeIsolation·Spawn이 프로덕션과 동일 경로로 검증된다.
var (
	testExeOnce sync.Once
	testExePath string
	testExeErr  error
)

func testSelfExe(t *testing.T) string {
	t.Helper()
	testExeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ctr-mcp-test-*")
		if err != nil {
			testExeErr = err
			return
		}
		bin := filepath.Join(dir, "ctr-test")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "github.com/wotjr1649/context-router/cmd/context-router")
		if out, err := cmd.CombinedOutput(); err != nil {
			testExeErr = fmt.Errorf("selfExe 빌드 실패: %w: %s", err, out)
			return
		}
		testExePath = bin
	})
	if testExeErr != nil {
		t.Fatalf("selfExe 빌드 실패: %v", testExeErr)
	}
	return testExePath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testExePath != "" {
		os.RemoveAll(filepath.Dir(testExePath))
	}
	os.Exit(code)
}

func TestToToolError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"not_found", store.ErrNotFound, codeNotFound},
		{"not_found_wrapped", fmt.Errorf("op: %w", store.ErrNotFound), codeNotFound},
		{"invalid_selector", store.ErrInvalidSelector, codeInvalidArgument},
		{"unavailable", store.ErrUnavailable, codeStorageUnavailable},
		{"no_isolation", transform.ErrNoIsolation, codeStorageUnavailable},
		{"budget", transform.ErrBudget, codeBudgetExceeded},
		{"output_limit", transform.ErrOutputLimit, codeOutputLimitExceeded},
		{"workspace", ingest.ErrWorkspace, codeWorkspaceViolation},
		{"unsupported", ingest.ErrUnsupported, codeUnsupportedFile},
		{"network_denied", netfetch.ErrDenied, codeNetworkDenied},
		{"body_too_large", netfetch.ErrBodyTooLarge, codeOutputLimitExceeded},
		{"too_many_redirects", netfetch.ErrTooManyRedirects, codeNetworkDenied},
		{"unsupported_media", netfetch.ErrUnsupportedMedia, codeUnsupportedFile},
		{"not_exist", fs.ErrNotExist, codeNotFound},
		{"not_exist_wrapped", fmt.Errorf("ingest: canonicalize: %w", fs.ErrNotExist), codeNotFound},
		{"unknown", errors.New("boom"), codeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toToolError(tt.err).Error()
			want := "[" + tt.code + "]"
			if !strings.HasPrefix(got, want) {
				t.Fatalf("toToolError(%v) = %q, want prefix %q", tt.err, got, want)
			}
		})
	}
}

// TestToToolErrorCancellation: 최종리뷰 C2(fable Imp3) — 취소/데드라인은 sentinel 매핑을
// 타지 않고 SDK가 처리하도록 원본 ctx 오류를 그대로 반환한다(INTERNAL로 뭉개거나 slog
// 소음을 내지 않음). toToolError가 모든 핸들러의 단일 오류 변환 지점이므로(§6) 여기서
// 검증하면 handler 호출 경로 전체를 대표한다.
func TestToToolErrorCancellation(t *testing.T) {
	tests := []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("ingest: run web: %w", context.Canceled),
	}
	for _, err := range tests {
		got := toToolError(err)
		if got != err {
			t.Fatalf("toToolError(%v) = %v, want original error unchanged", err, got)
		}
		if strings.Contains(got.Error(), codeInternal) {
			t.Fatalf("toToolError(%v) = %q, want no INTERNAL code", err, got)
		}
	}
}

// newTestServer: canon+실물 store로 서버를 만들고 in-memory transport로 클라이언트까지 연결한다.
func newTestServer(t *testing.T, enable []string) (*mcp.ClientSession, ident.Canon) {
	t.Helper()
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := NewServer(Config{Canon: canon, Store: st, SelfExe: testSelfExe(t), Enable: enable})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, canon
}

func TestNewServerProfileGating(t *testing.T) {
	tests := []struct {
		name   string
		enable []string
		want   []string
	}{
		{"base", nil, []string{"ctr_fetch", "ctr_search", "ctr_transform"}},
		{"ingest", []string{"ingest"}, []string{"ctr_fetch", "ctr_index", "ctr_search", "ctr_transform"}},
		{"net", []string{"net"}, []string{"ctr_fetch", "ctr_fetch_and_index", "ctr_search", "ctr_transform"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs, _ := newTestServer(t, tt.enable)
			lt, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			got := make([]string, len(lt.Tools))
			for i, tl := range lt.Tools {
				got[i] = tl.Name
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("tools=%v want %v", got, tt.want)
			}
		})
	}
}

func remarshal(t *testing.T, v, out any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("remarshal unmarshal: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	cs, canon := newTestServer(t, []string{"ingest"})
	ctx := context.Background()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range lt.Tools {
		byName[tl.Name] = tl
	}
	if tl := byName["ctr_search"]; tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_search readOnlyHint 누락: %+v", tl)
	}
	if tl := byName["ctr_fetch"]; tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_fetch readOnlyHint 누락: %+v", tl)
	}

	tmpFile := filepath.Join(canon.ProjectRoot, "note.txt")
	if err := os.WriteFile(tmpFile, []byte("needle content for round trip\n"), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	idxRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: tmpFile}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if idxRes.IsError {
		t.Fatalf("ctr_index error: %+v", idxRes.Content)
	}
	var idxOut IndexOutput
	remarshal(t, idxRes.StructuredContent, &idxOut)
	if idxOut.Indexed != 1 {
		t.Fatalf("indexed=%d want 1 (skipped=%+v)", idxOut.Indexed, idxOut.Skipped)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	if searchRes.IsError {
		t.Fatalf("ctr_search error: %+v", searchRes.Content)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if !searchOut.Untrusted {
		t.Fatalf("untrusted flag missing: %+v", searchOut)
	}
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits: %+v", searchOut.Results)
	}
	hit := searchOut.Results[0].Hits[0]

	chunkID := hit.ChunkID
	fetchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch", Arguments: FetchInput{ArtifactID: hit.ArtifactID, ChunkID: &chunkID}})
	if err != nil {
		t.Fatalf("ctr_fetch call: %v", err)
	}
	if fetchRes.IsError {
		t.Fatalf("ctr_fetch error: %+v", fetchRes.Content)
	}
	var fetchOut FetchOutput
	remarshal(t, fetchRes.StructuredContent, &fetchOut)
	if fetchOut.ExactScope != "artifact" {
		t.Fatalf("exact_scope=%q want artifact", fetchOut.ExactScope)
	}
	if fetchOut.Content == "" {
		t.Fatalf("fetch content empty")
	}
	if fetchOut.Provenance.SrcHash == "" {
		t.Fatalf("provenance.src_hash empty: %+v", fetchOut.Provenance)
	}
	if fetchOut.Provenance.Source == "" || filepath.IsAbs(fetchOut.Provenance.Source) {
		t.Fatalf("provenance.source want project-relative, got %q", fetchOut.Provenance.Source)
	}
	if fetchOut.Provenance.Stale {
		t.Fatalf("provenance.stale=true, want false before modification")
	}

	// 파일 수정 후 재-fetch: 같은 chunk 선택자라도 provenance.stale이 true로 바뀌어야 한다.
	future := time.Now().Add(time.Hour)
	if err := os.WriteFile(tmpFile, []byte("needle content MODIFIED for round trip\n"), 0o644); err != nil {
		t.Fatalf("modify tmp file: %v", err)
	}
	if err := os.Chtimes(tmpFile, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	fetchRes2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch", Arguments: FetchInput{ArtifactID: hit.ArtifactID, ChunkID: &chunkID}})
	if err != nil {
		t.Fatalf("ctr_fetch call 2: %v", err)
	}
	if fetchRes2.IsError {
		t.Fatalf("ctr_fetch error 2: %+v", fetchRes2.Content)
	}
	var fetchOut2 FetchOutput
	remarshal(t, fetchRes2.StructuredContent, &fetchOut2)
	if !fetchOut2.Provenance.Stale {
		t.Fatalf("provenance.stale=false after modification, want true: %+v", fetchOut2.Provenance)
	}
}

// TestServeStdoutPurity: 실제 Serve()가 stdio에 물릴 os.Stdin/os.Stdout을 파이프로 교체해
// JSON-RPC 프로토콜 외 바이트가 stdout에 전혀 쓰이지 않음을 확인한다(배너는 stderr, §5.5).
func TestServeStdoutPurity(t *testing.T) {
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer st.Close()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	inW.Close() // 즉시 EOF → Run()이 곧바로 반환
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	serveErr := Serve(context.Background(), Config{Canon: canon, Store: st})
	os.Stdin, os.Stdout = oldIn, oldOut
	outW.Close()

	data, err := io.ReadAll(outR)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("stdout polluted: %q (serve err=%v)", data, serveErr)
	}
}

// TestWorktreeRootIsPathBasis: linked git worktree를 Canon{ProjectRoot:A, WorktreeRoot:B}로
// 흉내낸다(실제 git worktree 픽스처 불요) — B 하위 파일이 ctr_index에 성공하고(A 기준이면
// WORKSPACE_VIOLATION) source가 B-relative여야 한다(β2-1).
func TestWorktreeRootIsPathBasis(t *testing.T) {
	projCanon, err := ident.Canonicalize(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize project: %v", err)
	}
	wtDir := t.TempDir()
	wtCanon, err := ident.Canonicalize(wtDir)
	if err != nil {
		t.Fatalf("canonicalize worktree: %v", err)
	}
	canon := ident.Canon{ProjectRoot: projCanon.ProjectRoot, WorktreeRoot: wtCanon.WorktreeRoot}

	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer st.Close()
	srv, err := NewServer(Config{Canon: canon, Store: st, Enable: []string{"ingest"}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	tmpFile := filepath.Join(wtDir, "note.txt")
	if err := os.WriteFile(tmpFile, []byte("needle content in worktree\n"), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	idxRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: tmpFile}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if idxRes.IsError {
		t.Fatalf("ctr_index error (want success under WorktreeRoot): %+v", idxRes.Content)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits: %+v", searchOut.Results)
	}
	if source := searchOut.Results[0].Hits[0].Source; source != "note.txt" {
		t.Fatalf("source=%q want worktree-relative %q", source, "note.txt")
	}
}

// TestCtrIndexMissingPathIsNotFound: 존재하지 않는 path는 INTERNAL이 아니라 NOT_FOUND여야
// 한다(β2-3, toToolError의 fs.ErrNotExist 분기).
func TestCtrIndexMissingPathIsNotFound(t *testing.T) {
	cs, _ := newTestServer(t, []string{"ingest"})
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "does-not-exist.txt")
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: missing}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for missing path, got %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "["+codeNotFound+"]") {
		t.Fatalf("want %s prefix, got %q", codeNotFound, text)
	}
}

// TestValidateIndexInput: path/content는 XOR이고 content 사용 시 title이 필수다(β2-4).
func TestValidateIndexInput(t *testing.T) {
	tests := []struct {
		name    string
		in      IndexInput
		wantErr bool
	}{
		{"neither", IndexInput{}, true},
		{"both_path_and_content", IndexInput{Path: "p", Content: "c", Title: "t"}, true},
		{"content_without_title", IndexInput{Content: "c"}, true},
		{"path_only", IndexInput{Path: "p"}, false},
		{"content_with_title", IndexInput{Content: "c", Title: "t"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIndexInput(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateIndexInput(%+v) err=%v wantErr=%v", tt.in, err, tt.wantErr)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "["+codeInvalidArgument+"]") {
				t.Fatalf("err=%v want %s prefix", err, codeInvalidArgument)
			}
		})
	}
}

// TestSourceCoordsExact: fetch의 source_coords_exact는 search와 같은 의미론이어야 한다(β2-2).
func TestSourceCoordsExact(t *testing.T) {
	base := store.RangeResult{Artifact: store.ArtifactMeta{Redaction: "none"}}

	inline := base
	inline.HasSource = true
	inline.Source = store.SourceInfo{Kind: "inline"}
	if !sourceCoordsExact(inline) {
		t.Fatalf("want true for inline no-redaction, got false: %+v", inline)
	}

	noSource := base
	noSource.HasSource = false
	if sourceCoordsExact(noSource) {
		t.Fatalf("want false when HasSource=false, got true: %+v", noSource)
	}
}

// TestRepresentationOf: source_kind==inline이면 media_type과 무관하게 "inline"이어야 한다(β2-5).
func TestRepresentationOf(t *testing.T) {
	if got := representationOf("text/plain", "inline"); got != "inline" {
		t.Fatalf("got=%q want inline", got)
	}
	if got := representationOf("text/markdown", "file"); got != "markdown" {
		t.Fatalf("got=%q want markdown", got)
	}
	if got := representationOf("text/plain", "file"); got != "file" {
		t.Fatalf("got=%q want file", got)
	}
}

// TestApplyFetchBudgetNewlineBoundary: 절단이 개행으로 정확히 끝나면 line_end가 다음 줄로
// 과계산되지 않아야 한다(β2-6).
func TestApplyFetchBudgetNewlineBoundary(t *testing.T) {
	res := store.RangeResult{
		Text: []byte("a\nb\nc\n"), ByteStart: 0, ByteEnd: 6,
		LineStart: 1, LineEnd: 3,
	}
	text, byteEnd, lineEnd, truncated := applyFetchBudget(res, 4) // cut="a\nb\n"
	if !truncated || string(text) != "a\nb\n" || byteEnd != 4 {
		t.Fatalf("text=%q byteEnd=%d truncated=%v", text, byteEnd, truncated)
	}
	if lineEnd != 2 {
		t.Fatalf("lineEnd=%d want 2 (개행 경계 과계산 회귀)", lineEnd)
	}
}

// TestCtrTransformRoundTrip: 색인(ingest) → artifact_id → ctr_transform이 저장된 텍스트
// 길이를 정확히 반환해야 한다(T3 TDD 항목 2). def 래핑(top-level for/재귀 비활성) 준수 스크립트.
func TestCtrTransformRoundTrip(t *testing.T) {
	cs, canon := newTestServer(t, []string{"ingest"})
	ctx := context.Background()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range lt.Tools {
		byName[tl.Name] = tl
	}
	if tl := byName["ctr_transform"]; tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_transform readOnlyHint 누락: %+v", tl)
	}

	body := "needle content for transform round trip\n"
	tmpFile := filepath.Join(canon.ProjectRoot, "xform.txt")
	if err := os.WriteFile(tmpFile, []byte(body), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	idxRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: tmpFile}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if idxRes.IsError {
		t.Fatalf("ctr_index error: %+v", idxRes.Content)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits: %+v", searchOut.Results)
	}
	artifactID := searchOut.Results[0].Hits[0].ArtifactID

	script := "def f():\n  emit(str(len(inputs[0].text())))\nf()\n"
	xRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{Script: script, Inputs: []int64{artifactID}}})
	if err != nil {
		t.Fatalf("ctr_transform call: %v", err)
	}
	if xRes.IsError {
		t.Fatalf("ctr_transform error: %+v", xRes.Content)
	}
	var xOut TransformOutput
	remarshal(t, xRes.StructuredContent, &xOut)
	want := fmt.Sprintf("%d", len(body))
	if xOut.Result != want {
		t.Fatalf("result=%q want %q (stored text length)", xOut.Result, want)
	}
}

// TestCtrTransformCapsMapping: budget/output_limit 초과 스크립트가 각각 BUDGET_EXCEEDED/
// OUTPUT_LIMIT_EXCEEDED로 매핑돼야 한다(T3 TDD 항목 3).
func TestCtrTransformCapsMapping(t *testing.T) {
	cs, _ := newTestServer(t, nil)
	ctx := context.Background()

	budgetRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{
		Script: "def f():\n\tfor i in range(100000000):\n\t\tpass\n\nf()\n",
	}})
	if err != nil {
		t.Fatalf("budget call: %v", err)
	}
	if !budgetRes.IsError {
		t.Fatalf("want IsError=true for budget script, got %+v", budgetRes)
	}
	if text := budgetRes.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeBudgetExceeded+"]") {
		t.Fatalf("want %s prefix, got %q", codeBudgetExceeded, text)
	}

	outRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{
		Script:         "def f():\n\tfor i in range(1000):\n\t\temit(\"x\")\n\nf()\n",
		MaxOutputBytes: 4,
	}})
	if err != nil {
		t.Fatalf("output_limit call: %v", err)
	}
	if !outRes.IsError {
		t.Fatalf("want IsError=true for output_limit script, got %+v", outRes)
	}
	if text := outRes.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeOutputLimitExceeded+"]") {
		t.Fatalf("want %s prefix, got %q", codeOutputLimitExceeded, text)
	}
}

// TestCtrTransformInputValidation: inputs 9개(최대 8 초과)·script 64KB 초과는 각각
// INVALID_ARGUMENT여야 한다(T3 TDD 항목 4, 승계 (c)).
func TestCtrTransformInputValidation(t *testing.T) {
	cs, _ := newTestServer(t, nil)
	ctx := context.Background()

	tooManyInputs := make([]int64, 9)
	for i := range tooManyInputs {
		tooManyInputs[i] = int64(i + 1)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{
		Script: "def f():\n  emit('x')\nf()\n", Inputs: tooManyInputs,
	}})
	if err != nil {
		t.Fatalf("9-inputs call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for 9 inputs, got %+v", res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeInvalidArgument+"]") {
		t.Fatalf("want %s prefix, got %q", codeInvalidArgument, text)
	}

	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{Script: strings.Repeat("a", 70000)}})
	if err != nil {
		t.Fatalf("big-script call: %v", err)
	}
	if !res2.IsError {
		t.Fatalf("want IsError=true for 64KB+ script, got %+v", res2)
	}
	if text := res2.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeInvalidArgument+"]") {
		t.Fatalf("want %s prefix, got %q", codeInvalidArgument, text)
	}
}

// TestCtrTransformDescriptionMentionsDefWrapping: 도구 description에 def 래핑 제약이
// 명시돼야 한다(T1/T2 승계 (b) — 모르면 자연스러운 top-level for/while 스크립트가 실패한다).
func TestCtrTransformDescriptionMentionsDefWrapping(t *testing.T) {
	cs, _ := newTestServer(t, nil)
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tl := range lt.Tools {
		if tl.Name != "ctr_transform" {
			continue
		}
		if !strings.Contains(tl.Description, "def f()") {
			t.Fatalf("description에 def 래핑 제약 누락: %q", tl.Description)
		}
		// 최종리뷰 C5(fable triage b): while/재귀는 def 안에서도 지원 안 됨을 정확히 명시.
		const wantPhrase = "최상위 for는 def 함수 안에서 사용. while·재귀는 starlark 기본 설정상 지원 안 됨(def 안에서도)."
		if !strings.Contains(tl.Description, wantPhrase) {
			t.Fatalf("description에 while/재귀 정정 문구 누락: %q", tl.Description)
		}
		return
	}
	t.Fatal("ctr_transform 도구 없음")
}

// --- ctr_fetch_and_index (설계 §4.5, T6) ---

// srvPort extracts srv.URL's port as int (httptest 서버는 임의 포트를 쓰므로 ExtraPorts에
// 필요 — internal/netfetch/netfetch_test.go의 동형 헬퍼를 패키지 경계상 재사용 불가해 복제).
func srvPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", rawURL, err)
	}
	return p
}

// newNetTestServer: newTestServer과 동형이나 "net" 프로필+NetAllowLocal/NetPorts까지
// 구성하고, store와 저장 디렉터리(raw blob 파일 확인용)도 반환한다(T6 전용).
func newNetTestServer(t *testing.T, allowLocal bool, ports []int) (*mcp.ClientSession, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	storeDir := t.TempDir()
	st, err := store.Open(storeDir, false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := NewServer(Config{
		Canon: canon, Store: st, SelfExe: testSelfExe(t), Enable: []string{"net"},
		NetAllowLocal: allowLocal, NetPorts: ports,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, st, storeDir
}

// secretCanary: AWS 키 형태 캐너리. 소스에 연속 토큰을 두지 않는다(규약 §8, push
// protection 재발 방지 — .superpowers/sdd/progress.md 2026-07-18 CANARY FIX) — 런타임
// 분할 리터럴 + 비실토큰 값으로 구성.
const secretCanary = "AKIA" + "NOTAREALKEY01234"

const fetchAndIndexTestPage = `<html><body><h1>Sample Doc</h1><p>needle unique marker text for search verification. token=` + secretCanary + `</p><script>var rawOnlyMarkerXYZ789 = 1;</script></body></html>`

// TestCtrFetchAndIndexRoundTrip: fetch_and_index→search 본문 hit(+SourceCoordsExact=false
// [이월 검증])·raw blob 파일 보존·FTS 미등록(스크립트 전용 마커는 검색으로 안 잡힘)을 확인한다.
func TestCtrFetchAndIndexRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fetchAndIndexTestPage))
	}))
	defer srv.Close()

	cs, st, storeDir := newNetTestServer(t, true, []int{srvPort(t, srv.URL)})
	ctx := context.Background()

	fiRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch_and_index", Arguments: FetchAndIndexInput{URL: srv.URL}})
	if err != nil {
		t.Fatalf("ctr_fetch_and_index call: %v", err)
	}
	if fiRes.IsError {
		t.Fatalf("ctr_fetch_and_index error: %+v", fiRes.Content)
	}
	var fiOut FetchAndIndexOutput
	remarshal(t, fiRes.StructuredContent, &fiOut)
	if fiOut.ArtifactID == 0 || fiOut.IndexedChunks == 0 || fiOut.ByteLength == 0 {
		t.Fatalf("bad fetch_and_index output: %+v", fiOut)
	}
	if fiOut.Extraction == "" {
		t.Fatalf("extraction empty: %+v", fiOut)
	}
	if len(fiOut.Snippet) == 0 || len(fiOut.Snippet) > 1024 {
		t.Fatalf("bad snippet length=%d", len(fiOut.Snippet))
	}
	// 최종리뷰 C1(수렴 Critical): snippet은 저장본(redacted) 기준이어야 한다 — netfetch 원문
	// 기준이면 redaction 이전 secret이 응답 1KB로 그대로 유출된다.
	if strings.Contains(fiOut.Snippet, secretCanary) {
		t.Fatalf("snippet leaks secret canary (redaction bypass): %q", fiOut.Snippet)
	}
	// 최종리뷰 C4(fable Imp5): 원격 웹 콘텐츠 반환 도구는 SearchOutput/FetchOutput과 일관되게
	// untrusted 마커를 달아야 한다(프롬프트 주입 표면 표시).
	if !fiOut.Untrusted {
		t.Fatalf("untrusted flag missing: %+v", fiOut)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits for body text: %+v", searchOut.Results)
	}
	if searchOut.Results[0].Hits[0].SourceCoordsExact {
		t.Fatalf("want SourceCoordsExact=false for web source: %+v", searchOut.Results[0].Hits[0])
	}

	var rawBlobHash string
	if err := st.Reader().QueryRow(`SELECT raw_blob_hash FROM sources WHERE artifact_id=?`, fiOut.ArtifactID).Scan(&rawBlobHash); err != nil {
		t.Fatalf("query raw_blob_hash: %v", err)
	}
	if rawBlobHash == "" {
		t.Fatalf("raw_blob_hash empty")
	}
	blobPath := filepath.Join(storeDir, "artifacts", rawBlobHash[:2], rawBlobHash)
	raw, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read raw blob file: %v", err)
	}
	if string(raw) != fetchAndIndexTestPage {
		t.Fatalf("raw blob content mismatch: %q", raw)
	}

	searchRes2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"rawOnlyMarkerXYZ789"}}})
	if err != nil {
		t.Fatalf("ctr_search(script marker) call: %v", err)
	}
	var searchOut2 SearchOutput
	remarshal(t, searchRes2.StructuredContent, &searchOut2)
	if len(searchOut2.Results) != 1 || len(searchOut2.Results[0].Hits) != 0 {
		t.Fatalf("script-only marker unexpectedly indexed: %+v", searchOut2.Results)
	}

	// secret 캐너리는 redaction으로 저장 단계에서 사라지므로 search/fetch 어디서도 안 나와야
	// 한다(기존 보장 유지 확인 — snippet 쪽 유출만 이번 회귀의 신규 지점이었다).
	searchRes3, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{secretCanary}}})
	if err != nil {
		t.Fatalf("ctr_search(canary) call: %v", err)
	}
	var searchOut3 SearchOutput
	remarshal(t, searchRes3.StructuredContent, &searchOut3)
	if len(searchOut3.Results) != 1 || len(searchOut3.Results[0].Hits) != 0 {
		t.Fatalf("secret canary unexpectedly searchable: %+v", searchOut3.Results)
	}

	var bs, be int64 = 0, fiOut.ByteLength
	fetchRes3, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch", Arguments: FetchInput{ArtifactID: fiOut.ArtifactID, ByteStart: &bs, ByteEnd: &be}})
	if err != nil {
		t.Fatalf("ctr_fetch(full) call: %v", err)
	}
	if fetchRes3.IsError {
		t.Fatalf("ctr_fetch(full) error: %+v", fetchRes3.Content)
	}
	var fetchOut3 FetchOutput
	remarshal(t, fetchRes3.StructuredContent, &fetchOut3)
	if strings.Contains(fetchOut3.Content, secretCanary) {
		t.Fatalf("fetch leaks secret canary: %q", fetchOut3.Content)
	}
}

// TestCtrFetchAndIndexDenied: AllowLocal=false에서 사설/루프백 목적지는 NETWORK_DENIED.
func TestCtrFetchAndIndexDenied(t *testing.T) {
	cs, _, _ := newNetTestServer(t, false, nil)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch_and_index", Arguments: FetchAndIndexInput{URL: "http://127.0.0.1:1/"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for denied local address, got %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "["+codeNetworkDenied+"]") {
		t.Fatalf("want %s prefix, got %q", codeNetworkDenied, text)
	}
}
