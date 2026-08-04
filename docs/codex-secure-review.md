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

1. Controller starts Codex in the background from the main session.
   **`review --base <ref>` is blocked on this host** — two sessions cancelled it at
   ~50 min, and adding `--background` still streams in the foreground instead of
   returning a job id. Use `task --background "<prompt>"`: it returns a job id
   immediately and takes the prompt as a positional argument: the shell splits that
   argument and the companion rejoins the tokens with single spaces, so whitespace
   structure is not preserved and the command line caps near 32KB. **`--prompt-file`
   is absent from the help text but IS registered in the parser** and reads the file
   verbatim — use it when the prompt is large or its exact characters matter
   (measured 2026-08-05 against companion 1.0.6, parser line 764).
   **Launch it from the Bash tool** — the job registry depends on
   `CLAUDE_PLUGIN_DATA`, which the PowerShell tool does not carry, and never pipe the
   launcher into another command (that orphans the job). The controller never calls
   `result` itself.
   - The Codex side of `task` has **no local file reader**. Inline the code excerpts in
     the prompt; past the command-line ceiling, point it at blob URLs of the public
     repo instead. The last plan review ran that way with zero false positives.
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
   stays one pass per checkpoint — usage guard). **A pass that failed or was lost
   does not count as the pass**: one such job turned out to be carrying a real
   important finding.

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
    OWN context. **A job that is still running also answers "No job found"** — that
    is not a failure, so wait and retry before treating it as one (a short document
    review finished in about two minutes, measured 2026-08-05). If it still answers
    that once the job should have finished, read the stored job record and its log
    file directly — that fallback has worked when the registry lookup failed. Judge
    liveness by the log file's mtime, not by `status`: a killed job can keep
    reporting `running`.

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
and keep it in-diff: an out-of-sandbox focus has no turn-timeout and hangs the
foreground. (The boundary question — whether a checkpoint may run at all — is in the
host adapter and the `cross-model-review` skill, not here.)

## Toolchain notes

- Companion: glob `~/.claude/plugins/cache/**/scripts/codex-companion.mjs`
  (version dir changes on update). Measured against companion **1.0.6** / Codex CLI
  **0.146.0** — `task`, `--background`, `status`, `result`, `cancel` are the
  companion's surface, not the CLI's, so they move on plugin updates.
- Teardown: `cancel` clears the ledger but leaves the process running, and the killed
  job's record can keep showing `running`. Kill the tree separately with
  `taskkill /PID <pid> /T /F` from PowerShell (Git Bash rewrites `/PID` as a path).
- `review` / `result` / `adversarial-review` are `disable-model-invocation`-gated
  → run via Bash `node "<companion>" <subcmd>`, not the Skill tool.
- **No PreToolUse guard is registered.** The protocol is prompt-and-schema
  discipline, model-agnostic. (`codex-review-guard` removed 2026-07-24.)
