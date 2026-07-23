# Session 26 — v0.9 관찰 웨이브·v0.10(D53–D57) 브레인스토밍~스펙~계획 (2026-07-23)

1. **Session/span/model**: session-26, 2026-07-23, claude-fable-5(main
   컨트롤러), 서브에이전트 opus(버전 중앙화 분석·스펙 적대 검수 1차/
   재검수·계획 검수) + claude-code-guide(훅 스키마)·haiku Explore
   (훅 실측용), Codex adversarial-review 3패스(스펙 1차·재검수·계획
   — 재검수/계획은 사용자 명시 지시로 1패스 가드 해제, 전부
   run_in_background).

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-25)을 읽고 재개. v0.9.0 릴리스 완료 상태(main 7279a48, tag v0.9.0, 도그푸딩 marker 0.9.0 — cc·codex(project+user) 재설치, doctor [16] 분기④ 정상, store 116.7MB D46 발화 중). 선결: 사용자에게 Codex /hooks 재신뢰(0.9.0) 완료 여부 확인 + 재신뢰 직후 config.toml `[mcp_servers.ctr]` 블록 생존 확인(§9-2 지속). 이번 세션 후보: (a) v0.9 관찰 웨이브 — D51 실효(교차 프로젝트 unknown-session 신규 발생 정지: 기준점 본 프로젝트 399·sufficode 981 / 합성 등록 세션·compare synthetic=N 실관측)·doctor [16] 지속·D46 잔존 회수(창은 사용자 결정)·empty GC(≈07-28+)·cx arm, (b) 다음 버전 브레인스토밍(v0.9 §4 후보 — A/B 하네스·exec 3종·synthetic 표식 영속화 등). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink

3. **What was done**:
   - **선결**: 재신뢰 완료(사용자 확인) + 블록 생존(line 515, §9-2
     지속). 재신뢰 직후 14:31 KST 양 프로젝트 cx session_start 동시
     등록 실관측.
   - **(a) 관찰 웨이브 — D51 실효 확정**: install mtime 14:24:01
     KST 기준 전 프로젝트 unknown-session 신규 0(본 399 불변·
     sufficode 981→1536(+555 전부 설치 전)에서 정지·ws/memory/
     matcher 불변). 합성 등록 0건(정상 — unknown 미발생), compare
     `synthetic=0` cc·cx 실표시. cc 5점째 일관(on26 0.851/0.658),
     cx n 정체. doctor [16] 분기④ 지속, [14] 116,740,096B 불변,
     empty 83→84(분포 07-20:8/21:46/22:18/23:12 — GC ≈07-27/28+),
     cx shadow +126KB.
   - **(b) 브레인스토밍(v0.10)**: 사용자 결정 — 축=서브에이전트
     캡처(생애주기→실측 후 표식 병기 승격)+Grep 가드+hook-only
     VACUUM 소급, **+사용자 추가 2건**: ① 버전 중앙 집중화(서브
     에이전트+웹 분석 → 사용자 지정 접근3: `internal/buildinfo`·
     `-dev` 스킴·VCS 진단 doctor 한정·CI 태그 검증) ② ctxscribe/
     context-mode 대비 방향성 설명 → **장기 방향 결정: 수렴 —
     ctr가 종착지, D21 exec=대체 게이트(D57)**. 근거 실측: ctr
     도구 8회 vs ctxscribe 422회.
   - **스펙 전 실측(관례 §3)**: settings.local.json 임시 덤프 훅 +
     Explore 1회 — **세션 중 훅 리로드 반영 실증**. SubagentStart/
     Stop 인풋 필드 확정(agent_id/agent_type·last_assistant_message
     ·agent_type="" 실존), **서브에이전트 내부 PostToolUse에
     agent_id 실림 확정**(표식 공짜), Grep head_limit 미지정=필드
     부재 확정. 실측 후 훅 제거.
   - **스펙**: `docs/context-router-design-v0.10-ko.md` 초안
     6b60d3c → 이중 적대 검수 1차(서브 opus P2×3/M다수 + Codex
     high1/med3 — denyTool reason 하드코딩·buildinfo 아키텍처 규약
     충돌·-dev 형상 미탐·empty/GC 술어 공유 등) 수렴 **d4d4851**
     → 재검수(사용자 지시 — 서브 P2×3/M1 + Codex med1, retention
     +7일 오류 **양 검수 독립 동시 발견**) 수렴 **6f6c883**(agent
     표식 RawMessage 기전·version 서브커맨드 신설·marker 경계 2개
     정정·수거 시점 정정).
   - **계획**: `docs/superpowers/plans/2026-07-23-v0.10-d53-d57.md`
     초안 721a641(7태스크) → 이중 검수(Codex high3/med3 + 서브 opus
     P1×2/P2×1/M다수 — attrs `map[string]any` 컴파일 차단·retention
     헬퍼 시그니처 전면 불일치 **양 검수 동시**·릴리스 범위 누락)
     수렴 **c23b7d1**: 8태스크 확정(T1 D53 생애주기·T2 표식+empty/
     GC·T3 D54 Grep 가드·T4 D55 VACUUM·T5 D56 buildinfo+version·
     T6 doctor[17]+CI+아키텍처 문서·T7 D57 vision·T8 릴리스
     체크포인트).

