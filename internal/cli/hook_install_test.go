package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/store"
)

// TestMain — 이 패키지 테스트 전역 격리: doctor([9] 사용자 범위 검사)·`hook install --user`가
// 실사용자 ~/.claude를 절대 건드리지 않도록 홈을 프로세스 임시 디렉터리로 돌린다(os.UserHomeDir
// 이음새 = Windows USERPROFILE / 그 외 HOME). 사용자 범위에 실제로 기록하는 테스트는 t.Setenv로
// 자기 전용 임시 홈을 덮어써(격리·자동 복원) 이 기본 홈을 오염시키지 않는다.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "ctr-cli-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: 임시 홈 생성 실패:", err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
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

// ① 빈 설정에 install → 4개 이벤트 등록·유효 JSON·PreToolUse matcher "Read|Bash"·timeout 10.
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
	if s.Hooks["PreToolUse"][0].Matcher != "Read|Bash" {
		t.Fatalf("PreToolUse matcher=%q want Read|Bash", s.Hooks["PreToolUse"][0].Matcher)
	}
}

// ①-b D32 업그레이드 재설치(설계 §8 설치 게이트): v0.2 형태 settings(marker 0.2.0 + PreToolUse
// matcher "Read")를 seed → install 재실행 → PreToolUse 관리 그룹 1개·matcher "Read|Bash"·총 4그룹·
// marker 현재 버전으로 갱신(구 matcher 그룹이 잔존하지 않고 대칭 교체된다).
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

	if n, _ := countRegisteredHooks(path); n != 4 {
		t.Fatalf("after upgrade registered=%d want 4 (관리 그룹 중복/누락)", n)
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
	if pre[0].Matcher != "Read|Bash" {
		t.Fatalf("PreToolUse matcher=%q want Read|Bash (구 Read 그룹 미교체): %s", pre[0].Matcher, data)
	}
	if pre[0].Managed != "context-router/0.3.0" {
		t.Fatalf("marker=%q want context-router/0.3.0 (버전 미갱신): %s", pre[0].Managed, data)
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
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		var buf bytes.Buffer
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0"); err != nil {
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
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0"); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		if !strings.Contains(out, "[9] hooks: project=등록됨") {
			t.Fatalf("out missing registered-hooks line:\n%s", out)
		}
		if !strings.Contains(out, "[12] drops: store-root=2(a=1,b=1) worktree=3(x=1,y=1,z=1) total=5") {
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
	total, reasons := dropsByReason(path)
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0"); err != nil {
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

// D33a 마커 일치: 현재 버전으로 install한 뒤 같은 버전 바이너리로 doctor를 돌리면 [9]가
// "marker <v>"만 표기하고 불일치 경고(≠)를 내지 않는다.
func TestDoctor_HookMarkerVersionMatch(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	var iout bytes.Buffer
	if err := runHookInstall(nil, storeRoot, "", false, projectRoot, "9.9.9", &iout); err != nil {
		t.Fatalf("install: %v", err)
	}
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "9.9.9"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "project=등록됨(4개, marker 9.9.9)") {
		t.Fatalf("out missing matched marker version:\n%s", out)
	}
	if strings.Contains(out, "≠") {
		t.Fatalf("out must not warn mismatch on matching versions:\n%s", out)
	}
}

// D33a 마커 불일치: 구버전 마커로 install(= 구버전 마커 seed)한 뒤 신버전 바이너리로 doctor를
// 돌리면 [9]가 "marker <old>≠<new> — hook install 재실행"으로 재설치를 안내한다.
func TestDoctor_HookMarkerVersionMismatch(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	var iout bytes.Buffer
	if err := runHookInstall(nil, storeRoot, "", false, projectRoot, "0.1.0", &iout); err != nil {
		t.Fatalf("install(old): %v", err)
	}
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.3.0"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "project=등록됨(4개, marker 0.1.0≠0.3.0 — hook install 재실행)") {
		t.Fatalf("out missing marker mismatch warning:\n%s", out)
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
