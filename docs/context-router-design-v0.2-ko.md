# Context Router v0.2 설계서 (강제 채널 — Claude Code 훅 패키징)

- 작성: 2026-07-20 (session-09). 정본 계약은 본 문서 + v0.1 설계서 + v0.0.1 설계서의 승계 조항.
- 전제: v0.1.0 태그(972ad10), 실호스트 스모크 GREEN(stdio+codex), main @ 334442b.
- 스코프 확정 근거: session-08 §5 브레인스토밍 → session-09 사용자 5답
  (옵션 A + exec 분리 / Claude Code 먼저 / 수동 측정 먼저 / 계측 9종 전부 /
  spill journal v0.3 이월 + drop 카운터).

## 0. 결정 이력 (v0.2 신규 — D20까지는 v0.1 설계서·vision-proposal)

- **D21** v0.2 범위 = "강제 채널"(옵션 A). exec 3종은 별도 트랙으로 분리 — vision §7 로드맵 개정.
- **D22** 훅 세션 식별: 호스트 `session_id`를 `cc:` 네임스페이스로 채택. §2.4의 "입력 금지"는 model-plane(MCP 도구) 한정으로 문면 개정.
- **D23** 훅 fail-open: 호스트를 절대 막지 않음. 기록 불가 시 exit 0 + drops 사이드카. spill journal은 drop 데이터가 필요성을 입증할 때 v0.3에서.
- **D24** 자동 계측 이벤트 9종 확정: 기준서 §6.2 초기 15종 − 모델 기록 6종. 훅→이벤트 매핑표(§3)가 생산 계약.
- **D25** large-read guard: PreToolUse(Read) 임계 초과 deny + 현장 선-인덱싱 + 대체 경로 안내.
- **D26** Shadow Recall: PostToolUse 대용량 출력의 패시브 인덱싱(ingest 파이프라인 재사용). MCP 표면의 ingest 프로필 게이트는 유지 — 훅 경유는 호스트 트리거 로컬 경로라 게이트 대상이 아님.
- **D27** 측정은 수동 먼저: `ctr usage`(로컬 transcript 집계) + 수동 A/B 프로토콜. v0.2 exit gate 문면을 "수동 측정 결과 기록"으로 완화(vision §7 개정). 무작위 하네스·OTel은 신호 확인 후.
- **D28** 패키징: `ctr hook install`의 settings.json 멱등 병합. plugin manifest·Codex 훅은 이월.

## 1. v0.2 제품 계약

### 1.1 범위

- `ctr hook` 진입점(단명 프로세스, stdin JSON 1건 처리) + `ctr hook install/uninstall`.
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
- **[plan 검증]** Claude Code 훅 현행 문서 확인 4건: ① PostToolUse가 도구 실패 시에도 발화하는지 + `tool_response` 오류 신호의 실제 형태, ② PreToolUse `permissionDecision` JSON 스키마 현행, ③ SessionStart payload(`source` 등), ④ settings.json `hooks` 스키마. 훅 stdin 골든 픽스처는 확인된 실페이로드 형태로 고정.

## 2. 훅 아키텍처

### 2.1 진입점·프로세스 모델

- Claude Code가 훅 이벤트마다 `ctr hook`(같은 단일 바이너리의 서브커맨드)을 stdin JSON으로 호출. 처리 1건 후 즉시 종료.
- MCP 서버 경유 없음: internal/session을 직접 사용해 session.db에 append. v0.1 §2.1이 다중 프로세스 동시 append를 지원 토폴로지로 보장(순수 INSERT, session_id 자연 분리).
- 신규 코드는 `internal/hook/hook.go`(+`hook_test.go`) 1~2파일(D13 선호 밴드), `internal/cli`가 디스패치. mcp 패키지의 import 금지 규율(database/sql·net/http·os/exec)은 hook에는 해당 없음 — hook은 cli 평면.
- 종료 코드는 항상 0(fail-open, §2.3). PreToolUse의 차단은 exit code가 아니라 stdout JSON `permissionDecision`으로만 표현(§4). 진단은 stderr slog.

### 2.2 세션 식별 (v0.1 §2.4 개정 — D22)

- 훅 stdin의 호스트 `session_id`(Claude Code 발급 UUID)를 `cc:<uuid>` 형태로 채택. 접두사는 출처 표기이자 서버 발급 UUIDv7 공간과의 구분(향후 `cx:` = Codex).
- §2.4 문면 개정: "session_id는 입력으로 받지 않는다"는 **model-plane(MCP 도구 인자) 한정**. 훅 stdin은 호스트 런타임이 산출하는 사실(worktree root와 동급의 신뢰 평면)이다. MCP 도구 표면은 변경 없음.
- SessionStart 훅 → `sessions` 행 INSERT OR IGNORE(started_at·producer `context-router/<ver>`·retention_sec) + `session_start` 이벤트 append(payload: source, worktree root). 그 외 훅 이벤트가 미지 세션을 만나면 행만 보강하고 `session_start` 이벤트는 만들지 않는다(설치 중간 합류 케이스).
- MCP 서버 스트림(자체 UUIDv7)과 훅 스트림(`cc:`)은 같은 논리 세션이라도 session_id가 다른 채 공존. `ctr_session_summary`/`ctr_export_events` 기본 범위가 worktree 전체라 복원 흐름은 두 스트림을 함께 읽는다 — 스트림 통합은 **비목표**(stdio에 프로토콜 세션 식별자가 없어 서버 쪽에서 원리적으로 불가).

