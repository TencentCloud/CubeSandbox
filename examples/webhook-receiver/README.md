# CubeSandbox Webhook Receiver Example

This example starts a small HTTP server for testing CubeAPI Webhook delivery.
It uses only the Python standard library.

## Start the receiver

```bash
cd examples/webhook-receiver
python3 receiver.py --port 9000 --secret change-me
```

CubeAPI should point to:

```text
http://127.0.0.1:9000/webhook
```

## CubeAPI config

CubeAPI reads webhook config from environment variables. Start CubeAPI with:

```bash
export WEBHOOK__ENABLED=true
export WEBHOOK__ENDPOINTS__0__NAME=local-receiver
export WEBHOOK__ENDPOINTS__0__URL=http://127.0.0.1:9000/webhook
export WEBHOOK__ENDPOINTS__0__EVENTS__0=sandbox.created
export WEBHOOK__ENDPOINTS__0__EVENTS__1=sandbox.deleted
export WEBHOOK__ENDPOINTS__0__EVENTS__2=sandbox.paused
export WEBHOOK__ENDPOINTS__0__EVENTS__3=sandbox.resumed
export WEBHOOK__ENDPOINTS__0__SECRET=change-me
```

## Signature verification

CubeAPI signs requests when `secret` is configured:

```text
X-Cube-Webhook-Timestamp: <unix seconds>
X-Cube-Webhook-Signature: sha256=<hex hmac>
```

The signed payload is:

```text
<timestamp>.<raw_body>
```

The example receiver verifies the signature and rejects stale timestamps using `--tolerance-seconds`.

## Enterprise WeChat forwarding

Enterprise WeChat robot payloads are not compatible with the generic CubeSandbox Webhook payload. Use the receiver as a small adapter:

```bash
python3 receiver.py \
  --port 9000 \
  --secret change-me \
  --wechat-webhook-url "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"
```

The receiver converts `event_id`, `event`, `sandbox_id`, `timestamp`, `template_id`, and host context into a text message before forwarding.

## Local mock verification

When a local host cannot run the CubeSandbox compute data plane, run the
CubeAPI lifecycle webhook path against the included CubeMaster contract mock:

```bash
./examples/webhook-receiver/verify-local-mock.sh
```

The script starts the mock, this receiver, and a locally built CubeAPI binary.
It verifies create, pause, resume, and delete responses, then prints the four
received webhook payloads. Set `CUBE_API_BIN` to override the default binary
path (`CubeAPI/target/debug/cube-api`).

## Expected payload

```json
{
  "event_id": "sandbox.created.sandbox-1.1782945600000000000",
  "event": "sandbox.created",
  "timestamp": "2026-07-01T20:00:00Z",
  "sandbox_id": "sandbox-1",
  "template_id": "template-1",
  "host_id": "node-1",
  "host_ip": "10.0.0.1",
  "instance_type": "cubebox"
}
```

Use `event_id` for idempotency. Delivery is at-most-once with limited retries and is not persisted.
