# CubeSandbox Webhook receiver

This dependency-free Python receiver prints every verified CubeSandbox lifecycle event as JSON.

```bash
export CUBE_WEBHOOK_SECRET='replace-with-a-random-shared-secret'
# Optional replay window; defaults to 300 seconds.
export CUBE_WEBHOOK_MAX_AGE_SECONDS=300
# Optional listen port; defaults to 9010.
export PORT=9010
python3 receiver.py
```

Configure cube-lifecycle-manager with the same value:

```bash
export CUBE_LCM_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:9010/webhooks/cube","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"replace-with-a-random-shared-secret"}]'
```

Restart cube-lifecycle-manager, then create, pause, resume, or delete a sandbox. The receiver prints a payload similar to:

```json
{
  "version": "1",
  "event": "sandbox.created",
  "timestamp": "2026-07-11T08:15:30Z",
  "sandbox_id": "sbx-123",
  "template_id": "tpl-456"
}
```

`X-Cube-Signature-256` is `sha256=` followed by the lowercase hex HMAC-SHA256 of the raw request body. Receivers that do not support this verification header can omit `secret`.

After verifying the signature, receivers that trigger operational actions should reject replayed events. The signed body is authoritative: this example requires `X-Cube-Event` and `X-Cube-Timestamp` to match the body fields, and accepts events for five minutes, with 30 seconds of future clock skew. Keep receiver clocks synchronized and adjust `CUBE_WEBHOOK_MAX_AGE_SECONDS` only when delivery retries require a larger window.

The example limits request bodies to 1 MiB and aborts socket reads after 30 seconds.

Generic alerting products may require a product-specific payload rather than the CubeSandbox event schema. For example, a WeCom bot expects a `msgtype` message and cannot be used as an endpoint directly. Put an adapter such as this receiver in front of those products and translate the verified event into their required payload.
