# Webhook Events

CubeAPI supports pushing sandbox lifecycle events to external HTTP endpoints. It currently covers six event types: sandbox creation, timeout update, refresh, pausing, resuming, and deletion. These events can be used for real-time monitoring, alerting platform integration, and sandbox usage analytics.

If you need to integrate with tools such as WeCom that only accept a fixed message format, add an adapter service between CubeAPI and the third-party webhook. See [WeCom integration](#wecom-integration) below.

## Quick start

> This section assumes you have completed CubeSandbox deployment via [Quick Start](./quickstart.md) or [PVM Deployment](./pvm-deploy.md). If you are unfamiliar with systemd service management, see [Service Management and Logs](./service-management.md).

### Start the example receiver

The `examples/` directory in the `CubeSandbox` repository provides a webhook receiver based on the Python standard library. No additional dependencies are required:

```bash
python3 examples/webhook-receiver/receiver.py
```

The receiver listens on `:18080` by default. To change the port or enable HMAC verification, configure the following environment variables:

| Environment variable | Description                                                  |
| -------------------- | ------------------------------------------------------------ |
| `WEBHOOK_PORT`       | Receiver listen port. Defaults to `18080`                    |
| `WEBHOOK_SECRET`     | HMAC verification secret. If empty, signature verification is disabled |

First, send a test event with curl to confirm that the receiver is working:

```bash
curl -X POST http://localhost:18080/webhook \
  -H "Content-Type: application/json" \
  -d '{"events":[{"timestamp":"2026-01-01T12:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"test-1","template_id":"tmpl-1"}]}'
```

The receiver should print output similar to:

```text
[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:00Z] sandbox.created
    sandbox_id : test-1
    template_id: tmpl-1
```

The first line is the time when the receiver actually received the request. The timestamp on the event line comes from the webhook payload.

### Configure CubeAPI

::: tip Location of `.one-click.env`
After a one-click installation, `.one-click.env` is located at:

```text
/usr/local/services/cubetoolbox/.one-click.env
```

:::

Edit `.one-click.env` and append the webhook configuration:

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://127.0.0.1:18080/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
```

If the receiver and CubeAPI are not running on the same machine, replace `127.0.0.1` with an address that CubeAPI can reach.

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

This is the simplest approach using systemd and environment variables. It can register only one endpoint at a time. For multiple endpoints, config file usage, HMAC signing, or CLI flags, see [Configure webhooks](#configure-webhooks) below.

### Trigger events

Create, pause, resume, and delete a sandbox in sequence:

```bash
# Create a sandbox
curl -X POST http://localhost:3000/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "<template ID>"}'
# Record the returned sandbox_id

# Pause the sandbox
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/pause

# Resume the sandbox
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/resume \
  -H "Content-Type: application/json" \
  -d '{}'

# Delete the sandbox
curl -X DELETE http://localhost:3000/sandboxes/<sandbox_id>
```

After each successful operation, the receiver terminal should print the corresponding event. For example:

```text
[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:00Z] sandbox.created
    sandbox_id : your-sandbox-id
    template_id: your-template-id

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:05Z] sandbox.timeout.updated
    sandbox_id : your-sandbox-id
    timeout: 300

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:10Z] sandbox.refreshed
    sandbox_id : your-sandbox-id
    duration: 600

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:15Z] sandbox.paused
    sandbox_id : your-sandbox-id

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:20Z] sandbox.resumed
    sandbox_id : your-sandbox-id

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:25Z] sandbox.deleted
    sandbox_id : your-sandbox-id
```

At this point, the webhook delivery path from CubeAPI to an external HTTP receiver is working. For more configuration options, see [Configure webhooks](#configure-webhooks) below.

## Supported events

| Event                      | Trigger                                     | Carried fields              |
| -------------------------- | ------------------------------------------- | --------------------------- |
| `sandbox.created`          | `POST /sandboxes` succeeds                  | `sandbox_id`, `template_id` |
| `sandbox.timeout.updated`  | `POST /sandboxes/:id/timeout` succeeds      | `sandbox_id`, `timeout`     |
| `sandbox.refreshed`        | `POST /sandboxes/:id/refreshes` succeeds    | `sandbox_id`, `duration`    |
| `sandbox.paused`           | `POST /sandboxes/:id/pause` succeeds        | `sandbox_id`                |
| `sandbox.resumed`          | `POST /sandboxes/:id/resume` succeeds       | `sandbox_id`                |
| `sandbox.deleted`          | `DELETE /sandboxes/:id` succeeds            | `sandbox_id`                |
| `sandbox.resumed` | `POST /sandboxes/:id/resume` succeeds | `sandbox_id`                |

### Event subscription

Each endpoint can configure the event types it wants to receive:

| Pattern           | Matches                                           |
| ----------------- | ------------------------------------------------- |
| `sandbox.created` | Exact match for a single event                    |
| `sandbox.*`       | All sandbox lifecycle events. This is the default |
| `*`               | All events, including debug-level events          |

If an endpoint does not specify `events`, it defaults to `sandbox.*`.

`api.request` is a debug-level event used internally by CubeAPI to record HTTP handler calls. It is not a sandbox lifecycle event and will not be delivered to webhook endpoints unless `*` is explicitly subscribed.

## Configure webhooks

When multiple configuration sources coexist, they are merged by the following priority:

```text
CLI flags > environment variables > config file > defaults
```

`CUBE_API_WEBHOOK_URL` and `--webhook-url` define a single-endpoint configuration. When either one is set, it replaces the `endpoints` list from the config file instead of appending to it.

### systemd / one-click (recommended)

When CubeAPI runs as a systemd service, it is recommended to inject webhook configuration through `.one-click.env`.

::: tip Location of `.one-click.env`
After a one-click installation, `.one-click.env` is located at:

```text
/usr/local/services/cubetoolbox/.one-click.env
```

:::

Append the following to the end of the file:

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://receiver-host:18080/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
CUBE_API_WEBHOOK_SECRET=<your-hmac-secret>     # optional; omitted means no signing
```

Restart CubeAPI to apply the change:

```bash
systemctl restart cube-sandbox-cube-api.service
```

This approach does not require changing the systemd unit file, but it can register only one endpoint at a time. To push events to multiple receivers simultaneously, see [Config file (multiple endpoints)](#config-file-multiple-endpoints) below.

### Config file (multiple endpoints)

Use a config file when you need to push events to multiple receivers, or when different endpoints need different `hmac_secret` values and event subscriptions:

```toml
enabled = true

# Delivery tuning. All values below are defaults and can be adjusted as needed.
batch_size = 100              # max events per batch
flush_interval_secs = 5       # max flush interval, in seconds
max_retries = 3               # max retry attempts after the initial failure
retry_backoff_millis = 200    # base backoff interval, in milliseconds
request_timeout_secs = 5      # per-request timeout, in seconds

# Endpoint A: receive only created / deleted
[[endpoints]]
url = "http://receiver-a:18080/webhook"
events = ["sandbox.created", "sandbox.deleted"]
hmac_secret = "secret-for-a"

# Endpoint B: receive all sandbox lifecycle events
[[endpoints]]
url = "http://receiver-b:18081/webhook"
events = ["sandbox.*"]
hmac_secret = "secret-for-b"
```

The recommended config file location is:

```text
/usr/local/services/cubetoolbox/CubeAPI/webhook.toml
```

Then reference the config file path from `.one-click.env`:

```ini
CUBE_API_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeAPI/webhook.toml
```

Restart CubeAPI to apply the change:

```bash
systemctl restart cube-sandbox-cube-api.service
```

### Environment variables

If you run the `./cube-api` binary directly instead of using systemd, export the environment variables before startup:

```bash
export CUBE_API_WEBHOOK_ENABLED=true
export CUBE_API_WEBHOOK_URL=http://receiver:18080/webhook
export CUBE_API_WEBHOOK_EVENTS=sandbox.*
export CUBE_API_WEBHOOK_SECRET=your-secret
./cube-api
```

Optional tuning variables are listed below. Defaults apply when a variable is not set:

| Variable                                | Default | Description                            |
| --------------------------------------- | ------- | -------------------------------------- |
| `CUBE_API_WEBHOOK_BATCH_SIZE`           | 100     | Max events per batch                   |
| `CUBE_API_WEBHOOK_FLUSH_INTERVAL_SECS`  | 5       | Max flush interval, in seconds         |
| `CUBE_API_WEBHOOK_MAX_RETRIES`          | 3       | Max retry attempts                     |
| `CUBE_API_WEBHOOK_RETRY_BACKOFF_MILLIS` | 200     | Base backoff interval, in milliseconds |
| `CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS` | 5       | Per-request timeout, in seconds        |

::: warning Note
Running `./cube-api` directly only starts the CubeAPI process itself. In production, CubeAPI depends on cubemaster, cubelet, and other backend services for full sandbox lifecycle support. For regular deployments, prefer the systemd approach.
:::

### CLI flags

During development, CLI flags can be used to quickly verify endpoint connectivity. CLI flags have the highest priority:

```bash
./cube-api \
  --webhook-url http://127.0.0.1:18080/webhook \
  --webhook-events sandbox.* \
  --webhook-secret your-secret
```

| CLI flag           | Environment variable      | Description                |
| ------------------ | ------------------------- | -------------------------- |
| `--webhook-config` | `CUBE_API_WEBHOOK_CONFIG` | Config file path           |
| `--webhook-url`    | `CUBE_API_WEBHOOK_URL`    | Single endpoint URL        |
| `--webhook-events` | `CUBE_API_WEBHOOK_EVENTS` | Comma-separated event list |
| `--webhook-secret` | `CUBE_API_WEBHOOK_SECRET` | HMAC secret                |

## Payload

CubeAPI sends a `POST` request to each endpoint with `Content-Type: application/json`.

The payload is sent in batch form. Even if the current batch contains only one event, it is still placed in the `events` array:

```json
{
  "events": [
    {
      "timestamp": "2026-01-01T12:00:00Z",
      "level": "info",
      "event": "sandbox.created",
      "sandbox_id": "sbx-abc123",
      "template_id": "tmpl-xyz789"
    }
  ]
}
```

Common fields in an event object are listed below:

| Field         | Description                                     |
| ------------- | ----------------------------------------------- |
| `timestamp`   | Time when the event was generated               |
| `level`       | Event level                                     |
| `event`       | Event name                                      |
| `sandbox_id`  | Sandbox ID                                      |
| `template_id` | Template ID. Only included in `sandbox.created`     |
| `timeout`     | New timeout value in seconds. Only included in `sandbox.timeout.updated` |
| `duration`    | Refresh duration in seconds. Only included in `sandbox.refreshed` |

Request headers:

| Header                     | Value                                                        |
| -------------------------- | ------------------------------------------------------------ |
| `Content-Type`             | `application/json`                                           |
| `X-Cube-Webhook-Event`     | `batch`                                                      |
| `X-Cube-Webhook-Signature` | `sha256=<hex>`, only included when `hmac_secret` is configured |

## HMAC-SHA256 signature verification

When `hmac_secret` is configured for an endpoint, CubeAPI computes an HMAC-SHA256 signature over the raw POST body and attaches it to the request header:

```text
X-Cube-Webhook-Signature: sha256=<hex>
```

Receiver-side verification example:

```python
import hmac, hashlib

def verify(raw_body: bytes, header_value: str, secret: str) -> bool:
    if not header_value or not header_value.startswith("sha256="):
        return False
    expected = hmac.new(
        secret.encode(), raw_body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(f"sha256={expected}", header_value)
```

::: tip Verification uses raw bytes
The signature is computed over the **raw HTTP request body bytes**. If the receiver does `json.loads()` followed by `json.dumps()`, field ordering or whitespace changes can cause verification to fail. Compute the signature directly over the raw bytes.
:::

See [examples/webhook-receiver/receiver.py](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/webhook-receiver/receiver.py) for the complete receiver implementation.

## Retry strategy

Webhook delivery runs in an independent background task and does not block API handlers for sandbox creation, pausing, resuming, or deletion.

When delivery fails, CubeAPI retries a limited number of times with exponential backoff:

```text
failure 1 → wait base
failure 2 → wait base × 2
failure 3 → wait base × 4
...
```

Where `base` is `retry_backoff_millis`, in milliseconds.

| Setting                | Default | Description                                       |
| ---------------------- | ------- | ------------------------------------------------- |
| `max_retries`          | 3       | Retry attempts after the initial delivery failure |
| `retry_backoff_millis` | 200     | Base backoff interval, in milliseconds            |
| `request_timeout_secs` | 5       | Per-request timeout, in seconds                   |
| `batch_size`           | 100     | Max events per batch                              |
| `flush_interval_secs`  | 5       | Max flush interval, in seconds                    |

After all retries are exhausted, CubeAPI records an error log and drops the batch. A delivery failure for one endpoint does not affect other endpoints and does not block CubeAPI API calls.

## WeCom integration

WeCom group bots only accept their own message format, such as `msgtype` + `text` / `markdown`. They do not accept arbitrary JSON payloads directly. Therefore, do not configure a WeCom group bot webhook URL directly as a CubeAPI webhook endpoint.

The recommended approach is to add an adapter service in the middle:

```text
CubeAPI -- JSON --> Adapter -- WeCom format --> Group bot
```

Steps:

1. Add a group bot in your WeCom group and obtain the bot webhook URL:

```text
https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY
```

2. Deploy an adapter service that receives CubeAPI webhook callbacks, converts events into WeCom markdown or text messages, and forwards them to the bot URL.

3. `examples/webhook-receiver/` can serve as a starting point for the adapter. Add WeCom message formatting and HTTP forwarding logic on top of it.

4. Configure CubeAPI to send events to the adapter:

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://<your-adapter>:<your-port>/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
```

## FAQ

**No `webhook_enabled=true` in the log after restart**

Check that the variable name in `.one-click.env` is correct. It should be `CUBE_API_WEBHOOK_ENABLED`, not `ENABLE`. After fixing it, restart CubeAPI and check the log again.

**The receiver does not receive events**

First confirm that the CubeAPI startup log contains `webhook_enabled=true` and that the endpoint URL is correct.

Then verify that the receiver is reachable from the CubeAPI machine:

```bash
curl http://<receiver>:18080/health
```

A healthy receiver should return:

```json
{"status":"ok"}
```

If events are still not delivered, check the delivery error logs:

```bash
journalctl -u cube-sandbox-cube-api.service --no-pager | grep HttpLogger
```

**HMAC signature mismatch**

Confirm that the `hmac_secret` configured on the CubeAPI side exactly matches `WEBHOOK_SECRET` on the receiver side.

The signature is computed over the raw POST body bytes. Do not re-serialize JSON on the receiver side before verifying the signature. To verify manually, save the body to a file and run:

```bash
openssl dgst -sha256 -hmac "your-secret" < body.bin
```

**Persistent delivery failures in logs**

You can temporarily increase `retry_backoff_millis` or decrease `max_retries` to reduce unnecessary retry frequency.

If an endpoint remains unreachable for an extended period, comment out the corresponding config entry and restart CubeAPI. Add it back once the receiver is restored.