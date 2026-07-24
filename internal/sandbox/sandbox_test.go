package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewScratchCreatesPrivateDir(t *testing.T) {
	parent := t.TempDir()
	dir, err := NewScratch(parent)
	if err != nil {
		t.Fatalf("NewScratch: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("스크래치 미생성: %v", err)
	}
	if filepath.Dir(dir) != parent {
		t.Fatalf("부모 불일치: %s", dir)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("owner-only 아님: %v", fi.Mode().Perm())
	}
}

func TestSweepStaleRemovesOldKeepsFresh(t *testing.T) {
	parent := t.TempDir()
	old, _ := NewScratch(parent)
	fresh, _ := NewScratch(parent)
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	SweepStale(parent, 24*time.Hour)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("스테일 미제거")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("신선분 오삭제: %v", err)
	}
}

func TestBaseEnvClosedTable(t *testing.T) {
	t.Setenv("CTR_TEST_EXCLUDED_VAR", "marker")
	t.Setenv("PATH", os.Getenv("PATH")) // 존재 보장
	for _, kv := range BaseEnv() {
		if strings.HasPrefix(kv, "CTR_TEST_EXCLUDED_VAR=") {
			t.Fatalf("닫힌 표 위반: 표 밖 변수 통과")
		}
	}
	found := false
	for _, kv := range BaseEnv() {
		if strings.HasPrefix(kv, "PATH=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("PATH 누락")
	}
}
