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
	"github.com/wotjr1649/context-router/internal/session"
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

// seedCXSessionAt — 지정 worktree 디렉터리에 cx: 세션을 등록한다(seedCCSession 패턴 —
// 다중 worktree 순회 검증용으로 sessDir를 직접 받는다).
func seedCXSessionAt(t *testing.T, sessDir, uuid string) {
	t.Helper()
	ad, err := session.OpenAppend(context.Background(), sessDir, session.AppendOptions{ExternalSessionID: "cx:" + uuid, Producer: "test", RetentionSec: 0})
	if err != nil {
		t.Fatalf("OpenAppend %s: %v", uuid, err)
	}
	if _, err := ad.EnsureSession(context.Background(), "startup", ""); err != nil {
		t.Fatalf("EnsureSession %s: %v", uuid, err)
	}
	if err := ad.Close(); err != nil {
		t.Fatalf("Close %s: %v", uuid, err)
	}
}

func wtDir(t *testing.T, storeRoot, projectRoot, name string) string {
	t.Helper()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	return filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", name)
}

func TestLoadCXSessions_MergesWorktrees(t *testing.T) {
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	canon, _ := ident.Canonicalize(projectRoot)
	// 현재 worktree + 별도 worktree 양쪽에 등록(D40 ③ — 단일 worktree 조인은 체계적 과소귀속).
	u1 := "019f0000-0000-7000-8000-0000000000a1"
	u2 := "019f0000-0000-7000-8000-0000000000b2"
	seedCXSessionAt(t, filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID), u1)
	seedCXSessionAt(t, wtDir(t, storeRoot, projectRoot, "otherwt"), u2)
	set, incomplete := loadCXSessions(context.Background(), storeRoot, projectRoot)
	if incomplete {
		t.Fatal("정상 순회가 incomplete")
	}
	if !set["cx:"+u1] || !set["cx:"+u2] || len(set) != 2 {
		t.Fatalf("병합 실패: %v", set)
	}
}

func TestLoadCXSessions_UnusableDBIsIncomplete(t *testing.T) {
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	u1 := "019f0000-0000-7000-8000-0000000000a1"
	canon, _ := ident.Canonicalize(projectRoot)
	seedCXSessionAt(t, filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID), u1)
	// 손상 DB worktree — 그 worktree만 skip + incomplete(스펙 §2: off 확정 금지의 근거 신호).
	bad := wtDir(t, storeRoot, projectRoot, "badwt")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "session.db"), []byte("not a sqlite db"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, incomplete := loadCXSessions(context.Background(), storeRoot, projectRoot)
	if !incomplete {
		t.Fatal("손상 DB가 incomplete=false")
	}
	if !set["cx:"+u1] {
		t.Fatal("정상 worktree 결과가 유실")
	}
	// 진짜 부재(빈 스토어)는 complete — 등록이 없었던 것이지 관측 불능이 아니다(계획 검수 교정:
	// complete=부재일 때만, ReadDir/Stat의 그 외 오류·Canonicalize 실패는 incomplete).
	set2, inc2 := loadCXSessions(context.Background(), t.TempDir(), projectRoot)
	if inc2 || len(set2) != 0 {
		t.Fatalf("빈 스토어: incomplete=%v set=%v", inc2, set2)
	}
}

// tsFor — now 기준 d 전 시각을 rollout 파일명 ts 형식(로컬)으로.
func tsFor(now time.Time, d time.Duration) string {
	return now.Add(-d).Format("2006-01-02T15-04-05")
}

