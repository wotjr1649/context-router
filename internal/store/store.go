// Package store — DB 수명·PRAGMA·스키마·단일 트랜잭션 계약·blob IO. 설계서 §3.3~3.6.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite" // driver(§10 규약상 blank import 예외) + Error.Code() BUSY/LOCKED 판별에 사용
)

var (
	ErrNotFound        = errors.New("store: not found")
	ErrUnavailable     = errors.New("store: unavailable")
	ErrConflict        = errors.New("store: conflict")
	ErrInvalidSelector = errors.New("store: invalid selector")
)

const SchemaVersion = 1

type Store struct {
	dir            string
	writer, reader *sql.DB
	ledger         *sql.DB
	// ledgerCols — Open 시점에 관측한 ledger 테이블의 실제 열 집합(migrateLedger가 낸다).
	// LedgerAppendFetch가 **있는 열만 지명**하려고 읽는다(릴리스 패스 소견 F1): 읽는 쪽이
	// PRAGMA table_info로 부분 이관을 관용하는데 쓰는 쪽만 세 열을 무조건 지명하면, 그 원장에서
	// INSERT가 통째로 거절되고 best-effort 관례가 그 오류를 삼킨다.
	// **Open 시점 스냅샷이라 낡을 수 있다** — 이 Store가 살아 있는 동안 다른 프로세스가 writable
	// Open으로 열을 붙일 수 있다(원장 DDL은 store open-lock 안에서 도는데 그 잠금은 Open 반환과
	// 함께 풀린다). 그때 이 Store는 한 계단 아래 형태로 계속 적고, 그 행들은 레거시로 읽힌다 —
	// 편향이지 유실이 아니다. 매 INSERT마다 PRAGMA를 다시 도는 값은 하지 않는다.
	ledgerCols map[string]bool
}

// journalSizeLimit — D102 계약 5·6·9: 병합(`optimize`)이 훑고 지나가며 남기는 WAL 고수위를
// 되돌리는 바닥값. DSN에 이 pragma가 없으면 번들 SQLite 기본 −1이 걸려 체크포인트가 WAL을
// *재사용*할 뿐 줄이지 않고(계약 5), 그 몫이 서버 세션 내내 `doctor [14]`의 file 축(계약 6,
// 본체+`-wal`+`-shm`)에 잡힌다. wal_autocheckpoint가 1000페이지(4 MiB)라 정상 운용의 WAL
// 작업 집합은 그 언저리다 — 32 MiB는 그 위로 넉넉해 평시에는 절단이 아예 일어나지 않고
// (반복 truncate/extend로 쓰기 경로를 무겁게 만들지 않는다), 병합이 만드는 96 MiB
// 스파이크보다는 확실히 아래라 그 몫이 회수된다.
//
// 병합 뒤 CheckpointTruncate를 직접 부르는 대신 DSN 파라미터를 쓴다 — 전자는 열린 reader와
// 공존 시 busy_timeout 소진까지 최대 5초 쓰기 락을 늘리지만(TestCheckpointTruncateBusyWithReader),
// 후자는 추가 락 보유가 0이다(계약 9가 최소화하려는 바로 그 구간).
//
// 바닥이지 0이 아니다: 절단은 체크포인트 자체가 아니라 그 뒤 빈 WAL에 첫 프레임을 쓰는
// 다음 커밋에서 일어난다(modernc.org/sqlite 소스 대조, TestJournalSizeLimitShrinksWALOnPassiveCheckpoint
// 참고) — 병합 직후 리더가 물려 있으면 체크포인트가 부분에 그쳐 절단이 미뤄지고, 다음 커밋의
// 체크포인트가 완료될 때 96 MiB → 32 MiB로 내려간다(자기 치유이되 즉시는 아니다). 문자열
// 상수인 이유는 pragmas DSN에 그대로 연결하고, 테스트도 이 상수 하나만 읽어 값을 두 곳에
// 적지 않기 위해서다.
const journalSizeLimit = "33554432" // 32 MiB

const pragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=journal_size_limit(" + journalSizeLimit + ")"

const lockFileName = "content.db.rebuild.lock"

// errLockBusy: tryLockFile(build-tag별 unix/windows 구현)의 논블로킹 시도가 다른 프로세스
// 보유로 실패했음을 나타내는 내부 신호(lockStore 재시도 대상) — 권한 오류 등 그 외 실패와
// 구분해 후자는 즉시 반환한다(무한 재시도 방지).
var errLockBusy = errors.New("store: lock busy")

// lockStore: writable Open()을 프로세스 간 직렬화한다. 신규 DB 최초 WAL 전환 시 SQLite의
// wal-index recovery 락 경로(WAL_RECOVER_LOCK)는 busy handler를 거치지 않고 SQLITE_BUSY를
// 즉시 반환한다(SQLite 정본 동작, modernc.org/sqlite v1.54.0 동일 — DSN _pragma 순서는
// applyQueryParams가 busy_timeout을 자체 우선정렬해 no-op이라 근본 수정이 못 된다). 이
// 구간은 busy_timeout으로 못 덮으므로 OS advisory lock으로 대신 직렬화한다(설계 §3.5의
// 배타 잠금 파일 경로를 store 생명주기 잠금으로 재사용). tryLockFile은 unix(flock)/
// windows(LockFileEx) 각각 store_lock_unix.go/store_lock_windows.go에 구현.
//
// 논블로킹 시도를 10→20→40→80→160ms(이후 160ms 유지) 지수 백오프로 재시도하며 총 5초
// 초과 시 ErrUnavailable로 포기한다(보유 프로세스 hang 시 무한대기 방지). 실패 모드: 보유
// 프로세스 크래시 → 커널이 잠금 자동 해제(stale lock 없음) / 마이그레이션 중 크래시 →
// 다음 프로세스가 멱등 스키마 재실행. 잠금 해제 후 파일 자체는 삭제하지 않는다.
func lockStore(dir string) (func(), error) {
	return lockStoreCtx(context.Background(), dir)
}

// lockStoreCtx: lockStore의 ctx 관측 변형. 백오프 sleep 구간에서 ctx.Done()도 함께 감시해
// deadline/취소 시 5s 하드 한도 이전에 ErrUnavailable로 포기한다(훅 deadline 예산 — 설계 §2.3).
// context.Background()로 부르면 ctx.Done()이 절대 발화하지 않아 기존 lockStore와 완전히 동일하게
// 동작한다(time.Sleep→select{time.After}는 무경합 시 동형). D13 예외: Open에 시그니처를 강제
// 전파(전 호출부 40여 곳 파급)하는 대신 ctx-aware 대기 변형 1건만 추가한다(arch §2 등재).
func lockStoreCtx(ctx context.Context, dir string) (func(), error) {
	path := filepath.Join(dir, lockFileName)
	deadline := time.Now().Add(5 * time.Second)
	delay := 10 * time.Millisecond
	for {
		release, err := tryLockFile(path, false) // exclusive
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("store open: 잠금 획득 실패: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("store open: 잠금 대기 초과: %w", ErrUnavailable)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("store open: 잠금 대기 취소: %w", ErrUnavailable)
		case <-time.After(delay):
		}
		if delay < 160*time.Millisecond {
			delay *= 2
		}
	}
}

// AcquireLock: path에 대한 논블로킹 파일 잠금 공개 API(v0.1 session DB — internal/session,
// 설계 §8 예외 2건 중 1건). shared=false는 exclusive, shared=true는 shared — 실패는
// lockStore와 달리 재시도 없이 즉시 error로 반환한다(호출자가 재시도 정책을 정한다).
// unix/windows 구현은 각각 store_lock_unix.go/store_lock_windows.go의 tryLockFile을
// 그대로 재사용(위 lockStore도 동일 함수를 exclusive 모드로 경유). path의 부모 디렉터리는
// 호출자가 미리 만들어둬야 한다(O_CREATE는 파일만 생성, 디렉터리는 생성하지 않음).
func AcquireLock(path string, shared bool) (release func(), err error) {
	return tryLockFile(path, shared)
}

func Open(dir string, readOnly bool) (*Store, error) {
	return OpenContext(context.Background(), dir, readOnly)
}

// OpenContext: Open과 동일하되 writable 오픈의 open-lock 대기(lockStoreCtx)를 ctx로 관측한다 —
// deadline/취소 시 5s 하드 한도 이전에 ErrUnavailable로 포기시켜 훅 deadline 예산을 지킨다
// (설계 §2.3). readOnly 경로는 잠금 대기가 없어 ctx를 쓰지 않는다. Open은 background ctx로
// 위임해 기존 무기한(5s까지) 대기 동작을 그대로 유지한다.
func OpenContext(ctx context.Context, dir string, readOnly bool) (*Store, error) {
	if !readOnly {
		// 0o700: store 루트+artifacts 모두 이 한 호출로 생성(MkdirAll이 만드는 모든 중간
		// 디렉터리에 동일 perm 적용) — Windows는 Unix perm bit 무시(§10 no-op, 주석만).
		if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
			return nil, sanitizeIOErr("open mkdir", err)
		}
		// migrate()·ledger.db DDL까지 포함해 Open 반환 시점(defer)까지 보유 — 아래 lockStore 주석 참조.
		release, err := lockStoreCtx(ctx, dir)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "content.db")) + pragmas
	wdsn := dsn
	if readOnly {
		dsn += "&mode=ro&_pragma=query_only(ON)"
		wdsn = dsn
	} else {
		wdsn += "&_txlock=immediate" // Register: Begin()/BeginTx()가 BEGIN IMMEDIATE로 즉시 쓰기 락 (설계 §3.5)
	}
	w, err := sql.Open("sqlite", wdsn)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("store open: %w", err)
	}
	r.SetMaxOpenConns(4)
	s := &Store{dir: dir, writer: w, reader: r}
	if !readOnly {
		if err := s.migrate(); err != nil {
			w.Close()
			r.Close()
			return nil, err
		}
		l, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
		if err == nil {
			l.SetMaxOpenConns(1)
			// ledger는 best-effort 보조 DB(Store 계약 미포함, Close와 동일 취급) — 테이블 생성
			// 실패해도 이후 ledger insert들이 그냥 계속 실패할 뿐 Store 본체 동작에는 영향 없다.
			// 그래도 침묵하지는 않는다(릴리스 패스 소견 F11): 이 실패는 이 세션의 사용량 기록을
			// 통째로 없애는데, 로그에 안 남으면 관측할 길 자체가 없다 — MergeFTSIfDue의 스탬프
			// 실패를 경고로 바꾼 것과 같은 판단이다.
			if _, err := l.Exec(`CREATE TABLE IF NOT EXISTS ledger(
				id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
				bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
				duration_ms INTEGER NOT NULL DEFAULT 0)`); err != nil {
				slog.Warn("store: 원장 테이블 생성 실패 — 이 세션의 사용량 기록이 통째로 유실된다", "error", err)
			}
			// D103 계약 1의 세 열(회수 실적 둘 + 소견 F4의 귀속 표식)을 옛 ledger.db에도 붙인다.
			// **없는 열만** 붙이고 실패는 경고로 낸다 — 근거는 migrateLedger의 주석(소견 F11).
			// 셋은 독립 문장이라 일부만 성공한 부분 이관이 여전히 도달 가능하고, 그때 읽는 쪽은
			// PRAGMA table_info로 있는 열만 보고(계약 7) 쓰는 쪽은 아래 ledgerCols로 같은 계단을
			// 탄다 — 둘이 갈리면 소견 F1이다.
			s.ledger = l
			s.ledgerCols = migrateLedger(l)
		}
	}
	return s, nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.writer.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("store migrate: %w", err)
	}
	switch {
	case v == 0:
		if err := s.applySchemaV1(); err != nil {
			return fmt.Errorf("store migrate: %w", err)
		}
	case v == SchemaVersion:
	case v > SchemaVersion:
		return fmt.Errorf("store migrate: db user_version=%d > 지원 %d — 비파괴 거부: %w", v, SchemaVersion, ErrUnavailable)
	default:
		return fmt.Errorf("store migrate: 알 수 없는 하위 버전 %d: %w", v, ErrUnavailable)
	}
	// D73: 색인은 버전 스위치 **밖**에서 적용한다 — 신규(v==0→v1)와 기존(v==1)이 같은 한 번의
	// Open에서 색인까지 도달해야 하고, 버전을 올리지 않아 구 바이너리도 이 DB를 계속 연다.
	s.ensureIndexes()
	return nil
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS artifacts(
  id INTEGER PRIMARY KEY, content_hash TEXT NOT NULL, media_type TEXT NOT NULL,
  byte_length INTEGER NOT NULL, redaction TEXT NOT NULL DEFAULT 'none', created_at INTEGER NOT NULL,
  UNIQUE(content_hash, media_type));
