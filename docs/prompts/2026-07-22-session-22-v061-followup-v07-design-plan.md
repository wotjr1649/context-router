# Session 22 — v0.6.1 follow-up 웨이브 + v0.7 설계·계획 (2026-07-22~23)

1. **Session/span/model**: session-22, 2026-07-22~23, claude-fable-5
   (main 컨트롤러), 서브에이전트 opus(리뷰어·검수자), Codex
   review/adversarial-review(체크포인트당 1패스).

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-21)을 읽고 재개. v0.6.0 릴리스 완료 상태. 이번 세션 후보: (a) v0.6.x follow-up 소형 웨이브 — 캐리오버 ①~③ + 도그푸딩 관찰(D46 발화 여부·usage --compare 신호 재확인), 또는 (b) 다음 버전 브레인스토밍(§8 후보: D35 후속 Codex 가드 동등성 — cx: tool_call 실증 소재 준비됨). 사용자에게 축을 물어보고 시작. ultrathink

   사용자 선택: (a)→(b) 순차 + repo-root ctr.exe 지금 처리(확인 결과
   이미 삭제되어 있었음 — 잔여 수동 항목 종결). 중간 사용자 지시(2패스
   검수, 원문): "스펙을 코덱스와 서브에이전트로 적대적 검수, 검토,
   자문, 리뷰를 한뒤 문제 발견되면 fix혹은 브레인스토밍하여 사용자에게
   질문하고 문제 없으면 승인하여 superpowers:writing-plans로 구현 계획
   작성에 들어간다. ultrathink"

3. **What was done**:
   - **(a) v0.6.1 웨이브**: 관찰 — D46 미발화(file 92.5→94.3MB, 임계
     100MiB의 89.9%), usage --compare cc 신호 2점째 안정(cache_read/rec
     0.643·output/rec 0.866), empty 67→72 증가(7일 GC 창 미도래), drops
     325 불변, cx 파싱 skipped files=1. 캐리오버 ① loadCXSessions 행
     Scan 오류→incomplete 강등 + ② D46 역방향 축 단정(4dce53d), ③
     doctor 픽스처 점검(건전·무변경), 0.6.1 범프(ffc8cd3). 이중 리뷰:
     서브 Merge-Ready(0C/0I/1M) + **Codex P2 실증 1건**(부모 env 임계
     키 누수 시 신규 무경고 단정 거짓 실패 — 실행 재현) → 공유 픽스처
     양 축 env 고정 fix(9f01db1) → 재리뷰 Merge-Ready → PR #18 머지
     **28a5958**, tag **v0.6.1**, CI 3-OS GREEN, 도그푸딩 marker 0.6.1.
   - **(b) v0.7 브레인스토밍**(superpowers): 실측 마이닝 — cx:
     tool_call 148건 분해(Bash 48[Get-Content 22=46%]·MCP 95·
     update_plan 5 — v0.6 §7 "전부 Bash:" 교정), 도그푸딩 config.toml
     실물 ctr 부분열 0회. 사용자 결정 4건: 축=D35 후속 Codex 가드
     동등성 / deny 대체 경로=ctr MCP 재등록 포함(완전 동등성) /
     설치=hook install --codex가 MCP까지 자동 병합(TOML) / 편승=D46
     회수 자동화 **설계만 선행**. 접근 A(게이트 이식+관리 블록) 승인,
     설계 4섹션 승인.
   - **설계 `docs/context-router-design-v0.7-ko.md`(D47·D48·D49)**:
     초안 2b9c05c → 1패스 이중 검수 반영 585dcaa(수렴 2건 — 게이트
     직렬 폐기→**호스트×GOOS 단독 게이트**[게이트=술어+경로해석 완결
     정책, cat /c/… MSYS 오파일 반례], 충돌 스캔 광역화) → 재검수 SHIP
     0772e2a → **2패스(사용자 지시)** 이중 검수 반영 8526d49(마커
     소유권=정확 라인 매치+본문 검증[Codex critical], electron/spectra
     부분열 오탐→**키-경계 보수 스캔**, **D47↔D48 설치 결합**[MCP 확정
     시에만 PreToolUse 등록 — D32 위반 조합 봉쇄], G4 관측 4항목·순서
     고정 판정[payload 운반·자문-deny 불합격 추가]) → 재검수 SHIP +
     Minor 즉시 반영 a13d676(루트 완전-점표기 `.ctr.` 신호). G5는 설계
     시점 실코드로 종결(dispatch 호스트 무관 PreToolUse 분기 실재,
     구버전 창 deny 표면 ~0).
   - **계획 `docs/superpowers/plans/2026-07-23-v0.7-codex-guard-parity.md`**:
     0bd7e07 → 이중 검수 반영 63f7fe5(Step 6 실헬퍼 정합[runHookHost
     []byte·evalLong·CTR_HOOK_DEADLINE_MS], G4 행 3 분기 재기술[Task 1
     전체 제외+Task 3 withGuard 상시 false 축소], mergeCodexHooks
     테스트 7곳 파급 명시, CRLF 무개행 EOF·연속 빈 줄·왕복 oracle EOL
     보강) → 재검수 **SHIP**(게이트 9건·TOML 26케이스 손검산 통과).
     구성: Task 0 G4/G6 프리체크(컨트롤러 — 판정이 T1/T3 게이트) → T1
     가드 host 스레딩+guardDumpPath → T2 TOML 관리 블록 병합기 → T3
     설치 결합 → T4 0.7.0 범프.

