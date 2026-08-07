# OpenHands × Cube Sandbox

Run [OpenHands](https://www.openhands.dev/) coding agents with every command,
file edit, and script executed inside a hardware-isolated Cube Sandbox
MicroVM — from a template whose boot snapshot already contains a **running**
OpenHands agent server.

[中文文档](./README_zh.md)

## Why this integration

OpenHands' stock `DockerWorkspace` boots a fresh agent-server container for
every session. Cube Sandbox templates snapshot the VM *after* first boot, so
this example bakes the agent server into the template and freezes it live:

| | DockerWorkspace | CubeSandboxWorkspace (this example) |
|---|---|---|
| Session startup | Container boot + server cold start | Snapshot hot-start, server already listening |
| Isolation | Shared host kernel | KVM MicroVM with its own kernel |
| Long sessions | Lost if the container stops | Whole-VM `pause()` / `resume()` — agent server, shells, and in-flight processes freeze and thaw bit-for-bit |
| Network policy | Docker networks | Platform egress control (allowlist / denylist / no-internet) |

Because the agent's session state lives in the in-VM server process,
`pause()` / `resume()` freeze and thaw the *entire session*, not just the
execution environment (see `pause_resume.py`).

## How it works

```
 host                                Cube Sandbox platform
┌──────────────────────────┐        ┌──────────────────────────────────┐
│ OpenHands SDK            │        │ MicroVM (from openhands template)│
│  Conversation ───────────┼─HTTP──▶│  agent-server :8000  (pre-warmed │
│  CubeSandboxWorkspace    │ proxy  │   in the template snapshot)      │
│   └─ e2b SDK (create/    │        │  envd :49983 (SDK ops daemon)    │
│      pause/resume/kill)  │        │  /workspace  (agent working dir) │
└──────────────────────────┘        └──────────────────────────────────┘
```

`CubeSandboxWorkspace` subclasses the SDK's `RemoteWorkspace` — the same
extension point used by `DockerWorkspace` — and points it at the sandbox's
proxied agent-server URL. `Conversation(agent=..., workspace=...)` then
automatically becomes a `RemoteConversation`: the agent loop runs inside the
MicroVM, so LLM traffic originates from the sandbox (see
[Security](#security-alignment)).

## Prerequisites

- A deployed Cube Sandbox (single-node is fine) — see the
  [Quick Start](../../docs/guide/quickstart.md) /
  [Bare-Metal Deployment](../../docs/guide/bare-metal-deploy.md) guides
- `cubemastercli` on `$PATH`, Docker for the image build
- [`uv`](https://docs.astral.sh/uv/) (or a current pip >= 26) for the
  host-side scripts — **older pips such as Ubuntu 24.04's stock 24.0 cannot
  resolve the OpenHands dependency graph** (an upstream `lmnr` /
  `opentelemetry` constraint conflict)
- An OpenAI-compatible LLM endpoint + key (`main.py` only —
  `smoke_test.py` and `pause_resume.py` need no LLM)

## 1. Build the template image

```bash
docker build -t openhands-sandbox:latest examples/openhands-integration
```

Optional plain-Docker sanity check before involving the platform:

```bash
docker run --rm -d -p 8000:8000 -p 49983:49983 --name oh-sbx openhands-sandbox:latest
curl http://127.0.0.1:8000/ready
curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health   # => 204
docker stop oh-sbx
```

## 2. Register the template

Push the image to a registry your deployment can pull from, then:

```bash
cubemastercli tpl create-from-image \
  --image     <registry>/openhands-sandbox:latest \
  --writable-layer-size 2G \
  --expose-port 8000 \
  --expose-port 49983 \
  --probe 8000 \
  --probe-path /ready
```

Probing **the agent server's readiness endpoint** (`:8000/ready`, which stays
non-2xx until initialization completes — not just envd, and not the liveness
`/health`) makes the platform take the boot snapshot only after the server is
fully initialized — the hot-start guarantee is enforced by the build, not
hoped for. Watch the build
until `READY` (see
[Creating Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md))
and note the generated `tpl-...` id.

## 3. Configure

```bash
cd examples/openhands-integration
uv venv .venv --python 3.12 && source .venv/bin/activate
uv pip install -r requirements.txt
cp .env.example .env   # fill in E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID, LLM_*
```

## 4. Run

```bash
python smoke_test.py       # no LLM key needed
python pause_resume.py     # no LLM key needed
python main.py             # full agent demo (needs LLM_* in .env)
```

- `smoke_test.py` proves the integration end to end: prints the
  create→healthy latency (the hot-start evidence), calls `/server_info`,
  and round-trips bash and file transfer through the OpenHands workspace
  API.
- `pause_resume.py` starts a 1-second ticker inside the VM, pauses the VM,
  **drops the workspace object entirely** (`kill_on_exit=False`), re-attaches
  from a fresh `CubeSandboxWorkspace(sandbox_id=...)` 8 wall-clock seconds
  later, and shows the tick sequence has no gap — the session survives the
  original process and continues from the exact frozen instant.
- `main.py` runs a real OpenHands conversation (write a program, execute it,
  fix errors) and then verifies the result *from inside the sandbox*,
  independent of the agent's own claims.

## Using it in your own code

```python
from openhands.sdk import LLM, Conversation
from openhands.tools.preset.default import get_default_agent
from cubesandbox_workspace import CubeSandboxWorkspace

agent = get_default_agent(llm=LLM(model="openai/...", api_key=...), cli_mode=True)

with CubeSandboxWorkspace(template="tpl-...") as workspace:
    conversation = Conversation(agent=agent, workspace=workspace)
    conversation.send_message("fix the failing test in /workspace/repo")
    conversation.run()
    workspace.pause()      # freeze the whole session ...
    workspace.resume()     # ... and continue it later

# Or keep a paused session beyond this process (kill_on_exit=False),
# then re-attach — even days later, even from another process:
#   workspace = CubeSandboxWorkspace(sandbox_id="...")   # auto-resumes
```

## Security alignment

- **Egress**: the agent loop runs inside the MicroVM, so the sandbox needs
  egress to your LLM endpoint. To restrict everything else, create the
  sandbox with a network allowlist containing only that endpoint (see the
  [network policy guide](../../docs/guide/network-policy.md) and the
  quickstart's `network_allowlist.py` pattern) — the agent then cannot reach
  anywhere except its own brain.
- **Where the LLM key lives**: OpenHands' agent-server architecture runs the
  agent loop inside the workspace, so a stock setup — upstream's
  `DockerWorkspace` included — sends `LLM(api_key=...)` into the sandbox
  with the conversation. If the raw key must stay out of the VM, configure
  the agent with a placeholder and let CubeEgress
  [credential injection](../../docs/guide/security-proxy.md) attach the real
  `Authorization` header on the wire (pattern:
  [`examples/pi-agent-integration/network_policy.py`](../pi-agent-integration/network_policy.py));
  this template's `SSL_CERT_FILE` already makes the pre-warmed server trust
  the interception CA.
- **Inbound — one flag to go private**: `CubeSandboxWorkspace(...,
  private_traffic=True)` creates the sandbox with
  `allow_public_traffic=False` and sends the per-sandbox
  [traffic token](../../docs/guide/restrict-public-access.md) with every
  workspace HTTP request (`e2b-traffic-access-token`), so only token holders
  can reach the agent server through the platform proxy — strongly
  recommended on shared deployments. Scope: OpenHands 1.38.0's conversation
  WebSocket does not attach custom headers, so full `Conversation` runs need
  public traffic; workspace API calls are unaffected. The token stays valid
  across pause/resume (the platform validates it server-side and the
  workspace preserves it client-side); to re-attach to a private sandbox
  from a new process, persist `workspace.traffic_token` and pass it back as
  `traffic_access_token=`. For defense in depth the server also supports
  session keys:
  launch it with `SESSION_API_KEY=<secret>` (the template CMD forwards env)
  and set the same value as `CubeSandboxWorkspace(api_key=...)` — the SDK
  sends it as `X-Session-API-Key`.
- **Accounts**: the agent server runs as the same uid-1000 `user` account
  envd uses for SDK file/command operations, so agent-created and SDK-created
  files stay mutually accessible. This is an ownership convention, not a
  privilege boundary — `cubesandbox-base` grants `user` passwordless sudo;
  the isolation boundary is the MicroVM itself.

## Resource recommendations

- Template build: ~750 MB image (standalone CPython + OpenHands server).
- Sandbox: 2 vCPU / 2 GB RAM is comfortable for the CLI toolset
  (bash + file editor); add headroom for whatever your tasks compile or run.
- `--writable-layer-size 2G` covers agent-created files and pip caches for
  typical tasks (a freshly booted sandbox uses only ~164 KB of it); raise it
  for repository-heavy work.

## Known limitations

- **Old pips cannot install the host deps**: `lmnr` pins a pre-release
  opentelemetry that older pip resolvers (e.g. Ubuntu 24.04's stock 24.0)
  fail on — use uv or a current pip (>= 26). The template build already uses
  uv internally.
- **Browser toolset disabled** (`cli_mode=True`): keeping Playwright/Chromium
  out of the template keeps it small. To enable browsing agents, install the
  browser stack in the Dockerfile and drop `cli_mode`.
- **Version pairing**: host `openhands-sdk`/`openhands-tools` and the
  template's `openhands-agent-server` are pinned to the same release train
  (1.38.0). Bump them together; `workspace.get_server_info()` shows the
  server side for debugging mismatches.

## Troubleshooting

- *Health timeout in `CubeSandboxWorkspace`*: confirm the template was built
  with the agent-server CMD (step 1's local Docker check), that it was
  registered with `--expose-port 8000`, and that `E2B_API_URL` points at your
  deployment. If your deployment's cube-proxy serves HTTP on a non-80 port,
  set `CUBE_PROXY_HTTP_PORT` in `.env` to match.
- *`get_server_info` succeeds but conversations fail*: usually an LLM config
  problem — remember the LLM is called **from inside the sandbox**; check
  egress policy and `LLM_BASE_URL` reachability from the VM
  (`workspace.execute_command("curl -sI $LLM_BASE_URL")`).

## References

- Integration guide: [`docs/guide/integrations/openhands.md`](../../docs/guide/integrations/openhands.md)
- Creating templates from OCI images: [`docs/guide/tutorials/template-from-image.md`](../../docs/guide/tutorials/template-from-image.md)
- OpenHands Agent SDK: <https://github.com/OpenHands/software-agent-sdk>
