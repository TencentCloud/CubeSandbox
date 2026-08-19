# Webhook Event Notifications

CubeSandbox can push sandbox lifecycle events (created, deleted, paused, resumed) as HTTP POSTs to callback URLs you specify. Typical uses: a business system initializes resources after a sandbox is created, reclaims external resources after deletion, or audits and tracks sandbox lifecycle as an event stream.

Delivery is handled by the delivery component built into CubeOps: it consumes the platform's internal lifecycle event stream, writes each event to the delivery ledger per subscription, then pushes it to each subscribed endpoint with automatic retry on failure. The component is **disabled by default**; see [Enabling and Configuration](#enabling-and-configuration).

This document covers two layers of responsibility, which in practice are often handled by the same person or team:

- **Event consumer (business-system side)**: subscribes to sandbox events and handles the callbacks in your existing business system, integrating CubeSandbox with your business. Start with [Quickstart](#quickstart), then read [Event Types and Payload](#event-types-and-payload) and [Headers and Signature Verification](#headers-and-signature-verification) to finish the integration.
- **Platform operator**: enables, configures, and monitors the delivery component. Focus on [Enabling and Configuration](#enabling-and-configuration), [Delivery Semantics](#delivery-semantics), [Monitoring and Alerting](#monitoring-and-alerting), and [Manual Replay](#manual-replay).

## Quickstart

Goal: complete an end-to-end "enable delivery → register a subscription → receive the first event" verification within 30 minutes. The steps below run on a deployed CubeSandbox environment (for example, the one-click install) and only require two additional guarantees:

- Redis ≥ 7, reachable by CubeOps (reuse the platform Redis);
- a CubeOps admin account (default `admin` / `admin` — change the password in production).

### Step 1: Enable the Delivery Component

The delivery component is controlled by environment variables read **at CubeOps startup** and is disabled by default; subscriptions are registered via the REST API in Step 3 (`POST /api/v1/webhooks`). These are two different layers: environment variables decide whether the component runs, REST decides which endpoint receives which events.

For a one-click / systemd deployment, CubeOps reads its `EnvironmentFile` at `/usr/local/services/cubetoolbox/.one-click.env`; append the following variables and restart the service:

```bash
cat >> /usr/local/services/cubetoolbox/.one-click.env <<'EOF'
# Platform Redis (one-click default password ceuhvu123, see the tip below)
REDIS_URL=redis://:ceuhvu123@127.0.0.1:6379/0
# Enable the delivery component
CUBE_OPS_WEBHOOK_ENABLED=true
# Local development only; keep false in production (see the warning below)
CUBE_OPS_WEBHOOK_ALLOW_PRIVATE_NETWORKS=true
EOF

# Restart the service to apply the config
systemctl restart cube-sandbox-cubeops
```

> Non-systemd deployments (running the CubeOps binary directly): `export` the same three variables before starting the process; no env file changes are needed.

Verify:

```bash
curl -s http://127.0.0.1:3010/webhook/healthz
# Expected {"webhook":"ready"}; {"webhook":"disabled"} when not enabled
```

::: tip About the REDIS_URL password
The one-click deployment enables `requirepass` on Redis by default (default password `ceuhvu123`, overridable at install time via `CUBE_SANDBOX_REDIS_PASSWORD` / `CUBE_EXTERNAL_REDIS_PASSWORD`). If you are unsure of the actual password, run `docker inspect cube-sandbox-redis --format '{{.Config.Cmd}}'` to see the value after `--requirepass`. The URL format is `redis://[:password]@host:port/db`; the password can only be omitted when you run your own Redis without `requirepass`.
:::

::: warning About allow_private_networks
For SSRF protection, the delivery component refuses to push to loopback and RFC1918 private addresses by default. This example's receiver runs on local `127.0.0.1`, so the flag is temporarily enabled. **In production it must stay `false`**; endpoints should be publicly reachable or routable within the cluster.
:::

### Step 2: Start the Example Receiver

This step is a reference implementation for the "receiver-side developer": the example receiver stands in for the business service you are going to integrate, letting you verify that events arrive with a valid signature. If you already have your own receiving endpoint, skip to Step 3 and use your URL when creating the subscription.

The repo ships a minimal receiver `examples/webhook-receiver` (Rust) that verifies the signature, validates the timestamp, deduplicates by delivery ID, and prints events to the terminal. It only needs to be reachable from CubeOps; it does not have to run on the same host.

Run it on a machine with the repo source (clone the repo first if needed):

```bash
git clone https://github.com/tencentcloud/CubeSandbox.git
cd CubeSandbox/examples/webhook-receiver
WEBHOOK_SECRET=my-test-secret PORT=9095 cargo run
# webhook-receiver listening on http://127.0.0.1:9095
```

> The receiver listens on `127.0.0.1:9090` by default, but the one-click deployment's cube-egress (sandbox egress proxy) already occupies 9090 (cube-proxy's gRPC also defaults to 9090), so this document uses `PORT=9095` consistently, and the subscription URL in Step 3 must use the same port. If you pick another port, keep both in sync.

If you have neither the source nor a Rust toolchain (for example, verifying directly on the deployment server, python3 required), start a minimal receiver with python3 instead:

```bash
cat > /tmp/webhook-recv.py <<'PY'
import hmac, hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        sig = (self.headers.get('X-Cube-Signature-256') or '').strip()
        if sig.startswith('sha256='):
            sig = sig[len('sha256='):]
        ok = hmac.compare_digest(sig, hmac.new(b'my-test-secret', body, hashlib.sha256).hexdigest())
        print('sig_ok=%s event_id=%s body=%s' % (ok, self.headers.get('X-Cube-Event-ID'), body.decode('utf-8', 'replace')), flush=True)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')
    def log_message(self, *a):
        pass
HTTPServer(('127.0.0.1', 9095), H).serve_forever()
PY
python3 /tmp/webhook-recv.py
# webhook-receiver listening on http://127.0.0.1:9095
```

> Whichever way you choose, the receiver SECRET must match the subscription `secret` in Step 3 (this example uses `my-test-secret` for both).

### Step 3: Log In to CubeOps and Create a Subscription

```bash
# Login to get a JWT (default admin/admin; the response field is camelCase accessToken)
TOKEN=$(curl -s -X POST http://127.0.0.1:3010/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin"}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['accessToken'])")

# Create a subscription: receive sandbox.created events, pushed to the local receiver
curl -s -X POST http://127.0.0.1:3010/api/v1/webhooks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "local-test",
    "url": "http://127.0.0.1:9095/webhook",
    "events": ["sandbox.created"],
    "secret": "my-test-secret"
  }'
```

A successful creation returns 201; the `id` in the response is the subscription ID (assumed `1` below). The `secret` must match the `WEBHOOK_SECRET` the receiver was started with, otherwise signature verification fails.

### Step 4: Send a Test Delivery

```bash
curl -s -X POST http://127.0.0.1:3010/api/v1/webhooks/1/test \
  -H "Authorization: Bearer $TOKEN"
# {"delivery_id":1}
```

The receiver should immediately print a `sandbox.created` event (with `sandbox_id` `test-sandbox`). If it prints `REJECTED (signature mismatch)`, the subscription `secret` does not match the receiver's.

### Step 5: Trigger a Real Event

Create a sandbox and the receiver will receive a real `sandbox.created` whose payload carries the sandbox's `sandbox_id` and template info. The most direct way is the API below (E2B-compatible endpoint, port `3000`); you can also create one with the Python SDK following the [quickstart](./quickstart.md).

First make sure there is a `READY` template (created in quickstart Step 3, IDs look like `tpl-xxx`; or list them with `cubemastercli tpl list`), then:

```bash
curl -s -X POST http://127.0.0.1:3000/sandboxes \
  -H "Content-Type: application/json" -H "X-API-Key: e2b_000000" \
  -d '{"templateID":"tpl-<your-template-id>"}'
# Example response:
# {"templateID":"tpl-xxx","sandboxID":"...","clientID":"...","envdVersion":"...","domain":"cube.app"}
```

> Note the request body field is camelCase `templateID` (not `template`). After creation, the receiver log will show a real `sandbox.created` whose `event_id` is a numeric Redis Stream ID.

### Step 6: Check the Delivery Records

```bash
curl -s "http://127.0.0.1:3010/api/v1/webhooks/1/deliveries?limit=20" \
  -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
```

Each delivery record contains `status` (`succeeded` / `failed` / …), `attempts`, `http_status`, `last_error`, and more — the first place to look when the receiver isn't getting events.

::: tip One-command smoke script
`scripts/webhook-local-smoke.sh` automates the flow above (checks dependencies, registers a subscription, sends a test delivery, and prints PASS/FAIL). The script explicitly sets `allow_private_networks=true` and prints a warning; local use only.
:::

## Event Types and Payload

Subscriptions must explicitly declare which events to receive. The allowed values are:

| Event | Trigger |
| --- | --- |
| `sandbox.created` | Sandbox created successfully |
| `sandbox.deleted` | Sandbox deletion completed |
| `sandbox.paused` | Sandbox paused |
| `sandbox.resumed` | Sandbox resumed running |

The payload is UTF-8 JSON. Common fields are fixed; optional fields appear per event type (tolerate unknown fields so future extensions do not break your parser):

| Field | Type | Description |
| --- | --- | --- |
| `schema_version` | string | Payload schema version, currently `1` |
| `event_id` | string | Unique event ID (Redis stream ID or `test:<uuid>`); shared by all deliveries of the same event |
| `event` | string | Event type, see the table above |
| `timestamp` | number | Event time, Unix milliseconds |
| `occurred_at` | string | Event time, RFC3339, same source as `timestamp` |
| `sandbox_id` | string | Sandbox ID |
| `template_id` | string | Optional; template used to create the sandbox, omitted when metadata is missing |
| `source` | string | Optional; pause/resume events only, values `api` / `auto_pause` / `auto_resume` — distinguishes user actions from platform auto pause/resume |
| `reason` | string | Optional; delete events only, values `request` / `timeout` / `orphaned` etc., passed through verbatim |

Full `sandbox.created` example:

```json
{
  "schema_version": "1",
  "event_id": "1789312345678-0",
  "event": "sandbox.created",
  "timestamp": 1789312345678,
  "occurred_at": "2026-08-18T09:25:45Z",
  "sandbox_id": "sbx-abc123",
  "template_id": "tpl-xyz"
}
```

## Headers and Signature Verification

Every push is an HTTP POST with the following headers:

```http
POST {url}
Content-Type: application/json
X-Cube-Event-ID: {event_id}
X-Cube-Delivery: {event_id}:{subscription_id}
X-Cube-Timestamp: {unix_ms}
X-Cube-Signature-256: {hex}   ← only present when the subscription has a secret
```

Receivers should verify in this order:

1. Compute `hex(HMAC-SHA256(secret, raw request body bytes))` with the subscription secret and compare it in constant time against `X-Cube-Signature-256`;
2. check the skew between `X-Cube-Timestamp` and the current time (suggested ±5 minutes; widen or tighten as needed);
3. deduplicate by `X-Cube-Delivery` — all retries of the same delivery reuse the same delivery ID; no new ID is generated.

`examples/webhook-receiver` is a reference implementation of all three steps; you can reuse its verification code directly.

::: warning Timestamp checks do not prevent replay
The signature only covers the request body; headers such as `X-Cube-Timestamp` are not signed. An attacker who intercepts a request can rewrite these unsigned headers, so a timestamp-skew check only filters expired requests — it cannot prevent replay. Real replay protection relies on the idempotent dedup in step 3 (`X-Cube-Delivery`, or the signed `event_id` in the body); receivers must implement dedup and not rely on the timestamp window alone. For production endpoints, add an IP allowlist or gateway auth at the entry as well.
:::

## Subscription Management API

All endpoints sit behind JWT auth under the `/api/v1/webhooks` prefix. In the current version, any authenticated user can manage all subscriptions (no per-owner permission isolation yet).

| Method | Path | Description |
| --- | --- | --- |
| POST | `/api/v1/webhooks` | Create a subscription; returns 201 |
| GET | `/api/v1/webhooks` | List subscriptions (deleted ones excluded); paginated with `limit` / `offset`, default 50, max 200 |
| GET | `/api/v1/webhooks/:id` | Subscription detail; deleted subscriptions return 200 with a `deleted_at` field (read-only, not actionable) |
| PUT | `/api/v1/webhooks/:id` | Partial update; deleted subscriptions return 404 |
| DELETE | `/api/v1/webhooks/:id` | Soft delete; returns 204; repeated DELETE returns 404 |
| POST | `/api/v1/webhooks/:id/test` | Writes a test delivery and returns `{"delivery_id":...}`; 503 when globally disabled, 409 when the subscription is disabled, 404 when deleted; rate-limited to 5 per subscription per minute (per process; N replicas → 5 × N) |
| GET | `/api/v1/webhooks/:id/deliveries` | Query delivery records; supports `status`, `event_id_prefix`, `limit` / `offset` |

Example create body:

```json
{
  "name": "business-system",
  "url": "https://example.com/cubesandbox/events",
  "events": ["sandbox.created", "sandbox.deleted"],
  "secret": "optional-hmac-secret",
  "enabled": true
}
```

Field rules:

- `name` is globally unique, max 128 chars; soft delete releases the name, so recreating with the same name yields a new subscription ID;
- `events` must be non-empty and all in the whitelist above;
- `url` only allows `http` / `https`, and must not carry userinfo;
- `secret` is optional. Stored encrypted; no query endpoint echoes the plaintext. On PUT, omitting it keeps the old value; passing an explicit empty string clears the signature (subsequent pushes carry no signature header);
- deleting a subscription does not delete historical delivery records; they remain queryable via `GET /:id/deliveries` with the old subscription ID.

## Delivery Semantics

Understanding these semantics helps you implement the receiver correctly:

- **At-least-once**: events successfully written to the platform event stream are guaranteed to be delivered at least once; write failures or events trimmed from the Redis stream by length are a declared loss window (surfaced via the XADD failure counter and consumer-lag alerts, see [Monitoring and Alerting](#monitoring-and-alerting)). Receivers may therefore see duplicate pushes and must deduplicate by `X-Cube-Delivery`.
- **No ordering guarantee**: multiple events of the same sandbox are not guaranteed to arrive in order. If you need strong ordering, use `timestamp` / `occurred_at` in the payload to judge and drop stale events.
- **Any 2xx means success**: any 2xx status from the receiver counts as a successful delivery; the platform does not do end-to-end business confirmation. Business-side outcomes should be guaranteed by your own mechanisms, not by delivery retries.

### Retries and Dead Letters

- Retryable failures: HTTP 408 / 429 / 5xx, network errors, timeouts, redirects. Each retry increments `attempts`, with exponential backoff (1s base, 10m cap, with jitter).
- Permanent failures: other 4xx (receiver explicitly rejected), secret decryption failure, SSRF address-policy denial. The delivery is set to `permanent_failed`, keeping `last_error` / `http_status` for troubleshooting; no more retries.
- Two fallback modes (`dead_letter_mode` config):
  - `keep-pending` (default): keeps retrying, bounded by `keep_pending_max_retry_window` (default 7 days); rows past the window become `dead`. Set to `0` for infinite retries — **only via the environment variable `CUBE_OPS_WEBHOOK_KEEP_PENDING_MAX_RETRY_WINDOW=0`** (YAML cannot express a bare `0`), which requires backlog alerts and a manual handling process.
  - `dead-letter`: after `max_attempts` (default 5) is exhausted, the delivery becomes `dead`.

## Enabling and Configuration

Beyond the two environment variables at the start of this section, the full configuration lives under the `webhook:` section of `CubeOps/config.example.yaml`; every field has a matching `CUBE_OPS_WEBHOOK_*` environment-variable override. Common items:

| Config | Default | Description |
| --- | --- | --- |
| `enabled` | `false` | Master switch; requires `redis_url` when enabled |
| `consumer_group` | `cube-webhook` | Redis consumer group name; must not collide with platform components (`cube-proxy-sidecar`) |
| `worker_concurrency` / `per_subscription_concurrency` | 8 / 2 | Send concurrency per replica: global cap and per-subscription cap. With N replicas, total concurrency is N × the value |
| `http_timeout` | `10s` | Timeout for a single push |
| `max_attempts` | `5` | Max attempts in `dead-letter` mode |
| `keep_pending_max_retry_window` | `168h` | Total retry window in `keep-pending` mode; `0` = infinite retries (requires alerting), **settable only via the environment variable `CUBE_OPS_WEBHOOK_KEEP_PENDING_MAX_RETRY_WINDOW=0`** — a bare `0` in YAML fails to parse and `"0s"` is overwritten by the default |
| `dead_letter_mode` | `keep-pending` | Fallback mode, see above |
| `allow_private_networks` | `false` | Allow loopback / RFC1918 addresses; local development only, must stay off in production |
| `cleanup.*` | 30 days / 90 days / 24h | Retention of terminal delivery records and the cleanup interval |

Prerequisite: Redis ≥ 7 (the consumer-group lag alert relies on the `lag` field of `XINFO GROUPS`; on older versions the alert degrades to an approximation).

## Monitoring and Alerting

CubeOps exposes all delivery-component metrics at `GET /metrics` (default port 3010). Key metrics:

| Metric | Description |
| --- | --- |
| `cubeops_webhook_delivery_result_total` | Delivery result counts (succeeded / retryable / permanent / shutdown) |
| `cubeops_webhook_delivery_duration_seconds` | Push HTTP latency distribution |
| `cubeops_webhook_backlog_rows` | Actionable backlog by status (pending / retryable failed) |
| `cubeops_webhook_keep_pending_dead_total` | Rows turned `dead` because the window was exhausted |
| `cubeops_webhook_lease_contention_total` | Multi-replica lease contention count |

CubeMaster additionally exposes `cubemaster_lifecycle_xadd_failures_total` (failed writes to the event stream). Consider alerting on at least: delivery latency P95, `failed` / `dead` backlog growth, non-zero XADD failures, consumer-group lag above a threshold, and the backlog level when `keep_pending_max_retry_window=0`.

## Manual Replay

For `permanent_failed` or `dead` delivery rows, after fixing the receiver you can replay them with SQL (replay clears `first_failed_at`, so they will not immediately turn `dead` again):

```sql
UPDATE t_webhook_delivery
SET status='pending', attempts=0, first_failed_at=NULL, next_retry_at=now(),
    lease_owner=NULL, lease_until=NULL, http_status=NULL, last_error=NULL
WHERE id=? AND status IN ('permanent_failed','dead');

-- If the event previously triggered a materialization-failure quarantine,
-- optionally clear the failure record (the 90-day retention also cleans it up)
DELETE FROM t_webhook_materialization_failure WHERE event_id = ?;
```

## Security Limits

- SSRF protection: before pushing, the callback domain is DNS-resolved once and every resolved address is CIDR-checked (loopback, RFC1918, link-local including cloud metadata addresses, CGNAT, multicast, reserved ranges, and IPv4-mapped IPv6 are all covered); if any address is disallowed the push is rejected as a whole (fail-closed).
- HTTP redirects are not followed (3xx is treated as a retryable failure); the response body read limit is 1 MiB.
- Secrets are stored encrypted; logs never output the secret or signature values.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| Test delivery returns 503 | The delivery component is not enabled (`CUBE_OPS_WEBHOOK_ENABLED`), or startup failed because `redis_url` is not configured |
| `/webhook/healthz` returns not_ready | Check Redis connectivity and version (≥ 7), and webhook-related errors in the CubeOps log |
| Receiver gets no events at all | Check in order: whether the subscription `enabled` and `events` include the target event; whether a delivery row was created in the `deliveries` API — a row with `failed` means a push problem (see `last_error`), no row means the event did not match a subscription |
| Keeps retrying without success | Locate via `last_error` / `http_status`: connection errors → check network and SSRF policy (private endpoints need a public address); 401/403 → check the receiver's signature verification |
| Receiver reports signature mismatch | The subscription `secret` does not match the receiver's config; or the receiver re-encoded the body (HMAC must be computed over the raw request body bytes) |
| Events noticeably delayed | Check `backlog_rows` backlog and consumer-group lag; a single subscription endpoint returning sustained 5xx slows everything down — lower that subscription's priority or temporarily disable it |
| Want to replay a failed delivery | See [Manual Replay](#manual-replay) |

Push-side logs live in the CubeOps log directory (default `/data/log/CubeOps`); search for `webhook` to see the classification result of each push.
