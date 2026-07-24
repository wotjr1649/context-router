# Session 30 — v0.11 SDD 실행 착수(T1·T2 완료) + Fable 세이프가드 인시던트·프로토콜 확정 (2026-07-24)

1. **Session/span/model**: session-30, 2026-07-24. 시작 claude-fable-5(xhigh) → **T2 리뷰 체크포인트에서 세이프가드 발동**해 사용자가 Opus 4.8(max)로 전환, 인시던트 처리·핸드오프 작성을 Opus에서 수행. 다음 세션은 다시 Fable로 재개 예정. 서브에이전트(구현/리뷰/fix)는 전부 model: "opus".

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-29)을 읽고 재개. v0.11 스펙·계획은 SDD 전 Fable 검토까지 완료(계획 말미 "검토 반영 이력" 참조 — 초안 스니펫을 정정하는 노트가 각 태스크에 박혀 있으니 implementer 프롬프트에 노트 포함 필수). 이번 세션 = **v0.11 SDD 실행**: superpowers:subagent-driven-development로 `docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md` 11태스크 실행(T1부터, 태스크별 fresh 서브에이전트 — general-purpose는 model: "opus" 명시, BASE는 원장 기록). 태스크/통합/최종 리뷰 체크포인트는 서브 리뷰어 + Codex `review --base <BASE>` 병렬 1패스(재리뷰는 서브 단독). 신규 의존은 go-landlock v0.9.0 1건만. 메모리 캡 테스트 규칙(go test -p 1) 준수, gofumpt 로컬 확인, git add -A 금지. Windows 로컬에서 unix/linux 파일은 컴파일 대상이 아니므로 CI 3-OS로 실검증(T11 PR 게이트). **Fable 유지 + 보안 서술 최소화 엄수(exec/sandbox 구현 커밋 메시지·리뷰 문면도 개념 수준 — 벡터 열거 금지)**. ultrathink

3. **What was done**:
   - **브랜치**: `feat/v0.11-d58-d62` 생성(BASE main=1223504). 11태스크 원장 헤더 기록.
   - **T1 완료**(internal/sandbox 공통 — Spec/Result·ErrSetup·NewScratch·SweepStale·BaseEnv 닫힌 표): commits `4110888`+`77ccf3c`. 서브 리뷰 Approved + Codex P1 2건 채택 → fix 1라운드(SweepStale Lstat·IsDir 가드+scratchPrefix 필터 / BaseEnv 비-nil 보장 — nil이 exec.Cmd.Env로 가면 부모 환경 전체 상속) → 재리뷰(서브) Approved.
   - **T2 완료**(sandbox Windows — Job Object 실행·Probe): commits `3aecb8e`+`2bf20be`. 서브 Approved + Codex P1x1·P2x3 전건 채택 → fix 1라운드(명시적 잡 종료를 1차 메커니즘으로·닫힘은 crash 백스톱 / Start 전 컨텍스트 종료를 설정실패 sentinel로 오분류 않고 deadline→timeout·cancel→ctx 오류 전파+메시지 경로 제거 / 트리 종료 테스트가 후손 프로세스 실제 소멸 단정 / 메모리 캡 테스트를 무할당 직접 조회로 재작성해 대용량 커밋 위험 제거) → 재리뷰(서브) Approved(Go stdlib pre-done-ctx 동작·x/sys v0.47.0 심볼 실검증). Spec에 `MemLimitBytes uint64`/`ProcLimit uint32`(0=기본, 4GiB/64) 추가.
   - **세이프가드 인시던트**: T2 리뷰 체크포인트에서 **Codex `review` 원문을 `sed`로 Fable 메인 컨텍스트에 직접 읽어 병합·판정**하던 단계에서 dual-use 세이프가드 발동(리뷰 문면이 격리 관련 서술을 밀집 나열). 사용자가 Opus로 전환, T2 병합·판정을 개념 수준으로 완료하고 정상 마감. **재발 방지 프로토콜 확정**(아래 §6·메모리).

4. **Repo state**: 브랜치 `feat/v0.11-d58-d62` HEAD=`2bf20be`. main=`1223504`(미변경). chain `1223504..2bf20be` = 4커밋(4110888, 77ccf3c, 3aecb8e, 2bf20be). **오픈 PR 없음**(로컬 커밋만, 미푸시 상태에서 세션 말미 푸시). tag v0.10.0 유지. **T3~T11 미착수.**