CREATE TABLE IF NOT EXISTS sources(
  uri TEXT PRIMARY KEY, artifact_id INTEGER NOT NULL REFERENCES artifacts(id),
  source_kind TEXT NOT NULL, src_size INTEGER, src_mtime_ns INTEGER, src_hash TEXT,
  raw_blob_hash TEXT, extraction TEXT, indexed_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY, artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  ordinal INTEGER NOT NULL, byte_start INTEGER, byte_end INTEGER, line_start INTEGER, line_end INTEGER,
  title TEXT, text TEXT NOT NULL, UNIQUE(artifact_id, ordinal));
CREATE VIRTUAL TABLE IF NOT EXISTS fts_porter  USING fts5(title, text, content='chunks', content_rowid='id', tokenize='porter unicode61');
CREATE VIRTUAL TABLE IF NOT EXISTS fts_trigram USING fts5(title, text, content='chunks', content_rowid='id', tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
  INSERT INTO fts_porter(rowid, title, text) VALUES (new.id, new.title, new.text);
  INSERT INTO fts_trigram(rowid, title, text) VALUES (new.id, new.title, new.text);
END;
CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
  INSERT INTO fts_porter(fts_porter, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
  INSERT INTO fts_trigram(fts_trigram, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
END;
CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
  INSERT INTO fts_porter(fts_porter, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
  INSERT INTO fts_porter(rowid, title, text) VALUES (new.id, new.title, new.text);
  INSERT INTO fts_trigram(fts_trigram, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
  INSERT INTO fts_trigram(rowid, title, text) VALUES (new.id, new.title, new.text);
END;
PRAGMA user_version = 1;`

// applySchemaV1 executes schemaV1 inside an explicit transaction so a mid-script
// failure can't leave user_version=0 with only some objects created (partial-schema
// lockout). schemaV1 statements are all idempotent (IF NOT EXISTS) so a retry after
// a rollback — or after a failure under a driver that rejects multi-statement Exec
// inside a Tx — heals cleanly either way.
func (s *Store) applySchemaV1() error {
	tx, err := s.writer.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(schemaV1); err != nil {
		_ = tx.Rollback() // 커밋 전이라 무해 — 원 오류(err)만 반환
		return err
	}
	return tx.Commit()
}

// shadowIndexDDL — D73: shadow 보존 술어(shadowOwnedHashQuery + shadowOwnedFilter의 나이 절)가
// 쓰는 진입 컬럼을 선두로 둔 복합 색인. 술어 지점과의 대응:
//
//	· hook JOIN·첫 NOT EXISTS의 s2 — artifact_id → source_kind
//	· 둘째 NOT EXISTS의 s3        — raw_blob_hash → source_kind
//	· 나이 절의 s4                — artifact_id → indexed_at
//
// a2·a4·ORDER BY의 content_hash는 기존 UNIQUE(content_hash, media_type)의 선두 컬럼이 커버하므로
// 추가하지 않는다. indexed_at 단독 색인은 형제 경로 PurgeOlderThan용이며 이 결정의 범위 밖이다.
var shadowIndexDDL = []struct{ name, ddl string }{
	{"idx_sources_artifact_kind", `CREATE INDEX IF NOT EXISTS idx_sources_artifact_kind ON sources(artifact_id, source_kind)`},
	{"idx_sources_blobhash_kind", `CREATE INDEX IF NOT EXISTS idx_sources_blobhash_kind ON sources(raw_blob_hash, source_kind)`},
	{"idx_sources_artifact_indexed", `CREATE INDEX IF NOT EXISTS idx_sources_artifact_indexed ON sources(artifact_id, indexed_at)`},
}

// ensureIndexes — D73: 버전 스위치 **밖**에서 색인을 적용한다. SchemaVersion을 올리면
// case v > SchemaVersion이 구 바이너리의 writable Open을 영구 거부하고, 스위치 안에 두면
// case v == SchemaVersion이 DDL 앞에서 반환해 기존 DB가 색인을 받지 못한다. 색인 부재는
// 느리지만 정확한 상태로 퇴화하므로(D65 분류의 이중 안전장치) 실패가 Open을 막지 않는다 —
// 색인마다 개별로 내고 첫 실패에서 나머지를 중단하지 않으며, 실패한 색인마다 경고 한 줄을
// 남긴다(따라서 최대 3줄). 실제 적용 상태는 doctor [3]의 indexes 병기가 관측한다.
func (s *Store) ensureIndexes() {
	for _, ix := range shadowIndexDDL {
		if _, err := s.writer.Exec(ix.ddl); err != nil {
			slog.Warn("store: 색인 생성 실패 — 술어는 정확하나 스캔 경계가 넓어진다", "index", ix.name, "error", err)
		}
	}
}

func (s *Store) Reader() *sql.DB { return s.reader }

func (s *Store) Close() error {
	if s.ledger != nil {
		s.ledger.Close() // best-effort: 보조 DB, Store 계약에 미포함
	}
	_, checkpointErr := s.writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	readerErr := s.reader.Close()
	writerErr := s.writer.Close()
	return errors.Join(checkpointErr, readerErr, writerErr)
}

// --- Register / ReadRange 계약 타입 (설계 §3.5, §4.2) ---

type SourceMeta struct {
	URI, Kind                        string
	Size, MtimeNS                    int64
	SrcHash, RawBlobHash, Extraction string
}

type Chunk struct {
	Ordinal            int
	ByteStart, ByteEnd int64
	LineStart, LineEnd int
	Title, Text        string
}

type Registration struct {
	StoredBytes          []byte
	MediaType, Redaction string
	Source               SourceMeta
	Chunks               []Chunk
	ExpectedOldSrcHash   string // ""=신규 허용, 그 외=CAS 조건 (§3.5)
	RawBlob              []byte // nil 아니면 비색인 원본 blob 보존(§4.5) — writeBlob 재사용, chunks/FTS 미포함
}

// Selector.Kind: "chunk"|"line"|"byte"
type Selector struct {
	ChunkID            int64
	LineStart, LineEnd int
	ByteStart, ByteEnd int64
	Kind               string
}

// SourceInfo — sources 1행의 provenance 부분집합(§4.2 ctr_fetch 계약, RangeResult가 담아 반환).
type SourceInfo struct {
	URI, Kind           string
	Size, MtimeNS       int64
	SrcHash, Extraction string
}

type RangeResult struct {
	Text               []byte
	ByteStart, ByteEnd int64
	LineStart, LineEnd int
	Artifact           ArtifactMeta
	Source             SourceInfo
	HasSource          bool
	Stale              bool
}

type ArtifactMeta struct {
	ID                                int64
	ContentHash, MediaType, Redaction string
	ByteLength                        int64
	CreatedAt                         int64
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// sanitizeIOErr — PathError/LinkError의 경로를 벗겨 syscall 원인만 wrap (§5.5: 오류에 절대경로 금지)
func sanitizeIOErr(op string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("store: %s: %w", op, pe.Err)
	}
	var le *os.LinkError // os.Rename은 PathError가 아닌 LinkError(Old/New 경로 포함)를 반환
	if errors.As(err, &le) {
		return fmt.Errorf("store: %s: %w", op, le.Err)
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

var tmpSeq atomic.Uint64 // writeBlob 임시파일명 유일성(동시 Register 충돌 방지)

// writeBlob: artifacts/<h[:2]>/<h>에 원자적으로 기록 — 임시파일→fsync→rename.
// 대상이 이미 존재해도 os.Rename이 덮어써 no-op처럼 동작(동일 content_hash라 내용은 항상 동일).
func (s *Store) writeBlob(hash string, data []byte) error {
	dir := filepath.Join(s.dir, "artifacts", hash[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil { // Windows: perm bit 무시(no-op)
		return sanitizeIOErr("blob mkdir", err)
	}
	tmp := filepath.Join(dir, fmt.Sprintf("%s.tmp.%d.%d", hash, os.Getpid(), tmpSeq.Add(1)))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return sanitizeIOErr("blob write", err)
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		os.Remove(tmp)
		return sanitizeIOErr("blob write", errors.Join(werr, serr, cerr))
	}
	if err := os.Rename(tmp, filepath.Join(dir, hash)); err != nil {
		os.Remove(tmp)
		return sanitizeIOErr("blob rename", err)
	}
	return nil
}

// readBlob: content_hash 전체를 메모리로 로드.
// ponytail: 부분 읽기(os.File.ReadAt) 없이 전체 로드 — blob이 커져 문제되면 그때 스트리밍으로 바꾼다.
func (s *Store) readBlob(hash string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, "artifacts", hash[:2], hash))
	if err != nil {
		return nil, sanitizeIOErr("blob read", err)
	}
	return b, nil
}

// txRetry: BEGIN IMMEDIATE(§3.5, DSN _txlock=immediate) 트랜잭션 1개로 fn 실행.
// BUSY/LOCKED면 트랜잭션 전체를 지수 백오프(50/200/800ms, ctx 존중)로 최대 3회 재시도.
func (s *Store) txRetry(ctx context.Context, fn func(tx *sql.Tx) error) error {
	delays := [3]time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 800 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		err := s.runTx(ctx, fn)
		if err == nil || !isBusy(err) {
			return err
		}
		if attempt >= len(delays) {
			return fmt.Errorf("store txRetry: 재시도 소진: %w", ErrUnavailable)
		}
		select {
		case <-time.After(delays[attempt]):
		case <-ctx.Done():
			return fmt.Errorf("store txRetry: %w", ctx.Err())
		}
	}
}

func (s *Store) runTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() // 커밋 전이라 무해 — 원 오류(err)만 반환
		return err
	}
	return tx.Commit()
}

// isBusy: SQLITE_BUSY(5)/SQLITE_LOCKED(6) 여부 — 확장 코드는 하위 8비트가 기본 코드와 같다(SQLite 불변식).
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == 5 || code == 6
}

// Register: blob을 먼저 원자 배치(DB 커밋 전)한 뒤 BEGIN IMMEDIATE 단일 트랜잭션으로
// artifact SELECT-or-INSERT → chunks 전량 INSERT → sources CAS/upsert (설계 §3.5).
func (s *Store) Register(ctx context.Context, reg Registration) (int64, error) {
	sum := sha256.Sum256(reg.StoredBytes)
	contentHash := hex.EncodeToString(sum[:])
	// 롤백 시 blob은 남을 수 있음 — 콘텐츠 주소라 무해하며 purge --gc가 정리 (설계 §3.3)
	if err := s.writeBlob(contentHash, reg.StoredBytes); err != nil { // DB 커밋 전 배치 (§3.5)
		return 0, err
	}
	rawBlobHash := ""
	if reg.RawBlob != nil {
		rsum := sha256.Sum256(reg.RawBlob)
		rawBlobHash = hex.EncodeToString(rsum[:])
		if err := s.writeBlob(rawBlobHash, reg.RawBlob); err != nil {
			return 0, err
		}
	}
	var artID int64
	err := s.txRetry(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow("SELECT id FROM artifacts WHERE content_hash=? AND media_type=?", contentHash, reg.MediaType).Scan(&artID); err == sql.ErrNoRows {
			res, err := tx.Exec("INSERT INTO artifacts(content_hash,media_type,byte_length,redaction,created_at) VALUES(?,?,?,?,?)",
				contentHash, reg.MediaType, len(reg.StoredBytes), reg.Redaction, time.Now().Unix())
			if err != nil {
				return err
			}
			artID, _ = res.LastInsertId()
			for _, c := range reg.Chunks {
				if _, err := tx.Exec(`INSERT INTO chunks(artifact_id,ordinal,byte_start,byte_end,line_start,line_end,title,text)
					VALUES(?,?,?,?,?,?,?,?)`, artID, c.Ordinal, c.ByteStart, c.ByteEnd, c.LineStart, c.LineEnd, c.Title, c.Text); err != nil {
					return err
				}
			}
		} else if err != nil {
			return err
		}
		// sources CAS upsert (§3.5)
		if reg.ExpectedOldSrcHash != "" {
			res, err := tx.Exec(`UPDATE sources SET artifact_id=?,source_kind=?,src_size=?,src_mtime_ns=?,src_hash=?,raw_blob_hash=?,extraction=?,indexed_at=?
				WHERE uri=? AND src_hash=?`,
				artID, reg.Source.Kind, reg.Source.Size, reg.Source.MtimeNS, reg.Source.SrcHash,
				nullIfEmpty(rawBlobHash), nullIfEmpty(reg.Source.Extraction), time.Now().Unix(),
				reg.Source.URI, reg.ExpectedOldSrcHash)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("register %s: CAS 불일치: %w", reg.Source.Kind, ErrConflict)
			}
			return nil
		}
		_, err := tx.Exec(`INSERT INTO sources(uri,artifact_id,source_kind,src_size,src_mtime_ns,src_hash,raw_blob_hash,extraction,indexed_at)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(uri) DO UPDATE SET artifact_id=excluded.artifact_id,source_kind=excluded.source_kind,
			  src_size=excluded.src_size,src_mtime_ns=excluded.src_mtime_ns,src_hash=excluded.src_hash,
			  indexed_at=excluded.indexed_at,raw_blob_hash=excluded.raw_blob_hash,extraction=excluded.extraction`,
			reg.Source.URI, artID, reg.Source.Kind, reg.Source.Size, reg.Source.MtimeNS, reg.Source.SrcHash,
			nullIfEmpty(rawBlobHash), nullIfEmpty(reg.Source.Extraction), time.Now().Unix())
		return err
	})
	return artID, err
}

// snapUTF8: [start,end)을 UTF-8 문자 경계로 스냅한다. start는 RuneStart까지 후퇴하고,
// 그 지점부터 유효한 룬을 하나씩 전진 소비하며 end를 넘지 않는 지점에서 멈춘다 — 잘린
// 멀티바이트나 손상된 바이트를 절대 포함하지 않으므로 임의 바이트 입력에도 panic 없이
// 항상 유효한 UTF-8 부분열을 반환한다(FuzzSnapUTF8 불변식).
func snapUTF8(data []byte, start, end int64) (int64, int64) {
	n := int64(len(data))
	if start < 0 {
		start = 0
	} else if start > n {
		start = n
	}
	if end < start {
		end = start
	} else if end > n {
		end = n
	}
	for start > 0 && start < n && !utf8.RuneStart(data[start]) {
		start--
	}
	pos := start
	for pos < end {
		r, size := utf8.DecodeRune(data[pos:])
		if size == 0 || (r == utf8.RuneError && size <= 1) || pos+int64(size) > end {
			break
		}
		pos += int64(size)
	}
	return start, pos
}

// countLines: data의 실제 줄 수(마지막 줄이 개행으로 끝나도 그 뒤의 빈 phantom 줄은
// 세지 않는다 — "abc\n"은 1줄, "abc"도 1줄, ""은 0줄).
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 1
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	if data[len(data)-1] == '\n' {
		n--
	}
	return n
}

// lineByteRange: 1-based [lineStart,lineEnd] 줄 구간의 바이트 범위. 각 줄은 자신의 개행문자를
// 포함하며(마지막 줄에 개행이 없으면 데이터 끝까지), 범위를 벗어나면 빈 구간을 반환한다.
// 호출 전 ReadRange가 countLines로 범위를 엄격 검증하므로(α2) 여기서는 방어적으로만 clamp한다.
func lineByteRange(data []byte, lineStart, lineEnd int) (int64, int64) {
	starts := []int64{0}
	for i, b := range data {
		if b == '\n' {
			starts = append(starts, int64(i+1))
		}
	}
	lo := lineStart - 1
	if lo < 0 {
		lo = 0
	}
	if lo >= len(starts) {
		return int64(len(data)), int64(len(data))
	}
	begin := starts[lo]
	end := int64(len(data))
	if lineEnd < len(starts) {
		end = starts[lineEnd]
	}
	if end < begin { // 방어: 역전 방지(slice panic 회피)
		end = begin
	}
	return begin, end
}

// readChunk: chunk 저장 좌표로 blob을 읽는다. 좌표가 없으면 chunks.text로 대체한다 (§3.5).
// chunk_id 자체가 없으면 ErrNotFound, 있지만 다른 artifact 소속이면 ErrInvalidSelector(α2).
func (s *Store) readChunk(res *RangeResult, artifactID, chunkID int64) error {
	var gotArtifactID int64
	var bs, be, ls, le sql.NullInt64
	var text string
	err := s.reader.QueryRow(`SELECT artifact_id,byte_start,byte_end,line_start,line_end,text FROM chunks WHERE id=?`,
		chunkID).Scan(&gotArtifactID, &bs, &be, &ls, &le, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store ReadRange: chunk 없음: %w", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store ReadRange: %w", err)
	}
	if gotArtifactID != artifactID {
		return fmt.Errorf("store ReadRange: chunk %d은 artifact %d 소속 아님: %w", chunkID, artifactID, ErrInvalidSelector)
	}
	res.LineStart, res.LineEnd = int(ls.Int64), int(le.Int64)
	// 좌표 미상 chunk: ByteStart=0은 실제 오프셋이 아님 (text 폴백 경로)
	if !bs.Valid || !be.Valid {
		res.Text = []byte(text)
		res.ByteEnd = int64(len(res.Text))
		return nil
	}
	blob, err := s.readBlob(res.Artifact.ContentHash)
	if err != nil {
		return err
	}
	res.ByteStart, res.ByteEnd = bs.Int64, be.Int64
	res.Text = blob[res.ByteStart:res.ByteEnd]
	return nil
}

// sourceOf: artifactID의 sources 중 대표 1행 — D37 kind-티어 우선(명시 ingest > hook 패시브),
// 티어 내 uri ASC(search.hitQuery와 동일 순서 — α6). 없으면 ok=false.
func (s *Store) sourceOf(artifactID int64) (SourceInfo, bool) {
	var info SourceInfo
	var size, mtimeNS sql.NullInt64
	var srcHash, extraction sql.NullString
	err := s.reader.QueryRow(`SELECT uri,source_kind,src_size,src_mtime_ns,src_hash,extraction
		FROM sources WHERE artifact_id=? ORDER BY (source_kind = 'hook') ASC, uri ASC LIMIT 1`, artifactID).
		Scan(&info.URI, &info.Kind, &size, &mtimeNS, &srcHash, &extraction)
	if err != nil {
		return SourceInfo{}, false
	}
	info.Size, info.MtimeNS = size.Int64, mtimeNS.Int64
	info.SrcHash, info.Extraction = srcHash.String, extraction.String
	return info, true
}

// StaleOf: source_kind!="file"이면 항상 false(설계 §3.6). file이면 os.Stat으로 size/mtime_ns를
// info와 비교하고, 불일치 시 원본을 재해시해 SrcHash와 대조한다(content_hash는 저장본 주소라
// 원본 대조에 미사용). Stat 실패도 stale=true.
func StaleOf(info SourceInfo) bool {
	if info.Kind != "file" {
		return false
	}
	p := filepath.FromSlash(info.URI)
	fi, err := os.Stat(p)
	if err != nil {
		return true
	}
	if fi.Size() == info.Size && fi.ModTime().UnixNano() == info.MtimeNS {
		return false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return true
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]) != info.SrcHash
}

// ArtifactText: artifactID의 저장 콘텐츠 전체를 문자열로 반환한다(ctr_transform 입력 로더,
// 설계 §4.2.3). byte_length(메타데이터)가 maxBytes를 넘으면 blob을 읽지 않고 즉시
// ErrInvalidSelector로 거부한다. maxBytes<=0이면 상한 미적용.
func (s *Store) ArtifactText(ctx context.Context, artifactID int64, maxBytes int64) (string, error) {
	var contentHash string
	var byteLength int64
	err := s.reader.QueryRowContext(ctx, "SELECT content_hash,byte_length FROM artifacts WHERE id=?", artifactID).
		Scan(&contentHash, &byteLength)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store ArtifactText: artifact 없음: %w", ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store ArtifactText: %w", err)
	}
	if maxBytes > 0 && byteLength > maxBytes {
		return "", fmt.Errorf("store ArtifactText: byte_length=%d > maxBytes=%d: %w", byteLength, maxBytes, ErrInvalidSelector)
	}
	blob, err := s.readBlob(contentHash)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

// ArtifactHashByID: artifacts.content_hash 단일 조회(v0.1 session DB — internal/session,
// 설계 §8 예외 2건 중 1건 — artifact 참조 무결성 확인용). 미존재 id는 ErrNotFound.
func (s *Store) ArtifactHashByID(ctx context.Context, id int64) (string, error) {
	var hash string
	err := s.reader.QueryRowContext(ctx, "SELECT content_hash FROM artifacts WHERE id=?", id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store ArtifactHashByID: artifact 없음: %w", ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store ArtifactHashByID: %w", err)
	}
	return hash, nil
}

// ReadRange: Selector.Kind 하나로 chunk 저장 좌표, blob 라인 스캔, blob UTF-8 스냅 바이트
// 구간 중 하나를 읽는다 (설계 §3.5).
func (s *Store) ReadRange(ctx context.Context, artifactID int64, sel Selector) (RangeResult, error) {
	switch sel.Kind {
	case "chunk", "byte", "line":
	default:
		return RangeResult{}, fmt.Errorf("store ReadRange: kind=%q: %w", sel.Kind, ErrInvalidSelector)
	}
	var meta ArtifactMeta
	err := s.reader.QueryRowContext(ctx, `SELECT id,content_hash,media_type,redaction,byte_length,created_at
		FROM artifacts WHERE id=?`, artifactID).
		Scan(&meta.ID, &meta.ContentHash, &meta.MediaType, &meta.Redaction, &meta.ByteLength, &meta.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RangeResult{}, fmt.Errorf("store ReadRange: artifact 없음: %w", ErrNotFound)
	}
	if err != nil {
		return RangeResult{}, fmt.Errorf("store ReadRange: %w", err)
	}
	res := RangeResult{Artifact: meta}
	switch sel.Kind {
	case "chunk":
		if err := s.readChunk(&res, artifactID, sel.ChunkID); err != nil {
			return RangeResult{}, err
		}
	case "line":
		blob, err := s.readBlob(meta.ContentHash)
		if err != nil {
			return RangeResult{}, err
		}
		lc := countLines(blob)
		if sel.LineStart < 1 || sel.LineEnd < sel.LineStart || sel.LineStart > lc || sel.LineEnd > lc {
			return RangeResult{}, fmt.Errorf("store ReadRange: line 범위 잘못됨 start=%d end=%d (valid: 1..%d): %w",
				sel.LineStart, sel.LineEnd, lc, ErrInvalidSelector)
		}
		res.ByteStart, res.ByteEnd = lineByteRange(blob, sel.LineStart, sel.LineEnd)
		res.Text = blob[res.ByteStart:res.ByteEnd]
		res.LineStart, res.LineEnd = sel.LineStart, sel.LineEnd
	case "byte":
		blob, err := s.readBlob(meta.ContentHash)
		if err != nil {
			return RangeResult{}, err
		}
		n := int64(len(blob))
		if sel.ByteStart < 0 || sel.ByteEnd <= sel.ByteStart || sel.ByteStart >= n || sel.ByteEnd > n {
			return RangeResult{}, fmt.Errorf("store ReadRange: byte 범위 잘못됨 start=%d end=%d (valid: 0..%d): %w",
				sel.ByteStart, sel.ByteEnd, n, ErrInvalidSelector)
		}
		res.ByteStart, res.ByteEnd = snapUTF8(blob, sel.ByteStart, sel.ByteEnd)
		res.Text = blob[res.ByteStart:res.ByteEnd]
	}
	if info, ok := s.sourceOf(artifactID); ok {
		res.Source, res.HasSource = info, true
		res.Stale = StaleOf(info)
	}
	return res, nil
}

// PurgeOlderThan: sources.indexed_at < cutoffUnix인 행을 삭제하고, 그 결과 어떤 source도
// 참조하지 않게 된 artifacts와 그 chunks를 같은 트랜잭션(txRetry)에서 삭제한다 — chunks를
// artifacts보다 먼저 명시적으로 지워 AFTER DELETE 트리거가 정상 발화해 FTS를 동기화한다
// (설계 §7 purge 선택 삭제). 커밋 후 fts_porter/fts_trigram 양쪽에 integrity-check를 실행해
// 실패하면 오류로 보고한다(게이트 6) — 이 시점에는 이미 커밋된 뒤라 롤백은 없다.
func (s *Store) PurgeOlderThan(ctx context.Context, cutoffUnix int64) (sources, artifacts int64, err error) {
	err = s.txRetry(ctx, func(tx *sql.Tx) error {
		res, txErr := tx.ExecContext(ctx, "DELETE FROM sources WHERE indexed_at < ?", cutoffUnix)
		if txErr != nil {
			return txErr
		}
		sources, _ = res.RowsAffected()
		if _, txErr = tx.ExecContext(ctx, `DELETE FROM chunks WHERE artifact_id IN
			(SELECT id FROM artifacts WHERE id NOT IN (SELECT artifact_id FROM sources))`); txErr != nil {
			return txErr
		}
		res, txErr = tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id NOT IN (SELECT artifact_id FROM sources)`)
		if txErr != nil {
			return txErr
		}
		artifacts, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if err := s.checkFTSIntegrity(ctx); err != nil {
		return sources, artifacts, err
	}
	return sources, artifacts, nil
}

// checkFTSIntegrity: fts_porter/fts_trigram 양쪽에 SQLite FTS5 integrity-check 특수 명령을
// 실행한다(게이트 6) — chunks와 FTS 인덱스가 어긋나면 실패한다. rank=1을 넘긴다(최종리뷰
// F7) — rank 생략(=0)은 FTS5 내부 구조만 자기검사하고 external-content table(chunks)과의
// 대조는 하지 않는다. rank가 0이 아니면 SQLite가 그 대조까지 수행해 chunks↔인덱스 행/토큰
// 드리프트(예: 트리거 누락으로 인덱스만 갱신 안 된 경우)도 잡아낸다.
func (s *Store) checkFTSIntegrity(ctx context.Context) error {
	for _, fts := range [2]string{"fts_porter", "fts_trigram"} {
		if _, err := s.writer.ExecContext(ctx, "INSERT INTO "+fts+"("+fts+", rank) VALUES('integrity-check', 1)"); err != nil {
			return fmt.Errorf("store: %s integrity-check 실패: %w", fts, err)
		}
	}
	return nil
}

// MergeFTS — D102: fts_porter·fts_trigram의 세그먼트를 하나로 병합한다. 외부 콘텐츠 FTS5의
// 삭제는 tombstone을 새 세그먼트에 쌓기만 하고 automerge(기본 4)가 그것을 따라잡지 못해,
// 병합 없이는 퍼지가 지운 몫이 파일에서 회수되지 않는다 — 실측으로 이 저장소 파일의 75.9%가
// 그렇게 쌓인 것이었다(설계 v0.20 관측 B).
//
// checkFTSIntegrity와 같은 **커밋 후 writer 경로**다. tx 안에서 부르지 않는다 — optimize는
// 자체 트랜잭션을 잡고, 삭제 tx와 한 덩어리로 묶으면 그 tx의 락 보유 시간이 병합 시간만큼
// 늘어난다(D67이 묶어 둔 예산 규율을 깬다).
//
// 원시 명령은 'optimize'다. 'merge=N'은 검토 후 기각됐다 — merge=512 한 번이 실측 churn의
// 1/4~1/18에 그쳐 하루 한 번으로는 수렴하지 못한다(D102 계약 3). 비용 논거가 기대는 성질:
// **이미 한 세그먼트로 병합된 인덱스에 건 optimize는 일 없이 반환한다**(번들 SQLite 3.53.3 /
// modernc.org/sqlite v1.54.0) — 그래서 필요 없는 실행이 싸고, 조정 손잡이(환경 변수)를
// 지금 만들지 않는다.
//
// **둘 다 시도한 뒤 실패를 errors.Join으로 합쳐 반환한다**(최종리뷰 F4) — 앞엣것에서 즉시
// 반환하면 porter에만 지속되는 오류가 trigram의 병합을 영영 막아 그 축만 무한히 자란다.
// 호출자는 이 실패로 기동이나 회수를 막지 않는다 — 병합은 멱등이라 다음 기회에 다시 돌면 된다.
// (errors.Join은 Is/As를 원소로 전개하므로 호출부의 errors.Is(err, context.Canceled) 강등
// 분기는 그대로 산다.)
func (s *Store) MergeFTS(ctx context.Context) error {
	var errs []error
	for _, fts := range [2]string{"fts_porter", "fts_trigram"} {
		if _, err := s.writer.ExecContext(ctx,
			"INSERT INTO "+fts+"("+fts+") VALUES('optimize')"); err != nil {
			errs = append(errs, fmt.Errorf("store: %s optimize 실패: %w", fts, err))
		}
	}
	return errors.Join(errs...)
}

// mergeStampName — D102 계약 2의 "하루 한 번"을 재는 자리. content.db 스키마를 건드리지
// 않으려고 파일 mtime을 쓴다 — 스키마에 넣으면 user_version 축과 구 바이너리 호환을 함께
// 건드려야 하고, 그 값은 이 한 타임스탬프에 비례하지 않는다. 같은 디렉터리에
// content.db.rebuild.lock이 이미 같은 부류로 있다.
const mergeStampName = "fts-merge.stamp"

// MergeFTSIfDue — D102 계약 2: 마지막 병합에서 interval이 지났을 때만 MergeFTS를 돌리고,
// **성공했을 때만** 스탬프를 갱신한다. 돌았으면 true.
//
// 조건은 시간 하나다. 퍼지가 몇 건을 지웠는지는 보지 않는다 — 세그먼트는 삽입으로도 쌓이므로
// 건수 문턱은 삽입만 있고 삭제가 적은 구간에서 병합을 영영 막는다(설계 v0.20 D102 계약 2).
//
// **"돌 때가 됐다"로 읽는 것이 둘이다**: ① 스탬프를 못 읽는 어떤 사유(부재·권한·손상) —
// 병합은 멱등이고, 못 읽어서 영영 안 도는 쪽이 더 나쁜 실패다. ② mtime이 now보다 **미래**인
// 스탬프 — 시계 되돌림이나 복원 뒤에 음수 경과가 나오는데, 그것을 "아직 이르다"로 읽으면
// 그 저장소는 영구히 병합하지 않는다. 반대로 스탬프 **쓰기** 실패는 무시한다: 다음 기회에
// 한 번 더 도는 것이 전부다.
//
// 프로세스 둘이 동시에 기동하면 둘 다 스탬프를 낡은 것으로 보고 병합할 수 있다. 쓰기 락이
// 둘을 직렬화하고 뒤엣것은 이미 한 세그먼트가 된 인덱스에 optimize를 걸어 일 없이 반환한다
// (번들 SQLite 3.53.3 소스 대조, D102 계약 2·3). **다만 그 직렬화는 유한하다**(릴리스 패스 M4):
// DSN의 busy_timeout이 5000 ms라, 앞엣것의 병합이 그보다 오래 걸리면 뒤엣것은 BUSY로 실패한다.
// 무해한 것은 그 실패의 결과다 — 스탬프가 안 찍혀 다음 주기가 그대로 재시도하고, BUSY는
// IsBusyErr로 진짜 결함과 갈라져 로그·안내 문면이 달라진다.
func (s *Store) MergeFTSIfDue(ctx context.Context, interval time.Duration, now time.Time) (bool, error) {
	if s.FTSMergeDueIn(interval, now) > 0 {
		return false, nil
	}
	if err := s.MergeFTS(ctx); err != nil {
		return false, err
	}
	// Chtimes는 생성 성공 분기 안에 둔다(최종리뷰 F12) — 밖에 두면 파일이 없는 상태에서도
	// 부르게 되고, 그 호출은 반드시 실패하므로 하는 일이 없다.
	// 동작은 best-effort 그대로지만 **침묵하지는 않는다**(릴리스 패스 M1): 스탬프를 못 남기면
	// 이 게이트는 조용히 "기동마다 병합"으로 퇴화하는데, 그 퇴화가 로그에 안 남으면 관측할
	// 길이 자체가 없다. 경로는 sanitizeIOErr로 떼고 낸다(§12).
	stamp := filepath.Join(s.dir, mergeStampName)
	if f, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
		slog.Warn("store: FTS 병합 스탬프 생성 실패 — 주기 게이트가 기동마다 병합으로 퇴화한다",
			"error", sanitizeIOErr("merge stamp create", err))
	} else {
		_ = f.Close()
		if err := os.Chtimes(stamp, now, now); err != nil {
			slog.Warn("store: FTS 병합 스탬프 시각 기록 실패 — 주기 판정이 파일 mtime과 어긋난다",
				"error", sanitizeIOErr("merge stamp chtimes", err))
		}
	}
	return true, nil
}

// FTSMergeDueIn — 다음 병합 만기까지 남은 시간. 이미 만기이거나 판정할 수 없으면 0이다
// (스탬프 부재·읽기 실패, 그리고 now보다 미래인 mtime — 위 주석의 "돌 때가 됐다" 둘과 같은
// 산술을 MergeFTSIfDue와 공유한다).
//
// 자동 루프가 **다음 확인 시각**을 이 값으로 잡는다(릴리스 패스 B3). interval로 무조건 리셋하면
// 아직 만기가 아니어서 안 돈 틱 뒤에 다음 확인이 만기를 통째로 지나쳐, 계약 2의 "하루 한 번"이
// 실효 최대 약 48시간이 된다 — 만기가 아닌 것은 병합이지 **확인**이 아니다.
func (s *Store) FTSMergeDueIn(interval time.Duration, now time.Time) time.Duration {
	fi, err := os.Stat(filepath.Join(s.dir, mergeStampName))
	if err != nil {
		return 0
	}
	elapsed := now.Sub(fi.ModTime())
	if elapsed < 0 || elapsed >= interval {
		return 0
	}
	return interval - elapsed
}

// hashesFromQuery: query가 반환하는 문자열 컬럼 1개를 집합으로 모은다(GCOrphanBlobs 전용
// 소소한 헬퍼 — content_hash/raw_blob_hash 두 조회에 재사용).
func hashesFromQuery(ctx context.Context, db *sql.DB, query string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		set[h] = true
	}
	return set, rows.Err()
}

// gcOrphanMinAge: blob 배치→커밋 윈도 보호 — Register는 blob을 DB 커밋 이전에 배치한다
// (§3.5 원자성 계약). 동시 GC가 "DB 참조 없음+파일 존재"인 등록 진행 중 blob을 고아로
// 오판해 지우면 커밋 후 DB가 없는 blob을 가리키게 되어 저장소가 손상된다. mtime이 이보다
// 최근인 파일은 삭제 후보에서 제외한다(git prune --expire 선례와 동일한 발상) — 크래시로
// 남은 진짜 고아는 다음 GC 실행에서(이 유예 기간이 지난 뒤) 수거된다.
const gcOrphanMinAge = time.Hour

// GCOrphanBlobs: artifacts/ 아래 blob 파일 중 artifacts.content_hash에도 sources.raw_blob_hash
// (NULL 아닌 것)에도 없는 해시만 삭제한다(계획2 이월 "과거 raw blob 물리 GC" 해소, 설계 §7).
// reader로 참조 해시 집합을 모은 뒤 os.ReadDir(artifacts/<prefix>/)로 대조한다. 파일명 길이가
// sha256 hex(64자)가 아니면 blob이 아니라 writeBlob의 임시파일(hash.tmp.pid.seq)이므로 건드리지
// 않는다. mtime이 gcOrphanMinAge보다 최근인 미참조 파일은 건너뛴다(위 불변식 참조).
//
// 삭제 수행 전 lockStore(s.dir)를 획득한다(최종리뷰 F2) — age gate(1h)는 크래시로 남은 고아를
// 늦게 수거할 뿐, Register가 blob을 배치(writeBlob)하고 커밋하는 그 짧은 창과 GC가 우연히
// 겹치는 경우까지는 못 막는다: GC가 "DB 참조 없음"으로 읽은 직후 Register가 커밋해버리면 GC는
// 방금 참조된 blob을 여전히 고아로 보고 지울 수 있어(파일 삭제는 트랜잭션 밖) DB가 없는 blob을
// 가리키는 손상이 생긴다. Register 자체의 쓰기 트랜잭션은 writer 직렬화(SetMaxOpenConns(1))로
// 보호되지만 blob 물리 삭제는 그 보호 밖이므로, GC와 blob 배치를 같은 프로세스간 잠금
// (lockStore)으로 직렬화하는 것이 근본 폐쇄다. age gate는 이중방어로 그대로 유지한다.
func (s *Store) GCOrphanBlobs(ctx context.Context) (removed int64, err error) {
	release, err := lockStore(s.dir)
	if err != nil {
		return 0, fmt.Errorf("store GCOrphanBlobs: %w", err)
	}
	defer release()

	referenced, err := hashesFromQuery(ctx, s.reader, "SELECT content_hash FROM artifacts")
	if err != nil {
		return 0, fmt.Errorf("store GCOrphanBlobs: %w", err)
	}
	rawReferenced, err := hashesFromQuery(ctx, s.reader, "SELECT raw_blob_hash FROM sources WHERE raw_blob_hash IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("store GCOrphanBlobs: %w", err)
	}
	for h := range rawReferenced {
		referenced[h] = true
	}

	blobRoot := filepath.Join(s.dir, "artifacts")
	prefixes, err := os.ReadDir(blobRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, sanitizeIOErr("gc readdir", err)
	}
	for _, p := range prefixes {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return removed, ctxErr
		}
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(blobRoot, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return removed, sanitizeIOErr("gc readdir", err)
		}
		for _, e := range entries {
			name := e.Name()
			// 크래시로 남은 격리본(<64hex>.purging): reclaimHookBlobs가 rename↔remove 사이에 죽으면
			// DB 행은 이미 삭제돼 hash가 다시 선택되지 않으므로 이 파일은 영구 고아가 된다(최종리뷰 P2).
			// hash 참조 대상이 아니므로 참조 대조 없이 age gate만 적용해 수거한다(진행 중 격리는 이미
			// lockStore로 배제됨 — age gate는 이중방어). 정상 blob은 len==64 + 미참조일 때만 후보.
			isPurging := len(name) == 64+len(".purging") && strings.HasSuffix(name, ".purging")
			if !isPurging && (len(name) != 64 || referenced[name]) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) { // 대조 시점 사이 이미 사라짐 — 정상
					continue
				}
				return removed, sanitizeIOErr("gc stat", err)
			}
			if time.Since(info.ModTime()) < gcOrphanMinAge {
				continue // age gate: 등록/회수 진행 중일 수 있음(§3.5) — 다음 GC로 미룬다
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return removed, sanitizeIOErr("gc remove", err)
			}
			removed++
		}
	}
	return removed, nil
}

