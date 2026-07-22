# Outbound URL Security Layer

Before CubeAPI calls an external URL (such as `auth_callback_url`), the request goes through the outbound URL security layer to prevent SSRF (Server-Side Request Forgery) attacks and DNS rebinding bypasses.

## Protection Goals

The following protections are always active (default build):

- Block local loopback addresses: `127.0.0.1`, `::1`, `localhost`
- Block private network addresses: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, etc.
- Block link-local addresses: `169.254.0.0/16` (including cloud metadata service `169.254.169.254`)
- Block IPv6 unique-local, multicast, and other non-public addresses
- Prevent DNS rebinding attacks where a hostname resolves to a public IP during validation but to an internal IP at request time (DNS pinning)

Additional webhook-specific helpers (including response body size limiting via `read_body_with_limit`) are included in the default build through the `webhooks` Cargo feature. To disable them, build with `--no-default-features`.

## Configuration

Configure through environment variables using `__` as the nested separator:

```bash
# Allowed URL schemes, default: https
OUTBOUND_URL_SECURITY__ALLOWED_SCHEMES=https

# Whether to allow resolving to private/loopback/link-local addresses, default: false
OUTBOUND_URL_SECURITY__ALLOW_PRIVATE_IPS=false

# DNS resolution timeout in milliseconds, default: 5000
OUTBOUND_URL_SECURITY__RESOLVE_TIMEOUT_MS=5000
```

`ALLOWED_SCHEMES` supports multiple comma-separated values:

```bash
OUTBOUND_URL_SECURITY__ALLOWED_SCHEMES=http,https
```

## Default Behavior

Production defaults:

- Only `https` is allowed
- All private/loopback/link-local addresses are rejected
- DNS resolution timeout is 5 seconds

If the configured `auth_callback_url` violates the above policy, CubeAPI fails immediately on startup with a clear error message.

## Local Development

In local development or test environments, callback URLs often use `http://127.0.0.1` or LAN addresses. You can relax the restrictions with:

```bash
OUTBOUND_URL_SECURITY__ALLOWED_SCHEMES=http,https
OUTBOUND_URL_SECURITY__ALLOW_PRIVATE_IPS=true
```

::: warning Do not enable allow_private_ips in production
Setting `allow_private_ips=true` allows CubeAPI to access internal services, which significantly increases SSRF risk. Use it only in local development.
:::

## Changes to auth_callback_url

When `auth_callback_url` is enabled:

1. CubeAPI validates the URL against the outbound security policy during startup.
2. After validation, a dedicated hardened HTTP client is built:
   - Automatic redirects are disabled
   - HTTP proxy is disabled (the client ignores `HTTP_PROXY`/`HTTPS_PROXY`)
   - Connection and request timeouts are set
   - DNS resolution results are pinned to prevent DNS rebinding
3. The auth middleware uses this dedicated client to send POST requests to the callback URL.

Because the proxy is disabled, environments that require a forward proxy for egress cannot
use `auth_callback_url` unless the policy is relaxed or a future proxy-aware option is added.

If startup fails with an error like `resolved address 127.0.0.1 is not a public IP`, it means `auth_callback_url` points to a forbidden address. Change it to a public reachable address, or relax the policy in a local development environment.

## Future Reuse by Webhooks

This security layer is also designed for future webhook implementations. Webhook code can use it as follows:

```rust
use cube_api::security::outbound_url::{OutboundUrlPolicy, build_secure_client};
use std::time::Duration;

let policy = OutboundUrlPolicy::webhook_default();
let validated = policy.validate("https://your-webhook.example.com/hook").await?;
let client = build_secure_client(&validated, Duration::from_secs(5), Duration::from_secs(10))?;
```

`webhook_default()` uses the same strict policy as production: `https` only and private addresses rejected.

## Validation Examples

The following URLs are rejected by the default policy:

| URL | Rejection Reason |
|---|---|
| `http://example.com/hook` | Scheme not in allowlist |
| `https://127.0.0.1/hook` | Loopback address |
| `https://localhost/hook` | Explicitly forbidden host |
| `https://10.0.0.1/hook` | Private address |
| `https://192.168.1.1/hook` | Private address |
| `https://169.254.169.254/hook` | Link-local / metadata address |
| `https://[::ffff:127.0.0.1]/hook` | IPv4-mapped loopback address |
| `https://user:pass@example.com/hook` | Embedded credentials |

The following URLs pass by default:

| URL | Reason |
|---|---|
| `https://example.com/hook` | Public domain resolving to a public IP |
| `https://1.2.3.4/hook` | Public IP literal |
| `https://example.com:8443/hook` | Custom port is preserved |
