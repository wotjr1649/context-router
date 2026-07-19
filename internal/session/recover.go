// recover.go — CLI `session recover`의 마커·인양·게시 7단계(설계 §6.3, G8). cli 패키지는
// 플래그 해석·프로젝트/worktree 배선만 담당하고, 인양 루프·단계 로직은 여기 소유한다(브리프
// 태스크9b 명문 경계). 각 단계를 별도 함수로 분리한 이유는 두 가지: (1) 설계 §6.3 1~7단계와
// 1:1 대응시켜 리뷰 가능하게, (2) crash-then-resume(G8)을 테스트가 "게시 직전까지만 직접
// 호출→중단"으로 주입할 수 있게(recover_test.go 참고). 원본 family는 rescue/verify 단계 내내
// read-only로만 열리므로(OpenReadOnly) publish(⑥) 전까지 몇 번을 재시도해도 항상 안전 —
// "재개"에 별도의 진행 상태 저장이 필요 없다(단순 재실행 = 안전한 재개).
package session

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wotjr1649/context-router/internal/store"
)

const (
	recoverTmpName = "session.recover-tmp" // 인양본 임시 파일(publish 전 단일 파일, dir 내)
	bakInfix       = ".bak-"               // session.db.bak-<ts> family 접두사(설계 §6.3 ⑥)
	rescueBatch    = 200                   // 인양 루프 배치 크기(튜닝 필요해지면 그때 조정)
	rescueMaxStall = 8                     // 연속 무진전 재시도 상한(구조적 손상이 계속 막을 때 무한루프 방지)
)

// RecoverResult — session recover 실행 결과(설계 §6.3 ⑦ 보고, cli가 stderr 문구를 조립).
type RecoverResult struct {
	NoOp              bool   // 마커 없음 + quick_check 정상 — 손상 아님, 조치 없음
	MarkerOnly        bool   // 마커 존재 + 게시 이미 완료 — 마커만 삭제(N-2 회귀 차단)
	RecoveredEvents   int64  // session_events 인양 행 수
	RecoveredSessions int64  // sessions 인양 행 수
	BackupPrefix      string // 원본이 rename된 session.db.bak-<ts> 파일명(dir 상대, 표시용)
}

// Recover — session recover 서브커맨드 본체(설계 §6.3 1~7단계). dir은 worktree 디렉터리
// (projects/<pid>/worktrees/<wid>) — session.db 존재 확인은 호출자 책임(cli 관례, export와
// 동형).
//
//  1. session.lock exclusive 논블로킹 취득 — 실패(서버 실행 중)는 즉시 거부(대기 없음).
//  2. 마커 없음: fresh quick_check 재확인, 정상이면 NoOp. 마커 있음: 잔여 상태(bak 존재·현재
//     DB 건강) 검증, 이미 게시 완료면 마커만 삭제하고 MarkerOnly.
//  3. 마커 생성 + fsync.
//  4. rowid 보존 인양(session_events·sessions, 손상 구간 재시도 루프).
//  5. 인양본 wal_checkpoint(TRUNCATE)+연결 종료로 단일 파일화 + quick_check·user_version 검증.
//  6. 게시: 원본 family(-shm→-wal→db 순) → session.db.bak-<ts>, 인양본 → session.db, 디렉터리
//     fsync.
//  7. 마커 삭제 + 결과 반환(cli가 stderr로 보고).
func Recover(dir string) (RecoverResult, error) {
	release, lockErr := store.AcquireLock(filepath.Join(dir, lockFileName), false)
	if lockErr != nil {
		return RecoverResult{}, fmt.Errorf("session recover: session.lock exclusive 획득 실패(서버 실행 중일 수 있음): %w: %v", ErrLeaseHeld, lockErr)
	}
	defer release()

	markerPath := filepath.Join(dir, recoverMarkerName)
	markerExists, err := fileExists(markerPath)
	if err != nil {
		return RecoverResult{}, err
	}

	if !markerExists {
		corrupt, err := probeCorrupt(dir)
		if err != nil {
			return RecoverResult{}, err
		}
		if !corrupt {
			return RecoverResult{NoOp: true}, nil
		}
		if err := createMarker(markerPath); err != nil {
			return RecoverResult{}, err
		}
	} else {
		done, err := publishAlreadyComplete(dir)
		if err != nil {
			return RecoverResult{}, err
		}
		if done {
			if rmErr := os.Remove(markerPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return RecoverResult{}, sanitizeIOErr("recover marker remove", rmErr)
			}
			return RecoverResult{MarkerOnly: true}, nil
		}
		// 마커는 있으나 게시 미완료 — ④부터 이어서 진행(단순 재실행, 위 패키지 주석 참고).
	}

	nEvents, nSessions, err := rescueAll(dir)
	if err != nil {
		return RecoverResult{}, err
	}
	if err := verifyRescued(dir); err != nil {
		return RecoverResult{}, err
	}
	backupPrefix, err := publishRescued(dir)
	if err != nil {
		return RecoverResult{}, err
	}
	if err := os.Remove(markerPath); err != nil {
		return RecoverResult{}, sanitizeIOErr("recover marker remove", err)
	}
	return RecoverResult{RecoveredEvents: nEvents, RecoveredSessions: nSessions, BackupPrefix: backupPrefix}, nil
}

