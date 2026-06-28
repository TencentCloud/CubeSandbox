# Network Hardening

Cube Sandbox's control-plane and management services are optimized for fast
local evaluation, and several of them bind to `0.0.0.0` by default. On a machine
reachable from untrusted networks this exposes an attack surface: most
management endpoints have no authentication or TLS. This guide explains the
default listening surface and how to lock it down with **bind-address
configuration** and **firewall rules**.

::: warning
The one-click / self-build deployments are designed for development and
evaluation. Before placing a deployment on a public-facing machine, review this
entire page and apply at least one of the hardening strategies below.
:::

## Default listening surface

| Process | Default Bind | Port | Config Method | Notes |
|---------|-------------|------|---------------|-------|
| CubeMaster | `0.0.0.0` | 8089 | `CUBEMASTER_HTTP_BIND` in `.env` | Cluster management HTTP API, **no auth** |
| CubeAPI | `0.0.0.0` | 3000 | `CUBE_API_BIND` in `.env` | Sandbox lifecycle API |
| Cubelet gRPC | `0.0.0.0` | 9999 | `tcp_address` in `Cubelet/config/config.toml` | Node management RPC, **no TLS** |
| Cubelet HTTP | `0.0.0.0` | 9998 | `[http] address` in `Cubelet/config/config.toml` | Debug / metrics |
| cube-proxy | `0.0.0.0` | 80 / 443 | `CUBE_PROXY_HTTP_PORT` / `CUBE_PROXY_HTTPS_PORT` | Intentionally public-facing |
| WebUI | `0.0.0.0` | 12088 | `WEB_UI_HOST_PORT` in `.env` (port only) | Dashboard |
| MySQL | `127.0.0.1` | 3306 | Hardcoded in compose template | Already loopback-only |
| Redis | `127.0.0.1` | 6379 | Hardcoded in compose template | Already loopback-only |

MySQL and Redis are already bound to loopback by the bundled compose template
and are not reachable from the network. The remaining services listed with a
`0.0.0.0` default are the ones you need to consider.

## Per-service bind address configuration

### CubeMaster

Set in `.env` before running `install.sh`:

```bash
# Bind to the private NIC (multi-node safe):
CUBEMASTER_HTTP_BIND=10.0.0.11

# Or loopback-only (single all-in-one node, no compute nodes):
CUBEMASTER_HTTP_BIND=127.0.0.1
```

::: warning
Do **not** set `CUBEMASTER_HTTP_BIND=127.0.0.1` if you have compute nodes or a
host-network cube-proxy — they reach CubeMaster via the node's external/private
IP, and loopback binding will break them.
:::

### CubeAPI

Set in `.env`. The value is `<address>:<port>`:

```bash
# Bind to the private NIC:
CUBE_API_BIND=10.0.0.11:3000
```

When changing `CUBE_API_BIND`, also update `CUBE_API_HEALTH_ADDR` to match:

```bash
CUBE_API_HEALTH_ADDR=10.0.0.11:3000
```

::: warning
The WebUI container reaches CubeAPI via `host.docker.internal`. Binding CubeAPI
to `127.0.0.1` will break the WebUI in bridge-mode Docker. Use a private-IP bind
or a firewall rule instead.
:::

### Cubelet (gRPC + HTTP)

Cubelet reads its listen addresses from `config/config.toml`. After
installation the file lives at `<PKG_ROOT>/Cubelet/config/config.toml`. Edit:

```toml
[http]
  # Default ":9998" means 0.0.0.0:9998
  address = "10.0.0.11:9998"

[grpc]
  # Default ":9999" means 0.0.0.0:9999
  tcp_address = "10.0.0.11:9999"
```

::: warning
In multi-node deployments the control node connects to each compute node's
Cubelet on `tcp_address`. If you bind to a private IP, ensure the control node
can route to it. Binding to `127.0.0.1` **will break** multi-node communication.
:::

