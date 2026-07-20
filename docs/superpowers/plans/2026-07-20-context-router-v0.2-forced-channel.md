# Context Router v0.2 (forced channel — Claude Code 훅 패키징) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claude Code 훅 채널(`context-router hook`) — 자동 계측 이벤트 9종 + Shadow Recall 패시브 인덱싱 + large-read guard + `hook install`/`usage`/doctor 확장 + 부채 6건 편승 (설계 v0.2 전문, D21~D28).

**Architecture:** 신규 `internal/hook` 패키지(stdin 파싱·분류·guard·shadow), `internal/session`에 훅 전용 append API(`OpenAppend` — 현행 `Open()`은 재사용 불가, 설계 §2.1), ingest는 소형 예외 2건(denylist 조회 노출 + SourceKind 1필드), cli가 hook/usage 서브커맨드 디스패치와 install/doctor를 소유. 훅은 단명 프로세스·fail-open(항상 exit 0)·deadline 예산 2s.

**Tech Stack:** 기존 그대로 — 신규 의존성 0.

## Global Constraints

- **정본 계약: `docs/context-router-design-v0.2-ko.md` 전문**(D21~D28, §1~§13). 본 계획의 모든 수치·문구·순서는 그 문서가 우선 — 충돌 시 설계서를 따르고 계획 수정을 보고.
- 선행: v0.1.0 코드 기반(main d4f45ab 이후). 실행 브랜치 `feat/v0.2-forced-channel`(superpowers:using-git-worktrees).
- v0.0.1/v0.1 규약 승계: 자기 정의 인터페이스 0 · 핸들러 ≤50줄 · mcp는 database/sql·net/http·os/exec import 금지 · 오류 sentinel→코드 매핑 `toToolError` 단일점 · 이벤트 상한(type ≤64B `[a-z0-9_]+`·summary ≤2048B·attributes ≤4096B·총 ≤8KB).
- D13 배치: 신규 소스는 `internal/hook/`(hook.go+hook_test.go+testdata/; 응집 이음새 분리 **사전 승인**: classify.go·shadow.go·guard.go — 파일당 선호 밴드 준수). session 변경은 OpenAppend 계열 + `ValidateEvent` 이관, ingest 변경은 `Request.SourceKind`·`Report.Hash` 2건(denylist는 기존 공개 `ingest.DeniedFilename` 재사용 — 신규 별칭 금지), store 변경은 open-lock 대기 ctx-aware 변형 1건, cli는 hook install/uninstall·usage·doctor 확장. T4가 `docs/context-router-code-architecture-ko.md` 의존 그래프에 internal/hook을 등재.
- 훅 계약(설계 §2): 명령 `context-router hook`(PATH 실행 파일명 — `ctr` 아님) · **exit 항상 0** · env: `CTR_HOOKS_OFF`, `CTR_SHADOW_OFF`, `CTR_HOOK_DEADLINE_MS`(기본 2000), `CTR_GUARD_READ_MAX`(기본 262144), `CTR_SHADOW_MIN`(기본 16384), `CTR_SHADOW_MAX`(기본 1048576), `CTR_HOOK_RETENTION_SEC`(기본 2592000), `CTR_STORE_ROOT`.
- 세션 식별(설계 §2.2): host session_id는 canonical UUID 검증 후 `cc:<uuid>`(불합격 = drop) · 세션 생성은 SessionStart만 · 미지 세션 이벤트 drop · session_start는 sessions 행 신규 삽입 시 1회.
- summary·payload는 allowlist 조립(§3) + `ingest.Redact` 2차. drops 사이드카 `session.drops.log`(`<unix-ts>\t<사유>\n`, O_APPEND).
- 테스트: `go test -p 1 ./...`(메모리 캡 전역 규칙), secret canary 분할 리터럴, 응답 분할 규율(파일 전체 재작성 금지, 긴 데이터는 `strings.Repeat`/testdata).
- 게이트: 설계 §10 전문. 호스트 의존 비결정 검증(T11)은 비차단.
- 커밋 trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. 리뷰: 태스크 리뷰 서브에이전트, 통합·최종 체크포인트는 서브에이전트 + Codex 병렬 1패스(재리뷰는 서브에이전트만). BASE는 SDD 원장 기준(`HEAD~1` 금지).

---

### Task 0: [plan 검증] 훅 API 사실 확인 5건 + 골든 픽스처 (설계 §1.3)

**Files:** Create `internal/hook/testdata/pretooluse-read.json`, `posttooluse-bash.json`, `posttooluse-write.json`, `posttooluse-error.json`, `sessionstart.json`, `internal/hook/testdata/README.md`

**Interfaces:**
- Produces: 이후 전 태스크의 stdin 골든 픽스처(실측 페이로드 형태). README.md에 검증 결과 5건 기록 — 이후 태스크는 이 기록을 계약으로 소비.
- Consumes: 없음(leaf, 코드 없음).

