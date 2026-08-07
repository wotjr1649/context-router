# context-router

`context-router` is a local-first, single-binary Go MCP server that keeps
large tool outputs and files out of the model's context window. It stores
tool output and indexed files in a per-project SQLite database (FTS5 search,
byte-exact fetch of stored artifacts), and adds a hermetic Starlark transform
sandbox and an SSRF-safe web fetch + index pipeline. All tools it exposes are
named `ctr_*`.

> This project is an independent Go implementation informed by the ctxscribe
> tool contract; not affiliated with or endorsed by the upstream authors.

See `NOTICE` for the full upstream/third-party attribution and `LICENSE` for
license terms.

## Tool name mapping

`context-router` reimplements the tool contract observed in ctxscribe /
context-mode (the reference "oracle") under new `ctr_*` names, to avoid any
confusion with an installed ctxscribe plugin and to signal an independent
reimplementation. Some tools are renamed with a redefined contract, some are
new, and some are deferred:

| ctxscribe (oracle) | context-router | Note |
|---|---|---|
| `ctx_search` | `ctr_search` | |
| `ctx_fetch` | `ctr_fetch` | contract redefined — oracle's `fetch` is a web fetch; `ctr_fetch` is a byte-exact read of a stored artifact |
| `ctx_execute` | — | reserved for v0.2 (exec surface deferred by design) |
| `ctx_index` | `ctr_index` | |
| `ctx_fetch_and_index` | `ctr_fetch_and_index` | |
| — | `ctr_transform` | new: hermetic Starlark transform |
| — | `ctr_global_search` | new: cross-project search (requires `--profile global-search`) |
| `ctx_stats` / `ctx_doctor` / `ctx_upgrade` / `ctx_purge` | CLI: `stats` / `doctor` / `upgrade` / `purge` | exposed as CLI subcommands, not MCP tools |

Precise field-level contract mapping (request/response shapes) is tracked
separately in `docs/oracle-mapping-ko.md`.

## Install & host registration

`context-router` installs as a Claude Code / Codex plugin — the host reads
the plugin manifest and registers the MCP server itself. This program does
not write to `.mcp.json`, `settings.json`, or `config.toml`.

**0. Remove any existing hand-written registration.** Both hosts silently
discard a plugin-declared MCP server when its `command` + `args` already
match a registered one — no warning, no error; the symptom is that the
plugin appears to do nothing. Remove old registrations (from a prior
version of this tool, or a manual `mcp add`) first:

```sh
claude mcp remove <name>
codex mcp remove <name>
```

`codex mcp remove` exits 0 even when `<name>` does not exist, so the exit
code cannot confirm removal — check with `mcp list` that the entry is
actually gone.

**1. Get the binary:**

```sh
go install github.com/wotjr1649/context-router/cmd/context-router@latest
```

or build locally with `go build -o context-router ./cmd/context-router`. The
plugin manifest launches the bare `context-router` command, so the binary
needs to resolve on `PATH`.

**2. Install the plugin:**

```sh
# Claude Code
claude plugin marketplace add wotjr1649/context-router
claude plugin install context-router --scope <user|project|local>

# Codex
codex plugin marketplace add wotjr1649/context-router
codex plugin add context-router
```

**3. Verify.** `mcp list` shows the server, in each host's own format:
Claude Code as `plugin:context-router:ctr … Connected`, Codex as
`ctr … enabled`.

`ingest` and `net` are enabled by default; set `CTR_ENABLE` to a
comma-separated list of `ingest` / `net` / `exec` to change that (e.g.
`CTR_ENABLE=ingest,net,exec`) — the plugin manifest passes no `--enable`
flag, so this environment variable is what a plugin-managed install reads.
A `--enable` flag, if you invoke the binary directly, always wins over
`CTR_ENABLE`.

`context-router doctor` diagnoses the store root, DB, and FTS5 setup for the
current project, and prints the steps above together with the current tool
prefix (`mcp__plugin_context-router_ctr__`) and a sample permission rule for
`ctr_index` / `ctr_fetch_and_index` (both default to "ask").

## CLI

```text
context-router                      # run as an MCP server (flags below)
context-router doctor               # diagnose store/DB/FTS5 + host registration snippets
context-router stats [--provider <transcript-path>]
context-router purge (--project <id|path> | --all) [--older-than <dur>] [--gc] [--force]
context-router upgrade               # print current + latest release version
```

`purge` always asks for confirmation before deleting: in a TTY it shows the
target project slug and requires you to type that exact slug back; in a
non-TTY (automation) context it refuses unless `--force` is passed.

## Server flags

| Flag | Default | Purpose |
|---|---|---|
| `--root <path>` | cwd | project root |
| `--store-root <path>` | OS-default store dir | override storage location (env `CTR_STORE_ROOT`) |
| `--profile <list>` | `search,fetch,transform` | v0.0.1 accepts only the default 3-tool profile or `global-search` alone — arbitrary subsets are rejected at startup (tool gating by profile is reserved for a later version) |
| `--enable <list>` | (none) | opt-in extra capabilities: `ingest`, `net` |
| `--allow-path <path>` | (none) | extra `ctr_index` allowed root (repeatable) |
| `--projects <list>` | (none) | project allowlist, required with `--profile global-search` |
| `--net-allow-local` | off | allow `127.0.0.1`/`::1` destinations for `ctr_fetch_and_index` |
| `--net-ports <list>` | (none) | extra allowed ports for `ctr_fetch_and_index` |
| `--log-level <lvl>` | `info` | log level |

## License

`context-router` is licensed under the Elastic License 2.0 — see `LICENSE`
for the full text. Third-party dependency licenses and upstream attribution
are listed in `NOTICE`.