func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, sanitizeIOErr("recover stat", err)
	}
}

// probeCorrupt — fresh read-only 연결로 quick_check(설계 §6.2 보수 판정 재사용). session.db가
// 아직 없으면 손상 아님(재생성은 session.Open 몫, recover 소관 아님).
func probeCorrupt(dir string) (bool, error) {
	exists, err := fileExists(filepath.Join(dir, dbFileName))
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	reader, err := OpenReadOnly(dir)
	if err != nil {
		return false, fmt.Errorf("session recover: %w", err)
	}
	defer func() { _ = reader.Close() }()
	if err := quickCheck(reader); err != nil {
		if errors.Is(err, ErrCorrupt) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// publishAlreadyComplete — 마커 존재 상태의 잔여 검증(설계 §6.3 ②, N-2 회귀): 현재 session.db가
// 건강하고(quick_check ok) .bak-<ts> family가 하나 이상 있으면 게시가 이미 끝난 것으로 판단한다
// — session.Open은 마커 존재 시 quick_check 결과와 무관하게 거부하므로(§6.2), 마커가 남아있는
// 동안 session.db가 건강해질 수 있는 유일한 경로는 recover 자신의 게시뿐이다.
func publishAlreadyComplete(dir string) (bool, error) {
	exists, err := fileExists(filepath.Join(dir, dbFileName))
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	corrupt, err := probeCorrupt(dir)
	if err != nil {
		return false, err
	}
	if corrupt {
		return false, nil
	}
	return hasBackupFamily(dir)
}

func hasBackupFamily(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, sanitizeIOErr("recover dir read", err)
	}
	prefix := dbFileName + bakInfix
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			return true, nil
		}
	}
	return false, nil
}

// createMarker — session.recover-pending 생성 + fsync(설계 §6.3 ③). 이미 존재해도 그대로
// 통과한다(멱등 — 재실행 경로에서 재호출돼도 안전, 내용은 의미 없는 sentinel).
func createMarker(markerPath string) error {
	f, err := os.OpenFile(markerPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return sanitizeIOErr("recover marker create", err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		return sanitizeIOErr("recover marker create", errors.Join(syncErr, closeErr))
	}
	return syncDir(filepath.Dir(markerPath))
}

// removeDBFamily — path(+ -wal + -shm)를 삭제한다(없으면 무시). rescueAll이 이전 시도의
// 인양본 잔재를 지우고 항상 처음부터 다시 인양할 때 쓴다.
func removeDBFamily(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return sanitizeIOErr("recover tmp cleanup", err)
		}
	}
	return nil
}

// rescueAll — 설계 §6.3 ④: 원본을 그대로 둔 채(read-only) 임시 인양본(recoverTmpName)에
// 스키마를 적용하고 rowid 보존 복사한다. 재시도는 항상 처음부터(원본이 불변이라 안전, 패키지
// 주석 참고) — 이전 시도의 인양본 잔재를 먼저 지운다.
func rescueAll(dir string) (events int64, sessions int64, err error) {
	tmpPath := filepath.Join(dir, recoverTmpName)
	if err := removeDBFamily(tmpPath); err != nil {
		return 0, 0, err
	}

	src, err := OpenReadOnly(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("session recover: 원본 열기 실패: %w", err)
	}
	defer func() { _ = src.Close() }()

	dsn := "file:" + filepath.ToSlash(tmpPath) + pragmas + "&_txlock=immediate"
	dst, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, 0, fmt.Errorf("session recover: 인양본 열기 실패: %w", err)
	}
	dst.SetMaxOpenConns(1)
	defer func() { _ = dst.Close() }()

	if err := applyTmpSchema(dst); err != nil {
		return 0, 0, err
	}

	sessions, err = rescueSessions(src, dst)
	if err != nil {
		return 0, 0, err
	}
	events, err = rescueSessionEvents(src, dst)
	if err != nil {
		return 0, 0, err
	}
	return events, sessions, nil
}

