// retention.go — 설계 §5(D17) 스윕 엔진 + §4(D42) 빈 세션 GC: 서버 시작 시 1회.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SweepReport — Sweep 1회 실행 결과(설계 §4 검수 정정 — 두 종류 삭제는 단일 int64로 보고
// 불가하므로 평범한 값 구조체로 병기; D13 관례상 새 인터페이스가 아니다).
type SweepReport struct {
	EventsDeleted        int64 // §5 retention 삭제 이벤트 수
	EmptySessionsDeleted int64 // §4 빈 세션 GC 세션 수
}

const (
	// emptySessionMaxAgeSec — 빈 세션 GC 나이 게이트(설계 §4): started_at가 now-7d보다 과거인
	// 세션만 대상. 세션 retention_sec 정책과는 무관하다.
	emptySessionMaxAgeSec = 7 * 24 * 3600
	// sweepBatchSessions — 배치당 최대 삭제 행 수(설계 §4 잠금 예산 — 단일 대형 tx 금지).
	// 배치당 BEGIN IMMEDIATE tx 1개, 배치 간 잠금 양보(tx 종료로 자동). retention 이벤트
	// 삭제와 빈 세션 GC가 같은 상수를 공유한다.
	sweepBatchSessions = 64
)

// Sweep — retention_sec > 0을 표명한 세션의 session_events만 그 세션 자신의 retention_sec를
// 기준으로 삭제하고(설계 §5 "정책 충돌 방지" — sessions 조인), 이어서 7일 경과한 빈 세션을
// GC한다(설계 §4). 둘 다 sweepBatchSessions 크기의 배치로 나눠 수행한다(배치당 BEGIN
// IMMEDIATE tx 1개, 배치 간 잠금 양보). retention_sec = 0(미표명) 세션의 이벤트는 이 함수가
// 절대 건드리지 않는다(M-4 회귀 방지 — 전역 컷오프 금지). now는 호출자가 값으로 주입한다
// (G7 결정론 — 이 함수는 내부에서 time.Now()를 호출하지 않는다).
//
// 스윕이 교정 이벤트(supersedes 보유 행)를 지워 dangling supersedes가 생기는 것은 허용한다
// (설계 §5) — Summarize/QueryEvents의 superseded 판정은 이미 잔존 행 기준이라, 교정 이벤트가
// 사라지면 원본이 자연히 비superseded로 복귀한다. DELETE는 session_events_ad 트리거가
// FTS(fts_ev_porter/fts_ev_trigram) 동기화를 처리하므로 추가 FTS 코드가 불요하다.
func Sweep(ctx context.Context, d *DB, now time.Time) (SweepReport, error) {
	nowUnix := now.Unix()
	var rep SweepReport

	// §5 retention: 세션별 retention_sec 기준 만료 이벤트를 배치 삭제(단일 대형 tx 금지).
	for {
		var n int64
		err := d.txRetry(ctx, func(tx *sql.Tx) error {
			res, execErr := tx.ExecContext(ctx, `DELETE FROM session_events WHERE id IN (
				SELECT e.id FROM session_events e
				JOIN sessions s ON s.session_id = e.session_id
				WHERE s.retention_sec > 0 AND e.ts < ? - s.retention_sec
				ORDER BY e.id LIMIT ?)`, nowUnix, sweepBatchSessions)
			if execErr != nil {
				return execErr
			}
			n, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			return rep, fmt.Errorf("session Sweep(retention): %w", err)
		}
		rep.EventsDeleted += n
		if n == 0 {
			break
		}
	}

	// §4 빈 세션 GC: 비-session_start 이벤트 0건 AND started_at < now-7d(retention 무관).
	// 후보 선정·빈 술어 재검증·양 DELETE가 전부 같은 BEGIN IMMEDIATE tx 안에서 실행된다
	// (검수 정정 — tx 밖 후보 선정은 선정↔삭제 사이 커밋된 실이벤트를 오삭제하는 TOCTOU).
	// event-cleanup DELETE와 sessions DELETE의 후보 부분질의는 동일(같은 WHERE 술어 +
	// ORDER BY session_id + LIMIT)이라 한 tx 안에서 결정적으로 같은 배치를 지목한다
	// (session_start 삭제는 빈-술어·정렬을 바꾸지 않음 → orphan 없음).
	cutoff := nowUnix - emptySessionMaxAgeSec
	for {
		var n int64
		err := d.txRetry(ctx, func(tx *sql.Tx) error {
			// 대상 배치 세션의 session_start 이벤트를 먼저 제거(FTS는 AFTER DELETE 트리거 동기).
			if _, execErr := tx.ExecContext(ctx, `DELETE FROM session_events
				WHERE event_type = ? AND session_id IN (
					SELECT s.session_id FROM sessions s
					WHERE s.started_at < ?
					  AND NOT EXISTS(SELECT 1 FROM session_events e
					        WHERE e.session_id = s.session_id AND e.event_type != ?)
					ORDER BY s.session_id LIMIT ?)`,
				eventTypeSessionStart, cutoff, eventTypeSessionStart, sweepBatchSessions); execErr != nil {
				return execErr
			}
			// 빈-술어를 DELETE WHERE에 직접 내장해 같은 배치 세션을 삭제(실이벤트 보유 세션은
			// NOT EXISTS가 걸러 절대 삭제되지 않는다 — barrier).
			res, execErr := tx.ExecContext(ctx, `DELETE FROM sessions WHERE session_id IN (
				SELECT s.session_id FROM sessions s
				WHERE s.started_at < ?
				  AND NOT EXISTS(SELECT 1 FROM session_events e
				        WHERE e.session_id = s.session_id AND e.event_type != ?)
				ORDER BY s.session_id LIMIT ?)`,
				cutoff, eventTypeSessionStart, sweepBatchSessions)
			if execErr != nil {
				return execErr
			}
			n, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			return rep, fmt.Errorf("session Sweep(empty GC): %w", err)
		}
		rep.EmptySessionsDeleted += n
		if n == 0 {
			break
		}
	}

	return rep, nil
}
