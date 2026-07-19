package search

import (
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/session"
)

// seedEventsT: 3개 이벤트를 session.Append로 직접 시딩(porter/trigram 각 우세 쿼리 2 테스트가
// 공유하는 코퍼스 — internal/store seedT의 이벤트판, 동일 텍스트로 대칭 검증).
func seedEventsT(t *testing.T) *session.DB {
	t.Helper()
	d, err := session.Open(t.TempDir(), session.Options{Producer: "test/events"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, summary := range []string{
		"the cache was cached quickly",
		"useEffect cleanup runs",
		"caching useEffect guide",
	} {
		if _, _, _, err := d.Append(session.Event{Type: "note", Summary: summary}); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func hasSummary(hits []EventHit, sub string) bool {
	for _, h := range hits {
		if strings.Contains(h.Summary, sub) {
			return true
		}
	}
	return false
}

// TestQueryEvents_PorterDominant — 브리프 Step1 ⑤ porter 우세 쿼리: "caching"은 porter
// stem으로 "cache"/"cached"/"caching"을 모두 묶는다(RRF가 porter 전용 매치도 병합해 반환).
func TestQueryEvents_PorterDominant(t *testing.T) {
	d := seedEventsT(t)
	hits, err := QueryEvents(t.Context(), d.Reader(), "caching", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSummary(hits, "cache was cached") || !hasSummary(hits, "caching useEffect") {
		t.Fatalf("want cache+caching summaries(porter stem), got %+v", hits)
	}
}

// TestQueryEvents_TrigramDominant — 브리프 Step1 ⑤ trigram 우세 쿼리: "useEff"는 porter
// 토큰 매치가 없고 trigram substring으로만 "useEffect" 포함 이벤트를 찾는다.
func TestQueryEvents_TrigramDominant(t *testing.T) {
	d := seedEventsT(t)
	hits, err := QueryEvents(t.Context(), d.Reader(), "useEff", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSummary(hits, "useEffect cleanup") || !hasSummary(hits, "caching useEffect guide") {
		t.Fatalf("want useEffect summaries(trigram substring), got %+v", hits)
	}
	if hasSummary(hits, "cache was cached") {
		t.Fatalf("cache summary(useEff 없음) 매치되면 안 됨, got %+v", hits)
	}
}

// TestQueryEvents_SupersededFlag — 브리프 Step1 ④(G5): supersede된 이벤트도 검색되되
// Superseded: true로 표기된다(색인에서 제거하지 않음, 설계 §2.3).
func TestQueryEvents_SupersededFlag(t *testing.T) {
	dir := t.TempDir()
	d, err := session.Open(dir, session.Options{Producer: "test/superseded"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	_, origID, _, err := d.Append(session.Event{Type: "decision", Summary: "adopt widgetfoo approach"})
	if err != nil {
		t.Fatal(err)
	}
	_, newID, _, err := d.Append(session.Event{Type: "decision", Summary: "revise widgetfoo approach", Supersedes: origID})
	if err != nil {
		t.Fatal(err)
	}

	hits, err := QueryEvents(t.Context(), d.Reader(), "widgetfoo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits(superseded 포함), got %d: %+v", len(hits), hits)
	}
	var sawOrig, sawNew bool
	for _, h := range hits {
		switch h.EventID {
		case origID:
			sawOrig = true
			if !h.Superseded {
				t.Fatalf("original event(%s) want Superseded=true, got false: %+v", origID, h)
			}
		case newID:
			sawNew = true
			if h.Superseded {
				t.Fatalf("new event(%s) want Superseded=false, got true: %+v", newID, h)
			}
		}
	}
	if !sawOrig || !sawNew {
		t.Fatalf("want both orig+new event in hits, got %+v", hits)
	}
}
