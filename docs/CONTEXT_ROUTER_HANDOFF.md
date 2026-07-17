# Context Router 설계 인수인계 기준선

> 상태: 설계 재작성용 handoff / decision-and-evidence baseline  
> 기준 시각: 2026-07-17, Asia/Seoul  
> 새 작업공간: `C:\Users\js\Documents\AI_DEV\context-router`  
> 참조 구현: `C:\Users\js\Documents\ClaudeCode\context-mode`  
> 이전 Codex 작업: `019f5f7f-b7f5-75c2-8e33-d989e43c14f9`  
> 현재 Codex 작업: `019f5fb8-9128-7cf3-a107-e286ace3cd77`

이 문서는 새 설계서가 아니다. 지금까지의 사용자 결정, 코드·웹 검증 결과, 폐기된 중간안, 보안 차단 사유와 미결정 사항을 Claude에게 넘기기 위한 기준선이다. 새 설계는 이 문서의 `확정`과 `차단`을 출발점으로 다시 작성하되, `폐기` 항목을 되살리지 않아야 한다.

## 1. 상태 표기

| 표기 | 의미 |
|---|---|
| `확정` | 사용자가 명시적으로 승인한 기준선 |
| `검증` | 코드, 테스트 또는 공식 자료로 확인한 사실 |
| `차단` | 현재 형태로 설계에 포함하면 안 되는 항목 |
| `보류` | 새 설계에서 결정해야 하는 항목 |
| `폐기` | 이후 결정으로 대체된 과거 중간안 |

## 2. 한 페이지 요약

- `context-router`는 기존 Node 제품을 참조해 만드는 **Node 비의존 단일 Go 바이너리**다. `context-mode` 디렉터리는 참조/oracle이고 새 구현 작업공간은 현재 디렉터리다. `확정`
- Graphify는 다른 워크스페이스에서 별도로 개발한다. 이 저장소 범위에 포함하지 않는다. `확정`
- 첫 배포선은 SemVer `v0.0.1`이며, 구현을 단계적으로 하더라도 합의된 전체 범위를 끝내기 전에는 태그하지 않는다. `확정`
- `v0.0.1` 계약 목록은 `ctx_insight`를 제외한 10개 `ctx_*` 기능이다. 다만 **10개를 모두 일반 MCP 호출 표면에 노출한다는 뜻은 아니다**. 보안 검수 이후 노출 프로필은 다시 설계해야 한다. `확정 + 보류`
- 실행 언어는 `v0.0.1`에서 Go와 셸만 대상으로 하고 JavaScript/TypeScript는 제외한다. `확정`
- Windows, Linux, macOS를 지원한다. amd64/arm64 6종 배포는 기존 제안이며 실제 지원 매트릭스와 CI는 새 설계에서 확정한다. `확정 + 보류`
- 저장 구조의 기준선은 **공통 저장 루트 + 프로젝트별 Content DB + worktree/세션별 Session DB + 명시적 전역 검색**이다. `확정`
- 이 구조는 우발적 프로젝트 혼선을 막는 논리적 격리다. 같은 OS 사용자 권한의 악성 프로세스를 막는 보안 경계는 아니다. `검증`
- `ctx_search + ctx_execute`만 상시 로드하는 것은 합리적이며 참조 구현이 이미 그렇게 한다. 두 도구만 실제 호출 가능하게 만드는 hard exposure는 권장하지 않는다. `검증`
- 현재 `ctx_execute`는 OS sandbox가 아니라 동일 사용자 권한의 범용 코드 실행기다. 1-S 코어에 그대로 포함할 수 없다. `차단`
- context 절약 메커니즘은 큰 원문을 로컬에서 처리하고 작은 결과만 반환할 때 활성 context window를 실제로 줄인다. 전체 API/청구 토큰과 비용 절감은 아직 입증되지 않았다. `검증`
- 참조 구현의 `ctx_stats` 절약률과 달러 환산은 provider usage가 아니라 바이트 대리치이므로 회계·마케팅 근거로 사용할 수 없다. `차단`
- SQLite는 `modernc.org/sqlite v1.53.0`과 `modernc.org/libc v1.73.4`를 직접 고정한다. `ncruces/go-sqlite3`는 Windows WAL 수정 릴리스 이후 재평가한다. `확정`

## 3. 제품·구현 방향 결정 기록

