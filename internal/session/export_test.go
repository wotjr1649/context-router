package session

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// insertRawEvent — G6 결정론 테스트 전용: Append(UUIDv7·now 자동발급)를 우회해 event_id·ts를
// 고정 값으로 직접 삽입한다(같은 패키지 white-box 접근, 브리프 지시 — "테스트 헬퍼로 행을
// 직접 삽입"). 컬럼 순서는 schemaV1과 동일. 반환값은 삽입된 행의 rowid(id).
func insertRawEvent(t *testing.T, d *DB, eventID, sessionID, eventType string, ts int64, summary string,
	payload, artifactRefs, related []byte, redaction, supersedes string,
) int64 {
	t.Helper()
	res, err := d.writer.Exec(`INSERT INTO session_events(event_id, session_id, event_type, ts, summary, payload, artifact_refs, related, redaction, supersedes)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		eventID, sessionID, eventType, ts, summary,
		nullableBytes(payload), nullableBytes(artifactRefs), nullableBytes(related),
		redaction, nullIfEmpty(supersedes))
	if err != nil {
		t.Fatalf("insertRawEvent: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insertRawEvent lastInsertId: %v", err)
	}
	return id
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// lastEventID — 시드 전 현재 최대 rowid(session_start 자동 이벤트를 건너뛰기 위한 after 기준점).
func lastEventID(t *testing.T, d *DB) int64 {
	t.Helper()
	var id int64
	if err := d.Reader().QueryRow("SELECT MAX(id) FROM session_events").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestExport_GoldenAllFields — 브리프 Step1 ①②⑤: 시드 3건(정상 attributes/refs/related,
// supersedes로 교정, redaction=spans, 미지 event_type) → Export가 §26 전 필드를 정확히
// 채운다. schemaVersion·privacyLabel 상수도 전 이벤트에서 확인한다(②).
func TestExport_GoldenAllFields(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "context-router/0.1.0"})
	after := lastEventID(t, d)

	const ts1, ts2, ts3 = int64(1700000001), int64(1700000002), int64(1700000003)
	insertRawEvent(t, d, "evt-001", d.SessionID(), "test_run", ts1, "PostgreSQL integration tests failed",
		[]byte(`{"exitCode":1,"failedTests":3}`),
		[]byte(`["artifact://`+d.SessionID()+`/sha256-output"]`),
		[]byte(`["symbol://csharp/Lib.Db.PostgreSqlProvider"]`),
		"none", "")
	insertRawEvent(t, d, "evt-002", d.SessionID(), "note", ts2, "has a secret token redacted",
		nil, nil, nil, "spans", "evt-001")
	id3 := insertRawEvent(t, d, "evt-003", d.SessionID(), "vendor_custom_widget_ping", ts3, "unknown event type preserved",
		nil, nil, nil, "none", "")

	events, nextAfter, err := Export(context.Background(), d.Reader(), after, "", 10)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("len(events)=%d want 3: %+v", len(events), events)
	}
	if nextAfter != id3 {
		t.Fatalf("nextAfter=%d want %d(last seeded id)", nextAfter, id3)
	}

	for _, ev := range events {
		if ev.SchemaVersion != "1.0" {
			t.Fatalf("ev(%s).SchemaVersion=%q want 1.0", ev.EventID, ev.SchemaVersion)
		}
		if ev.PrivacyLabel != "internal" {
			t.Fatalf("ev(%s).PrivacyLabel=%q want internal", ev.EventID, ev.PrivacyLabel)
		}
		if ev.Producer != (Producer{Name: "context-router", Version: "0.1.0"}) {
			t.Fatalf("ev(%s).Producer=%+v want {context-router 0.1.0}", ev.EventID, ev.Producer)
		}
	}

	e1, e2, e3 := events[0], events[1], events[2]

	if e1.EventID != "evt-001" || e1.SessionID != d.SessionID() || e1.EventType != "test_run" {
		t.Fatalf("e1 identity=%+v", e1)
	}
	wantTS1 := time.Unix(ts1, 0).UTC().Format(time.RFC3339)
	if e1.Timestamp != wantTS1 {
		t.Fatalf("e1.Timestamp=%q want %q", e1.Timestamp, wantTS1)
	}
	if e1.Summary != "PostgreSQL integration tests failed" {
		t.Fatalf("e1.Summary=%q", e1.Summary)
	}
	if len(e1.ArtifactRefs) != 1 || e1.ArtifactRefs[0] != "artifact://"+d.SessionID()+"/sha256-output" {
		t.Fatalf("e1.ArtifactRefs=%v", e1.ArtifactRefs)
	}
	if len(e1.RelatedResources) != 1 || e1.RelatedResources[0] != "symbol://csharp/Lib.Db.PostgreSqlProvider" {
		t.Fatalf("e1.RelatedResources=%v", e1.RelatedResources)
	}
	attrsJSON, err := json.Marshal(e1.Attributes)
	if err != nil {
		t.Fatalf("marshal e1.Attributes: %v", err)
	}
	if string(attrsJSON) != `{"exitCode":1,"failedTests":3}` {
		t.Fatalf("e1.Attributes=%s", attrsJSON)
	}
	if e1.Supersedes != "" {
		t.Fatalf("e1.Supersedes=%q want empty", e1.Supersedes)
	}
	if e1.Redaction != "" {
		t.Fatalf("e1.Redaction=%q want empty(redaction=none omitted)", e1.Redaction)
	}

	if e2.EventID != "evt-002" || e2.EventType != "note" {
		t.Fatalf("e2 identity=%+v", e2)
	}
	if e2.Supersedes != "evt-001" {
		t.Fatalf("e2.Supersedes=%q want evt-001", e2.Supersedes)
	}
	if e2.Redaction != "spans" {
		t.Fatalf("e2.Redaction=%q want spans", e2.Redaction)
	}
	if e2.Attributes != nil {
		t.Fatalf("e2.Attributes=%v want nil", e2.Attributes)
	}
	if len(e2.ArtifactRefs) != 0 || len(e2.RelatedResources) != 0 {
		t.Fatalf("e2 refs/related not empty: %+v", e2)
	}

	// e3: 미지 event_type 보존(§26, 브리프 ⑤).
	if e3.EventID != "evt-003" || e3.EventType != "vendor_custom_widget_ping" {
		t.Fatalf("e3 identity=%+v want event_type=vendor_custom_widget_ping preserved", e3)
	}
	if e3.Redaction != "" || e3.Supersedes != "" {
		t.Fatalf("e3 redaction/supersedes not default: %+v", e3)
	}
}

// TestExport_ProducerDerivedFromSessionsRow — 브리프 Step1 ③: producer.version은
// sessions.producer("context-router/<version>")에서 유도되고, sessions 행이 삭제되면(인양
// 유실 시뮬레이션) "unknown"으로 폴백한다(설계 §3.3). name은 v0.1 상수로 불변.
func TestExport_ProducerDerivedFromSessionsRow(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "context-router/9.9.9"})
	after := lastEventID(t, d)
	insertRawEvent(t, d, "evt-p1", d.SessionID(), "note", 1700000000, "s", nil, nil, nil, "none", "")

	events, _, err := Export(context.Background(), d.Reader(), after, "", 10)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events)=%d want 1", len(events))
	}
	if events[0].Producer != (Producer{Name: "context-router", Version: "9.9.9"}) {
		t.Fatalf("Producer=%+v want {context-router 9.9.9}", events[0].Producer)
	}

	if _, err := d.writer.Exec("DELETE FROM sessions WHERE session_id=?", d.SessionID()); err != nil {
		t.Fatalf("delete sessions row: %v", err)
	}

	events2, _, err := Export(context.Background(), d.Reader(), after, "", 10)
	if err != nil {
		t.Fatalf("Export(after sessions row deleted): %v", err)
	}
	if len(events2) != 1 {
		t.Fatalf("len(events2)=%d want 1", len(events2))
	}
	if events2[0].Producer != (Producer{Name: "context-router", Version: "unknown"}) {
		t.Fatalf("Producer(after delete)=%+v want {context-router unknown}", events2[0].Producer)
	}
}

// TestExport_CursorPagination — 브리프 Step1 ④: limit=1로 3회 순회하면 시드된 이벤트 전부를
// 정확히 한 번씩, 삽입 순서대로 방문하고 next_after가 매 순회 단조 증가한다(§3.3 rowid
// 커서). 4번째 순회는 빈 결과 + next_after 불변(진행 없음).
func TestExport_CursorPagination(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})
	after := lastEventID(t, d)

	want := []string{"evt-a", "evt-b", "evt-c"}
	for i, eid := range want {
		insertRawEvent(t, d, eid, d.SessionID(), "note", int64(1700000000+i), "s", nil, nil, nil, "none", "")
	}

	var got []string
	lastAfter := after
	for i := 0; i < 3; i++ {
		events, next, err := Export(context.Background(), d.Reader(), after, "", 1)
		if err != nil {
			t.Fatalf("Export(iter %d): %v", i, err)
		}
		if len(events) != 1 {
			t.Fatalf("Export(iter %d) len=%d want 1", i, len(events))
		}
		if next <= lastAfter {
			t.Fatalf("Export(iter %d) next_after=%d want > %d(단조 증가)", i, next, lastAfter)
		}
		got = append(got, events[0].EventID)
		after = next
		lastAfter = next
	}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%s want %s(순서/중복 위반)", i, got[i], want[i])
		}
	}

	events, next, err := Export(context.Background(), d.Reader(), after, "", 1)
	if err != nil {
		t.Fatalf("Export(exhausted): %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Export(exhausted) len=%d want 0", len(events))
	}
	if next != after {
		t.Fatalf("Export(exhausted) next_after=%d want unchanged %d", next, after)
	}
}

// TestExport_IncludesSupersededEvents — 브리프 Step1 ⑥: export는 무필터 전체 스트림이라
// superseded된 이벤트도 그대로 포함한다(Summarize와 달리 제외하지 않음, §3.3).
func TestExport_IncludesSupersededEvents(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "p"})
	after := lastEventID(t, d)

	insertRawEvent(t, d, "evt-old", d.SessionID(), "decision", 1700000000, "first take", nil, nil, nil, "none", "")
	insertRawEvent(t, d, "evt-new", d.SessionID(), "decision", 1700000001, "corrected", nil, nil, nil, "none", "evt-old")

	events, _, err := Export(context.Background(), d.Reader(), after, "", 10)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2(superseded event included)", len(events))
	}
	if events[0].EventID != "evt-old" || events[1].EventID != "evt-new" {
		t.Fatalf("events=[%s %s] want [evt-old evt-new]", events[0].EventID, events[1].EventID)
	}
	if events[1].Supersedes != "evt-old" {
		t.Fatalf("events[1].Supersedes=%q want evt-old", events[1].Supersedes)
	}
}

// TestExport_WireJSONShape — §26 계약: EventV1을 실제로 json.Marshal했을 때 키 집합이 정확히
// camelCase 필드와 일치하고, 내부 커서 부기용 RowID(json:"-")는 어떤 형태로도 노출되지 않는다.
func TestExport_WireJSONShape(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "context-router/1.2.3"})
	after := lastEventID(t, d)
	insertRawEvent(t, d, "evt-shape", d.SessionID(), "note", 1700000000, "s",
		[]byte(`{"a":1}`), []byte(`["artifact://x/sha256-a"]`), []byte(`["symbol://y"]`),
		"spans", "evt-prior")

	events, _, err := Export(context.Background(), d.Reader(), after, "", 10)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events)=%d want 1", len(events))
	}

	b, err := json.Marshal(events[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	wantKeys := []string{
		"schemaVersion", "eventId", "sessionId", "eventType", "timestamp",
		"summary", "artifactRefs", "relatedResources", "attributes", "privacyLabel",
		"producer", "supersedes", "redaction",
	}
	if len(m) != len(wantKeys) {
		t.Fatalf("json=%s has %d keys want %d(%v)", b, len(m), len(wantKeys), wantKeys)
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("json=%s missing key %q", b, k)
		}
	}
	for _, forbidden := range []string{"RowID", "rowID", "row_id", "rowid"} {
		if _, ok := m[forbidden]; ok {
			t.Fatalf("json=%s leaked internal cursor field %q", b, forbidden)
		}
	}
}

// TestExport_OmitemptyEmptyFieldsGolden — 부채 ③(설계 §9, §26): 빈 필드 훅 이벤트를 export하면
// omitempty optional 키(artifactRefs·relatedResources·attributes·supersedes·redaction)는 wire에서
// 아예 빠진다 — §26의 "키 상시 존재"를 wire 형식이 강제하지 않으며 소비자는 키 부재를 허용해야
// 한다는 계약을 고정한다(비-omitempty 상시 키만 남는다). TestExport_WireJSONShape(전 필드 채움)의
// 빈 필드 대칭 짝.
func TestExport_OmitemptyEmptyFieldsGolden(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "context-router/0.2.0"})
	after := lastEventID(t, d)
	// 훅 최소 이벤트: attributes/refs/related/supersedes 전부 비고 redaction=none.
	insertRawEvent(t, d, "evt-min", d.SessionID(), "tool_call", 1700000000, "ran ctr_search",
		nil, nil, nil, "none", "")

	events, _, err := Export(context.Background(), d.Reader(), after, "", 10)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events)=%d want 1", len(events))
	}
	b, err := json.Marshal(events[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	wantPresent := []string{"schemaVersion", "eventId", "sessionId", "eventType", "timestamp", "summary", "privacyLabel", "producer"}
	wantAbsent := []string{"artifactRefs", "relatedResources", "attributes", "supersedes", "redaction"}
	if len(m) != len(wantPresent) {
		t.Fatalf("json=%s has %d keys want %d(빈 omitempty 필드 제거)", b, len(m), len(wantPresent))
	}
	for _, k := range wantPresent {
		if _, ok := m[k]; !ok {
			t.Fatalf("json=%s missing 상시 키 %q", b, k)
		}
	}
	for _, k := range wantAbsent {
		if _, ok := m[k]; ok {
			t.Fatalf("json=%s 빈 omitempty 키 %q 존재(§26 상시 존재 요구 아님을 위반)", b, k)
		}
	}
}

// goldenRegenBlocked — UPDATE_GOLDEN 골든 재생성 가드(T10b, session-10 채택): CI에서 골든을
// 재생성하면 방금 쓴 바이트와 비교해 byte-exact 테스트가 무조건 통과 → 회귀 가드가 무력화된다.
// CI(env CI 설정)에서 UPDATE_GOLDEN이 켜지면 재생성을 막는다 — 로컬 재생성만 허용.
func goldenRegenBlocked(updateGolden, ci string) bool {
	return updateGolden != "" && ci != ""
}

// TestGoldenRegenBlocked — 가드 진리표: CI∧UPDATE_GOLDEN만 차단, 나머지 통과.
func TestGoldenRegenBlocked(t *testing.T) {
	for _, c := range []struct {
		upd, ci string
		want    bool
	}{
		{"", "", false},
		{"1", "", false},    // 로컬 재생성 허용
		{"", "true", false}, // CI 평시(재생성 아님) 통과
		{"1", "true", true}, // CI 재생성 차단
	} {
		if got := goldenRegenBlocked(c.upd, c.ci); got != c.want {
			t.Fatalf("goldenRegenBlocked(%q,%q)=%v want %v", c.upd, c.ci, got, c.want)
		}
	}
}

// TestExport_ByteExactGolden — 부채 ④(설계 §9, v0.1 최종리뷰 요구): 고정 cc: 세션·고정
// event_id/ts로 시드한 훅 이벤트(빈 필드 1건 + 전 필드 1건)를 export한 바이트를 testdata 골든과
// 바이트 단위로 비교한다 — 필드 순서·키 이름·타임스탬프 형식·omitempty 드롭의 어떤 표류도 잡는다.
// session_id는 고정 리터럴, producer는 sessions 행으로 고정해 완전 결정적. 골든 재생성:
// `UPDATE_GOLDEN=1 go test -run TestExport_ByteExactGolden ./internal/session/`.
func TestExport_ByteExactGolden(t *testing.T) {
	dir := t.TempDir()
	d := openT(t, dir, Options{Producer: "context-router/0.2.0"})

	const sid = "cc:00000000-0000-7000-8000-0000000000aa"
	if _, err := d.writer.Exec(`INSERT INTO sessions(session_id, started_at, producer, retention_sec) VALUES(?,?,?,?)`,
		sid, int64(1700000000), "context-router/0.2.0", 0); err != nil {
		t.Fatalf("insert sessions row: %v", err)
	}
	insertRawEvent(t, d, "evt-min", sid, "tool_call", 1700000001, "ran ctr_search",
		nil, nil, nil, "none", "")
	insertRawEvent(t, d, "evt-full", sid, "test_run", 1700000002, "3 integration tests failed",
		[]byte(`{"exitCode":1,"failed":3}`),
		[]byte(`["artifact://`+sid+`/sha256-abc123"]`),
		[]byte(`["symbol://csharp/Lib.Db.PostgreSqlProvider"]`),
		"spans", "evt-min")

	// sid 필터로 자동 session_start(실제 UUID 세션)를 제외 → 시드 2건만 결정적으로 반환.
	events, _, err := Export(context.Background(), d.Reader(), 0, sid, 10)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	got, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "export_golden.json")
	update := os.Getenv("UPDATE_GOLDEN")
	if goldenRegenBlocked(update, os.Getenv("CI")) {
		t.Fatal("UPDATE_GOLDEN은 CI에서 금지 — 재생성은 로컬 전용(byte-exact 골든 회귀 가드 무력화 방지)")
	}
	if update != "" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden(%s): %v — 최초 생성은 UPDATE_GOLDEN=1", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("byte-exact golden 불일치:\n got=%q\nwant=%q", got, want)
	}
}

// TestExport_SessionIDFilter — session_id 지정 시 해당 세션 이벤트만(Summarize의 동명 테스트와
// 동형).
func TestExport_SessionIDFilter(t *testing.T) {
	dir := t.TempDir()
	d1 := openT(t, dir, Options{Producer: "test/s1"})
	d2 := openT(t, dir, Options{Producer: "test/s2"})
	after := lastEventID(t, d2) // session_start(d1)+session_start(d2) 이후부터

	insertRawEvent(t, d1, "evt-s1", d1.SessionID(), "note", 1700000000, "from-s1", nil, nil, nil, "none", "")
	insertRawEvent(t, d2, "evt-s2", d2.SessionID(), "note", 1700000001, "from-s2", nil, nil, nil, "none", "")

	events, _, err := Export(context.Background(), d1.Reader(), after, d1.SessionID(), 10)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(events) != 1 || events[0].EventID != "evt-s1" {
		t.Fatalf("events=%+v want exactly [evt-s1]", events)
	}
}
