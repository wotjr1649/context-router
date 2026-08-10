package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
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

// TestRegister_ReindexUpdatesSourceKind(최종 리뷰 C6): 같은 URI를 다른 kind로 재등록하면
// source_kind도 최신 값으로 갱신돼야 한다 — 과거엔 ON CONFLICT DO UPDATE가 이 컬럼을 빼먹어
// inline:<title> 충돌(§5 한계) 시 provenance가 최초 kind에 고정됐다(CAS 경로는 갱신하므로 정합).
func TestRegister_ReindexUpdatesSourceKind(t *testing.T) {
	s := openT(t)
	uri := "inline:Read"
	reg1 := Registration{
		StoredBytes: []byte("v1"), MediaType: "text/plain",
		Source: SourceMeta{URI: uri, Kind: "inline", SrcHash: "k1"},
		Chunks: []Chunk{{Ordinal: 0, Text: "v1"}},
	}
	if _, err := s.Register(t.Context(), reg1); err != nil {
		t.Fatal(err)
	}
	reg2 := Registration{
		StoredBytes: []byte("v2"), MediaType: "text/plain",
		Source: SourceMeta{URI: uri, Kind: "hook", SrcHash: "k2"},
		Chunks: []Chunk{{Ordinal: 0, Text: "v2"}},
	}
	if _, err := s.Register(t.Context(), reg2); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := s.reader.QueryRow("SELECT source_kind FROM sources WHERE uri=?", uri).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "hook" {
		t.Fatalf("source_kind=%q want %q (재등록 후 stale provenance)", kind, "hook")
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
			// 자식은 signal 파일을 시한 없이 폴링한다 — signal 기입 전에 테스트가 이탈하면
			// 상한 없이 남는다. main_test.go의 spawnCtr과 같은 안전망(정상 Wait 후에는 no-op).
			t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
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
		// 시한 없는 signal 폴링 — 이탈 시 상한 없는 잔존을 막는다(spawnCtr과 같은 형태).
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
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
	// 시한 없는 signal 폴링 — 아래 Kill이 t.Fatalf로 이탈하는 갈래(kill 실패·readiness
	// 타임아웃)를 포함해 잔존을 막는다(spawnCtr과 같은 형태).
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
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

// ledgerSchemaGuard — 픽스처가 정말 의도한 이관 계단에 도달했는지 PRAGMA table_info로 확인한다.
// 계단마다 테스트가 따로 있는데 이 가드가 없으면 픽스처 실수 하나가 **어느 계단의 테스트인지를
// 조용히 바꿔 놓고**(예: 열을 빼먹어 한 계단 아래로 미끄러짐) 그래도 통과한다.
func ledgerSchemaGuard(t *testing.T, dir string, want, absent []string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+"?mode=ro")
	if err != nil {
		t.Fatalf("사전 가드 open: %v", err)
	}
	cols, err := ledgerColumns(db)
	closeErr := db.Close()
	if err != nil {
		t.Fatalf("사전 가드 ledgerColumns: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("사전 가드 Close: %v", closeErr)
	}
	for _, c := range want {
		if !cols[c] {
			t.Fatalf("픽스처가 의도한 계단이 아니다: %q가 있어야 한다 — 실제 %v", c, cols)
		}
	}
	for _, c := range absent {
		if cols[c] {
			t.Fatalf("픽스처가 의도한 계단이 아니다: %q가 없어야 한다 — 실제 %v", c, cols)
		}
	}
}

// TestLedgerFetchStatsNoLedgerTable: ledger.db는 있는데 `ledger` 테이블이 없는 계단.
// **총 호출조차 측정값이 아니다** — 0을 찍으면 "회수가 한 번도 없었다"로 읽히는데 실제로는
// 아직 아무것도 못 읽은 상태다(v0.19.1 릴리스 패스가 doctor [14] free=0B에서 고친 것과 같은
// 결함). LedgerOK=false가 그 구분을 호출부까지 나른다.
func TestLedgerFetchStatsNoLedgerTable(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE other(x INTEGER)`); err != nil { // 파일은 있되 ledger는 없다
		t.Fatalf("무관 테이블: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ledgerSchemaGuard(t, dir, nil, []string{"tool", "artifact_id", "artifact_age_s", "shadow_owned"})

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("ledger 테이블 부재에서 오류가 났다: %v", err)
	}
	if fs.LedgerOK || fs.OutcomeOK || fs.ShadowOK {
		t.Fatalf("측정 못 한 계단인데 측정 표식이 섰다: %+v", fs)
	}
}

// TestLedgerFetchStatsToleratesOldSchema: 새 열이 없는 옛 ledger.db를 만나도 실패하지 않는다.
// ALTER는 writable Open에서만 도는데 LedgerFetchStats는 ledger.db를 따로 read-only로 연다 —
// 새 바이너리의 stats가 아직 이관되지 않은 원장을 먼저 만날 수 있다(설계 v0.20 D103 계약 7).
// 총 호출(Calls)은 옛 원장에서도 읽힌다 — 그 열은 처음부터 있었다.
func TestLedgerFetchStatsToleratesOldSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger(
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
		bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("옛 스키마 생성: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms) VALUES(1,'ctr_fetch',0,10,1)`,
	); err != nil {
		t.Fatalf("레거시 행: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ledgerSchemaGuard(t, dir, []string{"tool"}, []string{"artifact_id", "artifact_age_s", "shadow_owned"})

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("옛 스키마에서 오류가 났다: %v", err)
	}
	if fs.Calls != 1 {
		t.Fatalf("총 호출=%d, 기대 1(레거시 행도 센다)", fs.Calls)
	}
	if fs.Resolved != 0 || fs.Missed != 0 {
		t.Fatalf("레거시 행이 해소/미해소로 셌다: %+v", fs)
	}
	// 이 계단에서 0인 해소·미해소는 **측정값이 아니다** — 호출부가 0으로 찍으면 안 된다.
	if !fs.LedgerOK {
		t.Fatalf("총 호출은 읽었는데 LedgerOK=false: %+v", fs)
	}
	if fs.OutcomeOK || fs.ShadowOK {
		t.Fatalf("나이 열이 없는데 측정 표식이 섰다: %+v", fs)
	}
}

// TestLedgerFetchStatsPartialMigration: 세 ALTER는 서로 독립이라 **일부만 성공한 상태가 도달
// 가능하다**. 그 상태에서 없는 열을 지명하면 경성 오류가 나므로, 읽는 쪽은 계단 셋으로 퇴화한다
// (설계 v0.20 D103 계약 7): ①`artifact_id`·`artifact_age_s` 중 하나라도 없으면 총 호출만,
// ②둘 다 있고 `shadow_owned`가 없으면 해소·미해소까지, ③셋 다 있어야 귀속·분위수까지.
// 이 픽스처는 ①(나이 열만 없다)을 세운다 — ②는 TestLedgerFetchStatsShadowColumnMissing이다.
func TestLedgerFetchStatsPartialMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger(
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
		bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0, artifact_id INTEGER)`); err != nil { // age 열만 없다
		t.Fatalf("부분 이관 스키마: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ledgerSchemaGuard(t, dir, []string{"artifact_id"}, []string{"artifact_age_s", "shadow_owned"})

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("부분 이관에서 오류가 났다: %v", err)
	}
	if fs.Resolved != 0 || fs.Missed != 0 {
		t.Fatalf("부분 이관에서 0이 아닌 값: %+v", fs)
	}
	if !fs.LedgerOK || fs.OutcomeOK || fs.ShadowOK {
		t.Fatalf("부분 이관의 측정 표식이 틀렸다: %+v (기대 LedgerOK만 true)", fs)
	}
}

// TestLedgerColumnsAddedOnWritableOpen: writable Open이 세 열을 **실제로** 붙인다.
// 판정을 PRAGMA table_info로 하는 이유: LedgerFetchStats가 열 부재를 관용하므로, 반환값만
// 보면 ALTER를 아예 넣지 않은 구현도 0을 내며 통과한다. 두 번 열어도 아무 일이 없다 — 옛
// 형태는 "duplicate column name"을 내고 best-effort 관례가 그것을 삼켰지만, 이제는 이관이
// 열 부재를 먼저 보므로 둘째 Open에서 DDL이 한 문장도 돌지 않는다(소견 F11).
func TestLedgerColumnsAddedOnWritableOpen(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("첫 Close: %v", err)
	}
	st2 := openAt(t, dir) // 두 번째 Open에서 ALTER가 중복 실패한다 — 그래도 열려야 한다
	if err := st2.Close(); err != nil {
		t.Fatalf("둘째 Close: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+"?mode=ro")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	cols, err := ledgerColumns(db)
	if err != nil {
		t.Fatalf("ledgerColumns: %v", err)
	}
	if !cols["artifact_id"] || !cols["artifact_age_s"] || !cols["shadow_owned"] {
		t.Fatalf("writable Open이 세 열을 붙이지 않았다: %v", cols)
	}
}

// seedLedgerSchema — ledger.db를 원하는 이관 계단으로 **직접** 만든다. 다섯 옛 열은 항상 있고
// extra로 준 새 열만 더 붙는다. writable Open은 이제 빠진 열을 메우므로(migrateLedger) 부분
// 이관 상태를 Open으로는 만들 수 없다 — 쓰는 쪽의 관용을 보려면 이렇게 시딩해야 한다.
func seedLedgerSchema(t *testing.T, dir string, extra ...string) {
	t.Helper()
	cols := `id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
		bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0`
	for _, c := range extra {
		cols += ", " + c + " INTEGER"
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
	if err != nil {
		t.Fatalf("원장 시딩 open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger(` + cols + `)`); err != nil {
		t.Fatalf("원장 시딩 CREATE: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("원장 시딩 Close: %v", err)
	}
}

// storeOverLedger — 시딩된 ledger.db 위에 Store를 그 스키마 **그대로** 얹는다(이관하지 않는다).
// 생산 코드가 Open에서 하는 것과 같은 방식으로 열 집합을 잡는다 — ledgerColumns가 원천이다.
func storeOverLedger(t *testing.T, dir string) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
	if err != nil {
		t.Fatalf("원장 open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	cols, err := ledgerColumns(db)
	if err != nil {
		t.Fatalf("ledgerColumns: %v", err)
	}
	return &Store{dir: dir, ledger: db, ledgerCols: cols}
}

// TestLedgerAppendFetchPartialMigrationRecordsOutcome — 릴리스 패스 소견 F1. 훅이 2000 ms
// deadline에 죽어 `shadow_owned`만 못 붙은 원장이 도달 가능한데(계약 7), 쓰는 쪽의 INSERT가
// 세 열을 다 지명하면 SQLite가 그 문장을 거절하고 best-effort 관례가 오류를 삼켜 **해소도
// 미해소도 한 행 없이 통째로 사라진다**. 읽는 쪽은 그 계단에서 OutcomeOK=true를 세우므로
// `stats`는 그 0을 측정값으로 렌더한다 — 아무것도 안 잰 원장이 관측으로 들어간다.
// 쓰는 쪽도 읽는 쪽과 같은 계단을 관용해야 한다: 있는 열까지만 적고 퇴화한다.
func TestLedgerAppendFetchPartialMigrationRecordsOutcome(t *testing.T) {
	dir := t.TempDir()
	seedLedgerSchema(t, dir, "artifact_id", "artifact_age_s")
	ledgerSchemaGuard(t, dir, []string{"artifact_id", "artifact_age_s"}, []string{"shadow_owned"})

	st := storeOverLedger(t, dir)
	st.LedgerAppendFetch(t.Context(), 0, 1, 7, agePtr(30), true) // 해소
	st.LedgerAppendFetch(t.Context(), 0, 1, 0, nil, false)       // 미해소

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Calls != 2 {
		t.Fatalf("Calls=%d want 2 — 부분 이관에서 행이 통째로 버려졌다(F1): %+v", fs.Calls, fs)
	}
	if fs.Resolved != 1 || fs.Missed != 1 {
		t.Fatalf("Resolved=%d Missed=%d want 1/1 — 있는 두 열까지는 적어야 한다: %+v", fs.Resolved, fs.Missed, fs)
	}
	if fs.Legacy != 0 {
		t.Fatalf("Legacy=%d want 0 — 두 열이 있는데 레거시로 적혔다: %+v", fs.Legacy, fs)
	}
	if !fs.OutcomeOK || fs.ShadowOK {
		t.Fatalf("계단 표식이 틀렸다: %+v (기대 OutcomeOK만 true)", fs)
	}
}

// TestLedgerAppendFetchPreMigrationRecordsCall — 소견 F1의 아래 계단. 새 열이 하나도 없는
// 원장(구 바이너리가 만들고 아직 아무도 이관하지 않은 상태)에서도 **호출 자체는 남아야 한다**.
// 남은 행은 두 새 열이 없으니 이관 뒤에 레거시로 읽히는데, 그게 정확한 진술이다 — 결과를
// 표현할 수 없던 시점의 기록이다. 통째로 버리면 `calls`마저 아래로 편향된다.
func TestLedgerAppendFetchPreMigrationRecordsCall(t *testing.T) {
	dir := t.TempDir()
	seedLedgerSchema(t, dir)
	ledgerSchemaGuard(t, dir, []string{"tool"}, []string{"artifact_id", "artifact_age_s", "shadow_owned"})

	st := storeOverLedger(t, dir)
	st.LedgerAppendFetch(t.Context(), 0, 1, 7, agePtr(30), true)

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Calls != 1 {
		t.Fatalf("Calls=%d want 1 — 이관 전 원장에서 행이 버려졌다(F1): %+v", fs.Calls, fs)
	}
	if !fs.LedgerOK || fs.OutcomeOK || fs.ShadowOK {
		t.Fatalf("계단 표식이 틀렸다: %+v (기대 LedgerOK만 true)", fs)
	}
}

// TestMigrateLedgerAddsOnlyMissingColumns — 소견 F11. 이관은 `ledgerColumns`가 없다고 한 열에만
// ALTER를 건다. 계단 둘(전무·부분)에서 시작해 셋을 다 채우는지, 그리고 반환한 열 집합이 실제
// 스키마와 같은지를 함께 본다 — 반환값이 곧 쓰는 쪽의 계단 판정이라 둘이 갈리면 F1이 되돌아온다.
func TestMigrateLedgerAddsOnlyMissingColumns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"이관 전", nil},
		{"부분 이관", []string{"artifact_id"}},
		{"이미 완전", []string{"artifact_id", "artifact_age_s", "shadow_owned"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seedLedgerSchema(t, dir, tc.extra...)
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()

			got := migrateLedger(db)
			for _, c := range ledgerFetchColumns {
				if !got[c] {
					t.Fatalf("반환 집합에 %q가 없다: %v", c, got)
				}
			}
			// 반환값을 믿지 않고 스키마를 다시 읽는다 — 둘이 갈리는 것이 이 테스트가 잡는 것이다.
			fresh, err := ledgerColumns(db)
			if err != nil {
				t.Fatalf("재확인 ledgerColumns: %v", err)
			}
			for _, c := range ledgerFetchColumns {
				if !fresh[c] {
					t.Fatalf("스키마에 %q가 실제로 없다: %v", c, fresh)
				}
			}
		})
	}
}

// TestMigrateLedgerSecondPassIsSilent — 소견 F11의 요지. 이관이 끝난 원장에 다시 걸어도 DDL이
// 한 문장도 돌지 않는다. 관측 지점은 경고다: 게이트 없이 세 ALTER를 그냥 던지면 셋 다
// "duplicate column name"으로 실패하고, 실패를 경고로 내는 이 구현에서는 그것이 매 Open마다
// 경고 세 줄로 나타난다. 침묵이 곧 "안 돌았다"의 증거다.
func TestMigrateLedgerSecondPassIsSilent(t *testing.T) {
	dir := t.TempDir()
	seedLedgerSchema(t, dir)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	migrateLedger(db) // 첫 번째 — 여기서 세 열이 붙는다

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	migrateLedger(db) // 두 번째 — 아무 일도 없어야 한다
	if logBuf.Len() != 0 {
		t.Fatalf("이관 뒤 재실행이 조용하지 않다 — DDL이 또 돌았다(F11):\n%s", logBuf.String())
	}
}

// TestMigrateLedgerWarnsOnDDLFailure — 소견 F11의 나머지 절반. `_, _ =` 관례가 진짜 실패와
// "이미 이관됨"을 구분 불가능하게 만드는 것이 F1의 침묵 상태에 경고 한 줄 없이 도달하는 경로다.
// ensureIndexes가 색인마다 하는 것과 같이 열마다 한 줄을 남기고, 첫 실패에서 나머지를 멈추지
// 않는다. 실패는 read-only 핸들로 주입한다 — 실제 권한 사고와 같은 오류이고, 그 핸들에서도
// PRAGMA table_info는 읽히므로 이관 판정 자체는 정상으로 돈다.
func TestMigrateLedgerWarnsOnDDLFailure(t *testing.T) {
	dir := t.TempDir()
	seedLedgerSchema(t, dir)
	ro, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	defer ro.Close()

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	got := migrateLedger(ro)

	for _, c := range ledgerFetchColumns {
		if got[c] {
			t.Fatalf("붙지 않은 열 %q가 반환 집합에 들었다 — 쓰는 쪽이 없는 열을 지명하게 된다: %v", c, got)
		}
		if !strings.Contains(logBuf.String(), c) {
			t.Fatalf("%q의 실패가 경고에 없다 — 첫 실패에서 멈췄거나 삼켰다:\n%s", c, logBuf.String())
		}
	}
	if n := strings.Count(logBuf.String(), "level=WARN"); n != len(ledgerFetchColumns) {
		t.Fatalf("경고 줄 수=%d want %d:\n%s", n, len(ledgerFetchColumns), logBuf.String())
	}
}

// TestMigrateLedgerSkipsWhenSchemaUnknown — 소견 F11의 원칙("모르면 던지지 않는다")을 판정할 수
// 없는 두 입력에 건다. ① 스키마를 아예 못 읽는 핸들: 경고는 그 사실 하나뿐이어야 하고, 열마다
// 하나씩 더 붙으면 그것이 옛 무조건 ALTER다. ② `ledger` 테이블이 없는 원장(위 CREATE가 실패한
// 상태 — 그쪽이 이미 경고를 냈다): 붙일 곳이 없으므로 완전한 침묵이 옳다.
// 둘 다 반환 집합에 새 열이 없어야 한다 — 있으면 쓰는 쪽이 없는 열을 지명한다(소견 F1).
func TestMigrateLedgerSkipsWhenSchemaUnknown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		db       func(t *testing.T) *sql.DB
		wantWarn int
	}{
		{"스키마 판정 불가", func(t *testing.T) *sql.DB {
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "ledger.db"))+pragmas)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := db.Close(); err != nil { // 닫힌 핸들 — PRAGMA도 ALTER도 못 돈다
				t.Fatalf("Close: %v", err)
			}
			return db
		}, 1},
		{"ledger 테이블 없음", func(t *testing.T) *sql.DB {
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "ledger.db"))+pragmas)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.Exec(`CREATE TABLE other(x INTEGER)`); err != nil {
				t.Fatalf("무관 테이블: %v", err)
			}
			return db
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db(t)
			var logBuf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			got := migrateLedger(db)
			for _, c := range ledgerFetchColumns {
				if got[c] {
					t.Fatalf("판정 불가인데 %q가 반환 집합에 들었다: %v", c, got)
				}
			}
			if n := strings.Count(logBuf.String(), "level=WARN"); n != tc.wantWarn {
				t.Fatalf("경고 줄 수=%d want %d — ALTER를 던졌다(F11):\n%s", n, tc.wantWarn, logBuf.String())
			}
		})
	}
}

// TestOpenSnapshotsLedgerColumns — 위 F11 테스트들을 실제 Open 경로에 묶는다. 둘을 본다:
// ① Open이 쓰는 쪽에 넘기는 열 스냅샷이 **실제 스키마와 같다** — 이것이 F1 수정이 딛는
// 불변식이고, OpenContext가 migrateLedger를 안 부르면 스냅샷이 비어 쓰는 쪽이 영원히 아래
// 계단으로 퇴화한다. ② 이관이 끝난 원장을 다시 여는 둘째 Open이 조용하다 — 하루 약 295회의
// 훅 포착 + 서버 기동마다 도는 자리라, 게이트가 빠지면 그 전부가 경고 세 줄이 된다.
func TestOpenSnapshotsLedgerColumns(t *testing.T) {
	dir := t.TempDir()
	if err := openAt(t, dir).Close(); err != nil {
		t.Fatalf("첫 Close: %v", err)
	}
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	st := openAt(t, dir)
	for _, c := range ledgerFetchColumns {
		if !st.ledgerCols[c] {
			t.Fatalf("Open이 넘긴 스냅샷에 %q가 없다 — 쓰는 쪽이 아래 계단으로 퇴화한다: %v", c, st.ledgerCols)
		}
		if strings.Contains(logBuf.String(), c) {
			t.Fatalf("둘째 writable Open이 %q에 DDL을 또 걸었다(F11):\n%s", c, logBuf.String())
		}
	}
	// 스냅샷이 실제 스키마와 갈리면 F1이 되돌아온다 — 반환값이 아니라 DB를 다시 읽어 대조한다.
	fresh, err := ledgerColumns(st.ledger)
	if err != nil {
		t.Fatalf("ledgerColumns: %v", err)
	}
	if !reflect.DeepEqual(st.ledgerCols, fresh) {
		t.Fatalf("스냅샷 %v != 실제 스키마 %v", st.ledgerCols, fresh)
	}
}

// ledgerExec — 원장에 SQL 한 문장을 직접 건다(구 바이너리의 다섯 열 기록을 흉내내거나 워터마크를
// 직접 확인할 때 쓴다). Store를 거치지 않는 것이 요지다 — 흉내낼 기록자가 이 코드가 아니다.
func ledgerExec(t *testing.T, dir, q string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
	if err != nil {
		t.Fatalf("원장 exec open: %v", err)
	}
	_, execErr := db.Exec(q)
	closeErr := db.Close()
	if execErr != nil {
		t.Fatalf("원장 exec %q: %v", q, execErr)
	}
	if closeErr != nil {
		t.Fatalf("원장 exec Close: %v", closeErr)
	}
}

// ledgerUserVersion — ledger.db의 워터마크를 read-only로 읽는다(사전 가드·사후 대조 공용).
func ledgerUserVersion(t *testing.T, dir string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+"?mode=ro")
	if err != nil {
		t.Fatalf("워터마크 open: %v", err)
	}
	var v int64
	scanErr := db.QueryRow(`PRAGMA user_version`).Scan(&v)
	closeErr := db.Close()
	if scanErr != nil {
		t.Fatalf("워터마크 읽기: %v", scanErr)
	}
	if closeErr != nil {
		t.Fatalf("워터마크 Close: %v", closeErr)
	}
	return v
}

// oldWriterFetchRow — 옛 바이너리(v0.19.1)의 ctr_fetch 기록. 다섯 열만 지명하므로 새 두 열이
// NULL로 남아 레거시로 읽힌다 — F2가 말하는 "옛 서버가 유일한 기록자" 상태의 실제 산물이다.
const oldWriterFetchRow = `INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms)
	VALUES(1,'ctr_fetch',0,10,1)`

// TestLedgerWatermarkSurvivesLaterOpens — 릴리스 패스 소견 F2의 핵심. 세션이 열린 채 새
// 바이너리를 깔면 훅이 원장을 이관하는 동안 **옛 서버가 ctr_fetch의 유일한 기록자**로 남는다.
// 그 행들은 레거시로 읽히므로 표식이 하나도 서지 않고(열은 다 있다) 나머지 칸이 다 숫자로
// 찍히는데, 해소·미해소가 0이라 결정표가 행 2("채택의 문제")로 떨어진다 — 할 일은 서버를 다시
// 띄우는 것인데. 이 수가 그 상태를 받는 행 0b를 발화시킨다.
//
// **그리고 훅은 하루 약 295번 뛴다.** 이관이 돌 때마다 워터마크를 다시 찍으면 그다음 훅이
// 워터마크를 옛 서버의 행들 **위로** 올려 경보를 지운다 — F2 시나리오의 뒤쪽 절반이 앞쪽
// 절반의 증거를 몇 분 만에, 영구히 없앤다. 그래서 이 테스트의 마지막 Open이 본체다.
func TestLedgerWatermarkSurvivesLaterOpens(t *testing.T) {
	dir := t.TempDir()
	// ① 이관 전 역사 두 행 — 정상 상태다. 경보에 절대 들면 안 된다.
	seedLedgerSchema(t, dir)
	ledgerSchemaGuard(t, dir, []string{"tool"}, ledgerFetchColumns)
	ledgerExec(t, dir, oldWriterFetchRow)
	ledgerExec(t, dir, oldWriterFetchRow)
	if got := ledgerUserVersion(t, dir); got != 0 {
		t.Fatalf("사전 가드: 이관 전 워터마크=%d want 0", got)
	}

	// ② 훅이 새 바이너리로 원장을 이관한다 — 여기서 워터마크가 찍힌다(= max(id)+1 = 3).
	if err := openAt(t, dir).Close(); err != nil {
		t.Fatalf("이관 Open Close: %v", err)
	}
	ledgerSchemaGuard(t, dir, ledgerFetchColumns, nil)
	mark := ledgerUserVersion(t, dir)
	if mark != 3 {
		t.Fatalf("워터마크=%d want 3(= 이관 전 2행의 max(id)+1)", mark)
	}

	// ③ 옛 서버가 아직 살아서 한 행 더 적는다 — 이것이 경보의 대상이다.
	ledgerExec(t, dir, oldWriterFetchRow)
	// ④ 그리고 새 바이너리도 정상으로 적는다(해소 하나·미해소 하나).
	st := openAt(t, dir)
	st.LedgerAppendFetch(t.Context(), 0, 1, 9, agePtr(30), true)
	st.LedgerAppendFetch(t.Context(), 0, 1, 0, nil, false)

	// ⑤ **본체**: 훅이 또 뛴다. 이관은 이미 끝났으므로 워터마크가 움직이면 안 된다.
	if err := openAt(t, dir).Close(); err != nil {
		t.Fatalf("둘째 훅 Open Close: %v", err)
	}
	if got := ledgerUserVersion(t, dir); got != mark {
		t.Fatalf("워터마크가 %d→%d로 움직였다 — 다음 훅이 경보를 지운다(F2)", mark, got)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if !fs.MigrateMarkOK {
		t.Fatalf("워터마크를 찍었는데 MigrateMarkOK=false: %+v", fs)
	}
	if fs.Legacy != 3 {
		t.Fatalf("Legacy=%d want 3(이관 전 2 + 옛 기록자 1)", fs.Legacy)
	}
	if fs.LegacyAfterMigrate != 1 {
		t.Fatalf("LegacyAfterMigrate=%d want 1 — 이관 전 역사가 새거나 옛 기록자의 행을 놓쳤다: %+v",
			fs.LegacyAfterMigrate, fs)
	}
	if fs.Resolved != 1 || fs.Missed != 1 {
		t.Fatalf("Resolved=%d Missed=%d want 1/1 — 결과 행이 경보 셈에 섞였는지 함께 본다: %+v",
			fs.Resolved, fs.Missed, fs)
	}
}

// TestLedgerWatermarkNotMovedWhenLaterAlterCompletes — 위 테스트가 **못 잡는** 나머지 경로.
// 거기서는 둘째 Open이 붙일 열이 없어 "열을 붙였을 때만 찍는다"는 조건이 한 번만 참이고, 그
// 조건 홀로 표식을 지킨다. 여기서는 그 조건이 **두 번** 참이 된다.
//
// 도달 경로(소견 F11이 이름 붙인 그 상태): 앞선 실행이 앞 두 ALTER는 성공하고 셋째에서 잠금
// 경쟁이 busy_timeout을 넘겨 실패했다 — 죽은 게 아니라 루프를 끝냈으므로 **표식은 찍혔다**.
// 그 뒤 옛 기록자가 행을 남기고, 다음 실행이 셋째 열을 마저 붙인다. 이때 표식을 다시 쓰면
// 그 사이의 행이 워터마크 **아래로** 숨어 경보가 사라진다. 픽스처는 그 상태를 직접 만든다 —
// 열 하나만 골라 실패시키는 주입보다 도달 상태를 그대로 세우는 쪽이 정직하다.
func TestLedgerWatermarkNotMovedWhenLaterAlterCompletes(t *testing.T) {
	dir := t.TempDir()
	seedLedgerSchema(t, dir, "artifact_id", "artifact_age_s") // 셋째 ALTER만 실패했던 원장
	ledgerSchemaGuard(t, dir, []string{"artifact_id", "artifact_age_s"}, []string{"shadow_owned"})
	ledgerExec(t, dir, oldWriterFetchRow) // 이관 전 역사 1행(id=1)
	ledgerExec(t, dir, `PRAGMA user_version = 2`)
	if got := ledgerUserVersion(t, dir); got != 2 { // 사전 가드: 앞선 실행이 남긴 표식
		t.Fatalf("사전 가드: 워터마크=%d want 2", got)
	}
	ledgerExec(t, dir, oldWriterFetchRow) // 옛 기록자가 표식 뒤에 남긴 행(id=2) — 경보 대상

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	migrateLedger(db) // 셋째 열을 마저 붙인다 → "열을 붙였다"가 두 번째로 참이 되는 지점
	ledgerSchemaGuard(t, dir, ledgerFetchColumns, nil)

	if got := ledgerUserVersion(t, dir); got != 2 {
		t.Fatalf("워터마크가 2→%d로 움직였다 — 이관 뒤 레거시가 워터마크 아래로 숨는다", got)
	}
	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if !fs.MigrateMarkOK || fs.LegacyAfterMigrate != 1 {
		t.Fatalf("LegacyAfterMigrate=%d MigrateMarkOK=%v want 1/true: %+v",
			fs.LegacyAfterMigrate, fs.MigrateMarkOK, fs)
	}
}

// TestLedgerFetchStatsNoWatermarkIsUnjudgeable — 이 브랜치의 **앞선 빌드**가 열만 붙이고 표식은
// 안 남긴 원장. 그 원장의 레거시 행이 이관 앞인지 뒤인지는 되물을 방법이 없으므로, 전부 경보로
// 찍으면 정상 역사가 통째로 경보가 된다. 답은 계단 사다리가 다른 데서 내는 것과 같은
// **판정 불가**다 — MigrateMarkOK=false, 그리고 그 0은 "없다"가 아니라 "못 잼"이다.
// 표식이 없다고 writable Open이 뒤늦게 찍어 넣지도 않는다 — 늦게 찍은 워터마크는 실제 이관
// 시점보다 뒤라 그 사이의 옛 기록자 행을 "이관 전"으로 만든다.
func TestLedgerFetchStatsNoWatermarkIsUnjudgeable(t *testing.T) {
	dir := t.TempDir()
	seedLedgerSchema(t, dir, ledgerFetchColumns...) // 열은 다 있고 워터마크만 없다
	ledgerSchemaGuard(t, dir, ledgerFetchColumns, nil)
	ledgerExec(t, dir, oldWriterFetchRow)
	ledgerExec(t, dir, oldWriterFetchRow)
	if got := ledgerUserVersion(t, dir); got != 0 {
		t.Fatalf("사전 가드: 워터마크=%d want 0", got)
	}

	if err := openAt(t, dir).Close(); err != nil { // 붙일 열이 없다 → 표식도 안 찍는다
		t.Fatalf("Open Close: %v", err)
	}
	if got := ledgerUserVersion(t, dir); got != 0 {
		t.Fatalf("이관한 적 없는 원장에 워터마크 %d가 뒤늦게 찍혔다", got)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if !fs.OutcomeOK {
		t.Fatalf("열이 다 있는데 OutcomeOK=false: %+v", fs)
	}
	if fs.MigrateMarkOK {
		t.Fatalf("워터마크가 없는데 판정 가능으로 섰다: %+v", fs)
	}
	if fs.LegacyAfterMigrate != 0 {
		t.Fatalf("판정 불가인데 경보 수가 찍혔다: %+v", fs)
	}
	if fs.Legacy != 2 {
		t.Fatalf("Legacy=%d want 2 — 레거시 자체는 그대로 측정값이다: %+v", fs.Legacy, fs)
	}
}

// TestMarkLedgerMigratedFailuresAreWarnedNotFatal — 표식을 못 남기는 세 지점(현재값 읽기 ·
// max(id) 읽기 · PRAGMA 쓰기)에서 경고만 내고 넘어간다. 원장 전체가 best-effort이므로 표식
// 실패가 이관이나 Open을 막으면 안 되고, 못 찍힌 결과는 위 "판정 불가"로 정확히 퇴화한다.
func TestMarkLedgerMigratedFailuresAreWarnedNotFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   func(t *testing.T) *sql.DB
	}{
		{"현재값 못 읽음", func(t *testing.T) *sql.DB { // 닫힌 핸들 — PRAGMA 읽기부터 실패한다
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "ledger.db"))+pragmas)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			return db
		}},
		{"max(id) 못 읽음", func(t *testing.T) *sql.DB { // ledger 테이블이 없다
			dir := t.TempDir()
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+pragmas)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.Exec(`CREATE TABLE other(x INTEGER)`); err != nil {
				t.Fatalf("무관 테이블: %v", err)
			}
			return db
		}},
		{"PRAGMA 못 씀", func(t *testing.T) *sql.DB { // read-only 핸들 — 읽기 둘은 되고 쓰기만 막힌다
			dir := t.TempDir()
			seedLedgerSchema(t, dir)
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatalf("open ro: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			markLedgerMigrated(tc.db(t)) // panic도 오류 반환도 없다
			if n := strings.Count(logBuf.String(), "level=WARN"); n != 1 {
				t.Fatalf("경고 줄 수=%d want 1:\n%s", n, logBuf.String())
			}
		})
	}
}

// TestLedgerFetchStatsFullMigration: 완전 이관된 ledger.db를 SQL로 직접 시딩해 계약 1의 세
// 상태(레거시/미해소/해소)와 분위수 계산을 한 픽스처에서 함께 태운다. 위 두 테스트는 둘 다
// 새 열이 없거나 하나만 있는 스키마로 조기 반환 경로만 타므로, Resolved/Missed 집계 질의와
// AgeP50/AgeP90/AgeMax 루프는 그 어느 테스트에서도 실행되지 않는다(릴리스 리뷰 소견 —
// LedgerFetchStats 커버리지 44.4%, count=0 구간이 바로 이 두 블록). writer(Task 8a)가 아직
// 없으므로 SQL을 직접 써서 완전 이관 스키마를 만든다.
//
// 나이를 10/20/30/40/50 다섯 개(중복 없음, 삽입 순서는 뒤섞음 — ORDER BY가 실제로 정렬하게
// 만든다)로 고른 이유: Resolved=5면 오프셋이 2/3/4로 갈라져 P50·P90·Max가 서로 다른 행을
// 가리킨다 — 셋 중 하나라도 오프셋이 틀리면 그 하나만 값이 바뀐다. 미해소 2행(age=-1)을 함께
// 넣어 그 값이 분위수 정렬에 섞이지 않는지도 같이 확인한다(-1은 10보다 작으므로 섞였다면
// AgeP50/AgeP90/AgeMax가 바뀐다). ctr_search 행 하나(age=999)는 tool='ctr_fetch' 필터가
// 실제로 걸러내는지 본다 — 안 걸러지면 Calls=9, Resolved=6, AgeMax=999가 된다.
func TestLedgerFetchStatsFullMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger(
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
		bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0, artifact_id INTEGER, artifact_age_s INTEGER,
		shadow_owned INTEGER)`); err != nil {
		t.Fatalf("완전 이관 스키마: %v", err)
	}
	inserts := []string{
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(1,'ctr_fetch',NULL,NULL,NULL)`, // 레거시
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(2,'ctr_fetch',NULL,-1,NULL)`,   // 미해소
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(3,'ctr_fetch',NULL,-1,NULL)`,   // 미해소
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(4,'ctr_fetch',1,30,1)`,         // 해소 — 순서 섞음
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(5,'ctr_fetch',2,10,1)`,         // 해소
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(6,'ctr_fetch',3,50,1)`,         // 해소
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(7,'ctr_fetch',4,20,1)`,         // 해소
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(8,'ctr_fetch',5,40,1)`,         // 해소
		`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s,shadow_owned) VALUES(9,'ctr_search',6,999,1)`,       // 다른 도구 — 필터 확인
	}
	for _, q := range inserts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("행 삽입 %q: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// pre-guard: 픽스처가 실제로 완전 이관(세 열 다 있음) 상태인지 먼저 확인한다. 이게 없으면
	// 픽스처 실수가 이 테스트를 조기 반환 경로로 흘려보내면서도 통과시킨다 — 이 테스트가 잡으려는
	// 바로 그 가짜 커버리지 상태다.
	ledgerSchemaGuard(t, dir, []string{"artifact_id", "artifact_age_s", "shadow_owned"}, nil)

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Calls != 8 {
		t.Fatalf("Calls=%d want 8 (ctr_search 행은 제외)", fs.Calls)
	}
	if fs.Resolved != 5 {
		t.Fatalf("Resolved=%d want 5", fs.Resolved)
	}
	if fs.Missed != 2 {
		t.Fatalf("Missed=%d want 2", fs.Missed)
	}
	// 소견 F9: 레거시 행 수를 따로 낸다 — Calls 안에 섞여 있어 그 줄만 보면 결과 분모로 읽힌다.
	if fs.Legacy != 1 {
		t.Fatalf("Legacy=%d want 1(두 열 다 NULL인 이관 전 행)", fs.Legacy)
	}
	// 완전 이관에서만 세 표식이 다 선다 — 여기의 0은 전부 진짜 측정값이다.
	if !fs.LedgerOK || !fs.OutcomeOK || !fs.ShadowOK {
		t.Fatalf("완전 이관인데 측정 표식이 빠졌다: %+v", fs)
	}
	if fs.AgeP50 != 30 || fs.AgeP90 != 40 || fs.AgeMax != 50 {
		t.Fatalf("분위수 틀림: %+v want {AgeP50:30 AgeP90:40 AgeMax:50}", fs)
	}
}

// TestLedgerFetchStats_DirMissing: ledger.db가 아예 없는 디렉터리는 LedgerStats와 동일하게
// 빈 값 + nil이다(os.Stat 조기 반환 경로 — LedgerFetchStats의 os.Stat 분기 중 파일 미존재
// 쪽은 위 세 테스트 모두 ledger.db를 만들어 두므로 그전까지 커버되지 않았다).
func TestLedgerFetchStats_DirMissing(t *testing.T) {
	fs, err := LedgerFetchStats(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.db 미존재: err=%v want nil", err)
	}
	if fs != (FetchStat{}) {
		t.Fatalf("ledger.db 미존재: fs=%+v want 제로값", fs)
	}
}

// TestLastIndexedAtByHashUsesMaxOverSiblingArtifacts: 나이 시계가 마지막 포착이고 범위가
// content_hash다. 두 축을 한 번에 잰다 — 같은 바이트를 media_type 둘로 등록하면 artifact 행이
// 둘 생기는데(store.go:479의 조회 키가 (content_hash, media_type)), 퍼지 술어는 그 둘의 소스를
// 전부 본다(shadowOwnedFilter). **artifact 단위로 재는 구현은 이 테스트에서 떨어진다** —
// 형제 쪽이 최근 값을 쥐고 있기 때문이다.
func TestLastIndexedAtByHashUsesMaxOverSiblingArtifacts(t *testing.T) {
	st := openAt(t, t.TempDir())
	regSource(t, st, "aged", "text/plain", "shadow:Bash:first", "hook")
	regSource(t, st, "aged", "application/json", "shadow:Bash:second", "hook") // 같은 바이트, 다른 media_type

	old, recent := int64(1_000), int64(9_000)
	if _, err := st.writer.Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:first'`, old,
	); err != nil {
		t.Fatalf("첫 소스 시각: %v", err)
	}
	if _, err := st.writer.Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:second'`, recent,
	); err != nil {
		t.Fatalf("둘째 소스 시각: %v", err)
	}
	// 형제 둘 중 **오래된 쪽**의 artifact를 회수했다고 가정해도 나이는 최근 값이어야 한다.
	got, _, err := st.LastIndexedAtByHash(t.Context(), hashOf("aged"))
	if err != nil {
		t.Fatalf("LastIndexedAtByHash: %v", err)
	}
	if got != recent {
		t.Fatalf("LastIndexedAtByHash=%d, 기대 %d(형제 포함 최댓값)", got, recent)
	}
}

