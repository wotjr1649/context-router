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

## Docs: where things live, and when to read them

Nothing here is auto-loaded — read on demand. Three tiers, matching the ADR/KEP/RFC
industry split; do not conflate them:

- `docs/*.md` — **living canonical contracts**: design specs, architecture, vision.
  Amended in place, no date stamp. Brainstorm output that becomes a product contract
  is written here directly (project override of the superpowers skill's default spec
  path). Immutable decision history lives inside these docs as D-numbers.
  - **Design specs are a delta chain**: `context-router-design-v*-ko.md`, each version
    a delta on the one before it. The **highest version on disk is the current
    contract**; read an earlier one only to trace where a D-number came from. A
    D-number existing does not mean it is live — some were raised and then
    withdrawn. Which version is in play comes from the handoff record, not this file.
  - Architecture and conventions: `context-router-code-architecture-ko.md` — zero
    self-defined interfaces, D13 anti-fragmentation, dependency graph, single-point
    error mapping, corruption-prevention contracts.
  - Origin decisions D1–D13, tool audit, vision:
    `context-router-vision-proposal-ko.md`, `context-router-tool-audit-ko.md`.
- `docs/superpowers/specs/` + `plans/` — **dated immutable process records**:
  one-off and auxiliary specs, implementation plans (lowercase paths).
- `docs/prompts/` — session handoff records (own CLAUDE.md, append-only).

## Standing protocols

- **Stuck** (implementer BLOCKED / unclear fix): the paths here are Codex rescue for
  design and root-cause, context7 for library facts. A new topic starts a new thread.
- **Reviews — task/integration/final (user directive 2026-07-18)**: every review
  checkpoint runs a subagent reviewer **plus** one cross-model pass. Re-reviews after
  fix rounds are subagent-only — Codex stays at max one pass per checkpoint.
- **Codex invocation (measured, session-48/49)**: `review --base <ref>` is blocked on
  this host — two sessions cancelled it at ~50 min. Use `task --background
  "<prompt>"` instead: it returns a job id immediately, takes the prompt as a
  positional argument (~32KB), and has no local file reader — point it at GitHub
  blob URLs. That route returned zero false positives on the last plan review.
- **Pre-release deep review (session-46 evidence)**: before tagging a release, run
  `/code-review max` once. A branch that 11 per-task reviews, one whole-branch review
  and one Codex pass had all judged clean still yielded five shipping-blocker
  regressions and two config-corruption paths. The difference is agent count and
  *execution* verification — that pass actually built and ran the code in an isolated
  module. Task-level reviews being clean does not substitute for it.
- **Cross-model review (controller-safe)**: the controller never reads the Codex
  output directly. One opus subagent consumes it in its own context, checks it
  against the diff, and integrates both reviews into a fixed JSON findings schema;
  the controller reads only that low-density JSON and points the fix subagent at the
  file. Neutral engineering wording throughout. Schema, dispatch template and why the
  earlier hook approach was dropped: `docs/codex-secure-review.md`.
- **Execution**: superpowers `subagent-driven-development` — fresh subagent per task,
  task review (spec + quality), fix → re-review. BASE from the ledger, never `HEAD~1`.
- **Plan close-out (branch to PR)**: when a plan's feat branch is done, run one final
  whole-branch double review (max-effort subagent + one Codex pass), fix
  the merged findings, and only then open the GitHub PR — the PR shows reviewed work.
- **Subagent hygiene**: response-splitting discipline (no whole-file rewrites; build
  long test data with `strings.Repeat`/testdata). Dense tasks get split (e.g. 7a/7b).
  Go tests run with `-p 1`; respect the memory-capped test rule.
- **Secret canaries in tests**: runtime split literals (`"xox"+"b-..."`) so the source
  bytes never hold a contiguous token — avoids GitHub push-protection false positives.
- **Model and response prose (user decision 2026-07-19)**: sessions here stay on Fable.
  Keep security prose in responses minimal and abstract — discuss concepts and design
  normally, but do not enumerate sensitive categories or concrete attack notation, and
  do not repeatedly analyze or explain the safety layer itself (that re-analysis is
  itself a trigger). When writing review findings into specs (audit-record or
  threat-model sections), record the decision and disposition at concept level and
  compress the vector list; never re-enumerate the original vectors. Fallback if it
  still trips: put security-sensitive tasks on Opus
  and return to Fable for docs/instrumentation tasks.