- [ ] **Step 1: 문서 검증** — code.claude.com/docs 훅 문서(WebFetch 또는 claude-code-guide 서브에이전트)로 ① PostToolUse가 도구 실패 시에도 발화하는지 + `tool_response` 오류 신호 형태 ② PreToolUse `permissionDecision` JSON 스키마(`hookSpecificOutput.permissionDecision: "deny"` + `permissionDecisionReason`) ③ SessionStart payload(`source`: startup/resume/clear/compact) ④ settings.json `hooks` 스키마(matcher·hooks 배열·timeout 단위) 확인.
- [ ] **Step 2: 픽스처 작성** — ①~④ 검증된 실형태로 5개 JSON + README.md(발견 기록). ⑤ `tool_response` 최대 크기·절단 정책은 **T11 실호스트 스모크에서 실측 확인할 가정**으로 README에 명시(사용자 실설정 파일을 격리 없이 편집하는 실측 스텝은 재현성·복구 안전성 문제로 배제 — 리뷰 반영). 문서 단계에서 절단이 확인되면 설계 §5의 "전문 확보" 전제 수정을 보고하고 중단(사용자 결정).
- [ ] **Step 3: Commit** — `"test(hook): 훅 stdin 골든 픽스처 + API 검증 기록 (v0.2 설계 §1.3, ⑤는 T11 실측 가정)"`

### Task 1: 부채 정리 A — 코드 4건 (설계 §9)

**Files:** Modify `internal/search/search.go`(+test), `internal/session/recover.go`(+test), `internal/mcp/mcp.go`(record_event description), `internal/cli/cli.go`(recover 오류 문구)

**Interfaces:** 전부 leaf(동작 계약 무변) — ① `filterShortTokenCandidates` ~45줄 중복 파라미터화(공통 헬퍼 1개로 흡수) ② recover의 `.bak` family ts-orphan 잔존물 정리(같은 ts 계열 미완 파일 스윕) ③ `ctr_record_event` description에 attributes float64 캐비앗 문구(JSON 숫자는 float64 — 큰 정수 정밀도 주의) ④ recover worktree-absent 오류 메시지 UX(존재하지 않는 worktree id 지정 시 후보 목록 안내).

