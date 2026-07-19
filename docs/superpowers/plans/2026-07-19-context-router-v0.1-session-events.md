# Context Router v0.1 (session events) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** worktree당 Session DB(1차 데이터) + 세션 3종 도구(`ctr_record_event`/`ctr_session_summary`/`ctr_export_events`) + 이벤트 FTS(ctr_search scope) + SessionEvent v1 export + retention 스윕 + fail-closed 손상 대응 — "무손실 복원"(설계 v0.1 전문, D14~D20).

**Architecture:** 신규 `internal/session` 패키지(저장 계층 + §26 wire struct 소유), store는 소형 예외 2건만(잠금 shared 공개화·hash 조회), FTS 질의·랭킹은 search 소유(reader 인자 수령), scope 결합·도구 등록은 mcp, JSONL/recover는 cli. 손상 대응은 fail-closed + shared/exclusive lease + 수동 복구 CLI(자동 rename 금지 — Codex NO-SHIP 반영).

**Tech Stack:** 기존 + `github.com/google/uuid v1.6.0`(indirect→**direct 승격만**, `NewV7()` — 신규 의존성 0).

## Global Constraints

- **정본 계약: `docs/context-router-design-v0.1-ko.md` 전문**(D14~D20, §1~§11). 본 계획의 모든 수치·문구·순서는 그 문서가 우선 — 충돌 시 설계서를 따르고 계획 수정을 보고.
- **선행 조건: v0.0.2 worker robustness 패치 머지**(별도 계획 `2026-07-19-context-router-v0.0.2-worker-robustness.md`) — 미머지 상태로 본 계획 착수 금지.
- v0.0.1 규약 승계: 자기 정의 인터페이스 0 · 핸들러 ≤50줄·구체 호출 ≤2 · mcp는 database/sql·net/http·os/exec import 금지 · 오류는 **신규 코드 없이** 기존 9종 재사용, sentinel→코드 매핑은 `toToolError`(mcp.go:103) 단일점 · 저장 본문 반환에 `untrusted: true` · 호출마다 `LedgerAppend`.
- D13: 신규 소스는 `internal/session/session.go`(+`session_test.go`) 1~2파일(선호 밴드 300~1,000줄). store 변경은 예외 2건만(§8): `store_lock_*.go` shared 모드+공개화, hash 조회 메서드 1개.
- 상한(설계 §3.1): event_type ≤64B `[a-z0-9_]+` · summary ≤2048B · attributes 직렬화 ≤4096B · artifact_refs/related_resources 각 ≤16개 · related 항목 ≤512B(URI, 스킴 필수) · 이벤트 총 직렬화 ≤8KB — 초과 = INVALID_ARGUMENT.
- 테스트: `go test -p 1 ./...`(메모리 캡 전역 규칙), secret canary는 **분할 리터럴**(`"xox"+"b-..."`), 응답 분할 규율(파일 전체 재작성 금지, 긴 테스트 데이터는 `strings.Repeat`/testdata).
- 게이트: `docs/context-router-design-v0.1-ko.md` §9 G1~G9 + 사전 규칙(호스트 의존 비결정 검증 = 비차단). 각 태스크의 테스트가 해당 게이트의 실체다.
- 커밋 trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. 리뷰: 태스크 리뷰 서브에이전트, 통합·최종 체크포인트는 서브에이전트 + Codex 병렬 1패스(재리뷰는 서브에이전트만).

---

### Task 1: store 예외 2건 — 잠금 shared 공개화 + hash 조회 (설계 §8)

**Files:** Modify `internal/store/store_lock_windows.go`, `internal/store/store_lock_unix.go`, `internal/store/store.go`(조회 메서드 + 공개 잠금 API), `internal/store/store_test.go`

**Interfaces:**
- Produces: `func AcquireLock(path string, shared bool) (release func(), err error)` — **논블로킹 고정**(실패 즉시 error). unix: `flock(LOCK_SH|LOCK_EX, LOCK_NB)`, windows: `LockFileEx`(shared: flags 0 / exclusive: `LOCKFILE_EXCLUSIVE_LOCK`, 공통 `LOCKFILE_FAIL_IMMEDIATELY`). 기존 비공개 `tryLockFile`/`lockStore`는 이 함수를 경유하도록 리팩터(동작 불변 — exclusive·논블로킹 그대로).
- Produces: `func (s *Store) ArtifactHashByID(ctx context.Context, id int64) (string, error)` — artifacts 단일 행 조회, 미존재 `ErrNotFound`.
- Consumes: 없음(leaf).

- [ ] **Step 1: 실패 테스트** — ① shared+shared 동시 성립(두 release 전 둘 다 성공) ② shared 보유 중 exclusive 실패, release 후 성공 ③ exclusive 보유 중 shared 실패 ④ 논블로킹(블로킹 대기 없음 — 실패가 즉시) ⑤ ArtifactHashByID: 등록 artifact의 hash 반환·미존재 id → ErrNotFound ⑥ 기존 lockStore 회귀(재오픈 경합 테스트 무변).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/store/ -run 'TestAcquireLock|TestArtifactHashByID' -v` → undefined.
- [ ] **Step 3: 구현** — OS별 파일에 shared 분기 추가 + 공개 래퍼. store.go 본체 추가는 조회 메서드 1개와 래퍼 노출만.
- [ ] **Step 4: GREEN** — store 전체 + `GOOS=linux go build ./...` 교차 컴파일.
- [ ] **Step 5: Commit** — `"feat(store): AcquireLock shared 모드 공개화 + ArtifactHashByID (v0.1 설계 §8 예외 2건)"`

### Task 2: internal/session 저장 계층 — 스키마·lease·발급 (설계 §2, §6.2)

**Files:** Create `internal/session/session.go`, `internal/session/session_test.go`; Modify `go.mod`(uuid direct 승격)

**Interfaces:**
- Consumes: `store.AcquireLock`(T1).
- Produces:
  - `type Options struct{ RetentionSec int64; Producer string }` / `func Open(dir string, opts Options) (*DB, error)` — dir = `projects/<pid>/worktrees/<wid>`. 순서: ① `session.lock` **shared** 취득·보유(실패 → sentinel `ErrLeaseHeld`) ② 마커 `session.recover-pending` 존재 → `ErrRecoverPending` ③ DB 파일 미존재 시 `session.init.lock` **exclusive**로 생성 직렬화(완료 후 해제) ④ PRAGMA(D9 세트)·`user_version=1` 멱등 DDL 단일 트랜잭션(§2.2 전문: session_events + 인덱스 4종 + sessions; FTS DDL은 T6에서 같은 applySchemaV1에 합류 — 출시 전 v1 완성 중이므로 마이그레이션 아님) ⑤ quick_check — **명시적 malformed만** `ErrCorrupt`(BUSY·일시 I/O는 재시도 후 미확정=통과, §6.2 보수 판정) ⑥ `session_id = uuid.NewV7()` 발급, sessions 행 INSERT(started_at·producer·retention_sec), `session_start` 이벤트 자동 append.
  - `type Event struct{ Type, Summary string; Attributes json.RawMessage; ArtifactRefs, Related []string; Supersedes string }`(입력 표현 — redaction·검증 완료본을 받는다) / `func (d *DB) Append(ev Event) (id int64, eventID string, ts int64, err error)` — event_id = NewV7, supersedes 존재 검증(미존재 → `ErrNotFound` 계열 sentinel).
  - `func (d *DB) SessionID() string`, `func (d *DB) Reader() *sql.DB`(search·mcp 조회용), `func (d *DB) Close() error`(lease 해제 포함).
  - sentinel: `ErrLeaseHeld`, `ErrRecoverPending`, `ErrCorrupt` — 셋 다 mcp 조립의 fail-closed 신호(T10). `toToolError` 매핑은 저장소 불용 계열 → `STORAGE_UNAVAILABLE`(T3에서 배선).

- [ ] **Step 1: 실패 테스트** — ① Open→재Open 멱등(user_version 유지) ② 2-프로세스 동시 append 무손실(**G2**: 두 `*DB`(같은 dir, 프로세스 경계는 t.Run 병렬 고루틴 + 실제 별도 연결로 근사, E2E 2-프로세스는 T10) — 각 100건 append 후 총 200건·event_id 전유일) ③ session_start 자동 기록 + sessions 행(producer·retention_sec) ④ 마커 존재 시 ErrRecoverPending ⑤ 파일 바이트 훼손(헤더 덮어쓰기) 후 Open → ErrCorrupt·**원본 파일 불변**(**G8 전반부**) ⑥ supersedes 미존재 → 오류 ⑦ UUIDv7 시간정렬(연속 발급 사전순 증가).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/session/ -v` → undefined.
- [ ] **Step 3: 구현** — `go get github.com/google/uuid@v1.6.0`(direct 승격). PRAGMA·연결 규율은 store.go D9 세트를 session에 상수 복제 대신 **문자 그대로 동일 값**으로 작성하고 출처 주석(§2.1). writer 1연결 + txRetry 동형.
- [ ] **Step 4: GREEN** — session + store 회귀.
- [ ] **Step 5: Commit** — `"feat(session): Session DB 저장 계층 — 스키마 v1·lease 2파일·UUIDv7·fail-closed 신호 (v0.1 설계 §2·§6.2, G1·G2·G8)"`

