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
	"regexp"
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
	dbFileName = "session.db"
	// LockFileName: lifetime lease(shared) 파일명 — §6.2 ①. 외부(훅 테스트 등)가 락 경합을
	// 재현할 때 리터럴 하드코딩 대신 참조하는 단일 진실원(T6 잔여).
	LockFileName      = "session.lock"
	initLockFileName  = "session.init.lock" // 신규 생성·WAL 전환 직렬화(exclusive) — §6.2 ②
	recoverMarkerName = "session.recover-pending"
)

// pragmas — internal/store/store.go의 `pragmas` 상수(D9 PRAGMA 세트)와 기반 넷
// (journal_mode·synchronous·busy_timeout·foreign_keys)은 문자 그대로 동일한 값이다. session은
// store 상수를 import하지 않고 리터럴을 복제한다(설계 §2.1) — 두 패키지가 독립적으로
// 진화해도 D9 규율 자체는 텍스트로 고정된다.
//
// 더는 완전히 동일하지 않다: store 쪽은 여기에 `journal_size_limit(33554432)`를 하나 더
// 붙인다(D102 계약 5·6·9, store.go의 journalSizeLimit) — 매일 도는 FTS 세그먼트 병합
// (`optimize`)이 한 트랜잭션에서 전체 인덱스를 다시 쓰며 남기는 WAL 고수위를 되돌리기
// 위해서다. session.db에는 그 병합이 없다(이벤트 FTS는 트리거로만 동기화한다, retention.go) —
// 이 pragma가 푸는 문제 자체가 content.db 전용이므로 session 쪽은 그대로 드라이버 기본값
// (무제한)으로 둔다. **여기에 옮겨 붙이지 마라** — session.db가 그 문제를 갖게 되기 전에는
// 재는 게 없는 한도일 뿐이다.
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
	Source       string `json:"source,omitempty"` // 훅 SessionStart의 source(§2.2 EnsureSession). Open()은 미주입(omitempty로 생략).
	WorktreeRoot string `json:"worktree_root"`
	RetentionSec int64  `json:"retention_sec"`
}

