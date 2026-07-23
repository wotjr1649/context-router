# Session 27 — v0.10(D53–D57) SDD 실행·릴리스 v0.10.0 (2026-07-23)

1. **Session/span/model**: session-27, 2026-07-23, claude-fable-5(main
   컨트롤러), 서브에이전트 opus(구현자 7·태스크 리뷰어 7·최종
   whole-branch·fix 웨이브·재리뷰) + haiku Explore(D53 실관측용 미니
   spawn), Codex review 1패스(`--base 22a2535`, run_in_background).

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-26)을 읽고 재개. v0.10 스펙(docs/context-router-design-v0.10-ko.md, 6f6c883)·구현 계획(docs/superpowers/plans/2026-07-23-v0.10-d53-d57.md, c23b7d1, 8태스크) 이중 검수 완료 상태 — main은 session-26 기록 커밋, 도그푸딩 marker 0.9.0 유지(dev 중 미설치가 표준 절차). superpowers:subagent-driven-development로 계획 실행: 브랜치 feat/v0.10-d53-d57 생성, BASE 원장 기록, 태스크별 fresh 서브에이전트(general-purpose는 model "opus" 명시) + 태스크 리뷰(스펙+품질, fix→재리뷰), Task 1~7 후 최종 검증·최종 이중 리뷰(서브 whole-branch + Codex review --base <BASE> 병렬 1패스) → Task 8 릴리스 체크포인트(범프 0.10.0·PR·CI·tag-version 잡 확인·도그푸딩 설치·사용자 cx 재신뢰·실관측). 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. Fable 유지, 보안 서술 최소화 준수. ultrathink

3. **What was done**:
   - **SDD T1~T7**(BASE 22a2535, branch feat/v0.10-d53-d57): T1 D53
     생애주기 9f32ba8 / T2 표식+empty·GC 핀 9d92c9b / T3 D54 Grep
     가드 77c1fdb / T4 D55 VACUUM 소급 c472520 / T5 D56 buildinfo
     6614394 / T6 doctor[17]+CI+아키텍처 0840dba / T7 D57 vision
     40954a4. **태스크 리뷰 7회 전건 spec ✅/Approved, fix 라운드 0**.
   - **최종 검증**: build·vet·`go test -p 1 ./...` GREEN, 잔존
     "0.9.0" 비테스트 0, §1.1 D53~D57 커밋 전수.
   - **최종 이중 리뷰**: 서브 whole-branch **Yes**(C0/I0/M3 수용) +
     Codex **P2×2 — 둘 다 채택·RED로 실버그 실증**: ① version 명령
     store-root 해석 선행 실패(양 검수 수렴 발견) → dispatchCLI 조기
     디스패치 ② 초대형 agent 표식 → MaxAttributesBytes 초과 →
     **기본 이벤트 유실**(D53 best-effort 계약 위반) → agentStrings
     256B 캡(단일 지점, 생애주기·표식 공동 보호) + 스펙 §0 1문장
     추기. fix 웨이브 1회 cf47236(+FourItems→SixItems 리네임) →
     재리뷰(서브 단독) 승인.
   - **T8 릴리스**: 범프 03070e1 → PR #22 → CI 중 **lint 인시던트**
     (fix 웨이브의 hook_test.go 주석 정렬 공백 2건 — gofumpt 게이트
     CI 전용이라 로컬 미탐, 테스트·빌드 무관) → style 7cbcaa5 main
     직push + **tag v0.10.0 재지정**(a766b44→7cbcaa5, 머지 수분 내
     솔로 저장소) → CI 전체 GREEN + **tag-version 잡 success = D56
     실태그 검증 첫 실관측**. 피처 브랜치 정리(로컬·원격 삭제).
   - **도그푸딩·실관측(§1.1 전 항목)**: version=0.10.0, cc
     **6이벤트**(marker 0.10.0), Grep matcher
     `Read|Bash|PowerShell|Grep` 실물, doctor [17] build 라인(dirty=
     untracked .claude/.codex 탓 코스메틱 — marker 비교 비관여 실증),
     codex **project+user 0.10.0**(user 스코프는 `--user` 플래그
     별도 — [16]이 스테일 0.9.0 정확 검출), **subagent_start/stop
     실기록**(Explore 쌍 019f8eec-8741/-9ea6 — 설치 직후 **세션 중
     훅 리로드 실증**), 사용자 cx /hooks 재신뢰(0.10.0) 후
     `[mcp_servers.ctr]` 블록 생존(line 518, §9-2 지속).

4. **Repo state**: main **7cbcaa5**, tag **v0.10.0**(=7cbcaa5), 오픈
   PR·피처 브랜치 없음. 도그푸딩 marker 0.10.0(cc 6이벤트·codex
   project+user), cx 재신뢰 완료.

5. **Carryovers**:
   - **관찰**: ① D46 store 116.7MB — 회수 창 사용자 결정 대기(이제
     D55 hook-only VACUUM 도구 사용 가능) ② empty GC 첫 회수
     ≈07-27·최대 ≈07-28+(D53 생애주기 세션은 empty 제외 반영됨)
     ③ D51 합성 경로 엣지·cx arm n 정체 지속 관찰 ④ doctor [17]
     dirty — untracked 탓, 클린 트리 go install 시 소멸(코스메틱).
   - **v0.11 후보**: D57 로드맵 — exec 3종(D21 트랙) 주력 격상,
     회수 경로 채택 개선(ctr_search/ctr_fetch 사용 유도).
   - 스크래치 C:\tmp\ctr-g4 수동 삭제(사용자 — 이월).

6. **Standing protocols**: session-26 §6 그대로. 추가 교훈: ①
   **gofumpt 게이트는 CI 전용** — Go 파일 커밋 전 `gofumpt -l .`
   로컬 확인(fix 웨이브 파견에도 명시), `gh pr checks` 워치 출력은
   전체 확인(tail 잘림이 lint 실패를 가림 — 이번 인시던트 원인) ②
   codex 훅 user 스코프는 `hook install --codex --user` 별도 실행
   ③ 태그 재지정은 솔로 저장소·머지 직후 한정 ④ D53 훅은 세션 중
   리로드 반영(재기동 불요) — 실관측으로 재확증.

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-27)을 읽고 재개. v0.10.0 릴리스 완료 상태(main 7cbcaa5, tag v0.10.0, 도그푸딩 marker 0.10.0 — cc 6이벤트·codex project+user, cx 재신뢰·§9-2 블록 생존 확인). 이번 세션 후보: (a) v0.10 관찰 웨이브 — subagent_start/stop 누적·Grep 가드 실발화·D46 116.7MB 회수(D55 hook-only VACUUM 창은 사용자 결정)·empty GC(≈07-27/28+, D53 제외 반영)·doctor [17]/[16] 지속, (b) v0.11 브레인스토밍(D57 로드맵 — exec 3종 D21 트랙 격상·회수 경로 채택 개선). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink
