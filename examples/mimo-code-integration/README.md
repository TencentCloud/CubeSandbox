# MiMo Code + CubeSandbox Example

[中文文档](README_zh.md)

Run [MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code), an OpenCode-derived
terminal coding agent with persistent memory, checkpoints, subagents, and
Compose workflows, inside a CubeSandbox MicroVM.

This example demonstrates:

- a pinned MiMo Code template image;
- one-shot headless execution with NDJSON output;
- exact conversation continuation after `pause()` / `Sandbox.connect()`;
- default-deny CubeEgress with on-the-wire MiMo Platform key injection;
- optional MiMo Compose mode without adding a separate orchestration layer.

## Directory layout

```text
mimo-code-integration/
├── Dockerfile
├── build-template.sh
├── .env.example
├── .gitignore
├── requirements.txt
├── env_utils.py
├── _mimo_common.py
├── run_mimo_code.py
├── resume_mimo_code.py
├── network_policy.py
├── tests/
├── README.md
└── README_zh.md
```

## Prerequisites

- A running CubeSandbox deployment with CubeAPI reachable at
  `http://<cube-host>:3000`.
- `cubemastercli`, Docker, and a registry reachable by Cube nodes.
- Python 3.10+ on the host.
- A MiMo Platform API key from <https://platform.xiaomimimo.com>.
- CubeSandbox platform `>= 0.3.0` for pause/resume and `>= 0.4.0` for
  CubeEgress credential injection.

The initial example intentionally targets MiMo Platform only. It does not infer
providers or authentication schemes from arbitrary URLs.

## 1. Build and register the template

The convenience script builds for `linux/amd64`, pushes the image, and submits
the template import:

```bash
export MIMO_IMAGE="<your-registry>/mimo-code-cube:0.1.7"
./examples/mimo-code-integration/build-template.sh
cubemastercli tpl watch --job-id <job_id>
```

Equivalent manual commands:

```bash
docker build --platform linux/amd64 \
  --build-arg MIMO_VERSION=0.1.7 \
  -t <your-registry>/mimo-code-cube:0.1.7 \
  examples/mimo-code-integration
docker push <your-registry>/mimo-code-cube:0.1.7

cubemastercli tpl create-from-image \
  --image <your-registry>/mimo-code-cube:0.1.7 \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

The image pins `@mimo-ai/cli@0.1.7`, verifies `mimo --version`, and inherits
CubeSandbox's `envd` entrypoint.

## 2. Configure the host drivers

```bash
cd examples/mimo-code-integration
install -m 600 .env.example .env
# Set E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID, and MIMO_API_KEY.
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

| Variable | Purpose |
| --- | --- |
| `E2B_API_URL` / `E2B_API_KEY` | CubeAPI connection |
| `CUBE_TEMPLATE_ID` | READY template ID |
| `MIMO_API_KEY` | MiMo Platform credential |
| `MIMO_MODEL` | Defaults to `mimo/mimo-v2.5-pro` |
| `MIMOCODE_HOME` | Absolute profile root; defaults to `/root/.mimocode` |
| `MIMO_WORKSPACE` | Agent working directory; defaults to `/workspace` |
| `MIMO_SANDBOX_TIMEOUT` | Sandbox idle timeout; defaults to `1800` seconds |
| `MIMO_AGENT_EXEC_TIMEOUT` | MiMo command timeout; defaults to `900` seconds |
| `MIMO_NODE_EXTRA_CA_CERTS` | `network_policy.py` CA bundle; defaults to the system bundle |

`MIMOCODE_HOME` places `config/`, `data/`, `state/`, and `cache/` under one
profile root. MiMo's session database, persistent memory, and checkpoints
therefore move together with the CubeSandbox snapshot.

Use HTTPS for a remote, authentication-enabled CubeAPI. Plain HTTP is intended
only for a trusted local deployment where no real Cube API key crosses an
untrusted network.

### End-to-end preflight and evidence

Before a live run, confirm that `cubemastercli tpl list` reports the template
configured in `.env` as `READY`, CubeAPI reports healthy sandboxes, and the
host Python environment can import both SDKs:

```bash
cubemastercli tpl list
curl -fsS http://<cube-host>:3000/health
python -c 'import e2b, cubesandbox; print("SDK dependencies OK")'
```

For `network_policy.py`, also confirm that CubeEgress is running, its audit log
is writable at `/data/log/cube-egress/access.jsonl`, and
`MIMO_NODE_EXTRA_CA_CERTS` (when set) contains the CubeEgress CA. The template
ID belongs to one CubeSandbox cluster; import the image on each cluster and
set its newly created `READY` template ID in the local `.env`. Do not put an
environment-specific ID in `.env.example`.

Save redacted command output under `output/` (already ignored by Git), record
the image digest, template state, sandbox/session IDs, result markers, and
final `sandboxes: 0` health response. Do not save `.env`, real API keys,
registry credentials, or complete authorization headers. Capture screenshots
of the same redacted terminal or console evidence when preparing an issue or
pull request.

## 3. Run a one-shot task

```bash
python run_mimo_code.py
```

Custom prompts must create `result.md` with `CUBE_MIMO_RUN_OK`, or pass
`--skip-result-check` when the task has a different output contract.

The runner seeds a tiny Python project and invokes:

```bash
mimo run --format json --dir /workspace \
  --model mimo/mimo-v2.5-pro \
  --agent build \
  --dangerously-skip-permissions "<prompt>"
```

