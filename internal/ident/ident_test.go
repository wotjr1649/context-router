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

func FuzzFold(f *testing.F) {
	f.Add(`C:\a\..\b`)
	f.Add(`\\?\C:\x`)
	f.Add("//server/share/p")
	f.Fuzz(func(t *testing.T, p string) { _ = Fold(p) }) // 불변식: panic 없음
}
