# Context Router v0.2 설계서 (강제 채널 — Claude Code 훅 패키징)

- 작성: 2026-07-20 (session-09). 정본 계약은 본 문서 + v0.1 설계서 + v0.0.1 설계서의 승계 조항.
- 전제: v0.1.0 태그(972ad10), 실호스트 스모크 GREEN(stdio+codex), main @ 334442b.
- 스코프 확정 근거: session-08 §5 브레인스토밍 → session-09 사용자 5답
  (옵션 A + exec 분리 / Claude Code 먼저 / 수동 측정 먼저 / 계측 9종 전부 /
  spill journal v0.3 이월 + drop 카운터).

## 0. 결정 이력 (v0.2 신규 — D20까지는 v0.1 설계서·vision-proposal)

- **D21** v0.2 범위 = "강제 채널"(옵션 A). exec 3종은 별도 트랙으로 분리 — vision §7 로드맵 개정.
- **D22** 훅 세션 식별: 호스트 `session_id`(canonical UUID 형식 검증)를 `cc:` 네임스페이스로 채택. §2.4의 "입력 금지"는 model-plane(MCP 도구) 한정으로 문면 개정. 세션 생성은 SessionStart만, 미지 세션 이벤트는 drop. 훅 전용 append API 신설(MCP 비노출).
- **D23** 훅 fail-open: 호스트를 절대 막지 않음. 기록 불가·deadline 초과 시 exit 0 + drops 사이드카 — 훅 전용 시간 예산이 모든 DB 대기·재시도를 상계(§2.3). spill journal은 drop 데이터가 필요성을 입증할 때 v0.3에서.
- **D24** 자동 계측 이벤트 9종 확정: 기준서 §6.2 초기 15종 − 모델 기록 6종. 훅→이벤트 매핑표(§3)가 생산 계약. summary·payload는 allowlist 조립(비밀 운반 차단).
- **D25** large-read guard: PreToolUse(Read) — 워크스페이스 내 전체-파일 Read가 임계 초과이고 **현장 인덱싱 성공을 확인한 경우에만** deny + 대체 경로 안내. 실패·경계 밖·부분 읽기는 통과.
- **D26** Shadow Recall: PostToolUse 대용량 출력의 패시브 인덱싱 — 전용 하드 캡·denylist 대조·바이너리 판정을 명시 구현(무검사 인라인 경로에 기대지 않음). 동의 경계는 `hook install` 행위 자체(+`--no-shadow` 옵트아웃). MCP 표면의 ingest 프로필 게이트는 별도 장치로 유지.
- **D27** 측정은 수동 먼저: `ctr usage`(로컬 transcript 집계) + 수동 A/B 프로토콜. v0.2 exit gate 문면을 "수동 측정 결과 기록"으로 완화(vision §7 개정). 무작위 하네스·OTel은 신호 확인 후.
- **D28** 패키징: `context-router hook install`의 settings.json 멱등 병합(원자 쓰기·타 도구 항목 보존). plugin manifest·Codex 훅은 이월.

## 1. v0.2 제품 계약

### 1.1 범위

- `context-router hook` 진입점(단명 프로세스, stdin JSON 1건 처리) + `context-router hook install/uninstall`.
- 자동 계측 이벤트 9종: `tool_call` `tool_result_summary` `artifact_created` `file_edit` `git_diff` `build_run` `test_run` `error` `warning`.
- Shadow Recall 패시브 인덱싱, large-read guard.
- `ctr usage` 수동 측정 리포트 + 수동 A/B 프로토콜 문서.
- doctor 확장(훅 등록 상태·drops 보고) + 항목 순서 nit 수정.
- 부채 편승 10건(§9).

### 1.2 명시적 비범위 (v0.2)

