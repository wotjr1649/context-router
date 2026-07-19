// recover.go — CLI `session recover`의 마커·인양·게시 7단계(설계 §6.3, G8). cli 패키지는
// 플래그 해석·프로젝트/worktree 배선만 담당하고, 인양 루프·단계 로직은 여기 소유한다(브리프
// 태스크9b 명문 경계). 각 단계를 별도 함수로 분리한 이유는 두 가지: (1) 설계 §6.3 1~7단계와
// 1:1 대응시켜 리뷰 가능하게, (2) crash-then-resume(G8)을 테스트가 "게시 직전까지만 직접
// 호출→중단"으로 주입할 수 있게(recover_test.go 참고). 원본 family는 rescue/verify 단계 내내
// read-only로만 열리므로(OpenReadOnly) publish(⑥) 전까지 몇 번을 재시도해도 항상 안전.
// "재개"는 진행 상태를 별도로 저장하지 않고 재실행 시점의 잔여 상태(마커·인양본·백업)를
// 감지해 이어간다 — 단, 재리뷰(A2) 이후 "재실행=처음부터 재인양"이 아니라 **검증 완료된
// 인양본(tmp)이 남아 있으면 그것을 최우선으로 게시**한다: 마커 하에서 원본 DB는 §6.2상 서버가
// fail-closed라 read-only로만 열리므로 verifyRescued를 통과한 tmp가 항상 최신·완전하고, 원본에서
// 재인양하면 backupOriginal이 이미 분리해 간 원본 -wal 꼬리 커밋을 잃는다.
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

		// 재리뷰(A2): 검증 완료된 인양본(tmp) 건강을 dbExists와 무관하게 최우선 판정한다.
		// tmp가 건강하면(verifyRescued 통과 시점 그대로) 재인양 없이 게시만 마저 끝낸다 —
		// 그렇지 않고 아래 공용 파이프라인으로 떨어지면, backupOriginal이 원본 -wal/-shm만
		// 옮기고 main rename 전 crash한 경우(마커+db 잔존+건강 tmp) rescueAll이 건강 tmp를
		// 폐기하고 -wal 없는 원본을 재인양해 WAL 꼬리 커밋을 조용히 잃는다(재리뷰 Important).
		// 마커 하에서 DB는 §6.2상 서버 fail-closed로 read-only로만 열리므로 건강 tmp가 항상
		// 최신·완전하다는 것이 이 우선순위의 근거다.
		healthy, err := tmpIsHealthy(dir)
		if err != nil {
			return RecoverResult{}, err
		}
		if healthy {
			nEvents, nSessions, backupPrefix, err := resumePublishFromTmp(dir)
			if err != nil {
				return RecoverResult{}, err
			}
			if err := os.Remove(markerPath); err != nil {
				return RecoverResult{}, sanitizeIOErr("recover marker remove", err)
			}
			return RecoverResult{RecoveredEvents: nEvents, RecoveredSessions: nSessions, BackupPrefix: backupPrefix}, nil
		}

		// tmp가 없거나 불건강. session.db가 없으면(게시 rename 도중 crash 이후 tmp까지 유실)
		// 가장 최근 백업을 session.db 자리로 되돌려(restoreLatestBackup) 아래 공용 파이프라인이
		// 처음부터 다시 인양·게시하게 한다. db가 남아 있으면 그대로 원본에서 재인양한다.
		dbExists, err := fileExists(filepath.Join(dir, dbFileName))
		if err != nil {
			return RecoverResult{}, err
		}
		if !dbExists {
			if err := restoreLatestBackup(dir); err != nil {
				return RecoverResult{}, err
			}
		}
		// 마커는 있으나 게시 미완료 — ④부터 이어서 진행(위 패키지 주석 참고).
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
// 건강하고(strict quick_check ok) .bak-<ts> **main** 멤버가 하나 이상 있으면 게시가 이미 끝난
// 것으로 판단한다 — session.Open은 마커 존재 시 quick_check 결과와 무관하게 거부하므로(§6.2),
// 마커가 남아있는 동안 session.db가 건강해질 수 있는 유일한 경로는 recover 자신의 게시뿐이다.
//
// A4(strict): 건강 판정은 probeHealthyStrict — 미확정(BUSY/일시 오류 재시도 소진)을 건강으로
// 오인하면 부분 게시 상태를 "완료"로 선언해 마커를 조기 삭제하므로, "명시적 ok"만 healthy로
// 취급한다. A3②(Codex P1): sidecar 고아(-wal/-shm만)로는 family 성립을 인정하지 않고 백업
// main 파일 실재(latestBackupMain)를 요건으로 요구한다 — 그렇지 않으면 부분 이동 잔재만으로
// "원본이 이미 .bak로 옮겨졌다"는 게시 완료를 오판한다.
func publishAlreadyComplete(dir string) (bool, error) {
	exists, err := fileExists(filepath.Join(dir, dbFileName))
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	healthy, err := probeHealthyStrict(dir)
	if err != nil {
		return false, err
	}
	if !healthy {
		return false, nil
	}
	_, found, err := latestBackupMain(dir)
	return found, err
}

