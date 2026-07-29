# Cube E2B API

A Rust-based **E2B-compatible API Server** built on the [Axum](https://github.com/tokio-rs/axum) framework, running on top of Cube's proprietary sandbox infrastructure.

No client code changes are needed — simply point `E2B_API_URL` and `E2B_API_KEY` to this service to seamlessly migrate from the E2B cloud to the Cube platform. For HTTPS access to sandboxes, configure `SSL_CERT_FILE` as needed (see details below).

---

## Table of Contents

- [Supported Features](#supported-features)
- [Quick Start](#quick-start)
- [Examples](#examples)

---

## Supported Features

The following Sandbox core APIs are **fully E2B-compatible** and can be used directly with the official `e2b` / `e2b-code-interpreter` Python SDK:

| Method | Path | Description | Implemented |
|--------|------|-------------|:-----------:|
| GET | `/health` | Health check (no authentication required) | ✅ |
| GET | `/sandboxes` | List all sandboxes (v1) | ✅ |
| GET | `/v2/sandboxes` | List sandboxes (v2, supports state/metadata filtering, limit) | ✅ |
| POST | `/sandboxes` | Create a sandbox | ✅ |
| GET | `/sandboxes/:sandboxID` | Get single sandbox details | ✅ |
| DELETE | `/sandboxes/:sandboxID` | Destroy a sandbox | ✅ |
| POST | `/sandboxes/:sandboxID/pause` | Pause a sandbox (preserves memory snapshot) | ✅ |
| POST | `/sandboxes/:sandboxID/resume` | Resume a sandbox (deprecated, replaced by connect) | ✅ |
| POST | `/sandboxes/:sandboxID/connect` | Connect to a sandbox (auto-resumes, replaces resume) | ✅ |
| GET | `/sandboxes/:sandboxID/logs` | Get sandbox logs (v1, deprecated) | ❌ |
| GET | `/v2/sandboxes/:sandboxID/logs` | Get sandbox logs (v2, cursor-based pagination) | ❌ |
| POST | `/sandboxes/:sandboxID/timeout` | Set sandbox timeout (absolute TTL) | ❌ |
| POST | `/sandboxes/:sandboxID/refreshes` | Extend sandbox lifetime (relative TTL) | ❌ |
| POST | `/sandboxes/:sandboxID/snapshots` | Create a sandbox snapshot | ❌ |
| GET | `/sandboxes/:sandboxID/metrics` | Get sandbox metrics | ❌ |
| GET | `/sandboxes/snapshots` | List all snapshots for the team | ❌ |
| PUT | `/sandboxes/:sandboxID/network` | Update sandbox network config (egress rules) | ❌ |

**Legend:** ✅ Fully implemented | ❌ Route not registered or depends on pending CubeMaster APIs

### Cube Extensions

| Feature | Description |
|---------|-------------|
| **Host Directory Mount** | Mount a host directory into the sandbox via `metadata.host-mount` at creation time |
| **Browser Sandbox** | Built-in Chromium inside the sandbox, exposed via CDP, allowing direct Playwright control |

---

## Quick Start

### Running the Service

```bash
# Development mode
RUST_LOG=debug cargo run

# Production build
cargo build --release
./target/release/cube-api
```

### Server Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBE_API_BIND` | `0.0.0.0:3000` | Listen address |
| `LOG_LEVEL` | `info` | Log level |
| `CUBE_MASTER_ADDR` | `http://127.0.0.1:8089` | CubeMaster base URL |
| `CUBE_API_SANDBOX_DOMAIN` | `cube.app` | Domain returned in sandbox API responses |
| `AUTH_CALLBACK_URL` | *(unset)* | External auth callback URL (callback mode) |
| `CUBE_API_KEY` | *(unset)* | Built-in API key for simple auth (simple-key mode) |

### Authentication

CubeAPI provides two mutually exclusive authentication modes (callback mode
takes priority when both are set). When neither is set, all requests pass
through without authentication.

**Mode 1 — Callback auth (`AUTH_CALLBACK_URL`)**

For deployments that already have an authentication system. Every request
(except `/health`) must carry either `Authorization: Bearer <token>` or
`X-API-Key: <key>`. CubeAPI forwards the credential plus `X-Request-Path`
and `X-Request-Method` to the callback URL; HTTP 200 grants access, any
other status returns 401.

**Mode 2 — Simple key auth (`CUBE_API_KEY`)**

For deployments without a separate auth system. Set one environment variable:

```bash
CUBE_API_KEY=your-secret-key
```

Every request (except `/health`) must carry either `Authorization: Bearer <your-secret-key>`
or `X-API-Key: <your-secret-key>`. The credential is compared as a string
against `CUBE_API_KEY`; a match grants access, a mismatch or missing
credential returns 401.

---

## Examples

The [`examples/`](examples/) directory provides complete examples based on the official `e2b` / `e2b-code-interpreter` Python SDK.

### Example Overview

| File | Description |
|------|-------------|
| `create.py` | Create a sandbox and print basic info |
| `cmd.py` | Execute shell commands inside a sandbox |
| `exec_code.py` | Execute Python code inside a sandbox |
| `read.py` | Read files from the sandbox filesystem |
| `pause.py` | Pause a sandbox, wait, then resume and verify state |
| `create_with_mount.py` | Create a sandbox with a host directory mount (Cube extension) |
| `browser.py` | Launch a sandbox with Chromium and control the browser via Playwright |
| `test.py` | Multi-threaded stress test: create sandboxes, execute code and commands in a loop |


### Running the Examples

**1. Install Python dependencies**

```bash
cd examples
pip install -r requirements.txt

# If running browser.py, also install the Playwright browser driver
playwright install chromium
```

**2. Set environment variables**

The following four environment variables must be exported before running:

| Variable | Description |
|----------|-------------|
| `CUBE_TEMPLATE_ID` | Cube sandbox template ID. All examples use this to determine which template to create sandboxes from; must be explicitly set. |
| `E2B_API_URL` | Address of the Cube E2B API service. The SDK defaults to the official E2B cloud service, so this must be overridden with the local or deployed address — otherwise requests will go to the official service instead of Cube. |
| `E2B_API_KEY` | The E2B SDK requires this field to be present (it performs a non-empty check). For local deployments, any non-empty string works, e.g. `e2b_000000`. |
| `SSL_CERT_FILE` | When accessing sandboxes using Cube's built-in test certificate (`cube.app`), set this variable to the corresponding CA root certificate path so that the E2B SDK's httpx/requests can complete TLS verification. We recommend using a locally signed certificate from mkcert: `/root/.local/share/mkcert/rootCA.pem`.<br>If you use a custom domain with a trusted certificate, or access sandboxes over HTTP, this variable is not needed. See [CubeProxy TLS Configuration](../docs/guide/cubeproxy-tls.md). |

```bash
export CUBE_TEMPLATE_ID=<your-template-id>
export E2B_API_URL=http://localhost:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

**3. Run**

```bash
python create.py
python cmd.py
python exec_code.py
python read.py
python pause.py
python create_with_mount.py
python browser.py
python test.py

