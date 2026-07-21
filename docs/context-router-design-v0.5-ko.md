# Context Router v0.5 설계서 (store 수명주기)

- 전제: v0.4.0 태그 + follow-up 웨이브 머지(main d4c204b, PR #14, CI 3-OS
  GREEN — 릴리스 아님), 도그푸딩 Claude marker 0.4.0 + Codex 실세션 A/B 가동
  (`/hooks` 신뢰 승인 완료). 실측(2026-07-22, §9): [14] blob(CAS)
  14.1MB(임계 14%)·content.db 파일 52.9MB(별개 축), session.db 78세션 중
  빈 세션 55개
  (스테일 0.1.0 MCP, retention 0 영구 잔존), drops unknown-session 246건
  (3클러스터 — 과도기 산물 판정), cx: 5세션·9이벤트(축적 초기).
- 스코프 확정 근거: session-18 브레인스토밍 → 사용자 결정 7건(축=store
  수명주기 통합 웨이브 / sweep 제외·집계+purge만 / cx:는 귀속 집계 접두
  차원만 편입 / breadcrumb 편승 제거 / 표면=doctor+purge 확장 / 블록
  A·B·C 각 확정).

## 0. 결정 이력 (v0.5 신규 — D39까지는 v0.4 설계서·이전 델타 체인)

- **D40** shadow 귀속 바이트 집계 = doctor 확장만(새 커맨드·MCP 도구 없음):
  ① [14]에 content.db **파일 크기 병기** — blob(BlobBytes)은 content.db
  **밖** `artifacts/` CAS 물리 파일 합산이고 content.db 파일은 청크 텍스트+
  FTS 인덱스로 **별개 저장소 축**이다(적대 검수 교정 §11). 병기는 디스크
  점유 2축의 가시화이며, D38 경고 평가 기준은 현행 blob(CAS) 총량 유지 —
  D41이 CAS 실회수를 포함하므로 "purge → blob 감소 → 경고 해소" 일관이
  성립한다(content.db 파일은 free page 탓에 즉시 안 줄어 기준 부적합).
  ② [15] 신설 `shadow-owned` — 귀속 단위는 artifact가 아니라
  **content_hash**(CAS 파일 키): 어떤 hash가 shadow 귀속 ⟺ 그 hash를
  참조하는 **모든** artifact의 모든 source가 `kind=hook`이고, 그 hash를
  `raw_blob_hash`로 참조하는 비-hook source도 없다(cross-media 공유·raw
  참조가 있으면 explicit 귀속 — D37 티어 정합, 적대 검수 교정 §11).
  바이트는 물리 CAS 파일 크기 합산(dedup 자동 — 견적=회수 일치). ③ [15]에
  세션 접두 분해(`cc:`/`cx:`/`shared`/`unattributed`) 병기 — **프로젝트의
  `worktrees/*/session.db` 전체 순회 병합**으로 `artifact_created`
  refs(content_hash)와 조인(content.db는 프로젝트 레벨·session.db는
  worktree 레벨 — 단일 worktree 조인은 체계적 과소귀속, 적대 검수 교정
  §11). 다접두 공유는 임의 티어 없이 `shared` 버킷(정직 우선). 일부
  session.db 불용 시 `incomplete` 표시, 전부 불용 시 접두 분해만
  폴백([15] 자체는 유지).
- **D41** hook 전용 선택 purge = 기존 `purge`에 `--hook-only` 플래그:
  삭제 술어는 **D40 [15] 귀속 집합(content_hash 단위)과 동일**(explicit
  공유 hash 보존) — [15]가 곧 회수 견적. 회수는 **단일 store 작업**:
  트랜잭션 내 술어 재검증 → 행 삭제 → 커밋 후 그 hash 명시 집합의 **CAS
  물리 파일 직접 회수**(행 삭제+VACUUM만으로는 blob이 회수되지 않는다는
  적대 검수 교정 §11 — 상세 실행 계약은 §3). 보고는 견적이 아닌 **실회수
  바이트**. `--project`와만 조합(`--all`·`--older-than` 조합 거부).
  runPurge 합류는 조기 전용 분기(§3 — 전체 삭제 기본 분기 도달 금지).
  session.db·ledger 불변(세션 수명은 retention 스윕 소관 — 경계 분리).
  confirmPurge 확인 게이트 재사용 + 확인 문구에 회수 견적 표시. D38 경고
  문구는 "비선택 성격 병기" → "`--hook-only` 선택 삭제 가능" 안내로 승격.
