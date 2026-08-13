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
  훅 매니페스트의 `command`가 `context-router`, 즉 **PATH의 설치본**이므로 머지 자체는 훅이
  부르는 바이너리를 바꾸지 못한다 `[확인 — hooks/hooks.json]`. 판정 전 오염 경로는 둘뿐이다:
  (1) 창 안에 새로 설치하는 것 — 사람의 행동이지 자동이 아니다. (2) **머지된 이 문서들 자체.**
  판정 대상 원장이 될 수 있는 프로젝트에서 이 계획·스펙을 읽으면 그 문면이 `ctr_fetch`를
  권하고, 그 회수가 `resolved_artifacts`에 들어간다. 세션 59의 "검증 회수를 판정 원장에서 하지
  마라"를 문서 읽기에도 그대로 적용한다.
- **★ Task 3(문서)을 가장 먼저 실행한다.** 번호가 3인 것은 문서 순서일 뿐이다. 뒤로 미루면 Task 2 커밋과 Task 3 커밋 사이의 `main`에 **배송된 코드와 모순되는 스펙**이 남는다.

## File Structure

| 파일 | 책임 | 변경 |
|---|---|---|
| `internal/store/store.go` | 존재 여부 조회 하나를 `shadowOwnedHashQuery` 상수 위에 세운다 | 함수 1개 추가 |
| `internal/store/store_test.go` | 그 조회의 귀속 판정을 케이스 테이블로 고정 | 테스트 1개 추가 |
| `internal/hook/hook.go` | `SessionStart` 분기에서 주입을 호출하고, stdout 계약 주석을 넓힌다 | 함수 1개 + 상수 1개 추가, 기존 분기 1줄 추가, 주석 2곳 수정 |
| `internal/hook/hook_test.go` | host·source·재고 세 축의 반사실 골든 + 실패 경로 + 문면 계약 | 헬퍼 3개(일반화 1 · 신규 2), 시드 헬퍼 1개, 테스트 7개 추가 |
| `docs/superpowers/specs/2026-08-13-sessionstart-recall-hint-design.md` | 불변 공정 기록이라 **본문을 고치지 않고** "구현 중 정정" 절을 덧붙인다 | 절 1개 append |
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
// 싣는 것이 존재 여부뿐이고, EXISTS는 적격 행을 찾는 즉시 멈춘다 — 재고가 있는 흔한 경우에
// shadow 색인 셋의 선두 컬럼이 source_kind가 아닌 데서 오는 전(全)스캔을 피한다. **재고가 0이면
// 그 이득이 없다**(끝까지 훑는다): 조용해야 할 경로가 가장 비싸다는 뜻이고, 상한은 호출부가
// 넘기는 ctx 예산이 건다. 술어는 shadowOwnedHashQuery를 그대로 쓴다 — SizeStats·purge와 같은
// 정의를 공유해야 doctor가 렌더하는 수와 갈리지 않는다(D13). 같은 상수를 EXISTS로 감싸는 선례가
// 이미 있다(lastIndexedAtByHashQuery). 나이 예산을 붙이지 않는 이유: DB에 있으면 회수 가능하고
// 퍼지가 지운 것은 이미 없다 — 다만 그것은 이 순간에만 참이라, 다른 호스트의 서버가 곧 퍼지를
// 돌리면 헛걸음이 한 번 난다.
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
- Test: `internal/hook/hook_test.go` — 헬퍼 `runHookCaptureStdout`을 일반화하고, 시드 헬퍼 1개와 테스트 6개를 그 뒤에 넣는다

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

- [ ] **Step 2: 실패하는 테스트 일곱을 쓴다**

같은 파일에 넣는다. `guardSetup`·`fixtureWith`·`bigStdout`·`runHook`은 이미 그 파일에 있다.
**새 import는 없다** — `errors`·`os`·`filepath`·`strings`는 이미 그 파일의 import 블록에 있다.
(④가 `filepath.WalkDir` 대신 `filepath.Walk`를 쓰는 이유가 그것이다. `io/fs`는 없다.)

