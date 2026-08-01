package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/store"
)

// TestMain — 이 패키지 테스트 전역 격리: doctor([9] 사용자 범위 검사)·`hook install --user`가
// 실사용자 ~/.claude를 절대 건드리지 않도록 홈을 프로세스 임시 디렉터리로 돌린다(os.UserHomeDir
// 이음새 = Windows USERPROFILE / 그 외 HOME). 사용자 범위에 실제로 기록하는 테스트는 t.Setenv로
// 자기 전용 임시 홈을 덮어써(격리·자동 복원) 이 기본 홈을 오염시키지 않는다.
// CODEX_HOME도 함께 중화한다 — codexConfigPath/codexHooksPath는 CODEX_HOME을 홈보다 우선하므로,
// 상속된 CODEX_HOME이 있으면 install/uninstall --codex e2e가 홈 격리를 우회해 실사용자
// config.toml을 변조·삭제할 수 있다(빈 값 = 미설정 → 임시 홈 폴백; 필요한 테스트는 t.Setenv로 재설정).
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "ctr-cli-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: 임시 홈 생성 실패:", err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
	_ = os.Setenv("CODEX_HOME", "")
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

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

// ① 빈 설정에 install → 6개 이벤트 등록·유효 JSON·PreToolUse matcher "Read|Bash|PowerShell|Grep"·timeout 10.
func TestHookInstall_EmptyRegistersSixItems(t *testing.T) {
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
	if n != 6 {
		t.Fatalf("registered=%d want 6\n%s", n, data)
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
	for _, ev := range []string{"SessionStart", "PreToolUse", "PostToolUse", "PostToolUseFailure", "SubagentStart", "SubagentStop"} {
		if len(s.Hooks[ev]) != 1 {
			t.Fatalf("event %q groups=%d want 1: %s", ev, len(s.Hooks[ev]), data)
		}
		h := s.Hooks[ev][0].Hooks
		if len(h) != 1 || h[0].Command != "context-router hook" || h[0].Timeout != 10 {
			t.Fatalf("event %q bad command/timeout: %+v", ev, s.Hooks[ev][0])
		}
	}
	if s.Hooks["PreToolUse"][0].Matcher != "Read|Bash|PowerShell|Grep" {
		t.Fatalf("PreToolUse matcher=%q want Read|Bash|PowerShell|Grep", s.Hooks["PreToolUse"][0].Matcher)
	}
}

// ①-b D32 업그레이드 재설치(설계 §8 설치 게이트): v0.2 형태 settings(marker 0.2.0 + PreToolUse
// matcher "Read")를 seed → install 재실행 → PreToolUse 관리 그룹 1개·matcher "Read|Bash|PowerShell|Grep"·총 6그룹·
// marker 무버전 표식(hookGroupMarker)으로 갱신(구 matcher 그룹이 잔존하지 않고 대칭 교체된다 —
// D82 이후 훅 등록물의 마커는 install에 넘긴 version 인자와 무관하다).
func TestHookInstall_UpgradeReinstallWidensMatcher(t *testing.T) {
	projectRoot := t.TempDir()
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// v0.2가 기입했던 형태 — PreToolUse matcher는 "Read"(단일 그룹), 마커는 구버전.
	seed := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": "context-router/0.2.0"}
    ],
    "PreToolUse": [
      {"matcher": "Read", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": "context-router/0.2.0"}
    ],
    "PostToolUse": [
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": "context-router/0.2.0"}
    ],
    "PostToolUseFailure": [
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": "context-router/0.2.0"}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runHookInstall(nil, "/store", "", false, projectRoot, "0.3.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}

	if n, _ := countRegisteredHooks(path); n != 6 {
		t.Fatalf("after upgrade registered=%d want 6 (관리 그룹 중복/누락)", n)
	}
	data, _ := os.ReadFile(path)
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Managed string `json:"__ctrManaged"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse: %v\n%s", err, data)
	}
	pre := s.Hooks["PreToolUse"]
	if len(pre) != 1 {
		t.Fatalf("PreToolUse groups=%d want 1(단일 관리 그룹 유지): %s", len(pre), data)
	}
	if pre[0].Matcher != "Read|Bash|PowerShell|Grep" {
		t.Fatalf("PreToolUse matcher=%q want Read|Bash|PowerShell|Grep (구 Read 그룹 미교체): %s", pre[0].Matcher, data)
	}
	if pre[0].Managed != hookGroupMarker {
		t.Fatalf("marker=%q want %s (구버전 마커 미교체, D82): %s", pre[0].Managed, hookGroupMarker, data)
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
	if n != 6 {
		t.Fatalf("after 3 installs registered=%d want 6 (멱등 위반)", n)
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
	if n, _ := countRegisteredHooks(path); n != 6 {
		t.Fatalf("after install registered=%d want 6", n)
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

// ⑥-b JSON `null`·`{"hooks":null}` 설정(구문상 유효)에서 install이 패닉 없이 병합한다
// (최종 리뷰 C5 — Unmarshal이 null을 nil 맵으로 설정해 할당 시 패닉하던 경로).
func TestMergeHookSettings_NullTolerant(t *testing.T) {
	for _, existing := range []string{"null", `{"hooks":null}`} {
		out, err := mergeHookSettings([]byte(existing), "context-router hook", "context-router/0.2.0", true)
		if err != nil {
			t.Fatalf("existing=%q merge err: %v", existing, err)
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(out, &settings); err != nil {
			t.Fatalf("existing=%q 출력이 유효 JSON 아님: %v", existing, err)
		}
		if _, ok := settings["hooks"]; !ok {
			t.Fatalf("existing=%q 병합 결과에 hooks 없음: %s", existing, out)
		}
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
		{true, "/custom/store", false, "context-router hook --store-root '/custom/store'"},
		{true, "/custom/store", true, "context-router hook --store-root '/custom/store' --no-shadow"},
		{false, "/ignored", false, "context-router hook"}, // 미명시 → 무기입
		// T11 실측: 훅 명령은 sh 파싱 — 백슬래시·공백 경로는 홑따옴표 인용으로만 무변형 전달.
		{true, `C:\tmp\ctr t11 store`, false, `context-router hook --store-root 'C:\tmp\ctr t11 store'`},
		{true, "/it's/store", false, `context-router hook --store-root '/it'\''s/store'`},
	}
	for _, c := range cases {
		if got := buildHookCommand(c.explicit, c.raw, c.noShadow); got != c.want {
			t.Fatalf("buildHookCommand(%v,%q,%v)=%q want %q", c.explicit, c.raw, c.noShadow, got, c.want)
		}
	}

	t.Run("install_writes_no_shadow_and_explicit_store_root", func(t *testing.T) {
		projectRoot := t.TempDir()
		var out bytes.Buffer
		// storeRoot(canon)와 storeRootRaw(원시)를 다르게 줘서 명시 store-root가 주입되는지 확인.
		if err := runHookInstall([]string{"--no-shadow"}, "/canon/store", "/raw/store", true, projectRoot, "0.1.0", &out); err != nil {
			t.Fatalf("install: %v", err)
		}
		cmd := firstHookCommand(t, projectRoot)
		if !strings.Contains(cmd, "--no-shadow") {
			t.Fatalf("cmd=%q missing --no-shadow", cmd)
		}
		absRaw, err := filepath.Abs("/raw/store") // F3: install이 절대화해 주입
		if err != nil {
			t.Fatalf("abs: %v", err)
		}
		if !strings.Contains(cmd, "--store-root '"+absRaw+"'") {
			t.Fatalf("cmd=%q must inject the absolutized store-root %q (single-quoted)", cmd, absRaw)
		}
	})

	// F3: 상대 --store-root는 절대화돼 settings에 기입돼야 한다(원시 상대경로가 박히면 프로젝트별
	// cwd로 store가 파편화). settings JSON을 파싱해 훅 명령을 뽑아 확인한다(TempDir 격리).
	t.Run("install_absolutizes_relative_store_root", func(t *testing.T) {
		projectRoot := t.TempDir()
		var out bytes.Buffer
		const rel = "./cache"
		if err := runHookInstall(nil, "/canon/store", rel, true, projectRoot, "0.1.0", &out); err != nil {
			t.Fatalf("install: %v", err)
		}
		cmd := firstHookCommand(t, projectRoot)
		abs, err := filepath.Abs(rel)
		if err != nil {
			t.Fatalf("abs: %v", err)
		}
		if !strings.Contains(cmd, "--store-root '"+abs+"'") {
			t.Fatalf("cmd=%q must contain absolutized store-root %q (single-quoted)", cmd, abs)
		}
		if strings.Contains(cmd, "--store-root '"+rel+"'") {
			t.Fatalf("cmd=%q must NOT contain the raw relative path %q", cmd, rel)
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
		isolateCodexHome(t)
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		var buf bytes.Buffer
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0", false); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		for _, want := range []string{
			"[9] hooks: project=미등록",
			"[10] context-router:",
			"[11] store-root path:",
			"[12] drops:",
			"[13] sidecar writable:",
			"[14] content.db:", // store 없는 unregistered 경로 — fail-soft "없음" 라인으로 방출
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("out missing %q:\n%s", want, out)
			}
		}
		assertDoctorAscending(t, out)
	})

	t.Run("registered_with_drops", func(t *testing.T) {
		isolateCodexHome(t)
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
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0", false); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		if !strings.Contains(out, "[9] hooks: project=등록됨") {
			t.Fatalf("out missing registered-hooks line:\n%s", out)
		}
		if !strings.Contains(out, "[12] drops: store-root=2(a=1@1970-01-01,b=1@1970-01-01) worktree=3(x=1@1970-01-01,y=1@1970-01-01,z=1@1970-01-01) total=5") {
			t.Fatalf("out missing reason-rollup drops line:\n%s", out)
		}
		assertDoctorAscending(t, out)
	})
}

