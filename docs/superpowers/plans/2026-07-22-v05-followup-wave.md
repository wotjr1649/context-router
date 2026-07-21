# v0.5 Follow-up Wave Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** v0.4 최종 리뷰 이월분 일괄 해소 — `CTR_SHADOW_WARN_BYTES`→`CTR_STORE_WARN_BYTES` 개명(사용자 결정 2026-07-22), Codex 설치 테스트 갭 2건, buildCodexHookCommand 인용 실측, 문서·주석 정밀화.

**Architecture:** 신규 설계 없음 — 전 항목이 v0.4 최종 리뷰(세션 16)에서 이미 triage된 처방을 그대로 집행한다. 코드 태스크는 TDD, 실측 태스크(Task 3)는 컨트롤러가 실호스트 codex로 수행한다.

**Tech Stack:** Go(stdlib only), 기존 테스트 하네스(`internal/cli`, `internal/hook`).

## Global Constraints

- Branch `feat/v0.5-followup-wave`, **BASE = main `f2d9bed4ad0bdfecdabe61e4b41bbcf12670e223`** (SDD 원장 기준 — `HEAD~1` 금지).
- Go 테스트는 항상 `-p 1` (메모리 캡 규칙).
- 파일 출력 UTF-8(no BOM)·LF. `git add -A` 금지(untracked `.claude/`·`.codex/` 보호) — 파일 명시 add만.
- `docs/superpowers/plans/2026-07-21-v04-channel-expansion.md`·`docs/prompts/*`는 **불변 기록 — 구 env명이 남아 있어도 수정 금지**. 살아있는 정본(`docs/context-router-design-v0.4-ko.md`)만 제자리 개정.
- 개명은 **별칭 없는 완전 교체**(신규 knob·단일 사용자 — 사용자 결정: 호환 비용 0 시점).
- 버전 범프 없음(릴리스 웨이브 아님 — 상수 0.4.0 유지).
- 커밋 트레일러: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- 서브에이전트 Agent 호출에는 `model: "opus"` 명시.

## File Structure

- `internal/cli/cli.go` — storeWarnBytes 함수·주석·doctor [14] 경고 문구 (Task 1)
- `internal/cli/cli_test.go` — env명 교체 + storeWarnBytes 직접 단정 (Task 1)
- `docs/context-router-design-v0.4-ko.md` — D38 문면 2줄(env명+100MiB), §2 업그레이드 순서 불릿 (Task 1·4)
- `internal/cli/hook_install_test.go` — isOurCodexGroup 에지 + uninstall --codex e2e (Task 2)
- `internal/cli/hook_install.go` — buildCodexHookCommand 인용 주석(실측 결과 반영) (Task 3)
- `internal/hook/shadow.go` — commandDumpPath "이름 기반" 주석 정밀화 (Task 4)
- `internal/hook/hook_test.go` — TestPsAbsPath:1425 "ToSlash" 잔재 주석 (Task 4)

**실행 형태(v0.3 follow-up 웨이브 선례):** Task 1·2·4 = 구현 서브에이전트 1기 번들 파견(작은 처방형 태스크 3건 — 태스크별 신선 서브에이전트 대신 웨이브 번들, ADR-0008 스폰 비용 근거). Task 3 = 컨트롤러 실측(실호스트 codex 필요). 리뷰 체크포인트는 최종 1곳: 서브에이전트(opus) whole-branch + `Codex review --base f2d9bed` 병렬 1패스.

---

### Task 1: `CTR_STORE_WARN_BYTES` 개명 (+비양수 분기 직접 단정, D38 문면·100MiB 표기)

**Files:**
- Modify: `internal/cli/cli.go:32-41` (상수 주석·함수 주석·env명), `internal/cli/cli.go:1312` (경고 문구 내 env명)
- Modify: `internal/cli/cli_test.go:264` (t.Setenv env명) + 직접 단정 테스트 추가
- Modify: `docs/context-router-design-v0.4-ko.md:49`, `:188` (D38 문면 — env명 개정 + 100MB→100MiB)

**Interfaces:**
- Consumes: 기존 `storeWarnBytes(getenv func(string) string) int64`, `defaultStoreWarnBytes` (시그니처 불변).
- Produces: 없음(이름·문면만 변경 — 호출부 무변).

