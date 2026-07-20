package hook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// runHook — Run을 결정론 입력(env 맵·io.Discard stdout)으로 호출한다.
func runHook(t *testing.T, storeRoot string, in []byte, env map[string]string) int {
	t.Helper()
	return Run(context.Background(), bytes.NewReader(in), io.Discard, storeRoot, "test", func(k string) string { return env[k] })
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

// ③ 비-SessionStart 이벤트를 미지 세션으로 → 이벤트 0건 + drops 1줄(unknown-session).
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
	qErr := reader.QueryRow("SELECT count(*) FROM session_events").Scan(&n)
	_ = reader.Close()
	if qErr != nil {
		t.Fatalf("count events: %v", qErr)
	}
	if n != 0 {
		t.Fatalf("events=%d want 0 (unknown session must not append)", n)
	}
	if got := readDrops(t, dir); !strings.Contains(got, "unknown-session") {
		t.Fatalf("drops=%q want unknown-session", got)
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
	release, err := store.AcquireLock(filepath.Join(dir, "session.lock"), false) // exclusive 선점
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
	rc := Run(context.Background(), cr, io.Discard, storeRoot, "test", func(k string) string {
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

	refs := eventRefs(t, sdir, "tool_result_summary")
	if len(refs) != 1 {
		t.Fatalf("tool_result_summary refs=%v want 1개", refs)
	}
	want := "artifact://cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301/sha256-"
	if !strings.HasPrefix(refs[0], want) {
		t.Fatalf("ref=%q want prefix %q", refs[0], want)
	}
	hash := strings.TrimPrefix(refs[0], want)
	if len(hash) != 64 {
		t.Fatalf("ref hash 길이=%d want 64(hex sha256)", len(hash))
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