// ⑦ dropsByReason 엄격 파싱: appendDrop 계약("<unix초>\t<사유>") 비준수 줄은 전부 unparsed로 세되
// total은 빈 줄 포함 모든 줄을 센다(줄 수 계약). 빈 줄·탭 없는 줄·비숫자 ts·3필드(탭 초과) 커버 —
// 비준수 줄이 자기 사유(bad-input·foo\tbar)로 새지 않음을 len==2로 확인(사유 TAB 혼입 회귀 방지).
func TestDropsByReason_StrictParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.drops.log")
	// 유효 1줄 + 빈 줄 + 탭 없는 줄 + 비숫자 ts + 3필드(탭 혼입) = 5줄.
	body := "1\ta\n\nnofield\ngarbage\tbad-input\n123\tfoo\tbar\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	total, reasons, _ := dropsByReason(path)
	if total != 5 {
		t.Fatalf("total=%d want 5(빈 줄 포함 모든 줄)", total)
	}
	if reasons["a"] != 1 {
		t.Fatalf("reasons[a]=%d want 1(유효 줄)", reasons["a"])
	}
	if reasons["unparsed"] != 4 {
		t.Fatalf("reasons[unparsed]=%d want 4(빈 줄·탭 없음·비숫자 ts·3필드)", reasons["unparsed"])
	}
	if len(reasons) != 2 {
		t.Fatalf("reasons=%v want 정확히 {a,unparsed}(bad-input/foo 등 미유입)", reasons)
	}
}

// TestDropsByReasonFiveFields — D43: 정확 5필드 신형식 라인의 reason을 집계한다.
// 3필드(그 외 필드 수)는 여전히 unparsed(느슨 수용 금지, 설계 §5).
func TestDropsByReasonFiveFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.drops.log")
	data := "1700000000\tunknown-session\tcc:99999\tPostToolUse\tRead\n" +
		"1700000001\tshadow-oversize\t-\t-\t-\n" +
		"1700000002\tbroken\textra\n" // 3필드 → unparsed
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, got, _ := dropsByReason(p) // 실제 시그니처: (total, reasons, lastSeen)
	if got["unknown-session"] != 1 || got["shadow-oversize"] != 1 {
		t.Fatalf("5필드 reason 집계 실패: %v", got)
	}
	if got["unparsed"] != 1 {
		t.Fatalf("3필드는 unparsed 유지: %v", got)
	}
}

// ⑧ F5: `hook install --user` 후 doctor가 사용자 범위 등록을 인식하고 프로젝트 범위는 미등록으로
// 보고해야 한다(프로젝트-only 검사 회귀 방지). 사용자 홈은 t.Setenv로 자기 전용 TempDir로 덮어써
// 실사용자 ~/.claude를 건드리지 않는다(TestMain 기본 홈 격리 위에서 추가 격리·자동 복원).
func TestDoctor_UserScopeHookRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir 이음새
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()

	var iout bytes.Buffer
	if err := runHookInstall([]string{"--user"}, storeRoot, "", false, projectRoot, "0.1.0", &iout); err != nil {
		t.Fatalf("install --user: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("user-scope settings not written under temp home: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "user=등록됨") {
		t.Fatalf("doctor must report user-scope registration:\n%s", out)
	}
	if !strings.Contains(out, "project=미등록") {
		t.Fatalf("doctor must report project scope as unregistered:\n%s", out)
	}
	assertDoctorAscending(t, out)
}

// TestDoctorVersionlessHookMarker — D82 doctor 경고(§2-9). 무버전 마커가 설치된 상태에서
// 훅 스코프는 버전 불일치 경고를 내지 **않는다**. 같은 픽스처에서 그 훅 등록이 **소유로
// 인식되는지**를 등록 개수로 함께 단정한다 — 경고만 보면 무버전 마커가 소유 판정에서 아예
// 탈락해 개수가 0이 된 경우도 "경고 없음"으로 통과한다.
// 불일치 부재는 **훅 스코프 줄([9]·[16])에 한정**한다 — D83의 [20]은 MCP 등록물의 버전을
// 비교하는 자리라 같은 픽스처에서 '≠'를 정당하게 인쇄한다.
func TestDoctorVersionlessHookMarker(t *testing.T) {
	isolateCodexHome(t)
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	var iout bytes.Buffer
	if err := runHookInstall(nil, storeRoot, "", false, projectRoot, "0.15.0", &iout); err != nil {
		t.Fatalf("install: %v", err)
	}
	// 설치 버전과 다른 버전으로 doctor를 돌려도 훅 스코프는 흔들리지 않는다.
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.16.0", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "project=등록됨(6개)") {
		t.Fatalf("무버전 마커가 소유로 인식되지 않았다(개수 0이면 경고 없음도 공허하다):\n%s", out)
	}
	// '≠' 부재는 **훅 스코프 줄에 한정**해 본다 — Task 11이 더하는 [20]은 MCP 등록물의
	// 버전을 비교하므로 같은 픽스처(설치 0.15.0 · doctor 0.16.0)에서 정당하게 '≠'를
	// 인쇄한다. 출력 전체를 보면 그 줄이 이 단정을 깨뜨린다.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "[9] hooks:") && !strings.HasPrefix(line, "[16] codex:") {
			continue
		}
		if strings.Contains(line, "≠") {
			t.Fatalf("훅 스코프가 버전 불일치 경고를 냈다: %s", line)
		}
	}
	// 설치 산출물의 마커 값 자체가 무버전이어야 한다(문면만 고친 구현을 배제한다).
	sb, err := os.ReadFile(filepath.Join(projectRoot, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sb), `"__ctrManaged": "context-router"`) {
		t.Fatalf("훅 그룹 마커가 무버전이 아니다:\n%s", sb)
	}
	if strings.Contains(string(sb), `"__ctrManaged": "context-router/`) {
		t.Fatalf("훅 그룹에 버전 있는 마커가 남았다:\n%s", sb)
	}
}

// TestHookRegistrationBytesVersionIndependent — D82의 핵심 단정(§2-8). 서로 다른 두 버전
// 문자열로 훅 등록물을 조립해 **바이트가 같음**을 단정한다. 조립 지점은 version 인자를 계속
// 받는 진입점(runHookInstall·runHookInstallCodex)이다 — 마커가 상수가 된 뒤 version을 받지
// 않는 함수(mergeHookSettings·mergeCodexHooks는 marker만 받는다) 수준에서 조립하면 두 산출이
// 자동으로 같아져 공허하게 통과한다. 같은 테스트가 **MCP 등록물에서는 버전이 반영됨**을 함께
// 단정한다 — 한쪽만 보면 "전부 무버전화" 같은 과잉 변경이 통과한다.
// t.Setenv 사용 → t.Parallel 금지.
func TestHookRegistrationBytesVersionIndependent(t *testing.T) {
	run := func(t *testing.T, version string) (settings, mcp, codexHooks, codexCfg []byte) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("CODEX_HOME", home)
		proj := t.TempDir()
		var out bytes.Buffer
		if err := runHookInstall(nil, t.TempDir(), "", false, proj, version, &out); err != nil {
			t.Fatalf("install(%s): %v", version, err)
		}
		if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, proj, version, &out); err != nil {
			t.Fatalf("install --codex(%s): %v", version, err)
		}
		settings, _ = os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
		mcp, _ = os.ReadFile(mcpConfigPath(proj))
		codexHooks, _ = os.ReadFile(filepath.Join(home, "hooks.json"))
		codexCfg, _ = os.ReadFile(filepath.Join(home, "config.toml"))
		return settings, mcp, codexHooks, codexCfg
	}
	s1, m1, ch1, cc1 := run(t, "0.15.0")
	s2, m2, ch2, cc2 := run(t, "9.9.9")
	if !bytes.Equal(s1, s2) {
		t.Errorf("settings.json이 버전만으로 달라졌다:\n1: %s\n2: %s", s1, s2)
	}
	if !bytes.Equal(ch1, ch2) {
		t.Errorf("codex hooks.json이 버전만으로 달라졌다:\n1: %s\n2: %s", ch1, ch2)
	}
	if bytes.Equal(m1, m2) {
		t.Errorf(".mcp.json에 버전이 반영되지 않았다(전부 무버전화는 과잉 변경이다):\n%s", m1)
	}
	if bytes.Equal(cc1, cc2) {
		t.Errorf("config.toml의 env.CTR_MANAGED에 버전이 반영되지 않았다:\n%s", cc1)
	}
}

