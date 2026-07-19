// Package session — Session DB(worktree당 1개) 저장 계층: 스키마 v1·lease·UUIDv7 발급·append.
// 설계서 §2(스키마·PRAGMA·잠금)·§6.2(손상 판정·recover 프로토콜).
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"modernc.org/sqlite" // driver(§10 규약상 blank import 예외, store.go와 동일) + Error.Code() 판별

	"github.com/wotjr1649/context-router/internal/store"
)

// 신규 sentinel 3종(설계 §6.2) — mcp 조립의 fail-closed 신호(T10). 그 외 오류는 기존 패턴
// 재사용(예: supersedes 미존재는 store.ErrNotFound를 그대로 wrap — session은 이미 store를
// AcquireLock 재사용 때문에 import하므로 별도 sentinel을 새로 만들 이유가 없다, D13).
var (
	ErrLeaseHeld      = errors.New("session: lease held")
	ErrRecoverPending = errors.New("session: recover pending")
	ErrCorrupt        = errors.New("session: corrupt")
)

const (
	dbFileName        = "session.db"
	lockFileName      = "session.lock"      // lifetime lease(shared) — §6.2 ①
	initLockFileName  = "session.init.lock" // 신규 생성·WAL 전환 직렬화(exclusive) — §6.2 ②
	recoverMarkerName = "session.recover-pending"
)

// pragmas — internal/store/store.go의 `pragmas` 상수(D9 PRAGMA 세트)와 문자 그대로 동일한
// 값이다. session은 store 상수를 import하지 않고 리터럴을 복제한다(설계 §2.1) — 두 패키지가
// 독립적으로 진화해도 D9 규율 자체는 텍스트로 고정된다.
const pragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

const schemaVersion = 1 // session.db 독자 PRAGMA user_version 공간(설계 §2.1) — store.SchemaVersion과 무관

// schemaV1 — 설계서 §2.2 스키마 v1 전문 + §2.3 이벤트 FTS(T6, 같은 applySchemaV1에 합류 —
// 출시 전 v1 완성 중이므로 마이그레이션 아님, user_version은 1 그대로). 주의: 이 DDL은
// migrate()의 user_version==0 분기 뒤에서만 실행된다(applySchemaV1 참고) — 이미 v1로 열린
// 적 있는 기존 로컬 dev session.db에는 FTS 테이블이 생기지 않는다(출시 전이라 허용, 코드로
// 우회 마이그레이션을 만들지 않음 — 태스크 브리프 명시 지침).
const schemaV1 = `
CREATE TABLE IF NOT EXISTS session_events(
  id            INTEGER PRIMARY KEY,   -- rowid = append 순서(커밋 순 전순서)
  event_id      TEXT NOT NULL UNIQUE,  -- UUIDv7(시간정렬·전역유일) — export eventId·supersedes 대상
  session_id    TEXT NOT NULL,         -- 서버 프로세스 시작 시 발급(UUIDv7)
  event_type    TEXT NOT NULL,         -- CHECK 없음(§26 미지 타입 보존 의무)
  ts            INTEGER NOT NULL,      -- unix 초
  summary       TEXT NOT NULL,         -- 항상 존재하는 요약 — 복원·FTS의 최소 단위
  payload       TEXT,                  -- attributes JSON(redaction 후)
  artifact_refs TEXT,                  -- JSON 배열 ["artifact://<session-id>/sha256-...", ...] (§3.1)
  related       TEXT,                  -- JSON 배열(relatedResources)
  redaction     TEXT NOT NULL DEFAULT 'none',  -- none | spans (content.db와 동일 의미론)
  supersedes    TEXT                   -- nullable event_id — 교정 관계(삭제 금지, §6.2 승계)
);
CREATE INDEX IF NOT EXISTS idx_ev_session ON session_events(session_id, id);
CREATE INDEX IF NOT EXISTS idx_ev_type    ON session_events(event_type, id);  -- summary 타입별 최신 N
CREATE INDEX IF NOT EXISTS idx_ev_ts      ON session_events(ts);              -- retention 스윕
CREATE INDEX IF NOT EXISTS idx_ev_sup     ON session_events(supersedes) WHERE supersedes IS NOT NULL;

CREATE TABLE IF NOT EXISTS sessions(       -- 불변 세션 메타(가변 컬럼·FK 없음)
  session_id    TEXT PRIMARY KEY,
  started_at    INTEGER NOT NULL,
  producer      TEXT NOT NULL,              -- "context-router/<version>" — export producer 유도(오귀속 방지)
  retention_sec INTEGER NOT NULL DEFAULT 0  -- 0 = 무기한(정책 미표명) — §5 스윕 의미론
);

-- 이벤트 FTS(설계 §2.3) — content.db(internal/store/store.go)의 porter+trigram external
-- content 패턴과 동형이나 색인 대상은 summary 컬럼만(payload는 노이즈·비밀 표면이라 제외).
-- session_events는 append-only + retention DELETE만 있어(UPDATE 없음) AFTER UPDATE 트리거는
-- 불필요.
CREATE VIRTUAL TABLE IF NOT EXISTS fts_ev_porter  USING fts5(summary, content='session_events', content_rowid='id', tokenize='porter unicode61');
CREATE VIRTUAL TABLE IF NOT EXISTS fts_ev_trigram USING fts5(summary, content='session_events', content_rowid='id', tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS session_events_ai AFTER INSERT ON session_events BEGIN
  INSERT INTO fts_ev_porter(rowid, summary) VALUES (new.id, new.summary);
  INSERT INTO fts_ev_trigram(rowid, summary) VALUES (new.id, new.summary);
END;
CREATE TRIGGER IF NOT EXISTS session_events_ad AFTER DELETE ON session_events BEGIN
  INSERT INTO fts_ev_porter(fts_ev_porter, rowid, summary) VALUES ('delete', old.id, old.summary);
  INSERT INTO fts_ev_trigram(fts_ev_trigram, rowid, summary) VALUES ('delete', old.id, old.summary);
END;
PRAGMA user_version = 1;`

