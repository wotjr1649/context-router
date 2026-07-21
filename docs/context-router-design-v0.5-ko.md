# Context Router v0.5 설계서 (store 수명주기)

- 전제: v0.4.0 태그 + follow-up 웨이브 머지(main d4c204b, PR #14, CI 3-OS
  GREEN — 릴리스 아님), 도그푸딩 Claude marker 0.4.0 + Codex 실세션 A/B 가동
  (`/hooks` 신뢰 승인 완료). 실측(2026-07-22, §9): [14] blob 14.1MB(임계
  14%)·content.db 파일 52.9MB(3.7배 갭), session.db 78세션 중 빈 세션 55개
  (스테일 0.1.0 MCP, retention 0 영구 잔존), drops unknown-session 246건
  (3클러스터 — 과도기 산물 판정), cx: 5세션·9이벤트(축적 초기).
- 스코프 확정 근거: session-18 브레인스토밍 → 사용자 결정 7건(축=store
  수명주기 통합 웨이브 / sweep 제외·집계+purge만 / cx:는 귀속 집계 접두
  차원만 편입 / breadcrumb 편승 제거 / 표면=doctor+purge 확장 / 블록
  A·B·C 각 확정).

## 0. 결정 이력 (v0.5 신규 — D39까지는 v0.4 설계서·이전 델타 체인)