- exec 3종 — 별도 트랙(D21, 로드맵 개정). OS 격리 설계가 무겁고 직교적.
- Codex 훅 — Claude Code 훅 계약 안정 후(D28).
- 무작위 A/B 하네스·OTel 어댑터 — 수동 측정 신호 확인 후(D27).
- spill journal — v0.3 후보(D23). v0.2는 drop 카운터로 필요성 데이터를 수집.
- `user_instruction`/`compaction_summary` 등 모델 기록 6종의 자동 수집 — 모델 기록 영역 침범 + 프라이버시 표면(9종 밖).
- Bash `cat`·Grep 대용량 출력 가드 — 명령 휴리스틱이 취약, v0.2.x 검토(§4).
- `repository{}` 기입, `invalidates`, payload 필드 조회, title dedup — v0.1 §11에서 이월된 채로 유지(§12).

### 1.3 선행 조건·검증 항목

- v0.1.0 코드 기반(main 334442b). 스키마 변경 없음 — event_type은 이미 자유 문자열(≤64B `[a-z0-9_]+`), 상한(summary ≤2048B, 이벤트 ≤8KB)도 승계.
- **[plan 검증]** Claude Code 훅 현행 문서 확인 5건: ① PostToolUse가 도구 실패 시에도 발화하는지 + `tool_response` 오류 신호의 실제 형태, ② PreToolUse `permissionDecision` JSON 스키마 현행, ③ SessionStart payload(`source` 등), ④ settings.json `hooks` 스키마, ⑤ PostToolUse stdin `tool_response`의 최대 크기·절단 정책 실측(Shadow Recall의 "전문 확보" 전제 검증). 훅 stdin 골든 픽스처는 확인된 실페이로드 형태로 고정.

## 2. 훅 아키텍처

### 2.1 진입점·프로세스 모델

- Claude Code가 훅 이벤트마다 `context-router hook`(같은 단일 바이너리의 서브커맨드)을 stdin JSON으로 호출. 처리 1건 후 즉시 종료. PATH 실행 파일명은 `context-router`다 — `ctr`은 MCP 등록 키일 뿐 실행 파일이 아니다(doctor 스니펫 §9와 동일).
- MCP 서버 경유 없음. 단 **현행 `session.Open()`은 재사용 불가**(항상 자체 UUIDv7 발급 + `sessions` 평문 INSERT + `session_start` 자동 append): **훅 전용 append API를 internal/session에 신설**한다 — `ExternalSessionID` 옵션, `sessions` INSERT OR IGNORE, `session_start` 조건부 append(§2.2), quick_check 생략(§2.3), MCP 도구 표면 비노출. 마일스톤 ②의 명시 산출물. 다중 프로세스 동시 append 자체는 v0.1 §2.1 지원 토폴로지(shared+shared lease 공존은 store_lock 계약으로 확인됨).
- 신규 코드는 `internal/hook/hook.go`(+`hook_test.go`) 1~2파일(D13 선호 밴드), `internal/cli`가 디스패치. mcp 패키지의 import 금지 규율(database/sql·net/http·os/exec)은 hook에는 해당 없음 — hook은 cli 평면.
- 종료 코드는 항상 0(fail-open, §2.3). PreToolUse의 차단은 exit code가 아니라 stdout JSON `permissionDecision`으로만 표현(§4). 진단은 stderr slog.

### 2.2 세션 식별 (v0.1 §2.4 개정 — D22)

