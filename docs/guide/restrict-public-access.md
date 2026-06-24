# Restrict Public Access

By default a Cube Sandbox's [public URL](/guide/network-policy) is
reachable by anyone who knows the sandbox ID — the URL is unguessable,
but knowledge of it is the only thing standing between the workload and
the internet. For sensitive use cases (long-running agents handling
private data, dashboards exposed to partner networks, demos that
shouldn't be replayed) Cube Sandbox can require every caller to
authenticate with a per-sandbox token before any request reaches the
services inside.

This page is the operator-facing counterpart of E2B's
[Restricting public access][e2b-doc] feature; both share the
`network.allowPublicTraffic` parameter and the `e2b-traffic-access-token`
header, so existing e2b code ports without changes.

[e2b-doc]: https://e2b.dev/docs/network/restrict-public-access

## How it works

Creating a sandbox with `network.allow_public_traffic = false` returns a
per-sandbox `traffic_access_token` on the create response. Every inbound
request to the sandbox's public URL must then carry the token in either
of two equivalent headers:

- `e2b-traffic-access-token` (E2B-compatible)
- `cube-traffic-access-token` (CubeSandbox-native alias)

Requests missing the header — or carrying a wrong value — are rejected
with **HTTP 403** before reaching the sandbox.

## Quickstart

```python
from cubesandbox import Sandbox
import requests

sandbox = Sandbox.create(
    template=template_id,
    network={"allow_public_traffic": False},
)

print(sandbox.traffic_access_token)
# e.g. 4f8a2b1c9d7e3f5a6b0c8d2e4f6a9b1c3d5e7f9a0b2c4d6e8f1a3b5c7d9e0f2a

# Start a server inside the sandbox (the all-in-one test image already
# has nginx on :80, otherwise launch your own and use commands.run).
url = f"http://{sandbox.get_host(80)}/"

# No header → 403
resp = requests.get(url)
assert resp.status_code == 403

# E2B-compatible header → 200
resp = requests.get(
    url,
    headers={"e2b-traffic-access-token": sandbox.traffic_access_token},
)
assert resp.status_code == 200

# CubeSandbox-native alias → also 200
resp = requests.get(
    url,
    headers={"cube-traffic-access-token": sandbox.traffic_access_token},
)
assert resp.status_code == 200
```

The full TUI version of this script — with side-by-side probe
summaries and a verdict panel — lives at
[`examples/code-sandbox-quickstart/restrict_public_access.py`][demo].

[demo]: https://github.com/tencentcloud/CubeSandbox/blob/main/examples/code-sandbox-quickstart/restrict_public_access.py

## Default behavior

Omitting `allow_public_traffic` (or passing `True`) preserves the
historical "anyone with the URL can reach the sandbox" behavior. No
token is minted, and `sandbox.traffic_access_token` is `None`. This
keeps all existing callers working unchanged.

| Caller intent | What to pass | `traffic_access_token` | CubeProxy behavior |
|---|---|---|---|
| Default — publicly reachable | omit `network`, or `allow_public_traffic=True` | `None` | accepts every request |
| Lock down | `network={"allow_public_traffic": False}` | opaque token | rejects requests without a valid token header (403) |

## Header semantics

Both `e2b-traffic-access-token` and `cube-traffic-access-token` accept
the same opaque token, are case-insensitive (HTTP header rules), and
take precedence in that order — if both are present the `e2b-` value
wins. Provide them via your HTTP client:

```bash
curl -H "e2b-traffic-access-token: $TOKEN" \
     "http://80-$SANDBOX_ID.cube.app/"
```

```javascript
fetch(url, { headers: { "e2b-traffic-access-token": token } })
```

Token values never appear in server logs.

## Lifecycle & persistence

- **Single delivery.** The token is only attached to the response of the
  original create request. If you need it later in the workflow,
  persist it on the caller side at that moment.
- **No rotation API.** A given sandbox keeps the same token for its
  entire lifetime. To rotate, create a new sandbox.
- **No effect on `connect()` / `resume()`.** Resuming a paused sandbox
  does not re-emit the token; existing callers continue to use the
  token they already have.
- **Cleanup is automatic.** The token is dropped when the sandbox is
  destroyed.

## Combining with other network policies

`allow_public_traffic` controls **inbound** access to the sandbox's
public URL. It is orthogonal to:

- [Network Policy](/guide/network-policy) — outbound CIDR
  allow/deny lists (`allow_out` / `deny_out`,
  `allow_internet_access`).
- [Security Proxy](/guide/security-proxy) — L7 rule list applied to
  outbound HTTP/HTTPS.

A typical "private agent" deployment combines all three: deny most
outbound traffic except a few SaaS APIs (network policy + security
proxy), inject the API credential server-side so it never enters the
sandbox (security proxy), and require a token on every inbound request
(this page).

## Error model

| HTTP status | Returned when |
|---|---|
| `200` (or your upstream's status) | token matches |
| `403` | sandbox is restricted (`allow_public_traffic=false`), but the request carries no `e2b-traffic-access-token` / `cube-traffic-access-token`, or the value does not match |

All other sandbox error paths (sandbox not found, upstream unhealthy,
etc.) are unchanged from the public-by-default flow.
