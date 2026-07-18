// Package search — FTS 질의→BM25→RRF 병합→스니펫·예산. 설계서 §4.1.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
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
// 3자 미만 질의를 거부하므로 그런 토큰은 trigram 식에서 제외하고 shortToks(원문 그대로)로
// 반환한다 — 호출자가 trigram 후보를 그 토큰들의 리터럴 존재로 재필터링해 AND 계약을
// 지키는 데 쓴다(Fix E).
func normalizeQuery(q string) (porter, trigram string, shortToks []string) {
	var pt, tt []string
	for _, tok := range strings.Fields(q) {
		esc := `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
		pt = append(pt, esc)
		if utf8.RuneCountInString(tok) >= 3 {
			tt = append(tt, esc)
		} else {
			shortToks = append(shortToks, tok)
		}
	}
	return strings.Join(pt, " AND "), strings.Join(tt, " AND "), shortToks
}

// filterShortTokenCandidates: trigram 후보(ids, 순위 순서 유지) 중 shortToks(<3자라
// trigram 식에서 빠진 토큰)가 하나라도 chunk.text에 리터럴로 없는 후보를 버린다 — trigram
// 식이 짧은 토큰을 누락해 AND 계약이 깨지는 것을 막는다(Fix E). porter 후보는 이미 모든
// 토큰을 AND로 포함하므로 이 필터를 거치지 않는다.
func filterShortTokenCandidates(ctx context.Context, db *sql.DB, ids []int64, shortToks []string) ([]int64, error) {
	if len(shortToks) == 0 || len(ids) == 0 {
		return ids, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := db.QueryContext(ctx,
		"SELECT id, text FROM chunks WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	texts := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		texts[id] = text
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kept := make([]int64, 0, len(ids))
	for _, id := range ids {
		text, ok := texts[id]
		if !ok {
			continue // orphan id(재색인 등) — loadHit 단계와 동일하게 skip
		}
		all := true
		for _, tok := range shortToks {
			if foldIndex(text, tok) < 0 {
				all = false
				break
			}
		}
		if all {
			kept = append(kept, id)
		}
	}
	return kept, nil
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
	s.src_size, s.src_mtime_ns, s.src_hash, s.extraction
	FROM chunks c JOIN artifacts a ON a.id = c.artifact_id JOIN sources s ON s.artifact_id = a.id
	WHERE c.id = ? LIMIT 1`

// loadHit: chunkID 1건을 Hit으로 채운다. q는 스니펫 매치 토큰 탐색에, projectRoot는 Source
// project-relative 계산에, staleCache는 Query 호출 1회 내 uri별 stale 판정 캐시(§3.6 "같은
// uri는 1회만 검사")에 쓰인다.
func loadHit(ctx context.Context, db *sql.DB, chunkID int64, score float64, q, projectRoot string, staleCache map[string]bool) (Hit, error) {
	var h Hit
	var text, redaction, uri, kind string
	var srcSize, srcMtimeNS sql.NullInt64
	var srcHash, extraction sql.NullString
	err := db.QueryRowContext(ctx, hitQuery, chunkID).
		Scan(&h.ArtifactID, &h.LineStart, &h.LineEnd, &text, &redaction, &uri, &kind, &srcSize, &srcMtimeNS, &srcHash, &extraction)
	if err != nil {
		return Hit{}, fmt.Errorf("search: load chunk %d: %w", chunkID, err)
	}
	h.ChunkID = chunkID
	h.Score = score
	h.Source = RelativizeSource(projectRoot, uri)
	h.Snippet = snippetWindow(text, firstMatchToken(q, text))
	h.Redacted = redaction == "spans"
	// SourceCoordsExact: file/inline은 저장된 좌표가 원문을 그대로 가리켜 정확하다. web처럼
	// 변환을 거친 kind는 extraction 유무와 무관하게 항상 false(원문 좌표 자체가 없음).
	h.SourceCoordsExact = redaction == "none" && extraction.String == "" && (kind == "file" || kind == "inline")
	h.Stale = isStale(uri, kind, srcSize.Int64, srcMtimeNS.Int64, srcHash.String, staleCache)
	return h, nil
}

// RelativizeSource: uri(ident.Fold된 절대경로 또는 inline:제목)를 projectRoot(ident.Fold된
// canonical project root) 기준 project-relative 경로로 바꾼다(설계 §4.1 source(project-relative);
// 절대경로 전체 노출 금지가 계약). uri가 projectRoot+"/"로 시작하면 그 접두를 잘라낸 진짜
// 상대경로를 반환한다. 아니면(프로젝트 밖 uri, projectRoot 불명 등) fallback: 선행 "/" 제거,
// 드라이브 세그먼트("c:" 등) 제거 후 마지막 최대 3개 '/' 세그먼트만 남기는 근사치.
func RelativizeSource(projectRoot, uri string) string {
	if projectRoot != "" {
		if rel, ok := strings.CutPrefix(uri, projectRoot+"/"); ok {
			return rel
		}
	}
	parts := strings.Split(uri, "/")
	for len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && isDriveSeg(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) > 3 {
		parts = parts[len(parts)-3:]
	}
	return strings.Join(parts, "/")
}

// isDriveSeg: Windows 드라이브 세그먼트("c:" 등, 정규식 ^[a-z]:$와 동형)인지 검사한다.
// ident.Fold가 드라이브 문자를 소문자화하므로 소문자만 확인한다.
func isDriveSeg(s string) bool {
	return len(s) == 2 && s[1] == ':' && s[0] >= 'a' && s[0] <= 'z'
}

const (
	snippetHalf     = 250
	snippetFallback = 500
)

// firstMatchToken: q의 공백 토큰 중 text에 대소문자 무시 부분문자열로 존재하는 첫 토큰을
// 반환한다. 리터럴 매치가 하나도 없으면(예: 쿼리 "caching"·본문 "cached" — porter 스템만
// 일치) 각 토큰의 접두(max(4, len-3)룬)로 재시도해 어간 근사 매치를 찾는다. 그래도 없으면
// ""(snippetWindow가 앞 500B로 폴백).
// ponytail: 어간 근사 — FTS5 snippet() 도입 시 대체.
func firstMatchToken(q, text string) string {
	toks := strings.Fields(q)
	for _, tok := range toks {
		if foldIndex(text, tok) >= 0 {
			return tok
		}
	}
	for _, tok := range toks {
		p := tokenPrefix(tok)
		if p != "" && foldIndex(text, p) >= 0 {
			return p
		}
	}
	return ""
}

// tokenPrefix: tok 앞 max(4, len-3)룬(스템 근사 접두). tok가 그보다 짧으면 tok 전체.
func tokenPrefix(tok string) string {
	r := []rune(tok)
	n := len(r) - 3
	if n < 4 {
		n = 4
	}
	if n > len(r) {
		n = len(r)
	}
	return string(r[:n])
}

// foldIndex: text에서 token을 대소문자 무시로 찾아 원본 text 기준 바이트 오프셋을
// 반환한다(없으면 -1). strings.ToLower(text) 사본에서 구한 idx를 원본에 그대로 쓰면
// 케이스폴딩 시 바이트 길이가 바뀌는 룬(U+023A 확장/U+212A 축소 등)에서 오프셋이
// 어긋나 panic이나 무효 UTF-8을 낼 수 있어, 룬 단위로 직접 비교해 원본 오프셋을
// 보존한다.
func foldIndex(text, token string) int {
	if token == "" {
		return -1
	}
	tr := []rune(strings.ToLower(token))
	for i := 0; i < len(text); {
		r, sz := utf8.DecodeRuneInString(text[i:])
		if unicode.ToLower(r) == tr[0] {
			j, k := i, 0
			for k < len(tr) {
				rr, s2 := utf8.DecodeRuneInString(text[j:])
				if s2 == 0 || unicode.ToLower(rr) != tr[k] {
					break
				}
				j += s2
				k++
			}
			if k == len(tr) {
				return i
			}
		}
		i += sz
	}
	return -1
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

// snippetWindow: tok의 text 내 첫 출현 위치(대소문자 무시, foldIndex — 원본 오프셋 보존)
// 중심 ±snippetHalf 바이트 창을 반환하고, 창 경계가 단어 중간이면 안쪽으로 스냅한다.
// 공백/개행이 없어 스냅이 실패해도(예: 공백 없는 CJK 장문) 마지막에 항상 룬 경계로
// 보정하므로 무효 UTF-8이 나오지 않는다. tok==""나 미발견이면 앞 500B(UTF-8 안전 절단).
func snippetWindow(text, tok string) string {
	if tok == "" {
		return capBytes(text, snippetFallback)
	}
	idx := foldIndex(text, tok)
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
	start, end = snapWindow(text, start, end) // 공백 스냅 성공 여부와 무관하게 항상 룬 경계 보정
	return text[start:end]
}

// snapWindow: store.snapUTF8과 동형(문자열판) — [start,end)를 UTF-8 룬 경계로 스냅한다.
// search는 별도 패키지라 로컬로 재구현(capBytes와 동일 사유). start는 RuneStart까지
// 후퇴하고, 그 지점부터 룬을 전진 소비하며 end를 넘지 않는 지점에서 멈춘다.
func snapWindow(s string, start, end int) (int, int) {
	for start > 0 && start < len(s) && !utf8.RuneStart(s[start]) {
		start--
	}
	pos := start
	for pos < end {
		r, size := utf8.DecodeRuneInString(s[pos:])
		if size == 0 || (r == utf8.RuneError && size <= 1) || pos+size > end {
			break
		}
		pos += size
	}
	return start, pos
}

// isStale: 판정 자체는 store.StaleOf(설계 §3.6)에 위임한다. cache는 Query 호출 1회 동안
// uri별 1회만 계산하도록 재사용된다(kind!="file" 단락은 store.StaleOf 호출조차 생략하는
// 빠른 경로).
func isStale(uri, kind string, size, mtimeNS int64, srcHash string, cache map[string]bool) bool {
	if kind != "file" {
		return false
	}
	if v, ok := cache[uri]; ok {
		return v
	}
	stale := store.StaleOf(store.SourceInfo{URI: uri, Kind: kind, Size: size, MtimeNS: mtimeNS, SrcHash: srcHash})
	cache[uri] = stale
	return stale
}

// Query: queries 각각에 대해 fts_porter+fts_trigram을 병행 질의하고 상위 limit×4개씩을
// RRF(k=60)로 병합해 상위 limit개 Hit를 반환한다. budgetBytes>0이면 쿼리 수로 균등 분할해
// 스니펫 바이트 예산을 배분하고 미사용분은 다음 쿼리로 이월한다(설계 §4.1). budgetBytes<=0은
// 무제한. projectRoot는 ident.Fold된 canonical project root — Hit.Source의 project-relative
// 계산 기준(Fix B).
func Query(ctx context.Context, st *store.Store, projectRoot string, queries []string, limit, budgetBytes int) ([]QueryResult, error) {
	db := st.Reader()
	results := make([]QueryResult, 0, len(queries))
	staleCache := make(map[string]bool)
	avail := budgetBytes
	for i, q := range queries {
		porter, trigram, shortToks := normalizeQuery(q)
		pIDs, err := bm25Rank(ctx, db, "fts_porter", porter, limit*4)
		if err != nil {
			return nil, err
		}
		tIDs, err := bm25Rank(ctx, db, "fts_trigram", trigram, limit*4)
		if err != nil {
			return nil, err
		}
		tIDs, err = filterShortTokenCandidates(ctx, db, tIDs, shortToks)
		if err != nil {
			return nil, err
		}
		top := topN(rrfMerge(pIDs, tIDs), limit)
		hits := make([]Hit, 0, len(top))
		for _, sc := range top {
			h, err := loadHit(ctx, db, sc.id, sc.score, q, projectRoot, staleCache)
			if errors.Is(err, sql.ErrNoRows) {
				continue // 재색인 orphan(구 chunk — sources.artifact_id가 신규를 가리켜 no-row): skip, limit*4 후보 폭이 보충
			}
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
