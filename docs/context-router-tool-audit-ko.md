# Context Router 도구 감사·효과성 검증

> 문서 상태: 분석 (설계서 M0 착수 전 검증)
> 작성일: 2026-07-17
> 목적: ① 이 MCP가 실제로 context/context-window를 절약하는지 외부 증거로 검증 ② 구현할 도구 전수 나열 + "10개 전부 구현 vs 차별화 집중" 판정
> 입력: 참조 구현 62일 코퍼스 실측(ADR-0007/0008), HANDOFF §9~10, 비전 제안서 §8 리서치, 2026-07-17 웹 증거 조사 2건

## 1. 결론 요약

- **Q-A. 정말 절약하는가?** — **조건부 참.** window ledger 절약은 구조적으로 참이지만 네 가지 조건(첫 유입 불가피 · retrieval 품질 · 소형 고정비 · **cache 보존**)이 붙고, token ledger 절약은 워크로드 의존이다(§4). 외부 증거의 핵심 경고 둘: **retrieval이 낮으면 전부 역효과**(제3자 재현 34–64% 사례), **lossy 요약은 실측으로 해롭다**(trajectory +15%) — 즉 우리의 byte-exact 노선과 retrieval 품질 투자가 정확히 옳은 방향이다. 포지셔닝은 비용·재현성 중심(품질 이득은 모델 세대마다 축소 중). "절약하는가?"가 아니라 "어떤 조건에서 절약하는가"가 옳은 질문이고, 그 조건의 성문화가 비전 제안서 §5.3 라우팅 표다.
- **Q-B. 10개 전부 구현이 맞는가?** — **아니오.** 감사 결과(§2), oracle 10개 중 v0.0.1에서 사용자 가치를 내는 것은 5개뿐이고, 반대로 우리 목표(G4 byte-exact 회수)에 필수인 도구 1개(fetch)가 oracle 계약에 **없다**. 계약 재정의를 제안한다(§3, 결정 D10).

## 2. 도구 인벤토리 전수 감사

### 2.1 oracle 10개 — 62일 실측 기반 판정

| oracle 도구 | 실측 근거 (62일 코퍼스) | 판정 | v0.0.1 처분 |
|---|---|---|---|
| `ctx_search` | 275 호출 × 5.05 쿼리 — 인덱스가 실제로 소비됨 | **핵심 가치 실증** | `ctr_search`로 승계 (기본 ON) |
| `ctx_execute` | 3,041 호출 중 37%가 shell(=Bash보다 나쁜 포장), `intent` 활용 4.3%, 볼륨 80%가 강제 채널 주입. 실가치 = "큰 원문→작은 집계" | 가치의 본질은 **변환**이지 실행이 아님 | 집계 가치는 `ctr_transform`이 승계. 실행 3종은 **구현째 v0.2 이전** (§3) |
| `ctx_execute_file` | execute의 파일 입력 변형 | `ctr_index`(수집)+`ctr_transform`(변환) 조합이 동일 가치 커버 | v0.2에서 독립 구현 필요성 재평가 |
| `ctx_batch_execute` | 편의 래퍼; 호스트가 병렬 tool call 지원 | 독립 가치 낮음 | v0.2 재평가 |
| `ctx_index` | R1 패시브 색인의 수동 대응물; v0.0.1에서 저장소를 채우는 유일 경로 | 필수 | `ctr_index` (옵트인 `--enable ingest`) |
| `ctx_fetch_and_index` | BENCHMARK: 문서 워크플로 82~96% 실증 (Context7/웹) | 실증된 사용처 | `ctr_fetch_and_index` (옵트인 `--enable net`, SSRF 정책 필수) |
| `ctx_stats` | 산식 과대계상으로 `차단` 판정 이력 | 재설계 필수 | CLI `stats` — two-ledger + 인과 A/B (D6·D9 확정) |
| `ctx_doctor` | 저비용 진단 | 유지 | CLI `doctor` |
| `ctx_upgrade` | 자기 갱신 = 공급망·파일 잠금 표면 | 축소 | CLI `upgrade` — v0.0.1은 최소형(신규 릴리스 확인+안내), self-replace는 후속 |
| `ctx_purge` | 비가역 삭제 | 유지하되 격리 | CLI `purge` — 이중 확인 (D1 확정) |

