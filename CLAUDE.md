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

- Design spec (v0.0.1): `docs/context-router-design-v0.0.1-ko.md`
- Code architecture / conventions: `docs/context-router-code-architecture-ko.md`
  — zero self-defined interfaces, D13 anti-fragmentation, dependency graph,
  single-point error mapping, corruption-prevention contracts.
- Decisions D1–D13, tool audit, vision: `docs/context-router-vision-proposal-ko.md`,
  `docs/context-router-tool-audit-ko.md`.

## Standing protocols

- **Stuck** (implementer BLOCKED / 2–3 failed attempts / unclear fix): collaborate —
  Codex rescue for design/root-cause, web or context7 for library facts. No blind retry.
- **Integration & final reviews**: subagent reviewer **plus** Codex
  `review --base <ref>` in parallel, then merge findings before fixing.
- **Execution**: superpowers `subagent-driven-development` — fresh subagent per task,
  task review (spec + quality), fix → re-review. BASE from the ledger, never `HEAD~1`.
- **Subagent hygiene**: response-splitting discipline (no whole-file rewrites; build
  long test data with `strings.Repeat`/testdata). Dense tasks get split (e.g. 7a/7b).
  Go tests run with `-p 1`; respect the memory-capped test rule.
- **Secret canaries in tests**: runtime split literals (`"xox"+"b-..."`) so the source
  bytes never hold a contiguous token — avoids GitHub push-protection false positives.
- Repo `github.com/wotjr1649/context-router`. Commit trailer:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
