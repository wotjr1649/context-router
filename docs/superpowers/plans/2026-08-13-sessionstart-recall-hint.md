# SessionStart 회수 힌트 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 압축 직후 세션에 "이 프로젝트에 보관된 도구 출력이 있다"는 한 줄을 주입해, 원문을 잃은 모델이 `ctr_search`로 되찾는 경로를 열어 준다.

**Architecture:** 이미 등록된 `SessionStart` 훅의 처리 분기 끝에 걸음 하나를 더한다. `content.db`를 read-only로 열어 shadow 귀속 아티팩트의 **존재 여부**만 묻고, 있으면 `hookSpecificOutput.additionalContext` JSON 한 줄을 stdout에 쓴다. 새 훅·새 MCP 도구·매니페스트 변경이 **전부 없다**.

**Tech Stack:** Go 1.26.5 · modernc.org/sqlite · 표준 라이브러리 `encoding/json`

## Global Constraints

- **Go 1.26.5** (`go.mod`). 새 의존성을 더하지 않는다.
- **게이트 다섯을 모든 태스크의 마지막에 돌린다**: `go build ./...` · `go vet ./...` · `go test ./... -count=1 -p 1` · `gofumpt -l .`(무출력) · `golangci-lint run`.
- **`go test`에 `-p 1`은 필수다.**
- **`internal/exec` 패키지의 실패는 이 머신의 환경 조건이다**(PowerShell 실행 정책). 이 변경의 파일이 그 패키지에 0개이므로 회귀가 아니다. 게이트 다섯을 `&&`로 잇지 말고 하나씩 돌려 그 패키지만 빨간 것을 확인한다.
- **행 번호를 주석·문서에 적지 않는다.** 심볼 이름을 적는다.
- **긴 테스트 데이터는 `strings.Repeat` 또는 testdata로 만든다.** 소스에 대용량 리터럴을 넣지 않는다.
- **문면 문자열은 D100 계약 2에 묶인다**: `MANDATORY`·`BLOCKED`·`Do NOT`·`Never`·`PREFER X OVER Y`·✅/❌ 금지. 사실과 대체 목적지만 적는다.
- **주입 문면은 호스트가 준 문자열을 담지 않는다.** 도구 이름·파일 경로·제목을 넣지 않는다.
- **머지는 지금 해도 되나 설치는 D104 판정(2026-08-26) 뒤다.** 이 플랜은 머지까지만 다룬다.

## File Structure

| 파일 | 책임 | 변경 |
|---|---|---|
| `internal/store/store.go` | 존재 여부 조회 하나를 `shadowOwnedHashQuery` 상수 위에 세운다 | 함수 1개 추가 |
| `internal/store/store_test.go` | 그 조회의 귀속 판정을 케이스 테이블로 고정 | 테스트 1개 추가 |
| `internal/hook/hook.go` | `SessionStart` 분기에서 주입을 호출하고, stdout 계약 주석을 넓힌다 | 함수 1개 + 상수 1개 추가, 기존 분기 1줄 추가, 주석 2곳 수정 |
| `internal/hook/hook_test.go` | host·source·재고 세 축의 골든 + 실패 경로 | 헬퍼 1개 일반화, 시드 헬퍼 1개 + 테스트 6개 추가 |
| `docs/superpowers/specs/2026-08-13-sessionstart-recall-hint-design.md` | 스펙의 matcher 항목과 reason code 이름을 구현과 맞춘다 | 2곳 수정 |
| `docs/context-router-design-v0.20-ko.md` | D104 결정표에 나이 편향을 기록 | 1곳 추가 |

**매니페스트는 건드리지 않는다.** 스펙 초안은 `hooks/hooks.json`의 matcher를 `compact`로 좁히자고 했으나, 그 분기는 `EnsureSession`(세션 등록)도 하므로 좁히면 `startup`·`resume`·`clear`에서 세션이 등록되지 않는다. 대신 **코드에서 `source`를 본다.** Task 3이 스펙을 그렇게 고친다.

---

### Task 1: `store.ShadowOwnedExists`

**Files:**
- Modify: `internal/store/store.go` — 함수 `ArtifactHashByID` 바로 뒤에 넣는다
- Test: `internal/store/store_test.go` — 테스트 `TestShadowOwnedAttribution` 바로 뒤에 넣는다

