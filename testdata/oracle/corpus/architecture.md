# Orbit API Gateway — Architecture Overview

Orbit is an HTTP API gateway that sits in front of a fleet of internal
services. It terminates TLS, authenticates requests, applies rate limiting,
caches idempotent responses, and forwards everything else to upstream
services over a pooled connection layer. This document describes the major
components and how a request flows through them.

## Request lifecycle

A request enters through the edge listener and passes through an ordered
middleware chain before it ever reaches an upstream:

1. TLS termination and HTTP/2 negotiation.
2. Authentication — the auth middleware validates the JWT bearer token
   attached to the `Authorization` header and rejects anonymous traffic on
   protected routes.
3. Rate limiting — a per-client rate limiter using a sliding window counter
   decides whether the request is admitted or shed with `429`.
4. Response cache lookup — safe methods (`GET`, `HEAD`) consult the cache
   before any upstream call is made.
5. Upstream dispatch — the router selects a backend and borrows a live
   connection from the connection pool.

Each stage is independent and can be disabled per route through the route
configuration table.

## Authentication

The gateway does not own user identity. It verifies signed JWT tokens issued
by the identity service and trusts the claims inside once the signature
checks out. The auth middleware caches the identity service's public keys and
rotates them on a schedule. Password verification and token issuance live in
the identity service, not here; see `security.md` for the JWT token and
bcrypt password details that govern issuance.

## Rate limiting

Rate limiting protects upstreams from traffic spikes and abusive clients. The
gateway runs a sliding window rate limiter keyed on the client identifier
extracted from the token. When a client exceeds its quota the limiter returns
a `429 Too Many Requests` with a `Retry-After` header. The sliding window
algorithm smooths out the bursts that a fixed window would allow at the
boundary between two windows.

## Response caching

Idempotent responses are cached in an in-process LRU cache with a per-entry
TTL. Cache eviction happens both on TTL expiry and on capacity pressure —
the least recently used entry is dropped when the cache is full. Cache
invalidation is driven by upstream write events. The caching subsystem is
described in depth in `caching.md`.

## Connection pooling

Every upstream is fronted by a connection pool. Pooling avoids the cost of a
fresh TCP and TLS handshake on every request. The pool bounds the maximum
number of concurrent connections per upstream, enforces an acquire timeout,
and runs a periodic health check on idle connections so that dead sockets are
discarded before they are handed to a caller.

## Observability

The gateway exposes Prometheus metrics at `/metrics`: request counts, latency
histograms, cache hit ratio, rate limiter rejections, and connection pool
saturation. A health check endpoint at `/healthz` reports readiness. Traces
are emitted for every request and sampled at the edge. See `monitoring.md`
for the full list of Prometheus metrics and alerting rules.

## Failure handling

Upstream failures are isolated per route with a circuit breaker. When an
upstream trips its breaker the gateway serves a cached response if one is
available, otherwise it returns `503`. Panics in a handler are recovered by
the outermost middleware, logged with a full stack trace, and turned into a
`500` so a single bad request cannot take down the process.
