# Context Router v0.1 설계서 (session events)

> **상태: 브레인스토밍 합의 반영 초안 (2026-07-19, session-05).**
> v0.0.1 설계서(`context-router-design-v0.0.1-ko.md`)의 **델타 문서** — 여기 없는 계약은 v0.0.1이 그대로 유효하다.
> 계약 승계: 설계 기준서(`context-router-graph-engine-go-mcp-design-ko.md`) §26 SessionEvent v1, §6.2 이벤트 어휘.
> 손상 대응(§6)은 Codex adversarial-review 1패스(NO-SHIP 판정)를 반영해 원안(자동 rename)을 기각하고 재설계했다.

## 0. 결정 이력 (v0.1 신규, D13까지는 vision-proposal)

| # | 결정 | 근거 요약 |
|---|---|---|
| D14 | 이벤트 = 요약+포인터(덤프 금지). event_type 자유 문자열 + 권장 어휘. 신원 필드(event_id·session_id·ts·producer)는 서버 기입 | §26이 export 측에 "미지 eventType 보존"을 요구 — 생산 측만 닫는 것은 비대칭. 기준서 §7.4 |
| D15 | Session DB = 단일 이벤트 테이블 + JSON payload. artifact 참조는 content-hash URI **hint**(GC root 아님). 이벤트 FTS 포함(ctr_search scope 확장) | append-only 계약과 1:1, `.bak` 인양 단순. 정수 id는 재구축 시 rowid 재배정으로 오귀속 위험. FTS는 사용자 결정(복구 시 자유 텍스트 탐색 선투자) |
| D16 | export 이원화: MCP 도구 = §26 camelCase JSON 배열 + 커서, JSONL = CLI `session export`. wire struct는 `internal/session` 소유 | JSON 안 JSONL은 이중 인코딩. 매핑 지점 1곳, cli→mcp 의존 회피 |
| D17 | retention: 서버 시작 시 1회 스윕 엔진 + **기본 무기한**(보존 기간 opt-in). `.bak` 스윕·purge 제외. auto_vacuum 미사용 | 호스트가 지우는 기록의 로컬 생존이 제품 피치 — 공격적 기본 삭제는 자기 반박(Claude Code cleanupPeriodDays 반발 실증). 이벤트 공간 비용 미미(건당 ≤8KB) |
| D18 | 손상 대응: **fail-closed**(세션 도구만 미등록) + 수동 CLI 복구(shared/exclusive lease). 자동 rename/recreate 기각 | Codex critical: 복구 잠금은 이미 열린 sibling writer를 차단하지 못함 → 유일본 분기. 3파일(-wal/-shm) 이동 비원자성. 가용성 저하가 조용한 분기보다 안전 |
| D19 | worker robustness 3건(GOMEMLIMIT·RLIMIT_AS 여유분·kill 사유 구분)은 **v0.0.2 선행 패치**로 분리. ctr_fetch 설명 강화만 v0.1 편입 | v0.1 개발이 transform 스모크를 소비 — flake 원천 선제거. 회귀 귀속 분리 |
| D20 | 게이트: v0.1 신규 표면 한정 승계(6~8항) + "호스트 의존 비결정 검증은 비차단" 사전 규칙 + 게이트 11 재기준화 | v0.0.1 실측: 고비용은 게이트 10(호스트 비결정을 차단 게이트화). 형식 자체는 실결함을 잡음 |

## 1. v0.1 제품 계약

### 1.1 범위

- Session DB(worktree당 1개) + MCP 도구 3종: `ctr_record_event` · `ctr_session_summary` · `ctr_export_events` (모두 **기본 등록** — ctr_index의 opt-in 사유인 워크스페이스 파일 읽기가 없음).
- 이벤트 FTS + `ctr_search` scope 확장.
- SessionEvent v1 export (MCP + CLI JSONL).
- retention 스윕 엔진(기본 off) + purge CLI의 session 대상 확장.
- Session DB 손상 fail-closed + 수동 복구 CLI.
- `ctr_fetch` 설명 강화(웹 fetch 오인 방지 — 개명은 보류, 문구만).

### 1.2 명시적 비범위 (v0.1)

- 훅·패시브 이벤트 수집(자동 계측) — v0.2. 따라서 tool_call/file_edit/git_diff/build_run/test_run 계열은 생산자가 없다.
- spill journal(손상 중 기록 지속) — v0.2 후보(§6.4).
- SessionEvent `repository{}` 필드 기입(commit/branch 수집) — optional이므로 v0.1 미기입, v0.2 후보.
- title dedup — 독립 소형 태스크로 분리(Session DB는 content.db 스키마를 건드리지 않으므로 묶을 근거 없음).
- worker robustness 3건 — v0.0.2 선행 패치(D19).

### 1.3 선행 조건

**v0.0.2 (worker robustness) 패치가 v0.1 착수 전에 머지**되어야 한다: (a) worker `GOMEMLIMIT`(캡의 ~80%, 최종 수치는 churn 20회 실측으로 확정; OS 캡은 하드 백스톱 유지 — GOMEMLIMIT은 soft limit), (b) linux RLIMIT_AS에 Go 런타임 VA 예약 여유분(CI 실측), (c) `worker killed` 사유 구분(취소/메모리/시간 — 기존 `"worker killed"` 접두사 유지 + 사유 접미사, ErrSummary 위생 계약 유지). 검증: 게이트 8 스위트 + churn 재현 + linux CI. 게이트 문서 신설 없음.

## 2. Session DB

### 2.1 배치·수명

- 경로: `projects/<pid>/worktrees/<wid>/session.db` (v0.0.1 §3.3 예약 승계; worktreeRoot는 이미 기록됨).
- content.db와 물리 분리(D10). **session.db는 파생물이 아닌 1차 데이터** — 이벤트는 어디서도 재구성할 수 없다. 이 비대칭이 §6(손상 대응)과 §7(retention 기본값)의 모든 차이를 결정한다.
- PRAGMA·연결 규율은 v0.0.1 §3.5(D9)와 동일: WAL, writer 1연결(`SetMaxOpenConns(1)`, `_txlock=immediate`) + reader, `txRetry`, 신규 생성 WAL 전환 경합은 `lockStore` 패턴 재사용. 독자 `PRAGMA user_version` 공간(v1=1), 멱등 DDL 단일 트랜잭션.
- 다중 프로세스: 두 호스트(예: Claude Code + Codex CLI)가 같은 worktree의 session.db에 동시 append하는 것은 **지원 토폴로지**. 순수 INSERT라 CAS 불필요, session_id가 프로세스별로 달라 자연 분리된다.

### 2.2 스키마 v1

```sql
CREATE TABLE IF NOT EXISTS session_events(
  id            INTEGER PRIMARY KEY,   -- rowid = append 순서(커밋 순 전순서)
  event_id      TEXT NOT NULL UNIQUE,  -- UUIDv7(시간정렬·전역유일) — export eventId·supersedes 대상
  session_id    TEXT NOT NULL,         -- 서버 프로세스 시작 시 발급(UUIDv7)
  event_type    TEXT NOT NULL,         -- CHECK 없음(§26 미지 타입 보존 의무)
  ts            INTEGER NOT NULL,      -- unix 초
  summary       TEXT NOT NULL,         -- 항상 존재하는 요약 — 복원·FTS의 최소 단위
  payload       TEXT,                  -- attributes JSON(redaction 후)
  artifact_refs TEXT,                  -- JSON 배열 ["artifact://sha256-...", ...]
  related       TEXT,                  -- JSON 배열(relatedResources)
  redaction     TEXT NOT NULL DEFAULT 'none',  -- none | spans (content.db와 동일 의미론)
  supersedes    TEXT                   -- nullable event_id — 교정 관계(삭제 금지, §6.2 승계)
);
CREATE INDEX IF NOT EXISTS idx_ev_session ON session_events(session_id, id);
CREATE INDEX IF NOT EXISTS idx_ev_ts      ON session_events(ts);   -- retention 스윕 전용
```

- **FK 없음**(supersedes 포함): `.bak` 인양을 단일 패스 행 복사로 유지하기 위해 참조 무결성은 기록 시점 애플리케이션 검증으로 대체.
- 세션 메타(호스트 종류·worktree root·목표 등)는 가변 컬럼이 아니라 **이벤트로 기록**한다: 서버 시작 시 `session_start` 이벤트 자동 append(payload: worktree root, producer 버전, 플래그 요약). immutable append 원칙과 정합.
- `invalidates`(기준서 §6.2)는 v0.1 제외 — `supersedes` + event_type 조합으로 표현 가능(YAGNI).
- 이벤트 타입 15종(§6.2) 중 v0.1 **권장 어휘**(스키마 강제 아님, 도구 description에 명시): `user_instruction` `constraint` `decision` `rejected_approach` `error` `note` `artifact_created` `compaction_summary` `session_checkpoint` (+ `session_start` — 서버가 자동 기록; 입력 차단은 두지 않는다(어휘 비강제 원칙). 세션 경계 판정은 session_id 기준이므로 모델의 중복 기록으로 오염되지 않음). warning은 error payload로, 자동 계측 계열은 v0.2로.

### 2.3 이벤트 FTS

- content.db와 동일한 porter+trigram 이중 FTS 패턴을 session.db 안에 구성하되, **색인 대상은 `summary`만** — payload(JSON)는 노이즈·비밀 표면이므로 제외.
- superseded된 이벤트는 색인에서 제거하지 않고 검색 결과에 `superseded: true` 플래그로 표기(§3.6 stale 철학과 대칭). `ctr_session_summary`는 superseded를 제외한다(§4.2).
- FTS 동기화 계약·integrity 게이트는 v0.0.1 §3.4의 규율 승계.

### 2.4 세션 식별 (확정 사실 기반)

MCP stdio에는 프로토콜 수준 세션 식별자가 없다(go-sdk v1.6.1 실코드 확인: `ServerSession.ID()`는 Streamable HTTP 전용). 따라서 **stdio 1연결 = 서버 프로세스 1개 = 세션 1개**로 정의한다:

- `session_id` = 서버 프로세스 시작 시 UUIDv7 발급. **입력으로 받지 않는다**(호스트/모델이 지어내는 경로 차단).
- 컴팩션은 같은 프로세스 → 같은 세션. 호스트 재시작·`--resume`은 새 프로세스 → 새 세션.
- 복구 시나리오는 "새 세션이 이전 세션들의 이벤트를 읽는" 교차-세션 조회로 성립 — 그래서 `ctr_session_summary`/`ctr_export_events`의 기본 범위는 **worktree 전체**다(§4.2, §4.3).
- UUIDv7 채택 근거: 시간정렬+전역유일이 필요하고(동시 세션 2개가 같은 DB에 기록), `github.com/google/uuid v1.6.0`이 이미 모듈 그래프에 있어 신규 의존성이 없다(indirect→direct 승격만).

## 3. MCP 도구 계약 (v0.0.1 §4.0 공통 규칙 승계)

공통: snake_case 입력 + 한국어 jsonschema 설명, 오류는 기존 코드 재사용(`INVALID_ARGUMENT` 등, `toToolError` 단일 지점), 저장 본문 반환에 `untrusted: true`, 호출마다 `LedgerAppend`. 신규 오류 코드 없음.

### 3.1 `ctr_record_event`

- 입력 — 필수: `event_type`(≤64B, `[a-z0-9_]+`), `summary`(≤2048B). 선택: `attributes`(JSON 객체, 직렬화 ≤4096B), `artifact_refs`(`[]int64` artifact_id, ≤16개), `related_resources`(`[]string`, ≤16개), `supersedes`(event_id — 기록 시점 존재 검증, 미존재 시 INVALID_ARGUMENT).
- 이벤트 1건 직렬화 총합 ≤8KB — 초과는 INVALID_ARGUMENT. 이벤트는 요약+포인터다: 대용량은 `ctr_index`(inline)로 저장하고 artifact_refs로 가리킨다(도구 description에 명시).
- `artifact_refs` 해석: 기록 시점에 content.db에서 artifact_id→content_hash 조회 → `artifact://sha256-<hash>` URI로 저장. 미존재 id는 INVALID_ARGUMENT. content.db 접근 불가 시 artifact_refs를 동반한 호출만 실패한다.
- redaction: `summary`·직렬화된 `attributes`에 `ingest.Redact` 1회 적용 후 append(§3.0 정본 파이프라인과 동형 — 기록 전 1회가 immutable append 아래 유일하게 건전한 시점). redaction 후 `json.Valid` 재검증 — 실패 시 attributes 전체를 단일 redacted 문자열로 강등. 행에 `redaction: none|spans` 기록.
- 출력: `{event_id, session_id, ts}` — 모델이 supersedes 체이닝에 사용.
- annotation: `DestructiveHint: false`.

### 3.2 `ctr_session_summary`

- 입력: `session_id?`(필터, 미지정 = worktree 전체), `limit?`(타입 그룹당, 기본 5·최대 20), `max_return_bytes?`(기본 8192).
- 출력: `{ checkpoint?, groups: [{event_type, events: [{event_id, session_id, ts, summary, artifact_refs: [{uri, missing?}]}], truncated}], untrusted: true }`.
  - `checkpoint` = 최신 `session_checkpoint` 이벤트 전문.
  - 그룹 내 시간 역순, superseded 이벤트 제외, 그룹별 `truncated` 개별 표기(§4.1 예산 규약 승계).
  - artifact 참조 해석 실패(purge·GC·재구축 이후)는 오류가 아니라 `missing: true` 동반 반환 — **hint 의미론**(D15). "무손실 복원"의 실체: 결정·오류의 의미(summary·payload)는 session.db가 무손실 보유, 대용량 바이트는 retention 계약 내에서만 회수 가능.
- summary 텍스트는 자르지 않는다(이벤트 ≤8KB이므로 전문 반환 가능 — 별도 event-fetch 도구 불필요, YAGNI).

### 3.3 `ctr_export_events`

- 입력: `after?`(event_id 커서), `session_id?`, `limit?`(기본 50·최대 200), `max_return_bytes?`.
- 출력: `{ events: []SessionEventV1, truncated, next_after }` — 원소 각각이 §26 camelCase 객체(eventId/sessionId/eventType/timestamp(RFC3339)/summary/artifactRefs/relatedResources/attributes/privacyLabel/producer). `privacyLabel`은 v0.1 상수 `"internal"`(입력 표면 금지), `producer = {name: "context-router", version: <빌드 버전>}`, `repository`는 미기입(§1.2).
- snake_case 내부 ↔ camelCase export 매핑은 `internal/session`의 wire struct **1곳**에서만(D16).

### 3.4 `ctr_search` 확장

- `scope?: "content" | "events" | "all"`, 기본 `"content"`(후방 호환 — 기존 호출 무영향).
- events 결과 항목: `{event_id, session_id, event_type, ts, summary, superseded}`. RRF 결합 규율은 content 결과와 동일 패턴.
- session.db가 fail-closed 상태면 `events`/`all` scope는 명시적 오류(조용한 빈 결과 금지).

### 3.5 `ctr_fetch` 설명 강화

- 설명 문구에 "저장된 artifact의 byte-exact 조회 — **웹 fetch 아님**(웹은 `ctr_fetch_and_index`)" 명시. 개명은 파괴적이라 보류(session-03 오인 실측 반영).
- 게이트 11(스키마 토큰 예산)은 세션 3종 + scope 확장 + 이 문구를 반영해 재기준화(§9).

## 4. 보안 계약

- redaction 재사용: §3.1대로 기록 경로 1회. span 패턴은 v0.0.1 §5.1과 동일 세트 — 별도 이벤트 전용 패턴 없음. 파일명 denylist는 무관(파일 입력 없음).
- 이벤트 본문은 항상 `untrusted: true`로 반환(저장 본문 반환 공통 규칙).
- 로그·오류 위생(§5.5) 승계: 이벤트 summary/payload를 서버 로그에 에코하지 않는다.

## 5. retention (D17)