4. **Repo state**: main **c23b7d1**(+이 기록 커밋), tag v0.9.0
   불변, 오픈 PR·피처 브랜치 없음. 도그푸딩 marker 0.9.0 유지
   (§1.3 표준 절차 — dev 중 미설치, 훅은 0.9.0 릴리스본), store
   116.7MB(D46 발화 중), `.claude/settings.local.json`은 `{}`(실측
   훅 제거됨).

5. **Carryovers**:
   - **다음 세션 = v0.10 SDD 실행**: 계획 c23b7d1 8태스크,
     superpowers:subagent-driven-development, 브랜치
     feat/v0.10-d53-d57, BASE 원장. Task 8은 최종 이중 리뷰 후
     (사용자 cx 재신뢰 참여 지점 포함).
   - **관찰**: ① D46 116.7MB — 회수 창 사용자 결정 대기(D55가
     도구 보강) ② empty=84 GC — 첫 회수 ≈07-27·최대(46건)
     ≈07-28+ ③ cx arm n 정체·[16]·§9-2 지속 ④ D51 합성 경로
     실증은 중간 재신뢰류 엣지 발생 대기.
   - 스크래치 C:\tmp\ctr-g4 수동 삭제(사용자 — 이월).

6. **Standing protocols**: session-25 §6 그대로(이중 검수=서브+
   Codex 병렬·Codex는 체크포인트당 1패스가 기본이나 **사용자 명시
   지시로 재검수 가능**, Codex 포그라운드 타임아웃 킬 금지 —
   run_in_background, SDD 파일 핸드오프, BASE 원장, go test -p 1,
   §12 canary, byte-for-byte 게이트). 추가 교훈: ① 훅 인풋 실측은
   settings.local.json 임시 덤프 훅으로 세션 중 가능(리로드 반영
   실증 — 실측 후 제거 필수) ② 계획 내 Go 코드는 실코드 시그니처
   전수 대조 후 작성(attrs 타입·헬퍼 인자 — 이번 P1 2건의 교훈)
   ③ general-purpose 서브에이전트는 model:"opus" 명시(글로벌 §6).

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-26)을 읽고 재개. v0.10 스펙(docs/context-router-design-v0.10-ko.md, 6f6c883)·구현 계획(docs/superpowers/plans/2026-07-23-v0.10-d53-d57.md, c23b7d1, 8태스크) 이중 검수 완료 상태 — main은 session-26 기록 커밋, 도그푸딩 marker 0.9.0 유지(dev 중 미설치가 표준 절차). superpowers:subagent-driven-development로 계획 실행: 브랜치 feat/v0.10-d53-d57 생성, BASE 원장 기록, 태스크별 fresh 서브에이전트(general-purpose는 model "opus" 명시) + 태스크 리뷰(스펙+품질, fix→재리뷰), Task 1~7 후 최종 검증·최종 이중 리뷰(서브 whole-branch + Codex review --base <BASE> 병렬 1패스) → Task 8 릴리스 체크포인트(범프 0.10.0·PR·CI·tag-version 잡 확인·도그푸딩 설치·사용자 cx 재신뢰·실관측). 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink
