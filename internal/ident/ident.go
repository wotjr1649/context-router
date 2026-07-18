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
	p = strings.TrimPrefix(p, `\\?\`)
	if strings.HasPrefix(p, `UNC\`) || strings.HasPrefix(p, `UNC/`) {
		p = `\\` + p[4:] // \\?\UNC\server\share → \\server\share
	}
	p = filepath.ToSlash(p)
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
	pr, ok, err := findGitProjectRoot(real)
	if err != nil {
		return Canon{}, err
	}
	if ok {
		project = Fold(pr)
	}
	return Canon{
		ProjectRoot: project, WorktreeRoot: worktree,
		ProjectID: id(project), WorktreeID: id(worktree),
	}, nil
}

// findGitProjectRoot: 상향 탐색. .git 디렉터리 → 부모. .git 파일 → gitdir: 파싱.
// commondir 파일이 있으면 그 target을 해석해 주 .git의 부모를 반환하고, 해석 실패는
// 오류로 전파한다(침묵 fallback 금지). commondir 파일이 없으면(submodule) .git 파일이
// 있는 그 디렉터리 자체를 프로젝트 루트로 본다. git 바이너리 미호출.
func findGitProjectRoot(start string) (string, bool, error) {
	for dir := start; ; {
		g := filepath.Join(dir, ".git")
		if fi, err := os.Stat(g); err == nil {
			if fi.IsDir() {
				return dir, true, nil
			}
			b, err := os.ReadFile(g)
			if err != nil {
				return "", false, fmt.Errorf("canonicalize: %w", err)
			}
			gd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
			if !filepath.IsAbs(gd) {
				gd = filepath.Join(dir, gd)
			}
			cb, err := os.ReadFile(filepath.Join(gd, "commondir"))
			if err != nil {
				return dir, true, nil // submodule: .git 파일이 있는 그 디렉터리가 프로젝트
			}
			common := strings.TrimSpace(string(cb))
			if !filepath.IsAbs(common) {
				common = filepath.Join(gd, common)
			}
			r, err := filepath.EvalSymlinks(common)
			if err != nil {
				return "", false, fmt.Errorf("canonicalize: 손상된 worktree(commondir 해석 실패): %w", err)
			}
			return filepath.Dir(r), true, nil // 주 .git의 부모
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
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
