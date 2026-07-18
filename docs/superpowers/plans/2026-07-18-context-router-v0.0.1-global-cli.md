# context-router 계획 3 — v0.0.1 재정의 계약 완성 (global + CLI 4종 + 게이트 → 태그)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** v0.0.1 재정의 계약의 잔여분(ctr_global_search + CLI 4종 + WAL 최초경합 근본수정 + 라이선스 고지 + 마이너 이월 + §12 수용 게이트)을 완성해 v0.0.1 태그 준비를 마친다.

**Architecture:** 기존 8패키지 구조에 `internal/cli`(doctor/stats/purge/upgrade)를 추가하고, mcp에 global-search 프로필 분기(별도 등록 `ctr-global`, read-only+query_only 다중 프로젝트 연결)를 넣는다. store는 원시 SQL/blob 연산(ledger 집계·purge·orphan GC)을 소유하고 cli는 포맷·확인 UX만 담당한다. 게이트는 oracle golden(1)·retrieval 하네스(2)·CI 3-OS(3/7/12)·토큰 예산(11)로 마감한다.

**Tech Stack:** Go 1.26, modelcontextprotocol/go-sdk v1.6.1, modernc.org/sqlite v1.54.0 (드라이버명 `"sqlite"`), 표준 flag. **신규 의존성 금지**(D8).

**Base branch:** `feat/v0.0.1-global-cli` (main d73294a에서 분기). 설계서 = `docs/context-router-design-v0.0.1-ko.md`(§ 참조 기준), 규약 = `docs/context-router-code-architecture-ko.md`.

## Global Constraints (모든 태스크에 암묵 포함)

- **D8**: 의존성 7개 고정 — `go.mod` require(direct)에 새 모듈 추가 금지. 프레임워크 금지.
- **D13 반파편화**: 패키지당 소스 1~2개, 테스트 `<pkg>_test.go` 1개. helpers.go/utils.go/타입별 1파일 금지. 선호 밴드 300~1,000줄.
- **자체 정의 인터페이스 0개** (규약 §4). 테스트 심 = 순수 함수 분리 / stdlib func·파라미터 주입 / 실물(임시 디렉터리 실제 SQLite, 자기 바이너리).
- **오류 규약** (규약 §6): 패키지별 sentinel, mcp `toToolError` 단일 변환, `fmt.Errorf("동작: %w", err)`, 메시지에 사용자 입력·절대경로·env·비밀값 미포함. cli는 자체 오류→종료 메시지, `os.Exit`은 main에서만.
- **rot-path** (규약 §10): mcp에 `database/sql`·`net/http`·`os/exec` import 금지. store에 ranking/redaction/경로정책 유입 금지. 설계서 §번호 없는 신규 플래그·동작 금지.
- **테스트**: `go test -p 1 ./...` (메모리 캡 규율). 실물 우선, mock 금지. 새 비밀 캐너리는 런타임 분할 리터럴(규약 §8).
- **커밋**: 태스크당 1커밋 이상, 메시지 뒤에 `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` 트레일러.
- **파일 출력**: UTF-8(BOM 없음), LF.
- **응답 분할 규율**(서브에이전트): 전체 파일 재작성 금지 — 단계별 소규모 Edit. 긴 테스트 데이터는 리터럴 금지(`strings.Repeat`/생성 루프/testdata 파일).
- 포맷: gofumpt. 주석·식별자 스타일은 기존 파일과 동일(한국어 주석 + 설계서 § 참조).

## 파일 지도 (생성/수정 총괄)

| 파일 | 태스크 | 책임 |
|---|---|---|
| `internal/store/store.go` (수정) | T1, T4, T5 | pragmas 재배열 / LedgerStats / Purge·GC 원시 연산 |
| `cmd/context-router/main.go` (수정) | T2, T3 | `--projects` 플래그·global 분기 / CLI 서브커맨드 dispatch |
| `internal/mcp/mcp.go` (수정) | T2, T6 | global 서버(NewGlobalServer)+ctr_global_search / TransformTimeout·title 배선 |
| `internal/cli/cli.go` (생성) | T3, T4, T5 | doctor·upgrade·stats·purge (단일 파일, D13) |
| `internal/cli/cli_test.go` (생성) | T3, T4, T5 | table-driven (purge 확인 규칙 등, 규약 §8) |
| `internal/netfetch/netfetch.go` (수정) | T6 | Result.Title / DNS 다중 주소 재시도 / 주석 드리프트 |
| `internal/ingest/ingest.go` (수정) | T6 | RunWeb title 파라미터 / RunWeb 주석(A5) |
| `internal/transform/transform.go` (수정) | T6 | Spawn의 ErrNoIsolation 원인 보존(%w) |
| `internal/ident/ident.go` (수정) | T6 | `.git` 읽기 실패 err prefix 통일 |
| `internal/search/search.go` (수정) | T6 | foldIndex 다중룬 한계 주석 1줄 |
| `LICENSE`, `NOTICE`, `README.md` (생성) | T7 | ELv2 / 서드파티 고지 / 이름 매핑표 |
| `docs/oracle-mapping-ko.md`, `testdata/oracle/*` (생성) | T8 | 게이트 1 동등성 |
| `internal/search/retrieval_test.go` 또는 search_test.go 내 (수정) | T8 | 게이트 2 recall@k |
| `.github/workflows/ci.yml` (생성) | T9 | 게이트 3/7/8/12 — 3-OS·크로스빌드·RLIMIT 실측 |
| `internal/mcp/mcp_test.go` (수정) | T2, T6, T10 | global 도구 / 좌표 테스트 / 토큰 예산 게이트 |
| `docs/gates-v0.0.1-ko.md` (생성) | T10 | 게이트 13항 증거표 → 태그 절차 |

태스크 순서는 의존 순서다: T1(store 기반) → T2(global) → T3~T5(CLI) → T6(마이너) → T7(고지) → T8(oracle/retrieval) → T9(CI) → T10(예산·체크리스트·마감).

---

### Task 1: WAL 최초 기동 경합 근본 수정 (store DSN 프라그마 재배열 + 2-프로세스 최초 마이그레이션 테스트)

**배경(재판정 2026-07-18 — 구현자 반증 + Codex 자문 병합):** 당초 진단(DSN `_pragma` 순서)은 **no-op으로 실증 반증됨** — modernc.org/sqlite v1.54.0 `applyQueryParams`가 DSN 순서와 무관하게 busy_timeout을 최우선 정렬·실행한다(업스트림 gitlab issue 198). 실제 원인: 신규 DB 최초 WAL 전환 시 wal-index recovery 락 경로(`walTryBeginRead`/`WAL_RECOVER_LOCK`)는 **busy handler를 호출하지 않고** SQLITE_BUSY(_RECOVERY)를 즉시 반환한다 — SQLite 정본 동작이며 modernc 번역 구현도 동일. 드라이버 버전 업은 불가(2026-07 최신 = v1.54.0, 해당 수정 없음). **근본 수정 = writable `Open()` 전체를 OS advisory lock으로 프로세스 간 직렬화** (설계 §3.5의 배타 잠금 파일 경로 재사용).

