# Context Router v0.0.1 계획 2 (M4~M5: transform worker + netfetch) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ctr_transform`(OS 메모리 상한 worker, starlark)과 `ctr_fetch_and_index`(SSRF 불변식 I1~I8 + D12 readability→markdown)를 기본/옵트인 표면에 편입 — 설계 기본 3-도구(search/fetch/transform) 완성.

**Architecture:** transform = 자기 바이너리 재실행 worker(`__transform-worker` 숨김 모드, stdin/stdout JSON, build tag로 OS별 상한) — in-process 금지(설계 §4.3). netfetch = leaf 패키지(저장 안 함), fetch→ingest 배선은 mcp 핸들러(규약 §2).

**Tech Stack:** 기존 + `go.starlark.net`(T1), `codeberg.org/readeck/go-readability` + `github.com/JohannesKaufmann/html-to-markdown/v2`(T5 — Step 0에서 모듈 경로·버전 실측 후 pin, D8/D12 승인분).

## Global Constraints

- 규약 전문 준수: 인터페이스 0 · D13(신규 소스 파일 = transform.go, worker_windows.go, worker_unix.go, netfetch.go 4개 + 테스트; build tag 분리는 D13 허용 사유 ①) · mcp는 database/sql·net/http·os/exec import 금지(netfetch 배선은 핸들러가 함수 호출로) · 핸들러 ≤50줄·구체 호출 ≤2 · 오류: 신규 sentinel `transform.ErrBudget`·`transform.ErrOutputLimit`·`netfetch.ErrDenied` + toToolError 확장(BUDGET_EXCEEDED/OUTPUT_LIMIT_EXCEEDED/NETWORK_DENIED).
- 응답 분할 규율(32K 예방): 한 턴 하나의 작은 단계, 문자열 상수 200자 금지(HTML corpus는 파일 생성 또는 Repeat), 파일 전체 재작성 금지.
- transform 상한(설계 §4.3): inputs ≤8·총합 ≤16MB·개당 ≤8MB, 메모리 hard 256MB, timeout 10s, 출력 32KB 기본/256KB 상한, emit 증분 검사, 동시 worker ≤2, 상한 불가 환경 = 도구 비활성(fallback 금지).
- SSRF 불변식 I1~I8(설계 §5.2) 전부. `--net-allow-local`일 때만 127.0.0.1.
- 커밋 trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. module `github.com/wotjr1649/context-router`.
- 계획 1 이월분 편입: UNC 슬래시 변형 Fold 테스트(T2에 동봉), store Open MkdirAll sanitizeIOErr(T3에 동봉), extraction!="" 좌표 테스트(T6에서 web 색인으로 자연 검증).

---

### Task 1: transform 엔진 코어 (starlark, in-library)

**Files:** Create `internal/transform/transform.go`, `internal/transform/transform_test.go`
**Interfaces:**
- Produces: `type Caps struct{ MaxSteps int64; MaxOutputBytes int }`, `type Request struct{ Script string; Inputs []Input; Args map[string]string; Caps Caps }`, `type Input struct{ ID int64; Text string }`, `type Result struct{ Output string; StepsUsed int64; Truncated bool; ErrKind string }` (ErrKind: ""|"budget"|"output_limit"|"script"), `func Eval(req Request) Result` — **순수 함수**(파일·네트워크·env 접근 없음), worker(T2)와 mcp 오류 매핑이 소비. sentinel: `ErrBudget`, `ErrOutputLimit`.

