# CubeSandbox Webhook Receiver

A minimal Flask-based webhook receiver that verifies and logs CubeSandbox webhook events.

## Quick Start

```bash
# 1. Install dependencies
pip install -r requirements.txt

# 2. Start the receiver (no signature verification)
python receiver.py --port 5000

# 3. Or, with HMAC-SHA256 signature verification enabled:
python receiver.py --port 5000 --secret "your-shared-secret"
```

The receiver listens on `0.0.0.0:5000` and exposes:

- `GET  /health`  — health check
- `POST /webhook` — receives webhook events

Received events are printed to stdout and appended to `webhook_events.jsonl`.

## Exposing the Receiver

For local development, use a tunnel tool:

```bash
# ngrok
ngrok http 5000

# Cloudflare Tunnel
cloudflared tunnel --url http://localhost:5000
```

Copy the public URL and set it as `WEBHOOK_ENDPOINTS` in CubeAPI.

## Configuring CubeSandbox

```bash
export WEBHOOK_ENDPOINTS="https://your-tunnel.ngrok.io/webhook"
export WEBHOOK_EVENTS="sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed"
export WEBHOOK_SECRET="your-shared-secret"  # optional
export WEBHOOK_RETRY_MAX=3
export WEBHOOK_RETRY_BASE_MS=1000
```

Then restart `cube-api`.

## Testing

```bash
# Create sandbox — triggers "sandbox.created"
curl -X POST http://localhost:3000/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "your-template-id"}'

# Pause — triggers "sandbox.paused"
curl -X POST http://localhost:3000/sandboxes/<sandbox-id>/pause

# Resume — triggers "sandbox.resumed"
curl -X POST http://localhost:3000/sandboxes/<sandbox-id>/resume \
  -H "Content-Type: application/json" -d '{}'

# Delete — triggers "sandbox.deleted"
curl -X DELETE http://localhost:3000/sandboxes/<sandbox-id>
```

## Integrating with WeCom (Enterprise WeChat)

```python
# wecom_bridge.py
import requests
from flask import Flask, request

WECOM_WEBHOOK_URL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"

EVENT_LABELS = {
    "sandbox.created": "Sandbox Created",
    "sandbox.deleted": "Sandbox Destroyed",
    "sandbox.paused": "Sandbox Paused",
    "sandbox.resumed": "Sandbox Resumed",
}

app = Flask(__name__)

@app.route("/webhook", methods=["POST"])
def webhook():
    payload = request.get_json()
    event = payload.get("event", "unknown")
    sandbox_id = payload.get("sandbox_id", "?")

    label = EVENT_LABELS.get(event, event)
    message = {
        "msgtype": "markdown",
        "markdown": {
            "content": "## %s\n> Sandbox ID: `%s`\n> Time: %s" % (
                label, sandbox_id, payload.get("timestamp", "?")
            ),
        },
    }
    requests.post(WECOM_WEBHOOK_URL, json=message)
    return {"status": "ok"}, 200

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000)
```

## Event Payload

| Field         | Type   | Description                                |
|---------------|--------|--------------------------------------------|
| `event`       | string | Event type (e.g. `sandbox.created`)         |
| `timestamp`   | string | ISO-8601 UTC timestamp                     |
| `sandbox_id`  | string | ID of the affected sandbox                 |
| `template_id` | string | Template ID (for `sandbox.created` only)    |

Example:
```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-27T08:30:00.123456Z",
  "sandbox_id": "sb-abc123def456",
  "template_id": "tpl-python-3.11"
}
```

## Signature Verification

When `WEBHOOK_SECRET` is set in CubeAPI, every webhook carries an `X-Cube-Webhook-Signature` header with a hex-encoded HMAC-SHA256 of the body.

**Python:**
```python
import hmac, hashlib
def verify(body, signature, secret):
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)
```

**Node.js:**
```javascript
const crypto = require("crypto");
function verify(body, signature, secret) {
  const expected = crypto.createHmac("sha256", secret).update(body).digest("hex");
  return crypto.timingSafeEqual(Buffer.from(expected, "hex"), Buffer.from(signature, "hex"));
}
```

**Go:**
```go
import ("crypto/hmac"; "crypto/sha256"; "encoding/hex")
func verify(body []byte, signature, secret string) bool {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    expected := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(signature))
}
```
