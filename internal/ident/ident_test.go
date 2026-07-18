package ident

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
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

func TestCanonicalize_BrokenWorktreeCommondirFails(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "wt1"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "worktrees", "wt1", "commondir"),
		[]byte("../../this-does-not-exist\n"), 0o644)
	wt := t.TempDir()
	os.WriteFile(filepath.Join(wt, ".git"),
		[]byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt1")+"\n"), 0o644)
	if _, err := Canonicalize(wt); err == nil {
		t.Fatal("손상된 worktree인데 오류가 없음 — 침묵 fallback 금지")
	}
}

func TestCanonicalize_SubmoduleUsesOwnRoot(t *testing.T) {
	super := t.TempDir()
	os.MkdirAll(filepath.Join(super, ".git", "modules", "sub"), 0o755)
	sub := filepath.Join(super, "sub")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".git"),
		[]byte("gitdir: "+filepath.Join(super, ".git", "modules", "sub")+"\n"), 0o644)
	c, err := Canonicalize(sub)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProjectRoot != Fold(mustReal(t, sub)) {
		t.Fatalf("submodule projectRoot=%q want %q", c.ProjectRoot, Fold(mustReal(t, sub)))
	}
}

// TestFindGitProjectRoot_ReadFailureWrapped: α5 — .git 파일 읽기 실패는 raw 전파가 아니라
// "canonicalize:" 로 wrap되어야 한다(§5.5류 오류 일관성). unix perm으로만 결정적 재현 가능.
func TestFindGitProjectRoot_ReadFailureWrapped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perm bit로만 결정적 재현 가능")
	}
	repo := t.TempDir()
	gitFile := filepath.Join(repo, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: somewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitFile, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(gitFile, 0o644) // t.TempDir 정리 허용
	_, err := Canonicalize(repo)
	if err == nil {
		t.Fatal("want error(.git 읽기 실패), got nil")
	}
	if !strings.HasPrefix(err.Error(), "canonicalize:") {
		t.Fatalf("want wrapped with canonicalize: prefix, got %v", err)
	}
}

func TestFold_ExtendedUNC(t *testing.T) {
	a := Fold(`\\?\UNC\Server\Share\p`)
	b := Fold(`\\Server\Share\p`)
	if a != b {
		t.Fatalf("동일 경로 다른 fold: %q vs %q", a, b)
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
	f.Add(`\\?\UNC\server\share\x`)
	f.Fuzz(func(t *testing.T, p string) { _ = Fold(p) }) // 불변식: panic 없음
}