// LedgerAppend: best-effort 사용량 기록 — ledger 없음/오류는 무시(§3.5).
// 기존 호출부 아홉 곳(전부 internal/mcp)의 시그니처를 유지하려고 남긴 얇은 위임이다.
func (s *Store) LedgerAppend(tool string, stored, returned, ms int64) {
	s.LedgerAppendContext(context.Background(), tool, stored, returned, ms)
}

// LedgerAppendContext — D103 계약 8: ctx를 ExecContext로 넘기는 원장 기록. **훅이 부르는
// 경로가 이것이다**: ledger.db의 busy_timeout은 5000 ms인데 훅의 총예산은 2000 ms라
// (internal/hook/hook.go의 defaultDeadlineMS·deadline) ctx 없이 쓰면 훅 프로세스가 겹칠 때 그
// INSERT가 예산 밖에서 블록된다. 예산 초과는 오류로 돌아오고 best-effort라 삼켜진다 — 훅의
// fail-open이 유지된다.
func (s *Store) LedgerAppendContext(ctx context.Context, tool string, stored, returned, ms int64) {
	if s.ledger == nil {
		return
	}
	_, _ = s.ledger.ExecContext(ctx,
		`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms) VALUES(?,?,?,?,?)`,
		time.Now().Unix(), tool, stored, returned, ms)
}

