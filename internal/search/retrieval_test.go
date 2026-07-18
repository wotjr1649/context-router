package search

// 게이트 1·2 하네스(설계 §12). 게이트 1 = oracle(ctxscribe 1.3.0) 동등성 골든 대조,
// 게이트 2 = 라벨링된 코퍼스에서 recall@k 회귀 플로어. 공유 코퍼스는
// testdata/oracle/corpus/(실제 store·실제 ingest·실제 search — mock 없음), oracle 골든
// 생성 절차는 testdata/oracle/README.md, 필드 매핑·동등성 판정 기준은
// docs/oracle-mapping-ko.md 참조.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/store"
)

// minRecallAt5: 최초 실측 기준선(회귀 플로어). 2026-07 최초 실행 실측 mean recall@5 =
// 0.9306(= (1.0×10 + 2/3 + 1/2)/12 = 0.930556), recall@10 = 1.0000. 코퍼스·라벨·검색
// 구현이 모두 결정적이라 실측은 항상 동일해야 한다. 플로어는 부동소수 반올림 취약성을
// 피해 실측 바로 아래(0.9305)로 고정 — 이 값을 밑돌면 검색 품질 회귀다. 값을 올리려면
// 코퍼스/라벨/구현 개선을 재실측해 함께 갱신할 것.
const minRecallAt5 = 0.9305

// minRecallAt10: 최초 실측 recall@10 = 1.0000 직하 플로어. 6~10위 구간 퇴행(정답이
// top-5엔 남아도 top-10 밖으로 밀리는 회귀)도 게이트가 잡도록 `<`로 비교한다.
const minRecallAt10 = 0.9999