### Task 3: ctr_record_event (설계 §3.1, §4)

**Files:** Modify `internal/mcp/mcp.go`(registerRecordEvent + toToolError 확장 + NewServer 배선), `internal/mcp/mcp_test.go`

**Interfaces:**
- Consumes: `session.DB.Append`/`SessionID`(T2), `store.ArtifactHashByID`(T1), `ingest.Redact`(기존 공개 순수 함수).
- Produces: 도구 `ctr_record_event{event_type, summary, attributes?, artifact_refs? []int64, related_resources? []string, supersedes?}` → `{event_id, session_id, ts}`. 핸들러 순서: 형식·상한 검증(Global Constraints 수치) → related 각 항목 URI 파싱(스킴 필수) → `artifact_refs` 해석: ArtifactHashByID → `artifact://<session_id>/sha256-<hash>`(정본 형식, 미존재 INVALID_ARGUMENT) → summary·attributes 직렬화·related 직렬화에 각각 `ingest.Redact` → attributes는 redaction 후 `json.Valid` 재검증(실패 시 필드 전체를 단일 redacted 문자열로 강등) → Append. annotation `DestructiveHint: false`. `toToolError`에 session sentinel 3종 → `STORAGE_UNAVAILABLE` 추가(단일점). NewServer가 session.Open 결과에 따라 등록(기본 등록 — Enable 불요; open 실패 모드는 T10).

