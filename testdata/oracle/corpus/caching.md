# Response Caching

Orbit caches idempotent upstream responses in an in-process store to cut
latency and shield upstreams from repeated identical work. This document
describes the cache data structure, its eviction rules, and how entries are
invalidated.

## Cache key

The cache key is derived from the request method, the normalized path, the
`Vary` headers the upstream declared, and the authenticated tenant id. Two
tenants never share a cache entry even when the path is identical. Only `GET`
and `HEAD` responses are eligible; any response carrying `Cache-Control:
no-store` is skipped.

## LRU eviction

The store is a bounded LRU cache. Every read moves the entry to the most
recently used position; when the cache is at capacity a write drops the least
recently used entry first. LRU cache eviction keeps the working set resident
while letting cold entries fall out under memory pressure. The capacity is a
hard bound on the number of entries, configured by `cache.max_entries`.

## TTL expiry

Independently of the LRU bound, each entry carries a time-to-live. A cache
TTL is taken from the upstream `Cache-Control: max-age` when present and
otherwise falls back to the route default. Cache eviction on TTL expiry is
lazy: an expired entry is detected and dropped on the next read rather than by
a background sweeper, so an entry that is never read again simply ages out
when the LRU bound reclaims it. The default cache TTL is configured by
`cache.default_ttl_seconds`.

## Invalidation

Beyond TTL, entries are invalidated explicitly when an upstream reports a
write. The gateway subscribes to upstream change events and evicts every
cache entry whose key prefix matches the changed resource. Cache invalidation
is best-effort: if the event bus drops a message the stale entry still ages
out under its TTL, so invalidation is a latency optimization rather than a
correctness guarantee.

## Stampede protection

When a popular entry expires, many concurrent requests could all miss and
hammer the upstream at once. The cache uses single-flight coalescing: the
first miss for a key acquires a lock, fetches from the upstream, and fills the
cache; concurrent misses for the same key wait for that fill instead of each
issuing their own upstream call.

## Metrics

The cache reports hit ratio, entry count, eviction count split by reason
(capacity versus TTL), and single-flight wait time. These feed the Prometheus
metrics documented in `monitoring.md`.
