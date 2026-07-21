# CubeSandbox Webhook Receiver

[中文文档](README_zh.md)

A runnable, dependency-free example that receives CubeSandbox **webhook event
notifications** for sandbox lifecycle changes (`sandbox.created`,
`sandbox.paused`, `sandbox.resumed`, `sandbox.deleted`), verifies the optional
HMAC-SHA256 signature, and prints each event.

For the full feature reference (config format, payload spec, signature
scheme), see the [Webhooks guide](../../docs/guide/webhooks.md).

## What you get

| File | Purpose |
|---|---|
| `receiver.py` | Prints received events to stdout; verifies signatures. |
| `wecom_bridge.py` | Forwards events to a WeCom (企业微信) group bot. |
| `webhooks.example.yaml` | Sample CubeAPI webhook config to copy from. |

Both scripts use only the Python 3 standard library — no `pip install` needed.

## End-to-end in ~5 minutes

### 1. Start the receiver

```bash
# Plain mode (no signature checking)
python3 receiver.py

# Or verify signatures with a shared secret
CUBE_WEBHOOK_SECRET=my-shared-secret python3 receiver.py
```

It listens on `http://0.0.0.0:9100/webhook` by default (override with `HOST` /
`PORT`).

### 2. Configure CubeAPI

Copy `webhooks.example.yaml` and point it at your receiver:

```yaml
webhooks:
  - url: "http://127.0.0.1:9100/webhook"
    events: [sandbox.created, sandbox.paused, sandbox.resumed, sandbox.deleted]
    secret: "my-shared-secret"   # must match CUBE_WEBHOOK_SECRET above
```

Start (or restart) CubeAPI with the config:

```bash
cube-api --webhook-config /path/to/webhooks.yaml
# or
CUBE_API_WEBHOOK_CONFIG=/path/to/webhooks.yaml cube-api
```

On startup you should see a log line like
`webhook event notifications enabled endpoints=1`.

### 3. Trigger events

Use the API or SDK to create, pause, resume, and delete a sandbox. Each
operation makes the receiver print an event, e.g.:

```
[2026-07-21 08:30:12 UTC] ✓ sandbox.created  (signature verified ✓)
{
  "event": "sandbox.created",
  "timestamp": "2026-07-21T08:30:12.481Z",
  "sandbox_id": "sbx-abc123",
  "template_id": "base"
}
```

> `template_id` is included on `sandbox.created`; the other three events carry
> `sandbox_id` and `timestamp`.

## Verifying the signature yourself

When `secret` is set, CubeAPI signs the **raw request body** and sends
`X-Cube-Signature: sha256=<hex>`. To verify:

```python
import hashlib, hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest("sha256=" + expected, header)
```

Always compute the HMAC over the exact bytes received (do not re-serialize the
JSON first — key ordering may differ).

## Forwarding to WeCom (企业微信)

Create a group bot in WeCom, copy its webhook URL, then:

```bash
WECOM_BOT_URL="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxxxxxx" \
CUBE_WEBHOOK_SECRET=my-shared-secret \
python3 wecom_bridge.py
```

Every sandbox event is relayed to the group as a markdown message. The same
pattern works for Slack, Feishu, or any generic HTTP alerting endpoint — swap
the `send_to_wecom` function for your target's API.
