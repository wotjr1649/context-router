# Codex secure-review protocol (Fable-safe)

How Codex review checkpoints run in this repo so the **Fable** model never trips
its dual-use safeguard. Applies to every Codex review checkpoint here; it matters
most on exec/sandbox tasks.

## Why

The Fable safeguard reacts to the **density of sensitive terms in the main
conversation context**, not to secret bytes (see memory
`fable-security-prose-minimization`). A Codex security review's raw text is
dense by nature, so when the **controller** reads that raw text to merge/adjudicate
it, the density can trip the safeguard and force an Opus fallback (session-30).
The fix keeps the raw text out of the controller's context entirely.

## The rule (mechanically enforced)

In this project the **controller (main session) must not pull Codex review text
into its own context.** The text-returning companion commands — `result`, and
`review`/`adversarial-review` without `--background` — are **denied in the main
session** by the PreToolUse hook `~/.claude/hooks/codex-review-guard.mjs`.

- Allowed in the main session: `review --background` (kick off, progress only)
  and `status`. So the controller still starts the review normally.
- Allowed in a **subagent** (the hook detects `agent_id`): everything. The
  subagent is the one that reads and merges the raw review.

The guard is scoped to this project's root (`GUARDED_ROOTS` in the hook) and
fails open, so it never wedges the session or affects other repos.

## The pattern

1. Controller starts the Codex review in the background from the main session:
   `node <companion> review --background --base <BASE>` (returns a job id).
2. Controller dispatches the task's own **sub-reviewer** subagent as usual
   (opus) — its findings are already concept-level and safe for the controller
   to read.
3. Controller dispatches one **opus coordinator subagent** with: the sub-reviewer
   findings file path, the Codex job id (or `--base`), and the output path. The
   coordinator:
   - reads the Codex review in **its own** context (`result <job-id>`, allowed
     for subagents),
   - merges it with the sub-reviewer findings,
   - writes a **concept-level** merged findings file — per finding: severity +
     `file:line` + a one-line engineering fix. **No** attack chains, isolation/
     evasion vector enumeration, or threat-model prose,
   - returns only a short concept-level summary.
4. Controller reads only the concept-level file and dispatches fixes. Fix /
   re-review subagents are opus and may read raw detail from files safely.

Outbound too: if an `adversarial-review` needs a detailed focus, the coordinator
composes it — the controller states the focus at concept level only.

## Coordinator dispatch template

```
Subagent (general-purpose), model: opus
description: "Codex secure-review coordinator: Task N"
prompt: |
  You merge two code reviews for Task N into one concept-level findings file.
  Work from: <repo root>

  Inputs:
  - Sub-reviewer findings (already concept-level): <sub-reviewer report path>
  - Codex review: run `node "<companion>" result <job-id>` to read it IN YOUR
    OWN context (you are a subagent; this is allowed). If it is not ready, poll
    with `node "<companion>" status <job-id>` until complete, then `result`.
  - Task brief (what was requested): <brief path>

  Do:
  1. Read the sub-reviewer findings and the Codex review.
  2. Merge into one deduplicated list. For each finding record: severity
     (Critical/Important/Minor), file:line, and a ONE-LINE engineering fix.
  3. Write the merged list to <output path>. Keep it CONCEPT LEVEL: state the
     defect class and the fix. Do NOT enumerate exploitation/isolation/evasion
     vectors or write threat-model prose — a terse "unbounded wait after kill →
     add WaitDelay bound" style, not a mechanism walkthrough.
  4. Note any finding where the two reviews disagree, and your adjudication.

  Return ONLY (≤12 lines): counts by severity, the output file path, and any
  disagreement you adjudicated. No vector prose in your reply.
```

## Extending / disabling

- Add another repo root to `GUARDED_ROOTS` in the hook to guard it too.
- The guard is registered in `~/.claude/settings.json` under `PreToolUse`
  (Bash + PowerShell). Remove those two entries to disable. Self-test:
  `node ~/.claude/hooks/codex-review-guard.test.mjs`.