(`ctx_insight`: 62일간 0 호출로 upstream에서 이미 제거 — 승계 대상 아님.)

### 2.2 확정 인벤토리 제안 — 버전별 전체 목록

**v0.0.1 — MCP (`ctr` 등록, 도구 5)**

| 도구 | 상태 | 역할 |
|---|---|---|
| `ctr_search` | 계약 승계 | 현재 프로젝트 FTS 검색 (read-only, provenance·stale 표시) |
| `ctr_fetch` | **신규** (설계 기준서 Part A `context_fetch` 승계) | artifact/청크의 **byte-exact 범위 회수** — G4의 실현 수단. oracle 계약에 없던 공백 |
| `ctr_transform` | **신규** (D2 확정) | 저장된 artifact 대상 hermetic starlark 변환 (고정 연산 내장 함수) |
| `ctr_index` | 계약 승계 | 수동 수집 (옵트인 플래그) |
| `ctr_fetch_and_index` | 계약 승계 | 웹 문서 수집 (옵트인 플래그) |

**v0.0.1 — 별도 등록 (`ctr-global`, 선택 설치)**: `ctr_global_search` (read-only, 프로젝트 allowlist)

**v0.0.1 — CLI (모델 호출 불가)**: `doctor` · `stats`(two-ledger) · `purge`(이중 확인) · `upgrade`(최소형)

**v0.1 — 세션 연속성 (설계 기준서 Part A 승계)**: `ctr_record_event` · `ctr_session_summary` · `ctr_export_events` (+ `artifact://`/`session://` MCP resources 노출 여부는 호스트 지원 조사 결과로 결정)

**v0.2 — 실행·강제 채널**: `ctr_execute` (실질 격리 + §8.3 계약 + 호스트 ask/prompt), `ctr_execute_file`·`ctr_batch_execute`(재평가), Shadow Recall 훅(도구 아님 — Claude Code PostToolUse/PreToolUse), exec 프로필 노출 (D3 확정)

**영구 제외**: `ctx_insight` 상당물, 질의용 CLI(기능 파편화 금지), MCP 노출형 purge/upgrade

### 2.3 기본 노출 표면 요약

기본 `tools/list` = **3개** (`ctr_search`, `ctr_fetch`, `ctr_transform`) — 참조 구현의 "always-load 2개" 검증치와 같은 자릿수. Codex 호스트(도구 deferral 없음)에서도 부담 없는 크기.

## 3. 계약 재정의 제안 (결정 D10)

**현행 확정**: "`ctx_insight` 제외 10개 기능 = v0.0.1 계약, 합의된 전체 범위 완료 전 태그 금지" (HANDOFF §2)

**변경 제안**: v0.0.1 계약을 §2.2 인벤토리로 재정의한다.

근거:

1. **노출 없는 구현은 v0.0.1 가치가 0이다.** exec 3종은 D3 확정으로 v0.0.1에서 MCP 등록 경로가 없다. 그런데 구현 비용은 코드베이스 최대(스트리밍 캡처, 프로세스 트리 제어, env allowlist, OS별 셸 계약)다. 사용자 가치 없는 최대 비용 컴포넌트를 첫 릴리스에 넣는 것은 ponytail 원칙과 정면 충돌한다.
2. **oracle 계약이 우리 목표와 어긋나 있다.** G4(byte-exact 회수)는 이 제품의 핵심 차별점인데 oracle 10개에는 fetch가 없다. "10개 동등"을 완주하는 것보다 목표 정합적인 표면을 완성하는 것이 옳다.
3. **태그 규율의 의도는 유지된다.** 원 확정의 의도는 "반쪽 출시 금지"다. 재정의된 범위(§2.2 v0.0.1 전체)를 완료하기 전에는 태그하지 않는다 — 규율 자체는 그대로다.
4. **oracle 호환성도 유지된다.** golden fixture는 승계 도구 5종+CLI에 대해 이름 매핑(`ctx_*`→`ctr_*`)으로 작성하고, exec fixtures는 구현과 함께 v0.2로 이동한다.

파급: HANDOFF의 `확정` 1건 변경이므로 사용자 명시 승인 필요. 승인 시 설계서(M0)는 §2.2 인벤토리를 계약으로 삼는다.

