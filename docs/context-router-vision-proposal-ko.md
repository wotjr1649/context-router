# Context Router 비전·목표 제안서

> 문서 상태: 제안 (사용자 승인 전, 구현 착수 금지)
> 작성일: 2026-07-17
> 기준선: `CONTEXT_ROUTER_HANDOFF.md`의 `확정`·`차단`·`폐기`를 그대로 준수한다
> 병행 참조: `context-router-graph-engine-go-mcp-design-ko.md` (설계 기준서 초안 0.1.0)
> 참조 구현(oracle): `C:\Users\js\Documents\ClaudeCode\context-mode` (ctxscribe 1.3.0)

이 문서는 새 설계서가 아니라, 설계서를 쓰기 전에 "context-router로 무엇을 이루려는가"를 확정하기 위한 제안이다. HANDOFF §13의 12개 질문 중 방향을 정해야 진도가 나가는 것(Q1, Q2, Q9, Q11)에 권장안을 제시하고, 마지막에 사용자 결정 목록으로 끝난다.

---

## 1. 미션

> **컨텍스트 창은 추론에 쓰고, 바이트는 디스크에 둔다.**

context-router는 AI 코딩 에이전트 세션에서 발생하는 대형 원문(파일, 빌드/테스트 로그, 도구 출력, 웹 문서)을 모델 컨텍스트 창 밖의 로컬 저장소에 두고, 필요한 순간 **정확한 원문 조각만** 창 안으로 통과시키는 **Node 비의존 단일 Go 바이너리 MCP 서버**다.

한 줄 포지셔닝: *ctxscribe가 증명한 것(검색 인덱스는 진짜다)과 반증한 것(자발적 채택과 바이트 환산 절약 주장은 가짜다)을 모두 반영한 2세대 재설계.*

## 2. 이루고자 하는 것 (제안)

측정 가능한 목표 7개. 근거는 모두 참조 구현의 62일 코퍼스 실측(ADR-0007/0008)과 HANDOFF 검증 항목이다.

