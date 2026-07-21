# Context Router v0.4 설계서 (채널 확장)

- 전제: v0.3.0 태그(main 48f610e, PR #11, CI 3-OS GREEN), 이 레포에 훅 도그푸딩
  설치(프로젝트 범위, marker 0.3.0, matcher `Read|Bash`). 실측(2026-07-21 재점검):
  정상 운영 drop 0건 지속([12] 216건은 설치 시점 버스트로 종결, §1.3 판정 유지),
  [14] blob 7.6MB·중량 세션 1회당 +0.4~1.1MB(§5), `usage --totals` hooks:on
  축적 개시(중량 2세션 포함 573 records).
- 스코프 확정 근거: session-14 브레인스토밍 → 사용자 3답
  (테마 = 채널 확장 / Codex 훅은 측정·캐프처만 / PowerShell 가드는 D32 동등 전체,
  편승 = provenance 표시 + 규모 임계 경고) + 적대 검수 후 4결정(§11).

## 0. 결정 이력 (v0.4 신규 — D33까지는 v0.3 설계서·이전 델타 체인)

- **D34** v0.4 범위 = "채널 확장": Codex 훅 `cx:` 캐프처(D35) + PowerShell 도구
  가드(D36) + 편승 2건(provenance 표시 D37, store 용량 경고 D38) + shadow
  denylist 보강(D39) + 부채 편승(§6, 계획 단계 확정). exec 3종은 v0.5+ 별도
  트랙 유지(D21 — v0.3까지 OS 격리 트랙 미착수, 재이월 사유), shadow 자동
  sweep은 비도입(재상정 조건 = D38 경고 실발화), Grep 가드·spill journal은
  기각/이월 유지(§1.2).
- **D35** Codex 훅 `cx:` = 측정·캐프처만: Codex CLI 세션을 강제 채널의 두 번째
  호스트로 등록(`cx:` 접두, D28 예약 승계) + 이벤트 계측 + (표면이 허용하는
  범위의) shadow 패시브 색인. **차단(deny) 시맨틱은 명시적 비목표**(가드
  동등성은 v0.5+ 재상정). 진입점은 명시적 호스트 경계로 분기한다 — 현행
  `hook.Run`은 `cc:` 고정이므로 호스트 인자 계약을 신설하고, 동일 UUID의
  cc/cx 오귀속 금지를 테스트로 격리(§8). 구현 수준은 G1·G3 관측 결과를 §2
  결정표에 대입해 판정하고, 판정·채택 행은 본 설계서 §11에 추기한다(정본
  기록 — 계획 산출물에만 남기지 않는다).
- **D36** PowerShell 가드 = D32의 형제: PreToolUse matcher `Read|Bash` →
  `Read|Bash|PowerShell`(단일 그룹 유지, 디스패치 tool_name 라우팅에
  guardPowerShell 합류). PS 어휘 게이트 신설(bashDumpArg 자매) — 덤프 후보는
  "대소문자 무시 명령 토큰(`Get-Content`·`gc`·`cat`·`type`) + 옵션 없는
  절대경로 리터럴 위치 인자 1개"로 한정하고, deny는 D25 공유 조건 + 현장
  색인 성공(`Indexed==1`)의 **전건 성립 시에만**(§3 순서 고정). 판정 불가·
  복합·상대경로는 전부 통과(allow 편향, fail-open). PostToolUse shadow
  캡처도 PowerShell에 합류(D39 적용). 선행 게이트: G2 실호스트 관측으로
  tool_name·tool_input 계약 확인(§3).
- **D37** provenance 다중 source 표시 우선순위 = **source_kind 기준**: 같은
  artifact를 여러 source가 가리키면 대표는 `source_kind=="hook"`(패시브)
  최하위 티어, 그 외(명시 ingest — inline·file·web 등 전부) 상위 티어에서
  고르고, 티어 내 동순위는 URI 오름차순. URI 접두 기준이 아니다 — `inline:`
  은 ctr_index 명시 ingest의 현역 네임스페이스이므로 접두만으로는 명시/구
  shadow를 구분할 수 없다(적대 검수 교정, §11). 구현은 hitQuery·sourceOf의
  `ORDER BY uri ASC`를 kind-티어 우선식으로 교체(질의 변경이며 "질의 불변"
  아님). 현행 관측 가능한 조합에서는 표시 결과가 바뀌지 않고(기존 coexist
  테스트 계약 유지 — 구 inline·신 shadow는 둘 다 kind=hook 동티어) 사전식
  우연을 의도된 계층으로 승격하는 결정이다(§4).
- **D38** store 용량 임계 경고: doctor [14]에 **content.db CAS 전체 blob
  총량**(shadow 전용 아님 — [14]의 측정 실체 그대로) 임계 초과 시 경고 1줄
  추가(기본 100MB, `CTR_SHADOW_WARN_BYTES` 오버라이드). 자동 삭제 없음.
  경고 문구는 수동 구제 경로(purge 계열 CLI)를 안내하되 **현행 purge의
  비선택(source_kind 무구분) 삭제 성격을 병기**한다. SizeStats 실패([14]
  "없음") 시 경고 미평가. shadow 귀속 바이트 집계·hook 전용 purge는 v0.5+
  후보(§10). 이 경고의 실발화가 sweep 재상정 조건(§5).
- **D39** command 계열 shadow의 정적 증명 출처 denylist 대조: 현행
  shadowCapture의 denylist 대조는 파일-유래 도구(Read·NotebookRead)의
  tool_input 경로에만 적용되고 command 출력(Bash — v0.4부터 PowerShell)은
  대조 없이 색인된다(Redact 의존). v0.4에서 Bash·PowerShell 대칭 보강 —
  **어휘 게이트가 명령을 단일 파일 덤프로 정적으로 증명하는 경우 그 경로를
  denylist에 대조**, 걸리면 미저장(drops `shadow-denylist`). 증명 불가
  출력(파이프·복합식 등)은 현행대로 색인하며 그 한계를 §7에 명문화(§3).

## 1. v0.4 제품 계약

### 1.1 범위

- Codex 훅 `cx:` 캐프처 — 호스트 경계·세션 등록·이벤트 계측·(결정표 수준의)
  shadow 색인(§2).
- PowerShell 단일파일 덤프 가드 + shadow 캡처 합류(§3).
- command 계열 shadow denylist 정적 대조 보강 — Bash·PowerShell 대칭(D39, §3).
- provenance 표시 우선순위 규약(§4) + store 용량 임계 경고(§5).
- 부채 편승(§6 — 계획 단계 확정).

### 1.2 명시적 비범위 (v0.4)

- exec 3종 — v0.5+ 별도 트랙 유지(D21). OS 격리 + §8.3 실행 계약 + 호스트
  승인이 묶인 대형 단독 릴리스 대상.
- Codex 가드 동등성(차단) — D35 비목표. 캐프처 실측 후 v0.5+ 재상정.
- `usage --totals`의 Codex 집계 확장 — usage는 Claude Code 로컬 transcript
  열거 도구(행 키=transcript 파일, hooks 판정 `cc:`+uuid)라 Codex 반영은
  세 번째 저장 형식 계약이 필요한 별도 작업. cx: 검증은 session.db 표면이
  담당(§8 ③).
- PowerShell 명령의 이벤트 분류(build/test/git 2차 switch 확장) — 비범위,
  PS 기본 이벤트는 tool_call 유지.
- shadow 자동 sweep — 실측(+0.4MB/중량 세션)상 긴급성 없음. D38 경고
  실발화 시 재상정.
- Grep 도구 출력 가드 — 호스트 기본 캡(250줄) 억제 유지, 실측 필요성 미입증.
- spill journal — v0.3 §1.3 판정 종료 유지(2026-07-21 재실측: 정상 운영
  drop 0건 지속). drop 실측이 뒤집히면 재상정.

### 1.3 선행 게이트 (계획 Task 0 상당 — 실패 시 해당 축 폴백/중단·보고)

- **G1 Codex 런타임 표면 관측**: T3 리서치(공식 문서) + 실호스트 프리체크로
  Codex 훅/notify가 주는 세션 생성 신호·이벤트 단위(도구/턴/세션)·출력
  페이로드 유무를 기록. 결과를 §2 결정표에 대입해 구현 수준을 판정.
- **G2 PowerShell 계약 관측**: 스크래치 프로젝트 캡처 훅으로 PowerShell
  도구의 tool_name 문자열·tool_input 스키마(command 필드 형태) 실측.
  matcher 확장 문자열과 어휘 게이트 입력 계약을 여기서 확정.
- **G3 Codex 설치 표면 관측**: `~/.codex/config.toml`의 훅/notify 등록
  스키마(테이블·키·값 형식) 실측 — G1(런타임)과 별개의 미관측 계약. §2
  TOML 병합 계약의 소유 항목 정의가 여기 의존.
- G1·G2·G3의 관측 결과와 채택 분기는 본 설계서 §11에 추기하고 세션 기록
  (docs/prompts)에도 남긴다 — 계획 산출물 단독 기록 금지(유실 방지).

## 2. Codex 훅 `cx:` 캐프처 (D35)

- 세션 식별: v0.2 §2.2의 external 접두 규약을 호스트별로 확장 — Claude Code
  `cc:<session_id>`, Codex `cx:<session_id>`. 진입점은 명시적 호스트 인자
  계약으로 분기(현행 `hook.Run`의 `cc:` 고정 유지 + Codex 경로 신설 —
  재사용으로 인한 오귀속 금지, 동일 UUID cc/cx 격리 테스트 §8). 세션 등록·
  후속 이벤트 drop 규칙(§2.2)은 네임스페이스별 동일 적용.
- 호스트 구분 표면: `ctr_session_summary`/`ctr_export_events`는 세션 접두로
  구분된다(session.db 기반). **`usage`는 아니다**(§1.2 — transcript 열거
  도구). 계측 매핑은 G1 관측 결과에 따라 v0.2 §3(D24) 표의 재사용 가능한
  부분만 재사용하고, 없는 이벤트는 기입하지 않는다(허위 계측 금지 — D27).
- **구현 수준 결정표** (G1 관측 결과 → 구현; 판정·채택 행은 §11 추기):

  | 행 | 세션 생성 신호 | 도구 단위 이벤트 | 출력 페이로드 | 구현 수준 |
  |----|----|----|----|----|
  | 1 | 有 | 有 | 有 | 전체: 등록 + 도구 단위 계측 + shadow(D30·D31·D39 게이트) |
  | 2 | 有 | 有 | 無 | 등록 + 도구 단위 계측. shadow 재이월 |
  | 3 | 有 | 無(턴/세션 단위만) | — | 등록 + 해당 단위 계측. shadow 재이월 |
  | 4 | 無 | 有(어느 단위든) | — | 첫-이벤트 등록 승격이 Codex 진입점에서 안전하게 정의 가능한지 G1이 판정(§2.2 drop 규칙과 충돌하는 형태 금지). 불가 시 행 5 |
  | 5 | 표면 부재·문서화 안 됨·버전 간 불안정 | — | — | 축 중단·보고(설계 개정 없이 v0.5 재상정) |

  판정 주체는 계획 Task 0의 컨트롤러 세션. 행 1~3은 서로 배타(도구 단위
  이벤트 유무 → 페이로드 유무 순으로 판정), 행 4·5는 선행 축(세션 신호·표면
  안정성) 실패 시 우선한다.
- 설치: `~/.codex/config.toml` 병합기는 **TOML 신규 구현** — D28에서
  승계하는 것은 원칙(멱등·원자 쓰기·타 항목 보존·제거 대칭·실패 시 원본
  무변경)뿐이고, JSON(settings.json) 병합기 구현은 재사용 불가. 소유 항목
  식별(마커)·중복 처리·주석/미지 키 보존 수준은 G3 관측 후 설치 계약으로
  확정해 §11에 추기.

## 3. PowerShell 가드 (D36) + shadow denylist 보강 (D39)

- 배선: matcher `Read|Bash|PowerShell` 단일 그룹(호스트 정규식 매처 — v0.3
  D32의 단일 그룹 유지 근거 승계: merge의 동일-이벤트 상호 제거 함정 회피).
  dispatch의 tool_name switch에 `PowerShell` → guardPowerShell 추가.
- 어휘 게이트(psDumpArg, bashDumpArg 자매) — 덤프 후보의 최소 문법:
  **대소문자 무시 명령 토큰(`Get-Content`·`gc`·`cat`·`type`) + 옵션 없는
  절대경로 리터럴 위치 인자 정확히 1개**. 다음은 전부 통과(allow 편향):
  부분 읽기 플래그(`-TotalCount`/`-Head`/`-Tail` — offset/limit 상당),
  그 외 모든 대시 토큰(`-Raw`·`-Path`·`-LiteralPath` 등 명명 파라미터 포함),
  상대경로(판정 제외 — 훅 cwd ≠ 도구 cwd 오탐-deny 방지, D32 §4 승계),
  배열/다중 경로·와일드카드·따옴표 밖 백틱·괄호·변수 전개·서브식·파이프·
  리다이렉트·복합식(`;`·`&&`)·단독 `&`. 확정 케이스 표는 G2 관측 후 계획에
  명시(bashDumpArg 16행 표 관례).
- **deny 순서 고정(D25·D32 승계)**: ① 정적 단일파일 덤프 판정(위 문법) →
  ② 절대경로 존재·정규 파일·경계 내·임계 초과(D25 공유 조건) → ③ denylist
  정적 대조(D39 — 걸리면 deny 아닌 allow + 미색인, v0.3 T3 관례) → ④ 현장
  색인 성공 `Indexed==1` → **전건 성립 시에만 deny + 대체 경로 안내**(상대
  경로·절대경로 비포함 양식 D32 승계). 하나라도 실패하면 allow — "차단됐는데
  ctr_search에 자료가 없는" 복구 불능 상태를 만들지 않는다.
- shadow(D39 포함): PostToolUse PowerShell 출력도 Bash와 동일 게이트
  (OFF→MIN→MAX→Redact→decode-sniff)로 색인. command 계열(Bash·PowerShell)
  공통 보강 — 어휘 게이트가 해당 명령을 단일 파일 덤프로 정적으로 증명하면
  그 경로를 denylist 대조 후, 걸리면 미저장(drops `shadow-denylist`).
  증명 불가 출력(파이프·복합식·다중 파일)은 현행대로 색인 — 이 잔여 표면은
  Redact 의존이며 §7에 한계로 명문화.
- 이식성 교훈 승계(session-13 프로토콜): 어휘 게이트는 allow 편향이므로
  비ASCII·8.3 short-name 경로는 가드를 그냥 통과한다(프로덕션 by-design).
  픽스처 경로는 `filepath.EvalSymlinks` 정규화 필수, CI 3-OS 게이트 유지.
- PS 명령의 이벤트 분류(2차 switch build/test/git 확장)는 비범위(§1.2) —
  기본 이벤트 tool_call 유지.

## 4. provenance 표시 우선순위 (D37)

- 대상: 검색 히트·fetch 응답에서 artifact의 대표 source 표시 문자열 선택.
- 규약: 같은 artifact(content_hash)를 가리키는 source가 복수면
  **`source_kind!="hook"`(명시 ingest — inline·file·web 등 전부) 티어가
  `source_kind=="hook"`(패시브) 티어에 우선**하고, 티어 내 동순위는 URI
  오름차순. URI 접두 기준 아님 — `inline:`은 ctr_index 명시 ingest의 현역
  네임스페이스라 접두로는 명시/패시브를 구분할 수 없다.
- 구현 명시: 대표 선택은 현재 hitQuery·sourceOf 두 곳의 SQL
  `ORDER BY uri ASC`가 결정하므로 두 질의를 kind-티어 우선식으로 동시
  교체한다(질의 변경). 현행 관측 가능한 조합에서 표시 결과는 불변 —
  구 `inline:<Tool>`(pre-D30 shadow)과 신 `shadow:` 행은 둘 다 kind=hook
  동티어라 기존 `TestQuery_HitSourceShadowCoexist` 계약(inline 우선)이
  유지된다. 신규 테스트는 티어 규칙 자체를 단정한다(예: 사전순으로 명시
  티어보다 앞서는 hook URI가 대표가 되지 않음).
- v0.3 §2 승계 한계("provenance 표시에 국한")의 재설계 완결 — 사전식 우연을
  의도된 계층으로 승격. 저장 스키마 불변.

## 5. store 용량 임계 경고 (D38)

- doctor [14] 행 뒤에 조건부 경고 1줄: **content.db CAS 전체 blob 총량**
  (shadow 전용 아님 — 명시 ingest 포함, [14] 측정 실체 그대로) > 임계
  (기본 100MB, `CTR_SHADOW_WARN_BYTES` 파싱 실패·비양수 → 기본값) 시에만
  출력. 문구는 총량 현황 + 수동 구제 경로(purge 계열 CLI) 안내 + **현행
  purge의 비선택 삭제 성격**(source_kind 무구분 — shadow만 골라 지울 수
  없음)을 함께 명시. SizeStats 실패([14] "없음", ro-open 경합 등) 시 경고
  미평가. 자동 삭제·백그라운드 동작 없음.
- 근거 실측: 현재 7.6MB, 중량 세션당 +0.4~1.1MB(session-13·14 두 실측) —
  기본 임계 도달에 중량 세션 ~85~230회. 경고는 관측 채널이지 정책 집행이
  아니다(D27 원칙 정합). shadow
  귀속 바이트 집계와 hook 전용 선택 purge는 v0.5+ 후보(§10) — 경고
  실발화가 그 재상정 조건.

## 6. 부채 편승 배치

v0.3 최종 리뷰 follow-up 5건은 v0.4 착수 전 별도 웨이브로 선처리 완료
(session-14, PR #12). v0.4 편승 목록은 그 웨이브의 태스크 리뷰 minors +
v0.3 accept 항목 중 재부상분을 계획(writing-plans) 단계에서 확정한다.

## 7. 보안 계약 (승계 중심)

- 가드·shadow의 기존 계약 전부 승계: fail-open(훅 실패가 호스트를 막지
  않음), allow 편향 어휘 게이트, denylist·디코드-sniff·CTR_SHADOW_MAX,
  Redact 통과, 비밀 미색인 canary 게이트, 안내 이벤트에 절대경로 비포함.
- D39로 command 계열 shadow의 denylist 표면이 좁아진다(정적 증명 경로 대조).
  **잔여 한계 명문화**: 증명 불가 command 출력(파이프·복합식 경유 비밀 파일
  내용 등)은 denylist 대조 없이 Redact·sniff만 통과 후 색인된다 — 이 표면의
  추가 축소는 v0.5+ 재상정 대상이며, canary 게이트는 정적 증명 케이스의
  미색인을 검증한다.
- Codex 채널: 캐프처 전용(차단 없음)이므로 신규 거부 표면 없음. 설치 병합은
  원칙 승계 + TOML 신규 구현(§2) — 사용자 전역 Codex 설정의 타 항목·주석
  보존과 실패 시 원본 무변경을 계약으로 고정(파손 금지).
- exec 관련 표면 개방 없음(§1.2).

## 8. 테스트·수용 게이트

- 선행 게이트 G1·G2·G3 기록이 계획 Task 0 산출물이자 §11 추기 대상(실패 시
  §2 결정표 행 4·5 / D36 중단 보고 발동).
- 전체 `go test -p 1 ./...` GREEN + gofumpt clean + CI 3-OS GREEN(메모리 캡
  테스트 규칙 유지).
- 실호스트 스모크: ① PS 대형 단일파일 덤프 deny + 안내 이벤트 + 현장 색인
  실증 ② PS 정상 명령(파이프·부분읽기 플래그) 통과 ③ Codex 세션 `cx:` 등록
  실증 — **session.db 표면 기준**(`ctr_session_summary`/`ctr_export_events`
  의 `cx:` 레코드, G1 채택 행 수준에 맞춤; usage 비대상 §1.2) ④ doctor
  [14] 경고: 임계 미만 무출력 확인(+ 초과 케이스는 테스트 픽스처).
- 격리·회귀 테스트: 동일 UUID의 cc/cx 오귀속 금지(D35), D39 정적 증명
  denylist 미색인(Bash·PS 각 1 + 증명 불가 색인 유지 케이스), D37 티어
  규칙 단정 + 기존 coexist 계약 유지 확인, [14] 정확-문자열 단정(경고
  미발화 경로) 무회귀.
- 가드·게이트 픽스처는 EvalSymlinks 정규화(8.3 함정 프로토콜).

## 9. 마일스톤 스케치 (상세는 writing-plans)

1. Task 0: G1·G2·G3 관측 프리체크(병렬 가능) — §2 결정표 행·설치 계약 확정,
   §11 추기.
2. PowerShell 가드(D36) + D39 보강(Bash 대칭 포함) — 게이트·통합·재설치
   테스트.
3. Codex 캐프처(D35) — 호스트 경계·설치 병합(TOML)·세션 등록·계측·(결정표
   행별) shadow.
4. provenance 티어(D37) + store 용량 경고(D38) + 부채 편승.
5. 버전 범프·재설치·실호스트 스모크·최종 이중 리뷰·CI·머지.

## 10. 의도적 미결 (v0.5+ 후보)

exec 3종(D21 트랙), Codex 가드 동등성(D35 후속), shadow 자동 sweep +
shadow 귀속 바이트 집계 + hook 전용 선택 purge(D38 경고 실발화 시),
command shadow denylist 잔여 표면 축소(D39 후속 — 증명 불가 출력),
Grep 도구 가드, plugin manifest, 무작위 A/B 하네스·OTel(D27), semantic
보강(recall@k 기준선 후), spill journal(재상정 조건 v0.3 §1.3),
`repository{}` 기입, `invalidates`, payload 필드 조회(virtual generated
column), title dedup, CAS 갱신 시 구버전 blob 즉시 orphan-GC(실해 미관측).

## 11. 적대 검증 처리 기록 (2026-07-21, 설계 체크포인트)

- 이중 적대 검수 1패스: 서브에이전트(opus) C2·I5·M5 + Codex(high 5·medium 3).
  수렴 6건(cx:/usage 불가, D35 폴백 비망라, TOML 설치 과대 승계, psDumpArg
  미명세, D37 inline 오분류, D38 측정 대상 오류) + 고유 2건(Codex: command
  shadow denylist 부재 — 실코드 `fileOriginTools={Read,NotebookRead}` 확인,
  deny 선행조건 `Indexed==1` 누락) + 서브에이전트 고유(D37의 coexist 테스트
  파손 — URI 접두 규칙 전제) 전부 반영.
- 사용자 4결정: ① cx: 검증은 session.db 표면으로 게이트 재기술, usage 확장
  비범위 ② command shadow는 정적 증명 경로만 denylist 대조(D39 신설, Bash
  대칭) ③ D37은 source_kind 티어 기준으로 재정의(URI 접두 규칙 폐기 —
  coexist 계약 유지) ④ D38은 전체 용량 경고로 정정 + purge 비선택 성격
  병기.
- 하향/기각: Codex 원권고 "증명 불가 command 출력 전부 미색인"은 shadow
  커버리지 급감으로 기각(잔여 표면은 §7 한계 명문화 + v0.5 재상정).
  "usage에 Codex 집계 추가"는 세 번째 저장 형식 계약이라 비범위 처리.
- (계획 체크포인트의 adversarial-review 1패스는 별도 — 계획 확정 시 수행해
  여기 추기.)

### §11.1 관측 프리체크 결과·채택 (계획 Task 0, 2026-07-21 — 컨트롤러 세션 수행·판정)

- **G1 — §2 결정표 행 1 채택(전체: 등록 + 도구 단위 계측 + shadow).** 근거:
  codex-cli 0.144.6 로컬 실측 + 공식 훅 문서(developers.openai.com/codex/hooks).
  Codex는 Claude Code 동형 훅 시스템을 갖는다 — 공통 stdin 필드 `session_id`
  (canonical UUID — rollout session_meta의 payload.session_id 실측 일치)·
  `transcript_path`·`cwd`·`hook_event_name`·`model`·`permission_mode`; 이벤트
  SessionStart(`source`=startup/resume/clear/compact, matcher는 source 대상)·
  PreToolUse·PostToolUse(`tool_name`·`tool_use_id`·`tool_input` — Bash·
  apply_patch는 `.command`·`tool_response` JSON 출력 페이로드 有, Bash 비-0
  종료도 PostToolUse로 발화하며 PostToolUseFailure 이벤트는 부재)·
  UserPromptSubmit·SubagentStart/Stop·Stop·Pre/PostCompact. 세션 생성 신호 有
  + 도구 단위 이벤트 有 + 출력 페이로드 有 → 행 1. 계측 매핑: 기존 classify/
  buildEvent 재사용(Bash 분류 유효 — Codex tool_name `Bash` 실재; apply_patch·
  mcp__*는 기본 tool_call), 없는 이벤트 미기입(D27). Codex의 Bash 비-0 종료가
  tool_call로 계상되는 것은 정직한 한계로 수용(error 이벤트 부재 — 허위 계측
  금지 우선).
- **G2 — matcher `Read|Bash|PowerShell` 확정.** 스크래치 프로젝트 캡처 훅
  실측(Claude Code, win32): tool_name 정확히 `"PowerShell"`, PreToolUse 발화
  확인, tool_input `{command, description}`(어휘 게이트 입력 =
  `tool_input.command`), PostToolUse tool_response `{stdout, stderr,
  interrupted, isImage}`(Bash 동형 — shadow 게이트 무수정 적용 가능),
  session_id canonical UUID.
- **G3 — §2 "TOML 병합기 신규 구현" 항목 비발동, hooks.json JSON 병합기로
  대체.** 등록 표면 실측·문서: `~/.codex/hooks.json`(user)·
  `<repo>/.codex/hooks.json`(project) — Claude settings.json `hooks` 필드와
  동형 스키마(`{"hooks":{"<Event>":[{matcher, hooks:[{type:"command",
  command, timeout, statusMessage}]}]}}`). `config.toml`의 `[hooks.state]`는
  Codex 소유 trust-hash 저장소라 설치기 기입 금지(신뢰 승인 우회 방지),
  inline `[hooks]`는 비채택(공식 권고 "Prefer one representation per
  layer"). 설치 계약: JSON 병합에 D28 원칙 승계(멱등·원자 쓰기·타 항목/미지
  키 raw 보존·제거 대칭·실패 시 원본 무변경). 소유 마커는 미지 필드
  (`__ctrManaged`)의 스키마 관용성이 문서상 미보증이라 공식 필드
  `statusMessage`에 `context-router/<version>`을 탑재하고 command 토큰 정확
  일치(`context-router codex-hook` — §11.2 F3의 전용 러닝 서브커맨드, 소유
  판정은 §11.2 F4의 전건 규칙)와의 결합으로 판정한다. 훅 실행은 사용자의
  Codex `/hooks` 신뢰 승인에 의존(정의 변경 시 재신뢰) — 설치기가 안내 1줄을
  출력한다. Codex 등록 이벤트 = SessionStart + PostToolUse만(D35 캐프처 전용
  — PreToolUse 미등록, §7 "신규 거부 표면 없음" 정합).
- **파생 확정 2건.** ① D39 대조 경로는 어휘 게이트 반환값을 절대화 없이
  ToSlash+Clean 정규화만 거쳐 `ingest.DeniedFilename`에 대조한다(§11.2 F2
  정정 — 점 세그먼트 변형의 `.docker/config.json` 접미 규칙 우회 봉쇄) —
  대조는 이름 기반이라 상대경로 덤프(`cat secrets/.env`)도 커버하며, deny
  게이트(D25·D32)의 절대경로 요건과는 무관하다. ② psDumpArg의 절대경로 판정은 bashDumpArg용 MSYS
  `/x/…` 변환을 승계하지 않는 형제 함수(psAbsPath)로 한다 — PowerShell에서
  `/c/x`는 "현재 드라이브 루트 상대" 경로라 MSYS 변환을 적용하면 오파일
  판정 위험; Windows는 드라이브형(`X:\`·`X:/`, ToSlash 정규화)만, Unix
  pwsh는 `/`-접두만 절대로 인정한다.

### §11.2 계획 체크포인트 적대 검수 처리 기록 (2026-07-21)

- 이중 적대 검수 1패스(계획 `docs/superpowers/plans/2026-07-21-v04-channel-expansion.md`
  대상): 서브에이전트(opus) Critical 1·Minor 4 + Codex adversarial-review(high 4·
  medium 2, 판정 needs-attention). 수렴 2건 반영 — 격리 테스트 픽스처 UUID
  불일치(C1=F5: 동일 UUID cc/cx 격리를 검증하려면 Codex 픽스처가 형제 픽스처와
  같은 UUID여야 함), Run 호출부·임포트 전수 누락(M1·M2=F6: runGuard 호출부,
  cli.go `strconv`·hook_test.go `runtime`·shadow.go `path`/`path/filepath`).
- **Codex 고유 채택 3건**:
  - **F3 → Codex 러닝 진입점은 `--host` 플래그가 아니라 전용 서브커맨드
    `codex-hook`**: v0.3 러닝 훅은 미지 인자를 fail-open으로 무시하므로 플래그
    방식은 "구버전 바이너리 + 신버전 hooks.json" 조합에서 Codex 이벤트를
    조용히 `cc:`로 오귀속시킨다. 미지 서브커맨드는 구버전 dispatchCLI가 exit
    1로 거부하므로 서브커맨드가 구조적 버전 게이트다(D35 오귀속 금지의 배포
    호환성 확장). hook.Run의 host 인자 검증(bad-host drop)은 심층 방어로 유지.
  - **F4 → Codex 그룹 소유 판정은 전건 규칙**: 그룹의 모든 훅 항목이 command
    토큰 정확 일치 AND statusMessage 마커 접두일 때만 자기 그룹(any-판정이면
    사용자가 항목을 추가한 혼합 그룹까지 통째 삭제됨 — Claude 쪽은 그룹 레벨
    `__ctrManaged` 마커가 있어 노출이 다름). 혼합 그룹은 불가침 — 파손 금지가
    멱등 완전성에 우선하고, 잔존 정리는 사용자 `/hooks` 몫. Codex 원권고의
    훅-항목 단위 외과 제거는 그룹 재작성이 미지 그룹 필드를 파괴할 수 있어
    전건 판정으로 대체. 동일 버전 f(f(x))==f(x) 바이트 멱등 테스트 추가.
  - **F2 부분 → D39 대조 전 ToSlash+Clean 정규화**(§11.1 파생 ① 정정): 점
    세그먼트(`cat ./.docker/./config.json`)의 접미 규칙 우회 봉쇄 + PS 백슬래시
    경로의 basename 판정 OS 무관화.
- **기각/하향**: F1(psDumpArg 덤프 토큰의 별칭 재정의·프로필 shadow로 인한 오탐
  deny)은 D32 bash `cat` 셰도잉과 동일 클래스의 기수용 리스크 — deny 시에도
  대상 파일이 현장 색인돼 ctr_search/ctr_fetch로 복구 가능(비가역 아님)하고,
  module-qualified 한정은 가드 무력화라 기각. F2 잔여(대소문자·symlink 변형,
  PS provider 경로의 shadow 오드롭 — 예: `Get-Content Env:\.env`)는 기존 Read
  경로 denylist와 동일한 잔여 표면이며 오동작 방향이 안전(미저장)이라 §7
  한계로 기록(v0.5+ 재상정 후보). 서브에이전트 M3(병합기 형제 중복)은 G3 확정
  사항 재확인, M4(다중 소스 테스트 전수)는 Task 6의 패키지 전체 GREEN 게이트가
  커버.
