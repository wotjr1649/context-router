package ident

import (
	"os"
	"os/exec"
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

// TestFold_ExtendedUNC / _SlashVariant: 확장 UNC 경로 문법(`\\?\...`, `//?/...`)은
// windows 전용 개념이다 — unix(darwin 포함)에서는 `\`가 구분자가 아니라 합법 파일명
// 바이트라 이 두 픽스처를 "동일 경로"로 접을 근거가 없다(Codex 교차리뷰 P1-1).
func TestFold_ExtendedUNC(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("UNC 확장경로 문법은 windows 전용 — unix에서는 백슬래시가 구분자가 아니다")
	}
	a := Fold(`\\?\UNC\Server\Share\p`)
	b := Fold(`\\Server\Share\p`)
	if a != b {
		t.Fatalf("동일 경로 다른 fold: %q vs %q", a, b)
	}
}

// TestFold_ExtendedUNC_SlashVariant: 계획2 Task2 이월 — 확장 UNC 접두의 슬래시 변형
// (`//?/UNC/...`)도 백슬래시 변형(`\\?\UNC\...`)과 동일하게 fold되어야 한다(windows 전용).
func TestFold_ExtendedUNC_SlashVariant(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("UNC 확장경로 문법은 windows 전용 — unix에서는 백슬래시가 구분자가 아니다")
	}
	a := Fold(`//?/UNC/Server/Share/p`)
	b := Fold(`\\Server\Share\p`)
	if a != b {
		t.Fatalf("동일 경로 다른 fold: %q vs %q", a, b)
	}
}

// TestFold_OSRule: 구분자 통일(windows만)과 case-fold(windows·darwin)는 서로 다른
// 게이트다 — darwin은 unix 계열이라 `\`를 구분자로 바꾸지 않지만 대소문자는 접는다.
func TestFold_OSRule(t *testing.T) {
	got := Fold(`C:\Some\Dir`)
	switch runtime.GOOS {
	case "windows":
		if got != "c:/some/dir" {
			t.Fatalf("fold=%q", got)
		}
	case "darwin":
		if got != `c:\some\dir` {
			t.Fatalf("darwin fold=%q want case-fold만(슬래시 미변환)", got)
		}
	default: // linux 등 — case-sensitive, 슬래시도 구분자 아님(백슬래시는 합법 파일명 바이트)
		if got != `C:\Some\Dir` {
			t.Fatalf("linux fold=%q want 원본 그대로", got)
		}
	}
}

// TestCanonicalize_JunctionResolvesToRealTarget: 게이트3 심층(P2-1a, Codex 교차리뷰,
// windows 전용) — NTFS junction(mklink /J)을 경유한 경로도 실제 대상 디렉터리로
// canonicalize돼야 경계 판정(allow-path/store-root 등, §4.4)이 junction 우회 없이
// 실경로 기준으로 일관된다.
func TestCanonicalize_JunctionResolvesToRealTarget(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("NTFS junction은 windows 전용")
	}
	target := t.TempDir()
	junction := filepath.Join(t.TempDir(), "junc")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}

	viaTarget, err := Canonicalize(target)
	if err != nil {
		t.Fatalf("canonicalize target: %v", err)
	}
	viaJunction, err := Canonicalize(junction)
	if err != nil {
		t.Fatalf("canonicalize junction: %v", err)
	}
	if viaJunction.WorktreeRoot != viaTarget.WorktreeRoot {
		t.Fatalf("junction WorktreeRoot=%q want %q(실경로 미해석 — 우회 가능)", viaJunction.WorktreeRoot, viaTarget.WorktreeRoot)
	}
	if viaJunction.WorktreeID != viaTarget.WorktreeID {
		t.Fatalf("junction WorktreeID=%q want %q", viaJunction.WorktreeID, viaTarget.WorktreeID)
	}
}

// TestCanonicalize_CaseOnlyDirsFoldToSameIdentity: 게이트3 심층(P2-1b, Codex 교차리뷰,
// windows 전용) — Windows 기본 볼륨(대소문자 비구분)에서는 실제로 존재하는 디렉터리 하나를
// 가리키는, 대소문자만 다른 두 경로 문자열이 canonicalize에서 동일한 WorktreeRoot/
// WorktreeID로 귀결돼야 한다(§4.4 경계 판정 일관 — "실제 대소문자만 다른 두 디렉터리"를
// per-directory case-sensitivity로 만드는 케이스는 fsutil.exe에 admin 권한이 필요해 CI 러너
// 권한 제약상 보류, docs/gates-v0.0.1-ko.md 게이트 3 참조).
func TestCanonicalize_CaseOnlyDirsFoldToSameIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 기본 볼륨의 대소문자 비구분 폴딩 전용")
	}
	parent := t.TempDir()
	mixedCase := filepath.Join(parent, "FooBar")
	if err := os.Mkdir(mixedCase, 0o755); err != nil {
		t.Fatal(err)
	}

	want, err := Canonicalize(mixedCase)
	if err != nil {
		t.Fatalf("canonicalize FooBar: %v", err)
	}
	for _, variant := range []string{filepath.Join(parent, "FOOBAR"), filepath.Join(parent, "foobar")} {
		got, err := Canonicalize(variant)
		if err != nil {
			t.Fatalf("canonicalize %s: %v", variant, err)
		}
		if got.WorktreeRoot != want.WorktreeRoot || got.WorktreeID != want.WorktreeID {
			t.Fatalf("%s canon=%+v want %+v(동일 실체가 다른 identity로 갈림)", variant, got, want)
		}
	}
}

// TestFold_PreservesBackslashInUnixFilename: Codex 교차리뷰 P1-1 — unix에서 `\`는
// 합법 파일명 바이트다. 전역 치환을 windows 전용으로 되돌린 회귀 가드: 실제 파일명에
// 백슬래시가 섞여 있어도(예: "work\root") 구분자로 오인돼 경계 판정을 깨서는 안 된다.
func TestFold_PreservesBackslashInUnixFilename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows는 `\\`가 실제 구분자 — TestFold_OSRule/ExtendedUNC가 이미 커버")
	}
	p := `/tmp/work\root/file.txt`
	want := p
	if runtime.GOOS == "darwin" {
		want = strings.ToLower(p) // case-fold만 적용, 백슬래시는 그대로
	}
	if got := Fold(p); got != want {
		t.Fatalf("Fold(%q)=%q want %q (백슬래시가 구분자로 오인돼 변형됨)", p, got, want)
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