// lastIndexedAtByHashQuery — 나이 시계와 shadow 귀속 여부를 **한 왕복에** 낸다.
// 귀속 술어를 손으로 다시 쓰지 않고 퍼지가 쓰는 그 상수(shadowOwnedHashQuery)를 EXISTS로
// 감싸는 것이 요지다(소견 F4): 정의가 갈리는 순간 나이 분포가 보존 창이 실제로 지우는 집합과
// 다른 모집단을 재게 된다. EXISTS 안의 `a`는 바깥 `a`를 가리는 별개 별칭이라 상관 참조가
// 아니고, 그래서 같은 hash를 인자로 한 번 더 바인딩한다(두 `?`는 같은 값).
// 집계라 소스가 한 행도 없어도 1행이 오는데 그때 두 값은 NULL일 수 있다 — 호출부가 NULL을
// (0, false)로 받는다.
const lastIndexedAtByHashQuery = `SELECT max(s.indexed_at), EXISTS(` + shadowOwnedHashQuery + `
  AND a.content_hash = ?)
	FROM sources s JOIN artifacts a ON a.id = s.artifact_id
	WHERE a.content_hash = ?`

// LastIndexedAtByHash — D103 계약 2: 이 콘텐츠의 **마지막 포착** 시각(unix 초)과, 소견 F4의
// **shadow 귀속 여부**. 시계도 범위도 D67 퍼지와 같은 것을 쓴다 — 퍼지 술어
// (shadowOwnedFilter의 cutoffUnix 분기가 붙이는 나이 절)가 같은 content_hash를 가진 모든 artifact의
// 모든 소스에 대해 indexed_at을 보므로, 나이도 그렇게 재야 분포가 보존 창 위에 그대로
// 겹쳐진다. artifact 단위로 재면 형제가 방금 재포착된 아티팩트가 실제보다 늙어 보이고 그
// 오차는 창을 늘리는 쪽으로만 작용한다. 소스가 없으면 (0, false, nil).
// **귀속 여부를 회수 시점에 함께 내는 이유**: explicit 아티팩트는 퍼지 대상이 아니라 영원히
// 남으므로 그 회수 나이는 창의 길이에 대해 아무 말도 하지 않는데, 나중에는 아티팩트 자체가
// 없어 되물을 수 없다. 오류일 때 호출부가 받는 false는 "explicit이다"가 아니라 "모른다"이고,
// 모든 귀속 제한 질의가 그 행을 똑같이 뺀다.
// 색인: artifacts는 UNIQUE(content_hash, media_type)의 선두 컬럼이, sources는
// idx_sources_artifact_indexed(artifact_id, indexed_at)가 각각 덮는다.
func (s *Store) LastIndexedAtByHash(ctx context.Context, contentHash string) (int64, bool, error) {
	var ts sql.NullInt64
	var owned sql.NullBool
	if err := s.reader.QueryRowContext(ctx, lastIndexedAtByHashQuery, contentHash, contentHash).
		Scan(&ts, &owned); err != nil {
		return 0, false, fmt.Errorf("store LastIndexedAtByHash: %w", err)
	}
	return ts.Int64, owned.Bool, nil // NullInt64/NullBool의 무효값은 0/false다
}