| 항목 | 상태 | 결정 |
|---|---|---|
| 구현 언어 | 확정 | 서버, DB, CLI, 설치 대상 코어를 Go로 작성 |
| Node 의존성 | 확정 | 제품 코어에서 완전히 제거 |
| 배포 형태 | 확정 | 운영체제별 단일 실행 파일 |
| 버전 정책 | 확정 | SemVer, 첫 목표 `v0.0.1`; 필요할 때만 prerelease 사용 |
| 운영체제 | 확정 | Windows, Linux, macOS |
| 저장소 범위 | 확정 | 이 작업공간은 `context-router` 전용; Graphify 제외 |
| MCP 전송 | 확정 | `v0.0.1`은 직접 등록형 stdio MCP 코어 우선 |
| 플러그인·훅 패키징 | 확정 | Claude Code/Codex 플러그인과 훅 설치 UX는 후속 버전 |
| 실행 언어 | 확정 | Go·셸 우선; JS/TS 제외 |
| 참조 데이터 | 확정 | 기존 `context-mode` DB 마이그레이션 없이 새 저장소 시작 |
| 구현 방식 | 확정 | 공개 도구 계약/golden fixture 기반 Go 재구현; 파일별 기계적 포팅 금지 |
| 참조 구현 | 확정 | 기존 Node fork는 계약 확인용 oracle이며 런타임·빌드 의존성 아님 |
| 최소화 원칙 | 확정 | Ponytail full: speculative abstraction, driver interface, DI framework 금지 |
| 라이선스 관계 | 확정된 운영 원칙 | 실제 fork/port 관계를 숨기지 않고 ELv2·원저작권·수정 고지 유지. 법률 자문은 아님 |

### 단일 바이너리의 아직 모호한 부분

`단일 Go 바이너리`는 context-router 자체가 Node 런타임을 요구하지 않는다는 뜻으로 확정됐다. 다만 `ctx_execute(language=go)`가 외부 Go toolchain을 호출할지, 제한된 내장 실행기를 사용할지는 결정되지 않았다. 셸도 Windows와 Unix에서 실행 계약이 다르다. 이 두 항목은 새 설계에서 명시해야 한다.

현재 작업 환경에는 다음 Go가 설치되어 있다.

```text
go version go1.26.5 windows/amd64
```

## 4. v0.0.1 기능 계약 인벤토리

과거 결정은 `ctx_insight`를 제외한 10개 기능을 `v0.0.1`에서 제공하는 것이다. 보안 검수 결과, 기능 존재와 MCP 노출은 분리해야 한다.

| 기능 | 기존 역할 | 주요 비용·위험 | 새 설계 권장 위치 |
|---|---|---|---|
| `ctx_search` | 프로젝트/세션 FTS 검색 | 데이터 노출, prompt injection, recall 실패 | 현재 프로젝트 read-only 기본 표면 |
| `ctx_execute` | 임의 코드·셸 실행 후 축약 반환 | RCE, 파일·환경·네트워크·자격증명, 지속 프로세스 | 기본 제외; 격리 실행 프로필 또는 `ctx_transform`으로 대체 |
| `ctx_execute_file` | 파일을 로컬 처리하고 결과만 반환 | 경로 이탈, 파일 노출, subprocess | 제한된 파일 입력 프로필 |
| `ctx_batch_execute` | 복수 명령 실행·자동 색인 | RCE 확대, DB 팽창, 상태 경합 | 특권 프로필; 기본 제외 |
| `ctx_index` | 파일·디렉터리·inline 콘텐츠 색인 | 영구 저장, 비밀정보·stale data | 명시적 ingestion 프로필 |
| `ctx_fetch_and_index` | HTTP fetch, 변환, cache, 색인 | SSRF, network egress, 오염된 원문 | 별도 network ingestion 프로필 |
| `ctx_stats` | context 절약 통계 | 잘못된 비용·절약 주장 | 진단 전용; provider usage 기반으로 재설계 |
| `ctx_doctor` | 설치·runtime·FTS 진단 | 비교적 낮음 | CLI 우선 |
| `ctx_upgrade` | 업그레이드 명령·절차 | 네트워크, overwrite, 공급망 | CLI + 명시 승인 |
| `ctx_purge` | 세션/프로젝트 데이터 삭제 | 비가역 데이터 손실 | CLI 또는 별도 관리 표면 + 이중 확인 |

`ctx_insight`는 upstream 별도 호스팅 대시보드 launcher이며 독립 단일 바이너리 원칙과 맞지 않아 제외했다. 참조 구현 1.3.0의 활성 도구 목록도 위 10개다.

