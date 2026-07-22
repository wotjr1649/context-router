# Session 23 — v0.7 SDD 실행·릴리스 (2026-07-23)

1. **Session/span/model**: session-23, 2026-07-23, claude-fable-5(main
   컨트롤러), 서브에이전트 opus(구현자 4·태스크 리뷰어 4·최종
   리뷰어·fix·재리뷰어), Codex review 1패스.

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-22)을 읽고 재개. 상태: main 63f7fe5, tag v0.6.1, v0.7 스펙(docs/context-router-design-v0.7-ko.md, SHIP a13d676)·계획(docs/superpowers/plans/2026-07-23-v0.7-codex-guard-parity.md, SHIP 63f7fe5) 확정. 이번 세션: v0.7 SDD 실행 — 먼저 계획 Task 0(G4/G6 관측 프리체크, 컨트롤러 수행·§10.3 추기, 판정이 T1/T3을 게이트)을 수행하고, superpowers:subagent-driven-development로 T1~T4 실행(태스크당 프레시 구현자+리뷰어, BASE는 원장 기록). 완료 후 최종 이중 리뷰(서브+Codex review --base) → PR → CI 3-OS → 머지 → tag v0.7.0 → 도그푸딩 갱신(hook install --codex --user 설치 결합 실발화·config.toml 블록 확인·/hooks 재신뢰 안내). 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(사용자 확정 — 재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink

3. **What was done**:
   - **Task 0(컨트롤러, 645b87a)**: G4 실증 — 공식 문서 + 실호스트
     스크래치(격리 CODEX_HOME=C:\tmp\ctr-g4, codex-cli 0.144.6,
     `--dangerously-bypass-hook-trust`). ① hooks.json 등록 수용 ②
     payload tool_name="Bash"·tool_input.command **raw PS 구문** 운반
     ③ sandbox 해제 baseline(파일 생성 성공) 대비 deny JSON만으로
     실행 차단(부작용 파일 미생성 — 자문-deny 아님) ④ Claude 동형
     permissionDecision 수용 → **행 1**(T1 Step 8 비활성, 원안 진행).
     G6: 실물 config.toml LF 지배·마커 0·ctr 신호 0 → append 예측.
     §10.3 추기.
   - **T1~T4 SDD**(브랜치 feat/v0.7-codex-guard-parity, BASE 0632630):
     T1 6ef9a8a(D47 host 스레딩+guardDumpPath, 계약 핀 ② 포함 9케이스
     +통합 3종) / T2 e71f33f(D48 병합기, 32케이스 — 리뷰어 전량 바이트
     손추적) / T3 2be3932(설치 결합, 파급 9곳) / T4 4a002bc(0.7.0
     범프, 전체 회귀 11/11). 태스크 리뷰 전부 Approved(fix 라운드 0).
   - **최종 이중 리뷰**: 서브(opus) With-fixes C0/I1/M3 + Codex
     P1×3/P2×2 병합 판정 → **P1-1 채택**(루트 인라인
     `mcp_servers={…}`가 §3(b) AND 조건 회피 → append 시 인라인
     테이블 확장 불가로 **파스 에러** — tomllib 실검증, 계약 자체 갭
     → 프리픽스 단독 충돌 신호 개정), P1-2=서브 I **수렴**(스테일
     가드 — 해법 상충으로 §9-1 양론 기록·코드 무변경·**사용자 결정
     이월**), P1-3 부분 채택(전역 블록 수명 — uninstall 힌트+§9-2),
     P2-1 채택(hooks.json 부재 시 config 정리 지속), P2-2 문서화
     (§9-3), 서브 M 채택(doctor 주석 6도구). fix 웨이브 6dc92ef +
     문서 45d73af → 재리뷰(서브 단독) 승인.
   - **릴리스**: PR #19 → CI 3-OS 양 런 GREEN → 머지 **0be65fe**
     (+875/−68, 10파일) → tag **v0.7.0** 푸시.
   - **도그푸딩**: go install + hook install(cc: 4이벤트, marker
     0.7.0) + `hook install --codex --user` 설치 결합 **실발화** —
     config.toml "기입 완료"(mcpWritten, L509 append·LF·6도구 —
     G6 예측 그대로) + "3개 이벤트"(캐프처 2+PreToolUse 가드 1).
     스크래치 auth 사본 무해화(빈 JSON 덮어쓰기).

4. **Repo state**: main **0be65fe**(+이 기록 커밋), tag v0.7.0, 오픈
   PR·피처 브랜치 없음(로컬·리모트 삭제). 도그푸딩 marker 0.7.0,
   Codex user hooks.json에 PreToolUse(Bash) 등록, config.toml 관리
   블록 실물 기입.

5. **Carryovers**:
   - **사용자 수동 1건(필수)**: Codex `/hooks`에서 신규 PreToolUse
     훅 **재신뢰**(훅 정의 해시 변경 — 신뢰 전까지 스킵됨).
   - **사용자 결정 1건**: 스테일 가드(§9-1) — 문서화 유지 vs
     자가치유(미확정 재설치 시 가드 제거). 서브=문서화(conflict
     대부분 유효 MCP라 가드 잔존 정상), Codex=자가치유(블록 삭제
     후 재설치는 MCP 실부재). 현행=문서화.
   - **관찰**: cx: 가드 실발화 관측(다음 도그푸딩 창), D46 발화
     임박(89.9% — 발화 시 D49 착수 조건), empty 세션 7일 GC(≈07-27+),
     A/B: cx: arm treatment 경계=Producer 0.7.0 세션부터(§5 주석).
   - 소형 후보(v0.7.x): uninstall no-file 테스트 CODEX_HOME 격리
     하드닝, cx deny 테스트 session_id cx:% 필터, 스크래치
     C:\tmp\ctr-g4 수동 삭제(자동 삭제 가드로 미수행).
   - **다음 버전 후보**: §9 목록(D49 대기, doctor Codex MCP 상태
     검사, exec 3종 등) — 브레인스토밍으로 축 결정.

6. **Standing protocols**: session-22 §6 그대로(이중 검수=서브+Codex
   병렬 1패스·재검수 서브만, SDD 파일 핸드오프, BASE 원장, go test
   -p 1, §12 canary, byte-for-byte 게이트). 추가 관례: G4류 실호스트
   실증은 격리 CODEX_HOME+trust 우회 플래그, 실증 아티팩트 스크래치.

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-23)을 읽고 재개. v0.7.0 릴리스 완료 상태(main 0be65fe, tag v0.7.0, 도그푸딩 marker 0.7.0·Codex 가드+MCP 블록 설치됨). 선결: 사용자에게 Codex /hooks 재신뢰 완료 여부 확인. 이번 세션 후보: (a) v0.7 관찰 웨이브 — cx: 가드 실발화·usage --compare cx arm 신호·D46 발화 여부(발화 시 D49 착수)·empty GC 회수 확인 + 소형 캐리오버(uninstall 테스트 격리·cx deny 필터), (b) 스테일 가드 §9-1 사용자 결정(문서화 유지 vs 자가치유) 반영, (c) 다음 버전 브레인스토밍(§9 후보). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink
