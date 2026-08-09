package hook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
)

// fixtureWith — testdata 골든 픽스처를 읽어 overrides로 필드를 덮어쓴 stdin JSON을 만든다.
// cwd는 실재해야 ident.Canonicalize가 통과하므로 각 테스트가 t.TempDir()로 대체한다(픽스처의
// 하드코딩 경로는 호스트 의존적이라 hermetic하지 않다).
func fixtureWith(t *testing.T, name string, overrides map[string]any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	for k, v := range overrides {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// runHook — Run을 결정론 입력(env 맵·io.Discard stdout)으로 호출한다(cc 기본).
func runHook(t *testing.T, storeRoot string, in []byte, env map[string]string) int {
	t.Helper()
	return runHookHost(t, HostClaude, storeRoot, in, env)
}

// runHookHost — 호스트 경계 주입 변형(D35).
func runHookHost(t *testing.T, host Host, storeRoot string, in []byte, env map[string]string) int {
	t.Helper()
	return Run(context.Background(), bytes.NewReader(in), io.Discard, storeRoot, "test", host, func(k string) string { return env[k] })
}

// runHookCaptureStdout — runHook 동형이되 stdout(=guard deny JSON 또는 빈 문자열)을 캡처해 반환한다.
func runHookCaptureStdout(t *testing.T, storeRoot string, in []byte) string {
	t.Helper()
	var out bytes.Buffer
	if rc := Run(context.Background(), bytes.NewReader(in), &out, storeRoot, "test", HostClaude, func(string) string { return "" }); rc != 0 {
		t.Fatalf("hook rc=%d want 0", rc)
	}
	return out.String()
}

// D35×D51 — 동일 UUID의 cc/cx 격리: cc SessionStart 후 같은 UUID의 cx 이벤트는 (drop이 아니라)
// cx: 네임스페이스 세션으로 자동 등록·기록된다. cc: 세션으로의 오귀속 0이 격리 단정의 본체.
func TestHookHostIsolation(t *testing.T) {
	storeRoot := t.TempDir()
	cwd := evalLong(t, t.TempDir())
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	post := fixtureWith(t, "posttooluse-codex-bash.json", map[string]any{"cwd": cwd})

	if rc := runHook(t, storeRoot, start, env); rc != 0 { // cc 세션 등록
		t.Fatalf("cc SessionStart rc=%d", rc)
	}
	if rc := runHookHost(t, HostCodex, storeRoot, post, env); rc != 0 { // 같은 UUID의 cx 이벤트
		t.Fatalf("cx PostToolUse rc=%d", rc)
	}
	sdir := sessDir(t, storeRoot, cwd)
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	var cxSessions, ccEvents int
	e1 := reader.QueryRow("SELECT count(*) FROM sessions WHERE session_id LIKE 'cx:%'").Scan(&cxSessions)
	e2 := reader.QueryRow("SELECT count(*) FROM session_events e JOIN sessions s ON s.session_id=e.session_id WHERE s.session_id LIKE 'cc:%' AND e.event_type<>'session_start'").Scan(&ccEvents)
	_ = reader.Close()
	if e1 != nil || e2 != nil {
		t.Fatalf("query: %v %v", e1, e2)
	}
	if cxSessions != 1 {
		t.Fatalf("cx sessions=%d want 1 (D51 합성 등록)", cxSessions)
	}
	if ccEvents != 0 {
		t.Fatalf("cc 비-session_start 이벤트=%d want 0 (cx→cc 오귀속 금지)", ccEvents)
	}
	if got := readDropsOpt(t, sdir); strings.Contains(got, "unknown-session") {
		t.Fatalf("drops=%q — D51 후 unknown-session 미발생", got)
	}
}

// D35 — 미지 host는 오귀속 대신 drop(bad-host, storeRoot 사이드카) + exit 0.
func TestHookBadHostDrops(t *testing.T) {
	storeRoot := t.TempDir()
	cwd := evalLong(t, t.TempDir())
	in := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	if rc := runHookHost(t, Host("zz"), storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0(fail-open)", rc)
	}
	if got := readDrops(t, storeRoot); !strings.Contains(got, "bad-host") {
		t.Fatalf("drops=%q want bad-host", got)
	}
}

// sessDir — Run과 동일한 규칙으로 storeRoot·cwd에서 worktree 세션 디렉터리를 도출한다(main과
// 동형: <storeRoot>/projects/<pid>/worktrees/<wid>).
func sessDir(t *testing.T, storeRoot, cwd string) string {
	t.Helper()
	canon, err := ident.Canonicalize(cwd)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", cwd, err)
	}
	return filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
}

func readDrops(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "session.drops.log"))
	if err != nil {
		t.Fatalf("read drops in %s: %v", dir, err)
	}
	return string(b)
}

// readDropsOpt — drops 파일이 없으면 ""(부재=드롭 0건)로 관대하게 읽는다. D51 후 unknown-session
// 드롭이 소멸해 드롭 파일 자체가 안 생기는 경로의 "특정 사유 미발생" 단정용(readDrops는 파일
// 필수라 부재 시 Fatal — 부재가 곧 통과인 케이스에 부적합).
func readDropsOpt(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "session.drops.log"))
	if err != nil {
		return ""
	}
	return string(b)
}

// TestAppendDropSanitizesFields — appendDrop이 기록하는 각 필드(reason·sid·event·tool)의 탭·개행·
// CR을 공백으로 무해화하고 64자 상한을 적용해, 사이드카 5필드 TSV 파서가 주입 개행/구분자로
// 오염되지 않음을 고정(설계 §5). 구현 무변경 회귀 핀.
func TestAppendDropSanitizesFields(t *testing.T) {
	dir := t.TempDir()
	appendDrop(dir,
		"r\ta\nb\rc",            // reason: 탭·개행·CR → 공백
		"s\ti\nd",               // sessionID(≤8자라 절단 없음): 탭·개행 → 공백
		"Pre\nToolUse",          // hookEvent: 개행 → 공백
		strings.Repeat("T", 80)) // tool: 64자 상한

	raw := readDrops(t, dir)
	rec := strings.TrimRight(raw, "\n")
	// 주입 개행이 레코드를 여러 줄로 쪼개면 안 된다(단일 레코드 = 개행 0).
	if strings.Contains(rec, "\n") {
		t.Fatalf("주입 개행이 레코드를 분할: %q", raw)
	}
	fields := strings.Split(rec, "\t")
	if len(fields) != 5 { // ts + 4필드 — 탭 주입이 컬럼을 늘리면 안 된다
		t.Fatalf("필드 수=%d want 5(탭 주입 오염): %q", len(fields), raw)
	}
	for _, f := range fields[1:] {
		if strings.ContainsAny(f, "\n\r") {
			t.Fatalf("무해화 안 된 개행/CR 잔존: %q", f)
		}
		if len(f) > 64 {
			t.Fatalf("64자 상한 위반: len=%d %q", len(f), f)
		}
	}
	if fields[1] != "r a b c" || fields[3] != "Pre ToolUse" {
		t.Fatalf("무해화 결과 불일치: reason=%q event=%q", fields[1], fields[3])
	}
	if fields[4] != strings.Repeat("T", 64) { // 64자 정확 절단
		t.Fatalf("tool 절단 오류: %q", fields[4])
	}
}

// countReader — Read로 소비된 총 바이트를 센다(⑧ drain 검증용).
type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	m, err := c.r.Read(p)
	c.n += m
	return m, err
}

// ① CTR_HOOKS_OFF=1 → 0 반환·DB(세션 디렉터리) 미생성.
func TestHookHooksOff(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	if rc := runHook(t, storeRoot, in, map[string]string{"CTR_HOOKS_OFF": "1"}); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if _, err := os.Stat(sessDir(t, storeRoot, cwd)); !os.IsNotExist(err) {
		t.Fatalf("session dir must not exist (HOOKS_OFF), stat err=%v", err)
	}
}

// ② sessionstart.json → sessions 행(cc: id, retention 기본 2592000) + session_start 1건.
func TestHookSessionStart(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}

	reader, err := session.OpenReadOnly(sessDir(t, storeRoot, cwd))
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var sid string
	var retention int64
	if err := reader.QueryRow("SELECT session_id, retention_sec FROM sessions").Scan(&sid, &retention); err != nil {
		t.Fatalf("sessions row: %v", err)
	}
	if want := "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301"; sid != want {
		t.Fatalf("session_id=%q want %q", sid, want)
	}
	if retention != 2592000 {
		t.Fatalf("retention_sec=%d want 2592000", retention)
	}

	var n int
	var etype string
	if err := reader.QueryRow("SELECT count(*), coalesce(max(event_type),'') FROM session_events").Scan(&n, &etype); err != nil {
		t.Fatalf("events: %v", err)
	}
	if n != 1 || etype != "session_start" {
		t.Fatalf("events n=%d type=%q want 1/session_start", n, etype)
	}
}

// ②-b source 길이 봉인(최종 리뷰 C4): 거대 source(신뢰 불가 stdin)는 maxSourceBytes(64)로
// 절단되어 session_start payload에 기록된다 — EnsureSession의 ValidateEvent 우회 상한 방어.
func TestHookSessionStartSourceTruncated(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "sessionstart.json", map[string]any{
		"cwd":    cwd,
		"source": strings.Repeat("s", 5000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	reader, err := session.OpenReadOnly(sessDir(t, storeRoot, cwd))
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var payload string
	if err := reader.QueryRow("SELECT payload FROM session_events WHERE event_type='session_start'").Scan(&payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	var p struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(p.Source) != maxSourceBytes {
		t.Fatalf("source len=%d want %d(절단)", len(p.Source), maxSourceBytes)
	}
}

// ②-c source 멀티바이트 경계 절단(C4 rune-safe): maxSourceBytes(64)가 3바이트 룬(U+AC00) 중간에
// 떨어지는 입력에서 byte-slice(src[:64])는 룬을 반쪽 내 깨진 UTF-8을 남기지만, truncateUTF8은
// 룬 경계(63B=21룬)까지 물러나 온전한 UTF-8만 기록한다. 결과가 21룬 프리픽스와 정확히 같아야 한다.
func TestHookSessionStartSourceRuneBoundary(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	han := string(rune(0xAC00)) // "가" — 3바이트 UTF-8(소스에 멀티바이트 리터럴 미포함)
	in := fixtureWith(t, "sessionstart.json", map[string]any{
		"cwd":    cwd,
		"source": strings.Repeat(han, 100), // 300B, 64B는 22번째 룬(바이트 63~65) 중간
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	reader, err := session.OpenReadOnly(sessDir(t, storeRoot, cwd))
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var payload string
	if err := reader.QueryRow("SELECT payload FROM session_events WHERE event_type='session_start'").Scan(&payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	var p struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if want := strings.Repeat(han, 21); p.Source != want { // byte-slice면 64B(반쪽 룬)로 불일치
		t.Fatalf("source=%q(len=%d) want 21룬 프리픽스(len=%d)", p.Source, len(p.Source), len(want))
	}
}

// ③ 비-SessionStart 이벤트를 미지 세션으로 → D51: drop 대신 합성 등록(source="first-event") 후 계속.
// PreToolUse(Read)는 가드 통과 시 이벤트를 만들지 않으므로 session_start 1건만 남는다.
func TestHookUnknownSession(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "pretooluse-read.json", map[string]any{"cwd": cwd})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}

	dir := sessDir(t, storeRoot, cwd)
	reader, err := session.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	var n int
	var payload string
	qErr := reader.QueryRow("SELECT count(*) FROM session_events").Scan(&n)
	pErr := reader.QueryRow("SELECT payload FROM session_events WHERE event_type='session_start'").Scan(&payload)
	_ = reader.Close()
	if qErr != nil || pErr != nil {
		t.Fatalf("query: count=%v payload=%v", qErr, pErr)
	}
	if n != 1 {
		t.Fatalf("events=%d want 1 (session_start 합성 등록만 — PreToolUse 통과는 무이벤트)", n)
	}
	if !strings.Contains(payload, `"source":"first-event"`) {
		t.Fatalf("payload=%q want source=first-event(합성 마커)", payload)
	}
	if got := readDropsOpt(t, dir); strings.Contains(got, "unknown-session") {
		t.Fatalf("drops=%q — D51 후 unknown-session은 신규 발생 소멸", got)
	}
}

// D51 — 미지 세션의 PostToolUse: 합성 등록(session_start) + tool_call까지 총 2이벤트가
// 기록된다(트리거 이벤트도 정상 처리 경로 — 스펙 §2).
func TestHookFirstEventRegistrationPostToolUse(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "pretooluse-read.json", map[string]any{
		"cwd":             cwd,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Read",
	})
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"} // 느린 러너 fail-open drop 방지
	if rc := runHook(t, storeRoot, in, env); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	dir := sessDir(t, storeRoot, cwd)
	reader, err := session.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	var starts, calls int
	e1 := reader.QueryRow("SELECT count(*) FROM session_events WHERE event_type='session_start'").Scan(&starts)
	e2 := reader.QueryRow("SELECT count(*) FROM session_events WHERE event_type='tool_call'").Scan(&calls)
	_ = reader.Close()
	if e1 != nil || e2 != nil {
		t.Fatalf("query: %v %v", e1, e2)
	}
	if starts != 1 || calls != 1 {
		t.Fatalf("session_start=%d tool_call=%d want 1/1", starts, calls)
	}
}

// D51 — 동일 미지 세션 이벤트 2연속: 세션 1행·session_start 1건(EnsureSession 멱등의
// 신규 호출 지점 재확인 — 스펙 §2).
func TestHookFirstEventIdempotent(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "pretooluse-read.json", map[string]any{
		"cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "Read",
	})
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	for i := 0; i < 2; i++ {
		if rc := runHook(t, storeRoot, in, env); rc != 0 {
			t.Fatalf("run %d rc=%d want 0", i, rc)
		}
	}
	dir := sessDir(t, storeRoot, cwd)
	reader, err := session.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	var sessions, starts int
	e1 := reader.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions)
	e2 := reader.QueryRow("SELECT count(*) FROM session_events WHERE event_type='session_start'").Scan(&starts)
	_ = reader.Close()
	if e1 != nil || e2 != nil {
		t.Fatalf("query: %v %v", e1, e2)
	}
	if sessions != 1 || starts != 1 {
		t.Fatalf("sessions=%d starts=%d want 1/1 (멱등)", sessions, starts)
	}
}

// D51 — 가드가 ad를 쓰는 유일 경로 회귀(스펙 §2): SessionStart 없이 대형 in-boundary 파일의
// PreToolUse(Read)를 발화하면 합성 등록 후 가드가 deny를 내고, 그 warning이 합성 세션에 1건
// 기록된다. guardSetup(선등록)을 쓰지 않는 게 핵심 — deny 경로의 ad.Append가 first-event 자동
// 등록 세션 위에서 성립함을 고정한다. 대형 파일은 writeSized(strings.Repeat)로 임계 초과 조립.
func TestHookFirstEventGuardDenyWarning(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := evalLong(t, t.TempDir())
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024) // 임계 256KiB 초과 — in-boundary
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	out := runGuard(t, storeRoot, map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"file_path": big},
	}, env)
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("deny 출력 없음(합성 등록 후 가드 발화): %q", out)
	}
	reader, err := session.OpenReadOnly(sessDir(t, storeRoot, cwd))
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	var starts, warns int
	e1 := reader.QueryRow("SELECT count(*) FROM session_events WHERE event_type='session_start'").Scan(&starts)
	e2 := reader.QueryRow("SELECT count(*) FROM session_events WHERE event_type='warning'").Scan(&warns)
	_ = reader.Close()
	if e1 != nil || e2 != nil {
		t.Fatalf("query: %v %v", e1, e2)
	}
	if starts != 1 || warns != 1 {
		t.Fatalf("session_start=%d warning=%d want 1/1(합성 등록 + 가드 deny warning)", starts, warns)
	}
}

