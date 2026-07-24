# Cross-model review protocol (structured, controller-safe)

How Codex review checkpoints run in this repo. Goal: the controller (main
session) can consume a cross-model review's outcome without its content-safety
layer tripping on dense review prose. The root fix is **neutral framing +
structured output**, not a wrapper that hides the text from the controller.

## Why the earlier hook approach was removed (2026-07-24)

A PreToolUse guard once blocked the controller from reading raw review text and
forced a coordinator subagent to merge it. That treated the symptom: the
controller still had to read *some* rendered summary, and if that summary was
dense prose it tripped the layer regardless of who produced it. Blocking "who
reads it" never fixed "what gets rendered". The guard hook and its coordinator
hand-off were removed; the durable fix shapes the review so its consumable
output is low-density by construction.

## Principles (web research — `.superpowers/sdd/safeguard-research.md`; G-Research, Crash Override)

1. **Structured findings, not prose.** Normalize the review to a fixed JSON
   schema: severity + `file:line` + one neutral line for the defect + one neutral
   line for the fix. A normalized array is low-density; narrative prose is not.
2. **Neutral framing.** Ask for a general correctness / contract-compliance /
   resource-safety review, not an adversarial/exploit-labeled one. Fewer dense
   topic terms in both the request and the result.
3. **Legitimate context up front.** Own code, defensive/quality task.
4. **Reviewer output is untrusted.** Validate to the schema; the controller
   reads counts + verdict + `file:line` and drives fixes from the JSON file.

## The pattern (no hook, one review subagent)

1. Controller starts Codex in the background from the main session:
   `node "<companion>" review --background --base <BASE>` — returns a job id.
   The controller never calls `result` itself.
2. Controller dispatches **one review subagent (opus)**. It:
   - reviews the diff package itself, in neutral framing,
   - reads the Codex review in its own context (`result <job-id>`),
   - verifies each Codex claim against the real diff (accept / downgrade /
     reject false positive, each with a one-line reason),
   - integrates both into a single findings file in the schema below,
   - returns ONLY: verdict + counts by severity + the file path + the ids of any
     `plan_conflict` findings. Nothing beyond one neutral line per finding.
3. Controller reads the JSON (low-density) and dispatches a fix subagent pointed
   at the JSON file as its spec ("fix every critical + important").
4. Re-review after a fix round = the review subagent again (subagent-only; Codex
   stays one pass per checkpoint — usage guard).

Fix / re-review subagents are opus and may read raw detail from files safely.

## Findings schema

```json
{
  "verdict": "approved | needs_fixes",
  "counts": { "critical": 0, "important": 0, "minor": 0 },
  "findings": [
    {
      "id": "F1",
      "severity": "critical | important | minor",
      "file": "internal/exec/exec.go",
      "line": 116,
      "defect": "one neutral line: the observed wrong behavior",
      "fix": "one neutral line: the engineering change",
      "source": "sub | codex | both",
      "plan_conflict": false
    }
  ]
}
```

- `defect` / `fix`: neutral engineering wording — state the behavior and the
  change. Do **not** write exploit / isolation / evasion narrative.
- `plan_conflict: true` → the fix would contradict the plan's text; the
  controller escalates to the human (these are typically non-security, e.g. an
  env-key or closed-table decision).
- `source`: which review raised it (used for final-review triage).

## Review subagent dispatch template

```
Subagent (general-purpose), model: opus
description: "Cross-model review: Task N"
prompt: |
  Review one task's diff for correctness, contract-compliance, resource safety,
  and error handling. This is your own code under a quality task.
  Work from: <repo root>

  Inputs:
  - Task brief (requirements): <brief path>
  - Diff package: <review-package path>
  - Codex review: run `node "<companion>" result <job-id>` to read it in YOUR
    OWN context. If not ready, poll `status <job-id>`, then result.

  Do:
  1. Review the diff yourself against the brief.
  2. Read the Codex review; verify each claim against the real diff
     (accept / downgrade / reject false positive, one-line reason each).
  3. Integrate both into ONE findings file at <output path>, in the fixed JSON
     schema (verdict, counts, findings[]). Each finding: severity + file:line +
     ONE neutral line for the defect + ONE neutral line for the fix + source +
     plan_conflict. Neutral engineering wording only — the behavior and the
     change; NO exploit/isolation/evasion narrative.
  4. Set plan_conflict=true on any finding whose fix would contradict the brief.

  Return ONLY (<=10 lines): verdict, counts by severity, the output file path,
  and the ids of any plan_conflict findings. No prose beyond that.
```

## Outbound (adversarial-review)

Prefer neutral `review` for routine checkpoints; avoid `adversarial-review`. If a
focused check is genuinely needed, state the focus in neutral engineering terms
(a contract to verify, an input class to exercise) — never an exploit narrative —
and keep it in-diff (an out-of-sandbox focus has no turn-timeout and hangs; see
global CLAUDE.md §5).

## Toolchain notes

- Companion: glob `~/.claude/plugins/cache/**/scripts/codex-companion.mjs`
  (version dir changes on update).
- `review` / `result` / `adversarial-review` are `disable-model-invocation`-gated
  → run via Bash `node "<companion>" <subcmd>`, not the Skill tool.
- **No PreToolUse guard is registered.** The protocol is prompt-and-schema
  discipline, model-agnostic. (`codex-review-guard` removed 2026-07-24.)
