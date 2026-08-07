---
name: ctr-usage
description: Use when a command's output, a web page, or a file is large enough that reading it whole would fill the context window - index it, search it, and fetch only the span that answers the question.
---

# Routing large payloads through the store

The store holds the bytes; the context window holds the answer.

1. **Put it in** — `ctr_index` takes a file, a directory, or inline text.
   `ctr_fetch_and_index` does the same for a URL.
2. **Find the part you need** — `ctr_search` runs BM25 over the index and returns
   snippets with their locators.
3. **Take exactly that part** — `ctr_fetch` returns a byte-exact range by chunk,
   line, or byte offset.

`ctr_transform` reshapes a stored artifact with a hermetic starlark script when the
answer needs computation rather than retrieval.

Session events (`ctr_record_event`, `ctr_session_summary`, `ctr_export_events`)
carry summaries plus pointers; large payloads belong in the store, referenced by id.
