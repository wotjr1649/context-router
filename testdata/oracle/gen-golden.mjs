// gen-golden.mjs — one-shot generator for the gate-1 oracle equivalence golden.
//
// Drives the *reference* implementation (ctxscribe 1.3.0 / context-mode) exactly
// as its ctx_index + ctx_search(sort:"relevance") tools do, over the shared
// corpus, and writes the top-k source-file list per query to golden.json.
//
// Reference paths (server.ts):
//   - ctx_index(content, source) -> ContentStore.index({content, source})
//       -> #chunkMarkdown() for ALL content (regardless of extension).
//   - ctx_search(queries, limit) with sort="relevance" (default)
//       -> searchAllSources() -> store.searchWithFallback(query, limit,
//          undefined, undefined, "like") — RRF(porter OR + trigram OR) +
//          proximity rerank, with a fuzzy-correction fallback.
//
// Determinism: files are indexed in sorted order; the reference search is a
// pure function of the corpus + query. Re-running reproduces golden.json byte
// for byte. This is a MANUAL, committed artifact — the Go equivalence test
// reads golden.json, it does NOT run this script.
//
// Usage (run from the reference repo so better-sqlite3 + build/store.js resolve):
//   cd C:/Users/js/Documents/ClaudeCode/context-mode
//   node <ctr>/testdata/oracle/gen-golden.mjs
//
// Override the reference build with CTX_ORACLE_STORE (absolute path to its
// compiled build/store.js) if the repo lives elsewhere.

import { readFileSync, writeFileSync, readdirSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";
import { dirname, join, basename } from "node:path";

const HERE = dirname(fileURLToPath(import.meta.url));
const CORPUS_DIR = join(HERE, "corpus");
const QUERIES_FILE = join(HERE, "queries.json");
const OUT_FILE = join(HERE, "golden.json");

const ORACLE_STORE =
  process.env.CTX_ORACLE_STORE ||
  "C:/Users/js/Documents/ClaudeCode/context-mode/build/store.js";

// oracle 버전 검증: 참조 저장소 package.json에서 실측 버전을 읽어 1.3.0이 아니면 생성 거부
// (다른 버전으로 만든 골든이 조용히 커밋되는 것을 막는다). 실측 버전을 golden에 기록한다.
const EXPECTED_VERSION = "1.3.0";
const ORACLE_ROOT = dirname(dirname(ORACLE_STORE));
const { version: ORACLE_VERSION } = JSON.parse(
  readFileSync(join(ORACLE_ROOT, "package.json"), "utf-8"),
);
if (ORACLE_VERSION !== EXPECTED_VERSION) {
  throw new Error(
    `oracle 버전 불일치: ${ORACLE_ROOT}/package.json version=${ORACLE_VERSION}, ` +
      `기대=${EXPECTED_VERSION} — golden 생성 거부.`,
  );
}

const { ContentStore } = await import(pathToFileURL(ORACLE_STORE).href);

const { queries, oracle_k: K } = JSON.parse(readFileSync(QUERIES_FILE, "utf-8"));

const store = new ContentStore(":memory:");

// Index every corpus file in sorted order for determinism. source = basename,
// matching the basename normalization the Go equivalence test applies to
// ctr's project-relative Hit.Source.
const files = readdirSync(CORPUS_DIR).sort();
for (const f of files) {
  const content = readFileSync(join(CORPUS_DIR, f), "utf-8");
  store.index({ content, source: f });
}

// For each query take the reference top-K results and collect the distinct
// source files in rank order (the "top-3 source set" of the equivalence rule).
const results = {};
for (const q of queries) {
  const hits = store.searchWithFallback(q, K, undefined, undefined, "like");
  const seen = [];
  for (const h of hits) {
    const src = basename(h.source);
    if (!seen.includes(src)) seen.push(src);
  }
  results[q] = seen;
}

store.close?.();

const golden = {
  generated_by: "testdata/oracle/gen-golden.mjs",
  oracle: "ctxscribe / context-mode",
  oracle_version: ORACLE_VERSION,
  k: K,
  corpus_files: files,
  results,
};

writeFileSync(OUT_FILE, JSON.stringify(golden, null, 2) + "\n", "utf-8");
console.log(`wrote ${OUT_FILE}`);
for (const q of queries) console.log(`  ${q} -> ${results[q].join(", ")}`);