// LedgerAppendFetch — D103: ctr_fetch 전용 원장 기록. artifactID<=0이면 artifact_id를 NULL로,
// artifact_age_s를 **−1**로 남겨 **해소되지 않은 회수**를 기록한다 — 그것이 "창이 짧아서 못
// 찾았다"의 유일한 직접 증거이고, 성공 기록만 남기면 영원히 나오지 않는다(계약 3).
// −1인 이유: ALTER가 남긴 레거시 행은 두 열이 다 NULL이라 그 둘을 값으로 갈라야 한다(계약 1).
// shadowOwned는 **해소 행에만** 값으로 남는다(소견 F4) — 미해소 행은 아티팩트가 없어 귀속을
// 물을 수 없으므로 NULL이다. 0으로 적으면 "explicit이었다"는 거짓 진술이 되고, 그 거짓이
// 나중에 창과 무관한 회수를 창의 증거로 만든다.
// ageS가 **nil이면 나이 미상**이고 NULL로 적는다(소견 F6) — 0으로 적으면 "방금 포착한 것을
// 같은 초에 회수했다"와 구분되지 않아 분포를 아래로 끌어내린다. 그 행도 해소로는 센다(바이트를
// 실제로 돌려줬다). *int64인 이유: 아키텍처 문서 "부패 방지 계약"의 mcp god package 방지
// 항목이 internal/mcp의 `database/sql` import를 금지해 sql.NullInt64를 호출부에 둘 수 없다.
// ctx를 받는 이유는 계약 8과 같다 — 형제 LedgerAppendContext처럼 호출부의 예산을 탄다.
// LedgerAppend와 같은 best-effort 계약이다(ledger 없음·오류는 무시). S4: 정수와 도구 이름만
// 담는다 — 선택자도 경로도 내용도 담지 않는다(계약 6).
// **부분 이관 원장에서는 있는 열까지만 적고 퇴화한다**(릴리스 패스 소견 F1) — 계단은 아래
// switch에 있고, 읽는 쪽 LedgerFetchStats의 계단과 같은 자리에서 갈린다.
func (s *Store) LedgerAppendFetch(ctx context.Context, returned, ms, artifactID int64, ageS *int64, shadowOwned bool) {
	if s.ledger == nil {
		return
	}
	var idCol, owned any  // nil = NULL
	age := any(int64(-1)) // 미해소 표식
	if artifactID > 0 {
		idCol, owned = artifactID, shadowOwned
		age = nil // 나이 미상
		if ageS != nil {
			age = *ageS
		}
	}
	// 있는 열만 지명한다 — 계단은 읽는 쪽(LedgerFetchStats)의 것과 **같은 둘**이다(소견 F1).
	// 갈라 두면 부분 이관 원장에서 INSERT가 통째로 거절되고 best-effort 관례가 그 오류를 삼켜
	// 해소도 미해소도 한 행 없이 사라지는데, 읽는 쪽은 그 계단에서 OutcomeOK를 세우므로 `stats`가
	// 그 0을 측정값으로 렌더한다.
	switch {
	case !s.ledgerCols["artifact_id"] || !s.ledgerCols["artifact_age_s"]:
		// 결과를 적을 두 열이 없다. 판정의 축은 artifact_id이므로 하나만 있어도 결과는 못 적는다 —
		// artifact_age_s만 적어 두면 나중에 열이 채워졌을 때 해소 행이 **미해소로 읽힌다**(계약 1).
		// 구 바이너리와 같은 다섯 열로 남기면 이관 뒤 레거시로 읽히는데, 그것이 정확한 진술이다.
		s.LedgerAppendContext(ctx, "ctr_fetch", 0, returned, ms)
	case !s.ledgerCols["shadow_owned"]:
		// 셋째 ALTER 이전 계단. 해소/미해소는 그대로 측정값이고(읽는 쪽도 OutcomeOK를 세운다),
		// 귀속 미상은 NULL로 남아 fetchAgeBasis의 `shadow_owned=1`이 자연히 뺀다 — 나중에 열이
		// 붙어도 이 행들은 분위수에 안 든다. 과소 계수이지 거짓 양성이 아니다.
		_, _ = s.ledger.ExecContext(
			ctx,
			`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms,artifact_id,artifact_age_s)
			 VALUES(?,'ctr_fetch',0,?,?,?,?)`,
			time.Now().Unix(), returned, ms, idCol, age,
		)
	default:
		_, _ = s.ledger.ExecContext(
			ctx,
			`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms,artifact_id,artifact_age_s,shadow_owned)
			 VALUES(?,'ctr_fetch',0,?,?,?,?,?)`,
			time.Now().Unix(), returned, ms, idCol, age, owned,
		)
	}
}

// ToolStat: ledger.db 도구별 집계 1행(설계 §6 stats local 계약). FirstTS/LastTS는
// LedgerAppend가 기록하는 unix 초 단위(time.Now().Unix())다.
type ToolStat struct {
	Tool                       string
	Calls                      int64
	BytesStored, BytesReturned int64
	FirstTS, LastTS            int64
}

