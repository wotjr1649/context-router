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
	"runtime"
	"slices"
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

// seedHookTimeoutSec — 옛 설치기가 등록물에 적던 timeout(초). 제거 판정은 이 값을 보지
// 않으므로 프로덕션 상수로 둘 이유가 사라졌고, 픽스처가 재현하려는 옛 형태의 일부로 여기 산다.
const seedHookTimeoutSec = 10

// seedClaudeHooks — 옛 설치기가 `.claude/settings.json`에 남긴 형태를 그대로 조립한다
// (hookRegistrations의 여섯 이벤트 각각에 우리 그룹 하나, __ctrManaged=marker).
// 등록 코드가 v0.19에서 사라졌으므로(D96) 제거 방향을 재는 픽스처는 테스트가 스스로 만든다 —
// 프로덕션 조립기를 되살려 픽스처를 만들면 그 조립기가 테스트만을 소비처로 삼아 살아남는다.
func seedClaudeHooks(marker string) []byte {
	groups := map[string][]any{}
	for _, event := range hookRegistrations {
		matcher := ""
		if event == "PreToolUse" {
			matcher = "Read|Bash|PowerShell|Grep"
		}
		groups[event] = []any{map[string]any{
			"matcher": matcher,
			"hooks": []any{map[string]any{
				"type": "command", "command": "context-router hook", "timeout": seedHookTimeoutSec,
			}},
			"__ctrManaged": marker,
		}}
	}
	b, err := json.MarshalIndent(map[string]any{"hooks": groups}, "", "  ")
	if err != nil {
		panic(err) // 리터럴 맵이라 실패 경로가 없다
	}
	return append(b, '\n')
}

// writeSeedClaudeHooks — seedClaudeHooks를 대상 스코프의 settings.json에 놓는다.
func writeSeedClaudeHooks(t *testing.T, user bool, projectRoot, marker string) string {
	t.Helper()
	path, err := hookSettingsPath(user, projectRoot)
	if err != nil {
		t.Fatalf("hookSettingsPath: %v", err)
	}
	if err := atomicWriteFile(path, seedClaudeHooks(marker)); err != nil {
		t.Fatalf("seed 쓰기: %v", err)
	}
	return path
}

