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
event_queue_capacity = 10000
max_outstanding_deliveries = 1000
max_concurrent_requests = 100
default_batch_size = 1
flush_interval_secs = 5
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10

[[endpoints]]
name = "local-dev-lifecycle"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
batch_size = 1
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

The capacity settings bound events waiting in the channel, endpoint batch
deliveries that have not completed, and HTTP attempts performing network I/O,
respectively:

```text
event_queue_capacity -> max_outstanding_deliveries -> max_concurrent_requests
```

They count different units, so there is no mandatory numeric ordering. It is
normally useful for `max_outstanding_deliveries` to exceed
`max_concurrent_requests`, allowing deliveries in retry backoff to retain their
task slot without consuming all available HTTP request concurrency.

You can reuse the same `url` in another endpoint entry for a disjoint high-volume
event set, for example `api.request` and `api.response`, and give that entry a
larger `batch_size`. CubeAPI rejects duplicate subscriptions for the same `url`
and event.

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

Create, pause, resume, and delete a sandbox. With `batch_size = 1`, the receiver
should print one JSON batch per delivered event.

## Failure Simulation

Point `url` at an unused port, restart CubeAPI, and trigger an event. The sandbox
API call should still succeed while CubeAPI logs retry and final delivery errors.
