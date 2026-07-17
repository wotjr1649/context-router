// Package search — FTS 질의→BM25→RRF 병합→스니펫·예산. 설계서 §4.1.
package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/wotjr1649/context-router/internal/store"
)

type Hit struct {
	ArtifactID, ChunkID                int64
	Source                             string
	LineStart, LineEnd                 int
	Snippet                            string
	Score                              float64
	Stale, Redacted, SourceCoordsExact bool
}

type QueryResult struct {
	Query     string
	Hits      []Hit
	Truncated bool
}

const rrfK = 60

// normalizeQuery splits q into whitespace 토큰으로 나눠 porter/trigram 각각의 FTS5
// MATCH 식을 만든다. 토큰마다 이중따옴표로 감싸(내부 "는 ""로 이스케이프) 하이픈·콜론
// 등 FTS5 특수문자를 리터럴로 취급시키고 " AND "로 묶는다. trigram 토크나이저는
// 3자 미만 질의를 거부하므로 그런 토큰은 trigram 식에서 제외한다.
func normalizeQuery(q string) (porter, trigram string) {
	var pt, tt []string
	for _, tok := range strings.Fields(q) {
		esc := `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
		pt = append(pt, esc)
		if utf8.RuneCountInString(tok) >= 3 {
			tt = append(tt, esc)
		}
	}
	return strings.Join(pt, " AND "), strings.Join(tt, " AND ")
}

// bm25Rank: table(패키지 상수만 전달되는 fts_porter/fts_trigram — 사용자 입력 아님,
// 식별자 연결이 안전) MATCH match 결과를 bm25() 오름차순(=관련도 높은 순)으로 상위
// n개 chunk id 반환. match==""면(모든 토큰이 trigram 제외 등) 질의 자체를 생략한다.
func bm25Rank(ctx context.Context, db *sql.DB, table, match string, n int) ([]int64, error) {
	if match == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		"SELECT rowid FROM "+table+" WHERE "+table+" MATCH ? ORDER BY bm25("+table+") LIMIT ?", match, n)
	if err != nil {
		return nil, fmt.Errorf("search: %s match: %w", table, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// rrfMerge: 순위 리스트(들)를 RRF(k=60)로 병합한 chunk id→score 맵. 한 chunk id가
// 여러 리스트에 나타나면 점수가 합산되고(다중 인덱스 매치 우대), 반환 맵은 chunk id당
// 항목이 하나뿐이라 중복이 자연히 제거된다.
func rrfMerge(lists ...[]int64) map[int64]float64 {
	scores := make(map[int64]float64)
	for _, list := range lists {
		for rank, id := range list {
			scores[id] += 1.0 / float64(rrfK+rank+1)
		}
	}
	return scores
}

type scoredID struct {
	id    int64
	score float64
}

// topN: scores를 점수 내림차순(동점이면 id 오름차순 — 결정적 순서)으로 정렬해 상위
// n개를 반환한다.
func topN(scores map[int64]float64, n int) []scoredID {
	list := make([]scoredID, 0, len(scores))
	for id, sc := range scores {
		list = append(list, scoredID{id, sc})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].id < list[j].id
	})
	if len(list) > n {
		list = list[:n]
	}
	return list
}

// hitQuery: chunks→artifacts→sources를 artifact_id로 조인한다. 한 artifact에 소스가
// 여러 개면 LIMIT 1이 임의 1행을 고른다(브리프: "다중 소스면 아무 1행" — JOIN 형태는
// 단순 우선).
const hitQuery = `SELECT c.artifact_id, c.line_start, c.line_end, c.text, a.redaction, s.uri, s.source_kind,
	s.src_size, s.src_mtime_ns, s.src_hash
	FROM chunks c JOIN artifacts a ON a.id = c.artifact_id JOIN sources s ON s.artifact_id = a.id
	WHERE c.id = ? LIMIT 1`

// loadHit: chunkID 1건을 Hit으로 채운다. q는 스니펫 매치 토큰 탐색에, staleCache는 Query
// 호출 1회 내 uri별 stale 판정 캐시(§3.6 "같은 uri는 1회만 검사")에 쓰인다.
func loadHit(ctx context.Context, db *sql.DB, chunkID int64, score float64, q string, staleCache map[string]bool) (Hit, error) {
	var h Hit
	var text, redaction, uri, kind string
	var srcSize, srcMtimeNS sql.NullInt64
	var srcHash sql.NullString
	err := db.QueryRowContext(ctx, hitQuery, chunkID).
		Scan(&h.ArtifactID, &h.LineStart, &h.LineEnd, &text, &redaction, &uri, &kind, &srcSize, &srcMtimeNS, &srcHash)
	if err != nil {
		return Hit{}, fmt.Errorf("search: load chunk %d: %w", chunkID, err)
	}
	h.ChunkID = chunkID
	h.Score = score
	h.Source = relativizeSource(uri)
	h.Snippet = snippetWindow(text, firstMatchToken(q, text))
	h.Redacted = redaction == "spans"
	h.SourceCoordsExact = redaction == "none" && kind == "file"
	h.Stale = isStale(uri, kind, srcSize.Int64, srcMtimeNS.Int64, srcHash.String, staleCache)
	return h, nil
}

// relativizeSource: uri(Fold된 절대경로 또는 inline:제목)에서 절대 접두(드라이브/루트/UNC
// 선행 빈 세그먼트)를 걷어내고 마지막 3개 '/' 세그먼트만 남긴다. Query는 projectRoot를 모르는
// 채로 호출되므로 완전한 project-relative 경로 대신 근사치다(설계 §4.1 source(project-relative);
// 절대경로 전체 노출 금지가 계약).
func relativizeSource(uri string) string {
	parts := strings.Split(uri, "/")
	for len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) > 3 {
		parts = parts[len(parts)-3:]
	}
	return strings.Join(parts, "/")
}

const (
	snippetHalf     = 250
	snippetFallback = 500
)

// firstMatchToken: q의 공백 토큰 중 text에 대소문자 무시 부분문자열로 존재하는 첫 토큰을
// 반환한다(없으면 "").
func firstMatchToken(q, text string) string {
	lower := strings.ToLower(text)
	for _, tok := range strings.Fields(q) {
		if strings.Contains(lower, strings.ToLower(tok)) {
			return tok
		}
	}
	return ""
}

// capBytes: s를 최대 n바이트로 자르되 UTF-8 룬 경계에서 멈춘다(store.snapUTF8과 동형 —
// search는 별도 패키지라 로컬로 재구현).
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// snippetWindow: tok의 text 내 첫 출현 위치(대소문자 무시) 중심 ±snippetHalf 바이트 창을
// 반환하고, 창 경계가 단어 중간이면 안쪽으로 스냅한다. 공백/개행은 항상 1바이트 ASCII라
// 스냅 결과는 자동으로 UTF-8 경계다. tok==""나 미발견이면 앞 500B(UTF-8 안전 절단).
func snippetWindow(text, tok string) string {
	if tok == "" {
		return capBytes(text, snippetFallback)
	}
	idx := strings.Index(strings.ToLower(text), strings.ToLower(tok))
	if idx < 0 {
		return capBytes(text, snippetFallback)
	}
	start, end := idx-snippetHalf, idx+snippetHalf
	if start <= 0 { // 0 포함: 텍스트 시작이면 잘린 단어가 없음 — 스냅 불필요
		start = 0
	} else if i := strings.IndexAny(text[start:idx], " \n"); i >= 0 {
		start += i + 1 // 앞쪽 잘린 단어 제외
	}
	if end >= len(text) { // len(text) 포함: 텍스트 끝이면 잘린 단어가 없음
		end = len(text)
	} else if i := strings.LastIndexAny(text[idx:end], " \n"); i >= 0 {
		end = idx + i // 뒤쪽 잘린 단어 제외
	}
	return text[start:end]
}

// isStale: source_kind!="file"이면 항상 false(설계 §3.6). file이면 os.Stat으로 size/mtime_ns를
// sources 행과 비교하고, 불일치 시 원본을 재해시해 src_hash와 대조한다(content_hash는 저장본
// 주소라 원본 대조에 미사용). Stat 실패도 stale=true. cache는 Query 호출 1회 동안 uri별 1회만
// 계산하도록 재사용된다.
func isStale(uri, kind string, size, mtimeNS int64, srcHash string, cache map[string]bool) bool {
	if kind != "file" {
		return false
	}
	if v, ok := cache[uri]; ok {
		return v
	}
	stale := statStale(uri, size, mtimeNS, srcHash)
	cache[uri] = stale
	return stale
}

func statStale(uri string, size, mtimeNS int64, srcHash string) bool {
	p := filepath.FromSlash(uri)
	fi, err := os.Stat(p)
	if err != nil {
		return true
	}
	if fi.Size() == size && fi.ModTime().UnixNano() == mtimeNS {
		return false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return true
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]) != srcHash
}

// Query: queries 각각에 대해 fts_porter+fts_trigram을 병행 질의하고 상위 limit×4개씩을
// RRF(k=60)로 병합해 상위 limit개 Hit를 반환한다. budgetBytes>0이면 쿼리 수로 균등 분할해
// 스니펫 바이트 예산을 배분하고 미사용분은 다음 쿼리로 이월한다(설계 §4.1). budgetBytes<=0은
// 무제한.
func Query(ctx context.Context, st *store.Store, queries []string, limit, budgetBytes int) ([]QueryResult, error) {
	db := st.Reader()
	results := make([]QueryResult, 0, len(queries))
	staleCache := make(map[string]bool)
	avail := budgetBytes
	for i, q := range queries {
		porter, trigram := normalizeQuery(q)
		pIDs, err := bm25Rank(ctx, db, "fts_porter", porter, limit*4)
		if err != nil {
			return nil, err
		}
		tIDs, err := bm25Rank(ctx, db, "fts_trigram", trigram, limit*4)
		if err != nil {
			return nil, err
		}
		top := topN(rrfMerge(pIDs, tIDs), limit)
		hits := make([]Hit, 0, len(top))
		for _, sc := range top {
			h, err := loadHit(ctx, db, sc.id, sc.score, q, staleCache)
			if err != nil {
				return nil, err
			}
			hits = append(hits, h)
		}
		qr := QueryResult{Query: q, Hits: hits}
		if budgetBytes > 0 {
			share := avail / (len(queries) - i)
			var used int
			qr.Hits, used, qr.Truncated = applyBudget(hits, share)
			avail -= used
		}
		results = append(results, qr)
	}
	return results, nil
}

// applyBudget: hits(이미 점수순 정렬됨)를 순서대로 채우다 스니펫 바이트 누적합이 share를
// 초과하는 hit을 만나면 그 hit부터 절단한다(설계 §4.1 예산 배분).
func applyBudget(hits []Hit, share int) (kept []Hit, used int, truncated bool) {
	for _, h := range hits {
		n := len(h.Snippet)
		if used+n > share {
			return kept, used, true
		}
		used += n
		kept = append(kept, h)
	}
	return kept, used, false
}
