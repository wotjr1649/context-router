# Context Router 구현 규약 — 아키텍처·코드 컨벤션

> 문서 상태: 확정 (2026-07-17) — 3채널 브레인스토밍(웹 1차 소스 리서치 · 아키텍처 합성 · Codex 교차 모델) 병합. 개정 4항(2026-07-20, v0.1 설계 §8 — session 패키지 그래프·D16 wire 예외·store 예외 2건 근거·§ 매핑) 반영.
> 지위: 구현 계획(writing-plans)과 모든 코드 리뷰의 판정 기준. 설계서와 충돌 시 설계서 개정이 선행한다.
> 준수: D8(의존성 7·프레임워크 금지) · D13(반파편화) · HANDOFF 최소화 원칙(ponytail full)

**설계서 § 매핑** (v0.1부터 설계서가 2개 문서로 분리되며 §번호가 겹친다 — 예: 양쪽 다 §8을 가짐):

| 접두어 | 문서 |
|---|---|
| "설계 §N" (접두어 없음) | `context-router-design-v0.0.1-ko.md` |
| "v0.1 설계 §N" | `context-router-design-v0.1-ko.md` (델타 문서 — 여기 없는 계약은 v0.0.1이 그대로 유효) |

## 1. 원칙 서열과 근거

사용자 결정(D8·D13) > 이 규약 > Google Go Style/Uber Guide > 일반 관행. 외부 근거의 기준점: Google Go Style(소비자 정의 인터페이스, util 패키지 금지, 전역 상태 금지, 실물 테스트), Uber Guide(%w wrap, goroutine 수명, exit-once), 실전 코드베이스 계수 — **litestream 139파일에 인터페이스 2개, age 46파일에 3개**(전부 복수 구현이 실존하는 seam). 우리는 0개에서 시작한다.

## 2. 패키지 의존 그래프 (확정)

```text
cmd ──┬─ mcp ─┬─ search ──→ store
      │       ├─ session → store          # 조회만 — v0.1, 아래 "session 신설" 참조
      │       ├─ ingest ──→ store, ident
      │       ├─ transform → store        # blob 경로 조회용
      │       ├─ netfetch                 # leaf — 저장하지 않는다
      │       └─ store, ident             # Config 배선(Store 포인터·Canon)
      ├─ cli ─┬───────────→ store, ident, session   # session — v0.1, export/recover 서브커맨드
      │       └─ hook ─────→ session, ident, store, ingest  # v0.2 §2 — 훅 서브커맨드 위임(store·ingest는 T5~T7 소비)
      ├─ store                            # store.Open 직접 호출
      └─ ident                            # leaf, 순수 함수
store · ident · netfetch · transform : internal 상호 의존 0 (leaf)
search → session: 없음(의도) — search.QueryEvents(v0.1)는 *sql.DB reader를 인자로만 받는다,
                  session 타입을 import하지 않는다(D16 "search→session 타입 의존 없음").
```

- **단방향 규칙**: 표에 없는 import는 전부 금지. 어떤 패키지도 `mcp`를 import하지 않는다.
- **순환 차단 3규칙**: ① store는 스키마·트랜잭션·blob 원시 연산만 — 청킹·redaction·랭킹을 모른다(완성된 행을 받는다). ② netfetch는 바이트+메타(`Result{Raw, Body, MediaType, Extraction, FinalURL}`)만 반환 — "fetch 후 ingest" 배선은 mcp 핸들러가 한다. ③ 오류 코드 변환은 mcp에만 있다(§6).
- **search·store 분리 유지 판정**: Google "항상 같이 import되면 합쳐라" 기준은 불성립(cli·ingest가 store를 단독 사용). 합치면 store God-package 부패 경로와 충돌 — 분리 유지(2:1 판정).
- **session 신설(v0.1, 설계 §8)**: Session DB(worktree당 1개, content.db와 물리 분리 — D10 승계)의 스키마·PRAGMA·lease·append·조회·§26 wire 표현(`EventV1`, D16) 전담. store로 합치지 않는 이유는 search·store 판정과 동일 논리 연장 — 소유 스키마가 다른 DB는 패키지도 분리한다. session→store 의존은 **조회 전용**(`AcquireLock` 재사용, `ArtifactHashByID` 호출)뿐이고 store→session 의존은 0.
- **store 소형 예외 2건의 근거(v0.1, 설계 §8)**: ① `AcquireLock(path string, shared bool)` 공개화(`store.go`) — 기존 `lockStore`가 내부적으로 쓰던 unix/windows `tryLockFile` OS별 잠금 원시 코드를 session의 lease 2파일(lifetime·init lock)이 그대로 재사용한다(OS별 잠금 코드 중복 금지, D13). ② `ArtifactHashByID(ctx, id) (string, error)` 신설 — artifact_id→content_hash 단일 조회. session이 이 조회를 raw SQL로 직접 하면 store가 소유한 artifacts 스키마 지식이 session으로 유출된다(캡슐화 역행) — 좁은 조회 메서드 1개를 store에 추가하는 편이 §10 "store 만능화 방지"(기능 질의 조립 금지)와 상충하지 않으면서 더 낫다. 이 2건 외 content.db 마이그레이션은 0건.
- **이벤트 FTS 질의는 search 소유, `ArtifactHashExists`도 search 소유(v0.1, 설계 §8 정신의 확장 — T4 리뷰 판정)**: `search.QueryEvents`(porter+trigram RRF, session.db reader 인자)는 설계 §8이 명시적으로 예고한 이동. `search.ArtifactHashExists(ctx, st *store.Store, hash) (bool, error)`(ctr_session_summary의 artifact_refs missing 힌트 판정용)는 설계 §8이 이름을 못 박지 않았지만, store 예외가 이미 2건 소진됐고 session은 "조회만" 원칙상 가질 수 없어 — search가 이미 `st.Reader()`로 artifacts에 raw SQL을 실행하는 기존 패턴을 갖고 있다는 점에서 store 불변 유지 + 자연스러운 소유자로 T4 리뷰가 판정했다.
- 책임 1줄: **ident** canonicalization·ID(설계 §3.2) / **store** DB 수명·PRAGMA·단일 트랜잭션 계약·blob IO(§3.3~3.5) / **search** FTS 질의→BM25→RRF→스니펫·예산(§4.1) + 이벤트 질의(v0.1 설계 §8) / **session** Session DB 수명·스키마·append·lease·§26 wire·복구(v0.1 설계 §2·§3·§6) / **ingest** §3.0 파이프라인+경로 정책(§4.4) / **netfetch** SSRF 불변식+readability+html→md(§5.2·§4.5) / **transform** starlark 엔진·worker 프로토콜·OS 상한(§4.3) / **mcp** 도구 스키마·등록·핸들러(§4) / **cli** doctor·stats·purge·upgrade(§7) + session export/recover(v0.1 설계 §6.3·§7) / **hook** 훅 서브프로세스 진입점 — stdin JSON→cc: 세션 append·fail-open·deadline·drops 사이드카(v0.2 설계 §2), 계측 매핑은 T5~T7.

## 3. 타입 소유권

| 타입 | 소유 | 규칙 |
|---|---|---|
| `Artifact`·`Source`·`Chunk` | store | 스키마 행 표현 — 스키마 소유자가 타입 소유. ingest는 store.Chunk를 생산 타입으로 재사용(중복 정의 금지) |
| `Hit` | search | 검색 산출물 계약 |
| `Result` | netfetch | fetch 산출물 |
| worker `Request`/`Result` | transform | stdin/stdout JSON 프로토콜 양단의 단일 정의 |
| MCP 요청/응답 struct (jsonschema 태그) | mcp | 내부 타입→wire 변환은 각 도구 핸들러 1곳. 내부 타입에 MCP 태그 부착 금지 |
| `session.EventV1`(SessionEvent v1 export wire) | **session — D16 예외(v0.1)** | "wire 타입은 mcp 소유" 규약의 유일한 예외. `ctr_export_events`(MCP, camelCase JSON+커서)와 CLI `session export`(JSONL) 양쪽이 공유하는 wire 표현이라 mcp에 두면 cli→mcp 의존이 생긴다(금지, §2 단방향 규칙) — session에 두면 매핑 지점이 1곳으로 유지된다. mcp 핸들러는 이 타입을 그대로 반환만 하고 자체 변환을 하지 않는다(다른 MCP 요청/응답 struct와 달리 핸들러 내부 wire 변환이 없음) |

공유 `types`/`models`/`domain` 패키지 금지.

## 4. 캡슐화

- **자체 정의 인터페이스 0개.** 두 번째 *실존* 구현이 생기기 전까지 `Clock`·`Repository`·`Fetcher`·`Runner`류 금지. stdlib 인터페이스(io.Reader/Writer 등)는 자유. transform 경계는 Go 인터페이스가 아니라 **프로세스+JSON 프로토콜**이다(3채널 만장일치) — `RunWorker(r io.Reader, w io.Writer)`로 충분.
- **테스트 심 3수단**(인터페이스 대체): ① 순수 함수 분리(SSRF 판정 = `netip.Addr→verdict`), ② stdlib func 필드 주입(`http.Transport.DialContext`), ③ 실물(임시 디렉터리의 진짜 SQLite, 테스트용 자기 바이너리). 테스트 목적 인터페이스 wrapping·mock 클라이언트 금지(Google 규칙).
- **생성자**: 필수 인자 ≤3개면 개별 인자(`store.Open(path string, readOnly bool)`), 정책 값 ≥4개면 검증 가능한 `Config` struct(`netfetch.Config{...}`). 의존성을 Config에 숨기지 않는다. **functional options 금지**(기본값·필수값 grep 불가 + 투기적 확장점).
- **배선**: `main()`은 `run(ctx, args, stderr) error` 1회 호출 + `os.Exit` 1회(exit-once). `signal.NotifyContext`는 run 안. store는 main이 1회 열어 **구체 포인터**를 전달. 도구별 등록 함수(`mcp.RegisterSearch(s *search.Service)`) — 거대 Deps struct·nil 필드·전역 singleton·service locator 금지.
- export는 "다른 internal 패키지가 지금 호출하는 것"만. `Get` 접두사 금지(`Counts`, 비용 크면 `Compute`/`Fetch`).

## 5. 파일 구성 — 반파편화 (D13)

- **기본형**: 패키지당 소스 1~2개(`<pkg>.go` + 자연 이음새 최대 1개), 테스트는 패키지당 `<pkg>_test.go` 1개.
- **분리가 정당한 3경우**: ① OS별 build tag(`_windows.go`/`_unix.go` — Job Object/rlimit), ② ~1,000줄 초과 + 응집된 이음새, ③ 생성물/embed 자산.
- **금지**: 타입별 1파일, doc.go 단독 파일(패키지 주석은 주 파일 상단 — 내용은 "1줄 책임 + 설계서 §번호"), helpers.go/utils.go, 함수 하나짜리 파일.
- **선호 밴드 300~1,000줄** (상한은 초대형 파일의 역효과 방지). **v0.0.1 목표 ≈12~15 소스 파일**(테스트 제외).
- store가 밴드를 넘을 때의 **사전 승인된 이음새**: `schema` / `blob` / `recovery` (그 전까지 store.go 하나). 사전 분할 금지.
- **mcp 실제 분리(v0.1, 사전 승인 ② 적용 — 새 이음새 목록 아님)**: 세션 도구 3종 추가로 `mcp.go`가 밴드를 넘어 `mcp_session.go`(ctr_record_event·ctr_session_summary·ctr_export_events, 응집된 이음새)로 분리했다. `mcp.go`는 도구 스키마·등록·핵심 핸들러+`toToolError`를 유지.
- **session(v0.1) 실제 구성 — 5파일**(예상 1~2파일에서 실제 응집 경계에 맞춰 확장, 사전 승인 ②): `session.go`(스키마·PRAGMA·lease 2파일·UUIDv7·append) / `summary.go`(`ctr_session_summary` 집계) / `export.go`(§26 `EventV1` wire·export 페이지네이션, D16) / `retention.go`(스윕 엔진, §5) / `recover.go`(fail-closed 판정·수동 복구 7단계, §6). 각 파일이 독립 기능 경계(스키마/요약/export/retention/복구)를 가져 병합이 오히려 응집도를 해친다 — 추가 분할은 금지, 현 5파일이 안정 상태.

## 6. 오류 규약

- **패키지별 sentinel**(자기 의미만 소유, MCP 코드 import 금지): `store.ErrNotFound`·`store.ErrUnavailable`, `ingest.ErrWorkspace`·`ingest.ErrUnsupported`, `netfetch.ErrDenied`, `transform.ErrBudget`·`transform.ErrOutputLimit`.
- **단일 변환 지점**: `internal/mcp`의 `toToolError(error)` 하나가 sentinel→MCP 코드 9종(설계 §4.0) 매핑을 전담. 매핑 없는 오류는 `INTERNAL`로 뭉개고 상세는 stderr slog에만. *(별도 ctrerr 패키지는 기각 — "error framework" 과잉설계이며 D13과 상충. worker 경계의 오류 직렬화는 transform이 자기 프로토콜 안에서 kind 문자열로 처리.)*
- 문맥 wrap은 항상 `fmt.Errorf("동작: %w", err)` — 사용자 입력·절대경로·env·비밀값 미포함(생성 시점 위생, §12 canary가 오류 메시지도 검사). 판정은 `errors.Is`(제한 수치 필요한 typed error만 `errors.As`). 문자열 비교·`%v` 재포장 금지.
- cancellation은 SDK로 그대로 반환. cli는 자체적으로 오류→종료 메시지 변환(MCP 코드 무관), `os.Exit`은 main에서만.

## 7. 동시성 규약

- **쓰기 직렬화 지점은 writer `*sql.DB`(`SetMaxOpenConns(1)`) 하나뿐** — 프로세스 내부 mutex를 안전성 근거로 삼지 않는다(프로세스 간 안전은 §3.5 단일 트랜잭션+CAS의 몫). reader 풀 ≤4. 별도 writer goroutine·actor 금지.
- ingest 워크: `min(GOMAXPROCS, 4)` 고정 worker pool(읽기·hash·redaction·chunking 병렬, 등록은 직렬 제출). 파일별 goroutine 생성 금지.
- transform worker 동시 스폰 ≤2(최악 512MB 예측 가능), 초과는 ctx-aware semaphore 대기.
- goroutine은 스폰한 소유자가 취소·회수·wait까지 책임(fire-and-forget·패키지 레벨 스폰·공개 메서드 반환 후 잔존 goroutine 금지). `context.Context`는 첫 인자·struct 저장 금지·커스텀 context 타입 금지. zero-value `sync.Mutex` 그대로 사용.
- 종료 시퀀스: stdio EOF → 신규 호출 중단 → root ctx cancel → ingest 회수 → worker 트리 전멸·wait → `wal_checkpoint(TRUNCATE)` 시도 → ledger/reader/writer DB 순서로 close.

## 8. 테스트 규약

실물 우선(mock 금지 — §4 심 3수단). 패키지별 전략(설계 §12 게이트 연결):