### WebUI

The WebUI docker-compose template publishes the port as `<WEB_UI_HOST_PORT>:80`
and the variable currently accepts a port number only. To restrict access, use a
firewall rule:

```bash
sudo iptables -A INPUT -p tcp --dport 12088 -s 10.0.0.0/24 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 12088 -j DROP
```

### cube-proxy (HTTP/HTTPS)

cube-proxy uses host networking and is typically the public entry point for
sandbox traffic. Keep it on `0.0.0.0` unless you front it with a dedicated
reverse proxy:

```bash
CUBE_PROXY_HTTP_PORT=80
CUBE_PROXY_HTTPS_PORT=443
```

If you need to restrict source IPs, use firewall rules.

### MySQL / Redis

The bundled containers already bind to `127.0.0.1` via the compose template — no
extra configuration needed. If you use external MySQL/Redis
(`CUBE_EXTERNAL_MYSQL_HOST`), enforce access control at the network level.

## Hardening strategies

Choose one or both of the following, depending on your environment.

### Option A: Bind to the private network interface

If the machine has both a public and a private NIC, bind management services to
the **private (internal) IP** so only the internal network can reach them while
the public interface stays unexposed:

```bash
# .env
CUBEMASTER_HTTP_BIND=10.0.0.11
CUBE_API_BIND=10.0.0.11:3000
CUBE_API_HEALTH_ADDR=10.0.0.11:3000
```

```toml
# Cubelet/config/config.toml
[http]
  address = "10.0.0.11:9998"
[grpc]
  tcp_address = "10.0.0.11:9999"
```

This keeps multi-node communication intact (compute nodes connect via the
private network) while preventing public internet access.

For a **single all-in-one node with no compute nodes**, you can bind to
`127.0.0.1` for maximum restriction (subject to the caveats noted per service
above).

### Option B: Firewall source-IP whitelisting

Keep the default `0.0.0.0` binding but allow connections **only from trusted
source IPs** (your compute nodes and admin machine).

Using `iptables`:

```bash
# Allow CubeMaster only from compute nodes + admin
sudo iptables -A INPUT -p tcp --dport 8089 -s 10.0.0.12 -j ACCEPT       # compute-1
sudo iptables -A INPUT -p tcp --dport 8089 -s 10.0.0.13 -j ACCEPT       # compute-2
sudo iptables -A INPUT -p tcp --dport 8089 -s 192.168.1.100 -j ACCEPT   # admin
sudo iptables -A INPUT -p tcp --dport 8089 -j DROP                      # deny others

# Repeat the same pattern for 9999, 3000, 12088 as needed
```

Using `ufw`:

```bash
sudo ufw default deny incoming
sudo ufw allow from 10.0.0.0/24 to any port 8089 proto tcp   # CubeMaster
sudo ufw allow from 10.0.0.0/24 to any port 9999 proto tcp   # Cubelet
sudo ufw allow from 10.0.0.0/24 to any port 3000 proto tcp   # CubeAPI
sudo ufw allow from 10.0.0.0/24 to any port 12088 proto tcp  # WebUI
sudo ufw allow 22/tcp     # SSH
sudo ufw allow 80/tcp     # cube-proxy (public if needed)
sudo ufw allow 443/tcp    # cube-proxy TLS
sudo ufw enable
```

### Combining both approaches (recommended)

For defense in depth, **bind to a private IP and set firewall rules**. Even if
one layer is misconfigured, the other still protects you.

## Multi-node port reachability requirements

When deploying compute nodes, the following connectivity must remain open.
Ensure your bind addresses and firewall rules do **not** block these paths:

| Direction | Port | Service | Required for |
|-----------|------|---------|-------------|
| Compute → Control | 8089/tcp | CubeMaster | Sandbox metadata, node registration |
| Control → Compute | 9999/tcp | Cubelet gRPC | Sandbox lifecycle operations |

## Default credentials

The following values in `env.example` are **examples only** — change them for
any deployment beyond local evaluation:

| Variable | Risk |
|----------|------|
| `CUBE_SANDBOX_MYSQL_ROOT_PASSWORD` | Full database access |
| `CUBE_SANDBOX_MYSQL_PASSWORD` | Application database access |
| `CUBE_SANDBOX_REDIS_PASSWORD` | Cache / session access |
| `E2B_API_KEY` (defaults to `e2b_000000`) | API authentication |
| `DATABASE_URL` | Embeds the MySQL user / password |

## Custom Authentication (Auth Callback)

By default CubeAPI accepts **every request without any credential check**. For
any deployment beyond local evaluation you should delegate authentication to an
external service via the **auth callback** mechanism. When configured, every API
request (except `GET /health`) must carry a credential, and CubeAPI forwards it
to your callback for a permit/deny decision.

### Enabling

Set the callback URL in `.env`, or pass it as a CLI flag (the flag overrides the
environment variable):

```bash
# .env
AUTH_CALLBACK_URL=https://your-auth-service/verify

# Or CLI flag
./cube-api --auth-callback-url https://your-auth-service/verify
```

When `AUTH_CALLBACK_URL` is **not set** (the default), all requests are allowed
without any credential check.

### How it works

```
Client ──→ CubeAPI
               │
               ├─ Extract credential (Authorization: Bearer / X-API-Key)
               ├─ Capture path + HTTP method
               │
               └─ POST → AUTH_CALLBACK_URL
                              │
                     200 ─────┤──→ allow request
                  non-200 ────┘──→ 401 Unauthorized
```

CubeAPI forwards these headers to your callback:

| Header | Description |
|--------|-------------|
| `Authorization` | `Bearer <token>` (when client used Bearer auth) |
| `X-API-Key` | `<key>` (when client used API Key auth) |
| `X-Request-Path` | Original request path, e.g. `/templates/my-tmpl` |
| `X-Request-Method` | HTTP method, e.g. `GET`, `DELETE`, `PATCH` |

`Authorization` and `X-API-Key` are mutually exclusive — the callback receives
whichever one the client sent (Bearer takes priority). If the client sends
neither, CubeAPI returns `401` without calling your service.

::: warning Always validate path **and** method
The same path (e.g. `/templates/:id`) serves GET (read), POST (rebuild),
DELETE, and PATCH (update). A callback that only checks the path cannot prevent
a read-only key from escalating to a delete operation. Always check **both**
`X-Request-Path` and `X-Request-Method`.
:::

::: warning Callback availability
If the callback URL is unreachable, CubeAPI fails closed (the request is
rejected). Host your auth service with sufficient availability and keep its
latency low, since every API request waits on it.
:::

### SDK integration

The E2B SDK sends `E2B_API_KEY` as `Authorization: Bearer <key>` automatically:

```bash
export E2B_API_KEY=your-actual-api-key
```

For non-SDK clients, send `X-API-Key: <key>` directly.

::: tip Full guide
For a complete callback implementation example (Python/FastAPI) and error
response reference, see [Authentication](/guide/authentication).
:::

## TLS

- CubeMaster and CubeAPI do not terminate TLS. Place them behind cube-proxy or a
  separate reverse proxy if external TLS is required.
- Cubelet gRPC is plaintext; rely on network-level isolation (bind address or
  firewall) to protect it.
- cube-proxy handles TLS termination via `mkcert`-generated certificates by
  default; replace these with production certificates for public-facing
  deployments.

## Related

- [Network Policy](/guide/network-policy) — sandbox outbound CIDR allow/deny.
- [Security Proxy](/guide/security-proxy) — L7 rules on sandbox outbound HTTP/HTTPS.
- [Restrict Public Access](/guide/restrict-public-access) — per-sandbox inbound token.
- [Multi-Node Cluster Deployment](/guide/multi-node-deploy) — adding compute nodes.