### 2.3 fail-open 계약 (D23)

- session.db 사용 불가(fail-closed 손상 상태·락 경합 초과·스키마 불일치 등) 시: 즉시 exit 0, store-root의 session.db 옆 `session.drops.log`에 1줄 append(`<unix-ts>\t<사유>`, O_APPEND 단일 write). doctor가 존재·건수를 항목으로 보고.
- `CTR_HOOKS_OFF=1`: stdin 소비 후 즉시 exit 0 — 수동 A/B의 off 토글이자 비상 탈출구.
- 시간 예산: 목표 ≤200ms(이벤트 append 경로), Shadow Recall 인덱싱 포함 시에도 훅 등록의 timeout(install이 10s 기입)을 넘기지 않는다. 초과 위험 크기는 §5의 캡으로 사전 차단.

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
- summary 구성: `<도구명>: <입력 요지>`(경로·명령 첫 토큰 중심), 기존 상한(≤2048B) 안에서 절단. payload(attributes)에 분류 근거(매치 패턴)·종료 상태 등 구조 필드. 볼륨 억제는 기존 이벤트 상한 + retention 승계로 충분 — 별도 카운트 캡은 두지 않는다.
- 어휘는 여전히 스키마 비강제(§26 미지 타입 보존 의무 승계). v0.1 §2.2의 "권장 어휘" 목록에 9종을 추가하는 문면 갱신만.

## 4. large-read guard (D25)

- PreToolUse(matcher: Read): `tool_input.file_path`의 파일 크기 > 임계(기본 256KiB, `CTR_GUARD_READ_MAX` 바이트로 조정) → stdout JSON `permissionDecision: "deny"`.
- deny 사유에 대체 경로를 명시: 해당 파일을 그 자리에서 content.db에 인덱스(ingest 파이프라인 재사용 — redaction·해시 dedup·크기 캡 포함)한 뒤 "이미 인덱스됨 — `ctr_search`로 검색, `ctr_fetch`로 바이트 정확 조회"를 안내. 인덱스 실패 시에도 deny는 유지하되 사유에 도구 경로만 안내(가드의 목적은 컨텍스트 보호).
- 발화 시 `warning` 이벤트 기록(차단 파일·크기·안내 요지) — 가드 활동이 세션 DB에서 측정 가능. 이벤트 기록 실패는 deny 판정에 영향 없음(fail-open은 기록 경로에만 적용 — 가드 판정은 DB 없이 성립).
- deny는 모델에 피드백되는 소프트 강제다. 사용자 복귀 경로: `CTR_HOOKS_OFF`, 임계 상향, `ctr hook uninstall`.
- Bash `cat`/Grep 출력 가드는 비범위(§1.2) — 명령 문자열 휴리스틱의 오탐 비용이 이득을 넘는다. Shadow Recall(§5)이 출력 측을 사후 보완.

## 5. Shadow Recall 패시브 인덱싱 (D26)

- PostToolUse에서 `tool_response` 직렬화 크기 > 임계(기본 16KiB, `CTR_SHADOW_MIN`) → 전문을 content.db 아티팩트로 저장: provenance=hook, 콘텐츠 해시 dedup, ingest redaction 파이프라인 통과, 기존 ingest 크기 캡 승계. 캡 초과는 절단 저장이 아니라 **미저장** + drops 기록(사유 `shadow-oversize`).
- 저장 시 `artifact_created` 이벤트 + 기본 이벤트에 더해 `tool_result_summary` 이벤트(artifact ref, 결과 요지)를 append.
- 가치 축은 **컴팩션·다음 세션 후의 재소환**이다 — PostToolUse 시점에 출력은 이미 모델 컨텍스트에 들어가 있으므로, 이 채널은 현재 턴 절약이 아니라 재독(re-read) 절약을 만든다. 현재 턴 절약은 §4 guard와 기존 MCP 자발 경로의 몫.
- content.db 다중 프로세스 쓰기(훅 프로세스 vs MCP 서버): WAL + writer 1연결 + txRetry의 기존 규율(v0.0.1 §3.5)로 지원. 동시 쓰기 테스트를 게이트에 포함(§10).
- MCP 표면 `ctr_index`/`ctr_fetch_and_index`의 프로필 게이트(opt-in)는 **그대로**: 그 게이트는 모델 주도 워크스페이스 수집을 묻는 장치고, 훅 경유 인덱싱은 호스트가 트리거하는 로컬 CLI 경로다(D26).

## 6. 측정 (D27)