// probeHealthyStrict — fresh read-only 연결로 strict quick_check(A4). 명시적 "ok"만 healthy로
// 판정하고 malformed·미확정은 모두 not-healthy(false)로 돌린다 — probeCorrupt(§6.2 보수 판정,
// 미확정=손상 아님)와 정반대 방향으로, recover의 완료/tmp 게시 판정 전용이다(미확정을 건강으로
// 흘려 데이터를 유실시키지 않기 위해).
func probeHealthyStrict(dir string) (bool, error) {
	reader, err := OpenReadOnly(dir)
	if err != nil {
		return false, fmt.Errorf("session recover: %w", err)
	}
	defer func() { _ = reader.Close() }()
	if err := quickCheckStrict(reader); err != nil {
		if errors.Is(err, ErrCorrupt) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
// 것을 실측 확인했지만(Windows), 크로스플랫폼 방어로 명시 삭제도 병행한다. 마지막에 syncDir로
// 인양본을 디스크에 확정한다 — 재리뷰 Critical 수정: 게시(⑥) 도중 crash 재개 분기
// (resumePublishOnly)가 "검증까지 끝난 tmp가 디스크에 살아있다"는 사실에 의존하게 됐으므로,
// 이 fsync는 이제 단순 defense-in-depth가 아니라 재개 계약의 전제(load-bearing)다.
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
	if err := checkRescuedHealth(vconn); err != nil {
		return err
	}
	return syncDir(dir)
}

// checkRescuedHealth — quick_check + user_version(스키마) 확인(설계 §6.3 ⑤ 검증부). conn은
// 이미 열려 있는 읽기 전용 연결. verifyRescued(최초 검증)와 tmpIsHealthy(게시 crash 재개
// 분기가 이전에 검증된 tmp를 재확인할 때)가 공유한다 — 재리뷰 Critical 수정으로 분리.
func checkRescuedHealth(conn *sql.DB) error {
	if err := quickCheckStrict(conn); err != nil { // A4: 인양본은 미확정도 손상 취급(strict)
		return fmt.Errorf("session recover: 인양본 quick_check 실패: %w", err)
	}
	var uv int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		return fmt.Errorf("session recover: 인양본 user_version 확인 실패: %w", err)
	}
	if uv != schemaVersion {
		return fmt.Errorf("session recover: 인양본 user_version=%d 기대값=%d 불일치", uv, schemaVersion)
	}
	return nil
}

// tmpIsHealthy — 게시(⑥) 도중 crash 재개 분기 전용(재리뷰 Critical 수정, 설계 §6.3 ⑦): 이미
// verifyRescued를 통과한 적 있는 인양본(recoverTmpName)이 재실행 시점에도 여전히 건강한지
// 재확인한다. 존재하지 않거나 열기·검증 중 어떤 오류든 나면 "건강하지 않음"으로 취급해
// 호출자가 재인양 경로(restoreLatestBackup)로 넘어가게 한다 — 이 함수 자체는 원인을 구분하지
// 않는다(보수적 기본값).
func tmpIsHealthy(dir string) (bool, error) {
	tmpPath := filepath.Join(dir, recoverTmpName)
	exists, err := fileExists(tmpPath)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	conn, err := openReadOnlyAt(tmpPath)
	if err != nil {
		return false, nil
	}
	defer func() { _ = conn.Close() }()
	if err := checkRescuedHealth(conn); err != nil {
		return false, nil
	}
	return true, nil
}

// latestBackupMain — dbFileName+bakInfix<ts> 메인 멤버(‑wal/‑shm 접미사 없는 것) 중 사전순
// (=시간순, ts 포맷이 정렬 가능) 최신 것의 파일명을 반환한다(설계 §6.3 ⑦ 재개 방어 분기 —
// bak family로부터 원본 위치 복원 대상 선정, 재리뷰 Critical 수정).
func latestBackupMain(dir string) (string, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, sanitizeIOErr("recover dir read", err)
	}
	prefix := dbFileName + bakInfix
	var latest string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		if name > latest {
			latest = name
		}
	}
	return latest, latest != "", nil
}

// restoreLatestBackup — 게시(⑥) crash 재개의 방어 분기(재리뷰 Critical 수정, 설계 §6.3 ⑦):
// session.db가 없고 인양본(tmp)도 못 쓰는 상태에서, 가장 최근 .bak-<ts> family(원본이 게시
// 직전 rename된 것, -shm/-wal 포함)를 session.db 위치로 되돌린다. 되돌린 뒤에는 다시 "훼손된
// 원본이 그 자리에 있는" 상태가 되어 호출자(Recover)의 정상 인양 파이프라인(④~⑦)이 처음부터
// 다시 돈다. 수동 CLI(session recover) 내부에서, 사용자가 명시적으로 recover를 실행했다는
// 동의 하에 recover 자신이 만든 백업을 자신이 되돌리는 것뿐이므로, 설계 §6.1이 기각한 "서버가
// 스스로 감지해 rename"과는 성격이 다르다 — §6.2의 자동 rename 금지(서버 fail-closed 유지
// 목적)와 충돌하지 않는다.
func restoreLatestBackup(dir string) error {
	main, found, err := latestBackupMain(dir)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("session recover: 게시 중단 감지됐으나 인양본·백업 모두 없음 — 수동 확인 필요")
	}
	// A3①: main을 **마지막**에 복원한다(-wal→-shm→main). 불변식: main 복원 완료 전까지
	// dbExists=false를 유지해야 한다 — 그렇지 않으면(main 먼저 복원 후 crash) 재실행이 dbExists=
	// true로 판정해 -wal 없는 main을 재인양하고 WAL 꼬리를 잃는다. main이 마지막이므로 어느
	// 시점에 crash해도 재실행은 dbExists=false를 보고 이 복원을 처음부터 다시 완료한다.
	for _, suffix := range []string{"-wal", "-shm", ""} {
		src := filepath.Join(dir, main+suffix)
		exists, err := fileExists(src)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		dst := filepath.Join(dir, dbFileName+suffix)
		if err := os.Rename(src, dst); err != nil {
			return sanitizeIOErr("recover restore backup rename", err)
		}
	}
	return nil
}

// publishRescued — 설계 §6.3 ⑥: 원본을 백업하고(backupOriginal) 인양본을 게시한다
// (finishPublish). 둘로 나뉜 이유(재리뷰 Critical 수정): (1) 테스트가 backupOriginal까지만
// 호출해 "원본은 이미 .bak로 옮겨졌지만 인양본 게시는 아직" crash 상태를 주입할 수 있게,
// (2) resumePublishOnly(게시 crash 재개 경로)가 finishPublish만 재사용할 수 있게(원본은 이미
// 이전 실행에서 백업됐으므로 다시 백업하면 안 된다).
func publishRescued(dir string) (string, error) {
	backupName, err := backupOriginal(dir)
	if err != nil {
		return "", err
	}
	if err := finishPublish(dir); err != nil {
		return "", err
	}
	return backupName, nil
}

// backupOriginal — 설계 §6.3 ⑥ 전반부: 원본 family(-shm→-wal→session.db 순)를
// session.db.bak-<ts> family로 rename한다.
func backupOriginal(dir string) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	backupName := dbFileName + bakInfix + ts
	dbPath := filepath.Join(dir, dbFileName)

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
	return backupName, nil
}