func TestScanRollouts_ArmClassification(t *testing.T) {
	projectRoot := t.TempDir()
	rolloutRoot := t.TempDir()
	day := filepath.Join(rolloutRoot, "2026", "07", "22") // YYYY/MM/DD 재귀 순회(§2)
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local) // 기준 시각 주입(§6 — 결정론)
	uOn := "019f0000-0000-7000-8000-00000000000a"
	uOff := "019f0000-0000-7000-8000-00000000000b"
	uOld := "019f0000-0000-7000-8000-00000000000c"
	uDiv := "019f0000-0000-7000-8000-00000000000d"
	uOut := "019f0000-0000-7000-8000-00000000000e"
	writeRollout(t, day, tsFor(now, 24*time.Hour), uOn, []string{metaLine(uOn, projectRoot), tcLine(100, 50, 10, 110)})
	writeRollout(t, day, tsFor(now, 48*time.Hour), uOff, []string{metaLine(uOff, projectRoot), tcLine(40, 20, 5, 45)})
	// 창 밖(8일 전) 미등록 → unknown(§2 — 행 삭제 불변식은 7일까지만 증명).
	writeRollout(t, day, tsFor(now, 8*24*time.Hour), uOld, []string{metaLine(uOld, projectRoot), tcLine(7, 3, 1, 8)})
	// cwd 발산 → 제외+집계.
	writeRollout(t, day, tsFor(now, 24*time.Hour), uDiv, []string{metaLine(uDiv, projectRoot), tcLine(1, 0, 0, 1), metaLine(uDiv, t.TempDir())})
	// 타 프로젝트 cwd → 자연 배제(집계 없음).
	writeRollout(t, day, tsFor(now, 24*time.Hour), uOut, []string{metaLine(uOut, t.TempDir()), tcLine(1, 0, 0, 1)})
	// 손상 파일(meta 없음) → 파일 단위 스킵 집계.
	writeRollout(t, day, tsFor(now, 24*time.Hour), "019f0000-0000-7000-8000-00000000000f", []string{tcLine(1, 0, 0, 1)})

	cxSet := map[string]bool{"cx:" + uOn: true, "cx:019f0000-0000-7000-8000-0000000000ff": true} // 후자=rollout 부재(n/a K)
	sc, err := scanRollouts(context.Background(), rolloutRoot, projectRoot, cxSet, false, now)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(sc.on) != 1 || sc.on[0].id != uOn || len(sc.off) != 1 || sc.off[0].id != uOff || len(sc.unknown) != 1 || sc.unknown[0].id != uOld {
		t.Fatalf("arm 분류: on=%d off=%d unknown=%d", len(sc.on), len(sc.off), len(sc.unknown))
	}
	if sc.diverged != 1 || sc.skipFiles != 1 {
		t.Fatalf("diverged=%d skipFiles=%d", sc.diverged, sc.skipFiles)
	}
	if sc.registered != 2 || sc.matched != 1 { // K = 2-1 = 1
		t.Fatalf("registered=%d matched=%d", sc.registered, sc.matched)
	}
	if !sc.on[0].start.Equal(now.Add(-24 * time.Hour)) {
		t.Fatalf("start 파싱: %v", sc.on[0].start)
	}
}

func TestScanRollouts_IncompleteDemotesOffToUnknown(t *testing.T) {
	// DB 불용 시 부재 관측 신뢰 불가 → off 확정 금지, 전부 unknown(§2 — 2패스 수렴).
	projectRoot := t.TempDir()
	rolloutRoot := t.TempDir()
	day := filepath.Join(rolloutRoot, "2026", "07", "22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local)
	u := "019f0000-0000-7000-8000-00000000000b"
	writeRollout(t, day, tsFor(now, 24*time.Hour), u, []string{metaLine(u, projectRoot), tcLine(1, 0, 0, 1)})
	sc, err := scanRollouts(context.Background(), rolloutRoot, projectRoot, nil, true, now)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(sc.off) != 0 || len(sc.unknown) != 1 || !sc.incomplete {
		t.Fatalf("incomplete 강등 실패: off=%d unknown=%d incomplete=%v", len(sc.off), len(sc.unknown), sc.incomplete)
	}
}

func TestScanRollouts_WindowBoundaryInclusive(t *testing.T) {
	// 정확히 7일 전 시작: GC 술어는 started_at < now-7d(엄격 미만)라 행 존재가 보장된다 —
	// off 확정은 inclusive(!start.Before(cutoff), 계획 검수 교정 — 스펙 §2 "7일 이내").
	projectRoot := t.TempDir()
	rolloutRoot := t.TempDir()
	day := filepath.Join(rolloutRoot, "2026", "07", "15")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local)
	u := "019f0000-0000-7000-8000-00000000000b"
	writeRollout(t, day, tsFor(now, cxOffWindow), u, []string{metaLine(u, projectRoot), tcLine(1, 0, 0, 1)})
	sc, err := scanRollouts(context.Background(), rolloutRoot, projectRoot, nil, false, now)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(sc.off) != 1 || len(sc.unknown) != 0 {
		t.Fatalf("경계 세션이 off가 아님: off=%d unknown=%d", len(sc.off), len(sc.unknown))
	}
}

