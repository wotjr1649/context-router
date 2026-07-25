package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/session"
)

// writeUsageTranscript — testdata용 축약 합성 transcript(민감값 없음, 실파일 복사 금지 —
// 브리프 Step 1). 파일명은 <session-uuid>.jsonl(호스트 세션 = 파일 = UUID, Step 1 실측 확인).
func writeUsageTranscript(t *testing.T, dir, uuid string, lines []string) {
	t.Helper()
	p := filepath.Join(dir, uuid+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript %s: %v", uuid, err)
	}
}

// TestRunUsage_PerSessionSums(시나리오 ①) — transcript 2세션 → 파일별(세션별) usage 합산이
// 정확해야 한다(설계 §6). usage 없는 줄(user)은 합산 대상 밖.
func TestRunUsage_PerSessionSums(t *testing.T) {
	tdir := t.TempDir()
	writeUsageTranscript(t, tdir, "aaaaaaaa-0000-0000-0000-000000000001", []string{
		`{"type":"user","message":{"role":"user"}}`,
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":1}}}`,
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})
	writeUsageTranscript(t, tdir, "bbbbbbbb-0000-0000-0000-000000000002", []string{
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":9}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run usage err=%v out=%s", err, out.String())
	}
	got := out.String()
	// 컬럼: session\tinput\toutput\tcache_read\tcache_creation\trecords\thooks
	for _, want := range []string{
		"aaaaaaaa-0000-0000-0000-000000000001\t150\t30\t5\t1\t2\t",
		"bbbbbbbb-0000-0000-0000-000000000002\t7\t3\t2\t9\t1\t",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("out missing row %q:\n%s", want, got)
		}
	}
}

// TestRunUsage_HooksOnWhenCCSeeded(시나리오 ②) — session.db에 cc:<uuid>가 존재하면(훅 스트림
// 존재 신호) 그 세션 행은 hooks:on으로 표기해야 한다(설계 §6). cc: 세션은 OpenAppend+
// EnsureSession으로 시드한다(hook 런타임과 동일 경로).
func TestRunUsage_HooksOnWhenCCSeeded(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	uuid := "cccccccc-0000-0000-0000-000000000003"

	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	ad, err := session.OpenAppend(context.Background(), sessDir, session.AppendOptions{ExternalSessionID: "cc:" + uuid, Producer: "test", RetentionSec: 0})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	if _, err := ad.EnsureSession(context.Background(), "startup", ""); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := ad.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tdir := t.TempDir()
	writeUsageTranscript(t, tdir, uuid, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run usage err=%v out=%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, uuid) {
		t.Fatalf("row for %s missing:\n%s", uuid, got)
	}
	// uuid 줄이 hooks:on이어야 한다 — 그 줄만 뽑아 확인(다른 줄 오염 방지).
	line := usageRowFor(got, uuid)
	if !strings.Contains(line, "hooks:on") {
		t.Fatalf("want %s marked hooks:on, got line %q", uuid, line)
	}
}

// TestRunUsage_HooksOffWhenNoSession(시나리오 ③) — 그 UUID의 cc: 세션이 session.db에 없으면
// hooks:off여야 한다(설계 §6). session.db 자체가 없는 경우(훅 미사용 프로젝트)도 동일.
func TestRunUsage_HooksOffWhenNoSession(t *testing.T) {
	tdir := t.TempDir()
	uuid := "dddddddd-0000-0000-0000-000000000004"
	writeUsageTranscript(t, tdir, uuid, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run usage err=%v out=%s", err, out.String())
	}
	got := out.String()
	line := usageRowFor(got, uuid)
	if line == "" {
		t.Fatalf("row for %s missing:\n%s", uuid, got)
	}
	if !strings.Contains(line, "hooks:off") {
		t.Fatalf("want %s hooks:off (no session seeded), got line %q", uuid, line)
	}
}

// TestRunUsage_CorruptLineSkipped(시나리오 ④) — 비JSON 손상 줄은 건너뛰고 명령 전체는
// 중단되지 않아야 한다(설계 §6, readTranscriptLine/providerUsageLine 재사용 계약). 앞뒤 정상
// 줄은 계속 합산된다.
func TestRunUsage_CorruptLineSkipped(t *testing.T) {
	tdir := t.TempDir()
	uuid := "eeeeeeee-0000-0000-0000-000000000005"
	writeUsageTranscript(t, tdir, uuid, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":9,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		`not valid json`,
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("corrupt line must not abort: err=%v out=%s", err, out.String())
	}
	got := out.String()
	// 정상 2줄만: input 10, output 1, cache 0/0, records 2 (손상 줄은 제외).
	if want := uuid + "\t10\t1\t0\t0\t2\t"; !strings.Contains(got, want) {
		t.Fatalf("want corrupt-skipped sums %q, got:\n%s", want, got)
	}
}