// TestAppendDropLineFormat — D43 5필드 포맷 핀(v0.9 이관): unknown-session 소멸로 결정적
// 발화 경로가 사라져 appendDrop을 직접 호출해 라인 계약을 고정한다(스펙 §2).
// "<unix-ts>\t<reason>\t<sid8>\t<hook_event>\t<tool>\n", sid8=앞 8자, 미상="-", 탭·개행 sanitize.
func TestAppendDropLineFormat(t *testing.T) {
	dir := t.TempDir()
	appendDrop(dir, "format-probe", "cc:99999999-0000-7000-8000-000000000000", "PostToolUse", "Read")
	appendDrop(dir, "tab\tnewline\n", "", "", "")
	data, err := os.ReadFile(filepath.Join(dir, "session.drops.log"))
	if err != nil {
		t.Fatalf("read drops: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d want 2", len(lines))
	}
	f := strings.Split(lines[0], "\t")
	if len(f) != 5 {
		t.Fatalf("fields=%d want 5 (line=%q)", len(f), lines[0])
	}
	if f[1] != "format-probe" || f[2] != "cc:99999" || f[3] != "PostToolUse" || f[4] != "Read" {
		t.Fatalf("fields=%v want [ts format-probe cc:99999 PostToolUse Read]", f)
	}
	g := strings.Split(lines[1], "\t")
	if len(g) != 5 || g[1] != "tab newline " || g[2] != "-" || g[3] != "-" || g[4] != "-" {
		t.Fatalf("sanitize/미상 필드 위반: %v", g)
	}
}

// ④ session_id 형식 불량(비UUID) → drop(bad-session-id), 세션 DB 미생성.
func TestHookBadSessionID(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "session_id": "not-a-uuid"})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	// 식별(session dir 도출) 전 단계 drop이라 storeRoot 레벨 drops.log에 기록된다(브리프 순서).
	if got := readDrops(t, storeRoot); !strings.Contains(got, "bad-session-id") {
		t.Fatalf("drops=%q want bad-session-id", got)
	}
	if _, err := os.Stat(filepath.Join(sessDir(t, storeRoot, cwd), "session.db")); !os.IsNotExist(err) {
		t.Fatalf("session.db must not exist for bad session_id, stat err=%v", err)
	}
}

// ④-b cwd가 실재하지 않으면(worktree 미식별) → drop(bad-cwd), 세션 DB 미생성. bad-session-id와
// 마찬가지로 세션 dir 도출 전 단계라 storeRoot 레벨 drops.log에 기록된다(hook.go 순서).
func TestHookBadCWD(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	in := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": filepath.Join(cwd, "no-such-dir")})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if got := readDrops(t, storeRoot); !strings.Contains(got, "bad-cwd") {
		t.Fatalf("drops=%q want bad-cwd", got)
	}
}