4. **Repo state**: main **63f7fe5**(+이 기록 커밋), tag v0.6.1, 오픈
   PR·피처 브랜치 없음. 도그푸딩 marker 0.6.1, [14] blob 25.6MB·file
   94.3MB(임계 100MiB의 89.9% — D46 발화 임박), [6] sessions=99
   (empty=72), [12] drops=325.

5. **Carryovers**:
   - **v0.7 SDD 실행이 다음 세션 전부**: 계획 문서의 Task 0(G4/G6
     프리체크 — 컨트롤러, 판정 §10.3 추기)부터. G4 행 1/2/3에 따라
     T1/T3 분기(계획 Global Constraints). 실행 후 최종 이중 리뷰 → PR
     → CI → 머지 → tag v0.7.0 → 도그푸딩(`hook install --codex --user`
     설치 결합 실발화 확인 + Codex `/hooks` 재신뢰 수동 + config.toml
     블록 실물 확인).
   - 관찰: D46 발화 임박(89.9%) — 발화 시 D49 구현 착수 조건 충족(§0
     D49). empty=72 7일 GC 회수(≈07-27+) 관측. cx skipped files=1.
   - A/B 해석: v0.7 배포 후 cx: arm treatment 경계(세션 단위, Producer
     버전이 기계 경계) — usage --compare 해석 시 §5 주석 준수.

6. **Standing protocols**: session-21 §6 그대로(이중 검수=서브+Codex
   병렬 1패스·재검수는 서브만, SDD 파일 핸드오프, BASE는 원장, go test
   -p 1, §12 canary, byte-for-byte 게이트) + **계획 Task 0 게이트**
   (G4 판정이 후속 태스크 분기·컨트롤러 수행·서브 위임 금지).

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-22)을 읽고 재개. 상태: main 63f7fe5, tag v0.6.1, v0.7 스펙(docs/context-router-design-v0.7-ko.md, SHIP a13d676)·계획(docs/superpowers/plans/2026-07-23-v0.7-codex-guard-parity.md, SHIP 63f7fe5) 확정. 이번 세션: v0.7 SDD 실행 — 먼저 계획 Task 0(G4/G6 관측 프리체크, 컨트롤러 수행·§10.3 추기, 판정이 T1/T3을 게이트)을 수행하고, superpowers:subagent-driven-development로 T1~T4 실행(태스크당 프레시 구현자+리뷰어, BASE는 원장 기록). 완료 후 최종 이중 리뷰(서브+Codex review --base) → PR → CI 3-OS → 머지 → tag v0.7.0 → 도그푸딩 갱신(hook install --codex --user 설치 결합 실발화·config.toml 블록 확인·/hooks 재신뢰 안내). 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(사용자 확정 — 재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink
