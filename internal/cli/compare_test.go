package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"   // wtDirCanon이 사용(계획 검수 Critical 교정)
	"github.com/wotjr1649/context-router/internal/session" // D51 합성 세션 등록(source="first-event") — 기존 seedCC/CX 헬퍼가 source="startup" 고정이라 인라인 복제
)

// runCompareString — runUsageCompare를 고정 기준 시각으로 직접 호출(§6 — 결정론 검증은 주입 시계).
func runCompareString(t *testing.T, storeRoot, projectRoot, tdir, rolloutRoot string, minRecords int64, now time.Time) string {
	t.Helper()
	var out bytes.Buffer
	if err := runUsageCompare(context.Background(), &out, storeRoot, projectRoot, tdir, rolloutRoot, minRecords, now); err != nil {
		t.Fatalf("runUsageCompare: %v", err)
	}
	return out.String()
}

func compareFixture(t *testing.T) (storeRoot, projectRoot, tdir, rolloutRoot string, now time.Time) {
	t.Helper()
	storeRoot, projectRoot, tdir = t.TempDir(), t.TempDir(), t.TempDir()
	rolloutRoot = t.TempDir()
	now = time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local)
	// cc: on 1세션(records 2) + off 1세션(records 1).
	ccOn := "aaaaaaaa-0000-0000-0000-0000000000a1"
	ccOff := "cccccccc-0000-0000-0000-0000000000c3"
	seedCCSession(t, storeRoot, projectRoot, ccOn)
	writeUsageTranscript(t, tdir, ccOn, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":6,"cache_creation_input_tokens":1}}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":0}}}`,
	})
	writeUsageTranscript(t, tdir, ccOff, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":9}}}`,
	})
	// cx: on 1(rollout 매칭) + 등록만 1(n/a K) + off 1(창 내 미등록).
	day := filepath.Join(rolloutRoot, "2026", "07", "22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	cxOn := "019f0000-0000-7000-8000-00000000000a"
	cxOff := "019f0000-0000-7000-8000-00000000000b"
	canonSeed(t, storeRoot, projectRoot, cxOn)
	canonSeed(t, storeRoot, projectRoot, "019f0000-0000-7000-8000-0000000000ff") // rollout 부재 → K
	writeRollout(t, day, tsFor(now, 24*time.Hour), cxOn, []string{metaLine(cxOn, projectRoot), tcLine(100, 50, 10, 110), tcLine(200, 100, 20, 220)})
	writeRollout(t, day, tsFor(now, 48*time.Hour), cxOff, []string{metaLine(cxOff, projectRoot), tcLine(40, 20, 5, 45)})
	return
}

// canonSeed — 현재 worktree session.db에 cx: 세션 등록(seedCXSessionAt 래퍼).
func canonSeed(t *testing.T, storeRoot, projectRoot, uuid string) {
	t.Helper()
	seedCXSessionAt(t, wtDirCanon(t, storeRoot, projectRoot), uuid)
}

func wtDirCanon(t *testing.T, storeRoot, projectRoot string) string {
	t.Helper()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	return filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
}

func TestCompare_FullReport(t *testing.T) {
	storeRoot, projectRoot, tdir, rolloutRoot, now := compareFixture(t)
	got := runCompareString(t, storeRoot, projectRoot, tdir, rolloutRoot, 0, now)
	want := strings.Join([]string{
		"== cc (단위=record) ==",
		"arm\tsessions\trecords\toutput/rec\tcache_read/rec",
		"hooks:on\t1\t2\t15\t5",
		"hooks:off\t1\t1\t3\t2",
		"ratio(on/off)\t-\t-\t5.000\t2.500",
		"excluded(--min-records): 0 | synthetic=0",
		"== cx (단위=turn, experimental) ==",
		"arm\tsessions\tturns\tinput/turn\tcached_input/turn\toutput/turn",
		"hooks:on\t1\t2\t100\t50\t10",
		"hooks:off\t1\t1\t40\t20\t5",
		"ratio(on/off, 대화형(경량) 코호트 한정)\t-\t-\t2.500\t2.500\t2.000",
		"coverage: on 등록=2 매칭=1 n/a=1 | off 창내미등록=1 | unknown=0 | excluded(--min-records)=0 | synthetic=0",
		"skipped: files(미귀속)=0 lines=0 cwd_diverged=0",
		"주의: 관찰 데이터(무작위화 아님) — 워크로드 교란 존재",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("리포트 불일치:\n got %q\nwant %q", got, want)
	}
	// 결정론(§6): 동일 입력·동일 기준 시각 → byte 동일.
	if again := runCompareString(t, storeRoot, projectRoot, tdir, rolloutRoot, 0, now); again != got {
		t.Fatal("반복 실행 출력 상이")
	}
}

func TestCompare_MinRecordsAndMissingRoot(t *testing.T) {
	storeRoot, projectRoot, tdir, rolloutRoot, now := compareFixture(t)
	// --min-records 2: cc off(1rec)·cx 양쪽(2·1turns 중 1turn 세션) 제외 집계.
	got := runCompareString(t, storeRoot, projectRoot, tdir, rolloutRoot, 2, now)
	if !strings.Contains(got, "excluded(--min-records): 1") {
		t.Fatalf("cc 제외 미표기:\n%s", got)
	}
	if !strings.Contains(got, "| excluded(--min-records)=1") {
		t.Fatalf("cx 제외 미표기:\n%s", got)
	}
	if !strings.Contains(got, "ratio(on/off)\t-\t-\tn/a\tn/a") {
		t.Fatalf("빈 off arm 비율이 n/a가 아님:\n%s", got)
	}
	// rollout 루트 부재 → cx 절 대체 + usage 성공(§2·§3).
	got2 := runCompareString(t, storeRoot, projectRoot, tdir, filepath.Join(rolloutRoot, "no-such"), 0, now)
	if !strings.Contains(got2, "rollout 루트 없음") || !strings.Contains(got2, "주의: 관찰 데이터") {
		t.Fatalf("루트 부재 렌더:\n%s", got2)
	}
}

func TestCompare_SkipWarningRendered(t *testing.T) {
	// §6 "스킵>0 경고 병기 단정": 프로젝트 귀속 파일의 손상 라인 → 비율 행에 경고 병기(§3).
	storeRoot, projectRoot, tdir, rolloutRoot, now := compareFixture(t)
	day := filepath.Join(rolloutRoot, "2026", "07", "22")
	uBad := "019f0000-0000-7000-8000-0000000000aa"
	writeRollout(t, day, tsFor(now, 24*time.Hour), uBad, []string{metaLine(uBad, projectRoot), `{broken`, tcLine(1, 0, 0, 1)})
	got := runCompareString(t, storeRoot, projectRoot, tdir, rolloutRoot, 0, now)
	if !strings.Contains(got, " [경고: 파싱 스킵>0]") || !strings.Contains(got, "lines=1") {
		t.Fatalf("스킵 경고 미병기:\n%s", got)
	}
}

func TestCompare_CLIWiring(t *testing.T) {
	storeRoot, projectRoot, tdir, rolloutRoot, _ := compareFixture(t)
	// CLI 경유(--compare) — 시계는 실시간이나 픽스처가 경계에서 멀어 분류 불변.
	out := runUsageString(t, storeRoot, projectRoot, tdir, "--compare", "--rollouts", rolloutRoot)
	if !strings.Contains(out, "== cc (단위=record) ==") || !strings.Contains(out, "experimental") {
		t.Fatalf("CLI 배선:\n%s", out)
	}
	// --totals와 상호 배타(§3).
	var buf, errOut bytes.Buffer
	err := Run(context.Background(), "usage", []string{"--transcripts", tdir, "--compare", "--totals"}, storeRoot, projectRoot, "0.0.1-dev", &buf, &errOut)
	if err == nil || !strings.Contains(err.Error(), "--compare") {
		t.Fatalf("배타 검사 미동작: %v", err)
	}
	// --min-records·--rollouts는 --compare 전용.
	err = Run(context.Background(), "usage", []string{"--transcripts", tdir, "--min-records", "2"}, storeRoot, projectRoot, "0.0.1-dev", &buf, &errOut)
	if err == nil {
		t.Fatal("--compare 없는 --min-records가 통과")
	}
	// 무플래그·--totals 출력 byte-for-byte 불변(§6 회귀) — 기존 TestUsage_TotalsFlag가 담당하나
	// 신규 플래그 추가 후에도 무플래그 경로가 그대로임을 이중 확인.
	plain1 := runUsageString(t, storeRoot, projectRoot, tdir)
	plain2 := runUsageString(t, storeRoot, projectRoot, tdir)
	if plain1 != plain2 || strings.Contains(plain1, "== cc") {
		t.Fatalf("무플래그 경로 오염:\n%s", plain1)
	}
}

// TestCompareSyntheticExcluded — D51(v0.9 §0 A/B 측정 무결성): source="first-event" 합성 등록
// cc: 세션은 --compare 집계에서 기본 제외되고 cc excluded 라인에 synthetic=N으로 보고된다.
func TestCompareSyntheticExcluded(t *testing.T) {
	storeRoot, projectRoot, tdir, rolloutRoot := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	normal := "aaaaaaaa-0000-0000-0000-0000000000a1" // source="startup"(정상)
	synth := "bbbbbbbb-0000-0000-0000-0000000000b2"  // source="first-event"(합성)
	seedCCSession(t, storeRoot, projectRoot, normal)
	// 합성 등록 세션 — seedCCSession 본문을 복제하되 source만 "first-event"로 교체(브리프 Step 1;
	// 기존 seedCCSession이 source="startup" 고정이라 새 헬퍼 대신 인라인 복제).
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	ad, err := session.OpenAppend(context.Background(), sessDir, session.AppendOptions{ExternalSessionID: "cc:" + synth, Producer: "test", RetentionSec: 0})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	if _, err := ad.EnsureSession(context.Background(), "first-event", ""); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := ad.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writeUsageTranscript(t, tdir, normal, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":6,"cache_creation_input_tokens":1}}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":0}}}`,
	})
	writeUsageTranscript(t, tdir, synth, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":9,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}}`,
	})
	// --rollouts는 빈 임시 루트로 고정(기본값 ~/.codex/sessions 실데이터 순회 차단 — 결정론).
	out := runUsageString(t, storeRoot, projectRoot, tdir, "--compare", "--rollouts", rolloutRoot)
	if !strings.Contains(out, "hooks:on\t1\t") { // 정상 세션만 on(합성 제외로 1세션)
		t.Fatalf("정상 세션 on=1 미계상(합성 제외 기대):\n%s", out)
	}
	if !strings.Contains(out, "hooks:off\t0\t") { // 합성이 off로 새어들지 않음
		t.Fatalf("합성 세션이 off로 계상됨(off=0 기대):\n%s", out)
	}
	if !strings.Contains(out, "excluded(--min-records): 0 | synthetic=1") {
		t.Fatalf("cc synthetic=1 미보고:\n%s", out)
	}
}

