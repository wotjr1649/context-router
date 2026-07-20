// summary.go — ctr_session_summary 조회 함수(설계 §3.2). SQL 질의·그룹핑은 session 패키지가
// 소유한다(mcp는 database/sql을 import하지 않는다, 코드-아키텍처 규약). 예산(max_return_bytes)
// 클램프·절단은 여기 소관이 아니다 — Summarize는 limitPerType(개수 상한)까지만 적용하고, 바이트
// 예산 배분·checkpoint 우선순위·missing 힌트는 mcp 계층(순수 Go 구조체 후처리, applyFetchBudget과
// 동형)이 수행한다.
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// eventTypeSessionCheckpoint — ctr_session_summary의 checkpoint 필드가 선정 대상으로 삼는
// 고정 event_type(설계 §3.2). eventTypeSessionStart와 동일한 관례(고정 어휘 예외).
const eventTypeSessionCheckpoint = "session_checkpoint"

// supersededExclusion — idx_ev_sup(supersedes) 부분 인덱스를 활용하는 superseded 제외 조건
// (설계 §3.2: "event_id NOT IN (SELECT supersedes ... WHERE supersedes IS NOT NULL)").
const supersededExclusion = `event_id NOT IN (SELECT supersedes FROM session_events WHERE supersedes IS NOT NULL)`

// maxSummaryGroups — Summarize가 반환하는 event_type 그룹 수의 하드 상한(설계 §9, Codex P2-4
// 이월). event_type은 CHECK 없는 자유 어휘(§26 미지 타입 보존)라, 노이즈 많은/위조된 훅 스트림이
// 서로 다른 타입을 무한정 만들면 타입당 1개씩의 per-type 질의 fan-out과 응답 그룹 수가 상한
// 없이 커진다. 정상 세션의 구별 타입은 계측 매핑 9종 + 고정 어휘(session_start·
// session_checkpoint) + 모델 주도 record_event 소수 = 대략 10여 개이므로, 32는 정상 범위에 넉넉한
// 여유를 두면서(오탐 절단 회피) 병리적 fan-out만 차단한다. 상한 초과 시 event_type 오름차순
// (queryEventTypes의 결정적 순서) 앞에서 상한까지만 취하고 Summary.GroupsTruncated로 표기한다.
const maxSummaryGroups = 32

// SummaryEvent — Summarize가 반환하는 이벤트 1건(mcp가 wire 타입으로 변환·직렬화한다, D16과
// 동형 — search.Hit/toSearchHit 패턴 승계). ArtifactRefs는 저장된 정본 URI 문자열 그대로다;
// missing 힌트 판정(content.db 조회)은 session 소관이 아니다(session→store 조회만, 설계 §8).
type SummaryEvent struct {
	EventID, SessionID, Summary string
	Ts                          int64
	ArtifactRefs                []string
}

// EventGroup — event_type 1개의 이벤트 목록(이미 limitPerType으로 절단, id 역순 = 시간 역순).
type EventGroup struct {
	EventType string
	Events    []SummaryEvent
}

// Summary — Summarize의 반환값(설계 §3.2). Checkpoint가 nil이면 비superseded
// session_checkpoint 이벤트가 없다는 뜻. GroupsTruncated는 구별 event_type 수가
// maxSummaryGroups를 넘어 그룹 목록을 상한까지 잘랐다는 신호(§9 fan-out 캡).
type Summary struct {
	Checkpoint      *SummaryEvent
	Groups          []EventGroup
	GroupsTruncated bool
}

// Summarize — 설계 §3.2: session_id(빈 문자열이면 worktree 전체, §2.4 기본 범위) 기준으로
// (1) 최신 비superseded session_checkpoint 1건과 (2) 이벤트 타입별 그룹(각 최대
// limitPerType건, id 역순 = 시간 역순, superseded 제외)을 조회한다. checkpoint로 선정된
// 이벤트는 자신의 타입 그룹에서 제외한다("groups에 중복 포함하지 않음"). limitPerType의
// 값 검증(기본 5·최대 20 클램프)은 호출자(mcp) 책임 — 여기선 그대로 LIMIT에 사용한다.
func Summarize(ctx context.Context, r *sql.DB, sessionID string, limitPerType int) (Summary, error) {
	var sum Summary

	ckpt, err := queryCheckpoint(ctx, r, sessionID)
	if err != nil {
		return Summary{}, err
	}
	sum.Checkpoint = ckpt

	types, truncated, err := queryEventTypes(ctx, r, sessionID)
	if err != nil {
		return Summary{}, err
	}
	sum.GroupsTruncated = truncated
	for _, t := range types {
		evs, err := queryEventsByType(ctx, r, sessionID, t, limitPerType)
		if err != nil {
			return Summary{}, err
		}
		if ckpt != nil && t == eventTypeSessionCheckpoint {
			evs = removeByEventID(evs, ckpt.EventID)
		}
		if len(evs) > 0 {
			sum.Groups = append(sum.Groups, EventGroup{EventType: t, Events: evs})
		}
	}
	return sum, nil
}

