# Session 24 — v0.7 관찰 웨이브·§9-1 확정·v0.8 D49 구현 릴리스 (2026-07-23)

1. **Session/span/model**: session-24, 2026-07-23, claude-fable-5(main
   컨트롤러), 서브에이전트 opus(구현자 4·태스크 리뷰어 4·최종
   리뷰어·fix·재리뷰어·설계 적대 검수), Codex adversarial-review 1 +
   review 1(체크포인트별 1패스).

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-23)을 읽고 재개. v0.7.0 릴리스 완료 상태(main 0be65fe, tag v0.7.0, 도그푸딩 marker 0.7.0·Codex 가드+MCP 블록 설치됨). 선결: 사용자에게 Codex /hooks 재신뢰 완료 여부 확인. 이번 세션 후보: (a) v0.7 관찰 웨이브 — cx: 가드 실발화·usage --compare cx arm 신호·D46 발화 여부(발화 시 D49 착수)·empty GC 회수 확인 + 소형 캐리오버(uninstall 테스트 격리·cx deny 필터), (b) 스테일 가드 §9-1 사용자 결정(문서화 유지 vs 자가치유) 반영, (c) 다음 버전 브레인스토밍(§9 후보). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink

3. **What was done**:
   - **(a) 관찰 웨이브**: 재신뢰 완료 확인(사용자). D46 99.90%
     (세션 시작)→literal 발화 대기. cx: 가드 실발화 0건(warning은
     cc: 스모크 2건뿐), 캡처 축은 cx:=768KB 실동작. usage --compare
     cx arm on 5세션·10턴(0.270/0.196/2.249 — 파싱 스킵 경고·판단
     유보), cc: 3점째 일관(0.873/0.645). empty=74(GC ≈07-27+ 미도래),
     drops=325 불변(기지). 소형 캐리오버 2건 처리 **e4fc70d**(cli
     TestMain CODEX_HOME 전역 중화 — install/uninstall e2e 3건이
     상속 CODEX_HOME으로 실사용자 config.toml 변조 가능했던 격리
     우회 봉쇄 / TestGuardCodexBashDeny cx:% 세션 필터).
   - **(b) §9-1 확정**: 사용자 결정=문서화 유지(자가치유 비채택,
     실사고 시 재상정) — 설계 반영 **52ce41b**.
   - **(c) v0.8 브레인스토밍**: 축=D49 회수 단독, --vacuum 수식어
     전용(사용자 결정 2건). 스펙 초안 acf09ff → **이중 적대 검수**
     (서브 opus P1×3/P2×2 + Codex needs-attention high2/med2):
     수렴 3건(라이브 감지 2단 폐기→사후 wal_checkpoint(TRUNCATE)
     busy 검증·WAL 오측→db+wal+shm 총합 실측(Codex 실험: reader
     공존 VACUUM 성공+main 불변+WAL 팽창·checkpoint Exec nil인데
     busy=1)·hook-only 기왕 무조건 VACUUM) + 서브 P1-B(gcOnly
     read-only+행 미삭제 → 조합 규칙 --older-than 단일 결합 축소)
     + Codex --all 집계 exit 채택, lifecycle lock 비채택. 개정
     **200a502**(§5 처리 기록), 사용자 스펙 승인.
   - **계획 858ad5a**(T1~T4, writing-plans) → **SDD 실행**(브랜치
     feat/v0.8-d49-vacuum, BASE 858ad5a): T1 2735640(store.
     CheckpointTruncate QueryRow busy 검증+IsBusyErr/IsDiskErr) /
     T2 d82dc08(정적 검증 — --older-than 결합 전용·최우선 판정·
     오류 우선순위) / T3 9ec223a(selective 배선·총합 보고·--all
     집계·디스크 중단) / T4 8b4cda0(0.8.0 범프 2지점·전체 회귀
     11패키지 ok). 태스크 리뷰 4회 전부 Approved(fix 라운드 0).
   - **최종 이중 리뷰**: Codex review 클린(발견 0) + 서브 Yes
     (I1: vacuum-busy/disk-abort 분기 미테스트 — 비차단 triage).
     fix 웨이브 **fedd5d3**(vacuumReclaim BUSY 매핑 직접 단정
     테스트·disk 중단 스킵 else 통지·주석 교정·WAL 단정 강화) →
     재리뷰(서브 단독) 승인.
   - **릴리스**: PR #20 → CI 3-OS 양 런 GREEN(fail 0) → 머지
     **5df2909**(+430/−3, 6파일) → tag **v0.8.0**.
   - **도그푸딩**: go install + hook install(cc 4이벤트·codex
     3이벤트, marker 0.8.0). **D46 literal 발화**(111,267,840B =
     임계+6.4MB, doctor [14] 경고 라인 실관측). **purge
     --older-than 48h --vacuum 실발화 성공**(사용자 창 선택):
     111,300,608→109,232,128B 회수 2,068,480B, checkpoint busy=0,
     라이브 MCP 서버 공존 성공, rc=0. 경고는 잔존(109.2MB>임계 —
     48h 창의 삭제량이 적었던 탓, 24h 재실행로 소멸 가능).
   - **특이 관찰 2건**: ① --force는 TTY 감지 시 확인 강제(자동화
     전용 설계 — 파이프 stdin으로 해소, 정상). ② **v0.7 codex MCP
     블록이 v0.8 설치 직전 부재**(mcpWritten 재기입이 증명, 현재
     단일 블록 건강) — §9-2 전역 블록 수명 리스크 첫 실사례. 재신뢰가
     config.toml에 [hooks.state] trust-hash를 기록함은 확인, 소멸
     원인은 미확정(설치가 mtime 덮어써 증거 유실 — 다음 재신뢰
     전후로 블록 존재를 관찰할 것).

