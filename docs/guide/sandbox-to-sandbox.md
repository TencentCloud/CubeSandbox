# Sandbox-to-Sandbox Access

One sandbox can reach a service inside another sandbox over two data-plane
paths: **CubeProxy Host routing** (the same ingress as the public URL) and
the **node host-port mapping** (CubeVS eBPF DNAT). Neither requires
`*.cube.app` DNS — a sandbox only needs the node IP.

```text
Option 1: CubeProxy Host routing
  sandbox A ──► CubeProxy :80 ── Host: <port>-<sandboxID-B>.cube.app ──► service in B

Option 2: Node host-port mapping (hostIP:hostPort)
  sandbox A ──► nodeIP:hostPort ── from_world eBPF DNAT (NIC ingress) ──► service in B
```

Runnable end-to-end examples live in
[examples/code-sandbox-quickstart](https://github.com/TencentCloud/CubeSandbox/tree/main/examples/code-sandbox-quickstart):
`sandbox-curl-sandbox.py` (option 1) and `sandbox-port-mapping.py` (option 2).

## Choosing Between Them

| Dimension | Option 1: CubeProxy Host routing | Option 2: Host-port mapping |
|-----------|----------------------------------|-----------------------------|
| Protocols | HTTP / HTTPS / gRPC only (L7, Host-header routing) | Any TCP protocol (L4 DNAT) |
| Data plane | Via CubeProxy, one proxy hop | eBPF DNAT straight into the sandbox, no proxy hop |
| Ingress auth | Supports the per-sandbox token of `allow_public_traffic=False` | No token support; exposure is governed by network policy + expose-port |
| Service port source | Any port — the `<port>` prefix in the URL selects it | Only ports declared via template `--expose-port` |
| Endpoint stability | Stable URL format, unchanged across pause/resume | hostIP:hostPort may change across pause/resume (see below) |
| DNS in sandbox | Not needed (`--resolve` pins the proxy IP) | Not needed (plain node IP) |

Typical scenarios:

- **Option 1**: an agent in one sandbox calls an HTTP API / gRPC service
  hosted in another; you want to reuse CubeProxy's token auth; the service
  port is not fixed or is dictated by the image.
- **Option 2**: arbitrary TCP between sandboxes (databases, message queues,
  custom binary protocols); latency-sensitive paths that should bypass the
  proxy; ports can be declared in the template upfront.

## Option 1: CubeProxy Host Routing

CubeProxy routes by the `<port>-<sandboxID>.<domain>` Host header. Inside
sandbox A, pin the name to the CubeProxy node IP with curl `--resolve` — no
DNS needed:

```bash
curl -sS --resolve 8080-<sandboxID-B>.cube.app:80:<proxy-node-ip> \
     http://8080-<sandboxID-B>.cube.app/
```

### Template build

Option 1 has **no special template requirements**: CubeProxy forwards to any
port inside the sandbox based on the `<port>` prefix in the URL, with no
`--expose-port` declaration needed. The standard template from the
[Quickstart](./quickstart.md) is enough:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

(The two `--expose-port` flags here belong to the sandbox-code image's own
code interpreter / envd and are unrelated to option-1 routing — under option 1
the service in B can listen on any port.)

Properties:

- Same ingress as the public URL, so **auth semantics are identical**: when B
  was created with `network={"allow_public_traffic": False}`, requests must
  carry the `e2b-traffic-access-token` header (see
  [Restrict Public Access](./restrict-public-access.md)).
- Requires CubeProxy.

Limitations:

- **L7 protocols only**: HTTP / HTTPS / gRPC (gRPC rides HTTP/2, which the
  Host routing handles). Arbitrary TCP/UDP services (databases, Redis, custom
  protocols) cannot use this path — use option 2 instead.

## Option 2: Node Host-Port Mapping (hostIP:hostPort)

When a template declares `--expose-port`, Cubelet programs a CubeVS port
mapping at sandbox create: TCP traffic to `<nodeIP>:<hostPort>` is DNAT'd by
the `from_world` eBPF program on the primary NIC ingress directly into the
sandbox — **no CubeProxy involved**.

### Prerequisites

- cubemaster `cubelet_conf.enable_exposed_port: true` (default);
- the template was built with the service port in `--expose-port`.

### Building a template with exposed ports

Using the official `sandbox-code` image as an example (see the
[Quickstart](./quickstart.md) for registry addresses; use the
`cube-sandbox-cn` registry in mainland China):

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \   # code interpreter (sandbox-code default)
  --expose-port 49983 \   # envd (sandbox-code default)
  --expose-port 8080  \   # your service port
  --probe 49999
```

Notes:

- `--expose-port` may be repeated; only declared ports get a hostPort mapping.
- `--probe` is the template readiness probe port — 49999 for `sandbox-code`.
- The command prints a `template_id` and waits for the build to reach
  `READY`; `cubemastercli tpl list` shows the status too.
- Adding ports to an existing template means rebuilding it — expose-port is a
  template property and cannot be changed at runtime.

Create sandboxes with the new template ID; Cubelet programs the port mapping
automatically at sandbox start, with no extra create-request parameters.

### Discovering the allocated host port

Host ports are allocated per sandbox. Query CubeMaster's info API:

```bash
curl -s -H 'X-Cube-Caller-Host-IP: 198.51.100.254' \
  'http://<cubemaster>:8089/cube/sandbox/info?sandbox_id=<B>&container_port=8080'
# → data[0].exposed_port_endpoint = "9.135.152.214:20006"
#   data[0].exposed_port_mode     = "host_port"
```

Callers on the sandbox's own node get the `tap_ip` endpoint
(`sandboxIP:containerPort`) by default; the `X-Cube-Caller-Host-IP` header
forces the `host_port` answer.

### Accessing from sandbox A

```bash
curl -sS http://<nodeIP>:<hostPort>/
```

When A and B are on the same node, A's packets are SNATed in `from_cube`
and looped back into the node NIC ingress in software, which triggers the
DNAT — this does not depend on NIC/switch hairpinning. Cross-node traffic
(A and B on different nodes) traverses the wire to the same ingress path.
One caveat:

- Curling from the **node itself** (127.0.0.1 or the node IP) always fails —
  host-local traffic never traverses NIC ingress. Use the `tap_ip` endpoint
  or CubeProxy for on-node access.

Properties:

- Direct data plane, no proxy hop; **any TCP protocol** works (HTTP,
  databases, message queues, custom binary protocols).
- Bypasses CubeProxy, so the `allow_public_traffic` token does **not** apply —
  exposure is governed by [Network Policy](./network-policy.md) plus the
  template's expose-port list.

Limitations:

- **TCP only**, no UDP.
- The service port must be declared via the template's `--expose-port`.
- **The endpoint may change across pause/resume**: host ports are allocated
  at sandbox create (including the internal re-create on resume), so after a
  pause → resume cycle `hostIP:hostPort` can differ (the hostIP changes too
  when the sandbox is rescheduled to another node). Callers must re-query the
  info API after B resumes — do not cache the endpoint long-term.

## Alternative: control plane (SDK connect)

When the goal is to *operate* sandbox B from inside A (run commands, read and
write files) rather than reach a service port, install the `cubesandbox` SDK
in A and `Sandbox.connect()` to B:

```python
from cubesandbox import Sandbox
sandbox_b = Sandbox.connect("<sandboxID-B>")
sandbox_b.commands.run("echo hello")
```

Inside A, set `CUBE_API_URL` to a node-reachable CubeAPI address (a loopback
`127.0.0.1` from your `.env` would point at the sandbox itself) and
`CUBE_PROXY_NODE_IP`. Full example: `sandbox-in-sandbox.py`.

## Testing

Both options ship runnable end-to-end demo scripts (in
[examples/code-sandbox-quickstart](https://github.com/TencentCloud/CubeSandbox/tree/main/examples/code-sandbox-quickstart)).
Each script: creates sandboxes A and B → starts an HTTP service in B → curls
it from A → asserts the response body:

- `sandbox-curl-sandbox.py` — option 1, works with the standard template;
- `sandbox-port-mapping.py` — option 2, requires a template with the service
  port in `--expose-port` (see "Building a template with exposed ports").

Setup, run commands, and expected outputs are documented in that directory's
README. If the curl from A gets ECONNREFUSED (exit code 2),
`sandbox-port-mapping.py` prints a troubleshooting checklist (port not
exposed, `enable_exposed_port` off, egress policy blocking).

## Common pitfalls

- **Egress policy**: if A uses `allow_out` / `deny_out` (see
  [Network Policy](./network-policy.md)), the allowlist must cover the
  CubeAPI / CubeProxy / host node IP.
- **API address**: for control-plane calls from inside a sandbox, loopback
  (`127.0.0.1`) refers to the sandbox itself — replace it with a
  node-reachable address.

## Further reading

- [Network Policy](./network-policy.md) — sandbox egress allow/deny lists and L7 rules.
- [Restrict Public Access](./restrict-public-access.md) — per-sandbox tokens on the CubeProxy ingress.
- [Security Proxy](./security-proxy.md) — domain filtering and credential injection for outbound HTTP/HTTPS.
