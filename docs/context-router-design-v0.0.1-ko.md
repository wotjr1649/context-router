# Context Router v0.0.1 설계서 (M0)

> 문서 상태: 설계서 초안 — 사용자 검토 대상, 승인 후 구현 계획(writing-plans)으로 이행
> 작성일: 2026-07-17
> 계약 기준: 도구 감사 문서 §2.2 인벤토리 (D10 확정)
> 준수: HANDOFF `확정`·`차단`·`폐기`, 결정 D1~D10, 비전 제안서 테제 T1~T4·라우팅 §5.3
> freeze 조건: MCP 스펙 2026-07-28 RC 확인 후 최종 확정 (그 전까지 "초안")

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
| D8 | go-sdk v1.6.1 + modernc v1.54.0/libc v1.74.1 + starlark-go + x/sys; cobra·yaml·ORM·DI 미사용 |
| D9 | 전 DB `WAL + synchronous=NORMAL + busy_timeout=5000 + foreign_keys=ON + user_version` |
| D10 | v0.0.1 계약 재정의: 코어 6 MCP 도구 + CLI 4종, exec 3종 구현째 v0.2 |

## 1. v0.0.1 제품 계약

### 1.1 범위

- **MCP 도구 6종**: `ctr_search`, `ctr_fetch`, `ctr_transform` (기본) · `ctr_index`, `ctr_fetch_and_index` (옵트인 플래그) · `ctr_global_search` (별도 등록)
- **CLI 4종**: `doctor`, `stats`, `purge`, `upgrade` (모델 호출 불가)
- **저장소**: 공통 루트 + 프로젝트별 Content DB + content-addressed artifact 파일 (1-S)
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
 │    쓰기: 프로젝트당 이 프로세스 1개만 (WAL 단일 writer)
 └─ MCP 등록 "ctr-global" = context-router --profile global-search --projects <목록>  (선택 설치)
      tools/list: ctr_global_search (전 DB read-only 연결)

CLI(사용자 전용): context-router doctor | stats | purge | upgrade
```

3층 모델의 성문화: **구현됨**(코드+fixture) / **등록됨**(tools/list — 보안 경계, 시작 플래그로만 변경, 런타임 승격 불가) / **로드됨**(호스트 deferral — 토큰 최적화). 시작 시 stderr 배너 1줄: `[ctr] v0.0.1 profile=search,fetch,transform ingest=off net=off root=<project-root>`.

### 2.2 시작 시퀀스

```text
플래그 파싱 → 루트 결정(--root > cwd) → canonicalize(§3.2) → 저장 루트 확보(§3.1)
→ Content DB open + PRAGMA(§3.5) + user_version 마이그레이션 체크
→ 프로필별 도구 등록 → stderr 배너 → stdio 서빙
```

- 실패 정책: DB open 실패 시 quick_check → 복구 절차(§3.5) → 그래도 실패면 명확한 오류로 종료(반쯤 뜬 서버 금지).
- stdout은 MCP 프로토콜 전용. 로그는 전부 stderr(slog).

### 2.3 기술 기반

- Go 1.25+ (go-sdk 최소 요건), `github.com/modelcontextprotocol/go-sdk` v1.6.1 (spec 2025-11-25), stdio transport.
- 도구 스키마는 struct 태그로 생성. `readOnlyHint` 등 annotation 부착(§4.0).
- 2026-07-28 RC(stateless core 등) 확인 후: 세션 상태 무의존 설계(이미 디스크 기반)라 영향은 SDK 업그레이드 경로로 한정 — v1.7 안정화 시 추종.

## 3. 저장 계약 (Q5, Q6)

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
   (macOS case-sensitive 볼륨의 병합 가능성은 문서화된 한계로 수용)
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
   ├─ content.db                     # 메타데이터 + FTS
   └─ artifacts/<h[0:2]>/<sha256>    # 원문, content-addressed, 2단 fan-out
```

- 동일 content_hash는 1회만 저장(dedup). artifact 삭제는 참조 카운트(chunks/sources) 0일 때만.
- Session DB는 v0.1에서 `projects/<pid>/worktrees/<wid>/session.db`로 추가 예약.