**Files:**
- Modify: `internal/store/store.go` (Open의 !readOnly 분기에 잠금 획득/해제)
- Create: `internal/store/store_lock_windows.go`, `internal/store/store_lock_unix.go` (D13 §5-① OS build-tag 예외 — worker_windows/unix 선례)
- Test: `internal/store/store_test.go` — **재현 테스트는 이미 작업 트리에 미커밋 상태로 존재**(`TestMain`+`CTR_TEST_CHILD` 자식 모드, `TestOpen_ConcurrentFirstMigration` 8회 반복·서브프로세스 2개; RED 실증 완료). 재작성 금지, 그대로 사용.

**Interfaces:**
- Produces: `lockStore(dir string) (release func(), err error)` — 비공개, build-tag 파일 쌍 양쪽 동일 시그니처.

**잠금 계약 (Codex 자문 병합판):**
- 파일: `filepath.Join(dir, "content.db.rebuild.lock")` — 설계 §3.5의 잠금 경로를 store 생명주기 잠금으로 재사용(별도 init.lock 신설 금지). 모드 0600. **잠금 해제 후 파일은 삭제하지 않는다.**
- unix(`//go:build !windows`): `syscall.Flock(fd, LOCK_EX|LOCK_NB)`. windows: `golang.org/x/sys/windows`의 `LockFileEx(EXCLUSIVE|FAIL_IMMEDIATELY)`로 `[0,1)` 1바이트 잠금(x/sys는 기존 직접 의존성 — 신규 의존 아님).
- 논블로킹 시도 + 지수 백오프 10→20→40→80→160ms(이후 160ms 유지), **총 deadline 5초** 초과 시 `fmt.Errorf("store open: 잠금 대기 초과: %w", ErrUnavailable)` (→ mcp에서 STORAGE_UNAVAILABLE).
- 배치: Open의 `!readOnly` 분기, `os.MkdirAll` 성공 **직후·첫 `sql.Open` 이전** 획득 — migrate()·ledger.db 최초 DDL까지 포함해 **Open 반환 직전 defer 해제**. 이유: modernc는 lazy 연결이라 PRAGMA/WAL 전환이 첫 QueryRow(migrate)에서 발생 — migrate만 감싸면 늦다. `readOnly` 경로는 잠금 파일을 만들지도 잡지도 않는다(doctor no-create 계약).
- 실패 모드(문서화됨): 보유 프로세스 크래시 → 커널 자동 해제(stale lock 없음) / 마이그레이션 중 크래시 → 다음 프로세스가 멱등 스키마 재실행 / 보유 프로세스 hang → 5s 후 STORAGE_UNAVAILABLE(무한 대기 방지).

- [ ] **Step 1: store.go:35 원상 복구** — 미커밋 재배열을 되돌린다(원래 상수: `"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"`, 추가 주석 없음). 근본 원인 설명 주석은 잠금 코드 쪽에 단다(driver가 busy_timeout을 자체 정렬한다는 사실 + recovery 락은 busy handler 미경유 + 설계 §3.5 참조).
- [ ] **Step 2: RED 확인** — Run: `go test -p 1 -run TestOpen_ConcurrentFirstMigration -count 3 ./internal/store/` → 여전히 간헐 FAIL(SQLITE_BUSY) 확인.
- [ ] **Step 3: lockStore 구현** — 위 잠금 계약대로 build-tag 파일 쌍 작성 + Open 배선.
- [ ] **Step 4: GREEN 확인** — Run: `go test -p 1 -run TestOpen_ConcurrentFirstMigration -count 5 ./internal/store/` → PASS(40/40 반복), 이어서 `go test -p 1 ./...` 전체 GREEN + `CGO_ENABLED=0 GOOS=linux go build ./...`(unix 잠금 파일 컴파일 확인) + `GOOS=darwin go build ./...`.
- [ ] **Step 5: Commit** — `fix(store): writable Open을 OS advisory lock으로 직렬화 — 최초 WAL 전환 recovery-lock 경합 근본 수정 (게이트 7 심층, Codex 자문 병합)`

---

### Task 2: `ctr_global_search` — global-search 프로필 (설계 §4.6, §5.4)

**Files:**
- Modify: `cmd/context-router/main.go` (`--projects` 플래그, run()의 global 분기)
- Modify: `internal/mcp/mcp.go` (GlobalConfig / NewGlobalServer / registerGlobalSearch)
- Test: `internal/mcp/mcp_test.go`, `cmd/context-router/main_test.go`

**Interfaces:**
- Consumes: `store.Open(dir, true)` (read-only는 이미 `mode=ro&_pragma=query_only(ON)` 적용 — store.go:48), `search.Query(ctx, st, projectRoot, queries, limit, budgetBytes)`, `ident.Canonicalize`.
- Produces: `mcp.GlobalProject{ID string; Root string; Store *store.Store}`, `mcp.GlobalConfig{Projects []GlobalProject}`, `mcp.ServeGlobal(ctx, GlobalConfig) error`, `mcp.NewGlobalServer(GlobalConfig) (*sdk.Server, error)`. main의 `--projects` 파싱·검증. (T3 doctor 스니펫이 ctr-global 등록 예시를 출력할 때 이 플래그 계약을 참조한다.)

