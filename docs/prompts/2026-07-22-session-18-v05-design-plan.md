# Session 18 — v0.5 design + plan: store 수명주기 브레인스토밍 → 설계 정본(2차 검수) → 구현 계획(체크포인트 검수) → 실행 준비

- **Span:** 2026-07-22
- **Model:** Fable 5. Subagents (opus, explicit `model:`): 설계 검수 ×2
  (초안·개정본), 계획 검수 ×1, 페이지파일 리서치 ×1, drops 프로브
  (Explore/haiku) ×1. Codex: adversarial-review ×3(설계 1·2차 — 2차는
  사용자 명시 지시, 계획 1차) + codex-rescue 자문 ×1(페이지파일).
- **One line:** §10 미결·실측 신호를 브레인스토밍(사용자 결정 7건)으로
  v0.5 "store 수명주기" 범위로 확정, 설계 정본 D40~D43 작성 → 이중 적대
  검수 2회(발견 17건 전건 반영) → writing-plans로 10태스크 계획 작성 →
  계획 체크포인트 이중 검수(발견 13건 전건 반영) → 실행 진입 준비 완료.

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router v0.5 신규 범위 브레인스토밍을 진행한다. 먼저 docs/prompts/2026-07-22-session-17-v05-followup.md(최신 기록)를 정독한다. 상태: main d4c204b(+docs), tag v0.4.0(신규 태그 없음 — follow-up은 릴리스 아님), 도그푸딩 Claude marker 0.4.0 + Codex 실세션 A/B 가동 중(cx: 축적).
1. superpowers:brainstorming으로 v0.5 신규 범위를 탐색한다 — 소재: 설계 §10 의도적 미결(shadow 귀속 바이트 집계·hook 전용 purge·[14] 경고 실발화 시 sweep 재상정), A/B 도그푸딩에서 축적된 cx: 데이터 활용처, 구명 breadcrumb 정리 웨이브 편승 여부. 산출은 설계 정본 델타(docs/context-router-design-v0.5-ko.md)로.
2. 설계 확정 시 이중 적대 검수(서브에이전트 + Codex adversarial-review 인-레포 포커스) 후 writing-plans로.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/·.codex/ 보호). Fable 유지, 보안 서술 최소화 준수. 페이지파일 활성화 여부(캐리오버 3)를 세션 초에 확인. ultrathink
```

(이후 사용자 후속 지시 3건: ① 개정 스펙 재검수를 Codex+서브에이전트로
— fix 후 승인 → writing-plans, 페이지파일은 **비활성 유지가 확정 결정**,
필요성 자문은 웹검색·Codex·서브에이전트 3채널 ② 계획을 Codex+서브에이전트
로 검수 — fix 후 다음 세션 subagent-driven-development 진입 프롬프트 작성.)

## 2. What was done

**1) 브레인스토밍 (사용자 결정 7건):** 축=store 수명주기 통합 웨이브 /
sweep 제외(집계·purge만, D38 문면 유지) / cx:는 귀속 집계 접두 차원만
편입 / breadcrumb 편승 제거 / 표면=doctor+purge 확장(신규 커맨드 없음) /
설계 블록 A·B·C 각 확정.

**2) 컨트롤러 실측 (설계 §9에 기록):** [14] blob(CAS) 14.1MB vs
content.db 파일 52.9MB(별개 축 — 검수에서 확정), session.db 78세션 중
**빈 세션 55개**(스테일 0.1.0 `mcp_servers.ctr`가 기동마다 생성,
retention 0), **drops unknown-session 246건 = 3클러스터**(훅 설치/승인
과도기 산물 판정), cx: 5세션·9이벤트. **서브에이전트 프로브: 서브에이전트
도구 호출에 훅 미발화 확정**(부모 귀속도 drop도 없음 — §7 한계).

**3) 설계 정본** `docs/context-router-design-v0.5-ko.md`: D40(귀속 집계 —
content_hash 단위·물리 CAS 기저·접두 분해 worktrees/* 순회), D41(`purge
--hook-only` — 견적=회수 일치·rename 격리+age gate CAS 실회수), D42(빈
세션 GC 7일·retention 무관·배치 분할·append 게이트·resume 자가 회복),
D43(drops 5필드 + [12] {2,5}필드 파서), 편승(breadcrumb 2건), v0.5.0
릴리스 대상. 커밋 체인: 4389e92(초안) → d374c53(1차 검수 반영) →
22eb061(2차 검수 반영) → a6bfd50(계획 검수 파급 — §3 rename 격리·§11.2).

**4) 검수 이력 (설계 §11·§11.1·§11.2에 정본 기록):**
- 설계 1차(서브에이전트 C1I3M3 + Codex NO-SHIP 5건): CAS는 content.db
  밖 물리 파일(행 삭제+VACUUM 회수 불가) → 실회수 재설계, 귀속 단위
  artifact→content_hash, GC×append TOCTOU, worktree 과소귀속, [12] 파서.
- 설계 2차(사용자 지시 — 서브에이전트 I2M3 + Codex NO-SHIP 4건):
  "미참조 재확인"으로 TOCTOU 안 닫힘 → age gate 재사용, 전체 append
  게이트(shadowCapture 2건), resume 상호작용, Sweep 잠금 예산(배치),
  artifact_refs는 URI 배열(원값 아님), D43 {2,5} 고정.
- 계획 1차(서브에이전트 C1I3M5 + Codex NO-SHIP 6건): mcp.ServerVersion
  동반 범프(핀 테스트 실재), rename 격리(Stat↔unlink 창), GC 후보 tx 내
  재검증, SweepReport 반환 계약, Vacuum API 신설, ShadowOwned 맵,
  SizeStat 심볼 고정, `go install` 스모크 금지(활성 훅 오염) →
  `go build -o` 임시경로, Task 3 분할(3a/3b), confirm 중복·보고 순서.

**5) 구현 계획** `docs/superpowers/plans/2026-07-22-v05-store-lifecycle.md`
(10태스크: 1 D43 → 2 SizeStat → 3a [14] → 3b [15] → 4a GC → 4b 게이트 →
5a PurgeHookOnly → 5b CLI → 6 편승·버전 → 7 통합·PR). 검수 반영 완료 —
실행 준비 상태.

**6) 페이지파일 3채널 자문 (사용자 결정: 비활성 유지 확정):** 웹검색
(Microsoft Learn·Russinovich 인용)·Codex·서브에이전트 일치 — 필수 아님,
단 커밋 한도=RAM·크래시 덤프 불가가 확정 비용이며 관측된 OOM은 정상
귀결. 완화책: Commit Free 감시(Process Explorer/perfmon 85% 알람),
explorer Private Bytes 폭식 시 재시작, 무거운 프로세스 상시 직렬화(이
머신 표준 운영 모드 — auto-memory 갱신 완료, 활성화 재권고 금지).
참고: 덤프 필요 시 DedicatedDumpFile(페이징 미사용) 분리 수단 존재.

## 3. Current repo state

- `main` @ a6bfd50(+이 기록 커밋) — 전부 docs(설계·계획·기록), 코드
  무변경. 최신 태그 v0.4.0. 오픈 PR 없음, 피처 브랜치 없음.
- 도그푸딩: Claude marker 0.4.0, Codex A/B 가동(cx: 축적). 스테일
  `mcp_servers.ctr`(0.1.0)이 여전히 빈 세션 생산 중 — 사용자 수동 정리
  대기(세션-17 캐리오버 4).

## 4. Carryovers / next work

1. **v0.5 구현 실행** — 계획을 subagent-driven-development로 태스크별
   실행(§6 프롬프트). BASE = 실행 시점 main HEAD를 레저에 기록.
2. Codex 예산 상태: 설계 2패스·계획 1패스 소진. **잔여는 최종(pre-merge)
   체크포인트 `review --base <ref>` 1패스뿐** — 태스크/fix 재리뷰는
   서브에이전트 전용.
3. 스테일 `mcp_servers.ctr`·repo-root ctr.exe 정리(사용자 수동 — 빈 세션
   재생산 즉시 중단됨), 기타 세션-17 캐리오버 4의 수동 정리 잔여.
4. 페이지파일: **비활성 유지 확정(사용자)** — 활성화 재상정 금지. 무거운
   프로세스 직렬화가 표준 운영 모드.
5. 릴리스 후: [14]/[15]/[6] 실측 관측 재개(경고 키 `CTR_STORE_WARN_BYTES`),
   merge 후 `go install`로 도그푸딩 바이너리 갱신(계획 Task 7 Step 4).

## 5. Standing protocols (delta only — rest as session-17 §5)

- **페이지파일 의도적 비활성(사용자 확정)**: 커밋 프리 확인
  (`Win32_OperatingSystem.FreeVirtualMemory`) 후 무거운 프로세스 기동,
  상시 직렬화. 활성화 권고 금지.
- **서브에이전트 도구 호출은 훅 미발화**(2026-07-22 실측) — 도그푸딩
  계측은 컨트롤러 세션만 반영. drops 재발 시 D43 필드로 판정.
- 계획 코드 블록은 "설계 계약 기준" — 태스크 구현자는 선행 읽기 파일로
  실제 심볼 확정 후 Interfaces 계약(이름·타입 고정)만 불변 유지.
- general-purpose 서브에이전트는 `model: "opus"` 명시(전역 규칙).

## 6. Next-session starting prompt (paste-ready)

```
context-router v0.5 store 수명주기 구현을 진행한다. 먼저 docs/prompts/2026-07-22-session-18-v05-design-plan.md(최신 기록)를 정독한다. 상태: main a6bfd50(+기록 커밋, 전부 docs — 설계 정본 docs/context-router-design-v0.5-ko.md, 계획 docs/superpowers/plans/2026-07-22-v05-store-lifecycle.md 둘 다 이중 적대 검수 반영 완료), tag v0.4.0, 도그푸딩 Claude marker 0.4.0 + Codex A/B 가동.
1. superpowers:subagent-driven-development로 계획을 태스크별 실행한다 — 브랜치 feat/v0.5-store-lifecycle(BASE = 분기 시점 main HEAD를 레저에 기록, HEAD~1 금지), 순서 1→2→3a→3b→4a→4b→5a→5b→6→7. 태스크 구현자는 general-purpose + model:"opus" 명시, 태스크마다 계획의 "선행 읽기"를 먼저 수행. 태스크 리뷰(스펙+품질)와 fix→재리뷰는 서브에이전트 전용 — Codex는 최종(pre-merge) 체크포인트 review --base 1패스만 잔여.
2. 계획의 Global Constraints 준수: go test -p 1, deny+색인 테스트 CTR_HOOK_DEADLINE_MS=60000, git add -A 금지, 응답 분할 규율, D13 반파편화.
메모리 캡 테스트 규칙(-p 1) 준수. 페이지파일 의도적 비활성(사용자 확정 — 재권고 금지): 무거운 프로세스(go test·빌드·codex) 상시 직렬화, 기동 전 커밋 프리 확인. Fable 유지, 보안 서술 최소화 준수.
```