## 5. 폐기되거나 대체된 중간안

| 과거 안 | 상태 | 대체 결정 |
|---|---|---|
| `v0.0.1`을 `ctx_execute`, `ctx_execute_file`만으로 출시 | 폐기 | 내부 구현 순서일 뿐, 10개 계약을 완료하기 전 태그하지 않음 |
| 모든 프로젝트가 하나의 전역 DB 사용 | 폐기 | 공통 root 아래 프로젝트별 Content DB와 worktree별 Session DB |
| `ncruces/go-sqlite3`를 즉시 기본 드라이버로 채택 | 폐기 | modernc 고정, ncruces 수정 릴리스 후 재평가 |
| driver interface/build tag로 두 SQLite 구현 동시 지원 | 폐기 | `database/sql` 교체점만 사용, v0.0.1에 제품 추상화 추가 금지 |
| 기존 DB 탐색·마이그레이션·이름 호환 | 폐기 | 새 저장소로 시작 |
| `ctx_search + ctx_execute`만 노출하면 자동으로 안전 | 폐기 | execute가 나머지 권한을 코드로 재현하므로 보안 경계가 되지 않음 |
| `ctx_execute`를 sandbox라고 간주 | 폐기 | 현재 구현은 동일 사용자 subprocess; OS 격리 없음 |
| `ctx_stats`의 90%대 절약률을 실제 API 비용으로 간주 | 폐기 | server-side bytes suppressed 추정치로만 취급 |
| prompt caching이 context window를 비운다고 간주 | 폐기 | caching은 비용/지연 최적화이며 window 점유 제거와 다름 |

## 6. 저장소 구조 1-S

### 6.1 확정된 논리 구조

실제 디렉터리명과 project ID 알고리즘은 미정이지만, 논리 구조는 다음과 같다.

```text
shared user-owned root
├─ project A
│  └─ Content DB (FTS5, indexed sources)
├─ project B
│  └─ Content DB
└─ worktree/session stores
   ├─ worktree A1 Session DB
   └─ worktree A2 Session DB
```

- 기본 검색은 현재 프로젝트 DB만 조회한다.
- 전역 검색은 사용자가 명시적으로 선택한 별도 capability여야 한다.
- Content와 Session의 생명주기, purge 범위, writer 장애 범위를 분리한다.
- 한 DB의 잠금·손상·삭제가 모든 프로젝트로 전파되는 단일 전역 DB를 피한다.
- 신규 구현이므로 과거 DB 탐색과 자동 migration은 만들지 않는다.

### 6.2 다른 Claude/Codex/워크스페이스 접근

같은 OS 사용자로 실행되고 동일한 공통 root와 MCP 등록을 사용하는 Claude/Codex는 다른 작업공간에서도 저장소에 접근할 수 있다. 이것은 의도한 cross-workspace recall 기능이다. 단, 다음을 구분해야 한다.

- **제품 수준 접근 제어:** 기본 current-project search와 명시적 global search를 분리한다.
- **OS 수준 보안:** 같은 사용자의 임의 프로세스는 DB 파일을 직접 읽을 수 있다. 1-S만으로 악성 same-user process를 막을 수 없다.
- 다른 프로젝트/에이전트를 적대적 tenant로 취급하려면 별도 OS identity, ACL, AppContainer/컨테이너 또는 별도 암호화 키가 필요하다.

현재 참조 구현의 `ctx_search`는 `project` 인수에 생략/current, `global`, 절대 경로를 받는다. 모델이 `project: "global"`을 자체 선택할 수 있으므로 이것만으로는 “사용자의 명시적 전역 검색”이 아니다. 새 설계에서는 `ctx_global_search` 또는 별도 MCP 등록/프로필처럼 승인 가능한 capability로 분리해야 한다.

## 7. 참조 구현 1.3.0 상태

2026-07-17 재확인 결과:

```text
repository: C:\Users\js\Documents\ClaudeCode\context-mode
branch: main
HEAD: 54842bd 1.3.0
worktree: clean
package: ctxscribe 1.3.0
active ctx tools: 10
```

### 7.1 도구 로딩

