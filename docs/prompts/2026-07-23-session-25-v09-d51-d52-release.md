# Session 25 — v0.8 관찰 웨이브·v0.9(D51·D52) 브레인스토밍~릴리스 (2026-07-23)

1. **Session/span/model**: session-25, 2026-07-23, claude-fable-5(main
   컨트롤러), 서브에이전트 opus(스펙 적대 검수·구현자 T1~T3·태스크
   리뷰어 T1~T3·최종 whole-branch) + sonnet(T3 fix·재리뷰·T4
   구현·리뷰), Codex adversarial-review 1 + review 1(체크포인트별
   1패스 — 1차 adversarial 패스는 컨트롤러 5분 타임아웃 킬로
   고아화, 동일 초점 재기동으로 완수).

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-24)을 읽고 재개. v0.8.0 릴리스 완료 상태(main 5df2909, tag v0.8.0, 도그푸딩 marker 0.8.0, store 109.2MB — D46 경고 발화 중). 선결: 사용자에게 Codex /hooks 재신뢰 완료 여부 확인 + 재신뢰 직후 config.toml `[mcp_servers.ctr]` 블록 생존 확인(§9-2 실사례 원인 추적). 이번 세션 후보: (a) v0.8 관찰 웨이브 — cx: 가드 실발화·D46 잔존 회수(24h 창은 사용자 결정)·empty GC 회수(≈07-27+)·cx arm 신호, (b) 다음 버전 브레인스토밍(v0.8 §4 후보 — A/B 하네스·exec 3종·D43·doctor Codex MCP 검사 등). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink

3. **What was done**:
   - **선결**: 재신뢰 완료 + **재신뢰 직후 블록 생존 사용자 직접
     확인** — §9-2 소멸 원인에서 재신뢰 배제 확정.
   - **(a) 관찰 웨이브**: cx: 가드 실발화 0건 불변(warning 2건 =
     기존 cc: 스모크뿐). D46 잔존 발화 109.0MB(회수는 사용자
     결정으로 관찰만 — 세션 말 116.7MB로 증가). empty 74→79→83.
     cx arm --compare 불변(on 5세션·10턴, 0.270/0.196/2.249 — n
     정체), cc 4점째 일관(on25/off19, 0.862/0.658). shadow
     cx:=973,941B(+186KB — 재신뢰 후 cx 캡처 실동작). **핵심
     발견**: unknown-session drop 재발·확산 — 본 프로젝트
     324→329(세션 말 399), 교차 프로젝트 sufficode **981건**(07-22
     ~23 3시간 창 진행 중·43세션 전부 session_start 단독), ws 23·
     memory 6·matcher 5, 합산 ≈1,344건. D43 5필드가 판정 실증.
   - **(b) 브레인스토밍**: 사용자 결정 3건 — 축=**D51(D43 재상정
     채택: register-on-first-event)+D52(doctor Codex MCP 검사)**,
     doctor drops 전 프로젝트 집계 비범위, D46 이번 세션 관찰만.
   - **스펙**: `docs/context-router-design-v0.9-ko.md` 초안 9a903cc
     → 이중 적대 검수(서브 opus P1×2/P2×5/M×3 — D52 상태기계
     혼동·marker 스캐너 부재 등 + Codex high2/med1 — 생애주기·
     원자성 문면·A/B 오염) → 수렴 3건 채택·기각 2건(cap/TTL·원자
     결합) → 개정 a475ea8 → 사용자 승인(compare synthetic 제외
     승격 수용). §3 sufficode "session.db 부재" 서술은 다중
     worktree export 오독으로 판명·교정(컨트롤러 재검증).
   - **계획 520b134**(T1~T4) → **SDD**(브랜치 feat/v0.9-d51-d52,
     BASE 520b134): T1 **2a57040**(hook.go dispatch 미지 세션
     합성 등록·테스트 반전 3건·D43 포맷 핀 appendDrop 이관·E2E
     정당 편입) / T2 **9805e35**(loadCCSynthetic·loadCXSessions
     3반환·scanRollouts 선차 제외·synthetic=N) / T3 **793339f**+fix
     **2104e0c**(probeCodexMCPBlock·scanCodexRegisteredHooks·doctor
     [16] 5분기 — Important: probe 우선순위 발산 셀 false-healthy
     → hasHeader>conflict>classReplace 정확 미러 교정) / T4
     **1e9bd3b**(0.9.0 범프 2지점·전 11패키지 ok). 태스크 리뷰
     4회 Approved(fix 1라운드).
   - **최종 이중 리뷰**: 서브 whole-branch **Yes**(Crit/Imp 0 —
     §2 전수 대조 누락 0·비범위 침범 0·마커 3지점 일치, Minor
     전건 이월 수용) + Codex review **P2 1건**(synthetic 표식이
     retention 스윕에 소실 — Sweep event_type 무면제 실검증,
     문서화 채택 **1bfb6e6**·표식 영속화 §4 조건부 이월).
   - **릴리스**: PR #21 → CI(windows 1잡 플레이크
     TestE2E_TwoProcessSessionLease — 느린 러너 9m6s, 병행 run
     pass, 재실행 후 전 GREEN) → 머지 **7279a48**(+637/−136,
     16파일) → tag **v0.9.0**.
   - **도그푸딩**: go install + 훅 재설치(cc 4이벤트 marker
     0.9.0·codex project 3이벤트). **doctor [16] 첫 실관측이 즉시
     가치 증명**: user 레벨 hooks.json 스테일 0.8.0 탐지 →
     `hook install --codex --user` 재기입 → 분기④ 정상(양 레벨
     0.9.0·블록=존재). drops 기준점 399(설치 시점).

