package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
