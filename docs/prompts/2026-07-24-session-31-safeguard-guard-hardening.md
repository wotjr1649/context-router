# Session 31 — v0.11 SDD T3 구현·이중리뷰 병합 + Fable 세이프가드 기계적 강제(근본개선) (2026-07-24)

1. **Session/span/model**: session-31, 2026-07-24. Fable(xhigh)로 T3 재개 착수 →
   사용자 지시로 **Opus 4.8(max) 전환**해 세이프가드 근본개선(가드 훅+코디네이터 패턴)
   구축·dogfood → 세션 말미 **Fable(xhigh) 복귀**(핸드오프 작성). 서브에이전트(구현/리뷰/코디네이터)
   전부 model:"opus". **핸드오프 사유**: 현재 세션 컨텍스트가 세이프가드 트립 상태로 남아
   Fable 응답이 계속 폴백 — 다음 세션은 신규 컨텍스트로 재개.

2. **Starting prompt (verbatim)** — 이 세션을 부팅한 session-30 다음-세션 프롬프트:
   > docs/prompts 최신 기록(session-30)을 읽고 재개. v0.11 SDD 실행 중 — 브랜치 `feat/v0.11-d58-d62`, **T1·T2 완료(chain 1223504..2bf20be), 다음 = T3(sandbox Unix)부터 BASE=2bf20be**. superpowers:subagent-driven-development 계속(`docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md`, 태스크별 fresh 서브에이전트 general-purpose는 model:"opus" 명시, 계획 각 태스크의 "검토 반영" 노트를 implementer에 필수 전달, BASE는 원장 기록). T3 신규 의존 go-landlock v0.9.0 1건 추가. **보안 민감 태스크(T3·T5) 리뷰 체크포인트에서는 Codex 리뷰 원문을 메인 컨텍스트로 직접 읽지 말 것 — 서브 리뷰어+Codex 병합·판정을 opus 서브에이전트에 위임하고 개념 수준 요약만 받아 fix 파견(session-30 세이프가드 인시던트 재발 방지, 메모리 참조)**. 태스크/통합/최종 리뷰는 서브 리뷰어+Codex review --base <BASE> 병렬 1패스(재리뷰 서브 단독). 다운스트림: T2 post-Start vs pre-Start 취소 반환형태 상이 → T4/T5에서 ctx.Err() 참조. go test -p 1·gofumpt 로컬·git add -A 금지. Windows 로컬 미컴파일 unix 파일은 CI 3-OS로 실검증(T11). **Fable 유지 + 보안 서술 최소화(벡터 열거 금지)**. ultrathink

   세션 중 사용자 지시 2건(verbatim):
   > codex에 리뷰나 의뢰 할때 safeguard에 걸린 만한 메시지나 프롬프트를 제공하지 못하게 근본 개선을 해서 다음 부터 fable 모델 safeguard에 걸리지 않게 해결하라. ultrathink
   > 현재 세션은 safeguard가 계속 걸려있다. 다음세션에서 이어서 진행할 수 있게 핸드오프작성한다. ultrathink

3. **What was done**:
   - **T3(sandbox Unix) 구현 완료**: commit `c8700f9`(base 49f2203). run_unix.go(setpgid 그룹 실행)
     + launcher_linux/darwin/other.go(OS별 FS 제한) + main.go `__exec-launcher` 분기 + landlock 실증
     테스트. Step 0 capWriter를 sandbox.go 공통 승격, Spec에 SelfExe 추가, cancellation 계약을 windows와
     정합하게 미러(pre-Start ctx 분류 먼저, SelfExe=="" 무격리 우회 없음=fail-closed). 신규 의존
     go-landlock v0.9.0(+ 불가피 전이의존 psx v1.2.77). Windows 로컬 미컴파일 → cross-compile
     빌드(linux/darwin/freebsd)+vet+windows 스위트로 검증, 실 unix 동작은 CI 3-OS(T11)로 이월.
   - **T3 이중 리뷰 완료 + 병합(코디네이터 dogfood)**: 서브 리뷰어(opus) + Codex `review --base 49f2203`
     병렬. 신설 코디네이터 서브에이전트가 양 산출물 병합 → 개념 수준 findings(§5 이월에 복제). **T3 fix
     라운드 미수행.**
   - **세이프가드 근본개선(주 deliverable, session-31 신규)**: session-30의 "규율"을 **기계적 강제**로
     승격. ① PreToolUse 훅 `~/.claude/hooks/codex-review-guard.mjs`(settings.json Bash·PowerShell
     매처 등록, JSON 검증) — context-router에서 **메인 세션**의 텍스트-반환 Codex 명령(`result`,
     `--background` 없는 `review`/`adversarial-review`)을 deny, **서브에이전트는 `agent_id`로 허용**
     (Claude Code hooks 레퍼런스 확인 — 서브에이전트 호출에만 존재). 메인은 `review --background`·`status`만.
     fail-open, 타 프로젝트·일반 리뷰 무영향, 모델 무관 강제. 자체테스트 15/15
     (`~/.claude/hooks/codex-review-guard.test.mjs`). **라이브 양방향 검증**: 메인 `result`→deny,
     코디네이터 서브 `result`→허용. ② 코디네이터 패턴·파견 템플릿·확장/해제법 = `docs/codex-secure-review.md`.
     ③ 프로젝트 CLAUDE.md Standing protocols·메모리(fable-security-prose-minimization)·MEMORY.md 갱신.

4. **Repo state**: 브랜치 `feat/v0.11-d58-d62`. **이 세션 커밋 전 HEAD=`c8700f9`.** main=`1223504`(미변경).
   이 세션이 추가 커밋: (a) 세이프가드 docs(`CLAUDE.md`+`docs/codex-secure-review.md`), (b) 이 핸드오프
   기록. **오픈 PR 없음**(로컬 커밋만, 세션 말미 푸시). tag v0.10.0 유지. **세이프가드 훅/settings/테스트/메모리는
   `~/.claude` 전역(이 repo 밖, 이미 활성)** — 저장소는 `docs/codex-secure-review.md`로 참조·재활성법 문서화.

5. **Carryovers**:
   - **다음 = T3 fix 라운드부터, BASE 유지 `49f2203`**. 병합 findings(로컬 `.superpowers/sdd/task-3-merged-findings.md`는
     gitignore라 아래 개념수준 복제 — 이게 소스오브트루스):
     - **Critical 1**: `run_unix.go` — D59 "종료 유예(WaitDelay) 5s 전 OS" 미설정 → kill 후 무한 대기 가능
       (서브·Codex 공통). 수정: `waitDelay` 상수를 `sandbox.go` 공통으로 이동 + `Start()` 전
       `cmd.WaitDelay = waitDelay` 설정(windows run_windows.go와 동형).
     - **Important 2**(Codex 단독): ① `cmd/context-router/main.go` 런처 진입부 — 설정 실패가 `ErrSetup`을
       가려 exit 1로만 나감 → 상태 전파(예약 exit code/상태 파이프). ② `launcher_linux.go` — `RWDirs(scratch)`에
       REFER 누락 → `.WithRefer()` 추가(ABI 2+에서 스크래치 내부 rename/link 대비).
     - **Minor 3**: `run_unix.go` 인플라이트 ctx deadline이 `TimedOut=false`(pre-Start/windows와 불일치)
       → deadline이면 true. `TestRunUnixTimeoutKillsGroup`가 그룹 시맨틱 미검증(단일 프로세스). 런처 오류
       경로 redaction(스크래치 경로 노출 소지).
     - **Disputed 1**: darwin SBPL "macOS 전면 실패"(Codex P1) = **코디네이터가 오탐 판정**(Go raw-string
       오독, 제안 수정은 no-op) → **맹목 수정 금지**, macOS CI 스모크로 종결.
   - fix 후 **재리뷰(서브 단독)**. 이후 T4~T11.
   - **보안 민감 리뷰(기계적 강제됨)**: 컨트롤러는 Codex 원문을 절대 읽지 않음 — 훅이 메인의 `result`/비-`--background`
     `review`를 deny. 시작은 `review --background`, 원문 읽기·병합은 opus 코디네이터 서브에이전트 파견
     (`docs/codex-secure-review.md`의 템플릿). T3 리뷰 Codex job id = `review-mryjtt3w-8aztsu`(필요시 서브에이전트가 재조회).
   - **다운스트림 계약(T2→T4/T5)**: post-Start 부모 취소=`(Result,nil)`(killed·TimedOut=false), pre-Start
     취소=ctx 오류 전파 → 시점별 반환형태 상이. T4/T5는 취소/비정상종료 구분에 `ctx.Err()` 참조.
   - Windows 로컬 unix 미컴파일 → CI 3-OS(T11 PR 게이트) 실검증.
   - 최종 리뷰 triage 이월 Minor(원장): T1 심링크 가드 직접테스트 없음, T2 post-Start 반환형태·386 트렁케이션,
     그리고 위 T3 Minor 3건.
   - 관찰 이월(session-28~29): D46 139MB 회수(사용자 창), empty GC 회수, doctor [17] dirty=untracked 코스메틱.

6. **Standing protocols**: session-30 §6 그대로 + **신규 확정(session-31)**:
   - **세이프가드 기계적 강제**: 컨트롤러(Fable/Opus 무관)는 Codex 리뷰 원문을 컨텍스트로 못 당김 — PreToolUse
     훅이 강제. 보안 민감 리뷰 병합은 opus 코디네이터 서브에이전트에 위임(개념 수준 findings만 회수). 상세·템플릿
     `docs/codex-secure-review.md`, 메모리 [[fable-security-prose-minimization]].
   - 유지: Fable + 보안 서술 최소화(벡터 열거 금지) / general-purpose 서브에이전트 model:"opus" 명시 /
     go test -p 1 / gofumpt 로컬 / `git add -A` 금지 / Codex 1패스/체크포인트(재리뷰 서브 단독) /
     스펙-계획 충돌 시 스펙 승 / 페이지파일 재활성화됨(자식 OOM 시 커밋 차지 1차 점검).

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-31)을 읽고 재개. v0.11 SDD 실행 중 — 브랜치 `feat/v0.11-d58-d62`, **T1·T2·T3 구현 완료(HEAD=c8700f9 + session-31 docs 커밋), 다음 = T3 fix 라운드부터, BASE 유지 49f2203**. T3 병합 findings는 session-31 §5에 개념수준으로 복제됨(로컬 .superpowers/sdd/task-3-merged-findings.md는 gitignore): **Critical 1**(run_unix.go WaitDelay 전OS 미설정 → waitDelay 상수 sandbox.go 이동+Start 전 cmd.WaitDelay 설정), **Important 2**(main.go 런처 설정실패가 ErrSetup 마스킹→상태 전파 / launcher_linux.go RWDirs .WithRefer() 누락), **Minor 3**, **Disputed 1**(darwin "macOS 전면실패"=오탐, 맹목수정 금지·macOS CI 스모크). T3 fix 서브에이전트(opus) 파견→재리뷰(서브 단독)→T3 완료 후 T4~T11. **보안 민감 리뷰는 기계적 강제됨**: 컨트롤러는 Codex 원문 못 읽음(PreToolUse 훅), 시작은 `review --background`·원문 병합은 opus 코디네이터 서브에이전트 파견(docs/codex-secure-review.md 템플릿, Codex job id review-mryjtt3w-8aztsu). superpowers:subagent-driven-development 계속(계획 `docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md`, "검토 반영" 노트 필수 전달, BASE 원장 기록). 다운스트림: T2 post-Start vs pre-Start 취소 반환형태 상이 → T4/T5 ctx.Err() 참조. go test -p 1·gofumpt 로컬·git add -A 금지. Windows 로컬 미컴파일 unix는 CI 3-OS 실검증(T11). **Fable 유지 + 보안 서술 최소화(벡터 열거 금지)**. ultrathink