// TestLegacyVersionedMarkerStillOwned — D82 하위 호환(§2-10). 구 마커(context-router/0.14.0)가
// 소유로 인정되고 대칭 제거된다. uninstall이 구·신 마커 양쪽을 지운다.
func TestLegacyVersionedMarkerStillOwned(t *testing.T) {
	for _, marker := range []string{"context-router/0.14.0", "context-router"} {
		proj := t.TempDir()
		seed, err := mergeHookSettings(nil, buildHookCommand(false, "", false), marker, true)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(proj, ".claude", "settings.json")
		if err := atomicWriteFile(path, seed); err != nil {
			t.Fatal(err)
		}
		n, _, err := scanRegisteredHooks(path)
		if err != nil || n != len(hookRegistrations) {
			t.Fatalf("marker %q: 소유 그룹 %d개 (err=%v) want %d", marker, n, err, len(hookRegistrations))
		}
		var out bytes.Buffer
		if err := runHookUninstall(nil, proj, &out); err != nil {
			t.Fatalf("marker %q: uninstall: %v", marker, err)
		}
		after, _ := os.ReadFile(path)
		if strings.Contains(string(after), "context-router hook") {
			t.Fatalf("marker %q: 대칭 제거되지 않았다:\n%s", marker, after)
		}
	}
}

// hookRunPayload — cli 러닝 훅 stdin JSON을 조립한다(경로 이스케이프 위해 json.Marshal 사용).
// session_id는 hook.canonicalUUIDRe를 통과하는 고정 UUID.
func hookRunPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	fields["session_id"] = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// setStdin — os.Stdin을 payload를 담은 임시 파일로 교체한다(cli.runHook은 os.Stdin을 직접 읽는다).
func setStdin(t *testing.T, payload []byte) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*.json")
	if err != nil {
		t.Fatalf("temp stdin: %v", err)
	}
	if _, err := f.Write(payload); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek stdin: %v", err)
	}
	os.Stdin = f
	t.Cleanup(func() { _ = f.Close() })
}

// contentArtifactCount — contentDir/content.db(read-only)의 artifacts 행 수. 미존재면 -1(Shadow 미저장).
func contentArtifactCount(t *testing.T, contentDir string) int {
	t.Helper()
	if _, err := os.Stat(filepath.Join(contentDir, "content.db")); os.IsNotExist(err) {
		return -1
	}
	st, err := store.Open(contentDir, true)
	if err != nil {
		t.Fatalf("open content.db ro: %v", err)
	}
	defer func() { _ = st.Close() }()
	var n int
	if err := st.Reader().QueryRow("SELECT count(*) FROM artifacts").Scan(&n); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	return n
}

// F7: cli 러닝 훅의 --no-shadow 분기가 CTR_SHADOW_OFF로 반영돼 Shadow 아티팩트를 저장하지 않는지
// 행동으로 확인한다(플래그 없는 대조군은 저장). SessionStart로 세션을 만든 뒤 >CTR_SHADOW_MIN
// PostToolUse를 발화한다(러닝 경로는 os.Stdin을 읽으므로 각 호출마다 stdin 교체). TempDir 격리.
func TestRunHook_NoShadowRunningBranch(t *testing.T) {
	t.Setenv("CTR_SHADOW_MIN", "100") // 소형 payload로 게이트 통과(대용량 리터럴 회피)
	t.Setenv("CTR_SHADOW_OFF", "")    // 대조군이 실제 env 잔여 OFF에 오염되지 않도록
	t.Setenv("CTR_HOOKS_OFF", "")
	origStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = origStdin })

	run := func(args []string) int {
		storeRoot := t.TempDir()
		cwd := t.TempDir()
		// ① 세션 생성 — 후속 PostToolUse가 미지 세션으로 drop되지 않도록.
		setStdin(t, hookRunPayload(t, map[string]any{
			"hook_event_name": "SessionStart", "cwd": cwd, "source": "startup",
		}))
		if err := runHook(context.Background(), nil, storeRoot, "", false, cwd, "test", io.Discard); err != nil {
			t.Fatalf("sessionstart: %v", err)
		}
		// ② >MIN PostToolUse — args(--no-shadow 유무)에 따라 Shadow on/off.
		setStdin(t, hookRunPayload(t, map[string]any{
			"hook_event_name": "PostToolUse", "cwd": cwd, "tool_name": "Bash",
			"tool_response": map[string]any{"stdout": strings.Repeat("a", 500), "stderr": ""},
		}))
		if err := runHook(context.Background(), args, storeRoot, "", false, cwd, "test", io.Discard); err != nil {
			t.Fatalf("posttooluse: %v", err)
		}
		canon, err := ident.Canonicalize(cwd)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		return contentArtifactCount(t, filepath.Join(storeRoot, "projects", canon.ProjectID))
	}

	if n := run([]string{"--no-shadow"}); n != -1 {
		t.Fatalf("--no-shadow artifacts=%d want -1(미저장) — CTR_SHADOW_OFF 미반영", n)
	}
	if n := run(nil); n != 1 {
		t.Fatalf("control artifacts=%d want 1(저장)", n)
	}
}

// D35 설치 — 병합이 타 그룹·미지 최상위 키를 보존하고 자기 2이벤트만 소유한다.
// D52 — Codex hooks.json 자기 그룹 marker 버전 추출(v0.9 §0): isOurCodexGroup은 bool만
// 반환하므로 신설(적대 검수 P1). 파일 부재는 (0,"",nil) — 미설치 정보 분기.
func TestScanCodexRegisteredHooks(t *testing.T) {
	// ① 부재: count=0 marker="" err=nil
	if n, m, err := scanCodexRegisteredHooks(filepath.Join(t.TempDir(), "hooks.json")); n != 0 || m != "" || err != nil {
		t.Fatalf("부재: n=%d m=%q err=%v want 0/\"\"/nil", n, m, err)
	}
	// ② 자기 그룹 존재: mergeCodexHooks(guard 포함 3그룹) 산출을 임시 파일로 쓰고 → count>0, marker 추출.
	const wantMarker = "0.9.0"
	self, err := mergeCodexHooks(nil, buildCodexHookCommand(false, "", false), hookMarker(wantMarker), true, true)
	if err != nil {
		t.Fatalf("self 조립: %v", err)
	}
	selfPath := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(selfPath, self, 0o600); err != nil {
		t.Fatalf("self write: %v", err)
	}
	if n, m, err := scanCodexRegisteredHooks(selfPath); n <= 0 || m != wantMarker || err != nil {
		t.Fatalf("자기 그룹: n=%d m=%q err=%v want >0/%q/nil", n, m, err, wantMarker)
	}
	// ③ 타인 그룹만: statusMessage 마커 접두 없는 그룹 → count=0, marker=""(isOurCodexGroup 전건 탈락).
	foreign := []byte(`{"hooks":{"PostToolUse":[{"matcher":"","hooks":[{"type":"command","command":"pwsh -File user.ps1","timeout":10,"statusMessage":"user"}]}]}}`)
	foreignPath := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
		t.Fatalf("foreign write: %v", err)
	}
	if n, m, err := scanCodexRegisteredHooks(foreignPath); n != 0 || m != "" || err != nil {
		t.Fatalf("타인 그룹: n=%d m=%q err=%v want 0/\"\"/nil", n, m, err)
	}
}