// TestRunUsage_MissingDirError(시나리오 ⑤) — transcript 디렉터리 부재는 명확한 오류여야 하고,
// 반환 오류 문구에 절대경로가 섞이면 안 된다(§12 canary — os.ReadDir의 *fs.PathError는 경로를
// 담는다).
func TestRunUsage_MissingDirError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-transcripts-dir")
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "usage", []string{"--transcripts", missing}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
	if err == nil {
		t.Fatal("want error for missing transcripts dir, got nil")
	}
	if !strings.Contains(err.Error(), "디렉터리") {
		t.Fatalf("want a clear directory error, got: %v", err)
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("error must not leak the path: %v", err)
	}
}

// TestTranscriptDirFor_NonAlnumToHyphen — Step 1 실측 계약 고정: cwd → transcript 디렉터리명은
// 영숫자가 아닌 모든 문자를 '-'로 치환한다(밑줄·점 포함). plan의 "경로 구분자·콜론만" 규칙이었다면
// AI_DEV의 밑줄이 남아 실제 디렉터리(AI-DEV)와 어긋난다 — 이 테스트가 회귀를 막는다(브리프 Step 1).
func TestTranscriptDirFor_NonAlnumToHyphen(t *testing.T) {
	// 실측 예: C:\Users\js\Documents\AI_DEV\context-router
	//        → C--Users-js-Documents-AI-DEV-context-router
	got := transcriptDirRe.ReplaceAllString(`C:\Users\js\Documents\AI_DEV\context-router`, "-")
	if want := "C--Users-js-Documents-AI-DEV-context-router"; got != want {
		t.Fatalf("transform mismatch: got %q want %q", got, want)
	}
	if strings.Contains(got, "_") { // 밑줄이 '-'로 바뀌었는지(plan 규칙과의 차이) 명시 확인
		t.Fatalf("underscore must be replaced with '-': %q", got)
	}
}

// usageRowFor — out에서 uuid로 시작하는 표 행 하나를 돌려준다(없으면 "").
func usageRowFor(out, uuid string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, uuid) {
			return ln
		}
	}
	return ""
}

// seedCCSession — cc:<uuid> 세션을 session.db에 시드한다(hook 런타임과 동일한 OpenAppend+
// EnsureSession 경로 — usage의 hooks:on 표기 조건). 여러 번 호출하면 여러 cc: 세션이 쌓인다.
func seedCCSession(t *testing.T, storeRoot, projectRoot, uuid string) {
	t.Helper()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	ad, err := session.OpenAppend(context.Background(), sessDir, session.AppendOptions{ExternalSessionID: "cc:" + uuid, Producer: "test", RetentionSec: 0})
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

// runUsageString — usage 서브명령을 tdir·extra 플래그로 실행하고 stdout 전문을 돌려준다.
func runUsageString(t *testing.T, storeRoot, projectRoot, tdir string, extra ...string) string {
	t.Helper()
	args := append([]string{"--transcripts", tdir}, extra...)
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run usage err=%v out=%s", err, out.String())
	}
	return out.String()
}

// totalRowFrom — plain 본표의 세션 행(7열)을 파싱해 label(hooks:on|off) 그룹의 토큰·records
// 합계를 계산하고 runUsage가 찍을 TOTAL 행 문자열을 조립한다(단정을 하드코딩 아닌 실측 합산으로).
func totalRowFrom(plain, label string) string {
	var c [5]int64
	for _, ln := range strings.Split(plain, "\n") {
		f := strings.Split(ln, "\t")
		if len(f) != 7 || f[0] == "session" || f[6] != label {
			continue
		}
		for i := 0; i < 5; i++ {
			v, _ := strconv.ParseInt(f[1+i], 10, 64)
			c[i] += v
		}
	}
	return fmt.Sprintf("TOTAL:%s\t%d\t%d\t%d\t%d\t%d\t%s\n", label, c[0], c[1], c[2], c[3], c[4], label)
}

