# Context Router v0.0.1 설계서 (M0)

> 문서 상태: 설계서 개정판 — 적대 검증 1라운드(Codex adversarial-review + 보안 감사 + 정합성 검증) 반영. 사용자 검토 대상, 승인 후 구현 계획(writing-plans)으로 이행
> 작성일: 2026-07-17 (개정 동일일)
> 계약 기준: 도구 감사 문서 §2.2 인벤토리 (D10 확정)
> 준수: HANDOFF `확정`·`차단`·`폐기`, 결정 D1~D12, 비전 제안서 테제 T1~T4·라우팅 §5.3
> SDK 방침(D11 확정): 현행 go-sdk v1.6.1(spec 2025-11-25)로 구현을 진행한다. 차기 MCP 스펙이 정식 출시되고 go-sdk 안정판(v1.7+)이 나오면 별도 업그레이드 마일스톤으로 반영 — RC 대기로 구현을 지연하지 않는다.

## 0. 결정 이력 요약

| ID | 확정 내용 (2026-07-17) |
|---|---|
| D1 | 단일 `ctr` 등록 + 시작 플래그 프로필, `ctr-global`만 별도 read-only 등록, 운영·파괴는 CLI 전용, 시작 배너 |
| D2 | `ctr_transform` 추가 — starlark 기반 순수 변환, 기본 표면 포함 |
| D3 | exec 노출 v0.2 연기 |
| D4 | 도구 접두사 `ctr_*`, 서버 등록명 `ctr`, 바이너리명 `context-router` |
| D5 | Go 실행 = 외부 toolchain 감지 (v0.2 구현 시) |
| D6 | stats CLI 전용 |
| D7 | 이름 유지 + 네임스페이스 선점 (git 저장소 생성 완료) |
| D8 | go-sdk v1.6.1 + modernc v1.54.0/libc v1.74.1 + starlark-go + x/sys **+ readeck/go-readability + html-to-markdown/v2 (D12 수반, 총 7개 — 아래 v0.17 개정)**; cobra·yaml·ORM·DI 미사용 |
| D9 | 전 DB `WAL + synchronous=NORMAL + busy_timeout=5000 + foreign_keys=ON + user_version` |
| D10 | v0.0.1 계약 재정의: 코어 6 MCP 도구 + CLI 4종, exec 3종 구현째 v0.2 |
| D11 | SDK: 현행 v1.6.1로 진행, 차기 스펙 정식 출시 시 업그레이드 마일스톤 |
| D12 | fetch text/html = readability 본문 추출 → markdown 변환, 구조 보존 판정 실패 시 전체 변환 fail-open, 원문 HTML은 비색인 source blob으로 보존 |

> **v0.17 개정**: "총 7개"는 실측(직접 9)과 어긋난 채 남아 있었다. 검증 전용 TOML 파서
> (pelletier/go-toml/v2)를 더해 직접 10이 된다. 이 항목은 **개수 상한이 아니라 프레임워크
> 금지**가 본절이다 — cobra·yaml·ORM·DI 미사용은 그대로다.

## 1. v0.0.1 제품 계약

### 1.1 범위

- **MCP 도구 6종**: `ctr_search`, `ctr_fetch`, `ctr_transform` (기본) · `ctr_index`, `ctr_fetch_and_index` (옵트인 플래그) · `ctr_global_search` (별도 등록)
- **CLI 4종**: `doctor`, `stats`, `purge`, `upgrade` (모델 호출 불가)
- **저장소**: 공통 루트 + 프로젝트별 Content DB + content-addressed artifact 파일 + 별도 ledger DB (1-S)
- **측정 기반**: local bytes ledger + provider ledger 자리 (§6)

### 1.2 명시적 비범위 (v0.0.1)

| 항목 | 시점 | 근거 |
|---|---|---|
| `ctr_execute`/`ctr_execute_file`/`ctr_batch_execute` (구현 포함) | v0.2 | D3+D10 — 실질 격리 없는 실행 금지 |
| 세션 이벤트·복구 3종, Session DB | v0.1 | D10 — v0.0.1은 Content DB만 |
| 훅/플러그인 패키징 (Shadow Recall, large-read guard) | v0.2 | HANDOFF 확정 (후속 버전) |
| MCP resources (`artifact://`) | 수요 확인 후 | Codex 미지원, Claude Code 템플릿 미지원 — tool 전용 |
| HTTP transport, 팀 공유, 벡터 검색, 웹 UI | 후속 | 설계 기준서 §1.3 유지 |
| 자동 retention 스윕 | v0.1 | v0.0.1은 `purge --older-than` 수동 |

### 1.3 태그 규율

§12 수용 게이트 전체 통과 전 `v0.0.1` 태그 금지. prerelease는 필요할 때만.

## 2. 아키텍처 개요

### 2.1 프로세스·등록 토폴로지 (Q1, Q4)

```text
호스트(Claude Code / Codex CLI)
 ├─ MCP 등록 "ctr"        = context-router [--enable ingest,net] (stdio)
 │    tools/list: ctr_search, ctr_fetch, ctr_transform
 │                (+ ctr_index, ctr_fetch_and_index — 플래그 시)
 │    쓰기: 등록 구조상 호스트당 이 프로세스 1개 (옵션 B의 프로필별 다중 writer 회피)
 │         두 호스트(Claude Code+Codex)를 같은 프로젝트에 동시 사용하면 프로세스 2개가
 │         같은 DB를 연다 — §3.5의 단일 트랜잭션+CAS 계약과 SQLite 파일 잠금으로 안전을
 │         보장하고 §12 게이트의 다중 프로세스 시나리오로 검증한다
 └─ MCP 등록 "ctr-global" = context-router --profile global-search --projects <목록>  (선택 설치)
      tools/list: ctr_global_search (전 DB read-only 연결)

내부 worker: context-router __transform-worker (숨김 모드, §4.3 — ctr_transform 호출별 스폰)
CLI(사용자 전용): context-router doctor | stats | purge | upgrade
```

3층 모델의 성문화: **구현됨**(코드+fixture) / **등록됨**(tools/list — 보안 경계, 시작 플래그로만 변경, 런타임 승격 불가) / **로드됨**(호스트 deferral — 토큰 최적화). 시작 시 stderr 배너 1줄: `[ctr] v0.0.1 profile=search,fetch,transform ingest=off net=off root=<project-root>`.

### 2.2 시작 시퀀스

```text
플래그 파싱 → 루트 결정(--root > cwd) → canonicalize(§3.2) → 저장 루트 확보(§3.1)
→ Content DB open + PRAGMA(§3.5) + user_version 검사(§3.5 — 상위 버전이면 비파괴 거부)
→ 프로필별 도구 등록 → stderr 배너 → stdio 서빙
```

- 실패 정책: DB open 실패 시 quick_check → 복구 절차(§3.5, 배타 잠금 하) → 그래도 실패면 명확한 오류로 종료(반쯤 뜬 서버 금지).
- stdout은 MCP 프로토콜 전용. 로그는 전부 stderr(slog).

### 2.3 기술 기반

- Go 1.25+ (go-sdk 최소 요건), `github.com/modelcontextprotocol/go-sdk` v1.6.1 (spec 2025-11-25), stdio transport.
- 도구 스키마는 struct 태그로 생성, `readOnlyHint` 등 annotation 부착(§4.0). **스키마 토큰 예산 게이트**(§12) — Codex 호스트는 deferral이 없으므로 기본 3종의 정의 크기를 tokenizer 실측으로 관리한다.
- D11: 차기 스펙(stateless core 등)은 디스크 store 기반 설계라 아키텍처 영향 없음 — SDK 안정판 승격 시 별도 마일스톤.

## 3. 저장 계약 (Q5, Q6)

### 3.0 정본 파이프라인 (적대 검증 반영 — 모든 저장 경로의 순서 고정)

```text
raw bytes ──sha256──▶ src_hash
  → (web html이면: readability 추출 → markdown 변환, §4.5)
  → span redaction (청킹 이전, 전체 바이트에 1회 — 경계 분할 비밀 차단)
  → stored bytes ──sha256──▶ content_hash  → artifact blob (임시파일 → fsync → atomic rename)
  → chunking → FTS 색인
좌표 계약: chunks의 byte/line 범위와 ctr_fetch/ctr_search가 반환하는 모든 좌표는
          **stored bytes(저장본) 기준**이다. 원본 좌표 동일성은 redaction=none이고
          변환이 없는 경우에만 성립하며 `source_coords_exact`로 표시한다(§4.0).
```

### 3.1 저장 루트

| OS | 경로 |
|---|---|
| Windows | `%LOCALAPPDATA%\context-router` |
| Linux | `${XDG_DATA_HOME:-~/.local/share}/context-router` |
| macOS | `~/Library/Application Support/context-router` |

override: `--store-root` 플래그 (환경변수 `CTR_STORE_ROOT` 허용). 프로젝트 디렉터리 안에는 아무것도 만들지 않는다.

### 3.2 프로젝트·워크트리 식별 (Q5)

canonicalization 알고리즘 (순서 고정):

```text
1. root := --root 플래그 > 프로세스 cwd
2. abs  := filepath.Abs(root)
3. real := filepath.EvalSymlinks(abs)          # symlink/junction 해석. 실패 시 서버 시작 거부
4. Windows: \\?\ 접두사 제거, 드라이브 문자 대문자 유지, UNC(//host/share) 보존
5. case-fold: Windows·macOS는 소문자화(기본 FS가 대소문자 무시), Linux는 유지
   (macOS case-sensitive 볼륨·Windows per-dir case-sensitivity(fsutil/WSL)에서 실제 대소문자만
    다른 두 디렉터리가 병합될 수 있음 — 문서화된 한계로 수용하고 §12 게이트로 동작을 고정)
6. 구분자 '/' 통일 → canonical string
7. projectRoot 결정: real에서 상향 탐색으로 .git 발견 시
   - .git이 디렉터리 → 그 부모가 projectRoot
   - .git이 파일(worktree/submodule) → gitdir: 경로 파싱 → commondir 파일 추적
     → 주 저장소 .git의 부모가 projectRoot   # git 바이너리 미호출, 파일 파싱만
   - .git 없음 → projectRoot = real
8. worktreeRoot := real  (v0.1 Session DB 배치에 사용; v0.0.1은 기록만)
9. ID := slug(basename(projectRoot))[:32] + "-" + hex(sha256(canonical(projectRoot)))[:12]
```

### 3.3 디렉터리 배치

```text
<store-root>/
└─ projects/<slug-hash>/
   ├─ content.db                     # 메타데이터 + FTS (파생 데이터)
   ├─ ledger.db                      # 측정 원장 — content.db 재구축과 생명주기 분리(§6)
   └─ artifacts/<h[0:2]>/<sha256>    # 저장본 blob + (web) 원문 HTML source blob, 2단 fan-out
```

- 동일 content_hash는 1회만 저장(dedup). artifact 삭제 가능 조건: **sources 참조 0** (chunks는 artifact의 파생물이므로 참조자가 아님).
- orphan GC: `purge --gc`가 artifacts/ 디렉터리와 DB 참조를 대조해 고아 blob을 제거하고, content.db 재구축 직후 자동 실행된다.
- Session DB는 v0.1에서 `projects/<pid>/worktrees/<wid>/session.db`로 추가 예약.

### 3.4 Content DB 스키마 v1 (적대 검증 반영: provenance 정규화 + FTS 동기화 계약)

```sql
PRAGMA user_version = 1;

CREATE TABLE artifacts(                        -- 저장 표현만 (content-addressed)
  id            INTEGER PRIMARY KEY,
  content_hash  TEXT NOT NULL UNIQUE,          -- 저장본(변환·redaction 후) sha256
  media_type    TEXT NOT NULL,
  byte_length   INTEGER NOT NULL,
  redaction     TEXT NOT NULL DEFAULT 'none',  -- none | spans
  created_at    INTEGER NOT NULL
);
CREATE TABLE sources(                          -- provenance·staleness의 유일한 소유자
  uri           TEXT PRIMARY KEY,              -- 파일 절대경로 | (label::)URL | inline:<title>
  artifact_id   INTEGER NOT NULL REFERENCES artifacts(id),
  source_kind   TEXT NOT NULL,                 -- file | web | inline
  src_size      INTEGER,
  src_mtime_ns  INTEGER,
  src_hash      TEXT,                          -- 원본 바이트 sha256 (변환·redaction 전)
  raw_blob_hash TEXT,                          -- web 전용: 보존된 원문 HTML blob 주소(비색인)
  extraction    TEXT,                          -- web 전용: readability | full
  indexed_at    INTEGER NOT NULL
);
CREATE TABLE chunks(
  id          INTEGER PRIMARY KEY,
  artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  ordinal     INTEGER NOT NULL,
  byte_start  INTEGER, byte_end INTEGER,       -- stored bytes 좌표 (§3.0)
  line_start  INTEGER, line_end INTEGER,
  title       TEXT,
  text        TEXT NOT NULL,
  UNIQUE(artifact_id, ordinal)                 -- 다중 프로세스 중복 등록 차단
);
CREATE VIRTUAL TABLE fts_porter  USING fts5(title, text, content='chunks', content_rowid='id',
                                            tokenize='porter unicode61');
CREATE VIRTUAL TABLE fts_trigram USING fts5(title, text, content='chunks', content_rowid='id',
                                            tokenize='trigram');
-- external-content 동기화 계약: chunks의 INSERT/UPDATE/DELETE는 트리거 6종(ai/au/ad × 2 인덱스)으로
-- 두 FTS에 동기 반영한다. 검증: INSERT INTO fts_x(fts_x) VALUES('integrity-check') — §12 게이트.
```

- 검색: porter·trigram 병행 질의 → BM25 → RRF 병합 (참조 구현 동등성).
- chunking v1: 라인 블록 기반 목표 ~4KB, Markdown은 헤딩 우선 분할, 청크 간 1라인 오버랩.
- dedup×provenance: 서로 다른 소스가 같은 저장본으로 수렴하면(redaction·추출 후 동일 바이트) **artifact 1행을 공유하고 sources 행은 각자** 유지 — staleness는 소스 단위로 판정되므로 false-fresh가 없다.
- ledger는 `ledger.db`(동일 PRAGMA): `ledger(id, ts, tool, bytes_stored, bytes_returned, duration_ms)`.

### 3.5 PRAGMA·연결·트랜잭션·복구 (D9 + 적대 검증 반영)

```text
연결 시:  journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000;
          foreign_keys=ON; (global reader는 추가로 query_only=ON)
연결 수:  writer 1 (쓰기 직렬화) + reader 풀 ≤4  — database/sql 두 개의 풀

논리 등록의 원자성 (다중 프로세스 안전의 핵심):
  하나의 ingest = BEGIN IMMEDIATE 트랜잭션 1개로
    artifact upsert → chunks INSERT → FTS 동기 → sources upsert → 커밋
  · sources upsert는 CAS — WHERE uri=? AND (src_hash=구지문 OR 신규) 조건부 갱신으로,
    두 프로세스가 같은 소스의 구/신버전을 엇갈려 커밋해 포인터가 과거로 되돌아가는 것을 차단
  · 재시도는 트랜잭션 전체 단위만 (문장 단위 재시도 금지 — 부분 반영 방지),
    SQLITE_BUSY 3회 지수 백오프 후 STORAGE_UNAVAILABLE
  · artifact blob은 임시파일 → fsync → atomic rename, DB 커밋 이전에 배치

user_version 검사:
  0(신규) → 스키마 생성 · =지원버전 → 정상 · <지원버전 → 마이그레이션
  >지원버전 → **비파괴 거부** (오류 종료 + 안내. 삭제·재생성 금지 — 상위 버전 데이터 보호)

종료 시:  PRAGMA wal_checkpoint(TRUNCATE) 시도(실패 무해)
손상 시:  quick_check 실패 → content.db는 파생 데이터: `<content.db>.rebuild.lock` 배타 파일 잠금
          하에서만 교체·재생성(두 프로세스의 복구 경합 차단), artifacts/·ledger.db는 보존,
          재구축 직후 orphan GC. (v0.1) Session DB는 .bak 보존 후 재생성
튜닝 유보: cache_size, mmap_size, wal_autocheckpoint = 기본값 (측정 후 결정)
```

### 3.6 staleness

소스 단위로 판정한다: 조회 시 `sources.src_size/src_mtime_ns` 비교(불일치 시 원본 재해시 → `sources.src_hash` 대조)로 `stale` 플래그를 계산해 **모든 반환에 포함**한다. stale 항목은 결과에서 제외하지 않는다(회수 판단은 호출자 몫) — 단 명시 표기. `content_hash`(저장본 주소)는 원본 대조에 사용하지 않는다.

## 4. MCP 도구 계약 (Q1)

### 4.0 공통 규칙

- **annotation**: `ctr_search`/`ctr_fetch`/`ctr_transform`/`ctr_global_search` = `readOnlyHint:true`. `ctr_index`/`ctr_fetch_and_index` = readOnly 아님(저장소 기록), `destructiveHint:false`.
- **untrusted 표시 + 구조적 펜싱**: 검색·회수 결과의 저장 콘텐츠 본문은 명시적 경계 필드(`content`)에 격리하고 `untrusted: true`를 동반한다. 이는 모델 자문 라벨이며 강제 경계가 아님을 설계가 인정한다 — 실효 방어는 호스트 승인 규칙(§9 어댑터 스니펫에 ingest/net/global ask를 **기본 포함**)과 net 프로필 기본 OFF가 담당한다.
- **좌표 의미론(§3.0)**: 모든 line/byte 좌표는 저장본 기준. `source_coords_exact: bool` — redaction=none이고 변환 없는 소스(file/inline 원문)일 때만 true. false면 원본 파일 편집 좌표로 사용 금지를 뜻한다.
- **budget**: `ctr_search`/`ctr_fetch`는 선택 파라미터 `max_return_bytes` — 초과분은 잘라내고 `truncated:true`+생략 요약 반환.
- **오류 코드**: `INVALID_ARGUMENT · NOT_FOUND · WORKSPACE_VIOLATION · UNSUPPORTED_FILE · BUDGET_EXCEEDED · OUTPUT_LIMIT_EXCEEDED · NETWORK_DENIED · STORAGE_UNAVAILABLE · INTERNAL`. 오류 메시지에 절대경로·env·비밀값 미포함.
- 모든 호출은 ledger.db에 (bytes_stored, bytes_returned, duration) 기록.

### 4.1 `ctr_search`

```text
입력: queries: []string (1~8) · limit?: int(기본 3/query, ≤10) · max_return_bytes?: int(기본 8192)
출력: results[query별]: { hits[{ artifact_id, source(project-relative), line_start, line_end,
       snippet(~500B, 매치 중심 창), score, stale, redacted, source_coords_exact }],
       truncated: bool }  · untrusted: true
예산 배분: max_return_bytes를 쿼리 수로 균등 분할, 미사용분은 후순위 쿼리로 이월,
          쿼리별 truncated를 개별 표기 — "예산 소진"과 "매치 없음"을 구분 가능하게
동작: porter+trigram 병행 → BM25 → RRF 병합 → 중복 조각 제거 → 예산 내 절단
제한: 현재 프로젝트 DB 고정 — project/global 인수 자체가 스키마에 없음
```

### 4.2 `ctr_fetch`

```text
입력: artifact_id + selector 정확히 1개: (chunk_id | line_start&line_end | byte_start&byte_end)
      — 0개·2개 이상 지정, artifact 비소속 chunk_id → INVALID_ARGUMENT
      · max_return_bytes?: int(기본 16384, ≤65536)
반환 범위: byte 범위는 UTF-8 문자 경계로 스냅하고 **실제 반환 범위**(byte_start/end, line_start/end)를
      함께 회신 — 요청 범위와 다를 수 있음
출력: content(저장본 그대로, redacted span 제외) · exact_scope: "artifact" · representation:
      file|markdown|inline · source_coords_exact · provenance{ content_hash, src_hash, source,
      source_kind, extraction?, created_at, stale, redaction } · untrusted: true
보증: 반환 바이트는 저장본과 동일(redacted span 제외). 변환(markdown)·redaction된 artifact에
      대해 원본 바이트 동일성은 주장하지 않는다 — representation과 source_coords_exact가 그 경계다.
```

### 4.3 `ctr_transform` (Q2) — worker 프로세스 실행 (적대 검증 반영)

```text
입력: script: string(starlark) · inputs: [artifact_id] (≤8, 총합 ≤16MB, 개당 ≤8MB)
      · args?: map<string,string> · max_output_bytes?: int(기본 32768, ≤262144)
실행 모델: 서버 in-process 금지. 호출마다 자기 바이너리를 `__transform-worker` 숨김 모드로 재실행:
  · OS hard 메모리 상한 — Windows: Job Object(JOB_OBJECT_LIMIT_PROCESS_MEMORY),
    Linux: RLIMIT_AS, macOS: RLIMIT_AS + RSS watchdog(best-effort). 기본 상한 256MB
  · timeout 10s 초과·상한 위반 시 프로세스 트리 강제 종료(KILL_ON_JOB_CLOSE / kill(-pgid))
  · 상한을 걸 수 없는 환경에서는 transform을 오류로 비활성 (in-process fallback 금지)
  · worker는 지정된 artifact blob 경로만 read-only로 열고, stdin {script,args,caps} → stdout 결과 JSON
언어 환경: starlark-go — 파일·네트워크·env·프로세스·시계·난수 미노출(모듈 자체 부재),
  스텝 상한 5M(보조 제한 — 메모리의 주 방어선은 OS 상한), deterministic
predeclared: inputs[i].text() / .lines() / .json(), args, emit(x)
  · emit은 **증분 검사** — 누적 출력이 max_output_bytes 초과 시 즉시 OUTPUT_LIMIT_EXCEEDED
내장 함수(고정 연산): regex_extract, json_project, line_window, head, tail,
                      count, sort, dedupe, group_count
출력: result(text) · steps_used · truncated?
결정론: 동일 입력·스크립트 → 동일 출력 (golden test 가능)
```

### 4.4 `ctr_index` (Q7 일부)

```text
입력: path: string (파일|디렉터리) | content: string + title: string (inline)
      · include?/exclude?: []glob · max_file_bytes?: int(기본 5MB)
경로 정책 (판정 알고리즘 명시):
  · 허용 판정 = canonicalize(대상) 후 filepath.Rel(root, 대상)이 ".." 세그먼트/절대경로를
    포함하지 않을 것 — 문자열 접두사 매칭 금지(/proj vs /proj-evil 오탐 차단)
  · 디렉터리 워크 중 **파일마다** EvalSymlinks 재검증(내부→외부 심링크/junction 차단),
    Linux는 가능 시 openat2(RESOLVE_BENEATH) 사용
  · 허용 root = projectRoot + --allow-path 목록(시작 시 canonicalize, store-root 하위는 거부
    — 자기 재귀 색인 방지). 위반 → WORKSPACE_VIOLATION
스킵 규칙: 바이너리 sniff, max_file_bytes 초과, secret 파일명 denylist(§5.1)
출력: indexed: n · skipped: [{path(project-relative), reason}] · bytes_stored
```

### 4.5 `ctr_fetch_and_index` (Q8, D12 확정)

```text
입력: url: string · max_bytes?: int(기본 10MB)
정책: §5.2 SSRF 계약 전체 적용.
text/html 파이프라인(D12):
  raw HTML(≤max_bytes, src_hash 계산, **비색인 source blob으로 보존** — 재처리·감사용)
  → 본문 추출: readeck/go-readability (go-shiori 원본은 deprecated — 활성 포크, 코드 보존 수정 이력)
  → 충실도 판정(추출 "성공"≠충실 — 적대 검증 반영): 추출 전후 pre/code/table 노드 수와
    가시 텍스트 보존율을 **DOM 비교**로 판정. fail-open 조건(OR):
    추출 오류·빈 결과 / 텍스트 <500자 / 가시 텍스트 보존율 <30% / pre·code 보존율 <50%
    → 전체 DOM 변환으로 전환, extraction:"full" 기록
  → markdown 변환: JohannesKaufmann/html-to-markdown v2(+table 플러그인)
  → 이후 §3.0 정본 파이프라인(redaction → 저장 → 청킹 → 색인)
text/*·application/json·application/xml: 원문 그대로 §3.0 파이프라인
출력: artifact_id · title · byte_length · extraction · indexed_chunks · 첫 스니펫(≤1KB)
```

배경(D12): 참조 구현은 Turndown으로 **페이지 전체**를 markdown화해 boilerplate(네비·푸터·배너)가 저장소·FTS를 오염시켰다 — retrieval 병목 경고(감사 문서 §4.2)와 직결. 2차 추출기 후보: go-trafilatura(기사 정밀도 우수, 코드 보존은 readability 방식이 안전) — 게이트 실패 시 재평가.

### 4.6 `ctr_global_search` (Q4)

```text
등록: 별도 서버명 ctr-global, 시작 플래그 --projects <경로|ID 목록> (기본값 없음 — 미지정 시 시작 거부)
입력: ctr_search와 동일 + 반환에 project 라벨 추가
연결: 각 프로젝트 content.db를 read-only + query_only=ON 으로 open (--projects 경로도 canonicalize)
금지: 이 등록에는 다른 어떤 도구도 없음. §9 어댑터 스니펫에 per-call ask 규칙 기본 포함
```

## 5. 보안 계약

### 5.1 수집 보안 (Q7)

- **redaction 순서 (§3.0)**: 청킹 **이전**, 전체 artifact 바이트에 1회 적용 — 청크 경계에 걸친 비밀(`-----BEGIN PRIVATE KEY-----` 블록 등)의 분할 유출을 차단. 청크·FTS·blob 어디에도 원문 비밀이 남지 않는다.
- **파일명 denylist** (전체 스킵, v0.0.1 고정 목록): `.env*`, `*.pem`, `*.key`, `id_rsa*`, `id_ed25519*`, `*.pfx`, `*.p12`, `*.kdbx`, `credentials*`, `*.tfstate`, `.netrc`, `.npmrc`, **`*.har`, `*.jks`, `*.p8`, `kubeconfig*`, docker `config.json`(경로 패턴 `**/.docker/config.json`)**. `*.log`는 제외 — 로그 분석이 제품 핵심 사용처이므로 스팬 패턴으로 방어한다.
- **span redaction 패턴**: AWS `AKIA…`, GitHub `ghp_/gho_/ghs_…`, Slack `xox?-`, `-----BEGIN … PRIVATE KEY-----` 블록, `password=`/`pwd=` 값, JDBC/ADO 연결 문자열 자격 부분, JWT 3-세그먼트, **`Authorization:`/`Proxy-Authorization:`(Basic/Bearer 값), `Cookie:`/`Set-Cookie:` 값, docker `"auth": "<base64>"`**. 스캔은 raw 뷰 + JSON-unescape 뷰 2회(이스케이프 은닉 차단). 매치 시 `«REDACTED:<type>»` 치환 + `artifacts.redaction='spans'` — canary 게이트 §12.
- **경로 정책**: §4.4의 판정 알고리즘. retention: v0.0.1은 자동 스윕 없음, `purge --older-than` 수동(§7).