**Interfaces:**
- Consumes: 같은 패키지의 상수 `shadowOwnedHashQuery`, 필드 `Store.reader`
- Produces: `func (s *Store) ShadowOwnedExists(ctx context.Context) (bool, error)` — shadow 귀속 아티팩트가 하나라도 있으면 `true`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/store/store_test.go`의 `TestShadowOwnedAttribution` 바로 뒤에 넣는다. 시드 헬퍼(`seedHookOnly`·`seedHookPlusFile`·`seedNoSource`·`seedTwoHooks`)와 `openAt`은 이미 그 파일에 있다 — `openAt`은 `t.Cleanup`으로 닫으므로 명시적 `Close`를 부르지 않는다.

```go
// TestShadowOwnedExists — ShadowOwnedExists는 TestShadowOwnedAttribution과 같은 귀속 술어를
// 공유한다(같은 상수). 그 표가 hash 수를 보는 자리에서 이쪽은 존재 여부만 본다.
func TestShadowOwnedExists(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, st *Store)
		want bool
	}{
		{"빈 store → false", func(t *testing.T, st *Store) {}, false},
		{"hook만 참조 → true", seedHookOnly, true},
		{"hook+explicit 공유 → false", seedHookPlusFile, false},
		{"source 0개 → false", seedNoSource, false},
		{"hook 2개(동일 hash) → true", seedTwoHooks, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := openAt(t, t.TempDir())
			c.seed(t, st)
			got, err := st.ShadowOwnedExists(t.Context())
			if err != nil {
				t.Fatalf("ShadowOwnedExists: %v", err)
			}
			if got != c.want {
				t.Fatalf("ShadowOwnedExists=%v want %v", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run TestShadowOwnedExists -count=1 -p 1`
Expected: 컴파일 실패 — `st.ShadowOwnedExists undefined`

- [ ] **Step 3: 최소 구현을 넣는다**

`internal/store/store.go`의 `ArtifactHashByID` 바로 뒤:

```go
// ShadowOwnedExists — shadow 귀속 아티팩트가 하나라도 있는가(설계 spec 2026-08-13 §4.3).
// **수가 아니라 존재 여부만 반환하는 것이 계약이다**: 부르는 쪽(훅의 SessionStart 주입)이
// 싣는 것이 존재 여부뿐이고, EXISTS는 첫 적격 행에서 멈춰 shadow 색인 셋의 선두 컬럼이
// source_kind가 아닌 데서 오는 전(全)스캔을 피한다. 술어는 shadowOwnedHashQuery를 그대로
// 쓴다 — SizeStats·purge와 같은 정의를 공유해야 doctor가 렌더하는 수와 갈리지 않는다(D13).
// 나이 예산을 붙이지 않는 이유: DB에 있으면 회수 가능하고, 퍼지가 지운 것은 이미 없다.
func (s *Store) ShadowOwnedExists(ctx context.Context) (bool, error) {
	var found int
	err := s.reader.QueryRowContext(ctx, `SELECT EXISTS(`+shadowOwnedHashQuery+`)`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store ShadowOwnedExists: %w", err)
	}
	return found == 1, nil
}
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/store -run TestShadowOwnedExists -v -count=1 -p 1`
Expected: PASS — 5개 서브테스트 전부

- [ ] **Step 5: 게이트 다섯을 돌린다**

각각 따로 돌린다:
```
go build ./...
go vet ./...
go test ./... -count=1 -p 1
gofumpt -l .
golangci-lint run
```
Expected: `gofumpt -l .`는 무출력. `go test`는 `internal/exec`만 빨갛고(환경 조건) 나머지 전부 ok.

- [ ] **Step 6: 커밋**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): shadow 귀속 존재 여부 조회를 더한다"
```

---

### Task 2: `SessionStart`의 압축 직후 주입

**Files:**
- Modify: `internal/hook/hook.go` — 함수 `dispatch`의 `SessionStart` 분기 · 함수 `Run`의 doc comment · 함수 `denyTool`의 doc comment · 새 상수와 새 함수를 `denyTool` 뒤에 넣는다
- Test: `internal/hook/hook_test.go` — 헬퍼 `runHookCaptureStdout`을 일반화하고, 테스트 4개를 그 뒤에 넣는다

**Interfaces:**
- Consumes: Task 1의 `(*store.Store).ShadowOwnedExists(ctx) (bool, error)` · `store.OpenContext(ctx, dir string, readOnly bool) (*store.Store, error)` · 같은 패키지의 `hookInput.Source`·`Host`·`HostClaude`·`appendDrop(dir, reason, sessionID, hookEvent, tool string)`
- Produces: 없음(패키지 내부에서 닫힌다)

- [ ] **Step 1: 헬퍼를 일반화한다**

`internal/hook/hook_test.go`의 기존 `runHookCaptureStdout`을 아래 둘로 바꾼다. 기존 호출부는 시그니처가 그대로라 손대지 않는다.

```go
// runHookCaptureStdout — runHook 동형이되 stdout(=guard deny JSON, SessionStart 주입 JSON,
// 또는 빈 문자열)을 캡처해 반환한다.
func runHookCaptureStdout(t *testing.T, storeRoot string, in []byte) string {
	t.Helper()
	return runHookCaptureStdoutHost(t, HostClaude, storeRoot, in, nil)
}

// runHookCaptureStdoutHost — 위의 호스트·env 주입 변형(D35 경계와 deadline 주입용).
func runHookCaptureStdoutHost(t *testing.T, host Host, storeRoot string, in []byte, env map[string]string) string {
	t.Helper()
	var out bytes.Buffer
	if rc := Run(context.Background(), bytes.NewReader(in), &out, storeRoot, "test", host, func(k string) string { return env[k] }); rc != 0 {
		t.Fatalf("hook rc=%d want 0", rc)
	}
	return out.String()
}
```

- [ ] **Step 2: 실패하는 테스트 넷을 쓴다**

같은 파일에 넣는다. `guardSetup`·`fixtureWith`·`bigStdout`·`runHook`은 이미 그 파일에 있다.

```go
// seedOneCapture — PostToolUse 포착 1건으로 shadow 재고를 만든다(주입 조건의 전제).
func seedOneCapture(t *testing.T, storeRoot, cwd string, env map[string]string) {
	t.Helper()
	post := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd": cwd, "tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, post, env); rc != 0 {
		t.Fatalf("포착 rc=%d want 0", rc)
	}
}

// ① compact + Claude + 재고 있음 → 주입 JSON.
func TestSessionStartCompactInjectsHint(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	seedOneCapture(t, storeRoot, cwd, env)

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "compact"})
	out := runHookCaptureStdoutHost(t, HostClaude, storeRoot, start, env)

	var got map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("주입 stdout이 유효 JSON 아님: %v (out=%q)", err, out)
	}
	hso := got["hookSpecificOutput"]
	if hso["hookEventName"] != "SessionStart" {
		t.Fatalf("hookEventName=%q want SessionStart", hso["hookEventName"])
	}
	if !strings.Contains(hso["additionalContext"], "ctr_search") {
		t.Fatalf("additionalContext=%q want ctr_search 안내 포함", hso["additionalContext"])
	}
	// 호스트가 준 도구 이름이 문면에 실리지 않는다(설계 spec §4.2).
	if strings.Contains(hso["additionalContext"], "Bash") {
		t.Fatalf("문면에 호스트 도구 이름이 실렸다: %q", hso["additionalContext"])
	}
}

// ② startup + Claude + 재고 있음 → 빈 stdout(압축 직후가 아니면 주입하지 않는다).
func TestSessionStartStartupDoesNotInject(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	seedOneCapture(t, storeRoot, cwd, env)

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "startup"})
	if out := runHookCaptureStdoutHost(t, HostClaude, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (source=startup은 주입 대상 아님)", out)
	}
}

// ③ compact + Codex + 재고 있음 → 빈 stdout(Claude 형식을 다른 호스트에 쓰지 않는다).
func TestSessionStartCompactCodexDoesNotInject(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	seedOneCapture(t, storeRoot, cwd, env)

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "compact"})
	if out := runHookCaptureStdoutHost(t, HostCodex, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (Codex 호스트는 주입 대상 아님)", out)
	}
}

// ④ compact + Claude + 재고 없음(content.db 부재) → 빈 stdout + drops 무증가.
// 포착 전 프로젝트는 정상 상태이므로 진단 줄을 남기지 않는다.
func TestSessionStartCompactNoStoreIsSilent(t *testing.T) {
	storeRoot := t.TempDir()
	cwd := evalLong(t, t.TempDir())
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "compact"})
	if out := runHookCaptureStdoutHost(t, HostClaude, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (재고 없음)", out)
	}
	sdir := sessDir(t, storeRoot, cwd)
	if b, err := os.ReadFile(filepath.Join(sdir, "session.drops.log")); err == nil && strings.Contains(string(b), "hint-") {
		t.Fatalf("포착 전 프로젝트에 hint drop이 남았다: %s", b)
	}
}

// ⑤ compact + Claude + content.db는 있으나 shadow 귀속 0 → 빈 stdout.
// ④와 다른 축이다: 저장소가 **있는데** 귀속 술어가 비는 경우를 고정한다.
func TestSessionStartCompactNoShadowIsSilent(t *testing.T) {
	storeRoot, cwd, contentDir, _ := guardSetup(t)
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	// 비-hook 소스만 담은 store를 만든다 → 귀속 술어가 비운다.
	st, err := store.Open(contentDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("explicit-only"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "file:x", Kind: "file", SrcHash: "sh-x"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "compact"})
	if out := runHookCaptureStdoutHost(t, HostClaude, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (shadow 귀속 0)", out)
	}
}

// ⑥ compact + Claude + 저장소 불용 → 빈 stdout + hint- 사유 1줄.
// 조용한 스킵만 두면 "주입을 안 했다"와 "했는데 안 닿았다"가 같은 음성이 된다.
func TestSessionStartCompactStoreErrorLeavesReason(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	seedOneCapture(t, storeRoot, cwd, env)

	// family 셋을 지우고 비-DB 바이트로 대체 → os.Stat은 통과하고 열기/조회가 실패한다.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(contentDir, "content.db"+suffix))
	}
	if err := os.WriteFile(filepath.Join(contentDir, "content.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatalf("손상 주입: %v", err)
	}

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "compact"})
	if out := runHookCaptureStdoutHost(t, HostClaude, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (저장소 불용)", out)
	}
	b, err := os.ReadFile(filepath.Join(sdir, "session.drops.log"))
	if err != nil {
		t.Fatalf("drops 읽기: %v", err)
	}
	// hint-store(열기 실패)와 hint-query(조회 실패) 중 어느 쪽이 나오는지는 드라이버가 언제
	// 실패를 내는지에 달렸다 — 이 테스트가 고정하는 것은 "사유가 남는다"이지 어느 사유인지가 아니다.
	if !strings.Contains(string(b), "hint-") {
		t.Fatalf("hint 사유가 drops에 없다: %s", b)
	}
}
```

- [ ] **Step 3: 실패를 확인한다**

Run: `go test ./internal/hook -run TestSessionStart -count=1 -p 1`
Expected: **①과 ⑥만 FAIL** — ①은 stdout이 비어 `Unmarshal`이 "unexpected end of JSON input", ⑥은 `hint-` 사유가 없다. ②③④⑤는 이미 통과한다(아직 아무것도 주입하지 않으므로) — 그것이 정상이고, 빨간 둘이 ①·⑥인 것을 확인한다.

- [ ] **Step 4: 주입 함수를 넣는다**

`internal/hook/hook.go`의 `denyTool` 바로 뒤:

```go
// recallHint — SessionStart 압축 직후 주입 문면(설계 spec 2026-08-13 §4.2). D100 계약 2에
// 묶인다: 사실 하나와 대체 목적지 하나만 적고 훈계 어휘를 쓰지 않는다. **호스트가 준 문자열을
// 담지 않는다** — 도구 이름은 검증 없이 sources.uri로 흐르는 값이라 그것을 문면에 실으면
// 신뢰 불가 입력이 모델 컨텍스트로 가는 경로가 하나 더 생긴다. 진입을 ctr_search 하나로 적는
// 이유는 그 시점에 모델이 쥔 artifact id가 0개라서다(ctr_fetch는 필수 id를 받는다).
const recallHint = "context-router: 이 프로젝트에 보관된 도구 출력이 있다. ctr_search로 찾는다."

// injectRecallHint — 압축 직후(source=compact)에, Claude 호스트에서, 재고가 있을 때만 stdout에
// additionalContext JSON 한 줄을 쓴다(설계 spec §4.1·§4.3). 셋 중 하나라도 아니면 stdout을
// 비운다. 어떤 실패도 훅 종료 코드를 바꾸지 않는다(§2.3 fail-open) — 잃는 것은 안내 한 줄뿐이다.
// content.db 부재는 포착 전 프로젝트의 정상 상태이므로 진단 줄도 남기지 않고, 그 뒤의 실패만
// drops에 사유를 남긴다: 조용한 스킵만 두면 "주입을 안 했다"와 "했는데 안 닿았다"가 같은
// 음성으로 보여 다음 구간의 판정이 그것을 채택 부진으로 오독한다.
func injectRecallHint(ctx context.Context, in hookInput, dir, contentDir string, host Host, source string, stdout io.Writer) {
	if host != HostClaude || source != "compact" {
		return
	}
	if _, err := os.Stat(filepath.Join(contentDir, "content.db")); err != nil {
		return // 포착 전 프로젝트 — 정상, 진단 줄 없음
	}
	st, err := store.OpenContext(ctx, contentDir, true)
	if err != nil {
		appendDrop(dir, "hint-store", "", in.HookEventName, "")
		return
	}
	defer func() { _ = st.Close() }() // ro 커넥션의 체크포인트 오류는 무시한다
	// ctx를 반드시 넘긴다: OpenContext의 readOnly 경로는 ctx를 쓰지 않고 busy_timeout이
	// 훅 총예산보다 커서, ctx 없이 물으면 예산 밖에서 블록된다(D103 계약 8과 같은 함정).
	ok, err := st.ShadowOwnedExists(ctx)
	if err != nil {
		appendDrop(dir, "hint-query", "", in.HookEventName, "")
		return
	}
	if !ok {
		return
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "SessionStart",
		"additionalContext": recallHint,
	}}
	if b, err := json.Marshal(out); err == nil {
		_, _ = stdout.Write(b)
	}
}
```

- [ ] **Step 5: `SessionStart` 분기에서 부른다**

`internal/hook/hook.go`의 함수 `dispatch` 안, `SessionStart` 분기의 `return` 바로 앞에 한 줄을 더한다. `EnsureSession`이 실패해도 주입은 시도한다 — 세션 등록과 안내는 서로 독립이다.

```go
		if _, err := ad.EnsureSession(ctx, src, worktreeRoot); err != nil {
			appendDrop(dir, "ensure-failed", external, in.HookEventName, in.ToolName)
		}
		injectRecallHint(ctx, in, dir, contentDir, host, src, stdout)
		return
```

- [ ] **Step 6: stdout 계약 주석 둘을 넓힌다**

함수 `Run`의 doc comment에서 *"stdout은 guard(T7)의 permissionDecision JSON 전용이라 골격에서는 미사용"*을 아래로 바꾼다:

```
// stdout은 두 이벤트가 배타적으로 쓴다 — PreToolUse는 guard(T7)의 permissionDecision JSON,
// SessionStart는 압축 직후 회수 안내의 additionalContext JSON(injectRecallHint). 한 훅 실행은
// 한 이벤트만 처리하므로 두 스키마가 한 스트림에 섞이지 않는다.
```

함수 `denyTool`의 doc comment에서 *"stdout은 deny JSON 전용이라(Claude Code가 exit 0 stdout을 파싱) 그 외 바이트는 쓰지 않는다"*를 아래로 바꾼다:

```
// PreToolUse 경로의 stdout은 이 deny JSON 전용이라(Claude Code가 exit 0 stdout을 파싱) 그 외
// 바이트를 쓰지 않는다. SessionStart 경로가 같은 스트림에 쓰는 것은 injectRecallHint뿐이고 두
// 이벤트는 배타적이다.
```

- [ ] **Step 7: 통과를 확인한다**

Run: `go test ./internal/hook -run 'TestSessionStart' -v -count=1 -p 1`
Expected: PASS — 넷 전부

- [ ] **Step 8: 게이트 다섯을 돌린다**

Task 1의 Step 5와 같다. 각각 따로 돌린다.

- [ ] **Step 9: 커밋**

```bash
git add internal/hook/hook.go internal/hook/hook_test.go
git commit -m "feat(hook): 압축 직후 SessionStart에 회수 안내 한 줄을 주입한다"
```

---

### Task 3: 스펙·결정표 문서 정정

**Files:**
- Modify: `docs/superpowers/specs/2026-08-13-sessionstart-recall-hint-design.md` — §4.1의 matcher 문단, §4.3의 reason code 이름
- Modify: `docs/context-router-design-v0.20-ko.md` — D104 판정 규칙표 뒤의 ★ 계열에 항목 추가

**Interfaces:**
- Consumes: Task 2가 확정한 실제 동작(코드가 `source`를 보고, reason은 `hint-store`·`hint-query` 둘)
- Produces: 없음

- [ ] **Step 1: 스펙 §4.1의 matcher 문단을 고친다**

*"**매니페스트의 matcher를 `compact`로 좁힌다.** 지금은 `""`(전체)다."*로 시작하는 문단을 아래로 바꾼다:

```
**매니페스트는 건드리지 않고 코드에서 `source`를 본다.** 초안은 matcher를 `compact`로 좁히려
했으나 그 분기는 `EnsureSession`(세션 등록)도 하므로, 좁히면 `startup`·`resume`·`clear`에서
세션이 등록되지 않는다(D51의 합성 등록이 받아 주기는 하나 `source`가 `first-event`로 바뀌어
세션 메타가 달라진다). `SessionStart`의 matcher는 `""` 그대로 두고 **주입만** `in.Source ==
"compact"`로 좁힌다 — 압축 직후가 원문을 방금 잃은 유일한 순간이다. 결과로 `hooks/hooks.json`과
`hooks/codex-hooks.json` 둘 다 무수정이다.
```

- [ ] **Step 2: 스펙 §4.3의 reason code 이름을 실제와 맞춘다**

`skipped-no-db` · `skipped-locked` · `skipped-query-error`를 아래로 바꾼다:

```
reason은 둘이다: `hint-store`(read-only 열기 실패 — 잠금 경합·손상) · `hint-query`(조회 실패).
**`content.db` 부재는 진단 줄을 남기지 않는다** — 포착 전 프로젝트의 정상 상태이고, 그것까지
세면 새 워크트리마다 잡음이 쌓인다.
```

- [ ] **Step 3: D104 결정표에 나이 편향을 기록한다**

`docs/context-router-design-v0.20-ko.md`에서 판정 규칙표의 `★**T —` 로 시작하는 문단을 찾아, 그 문단 **앞**에 아래를 넣는다:

```
★**A — 채택 레버가 만든 회수는 나이 분포를 최근으로 기울인다**(세션 60). 레버로 유발된 회수는
"잃어서 필요해진 회수"가 아니라 "있다고 알려줘서 해 본 회수"이고 그 모집단은 최근 쪽에 몰린다.
**행 5가 읽는 절단 보정 p90이 그만큼 작아지므로, 채택을 만들려고 넣은 레버가 "72시간이면
충분하다"를 생산할 수 있다.** 어떤 채택 레버든 같은 편향을 만들므로 설계로 제거할 수 없다 —
행 5로 판정할 때 **그 구간에 채택 레버가 배포돼 있었는지를 함께 적고**, 배포돼 있었으면 나온 값을
**하한으로만** 읽는다(위 ★F2와 같은 취급). 세션 60 §3.10의 손실(오래된 포착 100건이 72h 퍼지로
사라졌다)도 같은 방향으로 분포를 밀었다.
```

- [ ] **Step 4: 문서만 바뀐 것을 확인한다**

Run: `git diff --stat`
Expected: `.md` 파일 둘만. Go 파일 0개.

- [ ] **Step 5: 게이트 다섯을 돌린다**

문서만 바뀌었어도 규약대로 돌린다. Task 1의 Step 5와 같다.

- [ ] **Step 6: 커밋**

```bash
git add docs/superpowers/specs/2026-08-13-sessionstart-recall-hint-design.md docs/context-router-design-v0.20-ko.md
git commit -m "docs: 회수 힌트 스펙을 구현과 맞추고 D104에 나이 편향을 기록한다"
```

---

## 착수 후 — 이 플랜 밖

1. **설치는 D104 판정(2026-08-26) 뒤다.** 첫 구간이 "레버 없음 = 회수 0" 기준선으로 남아야 두 번째 구간이 레버의 시험이 된다.
2. **주입 발효 시각을 손으로 적는다.** 이 걸음은 원장에 기록하지 않으므로 사후에 원장만으로 전/후를 가를 수 없다 — 세션 60이 "336h 창 실효 시작"을 계측 t0와 별개 시계로 적은 것과 같은 처방이다.
3. **판정이 행 2가 아니면 이 설계의 전제를 다시 본다.**
