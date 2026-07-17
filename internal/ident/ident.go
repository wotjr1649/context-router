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