### 5.2 네트워크 수집 — SSRF 불변식 (Q8, 적대 검증 반영)

```text
I1. URL host가 literal IP면 netip 파싱 → Addr.Unmap()(v4-mapped 정규화) → IPv6 zone 있으면 거부
I2. hostname이면 전체 A/AAAA 조회 → 모든 레코드를 I1과 동일하게 정규화 후 판정
I3. 판정은 열거 denylist가 아니라 특수용도 전체 기준:
    차단 = !IsGlobalUnicast() ∪ IsLoopback ∪ IsPrivate ∪ IsLinkLocal(Unicast|Multicast)
         ∪ 100.64.0.0/10(CGNAT) ∪ 64:ff9b::/96(NAT64) ∪ 0.0.0.0/8 ∪ 예약·멀티캐스트
    (--net-allow-local 시작 플래그일 때만 127.0.0.1 예외)
I4. dial은 검증에서 캡처한 IP:port로만 — custom DialContext, hostname 재조회 경로 자체가 없음
I5. TLS는 ServerName=정규화 hostname으로 정상 인증서 검증 유지 (IP dial + SNI/verify 분리)
I6. http.Client: 자동 redirect 비활성(수동 순회 최대 5, 매 hop I1~I5 전체 재적용),
    Proxy=nil(환경 프록시 무시), fallback resolver 금지
I7. scheme http/https만, port 80/443(+--net-ports), https→http 강등 hop 거부
I8. 크기 max_bytes 스트리밍 강제(초과 중단), timeout 30s, 쿠키·자격 헤더 미전송, UA: context-router/<ver>
```

### 5.3 transform 격리 (Q2)

주 방어선 = worker 프로세스의 **OS hard 메모리·CPU 상한**(§4.3). starlark의 무 I/O 환경과 스텝 상한은 보조 방어선이다(스텝 상한은 `s+s` 배증·거대 리스트 같은 소수 연산 대량 할당을 막지 못함이 검증됨). 실행 결과는 ledger 외 영구 저장하지 않음(자동 색인 금지 — HANDOFF §8.3 승계).

### 5.4 global 분리 (Q4)

allowlist는 시작 플래그로만(canonicalize 포함). 연결은 `query_only=ON`(쓰기 SQL 자체 거부). execute류 부재. 반환에 project 라벨 필수. 어댑터 스니펫이 per-call ask를 기본 포함(§9).

### 5.5 로그·오류 위생

slog(stderr): `ts, level, tool, duration_ms, bytes_in, bytes_out, error_code`만. 원문·쿼리 본문·절대경로·env 미기록. `--log-level debug`에서도 원문 미기록(바이트 수만).

## 6. 측정 계약 (Q9)

- **local ledger** (v0.0.1): `ledger.db` — content.db 재구축과 생명주기 분리(측정 연속성). `stats` CLI가 도구별 bytes_stored/returned·호출수·기간 출력. **토큰·달러 환산 출력 금지** — 표기는 "bytes suppressed (local, 진단용)".
- **provider ledger** (v0.0.1 기반 → v0.2 완성): `stats --provider <transcript-path>` — Claude Code transcript JSONL usage 필드 집계 최소 구현. OTel·Codex usage 어댑터는 v0.2. 실측 토큰만 표시, 절약 주장 없음.
- **인과 절약 주장**: v0.2 무작위 A/B(task/session 단위) 하네스 전까지 어떤 채널에서도 절약률 주장 금지 (차단 항목 준수).
- **append-only 원칙**: ctr가 호스트 대화 이력을 재작성하는 기능을 만들지 않는다(cache 보존).

## 7. CLI 계약 (Q11)

```text
context-router                      # MCP 서버 (기본, 플래그: §8)
context-router doctor               # 저장 루트/DB/FTS5/프로젝트 식별 진단 + 호스트 등록 스니펫
                                    #   (스니펫에 ingest/net/global ask 권한 규칙 기본 포함)
context-router stats [--provider <transcript-path>]   # §6
context-router purge (--project <id|path> | --all) [--older-than <dur>] [--gc]
                                    # 확인: TTY 필수 + 대상 프로젝트 slug를 직접 입력(정적 'yes' 금지)
                                    # 비TTY·자동화는 --force 명시 시에만. --gc = orphan blob 정리
context-router upgrade              # 최소형: 현재 버전 + 최신 릴리스 버전 확인 + 수동 설치 안내
                                    #   릴리스 host는 컴파일타임 상수 — 응답 제공 URL·명령은 출력하지
                                    #   않는다(버전 문자열만 취함). 네트워크 실패·타임아웃(10s) 시
                                    #   현재 버전만 출력하고 정상 종료
```