// eventTypeSessionStart — 서버 시작 시 자동 append되는 이벤트 타입(설계 §2.2). 어휘 비강제
// 원칙상 CHECK 제약은 없지만, 서버가 스스로 발행하는 유일한 고정 event_type이다.
const eventTypeSessionStart = "session_start"

// sessionStartPayload — session_start 이벤트의 payload(설계 §2.2: "가변 세션 상태는 컬럼이
// 아니라 이벤트로 기록" — worktree root·보존 정책 플래그 요약).
type sessionStartPayload struct {
	WorktreeRoot string `json:"worktree_root"`
	RetentionSec int64  `json:"retention_sec"`
}

// Options — Open()의 세션 시작 정책(설계 §2.2·§5).
type Options struct {
	RetentionSec int64  // 0 = 무기한(정책 미표명)
	Producer     string // "context-router/<version>" — sessions.producer로 불변 기록
}

// Event — ctr_record_event 입력 표현(redaction·검증 완료본, 설계 §3.1). Append가 event_id·
// session_id·ts를 서버 측에서 기입한다. Redaction은 호출자(mcp 핸들러)가 ingest.Redact 적용
// 결과로 채운다 — 빈 값이면 Append가 'none'으로 기입한다(T3 이관 계약).
type Event struct {
	Type, Summary         string
	Attributes            json.RawMessage
	ArtifactRefs, Related []string
	Supersedes            string // nullable event_id — 존재하면 append 시점에 존재 검증
	Redaction             string // "none"|"spans" — 빈 문자열은 'none'으로 정규화(Append)
}

// DB — 열린 session.db 핸들. writer 1연결(SetMaxOpenConns(1), _txlock=immediate) + reader
// (store.go와 동형, 설계 §2.1). leaseRelease는 Close에서 1회만 호출한다(store.AcquireLock의
// release는 idempotent가 아니다 — 두 번 호출 금지).
type DB struct {
	writer, reader *sql.DB
	sessionID      string
	leaseRelease   func()
}

// Open — session.db를 연다(설계 §6.2 순서 ①~⑥). dir은 `projects/<pid>/worktrees/<wid>`이며
// 호출자가 아직 만들지 않았을 수 있어 여기서 MkdirAll한다.
//
//  1. session.lock shared 논블로킹 취득·보유(프로세스 수명) — 실패 시 ErrLeaseHeld(복구 CLI가
//     exclusive 보유 중).
//  2. 복구 마커(session.recover-pending) 존재 시 ErrRecoverPending — quick_check 결과와
//     무관하게 fail-closed(§6.3: 부분 게시 상태에서 빈 DB를 신규 생성하지 않는다).
//  3. session.db 파일이 아직 없으면 session.init.lock exclusive로 최초 WAL 전환을 프로세스간
//     직렬화(store.lockStore와 동일 문제의식 — 완료 후 해제, 기존 파일이면 생략).
//  4. PRAGMA(D9)+user_version=1 멱등 DDL 단일 트랜잭션.
//  5. quick_check 보수적 판정(§6.2) — 명시적 malformed만 ErrCorrupt.
//  6. session_id(UUIDv7) 발급, sessions 행 INSERT, session_start 이벤트 자동 append.
func Open(dir string, opts Options) (*DB, error) {
	if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
		return nil, sanitizeIOErr("open mkdir", mkErr)
	}

	// ① lifetime lease — 프로세스 종료까지 보유, 실패 시 재시도 없이 즉시 fail-closed.
	leaseRelease, lockErr := store.AcquireLock(filepath.Join(dir, lockFileName), true)
	if lockErr != nil {
		return nil, fmt.Errorf("session Open: session.lock 공유 잠금 획득 실패: %w: %v", ErrLeaseHeld, lockErr)
	}
	ok := false
	defer func() {
		if !ok {
			leaseRelease()
		}
	}()

	// ② 복구 마커 — 존재하면 quick_check 결과와 무관하게 fail-closed.
	if _, statErr := os.Stat(filepath.Join(dir, recoverMarkerName)); statErr == nil {
		return nil, fmt.Errorf("session Open: %w", ErrRecoverPending)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, sanitizeIOErr("recover marker stat", statErr)
	}

	// ③ 신규 생성 직렬화 — 파일이 이미 있으면 생략(다중 프로세스 동시 append는 지원 토폴로지,
	// 설계 §2.1 — 매 Open마다 exclusive를 잡을 이유가 없다).
	dbPath := filepath.Join(dir, dbFileName)
	isNew := false
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		isNew = true
	} else if statErr != nil {
		return nil, sanitizeIOErr("session.db stat", statErr)
	}

	var initRelease func()
	if isNew {
		rel, initErr := acquireInitLock(filepath.Join(dir, initLockFileName), dbPath)
		if initErr != nil {
			return nil, initErr
		}
		initRelease = rel // nil이면 경합 승자가 이미 생성을 끝낸 것을 감지해 락 없이 합류
	}
	defer func() {
		if initRelease != nil {
			initRelease()
		}
	}()

	dsn := "file:" + filepath.ToSlash(dbPath) + pragmas
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("session Open: %w", err)
	}
	w.SetMaxOpenConns(1)
	defer func() {
		if !ok {
			w.Close()
		}
	}()
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("session Open: %w", err)
	}
	r.SetMaxOpenConns(4)
	defer func() {
		if !ok {
			r.Close()
		}
	}()

	d := &DB{writer: w, reader: r}
	// ④ PRAGMA+DDL. 실제로는 파일 헤더가 훼손된 경우 이 첫 쿼리(PRAGMA user_version)에서
	// SQLITE_NOTADB/CORRUPT가 실측 확인됨(실장 프로브) — migrate() 내부에서 즉시 ErrCorrupt로
	// 분류한다.
	if migrateErr := d.migrate(); migrateErr != nil {
		return nil, migrateErr
	}
	if initRelease != nil {
		initRelease() // "완료 후 해제"
		initRelease = nil
	}

	// ⑤ quick_check — 보수적 판정(§6.2): 명시적 malformed만 ErrCorrupt, BUSY/일시 I/O는
	// 재시도 후 미확정=통과.
	if qcErr := quickCheck(d.reader); qcErr != nil {
		return nil, qcErr
	}

	// ⑥ session_id 발급 + sessions 행 + session_start 자동 이벤트.
	sid, uuidErr := uuid.NewV7()
	if uuidErr != nil {
		return nil, fmt.Errorf("session Open: session_id 발급 실패: %w", uuidErr)
	}
	d.sessionID = sid.String()

	startedAt := time.Now().Unix()
	if txErr := d.txRetry(context.Background(), func(tx *sql.Tx) error {
		_, execErr := tx.Exec(`INSERT INTO sessions(session_id, started_at, producer, retention_sec) VALUES(?,?,?,?)`,
			d.sessionID, startedAt, opts.Producer, opts.RetentionSec)
		return execErr
	}); txErr != nil {
		return nil, fmt.Errorf("session Open: sessions 행 삽입 실패: %w", txErr)
	}

	payload, _ := json.Marshal(sessionStartPayload{WorktreeRoot: dir, RetentionSec: opts.RetentionSec})
	if _, _, _, appendErr := d.Append(Event{Type: eventTypeSessionStart, Summary: "session started", Attributes: payload}); appendErr != nil {
		return nil, fmt.Errorf("session Open: session_start 이벤트 기록 실패: %w", appendErr)
	}

	d.leaseRelease = leaseRelease
	ok = true
	return d, nil
}

