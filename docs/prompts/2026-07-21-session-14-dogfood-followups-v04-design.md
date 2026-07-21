# Session 14 — dogfood check, follow-up wave (PR #12), v0.4 design (D34~D39)

- **Span:** 2026-07-21
- **Model:** Fable 5 (ultrathink). Subagents (opus, explicit `model:`): 1
  implementer + 1 task reviewer + 1 design adversarial reviewer. Codex: 1
  `adversarial-review` pass (design checkpoint — consumed).
- **One line:** v0.3 instrumentation channels verified against live dogfood
  data (drops fully explained, zero steady-state loss) → follow-up backlog 5
  items shipped via single wave (PR #12, CI 3-OS GREEN, merged `1b7ed4c`) →
  v0.4 scope brainstormed with user, design doc written, dual-adversarial
  reviewed (2 Critical + 5 Important + 5 Minor subagent / 5 high + 3 medium
  Codex), revised per 4 user decisions to D34~**D39**, final-approved.

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router 후속 작업을 진행한다. 먼저 docs/prompts/2026-07-21-session-13-v0.3-implementation-merge.md(최신 기록)를 정독한다. 상태: v0.3.0 태그 완료(main 48f610e, PR #11 머지, CI 3-OS GREEN), 도그푸딩 훅 marker 0.3.0 재설치됨(이 세션부터 hooks:on 중량 세션 축적 시작).
1. 도그푸딩 데이터 점검: usage --totals와 doctor([12] 사유 롤업·[14] content.db 규모)로 v0.3 계측 채널의 실사용 축적을 확인하고, 눈에 띄는 계측 공백/오분류를 기록한다.
2. follow-up 백로그 5건(기록 §4.1 — 각 1~10줄) 처리: 단일 웨이브 + 태스크 리뷰 1회.
3. v0.4 스코프 브레인스토밍(superpowers:brainstorming부터): 설계 §10 이월(exec 3종, Codex 훅, provenance 표시, shadow 자동 캡/sweep — [14] 실측 후, PowerShell/Grep 가드, spill journal 재상정 §1.3)에서 사용자와 우선순위를 정하고 설계 델타(D34~)를 잡는다.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/ 보호). Fable 유지, 보안 서술 최소화 준수. ultrathink
```

## 2. What was done

**1) Dogfood instrumentation check — all channels consistent, no
misclassification.** `[12]` 217 drops = install-time 87-min burst 216
(unknown-session, already adjudicated in v0.3 design §1.3) + bad-input 1 (T11
window); steady-state drops **0** through session-13's heavy subagent waves
AND this session (natural experiment: 3 opus subagents, tens of tool calls →
drops unchanged 216/1 — subagent events attribute to the parent session, no
capture gap). `[14]` growth: blob 6.09→6.51→7.62MB (session-13 +0.4MB,
session-14 +1.1MB — two data points). `usage --totals` hooks:on accumulation
live (573 records incl. 2 heavy sessions; current session captured in
real time). Only gap noted: drop lines carry `ts\treason` only (no session
attribution) — recorded, not scoped (steady-state drops are 0; YAGNI).

**2) Follow-up wave (5 items, single wave + 1 task review).** Branch
`chore/v0.3-followups` (base `eaab547`), implementer(opus) commit `bb0444c`:
① cli.go 714·1201 `session.LockFileName` (1203 display label kept — sibling
labels unexported, adjudicated) ② matched_pattern `git_diff`/`build_run`
asserts ③ artifact_created ref 64-hex assert ④ `mcp.ServerVersion` export +
version/serverVersion pin test in main_test.go ⑤ SizeStats ro-open fail-soft
documented (store+doctor comments + v0.3 design D33a 1 line). Task
review(opus): **Spec ✅ / Quality Approved, C0/I0/M2 advisory** (⑤ WAL causal
wording loose but within hedge; ① reverse-of-hint choice well-grounded).
Full `-p 1` GREEN, gofumpt clean. **PR #12 → CI 3-OS all GREEN → merged
`1b7ed4c`** (merge commit, branch deleted).

**3) v0.4 design.** Brainstorming (superpowers:brainstorming): user picked
**채널 확장** theme / Codex hook = **측정·캐프처만** / PS guard = **D32 동등
전체** / piggyback = provenance 표시 + [14] 임계 경고. Wrote
`docs/context-router-design-v0.4-ko.md` (D34~D38). Then per user directive,
**dual adversarial review** (subagent(opus) C2·I5·M5 + Codex 1 pass high5·
medium3; converged 6, unique 2). Key corrections: `usage` is a Claude
transcript enumerator (`cc:`+uuid hardcoded) so cx: can never appear there;
D35 fallback cases non-exhaustive; TOML installer is net-new (D28 principles
only); psDumpArg grammar unpinned; **`inline:` is live ctr_index explicit
ingest, not legacy** (URI-prefix priority inverted user intent AND broke
`TestQuery_HitSourceShadowCoexist`); D38 measures whole-CAS blob, not shadow;
shadowCapture denylist covers only `fileOriginTools={Read,NotebookRead}`
(ground truth verified); deny precondition `Indexed==1` was missing. **4 user
decisions:** ① cx: acceptance via session.db surfaces (`ctr_session_summary`/
`ctr_export_events`), usage extension out of scope ② command shadow denylist
= statically-proven-path comparison only (**new D39**, Bash+PS symmetric) ③
D37 redefined as **source_kind tier** (hook=passive lowest; keeps coexist
test contract — promotes lexical accident to intended hierarchy) ④ D38
recast as whole-store size warning + purge non-selective nature stated.
Doc revised (orthogonal G1 decision table rows 1–5, gates G1/G2/G3, psDumpArg
minimal grammar, deny order ①~④ with `Indexed==1`, §11 adjudication record)
→ **final-approved, committed**.

**4) 자문 (user asked):** implementation plan deferred to next session —
rationale: session already carried wave+reviews+PR+design+dual review; plan
checkpoint deserves its own Codex pass and fresh context (session-12
precedent); docs/prompts handoff is the natural cut.

## 3. Current repo state

- `main` @ (this docs commit; parent `1b7ed4c` = Merge PR #12), tag v0.3.0.
  No open PRs, no feature branches. CI GREEN.
- Dogfooding hooks live, marker `context-router/0.3.0`, matcher `Read|Bash`.
- Untracked (intentional): `.claude/` (dogfood), `ctr-new.exe` (stale).

## 4. Carryovers / next work

1. **v0.4 implementation plan** (superpowers:writing-plans) from
   `docs/context-router-design-v0.4-ko.md` (D34~D39). Task 0 = G1(Codex
   런타임 표면)·G2(PS tool_name/tool_input)·G3(config.toml 등록 스키마)
   관측 프리체크 — 결과·채택 분기를 설계서 §11에 추기(계획 산출물 단독
   기록 금지). Plan checkpoint: Codex adversarial-review 1 pass (in-repo
   focus only) + subagent, then subagent-driven-development.
2. **Dogfood A/B continues** — hooks:on heavy-session accumulation; [14]
   growth now 0.4~1.1MB/heavy session (two data points, D38 rationale).
3. Wave M2 advisories: none actionable (recorded in review file only).

## 5. Standing protocols (delta only — rest as session-13 §5)

- **Codex `adversarial-review` with a broad in-repo focus exceeds the 300s
  foreground window** → auto-backgrounds and completes fine (~10min this
  session). Plan for background monitoring; in-diff/in-repo focus rule held
  (no hang).
- **Subagent tool events attribute to the parent session** — zero
  unknown-session drops across 3-subagent activity (natural experiment).
  Don't chase subagent-capture gaps.
- **shadowCapture denylist ground truth**: `fileOriginTools={Read,
  NotebookRead}` only (`shadow.go:23`) — command outputs (Bash, future PS)
  index without path denylist today; D39 is the scoped fix.
- Review-package BASE for the wave was ledger-recorded `eaab547` (post
  session-13-docs commit, not the merge commit) — BASE-from-ledger rule paid
  again.

## 6. Next-session starting prompt (paste-ready)

```
context-router v0.4 구현 계획을 진행한다. 먼저 docs/prompts/2026-07-21-session-14-dogfood-followups-v04-design.md(최신 기록)를 정독한다. 상태: main에 v0.4 설계서(docs/context-router-design-v0.4-ko.md, D34~D39 — 이중 적대 검수·사용자 4결정 반영 완료) 커밋됨, follow-up 5건 PR #12 머지 완료, 도그푸딩 훅 marker 0.3.0.
1. superpowers:writing-plans로 v0.4 구현 계획(docs/superpowers/plans/)을 작성한다 — Task 0 = G1(Codex 런타임 표면)·G2(PowerShell tool_name/tool_input 계약)·G3(~/.codex/config.toml 등록 스키마) 관측 프리체크, 결과·채택 분기는 설계서 §11 추기. 이후 태스크는 설계 §9 마일스톤(PS 가드+D39 → Codex 캐프처 → D37/D38+편승 → 범프·스모크) 순.
2. 계획 확정 체크포인트: 서브에이전트 + Codex adversarial-review 1패스 병렬(인-레포 포커스 한정 — 외부 조회 금지 문면, 광역 포커스는 300s 초과·백그라운드 완주 가능) → 발견 병합·반영.
3. 계획 승인 후 superpowers:subagent-driven-development 실행 여부를 사용자와 확정한다(같은 세션 실행 vs 다음 세션).
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/ 보호). Fable 유지, 보안 서술 최소화 준수. ultrathink
```