// LedgerStats: dir/ledger.db를 read-only로 열어 도구별 사용량을 집계한 뒤 닫는다(설계 §6). ledger.db
// 미존재(os.ErrNotExist — io/fs.ErrNotExist와 동일값)는 오류가 아니다 — LedgerAppend와 동일하게
// ledger를 best-effort 보조 산출물로 취급해 빈 슬라이스+nil을 반환한다(os.Stat 선판정, 없는
// 파일을 sql.Open이 새로 만들지 않도록). 그 외 os.Stat 오류(권한 등)는 진짜 문제이므로 삼키지
// 않고 반환한다 — sanitizeIOErr로 절대경로는 벗기고 원인만 남긴다(리뷰 Fix Round 3, item 3).
func LedgerStats(dir string) ([]ToolStat, error) {
	path := filepath.Join(dir, "ledger.db")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, sanitizeIOErr("ledger stat", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store LedgerStats: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT tool, COUNT(*), SUM(bytes_stored), SUM(bytes_returned), MIN(ts), MAX(ts)
		FROM ledger GROUP BY tool ORDER BY tool`)
	if err != nil {
		return nil, fmt.Errorf("store LedgerStats: %w", err)
	}
	defer rows.Close()

	var out []ToolStat
	for rows.Next() {
		var st ToolStat
		if err := rows.Scan(&st.Tool, &st.Calls, &st.BytesStored, &st.BytesReturned, &st.FirstTS, &st.LastTS); err != nil {
			return nil, fmt.Errorf("store LedgerStats: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store LedgerStats: %w", err)
	}
	return out, nil
}

// FetchStat: D103 회수 실적. Calls는 원장의 ctr_fetch 행 전부(레거시 포함)다 — **D104의 채택
// 문턱이 읽는 수가 아니다**: 문턱은 `Resolved + Missed`이고(W2 소유자 판정), 이 열은 이관 전
// 레거시까지 품어 배포 첫날 역사만으로 문턱을 넘긴다. `stats`의 `total` 행도 아니다 — 그 행은
// 이제 M6만 읽는다. Resolved는 artifact를 실제로 돌려준 fetch, Missed는 **artifact 부재**로 끝난
// fetch다(계약 3 — 잘못된 chunk id는 여기 들지 않는다). Age*는 **회수 시점에 박아 둔**
// 나이(초)의 분포 — 아티팩트가 나중에 지워져도 남는다는 것이 이 계측의 요지다(계약 2).
//
// Legacy는 그 Calls 중 이관 전 행(두 열 다 NULL)이다 — 해소에도 미해소에도 들지 않으므로
// Calls를 결과의 분모로 읽으면 그만큼 희석된다(소견 F9).
//
// ShadowResolved는 그 해소 중 **shadow 귀속**(= 퍼지가 실제로 지우는)이면서 **나이가 기록된**
// 행만 센 수이고(fetchAgeBasis 그대로 — 나이 미상은 빠진다, 소견 F6),
// Age*는 그 부분집합에서만 나온다(소견 F4). Resolved와 갈라 두는 이유: 채택 게이트는 도구가
// 쓰이는지를 묻고(모든 해소가 답이다) 창의 길이는 지워질 수 있는 것만 답할 수 있다.
// ShadowArtifacts는 그 같은 모집단의 **distinct artifact_id**이고 D104의 착수 조건이 읽는
// 수다(소견 F5) — ctr_fetch는 기본 16 KiB까지만 돌려주므로 아티팩트 하나가 여러 행이 되고,
// 행으로 세면 페이징 한 번이 조건을 채운다. 둘을 나란히 내면 그 집중이 보인다.
//
// ★ 세 `*OK`는 **어느 수가 측정값인가**를 나른다(SizeStat.PageStatsOK와 같은 관례, 릴리스
// 패스 M3). ledger는 user_version이 없는 best-effort 보조 DB라 세 ALTER가 **아직 안 돈 원장을
// 읽는 상태가 정상적으로 도달 가능**하고(계약 7), 그때 위 수들은 0이 아니라 **못 잰 것**이다.
// 0으로 렌더하면 결정표가 **아무것도 재지 않은 원장을 관측으로 받는다**: 여섯 수가 다 숫자라
// 행 0이 발화하지 못하고 ShadowArtifacts=0이 아니라 `Resolved + Missed = 0`이 행 2로 데려간다.
// 창 판단이 닫히지는 않지만 **처방이 틀린다** — 행 2는 채택을 늘리라고 하는데 할 일은
// 바이너리를 가는 것이고, 그것을 모르면 다음 14일도 아무것도 재지 않는다.
// 그래서 D104는 상태가 셋이다: 충족·불충족, 그리고 **판정 불가**(결정표 행 0 — 숫자가 아닌
// 칸이 하나라도 있으면 그 원장으로는 조건을 판정하지 않는다).
// 계단은 셋이고 단조다: LedgerOK ⊇ OutcomeOK ⊇ ShadowOK.
type FetchStat struct {
	Calls, Legacy          int64
	Resolved, Missed       int64
	ShadowResolved         int64
	ShadowArtifacts        int64
	AgeP50, AgeP90, AgeMax int64

	LedgerOK  bool // ledger 테이블이 있다 → Calls가 측정값 (false면 ledger.db 부재도 포함)
	OutcomeOK bool // artifact_id·artifact_age_s가 있다 → Resolved/Missed/Legacy가 측정값
	ShadowOK  bool // shadow_owned가 있다 → ShadowResolved/ShadowArtifacts·Age*가 측정값
}

// fetchAgeBasis — 나이 분위수와 D104 착수 조건이 **함께** 보는 모집단의 술어. 두 문장이 이
// 상수를 공유하는 이유는 D13이다 — 분모와 분포가 갈리면 어느 쪽도 읽을 수 없다.
// shadow_owned=1로 제한하는 근거는 소견 F4: explicit 아티팩트는 퍼지 대상이 아니라 영원히
// 남으므로 그 회수 나이는 창의 길이에 대해 아무 말도 하지 않는데, 섞이면 "해소 30건"을 채우고
// 분위수까지 지배한다. NULL(미해소·레거시·귀속 미상)은 `=1`이 자연히 뺀다.
// artifact_age_s IS NOT NULL은 소견 F6: 나이를 모르는 해소 행을 뺀다. 오늘은 귀속 조건이
// 그것을 대개 함께 배제하지만(귀속 판정에 hook 소스가 필요하고 그 소스가 곧 시계다) 그 결합은
// 우연이다 — 명시하지 않으면 다음 변경이 조용히 NULL을 정렬 선두로 들여보낸다.
const fetchAgeBasis = `tool='ctr_fetch' AND artifact_id IS NOT NULL AND shadow_owned=1
	AND artifact_age_s IS NOT NULL`

// ledgerColumns: PRAGMA table_info(ledger)의 열 이름 집합. 테이블이 없으면 빈 집합(오류 아님).
// 드라이버 오류 문자열 대조("no such column")를 쓰지 않는 이유: 그 문면은 우리가 통제하지
// 않는 계약이고 드라이버 판마다 바뀐다(설계 v0.20 D103 계약 7).
func ledgerColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(ledger)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// ledgerFetchColumns — D103 계약 1이 원장에 더하는 세 열. 이 순서가 곧 ALTER를 거는 순서다.
// 셋 다 INTEGER라 형은 코드에 한 번만 적는다.
var ledgerFetchColumns = []string{"artifact_id", "artifact_age_s", "shadow_owned"}

// migrateLedger — 세 열 중 **없는 것만** 붙이고, 그 뒤의 실제 열 집합을 낸다. 반환값이 곧
// LedgerAppendFetch의 계단 판정이다(Store.ledgerCols).
//
// 조건 없이 세 ALTER를 던지던 옛 형태를 게이트로 바꾼 이유는 릴리스 패스 소견 F11이다.
// ① 그 형태는 하루 약 295회의 훅 포착 + 서버 기동마다 이미 있는 열에 DDL 셋을 던져 매번
// "duplicate column name"으로 실패했다 — 제품 수명 내내. ② 더 나쁘게, `_, _ =` 관례가
// **진짜 실패**(잠금 경쟁이 busy_timeout을 넘긴 경우 등)와 "이미 이관됨"을 구분 불가능하게
// 만들었다. 그 구분 불가가 소견 F1의 침묵 상태(부분 이관 + 모든 행 유실)에 경고 한 줄 없이
// 도달하는 경로다. ensureIndexes가 색인마다 하듯 열마다 한 줄을 남기고, 첫 실패에서 나머지를
// 멈추지 않는다 — 열은 서로 독립이라 하나가 막혀도 나머지는 붙는 편이 낫다.
//
// 이관 판정에 드라이버 오류 문자열을 쓰지 않는 근거는 ledgerColumns의 주석과 같다(계약 7).
// 원장 전체가 best-effort 보조 DB이므로 실패는 여전히 오류로 **반환하지 않는다** — 경고는
// 반환된 오류가 아니고, 못 붙은 열은 반환 집합에서 빠져 쓰는 쪽이 그만큼 낮은 계단으로 퇴화한다.
func migrateLedger(l *sql.DB) map[string]bool {
	cols, err := ledgerColumns(l)
	if err != nil {
		slog.Warn("store: 원장 스키마 확인 실패 — 회수 실적 열을 이관하지 않는다", "error", err)
		return nil // 모르는 상태에서 ALTER를 던지지 않는다 — 그것이 F11이 없앤 바로 그 동작이다
	}
	if len(cols) == 0 {
		return cols // ledger 테이블 자체가 없다(위 CREATE가 이미 경고를 냈다) — 붙일 곳이 없다
	}
	for _, c := range ledgerFetchColumns {
		if cols[c] {
			continue
		}
		if _, err := l.Exec(`ALTER TABLE ledger ADD COLUMN ` + c + ` INTEGER`); err != nil {
			slog.Warn("store: 원장 열 추가 실패 — 그 열이 나르는 회수 실적을 못 잰다", "column", c, "error", err)
			continue
		}
		cols[c] = true
	}
	return cols
}

// LedgerFetchStats: dir/ledger.db를 read-only로 열어 회수 실적을 낸다.
// 파일 미존재는 LedgerStats와 동일하게 빈 값+nil이다. **새 열이 없는(또는 하나만 있는) 원장도
// 해소/미해소 0 + nil이다** — ALTER는 writable Open에서만 돌므로 이 경로가 이관 전 원장을
// 먼저 만날 수 있고, 그것은 손상이 아니다(설계 v0.20 D103 계약 7). 그 외 오류는 삼키지 않는다.
func LedgerFetchStats(dir string) (FetchStat, error) {
	var fs FetchStat
	path := filepath.Join(dir, "ledger.db")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fs, nil
		}
		return fs, sanitizeIOErr("ledger stat", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return fs, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	defer db.Close()

	cols, err := ledgerColumns(db)
	if err != nil {
		return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	if len(cols) == 0 { // ledger 테이블 자체가 없다 — 이관 전과 같은 빈 값 경로
		return fs, nil // LedgerOK=false: 여기의 0은 "호출 0회"가 아니라 "못 쟀다"다
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM ledger WHERE tool='ctr_fetch'`,
	).Scan(&fs.Calls); err != nil {
		return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	fs.LedgerOK = true
	if !cols["artifact_id"] || !cols["artifact_age_s"] {
		return fs, nil // 이관 전·부분 이관 — 총 호출만 유효하다
	}
	// 계약 1의 표: 레거시는 두 열 다 NULL, 미해소는 age=-1, 해소는 artifact_id가 값
	// (나이는 미상일 수 있다 — 소견 F6). 레거시를 같은 문장에서 세는 이유는 소견 F9다.
	if err := db.QueryRow(`SELECT
			coalesce(sum(artifact_id IS NOT NULL),0),
			coalesce(sum(artifact_id IS NULL AND artifact_age_s IS NOT NULL),0),
			coalesce(sum(artifact_id IS NULL AND artifact_age_s IS NULL),0)
		FROM ledger WHERE tool='ctr_fetch'`).Scan(&fs.Resolved, &fs.Missed, &fs.Legacy); err != nil {
		return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	fs.OutcomeOK = true
	if !cols["shadow_owned"] {
		return fs, nil // 셋째 ALTER 이전 원장 — 귀속으로 제한한 수치는 낼 수 없다(계약 7과 같은 관용)
	}
	if err := db.QueryRow(
		`SELECT count(*), count(DISTINCT artifact_id) FROM ledger WHERE `+fetchAgeBasis,
	).Scan(&fs.ShadowResolved, &fs.ShadowArtifacts); err != nil {
		return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	fs.ShadowOK = true
	if fs.ShadowResolved == 0 {
		return fs, nil
	}
	// 분위수는 SQLite에 내장이 없으므로 정렬 + OFFSET으로 낸다. 행 수가 수만 단위라
	// 이 방식으로 충분하다(하루 약 300행 × 무기한 보존 = 1년 11만 행).
	for _, q := range []struct {
		dst    *int64
		offset int64
	}{
		{&fs.AgeP50, (fs.ShadowResolved - 1) * 50 / 100},
		{&fs.AgeP90, (fs.ShadowResolved - 1) * 90 / 100},
		{&fs.AgeMax, fs.ShadowResolved - 1},
	} {
		if err := db.QueryRow(`SELECT artifact_age_s FROM ledger WHERE `+fetchAgeBasis+`
			ORDER BY artifact_age_s LIMIT 1 OFFSET ?`, q.offset).Scan(q.dst); err != nil {
			return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
		}
	}
	return fs, nil
}

// SizeStat: content.db 규모 스냅샷(doctor [14] shadow 성장 관측 채널, 설계 v0.3 §2·D33).
// BlobBytes는 artifacts/ CAS의 물리 blob 파일 합산 — DB 파일 크기가 아니다.
type SizeStat struct {
	Sources, Artifacts int64
	BlobBytes          int64
	FileBytes          int64            // content.db 본체 파일 실크기(os.Stat) — D40. **live 산출에 쓰지 않는다**(계약 6 개정)
	FreeBytes          int64            // free page × page_size — 회수 가능분(D102 계약 7, 표시 전용)
	LiveBytes          int64            // (page_count-freelist_count) × page_size — D102 계약 6의 live 축 판정값
	PageStatsOK        bool             // 위 두 값이 실제 측정인가 — false면 0은 "없음"이 아니라 "못 쟀다"(릴리스 패스 M3)
	ShadowOwnedBytes   int64            // 귀속 hash들의 물리 CAS 파일 바이트 합 (= ShadowOwned 값 합, 불변)
	ShadowOwnedHashes  int              // 귀속 hash 수 (= len(ShadowOwned), 불변)
	ShadowOwned        map[string]int64 // 귀속 content_hash → 물리 CAS 파일 바이트 — Task 3b/5a/5b 공용 원천
}

// D40 §2: shadow 귀속 content_hash — 그 hash를 직접 참조하는 소스가 전부 kind='hook'이고 hook
// 소스가 1개 이상이며, 어떤 비-hook 소스도 그 hash를 raw_blob_hash로 참조하지 않는 hash. hook JOIN이
// "hook 소스 ≥1"과 "source 0개 → 비귀속"을 함께 보장하고, 두 NOT EXISTS가 직접·raw_blob 비-hook
// 참조를 배제한다. DISTINCT로 다중 미디어/다중 소스 행을 hash 단위로 접는다. 바이트는 호출부가 CAS
// 물리 파일에서 합산(논리 byte_length 아님).
const shadowOwnedHashQuery = `
SELECT DISTINCT a.content_hash
FROM artifacts a
JOIN sources sh ON sh.artifact_id = a.id AND sh.source_kind = 'hook'
WHERE NOT EXISTS (
    SELECT 1 FROM artifacts a2 JOIN sources s2 ON s2.artifact_id = a2.id
    WHERE a2.content_hash = a.content_hash AND s2.source_kind != 'hook')
  AND NOT EXISTS (
    SELECT 1 FROM sources s3
    WHERE s3.raw_blob_hash = a.content_hash AND s3.source_kind != 'hook')`

// SizeStats: dir/content.db를 read-only로 열어 sources·artifacts 행수를 세고, artifacts/ CAS
// 디렉터리의 물리 blob 바이트를 합산한다(설계 v0.3 §2·D33). content.db 미존재는 LedgerStats와
// 동일하게 (nil, nil) — 호출자(doctor [14])가 "없음"으로 fail-soft 렌더한다. 서버가 content.db를
// 동시 점유(WAL 라이터) 중이면 이 ro 열기/조회가 (nil, err)로 실패할 수 있는데, 이 역시 doctor가
// "없음"으로 렌더한다 — 손상이 아니라 일시적 경합이므로 재실행하면 값이 나온다(비관례 직접 open은
// 정보성 진단 경로 한정, fail-soft). blob 바이트는
// GCOrphanBlobs와 동일 관례로 파일명이 sha256 hex(64자)인 것만 센다 — writeBlob 임시파일
// (hash.tmp.pid.seq)은 길이가 달라 자연히 제외된다. dedup은 물리 파일 합산이라 자동(동일
// content_hash는 한 파일).
func SizeStats(dir string) (*SizeStat, error) {
	path := filepath.Join(dir, "content.db")
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, sanitizeIOErr("size stat", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store SizeStats: %w", err)
	}
	defer db.Close()

	st := SizeStat{FileBytes: fi.Size(), ShadowOwned: map[string]int64{}}

	// D102 계약 6·7: live 축(판정값)과 회수 가능분(표시값)을 **한 스냅샷 안에서** 낸다.
	// free page는 삭제가 만들고 이후 기록이 재사용하므로, free가 크다는 것은 "파일이 크다"가
	// 아니라 "지운 만큼이 아직 파일 안에 있다"는 뜻이다. live는 그 반대편 — 병합되지 않은
	// 세그먼트는 free page가 아니라 live page라 freelist는 결함이 있을 때 오히려 낮게 읽힌다.
	//
	// **FileBytes를 빼서 구하지 않는다**(릴리스 패스 B1). 옛 산출 `FileBytes-FreeBytes`는 서로
	// 다른 두 스냅샷을 뺐다: FileBytes는 본체 파일의 os.Stat이고 freelist_count는 WAL 프레임까지
	// 반영한 커밋 스냅샷이다. 자동 병합 경로는 체크포인트를 하지 않으므로(계약 5) 병합 직후에는
	// free가 본체 파일 크기를 넘고, 그러면 live가 음수 → 클램프해 0이 된다 — **스토어가 가장
	// 클 때 경고가 죽는다**(재현: file=4096 wal≈90 MB free≈89.5 MB, live(raw)<0).
	//
	// 세 pragma를 **한 문장**으로 읽는 이유도 같다: 따로 내면 각각이 제 암시 트랜잭션을 열어,
	// 라이브 서버의 커밋이 사이에 끼면 page_count와 freelist_count가 다시 어긋난다.
	// 실패는 삼키지 않고 PageStatsOK=false로 구분한다(릴리스 패스 M3) — 0 하나로는 "free page
	// 없음"과 "못 쟀다"를 호출자가 가를 수 없다. 진단 전체를 실패시키지는 않는다(fail-soft).
	var pageSize, pageCount, freeCount int64
	if err := db.QueryRow(
		"SELECT * FROM pragma_page_size(), pragma_page_count(), pragma_freelist_count()",
	).Scan(&pageSize, &pageCount, &freeCount); err == nil {
		st.FreeBytes = pageSize * freeCount
		st.LiveBytes = pageSize * (pageCount - freeCount)
		st.PageStatsOK = true
	}

	if err := db.QueryRow("SELECT count(*) FROM sources").Scan(&st.Sources); err != nil {
		return nil, fmt.Errorf("store SizeStats: %w", err)
	}
	if err := db.QueryRow("SELECT count(*) FROM artifacts").Scan(&st.Artifacts); err != nil {
		return nil, fmt.Errorf("store SizeStats: %w", err)
	}

	// D40 §2: 귀속 content_hash 집합을 1쿼리로 산출한다. 물리 바이트는 아래 blob walk에서 이 집합
	// 멤버십 파일만 합산 — 논리 byte_length가 아닌 CAS 실파일 크기(귀속 hash는 파일 1개).
	owned := map[string]struct{}{}
	rows, err := db.Query(shadowOwnedHashQuery)
	if err != nil {
		return nil, fmt.Errorf("store SizeStats: %w", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store SizeStats: %w", err)
		}
		owned[h] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store SizeStats: %w", err)
	}
	rows.Close()

	// artifacts/<hex 2자 prefix>/<64-hex> 2단 레이아웃을 GCOrphanBlobs와 동형으로 순회한다.
	blobRoot := filepath.Join(dir, "artifacts")
	prefixes, err := os.ReadDir(blobRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &st, nil // artifacts/ 부재 — blob 0(DB만 있고 blob 미배치인 비정상도 fail-soft)
		}
		return nil, sanitizeIOErr("size readdir", err)
	}
	for _, p := range prefixes {
		if !p.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(blobRoot, p.Name()))
		if err != nil {
			return nil, sanitizeIOErr("size readdir", err)
		}
		for _, e := range entries {
			if len(e.Name()) != 64 { // 64-hex CAS blob만 — 임시파일·기타 제외(GCOrphanBlobs 관례)
				continue
			}
			info, err := e.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) { // 순회 중 사라짐(동시 GC 등) — 무시
					continue
				}
				return nil, sanitizeIOErr("size stat", err)
			}
			st.BlobBytes += info.Size()
			if _, ok := owned[e.Name()]; ok { // D40 §2: 귀속 hash면 물리 파일 크기를 맵·합계에 기록
				st.ShadowOwned[e.Name()] = info.Size()
				st.ShadowOwnedBytes += info.Size()
			}
		}
	}
	st.ShadowOwnedHashes = len(st.ShadowOwned) // 스칼라 = 맵 len (불변)
	return &st, nil
}