- `src/server.ts`는 `ALWAYS_LOAD_TOOLS = {ctx_execute, ctx_search}`로 설정한다.
- Claude Tool Search를 지원하는 호스트에서는 나머지 8개가 deferred된다.
- Codex는 공식 `enabled_tools`, `disabled_tools`, server/per-tool approval 설정으로 실제 allowlist를 구성할 수 있다.
- 현재 `.codex-plugin/mcp.json`의 `default_tools_approval_mode`는 여전히 `approve`다. `ctx_execute`와 함께 사용하면 안 된다.

현재 활성 tool JSON을 `bytes/4`로 단순 근사한 고정 비용은 다음과 같다. 실제 tokenizer 비용은 아니다.

| 도구 | token-equivalent 근사 |
|---|---:|
| `ctx_batch_execute` | 1,039 |
| `ctx_fetch_and_index` | 903 |
| `ctx_execute_file` | 775 |
| `ctx_search` | 762 |
| `ctx_index` | 752 |
| `ctx_execute` | 743 |
| `ctx_purge` | 592 |
| `ctx_upgrade` | 127 |
| `ctx_doctor` | 108 |
| `ctx_stats` | 105 |
| 합계 | 약 5,906 |
| `ctx_search + ctx_execute` | 약 1,505 |

### 7.2 ADR-0008 R1/R2/R3 진행 상태

이전 검수 당시 roadmap이던 일부 항목은 1.1.0~1.3.0에서 구현됐다.

- **R1 passive Read indexing:** full-file Read만 수동 요청 없이 색인한다. Bash 결과는 비밀정보를 장기 저장할 위험 때문에 제외하며, windowed Read도 제외한다.
- **R1 duplicate Read guard:** 같은 세션에서 성공적으로 색인됐고 DB identity, file size, mtime, SHA-256이 동일한 byte-identical full reread만 차단한다. 48시간 expiry와 kill switch가 있고 애매하면 fail-open한다.
- **R2 subagent rule:** 작은 탐색 작업의 고정 토큰 비용을 피하도록 spawn 기준을 문서화했다.
- **R3 large-read guard:** 1 MiB를 초과하는 full Read를 차단하고 작은 windowed read recipe를 반환한다. subagent와 windowed Read는 제외하며 애매하면 fail-open한다.

R1/R3은 parent window 보호를 강화하지만, provider-level 총 토큰 절감을 자동으로 증명하지 않는다.

## 8. 보안 검수 결과

### 8.1 판정

`ctx_search + ctx_execute`를 1-S 코어의 유일한 두 도구로 노출하는 현재 안은 **Block**이다. 문제는 도구 개수가 아니라 `ctx_execute` 하나가 가진 범용 권한이다.

### 8.2 주요 finding

#### Critical — 동일 사용자 RCE와 1-S 우회

- 참조 구현은 임시 파일을 만들지만 실제 자식 프로세스는 프로젝트 또는 호출자 지정 `cwd`에서 일반 `spawn`으로 실행된다.
- 파일시스템, 공통 storage root, 다른 프로젝트 DB, 사용자 home, 네트워크에 OS 사용자 권한으로 접근할 수 있다.
- `ctx_index`, `ctx_fetch_and_index`, `ctx_purge`를 숨겨도 동일하거나 더 위험한 행위를 코드로 재현할 수 있다.

#### Critical — 환경·자격증명 노출

- runtime injection 변수 일부만 denylist로 제거하고 부모 환경 대부분을 복사한다.
- 참조 테스트는 SSH agent socket과 일반 token/API-key 계열 환경변수 전달을 의도적으로 보장한다.
- 검색 결과나 저장소 문서의 prompt injection이 execute로 이어지면 파일·환경·네트워크를 통한 유출 경로가 생긴다.

#### Critical — Codex 기본 자동 승인

- 현재 plugin 설정은 server default가 `approve`다.
- Codex의 `prompt`/per-tool approval과 Claude의 exact-tool `ask`는 최소 조건이지만 OS 격리를 대체하지 않는다.

#### High — 잘못된 sandbox 설명

- 현재 설명은 sandboxed subprocess처럼 보이지만 임시 스크립트 디렉터리와 보안 sandbox는 다르다.
- 쓰기 결과는 실제 프로젝트/파일시스템에 남을 수 있다.

#### High — 실행 코드·출력의 영구 저장

- 큰 stdout/stderr와 실행 provenance가 FTS에 자동 저장될 수 있다.
- session event redaction과 동일한 secret filtering이 모든 execute indexing 경로에 보장되지 않는다.
- 전역 검색과 결합하면 한 프로젝트의 민감 출력과 간접 prompt injection이 다른 프로젝트에서 회수될 수 있다.

