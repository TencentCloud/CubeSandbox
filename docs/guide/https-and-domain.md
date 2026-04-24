# HTTPS & Domain Resolution

This guide explains how sandbox access domains work in Cube Sandbox and how to resolve common **HTTPS certificate** and **DNS resolution** issues.

> **Note:** TLS configuration only affects how clients reach CubeProxy. `E2B_API_URL` always points to the **Cube API Server** (default port `3000`) — a separate component from CubeProxy.

---

## How Sandbox Domains Work

When an E2B client accesses a sandbox, the domain name is **dynamically generated** using the following format:

```
<sandbox-service-port>-<sandboxId>-<domain>
```

For example:

```
49999-abc123def456-cube.app
```

Where:
- `sandbox-service-port`: the port your sandbox application listens on (e.g. `49999`)
- `sandboxId`: the unique sandbox ID
- `domain`: the base domain, defaults to `cube.app`, customizable via `--sandbox-domain`

Because the `sandboxId` part changes dynamically for every sandbox, **the network environment where the E2B client runs must support wildcard DNS resolution** — i.e., `*.<domain>` must resolve to the IP of the machine running CubeProxy.

### Built-in CoreDNS in One-Click Install (for Quick Start)

The Cube one-click install script starts a **CoreDNS** service on the installation node that automatically handles wildcard resolution for `*.cube.app`. This lets you experience the full workflow on that machine without any extra DNS setup.

> **Note:** The built-in CoreDNS is intended for local/quick-start use only — it is not suitable for production or multi-machine shared deployments.

### Production: Custom Domain + Your Own DNS

For production deployments, use your own domain and configure a wildcard DNS record:

```
*.your.domain.com  →  <IP of the node running CubeProxy>
```

Then tell the Cube API Server to use that domain at startup:

```bash
# CLI flag
./cube-api --sandbox-domain your.domain.com

# Or via environment variable
export CUBE_API_SANDBOX_DOMAIN=your.domain.com
./cube-api
```

With this in place, the `domain` field in API responses will return `your.domain.com`, allowing the E2B SDK to correctly construct sandbox access URLs.

---

## HTTPS Certificate Configuration

CubeProxy serves both **HTTPS (port 443) and HTTP (port 80)** out of the box. The E2B SDK uses HTTPS by default. The one-click install pre-installs a `cube.app` test certificate so you can try HTTPS immediately.

To use a custom domain or production certificate, choose one of the options below:

### Option A — mkcert (Local Dev Quick Setup)

`mkcert` generates a locally-trusted certificate for a custom hostname in seconds:

```bash
mkcert -install
mkcert <your-host-ip-or-domain>
```

Set `SSL_CERT_FILE` so the E2B SDK trusts the generated CA:

```bash
export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

> mkcert certificates are only trusted on machines where `mkcert -install` has been run. Not suitable for production or shared deployments.

### Option B — Your Own Certificate / Domain (Production)

Edit CubeProxy's `nginx.conf` to use your certificate and private key:

```nginx
server {
    listen 443 ssl;
    server_name your.domain.com;

    ssl_certificate     /path/to/your/cert.pem;
    ssl_certificate_key /path/to/your/key.pem;
}
```

Also pass `--sandbox-domain` to the Cube API Server to publish the correct domain in API responses (see the "Production: Custom Domain" section above).

### Option C — HTTPS-Only (Disable HTTP)

By default, CubeProxy listens on both HTTP and HTTPS. To disable the HTTP port entirely, remove the HTTP server block from CubeProxy's `nginx.conf` and drop the corresponding port mapping in `docker-compose.yaml`.

> The E2B SDK only uses HTTPS, so disabling HTTP has no impact on SDK-based clients.

---

## Quick-Start Alternative: e2b-dev-sidecar

If you are experimenting with Cube in a local development environment and want to skip both wildcard DNS setup and self-signed certificate trust issues, the **e2b-dev-sidecar** approach is the easiest option.

It starts a lightweight local proxy that intercepts E2B SDK data-plane requests and rewrites the `Host` header before forwarding them to CubeProxy. This means:

- **No wildcard DNS needed**: no `*.cube.app` record required on the developer machine
- **No certificate trust needed**: the sidecar skips server certificate verification by default (`CUBE_REMOTE_PROXY_VERIFY_SSL=false`)

See: [e2b-dev-sidecar example](../../examples/e2b-dev-sidecar/README.md)

> **Note:** e2b-dev-sidecar is a minimal dev-only implementation and is not intended for production use.
