# gRPC Ingress Quickstart

[中文文档](README_zh.md)

Minimal example for CubeProxy **plaintext gRPC ingress** on port `9091`.

The CubeSandbox Python SDK (`commands`, `files`, etc.) talks to envd over
**HTTP/Connect** on CubeProxy HTTP/HTTPS (`80`/`443`, or `CUBE_PROXY_NODE_IP` +
`CUBE_PROXY_PORT_HTTP` with a `Host` header). **No SDK changes are required** for
those APIs.

Use port `9091` when you have a **native gRPC client** (`grpcio`, `grpc-go`, …)
that cannot rely on wildcard DNS (`*.cube.app`) or TLS on CubeProxy.

> **Security:** Port `9091` speaks **plaintext** gRPC (`grpc.insecure_channel`).
> Do not expose it to untrusted networks unless you terminate TLS elsewhere
> (for example a load balancer). Prefer CubeProxy HTTP/HTTPS for Connect-style
> traffic when TLS is required.

## Flow

```text
  grpc_plaintext.py
        │
        ├─ Sandbox.create()  ──► CubeAPI (control plane)
        │
        └─ grpc.insecure_channel(proxy:9091, authority=<port>-<sandbox_id>)
                                    │
                                    ▼
                               CubeProxy :9091
                                    │
                                    ▼
                               envd in sandbox (:49983)
```

## Prerequisites

- A running Cube Sandbox deployment with CubeProxy gRPC ingress enabled
- Python 3.8+
- A template that exposes envd on port `49983` (standard Cube templates)

```bash
pip install -r requirements.txt
cp .env.example .env
# edit .env
python grpc_plaintext.py
```

## Environment variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `CUBE_API_URL` | yes | — | Cube API base URL |
| `CUBE_TEMPLATE_ID` | yes | — | Sandbox template ID |
| `CUBE_PROXY_NODE_IP` | yes | — | CubeProxy IP (no DNS needed) |
| `CUBE_PROXY_GRPC_PORT` | no | `9091` | Plaintext gRPC listen port |
| `ENVD_PORT` | no | `49983` | Sandbox service port in `:authority` |

## Restricted sandboxes

If the sandbox is created with `network={"allow_public_traffic": False}`,
attach the traffic token as gRPC metadata on every RPC:

```python
metadata = (("cube-traffic-access-token", sandbox.traffic_access_token),)
# stub.SomeMethod(request, metadata=metadata)
```

See [Restrict Public Access](../../docs/guide/restrict-public-access.md).

## Template

```bash
cubemastercli tpl create-from-image \
  --image cubesandbox-base:latest \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```
