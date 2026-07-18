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

Build from source (no tagged release yet):

```sh
go build -o context-router ./cmd/context-router
```

Then run:

```sh
./context-router doctor
```

`doctor` diagnoses the store root, DB, and FTS5 setup for the current
project, and prints ready-to-use host registration snippets (e.g. for Claude
Code's `.mcp.json` / `claude mcp add`, or Codex's `~/.codex/config.toml`),
including recommended default permission rules (`ctr_index` /
`ctr_fetch_and_index` and any `global-search`-profile tool default to "ask").

## CLI

```text
context-router                      # run as an MCP server (flags below)
context-router doctor               # diagnose store/DB/FTS5 + host registration snippets
context-router stats [--provider <transcript-path>]
context-router purge (--project <id|path> | --all) [--older-than <dur>] [--gc]
context-router upgrade               # print current + latest release version
```

## Server flags

| Flag | Default | Purpose |
|---|---|---|
| `--root <path>` | cwd | project root |
| `--store-root <path>` | OS-default store dir | override storage location (env `CTR_STORE_ROOT`) |
| `--profile <list>` | `search,fetch,transform` | tool profile; `global-search` for cross-project search |
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
