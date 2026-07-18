# Troubleshooting

A field guide to the failures operators see most often, what they look like in
the logs, and how to confirm the cause. Pair this with the Prometheus metrics
in `monitoring.md`.

## Reading a stack trace

An unhandled error is logged at `error` level with the request id and a full
stack trace. Start at the top frame — that is where the panic or error was
raised — and walk down until you reach the first frame in gateway code. The
request id in the stack trace line correlates with the `500` returned to the
client and with the access log entry for the same request.

## 500 spikes

A sudden rise in `500` responses is almost always one upstream. Filter the
error log by the `upstream` field, find the dominant one, and check whether
its circuit breaker is open. If the stack trace points into the connection
pool acquire path rather than into a handler, the real problem is pool
exhaustion, not the upstream itself.

## Connection pool exhaustion

When `pool_acquire_wait_seconds` climbs and requests fail with
`503 upstream unavailable`, the connection pool is exhausted: every pooled
connection is checked out and new callers time out waiting. Causes are a slow
upstream holding connections, a `pool_max` set too low, or a leak where a
handler borrows a connection and never returns it. Confirm with the in-use
versus idle connection metrics; a healthy pool always has idle capacity.

## Latency without errors

If latency rises but the error ratio is flat, look at the cache hit ratio
first. A drop in hits — often from TTLs set too short or an over-eager
invalidation — pushes load onto upstreams and shows up as latency long before
it shows up as errors.

## Rate limit false positives

Clients reporting unexpected `429` responses are usually sharing one client
identifier — for example many browser tabs behind one token. Confirm by
grouping `ratelimit_rejections_total` by client id; one id far above the rest
is a shared credential, not an attack.

## Cold start

Right after a rollout, latency is briefly high while each new pod fills its
connection pool and warms its response cache. This is expected and clears
within a scrape interval; do not react to a stack trace or `503` in the first
few seconds after a pod reports ready.