#### Medium — 자원 고갈

- 기본 timeout 부재 경로, 큰 출력 상한, CPU/memory/disk/network quota 부재, background process가 공격 표면이다.

### 8.3 새 설계의 최소 보안 계약

#### 기본 검색 프로필

- 현재 프로젝트 `ctx_search`만 제공
- Content DB read-only
- `project/global` 인수 미노출
- source/project/session provenance와 stale 여부 반환
- 검색 결과는 untrusted data로 표시

#### 전역 검색 프로필

- 별도 `ctx_global_search` 또는 별도 MCP server
- 매 호출 사용자 승인
- 프로젝트 allowlist
- execute 기능 미포함
- 결과에서 후속 실행으로 넘어갈 때 별도 승인

#### 실행 프로필

- 기본 비활성화
- 임의 `cwd`와 `background` 제거
- 환경변수 allowlist; HOME, agent socket, credential 변수 미전달
- project read-only, temp-only write 또는 명시적 input handle만 허용
- network 기본 차단
- timeout, process-tree kill, CPU/memory/disk/output 제한
- execute 코드·출력의 자동 영구 색인 금지 또는 session-ephemeral 저장
- Codex `prompt`, Claude exact-tool `ask`

가장 단순한 장기안은 범용 `ctx_execute`를 MCP 코어에서 제외하고, 파일·환경·네트워크·process 접근이 없는 `ctx_transform`을 제공하는 것이다. host의 native sandbox 도구를 쓰는 경우에도 MCP 자식 프로세스가 host shell sandbox에 자동 포함된다고 가정하면 안 된다.

### 8.4 필수 보안 테스트

- credential 계열 환경변수와 SSH agent socket 비가시성
- 절대경로, `..`, symlink/junction, UNC를 통한 다른 프로젝트/common root 접근 차단
- 프로젝트 write 비지속, temp-only write
- public, localhost, 사설망, metadata endpoint 네트워크 차단
- timeout 뒤 전체 process tree 종료, background 생존 불가
- 실행 출력 canary가 영구 검색에서 회수되지 않음
- 기본 tools/list에 global search와 execute가 없음
- 전역 server에 execute가 없음
- Codex 실행은 항상 prompt, Claude exact-tool ask 적용
- 검색 결과 prompt injection이 execute로 자동 연결되지 않음

## 9. 실제 context 절약 분석

### 9.1 네 개의 ledger

1. **로컬 처리 바이트:** 프로세스와 SQLite가 읽고 쓴 양
2. **활성 context window:** 다음 모델 호출에 들어가는 schema, tool arguments/results, 대화 이력
3. **전체 API 토큰:** 모든 모델 호출의 input/output, cache read/write, retry, subagent 합계
4. **로컬 자원:** CPU, subprocess startup, RSS, SQLite/FTS/WAL disk 증가

`ctx_execute`가 100 KB 원문을 읽고 1 KB만 반환하면 2번은 크게 줄 수 있다. 그러나 코드 생성, 도구 정의, 호출 인자, 검색 재시도와 추가 inference가 생기면 3번은 늘 수 있다.

### 9.2 외부 근거가 말하는 범위

- Anthropic Tool Search의 85% reduction은 50개 이상, 약 77K token의 대형 tool set을 약 8.7K로 줄인 내부 사례다. 10개 미만 또는 compact schema에서는 search round-trip 때문에 upfront loading이 더 빠를 수 있다.
- Anthropic Programmatic Tool Calling은 75-tool benchmark에서 billed input 약 38% 감소를 보고했지만, 1~2 sequential call workload에서는 약 8% 비쌌다. 10~49 tools production traffic의 전형적 범위는 20~40%로 소개되지만 workload-dependent다.
- 이 수치는 host가 제공하는 실제 sandbox와 programmatic orchestration의 결과다. 동일 사용자 subprocess인 참조 `ctx_execute`의 검증값으로 전용할 수 없다.

### 9.3 로컬 benchmark에서 확인된 것

참조 구현의 executor/FTS microbenchmark는 모델 inference와 MCP 왕복을 제외한 로컬 비용만 측정했다.

| 항목 | 측정값 개요 |
|---|---:|
| JavaScript/Bun execute | 약 75~100 ms |
| Python execute | 약 280~320 ms |
| Windows PowerShell shell | 약 1.06~1.25 s |
| 5,000 docs fuzzy search cold | 약 13 ms |
| warm LRU/FTS | 로컬 함수 수준에서는 매우 빠름; end-to-end 대표값 아님 |

