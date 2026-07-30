# CubeSandbox Webhook Receiver Example

This directory contains a Python standard-library receiver for CubeOps
Webhook delivery. It validates the optional HMAC signature and can forward a
text message to an Enterprise WeChat robot.

## Start the receiver

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 9000 \
  --secret change-me
```

Configure CubeOps—not CubeAPI—to send to:

```text
http://127.0.0.1:9000/webhook
```

The complete YAML configuration is in
[`docs/guide/webhook.md`](../../docs/guide/webhook.md).

## Signature

With `secret` configured, CubeOps sends:

```text
X-Cube-Webhook-Timestamp: <unix seconds>
X-Cube-Webhook-Signature: sha256=<HMAC-SHA256(timestamp + "." + raw_body)>
```

The receiver rejects invalid or stale signatures. Change the replay window
with `--tolerance-seconds`.

## Redis Stream smoke test

With Redis and a webhook-enabled CubeOps already running:

```bash
REDIS_URL='redis://:PASSWORD@127.0.0.1:6379/0' \
./examples/webhook-receiver/verify-local-mock.sh
```

The script starts the receiver and injects representative CubeMaster lifecycle
entries into `cube:v1:shared:sandbox:lifecycle:events`. It expects four
callbacks in create, pause, resume and delete order.

This test does not start CubeOps because CubeOps requires the deployment
database. It checks `/health` before injecting events.

## Real cluster

On a one-click control node with the current CubeMaster and CubeOps installed:

```bash
./examples/webhook-receiver/verify-real-cluster.sh
```

The verifier temporarily configures the `cube-sandbox-cubeops.service` with a
unique consumer group, runs a real create/pause/resume/delete sequence through
CubeAPI, checks each signed callback before continuing, then restores the
service configuration and prior CubeOps binary.

## Enterprise WeChat

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 9000 \
  --secret change-me \
  --wechat-webhook-url \
  "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"
```

The receiver converts the generic CubeSandbox payload to a WeChat text
message. Keep this adapter between CubeOps and WeChat because robot payloads
do not use the generic Webhook schema.

## Example payload

```json
{
  "event_id": "80639f37-1b79-42c4-93ff-a33cd93c5eef",
  "event": "sandbox.created",
  "timestamp": "2026-07-01T20:00:00Z",
  "sandbox_id": "sandbox-1",
  "template_id": "template-1",
  "host_id": "node-1",
  "instance_type": "cubebox"
}
```

Receivers must use `event_id` for idempotency because Redis pending recovery
can cause duplicate delivery.