- [ ] **Step 1: 실패 테스트** — ① 파라미터화 전후 동일 입력→동일 출력(기존 테스트 유지 + 헬퍼 직접 테스트 1건) ② ts-orphan: 시드된 미완 `.bak` 계열 → recover 후 부재 ③ description 문구 존재 assert ④ 부재 worktree → 오류 문구에 후보 나열.
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/search/ ./internal/session/ ./internal/mcp/ ./internal/cli/ -run 'TestFilterShort|TestRecover|TestRecordEventSchema' -v`.
- [ ] **Step 3: 구현** — 각 항목 최소 diff. 인접 코드 개선 금지.
- [ ] **Step 4: GREEN** — 전체 회귀 `go test -p 1 ./...`.
- [ ] **Step 5: Commit** — `"chore: v0.2 개막 부채 정리 A — 중복 파라미터화·bak ts-orphan·float64 문구·recover UX (설계 §9)"`

### Task 2: 부채 정리 B — 테스트 2건 (설계 §9)

**Files:** Modify `internal/session/session_test.go`, `internal/store/store_test.go`

**Interfaces:** leaf — ① modernc query-plan 의존 corruption fixture(헤더 훼손 외 페이지 중간 훼손 변형 추가, quick_check 판정 안정성) ② `migrateBusyRetry` 분기 단위 테스트(BUSY 재시도 경로·소진 경로).

- [ ] **Step 1: 테스트 작성** — 위 2건. 구현 변경 없음(테스트만 — 기존 동작의 게이트 보강).
- [ ] **Step 2: GREEN 확인** — `go test -p 1 ./internal/session/ ./internal/store/ -v`. 실패 발견 시 = 실제 버그 → 별도 보고 후 최소 수정.
- [ ] **Step 3: Commit** — `"test: corruption fixture 변형 + migrateBusyRetry 분기 게이트 (설계 §9)"`

### Task 3: 훅 전용 session append API (설계 §2.1~§2.3)

**Files:** Modify `internal/session/session.go`(OpenAppend 계열), `internal/session/session_test.go`

**Interfaces:**
- Consumes: `store.AcquireLock`(shared). 주의 2건(리뷰 반영): ① `pragmas` 상수는 busy_timeout(5000) 내장이라 **직접 재사용 불가** — 500ms 변형 DSN을 조립한다. ② 기존 `(*DB).Append`는 직렬화+INSERT만 수행하며 **상한 검증이 없다**(검증은 mcp `validateRecordEventInput` 전용) — 아래 ValidateEvent 이관이 이를 해소한다.
- Produces:
  - `type AppendOptions struct{ ExternalSessionID, Producer string; RetentionSec int64 }` / `func OpenAppend(ctx context.Context, dir string, opts AppendOptions) (*AppendDB, error)` — 순서: ① shared lease(논블로킹, 실패 `ErrLeaseHeld`) ② recover 마커 → `ErrRecoverPending` ③ DB 미존재 시 init lock 생성 직렬화 — **현행 `acquireInitLock`은 최대 5s 무조건 sleep이라 재사용 불가**: ctx-aware 대기 변형(deadline 관측 폴링)으로 작성 ④ PRAGMA+멱등 DDL — **busy_timeout 500ms DSN 변형**(deadline 예산 안, 설계 §2.3) ⑤ **quick_check 생략**(손상은 append 시점 오류 분류로 사후 감지) ⑥ sessions INSERT·session_start **하지 않음**. `opts.ExternalSessionID`는 이미 `cc:` 접두사 완성형(형식 검증은 호출자 = hook, T4).
  - `func ValidateEvent(ev Event) error` — 상한·형식 검증의 **session 계층 이관**(리뷰 반영: type ≤64B `[a-z0-9_]+`·summary ≤2048B·attributes ≤4096B·refs/related 개수·항목 크기·총 ≤8KB — mcp `validateRecordEventInput`(mcp_session.go)은 wire 변환 후 이 함수 호출로 축소, 규칙 중복 금지). `AppendDB.Append`가 저장 전 항상 호출.
  - `func (d *AppendDB) EnsureSession(ctx context.Context, source, worktreeRoot string) (created bool, err error)` — sessions `INSERT OR IGNORE`와 **신규 삽입 시의 `session_start` append를 단일 트랜잭션**으로(리뷰 반영 — 단명 프로세스가 두 작업 사이에서 kill되면 sessions 행만 남아 session_start가 영구 누락되는 경로 차단).
  - `func (d *AppendDB) SessionExists(ctx context.Context) (bool, error)` — sessions 행 존재 조회.
  - `func (d *AppendDB) Append(ctx context.Context, ev Event) (id int64, eventID string, ts int64, err error)` — 기존 Append와 동일 검증·상한, ctx deadline을 txRetry에 전파(초과 시 `context.DeadlineExceeded` 반환 — drop 판정은 호출자).
  - `func (d *AppendDB) Close() error` — lease 해제 포함. MCP 표면 비노출(mcp 패키지가 import하지 않음 — 게이트).

- [ ] **Step 1: 실패 테스트** — ① OpenAppend가 sessions 행·session_start를 만들지 않음 ② EnsureSession 신규 → created=true + sessions 행 + session_start 1건 / 재호출 → created=false + session_start 여전히 1건(clear/compact 재발화 모사) ③ SessionExists 참/거짓 ④ Append가 ExternalSessionID로 기록(`cc:` 세션 조회로 검증) ⑤ recover 마커 → ErrRecoverPending ⑥ 이미 취소된 ctx로 Append → DeadlineExceeded 계열(블로킹 없이 즉시) ⑦ 기존 Open()과의 공존: Open 세션 + OpenAppend 세션 동시 append 무손실(shared+shared) ⑧ quick_check 미실행(파일 헤더 훼손 상태에서도 OpenAppend 자체는 성공, 첫 Append에서 오류 분류) ⑨ ValidateEvent 경계 4종(type 65B·summary 2049B·attributes 4097B·총 8KB 초과) + 타입 정규식 위반 → 오류(저장 0건) ⑩ EnsureSession 원자성: session_start append 실패 주입 시 sessions 행도 롤백(둘 다 부재) ⑪ mcp record_event 상한 테스트 회귀 무변 GREEN(검증 이관 후).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/session/ -run 'TestOpenAppend|TestEnsureSession|TestValidateEvent' -v` → undefined.
- [ ] **Step 3: 구현** — Open()과 공통 단계는 비공개 헬퍼로 추출(중복 금지, 동작 불변). AppendDB는 ***DB 임베드 금지**(무-ctx `DB.Append`와 시그니처 상이 그림자화 방지) — 필드 재사용.
- [ ] **Step 4: GREEN** — session 전체 회귀(-p 1).
- [ ] **Step 5: Commit** — `"feat(session): OpenAppend — 훅 전용 append API(외부 세션 id·INSERT OR IGNORE·조건부 session_start·quick_check 생략·deadline 전파) (v0.2 설계 §2)"`

### Task 4: context-router hook 골격 — 디스패치·fail-open·deadline (설계 §2)

**Files:** Create `internal/hook/hook.go`, `internal/hook/hook_test.go`; Modify `cmd/context-router/main.go`(서브커맨드 인지), `cmd/context-router/main_test.go`(dispatch 테스트), `internal/cli/cli.go`(디스패치), `docs/context-router-code-architecture-ko.md`(의존 그래프에 internal/hook 등재 + cli 책임줄 갱신)

