[English](./README.md) | [中文](./README_zh.md)

# CubeSandbox Webhook Receiver

This dependency-free Python example receives CubeSandbox lifecycle webhooks,
verifies the optional HMAC-SHA256 signature, prints each event as one JSON line,
and can optionally forward a text notification to a WeCom group robot.

## Files

- `receiver.py`: threaded HTTP receiver with payload and signature validation
- `send_test_event.py`: sends one signed sample event without requiring CubeAPI
- `test_receiver.py`: unit tests for signature and payload validation
- `.env.example`: environment variable reference

## Quick Start

Python 3.9 or later is sufficient; no packages need to be installed.

Terminal 1:

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 receiver.py
```

Terminal 2:

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 send_test_event.py
```

Expected output:

```text
receiver returned HTTP 204
```

The receiver prints a JSON line containing `delivery_id`, `event`, `timestamp`,
and `sandbox_id`. Run its unit tests with:

```bash
python3 -m unittest -v test_receiver.py
```

## Connect CubeAPI

Set `CUBE_API_WEBHOOKS` before starting CubeAPI. It is a JSON array, so one
CubeAPI process can deliver to multiple independently subscribed endpoints.

```bash
export CUBE_API_WEBHOOKS='[
  {
    "name": "local-receiver",
    "url": "http://127.0.0.1:8088/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "local-development-secret"
  }
]'
```

Restart CubeAPI, set a ready template ID, then run the complete lifecycle:

```bash
export CUBE_API_URL="${CUBE_API_URL:-http://127.0.0.1:3000}"
export CUBE_TEMPLATE_ID="replace-with-a-ready-template-id"

SANDBOX_ID="$(
  curl -fsS \
    -H 'Content-Type: application/json' \
    -d "{\"templateID\":\"${CUBE_TEMPLATE_ID}\",\"timeout\":300}" \
    "${CUBE_API_URL}/sandboxes" |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["sandboxID"])'
)"
printf 'sandbox: %s\n' "${SANDBOX_ID}"

curl -fsS -o /dev/null -X POST \
  "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}/pause"
curl -fsS -o /dev/null -X POST \
  -H 'Content-Type: application/json' \
  -d '{"timeout":300}' \
  "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}/resume"
curl -fsS -o /dev/null -X DELETE \
  "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}"
```

The receiver should print callbacks for `sandbox.created`, `sandbox.paused`,
`sandbox.resumed`, and `sandbox.deleted`. Deliveries run concurrently, so their
arrival order is not guaranteed. The created and resumed payloads also carry
`template_id`. Add the deployment's authorization header to each `curl` command
when CubeAPI authentication is enabled.

For one-click installations, put the same variable in the installation `.env`
file and restart CubeAPI:

```bash
sudo systemctl restart cube-sandbox-cube-api.service
sudo journalctl -u cube-sandbox-cube-api.service -f
```

See the [Webhook guide](../../docs/guide/webhooks.md) for all delivery settings,
payload fields, retry behavior, deployment examples, and troubleshooting.

## WeCom Forwarding

Set `WECOM_BOT_URL` on the receiver, not on CubeAPI. The receiver translates the
CubeSandbox payload into the format expected by a WeCom group robot.

```bash
export WECOM_BOT_URL='https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me'
python3 receiver.py
```

If WeCom forwarding fails, the receiver returns HTTP 502. CubeAPI then retries
the original delivery, so consumers should use `X-CubeSandbox-Delivery` for
deduplication in production.

## Security and Limits

- The receiver binds to `127.0.0.1` by default. Put it behind authenticated TLS
  before exposing it outside a trusted host or network.
- Keep `WEBHOOK_SECRET` out of source control and use a high-entropy value in
  production.
- Signature verification covers the exact request body bytes. Parse JSON only
  after verification.
- The example prints events and keeps no durable delivery history.
- Stop the receiver with `Ctrl-C`; it creates no containers or persistent files.
