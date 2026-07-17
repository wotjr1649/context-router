package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