- [ ] **Step 1: 직접 단정 테스트 작성(신규 — 개명 후 이름 기준)**

`internal/cli/cli_test.go`의 `TestRunDoctor_StoreSizeWarn` 위에 추가:

```go
// D38 — storeWarnBytes 오버라이드 채택 규칙 직접 단정(v0.4 최종 리뷰 이월): 양수만 채택,
// 비양수·파싱 실패·미설정은 기본값(100MiB) 폴백.
func TestStoreWarnBytes(t *testing.T) {
	cases := []struct {
		env  string
		want int64
	}{
		{"5", 5}, {"0", defaultStoreWarnBytes}, {"-1", defaultStoreWarnBytes},
		{"abc", defaultStoreWarnBytes}, {"", defaultStoreWarnBytes},
	}
	for _, c := range cases {
		getenv := func(k string) string { // 키 대조 — 신규 env명을 정확히 읽는지가 단정의 일부
			if k == "CTR_STORE_WARN_BYTES" {
				return c.env
			}
			return ""
		}
		if got := storeWarnBytes(getenv); got != c.want {
			t.Fatalf("storeWarnBytes(env=%q)=%d want %d", c.env, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/cli -p 1 -run TestStoreWarnBytes`
Expected: FAIL — `{"5", 5}` 케이스에서 100MiB 반환(신규 env명 `CTR_STORE_WARN_BYTES`를 아직 읽지 않음).

- [ ] **Step 3: cli.go 개명**

`internal/cli/cli.go:32-41`을 다음으로 교체(시그니처·기본값 불변, 주석의 "100MB"도 100MiB로):

```go
// defaultStoreWarnBytes — D38 store 용량 경고 임계 기본값(설계 v0.4 §5 "기본 100MiB").
const defaultStoreWarnBytes = 100 << 20 // 100MiB

// storeWarnBytes — CTR_STORE_WARN_BYTES 양수만 채택, 파싱 실패·비양수는 기본값(D38 — 측정
// 실체가 CAS 전체 blob이라 STORE 명명; 구명 CTR_SHADOW_WARN_BYTES는 v0.5 개명, 별칭 없음).
func storeWarnBytes(getenv func(string) string) int64 {
	if v, err := strconv.ParseInt(getenv("CTR_STORE_WARN_BYTES"), 10, 64); err == nil && v > 0 {
		return v
	}
	return defaultStoreWarnBytes
}
```

`internal/cli/cli.go:1312` 경고 문구의 `CTR_SHADOW_WARN_BYTES`를 `CTR_STORE_WARN_BYTES`로 교체(그 외 문구 byte 불변):

```go
			fmt.Fprintf(w, "[14] warning: blob %dB > 임계 %dB(CTR_STORE_WARN_BYTES) — 수동 구제는 purge 계열 CLI(현행 purge는 source_kind 무구분 삭제 — shadow만 선택 삭제 불가). 자동 삭제 없음\n", sz.BlobBytes, warn)
```

`internal/cli/cli_test.go:264`:

```go
	t.Setenv("CTR_STORE_WARN_BYTES", "5") // blob 10B > 5B → 발화
```

- [ ] **Step 4: 통과 + 구명 잔재 0 확인**

Run: `go test ./internal/cli -p 1 -run 'TestStoreWarnBytes|TestRunDoctor_StoreSize'`
Expected: PASS (3테스트).
Run: Grep `SHADOW_WARN` glob `*.go` → **매치 0** (docs의 불변 기록 매치는 정상).

- [ ] **Step 5: 설계 D38 문면 제자리 개정**

`docs/context-router-design-v0.4-ko.md:49`:

```
  추가(기본 100MiB, `CTR_STORE_WARN_BYTES` 오버라이드 — v0.5 개명: 구
  `CTR_SHADOW_WARN_BYTES`, 측정 실체 정합·별칭 없음). 자동 삭제 없음.
```

`:188` (개정 후 줄번호 주의 — L49가 2줄이 되며 이후 +1 시프트):

```
  (기본 100MiB, `CTR_STORE_WARN_BYTES` 파싱 실패·비양수 → 기본값) 시에만
```

