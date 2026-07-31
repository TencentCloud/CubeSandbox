# Webhook receiver example

This dependency-free Python server prints CubeSandbox lifecycle events and,
when a secret is configured, verifies their HMAC-SHA256 signatures.

## Run

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=development-secret python3 receiver.py
```

Configure CubeAPI to call an address reachable from the CubeAPI process:

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[{"name":"local","url":"http://127.0.0.1:8080","events":["*"],"secret":"development-secret"}]'
```

Restart CubeAPI, then create, pause, resume, or delete a sandbox. The receiver
prints each event as formatted JSON. If CubeAPI runs in a container or VM,
replace `127.0.0.1` with the receiver host address that it can reach.

For production configuration and protocol details, see
[`docs/guide/webhooks.md`](../../docs/guide/webhooks.md).
