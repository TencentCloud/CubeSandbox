# Webhook Receiver — Cube Sandbox Event Callbacks

[中文文档](README_zh.md)

Receive and inspect [Cube Sandbox](https://github.com/tencent-cube/cube-sandbox) lifecycle events in real time. CubeAPI sends HTTP POST callbacks for sandbox lifecycle changes (create, delete, pause, resume, etc.) to any configured webhook URL.

---

## 1. Architecture

```
                          Server
  ┌─────────────────────────────────────────────────────┐
  │                                                     │
  │  python create.py  ──▶  CubeAPI (:3000)             │
  │  (SDK client)               │                       │
  │                             ▼                       │
  │                    python receiver.py (:8080)        │
  │                    (POST /webhook)                   │
  │                             │                       │
  │                        stdout: formatted events      │
  └─────────────────────────────────────────────────────┘
```

CubeAPI delivers events asynchronously via `tokio::spawn` (non-blocking), with exponential-backoff retry (3 attempts: 200 ms → 500 ms → 1 s).

---

## 2. Server Setup

### 2.1 Upload the Compiled CubeAPI Binary

Build CubeAPI (from the project root):

```bash
cd CubeAPI/
cargo build --release
```

The compiled binary is at `CubeAPI/target/release/cube-api`. Upload it along with the receiver script:

```bash
scp target/release/cube-api user@<server>:/opt/cube/
scp examples/webhook-receiver/receiver.py user@<server>:/opt/cube/
```

### 2.2 Start the Receiver

```bash
cd /opt/cube/

# Default: http://0.0.0.0:8080/webhook
python receiver.py

# With signature verification:
# WEBHOOK_SECRET=my-shared-secret python receiver.py
```

### 2.3 Configure & Start CubeAPI

Set these **before starting CubeAPI**:

```bash
export CUBE_API_WEBHOOK_URLS=http://localhost:8080/webhook   # receiver is on the same machine
export CUBE_API_WEBHOOK_EVENTS=*                              # all events
export CUBE_API_WEBHOOK_SECRET=my-shared-secret               # optional — match receiver's WEBHOOK_SECRET

# Start CubeAPI
./cube-api
```

### 2.4 Trigger Events

SDK client scripts are in [`code-sandbox-quickstart/`](../code-sandbox-quickstart/). From that directory:

```bash
cd ../code-sandbox-quickstart/

# Configure
cp .env.example .env
# Edit .env: E2B_API_URL=http://localhost:3000
#            CUBE_TEMPLATE_ID=<your-template-id>

pip install -r requirements.txt

# Trigger events — watch the receiver terminal
python create.py       # → sandbox.created, sandbox.deleted
python pause.py        # → sandbox.paused, sandbox.resumed
python exec_code.py
python cmd.py
```

---

## 3. Event Payload Format

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-24T12:00:00Z",
  "sandbox_id": "sb-abc123",
  "template_id": "tpl-python-3.12"
}
```

### Event Types

| Event                    | Description                         | Extra Fields           |
|--------------------------|-------------------------------------|------------------------|
| `sandbox.created`        | Sandbox created successfully        | —                      |
| `sandbox.deleted`        | Sandbox deleted                     | —                      |
| `sandbox.paused`         | Sandbox paused                      | —                      |
| `sandbox.resumed`        | Sandbox resumed                     | —                      |
| `sandbox.timeout.updated`| Sandbox timeout changed             | `timeout` (seconds)    |
| `sandbox.refreshed`      | Sandbox TTL refreshed               | `duration` (seconds)   |
| `api.response`           | API request completed               | —                      |
| `api.error`              | API handler error                   | `handler`, `error`     |

### Signature Verification

When `CUBE_API_WEBHOOK_SECRET` is set, CubeAPI sends:

```
X-Cube-Signature-256: sha256=<hmac-hex>
```

The HMAC is **HMAC-SHA256** of the raw JSON body. The receiver verifies this when `WEBHOOK_SECRET` is set. Invalid/missing signature → HTTP 401.

---

## 4. End-to-End Test

```bash
# Terminal 1 — start the receiver
cd examples/webhook-receiver/
WEBHOOK_SECRET=test-secret python receiver.py

# Terminal 2 — start CubeAPI with webhooks
cd ../..
export CUBE_API_WEBHOOK_URLS=http://localhost:8080/webhook
export CUBE_API_WEBHOOK_SECRET=test-secret
export CUBE_API_WEBHOOK_EVENTS=*
cargo run --release

# Terminal 3 — trigger events
cd examples/code-sandbox-quickstart/
python create.py

# → Terminal 1 shows:
#   sandbox.created  2026-07-24 20:03:15.123  sandbox=sb-abc123
```

---

## 5. Event Filtering

```bash
export CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
```

Filtering is on CubeAPI side — the receiver prints everything it receives.

---

## 6. Production Notes

This receiver is designed for **development and testing**. For production:

- Use a dedicated webhook framework (Flask, FastAPI, or message queue consumer)
- Add persistent storage (file log, database, or message queue)
- Implement idempotency (retries may cause duplicate deliveries)
- Consider running as a systemd service or Docker container

---

## 7. Reference

- [CubeAPI webhook source](../../CubeAPI/src/logging/http.rs) — Rust delivery backend
- [Event log types](../../CubeAPI/src/logging/mod.rs) — `LogEvent` struct
- [Configuration fields](../../CubeAPI/src/config/mod.rs) — `ServerConfig.webhook_*`
- [SDK Client Scripts](../code-sandbox-quickstart/) — scripts to trigger sandbox events
