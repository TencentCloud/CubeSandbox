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

The receiver converts `event_id`, `event`, `sandbox_id`, `timestamp`, and `template_id` into a text message before forwarding.

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

## Real local-cluster verification

Run the shared real-cluster verifier from the CubeAPI/CubeMaster control node
after the cluster services are healthy:

```bash
./examples/webhook-receiver/verify-real-cluster.sh
```

The verifier builds and installs the current CubeAPI binary by default,
creates a template from
`sandbox-code:latest`, and verifies signed `created`, `paused`, `resumed`, and
`deleted` callbacks. It writes an evidence archive to
`/tmp/cube-webhook-real.tar.gz`.

The verifier does not start services, alter networking, or modify persistent
webhook configuration. It waits for any healthy compute node registered in
CubeMaster, so it can verify a multi-node cluster when run on its
CubeAPI/CubeMaster control node.

For a control node with non-default local addresses, configure the endpoints:

```bash
CUBE_API_URL=http://10.0.0.10:3000 \
CUBEMASTER_ADDRESS=10.0.0.10 \
CUBEMASTER_PORT=8089 \
./examples/webhook-receiver/verify-real-cluster.sh
```

The receiver is local to the control node by default. When CubeAPI needs to
reach a receiver on another host, bind it and publish its callback URL:

```bash
RECEIVER_HOST=0.0.0.0 \
WEBHOOK_ENDPOINT_URL=http://10.0.0.20:9000/webhook \
WEBHOOK_NO_PROXY=10.0.0.20,127.0.0.1,localhost \
./examples/webhook-receiver/verify-real-cluster.sh
```

If the registry is only reachable through a configured host proxy, set it
explicitly:

```bash
CUBEMASTER_PROXY=http://host.docker.internal:7897 \
  ./examples/webhook-receiver/verify-real-cluster.sh
```

## Expected payload

```json
{
  "event_id": "sandbox.created.sandbox-1.1782945600000000000",
  "event": "sandbox.created",
  "timestamp": "2026-07-01T20:00:00Z",
  "sandbox_id": "sandbox-1",
  "template_id": "template-1"
}
```

`template_id` is included when available. Use `event_id` for idempotency. Delivery is at-most-once with limited retries and is not persisted.