// ⑤ stdin 파싱 불능(잘린 JSON) → 0 반환 + drops(bad-input).
func TestHookBadInput(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	if rc := runHook(t, storeRoot, []byte("{not valid json"), nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if got := readDrops(t, storeRoot); !strings.Contains(got, "bad-input") {
		t.Fatalf("drops=%q want bad-input", got)
	}
}

// ⑥ lease 충돌(exclusive 선점) → 0 반환 + drops(lease-held).
func TestHookLeaseHeld(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	dir := sessDir(t, storeRoot, cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	release, err := store.AcquireLock(filepath.Join(dir, session.LockFileName), false) // exclusive 선점
	if err != nil {
		t.Fatalf("acquire exclusive: %v", err)
	}
	defer release()

	in := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if got := readDrops(t, dir); !strings.Contains(got, "lease-held") {
		t.Fatalf("drops=%q want lease-held", got)
	}
}

// ⑦ 반환값은 어떤 입력에서도 항상 0(fail-open 공통 계약).
func TestHookAlwaysReturnsZero(t *testing.T) {
	cwd := t.TempDir()
	valid := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	cases := []struct {
		name string
		in   []byte
		env  map[string]string
	}{
		{"hooks_off", valid, map[string]string{"CTR_HOOKS_OFF": "1"}},
		{"empty", []byte(""), nil},
		{"garbage", []byte("\x00\x01not json"), nil},
		{"bad_cwd", fixtureWith(t, "sessionstart.json", map[string]any{"cwd": filepath.Join(cwd, "nope")}), nil},
		{"valid", valid, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storeRoot := filepath.Join(t.TempDir(), "storeroot")
			if rc := runHook(t, storeRoot, tc.in, tc.env); rc != 0 {
				t.Fatalf("rc=%d want 0", rc)
			}
		})
	}
}

// ⑧ CTR_HOOKS_OFF 경로가 stdin을 EOF까지 drain한다(broken pipe 방지, 설계 §2.3).
func TestHookHooksOffDrainsStdin(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	payload := []byte(`{"hook_event_name":"SessionStart","session_id":"x","cwd":"y","source":"startup"}`)
	cr := &countReader{r: bytes.NewReader(payload)}
	rc := Run(context.Background(), cr, io.Discard, storeRoot, "test", HostClaude, func(k string) string {
		if k == "CTR_HOOKS_OFF" {
			return "1"
		}
		return ""
	})
	if rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if cr.n != len(payload) {
		t.Fatalf("drained %d bytes, want %d (stdin not consumed to EOF)", cr.n, len(payload))
	}
}

// ⑨ stdin이 상한(maxStdinBytes) 초과 → drop(stdin-oversize)·세션 DB 미생성·stdin EOF까지 drain
// (fail-open 봉인, 거대 payload OOM 방지). payload는 strings.Repeat 생성(리터럴 금지).
func TestHookStdinOversize(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	payload := []byte(strings.Repeat("a", maxStdinBytes+4096)) // 상한 초과 + drain할 잉여
	cr := &countReader{r: bytes.NewReader(payload)}
	rc := Run(context.Background(), cr, io.Discard, storeRoot, "test", HostClaude, func(string) string { return "" })
	if rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if got := readDrops(t, storeRoot); !strings.Contains(got, "stdin-oversize") {
		t.Fatalf("drops=%q want stdin-oversize", got)
	}
	if cr.n != len(payload) {
		t.Fatalf("drained %d bytes, want %d (stdin not drained to EOF)", cr.n, len(payload))
	}
	if _, err := os.Stat(filepath.Join(sessDir(t, storeRoot, cwd), "session.db")); !os.IsNotExist(err) {
		t.Fatalf("session.db must not exist on oversize stdin, stat err=%v", err)
	}
}

// ─── D53: 서브에이전트 생애주기 (스펙 v0.10 §0·§2) ────────────────────────────

// D53 — SubagentStart/Stop 생애주기 이벤트(스펙 v0.10 §0·§2). 등록된 세션에 각 1이벤트,
// summary는 "subagent started: <type>"·빈 type이면 접미 생략, attrs는 빈 값 포함 2키 기록.
func TestHookSubagentLifecycle(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	sid := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	// SessionStart로 세션 등록(기존 관례)
	start := mustJSON(t, map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": cwd, "source": "startup"})
	if rc := runHook(t, storeRoot, start, nil); rc != 0 {
		t.Fatalf("SessionStart rc=%d", rc)
	}
	cases := []struct {
		event, agentType, wantType, wantSummary string
	}{
		{"SubagentStart", "Explore", "subagent_start", "subagent started: Explore"},
		{"SubagentStop", "Explore", "subagent_stop", "subagent stopped: Explore"},
		{"SubagentStart", "", "subagent_start", "subagent started"}, // 빈 type — 접미 생략(§3 실측 근거)
	}
	for _, c := range cases {
		in := mustJSON(t, map[string]any{
			"hook_event_name": c.event, "session_id": sid, "cwd": cwd,
			"agent_id": "a1b2c3d4e5f6", "agent_type": c.agentType,
		})
		if rc := runHook(t, storeRoot, in, nil); rc != 0 {
			t.Fatalf("%s rc=%d", c.event, rc)
		}
		ev := lastEvent(t, sessDir(t, storeRoot, cwd), sid) // (type, summary, attrs) 조회 헬퍼
		if ev.Type != c.wantType || ev.Summary != c.wantSummary {
			t.Fatalf("got (%s,%q) want (%s,%q)", ev.Type, ev.Summary, c.wantType, c.wantSummary)
		}
		if ev.Attrs["agent_id"] != "a1b2c3d4e5f6" || ev.Attrs["agent_type"] != c.agentType {
			t.Fatalf("attrs=%v want agent_id/agent_type 병기(빈 값 포함)", ev.Attrs)
		}
	}
}

// D53 — 미등록 세션의 SubagentStart는 D51 합성 등록 경유 후 기록(파이프라인 합류 회귀).
func TestHookSubagentUnknownSessionSynthesizes(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	sid := "4a2504e0-4f89-41d3-9a0c-0305e82c3302"
	in := mustJSON(t, map[string]any{
		"hook_event_name": "SubagentStart", "session_id": sid, "cwd": cwd,
		"agent_id": "x1", "agent_type": "Explore",
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	// session_start(Source="first-event") + subagent_start = 2이벤트
	n := countEvents(t, sessDir(t, storeRoot, cwd), sid)
	if n != 2 {
		t.Fatalf("events=%d want 2 (합성 등록 + subagent_start)", n)
	}
}

// D53 비수용 부정(§2): SubagentStop의 last_assistant_message·agent_transcript_path는
// summary·attrs 어디에도 미출현.
func TestHookSubagentStopRejectsBodyFields(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	sid := "5b2504e0-4f89-41d3-9a0c-0305e82c3303"
	start := mustJSON(t, map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": cwd, "source": "startup"})
	if rc := runHook(t, storeRoot, start, nil); rc != 0 {
		t.Fatalf("SessionStart rc=%d", rc)
	}
	secret := "CANARY" + "SECRET" + "BODY" // 분해 조립(§12)
	in := mustJSON(t, map[string]any{
		"hook_event_name": "SubagentStop", "session_id": sid, "cwd": cwd,
		"agent_id": "x2", "agent_type": "claude",
		"last_assistant_message": secret, "agent_transcript_path": "C:/tmp/" + secret + ".jsonl",
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	ev := lastEvent(t, sessDir(t, storeRoot, cwd), sid)
	joined := ev.Summary + fmt.Sprint(ev.Attrs)
	if strings.Contains(joined, secret) {
		t.Fatalf("본문 필드 유출: %q", joined)
	}
}

// D53 표식(스펙 §0·§2): agent_id 실린 PostToolUse → attrs 병기, 부재 → 두 키 부재,
// 빈 값 → 표식 생략, wrong-type → 기본 이벤트 미드롭(RawMessage 기전이 계약 — 재검수 P2).
func TestHookAgentAttribution(t *testing.T) {
	cases := []struct {
		name      string
		event     string // PostToolUse | PostToolUseFailure(§2 — Failure도 동일 표식, 검수 반영)
		agentID   any    // nil=필드 자체 생략
		agentType any
		wantMark  bool
		wantType  string // 기본 이벤트 event_type(미드롭 단정)
	}{
		{"present", "PostToolUse", "a1b2", "Explore", true, "tool_call"},
		{"present_failure", "PostToolUseFailure", "a1b2", "Explore", true, "error"},
		{"absent", "PostToolUse", nil, nil, false, "tool_call"},
		{"empty_id", "PostToolUse", "", "Explore", false, "tool_call"},                 // best-effort — 표식 생략
		{"wrong_type", "PostToolUse", 123, map[string]int{"x": 1}, false, "tool_call"}, // 기본 이벤트 미드롭
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeRoot := filepath.Join(t.TempDir(), "storeroot")
			cwd := t.TempDir()
			sid := "6c2504e0-4f89-41d3-9a0c-0305e82c3304"
			start := mustJSON(t, map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": cwd, "source": "startup"})
			if rc := runHook(t, storeRoot, start, nil); rc != 0 {
				t.Fatalf("SessionStart rc=%d", rc)
			}
			m := map[string]any{
				"hook_event_name": c.event, "session_id": sid, "cwd": cwd,
				"tool_name": "Glob", "tool_input": map[string]string{"pattern": "*.go"},
			}
			if c.event == "PostToolUseFailure" {
				m["error"] = "exit code 1"
			} else {
				m["tool_response"] = map[string]string{"result": "ok"}
			}
			if c.agentID != nil {
				m["agent_id"] = c.agentID
			}
			if c.agentType != nil {
				m["agent_type"] = c.agentType
			}
			if rc := runHook(t, storeRoot, mustJSON(t, m), nil); rc != 0 {
				t.Fatalf("%s rc=%d", c.event, rc)
			}
			ev := lastEvent(t, sessDir(t, storeRoot, cwd), sid)
			if ev.Type != c.wantType {
				t.Fatalf("기본 이벤트 소실 — type=%s want %s (드롭 회귀)", ev.Type, c.wantType)
			}
			_, hasID := ev.Attrs["agent_id"]
			_, hasType := ev.Attrs["agent_type"]
			if c.wantMark {
				if ev.Attrs["agent_id"] != "a1b2" || ev.Attrs["agent_type"] != "Explore" {
					t.Fatalf("표식 값 불일치: attrs=%v", ev.Attrs)
				}
			} else if hasID || hasType { // 두 키 동시 부재(§2 — 검수 반영: type만 남는 회귀 차단)
				t.Fatalf("비표식인데 agent 키 잔존: attrs=%v", ev.Attrs)
			}
		})
	}
}

// F2(최종 리뷰): 병적으로 큰 agent_id는 PostToolUse 기본 이벤트를 죽이면 안 된다(스펙 v0.10 §0
// best-effort). 5000자 agent_id가 attrs로 흘러들면 session.MaxAttributesBytes(4096) 초과로
// Append가 ValidateEvent에서 실패하고 appendDrop이 기본 이벤트 전체를 버렸다(회귀). 가드 후:
// 표식만 생략(id 무효→ok=false)되고 tool_call 기본 이벤트는 살아남으며 agent 두 키는 미출현.
func TestHookAgentAttribution_OversizedIDPreservesBaseEvent(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	sid := "7d2504e0-4f89-41d3-9a0c-0305e82c3305"
	start := mustJSON(t, map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": cwd, "source": "startup"})
	if rc := runHook(t, storeRoot, start, nil); rc != 0 {
		t.Fatalf("SessionStart rc=%d", rc)
	}
	hugeID := strings.Repeat("a", 5000) // §12: 리터럴 아님(strings.Repeat) — 시크릿 형태 회피
	in := mustJSON(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": sid, "cwd": cwd,
		"tool_name": "Glob", "tool_input": map[string]string{"pattern": "*.go"},
		"tool_response": map[string]string{"result": "ok"},
		"agent_id":      hugeID, "agent_type": "Explore",
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("PostToolUse rc=%d", rc)
	}
	ev := lastEvent(t, sessDir(t, storeRoot, cwd), sid)
	if ev.Type != "tool_call" {
		t.Fatalf("기본 이벤트 소실 — type=%s want tool_call (거대 agent_id의 attrs 상한 초과로 Append 실패→드롭)", ev.Type)
	}
	if _, ok := ev.Attrs["agent_id"]; ok {
		t.Fatalf("거대 agent_id가 표식으로 실림: attrs=%v", ev.Attrs)
	}
	if _, ok := ev.Attrs["agent_type"]; ok {
		t.Fatalf("agent_type 키 잔존(표식 생략 위반): attrs=%v", ev.Attrs)
	}
}

// F2(최종 리뷰): 병적으로 큰 agent_type은 생애주기 이벤트를 죽이면 안 된다(동일 attrs 상한 경로,
// classify가 두 키를 항상 병기). 가드 후 subagent_start는 살아남고 agent_type만 ""로 기록된다
// (agent_id는 정상값 유지, 두 키 병기 불변).
func TestHookSubagentLifecycle_OversizedTypeSurvives(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	sid := "8e2504e0-4f89-41d3-9a0c-0305e82c3306"
	start := mustJSON(t, map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": cwd, "source": "startup"})
	if rc := runHook(t, storeRoot, start, nil); rc != 0 {
		t.Fatalf("SessionStart rc=%d", rc)
	}
	hugeType := strings.Repeat("t", 5000) // §12: 리터럴 아님(strings.Repeat)
	in := mustJSON(t, map[string]any{
		"hook_event_name": "SubagentStart", "session_id": sid, "cwd": cwd,
		"agent_id": "a1b2c3d4e5f6", "agent_type": hugeType,
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("SubagentStart rc=%d", rc)
	}
	ev := lastEvent(t, sessDir(t, storeRoot, cwd), sid)
	if ev.Type != "subagent_start" {
		t.Fatalf("생애주기 이벤트 소실 — type=%s want subagent_start (거대 agent_type의 attrs 상한 초과로 Append 실패→드롭)", ev.Type)
	}
	idVal, hasID := ev.Attrs["agent_id"]
	typVal, hasType := ev.Attrs["agent_type"]
	if !hasID || !hasType {
		t.Fatalf("생애주기 attrs 두 키 병기 위반: attrs=%v", ev.Attrs)
	}
	if s, _ := typVal.(string); s != "" {
		t.Fatalf("거대 agent_type가 그대로 실림(생략 안 됨): len=%d", len(s))
	}
	if idVal != "a1b2c3d4e5f6" {
		t.Fatalf("정상 agent_id 손상: agent_id=%v", idVal)
	}
}

// evRow — lastEvent 조회 결과(event_type·summary·payload attrs).
type evRow struct {
	Type    string
	Summary string
	Attrs   map[string]any
}

// mustJSON — map을 stdin JSON 바이트로 조립한다(json.Marshal 래퍼).
func mustJSON(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// lastEvent — sdir/session.db의 cc:<sid> 세션 최신 이벤트(type·summary·payload attrs)를 읽는다.
// hook 경로는 이벤트를 cc:+sid로 저장한다(HostClaude 귀속) — id DESC가 append 최신 행.
func lastEvent(t *testing.T, sdir, sid string) evRow {
	t.Helper()
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var ev evRow
	var payload sql.NullString
	if err := reader.QueryRow(
		"SELECT event_type, summary, payload FROM session_events WHERE session_id=? ORDER BY id DESC LIMIT 1",
		"cc:"+sid,
	).Scan(&ev.Type, &ev.Summary, &payload); err != nil {
		t.Fatalf("last event: %v", err)
	}
	if payload.Valid && payload.String != "" {
		if err := json.Unmarshal([]byte(payload.String), &ev.Attrs); err != nil {
			t.Fatalf("unmarshal attrs %q: %v", payload.String, err)
		}
	}
	return ev
}

// countEvents — sdir/session.db의 cc:<sid> 세션 이벤트 총수.
func countEvents(t *testing.T, sdir, sid string) int {
	t.Helper()
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var n int
	if err := reader.QueryRow("SELECT count(*) FROM session_events WHERE session_id=?", "cc:"+sid).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// ─── T5: 계측 매핑 + summary allowlist (설계 §3) ──────────────────────────────

// bashEvent — Bash tool_input(command)을 담은 hookInput을 만든다(classify 순수 함수 입력).
func bashEvent(t *testing.T, event, cmd string) hookInput {
	t.Helper()
	ti, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatalf("marshal tool_input: %v", err)
	}
	return hookInput{HookEventName: event, ToolName: "Bash", ToolInput: ti}
}

// 시나리오① — 분류 테이블 테스트(git/build/test/기본 각 2케이스) + 우선순위(실패 이벤트 >
// test_run): PostToolUseFailure는 명령이 test 패턴이어도 error로 분류돼야 한다(이벤트명 우선).
func TestClassify(t *testing.T) {
	cmds := []struct{ name, event, cmd, want string }{
		{"git_diff", "PostToolUse", "git diff HEAD", "git_diff"},
		{"git_commit", "PostToolUse", "git commit -m wip", "git_diff"},
		{"build_go", "PostToolUse", "go build ./...", "build_run"},
		{"build_npm", "PostToolUse", "npm run build", "build_run"},
		{"build_make", "PostToolUse", "make", "build_run"},
		{"test_go", "PostToolUse", "go test ./internal/...", "test_run"},
		{"test_pytest", "PostToolUse", "pytest -q", "test_run"},
		{"default_ls", "PostToolUse", "ls -la", "tool_call"},
		{"default_echo", "PostToolUse", "echo hi", "tool_call"},
		{"neg_grep_go_test", "PostToolUse", `grep -R "go test" .`, "tool_call"},    // F8: 인자 속 부분열 미분류
		{"neg_echo_npm_build", "PostToolUse", "echo npm run build", "tool_call"},   // F8: 인자 속 부분열 미분류
		{"sep_cd_and_go_test", "PostToolUse", "cd x && go test ./...", "test_run"}, // F8: 셸 구분자 직후는 분류
		{"priority_failure_over_test", "PostToolUseFailure", "go test ./...", "error"},
	}
	for _, tc := range cmds {
		t.Run(tc.name, func(t *testing.T) {
			in := bashEvent(t, tc.event, tc.cmd)
			if tc.event == "PostToolUseFailure" {
				in.Error = "Command exited with non-zero status code 1"
			}
			if et, _, _ := classify(in); et != tc.want {
				t.Fatalf("classify(%q)=%q want %q", tc.cmd, et, tc.want)
			}
		})
	}
	// file_edit: Write/Edit/NotebookEdit는 명령 패턴과 무관하게 file_edit.
	for _, tool := range []string{"Write", "Edit", "NotebookEdit"} {
		t.Run("file_edit_"+tool, func(t *testing.T) {
			in := hookInput{HookEventName: "PostToolUse", ToolName: tool, CWD: t.TempDir()}
			if et, _, _ := classify(in); et != "file_edit" {
				t.Fatalf("classify(%s)=%q want file_edit", tool, et)
			}
		})
	}
	// tool_call: 파일/Bash가 아닌 도구(Read 등)는 tool_call.
	t.Run("tool_call_read", func(t *testing.T) {
		in := hookInput{HookEventName: "PostToolUse", ToolName: "Read"}
		if et, _, _ := classify(in); et != "tool_call" {
			t.Fatalf("classify(Read)=%q want tool_call", et)
		}
	})
}

// 시나리오②③④⑤ — summary allowlist 조립: 파일 상대 경로·env 할당 마스킹·canary 부재·상한 절단.
func TestSummaryAllowlist(t *testing.T) {
	// ② file_edit → 워크스페이스 상대 경로 summary(절대경로 미노출).
	t.Run("file_edit_relative_path", func(t *testing.T) {
		base := t.TempDir()
		fp := filepath.Join(base, "internal", "hook", "hook.go")
		ti, _ := json.Marshal(map[string]string{"file_path": fp, "content": "x"})
		in := hookInput{HookEventName: "PostToolUse", ToolName: "Write", CWD: base, ToolInput: ti}
		_, summary, _ := classify(in)
		if summary != "Write: internal/hook/hook.go" {
			t.Fatalf("summary=%q want %q", summary, "Write: internal/hook/hook.go")
		}
		if strings.Contains(summary, base) {
			t.Fatalf("summary leaks absolute base %q: %q", base, summary)
		}
	})
	// ③ env 할당 첫 토큰 마스킹: `SERVICE_KEY=abc deploy` → 첫 토큰 <arg>(뒤 토큰 미포함).
	t.Run("env_assignment_masked", func(t *testing.T) {
		_, summary, _ := classify(bashEvent(t, "PostToolUse", "SERVICE_KEY=abc deploy"))
		if summary != "Bash: <arg>" {
			t.Fatalf("summary=%q want %q", summary, "Bash: <arg>")
		}
	})
	// 명령 단어 형태 첫 토큰은 원문 유지.
	t.Run("word_first_token_kept", func(t *testing.T) {
		_, summary, _ := classify(bashEvent(t, "PostToolUse", "git diff HEAD"))
		if summary != "Bash: git" {
			t.Fatalf("summary=%q want %q", summary, "Bash: git")
		}
	})
	// T5: 분류된 Bash는 매치 패턴명(안정 enum = event_type)을 matched_pattern attr로 방출하고,
	// 미매치(tool_call)는 방출하지 않는다(설계 §3 "매치 패턴명" allowlist).
	t.Run("matched_pattern_attr", func(t *testing.T) {
		if _, _, attrs := classify(bashEvent(t, "PostToolUse", "go test ./...")); attrs["matched_pattern"] != "test_run" {
			t.Fatalf("matched_pattern=%v want test_run", attrs["matched_pattern"])
		}
		if _, _, attrs := classify(bashEvent(t, "PostToolUse", "git diff HEAD")); attrs["matched_pattern"] != "git_diff" {
			t.Fatalf("matched_pattern=%v want git_diff", attrs["matched_pattern"])
		}
		if _, _, attrs := classify(bashEvent(t, "PostToolUse", "go build ./...")); attrs["matched_pattern"] != "build_run" {
			t.Fatalf("matched_pattern=%v want build_run", attrs["matched_pattern"])
		}
		if _, _, attrs := classify(bashEvent(t, "PostToolUse", "ls -la")); attrs["matched_pattern"] != nil {
			t.Fatalf("tool_call matched_pattern=%v want absent", attrs["matched_pattern"])
		}
	})
	// ④ secret canary(분할 리터럴)를 비-첫토큰 인자에 심으면 summary·attrs에 원문 부재(allowlist 1차 방어).
	t.Run("canary_absent_from_summary_and_attrs", func(t *testing.T) {
		canary := "xox" + "b-Ca" + "naryLeak0123456789" // 런타임 조립 — 소스에 연속 토큰 부재
		_, summary, attrs := classify(bashEvent(t, "PostToolUse", "deploy --token="+canary))
		if strings.Contains(summary, canary) {
			t.Fatalf("canary leaked into summary: %q", summary)
		}
		if aj, _ := json.Marshal(attrs); strings.Contains(string(aj), canary) {
			t.Fatalf("canary leaked into attrs: %s", aj)
		}
	})
	// ④(2차 방어) 첫 토큰 자체가 비밀 형태면 Redact가 가리고 redaction=spans로 기록.
	t.Run("secret_first_token_redacted", func(t *testing.T) {
		canary := "xox" + "b-Ca" + "naryLeak0123456789"
		ev, ok := buildEvent(bashEvent(t, "PostToolUse", canary))
		if !ok {
			t.Fatalf("buildEvent ok=false")
		}
		if strings.Contains(ev.Summary, canary) {
			t.Fatalf("canary survived Redact in summary: %q", ev.Summary)
		}
		if ev.Redaction != "spans" {
			t.Fatalf("redaction=%q want spans", ev.Redaction)
		}
	})
	// ⑤ 상한: 초대형 첫 토큰 → summary는 2048B 이하로 절단.
	t.Run("summary_truncated_2048", func(t *testing.T) {
		ev, ok := buildEvent(bashEvent(t, "PostToolUse", strings.Repeat("a", 3000)))
		if !ok {
			t.Fatalf("buildEvent ok=false")
		}
		if len(ev.Summary) > session.MaxSummaryBytes {
			t.Fatalf("summary len=%d want <=%d", len(ev.Summary), session.MaxSummaryBytes)
		}
	})
	// 오류 요약: 정규화 분류·코드만(원문 미수용) + exit_code attr.
	t.Run("error_normalized_no_raw", func(t *testing.T) {
		in := hookInput{
			HookEventName: "PostToolUseFailure", ToolName: "Bash",
			Error: "Command exited with non-zero status code 1",
		}
		et, summary, attrs := classify(in)
		if et != "error" {
			t.Fatalf("event_type=%q want error", et)
		}
		if summary != "Bash: exit 1" {
			t.Fatalf("summary=%q want %q", summary, "Bash: exit 1")
		}
		if strings.Contains(summary, "Command exited") {
			t.Fatalf("raw error text leaked into summary: %q", summary)
		}
		if attrs["exit_code"] != 1 {
			t.Fatalf("attrs exit_code=%v want 1", attrs["exit_code"])
		}
	})
}

// 시나리오⑥ — 픽스처 round-trip(§10 canary 게이트, 세션 측): posttooluse-bash.json을 canary
// 삽입 후 실행 → tool_call 1건 저장 + canary가 FTS(요약 색인)에서 미회수.
func TestHookInstrumentationCanaryGate(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	cwd := t.TempDir()
	// 세션 먼저 생성(미지 세션의 후속 이벤트는 drop되므로).
	if rc := runHook(t, storeRoot, fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd}), nil); rc != 0 {
		t.Fatalf("sessionstart rc=%d want 0", rc)
	}
	canary := "xox" + "b-Ca" + "naryLeak0123456789" // 런타임 조립 slack 형태 비밀
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"command": "deploy --token=" + canary, "description": "x"},
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("posttooluse rc=%d want 0", rc)
	}

	dir := sessDir(t, storeRoot, cwd)
	reader, err := session.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var n int
	if err := reader.QueryRow("SELECT count(*) FROM session_events WHERE event_type='tool_call'").Scan(&n); err != nil {
		t.Fatalf("count tool_call: %v", err)
	}
	if n != 1 {
		t.Fatalf("tool_call events=%d want 1", n)
	}

	// canary substring(순수 영숫자 — FTS5 bareword 안전)이 trigram 색인에서 미회수여야 한다.
	leak := "Nary" + "Leak0123456789"
	var rowid int64
	qErr := reader.QueryRow("SELECT rowid FROM fts_ev_trigram WHERE fts_ev_trigram MATCH ?", leak).Scan(&rowid)
	if qErr == nil {
		t.Fatalf("canary가 FTS에서 회수됨 — 요약에 비밀 유출")
	}
	if !errors.Is(qErr, sql.ErrNoRows) {
		t.Fatalf("fts query: %v", qErr)
	}
}

// ─── T6: Shadow Recall (설계 §5) ─────────────────────────────────────────────

// shadowSetup — session_start를 발화해 세션을 만들고 (storeRoot, cwd, contentDir, sessDir)를
// 반환한다. contentDir=<storeRoot>/projects/<pid>(content.db 위치, main·§5 join과 동일).
func shadowSetup(t *testing.T) (storeRoot, cwd, contentDir, sdir string) {
	t.Helper()
	storeRoot = filepath.Join(t.TempDir(), "storeroot")
	cwd = t.TempDir()
	if rc := runHook(t, storeRoot, fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd}), nil); rc != 0 {
		t.Fatalf("sessionstart rc=%d want 0", rc)
	}
	canon, err := ident.Canonicalize(cwd)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	contentDir = filepath.Join(storeRoot, "projects", canon.ProjectID)
	sdir = filepath.Join(contentDir, "worktrees", canon.WorktreeID)
	return
}

// bigStdout — n바이트 'a'로 채운 tool_response 오브젝트 override(strings.Repeat 생성 —
// 16KiB+ 리터럴 금지 규율). 직렬화 크기는 key/quote 오버헤드로 n보다 약간 크다.
func bigStdout(n int) map[string]any {
	return map[string]any{"stdout": strings.Repeat("a", n), "stderr": ""}
}

// contentArtifacts — contentDir의 content.db(read-only)에서 artifacts 행 수를 센다.
// content.db 미존재(=Shadow 미저장)면 -1.
func contentArtifacts(t *testing.T, contentDir string) int {
	t.Helper()
	if _, err := os.Stat(filepath.Join(contentDir, "content.db")); os.IsNotExist(err) {
		return -1
	}
	st, err := store.Open(contentDir, true)
	if err != nil {
		t.Fatalf("open content.db ro: %v", err)
	}
	defer func() { _ = st.Close() }()
	var n int
	if err := st.Reader().QueryRow("SELECT count(*) FROM artifacts").Scan(&n); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	return n
}

// eventRefs — sdir/session.db에서 event_type 첫 행의 artifact_refs(JSON 배열)를 읽는다.
func eventRefs(t *testing.T, sdir, eventType string) []string {
	t.Helper()
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var refsJSON sql.NullString
	if err := reader.QueryRow("SELECT artifact_refs FROM session_events WHERE event_type=? LIMIT 1", eventType).Scan(&refsJSON); err != nil {
		t.Fatalf("query %s refs: %v", eventType, err)
	}
	var refs []string
	if refsJSON.Valid && refsJSON.String != "" {
		if err := json.Unmarshal([]byte(refsJSON.String), &refs); err != nil {
			t.Fatalf("unmarshal refs %q: %v", refsJSON.String, err)
		}
	}
	return refs
}

// ① tool_response ≤ CTR_SHADOW_MIN(기본 16KiB) → 미저장(content.db 미생성).
func TestShadowUnderMinSkips(t *testing.T) {
	storeRoot, cwd, contentDir, _ := shadowSetup(t)
	// 기본 픽스처(작은 tool_response)로 PostToolUse 발화.
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{"cwd": cwd})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(미저장) — MIN 이하는 Shadow 미저장", n)
	}
}

// ② tool_response > MIN → content.db 아티팩트 + artifact_created + tool_result_summary,
// tool_result_summary는 artifact ref(artifact://cc:<uuid>/sha256-<64hex>) 형식을 담는다.
func TestShadowOverMinStores(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_response": bigStdout(20000), // >16384
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1", n)
	}
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	for _, et := range []string{"artifact_created", "tool_result_summary"} {
		var n int
		if err := reader.QueryRow("SELECT count(*) FROM session_events WHERE event_type=?", et).Scan(&n); err != nil {
			_ = reader.Close()
			t.Fatalf("count %s: %v", et, err)
		}
		if n != 1 {
			_ = reader.Close()
			t.Fatalf("%s events=%d want 1", et, n)
		}
	}
	_ = reader.Close()

	want := "artifact://cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301/sha256-"
	// D30: artifact_created·tool_result_summary 둘 다 rep.Hash 기반 동일 ref를 실어야 한다
	// (shadow: URI로 저장돼도 ref는 content_hash 주소 — §5).
	hexHash := regexp.MustCompile(`^[0-9a-f]{64}$`) // ingest_test.go:855 관례 — hex-charset까지 단정
	for _, et := range []string{"artifact_created", "tool_result_summary"} {
		refs := eventRefs(t, sdir, et)
		if len(refs) != 1 {
			t.Fatalf("%s refs=%v want 1개", et, refs)
		}
		if !strings.HasPrefix(refs[0], want) {
			t.Fatalf("%s ref=%q want prefix %q", et, refs[0], want)
		}
		if hash := strings.TrimPrefix(refs[0], want); !hexHash.MatchString(hash) {
			t.Fatalf("%s ref hash=%q want ^[0-9a-f]{64}$(hex sha256)", et, hash)
		}
	}
}

// ③ tool_response > CTR_SHADOW_MAX → 미저장 + drops(shadow-oversize). MAX를 env로 낮춰
// 대용량 리터럴 없이 게이트를 검증한다.
func TestShadowOversizeDrops(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_response": bigStdout(25000),
	})
	env := map[string]string{"CTR_SHADOW_MIN": "100", "CTR_SHADOW_MAX": "20000"}
	if rc := runHook(t, storeRoot, in, env); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(oversize 미저장)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-oversize") {
		t.Fatalf("drops=%q want shadow-oversize", got)
	}
}