// seedCodexHooks — 같은 취지의 Codex `hooks.json` 픽스처. 옛 설치기는 캡처 2이벤트에
// D47 가드(PreToolUse matcher Bash)를 더해 최대 세 그룹을 남겼다 — 소유 표식은
// statusMessage가 겸한다(§11.1 G3, 미지 필드 금지).
func seedCodexHooks(marker string, withGuard bool) []byte {
	group := func(matcher string) any {
		return map[string]any{
			"matcher": matcher,
			"hooks": []any{map[string]any{
				"type": "command", "command": "context-router codex-hook",
				"timeout": seedHookTimeoutSec, "statusMessage": marker,
			}},
		}
	}
	groups := map[string][]any{
		"SessionStart": {group("")},
		"PostToolUse":  {group("")},
	}
	if withGuard {
		groups["PreToolUse"] = []any{group("Bash")}
	}
	b, err := json.MarshalIndent(map[string]any{"hooks": groups}, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// ③ 타 도구 훅 항목 + 미지 키 + 우리 그룹이 함께 있는 파일에서 uninstall이 **우리 것만** 지운다.
// 미지 최상위 키·타 도구 그룹·관리 대상 아닌 이벤트가 전부 원형 보존돼야 한다.
func TestHookUninstall_PreservesUnknownAndOtherTools(t *testing.T) {
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
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": "context-router"}
    ],
    "PostToolUse": [
      {"matcher": "Write", "hooks": [{"type": "command", "command": "other-tool run", "timeout": 5}]},
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": "context-router"}
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

	assertPreserved("before uninstall")
	if n, _ := countRegisteredHooks(path); n != 2 {
		t.Fatalf("before uninstall registered=%d want 2", n)
	}

	var out bytes.Buffer
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
func TestHookUninstall_SymmetricPreservesLookalikes(t *testing.T) {
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

// ⑤ 원자성 — 제거 후 임시 파일(.ctr-settings-*.tmp) 잔존물 부재. 두 번째 실행으로 Claude
// 갈래의 **파일 안정성**도 함께 잰다(리뷰 I3): 재실행이 파일을 흔들면(키 정렬·빈 컨테이너 처리
// drift) 안 된다. 파일 수준에서 재는 이유는 runHookUninstall이 순수 함수가 아니라 읽기·병합·
// 쓰기 셋을 엮기 때문이다.
//
// **재기준선(F1)**: 2차 실행은 이제 아예 쓰지 않으므로(지울 자기 그룹이 없다) 파일 비교만으로는
// 병합기의 바이트 멱등을 더는 재지 못한다 — 안 쓰는 코드도 그 단정을 통과한다. 두 자리를 나눠
// 든다: 여기서는 2차가 쓰지 않는다는 것(문면 + 바이트 동일)을, 병합기의 바이트 멱등은 아래
// removeHookSettings 직접 호출이 잰다.
func TestHookUninstall_AtomicWriteNoTempLeftover(t *testing.T) {
	projectRoot := t.TempDir()
	writeSeedClaudeHooks(t, false, projectRoot, hookBinaryName)
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	var out bytes.Buffer
	if err := runHookUninstall(nil, projectRoot, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	once, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("1차 read: %v", err)
	}
	if !strings.Contains(out.String(), "제거 완료") {
		t.Fatalf("1차 제거 완료 문면 누락: %q", out.String())
	}
	var second bytes.Buffer
	if err := runHookUninstall(nil, projectRoot, &second); err != nil {
		t.Fatalf("2차 uninstall: %v", err)
	}
	twice, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("2차 read: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("Claude 갈래 제거가 멱등이 아니다:\n1차=%s\n2차=%s", once, twice)
	}
	if !strings.Contains(second.String(), "제거할 항목 없음") {
		t.Fatalf("2차가 지울 것이 없다고 알리지 않았다: %q", second.String())
	}
	// 병합기 자체의 바이트 멱등 — f(f(x)) == f(x). Codex 형제
	// (TestRemoveCodexHooksIdempotentBytes)가 이미 재던 것을 Claude 갈래에도 둔다.
	mergedOnce, removedOnce, err := removeHookSettings(seedClaudeHooks(hookBinaryName))
	if err != nil || !removedOnce {
		t.Fatalf("1차 병합: removed=%v err=%v", removedOnce, err)
	}
	mergedTwice, removedTwice, err := removeHookSettings(mergedOnce)
	if err != nil || removedTwice {
		t.Fatalf("2차 병합: removed=%v err=%v", removedTwice, err)
	}
	if !bytes.Equal(mergedOnce, mergedTwice) {
		t.Fatalf("병합기 바이트 멱등 위반:\n1차=%s\n2차=%s", mergedOnce, mergedTwice)
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

// TestHookUninstall_NoFileCreatesNothing — D96 계약 1의 중심 주장: **이 프로그램은 호스트
// 설정 파일을 만들지 않는다.** Codex 형제(TestRunHookUninstallCodexNoFile)가 그 갈래를
// 지키고 있었고 Claude 갈래에는 단정이 없었다(리뷰 I1) — 그 자리를 채운다. 파일뿐 아니라
// `.claude` 디렉터리 자체가 생기지 않는지도 본다: atomicWriteFile이 MkdirAll을 하므로
// 조기 반환이 사라지면 빈 디렉터리부터 남는다.
func TestHookUninstall_NoFileCreatesNothing(t *testing.T) {
	projectRoot := t.TempDir()
	var out bytes.Buffer
	if err := runHookUninstall(nil, projectRoot, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !strings.Contains(out.String(), "설정 파일 없음") {
		t.Fatalf("no-op 안내 누락: %q", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(projectRoot, ".claude")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("uninstall이 없던 .claude 디렉터리를 만들었다: %v", statErr)
	}
	entries, err := os.ReadDir(projectRoot)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("uninstall이 없던 파일을 만들었다: %v", entries)
	}
}

// TestHookUninstall_UnparsableSettingsErrors — 해석되지 않는 settings.json에서 제거는 문면을
// 내고 **오류를 반환한다**(리뷰 I2). 종료코드가 실패를 반영해야 사용자가 훅이 그대로 남은 것을
// 안다 — 오류를 삼키고 exit 0을 내는 회귀는 이 단정 없이는 스위트 전체를 통과한다.
// 파일이 손대지지 않은 것도 함께 본다: 해석하지 못한 파일에 쓰지 않는 것이 계약이다.
func TestHookUninstall_UnparsableSettingsErrors(t *testing.T) {
	projectRoot := t.TempDir()
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const broken = "{\"hooks\": {\n" // 닫히지 않은 JSON
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall(nil, projectRoot, &out); err == nil {
		t.Fatalf("훅 설정 정리 실패가 오류로 반환되지 않았다 — 종료코드가 실패를 반영해야 한다: %q", out.String())
	}
	if !strings.Contains(out.String(), "훅 설정 정리 실패") {
		t.Fatalf("실패 문면 누락: %q", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != broken {
		t.Fatalf("해석하지 못한 파일을 고쳐 놓았다:\n%s", after)
	}
}

// ⑥-b JSON `null`·`{"hooks":null}` 설정(구문상 유효)에서 제거가 패닉 없이 돈다
// (최종 리뷰 C5 — Unmarshal이 null을 nil 맵으로 설정해 할당 시 패닉하던 경로).
// 빈 컨테이너 정리 계약도 함께 잰다: 지울 것이 없으면 hooks 키를 만들지 않는다.
func TestRemoveHookSettings_NullTolerant(t *testing.T) {
	for _, existing := range []string{"null", `{"hooks":null}`} {
		out, removed, err := removeHookSettings([]byte(existing))
		if err != nil {
			t.Fatalf("existing=%q 제거 err: %v", existing, err)
		}
		// 재기준선(F1): 지울 자기 그룹이 없는 입력이므로 removed는 거짓이다 — 호출자는 쓰지 않는다.
		if removed {
			t.Fatalf("existing=%q: 지운 것이 없는데 removed=true", existing)
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(out, &settings); err != nil {
			t.Fatalf("existing=%q 출력이 유효 JSON 아님: %v", existing, err)
		}
		if _, ok := settings["hooks"]; ok {
			t.Fatalf("existing=%q 빈 hooks 컨테이너가 남았다: %s", existing, out)
		}
	}
}

// ⑥ doctor: 훅 등록/미등록 상태 문구 + drops 두 위치 합산 + 항목 [1]~[13] 오름차순.
func TestDoctor_HookItemsAndAscendingOrder(t *testing.T) {
	t.Run("unregistered", func(t *testing.T) {
		isolateCodexHome(t)
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		var buf bytes.Buffer
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0"); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		for _, want := range []string{
			// 재기준선(최종 리뷰): 그룹이 없는 상태의 문면은 "미등록"이 아니라 "옛 그룹 없음"이다 —
			// 플러그인 매니페스트로만 설치한 사용자에게 이것이 정상 상태이기 때문이다.
			"[9] hooks: project=옛 그룹 없음",
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
		writeSeedClaudeHooks(t, false, projectRoot, hookBinaryName)
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

// ⑧ F5: 사용자 스코프에만 우리 훅 그룹이 있는 상태에서 doctor가 그것을 인식하고 프로젝트
// 범위는 미등록으로 보고해야 한다(프로젝트-only 검사 회귀 방지). 사용자 홈은 t.Setenv로 자기
// 전용 TempDir로 덮어써 실사용자 ~/.claude를 건드리지 않는다(TestMain 기본 홈 격리 위에서
// 추가 격리·자동 복원).
func TestDoctor_UserScopeHookRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir 이음새
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()

	writeSeedClaudeHooks(t, true, projectRoot, hookBinaryName)
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("user-scope settings not seeded under temp home: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.1.0"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "user=등록됨") {
		t.Fatalf("doctor must report user-scope registration:\n%s", out)
	}
	if !strings.Contains(out, "project=옛 그룹 없음") {
		t.Fatalf("doctor must report project scope as free of legacy groups:\n%s", out)
	}
	assertDoctorAscending(t, out)
}

// TestDoctorVersionlessHookMarker — D82 doctor 경고(§2-9). 무버전 마커가 있는 상태에서
// 훅 스코프는 버전 불일치 경고를 내지 **않는다**. 같은 픽스처에서 그 훅 등록이 **소유로
// 인식되는지**를 등록 개수로 함께 단정한다 — 경고만 보면 무버전 마커가 소유 판정에서 아예
// 탈락해 개수가 0이 된 경우도 "경고 없음"으로 통과한다.
// 불일치 부재는 **훅 스코프 줄([9]·[16] codex hooks:)에 한정**한다 — [20]은 MCP 등록물의
// 버전을 비교하는 자리라 같은 픽스처에서 '≠'를 정당하게 인쇄한다.
func TestDoctorVersionlessHookMarker(t *testing.T) {
	isolateCodexHome(t)
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	writeSeedClaudeHooks(t, false, projectRoot, hookBinaryName)
	// 바이너리 버전과 무관하게 훅 스코프는 흔들리지 않는다.
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.16.0"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "project=등록됨(6개 — hook uninstall로 옛 그룹을 지우고 플러그인 설치로 옮기세요 — 두 벌이 함께 있으면 같은 포착이 두 번 일어납니다)") {
		t.Fatalf("무버전 마커가 소유로 인식되지 않았다(개수 0이면 경고 없음도 공허하다):\n%s", out)
	}
	// '≠' 부재는 **훅 스코프 줄에 한정**해 본다 — [20]은 MCP 등록물의 버전을 비교하므로
	// 다른 픽스처에서 정당하게 '≠'를 인쇄한다. 출력 전체를 보면 그 줄이 이 단정을 깨뜨린다.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "[9] hooks:") && !strings.HasPrefix(line, "[16] codex hooks:") {
			continue
		}
		if strings.Contains(line, "≠") {
			t.Fatalf("훅 스코프가 버전 불일치 경고를 냈다: %s", line)
		}
	}
}

// TestDoctorVersionedHookMarkerNextStep — 버전 있는 마커(D82 이전 설치본)에는 다음 걸음이
// 붙는다. 그 문면이 더는 존재하지 않는 등록 명령을 가리키면 안 된다(D96 — `hook install`은
// 등록하지 않는다). uninstall과 플러그인 설치가 그 자리를 대신한다.
func TestDoctorVersionedHookMarkerNextStep(t *testing.T) {
	isolateCodexHome(t)
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	writeSeedClaudeHooks(t, false, projectRoot, hookMarkerPrefix()+"0.14.0")
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.19.0"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "hook uninstall로 옛 그룹을 지우고") {
		t.Fatalf("버전 불일치 다음 걸음이 없다:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[9] hooks:") && strings.Contains(line, "hook install") {
			t.Fatalf("더는 등록하지 않는 명령을 다음 걸음으로 안내한다: %s", line)
		}
	}
}

// TestLegacyVersionedMarkerStillOwned — D82 하위 호환(§2-10). 구 마커(context-router/0.14.0)가
// 소유로 인정되고 대칭 제거된다. uninstall이 구·신 마커 양쪽을 지운다.
func TestLegacyVersionedMarkerStillOwned(t *testing.T) {
	for _, marker := range []string{"context-router/0.14.0", "context-router"} {
		proj := t.TempDir()
		path := writeSeedClaudeHooks(t, false, proj, marker)
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
		if err := runHook(context.Background(), nil, storeRoot, cwd, "test", io.Discard); err != nil {
			t.Fatalf("sessionstart: %v", err)
		}
		// ② >MIN PostToolUse — args(--no-shadow 유무)에 따라 Shadow on/off.
		setStdin(t, hookRunPayload(t, map[string]any{
			"hook_event_name": "PostToolUse", "cwd": cwd, "tool_name": "Bash",
			"tool_response": map[string]any{"stdout": strings.Repeat("a", 500), "stderr": ""},
		}))
		if err := runHook(context.Background(), args, storeRoot, cwd, "test", io.Discard); err != nil {
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

// D52 — Codex hooks.json 자기 그룹 marker 버전 추출(v0.9 §0): isOurCodexGroup은 bool만
// 반환하므로 신설(적대 검수 P1). 파일 부재는 (0,"",nil) — 미등록 정보 분기.
func TestScanCodexRegisteredHooks(t *testing.T) {
	// ① 부재: count=0 marker="" err=nil
	if n, m, err := scanCodexRegisteredHooks(filepath.Join(t.TempDir(), "hooks.json")); n != 0 || m != "" || err != nil {
		t.Fatalf("부재: n=%d m=%q err=%v want 0/\"\"/nil", n, m, err)
	}
	// ② 자기 그룹 존재(가드 포함 3그룹) → count>0, marker 추출.
	const wantMarker = "0.9.0"
	self := seedCodexHooks(hookMarkerPrefix()+wantMarker, true)
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

// 제거가 타 그룹·미지 최상위 키를 보존하고 우리 세 이벤트만 비운다.
func TestRemoveCodexHooksPreservesForeign(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[` +
		`{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10,"statusMessage":"policy"}]},` +
		`{"matcher":"Bash","hooks":[{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"context-router"}]}` +
		`],"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"context-router"}]}]},"otherTop":1}`)
	out, removed, err := removeCodexHooks(existing)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !removed { // 재기준선(F1): 자기 그룹 둘을 지웠으므로 참 — 호출자가 쓰는 갈래다
		t.Fatalf("자기 그룹을 지웠는데 removed=false: %s", out)
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
	if len(hooks["PreToolUse"]) != 1 || !strings.Contains(string(hooks["PreToolUse"][0]), "policy.ps1") {
		t.Fatalf("타 그룹 보존 실패: %v", hooks["PreToolUse"])
	}
	// 우리 그룹만 있던 이벤트는 빈 컨테이너 정리로 키째 사라진다.
	if _, ok := hooks["SessionStart"]; ok {
		t.Fatalf("빈 이벤트 키가 남았다: %s", out)
	}
	if strings.Contains(string(out), "codex-hook") {
		t.Fatalf("자기 항목 잔존: %s", out)
	}
}

// 제거 멱등: f(f(x)) == f(x) 바이트 동일(중복·순서 drift 검출). 타 그룹은 두 번 다 보존된다.
func TestRemoveCodexHooksIdempotentBytes(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[` +
		`{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10}]},` +
		`{"matcher":"Bash","hooks":[{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"context-router"}]}]}}`)
	once, removed, err := removeCodexHooks(existing)
	if err != nil {
		t.Fatalf("1차: %v", err)
	}
	if !removed {
		t.Fatalf("1차에서 자기 그룹을 지웠는데 removed=false: %s", once)
	}
	twice, removedAgain, err := removeCodexHooks(once)
	if err != nil {
		t.Fatalf("2차: %v", err)
	}
	// 재기준선(F1): 2차에는 지울 것이 없다 — 멱등의 다른 얼굴이고, 이것이 곧 2차 쓰기가
	// 일어나지 않는 근거다.
	if removedAgain {
		t.Fatalf("2차에 지울 것이 없는데 removed=true: %s", twice)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("멱등 위반:\n1차=%s\n2차=%s", once, twice)
	}
	if !strings.Contains(string(twice), "policy.ps1") {
		t.Fatalf("타 그룹 소실: %s", twice)
	}
}

// F4 — 혼합 그룹(자기 항목 + 사용자 항목 동거)은 불가침: 제거가 그 그룹을 건드리지 않는다
// (파손 금지 > 제거 완전성 — 혼합 그룹의 자기 잔존 항목 정리는 사용자 /hooks 몫).
func TestRemoveCodexHooksMixedGroupUntouched(t *testing.T) {
	mixed := []byte(`{"hooks":{"PostToolUse":[{"matcher":"","hooks":[` +
		`{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"context-router/0.3.9"},` +
		`{"type":"command","command":"pwsh -File user.ps1","timeout":10,"statusMessage":"user"}]}]}}`)
	out, removed, err := removeCodexHooks(mixed)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	// 재기준선(F1): 혼합 그룹은 불가침이므로 지운 것이 없다 — 그러니 이 파일은 쓰이지도 않는다.
	if removed {
		t.Fatalf("혼합 그룹만 있는데 removed=true: %s", out)
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
	if len(hooks["PostToolUse"]) != 1 {
		t.Fatalf("PostToolUse 그룹 수=%d want 1(혼합 보존)", len(hooks["PostToolUse"]))
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

// e2e: hook uninstall --codex run 분기 — 옛 등록물에서 자기 그룹만 제거, 선존 외래 그룹
// 보존, 제거 완료 안내 출력. **config.toml은 손대지 않는다**(D96 계약 1)를 함께 잰다.
func TestRunHookUninstallCodex(t *testing.T) {
	home := isolateCodexHome(t) // config.toml은 스코프 무관 CODEX_HOME/홈 — 공유 임시 홈 오염 차단
	root := t.TempDir()
	const cfgSrc = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	writeCodexConfig(t, home, cfgSrc)
	// 옛 설치기가 남긴 형태 + 선존 외래 그룹.
	seeded := seedCodexHooks(hookBinaryName, true)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(seeded, &doc); err != nil {
		t.Fatalf("seed 파싱: %v", err)
	}
	var events map[string][]json.RawMessage
	if err := json.Unmarshal(doc["hooks"], &events); err != nil {
		t.Fatalf("seed hooks 파싱: %v", err)
	}
	events["PreToolUse"] = append(events["PreToolUse"],
		json.RawMessage(`{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10}]}`))
	doc["hooks"], _ = json.Marshal(events)
	merged, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("seed 재직렬화: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codex", "hooks.json"), merged, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
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
	// D96 계약 1 — uninstall은 config.toml을 바이트 하나도 바꾸지 않고 .bak도 남기지 않는다.
	cfgAfter, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml 읽기: %v", err)
	}
	if string(cfgAfter) != cfgSrc {
		t.Fatalf("uninstall이 config.toml을 바꿨다:\n%s", cfgAfter)
	}
	if _, statErr := os.Stat(filepath.Join(home, "config.toml.bak")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("uninstall이 config.toml.bak을 남겼다: %v", statErr)
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

// D47 가드 그룹 — 제거는 캡처 2이벤트뿐 아니라 PreToolUse(matcher Bash) 가드까지 소거한다.
// 옛 설치기가 그 그룹을 MCP 확정 시에만 등록했으므로, 제거가 그 이벤트를 보지 않으면 가드가
// 파일에 영구 잔존한다. 빈 컨테이너 정리로 hooks 키까지 사라지는 것도 함께 잰다.
func TestRemoveCodexHooksClearsGuardGroup(t *testing.T) {
	seeded := seedCodexHooks(hookBinaryName, true)
	if !strings.Contains(string(seeded), `"PreToolUse"`) || !strings.Contains(string(seeded), `"matcher": "Bash"`) {
		t.Fatalf("픽스처에 가드 그룹이 없다:\n%s", seeded)
	}
	out, removed, err := removeCodexHooks(seeded)
	if err != nil {
		t.Fatal(err)
	}
	if !removed { // 재기준선(F1): 가드 포함 자기 그룹 셋을 지웠으므로 참
		t.Fatalf("가드 포함 자기 그룹을 지웠는데 removed=false:\n%s", out)
	}
	if strings.Contains(string(out), "codex-hook") || strings.Contains(string(out), `"PreToolUse"`) {
		t.Fatalf("가드 포함 자기 그룹 잔존:\n%s", out)
	}
	if strings.Contains(string(out), `"hooks"`) {
		t.Fatalf("빈 hooks 컨테이너가 남았다:\n%s", out)
	}
	// 멱등: f(f(x)) == f(x) 바이트 동일
	again, removedAgain, err := removeCodexHooks(out)
	if err != nil {
		t.Fatal(err)
	}
	if removedAgain {
		t.Fatalf("2차에 지울 것이 없는데 removed=true:\n%s", again)
	}
	if !bytes.Equal(out, again) {
		t.Fatalf("제거 멱등 위반:\n1차=%s\n2차=%s", out, again)
	}
}

// TestHookInstallGuidanceWritesNothing — D96·D97. `hook install`은 이름 없는 오류로 떨어지지
// 않는다: 안내를 내고 비-0으로 끝나며 **어떤 파일도 만들지 않는다**(디렉터리 스냅숏 비교).
// 안내 문면이 hostSnippet과 갈라지지 않는지도 잰다 — 같은 상수(hostInstallSteps)를 읽는다.
func TestHookInstallGuidanceWritesNothing(t *testing.T) {
	home := isolateCodexHome(t)
	proj := t.TempDir()
	snapshot := func(dir string) []string {
		t.Helper()
		var names []string
		if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			names = append(names, p)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
		return names
	}
	beforeProj, beforeHome := snapshot(proj), snapshot(home)

	var out bytes.Buffer
	err := runHook(context.Background(), []string{"install"}, t.TempDir(), proj, "0.19.0", &out)
	if err == nil {
		t.Fatalf("hook install이 비-0으로 끝나지 않았다: %s", out.String())
	}
	if !slices.Equal(beforeProj, snapshot(proj)) {
		t.Fatalf("hook install이 프로젝트에 파일을 만들었다: %v", snapshot(proj))
	}
	if !slices.Equal(beforeHome, snapshot(home)) {
		t.Fatalf("hook install이 CODEX_HOME에 파일을 만들었다: %v", snapshot(home))
	}
	// 0번 걸음(옛 등록물 제거)이 안내에 실린다 — A⑧의 무성 폐기를 막는 걸음이다.
	if !strings.Contains(out.String(), "옛 등록물을 먼저 지운다") {
		t.Fatalf("0번 걸음 안내가 없다:\n%s", out.String())
	}
	if !strings.Contains(out.String(), hostInstallSteps) {
		t.Fatalf("안내가 hostSnippet과 같은 절차 상수를 쓰지 않는다:\n%s", out.String())
	}
	for _, b := range bannedHortatoryVocab {
		if strings.Contains(out.String(), b) {
			t.Errorf("hook install 안내에 금지 어휘 %q가 있다", b)
		}
	}
	// --user·--codex 같은 옛 플래그도 같은 안내로 수렴한다(플래그별 갈래를 남기지 않는다).
	var out2 bytes.Buffer
	if err := runHook(context.Background(), []string{"install", "--codex", "--user"}, t.TempDir(), proj, "0.19.0", &out2); err == nil {
		t.Fatalf("플래그 있는 hook install이 비-0으로 끝나지 않았다: %s", out2.String())
	}
	if out2.String() != out.String() {
		t.Fatalf("플래그에 따라 안내가 갈린다:\n%s\n---\n%s", out.String(), out2.String())
	}
}

// foreignOnlyClaudeSettings — 우리 항목이 하나도 없는 손으로 쓴 settings.json. 재직렬화가
// 바이트 중립이 아님을 재려고 일부러 세 가지를 넣는다: 최상위 키가 사전순이 아니고(model이
// $schema보다 앞), 들여쓰기가 4칸이며(MarshalIndent는 2칸), 값에 `&`가 있다(encoding/json은
// 기본으로 `&`로 이스케이프한다 `[실측]` — 아래 테스트가 그 셋을 한꺼번에 잡는다).
const foreignOnlyClaudeSettings = `{
    "model": "opus",
    "$schema": "https://example/schema.json?a=1&b=2",
    "hooks": {
        "PostToolUse": [
            {"matcher": "Write", "hooks": [{"type": "command", "command": "other-tool run", "timeout": 5}]}
        ]
    }
}
`

// TestHookUninstall_NoOwnedGroupsLeavesFileUntouched — F1(적대 검토). 우리 항목이 하나도 없으면
// 쓰지 않는다. 재기록은 바이트 중립이 아니어서(키 정렬·유니코드 이스케이프·들여쓰기) 남의
// 서버만 담긴 손으로 쓴 파일을 우리가 고쳐 놓게 된다. 삭제된 `.mcp.json` 제거 경로가 이 가드를
// 그대로 들고 있었고 훅 경로에는 없었다.
//
// 문면도 함께 잰다 — 하지 않은 일을 했다고 말하지 않는다. 바이트만 재면 "제거 완료"를 내면서
// 아무것도 지우지 않은 실행이 초록으로 통과한다.
func TestHookUninstall_NoOwnedGroupsLeavesFileUntouched(t *testing.T) {
	projectRoot := t.TempDir()
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(foreignOnlyClaudeSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall(nil, projectRoot, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != foreignOnlyClaudeSettings {
		t.Fatalf("우리 항목이 없는 파일을 고쳐 놓았다:\n%s", after)
	}
	if strings.Contains(out.String(), "제거 완료") {
		t.Fatalf("지운 것이 없는데 제거 완료를 알렸다: %q", out.String())
	}
	if !strings.Contains(out.String(), "제거할 항목 없음") {
		t.Fatalf("no-op 문면 누락: %q", out.String())
	}
}

// foreignOnlyCodexHooks — 같은 취지의 Codex hooks.json 픽스처(F1의 Codex 갈래).
const foreignOnlyCodexHooks = `{
    "otherTop": "a&b",
    "hooks": {
        "PreToolUse": [
            {"matcher": "Bash", "hooks": [{"type": "command", "command": "pwsh -File policy.ps1", "timeout": 10}]}
        ]
    }
}
`

// TestRunHookUninstallCodex_NoOwnedGroupsLeavesFileUntouched — F1의 Codex 갈래. 두 호스트
// 경로가 같은 가드를 든다.
func TestRunHookUninstallCodex_NoOwnedGroupsLeavesFileUntouched(t *testing.T) {
	isolateCodexHome(t)
	root := t.TempDir()
	path := filepath.Join(root, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(foreignOnlyCodexHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall([]string{"--codex"}, root, &out); err != nil {
		t.Fatalf("uninstall --codex: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != foreignOnlyCodexHooks {
		t.Fatalf("우리 항목이 없는 파일을 고쳐 놓았다:\n%s", after)
	}
	if strings.Contains(out.String(), "제거 완료") {
		t.Fatalf("지운 것이 없는데 제거 완료를 알렸다: %q", out.String())
	}
	if !strings.Contains(out.String(), "제거할 항목 없음") {
		t.Fatalf("no-op 문면 누락: %q", out.String())
	}
}

// TestHookUninstall_MixedGroupUntouched — F2(적대 검토). 마커가 붙은 그룹이라도 사용자가 자기
// 항목을 더해 놓았으면 통째로 지우지 않는다(파손 금지 > 멱등 완전성). Codex 형제
// (TestRemoveCodexHooksMixedGroupUntouched)가 이미 재던 계약을 Claude 갈래에 맞춘다.
//
// 순수 자기 그룹(SessionStart)을 함께 두어 **제거 자체는 일어나는** 실행에서 재므로 F1의
// 조기 반환이 이 단정을 대신 통과시키지 않는다.
func TestHookUninstall_MixedGroupUntouched(t *testing.T) {
	projectRoot := t.TempDir()
	path := filepath.Join(projectRoot, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "context-router hook", "timeout": 10}], "__ctrManaged": "context-router"}
    ],
    "PostToolUse": [
      {"matcher": "", "hooks": [
        {"type": "command", "command": "context-router hook", "timeout": 10},
        {"type": "command", "command": "user-tool run", "timeout": 5}
      ], "__ctrManaged": "context-router"}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall(nil, projectRoot, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(after), "user-tool run") {
		t.Fatalf("혼합 그룹의 사용자 항목이 삭제됐다:\n%s", after)
	}
	var s struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(after, &s); err != nil {
		t.Fatalf("out 파싱: %v\n%s", err, after)
	}
	if len(s.Hooks["PostToolUse"]) != 1 {
		t.Fatalf("PostToolUse 그룹 수=%d want 1(혼합 그룹 보존): %s", len(s.Hooks["PostToolUse"]), after)
	}
	// 받아들인 거래: 혼합 그룹에 남은 우리 항목은 사용자 /hooks 몫이다.
	if !strings.Contains(string(s.Hooks["PostToolUse"][0]), "context-router hook") {
		t.Fatalf("혼합 그룹이 변형됐다(불가침 위반): %s", after)
	}
	// 순수 자기 그룹은 그대로 사라진다 — 혼합 보존이 제거 자체를 막지 않는다.
	if _, ok := s.Hooks["SessionStart"]; ok {
		t.Fatalf("순수 자기 그룹이 남았다: %s", after)
	}
}

// TestIsOurHookGroupEdges — 소유 판정 직접 에지(Codex 형제 TestIsOurCodexGroupEdges의 짝).
// 전건 판정(F2): 마커가 소유 값이고 **모든** 훅 항목의 command 토큰이 일치할 때만 자기 그룹.
func TestIsOurHookGroupEdges(t *testing.T) {
	ours := `{"type":"command","command":"context-router hook","timeout":10}`
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"비JSON", `not-json`, false},
		{"마커 없음", `{"matcher":"","hooks":[` + ours + `]}`, false},
		{"빈 그룹", `{"matcher":"","hooks":[],"__ctrManaged":"context-router"}`, false},
		{"전건 자기 항목", `{"matcher":"","hooks":[` + ours + `],"__ctrManaged":"context-router"}`, true},
		{"버전 마커", `{"matcher":"","hooks":[` + ours + `],"__ctrManaged":"context-router/0.14.0"}`, true},
		{"후행 플래그 허용", `{"matcher":"","hooks":[{"type":"command","command":"context-router hook --no-shadow"}],"__ctrManaged":"context-router"}`, true},
		{"혼합(자기+외래)", `{"matcher":"","hooks":[` + ours + `,{"type":"command","command":"user-tool run"}],"__ctrManaged":"context-router"}`, false},
		{"접두 닮은 명령(hook-wrapper)", `{"matcher":"","hooks":[{"type":"command","command":"context-router hook-wrapper"}],"__ctrManaged":"context-router"}`, false},
		{"마커 값 불일치", `{"matcher":"","hooks":[` + ours + `],"__ctrManaged":"other/0.1.0"}`, false},
	}
	for _, c := range cases {
		if got := isOurHookGroup(json.RawMessage(c.raw)); got != c.want {
			t.Fatalf("%s: isOurHookGroup=%v want %v", c.name, got, c.want)
		}
	}
}

// TestAtomicWriteFile_SymlinkDestination — F7(적대 검토). 이 대상(dotfiles 저장소를 쓰는
// 사용자)에게 `settings.json`이 심링크인 경우는 흔하다. rename은 링크를 일반 파일로 갈아치우고
// 원래 대상 파일은 옛 내용을 든 채 남는다 — 호스트가 읽는 파일과 사용자가 관리하는 파일이
// 갈라진다. 목적지를 먼저 풀어 임시 파일 위치와 rename 대상을 링크 대상 쪽에서 고른다.
//
// 링크와 대상을 **다른 디렉터리**에 두어 임시 파일이 어느 쪽에 생겼는지까지 잰다(같은 볼륨
// rename만 원자적이므로 위치 선택이 계약의 일부다). 심링크 생성 권한이 없는 호스트에서는
// 잴 수 없다 — Windows는 개발자 모드나 관리자 권한이 필요하다 `[실측]`.
func TestAtomicWriteFile_SymlinkDestination(t *testing.T) {
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real-settings.json")
	if err := os.WriteFile(target, []byte("{\"old\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("이 호스트에서 심링크를 만들 수 없다: %v", err)
	}
	want := []byte("{\"new\":true}\n")
	if err := atomicWriteFile(link, want); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("심링크가 일반 파일로 대체됐다: mode=%v", fi.Mode())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("링크 대상 읽기: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("링크 대상이 갱신되지 않았다: %s", got)
	}
	assertNoTempLeftover(t, linkDir)
	assertNoTempLeftover(t, targetDir)

	// 목적지 부재 — EvalSymlinks가 오류를 내는 경로다. 그대로 새 파일을 만든다.
	fresh := filepath.Join(linkDir, "fresh.json")
	if err := atomicWriteFile(fresh, want); err != nil {
		t.Fatalf("목적지 부재 쓰기: %v", err)
	}
	if got, err := os.ReadFile(fresh); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("목적지 부재 결과=%s err=%v", got, err)
	}
}

// assertNoTempLeftover — dir에 `.ctr-settings-*.tmp` 잔존물이 없는지.
func assertNoTempLeftover(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ctr-settings-") {
			t.Fatalf("임시 파일 잔존: %s/%s", dir, e.Name())
		}
	}
}

// TestAtomicWriteFile_PreservesDestinationMode — F8(적대 검토). os.CreateTemp은 0600으로 만들고
// rename은 그 모드를 목적지에 옮긴다 `[실측]`(프로브: temp를 0444로 chmod 후 rename하면 목적지가
// 0444가 된다) — 그래서 재기록이 사용자 소유 파일의 권한을 조용히 좁힌다. 기존 목적지의 모드를
// 물려받고, 목적지가 없을 때만 0600으로 둔다.
//
// **Windows에서 이 단정은 공허하다** `[실측]`: Go는 0600·0644·0755를 모두 같은 속성(쓰기 가능)
// 으로 접고 Stat이 0666을 돌려준다 — 구분되는 값은 읽기 전용 0444 하나인데 그 목적지 위로는
// rename 자체가 Access denied로 실패한다. 판별력은 3-OS CI의 ubuntu·macos 러너에 있다.
func TestAtomicWriteFile_PreservesDestinationMode(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(dest, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dest, 0o644); err != nil { // umask와 무관하게 확정
		t.Fatalf("chmod: %v", err)
	}
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(dest, []byte("{\"a\":1}\n")); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	after, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("목적지 권한이 바뀌었다: before=%04o after=%04o", before.Mode().Perm(), after.Mode().Perm())
	}

	fresh := filepath.Join(dir, "fresh.json")
	if err := atomicWriteFile(fresh, []byte("{}\n")); err != nil {
		t.Fatalf("신규 목적지 쓰기: %v", err)
	}
	fi, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("목적지가 없던 자리의 모드=%04o want 0600", fi.Mode().Perm())
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