4. **Repo state**: main **7279a48**(+이 기록 커밋), tag v0.9.0,
   오픈 PR·피처 브랜치 없음(로컬·리모트 삭제). 도그푸딩 marker
   0.9.0(cc·codex project+user), store 116.7MB(D46 경고 발화 중),
   [16] 분기④ 정상.

5. **Carryovers**:
   - **사용자 수동 1건(필수)**: Codex `/hooks` **재신뢰**(marker
     0.8.0→0.9.0, project+user 양 레벨 — 신뢰 전까지 cx 훅 스킵
     = D51 cx 축 실효도 재신뢰 후 시작).
   - **관찰**: ① **D51 실효** — 재신뢰 후 교차 프로젝트
     unknown-session 신규 발생 정지(기준점: 본 프로젝트 399·
     sufficode 981) + 합성 등록 세션(source=first-event)·compare
     `synthetic=N` 실관측 ② 재신뢰 전후 블록 생존(§9-2 지속) ③
     D46 잔존 116.7MB — 회수 창은 사용자 결정 ④ empty=83 GC(7일
     게이트 — 07-21 등록분 ≈07-28+) ⑤ cx arm n·[16] 지속.
   - **v0.10+ 후보**: v0.9 §4 — A/B 하네스·OTel(D27), exec 3종
     (D21), 서브에이전트 캡처, Grep 가드, synthetic 표식 영속화
     (오분류 실증 시), doctor drops 전 프로젝트 집계(재증가 실증
     시), hook-only VACUUM 소급, sweep 자동화 등.
   - 스크래치 C:\tmp\ctr-g4 수동 삭제(사용자 — 이월).

6. **Standing protocols**: session-24 §6 그대로(이중 검수=서브+
   Codex 병렬 1패스·재검수 서브만, SDD 파일 핸드오프, BASE 원장,
   go test -p 1, §12 canary, byte-for-byte 게이트, 실 store purge
   사용자 창 선택). 추가 교훈: **Codex companion 포그라운드 호출에
   타임아웃 킬 금지**(드라이버 고아화 — run_in_background로 실행,
   Git Bash의 taskkill /PID 경로 변환 함정도 회피: cancel류는
   PowerShell로).

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-25)을 읽고 재개. v0.9.0 릴리스 완료 상태(main 7279a48, tag v0.9.0, 도그푸딩 marker 0.9.0 — cc·codex(project+user) 재설치, doctor [16] 분기④ 정상, store 116.7MB D46 발화 중). 선결: 사용자에게 Codex /hooks 재신뢰(0.9.0) 완료 여부 확인 + 재신뢰 직후 config.toml `[mcp_servers.ctr]` 블록 생존 확인(§9-2 지속). 이번 세션 후보: (a) v0.9 관찰 웨이브 — D51 실효(교차 프로젝트 unknown-session 신규 발생 정지: 기준점 본 프로젝트 399·sufficode 981 / 합성 등록 세션·compare synthetic=N 실관측)·doctor [16] 지속·D46 잔존 회수(창은 사용자 결정)·empty GC(≈07-28+)·cx arm, (b) 다음 버전 브레인스토밍(v0.9 §4 후보 — A/B 하네스·exec 3종·synthetic 표식 영속화 등). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink
