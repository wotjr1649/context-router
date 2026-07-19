# Context Router v0.0.2 (worker robustness 선행 패치) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** transform worker의 두 사망 메커니즘(windows Job commit 캡 + GC 반환 지연 / linux RLIMIT_AS ↔ Go VA 예약 충돌)을 실측 기반으로 완화하고 kill 사유를 구분 — v0.1 개발이 소비할 transform 스모크의 flake 원천 제거(설계 v0.1 §1.3, D19).

**Architecture:** 코드 변경은 전부 `internal/transform` 3파일 내: GOMEMLIMIT env 주입(부모, OS 캡은 하드 백스톱 유지), RLIMIT_AS 산정에 VA headroom 가산(unix self-apply), Spawn 오류 경로의 사유 접미사. 태그 없음, PR 1개, 게이트 문서 신설 없음(수용 기준 = 게이트 8 스위트 + churn 재현 + 3-OS CI GREEN).

**Tech Stack:** 기존 그대로(신규 의존성 0).

## Global Constraints

- 근거 설계: `docs/context-router-design-v0.1-ko.md` §1.3(D19) + session-04 실측(`docs/prompts/2026-07-19-session-04-v0.0.1-tag-complete.md` §2: VirtualAlloc errno=1455 7/20회, ubuntu 8.4ms 조기 사망).
- **`"worker killed"` 접두사 불변** — `cmd/context-router/main_test.go:1026,1077`·`internal/mcp/mcp_test.go`가 부분 문자열 단언. 사유는 접미사로만. ErrSummary 위생 계약(경로·env 미포함) 유지.
- GOMEMLIMIT은 **soft limit**(Go gc guide) — Job Object/RLIMIT 하드 백스톱을 절대 제거하지 않는다. 적대 할당이 mem-kill 대신 thrash→timeout으로 죽는 모드 전환은 허용(게이트 8 테스트가 "죽는다" 자체를 검증하도록 유지).
- 테스트: `go test -p 1 ./...`(메모리 캡 전역 규칙 준수), 장시간 재현 테스트는 `testing.Short()` 스킵. 응답 분할 규율(파일 전체 재작성 금지).
- 커밋 trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- 리뷰: 태스크 리뷰는 서브에이전트, 최종(pre-commit) 체크포인트에서 서브에이전트 + Codex `review --base <ref>` 병렬 1패스.

---

### Task 1: kill 사유 구분 (D19-c)

**Files:** Modify `internal/transform/transform.go:248-255`(Spawn 오류 경로), `internal/transform/transform_test.go`

**Interfaces:**
- Consumes: 기존 `Spawn(ctx, selfExe, req) (Result, error)` — waitErr 분기 2곳(transform.go:251, 254).
- Produces: `Result.ErrSummary` 사유 구분 — `"worker killed (cancelled)"` / `"worker killed (time limit)"` / `"worker killed (memory or crash)"` / stdout 파싱 실패는 `"worker killed (bad output)"`. 판정: `ctx.Err()`가 `context.Canceled` → cancelled, `context.DeadlineExceeded` → time limit, 그 외 waitErr(비정상 exit code — windows Job mem-kill은 exit 1, Go 런타임 OOM abort는 exit 2) → memory or crash. ErrKind는 "script" 유지(호출자 계약 불변).

