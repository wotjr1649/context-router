package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
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
