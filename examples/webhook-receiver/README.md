# CubeSandbox Webhook Receiver

This example prints CubeAPI webhook events and optionally verifies the
`X-Cube-Signature` HMAC-SHA256 header.

For the full integration guide, see
[`docs/guide/webhooks.md`](../../docs/guide/webhooks.md).

## Run the receiver

```bash
cd examples/webhook-receiver
WEBHOOK_HOST=0.0.0.0 WEBHOOK_PORT=9000 WEBHOOK_SECRET=dev-secret \
  python3 receiver.py
```

## Enable CubeAPI webhooks

Start `cube-api` with `CUBE_API_WEBHOOKS` set to a JSON array:

```bash
export CUBE_API_WEBHOOKS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "dev-secret",
    "timeout_secs": 3,
    "max_retries": 3
  }
]'
```

Each POST body is JSON and includes `event`, `timestamp`, `sandbox_id`, and any
extra context attached by CubeAPI, such as `template_id` on create events.

Signed requests use:

- `X-Cube-Event`: event name
- `X-Cube-Timestamp`: Unix timestamp seconds
- `X-Cube-Signature`: `sha256=<hex hmac>`

The signed string is:

```text
<X-Cube-Timestamp>.<raw request body>
```
