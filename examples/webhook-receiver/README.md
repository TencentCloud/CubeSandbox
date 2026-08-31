# Webhook Receiver

A minimal, zero-dependency HTTP receiver for Cube Sandbox sandbox lifecycle
webhooks. It listens for the `POST` events that **cube-lifecycle-manager (CLM)**
(or CubeAPI) pushes and prints them in a human-readable form, with optional
HMAC-SHA256 signature verification.

> The wire protocol is shared with CubeAPI's webhook emitter, so this receiver
> works for either emitter unchanged. See
> [`docs/guide/integrations/webhook.md`](../../docs/guide/integrations/webhook.md)
> for the full integration guide.

## Requirements

- Python 3.10+ (stdlib only — no `pip install` needed)

## Quick start

**1. Start the receiver**

```sh
cd examples/webhook-receiver

# No signature verification (dev only)
python receiver.py

# With signature verification (recommended)
WEBHOOK_SECRET=my-shared-secret python receiver.py

# Custom host/port/path
python receiver.py --host 0.0.0.0 --port 9999 --path /events
```

**2. Point cube-lifecycle-manager at it**

Set these in the CLM process environment:

```bash
export CUBE_LCM_WEBHOOK_URLS="http://127.0.0.1:8081/webhook"
export CUBE_LCM_WEBHOOK_EVENTS="*"              # or a comma-separated filter
export CUBE_LCM_WEBHOOK_SECRET="my-shared-secret"  # must match WEBHOOK_SECRET above
```

Restart CLM. It now consumes the lifecycle Redis stream through its own
consumer group (`cube-webhook-delivery`) and delivers every mapped event to the
receiver. See the receiver's `README` in `.env.example` for the full variable
list.

**3. See events arrive**

Every sandbox `create / pause / resume / delete / timeout-update` now prints a
colour-coded block, for example:

```
sandbox.created  2026-07-24 12:00:00.000  sandbox=sb-abc123
  template_id: tpl-python-3.12
  instance_type: cubebox
  timeout_seconds: 300
  auto_pause: true
  auto_resume: true
  created_at: 1784966400000
  end_at: 1784966700000
```

## Signature verification

When `WEBHOOK_SECRET` is set, the receiver rejects any request whose
`X-Cube-Signature-256` header does not match `sha256=<hex>` where the hex is
the **HMAC-SHA256 of the raw request body** keyed by the secret. Use the same
secret on the CLM side (`CUBE_LCM_WEBHOOK_SECRET`). A bad signature returns
`401`.

## Delivery semantics (what to expect)

- **Async + non-blocking**: delivery happens in the background; it never blocks
  sandbox create/destroy.
- **Retry**: CLM retries each delivery up to `CUBE_LCM_WEBHOOK_MAX_RETRIES + 1`
  times (default 3 total) with exponential backoff, then acknowledges and drops
  the event. Your receiver should answer `2xx` to stop the retry.
- **At-least-once**: events are acknowledged only after delivery, so a CLM crash
  can redeliver. Deduplicate on the `event_id` field (a stable stream ID) in
  your receiver.
- **Payload**: JSON with `event`, `timestamp`, `event_id`, `sandbox_id`, plus
  best-effort context (`template_id`, `host_id`, …) flattened into the root
  object.

## Forwarding to WeCom or a generic alert endpoint

This receiver is a *display* sink. To turn events into alerts:

- **Generic HTTP alerting** (PagerDuty, Slack, your own API): point
  `CUBE_LCM_WEBHOOK_URLS` at a small relay that verifies the signature and
  reformats the payload for the downstream API.
- **WeCom (企业微信) bot**: the bot webhook expects a different body shape
  (`{"msgtype":"markdown",...}`), so route CLM → a tiny relay → WeCom. A
  ready-to-use relay snippet and both recipes are in the
  [integration guide](../../docs/guide/integrations/webhook.md#alerting-wecom-bot-and-generic-http).

## License

Apache-2.0