// applyTmpSchema — store.go/session.go의 applySchemaV1과 동형(단일 트랜잭션, 멱등 DDL).
func applyTmpSchema(dst *sql.DB) error {
	tx, err := dst.Begin()
	if err != nil {
		return fmt.Errorf("session recover: 인양본 스키마 시작 실패: %w", err)
	}
	if _, err := tx.Exec(schemaV1); err != nil {
		_ = tx.Rollback() // 커밋 전이라 무해
		return fmt.Errorf("session recover: 인양본 스키마 적용 실패: %w", err)
	}
	return tx.Commit()
}

// rescueTable — 손상 감내 rowid 보존 복사 공용 루프(session_events·sessions 공용, 설계 §6.3
// ④). queryBatch(cursor)는 매 시도마다 "WHERE rowid-등가컬럼 > cursor ORDER BY ... LIMIT n"
// 형태의 새 질의를 연다 — 실측 확인: 진행 중이던 Rows를 계속 읽다가 SQLITE_CORRUPT를 만나면
// 그 지점에서 완전히 막히지만, 같은 커서로 "새로" 연 질의는 손상된 구간을 조용히 건너뛰고
// 그 뒤에 남은 행을 반환한다(SQLite 옵티마이저가 WHERE 계획에서는 순수 순방향 커서 스캔과
// 다른 탐색 경로를 타는 것으로 관찰됨) — 그래서 "마지막 성공 이후 구간을 건너뛰며 재개"가
// 별도의 확률 탐색 없이 "커서를 유지한 채 재질의"만으로 성립한다. copyRow(rows)는 현재 행을
// 스캔+삽입하고 다음 커서 값을 반환한다. rescueMaxStall번 연속 무진전(오류만 있고 새 행 0건)
// 이면 지금까지 인양분으로 종료한다(구조적으로 더 못 건너뛰는 손상 — 유실 인정, §6.3 ⑦).
func rescueTable(queryBatch func(cursor int64) (*sql.Rows, error), copyRow func(rows *sql.Rows) (nextCursor int64, err error)) (int64, error) {
	var total int64
	cursor := int64(0)
	stall := 0
	for {
		rows, qErr := queryBatch(cursor)
		if qErr != nil {
			if !isMalformed(qErr) {
				return total, qErr
			}
			if stall++; stall > rescueMaxStall {
				return total, nil
			}
			continue
		}

		got := 0
		var scanErr error
		for rows.Next() {
			var next int64
			if next, scanErr = copyRow(rows); scanErr != nil {
				break
			}
			cursor = next
			got++
			total++
		}
		if scanErr == nil {
			scanErr = rows.Err()
		}
		_ = rows.Close()

		switch {
		case got == 0 && scanErr == nil:
			return total, nil // 정상 종료 — 더 읽을 행 없음
		case got == 0 && scanErr != nil:
			if !isMalformed(scanErr) {
				return total, scanErr
			}
			if stall++; stall > rescueMaxStall {
				return total, nil
			}
		default:
			stall = 0 // 이번 배치에서 진전이 있었음(scanErr가 있어도 부분 성공)
		}
	}
}

