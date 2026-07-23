# CubeSandbox Webhook Receiver

This dependency-free Python server receives CubeSandbox lifecycle webhooks and
optionally verifies the `X-Cube-Signature-256` HMAC-SHA256 header.

## Run it

From the repository root:

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 8099 \
  --secret local-development-secret
```

Configure CubeAPI with one or more comma-separated endpoints:

```bash
export WEBHOOK_URLS=http://127.0.0.1:8099/webhook
export WEBHOOK_SECRET=local-development-secret
export WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
export WEBHOOK_MAX_RETRIES=3
export WEBHOOK_RETRY_BASE_MS=250
```

Restart `cube-api`, then use the normal CubeAPI sandbox API. The receiver prints
each accepted JSON payload and responds with `204 No Content`.

For a receiver on another host, use HTTPS and keep the secret in the deployment
secret manager rather than putting it in a shell history or source file.

## Payload

The payload is the structured `LogEvent` object. Lifecycle events always include
`event`, `timestamp`, and `sandbox_id`; creation also includes `template_id`.

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-23T12:00:00.000Z",
  "level": "info",
  "sandbox_id": "sbx-example",
  "template_id": "tpl-example"
}
```

## Verify a signature manually

The signature is calculated over the exact request bytes, not a re-encoded JSON
object:

```bash
secret='local-development-secret'
body='{"event":"sandbox.created","timestamp":"2026-07-23T12:00:00Z","sandbox_id":"sbx-demo"}'
signature="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$secret" | awk '{print $2}')"

curl -i http://127.0.0.1:8099/webhook \
  -H 'Content-Type: application/json' \
  -H "X-Cube-Signature-256: $signature" \
  --data-binary "$body"
```

The expected response is `204`. A missing or incorrect signature returns `401`
when the receiver was started with `--secret`.

## Forward to WeCom or a generic alert endpoint

The receiver can be extended after signature verification. For a WeCom bot,
POST a transformed message to the bot URL:

```python
import os
import urllib.request
import json

message = {
    "msgtype": "markdown",
    "markdown": {"content": f"CubeSandbox event: {payload['event']}\n"
                              f"sandbox: {payload['sandbox_id']}"},
}
request = urllib.request.Request(
    os.environ["WECOM_BOT_URL"],
    data=json.dumps(message).encode(),
    headers={"Content-Type": "application/json"},
)
urllib.request.urlopen(request, timeout=5).read()
```

For generic alerting, preserve the original payload and add an authorization
header or destination-specific envelope in the same handler. Do not forward
the CubeSandbox HMAC header as if it authenticated the downstream service.
