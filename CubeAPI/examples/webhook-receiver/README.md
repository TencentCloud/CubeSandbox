# CubeAPI Webhook Receiver

[简体中文](./README_zh.md)

This example provides a local CubeAPI webhook receiver implemented with the Python standard library. It listens on `127.0.0.1:18080` and accepts `POST /webhook`.

## Prerequisites

This guide assumes that:

- A local CubeSandbox deployment is already working.
- CubeAPI is reachable at `http://127.0.0.1:3000`.
- A valid sandbox template ID is available.
- Python 3 and the Rust toolchain are installed.
- Port `18080` is available.
- You can temporarily stop and restart the existing CubeAPI service.

For installation from scratch, follow the main CubeSandbox deployment documentation before using this example.

## Quick Start

Use three terminals for the following steps. This keeps the receiver output, CubeAPI runtime logs, and lifecycle API commands separate during validation.

### 1. Start the Receiver

In the first terminal, start the receiver with HMAC-SHA256 verification enabled:

```bash
cd CubeAPI/examples/webhook-receiver
WEBHOOK_SECRET=test-secret python3 receiver.py
```

To test an unsigned endpoint, start the receiver without `WEBHOOK_SECRET` and omit the `secret` field from `CUBE_API_WEBHOOK_ENDPOINTS`:

```bash
python3 receiver.py
```

When signing is enabled, the receiver verifies `X-Cube-Webhook-Signature` using:

```text
timestamp + "." + delivery_id + "." + raw_request_body
```

The expected signature format is:

```text
v1=<lowercase-hex>
```

### 2. Start CubeAPI from This Branch

In the second terminal, build the current branch:

```bash
cd CubeAPI
cargo build
```

If an existing CubeAPI service is running, temporarily stop only that service:

```bash
sudo systemctl stop cube-sandbox-cube-api.service
```

Start the binary built from this branch:

```bash
CUBE_API_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:18080/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"test-secret","enabled":true,"allow_private_urls":true}]' \
  ./target/debug/cube-api
```

Keep this process running during validation.

> The deployed CubeAPI may depend on additional environment variables, configuration files, or a specific working directory. Preserve the settings used by the existing CubeAPI service when running the branch binary.

`allow_private_urls` is enabled only because this local receiver uses `127.0.0.1`. Private, loopback, and link-local targets are rejected by default. Do not enable this option for an untrusted endpoint.

### 3. Run the Lifecycle Check

In the third terminal, first confirm that CubeAPI is available:

```bash
curl -fsS http://127.0.0.1:3000/health
```

Then set a valid template ID:

```bash
export CUBE_TEMPLATE_ID=<your-template-id>
```

Create a sandbox and obtain its ID:

```bash
CREATE_RESPONSE=$(curl -fsS -X POST http://127.0.0.1:3000/sandboxes \
  -H 'Content-Type: application/json' \
  -d "{\"templateID\":\"${CUBE_TEMPLATE_ID}\",\"timeout\":300}")

SANDBOX_ID=$(printf '%s' "$CREATE_RESPONSE" | python3 -c \
  'import json, sys; print(json.load(sys.stdin)["sandboxID"])')
```

Pause, resume, and delete the sandbox:

```bash
curl -fsS -X POST \
  "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}/pause"

curl -fsS -X POST \
  "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}/resume" \
  -H 'Content-Type: application/json' \
  -d '{"timeout":300}'

curl -fsS -X DELETE \
  "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}"
```

If authentication is enabled, add the required authentication headers to each request.

### 4. Restore the Original CubeAPI Service

After validation, stop the foreground CubeAPI process with `Ctrl+C`, then restore the original service:

```bash
sudo systemctl start cube-sandbox-cube-api.service
sudo systemctl status cube-sandbox-cube-api.service --no-pager -l
```

The service name may differ in customized or non-systemd deployments.

## Expected Output

The receiver should print one callback for each lifecycle event:

```text
sandbox.created
sandbox.paused
sandbox.resumed
sandbox.deleted
```

Each notification contains delivery metadata and selected lifecycle fields. It is not a complete `Sandbox` object.

When `WEBHOOK_SECRET` is configured, the receiver prints an event only after the request passes HMAC verification. An invalid signature returns:

```text
401 invalid signature
```

## Troubleshooting

| Problem | Possible cause |
|---|---|
| `401 invalid signature` | The receiver secret does not match the endpoint secret, or a proxy changed the signed body or headers |
| `400 invalid JSON` | The request body is not valid UTF-8 JSON |
| Connection refused or retries | The receiver is not running, the port is unavailable, or the URL is incorrect |
| No callback received | The endpoint is disabled, the event is not subscribed, or CubeAPI was started without the webhook configuration |
| Private-address startup error | The local receiver requires `"allow_private_urls": true` |
| Receiver unavailable from a container | `127.0.0.1` refers to the container itself; use a reachable host address or service name |

## Alerting Adapter Pattern

CubeAPI emits generic HTTP JSON rather than vendor-specific robot messages.

To integrate with WeCom, Feishu, Slack, or another alerting service, use a small adapter that:

1. receives the CubeAPI webhook;
2. verifies the HMAC signature against the raw body;
3. converts the lifecycle event into the target service format; and
4. forwards it using credentials stored by the adapter.

Do not hard-code vendor protocols into CubeAPI, and do not include credentials in webhook payloads, application logs, or pull-request descriptions.
