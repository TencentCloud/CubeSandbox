# Security Proxy

Cube Sandbox includes a per-host transparent egress proxy — **CubeEgress** —
that intercepts every outbound HTTP/HTTPS request a sandbox makes,
matches it against operator-supplied L7 rules, and either lets it
through, denies it, or rewrites it on the fly. The data plane lives
between the sandbox's TAP device and the public internet; the
sandbox itself can't bypass it without leaving the host.

The proxy gives you three primary controls, all driven by the same
rule list attached at sandbox-creation time:

- **Domain filtering** — allow/deny outbound requests by SNI, host,
  HTTP method, scheme, or exact path.
- **Credential injection** — append static headers (typically
  `Authorization: Bearer …`) so the workload never sees the raw
  secret.
- **Access auditing** — every decision (allow / deny / inject /
  TLS handshake outcome) is written to a per-host JSONL audit log.

## How it intercepts

CubeEgress runs as a host-network container and binds two TPROXY
listeners on the sandbox-facing IP:

```
sandbox ──→ cube-dev (host iface)
              │
              ├─ iptables mangle/PREROUTING -j TPROXY
              │     port 80  → 192.168.0.1:8080  (HTTP listener)
              │     port 443 → 192.168.0.1:8443  (HTTPS listener)
              │
              ▼
       CubeEgress (OpenResty + lua)
              │
              ├─ ssl_certificate_by_lua → mint a leaf cert for the
              │                            requested SNI, signed by
              │                            the CubeEgress root CA
              │                            (baked into the template
              │                            at build time)
              │
              ├─ access_by_lua → match L7 rules; allow / deny / inject
              │
              └─ proxy_pass → original destination IP (preserved via
                              IP_TRANSPARENT)
```

Because the leaf cert's chain validates against the CubeEgress root
CA already trusted by the sandbox's system store, the workload's
TLS client doesn't notice the MITM and the proxy can read and
rewrite request/response data legitimately.

## Domain filtering

Each rule has a `match` object listing the conditions it applies to,
and an `action` saying what to do when the conditions all match.
First-match-wins; anything that matches no rule is denied by
default.

```python
from cubesandbox import Sandbox, Rule, Match, Action

rules = [
    # Block the apex by exact host match.
    Rule(
        name="deny_example_apex",
        match=Match(scheme="https", host="example.com"),
        action=Action(allow=False),
    ),
    # Allow any subdomain via the *.<domain> SNI form.
    Rule(
        name="allow_example_subdomains",
        match=Match(scheme="https", sni="*.example.com"),
        action=Action(allow=True),
    ),
]

with Sandbox.create(network={"rules": rules}) as sb:
    sb.commands.run("curl -s https://www.example.com")  # → upstream
    sb.commands.run("curl -s https://example.com")      # → 403 from CubeEgress
```

Match fields (all optional, AND'd together):

| Field | Type | Notes |
| --- | --- | --- |
| `scheme` | `"http"` / `"https"` | |
| `sni` | string | TLS ClientHello SNI; supports leading `*.` for "any subdomain" — `*.example.com` matches `www.example.com` and `foo.bar.example.com`, but not the apex |
| `host` | string | Match against the HTTP `Host:` header (port stripped); same semantics as `sni` — exact match, or leading `*.` for "any subdomain" (case-insensitive) |
| `method` | list of methods | OR within the list (`["GET", "POST"]`) |
| `path` | string | Match against `ngx.var.uri`; exact match by default, or a single trailing `*` for prefix match (e.g. `/v1/*` matches `/v1/chat` and `/v1/embeddings`) |

A request must match every present field; absent fields are
wildcarded.

::: tip Single-level vs multi-level subdomain
`*.example.com` matches **all** subdomains regardless of label
depth. To allow only single-level subdomains (e.g. `www`,
`api`) you have to add explicit `host="…"` deny rules for the
nested cases you don't want.
:::

A deny action returns HTTP 403 from CubeEgress without ever
contacting the upstream — the sandbox sees the rejection
immediately, no DNS leak, no TCP handshake.

## Credential injection

Inject rules attach static headers to outbound requests after the
match succeeds. The classic case is auth-token injection:

```python
from cubesandbox import Sandbox, Rule, Match, Action, Inject

rules = [
    Rule(
        name="deepseek_api",
        match=Match(scheme="https", host="api.deepseek.com",
                    method=["POST"], path="/v1/chat",
                    sni="api.deepseek.com"),
        action=Action(
            allow=True,
            audit="metadata",
            inject=[Inject(
                header="Authorization",
                format="Bearer ${SECRET}",
                secret="sk_xxxxxxxx",  # operator-side, never seen by the sandbox
            )],
        ),
    ),
]
```

Behavior:

- The workload calls the API **without** any `Authorization`
  header. CubeEgress sees the bare request, evaluates the
  inject list after the match, and adds `Authorization: Bearer
  sk_xxxxxxxx` before forwarding.
- `format` defaults to `"${SECRET}"` (the raw secret as the
  whole header value); use `"Bearer ${SECRET}"` or any other
  template containing the `${SECRET}` placeholder for non-bearer
  schemes.
- Inject only fires when `action.allow=true`. A deny rule with
  `inject=[…]` is a configuration error — the inject is dropped.

The point of the inject path is that the secret stays on the
operator side: it lives in the rule list, gets pushed to
CubeEgress at sandbox creation, and is never exposed to the
sandbox's environment, filesystem, or process space.

## Access auditing

Every request goes through one of three audit levels, controlled
per rule via `action.audit`:

| Level | What gets logged |
| --- | --- |
| `none` | Nothing |
| `metadata` (default) | timestamp, sandbox IP, dst IP/port, scheme, host, method, path, status, request/response sizes, latency, TLS version + cipher, upstream addr |
| `full` | Reserved — same as `metadata` today; full request/response body capture is planned |

Logs are JSONL on the host at `/data/log/cube-egress/access.jsonl`,
one line per request:

```json
{
  "ts": "2026-05-29T11:24:01+08:00",
  "sandbox_ip": "192.168.1.154",
  "dst_ip": "104.16.132.229",
  "dst_port": 443,
  "scheme": "https",
  "host": "api.deepseek.com",
  "method": "POST",
  "path": "/v1/chat",
  "status": 200,
  "bytes_in": 412,
  "bytes_out": 1856,
  "latency_ms": 384,
  "tls_version": "TLSv1.3",
  "cipher": "TLS_AES_128_GCM_SHA256",
  "upstream_status": "200",
  "upstream_addr": "104.16.132.229:443"
}
```

Two extra event types land in the same file with their own shape:

- **`security_event`** — for default-deny rejections, malformed
  rule short-circuits, host/SNI mismatches, and inject-fired
  events. Carries the rule name plus a `reason` field describing
  which guard fired.
- **`tls_handshake`** — emitted when the handshake itself fails
  (cert signing failed, client closed the connection mid-handshake,
  etc.) before any HTTP message exists. Lets you distinguish "TLS
  never completed" from "request was rejected post-decrypt".

::: warning Secrets are redacted
Inject `secret` values are scrubbed from any log path before
write. The audit log records only the rule name and the fact that
inject ran; the secret itself never leaves the lua VM in clear.
:::

## When the proxy isn't in the path

A few edge cases skip CubeEgress entirely; they're worth knowing
about:

- **Internal `cube-dev` traffic** — sandbox-to-sandbox traffic and
  traffic to in-cluster services (Cube API, etc.) doesn't enter the
  TPROXY chain, so rules don't apply.
- **Non-HTTP egress on TCP/UDP** — the TPROXY chain only redirects
  ports 80 and 443. Direct TCP to other ports still goes out
  subject to the L3/L4 `allow_out` / `deny_out` policy on the
  CubeNet data plane, but is invisible to CubeEgress.
- **Sandboxes built from templates without the CA bake** — if the
  template was created with `--with-cube-ca=false`, the sandbox's
  TLS clients don't trust CubeEgress's leaf certs and HTTPS calls
  fail with self-signed-cert errors before any rule is consulted.

For host-level deployment of the proxy itself (systemd units, CA
bootstrap, the iptables one-shot service), see the
[`deploy/one-click`](https://github.com/tencentcloud/CubeSandbox/tree/master/deploy/one-click)
sources — those wire CubeEgress into both the control and compute
targets automatically.

## Extending the proxy

The match/inject grammar above is intentionally narrow — it covers
the 80% case (allow this host, deny that one, attach a token) with
operator-level configuration only. When you need behavior the
declarative rules can't express — content inspection, cross-request
state, calls to an external classifier — CubeEgress is a regular
OpenResty server and you can drop in your own lua to extend it.

### Where the lua lives

The data plane is split into one file per phase under
`CubeEgress/lua/`:

| File | Phase | What it does |
| --- | --- | --- |
| `cert_signer.lua` | `ssl_certificate_by_lua` | Mints leaf certs for the SNI seen on the wire |
| `bootstrap.lua` | `init_worker_by_lua` (worker 0 only) | Pulls initial policies from network-agent |
| `access_phase.lua` | `access_by_lua` | Runs match → action → inject for every request |
| `policy.lua` | (module) | In-memory policy store, fed by `admin.lua` and `bootstrap.lua` |
| `admin.lua` | `content_by_lua` on `:9090` | CRUD admin API for policies |
| `audit.lua` | `log_by_lua` | Writes the JSONL audit line per request |
| `redactor.lua` | (helper) | Scrubs secrets from anything user-visible |

`nginx.conf` reaches each of these via `require("…")` from the
phase block; e.g. the `access_by_lua_block` for both the HTTP and
HTTPS server blocks calls `require("access_phase").decide()`.

`lua_package_path` is set to
`/usr/local/openresty/nginx/lua/?.lua`, so any new file dropped
into that directory becomes loadable with `require("name")` once
nginx reloads.

### Adding a new phase hook

The simplest extension shape is a new module that you `require`
from a new phase block in `nginx.conf`. Example: a prompt-content
filter that intercepts request bodies sent to LLM endpoints,
inspects the prompt, and rejects or rewrites it.

**1) Drop a new lua module under `CubeEgress/lua/`:**

```lua
-- lua/prompt_filter.lua
-- Phase 2 (after access_by_lua decides allow): inspect the JSON
-- body of POST requests to LLM upstreams, reject ones containing
-- forbidden patterns, optionally rewrite those that look risky.

local cjson = require("cjson.safe")
local _M = {}

-- Cheap heuristic; replace with whatever your security team needs.
-- A real deployment would call out to a classifier service via
-- lua-resty-http (already shipped in the openresty-tproxy base
-- image), or load a regex set from policy.
local FORBIDDEN_PATTERNS = {
    "ignore previous instructions",
    "system prompt",
    "DAN mode",
}

local function is_target_endpoint(host, path)
    -- Apply the filter only to known LLM chat endpoints. Add hosts
    -- here, or read this list from a lua_shared_dict that the admin
    -- API can poke at runtime.
    return host == "api.deepseek.com" and path == "/v1/chat/completions"
end

function _M.inspect()
    local host = ngx.var.cube_audit_host or ngx.var.http_host
    local path = ngx.var.uri
    if not is_target_endpoint(host, path) then
        return
    end

    -- Body capture: explicitly read it; nginx doesn't buffer POST
    -- bodies into Lua-readable form by default. The
    -- proxy_request_buffering=on already in nginx.conf means we
    -- can read here and the buffered copy still goes upstream.
    ngx.req.read_body()
    local body = ngx.req.get_body_data() or ""

    local payload, err = cjson.decode(body)
    if not payload or err then
        return  -- non-JSON, leave it alone
    end

    -- Walk the OpenAI-shape messages array.
    for _, msg in ipairs(payload.messages or {}) do
        local content = (msg or {}).content or ""
        for _, pat in ipairs(FORBIDDEN_PATTERNS) do
            if string.find(string.lower(content), pat, 1, true) then
                ngx.log(ngx.WARN, "prompt filter rejected: pattern=", pat,
                                  " sandbox=", ngx.var.remote_addr)
                -- Reuse the audit module so this rejection lands in
                -- access.jsonl alongside ordinary deny events.
                local audit = require("audit")
                pcall(audit.write_security_event, "prompt_filter:" .. pat,
                      ngx.ctx.cube_decision)
                return ngx.exit(ngx.HTTP_FORBIDDEN)
            end
        end
    end
end

return _M
```

**2) Wire it into `nginx.conf`:**

