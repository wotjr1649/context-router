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

func TestSweepStaleSkipsNonScratchEntries(t *testing.T) {
	parent := t.TempDir()
	keep := filepath.Join(parent, "keep-me") // run- 접두 아님 → 스윕 밖
	if err := os.Mkdir(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(keep, past, past); err != nil {
		t.Fatal(err)
	}
	SweepStale(parent, 24*time.Hour)
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("비-스크래치 항목 오삭제: %v", err)
	}
}

func TestBaseEnvNeverNil(t *testing.T) {
	// 닫힌 표의 모든 키를 지워 빈 케이스를 강제(t.Setenv가 원값 원복 등록 후 Unsetenv).
	for _, k := range baseKeys() {
		t.Setenv(k, "x")
		os.Unsetenv(k)
	}
	got := BaseEnv()
	if got == nil {
		t.Fatalf("빈 케이스에서 nil — 부모 환경 상속 위험")
	}
	if len(got) != 0 {
		t.Fatalf("빈 케이스인데 비어있지 않음: %v", got)
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