- **D42** 세션 위생 = 빈 세션 GC + [6] 집계 표시: 시작 시 1회 retention
  스윕에 빈 세션 GC 합류 — **이벤트가 session_start뿐이고 시작 후 7일
  경과**한 세션의 행+이벤트 삭제, **retention_sec 값과 무관하게 적용**
  (0은 스테일 0.1.0 바이너리의 기본값이지 의도 표명이 아니고, 빈 껍데기는
  표명 대상 데이터가 없음). 실이벤트 보유 세션은 retention 표명 존중(현행
  불변). doctor [6]에 `sessions=N (empty=M)` 병기. MCP 기동 세션의 lazy
  생성은 비채택(기동 기록의 관측 가치 + GC로 결과 동일 — 구조 변경 회피).
  근본 처방은 스테일 `mcp_servers.ctr` 등록 정리(사용자 수동 — 세션-17
  캐리오버 4)이며 본 결정은 재발 대비 위생. GC와 동시 hook append의
  TOCTOU 봉쇄: hook append는 세션 존재 확인과 이벤트 INSERT를 **같은
  트랜잭션**으로 묶어 삭제된 세션으로의 커밋을 거부한다(현행 SessionExists
  선조회+별도 Append·FK 부재 구조에서는 GC가 그 사이에 세션을 지우면
  영구 orphan 이벤트 — Codex 고유 발견, §11·§4).
- **D43** drops 로그 진단 필드: 현행 `ts \t reason` 2필드로는 근인 판정이
  우회 실측 의존(§9 — 246건 판정에 시간 클러스터×세션 정렬 필요). 라인을
  `ts \t reason \t 세션ID접두8 \t hook_event \t tool_name`으로 확장, 가용
  필드만 채우고 없으면 `-`(전체 UUID 미기록 — 로그 파일 식별자 최소화).
  테스트 소비자(readDrops)는 substring 검사라 무해하나 **doctor [12]의
  `dropsByReason`은 정확-2필드 강제라 신형식이 전부 `unparsed`로 계상된다
  (적대 검수 수렴 발견 §11)** — [12] 집계기를 신구 공용으로 완화하고(§5)
  신규 필드는 sanitize한다. **register-on-first-event(미지 세션 자동
  등록)는 비채택**
  — 246건은 과도기 산물(§9 판정)이라 상시 표면을 늘리지 않고, 재발 시
  D43 필드로 즉시 판정 가능해진 뒤 재상정.

## 1. v0.5 제품 계약

### 1.1 범위

- shadow 귀속 바이트 집계 + 접두(cc:/cx:) 분해 — doctor [14] 확장·[15]
  신설(D40, §2).
- hook 전용 선택 purge `--hook-only` + CAS 실회수(D41, §3).
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
  FileBytes(content.db `os.Stat` 크기)·ShadowOwnedBytes·ShadowOwnedHashes
  추가. 귀속 술어는 **content_hash 단위**: 그 hash를 참조하는 전체
  artifact·source를 묶어 판정 — 비-hook source가 하나라도 있으면(직접
  참조든 `raw_blob_hash` 참조든) 비귀속, hook source 1개 이상이고 전부
  hook이면 귀속(source 0개 hash는 비귀속 — 명시 등록 경로의 일시 상태를
  삭제 대상에 넣지 않는다). ShadowOwnedBytes는 귀속 hash의 **물리 CAS
  파일 크기 합산**(BlobBytes와 동일 기저 — [15]와 [14]가 같은 축, 논리
  byte_length 합산은 cross-media dedup에서 과대 계상이라 비채택 §11).
