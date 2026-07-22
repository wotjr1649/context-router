# Session 19 — v0.5 구현·릴리스: store 수명주기 subagent-driven 실행 → 이중 최종 리뷰 → v0.5.0 머지·태그·도그푸딩 갱신

- **Span:** 2026-07-22
- **Model:** Fable 5 (컨트롤러). Subagents (opus, explicit `model:`): 구현자
  ×9(태스크 8 + fix 2 중 1은 재사용), 태스크 리뷰어 ×8, 최종 whole-branch
  리뷰어 ×1(재리뷰 2회 SendMessage 재개). Codex: pre-merge `review --base`
  1패스(잔여 예산 전부).
- **One line:** 계획 10태스크를 subagent-driven-development로 전 태스크
  실행(태스크별 opus 구현+리뷰, fix 2웨이브) → 최종 이중 리뷰에서 Codex
  P1이 서브에이전트 2기의 판정을 반증(재색인 잔재 실버그) → 3건 fix 후
  재리뷰 승인 → CI 3-OS GREEN → PR #15 머지 90ce825, tag v0.5.0, 도그푸딩
  marker 0.5.0 가동.

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router v0.5 store 수명주기 구현을 진행한다. 먼저 docs/prompts/2026-07-22-session-18-v05-design-plan.md(최신 기록)를 정독한다. 상태: main a6bfd50(+기록 커밋, 전부 docs — 설계 정본 docs/context-router-design-v0.5-ko.md, 계획 docs/superpowers/plans/2026-07-22-v05-store-lifecycle.md 둘 다 이중 적대 검수 반영 완료), tag v0.4.0, 도그푸딩 Claude marker 0.4.0 + Codex A/B 가동.
1. superpowers:subagent-driven-development로 계획을 태스크별 실행한다 — 브랜치 feat/v0.5-store-lifecycle(BASE = 분기 시점 main HEAD를 레저에 기록, HEAD~1 금지), 순서 1→2→3a→3b→4a→4b→5a→5b→6→7. 태스크 구현자는 general-purpose + model:"opus" 명시, 태스크마다 계획의 "선행 읽기"를 먼저 수행. 태스크 리뷰(스펙+품질)와 fix→재리뷰는 서브에이전트 전용 — Codex는 최종(pre-merge) 체크포인트 review --base 1패스만 잔여.
2. 계획의 Global Constraints 준수: go test -p 1, deny+색인 테스트 CTR_HOOK_DEADLINE_MS=60000, git add -A 금지, 응답 분할 규율, D13 반파편화.
메모리 캡 테스트 규칙(-p 1) 준수. 페이지파일 의도적 비활성(사용자 확정 — 재권고 금지): 무거운 프로세스(go test·빌드·codex) 상시 직렬화, 기동 전 커밋 프리 확인. Fable 유지, 보안 서술 최소화 준수. ultrathink
```

## 2. What was done

**1) 태스크 실행 (BASE main 14ed5f6, 브랜치 feat/v0.5-store-lifecycle):**
전 태스크 TDD·태스크별 리뷰 통과. 커밋 체인: 6dc0eb4(T1 D43 drops
5필드+[12] {2,5}) → 8955763(T2 D40 SizeStat·ShadowOwned 맵) →
01f24a8(T3a [14] file=) → f79d881(T3b [15] 접두 분해) → 41c6852+a6d8916
(T4a 빈 세션 GC+[6], fix=테스트 시계 독립화) → c66c667(T4b append
WHERE EXISTS 게이트) → 1554422(T5a PurgeHookOnly·rename 격리·Vacuum) →
cca8417(T5b --hook-only CLI) → 124753c(T6 breadcrumb·0.5.0 동반 범프).
태스크 리뷰 fix 1건(T4a Important: fake-now×auto-session 시한폭탄 —
2027-01-08 이후 하드 FAIL을 `querySessionIDs` cc:% 스코프로 제거).
T4b Important(증거 갭)는 컨트롤러 직접 `-count=1` fresh GREEN으로 해소.

**2) 최종 이중 리뷰 (base 14ed5f6, head 124753c, 10커밋):**
- 서브에이전트(opus): With fixes — I1: mcp_session.go:174 불변식을 T4b가
  falsify(세션부재 Append가 supersedes INVALID_ARGUMENT 오매핑). seam
  5종 정합 확인, 이월 minors triage(#1 FIX-NOW, 4 FOLLOW-UP, 4 ACCEPT).
- Codex(P1·P2): **P1 = 서브에이전트 2기(T5a 리뷰·최종 리뷰) 판정 반증** —
  purgeHookRows 고아 정리가 DB 전체 source-less artifact 삭제; 컨트롤러
  검증으로 채택 확정(store.go:452 `ON CONFLICT(uri) DO UPDATE
  artifact_id`가 재색인 잔재를 정상 산물로 만듦 → "크래시 잔재
  unreachable" 판정 오류; 검색은 INNER JOIN이라 도달 불가하나 선택 삭제
  계약 위반 실재). P2 = .purging 크래시 잔재 영구 누수(행 삭제로 재선택
  불가 + GC len≠64 스킵).
- Fix 웨이브 04acd15(단일 서브에이전트 3건): F1 chunks→sources→artifacts
  재순서+artifacts DELETE 선캡처 hashes 바인드, F2 GCOrphanBlobs stale
  .purging age-gate 스윕, F3 supersedes 매핑 `in.Supersedes!=""` 게이트.
  재리뷰(opus 단독): approved — 3건 근본원인·비공허 RED→GREEN·신규 0.

**3) CI·릴리스:** CI 1차 lint FAIL(gofumpt 주석 정렬 2파일 — 구현자
주장 누락분) → 컨트롤러 직접 116ee9c → 재실행 전부 GREEN(양 런 3-OS).
PR #15 머지 **90ce825**(18 files +1695/-95), **tag v0.5.0**, 브랜치 삭제.
`go install` + `hook install` 재실행 — [9] marker 0.5.0, [14] file= 병기,
[15] cc:=14.0MB/cx:=768KB 실가동, [6] empty=64 실측(스테일 빈 세션 —
7일 후 GC 자동 회수 대상).

## 3. Current repo state

- `main` @ 90ce825(+이 기록 커밋), tag **v0.5.0**. 오픈 PR 없음, 피처
  브랜치 없음(remote+local 삭제).
- 도그푸딩: Claude marker 0.5.0 + Codex A/B 가동. 빈 세션 GC·[15] 접두
  분해·--hook-only 실배포 상태.

## 4. Carryovers / next work

1. **v0.5.x follow-up 백로그** (전부 비차단 minors — 최종 리뷰 triage):
   [15] shared/unattributed/다중-worktree/폴백 테스트 보강(설계 §8 완전
   이행), rename 비-부재 실패 FailedFiles++(Windows 공유 위반 관측성),
   FTS MATCH pre-state 핀+trigram 검사, appendDrop sanitize 단위테스트,
   mcp_session.go:57-58 스테일 주석, confirm 전문 hook-only 문구,
   shadowAppend event/tool 전달, 견적↔유예 발산 문서 각주.
2. **purge --hook-only 실사용 여부는 사용자 결정** — 현재 shadow-owned
   14.8MB/334 hashes (cc: 14.0MB). doctor [14] blob 21.1MB로 100MiB 경고
   한참 아래라 급하지 않음.
3. 수동 정리 잔여(세션-17~19 누적): 스테일 `mcp_servers.ctr`(0.1.0 — 빈
   세션 재생산 근원; GC가 7일 주기로 회수하나 차단은 수동), repo-root
   ctr.exe, C:\tmp 프로브, scratchpad "ctr quote probe".
4. [14]/[15]/[6] 실측 관측 재개(`CTR_STORE_WARN_BYTES` 키), cx: A/B 축적
   데이터의 다음 활용처는 차기 브레인스토밍 소재.

## 5. Standing protocols (delta only — rest as session-18 §5)

- **Codex 교차 리뷰 가치 실증 사례 기록**: P1(재색인 잔재)은 서브에이전트
  태스크 리뷰·최종 리뷰 2기가 연속 오판("크래시 잔재 unreachable")한
  것을 반증 — 컨트롤러가 채택 전 직접 소스 검증(ON CONFLICT repoint)으로
  판정하는 절차가 유효했다. 채택 판정은 리뷰어 합의가 아니라 소스 증거로.
- CI lint(gofumpt)는 구현자 자기보고와 별개로 최종 push 전 컨트롤러
  `gofumpt -l .` 1회가 싸다(이번 2파일 누락 → CI 왕복 1회 낭비).
- 페이지파일 비활성·무거운 프로세스 직렬화·`-p 1` 그대로 유지.

## 6. Next-session starting prompt (paste-ready)

```
context-router 다음 작업을 진행한다. 먼저 docs/prompts/2026-07-22-session-19-v05-implementation.md(최신 기록)를 정독한다. 상태: main 90ce825(+기록 커밋), tag v0.5.0 릴리스 완료, 도그푸딩 Claude marker 0.5.0 + Codex A/B 가동, 빈 세션 GC·[15] 접두 분해·purge --hook-only 실배포.
후보(우선순위는 사용자와 확인): ① v0.5.x follow-up 웨이브(기록 §4.1 백로그 — 전부 비차단 minors, 1구현자 번들 선례) ② v0.6 신규 범위 브레인스토밍(cx: A/B 축적 데이터 활용처, §10 잔여) ③ 수동 정리 안내(스테일 mcp_servers.ctr 차단 등).
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(사용자 확정 — 재권고 금지): 무거운 프로세스 상시 직렬화. Fable 유지, 보안 서술 최소화 준수.
```
