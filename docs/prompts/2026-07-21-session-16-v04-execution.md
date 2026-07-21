# Session 16 — v0.4 execution: SDD Task 0~8, dual final review, CI, merge, v0.4.0

- **Span:** 2026-07-21
- **Model:** Fable 5 (ultrathink). Subagents (opus, explicit `model:`): 7 implementers,
  7 task reviewers, 1 final whole-branch reviewer, 3 fix agents, 2 re-reviewers.
  Codex: 1 `review --base` pass (final checkpoint — consumed).
- **One line:** Executed the v0.4 plan end-to-end via subagent-driven-development on
  `feat/v0.4-channel-expansion` (BASE `3a673dc`) — Tasks 1~7 all Approved without a
  fix round; Task 8 controller work (bump 0.4.0 → reinstall → real-host smoke 4/4 →
  dual final review → Codex P2 `CODEX_HOME` adopted+fixed → CI 2-fix wave) → PR #13 →
  3-OS GREEN → merge `756670d` → tag **v0.4.0**.

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router v0.4 구현을 진행한다. 먼저 docs/prompts/2026-07-21-session-15-v04-plan-dual-review.md(최신 기록)를 정독한다. 상태: main에 v0.4 구현 계획(docs/superpowers/plans/2026-07-21-v04-channel-expansion.md — 관측 프리체크 §11.1·이중 적대 검수 §11.2 반영 완료, 8f37e84) 커밋됨, 도그푸딩 훅 marker 0.3.0.
1. superpowers:subagent-driven-development로 계획 Task 0~8을 실행한다 — feat/v0.4-channel-expansion 브랜치(BASE = main HEAD, SDD 원장에 해시 기록), 태스크당 신선한 서브에이전트(opus — Agent 호출에 model: "opus" 명시) + 태스크 리뷰 → fix → 재리뷰. 순서 제약: Task 4를 Task 2·3보다 먼저 실행 금지. 서브에이전트 응답 분할 규율(긴 테스트 데이터는 strings.Repeat/testdata) 준수.
2. 최종 리뷰 체크포인트: 서브에이전트 + Codex review --base <BASE> 병렬 1패스 → 발견 병합·반영(재리뷰는 서브에이전트만, Codex는 체크포인트당 1패스).
3. Task 8: 버전 0.4.0 범프 → 바이너리 재빌드·재설치(claude marker 0.4.0 + codex hooks.json) → 실호스트 스모크 4종(PS deny/통과, codex cx: 등록 — Codex /hooks 신뢰 승인 필요, doctor [14] 경고 무발화) → PR → CI 3-OS GREEN → 머지 → v0.4.0 태그 → session-16 기록.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/ 보호). Fable 유지, 보안 서술 최소화 준수. ultrathink
```

## 2. What was done

**1) SDD Tasks 0~7** (branch `feat/v0.4-channel-expansion`, BASE `3a673dc`; every
implementer+reviewer opus with explicit `model:`; ordering constraint honored —
Task 4 after 2·3; all 7 task reviews Approved with zero fix rounds):
- Task 0 verify-only: design §11.1 exists with the 3 adoptions (G1 row 1 / G2
  matcher / G3 hooks.json). Task 1 `b052e6e` psDumpArg·psAbsPath. Task 2 `7feb8d2`
  guardPowerShell + matcher `Read|Bash|PowerShell` (.go-file migration verified
  complete). Task 3 `843c80c` D39 commandDumpPath (reviewer independently proved
  Clean-normalization is load-bearing vs `DeniedFilename`). Task 4 `e554372` Host
  cc/cx + `codex-hook` subcommand + same-UUID isolation (reviewer verified sibling
  fixture UUID premise + all Run( call sites). Task 5 `60a36ee` Codex hooks.json
  merger (ALL-entries ownership traced both-directions conservative; byte
  idempotence real). Task 6 `eaebba7` D37 kind-tier two-query swap (coexist
  contract proven non-coincidental). Task 7 `d561d43` D38 doctor [14] warning.

**2) Task 8 controller work:**
- Bump `a2c8d0b` (0.4.0 ×2 constants), full gate GREEN (11 pkg `-p 1`), gofumpt/vet
  clean. `go install` + `hook install` (marker 0.4.0, doctor [9] resolved) +
  `hook install --codex` (project `.codex/hooks.json`, 2 events, trust-advice line).
- **Real-host smoke 4/4 PASS:** ① headless PS `Get-Content` 348KB → deny + warning
  `PowerShell: Get-Content smoke-ps-big.txt 348894B — ctr_search/ctr_fetch` (relative
  path only) + on-the-spot indexing (artifact searchable). ② `-TotalCount`/pipe pass
  (warning count stayed 1). ③ real `codex exec --dangerously-bypass-hook-trust` →
  `cx:` sessions+events recorded from the first run (project `.codex` layer fires;
  live mixed-group foreign preservation + byte-idempotent reinstall also observed).
  ④ [14] 12.1MB < 100MiB, no warning.
- **Smoke ③ detour (lesson):** `session export` JSONL uses **camelCase**
  (`sessionId`/`eventType`); grepping DB column names (`session_id`) produced false
  negatives that mimicked "hooks not firing". Also: bash heredoc collapsed `\\` in a
  hand-written probe hooks.json → invalid JSON → codex startup hang; JSON probes must
  be written with the Write tool.

**3) Final dual review (base `3a673dc`):**
- Subagent (opus): Ready-to-merge YES, C0/I0/M4; carryover-minors triage → zero
  must-fix (T3/T5 → v0.5, rest ACCEPT). Verified version-gate exit-1 path, cc/cx
  ExternalSessionID isolation, D39 bypass closure.
- Codex: **P2 ×1 — `hook install --codex --user` ignored `CODEX_HOME`** (silent
  mis-install under custom home). Validated against official env-vars doc → adopted.
  Fix `2c505b3` (non-empty honor / empty fallback; default path byte-identical);
  re-review (subagent-only) approved.
- Not adopted now (user decision / docs): `CTR_SHADOW_WARN_BYTES` naming (design D38
  mandates the name), 100MB↔100MiB wording, upgrade-order doc note.

**4) CI wave:** first 3-OS run FAILED — F1 deterministic (ubuntu+macos):
`psAbsPath` windows branch used host-dependent `filepath.ToSlash` → replaced with
goos-keyed `strings.ReplaceAll` (Windows byte-identical; same class as v0.3 F1 —
the 3-OS gate doing its §8 job). F2 flake (slow windows runner): isolation test's
big shadow capture blew the 2s `CTR_HOOK_DEADLINE_MS` default → test now injects
60000ms via the helpers' env map (assertions unchanged, production default
untouched). Fix `1fa7c67`, re-review approved, CI rerun **all GREEN** (both runs).

**5) Merge:** PR #13 → merge commit `756670d`, branch deleted, tag **v0.4.0**
pushed. 15 files +1020/−36.

## 3. Current repo state

- `main` @ `756670d` (merge of PR #13) + this docs commit; tag `v0.4.0`. No open
  PRs, no feature branches.
- Dogfooding hooks live at **marker 0.4.0**, matcher `Read|Bash|PowerShell` (this
  session's own hooks still run the 0.3.0-era snapshot until restart). Codex
  project hooks installed (`<repo>/.codex/hooks.json`, untracked) — **trusted only
  via bypass flag so far; interactive `/hooks` approval is the user's step**.
- Untracked (intentional): `.claude/`, `.codex/`, `context-router.exe`,
  `ctr-new.exe`, `smoke-ps-big.txt` (delete-guard — manual cleanup candidates).
  Synthetic `cx:aaaa1111…` events (2) in session.db from manual probes (harmless).
  `C:\tmp` probe files ×3.

## 4. Carryovers / next work

1. **User decisions:** ① rename `CTR_SHADOW_WARN_BYTES` → e.g. `CTR_STORE_WARN_BYTES`
   (design D38 names it; new knob, zero compat cost — cheapest before release notes)
   ② Codex `/hooks` interactive trust approval for the project hooks (then dogfood
   A/B with real Codex sessions).
2. **v0.5 follow-up bundle (code):** buildCodexHookCommand quoting verified against
   real Codex tokenization + `uninstall --codex` run-branch e2e + isOurCodexGroup
   direct edge assertions (+ optional storeWarnBytes non-positive case).
3. **Docs wave:** design 100MB→100MiB wording; upgrade-order note (replace binary
   before `install --codex`); comment nuances (commandDumpPath "이름 기반",
   TestPsAbsPath:1425 inline). Plan-deviation dispositions (CODEX_HOME / ReplaceAll /
   deadline-env) could be appended to design §11 if v0.5 wants the canonical trail.
4. Manual cleanup of untracked binaries/smoke file; old `~/.codex/config.toml`
   `mcp_servers.ctr` entry still points at stale repo-root `ctr.exe` (v0.0.1-era).
5. Dogfood A/B continues (hooks:on heavy sessions; [14] growth watch).

## 5. Standing protocols (delta only — rest as session-15 §5)

- **EventV1 JSONL is camelCase** (`sessionId`, `eventType`) — never grep DB column
  names against `session export` output.
- **Codex project-layer hooks verified live**: `<repo>/.codex/hooks.json` fires under
  `codex exec --dangerously-bypass-hook-trust` when the project is trusted; trust
  hashes live per-entry in `config.toml [hooks.state]` (user file only until /hooks
  approval). `CODEX_HOME` overrides `~/.codex` (now honored by `--user` install).
- **JSON probes via Write tool only** — bash heredoc mangles backslash escapes into
  invalid JSON (codex hangs at startup on a malformed hooks.json layer).
- **goos-parameterized helpers must not call host-dependent stdlib** (filepath.ToSlash
  class) — 3-OS CI catches it; normalize keyed to the goos argument.
- Codex review budget: 1 pass per checkpoint, consumed at final. Re-reviews
  subagent-only.

## 6. Next-session starting prompt (paste-ready)

```
context-router v0.4 릴리스 후속을 진행한다. 먼저 docs/prompts/2026-07-21-session-16-v04-execution.md(최신 기록)를 정독한다. 상태: main 756670d(+docs), tag v0.4.0, 도그푸딩 marker 0.4.0(Claude), Codex 프로젝트 훅 설치됨(대화형 /hooks 신뢰 승인은 아직).
1. 사용자 결정 2건을 먼저 확정한다: ① CTR_SHADOW_WARN_BYTES 개명 여부(설계 D38 문면 — 릴리스 노트 전이 최저비용) ② Codex /hooks 신뢰 승인 후 실세션 A/B 개시 여부.
2. 결정에 따라 v0.5 follow-up 웨이브(Codex 설치 인용 실측·uninstall e2e·isOurCodexGroup 직접 단정 번들 + 문서 웨이브: 100MiB 표기·업그레이드 순서 노트·주석 뉘앙스 2건)를 superpowers 프로세스로 실행하거나, v0.5 신규 범위 브레인스토밍으로 진행한다.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/·.codex/ 보호). Fable 유지, 보안 서술 최소화 준수.
```