| 패키지 | 전략 |
|---|---|
| ident | table-driven + fuzz(canonicalizer) — hostile-paths (게이트 3) |
| ingest | golden(청킹) + canary(경계 걸친 키 포함, 게이트 4) + fuzz(chunker 불변식·redaction 스캐너) |
| netfetch | SSRF matrix table(게이트 5) + fuzz(classifier) + golden(html corpus→md, 게이트 9) |
| search | oracle golden 동등성(게이트 1) + FTS integrity(게이트 6) |
| store | 다중 프로세스 CAS·kill 내구성(게이트 7) + fuzz(fetch 범위 산술·UTF-8 스냅) |
| transform | golden(결정론) + 상한·트리킬(게이트 8) |
| mcp | stdio 스모크·stdout 오염 0·스키마 토큰 예산(게이트 10·11) |
| cli | purge 확인 규칙 table-driven |

fuzz는 CI에서 시드 corpus 회귀만(상시 fuzzing은 로컬 수동), 5,000-doc 성능 스모크는 별도 태그 — memory-capped CI(전역 test-guard 규율) 준수.

**비밀 캐너리 작성 규칙 (2026-07-18 추가)**: 새로 추가하는 비밀 패턴 테스트 픽스처는 소스에 연속 리터럴로 쓰지 않고 **런타임 결합**(`"xoxb-" + "1234..."` 또는 `string(rune(...))`)으로 구성한다 — GitHub push protection 등 소스 바이트 스캐너 오탐 방지(실사례: Slack 캐너리 차단). 기존 캐너리는 GitHub allowlist로 처리됨(이력 rewrite 비용 > 실익).

## 9. 도구 체인 (개발 도구 — 의존성 아님)

- 포맷: **gofumpt**(설정 0). 린트: **golangci-lint v2 `default: standard`**(errcheck·govet·ineffassign·staticcheck·unused) + misspell — 추가 린터는 점진 도입만.
- `go vet` 1.25+의 waitgroup·hostport 분석기 활용(동시성 검증), 릴리스마다 `go fix`(1.26 modernizers) 1회.

## 10. 부패 방지 계약 (rot-path 예방 — 3채널 병합)

1. **mcp god package 방지**: 핸들러 = decode/validate → 구체 호출 ≤2회 → encode, ≤50줄. mcp는 `database/sql`·`net/http`·`os/exec` **import 금지**(기계적 검사 가능).
2. **store 만능화 방지**: 영속성 불변식만 소유 — ranking·redaction·경로 정책 유입 금지. 기능 질의 조립은 기능 패키지가, 원시 SQL·트랜잭션·blob은 store가.
3. **ingest 잡동사니화 방지**: 정규화→redaction→chunk→등록 파이프라인만 — `net/http`·복구 SQL import 금지.
4. **표면 증식 방지**: 설계서 §번호가 없는 신규 동작·플래그 금지(설계서 개정 선행). 플래그 5개 초과 시 재검토(설계 §8).
5. **전역 상태·init() 금지**(드라이버 blank import 제외), 패키지 var는 불변 테이블만.

## 11. 판정 기록 (갈린 지점의 심판)

| 쟁점 | 판정 | 근거 |
|---|---|---|
| ctrerr 공유 오류 패키지 vs 패키지별 sentinel | **sentinel + mcp 단일 변환** | 매핑이 9×8 고정 소규모라 N×M 우려 불성립, worker 직렬화는 transform 프로토콜 내부 처리 가능, "error framework"는 과잉설계(Codex), 패키지 수 증가는 D13 역행 |
| doc.go 별도 파일 | **기각** | D13 — 패키지 주석은 주 파일 상단 |
| 파일 400줄 분리 검토 | **기각 → 300~1,000 밴드** | D13 (사용자 지시) |
| search를 store로 통합 | **분리 유지** | "항상 같이 import" 기준 불성립 + store 부패 경로 충돌 |
| ingest 병렬도 semaphore=4 vs min(GOMAXPROCS,4) | **min(GOMAXPROCS,4)** | 저코어 머신 과잉 스폰 방지 |
| 과소설계 경고(Codex): blob-커밋 원자성·CAS·FTS 동기의 crash 윈도, worker 트리 회수·재검증의 OS 편차 | **구현 계획의 중점 검증 항목으로 승계** | 설계 게이트 7·8과 연결 |