func nullableAny(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func rescueSessionEvents(src, dst *sql.DB) (int64, error) {
	return rescueTable(
		func(cursor int64) (*sql.Rows, error) {
			return src.Query(`SELECT id, event_id, session_id, event_type, ts, summary, payload, artifact_refs, related, redaction, supersedes
				FROM session_events WHERE id > ? ORDER BY id LIMIT ?`, cursor, rescueBatch)
		},
		func(rows *sql.Rows) (int64, error) {
			var id, ts int64
			var eventID, sessionID, eventType, summary, redaction string
			var payload, artifactRefs, related, supersedes sql.NullString
			if err := rows.Scan(&id, &eventID, &sessionID, &eventType, &ts, &summary, &payload, &artifactRefs, &related, &redaction, &supersedes); err != nil {
				return 0, err
			}
			_, err := dst.Exec(`INSERT INTO session_events(id, event_id, session_id, event_type, ts, summary, payload, artifact_refs, related, redaction, supersedes)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				id, eventID, sessionID, eventType, ts, summary, nullableAny(payload), nullableAny(artifactRefs), nullableAny(related), redaction, nullableAny(supersedes))
			if err != nil {
				return 0, fmt.Errorf("session recover: session_events 인양 삽입 실패: %w", err)
			}
			return id, nil
		},
	)
}

func rescueSessions(src, dst *sql.DB) (int64, error) {
	return rescueTable(
		func(cursor int64) (*sql.Rows, error) {
			return src.Query(`SELECT rowid, session_id, started_at, producer, retention_sec FROM sessions WHERE rowid > ? ORDER BY rowid LIMIT ?`, cursor, rescueBatch)
		},
		func(rows *sql.Rows) (int64, error) {
			var rowid, startedAt, retention int64
			var sessionID, producer string
			if err := rows.Scan(&rowid, &sessionID, &startedAt, &producer, &retention); err != nil {
				return 0, err
			}
			_, err := dst.Exec(`INSERT INTO sessions(session_id, started_at, producer, retention_sec) VALUES(?,?,?,?)`,
				sessionID, startedAt, producer, retention)
			if err != nil {
				return 0, fmt.Errorf("session recover: sessions 인양 삽입 실패: %w", err)
			}
			return rowid, nil
		},
	)
}

// verifyRescued — 설계 §6.3 ⑤: 인양본을 wal_checkpoint(TRUNCATE)+연결 종료로 단일 파일로 접은
// 뒤 quick_check + user_version(스키마) 확인. 체크포인트+정상 종료 후 -wal/-shm이 남지 않는
// 것을 실측 확인했지만(Windows), 크로스플랫폼 방어로 명시 삭제도 병행한다.
func verifyRescued(dir string) error {
	tmpPath := filepath.Join(dir, recoverTmpName)
	dsn := "file:" + filepath.ToSlash(tmpPath) + pragmas + "&_txlock=immediate"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("session recover: 인양본 검증 열기 실패: %w", err)
	}
	if _, err := conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = conn.Close()
		return fmt.Errorf("session recover: 인양본 checkpoint 실패: %w", err)
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("session recover: 인양본 닫기 실패: %w", err)
	}
	if err := os.Remove(tmpPath + "-wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return sanitizeIOErr("recover tmp wal cleanup", err)
	}
	if err := os.Remove(tmpPath + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return sanitizeIOErr("recover tmp shm cleanup", err)
	}

	vconn, err := openReadOnlyAt(tmpPath)
	if err != nil {
		return fmt.Errorf("session recover: 인양본 재확인 열기 실패: %w", err)
	}
	defer func() { _ = vconn.Close() }()

	if err := quickCheck(vconn); err != nil {
		return fmt.Errorf("session recover: 인양본 quick_check 실패: %w", err)
	}
	var uv int
	if err := vconn.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		return fmt.Errorf("session recover: 인양본 user_version 확인 실패: %w", err)
	}
	if uv != schemaVersion {
		return fmt.Errorf("session recover: 인양본 user_version=%d 기대값=%d 불일치", uv, schemaVersion)
	}
	return nil
}

// publishRescued — 설계 §6.3 ⑥: 원본 family(-shm→-wal→session.db 순)를 session.db.bak-<ts>
// family로 rename, 인양본(단일 파일)→session.db, 디렉터리 fsync. 반환값은 부여된 백업 파일명
// (dir 상대, 표시용).
func publishRescued(dir string) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	backupName := dbFileName + bakInfix + ts
	dbPath := filepath.Join(dir, dbFileName)
	tmpPath := filepath.Join(dir, recoverTmpName)

	for _, suffix := range []string{"-shm", "-wal", ""} {
		src := dbPath + suffix
		exists, err := fileExists(src)
		if err != nil {
			return "", err
		}
		if !exists {
			continue // 클린 종료 후엔 -wal/-shm이 없을 수 있다 — 정상
		}
		dst := filepath.Join(dir, backupName+suffix)
		if err := os.Rename(src, dst); err != nil {
			return "", sanitizeIOErr("recover publish backup rename", err)
		}
	}

	if err := os.Rename(tmpPath, dbPath); err != nil {
		return "", sanitizeIOErr("recover publish rename", err)
	}

	if err := syncDir(dir); err != nil {
		return "", err
	}
	return backupName, nil
}

// syncDir — 디렉터리 fsync(설계 §6.3 ⑥, publish 원자성 보강 — defense-in-depth일 뿐 정확성의
// 필요조건은 아니다: rename 자체는 이미 파일시스템 저널에 의해 원자적이다). Windows는 디렉터리
// 핸들 Sync가 일반적으로 지원되지 않으므로 그 경우만 무해 처리한다(다른 OS의 실패는 그대로
// 전파).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return sanitizeIOErr("recover dir sync open", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return sanitizeIOErr("recover dir sync", err)
	}
	return nil
}