**②~⑤의 침묵 단정은 반드시 반사실이어야 한다.** `stdout == ""` 하나만 보는 테스트는 훅이
무슨 이유로든 조용히 끝나기만 하면 통과한다 — 이벤트 파싱이 통째로 깨져도 넷이 다 초록이 된다.
그래서 넷 모두 **①과 같은 픽스처에서 한 축만 바꾸고**, 빈 stdout에 더해 *부작용 축*을 하나씩
본다: ②③은 사유가 남지 않은 것(= 저장소를 열지 않았다), ④는 파일이 생기지 않은 것.

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

// assertHintReason — session.drops.log에서 want 사유 줄을 찾는다. appendDrop의 형식이
// "<ts>\t<reason>\t<sid8>\t<hook_event>\t<tool>"이므로 탭으로 감싸 정확히 대조한다.
func assertHintReason(t *testing.T, sdir, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sdir, "session.drops.log"))
	if err != nil {
		t.Fatalf("drops 읽기: %v", err)
	}
	if !strings.Contains(string(b), "\t"+want+"\t") {
		t.Fatalf("drops에 %q 사유가 없다:\n%s", want, b)
	}
}

// assertNoHintReason — hint- 사유가 한 줄도 없음을 고정한다. 게이트(source·host) 앞에서
// 끝나는 경로는 **저장소를 열지 않았다**는 증거가 이것뿐이다. 빈 stdout만 보는 단정은
// 훅이 어떤 이유로든 조용히 끝나기만 하면 통과하므로 그 축을 못 잡는다.
func assertNoHintReason(t *testing.T, sdir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sdir, "session.drops.log"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("drops 읽기: %v", err)
	}
	if strings.Contains(string(b), "\thint-") {
		t.Fatalf("게이트 앞에서 끝나야 하는데 hint- 사유가 남았다:\n%s", b)
	}
}

// ① compact + Claude + 재고 있음 → 주입 JSON + hint-ok 한 줄.
func TestSessionStartCompactInjectsHint(t *testing.T) {
	storeRoot, cwd, _, sdir := guardSetup(t)
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
	// 문면은 상수와 **정확히** 대조한다. 부분 단정(Contains)으로는 계약을 못 고정한다 —
	// recallHint가 보간 없는 const라 "호스트 도구 이름이 안 실렸다" 류는 구조적으로 항상 통과한다.
	if hso["additionalContext"] != recallHint {
		t.Fatalf("additionalContext=%q want %q", hso["additionalContext"], recallHint)
	}
	assertHintReason(t, sdir, "hint-ok")
}

// ①-b D100 계약 2 — 훈계 어휘 금지. 문면이 바뀌어도 이 축은 남는다. 주입 경로와 독립이라
// 별도 테스트로 둔다(문면 상수만 본다).
func TestRecallHintObeysWordingContract(t *testing.T) {
	for _, banned := range []string{"MANDATORY", "BLOCKED", "Do NOT", "Never", "PREFER", "✅", "❌"} {
		if strings.Contains(recallHint, banned) {
			t.Fatalf("recallHint에 금지 어휘 %q가 있다: %q", banned, recallHint)
		}
	}
	// 착수 문턱을 움직이는 도구를 가리켜야 한다 — 검색만으로는 resolved_artifacts·missed가
	// 안 움직인다(그 칸을 쓰는 것은 ctr_fetch 경로의 LedgerAppendFetch뿐이다).
	for _, want := range []string{"ctr_search", "ctr_fetch"} {
		if !strings.Contains(recallHint, want) {
			t.Fatalf("recallHint에 %q가 없다: %q", want, recallHint)
		}
	}
}