**계약(설계 §4.6/§5.4 원문 준수):**
- 등록: global 프로필 서버에는 `ctr_global_search` **하나만** 등록(다른 도구 절대 금지 — search/fetch/transform 미등록).
- `--projects <경로|ID 목록>` 콤마 구분, **기본값 없음** — global-search 프로필인데 미지정이면 시작 거부(오류 종료). 반대로 기본 프로필에서 `--projects` 지정은 무시하지 말고 오류로 거부한다(모호성 차단).
- 엔트리 판별: 경로 구분자(`/`,`\`) 포함 또는 존재하는 디렉터리 → 경로로 취급해 `ident.Canonicalize`로 ProjectID·WorktreeRoot 도출. 그 외 → ProjectID 문자열로 취급(Root=""). store 디렉터리는 `<storeRoot>/projects/<ProjectID>`.
- 열기 실패(디렉터리/DB 없음 포함)는 **시작 거부**(allowlist는 명시 계약 — fail-closed). read-only Open은 mkdir 부작용이 없다(store.go:38-44).
- 입력은 ctr_search와 동일(queries/limit/budget), 반환 hit에 `project` 라벨(ProjectID) 추가.
- 병합: 프로젝트별로 `search.Query`를 동일 limit·budget으로 호출 → 쿼리별로 hit들을 project 라벨 부착 후 RRF score 내림차순 병합 → limit 절단. `truncated` = 어느 한 프로젝트라도 truncated였거나 병합 절단 발생. (RRF score는 rank 기반이라 프로젝트 간 비교 가능 — 이 근거를 도구 설명 또는 주석에 1줄 기록.)
- Root=""(ID 엔트리)인 프로젝트의 hit은 staleness 원본 대조·상대화가 제한됨 — `search.Query`에 projectRoot=""를 넘기고 URI가 절대경로로 반환됨을 도구 설명에 명시.

- [ ] **Step 1: mcp 실패 테스트** — `TestGlobalSearch_MergesAcrossProjects`: 임시 store 2개를 만들어 각각 서로 다른 내용 Register → `NewGlobalServer(GlobalConfig{...})` → in-memory transport로 `ctr_global_search` 호출(기존 mcp_test.go의 서버 호출 패턴 재사용) → 두 프로젝트의 hit이 project 라벨과 함께 병합돼 나오는지, score 내림차순인지 검증. 추가 케이스: 도구 목록에 ctr_global_search 외 도구가 **없는지**(tools/list 검증 — §4.6 금지 조항).
- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 -run TestGlobalSearch ./internal/mcp/` → FAIL (NewGlobalServer 미정의 컴파일 오류).
- [ ] **Step 3: mcp 구현** — mcp.go에 GlobalProject/GlobalConfig 타입, NewGlobalServer(빈 Projects면 오류), registerGlobalSearch(입출력 struct는 기존 searchHit에 `Project string \"json:\"project\"\"` 필드를 더한 globalHit — wire 타입은 mcp 소유, 규약 §3), ServeGlobal(NewGlobalServer+StdioTransport Run). 핸들러는 decode→각 프로젝트 search.Query→병합·encode ≤50줄(규약 §10-1), 초과 시 병합 로직을 파일 내 비공개 함수 `mergeGlobalHits`로 분리.
- [ ] **Step 4: mcp 테스트 통과 확인** — Run: `go test -p 1 -run TestGlobalSearch ./internal/mcp/` → PASS.
- [ ] **Step 5: main 배선 실패 테스트** — main_test.go: `TestParseFlags_Projects`(콤마 파싱), `TestRun_GlobalProfile_RequiresProjects`(global-search 프로필 + projects 미지정 → 오류), `TestRun_DefaultProfile_RejectsProjects`(기본 프로필 + --projects → 오류). 기존 parseFlags/run 테스트 패턴 재사용.
- [ ] **Step 6: main 구현** — serverFlags에 `Projects []string` 추가, parseFlags에 `fs.StringVar(&projects, "projects", "", ...)` + 콤마 분해. run()에서 `slices.Contains(f.Profile, "global-search")`면: projects 필수 검증 → 엔트리별 판별·canonicalize → `store.Open(dir, true)` (전부 defer Close) → `mcp.ServeGlobal`. 배너는 기존 banner() 그대로(profile=global-search 표시됨). global 분기에서는 프로젝트 store.Open(cwd), transform probe, ingest/net 게이팅을 수행하지 않는다.
- [ ] **Step 7: 전체 확인** — Run: `go test -p 1 ./...` → GREEN. `go build ./...` + `CGO_ENABLED=0 GOOS=linux go build ./...` 확인.
- [ ] **Step 8: Commit** — `feat(mcp,cmd): ctr_global_search — global-search 프로필·--projects allowlist·read-only 다중 프로젝트 병합 (설계 §4.6/§5.4)`

---

### Task 3: `internal/cli` 골격 + `doctor` + `upgrade` + main dispatch (설계 §7, §9)

**Files:**
- Create: `internal/cli/cli.go`, `internal/cli/cli_test.go`
- Modify: `cmd/context-router/main.go` (서브커맨드 dispatch)

**Interfaces:**
- Consumes: `store.Open(dir, readOnly)`, `ident.Canonicalize`, main의 `storeRootFor`/`defaultStoreRoot` 로직(→ **cli로 이동 또는 복제 금지**: main의 storeRoot 결정 함수를 cli가 쓸 수 있도록 main은 결정된 storeRoot를 인자로 넘긴다).
- Produces: `cli.Run(ctx context.Context, sub string, args []string, storeRoot, projectRoot, version string, stdout, stderr io.Writer) error` — 단일 진입. sub ∈ {"doctor","stats","purge","upgrade"}. T4/T5는 이 dispatch에 케이스만 추가한다. 내부: `runDoctor`, `runUpgrade(w io.Writer, client *http.Client, releaseURL, current string) error`(테스트 주입 심 — 파라미터 주입, 인터페이스 금지).

**계약(설계 §7):**
- `doctor`: 진단 항목 — ① 저장 루트 존재·쓰기 가능 ② 프로젝트 식별(canonicalize cwd → ProjectID/WorktreeRoot 출력) ③ content.db 존재 시 read-only open + `PRAGMA quick_check` + user_version 표시(미존재는 "not initialized" — 오류 아님, doctor가 store를 **생성하면 안 됨**: `store.Open(dir, true)` 사용) ④ FTS5 가용성(reader로 `CREATE VIRTUAL TABLE temp.probe USING fts5(x)` 시도 후 DROP; DB 미존재 시 `:memory:` 연결로 확인) ⑤ ledger.db 존재 여부. 마지막에 호스트 등록 스니펫 출력: Claude Code `.mcp.json`(ctr 기본 + ctr-global 예시, permissions에 `ctr_index`/`ctr_fetch_and_index`=ask, ctr-global 도구=ask **기본 포함** — §9) + Codex `config.toml`(기본 3-도구 프로필 권장, ingest/net 시 approval prompt 권장). 실패 항목 있으면 error 반환(main이 exit 1).
- `upgrade`(최소형): 컴파일타임 상수 `const releaseURL = "https://api.github.com/repos/wotjr1649/context-router/releases/latest"`. `client.Timeout=10s`로 GET → JSON `tag_name`만 취한다. 출력은 정확히 두 줄 — `current: v<현재>` / `latest: <tag_name>` + 수동 설치 안내 1줄(안내 문구는 **하드코딩**: "install: download from the project releases page and replace the binary"). **응답이 제공한 URL·명령·기타 필드는 절대 출력하지 않는다**(응답에서 취하는 것은 버전 문자열뿐 — §7). tag_name은 출력 전 위생 검증: `^v?[0-9A-Za-z.+-]{1,64}$` 불일치 시 네트워크 실패와 동일 취급. 네트워크 실패·타임아웃·비200·파싱 실패 → `current: v<현재>`만 출력하고 **nil 반환(정상 종료)**.
- main dispatch: `main()`에서 worker 분기 다음에 `if len(os.Args) > 1 && os.Args[1]이 4 서브커맨드 중 하나` → storeRoot 결정(기존 storeRootFor+canonicalizeStoreRoot 재사용, 플래그는 서브커맨드 args에서 파싱) → `cli.Run(...)` → 오류 시 exit 1. `os.Exit`은 main에만(규약 §6).

- [ ] **Step 1: 실패 테스트 작성** — cli_test.go: `TestRunUpgrade_Table` — httptest 서버로 케이스: (정상 tag_name → 두 줄 출력) / (500 응답 → current만, err=nil) / (타임아웃: 핸들러 sleep, client.Timeout 50ms → current만, err=nil) / (악성 tag_name `"v1.0\nmalicious: curl ..."` → current만 — 위생 검증) / (응답 body의 URL이 출력에 미포함 검증). `TestRunDoctor_Smoke` — 임시 storeRoot+임시 git 프로젝트 디렉터리로 doctor 실행 → 출력에 ProjectID·"not initialized"·스니펫 마커(`.mcp.json`) 포함, err=nil; store 디렉터리가 **생성되지 않았음** 확인. `TestRun_UnknownSub` — 미지 서브커맨드 → error.
- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 ./internal/cli/` → FAIL(컴파일: 패키지 미존재).
- [ ] **Step 3: cli.go 구현** — 패키지 주석(1줄 책임 + 설계 §7)과 함께 Run dispatch + runDoctor + runUpgrade. upgrade JSON 파싱은 `encoding/json` + `struct{ TagName string \"json:\"tag_name\"\" }`.
- [ ] **Step 4: 통과 확인** — Run: `go test -p 1 ./internal/cli/` → PASS.
- [ ] **Step 5: main dispatch 배선 + 테스트** — main_test.go에 `TestMainDispatch_CLI`(run 아닌 dispatch 함수를 분리해 테스트: `dispatchCLI(ctx, os.Args) (handled bool, err error)` 형태 권장) 추가, doctor가 임시 디렉터리에서 정상 종료하는지 확인. Run: `go test -p 1 ./cmd/... ./internal/cli/` → PASS.
- [ ] **Step 6: Commit** — `feat(cli): internal/cli 골격 + doctor·upgrade + main dispatch (설계 §7/§9)`

---

### Task 4: `stats` — 이중 원장 (local ledger + provider transcript) (설계 §6)

**Files:**
- Modify: `internal/store/store.go` (ledger 집계 — 원시 SQL은 store 소유, 규약 §10-2)
- Modify: `internal/cli/cli.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: T3의 `cli.Run` dispatch.
- Produces: `store.ToolStat{Tool string; Calls, BytesStored, BytesReturned int64; FirstTS, LastTS int64}`, `store.LedgerStats(dir string) ([]ToolStat, error)` — ledger.db를 read-only로 열어 도구별 집계 후 닫는다(ledger.db 미존재 시 빈 슬라이스, 오류 아님).

**계약(설계 §6):**
- local: `context-router stats` → 현재 프로젝트(`<storeRoot>/projects/<ProjectID>`)의 ledger.db 집계를 표로 출력: tool / calls / bytes_stored / bytes_returned / span(first~last, RFC3339). 합계 줄 끝에 고정 문구 `bytes suppressed (local, 진단용)`. **토큰·달러 환산 출력 금지. 절약률 주장 금지**(§6 차단 항목 — v0.2 A/B 전까지).
- provider: `stats --provider <transcript-path>` → Claude Code transcript JSONL을 한 줄씩 스캔, `message.usage`의 `input_tokens`/`output_tokens`/`cache_read_input_tokens`/`cache_creation_input_tokens` 합계 + usage 보유 레코드 수 출력. **실측 토큰만 표시, 절약 주장·비교 문구 없음.** 파싱 불가 줄은 건너뛴다(로그 없이 카운트만 `skipped: N`). 파일 크기와 무관하게 `bufio.Scanner` + `Buffer(…, 10<<20)` (transcript 한 줄이 큰 경우 대비).
- transcript 파싱은 cli 소유(호스트 산출물이지 store 데이터가 아님).

- [ ] **Step 1: store 집계 실패 테스트** — store_test.go: `TestLedgerStats` — 임시 store에 `LedgerAppend` 3건(도구 2종) → `store.LedgerStats(dir)` → 도구별 calls/합계 검증. ledger 미존재 디렉터리 → 빈 슬라이스, err=nil.
- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 -run TestLedgerStats ./internal/store/` → FAIL(미정의).
- [ ] **Step 3: store.LedgerStats 구현** — `sql.Open("sqlite", "file:"+ledgerPath+"?mode=ro&_pragma=busy_timeout(5000)")`, `SELECT tool, COUNT(*), SUM(bytes_stored), SUM(bytes_returned), MIN(ts), MAX(ts) FROM ledger GROUP BY tool ORDER BY tool` → PASS 확인.
- [ ] **Step 4: cli stats 실패 테스트** — cli_test.go: `TestRunStats_Local`(임시 store에 LedgerAppend 후 출력에 도구명·`bytes suppressed` 문구 포함, `token`/`$` 문자열 **미포함** 검증), `TestRunStats_Provider`(testdata 아닌 임시 파일에 JSONL 3줄 — usage 2건+파싱불가 1건 — 를 `strings.Repeat` 없이 짧게 생성 → 합계·skipped 검증).
- [ ] **Step 5: cli 구현·통과** — Run dispatch에 stats 케이스(플래그셋: `--provider`). Run: `go test -p 1 ./internal/cli/ ./internal/store/` → PASS.
- [ ] **Step 6: Commit** — `feat(cli,store): stats — local ledger 집계 + provider transcript 실측 (설계 §6, 환산·절약주장 금지)`

---

### Task 5: `purge` — 2단 확인 + `--older-than` + `--gc` orphan blob GC (설계 §7, 게이트 6 일부)

**Files:**
- Modify: `internal/store/store.go` (purge/GC 원시 연산)
- Modify: `internal/cli/cli.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: T3 dispatch, `store.Open`.
- Produces: `(s *Store) PurgeOlderThan(ctx context.Context, cutoffUnix int64) (sources, artifacts int64, err error)`, `(s *Store) GCOrphanBlobs(ctx context.Context) (removed int64, err error)`. cli: `confirmPurge(in io.Reader, out io.Writer, isTTY bool, force bool, expected string) error` (순수 함수 — 확인 규칙 table-driven 테스트 대상, 규약 §8).

**계약(설계 §7):**
- 사용법: `purge (--project <id|path> | --all) [--older-than <dur>] [--gc] [--force]`. `--project`와 `--all`은 정확히 하나(둘 다/둘 다 아님 → 사용법 오류).
- **확인 규칙**: TTY 필수(`os.Stdin`의 `os.ModeCharDevice` 검사 → isTTY로 전달). 프롬프트가 대상 슬러그를 보여주고 사용자가 **그 슬러그를 그대로 입력**해야 진행(`--project` → ProjectID, `--all` → `all-<N>-projects`, N=프로젝트 수). 정적 `yes` 금지. 비TTY는 `--force` 명시 시에만(그 외 오류). 불일치 입력 → 오류 종료, 아무것도 삭제 안 함.
- 삭제 의미론: `--older-than <dur>`(Go `time.ParseDuration` + `d`/일 단위 확장은 하지 않음 — 표준 단위만) 지정 시 **선택 삭제**: `sources.indexed_at < now-dur`인 sources 행 삭제 → 어떤 source도 참조하지 않는 artifacts 행 삭제(chunks는 FK CASCADE, FTS는 트리거가 동기) — 전부 단일 트랜잭션(`txRetry`). 미지정 시 **전체 삭제**: 프로젝트 디렉터리(content.db, artifacts/, ledger.db) `os.RemoveAll`. `--all`은 `<storeRoot>/projects/` 하위 전체에 반복.
- `--gc`: artifacts/ 디렉터리의 blob 파일 중 `artifacts.content_hash`에도 `sources.raw_blob_hash`에도 없는 해시를 삭제(계획2 이월 "과거 raw blob 물리 GC" 해소). 단독 사용 가능(`purge --project X --gc`만 주면 GC만 수행 — 삭제 확인 규칙은 데이터 삭제가 아니므로 **GC 단독은 확인 생략**; older-than/전체 삭제와 함께면 확인 후 삭제→GC 순서).
- 삭제 후 무결성: 선택 삭제 경로는 같은 트랜잭션 커밋 후 `INSERT INTO fts_porter(fts_porter) VALUES('integrity-check')`+trigram 동일 검사를 수행하고 실패 시 오류 보고(게이트 6 연결).

- [ ] **Step 1: store 실패 테스트** — store_test.go: `TestPurgeOlderThan`(구/신 2 source 등록 — `indexed_at` 조작은 Register 후 writer로 UPDATE 하는 테스트 헬퍼 대신 **cutoff를 경계로 주는 방식**: 등록 시각 사이에 time.Now를 캡처해 cutoff로 사용 → 구 source만 삭제, chunks/FTS 동기 확인, integrity-check 통과), `TestGCOrphanBlobs`(등록 후 재색인으로 raw_blob_hash 교체된 상황을 모사: artifacts/에 참조 없는 더미 blob 파일을 직접 생성 → GC가 그것만 삭제).
- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 -run 'TestPurgeOlderThan|TestGCOrphanBlobs' ./internal/store/` → FAIL(미정의).
- [ ] **Step 3: store 구현** — PurgeOlderThan: txRetry 안에서 `DELETE FROM chunks WHERE artifact_id IN (SELECT ...)` 순서 주의 — ① 대상 sources 삭제 ② 미참조 artifacts의 chunks 삭제(트리거가 FTS 동기) ③ 미참조 artifacts 삭제. GCOrphanBlobs: reader로 두 해시 집합 조회 → `os.ReadDir(artifacts/)` 대조 → 미참조 파일 삭제. → PASS 확인.
- [ ] **Step 4: cli 확인 규칙 table-driven 테스트** — `TestConfirmPurge`: (TTY+정확 슬러그 → nil) / (TTY+오입력 → err) / (TTY+"yes" → err) / (비TTY+force 없음 → err) / (비TTY+force → nil, 입력 안 읽음). `TestRunPurge_E2E`: 임시 store 등록 → `--project <id> --force --older-than 1ns`(비TTY 경로) → 데이터 삭제 확인; `--gc` 단독 → 확인 프롬프트 없이 orphan만 삭제.
- [ ] **Step 5: cli 구현·통과** — Run: `go test -p 1 ./internal/cli/ ./internal/store/` → PASS.
- [ ] **Step 6: Commit** — `feat(cli,store): purge — TTY 슬러그 2단 확인·--older-than 선택 삭제·--gc orphan blob GC (설계 §7)`

---

### Task 6: 마이너 이월 번들 (세션01 기록 §4 — 8건)

**Files:**
- Modify: `internal/mcp/mcp.go`, `internal/netfetch/netfetch.go`, `internal/ingest/ingest.go`, `internal/transform/transform.go`, `internal/ident/ident.go`, `internal/search/search.go` (+ 각 `_test.go`)

**Interfaces:**
- Consumes: 기존 API 전부.
- Produces: `netfetch.Result.Title string` 필드 추가, `ingest.RunWeb(ctx, st, url string, rawHTML, body []byte, mediaType, extraction, title string)` — **시그니처 변경**(마지막 title 파라미터 추가), `mcp.Config.TransformTimeout time.Duration`(0이면 NewServer가 10s로 채움). 호출부: mcp.go:559(RunWeb), ingest_test.go의 RunWeb 호출들 전부 갱신.

각 항목은 독립 커밋 불요 — 항목별 검증 후 **하나의 커밋**으로 마감(전부 소규모). 항목 순서대로:

- [ ] **(1) transform timeout → Config**: mcp.Config에 `TransformTimeout time.Duration` 추가, NewServer에서 `if cfg.TransformTimeout == 0 { cfg.TransformTimeout = 10 * time.Second }`, registerTransform 시그니처에 timeout 전달 → 핸들러가 `ctx, cancel := context.WithTimeout(ctx, timeout)`으로 Spawn 호출(transform.go:214의 안전망 defaultWorkerTimeout은 그대로 둔다 — deadline 없는 직접 호출 보호). main.go는 필드 미설정(기본값). 검증: mcp_test.go에 `TransformTimeout: 50*time.Millisecond` + sleep성 스크립트(스텝 소모 큰 루프)로 timeout 오류 확인 — 기존 transform 테스트의 스크립트 패턴 재사용.
- [ ] **(2) title = readability Article.Title**: netfetch.Result에 `Title string` 추가 — `convertToMarkdown`이 readability Article에서 Title을 얻어 반환하도록 확장(반환값에 title 추가 또는 Result 조립 지점에서 설정; `Fetch`에서 text/html 경로일 때만 채움). ingest.RunWeb에 title 파라미터 추가 — 청크 title 기본값으로 URL 대신 title 사용(빈 문자열이면 기존 URL fallback 유지). mcp.go:559 `ingest.RunWeb(ctx, st, res.FinalURL, res.RawHTML, res.Body, res.MediaType, res.Extraction, res.Title)`. 검증: netfetch golden 픽스처 중 title 있는 HTML로 Result.Title 확인 + ingest_test.go RunWeb 케이스 1건에 title 전달·청크 title 반영 확인.
- [ ] **(3) DNS 다중 주소 재시도**: `resolveAndValidate`(netfetch.go:155)를 `([]netip.Addr, error)` 반환으로 변경 — 전 주소 검증은 지금처럼 전부 수행(하나라도 거부면 전체 거부 — 기존 의미 유지), 반환만 전체 목록(각각 `.Unmap()`). `Fetch`의 hop 루프에서 주소 순서대로 dial 시도, **연결 계층 오류**(dial/handshake — `errors.As(&net.OpError{})` 또는 client.Do가 반환한 `*url.Error`의 원인 검사)일 때만 다음 주소로 재시도, HTTP 응답을 받은 뒤의 오류는 재시도하지 않는다. 리터럴 IP는 기존대로 단일 원소. 검증: netfetch_test.go — 두 주소 중 첫째가 닫힌 포트, 둘째가 httptest 서버인 상황은 로컬에서 구성 어렵다 → **순수 함수 검증으로 대체**: resolveAndValidate가 다중 주소를 모두 반환하는 것 + 재시도 판별 함수(`retryableDialErr(err) bool`로 분리)의 table-driven 테스트.
- [ ] **(4) 주석 드리프트 A3·A5**: (A3) netfetch.go에서 keep-alive/Transport 관련 defer 스코프 주석을 현행 코드와 일치하게 수정 — `grep -n "defer" internal/netfetch/netfetch.go`로 Fetch 내 위치 확인 후 불일치 문구만 교정. (A5) ingest.go:758 RunWeb 주석에 "non-html의 src_hash는 **디코딩 후(post-decode) 바이트** 기준" 1줄 반영. 코드 변경 없음 — 주석만.
- [ ] **(5) ident `.git` 읽기 실패 err prefix**: ident.go:71의 `fmt.Errorf("canonicalize: %w", err)`를 파일 내 다른 오류들의 prefix 관례와 통일(`grep -n 'fmt.Errorf' internal/ident/ident.go`로 지배적 prefix 확인 후 동일화).
- [ ] **(6) foldIndex 다중룬 한계 주석**: search.go:278 foldIndex 위에 1줄 — `// 한계: ß→ss 같은 1:N 폴딩은 인덱스 사상이 어긋나 미지원(단순 케이스폴드만) — 설계 유보.`
- [ ] **(7) extraction!="" 좌표 테스트**: mcp_test.go — 웹 경로(RunWeb, extraction="readability")로 색인한 artifact를 ctr_fetch로 조회 → 응답의 `source_coords_exact`가 false인지 검증(mcp.go:289 sourceCoordsExact 분기 직접 실증).
- [ ] **(8) applyMemLimit 원인 보존**: transform.go Spawn(:209 이후)에서 applyMemLimit/ProbeIsolation 실패를 ErrNoIsolation으로 바꾸는 지점을 찾아(`grep -n "ErrNoIsolation" internal/transform/transform.go`) `fmt.Errorf("...: %w", ErrNoIsolation)` 형태에 원인 err을 함께 보존(`errors.Join(ErrNoIsolation, err)` 또는 메시지에 `%v` 포함 + sentinel `%w` — `errors.Is(err, ErrNoIsolation)` 판정 유지가 조건). 기존 판정 테스트 GREEN 유지 확인.
- [ ] **전체 확인** — Run: `go test -p 1 ./...` → GREEN, `CGO_ENABLED=0 GOOS=linux go build ./...` → OK.
- [ ] **Commit** — `fix: 계획2 마이너 이월 8건 — transform timeout Config·Article.Title·DNS 다중주소·주석 드리프트·err prefix·좌표 테스트·원인 보존 (세션01 §4)`

---

### Task 7: LICENSE(ELv2) + NOTICE(서드파티) + README(이름 매핑표) (설계 §10)

**Files:**
- Create: `LICENSE`, `NOTICE`, `README.md`

**Interfaces:** 없음(문서만). 태그 전 필수 항목.

**계약(설계 §10):**
- `LICENSE`: Elastic License 2.0 전문(https://www.elastic.co/licensing/elastic-license 의 공식 텍스트 그대로 — 변형 금지). licensor 표기는 파일 하단 Copyright 줄: `Copyright (c) 2026 wotjr1649 (context-router contributors)`.
- `NOTICE`: ① upstream 고지 — "This project is an independent Go implementation informed by the tool contract of ctxscribe / context-mode (Elastic License 2.0). Not affiliated with or endorsed by the upstream authors." + upstream 저작권 줄 ② 서드파티 표 — 각 direct 의존성의 모듈 경로·버전·라이선스 종류·저작권자. **구현 시 검증 필수**: 각 모듈의 실제 LICENSE 파일을 모듈 캐시(`go env GOMODCACHE` 하위)에서 열어 종류·저작권자를 확인하고 표에 기입한다(추정 금지). 대상: `codeberg.org/readeck/go-readability`, `github.com/JohannesKaufmann/html-to-markdown/v2`, `github.com/modelcontextprotocol/go-sdk`, `go.starlark.net`, `golang.org/x/net`, `golang.org/x/sys`, `modernc.org/sqlite`.
- `README.md`: ① 1문단 소개(local-first single-binary Go MCP server …) ② 고지 문구 — `independent Go implementation informed by the ctxscribe tool contract; not affiliated` **원문 그대로 포함** ③ 도구 이름 매핑표:

| ctxscribe (oracle) | context-router |
|---|---|
| ctx_search | ctr_search |
| ctx_fetch *(계약 재정의 — oracle의 fetch는 web fetch)* | ctr_fetch (저장본 byte-exact 조회) |
| ctx_execute (v0.2 예약) | — (D3: exec는 v0.2) |
| ctx_index | ctr_index |
| ctx_fetch_and_index | ctr_fetch_and_index |
| — | ctr_transform (D2 신규) |
| — | ctr_global_search (Q4) |
| ctx_stats / ctx_doctor / ctx_upgrade / ctx_purge | CLI: stats / doctor / upgrade / purge (D6) |

  ④ 설치·등록(doctor 스니펫 안내), 프로필/플래그 요약(§7/§8 표 축약) ⑤ License 절(ELv2 + NOTICE 참조). *매핑의 정밀 필드 대응은 T8의 `docs/oracle-mapping-ko.md` 링크로 위임.*

- [ ] **Step 1: 서드파티 라이선스 실사** — `go env GOMODCACHE`에서 7개 모듈 LICENSE 확인·기록.
- [ ] **Step 2: LICENSE/NOTICE/README 작성** — 위 계약대로.
- [ ] **Step 3: 검증** — README 매핑표의 도구 명이 코드와 일치하는지(`grep -o 'ctr_[a-z_]*' internal/mcp/mcp.go | sort -u` 대조), NOTICE 표의 버전이 go.mod와 일치하는지 확인.
- [ ] **Step 4: Commit** — `docs: LICENSE(ELv2)·NOTICE(서드파티 실사)·README(이름 매핑표) — 태그 전 필수 (설계 §10)`

---

### Task 8: 게이트 1·2 — oracle 동등성 golden + retrieval recall@k 하네스

**Files:**
- Create: `docs/oracle-mapping-ko.md`, `testdata/oracle/` (fixture corpus + golden), `testdata/retrieval/` (labeled corpus)
- Modify: `internal/search/search_test.go` (또는 `//go:build retrieval` 태그 파일 1개 — D13 내에서 판단)

**Interfaces:**
- Consumes: `search.Query`, `ingest` 색인 경로.
- Produces: 게이트 1·2의 증거(테스트 + 기준선 문서) — T10 게이트 체크리스트가 참조.

**계약(설계 §12 게이트 1·2):**
- **oracle**: ctxscribe 1.3.0 (로컬 참조 구현: `C:\Users\js\Documents\ClaudeCode\context-mode`). 게이트 1의 의미 = *"이름·필드 매핑 문서 하에"* 동등성 — byte 동일성이 아니라 **문서화된 매핑 기준의 의미 동등성**이다.
- `docs/oracle-mapping-ko.md`: ctr_search↔ctx_search, ctr_index↔ctx_index의 입력/출력 필드 대응표 + 의도적 차이 목록(RRF 파라미터·스니펫 창·budget 의미 등 — 실제 구현을 읽고 확인된 것만 기재) + **동등성 판정 기준 명문화**: "동일 corpus·동일 쿼리에서 oracle top-3 결과의 소스 파일 집합과 ctr top-3 소스 파일 집합의 교집합 ≥ 2 (10개 쿼리 중 8개 이상)".
- golden 생성: oracle 실행이 필요한 부분은 **1회성 수동 생성**이다 — 구현자는 `testdata/oracle/README.md`에 생성 절차(oracle 쪽 인덱싱·검색을 수행한 정확한 명령)를 기록하고, 산출된 golden JSON(쿼리→oracle top-k 소스 목록)을 커밋한다. oracle 실행이 로컬 환경에서 불가능하면 **BLOCKED로 보고**하고 대안(수동 생성을 사용자에게 요청)을 제시한다 — 임의 골든 조작 금지.
- **retrieval 하네스**: `testdata/retrieval/`에 fixture 문서 ≥12개(코드·로그·마크다운 혼합, 각 ≥3청크 분량 — `strings.Repeat`/생성 스크립트로 팽창 금지, 실제적 내용) + 쿼리 ≥10개와 각 쿼리의 정답 소스(relevance label) 매니페스트(JSON). 테스트가 recall@5·recall@10을 계산해 **기준선 이상**을 단언하고, 측정값을 테스트 로그로 출력. 최초 실행에서 얻은 실측값을 기준선으로 `docs/gates-v0.0.1-ko.md`(T10에서 생성 — 이 태스크에서는 임시로 테스트 상수에 기록)와 테스트 상수 `minRecallAt5`에 고정한다(향후 회귀 방지 플로어).

- [ ] **Step 1: 매핑 실사** — oracle 구현(`context-mode` 저장소)의 search/index 입출력 스키마를 읽고 매핑표 초안 작성(추정 금지 — 코드 근거 인용).
- [ ] **Step 2: oracle golden 생성 절차 수행·기록** — 위 계약대로. 불가 시 BLOCKED 보고.
- [ ] **Step 3: 동등성 테스트 작성·실행** — `TestOracleEquivalence`: golden의 corpus를 ctr로 색인 → 동일 쿼리 실행 → 판정 기준 적용. Run: `go test -p 1 -run TestOracleEquivalence ./internal/search/` → PASS(기준 미달이면 원인 분석 후 매핑 문서의 "의도적 차이"에 기록하고 기준 재협의 — 임의 완화 금지, 사용자 확인 필요).
- [ ] **Step 4: retrieval 하네스 작성·기준선 기록** — `TestRetrievalRecall`: 위 계약. Run: `go test -p 1 -run TestRetrievalRecall ./internal/search/` → PASS + 로그에 recall 실측 출력.
- [ ] **Step 5: Commit** — `test(search): 게이트1 oracle 동등성 golden + 게이트2 recall@k 하네스·기준선`

---

### Task 9: CI — 3-OS 매트릭스 + 크로스빌드 6타깃 + unix RLIMIT 실측 (게이트 3/7/8/12)

**Files:**
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: 전체 테스트 스위트(-p 1 규율), `worker_unix.go`의 RLIMIT_AS self-apply 경로.
- Produces: 게이트 3(경로 3-OS)·7(DB 동시성 3-OS)·8(unix 메모리 상한 실측)·12(크로스빌드+크기 기록)의 증거 — T10 체크리스트가 CI run URL을 증거로 인용.

**계약:**
- job `test`: matrix `os: [ubuntu-latest, windows-latest, macos-latest]`, `actions/setup-go`(go.mod의 go 버전 사용, `go-version-file: go.mod`), `go test -p 1 ./...`. ubuntu에서만 추가로 `go test -p 1 -race ./...`(CGO 가용 러너 — 게이트 7·8의 unix 실측이 이 job에서 처음 실행된다: worker RLIMIT_AS·트리킬 테스트가 skip 없이 도는지 로그 확인 조건 포함).
- job `lint`: gofumpt diff 검사(`gofumpt -l . | 비어있음 확인`) + `golangci-lint` v2 `default: standard`(+misspell) — 규약 §9. golangci 설정 파일은 저장소에 없으면 `.golangci.yml` 최소 구성으로 함께 생성.
- job `crossbuild`: `CGO_ENABLED=0`으로 6타깃 `{linux,darwin,windows} × {amd64,arm64}` `go build -trimpath -ldflags "-s -w" -o dist/...` 후 **바이너리 크기를 GitHub Step Summary에 기록**(게이트 12 "크기 기록").
- 전 job 트리거: `push`(브랜치 전체) + `pull_request`.
- 메모리 캡 규율: Go 테스트는 `-p 1`이 핵심 캡(전역 test-guard 취지 준수). 5,000-doc 성능 스모크가 별도 태그면 CI 제외 유지(설계 §12 게이트 6 성능 스모크는 로컬 수동).

- [ ] **Step 1: ci.yml 작성** — 위 계약 그대로 3 job.
- [ ] **Step 2: 로컬 예행** — CI 러너 없이 검증 가능한 것: `gofumpt -l .` 결과 0건, `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` 등 6타깃 로컬 빌드 성공, `go vet ./...` 통과. golangci-lint 로컬 미설치면 그 사실을 보고서에 기록(CI에서 최초 실행).
- [ ] **Step 3: Commit + push로 CI 트리거** — `ci: 3-OS 매트릭스·크로스빌드 6타깃·race — 게이트 3/7/8/12`. push 후 `gh run watch`(또는 `gh run list --branch feat/v0.0.1-global-cli --limit 1`)로 3-OS 결과 확인. **실패 시 로그를 읽고 수정 커밋**(특히 macos/ubuntu 최초 실행 — Windows에서만 검증돼 온 테스트의 OS 편차가 여기서 드러난다. 경로·권한·TTY 가정 편차는 근본 수정하되, OS 특성상 불가한 테스트는 명시적 `t.Skipf`+사유로 처리하고 보고서에 기록).

---

### Task 10: 게이트 11·10 마감 + 게이트 체크리스트 + 버전 정리 (태그 준비 완료)

**Files:**
- Modify: `internal/mcp/mcp_test.go` (토큰 예산·프로토콜 위생), `cmd/context-router/main.go:26`·`internal/mcp/mcp.go:26`·`internal/netfetch/netfetch.go:54` (버전 통일)
- Create: `docs/gates-v0.0.1-ko.md`

**Interfaces:**
- Consumes: T1~T9 전부의 증거.
- Produces: 게이트 13항 증거표 → 태그 판정 문서.

**계약:**
- **게이트 11(스키마 토큰 예산)**: `TestSchemaTokenBudget` — NewServer(기본 3-도구)로 tools/list 결과 JSON을 직렬화해 바이트 수 측정, 근사 토큰 = bytes/4(근사임을 주석 명기 — Claude 정확 tokenizer는 비공개, 바이트 상한 관리가 실질 게이트). 상한 상수 `maxToolSchemaBytes`: **최초 실측값 × 1.2를 반올림해 고정**하고 실측값·상한을 `docs/gates-v0.0.1-ko.md`에 기록(Codex 무deferral 부담 관리 — 설계 §2.3).
- **게이트 10(프로토콜 위생)**: 기존 E2E(stdout 오염 0)의 커버 범위를 확인하고, 툴콜 **실행 중** stderr 로그 발생 상황에서 stdout이 JSON-RPC만 유지되는지 1케이스 보강(Plan 1 Task 8 minor "stdout purity 테스트 narrow" 해소). Claude Code·Codex **실 등록 스모크는 수동 게이트** — 체크리스트에 수동 절차(등록 명령·tools/list 확인·호출 1회·cancellation)와 결과 기입란을 만들고 사용자 확인을 받는다.
- **버전 통일**: `version`/`serverVersion` = `"0.0.1"`, `defaultUserAgent` = `"context-router/0.0.1"` (릴리스 커밋 — `-dev` 제거는 이 태스크에서만).
- **`docs/gates-v0.0.1-ko.md`**: 13항 각각 — 게이트 요지 1줄 / 증거(테스트 명·CI run·수동 절차) / 상태(PASS·수동 대기·N/A 사유). 3-OS 항목은 T9 CI run URL을 증거로. **미통과 항목이 있으면 태그 금지(게이트 13)** — 문서 하단에 태그 절차 기록: "PR 머지 → main CI GREEN → 수동 스모크 2건 사용자 확인 → `git tag v0.0.1 && git push origin v0.0.1`".
- **잔여 이월 명시**: 이 계획이 다루지 않는 것(설계 §14 유보 그대로): v0.1 session events, v0.2 exec/Shadow Recall/OTel·Codex 어댑터/무작위 A/B, TOCTOU 완전판(openat2), nested-job Assign CI 실측, semantic 보강. 게이트 문서 하단 "v0.0.1 이후" 절에 기록.

- [ ] **Step 1: 게이트 11 테스트 작성·실측·상한 고정** — Run: `go test -p 1 -run TestSchemaTokenBudget ./internal/mcp/` → PASS + 로그에 실측 바이트.
- [ ] **Step 2: 게이트 10 보강 테스트** — Run: `go test -p 1 ./internal/mcp/ ./cmd/...` → PASS.
- [ ] **Step 3: 버전 통일 + 전체 GREEN** — Run: `go test -p 1 ./...` → GREEN.
- [ ] **Step 4: 게이트 체크리스트 문서 작성** — 13항 증거표 + 태그 절차 + 이후 이월.
- [ ] **Step 5: Commit** — `chore: v0.0.1 버전 통일 + 게이트 11/10 마감 + 게이트 증거표 (태그 준비)`

---

## 마감 절차 (태스크 아님 — 오케스트레이터 수행)

1. **최종 whole-branch 이중 리뷰**: 서브에이전트 리뷰어(전 브랜치 diff) **+** Codex `review --base main` 병행 → 병합 판정 → 수정 웨이브 → 재검증 Approved. (계획 1·2와 동일 프로토콜 — 프로젝트 CLAUDE.md standing protocol.)
2. **PR 생성**: `feat/v0.0.1-global-cli` → `main`. 본문에 계획 링크·게이트 문서 링크. CI GREEN 확인.
3. **세션 기록**: `docs/prompts/`에 session-02 기록 작성(7항목 규약) + commit/push.
4. **태그는 PR 머지 후** 게이트 문서 절차대로 — 수동 스모크 2건은 사용자 확인 필요(자동화 금지).

## 리스크·판단 메모

- T8 oracle golden은 외부 실행(ctxscribe)이 필요한 유일한 태스크 — BLOCKED 규칙(맹목 재시도 금지, Codex/웹 협업 또는 사용자 에스컬레이션)이 가장 먼저 발동할 후보.
- T9는 이 저장소 최초의 CI — macos/ubuntu 첫 실행에서 OS 편차 발견 가능성이 높다. 수정은 근본 원인 기준(가짜 skip 남발 금지).
- T2 병합 랭킹은 v0.0.1 최소 구현(RRF rank 기반 score 직병합) — semantic 정합은 §14 유보와 함께 후속.