### 3.4 Content DB 스키마 v1

```sql
PRAGMA user_version = 1;

CREATE TABLE artifacts(
  id            INTEGER PRIMARY KEY,
  content_hash  TEXT NOT NULL UNIQUE,        -- sha256 hex
  media_type    TEXT NOT NULL,
  byte_length   INTEGER NOT NULL,
  source_kind   TEXT NOT NULL,               -- file | web | inline
  source_uri    TEXT,                        -- 파일 절대경로 또는 URL (반환 시엔 project-relative 표시)
  src_size      INTEGER,                     -- file staleness probe
  src_mtime_ns  INTEGER,
  redaction     TEXT NOT NULL DEFAULT 'none',-- none | spans
  created_at    INTEGER NOT NULL
);
CREATE TABLE sources(                        -- source당 현재 artifact 포인터
  uri         TEXT PRIMARY KEY,
  artifact_id INTEGER NOT NULL REFERENCES artifacts(id),
  indexed_at  INTEGER NOT NULL
);
CREATE TABLE chunks(
  id          INTEGER PRIMARY KEY,
  artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  ordinal     INTEGER NOT NULL,
  byte_start  INTEGER, byte_end INTEGER,
  line_start  INTEGER, line_end INTEGER,
  title       TEXT,
  text        TEXT NOT NULL
);
CREATE VIRTUAL TABLE fts_porter  USING fts5(title, text, content='chunks', content_rowid='id',
                                            tokenize='porter unicode61');
CREATE VIRTUAL TABLE fts_trigram USING fts5(title, text, content='chunks', content_rowid='id',
                                            tokenize='trigram');
CREATE TABLE ledger(                         -- local bytes ledger (§6)
  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
  bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0
);
```

- 검색: porter·trigram 두 인덱스 병행 질의 → BM25 → RRF 병합 (참조 구현 동등성).
- chunking v1: 라인 블록 기반 목표 ~4KB, Markdown은 헤딩 우선 분할, 청크 간 1라인 오버랩.
- 같은 source 재색인: 새 artifact 행 + `sources.uri` 포인터 교체, 구 artifact는 참조 0이면 purge 대상.

### 3.5 PRAGMA·연결·복구 (D9 성문화)

```text
연결 시:  journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000;
          foreign_keys=ON; (global reader는 추가로 query_only=ON)
연결 수:  writer 1 (쓰기 직렬화) + reader 풀 ≤4  — database/sql 두 개의 풀
재시도:   SQLITE_BUSY 시 bounded retry 3회(지수 백오프), 이후 STORAGE_UNAVAILABLE
종료 시:  PRAGMA wal_checkpoint(TRUNCATE) 시도(실패 무해)
손상 시:  open 실패 또는 integrity 오류 → PRAGMA quick_check
          → Content DB는 파생 데이터: 파일 교체(삭제) 후 빈 스키마 재생성, stderr 경고
            (artifacts/ 파일은 보존 — 재색인으로 복원 가능)
          → (v0.1) Session DB는 .bak 보존 후 재생성
튜닝 유보: cache_size, mmap_size, wal_autocheckpoint = 기본값 (측정 후 결정)
```

### 3.6 staleness

file-backed artifact는 조회 시 `src_size/src_mtime_ns` 비교(불일치 시 hash 재확인)로 `stale` 플래그를 계산해 **모든 반환에 포함**한다. stale 항목은 결과에서 제외하지 않는다(회수 판단은 호출자 몫) — 단 명시 표기.

## 4. MCP 도구 계약 (Q1)

### 4.0 공통 규칙

- **annotation**: `ctr_search`/`ctr_fetch`/`ctr_transform`/`ctr_global_search` = `readOnlyHint:true`. `ctr_index`/`ctr_fetch_and_index` = readOnly 아님(저장소 기록), `destructiveHint:false`.
- **untrusted 표시**: 검색·회수 결과 본문은 `untrusted: true` 필드와 함께 반환 — 저장 원문 내 지시문은 데이터다.
- **budget**: `ctr_search`/`ctr_fetch`는 선택 파라미터 `max_return_bytes`(기본 §4.x별) — 예산 초과분은 잘라내고 `truncated:true`+생략 요약을 반환.
- **오류 코드**: `INVALID_ARGUMENT · NOT_FOUND · WORKSPACE_VIOLATION · UNSUPPORTED_FILE · BUDGET_EXCEEDED · OUTPUT_LIMIT_EXCEEDED · NETWORK_DENIED · STORAGE_UNAVAILABLE · INTERNAL`. 오류 메시지에 절대경로·env·비밀값 미포함.
- 모든 호출은 ledger에 (bytes_stored, bytes_returned, duration) 기록.

### 4.1 `ctr_search`

```text
입력: queries: []string (1~8) · limit?: int(기본 3/query, ≤10) · max_return_bytes?: int(기본 8192)
출력: results[query별]: hits[{ artifact_id, source(project-relative), line_start, line_end,
       snippet(~500B, 매치 중심 창), score, stale, redacted }], untrusted: true, truncated?
동작: porter+trigram 병행 → BM25 → RRF 병합 → 중복 조각 제거 → 예산 내 절단
제한: 현재 프로젝트 DB 고정 — project/global 인수 자체가 스키마에 없음
```

### 4.2 `ctr_fetch`

```text
입력: artifact_id + (chunk_id | line_start&line_end | byte_start&byte_end)
      · max_return_bytes?: int(기본 16384, ≤65536)
출력: text(byte-exact, redacted span 제외) · provenance{ content_hash, source, source_kind,
      created_at, stale, redaction } · exact: true · untrusted: true
오류: 범위 초과 → INVALID_ARGUMENT(유효 범위 안내), artifact 없음 → NOT_FOUND
```

byte-exact 보증: redaction span(§5.1)을 제외한 모든 바이트는 저장 시점 원문과 동일. 보증을 문서·스키마 설명에 명기.

### 4.3 `ctr_transform` (Q2)

```text
입력: script: string(starlark) · inputs: [artifact_id] (≤8) · args?: map<string,string>
      · max_output_bytes?: int(기본 32768, ≤262144)
실행: starlark-go — 파일·네트워크·env·프로세스·시계·난수 미노출(모듈 자체 부재)
      스텝 상한(기본 5M) · 입력당 읽기 상한 8MB · 출력 상한 · context timeout 10s
predeclared: inputs[i].text() / .lines() / .json(), args, emit(x)  (emit 누적이 결과)
내장 함수(고정 연산): regex_extract, json_project, line_window, head, tail,
                      count, sort, dedupe, group_count
출력: result(text) · steps_used · truncated?
결정론: 동일 입력·스크립트 → 동일 출력 (golden test 가능)
```

### 4.4 `ctr_index` (Q7 일부)

```text
입력: path: string (파일|디렉터리) | content: string + title: string (inline)
      · include?/exclude?: []glob · max_file_bytes?: int(기본 5MB)
경로 정책: projectRoot 하위만 허용. 밖 → WORKSPACE_VIOLATION.
          추가 허용은 서버 시작 플래그 --allow-path 로만 (런타임 확장 불가)
스킵 규칙: 바이너리 sniff, max_file_bytes 초과, secret 파일명 denylist(§5.1)
출력: indexed: n · skipped: [{path(project-relative), reason}] · bytes_stored
```

### 4.5 `ctr_fetch_and_index` (Q8)

```text
입력: url: string · max_bytes?: int(기본 10MB)
정책: §5.2 SSRF 계약 전체 적용. text/html은 markdown 변환 후 저장(원문 HTML은 미보존),
      text/*·application/json·application/xml은 원문 저장
출력: artifact_id · title · byte_length · indexed_chunks · 첫 스니펫(≤1KB)
```

### 4.6 `ctr_global_search` (Q4)

```text
등록: 별도 서버명 ctr-global, 시작 플래그 --projects <경로|ID 목록> (기본값 없음 — 미지정 시 시작 거부)
입력: ctr_search와 동일 + 반환에 project 라벨 추가
연결: 각 프로젝트 content.db를 read-only + query_only=ON 으로 open
금지: 이 등록에는 다른 어떤 도구도 없음. 호스트 설정에서 per-call ask 권장(어댑터 문서 §9)
```

## 5. 보안 계약

### 5.1 수집 보안 (Q7)

- **파일명 denylist** (전체 스킵): `.env*`, `*.pem`, `*.key`, `id_rsa*`, `id_ed25519*`, `*.pfx`, `*.p12`, `*.kdbx`, `credentials*`, `*.tfstate`, `.netrc`, `.npmrc`(auth 포함 가능) — v0.0.1에서는 고정 목록(축소 불가), 사용자 확장은 후속 버전.
- **span redaction** (텍스트 내 패턴): AWS `AKIA…`, GitHub `ghp_/gho_/ghs_…`, Slack `xox?-`, `-----BEGIN … PRIVATE KEY-----` 블록, `password=`/`pwd=` 값, JDBC/ADO 연결 문자열의 자격 부분, JWT 3-세그먼트 패턴. 매치 시 **chunk 텍스트와 artifact 파일 모두** `«REDACTED:<type>»`로 치환하고 `artifacts.redaction='spans'` 기록 — 검색·fetch 어느 경로로도 원문 비밀 미회수 (canary 게이트 §12).
- **경로 정책**: §4.4 — projectRoot 밖 금지, symlink 해석 후 검증(TOCTOU 최소화: open 후 fstat 검증).
- retention: v0.0.1은 자동 스윕 없음, `purge --older-than <dur>` 수동(§7).

### 5.2 네트워크 수집 — SSRF 계약 (Q8)

```text
scheme: http/https만 · port: 80/443(+ --net-ports 로 추가)
해석: DNS 조회 → 모든 A/AAAA 검증 → 검증된 IP로 직접 dial(Host 헤더 유지)  # rebinding 차단
차단 대역: 127/8, 10/8, 172.16/12, 192.168/16, 169.254/16(메타데이터 포함),
           0.0.0.0, ::1, fc00::/7, fe80::/10, 멀티캐스트/예약
redirect: 최대 5회, 매 hop 재검증 · 크기: max_bytes 스트리밍 강제(초과 시 중단)
timeout: 30s · 쿠키/자격 헤더 미전송 · UA: context-router/<ver>
로컬 문서 서버 예외: --net-allow-local 시작 플래그(기본 off)일 때만 127.0.0.1 허용
```

### 5.3 transform 격리 (Q2)

starlark 환경에 I/O 계열 모듈이 존재하지 않음(ambient authority 0) + 스텝·메모리·출력 상한 + timeout. 실행 결과는 ledger 외 영구 저장하지 않음(자동 색인 금지 — HANDOFF §8.3 승계).

### 5.4 global 분리 (Q4)

allowlist는 시작 플래그로만. 연결은 `query_only=ON`(쓰기 SQL 자체 거부). execute류 부재. 반환에 project 라벨 필수.

### 5.5 로그·오류 위생

slog(stderr): `ts, level, tool, duration_ms, bytes_in, bytes_out, error_code`만. 원문·쿼리 본문·절대경로·env 미기록. `--log-level debug`에서도 원문 미기록(바이트 수만).

## 6. 측정 계약 (Q9)

- **local ledger** (v0.0.1): §3.4 ledger 테이블. `stats` CLI가 도구별 bytes_stored/returned·호출수·기간 출력. **토큰·달러 환산 출력 금지** — 표기는 "bytes suppressed (local, 진단용)".
- **provider ledger** (v0.0.1 기반 → v0.2 완성): `stats --provider` 서브모드 자리. 데이터 소스 어댑터: ① Claude Code transcript JSONL usage 필드, ② Claude Code OTel 메트릭(수집 활성 시), ③ Codex usage 로그. v0.0.1은 어댑터 인터페이스 없이 ①만 최소 구현(파일 경로 인자로 받아 집계) — 절약 "주장"은 여기서도 하지 않고 실측 토큰만 표시.
- **인과 절약 주장**: v0.2 무작위 A/B(task/session 단위) 하네스 전까지 어떤 채널에서도 절약률 주장 금지 (차단 항목 준수). 추가 지표(recall@k, trajectory 길이, false-deny율 등)는 v0.2 하네스 요건으로 예약.
- **append-only 원칙**: ctr가 호스트 대화 이력을 재작성하는 기능을 만들지 않는다(cache 보존).

## 7. CLI 계약 (Q11)

```text
context-router                      # MCP 서버 (기본, 플래그: §8)
context-router doctor               # 저장 루트/DB/FTS5/프로젝트 식별 진단 + 호스트 등록 스니펫 출력
context-router stats [--provider <transcript-path>]   # §6
context-router purge (--project <id|path> | --all) [--older-than <dur>]
                                    # 2단 확인: 대상 요약 표시 → 'yes' 입력 → 삭제. --all은 프로젝트 목록 전체 표시 후 재확인
context-router upgrade              # v0.0.1 최소형: 현재 버전 + 최신 릴리스 확인 + 수동 설치 안내 출력 (자기 교체 없음)
```

doctor가 등록 스니펫(§9)을 출력하므로 별도 print-config 서브커맨드는 두지 않는다.

## 8. 서버 플래그 (전체)

```text
--root <path>            # 프로젝트 루트 (기본: cwd)
--store-root <path>      # 저장 루트 override (env CTR_STORE_ROOT)
--profile <list>         # 기본 search,fetch,transform | global-search
--enable <list>          # ingest, net
--allow-path <path>...   # ctr_index 허용 경로 추가 (반복 가능)
--projects <list>        # global-search 전용 allowlist (필수)
--net-allow-local        # fetch_and_index 127.0.0.1 허용
--net-ports <list>       # 추가 허용 포트
--log-level <lvl>        # info 기본
```

설정 파일 없음(D8). 플래그가 5개를 넘게 늘어나는 시점에 재검토.

## 9. 호스트 어댑터 (Q10)

문서 + `doctor` 출력으로 제공 (코드 어댑터는 v0.2 훅 패키징에서):

- **Claude Code** (`.mcp.json` 또는 `claude mcp add`): stdio, tool search 기본 ON이라 6-도구도 부담 없음. 권장 permissions: `ctr_index`/`ctr_fetch_and_index`는 ask, `ctr-global` 서버는 per-call ask.
- **Codex** (`~/.codex/config.toml`): deferral 없음 → 기본 3-도구 프로필 권장. `enabled_tools`로 추가 절단 가능, `default_tools_approval_mode`는 ingest/net 활성 시 prompt 권장.
- 차이표(제공 문서에 포함): deferral 유무, 승인 모델, resources 미지원 공통.

## 10. 라이선스·고지 (Q12)

- `LICENSE` = Elastic License 2.0 (HANDOFF 확정 운영 원칙 — fork/port 관계를 숨기지 않는 보수적 선택).
- `NOTICE` = upstream ctxscribe / context-mode(참조 oracle) 저작권·ELv2 고지 + "modified/independent Go implementation" 명시.
- `README` = "independent Go implementation informed by the ctxscribe tool contract; not affiliated" 문구 + 도구 이름 매핑표(`ctx_*`→`ctr_*`).
- 배포 전 라이선스 재검토 필요(법률 자문 아님) — HANDOFF §48 승계.

## 11. 패키지 구조 (Go)

```text
context-router/
├─ cmd/context-router/main.go   # 플래그, 조립(직접 배선 — DI 없음), CLI 분기
├─ internal/mcp/                # 서버, 도구 정의/등록, 프로필
├─ internal/store/              # sqlite open/PRAGMA/스키마/마이그레이션, artifact 파일 IO
├─ internal/search/             # FTS 질의, BM25+RRF, 스니펫 창
├─ internal/transform/          # starlark 엔진, 내장 함수, 상한
├─ internal/ingest/             # 청킹, secret 필터, 경로 정책
├─ internal/netfetch/           # SSRF 가드 fetch + html→md
├─ internal/ident/              # canonicalization (§3.2)
├─ internal/cli/                # doctor/stats/purge/upgrade
├─ testdata/                    # golden fixtures, canary, hostile paths
├─ go.mod                       # D8 의존성 5개
└─ docs/
```