It parses MiMo's NDJSON events, prints tool and text events, captures the
`sessionID`, and verifies the generated `result.md`.

The direct runner passes `MIMO_API_KEY` only to the MiMo process through
`commands.run(..., envs=...)`. This is convenient for development, but an agent
with unrestricted egress could exfiltrate the key. Use the CubeEgress flow for
shared environments.

> `--dangerously-skip-permissions` auto-approves non-denied tools. Use it only
> in an isolated, disposable sandbox with no host mounts or unrelated secrets.

## 4. Pause and continue the same MiMo session

```bash
python resume_mimo_code.py
```

The script:

1. starts a MiMo turn and captures its `sessionID` from NDJSON;
2. asks MiMo to remember a random token without writing it to `/workspace`;
3. pauses the MicroVM and reconnects with `Sandbox.connect()`;
4. verifies `/workspace`, `$MIMOCODE_HOME/data`, and `mimo session list`;
5. runs the second turn with `mimo run --session <id>`;
6. verifies that MiMo recalls the token and continues the original task.

This checks agent conversation continuity, not only filesystem persistence.
The script deliberately avoids `with Sandbox.create(...)`: leaving the context
would kill the sandbox before it could be resumed.

MiMo checkpoints and CubeSandbox snapshots solve different problems. MiMo
checkpoints compact and reconstruct model context; CubeSandbox snapshots retain
the whole VM, memory, filesystem, workspace, database, and profile.

## 5. Restrict egress and keep the real key outside the VM

```bash
python network_policy.py
```

This is the recommended shared-cluster pattern:

- `allow_internet_access=False` denies all unmatched traffic.
- Only `api.xiaomimimo.com` is allowed.
- The VM sees `MIMO_API_KEY=cube-egress-managed-placeholder`.
- CubeEgress replaces the outbound `api-key` header with the real secret.
- `NODE_EXTRA_CA_CERTS` points MiMo's runtime to the CubeEgress CA bundle.
- `example.com` is blocked: CubeEgress returns `403` when it receives the
  request, while deployments that enforce default deny at L3 can return curl
  status `000`; the authenticated MiMo task must succeed in either case.

The inline `MIMOCODE_CONFIG_CONTENT` contains `{env:MIMO_API_KEY}`, never the
real key. Sharing, telemetry, update checks, model-manifest downloads, LSP
downloads, and external skills are disabled so the allowlist stays narrow.

## 6. Try MiMo Compose

Compose is MiMo's primary multi-agent mode. It is available through the same
runner:

```bash
python run_mimo_code.py --agent compose \
  --prompt "Inspect app.py, improve it, run it, and write result.md containing CUBE_MIMO_RUN_OK."
```

Compose delegation is model-driven, so exact subagent activity is not used as
the basic smoke-test assertion. The smoke test verifies the generated artifact
and fixed marker instead.

## Tests

```bash
python -m unittest discover -s tests -v
python -m py_compile *.py
bash -n build-template.sh
```

The offline tests cover config generation, secret omission, command quoting,
MiMo Platform header injection, chunked NDJSON, and session-ID parsing. A real
cluster and API credential are still required for the end-to-end flows.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `mimo: command not found` | Stale template | Rebuild the image and import a new template |
| Unsupported platform binary | Image architecture differs from the Cube node | Build with `--platform linux/amd64` or a matching supported platform |
| MiMo authentication error | Missing/invalid key or wrong header | Set `MIMO_API_KEY`; MiMo Platform requires `api-key`, not Bearer |
| `403 Forbidden - CubeEgress` | Host did not match the exact allow rule | Keep the endpoint on `api.xiaomimimo.com` and inspect the audit log |
| TLS/certificate error | MiMo runtime does not trust CubeEgress CA | Set `MIMO_NODE_EXTRA_CA_CERTS` to the CA-containing bundle |
| Requests to models.dev, telemetry, or updates fail | Expected under the narrow allowlist | Keep the provided disable switches; do not enable those services accidentally |
| Template stuck in `PULLING` | Registry is unreachable or private | Use a node-reachable registry and configure pull credentials |
| Readiness probe timeout | Image did not inherit Cube's entrypoint | Build from the pinned CubeSandbox base image |
| No `sessionID` in output | CLI version/output mode drift | Use the pinned version and `--format json` |
| Session missing after reconnect | Different profile/workspace or failed snapshot | Keep the same absolute `MIMOCODE_HOME` and inspect pause/connect errors |
| Command timeout | Model/tool task exceeded the default | Increase `MIMO_AGENT_EXEC_TIMEOUT` and sandbox lifetime |

## Security notes

- Session databases and persistent memory may contain prompts, source code,
  paths, and command output. Restrict snapshot access and kill unused sandboxes.
- OAuth stores access/refresh tokens in `auth.json`; it is intentionally not
  used here because snapshots would persist those tokens.
- Do not publish raw `mimo export` output without reviewing and redacting it.
- Do not add package registries, MCP servers, or arbitrary domains to the
  CubeEgress allowlist unless the task requires them.

## References

- [MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code)
- [MiMo Code CLI options](https://mimo.xiaomi.com/mimocode/cli-options)
- [MiMo Code sessions](https://mimo.xiaomi.com/mimocode/sessions)
- [CubeSandbox integration guide](../../docs/guide/integrations/mimo-code.md)
- [CubeSandbox lifecycle](../../docs/guide/lifecycle.md)
- [CubeEgress security proxy](../../docs/guide/security-proxy.md)
