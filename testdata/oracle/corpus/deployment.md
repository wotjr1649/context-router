# Deployment

Orbit ships as a single static binary in a distroless container. This document
covers how it is rolled out on Kubernetes, how it drains connections on
shutdown, and how the platform decides an instance is healthy.

## Kubernetes rollout

The gateway runs as a Deployment behind a Service. Rollouts use the default
rolling update strategy with `maxUnavailable: 0` and `maxSurge: 1`, so a new
pod must become ready before an old one is removed. This keeps capacity
constant during a rollout. Configuration is delivered through a ConfigMap and
secrets through the platform secret store, both mounted read-only.

## Health check probes

Kubernetes drives two probes. The readiness probe hits the health check
endpoint `/healthz`, which returns `200` only when the connection pool has at
least one usable upstream connection and the config has loaded. The liveness
probe hits `/livez`, which returns `200` as long as the event loop is
responsive. A failing readiness health check pulls the pod out of the Service
rotation without killing it; a failing liveness check restarts it.

## Graceful shutdown

On `SIGTERM` the gateway begins a graceful shutdown. It stops accepting new
connections, fails its readiness probe so Kubernetes stops routing to it, and
then drains: in-flight requests are given up to the grace period to complete
while the connection pool is closed once its borrowed connections are all
returned. Graceful shutdown bounds the drain with `shutdown_grace_seconds`;
anything still running when the deadline passes is cancelled so the pod can
exit before Kubernetes sends `SIGKILL`.

## Connection pool tuning

Pool size is the dominant tuning knob under load. Each replica keeps its own
connection pool per upstream, so the aggregate upstream connection count is
`replicas * pool_max`. Set `pool_max` from the upstream's own connection
budget divided by the replica count, and set the acquire timeout below the
client's overall deadline so a saturated pool surfaces as a fast `503` rather
than a hung request. Watch pool saturation in the dashboards before raising
replica count.

## Resource limits

CPU is requested at half a core and limited at two; the gateway is bursty at
TLS handshake time. Memory request equals limit so the pod is in the
Guaranteed QoS class and is the last to be evicted under node pressure. The
in-process response cache is sized to fit inside the memory limit with
headroom for request buffers.

## Rollback

Every release is tagged and the previous image is kept warm. A rollback is a
one-command redeploy of the prior tag; because config is separate from the
image, a bad config change can be reverted without redeploying the binary.
