# context-router — project guide

Local-first single-binary Go MCP server that keeps large tool outputs and files
outside the model context window (per-project SQLite store + FTS search +
byte-exact fetch), plus a hermetic starlark transform and an SSRF-safe web
fetch. All tools are `ctr_*`.

## Session handoff — do this first

Before any work, read the **latest record in `docs/prompts/`** and follow its
"next session" prompt. That git-tracked directory is the source of truth for
cross-session handoff; `.superpowers/sdd/progress.md` is local-only detail that
does not survive a machine move or `git clean`. Record-writing convention:
`docs/prompts/CLAUDE.md`.

## Design & rules (read on demand, not auto-loaded)

- Design specs (delta chain): `docs/context-router-design-v0.0.1-ko.md` →
  `-v0.1-ko.md` (D14–D20) → `-v0.2-ko.md` (D21–D28).
- Code architecture / conventions: `docs/context-router-code-architecture-ko.md`
  — zero self-defined interfaces, D13 anti-fragmentation, dependency graph,
  single-point error mapping, corruption-prevention contracts.
- Decisions D1–D13, tool audit, vision: `docs/context-router-vision-proposal-ko.md`,
  `docs/context-router-tool-audit-ko.md`.

## Docs layout (directory convention, 2026-07-20)

Two tiers, matching the ADR/KEP/RFC industry split — do not conflate:

- `docs/*.md` — **living canonical contracts**: versioned design specs
  (`context-router-design-v0.X-ko.md`), vision, architecture. Amended in place,
  no date stamp. Brainstorm output that becomes a product contract is written
  here directly (project override of the superpowers skill's default spec path).
  Immutable decision history lives inside these docs as D-numbers.
- `docs/superpowers/specs/` + `plans/` — **dated immutable process records**:
  one-off/auxiliary specs and implementation plans (lowercase paths).
- `docs/prompts/` — session handoff records (own CLAUDE.md, append-only).

## Standing protocols

- **Stuck** (implementer BLOCKED / 2–3 failed attempts / unclear fix): collaborate —
  Codex rescue for design/root-cause, web or context7 for library facts. No blind retry.
- **Reviews — task/integration/final (user directive 2026-07-18)**: every review
  checkpoint runs a subagent reviewer **plus** Codex `review --base <ref>`
  (cross-model, multi-angle). Re-reviews after fix rounds are subagent-only —
  Codex stays at max one pass per checkpoint (usage guard).
- **Cross-model review (controller-safe, 2026-07-24 근본 개선)**: root fix is
  neutral framing + structured output, not a hook. Controller starts Codex with
  `review --background` (never calls `result`), then dispatches **one review
  subagent (opus)** that reads the Codex result in its own context, verifies it
  against the diff, and integrates both reviews into a fixed **JSON findings
  schema** (severity + `file:line` + one neutral line for defect + one for fix).
  The controller consumes only that low-density JSON (counts/verdict/`file:line`)
  and points the fix subagent at the JSON file. Neutral engineering wording only —
  no exploit/isolation/evasion narrative. No PreToolUse guard (`codex-review-guard`
  removed). Full protocol + schema + dispatch template: `docs/codex-secure-review.md`.
- **Execution**: superpowers `subagent-driven-development` — fresh subagent per task,
  task review (spec + quality), fix → re-review. BASE from the ledger, never `HEAD~1`.
- **Subagent hygiene**: response-splitting discipline (no whole-file rewrites; build
  long test data with `strings.Repeat`/testdata). Dense tasks get split (e.g. 7a/7b).
  Go tests run with `-p 1`; respect the memory-capped test rule.
- **Secret canaries in tests**: runtime split literals (`"xox"+"b-..."`) so the source
  bytes never hold a contiguous token — avoids GitHub push-protection false positives.
- Repo `github.com/wotjr1649/context-router`. Commit trailer:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