// HookPurgeReport — D41 §3: PurgeHookOnly 결과. 행 삭제된 귀속 hash 수와 물리 파일 회수 내역을 병기.
type HookPurgeReport struct {
	Hashes        int   // 행 삭제된 귀속 hash 수
	ReclaimedB    int64 // 실제 unlink된 물리 바이트 합
	DeferredFiles int   // age-gate/교체 감지 유예 건수
	FailedFiles   int   // unlink 실패(orphan 잔존) 건수
}

// shadowOwnedFilter — shadowOwnedHashQuery에 나이(cutoffUnix)·건수(maxHashes) 예산을 붙인
// SQL과 그 위치 인자를 만든다. purgeHookRows의 세 사용처가 모두 이 결과를 쓴다 — 하나라도
// 원본 상수를 그대로 쓰면 문장마다 다른 집합을 보게 되어 행이 어긋난다.
// ORDER BY는 LIMIT을 결정적으로 만들기 위한 것이다(같은 tx 스냅샷에서 세 번 평가되므로 순서가
// 고정돼야 같은 집합이 나온다). content_hash는 DISTINCT 결과 열이라 SQLite가 정렬을 허용한다.
//
// 나이 기준은 마지막 포착(sources.indexed_at)이다 — 첫 포착(artifacts.created_at)이 아니다.
// shadow URI는 콘텐츠 주소이고(ingest.go: "shadow:"+title+":"+contentHash) Register의 found 분기는
// sources만 upsert하므로, 바이트 동일한 출력을 다시 포착하면 옛 artifacts 행이 재사용되고 created_at은
// 첫 포착 시각에 머문다. 첫 포착 기준으로 고르면 방금 다시 포착한 콘텐츠가 지워져, 몇 초 전 모델에
// 넘긴 참조가 ctr_fetch에서 해소되지 않고 그 청크도 ctr_search에서 사라진다. indexed_at은 그 upsert가
// 갱신하므로 이 창을 닫는다 — 형제 보존 경로 PurgeOlderThan(store.go:744)과 같은 기준이다.
// 행 단위가 아니라 hash 단위 배제인 이유: Register의 조회 키가 (content_hash, media_type)이라 같은
// 콘텐츠가 media_type마다 별개 artifacts 행을 가질 수 있는데, 삭제와 CAS 회수는 hash 단위다 —
// 한 행만 최근이어도 공유 blob은 살려야 한다. hook이 raw_blob_hash로 참조하는 경우는 따로 볼 필요가
// 없다: RawBlob은 web 소스만 설정하고(ingest.go 웹 경로) 비-hook raw_blob 참조는 위 두 번째
// NOT EXISTS가 이미 hash 전체를 비귀속으로 떨어뜨린다.
// created_at·indexed_at은 모두 unix 초다(store.go:191·196).
func shadowOwnedFilter(cutoffUnix int64, maxHashes int) (string, []any) {
	q := shadowOwnedHashQuery
	var args []any
	if cutoffUnix > 0 {
		q += `
  AND NOT EXISTS (
    SELECT 1 FROM artifacts a4 JOIN sources s4 ON s4.artifact_id = a4.id
    WHERE a4.content_hash = a.content_hash AND s4.indexed_at >= ?)`
		args = append(args, cutoffUnix)
	}
	if maxHashes > 0 {
		q += "\nORDER BY a.content_hash LIMIT ?"
		args = append(args, maxHashes)
	}
	return q, args
}

// PurgeHookOnly — 예산 없이 shadow 귀속 아티팩트를 지운다(기존 계약 유지).
func (s *Store) PurgeHookOnly(ctx context.Context) (HookPurgeReport, error) {
	return s.PurgeHookOnlyOlderThan(ctx, 0, 0)
}

// DefaultShadowRetention — shadow 귀속 아티팩트 보존 기간(설계 v0.12 D67). 훅이 가로챈
// 출력은 대개 해당 세션 안에서 소비되므로 짧게 잡는다. v0.20에서 cmd/context-router 에서
// 이 패키지로 옮겼다 — doctor 문면이 실효값을 적으려면 internal/cli 도 이 해석기에 닿아야
// 하는데, 규칙을 복제하면 두 구현이 갈라진다(D13, 설계 v0.20 D102 계약 8).
const DefaultShadowRetention = 72 * time.Hour

// ShadowRetention — CTR_SHADOW_RETENTION(time.ParseDuration 형식) 양수만 채택한다.
// 파싱 실패·비양수는 기본값 — 잘못된 값이 정책을 무력화하지 않게 한다.
func ShadowRetention(getenv func(string) string) time.Duration {
	if d, err := time.ParseDuration(getenv("CTR_SHADOW_RETENTION")); err == nil && d > 0 {
		return d
	}
	return DefaultShadowRetention
}