// TestLastIndexedAtByHashMissingIsZero: 소스가 없으면 (0, false, nil)이다 — 호출부가 나이를
// 0으로 두고 계속한다(회수 자체는 성공했다). 귀속도 false다 — hook 소스가 하나도 없으니
// 퍼지 술어도 이 hash를 고르지 않는다. 집계 1행의 두 값이 다 NULL로 오는 경로라, 스캔이
// NULL을 받아도 오류가 나지 않는다는 확인이기도 하다.
func TestLastIndexedAtByHashMissingIsZero(t *testing.T) {
	st := openAt(t, t.TempDir())
	got, owned, err := st.LastIndexedAtByHash(t.Context(), hashOf("nothing here"))
	if err != nil || got != 0 || owned {
		t.Fatalf("got=%d owned=%v err=%v, 기대 0/false/nil", got, owned, err)
	}
}

// TestLastIndexedAtByHashShadowMarkerAgreesWithPurgeFilter: 회수 시점에 박는 shadow 귀속
// 표식은 **퍼지가 실제로 지우는 집합과 같은 정의**여야 한다(릴리스 리뷰 소견 F4). 정의가
// 갈리면 나이 분포가 보존 창이 손대지도 않는 explicit 아티팩트로 채워지고, D104의 착수 조건
// (해소 30건)이 창의 길이에 대해 아무 말도 하지 않는 회수로 충족된다.
// **기대값을 손으로 적지 않는 것이 이 테스트의 요지다** — shadowOwnedFilter가 내는 집합을
// 그대로 읽어 대조한다. 표식 쪽이 둘째 정의를 갖는 순간 이 대조가 깨진다.
func TestLastIndexedAtByHashShadowMarkerAgreesWithPurgeFilter(t *testing.T) {
	st := openAt(t, t.TempDir())
	// 귀속 술어의 네 갈래를 다 태운다: hook 전용(귀속) · hook+file 공유(비귀속, 첫 NOT EXISTS) ·
	// file 전용(비귀속, hook JOIN) · 비-hook 소스가 raw_blob_hash로 참조(비귀속, 둘째 NOT EXISTS).
	hookOnly, shared, fileOnly, rawRef := "f4-hook-only", "f4-shared", "f4-file-only", "f4-raw-ref"
	regSource(t, st, hookOnly, "text/plain", "shadow:Bash:f4a", "hook")
	regSource(t, st, shared, "text/plain", "shadow:Bash:f4b", "hook")
	regSource(t, st, shared, "text/plain", "/tmp/f4b.txt", "file")
	regSource(t, st, fileOnly, "text/plain", "/tmp/f4c.txt", "file")
	regSource(t, st, rawRef, "text/plain", "shadow:Bash:f4d", "hook")
	if _, err := st.Register(t.Context(), Registration{ // web 소스가 같은 바이트를 raw_blob으로 보존
		StoredBytes: []byte("f4-web-extracted"), MediaType: "text/plain",
		Source:  SourceMeta{URI: "https://example.invalid/f4", Kind: "web", SrcHash: "sh-f4web"},
		RawBlob: []byte(rawRef),
	}); err != nil {
		t.Fatal(err)
	}

	// 퍼지가 보는 집합 그대로(예산 0 = 나이·건수 필터 없음 — PurgeHookOnly와 같은 인자).
	sel, args := shadowOwnedFilter(0, 0)
	rows, err := st.reader.Query(sel, args...)
	if err != nil {
		t.Fatalf("shadowOwnedFilter 질의: %v", err)
	}
	purgeable := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		purgeable[h] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	all := []string{hookOnly, shared, fileOnly, rawRef}
	// 사전 가드: 넷 중 정확히 하나(hook 전용)만 퍼지 대상이어야 아래 대조가 무언가를 증명한다.
	// 집합이 비거나 넷 다면 "표식 항상 false"·"항상 true" 구현이 통과해버린다.
	owned := 0
	for _, c := range all {
		if purgeable[hashOf(c)] {
			owned++
		}
	}
	if owned != 1 || !purgeable[hashOf(hookOnly)] {
		t.Fatalf("픽스처가 의도한 상태가 아니다: 퍼지 대상 %d개(기대 1 = hook 전용), 집합 크기=%d", owned, len(purgeable))
	}

	for _, c := range all {
		h := hashOf(c)
		at, got, err := st.LastIndexedAtByHash(t.Context(), h)
		if err != nil {
			t.Fatalf("LastIndexedAtByHash(%s): %v", c, err)
		}
		if got != purgeable[h] {
			t.Fatalf("%s: 귀속 표식=%v, 퍼지 술어=%v — 두 정의가 갈렸다", c, got, purgeable[h])
		}
		if at <= 0 {
			t.Fatalf("%s: 나이 시계=%d — 한 질의가 두 답을 다 내야 한다(왕복 하나)", c, at)
		}
	}
}