- [ ] **Step 1: 실패 테스트** — ① round-trip: record → 반환 3필드 → Reader 직조회로 행 검증 ② 상한 위반 각 1건(type 65B·summary 2049B·attributes 4097B·refs 17개·총 8KB 초과) → INVALID_ARGUMENT ③ secret canary(**G4 기록 경로**): summary·attributes·related에 분할 리터럴 canary → 저장 행에 원문 부재 + `redaction='spans'` ④ artifact_refs: 유효 id → 정본 URI 저장 / 미존재 id → INVALID_ARGUMENT ⑤ supersedes 미존재 → NOT_FOUND ⑥ ledger 1행 ⑦ 스키마 게이팅(세션 3종이 기본 표면에 등장).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/mcp/ -run 'TestRecordEvent' -v`.
- [ ] **Step 3: 구현** — 핸들러 ≤50줄 준수(검증·redaction은 session 또는 mcp 비공개 헬퍼로 분리하되 wire 타입은 §26 관련만 session 소유 — 입력 wire struct는 mcp 소유 관례 유지).
- [ ] **Step 4: GREEN** — mcp 전체 회귀(기존 도구 게이팅 테스트 갱신 포함).
- [ ] **Step 5: Commit** — `"feat(mcp): ctr_record_event — 상한·redaction·정본 artifact URI (v0.1 설계 §3.1, G3·G4)"`

### Task 4: ctr_session_summary (설계 §3.2)

**Files:** Modify `internal/session/session.go`(조회 함수), `internal/mcp/mcp.go`(registerSessionSummary), 각 테스트

**Interfaces:**
- Consumes: T2 Reader/스키마, T1 ArtifactHashByID(missing 판정은 hash 존재 조회 재사용 — content.db에 해당 hash artifact 부재 시 `missing: true`).
- Produces: `session.Summarize(ctx, r *sql.DB, sessionID string, limitPerType int) (Summary, error)`(session 소유 — superseded 제외 = `event_id NOT IN (SELECT supersedes ... WHERE supersedes IS NOT NULL)`, idx_ev_sup 활용; 타입 그룹핑·시간 역순은 idx_ev_type). 도구 `ctr_session_summary{session_id?, limit?(기본 5·최대 20), max_return_bytes?(기본 8192)}` → `{checkpoint?, groups:[{event_type, events:[{event_id, session_id, ts, summary, artifact_refs:[{uri, missing?}]}], truncated}], untrusted: true}`. checkpoint = 최신 비superseded `session_checkpoint` 전문, groups 중복 없음, 예산 우선 1번 — **단독 초과 시 생략+truncated**(hard cap 유지). 기본 범위 worktree 전체(§2.4).

- [ ] **Step 1: 실패 테스트** — ① 3세션 분량 시드 후 무필터 summary → 전 세션 이벤트 타입별 그룹·시간 역순 ② session_id 필터 ③ superseded 제외(A를 B가 supersede → A 부재·B 존재) ④ checkpoint 규칙 3건(최신 비superseded 선정 / groups 비중복 / 소액 예산에서 checkpoint 생략+truncated) ⑤ missing refs: content.db에 없는 hash 참조 이벤트 → `missing: true`(오류 아님 — **D15 hint**) ⑥ 그룹별 truncated 개별 표기 ⑦ untrusted: true.
- [ ] **Step 2~4: FAIL→구현→GREEN** — `go test -p 1 ./internal/session/ ./internal/mcp/ -run 'TestSummar' -v` → 전체 회귀.
- [ ] **Step 5: Commit** — `"feat: ctr_session_summary — 타입 그룹·checkpoint 3규칙·missing hint (v0.1 설계 §3.2, G3)"`

### Task 5: ctr_export_events + SessionEvent v1 wire (설계 §3.3, D16)

**Files:** Modify `internal/session/session.go`(wire struct + Export 함수), `internal/mcp/mcp.go`(registerExportEvents), 각 테스트

**Interfaces:**
- Produces: `session.EventV1 struct`(**session 소유** — json 태그 camelCase: `schemaVersion`(상수 "1.0")·`eventId`·`sessionId`·`eventType`·`timestamp`(RFC3339, ts 변환)·`summary`·`artifactRefs`·`relatedResources`·`attributes`(RawMessage)·`privacyLabel`(상수 "internal")·`producer{name,version}`(sessions 조인 유도, 행 부재 시 version "unknown")·optional `supersedes`·`redaction`) — snake↔camel 매핑은 이 struct **1곳**. `session.Export(ctx, r *sql.DB, after int64, sessionID string, limit int) (events []EventV1, nextAfter int64, err error)` — rowid 순, 무필터(superseded 포함). 도구 `ctr_export_events{after?, session_id?, limit?(기본 50·최대 200), max_return_bytes?}` → `{events, truncated, next_after}`.

- [ ] **Step 1: 실패 테스트** — ① golden(**G6**): 시드 3건(supersedes·redaction·미지 event_type 포함) export → 필드 전수 일치(event_id·ts는 고정 주입 — Append를 직접 쓰지 않고 테스트 헬퍼로 행 삽입해 결정론 확보, §9 G3 마스킹 규약) ② schemaVersion·privacyLabel 상수 ③ producer: sessions 행 유도 + 행 삭제 후 "unknown" 폴백 ④ 커서: limit 1로 3회 순회 → 전 이벤트·next_after 단조 증가·중복 없음 ⑤ 미지 event_type 보존(§26) ⑥ superseded 포함(무필터).
- [ ] **Step 2~4: FAIL→구현→GREEN.**
- [ ] **Step 5: Commit** — `"feat: ctr_export_events + SessionEvent v1 wire(schemaVersion·producer 유도·rowid 커서) (v0.1 설계 §3.3, G6)"`

### Task 6: 이벤트 FTS (설계 §2.3)

**Files:** Modify `internal/session/session.go`(FTS DDL — applySchemaV1 합류 + 트리거), `internal/search/search.go`(QueryEvents), 각 테스트

**Interfaces:**
- Produces: session.db 내 `fts_ev_porter`/`fts_ev_trigram`(external content=session_events, **summary만** 색인) + ai/ad 트리거(content.db 패턴 동형 — UPDATE 트리거 불요, append-only·retention DELETE만). `search.QueryEvents(ctx, r *sql.DB, q string, limit int) ([]EventHit, error)` — `type EventHit struct{ EventID, SessionID, EventType string; TS int64; Summary string; Superseded bool }`; 기존 비공개 bm25/rrf 헬퍼 재사용(search→session 타입 의존 없음, reader 인자).
- Consumes: T2 스키마.

- [ ] **Step 1: 실패 테스트** — ① 색인 동기화: append 후 즉시 porter·trigram 양쪽 매치 ② retention DELETE 후 미매치(ad 트리거) ③ **payload 미색인**(attributes에만 있는 토큰 → 0건) ④ superseded 플래그: supersede된 이벤트도 검색되되 `Superseded: true`(**G5**) ⑤ RRF 결합(porter 우세·trigram 우세 쿼리 각 1) ⑥ FTS integrity(`INSERT INTO fts(fts) VALUES('integrity-check')` 통과).
- [ ] **Step 2~4: FAIL→구현→GREEN** — `go test -p 1 ./internal/session/ ./internal/search/ -v`.
- [ ] **Step 5: Commit** — `"feat: 이벤트 FTS(porter+trigram, summary만) + search.QueryEvents (v0.1 설계 §2.3, G5)"`

### Task 7: ctr_search scope 확장 + ctr_fetch 문구 + 게이트 11 재기준화 (설계 §3.4, §3.5)

**Files:** Modify `internal/mcp/mcp.go`(search 핸들러 scope 분기 + fetch description), `internal/mcp/mcp_test.go`(TestSchemaTokenBudget 재기준)

**Interfaces:**
- Consumes: `search.QueryEvents`(T6).
- Produces: `ctr_search`에 `scope?: "content"|"events"|"all"`(기본 "content" — 후방 호환, 기존 입력 무변). events/all 결과에 이벤트 섹션(EventHit 필드 그대로). session.db 불용 시 events/all → STORAGE_UNAVAILABLE(조용한 빈 결과 금지, §3.4). `ctr_fetch` description에 "저장된 artifact의 byte-exact 조회 — 웹 fetch 아님(웹은 ctr_fetch_and_index)" 추가. `TestSchemaTokenBudget` 기준치를 세션 3종+scope+문구 반영해 재측정·갱신(**G9** — 새 기준 수치를 커밋 메시지에 기록).

- [ ] **Step 1: 실패 테스트** — ① scope 기본값 content(기존 테스트 무변 통과) ② events: 시드 후 검색 → EventHit·superseded 플래그 ③ all: content+events 동시 ④ fail-closed 상태에서 events → STORAGE_UNAVAILABLE(T10 배선 전이므로 session=nil 주입으로) ⑤ fetch description 문구 존재 ⑥ TestSchemaTokenBudget 새 기준.
- [ ] **Step 2~4: FAIL→구현→GREEN** — 핸들러 ≤50줄 유지(scope 분기는 헬퍼).
- [ ] **Step 5: Commit** — `"feat: ctr_search scope(events/all) + ctr_fetch 문구 + 스키마 예산 재기준 (v0.1 설계 §3.4·§3.5, G5·G9)"`

### Task 8: retention 스윕 + purge 확장 (설계 §5)

**Files:** Modify `internal/session/session.go`(Sweep), `cmd/context-router/main.go`(`--retention-events` 플래그 + 시작 배선), `internal/cli/cli.go`(purge session 대상), 각 테스트

**Interfaces:**
- Produces: `session.Sweep(ctx, d *DB, now time.Time) (deleted int64, err error)` — **per-session**: `DELETE FROM session_events WHERE session_id IN (SELECT session_id FROM sessions WHERE retention_sec > 0) AND ts < now - (해당 세션 retention_sec)`(EXISTS 조인 — 미표명 세션 불가침). now는 값 주입(**G7 결정론**). main: `--retention-events <dur>`(`time.ParseDuration`, 기본 0=off) → Options.RetentionSec, 시작 시 Sweep 1회 log-and-continue + 삭제 건수 stderr 1줄. purge: session 대상 추가(`--sessions` — session.db 파일 삭제 계열은 기존 purge 의미론에 정합하게, `.bak-*`·마커는 제외). dangling supersedes 허용(§5 — 스윕 후 superseded 판정은 잔존 행 기준, T4 질의가 자연 충족).

- [ ] **Step 1: 실패 테스트** — ① 시계 주입: 세션 A(retention 1h)·세션 B(미표명) 시드, now+2h로 Sweep → **A 이벤트만 삭제·B 불가침**(**G7 + M-4 회귀**) ② 삭제 건수 반환 ③ off(전 세션 미표명) → 0 ④ 교정 이벤트 삭제 후 superseded였던 이벤트가 summary에 재등장(dangling 허용 의미론) ⑤ `.bak-*`·`recover-pending` 파일이 purge에서 잔존 ⑥ ParseDuration 형식(`"720h"` OK, `"30d"` 오류).
- [ ] **Step 2~4: FAIL→구현→GREEN** — cli·main 회귀 포함.
- [ ] **Step 5: Commit** — `"feat: retention 스윕(per-session·시계 주입) + purge session 대상 (v0.1 설계 §5, G7)"`

### Task 9: CLI session export/recover + doctor (설계 §6.3, §7)

**Files:** Modify `cmd/context-router/main.go`(sub "session" 허용), `internal/cli/cli.go`(runSessionExport/runSessionRecover + doctor 확장), `internal/session/session.go`(인양 헬퍼), 각 테스트

**Interfaces:**
- Consumes: T1 AcquireLock(exclusive), T2 스키마·ErrCorrupt, T5 EventV1(JSONL 재사용 — 매핑 1곳 유지).
- Produces: `context-router session export (--project) [--worktree <wid|path>] [--session] [--after <rowid>]` — stdout JSONL(행당 EventV1, UTF-8 no BOM); **worktree 특정 계약**: 다중 후보면 목록 출력 후 오류(생략은 단일일 때만). `context-router session recover (--project) [--worktree]` — §6.3의 7단계 그대로: exclusive lease → quick_check 재확인(**마커 존재 시 무작업 종료 금지** — 잔존 상태 검증 후 완료 확인되면 마커만 삭제) → 마커 생성+fsync → rowid 보존 인양(SQLITE_CORRUPT 시 마지막 성공 rowid 이후 구간 재시도 루프; sessions 동일) → `wal_checkpoint(TRUNCATE)`+연결 종료로 단일 파일화 → quick_check·스키마 검증 → 게시(-shm→-wal→db 순 `.bak-<ts>` rename, 인양본→session.db, 디렉터리 fsync, 마커 삭제) → 건수 보고. doctor: session.db quick_check·lease shared 프로브·마커 존재 3항목.

- [ ] **Step 1: 실패 테스트** — ① export JSONL: 행 파싱 → EventV1 round-trip(**G6 CLI 측**) ② worktree 계약: worktree 2개 시드 → --worktree 없이 오류+후보 목록 / 1개면 생략 허용 ③ recover 정상 경로: 훼손 DB → 인양·게시·마커 소멸·`.bak-<ts>` family 존재·이벤트 rowid 보존 ④ **마커 중단 재개**(**G8**): 게시 직전 단계에서 인위 중단(단계 함수 분리로 주입) → 재실행이 이어서 완료 ⑤ **건강 DB+마커**(N-2 회귀): 게시 완료·마커 잔존 상태 조작 → 재실행이 마커만 삭제 ⑥ 서버 실행 중(shared 보유) recover → 즉시 거부 ⑦ doctor 3항목 출력.
- [ ] **Step 2~4: FAIL→구현→GREEN** — 인양 루프는 session 헬퍼로(cli는 배선만, 규약 소유 경계 유지).
- [ ] **Step 5: Commit** — `"feat(cli): session export(JSONL)/recover(마커·인양·게시)/doctor — worktree 계약 (v0.1 설계 §6.3·§7, G6·G8)"`

### Task 10: fail-closed 조립 + E2E + 게이트 문서 (설계 §6.2, §9)

**Files:** Modify `internal/mcp/mcp.go`(NewServer 조립), `cmd/context-router/main.go`(session.Open 배선), `cmd/context-router/main_test.go`(E2E), `docs/context-router-design-v0.0.1-ko.md` §9 어댑터 스니펫 갱신(추가만 — 기존 서술 불변), Create `docs/gates-v0.1-ko.md`

**Interfaces:**
- Produces: main→NewServer에 `*session.DB`(nil 허용) 전달 — Open이 `ErrCorrupt`/`ErrRecoverPending`/`ErrLeaseHeld`면 nil + stderr 1줄 경고, NewServer는 nil이면 세션 3종 미등록 + search events scope 비활성(darwin ctr_transform 선례 패턴). content 도구는 정상. `docs/gates-v0.1-ko.md`: 사전 규칙(비결정=비차단) + G1~G9 표(각 게이트 ↔ 본 계획 태스크 테스트 매핑·결과 칸).
- Consumes: 전 태스크.

- [ ] **Step 1: 실패 테스트** — ① E2E(실바이너리, go-sdk ClientSession+CommandTransport — main_test.go 기존 패턴): record → summary → export → search(scope=events) round-trip ② E2E 손상: session.db 훼손 후 스폰 → 세션 3종 도구 목록 부재·ctr_search(content) 정상 ③ **2-프로세스 lease**(**G2·G8 실프로세스**): 바이너리 2개 동시 스폰·양쪽 record 성공(shared 공존) → 총 이벤트 무손실 ④ 마커 존재 스폰 → 세션 도구 부재 + stderr 안내.
- [ ] **Step 2~4: FAIL→구현→GREEN** — `go test -p 1 ./...` 전체 + E2E. 게이트 문서 작성(결과 칸은 이 시점 실측으로 기입).
- [ ] **Step 5: Commit** — `"feat: fail-closed 조립 + E2E + gates-v0.1 (v0.1 설계 §6.2·§9, G8·게이트 문서)"`

### Task 11: 문서 정리 + 통합 검증 + PR (설계 §8, §11)

**Files:** Modify `docs/context-router-code-architecture-ko.md`(개정 4항: session 패키지 그래프·D16 wire 예외·store 예외 2건 근거·§ 매핑), `THIRD-PARTY-NOTICES`(uuid direct 승격 반영 — P1 관례: 모듈 캐시 byte-compare 재생성), `docs/context-router-vision-proposal-ko.md`(로드맵 v0.1 행 체크)

- [ ] **Step 1: 아키텍처 문서 개정 4항 반영** — 설계 §8 목록 그대로.
- [ ] **Step 2: NOTICE 재생성** — uuid(BSD-3) 추가 확인, 알파벳 정렬·byte-compare.
- [ ] **Step 3: 전체 회귀 + 3-OS CI GREEN** — `go test -p 1 ./...`.
- [ ] **Step 4: 통합 교차 리뷰** — 서브에이전트(전체 diff, 설계서 대조) + Codex `review --base main` 병렬 1패스 → 발견 머지·수정(재리뷰 서브에이전트만).
- [ ] **Step 5: PR·머지·태그** — PR `feat: v0.1 session events`, CI GREEN 확인 후 머지 → gates-v0.1 결과 확정 → `git tag v0.1.0 && git push origin v0.1.0`.

## Self-Review 기록
- 설계서 커버리지: §1.1 범위 6항 → T3~T9(도구 3종·FTS·export·retention·recover·fetch 문구), §2 → T2·T6, §3 → T3~T7, §4 → T3(G4), §5 → T8, §6 → T2·T9·T10, §7 → T9, §8 → T1·T11, §9 G1~G9 → 각 태스크 Step 1에 게이트 표기, §10 마일스톤 0(v0.0.2)은 별도 계획. §1.2 비범위 침범 없음.
- 타입 스레딩: `session.Event`/`EventV1`/`EventHit`/`AcquireLock`/`ArtifactHashByID`/`Sweep(now 주입)` 시그니처를 Interfaces 블록 간 대조 완료. STORAGE_UNAVAILABLE 매핑은 T3에서 1회 배선(단일점).
- 플레이스홀더 스캔: "TBD/적절히/나중에" 부재 — 상한·문구·순서는 설계서 § 참조로 정본 위임(정본 우선 규칙 명시).