// ShadowCutoff — 보존 기간 d에 대한 회수 경계(unix 초). **0 이하면 회수를 건너뛰어야 한다**:
// shadowOwnedFilter가 cutoffUnix<=0을 "나이 필터 없음"으로 읽으므로(TestPurgeHookOnlyOlderThanZeroMeansAll이
// 그 계약을 고정한다) 보존을 늘리려는 설정이 나이 무관 전량 삭제로 반전된다. d가 epoch
// 경과분(약 56년) 이상이면 그 경계에 닿는다 — 경계가 정확히 0에서 시작하므로 판정도 `<= 0`이다.
func ShadowCutoff(now time.Time, d time.Duration) int64 {
	return now.Add(-d).Unix()
}

// PurgeHookOnlyOlderThan — D41 §3 + D67: shadow 귀속(그 hash를 참조하는 소스가 전부 hook) 아티팩트
// 중 마지막 포착(sources.indexed_at)이 cutoffUnix보다 오래된 것을 최대 maxHashes개까지, 행을 단일
// tx로 삭제한 뒤(술어를 tx 안에서 재실행해 외부 견적을 신뢰하지 않는다), 커밋 후 lockStoreCtx 하에 물리 CAS 파일을 rename
// 격리 프로토콜로 회수한다. 행 삭제와 파일 회수를 내부 두 단계(purgeHookRows·reclaimHookBlobs)로
// 나눠, 커밋↔회수 사이의 재등록 경합(Register.writeBlob 교체)을 rename 격리로 폐쇄하고 테스트가 그
// 창에 개입할 수 있게 한다(신규 공개 표면 아님). cutoffUnix<=0이면 나이 필터가, maxHashes<=0이면
// 건수 상한이 없다(둘 다 0이면 PurgeHookOnly와 동일 — 호출자 단순화). VACUUM은 하지 않는다 —
// 자동 회수 경로는 free page 재사용으로 성장을 억제하고, 파일 축소가 필요할 때만 호출자가 별도로
// 수행한다(설계 v0.12 D67).
func (s *Store) PurgeHookOnlyOlderThan(ctx context.Context, cutoffUnix int64, maxHashes int) (HookPurgeReport, error) {
	hashes, rep, err := s.purgeHookRows(ctx, cutoffUnix, maxHashes)
	if err != nil {
		return rep, err
	}
	if err := s.reclaimHookBlobs(ctx, hashes, &rep); err != nil {
		return rep, err // 행 삭제는 이미 커밋되어 유효 — 남은 파일은 --gc 후속 회수
	}
	return rep, nil
}

// purgeHookRows: 단일 tx(txRetry — PurgeOlderThan과 동일 BUSY 내성)에서 shadow 귀속 hash 집합을
// 술어 재실행으로 산출(회수 목록)하고, chunks→sources→artifacts 순으로 행을 삭제한다. chunks·sources는
// sources가 아직 있는 동안 shadow 술어 서브쿼리로 이 purge의 선택분만 지우고(chunks 선삭제로 FTS
// AFTER DELETE 트리거 동기), artifacts는 sources 삭제 후 술어가 비므로 미리 포착한 hashes 집합에
// 바인딩해 삭제한다 — PurgeOlderThan의 전역 "NOT IN sources" 고아 sweep과 달리 hook purge는 자기
// 선택분에만 국한한다(최종리뷰 P1). BUSY 재시도로 fn이 재실행될 수 있어 반환 상태를 매 시도 초기화한다.
// 회수 목록(SELECT)과 삭제 술어는 같은 tx 스냅샷에서 재평가하므로 항상 일치한다 — D67 예산(나이·건수)도
// shadowOwnedFilter 한 곳에서 만든 sel/selArgs를 세 문장이 공유해 그 일치를 유지한다.
func (s *Store) purgeHookRows(ctx context.Context, cutoffUnix int64, maxHashes int) ([]string, HookPurgeReport, error) {
	sel, selArgs := shadowOwnedFilter(cutoffUnix, maxHashes)
	var hashes []string
	var rep HookPurgeReport
	err := s.txRetry(ctx, func(tx *sql.Tx) error {
		hashes = hashes[:0]
		rep.Hashes = 0
		rows, err := tx.QueryContext(ctx, sel, selArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return err
			}
			hashes = append(hashes, h)
		}
		err = rows.Err()
		rows.Close() // writer는 SetMaxOpenConns(1) — 후속 Exec 전에 커서를 반드시 닫는다
		if err != nil {
			return err
		}
		// chunks: shadow 아티팩트의 청크를 sources 삭제 이전에 명시 삭제한다 — 술어가 아직 유효하고
		// (sources 존재) FTS AFTER DELETE 트리거가 발화한다(cascade는 recursive_triggers OFF라
		// 트리거 미발화). 이 purge의 선택분에만 국한한다(최종리뷰 P1 — 전역 고아 청크 sweep 금지).
		// 서브쿼리도 같은 예산을 받는다(selArgs를 그대로 넘긴다 — ?는 sel에만 있다). 예산을 SELECT에만
		// 걸면 여기서 cutoff 이후 shadow의 청크까지 지워져 소스·청크 없는 고아 아티팩트가 남는다(D67).
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE artifact_id IN
			(SELECT id FROM artifacts WHERE content_hash IN (`+sel+`))`, selArgs...); err != nil {
			return err
		}
		// sources: 귀속 아티팩트의 (전부 hook인) 소스 삭제 — 술어 재실행으로 tx 내 재검증. 예산 동일.
		if _, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE artifact_id IN
			(SELECT id FROM artifacts WHERE content_hash IN (`+sel+`))`, selArgs...); err != nil {
			return err
		}
		// artifacts: sources가 사라져 고아가 된 이 purge의 선택분만 삭제한다. sources 삭제 후엔 shadow
		// 술어가 비므로(hook 소스가 사라져 JOIN 실패) 커서에서 미리 포착한 hashes 집합에 바인딩한다 —
		// store 전역 고아(예: 재색인으로 source가 새 아티팩트로 재지정돼 남은 source-less 잔재)를 지우지
		// 않는다(최종리뷰 P1). ponytail: IN 리스트는 SQLITE_MAX_VARIABLE_NUMBER(기본 32766) 상한 —
		// hook purge 1회가 그 이상 hash를 모을 일은 없다(넘으면 temp table로 승격).
		if len(hashes) > 0 {
			ph := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")
			args := make([]any, len(hashes))
			for i, h := range hashes {
				args[i] = h
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE content_hash IN (`+ph+`)`, args...); err != nil {
				return err
			}
		}
		rep.Hashes = len(hashes)
		return nil
	})
	if err != nil {
		return nil, HookPurgeReport{}, err
	}
	return hashes, rep, nil
}

// reclaimHookBlobs: purgeHookRows 커밋 후 호출 — lockStoreCtx(ctx, s.dir) 하에 hash별 물리 CAS 파일을 rename
// 격리 프로토콜로 회수한다. ① os.Rename(p, p+".purging") — 원자, 실패는 부재만 무음 skip·그 외(공유 위반 등)는 Failed++ ② 격리본 re-Stat:
// mtime이 gcOrphanMinAge(1h) 이내(교체 감지 겸 age gate) 또는 DB 재확인에서 참조 존재 → 원 경로로
// 롤백 + Deferred++ ③ 아니면 os.Remove + ReclaimedB += size(Remove 실패는 롤백 + Failed++). rename
// 이후 도착한 Register.writeBlob은 원 경로에 새 파일을 만들 뿐 무충돌, rename 이전 교체분은 격리본의
// fresh mtime이 ②에서 걸려 롤백 — Stat↔unlink 오삭제 창을 폐쇄한다. lockStoreCtx 실패는 오류로
// 반환하되 행 삭제는 이미 유효하다(남은 파일은 --gc 후속) — 그 실패가 ctx 취소/데드라인이면
// ctx.Err()를 그대로 반환해(F4) 잠금 대기 구간과 루프 구간이 같은 계약을 지킨다: 그 외(다른
// 프로세스의 5초 하드 타임아웃 등 ctx와 무관한 사유)는 기존대로 wrapped ErrUnavailable이다.
// 루프 선두 ctx.Err() 체크(D74)로 취소 시 종료가 배치 크기만큼 지연되던 것과 deferred 부풀림을
// 함께 없앤다.
func (s *Store) reclaimHookBlobs(ctx context.Context, hashes []string, rep *HookPurgeReport) error {
	if len(hashes) == 0 {
		return nil
	}
	release, err := lockStoreCtx(ctx, s.dir)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil { // F4: 취소/데드라인이 원인이면 그 사유를 그대로 노출
			return ctxErr
		}
		return fmt.Errorf("store PurgeHookOnly: %w", err)
	}
	defer release()
	for _, h := range hashes {
		// D74: 회수 루프는 기동 경로에서 도는데 종료 신호를 보지 않으면 종료가 배치 크기만큼
		// 지연되고, 남은 hash가 deferred로 세어져 D67 임계값 튜닝의 입력을 부풀린다. 여기서
		// 반환하면 호출부(PurgeHookOnlyOlderThan)가 "행 삭제는 커밋되어 유효, 남은 파일은
		// --gc 후속 회수"라는 기존 계약대로 처리한다.
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.Join(s.dir, "artifacts", h[:2], h) // blobPath 헬퍼 부재 — 인라인 관례
		q := p + ".purging"
		if err := os.Rename(p, q); err != nil {
			if !os.IsNotExist(err) { // 부재는 무음 skip(이미 없거나 동시 GC 수거), 그 외(공유 위반 등)는 관측
				rep.FailedFiles++ // Windows 공유 위반 등 rename 실패 가시성(os.IsNotExist는 LinkError를 unwrap, 314 선례)
			}
			continue
		}
		fi, statErr := os.Stat(q)
		if statErr != nil || time.Since(fi.ModTime()) < gcOrphanMinAge || s.stillReferenced(ctx, h) {
			_ = os.Rename(q, p) // 롤백: 원 경로 복원
			rep.DeferredFiles++
			continue
		}
		if err := os.Remove(q); err != nil {
			_ = os.Rename(q, p)
			rep.FailedFiles++
			continue
		}
		rep.ReclaimedB += fi.Size()
	}
	return nil
}

// stillReferenced: h를 참조하는 소스가 다시 생겼는지 DB 재확인 — 커밋↔회수 창에 동시 Register가 h를
// 재등록(artifacts.content_hash 또는 sources.raw_blob_hash)했으면 회수를 유예한다. 조회 실패도
// 보수적으로 "참조 있음"으로 취급해 유예한다(오삭제보다 orphan 잔존이 안전 — --gc 후속).
func (s *Store) stillReferenced(ctx context.Context, h string) bool {
	var exists int
	err := s.reader.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM artifacts WHERE content_hash=?)
		    OR EXISTS(SELECT 1 FROM sources WHERE raw_blob_hash=?)`, h, h).Scan(&exists)
	return err != nil || exists == 1
}

// Vacuum — D41: content.db VACUUM(purge로 비운 페이지의 파일 크기 회수). writer 연결로 실행한다 —
// VACUUM은 배타 쓰기 잠금이 필요하다. Task 5b CLI가 purge 후 후행 호출한다.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.writer.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("store Vacuum: %w", err)
	}
	return nil
}

// CheckpointTruncate — D50: wal_checkpoint(TRUNCATE)를 실행하고 결과행(busy, log, checkpointed)을
// 돌려준다. 이 PRAGMA는 미완료를 오류가 아닌 busy=1로 알린다 — Exec nil 반환은 성공 증거가
// 아니므로(설계 v0.8 §5 실험) 반드시 QueryRow로 결과행을 검증해야 한다. WAL 모드에서 VACUUM의
// 파일 축소는 이 checkpoint가 완료(busy=0)되어야 main 파일에 반영된다. 호출자는 busy≠0을
// 실패(라이브 프로세스 추정)로 취급한다.
func (s *Store) CheckpointTruncate(ctx context.Context) (busy, walFrames, checkpointed int, err error) {
	if err := s.writer.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &walFrames, &checkpointed); err != nil {
		return 0, 0, 0, fmt.Errorf("store CheckpointTruncate: %w", err)
	}
	return busy, walFrames, checkpointed, nil
}

// IsBusyErr — isBusy의 공개 래퍼(D50 — CLI가 VACUUM 실패를 "라이브 프로세스 추정" 안내로
// 매핑하는 데 쓴다).
func IsBusyErr(err error) bool { return isBusy(err) }

// IsDiskErr — SQLITE_FULL(13)/SQLITE_IOERR(10) 여부(확장 코드는 하위 8비트가 기본 코드와
// 같다 — isBusy와 동일 불변식). D50 --all 루프가 디스크 계열 실패 시 잔여 프로젝트 VACUUM을
// 중단하는 판별에 쓴다.
func IsDiskErr(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == 13 || code == 10
}