인터페이스 추상화는 두지 않는다(검색 백엔드·저장소 인터페이스 없음 — `database/sql`이 교체점, HANDOFF 확정).

## 12. 테스트·수용 게이트 (v0.0.1 exit)

1. **golden fixtures**: `ctr_search`/`ctr_index`/`ctr_fetch_and_index`는 oracle(ctxscribe 1.3.0) 동작과 이름·필드 매핑 문서 하에 동등성 검증; `ctr_fetch`/`ctr_transform`은 자체 golden.
2. **retrieval 평가 하네스**: 자체 fixture 코퍼스에 recall@k 측정 스크립트 + 기준선 기록 (외부 증거 §4.2 교훈 — 게이트는 하네스 존재+기준선, 목표치는 측정 후 고정).
3. **경로 시험**: symlink/junction/case-fold/UNC/git worktree common-dir/`..` 탈출 — 3 OS.
4. **secret canary**: denylist 파일 미색인 + span redaction 후 search/fetch 양 경로에서 canary 미회수.
5. **SSRF matrix**: 사설/link-local/메타데이터/rebinding/redirect/크기 초과 차단.
6. **FTS 동등성**: porter/trigram/BM25+RRF 스모크 + 5,000 doc 성능 스모크.
7. **DB 내구성**: writer 1+reader 4 동시성, 쓰기 중 kill 후 무결성/재구축 경로 — 3 OS.
8. **transform 상한**: 스텝/메모리/출력/timeout 초과 시 오류 코드 + 프로세스 생존.
9. **프로토콜 위생**: stdout 오염 0, Claude Code·Codex 실 등록 스모크(tools/list·호출·cancellation).
10. **빌드**: `CGO_ENABLED=0` 6타깃 크로스빌드 + 크기 기록; memory-capped CI(전역 test-guard 규율).
11. 전 게이트 통과 전 태그 금지.

## 13. 구현 마일스톤 스케치

M1 스켈레톤(main·플래그·ident·store open·배너·stdio) → M2 ingest+search → M3 fetch+staleness → M4 transform → M5 netfetch(SSRF) → M6 global+CLI 4종 → M7 fixtures·게이트·호스트 스모크 → RC 확인 → freeze. 상세 작업 분해는 writing-plans에서.

## 14. 의도적 미결 (후속 설계 부록)

- v0.2: exec 3종 상세 계약(OS별 셸, Job Object/landlock/sandbox-exec 조합, 출력 ephemeral 정책), Shadow Recall 훅 설계, OTel 어댑터, 무작위 A/B 하네스.
- v0.1: Session DB 스키마, 이벤트 3종 계약, SessionEvent v1 export(설계 기준서 §26 승계), retention 자동화.
- 검색 semantic 보강(§4.2 retrieval 병목 대응) — recall@k 기준선 측정 후 판단.

## 15. HANDOFF 12질문 최종 매핑

| Q | 답 위치 | 상태 |
|---|---|---|
| Q1 계약 vs 노출 | §1, §2.1, §4 | 확정 |
| Q2 execute 대체 | §4.3, §5.3 (+v0.2 예약 §14) | 확정 |
| Q3 Go/셸 실행 계약 | D5 기록, 상세는 §14 v0.2 부록 | 원칙 확정·상세 연기 |
| Q4 current/global | §4.6, §5.4 | 확정 |
| Q5 프로젝트/워크트리 ID | §3.2 | 확정 |
| Q6 DB 계약 | §3.3~3.6 | 확정 |
| Q7 수집·secret·retention·stale | §5.1, §3.6 (패시브 인덱싱은 v0.2) | v0.0.1 범위 확정 |
| Q8 SSRF | §5.2 | 확정 |
| Q9 stats 분리 | §6 | 확정 |
| Q10 어댑터 | §9 | v0.0.1 범위 확정 |
| Q11 upgrade/purge CLI | §7 | 확정 |
| Q12 ELv2 고지 | §10 | 확정 (배포 전 재검토 1회) |
