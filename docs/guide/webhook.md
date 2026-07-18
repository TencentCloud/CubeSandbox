# Webhook Event Notifications

CubeAPI can send asynchronous HTTP callbacks when sandbox lifecycle events succeed.

Supported events:

- `sandbox.created`
- `sandbox.deleted`
- `sandbox.paused`
- `sandbox.resumed`

Webhook delivery runs outside the sandbox create, delete, pause, and resume main paths. Receiver timeout or network failure does not fail the sandbox API request.

## CubeAPI Configuration

CubeAPI reads webhook settings from environment variables. Nested fields use `__` as the separator.

```bash
export WEBHOOK__ENABLED=true
export WEBHOOK__QUEUE_SIZE=1024
export WEBHOOK__DELIVERY_CONCURRENCY=64
export WEBHOOK__DEFAULT_TIMEOUT_MS=3000
export WEBHOOK__DEFAULT_MAX_RETRIES=3
export WEBHOOK__DEFAULT_INITIAL_BACKOFF_MS=200
export WEBHOOK__DEFAULT_MAX_BACKOFF_MS=2000
export WEBHOOK__ENDPOINTS__0__NAME=local-receiver
export WEBHOOK__ENDPOINTS__0__URL=http://127.0.0.1:9000/webhook
export WEBHOOK__ENDPOINTS__0__EVENTS__0=sandbox.created
export WEBHOOK__ENDPOINTS__0__EVENTS__1=sandbox.deleted
export WEBHOOK__ENDPOINTS__0__EVENTS__2=sandbox.paused
export WEBHOOK__ENDPOINTS__0__EVENTS__3=sandbox.resumed
export WEBHOOK__ENDPOINTS__0__SECRET=change-me
export WEBHOOK__ENDPOINTS__0__TIMEOUT_MS=3000
export WEBHOOK__ENDPOINTS__0__MAX_RETRIES=3
```

CubeAPI registers the HTTP webhook logger in `MultiLogger` at startup. Restart CubeAPI after changing endpoints, secrets, or retry values.

Endpoint URL validation only requires `http` or `https` scheme and a non-empty host. Private and loopback addresses are allowed for local receivers and internal alerting systems.

## Payload

```json
{
  "event_id": "sandbox.created.sandbox-1.1782945600000000000",
  "event": "sandbox.created",
  "timestamp": "2026-07-01T20:00:00Z",
  "sandbox_id": "sandbox-1",
  "template_id": "template-1",
  "host_id": "node-1",
  "host_ip": "10.0.0.1",
  "instance_type": "cubebox",
  "metadata": {}
}
```

Fields:

- `event_id`: Unique event identifier in `<event>.<sandbox_id>.<unix_nano>` format. Use it for receiver-side idempotency.
- `event`: Event type.
- `timestamp`: Event time in UTC RFC3339Nano format.
- `sandbox_id`: Sandbox ID.
- `template_id`, `host_id`, `host_ip`, `instance_type`: Best-effort context fields.
- `metadata`: Reserved for future context.

## Signature

If an endpoint has `secret`, CubeAPI adds:

```text
X-Cube-Webhook-Timestamp: <unix seconds>
X-Cube-Webhook-Signature: sha256=<hex hmac>
```

Signature payload:

```text
<timestamp>.<raw_body>
```

Python verification example:

```python
import hashlib
import hmac

payload = timestamp.encode() + b"." + raw_body
expected = "sha256=" + hmac.new(secret.encode(), payload, hashlib.sha256).hexdigest()
ok = hmac.compare_digest(expected, signature)
```

Receivers should reject stale timestamps to reduce replay risk.

## Delivery semantics

Webhook delivery is at-most-once with limited retries. Events are queued in memory and are not persisted. If the queue is full or delivery concurrency is exhausted, CubeAPI drops the event and logs a warning.

Retries use exponential backoff with a bounded maximum backoff. Delivery work runs with a concurrency limit so failed endpoints do not block sandbox API requests.

## Local validation

Start the example receiver:

```bash
cd examples/webhook-receiver
python3 receiver.py --port 9000 --secret change-me
```

Configure CubeAPI to call `http://127.0.0.1:9000/webhook`, restart CubeAPI, then create, pause, resume, and delete a sandbox through CubeAPI. The receiver should print one payload for each subscribed event.

To verify non-blocking behavior, stop the receiver and repeat sandbox operations. Sandbox API calls should still succeed while CubeAPI logs delivery failures and retries.

## Enterprise WeChat

Enterprise WeChat robot payloads are not compatible with the generic CubeSandbox payload. Use the example receiver as an adapter:

```bash
python3 receiver.py \
  --port 9000 \
  --secret change-me \
  --wechat-webhook-url "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"
```

The receiver converts the CubeSandbox event into a WeChat text message and forwards it.
