package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func openT(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen_PragmasAndSchema(t *testing.T) {
	s := openT(t)
	for q, want := range map[string]string{
		"PRAGMA journal_mode": "wal",
		"PRAGMA foreign_keys": "1",
		"PRAGMA user_version": "1",
		"PRAGMA synchronous":  "1",
		"PRAGMA busy_timeout": "5000",
	} {
		var got string
		if err := s.reader.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != want {
			t.Fatalf("%s=%q want %q", q, got, want)
		}
	}
	// FTS integrity-check가 빈 DB에서 통과 (게이트 6 기초)
	for _, fts := range []string{"fts_porter", "fts_trigram"} {
		if _, err := s.writer.Exec("INSERT INTO " + fts + "(" + fts + ") VALUES('integrity-check')"); err != nil {
			t.Fatalf("%s integrity: %v", fts, err)
		}
	}
}

// TestCheckFTSIntegrity_DetectsContentDrift: 최종리뷰 F7 — rank=1을 넘기면 FTS5가
// external-content table(chunks)과의 대조까지 수행한다. 정상 경로에선 schemaV1의
// AFTER INSERT/DELETE/UPDATE 트리거가 chunks↔FTS를 항상 동기화하므로 드리프트가 생길 수
// 없다 — 그래서 존재하지 않는 rowid를 가리키는 FTS 엔트리를 트리거 밖에서 직접 삽입해
// 인위적으로 드리프트를 만든다(트리거가 관여하는 정상 chunks INSERT/DELETE 대신, 스키마
// 자체가 허용하는 수동 INSERT INTO fts_porter(rowid,...) 형태 — chunks_ai 트리거와 동일
// 문형). rank=1 integrity-check는 이를 잡아 실패해야 한다(rank 생략=0은 FTS 내부 구조만
// 봐서 이 드리프트를 통과시킨다).
func TestCheckFTSIntegrity_DetectsContentDrift(t *testing.T) {
	s := openT(t)
	if _, err := s.writer.Exec(`INSERT INTO fts_porter(rowid, title, text) VALUES(999999, 'ghost', 'ghost text')`); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	if err := s.checkFTSIntegrity(t.Context()); err == nil {
		t.Fatal("want error — rank=1 external-content 대조가 드리프트를 못 잡음(F7 회귀)")
	}
}

// TestOpen_UnixPermissions: α4 — store 루트·artifacts·blob 해시프리픽스 디렉터리는 0700,
// blob 파일은 0600(민감 콘텐츠 소유자 전용). Windows는 perm bit 미지원이라 skip.
func TestOpen_UnixPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perm bit만 검증 — windows는 무시됨")
	}
	base := t.TempDir()
	storeRoot := filepath.Join(base, "store") // Open이 직접 생성해야 검증 가능(기존 dir는 대상 아님)
	s, err := Open(storeRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{storeRoot, filepath.Join(storeRoot, "artifacts")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode=%v want 0700", p, fi.Mode().Perm())
		}
	}
	body := []byte("perm test")
	if _, err := s.Register(t.Context(), Registration{
		StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: "/perm.txt", Kind: "file", SrcHash: "hperm"},
		Chunks: []Chunk{{Ordinal: 0, Text: string(body)}},
	}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	blobDir := filepath.Join(storeRoot, "artifacts", hash[:2])
	if fi, err := os.Stat(blobDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("blobDir mode=%v err=%v want 0700", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(filepath.Join(blobDir, hash)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("blob file mode=%v err=%v want 0600", fi.Mode().Perm(), err)
	}
}

func TestOpen_NewerVersionRefusedNonDestructively(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = Open(dir, false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	// 비파괴 확인: 파일이 여전히 user_version=99
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "content.db")))
	defer db.Close()
	var v int
	db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != 99 {
		t.Fatalf("destroyed! user_version=%d", v)
	}
}

func TestMigrate_HealsPartialSchema(t *testing.T) {
	dir := t.TempDir()
	// 부분 생성 상태 시뮬레이션: artifacts만 있고 user_version=0
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "content.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE artifacts(
	  id INTEGER PRIMARY KEY, content_hash TEXT NOT NULL UNIQUE, media_type TEXT NOT NULL,
	  byte_length INTEGER NOT NULL, redaction TEXT NOT NULL DEFAULT 'none', created_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir, false) // 고착 없이 나머지 스키마 완성해야 함
	if err != nil {
		t.Fatalf("partial schema에서 open 실패(고착): %v", err)
	}
	defer s.Close()
	var v int
	if err := s.reader.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 1 {
		t.Fatalf("user_version=%d err=%v", v, err)
	}
	var n int
	if err := s.reader.QueryRow("SELECT count(*) FROM sources").Scan(&n); err != nil {
		t.Fatalf("sources 미생성: %v", err)
	}
}

func TestRegister_DedupTwoSourcesOneArtifact(t *testing.T) {
	s := openT(t)
	reg := Registration{
		StoredBytes: []byte("same body\nline2\n"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/a.txt", Kind: "file", SrcHash: "h-a"},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: 16, LineStart: 1, LineEnd: 2, Text: "same body\nline2\n"}},
	}
	id1, err := s.Register(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	reg.Source = SourceMeta{URI: "/b.txt", Kind: "file", SrcHash: "h-b"}
	id2, err := s.Register(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("dedup 실패: %d != %d", id1, id2)
	}
	var n int
	s.reader.QueryRow("SELECT count(*) FROM sources").Scan(&n)
	if n != 2 {
		t.Fatalf("sources=%d want 2", n)
	}
	// blob 존재 + 내용 일치
	var ch string
	s.reader.QueryRow("SELECT content_hash FROM artifacts WHERE id=?", id1).Scan(&ch)
	b, err := os.ReadFile(filepath.Join(s.dir, "artifacts", ch[:2], ch))
	if err != nil || string(b) != "same body\nline2\n" {
		t.Fatalf("blob: %v %q", err, b)
	}
}

// TestRegister_DedupRespectsMediaType: α3 — 같은 바이트를 다른 media_type으로 등록하면
// dedup이 (content_hash, media_type) 기준이라 별개 artifact가 되어야 한다(같은 바이트가
// .txt/.md로 색인될 때 두 번째가 첫 artifact를 재사용해 md 청킹을 잃는 문제 방지).
// blob 파일은 content_hash 주소라 여전히 1개만 공유된다.
func TestRegister_DedupRespectsMediaType(t *testing.T) {
	s := openT(t)
	body := []byte("same bytes different representation")
	reg := Registration{
		StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: "/x.txt", Kind: "file", SrcHash: "hx"},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), Text: string(body)}},
	}
	id1, err := s.Register(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	reg2 := reg
	reg2.MediaType = "text/markdown"
	reg2.Source = SourceMeta{URI: "/x.md", Kind: "file", SrcHash: "hmd"}
	id2, err := s.Register(t.Context(), reg2)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("want 별개 artifact(media_type 다름), got 동일 id=%d", id1)
	}
	var n int
	s.reader.QueryRow("SELECT count(*) FROM artifacts").Scan(&n)
	if n != 2 {
		t.Fatalf("artifacts=%d want 2", n)
	}
	var c1, c2 int
	s.reader.QueryRow("SELECT count(*) FROM chunks WHERE artifact_id=?", id1).Scan(&c1)
	s.reader.QueryRow("SELECT count(*) FROM chunks WHERE artifact_id=?", id2).Scan(&c2)
	if c1 != 1 || c2 != 1 {
		t.Fatalf("want 각자 청크 1개, got c1=%d c2=%d", c1, c2)
	}
	var ch1, ch2 string
	s.reader.QueryRow("SELECT content_hash FROM artifacts WHERE id=?", id1).Scan(&ch1)
	s.reader.QueryRow("SELECT content_hash FROM artifacts WHERE id=?", id2).Scan(&ch2)
	if ch1 != ch2 {
		t.Fatalf("want 동일 content_hash(같은 바이트), got %q vs %q", ch1, ch2)
	}
	blobs, _ := filepath.Glob(filepath.Join(s.dir, "artifacts", ch1[:2], ch1))
	if len(blobs) != 1 {
		t.Fatalf("want blob 파일 1개(공유), got %v", blobs)
	}
}

