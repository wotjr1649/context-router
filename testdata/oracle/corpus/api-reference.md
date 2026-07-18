# API Reference

This reference documents the public HTTP surface of the Orbit gateway: the
authentication contract, common headers, error codes, pagination, and the
CORS preflight behavior clients must account for.

## Authentication

All endpoints except `/healthz` and CORS preflight require a valid JWT token.
Send it as `Authorization: Bearer <jwt token>`. A missing or invalid JWT
token yields `401 Unauthorized`. A valid token whose claims lack the required
scope yields `403 Forbidden`.

## CORS preflight

Cross-origin browser callers must handle the CORS preflight. For any request
with a non-simple method or custom header the browser first sends an
`OPTIONS` request; the gateway answers the CORS preflight with the allowed
methods, headers, and max-age. Preflight requests never carry a body and are
not authenticated.

## Rate limit headers

Every response includes rate limiter state so clients can self-throttle:

- `X-RateLimit-Limit` — the quota for the current window.
- `X-RateLimit-Remaining` — requests left in the window.
- `X-RateLimit-Reset` — seconds until the window rolls over.

When the rate limit is exceeded the gateway returns `429 Too Many Requests`
with a `Retry-After` header. Clients should honor `Retry-After` rather than
retrying immediately.

## Pagination

List endpoints are cursor-paginated. Pass `?limit=` to bound the page size
(capped at 200) and `?cursor=` to continue. The response body carries a
`next_cursor` field that is null on the final page. Cursors are opaque and
must not be constructed by the client.

## Error format

Errors share one JSON shape:

```json
{ "error": { "code": "rate_limited", "message": "quota exceeded", "request_id": "..." } }
```

The `code` field is a stable machine-readable string. The `request_id`
correlates the error with the server logs and the stack trace recorded there.

## Status codes

- `200` / `201` — success.
- `304` — cache validators matched; body omitted.
- `400` — malformed request.
- `401` / `403` — authentication or authorization failure.
- `429` — rate limit exceeded.
- `500` — unhandled server error; a stack trace is logged with the request id.
- `503` — upstream unavailable or circuit breaker open.
