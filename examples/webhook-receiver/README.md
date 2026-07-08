# CubeSandbox Webhook Receiver Example

This example starts a small HTTP server that receives CubeAPI Webhook events,
prints the JSON payload, and optionally verifies `X-Cube-Signature-256`.

## Run

```bash
cd examples/webhook-receiver
python3 receiver.py
```

With signature verification:

```bash
export CUBE_WEBHOOK_SECRET_0='change-me'
python3 receiver.py
```

The server listens on `http://0.0.0.0:8088/webhook` by default. Override it with:

```bash
WEBHOOK_RECEIVER_HOST=127.0.0.1 WEBHOOK_RECEIVER_PORT=8089 python3 receiver.py
```

## CubeAPI Config

Create `/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml`:

```toml
[delivery]
queue_size = 10000
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10
max_in_flight = 100

[[endpoints]]
name = "local-dev"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

The `url` must be reachable from the CubeAPI process. Use `127.0.0.1` only when
the receiver runs on the same host as CubeAPI. In `dev-env`, if this receiver is
running on the host machine and CubeAPI is running inside the VM, use
`http://10.0.2.2:8088/webhook`.

Add these to `/usr/local/services/cubetoolbox/.one-click.env` and restart CubeAPI:

```bash
CUBE_API_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
```

```bash
sudo systemctl restart cube-sandbox-cube-api.service
```

Create, pause, resume, and delete a sandbox. The receiver should print one JSON
object per delivered event.

## Failure Simulation

Point `url` at an unused port, restart CubeAPI, and trigger an event. The sandbox
API call should still succeed while CubeAPI logs retry and final delivery errors.