func TestMergeCodexHooksInstallPreservesForeign(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10,"statusMessage":"policy"}]}]},"otherTop":1}`)
	out, err := mergeCodexHooks(existing, "context-router codex-hook", "context-router/0.4.0", true, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("out 파싱: %v", err)
	}
	if _, ok := m["otherTop"]; !ok {
		t.Fatalf("미지 최상위 키 소실: %s", out)
	}
	var hooks map[string][]json.RawMessage
	if err := json.Unmarshal(m["hooks"], &hooks); err != nil {
		t.Fatalf("hooks 파싱: %v", err)
	}
	if len(hooks["PreToolUse"]) != 1 {
		t.Fatalf("타 그룹 보존 실패: %v", hooks["PreToolUse"])
	}
	for _, ev := range []string{"SessionStart", "PostToolUse"} {
		if len(hooks[ev]) != 1 || !isOurCodexGroup(hooks[ev][0]) {
			t.Fatalf("%s 자기 그룹 미등록: %v", ev, hooks[ev])
		}
	}
	if strings.Contains(string(out), "__ctrManaged") {
		t.Fatalf("Codex hooks.json에 미지 필드 금지(§11.1 G3): %s", out)
	}
}

// 멱등: install 2회 = 1회와 동일 바이트. 제거 대칭: install→uninstall이 원본 구조를 복원.
func TestMergeCodexHooksIdempotentAndSymmetric(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10}]}]}}`)
	once, err := mergeCodexHooks(existing, "context-router codex-hook", "context-router/0.4.0", true, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	twice, err := mergeCodexHooks(once, "context-router codex-hook", "context-router/0.4.1", true, false)
	if err != nil {
		t.Fatalf("재install: %v", err)
	}
	if strings.Contains(string(twice), "0.4.0") {
		t.Fatalf("구버전 마커 잔존(교체 실패): %s", twice)
	}
	removed, err := mergeCodexHooks(twice, "", "", false, false)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if strings.Contains(string(removed), "context-router") {
		t.Fatalf("자기 항목 잔존: %s", removed)
	}
	if !strings.Contains(string(removed), "policy.ps1") {
		t.Fatalf("타 그룹 소실: %s", removed)
	}
}

// F4 — 혼합 그룹(자기 항목 + 사용자 항목 동거)은 불가침: install이 그 그룹을 건드리지 않고
// 순수 자기 그룹을 별도로 추가한다(파손 금지 > 멱등 완전성 — 혼합 그룹의 자기 잔존 항목
// 정리는 사용자 /hooks 몫).
func TestMergeCodexHooksMixedGroupUntouched(t *testing.T) {
	mixed := []byte(`{"hooks":{"PostToolUse":[{"matcher":"","hooks":[` +
		`{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"context-router/0.3.9"},` +
		`{"type":"command","command":"pwsh -File user.ps1","timeout":10,"statusMessage":"user"}]}]}}`)
	out, err := mergeCodexHooks(mixed, "context-router codex-hook", "context-router/0.4.0", true, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !strings.Contains(string(out), "user.ps1") || !strings.Contains(string(out), "context-router/0.3.9") {
		t.Fatalf("혼합 그룹이 변형·삭제됨(불가침 위반): %s", out)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("out 파싱: %v", err)
	}
	var hooks map[string][]json.RawMessage
	if err := json.Unmarshal(m["hooks"], &hooks); err != nil {
		t.Fatalf("hooks 파싱: %v", err)
	}
	if len(hooks["PostToolUse"]) != 2 {
		t.Fatalf("PostToolUse 그룹 수=%d want 2(혼합 보존 + 순수 신규)", len(hooks["PostToolUse"]))
	}
}

// F4 — 동일 버전 재적용의 진짜 멱등: f(f(x)) == f(x) 바이트 동일(중복·순서 drift 검출).
func TestMergeCodexHooksIdempotentBytes(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10}]}]}}`)
	once, err := mergeCodexHooks(existing, "context-router codex-hook", "context-router/0.4.0", true, false)
	if err != nil {
		t.Fatalf("1차: %v", err)
	}
	twice, err := mergeCodexHooks(once, "context-router codex-hook", "context-router/0.4.0", true, false)
	if err != nil {
		t.Fatalf("2차: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("멱등 위반:\n1차=%s\n2차=%s", once, twice)
	}
}

// 경로: 기본 project <root>/.codex/hooks.json, --user는 CODEX_HOME 미설정 시 ~/.codex/hooks.json.
// t.Setenv 사용 → t.Parallel 금지(기존 관례).
func TestCodexHooksPath(t *testing.T) {
	t.Setenv("CODEX_HOME", "") // 빈 문자열=미설정 → ~/.codex 폴백을 결정적으로 만든다(최종 리뷰 Codex P2)
	p, err := codexHooksPath(false, `C:\proj`)
	if err != nil || p != filepath.Join(`C:\proj`, ".codex", "hooks.json") {
		t.Fatalf("project 경로=%q err=%v", p, err)
	}
	u, err := codexHooksPath(true, `C:\proj`)
	if err != nil || !strings.HasSuffix(u, filepath.Join(".codex", "hooks.json")) {
		t.Fatalf("user 경로=%q err=%v", u, err)
	}
}

// --user는 CODEX_HOME이 설정되면 $CODEX_HOME/hooks.json을 쓴다(최종 리뷰 Codex P2 — 무성 오설치
// 방지). t.Setenv 사용 → t.Parallel 금지.
func TestCodexHooksPathCodexHome(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	u, err := codexHooksPath(true, `C:\proj`)
	if err != nil || u != filepath.Join(codexHome, "hooks.json") {
		t.Fatalf("user 경로=%q want=%q err=%v", u, filepath.Join(codexHome, "hooks.json"), err)
	}
}

// e2e: hook install --codex가 파일 생성 + 신뢰 승인 안내를 출력한다.
func TestRunHookInstallCodex(t *testing.T) {
	isolateCodexHome(t) // config.toml은 스코프 무관 CODEX_HOME/홈 — 공유 임시 홈 오염 차단
	root := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall([]string{"--codex"}, "", "", false, root, "0.4.0", &out); err != nil {
		t.Fatalf("install --codex: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json 미생성: %v", err)
	}
	if !strings.Contains(string(written), "context-router codex-hook") {
		t.Fatalf("러닝 명령이 codex-hook 서브커맨드가 아님(§11.2 F3): %s", written)
	}
	if !strings.Contains(out.String(), "/hooks") {
		t.Fatalf("신뢰 승인 안내 누락: %q", out.String())
	}
}

// isOurCodexGroup 직접 에지 단정(v0.4 최종 리뷰 이월 — 기존 커버는 merge 경유 간접).
// 전건 판정(§11.2 F4): 모든 항목이 command 토큰 정확 일치 AND statusMessage 마커 접두일
// 때만 자기 그룹 — 혼합 그룹 불가침의 근거 함수.
func TestIsOurCodexGroupEdges(t *testing.T) {
	ours := `{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"context-router/0.4.0"}`
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"비JSON", `not-json`, false},
		{"빈 그룹", `{"matcher":"","hooks":[]}`, false},
		{"전건 자기 항목", `{"matcher":"","hooks":[` + ours + `]}`, true},
		{"후행 플래그 허용", `{"matcher":"","hooks":[{"type":"command","command":"context-router codex-hook --no-shadow","timeout":10,"statusMessage":"context-router/0.4.0"}]}`, true},
		{"혼합(자기+외래)", `{"matcher":"","hooks":[` + ours + `,{"type":"command","command":"pwsh -File u.ps1","timeout":10,"statusMessage":"user"}]}`, false},
		{"command 불일치(claude 러닝)", `{"matcher":"","hooks":[{"type":"command","command":"context-router hook","timeout":10,"statusMessage":"context-router/0.4.0"}]}`, false},
		{"접두 닮은 명령(codex-hook-wrapper)", `{"matcher":"","hooks":[{"type":"command","command":"context-router codex-hook-wrapper","timeout":10,"statusMessage":"context-router/0.4.0"}]}`, false},
		{"marker 접두 불일치", `{"matcher":"","hooks":[{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"other/0.4.0"}]}`, false},
	}
	for _, c := range cases {
		if got := isOurCodexGroup(json.RawMessage(c.raw)); got != c.want {
			t.Fatalf("%s: isOurCodexGroup=%v want %v", c.name, got, c.want)
		}
	}
}

// e2e: hook uninstall --codex run 분기(v0.4 최종 리뷰 이월 — 기존 커버는 merge 레벨만) —
// install 산출물에서 자기 그룹만 제거, 선존 외래 그룹 보존, 제거 완료 안내 출력.
func TestRunHookUninstallCodex(t *testing.T) {
	isolateCodexHome(t) // config.toml은 스코프 무관 CODEX_HOME/홈 — 공유 임시 홈 오염 차단
	root := t.TempDir()
	foreign := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10}]}]}}`)
	if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codex", "hooks.json"), foreign, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := runHookInstall([]string{"--codex"}, "", "", false, root, "0.4.0", io.Discard); err != nil {
		t.Fatalf("install --codex: %v", err)
	}
	var out bytes.Buffer
	if err := runHookUninstall([]string{"--codex"}, root, &out); err != nil {
		t.Fatalf("uninstall --codex: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json 읽기: %v", err)
	}
	if strings.Contains(string(written), "context-router") {
		t.Fatalf("자기 항목 잔존: %s", written)
	}
	if !strings.Contains(string(written), "policy.ps1") {
		t.Fatalf("외래 그룹 소실: %s", written)
	}
	if !strings.Contains(out.String(), "제거 완료") {
		t.Fatalf("제거 완료 안내 누락: %q", out.String())
	}
}

