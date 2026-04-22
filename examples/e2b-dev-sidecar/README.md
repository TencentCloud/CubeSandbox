[English](./README.md) | [中文](./README_zh.md)

# E2B Dev Sidecar

This demo shows one thing: reuse the local `e2b_code_interpreter` SDK to access CubeSandbox directly, while forwarding sandbox data-plane traffic to CubeProxy through a local sidecar.

## Why we need `dev-sidecar`


E2B expects sandbox URLs to resolve to the target cluster's public IP through wildcard DNS. In a production deployment, that usually means adding a private DNS A record like:

```text
*.cube.app => <your cube master node ip>
```

That is inconvenient during local development. Setting up wildcard DNS on a developer machine is usually the annoying part, so `dev-sidecar` exists to let you connect your local machine to a Cube cluster and create sandboxes without changing the E2B SDK itself.

Applicable scenarios:

- The control plane is already reachable through `E2B_API_URL` to access CubeAPI.
- The data plane must go through a local sidecar that rewrites the `Host` header before forwarding to CubeProxy.

## Files

- `demo.py`: minimal runnable example
- `dev_sidecar.py`: starts an embedded sidecar and rewrites the SDK's data-plane access to the sidecar
- `env.example`: example environment variables

## Quick Start

```bash
cd examples/e2b-dev-sidecar
pip install -r requirements.txt
cp env.example .env
```

Edit `.env` and fill in at least these three values:

```bash
# **If you are running Cube on remote machine,** replace this with: http://<node-ip>:3000
E2B_API_URL="http://127.0.0.1:13000"
# **If you are running Cube on remote machine,** replace this with: https://<node-ip>:443
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
CUBE_TEMPLATE_ID="<your-template-id>"
```

If your cluster has auth enabled, replace `E2B_API_KEY="dummy"` in `.env` with a real key before running the demo.

Then run:

```bash
python demo.py
```

On success, you should see output similar to:

```text
Hello world Cube!
```

## What This Demo Does

When `demo.py` starts, it first calls `setup_dev_sidecar()`. That function does two things:

1. Starts a local sidecar. By default it listens on `127.0.0.1:12580`. If the port is already in use, it automatically picks the next available port.
2. Monkey patches the SDK helpers that need data-plane routing. In sidecar mode, envd, file URLs, MCP URLs, and sandbox port access are routed to:

```text
http://127.0.0.1:<local-port>/sandboxes/router/<sandbox_id>/<port>
```

The sidecar then forwards those requests to `CUBE_REMOTE_PROXY_BASE` and rewrites the `Host` header to:

```text
<port>-<sandbox_id>.<sandbox-domain>
```

This applies to:

- envd API traffic
- file upload/download URLs
- MCP URLs
- regular sandbox HTTP traffic
- WebSocket traffic proxied through the same router path

## Configuration

- `E2B_API_URL`
  Control-plane endpoint. The SDK sends requests to CubeAPI directly and does not go through the sidecar.
- `CUBE_REMOTE_PROXY_BASE`
  CubeProxy endpoint used by the sidecar when forwarding data-plane requests.
- `CUBE_TEMPLATE_ID`
  Template ID used when creating the sandbox.
- `CUBE_REMOTE_SANDBOX_DOMAIN`
  Optional. Defaults to `cube.app`.
- `CUBE_REMOTE_PROXY_VERIFY_SSL`
  Optional. Defaults to `false`, which is convenient for self-signed certificates or local development environments.
- `CUBE_DEV_PROXY_HOST`
  Optional. Listen address of the embedded sidecar. Defaults to `127.0.0.1`.
- `CUBE_DEV_PROXY_PORT`
  Optional. Preferred port of the embedded sidecar. Defaults to `12580`.
- `CUBE_DEV_PROXY_URL`
  Optional. If you already have an external sidecar, point to it directly. In that case, the embedded sidecar will not be started.

## Sidecar URL Semantics

- `sandbox.get_host(port)` returns a host-plus-router-path fragment in sidecar mode, not a plain DNS hostname.
- `download_url()`, `upload_url()`, and `get_mcp_url()` return full routed URLs and preserve the sidecar scheme.
- The embedded sidecar always listens on `http://...`.
- If you use `CUBE_DEV_PROXY_URL`, the generated file and MCP URLs follow that URL's scheme.

## Development Boundary

- This demo proxies only the data plane, not the control plane.
- This sidecar is a minimal dev-only implementation, not a production gateway.
