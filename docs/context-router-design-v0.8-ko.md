# Context Router v0.8 설계서 (D49 구현 — content.db 파일 축 회수)

- 전제: v0.7.0 태그(main 0be65fe, PR #19, CI 3-OS GREEN), 도그푸딩
  marker 0.7.0, Codex 가드+MCP 블록 설치·`/hooks` 재신뢰 완료
  (2026-07-23 사용자 확인). 관찰(2026-07-23, session-24, §4): D46
  파일 축 104,755,200→104,783,872B(임계 100MiB의 **99.93%** — literal
  미발화·사실상 임박), cx: 캡처 실동작(shadow-owned 768,226B), cx:
  가드 실발화 0건(재신뢰 직후 — 트리거 명령 미발생), empty=74(7일 GC
  창 ≈07-27+ 미도래), drops=325 불변, usage --compare cx arm n=10턴
  (파싱 스킵 경고 — 판단 유보).
- 스코프 확정 근거: session-24 브레인스토밍 → 사용자 결정 3건 — ①
  축=**D49 회수 구현 단독**(소형 단일 축 릴리스 관례) ② `--vacuum`
  **수식어 전용**(단독 불허 — 설계 원문 "행 삭제 후 VACUUM" 유지) ③
  §9-1 스테일 가드=문서화 유지 확정(v0.7 §9-1에 반영, 52ce41b).
- 착수 조건 처리: D49 착수 조건은 "D46 경고 실발화"(v0.7 §0)이나
  99.93%·잔여 72KB로 구현 기간 내 literal 발화가 확실해 사용자
  승인으로 착수한다. 릴리스 게이트(도그푸딩)에서 doctor [14] 경고
  실발화를 literal 재확인하고 실회수로 경고 소멸을 관측한다.

## 0. 결정 이력 (v0.8 신규 — D49까지는 v0.7 설계서·이전 델타 체인)

- **D50** `purge --vacuum` 상세 계약(D49의 "발화 후 확정" 이월분):
  - **수식어 전용 조합 규칙**: `--older-than`/`--gc`/`--hook-only` 중
    하나 이상과 결합해서만 유효. **정적 오류 3형** — ① 단독
    `--vacuum` ② 전체 삭제(선택 플래그 없음) 조합: 프로젝트
    디렉터리 자체가 삭제되므로 VACUUM 무의미 ③ `--sessions`만과의
    조합: session.db 축 삭제라 content.db 회수와 무관(무성 no-op
    대신 명시 거부 — 빈 `--all` 즉시 오류와 동일 관례). 오류는
    플래그 파싱 직후 정적 판정(삭제 착수 전).
  - **gcOnly 확인 생략 유지**: `--gc --vacuum`(+ selective·sessions
    아님)은 기존 gcOnly 확인-생략 모드를 유지한다 — VACUUM은 데이터
    삭제가 아닌 재배치라 확인 승격 사유가 아니다. "삭제 없이 순수
    회수" 실수요는 이 조합이 커버한다.
  - **실행 순서·실패 계약**: 기존 삭제 로직 완료 후 대상 프로젝트별
    content.db에 VACUUM. 삭제는 이미 커밋된 뒤이므로 VACUUM 실패가
    삭제를 되돌리지 않는다(오류 보고만). VACUUM 자체는 SQLite
    보장상 실패 시 원본 무손상 — 실패 후 `quick_check=ok` 유지가
    테스트 계약(§2).
  - **라이브 프로세스 감지**(v0.7 §0 D49 제약의 구체화): ① VACUUM 전
    락 표면 배타 프로브(논블로킹, `AcquireLock` 계열)로 사전 감지 →
    보유자 감지 시 즉시 명시 오류로 중단 ② 프로브 통과 후에도
    VACUUM 중 `SQLITE_BUSY`/`SQLITE_LOCKED` → 단일 지점 오류 매핑
    으로 "라이브 프로세스 가동 중 추정 — 종료 후 재시도" 안내.
    서버가 상시 보유하는 파일 락이 없으면 ②가 실질 방어선이다 —
    정확한 락 표면(`content.db.rebuild.lock` vs `session.lock`)은
    계획 단계에서 실코드로 핀한다.
  - **진행 보고**: 프로젝트별 1라인 `content.db: <전>B → <후>B
    (회수 <Δ>B)`. `--all`이면 프로젝트별 반복. 크기는 VACUUM 직전·
    직후 os.Stat 실측.
  - **`--all` 조합**: 허용 — 프로젝트 루프 안에서 프로젝트별 VACUUM.
    한 프로젝트의 VACUUM 실패는 해당 프로젝트 오류 보고 후 다음
    프로젝트 진행(부분 실패 허용 — 삭제 계약과 동일).

## 1. v0.8 제품 계약

### 1.1 범위

- `purge --vacuum` 플래그(D50) — 수식어 전용, 위 상세 계약 전부.
- 버전 0.8.0 범프(2지점), 도그푸딩: 실제 store(99.93%)에서 실발화
  회수 관측 — doctor [14] 경고 발화 확인 → purge+--vacuum 실행 →
  경고 소멸·파일 실감소 확인.

### 1.2 명시적 비범위 (v0.8)

- 자동 실행(스케줄·훅 트리거) 없음 — 수동 트리거 일관 원칙(D49).
- session.db/ledger.db VACUUM — D49는 content.db 파일 축 전용.
- doctor Codex MCP 상태 검사, Grep 가드, exec 3종, A/B 하네스·OTel,
  register-on-first-event(D43), 서브에이전트 캡처 — 전부 이월(§5).

## 2. 검증·테스트 계약

- 정적 오류 3형: 단독 `--vacuum` / 전체 삭제 조합 / `--sessions`만
  조합 — 각각 삭제 미착수 단정(파괴 부작용 0).
- `--older-than --vacuum`: 픽스처 대량 삽입(strings.Repeat 계열 —
  응답 분할 규율) → 삭제 → VACUUM → 파일 크기 실감소(전>후) 단정.
- `--gc --vacuum`: 확인 생략(gcOnly) 유지 + 정상 완료.
- 라이브 감지: 테스트가 락 선점 후 호출 → 명시 오류·DB 무변경.
- 실패 무손상: VACUUM 실패 유도(락 경합) 후 `quick_check=ok`.
- 보고 형식: `content.db: <전>B → <후>B (회수 <Δ>B)` 문자열 단정.
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
실효 검증 후). v0.7 §9 잔존 리스크 3건(스테일 가드=문서화 유지
확정·전역 블록 수명·라인 스캔 한계)은 그대로 유효.