// TestUsage_TotalsFlag: D33 — --totals가 hooks:on/off 그룹 합계 2행을 덧붙이고, 플래그가 없으면
// 출력이 byte-for-byte 불변이어야 한다(행=세션 1:1 계약 유지, 이중 집계 방지, 설계 §8 게이트).
func TestUsage_TotalsFlag(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()

	// hooks:on 2세션(cc: 시드) + hooks:off 1세션 — on 그룹에 2세션을 둬서 합산이 다중 세션을
	// 가로질러 맞는지 검증한다(단일 세션이면 열 합산 버그를 못 잡는다).
	onUUID1 := "aaaaaaaa-0000-0000-0000-0000000000a1"
	onUUID2 := "bbbbbbbb-0000-0000-0000-0000000000b2"
	offUUID := "cccccccc-0000-0000-0000-0000000000c3"
	seedCCSession(t, storeRoot, projectRoot, onUUID1)
	seedCCSession(t, storeRoot, projectRoot, onUUID2)

	tdir := t.TempDir()
	writeUsageTranscript(t, tdir, onUUID1, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":1}}}`,
	})
	writeUsageTranscript(t, tdir, onUUID2, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":4,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}}`,
	})
	writeUsageTranscript(t, tdir, offUUID, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":9}}}`,
	})

	// os.ReadDir는 파일명 오름차순 정렬을 보장 → aaaa < bbbb < cccc 순.
	wantPlain := strings.Join([]string{
		"session\tinput\toutput\tcache_read\tcache_creation\trecords\thooks",
		onUUID1 + "\t100\t20\t5\t1\t1\thooks:on",
		onUUID2 + "\t50\t4\t3\t0\t1\thooks:on",
		offUUID + "\t7\t3\t2\t9\t1\thooks:off",
		"",
	}, "\n")

	// 기본 출력 불변은 byte-for-byte로 고정한다 — "TOTAL 부재"만 검사하면 헤더·열 순서·행 형식
	// 회귀를 놓친다(설계 §8 게이트).
	if outPlain := runUsageString(t, storeRoot, projectRoot, tdir); outPlain != wantPlain {
		t.Fatalf("무플래그 출력 회귀:\n got %q\nwant %q", outPlain, wantPlain)
	}

	outTotals := runUsageString(t, storeRoot, projectRoot, tdir, "--totals")
	if !strings.HasPrefix(outTotals, wantPlain) {
		t.Fatalf("--totals 본표 prefix가 기본 출력과 다름:\n%s", outTotals)
	}
	// 본표 뒤(suffix)는 정확히 두 TOTAL 행(on 먼저 off 다음)이어야 한다 — "정확히 2행" 계약.
	// Contains만 쓰면 stray 3번째 행·on/off 순서 뒤바뀜을 놓친다. 합계는 세션 행 실측 파싱으로
	// 조립(열 1~5 정확성 포함).
	wantSuffix := totalRowFrom(wantPlain, "hooks:on") + totalRowFrom(wantPlain, "hooks:off")
	if suffix := outTotals[len(wantPlain):]; suffix != wantSuffix {
		t.Fatalf("--totals suffix 불일치:\n got %q\nwant %q", suffix, wantSuffix)
	}
}

// adoptionDirs — seedAdoptionEvents가 돌려주는 storeRoot·projRoot(runUsage 인자).
type adoptionDirs struct{ storeRoot, projRoot string }

// seedAdoptionEvents — tool_call 이벤트를 summary별 지정 횟수만큼 session.db에 주입한다(hook
// 런타임과 동일한 OpenAppend+EnsureSession+Append 경로 — seedCCSession 확장, D62). summary별
// 반복 append로 긴 리터럴 없이 카운트를 만든다.
func seedAdoptionEvents(t *testing.T, counts map[string]int) adoptionDirs {
	t.Helper()
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	ctx := context.Background()
	ad, err := session.OpenAppend(ctx, sessDir, session.AppendOptions{ExternalSessionID: "cc:adopt", Producer: "test", RetentionSec: 0})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	if _, err := ad.EnsureSession(ctx, "startup", ""); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	for summary, n := range counts {
		for i := 0; i < n; i++ {
			if _, _, _, err := ad.Append(ctx, session.Event{Type: "tool_call", Summary: summary}); err != nil {
				t.Fatalf("Append %s: %v", summary, err)
			}
		}
	}
	if err := ad.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return adoptionDirs{storeRoot, projectRoot}
}

// TestUsageAdoptionCounts — D63: summary를 서버 네임스페이스 단위로 버킷팅하고 세션 분모를
// 병기한다. v0.11의 ctr/ctxscribe 2행 + ratio 참고 문면은 없어졌다 — ratio는 브리지 종료 후
// 분모가 붕괴하는 지표였고, tool_call 세션 분모가 그 자리를 대신한다.
func TestUsageAdoptionCounts(t *testing.T) {
	dir := seedAdoptionEvents(t, map[string]int{
		"mcp__ctr__ctr_search": 2, "mcp__plugin_ctxscribe_mcp__ctx_execute": 5,
	})
	var buf bytes.Buffer
	if err := runUsage(context.Background(), &buf, []string{"--adoption"}, dir.storeRoot, dir.projRoot); err != nil {
		t.Fatalf("runUsage: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"server\tcalls\tsessions", "ctr\t2\t1", "plugin_ctxscribe_mcp\t5\t1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("집계 누락(%q):\n%s", want, got)
		}
	}
	if strings.Contains(got, "ratio") {
		t.Fatalf("폐기된 ratio 문면이 남아 있다:\n%s", got)
	}
}