// oracleRepoRel: 게이트 1·2 공유 코퍼스 루트(repo-root testdata, 패키지 기준 ../../).
func testdataPath(t *testing.T, parts ...string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(append([]string{"..", "..", "testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// indexCorpus: 공유 코퍼스를 실제 ingest 경로로 ctr store에 색인하고, Query에 넘길
// project-relative 기준 루트(ident.Fold된 canonical 코퍼스 경로)를 반환한다.
func indexCorpus(t *testing.T) (*store.Store, string, ingest.Report) {
	t.Helper()
	corpus, err := filepath.EvalSymlinks(testdataPath(t, "oracle", "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rep, err := ingest.Run(t.Context(), st, corpus, nil, ingest.Request{Path: corpus})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed < 12 {
		t.Fatalf("코퍼스 색인 %d개 < 12 (skipped=%+v)", rep.Indexed, rep.Skipped)
	}
	return st, ident.Fold(corpus), rep
}

// chunkCountsBySource: 색인된 소스 파일(basename)별 청크 수. 코퍼스 형태 회귀 가드용
// (실제 store 조회 — chunks↔sources join). 코퍼스 문서는 내용이 서로 달라 artifact↔source가
// 1:1이라 s.uri별 그룹이 파일별 청크 수와 같다.
func chunkCountsBySource(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	rows, err := st.Reader().Query(
		`SELECT s.uri, COUNT(*) FROM chunks c JOIN sources s ON s.artifact_id = c.artifact_id GROUP BY s.uri`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var uri string
		var n int
		if err := rows.Scan(&uri, &n); err != nil {
			t.Fatal(err)
		}
		m[filepath.Base(uri)] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return m
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

// distinctSourcesTopK: hits(랭크순)의 상위 k개 청크에서 소스 파일(basename) 집합을
// 등장 순서대로 중복 없이 뽑는다. oracle golden도 basename 기준이라 두 검색계의 경로
// 규약 차이(oracle=label, ctr=project-relative)를 basename으로 정규화해 비교한다.
func distinctSourcesTopK(hits []Hit, k int) []string {
	var out []string
	for i, h := range hits {
		if i >= k {
			break
		}
		b := filepath.Base(h.Source)
		if !contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func intersectCount(a, b []string) int {
	n := 0
	for _, x := range a {
		if contains(b, x) {
			n++
		}
	}
	return n
}

// TestOracleEquivalence — 게이트 1(설계 §12): 동일 corpus·동일 쿼리에서 oracle top-k
// 소스 파일 집합과 ctr top-k 소스 파일 집합의 교집합 ≥ 2 를 전체 쿼리의 80%(브리프
// 기준 "10개 중 8개") 이상 만족해야 한다. golden(oracle 실측)은 testdata/oracle/golden.json에
// 커밋되어 있고, 이 테스트는 그 골든을 읽어 ctr 실검색과 대조한다(oracle 재실행 아님).
func TestOracleEquivalence(t *testing.T) {
	var golden struct {
		K             int                 `json:"k"`
		OracleVersion string              `json:"oracle_version"`
		Results       map[string][]string `json:"results"`
	}
	loadJSON(t, testdataPath(t, "oracle", "golden.json"), &golden)
	var q struct {
		Queries []string `json:"queries"`
		OracleK int      `json:"oracle_k"`
	}
	loadJSON(t, testdataPath(t, "oracle", "queries.json"), &q)
	// 기준 전제 고정(Codex P2): k==3이 "교집합>=2" 판정의 전제 — 침묵 완화 방지.
	if golden.K != 3 {
		t.Fatalf("golden.k=%d != 3 (교집합>=2 기준의 전제)", golden.K)
	}
	if q.OracleK != 3 {
		t.Fatalf("queries.oracle_k=%d != 3", q.OracleK)
	}
	// golden은 oracle 1.3.0 참조 구현 산출물이어야 한다(버전 고정).
	if golden.OracleVersion != "1.3.0" {
		t.Fatalf("golden.oracle_version=%q != 1.3.0 — oracle 참조 구현 버전 불일치", golden.OracleVersion)
	}
	if len(q.Queries) < 10 {
		t.Fatalf("계약 위반: queries=%d < 10", len(q.Queries))
	}

	st, root, _ := indexCorpus(t)
	pass := 0
	for _, query := range q.Queries {
		oracle := golden.Results[query]
		if len(oracle) == 0 {
			t.Fatalf("golden에 쿼리 %q 없음 — gen-golden.mjs 재생성 필요", query)
		}
		res, err := Query(t.Context(), st, root, []string{query}, golden.K, 0)
		if err != nil {
			t.Fatal(err)
		}
		ctr := distinctSourcesTopK(res[0].Hits, golden.K)
		inter := intersectCount(oracle, ctr)
		ok := inter >= 2
		if ok {
			pass++
		}
		mark := "MISS"
		if ok {
			mark = "ok"
		}
		t.Logf("[equiv] %-20q ∩=%d %-4s oracle=%v ctr=%v", query, inter, mark, oracle, ctr)
	}
	total := len(q.Queries)
	t.Logf("[equiv] 동등성 통과 %d/%d (교집합>=2, 기준 80%%)", pass, total)
	// 기준 미달이면 임의 완화 금지 — 원인 분석 후 매핑 문서 "의도적 차이"에 기록하고 BLOCKED 보고.
	if pass*10 < total*8 {
		t.Fatalf("oracle 동등성 미달 %d/%d (<80%%) — 기준 임의 완화 금지, BLOCKED 보고 대상", pass, total)
	}
}

// TestRetrievalRecall — 게이트 2(설계 §12): 라벨링된 코퍼스에서 recall@5/@10을 계산해
// 회귀 플로어(minRecallAt5) 이상을 단언하고 실측을 로그로 남긴다. recall@k = 쿼리별
// |정답 소스 ∩ 상위 k 청크의 distinct 소스| / |정답 소스| 의 쿼리 평균.
func TestRetrievalRecall(t *testing.T) {
	var lbl struct {
		Labels map[string][]string `json:"labels"`
	}
	loadJSON(t, testdataPath(t, "retrieval", "labels.json"), &lbl)
	if len(lbl.Labels) < 10 {
		t.Fatalf("계약 위반: 라벨 쿼리 %d개 < 10", len(lbl.Labels))
	}
	queries := make([]string, 0, len(lbl.Labels))
	for q := range lbl.Labels {
		queries = append(queries, q)
	}
	sort.Strings(queries) // 결정적 순서

	st, root, _ := indexCorpus(t)
	var sum5, sum10 float64
	for _, query := range queries {
		relevant := lbl.Labels[query]
		if len(relevant) == 0 {
			t.Fatalf("쿼리 %q 정답 라벨 비어 있음", query)
		}
		res, err := Query(t.Context(), st, root, []string{query}, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		hits := res[0].Hits
		r5 := float64(intersectCount(relevant, distinctSourcesTopK(hits, 5))) / float64(len(relevant))
		r10 := float64(intersectCount(relevant, distinctSourcesTopK(hits, 10))) / float64(len(relevant))
		sum5 += r5
		sum10 += r10
		t.Logf("[recall] %-20q rel=%v @5=%.3f @10=%.3f", query, relevant, r5, r10)
	}
	n := float64(len(queries))
	mean5, mean10 := sum5/n, sum10/n
	t.Logf("[recall] mean recall@5=%.4f recall@10=%.4f over %.0f queries (floors @5=%.4f @10=%.4f)", mean5, mean10, n, minRecallAt5, minRecallAt10)
	if mean5 < minRecallAt5 {
		t.Fatalf("recall@5 회귀: 실측 %.4f < 기준선 %.4f — 코퍼스/라벨/검색 변경이 검색 품질을 떨어뜨렸는지 확인", mean5, minRecallAt5)
	}
	if mean10 < minRecallAt10 {
		t.Fatalf("recall@10 회귀: 실측 %.4f < 기준선 %.4f — 6~10위 구간 퇴행 확인", mean10, minRecallAt10)
	}
}

// TestCorpusShape — 코퍼스 형태 고정 가드(Codex P2): 코퍼스가 조용히 얇아지거나 색인에서
// 누락되는 회귀를 잡는다. (a) 스킵 없이 전부 색인, (b) 색인 소스 집합 == golden corpus_files,
// (c) 총 청크 수·다중청크(>=3) 파일 수 하한(최초 실측 기반). 파일당 >=3청크 편차는 의도 —
// 팽창 금지 우선(testdata/oracle/README.md 참조).
func TestCorpusShape(t *testing.T) {
	var golden struct {
		CorpusFiles []string `json:"corpus_files"`
	}
	loadJSON(t, testdataPath(t, "oracle", "golden.json"), &golden)
	if len(golden.CorpusFiles) < 12 {
		t.Fatalf("golden.corpus_files=%d < 12 — 스크립트가 corpus 목록을 기록하는지 확인", len(golden.CorpusFiles))
	}

	st, _, rep := indexCorpus(t)
	// (a) 스킵 없이 전부 색인.
	if len(rep.Skipped) != 0 {
		t.Fatalf("색인 스킵 발생(코퍼스는 전부 색인돼야 함): %+v", rep.Skipped)
	}
	if rep.Indexed != len(golden.CorpusFiles) {
		t.Fatalf("색인 %d개 != golden corpus_files %d개", rep.Indexed, len(golden.CorpusFiles))
	}

	counts := chunkCountsBySource(t, st)
	// (b) 색인된 소스 파일 집합 == golden corpus_files.
	if len(counts) != len(golden.CorpusFiles) {
		t.Fatalf("색인 소스 %d개 != golden corpus_files %d개 (counts=%v)", len(counts), len(golden.CorpusFiles), counts)
	}
	for _, f := range golden.CorpusFiles {
		if _, ok := counts[f]; !ok {
			t.Fatalf("golden corpus 파일 %q가 색인 소스 집합에 없음", f)
		}
	}

	// (c) 총 청크 수 >= 50, 다중청크(>=3) 파일 >= 7 (최초 실측 하한 — 코퍼스 축소 방지).
	total, multi := 0, 0
	for _, n := range counts {
		total += n
		if n >= 3 {
			multi++
		}
	}
	t.Logf("[corpus] files=%d total_chunks=%d multi(>=3)=%d", len(counts), total, multi)
	if total < 50 {
		t.Fatalf("총 청크 %d < 50 — 코퍼스가 얇아짐(회귀)", total)
	}
	if multi < 7 {
		t.Fatalf("다중청크(>=3) 파일 %d < 7 — 코퍼스가 얇아짐(회귀)", multi)
	}
}
