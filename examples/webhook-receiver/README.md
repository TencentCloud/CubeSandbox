# CubeAPI Webhook Receiver

This standard-library Python receiver prints each CubeAPI lifecycle event and
optionally verifies its HMAC-SHA256 signature.

```bash
export CUBE_WEBHOOK_SECRET=change-me
python3 receiver.py
```

Configure CubeAPI in another terminal:

```bash
export WEBHOOK_URLS=http://127.0.0.1:8088/webhook
export WEBHOOK_SECRET=change-me
export WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
```

The receiver prints each JSON payload. The payload always includes `event`,
`timestamp`, and `level`; lifecycle events include `sandbox_id`, while create
events also include `template_id` when CubeAPI receives it.

Set `WEBHOOK_RECEIVER_HOST` and `WEBHOOK_RECEIVER_PORT` to change the listen
address. Do not expose the example receiver to an untrusted network.