- 훅 stdin의 호스트 `session_id`는 **canonical UUID 형식 검증 후** `cc:<uuid>`로 채택(불합격 = 해당 이벤트 전체 drop). 접두사는 출처 표기이자 서버 발급 UUIDv7 공간과의 구분(향후 `cx:` = Codex).
- §2.4 문면 개정: "session_id는 입력으로 받지 않는다"는 **model-plane(MCP 도구 인자) 한정**으로 유지. 훅 stdin은 **비신뢰 입력으로 분류하되 수용**한다 — 공개 CLI라 위조 가능(모델도 Bash로 호출할 수 있다)하지만, 로컬 단일 사용자 위협 모델에서 session.db 파일 자체가 같은 권한으로 쓰기 가능하므로 출처 인증(IPC/capability)은 비목표다. 방어는 형식 검증 + 아래 생성 규칙 + 소비 측 untrusted 표시(v0.1 §4 승계)로 한정.
- 세션 생성은 **SessionStart 훅만**: `sessions` 행 INSERT OR IGNORE(started_at·producer `context-router/<ver>`·retention §아래) + 행이 **신규 삽입된 경우에만** `session_start` 이벤트 append(payload: source, worktree root) — clear/compact로 SessionStart가 재발화해도 중복 session_start를 만들지 않는다. **미지 세션의 후속 이벤트는 drop**(drops 기록 — 설치 중간 합류는 다음 세션부터 계측). 위조·오염 표면 축소.
- 훅 세션 기본 retention: `retention_sec` 기본 30일(`CTR_HOOK_RETENTION_SEC`로 조정, 0=무기한) — 패시브 수집은 모델의 선별 기록과 달리 무기한 누적의 근거가 없다(볼륨 대책, §3과 연동).
- MCP 서버 스트림(자체 UUIDv7)과 훅 스트림(`cc:`)은 같은 논리 세션이라도 session_id가 다른 채 공존. `ctr_session_summary`/`ctr_export_events` 기본 범위가 worktree 전체라 복원 흐름은 두 스트림을 함께 읽는다 — 스트림 통합은 **비목표**(stdio에 프로토콜 세션 식별자가 없어 서버 쪽에서 원리적으로 불가).

### 2.3 fail-open 계약 (D23)

- session.db 사용 불가(fail-closed 손상 상태·락 경합 초과·스키마 불일치 등) 시: 즉시 exit 0, store-root의 session.db 옆 `session.drops.log`에 1줄 append(`<unix-ts>\t<사유>`, O_APPEND 단일 write). doctor가 존재·건수를 항목으로 보고. **한계 명시**: store-root 전체 불용(디스크 풀·읽기전용·소실)이면 사이드카 기록도 함께 실패해 손실이 미집계된다 — spill journal(v0.3) 판정 시 이 과소집계 편향을 감안하고, doctor는 사이드카 기록 가능 여부를 별도 신호로 보고.
- **훅 전용 deadline 예산이 fail-open을 강제한다**: 현행 세션 경로의 대기 합(busy_timeout 5s + txRetry 재시도 + store open-lock 최대 5s + 매 Open quick_check 전 DB 스캔)은 훅 timeout 10s를 넘길 수 있고, 호스트가 프로세스를 kill하면 exit 0도 drops 기록도 없는 **무관측 유실**이 된다. 따라서 훅 경로는 총 예산 기본 2s(`CTR_HOOK_DEADLINE_MS`)의 context deadline을 모든 open/append/인덱싱에 전파하고, busy_timeout·재시도 합을 예산 내로 하향하며, `PRAGMA quick_check`는 생략한다(recover 마커 stat 확인은 유지해 fail-closed 의미 보존 — 손상은 append 시점 오류 분류로 사후 감지). deadline 초과 시 drops 기록 후 exit 0. 지연·경합 주입 시의 예산 준수·drops 기록은 결정론적 테스트로 게이트(§10).
- `CTR_HOOKS_OFF=1`: stdin 소비 후 즉시 exit 0 — 수동 A/B의 off 토글이자 비상 탈출구.
- 시간 예산 목표: 정상 append ≤200ms, Shadow Recall 인덱싱 포함 시에도 deadline 2s 안 — 초과 위험 크기는 §5의 캡으로 사전 차단.

## 3. 자동 계측 매핑 (D24)

| Claude Code 훅 | 조건 | event_type |
|---|---|---|
| SessionStart | — | `session_start` (+ sessions 행) |
| PostToolUse | 기본 | `tool_call` |
| PostToolUse | Write/Edit/NotebookEdit | `file_edit` |
| PostToolUse | Bash: git diff/commit/merge 계열 | `git_diff` |
| PostToolUse | Bash: 빌드 명령 패턴표 | `build_run` |
| PostToolUse | Bash: 테스트 명령 패턴표 | `test_run` |
| PostToolUse | tool_response 오류 신호 | `error` |
| PostToolUse | Shadow Recall 임계 초과로 인덱스됨 | `tool_result_summary` (기본 이벤트에 추가, artifact ref 포함) |
| (Shadow Recall 저장 시) | content.db 아티팩트 생성 | `artifact_created` |
| (guard 발화 시) | Read deny | `warning` |