// ④ 파일 유래 도구(Read) + denylist 경로(.env) → 미저장 + drops(shadow-denylist).
func TestShadowDenylistSkips(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_name":     "Read",
		"tool_input":    map[string]any{"file_path": filepath.Join(cwd, ".env")},
		"tool_response": bigStdout(20000), // MIN 통과해야 denylist 게이트 도달
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(denylist 미저장)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-denylist") {
		t.Fatalf("drops=%q want shadow-denylist", got)
	}
}

// D39-① Bash `cat .env`(정적 증명 덤프, denylist 파일) → 미저장 + drops shadow-denylist.
// 응답 본문에 런타임 분할 canary를 실어 "비밀이 store에 없다"까지 겸증한다(§8).
func TestShadowCommandDenylistSkipsBash(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	canary := "xox" + "b-1234567890-ABCDEFGHIJKLMNOP" // 런타임 조립 — 소스에 연속 토큰 금지
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_input":    map[string]any{"command": "cat .env"},
		"tool_response": map[string]any{"stdout": canary + strings.Repeat("x", 20000), "stderr": ""},
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(denylist 미저장 — content.db 미생성)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-denylist") {
		t.Fatalf("drops=%q want shadow-denylist", got)
	}
}

// D39-② PowerShell `Get-Content .env` — Bash와 대칭.
func TestShadowCommandDenylistSkipsPowerShell(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_name":     "PowerShell",
		"tool_input":    map[string]any{"command": "Get-Content .env"},
		"tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(denylist 미저장)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-denylist") {
		t.Fatalf("drops=%q want shadow-denylist", got)
	}
}