4. **Repo state**: main **5df2909**(+이 기록 커밋), tag v0.8.0, 오픈
   PR·피처 브랜치 없음(로컬·리모트 삭제). 도그푸딩 marker 0.8.0,
   store 109.2MB(D46 경고 발화 중), config.toml 블록 실물 1개.

5. **Carryovers**:
   - **사용자 수동 1건(필수)**: Codex `/hooks` **재신뢰**(marker
     0.7.0→0.8.0 훅 정의 해시 변경 — 신뢰 전까지 cx 훅 스킵).
   - **관찰**: ① 재신뢰 전후 config.toml 블록 생존 여부(§9-2 실사례
     원인 추적 — 재신뢰 직후 `[mcp_servers.ctr]` 존재 확인) ② D46
     경고 잔존(109.2MB) — 24h 창 재실행 or 시간 경과 후 회수 ③ cx:
     가드 실발화(재신뢰 후 트리거 대기) ④ empty=74 7일 GC(≈07-27+)
     ⑤ cx arm n 축적.
   - 소형 후보(v0.8.x): disk-abort 분기 실행 테스트(SQLITE_FULL
     재현 곤란 — 방법 발견 시), contentFootprintOf IsDir 대칭.
   - 스크래치 C:\tmp\ctr-g4 수동 삭제(사용자 — 자동 삭제 가드).
   - **다음 버전 후보**: v0.8 §4 목록(A/B 하네스·OTel, exec 3종,
     D43, Grep 가드, doctor Codex MCP 검사, hook-only checkpoint
     소급, 회수 자동화 등) — 브레인스토밍으로 축 결정.

6. **Standing protocols**: session-23 §6 그대로(이중 검수=서브+Codex
   병렬 1패스·재검수 서브만, SDD 파일 핸드오프, BASE 원장, go test
   -p 1, §12 canary, byte-for-byte 게이트). 추가 관례: 실 store
   purge류는 사용자 창 선택 필수(AskUserQuestion), --force는 비TTY
   전용이라 자동화는 파이프 stdin.

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-24)을 읽고 재개. v0.8.0 릴리스 완료 상태(main 5df2909, tag v0.8.0, 도그푸딩 marker 0.8.0, store 109.2MB — D46 경고 발화 중). 선결: 사용자에게 Codex /hooks 재신뢰 완료 여부 확인 + 재신뢰 직후 config.toml `[mcp_servers.ctr]` 블록 생존 확인(§9-2 실사례 원인 추적). 이번 세션 후보: (a) v0.8 관찰 웨이브 — cx: 가드 실발화·D46 잔존 회수(24h 창은 사용자 결정)·empty GC 회수(≈07-27+)·cx arm 신호, (b) 다음 버전 브레인스토밍(v0.8 §4 후보 — A/B 하네스·exec 3종·D43·doctor Codex MCP 검사 등). 사용자에게 축을 물어보고 시작. 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(재권고 금지). Fable 유지, 보안 서술 최소화 준수. ultrathink
