# CubeSandbox Webhook Receiver

This example starts a local HTTP receiver for CubeAPI webhook events. It uses
only the Python standard library.

## Run

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=dev-secret python3 receiver.py
```

The receiver listens on `http://127.0.0.1:9000/webhook` by default.

Configure CubeAPI with a webhook endpoint:

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "dev-secret"
  }
]'
```

Start CubeAPI with the environment variable above. When a sandbox is created,
paused, resumed, or deleted, the receiver prints the event payload.

## Signature

When `secret` is set, CubeAPI sends these headers:

- `X-Cube-Timestamp`
- `X-Cube-Nonce`
- `X-Cube-Signature-256`

The signature is:

```text
sha256=HMAC_SHA256(secret, timestamp + "." + nonce + "." + raw_body)
```

Set `WEBHOOK_SECRET` to the same value in the receiver to verify requests.

## Test

```bash
python3 -m unittest discover -s examples/webhook-receiver
```