// TestLedgerAppendFetchRecordsShadowOwnership: 귀속 표식이 원장 행에 실제로 박힌다 —
// 해소는 1/0, **미해소는 NULL**이다(아티팩트가 없으니 귀속을 알 길이 없고, 모른다를 0으로
// 적으면 "explicit이었다"는 거짓 진술이 된다). 반환값이 아니라 열을 직접 읽는 이유는
// LedgerFetchStats가 열 부재를 관용하므로 집계만 보면 열을 안 쓴 구현도 통과하기 때문이다.
func TestLedgerAppendFetchRecordsShadowOwnership(t *testing.T) {
	st := openAt(t, t.TempDir())
	st.LedgerAppendFetch(t.Context(), 0, 1, 11, agePtr(100), true)  // 해소 · shadow 귀속
	st.LedgerAppendFetch(t.Context(), 0, 1, 12, agePtr(200), false) // 해소 · explicit
	st.LedgerAppendFetch(t.Context(), 0, 1, 0, nil, false)          // 미해소

	rows, err := st.ledger.Query(`SELECT artifact_id, shadow_owned FROM ledger WHERE tool='ctr_fetch' ORDER BY id`)
	if err != nil {
		t.Fatalf("원장 조회: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, owned sql.NullInt64
		if err := rows.Scan(&id, &owned); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%v/%v", nullStr(id), nullStr(owned)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{"11/1", "12/0", "NULL/NULL"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("(artifact_id/shadow_owned) = %v, 기대 %v", got, want)
	}
}

// nullStr — NullInt64를 대조용 문자열로. NULL과 0을 눈으로 갈라야 하는 대조라서 필요하다.
func nullStr(v sql.NullInt64) string {
	if !v.Valid {
		return "NULL"
	}
	return strconv.FormatInt(v.Int64, 10)
}

// agePtr — LedgerAppendFetch의 나이 인자(nil = 미상)를 리터럴에서 만드는 테스트 헬퍼.
func agePtr(v int64) *int64 { return &v }

// TestLedgerAppendFetchUnknownAgeIsNull: 나이를 **모르는** 해소와 **같은 초에 회수한** 해소를
// 원장이 갈라 적는다(소견 F6). 전자를 0으로 적으면 분위수에 진짜 0으로 들어가 분포를 아래로
// 끌어내리고, "창이 넉넉하다"는 결론이 측정하지 못한 회수에서 나온다. 미상은 NULL이고
// **해소로는 계속 센다** — fetch는 실제로 바이트를 돌려줬다.
func TestLedgerAppendFetchUnknownAgeIsNull(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	st.LedgerAppendFetch(t.Context(), 0, 1, 7, nil, true)       // 해소인데 나이 미상
	st.LedgerAppendFetch(t.Context(), 0, 1, 8, agePtr(0), true) // 진짜 0초 — 방금 포착한 것을 회수

	rows, err := st.ledger.Query(`SELECT artifact_age_s FROM ledger WHERE tool='ctr_fetch' ORDER BY id`)
	if err != nil {
		t.Fatalf("원장 조회: %v", err)
	}
	var got []string
	for rows.Next() {
		var age sql.NullInt64
		if err := rows.Scan(&age); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		got = append(got, nullStr(age))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if want := []string{"NULL", "0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("artifact_age_s = %v, 기대 %v — 미상과 0초가 같은 값으로 적혔다", got, want)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Resolved != 2 {
		t.Fatalf("Resolved=%d want 2 — 나이를 몰라도 해소는 해소다", fs.Resolved)
	}
	if fs.ShadowResolved != 1 {
		t.Fatalf("ShadowResolved=%d want 1 — 나이 미상 행이 분위수 모집단에 들었다", fs.ShadowResolved)
	}
}

// TestLedgerFetchStatsRestrictsAgeToShadowOwned: 나이 분위수와 착수 조건이 읽는 수는
// **퍼지 대상(shadow 귀속) 해소만** 본다(소견 F4 — "행만"이라고 적혀 있었는데 분위수의 표본은
// 소견 F5 이후 아티팩트당 하나다). explicit 아티팩트는 영원히 안 지워지므로
// 그 회수 나이가 분포에 섞이면 "창이 넉넉하다"는 결론이 창과 무관한 데이터에서 나온다.
// 비귀속 행에 귀속 행보다 큰 나이를 심어 — 섞이면 p90·max가 즉시 달라진다.
func TestLedgerFetchStatsRestrictsAgeToShadowOwned(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	for i, age := range []int64{10, 20, 30, 40, 50} {
		st.LedgerAppendFetch(t.Context(), 0, 1, int64(i)+1, &age, true)
	}
	for i, age := range []int64{100_000, 200_000} { // explicit — 창이 손대지 않는 나이
		st.LedgerAppendFetch(t.Context(), 0, 1, int64(i)+100, &age, false)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Resolved != 7 {
		t.Fatalf("Resolved=%d want 7 — 해소 건수 자체는 귀속과 무관하다", fs.Resolved)
	}
	if fs.ShadowResolved != 5 {
		t.Fatalf("ShadowResolved=%d want 5", fs.ShadowResolved)
	}
	if fs.AgeP50 != 30 || fs.AgeP90 != 40 || fs.AgeMax != 50 {
		t.Fatalf("분위수에 explicit 나이가 섞였다: %+v want p50=30 p90=40 max=50", fs)
	}
}

// TestLedgerFetchStatsCountsDistinctShadowArtifacts: 착수 조건은 **행이 아니라 아티팩트**를
// 센다(소견 F5). ctr_fetch는 기본 16 KiB까지만 돌려주므로 큰 아티팩트 하나를 읽는 데 여러 번
// 불리고, 그 한 번의 페이징 폭주가 "해소 30건"을 채우고 분위수까지 지배할 수 있다. 행 수와
// 아티팩트 수를 나란히 내면 그 집중이 눈에 보인다.
func TestLedgerFetchStatsCountsDistinctShadowArtifacts(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	for _, age := range []int64{10, 20, 30} { // 아티팩트 1을 세 번 페이징
		st.LedgerAppendFetch(t.Context(), 0, 1, 1, &age, true)
	}
	st.LedgerAppendFetch(t.Context(), 0, 1, 2, agePtr(40), true)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.ShadowResolved != 4 {
		t.Fatalf("ShadowResolved=%d want 4(행 수)", fs.ShadowResolved)
	}
	if fs.ShadowArtifacts != 2 {
		t.Fatalf("ShadowArtifacts=%d want 2(distinct artifact_id) — 페이징이 착수 조건을 채웠다", fs.ShadowArtifacts)
	}
}

// seedPagedFetchLedger — 행 가중과 아티팩트 가중이 **다른 답을 내는** 원장을 만든다. 둘이 같은
// 픽스처는 아티팩트 가중을 하나도 증명하지 못하므로(이 브랜치가 세 번 저지른 실수), 수치를
// 골라서 갈라 놓았다:
//
//   - 아티팩트 1을 한 세션에서 다섯 번 이어 읽고(10·20·30·40·50) 뒤늦게 한 번 더 회수한다(350).
//     16 KiB 상한이 만드는 페이징 폭주 그 자체다. 뒤늦은 350이 있어야 **아티팩트당 max와 min이
//     서로 다른 순위에 서고**, 그래야 집계를 min으로 바꾼 구현이 걸린다.
//   - 아티팩트 2~7은 각각 한 번씩(100~600).
//   - 아티팩트 100은 explicit(귀속 아님)으로 두 번 — resolved_artifacts와 shadow_artifacts를
//     갈라 놓아 소견 F4의 제한이 앞의 수에는 걸리지 않는다는 것도 같은 픽스처가 잠근다.
//
// 그래서 세 답이 전부 다르다: 행 가중 p50/p90/max = 100/400/600, 아티팩트당 **min** =
// 300/500/600, 아티팩트당 **max**(옳은 것) = 350/500/600.
func seedPagedFetchLedger(t *testing.T, dir string) {
	t.Helper()
	st := openAt(t, dir)
	for _, age := range []int64{10, 20, 30, 40, 50, 350} { // 아티팩트 1 — 페이징 다섯 + 뒤늦은 회수 하나
		st.LedgerAppendFetch(t.Context(), 0, 1, 1, &age, true)
	}
	for i, age := range []int64{100, 200, 300, 400, 500, 600} { // 아티팩트 2~7 — 한 번씩
		st.LedgerAppendFetch(t.Context(), 0, 1, int64(i)+2, &age, true)
	}
	for range 2 { // explicit 아티팩트 — 귀속 제한이 거르는 쪽
		st.LedgerAppendFetch(t.Context(), 0, 1, 100, agePtr(100_000), false)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("픽스처 Close: %v", err)
	}
	ledgerSchemaGuard(t, dir, []string{"artifact_id", "artifact_age_s", "shadow_owned"}, nil)
}

// shadowRowsAndArtifacts — 원장을 **직접** 읽어 귀속 해소의 (행 수, distinct 아티팩트 수)를 낸다.
// 사전 가드 전용이라 LedgerFetchStats도 fetchAgeBasis도 거치지 않는다 — 시험 대상으로 시험
// 대상을 가드하면 그 둘이 함께 틀렸을 때 아무것도 안 잡힌다.
func shadowRowsAndArtifacts(t *testing.T, dir string) (int64, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+"?mode=ro")
	if err != nil {
		t.Fatalf("사전 가드 open: %v", err)
	}
	var rows, arts int64
	scanErr := db.QueryRow(`SELECT count(*), count(DISTINCT artifact_id) FROM ledger
		WHERE tool='ctr_fetch' AND shadow_owned=1 AND artifact_age_s IS NOT NULL`).Scan(&rows, &arts)
	closeErr := db.Close()
	if scanErr != nil {
		t.Fatalf("사전 가드 조회: %v", scanErr)
	}
	if closeErr != nil {
		t.Fatalf("사전 가드 Close: %v", closeErr)
	}
	return rows, arts
}

// TestLedgerFetchStatsWeighsAgeByArtifact — 릴리스 패스 소견 F5. 나이 분위수의 **모집단이
// 행이 아니라 아티팩트**다. 페이징은 한 세션 안에서 일어나므로 큰 아티팩트 하나가 거의 같은
// **젊은** 나이를 여럿 남기고, 행으로 세면 그 무리가 p90을 자기 쪽으로 끌어내린다. D104 행 5는
// 그 p90을 그대로 보존 창의 처방값으로 바꾸므로, 계측이 제 데이터가 지지하는 것보다 **짧은**
// 창을 처방하게 된다 — 아무도 눈치채지 못하는 종류의 오류다.
//
// 아티팩트당 대푯값으로 **max**를 쓰는 이유: 창의 길이가 답해야 하는 질문은 "이 아티팩트가
// 마지막으로 필요해진 것이 포착 뒤 얼마인가"이고, 그 답은 그 아티팩트의 **가장 늦은** 회수다.
// min을 쓰면 "언제 처음 읽혔나"를 재게 되어 창을 짧게 처방하는 같은 방향으로 틀린다.
func TestLedgerFetchStatsWeighsAgeByArtifact(t *testing.T) {
	dir := t.TempDir()
	seedPagedFetchLedger(t, dir)

	// 사전 가드: 두 가중이 실제로 갈라져 있어야 아래 단언이 뜻을 갖는다.
	rows, arts := shadowRowsAndArtifacts(t, dir)
	if rows != 12 || arts != 7 {
		t.Fatalf("사전 가드: 픽스처는 귀속 해소 행 12 · 아티팩트 7이어야 한다 — 실제 행 %d · 아티팩트 %d", rows, arts)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.ShadowResolved != 12 || fs.ShadowArtifacts != 7 {
		t.Fatalf("모집단이 픽스처와 다르다: %+v want shadow_rows=12 shadow_artifacts=7", fs)
	}
	if fs.AgeP50 != 350 || fs.AgeP90 != 500 || fs.AgeMax != 600 {
		t.Fatalf("분위수가 아티팩트 가중이 아니다: p50=%d p90=%d max=%d — want 350/500/600"+
			" (행 가중이면 100/400/600, 아티팩트당 min이면 300/500/600)", fs.AgeP50, fs.AgeP90, fs.AgeMax)
	}
}

// TestLedgerFetchStatsCountsDistinctResolvedArtifacts — 릴리스 패스 소견 F7. D104의 **채택
// 문턱**은 10건인데, 그 앞 항이 `resolved`(행)였을 때는 행을 셌다 — 지금 계약은
// `resolved_artifacts + missed`이고 이 테스트가 그 앞 항을 고정한다.
// 164 KiB 아티팩트 하나를 끝까지 읽으면 16 KiB
// 상한 때문에 열 번 불리고 해소 행 열 개가 남는다 — 아티팩트 **하나를 한 번** 읽은 14일,
// 즉 도구가 사실상 안 쓰인 구간이 문턱을 통과해 행 2를 건너뛰고 행 3("이 구간의 데이터로는
// 창을 늘리지 않는다")으로 떨어진다. 문턱이 막으려던 바로 그 오독이다.
//
// 이 픽스처는 두 수가 문턱 10을 **사이에 두고** 갈리게 골랐다 — 행 14(통과) 대 아티팩트
// 8(미달). 둘 다 같은 쪽에 있는 픽스처는 아무것도 증명하지 못한다.
func TestLedgerFetchStatsCountsDistinctResolvedArtifacts(t *testing.T) {
	dir := t.TempDir()
	seedPagedFetchLedger(t, dir)

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Resolved != 14 {
		t.Fatalf("Resolved=%d want 14(행 수는 그대로 낸다 — calls의 분해가 그 위에 서 있다)", fs.Resolved)
	}
	if fs.ResolvedArtifacts != 8 {
		t.Fatalf("ResolvedArtifacts=%d want 8(distinct artifact_id — 귀속 여부와 무관하게 센다)", fs.ResolvedArtifacts)
	}
	if fs.Resolved < 10 || fs.ResolvedArtifacts >= 10 {
		t.Fatalf("문턱 10을 사이에 두지 않는 픽스처는 이 소견을 증명하지 못한다: %+v", fs)
	}
	// 귀속 제한은 shadow_* 쪽에만 걸린다(소견 F4) — 채택 문턱은 "도구가 쓰이는가"를 묻는다.
	if fs.ShadowArtifacts != 7 {
		t.Fatalf("ShadowArtifacts=%d want 7 — 귀속 제한이 resolved_artifacts에까지 번졌다", fs.ShadowArtifacts)
	}
}

// TestLedgerFetchStatsReadsOneSnapshot — 릴리스 패스 소견 F8. 회수 줄의 수들이 **서로 다른
// 스냅샷**에서 나오면 그 줄은 어떤 원장에도 대응하지 않는 수의 모음이 된다. 훅이 하루 약 295번
// 쓰는 원장이므로 `stats`가 도는 사이 커밋이 끼는 것은 예외가 아니라 정상이다.
//
// 세 불변식을 건다 — 셋 다 이 줄을 읽는 사람이 실제로 기대는 것이다:
//
//	① calls = legacy + resolved + missed  (legacy 열을 더한 이유가 이 분해다)
//	② shadow_rows ≤ resolved · shadow_artifacts ≤ resolved_artifacts  (독스트링이 말하는 부분집합)
//	③ p50 ≤ p90 ≤ max  (세 오프셋이 같은 모집단에서 나온다)
//
// 동시 쓰기가 **작은 나이**를 넣는 이유: 뒤에 도는 질의일수록 같은 오프셋이 더 작은 값을
// 가리키므로, 스냅샷이 갈리면 p90이 max보다 커지는 상태에 실제로 도달한다.
// v0.19.1이 SizeStats에서 고친 것과 같은 부류다(그 근거는 SizeStats의 주석에 있다).
func TestLedgerFetchStatsReadsOneSnapshot(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	for i := range 20 { // 분위수가 설 만큼의 씨앗 — 나이는 크게
		age := int64(1000 + i*10)
		st.LedgerAppendFetch(t.Context(), 0, 1, int64(i)+1, &age, true)
	}
	done := make(chan struct{})
	first := make(chan struct{})
	var written atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 5000 {
			select {
			case <-done:
				return
			default:
			}
			st.LedgerAppendFetch(context.Background(), 0, 1, int64(1000+i), agePtr(1), true)
			written.Add(1)
			if i == 0 {
				close(first)
			}
		}
	}()
	defer wg.Wait()
	defer close(done)
	<-first // 첫 쓰기가 들어간 뒤에 읽기 시작 — 동시성 없는 통과를 원천 차단한다
	atStart := written.Load()

	for i := range 40 {
		fs, err := LedgerFetchStats(dir)
		if err != nil {
			t.Fatalf("반복 %d: %v", i, err)
		}
		if fs.Calls != fs.Legacy+fs.Resolved+fs.Missed {
			t.Fatalf("반복 %d: calls=%d != legacy+resolved+missed=%d — 한 스냅샷이 아니다",
				i, fs.Calls, fs.Legacy+fs.Resolved+fs.Missed)
		}
		if fs.ShadowResolved > fs.Resolved || fs.ShadowArtifacts > fs.ResolvedArtifacts {
			t.Fatalf("반복 %d: 부분집합 불변식이 깨졌다 — %+v", i, fs)
		}
		if fs.AgeP50 > fs.AgeP90 || fs.AgeP90 > fs.AgeMax {
			t.Fatalf("반복 %d: 분위수가 서로 다른 모집단에서 나왔다 — p50=%d p90=%d max=%d",
				i, fs.AgeP50, fs.AgeP90, fs.AgeMax)
		}
	}

	// 사후 가드: 읽는 40회 **동안** 쓰기가 실제로 흘렀는지 센다. "한 건 이상"으로는 부족하다 —
	// 루프 전에 한 건만 들어오고 그 뒤 조용한 경우에도 통과해 버려, 사실상 직렬인 실행을
	// 동시성 테스트라고 부르게 된다.
	if during := written.Load() - atStart; during < 20 {
		t.Fatalf("사후 가드: 읽는 40회 동안 동시 쓰기가 %d건뿐이다 — 그런 통과는 이 소견에 대해 "+
			"아무 말도 하지 않는다", during)
	}
}

// TestLedgerFetchStatsShadowColumnMissing: 세 번째 ALTER 이전 원장(세 열 중 둘만 있음 —
// 계약 7의 계단 ②)에서도
// 실패하지 않는다 — 해소·미해소는 읽히되 귀속으로 제한한 수치는 낼 수 없으므로 0이다
// (계약 7의 부분 이관 관용을 셋째 열로 확장한 것).
func TestLedgerFetchStatsShadowColumnMissing(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger(
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
		bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0, artifact_id INTEGER, artifact_age_s INTEGER)`); err != nil {
		t.Fatalf("두 열 스키마: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ledger(ts,tool,artifact_id,artifact_age_s) VALUES(1,'ctr_fetch',1,77)`); err != nil {
		t.Fatalf("해소 행: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ledgerSchemaGuard(t, dir, []string{"artifact_id", "artifact_age_s"}, []string{"shadow_owned"})

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("shadow 열 부재에서 오류가 났다: %v", err)
	}
	if fs.Resolved != 1 {
		t.Fatalf("Resolved=%d want 1", fs.Resolved)
	}
	if fs.ShadowResolved != 0 || fs.AgeMax != 0 {
		t.Fatalf("귀속 열 없이 분위수를 냈다: %+v", fs)
	}
	// **D104 착수 조건이 읽는 수가 이 계단에서 0이다.** 측정값이 아니라는 표식이 없으면 회수
	// 줄의 칸이 다 숫자로 보여 결정표 행 0이 발화하지 못하고 행 2로 떨어진다 — 창 판단은 열려 있지만
	// 처방이 "채택을 늘려라"가 되고, 정작 할 일인 바이너리 교체는 지시되지 않는다.
	if !fs.LedgerOK || !fs.OutcomeOK {
		t.Fatalf("총 호출·해소는 읽었는데 표식이 false: %+v", fs)
	}
	if fs.ShadowOK {
		t.Fatalf("shadow 열이 없는데 ShadowOK=true: %+v", fs)
	}
}

// TestLedgerAppendFetchMissMarksMinusOne: 미해소 행은 artifact_id NULL + artifact_age_s **−1**
// 이다. NULL로 적으면 ALTER가 남긴 레거시 행과 구분되지 않아, 배포 첫날 레거시 49건만으로
// D104의 "미해소 5건 이상"이 발화한다(설계 v0.20 D103 계약 1).
func TestLedgerAppendFetchMissMarksMinusOne(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	st.LedgerAppendFetch(t.Context(), 0, 1, 42, agePtr(3600), true) // 해소(귀속 — 분위수에 든다)
	st.LedgerAppendFetch(t.Context(), 0, 1, 0, nil, false)          // 미해소
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Calls != 2 || fs.Resolved != 1 || fs.Missed != 1 {
		t.Fatalf("calls=%d resolved=%d missed=%d, 기대 2/1/1", fs.Calls, fs.Resolved, fs.Missed)
	}
	if fs.AgeMax != 3600 {
		t.Fatalf("AgeMax=%d, 기대 3600(미해소의 −1이 분포에 섞이면 안 된다)", fs.AgeMax)
	}
}

// TestLedgerAppendContextUnderContention: 겹친 쓰기에서 원장 INSERT가 훅 예산 안에 든다.
// **단일 무경합 실행은 이 경로를 구조적으로 못 본다** — busy_timeout(5000ms)은 다른 연결이
// 락을 쥐고 있을 때만 개입하고, 그 값이 훅의 총예산 2000ms보다 크다는 것이 계약 8의 위험이다.
// 스토어를 넷 열어(각자 자기 ledger 연결) 동시에 쓴다: 같은 프로세스지만 연결이 별개라 파일
// 락 계층은 훅 프로세스 여럿과 같다.
func TestLedgerAppendContextUnderContention(t *testing.T) {
	dir := t.TempDir()
	const writers, perWriter = 4, 25
	stores := make([]*Store, writers)
	for i := range stores {
		stores[i] = openAt(t, dir)
	}

	var worst atomic.Int64
	var wg sync.WaitGroup
	for _, st := range stores {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			for range perWriter {
				ctx, cancel := context.WithTimeout(t.Context(), 2000*time.Millisecond) // 훅 총예산
				begin := time.Now()
				st.LedgerAppendContext(ctx, "hook:shadow", 16384, 0, 1)
				cancel()
				// CompareAndSwap 재시도 루프 — 네 고루틴이 동시에 worst.Load()를 읽고 비교한 뒤
				// Store하면 두 고루틴이 같은 낡은 값을 읽어 더 큰 쪽이 작은 쪽에 덮여 쓰이는
				// lost-update가 가능하다(원자적 개별 연산이라 -race는 못 잡는다). CAS 실패는
				// 다른 고루틴이 갱신했다는 뜻이므로 최신값으로 재비교한다.
				if ms := time.Since(begin).Milliseconds(); ms > worst.Load() {
					for {
						old := worst.Load()
						if ms <= old || worst.CompareAndSwap(old, ms) {
							break
						}
					}
				}
			}
		}(st)
	}
	wg.Wait()
	t.Logf("경합 INSERT 최악 소요 = %dms (훅 총예산 2000ms)", worst.Load())

	rows, err := LedgerStats(dir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	var got int64
	for _, r := range rows {
		if r.Tool == "hook:shadow" {
			got = r.Calls
		}
	}
	if want := int64(writers * perWriter); got != want {
		t.Fatalf("hook:shadow 행=%d, 기대 %d — 예산 안에서 못 쓴 INSERT가 있다", got, want)
	}
	if worst.Load() >= 2000 {
		t.Fatalf("경합 INSERT가 훅 예산을 넘겼다: %dms — 설계 §4-4의 분모 재배치 판단으로 간다", worst.Load())
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

	// FTS pre-state 핀: 삭제 전 같은 질의가 실제로 히트해야 이후 무히트 단언(1315)이 공허 통과가 아니다.
	var pre int
	if err := s.reader.QueryRow("SELECT count(*) FROM fts_porter WHERE fts_porter MATCH 'old'").Scan(&pre); err != nil || pre == 0 {
		t.Fatalf("pre-state 'old' 미히트(무히트 단언 무의미): n=%d err=%v", pre, err)
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

// writePurgingBlob: 테스트용 — dir/artifacts/hash[:2]/hash.purging 격리본을 만들고 mtime을
// now+age로 강제한다(크래시로 rename 이후·remove 이전에 프로세스가 죽은 상황 모사, 최종리뷰 P2).
func writePurgingBlob(t *testing.T, storeDir, hash string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(storeDir, "artifacts", hash[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, hash+".purging")
	if err := os.WriteFile(path, []byte("purging"), 0o600); err != nil {
		t.Fatal(err)
	}
	tm := time.Now().Add(age)
	if err := os.Chtimes(path, tm, tm); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGCOrphanBlobsSweepsStalePurging — 최종리뷰 P2: reclaimHookBlobs가 rename 격리 중
// 크래시하면 DB 행은 이미 삭제돼 hash가 다시 선택되지 않으므로 <64hex>.purging 파일이 영구
// 고아가 된다. GC가 age gate(1h)를 넘긴 격리본은 수거하고 신선한 것(진행 중일 수 있음)은
// 보존해야 한다.
func TestGCOrphanBlobsSweepsStalePurging(t *testing.T) {
	s := openT(t)
	staleHash := strings.Repeat("a", 64)
	stalePath := writePurgingBlob(t, s.dir, staleHash, -2*gcOrphanMinAge)
	freshHash := strings.Repeat("b", 64)
	freshPath := writePurgingBlob(t, s.dir, freshHash, 0)

	removed, err := s.GCOrphanBlobs(t.Context())
	if err != nil {
		t.Fatalf("GCOrphanBlobs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want 1(오래된 격리본만)", removed)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("오래된 .purging 잔존: err=%v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("신선한 .purging이 삭제됨(진행 중 회수 오삭제 위험): %v", err)
	}
}

// TestAcquireLock_SharedSharedCoexist: 두 shared 잠금이 동시에 성립해야 한다(release 전
// 둘 다 성공) — 같은 프로세스라도 별도 os.OpenFile 호출이라 open file description/핸들이
// 갈라져 flock/LockFileEx의 shared+shared 공존이 실제로 검증된다(vacuous하지 않음).
func TestAcquireLock_SharedSharedCoexist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	rel1, err := AcquireLock(path, true)
	if err != nil {
		t.Fatalf("first shared: %v", err)
	}
	defer rel1()
	rel2, err := AcquireLock(path, true)
	if err != nil {
		t.Fatalf("second shared(첫 shared 미해제 상태): %v", err)
	}
	rel2()
}

// TestAcquireLock_SharedThenExclusiveFails: shared 보유 중 exclusive는 실패해야 하고,
// shared release 후에는 성공해야 한다.
func TestAcquireLock_SharedThenExclusiveFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	relShared, err := AcquireLock(path, true)
	if err != nil {
		t.Fatalf("shared: %v", err)
	}
	if _, err := AcquireLock(path, false); err == nil {
		t.Fatal("want exclusive 실패(shared 보유 중), got nil")
	}
	relShared()
	relExcl, err := AcquireLock(path, false)
	if err != nil {
		t.Fatalf("release 후 exclusive: %v", err)
	}
	relExcl()
}

// TestAcquireLock_ExclusiveThenSharedFails: exclusive 보유 중 shared는 실패해야 한다.
func TestAcquireLock_ExclusiveThenSharedFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	relExcl, err := AcquireLock(path, false)
	if err != nil {
		t.Fatalf("exclusive: %v", err)
	}
	defer relExcl()
	if _, err := AcquireLock(path, true); err == nil {
		t.Fatal("want shared 실패(exclusive 보유 중), got nil")
	}
}

// TestAcquireLock_NonBlocking: 논블로킹 고정 — 경합 실패는 즉시 반환되어야 한다(lockStore의
// 5초 백오프 재시도 루프와 달리 AcquireLock은 단발 시도라 대기가 없다).
func TestAcquireLock_NonBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	relExcl, err := AcquireLock(path, false)
	if err != nil {
		t.Fatalf("exclusive: %v", err)
	}
	defer relExcl()
	start := time.Now()
	if _, err := AcquireLock(path, false); err == nil {
		t.Fatal("want 실패, got nil")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("논블로킹 위반 — 실패까지 %v 소요(블로킹 대기 의심)", elapsed)
	}
}

// TestArtifactHashByID: 등록된 artifact의 content_hash를 반환하고, 미존재 id는 ErrNotFound.
func TestArtifactHashByID(t *testing.T) {
	s := openT(t)
	body := []byte("hash-by-id-content")
	id, err := s.Register(t.Context(), Registration{
		StoredBytes: body, MediaType: "text/plain",
		Source: SourceMeta{URI: "/hbi.txt", Kind: "file", SrcHash: "h-hbi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	got, err := s.ArtifactHashByID(t.Context(), id)
	if err != nil {
		t.Fatalf("ArtifactHashByID: %v", err)
	}
	if got != want {
		t.Fatalf("hash=%q want %q", got, want)
	}
	if _, err := s.ArtifactHashByID(t.Context(), 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
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

// TestOpenContextLockDeadline: writable OpenContext의 open-lock 대기가 ctx deadline을 관측해
// 5초 하드 한도 이전에 ErrUnavailable로 포기한다(훅 deadline 예산, 설계 §2.3 — D13 예외 변형).
// Open(=OpenContext(background))의 무기한(5초까지) 대기 기본 동작은 불변이다.
func TestOpenContextLockDeadline(t *testing.T) {
	dir := t.TempDir()
	// content.db.rebuild.lock을 외부에서 배타 선점 — writable OpenContext의 lockStoreCtx가 경합.
	release, err := AcquireLock(filepath.Join(dir, lockFileName), false)
	if err != nil {
		t.Fatalf("선점 잠금: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = OpenContext(ctx, dir, false)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v want ErrUnavailable(ctx deadline 관측)", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ctx 미관측 의심 — %v 소요(5초 하드 대기 추정)", elapsed)
	}
}

// D37 — sourceOf(fetch 표시 경로)도 kind-티어 우선(α6: search/fetch 대표 일치).
func TestReadRangeSourceKindTier(t *testing.T) {
	s, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	body := "tier body\n"
	reg := func(uri, kind string) int64 {
		t.Helper()
		id, err := s.Register(t.Context(), Registration{
			StoredBytes: []byte(body), MediaType: "text/plain", Redaction: "none",
			Source: SourceMeta{URI: uri, Kind: kind, SrcHash: "h"},
			Chunks: []Chunk{{
				Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)),
				LineStart: 1, LineEnd: 1, Text: body,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := reg("inline:AAA", "hook") // uri 사전순 선행 hook 행
	if id2 := reg("inline:ZZZ", "inline"); id2 != id {
		t.Fatalf("동일 content인데 artifact 분리: %d != %d", id, id2)
	}
	r, err := s.ReadRange(t.Context(), id, Selector{Kind: "line", LineStart: 1, LineEnd: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasSource || r.Source.URI != "inline:ZZZ" || r.Source.Kind == "hook" {
		t.Fatalf("want inline:ZZZ(비-hook 티어), got %+v", r.Source)
	}
}

// --- D40: SizeStat shadow-owned 귀속·물리 파일 합산 ---

// openAt: 지정 dir에 store를 연다. SizeStats가 같은 dir을 ro로 재오픈하므로 호출부가 dir을 알아야
// 한다(기존 openT는 t.TempDir()을 내부 생성해 dir을 노출하지 않는다). 시드 후 명시적 Close를 하며,
// t.Cleanup의 두 번째 Close는 무해(반환값 무시).
func openAt(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const soContent = "shadow-owned-content" // 귀속 테스트 공용 콘텐츠(케이스별 TempDir 독립이라 재사용 무해)

// regSource: content를 주어진 media_type·uri·kind 소스로 Register(신규 CAS upsert).
func regSource(t *testing.T, st *Store, content, mediaType, uri, kind string) {
	t.Helper()
	if _, err := st.Register(t.Context(), Registration{
		StoredBytes: []byte(content), MediaType: mediaType,
		Source: SourceMeta{URI: uri, Kind: kind, SrcHash: "sh-" + uri},
	}); err != nil {
		t.Fatal(err)
	}
}

func seedHookOnly(t *testing.T, st *Store) {
	regSource(t, st, soContent, "text/plain", "shadow:Bash:1", "hook")
}

func seedHookPlusFile(t *testing.T, st *Store) {
	// 동일 콘텐츠·동일 media_type을 hook과 file이 공유 → 아티팩트 1행·소스 2행(비-hook 직접 참조 존재).
	regSource(t, st, soContent, "text/plain", "shadow:Bash:1", "hook")
	regSource(t, st, soContent, "text/plain", "/tmp/f.txt", "file")
}

func seedCrossMedia(t *testing.T, st *Store) {
	// 동일 콘텐츠·상이 media_type: hook(text/plain)+file(application/json) → content_hash 동일한 2 아티팩트.
	regSource(t, st, soContent, "text/plain", "shadow:Bash:1", "hook")
	regSource(t, st, soContent, "application/json", "/tmp/f.json", "file")
}

func seedRawRefByFile(t *testing.T, st *Store) {
	// hook 아티팩트(soContent) — 단독이면 귀속. 그러나 file 소스가 soContent를 raw_blob로 참조 → 비귀속.
	regSource(t, st, soContent, "text/plain", "shadow:Bash:1", "hook")
	if _, err := st.Register(t.Context(), Registration{
		StoredBytes: []byte("other-primary-content"), MediaType: "text/plain",
		Source:  SourceMeta{URI: "/tmp/raw.txt", Kind: "file", SrcHash: "sh-raw"},
		RawBlob: []byte(soContent), // sources.raw_blob_hash = sha256(soContent)
	}); err != nil {
		t.Fatal(err)
	}
}

func seedNoSource(t *testing.T, st *Store) {
	// 아티팩트는 있으나 소스 0개 → 비귀속(hook 소스 부재로 걸러진다).
	regSource(t, st, soContent, "text/plain", "shadow:Bash:1", "hook")
	if _, err := st.writer.Exec("DELETE FROM sources"); err != nil {
		t.Fatal(err)
	}
}

func seedTwoHooks(t *testing.T, st *Store) {
	// 동일 콘텐츠·동일 media_type을 hook 소스 2개가 참조 → content_hash 단위 dedup 1.
	regSource(t, st, soContent, "text/plain", "shadow:Bash:1", "hook")
	regSource(t, st, soContent, "text/plain", "shadow:Bash:2", "hook")
}

// seedTwoHookArtifactsSameHashDiffMedia: 동일 콘텐츠·상이 media_type을 hook 소스 2개로 Register
// → content_hash 동일한 2 아티팩트(전부 hook, 귀속)이나 물리 CAS 파일은 1개. content_hash 반환.
func seedTwoHookArtifactsSameHashDiffMedia(t *testing.T, st *Store) string {
	regSource(t, st, soContent, "text/plain", "shadow:Bash:1", "hook")
	regSource(t, st, soContent, "application/json", "shadow:Bash:2", "hook")
	sum := sha256.Sum256([]byte(soContent))
	return hex.EncodeToString(sum[:])
}

func statBlobFile(t *testing.T, dir, h string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, "artifacts", h[:2], h))
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// TestShadowOwnedAttribution — D40 §2: content_hash 단위 귀속 술어.
func TestShadowOwnedAttribution(t *testing.T) {
	cases := []struct {
		name       string
		seed       func(t *testing.T, st *Store)
		wantHashes int
	}{
		{"hook만 참조 → 귀속", seedHookOnly, 1},
		{"hook+explicit 공유 → 비귀속", seedHookPlusFile, 0},
		{"cross-media 공유(동일 hash·상이 media_type) → 비귀속", seedCrossMedia, 0},
		{"raw_blob_hash 비-hook 참조 → 비귀속", seedRawRefByFile, 0},
		{"source 0개 → 비귀속", seedNoSource, 0},
		{"hook 2개(동일 hash) → 귀속 1(hash 단위 dedup)", seedTwoHooks, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			st := openAt(t, dir)
			c.seed(t, st)
			st.Close() // SizeStats는 dir 기준 별도 ro open — 시드 후 닫고 조회
			sz, err := SizeStats(dir)
			if err != nil {
				t.Fatal(err)
			}
			if sz.ShadowOwnedHashes != c.wantHashes {
				t.Fatalf("ShadowOwnedHashes=%d want %d", sz.ShadowOwnedHashes, c.wantHashes)
			}
			if len(sz.ShadowOwned) != c.wantHashes {
				t.Fatalf("ShadowOwned len=%d want %d", len(sz.ShadowOwned), c.wantHashes)
			}
		})
	}
}

// TestShadowOwnedBytesPhysical — D40 §2: 물리 CAS 파일 기저 정확 단정 — 동일 hash의 hook-only
// cross-media artifact 2행이어도 ShadowOwnedBytes는 물리 파일 1개의 os.Stat 크기와 정확히 같다
// (논리 byte_length 2배 합산이면 오구현).
func TestShadowOwnedBytesPhysical(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	h := seedTwoHookArtifactsSameHashDiffMedia(t, st)
	st.Close()
	want := statBlobFile(t, dir, h).Size()
	sz, err := SizeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sz.ShadowOwnedBytes != want {
		t.Fatalf("ShadowOwnedBytes=%d want %d(물리 파일 크기 — 논리 합산 금지)", sz.ShadowOwnedBytes, want)
	}
}

// TestSizeStatFileBytes — D40 §2: FileBytes = content.db 파일 실크기.
func TestSizeStatFileBytes(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	seedHookOnly(t, st)
	st.Close()
	sz, err := SizeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sz.FileBytes <= 0 {
		t.Fatalf("FileBytes=%d want >0", sz.FileBytes)
	}
}

// TestSizeStatsReportsFreeBytes: SizeStats가 회수 가능 바이트(free page)를 낸다.
// 이 값이 없으면 "파일이 크다"와 "파일에 쓰레기가 있다"를 doctor에서 가를 수 없고,
// 그 구분이 없어서 D67의 임계가 15일간 죽은 신호였다(설계 v0.20 관측 B).
func TestSizeStatsReportsFreeBytes(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	body := strings.Repeat("free page seed ", 8000)
	for i := range 10 {
		regChunked(t, st, body+strconv.Itoa(i), "shadow:Bash:free"+strconv.Itoa(i))
	}
	if _, err := st.writer.Exec(`DELETE FROM chunks`); err != nil {
		t.Fatalf("DELETE FROM chunks: %v", err)
	}
	if err := st.MergeFTS(t.Context()); err != nil {
		t.Fatalf("MergeFTS: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sz, err := SizeStats(dir)
	if err != nil || sz == nil {
		t.Fatalf("SizeStats: sz=%v err=%v", sz, err)
	}
	if sz.FreeBytes <= 0 {
		t.Fatalf("삭제+병합 뒤인데 FreeBytes=%d", sz.FreeBytes)
	}
	if sz.FreeBytes > sz.FileBytes {
		t.Fatalf("FreeBytes(%d)가 FileBytes(%d)보다 크다", sz.FreeBytes, sz.FileBytes)
	}
	if !sz.PageStatsOK {
		t.Fatal("PageStatsOK=false — free/live가 측정값이 아니다")
	}
	if sz.LiveBytes <= 0 {
		t.Fatalf("LiveBytes=%d — 살아 있는 페이지가 0으로 읽힌다", sz.LiveBytes)
	}
}

// TestSizeStatsLiveBytesSurvivesLargeWAL — 릴리스 패스 B1. live 축이 **큰 WAL이 있는 상태에서도**
// 옳게 읽히는지 잰다. 옛 산출 `FileBytes-FreeBytes`는 서로 다른 두 스냅샷을 뺐다: FileBytes는
// 본체 파일의 os.Stat이고 freelist_count는 WAL 프레임까지 반영한 커밋 스냅샷이라, 체크포인트가
// 안 일어난 구간에서는 free가 본체 크기를 넘어 live가 음수 → 클램프해 0이 된다. **스토어가
// 가장 클 때 0으로 읽히는 것**이 이 결함의 본질이고, 그 상태를 못 만드는 테스트는 이 수정을
// 잠그지 못한다(재현 관측: file=4096 wal≈90 MB free≈89.5 MB).
//
// 상태를 만드는 방법은 `wal_autocheckpoint=0`이다. 라이브 서버에서 같은 상태가 나오는 기제는
// 다르지만(자동 병합 경로가 체크포인트를 하지 않고 — 계약 5 — 리더가 붙어 있으면 passive
// 체크포인트가 그냥 포기한다) **관측되는 파일/스냅샷 관계는 같다.**
func TestSizeStatsLiveBytesSurvivesLargeWAL(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	// writer는 SetMaxOpenConns(1)이라 이 pragma가 그 한 커넥션에 그대로 붙는다.
	if _, err := st.writer.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatalf("wal_autocheckpoint=0: %v", err)
	}
	body := strings.Repeat("large wal seed ", 8000) // 120 KB/건 — TestSizeStatsReportsFreeBytes와 같은 규모
	for i := range 10 {
		regChunked(t, st, body+strconv.Itoa(i), "shadow:Bash:wal"+strconv.Itoa(i))
	}
	if _, err := st.writer.Exec(`DELETE FROM chunks`); err != nil {
		t.Fatalf("DELETE FROM chunks: %v", err)
	}
	if err := st.MergeFTS(t.Context()); err != nil {
		t.Fatalf("MergeFTS: %v", err)
	}
	// **닫지 않는다** — Close가 wal_checkpoint(TRUNCATE)로 이 상태를 없앤다(store.go Close).
	sz, err := SizeStats(dir)
	if err != nil || sz == nil {
		t.Fatalf("SizeStats: sz=%v err=%v", sz, err)
	}
	if !sz.PageStatsOK {
		t.Fatal("PageStatsOK=false — pragma를 못 읽어 아래 판정이 공허해진다")
	}

	// 사전 가드 — 픽스처가 의도한 상태를 **실제로** 만들었는가. 이 셋이 서지 않으면 아래 단정은
	// 옛 산출에서도 통과하므로(공허 통과) 여기서 멈춘다.
	walB := fileSizeOrZero(t, filepath.Join(dir, "content.db-wal"))
	t.Logf("file=%d wal=%d free=%d live=%d old(file-free)=%d",
		sz.FileBytes, walB, sz.FreeBytes, sz.LiveBytes, sz.FileBytes-sz.FreeBytes)
	if walB <= sz.FileBytes {
		t.Fatalf("WAL이 본체보다 크지 않다 — 큰 WAL 상태가 아니다(wal=%d file=%d)", walB, sz.FileBytes)
	}
	if sz.FreeBytes <= sz.FileBytes {
		t.Fatalf("free가 본체 파일을 넘지 않는다 — 옛 산출이 음수가 되는 조건이 아니다(free=%d file=%d)",
			sz.FreeBytes, sz.FileBytes)
	}
	if old := sz.FileBytes - sz.FreeBytes; old >= 0 {
		t.Fatalf("옛 산출 file-free가 음수가 아니다(%d) — 이 픽스처는 B1을 재현하지 못한다", old)
	}

	// 본 단정 — 옛 눈금은 0으로 읽히고(경고가 영영 안 뜬다), 새 눈금은 살아 있는 바이트를 낸다.
	if got := max(0, sz.FileBytes-sz.FreeBytes); got != 0 {
		t.Fatalf("옛 클램프 산출이 0이 아니다: %d", got)
	}
	if sz.LiveBytes <= 0 {
		t.Fatalf("LiveBytes=%d — 큰 WAL 상태에서 live가 죽었다", sz.LiveBytes)
	}
	// live는 free를 뺀 나머지이므로 스냅샷 총량(page_count×page_size)보다 작고, 그 총량은
	// 본체 파일이 아니라 WAL 위에 있다 — 두 축이 서로 다른 것을 재고 있다는 확인.
	if sz.LiveBytes <= sz.FileBytes {
		t.Fatalf("live(%d)가 본체 파일(%d) 이하 — 스냅샷이 아니라 파일을 재고 있다", sz.LiveBytes, sz.FileBytes)
	}
}

// fileSizeOrZero — 진단용 파일 크기(부재=0).
func fileSizeOrZero(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// --- D41 PurgeHookOnly 헬퍼 (Task 5a) ---------------------------------------

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// ageBlobFile: h의 CAS 파일 mtime을 now+delta로 강제(결정론 — 설계 §8, os.Chtimes).
func ageBlobFile(t *testing.T, st *Store, h string, delta time.Duration) {
	t.Helper()
	tm := time.Now().Add(delta)
	if err := os.Chtimes(filepath.Join(st.dir, "artifacts", h[:2], h), tm, tm); err != nil {
		t.Fatal(err)
	}
}

func assertBlobFileExists(t *testing.T, st *Store, h string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(st.dir, "artifacts", h[:2], h)); err != nil {
		t.Fatalf("blob %s 부재(기대: 잔존): %v", h[:8], err)
	}
}

func assertHashIntact(t *testing.T, st *Store, h string) {
	t.Helper()
	var n int
	if err := st.reader.QueryRow("SELECT count(*) FROM artifacts WHERE content_hash=?", h).Scan(&n); err != nil || n == 0 {
		t.Fatalf("artifact %s 소멸(기대: 보존): n=%d err=%v", h[:8], n, err)
	}
	assertBlobFileExists(t, st, h)
}

func assertHashGone(t *testing.T, st *Store, h string) {
	t.Helper()
	var n int
	if err := st.reader.QueryRow("SELECT count(*) FROM artifacts WHERE content_hash=?", h).Scan(&n); err != nil || n != 0 {
		t.Fatalf("artifact %s 잔존(기대: 삭제): n=%d err=%v", h[:8], n, err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "artifacts", h[:2], h)); !os.IsNotExist(err) {
		t.Fatalf("blob %s 잔존(기대: 회수): err=%v", h[:8], err)
	}
}

// TestPurgeHookOnly — D41 §3: explicit 공유 보존 + hook-만 행·파일·FTS 삭제.
func TestPurgeHookOnly(t *testing.T) {
	st := openAt(t, t.TempDir())
	// hook-만(청크 포함 → FTS 동기 관측). seedHookOnly는 청크가 없어 직접 Register.
	hookContent := "purge-hook-only zzneedle"
	if _, err := st.Register(context.Background(), Registration{
		StoredBytes: []byte(hookContent), MediaType: "text/plain",
		Source: SourceMeta{URI: "shadow:Bash:po1", Kind: "hook", SrcHash: "h-po1"},
		Chunks: []Chunk{{Ordinal: 0, Text: hookContent}},
	}); err != nil {
		t.Fatal(err)
	}
	hookHash := hashOf(hookContent)
	// explicit 공유(hook+file, 동일 콘텐츠) — 비귀속, 완전 보존.
	regSource(t, st, "purge-shared", "text/plain", "shadow:Bash:po2", "hook")
	regSource(t, st, "purge-shared", "text/plain", "/tmp/po-shared.txt", "file")
	sharedHash := hashOf("purge-shared")

	ageBlobFile(t, st, hookHash, -2*time.Hour) // age gate 통과(1h 초과)
	// FTS pre-state 핀: 삭제 전 'zzneedle'가 실제로 히트해야 이후 무히트 단언(1909)이 공허 통과가 아니다.
	var pre int
	if err := st.reader.QueryRow("SELECT count(*) FROM fts_porter WHERE fts_porter MATCH 'zzneedle'").Scan(&pre); err != nil || pre == 0 {
		t.Fatalf("pre-state 'zzneedle' 미히트(무히트 단언 무의미): n=%d err=%v", pre, err)
	}
	rep, err := st.PurgeHookOnly(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hashes != 1 || rep.ReclaimedB <= 0 || rep.DeferredFiles != 0 || rep.FailedFiles != 0 {
		t.Fatalf("report=%+v want Hashes=1 ReclaimedB>0 Deferred=0 Failed=0", rep)
	}
	assertHashGone(t, st, hookHash)
	assertHashIntact(t, st, sharedHash)
	var n int
	if err := st.reader.QueryRow("SELECT count(*) FROM fts_porter WHERE fts_porter MATCH 'zzneedle'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("fts_porter zzneedle 잔존(FTS 미동기): n=%d err=%v", n, err)
	}
}

// TestPurgeHookOnlyAgeGateDefers — mtime 30분 전(1h 이내) 파일은 행만 삭제되고 unlink 유예
// (DeferredFiles=1, 파일 잔존 → --gc 후속 회수 경로).
func TestPurgeHookOnlyAgeGateDefers(t *testing.T) {
	st := openAt(t, t.TempDir())
	seedHookOnly(t, st)
	h := hashOf(soContent)
	ageBlobFile(t, st, h, -30*time.Minute)
	rep, err := st.PurgeHookOnly(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hashes != 1 || rep.DeferredFiles != 1 || rep.ReclaimedB != 0 {
		t.Fatalf("report=%+v want Hashes=1·유예 1·회수 0", rep)
	}
	assertBlobFileExists(t, st, h) // 파일 보존(재등록 경합 안전)
}

// TestPurgeHookOnlyRenameFailureCounts — rename 격리의 os.Rename(p,q) 실패가 부재(무음 skip)가
// 아닌 경우(공유 위반 등)는 FailedFiles로 관측된다(Windows 공유 위반 가시성). 이식 가능한 유도:
// 목적지 q(=p+".purging")를 비어있지 않은 디렉터리로 선점 → 파일→디렉터리 rename은 모든 OS에서
// 부재 아닌 실패. 원 blob은 rename 실패로 그대로 남는다.
func TestPurgeHookOnlyRenameFailureCounts(t *testing.T) {
	st := openAt(t, t.TempDir())
	seedHookOnly(t, st)
	h := hashOf(soContent)
	ageBlobFile(t, st, h, -2*time.Hour) // age gate 통과 — 회수(rename) 시도까지 도달
	p := filepath.Join(st.dir, "artifacts", h[:2], h)
	if err := os.MkdirAll(filepath.Join(p+".purging", "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	rep, err := st.PurgeHookOnly(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hashes != 1 || rep.FailedFiles != 1 || rep.ReclaimedB != 0 || rep.DeferredFiles != 0 {
		t.Fatalf("report=%+v want Hashes=1·Failed 1·회수 0·유예 0", rep)
	}
	assertBlobFileExists(t, st, h) // rename 실패 → 원 blob 잔존
}

// TestPurgeHookOnlyRevalidates — 견적 후 비-hook source가 생긴 hash는 tx 내 재검증으로 대상 제외
// (행·파일 모두 보존).
func TestPurgeHookOnlyRevalidates(t *testing.T) {
	st := openAt(t, t.TempDir())
	seedHookOnly(t, st) // 견적 시점엔 귀속(hook만)
	h := hashOf(soContent)
	// 견적 이후 비-hook 소스가 동일 콘텐츠를 참조 → 더 이상 귀속 아님.
	regSource(t, st, soContent, "text/plain", "/tmp/reval.txt", "file")
	ageBlobFile(t, st, h, -2*time.Hour) // age gate와 무관하게 애초에 대상 제외되어야 함
	rep, err := st.PurgeHookOnly(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hashes != 0 || rep.ReclaimedB != 0 || rep.DeferredFiles != 0 {
		t.Fatalf("report=%+v want Hashes=0(재검증 제외)", rep)
	}
	assertHashIntact(t, st, h)
}

// TestPurgeHookOnlyReplacedFileRollsBack — 경합 시뮬: 행 삭제 커밋 후·파일 회수 전에 동일 경로를
// fresh mtime 파일로 교체(Register.writeBlob 재등록 시뮬) → rename 격리 ②의 fresh-mtime 검출로
// 롤백(원 경로 복원·Deferred=1·회수 0). 내부 단계(purgeHookRows/reclaimHookBlobs)로 창에 개입.
func TestPurgeHookOnlyReplacedFileRollsBack(t *testing.T) {
	st := openAt(t, t.TempDir())
	seedHookOnly(t, st)
	h := hashOf(soContent)
	ageBlobFile(t, st, h, -2*time.Hour) // 원본은 age gate를 통과할 만큼 오래됨

	hashes, rep, err := st.purgeHookRows(context.Background(), 0, 0) // ① 행 삭제 tx만 커밋
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hashes != 1 {
		t.Fatalf("Hashes=%d want 1", rep.Hashes)
	}
	// ② 커밋↔회수 창에 fresh mtime으로 교체(writeBlob은 파일만 쓰고 DB 행은 만들지 않는다).
	if err := st.writeBlob(h, []byte("replaced-fresh")); err != nil {
		t.Fatal(err)
	}
	// ③ 회수 단계 — fresh-mtime 검출로 롤백해야 한다(오삭제 방지).
	if err := st.reclaimHookBlobs(context.Background(), hashes, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.DeferredFiles != 1 || rep.ReclaimedB != 0 {
		t.Fatalf("report=%+v want 롤백(Deferred=1·회수 0)", rep)
	}
	assertBlobFileExists(t, st, h) // 원 경로 복원
}

// TestVacuum — D41: purge 후 후행 VACUUM(Task 5b 소비)이 열린 store에서 오류 없이 실행되고 데이터가
// 보존되는지 확인(VACUUM은 tx 밖에서만 실행 가능 — 회귀 방지 스모크).
func TestVacuum(t *testing.T) {
	st := openAt(t, t.TempDir())
	seedHookOnly(t, st)
	if _, err := st.PurgeHookOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	regSource(t, st, "vacuum-keep", "text/plain", "/tmp/keep.txt", "file")
	if err := st.Vacuum(context.Background()); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	var n int
	if err := st.reader.QueryRow("SELECT count(*) FROM artifacts WHERE content_hash=?", hashOf("vacuum-keep")).Scan(&n); err != nil || n != 1 {
		t.Fatalf("VACUUM 후 데이터 손실: n=%d err=%v", n, err)
	}
}

// TestPurgeHookOnlyPreservesReindexResidue — 최종리뷰 P1: purge --hook-only의 고아 정리는
// 이 purge의 선택분(shadow 귀속 hash)에만 국한돼야 한다. 재색인 잔재(같은 URI를 A→B로
// 재등록하며 source_kind와 무관하게 생기는 source-less 아티팩트)는 hook과 무관한 store 전역
// 잔여라 hook-only purge가 건드리면 안 된다 — 잔재 아티팩트 행·청크(FTS 포함)는 보존하고
// hook-only 행만 삭제해야 한다.
func TestPurgeHookOnlyPreservesReindexResidue(t *testing.T) {
	st := openAt(t, t.TempDir())
	ctx := context.Background()
	// 재색인 잔재: 같은 URI를 content A→B로 재등록 → content A 아티팩트가 source-less 잔재.
	const residueURI = "/reindex.txt"
	const contentA = "reindex-residue-A zzresidue"
	if _, err := st.Register(ctx, Registration{
		StoredBytes: []byte(contentA), MediaType: "text/plain",
		Source: SourceMeta{URI: residueURI, Kind: "file", SrcHash: "sh-A"},
		Chunks: []Chunk{{Ordinal: 0, Text: contentA}},
	}); err != nil {
		t.Fatal(err)
	}
	residueHash := hashOf(contentA)
	const contentB = "reindex-current-B"
	if _, err := st.Register(ctx, Registration{
		StoredBytes: []byte(contentB), MediaType: "text/plain",
		Source: SourceMeta{URI: residueURI, Kind: "file", SrcHash: "sh-B"},
		Chunks: []Chunk{{Ordinal: 0, Text: contentB}},
	}); err != nil {
		t.Fatal(err)
	}
	// hook-only 대상.
	const hookContent = "purge-hook-residue zzneedle"
	if _, err := st.Register(ctx, Registration{
		StoredBytes: []byte(hookContent), MediaType: "text/plain",
		Source: SourceMeta{URI: "shadow:Bash:rr1", Kind: "hook", SrcHash: "h-rr1"},
		Chunks: []Chunk{{Ordinal: 0, Text: hookContent}},
	}); err != nil {
		t.Fatal(err)
	}
	hookHash := hashOf(hookContent)
	ageBlobFile(t, st, hookHash, -2*time.Hour) // age gate 통과

	// FTS pre-state 핀: 삭제 전 hook 청크('zzneedle')가 실제로 히트해야 이후 무히트 단언(2051)이 공허 통과가 아니다.
	var preHook int
	if err := st.reader.QueryRow("SELECT count(*) FROM fts_porter WHERE fts_porter MATCH 'zzneedle'").Scan(&preHook); err != nil || preHook == 0 {
		t.Fatalf("pre-state 'zzneedle' 미히트(무히트 단언 무의미): n=%d err=%v", preHook, err)
	}

	rep, err := st.PurgeHookOnly(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hashes != 1 {
		t.Fatalf("rep.Hashes=%d want 1(hook만 — 잔재는 shadow 귀속 아님)", rep.Hashes)
	}
	assertHashGone(t, st, hookHash)      // hook-only 행·blob 삭제
	assertHashIntact(t, st, residueHash) // 잔재 아티팩트 행·blob 보존(전역 고아 sweep 금지)
	var rn int
	if err := st.reader.QueryRow("SELECT count(*) FROM fts_porter WHERE fts_porter MATCH 'zzresidue'").Scan(&rn); err != nil || rn != 1 {
		t.Fatalf("잔재 청크 FTS 소멸(전역 청크 sweep): n=%d err=%v", rn, err)
	}
	var hn int
	if err := st.reader.QueryRow("SELECT count(*) FROM fts_porter WHERE fts_porter MATCH 'zzneedle'").Scan(&hn); err != nil || hn != 0 {
		t.Fatalf("hook 청크 FTS 잔존(미동기): n=%d err=%v", hn, err)
	}
}

// --- D67: 나이 필터 shadow purge (Task 11) ----------------------------------

// TestPurgeHookOnlyOlderThan: cutoff보다 새 shadow 아티팩트는 남고 오래된 것만 지워진다.
// explicit 소스는 나이와 무관하게 보존된다(--hook-only 계약 유지).
//
// 주의: 기존 TestPurgeHookOnlyAgeGateDefers(store_test.go:1930)의 age gate는 파일 unlink
// 유예(mtime 1시간)이고, 이 태스크가 더하는 것은 마지막 포착(sources.indexed_at) 기준 대상
// 선택이다. 서로 다른 축이므로 두 테스트가 함께 통과해야 한다.
func TestPurgeHookOnlyOlderThan(t *testing.T) {
	st := openAt(t, t.TempDir())
	regSource(t, st, "old-shadow", "text/plain", "shadow:Bash:old", "hook")
	regSource(t, st, "new-shadow", "text/plain", "shadow:Bash:new", "hook")
	regSource(t, st, "kept-explicit", "text/plain", "file:///kept.txt", "file")

	oldHash := hashOf("old-shadow")
	// CAS 파일도 나이를 먹여야 reclaimHookBlobs의 mtime 1시간 유예를 통과한다 — 그러지 않으면
	// 모든 unlink가 Deferred로 끝나고 ReclaimedB가 0이라 행 수만 검증하게 된다.
	ageBlobFile(t, st, oldHash, -2*gcOrphanMinAge)

	// regSource는 시각을 받지 않으므로 직접 내린다(쓰기 핸들은 패키지 사설 st.writer). 대상 선택을
	// 좌우하는 값은 마지막 포착(sources.indexed_at)이므로 그것을 반드시 내려야 한다 — 여기서
	// created_at만 내리면 이 테스트는 나이 필터를 전혀 검증하지 않는 공허 통과가 된다. created_at도
	// 함께 내려 "오래 전 첫 포착 후 재포착 없음"이라는 실제 상태로 시드한다(소스가 아티팩트보다 먼저
	// 색인된 불가능한 조합을 만들지 않는다).
	cutoff := time.Now().Add(-72 * time.Hour).Unix()
	oldTS := time.Now().Add(-96 * time.Hour).Unix()
	if _, err := st.writer.Exec(
		`UPDATE artifacts SET created_at=? WHERE content_hash=?`, oldTS, oldHash,
	); err != nil {
		t.Fatalf("첫 포착 나이 조정: %v", err)
	}
	if _, err := st.writer.Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:old'`, oldTS,
	); err != nil {
		t.Fatalf("마지막 포착 나이 조정: %v", err)
	}
	// regSource는 Registration.Chunks를 넘기지 않아 청크 행이 생기지 않는다(store.go:459 루프가
	// 빈 슬라이스를 돈다). chunks DELETE 경로를 덮으려면 새 shadow에 청크를 직접 심어야 한다.
	if _, err := st.writer.Exec(
		`INSERT INTO chunks(artifact_id, ordinal, text)
		 SELECT artifact_id, 0, 'new-chunk' FROM sources WHERE uri='shadow:Bash:new'`,
	); err != nil {
		t.Fatalf("청크 시드: %v", err)
	}
	// 오래된 shadow에는 페이지 여러 장을 차지할 만큼 큰 청크를 심는다 — 아래 freelist 단정이 회수로
	// 실제로 비워진 페이지를 보게 하는 전제다(작은 행만 지우면 free page가 0장일 수 있다).
	if _, err := st.writer.Exec(
		`INSERT INTO chunks(artifact_id, ordinal, text)
		 SELECT artifact_id, 0, ? FROM sources WHERE uri='shadow:Bash:old'`,
		strings.Repeat("old-chunk ", 40000), // 약 400KB
	); err != nil {
		t.Fatalf("청크 시드(old): %v", err)
	}

	rep, err := st.PurgeHookOnlyOlderThan(t.Context(), cutoff, 0)
	if err != nil {
		t.Fatalf("PurgeHookOnlyOlderThan: %v", err)
	}
	if rep.Hashes != 1 {
		t.Fatalf("rep.Hashes=%d want 1(오래된 shadow만)", rep.Hashes)
	}
	if rep.ReclaimedB == 0 {
		t.Errorf("물리 파일이 회수되지 않았다: deferred=%d failed=%d", rep.DeferredFiles, rep.FailedFiles)
	}
	var n int
	if err := st.Reader().QueryRow(
		`SELECT COUNT(*) FROM sources WHERE uri IN ('shadow:Bash:new','file:///kept.txt')`,
	).Scan(&n); err != nil {
		t.Fatalf("잔존 조회: %v", err)
	}
	if n != 2 {
		t.Errorf("잔존 소스=%d want 2(새 shadow + explicit)", n)
	}
	// 나이 조건이 회수 목록 SELECT에만 걸리면, cutoff 이후 shadow의 청크·소스는 서브쿼리
	// 기반 DELETE에 그대로 쓸려 나가고 artifacts 행만 남는다. 그 비대칭을 여기서 잡는다.
	var chunks int
	if err := st.Reader().QueryRow(
		`SELECT COUNT(*) FROM chunks WHERE artifact_id IN
		 (SELECT artifact_id FROM sources WHERE uri='shadow:Bash:new')`,
	).Scan(&chunks); err != nil {
		t.Fatalf("청크 조회: %v", err)
	}
	if chunks == 0 {
		t.Errorf("cutoff 이후 shadow의 청크가 지워졌다 — 나이 조건이 세 문장 전부에 걸리지 않았다")
	}
	// 설계 §2 D67의 "VACUUM 미수행"을 고정한다: 회수 뒤 free page가 남아 있다는 것이 그 증거다.
	// 이 경로에 VACUUM이 끼어들면 freelist가 0으로 회수되는데, 그것은 상시 로드(alwaysLoad) 서버의
	// 기동 지연 보장을 깨는 변경이므로 조용히 green으로 통과하면 안 된다. TestVacuum은 반대로 명시
	// 호출 VACUUM이 동작함을 고정한다 — 자동 회수 경로와 명시 축소는 서로 다른 계약이다.
	var freelist int
	if err := st.Reader().QueryRow(`PRAGMA freelist_count`).Scan(&freelist); err != nil {
		t.Fatalf("freelist 조회: %v", err)
	}
	if freelist == 0 {
		t.Errorf("freelist_count=0 — 회수가 free page를 남기지 않았다(VACUUM이 돌았다)")
	}
}

// TestPurgeHookOnlyOlderThanKeepsRecapturedContent: 나이 기준이 마지막 포착(sources.indexed_at)이라는
// 것을 고정한다. shadow URI는 콘텐츠 주소라(ingest.go: "shadow:"+title+":"+contentHash) 바이트 동일한
// 출력을 다시 포착하면 같은 URI로 upsert되어 옛 artifacts 행을 재사용하고, Register의 found 분기는
// sources만 갱신해 created_at은 첫 포착 시각에 머문다. 첫 포착 기준으로 고르면 방금 다시 포착한
// 콘텐츠가 지워져, 몇 초 전 모델에 넘긴 참조가 ctr_fetch에서 해소되지 않고 그 청크도 ctr_search에서
// 사라진다. 재포착되지 않은 대조군을 함께 두는 이유: 그것이 없으면 purge가 아무것도 고르지 않아도
// "재포착분 잔존"이 공허하게 통과한다.
func TestPurgeHookOnlyOlderThanKeepsRecapturedContent(t *testing.T) {
	st := openAt(t, t.TempDir())
	const fresh = "recaptured-shadow zzrecap"
	freshHash := hashOf(fresh)
	freshURI := "shadow:Bash:" + freshHash // 콘텐츠 주소 URI — 재포착이 같은 행을 upsert한다
	reg := Registration{
		StoredBytes: []byte(fresh), MediaType: "text/plain",
		Source: SourceMeta{URI: freshURI, Kind: "hook", SrcHash: "sh-" + freshURI},
		Chunks: []Chunk{{Ordinal: 0, Text: fresh}},
	}
	if _, err := st.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	regSource(t, st, "stale-shadow", "text/plain", "shadow:Bash:stale", "hook")
	staleHash := hashOf("stale-shadow")

	cutoff := time.Now().Add(-72 * time.Hour).Unix()
	oldTS := time.Now().Add(-96 * time.Hour).Unix()
	// 두 hash 모두 첫 포착을 창 밖으로 내린다(created_at·indexed_at 양쪽 — WHERE 없이 전량).
	if _, err := st.writer.Exec(`UPDATE artifacts SET created_at=?`, oldTS); err != nil {
		t.Fatalf("첫 포착 나이 조정: %v", err)
	}
	if _, err := st.writer.Exec(`UPDATE sources SET indexed_at=?`, oldTS); err != nil {
		t.Fatalf("포착 시각 나이 조정: %v", err)
	}
	// 재포착: 바이트 동일 → 같은 content_hash·같은 URI. artifacts는 found 분기라 청크도 재삽입되지
	// 않고 created_at도 그대로다. sources upsert만 indexed_at을 지금으로 올린다(store.go:483).
	if _, err := st.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	// CAS 파일 나이는 재포착(writeBlob은 매번 재작성 — store.go:374 rename) 이후에 먹인다. 그러지
	// 않으면 mtime 1시간 유예가 unlink를 막아 "blob 잔존" 단정이 공허 통과한다.
	ageBlobFile(t, st, freshHash, -2*gcOrphanMinAge)
	ageBlobFile(t, st, staleHash, -2*gcOrphanMinAge)

	// 나이·건수 예산을 함께 넘긴다 — 기동 경로(main.go: cutoff, startupPurgeMaxHashes)와 같은 모양이다.
	// 두 ? 의 위치 인자가 뒤바뀌면 `indexed_at >= 5`(전량 미배제) + 거대 LIMIT이 되어 아래 단정이
	// 깨진다. 그 조합을 쓰는 테스트가 여기뿐이라 이 자리에서 함께 고정한다.
	rep, err := st.PurgeHookOnlyOlderThan(t.Context(), cutoff, 5)
	if err != nil {
		t.Fatalf("PurgeHookOnlyOlderThan: %v", err)
	}
	if rep.Hashes != 1 {
		t.Fatalf("rep.Hashes=%d want 1(대조군만 삭제 — 재포착분은 마지막 포착이 cutoff 이후라 보존)", rep.Hashes)
	}
	assertHashGone(t, st, staleHash)
	assertHashIntact(t, st, freshHash) // artifacts 행 + CAS 파일
	var srcN, chunkN int
	if err := st.Reader().QueryRow(
		`SELECT (SELECT COUNT(*) FROM sources WHERE uri=?),
		        (SELECT COUNT(*) FROM chunks WHERE artifact_id IN
		           (SELECT id FROM artifacts WHERE content_hash=?))`, freshURI, freshHash,
	).Scan(&srcN, &chunkN); err != nil {
		t.Fatalf("잔존 조회: %v", err)
	}
	if srcN != 1 || chunkN != 1 {
		t.Errorf("재포착분 sources=%d chunks=%d want 1/1(ctr_fetch·ctr_search가 해소되어야 한다)", srcN, chunkN)
	}
}

// TestPurgeHookOnlyOlderThanZeroMeansAll: cutoff<=0·maxHashes<=0이면 예산 없이 전부 지운다 —
// 호출자가 "필터 있음/없음"으로 분기하지 않게 하는 계약이다.
func TestPurgeHookOnlyOlderThanZeroMeansAll(t *testing.T) {
	st := openAt(t, t.TempDir())
	regSource(t, st, "s1", "text/plain", "shadow:Bash:1", "hook")
	regSource(t, st, "s2", "text/plain", "shadow:Bash:2", "hook")

	rep, err := st.PurgeHookOnlyOlderThan(t.Context(), 0, 0)
	if err != nil {
		t.Fatalf("PurgeHookOnlyOlderThan: %v", err)
	}
	if rep.Hashes != 2 {
		t.Errorf("rep.Hashes=%d want 2(예산 없음)", rep.Hashes)
	}
}

// TestPurgeHookOnlyOlderThanCapsPerRun: maxHashes가 1회 회수량을 제한하고 나머지는 다음
// 호출로 넘어간다 — 기동 경로가 적체 전체를 한 번에 처리하지 않게 하는 예산(설계 v0.12 D67).
func TestPurgeHookOnlyOlderThanCapsPerRun(t *testing.T) {
	st := openAt(t, t.TempDir())
	for _, s := range []string{"cap1", "cap2", "cap3"} {
		regSource(t, st, s, "text/plain", "shadow:Bash:"+s, "hook")
	}
	rep, err := st.PurgeHookOnlyOlderThan(t.Context(), 0, 2)
	if err != nil {
		t.Fatalf("1회차: %v", err)
	}
	if rep.Hashes != 2 {
		t.Fatalf("1회차 rep.Hashes=%d want 2(상한)", rep.Hashes)
	}
	rep2, err := st.PurgeHookOnlyOlderThan(t.Context(), 0, 2)
	if err != nil {
		t.Fatalf("2회차: %v", err)
	}
	if rep2.Hashes != 1 {
		t.Errorf("2회차 rep.Hashes=%d want 1(잔여분)", rep2.Hashes)
	}
}

// TestShadowRetentionDefault: 기본 보존 기간이 3일이고, 환경변수로만 조정된다.
// 임의 상향으로 정책이 조용히 무력화되지 않도록 파싱 실패·비양수는 기본값을 쓴다
// (storeWarnBytes와 같은 규율).
func TestShadowRetentionDefault(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 72 * time.Hour},
		{"24h", 24 * time.Hour},
		{"0", 72 * time.Hour},
		{"-5h", 72 * time.Hour},
		{"garbage", 72 * time.Hour},
	}
	for _, c := range cases {
		got := ShadowRetention(func(string) string { return c.env })
		if got != c.want {
			t.Errorf("ShadowRetention(%q)=%v want %v", c.env, got, c.want)
		}
	}
}

// TestShadowCutoffBoundary: ShadowCutoff의 0 경계를 고정한다. 보존 기간이 epoch 경과분보다 크면
// 경계가 0 이하로 내려가고, store는 cutoffUnix<=0을 "나이 필터 없음"으로 읽는다
// (TestPurgeHookOnlyOlderThanZeroMeansAll이 그 계약을 고정한다) — 보존을 늘리려는 설정이
// 나이 무관 전량 삭제로 반전되는 데이터 손실형 결함이다(T12-F1). 기동 경로의 `cutoff <= 0` 건너뛰기가
// 그 반전을 막는데, 판정을 `< 0`으로 좁히면 정확히 0인 경계(아래 세 번째 케이스)가 그대로 store에
// 전달된다. 경계가 어디인지 여기서 고정해 조용한 이동을 막는다.
func TestShadowCutoffBoundary(t *testing.T) {
	now := time.Unix(1_800_000_000, 0) // 고정 기준 — 실행 시각에 흔들리지 않는다
	for _, c := range []struct {
		name string
		d    time.Duration
		want int64
	}{
		// 기대값은 리터럴로 적는다 — now.Add(-d).Unix()로 적으면 구현과 같은 식이라 부호·단위
		// 오류를 잡지 못한다. 1_800_000_000 - 72h(259_200s).
		{"기본 보존(72h)", DefaultShadowRetention, 1_799_740_800},
		{"epoch 직전", time.Duration(now.Unix()-1) * time.Second, 1},
		{"epoch 정각 — 여기서부터 반전", time.Duration(now.Unix()) * time.Second, 0},
		{"epoch 초과", time.Duration(now.Unix()+1) * time.Second, -1},
	} {
		if got := ShadowCutoff(now, c.d); got != c.want {
			t.Errorf("%s: ShadowCutoff(_, %v)=%d want %d", c.name, c.d, got, c.want)
		}
	}
}

// TestCheckpointTruncate — D50: 정상 경로에서 busy=0이고 WAL이 0B로 절단된다.
func TestCheckpointTruncate(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Register(context.Background(), Registration{
		StoredBytes: []byte("ckpt"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/ckpt.txt", Kind: "file", SrcHash: "h-ckpt"},
		Chunks: []Chunk{{Ordinal: 0, Text: "ckpt"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	busy, _, _, err := st.CheckpointTruncate(context.Background())
	if err != nil {
		t.Fatalf("CheckpointTruncate: %v", err)
	}
	if busy != 0 {
		t.Fatalf("busy=%d want 0", busy)
	}
	fi, err := os.Stat(filepath.Join(dir, "content.db-wal"))
	if err != nil {
		t.Fatalf("wal stat: %v (TRUNCATE는 파일을 0B로 자를 뿐 삭제하지 않음)", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("wal=%dB want 0(TRUNCATE)", fi.Size())
	}
}

// TestCheckpointTruncateBusyWithReader — D50: 열린 read 트랜잭션 공존 시 오류가 아니라
// busy≠0으로 미완료를 알린다(Exec nil을 성공으로 오인하는 회귀 방지 — 스펙 §5 실험 근거).
// busy_timeout(5000) 소진까지 ~5s 걸리는 것이 정상.
func TestCheckpointTruncateBusyWithReader(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Register(context.Background(), Registration{
		StoredBytes: []byte("busy"), MediaType: "text/plain",
		Source: SourceMeta{URI: "/busy.txt", Kind: "file", SrcHash: "h-busy"},
		Chunks: []Chunk{{Ordinal: 0, Text: "busy"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	dbPath := filepath.ToSlash(filepath.Join(dir, "content.db"))
	locker, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	defer func() { _ = locker.Close() }()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("locker conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	var n int
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sources").Scan(&n); err != nil {
		t.Fatalf("read txn: %v", err)
	}
	busy, _, _, err := st.CheckpointTruncate(context.Background())
	if err != nil {
		t.Fatalf("CheckpointTruncate: %v", err)
	}
	if busy == 0 {
		t.Fatalf("busy=0 — 열린 reader 공존인데 완료로 보고(회귀)")
	}
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
}

// TestJournalSizeLimitShrinksWALOnPassiveCheckpoint — D102 계약 5·6·9의 기제 자체를 검증한다:
// journal_size_limit이 이 드라이버(modernc.org/sqlite)에서 WAL을 그 한도 이하로 줄이게
// 만드는가. 32 MiB를 실제로 쓰지 않으려고 작은 한도(65536)를 건 생 sql.DB로 잰다 —
// store.Open을 거치지 않는다(배선은 TestOpen_JournalSizeLimit이 잰다).
//
// 절단은 체크포인트 그 자체가 아니라 **그 뒤 첫 커밋**에서 일어난다 — 드라이버 소스
// (modernc.org/sqlite v1.54.0, lib/의 _walFrames)를 직접 대조해 확인했다: 체크포인트가 WAL을
// 완전히 비우면 Wal.truncateOnCommit이 세워지고, 그 빈 WAL에 첫 프레임을 쓰는 **다음 커밋**의
// 프레임 쓰기 말미에서만 journal_size_limit을 넘는 만큼 파일을 자른다(Wal.mxWalSize 검사 —
// walLimitSize). 체크포인트만 걸고 후속 커밋 없이 파일 크기를 재면 이 테스트는 늘 실패한다 —
// 브리프 "정직하게 적을 것"의 "다음 커밋의 체크포인트가 완료될 때 내려간다"가 가리키는 바로
// 그 지점이다. 그래서 체크포인트 뒤 사소한 쓰기를 한 번 더 커밋한다.
//
// TRUNCATE가 아니라 PASSIVE인 것이 요점이다 — TRUNCATE는 한도와 무관하게 WAL을 0으로 만들어
// journal_size_limit을 아예 재지 않는다.
func TestJournalSizeLimitShrinksWALOnPassiveCheckpoint(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.ToSlash(filepath.Join(dir, "content.db"))
	const limit = 65536
	db, err := sql.Open("sqlite", "file:"+dbPath+
		"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)&_pragma=journal_size_limit("+strconv.Itoa(limit)+")")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1) // 체크포인트를 CREATE/INSERT와 같은 커넥션에서 실행(결정론 — 별개 커넥션의 스냅샷 잔존 배제)

	if _, err := db.Exec("CREATE TABLE t(v TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// 한도(65536B)를 확실히 넘도록 쓴다 — 리터럴 붙여넣기 대신 strings.Repeat.
	if _, err := db.Exec("INSERT INTO t(v) VALUES(?)", strings.Repeat("x", 300_000)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	walPath := filepath.Join(dir, "content.db-wal")
	fi, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("wal stat(사전 가드): %v", err)
	}
	// 사전 가드: 픽스처가 한도를 실제로 넘겼는지 확인한다 — 이 가드가 없으면 "원래 작았다"에도
	// 통과해 이 테스트가 공허 통과한다.
	if fi.Size() <= limit {
		t.Fatalf("wal(사전 가드)=%dB want >%dB — 픽스처가 한도를 못 넘김", fi.Size(), limit)
	}

	var busy, walFrames, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &walFrames, &checkpointed); err != nil {
		t.Fatalf("wal_checkpoint(PASSIVE): %v", err)
	}
	// 절단 트리거 — 위 주석 참조. 이 커밋이 없으면 파일은 체크포인트 전 크기 그대로 남는다.
	if _, err := db.Exec("INSERT INTO t(v) VALUES('')"); err != nil {
		t.Fatalf("post-checkpoint insert: %v", err)
	}

	fi, err = os.Stat(walPath)
	if err != nil {
		t.Fatalf("wal stat(사후): %v", err)
	}
	if fi.Size() > limit {
		t.Fatalf("wal(사후)=%dB want <=%dB (checkpoint busy=%d walFrames=%d checkpointed=%d) — journal_size_limit이 WAL을 못 줄임",
			fi.Size(), limit, busy, walFrames, checkpointed)
	}
}

// TestOpen_JournalSizeLimit — D102 계약 5·6·9 배선: pragmas 상수가 writable/read-only 두 DSN의
// 공통 접두라(store.go:145-152) journalSizeLimit 상수 한 자리 변경이 둘 다 덮는다.
func TestOpen_JournalSizeLimit(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	var got string
	if err := st.Reader().QueryRow("PRAGMA journal_size_limit").Scan(&got); err != nil {
		t.Fatalf("PRAGMA journal_size_limit: %v", err)
	}
	if got != journalSizeLimit {
		t.Fatalf("journal_size_limit=%q want %q", got, journalSizeLimit)
	}

	ro, err := Open(dir, true)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = ro.Close() }()
	var gotRO string
	if err := ro.Reader().QueryRow("PRAGMA journal_size_limit").Scan(&gotRO); err != nil {
		t.Fatalf("read-only PRAGMA journal_size_limit: %v", err)
	}
	if gotRO != journalSizeLimit {
		t.Fatalf("read-only journal_size_limit=%q want %q", gotRO, journalSizeLimit)
	}
}

// TestErrPredicates — 공개 술어의 비-sqlite 오류 음성 판정(양성은 실 BUSY 경로가 간접 커버).
func TestErrPredicates(t *testing.T) {
	if IsBusyErr(nil) || IsBusyErr(errors.New("x")) {
		t.Fatal("IsBusyErr 비-sqlite 오류에 true")
	}
	if IsDiskErr(nil) || IsDiskErr(errors.New("x")) {
		t.Fatal("IsDiskErr 비-sqlite 오류에 true")
	}
}

// TestReclaimHookBlobsHonorsCancel — D74: 행 삭제 커밋 이후 파일 회수 루프가 취소를 관측한다.
// 취소된 ctx를 PurgeHookOnlyOlderThan에 처음부터 넘기는 형태로는 이 회귀를 잡을 수 없다 —
// 그 경로는 행 삭제 tx에서 먼저 실패해 빈 리포트를 반환하므로 deferred가 원래 0이다. 그래서
// 회수 단계를 직접 부른다.
//
// 이 테스트는 루프 선두 체크만 잡는다(F3) — 락이 무경합이라 lockStoreCtx(ctx, s.dir)가 즉시
// 성공해 그 내부 select{ctx.Done(), time.After}는 절대 안 탄다. lockStoreCtx를 lockStore로
// 되돌려도 이 테스트는 그대로 통과한다. 잠금 대기 자체의 취소 관측은
// TestReclaimHookBlobsHonorsLockWaitCancel(아래)이 락을 선점해 강제로 검증한다.
func TestReclaimHookBlobsHonorsCancel(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir) // store_test.go:1696 — dir 지정 Open, t.Cleanup에 `_ = s.Close()` 등록
	// 회수 대상 blob 두 개를 CAS 배치대로 만든다: <dir>/artifacts/<h[:2]>/<h>
	hashes := []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
	for _, h := range hashes {
		p := filepath.Join(dir, "artifacts", h[:2], h)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 회수 시작 전에 취소된 상태
	var rep HookPurgeReport
	err := s.reclaimHookBlobs(ctx, hashes, &rep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
	if rep.DeferredFiles != 0 {
		t.Fatalf("deferred=%d want 0 — 취소 경로가 deferred를 부풀리면 D67 임계값 입력이 오염된다", rep.DeferredFiles)
	}
	// 취소로 중단됐으므로 파일은 그대로 남는다(후속 --gc가 회수한다).
	for _, h := range hashes {
		if _, statErr := os.Stat(filepath.Join(dir, "artifacts", h[:2], h)); statErr != nil {
			t.Fatalf("blob %s 가 사라졌다: %v", h[:8], statErr)
		}
	}
}

// TestReclaimHookBlobsHonorsLockWaitCancel — D74 F3/F4: reclaimHookBlobs의 lockStoreCtx 대기
// 자체가 ctx를 관측한다. content.db.rebuild.lock을 외부에서 배타 선점해(TestOpenContextLockDeadline과
// 동형 — store_test.go:1633) lockStoreCtx가 select{ctx.Done(), time.After}로 실제 진입하게
// 만든다. lockStoreCtx를 lockStore로 되돌리면(F3 회귀) ctx.Done()이 절대 발화하지 않아 내부
// 5초 하드 타임아웃까지 블록한다 — elapsed 상한이 그 회귀를 잡는다. 동시에 반환 오류가
// context.DeadlineExceeded 그대로인지도 검증한다(F4 — lockStoreCtx가 감싸는 ErrUnavailable이
// 아니라).
func TestReclaimHookBlobsHonorsLockWaitCancel(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	release, err := AcquireLock(filepath.Join(dir, lockFileName), false)
	if err != nil {
		t.Fatalf("선점 잠금: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	hashes := []string{strings.Repeat("c", 64)} // 락 획득 전에 반환되므로 실재 blob 불필요
	var rep HookPurgeReport
	start := time.Now()
	err = s.reclaimHookBlobs(ctx, hashes, &rep)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v want context.DeadlineExceeded(F4 계약)", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ctx 미관측 의심 — %v 소요(5초 하드 대기로 lockStore 회귀 가능성, F3)", elapsed)
	}
}

// --- D73: shadow 술어 색인(버전 스위치 밖) ---

// shadowIndexNames — D73 대상 색인. 진단의 분자와 같은 집합이다.
var shadowIndexNames = []string{
	"idx_sources_artifact_kind",
	"idx_sources_blobhash_kind",
	"idx_sources_artifact_indexed",
}

// countShadowIndexes — sqlite_master에서 대상 색인의 실재 수를 센다. sources 전체 색인 수를
// 세면 uri TEXT PRIMARY KEY의 autoindex가 포함돼 "색인 없음"이 0으로 관측되지 않는다.
// testing.TB로 잡은 이유: BenchmarkShadowPredicate(D73 §2 ⑧)가 *testing.B로도 이 함수를
// 불러야 해서 — 두 번째 병렬 카운터를 만들지 않고 시그니처만 넓힌다.
func countShadowIndexes(tb testing.TB, db *sql.DB) int {
	tb.Helper()
	n := 0
	for _, name := range shadowIndexNames {
		var got string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got)
		if err == nil {
			n++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			tb.Fatalf("index 조회(%s): %v", name, err)
		}
	}
	return n
}

// TestEnsureIndexesOnFreshDB — D73: 신규 생성 DB가 **첫 Open**에서 색인까지 도달하고
// user_version은 1로 남는다(구 바이너리 호환 회귀 방지).
func TestEnsureIndexesOnFreshDB(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	if got := countShadowIndexes(t, s.Reader()); got != len(shadowIndexNames) {
		t.Fatalf("indexes=%d want %d", got, len(shadowIndexNames))
	}
	var v int
	if err := s.Reader().QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Fatalf("user_version=%d want %d — 이 결정은 버전을 올리지 않는다", v, SchemaVersion)
	}
}

// TestEnsureIndexesOnExistingV1 — D73: 색인 없는 기존 v1 DB가 Open 후 색인을 얻는다.
// 버전 스위치 안에 두면 case v == SchemaVersion이 DDL 앞에서 반환해 이 테스트가 실패한다.
func TestEnsureIndexesOnExistingV1(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	for _, name := range shadowIndexNames { // v0.12 상태(색인 없음)를 만든다
		if _, err := s.writer.Exec("DROP INDEX IF EXISTS " + name); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close() // 전용 종료 헬퍼는 없다 — openAt의 cleanup이 두 번째 Close 오류를 버린다
	s2 := openAt(t, dir)
	if got := countShadowIndexes(t, s2.Reader()); got != len(shadowIndexNames) {
		t.Fatalf("재Open 후 indexes=%d want %d", got, len(shadowIndexNames))
	}
}

// TestShadowPredicateResultUnchangedByIndexes — D73: 색인은 성능 변경이며 의미 변경이 아니다.
// 같은 데이터에서 색인 유/무의 술어 결과 집합이 같아야 한다.
func TestShadowPredicateResultUnchangedByIndexes(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	seedShadowMixed(t, s) // 아래에서 새로 만드는 픽스처(기존 seed*는 uri 재사용으로 조합 불가)
	withIdx := shadowOwnedHashesT(t, s)
	if len(withIdx) == 0 {
		t.Fatal("픽스처가 귀속 hash를 만들지 못했다 — 빈 결과끼리의 비교는 무의미하다")
	}
	for _, name := range shadowIndexNames {
		if _, err := s.writer.Exec("DROP INDEX IF EXISTS " + name); err != nil {
			t.Fatal(err)
		}
	}
	withoutIdx := shadowOwnedHashesT(t, s)
	if !reflect.DeepEqual(withIdx, withoutIdx) {
		t.Fatalf("색인이 결과를 바꿨다: with=%v without=%v", withIdx, withoutIdx)
	}
}

// shadowOwnedHashesT — shadowOwnedFilter(0, 0)의 질의를 실행해 hash 슬라이스를 정렬 반환한다.
func shadowOwnedHashesT(t *testing.T, s *Store) []string {
	t.Helper()
	q, args := shadowOwnedFilter(0, 0)
	rows, err := s.Reader().Query(q, args...)
	if err != nil {
		t.Fatalf("shadow 술어: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// seedShadowMixed — 귀속 hash 1개(hook 단독)와 비귀속 hash 1개(hook + file이 같은 콘텐츠를
// 공유)를 서로 다른 콘텐츠·uri로 등록한다. 기존 seed*(store_test.go:1719-1768)는 전부 같은
// 콘텐츠와 uri "shadow:Bash:1"을 재사용하므로 조합하면 upsert로 덮인다 — regSource로 직접 만든다.
func seedShadowMixed(t *testing.T, st *Store) {
	t.Helper()
	regSource(t, st, "owned-by-hook-only", "text/plain", "shadow:Bash:owned", "hook")
	regSource(t, st, "shared-with-file", "text/plain", "shadow:Bash:shared", "hook")
	regSource(t, st, "shared-with-file", "text/plain", "file:///tmp/shared.txt", "file")
}

// TestEnsureIndexesReopenIsNoop — D73 §2 ③: 색인이 이미 셋 다 있는 DB를 다시 Open해도 오류가
// 없고 3/3이 유지된다. DDL은 IF NOT EXISTS라 멱등이므로 호출 횟수 자체는 계약이 아니다 —
// 계약은 ① 정상 경로마다 도달 ② 재개시 결과 불변 ③ 기동 예산 내(Task 2b)다.
func TestEnsureIndexesReopenIsNoop(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	if got := countShadowIndexes(t, s.Reader()); got != len(shadowIndexNames) {
		t.Fatalf("첫 Open indexes=%d want %d", got, len(shadowIndexNames))
	}
	_ = s.Close()
	s2 := openAt(t, dir)
	if got := countShadowIndexes(t, s2.Reader()); got != len(shadowIndexNames) {
		t.Fatalf("재Open indexes=%d want %d", got, len(shadowIndexNames))
	}
	var v int
	if err := s2.Reader().QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Fatalf("user_version=%d want %d", v, SchemaVersion)
	}
}

// TestShadowPredicateUsesIndexes — D73: 수치 기록만으로는 색인이 질의 계획에 반영되지 않아도
// 통과하므로, 계획에 sources 전체 스캔이 없음을 단정한다.
func TestShadowPredicateUsesIndexes(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	seedShadowMixed(t, s)
	q, args := shadowOwnedFilter(time.Now().Unix(), 100)
	rows, err := s.Reader().Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	// 판정 ①: 세 색인이 계획에 각각 나타난다.
	for _, name := range shadowIndexNames {
		if !strings.Contains(joined, name) {
			t.Fatalf("계획에 %s가 없다:\n%s", name, joined)
		}
	}
	// 판정 ②: "USING"이 없는 맨 SCAN이 없다. **별칭 SCAN 자체를 금지하면 안 된다** — 색인이
	// 전부 있어도 구동 테이블은 "SCAN sh USING COVERING INDEX idx_sources_artifact_kind"로
	// 남는다(실측). 그리고 술어의 sources 참조는 전부 별칭(sh·s2·s3·s4)이라 "SCAN sources"를
	// 찾는 형태는 색인이 없어도 항상 통과한다.
	for _, d := range plan {
		if strings.HasPrefix(strings.TrimSpace(d), "SCAN ") && !strings.Contains(d, "USING") {
			t.Fatalf("색인 없는 맨 SCAN이 남아 있다: %q\n전체 계획:\n%s", d, joined)
		}
	}
}

// TestEnsureIndexesFailureDoesNotBlockOpen — D73: 색인 DDL 실패는 Open을 막지 않고 경고로
// 남으며, 그 상태에서도 술어가 정확하고 진단의 실재 수가 3보다 작게 관측된다.
func TestEnsureIndexesFailureDoesNotBlockOpen(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	seedShadowMixed(t, s)
	want := shadowOwnedHashesT(t, s)
	// 대상 이름 하나를 다른 객체가 선점하게 만들어 CREATE INDEX를 실패시킨다.
	for _, name := range shadowIndexNames {
		if _, err := s.writer.Exec("DROP INDEX IF EXISTS " + name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.writer.Exec(`CREATE TABLE idx_sources_artifact_kind(x)`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	// §2 ⑩: 실패가 slog.Warn으로 관측되는지 본다 — 기본 로거를 갈아 캡처하고 되돌린다.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	s2 := openAt(t, dir) // Open이 실패하지 않아야 한다
	warnLines := 0
	for _, ln := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		if !strings.Contains(ln, "색인 생성 실패") {
			continue
		}
		warnLines++
		if !strings.Contains(ln, "idx_sources_artifact_kind") {
			t.Fatalf("경고에 실패한 색인 이름이 없다: %q", ln)
		}
	}
	if warnLines < 1 || warnLines > len(shadowIndexNames) {
		t.Fatalf("경고 줄 수=%d want 1..%d\n%s", warnLines, len(shadowIndexNames), logBuf.String())
	}
	// 정확히 실패한 색인 1개만 없어야 한다(= len-1). ">= len"만 보면 첫 실패에서 멈추는
	// 회귀(나머지 2개도 안 생겨 got=0)와 계속 진행(got=2)을 구분하지 못한다 — 리뷰 F1.
	if got := countShadowIndexes(t, s2.Reader()); got != len(shadowIndexNames)-1 {
		t.Fatalf("indexes=%d want %d(실패 1개 제외 나머지) — 첫 실패에서 멈추면 나머지도 안 생긴다", got, len(shadowIndexNames)-1)
	}
	if got := shadowOwnedHashesT(t, s2); !reflect.DeepEqual(got, want) {
		t.Fatalf("색인 부분 적용이 술어 결과를 바꿨다: got=%v want=%v", got, want)
	}
}

// --- D73 §2 ⑦⑧: 기동 예산 판정 + 술어 전후 벤치 ---

// openTB — openAt(store_test.go:1696)의 testing.TB 대응. 벤치에서도 써야 해서 *testing.T 고정을
// 푼 것뿐이고 cleanup 등록은 같다.
func openTB(tb testing.TB, dir string) *Store {
	tb.Helper()
	s, err := Open(dir, false)
	if err != nil {
		tb.Fatalf("Open(%s): %v", dir, err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}

// dropShadowIndexesOn — 열린 Store에서 대상 색인 셋을 지운다(색인 없는 상태 재현).
func dropShadowIndexesOn(tb testing.TB, s *Store) {
	tb.Helper()
	for _, name := range shadowIndexNames {
		if _, err := s.writer.Exec("DROP INDEX IF EXISTS " + name); err != nil {
			tb.Fatalf("DROP INDEX %s: %v", name, err)
		}
	}
}

// seedSourcesTB — sources n행을 artifacts 여러 행에 groupSize개씩 나눠 한 트랜잭션에 직접
// 넣는다. regSource를 n번 부르지 않는 이유는 그것이 blob·chunk·FTS까지 만들어 2만 행에서 측정
// 대상보다 픽스처 생성이 더 오래 걸리기 때문이다 — 술어가 훑는 것은 sources이므로 그 규모만
// 맞추면 된다. artifact_id를 하나로 몰지 않는 이유(리뷰 F1): 복합 색인 3개의 선두 컬럼은
// artifact_id·raw_blob_hash이고 D73이 명명한 비용 축은 "총 shadow 아티팩트 수"다 — 전부
// artifact_id=1이면 그 선두 컬럼의 선택도가 0이 되어 색인 유/무 비교가 대상 효과를 드러낼 수
// 없다(리뷰가 지적한 문제 — 이전 버전은 이 함수가 artifact_id=1로 전부 몰았다). groupSize는
// "적은 수"를 나타내는 임의 고정값이며 결과를 보고 고른 값이 아니다.
func seedSourcesTB(tb testing.TB, s *Store, n int) {
	tb.Helper()
	const groupSize = 5
	tx, err := s.writer.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	insArt, err := tx.Prepare(`INSERT INTO artifacts(id, content_hash, media_type, byte_length, created_at) VALUES(?, ?, 'text/plain', 1, 0)`)
	if err != nil {
		tb.Fatal(err)
	}
	insSrc, err := tx.Prepare(`INSERT INTO sources(uri, artifact_id, source_kind, indexed_at) VALUES(?, ?, 'hook', 0)`)
	if err != nil {
		tb.Fatal(err)
	}
	var artifactID int64
	for i := 0; i < n; i++ {
		if i%groupSize == 0 {
			artifactID++
			if _, err := insArt.Exec(artifactID, fmt.Sprintf("h%d", artifactID)); err != nil {
				tb.Fatalf("artifacts 삽입 %d: %v", artifactID, err)
			}
		}
		if _, err := insSrc.Exec(fmt.Sprintf("shadow:Bash:%d", i), artifactID); err != nil {
			tb.Fatalf("sources 삽입 %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// TestOpenBudgetWithIndexDDL — D73 §2 ⑦: 색인 생성이 기동 예산 안이다. alwaysLoad는 서버가
// 연결될 때까지 세션 시작을 막고 호스트 5초 타임아웃이 상한이므로(D63) 초과를 실패로 단정한다.
// 픽스처는 색인 없는 상태로 닫고, 다시 Open하는 구간을 잰다 — 그 구간이 DDL을 실행한다.
func TestOpenBudgetWithIndexDDL(t *testing.T) {
	const budget = 5 * time.Second
	for _, rows := range []int{2000, 20000} {
		t.Run(fmt.Sprintf("sources=%d", rows), func(t *testing.T) {
			dir := t.TempDir()
			seed := openTB(t, dir)
			seedSourcesTB(t, seed, rows)
			dropShadowIndexesOn(t, seed)
			_ = seed.Close()

			start := time.Now()
			s := openTB(t, dir)
			elapsed := time.Since(start)

			if got := countShadowIndexes(t, s.Reader()); got != len(shadowIndexNames) {
				t.Fatalf("indexes=%d want %d — Open이 색인을 만들지 않았다", got, len(shadowIndexNames))
			}
			t.Logf("rows=%d Open+색인 생성 %v", rows, elapsed)
			if elapsed >= budget {
				t.Fatalf("Open이 기동 예산을 넘었다: %v >= %v (rows=%d)", elapsed, budget, rows)
			}
		})
	}
}

// BenchmarkShadowPredicate — D73 §2 ⑧: 술어 실행 시간을 색인 유/무로 비교한다. 결과는 설계 §3에
// 실측으로 기록한다(기대값을 미리 적지 않는다).
func BenchmarkShadowPredicate(b *testing.B) {
	for _, rows := range []int{2000, 20000} {
		for _, withIndex := range []bool{false, true} {
			b.Run(fmt.Sprintf("sources=%d/index=%v", rows, withIndex), func(b *testing.B) {
				dir := b.TempDir()
				s := openTB(b, dir)
				seedSourcesTB(b, s, rows)
				if !withIndex {
					dropShadowIndexesOn(b, s)
				}
				// 리뷰 F2: 이름이 주장하는 색인 상태를 실제로 확인한다 — ensureIndexes는 실패를
				// 경고로만 남기고 반환하지 않는 fail-soft이고, dropShadowIndexesOn은 DROP INDEX
				// IF EXISTS라 두 팔이 조용히 같은 상태로 남아도 아무 것도 실패하지 않는다.
				wantIdx := 0
				if withIndex {
					wantIdx = len(shadowIndexNames)
				}
				if got := countShadowIndexes(b, s.Reader()); got != wantIdx {
					b.Fatalf("indexes=%d want %d(index=%v) — 이 팔이 이름이 주장하는 색인 상태가 아니다", got, wantIdx, withIndex)
				}
				q, args := shadowOwnedFilter(time.Now().Unix(), 100)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					rs, err := s.Reader().Query(q, args...)
					if err != nil {
						b.Fatal(err)
					}
					for rs.Next() {
					}
					if err := rs.Err(); err != nil {
						b.Fatal(err)
					}
					_ = rs.Close()
				}
			})
		}
	}
}

// TestPurgeHookOnlyLockHoldBudget — D77: 기동 purge의 실 잠금 보유 시간이 훅 예산의
// 절반을 넘지 않는다. 형태는 D73 §2⑦(TestOpenBudgetWithIndexDDL)을 따른다 —
// elapsed >= budget이면 Fatalf이며 수동 판독이 아니다.
//
// 픽스처는 두 조건을 함께 만족해야 한다(설계 v0.14 §2.1):
//   - 경로 조건: 블롭 mtime을 gcOrphanMinAge 이전으로 소급시킨다. 그러지 않으면
//     reclaimHookBlobs의 나이 게이트가 단락되어 stillReferenced 쿼리도 os.Remove도
//     실행되지 않고, 유예 경로만 재는 종이 게이트가 된다. 단정은 Hashes·Deferred·
//     Failed를 함께 본다(TestPurgeHookOnly, :1917과 같은 관용구) — Failed를 빼면
//     rename/unlink 실패(store.go:1242·:1254, Windows 공유 위반)로 일부 해시만
//     회수돼도 ReclaimedB>0·Deferred==0이 성립해 측정량이 줄어든 것을 감춘 채
//     넉넉한 여유를 보고한다.
//   - 규모 조건: 개발기 실측이 budget의 1/5 이하가 되는 해시 수를 쓴다. 이 게이트는
//     CI에서 3-OS와 -race로 네 번 도는데, 원 실측(100해시 632ms) 대비 여유 1.58배로는
//     러너 편차 한 번이 BLOCKED가 된다.
//
// 그래서 이 게이트가 잡는 것은 **해시당 비용의 회귀**다. startupPurgeMaxHashes(100해시)
// 배치의 절대 보유 시간은 합성 픽스처로 검증할 수 없다(실 저장소와 분포가 다르다).
func TestPurgeHookOnlyLockHoldBudget(t *testing.T) {
	const (
		budget = 1000 * time.Millisecond // 훅 총예산 2000ms의 50%(설계 v0.14 D77)
		hashes = 20                      // 규모 조건: 개발기 실측이 budget/5 이하가 되도록 고른 값
	)
	st := openAt(t, t.TempDir())
	for i := 0; i < hashes; i++ {
		c := fmt.Sprintf("lock-hold-budget-%d", i)
		regSource(t, st, c, "text/plain", fmt.Sprintf("shadow:Bash:%d", i), "hook")
		ageBlobFile(t, st, hashOf(c), -2*gcOrphanMinAge) // 경로 조건
	}

	start := time.Now()
	rep, err := st.PurgeHookOnlyOlderThan(t.Context(), 0, hashes)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PurgeHookOnlyOlderThan: %v", err)
	}
	if rep.Hashes != hashes {
		t.Fatalf("rep.Hashes=%d want %d — 픽스처가 의도한 규모로 처리되지 않았다", rep.Hashes, hashes)
	}
	if rep.ReclaimedB <= 0 || rep.DeferredFiles != 0 || rep.FailedFiles != 0 {
		t.Fatalf("회수 경로를 타지 않았다: ReclaimedB=%d DeferredFiles=%d FailedFiles=%d — 나이 게이트가 단락되면 유예 경로만 재는 종이 게이트가 되고, rename/unlink가 실패하면 줄어든 배치를 재고도 통과한다",
			rep.ReclaimedB, rep.DeferredFiles, rep.FailedFiles)
	}
	t.Logf("hashes=%d reclaimed=%dB deferred=%d failed=%d 보유 %v (budget %v, 여유 %.1f배)",
		rep.Hashes, rep.ReclaimedB, rep.DeferredFiles, rep.FailedFiles, elapsed, budget, float64(budget)/float64(elapsed))
	if elapsed >= budget {
		t.Fatalf("잠금 보유가 예산을 넘었다: %v >= %v (hashes=%d reclaimed=%dB deferred=%d failed=%d) — 개발기 기준값은 이 절반 이하였다",
			elapsed, budget, rep.Hashes, rep.ReclaimedB, rep.DeferredFiles, rep.FailedFiles)
	}
}

// regChunked: regSource와 같되 Chunks를 명시로 실어 **FTS 행을 실제로 만든다**. regSource
// (store_test.go:1717)는 Chunks 없는 Registration을 만들고 Register는 reg.Chunks에서만
// chunks를 INSERT하므로(store.go:459) FTS 행이 0개다 — 인덱스 바이트를 재는 테스트가 그
// 헬퍼로 시드하면 병합 유무와 무관하게 통과한다.
func regChunked(t *testing.T, st *Store, content, uri string) {
	t.Helper()
	if _, err := st.Register(t.Context(), Registration{
		StoredBytes: []byte(content), MediaType: "text/plain",
		Source: SourceMeta{URI: uri, Kind: "hook", SrcHash: "sh-" + uri},
		Chunks: []Chunk{{Ordinal: 0, ByteEnd: int64(len(content)), Text: content}},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMergeFTSShrinksIndex: 삭제가 남긴 세그먼트 표식을 MergeFTS가 걷어낸다.
// FTS5 외부 콘텐츠 테이블의 삭제는 tombstone을 새 세그먼트에 쌓을 뿐이라, 병합 없이는
// 행을 다 지워도 _data 바이트가 줄지 않는다 — 그것이 D102가 고치는 결함이고, 이 테스트가
// 고정하는 것도 "삭제만으로는 안 준다"와 "병합하면 준다" 두 가지다.
func TestMergeFTSShrinksIndex(t *testing.T) {
	st := openAt(t, t.TempDir())
	body := strings.Repeat("alpha beta gamma delta epsilon ", 4000) // 약 120 KB/건
	for i := range 20 {
		regChunked(t, st, body+strconv.Itoa(i), "shadow:Bash:seg"+strconv.Itoa(i))
	}
	seeded := ftsDataBytes(t, st, "fts_trigram_data")
	if seeded == 0 {
		t.Fatal("시드가 FTS 인덱스를 만들지 않았다 — 이 테스트가 공허 통과한다")
	}

	if _, err := st.writer.Exec(`DELETE FROM chunks`); err != nil {
		t.Fatalf("DELETE FROM chunks: %v", err)
	}
	afterDelete := ftsDataBytes(t, st, "fts_trigram_data")
	if afterDelete < seeded {
		t.Fatalf("삭제만으로 인덱스가 줄었다(%d → %d) — D102의 전제가 이 환경에서 성립하지 않는다",
			seeded, afterDelete)
	}

	if err := st.MergeFTS(t.Context()); err != nil {
		t.Fatalf("MergeFTS: %v", err)
	}
	// 줄었다는 것만으로는 "줄이면서 망가뜨린" 변경을 못 잡는다(최종리뷰 F1) — external-content
	// 대조까지 건다. rank=1이라 chunks↔인덱스 행/토큰 드리프트도 여기서 걸린다.
	if err := st.checkFTSIntegrity(t.Context()); err != nil {
		t.Fatalf("병합이 chunks↔인덱스 대조를 깼다: %v", err)
	}
	merged := ftsDataBytes(t, st, "fts_trigram_data")
	if merged >= afterDelete {
		t.Fatalf("MergeFTS 후에도 인덱스가 줄지 않았다: %d → %d", afterDelete, merged)
	}
}

// TestMergeFTSKeepsSearchable: **부분 삭제 후** 병합이 두 축(porter·trigram)에서 생존분만
// 반환한다 — optimize는 세그먼트를 합칠 뿐 색인 내용을 바꾸지 않아야 한다.
//
// 옛 형태는 **문서 1건·porter 축·삭제 없음**의 매치 수 하나뿐이었다(최종리뷰 F1). 그 그물은
// 무인으로 매일 전체 인덱스를 다시 쓰는 변경에 대해 셋을 못 잡는다: 병합이 tombstone을 잘못
// 접어 지운 문서를 되살리는 것, 생존 문서를 잃는 것, porter는 멀쩡한데 trigram만 망가지는 것
// (축소를 단정하는 축이 바로 trigram이다). 여기서 재는 것 넷 —
// ① 생존분이 두 축 모두에서 나온다 ② 삭제분이 두 축 모두에서 안 나온다 ③ 공통어의 총 히트가
// 생존 건수와 정확히 같다(되살아난 tombstone이 여기서 걸린다) ④ 병합 뒤 checkFTSIntegrity가
// 통과한다(chunks와 인덱스가 어긋나면 실패).
func TestMergeFTSKeepsSearchable(t *testing.T) {
	st := openAt(t, t.TempDir())
	const docs = 6 // 짝수 색인은 지우고 홀수 색인은 남긴다 — 전량 삭제와 달리 tombstone과 생존분이 섞인다
	filler := strings.Repeat("filler token ", 500)
	uri := func(i int) string { return "shadow:Bash:keep" + strconv.Itoa(i) }
	for i := range docs {
		regChunked(t, st, "haystack needle"+strconv.Itoa(i)+" "+filler, uri(i))
	}
	for _, fts := range [2]string{"fts_porter", "fts_trigram"} {
		if n := ftsMatchCount(t, st, fts, "haystack"); n != docs {
			t.Fatalf("%s 시드가 %d/%d만 검색된다 — 이 테스트가 공허 통과한다", fts, n, docs)
		}
	}

	// 부분 삭제 — chunks_ad 트리거가 두 축에 tombstone을 쌓는다. 병합이 접어야 할 대상이다.
	for i := 0; i < docs; i += 2 {
		if _, err := st.writer.Exec(
			`DELETE FROM chunks WHERE artifact_id IN (SELECT artifact_id FROM sources WHERE uri = ?)`,
			uri(i),
		); err != nil {
			t.Fatalf("DELETE chunks(%s): %v", uri(i), err)
		}
	}

	if err := st.MergeFTS(t.Context()); err != nil {
		t.Fatalf("MergeFTS: %v", err)
	}
	if err := st.checkFTSIntegrity(t.Context()); err != nil {
		t.Fatalf("병합이 chunks↔인덱스 대조를 깼다: %v", err)
	}

	for _, fts := range [2]string{"fts_porter", "fts_trigram"} {
		if n := ftsMatchCount(t, st, fts, "haystack"); n != docs/2 {
			t.Fatalf("%s 공통어 히트 %d, 기대 %d — 병합이 생존/삭제 집합을 바꿨다", fts, n, docs/2)
		}
		for i := range docs {
			want, term := int64(1), "needle"+strconv.Itoa(i)
			if i%2 == 0 {
				want = 0 // 삭제분 — 되살아나면 여기서 걸린다
			}
			if n := ftsMatchCount(t, st, fts, term); n != want {
				t.Fatalf("%s MATCH %q = %d, 기대 %d", fts, term, n, want)
			}
		}
	}
}

// ftsDataBytes: FTS5 그림자 테이블의 block 바이트 합 — 세그먼트 실점유의 직접 측정이다.
// 행 수가 아니라 바이트를 재는 이유는, 병합이 줄이는 것이 세그먼트 수가 아니라 중복
// 저장된 포스팅 바이트이기 때문이다.
func ftsDataBytes(t *testing.T, st *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := st.reader.QueryRow(
		`SELECT coalesce(sum(length(block)),0) FROM ` + table,
	).Scan(&n); err != nil {
		t.Fatalf("%s 조회: %v", table, err)
	}
	return n
}

// ftsMatchCount: 지정한 축(fts_porter·fts_trigram)에서 term의 히트 수. 축을 인자로 받는 이유는
// 보존 증거가 한 축에만 있으면 다른 축이 병합에 망가져도 통과하기 때문이다(최종리뷰 F1).
func ftsMatchCount(t *testing.T, st *Store, table, term string) int64 {
	t.Helper()
	var n int64
	if err := st.reader.QueryRow(
		`SELECT count(*) FROM `+table+` WHERE `+table+` MATCH ?`, term,
	).Scan(&n); err != nil {
		t.Fatalf("%s MATCH %q: %v", table, term, err)
	}
	return n
}

// TestMergeFTSIfDueStamp: 스탬프가 없으면 돌고, 방금 돌았으면 안 돌고, interval이 지나면
// 다시 돈다. 조건이 **시간 하나**라는 것이 계약이다 — 삭제 건수는 조건에 들어가지 않는다
// (설계 v0.20 D102 계약 2: 세그먼트는 삽입으로도 쌓이므로 건수 문턱은 삽입만 있고 삭제가
// 적은 구간에서 병합을 영영 막는다).
func TestMergeFTSIfDueStamp(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	base := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)

	ran, err := st.MergeFTSIfDue(t.Context(), 24*time.Hour, base)
	if err != nil || !ran {
		t.Fatalf("스탬프 없음에서 안 돌았다: ran=%v err=%v", ran, err)
	}
	ran, err = st.MergeFTSIfDue(t.Context(), 24*time.Hour, base.Add(23*time.Hour))
	if err != nil || ran {
		t.Fatalf("interval 이내에 또 돌았다: ran=%v err=%v", ran, err)
	}
	ran, err = st.MergeFTSIfDue(t.Context(), 24*time.Hour, base.Add(25*time.Hour))
	if err != nil || !ran {
		t.Fatalf("interval 경과 후 안 돌았다: ran=%v err=%v", ran, err)
	}
}

// TestFTSMergeDueInRemaining — 릴리스 패스 B3: 만기까지의 **잔여**를 낸다. 자동 루프가 다음
// 확인 시각을 이 값으로 잡으므로, 여기서 interval을 통째로 돌려주면 "하루 한 번"이 최대 약
// 48시간이 된다. 판정 불가(스탬프 부재·미래 mtime)와 이미 만기는 0이다 — MergeFTSIfDue의
// "돌 때가 됐다" 둘과 같은 산술이라는 것이 계약이다.
func TestFTSMergeDueInRemaining(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	const interval = 24 * time.Hour
	base := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)

	if d := st.FTSMergeDueIn(interval, base); d != 0 {
		t.Fatalf("스탬프 부재에서 잔여=%v, 기대 0(즉시 만기)", d)
	}
	if ran, err := st.MergeFTSIfDue(t.Context(), interval, base); err != nil || !ran {
		t.Fatalf("스탬프 시드 실패: ran=%v err=%v", ran, err)
	}

	// 스탬프가 base에 찍혔다 — 23시간 뒤의 잔여는 정확히 1시간이어야 한다.
	if d := st.FTSMergeDueIn(interval, base.Add(23*time.Hour)); d != time.Hour {
		t.Fatalf("잔여=%v, 기대 1h — interval을 통째로 돌려주면 다음 확인이 만기를 지나친다", d)
	}
	if d := st.FTSMergeDueIn(interval, base.Add(interval)); d != 0 {
		t.Fatalf("만기에서 잔여=%v, 기대 0", d)
	}
	// 미래 mtime: MergeFTSIfDue가 "돌 때가 됐다"로 읽는 자리라 잔여도 0이어야 한다 —
	// 여기서 양수를 내면 그 저장소는 영구히 병합하지 않는다.
	if d := st.FTSMergeDueIn(interval, base.Add(-72*time.Hour)); d != 0 {
		t.Fatalf("미래 스탬프에서 잔여=%v, 기대 0(영구 정지 방지)", d)
	}
}

// TestMergeFTSIfDueFutureStampIsDue: now보다 **미래**인 스탬프도 "돌 때가 됐다"로 읽는다.
// 시계 되돌림·복원·파일시스템 타임스탬프 이상으로 미래 mtime이 생기면 경과가 음수가 되는데,
// 그것을 "아직 이르다"로 읽으면 그 저장소는 **영구히** 병합하지 않는다(설계 v0.20 D102 계약 2).
func TestMergeFTSIfDueFutureStampIsDue(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	stamp := filepath.Join(dir, mergeStampName)
	f, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("스탬프 생성: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("스탬프 Close: %v", err)
	}
	future := now.Add(72 * time.Hour)
	if err := os.Chtimes(stamp, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	ran, err := st.MergeFTSIfDue(t.Context(), 24*time.Hour, now)
	if err != nil || !ran {
		t.Fatalf("미래 mtime에서 안 돌았다(영구 정지): ran=%v err=%v", ran, err)
	}
	fi, err := os.Stat(stamp)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.ModTime().After(now) {
		t.Fatalf("병합 후에도 스탬프가 미래에 남았다: %v", fi.ModTime())
	}
}

// TestMergeFTSIfDueFailureDoesNotStamp: 병합이 실패하면 스탬프를 찍지 않는다 — 찍으면 그
// 프로젝트는 하루 동안 재시도조차 하지 않는다. writer를 닫아 실패를 만든다.
func TestMergeFTSIfDueFailureDoesNotStamp(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	if err := st.writer.Close(); err != nil {
		t.Fatalf("writer Close: %v", err)
	}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	if _, err := st.MergeFTSIfDue(t.Context(), 24*time.Hour, now); err == nil {
		t.Fatal("닫힌 writer에서 오류가 나지 않았다")
	}
	if _, err := os.Stat(filepath.Join(dir, mergeStampName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("실패했는데 스탬프가 찍혔다: %v", err)
	}
}