- [ ] **Step 6: Commit**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go docs/context-router-design-v0.4-ko.md
git commit -m "feat(v0.5): CTR_STORE_WARN_BYTES 개명 — D38 측정 실체 정합(사용자 결정, 별칭 없음) + 채택 규칙 직접 단정 + 문면 100MiB"
```

---

### Task 2: Codex 설치 테스트 보강 — isOurCodexGroup 직접 에지 + uninstall --codex run 분기 e2e

**Files:**
- Modify: `internal/cli/hook_install_test.go` (테스트 3개 append — 프로덕션 코드 무변)

**Interfaces:**
- Consumes: `isOurCodexGroup(raw json.RawMessage) bool`, `runHookInstall(args []string, storeRoot, storeRootRaw string, storeRootExplicit bool, projectRoot, version string, stdout io.Writer) error`, `runHookUninstall(args []string, projectRoot string, stdout io.Writer) error` (기존 그대로).
- Produces: 없음(테스트만).

- [ ] **Step 1: 테스트 3개 작성**

`internal/cli/hook_install_test.go` 말미(`TestRunHookInstallCodex` 뒤)에 추가:

```go
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
```

주의: `errors` 임포트가 `hook_install_test.go`에 없으면 추가(파일 상단 import 블록 — gofumpt 정렬).

- [ ] **Step 2: 통과 확인 (신규 테스트는 기존 동작 단정 — 즉시 GREEN이 정상)**

Run: `go test ./internal/cli -p 1 -run 'TestIsOurCodexGroupEdges|TestRunHookUninstallCodex'`
Expected: PASS (3테스트). FAIL이면 신규 테스트의 전제가 틀린 것 — 프로덕션 코드를 고치지 말고 STOP·보고(이 태스크는 커버리지 보강이지 동작 변경이 아님).

- [ ] **Step 3: Commit**

```bash
git add internal/cli/hook_install_test.go
git commit -m "test(v0.5): isOurCodexGroup 직접 에지 단정 + uninstall --codex run 분기 e2e — v0.4 최종 리뷰 이월"
```

---

### Task 3 (컨트롤러 실측): buildCodexHookCommand 인용 — 실 Codex 토크나이즈 검증

**Files:**
- Modify: `internal/cli/hook_install.go:251-253` (주석 — 실측 결과 반영)
- (실패 시에만) Modify: `internal/cli/hook_install.go:254-263` + 테스트 (관측된 규칙에 맞춘 인용 수정)

**Interfaces:** 없음(주석/실측; 성공 경로는 코드 무변).

- [ ] **Step 1: 공백 포함 store-root로 스크래치 설치**

스크래치 디렉터리(세션 scratchpad 하위, 경로에 **공백 포함**) 준비:
`<scratch>\ctr quote probe\proj` (프로젝트) + `<scratch>\ctr quote probe\store` (store root).
proj에서: `context-router hook install --codex --store-root "<scratch>\ctr quote probe\store"`
확인: `<proj>\.codex\hooks.json`의 command가 `context-router codex-hook --store-root '<abs>'` 홑따옴표 인용 형태.

- [ ] **Step 2: 실 codex 발화**

proj를 cwd로 `codex exec --dangerously-bypass-hook-trust "<간단 프롬프트>"` 1회.
(JSON 프로브 파일이 필요하면 반드시 Write 도구로 — bash heredoc 백슬래시 붕괴 금지. 세션 16 교훈.)

- [ ] **Step 3: 판정**

- `<scratch>\ctr quote probe\store\session.db` 존재 + `context-router --store-root "<...>\store" session export`(또는 summary)에 `cx:` 세션 기록 → **인용 유효** (JSONL 필드는 camelCase `sessionId`/`eventType` — DB 컬럼명 grep 금지).
- 실패 징후: 의도 경로에 session.db 부재, 스크래치 주변에 잘린 경로 조각 디렉터리(`'C:...` 등) 생성, 이벤트 무기록.

- [ ] **Step 4-A (인용 유효 시): 주석만 갱신**

`internal/cli/hook_install.go:251-253` 주석의 `(가정 — Codex 훅 명령 파싱 규칙은 실측 전, 도그푸딩은 --store-root 미명시 경로만 사용)` 부분을:

```go
// buildCodexHookCommand — buildHookCommand의 Codex 형제: 러닝 서브커맨드가 `codex-hook`이다
// (D35 호스트 경계 + §11.2 F3 버전 게이트). 인용 규칙은 T11 관례 승계 — 2026-07-22 실측:
// 공백 포함 --store-root가 홑따옴표 인용으로 무변형 전달됨(실 codex exec, Windows).
```

