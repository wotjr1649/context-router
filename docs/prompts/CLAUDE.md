# docs/prompts — session prompt log & handoff convention

This directory records, per session, **which prompt started it and what got
finished**, so the next session resumes without context loss.

> **Why here:** the SDD execution ledger (`.superpowers/sdd/progress.md`) is
> gitignored and does **not** survive a machine move or `git clean`. The records
> in this directory are committed to git and survive across sessions, machines,
> and compaction — the **source of truth for cross-session handoff**. On the same
> machine the SDD ledger is a useful detail companion; if they conflict, this wins.

## Record files

- Name: `YYYY-MM-DD-session-NN-<topic>.md` (session **start date**, `NN` = 2-digit index).
- The **departing** session writes/updates it and commits + pushes at session end.
- Never edit past records (append only); make corrections in a new record.

## Each record must contain

1. Session number, span.
2. **Starting prompt (verbatim)** — keep the original language; do not translate a quote.
3. **What was done** — commits / branches / PR / key decisions (Dnn).
4. **Current repo state** — `main` HEAD, open PRs, active branch, base for next work.
5. **Carryovers** — what the next session must handle (with design § references).
6. **Standing protocols** — short list + pointers.
7. **Next-session starting prompt** — a complete, paste-ready prompt that boots the next session.

## Handoff procedure

- **At session end:** fill the 7 items, prepare the next-session prompt, then
  `git add docs/prompts && git commit && git push`.
- **At session start:** read the **latest** record here and follow its
  "next-session starting prompt". Then skim the design/architecture docs and,
  on the same machine, `.superpowers/sdd/progress.md`.

Records may be in English for token efficiency; the verbatim starting prompt in
item 2 stays in its original language.