func TestRegister_CASRejectsStaleWriter(t *testing.T) {
	s := openT(t)
	base := Registration{
		StoredBytes: []byte("v1"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/f.txt", Kind: "file", SrcHash: "hash-v1"},
		Chunks: []Chunk{{Ordinal: 0, Text: "v1"}},
	}
	if _, err := s.Register(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	newer := base
	newer.StoredBytes, newer.Source.SrcHash, newer.ExpectedOldSrcHash = []byte("v2"), "hash-v2", "hash-v1"
	newer.Chunks = []Chunk{{Ordinal: 0, Text: "v2"}}
	if _, err := s.Register(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	stale := base // 구버전을 v1 기대로 다시 커밋 시도 → 현재는 hash-v2라 거부
	stale.ExpectedOldSrcHash = "hash-v1"
	if _, err := s.Register(t.Context(), stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

// TestRegister_ReindexUpdatesRawBlobHashAndExtraction: 리뷰 Important — 같은 URI를
// ExpectedOldSrcHash 없이(ingest.RunWeb 경로) raw blob/extraction이 다른 값으로 재등록하면
// sources.raw_blob_hash·extraction이 최신 값으로 갱신돼야 한다. 과거엔 ON CONFLICT DO UPDATE가
// 이 두 컬럼을 빼먹어 최초 값에 고정되고 새로 쓴 raw blob이 영구 고아가 됐다.
func TestRegister_ReindexUpdatesRawBlobHashAndExtraction(t *testing.T) {
	s := openT(t)
	uri := "https://example.com/p"
	reg1 := Registration{
		StoredBytes: []byte("body v1"), MediaType: "text/plain",
		Source:  SourceMeta{URI: uri, Kind: "web", SrcHash: "h-v1", Extraction: "readability"},
		Chunks:  []Chunk{{Ordinal: 0, Text: "body v1"}},
		RawBlob: []byte("<html>v1</html>"),
	}
	if _, err := s.Register(t.Context(), reg1); err != nil {
		t.Fatal(err)
	}
	reg2 := Registration{
		StoredBytes: []byte("body v2"), MediaType: "text/plain",
		Source:  SourceMeta{URI: uri, Kind: "web", SrcHash: "h-v2", Extraction: "full"},
		Chunks:  []Chunk{{Ordinal: 0, Text: "body v2"}},
		RawBlob: []byte("<html>v2 differs</html>"),
	}
	if _, err := s.Register(t.Context(), reg2); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(reg2.RawBlob)
	want := hex.EncodeToString(sum[:])
	var gotHash, gotExtraction string
	if err := s.reader.QueryRow("SELECT raw_blob_hash, extraction FROM sources WHERE uri=?", uri).
		Scan(&gotHash, &gotExtraction); err != nil {
		t.Fatal(err)
	}
	if gotHash != want {
		t.Fatalf("raw_blob_hash=%q want %q (재색인 후 stale — 고아 blob)", gotHash, want)
	}
	if gotExtraction != "full" {
		t.Fatalf("extraction=%q want %q", gotExtraction, "full")
	}
}

// TestRegister_FileReindexRawBlobHashStaysEmpty: file 경로는 RawBlob/Extraction을 넘기지
// 않으므로(둘 다 빈값) 위 수정으로 excluded 참조를 추가해도 재색인 후 계속 NULL이어야 한다
// (회귀 없음).
func TestRegister_FileReindexRawBlobHashStaysEmpty(t *testing.T) {
	s := openT(t)
	reg := Registration{
		StoredBytes: []byte("v1"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/f.txt", Kind: "file", SrcHash: "h1"},
		Chunks: []Chunk{{Ordinal: 0, Text: "v1"}},
	}
	if _, err := s.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	reg.StoredBytes, reg.Source.SrcHash = []byte("v2"), "h2"
	reg.Chunks = []Chunk{{Ordinal: 0, Text: "v2"}}
	if _, err := s.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	var gotHash, gotExtraction sql.NullString
	if err := s.reader.QueryRow("SELECT raw_blob_hash, extraction FROM sources WHERE uri=?", "/f.txt").
		Scan(&gotHash, &gotExtraction); err != nil {
		t.Fatal(err)
	}
	if gotHash.Valid || gotExtraction.Valid {
		t.Fatalf("want NULL raw_blob_hash/extraction, got %v/%v", gotHash, gotExtraction)
	}
}

func TestReadRange_Selectors(t *testing.T) {
	s := openT(t)
	body := "alpha\nbravo\ncharlie\n" // bytes: alpha(0-5)...
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/r.txt", Kind: "file", SrcHash: "h"},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), LineStart: 1, LineEnd: 3, Text: body}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ReadRange(t.Context(), id, Selector{Kind: "line", LineStart: 2, LineEnd: 2})
	if err != nil || string(r.Text) != "bravo\n" {
		t.Fatalf("line sel: %v %q", err, r.Text)
	}
	// UTF-8 스냅: 한글 3바이트 중간을 요청해도 문자 경계로 스냅
	id2, _ := s.Register(t.Context(), Registration{
		StoredBytes: []byte("가나다"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/k.txt", Kind: "file", SrcHash: "hk"},
		Chunks: []Chunk{{Ordinal: 0, Text: "가나다"}},
	})
	r2, err := s.ReadRange(t.Context(), id2, Selector{Kind: "byte", ByteStart: 1, ByteEnd: 4})
	if err != nil {
		t.Fatal(err)
	}
	if string(r2.Text) != "가" && string(r2.Text) != "나" { // 스냅 결과는 완전한 문자
		t.Fatalf("snap: %q (start=%d end=%d)", r2.Text, r2.ByteStart, r2.ByteEnd)
	}
	if _, err := s.ReadRange(t.Context(), 9999, Selector{Kind: "chunk", ChunkID: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReadRange_IOErrorHidesPath(t *testing.T) {
	s := openT(t)
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte("alpha\nbravo\n"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/p.txt", Kind: "file", SrcHash: "hp"},
		Chunks: []Chunk{{Ordinal: 0, Text: "alpha\nbravo\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ch string
	s.reader.QueryRow("SELECT content_hash FROM artifacts WHERE id=?", id).Scan(&ch)
	if err := os.Remove(filepath.Join(s.dir, "artifacts", ch[:2], ch)); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReadRange(t.Context(), id, Selector{Kind: "line", LineStart: 1, LineEnd: 1})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), s.dir) {
		t.Fatalf("오류에 경로 노출: %v", err)
	}
}

func TestRegister_ConcurrentDistinctBlobsNoTmpLeftover(t *testing.T) {
	s := openT(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf("content-%d-unique-payload", i)
			_, err := s.Register(t.Context(), Registration{
				StoredBytes: []byte(body), MediaType: "text/plain",
				Source: SourceMeta{URI: fmt.Sprintf("/c%d.txt", i), Kind: "file", SrcHash: fmt.Sprintf("h%d", i)},
				Chunks: []Chunk{{Ordinal: 0, Text: body}},
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("content-%d-unique-payload", i)
		sum := sha256.Sum256([]byte(body))
		hash := hex.EncodeToString(sum[:])
		b, err := os.ReadFile(filepath.Join(s.dir, "artifacts", hash[:2], hash))
		if err != nil || string(b) != body {
			t.Fatalf("blob %d mismatch: %v %q", i, err, b)
		}
	}
	leftover, _ := filepath.Glob(filepath.Join(s.dir, "artifacts", "*", "*.tmp*"))
	if len(leftover) != 0 {
		t.Fatalf("임시파일 잔존: %v", leftover)
	}
}

func TestReadRange_LineInvalidRangeRejected(t *testing.T) {
	s := openT(t)
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte("a\nb\nc\n"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/x.txt", Kind: "file", SrcHash: "hx"},
		Chunks: []Chunk{{Ordinal: 0, Text: "a\nb\nc\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sel := range []Selector{
		{Kind: "line", LineStart: 3, LineEnd: 1},
		{Kind: "line", LineStart: 1, LineEnd: 0},
		{Kind: "line", LineStart: 4, LineEnd: 4}, // α2: 실제 줄 수(3) 초과 — 관대 클램프 금지
		{Kind: "line", LineStart: 1, LineEnd: 4}, // α2: LineEnd만 초과해도 거부
	} {
		if _, err := s.ReadRange(t.Context(), id, sel); !errors.Is(err, ErrInvalidSelector) {
			t.Fatalf("sel=%+v: want ErrInvalidSelector, got %v (no panic expected)", sel, err)
		}
	}
	// 힌트에 실제 줄 수(3) 포함 — 경로·원문 없이 숫자만 (α2)
	_, err = s.ReadRange(t.Context(), id, Selector{Kind: "line", LineStart: 4, LineEnd: 4})
	if err == nil || !strings.Contains(err.Error(), "1..3") {
		t.Fatalf("want hint 1..3, got %v", err)
	}
}

// TestReadRange_ByteOutOfRangeRejected: α2 — byte 선택자가 blob 길이를 벗어나면(음수·
// 역전·시작이 끝이상·끝이 길이초과) 관대 클램프 없이 ErrInvalidSelector, 힌트에 실제
// 길이가 숫자로만 포함된다.
func TestReadRange_ByteOutOfRangeRejected(t *testing.T) {
	s := openT(t)
	body := "abcde" // len=5
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/b.txt", Kind: "file", SrcHash: "hb"},
		Chunks: []Chunk{{Ordinal: 0, Text: body}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sel := range []Selector{
		{Kind: "byte", ByteStart: -1, ByteEnd: 3},
		{Kind: "byte", ByteStart: 2, ByteEnd: 2},
		{Kind: "byte", ByteStart: 5, ByteEnd: 5}, // ByteStart>=len
		{Kind: "byte", ByteStart: 0, ByteEnd: 6}, // ByteEnd>len — 초과분 클램프 금지
	} {
		if _, err := s.ReadRange(t.Context(), id, sel); !errors.Is(err, ErrInvalidSelector) {
			t.Fatalf("sel=%+v: want ErrInvalidSelector, got %v", sel, err)
		}
	}
	_, err = s.ReadRange(t.Context(), id, Selector{Kind: "byte", ByteStart: 0, ByteEnd: 6})
	if err == nil || !strings.Contains(err.Error(), "0..5") {
		t.Fatalf("want hint 0..5, got %v", err)
	}
}

// TestReadRange_ChunkWrongArtifactRejected: α2 — chunk_id가 실재하되 요청한 artifact
// 소속이 아니면 ErrNotFound가 아닌 ErrInvalidSelector.
func TestReadRange_ChunkWrongArtifactRejected(t *testing.T) {
	s := openT(t)
	idA, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte("artifact A body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/a2.txt", Kind: "file", SrcHash: "ha2"},
		Chunks: []Chunk{{Ordinal: 0, Text: "artifact A body"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte("artifact B body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/b2.txt", Kind: "file", SrcHash: "hb2"},
		Chunks: []Chunk{{Ordinal: 0, Text: "artifact B body"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var chunkIDofB int64
	if err := s.reader.QueryRow("SELECT id FROM chunks WHERE artifact_id=?", idB).Scan(&chunkIDofB); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReadRange(t.Context(), idA, Selector{Kind: "chunk", ChunkID: chunkIDofB})
	if !errors.Is(err, ErrInvalidSelector) {
		t.Fatalf("want ErrInvalidSelector(chunk 실재·타 artifact), got %v", err)
	}
	// 진짜 미존재 chunk_id는 여전히 ErrNotFound
	_, err = s.ReadRange(t.Context(), idA, Selector{Kind: "chunk", ChunkID: 99999})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound(진짜 미존재), got %v", err)
	}
}

func TestSourceOf(t *testing.T) {
	s := openT(t)
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte("body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/z.txt", Kind: "file", SrcHash: "hz"},
		Chunks: []Chunk{{Ordinal: 0, Text: "body"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, ok := s.sourceOf(id)
	if !ok || info.URI != "/z.txt" || info.Kind != "file" || info.SrcHash != "hz" {
		t.Fatalf("sourceOf=%+v ok=%v", info, ok)
	}
	if _, ok := s.sourceOf(9999); ok {
		t.Fatal("want ok=false for unknown artifact_id")
	}
}

func TestStaleOf(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "doc.txt")
	body := []byte("hello world")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	info := SourceInfo{
		URI: filepath.ToSlash(file), Kind: "file", Size: fi.Size(),
		MtimeNS: fi.ModTime().UnixNano(), SrcHash: hex.EncodeToString(sum[:]),
	}

	if StaleOf(info) {
		t.Fatal("want 수정 전 Stale=false")
	}
	inlineInfo := info
	inlineInfo.Kind = "inline"
	inlineInfo.URI = "/does/not/exist.txt" // kind!=file → os.Stat조차 하지 않는 단락 경로
	if StaleOf(inlineInfo) {
		t.Fatal("want kind=inline 항상 Stale=false")
	}

	future := time.Now().Add(time.Hour)
	if err := os.WriteFile(file, []byte("hello world MODIFIED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	if !StaleOf(info) {
		t.Fatal("want 수정 후 Stale=true")
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if !StaleOf(info) {
		t.Fatal("want 삭제 후 Stale=true")
	}
}

func TestReadRange_FillsSourceAndStale(t *testing.T) {
	s := openT(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "src.txt")
	body := []byte("line one\n")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	uri := filepath.ToSlash(file)
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: uri, Kind: "file", Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano(), SrcHash: hex.EncodeToString(sum[:])},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), LineStart: 1, LineEnd: 1, Text: string(body)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ReadRange(t.Context(), id, Selector{Kind: "line", LineStart: 1, LineEnd: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasSource || r.Source.URI != uri || r.Stale {
		t.Fatalf("want HasSource=true Stale=false, got HasSource=%v Source=%+v Stale=%v", r.HasSource, r.Source, r.Stale)
	}
}

// TestOpen_MkdirAllErrorHidesPath: 이월 c — Open()의 MkdirAll 실패도(readBlob 계열과
// 동일하게) sanitizeIOErr를 거쳐 절대경로를 노출하면 안 된다.
func TestOpen_MkdirAllErrorHidesPath(t *testing.T) {
	base := t.TempDir()
	blocked := filepath.Join(base, "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// blocked는 파일이라 MkdirAll(blocked/artifacts)가 반드시 실패한다.
	_, err := Open(blocked, false)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), blocked) {
		t.Fatalf("오류에 경로 노출: %v", err)
	}
}

// TestArtifactText: ctr_transform 입력 로더 — 존재하는 artifact는 원문 그대로, 없으면
// ErrNotFound, byte_length가 maxBytes를 넘으면 ErrInvalidSelector(§4.2.3).
func TestArtifactText(t *testing.T) {
	s := openT(t)
	body := "hello transform input"
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/t.txt", Kind: "file", SrcHash: "ht"},
		Chunks: []Chunk{{Ordinal: 0, Text: body}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ArtifactText(t.Context(), id, 0)
	if err != nil || got != body {
		t.Fatalf("got=%q err=%v want %q", got, err, body)
	}
	if _, err := s.ArtifactText(t.Context(), id, int64(len(body)-1)); !errors.Is(err, ErrInvalidSelector) {
		t.Fatalf("want ErrInvalidSelector for maxBytes 초과, got %v", err)
	}
	if _, err := s.ArtifactText(t.Context(), 9999, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestMain: CTR_TEST_CHILD=1이면 자기 바이너리 재실행 자식 프로세스 모드(go/os/exec 표준
// helper-process 패턴) — CTR_TEST_CHILD_MODE로 시나리오를 고른다(미지정=기존 최초-마이그레이션
// 경합, "cas-race"=게이트7 심층 CAS 2-프로세스 경쟁, "kill-write"=게이트7 심층 강제kill 내구성).
func TestMain(m *testing.M) {
	if os.Getenv("CTR_TEST_CHILD") == "1" {
		switch os.Getenv("CTR_TEST_CHILD_MODE") {
		case "cas-race":
			os.Exit(runCASRaceChild())
		case "kill-write":
			os.Exit(runKillWriteChild())
		default:
			os.Exit(runConcurrentOpenChild())
		}
	}
	os.Exit(m.Run())
}

// runConcurrentOpenChild: 부모가 signal 파일을 만들 때까지 폴링 대기했다가 동시에
// Open→Register→Close 1세트를 수행한다. 실패 시 stderr에 원인을 남기고 1 반환.
func runConcurrentOpenChild() int {
	dir := os.Getenv("CTR_TEST_CHILD_DIR")
	signal := os.Getenv("CTR_TEST_CHILD_SIGNAL")
	id := os.Getenv("CTR_TEST_CHILD_ID")
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s, err := Open(dir, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return 1
	}
	body := "concurrent-first-migration-" + id
	if _, err := s.Register(context.Background(), Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/child-" + id + ".txt", Kind: "file", SrcHash: "h" + id},
		Chunks: []Chunk{{Ordinal: 0, Text: body}},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		s.Close()
		return 1
	}
	if err := s.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close:", err)
		return 1
	}
	return 0
}

// TestOpen_ConcurrentFirstMigration: 신규 store 디렉터리 하나에 서브프로세스 2개가 동시에
// 최초 Open(=최초 마이그레이션)을 개시했을 때 둘 다 성공해야 한다. 최초 WAL 전환 시
// SQLite의 wal-index recovery 락 경로(WAL_RECOVER_LOCK)는 busy_timeout(busy handler)을
// 거치지 않고 SQLITE_BUSY를 즉시 반환한다(실제 원인 — 최종리뷰 F10, DSN _pragma 순서
// 문제라는 이전 서술은 반증됨. lockStore 주석 참조) — Open()의 advisory lock(lockStore)
// 으로 직렬화해 근본 수정했다(게이트 7 심층, 세션01 Task9 발견). reps회 반복해 레이스
// 확률 확보.
func TestOpen_ConcurrentFirstMigration(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const reps = 8
	for i := 0; i < reps; i++ {
		root := t.TempDir()
		storeDir := filepath.Join(root, "store") // Open이 MkdirAll로 신규 생성 — 두 자식 모두 최초 기동
		signal := filepath.Join(root, "start.signal")

		cmds := make([]*exec.Cmd, 2)
		outs := make([]bytes.Buffer, 2)
		for c := range cmds {
			cmd := exec.Command(exe)
			cmd.Env = append(
				os.Environ(),
				"CTR_TEST_CHILD=1",
				"CTR_TEST_CHILD_DIR="+storeDir,
				"CTR_TEST_CHILD_SIGNAL="+signal,
				fmt.Sprintf("CTR_TEST_CHILD_ID=%d", c),
			)
			cmd.Stderr = &outs[c]
			if err := cmd.Start(); err != nil {
				t.Fatalf("rep %d child %d start: %v", i, c, err)
			}
			cmds[c] = cmd
		}
		time.Sleep(20 * time.Millisecond) // 두 자식이 signal 폴링 루프에 들어갈 시간 확보
		if err := os.WriteFile(signal, []byte("go"), 0o600); err != nil {
			t.Fatalf("rep %d signal: %v", i, err)
		}
		var wg sync.WaitGroup
		for c := range cmds {
			wg.Add(1)
			go func(c int) {
				defer wg.Done()
				if err := cmds[c].Wait(); err != nil {
					t.Errorf("rep %d child %d 실패: %v\nstderr: %s", i, c, err, outs[c].String())
				}
			}(c)
		}
		wg.Wait()
	}
}

// runCASRaceChild: 게이트7 심층(P1a, Codex 교차리뷰) 자식 — 같은 URI("/cas-race.txt")에
// 서로 다른 컨텐츠로 등록을 시도하되 둘 다 동일한 ExpectedOldSrcHash("h-base")로 CAS 경쟁한다
// (§3.5: UPDATE ... WHERE uri=? AND src_hash=? — 먼저 커밋한 쪽만 매치되고, 나머지는 그
// 시점엔 이미 src_hash가 바뀌어 있어 RowsAffected=0→ErrConflict). 결과를 stdout 한 줄로
// 보고해 부모가 "정확히 하나만 성공"을 판별하게 한다.
func runCASRaceChild() int {
	dir := os.Getenv("CTR_TEST_CHILD_DIR")
	signal := os.Getenv("CTR_TEST_CHILD_SIGNAL")
	id := os.Getenv("CTR_TEST_CHILD_ID")
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s, err := Open(dir, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return 1
	}
	defer s.Close()
	body := "cas-race-content-" + id
	_, err = s.Register(context.Background(), Registration{
		StoredBytes:        []byte(body),
		MediaType:          "text/plain",
		Source:             SourceMeta{URI: "/cas-race.txt", Kind: "file", SrcHash: "h-" + id},
		ExpectedOldSrcHash: "h-base",
		Chunks:             []Chunk{{Ordinal: 0, Text: body}},
	})
	switch {
	case err == nil:
		fmt.Println("WON")
		return 0
	case errors.Is(err, ErrConflict):
		fmt.Println("LOST")
		return 0
	default:
		fmt.Fprintln(os.Stderr, "register unexpected:", err)
		return 1
	}
}

// TestRegister_TwoProcessCASRace: 게이트7 심층(P1a, Codex 교차리뷰) — 실 OS 프로세스 2개가
// 같은 소스 URI를 서로 다른 콘텐츠·구지문 기반 CAS로 경쟁하면 정확히 하나만 커밋되고,
// 최종 sources 포인터는 두 후보 중 하나의 완결 상태여야 한다(과거 base로 회귀 금지) +
// FTS integrity-check 통과.
func TestRegister_TwoProcessCASRace(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	storeDir := t.TempDir()
	signal := filepath.Join(t.TempDir(), "start.signal")

	base, err := Open(storeDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Register(context.Background(), Registration{
		StoredBytes: []byte("base"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/cas-race.txt", Kind: "file", SrcHash: "h-base"},
		Chunks: []Chunk{{Ordinal: 0, Text: "base"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}

	cmds := make([]*exec.Cmd, 2)
	outs := make([]bytes.Buffer, 2)
	errBufs := make([]bytes.Buffer, 2)
	for c := range cmds {
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(),
			"CTR_TEST_CHILD=1", "CTR_TEST_CHILD_MODE=cas-race",
			"CTR_TEST_CHILD_DIR="+storeDir, "CTR_TEST_CHILD_SIGNAL="+signal,
			fmt.Sprintf("CTR_TEST_CHILD_ID=%d", c+1))
		cmd.Stdout = &outs[c]
		cmd.Stderr = &errBufs[c]
		if err := cmd.Start(); err != nil {
			t.Fatalf("child %d start: %v", c, err)
		}
		cmds[c] = cmd
	}
	time.Sleep(20 * time.Millisecond) // 두 자식 모두 signal 폴링 루프에 들어갈 시간 확보
	if err := os.WriteFile(signal, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for c := range cmds {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			if err := cmds[c].Wait(); err != nil {
				t.Errorf("child %d 실패: %v (stderr=%s)", c, err, errBufs[c].String())
			}
		}(c)
	}
	wg.Wait()

	wonCount, lostCount := 0, 0
	results := [2]string{strings.TrimSpace(outs[0].String()), strings.TrimSpace(outs[1].String())}
	for _, r := range results {
		switch r {
		case "WON":
			wonCount++
		case "LOST":
			lostCount++
		default:
			t.Fatalf("unexpected child stdout: %q (results=%v)", r, results)
		}
	}
	if wonCount != 1 || lostCount != 1 {
		t.Fatalf("want exactly 1 WON + 1 LOST, got results=%v", results)
	}

	final, err := Open(storeDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	var srcHash string
	var artifactID int64
	if err := final.Reader().QueryRow(
		"SELECT src_hash, artifact_id FROM sources WHERE uri='/cas-race.txt'",
	).Scan(&srcHash, &artifactID); err != nil {
		t.Fatalf("query final sources row: %v", err)
	}
	if srcHash != "h-1" && srcHash != "h-2" {
		t.Fatalf("src_hash=%q want h-1 or h-2 (과거 h-base로 회귀 금지)", srcHash)
	}
	wantBody := "cas-race-content-" + strings.TrimPrefix(srcHash, "h-")
	text, err := final.ArtifactText(context.Background(), artifactID, 0)
	if err != nil {
		t.Fatalf("ArtifactText: %v", err)
	}
	if text != wantBody {
		t.Fatalf("final content=%q want %q (src_hash=%s와 불일치 — 교차오염 의심)", text, wantBody, srcHash)
	}
	if err := final.checkFTSIntegrity(context.Background()); err != nil {
		t.Fatalf("integrity-check: %v", err)
	}
}

// runKillWriteChild: 게이트7 심층(P1b, Codex 교차리뷰) 자식 — 대량 Register 루프(각 호출이
// 자신의 BEGIN IMMEDIATE 트랜잭션, §3.5)를 돈다. 부모가 도중에 강제 kill할 것을 전제로 하며,
// 끝까지 완주해도(kill이 너무 늦었을 뿐) 무해하다. 첫 커밋 성공 직후 CTR_TEST_CHILD_READY
// 경로에 readiness 파일을 써 부모에게 알린다(최종리뷰 F8) — 부모가 고정 지연 대신 이
// 신호를 기다렸다 kill해야 "커밋 0건"으로 vacuous 통과하는 경우를 구조적으로 배제할 수 있다.
func runKillWriteChild() int {
	dir := os.Getenv("CTR_TEST_CHILD_DIR")
	signal := os.Getenv("CTR_TEST_CHILD_SIGNAL")
	ready := os.Getenv("CTR_TEST_CHILD_READY")
	n, err := strconv.Atoi(os.Getenv("CTR_TEST_CHILD_ITERS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "iters parse:", err)
		return 1
	}
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s, err := Open(dir, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return 1
	}
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("kill-write-body-%d", i)
		if _, err := s.Register(context.Background(), Registration{
			StoredBytes: []byte(body), MediaType: "text/plain",
			Source: SourceMeta{URI: fmt.Sprintf("/kill-race-%05d.txt", i), Kind: "file", SrcHash: fmt.Sprintf("h%d", i)},
			Chunks: []Chunk{{Ordinal: 0, Text: body}},
		}); err != nil {
			fmt.Fprintln(os.Stderr, "register", i, ":", err)
			return 1
		}
		if i == 0 {
			if err := os.WriteFile(ready, []byte("1"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "ready write:", err)
				return 1
			}
		}
	}
	return 0
}

// TestOpen_SurvivesWriteKillMidLoop: 게이트7 심층(P1b, Codex 교차리뷰) — 자식이 대량
// Register 루프 도중 부모의 강제 kill(SIGKILL 상당/TerminateProcess)을 맞아도, 재오픈 시
// quick_check·FTS integrity-check가 통과하고 이미 커밋된 행은 완전한 형태로 읽혀야 한다
// (§3.5 단일 트랜잭션 계약 — 부분 반영 없음. lockStore 주석대로 커널이 advisory lock을
// 자동 해제하므로 재오픈이 멎지 않는다). 최종리뷰 F8: 예전엔 signal 이후 고정 50ms만
// 기다려 kill했는데, 느린 CI에서는 그 50ms 안에 자식이 store Open조차 못 끝내 committed==0
// 으로 vacuous 통과(kill mid-transaction을 실제로 검증하지 못함)할 수 있었다. 이제는
// 자식의 첫 커밋 성공 readiness 신호를 폴링해 그 이후에만 kill하고, committed가 반드시
// 0보다 크고 n보다 작다는 것을 하드 단언한다.
func TestOpen_SurvivesWriteKillMidLoop(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	storeDir := t.TempDir()
	signal := filepath.Join(t.TempDir(), "start.signal")
	ready := filepath.Join(t.TempDir(), "first-commit.ready")
	const n = 5000

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"CTR_TEST_CHILD=1", "CTR_TEST_CHILD_MODE=kill-write",
		"CTR_TEST_CHILD_DIR="+storeDir, "CTR_TEST_CHILD_SIGNAL="+signal,
		"CTR_TEST_CHILD_READY="+ready,
		fmt.Sprintf("CTR_TEST_CHILD_ITERS=%d", n))
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("child start: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // 자식이 signal 폴링 루프에 들어갈 시간 확보
	if err := os.WriteFile(signal, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}

	// readiness(첫 커밋 성공) 폴링 — 고정 지연 대신 실제 진행 신호를 기다린다.
	readyDeadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(ready); statErr == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatalf("첫 커밋 readiness 신호 대기 타임아웃 — child stderr: %s", errBuf.String())
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond) // flake 방지 소폭 지연 — n=5000 완주에는 한참 못 미친다.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = cmd.Process.Wait() // 좀비 방지 — exit status는 강제kill이라 무시

	s, err := Open(storeDir, false)
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer s.Close()

	var quick string
	if err := s.Reader().QueryRow("PRAGMA quick_check").Scan(&quick); err != nil {
		t.Fatalf("quick_check query: %v", err)
	}
	if quick != "ok" {
		t.Fatalf("quick_check=%q want ok", quick)
	}
	if err := s.checkFTSIntegrity(context.Background()); err != nil {
		t.Fatalf("integrity-check: %v", err)
	}

	var committed int
	if err := s.Reader().QueryRow(
		"SELECT COUNT(*) FROM sources WHERE uri LIKE '/kill-race-%'",
	).Scan(&committed); err != nil {
		t.Fatalf("count sources: %v", err)
	}
	t.Logf("kill mid-loop: %d/%d rows committed before kill", committed, n)
	// 최종리뷰 F8: readiness 신호를 기다렸다 kill했으므로 committed==0은 구조적으로
	// 불가능하고(첫 커밋 후에만 kill), n 완주도 목표가 아니다(하드 단언, 고정 시간 추측 아님).
	if committed <= 0 || committed >= n {
		t.Fatalf("committed=%d want 0 < committed < %d (readiness 이후 kill인데 이 범위를 벗어남)", committed, n)
	}

	rows, err := s.Reader().Query("SELECT uri, artifact_id FROM sources WHERE uri LIKE '/kill-race-%'")
	if err != nil {
		t.Fatalf("query committed rows: %v", err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var uri string
		var artID int64
		if err := rows.Scan(&uri, &artID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var idx int
		if _, err := fmt.Sscanf(uri, "/kill-race-%05d.txt", &idx); err != nil {
			t.Fatalf("parse uri %q: %v", uri, err)
		}
		wantBody := fmt.Sprintf("kill-write-body-%d", idx)
		text, err := s.ArtifactText(context.Background(), artID, 0)
		if err != nil {
			t.Fatalf("ArtifactText(%d): %v", artID, err)
		}
		if text != wantBody {
			t.Fatalf("row %q content=%q want %q (부분 반영 의심)", uri, text, wantBody)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked != committed {
		t.Fatalf("checked=%d != committed count=%d", checked, committed)
	}
}

// TestConcurrency_Writer1Reader4: 게이트7(P1c, Codex 교차리뷰) — writer 고루틴 1개가 연속
// Register하는 동안 reader 고루틴 4개(=Reader() 풀 크기 SetMaxOpenConns(4)와 일치)가 raw FTS
// MATCH 질의 + ArtifactText를 연속 수행해도 오류가 0이어야 한다(-race는 CI ubuntu 잡이 커버).
func TestConcurrency_Writer1Reader4(t *testing.T) {
	s := openT(t)
	const writes = 300
	var errCount atomic.Int64
	var latestArtifactID atomic.Int64

	var writeWG sync.WaitGroup
	writeWG.Add(1)
	go func() {
		defer writeWG.Done()
		for i := 0; i < writes; i++ {
			body := fmt.Sprintf("needle concurrent body %d", i)
			id, err := s.Register(context.Background(), Registration{
				StoredBytes: []byte(body), MediaType: "text/plain",
				Source: SourceMeta{URI: fmt.Sprintf("/writer-%d.txt", i), Kind: "file", SrcHash: fmt.Sprintf("h%d", i)},
				Chunks: []Chunk{{Ordinal: 0, Text: body}},
			})
			if err != nil {
				t.Errorf("register %d: %v", i, err)
				errCount.Add(1)
				return
			}
			latestArtifactID.Store(id)
		}
	}()

	stop := make(chan struct{})
	go func() { writeWG.Wait(); close(stop) }()

	var readWG sync.WaitGroup
	for r := 0; r < 4; r++ {
		readWG.Add(1)
		go func(r int) {
			defer readWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rows, err := s.Reader().QueryContext(context.Background(),
					`SELECT rowid FROM fts_porter WHERE fts_porter MATCH ? LIMIT 5`, `"needle"`)
				if err != nil {
					t.Errorf("reader %d fts query: %v", r, err)
					errCount.Add(1)
					return
				}
				rows.Close()
				if id := latestArtifactID.Load(); id != 0 {
					if _, err := s.ArtifactText(context.Background(), id, 0); err != nil {
						t.Errorf("reader %d ArtifactText(%d): %v", r, id, err)
						errCount.Add(1)
						return
					}
				}
			}
		}(r)
	}
	readWG.Wait()
	if errCount.Load() != 0 {
		t.Fatalf("errCount=%d want 0", errCount.Load())
	}
}

// TestLedgerStats: LedgerAppend 3건(도구 2종)을 넣고 LedgerStats(dir)가 도구별로 calls·바이트
// 합계·span(first/last ts)을 정확히 집계하는지 확인한다(설계 §6 stats local 계약). ledger.db가
// 아예 없는 디렉터리는 빈 슬라이스 + err=nil이어야 한다(ledger는 best-effort 보조 산출물).
func TestLedgerStats(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	s.LedgerAppend("ctr_fetch", 100, 10, 5)
	s.LedgerAppend("ctr_fetch", 200, 20, 7)
	s.LedgerAppend("ctr_search", 50, 500, 3)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	stats, err := LedgerStats(dir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("len=%d want 2: %+v", len(stats), stats)
	}
	if stats[0].Tool != "ctr_fetch" || stats[0].Calls != 2 || stats[0].BytesStored != 300 || stats[0].BytesReturned != 30 {
		t.Fatalf("ctr_fetch row wrong: %+v", stats[0])
	}
	if stats[0].FirstTS == 0 || stats[0].LastTS == 0 || stats[0].FirstTS > stats[0].LastTS {
		t.Fatalf("ctr_fetch span wrong: %+v", stats[0])
	}
	if stats[1].Tool != "ctr_search" || stats[1].Calls != 1 || stats[1].BytesStored != 50 || stats[1].BytesReturned != 500 {
		t.Fatalf("ctr_search row wrong: %+v", stats[1])
	}

	empty, err := LedgerStats(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.db 미존재: err=%v want nil", err)
	}
	if len(empty) != 0 {
		t.Fatalf("ledger.db 미존재: stats=%+v want 빈 슬라이스", empty)
	}

	// ledger.db는 존재하지만 행이 0개인 경우(store만 열고 LedgerAppend를 한 번도 안 한 경우) —
	// "파일 미존재" 조기 반환과는 다른 코드 경로(실제 SELECT가 빈 결과셋을 만남)라 별도로 확인한다.
	emptyDir := t.TempDir()
	es, err := Open(emptyDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	emptyLedger, err := LedgerStats(emptyDir)
	if err != nil {
		t.Fatalf("빈 ledger.db: err=%v want nil", err)
	}
	if len(emptyLedger) != 0 {
		t.Fatalf("빈 ledger.db: stats=%+v want 빈 슬라이스", emptyLedger)
	}
}

// TestLedgerStats_StatErrorOtherThanNotExist: os.Stat 오류가 ErrNotExist가 아니면(권한 거부
// 등) 삼키지 않고 반환해야 한다(리뷰 Fix Round 3, item 3) — 그리고 그 반환 오류에 원본 경로가
// 섞이면 안 된다(sanitizeIOErr, §5.5). 실제 권한 거부는 OS 종속적이라 이식성 있게 재현하기
// 어려워, NUL 바이트가 든 경로로 os.Stat이 결정적으로 "invalid argument"(ErrNotExist 아님)를
// 내도록 유도한다(Windows에서 실측: errors.Is(err, os.ErrNotExist)=false 확인 완료).
func TestLedgerStats_StatErrorOtherThanNotExist(t *testing.T) {
	dir := "bad\x00dir"
	stats, err := LedgerStats(dir)
	if err == nil {
		t.Fatalf("want error for non-ErrNotExist os.Stat failure, got nil (stats=%+v)", stats)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("error must not leak the raw path: %v", err)
	}
}

// TestPurgeOlderThan: 구 source 1개(cutoff 이전에 등록) + 신 source 1개(cutoff 이후)를
// 등록하고 cutoff를 그 경계로 준다(등록 사이에 time.Now()를 캡처 — indexed_at 조작을 위한
// writer UPDATE 테스트 헬퍼 대신 실제 시각 흐름으로 경계를 만든다, 설계 §7). 구 source만
// 삭제되고 그 artifact/chunks도 함께 삭제되며(신 source의 artifact는 남음), FTS
// integrity-check가 통과해야 한다.
func TestPurgeOlderThan(t *testing.T) {
	s := openT(t)
	oldReg := Registration{
		StoredBytes: []byte("old body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/old.txt", Kind: "file", SrcHash: "h-old"},
		Chunks: []Chunk{{Ordinal: 0, Text: "old body"}},
	}
	oldID, err := s.Register(t.Context(), oldReg)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond) // indexed_at은 unix 초 단위 — 경계를 넘기려면 1초 이상 필요
	cutoff := time.Now().Unix()
	time.Sleep(1100 * time.Millisecond)

	newReg := Registration{
		StoredBytes: []byte("new body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/new.txt", Kind: "file", SrcHash: "h-new"},
		Chunks: []Chunk{{Ordinal: 0, Text: "new body"}},
	}
	newID, err := s.Register(t.Context(), newReg)
	if err != nil {
		t.Fatal(err)
	}

	gotSources, gotArtifacts, err := s.PurgeOlderThan(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if gotSources != 1 || gotArtifacts != 1 {
		t.Fatalf("sources=%d artifacts=%d want 1,1", gotSources, gotArtifacts)
	}

	var n int
	if err := s.reader.QueryRow("SELECT count(*) FROM sources WHERE uri=?", "/old.txt").Scan(&n); err != nil || n != 0 {
		t.Fatalf("old source 잔존: n=%d err=%v", n, err)
	}
	if err := s.reader.QueryRow("SELECT count(*) FROM artifacts WHERE id=?", oldID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old artifact 잔존: n=%d err=%v", n, err)
	}
	if err := s.reader.QueryRow("SELECT count(*) FROM chunks WHERE artifact_id=?", oldID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old chunks 잔존: n=%d err=%v", n, err)
	}
	if err := s.reader.QueryRow("SELECT count(*) FROM sources WHERE uri=?", "/new.txt").Scan(&n); err != nil || n != 1 {
		t.Fatalf("new source 삭제됨: n=%d err=%v", n, err)
	}
	if err := s.reader.QueryRow("SELECT count(*) FROM artifacts WHERE id=?", newID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("new artifact 삭제됨: n=%d err=%v", n, err)
	}

	// FTS 동기 확인: old chunk의 텍스트가 검색 인덱스에서도 사라졌어야 한다.
	if err := s.reader.QueryRow("SELECT count(*) FROM fts_porter WHERE fts_porter MATCH 'old'").Scan(&n); err != nil {
		t.Fatalf("fts_porter query: %v", err)
	}
	if n != 0 {
		t.Fatalf("fts_porter에 old chunk 잔존: n=%d", n)
	}
}

// TestPurgeOlderThan_MismatchLeavesEverything: cutoff가 모든 source보다 과거라 아무것도
// 삭제 대상이 아닌 경우 sources=0 artifacts=0이며 실제로 아무 행도 사라지지 않아야 한다.
func TestPurgeOlderThan_MismatchLeavesEverything(t *testing.T) {
	s := openT(t)
	if _, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte("keep me"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/keep.txt", Kind: "file", SrcHash: "h-keep"},
		Chunks: []Chunk{{Ordinal: 0, Text: "keep me"}},
	}); err != nil {
		t.Fatal(err)
	}
	gotSources, gotArtifacts, err := s.PurgeOlderThan(t.Context(), 1) // 1970년대 cutoff — 아무것도 오래되지 않음
	if err != nil {
		t.Fatal(err)
	}
	if gotSources != 0 || gotArtifacts != 0 {
		t.Fatalf("sources=%d artifacts=%d want 0,0", gotSources, gotArtifacts)
	}
	var n int
	s.reader.QueryRow("SELECT count(*) FROM sources").Scan(&n)
	if n != 1 {
		t.Fatalf("sources=%d want 1(무삭제)", n)
	}
}

// writeAgedOrphanBlob: 테스트용 — dir/artifacts/hash[:2]/hash에 참조되지 않는 blob 파일을
// 만들고 mtime을 gcOrphanMinAge보다 오래된 시각으로 되돌린다(age gate를 통과해 GC 삭제
// 대상이 되도록). age gate 자체를 검증하는 TestGCOrphanBlobs_AgeGate는 이 헬퍼를 쓰지
// 않고 직접 mtime을 다룬다.
func writeAgedOrphanBlob(t *testing.T, storeDir, hash string) string {
	t.Helper()
	dir := filepath.Join(storeDir, "artifacts", hash[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, hash)
	if err := os.WriteFile(path, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * gcOrphanMinAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGCOrphanBlobs: 정상 등록된 artifact의 blob(참조됨)과, 등록 없이 artifacts/에 직접
// 만든 더미 blob 파일(재색인으로 raw_blob_hash가 교체돼 고아가 된 상황을 모사, 설계 §7)을
// 함께 둔 뒤 GC가 고아만 지우고 참조 blob은 남기는지 확인한다. 더미 blob은 age gate를
// 통과하도록 mtime을 오래된 시각으로 만든다(신규 등록 blob과 구분, 리뷰 P1).
func TestGCOrphanBlobs(t *testing.T) {
	s := openT(t)
	body := []byte("referenced content")
	if _, err := s.Register(t.Context(), Registration{
		StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: "/ref.txt", Kind: "file", SrcHash: "h-ref"},
		Chunks: []Chunk{{Ordinal: 0, Text: string(body)}},
	}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	refHash := hex.EncodeToString(sum[:])

	orphanHash := strings.Repeat("f", 64) // 64자 hex 모양이지만 어디에도 참조되지 않는 더미 해시
	orphanPath := writeAgedOrphanBlob(t, s.dir, orphanHash)
	orphanDir := filepath.Dir(orphanPath)

	removed, err := s.GCOrphanBlobs(t.Context())
	if err != nil {
		t.Fatalf("GCOrphanBlobs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(orphanDir, orphanHash)); !os.IsNotExist(err) {
		t.Fatalf("orphan blob 잔존: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "artifacts", refHash[:2], refHash)); err != nil {
		t.Fatalf("참조 blob이 GC에 삭제됨: %v", err)
	}
}

// TestGCOrphanBlobs_PreservesRawBlobHash: sources.raw_blob_hash로만 참조되는 blob(RawBlob
// 필드로 저장된 원본 HTML 등 — content_hash 집합에는 없다)도 GC가 지우면 안 된다(설계 §7 —
// GC는 content_hash·raw_blob_hash 양쪽 집합 모두를 참조로 인정해야 한다).
func TestGCOrphanBlobs_PreservesRawBlobHash(t *testing.T) {
	s := openT(t)
	raw := []byte("<html>raw source, not content-addressed via artifacts</html>")
	if _, err := s.Register(t.Context(), Registration{
		StoredBytes: []byte("extracted text"), MediaType: "text/plain",
		Source:  SourceMeta{URI: "https://example.com/p", Kind: "web", SrcHash: "h-web", Extraction: "readability"},
		Chunks:  []Chunk{{Ordinal: 0, Text: "extracted text"}},
		RawBlob: raw,
	}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	rawHash := hex.EncodeToString(sum[:])

	orphanHash := strings.Repeat("9", 64)
	writeAgedOrphanBlob(t, s.dir, orphanHash)

	removed, err := s.GCOrphanBlobs(t.Context())
	if err != nil {
		t.Fatalf("GCOrphanBlobs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want 1(orphan만)", removed)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "artifacts", rawHash[:2], rawHash)); err != nil {
		t.Fatalf("raw_blob_hash로만 참조되는 blob이 GC에 삭제됨: %v", err)
	}
}

// TestGCOrphanBlobs_AgeGate: 리뷰 P1(Fix Round 1) — Register가 blob을 DB 커밋 이전에
// 배치하므로(§3.5) 동시 GC가 "막 배치된 미참조 blob"을 고아로 오판해 지우면 저장소가
// 손상된다. 참조 없는 blob 2개 중 mtime이 최근(now, gcOrphanMinAge 이내)인 것은 건너뛰고
// 오래된(2시간 전) 것만 삭제해야 한다.
func TestGCOrphanBlobs_AgeGate(t *testing.T) {
	s := openT(t)

	recentHash := strings.Repeat("1", 64)
	recentDir := filepath.Join(s.dir, "artifacts", recentHash[:2])
	if err := os.MkdirAll(recentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recentPath := filepath.Join(recentDir, recentHash)
	if err := os.WriteFile(recentPath, []byte("recent"), 0o600); err != nil { // mtime=now, 건드리지 않음
		t.Fatal(err)
	}

	oldHash := strings.Repeat("2", 64)
	oldPath := writeAgedOrphanBlob(t, s.dir, oldHash) // mtime=2*gcOrphanMinAge 전

	removed, err := s.GCOrphanBlobs(t.Context())
	if err != nil {
		t.Fatalf("GCOrphanBlobs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want 1(오래된 것만)", removed)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("age gate 실패 — 최근 blob이 삭제됨: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("오래된 orphan 잔존: err=%v", err)
	}
}

func FuzzSnapUTF8(f *testing.F) {
	f.Add([]byte("가나다"), int64(1), int64(4))
	f.Add([]byte("hello\nworld"), int64(0), int64(11))
	f.Add([]byte{0xE0, 0x41, 0x80, 0x80}, int64(0), int64(4))
	f.Fuzz(func(t *testing.T, data []byte, start, end int64) {
		s, e := snapUTF8(data, start, end)
		if s < 0 || e < s || e > int64(len(data)) {
			t.Fatalf("range invariant 위반: start=%d end=%d len=%d", s, e, len(data))
		}
		if !utf8.Valid(data[s:e]) {
			t.Fatalf("잘못된 UTF-8 반환: %q", data[s:e])
		}
	})
}