**Interfaces:**
- Consumes: T3 `session.OpenAppend`/`EnsureSession`/`SessionExists`/`Append`, T0 픽스처, main의 `storeRootFor`(우선순위 플래그>`CTR_STORE_ROOT`>기본 — 현행 유지), `ident`(cwd→projectRoot/worktree 해석, 서버와 동일 경로).
- Produces:
  - `func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, storeRoot, version string, getenv func(string) string) int` — **항상 0 반환**. 순서: `CTR_HOOKS_OFF` → **stdin을 EOF까지 drain 후** 0(설계 §2.3 "소비 후 exit" — broken pipe 방지, 리뷰 반영) / stdin JSON 파싱(hook_event_name·session_id·cwd·tool_name·tool_input·tool_response·source·error·is_interrupt — T0 확인: 실패는 별도 이벤트 `PostToolUseFailure`로 오며 `tool_response` 없음) → session_id canonical UUID 검증(불합격 = drop 기록 후 0) → `cc:<uuid>` 조립 → **stdin cwd를 ident.Canonicalize로 해석해 session dir = `<storeRoot>/projects/<pid>/worktrees/<wid>` 도출**(서버 main과 동일 규칙 — 디렉터리 join을 임의 결정하지 말 것) → deadline ctx(`CTR_HOOK_DEADLINE_MS` 기본 2000) → 이벤트별 분기: SessionStart = EnsureSession / 그 외 = SessionExists false → drop(사유 `unknown-session`), true → 처리(T5~T7). `CTR_HOOK_RETENTION_SEC`(기본 2592000)을 읽어 `AppendOptions.RetentionSec`으로 전달(설계 §2.2 훅 세션 기본 retention 30일).
  - `func appendDrop(dir, reason string)` — `session.drops.log` 1줄 O_APPEND(자체 오류는 stderr만 — 이중 실패 무시, 설계 §2.3 한계).
  - 훅 입력 struct `hookInput`(비공개) — 픽스처 형태 그대로.
  - cli: `Run(...)`의 sub 집합에 `"hook"` 추가(args 첫 요소 install/uninstall은 T8, 무인자는 hook.Run 위임). **주입 방식 확정**(리뷰 반영): cli.Run 내부에서 `hook.Run(ctx, os.Stdin, stdout, storeRoot, version, os.Getenv)` 호출 — cli.Run 시그니처 무변경(purge의 os.Stdin 직접 사용 선례), cli.Run의 projectRoot 인자는 훅이 stdin cwd로 재도출하므로 **미사용**. main은 `cliSubcommands` 맵에 "hook" 등재.

