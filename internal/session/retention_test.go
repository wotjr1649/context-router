package session

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// seedSession — 테스트 전용: sessions 행을 직접 삽입한다(export_test.go:390 방식). started_at·
// retention_sec를 결정적으로 고정해 GC 경계를 시드한다.
func seedSession(t *testing.T, d *DB, sessionID string, startedAt, retentionSec int64) {
	t.Helper()
	if _, err := d.writer.Exec(`INSERT INTO sessions(session_id, started_at, producer, retention_sec) VALUES(?,?,?,?)`,
		sessionID, startedAt, "test", retentionSec); err != nil {
		t.Fatalf("seedSession(%s): %v", sessionID, err)
	}
}

// seedSessionStart — 테스트 전용: session_start 이벤트 1건을 직접 삽입한다. event_id는 sid+ts로
// 유일하게 조립한다(schemaV1 UNIQUE 제약 충족).
func seedSessionStart(t *testing.T, d *DB, sessionID string, ts int64) {
	t.Helper()
	insertRawEvent(t, d, fmt.Sprintf("ss-%s-%d", sessionID, ts), sessionID, eventTypeSessionStart, ts, "session start", nil, nil, nil, "none", "")
}

// querySessionIDs — 시드한 cc: 네임스페이스의 잔존 세션 id만 정렬해 반환한다(slices.Equal 비교용).
// openT가 자동 생성하는 세션은 UUIDv7 id·실시간 started_at·session_start 뿐인 빈 세션이라, 실제
// 시각이 GC 컷오프를 지나면 old→fresh로 뒤집혀 보존/삭제가 바뀐다. 'cc:%' 필터로 그 자동 세션을
// 배제해 시드 세션의 GC 경계만 시계 독립적으로 비교한다.
func querySessionIDs(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.Reader().Query("SELECT session_id FROM sessions WHERE session_id LIKE 'cc:%' ORDER BY session_id")
	if err != nil {
		t.Fatalf("querySessionIDs: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("querySessionIDs scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("querySessionIDs rows: %v", err)
	}
	return ids
}

// countEventsFor — 세션 소속 이벤트 수(session_start 포함).
func countEventsFor(t *testing.T, d *DB, sessionID string) int64 {
	t.Helper()
	var n int64
	if err := d.Reader().QueryRow("SELECT COUNT(*) FROM session_events WHERE session_id=?", sessionID).Scan(&n); err != nil {
		t.Fatalf("countEventsFor(%s): %v", sessionID, err)
	}
	return n
}

// countSessionsLike — session_id가 패턴에 매칭되는 잔존 세션 수(배치 GC 검증용).
func countSessionsLike(t *testing.T, d *DB, pattern string) int64 {
	t.Helper()
	var n int64
	if err := d.Reader().QueryRow("SELECT COUNT(*) FROM sessions WHERE session_id LIKE ?", pattern).Scan(&n); err != nil {
		t.Fatalf("countSessionsLike(%s): %v", pattern, err)
	}
	return n
}

// TestSweepEmptySessionGC — D42 §4: 빈 세션 GC 경계. 비-session_start 0건 AND started_at <
// now-7d인 세션만 GC(retention_sec 무관). openT의 자동 세션(오래된 실시간 started_at + session_start
// 뿐)도 같은 술어로 GC되어 잔존 목록에 남지 않는다.
func TestSweepEmptySessionGC(t *testing.T) {
	d := openT(t, t.TempDir(), Options{Producer: "test"})
	now := time.Unix(1800000000, 0)
	old := now.Add(-8 * 24 * time.Hour).Unix()   // 7일 경과 → GC
	fresh := now.Add(-6 * 24 * time.Hour).Unix() // 미경과 → 보존

	seedSession(t, d, "cc:aaa-old-empty-r0", old, 0)         // retention 0 무관 GC
	seedSession(t, d, "cc:bbb-old-empty-r30d", old, 2592000) // retention 30d 무관 GC
	seedSessionStart(t, d, "cc:aaa-old-empty-r0", old)       // session_start만
	seedSessionStart(t, d, "cc:bbb-old-empty-r30d", old)
	seedSessionStart(t, d, "cc:bbb-old-empty-r30d", old+1) // session_start 복수도 빈 세션
	seedSession(t, d, "cc:ccc-fresh-empty", fresh, 0)      // 미경과 보존
	seedSessionStart(t, d, "cc:ccc-fresh-empty", fresh)
	seedSession(t, d, "cc:ddd-old-realevt", old, 0) // 실이벤트 보유 → 보존
	seedSessionStart(t, d, "cc:ddd-old-realevt", old)
	insertRawEvent(t, d, "e1", "cc:ddd-old-realevt", "tool_call", old+1, "x", nil, nil, nil, "none", "")

	if _, err := Sweep(context.Background(), d, now); err != nil {
		t.Fatal(err)
	}
	remain := querySessionIDs(t, d)
	want := []string{"cc:ccc-fresh-empty", "cc:ddd-old-realevt"}
	if !slices.Equal(remain, want) {
		t.Fatalf("잔존 세션=%v want %v", remain, want)
	}
	// GC된 세션의 이벤트도 소멸(FTS는 AFTER DELETE 트리거 동기 — 설계 §4).
	if n := countEventsFor(t, d, "cc:aaa-old-empty-r0"); n != 0 {
		t.Fatalf("GC 세션 이벤트 잔존 %d", n)
	}
}

// TestSweepEmptySessionGCBatch — 배치 상수 초과(65개) 시 복수 트랜잭션으로 전량 GC됨을 단정
// (분할이 결과를 바꾸지 않음). 65 > sweepBatchSessions(64)이라 한 tx의 LIMIT 64로는 다 못
// 지우므로, 전량 소멸은 최소 2배치가 커밋됐음을 의미한다.
func TestSweepEmptySessionGCBatch(t *testing.T) {
	d := openT(t, t.TempDir(), Options{Producer: "test"})
	now := time.Unix(1800000000, 0)
	old := now.Add(-8 * 24 * time.Hour).Unix()

	const total = sweepBatchSessions + 1 // 65
	for i := 0; i < total; i++ {
		sid := fmt.Sprintf("cc:batch-%03d", i)
		seedSession(t, d, sid, old, 0)
		seedSessionStart(t, d, sid, old)
	}

	rep, err := Sweep(context.Background(), d, now)
	if err != nil {
		t.Fatal(err)
	}
	if n := countSessionsLike(t, d, "cc:batch-%"); n != 0 {
		t.Fatalf("배치 GC 후 잔존 %d개 (전량 GC 실패 — 배치 루프 종료 안 됨?)", n)
	}
	if rep.EmptySessionsDeleted < total {
		t.Fatalf("EmptySessionsDeleted=%d want >= %d", rep.EmptySessionsDeleted, total)
	}
}

// TestSweepEmptySessionGCPreservesLateEvent — barrier(검수 요구): 후보로 보일 오래된 세션에
// Sweep 직전 실이벤트를 커밋하면 그 세션·이벤트가 보존된다(빈-술어가 후보 선정·DELETE와 같은
// tx 안에서 평가됨을 고정 — tx 밖 선정이었다면 오삭제될 TOCTOU를 봉쇄).
func TestSweepEmptySessionGCPreservesLateEvent(t *testing.T) {
	d := openT(t, t.TempDir(), Options{Producer: "test"})
	now := time.Unix(1800000000, 0)
	old := now.Add(-8 * 24 * time.Hour).Unix()

	seedSession(t, d, "cc:late", old, 0)
	seedSessionStart(t, d, "cc:late", old)
	insertRawEvent(t, d, "late-evt", "cc:late", "tool_call", old+1, "committed before sweep", nil, nil, nil, "none", "")

	if _, err := Sweep(context.Background(), d, now); err != nil {
		t.Fatal(err)
	}
	if got := querySessionIDs(t, d); !slices.Contains(got, "cc:late") {
		t.Fatalf("실이벤트 보유 세션이 GC됨(barrier 실패): 잔존=%v", got)
	}
	if n := countEventsFor(t, d, "cc:late"); n != 2 { // session_start + late-evt
		t.Fatalf("보존 세션 이벤트=%d want 2(session_start+실이벤트)", n)
	}
}
