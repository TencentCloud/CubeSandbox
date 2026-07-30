# CubeSandbox Webhook Receiver Example

This example runs a small HTTP server that receives webhook batches delivered
by CubeOps, prints their JSON payload, and optionally verifies
`X-Cube-Signature-256`.

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

The server listens on `http://0.0.0.0:8088/webhook` by default. Override it
with `WEBHOOK_RECEIVER_HOST` and `WEBHOOK_RECEIVER_PORT`.

## CubeOps Config

Create `/usr/local/services/cubetoolbox/CubeOps/webhooks.toml`:

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

The `url` must be reachable from CubeOps. In `dev-env`, if the receiver runs
on the host and CubeOps runs inside the VM, use
`http://10.0.2.2:8088/webhook`.

Add the following values to
`/usr/local/services/cubetoolbox/.one-click.env`, then restart CubeOps:

```bash
CUBE_OPS_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeOps/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
sudo systemctl restart cube-sandbox-cubeops.service
```

Create, pause, resume, and delete a sandbox. With `batch_size = 1`, the
receiver should print one JSON batch for each delivered event.

Delivery is best effort. Deduplicate completed external batches by `batch_id`,
which remains stable across retries of that batch. Record the completed batch
atomically before external side effects; batches may arrive zero, one, or
multiple times and separate batches may arrive out of order.

## Failure Simulation

Point `url` at an unused port, restart CubeOps, and trigger an event. The
sandbox API call still succeeds while CubeOps logs retry and final delivery
errors.
