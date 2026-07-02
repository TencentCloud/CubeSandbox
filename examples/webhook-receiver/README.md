# CubeSandbox Webhook Receiver

This is a runnable stdlib-only receiver for CubeAPI webhook events. It verifies
optional HMAC signatures, prints each payload, and can forward alerts to a WeCom
group robot.

## Run

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=change-me python3 receiver.py
```

Expose `http://<receiver-host>:9000/webhook` to CubeAPI, then configure CubeAPI:

```bash
export CUBE_API_WEBHOOKS='[
  {
    "url": "http://<receiver-host>:9000/webhook",
    "events": ["sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"],
    "secret": "change-me"
  }
]'
```

Restart CubeAPI and create, pause, resume, or delete a sandbox. The receiver
prints JSON payloads such as:

```json
{
  "event": "sandbox.created",
  "level": "info",
  "sandbox_id": "sb-123",
  "template_id": "tpl-123",
  "timestamp": "2026-07-02T10:00:00Z"
}
```

## WeCom Forwarding

Create a WeCom group robot, copy its webhook URL, and set:

```bash
export WECOM_BOT_WEBHOOK='https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...'
WEBHOOK_SECRET=change-me python3 receiver.py
```

The receiver still responds quickly to CubeAPI and logs forwarding failures to
stderr.