// TestCompareSyntheticExcludedCX — D51: source="first-event" 합성 등록 cx: 세션은 rollout이
// 있어도 on/off/unknown 어디에도 계상되지 않고(cxSet에서도 제외) coverage에 synthetic=1로 보고된다.
func TestCompareSyntheticExcludedCX(t *testing.T) {
	storeRoot, projectRoot, tdir, rolloutRoot := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local)
	day := filepath.Join(rolloutRoot, "2026", "07", "22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	synth := "019f0000-0000-7000-8000-00000000000a"
	// 합성 등록 cx: 세션 — seedCXSessionAt 본문 복제, source만 "first-event"로 교체(D51).
	sessDir := wtDirCanon(t, storeRoot, projectRoot)
	ad, err := session.OpenAppend(context.Background(), sessDir, session.AppendOptions{ExternalSessionID: "cx:" + synth, Producer: "test", RetentionSec: 0})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	if _, err := ad.EnsureSession(context.Background(), "first-event", ""); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := ad.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 그 UUID의 rollout(창 이내) — 등록됐지만 합성이라 어디에도 미계상.
	writeRollout(t, day, tsFor(now, 24*time.Hour), synth, []string{metaLine(synth, projectRoot), tcLine(100, 50, 10, 110)})
	got := runCompareString(t, storeRoot, projectRoot, tdir, rolloutRoot, 0, now)
	// on/off/unknown·registered 모두 0(합성은 cxSet에서 제외) + synthetic=1.
	want := "coverage: on 등록=0 매칭=0 n/a=0 | off 창내미등록=0 | unknown=0 | excluded(--min-records)=0 | synthetic=1"
	if !strings.Contains(got, want) {
		t.Fatalf("cx 합성 제외/synthetic=1 보고 실패:\n%s", got)
	}
}