## 4. 효과성 검증

### 4.1 내부 실측이 이미 말하는 것

**window ledger — 구조적으로 참, 조건 3개:**

1. **첫 유입은 절약할 수 없다.** 모델이 추론에 써야 하는 정보는 창에 들어와야 한다(정보이론적 하한). 절약 대상은 ① 반복 유입(62일 실측: 세션 내 58.2MB + 교차 세션 38.8MB — 참조 구현 ADR-0008, Read 총량 88.4MB 대비 지배적), ② 불필요 유입(대형 원문 중 실제 필요 비율이 낮은 부분), ③ 스키마/고정비다.
2. **recall precision이 낮으면 역효과.** 잘못된 회수 → 재시도 → 오히려 호출·토큰 증가 (Codex 자문 1·2 공통 지적). 검색 품질이 절약의 전제 조건.
3. **소형 원문은 고정비가 이득을 초과.** 2KB 이하 native 우세 (HANDOFF §9.4 — 실험 시작점).
4. **prompt cache를 깨면 총비용이 역전될 수 있다.** window 절약과 cache 보존은 별개 축 — 주입·회수는 append-only로 설계하고, cache-hit 반영 청구 토큰을 별도 계측한다 (§4.2 OpenHands 사례).

**token ledger — 워크로드 의존:** PTC 공식 수치도 다회 호출 워크로드 37% 절감 vs 1~2회 호출 -8%. 스폰 고정비 53.4K tok 사례처럼 "window 도구"가 token 손해일 수 있음 — two-ledger 분리(G6)가 그래서 정책이다.

**품질 채널:** window 절약의 실익이 비용이 아니라 **정확도 보존**일 가능성 — 외부 증거로 검증(§4.2).

### 4.2 외부 증거 (2026-07-17 웹 조사, 서브에이전트 수집)

**지지 증거:**

