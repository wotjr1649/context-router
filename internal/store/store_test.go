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
	"strings"
	"sync"
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
	if _, err := s.Register(t.Context(), Registration{StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: "/perm.txt", Kind: "file", SrcHash: "hperm"},
		Chunks: []Chunk{{Ordinal: 0, Text: string(body)}}}); err != nil {
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
	reg := Registration{StoredBytes: []byte("same body\nline2\n"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/a.txt", Kind: "file", SrcHash: "h-a"},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: 16, LineStart: 1, LineEnd: 2, Text: "same body\nline2\n"}}}
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
	reg := Registration{StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: "/x.txt", Kind: "file", SrcHash: "hx"},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), Text: string(body)}}}
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
	base := Registration{StoredBytes: []byte("v1"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/f.txt", Kind: "file", SrcHash: "hash-v1"},
		Chunks: []Chunk{{Ordinal: 0, Text: "v1"}}}
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
	reg1 := Registration{StoredBytes: []byte("body v1"), MediaType: "text/plain",
		Source:  SourceMeta{URI: uri, Kind: "web", SrcHash: "h-v1", Extraction: "readability"},
		Chunks:  []Chunk{{Ordinal: 0, Text: "body v1"}},
		RawBlob: []byte("<html>v1</html>")}
	if _, err := s.Register(t.Context(), reg1); err != nil {
		t.Fatal(err)
	}
	reg2 := Registration{StoredBytes: []byte("body v2"), MediaType: "text/plain",
		Source:  SourceMeta{URI: uri, Kind: "web", SrcHash: "h-v2", Extraction: "full"},
		Chunks:  []Chunk{{Ordinal: 0, Text: "body v2"}},
		RawBlob: []byte("<html>v2 differs</html>")}
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
	reg := Registration{StoredBytes: []byte("v1"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/f.txt", Kind: "file", SrcHash: "h1"},
		Chunks: []Chunk{{Ordinal: 0, Text: "v1"}}}
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
	id, err := s.Register(t.Context(), Registration{StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/r.txt", Kind: "file", SrcHash: "h"},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), LineStart: 1, LineEnd: 3, Text: body}}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ReadRange(t.Context(), id, Selector{Kind: "line", LineStart: 2, LineEnd: 2})
	if err != nil || string(r.Text) != "bravo\n" {
		t.Fatalf("line sel: %v %q", err, r.Text)
	}
	// UTF-8 스냅: 한글 3바이트 중간을 요청해도 문자 경계로 스냅
	id2, _ := s.Register(t.Context(), Registration{StoredBytes: []byte("가나다"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/k.txt", Kind: "file", SrcHash: "hk"},
		Chunks: []Chunk{{Ordinal: 0, Text: "가나다"}}})
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
	id, err := s.Register(t.Context(), Registration{StoredBytes: []byte("alpha\nbravo\n"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/p.txt", Kind: "file", SrcHash: "hp"},
		Chunks: []Chunk{{Ordinal: 0, Text: "alpha\nbravo\n"}}})
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
			_, err := s.Register(t.Context(), Registration{StoredBytes: []byte(body), MediaType: "text/plain",
				Source: SourceMeta{URI: fmt.Sprintf("/c%d.txt", i), Kind: "file", SrcHash: fmt.Sprintf("h%d", i)},
				Chunks: []Chunk{{Ordinal: 0, Text: body}}})
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
	id, err := s.Register(t.Context(), Registration{StoredBytes: []byte("a\nb\nc\n"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/x.txt", Kind: "file", SrcHash: "hx"},
		Chunks: []Chunk{{Ordinal: 0, Text: "a\nb\nc\n"}}})
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
	id, err := s.Register(t.Context(), Registration{StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/b.txt", Kind: "file", SrcHash: "hb"},
		Chunks: []Chunk{{Ordinal: 0, Text: body}}})
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
	idA, err := s.Register(t.Context(), Registration{StoredBytes: []byte("artifact A body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/a2.txt", Kind: "file", SrcHash: "ha2"},
		Chunks: []Chunk{{Ordinal: 0, Text: "artifact A body"}}})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := s.Register(t.Context(), Registration{StoredBytes: []byte("artifact B body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/b2.txt", Kind: "file", SrcHash: "hb2"},
		Chunks: []Chunk{{Ordinal: 0, Text: "artifact B body"}}})
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
	id, err := s.Register(t.Context(), Registration{StoredBytes: []byte("body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/z.txt", Kind: "file", SrcHash: "hz"},
		Chunks: []Chunk{{Ordinal: 0, Text: "body"}}})
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
	info := SourceInfo{URI: filepath.ToSlash(file), Kind: "file", Size: fi.Size(),
		MtimeNS: fi.ModTime().UnixNano(), SrcHash: hex.EncodeToString(sum[:])}

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
	id, err := s.Register(t.Context(), Registration{StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: uri, Kind: "file", Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano(), SrcHash: hex.EncodeToString(sum[:])},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), LineStart: 1, LineEnd: 1, Text: string(body)}}})
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
	id, err := s.Register(t.Context(), Registration{StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/t.txt", Kind: "file", SrcHash: "ht"},
		Chunks: []Chunk{{Ordinal: 0, Text: body}}})
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

// TestMain: CTR_TEST_CHILD=1이면 자기 바이너리 재실행 자식 프로세스 모드 —
// TestOpen_ConcurrentFirstMigration의 2-프로세스 경합 재현용(go/os/exec 표준 helper-process 패턴).
func TestMain(m *testing.M) {
	if os.Getenv("CTR_TEST_CHILD") == "1" {
		os.Exit(runConcurrentOpenChild())
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
// 최초 Open(=최초 마이그레이션)을 개시했을 때 둘 다 성공해야 한다. busy_timeout이
// journal_mode(WAL)보다 먼저 적용되지 않으면 timeout 0 상태에서 WAL 전환이 경합해
// SQLITE_BUSY가 난다(게이트 7 심층, 세션01 Task9 발견). reps회 반복해 레이스 확률 확보.
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
			cmd.Env = append(os.Environ(),
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
	oldReg := Registration{StoredBytes: []byte("old body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/old.txt", Kind: "file", SrcHash: "h-old"},
		Chunks: []Chunk{{Ordinal: 0, Text: "old body"}}}
	oldID, err := s.Register(t.Context(), oldReg)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond) // indexed_at은 unix 초 단위 — 경계를 넘기려면 1초 이상 필요
	cutoff := time.Now().Unix()
	time.Sleep(1100 * time.Millisecond)

	newReg := Registration{StoredBytes: []byte("new body"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/new.txt", Kind: "file", SrcHash: "h-new"},
		Chunks: []Chunk{{Ordinal: 0, Text: "new body"}}}
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
	if _, err := s.Register(t.Context(), Registration{StoredBytes: []byte("keep me"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/keep.txt", Kind: "file", SrcHash: "h-keep"},
		Chunks: []Chunk{{Ordinal: 0, Text: "keep me"}}}); err != nil {
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

// TestGCOrphanBlobs: 정상 등록된 artifact의 blob(참조됨)과, 등록 없이 artifacts/에 직접
// 만든 더미 blob 파일(재색인으로 raw_blob_hash가 교체돼 고아가 된 상황을 모사, 설계 §7)을
// 함께 둔 뒤 GC가 고아만 지우고 참조 blob은 남기는지 확인한다.
func TestGCOrphanBlobs(t *testing.T) {
	s := openT(t)
	body := []byte("referenced content")
	if _, err := s.Register(t.Context(), Registration{StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: "/ref.txt", Kind: "file", SrcHash: "h-ref"},
		Chunks: []Chunk{{Ordinal: 0, Text: string(body)}}}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	refHash := hex.EncodeToString(sum[:])

	orphanHash := strings.Repeat("f", 64) // 64자 hex 모양이지만 어디에도 참조되지 않는 더미 해시
	orphanDir := filepath.Join(s.dir, "artifacts", orphanHash[:2])
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, orphanHash), []byte("orphan blob"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if _, err := s.Register(t.Context(), Registration{StoredBytes: []byte("extracted text"), MediaType: "text/plain",
		Source:  SourceMeta{URI: "https://example.com/p", Kind: "web", SrcHash: "h-web", Extraction: "readability"},
		Chunks:  []Chunk{{Ordinal: 0, Text: "extracted text"}},
		RawBlob: raw}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	rawHash := hex.EncodeToString(sum[:])

	orphanHash := strings.Repeat("9", 64)
	orphanDir := filepath.Join(s.dir, "artifacts", orphanHash[:2])
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, orphanHash), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

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