5. **Carryovers**:
   - **다음 = T3(sandbox Unix) 부터, BASE=2bf20be**. run_unix.go(`//go:build !windows`) + launcher_linux/darwin/other.go + main.go `__exec-launcher` 분기(검토 반영으로 T3 이동). **신규 의존 go-landlock v0.9.0**(이 태스크에서 최초 추가·유일 승인 의존). T3 테스트의 selfExe는 `testSelfExe` 선례(실바이너리 1회 빌드) — `os.Executable()` 금지(무한재귀). `SelfExe==""` 무격리 우회 없음(fail-closed). landlock 실증 테스트 T3 편입, 커널 미지원은 `/sys/kernel/security/lsm`로 Skip.
   - **다운스트림 계약 이월(T2→T4/T5)**: post-Start 부모 취소는 `(Result, nil)`(killed exit code, TimedOut=false), pre-Start 취소는 ctx 오류 전파 → 반환 형태가 시점별로 상이. T4/T5는 취소/비정상종료 구분에 `ctx.Err()` 참조.
   - Windows 로컬에서 unix/linux 파일 미컴파일 → CI 3-OS로 실검증(T11 PR 게이트).
   - 최종 리뷰 triage 대상 Minor(원장): T1 심링크 가드 직접 테스트 없음, T2 post-Start 취소 반환형태·386 uintptr 트렁케이션(비대상).
   - 관찰 이월(session-28~29): D46 139MB 회수(사용자 창), empty GC 회수 관측, doctor [17] dirty=untracked 코스메틱.

6. **Standing protocols**: session-29 §6 그대로 + **신규 확정(session-30)**:
   - **보안 민감 태스크(exec/sandbox — T3·T5) 리뷰 체크포인트에서 Fable은 Codex 리뷰 원문을 직접 읽지 않는다.** Codex 리뷰는 백그라운드 파일로 두고, 서브 리뷰어+Codex 두 산출물의 **병합·판정을 opus 서브에이전트에 위임**(양 파일→개념 수준 병합 findings 파일+요약만 반환, 공격 벡터 열거 금지) → 컨트롤러는 개념 수준만 읽어 fix 파견. fix/재리뷰 서브에이전트는 opus라 원문 상세를 파일에서 읽어도 안전. 대안: 보안 민감 태스크만 Opus 유지, 문서·계측 태스크(T7~T10) Fable 복귀. (메모리 [[fable-security-prose-minimization]])
   - 유지: Fable + 보안 서술 최소화(커밋·리뷰 문면 개념 수준) / general-purpose 서브에이전트 model:"opus" 명시 / go test -p 1 / gofumpt 로컬 / `git add -A` 금지 / Codex 1패스/체크포인트(재리뷰 서브 단독) / 스펙-계획 충돌 시 스펙 승 / 페이지파일 재활성화됨(자식 OOM 시 커밋 차지 1차 점검).

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-30)을 읽고 재개. v0.11 SDD 실행 중 — 브랜치 `feat/v0.11-d58-d62`, **T1·T2 완료(chain 1223504..2bf20be), 다음 = T3(sandbox Unix)부터 BASE=2bf20be**. superpowers:subagent-driven-development 계속(`docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md`, 태스크별 fresh 서브에이전트 general-purpose는 model:"opus" 명시, 계획 각 태스크의 "검토 반영" 노트를 implementer에 필수 전달, BASE는 원장 기록). T3 신규 의존 go-landlock v0.9.0 1건 추가. **보안 민감 태스크(T3·T5) 리뷰 체크포인트에서는 Codex 리뷰 원문을 메인 컨텍스트로 직접 읽지 말 것 — 서브 리뷰어+Codex 병합·판정을 opus 서브에이전트에 위임하고 개념 수준 요약만 받아 fix 파견(session-30 세이프가드 인시던트 재발 방지, 메모리 참조)**. 태스크/통합/최종 리뷰는 서브 리뷰어+Codex review --base <BASE> 병렬 1패스(재리뷰 서브 단독). 다운스트림: T2 post-Start vs pre-Start 취소 반환형태 상이 → T4/T5에서 ctx.Err() 참조. go test -p 1·gofumpt 로컬·git add -A 금지. Windows 로컬 미컴파일 unix 파일은 CI 3-OS로 실검증(T11). **Fable 유지 + 보안 서술 최소화(벡터 열거 금지)**. ultrathink