## 8. 서버 플래그 (전체)

```text
--root <path>            # 프로젝트 루트 (기본: cwd)
--store-root <path>      # 저장 루트 override (env CTR_STORE_ROOT)
--profile <list>         # 기본 search,fetch,transform | global-search
--enable <list>          # ingest, net
--allow-path <path>...   # ctr_index 허용 경로 추가 (반복 가능, canonicalize + store-root 거부)
--projects <list>        # global-search 전용 allowlist (필수)
--net-allow-local        # fetch_and_index 127.0.0.1 허용
--net-ports <list>       # 추가 허용 포트
--log-level <lvl>        # info 기본
```

설정 파일 없음(D8). 플래그가 5개를 넘게 늘어나는 시점에 재검토.

## 9. 호스트 어댑터 (Q10)

문서 + `doctor` 출력으로 제공 (코드 어댑터는 v0.2 훅 패키징에서):

- **Claude Code** (`.mcp.json` 또는 `claude mcp add`): stdio, tool search 기본 ON. doctor 스니펫에 permissions 규칙 기본 포함: `ctr_index`/`ctr_fetch_and_index` = ask, `ctr-global` 서버 도구 = ask.
- **Codex** (`~/.codex/config.toml`): deferral 없음 → 기본 3-도구 프로필 권장. `enabled_tools` 절단 예시 포함, ingest/net 활성 시 `default_tools_approval_mode` prompt 권장.
- 차이표(제공 문서에 포함): deferral 유무, 승인 모델, resources 미지원 공통.

**v0.1 추가(session events, 델타 문서 `context-router-design-v0.1-ko.md` §1.1)**: `ctr_record_event`·
`ctr_session_summary`·`ctr_export_events` 세션 3종은 `ctr_index`/`ctr_fetch_and_index`와
달리 워크스페이스 파일 읽기가 없고 쓰기는 store-root(session.db) 한정이라 **기본 등록**
(opt-in `Enable` 불요)이고, 위 permissions 예시의 `ask` 목록에도 추가하지 않는다 — 기존
서술(3-도구 기본 프로필·ingest/net opt-in ask 규칙)은 그대로 유지된다. `doctor` 출력
스니펫(`internal/cli/cli.go` `hostSnippet`)이 실제 등록 도구 집합의 정본이므로, 세션
3종을 포함한 최신 형태는 항상 `doctor` 실행 결과를 그대로 복사해 쓴다(이 문서에 스니펫
사본을 중복 관리하지 않는다는 기존 원칙 승계).

## 10. 라이선스·고지 (Q12)

- `LICENSE` = Elastic License 2.0 (HANDOFF 확정 운영 원칙 — fork/port 관계를 숨기지 않는 보수적 선택).
- `NOTICE` = upstream ctxscribe / context-mode(참조 oracle) 저작권·ELv2 고지 + "modified/independent Go implementation" 명시.
- `README` = "independent Go implementation informed by the ctxscribe tool contract; not affiliated" 문구 + 도구 이름 매핑표(`ctx_*`→`ctr_*`).
- 배포 전 라이선스 재검토 필요(법률 자문 아님) — HANDOFF §48 승계.

## 11. 패키지 구조 (Go)

```text
context-router/
├─ cmd/context-router/main.go   # 플래그, 조립(직접 배선 — DI 없음), CLI 분기, __transform-worker 모드
├─ internal/mcp/                # 서버, 도구 정의/등록, 프로필
├─ internal/store/              # sqlite open/PRAGMA/스키마/트랜잭션 계약, artifact blob IO
├─ internal/search/             # FTS 질의, BM25+RRF, 스니펫 창
├─ internal/transform/          # starlark 엔진, 내장 함수, worker 프로토콜·OS 상한
├─ internal/ingest/             # §3.0 파이프라인, secret 필터, 청킹, 경로 정책
├─ internal/netfetch/           # SSRF 불변식 fetch + readability + html→md
├─ internal/ident/              # canonicalization (§3.2)
├─ internal/cli/                # doctor/stats/purge/upgrade
├─ testdata/                    # golden fixtures, canary, hostile paths, html corpus
├─ go.mod                       # D8 의존성 (개수는 §0 v0.17 개정)
└─ docs/
```

인터페이스 추상화는 두지 않는다(검색 백엔드·저장소 인터페이스 없음 — `database/sql`이 교체점, HANDOFF 확정).

## 12. 테스트·수용 게이트 (v0.0.1 exit)

