# Context Router v0.6 설계서 (측정 성숙 — D27 트랙)

- 전제: v0.5.1 태그(main 8cdec1e, PR #16, CI 3-OS GREEN), 도그푸딩 marker
  0.5.1(`.mcp.json` bare 등록), Codex는 훅 A/B만 유지(MCP 등록 제거).
  실측(2026-07-22, §9): usage 관찰 A/B 첫 신호 — cache_read/record
  hooks:on/off 비 0.60(중량 세션만 0.63), cx: 9세션·203이벤트(중량 3),
  [12] drops 324 증가 정지(스테일 차단 후 재발 0), content.db 파일
  82.5MB(100MiB의 79% — 현행 경고는 blob 축만), blob 22.4MB(임계 22%).
- 스코프 확정 근거: session-21 브레인스토밍 → 사용자 결정 4건(축=측정
  성숙 / 구성=usage 비교 리포트+Codex usage 어댑터, 무작위 하네스·OTel
  이월 / content.db 파일 축은 경고만 편승 / 접근=단일 usage 확장 —
  신규 명령·MCP 도구 0개).

## 0. 결정 이력 (v0.6 신규 — D43까지는 v0.5 설계서·이전 델타 체인)

- **D44** Codex usage 어댑터: `~/.codex/sessions/YYYY/MM/DD/
  rollout-<ts>-<uuid>.jsonl` 읽기 전용 파싱으로 cx: 토큰 축 신설.
  **v0.4 기각(§1.2 "세 번째 저장 형식 계약"·사용자 결정 ①)의 명시적
  번복**이며 번복 근거는 세 가지: ① D27의 재상정 조건("수동 측정 신호
  확인 후") 첫 충족 — cc: 관찰 A/B가 신호를 보였고(§9), 테제 검증을 둘째
  채널로 확장하려면 cx: 토큰 축이 필요하다. ② 계약 비용의 실측 한정 —
  v0.4 시점엔 미지 형식이었으나 로컬 실측(§9)으로 소비 필드가 2종뿐임을
  확인: `session_meta.payload{session_id,cwd}` + `event_msg/token_count.
  info.total_token_usage`(누적치 — last-wins). 전체 rollout 스키마가 아닌
  최소 계약이다. ③ cx: 축적 데이터 활용처가 본 버전의 지정 소재(사용자).
  파싱 계약: cwd 정규화(소문자·`\`→`/`) 후 워크트리 루트와 동일하거나
  루트 하위인 파일만 채택, 파일당 마지막 `token_count`의
  `total_token_usage`가 세션 총계(전 파일 UUID 중복 0 실측 — 파일=세션),
  손상 라인은 스킵(cc: transcript 파서와 동일 규율). hooks 판정은 cc:와
  동일 로직 공유 — rollout UUID가 session.db `cx:<uuid>`로 존재 ⟺
  hooks:on. **부분 커버리지 계약**: cx: 등록 세션인데 rollout 부재(§9 —
  exec 계열 3/3 미기록 실측)면 토큰 축 `n/a`로 별도 카운트해 표기한다.
  침묵 탈락 금지. 소비자는 D45 `--compare`뿐 — 무플래그 `usage` 본표는
  cc: transcript 열거 그대로(byte-for-byte 불변 게이트 승계, v0.3 §8).
- **D45** `usage --compare` 비교 리포트: 채널(cc/cx)×arm(hooks:on/off)
  집계표 + on/off 비율 + 고정 캐비앗을 한 커맨드로 출력(session-21에서
  수작업으로 수행한 계산의 제품화). 열은 채널별로 의미가 다르다 — cc:
  단위=record(어시스턴트 메시지), 열=output/rec·cache_read/rec; cx:
  단위=turn(`token_count` 이벤트 수), 열=output/turn·cached_input/turn·
  input/turn. **채널 간 교차 비교는 계약 밖**(단위 상이 — 표가 채널을
  분리 유지). `--min-records N`(기본 0)으로 중량 세션 필터 재현. cx:
  커버리지 라인(매칭 M/전체 N, n/a K) 필수. 마지막 줄 고정 캐비앗:
  "관찰 데이터(무작위화 아님) — 워크로드 교란 존재". 교란 통제(무작위
  배정)는 명시적 비범위(§1.2) — 1인 도그푸딩에서 세션 절반의 가드·shadow
  혜택을 포기하는 운영 비용이 커, 관찰 축적 후 재상정. 기존 무플래그·
  `--totals` 출력은 byte-for-byte 불변(기존 게이트 그대로).
- **D46** doctor [14] 파일 축 경고: `file > CTR_STORE_WARN_BYTES`(blob과
  동일 키) 시 **별도 경고 라인** 추가. D40 ①의 "D38 경고 평가 기준=blob"
  결정은 대체하지 않는다 — blob 경고는 "purge → 감소 → 해소" 일관이
  성립하는 기준 축으로 유지하고, 파일 축 라인은 자문(advisory) 성격으로
  병행한다. 문구에 축 특성을 정직하게 명시: 청크 텍스트+FTS 축이며 행
  삭제 후에도 free page 탓에 즉시 줄지 않고 VACUUM이 필요하나 라이브
  서버 제약이 있다(D41 §7 병기 승계). 회수 자동화는 비범위 — 경고
  실발화 후 재상정(D38 선례 패턴).

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
  이번 확인에서 미충족: drops 324로 증가 정지(§9).
- codex 플러그인·companion 수정 — 외부 플러그인 불변 원칙. exec 계열
  rollout 부재는 어댑터의 n/a 계약으로 흡수(D44).
- cx: 원시 행의 무플래그 `usage` 본표 편입 — byte-for-byte 게이트 유지.
  소비자가 생기면 그때 플래그로 재상정.
- exec 3종(D21)·Codex 가드 동등성(D35 후속)·서브에이전트 캡처·Grep 가드·
  plugin manifest·semantic 보강·spill journal 등 §8 잔여 — 전부 이월.

### 1.3 선행 게이트

- 없음 — 필요 실측(A/B 신호, rollout 스키마·UUID 매칭·중복, exec 계열
  부재, 파일 축 성장률)은 브레인스토밍 세션에서 선행 완료(§9).

## 2. Codex usage 어댑터 (D44)

- 소스 루트: `~/.codex/sessions`(존재하지 않으면 cx: 집계만 생략하고
  리포트에 "rollout 루트 없음" 표기 — usage 자체는 성공). 연·월·일
  하위 디렉터리 재귀 순회, `rollout-*.jsonl`만 채택.
- 파일당 파싱: 첫 `session_meta`에서 `session_id`·`cwd` 추출(재개 세션은
  같은 파일에 meta가 복수 기록됨 — 첫 것 채택, id 동일 실측), 이후
  `event_msg/token_count`를 추적해 마지막 `info.total_token_usage`를
  세션 총계로 채택(누적 필드 — last-wins가 곧 합계). `input_tokens`는
  `cached_input_tokens`를 포함하는 상위 값(실측 §9) — 열 정의에 반영.
- 프로젝트 필터: cwd를 프로젝트 식별자와 동일 규칙으로 정규화(소문자·
  구분자 통일 — 기존 헬퍼 재사용, 단일점)한 뒤 워크트리 루트 동일 또는
  루트+"/" 접두 일치만 채택. scratchpad·타 프로젝트 rollout은 자연 배제.
- arm 판정: 채택 파일의 UUID로 session.db `cx:<uuid>` 존재 조회 —
  존재=hooks:on, 부재=hooks:off. cc:의 transcript↔`cc:` 판정과 좌우
  대칭이며 판정 함수는 접두만 매개변수화해 공유(중복 구현 금지).
- 역방향 커버리지: session.db의 cx: 세션 중 rollout 비매칭(§9 exec 계열)
  은 hooks:on이되 토큰 `n/a` — D45 커버리지 라인의 K로 계상.
- 오류 규율: 파일 단위 실패(잘린 JSON·권한)는 해당 파일 스킵 + 스킵 수
  집계(침묵 탈락 금지), 전체 실패로 승격하지 않음(cc: 파서와 동일).

## 3. 비교 리포트 (D45)

- 표면: `usage --compare` 플래그(신규 서브커맨드 없음). `--transcripts`·
  `--min-records`와 조합 가능, `--totals`와는 상호 배타(둘 다 지정 시
  오류 — 표 의미가 다름).
- 출력 구조(열 너비·정렬은 계획 단계 확정, 의미 계약만 고정):
  1. cc 블록: arm별 sessions·records·output/rec·cache_read/rec 행 2개 +
     비율 행(on/off — cache_read/rec, output/rec).
  2. cx 블록: arm별 sessions·turns·input/turn·cached_input/turn·
     output/turn 행 2개 + 비율 행 + 커버리지 라인(hooks:on N 중 rollout
     매칭 M, n/a K; rollout 루트 없으면 그 표기).
  3. 고정 캐비앗 1행: 관찰 데이터(무작위화 아님) — 워크로드 교란 존재.
- `--min-records N`: cc는 records, cx는 turns 기준으로 N 미만 세션을
  집계에서 제외(제외 수를 블록에 표기 — 침묵 탈락 금지). 기본 0.
- 빈 arm(세션 0)은 행을 비우지 않고 0 표기, 비율 행은 분모 0이면 `n/a`.
- 정렬·재현성: 동일 입력에 동일 바이트 출력(맵 순회 순서 비의존).

## 4. 파일 축 경고 (D46)

- [14] 기존 blob 경고 라인 뒤에 조건 독립으로 파일 축 라인 추가:
  `[14] warning: file <n>B > 임계 <n>B(CTR_STORE_WARN_BYTES) — 청크
  텍스트+FTS 축; 행 삭제 후에도 free page로 즉시 줄지 않음(VACUUM 필요·
  라이브 서버 제약)`. 문구는 계획 단계에서 확정하되 축 특성·한계 명시는
  계약.
- 임계 키는 blob과 동일(`CTR_STORE_WARN_BYTES`) — 새 환경 변수를 만들지
  않는다(표면 최소화). 두 라인은 각자 조건 평가(둘 다 발화 가능).
- 현재 실측 82.5MB < 100MiB 기본 임계 — 릴리스 직후 미발화가 정상이며,
  성장 지속 시 가시화가 목적.

## 5. 한계 (v0.6 명문화)

- 관찰 A/B는 인과 추정이 아니다 — 세션 배정이 무작위가 아니고 워크로드·
  시기·모델 구성이 교란 변수. 비율은 신호이지 효과 크기 단정이 아님
  (캐비앗 상시 출력이 계약, D45).
- cx: 토큰 축은 부분 커버리지 — exec 계열(리뷰 잡)은 rollout을 남기지
  않아 n/a(§9 실측 3/3). Codex 측 기록 방식이 바뀌면 커버리지가 변한다
  (외부 형식 의존은 D44 최소 계약으로 한정).
- rollout 형식은 Codex CLI의 비공표 내부 형식 — 필드 개명·구조 변경 시
  파서가 스킵 집계로 강등될 수 있다(오류 규율 §2). 버전 고정은 하지
  않는다(로컬 도구 특성상 사용자 Codex 버전 추종).
- cc:와 cx:의 단위(record vs turn)는 등가가 아니다 — 채널 간 비교를
  출력이 유도하지 않도록 표를 분리(D45).
- content.db 파일 축 경고는 자문 성격 — 발화해도 자동 조치 없음(회수
  경로는 D41 purge 계열 + VACUUM 수동, §4 한계 문구).

## 6. 검증 계약 (계획 단계 상세화)

- D44: 합성 rollout testdata(정상·token_count 부재·cwd 불일치·잘린
  JSON·meta 복수 기록) 파싱 단위 테스트 — last-wins·첫 meta 채택·스킵
  집계 단정. cwd 정규화 경계(대소문자·구분자·접두 vs 동일) 케이스.
- D45: --compare 집계 산술(비율·분모 0·--min-records 제외 수·커버리지
  M/K) 단정 + 무플래그·--totals 출력 byte-for-byte 불변 회귀 +
  --totals/--compare 배타 오류.
- D46: 파일 축 경고 발화/미발화 임계 테스트(기존 blob 경고 테스트 패턴
  재사용 — CTR_STORE_WARN_BYTES 소액 설정으로 발화 확인, 두 라인 독립).
- 전체 회귀 `go test -p 1`(메모리 캡 규율). 도그푸딩 수용 기준: 실데이터
  에서 `usage --compare` 1커맨드로 cc:/cx: on/off 표+커버리지+캐비앗
  출력, cx: 토큰 총계가 rollout 실측(§9)과 일치.

## 7. 관측 실측 기록 (2026-07-22, 브레인스토밍 세션 — 컨트롤러 수행)

- usage 관찰 A/B(수작업 집계): hooks:on 21세션·2,207records —
  cache_read/rec 178,671·output/rec 1,687; hooks:off 17세션·3,960records
  — cache_read/rec 298,423·output/rec 1,976. 비 0.599/0.854. 중량
  (records≥100)만: on 9세션 184,714 vs off 11세션 293,111 — 비 0.630.
- cx: 축적: 9세션·203이벤트(session_start 9·tool_call 148·
  artifact_created 22·tool_result_summary 22·test_run 1·git_diff 1),
  중량 3세션(각 61~65이벤트 — Codex 리뷰 실행분), tool_call 148건 전부
  "Bash: …" 형태(Codex 표면=exec — D35 후속의 실증 소재), shadow 768KB.
- rollout 실측: `~/.codex/sessions` 3,754파일·UUID 중복 0.
  `session_meta.payload{session_id,cwd}` + `event_msg/token_count.info.
  total_token_usage{input,cached_input,cache_write,output,reasoning_
  output,total}` 누적(마지막=총계, input⊇cached_input). 프로젝트 루트
  cwd 일치 7파일 — 경량 cx: 5세션 매칭 5/5(hooks:on), 미등록 2파일
  (hooks:off arm 실례); 중량(companion 리뷰 잡, exec) 3세션 rollout
  부재 3/3 — 부분 커버리지의 근거. scratchpad 프로브 3파일은 루트 접두
  필터로 자연 배제 확인. cwd 형식은 `C:\Users\…` 원형(정규화 필요).
- doctor(0.5.1): [6] sessions=94(empty=68 — 7일 GC 회수 관측 계속),
  [12] drops total=325(unknown-session=324 — 세션 20과 동일, 증가 정지),
  [14] sources=358 artifacts=471 blob=22,416,690B file=82,481,152B(v0.5
  설계 시점 52.9MB → 약 하루 +56%), [15] cc:=15.4MB cx:=768KB shared=0
  unattributed=0. 경고 임계 평가는 blob 축만(cli.go 실코드 확인) —
  D46의 사각지대 근거.

## 8. 의도적 미결 (v0.7+ 후보)

무작위 A/B 하네스·OTel(D27 — 관찰 축적 후), content.db 파일 축 회수
자동화(D46 경고 실발화 후), exec 3종(D21 트랙), Codex 가드 동등성(D35
후속 — cx: tool_call 전수 Bash류 실증 §9), 서브에이전트 캡처(호스트
표면 필요), register-on-first-event(D43 — 재발 실측 후), Grep 도구
가드, plugin manifest, semantic 보강(recall@k 기준선 후), spill
journal(재상정 조건 v0.3 §1.3), `repository{}` 기입, `invalidates`,
payload 필드 조회(virtual generated column), title dedup, CAS 갱신 시
구버전 blob 즉시 orphan-GC(실해 미관측), [15] 접두 신규 네임스페이스
대응, cx: 원시 행 usage 본표 편입(소비자 발생 시).
