# Context Router v0.6 설계서 (측정 성숙 — D27 트랙)

- 전제: v0.5.1 태그(main 8cdec1e, PR #16, CI 3-OS GREEN), 도그푸딩 marker
  0.5.1(`.mcp.json` bare 등록), Codex는 훅 A/B만 유지(MCP 등록 제거).
  실측(2026-07-22, §7): usage 관찰 A/B 첫 신호 — cache_read/record
  hooks:on/off 비 0.60(중량 세션만 0.63), cx: 9세션·203이벤트(중량 3),
  [12] drops 324 증가 정지(스테일 차단 후 재발 0), content.db 파일
  82.5MB(100MiB의 79% — 현행 경고는 blob 축만), blob 22.4MB(임계 21%).
- 스코프 확정 근거: session-21 브레인스토밍 → 사용자 결정 4건(축=측정
  성숙 / 구성=usage 비교 리포트+Codex usage 어댑터, 무작위 하네스·OTel
  이월 / content.db 파일 축은 경고만 편승 / 접근=단일 usage 확장 —
  신규 명령·MCP 도구 0개). 초안 ea7d022에 이중 적대 검수 1패스 반영
  (§9 — arm 스코프·리셋 계약·임계 키 분리 등 구조 교정 포함).

## 0. 결정 이력 (v0.6 신규 — D43까지는 v0.5 설계서·이전 델타 체인)

- **D44** Codex usage 어댑터: `~/.codex/sessions/YYYY/MM/DD/
  rollout-<ts>-<uuid>.jsonl` 읽기 전용 파싱으로 cx: 토큰 축 신설.
  **v0.4 기각(§1.2 "세 번째 저장 형식 계약"·사용자 결정 ①)의 명시적
  번복**이며 번복의 실질 근거는 두 가지: ① cx: 축적 데이터 활용처가 본
  버전의 지정 소재(사용자 결정 — 수요 확정), ② D27의 재상정 조건("수동
  측정 신호 확인 후") 첫 충족 — cc: 관찰 A/B가 신호를 보였고(§7), 테제
  검증을 둘째 채널로 확장하려면 cx: 토큰 축이 필요하다. 로컬 실측으로
  소비 필드가 2종(`session_meta.payload{session_id,cwd}` +
  `event_msg/token_count.info.total_token_usage`)임을 확인했으나 이는
  파싱 표면의 한정이지 위험 해소가 아니다 — 비공개·무버전 내부 형식
  의존은 기각 사유 그대로 남으며(적대 검수 §9), 그 잔여 위험은 (a) cx:
  블록의 **experimental 표시**, (b) 스킵·불일치 집계의 상시 표기(0이
  아니면 비율 옆 경고), (c) 형식 변경 시 스킵 강등 규율(§2)로 봉한다.
  버전별 fixture 고정은 비채택(로컬 도구는 사용자 Codex 버전 추종 —
  §9 기각 기록). 파싱 계약(§2 상세): UUID 권위는
  `session_meta.session_id`(파일명은 발견 수단), 토큰 합산은
  **meta 구간별 delta 누적**(last-wins 아님 — 재개 시 누적 카운터
  리셋이면 과소계상, 적대 검수 수렴 §9), cwd 규율은 **모든 meta의
  cwd가 루트 하위일 때만 채택**(발산 파일은 제외+집계) + **미해석
  경로끼리 Fold 비교**(RealPath 비적용 — cc: transcript 경로 유도가
  Canonicalize를 의도적으로 회피하는 선례와 동형). arm 판정은
  session.db `cx:<uuid>` 존재 조회를 **프로젝트 `worktrees/*` 전체
  순회 병합**으로 수행(D40 ③ "단일 worktree 조인=체계적 과소귀속"
  정합 — 단일 조회 재사용은 하위 디렉터리 기동 세션을 오분류, 적대
  검수 Critical §9). 미등록=off 판정에는 **관측 창 게이트**: 세션
  시작이 30일(훅 retention 기본) 밖인 rollout은 session.db 부재가
  삭제의 결과일 수 있어 off가 아닌 **unknown**으로 계상·제외(과거
  hooks:on 세션의 off 전이 차단 — 적대 검수 §9). cx: 등록인데 rollout
  부재(§7 — exec 계열 3/3)면 토큰 축 `n/a` 별도 카운트. 침묵 탈락
  금지. 소비자는 D45 `--compare`뿐 — 무플래그 `usage` 본표는 cc:
  transcript 열거 그대로(byte-for-byte 불변 게이트 승계, v0.3 §8).
- **D45** `usage --compare` 비교 리포트: 채널(cc/cx)×arm(hooks:on/off)
  집계표 + on/off 비율 + 고정 캐비앗을 한 커맨드로 출력(session-21에서
  수작업으로 수행한 계산의 제품화). `--compare`는 **리포트 전용 출력**
  — 세션별 본표를 출력하지 않는다(대체, 병기 아님). 열은 채널별로
  의미가 다르다 — cc: 단위=record(어시스턴트 메시지),
  열=output/rec·cache_read/rec; cx: 단위=turn(`token_count` 이벤트 수),
  열=output/turn·cached_input/turn·input/turn. **채널 간 교차 비교는
  계약 밖**(단위 상이 — 표가 채널을 분리 유지). `--min-records N`
  (기본 0)으로 중량 세션 필터 재현. cx: 커버리지는 **양 arm 표기**
  (on: 등록 N·매칭 M·n/a K / off: 창 내 미등록 J·unknown U) + cx: 비율
  행 옆 고정 캐비앗 "exec 계열은 양 arm 모두 토큰 축에서 계통
  제외"(off-arm의 exec 세션은 흔적 자체가 없어 정량 불가 — §5 한계,
  적대 검수 §9). 마지막 줄 고정 캐비앗: "관찰 데이터(무작위화 아님) —
  워크로드 교란 존재". 교란 통제(무작위 배정)는 명시적 비범위(§1.2) —
  1인 도그푸딩에서 세션 절반의 가드·shadow 혜택을 포기하는 운영 비용이
  커, 관찰 축적 후 재상정. 기존 무플래그·`--totals` 출력은
  byte-for-byte 불변(기존 게이트 그대로).
- **D46** doctor [14] 파일 축 경고: `file > 임계` 시 별도 경고 라인
  추가하되 임계 키는 **blob과 분리한 전용 키
  `CTR_CONTENT_FILE_WARN_BYTES`(기본 100MiB)** — 초안의 동일 키 공유는
  이중 적대 검수 수렴 기각(§9): 파일 축(82.5MB)이 blob 축(22.4MB)보다
  3.7배 크고 빨리 자라 동일 키면 해소 곤란한 파일 경고가 기준 축을
  지배하고, 사용자가 한쪽을 조정하면 다른 축까지 무력화된다. D40 ①의
  "D38 경고 평가 기준=blob" 결정은 대체하지 않는다 — blob 경고는
  "purge → 감소 → 해소" 일관이 성립하는 기준 축으로 유지하고, 파일 축
  라인은 자문(advisory) 성격으로 병행한다. 문구에 축 특성과 구제
  경로를 정직하게 명시: 청크 텍스트+FTS 축이며 행 삭제(purge 계열)
  후에도 free page 탓에 즉시 줄지 않고, 회수는 VACUUM — 라이브 서버
  제약이 있어 서버 비가동 시 수행(D41 §7 병기 승계). 회수 자동화는
  비범위 — 경고 실발화 후 재상정(D38 선례 패턴).

## 1. v0.6 제품 계약

### 1.1 범위

- Codex usage 어댑터 — rollout JSONL 최소 파싱, cx: 토큰 축(D44, §2).
- `usage --compare` 비교 리포트 — 채널×arm 집계·비율·캐비앗(D45, §3).
- 편승: doctor [14] content.db 파일 축 경고 라인(D46, §4).
- 전부 읽기 전용·네트워크 없음·신규 명령/MCP 도구 0개(D13 반파편화).
- v0.6.0 릴리스 대상(버전 상수 0.6.0 — 사용자 가시 변경).

### 1.2 명시적 비범위 (v0.6)

- 무작위 A/B 하네스·OTel 어댑터 — D45 기재 사유(운영 비용·소비자 부재).
  관찰 데이터 축적과 신호 안정성 확인 후 재상정(D27 문면 유지).
- content.db 파일 축 회수 자동화(sweep·자동 VACUUM) — D46 경고 실발화 후.
- register-on-first-event — D43 비채택 유지. 재상정 조건(재발 실측)이
  이번 확인에서 미충족: drops 324로 증가 정지(§7).
- codex 플러그인·companion 수정 — 외부 플러그인 불변 원칙. exec 계열
  rollout 부재는 어댑터의 n/a 계약으로 흡수(D44).
- cx: arm 배정의 불변 manifest 저장 — 새 저장 표면(§9 기각). 관측 창
  게이트+unknown 버킷(D44)으로 휘발성 왜곡만 차단.
- rollout meta 구간별 프로젝트 귀속(교차 cwd 재개의 구간 분할) — cwd
  발산 파일 제외 규칙(D44)으로 대체(§9 하향).
- cx: 원시 행의 무플래그 `usage` 본표 편입 — byte-for-byte 게이트 유지.
  소비자가 생기면 그때 플래그로 재상정.
- exec 3종(D21)·Codex 가드 동등성(D35 후속)·서브에이전트 캡처·Grep 가드·
  plugin manifest·semantic 보강·spill journal 등 §8 잔여 — 전부 이월.

### 1.3 선행 게이트

- 없음 — 필요 실측(A/B 신호, rollout 스키마·UUID 매칭·중복, exec 계열
  부재, 파일 축 성장률)은 브레인스토밍 세션에서 선행 완료(§7).

## 2. Codex usage 어댑터 (D44)

- 소스 루트: `~/.codex/sessions`(존재하지 않으면 cx: 집계만 생략하고
  리포트에 "rollout 루트 없음" 표기 — usage 자체는 성공). 연·월·일
  하위 디렉터리 재귀 순회, `rollout-*.jsonl`만 채택.
- 파일당 파싱: `session_meta`의 `session_id`가 세션 권위 키(재개 세션은
  같은 파일에 meta 복수 기록 — id 동일 실측 §7; 파일명 UUID는 발견
  수단이며 불일치 시 meta 우선+집계). 토큰 합산은 **meta 구간별 delta
  누적**: `token_count.info.total_token_usage`는 구간 내 누적 필드이므로
  각 meta 구간의 마지막 값을 취해 구간 합으로 세션 총계를 구성하고,
  구간 내 값 감소는 카운터 리셋으로 간주해 새 구간으로 분리한다
  (`[100,200] → meta → [20,50]` = 250 — 단조성 전제 미실측이므로 계약이
  리셋을 흡수, 적대 검수 수렴 §9). `input_tokens`는
  `cached_input_tokens`를 포함하는 상위 값(실측 §7) — 열 정의에 반영.
- 프로젝트 필터: **모든** meta의 cwd가 정규화(소문자·구분자 통일 —
  `ident.Fold` 단일점, **RealPath 비적용**: rollout cwd는 미해석 원형
  이므로 미해석 워크트리 루트와 Fold끼리 비교, cc: transcript 경로
  유도의 Canonicalize 회피 선례와 동형) 후 루트 동일 또는 루트+"/"
  접두일 때만 채택. cwd 발산(meta 간 상이 프로젝트) 파일은 제외+집계.
  scratchpad·타 프로젝트 rollout은 자연 배제. junction/symlink 별칭
  경로는 매칭 실패 가능 — §5 한계(cc: 쪽과 동형 한계).
- arm 판정: 채택 파일의 session_id로 프로젝트 **`worktrees/*` 전체
  session.db 순회 병합** 위에서 `cx:<uuid>` 존재 조회(D40 ③ 정합 —
  [15] 접두 분해와 같은 순회 패턴, 단일 worktree 조회 재사용 금지) —
  존재=hooks:on. 부재는 **관측 창 게이트** 통과 시(세션 시작이 30일 —
  훅 retention 기본 — 이내)만 hooks:off, 창 밖은 unknown 계상·제외.
  cc:의 판정과 접두·스코프가 다르므로 공유는 존재 조회 술어까지만
  (전 판정 로직의 무조건 공유 아님).
- 역방향 커버리지: 순회 병합된 session.db의 cx: 세션 중 rollout
  비매칭(§7 exec 계열)은 hooks:on이되 토큰 `n/a` — D45 커버리지 라인의
  K로 계상.
- 오류 규율: 파일 단위 실패(잘린 JSON·권한)는 해당 파일 스킵 + 스킵 수
  집계(침묵 탈락 금지), 전체 실패로 승격하지 않음(cc: 파서와 동일).
  형식 변경(필드 부재·타입 불일치)도 같은 규율로 스킵 강등되며 스킵
  수가 0이 아니면 D45가 비율 옆에 경고를 표기한다(조용한 cohort 왜곡
  차단 — 적대 검수 §9).

## 3. 비교 리포트 (D45)

- 표면: `usage --compare` 플래그(신규 서브커맨드 없음). 리포트 전용
  출력 — 세션별 본표 미출력. `--transcripts`·`--min-records`와 조합
  가능, `--totals`와는 상호 배타(둘 다 지정 시 오류 — 표 의미가 다름).
- 출력 구조(열 너비·정렬은 계획 단계 확정, 의미 계약만 고정):
  1. cc 블록: arm별 sessions·records·output/rec·cache_read/rec 행 2개 +
     비율 행(on/off — cache_read/rec, output/rec).
  2. cx 블록(experimental 표시): arm별 sessions·turns·input/turn·
     cached_input/turn·output/turn 행 2개 + 비율 행(옆에 exec 계통 제외
     캐비앗) + 커버리지 라인 양 arm(on: 등록 N·매칭 M·n/a K / off: 창
     내 미등록 J·unknown U·스킵 S; rollout 루트 없으면 그 표기).
  3. 고정 캐비앗 1행: 관찰 데이터(무작위화 아님) — 워크로드 교란 존재.
- `--min-records N`: cc는 records, cx는 turns 기준으로 N 미만 세션을
  집계에서 제외(제외 수를 블록에 표기 — 침묵 탈락 금지). 기본 0.
- 빈 arm(세션 0)은 행을 비우지 않고 0 표기, 비율 행은 분모 0이면 `n/a`.
- 정렬·재현성: 동일 입력에 동일 바이트 출력(맵 순회 순서 비의존 —
  버킷 정렬 키는 session_id 사전순 고정).

## 4. 파일 축 경고 (D46)

- [14] 기존 blob 경고 라인 뒤에 조건 독립으로 파일 축 라인 추가:
  `[14] warning: file <n>B > 임계 <n>B(CTR_CONTENT_FILE_WARN_BYTES) —
  청크 텍스트+FTS 축; purge 행 삭제 후에도 free page로 즉시 줄지 않음,
  회수는 VACUUM(라이브 서버 제약 — 서버 비가동 시 수행)`. 문구는 계획
  단계에서 확정하되 축 특성·구제 경로 명시는 계약.
- 임계 키는 blob(`CTR_STORE_WARN_BYTES`)과 **분리** —
  `CTR_CONTENT_FILE_WARN_BYTES`, 기본 100MiB, 파싱 규율은 기존
  storeWarnBytes와 동일(양수만 채택). 분리 사유는 D46 결정 이력 참조.
  두 라인은 각자 조건 평가(둘 다 발화 가능).
- 현재 실측 82.5MB < 100MiB 기본 임계 — 릴리스 시점 미발화가 정상이나
  성장률(§7)상 조기 발화 가능성이 있으며 그것이 가시화 목적. 발화 시
  조치는 경고 문구의 구제 경로(수동).

## 5. 한계 (v0.6 명문화)

- 관찰 A/B는 인과 추정이 아니다 — 세션 배정이 무작위가 아니고 워크로드·
  시기·모델 구성이 교란 변수. 비율은 신호이지 효과 크기 단정이 아님
  (캐비앗 상시 출력이 계약, D45).
- cx: 토큰 축은 부분 커버리지이며 **계통 편향이 있다** — exec 계열
  (리뷰 잡)은 rollout을 남기지 않아(§7 실측 3/3) on-arm에서는 n/a로
  보이지만 **off-arm에서는 흔적 자체가 없어 dark count 정량 불가**.
  따라서 cx: 비율은 양 arm 모두 경량(대화형) 세션 표본에서 산출된다 —
  D45가 비율 옆 캐비앗으로 상시 고지. Codex 측 기록 방식이 바뀌면
  커버리지가 변한다.
- rollout 형식은 Codex CLI의 비공표 내부 형식 — 필드 개명·구조 변경 시
  파서가 스킵 집계로 강등된다(§2). 스킵>0 경고가 유일한 방어이며 표본
  구성 변화(구버전 파일만 잔존)까지는 탐지하지 못한다 — cx 블록의
  experimental 표시가 이 등급을 표명. 버전 고정은 하지 않는다(로컬
  도구 특성상 사용자 Codex 버전 추종).
- arm 판정의 관측 창 게이트(30일)는 훅 retention **기본값** 기준 —
  사용자가 retention을 짧게 표명한 세션은 창 내에서도 off 오분류
  가능(잔여 위험, unknown 확대는 표본 축소와 상충해 비채택).
- cc:와 cx:의 단위(record vs turn)는 등가가 아니다 — 채널 간 비교를
  출력이 유도하지 않도록 표를 분리(D45).
- cwd 매칭은 미해석 경로 Fold 비교 — junction/symlink 별칭으로 연
  세션은 누락 가능(cc: transcript 경로 유도와 동형 한계).
- content.db 파일 축 경고는 자문 성격 — 발화해도 자동 조치 없음(회수
  경로는 수동, §4 문구).

## 6. 검증 계약 (계획 단계 상세화)

- D44 파싱: 합성 rollout testdata(정상·token_count 부재·cwd 불일치·
  잘린 JSON·meta 복수 기록·**구간 리셋 `[100,200]→meta→[20,50]`=250**·
  **비단조 시퀀스**·**meta 간 cwd 발산=파일 제외**·파일명-meta id
  불일치) 단위 테스트 — 구간 delta 누적·meta 권위·스킵 집계 단정.
  cwd 정규화 경계(대소문자·구분자·접두 vs 동일·trailing separator).
- D44 arm: rollout UUID ↔ `cx:` 조인이 **worktrees/* 순회 병합** 위에서
  동작(타 worktree 등록 세션의 on 판정) + 역방향 커버리지 K 계상 +
  **관측 창 게이트**(30일 밖 미등록=unknown, 이내=off) 단정.
- D45: --compare 집계 산술(비율·분모 0·--min-records 제외 수·양 arm
  커버리지 M/K/J/U/S) 단정 + **반복 실행 동일 바이트 출력**(결정론 —
  정렬 키 고정 검증) + rollout 루트 부재=성공+표기 경로 +
  `--transcripts` 조합 + 무플래그·--totals 출력 byte-for-byte 불변
  회귀 + --totals/--compare 배타 오류.
- D46: 파일 축 경고 발화/미발화 임계 테스트(`CTR_CONTENT_FILE_WARN_
  BYTES` 소액 설정 발화, blob 키와 상호 독립 — 한쪽 조정이 다른 축
  무영향 단정, 기존 blob 경고 테스트 패턴 재사용).
- 전체 회귀 `go test -p 1`(메모리 캡 규율). 도그푸딩 수용 기준: 실데이터
  에서 `usage --compare` 1커맨드로 cc:/cx: on/off 표+커버리지+캐비앗
  출력, cx: 토큰 총계가 rollout 실측(§7)과 일치.

## 7. 관측 실측 기록 (2026-07-22, 브레인스토밍 세션 — 컨트롤러 수행)

- usage 관찰 A/B(수작업 집계): hooks:on 21세션·2,207records —
  cache_read/rec 178,671·output/rec 1,687; hooks:off 17세션·3,960records
  — cache_read/rec 298,423·output/rec 1,976. 비 0.599/0.854. 중량
  (records≥100)만: on 9세션 184,714 vs off 11세션 293,111 — 비 0.630.
- cx: 축적: 9세션·203이벤트(session_start 9·tool_call 148·
  artifact_created 22·tool_result_summary 22·test_run 1·git_diff 1) —
  전역([15]식 worktrees/* 병합) 집계이며 이 중 1세션은 현재 worktree
  밖 session.db 소재(단일 worktree 조회와의 9 vs 8 갭 — D44 arm 순회
  근거). 중량 3세션(각 61~65이벤트 — Codex 리뷰 실행분), tool_call
  148건 전부 "Bash: …" 형태(Codex 표면=exec — D35 후속의 실증 소재),
  shadow 768KB.
- rollout 실측: `~/.codex/sessions` 3,754파일·UUID 중복 0.
  `session_meta.payload{session_id,cwd}` + `event_msg/token_count.info.
  total_token_usage{input,cached_input,cache_write,output,reasoning_
  output,total}` 누적(input⊇cached_input; 재개 meta 경계의 누적 단조성
  은 미실측 — D44가 구간 delta 계약으로 흡수). 프로젝트 루트 cwd 일치
  7파일 — 경량 cx: 5세션 매칭 5/5(hooks:on), 미등록 2파일(hooks:off
  arm 실례); 중량(companion 리뷰 잡, exec) 3세션 rollout 부재 3/3 —
  부분 커버리지의 근거. scratchpad 프로브 3파일은 루트 접두 필터로
  자연 배제 확인. cwd 형식은 `C:\Users\…` 원형(정규화 필요).
- doctor(0.5.1): [6] sessions=94(empty=68 — 7일 GC 회수 관측 계속),
  [12] drops total=325(unknown-session=324 — 세션 20과 동일, 증가 정지),
  [14] sources=358 artifacts=471 blob=22,416,690B file=82,481,152B(v0.5
  설계 시점 52.9MB → 약 하루 +56%), [15] cc:=15.4MB cx:=768KB shared=0
  unattributed=0. 경고 임계 평가는 blob 축만(cli.go 실코드 확인) —
  D46의 사각지대 근거.

## 8. 의도적 미결 (v0.7+ 후보)

무작위 A/B 하네스·OTel(D27 — 관찰 축적 후), content.db 파일 축 회수
자동화(D46 경고 실발화 후), exec 3종(D21 트랙), Codex 가드 동등성(D35
후속 — cx: tool_call 전수 Bash류 실증 §7), 서브에이전트 캡처(호스트
표면 필요), register-on-first-event(D43 — 재발 실측 후), Grep 도구
가드, plugin manifest, semantic 보강(recall@k 기준선 후), spill
journal(재상정 조건 v0.3 §1.3), `repository{}` 기입, `invalidates`,
payload 필드 조회(virtual generated column), title dedup, CAS 갱신 시
구버전 blob 즉시 orphan-GC(실해 미관측), [15] 접두 신규 네임스페이스
대응, cx: 원시 행 usage 본표 편입(소비자 발생 시), cx: arm 불변
manifest(무작위 하네스 도입 시 동반 재상정).

## 9. 적대 검증 처리 기록 (2026-07-22, 설계 체크포인트)

- 이중 적대 검수 1패스(초안 ea7d022 대상): 서브에이전트(opus) C1·I5·M6
  + Codex adversarial-review(high 3·medium 2, verdict NO-SHIP) — 전 건
  판정·반영, NO-SHIP 사유 해소.
- 수렴 3건(양측 공통): ① last-wins는 재개 시 누적 카운터 리셋에서
  과소계상 — meta 구간별 delta 누적으로 계약 재정의 + 리셋 회귀
  테스트(D44·§2·§6) ② D46의 blob 임계 키 공유는 D40 ①이 분리한 두
  축을 재결합 — 파일 축이 3.7배 크고 빨라 기준 축 지배·상호 무력화,
  전용 키 `CTR_CONTENT_FILE_WARN_BYTES`로 분리(D46·§4) ③ cwd 규율
  공백(첫 meta 채택의 발산 미처리·lexical 접두·RealPath 모순 소지) —
  전 meta 루트 하위 요구+발산 제외+미해석 Fold 비교로 확정, junction
  별칭은 한계 명문화(D44·§2·§5).
- 서브에이전트 고유: ① **Critical** — arm 판정의 단일 worktree 조회
  재사용은 D40 ③ 재위반(하위 디렉터리 기동 cx: 세션의 on→off 오분류 +
  토큰 오배정; 실측 9 vs 8 갭이 이미 노출) → worktrees/* 순회 병합으로
  교체, 실측 기록에 스코프 주석 추가(§2·§7) ② 검증 계약 4건 공백(arm
  조인·루트 부재 경로·--compare 결정론·--transcripts 조합) → §6 보강
  ③ --compare 출력 범위 모호 → 리포트 전용(본표 대체)으로 확정(D45)
  ④ UUID 권위 모호 → session_meta.session_id 권위 확정(§2) ⑤ 백분율
  반올림(22%→21%) 등 문면 교정.
- Codex 고유: ① arm 판정의 retention 휘발성 — 30일 스윕 후 과거
  hooks:on 세션이 off로 전이(MNAR) → 관측 창 게이트+unknown 버킷
  신설(D44·§2), 불변 manifest 권고는 새 저장 표면이라 기각(§1.2 —
  무작위 하네스 도입 시 동반 재상정 §8) ② off-arm exec dark count
  정량 불가 — 커버리지 양 arm 확장+비율 옆 계통 제외 캐비앗+§5 한계
  명문화(D45) ③ 형식 의존 위험은 필드 수 한정으로 해소 안 됨 —
  번복 근거 서열 교정(수요·신호가 실질, 최소 계약은 표면 한정),
  experimental 표시+스킵>0 경고 채택; 버전별 fixture·형식 식별자는
  기각(버전 추종 원칙), "커버리지 대칭 게이트 미충족 시 비율 미출력"
  권고는 하향(관찰 데이터 전제와 과잉 차단 — 캐비앗·커버리지 상시
  표기로 대체).
- 무발견 확인(검수 검증 항목): 무플래그 byte-for-byte 게이트와
  --compare 양립(opt-in 플래그·본표 경로 불변), --totals/--compare
  배타 구현 자명성, `ident.Fold` 단일점 실재, cx: LIKE 조회 API
  실현성, D46이 D40 ① 기준 축을 대체하지 않음(문면 무모순), 기존
  doctor [14] 테스트 생존(Contains 기반·소형 픽스처), SizeStat.
  FileBytes 가용, 손상 라인 스킵 규율의 cc: 파서 정합.