- [ ] **Step 1: 실패 테스트** — ① `CTR_HOOKS_OFF=1` → 0 반환·DB 파일 미생성 ② 픽스처 sessionstart.json → sessions 행(`cc:` id, retention_sec 기본 2592000)+session_start 1건 ③ 비-SessionStart 픽스처를 미지 세션으로 → 이벤트 0건 + drops 1줄(`unknown-session`) ④ session_id 형식 불량(비UUID 문자열) → drop(`bad-session-id`) ⑤ stdin 파싱 불능(잘린 JSON) → 0 반환 + drops(`bad-input`) ⑥ lease 충돌 주입(exclusive 선점) → 0 반환 + drops(`lease-held`) ⑦ 반환값 항상 0(전 시나리오 공통 assert) ⑧ `CTR_HOOKS_OFF` 경로가 stdin을 EOF까지 drain(계측 reader로 assert) ⑨ **main dispatch**(main_test.go): `dispatchCLI`가 "hook"을 handled=true로 위임 + 미지 서브커맨드 거부 유지(리뷰 반영 — internal/hook만 테스트하면 맵 등재 누락에도 GREEN이 되는 사각).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/hook/ ./cmd/context-router/ -run 'TestHook|TestDispatch' -v` → undefined.
- [ ] **Step 3: 구현** — hook.go에 Run+appendDrop+파싱. cli/main 배선 최소 diff. 아키텍처 문서 의존 그래프에 internal/hook(간선 cli→hook, hook→{session,ingest,store,ident})·cli 책임줄 갱신을 같은 커밋에 편승.
- [ ] **Step 4: GREEN** — hook + cli + main 회귀(-p 1). `GOOS=linux go build ./...` 교차 컴파일.
- [ ] **Step 5: Commit** — `"feat(hook): context-router hook 골격 — stdin 파싱·cc: 세션·fail-open·drops·deadline (v0.2 설계 §2)"`

### Task 5: 계측 매핑 9종 + summary allowlist (설계 §3)

**Files:** Modify `internal/hook/hook.go`, `internal/hook/hook_test.go`

**Interfaces:**
- Consumes: T4 골격(PostToolUse 분기에서 호출), `ingest.Redact`(2차 방어).
- Produces:
  - `func classify(in hookInput) (eventType, summary string, attrs map[string]any)` — 우선순위 error(`hook_event_name=="PostToolUseFailure"` 이벤트명 기준 — T0 확인) > git_diff/build_run/test_run(Bash 패턴표) > file_edit(Write/Edit/NotebookEdit) > tool_call. 패턴표는 패키지 var 정규식 슬라이스 3종(git: `^git (diff|commit|merge|rebase|log|status)`, build: `go build|dotnet build|npm run build|msbuild|make(\s|$)`, test: `go test|dotnet test|pytest|vitest|npm test`) — 테이블 테스트가 계약.
  - summary allowlist 조립: `<도구명>: <허용 요소>` — Bash 첫 토큰은 `^[A-Za-z0-9_./-]+$` 일치 시만 원문, 불일치(env 할당 `KEY=값` 등) → `<arg>`. 파일 도구는 워크스페이스 상대 경로. 오류는 정규화 분류·코드만. 조립 후 `ingest.Redact` 통과 + redaction 상태 기록. attrs도 allowlist 필드만(exit_code·bytes·matched_pattern·relative_path·is_interrupt).
  - error 판정: `hook_event_name == "PostToolUseFailure"`(T0 검증 — `tool_response` 파싱 아님) → event_type=error(분류 최우선). summary는 `error` 문자열의 정규화 분류·코드만(전문 미수용, 설계 §3), attrs에 `is_interrupt`(존재 시).

- [ ] **Step 1: 실패 테스트** — ① 분류 테이블 테스트(git/build/test/기본 각 2케이스 + 우선순위: 실패한 test 명령 = PostToolUseFailure 픽스처(`error` 문자열) → error, test_run 아님) ② file_edit(Write 픽스처 → 상대 경로 summary) ③ env 할당 마스킹(`SERVICE_KEY=abc deploy` → `<arg> deploy` 형태가 아니라 첫 토큰 `<arg>`) ④ secret canary(분할 리터럴)를 tool_input에 심고 → summary·attrs에 원문 부재 ⑤ 상한: summary 2048B 절단 ⑥ 픽스처 round-trip: posttooluse-bash.json → tool_call 1건 저장·FTS에서 canary 미회수(**§10 canary 게이트 세션 측**).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/hook/ -run 'TestClassify|TestSummaryAllowlist' -v`.
- [ ] **Step 3: 구현** — classify+조립 헬퍼. hook.go 1,000줄 접근 시 분리 보고.
- [ ] **Step 4: GREEN** — hook 회귀.
- [ ] **Step 5: Commit** — `"feat(hook): 계측 매핑 9종 중 6종(tool_call·file_edit·git_diff·build_run·test_run·error) + summary allowlist (v0.2 설계 §3)"`

### Task 6: Shadow Recall — 자체 캡·denylist·인덱싱 (설계 §5)

**Files:** Modify `internal/hook/hook.go`(또는 사전 승인 이음새 `shadow.go`)(+test), `internal/ingest/ingest.go`(`Request.SourceKind`+`Report.Hash`), `internal/ingest/ingest_test.go`, `internal/store/store.go`+`store_test.go`(open-lock ctx-aware 변형 — D13 예외)

**Interfaces:**
- Consumes: `ingest.Run`(Content 경로), `ingest.DeniedFilename`(**기존 공개 함수** ingest.go:55 — 신규 별칭 금지, 리뷰 반영), T5 분류 결과. **content store dir = `<storeRoot>/projects/<pid>`**(session dir과 다르다 — worktrees 하위 아님, main과 동일 join. 리뷰 반영). `store.Open(dir, readOnly)`은 **ctx 인자가 없고 open-lock을 최대 5s 무조건 대기**하므로 ctx-aware 대기 변형(옵션 또는 변형 함수 1건, D13 예외 등재)을 추가해 deadline 예산 안에서 실패시킨다.
- Produces:
  - ingest 예외 2건: `Request.SourceKind string`(기본 "inline", 훅은 "hook" — SourceMeta.Kind로 전달), `Report.Hash string`(inline 단건 경로에서 `store.Register`가 계산한 저장 해시 반환 — **현행 Report에는 hash가 없어 artifact URI를 조립할 수 없다**, 리뷰 반영).
  - hook 내 `shadowCapture(ctx, storeRoot ...)` — 판정 순서: `CTR_SHADOW_OFF` → skip / tool_response 직렬화 크기 ≤ `CTR_SHADOW_MIN`(16KiB) → skip / **> `CTR_SHADOW_MAX`(1MiB) → 미저장 + drops(`shadow-oversize`)** / 파일 유래 도구(Read 등)면 tool_input 경로를 `ingest.DeniedFilename` 대조 → 일치 시 skip(drops `shadow-denylist`) / 바이너리 판정(NUL 바이트 sniff) → skip / 통과 시 `ingest.Run{Content, SourceKind:"hook"}` 저장 → `artifact_created` 이벤트 + 기본 이벤트에 더해 `tool_result_summary` 이벤트(artifact ref = `"artifact://" + sessionID + "/sha256-" + Report.Hash` 문자열 조립만, url.Parse 금지) append. 여기까지 9종 중 8종(warning은 T7 guard가 생산).
