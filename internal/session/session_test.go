package session

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/wotjr1649/context-router/internal/store"
)

func openT(t *testing.T, dir string, opts Options) *DB {
	t.Helper()
	d, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestOpen_ReopenIdempotent — 브리프 Step1 ①: Open→재Open이 user_version=1을 유지하고,
// 각 Open이 자신만의 session_id로 session_start 이벤트를 append한다(누적, 덮어쓰지 않음).
func TestOpen_ReopenIdempotent(t *testing.T) {
	dir := t.TempDir()
	d1, err := Open(dir, Options{Producer: "test/1"})
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := d1.Reader().QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version=%d want %d", v, schemaVersion)
	}
	sid1 := d1.SessionID()
	if sid1 == "" {
		t.Fatal("SessionID empty")
	}
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(dir, Options{Producer: "test/1"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	if err := d2.Reader().QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("reopen user_version=%d want %d (멱등 위반)", v, schemaVersion)
	}
	sid2 := d2.SessionID()
	if sid2 == "" || sid2 == sid1 {
		t.Fatalf("reopen SessionID=%q want 새 값 (sid1=%q)", sid2, sid1)
	}

	var starts int
	if err := d2.Reader().QueryRow("SELECT COUNT(*) FROM session_events WHERE event_type=?", eventTypeSessionStart).Scan(&starts); err != nil {
		t.Fatal(err)
	}
	if starts != 2 {
		t.Fatalf("session_start count=%d want 2 (Open 2회)", starts)
	}
}

// TestOpen_SessionStartAndSessionsRow — 브리프 Step1 ③: session_start 자동 기록 +
// sessions 행(producer·retention_sec)이 불변 메타로 남는다.
func TestOpen_SessionStartAndSessionsRow(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "context-router/test", RetentionSec: 42})

	var startedAt, retention int64
	var producer string
	if err := d.Reader().QueryRow("SELECT started_at, producer, retention_sec FROM sessions WHERE session_id=?", d.SessionID()).
		Scan(&startedAt, &producer, &retention); err != nil {
		t.Fatalf("sessions row: %v", err)
	}
	if producer != "context-router/test" || retention != 42 || startedAt <= 0 {
		t.Fatalf("sessions row = (started_at=%d, producer=%q, retention=%d), want producer=context-router/test retention=42 started_at>0",
			startedAt, producer, retention)
	}

	var count int
	if err := d.Reader().QueryRow("SELECT COUNT(*) FROM session_events WHERE event_type=? AND session_id=?",
		eventTypeSessionStart, d.SessionID()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("session_start events for this session=%d want 1", count)
	}
}

// TestOpen_RecoverMarkerBlocks — 브리프 Step1 ④: 마커 존재 시 ErrRecoverPending, quick_check
// 결과와 무관하게 fail-closed. 실패 후 shared lease가 정상 해제됐는지도 함께 검증한다.
func TestOpen_RecoverMarkerBlocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, recoverMarkerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := Open(dir, Options{Producer: "p"})
	if d != nil {
		t.Fatal("want nil *DB on ErrRecoverPending")
	}
	if !errors.Is(err, ErrRecoverPending) {
		t.Fatalf("err=%v want ErrRecoverPending", err)
	}

	// lease가 해제됐다면 exclusive 잠금을 새로 취득할 수 있어야 한다(leak 없음 검증).
	release, lockErr := store.AcquireLock(filepath.Join(dir, lockFileName), false)
	if lockErr != nil {
		t.Fatalf("session.lock 잔류(leak) 의심: exclusive 재취득 실패: %v", lockErr)
	}
	release()
}

// TestOpen_CorruptHeaderFailsClosed — 브리프 Step1 ⑤ / G8 전반부: DB 파일 헤더 훼손 후
// Open → ErrCorrupt, 원본 파일 바이트는 불변.
func TestOpen_CorruptHeaderFailsClosed(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dir, dbFileName)
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("NOT-A-VALID-SQLITE-HEADER-BYTES!"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	d2, err := Open(dir, Options{Producer: "p"})
	if d2 != nil {
		t.Fatal("want nil *DB on ErrCorrupt")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err=%v want ErrCorrupt", err)
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(before, after) {
		t.Fatal("session.db 바이트가 실패한 Open 시도 중 변경됨 — 원본 불변 위반")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAppend_SupersedesMissingFails — 브리프 Step1 ⑥: supersedes 미존재 → 오류
// (store.ErrNotFound 계열, 신규 sentinel 없음).
func TestAppend_SupersedesMissingFails(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})

	_, _, _, err := d.Append(Event{Type: "note", Summary: "x", Supersedes: "00000000-0000-7000-8000-000000000000"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err=%v want store.ErrNotFound", err)
	}
}

// TestAppend_SupersedesExistingSucceeds — supersedes가 실제 존재하는 event_id면 정상 append.
func TestAppend_SupersedesExistingSucceeds(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})

	_, firstID, _, err := d.Append(Event{Type: "decision", Summary: "first"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = d.Append(Event{Type: "decision", Summary: "corrected", Supersedes: firstID})
	if err != nil {
		t.Fatalf("supersedes existing event_id 실패: %v", err)
	}
}

// TestAppend_EventIDsAreTimeOrdered — 브리프 Step1 ⑦: UUIDv7 연속 발급은 사전순으로 증가한다.
func TestAppend_EventIDsAreTimeOrdered(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})

	const n = 30
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		_, eid, _, err := d.Append(Event{Type: "note", Summary: "x"})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = eid
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("event_id가 사전순 증가가 아님: %v", ids)
	}
	// 엄격 증가(중복 없음)까지 확인
	for i := 1; i < n; i++ {
		if ids[i-1] >= ids[i] {
			t.Fatalf("event_id[%d]=%q >= event_id[%d]=%q — 엄격 증가 위반", i-1, ids[i-1], i, ids[i])
		}
	}
}

// TestAppend_ConcurrentAcrossConnections — G2: 같은 dir을 가리키는 별도 *DB(별도 연결) 2개가
// 각 100건씩 동시 append해도 무손실(총 200건, event_id 전유일)이어야 한다.
func TestAppend_ConcurrentAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	d1 := openT(t, dir, Options{Producer: "host-a"})
	d2 := openT(t, dir, Options{Producer: "host-b"})

	const perConn = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := make(map[string]bool, perConn*2)
	var appendErrs []error

	run := func(d *DB, tag string) {
		defer wg.Done()
		for i := 0; i < perConn; i++ {
			_, eid, _, err := d.Append(Event{Type: "g2_test", Summary: tag})
			mu.Lock()
			if err != nil {
				appendErrs = append(appendErrs, err)
			} else {
				ids[eid] = true
			}
			mu.Unlock()
		}
	}

	wg.Add(2)
	go run(d1, "conn-1")
	go run(d2, "conn-2")
	wg.Wait()

	if len(appendErrs) != 0 {
		t.Fatalf("append 오류 %d건 (첫 번째: %v)", len(appendErrs), appendErrs[0])
	}
	if len(ids) != perConn*2 {
		t.Fatalf("고유 event_id 개수=%d want %d (무손실·전유일 위반)", len(ids), perConn*2)
	}

	var total int
	if err := d1.Reader().QueryRow("SELECT COUNT(*) FROM session_events WHERE event_type='g2_test'").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != perConn*2 {
		t.Fatalf("session_events(g2_test) 행 수=%d want %d", total, perConn*2)
	}
}