- 분류 우선순위: `error` > `git_diff`/`build_run`/`test_run`/`file_edit` > `tool_call`. 1 호출 = 기본 이벤트 1건(+조건부 `tool_result_summary`·`artifact_created`).
- Bash 분류는 선언적 패턴표(정규식 슬라이스, 테이블 테스트로 고정): git 계열(`git diff` `git commit` `git merge` `git rebase` 등), 빌드(`go build` `dotnet build` `npm run build` `msbuild` 등), 테스트(`go test` `dotnet test` `pytest` `vitest` `npm test` 등). 미매치는 `tool_call`.
- **summary·payload는 allowlist 조립 계약**(summary는 FTS 색인 대상이라 비밀 운반 차단이 1차 방어): summary = `<도구명>: <허용 요소>`로만 조립한다. 허용 요소 — ① Bash: 분류 결과 + 명령 첫 토큰이 **명령 단어 형태(`[A-Za-z0-9_./-]+`)일 때만**(env 할당 `KEY=값`·비정형 토큰은 `<arg>`로 마스킹), ② 파일 도구: 워크스페이스 상대 경로, ③ 공통: 종료코드·바이트 크기·매치 패턴명. 원시 인자 전문·오류 문구 전문·tool_response 본문은 summary에 넣지 않는다(오류는 정규화된 분류·코드로). payload(attributes)도 같은 allowlist 필드만. 조립 후 ingest `Redact()`를 2차 방어로 통과시키고 redaction 상태 기록. 비밀 문자열의 FTS 미회수를 canary 테스트로 게이트(§10). 기존 상한(≤2048B) 안에서 절단.
- 볼륨 억제: 기존 이벤트 상한 + 훅 세션 기본 retention 30일(§2.2)이 담당 — 별도 카운트 캡은 두지 않는다(drop 카운터·실측으로 재평가).
- 어휘는 여전히 스키마 비강제(§26 미지 타입 보존 의무 승계). v0.1 §2.2의 "권장 어휘" 목록에 9종을 추가하고, `warning`의 1급 event_type 승격은 v0.1 §2.2 "warning은 error payload로" 문면의 **명시 개정**임을 함께 기록.

## 4. large-read guard (D25)

- PreToolUse(matcher: Read) — **deny는 다음 4조건이 모두 성립할 때만**: ① 대상이 워크스페이스 경계 내(projectRoot+allowPaths — 경계 밖은 ingest가 `ErrWorkspace`로 색인 불가하므로 통과), ② 전체-파일 읽기(offset/limit으로 임계 미만 범위를 지정한 부분 읽기는 통과), ③ 파일 크기 > 임계(기본 256KiB, `CTR_GUARD_READ_MAX`), ④ 현장 인덱싱의 **성공 확인**(`Indexed==1` — denylist·oversize 파일은 ingest가 오류 없이 `Skipped`로 돌려주므로 err 검사만으로는 부족하다). 성립 시 stdout JSON `permissionDecision: "deny"` + "이미 인덱스됨 — `ctr_search`로 검색, `ctr_fetch`로 바이트 정확 조회" 안내.
- ①~④ 중 하나라도 불성립이면 **allow 통과** — 색인되지 않은 파일을 막으면 Read도 `ctr_fetch`도 불가능한 접근 사각지대가 된다(deny 사유의 "이미 인덱스됨"은 실제 색인 성공 시에만 참). 시나리오(경계 밖·denylist·oversize·부분 읽기 = allow / 정상 대형 파일 = deny)는 §10 게이트.
- 발화 시 `warning` 이벤트 기록(차단 파일·크기·안내 요지) — 가드 활동이 세션 DB에서 측정 가능. 이벤트 기록 실패는 deny 판정에 영향 없음(fail-open은 기록 경로에만 적용 — 가드 판정은 DB 없이 성립).
- deny는 모델에 피드백되는 소프트 강제다. 사용자 복귀 경로: `CTR_HOOKS_OFF`, 임계 상향, `context-router hook uninstall`.
- Bash `cat`/Grep 출력 가드는 비범위(§1.2) — 명령 문자열 휴리스틱의 오탐 비용이 이득을 넘는다. Shadow Recall(§5)이 출력 측을 사후 보완.