- 서버 플래그 `--retention-events <dur>` (기본 `0` = off = 무기한). 설정 시 **서버 시작 시 1회** 스윕: `DELETE FROM session_events WHERE ts < now-<dur>` 트랜잭션 + 삭제 건수 stderr 1줄 고지(조용한 삭제 금지). 실패는 log-and-continue(시작 비차단).
- content 쪽은 현행 유지(수동 purge). session vs content 차등의 근거: session.db는 1차 데이터·공간 비용 미미, content blob이 공간 지배자.
- purge CLI 확장: `context-router purge`에 session 대상 추가(플래그 형태는 구현 계획에서 확정). `.bak-<ts>` 파일과 (향후) spill 파일은 스윕·purge 대상에서 제외 — 명문 계약.
- `auto_vacuum` 미사용: append-위주 로그에서 DELETE로 생긴 free page는 후속 INSERT가 재사용. 필요 시 수동 `VACUUM`은 CLI 소관. D9 PRAGMA 세트 균일성 유지.
- 백그라운드 타이머·주기 스윕 없음(단일 바이너리 단순성). 휴면 프로젝트는 열 때까지 삭제되지 않음 — 무손실 복원과 정합.

## 6. 손상 대응 (D18 — Codex 반영 재설계)

### 6.1 기각된 원안과 이유

원안 "open 시 quick_check 실패 → 배타 잠금 → `.bak-<ts>` rename → 재생성"은 Codex adversarial-review에서 NO-SHIP: (1) 복구 잠금은 복구 시도끼리만 직렬화 — **이미 열린 sibling writer의 fd를 차단하지 못해** rename 후에도 이전 세대에 commit이 계속됨(유일본 분기), 동시 감지 시 이중 복구로 3세대 분기, BUSY/일시 I/O 오류 오분류 시 정상 DB에서도 발생, Windows는 열린 handle로 rename 실패/부분 완료. (2) DB·-wal·-shm 3파일 이동은 비원자 — 중간 crash·사이 commit으로 세대 혼합, 미체크포인트 이벤트 조용히 유실.

### 6.2 채택 프로토콜: fail-closed + lease

- **lifetime lease**: 서버는 session.db open 전에 `projects/<pid>/worktrees/<wid>/session.lock`에 **shared lock**(unix `flock LOCK_SH` / windows `LockFileEx` shared)을 취득하고 프로세스 종료까지 보유한다. 복구 CLI는 **exclusive lock**을 시도하고, 획득 실패(=서버 실행 중) 시 즉시 거부한다. 잠금 획득 후 fresh connection으로 quick_check을 재확인한다.
- **보수적 판정**: quick_check의 명시적 malformed 결과만 손상으로 분류. SQLITE_BUSY·일시 I/O 오류는 재시도 후에도 판정 불가면 "미확정"으로 두고 손상 취급하지 않는다(정상 DB 분기 차단).
- **fail-closed**: 손상 확정 시 — 원본 DB family(-wal/-shm 포함) **일체 불변**, 세션 3종 도구 미등록 + `ctr_search`의 events/all scope 비활성(darwin `ctr_transform` 미등록 선례 패턴 — 호스트가 도구 부재로 즉시 인지), stderr 1줄 경고, content 도구는 정상 서빙 계속.
- 가용성 트레이드오프 수용: 손상 동안 이벤트 기록 정지(신규 이벤트 유실). 유일본의 조용한 분기보다 안전하다는 것이 판정 근거.

### 6.3 수동 복구 CLI

`context-router session recover (--project <id|path>)`:

1. exclusive lease 획득(실패 = 서버 실행 중 → 안내 후 종료).
2. fresh connection quick_check 재확인(오탐이면 무작업 종료).
3. 원본 family를 그대로 둔 채 임시 DB로 행 인양(가능한 무결 행 복사 — 단일 테이블·FK 없음이 이를 단일 패스로 만듦, D15).
4. 인양본 quick_check + 스키마 검증 통과 시에만 **게시**: 원본 family → `session.db.bak-<ts>`(타임스탬프 — 반복 손상에도 덮어쓰기 없음, -wal/-shm 동반), 인양본 → `session.db`. exclusive lease 하이므로 rename이 안전하다.
5. 인양·유실 건수 보고. 실패 시 원본 불변 유지.

### 6.4 v0.2 후보: spill journal

손상 중에도 기록을 지속하려면 프로세스별 append-only spill journal(writer 식별 + sequence, 복구 시 병합)이 필요 — Codex 최소안. v0.1은 구현 복잡도 대비 가치(드문 손상 중 기록 지속)가 낮아 채택하지 않고, fail-closed의 명시성(도구 부재)으로 대체한다.

