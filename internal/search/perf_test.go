//go:build perf

// 게이트6 성능 스모크(설계 §12 "5,000 doc 성능 스모크", 계획3 Codex 교차리뷰 P2-2) — 기본
// `go test ./...`/CI에서는 빌드조차 제외되고 로컬에서만
// `go test -tags perf -run TestPerf_5000Docs -v ./internal/search/`로 실행한다. 합성 코퍼스
// 생성 루프는 이 성능 스모크 전용으로 명시 허용(브리프 — 평소 D13 anti-fragmentation
// 규율은 실제 리터럴 테스트데이터에 적용되고, 성능 측정용 합성 데이터는 예외).
// 회귀 문지기가 아니라(하드웨어 편차) 실측 로그가 목적 — 결과는 docs/gates-v0.0.1-ko.md
// 게이트 6에 기록한다.
package search

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/store"
)

// fillerPool: perf 코퍼스의 어휘 다양성용 — 실측 결과 모든 문서가 동일한 리터럴 보일러플레이트
// 문구를 그대로 반복하면(1차 시도) trigram FTS 포스팅 리스트가 병적으로 커져(문서 5000개가
// 사실상 거의 동일한 substring을 공유) 실제 다양한 산문 코퍼스보다 훨씬 느린, 오도된 실측이
// 나온다(로컬 실측: 반복 리터럴로 쿼리당 최대 2.2s, 어휘를 섞으니 아래처럼 정상화됨). 그래서
// 각 문서마다 이 풀에서 서로 다른 순열로 단어를 뽑아 채운다.
var fillerPool = []string{
	"system", "network", "process", "memory", "storage", "query", "index", "record",
	"cluster", "service", "handler", "request", "response", "buffer", "stream", "token",
	"session", "context", "module", "package", "runtime", "thread", "socket", "packet",
	"cache", "policy", "schema", "table", "column", "vector", "matrix", "graph",
	"queue", "worker", "engine", "driver", "client", "server", "gateway", "proxy",
	"router", "sensor", "signal", "metric", "report", "ledger", "config", "manifest",
	"pipeline", "artifact",
}

// fillerDoc: 문서 i의 본문 — topic(질의 대상 단어) 1회 + fillerPool에서 문서마다 다른 순열로
// 뽑은 15~24개 단어. 인접 문서끼리도 실제 겹치는 부분이 작도록 (i*31+j*17) 스트라이드로 섞는다.
func fillerDoc(i int, topic string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "document %d discusses %s in detail. ", i, topic)
	n := 15 + i%10
	for j := 0; j < n; j++ {
		b.WriteString(fillerPool[(i*31+j*17)%len(fillerPool)])
		b.WriteByte(' ')
	}
	return b.String()
}

func TestPerf_5000Docs(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer s.Close()

	const nDocs = 5000
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}

	indexStart := time.Now()
	for i := 0; i < nDocs; i++ {
		body := fillerDoc(i, words[i%len(words)])
		// Kind: "inline"(파일로 안 뒷받침되는 합성 문서) — "file"이면 검색 히트마다
		// isStale이 실 os.Stat을 시도해(경로가 실재하지 않아 매번 실패) 색인/검색 자체가
		// 아닌 부수적 I/O 지연이 스모크 결과에 섞인다.
		if _, err := s.Register(context.Background(), store.Registration{
			StoredBytes: []byte(body), MediaType: "text/plain",
			Source: store.SourceMeta{URI: fmt.Sprintf("inline:perf-%05d", i), Kind: "inline", SrcHash: fmt.Sprintf("h%d", i)},
			Chunks: []store.Chunk{{Ordinal: 0, Text: body}},
		}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	indexElapsed := time.Since(indexStart)

	const nQueries = 20
	queries := make([]string, nQueries)
	for i := range queries {
		queries[i] = words[i%len(words)]
	}
	queryStart := time.Now()
	for _, q := range queries {
		if _, err := Query(context.Background(), s, "", []string{q}, 10, 8192); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	queryElapsed := time.Since(queryStart)

	t.Logf("perf: index %d docs in %v (%.1f docs/s) | %d queries in %v (%.2f ms/query)",
		nDocs, indexElapsed, float64(nDocs)/indexElapsed.Seconds(),
		nQueries, queryElapsed, float64(queryElapsed.Milliseconds())/nQueries)
}