## 5. Shadow Recall 패시브 인덱싱 (D26)

- PostToolUse에서 `tool_response` 직렬화 크기 > 임계(기본 16KiB, `CTR_SHADOW_MIN`) → 전문을 content.db 아티팩트로 저장: provenance=hook, 콘텐츠 해시 dedup, `Redact()` 통과. **주의: 인라인 ingest 경로(runInline)는 denylist·바이너리 sniff·크기 캡을 적용하지 않는 무검사 경로다**(코드 주석이 "필요 시 여기서 추가"로 예고) — Shadow Recall은 이에 기대지 않고 자체 방어를 명시 구현한다: **하드 캡 `CTR_SHADOW_MAX`(기본 1MiB)를 직렬화 전에 적용**(초과 = 미저장 + drops `shadow-oversize`), **파일 유래 응답(Read 등)은 tool_input의 원본 경로를 ingest denylist에 대조**(비밀 파일 출력의 파일명 우회 색인 차단), 바이너리 판정 후 비텍스트 미저장. 비밀 미색인은 canary 테스트로 게이트(§10).
- 저장 시 `artifact_created` 이벤트 + 기본 이벤트에 더해 `tool_result_summary` 이벤트(artifact ref, 결과 요지)를 append.
- 가치 축은 **컴팩션·다음 세션 후의 재소환**이다 — PostToolUse 시점에 출력은 이미 모델 컨텍스트에 들어가 있으므로, 이 채널은 현재 턴 절약이 아니라 재독(re-read) 절약을 만든다. 현재 턴 절약은 §4 guard와 기존 MCP 자발 경로의 몫.
- content.db 다중 프로세스 쓰기(훅 프로세스 vs MCP 서버): WAL + writer 1연결 + txRetry의 기존 규율(v0.0.1 §3.5)로 지원. 동시 쓰기 테스트를 게이트에 포함(§10).
- 동의 경계: **`hook install` 행위 자체가 패시브 인덱싱의 명시적 opt-in**이다(가드만 원하면 `install --no-shadow` 또는 `CTR_SHADOW_OFF=1`). MCP 표면 `ctr_index`/`ctr_fetch_and_index`의 프로필 게이트(opt-in)는 별도 장치로 그대로 유지 — 그쪽은 모델 주도 수집을 묻는 게이트다(D26). 호스트가 훅을 발화했다는 사실이 tool_response **내용**의 신뢰를 만들지는 않으므로, 내용 방어는 위 캡·denylist 대조·`Redact()`가 담당한다.

## 6. 측정 (D27)

- `ctr usage` CLI 신설: Claude Code 로컬 transcript JSONL(`~/.claude/projects/<프로젝트 디렉터리>/*.jsonl`)을 읽기 전용 파싱해 세션별 토큰 집계(input/output/cache)를 표로 출력. 네트워크 없음. session.db의 `cc:` 세션 존재 여부와 대조해 훅 on/off 세션을 구분 표기.
- 수동 A/B 프로토콜(문서 절차): 같은 종류의 작업을 `CTR_HOOKS_OFF` on/off로 N세션씩 수행 → `ctr usage` 비교 → 결과를 세션 기록(docs/prompts)에 남긴다. **측정 대상 분리**: 세션별 토큰 비교가 잡는 것은 large-read guard 등 **현재 턴 효과**다. Shadow Recall의 가치(컴팩션·재개 후 재독 절약)는 교차 세션 효과라 세션별 총합에 원리적으로 잡히지 않는다 — 보조 지표(재개 세션의 동일 파일 재독 횟수, `cc:` 스트림 tool_result_summary 대비 ctr_search 사용 빈도)로 따로 기록한다. **v0.2 exit gate = 이 수동 측정 결과 기록**(vision §7 게이트 문면 완화).
- 무작위화 하네스·OTel 어댑터는 수동 측정이 신호를 보이면 별도 태스크로(§12).
- **[plan 검증]** transcript JSONL의 usage 필드 형태(모델 메시지별 usage 블록)를 실파일로 확인 후 파서 계약 고정.