// acquireInitLock — session.init.lock을 store.go의 lockStore()와 동일한 유계 백오프(10ms→
// 20→40→80→160ms 유지, 총 5초)로 재시도한다(설계 §6.2 ②: "기존 lockStore 패턴" — 대기 없는
// 즉시 거부는 계약 미달, 리뷰 Important). 두 호스트가 같은 신규 worktree를 동시 콜드스타트하면
// 패자는 매 실패 후 dbPath를 재확인해, 승자가 이미 생성을 끝냈으면(파일 존재) 락 없이
// nil,nil을 반환해 호출자가 기존 파일 경로(isNew=false와 동형)로 합류하게 한다. 유계 초과 시
// ErrLeaseHeld로 표면화(toToolError가 매핑 가능하도록 신규 sentinel 대신 기존 3종 재사용).
func acquireInitLock(lockPath, dbPath string) (func(), error) {
	deadline := time.Now().Add(5 * time.Second)
	delay := 10 * time.Millisecond
	for {
		release, err := store.AcquireLock(lockPath, false)
		if err == nil {
			return release, nil
		}
		if _, statErr := os.Stat(dbPath); statErr == nil {
			return nil, nil // 승자가 이미 생성 완료 — 락 없이 기존 파일 경로로 합류
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("session Open: session.init.lock 대기 초과: %w: %v", ErrLeaseHeld, err)
		}
		time.Sleep(delay)
		if delay < 160*time.Millisecond {
			delay *= 2
		}
	}
}

// migrate — store.go의 migrate()와 동형(설계 §2.1). PRAGMA user_version==0이면 스키마
// 최초 적용, ==schemaVersion이면 멱등 통과, 그 외는 비파괴 거부.
//
// 두 오퍼레이션(user_version 조회·applySchemaV1) 모두 migrateBusyRetry로 감싼다(재리뷰
// Important): 콜드스타트 경합에서 패자가 (a) acquireInitLock의 shortcut(승자가 이미 파일을
// 만든 것을 감지해 락 없이 합류)으로 오거나 (b) Open() 최상단 os.Stat이 [파일 생성~스키마
// 커밋] 사이의 좁은 창에 실행돼 애초에 isNew=false로 init.lock 자체를 건너뛰고 오거나, 두
// 경로 모두 여기서 승자의 최초 WAL 전환/스키마 커밋을 busy 재시도로 기다린 뒤 user_version=1
// 을 보고 멱등 통과해야 한다 — 그래야 store.go 주석의 WAL_RECOVER_LOCK(busy_timeout을
// 우회하는 즉시-BUSY)을 만나도 즉시 전파하지 않는다.
func (d *DB) migrate() error {
	var v int
	if err := migrateBusyRetry(func() error {
		return d.writer.QueryRow("PRAGMA user_version").Scan(&v)
	}); err != nil {
		return err
	}
	switch {
	case v == 0:
		return migrateBusyRetry(d.applySchemaV1)
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("session migrate: db user_version=%d > 지원 %d — 비파괴 거부: %w", v, schemaVersion, store.ErrUnavailable)
	default:
		return fmt.Errorf("session migrate: 알 수 없는 하위 버전 %d: %w", v, store.ErrUnavailable)
	}
}

