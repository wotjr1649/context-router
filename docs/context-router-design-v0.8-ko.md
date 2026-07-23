# Context Router v0.8 설계서 (D49 구현 — content.db 파일 축 회수)

- 전제: v0.7.0 태그(main 0be65fe, PR #19, CI 3-OS GREEN), 도그푸딩
  marker 0.7.0, Codex 가드+MCP 블록 설치·`/hooks` 재신뢰 완료
  (2026-07-23 사용자 확인). 관찰(2026-07-23, session-24, §3): D46
  파일 축 104,755,200→104,783,872B(임계 100MiB의 **99.93%** — literal
  미발화·사실상 임박), cx: 캡처 실동작(shadow-owned 768,226B), cx:
  가드 실발화 0건(재신뢰 직후 — 트리거 명령 미발생), empty=74(7일 GC
  창 ≈07-27+ 미도래), drops=325 불변, usage --compare cx arm n=10턴
  (파싱 스킵 경고 — 판단 유보).
- 스코프 확정 근거: session-24 브레인스토밍 → 사용자 결정 3건 — ①
  축=**D49 회수 구현 단독**(소형 단일 축 릴리스 관례) ② `--vacuum`
  **수식어 전용**(단독 불허 — 설계 원문 "행 삭제 후 VACUUM" 유지) ③
  §9-1 스테일 가드=문서화 유지 확정(v0.7 §9-1에 반영, 52ce41b).
  초안(acf09ff)의 "라이브 감지 2단 방어"·"--gc/--hook-only 유효
  조합"·"VACUUM 직후 content.db 단독 실측"은 이중 적대 검수가
  실코드·실험 반례로 폐기 — 사후 checkpoint 검증·`--older-than`
  단일 결합·총합 실측으로 교정(§5).
- 착수 조건 처리: D49 착수 조건은 "D46 경고 실발화"(v0.7 §0)이나
  99.93%·잔여 72KB로 구현 기간 내 literal 발화가 확실해 사용자
  승인으로 착수한다. 릴리스 게이트(도그푸딩)에서 doctor [14] 경고
  실발화를 literal 재확인하고 실회수로 경고 소멸을 관측한다.

## 0. 결정 이력 (v0.8 신규 — D49까지는 v0.7 설계서·이전 델타 체인)

- **D50** `purge --vacuum` 상세 계약(D49의 "발화 후 확정" 이월분):
  - **조합 규칙**: `--vacuum`은 **`--older-than`과 결합해서만
    유효**하다 — content.db 행을 실제로 삭제하는 모드는
    `--older-than`(selective, writable store)뿐이라 "행 삭제 후
    VACUUM" 전제가 성립하는 유일 결합이다. 다른 플래그는
    `--older-than` 기존 규칙을 따른다(`--older-than --gc --vacuum`·
    `--older-than --sessions --vacuum` 유효). `--older-than` 부재의
    모든 `--vacuum`은 **정적 오류**: 단독 / `--gc`만(gcOnly는
    read-only 연결(`mode=ro·query_only`)이라 VACUUM 실행 불가 +
    blob 물리 파일만 삭제해 content.db 행 미삭제 — 이중 부적합) /
    `--sessions`만(session.db 축이라 무관) / `--hook-only`(아래) /
    전체 삭제(디렉터리 삭제라 무의미). 검증은 **플래그 파싱 직후
    최우선** — 기존 `--project`/`--all` XOR 검사·`--hook-only` 조기
    분기보다 앞서 판정하며, 겹치는 입력에서는 vacuum 조합 오류가
    이긴다(오류 우선순위 명문).
  - **`--hook-only` 관계 명문화**: `runPurgeHookOnly`는 **현행에서
    이미 무조건 VACUUM을 후행한다**(D41, cli.go:776 — 기존 테스트
    `TestPurgeHookOnlyVacuumFailureContinues`가 무플래그 발화를
    단정). 따라서 `--vacuum` 수식 대상이 아니며 조합은 정적
    오류("--hook-only는 상시 VACUUM — --vacuum 불필요" 안내).
    hook-only 경로의 현행 동작(무조건 VACUUM·실패 시 stderr 보고
    후 rc=0)은 v0.8 **무변경**(checkpoint 검증 소급은 §4 이월).
  - **실행 순서·사후 검증**(초안 "라이브 감지 2단" 폐기 — §5):
    삭제 로직 완료 후 대상 프로젝트별 writer 연결로 VACUUM →
    **`PRAGMA wal_checkpoint(TRUNCATE)`를 QueryRow로 실행해
    busy/log/checkpointed 결과행을 검증**한다(이 PRAGMA는 미완료를
    오류가 아닌 busy=1로 알린다 — Exec nil 반환은 성공 증거가
    아님). busy≠0 → "라이브 프로세스 가동 중 추정 — 종료 후
    재시도" 명시 오류. 사전 락 프로브는 두지 않는다 — 서버가 상시
    보유하는 파일 락이 없어(rebuild.lock은 open/migrate·GC 창에서만
    보유) 프로브가 공허하고, WAL 특성상 idle 라이브 서버 공존
    VACUUM은 성공 자체는 무해하며 미완료 checkpoint로 표면화된다.
    경합(찰나 쓰기 트랜잭션) 시 busy_timeout(5000ms) 소진 후
    SQLITE_BUSY → 동일 단일 지점 오류 매핑. 어느 실패든 content.db
    무손상(VACUUM 원자성 — 임시 DB 구축 후 스왑)이고 **이미 커밋된
    삭제분은 유지**된다(실패가 삭제를 되돌리지 않음).
  - **진행 보고**: checkpoint 검증 통과 후 **content.db +
    content.db-wal + content.db-shm 총합**으로 전/후 실측(os.Stat,
    부재 파일=0B) — WAL 모드에서 main 파일 단독 직후 실측은 회수
    0/WAL 팽창으로 오측된다(§5 실험 근거). 1라인
    `content.db(+wal/shm): <전>B → <후>B (회수 <Δ>B)`. 삭제 모드가
    병기하는 blob 회수치(ReclaimedB — CAS blob 축)와는 축이 다름을
    출력 어휘로 구분한다.
  - **`--all` 집계 실패 계약**: 프로젝트별 VACUUM/checkpoint 실패는
    해당 프로젝트 오류 보고 후 다음 프로젝트 계속 진행하되, 오류를
    수집해 루프 종료 후 **하나라도 실패면 비-zero 집계 오류**로
    반환한다(자동화에서 부분 실패의 무성 성공 위장 방지). 단
    디스크 계열(SQLITE_FULL/IOERR) 실패는 잔여 프로젝트 VACUUM을
    중단한다(연쇄 악화 방지).
  - **임시 공간 주석**: VACUUM은 원본 크기급 임시 공간을
    요구한다(도그푸딩 ~100MiB → 여유 ~2배 권고) — 부족 시
    실패(무손상)로 표면화.

## 1. v0.8 제품 계약

### 1.1 범위

- `purge --vacuum` 플래그(D50) — `--older-than` 결합 전용, 위 상세
  계약 전부(정적 오류·사후 checkpoint 검증·총합 보고·집계 exit).
- 버전 0.8.0 범프(2지점), 도그푸딩: 실제 store(99.93%)에서 실발화
  회수 관측 — doctor [14] 경고 발화 확인 → `purge --project …
  --older-than <창> --vacuum` 실행 → checkpoint 검증 통과·총합
  실감소·경고 소멸 확인.

### 1.2 명시적 비범위 (v0.8)

- 자동 실행(스케줄·훅 트리거) 없음 — 수동 트리거 일관 원칙(D49).
- session.db/ledger.db VACUUM — D49는 content.db 파일 축 전용.
- hook-only 경로의 현행 무조건 VACUUM에 checkpoint 검증·총합 보고
  소급 적용(§4 이월 — 동작 변경이라 별도 상정).
- store 수명주기 공유 락(라이브 서버 선제 배제 — Codex 제안) 비도입:
  모든 open 경로·훅 성능에 침습적이고, 사후 checkpoint 검증이 동일한
  사용자 가시 계약(무성 오보 방지)을 의존 0으로 달성한다(§5).
- doctor Codex MCP 상태 검사, Grep 가드, exec 3종, A/B 하네스·OTel,
  register-on-first-event(D43), 서브에이전트 캡처 — 전부 이월(§4).

## 2. 검증·테스트 계약

- 정적 오류(대표 4형): 단독 `--vacuum` / `--gc --vacuum` /
  `--hook-only --vacuum` / 전체 삭제 조합 — 각각 삭제 미착수
  단정(파괴 부작용 0) + 기존 XOR·조기 분기 대비 오류 우선순위 단정.
- `--older-than --vacuum` 정상 경로: 픽스처 대량 삽입
  (strings.Repeat 계열 — 응답 분할 규율) → 삭제 → VACUUM+checkpoint
  → **총합(db+wal+shm) 전>후** 단정(main 파일 단독 단정 금지 —
  기존 TestVacuum이 크기 단정을 피한 것과 같은 WAL 가변성 근거).
- BUSY 유도: 별도 연결 `BEGIN IMMEDIATE` 선점(기존
  TestPurgeHookOnlyVacuumFailureContinues의 동기화 게이트 관례
  재사용) → 명시 오류 반환·`quick_check=ok`·삭제분 유지 단정.
- checkpoint busy 검증: 열린 reader 공존으로 busy≠0 유도 → 오류
  매핑 단정(Exec nil을 성공으로 오인하는 회귀 방지).
- `--all` 집계: 1번째 프로젝트 실패·2번째 성공 배치 → 2번째 실행
  단정 + 최종 비-zero 단정.
- hook-only 현행 무변경 회귀 핀: 무플래그 VACUUM 발화·실패 rc=0
  유지(기존 테스트 재확인).
- `go test -p 1`, CI 3-OS, §12 canary(비밀 리터럴 분해) 유지.

## 3. 관측 실측 기록 (2026-07-23, session-24 — 컨트롤러 수행)

- D46: 104,755,200B(99.902%, 세션 시작) → 104,783,872B(99.930%,
  브레인스토밍 중) — 세션 내 +28KB. WAL 체크포인트 후 잔여 72KB.
- doctor: [6] sessions=103(empty=74 — 72→74), [12] drops=325 불변
  (store-root=1 bad-input, worktree=324 unknown-session — v0.6 §7
  기지·안정), [14] sources=522 artifacts=635 blob=28,850,012B,
  [15] shadow-owned 22,576,994B(cc:=21,808,768B cx:=768,226B).
- usage --compare: cc: on/off output/rec 0.873·cache_read/rec 0.645
  (3점째 — 0.60/0.63→0.643→0.645 일관). cx: on 5세션·10턴
  input/turn 0.270·cached 0.196·output 2.249 — 파싱 스킵>0 경고,
  n 미성숙으로 판단 유보(대화형(경량) 코호트 한정 주석 유지).
- cx: 가드 실발화 0건 — warning 이벤트는 cc: 스모크 2건뿐. 재신뢰
  완료 직후이며 트리거(전량 덤프+임계 초과) 미발생 — 관찰 지속.
- 소형 캐리오버 처리(e4fc70d): cli TestMain CODEX_HOME 전역 중화
  (실사용자 config.toml 보호 — codexConfigPath가 CODEX_HOME을 홈보다
  우선해 격리 우회 가능했던 경로 봉쇄), TestGuardCodexBashDeny
  warning 단정에 `session_id LIKE 'cx:%'` 필터(D47 격리 단정 강화).

## 4. 의도적 미결 (v0.9+ 후보 — v0.7 §9에서 이월)

D49 완료 후 잔여: 무작위 A/B 하네스·OTel(D27), exec 3종(D21 트랙),
서브에이전트 캡처(호스트 표면 필요), register-on-first-event(D43),
Grep 도구 가드, plugin manifest, semantic 보강, spill journal,
`repository{}` 기입, `invalidates`, doctor Codex MCP 등록 상태 검사
(D48 사후 필요 실증 시), Producer 버전 기반 A/B treatment 자동 경계
표기(§5), content.db 회수 자동화(sweep 스케줄 — D50 수동 트리거
실효 검증 후), **hook-only VACUUM에 checkpoint 검증·총합 보고 소급**
(D50 사후 — 현행은 Exec nil을 성공으로 간주·rc=0), **수명주기 공유
락**(D50 사후 검증이 불충분하다고 실증될 때만). v0.7 §9 잔존 리스크
3건(스테일 가드=문서화 유지 확정·전역 블록 수명·라인 스캔 한계)은
그대로 유효.

## 5. 적대 검수 처리 기록 (2026-07-23, 설계 체크포인트)

- 이중 적대 검수(초안 acf09ff 대상): 서브에이전트(opus) P1×3/P2×2/M
  + Codex adversarial-review needs-attention(high 2·medium 2).
  **수렴 3건**이 계약 교정을 강제:
  - **라이브 감지 2단 폐기 → 사후 checkpoint 검증**(서브 P2×2 +
    Codex high-1): rebuild.lock은 open/migrate·GC 창에서만
    보유(store.go:118-122)라 배타 프로브 공허, WAL 특성 + modernc
    실험(열린 read 트랜잭션 중 VACUUM 성공)으로 BUSY도 감지기
    아님. "감지"를 팔지 않고 checkpoint busy 검증·원자성 무손상·
    삭제분 유지로 재서술. 초안의 "락 경합 시 DB 무변경" 테스트
    문구는 삭제 선행 계약과 모순이라 "삭제분 유지"로 교정.
  - **WAL 실측 교정**(서브 P1-C + Codex high-2): Codex 실험 —
    reader 공존 VACUUM에서 main 18,882,560B 불변·WAL
    824,032→5,578,512B 팽창(총 점유 악화), Close의
    `wal_checkpoint(TRUNCATE)` Exec는 nil인데 재조회 시 busy=1.
    → QueryRow busy 검증 + db+wal+shm 총합 실측으로 교체.
  - **hook-only 기왕 무조건 VACUUM**(서브 P1-A + Codex medium-1):
    cli.go:776 실재·기존 테스트가 무플래그 발화 단정 — 유효 조합
    에서 제외·정적 오류·현행 무변경 명문화(게이트화는 무언 동작
    변경이라 비채택).
- **서브 단독 P1-B 채택**: gcOnly는 read-only 연결(`query_only`)
  이라 VACUUM 실행 자체가 불가 + content.db 행 미삭제 — 초안의
  `--gc --vacuum` 축복 폐기, 조합 규칙을 `--older-than` 단일 결합
  으로 축소(구조상 규칙도 단순해짐).
- **Codex 단독 medium-2 채택**: `--all` 집계 exit 계약 신설(부분
  실패 무성 위장 방지) + 디스크 계열 오류 시 잔여 중단.
- **Codex lifecycle lock 제안 비채택**: 모든 open 경로·훅에 침습,
  사후 검증이 동일 계약을 의존 0으로 달성 — §1.2 비범위·§4 조건부
  이월로 기록.
- **서브 P2(오류 우선순위)·M(임시 공간·회수치 이원화) 채택**: 검증
  위치 최우선 명문·~2배 여유 주석·blob 축 구분.
