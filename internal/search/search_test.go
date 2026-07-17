package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/store"
)

// seedT: store.Register 직접 3건 — porter/trigram/RRF 3개 테스트가 공유하는 코퍼스.
func seedT(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	docs := []struct{ uri, hash, text string }{
		{"/a.txt", "ha", "the cache was cached quickly"},
		{"/b.txt", "hb", "useEffect cleanup runs"},
		{"/c.txt", "hc", "caching useEffect guide"},
	}
	for _, d := range docs {
		_, err := st.Register(t.Context(), store.Registration{
			StoredBytes: []byte(d.text), MediaType: "text/plain", Redaction: "none",
			Source: store.SourceMeta{URI: d.uri, Kind: "file", SrcHash: d.hash},
			Chunks: []store.Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(d.text)),
				LineStart: 1, LineEnd: 1, Text: d.text}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func hasSource(hits []Hit, base string) bool {
	for _, h := range hits {
		if h.Source == base {
			return true
		}
	}
	return false
}

func TestQuery_PorterStemsMatch(t *testing.T) {
	st := seedT(t)
	res, err := Query(t.Context(), st, []string{"caching"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if !hasSource(hits, "a.txt") || !hasSource(hits, "c.txt") {
		t.Fatalf("want a.txt+c.txt(porter stem), got %+v", hits)
	}
	if hasSource(hits, "b.txt") {
		t.Fatalf("b.txt(useEffect만) 매치되면 안 됨, got %+v", hits)
	}
}

func TestQuery_TrigramSubstringMatch(t *testing.T) {
	st := seedT(t)
	res, err := Query(t.Context(), st, []string{"useEff"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if !hasSource(hits, "b.txt") || !hasSource(hits, "c.txt") {
		t.Fatalf("want b.txt+c.txt(trigram substring), got %+v", hits)
	}
	if hasSource(hits, "a.txt") {
		t.Fatalf("a.txt(useEff 없음) 매치되면 안 됨, got %+v", hits)
	}
}

// regOne: store.Register 1건 직접 등록(예산·stale 테스트가 seedT 코퍼스와 독립적으로 쓴다).
func regOne(t *testing.T, st *store.Store, uri, hash, text string) {
	t.Helper()
	_, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte(text), MediaType: "text/plain", Redaction: "none",
		Source: store.SourceMeta{URI: uri, Kind: "file", SrcHash: hash},
		Chunks: []store.Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(text)),
			LineStart: 1, LineEnd: 1, Text: text}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuery_BudgetSplitsAndTruncatesIndependently(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	filler := strings.Repeat("word ", 120) // 600B
	big := filler + "caching " + filler    // ~1208B, "caching" 중앙 → ±250 창 전부 사용 가능
	regOne(t, st, "/big.txt", "hbig", big)
	regOne(t, st, "/small.txt", "hsmall", "useEffect cleanup")

	res, err := Query(t.Context(), st, []string{"caching", "useEff"}, 10, 600)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Truncated || len(res[0].Hits) != 0 {
		t.Fatalf("want caching 쿼리 truncated(창>300B share, 0 kept), got truncated=%v hits=%+v", res[0].Truncated, res[0].Hits)
	}
	if res[1].Truncated || len(res[1].Hits) == 0 {
		t.Fatalf("want useEff 쿼리 미truncated(독립적+이월로 여유), got truncated=%v hits=%+v", res[1].Truncated, res[1].Hits)
	}

	unl, err := Query(t.Context(), st, []string{"caching", "useEff"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if unl[0].Truncated || unl[1].Truncated || len(unl[0].Hits) == 0 || len(unl[1].Hits) == 0 {
		t.Fatalf("want budget<=0 무제한, got %+v", unl)
	}
}

func TestQuery_SnippetWindowCentersOnMatch(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	line := "filler line of text padding out the document body\n"
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(line)
	}
	b.WriteString("NEEDLE-SNIPPET-TARGET\n")
	for i := 0; i < 100; i++ {
		b.WriteString(line)
	}
	text := b.String()
	regOne(t, st, "/needle.txt", "hneedle", text)

	res, err := Query(t.Context(), st, []string{"NEEDLE-SNIPPET-TARGET"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if len(hits) == 0 {
		t.Fatal("want >=1 hit for marker query")
	}
	snip := hits[0].Snippet
	if len(snip) > 600 {
		t.Fatalf("want snippet <= 600B, got %d", len(snip))
	}
	if !strings.Contains(snip, "NEEDLE-SNIPPET-TARGET") {
		t.Fatalf("want snippet contain marker, got %q", snip)
	}
	if snip == text[:500] {
		t.Fatalf("want snippet != 문서 앞 500B(마커는 중반), got equal")
	}
}

// TestSnippetWindow_FoldExpandingRuneNoPanic: 'Ⱥ'(U+023A, lower가 3B로 확장)가 매치 앞에
// 쌓이면 케이스폴딩 idx가 원본보다 앞서 드리프트 — 구버전은 슬라이스 out-of-range panic.
func TestSnippetWindow_FoldExpandingRuneNoPanic(t *testing.T) {
	text := strings.Repeat("Ⱥ", 300) + " NEEDLE " + strings.Repeat("x", 100)
	tok := firstMatchToken("needle", text)
	snip := snippetWindow(text, tok)
	if !strings.Contains(snip, "NEEDLE") {
		t.Fatalf("want snippet contain NEEDLE, got %q", snip)
	}
}

// TestSnippetWindow_FoldShrinkingRuneValidUTF8: 'K'(U+212A 켈빈 사인, lower가 1B로 축소)가
// 매치 앞에 쌓이면 idx가 원본보다 뒤로 드리프트 — 구버전은 룬 중간을 잘라 무효 UTF-8.
func TestSnippetWindow_FoldShrinkingRuneValidUTF8(t *testing.T) {
	text := strings.Repeat("K", 300) + " NEEDLE " + strings.Repeat("x", 100)
	tok := firstMatchToken("needle", text)
	snip := snippetWindow(text, tok)
	if !utf8.ValidString(snip) {
		t.Fatalf("want valid UTF-8 snippet, got %q", snip)
	}
}

// TestSnippetWindow_CJKNoSpaceValidUTF8: 공백 없는 CJK 장문 — 창 경계 공백 스냅이 실패해도
// 마지막 룬 경계 보정이 항상 걸려야 무효 UTF-8이 나오지 않는다(Codex P2).
func TestSnippetWindow_CJKNoSpaceValidUTF8(t *testing.T) {
	text := strings.Repeat("가", 400) + "NEEDLE" + strings.Repeat("나", 400)
	tok := firstMatchToken("NEEDLE", text)
	snip := snippetWindow(text, tok)
	if !utf8.ValidString(snip) {
		t.Fatalf("want valid UTF-8 snippet, got %q", snip)
	}
	if !strings.Contains(snip, "NEEDLE") {
		t.Fatalf("want snippet contain NEEDLE, got %q", snip)
	}
}

func TestQuery_StaleDetectsModifiedFile(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	file := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(file, []byte("caching stale test v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Run(t.Context(), st, root, nil, ingest.Request{Path: file}); err != nil {
		t.Fatal(err)
	}

	before, err := Query(t.Context(), st, []string{"caching"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(before[0].Hits) == 0 || before[0].Hits[0].Stale {
		t.Fatalf("want Stale=false 수정 전, got %+v", before[0].Hits)
	}

	if err := os.WriteFile(file, []byte("caching stale test v2 modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}

	after, err := Query(t.Context(), st, []string{"caching"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after[0].Hits) == 0 || !after[0].Hits[0].Stale {
		t.Fatalf("want Stale=true 수정 후, got %+v", after[0].Hits)
	}
}

// TestQuery_ReindexOrphanDoesNotFailQuery: 같은 uri 재색인 시 구 artifact/chunk가
// orphan으로 남는다(sources.artifact_id만 갱신). orphan이 후보로 잡혀도 Query 전체가
// 오류로 죽지 않아야 한다(Fix D).
func TestQuery_ReindexOrphanDoesNotFailQuery(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	file := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(file, []byte("oldwordunique content here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Run(t.Context(), st, root, nil, ingest.Request{Path: file}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("newwordunique content here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Run(t.Context(), st, root, nil, ingest.Request{Path: file}); err != nil {
		t.Fatal(err)
	}

	oldRes, err := Query(t.Context(), st, []string{"oldwordunique"}, 10, 0)
	if err != nil {
		t.Fatalf("want no error querying orphaned term, got %v", err)
	}
	if len(oldRes[0].Hits) != 0 {
		t.Fatalf("want 0 hits for orphaned-only term, got %+v", oldRes[0].Hits)
	}

	newRes, err := Query(t.Context(), st, []string{"newwordunique"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(newRes[0].Hits) == 0 || !strings.Contains(newRes[0].Hits[0].Snippet, "newwordunique") {
		t.Fatalf("want new content hit, got %+v", newRes[0].Hits)
	}
}

func TestQuery_RRFRanksDualMatchTop(t *testing.T) {
	st := seedT(t)
	res, err := Query(t.Context(), st, []string{"caching useEffect"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if len(hits) == 0 || hits[0].Source != "c.txt" {
		t.Fatalf("want c.txt(porter+trigram 동시매치) top hit, got %+v", hits)
	}
}

// TestQuery_RawInputsNoFTSInjectionError: FTS5 MATCH 구문 특수문자·인젝션성 원시 입력이
// normalizeQuery의 토큰별 이중따옴표 이스케이프를 통과해 오류 없이 반환되는지 확인한다
// (Fix G, hits 유무는 무관).
func TestQuery_RawInputsNoFTSInjectionError(t *testing.T) {
	st := seedT(t)
	inputs := []string{
		`" OR 1; DROP TABLE x --`,
		"NEAR(",
		"*",
		`한글"질의`,
	}
	for _, q := range inputs {
		if _, err := Query(t.Context(), st, []string{q}, 10, 0); err != nil {
			t.Fatalf("want no error for input %q, got %v", q, err)
		}
	}
}
