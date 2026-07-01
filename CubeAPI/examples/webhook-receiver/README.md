# CubeAPI Webhook Receiver

This example is a local CubeAPI webhook receiver implemented with the Python standard library. It listens on `127.0.0.1:18080` and accepts `POST /webhook`.

## Start the Receiver

Without signature verification:

```bash
python3 receiver.py
```

With HMAC-SHA256 signature verification:

```bash
WEBHOOK_SECRET=test-secret python3 receiver.py
```

The receiver validates JSON before processing it. When `WEBHOOK_SECRET` is set, it also validates `X-Cube-Webhook-Signature` using the exact signing input:

```text
timestamp + "." + delivery_id + "." + raw_request_body
```

The expected header format is `v1=<lowercase-hex>`.

## Configure CubeAPI

Configure the same secret on the CubeAPI endpoint:

```bash
CUBE_API_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:18080/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"test-secret","enabled":true}]'
```

Export the value before starting CubeAPI, for example:

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:18080/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"test-secret","enabled":true}]'
```

Do not set `WEBHOOK_SECRET` in the receiver when testing an unsigned endpoint.

Real CubeAPI end-to-end delivery testing is performed in stage 5. This stage only provides the receiver and its documentation.