func TestScanRollouts_MetaIDAuthority(t *testing.T) {
	// 파일명-meta id 불일치 — meta의 session_id가 권위(§2: 파일명은 발견 수단), arm 조인도 meta 기준.
	projectRoot := t.TempDir()
	rolloutRoot := t.TempDir()
	day := filepath.Join(rolloutRoot, "2026", "07", "22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local)
	fileUUID := "019f0000-0000-7000-8000-0000000000f1"
	metaUUID := "019f0000-0000-7000-8000-0000000000f2"
	writeRollout(t, day, tsFor(now, 24*time.Hour), fileUUID, []string{metaLine(metaUUID, projectRoot), tcLine(1, 0, 0, 1)})
	sc, err := scanRollouts(context.Background(), rolloutRoot, projectRoot, map[string]bool{"cx:" + metaUUID: true}, false, now)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(sc.on) != 1 || sc.on[0].id != metaUUID || sc.matched != 1 {
		t.Fatalf("meta 권위 위반: on=%d matched=%d", len(sc.on), sc.matched)
	}
}

func TestScanRollouts_SessionFileGroupMerge(t *testing.T) {
	// 재개·포크는 같은 meta session_id로 새 rollout 파일을 만든다(D44 c65da3d — 파일=세션 폐기).
	// 두 파일이 한 세션으로 병합: turns 합·use 필드별 합·start=최초 ts·matched 세션당 1회.
	projectRoot := t.TempDir()
	rolloutRoot := t.TempDir()
	day := filepath.Join(rolloutRoot, "2026", "07", "22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local)
	sess := "019f0000-0000-7000-8000-00000000aa01"  // 공유 meta session_id
	fileA := "019f0000-0000-7000-8000-0000000000a1" // 파일명 UUID는 상이
	fileB := "019f0000-0000-7000-8000-0000000000a2"
	// 파일 B가 더 이르다(start=min 검증): A=24h 전, B=48h 전 — 둘 다 창 이내.
	writeRollout(t, day, tsFor(now, 24*time.Hour), fileA, []string{metaLine(sess, projectRoot), tcLine(100, 40, 10, 110)})
	writeRollout(t, day, tsFor(now, 48*time.Hour), fileB, []string{metaLine(sess, projectRoot), tcLine(50, 20, 5, 55)})

	// 등록 시: on 세션 1개·turns=2·use 필드별 합·start=이른 쪽·matched=1.
	sc, err := scanRollouts(context.Background(), rolloutRoot, projectRoot, map[string]bool{"cx:" + sess: true}, false, now)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(sc.on) != 1 || sc.on[0].id != sess || sc.matched != 1 {
		t.Fatalf("그룹 병합 실패: on=%d matched=%d", len(sc.on), sc.matched)
	}
	if sc.on[0].turns != 2 || sc.on[0].use != (cxUsage{input: 150, cachedInput: 60, output: 15, total: 165}) {
		t.Fatalf("합산 실패: turns=%d use=%+v", sc.on[0].turns, sc.on[0].use)
	}
	if !sc.on[0].start.Equal(now.Add(-48 * time.Hour)) {
		t.Fatalf("start=min 실패: %v", sc.on[0].start)
	}

	// 등록 없이 창 내 2파일 → off=1(세션 단위 계상 — 파일 2개로 세지 않는다).
	sc2, err := scanRollouts(context.Background(), rolloutRoot, projectRoot, nil, false, now)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(sc2.off) != 1 || len(sc2.on) != 0 || len(sc2.unknown) != 0 {
		t.Fatalf("off 세션 계상 실패: off=%d on=%d unknown=%d", len(sc2.off), len(sc2.on), len(sc2.unknown))
	}
}