// queryCheckpoint — 최신(id 역순 1건) 비superseded session_checkpoint. 없으면 (nil, nil).
func queryCheckpoint(ctx context.Context, r *sql.DB, sessionID string) (*SummaryEvent, error) {
	where := "event_type = ? AND " + supersededExclusion
	args := []any{eventTypeSessionCheckpoint}
	if sessionID != "" {
		where += " AND session_id = ?"
		args = append(args, sessionID)
	}
	row := r.QueryRowContext(ctx, `SELECT event_id, session_id, ts, summary, artifact_refs
		FROM session_events WHERE `+where+` ORDER BY id DESC LIMIT 1`, args...)
	ev, err := scanSummaryEvent(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session Summarize: checkpoint 조회 실패: %w", err)
	}
	return &ev, nil
}

// queryEventTypes — superseded 제외 후 남는(=summary에 실제로 등장할 수 있는) event_type의
// 오름차순 목록(그룹 순서 기준 — 결정적 출력). fan-out 캡(§9): maxSummaryGroups+1까지만
// 질의해 초과 여부를 감지하고, 초과 시 상한까지 자른 뒤 truncated=true를 반환한다.
func queryEventTypes(ctx context.Context, r *sql.DB, sessionID string) (types []string, truncated bool, err error) {
	where := supersededExclusion
	args := []any{}
	if sessionID != "" {
		where += " AND session_id = ?"
		args = append(args, sessionID)
	}
	args = append(args, maxSummaryGroups+1) // +1: 상한 초과 감지용 프로브
	rows, err := r.QueryContext(ctx, `SELECT DISTINCT event_type FROM session_events WHERE `+where+` ORDER BY event_type LIMIT ?`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("session Summarize: 타입 목록 조회 실패: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, false, fmt.Errorf("session Summarize: 타입 스캔 실패: %w", err)
		}
		types = append(types, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("session Summarize: 타입 목록 순회 실패: %w", err)
	}
	if len(types) > maxSummaryGroups {
		return types[:maxSummaryGroups], true, nil
	}
	return types, false, nil
}

// queryEventsByType — idx_ev_type(event_type, id) 활용: 타입 1개의 이벤트를 id 역순(=시간
// 역순)으로 최대 limit건.
func queryEventsByType(ctx context.Context, r *sql.DB, sessionID, eventType string, limit int) ([]SummaryEvent, error) {
	where := "event_type = ? AND " + supersededExclusion
	args := []any{eventType}
	if sessionID != "" {
		where += " AND session_id = ?"
		args = append(args, sessionID)
	}
	args = append(args, limit)
	rows, err := r.QueryContext(ctx, `SELECT event_id, session_id, ts, summary, artifact_refs
		FROM session_events WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("session Summarize: 이벤트 조회 실패(type=%s): %w", eventType, err)
	}
	defer rows.Close()
	var evs []SummaryEvent
	for rows.Next() {
		ev, err := scanSummaryEvent(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("session Summarize: 이벤트 스캔 실패(type=%s): %w", eventType, err)
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session Summarize: 이벤트 순회 실패(type=%s): %w", eventType, err)
	}
	return evs, nil
}

// scanSummaryEvent — QueryRow.Scan/Rows.Scan 공용 스캔 헬퍼(컬럼 순서: event_id, session_id,
// ts, summary, artifact_refs). artifact_refs는 NULL이거나 JSON 배열 문자열이다(Append의
// marshalStrings와 대칭).
func scanSummaryEvent(scan func(...any) error) (SummaryEvent, error) {
	var ev SummaryEvent
	var refsCol sql.NullString
	if err := scan(&ev.EventID, &ev.SessionID, &ev.Ts, &ev.Summary, &refsCol); err != nil {
		return SummaryEvent{}, err
	}
	if refsCol.Valid && refsCol.String != "" {
		if err := json.Unmarshal([]byte(refsCol.String), &ev.ArtifactRefs); err != nil {
			return SummaryEvent{}, fmt.Errorf("artifact_refs 파싱 실패: %w", err)
		}
	}
	return ev, nil
}

// removeByEventID — checkpoint로 선정된 이벤트를 자신의 타입 그룹에서 제거한다(그룹에 중복
// 포함하지 않음, 설계 §3.2). limitPerType은 작으므로 선형 탐색으로 충분하다.
func removeByEventID(evs []SummaryEvent, eventID string) []SummaryEvent {
	for i, ev := range evs {
		if ev.EventID == eventID {
			return append(evs[:i:i], evs[i+1:]...)
		}
	}
	return evs
}
