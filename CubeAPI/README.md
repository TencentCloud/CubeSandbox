# Cube E2B API

A Rust-based **E2B-compatible API Server** built on the [Axum](https://github.com/tokio-rs/axum) framework, running on top of Cube's proprietary sandbox infrastructure.

No client code changes are needed — simply point `E2B_API_URL` and `E2B_API_KEY` to this service to seamlessly migrate from the E2B cloud to the Cube platform. For HTTPS access to sandboxes, configure `SSL_CERT_FILE` as needed (see details below).

---

## Table of Contents

- [Supported Features](#supported-features)
- [Quick Start](#quick-start)
- [Webhook Event Notifications](#webhook-event-notifications)
- [Examples](#examples)

---

## Supported Features

The following Sandbox core APIs are **fully E2B-compatible** and can be used directly with the official `e2b` / `e2b-code-interpreter` Python SDK:

| Method | Path | Description | Implemented |
|--------|------|-------------|:-----------:|
| GET | `/health` | Health check (no authentication required) | ✅ |
| GET | `/sandboxes` | List all sandboxes (v1) | ✅ |
| GET | `/v2/sandboxes` | List sandboxes (v2, supports state/metadata filtering, limit) | ✅ |
| POST | `/sandboxes` | Create a sandbox | ✅ |
| GET | `/sandboxes/:sandboxID` | Get single sandbox details | ✅ |
| DELETE | `/sandboxes/:sandboxID` | Destroy a sandbox | ✅ |
| POST | `/sandboxes/:sandboxID/pause` | Pause a sandbox (preserves memory snapshot) | ✅ |
| POST | `/sandboxes/:sandboxID/resume` | Resume a sandbox (deprecated, replaced by connect) | ✅ |
| POST | `/sandboxes/:sandboxID/connect` | Connect to a sandbox (auto-resumes, replaces resume) | ✅ |
| GET | `/sandboxes/:sandboxID/logs` | Get sandbox logs (v1, deprecated) | ❌ |
| GET | `/v2/sandboxes/:sandboxID/logs` | Get sandbox logs (v2, cursor-based pagination) | ❌ |
| POST | `/sandboxes/:sandboxID/timeout` | Set sandbox timeout (absolute TTL) | ❌ |
| POST | `/sandboxes/:sandboxID/refreshes` | Extend sandbox lifetime (relative TTL) | ❌ |
| POST | `/sandboxes/:sandboxID/snapshots` | Create a sandbox snapshot | ❌ |
| GET | `/sandboxes/:sandboxID/metrics` | Get sandbox metrics | ❌ |
| GET | `/sandboxes/snapshots` | List all snapshots for the team | ❌ |
| PUT | `/sandboxes/:sandboxID/network` | Update sandbox network config (egress rules) | ❌ |

**Legend:** ✅ Fully implemented | ❌ Route not registered or depends on pending CubeMaster APIs

### Cube Extensions

| Feature | Description |
|---------|-------------|
| **Host Directory Mount** | Mount a host directory into the sandbox via `metadata.host-mount` at creation time |
| **Browser Sandbox** | Built-in Chromium inside the sandbox, exposed via CDP, allowing direct Playwright control |

---

## Quick Start

### Running the Service

```bash
# Development mode
RUST_LOG=debug cargo run

# Production build
cargo build --release
./target/release/cube-api
```

### Server Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBE_API_BIND` | `0.0.0.0:3000` | Listen address |
| `LOG_LEVEL` | `info` | Log level |

CubeAPI also exposes dashboard-oriented routes under `/cubeapi/v1`. The one-click WebUI is served by a separate nginx container on port `12088`; that nginx instance serves the packaged static dashboard and proxies same-origin `/cubeapi` requests back to the host CubeAPI through Docker `host-gateway`.

---

## Webhook Event Notifications

CubeAPI can asynchronously POST best-effort notifications to configured HTTP endpoints when these sandbox lifecycle events occur:

- `sandbox.created`
- `sandbox.deleted`
- `sandbox.paused`
- `sandbox.resumed`

Webhooks are implemented as an internal logging backend, not as a new REST API route. API handlers emit `LogEvent` values, and the existing `MultiLogger` fans them out to `FileLogger` and, when configured, `HttpLogger`. HTTP delivery and retries run asynchronously and do not block sandbox create, delete, pause, or resume operations. A failed delivery does not change the sandbox API response. If `CUBE_API_WEBHOOK_ENDPOINTS` is set but is not valid JSON, or an explicitly configured endpoint is invalid, CubeAPI fails during startup instead of silently disabling webhooks.

### Configuration

Set `CUBE_API_WEBHOOK_ENDPOINTS` to a JSON array:

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[
  {
    "url": "http://127.0.0.1:18080/webhook",
    "events": ["sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"],
    "secret": "test-secret",
    "enabled": true,
    "allow_private_urls": true
  }
]'
```

The array may contain multiple endpoint objects. CubeAPI evaluates each endpoint independently and fans a matching event out to every enabled endpoint.

Each endpoint supports:

| Field | Description |
|-------|-------------|
| `url` | Webhook receiver URL. |
| `events` | Event names subscribed to by this endpoint. |
| `secret` | Optional HMAC secret. When omitted, the request is unsigned. |
| `enabled` | Whether the endpoint is active. Optional, default `true`. |
| `allow_private_urls` | Optional, default `false`. Explicitly permit this endpoint to target a local-development or internal receiver, such as `localhost`, a loopback IP, a private IP, or a link-local IP. |

`max_payload_bytes` is a global `WebhookConfig` setting, separate from `CUBE_API_WEBHOOK_ENDPOINTS`. It defaults to 256 KiB, must be greater than zero, and has a hard maximum of 1 MiB. Configure it through:

```bash
export WEBHOOK__MAX_PAYLOAD_BYTES=262144   # max serialized webhook body, in bytes
```

CubeAPI fully serializes each webhook body with `serde_json::to_vec` before checking the limit. Each accepted `Delivery.body` is at most `max_payload_bytes`; retained delivery bodies remain regulated by the existing outstanding-task mechanism, and the matching-endpoint batch currently being constructed is retained temporarily. This is not a complete webhook memory bound: the limit does not bound the original `LogEvent`, the transient allocation required to serialize an oversized event, `FileLogger` memory, reqwest/hyper buffering, or CubeMaster response memory.

The finite default is an intentional runtime behavior change: an event whose serialized body exceeds 256 KiB and was previously deliverable is now dropped unless the configured limit is raised to accommodate it, up to the 1 MiB hard maximum.

### Endpoint URL Validation

CubeAPI validates every configured endpoint URL at startup and refuses to start on a violation:

- URLs that embed credentials (`http://user:pass@host/...`) are always rejected, regardless of `allow_private_urls`.
- URLs targeting obviously invalid or unsafe addresses are always rejected, even when `allow_private_urls` is `true`: the unspecified addresses `0.0.0.0` and `::`, the broadcast address `255.255.255.255`, and multicast addresses (`224.0.0.0/4`, `ff00::/8`).
- URLs targeting non-public addresses (loopback `127.0.0.0/8` and `::1`, private `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, and `fc00::/7`, link-local `169.254.0.0/16` and `fe80::/10`, or the hostname `localhost`) are rejected by default. Set `"allow_private_urls": true` on that endpoint to permit them; use this only for local development or trusted internal receivers.
- Domain-name endpoints are resolved during startup. Resolution failures and empty results fail startup. Every resolved IPv4 and IPv6 address must pass the same validation rules; mixed public and non-public results are rejected unless the endpoint explicitly sets `"allow_private_urls": true`.
- Validation error messages identify the endpoint by index only; they never include the URL, embedded credentials, or the endpoint secret.

Resolved domain addresses are pinned for the lifetime of the shared HTTP client, while the original hostname remains in the URL for HTTPS certificate validation and SNI. DNS changes or receiver IP rotation require restarting CubeAPI. Continue to restrict outbound network access at the deployment layer as defense in depth.

Event filtering follows these rules:

- `enabled = false`: skip the endpoint.
- `events = []`: subscribe to the four sandbox lifecycle events listed above.
- `events = ["*"]`: subscribe to every `LogEvent`.
- Any other non-empty list: match event names exactly.

Do not combine `"*"` with explicit event names in the same endpoint. Use either `events = ["*"]` or an explicit list; mixed subscriptions are rejected during startup.

### Request Format

Each event is sent as one HTTP POST per matching endpoint. Requests include:

```text
Content-Type: application/json
User-Agent: CubeAPI-Webhook/1.0
X-Cube-Webhook-Event: <event>
X-Cube-Webhook-Delivery: <uuid>
X-Cube-Webhook-Timestamp: <unix-seconds>
X-Cube-Webhook-Signature: v1=<lowercase-hex>
```

`X-Cube-Webhook-Signature` is present only when the endpoint has a `secret`. It is an HMAC-SHA256 over the exact bytes below:

```text
timestamp + "." + delivery_id + "." + raw_request_body
```

The `timestamp` and `delivery_id` values are taken from their corresponding request headers. `raw_request_body` means the exact body bytes received over HTTP, before parsing or reformatting JSON. The signature header format is exactly `v1=<lowercase-hex>`; receivers must not expect `sha256=<hex>`.

The JSON payload contains delivery metadata and flattened event fields:

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2026-07-01T08:00:00Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sandbox-123",
  "template_id": "template-456"
}
```

Lifecycle payloads include `event`, `timestamp`, and `sandbox_id`; they also include the delivery `id` and log `level`. `template_id` is included when it is available, such as for `sandbox.created` and `sandbox.resumed`, but callers must not assume it is present on every event. The payload is not a complete `Sandbox` object. Lifecycle handlers do not add tokens, environment variables, network configuration, or runtime state to these events.

### Delivery Semantics

- Any `2xx` response is successful.
- Network errors, request timeouts, `408`, `425`, `429`, and `5xx` responses are retried with exponential backoff capped by `max_backoff_ms`.
- `3xx` and other `4xx` responses are not retried.
- Delivery failures are logged but do not change the sandbox API result.
- Events whose fully serialized payload exceeds `max_payload_bytes` (default 256 KiB, hard maximum 1 MiB) are dropped for all matching endpoints and logged locally; they are never signed, sent, or retried.
- Delivery is best-effort. CubeAPI does not implement batch delivery, a database outbox, disk spool, persistent replay, dead-letter queues, or exactly-once delivery.

### Alerting and Enterprise IM Integration

CubeAPI sends a generic signed HTTP event and does not implement vendor-specific bot protocols. To integrate WeCom, Feishu, Slack, or another alerting platform, point CubeAPI at a small adapter service that:

1. verifies the CubeAPI HMAC signature against the raw request body;
2. maps the generic event to the vendor's message format;
3. stores and uses the vendor webhook key or token outside CubeAPI; and
4. handles vendor-specific rate limits and errors.

The same adapter pattern works for generic HTTP alerting systems. Do not put bot tokens, credentials, or complete sandbox objects into webhook event fields.

For a step-by-step local validation guide and a standard-library receiver, see the [English guide](examples/webhook-receiver/README.md) or the [简体中文指南](examples/webhook-receiver/README_zh.md).

---

## Examples

The [`examples/`](examples/) directory provides complete examples based on the official `e2b` / `e2b-code-interpreter` Python SDK.

### Example Overview

| File | Description |
|------|-------------|
| `create.py` | Create a sandbox and print basic info |
| `cmd.py` | Execute shell commands inside a sandbox |
| `exec_code.py` | Execute Python code inside a sandbox |
| `read.py` | Read files from the sandbox filesystem |
| `pause.py` | Pause a sandbox, wait, then resume and verify state |
| `create_with_mount.py` | Create a sandbox with a host directory mount (Cube extension) |
| `browser.py` | Launch a sandbox with Chromium and control the browser via Playwright |
| `test.py` | Multi-threaded stress test: create sandboxes, execute code and commands in a loop |


### Running the Examples

**1. Install Python dependencies**

```bash
cd examples
pip install -r requirements.txt

# If running browser.py, also install the Playwright browser driver
playwright install chromium
```

**2. Set environment variables**

The following four environment variables must be exported before running:

| Variable | Description |
|----------|-------------|
| `CUBE_TEMPLATE_ID` | Cube sandbox template ID. All examples use this to determine which template to create sandboxes from; must be explicitly set. |
| `E2B_API_URL` | Address of the Cube E2B API service. The SDK defaults to the official E2B cloud service, so this must be overridden with the local or deployed address — otherwise requests will go to the official service instead of Cube. |
| `E2B_API_KEY` | The E2B SDK requires this field to be present (it performs a non-empty check). For local deployments, any non-empty string works, e.g. `e2b_000000`. |
| `SSL_CERT_FILE` | When accessing sandboxes using Cube's built-in test certificate (`cube.app`), set this variable to the corresponding CA root certificate path so that the E2B SDK's httpx/requests can complete TLS verification. We recommend using a locally signed certificate from mkcert: `/root/.local/share/mkcert/rootCA.pem`.<br>If you use a custom domain with a trusted certificate, or access sandboxes over HTTP, this variable is not needed. See [CubeProxy TLS Configuration](../docs/guide/cubeproxy-tls.md). |

```bash
export CUBE_TEMPLATE_ID=<your-template-id>
export E2B_API_URL=http://localhost:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

**3. Run**

```bash
python create.py
python cmd.py
python exec_code.py
python read.py
python pause.py
python create_with_mount.py
python browser.py
python test.py
```