The existing HTTPS server block already runs
`access_by_lua_block { require("access_phase").decide() }` for the
match/inject decision. Hang the new module off the same block so
it fires only after the rule said "allow":

```nginx
server {
    listen 192.168.0.1:8443 ssl transparent reuseport;
    # ...

    location / {
        access_by_lua_block {
            require("access_phase").decide()
            require("prompt_filter").inspect()   -- ← new line
            require("debug_dump").dump_request("https")
        }
        # ...
    }
}
```

**3) Rebuild + redeploy:**

The lua files are baked into the cube-egress container image:

```bash
cd CubeEgress && make build         # rebuilds the image
sudo systemctl restart cube-sandbox-cube-egress
```

Or, for live iteration without a full image rebuild, bind-mount
the local `lua/` dir into the running container and reload:

```bash
sudo docker run -d --name cube-egress \
  --network=host \
  -v $PWD/CubeEgress/lua:/usr/local/openresty/nginx/lua:ro \
  -v $PWD/CubeEgress/nginx.conf:/usr/local/openresty/nginx/conf/nginx.conf:ro \
  ...other flags...
sudo docker exec cube-egress nginx -s reload
```

### What you can reach from a custom phase

When your module runs in `access_by_lua` or later phases, the
following are available because the upstream phases already ran:

| Source | Access | Useful for |
| --- | --- | --- |
| `ngx.ctx.cube_decision` | the decision struct from access_phase: `rule_name`, `allow`, `audit_level`, `inject_count` | branching on the matched rule |
| `ngx.var.ssl_server_name` | the original SNI captured at handshake | identity-locked routing |
| `ngx.var.cube_audit_host` | match host (SNI > Host header > dst IP) | endpoint lookups |
| `ngx.var.remote_addr` | sandbox IP — the policy_store key | per-sandbox state |
| `lua_shared_dict policy_store` | the live policy table | reading rules at runtime, e.g. extra match fields you encode in the rule body |
| `lua_shared_dict cert_cache` | leaf cert cache | TLS introspection |
| `audit.write_security_event(reason, decision)` | emits a `security_event` JSONL line | making rejections show up in audit |
| `redactor.scrub(s)` | scrub secrets out of a string | logging anything user-visible |

### Suggested patterns

- **Run after `access_phase`, not before.** That way deny rules
  short-circuit before your hook does expensive work.
- **Use the audit module rather than `ngx.log` only.** Hand-rolled
  log lines won't show up in `access.jsonl`; downstream tooling
  (SIEM ingest, SOC dashboards) keys on the JSONL schema.
- **Cap latency.** A prompt filter that calls out to an external
  classifier should set `lua-resty-http` timeouts well under the
  upstream `proxy_connect_timeout`. The sandbox is waiting on you.
- **Fail safe.** If your classifier service is down, decide up-front
  whether you want to deny (security-first) or allow (availability-
  first). Don't crash the worker — wrap the call in `pcall` and
  surface a `security_event` either way.
- **Don't put secrets in `ngx.shared.*`.** Those dicts are visible
  to every worker; sensitive material belongs in the inject path
  (which only writes outbound) or in `init_by_lua` per-worker
  globals.

For more involved scenarios — a middleware chain, content
rewriting, real-time classification — the same machinery applies;
OpenResty's full lua API surface is available, including
`lua-resty-http` for outbound calls and `cjson` for body
manipulation.
