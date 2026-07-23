# Context Router v0.10 설계서 (D53–D57 — 서브에이전트 캡처·Grep 가드·hook-only VACUUM 소급·버전 중앙화·수렴 로드맵)

- 전제: v0.9.0 태그(main 7279a48, PR #21, CI 3-OS GREEN), 도그푸딩
  marker 0.9.0(cc·codex project+user), Codex `/hooks` 재신뢰 완료 +
  재신뢰 전후 `[mcp_servers.ctr]` 블록 생존 확인(§9-2 지속 — 2026-07-23
  line 515 실존). 관찰(2026-07-23, session-26, §3): **D51 실효 확정**
  — 바이너리 설치(14:24:01 KST) 이후 전 프로젝트 unknown-session
  신규 발생 0(본 프로젝트 399 불변·sufficode 1536에서 정지·ws 23·
  memory 6·matcher 5 전부 불변, 마지막 drop 전부 설치 전 시각),
  재신뢰 직후(14:31 KST) 양 프로젝트 cx session_start 정상 등록
  실관측, 합성 등록 0건(unknown 세션 자체가 미발생 — D51 합성
  경로는 대기 상태가 정상), compare `synthetic=0` 필드 cc·cx 양
  블록 실표시. D46 116,740,096B 발화 지속(회수는 사용자 결정 대기),
  empty=84(07-21 등록분 46건이 최대 덩어리 — GC ≈07-28+), cx arm
  n 정체 불변(on 5세션·10턴).
- 스코프 확정 근거: session-26 브레인스토밍 — 사용자 결정 5건: ①
  축=**서브에이전트 캡처(생애주기+이벤트 표식) + 소형 2건(Grep
  가드·hook-only VACUUM 소급)**(트리거 조건부 후보들은 전부 조건
  미충족으로 자연 배제 — synthetic 표식 영속화=오분류 실증 없음·
  doctor drops 집계=재증가 없음·공유 락/sweep 자동화=선행 실증
  미충족) ② 서브에이전트 스코프=생애주기, 이후 실측으로 PostToolUse
  `agent_id` 실림 확정되어 **표식 병기 승격**(사용자 재확인) ③
  버전 중앙화 추가 — 사용자 지정 접근(internal/buildinfo·-dev
  스킴·VCS 진단 doctor 한정·릴리스 CI 태그 검증) ④ VACUUM 소급
  rc 정책=본경로 동일(rc≠0) ⑤ **장기 방향=수렴**(context-router가
  종착지, ctxscribe는 브리지 — D57로 문서화).
- 착수 조건 처리: 서브에이전트 캡처의 "호스트 표면 필요"(v0.9 §4
  이월 조건)는 이 세션 실측으로 해소 — SubagentStart/SubagentStop
  훅 인풋과 서브에이전트발 PostToolUse의 `agent_id`/`agent_type`
  필드를 실덤프로 확정(§3). Grep 가드의 조건 성립 여부(head_limit
  전달 방식)도 동일 실측으로 확정.

## 0. 결정 이력 (v0.10 신규 — D52까지는 v0.9 설계서·이전 델타 체인)

- **D53** 서브에이전트 캡처 — 생애주기 이벤트 + 이벤트 표식(cc
  전용):
  - **훅 표면**: hook install의 cc 이벤트 4→6 — `SubagentStart`
    (matcher "")·`SubagentStop`(matcher "") 추가. settings.json 자기
    그룹 내용이 변하므로 도그푸딩은 `hook install` 재실행으로 갱신
    (marker는 버전 문자열뿐이라 형식 불변). **cx 비범위** — Codex에
    해당 훅 표면이 없다.
  - **이벤트 2종 신설**: `subagent_start` / `subagent_stop`.
    classify에 HookEventName 분기를 더하고 dispatch에 PreToolUse
    분기와 동렬의 처리 분기를 추가한다(buildEvent→Append 경유,
    1 호출 = 1 이벤트). summary는 `subagent started: <agent_type>` /
    `subagent stopped: <agent_type>`로만 조립(관례 §3 — 원시 인자·
    응답 본문 불포함). **agent_type 빈 문자열 실관측**(§3) →
    빈 값이면 `subagent started`처럼 접미 생략(빈 summary 방지 —
    buildEvent의 summary=="" 드롭 계약과 무충돌). attrs =
    `{agent_id, agent_type}`(빈 값도 기록 — 판별용 원자료).
  - **비수용 명문**: SubagentStop의 `last_assistant_message`(최종
    응답 본문)·`agent_transcript_path`는 기록하지 않는다 — summary
    는 FTS 색인 대상이라 응답 본문 불포함이 비밀 운반 차단 1차
    방어(기존 관례)이고, 서브에이전트 내부 도구 출력은 이미
    PostToolUse 경로로 이벤트·shadow 캡처되고 있음이 실측 확정
    (§3 — 세션-25 SDD 세션의 구현자 file_edit 70건 실존). 수용
    재상정은 §4.
  - **이벤트 표식 병기**: hookInput에 `agent_id`/`agent_type` 필드
    2개를 파싱해, 존재 시 PostToolUse/PostToolUseFailure 기본
    이벤트의 attrs에 병기한다. 판별 계약(실측 §3): 서브에이전트
    내부 도구 호출에만 두 필드가 실리고 부모 세션 호출에는
    부재한다 — attrs 부재=부모발, 존재=서브발. PreToolUse 가드
    경로는 이벤트를 만들지 않으므로 무관(warning 이벤트에는 병기
    하지 않는다 — 가드 문면 최소 변경).
  - **상호작용 명문**: ① doctor [6] empty(session_start 단독)
    판정 — subagent_start가 기록된 세션은 empty에서 빠진다.
    서브에이전트를 실제 spawn한 세션이므로 의도된 방향. ② usage
    --compare records는 CC transcript(.jsonl)의 usage record 수라
    session_events 행 증가와 무관(실측 §3 — A/B 계측 무영향). ③
    D51과 동일 dispatch 파이프라인 통과 — 부모 미등록 엣지는
    합성 등록이 커버(신규 경로 없음). ④ SubagentStart/Stop에는
    tool_response가 없어 shadow 비대상(자연 — 코드 분기 불요).
- **D54** Grep 도구 가드(PreToolUse — v0.2 이월 "Bash/Grep 출력
  가드"의 Grep 절반):
  - **matcher**: `Read|Bash|PowerShell` → `Read|Bash|PowerShell|Grep`
    (hook install cc PreToolUse 매처 — settings 내용 변경, 재설치
    필요는 D53과 합류).
  - **deny 조건은 정확히 하나**: tool_input의
    `output_mode=="content"` **그리고** `head_limit` 필드가
    존재하며 값이 0(무제한 명시). 실측 근거(§3): head_limit
    미지정 시 tool_input에 필드 자체가 부재(호스트 기본 250 캡이
    적용되므로 안전)하고, 명시 0만이 무제한 전량 반환이다.
    부재/명시 0 구분은 `*int64` 포인터 파싱(기존 readGuardInput
    offset/limit 관례 재사용). 그 외 전 조합(-A/-B/-C 대값,
    files_with_matches, count, head_limit>0, 파싱 불가)은 전부
    통과 — allow-bias 관례(불확실하면 통과) 유지.
  - **deny 동작**: 기존 가드와 동형 — permissionDecision deny +
    reason(`head_limit` 지정 또는 `ctr_search` 대체 안내), warning
    이벤트 1건 append(guard-append/guard-store drop 계약 포함
    기존 가드 파이프라인 그대로).
  - **cx 무관**: Codex에 Grep 내장 도구가 없다(cx 훅은 Bash 계열
    가드만 — 기존 D35 계약 불변).
- **D55** hook-only VACUUM 소급(D50 사후 이월분 채택):
  - `runPurgeHookOnly`의 ⑤ VACUUM 후행(현행: `st.Vacuum` 실패 시
    stderr log-and-continue·rc=0, Exec nil을 성공 간주)을 D50
    본경로의 `vacuumReclaim` 호출로 교체한다 — `contentFootprint`
    before 측정(store open 후·PurgeHookOnly 실행 전) → VACUUM →
    `wal_checkpoint(TRUNCATE)` QueryRow busy 검증 → 총합 보고
    (`content.db(+wal/shm) AB → BB (파일 축 회수 CB)`).
  - **rc 정책**(사용자 결정): 본경로 동일 — busy 계열은 "라이브
    프로세스 가동 중 추정 — 종료 후 재시도" 명시 오류로 rc≠0.
    hook-only도 수동 CLI 실행이라 fail-open일 이유가 없다.
  - **순서 불변**: ④ 실회수 보고(shadow 귀속 삭제분)가 VACUUM보다
    먼저 출력되는 현행 순서 유지 — VACUUM 실패에도 이미 커밋된
    삭제분은 유지되고 부분 성공이 먼저 노출된다(vacuumReclaim
    계약 그대로 — 호출자가 되돌리지 않음).
  - **테스트 반전**: `TestPurgeHookOnlyVacuumFailureContinues`(현행
    rc=0 단정)를 실패 시 오류 반환 단정으로 반전 + 성공 시 총합
    보고 라인 단정 추가.
- **D56** 버전 중앙 집중화(`internal/buildinfo` — 사용자 지정
  접근):
  - **단일 소스**: 신규 패키지 `internal/buildinfo` — unexported
    `var productVersion = "0.10.0-dev"` + `func ProductVersion()
    string` accessor. import 0(순환 없음). var(const 아님)로 두어
    향후 GoReleaser/CI의 `-ldflags -X` 주입 전환 여지를 확보한다
    (외부 패키지 변경은 accessor로 차단).
  - **전 소비처 단일화**: `cmd/context-router/main.go:29`의 `const
    version`·`internal/mcp/mcp.go:30`의 `const ServerVersion`을
    제거하고 CLI 기동 배너·hook Producer·hook install marker·
    doctor 비교·MCP serverInfo.version 전부 `ProductVersion()`
    하류로 통일 — 릴리스 수동 지점 2→1. 핀 테스트
    `TestVersionPinnedToServerVersion`은 심볼 소멸로 목적 소멸 →
    삭제(단일 소스라 불일치가 표현 불가).
  - **-dev 스킴**: 개발 사이클 중 `"0.10.0-dev"`, 정식 릴리스
    커밋에서 `"0.10.0"`(-dev 제거)·차기 사이클 시작 시
    `"0.11.0-dev"` 범프. 개발 중 반복 `go install`에도 marker
    문자열 불변(cx 재신뢰 불필요) — marker 변경은 릴리스 경계
    1회로 수렴하고, 그 경계에서 doctor 스테일 탐지(D52)가 정확히
    발화한다(의도된 개선 — 종전에는 개발/릴리스 빌드가 동일
    문자열이라 경계 비가시). Producer에 `-dev`가 노출되는 것은
    진단 가치(어느 빌드가 캡처했는지)로 수용 — compare arm
    판정·synthetic 판별은 Producer를 쓰지 않아 무영향.
  - **VCS 진단(doctor 한정)**: `runtime/debug.ReadBuildInfo()`의
    vcs.revision(12hex 절단)·vcs.time·vcs.modified·Go toolchain을
    doctor 정보 라인 1개로 병기(`build:` — ReadBuildInfo 실패·
    필드 부재 시 해당 요소 생략, 경고 아님). **commit·dirty·
    pseudo-version은 marker·stale 비교에 절대 불포함**(안정
    SemVer 계약 — 웹 리서치 §3: Go 1.24+ VCS 스탬핑은 워킹트리
    더티/태그 비정위치에서 커밋마다 변해 등호 비교 기반 스테일
    탐지를 오탐으로 파괴한다. `--version`류 기본 출력도
    ProductVersion만).
  - **릴리스 CI 태그 검증**: ci.yml에 태그 push 조건부
    (`startsWith(github.ref, 'refs/tags/v')`) 검증 잡 추가 —
    빌드한 바이너리의 버전 출력(ProductVersion)과 태그의 v접두
    제거값 일치를 단정, 불일치(예: -dev 잔존 태그)는 실패로 차단.
  - **비도입**(사용자 결정): `VERSION` 평문 파일·`//go:embed` —
    Go 외부 패키징 도구나 다중 언어가 같은 파일을 읽어야 할 때만
    재상정(§4).
- **D57** 수렴 로드맵(방향성 결정 — 문서 계약):
  - **결정**: context-router가 종착지, ctxscribe(사용자의
    `mksglu/context-mode` 하드 포크)는 대체 시까지의 브리지.
    대체 게이트 = **exec 3종(D21 트랙)** — exec가 ctr에 이식되는
    시점에 ctxscribe의 잔여 고유 가치(think-in-code 실행 샌드박스)
    가 소멸하므로, 이후 포크 유지보수(업스트림 선택 포팅 노동)를
    종료한다. exec 3종은 §4에서 **v0.11+ 주력 후보로 격상**.
  - **근거 실측**(§3): 현 역할 분담이 데이터로 확정 — ctr MCP
    도구 호출 8회 vs ctxscribe 422회(전 기간, 본 프로젝트
    session_events). ctr의 고유 가치는 능동 도구가 아니라 배경
    레이어(zero-reliance 훅 캡처 shadow 26.7MB·A/B 계측 5점
    일관·가드)에 있고, 이는 ctxscribe가 구조적으로 갖지 못하는
    축. 반대로 ctr의 최대 약점은 회수 경로 미채택(캡처 대비 소비
    부재) — 병행 개선 축으로 §4 등재(ctr_search 유도).
  - **반영**: vision 문서(`context-router-vision-proposal-ko.md`)에
    로드맵 절 추가(이 결정의 사본이 아니라 참조 — 결정 원본은 이
    §0). v0.10 구현 범위에는 문서 갱신만 포함, exec 착수는 비범위.

## 1. v0.10 제품 계약

### 1.1 범위

- D53 서브에이전트 캡처 — cc 훅 6이벤트·subagent_start/stop 이벤트
  2종·PostToolUse attrs 표식 병기·상호작용 명문 전부.
- D54 Grep 가드 — matcher 확장·단일 deny 조건·warning 이벤트.
- D55 hook-only VACUUM 소급 — vacuumReclaim 합류·rc≠0·테스트 반전.
- D56 버전 중앙화 — internal/buildinfo·전 소비처 단일화·-dev 스킴·
  doctor build 라인·CI 태그 검증 잡.
- D57 수렴 로드맵 — vision 문서 갱신(문서 태스크).
- 버전 0.10.0 범프(단일 지점 — D56 이후), 도그푸딩: 재설치 후
  subagent_start/stop 실관측·Grep 가드 존재 확인·doctor build 라인
  확인.

### 1.2 명시적 비범위 (v0.10)

- cx 서브에이전트 캡처 — Codex에 훅 표면 부재(표면 등장 시 재상정).
- SubagentStop `last_assistant_message`·`agent_transcript_path` 수용
  — 응답 본문 불포함 관례(D53 비수용 명문). 내부 도구 출력은 기존
  PostToolUse 캡처가 이미 커버.
- SubagentStart/Stop matcher 필터(agent_type 선별 구독) — 전체("")
  구독으로 충분(YAGNI).
- Grep -A/-B/-C·대형 path 등 휴리스틱 deny — allow-bias 관례 위배
  (FP 위험). PostToolUse 관찰 기반 힌트도 비도입.
- VERSION 평문 파일·go:embed·ldflags 주입 실적용 — D56 비도입
  문면(여지만 확보).
- doctor drops 전 프로젝트 집계·synthetic 표식 영속화 — 재상정
  조건(각각 재증가 실측·오분류 실증) 이번 관찰 웨이브에서 미충족
  확인(§3), 조건 유지 이월.
- exec 3종 착수 — D57은 방향 결정과 문서 갱신까지(구현은 v0.11+).
- D46 회수 실행 — 사용자 결정 창 대기 지속(D55는 도구 보강일 뿐
  실행 결정 아님).

### 1.3 선행 게이트

- 없음 — 순수 내부 작업. 도그푸딩 반영에 `hook install` 재실행
  1회 필요(cc settings 6이벤트 갱신 — cx 훅 파일은 무변경이라
  재신뢰 불요. 단 D56 릴리스 시점의 marker 버전 변경은 기존과
  동일하게 재설치+cx 재신뢰 1회).

## 2. 검증·테스트 계약

- D53 생애주기: SubagentStart/SubagentStop 픽스처(실덤프 §3 필드
  미러) → subagent_start/stop 이벤트 각 1건·attrs
  {agent_id,agent_type} 단정. agent_type="" 픽스처 → 접미 생략
  summary·attrs 빈 값 기록 단정. 미등록 세션의 SubagentStart →
  D51 합성 등록 경유 후 기록 단정(파이프라인 합류 회귀).
- D53 표식: agent_id 실린 PostToolUse 픽스처 → 기본 이벤트 attrs
  병기 단정, 부재 픽스처 → attrs에 두 키 부재 단정(부모발 오표식
  0 회귀). PostToolUseFailure도 동일 1건.
- D53 install: hook install 후 settings.json에 6이벤트(기존 4 +
  SubagentStart/Stop) 단정·재실행 멱등 단정(기존 install 테스트
  관례 확장).
- D54: ① content+head_limit 0 → deny 출력·warning 이벤트 단정 ②
  content+head_limit 부재 → 통과(무출력·무이벤트) ③
  content+head_limit>0 → 통과 ④ files_with_matches+0 → 통과 ⑤
  tool_input 파싱 불가 → 통과. matcher 문자열 갱신 단정(install
  테스트).
- D55: 성공 경로 — hook-only purge 후 총합 보고 라인 단정. 실패
  경로 — `TestPurgeHookOnlyVacuumFailureContinues` 반전(오류
  반환·이미 커밋된 삭제분 유지 단정). ④→⑤ 순서(실회수 보고 선행)
  단정.
- D56: buildinfo 단일 소스 — 기동 배너·Producer·marker·doctor·
  MCP serverInfo가 전부 ProductVersion() 하류임을 단정(잔존 버전
  리터럴 grep 0 — 테스트 픽스처 제외 목록 명시). 핀 테스트 삭제.
  doctor build 라인 — ReadBuildInfo 성공/실패 양 경로 출력 단정
  (실패는 생략, 경고 아님). CI 태그 검증 잡은 로컬 테스트 불가 —
  ci.yml 문면 리뷰 + 실태그에서 검증(릴리스 절차).
- D57: 코드 없음 — vision 문서 갱신의 리뷰 확인만.
- 공통: `go test -p 1`(메모리 캡), CI 3-OS, §12 canary 유지,
  byte-for-byte 게이트(무플래그 usage 본표 등 기존 출력 계약
  무변경 — doctor는 라인 추가만).

## 3. 관측 실측 기록 (2026-07-23, session-26 — 컨트롤러 수행)

- **D51 실효**: 바이너리 install mtime 14:24:01 KST 기준 — 전
  프로젝트 unknown-session 신규 발생 0(본 프로젝트 399 불변·
  sufficode 981→1536(+555)이나 마지막 drop 13:06:23 KST로 전부
  설치 전 누적 후 정지·ws 23·memory 6·matcher 5 불변). 재신뢰
  직후 14:31:54~57 KST 본 프로젝트·sufficode에서 cx session_start
  정상 등록 동시 실관측(재신뢰 전 cx 훅 스킵 → 재신뢰가 발화
  재개 경계임을 확인). 합성 등록(payload Source="first-event")
  0건 — 재신뢰 후 session_start 정상 등록으로 unknown 세션
  자체가 미발생(D51 합성 경로는 대기 상태가 정상 — 실증은 향후
  중간 재신뢰류 엣지 발생 시).
- **compare**: cc on 26세션/3831rec·off 19세션/3960rec,
  output/rec 0.851·cache_read/rec 0.658(5점째 일관 —
  0.862→0.851·0.658 유지), `synthetic=0` 필드 cc·cx 양 블록
  실표시(D51 T2 계약 실관측). cx on 5세션·10턴
  0.270/0.196/2.249 불변(n 정체 — 판단 유보 지속).
- **doctor**: [6] sessions=118(empty=84 — 83→84, 등록일 분포
  07-20:8·07-21:46·07-22:18·07-23:12, GC 첫 회수 ≈07-27·최대
  덩어리 ≈07-28+), [9]/[16] marker 0.9.0 양 레벨·분기④ 정상
  지속, [12] drops=400(worktree 399 unknown-session +
  store-root 1 bad-input), [14] file=116,740,096B 발화
  지속(session-25 말 대비 불변), [15] shadow 26,676,520B(cc:=
  25,576,612B cx:=1,099,908B — cx +125,967B, 재신뢰 후 캡처
  실동작).
- **훅 인풋 실덤프**(D53·D54 설계 확정 근거 — 세션 중
  settings.local.json 덤프 훅 + Explore 서브에이전트 1회,
  실측 후 훅 제거):
  - SubagentStart: `{session_id(부모), transcript_path, cwd,
    prompt_id, agent_id, agent_type, hook_event_name}` —
    permission_mode/effort는 공식 문서와 달리 부재(실측 우선).
  - SubagentStop: 위 + `{permission_mode, stop_hook_active,
    agent_transcript_path, last_assistant_message,
    background_tasks, session_crons(, effort)}`.
    **agent_type="" 실관측 1건**(별도 백그라운드 에이전트의
    Stop — 빈 값 방어 필요 근거).
  - 서브에이전트 내부 도구 호출(Glob/Grep)의 PostToolUse:
    `agent_id`/`agent_type` 실림 확정. 부모 세션 자신의 호출
    (Agent 도구 포함)에는 두 필드 부재 — 존재/부재가 곧 판별.
  - Grep tool_input: 미지정 시 `head_limit` 필드 부재(호스트
    기본 250은 입력에 채워지지 않음), 명시 시만 존재(실측 값
    5 전달 확인). `output_mode`는 실전달.
- **도구 채택**(D57 근거): 본 프로젝트 session_events 전 기간 —
  ctr MCP 도구 호출 8회(ctr_search 4) vs ctxscribe 도구 호출
  422회. 서브에이전트 내부 활동은 부모 세션으로 이미 캡처
  중임도 확정(세션-25 SDD 세션 cc:1b24a241 file_edit 70건 =
  구현자 서브에이전트의 rollout.go 등 편집 실존).
- 버전 지점 전수(D56 근거 — 서브에이전트 조사): 수동 2지점
  (main.go:29·mcp.go:30, 직전 범프 1e9bd3b 실증) + 핀 테스트
  1건. 웹 리서치: Go 1.24+ VCS 스탬핑은 더티/비태그 워킹트리에서
  의사버전·+dirty로 변동 — bare `go install` 도그푸딩의 marker
  등호 비교와 양립 불가(ldflags도 동일 사유 기각), 안정 SemVer
  단일 소스 + VCS는 진단 한정이 결론.

## 4. 의도적 미결 (v0.11+ 후보 — v0.9 §4에서 이월·개정)

- **exec 3종(D21 트랙) — D57 수렴 로드맵의 대체 게이트로 격상,
  v0.11+ 주력 후보**(OS 격리 설계 선행 필요는 종전 문면 유지).
- 회수 경로 채택 개선(신규 — D57 근거 실측 8 vs 422): ctr_search/
  ctr_fetch 사용 유도 — 도구 설명·스킬·가드 deny 문구 연계 등
  표면 설계는 미정.
- 무작위 A/B 하네스·OTel(D27) — cx arm n 정체 지속으로 cc 축
  우선 검토가 자연.
- 서브에이전트 캡처 확장 — last_assistant_message 수용(응답 본문
  취급 계약 재설계 전제), agent_type matcher 선별, cx 표면 등장
  시 cx 축.
- synthetic 표식 영속화(retention 창 초과 생존 세션 오분류 실증
  시 — session-26 관찰: 합성 0건, 조건 미충족 지속), doctor drops
  전 프로젝트 집계(재증가 실측 시 — session-26: 신규 0, 미충족),
  content.db sweep 자동화(D50 수동 실효 검증 후 — D46 회수 자체가
  미실행), 수명주기 공유 락(불충분 실증 시만), hook-only 잔여
  후속은 D55로 소진.
- VERSION 평문 파일/go:embed(외부 도구가 버전 파일을 읽어야 할
  때), ldflags -X 실적용(GoReleaser류 파이프라인 도입 시 —
  buildinfo가 여지 확보).
- plugin manifest, semantic 보강, spill journal, `repository{}`
  기입, `invalidates`, Producer 버전 기반 A/B treatment 자동 경계
  표기, Bash 가드 잔여(Grep 절반은 D54로 소진) — 종전 문면 유지.
- v0.7 §9 잔존 리스크(전역 블록 수명 원인 규명·스테일 가드
  문서화·라인 스캔 한계) — 종전 문면 유지, D52가 탐지 담당.

## 5. 적대 검수 처리 기록 (2026-07-23, 설계 체크포인트)

- (검수 후 기입)