- [ ] **Step 4-B (인용 무효 시): STOP — 관측된 실 토크나이즈 규칙을 보고하고 수정 방향 합의 후 buildCodexHookCommand 인용 방식 수정 + 단위 테스트(관측 규칙 재현) 추가. 맹목 재시도 금지.**

- [ ] **Step 5: 스크래치 정리는 사용자 승인 후 수동(삭제 가드) — 잔재 목록만 보고. Commit:**

```bash
git add internal/cli/hook_install.go
git commit -m "docs(v0.5): buildCodexHookCommand 인용 규칙 실측 반영 — 공백 store-root 홑따옴표 전달 확인"
```

---

### Task 4: 문서·주석 웨이브 — 업그레이드 순서 노트 + 주석 뉘앙스 2건

**Files:**
- Modify: `docs/context-router-design-v0.4-ko.md` §2 말미(L132 뒤, 설치 불릿 다음)
- Modify: `internal/hook/shadow.go:115-116` (commandDumpPath 주석)
- Modify: `internal/hook/hook_test.go:1425` (인라인 주석)

**Interfaces:** 없음(문서·주석만 — 코드 동작 무변, 테스트 문면 불변).

- [ ] **Step 1: 설계 §2에 업그레이드 순서 불릿 추가** (`## 3.` 직전, 설치 불릿 다음):

```
- 업그레이드 순서: 바이너리 교체(`go install`)가 `hook install --codex` 재등록보다
  먼저다 — hooks.json의 `codex-hook`은 v0.3 이하 구 바이너리에서 미지 서브커맨드
  exit 1(버전 게이트, §11.2 F3)이므로 신 hooks.json + 구 바이너리 창에서는 cx:
  캐프처만 침묵 불발한다(오귀속 없음). Claude 쪽 `hook` 서브커맨드는 구버전도
  인식하므로 같은 창에서 cc: 캐프처는 지속된다.
```

- [ ] **Step 2: shadow.go 주석 정밀화** — `internal/hook/shadow.go:115-116`의 문장

기존: `않는다 — 대조는 이름 기반(ingest.DeniedFilename)이라 상대경로 덤프도 커버한다(§11.1 파생 ①).`

교체:

```go
// 현행대로 색인한다(잔여 표면은 설계 v0.4 §7 한계 명문화, Redact·sniff 의존). 절대화는 하지
// 않는다 — 대조는 basename 글롭 + `.docker/config.json` 경로 접미(ingest.DeniedFilename)라
// 상대경로 덤프도 커버한다(§11.1 파생 ①).
```

- [ ] **Step 3: hook_test.go:1425 인라인 주석** — `// ToSlash 정규화`(CI F1로 제거된 API 잔재)를:

```go
		{"windows", `C:\big\f.log`, "C:/big/f.log"}, // goos-키 백슬래시→슬래시 정규화(호스트 무관)
```

- [ ] **Step 4: 게이트**

Run: `go test ./internal/hook ./internal/cli -p 1` → PASS (주석만 변경 — 무회귀 확인).
Run: `gofumpt -l internal` → 출력 없음.

- [ ] **Step 5: Commit**

```bash
git add docs/context-router-design-v0.4-ko.md internal/hook/shadow.go internal/hook/hook_test.go
git commit -m "docs(v0.5): 업그레이드 순서 노트(§2) + commandDumpPath 접미 규칙·psAbsPath goos-키 주석 정밀화"
```

---

### 최종 체크포인트 (컨트롤러)

- [ ] 전체 게이트: `go test ./... -p 1` GREEN, `gofumpt -l .` 출력 없음, `go vet ./...` clean.
- [ ] 이중 최종 리뷰(병렬 1패스): 서브에이전트(opus, whole-branch base `f2d9bed`) + `node "<companion>" review --base f2d9bed` — 발견 병합 후 반영, 재리뷰는 서브에이전트만.
- [ ] PR → CI 3-OS GREEN → merge → SDD 원장·session-17 기록.
- 스킵 명시: 설계 §11 계획-이탈 3건 추기(세션 16 "could" 항목 — 원장·기록에 이미 존재, YAGNI), storeWarnBytes doctor 레벨 비양수 e2e(직접 단정으로 충분).