- **D40** shadow 귀속 바이트 집계 = doctor 확장만(새 커맨드·MCP 도구 없음):
  ① [14]에 content.db **파일 크기 병기**(blob과의 갭 = FTS 인덱스·free
  page 가시화; D38 경고 평가 기준은 **현행 blob 총량 유지** — 파일 크기는
  free page 탓에 purge 후에도 즉시 줄지 않아 경고 기준으로 삼으면 "purge해도
  경고 미해소" 모순). ② [15] 신설 `shadow-owned` — 귀속 규칙은 **모든
  source가 `kind=hook`인 artifact의 blob 바이트 합**(explicit ingest와 공유
  blob은 explicit 귀속 — D37 티어 정합). ③ [15]에 세션 접두 분해
  (`cc:`/`cx:`/`shared`/`unattributed`) 병기 — session.db `artifact_created`
  refs(content_hash)와의 조인, 다접두 공유는 임의 티어 없이 `shared` 버킷
  (정직 우선), session.db 불용 시 접두 분해만 폴백([15] 자체는 유지).
- **D41** hook 전용 선택 purge = 기존 `purge`에 `--hook-only` 플래그:
  삭제 술어는 **D40 [15] 귀속 집합과 동일**(explicit 공유 blob 보존) — [15]
  가 곧 회수 견적. `--project`와만 조합(`--all` 조합은 거부, 필요 실증 시
  후속). 선택 삭제 후 `VACUUM`으로 공간 회수(실패는 log-and-continue —
  행 삭제 자체는 유효). session.db·ledger 불변(세션 수명은 retention 스윕
  소관 — 경계 분리). confirmPurge 확인 게이트 재사용, 확인 문구에 회수
  견적 바이트 표시. D38 경고 문구는 "비선택 성격 병기" → "`--hook-only`
  선택 삭제 가능" 안내로 승격.
- **D42** 세션 위생 = 빈 세션 GC + [6] 집계 표시: 시작 시 1회 retention
  스윕에 빈 세션 GC 합류 — **이벤트가 session_start뿐이고 시작 후 7일
  경과**한 세션의 행+이벤트 삭제, **retention_sec 값과 무관하게 적용**
  (0은 스테일 0.1.0 바이너리의 기본값이지 의도 표명이 아니고, 빈 껍데기는
  표명 대상 데이터가 없음). 실이벤트 보유 세션은 retention 표명 존중(현행
  불변). doctor [6]에 `sessions=N (empty=M)` 병기. MCP 기동 세션의 lazy
  생성은 비채택(기동 기록의 관측 가치 + GC로 결과 동일 — 구조 변경 회피).
  근본 처방은 스테일 `mcp_servers.ctr` 등록 정리(사용자 수동 — 세션-17
  캐리오버 4)이며 본 결정은 재발 대비 위생.
- **D43** drops 로그 진단 필드: 현행 `ts \t reason` 2필드로는 근인 판정이
  우회 실측 의존(§9 — 246건 판정에 시간 클러스터×세션 정렬 필요). 라인을
  `ts \t reason \t 세션ID접두8 \t hook_event \t tool_name`으로 확장, 가용
  필드만 채우고 없으면 `-`(전체 UUID 미기록 — 로그 파일 식별자 최소화).
  기존 소비자(readDrops·doctor [12] reason 집계)는 substring/필드-2 검사라
  하위 호환 무해. **register-on-first-event(미지 세션 자동 등록)는 비채택**
  — 246건은 과도기 산물(§9 판정)이라 상시 표면을 늘리지 않고, 재발 시
  D43 필드로 즉시 판정 가능해진 뒤 재상정.

## 1. v0.5 제품 계약

### 1.1 범위

- shadow 귀속 바이트 집계 + 접두(cc:/cx:) 분해 — doctor [14] 확장·[15]
  신설(D40, §2).
- hook 전용 선택 purge `--hook-only` + VACUUM 공간 회수(D41, §3).
- 빈 세션 GC + [6] 세션 집계 표시(D42, §4).
- drops 로그 진단 필드(D43, §5).
- 편승: 구명 `CTR_SHADOW_WARN_BYTES` breadcrumb 2건 제거(§6).
- v0.5.0 릴리스 대상(버전 상수 0.5.0 — 사용자 가시 변경).

### 1.2 명시적 비범위 (v0.5)

- shadow 자동 sweep — D38 문면 유지(경고 실발화 시 재상정). 수동 선택
  purge(D41)가 생기므로 자동화 긴급성은 추가로 낮아짐.
- `usage --totals` Codex 집계 — v0.4 기각 이력 유지(세 번째 저장 형식
  계약). cx: 관측은 [15] 접두 분해가 담당.
- 서브에이전트 캡처 — 호스트 표면 부재(§7 한계, 2026-07-22 실측). 호스트가
  훅 표면을 제공하면 재상정.
- register-on-first-event — D43 비채택 사유 참조.
- MCP 기동 세션 lazy 생성 — D42 비채택 사유 참조.
- exec 3종(D21)·Codex 가드 동등성(D35 후속)·Grep 가드·plugin manifest·
  semantic 보강·spill journal 등 §10 잔여 — 전부 이월.

### 1.3 선행 게이트

- 없음 — v0.4의 G1~G3급 외부 표면 관측이 불필요한 순수 내부 작업이며,
  필요 실측(store 집계·drops 클러스터·서브에이전트 프로브)은 브레인스토밍
  세션에서 선행 완료(§9).

## 2. 집계 (D40)

- `store.SizeStat` 확장: 기존 Sources/Artifacts/BlobBytes에
  FileBytes(content.db `os.Stat` 크기)·ShadowOwnedBytes·ShadowOwnedBlobs
  추가. 귀속 술어(SQL 1쿼리): artifact의 sources 중 `source_kind != 'hook'`
  이 하나도 없으면 shadow 귀속(소스 0개 artifact는 비귀속 — 명시 등록
  경로의 일시 상태를 삭제 대상에 넣지 않는다).
- 접두 분해는 doctor 측에서 session.db 조인: `artifact_created` 이벤트의
  artifact_refs(content_hash 주소, §5 계약)를 세션 접두별로 수집해
  shadow-귀속 hash 집합과 교차 — 단일 접두면 그 접두, 복수면 `shared`,
  어떤 이벤트에도 없으면 `unattributed`(구버전 캡처 등). 접두 구분은
  `cc:`/`cx:` 리터럴 2종만 인정하고, 그 외 접두 세션의 artifact_created는
  `unattributed`로 계상한다(보수 폴백 — 현행 관측상 부재: 019 빈 세션은
  artifact_created 자체가 없어 자연 배제. 신규 네임스페이스가 실제로
  artifact_created를 만들면 전용 버킷을 §10에서 재상정).
- 출력:
  `[14] content.db: sources=%d artifacts=%d blob=%dB file=%dB`
  `[15] shadow-owned: %dB blobs=%d (cc:=%dB cx:=%dB shared=%dB unattributed=%dB)`
  session.db 불용 시 [15]는 `shadow-owned: %dB blobs=%d (세션 분해 없음)`.
  D38 경고 줄(임계 초과 시)은 [14] blob 기준 그대로, 문구만 D41 안내로
  갱신(§3).

## 3. hook 전용 선택 purge (D41)

- CLI: `context-router purge --project <id|path> --hook-only [--force]`.
  `--all --hook-only`·`--older-than --hook-only` 조합은 둘 다 사용 오류로
  거부 — 선택 삭제와 전체/시점 선별의 조합 의미는 실수요 실증 후
  정의(과잉 조합 방지).
- 삭제 트랜잭션: shadow-귀속 artifact 집합(D40 술어와 동일)에 대해
  sources·chunks(FTS 동기 삭제 포함)·artifacts 행 삭제. explicit 공유
  blob·그 sources는 불변. 삭제 후 VACUUM(트랜잭션 외부, 실패
  log-and-continue). 확인 게이트: confirmPurge 재사용, 문구에 사전 집계한
  회수 견적(`shadow-owned` 바이트·blob 수) 표시.
- session.db·ledger.db 불변 — hook **이벤트**는 세션 retention 소관이고
  이 명령은 content(shadow blob) 정리 전용. 이벤트의 artifact_refs가
  삭제된 blob을 가리키게 되는 것은 기존 계약 그대로(ref는 content_hash
  주소이며 fetch 실패는 정상 경로 — CAS 부재 응답).

## 4. 세션 위생 (D42)

- `session.Sweep` 확장(시작 시 1회 스윕에 합류): 빈 세션 GC — 술어는
  "비-session_start 이벤트 0건 AND started_at < now-7d"(session_start가
  복수여도 빈 세션). retention_sec 무관. 삭제는 세션 행 + 그 세션의
  session_events(FTS 동기 삭제 포함). 고지는 기존 스윕 stderr 1줄에 병합
  (`… empty-session GC n건`).
- doctor [6] 확장: `[6] session.db: quick_check=ok sessions=%d (empty=%d)`
  — empty는 경과 무관 현재 빈 세션 수(GC 예정량이 아닌 축적 관측).
- 기존 55개는 스테일 등록 정리(사용자) 후 7일 유예 경과분부터 자동 소멸.

## 5. drops 진단 필드 (D43)

- `appendDrop` 라인 형식: `ts \t reason \t sid8 \t hook_event \t tool` —
  sid8은 세션 ID 앞 8자(`cc:12345` 꼴 접두 포함 8자), 미상 필드는 `-`.
  모든 appendDrop 호출점(unknown-session·bad-input·shadow-oversize·
  shadow-denylist 등)에 가용 필드 전달.
- 소비자 호환: doctor [12]의 reason 집계는 필드 2 기준 유지, 테스트
  readDrops는 substring 검사 — 신구 라인 혼재 무해(라인 단위 독립).

## 6. 편승

- 구명 `CTR_SHADOW_WARN_BYTES` breadcrumb 2건 제거: v0.4 설계서 D38 문면의
  구명 병기 구절 + `internal/cli/cli.go` 구명 주석(36행 부근). 개명
  결정(세션-17 ①)의 zero-compat-cost 논리 일관 — 외부 사용자 부재·별칭
  부재 상태에서 breadcrumb만 유지할 이유 없음.

## 7. 한계 (v0.5 명문화)

- **서브에이전트 캡처 갭**: 서브에이전트의 도구 호출에는 훅이 발화하지
  않는다(2026-07-22 실측 — 프로브 서브에이전트 Read 1회가 부모 세션
  이벤트로도 drop으로도 기록되지 않음). 서브에이전트 작업량이 큰 워크플로
  에서 session.db 계측은 컨트롤러 세션만 반영한다. 호스트 훅 표면이 생기면
  재상정.
- drops 재발 표면: 훅 설치/업그레이드 시점에 이미 진행 중이던 세션과
  resume 세션은 SessionStart 없이 이벤트를 쏠 수 있다(§9 판정). D43
  필드로 재발 시 즉시 판정 가능 — 자동 등록은 그 후 재상정.
- [15] 접두 분해의 정확도는 artifact_created 이벤트 보존에 의존 — 세션
  retention 스윕으로 이벤트가 소멸하면 해당 blob은 `unattributed`로
  이동한다(집계는 스냅샷 관측이지 회계 원장이 아님).

## 8. 검증 계약 (계획 단계 상세화)

- D40: SizeStat 귀속 단위 테스트 — hook-만 artifact 귀속 / hook+explicit
  공유 artifact 비귀속 / 소스 0 artifact 비귀속 / FileBytes 실파일 반영.
  접두 분해 — cc:/cx:/shared/unattributed 각 1케이스 + session.db 불용
  폴백.
- D41: e2e — explicit 공유 blob 보존 + hook-만 blob·sources·chunks 삭제 +
  FTS 히트 소멸 + 견적 문구 + `--all`/`--older-than` 조합 거부 + VACUUM
  호출(파일 축소 단정은 플랫폼 비결정성으로 비단정, 호출 사실만).
- D42: 7일 경계(경과/미경과)·retention 0과 2592000 무관 적용·실이벤트
  세션 보존·session_start 복수 빈 세션 GC·[6] 집계 문면.
- D43: 라인 형식·미상 필드 `-`·doctor [12] reason 집계 신구 혼재.
- 공통: Go 테스트 `-p 1`(메모리 캡), deny 단정 + 현장 색인 테스트는
  `CTR_HOOK_DEADLINE_MS=60000` 주입(세션-17 F2 처방).

## 9. 관측 실측 기록 (2026-07-22, 브레인스토밍 세션 — 컨트롤러 수행)

- store: [14] sources=172 artifacts=285 blob=14,093,658B / content.db 파일
  52,871,168B(3.75배 — FTS·free page). 임계 100MiB의 14% — sweep 재상정
  조건(D38 경고 실발화) 미충족 재확인.
- session.db(970KB): 78세션·2,223이벤트. cc: 18세션·2,140이벤트(tool_call
  1142·file_edit 311·artifact_created 289·tool_result_summary 289·test_run
  67·git_diff 21·error 11·build_run 8·warning 2), cx: 5세션·9이벤트,
  019(무접두) 빈 세션 55개 — producer `context-router/0.1.0`(스테일 MCP
  등록이 기동마다 생성), retention_sec=0, session_start 외 이벤트 없음.
- drops(worktree unknown-session 246건): 시간 클러스터 3개 — ①100건/34분
  (07-20, cc: 첫 세션 등록 81초 전 개시) ②116건/36분(근처 세션 등록 전무)
  ③30건/13분(07-22, 스테일 0.1.0 MCP 세션·codex exec 시간대 정렬). 판정:
  훅 설치/업그레이드 시점에 이미 진행 중이던 세션 + Codex 신뢰 승인
  과도기의 SessionStart-부재 활동. 정상 상태 재발 드묾, 재발 표면은 §7.
- 서브에이전트 프로브: 최소 서브에이전트(Read 1회) 실행 → 부모 세션
  이벤트 0·drop 증가 0 — 서브에이전트 도구 호출에 훅 미발화 확정(§7).
- 도그푸딩 캡처 정상 확인: 컨트롤러 세션(cc:6ab876b0) tool_call 실시간
  기록 중(조회 1분 전까지).

## 10. 의도적 미결 (v0.6+ 후보)

exec 3종(D21 트랙), Codex 가드 동등성(D35 후속), shadow 자동 sweep(D38
경고 실발화 시 — D41 수동 purge 도입으로 긴급성 추가 하락), command
shadow denylist 잔여 표면 축소(D39 후속), 서브에이전트 캡처(호스트 표면
필요, §7), register-on-first-event(D43 재발 실측 후), Grep 도구 가드,
plugin manifest, 무작위 A/B 하네스·OTel(D27), semantic 보강(recall@k
기준선 후), spill journal(재상정 조건 v0.3 §1.3), `repository{}` 기입,
`invalidates`, payload 필드 조회(virtual generated column), title dedup,
CAS 갱신 시 구버전 blob 즉시 orphan-GC(실해 미관측), [15] 접두 신규
네임스페이스 대응(cc:/cx: 외 접두의 artifact_created 발생 시).

## 11. 적대 검증 처리 기록

- (설계 체크포인트 이중 적대 검수 후 추기.)
