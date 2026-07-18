# Monitoring and Alerting

Orbit is instrumented for Prometheus. This document lists the exported metrics,
the health check surface, and the alerts that page an operator.

## Prometheus metrics endpoint

The gateway exposes Prometheus metrics at `/metrics` in the standard text
exposition format. Scrape it every 15 seconds. All metrics carry a `route`
label and, where relevant, an `upstream` label so dashboards can slice by
either. The Prometheus metrics endpoint itself is unauthenticated but bound to
the internal interface only.

## Key metrics

- `http_requests_total` — counter, labeled by route and status code.
- `http_request_duration_seconds` — latency histogram per route.
- `cache_hits_total` / `cache_misses_total` — response cache effectiveness.
- `cache_evictions_total` — labeled by reason (`capacity`, `ttl`).
- `ratelimit_rejections_total` — requests shed by the rate limiter.
- `pool_connections_in_use` / `pool_connections_idle` — connection pool depth.
- `pool_acquire_wait_seconds` — histogram of time spent waiting for a pooled
  connection; a rising tail here is the earliest sign of pool exhaustion.

## Health check

The health check endpoint `/healthz` is what humans and uptime checks hit; it
returns a small JSON body with per-subsystem status (config, pool, cache).
Unlike the Kubernetes readiness probe it never short-circuits, so it is safe
to poll from an external monitor. A red health check almost always maps to a
specific subsystem line in the body.

## Alerting rules

Alerts are defined as Prometheus recording and alerting rules:

- High error ratio: `http_requests_total{status=~"5.."}` over total above 1%
  for five minutes pages immediately.
- Pool exhaustion: `pool_acquire_wait_seconds` p99 above the acquire timeout
  warns; sustained means raise `pool_max` or replica count.
- Cache collapse: hit ratio below a floor warns that TTLs may be too short.
- Rate limiter storm: a spike in `ratelimit_rejections_total` flags either an
  abusive client or a misconfigured quota.

## Dashboards

The default Grafana dashboard groups panels by subsystem: edge (requests,
latency, status codes), cache (hit ratio, evictions), rate limiter, and
connection pool. Every panel links back to the metric name so an operator can
pivot from a graph to an ad-hoc query quickly.