## 7. 패키징·설치 (D28)

- `context-router hook install [--user]`: 프로젝트 `.claude/settings.json`(기본) 또는 `--user`로 사용자 설정에 훅 등록을 멱등 병합. **병합 계약**: 기존 JSON 파싱 → 해당 훅 이벤트 배열에만 자기 항목 append/치환(식별은 명령 문자열 `context-router hook` + 버전 마커) → temp 파일 + rename **원자 쓰기**. 미지 키·타 도구의 훅 항목은 왕복 보존(보존 자체가 테스트 게이트, §10). `uninstall`이 대칭 제거. 등록 항목: SessionStart, PreToolUse(Read), PostToolUse(전 도구) + timeout 10s.
- 훅 명령은 PATH의 **`context-router`**(실행 파일명 — `ctr`은 MCP 등록 키일 뿐이다). **store-root 정합**: `.mcp.json` args의 `--store-root`는 훅 프로세스에 전파되지 않으므로(우선순위 플래그>`CTR_STORE_ROOT`>OS 기본, main 현행), 커스텀 store-root는 `CTR_STORE_ROOT` env 규약으로 통일하고 `install --store-root <p>`가 주어지면 훅 명령 args에 명시 주입한다. doctor 확장: 훅 등록 상태(설정 파일 파싱)·`context-router` PATH 해석·해석된 store-root·drops 건수를 항목으로 추가하고, 기존 항목 순서 nit([3]→[5]→[4])를 함께 정렬(§9 편승).
- Claude Code plugin manifest(마켓플레이스형)는 이월 — 로컬 우선 단일 바이너리에는 settings 병합이 최소완결. Codex 훅(`cx:`)은 Claude Code 계약 안정 후(§12).

## 8. 보안 계약 (승계 중심)

- 훅 stdin은 **비신뢰 입력**(§2.2 — 공개 CLI, 위조 가능)이고 그 **내용**(tool_input/tool_response)은 모델·외부 유래다. 저장 경로 방어: session 이벤트는 §3 allowlist 조립(원문 비수용이 1차 방어) + `Redact()` 2차 + v0.1 §4 상한, Shadow Recall은 §5의 자체 캡·denylist 대조·바이너리 판정·`Redact()`.
- 오류·로그 위생은 v0.0.1 §5.5 승계(stderr slog, 비밀·절대경로 미포함). 테스트의 secret canary 분리-리터럴 규율 승계.
- 훅이 새로 여는 네트워크 표면 없음. 파일 쓰기는 store-root(session.db·content.db·drops 사이드카) 한정.

## 9. 부채 편승 배치 (10건 + doctor nit)

- **개막 정리 태스크**(코드 인접 독립, 마일스톤 ①): filterShortTokenCandidates 파라미터화(~45줄 중복), bak family ts-orphan 정리, modernc corruption fixtures, migrateBusyRetry 분기 단위 테스트, attributes float64 캐비앗 문구, recover worktree-absent UX nit, doctor 항목 순서 정렬(§7과 연동).
- **계측 태스크에 흡수**(세션 인접): Summarize 타입 fan-out 캡, summary budget 정밀화(len(summary) 관례), EventV1 omitempty 소비자 재점검, byte-exact export golden — 훅 스트림이 만들 실제 볼륨·타입 다양성 위에서 테스트 확장으로 처리.

## 10. 테스트·수용 게이트