// e2e: hook uninstall --codex 파일 미존재 no-op 분기 — 안내만 출력, 오류·파일 생성 없음.
func TestRunHookUninstallCodexNoFile(t *testing.T) {
	isolateCodexHome(t) // config.toml은 스코프 무관 CODEX_HOME/홈 — 공유 임시 홈 오염 차단
	root := t.TempDir()
	var out bytes.Buffer
	if err := runHookUninstall([]string{"--codex"}, root, &out); err != nil {
		t.Fatalf("uninstall --codex: %v", err)
	}
	if !strings.Contains(out.String(), "설정 파일 없음") {
		t.Fatalf("no-op 안내 누락: %q", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(root, ".codex", "hooks.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("no-op인데 파일 생성됨: %v", statErr)
	}
}

// Codex P2: hooks.json 부재(부분 설치 — config만 쓰이고 hooks.json은 실패·수동 삭제)에서도
// config.toml 관리 블록 제거를 계속한다. 조기 반환이 정리를 건너뛰던 회귀 방지.
// t.Setenv 사용 → t.Parallel 금지.
func TestRunHookUninstallCodexConfigOnlyNoHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	block := "[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"args = [\"--enable\", \"ingest,net\"]\n" +
		"enabled_tools = [\"ctr_search\"]\n" +
		"[mcp_servers.ctr.env]\n" +
		"CTR_MANAGED = \"context-router/0.15.0\"\n"
	cfg := "model = \"gpt\"\n\n" + block
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall([]string{"--codex", "--user"}, t.TempDir(), &out); err != nil {
		t.Fatalf("uninstall --codex --user: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml 읽기: %v", err)
	}
	if strings.Contains(string(got), "[mcp_servers.ctr]") {
		t.Fatalf("hooks.json 부재로 관리 블록 정리가 생략됨:\n%s", got)
	}
	if !strings.Contains(out.String(), "MCP 등록 블록 제거 완료") {
		t.Fatalf("블록 제거 안내 누락: %q", out.String())
	}
	// D84 — "내용을 바꾸기 직전 config.toml.bak 단일 슬롯"은 제거 경로에도 선다(리뷰 P1).
	// 세 writer(install·doctor --fix·uninstall)가 같은 규칙을 써야 잘못된 제거를 되돌릴 수 있다.
	bak, bakErr := os.ReadFile(filepath.Join(home, "config.toml.bak"))
	if bakErr != nil {
		t.Fatalf("제거 경로가 백업을 남기지 않았다: %v", bakErr)
	}
	if string(bak) != cfg {
		t.Fatalf("백업이 원본과 다르다:\n%s", bak)
	}
	// 무변경 재실행은 백업도 쓰기도 하지 않는다 — 단일 슬롯이 무변경으로 덮이면 되돌릴 원본이
	// 사라진다(D84가 install 쪽에서 같은 이유로 changed 판정 뒤에 백업을 둔다).
	if rmErr := os.Remove(filepath.Join(home, "config.toml.bak")); rmErr != nil {
		t.Fatal(rmErr)
	}
	var again bytes.Buffer
	if err := runHookUninstall([]string{"--codex", "--user"}, t.TempDir(), &again); err != nil {
		t.Fatalf("2차 uninstall: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "config.toml.bak")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("무변경 재실행이 단일 슬롯을 덮었다: %v", statErr)
	}
}

// TestRunHookUninstallCodexReportsSpanAnomaly — 구간 판정 이상으로 config.toml을 손대지
// 못했으면 **그 사실과 사유**를 인쇄한다. 종전에는 changed 갈래에만 인쇄가 있어 이 이탈에서
// 아무 줄도 나오지 않았고, 사용자는 "훅 항목 제거 완료 + exit 0"만 보는데 두 관리 테이블이
// 파일에 남아 Codex가 매 세션 MCP 서버를 계속 띄웠다. D87이 이스케이프 표기 키를 공유 판정에
// 더하면서 이 이탈은 중복 헤더·미종료 문자열 밖으로 넓어졌다.
// 종료코드 계약은 그대로다 — 인쇄만 는다.
// t.Setenv 사용 → t.Parallel 금지.
func TestRunHookUninstallCodexReportsSpanAnomaly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	// 관리 테이블 안의 이스케이프 표기 키 — 어느 관리 키로도 디코드되지 않지만 우리는
	// 이스케이프를 해석하지 않으므로 구간 판정을 신뢰할 수 없다(D87).
	cfg := "[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		`"my\tkey" = 1` + "\n" +
		"[mcp_servers.ctr.env]\n" +
		codexMarkerKey + " = \"context-router/0.16.0\"\n"
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall([]string{"--codex", "--user"}, t.TempDir(), &out); err != nil {
		t.Fatalf("종료코드 계약이 바뀌었다: %v", err) // 인쇄만 는다 — 실패로 바뀌면 안 된다
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != cfg {
		t.Fatalf("이상 파일을 손댔다:\n%s", got)
	}
	s := out.String()
	if !strings.Contains(s, "config.toml") {
		t.Errorf("config.toml을 손대지 못했다는 사실을 알리지 않았다:\n%s", s)
	}
	if !strings.Contains(s, anomalyEscapedKey.reason()) {
		t.Errorf("무변경 사유를 알리지 않았다:\n%s", s)
	}
}

// TestHookUninstallCodexNotOwned — 소유 판정에 실패해 무변경으로 빠진 갈래에 문면이 있어야
// 한다. 없으면 훅 제거 문면과 종료코드 0만 보이는데 관리 테이블은 남아 Codex가 그 MCP
// 서버를 계속 띄운다. **테이블이 아예 없는 갈래는 무문면이다** — 미설치 사용자의 흔한
// 실행에 잔존 문면이 나가면 안 된다.
// t.Setenv(isolateCodexHome) 사용 → t.Parallel 금지.
func TestHookUninstallCodexNotOwned(t *testing.T) {
	cases := []struct {
		name, src string
		wantMsg   bool
	}{
		{"소유 실패", "[mcp_servers.ctr]\ncommand = \"other\"\n", true},
		{"테이블 부재", "[other]\nx = 1\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := isolateCodexHome(t)
			writeCodexConfig(t, home, c.src)
			var buf bytes.Buffer
			if err := runHookUninstallCodex(false, t.TempDir(), &buf); err != nil {
				t.Fatalf("uninstall 실패: %v", err)
			}
			// 사유까지 특정한다 — 여섯 사유가 공유하는 접두만 보면 엉뚱한 사유가 나가도 초록이다.
			got := strings.Contains(buf.String(), anomalyNotOwned.reason())
			if got != c.wantMsg {
				t.Errorf("문면=%v want %v\n%s", got, c.wantMsg, buf.String())
			}
		})
	}
}

// D47 설치 결합 — withGuard=true면 PreToolUse(matcher Bash) 그룹이 추가되고,
// uninstall은 withGuard 무관하게 전 이벤트 소거(제거 대칭). 가드 포함 재병합은 멱등.
// (matcher 단정은 json.MarshalIndent 실출력 형식 "matcher": "Bash"에 맞춘다 — 브리프의
// 무공백 substring은 실제 콜론-공백 출력과 불일치라 형식만 조정.)
func TestMergeCodexHooksGuardGroup(t *testing.T) {
	out, err := mergeCodexHooks(nil, "context-router codex-hook", "context-router/0.7.0", true, true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"PreToolUse"`) || !strings.Contains(s, `"matcher": "Bash"`) {
		t.Fatalf("PreToolUse(Bash) 그룹 누락:\n%s", s)
	}
	// withGuard=false — 캡처 2이벤트만
	out2, err := mergeCodexHooks(nil, "context-router codex-hook", "context-router/0.7.0", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out2), `"PreToolUse"`) {
		t.Fatalf("withGuard=false에 PreToolUse가 등록됨:\n%s", out2)
	}
	// 가드 포함 멱등: f(f(x)) == f(x) 바이트 동일
	again, err := mergeCodexHooks(out, "context-router codex-hook", "context-router/0.7.0", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, again) {
		t.Fatalf("가드 포함 멱등 위반:\n1차=%s\n2차=%s", out, again)
	}
	// 제거 대칭: guard 포함 설치본에서 uninstall이 PreToolUse까지 소거
	removed, err := mergeCodexHooks(out, "", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(removed), "codex-hook") {
		t.Fatalf("uninstall 잔존:\n%s", removed)
	}
}

// D47+D48 설치 결합: (a) 깨끗한 config.toml → 블록 기입 + PreToolUse 등록,
// (b) 키-경계 충돌 config.toml → 기입 생략 + PreToolUse 미등록(캡처만) + 안내.
func TestRunHookInstallCodexCoupling(t *testing.T) {
	// (a)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	var buf bytes.Buffer
	if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, t.TempDir(), "0.7.0", &buf); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || !strings.Contains(string(cfg), "[mcp_servers.ctr]") {
		t.Fatalf("블록 미기입: %v %s", err, cfg)
	}
	hooks, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	if !strings.Contains(string(hooks), `"PreToolUse"`) {
		t.Fatalf("가드 미등록:\n%s", hooks)
	}
	// (b)
	home2 := t.TempDir()
	t.Setenv("CODEX_HOME", home2)
	conflict := "[mcp_servers.\"ctr\"]\ncommand = \"custom\"\n"
	if err := os.WriteFile(filepath.Join(home2, "config.toml"), []byte(conflict), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, t.TempDir(), "0.7.0", &buf2); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := os.ReadFile(filepath.Join(home2, "config.toml"))
	if string(cfg2) != conflict {
		t.Fatalf("충돌 config가 변경됨:\n%s", cfg2)
	}
	hooks2, _ := os.ReadFile(filepath.Join(home2, "hooks.json"))
	if strings.Contains(string(hooks2), `"PreToolUse"`) {
		t.Fatalf("MCP 미확정인데 가드 등록됨:\n%s", hooks2)
	}
	if !strings.Contains(buf2.String(), "보류") {
		t.Fatalf("보류 안내 누락: %s", buf2.String())
	}
}

// TestRunHookInstallCodexBackupAndIdempotent — D84 백업과 멱등(§2-12). 내용 변경이 있을 때만
// .bak이 생기고, 무변경 멱등 실행에서는 생기지 않는다. 멱등 픽스처는 **호스트 재직렬화 형태**로
// 만든다(주석 없음 · **빈 args 포함** — §3 표4의 현재 사용자 파일이 그 상태이고, 부재와 []를
// 동치로 보는 D80 규칙이 걸리는 자리다 · §1.3-1 ②가 관측한 키 순서·인용·env 표기) — 우리
// 산출물을 그대로 입력으로 쓰면 실사용의 흔들림을 재현하지 못한다. .bak은 단일 슬롯이라
// 누적하지 않는다. t.Setenv 사용 → t.Parallel 금지.
func TestRunHookInstallCodexBackupAndIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cfg := filepath.Join(home, "config.toml")
	bak := cfg + ".bak"
	// 프로필이 빈 재설치(§3 표4의 현재 사용자 파일 상태) — args 키를 쓰지 않아야 한다.
	// **Task 1 게이트 ②의 관측 형태를 여기에 반영한다**: 관측한 키 순서·인용 방식·공백·env
	// 표기가 아래와 다르면 seed를 그 형태로 고친 뒤 같은 단정을 그대로 건다(스펙 §2-12).
	// 관측이 우리 기입과 같았으면 아래 그대로 둔다.
	seed := "model = \"gpt\"\n\n[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\nenabled_tools = [\"ctr_search\"]\n"
	if err := os.WriteFile(cfg, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, t.TempDir(), "0.15.0", &out); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	first, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(bak); statErr != nil {
		t.Fatalf("내용이 바뀌었는데 .bak이 없다: %v", statErr)
	}
	if b, _ := os.ReadFile(bak); string(b) != seed {
		t.Errorf(".bak이 변경 직전 내용이 아니다: %q", b)
	}
	// 빈 프로필을 이월했으므로 args 키를 쓰지 않는다(부재와 []를 동치로 본다, D80).
	if strings.Contains(string(first), "args = ") {
		t.Errorf("빈 프로필인데 args 줄을 썼다:\n%s", first)
	}
	// 무변경 멱등 실행: 바이트가 그대로이고 .bak의 mtime도 움직이지 않는다.
	bakInfo, err := os.Stat(bak)
	if err != nil {
		t.Fatal(err)
	}
	if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, t.TempDir(), "0.15.0", &out); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	second, _ := os.ReadFile(cfg)
	if !bytes.Equal(first, second) {
		t.Errorf("무변경 재실행이 바이트를 바꿨다:\n1: %s\n2: %s", first, second)
	}
	if again, _ := os.Stat(bak); !again.ModTime().Equal(bakInfo.ModTime()) {
		t.Errorf("무변경 재실행이 .bak을 다시 썼다 — 단일 슬롯 계약이 무의미해진다")
	}
}

// TestRunHookInstallCodexNotices — D81 설치기 안내(§2-7 exec opt-in 안내 · Codex 되읽기).
// 승인 모드 안내는 파일에 남지 않으므로(재직렬화가 주석을 지운다) stdout으로 내고, exec opt-in은
// 별도 [mcp_servers.ctr-exec]에 걸린 승인 게이트를 거치지 않는 두 번째 경로가 생긴다는 사실을
// 함께 알린다. 어느 안내도 config.toml에 키를 남기지 않는다.
func TestRunHookInstallCodexNotices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	var out bytes.Buffer
	if err := runHookInstall([]string{"--codex", "--user", "--enable-exec"}, "", "", false, t.TempDir(), "0.15.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	s := out.String()
	for _, want := range []string{"default_tools_approval_mode", "exec"} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout 안내에 %q 없음: %q", want, s)
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if strings.Contains(string(cfg), "approval_mode") {
		t.Errorf("설치기가 승인 모드 키를 기입했다:\n%s", cfg)
	}
	if !strings.Contains(string(cfg), "\"ctr_execute_file\"") {
		t.Errorf("--enable-exec이 enabled_tools에 반영되지 않았다:\n%s", cfg)
	}

	// 되읽지 못하는 args — 두 키를 손대지 않고 그 사실을 stdout으로 알린다.
	home2 := t.TempDir()
	t.Setenv("CODEX_HOME", home2)
	odd := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--profile\", \"global-search\"]\nenabled_tools = [\"custom\"]\n"
	if err := os.WriteFile(filepath.Join(home2, "config.toml"), []byte(odd), 0o600); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, t.TempDir(), "0.15.0", &out2); err != nil {
		t.Fatalf("install(odd): %v", err)
	}
	if !strings.Contains(out2.String(), "해석하지 못") {
		t.Errorf("되읽기 실패 안내가 없다: %q", out2.String())
	}
	cfg2, _ := os.ReadFile(filepath.Join(home2, "config.toml"))
	if !strings.Contains(string(cfg2), "args = [\"--profile\", \"global-search\"]") ||
		!strings.Contains(string(cfg2), "enabled_tools = [\"custom\"]") {
		t.Errorf("해석하지 못한 값을 덮어썼다:\n%s", cfg2)
	}

	// 되읽지 못하는 args지만 enabled_tools에 exec 도구가 이미 있다(리뷰 라운드 1 — 이월 T4-F3의
	// 근본 픽스). Profiles는 이 경우 되읽기 실패로 nil이라 "프로필에 exec가 있는가"만 보면 안내가
	// 빠진다 — 산출물이 실제로 노출하는 도구를 봐야 한다. "켰습니다"가 아니라 "이미 있다" 톤이어야
	// 정확하다(우리가 이번에 튼 것이 아니다).
	home3 := t.TempDir()
	t.Setenv("CODEX_HOME", home3)
	oddExec := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--profile\", \"global-search\"]\nenabled_tools = [\"ctr_execute\", \"ctr_execute_file\"]\n"
	if err := os.WriteFile(filepath.Join(home3, "config.toml"), []byte(oddExec), 0o600); err != nil {
		t.Fatal(err)
	}
	var out3 bytes.Buffer
	if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, t.TempDir(), "0.15.0", &out3); err != nil {
		t.Fatalf("install(odd+exec): %v", err)
	}
	if !strings.Contains(out3.String(), "exec") {
		t.Errorf("exec가 이미 노출돼 있는데 안내가 없다: %q", out3.String())
	}
	if strings.Contains(out3.String(), "켰습니다") {
		t.Errorf("우리가 켠 게 아닌데 '켰습니다' 문면이 나갔다: %q", out3.String())
	}
	cfg3, _ := os.ReadFile(filepath.Join(home3, "config.toml"))
	if !strings.Contains(string(cfg3), "enabled_tools = [\"ctr_execute\", \"ctr_execute_file\"]") {
		t.Errorf("해석하지 못한 enabled_tools를 덮어썼다:\n%s", cfg3)
	}
}

// TestHookInstallCodexAnomalyReason — 이상 사유가 설치기 안내에 실린다(§2-7). 사유가 다른
// 파일에서 중복 헤더 정리만 지시하면 사용자는 install이 무변경인 이유를 알 수 없다.
func TestHookInstallCodexAnomalyReason(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want string
	}{
		{"중복 헤더", "[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n", "헤더가 둘 이상"},
		{"스캐너 열림", "[mcp_servers.ctr]\nk = \"\"\"\nunclosed\n", "닫히지 않았습니다"},
		{"이스케이프 키", "[mcp_servers.ctr]\n\"comm\\u0061nd\" = \"x\"\n", "이스케이프 표기"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			cfgPath := filepath.Join(codexHome, "config.toml")
			write(t, cfgPath, []byte(c.cfg))
			var out bytes.Buffer
			if err := runHookInstallCodex(true, false, false, "", t.TempDir(), "0.16.0", nil, false, &out); err != nil {
				t.Fatalf("runHookInstallCodex: %v", err)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("안내에 %q 없음:\n%s", c.want, out.String())
			}
			after, rErr := os.ReadFile(cfgPath)
			if rErr != nil {
				t.Fatal(rErr)
			}
			if string(after) != c.cfg {
				t.Errorf("config.toml이 바뀌었다:\n%s", after)
			}
		})
	}
}

// TestRunHookInstallCodexMigratesOldFormat — X6(§2-9). v0.14 구 형식 파일을 **실제 파일로** 놓고
// 설치 배선이 1회 변환하는지, 재실행이 무변경인지, 백업이 단일 슬롯인지 본다. 지금까지 구
// 형식 픽스처는 순수 함수 테스트에만 있었고 이 배선은 테스트에 없었다.
func TestRunHookInstallCodexMigratesOldFormat(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cfgPath := filepath.Join(codexHome, "config.toml")
	old := codexBlockBegin + "\n" +
		"[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"args = [\"--enable\", \"ingest,net\"]\n" +
		codexBlockEnd + "\n"
	write(t, cfgPath, []byte(old))

	var out bytes.Buffer
	if err := runHookInstallCodex(true, false, false, "", t.TempDir(), "0.16.0", nil, false, &out); err != nil {
		t.Fatalf("1회차: %v out=%s", err, out.String())
	}
	after1, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(after1)
	// 마커 두 줄이 사라진다
	if strings.Contains(got, codexBlockBegin) || strings.Contains(got, codexBlockEnd) {
		t.Errorf("마커가 남아 있다:\n%s", got)
	}
	// 표식이 env로 옮겨진다
	if !strings.Contains(got, codexMarkerKey+" = \""+hookMarker("0.16.0")+"\"") {
		t.Errorf("표식이 기입되지 않았다:\n%s", got)
	}
	// 백업이 원본을 담는다
	bak, bErr := os.ReadFile(cfgPath + ".bak")
	if bErr != nil {
		t.Fatalf(".bak이 없다: %v", bErr)
	}
	if string(bak) != old {
		t.Errorf(".bak이 원본과 다르다:\n%s", bak)
	}

	// 2회차 — 무변경이고 .bak은 1회차 이전 원본을 그대로 담는다.
	// (backupCodexConfig는 단일 슬롯을 덮어쓰므로 ".bak이 다시 생기지 않는지"는 존재 검사로
	//  관측할 수 없다 — 바이트로 잰다. §2-5)
	var out2 bytes.Buffer
	if err := runHookInstallCodex(true, false, false, "", t.TempDir(), "0.16.0", nil, false, &out2); err != nil {
		t.Fatalf("2회차: %v", err)
	}
	after2, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after2) != got {
		t.Errorf("2회차가 파일을 바꿨다:\n%s", after2)
	}
	bak2, _ := os.ReadFile(cfgPath + ".bak")
	if string(bak2) != old {
		t.Errorf(".bak이 2회차에 덮였다 — 무변경이면 백업하지 않아야 한다:\n%s", bak2)
	}
}

// TestDropsLastSeenUTC — D71: 사유별 마지막 발생 시각을 UTC 날짜로 병기한다. 역순 ts가 섞여도
// 최댓값을 쓰고(로그가 시간순임을 가정하지 않는다), 정수 변환에 실패한 ts는 집계만 하고 병기를
// 생략하며, unparsed에는 ts가 없으므로 병기하지 않는다.
func TestDropsLastSeenUTC(t *testing.T) {
	// F4(리뷰): CI 러너(GitHub Actions 등)는 이미 UTC라 time.Local이 UTC와 같으면 .UTC() 유무가
	// 결과를 가르지 않아 이 감시선이 무력화된다. time.Local을 UTC가 아닌 고정 오프셋으로 강제
	// 교체해 실행 머신의 실제 타임존과 무관하게 .UTC() 제거를 잡는다(t.Setenv("TZ", ...)는 이미
	// 로드된 time.Local을 바꾸지 못해 효과가 없다). 패키지 전역 상태를 건드리므로 t.Parallel()과
	// 병행 금지(이 패키지에 t.Parallel() 호출 없음을 grep으로 확인) — defer로 반드시 복원한다.
	origLocal := time.Local
	time.Local = time.FixedZone("UTC+9", 9*60*60)
	defer func() { time.Local = origLocal }()

	p := filepath.Join(t.TempDir(), "session.drops.log")
	// a: 역순(둘째 줄이 더 과거) → 최댓값 1700000000 = 2023-11-14 UTC.
	// b: 자릿수 초과로 int64 변환 실패 → 집계는 되고 병기는 없다(isUnixTS는 통과한다).
	// 빈 줄 → unparsed.
	data := "1700000000\ta\n1600000000\ta\n" +
		"99999999999999999999\tb\n" +
		"\n"
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	total, reasons, last := dropsByReason(p)
	if total != 4 {
		t.Fatalf("total=%d want 4(빈 줄 포함 줄 수 계약)", total)
	}
	if reasons["a"] != 2 || reasons["b"] != 1 || reasons["unparsed"] != 1 {
		t.Fatalf("reasons=%v want a=2,b=1,unparsed=1", reasons)
	}
	if last["a"] != 1700000000 {
		t.Fatalf("last[a]=%d want 1700000000(최댓값 — 마지막 줄이 아니다)", last["a"])
	}
	if _, ok := last["b"]; ok {
		t.Fatalf("last[b]는 없어야 한다(int64 변환 실패): %v", last)
	}
	if _, ok := last["unparsed"]; ok {
		t.Fatalf("last[unparsed]는 없어야 한다: %v", last)
	}
	got := formatDropCount(total, reasons, last)
	want := "4(a=2@2023-11-14,b=1,unparsed=1)"
	if got != want {
		t.Fatalf("formatDropCount=%q want %q", got, want)
	}
	// §2 D71 ②: 빈 로그(total==0)는 병기 없이 "0"이다 — 위 빈 줄은 total>0의 unparsed
	// 케이스이므로 이 경로를 따로 단정한다.
	if got := formatDropCount(0, nil, nil); got != "0" {
		t.Fatalf("formatDropCount(0,nil,nil)=%q want \"0\"", got)
	}
}

// TestHookInstallCodexGateReport — D89 소비자 1·2·3. 기입을 건너뛴 실행이 기입 완료를
// 인쇄하거나 MCP 확정으로 가드 훅을 등록하면 안 된다.
func TestHookInstallCodexGateReport(t *testing.T) {
	home := isolateCodexHome(t)
	cfg := filepath.Join(home, "config.toml")
	writeCodexConfig(t, home, codexGateFixture)
	proj := t.TempDir()
	var buf bytes.Buffer
	if err := runHookInstallCodex(false, false, false, "", proj, "0.17.0", nil, false, &buf); err != nil {
		t.Fatalf("install 실패: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "MCP 관리 테이블 기입 완료") {
		t.Errorf("소비자 3 — 기입을 건너뛰고 완료를 인쇄했다:\n%s", out)
	}
	if !strings.Contains(out, anomalyOutputInvalid.reason()) {
		t.Errorf("사유가 나오지 않았다:\n%s", out)
	}
	// 소비자 1 — 파일이 바뀌지 않았다
	got, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("config.toml 읽기 실패: %v", err)
	}
	if string(got) != codexGateFixture {
		t.Errorf("config.toml이 바뀌었다:\n%s", got)
	}
	// 소비자 1 — **쓰기 분기 자체가 실행되지 않았다**. 위 바이트 단정만으로는 이것을 재지
	// 못한다: 게이트가 Out을 existing으로 되돌리므로 가드를 통째로 지워도 기입 바이트가 같다.
	// 가드 안에는 backupCodexConfig가 함께 있고 그것은 되돌린 Out과 무관하게 파일을 새로
	// 만드므로, .bak의 부재가 그 분기가 돌지 않았다는 관측 가능한 증거다(D84 단일 슬롯).
	if _, sErr := os.Stat(cfg + ".bak"); !os.IsNotExist(sErr) {
		t.Errorf("쓰기 분기가 돌았다 — 기입을 건너뛰었는데 백업이 생겼다(stat err=%v)", sErr)
	}
	// 소비자 2 — 가드 훅이 등록되지 않았다. 훅 파일 경로는 codexHooksPath가 정하므로
	// 그 함수로 얻는다(프로젝트/사용자 갈래가 있어 경로를 손으로 조립하면 어긋난다).
	hp, err := codexHooksPath(false, proj)
	if err != nil {
		t.Fatalf("훅 경로 해석 실패: %v", err)
	}
	hooks, err := os.ReadFile(hp)
	if err != nil {
		t.Fatalf("hooks.json 읽기 실패: %v", err)
	}
	if strings.Contains(string(hooks), "PreToolUse") {
		t.Errorf("MCP 미확정인데 가드 그룹이 등록됐다:\n%s", hooks)
	}
}

// TestHookInstallCodexUnparsableInput — 입력이 파스되지 않는 config.toml에 기입하면서 기입
// 완료만 인쇄하면 그 문장이 거짓이다: Codex는 그 파일의 어떤 설정도 읽지 못하므로 재시작해도
// 반영되지 않는다. 신호는 이미 결과에 실려 있고(D89 부수 결정 ②) 지금까지 doctor [16]만
// 인쇄했는데, **파일을 실제로 바꾸는 것은 이 경로다.**
func TestHookInstallCodexUnparsableInput(t *testing.T) {
	home := isolateCodexHome(t)
	// 같은 테이블 헤더 두 번 — 파서만 거부한다. codexManagedSpans는 우리 두 이름의 중복만
	// 세므로 관리 테이블은 그대로 잡히고, 게이트는 계약상 !InputParses에서 작동하지 않는다.
	src := "[a]\nx = 1\n\n[a]\ny = 2\n\n" + ctrTableFixture
	writeCodexConfig(t, home, src)
	// 픽스처가 실제로 그 갈래인지 먼저 못박는다 — 다른 상태로 흘러가면 아래 단정이 무엇을
	// 쟀는지 알 수 없어진다(기입이 일어나지 않으면 거짓 문면 자체가 성립하지 않는다).
	if v := codexRegistrationVerdict([]byte(src), "0.17.0"); v.InputParses || v.State != mcpWritten || !v.Changed {
		t.Fatalf("픽스처가 '입력 무효 + 기입' 갈래가 아니다: inputParses=%v state=%d changed=%v",
			v.InputParses, v.State, v.Changed)
	}
	var buf bytes.Buffer
	if err := runHookInstallCodex(false, false, false, "", t.TempDir(), "0.17.0", nil, false, &buf); err != nil {
		t.Fatalf("install 실패: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "TOML로 파스되지 않습니다") {
		t.Errorf("입력 파스 실패를 알리지 않는다:\n%s", out)
	}
	// 기입 완료 문면과 **함께** 나가야 한다 — 신호를 내면서 기입 보고를 지우면 이 경로의
	// 기입 정책이 바뀐 것이고 그것은 이 패치의 범위가 아니다(설계 §1.2 — 이미 무효인 파일에
	// 대한 기입 정책은 무변경).
	if !strings.Contains(out, "MCP 관리 테이블 기입 완료") {
		t.Errorf("기입 보고가 사라졌다 — 이 패치는 기입 정책을 바꾸지 않는다:\n%s", out)
	}
}

// TestHookInstallCodexParsableInputQuiet — 위 신호를 무조건 인쇄하는 구현은 정상 설치마다
// 오보를 낸다. 긍정 단정만 두면 그 회귀가 조용히 통과한다.
func TestHookInstallCodexParsableInputQuiet(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, ctrTableFixture)
	var buf bytes.Buffer
	if err := runHookInstallCodex(false, false, false, "", t.TempDir(), "0.17.0", nil, false, &buf); err != nil {
		t.Fatalf("install 실패: %v", err)
	}
	if strings.Contains(buf.String(), "TOML로 파스되지 않습니다") {
		t.Errorf("파스되는 입력에 파스 실패를 인쇄했다:\n%s", buf.String())
	}
}

// TestHookInstallCodexUnconfirmedGuardStays — 설치 결합(D47·D32)은 **등록 방향에만** 걸린다:
// MCP 미확정 실행은 가드를 새로 등록하지 않지만 앞선 설치가 등록한 가드 그룹을 지우지도
// 않는다. 그 상태에서 "가드 등록 보류"와 **의도한** 등록 개수만 인쇄하면 문면이 파일과
// 어긋나고, 사용자는 거부 표면이 없다고 읽는다. 형제 테스트(GateReport)는 갓 만든
// hooks.json만 보므로 "보류"와 "그대로 둠"을 구별하지 못한다.
func TestHookInstallCodexUnconfirmedGuardStays(t *testing.T) {
	home := isolateCodexHome(t)
	proj := t.TempDir()
	writeCodexConfig(t, home, "")
	var buf bytes.Buffer
	if err := runHookInstallCodex(false, false, false, "", proj, "0.17.0", nil, false, &buf); err != nil {
		t.Fatalf("1차 install 실패: %v", err)
	}
	path, pErr := codexHooksPath(false, proj)
	if pErr != nil {
		t.Fatalf("훅 경로 해석 실패: %v", pErr)
	}
	withGuard := len(codexRegistrations) + 1
	if n, _, _ := scanCodexRegisteredHooks(path); n != withGuard {
		t.Fatalf("1차가 가드를 등록하지 않았다 — 전제가 무너졌다: n=%d want %d", n, withGuard)
	}
	writeCodexConfig(t, home, codexGateFixture) // 게이트가 무는 형태 → mcpConfirmed=false
	buf.Reset()
	if err := runHookInstallCodex(false, false, false, "", proj, "0.17.0", nil, false, &buf); err != nil {
		t.Fatalf("2차 install 실패: %v", err)
	}
	// **동작은 그대로다.** 이 패치가 고치는 것은 문면뿐이며, 가드를 지우는 방향은 정상
	// 동작 중인 등록물에서 mcpConfirmed가 거짓이 되는 갈래(점 표기 이탈 등)의 보호를
	// 조용히 끄므로 설계 결정이 필요하다.
	if n, _, _ := scanCodexRegisteredHooks(path); n != withGuard {
		t.Fatalf("가드가 사라졌다 — 이 패치는 동작을 바꾸지 않는다: n=%d want %d", n, withGuard)
	}
	out := buf.String()
	if !strings.Contains(out, fmt.Sprintf("%d개 이벤트 등록 완료", withGuard)) {
		t.Errorf("등록 개수가 파일에 남은 것과 어긋난다(want %d):\n%s", withGuard, out)
	}
	if !strings.Contains(out, "앞선 설치가 등록한 PreToolUse 가드 그룹") {
		t.Errorf("남은 가드를 알리지 않는다:\n%s", out)
	}
}

// TestHookInstallCodexUnconfirmedNoPriorGuard — 앞선 가드가 없으면 그 줄을 내지 않는다.
// 긍정 단정만 두면 무조건 인쇄하는 구현이 통과하고, 가드가 없는 사용자에게 있다고 말한다.
func TestHookInstallCodexUnconfirmedNoPriorGuard(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, codexGateFixture)
	var buf bytes.Buffer
	if err := runHookInstallCodex(false, false, false, "", t.TempDir(), "0.17.0", nil, false, &buf); err != nil {
		t.Fatalf("install 실패: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "앞선 설치가 등록한 PreToolUse 가드 그룹") {
		t.Errorf("없는 가드를 남아 있다고 말했다:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d개 이벤트 등록 완료", len(codexRegistrations))) {
		t.Errorf("등록 개수가 파일에 남은 것과 어긋난다(want %d):\n%s", len(codexRegistrations), out)
	}
}