// migrateBusyRetry — quickCheck/lockStore와 동일 백오프(50/200/800ms, 최대 3회)로 op을
// 재시도한다. isMalformed(SQLITE_CORRUPT=11/NOTADB=26)는 즉시 ErrCorrupt, isBusy(BUSY=5/
// LOCKED=6)는 재시도, 그 외는 즉시 전파. 유계 소진 시 원시 오류를 그대로 흘리지 않고
// ErrLeaseHeld로 wrap한다(T3 toToolError 매핑 계약 — 비-sentinel 원시 오류가 mcp 계층에서
// 조용한 빈 결과로 뭉개지는 것을 방지).
func migrateBusyRetry(op func() error) error {
	delays := [3]time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 800 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if isMalformed(err) {
			return fmt.Errorf("session migrate: %w", ErrCorrupt)
		}
		if !isBusy(err) {
			return fmt.Errorf("session migrate: %w", err)
		}
		if attempt >= len(delays)-1 {
			return fmt.Errorf("session migrate: 최초 WAL 전환 대기 소진: %w: %v", ErrLeaseHeld, err)
		}
		time.Sleep(delays[attempt])
	}
}

// applySchemaV1 — store.go의 applySchemaV1()과 동형: 단일 트랜잭션으로 스키마+user_version을
// 적용해 중도 실패로 인한 부분 스키마(user_version=0 그대로 일부만 생성)를 방지한다. 모든
// 문(IF NOT EXISTS)이 멱등이라 롤백 후 재시도해도 안전하다.
func (d *DB) applySchemaV1() error {
	tx, err := d.writer.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(schemaV1); err != nil {
		_ = tx.Rollback() // 커밋 전이라 무해 — 원 오류만 반환
		return err
	}
	return tx.Commit()
}

// quickCheck — "PRAGMA quick_check" 보수적 판정(§6.2). SQLITE_BUSY/LOCKED나 그 외 미분류
// 오류는 지수 백오프로 재시도하고, 재시도 소진 후에도 미확정이면 손상으로 취급하지 않는다
// (살아있는/바쁜 정상 DB를 손상으로 오분류하지 않는 것이 이 함수의 유일한 목적). 명시적
// SQLITE_CORRUPT/NOTADB, 혹은 quick_check가 정상 실행되어 "ok" 아닌 결과를 반환하면
// ErrCorrupt로 확정한다.
func quickCheck(reader *sql.DB) error {
	delays := [3]time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 800 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		var result string
		err := reader.QueryRow("PRAGMA quick_check").Scan(&result)
		if err == nil {
			if result == "ok" {
				return nil
			}
			return fmt.Errorf("session quickCheck: %q: %w", result, ErrCorrupt)
		}
		if isMalformed(err) {
			return fmt.Errorf("session quickCheck: %w", ErrCorrupt)
		}
		if attempt >= len(delays)-1 {
			return nil // 미확정 — 손상 취급하지 않음(§6.2 보수 판정)
		}
		time.Sleep(delays[attempt])
	}
}

// isMalformed — SQLITE_CORRUPT(11)/SQLITE_NOTADB(26) 여부(확장 코드는 하위 8비트가 기본
// 코드와 같다, SQLite 불변식 — store.go의 isBusy와 동일 관례).
func isMalformed(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == 11 || code == 26
}

// isBusy — store.go의 isBusy()와 문자 그대로 동일 로직(unexported라 import 불가, 리터럴
// 복제). SQLITE_BUSY(5)/SQLITE_LOCKED(6) 여부.
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == 5 || code == 6
}

// txRetry — store.go의 txRetry()와 동형(§2.1 "writer 1연결 + txRetry 동형"): BEGIN IMMEDIATE
// 트랜잭션 1개로 fn 실행, BUSY/LOCKED면 지수 백오프(50/200/800ms)로 최대 3회 재시도.
func (d *DB) txRetry(ctx context.Context, fn func(tx *sql.Tx) error) error {
	delays := [3]time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 800 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		err := d.runTx(ctx, fn)
		if err == nil || !isBusy(err) {
			return err
		}
		if attempt >= len(delays) {
			return fmt.Errorf("session txRetry: 재시도 소진: %w", store.ErrUnavailable)
		}
		select {
		case <-time.After(delays[attempt]):
		case <-ctx.Done():
			return fmt.Errorf("session txRetry: %w", ctx.Err())
		}
	}
}

