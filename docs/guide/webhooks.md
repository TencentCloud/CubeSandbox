# Webhook Event Notifications

CubeAPI can send sandbox lifecycle events to one or more HTTP endpoints. Delivery
is asynchronous: successful sandbox API responses do not wait for webhook
network I/O, retries, or receiver availability.

The runnable receiver is available at
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).

## Supported events

| Event | Trigger | Additional context |
| --- | --- | --- |
| `sandbox.created` | A sandbox is created successfully | `template_id` |
| `sandbox.deleted` | A sandbox is deleted successfully | - |
| `sandbox.paused` | A sandbox is paused successfully | - |
| `sandbox.resumed` | A sandbox is resumed successfully, including automatic resume by `connect` | `template_id` |

Failed lifecycle API calls do not emit events.

## Configure endpoints

Set `CUBE_API_WEBHOOKS` to a JSON array before starting CubeAPI:

```bash
export CUBE_API_WEBHOOKS='[
  {
    "name": "automation",
    "url": "https://automation.example.com/cubesandbox/events",
    "events": ["sandbox.created", "sandbox.deleted"],
    "secret": "replace-with-a-high-entropy-secret"
  },
  {
    "name": "operations",
    "url": "https://alerts.example.com/cubesandbox/events",
    "events": ["sandbox.paused", "sandbox.resumed"]
  }
]'
```

Each endpoint supports these fields:

| Field | Required | Description |
| --- | --- | --- |
| `name` | No | Safe label used in CubeAPI logs. Defaults to `webhook-N`. |
| `url` | Yes | Full `http` or `https` URL. URL userinfo is rejected. Redirects are not followed. |
| `events` | No | Subscription list. An empty or omitted list subscribes to all supported events. |
| `secret` | No | HMAC-SHA256 signing secret. Empty or omitted disables signing for this endpoint. |

CubeAPI validates all endpoint URLs and subscriptions at startup. Invalid
configuration fails startup instead of silently disabling delivery. URLs and
secrets are redacted from configuration debug output and one-click upgrade
reports.

### One-click deployment

Add the single-line JSON value to the `.env` used by the installer, then restart
CubeAPI:

```bash
CUBE_API_WEBHOOKS='[{"name":"local","url":"http://127.0.0.1:8088/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"replace-me"}]'

sudo systemctl restart cube-sandbox-cube-api.service
sudo systemctl status cube-sandbox-cube-api.service
```

The installed runtime environment is root-readable only. Do not commit the real
secret to the repository.

### Direct binary or container

For a source build, export the variable before `cargo run`. For the CubeAPI
container, pass the same variable with your container orchestrator. The endpoint
must be reachable from the CubeAPI control-plane process; sandbox CubeEgress
policies do not control this traffic.

## Delivery tuning

The following optional environment variables apply to all endpoints:

| Variable | Default | Description |
| --- | ---: | --- |
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `1024` | Maximum queued lifecycle events per CubeAPI process |
| `CUBE_API_WEBHOOK_MAX_IN_FLIGHT` | `16` | Maximum concurrent endpoint deliveries |
| `CUBE_API_WEBHOOK_TIMEOUT_MS` | `5000` | Timeout for each HTTP attempt |
| `CUBE_API_WEBHOOK_MAX_ATTEMPTS` | `3` | Total attempts, including the first request |
| `CUBE_API_WEBHOOK_RETRY_BASE_MS` | `500` | Initial retry delay |
| `CUBE_API_WEBHOOK_RETRY_MAX_MS` | `30000` | Maximum retry delay |

Retry delay is exponential and capped:

```text
min(CUBE_API_WEBHOOK_RETRY_BASE_MS * 2^(failed_attempt - 1),
    CUBE_API_WEBHOOK_RETRY_MAX_MS)
```

CubeAPI retries connection and timeout failures, HTTP `408`, HTTP `429`, and
`5xx` responses. Other `3xx` and `4xx` responses are permanent failures.
Any `2xx` response is successful.

## Request format

CubeAPI sends one event per HTTP POST with `Content-Type: application/json`.

```json
{
  "timestamp": "2026-07-10T08:15:30.123Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sandbox-abc123",
  "template_id": "template-python"
}
```

Fields:

| Field | Type | Description |
| --- | --- | --- |
| `event` | string | Lifecycle event name |
| `timestamp` | string | UTC RFC 3339 event creation time |
| `sandbox_id` | string | Sandbox identifier |
| `template_id` | string | Present when already available without another control-plane request |
| `level` | string | Structured event severity; currently `info` for lifecycle events |

Headers:

| Header | Description |
| --- | --- |
| `X-CubeSandbox-Event` | Event name |
| `X-CubeSandbox-Delivery` | UUID stable across retries of one endpoint delivery |
| `X-CubeSandbox-Signature` | `sha256=<hex digest>` when the endpoint has a secret |

## Verify signatures

The signature is HMAC-SHA256 over the exact raw request body. Verify it before
parsing or re-serializing JSON.

```python
import hashlib
import hmac


def verify(body: bytes, header: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, header)
```

HMAC proves authenticity but does not by itself prevent replay. Production
receivers should reject unreasonably old `timestamp` values and deduplicate the
`X-CubeSandbox-Delivery` value for an appropriate retention period.

## Run the receiver example

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 receiver.py
```

In another terminal, verify the receiver before restarting CubeAPI:

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 send_test_event.py
```

Configure CubeAPI to use `http://127.0.0.1:8088/webhook`, then create, pause,
resume, and delete a sandbox. The receiver prints one JSON line per callback.

## WeCom and generic alerting

The CubeSandbox payload is not the same shape as the WeCom group robot API, so
do not configure the robot URL directly as a CubeAPI endpoint. Use the example
receiver as an adapter:

```bash
export WEBHOOK_SECRET=local-development-secret
export WECOM_BOT_URL='https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me'
python3 receiver.py
```

For a generic HTTP alerting system, either accept the documented payload
directly or transform it in the same adapter. Keep provider credentials on the
receiver side so CubeAPI only knows the receiver URL and signing secret.

## Delivery guarantees and limits

- The queue is bounded and in memory. A full queue drops new events and writes
  an error log; sandbox API calls still succeed.
- Graceful shutdown drains events queued before the flush barrier. A process or
  host crash can lose queued events.
- Retries provide at-least-once behavior within a running process, so receivers
  must tolerate duplicates.
- Delivery history and dead-letter storage are not persisted.
- In a multi-replica CubeAPI deployment, the replica handling the lifecycle API
  call emits that event. Endpoint configuration must be consistent across
  replicas.
- Redirects are disabled to avoid forwarding signatures to an unintended host.

Use a durable event bus or a receiver that persists callbacks when stronger
delivery guarantees are required.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| CubeAPI fails to start | Validate that `CUBE_API_WEBHOOKS` is a JSON array and all events are supported. |
| No callback arrives | Check receiver reachability from the CubeAPI host and inspect CubeAPI logs for the endpoint `name`. |
| HTTP 401 from receiver | Ensure both sides use the same secret and verify the raw body bytes. |
| Repeated callbacks | Deduplicate with `X-CubeSandbox-Delivery`; retries intentionally reuse it. |
| TLS failure | Install the receiver CA in the CubeAPI host/container trust store; do not disable certificate verification. |