// D39-④ 점 세그먼트 변형도 정규화 후 대조된다 — `.docker/config.json` 접미 규칙 우회 봉쇄
// (계획 리뷰 F2).
func TestShadowCommandDenylistNormalizes(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_input":    map[string]any{"command": "cat ./.docker/./config.json"},
		"tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(정규화 후 denylist 대조)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-denylist") {
		t.Fatalf("drops=%q want shadow-denylist", got)
	}
}

// D39-③ 증명 불가 출력(파이프)은 현행대로 색인 — 커버리지 급감 방지(설계 §7 잔여 한계).
func TestShadowCommandUnprovenStillIndexes(t *testing.T) {
	storeRoot, cwd, contentDir, _ := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_input":    map[string]any{"command": "cat .env | head"},
		"tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(증명 불가 = 현행 색인 유지)", n)
	}
}

// ⑤ NUL 바이트를 담은 tool_response(바이너리) → 미저장. 유효 JSON은 raw NUL을 못 담으므로
// (json은 로 escape) shadowCapture를 직접 호출해 raw NUL을 주입한다.
func TestShadowBinarySkips(t *testing.T) {
	_, _, contentDir, sdir := shadowSetup(t)
	ad, err := session.OpenAppend(context.Background(), sdir, session.AppendOptions{
		ExternalSessionID: "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		Producer:          "context-router/test",
	})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	defer func() { _ = ad.Close() }()
	body := append(bytes.Repeat([]byte("a"), 20000), 0x00) // raw NUL → 바이너리
	in := hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: json.RawMessage(body)}
	shadowCapture(context.Background(), ad, in, sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", func(string) string { return "" })
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(바이너리 미저장)", n)
	}
}

// ⑤-b 실경로 형태(최종 리뷰 C2): 유효 JSON에서 NUL은 유니코드 이스케이프 텍스트로 도착한다 —
// 전체 파이프라인(runHook)으로 이스케이프 판정을 검증한다(응답 문자열에 raw NUL을 넣으면
// fixtureWith의 json.Marshal이 실경로와 동일하게 이스케이프한다).
func TestShadowEscapedNULSkips(t *testing.T) {
	storeRoot, cwd, contentDir, _ := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_response": map[string]any{"stdout": strings.Repeat("a", 20000) + "\x00binary", "stderr": ""},
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(이스케이프 NUL 미저장)", n)
	}
}

// ⑥ §10 canary 게이트(shadow 측): 응답 본문의 secret(분할 리터럴)이 저장 아티팩트에서
// redaction=spans로 가려지고 FTS(trigram)에서 미회수.
func TestShadowCanaryRedacted(t *testing.T) {
	storeRoot, cwd, contentDir, _ := shadowSetup(t)
	canary := "xox" + "b-Ca" + "naryLeak0123456789" // 런타임 조립 slack 형태
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_response": map[string]any{"stdout": strings.Repeat("a", 20000) + " " + canary, "stderr": ""},
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	st, err := store.Open(contentDir, true)
	if err != nil {
		t.Fatalf("open content.db ro: %v", err)
	}
	defer func() { _ = st.Close() }()
	var redaction string
	if err := st.Reader().QueryRow("SELECT redaction FROM artifacts LIMIT 1").Scan(&redaction); err != nil {
		t.Fatalf("query redaction: %v", err)
	}
	if redaction != "spans" {
		t.Fatalf("redaction=%q want spans", redaction)
	}
	leak := "nary" + "Leak0123456789" // canary alnum 꼬리의 부분열
	var rowid int64
	qErr := st.Reader().QueryRow("SELECT rowid FROM fts_trigram WHERE fts_trigram MATCH ?", leak).Scan(&rowid)
	if qErr == nil {
		t.Fatalf("canary가 content.db FTS에서 회수됨 — 저장본 redaction 실패")
	}
	if !errors.Is(qErr, sql.ErrNoRows) {
		t.Fatalf("fts query: %v", qErr)
	}
}

// ⑧ 콘텐츠 해시 dedup — 같은 응답 2회 → 아티팩트 1개.
func TestShadowDedup(t *testing.T) {
	storeRoot, cwd, contentDir, _ := shadowSetup(t)
	mk := func() []byte {
		return fixtureWith(t, "posttooluse-bash.json", map[string]any{
			"cwd":           cwd,
			"tool_response": bigStdout(20000),
		})
	}
	for i := 0; i < 2; i++ {
		if rc := runHook(t, storeRoot, mk(), nil); rc != 0 {
			t.Fatalf("call %d rc=%d want 0", i, rc)
		}
	}
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(해시 dedup)", n)
	}
}

// ⑨ URI 해시 정합 — 조립한 ref의 hash == store.ArtifactHashByID(저장 artifact).
func TestShadowRefHashMatches(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	refs := eventRefs(t, sdir, "tool_result_summary")
	if len(refs) != 1 {
		t.Fatalf("refs=%v want 1", refs)
	}
	refHash := refs[0][strings.LastIndex(refs[0], "sha256-")+len("sha256-"):]

	st, err := store.Open(contentDir, true)
	if err != nil {
		t.Fatalf("open content.db ro: %v", err)
	}
	defer func() { _ = st.Close() }()
	var artID int64
	if err := st.Reader().QueryRow("SELECT id FROM artifacts LIMIT 1").Scan(&artID); err != nil {
		t.Fatalf("query artifact id: %v", err)
	}
	dbHash, err := st.ArtifactHashByID(context.Background(), artID)
	if err != nil {
		t.Fatalf("ArtifactHashByID: %v", err)
	}
	if refHash != dbHash {
		t.Fatalf("ref hash=%q != ArtifactHashByID=%q", refHash, dbHash)
	}
}

// ⑩ content store open-lock 점유 상태에서 deadline 300ms 내 실패 + drops(shadow-store) —
// ctx-aware OpenContext 변형이 5초 하드 대기 대신 예산 안에서 포기한다.
func TestShadowStoreLockDeadline(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	if err := os.MkdirAll(contentDir, 0o700); err != nil {
		t.Fatalf("mkdir contentDir: %v", err)
	}
	// content.db.rebuild.lock을 외부 배타 선점(store 내부 상수와 동일 파일명).
	release, err := store.AcquireLock(filepath.Join(contentDir, "content.db.rebuild.lock"), false)
	if err != nil {
		t.Fatalf("선점 잠금: %v", err)
	}
	defer release()

	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_response": bigStdout(20000),
	})
	start := time.Now()
	rc := runHook(t, storeRoot, in, map[string]string{"CTR_HOOK_DEADLINE_MS": "300"})
	elapsed := time.Since(start)
	if rc != 0 {
		t.Fatalf("rc=%d want 0(fail-open)", rc)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("deadline 미관측 의심 — %v 소요(5초 하드 대기 추정)", elapsed)
	}
	if n := contentArtifacts(t, contentDir); n > 0 {
		t.Fatalf("artifacts=%d want 0/-1(락 점유로 미저장)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-store") {
		t.Fatalf("drops=%q want shadow-store", got)
	}
}

// ⑪ store open은 성공하지만 ingest.Run이 실패하면 → 미저장 + drops(shadow-ingest). 취소된 ctx를
// 쓴다: lockStoreCtx는 락이 한가하면 첫 tryLockFile에서 ctx 검사 없이 즉시 성공하므로 OpenContext는
// 취소 ctx에서도 통과하고, ingest.Run 첫 줄 ctx.Err()가 실패를 낸다(shadow-ingest 경로의 결정적 트리거).
func TestShadowIngestDrops(t *testing.T) {
	_, _, contentDir, sdir := shadowSetup(t)
	ad, err := session.OpenAppend(context.Background(), sdir, session.AppendOptions{
		ExternalSessionID: "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		Producer:          "context-router/test",
	})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	defer func() { _ = ad.Close() }()
	// MIN 통과·유효 JSON·비바이너리 leaf여야 store open까지 도달한다(그다음이 ingest 실패 지점).
	body := append(append([]byte{'"'}, bytes.Repeat([]byte("a"), 20000)...), '"') // JSON 문자열 리터럴
	in := hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: json.RawMessage(body)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ingest.Run 진입 즉시 ctx.Err()로 실패
	shadowCapture(ctx, ad, in, sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", func(string) string { return "" })

	if n := contentArtifacts(t, contentDir); n > 0 {
		t.Fatalf("artifacts=%d want 0/-1(ingest 실패로 미저장)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-ingest") {
		t.Fatalf("drops=%q want shadow-ingest", got)
	}
}

// ⑫ ingest는 성공하지만 세션 append가 실패하면 → drops(shadow-append). ad를 shadowCapture 전에
// Close해 두면 store.OpenContext+ingest.Run은 fresh content store에서 성공(Indexed==1)하고, 이후
// shadowAppend→ad.Append가 닫힌 writer에 "database is closed"로 실패한다(shadow-append 경로).
func TestShadowAppendDrops(t *testing.T) {
	_, _, contentDir, sdir := shadowSetup(t)
	ad, err := session.OpenAppend(context.Background(), sdir, session.AppendOptions{
		ExternalSessionID: "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		Producer:          "context-router/test",
	})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	if err := ad.Close(); err != nil { // append 이전에 닫아 writer 실패를 주입
		t.Fatalf("Close: %v", err)
	}
	// MIN 통과·유효 JSON·비바이너리 leaf → ingest까지 성공한 뒤 append 단계에서만 실패시킨다.
	body := append(append([]byte{'"'}, bytes.Repeat([]byte("a"), 20000)...), '"') // JSON 문자열 리터럴
	in := hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: json.RawMessage(body)}
	shadowCapture(context.Background(), ad, in, sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", func(string) string { return "" })

	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(ingest 성공 후 append 실패 시나리오)", n)
	}
	got := readDrops(t, sdir)
	if !strings.Contains(got, "shadow-append") {
		t.Fatalf("drops=%q want shadow-append", got)
	}
	// 사이드카 필드 정합: shadow-append 실패 drop도 event/tool을 담아야 한다(hook.go appendDrop와 동형).
	for _, want := range []string{"PostToolUse", "Bash"} {
		if !strings.Contains(got, want) {
			t.Fatalf("shadow-append drop에 %q 없음(event/tool 미전달): %q", want, got)
		}
	}
}

// TestShadowCaptureRecordsLedgerRow: 성공한 포착마다 원장에 분모 행이 하나 남고, 저장되지
// 않은 호출은 남기지 않는다. 이 분모가 없으면 회수율의 분모를 72시간 스냅샷에서 세게 되고,
// 그것이 세션 54가 상계 11.6%를 잘못 낸 형태다 — 13일치 분자를 사흘치 분모로 나눴다.
// 읽는 자리는 store.LedgerStats(contentDir)다. **임계 미달 케이스는 store를 열기 전에
// 반환하므로 ledger.db 자체가 없고, 그때 LedgerStats는 nil 슬라이스+nil을 낸다** — 그것을
// "0행"으로 받는다(Fatal이 아니다).
func TestShadowCaptureRecordsLedgerRow(t *testing.T) {
	_, _, contentDir, sdir := shadowSetup(t)
	ad, err := session.OpenAppend(context.Background(), sdir, session.AppendOptions{
		ExternalSessionID: "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		Producer:          "context-router/test",
	})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	defer func() { _ = ad.Close() }()
	getenv := func(string) string { return "" }

	// ① 임계 미달 → 저장도 분모도 없다(store를 열기 전에 반환한다).
	small, err := json.Marshal(bigStdout(100))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	shadowCapture(context.Background(), ad,
		hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: small},
		sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", getenv)
	if n := hookLedgerRows(t, contentDir); n != 0 {
		t.Fatalf("임계 미달인데 분모 행=%d", n)
	}

	// ② 임계 초과 → 저장 1건, 분모 1행.
	big, err := json.Marshal(bigStdout(20000))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	shadowCapture(context.Background(), ad,
		hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: big},
		sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", getenv)
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1", n)
	}
	if n := hookLedgerRows(t, contentDir); n != 1 {
		t.Fatalf("성공한 포착 뒤 분모 행=%d want 1", n)
	}
}