- [ ] **Step 1: 실패 테스트** — ① 16KiB 이하 → 미저장 ② 초과 → content.db 아티팩트 + artifact_created + tool_result_summary(ref 형식 assert) ③ 1MiB 초과 → 미저장 + drops ④ Read 응답 + denylist 경로(`.env` 픽스처) → 미저장 + drops ⑤ NUL 포함 바이너리 → 미저장 ⑥ secret canary(분할 리터럴)가 응답 본문에 → 저장본에서 span redaction 적용(**§10 canary 게이트 shadow 측**) ⑦ SourceKind="hook"이 SourceMeta.Kind로 저장 ⑧ 해시 dedup(같은 응답 2회 → 아티팩트 1개) ⑨ **URI 해시 정합**: 조립한 ref의 hash == `store.ArtifactHashByID` 결과 ⑩ store open-lock 점유 상태에서 deadline 300ms 내 실패 + drops(ctx-aware 변형).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/hook/ ./internal/ingest/ ./internal/store/ -run 'TestShadow|TestSourceKind|TestReportHash' -v`.
- [ ] **Step 3: 구현** — ingest diff 최소(필드 2건). store 변형은 기존 대기 루프에 ctx 관측 추가(동작 기본값 불변). shadow 판정은 hook 소유.
- [ ] **Step 4: GREEN** — hook·ingest·store 회귀.
- [ ] **Step 5: Commit** — `"feat(hook): Shadow Recall — CTR_SHADOW_MIN/MAX·denylist 대조·바이너리 sniff·hook provenance, 매핑 8/9종 (v0.2 설계 §5)"`

### Task 7: large-read guard — 4조건 판정 (설계 §4)

**Files:** Modify `internal/hook/hook.go`(+test)

**Interfaces:**
- Consumes: T4 골격(PreToolUse 분기), `ingest.Run`(파일 경로 — denylist·캡·워크스페이스 경계 포함 전체 파이프라인), T0 픽스처(pretooluse-read.json).
- Produces: PreToolUse(Read) 처리 — **deny는 4조건 전부 성립 시만**: ① 대상이 projectRoot 경계 내(**정본 §4가 v0.2를 projectRoot만으로 개정 승인** — 훅은 서버 플래그 allow-path를 알 수 없음, allow-path 파일은 통과) ② 전체 파일 읽기(tool_input에 offset/limit 존재 시 통과) ③ 크기 > `CTR_GUARD_READ_MAX`(256KiB, os.Stat) ④ `ingest.Run` 성공 확인 **`Report.Indexed==1`**(denylist·oversize는 무오류 Skipped — err 검사 부족). 성립 → stdout에 `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"<이미 인덱스됨 — ctr_search/ctr_fetch 안내>"}}`(T0 검증 스키마) + `warning` 이벤트(차단 파일 상대 경로·크기). 불성립 → stdout 무출력(allow)·이벤트 없음.

- [ ] **Step 1: 실패 테스트** — ① 경계 밖 대형 파일 → allow(무출력) ② offset/limit 부분 읽기 → allow ③ 임계 이하 → allow ④ denylist 파일(`.env`, Indexed=0 Skipped) → allow(**"이미 인덱스됨" 거짓 안내 부재**) ⑤ 정상 대형 파일 → deny JSON 스키마 일치 + content.db 아티팩트 존재 + warning 이벤트 1건 ⑥ 인덱싱 실패 주입(store 잠금) → allow + drops.
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/hook/ -run 'TestGuard' -v`.
- [ ] **Step 3: 구현** — 판정 함수 1개 + deny 출력 헬퍼.
- [ ] **Step 4: GREEN** — hook 회귀.
- [ ] **Step 5: Commit** — `"feat(hook): large-read guard 4조건 판정 — Indexed==1 확인·경계 밖/부분 읽기 통과·warning 이벤트 (v0.2 설계 §4)"`

### Task 8: hook install/uninstall + doctor 확장 (설계 §7)

**Files:** Modify `internal/cli/cli.go`(+test), `cmd/context-router/main.go`+`main_test.go`(explicit store-root 전달 + dispatch 테스트)