// TestMCPServerOf: summary에서 MCP 서버 네임스페이스를 뽑는다. 서버 이름에 밑줄이 있어도
// 첫 "__" 구분자에서 끊는다(mcp__<server>__<tool>).
func TestMCPServerOf(t *testing.T) {
	cases := []struct {
		in     string
		server string
		ok     bool
	}{
		{"mcp__ctr-exec__ctr_execute", "ctr-exec", true},
		{"mcp__ctr__ctr_search", "ctr", true},
		{"mcp__plugin_ctxscribe_mcp__ctx_execute", "plugin_ctxscribe_mcp", true},
		{"Read", "", false},
		{"Bash: cd", "", false},
		{"mcp__", "", false},
		{"mcp__onlyserver", "", false},
	}
	for _, c := range cases {
		got, ok := mcpServerOf(c.in)
		if got != c.server || ok != c.ok {
			t.Errorf("mcpServerOf(%q)=(%q,%v) want (%q,%v)", c.in, got, ok, c.server, c.ok)
		}
	}
}

// TestUsageAdoptionBucketsByServer: 같은 도구 접두(ctr_)를 공유하는 두 서버가 분리 집계되고,
// 비-MCP 도구가 서버 행에 섞이지 않으며, 세션 분모 라인이 출력된다.
func TestUsageAdoptionBucketsByServer(t *testing.T) {
	dir := seedAdoptionEvents(t, map[string]int{
		"mcp__ctr-exec__ctr_execute":             3,
		"mcp__plugin_ctxscribe_mcp__ctx_execute": 5,
		"Read":                                   2,
	})
	var buf bytes.Buffer
	if err := runUsage(context.Background(), &buf, []string{"--adoption"}, dir.storeRoot, dir.projRoot); err != nil {
		t.Fatalf("runUsage: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"server\tcalls\tsessions",
		"ctr-exec\t3\t1",
		"plugin_ctxscribe_mcp\t5\t1",
		"# tool_call 세션 분모: 1",
		"# 비-MCP 도구 호출: 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("출력에 %q 없음:\n%s", want, out)
		}
	}
	// "ctr\t"로 시작하는 행은 서버 구분 없는 합산의 잔재다("ctr-exec\t"는 탭이 아니라 '-'가
	// 뒤따르므로 걸리지 않는다).
	if strings.Contains(out, "\nctr\t") {
		t.Errorf("서버 구분 없는 합산 행이 남아 있다:\n%s", out)
	}
}

// TestUsageAdoptionDaysWindow — --days 경계는 세 쿼리(호출 수·세션 분모·서버별 세션)에 같은
// 값으로 바인딩된다. 방금 시드한 이벤트는 창 안이므로 무필터와 같은 값이 나와야 한다 — 셋 중
// 하나만 조립이 깨져도(또는 분모·분자가 다른 창을 보면) 여기서 걸린다.
func TestUsageAdoptionDaysWindow(t *testing.T) {
	dir := seedAdoptionEvents(t, map[string]int{"mcp__ctr__ctr_search": 2, "Read": 1})
	var buf bytes.Buffer
	if err := runUsage(context.Background(), &buf, []string{"--adoption", "--days=1"}, dir.storeRoot, dir.projRoot); err != nil {
		t.Fatalf("runUsage: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"ctr\t2\t1", "# tool_call 세션 분모: 1", "# 비-MCP 도구 호출: 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("출력에 %q 없음:\n%s", want, got)
		}
	}
}

// TestUsageAdoptionGuards — 이월 Minor 두 건: ① 음수 --days는 오류(파일시스템·DB 접근 전에
// 걸러 출력이 전혀 나오지 않아야 한다), ② session.db 미초기화는 오류가 아니라 안내다(훅을 한
// 번도 쓰지 않은 프로젝트에서 "저장소를 열 수 없습니다"로 끝나던 오해 소지를 없앤다).
func TestUsageAdoptionGuards(t *testing.T) {
	var buf bytes.Buffer
	err := runUsage(context.Background(), &buf, []string{"--adoption", "--days=-1"}, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "음수") {
		t.Fatalf("음수 --days 오류 누락: err=%v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("음수 --days인데 출력이 있다: %q", buf.String())
	}

	buf.Reset()
	if err := runUsage(context.Background(), &buf, []string{"--adoption"}, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("session.db 미초기화는 오류가 아니어야 한다: %v", err)
	}
	if !strings.Contains(buf.String(), "세션 이벤트가 아직 없습니다") {
		t.Fatalf("미초기화 안내 누락:\n%s", buf.String())
	}
}
