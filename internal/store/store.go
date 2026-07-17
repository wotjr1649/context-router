// Package store — DB 수명·PRAGMA·스키마·단일 트랜잭션 계약·blob IO. 설계서 §3.3~3.6.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // 유일하게 허용되는 blank import (규약 §10)
)

var (
	ErrNotFound    = errors.New("store: not found")
	ErrUnavailable = errors.New("store: unavailable")
	ErrConflict    = errors.New("store: conflict")
)

const SchemaVersion = 1

type Store struct {
	dir            string
	writer, reader *sql.DB
	ledger         *sql.DB
}

const pragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

func Open(dir string, readOnly bool) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "content.db")) + pragmas
	if readOnly {
		dsn += "&mode=ro&_pragma=query_only(ON)"
	}
	w, err := sql.Open("sqlite", dsn)
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
		if _, err := s.writer.Exec(schemaV1); err != nil {
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
CREATE TABLE artifacts(
  id INTEGER PRIMARY KEY, content_hash TEXT NOT NULL UNIQUE, media_type TEXT NOT NULL,
  byte_length INTEGER NOT NULL, redaction TEXT NOT NULL DEFAULT 'none', created_at INTEGER NOT NULL);
CREATE TABLE sources(
  uri TEXT PRIMARY KEY, artifact_id INTEGER NOT NULL REFERENCES artifacts(id),
  source_kind TEXT NOT NULL, src_size INTEGER, src_mtime_ns INTEGER, src_hash TEXT,
  raw_blob_hash TEXT, extraction TEXT, indexed_at INTEGER NOT NULL);
CREATE TABLE chunks(
  id INTEGER PRIMARY KEY, artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  ordinal INTEGER NOT NULL, byte_start INTEGER, byte_end INTEGER, line_start INTEGER, line_end INTEGER,
  title TEXT, text TEXT NOT NULL, UNIQUE(artifact_id, ordinal));
CREATE VIRTUAL TABLE fts_porter  USING fts5(title, text, content='chunks', content_rowid='id', tokenize='porter unicode61');
CREATE VIRTUAL TABLE fts_trigram USING fts5(title, text, content='chunks', content_rowid='id', tokenize='trigram');
CREATE TRIGGER chunks_ai AFTER INSERT ON chunks BEGIN
  INSERT INTO fts_porter(rowid, title, text) VALUES (new.id, new.title, new.text);
  INSERT INTO fts_trigram(rowid, title, text) VALUES (new.id, new.title, new.text);
END;
CREATE TRIGGER chunks_ad AFTER DELETE ON chunks BEGIN
  INSERT INTO fts_porter(fts_porter, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
  INSERT INTO fts_trigram(fts_trigram, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
END;
CREATE TRIGGER chunks_au AFTER UPDATE ON chunks BEGIN
  INSERT INTO fts_porter(fts_porter, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
  INSERT INTO fts_porter(rowid, title, text) VALUES (new.id, new.title, new.text);
  INSERT INTO fts_trigram(fts_trigram, rowid, title, text) VALUES ('delete', old.id, old.title, old.text);
  INSERT INTO fts_trigram(rowid, title, text) VALUES (new.id, new.title, new.text);
END;
PRAGMA user_version = 1;`

func (s *Store) Close() error {
	if s.ledger != nil {
		s.ledger.Close()
	}
	s.writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	s.reader.Close()
	return s.writer.Close()
}