**Interfaces:**
- Consumes: T0 settings 스키마 검증 결과, T4 cli 디스패치.
- Produces:
  - `context-router hook install [--user] [--no-shadow] [--store-root <p>]` — 대상: 프로젝트 `.claude/settings.json`(기본) / `--user` = `~/.claude/settings.json`. **병합 계약**: 기존 JSON 파싱 → hooks의 해당 이벤트 배열에서 자기 항목(**명령 토큰 정확 일치**(`context-router hook` 완전 일치 — 접두사 매칭 금지: `context-router hook-wrapper` 등 유사 명령 오삭제 방지, 리뷰 반영) **+ 소유권 마커 결합**) 제거 후 append(버전 마커 필드) → **temp 파일+rename 원자 쓰기** → 미지 키·타 도구 훅 항목 왕복 보존. 등록: SessionStart / PreToolUse(matcher "Read") / PostToolUse(matcher 전체) / PostToolUseFailure(matcher 전체) + timeout 10(단위 초 — T0 확인). `--no-shadow` = 훅 명령에 `CTR_SHADOW_OFF` 반영(env 또는 args — T0 스키마 검증 결과에 따름). **`--store-root` 명시 여부 판별**(리뷰 반영): 현행 `prescanRootFlags`가 토큰을 소비해 cli.Run은 명시/기본을 구분 불가 — main dispatch가 `storeRootExplicit bool`(+원시값)을 cli.Run에 전달하도록 시그니처 확장, 명시된 경우에만 훅 args에 주입(기본 절대경로의 불필요한 영구 기입 방지). `uninstall`은 정확 일치+마커 항목만 대칭 제거.
  - doctor 확장: 항목 추가 — 훅 등록 상태(settings 파싱)·`context-router` PATH 해석·해석된 store-root 표기·drops 건수·사이드카 기록 가능 여부. 기존 항목 순서 nit([3]→[5]→[4]) 정렬(부채 편승, 설계 §9).
