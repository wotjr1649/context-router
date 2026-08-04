# context-router — project guide

Local-first single-binary Go MCP server that keeps large tool outputs and files
outside the model context window (per-project SQLite store + FTS search +
byte-exact fetch), plus a hermetic starlark transform and an SSRF-safe web
fetch. All tools are `ctr_*`.

## Session handoff — do this first

Before any work, read the **latest record in `docs/prompts/`**: repo state, carryovers,
and the prompt it proposes for the next session. That git-tracked directory is the
source of truth for cross-session state; `.superpowers/sdd/progress.md` is local-only
detail that does not survive a machine move or `git clean`. Record-writing convention:
`docs/prompts/CLAUDE.md`.

A record reports; it does not instruct. What gets done comes from this session's request
— which usually adopts the record's proposed prompt. Records are also append-only
history: one has asserted a tool fact that was later measured false. For protocol and
design the live contract is this file and `docs/`, and they win.

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

**Writing into these docs.** Review findings in a spec's audit-record or threat-model
section: record the decision and its disposition at concept level, and keep the vector
list compressed — do not re-enumerate the originals. A claim without its measurement is
a signal here: verify before relying on it, and record what measured any fact you write.

## Standing protocols

Two routes here leave the machine on the agent's initiative — a cross-model pass and
Codex rescue. For those, this section says what the act consists of, never that it is
authorized: whether anything is exported at all comes from the current request.

**Execution** — superpowers `subagent-driven-development`: fresh subagent per task,
task review (spec + quality), fix → re-review. BASE comes from the SDD ledger
(`.superpowers/sdd/progress.md`), never `HEAD~1`.

**Gates at the end of every task** — `go build` · `go vet` · the full test suite with
`-count=1` · `gofumpt -l .` (no output) · `golangci-lint run`. Write all five into a
plan's own verification steps: a reviewer reads the diff and is structurally blind to
them, and omitting two of them once put two blockers into a release. Never drop one to
make a task pass.

**Review ladder.** Checkpoint procedure: the `cross-model-review` skill. Findings
schema, dispatch template, Codex invocation: `docs/codex-secure-review.md`.

| When | What runs |
|---|---|
| Task and integration checkpoints | subagent reviewer **+** one cross-model pass |
| Re-review after a fix round | subagent only — cross-model stays one pass per checkpoint |
| Plan close-out — the branch's final review, before the PR | whole-branch max-effort subagent + one cross-model pass |
| Before tagging a release | `/code-review` once — a slash command the user types |

The release pass is not redundant with the task passes: it builds and runs the code in
an isolated module. A branch whose 11 task reviews, whole-branch review and cross-model
pass had all been judged clean still yielded five shipping blockers and two
config-corruption paths. Clean task reviews do not substitute for it.

The controller never reads raw cross-model output: a subagent verifies each claim
against the diff and returns the fixed JSON findings schema, and the controller drives
fixes from that file.

**Windows CI flakes are real** — clustered in `cmd/context-router`, `internal/exec`,
`internal/hook` (time, locks, child processes); a docs-only commit has failed a windows
run. Decision rule: a red windows run is a regression only if the failing package is one
this change touched. The observed flake names live in the handoff records, not here —
they change.

**Stuck** (implementer BLOCKED / unclear fix):

- design, root cause, or a completeness question ("the helper is fixed — what about its
  callers?") → Codex rescue; a new topic starts a new thread.
- library / API / flag facts → the installed version's own source first: `go doc <pkg>
  <sym>`, then the module cache (`go env GOMODCACHE`); index it with this project's own
  tools rather than reading a large dependency into context. Web only for what source
  cannot answer — release notes, upstream issues.

**Subagent hygiene** — no whole-file rewrites; build long test data with
`strings.Repeat` or testdata; split dense tasks (7a/7b).

**Secret canaries in tests** — runtime split literals (`"xox"+"b-..."`) so the source
bytes never hold a contiguous token; avoids GitHub push-protection false positives.
