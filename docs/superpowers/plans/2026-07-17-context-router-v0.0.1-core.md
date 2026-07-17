# Context Router v0.0.1 코어(M1~M3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 동작하는 3-도구 MCP 서버 — `ctr_index`(옵트인)로 색인, `ctr_search`로 검색, `ctr_fetch`로 byte-exact 회수. 저장 계약(§3)·수집 보안(§5.1) 포함.

**Architecture:** 단일 Go 바이너리, stdio MCP. leaf 패키지(ident/store) 위에 ingest/search, 최상위 mcp가 배선. 쓰기는 writer `*sql.DB(MaxOpenConns=1)`+`BEGIN IMMEDIATE` 단일 트랜잭션으로 직렬화.

**Tech Stack:** Go 1.25+, modelcontextprotocol/go-sdk v1.6.1, modernc.org/sqlite v1.54.0(+libc v1.74.1), stdlib(flag/slog/testing). starlark·readability는 계획 2.

## Global Constraints (설계서·규약 원문)

- 의존성: 위 4개 모듈 외 추가 금지 (D8; starlark-go 등 3개는 계획 2에서).
- 파일: 이 계획이 만드는 소스 = `cmd/context-router/main.go`, `internal/{ident,store,ingest,search,mcp}/<pkg>.go` 6개 + 패키지당 `_test.go` 1개 (D13: 밴드 300~1,000줄, utils/doc.go 금지, 패키지 주석은 주 파일 상단에 "책임 1줄 + 설계서 §").
- 인터페이스 자체 정의 0개. functional options 금지. 생성자 인자 ≤3 개별/≥4 Config (규약 §4).
- import 방향: 규약 §2 그래프만 허용. mcp는 `database/sql`·`net/http`·`os/exec` import 금지.
- 오류: 패키지 sentinel + mcp `toToolError` 단일 변환. 메시지에 절대경로·env·비밀값 금지 (규약 §6).
- PRAGMA: `WAL, synchronous=NORMAL, busy_timeout=5000, foreign_keys=ON, user_version=1` (D9).
- 모든 테스트 실행: `go test ./... -count=1` (memory-cap 불요 규모지만 `-p 1` 권장), 커밋 메시지 끝에 `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- module path: `github.com/wotjr1649/context-router`.

---

### Task 1: 모듈 스켈레톤 + run() 진입점

**Files:**
- Create: `go.mod`, `cmd/context-router/main.go`
- Test: `cmd/context-router/main_test.go`

**Interfaces:**
- Produces: `run(ctx context.Context, args []string, stdout, stderr io.Writer) error`, `type serverFlags struct { Root, StoreRoot, LogLevel string; Profile, Enable, AllowPaths []string }`, `parseFlags(args []string) (serverFlags, error)`, `const version = "0.0.1-dev"`

- [ ] **Step 1: go.mod 생성 + 실패 테스트 작성**

```bash
go mod init github.com/wotjr1649/context-router
```

`cmd/context-router/main_test.go`:
```go
package main

import (
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    serverFlags
		wantErr bool
	}{
		{"defaults", nil, serverFlags{Profile: []string{"search", "fetch", "transform"}, LogLevel: "info"}, false},
		{"enable", []string{"--enable", "ingest,net"}, serverFlags{Profile: []string{"search", "fetch", "transform"}, Enable: []string{"ingest", "net"}, LogLevel: "info"}, false},
		{"unknown", []string{"--bogus"}, serverFlags{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && strings.Join(got.Profile, ",") != strings.Join(tt.want.Profile, ",") {
				t.Fatalf("profile=%v want %v", got.Profile, tt.want.Profile)
			}
		})
	}
}

