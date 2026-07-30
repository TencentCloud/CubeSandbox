# Webhook Event Notifications

CubeSandbox can asynchronously notify HTTP endpoints after sandbox lifecycle
changes. CubeMaster writes lifecycle events to the existing Redis Stream and
CubeOps consumes them in a separate consumer group:

```text
CubeAPI / CubeOps control request
              |
              v
         CubeMaster ----> cube:v1:shared:sandbox:lifecycle:events
                                      |
                                      v
                                  CubeOps
                         filter / sign / retry / log
                                      |
                                      v
                              HTTP endpoint
```

CubeAPI is not part of Webhook delivery. An unavailable or slow receiver
therefore cannot delay sandbox API requests.

Supported events:

- `sandbox.created`
- `sandbox.deleted`
- `sandbox.paused`
- `sandbox.resumed`

The internal lifecycle `update` operation is not exposed as a Webhook event.

## CubeOps configuration

Add the following to `/etc/cube/ops.yaml`, or to the file selected by
`CUBE_OPS_CONFIG`:

```yaml
redis_url: "redis://:PASSWORD@127.0.0.1:6379/0"

webhook:
  enabled: true
  consumer_group: "cubeops-webhook"
  # Leave consumer_name empty to generate a unique name per process.
  consumer_name: ""
  read_block: "5s"
  pending_idle: "2m"
  workers: 8
  default_timeout: "3s"
  default_max_retries: 3
  initial_backoff: "200ms"
  max_backoff: "2s"
  endpoints:
    - name: local-receiver
      url: "http://127.0.0.1:9000/webhook"
      events:
        - sandbox.created
        - sandbox.deleted
        - sandbox.paused
        - sandbox.resumed
      secret: "change-me"
      timeout: "3s"
      max_retries: 3
```

Restart CubeOps after changing the static configuration. All replicas in one
CubeOps deployment must use the same endpoint configuration because the
consumer group distributes events between replicas.

For environment-only deployments, enable delivery and provide the endpoint
array as JSON:

```bash
export REDIS_URL='redis://:PASSWORD@127.0.0.1:6379/0'
export CUBE_OPS_WEBHOOK_ENABLED=true
export CUBE_OPS_WEBHOOK_ENDPOINTS='[
  {
    "name": "local-receiver",
    "url": "http://127.0.0.1:9000/webhook",
    "events": ["sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"],
    "secret": "change-me"
  }
]'
```

YAML is recommended when timeout and retry overrides are needed.

## Payload

```json
{
  "event_id": "80639f37-1b79-42c4-93ff-a33cd93c5eef",
  "event": "sandbox.created",
  "timestamp": "2026-07-01T20:00:00Z",
  "sandbox_id": "sandbox-1",
  "template_id": "template-1",
  "host_id": "node-1",
  "host_ip": "10.0.0.1",
  "instance_type": "cubebox",
  "metadata": {
    "auto_pause": true,
    "auto_resume": true,
    "timeout_seconds": 300
  }
}
```

`event_id` is a UUID generated once by CubeMaster and remains unchanged across
delivery retries. The Redis Stream ID is retained only as an internal consumer
cursor. Receivers should store `event_id` and handle duplicate deliveries
idempotently.

Context fields are best effort. Create events contain the lifecycle metadata
snapshot. Delete events always contain `sandbox_id`; pause/resume events also
contain their actor and source in `metadata`.

## Signature verification

When an endpoint has a `secret`, CubeOps adds:

```text
X-Cube-Webhook-Timestamp: <unix seconds>
X-Cube-Webhook-Signature: sha256=<hex HMAC>
```

The signed bytes are:

```text
<timestamp>.<raw request body>
```

Python verification:

```python
import hashlib
import hmac

signed = timestamp.encode() + b"." + raw_body
expected = "sha256=" + hmac.new(
    secret.encode(), signed, hashlib.sha256
).hexdigest()
valid = hmac.compare_digest(expected, signature)
```

Receivers should also reject timestamps outside a short tolerance window to
limit replay.

## Delivery semantics

- CubeMaster appends the event without waiting for an HTTP receiver.
- CubeOps reads through the `cubeops-webhook` Redis consumer group.
- Non-2xx responses, timeouts and connection errors use bounded exponential
  backoff.
- A process crash before acknowledgement leaves the entry pending; another
  CubeOps replica can reclaim it after `pending_idle`.
- When the configured retry budget is exhausted, CubeOps records the final
  error and acknowledges the entry. There is currently no dead-letter queue.
- Delivery is at-least-once while an event is pending, so duplicates are
  possible and `event_id` must be used for receiver-side deduplication.

## Local validation and Enterprise WeChat

Start the standard-library example receiver:

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 9000 \
  --secret change-me
```

After CubeOps is configured, create, pause, resume and delete a sandbox through
CubeAPI or CubeOps. The receiver prints each verified payload.

The same receiver can act as an Enterprise WeChat adapter:

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 9000 \
  --secret change-me \
  --wechat-webhook-url \
  "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"
```

See [the example README](../../examples/webhook-receiver/README.md) for a
Redis Stream smoke test and real-cluster verification.
