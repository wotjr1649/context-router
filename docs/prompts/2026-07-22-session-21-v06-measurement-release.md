# Session 21 — v0.6.0 측정 성숙: 브레인스토밍→설계→계획→구현→릴리스 (2026-07-22)

1. **Session/span/model**: session-21, 2026-07-22 (단일 세션에서 풀 사이클), claude-fable-5 (main), 서브에이전트 opus, Codex adversarial-review/review.

2. **Starting prompt**: session-20 기록의 next-session prompt로 부팅(v0.6 브레인스토밍 착수). 중간 사용자 지시(원문):
   > "스펙을 코덱스와 서브에이전트로 적대적 검수, 검토, 자문, 리뷰를 한뒤 문제 발견되면 fix혹은 브레인스토밍하여 사용자에게 질문하고 문제 없으면 승인하여 superpowers:writing-plans로 구현 계획 작성에 들어간다. ultrathink"

3. **What was done**:
   - 브레인스토밍(superpowers): 실측 기반 3방향 제시 → 사용자 결정: 축=측정 성숙(D27) / 구성=usage 비교 리포트+Codex usage 어댑터(무작위 하네스·OTel 이월) / content.db 파일 축은 경고만 편승 / 접근 A(단일 usage 확장). 관찰 A/B 첫 신호 실측(cache_read/rec on/off 0.60·중량 0.63).
   - 설계 `docs/context-router-design-v0.6-ko.md` (D44·D45·D46): 초안 ea7d022 → 1패스 이중 적대 검수 79f1059 → 2패스(사용자 지시) 94580b3 — 컨트롤러 실측 2건이 계약 확정(재개 98건 전수 단조→max-wins, 행 삭제 불변식→7일 관측 창) → 3판 재검수 SHIP 16b6a79. 사용자 결정: cx 비율은 코호트 라벨로 출력 유지.
   - 계획 `docs/superpowers/plans/2026-07-22-v0.6-measurement-maturity.md` + 이중 검수 1패스 반영 3a61bb5(=BASE).
   - SDD 실행(브랜치 feat/v0.6-measurement): T1 rollout 파서 75a6cf2(+fix d7625f0: Fold 플랫폼 게이트 Critical) → T2 cx 수집·arm bc8dd3a → T3 --compare e4d4a07 → T4 [14] 파일 축 경고 12a30a7 → T5 범프 d090a71. 전 태스크 리뷰 승인.
   - **도그푸딩 수용이 스펙 결함 적발**: "파일=세션" 반증(재개·포크가 같은 session_id로 복수 파일, 50파일=21세션, 경계 23/25 RESTART) → 스펙 교정 c65da3d + 그룹 합산 fix 2c600ec → 재실행 결과 독립 미러와 정수 일치, 전체 21세션 합계=§7 기준선 완전 일치.
   - 최종 이중 리뷰: 서브에이전트 Merge-Ready(M1 스테일 주석) + Codex P2 3건(cwd 누락 스킵 강등·WalkDir 오류 가시화·루트 경계 정규화) → 단일 fix 웨이브 6aec6b7 → 재확인 Merge-Ready. CI lint(gofumpt) 1회 수정 6c8818b.
   - **PR #17 머지 2864a29(main), tag v0.6.0 푸시, 도그푸딩 갱신(go install + hook install — [9] marker 0.6.0)**.

4. **Repo state**: main 2864a29 (tag v0.6.0), open PR 없음, feat 브랜치 삭제(remote). 도그푸딩 store: [14] blob 25.2MB·file 92.5MB(전용 임계 100MiB의 88% — D46 경고 발화 임박, 의도된 가시화), [12] drops 325 불변, [6] sessions 95(empty=69).

5. **Carryovers**:
   - v0.6.x 후보(최종 리뷰 이월, 비차단): ① loadCXSessions 행별 rows.Scan 오류 침묵 → incomplete 강등([15] 선례 정합, rollout.go) ② D46 역방향 축 독립 단정 1줄(file 키 조정 시 blob 침묵) ③ cli_test의 doctor 픽스처 재사용 범위 점검.
   - 관찰 항목: D46 파일 축 경고 실발화 시점(92.5MB→100MiB) — 발화 후 회수 자동화 재상정(§8). D42 빈 세션 GC 7일 회수(empty=69) 관측 계속.
   - 수동 잔여(이월): repo-root ctr.exe 삭제(사용자 승인 필요).
   - 다음 축 후보(§8): D35 후속(cx: tool_call 전수 Bash류 실증 소재 축적됨), 무작위 A/B 하네스(관찰 축적 후), exec 3종(D21).

6. **Standing protocols**: 리뷰 체크포인트=서브에이전트+Codex 병렬(재리뷰는 서브에이전트만, Codex 체크포인트당 1패스), SDD(브리프/보고서/리뷰패키지 파일 핸드오프, BASE는 원장), go test -p 1, §12 canary, byte-for-byte 게이트, 델타 체인 문서 규약 — 상세는 프로젝트 CLAUDE.md.

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-21)을 읽고 재개. v0.6.0 릴리스 완료 상태. 이번 세션 후보: (a) v0.6.x follow-up 소형 웨이브 — 캐리오버 ①~③ + 도그푸딩 관찰(D46 발화 여부·usage --compare 신호 재확인), 또는 (b) 다음 버전 브레인스토밍(§8 후보: D35 후속 Codex 가드 동등성 — cx: tool_call 실증 소재 준비됨). 사용자에게 축을 물어보고 시작.
