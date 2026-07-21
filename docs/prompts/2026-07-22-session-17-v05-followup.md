# Session 17 — v0.5 follow-up wave: user decisions ×3, rename+tests+실측, dual review, CI flake fix, merge

- **Span:** 2026-07-22 (one machine-reboot interruption mid-session)
- **Model:** Fable 5. Subagents (opus, explicit `model:`): 1 bundled implementer
  (Tasks 1·2·4 — resumed from transcript after the reboot), 1 final whole-branch
  reviewer. Codex: 1 `review --base` pass (final checkpoint — consumed, zero findings).
- **One line:** Confirmed the 3 pending user decisions (① rename ② Codex /hooks
  approval + A/B start ③ follow-up wave), executed the wave on
  `feat/v0.5-followup-wave` (BASE `f2d9bed`) with a bundled implementer + controller
  실측, dual final review C0/I0 + Codex zero findings, one F2-class CI flake fix →
  PR #14 → 3-OS GREEN → merge **`d4c204b`**.

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router v0.4 릴리스 후속을 진행한다. 먼저 docs/prompts/2026-07-21-session-16-v04-execution.md(최신 기록)를 정독한다. 상태: main 756670d(+docs), tag v0.4.0, 도그푸딩 marker 0.4.0(Claude), Codex 프로젝트 훅 설치됨(대화형 /hooks 신뢰 승인은 아직).
1. 사용자 결정 2건을 먼저 확정한다: ① CTR_SHADOW_WARN_BYTES 개명 여부(설계 D38 문면 — 릴리스 노트 전이 최저비용) ② Codex /hooks 신뢰 승인 후 실세션 A/B 개시 여부.
2. 결정에 따라 v0.5 follow-up 웨이브(Codex 설치 인용 실측·uninstall e2e·isOurCodexGroup 직접 단정 번들 + 문서 웨이브: 100MiB 표기·업그레이드 순서 노트·주석 뉘앙스 2건)를 superpowers 프로세스로 실행하거나, v0.5 신규 범위 브레인스토밍으로 진행한다.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/·.codex/ 보호). Fable 유지, 보안 서술 최소화 준수.
```

## 2. What was done

**1) User decisions (AskUserQuestion, all 3 confirmed):** ① rename to
`CTR_STORE_WARN_BYTES` (no alias — zero-compat-cost window) ② /hooks interactive
trust approval now + real-session A/B start ③ execute the v0.5 follow-up wave.

**2) Wave execution** (plan `docs/superpowers/plans/2026-07-22-v05-followup-wave.md`;
bundled implementer for Tasks 1·2·4 per v0.3 follow-up precedent, controller Task 3):
- `b67012f` Task 1: rename (cli.go ×3 + test + design D38 both lines incl.
  100MB→100MiB) + `TestStoreWarnBytes` direct adoption-rule assertions with
  **key-checking fake getenv** (real TDD RED captured).
- `b472721` Task 2: `TestIsOurCodexGroupEdges` 8 disjoint edges (incl. exact-match
  vs prefix-match discrimination) + `uninstall --codex` run-branch e2e ×2 —
  production untouched, all green on existing behavior.
- `20a022f` Task 4: design §2 upgrade-order bullet (binary before `install
  --codex`, version-gate rationale) + `commandDumpPath` suffix-rule comment +
  `TestPsAbsPath` stale "ToSlash" comment.
- `e076c9a` Task 3 (controller 실측): **quoting VALID** — spaced `--store-root`
  single-quoted by install, real `codex exec` fired SessionStart, `session.db`
  created exactly under the spaced store (`cx:019f8624` session_start, no mangled
  path fragments) → comment hedge "(가정 — 실측 전)" removed. **Codex tokenizes
  hook commands with POSIX rules** (T11 convention holds for Codex, Windows real host).
- `5c88ece` plan doc committed.

**3) Decision ② verification:** after user's interactive `/hooks` approval —
`config.toml [hooks.state]` has `trusted_hash` for both project entries
(session_start·post_tool_use) + project `trust_level="trusted"`; `codex exec`
**without** bypass flag → cx: sessions 4→5 (`cx:019f8628` session_start). Real-session
A/B dogfooding is live.

**4) Dual final review (base `f2d9bed`, head `5c88ece`):** subagent (opus) Ready
to merge YES — C0/I0/M3 (all observational: old-name breadcrumbs are intentional;
plan's grep-0 check was over-literal spec, implementer resolution sound;
TestStoreWarnBytes table mild duplication defensively fine). Codex: **zero
findings** ("개명이 코드·문서·테스트에 일관 반영, 회귀 결함 없음"). No fix wave.

**5) CI wave:** first run 11/12 pass, windows 1-run FAIL `TestGuardBashLargeFileDenies`
(deny stdout empty; sibling run passed) — diagnosed as **v0.4 CI F2 same class**
(slow runner blows default 2s `CTR_HOOK_DEADLINE_MS` during 300KB on-the-spot
indexing → fail-open drop; latent pre-existing flake, wave touched only a comment
in that file). Controller fix `93c3e9b`: F2 prescription (test env 60000ms) on both
LargeFileDenies (Bash fired + PS same structure); allow-tests immune (empty ==
expected). Rerun **all GREEN both runs**.

**6) Merge:** PR #14 → merge commit `d4c204b`, branch deleted, 8 files +473/−13.
No version bump, no new tag (not a release wave). Post-merge `go install
./cmd/context-router` — dogfooding binary on main tip ([9] marker 0.4.0, [14]
14.0MB no warning, new env name active).

**7) INFRA incident (mid-session):** first implementer dispatch died with the
session when the machine hit **commit-charge exhaustion** — this machine has **no
pagefile** (commit limit = physical RAM only); `explorer.exe` was leaking 6.25GB;
codex and even the dogfooding hook OOM'd at startup (Rust "memory allocation
failed" / Go "cannot allocate memory") despite 6GB free physical RAM. After the
user's reboot the same agent was resumed from transcript (ledger+git confirmed
zero partial work). Recorded in auto-memory (`no-pagefile-commit-exhaustion`).

## 3. Current repo state

- `main` @ `d4c204b` (merge of PR #14) + this docs commit; latest tag remains
  `v0.4.0`. No open PRs; no feature branches (only stale v0.0.1-era locals).
- Dogfooding: Claude hooks marker 0.4.0 (binary = post-merge main build). Codex
  project hooks **trusted via /hooks** — real-session A/B live, cx: events
  accumulating in the default store ([14] currently 14.0MB).

## 4. Carryovers / next work

1. **v0.5 신규 범위 브레인스토밍** — the main next work (design §10 의도적 미결
   후보: shadow 귀속 바이트 집계, hook 전용 purge 등; A/B cx: 데이터가 소재).
2. Later cleanup wave candidates (final-review minors, non-blocking): remove the
   two old-name breadcrumbs (design D38 line, cli.go:36) after a migration cycle.
3. **Machine (user decision):** enable a system-managed pagefile — root fix for
   the recurring commit-exhaustion OOM class.
4. Manual cleanup (delete-guard — user's step): repo-root `ctr.exe` + stale
   `config.toml` `mcp_servers.ctr` entry (v0.0.1-era); scratch `ctr quote probe\`
   dirs + its `trust_level` entry in config.toml; `C:\tmp` probe files ×3;
   synthetic `cx:aaaa1111…` events (harmless).
5. Dogfood A/B observation continues ([14] growth watch; doctor warning now keyed
   to `CTR_STORE_WARN_BYTES`).

## 5. Standing protocols (delta only — rest as session-16 §5)

- **Child-process OOM ≠ code bug by default**: on this no-pagefile machine, check
  commit charge first (`Win32_OperatingSystem.FreeVirtualMemory`); serialize
  heavy processes (go builds, codex exec, subagent toolchains).
- **Codex hook-command tokenization = POSIX rules** (2026-07-22 실측) — single-quote
  quoting valid for spaced `--store-root`; T11 convention generalizes.
- **Deny-asserting tests that do on-the-spot indexing must inject
  `CTR_HOOK_DEADLINE_MS` (60000ms)** — F2 prescription generalized beyond the
  isolation test; allow-asserting tests are deadline-immune.
- `session export` requires `--project`; JSONL is camelCase (session-16 승계).
- Codex review budget: 1 pass per checkpoint, consumed at final (zero findings).

## 6. Next-session starting prompt (paste-ready)

```
context-router v0.5 신규 범위 브레인스토밍을 진행한다. 먼저 docs/prompts/2026-07-22-session-17-v05-followup.md(최신 기록)를 정독한다. 상태: main d4c204b(+docs), tag v0.4.0(신규 태그 없음 — follow-up은 릴리스 아님), 도그푸딩 Claude marker 0.4.0 + Codex 실세션 A/B 가동 중(cx: 축적).
1. superpowers:brainstorming으로 v0.5 신규 범위를 탐색한다 — 소재: 설계 §10 의도적 미결(shadow 귀속 바이트 집계·hook 전용 purge·[14] 경고 실발화 시 sweep 재상정), A/B 도그푸딩에서 축적된 cx: 데이터 활용처, 구명 breadcrumb 정리 웨이브 편승 여부. 산출은 설계 정본 델타(docs/context-router-design-v0.5-ko.md)로.
2. 설계 확정 시 이중 적대 검수(서브에이전트 + Codex adversarial-review 인-레포 포커스) 후 writing-plans로.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지(untracked .claude/·.codex/ 보호). Fable 유지, 보안 서술 최소화 준수. 페이지파일 활성화 여부(캐리오버 3)를 세션 초에 확인.
```
