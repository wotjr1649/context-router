package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

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

// TestOpen_SessionStartWorktreeRoot — D3(fable, 설계 §2.2): session_start payload의
// worktree_root는 Options.WorktreeRoot(사용자 worktree 경로)를 기록하고, 미주입 시 store 내부
// dir로 폴백한다.
func TestOpen_SessionStartWorktreeRoot(t *testing.T) {
	readWorktreeRoot := func(t *testing.T, d *DB) string {
		t.Helper()
		var payload string
		if err := d.Reader().QueryRow("SELECT payload FROM session_events WHERE event_type=? AND session_id=?",
			eventTypeSessionStart, d.SessionID()).Scan(&payload); err != nil {
			t.Fatalf("payload query: %v", err)
		}
		var p struct {
			WorktreeRoot string `json:"worktree_root"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			t.Fatalf("payload unmarshal: %v", err)
		}
		return p.WorktreeRoot
	}

	t.Run("injected", func(t *testing.T) {
		dir := t.TempDir()
		d := openT(t, dir, Options{Producer: "context-router/test", WorktreeRoot: "/user/worktree/root"})
		if got := readWorktreeRoot(t, d); got != "/user/worktree/root" {
			t.Fatalf("worktree_root=%q want /user/worktree/root", got)
		}
	})
	t.Run("fallback_to_dir", func(t *testing.T) {
		dir := t.TempDir()
		d := openT(t, dir, Options{Producer: "context-router/test"})
		if got := readWorktreeRoot(t, d); got != dir {
			t.Fatalf("worktree_root=%q want dir fallback %q", got, dir)
		}
	})
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

// TestOpen_CorruptHeaderFailsClosed — 브리프 Step1 ⑤ / G8: DB 파일 헤더 훼손 후 Open →
// ErrCorrupt, family(session.db + -wal/-shm) 전체 바이트가 불변(설계 §6.2 fail-closed:
// "원본 DB family 일체 불변"). openT 대신 직접 Open/Close — t.Cleanup 이중 Close 방지
// (리뷰 Minor: Close는 DB당 정확히 1회만).
func TestOpen_CorruptHeaderFailsClosed(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{Producer: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dir, dbFileName)
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"

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

	beforeDB := readFileOrNil(t, dbPath)
	beforeWAL, beforeWALErr := os.ReadFile(walPath)
	beforeSHM, beforeSHMErr := os.ReadFile(shmPath)

	d2, err := Open(dir, Options{Producer: "p"})
	if d2 != nil {
		t.Fatal("want nil *DB on ErrCorrupt")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err=%v want ErrCorrupt", err)
	}

	afterDB := readFileOrNil(t, dbPath)
	if !bytesEqual(beforeDB, afterDB) {
		t.Fatal("session.db 바이트가 실패한 Open 시도 중 변경됨 — 원본 불변 위반")
	}
	afterWAL, afterWALErr := os.ReadFile(walPath)
	if (beforeWALErr == nil) != (afterWALErr == nil) || !bytesEqual(beforeWAL, afterWAL) {
		t.Fatal("session.db-wal이 실패한 Open 시도 중 생성/변경됨 — family 불변 위반")
	}
	afterSHM, afterSHMErr := os.ReadFile(shmPath)
	if (beforeSHMErr == nil) != (afterSHMErr == nil) || !bytesEqual(beforeSHM, afterSHM) {
		t.Fatal("session.db-shm이 실패한 Open 시도 중 생성/변경됨 — family 불변 위반")
	}
}

func readFileOrNil(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
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

// TestAppend_RedactionColumn — T3 이관 계약: Event.Redaction이 그대로 행에 기록되고,
// 빈 문자열은 'none'으로 정규화된다(mcp 핸들러가 spans>0일 때만 "spans"를 채운다).
func TestAppend_RedactionColumn(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})

	id1, _, _, err := d.Append(Event{Type: "note", Summary: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _, _, err := d.Append(Event{Type: "note", Summary: "has secret", Redaction: "spans"})
	if err != nil {
		t.Fatal(err)
	}

	var got1, got2 string
	if err := d.Reader().QueryRow("SELECT redaction FROM session_events WHERE id=?", id1).Scan(&got1); err != nil {
		t.Fatal(err)
	}
	if err := d.Reader().QueryRow("SELECT redaction FROM session_events WHERE id=?", id2).Scan(&got2); err != nil {
		t.Fatal(err)
	}
	if got1 != "none" {
		t.Fatalf("redaction(빈 값)=%q want none", got1)
	}
	if got2 != "spans" {
		t.Fatalf("redaction(spans)=%q want spans", got2)
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

// TestOpen_ColdStartInitLockContention — 리뷰 Important 회귀: 두 호스트가 같은 신규(파일
// 없는) worktree를 동시 콜드스타트하면 둘 다 isNew=true로 session.init.lock을 다투는데,
// 예전 코드는 재시도가 없어 패자의 Open() 자체가 실패했다(설계 §6.2 ②는 init.lock을
// "기존 lockStore 패턴" — 대기 후 진행 — 으로 명시). 경합이 우연에 기대지 않도록, 테스트가
// 먼저 session.init.lock을 선점해 두 Open() 고루틴을 강제로 재시도 루프에 들어가게 한 뒤
// 놓아준다 — 그래야 실제 경합 창이 보장된다("형식적 테스트 금지").
func TestOpen_ColdStartInitLockContention(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 두 Open() 모두 session.db가 없는 상태에서 시작하도록, init.lock을 테스트가 먼저 잡아
	// 두 고루틴 모두 acquireInitLock의 재시도 루프에 들어갈 수밖에 없게 만든다.
	release, err := store.AcquireLock(filepath.Join(dir, initLockFileName), false)
	if err != nil {
		t.Fatalf("사전 점유 실패: %v", err)
	}

	type result struct {
		d   *DB
		err error
	}
	results := make(chan result, 2)
	launch := func() {
		d, err := Open(dir, Options{Producer: "p"})
		results <- result{d, err}
	}
	go launch()
	go launch()

	// acquireInitLock의 초기 백오프는 10ms — 50ms면 두 고루틴 모두 최소 1회 이상 실패를
	// 겪고 재시도 대기 중임을 보장하기에 충분하다(store.go lockStore와 동일 백오프 수치).
	time.Sleep(50 * time.Millisecond)
	release()

	r1 := <-results
	r2 := <-results

	if r1.err != nil {
		t.Fatalf("첫 번째 Open 실패: %v", r1.err)
	}
	if r2.err != nil {
		t.Fatalf("두 번째 Open 실패(콜드스타트 경합 패자 구제 실패 — 리뷰 Important 회귀): %v", r2.err)
	}
	defer r1.d.Close()
	defer r2.d.Close()

	if r1.d.SessionID() == "" || r2.d.SessionID() == "" || r1.d.SessionID() == r2.d.SessionID() {
		t.Fatalf("session_id 이상: r1=%q r2=%q (서로 다른 비어있지 않은 값이어야 함)", r1.d.SessionID(), r2.d.SessionID())
	}

	if _, _, _, err := r1.d.Append(Event{Type: "note", Summary: "from d1"}); err != nil {
		t.Fatalf("d1 append: %v", err)
	}
	if _, _, _, err := r2.d.Append(Event{Type: "note", Summary: "from d2"}); err != nil {
		t.Fatalf("d2 append: %v", err)
	}

	dbPath := filepath.Join(dir, dbFileName)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("session.db 없음: %v", err)
	}
}

// --- Summarize (태스크 4, 설계 §3.2) ---

func mustAppend(t *testing.T, d *DB, ev Event) string {
	t.Helper()
	_, eventID, _, err := d.Append(ev)
	if err != nil {
		t.Fatalf("append(%+v): %v", ev, err)
	}
	return eventID
}

func findGroup(sum Summary, eventType string) *EventGroup {
	for i := range sum.Groups {
		if sum.Groups[i].EventType == eventType {
			return &sum.Groups[i]
		}
	}
	return nil
}

// TestSummarize_GroupsByTypeTimeDescendingAcrossSessions — 브리프 Step1 ①: 3세션(별도 Open)
// 분량 시드 후 session_id 무필터 Summarize → 타입별 그룹, 그룹 내 시간(삽입) 역순, 세션 경계를
// 넘어 worktree 전체가 기본 범위(설계 §2.4).
func TestSummarize_GroupsByTypeTimeDescendingAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	d1 := openT(t, dir, Options{Producer: "test/s1"})
	d2 := openT(t, dir, Options{Producer: "test/s2"})
	d3 := openT(t, dir, Options{Producer: "test/s3"})

	e1 := mustAppend(t, d1, Event{Type: "note", Summary: "n1-from-s1", ArtifactRefs: []string{"artifact://x/sha256-abc"}})
	e2 := mustAppend(t, d2, Event{Type: "note", Summary: "n2-from-s2"})
	e3 := mustAppend(t, d3, Event{Type: "note", Summary: "n3-from-s3"})
	dID := mustAppend(t, d1, Event{Type: "decision", Summary: "picked A"})

	sum, err := Summarize(context.Background(), d3.Reader(), "", 5)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Checkpoint != nil {
		t.Fatalf("Checkpoint=%+v want nil (no session_checkpoint seeded)", sum.Checkpoint)
	}

	notes := findGroup(sum, "note")
	if notes == nil || len(notes.Events) != 3 {
		t.Fatalf("note group=%+v want 3 events", notes)
	}
	gotIDs := []string{notes.Events[0].EventID, notes.Events[1].EventID, notes.Events[2].EventID}
	wantIDs := []string{e3, e2, e1} // 삽입 역순(가장 최근 먼저)
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("note group order=%v want %v", gotIDs, wantIDs)
		}
	}
	if len(notes.Events[2].ArtifactRefs) != 1 || notes.Events[2].ArtifactRefs[0] != "artifact://x/sha256-abc" {
		t.Fatalf("e1.ArtifactRefs=%v want [artifact://x/sha256-abc]", notes.Events[2].ArtifactRefs)
	}

	decisions := findGroup(sum, "decision")
	if decisions == nil || len(decisions.Events) != 1 || decisions.Events[0].EventID != dID {
		t.Fatalf("decision group=%+v want 1 event id=%s", decisions, dID)
	}
}

// TestSummarize_SessionIDFilter — 브리프 Step1 ②: session_id 지정 시 해당 세션 이벤트만.
func TestSummarize_SessionIDFilter(t *testing.T) {
	dir := t.TempDir()
	d1 := openT(t, dir, Options{Producer: "test/s1"})
	d2 := openT(t, dir, Options{Producer: "test/s2"})

	mustAppend(t, d1, Event{Type: "note", Summary: "from-s1"})
	mustAppend(t, d2, Event{Type: "note", Summary: "from-s2"})

	sum, err := Summarize(context.Background(), d1.Reader(), d1.SessionID(), 5)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	notes := findGroup(sum, "note")
	if notes == nil || len(notes.Events) != 1 || notes.Events[0].Summary != "from-s1" {
		t.Fatalf("note group=%+v want exactly [from-s1]", notes)
	}
}

// TestSummarize_SupersededExcluded — 브리프 Step1 ③: A를 B가 supersede → A는 그룹에서 제외,
// B만 남는다(idx_ev_sup 활용 질의).
func TestSummarize_SupersededExcluded(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "test/sup"})

	aID := mustAppend(t, d, Event{Type: "decision", Summary: "first take"})
	bID := mustAppend(t, d, Event{Type: "decision", Summary: "corrected", Supersedes: aID})

	sum, err := Summarize(context.Background(), d.Reader(), "", 5)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	decisions := findGroup(sum, "decision")
	if decisions == nil || len(decisions.Events) != 1 || decisions.Events[0].EventID != bID {
		t.Fatalf("decision group=%+v want exactly [%s] (A superseded)", decisions, bID)
	}
}

// TestSummarize_CheckpointSelection — 브리프 Step1 ④ 세션 계층 몫: 최신 비superseded
// session_checkpoint 선정(더 늦게 append됐지만 superseded된 것은 무시) + groups 비중복(선정된
// checkpoint는 자신의 타입 그룹에서 제외되고, 나머지 비superseded checkpoint는 그룹에 남는다).
func TestSummarize_CheckpointSelection(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "test/cp"})

	cpOld := mustAppend(t, d, Event{Type: "session_checkpoint", Summary: "cp-old"})
	cpSuperseded := mustAppend(t, d, Event{Type: "session_checkpoint", Summary: "cp-superseded"})
	cpFinal := mustAppend(t, d, Event{Type: "session_checkpoint", Summary: "cp-final", Supersedes: cpSuperseded})

	sum, err := Summarize(context.Background(), d.Reader(), "", 5)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Checkpoint == nil || sum.Checkpoint.EventID != cpFinal {
		t.Fatalf("Checkpoint=%+v want event_id=%s(cp-final)", sum.Checkpoint, cpFinal)
	}
	grp := findGroup(sum, "session_checkpoint")
	if grp == nil || len(grp.Events) != 1 || grp.Events[0].EventID != cpOld {
		t.Fatalf("session_checkpoint group=%+v want exactly [%s(cp-old)] (cp-final deduped, cp-superseded excluded)", grp, cpOld)
	}
}

// TestSummarize_LimitPerTypeClamps — limitPerType이 그대로 타입별 반환 개수 상한으로
// 작동한다(가장 최근 N개, 클램프 자체는 mcp 소관 — 여기선 값 전달만 검증).
func TestSummarize_LimitPerTypeClamps(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "test/limit"})
	var last string
	for i := 0; i < 7; i++ {
		last = mustAppend(t, d, Event{Type: "note", Summary: "n"})
	}
	sum, err := Summarize(context.Background(), d.Reader(), "", 3)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	notes := findGroup(sum, "note")
	if notes == nil || len(notes.Events) != 3 {
		t.Fatalf("note group len=%v want 3(limitPerType)", notes)
	}
	if notes.Events[0].EventID != last {
		t.Fatalf("note group[0]=%s want most recent %s", notes.Events[0].EventID, last)
	}
}

// matchesFTS: fts에서 token이 하나라도 MATCH되면 true(설계 §2.3 이벤트 FTS 동기화 검증
// 공용 헬퍼 — ①②③ 공유).
func matchesFTS(t *testing.T, d *DB, fts, token string) bool {
	t.Helper()
	var rowid int64
	err := d.reader.QueryRow("SELECT rowid FROM "+fts+" WHERE "+fts+" MATCH ?", token).Scan(&rowid)
	if err == nil {
		return true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	t.Fatalf("%s MATCH %q: %v", fts, token, err)
	return false
}

// TestFTSEvents_SyncOnAppend — 브리프 Step1 ①: append 직후 porter·trigram 양쪽에서 즉시
// 매치되어야 한다(session_events_ai 트리거).
func TestFTSEvents_SyncOnAppend(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})

	mustAppend(t, d, Event{Type: "note", Summary: "hello unique zzyzxsummary token"})

	for _, fts := range []string{"fts_ev_porter", "fts_ev_trigram"} {
		if !matchesFTS(t, d, fts, "zzyzxsummary") {
			t.Fatalf("%s: append 직후 매치 안 됨", fts)
		}
	}
}

// TestFTSEvents_DeleteRemovesFromIndex — 브리프 Step1 ②: retention DELETE 후 양쪽 FTS에서
// 미매치(session_events_ad 트리거, 'delete' 특수 INSERT).
func TestFTSEvents_DeleteRemovesFromIndex(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})

	id := mustAppendID(t, d, Event{Type: "note", Summary: "retentiontoken abcde"})
	if _, err := d.writer.Exec("DELETE FROM session_events WHERE id=?", id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for _, fts := range []string{"fts_ev_porter", "fts_ev_trigram"} {
		if matchesFTS(t, d, fts, "retentiontoken") {
			t.Fatalf("%s: DELETE 후에도 매치됨(트리거 미동작)", fts)
		}
	}
}

// TestFTSEvents_PayloadNotIndexed — 브리프 Step1 ③: payload(attributes)에만 있는 토큰은
// 색인되지 않는다(summary만 색인 — 설계 §2.3).
func TestFTSEvents_PayloadNotIndexed(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})

	attrs, err := json.Marshal(map[string]string{"secret_field": "onlyinpayloadxyz"})
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, d, Event{Type: "note", Summary: "generic summary text", Attributes: attrs})

	for _, fts := range []string{"fts_ev_porter", "fts_ev_trigram"} {
		if matchesFTS(t, d, fts, "onlyinpayloadxyz") {
			t.Fatalf("%s: payload 토큰이 색인됨(summary만 색인해야 함)", fts)
		}
	}
	for _, fts := range []string{"fts_ev_porter", "fts_ev_trigram"} {
		if !matchesFTS(t, d, fts, "generic") {
			t.Fatalf("%s: summary 토큰이 색인되지 않음", fts)
		}
	}
}

// TestFTSEvents_IntegrityCheckPasses — 브리프 Step1 ⑥: fts_ev_porter/fts_ev_trigram
// 양쪽에 FTS5 integrity-check 특수 명령이 통과해야 한다(store.go 선례와 동형).
func TestFTSEvents_IntegrityCheckPasses(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})
	mustAppend(t, d, Event{Type: "note", Summary: "some summary for integrity check"})

	for _, fts := range []string{"fts_ev_porter", "fts_ev_trigram"} {
		if _, err := d.writer.Exec("INSERT INTO " + fts + "(" + fts + ") VALUES('integrity-check')"); err != nil {
			t.Fatalf("%s integrity: %v", fts, err)
		}
	}
}

// mustAppendID — mustAppend와 동형이나 rowid(id)를 반환한다(DELETE 대상 지정용).
func mustAppendID(t *testing.T, d *DB, ev Event) int64 {
	t.Helper()
	id, _, _, err := d.Append(ev)
	if err != nil {
		t.Fatalf("append(%+v): %v", ev, err)
	}
	return id
}

// --- Sweep (태스크 8, 설계 §5/D17) ---

// TestSweep_PerSessionClockInjected — 브리프 Step1 ①②: 세션 A(retention 1h)·세션 B(미표명,
// retention_sec=0) 시드 후 now+2h로 Sweep → A의 이벤트만 삭제되고(Open 자동 session_start 1건
// + 수동 append 1건 = 2건), B는 불가침(G7 시계 주입 + M-4 회귀 — 전역 컷오프로 미표명 세션을
// 뭉개지 않는다).
func TestSweep_PerSessionClockInjected(t *testing.T) {
	dir := t.TempDir()
	dA := openT(t, dir, Options{Producer: "a", RetentionSec: 3600}) // 1h 표명
	dB := openT(t, dir, Options{Producer: "b"})                     // 미표명(0)

	idA := mustAppendID(t, dA, Event{Type: "note", Summary: "a-event"})
	idB := mustAppendID(t, dB, Event{Type: "note", Summary: "b-event"})

	deleted, err := Sweep(context.Background(), dA, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 { // session_start(Open 자동) + a-event, 둘 다 A 소속
		t.Fatalf("deleted=%d want 2(A의 session_start+a-event)", deleted)
	}

	var existsA, existsB int
	if err := dA.Reader().QueryRow("SELECT COUNT(*) FROM session_events WHERE id=?", idA).Scan(&existsA); err != nil {
		t.Fatal(err)
	}
	if existsA != 0 {
		t.Fatal("A(retention 표명) 이벤트가 스윕 후에도 남아있음")
	}
	if err := dA.Reader().QueryRow("SELECT COUNT(*) FROM session_events WHERE id=?", idB).Scan(&existsB); err != nil {
		t.Fatal(err)
	}
	if existsB != 1 {
		t.Fatal("B(미표명) 이벤트가 삭제됨 — M-4 회귀(전역 컷오프 금지)")
	}
}

// TestSweep_AllUndeclaredReturnsZero — 브리프 Step1 ③: 모든 세션이 retention 미표명이면
// 아무리 먼 미래 now로 Sweep해도 삭제 0건.
func TestSweep_AllUndeclaredReturnsZero(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"}) // retention 미표명
	mustAppend(t, d, Event{Type: "note", Summary: "x"})

	deleted, err := Sweep(context.Background(), d, time.Now().Add(999*time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted=%d want 0(모든 세션 미표명 — off와 동치)", deleted)
	}
}

// TestSweep_DanglingSupersedesAllowed — 브리프 Step1 ④: 교정 이벤트(B, A를 supersede)가
// 스윕으로 삭제되면 원래 superseded였던 A가 Summarize에 재등장한다(설계 §5 — dangling
// supersedes 허용, superseded 판정은 잔존 행 기준). B만 오래된 ts로 백데이트해 스윕이 B만
// 지우고 A(최근)는 살리도록 구성한다.
func TestSweep_DanglingSupersedesAllowed(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p", RetentionSec: 3600}) // 1h 표명

	aID := mustAppend(t, d, Event{Type: "decision", Summary: "first take"})
	bRowID, bID, _, err := d.Append(Event{Type: "decision", Summary: "corrected", Supersedes: aID})
	if err != nil {
		t.Fatalf("append B: %v", err)
	}

	sumBefore, err := Summarize(context.Background(), d.Reader(), "", 5)
	if err != nil {
		t.Fatalf("Summarize(before): %v", err)
	}
	decisionsBefore := findGroup(sumBefore, "decision")
	if decisionsBefore == nil || len(decisionsBefore.Events) != 1 || decisionsBefore.Events[0].EventID != bID {
		t.Fatalf("스윕 전 decision group=%+v want 정확히 [%s](B)", decisionsBefore, bID)
	}

	old := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := d.writer.Exec("UPDATE session_events SET ts=? WHERE id=?", old, bRowID); err != nil {
		t.Fatal(err)
	}

	deleted, err := Sweep(context.Background(), d, time.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d want 1(B만)", deleted)
	}

	sumAfter, err := Summarize(context.Background(), d.Reader(), "", 5)
	if err != nil {
		t.Fatalf("Summarize(after): %v", err)
	}
	decisionsAfter := findGroup(sumAfter, "decision")
	if decisionsAfter == nil || len(decisionsAfter.Events) != 1 || decisionsAfter.Events[0].EventID != aID {
		t.Fatalf("스윕 후 decision group=%+v want 정확히 [%s](A, dangling supersedes 복귀)", decisionsAfter, aID)
	}
}