- [ ] **Step 1: 실패 테스트** — ① 내장 8종 각 1케이스(regex_extract/json_project/line_window/head/tail/count/sort/dedupe — inputs[0].text()/lines()/json() + emit 조합, 기대 출력 명시) ② 결정론(같은 입력 2회 → 동일 Result) ③ 스텝 상한(무한 루프성 스크립트 → ErrKind "budget") ④ emit 증분(출력 상한 초과 시점에 즉시 "output_limit") ⑤ 금지 확인(starlark 코드에서 파일·모듈 접근 시도 → script 오류, 크래시 없음).
- [ ] **Step 2: FAIL 확인** — `go test ./internal/transform/ -v` → undefined.
- [ ] **Step 3: 구현** — `go get go.starlark.net@latest`(pseudo-version 기록). starlark.Thread{Steps 상한}, predeclared: inputs(list of struct with text/lines/json 메서드), args(dict), emit(builtin — bytes.Buffer 누적, 초과 시 즉시 중단 플래그), 내장 8종은 Go builtin으로 등록. resolve 옵션은 기본(모듈/재귀 비활성 유지). Result.ErrKind 매핑: `*starlark.EvalError`+스텝 소진 → budget / emit 초과 → output_limit / 그 외 → script.
- [ ] **Step 4: GREEN + FuzzEval**(스크립트·입력 임의 — panic 없음) 5s.
- [ ] **Step 5: Commit** — `git add internal/transform go.mod go.sum && git commit -m "feat: transform 엔진 코어 — starlark+내장 8종+상한 (설계 §4.3, T1)"` (+trailer)

### Task 2: worker 프로세스 + OS 상한

**Files:** Create `internal/transform/worker_windows.go`, `internal/transform/worker_unix.go`; Modify `internal/transform/transform.go`(프로토콜·클라이언트), `cmd/context-router/main.go`(`__transform-worker` 분기), 각 테스트
**Interfaces:**
- Produces: `func RunWorker(r io.Reader, w io.Writer) error`(worker 측: Request JSON 1건 읽고 Eval 후 Result JSON 1건 쓰기), `func Spawn(ctx context.Context, selfExe string, req Request) (Result, error)`(부모 측: 프로세스 스폰+상한+timeout 10s+트리킬, 동시 ≤2 semaphore), `func applyMemLimit(cmd *exec.Cmd, bytes int64) error`(build tag별 — windows: Job Object CREATE_SUSPENDED→Assign→Resume+JOB_OBJECT_LIMIT_PROCESS_MEMORY+KILL_ON_JOB_CLOSE / unix: Setrlimit RLIMIT_AS via SysProcAttr 또는 사전 fork 설정), 실패 시 `ErrNoIsolation` sentinel(도구 비활성 신호).
- Consumes: T1 Eval/Request/Result.

- [ ] **Step 1: 실패 테스트** — ① 프로토콜 round-trip(RunWorker를 파이프로 직접 — 프로세스 없이) ② Spawn 정상 경로(자기 테스트 바이너리 재실행 패턴: `os.Executable()`+`-test.run=TestHelperWorker` 관용구 또는 빌드된 실바이너리) ③ **메모리 폭발 격리**: `s='A'*1024; for i in range(30): s=s+s` 스크립트 → worker가 상한으로 죽고 부모는 오류 Result 수신·생존(§12 게이트 8) ④ timeout: 무한 루프 → 10s 내 트리킬 ⑤ 동시 ≤2(3개 스폰 시 3번째 대기). ⑥ [이월] `ident.Fold`의 `//?/UNC/` 슬래시 변형 테스트 1건(ident_test.go).
- [ ] **Step 2: FAIL 확인.**
- [ ] **Step 3: 구현** — main.go에 인자 `__transform-worker`면 `transform.RunWorker(os.Stdin, os.Stdout)` 후 즉시 종료(플래그 파싱 전 분기). Windows Job Object는 x/sys/windows 직접 호출(구현 규약 §2 — transform은 leaf 유지, x/sys는 stdlib 준함). unix는 syscall.Setrlimit를 자식에서 적용해야 하므로 환경변수 `CTR_WORKER_MEM=<bytes>`를 전달해 RunWorker 진입 시 self-limit(Setrlimit) 적용 방식 허용(주석 명시).
- [ ] **Step 4: GREEN** — Windows 로컬 실측 필수(③④가 이 머신에서 실제 통과), unix 분기는 컴파일 확인+CI 이월 주석.
- [ ] **Step 5: Commit** — `"feat: transform worker — OS 메모리 상한·트리킬·동시 2 (설계 §4.3, T2)"`