- `ctr usage` CLI 신설: Claude Code 로컬 transcript JSONL(`~/.claude/projects/<프로젝트 디렉터리>/*.jsonl`)을 읽기 전용 파싱해 세션별 토큰 집계(input/output/cache)를 표로 출력. 네트워크 없음. session.db의 `cc:` 세션 존재 여부와 대조해 훅 on/off 세션을 구분 표기.
- 수동 A/B 프로토콜(문서 절차): 같은 종류의 작업을 `CTR_HOOKS_OFF` on/off로 N세션씩 수행 → `ctr usage` 비교 → 결과를 세션 기록(docs/prompts)에 남긴다. **v0.2 exit gate = 이 수동 측정 결과 기록**(vision §7 게이트 문면 완화).
- 무작위화 하네스·OTel 어댑터는 수동 측정이 신호를 보이면 별도 태스크로(§12).
- **[plan 검증]** transcript JSONL의 usage 필드 형태(모델 메시지별 usage 블록)를 실파일로 확인 후 파서 계약 고정.

## 7. 패키징·설치 (D28)

- `ctr hook install [--user]`: 프로젝트 `.claude/settings.json`(기본) 또는 `--user`로 사용자 설정에 훅 등록을 멱등 병합. 자기 항목 식별은 명령 문자열(`ctr hook`)로 하고 버전 마커를 함께 기입, `ctr hook uninstall`이 대칭 제거. 등록 항목: SessionStart, PreToolUse(Read), PostToolUse(전 도구) + timeout 10s.
- 명령은 PATH의 `ctr`(v0.0.1 global-cli 승계). doctor 확장: 훅 등록 상태(설정 파일 파싱)·PATH 해석·drops 건수를 항목으로 추가하고, 기존 항목 순서 nit([3]→[5]→[4])를 함께 정렬(§9 편승).
- Claude Code plugin manifest(마켓플레이스형)는 이월 — 로컬 우선 단일 바이너리에는 settings 병합이 최소완결. Codex 훅(`cx:`)은 Claude Code 계약 안정 후(§12).

## 8. 보안 계약 (승계 중심)

- 훅 stdin은 호스트 런타임 산출물로 취급(모델 입력 아님) — 다만 그 **내용**(tool_input/tool_response)은 모델·외부 유래이므로, 저장 경로는 전부 기존 파이프라인을 지난다: session 이벤트는 v0.1 §4(요약 상한·redaction), Shadow Recall은 ingest redaction·해시·캡(v0.0.1 §5.1).
- 오류·로그 위생은 v0.0.1 §5.5 승계(stderr slog, 비밀·절대경로 미포함). 테스트의 secret canary 분리-리터럴 규율 승계.
- 훅이 새로 여는 네트워크 표면 없음. 파일 쓰기는 store-root(session.db·content.db·drops 사이드카) 한정.

## 9. 부채 편승 배치 (10건 + doctor nit)

- **개막 정리 태스크**(코드 인접 독립, 마일스톤 ①): filterShortTokenCandidates 파라미터화(~45줄 중복), bak family ts-orphan 정리, modernc corruption fixtures, migrateBusyRetry 분기 단위 테스트, attributes float64 캐비앗 문구, recover worktree-absent UX nit, doctor 항목 순서 정렬(§7과 연동).
- **계측 태스크에 흡수**(세션 인접): Summarize 타입 fan-out 캡, summary budget 정밀화(len(summary) 관례), EventV1 omitempty 소비자 재점검, byte-exact export golden — 훅 스트림이 만들 실제 볼륨·타입 다양성 위에서 테스트 확장으로 처리.

## 10. 테스트·수용 게이트

- 단위: Bash 분류 패턴표 테이블 테스트, 분류 우선순위, fail-open 경로(락 점유 중 exit 0 + drops 1줄), 임계 경계(guard·shadow), settings 병합 멱등성.
- 통합: 훅 stdin 골든(§1.3 검증 후 실페이로드 형태로 고정), content.db 동시 쓰기(훅+서버), guard deny stdout JSON 형식, `cc:` 세션의 summary/export 왕복.
- 실호스트 스모크: 본 저장소에서 `ctr hook install` 후 실제 세션 1회 — 최소 `session_start`/`tool_call`/`file_edit`/`test_run` 관측 + doctor GREEN(훅 항목 포함). 이때 `--profile ingest` fetch 정상 경로 스모크(session-08 §4.2 이월)를 함께 소화.
- Go 테스트는 `-p 1` 메모리 캡 규율 유지. CI gofumpt·lint 기존 게이트 승계.

## 11. 마일스톤 스케치 (상세는 writing-plans)

① 개막 정리(부채 7건+doctor 순서) → ② `ctr hook` 골격 + 세션 식별 + fail-open →
③ 계측 매핑 9종 → ④ Shadow Recall → ⑤ large-read guard → ⑥ `ctr usage` + install/uninstall + doctor 확장 → ⑦ 실호스트 스모크·수동 A/B 1회·기록.

## 12. 의도적 미결 (v0.3+ 후보)

- spill journal(drop 데이터로 필요성 판정 — D23), 무작위 A/B 하네스·OTel 어댑터(D27), Codex 훅 `cx:`(D28), plugin manifest, Bash/Grep 출력 가드, exec 3종(별도 트랙 — D21), `repository{}` 기입, `invalidates`, payload 필드 조회(virtual generated column), title dedup, semantic 보강(recall@k 기준선 후).