// hookLedgerRows — contentDir 원장의 hook:shadow 행 수. ledger.db 미존재는 nil 슬라이스라
// 자연히 0이 된다(store.LedgerStats 계약).
func hookLedgerRows(t *testing.T, contentDir string) int64 {
	t.Helper()
	rows, err := store.LedgerStats(contentDir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	for _, r := range rows {
		if r.Tool == "hook:shadow" {
			return r.Calls
		}
	}
	return 0
}

// ─── T7: large-read guard 4조건 판정 (설계 §4) ────────────────────────────────

// guardSetup — session_start를 발화해 세션을 선재시키고 (storeRoot, cwd, contentDir, sdir)를
// 반환한다. 가드는 SessionExists 통과 후 동작하므로 세션이 있어야 한다(shadowSetup과 동형).
// evalLong — 픽스처 디렉터리를 장문명으로 정규화한다. Windows CI의 t.TempDir()는 8.3 단축명
// (C:\Users\RUNNER~1\...)을 낼 수 있고, 그 ~는 bashDumpArg의 어휘 거부(설계상 allow-bias)에 걸려
// guardBash가 stat/store 전에 반환한다 — 로컬은 ~가 없어 통과하는 픽스처 이식성 버그. EvalSymlinks가
// Windows 8.3 성분을 장문명으로 해석한다(경로는 존재해야 함 — t.TempDir()은 존재). 프로덕션의 ~ allow는
// 그대로다. 정규화하지 않으면 allow-계열 테스트도 의도한 조건 전에 어휘 게이트에 걸려 무의미 통과한다.
func evalLong(t *testing.T, dir string) string {
	t.Helper()
	if p, err := filepath.EvalSymlinks(dir); err == nil {
		return p
	}
	return dir
}

func guardSetup(t *testing.T) (storeRoot, cwd, contentDir, sdir string) {
	t.Helper()
	storeRoot = filepath.Join(t.TempDir(), "storeroot")
	cwd = evalLong(t, t.TempDir())
	if rc := runHook(t, storeRoot, fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd}), nil); rc != 0 {
		t.Fatalf("sessionstart rc=%d want 0", rc)
	}
	canon, err := ident.Canonicalize(cwd)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	contentDir = filepath.Join(storeRoot, "projects", canon.ProjectID)
	sdir = filepath.Join(contentDir, "worktrees", canon.WorktreeID)
	return
}

// runGuard — pretooluse-read.json에 overrides(cwd·tool_input 필수 재정의 — 픽스처의 하드코딩
// file_path는 호스트 의존적)를 적용해 Run을 호출하고 stdout(=deny JSON 또는 빈 문자열)을 캡처한다.
func runGuard(t *testing.T, storeRoot string, overrides map[string]any, env map[string]string) string {
	t.Helper()
	in := fixtureWith(t, "pretooluse-read.json", overrides)
	var out bytes.Buffer
	rc := Run(context.Background(), bytes.NewReader(in), &out, storeRoot, "test", HostClaude, func(k string) string { return env[k] })
	if rc != 0 {
		t.Fatalf("guard rc=%d want 0", rc)
	}
	return out.String()
}

// writeSized — path에 size바이트('a') 파일을 쓴다(strings.Repeat 생성 — 대용량 리터럴 금지 규율).
func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("a", size)), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ① 경계 밖 대형 파일 → allow(stdout 무출력). ingest가 ErrWorkspace로 색인 불가 → Indexed==0.
func TestGuardOutsideBoundaryAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	outside := filepath.Join(t.TempDir(), "big.txt") // cwd와 형제 — 경계 밖
	writeSized(t, outside, 300*1024)
	out := runGuard(t, storeRoot, map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"file_path": outside},
	}, nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (경계 밖 = allow)", out)
	}
}

// ② offset/limit 부분 읽기 → allow(임계 초과·경계 내여도 통과).
func TestGuardPartialReadAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	out := runGuard(t, storeRoot, map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"file_path": big, "offset": 1, "limit": 100},
	}, nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (부분 읽기 = allow)", out)
	}
}

// ②-b offset 단독·limit 단독도 부분 읽기 → allow(T7: 둘 중 하나만 있어도 통과, 존재-판정이 &&가 아님).
func TestGuardPartialReadOffsetOrLimitAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]any
	}{
		{"offset_alone", map[string]any{"offset": 1}},
		{"limit_alone", map[string]any{"limit": 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storeRoot, cwd, _, _ := guardSetup(t)
			big := filepath.Join(cwd, "big.txt")
			writeSized(t, big, 300*1024) // 임계 초과·경계 내 — 부분 읽기 판정만이 통과 근거
			ti := map[string]any{"file_path": big}
			for k, v := range tc.in {
				ti[k] = v
			}
			out := runGuard(t, storeRoot, map[string]any{"cwd": cwd, "tool_input": ti}, nil)
			if out != "" {
				t.Fatalf("stdout=%q want empty (%s = allow)", out, tc.name)
			}
		})
	}
}

// ③ 임계 이하 → allow.
func TestGuardUnderThresholdAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	small := filepath.Join(cwd, "small.txt")
	writeSized(t, small, 1024) // 1KiB < 256KiB
	out := runGuard(t, storeRoot, map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"file_path": small},
	}, nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (임계 이하 = allow)", out)
	}
}

// ④ denylist 파일(.env, Indexed=0 Skipped) → allow + 거짓 "이미 인덱스됨" 안내 부재.
// err==nil인데 Indexed==0인 케이스 — err 검사만으로는 부족함을 게이트한다(브리프 ④).
func TestGuardDenylistAllowsNoFalseIndexed(t *testing.T) {
	storeRoot, cwd, contentDir, _ := guardSetup(t)
	env := filepath.Join(cwd, ".env")
	writeSized(t, env, 300*1024) // 임계 초과라 ③ 통과 → denylist라 Skipped(Indexed==0)
	out := runGuard(t, storeRoot, map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"file_path": env},
	}, nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty — denylist는 Skipped(Indexed==0)라 deny 금지", out)
	}
	if strings.Contains(out, "이미 인덱스됨") {
		t.Fatalf("거짓 '이미 인덱스됨' 안내 출력됨: %q", out)
	}
	// .env는 색인되지 않아야 한다(denylist).
	if n := contentArtifacts(t, contentDir); n > 0 {
		t.Fatalf("artifacts=%d want 0/-1(.env 미색인)", n)
	}
}

// ⑤ 정상 대형 파일 → deny JSON 스키마 일치 + content.db 아티팩트 1건 + warning 이벤트 1건.
func TestGuardLargeFileDenies(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	out := runGuard(t, storeRoot, map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"file_path": big},
	}, nil)

	// deny JSON 스키마(T0 검증) — 모든 하위 값이 문자열이라 중첩 맵으로 디코드.
	var got map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("deny stdout이 유효 JSON 아님: %v (out=%q)", err, out)
	}
	hso := got["hookSpecificOutput"]
	if hso["hookEventName"] != "PreToolUse" {
		t.Fatalf("hookEventName=%q want PreToolUse", hso["hookEventName"])
	}
	if hso["permissionDecision"] != "deny" {
		t.Fatalf("permissionDecision=%q want deny", hso["permissionDecision"])
	}
	if !strings.Contains(hso["permissionDecisionReason"], "이미 인덱스됨") {
		t.Fatalf("reason=%q want 포함 '이미 인덱스됨'", hso["permissionDecisionReason"])
	}
	if !strings.Contains(hso["permissionDecisionReason"], "ctr_search") || !strings.Contains(hso["permissionDecisionReason"], "ctr_fetch") {
		t.Fatalf("reason=%q want ctr_search·ctr_fetch 안내", hso["permissionDecisionReason"])
	}

	// 현장 인덱싱 아티팩트 존재.
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(현장 인덱싱 성공)", n)
	}

	// warning 이벤트 1건 + 차단 파일 상대 경로·크기 포함.
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var n int
	var summary string
	if err := reader.QueryRow("SELECT count(*), coalesce(max(summary),'') FROM session_events WHERE event_type='warning'").Scan(&n, &summary); err != nil {
		t.Fatalf("count warning: %v", err)
	}
	if n != 1 {
		t.Fatalf("warning events=%d want 1", n)
	}
	if !strings.Contains(summary, "big.txt") || !strings.Contains(summary, "307200") {
		t.Fatalf("warning summary=%q want 상대경로 big.txt·크기 307200 포함", summary)
	}
	if strings.Contains(summary, cwd) {
		t.Fatalf("warning summary가 절대경로 누출: %q", summary)
	}
}

// ⑥ 인덱싱 실패 주입(content store 잠금) → allow(무출력) + drops(guard-store). deadline
// 예산 안에서 포기(5초 하드 대기 금지).
func TestGuardIndexFailureAllows(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
	if err := os.MkdirAll(contentDir, 0o700); err != nil {
		t.Fatalf("mkdir contentDir: %v", err)
	}
	release, err := store.AcquireLock(filepath.Join(contentDir, "content.db.rebuild.lock"), false) // 배타 선점
	if err != nil {
		t.Fatalf("선점 잠금: %v", err)
	}
	defer release()

	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	start := time.Now()
	out := runGuard(t, storeRoot, map[string]any{
		"cwd":        cwd,
		"tool_input": map[string]any{"file_path": big},
	}, map[string]string{"CTR_HOOK_DEADLINE_MS": "300"})
	elapsed := time.Since(start)
	if out != "" {
		t.Fatalf("stdout=%q want empty (인덱싱 실패 = allow)", out)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("deadline 미관측 의심 — %v 소요", elapsed)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "guard-store") {
		t.Fatalf("drops=%q want guard-store", got)
	}
}

// ─── D32: Bash 단일파일 덤프 가드 정적 판정 (설계 v0.3 §4) ─────────────────────