// finishPublish — 설계 §6.3 ⑥ 후반부: 인양본(단일 파일) → session.db, 디렉터리 fsync.
func finishPublish(dir string) error {
	tmpPath := filepath.Join(dir, recoverTmpName)
	dbPath := filepath.Join(dir, dbFileName)
	if err := os.Rename(tmpPath, dbPath); err != nil {
		return sanitizeIOErr("recover publish rename", err)
	}
	return syncDir(dir)
}

// resumePublishFromTmp — 마커 하 tmp-우선 재개(A2, 설계 §6.3 ⑦): 검증 완료된 인양본(tmp)이
// 건강하면 재인양 없이 게시만 마저 끝낸다. 원본 session.db가 아직 남아 있으면(backupOriginal이
// 원본 -wal/-shm만 옮기고 main rename 전 crash) 남은 원본 family를 backupOriginal로 마저
// 백업한 뒤(존재하는 멤버만 옮기므로 부분 이동 family는 포렌식 잔재로 남는다) finishPublish로
// tmp를 게시한다 — 원본에서 재인양하면 이미 분리돼 나간 -wal 꼬리를 잃으므로 tmp를 그대로
// 게시하는 것이 핵심이다. 원본이 이미 없으면(직전 실행이 backupOriginal까지 끝냄) 기존 백업을
// 그대로 보고한다. 반환 건수는 tmp를 직접 세어 §6.3 ⑦의 "인양 건수 보고"를 재인양 없이 지킨다.
func resumePublishFromTmp(dir string) (events, sessionsN int64, backupPrefix string, err error) {
	events, sessionsN, err = countTmpRows(dir)
	if err != nil {
		return 0, 0, "", err
	}
	dbExists, err := fileExists(filepath.Join(dir, dbFileName))
	if err != nil {
		return 0, 0, "", err
	}
	if dbExists {
		backupPrefix, err = backupOriginal(dir) // 남은 원본 family를 마저 백업(신규 ts)
	} else {
		backupPrefix, _, err = latestBackupMain(dir) // 이미 백업됨 — 기존 백업 그대로 보고
	}
	if err != nil {
		return 0, 0, "", err
	}
	if err := finishPublish(dir); err != nil {
		return 0, 0, "", err
	}
	return events, sessionsN, backupPrefix, nil
}

// countTmpRows — 인양본(tmp)의 session_events·sessions 행 수를 센다(재개 시 재인양 없이 §6.3 ⑦
// 건수 보고용).
func countTmpRows(dir string) (events, sessionsN int64, err error) {
	tmpPath := filepath.Join(dir, recoverTmpName)
	conn, err := openReadOnlyAt(tmpPath)
	if err != nil {
		return 0, 0, fmt.Errorf("session recover: 재개 인양본 열기 실패: %w", err)
	}
	countErr := conn.QueryRow("SELECT COUNT(*) FROM session_events").Scan(&events)
	if countErr == nil {
		countErr = conn.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessionsN)
	}
	_ = conn.Close()
	if countErr != nil {
		return 0, 0, fmt.Errorf("session recover: 재개 인양본 건수 조회 실패: %w", countErr)
	}
	return events, sessionsN, nil
}

// HasRecoverArtifacts — cli(session recover)가 session.db 부재 시 "복구할 잔여 상태가 있는가"를
// 판정하는 헬퍼(A1). 마커·인양본(tmp)·백업 main 중 하나라도 있으면 true — session.db가 없어도
// Recover에 위임해야 함을 뜻한다(게시 rename 도중 crash로 session.db만 사라진 상태에서 CLI가
// "session.db 없음"으로 거부해 영구 wedge되던 경로 차단). 세 잔여 상태의 파일명은 이 패키지가
// 소유하므로 cli가 문자열을 하드코딩하지 않도록 여기서 노출한다(D13).
func HasRecoverArtifacts(dir string) (bool, error) {
	for _, name := range []string{recoverMarkerName, recoverTmpName} {
		exists, err := fileExists(filepath.Join(dir, name))
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	_, found, err := latestBackupMain(dir)
	return found, err
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