- [ ] **Step 1: 실패 테스트** — ① 취소: bounded-churn 스크립트 Spawn 후 즉시 cancel → ErrSummary에 `"(cancelled)"` 포함 ② timeout: 무한 루프성 스크립트 + 짧은 ctx deadline → `"(time limit)"` ③ 기존 `TestSpawn_MemoryExplosion`이 여전히 `"worker killed"` 접두사 매치(수정 없이 통과해야 함 — 접미사만 달라짐) ④ 모든 케이스에서 ErrSummary에 경로·env 문자열 부재.
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/transform/ -run 'TestSpawn_Kill' -v` → 접미사 불일치로 FAIL.
- [ ] **Step 3: 구현** — waitErr 분기에서 `ctx.Err()` 우선 검사 후 접미사 선택. 254행(JSON 파싱 실패)은 ctx 정상 종료인데 출력이 깨진 경우이므로 `"(bad output)"`. 문구는 이 4종 외 금지.
- [ ] **Step 4: GREEN** — `go test -p 1 ./internal/transform/ ./internal/mcp/ ./cmd/... -v` (기존 단언 회귀 포함).
- [ ] **Step 5: Commit** — `git add internal/transform && git commit -m "fix: worker killed 사유 구분(cancelled/time/memory) — 접두사 불변 (v0.1 설계 §1.3 D19-c)"` (+trailer)

### Task 2: GOMEMLIMIT 주입 (D19-a)

**Files:** Modify `internal/transform/worker_windows.go`(applyMemLimit), `internal/transform/worker_unix.go`(applyMemLimit), `internal/transform/transform.go`(상수), `internal/transform/worker_windows_test.go`

**Interfaces:**
- Produces: 상수 `gomemlimitRatio = 0.8` — 자식 env에 `GOMEMLIMIT=<int(defaultMemLimitBytes*0.8)>`(바이트 정수 문자열) 주입. windows: `cmd.Env = append(os.Environ(), ...)`를 applyMemLimit 진입부에 추가(현재 Env 미설정=상속). unix: 기존 `CTR_WORKER_MEM` append 줄에 한 줄 추가. **Job Object/RLIMIT 캡은 그대로**(하드 백스톱).
- 근거: windows 사망 원인은 GC의 commit 반환 지연 — GOMEMLIMIT이 GC를 캡의 80%에서 선제 발동시켜 commit이 Job 캡(256MB)에 도달하기 전에 회수시킨다. Go 공식 가이드는 5–10% 여유를 권하나 Job은 RSS가 아닌 **commit**을 재므로 보수적 80%(설계 §1.3, 최종 수치는 Step 4 실측으로 확정 — 실측이 요구하면 이 상수만 조정).

- [ ] **Step 1: 실패 테스트** — ① env 주입 검증: applyMemLimit 후 `cmd.Env`에 `GOMEMLIMIT=214748364`(256MB×0.8) 존재(windows/unix 공통 로직이므로 windows 테스트 파일에서 cmd 검사) ② churn 재현(상주, `testing.Short()` 스킵): session-04 재현 레시피 — 할당 중심 스크립트(예: `s='A'*1048576; out=[]; for i in range(120): out.append(s+str(i))` 계열, 8MB+ 누적)를 **20회 반복 Spawn** → 20회 전부 mem-kill 없이 정상 Result(script 오류 포함 가능하나 `worker killed` 부재).
- [ ] **Step 2: FAIL 확인** — env 부재로 ① FAIL, ②는 현행 코드에서 통계적 FAIL(7/20 수준 — flaky 확인이 목적이므로 1회 실행으로 사망 관찰되면 충분, 관찰 안 되면 그대로 진행).
- [ ] **Step 3: 구현** — 두 applyMemLimit에 env 주입. 값 산정은 transform.go의 비공개 헬퍼 `gomemlimitBytes() int64` 1개(중복 금지).
- [ ] **Step 4: GREEN + 실측** — `go test -p 1 ./internal/transform/ -run 'TestSpawn' -v`(게이트 8 스위트 전체: MemoryExplosion·timeout·동시 2) + churn 20회 로컬(windows) 통과. thrash→timeout 모드 전환이 관찰되면 그 결과와 함께 게이트 8 테스트 기대를 재확인(죽음의 형태가 바뀌어도 "부모 생존+오류 Result" 불변).
- [ ] **Step 5: Commit** — `"feat: worker GOMEMLIMIT(캡의 80%) 주입 — windows commit 압박 완화 (v0.1 설계 §1.3 D19-a)"`

### Task 3: linux RLIMIT_AS headroom (D19-b)

**Files:** Modify `internal/transform/worker_unix.go`(selfApplyMemLimit), `internal/transform/transform_test.go`(산정 로직 단위 테스트 — 순수 함수 분리 시)

**Interfaces:**
- Produces: `selfApplyMemLimit`가 RLIMIT_AS를 `CTR_WORKER_MEM` 값 그대로가 아니라 **`값 + vaHeadroomBytes`**로 설정. 상수 `vaHeadroomBytes = 768 << 20`(초기값 768MB — ubuntu 실측 "할당 0인데 8.4ms 사망 = Go 런타임 VA 예약이 256MB 초과"를 상회하는 보수값; CI 실측으로 조정, 이 상수 1곳만 변경). **실질 메모리 제어는 T2의 GOMEMLIMIT이 담당**하고 RLIMIT_AS는 백스톱으로 후퇴 — 주석에 이 역할 전환을 명시.
- 주의: `CTR_WORKER_MEM`의 의미(순수 캡)는 불변 — headroom 가산은 self-apply 지점 1곳에서만. windows 경로 무영향.

- [ ] **Step 1: 실패 테스트** — headroom 산정을 순수 함수 `rlimitASBytes(cap int64) int64`로 분리해 단위 테스트(cap=256MB → 256MB+768MB; 기존 hard limit이 더 낮으면 그 한도 유지 로직은 기존 코드 재사용 확인).
- [ ] **Step 2: FAIL 확인** — `go test -p 1 ./internal/transform/ -run 'TestRlimit' -v`(빌드는 `GOOS=linux go build ./...`로 교차 확인 — 로컬은 windows).
- [ ] **Step 3: 구현** — selfApplyMemLimit에서 `lim.Cur = rlimitASBytes(n)`. 기존 "Max 초과 시 절삭" 로직 유지.
- [ ] **Step 4: GREEN + CI 실측** — 로컬 GREEN 후 push하여 **ubuntu CI에서 E2E cancellation smoke의 churn-pair retry/skip 경로가 발동하지 않는지** 관찰(기존: RLIMIT_AS 충돌로 8.4ms 사망 → retry ≤3 → skip). 여전히 조기 사망이면 vaHeadroomBytes 증액 후 재실측(왕복 최소화: 1차 증액은 ×2).
- [ ] **Step 5: Commit** — `"fix: linux RLIMIT_AS에 Go VA 예약 headroom — 조기 사망 제거 (v0.1 설계 §1.3 D19-b)"`

### Task 4: 통합 검증 + PR

**Files:** 없음(검증·리뷰만)

- [ ] **Step 1: 전체 회귀** — `go test -p 1 ./...` GREEN(메모리 캡 규칙 하).
- [ ] **Step 2: 브랜치 push + 3-OS CI GREEN 확인** — 특히 ubuntu의 E2E smoke 로그(T3 Step 4 판정).
- [ ] **Step 3: 교차 리뷰** — 서브에이전트 리뷰(diff 전체) + Codex `review --base main` 병렬 → 발견 머지·수정. 재리뷰는 서브에이전트만.
- [ ] **Step 4: PR 머지** — 제목 `fix: worker robustness — GOMEMLIMIT·RLIMIT_AS headroom·kill 사유 구분 (v0.0.2)`. 머지 후 브랜치 삭제. 태그 없음.

## Self-Review 기록
- 설계 §1.3의 (a)(b)(c) 전부 태스크 대응(T2/T3/T1). (d) ctr_fetch 문구는 v0.1 계획 소관 — 본 계획 비범위.
- 접두사 불변 제약이 T1 테스트 ③에 반영. GOMEMLIMIT soft-limit 리스크는 T2 Step 4 재확인으로 커버.
- 타입 스레딩: gomemlimitBytes()/rlimitASBytes() 시그니처 일관.
