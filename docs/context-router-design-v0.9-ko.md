# Context Router v0.9 설계서 (D51·D52 — 미지 세션 자동 등록·doctor Codex MCP 등록 검사)

- 전제: v0.8.0 태그(main 5df2909, PR #20, CI 3-OS GREEN), 도그푸딩
  marker 0.8.0, Codex `/hooks` 재신뢰 완료 + **재신뢰 직후
  `[mcp_servers.ctr]` 블록 생존 사용자 직접 확인**(2026-07-23) —
  §9-2 실사례의 소멸 원인에서 재신뢰는 배제 확정. 관찰(2026-07-23,
  session-25, §3): **unknown-session drop 재발 실측** — 전 프로젝트
  합산 ≈1,344건(sufficode 981건은 07-22~23 3시간 창 집중·진행 중,
  본 프로젝트 324→329), D46 잔존 발화 109,002,752B(회수는 사용자
  결정으로 관찰만), empty=79(GC ≈07-27+ 미도래), cx: 가드 실발화
  0건, usage --compare cx arm n 정체(on 5세션·10턴 불변).
- 스코프 확정 근거: session-25 브레인스토밍 → 사용자 결정 3건 — ①
  축=**D43 재상정 + D48 묶음**(관측 증거 기반 견고성 2건, 합쳐도
  v0.8급 규모) ② doctor drops 전 프로젝트 집계 **비범위**(YAGNI —
  D51이 유실 자체를 제거, §1.2) ③ D46 회수는 이번 세션 관찰만.
- 착수 조건 처리: D43 재상정 조건(v0.5 §0 — "재발 시 D43 필드로
  즉시 판정 가능해진 뒤 재상정")은 문면 그대로 충족 — 재발이
  실측됐고, D43 진단 5필드가 설계 의도대로 즉시 판정을 실증했다
  (cx:/cc: 세션·Pre/PostToolUse만·session_start 미등록 → 세션 탄생
  시점 단일 이벤트 등록 의존이 근인. 미등록 세션 계급의 원인
  후보: 재신뢰 이전 시작 세션의 session_start 스킵 후 중간 재신뢰,
  서브에이전트/스레드류 세션의 session_start 표면 부재 — 어느
  후보든 D51이 동일하게 회수한다). D48 채택
  조건(v0.8 §4 — "사후 필요 실증 시")도 충족 — v0.7 블록이 v0.8
  설치 직전 부재였던 §9-2 첫 실사례(session-24 관찰).

## 0. 결정 이력 (v0.9 신규 — D50까지는 v0.8 설계서·이전 델타 체인)

- **D51** register-on-first-event 채택(v0.5 D43 비채택 절의 재상정
  조항 이행 — 미지 세션 자동 등록):
  - **위치·형태**: hook dispatch의 `SessionExists=false` 분기 단
    1곳 — `appendDrop("unknown-session")`을 `EnsureSession(ctx,
    "first-event", worktreeRoot)` 호출로 교체하고, 성공 시 해당
    이벤트를 **그대로 계속 처리**한다(트리거 이벤트는 drop 대신
    정상 처리 경로 — 가드·append·shadow 전부). 등록 커밋~이벤트
    append 사이의 kill·deadline 실패는 기존 append-failed drop
    계약과 동일하며, 등록+이벤트 원자 결합 API는 비도입 — 어떤
    이벤트에도 원자 보존을 약속하지 않는 fail-open 훅 계약과
    동형(§5 기각 기록). 실패 시 기존 reason
    `ensure-failed`로 drop 후 반환 — SessionStart 경로와 reason을
    공유하되 D43 5필드의 hook_event가 두 경로를 판별한다(신규
    reason·[12] 집계기 변경 없음).
  - **합성 마커**: source 상수 `"first-event"` — session_start
    이벤트 payload의 Source로 기록되어 호스트 발 source(startup/
    clear/compact 등)와 구분된다. 서버 발행 상수라 truncate 불요,
    enum 강제 없음(전방 호환 기존 문면 유지). `started_at`은 첫
    이벤트 시각 — 실 세션 시작보다 늦다(캐비앗 명기: 합성 등록
    세션의 세션 길이·이벤트 수는 하한 추정치). 합성 등록 후 같은
    세션의 실 SessionStart 재발화(clear/compact)는 EnsureSession
    멱등(재발행 없음)에 흡수된다 — source는 first-writer-wins로
    first-event 잔존(캐비앗, 기존 재발화 계약과 동형).
  - **핫패스 불변**: SessionExists(reader 조회) 선행 구조를
    유지하고 EnsureSession은 미지 판정 시에만 호출한다 — 기지
    세션의 이벤트당 비용 변화 0. EnsureSession은 기왕 INSERT OR
    IGNORE + session_start 발행 단일 트랜잭션(멱등, 그 두 작업
    간 kill-원자성 — session.go 계약)이라 동시 이벤트 경쟁은
    무해하다(한쪽만 created, 재발행 없음). 미지 세션의 첫
    이벤트만 EnsureSession write-tx 1회가 증분된다(deadline 예산
    내 — 종전 drop-log 1줄 대비 증가는 미지 경로 한정).
  - **생애주기·D42 empty GC 상호작용**: 합성 등록 세션의 수명은
    기존 계약으로 닫힌다 — 기본 retention(30일) 스윕이 이벤트를
    비우면 D42 empty GC(7일 게이트)가 세션 행을 수거한다(상한 ≈
    retention+7일). PreToolUse 가드 통과로 이벤트 없이 등록만 된
    세션은 session_start 단독이라 empty GC 직접 대상(7일 상한).
    `CTR_HOOK_RETENTION_SEC=0`(무기한 명시 설정) 환경은 합성
    세션도 영구 — 기존 0=무기한 계약의 의도된 귀결(캐비앗 명기).
    합성 세션 전용 cap·TTL은 비도입 — 실세션 재유실 위험이
    D51의 존재 이유와 모순(§5 기각 기록). retention으로 비워져
    GC된 구세션에 늦은 이벤트가 도착하면 자동 재등록된다(고아
    이벤트 방지의 자기치유). 신생 합성 세션은 started_at=now라
    7일 게이트에 걸리지 않아 GC와 경쟁하지 않는다 — 추가 락
    불요.
  - **의미 변화**: `unknown-session` drop 사유는 신규 발생이
    소멸한다(코드 경로 제거 — 과거 로그는 잔존하며 [12] 집계기는
    신구 공용 그대로). bad-host/bad-cwd/bad-session-id 등 식별 전
    단계 drop과 session-check-failed는 무변경. retention은
    `ad.retentionSec`(env) 그대로 — 특례 없음. cc/cx 공통(dispatch
    는 host 무관).
  - **A/B 측정 무결성**: 합성 등록 세션은 구성상 불완전 표본
    (시작 시각·초기 이벤트 결손)이라 usage --compare 집계에서
    **기본 제외**하고, coverage 라인에 제외 수를 `synthetic=N`
    으로 보고한다(이중 적대 검수 수렴 반영 — 캐비앗만으로는 배포
    전후 지표 변화가 훅 효과인지 표본 구성 변경인지 판별 불가,
    §5). 판별 근거는 session_start payload의
    Source="first-event"(합성 마커). 별도 synthetic arm 분해는
    비도입(제외+보고로 충분). **판별 유효 창 캐비앗**(최종 Codex
    리뷰 P2, §5): retention 스윕은 session_start도 만료 대상이라
    (retention.go §5 — event_type 면제 없음), 자기 retention
    창보다 오래 생존한 세션은 표식 소실로 일반 등록 분류로
    되돌아간다(우아한 퇴행 — synthetic 과소 보고, 기본 30일에선
    희귀). 표식 영속화는 §4 재상정 후보.
- **D52** doctor Codex MCP 등록 상태 검사(v0.8 §4 이월분 채택 —
  §9-2 전역 블록 수명 리스크의 조기 탐지 장치):
  - **신규 라인 [16]**: `codex:` — 기존 경로 해석을 재사용해
    (config.toml은 codexConfigPath — 사용자 단일, hooks.json은
    codexHooksPath — 프로젝트·사용자 **양 레벨** 각각, [9]의
    project/user 병기와 동형) config.toml의 `[mcp_servers.ctr]`
    블록 존재 여부와 hooks.json 자기 그룹 marker 버전을 보고한다.
  - **판정 함수 계약**(적대 검수 P1 반영 — install 상태기계는
    "무엇을 쓸지"의 분류라 존재/부재 판별에 부적합: classReplace/
    classAppend가 동일 mcpWritten으로 합류): 신규 TOML 파서 없이
    기존 라인 분류 헬퍼를 재사용하는 **소형 판별 헬퍼 2건을
    신설**한다 — ① config.toml 블록 존재 판별(관리 BEGIN/END
    마커 쌍 또는 맨 `[mcp_servers.ctr]` 헤더 → 존재; 마커 이상·
    키-경계 충돌은 별도 상태로 반환) ② Codex hooks.json marker
    버전 추출(isOurCodexGroup 판정에 StatusMessage의
    hookMarkerPrefix TrimPrefix를 더함 — 기존 함수는 bool만
    반환하므로 버전 추출 신설). 라인 스캔 한계 리스크는 v0.7 §9
    기존 문면 유지.
  - **5분기 출력 계약**: ① config.toml 부재 → `codex 미사용/
    미설치` 정보 라인(경고 아님) ② hooks marker 존재(어느
    레벨이든) + 블록 부재 → **warning**(§9-2 소멸 시그니처 —
    "훅은 설치됐으나 MCP 블록 부재: deny 안내가 가리키는
    ctr_search/ctr_fetch를 Codex가 볼 수 없음, `hook install
    --codex` 재기입 안내") ③ marker 부재(훅 미설치) → 정보
    라인(`hook install --codex` 안내, 경고 아님) ④ 둘 다 존재 →
    정상(`블록=존재 marker=<버전>` — hooks.json은 레벨별 병기)
    ⑤ 블록 마커 이상·키-경계 충돌(mcpMarkerAnomaly/mcpConflict
    상당) → **warning**(수동 확인 안내 — install의
    reportCodexMCPState 안내 문구와 동형).
  - **읽기 전용**: 자동 복구·재기입 없음(§9-1 자가치유 비채택
    결정과 동형 — doctor는 진단만). 경고의 rc 처리는 [14] D46
    경고와 동일(기존 doctor exit 계약 무변경).
  - **비범위**: trust 해시 검증(Codex 내부 알고리즘 재구현은
    스테일 리스크 — marker 버전 표시로 갈음), cc 측 .mcp.json
    검사(프로젝트 커밋 파일이라 소멸 리스크 자체가 다름).

## 1. v0.9 제품 계약

### 1.1 범위

- register-on-first-event(D51) — dispatch 미지 세션 분기 교체·합성
  source 상수·위 상세 계약 전부.
- doctor [16] codex 등록 상태 검사(D52) — 5분기 출력·양 레벨
  hooks.json·판별 헬퍼 2건.
- usage --compare 합성 세션 기본 제외 + coverage `synthetic=N`
  보고(D51 측정 무결성 — 이중 적대 검수 수렴 반영).
- 버전 0.9.0 범프(2지점), 도그푸딩: 재설치 후 교차 프로젝트
  unknown-session 신규 발생 정지 관측(§2)·doctor [16] 정상 분기
  실관측.

### 1.2 명시적 비범위 (v0.9)

- doctor drops 전 프로젝트 집계 — 사용자 결정(YAGNI): D51이 진행 중
  유실 자체를 제거하므로 과거 drop 가시성은 수동 관찰로 충분.
  재상정 조건: D51 배포 후에도 교차 프로젝트 drop 재증가 실측 시.
- 과거 drop 소급 회수 — drop은 사유 라인만 기록하고 이벤트 내용을
  보존하지 않는다(회수 불가 명기).
- usage --compare synthetic 별도 arm 분해 — 기본 제외 +
  `synthetic=N` 보고로 갈음(D51).
- 합성 세션 전용 cap·TTL·등록 상한 — 비도입(§5 기각 기록 — 실세션
  재유실 위험). 생애주기는 기존 retention+empty GC 계약으로 상한.
- trust 해시 검증·cc .mcp.json 검사(D52 비범위 참조).
- exec 3종·A/B 하네스·OTel·서브에이전트 캡처·Grep 가드 등 §4 잔여
  — 전부 이월.

### 1.3 선행 게이트

- 없음 — 순수 내부 작업(신규 외부 표면은 doctor 출력 라인 1개).

## 2. 검증·테스트 계약

- D51 미지 세션 등록: 미등록 세션의 PostToolUse 1건 → sessions 행
  생성 + session_start(payload Source="first-event") + tool_call
  이벤트까지 총 2이벤트 기록 단정. PreToolUse(가드 경로)에서도
  등록+warning append 성공 단정(가드가 ad를 쓰는 유일 경로 회귀).
- D51 멱등·경쟁: 동일 미지 세션 이벤트 2연속 → 세션 1행·
  session_start 1건 단정(EnsureSession 기왕 멱등 계약의 신규 호출
  지점 재확인).
- D51 회귀 핀(반전 대상 명시 열거 — 적대 검수 반영): ①
  TestHookUnknownSession — drop 단정을 등록+기록(events=2·payload
  Source="first-event") 단정으로 반전 ② TestHookHostIsolation
  (D35) — cx 이벤트가 cx: 세션으로 등록되고 cc: 세션 오귀속 0
  단정으로 반전(호스트 격리 속성 유지가 단정의 본체) ③
  TestDropLineDiagnosticFields — unknown-session 소멸로 결정적
  발화 경로가 사라지므로 D43 5필드 포맷 핀을 appendDrop 직접
  단위 테스트로 이관. SessionStart 경로 무변경(호스트 source
  전달·truncate 유지), bad-cwd/bad-session-id/session-check-failed
  drop 경로 불변.
- D51 compare 제외: source="first-event" 세션 픽스처 → 집계 제외 +
  coverage `synthetic=N` 보고 단정(기존 compare 픽스처 관례).
- D52 5분기: CODEX_HOME 격리(TestMain 전역 중화 관례, e4fc70d) 하에
  ① config.toml 부재 ② marker 존재+블록 부재(warning 문구) ③ marker
  부재 ④ 정상 ⑤ 마커 이상/충돌(warning) — 각 분기 출력 단정 +
  프로젝트/사용자 hooks.json 레벨 병기 단정. 실사용자 config.toml
  접촉 0.
- `go test -p 1`(메모리 캡), CI 3-OS, §12 canary(비밀 리터럴 분해)
  유지.

## 3. 관측 실측 기록 (2026-07-23, session-25 — 컨트롤러 수행)

- **unknown-session 재발**: 본 프로젝트 324→329(+5, cx: 07-22~23),
  교차 프로젝트 신규 발견 — sufficode 981건(07-22T23:03~07-23T02:11
  UTC 집중, cx: 세션 — session.db는 존재(81,920B)하고 등록 세션
  43건 전부 session_start 단독·이벤트 0(07-21 등록): 981 drop은 그
  등록 창 밖 세션들의 후속 이벤트다. 초안의 "session.db 부재 =
  session_start 무발화 증명" 서술은 다중 worktree export 실패를
  오독한 관찰 착오라 교정 — 적대 검수 P2 + 컨트롤러 재검증), ws
  23건(cc:, session.db 존재·이벤트 0), memory 6건,
  ctr-matcher-check 5건. 합산 ≈1,344건. D43 5필드가 판정 근거
  (sid8·hook_event·tool 필드로 세션 유형·경로 즉시 식별).
- §9-2: 재신뢰 완료 + 직후 블록 생존 사용자 직접 확인 — 소멸
  원인에서 재신뢰 배제. 현재 config.toml 블록 실물 1개 정상.
- D46: 109,002,752B > 임계 104,857,600B — 발화 지속(session-24
  109,232,128B 대비 −229,376B 자연 감소). 회수는 관찰만(사용자
  결정, 24h 창 재실행은 이월).
- doctor: [6] sessions=110(empty=79 — 74→79, GC ≈07-27+ 미도래),
  [9] marker 0.8.0, [12] drops=330(현 worktree 집계 — store-root=1
  bad-input·worktree=329 unknown-session), [14] sources=554
  artifacts=554 blob=30,841,963B, [15] shadow-owned 24,568,945B
  (cc:=23,595,004B cx:=973,941B — cx +약 186KB, 재신뢰 후 cx 캡처
  실동작 증거).
- usage --compare: cc: on 25세션/off 19세션, output/rec 0.862·
  cache_read/rec 0.658(4점째 일관 — 0.60/0.63→0.643→0.645→0.658).
  cx: on 5세션·10턴 0.270/0.196/2.249 **불변**(n 정체 — 파싱 스킵
  경고 지속·판단 유보 유지).
- cx: 가드 실발화 0건 — warning 이벤트 총 2건은 기존 cc: 스모크
  (07-21)뿐. 트리거(전량 덤프+임계 초과) 미발생 — 관찰 지속.

## 4. 의도적 미결 (v0.10+ 후보 — v0.8 §4에서 이월)

D51·D52 완료 후 잔여: 무작위 A/B 하네스·OTel(D27), exec 3종(D21
트랙), 서브에이전트 캡처(호스트 표면 필요 — D51은 서브에이전트발
이벤트를 세션으로 수용하나 캡처 표면 자체는 별개), Grep 도구 가드,
plugin manifest, semantic 보강, spill journal, `repository{}` 기입,
`invalidates`, Producer 버전 기반 A/B treatment 자동 경계 표기,
content.db 회수 자동화(sweep 스케줄 — D50 수동 트리거 실효 검증
후), hook-only VACUUM에 checkpoint 검증·총합 보고 소급(D50 사후),
수명주기 공유 락(D50 사후 검증 불충분 실증 시만), doctor drops 전
프로젝트 집계(§1.2 재상정 조건), synthetic 표식 영속화(retention
창 초과 생존 세션의 오분류 실증 시 — §0 D51 캐비앗·최종 Codex
리뷰 P2). v0.7 §9 잔존 리스크 중 전역 블록
수명은 D52가 탐지를 담당(원인 규명은 계속 — 다음 소멸 시 mtime
증거 보존), 스테일 가드=문서화 유지·라인 스캔 한계는 그대로 유효.

## 5. 적대 검수 처리 기록 (2026-07-23, 설계 체크포인트)

- 이중 적대 검수(초안 9a903cc 대상): 서브에이전트(opus) P1×2/P2×5/
  M×3 + Codex adversarial-review needs-attention(high 2·medium 1).
  1차 Codex 패스는 컨트롤러의 포그라운드 타임아웃 킬로 드라이버가
  고아화되어 미완 — 동일 초점 백그라운드 재기동으로 완수(재리뷰
  루프 아님).
- **수렴 채택 3건**:
  - **생애주기 완결**(서브 P2 + Codex high-1): 초안의 "empty GC
    비대상" 단문은 증식 통제 논증 미완 — 기본 retention(30일)
    스윕 → empty GC(7일)로 상한이 닫힘을 명문화, retention=0
    영구는 기존 0=무기한 계약의 의도된 귀결로 캐비앗화,
    PreToolUse 통과 세션은 session_start 단독이라 empty GC 직접
    대상(Codex의 "GC 비대상" 주장은 이 지점에서 부분 교정 — 서브
    실코드 검증과 일치).
  - **원자성 문면 정밀화**(Codex high-2): "트리거 이벤트 유실
    없음"이 등록+이벤트 원자 결합으로 오독될 소지 — "drop 대신
    정상 처리 경로"로 교정하고 등록 커밋~append 사이 실패는 기존
    append-failed 계약과 동일함을 명문. kill-원자성 문구는
    EnsureSession 내부(행+session_start)로 한정. 늦은 SessionStart
    멱등 흡수(source first-writer-wins) 캐비앗 추가.
  - **A/B 측정 무결성**(서브 캐비앗 + Codex medium): 캐비앗만으로
    는 표본 구성 변경과 훅 효과가 판별 불가 — compare 기본 제외 +
    coverage `synthetic=N` 보고를 범위로 승격.
- **서브 단독 채택**: D52 P1×2 — install 상태기계(classReplace/
  classAppend→동일 mcpWritten)로는 존재/부재 판별 불가 → 판별
  헬퍼 신설로 재서술; Codex hooks.json marker 추출 함수 부재
  (isOurCodexGroup은 bool) → 추출 신설 명세. P2×2 —
  mcpConflict/mcpMarkerAnomaly 분기(⑤ 신설)·hooks.json 프로젝트/
  사용자 이원 레벨([9] 동형 양 레벨 검사). 테스트 P2×2 — D35 격리
  테스트 반전 명시·D43 포맷 핀 appendDrop 단위 이관. M×2 — GC
  찰나 경쟁 과잉 서술 제거(started_at=now는 7일 게이트로 도달
  불가)·미지 경로 비용 증분 명기. §3 sufficode 서술 교정(컨트롤러
  재검증 — session.db 존재 81,920B·43세션 전부 session_start
  단독).
- **기각 2건**(Codex 권고):
  - 합성 세션 cap·TTL·등록 상한 — 상한 도달 시 실세션 이벤트를
    다시 유실해 D51의 존재 이유와 모순. 유입은 로컬 호스트 훅
    stdin(신뢰 경계 내)+canonical UUID 검증 후이며 생애주기가
    기존 계약으로 닫히므로 비도입(§1.2).
  - 등록+최초 이벤트 원자 결합 API — 기존 훅 계약은 어떤
    이벤트에도 원자 보존을 약속하지 않는 fail-open(deadline·
    drop)이라 신규 API가 지키는 불변식이 제품 계약에 없음. 문면
    정밀화로 갈음.
- **최종 이중 리뷰 기록(구현 체크포인트, 브랜치 520b134..1e9bd3b
  5커밋)**: 태스크 리뷰 4회(fix 1라운드 — T3 probe 우선순위 발산
  셀 false-healthy를 install 정확 미러로 교정) 후 서브(opus)
  whole-branch **Yes**(Critical/Important 0 — §2 계약 전수 대조
  누락 0·비범위 침범 0·마커 3지점 일치, Minor 전건 이월 수용) +
  Codex review **P2 1건**(위 판별 유효 창 — 문서화 채택, 표식
  영속화는 §4 조건부 이월: 스키마 변경이 엣지 대비 과대하고
  퇴행이 우아함). 참고: §2의 "PostToolUse→총 2이벤트" 계약은 전용
  테스트가 담당하고, 반전①(PreToolUse 픽스처)은 가드 통과 무이벤트
  특성상 session_start 단독(1이벤트) 단정이 정확 — 구현 커버리지가
  문면보다 정밀(서브 최종 리뷰 M-D).