// ② startup + Claude + 재고 있음 → 빈 stdout(압축 직후가 아니면 주입하지 않는다).
func TestSessionStartStartupDoesNotInject(t *testing.T) {
	storeRoot, cwd, _, sdir := guardSetup(t)
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	seedOneCapture(t, storeRoot, cwd, env)

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "startup"})
	if out := runHookCaptureStdoutHost(t, HostClaude, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (source=startup은 주입 대상 아님)", out)
	}
	// ①과 **source 한 축만** 다른 반사실이다. 빈 stdout에 더해 사유가 없어야 "게이트 앞에서
	// 끝났다"가 서고, 그래야 훅이 그냥 죽어도 통과하는 공허한 단정이 되지 않는다.
	assertNoHintReason(t, sdir)
}

// ③ compact + Codex + 재고 있음 → 빈 stdout(Claude 형식을 다른 호스트에 쓰지 않는다).
func TestSessionStartCompactCodexDoesNotInject(t *testing.T) {
	storeRoot, cwd, _, sdir := guardSetup(t)
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}
	seedOneCapture(t, storeRoot, cwd, env)

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "compact"})
	if out := runHookCaptureStdoutHost(t, HostCodex, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (Codex 호스트는 주입 대상 아님)", out)
	}
	// ①과 **host 한 축만** 다른 반사실이다. ②와 같은 이유로 사유 부재까지 본다.
	assertNoHintReason(t, sdir)
}