func TestBanner(t *testing.T) {
	f := serverFlags{Profile: []string{"search", "fetch", "transform"}, LogLevel: "info"}
	got := banner(f, "C:/proj")
	want := "[ctr] v" + version + " profile=search,fetch,transform ingest=off net=off root=C:/proj"
	if got != want {
		t.Fatalf("banner=%q want %q", got, want)
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./cmd/... -run 'TestParseFlags|TestBanner' -v` / Expected: FAIL (undefined: parseFlags)

- [ ] **Step 3: 최소 구현** — `cmd/context-router/main.go`:

```go
// Command context-router — MCP 서버 진입점·플래그·배선. 설계서 §2.2, §8.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
)

const version = "0.0.1-dev"

type serverFlags struct {
	Root, StoreRoot, LogLevel string
	Profile, Enable, AllowPaths []string
}

func parseFlags(args []string) (serverFlags, error) {
	fs := flag.NewFlagSet("context-router", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f serverFlags
	var profile, enable string
	fs.StringVar(&f.Root, "root", "", "project root (default: cwd)")
	fs.StringVar(&f.StoreRoot, "store-root", "", "store root override")
	fs.StringVar(&profile, "profile", "search,fetch,transform", "tool profile")
	fs.StringVar(&enable, "enable", "", "opt-in profiles: ingest,net")
	fs.StringVar(&f.LogLevel, "log-level", "info", "log level")
	fs.Func("allow-path", "extra ingest root (repeatable)", func(v string) error {
		f.AllowPaths = append(f.AllowPaths, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return serverFlags{}, err
	}
	f.Profile = strings.Split(profile, ",")
	if enable != "" {
		f.Enable = strings.Split(enable, ",")
	}
	return f, nil
}

func banner(f serverFlags, root string) string {
	onoff := func(name string) string {
		if slices.Contains(f.Enable, name) {
			return "on"
		}
		return "off"
	}
	return fmt.Sprintf("[ctr] v%s profile=%s ingest=%s net=%s root=%s",
		version, strings.Join(f.Profile, ","), onoff("ingest"), onoff("net"), root)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	root := f.Root
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	fmt.Fprintln(stderr, banner(f, root))
	_ = ctx // Task 8에서 MCP 서빙 연결
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ctr:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: 통과 확인** — Run: `go test ./cmd/... -v` / Expected: PASS ×2, `go vet ./...` clean

- [ ] **Step 5: Commit** — `git add go.mod cmd && git commit -m "feat: 모듈 스켈레톤과 run() 진입점 (설계 §2.2, §8)"` (+trailer)

---

### Task 2: ident — canonicalization과 프로젝트 ID

**Files:**
- Create: `internal/ident/ident.go`
- Test: `internal/ident/ident_test.go`, `internal/ident/testdata/` (git fixture는 테스트가 생성)

**Interfaces:**
- Produces: `type Canon struct { ProjectRoot, WorktreeRoot, ProjectID, WorktreeID string }`, `func Canonicalize(root string) (Canon, error)`, `func Fold(p string) string` (GOOS별 case-fold + 구분자 통일 — 테스트용 export)

- [ ] **Step 1: 실패 테스트** — `ident_test.go`:

```go
package ident

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

func TestCanonicalize_PlainDir(t *testing.T) {
	dir := t.TempDir()
	c, err := Canonicalize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProjectRoot != c.WorktreeRoot {
		t.Fatalf("plain dir: project=%q worktree=%q", c.ProjectRoot, c.WorktreeRoot)
	}
	if ok, _ := regexp.MatchString(`^[a-z0-9-]{1,32}-[0-9a-f]{12}$`, c.ProjectID); !ok {
		t.Fatalf("bad id %q", c.ProjectID)
	}
}

func TestCanonicalize_GitDirAndWorktreeFile(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "wt1"), 0o755)
	sub := filepath.Join(repo, "src", "deep")
	os.MkdirAll(sub, 0o755)
	c, err := Canonicalize(sub) // .git 디렉터리 상향 탐색
	if err != nil {
		t.Fatal(err)
	}
	if c.ProjectRoot != Fold(mustReal(t, repo)) {
		t.Fatalf("projectRoot=%q want %q", c.ProjectRoot, Fold(mustReal(t, repo)))
	}
	// worktree: .git 파일 + gitdir/commondir 체인
	wt := t.TempDir()
	os.WriteFile(filepath.Join(wt, ".git"),
		[]byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt1")+"\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".git", "worktrees", "wt1", "commondir"),
		[]byte("../..\n"), 0o644)
	c2, err := Canonicalize(wt)
	if err != nil {
		t.Fatal(err)
	}
	if c2.ProjectRoot != Fold(mustReal(t, repo)) {
		t.Fatalf("worktree projectRoot=%q want repo", c2.ProjectRoot)
	}
	if c2.WorktreeRoot == c2.ProjectRoot {
		t.Fatal("worktree root must stay the worktree dir")
	}
}

func TestFold_OSRule(t *testing.T) {
	got := Fold(`C:\Some\Dir`)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		if got != "c:/some/dir" {
			t.Fatalf("fold=%q", got)
		}
	} else if got != `C:\Some\Dir` && got != "C:/Some/Dir" {
		t.Fatalf("linux fold=%q", got)
	}
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(r)
}
```

- [ ] **Step 2: 실패 확인** — `go test ./internal/ident/ -v` → FAIL (undefined)

- [ ] **Step 3: 구현** — `ident.go` (설계 §3.2 알고리즘 9단계 그대로; 핵심):

```go
// Package ident — 경로 canonicalization과 프로젝트/워크트리 ID. 설계서 §3.2.
package ident

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type Canon struct{ ProjectRoot, WorktreeRoot, ProjectID, WorktreeID string }

func Fold(p string) string {
	p = filepath.ToSlash(strings.TrimPrefix(p, `\\?\`))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

func Canonicalize(root string) (Canon, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Canon{}, fmt.Errorf("canonicalize: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Canon{}, fmt.Errorf("canonicalize: %w", err) // 존재하지 않으면 시작 거부 (§2.2)
	}
	worktree := Fold(real)
	project := worktree
	if pr, ok := findGitProjectRoot(real); ok {
		project = Fold(pr)
	}
	return Canon{
		ProjectRoot: project, WorktreeRoot: worktree,
		ProjectID: id(project), WorktreeID: id(worktree),
	}, nil
}

// findGitProjectRoot: 상향 탐색. .git 디렉터리 → 부모. .git 파일 → gitdir: 파싱,
// <gitdir>/commondir 파일이 있으면 그 상대경로를 따라 주 .git으로 → 그 부모. git 바이너리 미호출.
func findGitProjectRoot(start string) (string, bool) {
	for dir := start; ; {
		g := filepath.Join(dir, ".git")
		if fi, err := os.Stat(g); err == nil {
			if fi.IsDir() {
				return dir, true
			}
			b, err := os.ReadFile(g)
			if err != nil {
				return "", false
			}
			gd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
			if !filepath.IsAbs(gd) {
				gd = filepath.Join(dir, gd)
			}
			if cb, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
				common := strings.TrimSpace(string(cb))
				if !filepath.IsAbs(common) {
					common = filepath.Join(gd, common)
				}
				if r, err := filepath.EvalSymlinks(common); err == nil {
					return filepath.Dir(r), true // 주 .git의 부모
				}
			}
			return filepath.Dir(gd), true // submodule류: gitdir의 부모로 근사
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func id(canonical string) string {
	base := slugRe.ReplaceAllString(strings.ToLower(filepath.Base(canonical)), "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "root"
	}
	if len(base) > 32 {
		base = base[:32]
	}
	h := sha256.Sum256([]byte(canonical))
	return base + "-" + hex.EncodeToString(h[:])[:12]
}
```

- [ ] **Step 4: 통과 확인** — `go test ./internal/ident/ -v` → PASS ×3

- [ ] **Step 5: fuzz 시드 추가 + 실행 1회**

```go
func FuzzFold(f *testing.F) {
	f.Add(`C:\a\..\b`)
	f.Add(`\\?\C:\x`)
	f.Add("//server/share/p")
	f.Fuzz(func(t *testing.T, p string) { _ = Fold(p) }) // 불변식: panic 없음
}
```
Run: `go test ./internal/ident/ -run Fuzz -fuzz FuzzFold -fuzztime 5s` → 크래시 0

- [ ] **Step 6: Commit** — `git add internal/ident && git commit -m "feat: ident canonicalization + 프로젝트 ID (설계 §3.2)"`

---

### Task 3: store — open/PRAGMA/스키마/user_version

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `func Open(dir string, readOnly bool) (*Store, error)`, `func (s *Store) Close() error`, `var ErrNotFound, ErrUnavailable, ErrConflict error`, `const SchemaVersion = 1`, 내부: writer db `SetMaxOpenConns(1)` + reader db `SetMaxOpenConns(4)`
- Consumes: 없음 (leaf)

- [ ] **Step 1: 실패 테스트**

```go
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
		"PRAGMA journal_mode":  "wal",
		"PRAGMA foreign_keys":  "1",
		"PRAGMA user_version":  "1",
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
```

- [ ] **Step 2: 실패 확인** — `go test ./internal/store/ -v` → FAIL

- [ ] **Step 3: 구현** — `store.go` 핵심 (설계 §3.4 DDL 전문 + §3.5):

```go
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
```

주의: modernc DSN `_pragma` 문법이 위와 다르면(드라이버 문서 확인: `go doc modernc.org/sqlite`) 연결 후 `Exec("PRAGMA ...")` 방식으로 대체하되 테스트(Step 1)를 계약으로 유지한다.

- [ ] **Step 4: 통과 확인** — `go test ./internal/store/ -v` → PASS ×2
- [ ] **Step 5: Commit** — `git add internal/store go.mod go.sum && git commit -m "feat: store open/PRAGMA/스키마 v1/user_version 계약 (설계 §3.4~3.5)"`

---

### Task 4: store — Register 단일 트랜잭션 + blob IO + ReadRange

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go` (추가)

**Interfaces:**
- Produces:
```go
type SourceMeta struct{ URI, Kind string; Size, MtimeNS int64; SrcHash, RawBlobHash, Extraction string }
type Chunk struct{ Ordinal int; ByteStart, ByteEnd int64; LineStart, LineEnd int; Title, Text string }
type Registration struct {
	StoredBytes []byte; MediaType, Redaction string
	Source SourceMeta; Chunks []Chunk
	ExpectedOldSrcHash string // ""=신규 허용, 그 외=CAS 조건 (§3.5)
}
func (s *Store) Register(ctx context.Context, reg Registration) (artifactID int64, err error)
type Selector struct{ ChunkID int64; LineStart, LineEnd int; ByteStart, ByteEnd int64; Kind string } // Kind: "chunk"|"line"|"byte"
type RangeResult struct{ Text []byte; ByteStart, ByteEnd int64; LineStart, LineEnd int; Artifact ArtifactMeta }
type ArtifactMeta struct{ ID int64; ContentHash, MediaType, Redaction string; ByteLength int64; CreatedAt int64 }
func (s *Store) ReadRange(ctx context.Context, artifactID int64, sel Selector) (RangeResult, error)
func (s *Store) LedgerAppend(tool string, stored, returned, ms int64)
```

- [ ] **Step 1: 실패 테스트 (핵심 3개)**

```go
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
```

- [ ] **Step 2: 실패 확인** — FAIL (undefined Register/ReadRange)

- [ ] **Step 3: 구현 핵심** (blob 원자 배치 → BEGIN IMMEDIATE 1트랜잭션 → CAS):

```go
func (s *Store) Register(ctx context.Context, reg Registration) (int64, error) {
	sum := sha256.Sum256(reg.StoredBytes)
	contentHash := hex.EncodeToString(sum[:])
	if err := s.writeBlob(contentHash, reg.StoredBytes); err != nil { // DB 커밋 전 배치 (§3.5)
		return 0, err
	}
	var artID int64
	err := s.txRetry(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow("SELECT id FROM artifacts WHERE content_hash=?", contentHash).Scan(&artID); err == sql.ErrNoRows {
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

// txRetry: BEGIN IMMEDIATE 전체 트랜잭션 재시도 ≤3 (지수 백오프), busy면 ErrUnavailable (§3.5)
// writeBlob: artifacts/<h[:2]>/<h> — 임시파일 → fsync → rename; 이미 있으면 no-op
// ReadRange: chunk→저장 좌표, line→blob 라인 스캔, byte→UTF-8 경계 스냅(utf8.RuneStart 후진)
```

(txRetry/writeBlob/ReadRange/snapUTF8/LedgerAppend 전체 구현 포함 — 각 ≤30줄, 테스트가 계약.)

- [ ] **Step 4: 통과 확인** — `go test ./internal/store/ -v -count=1` → PASS 전부. fuzz 시드 `FuzzSnapUTF8` 추가(임의 바이트+범위 → panic 없음·항상 유효 UTF-8) 5s 실행.
- [ ] **Step 5: Commit** — `"feat: store Register 단일 트랜잭션+CAS+blob 원자 배치, ReadRange UTF-8 스냅 (설계 §3.5, §4.2)"`

---

### Task 5: ingest — redaction 스캐너 + denylist

**Files:**
- Create: `internal/ingest/ingest.go` (redact 함수·패턴 테이블 포함 — D13: 같은 파일)
- Test: `internal/ingest/ingest_test.go`

**Interfaces:**
- Produces: `func Redact(b []byte) (out []byte, spans int)`, `func DeniedFilename(path string) bool`, `var ErrWorkspace, ErrUnsupported error`

- [ ] **Step 1: 실패 테스트** — canary 테이블 (설계 §5.1 패턴 전수):

```go
func TestRedact_Canaries(t *testing.T) {
	tests := []struct{ name, in, mustGone string }{
		{"aws", "key=AKIAIOSFODNN7EXAMPLE ok", "AKIAIOSFODNN7EXAMPLE"},
		{"github", "token ghp_abcdefghijklmnopqrstuvwxyz012345 x", "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{"privkey-multiline", "a\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\nxyz\n-----END RSA PRIVATE KEY-----\nb", "MIIE"},
		{"authorization", "Authorization: Bearer eyJhbGciOi.something.sig", "eyJhbGciOi"},
		{"cookie", "Set-Cookie: session=SECRETVAL; Path=/", "SECRETVAL"},
		{"docker-auth", `{"auths":{"r.io":{"auth":"dXNlcjpwYXNzd29yZDEyMw=="}}}`, "dXNlcjpwYXNzd29yZDEyMw"},
		{"json-escaped", `{"t":"ghp_abcdefghijklmnopqrstuvwxyz012345"}`, "abcdefghijklmnopqrstuvwxyz012345"},
		{"password-kv", "password=hunter2xx;db=x", "hunter2xx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, spans := Redact([]byte(tt.in))
			if strings.Contains(string(out), tt.mustGone) {
				t.Fatalf("누출: %q 가 남음\n%s", tt.mustGone, out)
			}
			if spans == 0 {
				t.Fatal("spans=0")
			}
			if !strings.Contains(string(out), "«REDACTED:") {
				t.Fatalf("마커 없음: %s", out)
			}
		})
	}
}

func TestDeniedFilename(t *testing.T) {
	for _, p := range []string{".env", ".env.local", "id_rsa", "cert.pem", "x.har", "kubeconfig", "a/.docker/config.json", "k.jks", "s.p8", "cred.tfstate"} {
		if !DeniedFilename(p) {
			t.Fatalf("허용됨: %s", p)
		}
	}
	for _, p := range []string{"build.log", "main.go", "config.json"} { // 일반 config.json은 허용
		if DeniedFilename(p) {
			t.Fatalf("차단됨: %s", p)
		}
	}
}
```

- [ ] **Step 2: 실패 확인** → FAIL
- [ ] **Step 3: 구현** — 패턴 테이블(정규식 컴파일은 패키지 불변 var — RE2라 ReDoS 없음), 2뷰 스캔(raw + `\uXXXX` 디코드 뷰 — 디코드 뷰 매치 시 raw 대응 범위 치환), PRIVATE KEY 블록은 `(?s)` 블록 정규식, 치환 토큰 `«REDACTED:aws»` 형식. `DeniedFilename`은 base name glob + `.docker/config.json` 경로 접미 매치.
- [ ] **Step 4: 통과 확인** + `FuzzRedact`(panic 없음·출력에 원문 canary 부재 불변식은 시드로) 5s.
- [ ] **Step 5: Commit** — `"feat: ingest redaction 스캐너(2뷰)+denylist (설계 §5.1)"`

---

### Task 6: ingest — 청킹 + 파이프라인 + 경로 정책

**Files:**
- Modify: `internal/ingest/ingest.go`
- Test: `internal/ingest/ingest_test.go` (추가), `internal/ingest/testdata/golden/`

**Interfaces:**
- Consumes: `store.Register`, `ident.Fold`
- Produces:
```go
type Request struct{ Path, Content, Title string; Include, Exclude []string; MaxFileBytes int64 }
type Report struct{ Indexed int; BytesStored int64; Skipped []SkipEntry }
type SkipEntry struct{ Path, Reason string }
func Run(ctx context.Context, st *store.Store, projectRoot string, allowPaths []string, req Request) (Report, error)
func ChunkText(text string, isMarkdown bool) []store.Chunk   // ~4KB 라인 블록, md 헤딩 우선, 1라인 오버랩 (§3.4)
```

- [ ] **Step 1: 실패 테스트** — ① `ChunkText` golden(testdata/golden/plain.txt·doc.md → 기대 청크 경계 JSON), ② 파이프라인: 임시 프로젝트에 파일 3개(일반/denylist/1MB 초과) → Report{Indexed:1, Skipped:2}, 저장본에 redaction 반영·`src_hash`가 원본 해시(≠content_hash, 비밀 포함 파일로 검증), ③ 경로 탈출: projectRoot 밖 절대경로·`..`·(unix) 외부로 향하는 심링크 → `ErrWorkspace`, store-root 하위 allow-path → 거부.
- [ ] **Step 2: 실패 확인** → FAIL
- [ ] **Step 3: 구현** — 워크: `filepath.WalkDir` + `min(GOMAXPROCS,4)` 풀(읽기·해시·redact·chunk 병렬, `store.Register` 직렬 호출 — 규약 §7). 경계 판정: `filepath.Rel(rootFold, ident.Fold(realpath))` 결과가 `..` 시작이면 위반. 파일마다 `EvalSymlinks` 재검증. 바이너리 sniff: 첫 8KB에 NUL. §3.0 순서 준수: 원본읽기→src_hash→Redact→stored→content_hash(Register 내부)→ChunkText.
- [ ] **Step 4: 통과 확인** — golden 포함 전부 PASS. `FuzzChunkText`(불변식: 청크 재결합 시 원문 포함·좌표 단조 증가) 5s.
- [ ] **Step 5: Commit** — `"feat: ingest 파이프라인·청킹·경로 정책 (설계 §3.0, §4.4)"`

---

### Task 7: search — FTS 질의 + RRF + 예산

**Files:**
- Create: `internal/search/search.go`
- Test: `internal/search/search_test.go`

**Interfaces:**
- Consumes: `store.Store`의 reader(`func (s *Store) Reader() *sql.DB` — store에 1줄 추가), sources 테이블
- Produces:
```go
type Hit struct{ ArtifactID, ChunkID int64; Source string; LineStart, LineEnd int; Snippet string; Score float64; Stale, Redacted, SourceCoordsExact bool }
type QueryResult struct{ Query string; Hits []Hit; Truncated bool }
func Query(ctx context.Context, st *store.Store, queries []string, limit, budgetBytes int) ([]QueryResult, error)
```

- [ ] **Step 1: 실패 테스트** — 시드 코퍼스(ingest로 5개 문서 색인) 후: ① porter("caching"→"cached" 문서 매치), ② trigram("useEff"→"useEffect" 매치), ③ RRF: 두 인덱스 동시 매치 문서가 단독 매치보다 상위, ④ 예산: budget=600·2쿼리 → 쿼리당 ~300B 분할, 초과 쿼리만 `Truncated=true`, ⑤ stale: 색인 후 원본 파일 수정 → `Stale=true`, ⑥ snippet ≤ ~500B·매치어 포함.
- [ ] **Step 2: 실패 확인** → FAIL
- [ ] **Step 3: 구현** — 쿼리당: `SELECT rowid, bm25(fts_porter) ...  WHERE fts_porter MATCH ?` + trigram 동형 → RRF(k=60) 병합 → chunk/artifact/source JOIN → stale 판정(src_size/mtime 비교, 불일치 시 재해시 대조 §3.6) → snippet 창(매치 오프셋 중심 ±250B, 단어 경계 스냅) → 예산 분할(균등+이월).
- [ ] **Step 4: 통과 확인** — PASS ×6.
- [ ] **Step 5: Commit** — `"feat: search FTS 병행+RRF+예산 분할+stale (설계 §4.1, §3.6)"`

---

### Task 8: mcp — 서버 조립 + 도구 3종 등록

**Files:**
- Create: `internal/mcp/mcp.go`
- Modify: `cmd/context-router/main.go` (run()에서 mcp.Serve 호출)
- Test: `internal/mcp/mcp_test.go`

**Interfaces:**
- Consumes: Task 2~7 전부.
- Produces: `func Serve(ctx context.Context, cfg Config) error`, `type Config struct{ Canon ident.Canon; Store *store.Store; Profile, Enable, AllowPaths []string; Stdin io.Reader; Stdout io.Writer }`, 내부 `toToolError(error) (code string)` — 규약 §6 매핑표.

- [ ] **Step 0: SDK API 확인** — Run: `go doc github.com/modelcontextprotocol/go-sdk/mcp | head -80` 및 `go doc github.com/modelcontextprotocol/go-sdk/mcp Server`. 아래 코드의 생성자/AddTool 시그니처가 다르면 **테스트를 계약으로 유지한 채** 실제 API에 맞춰 조정한다.

- [ ] **Step 1: 실패 테스트** — ① `toToolError` 테이블(각 sentinel → 코드 9종 매핑, 미지정 → INTERNAL), ② 프로필 게이팅: Enable에 ingest 없으면 등록 도구 = {ctr_search, ctr_fetch}(transform은 계획 2 — 이 계획에서는 미등록이 정상), 있으면 +ctr_index, ③ stdio round-trip: sdk의 in-memory/pipe transport로 `tools/list` 호출 → 이름·`readOnlyHint` 검증, `ctr_search` 호출 → untrusted:true 포함 응답, ④ stdout 순수성: Serve 중 stdout에 비 JSON-RPC 바이트 0 (배너는 stderr).
- [ ] **Step 2: 실패 확인** → FAIL
- [ ] **Step 3: 구현** — 도구별 등록 함수(`registerSearch(srv, st)`, `registerFetch`, `registerIndex`) + 입력 struct(jsonschema 태그, §4.1·§4.2·§4.4 파라미터 그대로: search{queries,limit,max_return_bytes}, fetch{artifact_id, chunk_id|line_start..|byte_start.., max_return_bytes — selector 정확히 1개 검증}, index{path|content+title,include,exclude,max_file_bytes}). 핸들러 = decode→검증→구체 호출 ≤2→encode(≤50줄, 규약 §10). 응답에 `untrusted`, `source_coords_exact`, fetch에는 `exact_scope:"artifact"`·`representation`·실반환 범위. LedgerAppend 호출. main.go run(): `ident.Canonicalize` → store 경로(`storeRoot/projects/<ProjectID>`) → `store.Open` → `mcp.Serve`.
- [ ] **Step 4: 통과 확인** — `go test ./... -count=1` 전체 green + `go build ./...`.
- [ ] **Step 5: Commit** — `"feat: mcp 서버 조립+ctr_search/fetch/index 등록, toToolError 단일 변환 (설계 §4, 규약 §6)"`

---

### Task 9: 통합 스모크 — E2E + 기초 다중 프로세스

**Files:**
- Test: `cmd/context-router/main_test.go` (추가)

**Interfaces:** Consumes: 전부 (바이너리 수준).

- [ ] **Step 1: 실패 테스트** — ① E2E: `go build`한 실바이너리를 `--enable ingest --store-root <tmp>`로 exec, stdin으로 JSON-RPC(`initialize`→`tools/list`→`ctr_index`(임시 프로젝트 파일)→`ctr_search`→`ctr_fetch`) 순차 전송 → 각 응답 검증, stderr에 배너 존재, stdout에 프로토콜 외 바이트 0. ② 기초 다중 프로세스: 같은 store-root로 프로세스 2개가 서로 다른 파일을 동시 `ctr_index` → 둘 다 성공, `PRAGMA integrity_check`=ok, sources=2. (심층 CAS 교차·kill 내구성은 계획 3 게이트 하네스.)
- [ ] **Step 2: 실패 확인** → FAIL (E2E 배선 미완이면 여기서 드러남)
- [ ] **Step 3: 수정** — 드러난 배선 결함만 수정(신규 기능 금지).
- [ ] **Step 4: 통과 확인** — `go test ./... -count=1 -p 1` 전체 green, 3 OS 중 최소 Windows(현 머신)에서 실행 확인 기록.
- [ ] **Step 5: Commit** — `"test: E2E stdio 스모크 + 기초 다중 프로세스 검증 (설계 §12-7,10 기초)"`

---

## Self-Review 기록

- 범위: 설계서 §2.2(시작 시퀀스)·§3 전체·§4.1/4.2/4.4·§5.1·§4.0(오류/annotation) → Task 1~9로 커버. §4.3/§4.5/§4.6/§7(CLI)은 계획 2·3 범위(의도적).
- 타입 일관성: `store.Chunk`/`Registration`/`Selector`/`search.Hit` 시그니처를 Interfaces 블록 간 대조 완료.
- 플레이스홀더: Task 4 Step 3의 축약(txRetry 등)은 "테스트가 계약" 원칙 하의 구현 지시로, 계약(시그니처·동작)은 전부 명시됨.