- 단위: Bash 분류 패턴표 테이블 테스트, 분류 우선순위, summary allowlist 조립(env 할당 마스킹 포함), fail-open 경로(락 점유 중 exit 0 + drops 1줄), **deadline 예산**(경합·지연 주입 시 예산 내 drops 기록 후 exit 0 — 결정론적), 임계 경계(guard·shadow), settings 병합 멱등성 + **타 도구 항목·미지 키 왕복 보존**.
- 통합: 훅 stdin 골든(§1.3 검증 후 실페이로드 형태로 고정), content.db 동시 쓰기(훅+서버), guard 판정 시나리오(경계 밖·denylist·oversize·부분 읽기 = allow / 정상 대형 파일 = deny + `Indexed==1` 확인) + deny stdout JSON 형식, **비밀 FTS 미회수 canary**(session summary·shadow 아티팩트 양쪽), `cc:` 세션의 summary/export 왕복(미지 세션 drop·session_start 1회 규칙 포함).
- 실호스트 스모크: 본 저장소에서 `context-router hook install` 후 실제 세션 1회 — 최소 `session_start`/`tool_call`/`file_edit`/`test_run` 관측 + doctor GREEN(훅 항목 포함). 이때 `--profile ingest` fetch 정상 경로 스모크(session-08 §4.2 이월)를 함께 소화.
- Go 테스트는 `-p 1` 메모리 캡 규율 유지. CI gofumpt·lint 기존 게이트 승계.

## 11. 마일스톤 스케치 (상세는 writing-plans)

① 개막 정리(부채 7건+doctor 순서) → ② 훅 전용 session append API(`ExternalSessionID`·INSERT OR IGNORE·조건부 session_start·quick_check 생략·deadline 수용) + `context-router hook` 골격 + 세션 식별 + fail-open →
③ 계측 매핑 9종(allowlist 조립 포함) → ④ Shadow Recall(자체 캡·denylist 대조) → ⑤ large-read guard(4조건 판정) → ⑥ `ctr usage` + install/uninstall + doctor 확장 → ⑦ 실호스트 스모크·수동 A/B 1회·기록.

## 12. 의도적 미결 (v0.3+ 후보)

- spill journal(drop 데이터로 필요성 판정 — D23), 무작위 A/B 하네스·OTel 어댑터(D27), Codex 훅 `cx:`(D28), plugin manifest, Bash/Grep 출력 가드, exec 3종(별도 트랙 — D21), `repository{}` 기입, `invalidates`, payload 필드 조회(virtual generated column), title dedup, semantic 보강(recall@k 기준선 후).

## 13. 적대 검증 처리 기록 (2026-07-20, 설계 체크포인트)

서브에이전트(opus) 12건 + Codex(교차 모델) 5건 병렬 검토 → 병합·실코드 검증 후 반영.

**주요 반영**: 훅 전용 session append API 신설(§2.1 — 현행 `Open()` 재사용 불가를 코드로 확정), Shadow Recall 자체 캡·denylist 대조(§5 — `runInline` 무검사 경로 확정), 훅 deadline 예산(§2.3 — 대기 합이 훅 timeout 10s 초과 가능 경로 확정), guard 4조건 판정(§4 — `Indexed==1` 확인·경계 밖/부분 읽기 통과), summary·payload allowlist 조립(§3), 세션 생성 SessionStart 한정 + 미지 세션 drop + UUID 형식 검증(§2.2), 훅 세션 기본 retention 30일(§2.2), settings 병합 원자성·타 도구 보존(§7), 훅 명령 `context-router` 정정·store-root env 규약(§7), 측정 대상 분리(§6), [plan 검증] ⑤ tool_response 절단 정책(§1.3), drops 사이드카 실패 도메인 한계(§2.3).

**미채택(근거)**: ① 출처 인증(IPC/capability) — 로컬 단일 사용자 위협 모델에서 session.db 파일 자체가 같은 권한으로 쓰기 가능하므로 과잉설계; 형식 검증+생성 규칙+untrusted 표시로 한정. ② summary에서 경로·명령어 전면 제거 — 재소환 유용성이 제품 피치의 전제라 allowlist+`Redact()` 절충. ③ Shadow Recall 기본 비활성/프로필 게이트 편입 — `hook install` 행위가 동의 경계이며 `--no-shadow` 옵트아웃 제공으로 절충(강제 채널 테제 유지).