- [ ] **Step 1: 실패 테스트** — ① 빈 설정에 install → 4항목 등록·유효 JSON ② 재install 멱등(항목 1벌 유지) ③ **타 도구 훅 항목+미지 키 시드 후 install/uninstall → 원형 보존** ④ uninstall 대칭(자기 항목만 제거) + **유사 접두사 보존**(`context-router hook-wrapper` 명령·마커 없는 수동 hook 항목이 살아남음) ⑤ 원자성(임시 파일 잔존물 부재) ⑥ doctor: 훅 등록/미등록 각 상태 문구 + drops 건수 표기 + 항목 순서 [1]~[n] 오름차순 ⑦ `--no-shadow` 반영 / `--store-root` 명시 시에만 args 주입(미명시 = 무기입) ⑧ main dispatch(main_test.go): explicit store-root 전달 경로.
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/cli/ -run 'TestHookInstall|TestDoctor' -v`.
- [ ] **Step 3: 구현** — 병합은 `map[string]json.RawMessage` 보존 패턴(미지 키 무손실).
- [ ] **Step 4: GREEN** — cli 회귀.
- [ ] **Step 5: Commit** — `"feat(cli): hook install/uninstall(원자 병합·타 도구 보존) + doctor 훅 항목·순서 정렬 (v0.2 설계 §7·§9)"`

### Task 9: ctr usage — transcript 집계 (설계 §6)

**Files:** Modify `internal/cli/cli.go`(+test), `cmd/context-router/main.go`+`main_test.go`(usage 서브커맨드 인지 + dispatch 테스트)

**Interfaces:**
- Consumes: `session.OpenReadOnly`(cc: 세션 존재 조회), **기존 transcript 파서 재사용**(리뷰 반영 — 재구현 금지): `readTranscriptLine`(cli.go:178)·`message.usage` 구조체(cli.go:164)·`runStatsProvider`(cli.go:205)의 합산 로직. usage는 그 위에 파일별(=세션별) 그룹핑 + `cc:` 대조만 얹는다. transcript 위치 `~/.claude/projects/<경로 문자 치환 디렉터리>/*.jsonl`.
- Produces: `context-router usage [--transcripts <dir>]` — 읽기 전용. 파일별(= 호스트 세션별) assistant 메시지의 usage 블록(input/output/cache 토큰) 합산 표 출력. 파일명의 세션 UUID가 session.db `sessions`에 `cc:<uuid>`로 존재하면 "hooks:on" 표기(훅 스트림 존재 = 그 세션에 훅이 켜져 있었다는 정확 신호). `--transcripts` 미지정 시 cwd에서 규칙 유도(경로 구분자·콜론 → `-` 치환) — **Step 1에서 실디렉터리로 규칙 검증**.

- [ ] **Step 1: 실측 검증** — 이 머신의 `~/.claude/projects/` 실디렉터리명 규칙과 transcript JSONL 1개의 usage 필드 형태를 **읽기 전용으로** 확인해 파서 계약 고정(설계 §6 [plan 검증] — 기존 runStatsProvider 계약과 대조). 픽스처는 **민감값을 제거한 축약 합성 JSONL**로 testdata에 고정(실파일 복사 금지, 리뷰 반영).
- [ ] **Step 2: 실패 테스트** — ① 픽스처 JSONL 2세션 → 세션별 합산 정확 ② cc: 세션 시드 → "hooks:on" 표기 ③ 훅 무세션 → "hooks:off" ④ 손상 줄(비JSON) → 건너뜀·전체 비중단 ⑤ 디렉터리 부재 → 명확한 오류(절대경로 비노출).
- [ ] **Step 3: FAIL→구현→GREEN** — `go test -p 1 ./internal/cli/ -run 'TestUsage' -v` → 회귀.
- [ ] **Step 4: Commit** — `"feat(cli): usage — transcript 세션별 토큰 집계 + cc: 스트림 대조(hooks on/off) (v0.2 설계 §6)"`

### Task 10: 통합 게이트 — 2-프로세스 E2E·deadline·동시성 (설계 §10)

**Files:** Modify `cmd/context-router/main_test.go`(**package main — E2E는 반드시 여기**: `buildCtrBinary`(710)·`spawnCtr`(727)는 package main 비공개라 internal/hook에서 접근 불가하고, `spawnCtr`는 장기 MCP stdio 전용이라 재사용 대상은 `buildCtrBinary`뿐. 훅 one-shot 실행은 신규 `exec.Command` 헬퍼(stdin 주입·env·exit code 검사)로 작성 — 리뷰 반영)

**Interfaces:** Consumes 전 태스크. Produces 게이트 실체:
- E2E: `buildCtrBinary`로 빌드 → 신규 one-shot 헬퍼로 `context-router hook`을 픽스처 stdin과 실행(SessionStart→PostToolUse 3건) → session.db에 `cc:` 세션·이벤트·아티팩트 검증. MCP 서버 프로세스(`spawnCtr`) 동시 가동 상태에서 훅 append 무손실(content.db·session.db 동시 쓰기).
- deadline 결정론(주입 기법 교체 — 리뷰 반영: exclusive lease 선점은 논블로킹 AcquireLock이 즉시 `ErrLeaseHeld`로 떨어져 **deadline 경로에 진입조차 못한다**): ① 동일 session.db에 `BEGIN IMMEDIATE` write txn 점유(SQLite BUSY 경로) ② 신규 DB init-lock 점유 ③ content store open-lock 점유 — 각각 훅 실행 → `CTR_HOOK_DEADLINE_MS=300` 안에 종료(측정)·exit 0·drops 1줄.
- `cc:` 세션 summary/export 왕복: `ctr_session_summary`/`ctr_export_events`가 훅 이벤트를 반환(producer 정확·untrusted 표기), 미지 세션 drop·session_start 1회 규칙 재확인.

- [ ] **Step 1: 실패 테스트** — 위 3계열 작성(프로세스 기동은 v0.1 T10 하네스 재사용).
- [ ] **Step 2: FAIL→구현(부족분 배선)→GREEN** — `go test -p 1 ./... ` 전체.
- [ ] **Step 3: 계측 흡수 부채 4건(설계 §9)** — 훅 스트림이 만든 실볼륨·타입 다양성 위에서: ① `session.Summarize` 타입 fan-out 캡(그룹 수 상한 + truncated 표기 — Codex P2-4 이월) ② summary budget 정밀화(len(summary)-only 관례 검증 테스트) ③ EventV1 omitempty 소비자 재점검(훅 이벤트 export에서 빈 필드 직렬화 형태 golden) ④ byte-exact export golden(훅 이벤트 포함 시드 → 바이트 고정 비교). 각각 실패 테스트→구현→GREEN, 별도 커밋 `"chore(session): 계측 흡수 부채 4건 — Summarize fan-out 캡·budget 검증·omitempty golden·byte-exact (설계 §9)"`.
- [ ] **Step 4: 통합 체크포인트 리뷰** — 서브에이전트 + Codex `review --base <원장 BASE>` 병렬 1패스, 발견 병합 후 수정.

### Task 11: 실호스트 스모크 + 수동 A/B 1회 (설계 §10·§6 — 비차단)

**Files:** 코드 변경 없음(발견 시 최소 수정 별도 커밋). 결과는 session-09 기록(docs/prompts)에.

- [ ] **Step 1:** 이 저장소에서 `context-router hook install` 후 실세션 1회 — `session_start`/`tool_call`/`file_edit`/`test_run` 관측, doctor GREEN(훅 항목 포함). **T0 이관 가정 실측**: 대형 출력 도구 1회로 PostToolUse stdin `tool_response`의 최대 크기·절단 정책 확인(절단 시 설계 §5 전제 수정 보고).
- [ ] **Step 2:** `--enable ingest`로 fetch 정상 경로 스모크 1회(ctr_index → search → fetch byte-exact, session-08 §4.2 이월 — ingest는 프로필이 아니라 `--enable` 옵트인, 리뷰 정정).
- [ ] **Step 3:** 수동 A/B 1회: `CTR_HOOKS_OFF` on/off 각 1세션 → `context-router usage` 비교, guard 발화·재독 보조 지표 기록(측정 대상 분리 규칙, 설계 §6).
- [ ] **Step 4:** 결과를 세션 기록에 남기고 push. 최종 체크포인트 리뷰(서브에이전트 + Codex 1패스)는 브랜치 머지 전.
