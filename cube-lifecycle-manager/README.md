# cube-lifecycle-manager

Standalone service that owns sandbox auto-pause / auto-resume coordination
between CubeMaster, CubeProxy and Redis.

- Consumes lifecycle events from `cube:v1:shared:sandbox:lifecycle:events`
- Discovers every live CubeProxy replica in real time via
  `cube:v1:shared:cube_proxy:{registry,heartbeat}` and broadcasts sandbox
  metadata + state through their `/admin/*` endpoints
- Handles the synchronous `/internal/resume` callback CubeProxy invokes when
  a paused sandbox receives a request

## Local development

```sh
make test    # go test ./...
make build   # local image, tag: cube-lifecycle-manager:v0.6.0-<arch>
```

## Publishing images

Multi-arch release flow (mirrors `CubeEgress/Makefile`). Runs against both
`cube-sandbox-int.tencentcloudcr.com` and `cube-sandbox-cn.tencentcloudcr.com`.

```sh
# On an amd64 host:
make build push ARCH=amd64

# On an arm64 host:
make build push ARCH=arm64

# On either host (both arch-specific images must already be pushed):
make manifest
```

Overrides:

- `IMAGE_TAG=<tag>` — override the release tag (default `v0.6.0`)
- `V=1` — verbose docker commands

## Configuration

All configuration is via environment variables (prefix `CUBE_LCM_`); see
`internal/config/config.go` for the authoritative list.

## Webhook event notifications

CLM can deliver `sandbox.created`, `sandbox.deleted`, `sandbox.paused`, and
`sandbox.resumed` events to external HTTP endpoints. Set
`CUBE_LCM_WEBHOOK_ENDPOINTS` to a JSON array before starting:

```bash
export CUBE_LCM_WEBHOOK_ENDPOINTS='[
  {
    "url": "https://ops.example.com/webhooks/cube",
    "events": ["sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"],
    "secret": "replace-with-a-random-shared-secret"
  }
]'
```

- `events` is a filter; omit it (or pass `[]`) to subscribe to every event.
- `secret` is optional. When set (minimum 16 bytes), every request carries
  `X-Cube-Signature-256: sha256=<hex HMAC>` over the exact request body.

Each endpoint owns an independent Redis consumer group
(`webhook:<hash>`) on the lifecycle events stream, so a slow or unreachable
receiver never delays other subscriptions or the auto-pause/resume loop.
Delivery is at-least-once: an event is acknowledged only after it reaches a
terminal state (delivered, permanently rejected, or retries exhausted), and a
restarted replica redelivers whatever its consumer left pending. A single
replica delivers events in stream order; with several replicas the group is
split across consumers, so cross-replica ordering is not guaranteed. Entries
left pending by a permanently dead replica stay observable via `XPENDING`
and can be reassigned with `XAUTOCLAIM`.

Delivery retries `5xx`, `408`, and `429` responses with exponential backoff
and jitter (base `CUBE_LCM_WEBHOOK_RETRY_BASE`, default 250ms; up to
`CUBE_LCM_WEBHOOK_MAX_RETRIES` retries, default 3; per-attempt timeout
`CUBE_LCM_WEBHOOK_TIMEOUT`, default 5s). The whole delivery is capped at 30
seconds. Other `4xx` responses are permanent and are not retried. Automatic
redirects are disabled so a configured endpoint cannot bounce delivery to an
internal service.

The JSON body always carries `version`, `event`, `timestamp` (RFC 3339), and
`sandbox_id`; `sandbox.created` adds `template_id` when known, and
`sandbox.paused` / `sandbox.resumed` add `state`. Every request sets
`X-Cube-Event` and `X-Cube-Timestamp` headers mirroring the body fields.

See [`../examples/webhook-receiver/`](../examples/webhook-receiver/) for a
dependency-free receiver with signature verification and replay protection.

Webhook endpoints are trusted operator configuration and may intentionally
target private or loopback services. Do not expose
`CUBE_LCM_WEBHOOK_ENDPOINTS` to untrusted users.
