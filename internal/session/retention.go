// retention.go — 설계 §5(D17) 스윕 엔진: 서버 시작 시 1회, per-session 정책 삭제.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Sweep — retention_sec > 0을 표명한 세션의 session_events만, 그 세션 자신의 retention_sec를
// 기준으로 삭제한다(설계 §5 "정책 충돌 방지" — sessions IN 조인). retention_sec = 0(미표명)인
// 세션의 이벤트는 이 함수가 절대 건드리지 않는다(M-4 회귀 방지 — 전역 컷오프 금지). now는
// 호출자가 값으로 주입한다(G7 결정론 — 이 함수는 내부에서 time.Now()를 호출하지 않는다).
//
// 스윕이 교정 이벤트(supersedes 보유 행)를 지워 dangling supersedes가 생기는 것은 허용한다
// (설계 §5) — Summarize/QueryEvents의 superseded 판정은 이미 잔존 행 기준(`event_id NOT IN
// (SELECT supersedes ...)`)이라, 교정 이벤트가 사라지면 원본이 자연히 비superseded로
// 복귀한다. 이 함수는 supersedes를 별도로 정리하지 않는다. DELETE는 session_events_ad
// 트리거가 FTS(fts_ev_porter/fts_ev_trigram) 동기화를 처리하므로 추가 FTS 코드가 불요하다.
func Sweep(ctx context.Context, d *DB, now time.Time) (int64, error) {
	nowUnix := now.Unix()
	var deleted int64
	err := d.txRetry(ctx, func(tx *sql.Tx) error {
		res, execErr := tx.ExecContext(ctx, `DELETE FROM session_events
			WHERE session_id IN (SELECT session_id FROM sessions WHERE retention_sec > 0)
			  AND ts < ? - (SELECT retention_sec FROM sessions s WHERE s.session_id = session_events.session_id)`,
			nowUnix)
		if execErr != nil {
			return execErr
		}
		deleted, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("session Sweep: %w", err)
	}
	return deleted, nil
}
