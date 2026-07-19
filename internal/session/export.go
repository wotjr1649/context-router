// export.go — ctr_export_events + CLI JSONL(예정) 공용 SessionEvent v1 wire 표현(설계
// §3.3, D16). snake_case 내부 스키마 ↔ camelCase export 매핑은 EventV1 1곳에서만 이뤄진다.
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	schemaVersionV1        = "1.0"            // §26 필수 상수
	privacyLabelInternal   = "internal"       // v0.1 상수(입력 표면 금지, 설계 §3.3)
	producerNameV1         = "context-router" // v0.1 단일 생산자(§26 예시와 동일)
	producerVersionUnknown = "unknown"        // sessions 행 부재(인양 유실 등) 폴백
)

// Producer — EventV1.producer(§26). name은 v0.1 상수, version은 sessions.producer 컬럼
// ("context-router/<version>" 형식, Options.Producer 주석)에서 유도한다.
type Producer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// EventV1 — SessionEvent v1 wire 표현(설계 §3.3, D16 — mcp·CLI 공용, snake↔camel 매핑 1곳).
//
// Attributes는 정적 타입을 any로 둔다(json.RawMessage가 아님) — mcp.AddTool의 출력 스키마
// 추론이 json.RawMessage(=[]byte 기반 named type)를 정수 배열로 오추론해, 실제 값이 JSON
// object(정상 attributes)든 string(redaction 강등 마커)이든 "validating tool output" 오류로
// 거부하는 것을 실측 확인했다(RecordEventInput.Attributes가 입력 측에서 겪은 문제와 동형,
// mcp_session.go 주석 참조). any는 jsonschema-go에서 무제한 스키마({}·"true")를 생성해 실제
// 값을 그대로 통과시킨다. json.Marshal은 any에 담긴 json.RawMessage의 MarshalJSON을 그대로
// 존중하므로 와이어 바이트는 동일하다 — 값이 없으면 필드를 아예 대입하지 않아(nil 유지)
// omitempty가 정상 동작한다(타입 있는 nil RawMessage를 담으면 "null"이 되어버려 대입 자체를
// 생략해야 한다).
type EventV1 struct {
	SchemaVersion    string   `json:"schemaVersion"`
	EventID          string   `json:"eventId"`
	SessionID        string   `json:"sessionId"`
	EventType        string   `json:"eventType"`
	Timestamp        string   `json:"timestamp"`
	Summary          string   `json:"summary"`
	ArtifactRefs     []string `json:"artifactRefs,omitempty"`
	RelatedResources []string `json:"relatedResources,omitempty"`
	Attributes       any      `json:"attributes,omitempty"`
	PrivacyLabel     string   `json:"privacyLabel"`
	Producer         Producer `json:"producer"`
	Supersedes       string   `json:"supersedes,omitempty"`
	Redaction        string   `json:"redaction,omitempty"`

	// RowID — session_events.id(rowid). json:"-"라 wire(§26)에 절대 나타나지 않는다; mcp가
	// max_return_bytes로 이 배열을 잘라낼 때 next_after를 실제로 포함된 마지막 이벤트의
	// rowid로 정확히 되돌리기 위한 내부 커서 부기 전용이다(그렇지 않으면 예산 절단이 뒤쪽
	// 이벤트를 영구 유실시킨다 — 무손실 재구성이 export의 존재 이유).
	RowID int64 `json:"-"`
}

// deriveProducer — 설계 §3.3: name은 v0.1 상수, version은 sessions.producer 값("context-
// router/<version>")의 "/" 뒤 구간. sessions 행이 없거나(인양 유실 등) "/"가 없으면(방어적)
// version은 "unknown".
func deriveProducer(raw sql.NullString) Producer {
	if raw.Valid {
		if _, version, ok := strings.Cut(raw.String, "/"); ok {
			return Producer{Name: producerNameV1, Version: version}
		}
	}
	return Producer{Name: producerNameV1, Version: producerVersionUnknown}
}