| # | 목표 | 성공 판정 | 근거 |
|---|---|---|---|
| G1 | **반복 원문 재유입 제거** — 같은 파일·출력의 2회째 이후 창 유입을 인덱스 recall(~0.8 KB/hit)로 대체 | 반복 Read 바이트의 50%+ 회수 전환 | 62일 코퍼스: 세션 내 반복 Read 58.2 MB + 교차 세션 38.8 MB가 최대 레버 (ADR-0008 #1) |
| G2 | **대형 출력 외부화** — 8 KB+ 출력·파일의 기본 경로를 artifact 저장 + 조각 회수로 전환 | break-even 산식(HANDOFF §9.4) 기준 순절감 양수 | 2 KB 이하는 native가 우세, 8~32 KB+부터 ctx 후보라는 실측 임계값 |
| G3 | **안전 기본값** — 기본 노출 표면은 현재 프로젝트 read-only 검색뿐. 실행·수집·전역 검색은 명시적 옵트인 프로필 | HANDOFF §8.4 보안 테스트 전체 통과 | 보안 검수 Block: `ctx_execute` 원형은 동일 사용자 RCE (§8.1~8.2) |
| G4 | **정확 회수** — 손실 요약이 아니라 원문 조각(라인/바이트 범위 + content-hash provenance + stale 표시) 반환 | 회수된 조각이 원문과 byte-identical | 코딩 에이전트에는 "설명"이 아닌 "정확한 청크"가 필요 (참조 BENCHMARK의 설계 원칙) |
| G5 | **세션 연속성** (v0.1) — 결정·제약·오류·테스트 결과를 이벤트로 기록, compaction 후 복구 요약 제공 | 재시작 후 목표·결정·실패 테스트 복원 | 설계 기준서 Part A §7.3; 요약은 이벤트 URI 근거를 가짐 |
| G6 | **정직한 회계** — window ledger와 provider token ledger를 분리하고, 절약 주장은 provider usage로만 | bytes/4는 진단 전용으로 강등 | `ctx_stats` 산식의 과대계상은 차단 항목 (HANDOFF §10) |
| G7 | **제로 마찰 배포** — 단일 실행 파일, Windows/Linux/macOS, Claude Code·Codex 등록 어댑터 | Node/Python 없는 머신에서 등록~검색 동작 | HANDOFF 확정: Node 의존 완전 제거, stdio MCP 코어 우선 |

## 3. 제품 테제 — 왜 이 형태인가

실측이 강제하는 4개의 테제. 새 설계의 모든 선택은 이 테제와 모순되면 안 된다.

- **T1. 설득은 레버가 아니다.** 최대 프롬프팅(CLAUDE.md 규칙 벽, 훅 팁, always-load)에도 자발적 채택률은 3.82%였고, `ctx_execute` 볼륨의 80%는 강제 주입된 라우팅 블록(서브에이전트 채널)에서 나왔다. → 제품은 **모델의 선의에 의존하지 않는 경로**(패시브 인덱싱, 훅 강제, 기본값)를 우선 설계한다. v0.0.1 코어는 그 채널들이 사용할 표면을 제공하고, 강제 채널 자체(훅/플러그인)는 후속 버전 확정 사항을 따른다.
- **T2. 창 절약과 토큰 절약은 다르다.** 서브에이전트 격리는 창 기준 최고의 도구(내부 135.2 MB → 반환 7.3 MB)지만 토큰 기준 최악(스폰 고정비 ~53.4 K tok, 순 -70 M tok)이었다. → 모든 기능·문서·통계는 **two-ledger**(window vs provider tokens)를 구분해 말한다.
- **T3. 안전하지 않으면 존재하지 않는 것과 같다.** 동일 사용자 권한 범용 실행기는 나머지 모든 격리(프로젝트 분리, 도구 숨김)를 코드로 우회한다. → 기본 표면은 read-only, 실행은 격리·제약 없이는 노출하지 않는다.
- **T4. 회수는 요약을 이긴다.** 요약만 저장하고 원문을 버리면 에이전트는 결국 원문을 다시 읽는다. → 원문은 content-addressed로 보존하고, 반환은 작게, 회수는 정확하게.

## 4. v0.0.1 형태 제안 — 기능 계약과 노출 표면의 분리 (HANDOFF Q1)

### 4.1 원칙 — 3층 분리

"구현됨 / 등록됨 / 로드됨"을 서로 다른 상태로 취급한다 (Codex 교차 검토에서 정식화, §8.4):

1. **기능 카탈로그(구현됨)**: 구현 여부와 노출 여부는 별개다. *(작성 당시 "10개 전부 구현"이었으나 D10 재정의로 대체 — 현행 계약은 도구 감사 문서 §2.2 인벤토리: 코어 6 MCP + CLI 4, exec 3종은 구현째 v0.2.)*
2. **capability 프로필(등록됨)**: MCP `tools/list`에 무엇이 등록되는지는 서버 시작 플래그/설정이 결정한다. 모델이 런타임 인수로 권한을 승격할 수 없다. **이것이 보안 경계다.**
3. **호스트 로딩 정책(로드됨)**: 등록된 도구의 스키마를 eager/deferred로 싣는 것은 호스트 몫(Claude Code tool search 등). **이것은 토큰 최적화이지 보안 경계가 아니다.** 참조 구현의 "always-load 2개 + deferral 8개"는 3층을 혼동하지 않았기에 유효했다.

추가 원칙: **파괴적·운영 동작은 MCP가 아니라 CLI로.** 모델이 트리거할 이유가 없는 기능(upgrade, purge, doctor)은 사용자 명령으로 옮긴다.

### 4.2 프로필 표 (권장)

**확정 토폴로지 (2026-07-17, D1=A)**: 단일 `ctr` 등록 + 시작 플래그로 프로필 구성(`--profile search,transform` 기본, `--enable ingest,net` 옵트인). `ctr-global`만 별도 read-only 등록. 프로젝트당 쓰기 프로세스 1개 보장(다중 프로세스 WAL 회피). 서버 시작 시 stderr 권한 배너 1줄(`[ctr] profile=... net=... root=...`). 도구 접두사는 `ctr_*`(D4), 바이너리명은 `context-router`(containerd `ctr` CLI 충돌 회피). 계약 검증은 oracle의 `ctx_*`와 이름 매핑으로 수행.

| 프로필 | 노출 도구 | 기본값 | 비고 |
|---|---|---|---|
| `search` | `ctr_search` (현재 프로젝트 고정, read-only) | **ON** | `project`/`global` 인수 자체를 미노출. 결과에 provenance·stale·untrusted 표시 |
| `transform` | `ctr_transform` (신규, §5) | ON | 저장된 artifact/인덱스 데이터 대상 **순수 변환** — 파일·env·네트워크·프로세스 접근 없음 |
| `ingest` | `ctr_index` | OFF (옵트인 플래그) | 명시적 수집. 비밀정보 필터 통과 후 저장 |
| `net-ingest` | `ctr_fetch_and_index` | OFF (옵트인 플래그) | SSRF/사설망/redirect/크기 정책 필수 (Q8) |
| `exec` | `ctr_execute`, `ctr_execute_file`, `ctr_batch_execute` | **v0.2로 구현째 이전 (D10 확정)** | 구현·fixture·노출 모두 v0.2 — 실질 격리(Job Object/landlock/sandbox-exec)와 HANDOFF §8.3 실행 계약(env allowlist, cwd 고정, timeout+tree-kill, output cap, background 금지, 출력 자동 영구색인 금지) + 호스트 ask/prompt와 함께 |
| `global` | `ctr_global_search` (`ctr-global` 별도 등록) | OFF (선택 설치) | 프로젝트 allowlist + 호출별 승인. execute와 절대 동거 금지 |
| (CLI) | `context-router doctor / stats / upgrade / purge` | CLI 전용 | purge는 이중 확인. stats는 두 ledger 출력 |

이 구조가 Q1의 답이다: **기능 10개 = 코드에 존재, 기본 tools/list = 사실상 2개(search + transform)**. 참조 구현의 "always-load 2개 + deferral 8개"가 검증한 방향을 보안 경계가 있는 형태로 승계한다.

주의: 설계 기준서 초안의 "사용자용 CLI 질의 기능 없음" 원칙과 표면상 충돌한다. 재해석 제안: **질의 기능은 MCP 전용**(search를 CLI로 만들지 않음), **운영·파괴 동작은 CLI 전용**(upgrade/purge를 MCP로 만들지 않음). 이 재해석은 사용자 승인 필요 (§9 결정 D1).

## 5. 실행 문제의 답 — `ctx_transform` 기본, `exec`는 격리와 함께 (HANDOFF Q2)

### 5.1 권장: 2단 구조

1. **`ctx_transform` (v0.0.1, 기본 ON, 신규)** — "서버가 이미 보관 중인 바이트"에 대해서만 동작하는 순수 변환기.
   - 입력: artifact handle/검색 결과 집합 (+ 예외적으로 inline 값). 임의 경로·cwd·환경변수·소켓·서브프로세스는 **인터페이스에 존재하지 않음**
   - 엔진: **starlark-go** (§8.3 판정) — 파일·네트워크·env 접근이 언어 차원에서 없고, deterministic하며, 스텝·메모리 제한 가능, Python 유사 문법이라 LLM 생성 코드 정확도가 높음
   - 표면: 자주 쓰는 고정 연산(regex 추출, JSON projection, line window, head/tail, count/sort/dedupe)을 **내장 함수**로 제공하고, 스크립트는 그 조합 — Codex의 "고정 연산만" 권고(§8.4)와 스크립트 표현력의 절충. 최악의 경우에도 임의 starlark는 hermetic하다
   - 반환: 변환 결과만. RCE 표면이 구조적으로 없으므로 보안 Block을 아키텍처로 해소
   - 이것이 "Programmatic Tool Calling을 로컬에서, 호스트 지원 없이" 구현하는 이 제품의 시그니처 무브다 (PTC는 1P API 전용 — §8.1)
2. **`exec` 프로필 — v0.0.1에서는 노출하지 않는다 (권장 변경, 결정 D3).**
   - 근거 ①: §8.3 스택 판정 — v0.0.x에서 신뢰 가능한 것은 Job Object(리소스/트리 제어)·landlock BestEffort·sandbox-exec best-effort까지이고, 진짜 권한 축소(AppContainer/seccomp 정책)는 v0.2+ 과제다. "제약은 있지만 격리는 없는 execute"를 노출하는 것은 보안 검수 High finding(잘못된 sandbox 설명)의 반복이다.
   - 근거 ②: Codex 교차 검토 동의견 — 세 OS의 서로 다른 격리 수준을 하나의 보안 계약처럼 포장하면 가장 약한 플랫폼이 전체 보증을 무너뜨린다.
   - 따라서 v0.0.1에서 execute 3종은 **구현 + golden fixture 검증까지만**(계약 이행), MCP 등록 경로는 v0.2에서 실질 격리·§8.3 실행 계약·호스트 승인(Claude exact-tool ask / Codex prompt)과 함께 최초 개방한다.
   - 구현 레벨 계약(설계서에서 확정): shell은 OS별 명시(Windows: PowerShell 7+ 우선, Unix: POSIX sh), Go는 외부 toolchain 감지 사용(미설치 시 명확한 오류), 내장 Go 인터프리터 금지. 프로세스 제어는 `WaitDelay` 필수 + Unix setpgid / Windows CREATE_SUSPENDED→Job Object→resume, `KILL_ON_JOB_CLOSE`.

### 5.2 왜 이 순서인가

- 62일 코퍼스에서 `ctx_execute`의 실제 가치는 "큰 원문 → 작은 집계"였고(37%는 native Bash보다 못한 shell 호출이었다 — ADR-0007), 그 대상 원문 대부분은 **이미 서버가 저장할 수 있는 것들**(Read 결과, 명령 출력, fetch 문서)이다. 저장을 선행시키면 변환은 순수 함수가 된다.
- 진짜 명령 실행(빌드·테스트)은 v0.2 강제 채널(훅)과 결합해야 출력 캡처→artifact 저장→요약 반환의 온전한 루프가 되므로, 노출 시점을 v0.2에 맞추는 것이 제품적으로도 자연스럽다.

### 5.3 기본 라우팅 정책 — "도구 대체"가 아니라 "출력 가상화" (2026-07-17 확정, Codex 교차 검증)

"Read/Write/Bash를 완전 대체할 수 있는가"에 대한 결론: **불가능하고, 바람직하지도 않다.** 행동(실행·편집)은 native에 남기고, 큰 결과와 반복 원문만 ctr로 외부화한다.

| 상황 | 기본 경로 | 근거 |
|---|---|---|
| 작은 읽기 (≤2KB) | native Read | MCP 스키마+프레이밍 고정비가 이득보다 큼 |
| 반복 읽기 (동일 hash·fresh) | **ctr recall** | 62일 코퍼스 최대 레버(~0.8KB/hit); stale·불확실이면 native로 fail-open |
| 대형 읽기 (희소 정보만 필요) | **ctr 검색/변환** | 전체 원문이 필요하면 native windowed Read |
| 편집 전 읽기 | native Read → native Edit/Write | 호스트 편집 상태와 byte-exact 원문 필요 (Claude Code는 기존 파일 편집에 native Read 선행 요구) |
| 검색 탐색 | native Grep (exact·저출력) / **ctr** (광범위·반복 탐색) | exact regex·경로 확인은 native가 더 정확 |
| 명령 실행 | native Bash 유지 + 대형 stdout/stderr만 **ctr 세션 한정 artifact**로 외부화 (v0.2) | 실행 sandbox는 host 몫. **Bash 출력의 자동 영구 FTS 색인 금지**(redaction만으로는 비밀·PII·injection 제거 불충분) |
| 웹 문서 (길거나 재사용) | **ctr fetch/index/search** (옵트인) | SSRF·격리 정책 필수 |
| TTY·streaming·background·부작용 명령, 호스트 전용 렌더링(PDF/이미지) | native 전용 | ctr 대체 부적합 영역 |

## 6. 저장 구조·측정 방향 (Q5/Q6/Q9 스케치)

설계서에서 확정할 사항이므로 방향만 제안한다.

- **저장 루트**: OS 표준 사용자 데이터 디렉터리 (`%LOCALAPPDATA%\context-router`, `~/.local/share/context-router`, `~/Library/Application Support/context-router`). 프로젝트 루트 오염(`.context-router/`) 대신 공통 루트 — HANDOFF 1-S 확정 구조 준수.
- **프로젝트 ID**: canonical realpath(symlink/junction 해석) + Windows case-fold + Git common-dir 우선. worktree는 Session DB 단위.
- **DB**: modernc.org/sqlite **v1.54.0 + libc v1.74.1 쌍 고정** (2026-07-17 사용자 승인으로 HANDOFF 고정판에서 갱신). Content DB(프로젝트별) / Session DB(worktree별) 분리.
- **PRAGMA 계약 (2026-07-17 확정)**: 모든 DB `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `foreign_keys=ON`, `PRAGMA user_version` 스키마 버전. 연결: 단일 프로세스 전제, DB당 writer 커넥션 1 + reader 풀 2~4. 손상 정책: Content DB(파생)=삭제+재색인, Session DB(원본)=`.bak` 보존 후 재생성. cache/mmap 튜닝은 측정 후.
- **원문**: DB BLOB이 아니라 content-addressed 파일(중복 제거, 참조 카운트) — 설계 기준서 §9.2 승계.
- **측정(Q9)**: `stats`를 두 ledger로 분리. ① local bytes ledger(저장/반환 바이트 — 진단 전용, 달러 환산 금지), ② provider ledger(호스트 transcript의 실제 usage 필드 사후 집계 — Claude Code JSONL, Codex usage 어댑터는 참조 구현에 전례 있음). 절약 주장은 ②로만. A/B 하네스는 HANDOFF §10 설계 채택.
- **인과 귀속(Codex 'Causal Savings Ledger' 채택)**: provider usage를 얻어도 캐시·retry·서브에이전트·난이도 편향 때문에 단순 비교로는 절약을 귀속할 수 없다. 세션/과제를 baseline군과 recall-enabled군으로 **무작위 배정**해 input/output/cache 토큰·정답률·p95를 합산 비교하고, 개별 호출 단위의 가상 "절약 토큰"은 주장하지 않는다.

## 7. 로드맵 제안

| 단계 | 내용 | 게이트 |
|---|---|---|
| M0 설계서 | 12질문 전부 답하는 설계문서 + golden fixtures 목록 | 사용자 승인 |
| v0.0.1 | **재정의 계약(D10)**: `ctr_search`/`ctr_fetch`/`ctr_transform` 기본 + `ctr_index`/`ctr_fetch_and_index` 옵트인 + `ctr_global_search` 별도 등록 + CLI 4종(doctor/stats/purge/upgrade) + 1-S 저장 | HANDOFF §14 acceptance gate(재정의 범위 적용). 전 범위 완료 전 태그 금지 |
| v0.1 | **완료(구현 랜딩, 태그 대기)** — 세션 이벤트/복구(`ctr_record_event`·`ctr_session_summary`·`ctr_export_events`) — 피치는 "무손실 복원" | 설계 기준서 Part A MVP 기준 |
| v0.2 | Claude Code 훅 패키징 — 패시브 인덱싱(Shadow Recall)·large-read guard·자동 계측 9종 등 **강제 채널** 활성화 (T1의 본론). exec 3종·Codex 훅은 분리(D21, 2026-07-20 개정 — v0.2 설계서) | 수동 A/B 측정 결과 기록(무작위 하네스는 후속, D27) |
| v0.3+ | exec 3종(OS 격리 별도 트랙, §8.3 계약·D10), Codex 훅, 무작위 A/B·OTel, OS 샌드박스 재평가, global search UX, graph-engine 연동(ContextPack 소비) | 별도 결정 |

핵심: **v0.0.1은 "코어 표면", v0.2가 "실제 절약이 발생하는 강제 채널"**이다. ADR-0008이 증명했듯 훅 없는 MCP 단독은 자발 채택의 한계(3.82%)에 갇힌다. 이 사실을 로드맵에 정직하게 반영한다.

## 8. 리서치 근거 (2026-07-17 수집)

### 8.1 Anthropic/MCP 최신 동향과 시사점

2026-07-17 웹 조사 결과 (서브에이전트 수집):

- **Advanced Tool Use 3종 성숙**: Tool Search Tool·Programmatic Tool Calling은 Claude API에서 GA. 실측치 확정 — 도구 정의 deferral 85% 절감, PTC 37% 토큰 절감, Tool Use Examples 파라미터 정확도 72→90%. 단 PTC는 1P API 전용(Bedrock/Vertex 미지원)이고 Claude Code/Codex 로컬 호스트에는 없다.
- **"Code execution with MCP"(98.7%) 패턴은 제품으로 흡수됨**: Anthropic Managed Agents가 100K 토큰 초과 MCP 출력을 샌드박스 파일로 **자동 오프로드**(관리형 전용). → 우리 패턴의 1P 검증. 로컬-first·호스트 불문(특히 Codex) 오프로드+FTS+partial fetch는 여전히 공백이며 context-router의 몫.
- **MCP 스펙 대개정 임박**: 현행 최종판 2025-11-25(Tasks 실험 도입, elicitation 개편). **2026-07-28 RC** 예정 — stateless core(initialize/session-id 제거), Extensions 프레임워크, Tasks의 extension 이동+lifecycle 재설계, Sampling/Roots/Logging deprecated, full JSON Schema 2020-12. → 설계 지침: 프로토콜 세션에 상태를 두지 말 것(디스크 store 기반이 정답), sampling 의존 설계 금지, `readOnlyHint`/`destructiveHint` annotation 지금부터 부착, Tasks는 extension 확정 후 채택.
- **호스트 비대칭**: Claude Code는 MCP Tool Search 기본 ON(2026-01, 정의가 컨텍스트 10% 초과 시 자동 defer; 단 stdio만 — HTTP transport 서버는 defer 안 됨). **Codex는 deferral 등가물이 없고** `enabled_tools`/`disabled_tools`+도구별 승인뿐. → Claude에서는 10-도구 표면이 저렴해졌지만 Codex에서는 여전히 도구 수 절제가 유효. 기본 2-도구 노출은 두 호스트 모두에서 안전한 선택.
- **호스트가 컨텍스트를 지우는 시대**: server-side compaction(beta)이 1차 전략으로 격상, context editing·memory tool(GA) 결합. → context-router의 포지션은 "호스트가 지우기 전 원본이 보존되고, 지운 뒤에도 정확히 재조회 가능한 진실 소스".

시사점 요약: 도구 정의 비용 문제는 호스트가 풀고 있고(스키마 쪽), **출력 바이트 문제는 1P 관리형에서만 풀리는 중**(콘텐츠 쪽) — 로컬·양호스트에서 콘텐츠 쪽을 푸는 것이 이 제품의 존재 이유로 재확인됨.

### 8.2 경쟁 지형과 차별화

2026-07-17 웹 조사 결과 (서브에이전트 수집, URL은 조사 원문 참고):

- **게이트웨이/라우터군은 컨텍스트를 다루지 않는다.** mcp-router, MetaMCP, Docker MCP Gateway, mcpjungle, IBM mcp-context-forge, Lasso 등 17종 서베이(Q1 2026)에서 출력 절감·외부 저장·정확 재인출 기능은 전무 — 전부 aggregation/인증/보안. 유일한 예외는 mcpproxy-go의 BM25 tool retrieval(스키마 토큰 절감)이다.
- **메모리 서버군은 lossy·global이 기본.** 공식 server-memory(단일 JSON), mem0/OpenMemory(vector, user 단위), Letta, cipher 모두 전역 스코프 요약 저장. per-project SQLite 선례는 context-portal(ConPort, 765★)뿐이며 그것도 "정리된 지식" 저장소지 원문 artifact 저장소가 아니다.
- **출력측 오프로딩은 ad-hoc 수준.** 토큰 절감 도구 대부분이 스키마 bloat만 공격(mcp-compressor, lazy-load 프록시, 호스트 Tool Search). 출력을 디스크로 내리고 핸들을 반환하는 것은 개인 프록시 실험 수준 — 제품 공백.
- **세션 연속성의 지배자 claude-mem(87.6k★ 보고)은 LLM 압축 요약 저장** — 결정·오류·테스트 원문의 byte-exact 복원 불가. Claude Code 네이티브 세션 메모리도 동일하게 lossy.
- **샌드박스 실행 서버**: pydantic mcp-run-python은 Pyodide+Deno 이중 격리였으나 retired; e2b/microsandbox/daytona는 microVM·컨테이너 — 전부 무거운 인프라 의존. "임베디드 순수 변환"(§5) 접근의 선례는 없다.
- **이름 충돌 1건**: `mohankrishnaalavala/context-router`(Python, 10★, 2026-04, PyPI `context-router-cli`) — "Memory-aware context engine for AI coding agents, MCP-native, local-first"로 **도메인까지 동일**. 규모는 작으나 조기 네임스페이스 확보(Go module/npm/GitHub) 또는 개명 검토 필요 → 결정 D7.

차별화 요약: ① 출력측 artifact offload + byte-exact 재인출(공백 시장), ② 런타임 의존 0의 단일 Go 바이너리(경쟁은 TS/Python/Docker), ③ per-project 격리 저장(전역 메모리 대비), ④ provider usage 기반 정직한 측정(시장 전체가 검증 불가 수치 경쟁 — 신뢰 격차 자체가 포지션), ⑤ lossy 요약이 아닌 이벤트+원문 결합의 정확한 세션 복구.

### 8.3 Go 구현 스택 검증

2026-07-17 웹 조사 결과 (서브에이전트 수집, 버전 실측):

| 항목 | 판정 | 근거 |
|---|---|---|
| MCP SDK | **공식 `modelcontextprotocol/go-sdk` v1.6.1** | v1.0.0(2025-09) 이후 호환성 보장 공식화, Google 공동 유지보수, 보안 패치 트랙, stdio 내장, struct 태그 기반 스키마 자동 생성. 2026-07-28 신스펙 대응 v1.7.0-pre 진행 중. mark3labs/mcp-go는 여전히 0.x |
| SQLite | **modernc 유지** — 최신 쌍은 v1.54.0 + libc v1.74.1 (SQLite 3.53.3) | 6개 타깃 전부 CGO-free 지원. HANDOFF 고정판(v1.53.0/v1.73.4)에서의 갱신 여부는 설계 확정 직전 재검(HANDOFF §16 규칙); **libc 버전은 sqlite go.mod와 쌍 일치 필수** |
| FTS5 | porter·trigram 모두 **코어 토크나이저**로 사용 가능 (3.53.3 포함) | 참조 구현과의 검색 동등성(porter/trigram/BM25) 달성 가능. CI에 FTS5 스모크 테스트 1줄 |
| ncruces | **재평가 시점 아님** | #404(Windows WAL corruption) 여전히 open, PR #405 미머지 — HANDOFF 보류 판단 그대로 유효 |
| Transform 엔진 | **starlark-go** | 언어 차원에서 파일/네트워크/env 부재, deterministic, 스텝 제한+취소 가능, Python 유사 문법(LLM 정확도). wazero는 WASM 바이너리 제출 요구로 "LLM이 작은 스크립트 제출" 시나리오에 부적합, cel/expr는 다단계 스크립트에 표현력 부족, risor는 릴리스 정체 |
| OS 격리 | v0.0.x는 **Job Object + landlock BestEffort(go-landlock v0.9.0) + sandbox-exec 래핑**까지만 신뢰 가능 | AppContainer는 유지보수되는 범용 Go 라이브러리 부재(직접 win32 필요), macOS sandbox-exec는 deprecated이나 실동작(공식 대체재 없음). 진짜 권한 축소는 v0.2+ 과제 → §5 exec 노출 연기의 직접 근거 |
| 프로세스 제어 | `Cmd.Cancel`+`WaitDelay`(필수) / Unix setpgid→`kill(-pid)` / Windows CREATE_SUSPENDED→`AssignProcessToJobObject`→resume, `KILL_ON_JOB_CLOSE` | 파이프 상속으로 인한 Wait 영구 블록 방지, 트리 전멸 보장 |
| 배포 | goreleaser v2.17, `CGO_ENABLED=0` 6타깃 순수 크로스컴파일 | 바이너리 크기 추정 15–25MB/타깃(-s -w, modernc 지배 요인), 릴리스 CI에서 실측 |

### 8.4 Codex 교차 모델 자문 (advisory)

Codex(GPT 계열)가 두 문서를 독립적으로 읽고 낸 의견. 자문이며, 아래와 같이 선별 반영했다.

**채택한 것**

- **3층 분리** — "기능이 구현돼 있다는 사실 / MCP에 등록됐다는 사실 / 모델 컨텍스트에 스키마가 로드됐다는 사실"을 서로 다른 상태로 취급 (→ §4.1). 로딩 정책은 토큰 최적화이지 보안 경계가 아니라는 명시 포함.
- **v0.0.1 `exec` 미노출** — 세 OS의 상이한 격리 수준을 하나의 보안 계약처럼 포장하지 말 것 (→ §5, 스택 검증 §8.3이 독립적으로 동일 결론).
- **Causal Savings Ledger** — 무작위 대조군 없이는 절약의 인과 귀속 불가; provider 토큰+정답률+p95 합산, 호출 단위 절약 주장 금지 (→ §6).
- **Shadow Recall** — Read 후 자동 색인, 동일 hash 재조회 직전 훅이 짧은 recall pack 자동 주입, 수정 목적·불확실 시 fail-open (→ v0.2 강제 채널의 구체안).
- **불변 권한 영수증** — capability별 별도 MCP 등록명 + 서버 시작 시 권한·경로·네트워크 상태 1줄 배너, 재등록 없이는 권한 불확대 (→ 설계서 등록 UX에 반영 예정).
- **틀렸을 가능성 지적 3건** — ① 패시브 색인이 항상 순절약이라는 가정(색인 비용·DB 증가·검색 재시도·낮은 precision으로 총 토큰 증가 가능), ② 강제 훅의 이식성·신뢰 가정(오차단·불투명 자동 저장은 제품 이탈 유발), ③ provider usage만으로 인과 계산 가능하다는 가정. → 전부 v0.2 측정 설계의 검증 항목으로 등재.

**채택하지 않은 것**

- *"10개 구현 완료를 출시 목표로 삼지 말라"* — 당시에는 HANDOFF 확정과 충돌하여 불채택. *(후기: 이후 도구 감사에 근거한 D10으로 계약 자체가 재정의되어, 결과적으로 이 방향의 절반 — exec 3종 이전 — 이 사용자 승인 하에 수용됨. 태그 규율은 재정의 범위 기준으로 유지.)*
- *transform을 고정 연산만으로 제한* — 부분 채택. 고정 연산을 내장 함수 셋으로 제공하되, hermetic starlark 스크립트로 조합 표현력을 확보 (§5.1). 위험 논거(임의 코드)의 실체는 ambient authority인데 starlark에는 그것이 언어 차원에서 없다.

### 8.5 Codex 교차 모델 자문 2 — 네이티브 도구 완전 대체 타당성 (advisory)

판정: **완전 대체 불가·부적절 — "선택적 출력 가상화"가 정답** (§5.3 라우팅 표로 채택). 우리 예비 분석에 대한 유효한 교정, 전부 수용:

- "Edit/Write가 항상 native Read를 요구한다"는 과대 진술 — Claude Code의 **기존 파일 편집/덮어쓰기 경로에 한정**된 불변식이며, 새 파일 Write와 Codex의 패치 도구에는 적용되지 않는다. 권장 경로 자체는 불변(native Edit/Write + 편집용 native windowed Read).
- break-even 2/8/32KB는 실험 시작점이지 보증값이 아님(HANDOFF §9.4 원문 그대로). 8~32KB 구간은 "ctr 우세"가 아니라 "작은 집계만 필요할 때 후보". Tool Search 85%는 50+ 도구·77K tok 사례 — 10개 표면에 일반화 금지. Codex deferral 부재도 호스트 버전별 재검증 대상.
- 코퍼스 수치 provenance: 88.4MB(Read 총량)·43.3%(대형 Read의 편집 선행률)는 **참조 구현 ADR-0008 출처**로 명시할 것(이 저장소 문서에는 없음). 반복 집합의 분모(세션 내 58.2MB vs 교차 세션 38.8MB)도 구분 표기. "편집으로 이어짐 ≠ 전체 원문 바이트 필요" — 편집 구간·실참조 바이트 비율을 측정 항목에 추가.
- **Bash는 실행과 출력 캡처를 분리**: 실행은 native 유지, 큰 출력만 세션 한정(ephemeral) artifact로 외부화. 자동 영구 FTS 색인 금지 → v0.2 설계 제약으로 채택.
- 3.82%는 ERA-2 organic 수치 — "CLAUDE.md 설득은 항상 3.82%"로 일반화 금지. Codex 호스트에서는 검증된 PreToolUse 등가물 없이는 hard enforcement를 주장하지 말 것(도구 필터·instructions는 노출 제어이지 라우팅 메커니즘이 아님).
- 측정 지표 확장: recall 전환율·provider 순토큰에 더해 **정답률, 추가 호출/retry, p95, stale/false-deny율, 편집 escape율, DB 증가량, 비밀정보 잔존**. 무작위화는 호출 단위가 아니라 task/session 단위.

## 9. 사용자 결정 요청 목록

| ID | 결정 | 권장 | 관련 |
|---|---|---|---|
| D1 | 프로필 구조 + CLI 재해석 | **확정 (2026-07-17)**: 토폴로지 A — 단일 `ctr` 등록+플래그, `ctr-global`만 별도 read-only 등록, 운영·파괴는 CLI 전용, 시작 배너로 권한 가시화 | Q1, Q11 |
| D2 | `ctr_transform`을 11번째 기능으로 추가하고 기본 compute로 삼는 것 | **확정 (2026-07-17)**: D1-A의 기본 표면(search+transform)에 포함 승인. starlark-go + 고정 연산 내장 함수 | Q2 |
| D3 | `exec` 노출 시점 | **확정 (2026-07-17)**: v0.2 연기. 구현·golden fixture는 v0.0.1 계약에 포함, MCP 등록 경로는 실질 격리+§8.3 계약+호스트 ask/prompt가 갖춰진 v0.2에서 최초 개방. 그동안 ctxscribe 병행으로 실사용 공백 없음 | Q2, Q3 |
| D4 | 도구 이름 | **확정 (2026-07-17)**: 사용자 제안 `ctr_*` 채택 (서버 등록명 `ctr`) — 설치된 ctxscribe `ctx_*`와의 혼동 제거 + 독립 재구현 정체성. 바이너리명은 `context-router`(containerd `ctr` CLI 충돌 회피). oracle 계약 검증은 이름 매핑 | 계약 |
| D5 | Go 실행 방식 | **확정 (2026-07-17)**: 외부 toolchain 감지(PATH의 `go`, 미설치 시 명확한 오류+shell 대체 안내). 내장 인터프리터(yaegi) 금지 | Q3 |
| D6 | `ctr_stats` 노출 위치 | **확정 (2026-07-17)**: CLI 전용 (D1 결정의 공통 전제 "doctor/stats/upgrade/purge = CLI"로 승인) | Q9, Q11 |
| D7 | 이름 | **확정 (2026-07-17)**: `context-router` 유지 + 네임스페이스 선점(GitHub 리포·Go module path; npm은 v0.2 플러그인 배포 시점 검토). 도구 접두사 `ctr_*`(D4)로 이미 차별화. git init + docs 초기 커밋 실행 | §8.2 |
| D8 | Go 스택 | **확정 (2026-07-17)**: go-sdk v1.6.1 + modernc v1.54.0/libc v1.74.1 + starlark-go + x/sys. cobra·yaml·ORM·DI·로깅 프레임워크 미사용 (stdlib flag/slog/testing), goreleaser CGO_ENABLED=0 6타깃 | §8.3 |
| D9 | SQLite PRAGMA 계약 | **확정 (2026-07-17)**: 전체 NORMAL 세트 (§6 PRAGMA 계약 참조) | Q6 |
| D10 | v0.0.1 계약 재정의 — "oracle 10개 동등" → 도구 감사 문서 §2.2 인벤토리(코어 5 MCP + global + CLI 4 + 신규 fetch/transform, exec 3종 구현째 v0.2) | **확정 (2026-07-17)** — 근거: `context-router-tool-audit-ko.md` | HANDOFF 확정 변경 (사용자 승인) |
| D11 | MCP SDK 방침 | **확정 (2026-07-17)**: 현행 go-sdk v1.6.1(spec 2025-11-25)로 구현 진행. 차기 스펙 정식 출시 + go-sdk 안정판 시 별도 업그레이드 마일스톤 — RC 대기로 지연하지 않음 | §8.1 |
| D12 | fetch 저장 파이프라인 개선 — 참조 구현은 Turndown으로 전체 페이지 변환(본문 추출 없음 → boilerplate 오염 확인) | **확정 (2026-07-17, 적대 검증 반영)**: readeck/go-readability(go-shiori는 deprecated 확인) → DOM 비교 충실도 판정(fail-open: <500자·텍스트<30%·pre/code<50%) → html-to-markdown v2(+table). 원문 HTML은 비색인 source blob 보존. D8 의존성 7개로 수정 | 설계서 §4.5 |
| D13 | 파일 반파편화 규약 — 패키지당 소스 1~2파일 시작, 분리는 ①OS build tag ②~1,000줄 초과+응집 이음새 ③생성물/embed 3경우만. 타입별 1파일·doc.go 단독·utils.go 금지. 선호 밴드 300~1,000줄, v0.0.1 목표 ≈12~15 소스 파일 | **확정 (2026-07-17)** — 사용자 지시. 구현 규약 문서에 성문화 | 유지보수 |

## 10. HANDOFF 12질문 매핑

| Q | 이 문서의 답 | 상태 |
|---|---|---|
| Q1 계약 vs 노출 | §4 프로필 구조 | 권장안 제시 |
| Q2 execute 대체 | §5 transform 기본 + exec는 v0.2 실질 격리와 함께 | 권장안 제시 |
| Q3 Go/셸 실행 계약 | §5.1 (OS별 셸 명시, 외부 toolchain) | 방향 제시, 설계서에서 상세 |
| Q4 current/global 분리 | §4.2 `global` 별도 등록 + allowlist | 권장안 제시 |
| Q5 프로젝트/worktree ID | §6 | 방향만, 설계서 확정 |
| Q6 DB 계약 | §6 | 방향만, 설계서 확정 |
| Q7 패시브 인덱싱·secret | v0.2 범위 + 설계서 | 미결 |
| Q8 fetch SSRF 정책 | `net-ingest` 프로필로 격리 | 방향만, 설계서 확정 |
| Q9 stats 분리 | §6 two-ledger | 권장안 제시 |
| Q10 Claude/Codex adapter | §7 v0.2 | 방향만 |
| Q11 upgrade/purge CLI 제한 | §4.2 CLI 전용 | 권장안 제시 |
| Q12 ELv2 고지 파일 | 설계서 Phase 0 문서 세트에 포함 | 미결 |

## 수렴 로드맵 (D57, 2026-07-23 — 결정 원본: design-v0.10 §0)

- **종착지 = context-router.** ctxscribe(사용자의 mksglu/context-mode
  하드 포크)는 대체 시까지의 브리지 — 대체 후 포크 유지보수(업스트림
  선택 포팅 노동)를 종료한다.
- **대체 게이트 = exec 3종(D21 트랙).** exec가 ctr에 이식되면
  ctxscribe의 잔여 고유 가치(think-in-code 실행 샌드박스)가 소멸한다.
  v0.11+ 주력 후보로 격상.
- **근거 실측(2026-07-23, session-26)**: ctr MCP 도구 호출 8회 vs
  ctxscribe 422회(본 프로젝트 전 기간) — ctr의 고유 가치는 능동
  도구가 아니라 배경 레이어(zero-reliance 훅 캡처·A/B 계측·가드)에
  있고, 이는 ctxscribe가 구조적으로 갖지 못하는 축.
- **병행 축**: 회수 경로 채택 개선(ctr_search/ctr_fetch 사용 유도 —
  캡처의 가치는 소비가 만든다).

---

*폐기 항목 재도입 없음 확인: 단일 전역 DB(×), ncruces 즉시 채택(×), driver interface(×), "2개만 노출하면 안전"(×), bytes/4 절약 주장(×), prompt caching=창 절약(×).*
