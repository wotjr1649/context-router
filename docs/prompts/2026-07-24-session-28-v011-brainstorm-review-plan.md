# Session 28 — v0.11(D58–D62) 브레인스토밍·이중 적대 검수·구현 계획 (2026-07-24)

1. **Session/span/model**: session-28, 2026-07-24, claude-fable-5(main
   컨트롤러). 서브에이전트: 적대 리뷰어 1(opus, 정적 코드 대조).
   Codex adversarial-review 1패스(스펙 대상, run_in_background).
   구현은 미착수 — 이 세션은 스펙→검수→계획까지만.

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-27)을 읽고 재개. v0.10.0 릴리스 완료 상태(main 7cbcaa5, tag v0.10.0, 도그푸딩 marker 0.10.0 — cc 6이벤트·codex project+user, cx 재신뢰·§9-2 블록 생존 확인). 이번 세션 후보: (a) v0.10 관찰 웨이브 — subagent_start/stop 누적·Grep 가드 실발화·D46 116.7MB 회수(D55 hook-only VACUUM 창은 사용자 결정)·empty GC(≈07-27/28+, D53 제외 반영)·doctor [17]/[16] 지속, (b) v0.11 브레인스토밍(D57 로드맵 — exec 3종 D21 트랙 격상·회수 경로 채택 개선). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 활성화 시켰다. Fable 유지, 보안 서술 최소화 준수. ultrathink

3. **What was done**:
   - **축 결정(사용자)**: (a)→(b) — 관찰 웨이브 실측 후 그 데이터를
     근거로 v0.11 브레인스토밍. 관찰(§3 상세): doctor [16] codex
     user 스코프 스테일 해소(양 스코프 0.10.0), **D54 Grep 가드
     라이브 파이어 최초 실증**(matcher 실기록 + head_limit=0 deny →
     warning 이벤트 `Grep: head_limit=0 content` 실기록), D53
     생애주기 누적 1쌍, empty 91건(GC 첫 적격 07-27·피크 07-28),
     D46 139.2MB(+22.5MB/일), C:\tmp\ctr-g4 삭제 확인. adoption
     베이스라인 ctr 9 vs ctxscribe 618 ≈ 1.5%.
   - **브레인스토밍(사용자 결정 11건)**: 축=exec 주력+채택 개선
     병행 / 표면=2종(execute·execute_file, batch 미도입 — 실측
     5.2%) / 언어=6종(shell·js·ts·py·go·**csharp**, SDK 10.0.301
     실확인) / 접근=A안 단일 릴리스 / timeout 상한 1800s / TS=bun→
     node(≥22.7 transform 조립) / C# in-job env 3종(빌드 상주 서버
     비활성) / 노출·cwd·캐시는 검수 후 재결정(아래).
   - **정본 스펙 작성**: `docs/context-router-design-v0.11-ko.md`
     (D58~D62, delta chain 관례). 커밋 c265f89.
   - **이중 적대 검수(프로토콜)**: 서브 리뷰어(opus, 코드 대조
     7건 — §3.5 가드·프로필 구조·직렬화 상한·usage 구조·doctor
     번호·shadow recall MCP 적용·아키텍처 정합) + Codex
     adversarial-review 병렬. 판정 서브 P1 0·P2 2·P3 5 / Codex
     needs-attention. **수용 10건·기각 1건**(더 강한 기본 격리
     요구는 능력 상한이 native Bash·ctxscribe 동등이라 §4 이월).
     사용자 재결정 3건: ⑨노출=exec 프로필 opt-in(`--enable exec`)+
     fail-closed 프로브 ⑩cwd=스크래치+CTR_WORKTREE env ⑪러너 캐시=
     스크래치 재지정. 반영 커밋 f3ae92d, §5 검수 기록 신설.
   - **Fable 세이프가드 재발·복구**: 검수 산출물을 §5·D59로 옮겨
     적는 과정에서 서술 밀도 임계 초과로 `model_refusal_fallback`
     발동. 실질 계약 불변·서술만 개념 수준 추상화로 복구(커밋
     a752eab). 메모리 갱신([[fable-security-prose-minimization]]).
   - **구현 계획**: `docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md`
     (11태스크 TDD, go-landlock v0.9.0 사전승인 의존, per-OS build
     tag, 셀프리뷰 3항 통과). 커밋 2104e10.

4. **Repo state**: main **2104e10**, tag **v0.10.0**(=7cbcaa5,
   기릴리스). 오픈 PR·피처 브랜치 없음. v0.11 커밋 4개(c265f89 스펙·
   f3ae92d 검수반영·a752eab 서술추상화·2104e10 계획) — **전부 문서,
   코드 미착수**. 도그푸딩 marker 0.10.0 유지(dev 중 미설치 표준).

5. **Carryovers**:
   - **다음 세션 = 계획·스펙 검토(Fable)**. 그 검토 통과 후 **그
     다음 세션에서 SDD 실행**(사용자 지시 2026-07-24). 이 세션에서
     바로 실행 안 함.
   - 검토 관점 예시: 11태스크의 실코드 시그니처 정합(sandbox.Spec/
     Result·exec.Request/Response·mcp.ExecuteInput/Output 일관),
     러너 캐시 재지정 완전성(go/dotnet/python 필수 쓰기 경로 누락
     없나), env 닫힌 표가 러너 실행성과 양립하나, C# `dotnet run
     file.cs` file-based apps 계약 SDK10 정합, 통합테스트 하네스가
     landlock 자기재실행을 무한재귀 없이 검증하나, doctor [18]·
     usage --adoption 헬퍼 재사용 지점의 실존.
   - **관찰 이월**: ① D46 139.2MB 회수 — 사용자 수동 창(전 세션
     종료 후 `purge --hook-only --project context-router-4d307dacd75b`,
     139→≈104MB 기대) ② empty GC 첫 회수 07-27·피크 07-28 ③ D51
     합성 경로·cx arm n 정체 지속 관찰 ④ doctor [17] dirty=untracked
     코스메틱.
   - **v0.12+ 후보**(스펙 §4): ctxscribe 브리지 종료 절차(adoption
     추세 확보 후)·격리 강화(AppContainer/seccomp)·batch 재도입·
     웜 캐시 허용·bun CI.

6. **Standing protocols**: session-27 §6 그대로 + 추가:
   - **Fable 세이프가드**: 적대 검수 산출물을 문서로 반영할 때가
     최고위험 트리거 — 결정·처분만 개념 수준으로 적고 벡터 열거는
     압축(재나열 금지). exec/sandbox 설계가 특히 취약
     ([[fable-security-prose-minimization]]).
   - **페이지파일 재활성화**(2026-07-24 — 비활성 기간 종료).
     자식 OOM 시 커밋 차지 확인은 여전히 1차 점검, test-guard 캡은
     계속 준수([[no-pagefile-commit-exhaustion]]).
   - 유지: gofumpt 로컬 확인 필수·gh pr checks 전체 출력·Codex
     companion run_in_background·계획 내 Go 시그니처 전수 대조·
     Codex 재검수 1패스 가드·실 purge류 사용자 창·`git add -A` 금지.

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-28)을 읽고 재개. v0.11은 스펙·계획까지 커밋 완료·코드 미착수 상태(main 2104e10). 이번 세션 목표 = **SDD 실행 전 계획·스펙 검토(Fable)**. 대상: 정본 스펙 `docs/context-router-design-v0.11-ko.md`(D58~D62)와 계획 `docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md`(11태스크). 검토 관점: ① 11태스크의 Go 타입·시그니처 정합을 실코드(internal/mcp·transform·cli·cmd)와 전수 대조 ② 러너 캐시 스크래치 재지정 완전성(go GOCACHE/GOMODCACHE·dotnet NUGET_PACKAGES/DOTNET_CLI_HOME·python PYTHONPYCACHEPREFIX 누락 여부) ③ env 닫힌 표가 6러너 실행성과 양립하나 ④ landlock 자기재실행(__exec-launcher) 통합테스트가 무한재귀 없이 실증 가능한가 ⑤ doctor [18]·usage --adoption의 헬퍼 재사용 지점 실존 확인 ⑥ D58~D62·§5 검수 수용 10건이 태스크에 빠짐없이 반영됐나. 발견 시 스펙/계획 fix 또는 사용자에게 질문, 문제 없으면 검토 완료 보고 후 종료(SDD 실행은 그다음 세션). 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 재활성화됨. **Fable 유지 + 보안 서술 최소화 엄수(검수 벡터 재나열이 세이프가드 재발 원인 — 개념 수준 추상화)**. ultrathink