// scanEventV1 — Rows.Scan 결과 12컬럼(id, event_id, session_id, event_type, ts, summary,
// payload, artifact_refs, related, redaction, supersedes, producer) → EventV1. artifact_refs/
// related는 저장된 JSON 배열 문자열 그대로 파싱(Append의 marshalStrings와 대칭). redaction은
// "none"(기본값)이면 노출하지 않는다(optional 확장 필드, §3.3).
func scanEventV1(scan func(...any) error) (EventV1, error) {
	var id, ts int64
	var eventID, sessionID, eventType, summary, redaction string
	var payload, artifactRefs, related, supersedes, producer sql.NullString
	if err := scan(&id, &eventID, &sessionID, &eventType, &ts, &summary,
		&payload, &artifactRefs, &related, &redaction, &supersedes, &producer); err != nil {
		return EventV1{}, err
	}

	ev := EventV1{
		SchemaVersion: schemaVersionV1,
		EventID:       eventID,
		SessionID:     sessionID,
		EventType:     eventType,
		Timestamp:     time.Unix(ts, 0).UTC().Format(time.RFC3339),
		Summary:       summary,
		PrivacyLabel:  privacyLabelInternal,
		Producer:      deriveProducer(producer),
		RowID:         id,
	}
	if artifactRefs.Valid && artifactRefs.String != "" {
		if err := json.Unmarshal([]byte(artifactRefs.String), &ev.ArtifactRefs); err != nil {
			return EventV1{}, fmt.Errorf("session Export: artifact_refs 파싱 실패(event_id=%s): %w", eventID, err)
		}
	}
	if related.Valid && related.String != "" {
		if err := json.Unmarshal([]byte(related.String), &ev.RelatedResources); err != nil {
			return EventV1{}, fmt.Errorf("session Export: related 파싱 실패(event_id=%s): %w", eventID, err)
		}
	}
	if payload.Valid && payload.String != "" {
		ev.Attributes = json.RawMessage(payload.String)
	}
	if supersedes.Valid {
		ev.Supersedes = supersedes.String
	}
	if redaction != "none" {
		ev.Redaction = redaction
	}
	return ev, nil
}

// Export — 설계 §3.3/D16: rowid(append 순서) 순, 무필터 전체 스트림(superseded 포함 —
// Summarize와 달리 제외하지 않는다). after=마지막으로 반환된 rowid(0이면 처음부터),
// sessionID=""는 worktree 전체(§2.4 기본 범위). producer는 sessions LEFT JOIN으로 유도한다
// (행이 없으면 deriveProducer가 "unknown"으로 폴백). 반환된 nextAfter는 실제로 반환된 이벤트가
// 없으면 after 그대로(진행 없음 — 커서 안정성).
func Export(ctx context.Context, r *sql.DB, after int64, sessionID string, limit int) ([]EventV1, int64, error) {
	where := "e.id > ?"
	args := []any{after}
	if sessionID != "" {
		where += " AND e.session_id = ?"
		args = append(args, sessionID)
	}
	args = append(args, limit)

	rows, err := r.QueryContext(ctx, `SELECT e.id, e.event_id, e.session_id, e.event_type, e.ts, e.summary,
		e.payload, e.artifact_refs, e.related, e.redaction, e.supersedes, s.producer
		FROM session_events e LEFT JOIN sessions s ON s.session_id = e.session_id
		WHERE `+where+` ORDER BY e.id LIMIT ?`, args...)
	if err != nil {
		return nil, after, fmt.Errorf("session Export: 조회 실패: %w", err)
	}
	defer rows.Close()

	events := []EventV1{}
	nextAfter := after
	for rows.Next() {
		ev, err := scanEventV1(rows.Scan)
		if err != nil {
			return nil, after, fmt.Errorf("session Export: 스캔 실패: %w", err)
		}
		events = append(events, ev)
		nextAfter = ev.RowID
	}
	if err := rows.Err(); err != nil {
		return nil, after, fmt.Errorf("session Export: 순회 실패: %w", err)
	}
	return events, nextAfter, nil
}