// Options — Open()의 세션 시작 정책(설계 §2.2·§5).
type Options struct {
	RetentionSec int64  // 0 = 무기한(정책 미표명)
	Producer     string // "context-router/<version>" — sessions.producer로 불변 기록
	// WorktreeRoot — session_start payload에 기록할 **사용자 worktree 경로**(설계 §2.2 의도).
	// 빈 값이면 Open의 dir(store 내부 경로)로 폴백해 기존 동작을 유지한다(D3, fable).
	WorktreeRoot string
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

// 이벤트 상한 상수(설계 §3.1 Global Constraints) — 정본 위치. mcp `validateRecordEventInput`
// 에서 이관(규칙·값 단일 소스). mcp는 테스트가 참조하는 둘(MaxSummaryBytes·MaxRefsOrRelated)만
// 별칭 참조해 값 중복을 없앤다.
const (
	MaxEventTypeBytes   = 64
	MaxSummaryBytes     = 2048
	MaxAttributesBytes  = 4096
	MaxRefsOrRelated    = 16 // artifact_refs·related 공용 개수 상한
	MaxRelatedItemBytes = 512
	MaxEventTotalBytes  = 8192
)

var eventTypeRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// ValidateEvent — 이벤트 상한·형식 검증(설계 §3.1). mcp `validateRecordEventInput`에서 이관한
// 단일 정본이다(규칙 중복 금지) — mcp는 wire 변환(refs URI 해석·redaction) 후 이 함수를 호출해
// INVALID_ARGUMENT로 매핑하고, AppendDB.Append는 저장 전 항상 호출한다. ev는 이미 변환 완료본
// 이라 ArtifactRefs는 정본 URI 문자열(각 119B 결정적)이므로 총합에 실제 바이트를 계상한다(구
// mcp의 artifactURIBytes×개수와 동치). 반환 문구는 mcp가 사용자에게 그대로 노출하므로 내부 오류
// prefix를 붙이지 않는다(사용자 대면 메시지).
func ValidateEvent(ev Event) error {
	switch {
	case len(ev.Type) > MaxEventTypeBytes || !eventTypeRe.MatchString(ev.Type):
		return errors.New("event_type은 [a-z0-9_]+이고 64바이트 이하여야 합니다")
	case len(ev.Summary) == 0 || len(ev.Summary) > MaxSummaryBytes:
		return errors.New("summary는 1~2048바이트여야 합니다")
	case len(ev.Attributes) > MaxAttributesBytes:
		return errors.New("attributes는 4096바이트 이하여야 합니다")
	case len(ev.ArtifactRefs) > MaxRefsOrRelated:
		return errors.New("artifact_refs는 16개 이하여야 합니다")
	case len(ev.Related) > MaxRefsOrRelated:
		return errors.New("related_resources는 16개 이하여야 합니다")
	}
	total := len(ev.Type) + len(ev.Summary) + len(ev.Attributes) + len(ev.Supersedes)
	for _, r := range ev.ArtifactRefs {
		total += len(r)
	}
	for _, r := range ev.Related {
		if len(r) > MaxRelatedItemBytes {
			return errors.New("related_resources 항목은 512바이트 이하여야 합니다")
		}
		total += len(r)
	}
	if total > MaxEventTotalBytes {
		return errors.New("이벤트 직렬화 총합이 8192바이트를 초과했습니다")
	}
	return nil
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
	// ① lifetime lease(MkdirAll 포함) — 프로세스 종료까지 보유, 실패 시 재시도 없이 즉시 fail-closed.
	leaseRelease, lockErr := acquireSharedLease(dir, "session Open")
	if lockErr != nil {
		return nil, lockErr
	}
	ok := false
	defer func() {
		if !ok {
			leaseRelease()
		}
	}()

	// ② 복구 마커 — 존재하면 quick_check 결과와 무관하게 fail-closed.
	if markerErr := checkRecoverMarker(dir, "session Open"); markerErr != nil {
		return nil, markerErr
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

	w, r, connErr := openWriterReader(dbPath, pragmas, 4)
	if connErr != nil {
		return nil, fmt.Errorf("session Open: %w", connErr)
	}
	defer func() {
		if !ok {
			w.Close()
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

	// D3: 사용자 worktree 경로를 기록한다(설계 §2.2). 미주입 시 store 내부 dir로 폴백.
	worktreeRoot := opts.WorktreeRoot
	if worktreeRoot == "" {
		worktreeRoot = dir
	}
	payload, _ := json.Marshal(sessionStartPayload{WorktreeRoot: worktreeRoot, RetentionSec: opts.RetentionSec})
	if _, _, _, appendErr := d.Append(Event{Type: eventTypeSessionStart, Summary: "session started", Attributes: payload}); appendErr != nil {
		return nil, fmt.Errorf("session Open: session_start 이벤트 기록 실패: %w", appendErr)
	}

	d.leaseRelease = leaseRelease
	ok = true
	return d, nil
}

// OpenReadOnly — session.db를 읽기 전용으로 연다(태스크 9a: CLI `session export`·doctor 전용
// 최소 헬퍼). Open()과 달리 lease 취득·마커 확인·신규 생성·sessions 행 INSERT·session_start
// 자동 append를 전혀 하지 않는다 — 조회 전용 배치 경로(export)가 대상 DB를 오염시키지 않기
// 위해서다(브리프 명시 지침: "session.Open을 export 경로에 쓰지 마라"). DSN은 store.go의
// read-only 관례(D9 PRAGMA + mode=ro&_pragma=query_only(ON))와 동일 형태 — session.db가 없으면
// sql.Open 자체는 성공하고(지연 연결) 첫 쿼리에서 오류가 난다, 존재 확인은 호출자 책임.
// 호출자가 Close 책임진다.
func OpenReadOnly(dir string) (*sql.DB, error) {
	return openReadOnlyAt(filepath.Join(dir, dbFileName))
}

// openReadOnlyAt — OpenReadOnly의 DSN 조립을 임의 경로에 적용한다(recover.go의 인양본
// 검증(§6.3 ⑤)이 dir/dbFileName이 아닌 임시 인양본 경로를 읽어야 해서 필요 — 새 패키지
// 인터페이스 없이 같은 패키지 내부 헬퍼로 공유, D13).
func openReadOnlyAt(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + pragmas + "&mode=ro&_pragma=query_only(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("session OpenReadOnly: %w", err)
	}
	return db, nil
}

// acquireSharedLease — MkdirAll(dir) 후 session.lock을 shared·논블로킹으로 취득한다(Open·
// OpenAppend 공통 ①, 중복 금지). 실패 시 ErrLeaseHeld로 wrap한다. op은 오류 문구 prefix(slog
// 전용, sentinel은 불변).
func acquireSharedLease(dir, op string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, sanitizeIOErr("open mkdir", err)
	}
	release, err := store.AcquireLock(filepath.Join(dir, LockFileName), true)
	if err != nil {
		return nil, fmt.Errorf("%s: session.lock 공유 잠금 획득 실패: %w: %v", op, ErrLeaseHeld, err)
	}
	return release, nil
}

// checkRecoverMarker — recover 마커(session.recover-pending) 존재 시 ErrRecoverPending(Open·
// OpenAppend 공통 ②). quick_check 결과와 무관하게 fail-closed — 부분 게시 상태에서 빈 DB를
// 신규 생성하지 않는다(§6.3). op은 오류 문구 prefix.
func checkRecoverMarker(dir, op string) error {
	if _, statErr := os.Stat(filepath.Join(dir, recoverMarkerName)); statErr == nil {
		return fmt.Errorf("%s: %w", op, ErrRecoverPending)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return sanitizeIOErr("recover marker stat", statErr)
	}
	return nil
}

// openWriterReader — writer(1연결·_txlock=immediate)와 reader 풀을 pragmasStr DSN으로 연다
// (Open·OpenAppend 공통 ④ 연결 부분, 중복 금지). Open은 pragmas(busy_timeout 5000), OpenAppend는
// hookPragmas(500ms)를 넘긴다. r 생성 실패 시 이미 연 w를 닫아 누수를 막는다.
func openWriterReader(dbPath, pragmasStr string, readerConns int) (w, r *sql.DB, err error) {
	dsn := "file:" + filepath.ToSlash(dbPath) + pragmasStr
	w, err = sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, nil, err
	}
	w.SetMaxOpenConns(1)
	r, err = sql.Open("sqlite", dsn)
	if err != nil {
		_ = w.Close()
		return nil, nil, err
	}
	r.SetMaxOpenConns(readerConns)
	return w, r, nil
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

// acquireInitLockCtx — acquireInitLock의 ctx-aware 변형(설계 §2.3, 훅 전용). 현행 acquireInitLock은
// 최대 5s를 무조건 sleep으로 소진해 훅 deadline 예산을 위반하므로 재사용 불가 — 대기 종료 조건을
// time.Now().After(deadline)가 아니라 ctx.Done()으로 바꿔 예산을 관측하며 폴링한다. 백오프(10ms→
// 160ms)·shortcut(승자가 이미 파일 생성 시 nil,nil로 락 없이 합류)·ErrLeaseHeld wrap은 동일하다.
func acquireInitLockCtx(ctx context.Context, lockPath, dbPath string) (func(), error) {
	delay := 10 * time.Millisecond
	for {
		release, err := store.AcquireLock(lockPath, false)
		if err == nil {
			return release, nil
		}
		if _, statErr := os.Stat(dbPath); statErr == nil {
			return nil, nil // 승자가 이미 생성 완료 — 락 없이 기존 파일 경로로 합류
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("session OpenAppend: session.init.lock 대기 예산 초과: %w: %v", ErrLeaseHeld, err)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, fmt.Errorf("session OpenAppend: session.init.lock 대기 예산 초과: %w: %v", ErrLeaseHeld, ctx.Err())
		}
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
func (d *DB) migrate() error { return migrateWriter(d.writer) }

// migrateWriter — migrate()의 writer 인자화(OpenAppend가 AppendDB의 writer에도 쓴다, 중복 금지).
// busy_timeout은 연결 DSN이 정하므로(Open=5000·OpenAppend=500) DSN-무관하게 동일 로직이 두
// 경로를 커버한다.
func migrateWriter(w *sql.DB) error {
	var v int
	if err := migrateBusyRetry(func() error {
		return w.QueryRow("PRAGMA user_version").Scan(&v)
	}); err != nil {
		return err
	}
	switch {
	case v == 0:
		return migrateBusyRetry(func() error { return applySchemaV1(w) })
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
func applySchemaV1(w *sql.DB) error {
	tx, err := w.Begin()
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
	// delays[2](800ms)는 실제 sleep에 쓰이지 않는 quirk다: 루프가 attempt>=len-1(=2)에서 sleep
	// 전에 반환하므로 attempt 0·1만 delays[0]·delays[1]을 sleep한다(migrateBusyRetry도 동형 —
	// txRetry만 len(delays) 상한이라 셋 다 사용). 배열 크기를 재시도 횟수 표기로 유지한다.
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

// quickCheckStrict — recover 경로 전용 엄격 변형(A4, 재리뷰 Codex P1). quickCheck의 §6.2 보수
// 판정(미확정=통과)과 정반대로, "ok"를 명시적으로 확인하지 못하면(malformed는 물론 BUSY·일시
// I/O 재시도 소진까지) ErrCorrupt로 확정한다. recover의 완료 선언(publishAlreadyComplete·
// probeHealthyStrict)과 인양본 건강 판정(checkRescuedHealth→verifyRescued·tmpIsHealthy)이 이걸
// 쓴다 — 미확정을 건강으로 오인하면 마커를 조기 삭제하거나 미검증 tmp를 게시해 데이터를
// 유실시키기 때문. 서버 Open(§6.2)은 살아있는 바쁜 DB를 손상으로 오분류하지 않으려 quickCheck
// (보수)를 그대로 유지한다 — 두 경로의 위험 방향이 반대다.
func quickCheckStrict(reader *sql.DB) error {
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
			return fmt.Errorf("session quickCheck: 미확정(재시도 소진) — recover strict 판정상 손상 취급: %w", ErrCorrupt)
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

// isCantOpen — SQLITE_CANTOPEN(14) 여부(런타임에 파일이 사라지거나 열 수 없게 된 경우 —
// isMalformed와 함께 "저장소 불용" 분류에 쓴다).
func isCantOpen(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code()&0xff == 14
}

// ClassifyStorageErr — startup 이후 런타임에 session.db가 malformed/불용이 됐을 때, mcp가
// raw SQLite 오류를 STORAGE_UNAVAILABLE로 매핑할 수 있게 분류한다(설계 §6.2, Codex P2). Open
// 시점 quick_check는 통과했지만 이후 훼손된 DB에 Summarize/Export/QueryEvents가 던지는 원시
// 오류(malformed/notadb/cantopen 계열)를 기존 ErrCorrupt로 wrap한다 — 신규 sentinel 없이
// toToolError 단일 매핑을 재사용하기 위함(§6, D13). 그 외 오류·nil은 그대로 통과시켜 정상
// 오류의 코드 매핑을 바꾸지 않는다. 원시 오류 상세는 재래핑하지 않는다(toToolError가 ErrCorrupt
// 를 일반 문구로 뭉개므로 사용자에겐 노출되지 않고, 절대경로 유출 표면도 만들지 않는다).
func ClassifyStorageErr(err error) error {
	if err == nil {
		return nil
	}
	if isMalformed(err) || isCantOpen(err) {
		return fmt.Errorf("session: 런타임 저장 오류: %w", ErrCorrupt)
	}
	return err
}

// txRetry — store.go의 txRetry()와 동형(§2.1 "writer 1연결 + txRetry 동형"): BEGIN IMMEDIATE
// 트랜잭션 1개로 fn 실행, BUSY/LOCKED면 지수 백오프(50/200/800ms)로 최대 3회 재시도.
func (d *DB) txRetry(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return txRetryWriter(ctx, d.writer, fn)
}

// txRetryWriter — txRetry의 writer 인자화(AppendDB.Append·EnsureSession이 ctx를 전파해 쓴다,
// 중복 금지). 즉시 취소된 ctx면 BeginTx가 곧장 ctx.Err()를 반환하고 !isBusy라 재시도 없이 즉시
// 표면화된다(블로킹 없음).
func txRetryWriter(ctx context.Context, w *sql.DB, fn func(tx *sql.Tx) error) error {
	delays := [3]time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 800 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		err := runTxWriter(ctx, w, fn)
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

func runTxWriter(ctx context.Context, w *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := w.BeginTx(ctx, nil)
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
	return appendEvent(context.Background(), d.writer, d.sessionID, ev)
}

// appendEvent — Append의 직렬화+INSERT 코어(ctx·writer·sessionID 인자화). DB.Append(무-ctx,
// 미검증)와 AppendDB.Append(ctx·사전 ValidateEvent)가 공유한다(중복 금지). ctx를 txRetry에
// 전파해 데드라인 초과 시 즉시 표면화한다(드롭 판정은 호출자). 상한 검증은 하지 않는다 —
// DB.Append는 미검증 승계, AppendDB.Append가 호출 전에 ValidateEvent를 돌린다.
func appendEvent(ctx context.Context, w *sql.DB, sessionID string, ev Event) (id int64, eventID string, ts int64, err error) {
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

	txErr := txRetryWriter(ctx, w, func(tx *sql.Tx) error {
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
		// D42 §4 세션 존재 게이트: INSERT…SELECT…WHERE EXISTS. 빈 세션 GC가 확인과 INSERT
		// 사이에 세션 행을 지워도(FK 부재) 삭제된 session_id로 이벤트가 커밋돼 retention 조인에서
		// 빠지는 영구 orphan을 만들지 않는다. 세션 부재면 0행 삽입 → store.ErrNotFound(기존 매핑
		// 경로 재사용, 호출자는 drop 처리). 미지 세션 이벤트는 실제 쓰기가 없다(WHERE EXISTS 0행).
		res, execErr := tx.Exec(`INSERT INTO session_events(event_id, session_id, event_type, ts, summary, payload, artifact_refs, related, redaction, supersedes)
			SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM sessions WHERE session_id=?)`,
			eventID, sessionID, ev.Type, ts, ev.Summary, payload, artifactRefsJSON, relatedJSON, redaction, nullIfEmpty(ev.Supersedes), sessionID)
		if execErr != nil {
			return execErr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("session Append: 세션 부재(session_id=%s): %w", sessionID, store.ErrNotFound)
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

// --- 훅 전용 append API (설계 §2.1~§2.3, MCP 표면 비노출) ---

// hookPragmas — pragmas와 동일 세트이나 busy_timeout을 500ms로 낮춘 훅 전용 DSN 조각(설계 §2.3:
// 훅 deadline 예산 안으로 모든 DB 대기를 상계). pragmas 상수(busy_timeout 5000)는 직접 재사용 불가.
const hookPragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(500)&_pragma=foreign_keys(ON)"

// AppendOptions — OpenAppend의 훅 세션 정책(설계 §2.2). ExternalSessionID는 "cc:<uuid>" 완성형
// (canonical UUID 형식 검증은 호출자 = hook의 책임, T4). Producer·RetentionSec은 EnsureSession이
// sessions 행에 기입한다.
type AppendOptions struct {
	ExternalSessionID string
	Producer          string
	RetentionSec      int64
}

// AppendDB — 훅 전용 append 핸들(설계 §2.1). *DB를 임베드하지 않는다 — 무-ctx DB.Append(ev)와
// ctx-수용 AppendDB.Append(ctx,ev)의 시그니처 그림자화 위험을 피하려 필드를 재사용한다.
type AppendDB struct {
	writer, reader *sql.DB
	sessionID      string // = opts.ExternalSessionID("cc:" 접두사 완성형)
	producer       string
	retentionSec   int64
	leaseRelease   func()
}

// OpenAppend — 훅 전용 append 표면을 연다(설계 §2.1~§2.3). Open()과 달리 ⑤ quick_check·⑥
// sessions INSERT·session_start 자동 append를 하지 않는다(세션 생성은 EnsureSession 전용). ctx
// deadline을 init-lock 대기와 이후 append에 전파하고 busy_timeout 500ms DSN을 써 모든 DB 대기를
// 훅 예산 안으로 상계한다. 손상은 여기서 감지하지 않고 첫 Append의 오류 분류로 사후 감지한다.
//
//	① shared lease(논블로킹) — 실패 시 ErrLeaseHeld
//	② recover 마커 → ErrRecoverPending(quick_check 결과 무관 fail-closed)
//	③ DB 미존재 시 ctx-aware init lock으로 최초 WAL 전환 직렬화
//	④ PRAGMA+멱등 DDL(500ms DSN)
//	⑤ quick_check 생략
//	⑥ sessions·session_start 미생성
func OpenAppend(ctx context.Context, dir string, opts AppendOptions) (*AppendDB, error) {
	leaseRelease, lockErr := acquireSharedLease(dir, "session OpenAppend")
	if lockErr != nil {
		return nil, lockErr
	}
	ok := false
	defer func() {
		if !ok {
			leaseRelease()
		}
	}()

	if markerErr := checkRecoverMarker(dir, "session OpenAppend"); markerErr != nil {
		return nil, markerErr
	}

	dbPath := filepath.Join(dir, dbFileName)
	isNew := false
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		isNew = true
	} else if statErr != nil {
		return nil, sanitizeIOErr("session.db stat", statErr)
	}

	var initRelease func()
	if isNew {
		rel, initErr := acquireInitLockCtx(ctx, filepath.Join(dir, initLockFileName), dbPath)
		if initErr != nil {
			return nil, initErr
		}
		initRelease = rel // nil이면 승자가 이미 생성을 끝낸 것 — 락 없이 합류
	}
	defer func() {
		if initRelease != nil {
			initRelease()
		}
	}()

	w, r, connErr := openWriterReader(dbPath, hookPragmas, 4)
	if connErr != nil {
		return nil, fmt.Errorf("session OpenAppend: %w", connErr)
	}
	defer func() {
		if !ok {
			w.Close()
			r.Close()
		}
	}()

	// ④ PRAGMA+멱등 DDL. migrateBusyRetry의 유계 sleep(≤250ms)은 예산 내라 ctx-aware화하지
	// 않는다 — 5s 무조건 sleep이던 init-lock과 busy_timeout(500ms 하향)이 초과 위험의 본체였다.
	if migrateErr := migrateWriter(w); migrateErr != nil {
		return nil, migrateErr
	}
	if initRelease != nil {
		initRelease() // "완료 후 해제"
		initRelease = nil
	}

	ad := &AppendDB{
		writer:       w,
		reader:       r,
		sessionID:    opts.ExternalSessionID,
		producer:     opts.Producer,
		retentionSec: opts.RetentionSec,
		leaseRelease: leaseRelease,
	}
	ok = true
	return ad, nil
}

// EnsureSession — sessions 행 INSERT OR IGNORE와, 행이 신규 삽입된 경우에만의 session_start
// append를 **단일 트랜잭션**으로 수행한다(설계 §2.2, 리뷰 반영). 단명 프로세스가 두 작업 사이에서
// kill돼 sessions 행만 남고 session_start가 영구 누락되는 경로를 차단한다 — session_start INSERT가
// 실패하면 sessions 행도 함께 롤백된다. 재호출(clear/compact 재발화) 시 이미 존재하면 created=false·
// 재발행 없음. session_start는 서버 발행 고정 이벤트라 ValidateEvent를 거치지 않는다(Open()과 동형).
func (ad *AppendDB) EnsureSession(ctx context.Context, source, worktreeRoot string) (created bool, err error) {
	startedAt := time.Now().Unix()
	eid, uuidErr := uuid.NewV7()
	if uuidErr != nil {
		return false, fmt.Errorf("session EnsureSession: event_id 발급 실패: %w", uuidErr)
	}
	payload, _ := json.Marshal(sessionStartPayload{Source: source, WorktreeRoot: worktreeRoot, RetentionSec: ad.retentionSec})

	txErr := txRetryWriter(ctx, ad.writer, func(tx *sql.Tx) error {
		created = false // BUSY 재시도 시 이전 롤백분의 잔상 방지 — 매 시도 재평가
		res, execErr := tx.Exec(`INSERT OR IGNORE INTO sessions(session_id, started_at, producer, retention_sec) VALUES(?,?,?,?)`,
			ad.sessionID, startedAt, ad.producer, ad.retentionSec)
		if execErr != nil {
			return execErr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 이미 존재 — session_start 재발행 안 함
		}
		if _, insErr := tx.Exec(`INSERT INTO session_events(event_id, session_id, event_type, ts, summary, payload, artifact_refs, related, redaction, supersedes)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			eid.String(), ad.sessionID, eventTypeSessionStart, startedAt, "session started", string(payload), nil, nil, "none", nil); insErr != nil {
			return insErr // session_start 실패 → tx 롤백 → sessions 행도 원복(원자성)
		}
		created = true
		return nil
	})
	if txErr != nil {
		return false, txErr
	}
	return created, nil
}

// SessionExists — 이 핸들의 ExternalSessionID로 sessions 행이 존재하는지 조회한다(미지 세션
// drop 판정용, 설계 §2.2).
func (ad *AppendDB) SessionExists(ctx context.Context) (bool, error) {
	var one int
	err := ad.reader.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE session_id=?", ad.sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Append — 이벤트 1건을 ExternalSessionID로 기록한다. 저장 전 항상 ValidateEvent를 돌리고(상한·
// 형식 단일 정본), ctx deadline을 txRetry에 전파한다 — 초과 시 context.DeadlineExceeded 계열을
// 즉시 반환한다(드롭 판정은 호출자, 설계 §2.3).
func (ad *AppendDB) Append(ctx context.Context, ev Event) (id int64, eventID string, ts int64, err error) {
	if vErr := ValidateEvent(ev); vErr != nil {
		return 0, "", 0, vErr
	}
	return appendEvent(ctx, ad.writer, ad.sessionID, ev)
}

// Close — writer/reader를 닫고 lifetime lease를 해제한다. DB.Close와 달리 wal_checkpoint를
// 하지 않는다 — 매 툴 호출마다 도는 단명 훅이 TRUNCATE checkpoint하면 서버 read txn과 겹칠 때
// busy_timeout(500ms)까지 대기해(게다가 deadline ctx 밖) 훅 latency만 부풀린다. WAL 정비는
// 장수 서버(DB.Close)·recover 경로의 몫이다(설계 §2.3). leaseRelease는 idempotent가 아니므로
// 한 핸들당 정확히 1회만 호출해야 한다.
func (ad *AppendDB) Close() error {
	readerErr := ad.reader.Close()
	writerErr := ad.writer.Close()
	ad.leaseRelease()
	return errors.Join(readerErr, writerErr)
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