// TestBashDumpArg: D32 어휘 판정 — 단일 단순 `cat <경로>`만 경로 인자를 반환하고
// 나머지는 전부 ""(allow). 오탐 deny 차단이 목적이므로 거부 케이스가 본론이다.
func TestBashDumpArg(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"cat /c/big/file.log", "/c/big/file.log"},
		{"cat C:/big/file.log", "C:/big/file.log"}, // 절대 여부는 dumpAbsPath 몫
		{"cat file.log", "file.log"},               // 어휘상 후보 — 절대 판정에서 탈락
		{"cat -n /c/f", ""},                        // 옵션 — 제외
		{"cat /c/a /c/b", ""},                      // 인자 2개 — 제외
		{"cat /c/f | head", ""},                    // 파이프 — 제외
		{"cat /c/f > /c/g", ""},                    // 리다이렉트 — 제외
		{"cat /c/f; ls", ""},                       // 체이닝 — 제외
		{"cat \"/c/f\"", ""},                       // 인용 — 제외(보수)
		{"cat /c/with\\ space", ""},                // 백슬래시 — 제외(bash가 소비)
		{"cat /c/f ", ""},                          // NBSP — bash IFS와 달리 Fields가 쪼갬 → 비ASCII 전면 거부
		{"cat /c/a!b", "/c/a!b"},                   // ! — 비대화형 bash에서 리터럴, 허용
		{"cat /c/~backup", ""},                     // ~ 전면 배제 — 의도적 미탐(allow 편향)
		{"type /c/big/file.log", ""},               // bash type=명령 조회, 덤프 아님
		{"tac /c/f", ""},                           // cat 외 명령 — 제외
		{"", ""},
	}
	for _, c := range cases {
		if got := bashDumpArg(c.cmd); got != c.want {
			t.Fatalf("bashDumpArg(%q)=%q want %q", c.cmd, got, c.want)
		}
	}
}

// TestDumpAbsPath: OS별 절대경로 정규화 — goos 주입으로 양쪽 분기를 한 OS에서 검증.
func TestDumpAbsPath(t *testing.T) {
	cases := []struct{ goos, arg, want string }{
		{"windows", "/c/big/f.log", "c:/big/f.log"}, // MSYS → 드라이브형 변환
		{"windows", "C:/big/f.log", "C:/big/f.log"},
		{"windows", "file.log", ""}, // 상대 — 제외
		{"windows", "/tmp/f", ""},   // 드라이브 불명 — 제외(보수)
		{"linux", "/tmp/f", "/tmp/f"},
		{"linux", "C:/big/f.log", ""}, // Unix에선 상대경로 — 제외
		{"linux", "file.log", ""},
	}
	for _, c := range cases {
		if got := dumpAbsPath(c.goos, c.arg); got != c.want {
			t.Fatalf("dumpAbsPath(%q,%q)=%q want %q", c.goos, c.arg, got, c.want)
		}
	}
}

// TestPsDumpArg: D36 어휘 판정(bashDumpArg 자매) — "대소문자 무시 덤프 토큰(Get-Content·gc·
// cat·type) + 위치 인자 정확히 1개"만 경로를 반환, 나머지는 전부 ""(allow). 오탐 deny 차단이
// 목적이라 거부 케이스가 본론이다. G2 실측(설계 §11.1): 입력은 tool_input.command.
func TestPsDumpArg(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{`Get-Content C:\big\file.log`, `C:\big\file.log`}, // 백슬래시는 PS 경로 구분자 — 허용(bash와 차이)
		{"get-content C:/big/file.log", "C:/big/file.log"}, // 대소문자 무시
		{"gc C:/big/file.log", "C:/big/file.log"},          // alias
		{"cat C:/big/file.log", "C:/big/file.log"},         // pwsh alias
		{"type C:/big/file.log", "C:/big/file.log"},        // cmd 유래 alias(PS에선 Get-Content)
		{"Get-Content file.log", "file.log"},               // 어휘상 후보 — 절대 판정은 psAbsPath 몫
		{"Get-Content -TotalCount 5 C:/f", ""},             // 부분 읽기 — 인자 3개
		{"gc C:/f -Tail 10", ""},                           // 부분 읽기(후위)
		{"gc -Raw C:/f", ""},                               // 대시 토큰
		{"gc -Path C:/f", ""},                              // 명명 파라미터
		{"gc C:/a,C:/b", ""},                               // 콤마 배열 — 제외
		{"gc C:/*.log", ""},                                // 와일드카드 — 제외
		{"gc $env:TEMP/f", ""},                             // 변수 전개 — 제외
		{"gc C:/f | Select-Object -First 5", ""},           // 파이프 — 제외
		{"gc C:/f; ls", ""},                                // 복합식 — 제외
		{"Get-Content 'C:/f'", ""},                         // 인용 — 제외(보수)
		{"gc `C:/f`", ""},                                  // 백틱(PS 이스케이프) — 제외
		{"gc C:/한글.log", ""},                               // 비ASCII — 전면 판정 포기
		{"gc ~/f", ""},                                     // ~ 홈 확장 — 제외
		{"gc @(C:/f)", ""},                                 // @ 서브식/배열 — 제외
		{"Set-Content C:/f", ""},                           // 덤프 아닌 명령
		{"", ""},
	}
	for _, c := range cases {
		if got := psDumpArg(c.cmd); got != c.want {
			t.Fatalf("psDumpArg(%q)=%q want %q", c.cmd, got, c.want)
		}
	}
}

// TestPsAbsPath: psDumpArg 인자의 절대 판정 — bash용 MSYS /x/ 변환을 승계하지 않는다
// (설계 §11.1 파생 ②: PS에서 /c/x는 현재 드라이브 루트 상대라 변환 시 오파일 판정 위험).
func TestPsAbsPath(t *testing.T) {
	cases := []struct{ goos, arg, want string }{
		{"windows", `C:\big\f.log`, "C:/big/f.log"}, // goos-키 백슬래시→슬래시 정규화(호스트 무관)
		{"windows", "C:/big/f.log", "C:/big/f.log"},
		{"windows", "/c/big/f.log", ""},  // MSYS형 — PS에선 드라이브 상대, 비절대(allow)
		{"windows", `\\srv\share\f`, ""}, // UNC — 드라이브형 아님, 보수 allow
		{"windows", "f.log", ""},         // 상대
		{"windows", "C:big.log", ""},     // 드라이브 상대(C: 뒤 구분자 없음)
		{"linux", "/var/log/big.log", "/var/log/big.log"},
		{"linux", "f.log", ""},
	}
	for _, c := range cases {
		if got := psAbsPath(c.goos, c.arg); got != c.want {
			t.Fatalf("psAbsPath(%q,%q)=%q want %q", c.goos, c.arg, got, c.want)
		}
	}
}

// TestGuardDumpPath — D47 호스트×GOOS 단독 게이트(스펙 §2·§6): 게이트는 술어+경로해석
// 묶음 정책이며 직렬·혼합 금지. ②는 psAbsPath의 드라이브 상대 판정이 bash MSYS 변환의
// 오파일 deny를 재도입하지 않는다는 핀(v0.4 §11.1 파생 ②).
func TestGuardDumpPath(t *testing.T) {
	cases := []struct {
		name    string
		host    Host
		goos    string
		command string
		want    string
	}{
		{"① cx+win PS게이트: Get-Content 백슬래시", HostCodex, "windows", `Get-Content C:\ws\big.txt`, "C:/ws/big.txt"},
		{"② cx+win: cat /c/… MSYS 오파일 금지 핀", HostCodex, "windows", "cat /c/big.log", ""},
		{"③ cx+unix bash게이트: cat 절대경로", HostCodex, "linux", "cat /abs/big", "/abs/big"},
		{"④ cx+win 양쪽 미매치: rg", HostCodex, "windows", "rg pattern C:/ws/f.txt", ""},
		{"cc+win 현행 유지: cat /c/… MSYS deny 핀", HostClaude, "windows", "cat /c/big.log", "c:/big.log"},
		{"cc+win 현행 유지: Get-Content은 비덤프", HostClaude, "windows", `Get-Content C:\ws\big.txt`, ""},
		{"cc+unix 현행 유지", HostClaude, "linux", "cat /abs/big", "/abs/big"},
		{"cx+win PS게이트: gc 드라이브 슬래시", HostCodex, "windows", "gc C:/ws/big.txt", "C:/ws/big.txt"},
		{"cx+win PS게이트: 파이프 → allow", HostCodex, "windows", `Get-Content C:\f.txt | Select-Object -First 5`, ""},
	}
	for _, c := range cases {
		if got := guardDumpPath(c.host, c.goos, c.command); got != c.want {
			t.Fatalf("%s: guardDumpPath=%q want %q", c.name, got, c.want)
		}
	}
}

// runGuardBash — posttooluse-bash.json을 PreToolUse(Bash)로 재정의하고 command만 교체해 Run을
// 호출한 뒤 stdout(deny JSON 또는 빈 문자열)을 반환한다(runGuard의 Bash 형제). command 조립은
// 호출자가 filepath.ToSlash로 슬래시화 — Windows t.TempDir() 백슬래시가 bashDumpArg 어휘 판정에서
// 거부되지 않도록(설계 §4).
func runGuardBash(t *testing.T, storeRoot, cwd, command string, env map[string]string) string {
	t.Helper()
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"hook_event_name": "PreToolUse",
		"cwd":             cwd,
		"tool_input":      map[string]any{"command": command},
	})
	var out bytes.Buffer
	rc := Run(context.Background(), bytes.NewReader(in), &out, storeRoot, "test", HostClaude, func(k string) string { return env[k] })
	if rc != 0 {
		t.Fatalf("guardBash rc=%d want 0", rc)
	}
	return out.String()
}

// D32-① 대형 파일 단순 cat → deny JSON + warning 이벤트(Bash·cat·상대경로·크기·ctr_search 포함,
// 절대경로 비포함) + 현장 인덱싱 아티팩트 1건.
func TestGuardBashLargeFileDenies(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	// 느린 CI 러너에서 기본 2s 데드라인이 fail-open drop(빈 stdout)을 유발 — deny 단정 + 300KB
	// 현장 색인 테스트는 러너 속도 무의존이어야 함(v0.4 CI F2와 동일 클래스·동일 처방).
	out := runGuardBash(t, storeRoot, cwd, "cat "+filepath.ToSlash(big), map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"})

	var got map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("deny stdout이 유효 JSON 아님: %v (out=%q)", err, out)
	}
	hso := got["hookSpecificOutput"]
	if hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "deny" {
		t.Fatalf("deny 스키마 불일치: %+v (out=%q)", hso, out)
	}

	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(현장 인덱싱 성공)", n)
	}

	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var n int
	var summary string
	if err := reader.QueryRow("SELECT count(*), coalesce(max(summary),'') FROM session_events WHERE event_type='warning'").Scan(&n, &summary); err != nil {
		t.Fatalf("count warning: %v", err)
	}
	if n != 1 {
		t.Fatalf("warning events=%d want 1", n)
	}
	for _, want := range []string{"Bash", "cat", "big.txt", "307200", "ctr_search"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("warning summary=%q want 포함 %q", summary, want)
		}
	}
	if strings.Contains(summary, cwd) || strings.Contains(summary, filepath.ToSlash(cwd)) {
		t.Fatalf("warning summary가 절대경로 누출: %q", summary)
	}
}

// cxDumpCmd — GOOS별 단일파일 덤프 명령: 비Windows는 bash `cat`, Windows는 raw PS `Get-Content`
// (§7 실측 — Codex Windows exec는 raw PS 구문). 양 OS CI에서 각자의 게이트를 검증한다(D47).
func cxDumpCmd(path string) string {
	if runtime.GOOS == "windows" {
		return "Get-Content " + path
	}
	return "cat " + path
}

// runCodexBashGuard — cx:<uuid> 세션을 SessionStart로 등록한 뒤 PreToolUse(Bash) 덤프를 발화하고
// stdout(deny JSON 또는 빈 문자열)을 돌려준다. guardSetup은 cc:만 등록하므로 cx: 등록을 별도 주입한다.
// runHookHost 4번째 인자는 []byte 계약 — json.Marshal한 바이트를 전달한다.
func runCodexBashGuard(t *testing.T, storeRoot, ws, uuid, command string, env map[string]string) string {
	t.Helper()
	start, _ := json.Marshal(hookInput{HookEventName: "SessionStart", SessionID: uuid, CWD: ws, Source: "startup"})
	if rc := runHookHost(t, HostCodex, storeRoot, start, env); rc != 0 {
		t.Fatalf("cx SessionStart rc=%d", rc)
	}
	ti, _ := json.Marshal(map[string]string{"command": command})
	pre, _ := json.Marshal(hookInput{HookEventName: "PreToolUse", ToolName: "Bash", SessionID: uuid, CWD: ws, ToolInput: ti})
	var out bytes.Buffer
	if rc := Run(context.Background(), bytes.NewReader(pre), &out, storeRoot, "test", HostCodex, func(k string) string { return env[k] }); rc != 0 {
		t.Fatalf("cx PreToolUse rc=%d", rc)
	}
	return out.String()
}

