package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
)

// metaLine — rollout session_meta 1줄(실측 스키마 §7: payload.session_id·id 병존, cwd 원형 경로).
func metaLine(id, cwd string) string {
	return `{"type":"session_meta","payload":{"session_id":"` + id + `","id":"` + id + `","cwd":` + strconv.Quote(cwd) + `}}`
}

// tcLine — token_count 1줄(누적 벡터 — input⊇cached_input, 실측 §7).
func tcLine(input, cached, output, total int64) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}}}}`, input, cached, output, total)
}

func writeRollout(t *testing.T, dir, ts, uuid string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, "rollout-"+ts+"-"+uuid+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("writeRollout: %v", err)
	}
	return p
}

const testUUID = "019f0000-0000-7000-8000-000000000001"

func TestRolloutStartFromName(t *testing.T) {
	start, id, ok := rolloutStartFromName("rollout-2026-07-21T20-51-22-" + testUUID + ".jsonl")
	if !ok || id != testUUID {
		t.Fatalf("ok=%v id=%q", ok, id)
	}
	want := time.Date(2026, 7, 21, 20, 51, 22, 0, time.Local) // 로컬 시간(스펙 §2 — KST 오프셋 실측 §7)
	if !start.Equal(want) {
		t.Fatalf("start=%v want=%v", start, want)
	}
	if _, _, ok := rolloutStartFromName("rollout-bad.jsonl"); ok {
		t.Fatal("불일치 파일명이 ok")
	}
	if _, _, ok := rolloutStartFromName("other-2026-07-21T20-51-22-" + testUUID + ".jsonl"); ok {
		t.Fatal("접두 불일치가 ok")
	}
}

func TestParseRolloutFile_MaxWins(t *testing.T) {
	root := t.TempDir()
	folded := ident.Fold(root)
	// 단조 지속(재개 meta 경계 넘어 누적 — 98건 전수 실측 §7): max=last.
	p := writeRollout(t, t.TempDir(), "2026-07-21T20-51-22", testUUID, []string{
		metaLine(testUUID, root),
		tcLine(100, 40, 10, 110),
		tcLine(200, 80, 20, 220),
		metaLine(testUUID, root), // 재개 meta — 구간 분리하지 않는다(max-wins)
		tcLine(250, 100, 30, 280),
	})
	r, err := parseRolloutFile(context.Background(), p, folded)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if r.id != testUUID || r.turns != 3 || r.skipped != 0 {
		t.Fatalf("id=%q turns=%d skipped=%d", r.id, r.turns, r.skipped)
	}
	if r.use != (cxUsage{input: 250, cachedInput: 100, output: 30, total: 280}) {
		t.Fatalf("use=%+v", r.use)
	}
	if !r.cwdAny || r.cwdOut {
		t.Fatalf("cwdAny=%v cwdOut=%v", r.cwdAny, r.cwdOut)
	}
}

func TestParseRolloutFile_DecreaseSnapshotIgnored(t *testing.T) {
	// 감소 스냅샷 혼입(가상의 재전송·순서 역전 — 실측 0건이나 계약이 흡수, 스펙 §2):
	// max-wins가 무시하고 벡터 단위로 최대 total 스냅샷을 유지한다(§6).
	root := t.TempDir()
	p := writeRollout(t, t.TempDir(), "2026-07-21T20-51-22", testUUID, []string{
		metaLine(testUUID, root),
		tcLine(200, 80, 20, 220),
		tcLine(100, 40, 10, 110), // 지연 재전송 — 새 구간을 열지 않고 무시
	})
	r, err := parseRolloutFile(context.Background(), p, ident.Fold(root))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if r.use.total != 220 || r.use.input != 200 || r.turns != 2 {
		t.Fatalf("use=%+v turns=%d", r.use, r.turns)
	}
}

func TestParseRolloutFile_VectorUnitSnapshot(t *testing.T) {
	// §6 "필드 일부 상이 스냅샷(벡터 단위 채택 단정)": total은 하위지만 특정 필드가 상위인
	// 스냅샷 — 필드별 max 구현이면 cachedInput=120이 나와 실패한다(벡터 단위 계약 판별).
	root := t.TempDir()
	p := writeRollout(t, t.TempDir(), "2026-07-21T20-51-22", testUUID, []string{
		metaLine(testUUID, root),
		tcLine(200, 80, 20, 220),
		tcLine(150, 120, 10, 180),
	})
	r, err := parseRolloutFile(context.Background(), p, ident.Fold(root))
	if err != nil || r.use != (cxUsage{input: 200, cachedInput: 80, output: 20, total: 220}) {
		t.Fatalf("err=%v use=%+v", err, r.use)
	}
}

func TestParseRolloutFile_TieKeepsFirst(t *testing.T) {
	// max-wins 동률 규칙(§2 "동률은 첫 스냅샷 유지" — strict `>`): total 동률·벡터 상이면
	// 나중 스냅샷을 채택하지 않는다(`>=`로 바뀌면 input=190이 나와 실패하는 판별 테스트).
	root := t.TempDir()
	p := writeRollout(t, t.TempDir(), "2026-07-21T20-51-22", testUUID, []string{
		metaLine(testUUID, root),
		tcLine(200, 80, 20, 220),
		tcLine(190, 90, 25, 220), // 동률 total — 채택 금지
	})
	r, err := parseRolloutFile(context.Background(), p, ident.Fold(root))
	if err != nil || r.use != (cxUsage{input: 200, cachedInput: 80, output: 20, total: 220}) {
		t.Fatalf("동률 첫 스냅샷 미유지: err=%v use=%+v", err, r.use)
	}
}

func TestParseRolloutFile_CwdDivergence(t *testing.T) {
	// meta 간 cwd 발산(스펙 §2 — 파일 제외+집계 대상 신호): cwdAny·cwdOut 동시 true.
	root := t.TempDir()
	other := t.TempDir()
	p := writeRollout(t, t.TempDir(), "2026-07-21T20-51-22", testUUID, []string{
		metaLine(testUUID, root),
		tcLine(10, 5, 1, 11),
		metaLine(testUUID, other),
	})
	r, err := parseRolloutFile(context.Background(), p, ident.Fold(root))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !r.cwdAny || !r.cwdOut {
		t.Fatalf("발산 미검출: cwdAny=%v cwdOut=%v", r.cwdAny, r.cwdOut)
	}
	// 하위 디렉터리는 루트 하위로 채택(루트+"/" 접두 — 스펙 §2).
	sub := filepath.Join(root, "internal")
	p2 := writeRollout(t, t.TempDir(), "2026-07-21T20-51-23", testUUID, []string{
		metaLine(testUUID, sub), tcLine(1, 0, 0, 1),
	})
	r2, err := parseRolloutFile(context.Background(), p2, ident.Fold(root))
	if err != nil || !r2.cwdAny || r2.cwdOut {
		t.Fatalf("하위 cwd 미채택: err=%v %+v", err, r2)
	}
	// §6 cwd 정규화 경계 — 대소문자: Fold는 windows/darwin만 케이스폴드(unix는 `\`·대소문자가
	// 합법 파일명이라 미적용, internal/ident/ident.go:35-37). unix에선 대문자 root가 발산하므로
	// 이 채택 단정은 케이스폴드 플랫폼에서만 유효하다.
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		upper := strings.ToUpper(root)
		p3 := writeRollout(t, t.TempDir(), "2026-07-21T20-51-24", testUUID, []string{
			metaLine(testUUID, upper), tcLine(1, 0, 0, 1),
		})
		r3, err := parseRolloutFile(context.Background(), p3, ident.Fold(root))
		if err != nil || !r3.cwdAny || r3.cwdOut {
			t.Fatalf("대소문자 정규화 실패: err=%v %+v", err, r3)
		}
	}
	// trailing separator는 플랫폼 무관 — 그대로 채택.
	p4 := writeRollout(t, t.TempDir(), "2026-07-21T20-51-25", testUUID, []string{
		metaLine(testUUID, root+string(os.PathSeparator)), tcLine(1, 0, 0, 1),
	})
	r4, err := parseRolloutFile(context.Background(), p4, ident.Fold(root))
	if err != nil || !r4.cwdAny || r4.cwdOut {
		t.Fatalf("trailing separator 실패: err=%v %+v", err, r4)
	}
}

func TestParseRolloutFile_CorruptAndMissing(t *testing.T) {
	root := t.TempDir()
	folded := ident.Fold(root)
	// 손상 줄 — skip 집계, 파일은 성공(스펙 §2 오류 규율).
	p := writeRollout(t, t.TempDir(), "2026-07-21T20-51-22", testUUID, []string{
		metaLine(testUUID, root), `{broken json`, tcLine(10, 5, 1, 11),
	})
	r, err := parseRolloutFile(context.Background(), p, folded)
	if err != nil || r.skipped != 1 || r.use.total != 11 {
		t.Fatalf("err=%v skipped=%d total=%d", err, r.skipped, r.use.total)
	}
	// meta 없음 → 파일 단위 오류(스캔 층에서 스킵 집계).
	p2 := writeRollout(t, t.TempDir(), "2026-07-21T20-51-23", testUUID, []string{tcLine(1, 0, 0, 1)})
	if _, err := parseRolloutFile(context.Background(), p2, folded); err == nil {
		t.Fatal("meta 없는 파일이 성공")
	}
	// token_count 없음 → turns=0·use 제로가 정상(성공).
	p3 := writeRollout(t, t.TempDir(), "2026-07-21T20-51-24", testUUID, []string{metaLine(testUUID, root)})
	r3, err := parseRolloutFile(context.Background(), p3, folded)
	if err != nil || r3.turns != 0 || r3.use.total != 0 {
		t.Fatalf("err=%v %+v", err, r3)
	}
	// token_count 필수 필드 부재(형식 변경 — 스펙 §2 스킵 강등): turn 미계상 + skipped 집계.
	p4 := writeRollout(t, t.TempDir(), "2026-07-21T20-51-26", testUUID, []string{
		metaLine(testUUID, root),
		`{"type":"event_msg","payload":{"type":"token_count","info":{}}}`,
		tcLine(10, 5, 1, 11),
	})
	r4, err := parseRolloutFile(context.Background(), p4, folded)
	if err != nil || r4.skipped != 1 || r4.turns != 1 || r4.use.total != 11 {
		t.Fatalf("필드 부재 스킵 강등 실패: err=%v %+v", err, r4)
	}
	// total_token_usage는 있으나 개별 리프(cached_input_tokens)만 부재(부분 커버 — §2 스킵 강등):
	// 벡터 불완전 = 형식 변경 → skipped++·turn 미계상.
	p5 := writeRollout(t, t.TempDir(), "2026-07-21T20-51-27", testUUID, []string{
		metaLine(testUUID, root),
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}}`,
		tcLine(10, 5, 1, 11),
	})
	r5, err := parseRolloutFile(context.Background(), p5, folded)
	if err != nil || r5.skipped != 1 || r5.turns != 1 || r5.use.total != 11 {
		t.Fatalf("리프 부재 스킵 강등 실패: err=%v %+v", err, r5)
	}
}
