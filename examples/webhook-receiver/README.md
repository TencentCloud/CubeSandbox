# Webhook Receiver Example

[中文文档](README_zh.md)

This example starts a lightweight HTTP receiver for CubeAPI webhook callbacks. It is useful for quickly verifying webhook delivery locally and can also serve as a reference implementation for building your own receiver.

Features:

* Python standard library only; no `pip install` required
* HMAC-SHA256 signature verification
* `/health` endpoint for connectivity checks
* Prints event names, sandbox IDs, and related fields to stdout

For full configuration details, including multiple endpoints, TOML configuration, retry tuning, and WeCom integration, see [Webhook Events](../../docs/guide/webhook-events.md).

## Quick start

```bash
python3 receiver.py
```

After startup, the receiver listens on `:18080` by default. Keep this terminal running, then send a test request from another terminal.

You can customize the runtime behavior with environment variables:

| Environment variable | Description                                                            |
| -------------------- | ---------------------------------------------------------------------- |
| `WEBHOOK_PORT`       | Receiver listen port. Defaults to `18080`                              |
| `WEBHOOK_SECRET`     | HMAC verification secret. If empty, signature verification is disabled |

For example:

```bash
WEBHOOK_PORT=19090 WEBHOOK_SECRET=secret python3 receiver.py
```

## Send a local test payload

```bash
curl -s -X POST http://localhost:18080/webhook \
  -H "Content-Type: application/json" \
  -d '{"events":[{"timestamp":"2026-06-26T12:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"sbx-1","template_id":"tmpl-1"}]}'
```

The receiver should print output similar to:

```text
[<receive-time>] received 1 event(s)
  [2026-06-26T12:00:00Z] sandbox.created
    sandbox_id : sbx-1
    template_id: tmpl-1
```

The first line is the time when the receiver actually received the request. The timestamp on the event line comes from the webhook payload.

## Enable HMAC and test the signature

Start the receiver with a secret:

```bash
WEBHOOK_SECRET=secret python3 receiver.py
```

Then calculate the signature and send the request from another terminal:

```bash
SECRET="secret"
BODY='{"events":[{"timestamp":"2026-06-26T12:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"sbx-1","template_id":"tmpl-1"}]}'
SIG="sha256=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | cut -d' ' -f2)"

curl -s -X POST http://localhost:18080/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Webhook-Signature: $SIG" \
  -d "$BODY"
```

The signature is computed over the **raw HTTP request body bytes**. Do not verify the signature after `json.loads()` followed by `json.dumps()`, because field ordering or whitespace changes can cause a signature mismatch.

## Send CubeAPI events to this receiver

Append the webhook configuration to `.one-click.env`. After a one-click installation, this file is usually located at:

```text
/usr/local/services/cubetoolbox/.one-click.env
```

Configuration:

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://127.0.0.1:18080/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
```

Restart CubeAPI:

```bash
systemctl restart cube-sandbox-cube-api.service
```

Check the startup log and confirm that webhook delivery is enabled:

```bash
journalctl -u cube-sandbox-cube-api.service -n 20 --no-pager
```

The log should contain output similar to:

```text
INFO cube_api: cube-api starting ... webhook_enabled=true webhook_endpoint_count=1
INFO cube_api: webhook endpoint configured url=http://127.0.0.1:18080/webhook
```

## Trigger lifecycle events

This receiver can be used to verify the following sandbox lifecycle events:

| Event                      | Trigger                                     |
| -------------------------- | ------------------------------------------- |
| `sandbox.created`          | `POST /sandboxes` succeeds                  |
| `sandbox.timeout.updated`  | `POST /sandboxes/:id/timeout` succeeds      |
| `sandbox.refreshed`        | `POST /sandboxes/:id/refreshes` succeeds    |
| `sandbox.paused`           | `POST /sandboxes/:id/pause` succeeds        |
| `sandbox.resumed`          | `POST /sandboxes/:id/resume` succeeds       |
| `sandbox.deleted`          | `DELETE /sandboxes/:id` succeeds            |

Run the following requests in sequence to trigger all events:

```bash
# Create a sandbox
curl -X POST http://localhost:3000/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "your-template-id"}'

# Update the sandbox timeout
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/timeout \
  -H "Content-Type: application/json" \
  -d '{"timeout": 300}'

# Refresh the sandbox timeout
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/refreshes \
  -H "Content-Type: application/json" \
  -d '{"duration": 600}'

# Pause the sandbox
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/pause

# Resume the sandbox
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/resume \
  -H "Content-Type: application/json" \
  -d '{}'

# Delete the sandbox
curl -X DELETE http://localhost:3000/sandboxes/<sandbox_id>
```

After each successful operation, the receiver terminal should print the corresponding webhook event.

## FAQ

**The log does not contain `webhook_enabled=true`**

Check that the variable name in `.one-click.env` is `CUBE_API_WEBHOOK_ENABLED`, not `ENABLE`. Restart CubeAPI after fixing it.

**The receiver does not receive events**

First confirm that the CubeAPI machine can reach the receiver:

```bash
curl http://<receiver>:18080/health
```

A healthy receiver should return:

```json
{"status":"ok"}
```

Also check whether the endpoint URL in the CubeAPI startup log is correct.

**HMAC signature mismatch**

Confirm that the `hmac_secret` configured on the CubeAPI side exactly matches `WEBHOOK_SECRET` on the receiver side. The signature must be computed over the raw body bytes. Do not re-serialize JSON before verification.

## Notes

* The receiver does not print secret material.
* The receiver only accepts `POST /webhook`; other paths return 404.
* The receiver listens on `0.0.0.0` by default, so it can be reached by CubeAPI running in another container or VM.
* For more configuration and troubleshooting details, see [Webhook Events](../../docs/guide/webhook-events.md).
