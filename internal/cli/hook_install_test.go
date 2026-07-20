package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/ident"
)

// firstHookCommand: settings.json의 SessionStart 첫 그룹 첫 명령 문자열을 뽑는다(설치가 쓴
// 훅 명령을 검증하는 헬퍼).
func firstHookCommand(t *testing.T, projectRoot string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectRoot, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse settings: %v\n%s", err, data)
	}
	g := s.Hooks["SessionStart"]
	if len(g) == 0 || len(g[0].Hooks) == 0 {
		t.Fatalf("no SessionStart hook command: %s", data)
	}
	return g[0].Hooks[0].Command
}

// assertDoctorAscending: doctor 본문의 [N] 항목 번호가 오름차순이고 [13]까지 도달하는지 확인.
func assertDoctorAscending(t *testing.T, out string) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\[(\d+)\]`)
	prev := 0
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(m[1])
		if n < prev {
			t.Fatalf("doctor 항목 순서가 오름차순이 아님: [%d] after [%d]\n%s", n, prev, out)
		}
		prev = n
	}
	if prev < 13 {
		t.Fatalf("doctor는 [13]까지 출력해야 함, 최대 [%d]\n%s", prev, out)
	}
}

// ① 빈 설정에 install → 4개 이벤트 등록·유효 JSON·PreToolUse matcher "Read"·timeout 10.
func TestHookInstall_EmptyRegistersFourItems(t *testing.T) {
	projectRoot := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall(nil, "/store", "", false, projectRoot, "0.1.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("invalid JSON:\n%s", data)
	}
	n, err := countRegisteredHooks(path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Fatalf("registered=%d want 4\n%s", n, data)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, ev := range []string{"SessionStart", "PreToolUse", "PostToolUse", "PostToolUseFailure"} {
		if len(s.Hooks[ev]) != 1 {
			t.Fatalf("event %q groups=%d want 1: %s", ev, len(s.Hooks[ev]), data)
		}
		h := s.Hooks[ev][0].Hooks
		if len(h) != 1 || h[0].Command != "context-router hook" || h[0].Timeout != 10 {
			t.Fatalf("event %q bad command/timeout: %+v", ev, s.Hooks[ev][0])
		}
	}
	if s.Hooks["PreToolUse"][0].Matcher != "Read" {
		t.Fatalf("PreToolUse matcher=%q want Read", s.Hooks["PreToolUse"][0].Matcher)
	}
}

// ② 재install 멱등 — 항목 1벌만 유지.
func TestHookInstall_Idempotent(t *testing.T) {
	projectRoot := t.TempDir()
	var out bytes.Buffer
	for i := 0; i < 3; i++ {
		if err := runHookInstall(nil, "/store", "", false, projectRoot, "0.1.0", &out); err != nil {
			t.Fatalf("install #%d: %v", i, err)
		}
	}
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	n, err := countRegisteredHooks(path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Fatalf("after 3 installs registered=%d want 4 (멱등 위반)", n)
	}
	data, _ := os.ReadFile(path)
	var s struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	json.Unmarshal(data, &s)
	for ev, groups := range s.Hooks {
		if len(groups) != 1 {
			t.Fatalf("event %q has %d groups want 1 (중복): %s", ev, len(groups), data)
		}
	}
}

// ③ 타 도구 훅 항목 + 미지 키 시드 후 install/uninstall → 원형(데이터) 보존.
func TestHookInstall_RoundTripPreservesUnknownAndOtherTools(t *testing.T) {
	projectRoot := t.TempDir()
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{
  "$schema": "https://example/schema.json",
  "model": "opus",
  "permissions": {"allow": ["Bash"]},
  "hooks": {
    "PostToolUse": [
      {"matcher": "Write", "hooks": [{"type": "command", "command": "other-tool run", "timeout": 5}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "foo-cmd"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	assertPreserved := func(stage string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s read: %v", stage, err)
		}
		var s map[string]json.RawMessage
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatalf("%s invalid JSON: %v\n%s", stage, err, data)
		}
		if string(s["$schema"]) != `"https://example/schema.json"` {
			t.Fatalf("%s: $schema lost: %s", stage, data)
		}
		if string(s["model"]) != `"opus"` {
			t.Fatalf("%s: model lost: %s", stage, data)
		}
		if !strings.Contains(string(s["permissions"]), "Bash") {
			t.Fatalf("%s: permissions lost: %s", stage, data)
		}
		if !strings.Contains(string(data), "other-tool run") {
			t.Fatalf("%s: other tool's PostToolUse entry lost: %s", stage, data)
		}
		if !strings.Contains(string(data), "UserPromptSubmit") || !strings.Contains(string(data), "foo-cmd") {
			t.Fatalf("%s: unrelated hook event lost: %s", stage, data)
		}
	}

	var out bytes.Buffer
	if err := runHookInstall(nil, "/store", "", false, projectRoot, "0.1.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	assertPreserved("after install")
	if n, _ := countRegisteredHooks(path); n != 4 {
		t.Fatalf("after install registered=%d want 4", n)
	}

	if err := runHookUninstall(nil, projectRoot, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	assertPreserved("after uninstall")
	if n, _ := countRegisteredHooks(path); n != 0 {
		t.Fatalf("after uninstall registered=%d want 0", n)
	}
}

// ④ uninstall 대칭(자기 항목만 제거) + 유사 접두사·마커 없는 수동 항목 보존.
// 소유권 마커 AND 명령 토큰 정확 일치 두 조건 결합을 직접 검증한다.
func TestHookInstall_UninstallSymmetricPreservesLookalikes(t *testing.T) {
	projectRoot := t.TempDir()
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := "context-router/0.1.0"
	seed := fmt.Sprintf(`{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Read", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": %q}
    ],
    "PostToolUse": [
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": %q},
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook-wrapper", "timeout": 10}], "__ctrManaged": %q},
      {"matcher": "Write", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}]}
    ]
  }
}`, marker, marker, marker)
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runHookUninstall(nil, projectRoot, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	if n, _ := countRegisteredHooks(path); n != 0 {
		t.Fatalf("after uninstall our-owned registered=%d want 0", n)
	}
	data, _ := os.ReadFile(path)
	var s struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	json.Unmarshal(data, &s)
	if len(s.Hooks["PreToolUse"]) != 0 {
		t.Fatalf("PreToolUse should be emptied (our only entry removed): %s", data)
	}
	post := s.Hooks["PostToolUse"]
	if len(post) != 2 {
		t.Fatalf("PostToolUse groups=%d want 2 (hook-wrapper + manual survive): %s", len(post), data)
	}
	if !strings.Contains(string(data), "context-router hook-wrapper") {
		t.Fatalf("hook-wrapper lookalike wrongly removed (접두사 매칭 금지): %s", data)
	}
	// 마커 없는 수동 context-router hook 항목(Write matcher)이 살아남아야 한다.
	sawManual := false
	for _, g := range post {
		if strings.Contains(string(g), `"Write"`) {
			sawManual = true
		}
	}
	if !sawManual {
		t.Fatalf("marker-less manual hook entry wrongly removed: %s", data)
	}
}

// ⑤ 원자성 — 설치 후 임시 파일(.ctr-settings-*.tmp) 잔존물 부재.
func TestHookInstall_AtomicWriteNoTempLeftover(t *testing.T) {
	projectRoot := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall(nil, "/store", "", false, projectRoot, "0.1.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(projectRoot, ".claude"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ctr-settings-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp leftover found: %q", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "settings.json" {
		t.Fatalf(".claude dir should hold only settings.json, got %v", entries)
	}
}

// ⑦ 훅 명령이 --no-shadow / --store-root(명시 시에만) 플래그를 반영한다.
func TestHookInstall_CommandReflectsFlags(t *testing.T) {
	cases := []struct {
		explicit bool
		raw      string
		noShadow bool
		want     string
	}{
		{false, "", false, "context-router hook"},
		{false, "", true, "context-router hook --no-shadow"},
		{true, "/custom/store", false, "context-router hook --store-root /custom/store"},
		{true, "/custom/store", true, "context-router hook --store-root /custom/store --no-shadow"},
		{false, "/ignored", false, "context-router hook"}, // 미명시 → 무기입
	}
	for _, c := range cases {
		if got := buildHookCommand(c.explicit, c.raw, c.noShadow); got != c.want {
			t.Fatalf("buildHookCommand(%v,%q,%v)=%q want %q", c.explicit, c.raw, c.noShadow, got, c.want)
		}
	}

	t.Run("install_writes_no_shadow_and_explicit_store_root", func(t *testing.T) {
		projectRoot := t.TempDir()
		var out bytes.Buffer
		// storeRoot(canon)와 storeRootRaw(원시)를 다르게 줘서 원시값이 주입되는지 확인.
		if err := runHookInstall([]string{"--no-shadow"}, "/canon/store", "/raw/store", true, projectRoot, "0.1.0", &out); err != nil {
			t.Fatalf("install: %v", err)
		}
		cmd := firstHookCommand(t, projectRoot)
		if !strings.Contains(cmd, "--no-shadow") {
			t.Fatalf("cmd=%q missing --no-shadow", cmd)
		}
		if !strings.Contains(cmd, "--store-root /raw/store") {
			t.Fatalf("cmd=%q must inject the raw store-root value", cmd)
		}
	})

	t.Run("install_omits_store_root_when_not_explicit", func(t *testing.T) {
		projectRoot := t.TempDir()
		var out bytes.Buffer
		if err := runHookInstall(nil, "/canon/store", "", false, projectRoot, "0.1.0", &out); err != nil {
			t.Fatalf("install: %v", err)
		}
		cmd := firstHookCommand(t, projectRoot)
		if strings.Contains(cmd, "--store-root") {
			t.Fatalf("cmd=%q must not inject store-root when not explicit", cmd)
		}
	})
}

// ⑥ doctor: 훅 등록/미등록 상태 문구 + drops 두 위치 합산 + 항목 [1]~[13] 오름차순.
func TestDoctor_HookItemsAndAscendingOrder(t *testing.T) {
	t.Run("unregistered", func(t *testing.T) {
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		var buf bytes.Buffer
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		for _, want := range []string{
			"[9] hooks: 미등록",
			"[10] context-router:",
			"[11] store-root path:",
			"[12] drops:",
			"[13] sidecar writable:",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("out missing %q:\n%s", want, out)
			}
		}
		assertDoctorAscending(t, out)
	})

	t.Run("registered_with_drops", func(t *testing.T) {
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		var iout bytes.Buffer
		if err := runHookInstall(nil, storeRoot, "", false, projectRoot, "0.1.0", &iout); err != nil {
			t.Fatalf("install: %v", err)
		}
		// drops 두 위치: store-root(식별 전) 2줄 + worktree(식별 후) 3줄.
		if err := os.WriteFile(filepath.Join(storeRoot, "session.drops.log"), []byte("1\ta\n2\tb\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		canon, err := ident.Canonicalize(projectRoot)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		wt := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
		if err := os.MkdirAll(wt, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "session.drops.log"), []byte("1\tx\n2\ty\n3\tz\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		if !strings.Contains(out, "[9] hooks: 등록됨") {
			t.Fatalf("out missing registered-hooks line:\n%s", out)
		}
		if !strings.Contains(out, "[12] drops: store-root=2 worktree=3 total=5") {
			t.Fatalf("out missing two-location drops sum:\n%s", out)
		}
		assertDoctorAscending(t, out)
	})
}