- **MCP tax 실재**: 도구 정의당 ~550–1,400 tok, 세션 시작 전 55K tok 소모 실사례 (MCP issue #2808, 2025–2026 커뮤니티 실측). → 오프로드 대상이 실재하되, ctr 자신의 도구 정의도 tax — 기본 3-도구 표면이 옳다.
- **긴 컨텍스트 품질 저하**: Chroma "Context Rot"(18개 frontier 모델, 한계 훨씬 전 30–50% 하락, distractor가 능동적 오답 유발), NoLiMa(128K 주장 12개 모델 중 10개가 **32K에서 이미** 단문 baseline의 50% 미만). → lean window의 정확도 근거.
- **lossy 요약의 역효과 — byte-exact 노선의 직접 근거**: JetBrains "Complexity Trap"(arXiv 2508.21433): SWE-bench에서 observation masking은 비용 -50%↑에 solve rate 동등~+2.6%인 반면, **LLM 요약은 실패 신호를 지워 trajectory를 +15% 연장**. → 무손실 fetch가 요약형 memory보다 안전하다는 실측.
- **retrieval > full-context**: Cursor semantic retrieval이 전 frontier 모델에서 agent 정확도 평균 +12.5%; Aider는 무관 컨텍스트가 편집 실패까지 유발한다고 명시.

**경고 증거 (설계 제약으로 수용):**

- **retrieval 품질이 병목**: 제3자 재현에서 Anthropic Tool Search의 retrieval/selection 정확도는 34–64% 수준(Arcade 4,027-tool 테스트: regex 56%/BM25 64%; Stacklok head-to-head: selection 34% vs hybrid 94%). → **못 찾으면 전부 역효과.** ctr_search는 자체 retrieval 평가(recall@k)를 상시 지표로 가져야 하고, FTS(porter+trigram+BM25) 이후 semantic 보강 여지를 열어둔다.
- **prompt cache 무효화가 총비용을 역전**: OpenHands condenser는 window·latency를 줄이고도 cache 손실로 총비용 +$40. → **append-only 주입 원칙**: recall/훅 주입은 대화 append 경로로만, 이력 재작성 금지. cache-hit 반영 실청구 토큰을 별도 계측.
- **훅 자동화의 상시 비용**: claude-mem 필드 리포트 — 세션 시작 토큰 40% 증발, background observer 3시간 $17+ (#618, #1742, #1848). → Shadow Recall(v0.2)은 자체 오버헤드를 절약분에서 차감해 보고.
- **품질 이득은 축소 중인 베팅**: 최상위 모델은 192K까지 유지(fiction.liveBench), "80% 정확도 도달 입력 길이"가 9개월간 250배 증가(Epoch AI). → 포지셔닝은 **비용·재현성 중심**, 품질 이득은 보너스로만 주장.
- **시장 수치 불신 재확인**: mem0/Zep의 LoCoMo 벤치 수치가 산술 오류·재실행 불일치로 붕괴(2026-05 검증 에세이); 벤더 수치(85/37/98.7%)는 비공개 데이터셋 best-case. → 정직한 회계(G6) 포지션 강화.

**측정 설계 추가 항목 (설계서 반영):** ① retrieval recall@k 독립 지표, ② window 토큰과 cache-hit 반영 실청구 토큰 분리 계측, ③ trajectory 길이/turn 수를 품질 부작용 지표로, ④ 훅·background 오버헤드를 절약분에서 차감, ⑤ char/4 휴리스틱 금지 — tokenizer 실측.

### 4.3 시장 검증 — 도구별 선례·공백 (2026-07-17 웹 조사, 서브에이전트 수집)

**도구별 판정** (table-stakes = 있어야 하지만 차별화 아님):

| 후보 | 가장 가까운 선례 | 판정 |
|---|---|---|
| `ctr_search` | claude-mem(SQLite+FTS5, 87.6k★), mcp-memory-service, cognee | table-stakes — 없으면 감점, 있어도 차별화 아님 |
| `ctr_fetch` | 공식 fetch 서버의 start_index는 **라이브 재페치**용; claude-mem은 ID 단위 whole-chunk만 | **차별화 — 공백 (a) 확인** |
| `ctr_transform` | mcp-run-python(WASM, 저장소 바인딩 없음), Bifrost Starlark "Code Mode"(게이트웨이 오케스트레이션) | **차별화(조건부)** — "artifact-bound hermetic" 조합 미발견, 단 업계 방향이라 시간차 우위 |
| `ctr_index` | mcp-local-rag, basic-memory 등 | table-stakes |
| `ctr_fetch_and_index` | 공식 fetch(저장 없음), graphlit | table-stakes — "저장 후 range 재열람"과 묶일 때만 가치 |
| v0.1 세션 복구 | **혼잡 시장**: claude-mem(87.6k★), precompact-hook, FlineDev/Recall, JSONL 파싱 복구 훅 | 차별화는 **exactness뿐** — 아래 (d) 참조 |
| v0.2 exec+Shadow Recall | Anthropic advanced tool use, claude-mem 자동 캡처 | 차별화(리스크: 호스트 네이티브 기능화 가능성) |

**시장 공백 4건 검증 결과:**

- (a) 저장된 출력의 byte/line-range fetch — **확인됨** (다중 표현 검색에서 미발견).
- (b) 저장된 출력 위 hermetic transform — **확인됨(조건부)** — 시간차 우위로 취급.
- (c) provider usage로 검증된 절감 수치 — **확인됨(아무도 안 함)**. 전원이 자체 벤치 추정치("~10x", "100x", "92%", "98.7%"). 반면 **Claude Code는 이미 OTel로 실측 token/cost 메트릭을 노출** — 검증 인프라는 있는데 쓰는 벤더가 0. 저비용·고신뢰 차별화 기회.
- (d) 구조화 이벤트 기반 compaction 복구 — **부분 반증**. "LLM 요약이 아닌 복구"는 이미 존재(JSONL transcript 파싱 훅 등). 미점유는 **"이벤트 + byte-exact artifact 저장소 + FTS의 결합"**뿐 → v0.1 피치를 "구조화 이벤트"가 아니라 **"무손실 artifact 복원"**으로 좁힌다.

**Resources vs Tools 판정 — v0.0.1은 tool 전용:** Codex CLI는 resources 미지원(tools+instructions만; resources/list 프로빙 버그 이력도 있음), Claude Code는 `@server:uri` 멘션은 되지만 resource template(동적 URI) 미지원 — `artifact://{id}?range=` 패턴 불가. 모델이 자율 호출하는 회수 경로는 tool이 유일하게 전 호스트 호환. static resource 노출은 수요 확인 후 optional.

**이름 충돌 재확인:** MCP registry에 `ctr` 서버명/접두사 충돌 없음. containerd `ctr`(8)과의 충돌은 바이너리명에만 해당 — 바이너리 `context-router` 유지(D4 확정)로 이미 회피.

**신규 기회 3건:** ① **OTel 연동 검증형 절감 리포팅** — `stats`가 Claude Code OTel 실측 토큰과 대조한 "billed-token verified savings" 산출(공백 (c)의 실행안, G6 완성). ② **Trajectory replay/audit** — v0.1 이벤트+exact artifact의 자연 확장(2026 관측성 시장 시그니처). ③ **Context budgeting 정합** — `ctr_search`/`ctr_fetch`에 token budget 파라미터(설계 기준서의 ContextBudget 승계) → 호스트 context editing과 경쟁이 아닌 보완 포지션.

## 5. context-router가 내세울 것 (차별화 스택)

시장 공백 검증(§4.3)과 결합한 차별화 우선순위:

| # | 차별화 기능 | 버전 | 근거 |
|---|---|---|---|
| 1 | **byte-exact 회수** — search+fetch, provenance·staleness 포함 | v0.0.1 | 시장 전체가 lossy 요약(claude-mem 계열) 또는 스키마 절감만 공격 |
| 2 | **hermetic transform** — 저장된 바이트 위의 starlark 순수 변환 = "PTC를 로컬에서" | v0.0.1 | 경쟁 샌드박스는 전부 microVM/컨테이너/SaaS 의존; 임베디드 순수 변환 선례 없음 |
| 3 | **per-project 격리 저장 + 명시적 global** | v0.0.1 | 메모리 제품 대부분 global user-scope; per-project SQLite 선례는 ConPort뿐(그마저 지식 뱅크) |
| 4 | **정직한 회계** — two-ledger + task 단위 무작위 A/B + **Claude Code OTel 실측 대조**("billed-token verified savings" — 업계 유일 기회, §4.3 공백 (c)) | v0.0.1 기반, v0.2 완성 | 시장 전체가 검증 불가 수치 경쟁("95%+", "4-32x", 벤치 붕괴 사례까지) — 신뢰 격차 자체가 포지션 |
| 5 | **무손실 세션 복구** — 이벤트 + byte-exact artifact + FTS 결합 (피치를 "구조화 이벤트"가 아니라 **"무손실 복원"**으로 좁힘 — §4.3 (d) 부분 반증 반영; replay/audit 확장 여지) | v0.1 | 복구 시장은 혼잡(claude-mem 87.6k★)하나 무손실 결합은 미점유; lossy 요약의 해악은 실측됨(trajectory +15%, §4.2) |
| 6 | **Shadow Recall 강제 채널** — 채택률 무관 절약 | v0.2 | 설득 무효(3.82%) 실측에 대한 유일하게 정직한 응답; 훅 통합형 recall 주입 선례 없음 |
| 7 | **단일 Go 바이너리, 런타임 의존 0, Claude Code+Codex 양호스트** | v0.0.1 | 경쟁은 TS/Node·Python·Docker 스택 |

핵심 프레임: **"도구 대체가 아니라 출력 가상화"** (비전 제안서 §5.3) — native 도구와 경쟁하지 않고, native가 만드는 바이트의 창 재유입을 차단한다.

시장 검증의 한 줄 요약: **search/index/수집은 입장료, `ctr_fetch` + `ctr_transform` + 검증된 절감 수치의 3종 결합이 차별화의 본체다.** 설계 반영 사항: `ctr_search`/`ctr_fetch`에 token budget 파라미터 포함 검토(§4.3 기회 ③), v0.0.1은 MCP tool 전용(resources는 호스트 지원 미성숙).

## 6. 결정 요청

| ID | 결정 | 권장 |
|---|---|---|
| D10 | v0.0.1 계약을 "oracle 10개 동등"에서 **§2.2 인벤토리**(코어 5 MCP + global + CLI 4, exec 3종 구현째 v0.2)로 재정의 | **확정 (2026-07-17)** — 사용자 승인. 설계서(M0)는 §2.2 인벤토리를 계약으로 삼고, 태그 규율(재정의 범위 완료 전 태그 금지)은 그대로 적용 |