### Task 3: mcp ctr_transform 등록 (기본 표면 3-도구 완성)

**Files:** Modify `internal/mcp/mcp.go`, `internal/mcp/mcp_test.go`; Modify `internal/store/store.go`(blob 텍스트 로더 1함수 — artifact_id→stored text, 상한 인자), `cmd/context-router/main.go`(selfExe 전달)
**Interfaces:**
- Produces: 도구 `ctr_transform{script, inputs []int64(artifact_id), args map, max_output_bytes}` → Eval via Spawn → `{result, steps_used, truncated}`. toToolError에 ErrBudget→BUDGET_EXCEEDED, ErrOutputLimit→OUTPUT_LIMIT_EXCEEDED 추가. `readOnlyHint:true`. 프로필: 기본(search/fetch/transform 3개 — 게이팅 테스트 갱신). ErrNoIsolation 환경이면 등록 자체 생략+stderr 경고 1줄.
- Consumes: T2 Spawn, store 로더(inputs 총합 16MB·개당 8MB 검증은 mcp 입력 검증에서).

- [ ] **Step 1: 실패 테스트** — 게이팅(기본 3개), round-trip(색인→artifact_id로 transform: `emit(str(len(inputs[0].text())))` → 정확 길이), 상한 위반 매핑(budget 스크립트→BUDGET_EXCEEDED), inputs 총합 초과→INVALID_ARGUMENT, ledger 기록. ⑥ [이월] store Open의 MkdirAll에 sanitizeIOErr 적용+경로 미노출 테스트.
- [ ] **Step 2~4: FAIL→구현→GREEN** (전체 회귀 포함).
- [ ] **Step 5: Commit** — `"feat: ctr_transform 등록 — 기본 3-도구 표면 완성 (설계 §4.2.3, T3)"`

### Task 4: netfetch — SSRF 불변식 fetcher

**Files:** Create `internal/netfetch/netfetch.go`, `internal/netfetch/netfetch_test.go`
**Interfaces:**
- Produces: `type Config struct{ AllowLocal bool; ExtraPorts []int; MaxBytes int64; Timeout time.Duration }`, `type Result struct{ RawHTML []byte; Body []byte; MediaType string; Extraction string; FinalURL string }`(T4에서 Extraction=""·Body=원문 — 변환은 T5), `func Fetch(ctx context.Context, cfg Config, rawURL string) (Result, error)`, `func ClassifyAddr(a netip.Addr) string`("ok"|"block" — 순수 함수), sentinel `ErrDenied`. leaf — store/ingest import 금지.

- [ ] **Step 1: 실패 테스트** — ① ClassifyAddr matrix(§12 게이트 5): loopback/RFC1918/link-local/169.254.169.254/CGNAT 100.64.0.0/10/NAT64 64:ff9b::/96/v4-mapped ::ffff:127.0.0.1(Unmap 후 판정)/zone 있는 IPv6 거부/공인 IP ok — 표 20행+. ② httptest 서버(127.0.0.1) — AllowLocal=false → ErrDenied / true → 성공. ③ redirect: 로컬 서버가 Location: http://169.254.169.254/ → ErrDenied(hop 재검증) / 6회 redirect → 오류. ④ https→http 강등 hop 거부(로직 단위 테스트). ⑤ 크기 초과 스트리밍 중단. ⑥ 환경 프록시 무시(HTTP_PROXY 설정 후에도 direct — Transport.Proxy nil 확인은 구조 검사로).
- [ ] **Step 2~4: FAIL→구현→GREEN + FuzzClassify** — 구현: I1~I8 그대로 — literal IP 우선 파싱→Unmap→zone 거부, hostname은 전 레코드 검증 후 **검증된 IP:port로 custom DialContext**(재조회 없음), TLS ServerName=hostname, CheckRedirect로 수동 순회(매 hop 전체 재적용), Transport{Proxy:nil}, 포트 80/443+Extra.
- [ ] **Step 5: Commit** — `"feat: netfetch SSRF 불변식 I1~I8 (설계 §5.2, T4)"`

### Task 5: D12 파이프라인 — readability → 충실도 판정 → markdown

**Files:** Modify `internal/netfetch/netfetch.go`(+테스트), Create `internal/netfetch/testdata/html/` corpus(문서형 2·기사형 1·비기사형 1 — 테스트가 생성 가능하면 생성)
**Interfaces:** Fetch가 text/html일 때 Result{Body=markdown, Extraction:"readability"|"full", RawHTML=원문 보존}. 그 외 미디어는 Body=원문·Extraction="".

- [ ] **Step 0: 의존성 실측** — readeck go-readability의 정확한 모듈 경로(`codeberg.org/readeck/go-readability` 가정)와 최신 태그, `github.com/JohannesKaufmann/html-to-markdown/v2` + table 플러그인 — `go get` 성공·API 1분 확인을 report에 기록. 경로 다르면 실측 우선.
- [ ] **Step 1: 실패 테스트** — ① 문서형 HTML(코드 블록·표 포함) → markdown에 코드펜스·표 보존+nav/footer 부재 ② 충실도 판정: pre/code 보존율 <50% 픽스처 → Extraction:"full" 전환 ③ <500자 추출 → full ④ RawHTML 보존 ⑤ 비HTML(json) 무변환.
- [ ] **Step 2~4: FAIL→구현→GREEN** — 판정은 DOM 노드 수 비교(추출 전후 pre/code/table count + 가시 텍스트 비율 <30%).
- [ ] **Step 5: Commit** — `"feat: D12 파이프라인 — readability+충실도 판정+markdown (설계 §4.5, T5)"`

### Task 6: mcp ctr_fetch_and_index 등록 + E2E

**Files:** Modify `internal/mcp/mcp.go`(+테스트), `internal/store/store.go`(raw blob 보존 지원 — Registration에 RawBlob []byte 선택 필드: 비색인 blob 저장+sources.raw_blob_hash 기록), `internal/ingest/ingest.go`(web 등록 헬퍼 — netfetch.Result+url→§3.0 파이프라인, Kind:"web", Extraction 기록), `cmd/context-router/main_test.go`(E2E 추가)
**Interfaces:**
- 도구 `ctr_fetch_and_index{url, max_bytes}` (--enable net일 때만 등록) → 핸들러: netfetch.Fetch → ingest.RunWeb → `{artifact_id, title, byte_length, extraction, indexed_chunks, snippet(≤1KB)}`. toToolError: ErrDenied→NETWORK_DENIED.

- [ ] **Step 1: 실패 테스트** — 게이팅(net 없으면 미등록), httptest+--net-allow-local 경유 round-trip(fetch_and_index→search로 본문 검색→hit, **extraction 필드와 SourceCoordsExact=false(web) 확인** [이월 검증]), raw blob 파일 존재+FTS 미등록, ErrDenied→NETWORK_DENIED.
- [ ] **Step 2~4: FAIL→구현→GREEN** — E2E: 실바이너리 `--enable net --net-allow-local`로 httptest URL 색인→검색 round-trip 1건 추가.
- [ ] **Step 5: Commit** — `"feat: ctr_fetch_and_index 등록+raw 보존+E2E (설계 §4.5, T6)"`

## Self-Review 기록
- 설계 §4.3/§4.5/§5.2/§12 게이트 5·8·9 → T1~T6 커버. 이월 3건(UNC/T2, MkdirAll/T3, extraction/T6) 편입, TOCTOU 완전판·WAL 최초 경합은 계획 3 유지.
- 타입 스레딩: transform.Request/Result·netfetch.Result·store.Registration.RawBlob 시그니처를 Interfaces 블록 간 대조 완료.
- 32K 예방: 각 태스크에 응답 분할 규율 상속(Global Constraints), HTML corpus는 파일/생성 방식.
