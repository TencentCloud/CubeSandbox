# Pi Agent + CubeSandbox Example

[中文文档](README_zh.md)

Run the [Pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
— a terminal-native AI coding agent — inside a CubeSandbox MicroVM. The agent
edits files, runs commands, and reaches an LLM API entirely within an isolated,
reproducible sandbox.

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An LLM provider API key (DeepSeek by default; Anthropic, OpenAI, and other
  providers are also supported).
- Python 3.10+ for the host driver scripts.

## 1. Build the base template image

```bash
docker build --platform linux/amd64 \
  -t localhost:5000/pi-agent-cube:latest \
  examples/pi-agent-integration
docker push localhost:5000/pi-agent-cube:latest
```

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image localhost:5000/pi-agent-cube:latest \
  --alias pi-agent \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

Note the `template_id` once the job reaches `READY`.

## 3. Configure the host driver

```bash
cd examples/pi-agent-integration
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and your provider key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `CUBE_WARMUP_TEMPLATE_ID` | `run_pi_warmup.py` | From the warmup template job below |
| `PI_PROVIDER` | Pi CLI | `deepseek` (default), `anthropic`, `openai`, ... |
| `PI_MODEL` | Pi CLI | Model id for the provider |

## 4. One-shot run (direct key flavor)

```bash
python run_pi_agent.py --prompt "Create hello.py that prints 'Hello from CubeSandbox' and run it."
```

The key is forwarded per-command via `sandbox.commands.run(..., envs=...)`, so it
lives only for the lifetime of that exec call — never written to a persistent
file inside the VM.

> **Security:** this direct flavor leaves egress open, so a compromised agent
> could exfiltrate the injected key. For shared clusters use the vault flavor
> (step 6): default-deny egress + on-the-wire key injection.

All three scripts run Pi with `--mode json` and render a concise transcript by
default (assistant text, tool calls, and any failures). Pass `--raw` (or set
`PI_STREAM_RAW=1`) to stream Pi's raw JSONL events instead.

## 5. Pause / resume (session persistence)

```bash
python resume_pi_agent.py
```

Turn 1 asks Pi to write `/workspace/plan.md`, then `sandbox.pause()` snapshots
the VM. The script reconnects with `Sandbox.connect(sandbox_id)`, verifies
`/workspace/plan.md` and Pi's state directory (`/root/.pi/agent`) survived, then
runs turn 2 to continue the work. The sandbox lifecycle is managed manually with
`try/finally` (not a context manager), so the pause is not undone by an early
`kill`.

## 6. Build a pre-warmed Pi SDK snapshot

The one-shot example starts a new `pi` process for every task. The warmup
variant instead starts a resident Node adapter as the image command. It creates
one persistent Pi SDK `AgentSession` during boot and only then makes
`GET /readyz` return HTTP 200. Cube waits for that probe before taking the
template snapshot, so restored sandboxes already contain the initialized Node
process and session.

```bash
docker build --platform linux/amd64 \
  -f Dockerfile.warmup \
  --build-arg PI_AGENT_IMAGE=localhost:5000/pi-agent-cube:latest \
  -t localhost:5000/pi-agent-warmup-cube:latest .
docker push localhost:5000/pi-agent-warmup-cube:latest
```

Create the warmup template using the adapter probe (port `8080` is the default
and can be changed with `PI_WARMUP_PORT`):

```bash
cubemastercli tpl create-from-image \
  --image localhost:5000/pi-agent-warmup-cube:latest \
  --alias pi-warmup \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe       8080 \
  --probe-path  /readyz
```

Put the resulting template ID in `CUBE_WARMUP_TEMPLATE_ID`, then send a task to
the restored session:

```bash
python run_pi_warmup.py \
  --prompt "Create hello.py that prints 'Hello from pre-warmed Pi' and run it."
```

The driver sends the provider key with the `/prompt` request. The adapter keeps
it in Pi's in-memory auth storage and does not write it into the session or the
template snapshot. The adapter deliberately serializes prompts for its single
session; a concurrent request receives HTTP 409.

## 7. Restricted egress + key vault (recommended for shared clusters)

```bash
python network_policy.py
```

- Egress is default-deny — only the LLM host (`PI_LLM_HOST`) is reachable.
- CubeEgress attaches the provider key as an HTTP header on the wire
  (`x-api-key` for Anthropic, `Authorization: Bearer` otherwise), so
  `printenv` inside the sandbox never shows the real key — it only sees a
  placeholder.
- Because Pi runs on Node.js (which ignores the system CA store), the script
  sets `NODE_EXTRA_CA_CERTS` so Pi trusts the CubeEgress interception CA;
  without it the vault path fails with `Connection error`. Override the bundle
  path via `PI_NODE_EXTRA_CA_CERTS` if your image keeps the CA elsewhere.
- Any other destination returns `403 Forbidden - CubeEgress`.

If the agent needs extra hosts (package registries, MCP servers), add matching
allow rules or preinstall those dependencies into the template.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `pi: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Auth error from the provider | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| `Connection error` / TLS failure from Pi on the vault path | Pi runs on Node, which ignores the system CA store and won't trust the CubeEgress interception CA | The script sets `NODE_EXTRA_CA_CERTS` to the system bundle; override with `PI_NODE_EXTRA_CA_CERTS` if your CA lives elsewhere |
| Readiness probe timeout | Image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| Warmup `/readyz` never succeeds | `PI_PROVIDER` / `PI_MODEL` does not identify a model bundled with the pinned Pi version | Check the adapter logs and use a supported provider/model pair |
| Warmup task returns HTTP 409 | The resident session is already processing a task | Wait for the current task to finish; one adapter owns one session |
| `pause()`/`connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |

## References

- Integration guide: [`docs/guide/integrations/pi-agent.md`](../../docs/guide/integrations/pi-agent.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Network / egress policy examples: [`examples/network-policy`](../network-policy)
- Pi coding agent: <https://www.npmjs.com/package/@earendil-works/pi-coding-agent>
