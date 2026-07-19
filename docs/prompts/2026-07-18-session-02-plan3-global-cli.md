# Session 02 — Plan 3 (v0.0.1 global + CLI + gates → tag-ready)

- **Span:** 2026-07-18 – 2026-07-19
- **Model:** Fable 5 (xhigh). Reviews: Claude subagents (sonnet/opus/fable) + Codex cross-model.
- **One line:** Plan 3 executed end-to-end — v0.0.1 redefined contract completed (ctr_global_search + 4 CLI + gates), all 10 tasks dual-reviewed, final whole-branch dual review merged, PR opened. Tag pending manual smoke.

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router 계획 3을 시작한다. 먼저 docs/prompts/2026-07-17-session-01-plan0-1-2.md 를 정독하고 그 안의 "Next-session starting prompt(§6)"를 그대로 따른다. PR #1은 이미 main(d73294a)에 머지됐으니, main에서 feat/v0.0.1-global-cli 브랜치를 따서 진행한다. ultrathink
```

Mid-session directive (verbatim): review protocol change —
```
리뷰 할 때에는 codex와 claude code로 다각도로 리뷰 할 수 있게 변경한다. claude 모델로 구현한 것을 claude 모델로 리뷰 하는것 보단 codex 모델로도 리뷰 해야지 여러 관점으로 판단이 가능 할 것 같다. ultrathink
```

## 2. What was done

**Plan doc:** `docs/superpowers/plans/2026-07-18-context-router-v0.0.1-global-cli.md` (10 tasks). Executed via `subagent-driven-development` (fresh implementer per task + task review + fix→re-review).

**Review protocol (upgraded mid-session, in project CLAUDE.md):** every review checkpoint = Claude subagent reviewer **+** Codex `review --base <ref>` in parallel → merge → fix. Re-reviews after fix rounds are subagent-only (Codex max one pass per checkpoint, usage guard). This caught defects an all-Claude review missed — see §carryover proof.

**10 tasks (all merged, branch `feat/v0.0.1-global-cli`, 38 commits off main d73294a):**
- T1 store advisory lock (5da2971) — WAL first-open race. Original plan (DSN reorder) **empirically disproven** by implementer (modernc sorts busy_timeout first); Codex rescue → root cause = wal-index recovery lock bypasses busy handler → OS advisory lock (`content.db.rebuild.lock`, flock/LockFileEx) serializing writable Open.
- T2 ctr_global_search (6048e8f) — separate registration, `--projects` allowlist, read-only multi-project merge (§4.6/§5.4).
- T3 cli + doctor + upgrade (5fa2d61) — dispatch, no-create doctor, minimal upgrade.
- T4 stats (c366917) — local ledger + provider transcript; 3 fix rounds (dispatchCLI prescan, path leak, Codex P2×7).
- T5 purge (2f71088) — TTY slug 2-step confirm + --older-than + --gc; Codex P1 = GC↔Register blob race → age gate.
- T6 minor bundle (c963957) — 8 carryovers; title redaction/cap, DNS multi-addr, transform timeout Config; 3 fix rounds (timing-test determinism).
- T7 LICENSE/NOTICE/THIRD-PARTY-NOTICES/README (146009b) — ELv2 + 25 linked-module license texts + profile fail-closed (validProfile).
- T8 gates 1·2 (969bd65) — oracle equivalence 12/12, recall@5 0.9306/@10 1.0; golden authenticity proven by oracle↔ctr divergence.
- T9 CI (f365b14) — 3-OS matrix + crossbuild 6 targets. CI found real bugs: main.go store-root symlink bypass; ident.Fold unix-`\` corruption + ingest TOCTOU (Codex reverted a Claude-approved direction).
- T10 gates 11·10 + checklist + version 0.0.1 (367c1a5) — schema budget 4359B→cap 5231B, stdout purity; gate-7 deep tests found Go stdlib NTFS-junction blindness → GetFinalPathNameByHandle realpath.

**Final whole-branch dual review** (base main d73294a): Claude(fable) With-fixes Imp×3 + Codex P1×2/P2×5. Converged finding = junction resolution half-applied. Single fix wave F1–F10 (see `.superpowers/sdd/final-fix-report.md`): F1 ident.RealPath export to all consumers, F2 GC lockStore serialization, F3 purge --all fail-stuck, F4 server positional-arg reject, F5 --projects ID/dedup, F6 doctor non-dir ancestor, F7 FTS integrity rank=1, F8 kill-test readiness, F9 netfetch per-addr TLS retry, F10 comment/doc corrections.

## 3. Current repo state

- `main` @ **78a7927** — **PR #2 MERGED** (2026-07-19): Plan 1+2+3 all on main. Branch `feat/v0.0.1-global-cli` (head e7dc7c5) merged, kept (not deleted).
- **PR #2**: https://github.com/wotjr1649/context-router/pull/2 — merged with dependency review doc included (bc6ab2e). Main CI run after merge must be confirmed GREEN before tagging.
- **Dependency review (2026-07-19, user-directed):** `docs/superpowers/Specs/2026-07-19-pr-2-go-dependency-library-review.md` audited against PR #2 — decision upheld: **PR #2 keeps current `go.mod` unchanged** (none of §11's 3 blocking exceptions met). Verified: `go mod verify` all-verified, `go mod tidy -diff` clean, govulncheck **0 vulnerabilities**, dual-OS `CGO_ENABLED=0` builds OK, NOTICE/THIRD-PARTY == actual 25-module linked closure (go.mod's 26th = `google/uuid`, genuinely unlinked). Note: spec §4 x/sys usage table predates T10 — `internal/ident/realpath_windows.go` is a third x/sys/windows consumer.
- All 9 packages GREEN (`go test -p 1 ./...`), 3-OS CI + crossbuild 6 targets GREEN.
- **v0.0.1 redefined contract: COMPLETE** — 6 MCP tools + ctr_global_search + 4 CLI. Gates 1–13 documented in `docs/gates-v0.0.1-ko.md`.

## 4. Carryovers (before v0.0.1 tag)

**Manual (user action required):**
- **Manual smoke gate 10** — Claude Code + Codex real registration (tools/list, call, cancellation). Procedure + result blanks in `docs/gates-v0.0.1-ko.md`. Platform-specific (darwin = 2 tools, no ctr_transform).
- **oracle LICENSE authorship** — NOTICE upstream copyright line ("Mert Koseoglu" from context-mode/LICENSE) vs package.json author mismatch — reconcile before release (T7 report).
- **Tag procedure:** PR merge → main CI GREEN → 2 manual smokes confirmed → `git tag v0.0.1 && git push origin v0.0.1`.

**Deferred (post-v0.0.1, documented in gates doc §"v0.0.1 이후"):**
- darwin RLIMIT_AS absent → ctr_transform fail-closed unregistered (no macOS isolation; needs real macOS or redesign).
- title dedup staleness (v0.1 source-level title schema).
- 5000-doc trigram FTS bottleneck (~1.6s query; informational, non-gating).
- per-directory case-sensitivity path test (CI admin-rights constraint).
- openat2 full TOCTOU (junction realpath done; openat2 deferred to §14).
- **Dependency follow-up PRs** (spec doc §10, separate from PR #2): P1 `go-readability` v0 → `/v2 v2.1.2` (API/output changes — needs fixture comparison); P2 `x/net v0.55.0→v0.57.0` + `x/sys v0.46.0→v0.47.0` maintenance bump (no security urgency — pinned versions already patched, govulncheck clean).
- Non-blocking minors rolled into ledger (T1 lock error double-wrap, clamp dup, etc.).

## 5. Standing protocols

- **Reviews = cross-model** (user directive, in CLAUDE.md): Claude subagent + Codex `review --base` in parallel at every checkpoint; re-reviews subagent-only. **Proof it works this session:** Codex found GC↔Register store-corruption race (T5), unix-`\` corruption + auth-root TOCTOU that Claude had approved (T9), gate over-PASS honesty (T10); Claude found junction half-application, snippet redaction, worker race. Neither model alone caught all.
- **Stuck → collaborate:** T1 blocked → Codex rescue resolved root cause. No blind retry.
- **SDD:** fresh subagent per task, BASE from ledger (`.superpowers/sdd/progress.md`), response-splitting discipline (32K cap deaths in earlier plans).
- **Canaries:** runtime split literals (conventions §8).
- **Test memory cap:** `-p 1`. Commit trailer: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

## 6. Next-session starting prompt (paste-ready)

```
context-router: finish the v0.0.1 tag. Resume context first:
1. Read root CLAUDE.md, docs/prompts/CLAUDE.md, and the latest record (docs/prompts/2026-07-18-session-02-plan3-global-cli.md).
2. Check git: is the Plan 3 PR (feat/v0.0.1-global-cli → main) merged? If not, confirm with me whether to merge.
3. Read docs/gates-v0.0.1-ko.md (gate 13 tag procedure + "v0.0.1 이후" deferrals).

Remaining before tag:
- Perform the 2 manual registration smokes (gate 10): Claude Code + Codex real tools/list + one call + cancellation. Record results in gates doc. (Platform note: darwin registers 2 tools, no ctr_transform.)
- Reconcile the NOTICE upstream copyright vs context-mode package.json author (T7 carryover).
- After PR merged + main CI GREEN + both smokes confirmed: git tag v0.0.1 && git push origin v0.0.1.

Then v0.1 planning (session events) or the deferred backlog (macOS transform isolation, title dedup, semantic retrieval) per my direction. Reviews stay cross-model (Claude subagent + Codex).

Model: keep Fable. Per my 2026-07-19 decision, keep security prose in your replies minimal/abstract (no dense enumeration of sensitive categories) to avoid Fable's dual-use refusal_fallback; route bulk security corpora/forensics through the sandbox with masking. See auto-memory `fable-security-prose-minimization`. ultrathink
```

> **PR #2 is merged (main 78a7927).** The §6 prompt above supersedes any "start Plan 3" framing — Plan 3 is complete; the next session finalizes the v0.0.1 tag.
