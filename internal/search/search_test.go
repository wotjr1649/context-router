package search

import (
	"testing"

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