1. **golden fixtures**: `ctr_search`/`ctr_index`는 oracle(ctxscribe 1.3.0) 동작과 이름·필드 매핑 문서 하에 동등성 검증. `ctr_fetch`/`ctr_transform`/**`ctr_fetch_and_index`**(D12로 oracle과 의도적으로 다른 파이프라인 — H3 해소)는 자체 golden(HTML fixture → 기대 markdown 포함).
2. **retrieval 평가 하네스**: 자체 fixture 코퍼스 recall@k 측정 스크립트 + 기준선 기록.
3. **경로 시험**: symlink/junction/case-fold(**실제 대소문자만 다른 두 디렉터리** — Windows per-dir case-sensitivity 포함)/UNC/git worktree common-dir/`..` 탈출/워크 중 심링크/**--allow-path 경계·store-root 거부** — 3 OS.
4. **secret canary**: denylist 파일 미색인 + span redaction(청킹 전 적용 — **청크 경계에 걸친 PRIVATE KEY 블록** fixture 포함) 후 search/fetch 양 경로에서 canary 미회수. JSON-escape·헤더 토큰 변형 포함.
5. **SSRF matrix**: I1~I8 전체 — 사설/link-local/메타데이터/**NAT64·CGNAT·0.0.0.0/8·v4-mapped·IPv6 zone**/rebinding/리터럴 IP redirect/https→http 강등/proxy env 무시/크기 초과.
6. **FTS 무결성·동등성**: `INSERT INTO fts(fts) VALUES('integrity-check')` 재색인·purge 후 통과, porter/trigram/BM25+RRF 스모크 + 5,000 doc 성능 스모크.
7. **DB 동시성·내구성**: writer 1+reader 4 + **프로세스 2개 동시 쓰기**(양 호스트) — 동일 소스 구/신버전 교차 커밋(CAS 검증), **동일 content_hash·상이 src_hash** dedup fixture, 쓰기 중 kill 후 무결성/배타 잠금 재구축, user_version 상위 버전 비파괴 거부 — 3 OS.
8. **transform 상한**: 스텝/출력 초과 + **거대 문자열 concat·리스트 증식이 worker 메모리 상한으로 종료**되고 서버 프로세스는 생존, timeout 시 트리 전멸.
9. **추출 충실도**: 대표 기술 문서 HTML corpus에서 pre/code/table 보존 판정과 fail-open 전환이 기준대로 동작 (D12 게이트).
10. **프로토콜 위생**: stdout 오염 0, Claude Code·Codex 실 등록 스모크(tools/list·호출·cancellation).
11. **스키마 토큰 예산**: 기본 3종 도구 정의를 tokenizer 실측 — 목표 상한 기록·관리(Codex 무deferral 부담).
12. **빌드**: `CGO_ENABLED=0` 6타깃 크로스빌드 + 크기 기록; memory-capped CI(전역 test-guard 규율).
13. 전 게이트 통과 전 태그 금지.

## 13. 구현 마일스톤 스케치

M1 스켈레톤(main·플래그·ident·store open·배너·stdio) → M2 ingest 파이프라인(§3.0)+search → M3 fetch+staleness → M4 transform worker → M5 netfetch(SSRF+D12) → M6 global+CLI 4종 → M7 fixtures·게이트·호스트 스모크 → freeze. 상세 작업 분해는 writing-plans에서.

## 14. 의도적 미결 (후속 설계 부록)

- v0.2: exec 3종 상세 계약(OS별 셸, Job Object/landlock/sandbox-exec 조합, 출력 ephemeral 정책), Shadow Recall 훅 설계, OTel·Codex usage 어댑터, 무작위 A/B 하네스.
- v0.1: Session DB 스키마, 이벤트 3종 계약, SessionEvent v1 export(설계 기준서 §26 승계), retention 자동화.
- 검색 semantic 보강(retrieval 병목 대응) — recall@k 기준선 측정 후 판단. 2차 추출기(go-trafilatura) — 게이트 9 실패 시.

## 15. HANDOFF 12질문 최종 매핑

| Q | 답 위치 | 상태 |
|---|---|---|
| Q1 계약 vs 노출 | §1, §2.1, §4 | 확정 |
| Q2 execute 대체 | §4.3, §5.3 (+v0.2 예약 §14) | 확정 |
| Q3 Go/셸 실행 계약 | D5 기록, 상세는 §14 v0.2 부록 | 원칙 확정·상세 연기 |
| Q4 current/global | §4.6, §5.4 | 확정 |
| Q5 프로젝트/워크트리 ID | §3.2 | 확정 |
| Q6 DB 계약 | §3.0, §3.3~3.6 | 확정 |
| Q7 수집·secret·retention·stale | §5.1, §3.6 (패시브 인덱싱은 v0.2) | v0.0.1 범위 확정 |
| Q8 SSRF | §5.2 | 확정 |
| Q9 stats 분리 | §6 | 확정 |
| Q10 어댑터 | §9 | v0.0.1 범위 확정 |
| Q11 upgrade/purge CLI | §7 | 확정 |
| Q12 ELv2 고지 | §10 | 확정 (배포 전 재검토 1회) |

## 16. 적대 검증 1라운드 처리 기록 (2026-07-17)

3중 검증(Codex adversarial-review "needs-attention" 6건 + 보안 감사 8건 + 정합성 검증 13건)에서 수렴 지적 2건(provenance 정규화, transform 메모리)을 포함해 유효 지적을 전부 반영했다. 주요 반영: §3.0 정본 파이프라인, §3.4 sources 분리+FTS 트리거 계약, §3.5 단일 트랜잭션+CAS+배타 잠금 복구+user_version 비파괴 거부, §4.2 exact_scope/좌표 의미론, §4.3 worker 프로세스, §5.1 redaction 순서·denylist/패턴 확장, §5.2 불변식 8종, §4.5 충실도 판정+원문 보존+라이브러리 교체(go-shiori deprecated→readeck 포크), §6 ledger 분리, §7 purge/upgrade 강화, §12 게이트 13항 확장. 기각: `*.log` denylist 편입(핵심 사용처 — 스팬 패턴으로 대체), transform v0.0.1 제외(Codex 대안 — worker 격리로 해소).