작은 fixed output이나 PowerShell one-liner는 native tool이 낫다. 대형 로그/JSON/목록을 작은 집계로 줄일 때만 execute 비용이 상쇄된다.

### 9.4 break-even 시작점

```text
순절감 =
  native tool이 반환했을 원문 B
  - ctx 결과 R
  - 추가 code/query/path 인자 A
  - 후속 검색 결과 S
  - 고정 schema/routing 비용 F의 회당 상각분
  - retry/cache-miss/추가 inference 비용 E
```

- 2 KB 이하: native Read/Grep 우선
- 2~8 KB: 필요한 정보 비율과 재사용 여부로 판단
- 8~32 KB 이상: 작은 aggregate/filter 결과만 필요하면 ctx 후보
- 100 KB 이상: 전체 원문이 필요하지 않다면 강한 후보
- 편집할 파일: 원문을 결국 확인하므로 native Read가 정당한 경우가 많음

이 임계값은 실험 시작점이지 제품 보증값이 아니다.

## 10. 현재 통계·벤치마크의 한계

참조 구현의 주요 산식은 대략 다음 형태다.

```text
keptOut = bytesIndexed + bytesSandboxed + cacheBytesSaved
totalProcessed = keptOut + bytesReturned
tokensSaved ~= keptOut / 4
```

문제점:

- 100 KB를 읽고 같은 100 KB를 반환해도 50% 절약처럼 보일 수 있다.
- `ctx_index(content: ...)`는 원문이 tool argument로 이미 context를 통과했어도 saved로 잡힐 수 있다.
- cache hit는 network/parsing/DB 작업을 줄이지만 동일 결과가 context에 반환되면 window 절약이 아니다.
- local event/snapshot byte가 실제로 회피된 model input이라는 보장이 없다.
- `bytes/4`는 tokenizer, JSON framing, schema, role boundary, model version을 반영하지 않는다.
- tool definition, 생성 코드, 추가 inference, follow-up search, retry와 정확도가 빠진다.
- 반대로 일부 `ctx_execute_file` 입력 바이트는 계측되지 않아 과소계측도 있다.

따라서 `ctx_stats`는 local routing trend와 bytes suppressed 진단용으로만 사용하고, 실제 토큰·비용·달러를 주장하는 근거로 사용하지 않는다.

### 최소 신뢰 가능한 A/B 실험

비교군:

1. native Read/Grep/Bash
2. `ctx_execute`
3. `ctx_execute_file`
4. `ctx_index + ctx_search`
5. 10-tool deferred mode
6. hard minimal-tool mode

실험 축:

- 원문 크기: 0.5, 2, 8, 32, 128, 512 KB
- 필요한 정보 비율: 0.1%, 1%, 10%, 50%
- 재사용: 1, 2, 5회
- cold/warm DB와 cache miss/hit
- 로그 filter, JSON aggregate, exact lookup, multi-file search, edit-before-read
- 정답 oracle가 있는 task

필수 지표:

- 각 모델 호출의 실제 provider input/output tokens
- cache creation/read tokens 분리
- tool schema, arguments, results tokens
- model call 수, retry, recall, task accuracy
- end-to-end p50/p95
- local CPU, peak RSS, DB/WAL 증가량
- parent active-window ledger와 total billed-token ledger 분리

동일 모델·버전·prompt 조건으로 순서를 무작위화하고 조건당 최소 30회 측정한다. `bytes/4`는 보조 진단에만 남긴다.

## 11. SQLite 결정과 재평가 조건

### 11.1 확정

```text
modernc.org/sqlite v1.53.0
modernc.org/libc v1.73.4
```

- `modernc.org/libc`는 upstream `go.mod`와 정확히 맞춘다.
- v0.0.1에는 SQLite driver interface, OS별 driver 분기, `ncruces` build tag를 만들지 않는다.
- `database/sql` 자체가 나중의 교체점이다.

### 11.2 ncruces 보류 이유

- `ncruces/go-sqlite3`는 benchmark-only 프로젝트가 아니며 FTS5와 실사용 사례가 있다.
- 그러나 검토 당시 최신 `v0.35.2`는 Windows 고동시성 WAL 데이터 손상 가능성을 경고했고 issue `#404`, fix PR `#405`가 열려 있었다.
- 특정 prepared statement benchmark의 약 3.15배 결과를 일반 성능 우위로 확대할 수 없다.
- 별도 프로세스 sandbox가 아니므로 “SQLite 오류가 Go 프로세스를 절대 죽이지 않는다”는 주장도 성립하지 않는다.

