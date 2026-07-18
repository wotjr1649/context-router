// Package store — DB 수명·PRAGMA·스키마·단일 트랜잭션 계약·blob IO. 설계서 §3.3~3.6.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

const pragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

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
	path := filepath.Join(dir, lockFileName)
	deadline := time.Now().Add(5 * time.Second)
	delay := 10 * time.Millisecond
	for {
		release, err := tryLockFile(path)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("store open: 잠금 획득 실패: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("store open: 잠금 대기 초과: %w", ErrUnavailable)
		}
		time.Sleep(delay)
		if delay < 160*time.Millisecond {
			delay *= 2
		}
	}
}

func Open(dir string, readOnly bool) (*Store, error) {
	if !readOnly {
		// 0o700: store 루트+artifacts 모두 이 한 호출로 생성(MkdirAll이 만드는 모든 중간
		// 디렉터리에 동일 perm 적용) — Windows는 Unix perm bit 무시(§10 no-op, 주석만).
		if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
			return nil, sanitizeIOErr("open mkdir", err)
		}
		// migrate()·ledger.db DDL까지 포함해 Open 반환 시점(defer)까지 보유 — 아래 lockStore 주석 참조.
		release, err := lockStore(dir)
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
			_, _ = l.Exec(`CREATE TABLE IF NOT EXISTS ledger(
				id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
				bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
				duration_ms INTEGER NOT NULL DEFAULT 0)`)
			s.ledger = l
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
		return nil
	case v == SchemaVersion:
		return nil
	case v > SchemaVersion:
		return fmt.Errorf("store migrate: db user_version=%d > 지원 %d — 비파괴 거부: %w", v, SchemaVersion, ErrUnavailable)
	default:
		return fmt.Errorf("store migrate: 알 수 없는 하위 버전 %d: %w", v, ErrUnavailable)
	}
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
			ON CONFLICT(uri) DO UPDATE SET artifact_id=excluded.artifact_id,src_size=excluded.src_size,
			  src_mtime_ns=excluded.src_mtime_ns,src_hash=excluded.src_hash,indexed_at=excluded.indexed_at,
			  raw_blob_hash=excluded.raw_blob_hash,extraction=excluded.extraction`,
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

// sourceOf: artifactID의 sources 중 첫 행(uri ASC — 다중 소스면 결정적으로 하나를 고른다).
// 없으면 ok=false.
func (s *Store) sourceOf(artifactID int64) (SourceInfo, bool) {
	var info SourceInfo
	var size, mtimeNS sql.NullInt64
	var srcHash, extraction sql.NullString
	err := s.reader.QueryRow(`SELECT uri,source_kind,src_size,src_mtime_ns,src_hash,extraction
		FROM sources WHERE artifact_id=? ORDER BY uri ASC LIMIT 1`, artifactID).
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
			hash := e.Name()
			if len(hash) != 64 || referenced[hash] {
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
				continue // age gate: 등록 진행 중일 수 있음(§3.5) — 다음 GC로 미룬다
			}
			if err := os.Remove(filepath.Join(dir, hash)); err != nil {
				return removed, sanitizeIOErr("gc remove", err)
			}
			removed++
		}
	}
	return removed, nil
}

// LedgerAppend: best-effort 사용량 기록 — ledger 없음/오류는 무시(§3.5).
func (s *Store) LedgerAppend(tool string, stored, returned, ms int64) {
	if s.ledger == nil {
		return
	}
	_, _ = s.ledger.Exec(`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms) VALUES(?,?,?,?,?)`,
		time.Now().Unix(), tool, stored, returned, ms)
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