// D47 — cx: Bash 덤프 deny: Windows는 PS 게이트(Get-Content), 비Windows는 bash 게이트(cat).
// deny JSON + warning 이벤트 + 현장 색인 artifact + ctr_search 안내를 단정한다(스펙 §6).
func TestGuardCodexBashDeny(t *testing.T) {
	storeRoot := t.TempDir()
	ws := evalLong(t, t.TempDir())
	big := filepath.Join(ws, "big.txt")
	writeSized(t, big, 300*1024) // 임계 256KiB 초과
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	out := runCodexBashGuard(t, storeRoot, ws, "019f0000-0000-7000-8000-0000000000c1", cxDumpCmd(big), env)

	for _, want := range []string{`"permissionDecision":"deny"`, "ctr_search"} {
		if !strings.Contains(out, want) {
			t.Fatalf("deny 출력에 %q 없음:\n%s", want, out)
		}
	}
	// 현장 인덱싱 artifact 1건(cx:<uuid> 세션) + warning 이벤트 1건 — D32-① 단정을 cx:에 적용.
	// warning 카운트는 cx:% 세션 한정 — 필터 없이는 deny가 다른 네임스페이스(cc:/bare)에
	// 오귀속돼도 통과해 D47 격리 단정이 무력화된다.
	canon, err := ident.Canonicalize(ws)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if n := contentArtifacts(t, filepath.Join(storeRoot, "projects", canon.ProjectID)); n != 1 {
		t.Fatalf("artifacts=%d want 1(현장 인덱싱 성공)", n)
	}
	reader, err := session.OpenReadOnly(sessDir(t, storeRoot, ws))
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var n int
	var summary string
	if err := reader.QueryRow("SELECT count(*), coalesce(max(summary),'') FROM session_events WHERE event_type='warning' AND session_id LIKE 'cx:%'").Scan(&n, &summary); err != nil {
		t.Fatalf("count warning: %v", err)
	}
	if n != 1 {
		t.Fatalf("warning events=%d want 1", n)
	}
	token := "cat"
	if runtime.GOOS == "windows" {
		token = "Get-Content"
	}
	for _, want := range []string{"Bash", token, "big.txt", "307200", "ctr_search"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("warning summary=%q want 포함 %q", summary, want)
		}
	}
	if strings.Contains(summary, ws) || strings.Contains(summary, filepath.ToSlash(ws)) {
		t.Fatalf("warning summary가 절대경로 누출: %q", summary)
	}
}

// D47/D39 — cx: denylist 파일(.env, Indexed==0 Skipped) 덤프 → allow(무출력) + 미색인.
// cc: denylist 관례(TestGuardBashDenylistAllows)를 HostCodex+GOOS별 명령으로 복제(스펙 §6).
func TestGuardCodexBashDenylistAllows(t *testing.T) {
	storeRoot := t.TempDir()
	ws := evalLong(t, t.TempDir())
	envFile := filepath.Join(ws, ".env")
	writeSized(t, envFile, 300*1024) // 임계 초과라 크기 게이트 통과 → denylist라 Skipped
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	out := runCodexBashGuard(t, storeRoot, ws, "019f0000-0000-7000-8000-0000000000c2", cxDumpCmd(envFile), env)
	if out != "" {
		t.Fatalf("stdout=%q want empty (denylist = allow)", out)
	}
	canon, err := ident.Canonicalize(ws)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if n := contentArtifacts(t, filepath.Join(storeRoot, "projects", canon.ProjectID)); n > 0 {
		t.Fatalf("artifacts=%d want 0/-1(.env 미색인)", n)
	}
}

// D32-② 파이프 포함 → allow(bashDumpArg가 | 로 어휘 거부).
func TestGuardBashPipeAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	out := runGuardBash(t, storeRoot, cwd, "cat "+filepath.ToSlash(big)+" | head", nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (파이프 = allow)", out)
	}
}

// D32-③ 상대경로 → allow(dumpAbsPath가 비절대로 거부). 상대경로라 슬래시화 불필요.
func TestGuardBashRelativePathAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	writeSized(t, filepath.Join(cwd, "big.txt"), 300*1024)
	out := runGuardBash(t, storeRoot, cwd, "cat big.txt", nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (상대경로 = allow)", out)
	}
}

// D32-④ 임계 미만 → allow.
func TestGuardBashUnderThresholdAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	small := filepath.Join(cwd, "small.txt")
	writeSized(t, small, 1024) // 1KiB < 256KiB
	out := runGuardBash(t, storeRoot, cwd, "cat "+filepath.ToSlash(small), nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (임계 이하 = allow)", out)
	}
}

// D32-⑤ 경계 밖 대형 파일 → allow(ingest ErrWorkspace → Indexed==0).
func TestGuardBashOutsideBoundaryAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	outside := filepath.Join(evalLong(t, t.TempDir()), "big.txt") // cwd와 형제 — 경계 밖
	writeSized(t, outside, 300*1024)
	out := runGuardBash(t, storeRoot, cwd, "cat "+filepath.ToSlash(outside), nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (경계 밖 = allow)", out)
	}
}

// D32-⑥ denylist 파일(.env, Indexed==0 Skipped) cat → allow + artifact 0건.
func TestGuardBashDenylistAllows(t *testing.T) {
	storeRoot, cwd, contentDir, _ := guardSetup(t)
	env := filepath.Join(cwd, ".env")
	writeSized(t, env, 300*1024) // 임계 초과라 크기 게이트 통과 → denylist라 Skipped
	out := runGuardBash(t, storeRoot, cwd, "cat "+filepath.ToSlash(env), nil)
	if out != "" {
		t.Fatalf("stdout=%q want empty (denylist = allow)", out)
	}
	if n := contentArtifacts(t, contentDir); n > 0 {
		t.Fatalf("artifacts=%d want 0/-1(.env 미색인)", n)
	}
}

// D32-⑦ content store open-lock 점유 → allow(무출력) + drops(guard-store). Bash 분기 fail-open
// 직접 커버(Read ⑥ 패턴 복제). deadline 예산 안에서 포기(5초 하드 대기 금지).
func TestGuardBashStoreLockAllows(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
	if err := os.MkdirAll(contentDir, 0o700); err != nil {
		t.Fatalf("mkdir contentDir: %v", err)
	}
	release, err := store.AcquireLock(filepath.Join(contentDir, "content.db.rebuild.lock"), false) // 배타 선점
	if err != nil {
		t.Fatalf("선점 잠금: %v", err)
	}
	defer release()

	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	start := time.Now()
	out := runGuardBash(t, storeRoot, cwd, "cat "+filepath.ToSlash(big), map[string]string{"CTR_HOOK_DEADLINE_MS": "300"})
	elapsed := time.Since(start)
	if out != "" {
		t.Fatalf("stdout=%q want empty (인덱싱 실패 = allow)", out)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("deadline 미관측 의심 — %v 소요", elapsed)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "guard-store") {
		t.Fatalf("drops=%q want guard-store", got)
	}
}

// runGuardPowerShell — runGuardBash의 PowerShell 형제(tool_name·tool_input만 다름).
func runGuardPowerShell(t *testing.T, storeRoot, cwd, command string, env map[string]string) string {
	t.Helper()
	in := fixtureWith(t, "pretooluse-read.json", map[string]any{
		"cwd":        cwd,
		"tool_name":  "PowerShell",
		"tool_input": map[string]any{"command": command, "description": "test"},
	})
	var out bytes.Buffer
	rc := Run(context.Background(), bytes.NewReader(in), &out, storeRoot, "test", HostClaude, func(k string) string { return env[k] })
	if rc != 0 {
		t.Fatalf("guardPowerShell rc=%d want 0", rc)
	}
	return out.String()
}

// D36-① 대형 파일 단순 Get-Content → deny JSON + warning 이벤트(PowerShell·명령 토큰·상대경로·
// 크기·ctr_search 포함, 절대경로 비포함) + 현장 인덱싱 아티팩트 1건.
func TestGuardPowerShellLargeFileDenies(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	// 느린 CI 러너 데드라인 fail-open drop 방지 — TestGuardBashLargeFileDenies와 동일 처방.
	out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content "+filepath.ToSlash(big), map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"})

	var got map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("deny stdout이 유효 JSON 아님: %v (out=%q)", err, out)
	}
	hso := got["hookSpecificOutput"]
	if hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "deny" {
		t.Fatalf("deny 스키마 불일치: %+v", hso)
	}
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(현장 인덱싱 성공)", n)
	}
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var n int
	var summary string
	if err := reader.QueryRow("SELECT count(*), coalesce(max(summary),'') FROM session_events WHERE event_type='warning'").Scan(&n, &summary); err != nil {
		t.Fatalf("count warning: %v", err)
	}
	if n != 1 {
		t.Fatalf("warning events=%d want 1", n)
	}
	for _, want := range []string{"PowerShell", "Get-Content", "big.txt", "307200", "ctr_search"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("warning summary=%q want 포함 %q", summary, want)
		}
	}
	if strings.Contains(summary, cwd) || strings.Contains(summary, filepath.ToSlash(cwd)) {
		t.Fatalf("warning summary가 절대경로 누출: %q", summary)
	}
}

// D36-② 파이프 포함 → allow. ③ 부분 읽기 플래그 → allow. ④ 상대경로 → allow.
// ⑤ MSYS형 /c/... → allow(psAbsPath 비승계 — bash 가드와 다른 지점의 회귀 방지).
func TestGuardPowerShellPipeAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content "+filepath.ToSlash(big)+" | Select-Object -First 5", nil); out != "" {
		t.Fatalf("stdout=%q want empty (파이프 = allow)", out)
	}
}

func TestGuardPowerShellPartialReadAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content -TotalCount 5 "+filepath.ToSlash(big), nil); out != "" {
		t.Fatalf("stdout=%q want empty (부분 읽기 = allow)", out)
	}
}

func TestGuardPowerShellRelativePathAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	writeSized(t, filepath.Join(cwd, "big.txt"), 300*1024)
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content big.txt", nil); out != "" {
		t.Fatalf("stdout=%q want empty (상대경로 = allow)", out)
	}
}

func TestGuardPowerShellMsysFormAllows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("드라이브형 경로 전제")
	}
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	msys := "/" + strings.ToLower(string(big[0])) + filepath.ToSlash(big[2:]) // C:\x → /c/x
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content "+msys, nil); out != "" {
		t.Fatalf("stdout=%q want empty (MSYS형 = PS 드라이브 상대 = allow)", out)
	}
}

// D54 — Grep 가드 5분기(스펙 §2 ①~⑤) + 전용 reason. deny는 content+head_limit 0 단 하나.
func TestGuardGrep(t *testing.T) {
	cases := []struct {
		name string
		ti   map[string]any
		deny bool
	}{
		{"content_unlimited", map[string]any{"pattern": "x", "output_mode": "content", "head_limit": 0}, true},
		{"content_default", map[string]any{"pattern": "x", "output_mode": "content"}, false}, // 부재=250 캡
		{"content_capped", map[string]any{"pattern": "x", "output_mode": "content", "head_limit": 50}, false},
		{"files_unlimited", map[string]any{"pattern": "x", "output_mode": "files_with_matches", "head_limit": 0}, false},
		{"unparsable", map[string]any{"head_limit": "zero"}, false}, // 파싱 불가 → 통과
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeRoot := filepath.Join(t.TempDir(), "storeroot")
			cwd := t.TempDir()
			sid := "8e2504e0-4f89-41d3-9a0c-0305e82c3306"
			start := mustJSON(t, map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": cwd, "source": "startup"})
			if rc := runHook(t, storeRoot, start, nil); rc != 0 {
				t.Fatalf("SessionStart rc=%d", rc)
			}
			in := mustJSON(t, map[string]any{
				"hook_event_name": "PreToolUse", "session_id": sid, "cwd": cwd,
				"tool_name": "Grep", "tool_input": c.ti,
			})
			out := runHookCaptureStdout(t, storeRoot, in)
			nEv := countEvents(t, sessDir(t, storeRoot, cwd), sid)
			if c.deny {
				if !strings.Contains(out, `"permissionDecision":"deny"`) {
					t.Fatalf("deny 미발화: %q", out)
				}
				if !strings.Contains(out, "head_limit") || !strings.Contains(out, "ctr_search") {
					t.Fatalf("Grep 전용 reason 아님(§2 ① — 하드코딩 회귀): %q", out)
				}
				if nEv != 2 { // session_start + warning 1건(§2 ①)
					t.Fatalf("warning 이벤트 수=%d want 2(start+warning)", nEv)
				}
			} else {
				if out != "" {
					t.Fatalf("통과 케이스에 출력: %q", out)
				}
				if nEv != 1 { // session_start뿐 — 무이벤트(§2 ②)
					t.Fatalf("통과인데 이벤트 증가: %d", nEv)
				}
			}
		})
	}
}