재평가 조건:

1. `#404/#405` 수정이 정식 릴리스에 포함
2. Windows/Linux/macOS multi-process WAL integrity gate 통과
3. 실제 FTS5/BM25 workload에서 modernc 대비 의미 있는 이점

### 11.3 새 설계에서 아직 확정할 DB 계약

- `journal_mode`, `synchronous`, `busy_timeout`, bounded retry의 정확한 값
- process당 connection 수와 writer coordination
- `PRAGMA integrity_check`, recovery, corruption handling
- project/worktree identity와 DB filename
- schema version (`PRAGMA user_version` 제안은 있었으나 최종 승인 아님)
- FTS5 porter/trigram/BM25 동등성

## 12. 검증 스냅샷

### 12.1 1.3.0 R1/R3 targeted tests

실행:

```powershell
$env:NODE_OPTIONS='--max-old-space-size=2048'
vitest run \
  tests/hooks/r1-posttooluse-index.test.ts \
  tests/hooks/r1-read-guard.test.ts \
  tests/hooks/r1-readstate.test.ts \
  tests/hooks/r1-toolindex.test.ts \
  tests/hooks/r3-large-read-guard.test.ts \
  tests/r1-refresh-budget.test.ts \
  --pool=forks --maxWorkers=1
```

결과:

```text
Test Files 6 passed
Tests 56 passed
Duration 3.11s
```

### 12.2 넓은 server/metrics regression 묶음

현재 `54842bd`에서 재실행했다.

```powershell
$env:NODE_OPTIONS='--max-old-space-size=2048'
vitest run tests/core/server.test.ts \
  tests/core/echo-commands.test.ts \
  tests/analytics/format-report.test.ts \
  --pool=forks --maxWorkers=1
```

결과:

```text
Test Files 2 failed | 1 passed
Tests 17 failed | 495 passed
Duration 76.40s
```

따라서 참조 구현 전체를 green 또는 보안 검증 완료로 간주할 수 없다. 이 문서 작업에서는 실패 원인을 수정하지 않았다.

### 12.3 adoption measurement

이전 검수 시점의 post-install measurement는 `0 sessions / 0 contexts`였고 최소 표본 기준은 60이었다. ADR corpus의 organic `ctx_execute` adoption 3.82%와 혼동하면 안 된다. 새 1.1~1.3 hook의 실제 효과도 provider-level 운영 표본으로 다시 측정해야 한다.

## 13. 새 설계서가 반드시 답해야 할 질문

1. `v0.0.1`의 **기능 계약 10개**와 **기본 MCP 노출 표면**을 어떻게 분리할 것인가?
2. `ctx_execute`를 제거하고 `ctx_transform`으로 대체할 것인가, 아니면 세 OS에서 어떤 OS sandbox를 사용할 것인가?
3. Go 실행은 외부 Go toolchain을 사용할 것인가? 셸 실행의 OS별 정확한 contract는 무엇인가?
4. current-project search와 explicit global search를 tool/server/profile 중 무엇으로 분리할 것인가?
5. project ID와 worktree ID를 realpath, case folding, symlink/junction, Git common-dir와 함께 어떻게 canonicalize할 것인가?
6. Content DB와 Session DB의 exact path, schema, WAL, connection, retry, purge contract는 무엇인가?
7. passive indexing 대상과 secret filtering, opt-in, retention, stale invalidation은 무엇인가?
8. `ctx_fetch_and_index`의 SSRF, DNS rebinding, localhost/private range, redirect, size/content-type 정책은 무엇인가?
9. `ctx_stats`를 provider usage와 local bytes ledger로 어떻게 분리할 것인가?
10. Codex와 Claude의 registration, allowlist, approval, tool deferral 차이를 어떻게 adapter로 흡수할 것인가?
11. self-upgrade와 purge를 MCP가 아니라 CLI로 제한할 것인가?
12. ELv2 attribution, modified notice, distribution/hosted-service 제한을 어떤 파일에 명시할 것인가?

## 14. 구현 전 acceptance gate

