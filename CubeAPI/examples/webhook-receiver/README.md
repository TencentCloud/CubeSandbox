# CubeAPI Webhook Receiver

This example is a local CubeAPI webhook receiver implemented with the Python standard library. It listens on `127.0.0.1:18080` and accepts `POST /webhook`.

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

## Configure CubeAPI

Export the endpoint configuration before starting CubeAPI. The endpoint secret must match `WEBHOOK_SECRET`:

```bash
cd CubeAPI
export CUBE_API_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:18080/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"test-secret","enabled":true}]'
cargo run
```

Do not set `WEBHOOK_SECRET` in the receiver when testing an unsigned endpoint.

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

The receiver should print `sandbox.created`, `sandbox.paused`, `sandbox.resumed`, and `sandbox.deleted`. Each notification contains delivery metadata and selected event fields; it is not a complete `Sandbox` object.

If CubeAPI authentication is enabled in your deployment, add the required authentication headers documented by that deployment to each curl request.

## Troubleshooting

- `401 invalid signature`: `WEBHOOK_SECRET` does not match the endpoint `secret`, or a proxy changed the raw request body or signed headers.
- `400 invalid JSON`: the request body is not valid UTF-8 JSON.
- Connection refused or delivery retries: the receiver is not running, the port is unavailable, or the configured address is wrong.
- No callback received: the endpoint may be disabled, its `events` list may not match, or CubeAPI may have been started before loading `CUBE_API_WEBHOOK_ENDPOINTS`.
- When CubeAPI runs in a container, `127.0.0.1` refers to that container. Use a reachable host address or container service name for the receiver.

## Alerting Adapter Pattern

CubeAPI emits generic HTTP JSON and does not include vendor-specific robot protocols. For WeCom, Feishu, Slack, or another HTTP alerting service, deploy a small adapter that:

1. receives the CubeAPI webhook;
2. verifies its HMAC against the raw body;
3. formats the event for the target service; and
4. forwards it using credentials held by the adapter.

Do not hard-code vendor protocols in CubeAPI, and do not copy secrets into logs, event payloads, or pull-request descriptions.
