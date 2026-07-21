# Session 15 — v0.4 observation prechecks (G1/G2/G3), implementation plan, dual adversarial review

- **Span:** 2026-07-21
- **Model:** Fable 5 (ultrathink). Subagents (opus, explicit `model:`): 1 plan
  adversarial reviewer. Codex: 1 `adversarial-review` pass (plan checkpoint —
  consumed; ~28min background completion).
- **One line:** Controller session executed all three observation gates itself
  (G1 Codex runtime → decision-table **row 1**; G2 PowerShell contract →
  matcher `Read|Bash|PowerShell`; G3 install surface → **hooks.json JSON, not
  TOML**), amended design §11.1 → wrote the full v0.4 implementation plan
  (Task 0~8) → dual adversarial review (subagent C1+M4 / Codex high4+med2)
  → merged & adjudicated (3 Codex-unique adoptions incl. `codex-hook`
  version-gate subcommand) → §11.2 record → committed `8f37e84`. Execution
  deferred to next session (user decision).

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router v0.4 구현 계획을 진행한다. 먼저 docs/prompts/2026-07-21-session-14-dogfood-followups-v04-design.md(최신 기록)를 정독한다. 상태: main에 v0.4 설계서(docs/context-router-design-v0.4-ko.md, D34~D39 — 이중 적대 검수·사용자 4결정 반영 완료) 커밋됨, follow-up 5건 PR #12 머지 완료, 도그푸딩 훅 marker 0.3.0.
1. superpowers:writing-plans로 v0.4 구현 계획(docs/superpowers/plans/)을 작성한다 — Task 0 = G1(Codex 런타임 표면)·G2(PowerShell tool_name/tool_input 계약)·G3(~/.codex/config.toml 등록 스키마) 관측 프리체크, 결과·채택 분기는 설계서 §11 추기. 이후 태스크는 설계 §9 마일스톤(PS 가드+D39 → Codex 캐프처 → D37/D38+편승 → 범프·스모크) 순.
2. 계획 확정 체크포인트: 서브에이전트 + Codex adversarial-review 1패스 병렬(인-레포 포커스 한정 — 외부 조회 금지 문면, 광역 포커스는 300s 초과·백그라운드 완주 가능) → 발견 병합·반영.
3. 계획 승인 후 superpowers:subagent-driven-development 실행 여부를 사용자와 확정한다(같은 세션 실행 vs 다음 세션).
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/ 보호). Fable 유지, 보안 서술 최소화 준수. ultrathink
```

## 2. What was done

**1) Task 0 prechecks executed by the controller session (before plan-writing
— design mandates "확정 케이스 표는 G2 관측 후 계획에 명시").**
- **G1 (Codex runtime) → decision-table row 1 adopted.** codex-cli 0.144.6
  local probe + official hooks doc (developers.openai.com/codex/hooks): Codex
  has a Claude-Code-shaped hook system — common stdin `session_id` (canonical
  UUID, matches rollout session_meta), `cwd`, `hook_event_name`; events
  SessionStart(`source`)·PreToolUse·PostToolUse(`tool_name`·`tool_input`·
  `tool_response` payload present; Bash non-zero exit also fires PostToolUse;
  no PostToolUseFailure). classify/buildEvent reusable as-is.
- **G2 (PowerShell contract) → matcher `Read|Bash|PowerShell` confirmed.**
  Scratch-project capture hook + headless `claude -p` run: tool_name exactly
  `"PowerShell"`, tool_input `{command, description}`, tool_response
  `{stdout, stderr, interrupted, isImage}` (Bash-shaped), PreToolUse fires.
- **G3 (install surface) → design's "TOML merger" clause does NOT fire.**
  Registration lives in `~/.codex/hooks.json` / `<repo>/.codex/hooks.json`
  (JSON, Claude-settings-shaped schema). `config.toml [hooks.state]` is
  Codex-owned trust-hash store — installer must never write it; user trusts
  via Codex `/hooks`. Marker rides in official `statusMessage` field.
- All results + adopted branches appended to design **§11.1** (canonical).

**2) Implementation plan written** —
`docs/superpowers/plans/2026-07-21-v04-channel-expansion.md` (writing-plans
format, TDD steps with full code). Task 0(done, verify-only) → 1(psDumpArg·
psAbsPath) → 2(guardPowerShell + matcher) → 3(D39 command shadow denylist) →
4(hook.Run Host cc/cx + `codex-hook` subcommand) → 5(Codex hooks.json
merger) → 6(D37 kind-tier ORDER BY) → 7(D38 size warning) → 8(bump 0.4.0 +
reinstall + real-host smoke + final dual review + PR/CI/merge/tag). Debt
piggyback (§6) = **none** (session-14 wave M2 advisories not actionable).

**3) Plan checkpoint — dual adversarial review, merged & adjudicated
(§11.2 is the canonical record).**
- Subagent(opus): C1 (fixture UUID mismatch → isolation test would fail AND
  not prove same-UUID isolation) + M1(strconv import)·M2(runGuard callsite)·
  M3(sibling-merger duplication — approved by G3)·M4(coverage note).
- Codex (verdict needs-attention, high4·med2): **converged** with C1/M1/M2
  (F5/F6). **Unique adopted:** F3 → drop `--host` flag, add dedicated running
  subcommand **`codex-hook`** (v0.3 binary rejects unknown subcommand exit 1 →
  "old binary + new hooks.json" cannot silently misattribute Codex events to
  cc:); F4 → group ownership becomes **all-entries rule** (mixed groups
  untouchable; 파손 금지 > idempotence completeness) + same-version
  f(f(x))==f(x) byte test; F2 partial → D39 compares after ToSlash+Clean
  (dot-segment bypass of `.docker/config.json` suffix rule closed).
  **Rejected:** F1 (alias/profile shadow of dump tokens → same accepted class
  as D32 bash `cat`; deny is recoverable via on-the-spot indexing); F2
  residue (case/symlink/PS provider drops — same surface as existing Read
  path, fail direction is safe) → recorded as §7 limits, v0.5+ candidates.
- Plan + design §11.1/§11.2 committed: **`8f37e84`** on main.

**4) Execution decision (user):** next session — this session already carried
prechecks + plan + dual review (session-14 precedent: fresh context per
phase).

## 3. Current repo state

- `main` @ (this docs commit; parent `8f37e84` = plan + §11.1/§11.2). No open
  PRs, no feature branches. Docs-only commits since v0.3.0 tag.
- Dogfooding hooks live, marker `context-router/0.3.0`, matcher `Read|Bash`
  (v0.4 Task 8 reinstalls with 0.4.0 + `Read|Bash|PowerShell` + codex).
- Untracked (intentional): `.claude/`, `ctr.exe`(stale build).
- Scratch G2 capture project lives in session scratchpad (ephemeral, not in
  repo).

## 4. Carryovers / next work

1. **Execute the plan** via superpowers:subagent-driven-development: branch
   `feat/v0.4-channel-expansion` from main HEAD (= this docs commit; record
   BASE in the SDD ledger). Task 0 is verify-only (§11.1 exists, row 1 /
   matcher / hooks.json adoptions recorded). **Ordering constraint: Task 4
   must not run before Tasks 2·3** (Run signature change updates their test
   helpers centrally).
2. Task 8 needs controller-side real-host work: binary rebuild/replace,
   `hook install` (marker 0.4.0) + `hook install --codex`, Codex trust
   approval (`/hooks`, or `--dangerously-bypass-hook-trust` for headless),
   smoke ①~④, then final dual review (subagent + Codex `review --base`).
3. Dogfood A/B continues (hooks:on heavy sessions; [14] growth channel).

## 5. Standing protocols (delta only — rest as session-14 §5)

- **Codex adversarial-review broad in-repo focus took ~28min** this time
  (vs ~10min in session-14) — Bash 600s foreground timeout auto-backgrounds
  the companion call; pair it with a status-polling Monitor as a wedge
  fallback. Plan for either signal arriving first.
- **G2-style capture harness is reusable**: scratch project + `.claude/
  settings.json` capture hooks (matcher "") + headless `claude -p ... 
  --allowedTools <Tool> --model haiku` — cheap way to observe any tool's
  hook payload contract.
- **codex-hook version gate**: never route a second host through flags on a
  fail-open entry point — old binaries swallow unknown flags silently; a new
  subcommand fails loud (exit 1) on old binaries. Generalize for future
  hosts (D28/D35 family).
- **Codex hooks.json ownership = all-entries rule** (no group-level marker
  field available): any-entry matching would delete user hooks in mixed
  groups on uninstall.

## 6. Next-session starting prompt (paste-ready)

```
context-router v0.4 구현을 진행한다. 먼저 docs/prompts/2026-07-21-session-15-v04-plan-dual-review.md(최신 기록)를 정독한다. 상태: main에 v0.4 구현 계획(docs/superpowers/plans/2026-07-21-v04-channel-expansion.md — 관측 프리체크 §11.1·이중 적대 검수 §11.2 반영 완료, 8f37e84) 커밋됨, 도그푸딩 훅 marker 0.3.0.
1. superpowers:subagent-driven-development로 계획 Task 0~8을 실행한다 — feat/v0.4-channel-expansion 브랜치(BASE = main HEAD, SDD 원장에 해시 기록), 태스크당 신선한 서브에이전트(opus — Agent 호출에 model: "opus" 명시) + 태스크 리뷰 → fix → 재리뷰. 순서 제약: Task 4를 Task 2·3보다 먼저 실행 금지. 서브에이전트 응답 분할 규율(긴 테스트 데이터는 strings.Repeat/testdata) 준수.
2. 최종 리뷰 체크포인트: 서브에이전트 + Codex review --base <BASE> 병렬 1패스 → 발견 병합·반영(재리뷰는 서브에이전트만, Codex는 체크포인트당 1패스).
3. Task 8: 버전 0.4.0 범프 → 바이너리 재빌드·재설치(claude marker 0.4.0 + codex hooks.json) → 실호스트 스모크 4종(PS deny/통과, codex cx: 등록 — Codex /hooks 신뢰 승인 필요, doctor [14] 경고 무발화) → PR → CI 3-OS GREEN → 머지 → v0.4.0 태그 → session-16 기록.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/ 보호). Fable 유지, 보안 서술 최소화 준수. ultrathink
```