- 공개 10개 계약의 golden fixtures와 compatibility matrix
- current-project/global search 권한 분리 검증
- path traversal, symlink/junction, case-fold, UNC 테스트
- secrets가 index, logs, stats, tool results에 남지 않는 canary 테스트
- execute/transform의 no-network, no-credential, resource-limit 테스트
- multi-process WAL integrity와 crash recovery를 세 OS에서 검증
- FTS5 porter/trigram/BM25 정확도·성능 검증
- provider token 기반 A/B benchmark
- Codex `enabled_tools`/approval과 Claude Tool Search/ask 실제 host smoke test
- formatting, unit, integration, cross-build를 memory-capped CI로 검증
- 전체 보안 scan은 위 경계가 설계·구현된 뒤 수행

## 15. Claude와 새 설계를 시작할 때 복사할 프롬프트

```text
CONTEXT_ROUTER_HANDOFF.md를 먼저 끝까지 읽어라.

이 문서는 새 설계서가 아니라 결정·증거 기준선이다.
- '확정'은 기본 요구사항으로 유지한다.
- '차단'은 해결책 없이 되살리지 않는다.
- '폐기'는 새 근거와 사용자 승인 없이는 재도입하지 않는다.
- 사실, 추론, 제안을 분리한다.

목표는 context-router의 새 설계문서를 처음부터 작성하는 것이다.
아직 구현하지 말고, 가장 큰 미결정 사항부터 한 번에 하나씩 사용자와 확정하라.
첫 질문은 10개 기능 계약과 기본 MCP 노출 표면을 분리하는 구조여야 한다.
Ponytail full을 적용하되 보안·데이터 손실 방지·검증은 축소하지 마라.
```

## 16. 주요 근거

### 로컬

- `C:\Users\js\Documents\ClaudeCode\context-mode\src\server.ts`
- `C:\Users\js\Documents\ClaudeCode\context-mode\src\executor.ts`
- `C:\Users\js\Documents\ClaudeCode\context-mode\src\store.ts`
- `C:\Users\js\Documents\ClaudeCode\context-mode\.codex-plugin\mcp.json`
- `C:\Users\js\Documents\ClaudeCode\context-mode\docs\adr\0007-ctx-execute-conditional-keep.md`
- `C:\Users\js\Documents\ClaudeCode\context-mode\docs\adr\0008-what-actually-saves-context.md`
- `C:\Users\js\Documents\ClaudeCode\context-mode\scripts\measure-adoption.mjs`
- `C:\Users\js\Documents\ClaudeCode\context-mode\BENCHMARK.md`

### 공식 웹 자료

- Anthropic Advanced Tool Use: https://www.anthropic.com/engineering/advanced-tool-use
- Anthropic Programmatic Tool Calling: https://platform.claude.com/docs/en/agents-and-tools/tool-use/programmatic-tool-calling
- Anthropic Tool Search: https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool
- Claude Code Tool Search: https://code.claude.com/docs/en/agent-sdk/tool-search
- Claude Code MCP: https://code.claude.com/docs/en/mcp
- Claude Code Sandboxing: https://code.claude.com/docs/en/sandboxing
- Codex MCP: https://learn.chatgpt.com/docs/extend/mcp
- MCP Client Best Practices: https://modelcontextprotocol.io/docs/develop/clients/client-best-practices
- MCP Tools Specification: https://modelcontextprotocol.io/specification/2025-11-25/server/tools
- Go MCP SDK: https://github.com/modelcontextprotocol/go-sdk
- modernc SQLite: https://pkg.go.dev/modernc.org/sqlite
- ncruces issue #404: https://github.com/ncruces/go-sqlite3/issues/404
- ncruces fix PR #405: https://github.com/ncruces/go-sqlite3/pull/405
- SQLite WAL: https://sqlite.org/wal.html

웹 자료의 버전·이슈 상태는 새 설계 확정 직전에 다시 확인한다.

## 17. 검수 provenance와 한계

- storage/1-S, minimal tool surface, context savings를 서로 다른 서브에이전트 관점으로 검수했다.
- 보안 검수와 절감 검수는 완료 결과를 받았다.
- tool matrix 검수는 핵심 중간 결과가 다른 두 검수와 중복 확인된 뒤 중단됐으므로 독립된 세 번째 완결 보고서로 세지 않는다.
- 공식 자료와 로컬 코드·테스트를 우선했고 커뮤니티 수치는 일반화하지 않았다.
- 로컬 경로와 비공개 저장소 정보는 웹 검색어로 보내지 않았다.
- 이 handoff 작성은 참조 `context-mode` 소스를 수정하지 않는다.