// ④ compact + Claude + content.db 부재 → 빈 stdout + hint-empty 한 줄.
// **게이트 둘을 지난 뒤에는 어느 경로로 끝나든 한 줄을 남긴다** — 그래야 "압축이 안 일어났다"와
// "일어났는데 재고가 없었다"가 갈린다.
func TestSessionStartCompactNoStoreLeavesEmptyReason(t *testing.T) {
	storeRoot := t.TempDir()
	cwd := evalLong(t, t.TempDir())
	env := map[string]string{"CTR_HOOK_DEADLINE_MS": "60000"}

	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd, "source": "compact"})
	if out := runHookCaptureStdoutHost(t, HostClaude, storeRoot, start, env); out != "" {
		t.Fatalf("stdout=%q want empty (재고 없음)", out)
	}
	assertHintReason(t, sessDir(t, storeRoot, cwd), "hint-empty")

	// 부재 경로가 **아무것도 만들지 않는 것**까지 본다. stdout만 보는 단정은 opener가 빈 DB나
	// 저널을 만들어도 통과하고, 그러면 "부재"라는 이 테스트의 전제 자체가 다음 실행에서 무너진다.
	// 경로 조립 헬퍼에 기대지 않도록 storeRoot 전체를 훑는다.
	var made []string
	if err := filepath.Walk(storeRoot, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(info.Name(), "content.db") {
			made = append(made, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("storeRoot 훑기: %v", err)
	}
	if len(made) != 0 {
		t.Fatalf("부재 경로가 content.db 계열을 만들었다: %v", made)
	}
}

// ⑤ compact + Claude + content.db는 있으나 shadow 귀속 0 → 빈 stdout.
// ④와 다른 축이다: 저장소가 **있는데** 귀속 술어가 비는 경우를 고정한다.
func TestSessionStartCompactNoShadowIsSilent(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
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
	assertHintReason(t, sdir, "hint-empty")
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
	// 사유가 하나뿐이라 정확히 대조한다. database/sql.Open은 연결하지 않으므로 열기·조회 실패가
	// 전부 같은 자리로 떨어지고, 그것을 두 이름으로 가르면 이름이 거짓이 된다.
	assertHintReason(t, sdir, "hint-unavailable")
}
```

- [ ] **Step 3: 실패를 확인한다**

Run: `go test ./internal/hook -run 'TestSessionStart|TestRecallHint' -count=1 -p 1`
Expected: **컴파일 실패 — `undefined: recallHint`.** 테스트 ①과 ①-b가 그 상수를 직접 대조하므로 red 단계는 개별 FAIL이 아니라 패키지 컴파일 실패로 나타난다. 그것이 정상이다 — Step 4가 상수와 함수를, Step 5가 호출부를 넣으면 풀린다. **부분 구현으로 넘어가지 말고 Step 4·5를 마친 뒤 Step 7에서 한 번에 green을 확인한다.**

- [ ] **Step 4: 주입 함수를 넣는다**

`internal/hook/hook.go`의 `denyTool` 바로 뒤:

```go
// recallHint — SessionStart 압축 직후 주입 문면(설계 spec 2026-08-13 §4.2 + 그 문서의 "구현 중
// 정정"). D100 계약 2에 묶인다: 훈계 어휘 없이 사실과 대체 목적지만 적는다. 형태는 같은 파일의
// denyReasonIndexed를 그대로 따른다 — 그쪽이 이미 배송된 준수 선례이고, 목적지를 명사구로
// 나열하는 꼴이 "리다이렉트는 대체 목적지만 말한다"에 가장 가깝다.
// **두 도구를 다 적는 것이 계약이다**: D104의 착수 문턱은 resolved_artifacts + missed이고 그
// 두 칸을 쓰는 것은 LedgerAppendFetch 하나뿐이며 그 프로덕션 호출부는 ctr_fetch 경로뿐이다
// (ctr_search는 LedgerAppend의 일반 행만 남긴다). 검색만 적으면 이 레버가 성공할수록 판정
// 지표가 조용해진다 — 스니펫으로 해결되면 원문을 안 가져오기 때문이다. 검색이 히트마다
// artifact_id를 실어 주므로 두 단계는 실제로 이어진다.
// **호스트가 준 문자열은 담지 않는다** — 도구 이름은 검증 없이 sources.uri로 흐르는 값이다.
const recallHint = "context-router: 이 프로젝트에 보관된 도구 출력 — ctr_search로 검색, ctr_fetch로 바이트 정확 조회"

// injectRecallHint — 압축 직후(source=compact)에, Claude 호스트에서, 재고가 있을 때만 stdout에
// additionalContext JSON 한 줄을 쓴다(설계 spec §4.1·§4.3). 어떤 실패도 훅 종료 코드를 바꾸지
// 않는다(§2.3 fail-open) — 잃는 것은 안내 한 줄뿐이다.
//
// ★ **두 게이트를 지난 뒤에는 어느 경로로 끝나든 반드시 진단 한 줄을 남긴다.** 게이트 뒤에서
// 조용히 반환하면 "압축이 한 번도 안 일어났다"와 "일어났는데 주입이 안 됐다"가 같은 음성이
// 된다 — 이 경로는 원장에도 세션 이벤트에도 흔적을 안 남기고(EnsureSession은 재호출 시
// session_start를 재발행하지 않는다), 그러면 다음 구간이 0을 채택 부진으로 오독한다.
// 그래서 **이 줄의 수가 곧 압축 발화 횟수이고 첫 줄의 타임스탬프가 곧 주입 발효 시각이다.**
// 원장에 쓰지 않는 이유는 계측 중립이 아니라 구조다: readOnly Open은 ledger를 개설하지 않고
// (s.ledger가 nil) writable Open은 lockStoreCtx·migrate를 타서 세션 시작의 락 경합에 걸린다.
func injectRecallHint(ctx context.Context, in hookInput, dir, contentDir string, host Host, source string, stdout io.Writer) {
	if source != "compact" || host != HostClaude {
		return // 이 훅의 대상이 아니다 — 진단도 남기지 않는다
	}
	if _, err := os.Stat(filepath.Join(contentDir, "content.db")); err != nil {
		appendDrop(dir, "hint-empty", "", in.HookEventName, "")
		return
	}
	st, err := store.OpenContext(ctx, contentDir, true)
	if err != nil {
		appendDrop(dir, "hint-unavailable", "", in.HookEventName, "")
		return
	}
	defer func() { _ = st.Close() }() // ro 커넥션의 체크포인트 오류는 무시한다
	// ctx를 반드시 넘긴다: OpenContext의 readOnly 경로는 ctx를 쓰지 않고 busy_timeout이
	// 훅 총예산보다 커서, ctx 없이 물으면 예산 밖에서 블록된다(D103 계약 8과 같은 함정).
	// 사유를 하나로 둔 이유: database/sql.Open은 연결하지 않으므로 부재·경합·손상이 전부
	// 이 질의까지 와서 실패한다 — 열기 실패와 조회 실패를 가르는 사유 이름은 거짓이 된다.
	ok, err := st.ShadowOwnedExists(ctx)
	if err != nil {
		appendDrop(dir, "hint-unavailable", "", in.HookEventName, "")
		return
	}
	if !ok {
		appendDrop(dir, "hint-empty", "", in.HookEventName, "")
		return
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "SessionStart",
		"additionalContext": recallHint,
	}}
	b, err := json.Marshal(out)
	if err != nil {
		appendDrop(dir, "hint-unavailable", "", in.HookEventName, "")
		return
	}
	_, _ = stdout.Write(b)
	appendDrop(dir, "hint-ok", "", in.HookEventName, "")
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

두 문면 다 **소스에서 여러 줄에 걸쳐 있고 같은 줄에 보존해야 할 다른 문장이 붙어 있다.** 아래 블록을 **통째로** 찾아 통째로 바꾼다 — 부분 문자열로 치환하면 옆 문장이 사라진다.

함수 `Run`의 doc comment. 이 세 줄을 찾는다(`SessionStart=EnsureSession`이라는 요약도 더는 사실이 아니라 함께 고친다):

```
// 도출 → deadline ctx → SessionStart=EnsureSession / 그 외=SessionExists 판정. host는 명시적 발신
// 호스트(D35) — 세션 네임스페이스 접두라 미지 값은 오귀속 대신 drop한다. stdout은 guard(T7)의
// permissionDecision JSON 전용이라 골격에서는 미사용. getenv는 테스트 주입점.
```

이렇게 바꾼다:

```
// 도출 → deadline ctx → SessionStart=EnsureSession + 압축 직후 회수 안내 주입(injectRecallHint) /
// 그 외=SessionExists 판정. host는 명시적 발신 호스트(D35) — 세션 네임스페이스 접두라 미지 값은
// 오귀속 대신 drop한다. stdout은 두 이벤트가 배타적으로 쓴다: PreToolUse는 guard(T7)의
// permissionDecision JSON, SessionStart는 injectRecallHint의 additionalContext JSON. 한 훅
// 실행은 한 이벤트만 처리하므로 두 스키마가 한 스트림에 섞이지 않는다. getenv는 테스트 주입점.
```

함수 `denyTool`의 doc comment. 이 두 줄을 찾는다:

```
// 가드 판정은 DB 없이 성립, §4). reason은 가드별 안내 문구(D54 — 색인형/Grep 분리). stdout은
// deny JSON 전용이라(Claude Code가 exit 0 stdout을 파싱) 그 외 바이트는 쓰지 않는다.
```

이렇게 바꾼다:

```
// 가드 판정은 DB 없이 성립, §4). reason은 가드별 안내 문구(D54 — 색인형/Grep 분리). PreToolUse
// 경로의 stdout은 이 deny JSON 전용이라(Claude Code가 exit 0 stdout을 파싱) 그 외 바이트를 쓰지
// 않는다 — 같은 스트림에 쓰는 다른 자리는 SessionStart의 injectRecallHint뿐이고 두 이벤트는
// 배타적이다.
```

- [ ] **Step 7: 통과를 확인한다**

Run: `go test ./internal/hook -run 'TestSessionStart|TestRecallHint' -v -count=1 -p 1`
Expected: PASS — 일곱 전부(①·①-b·②·③·④·⑤·⑥)

- [ ] **Step 8: 게이트 다섯을 돌린다**

Task 1의 Step 5와 같다. 각각 따로 돌린다.

- [ ] **Step 9: 커밋**

```bash
git add internal/hook/hook.go internal/hook/hook_test.go
git commit -m "feat(hook): 압축 직후 SessionStart에 회수 안내 한 줄을 주입한다"
```

---

### Task 3: 문서 — ★ **이 태스크를 가장 먼저 실행한다**

**실행 순서**: Task 1보다 **먼저** 돌린다. 뒤에 두면 Task 2 커밋과 이 커밋 사이의 `main`에
**배송된 코드와 정면으로 모순되는 스펙**(매니페스트 matcher를 좁힌다)이 남는다.

**Files:**
- Modify: `docs/superpowers/specs/2026-08-13-sessionstart-recall-hint-design.md` — **본문을 고치지 않고 문서 끝에 절 하나를 덧붙인다.** 그 디렉터리를 `CLAUDE.md`가 "dated immutable process records"로 규정하므로, in-place 개정이 아니라 append가 그 계약과 정합한다(`docs/prompts`의 append-only 관례와 같은 형태).
- Modify: `docs/context-router-design-v0.20-ko.md` — D104 판정 규칙표 뒤에 ★ 항목 추가. 이쪽은 **살아 있는 계약**이라 in-place가 맞다.

**Interfaces:**
- Consumes: 계획이 확정한 실제 동작(코드가 `source`를 본다 · reason 셋 · 문면이 두 도구를 가리킨다)
- Produces: 없음

- [ ] **Step 1: 스펙 문서 맨 끝에 아래 절을 그대로 덧붙인다**

```markdown
## 9. 구현 중 정정 (2026-08-13 — 계획 작성과 검토 셋에서)

본문은 고치지 않고 덧붙인다. 이 디렉터리는 불변 공정 기록이다.

**① matcher를 좁히지 않는다.** §4.1이 매니페스트의 matcher를 `compact`로 좁히자고 했으나, 그
분기는 `EnsureSession`(세션 등록)도 하므로 좁히면 `startup`·`resume`·`clear`에서 세션이 등록되지
않는다(D51 합성 등록이 받아 주지만 `source`가 `first-event`로 바뀐다). **매니페스트 둘 다
무수정**이고 코드에서 `in.Source == "compact"`로 주입만 좁힌다. §4.1의 불릿 *"`hooks/hooks.json`의
matcher 문자열 하나만 바뀐다"*도 그래서 무효다.

**② 문면에 `ctr_fetch`를 함께 적는다.** §4.2는 "진입은 검색 하나"로 정했는데, D104의 착수
문턱인 `resolved_artifacts + missed`를 쓰는 것은 `LedgerAppendFetch` 하나뿐이고 그 프로덕션
호출부는 `ctr_fetch` 경로뿐이다 — `ctr_search`는 `LedgerAppend`의 일반 행만 남긴다
`[실측 — 호출부 대조]`. **검색만 적으면 이 레버가 성공할수록 판정 지표가 조용해진다**(스니펫으로
해결되면 원문을 안 가져온다). 검색 히트가 `artifact_id`를 실어 주므로 두 단계는 실제로 이어지고,
같은 패키지의 `denyReasonIndexed`가 이미 그 형태로 배송돼 있다.

**③ reason은 셋이고, 게이트 뒤에는 반드시 한 줄을 남긴다.** §4.3의 `skipped-no-db` ·
`skipped-locked` · `skipped-query-error`는 무효다. `database/sql.Open`은 연결하지 않으므로
부재·경합·손상이 전부 조회까지 와서 실패한다 — 열기 실패와 조회 실패를 가르는 이름은 거짓이
된다. 실제 사유는 `hint-ok` · `hint-empty` · `hint-unavailable`이고, **`source=compact` +
Claude 게이트를 지난 뒤에는 어느 경로로 끝나든 한 줄을 남긴다.** 그러지 않으면 "압축이 한 번도
안 일어났다"와 "일어났는데 주입이 안 됐다"가 같은 음성이 되고, 이 경로는 원장에도 세션
이벤트에도 흔적이 없다(`EnsureSession`은 재호출 시 `session_start`를 재발행하지 않는다).

**④ §8.2의 "발효 시각을 손으로 적는다"가 기계화된다.** ③의 결과로 **첫 `hint-` 줄의
타임스탬프가 곧 발효 시각이고 그 줄의 수가 곧 압축 발화 횟수**다. 원장에 쓰지 않는 것은 선택이
아니라 구조다 — readOnly Open은 ledger를 개설하지 않고(`s.ledger`가 nil), writable Open은
`lockStoreCtx`·`migrate`를 타서 세션 시작의 락 경합에 걸린다.

**⑤ D13 셈 정정.** §4.3이 새 질의를 "넷째 구현"이라 했으나 `shadowOwnedHashQuery`를 `EXISTS`로
감싸는 코드는 **이미 있다**(`lastIndexedAtByHashQuery`). 새 질의는 최소 다섯째 소비자이고 베낄
형태가 그 함수에 있다. 결론(상수 재사용)은 그대로 옳다.

**⑥ 알려진 한계 다섯** — 설계로 없애지 않고 적어 둔다.

- **재고 0에서 전(全)스캔이다.** `EXISTS`는 적격 행을 **찾았을 때** 멈추므로 조용해야 할
  경로(귀속 0)가 가장 비싸다. ctx 예산이 상한을 걸고 초과는 `hint-unavailable`로 남는다.
- **서버가 살아 있는 동안 ro 조회가 실패할 수 있다**(`SizeStats` 독스트링의 기록). 다만 세션
  60이 라이브 서버와 동시에 `mode=ro`로 여러 번 조회해 전부 성공했으므로 `[실측]` 흔한 실패는
  아니다. 빈도는 `hint-` 줄의 사유 비율로 관측한다.
- **나이 예산이 없어 곧 지워질 재고를 "있다"고 말할 수 있다.** 세션 60 §3.10이 그 반증
  사례다(다른 호스트의 서버가 낮에 72h 퍼지를 돌렸다). 비용은 헛걸음 한 번이다.
- **`source` 열거의 출처가 갈린다.** 이 문서는 다섯(`fork` 포함 — 호스트 문서 확인)으로 적었고
  저장소가 보관한 인용은 넷이다. `compact`가 그 안에 있다는 것만은 양쪽이 일치한다.
- **★ 문장의 프로젝트와 검색의 프로젝트가 서로 다른 프로세스에서 정해진다.** 훅은 stdin
  payload의 `cwd`로 `contentDir`을 조립하고, `ctr_search`는 **서버 프로세스**가 자기
  `--root`/cwd로 연 store에 묶인다. storeRoot 해석(`--store-root` → `CTR_STORE_ROOT` → OS
  기본)도 둘이 따로 한다. `--profile global-search`는 한술 더 떠 **현재 프로젝트 store를 아예
  열지 않고** `--projects` allowlist만 연다. 두 축이 어긋나면 문장은 참인데 검색은 다른 곳을
  본다. 훅에서 서버의 설정을 볼 방법이 없으므로 설계로 없앨 수 없다. 가설이 아니다 — 세션 60
  §3.10이 같은 계열(두 프로세스가 env를 따로 받아 한쪽만 336h를 못 받았다)을 실측했다.

**⑦ 외부 검토가 제기하고 코드가 답한 것 셋** — 되묻히기 쉬운 자리라 답을 남긴다
`[검증 — 2026-08-14, 코드 대조]`.

- **"재고는 있는데 아직 검색이 안 되는 구간" 은 없다.** `schemaV1`의 `chunks_ai` AFTER INSERT
  트리거가 `chunks` 삽입마다 `fts_porter`·`fts_trigram`에 같이 넣고, `Store.Register`가
  artifacts→chunks→sources를 `txRetry` **한 트랜잭션**에서 처리한다. 가시성 경계가 곧 커밋이다.
  `MergeFTS`가 거는 것은 FTS5 `'optimize'`(세그먼트 압축, D102)이지 색인 등록이 아니므로
  **병합을 한 번도 안 돌려도 검색된다.** 다만 존재 ≠ 임의 질의 히트는 남는다 —
  `normalizeQuery`가 토큰을 AND로 묶으므로 "있다"가 참이어도 특정 질의는 0건일 수 있다.
- **다른 프로젝트의 hook 행이 이 문장을 띄우는 경로는 없다.** `shadowOwnedHashQuery`에 프로젝트
  열이 없는 것은 결함이 아니라 **연 파일이 곧 범위**이기 때문이다(`<storeRoot>/projects/<ProjectID>`).
  worktree·하위 디렉터리·심링크는 `findGitProjectRoot`와 `ident.RealPath`가 한 ID로 모은다.
  비-git 디렉터리는 갈라지는(누락) 방향이지 섞이는 방향이 아니다.
- **회수 경로의 신뢰 경계는 셋 중 둘이 이미 문서에 있다.** 이 레버는 `ctr_search`/`ctr_fetch`
  경로를 새로 만들지 않고 **더 쓰이게** 만들 뿐이지만, 그 경로가 컨텍스트로 들여오는 것은
  포착된 명령 출력이라 공격자가 심을 수 있는 텍스트다. 검색·회수 결과의 untrusted 취급은
  v0.0.1 §4.0(구조적 펜싱 + `untrusted: true`, 다만 "모델 자문 라벨이며 강제 경계가 아님"으로
  스스로 등급을 낮춘다)과 `CONTEXT_ROUTER_HANDOFF.md` §8.2·§8.4에, 프로젝트 격리는 같은 문서
  §6("우발적 혼선 방지이지 보안 경계가 아니다")에 있다. **없는 것은 artifact id 접근 범위**다 —
  id 추측·열거를 보안 속성으로 논한 절이 문서 전역에 없다. 이 레버는 문면에 id를 싣지 않으므로
  그 구멍을 넓히지 않는다. 착수 문턱이 아니라 별건으로 남긴다.
```

- [ ] **Step 2: 그 절 끝에 `compact` 실측 결과를 적는다**

T0(§3.1)는 헤드리스 신규 세션이라 `source=startup`에서 났다. `compact`에서 발화하는지, 그리고 그 주입이 압축 **후** 컨텍스트에 들어가는지는 별도 실측이었다. 세션 60이 쟀고 통과했다. 아래를 그대로 적는다:

```markdown
**⑧ `compact` 실측 — 통과.** 세션 60이 실제 `/compact`로 쟀다. 프로브 훅이 stdin의 `source`를
파일에 적고 같은 값을 `additionalContext` 마커에 실었다. 결과: 사이드카에 `source=compact` **한
줄**(startup 발화가 섞이지 않았다), 그리고 **같은 마커가 압축 후 컨텍스트에 도착했다** — 압축이
주입을 삼키지 않는다. §4.1의 자리 선택과 §4.2의 `source == "compact"` 게이트가 둘 다 실측으로
선다. ⑥의 넷째 항목("`source` 열거의 출처가 갈린다")도 이만큼 좁혀진다: 열거의 길이는 여전히
미확정이나 `compact`가 **호스트가 실제로 내보내는 값**이라는 것은 이제 문서 인용이 아니라 실측이다.
`[실측 — 2026-08-14]`
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