## 7. CLI 계약 확장 (v0.0.1 §7 승계)

- `context-router session export (--project <id|path>) [--session <id>] [--after <event_id>]` — stdout에 JSONL(행당 §26 객체 1개, UTF-8 no BOM). 파일 산출은 셸 리다이렉션 소관, MCP 도구는 파일을 만들지 않는다.
- `context-router session recover` — §6.3.
- `context-router purge` — session 대상 확장(§5).
- doctor: session.db quick_check·lease 상태 항목 추가.

## 8. 패키지 구조·아키텍처 개정 항목

- **`internal/session` 신설**: Session DB open/스키마/append/조회/FTS + §26 wire struct(저장·export 표현 소유, D16). mcp·cli가 session을 import. 예상 소스 1~2파일(선호 밴드 300~1,000줄 준수).
- store.go는 건드리지 않는다(artifact_id→hash 해석은 기존 store 조회 API 재사용; content.db 마이그레이션 0건).
- 아키텍처 문서(code-architecture) 개정 필요 항목: ① 패키지 그래프에 session 추가(의존 방향: mcp→session, cli→session, session→store 조회만), ② "wire 타입은 mcp 소유" 규약에 D16 예외 명시, ③ 설계서 §번호 매핑 표에 본 문서 절 추가.

## 9. 테스트·수용 게이트 (D20)

**사전 규칙(게이트 문서 서두에 명문화)**: 호스트 의존·비결정 검증은 처음부터 비차단(정보성 + 1회 재시도 기입). 차단 게이트는 스크립트드-결정론 가능한 것만.

v0.1 신규 표면 게이트(초안 — 게이트 문서에서 확정):

| # | 게이트 | 검증 방식(전부 결정론) |
|---|---|---|
| G1 | Session DB 스키마·PRAGMA·user_version·멱등 DDL | 단위 + 재오픈 |
| G2 | 다중 프로세스 동시 append 무손실 | 2-프로세스 스크립트드(순수 INSERT 경합) |
| G3 | 이벤트 3종 계약(golden + 상한 + 오류 매핑) | golden 파일 |
| G4 | redaction 재사용(이벤트 경로 canary — 소스는 분할 리터럴) | search/summary/export 3경로 미회수 |
| G5 | 이벤트 FTS + scope(superseded 플래그, fail-closed 시 명시 오류) | 단위 + 통합 |
| G6 | export 준수(§26 스키마·JSONL·커서 페이지네이션) | golden + round-trip |
| G7 | retention 스윕 결정론 | 시계 주입 |
| G8 | fail-closed + 수동 복구(lease 거부 포함) | 파일 바이트 훼손 + 2-프로세스 lease |
| G9 | 스키마 토큰 예산 재기준화(게이트 11 승계) | `TestSchemaTokenBudget` 신규 기준 |

기존 v0.0.1 게이트: "main CI GREEN = 회귀 없음" 1줄로 승계, 재판정 금지. 교차 모델 리뷰는 현행 프로토콜(체크포인트당 Codex 1패스 상한) 유지.

## 10. 마일스톤 스케치 (상세는 writing-plans)

0. **v0.0.2 worker robustness** (선행 패치, §1.3) → 태그 없이 머지.
1. `internal/session` 저장 계층(스키마·append·lease·session_start).
2. `ctr_record_event` + redaction 경로.
3. `ctr_session_summary` + `ctr_export_events` + wire 매핑.
4. 이벤트 FTS + `ctr_search` scope + `ctr_fetch` 문구 + 게이트 11 재기준화.
5. retention 스윕 + CLI(export/recover/purge/doctor).
6. fail-closed·복구 시나리오 + 게이트 문서 + 세션 기록.

## 11. 의도적 미결 (v0.2 후보)

- spill journal(§6.4), 훅·패시브 수집 채널과 자동 계측 이벤트 9종, `repository{}` 기입, `invalidates` 관계, 이벤트 payload 필드 조회(virtual generated column — 무마이그레이션 탈출구 확보됨), title dedup(독립 태스크).