- 접두 분해는 doctor 측에서 session.db 조인: 프로젝트의
  `worktrees/*/session.db` **전체 순회**(purgeSessionFiles의 worktrees/*
  순회 관례와 동형)로 각 DB의 `artifact_created` 이벤트
  artifact_refs(content_hash 주소, §5 계약)를 세션 접두별로 병합 수집해
  shadow-귀속 hash 집합과 교차 — 단일 접두면 그 접두, 복수면 `shared`,
  어떤 이벤트에도 없으면 `unattributed`(구버전 캡처 등). 접두 구분은
  `cc:`/`cx:` 리터럴 2종만 인정하고, 그 외 접두 세션의 artifact_created는
  `unattributed`로 계상한다(보수 폴백 — 현행 관측상 부재: 019 빈 세션은
  artifact_created 자체가 없어 자연 배제. 신규 네임스페이스가 실제로
  artifact_created를 만들면 전용 버킷을 §10에서 재상정). 일부 session.db
  열기 실패 시 분해 결과에 `incomplete` 병기(불완전 집계의 무표시 금지).
- 출력:
  `[14] content.db: sources=%d artifacts=%d blob=%dB file=%dB`
  `[15] shadow-owned: %dB hashes=%d (cc:=%dB cx:=%dB shared=%dB unattributed=%dB)`
  일부 session.db 불용 시 괄호 끝 ` incomplete` 병기, 전부 불용 시
  `shadow-owned: %dB hashes=%d (세션 분해 없음)`. D38 경고 줄(임계 초과
  시)은 [14] blob 기준 그대로, 문구만 D41 안내로 갱신(§3).

## 3. hook 전용 선택 purge (D41)

- CLI: `context-router purge --project <id|path> --hook-only [--force]`.
  `--all --hook-only`·`--older-than --hook-only` 조합은 둘 다 사용 오류로
  거부 — 선택 삭제와 전체/시점 선별의 조합 의미는 실수요 실증 후
  정의(과잉 조합 방지).
- 실행 계약(단일 store 작업): ① 확인 게이트(confirmPurge 재사용 — 문구에
  사전 집계 회수 견적 `shadow-owned` 바이트·hash 수 표시) → ② 삭제
  트랜잭션 — **술어 재검증 후**(견적 시점 이후 비-hook source가 생긴
  hash는 대상 제외 — 견적↔삭제 경합 해소) shadow-귀속 hash의
  sources·chunks(FTS 동기 삭제 포함)·artifacts 행 삭제 → ③ 커밋 후 그
  hash **명시 집합**의 CAS 물리 파일 회수 — 삭제 직전 미참조
  재확인(경합 등록 보호), 범용 orphan 스캔의 age-gate 경로 비의존 →
  ④ 보고는 **실회수 바이트**(견적 아님) + 파일 부분 실패 목록(orphan
  잔존은 안전 방향 실패 — `--gc`로 후속 회수 가능) → ⑤ VACUUM
  (content.db 축 부차 회수, 트랜잭션 외부, 실패 log-and-continue —
  라이브 서버 시 실패 흔함 §7). explicit 공유 hash·그 sources는 불변.
- runPurge 합류: `sessions`/`gcOnly`와 동형의 **조기 전용 분기**로
  인터셉트 — selective/전체 삭제 기본 분기(`os.RemoveAll`)에 도달하지
  않음을 계약으로 명시(플래그만 추가하고 분기를 앞세우지 않으면
  프로젝트 통삭제 랜드마인 — 적대 검수 §11).
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
- 동시성 계약: hook append는 세션 존재 확인과 이벤트 INSERT를 **같은
  트랜잭션**으로 묶는다(BEGIN IMMEDIATE 또는 `INSERT … SELECT … WHERE
  EXISTS` 동치) — 현행 SessionExists 선조회+별도 Append·FK 부재
  구조에서는 GC가 그 사이에 세션을 지우면 삭제된 session_id로 이벤트가
  커밋돼 retention 조인에서도 빠지는 영구 orphan이 된다(Codex 고유
  발견 §11).
- doctor [6] 확장: `[6] session.db: quick_check=ok sessions=%d (empty=%d)`
  — empty는 경과 무관 현재 빈 세션 수(GC 예정량이 아닌 축적 관측).
- 기존 55개는 스테일 등록 정리(사용자) 후 7일 유예 경과분부터 자동 소멸.

## 5. drops 진단 필드 (D43)

- `appendDrop` 라인 형식: `ts \t reason \t sid8 \t hook_event \t tool` —
  sid8은 세션 ID 앞 8자(`cc:12345` 꼴 접두 포함 8자), 미상 필드는 `-`.
  모든 appendDrop 호출점(unknown-session·bad-input·shadow-oversize·
  shadow-denylist 등)에 가용 필드 전달.
- 소비자 갱신: doctor [12] `dropsByReason`은 현행 정확-2필드 강제(탭
  초과 시 unparsed)라 신형식이 전부 `unparsed`로 계상 — **신구 공용
  파서로 완화**(탭 분리 필드[1]=reason, 2·5필드 모두 수용, `isUnixTS
  (필드[0])` 유지). 기록 측은 신규 필드 sanitize(탭·개행 제거·길이 제한)
  로 파서 오염 방지. 테스트 readDrops는 substring 검사라 무해(신구 라인
  혼재는 라인 단위 독립). 기존 "3필드 → unparsed" 단정 테스트는
  신계약으로 갱신.

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
  retention 스윕으로 이벤트가 소멸하면 해당 hash는 `unattributed`로
  이동한다(집계는 스냅샷 관측이지 회계 원장이 아님).
- VACUUM은 서버 동시 가동 시 실패가 흔하다(WAL 체크포인트 배타 요건) —
  content.db 축 공간 회수는 서버 정지 후 재실행 시 실현. CAS 축
  회수(D41 ③)는 이와 무관하게 동작한다.

## 8. 검증 계약 (계획 단계 상세화)

- D40: SizeStat 귀속 단위 테스트 — hook-만 hash 귀속 / hook+explicit 공유
  hash 비귀속 / **cross-media 공유**(동일 content_hash·상이 media_type의
  hook·explicit artifact) 비귀속 / **raw_blob_hash-만 비-hook 참조** hash
  비귀속 / source 0개 hash 비귀속 / FileBytes 실파일 반영 /
  ShadowOwnedBytes=물리 파일 기저. 접두 분해 — cc:/cx:/shared/unattributed
  각 1케이스 + **다중 worktree 병합** + 일부 불용 `incomplete` + 전부
  불용 폴백.
- D41: e2e — explicit 공유 hash 보존 + hook-만 hash의 행·**CAS 물리 파일**
  삭제 + FTS 히트 소멸 + **실회수 바이트 보고 문면** + 술어 재검증(견적
  후 비-hook source 추가 시 대상 제외) + `--all`/`--older-than` 조합 거부
  + **전체 삭제 기본 분기 비도달**(--hook-only 후 content.db·비대상 행
  잔존 단정) + VACUUM 호출(파일 축소 단정은 플랫폼 비결정성으로 비단정,
  호출 사실만).
- D42: 7일 경계(경과/미경과)·retention 0과 2592000 무관 적용·실이벤트
  세션 보존·session_start 복수 빈 세션 GC·[6] 집계 문면·**append×GC
  동시성**(존재 확인+INSERT 동일 트랜잭션 — 삭제 후 삽입 거부 회귀).
- D43: 라인 형식·미상 필드 `-`·doctor [12] 신구 혼재 집계(2·5필드)·
  sanitize·기존 "3필드 unparsed" 단정 테스트의 신계약 갱신.
- 편승: cli_test.go의 [14] 경고문 "무구분" 부분문자열 단정을 신문구로
  갱신(D38 문구 승격의 유일 실브레이크 — 적대 검수 §11).
- 공통: Go 테스트 `-p 1`(메모리 캡), deny 단정 + 현장 색인 테스트는
  `CTR_HOOK_DEADLINE_MS=60000` 주입(세션-17 F2 처방).

## 9. 관측 실측 기록 (2026-07-22, 브레인스토밍 세션 — 컨트롤러 수행)

- store: [14] sources=172 artifacts=285 blob(CAS 물리 파일)=14,093,658B /
  content.db 파일(청크 텍스트+FTS — 별개 축) 52,871,168B. 임계 100MiB의
  14% — sweep 재상정 조건(D38 경고 실발화) 미충족 재확인.
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

## 11. 적대 검증 처리 기록 (2026-07-22, 설계 체크포인트)

- 이중 적대 검수 1패스(초안 4389e92 대상): 서브에이전트(opus) C1·I3·M3 +
  Codex adversarial-review(high 3·medium 2, verdict NO-SHIP) — 전 건 반영,
  NO-SHIP 사유 해소.
- 수렴 3건(양측 공통): ① `--hook-only`의 행 삭제+VACUUM은 CAS 바이트를
  회수하지 못함 — blob은 content.db 밖 `artifacts/` 물리 파일이고 회수
  경로는 GC뿐(D41을 실회수 단일 작업으로 재설계, D40 ①의 측정 축 서술
  정정, 회수 견적↔실회수 일치 계약) ② D43 신형식이 doctor [12]
  정확-2필드 파서에서 전부 unparsed — 자기 목적 무력화(신구 공용 파서로
  완화 + sanitize) ③ [15] 단일 worktree 조인은 다중 worktree에서 체계적
  과소귀속(worktrees/* 전체 순회 병합 + `incomplete` 표시).
- Codex 고유 2건: ① 귀속 단위 artifact → **content_hash** 교정 —
  artifact는 (content_hash, media_type) 유일이지만 CAS 파일은 hash 단일
  키라 cross-media 공유·`raw_blob_hash` 참조를 artifact 단위 술어가 보지
  못함(과대 견적 + explicit 데이터 오삭제 위험 — D40 ②·§2 술어 재정의,
  물리 파일 기저 합산) ② 빈 세션 GC × hook append TOCTOU — SessionExists
  선조회+별도 Append·FK 부재에서 영구 orphan 이벤트(존재 확인+INSERT
  동일 트랜잭션 계약 — D42·§4).
- 서브에이전트 고유 3건: ① runPurge 합류 미명세 시 `--hook-only`가
  비-selective 전체 삭제 기본 분기로 새는 랜드마인(§3 조기 전용 분기
  계약) ② cli_test "무구분" 경고문 단정 실브레이크(§8 열거) ③ VACUUM
  라이브 서버 실패 흔함(§7 병기 + D41에서 부차로 강등).
- 무발견 확인(검수 검증 항목): source_kind 실값 {hook,file,web,inline}과
  D37 정합, artifact_created ref=content_hash 주소 계약, doctor
  session.db 잠금 무경합(OpenReadOnly), D42 Sweep 합류 정합·자기 세션
  7일 게이트 자연 배제, breadcrumb 제거 참조 무파괴, [6]/[14] Contains
  단정의 접미 확장 안전.
- (계획 체크포인트의 adversarial-review 1패스는 별도 — 계획 확정 시
  수행해 여기 추기.)
