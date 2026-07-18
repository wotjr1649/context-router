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

func Open(dir string, readOnly bool) (*Store, error) {
	if !readOnly {
		// 0o700: store 루트+artifacts 모두 이 한 호출로 생성(MkdirAll이 만드는 모든 중간
		// 디렉터리에 동일 perm 적용) — Windows는 Unix perm bit 무시(§10 no-op, 주석만).
		if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
			return nil, sanitizeIOErr("open mkdir", err)
		}
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
			l.Exec(`CREATE TABLE IF NOT EXISTS ledger(
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
		tx.Rollback()
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
		tx.Rollback()
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
				nullIfEmpty(reg.Source.RawBlobHash), nullIfEmpty(reg.Source.Extraction), time.Now().Unix(),
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
			  src_mtime_ns=excluded.src_mtime_ns,src_hash=excluded.src_hash,indexed_at=excluded.indexed_at`,
			reg.Source.URI, artID, reg.Source.Kind, reg.Source.Size, reg.Source.MtimeNS, reg.Source.SrcHash,
			nullIfEmpty(reg.Source.RawBlobHash), nullIfEmpty(reg.Source.Extraction), time.Now().Unix())
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

// LedgerAppend: best-effort 사용량 기록 — ledger 없음/오류는 무시(§3.5).
func (s *Store) LedgerAppend(tool string, stored, returned, ms int64) {
	if s.ledger == nil {
		return
	}
	_, _ = s.ledger.Exec(`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms) VALUES(?,?,?,?,?)`,
		time.Now().Unix(), tool, stored, returned, ms)
}
