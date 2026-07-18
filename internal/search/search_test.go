package search

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/store"
)

// realDir: t.TempDir()류 경로를 canonical(Abs+EvalSymlinks)로 해석한다. ingest.Run의
// projectRoot 인자는 이미 canonical함을 계약으로 삼는다(§2.1 인가 루트 고정, Codex
// 교차리뷰 P1-2) — 운영 경로는 mcp.go가 ident.Canonicalize로 이렇게 넘긴다.
func realDir(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

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
			Chunks: []store.Chunk{{
				Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(d.text)),
				LineStart: 1, LineEnd: 1, Text: d.text,
			}},
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
	res, err := Query(t.Context(), st, "", []string{"caching"}, 10, 0)
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
	res, err := Query(t.Context(), st, "", []string{"useEff"}, 10, 0)
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
		Chunks: []store.Chunk{{
			Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(text)),
			LineStart: 1, LineEnd: 1, Text: text,
		}},
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

	res, err := Query(t.Context(), st, "", []string{"caching", "useEff"}, 10, 600)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Truncated || len(res[0].Hits) != 0 {
		t.Fatalf("want caching 쿼리 truncated(창>300B share, 0 kept), got truncated=%v hits=%+v", res[0].Truncated, res[0].Hits)
	}
	if res[1].Truncated || len(res[1].Hits) == 0 {
		t.Fatalf("want useEff 쿼리 미truncated(독립적+이월로 여유), got truncated=%v hits=%+v", res[1].Truncated, res[1].Hits)
	}

	unl, err := Query(t.Context(), st, "", []string{"caching", "useEff"}, 10, 0)
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

	res, err := Query(t.Context(), st, "", []string{"NEEDLE-SNIPPET-TARGET"}, 10, 0)
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
	root := realDir(t, t.TempDir())
	file := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(file, []byte("caching stale test v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Run(t.Context(), st, root, nil, ingest.Request{Path: file}); err != nil {
		t.Fatal(err)
	}

	before, err := Query(t.Context(), st, "", []string{"caching"}, 10, 0)
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

	after, err := Query(t.Context(), st, "", []string{"caching"}, 10, 0)
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
	root := realDir(t, t.TempDir())
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

	oldRes, err := Query(t.Context(), st, "", []string{"oldwordunique"}, 10, 0)
	if err != nil {
		t.Fatalf("want no error querying orphaned term, got %v", err)
	}
	if len(oldRes[0].Hits) != 0 {
		t.Fatalf("want 0 hits for orphaned-only term, got %+v", oldRes[0].Hits)
	}

	newRes, err := Query(t.Context(), st, "", []string{"newwordunique"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(newRes[0].Hits) == 0 || !strings.Contains(newRes[0].Hits[0].Snippet, "newwordunique") {
		t.Fatalf("want new content hit, got %+v", newRes[0].Hits)
	}
}

// TestQuery_OrphanDoesNotPreemptLimit: α1 — orphan 후보(bm25 상 더 유리하게 만들어 재현
// 확실화)가 limit*4 후보 슬롯을 선점해 현재 artifact hit이 사라지면 안 된다. 같은 uri를
// 5회 재색인(직전 버전마다 orphan 발생)한 뒤 작은 limit으로 질의한다.
func TestQuery_OrphanDoesNotPreemptLimit(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for i := 0; i < 5; i++ {
		body := strings.Repeat("sharedneedle ", 5) + fmt.Sprintf("orphanunique%d", i)
		regOne(t, st, "/doc.txt", fmt.Sprintf("horphan%d", i), body)
	}
	regOne(t, st, "/doc.txt", "hcurrent", "sharedneedle currentunique")

	res, err := Query(t.Context(), st, "", []string{"sharedneedle"}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if len(hits) == 0 || !strings.Contains(hits[0].Snippet, "currentunique") {
		t.Fatalf("want current artifact hit(orphan이 LIMIT 선점하면 안 됨), got %+v", hits)
	}
}

func TestQuery_RRFRanksDualMatchTop(t *testing.T) {
	st := seedT(t)
	res, err := Query(t.Context(), st, "", []string{"caching useEffect"}, 10, 0)
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
		if _, err := Query(t.Context(), st, "", []string{q}, 10, 0); err != nil {
			t.Fatalf("want no error for input %q, got %v", q, err)
		}
	}
}

// TestRelativizeSource_ProjectRelativeInvariants: Fix B — Source가 진짜 project-relative
// 경로여야 한다(설계 §4.1). projectRoot 하위는 TrimPrefix로 진짜 상대경로, 밖(또는 불명)은
// fallback(선행 "/"·드라이브 세그먼트 제거 후 마지막 <=3 세그먼트)으로 근사한다.
func TestRelativizeSource_ProjectRelativeInvariants(t *testing.T) {
	cases := []struct{ name, uri, root, want string }{
		{"deep under root", "c:/repo/sub/dir/file.go", "c:/repo", "sub/dir/file.go"},
		{"direct child of root", "c:/repo/a.txt", "c:/repo", "a.txt"},
		{"outside root fallback", "c:/other/a/b/c/d/file.go", "c:/repo", "c/d/file.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RelativizeSource(c.root, c.uri)
			if got != c.want {
				t.Fatalf("RelativizeSource(%q,%q) = %q, want %q", c.root, c.uri, got, c.want)
			}
			if filepath.IsAbs(got) {
				t.Fatalf("want !IsAbs, got %q", got)
			}
			if len(got) >= 2 && got[0] >= 'a' && got[0] <= 'z' && got[1] == ':' {
				t.Fatalf("want no drive prefix, got %q", got)
			}
			if strings.HasPrefix(got, "/") {
				t.Fatalf("want no leading slash, got %q", got)
			}
		})
	}
}

// TestQuery_SourceCoordsExact: Fix C — inline 원문(redaction 없음)은 좌표가 정확해 true,
// redaction spans가 있는 file은 false(kind만으로 결정하던 구버전은 inline을 오탐 false 처리).
func TestQuery_SourceCoordsExact(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	inline := "caching notes inline"
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte(inline), MediaType: "text/plain", Redaction: "none",
		Source: store.SourceMeta{URI: "inline:notes", Kind: "inline", SrcHash: "hi"},
		Chunks: []store.Chunk{{
			Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(inline)),
			LineStart: 1, LineEnd: 1, Text: inline,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	secret := "caching password hunter2 redacted"
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte(secret), MediaType: "text/plain", Redaction: "spans",
		Source: store.SourceMeta{URI: "/secret.txt", Kind: "file", SrcHash: "hs"},
		Chunks: []store.Chunk{{
			Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(secret)),
			LineStart: 1, LineEnd: 1, Text: secret,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Query(t.Context(), st, "", []string{"caching"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var gotInline, gotSecret bool
	for _, h := range res[0].Hits {
		switch h.Source {
		case "inline:notes":
			gotInline = true
			if !h.SourceCoordsExact {
				t.Fatalf("want inline SourceCoordsExact=true, got %+v", h)
			}
		case "secret.txt":
			gotSecret = true
			if h.SourceCoordsExact {
				t.Fatalf("want redacted file SourceCoordsExact=false, got %+v", h)
			}
		}
	}
	if !gotInline || !gotSecret {
		t.Fatalf("want both inline+secret hits, got %+v", res[0].Hits)
	}
}

// TestQuery_TrigramShortTokenAND: Fix E — trigram 후보 중 normalizeQuery가 <3자라 제외한
// 토큰("go")이 chunk.text에 리터럴로 없는 후보(rust 문서)는 버려야 한다. 쿼리를 "go cachin"
// (porter 스템 불일치라 porter 경로는 무력화됨)으로 줘 trigram 경로만으로 AND 계약을 검증한다.
func TestQuery_TrigramShortTokenAND(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	regOne(t, st, "/go.txt", "hgo", "caching guide for go services")
	regOne(t, st, "/rust.txt", "hrust", "caching guide for rust services")

	res, err := Query(t.Context(), st, "", []string{"go cachin"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if hasSource(hits, "rust.txt") {
		t.Fatalf("want rust.txt(no literal 'go') excluded, got %+v", hits)
	}
	if !hasSource(hits, "go.txt") {
		t.Fatalf("want go.txt(trigram 'cachin' + literal 'go') present, got %+v", hits)
	}
}

// TestQuery_SnippetStemPrefixFallback: Fix F — 쿼리 "caching"·본문 "cached"(porter 스템 매치,
// 리터럴 불일치)에서 앵커 탐색이 접두(prefix) 폴백으로 실제 매치 지점을 찾아야 한다(구버전은
// 앞 500B로 폴백해 무관 스니펫을 냈다).
// TestQuery_HitSourceDeterministicMultiSource: α6 — 다중 소스가 같은 artifact를 가리킬 때
// hitQuery도 store.sourceOf와 동일하게 uri 오름차순 첫 행을 결정적으로 고른다. 알파벳
// 역순으로 등록해(z 먼저, a 나중) 삽입순 우연 일치를 배제한다.
func TestQuery_HitSourceDeterministicMultiSource(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	body := "sharedbody multisource marker text"
	reg := store.Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/z-later.txt", Kind: "file", SrcHash: "hz"},
		Chunks: []store.Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), LineStart: 1, LineEnd: 1, Text: body}},
	}
	if _, err := st.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	reg.Source = store.SourceMeta{URI: "/a-first.txt", Kind: "file", SrcHash: "ha"}
	if _, err := st.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}

	res, err := Query(t.Context(), st, "", []string{"multisource"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if len(hits) != 1 || hits[0].Source != "a-first.txt" {
		t.Fatalf("want 결정적 a-first.txt(uri ASC, store.sourceOf와 일치), got %+v", hits)
	}
}

func TestQuery_SnippetStemPrefixFallback(t *testing.T) {
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
	b.WriteString("cached\n")
	for i := 0; i < 100; i++ {
		b.WriteString(line)
	}
	text := b.String()
	regOne(t, st, "/stem.txt", "hstem", text)

	res, err := Query(t.Context(), st, "", []string{"caching"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := res[0].Hits
	if len(hits) == 0 {
		t.Fatal("want >=1 hit(porter stem match)")
	}
	snip := hits[0].Snippet
	if !strings.Contains(snip, "cached") {
		t.Fatalf("want snippet contain 'cached'(stem-prefix fallback), got %q", snip)
	}
	if snip == text[:500] {
		t.Fatalf("want snippet != 앞 500B fallback(증명: 중반 배치), got equal")
	}
}