func (d *DB) runTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() // 커밋 전이라 무해 — 원 오류만 반환
		return err
	}
	return tx.Commit()
}

// Append — 이벤트 1건을 session_events에 추가한다(설계 §3.1 저장 경로). event_id(UUIDv7)·
// session_id·ts는 서버가 발급한다. ev.Supersedes가 있으면 같은 트랜잭션 안에서 존재를
// 검증한다(§2.2: FK 없음 — 애플리케이션 검증으로 대체). 미존재면 store.ErrNotFound로 wrap
// (신규 sentinel 대신 기존 패턴 재사용, D13).
func (d *DB) Append(ev Event) (id int64, eventID string, ts int64, err error) {
	eid, uuidErr := uuid.NewV7()
	if uuidErr != nil {
		return 0, "", 0, fmt.Errorf("session Append: event_id 발급 실패: %w", uuidErr)
	}
	eventID = eid.String()
	ts = time.Now().Unix()

	artifactRefsJSON, jErr := marshalStrings(ev.ArtifactRefs)
	if jErr != nil {
		return 0, "", 0, fmt.Errorf("session Append: artifact_refs 직렬화 실패: %w", jErr)
	}
	relatedJSON, jErr := marshalStrings(ev.Related)
	if jErr != nil {
		return 0, "", 0, fmt.Errorf("session Append: related 직렬화 실패: %w", jErr)
	}
	var payload any
	if len(ev.Attributes) > 0 {
		payload = string(ev.Attributes)
	}

	redaction := ev.Redaction
	if redaction == "" {
		redaction = "none"
	}

	txErr := d.txRetry(context.Background(), func(tx *sql.Tx) error {
		if ev.Supersedes != "" {
			var exists int
			qErr := tx.QueryRow("SELECT 1 FROM session_events WHERE event_id=?", ev.Supersedes).Scan(&exists)
			if errors.Is(qErr, sql.ErrNoRows) {
				return fmt.Errorf("session Append: supersedes 이벤트 없음(event_id=%s): %w", ev.Supersedes, store.ErrNotFound)
			}
			if qErr != nil {
				return qErr
			}
		}
		res, execErr := tx.Exec(`INSERT INTO session_events(event_id, session_id, event_type, ts, summary, payload, artifact_refs, related, redaction, supersedes)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			eventID, d.sessionID, ev.Type, ts, ev.Summary, payload, artifactRefsJSON, relatedJSON, redaction, nullIfEmpty(ev.Supersedes))
		if execErr != nil {
			return execErr
		}
		id, _ = res.LastInsertId()
		return nil
	})
	if txErr != nil {
		return 0, "", 0, txErr
	}
	return id, eventID, ts, nil
}

// marshalStrings — v가 비어 있으면 NULL(nil), 아니면 JSON 배열 문자열로 직렬화한다
// (artifact_refs·related 컬럼 공용 헬퍼).
func marshalStrings(v []string) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// nullIfEmpty — store.go의 동명 헬퍼와 동일 관례(리터럴 복제, unexported라 import 불가).
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// SessionID — 이 프로세스가 Open 시점에 발급한 session_id(UUIDv7).
func (d *DB) SessionID() string { return d.sessionID }

// Reader — search·mcp 조회용 읽기 커넥션 풀.
func (d *DB) Reader() *sql.DB { return d.reader }

// Close — writer/reader를 닫고 lifetime lease를 해제한다. leaseRelease는 idempotent가
// 아니므로(store.AcquireLock 계약) Close는 한 DB당 정확히 1회만 호출해야 한다.
func (d *DB) Close() error {
	_, checkpointErr := d.writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	readerErr := d.reader.Close()
	writerErr := d.writer.Close()
	d.leaseRelease()
	return errors.Join(checkpointErr, readerErr, writerErr)
}

// sanitizeIOErr — store.go의 동명 헬퍼와 동일 관례(리터럴 복제, unexported라 import 불가):
// PathError/LinkError의 절대경로를 벗겨 syscall 원인만 wrap한다(코드-아키텍처 §5.5: 오류
// 메시지에 절대경로 금지).
func sanitizeIOErr(op string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("session: %s: %w", op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("session: %s: %w", op, le.Err)
	}
	return fmt.Errorf("session: %s: %w", op, err)
}
