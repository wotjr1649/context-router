# Security Model

This document describes how Orbit authenticates callers, protects secrets,
and hardens the edge. Security review sign-off is required before any change
to the files referenced here.

## JWT token verification

Every protected route requires a JWT bearer token in the `Authorization`
header. The gateway verifies the token signature against the identity
service's rotating public keys, checks the `exp` and `nbf` claims, and
rejects any token whose audience does not match the gateway's configured
audience. A JWT token that fails any of these checks is treated as
anonymous and the request is refused on protected routes. Verified claims
are forwarded to upstreams in a signed internal header.

## Password storage

The identity service stores passwords using bcrypt with a per-user salt and a
work factor of 12. A bcrypt password hash is never logged and never leaves
the identity service. On login the plaintext is compared against the stored
bcrypt hash in constant time; on success a short-lived JWT token plus a
refresh token are issued. Increasing the bcrypt work factor is a config
change that takes effect for new hashes on the next password rotation.

## CORS policy

Browser clients trigger a CORS preflight (`OPTIONS`) before any cross-origin
request that carries credentials or custom headers. The gateway answers the
CORS preflight from an allowlist of origins; a request from an origin that is
not on the allowlist receives no `Access-Control-Allow-Origin` header and the
browser blocks it. Preflight responses are cached by the browser for the
`Access-Control-Max-Age` we advertise. The CORS preflight path is exempt from
authentication so that the browser can complete the handshake.

## Secret rotation

All signing keys and upstream credentials are rotated on a fixed schedule.
Rotation is overlap-based: the new secret is published and accepted before the
old one is withdrawn, so there is no window where valid tokens are rejected.
Secrets are pulled from the secret manager at boot and refreshed in the
background; they are never written to disk or included in a stack trace.

## Transport security

The edge terminates TLS 1.3 only. Older protocol versions and weak ciphers
are disabled at the listener. Internal hops between the gateway and upstreams
use mutual TLS with certificates issued by the internal CA. HSTS is advertised
with a long max-age so browsers refuse to downgrade to plaintext.

## Input hardening

Request bodies are size-capped before they are read into memory. Header count
and header size are bounded. The gateway strips hop-by-hop headers and any
inbound copy of the internal claims header so a caller cannot forge identity.
