# CubeAPI Webhook Receiver

This example is a local CubeAPI webhook receiver implemented with the Python standard library. It listens on `127.0.0.1:18080` and accepts `POST /webhook`.

## Prerequisites

This guide verifies webhook delivery against a working local CubeSandbox deployment. It does not replace the full CubeSandbox installation guide.

Before starting, make sure that:

- A local CubeSandbox deployment is already working, or you are deploying one from this source tree using the normal CubeSandbox deployment process.
- The lifecycle curl commands below assume CubeAPI is reachable at `http://127.0.0.1:3000` after it starts.
- You have a valid template ID that can create a sandbox.
- You can stop and restart the local CubeAPI service if you are validating against an existing deployment.
- Port `18080` is available for the example receiver.
- Python 3 and the Rust toolchain are available on the validation machine.

## Start the Receiver

Without signature verification:

```bash
cd CubeAPI/examples/webhook-receiver
python3 receiver.py
```

With HMAC-SHA256 signature verification:

```bash
cd CubeAPI/examples/webhook-receiver
WEBHOOK_SECRET=test-secret python3 receiver.py
```

The receiver validates JSON before processing it. When `WEBHOOK_SECRET` is set, it also validates `X-Cube-Webhook-Signature` using the exact signing input:

```text
timestamp + "." + delivery_id + "." + raw_request_body
```

The expected header format is `v1=<lowercase-hex>`.

## Configure and Start CubeAPI

The lifecycle check below requires a working local CubeSandbox deployment. The webhook receiver only receives notifications; the sandbox lifecycle APIs still require the normal CubeSandbox services, templates, and runtime components to be available.

Configure `CUBE_API_WEBHOOK_ENDPOINTS` before starting the CubeAPI process. The endpoint secret must match `WEBHOOK_SECRET`.

### Option A: Validate Against an Existing Local CubeSandbox Deployment

Use this option when a local CubeSandbox deployment is already running and you want to validate this source tree or pull request. In this case, starting only the webhook receiver is not enough if the deployed CubeAPI service is still an older build without these webhook changes.

Keep the other CubeSandbox services running, temporarily stop only the existing CubeAPI service, and run the CubeAPI binary built from this source tree:

```bash
cd CubeAPI
cargo build

sudo systemctl stop cube-sandbox-cube-api.service

CUBE_API_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:18080/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"test-secret","enabled":true,"allow_private_urls":true}]' \
  ./target/debug/cube-api
```

If your local CubeAPI service depends on additional environment variables, config files, or a specific working directory, run the debug binary with the same required service environment.

Keep this foreground `cube-api` process running while you perform the lifecycle check below. After validation, stop it with `Ctrl+C` and restore the original service:

```bash
sudo systemctl start cube-sandbox-cube-api.service
sudo systemctl status cube-sandbox-cube-api.service --no-pager -l
```

The exact service name may differ in non-systemd or customized deployments. The important point is that the running CubeAPI process must be built from this source tree and must be started with `CUBE_API_WEBHOOK_ENDPOINTS` already set.

### Option B: Deploy CubeSandbox from This Source Tree

Use this option when you are deploying a new local CubeSandbox environment from this source tree. Follow the normal CubeSandbox deployment process for this repository or branch. Make sure the CubeAPI service created by that deployment is built from this source tree, and configure `CUBE_API_WEBHOOK_ENDPOINTS` in the CubeAPI service environment before the service starts.

The exact way to set the service environment depends on the deployment method. For a foreground or manual start, the CubeAPI process would use an environment variable like this; for systemd, Docker, or Compose deployments, add the equivalent variable to the CubeAPI service environment before the service starts:

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:18080/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"test-secret","enabled":true,"allow_private_urls":true}]'
```

Do not start a second `cargo run` CubeAPI process on top of an already running deployment. If a CubeAPI service is already listening on `127.0.0.1:3000`, either configure that service before it starts or use Option A to temporarily replace it for validation.

Do not set `WEBHOOK_SECRET` in the receiver when testing an unsigned endpoint.

`"allow_private_urls": true` is required here because the receiver listens on the loopback address `127.0.0.1`, which CubeAPI rejects by default. It exists to make local development work; do not enable it for production endpoints unless the target really is a trusted internal or loopback receiver.

## End-to-End Lifecycle Check

Run the receiver and CubeAPI in separate terminals, then create, pause, resume, and delete one sandbox. Set a valid template ID first:

```bash
export CUBE_TEMPLATE_ID=<your-template-id>

CREATE_RESPONSE=$(curl -fsS -X POST http://127.0.0.1:3000/sandboxes \
  -H 'Content-Type: application/json' \
  -d "{\"templateID\":\"${CUBE_TEMPLATE_ID}\",\"timeout\":300}")

SANDBOX_ID=$(printf '%s' "$CREATE_RESPONSE" | python3 -c \
  'import json, sys; print(json.load(sys.stdin)["sandboxID"])')

curl -fsS -X POST "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}/pause"
curl -fsS -X POST "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}/resume" \
  -H 'Content-Type: application/json' \
  -d '{"timeout":300}'
curl -fsS -X DELETE "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}"
```

If CubeAPI authentication is enabled in your deployment, add the required authentication headers documented by that deployment to each curl request.

## Expected Output

A successful run prints one request per lifecycle event in the receiver terminal. You should see all of the following events:

```text
sandbox.created
sandbox.paused
sandbox.resumed
sandbox.deleted
```

When the lifecycle curl commands are run sequentially and the receiver is available, these events should normally appear in the same lifecycle order. Each notification contains delivery metadata and selected event fields; it is not a complete `Sandbox` object.

When `WEBHOOK_SECRET` is set, each request must pass HMAC verification before the event is printed. If verification fails, the receiver returns `401 invalid signature`.

## Troubleshooting

- `401 invalid signature`: `WEBHOOK_SECRET` does not match the endpoint `secret`, or a proxy changed the raw request body or signed headers.
- `400 invalid JSON`: the request body is not valid UTF-8 JSON.
- Connection refused or delivery retries: the receiver is not running, the port is unavailable, or the configured address is wrong.
- No callback received: the endpoint may be disabled, its `events` list may not match, or CubeAPI may have been started before loading `CUBE_API_WEBHOOK_ENDPOINTS`.
- CubeAPI fails to start with a "private, loopback, or link-local address" error: the receiver address is non-public, so the endpoint needs `"allow_private_urls": true`.
- When CubeAPI runs in a container, `127.0.0.1` refers to that container. Use a reachable host address or container service name for the receiver.

## Alerting Adapter Pattern

CubeAPI emits generic HTTP JSON and does not include vendor-specific robot protocols. For WeCom, Feishu, Slack, or another HTTP alerting service, deploy a small adapter that:

1. receives the CubeAPI webhook;
2. verifies its HMAC against the raw body;
3. formats the event for the target service; and
4. forwards it using credentials held by the adapter.

Do not hard-code vendor protocols in CubeAPI, and do not copy secrets into logs, event payloads, or pull-request descriptions.
