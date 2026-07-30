# Agent tool allowlist — BYOI template + host gate

Buildable Cube **template** (this directory) paired with the host-side argv
**tool allowlist** demos in
[`../code-sandbox-quickstart/`](../code-sandbox-quickstart/).

| Layer | Where | Role |
|-------|--------|------|
| Guest intent | `Dockerfile` → `/etc/cube-sandbox/tool-profile.txt` | Declares the intended toolbox |
| Host policy | `../code-sandbox-quickstart/tool_allowlist.py` | Authoritative argv gate before `Sandbox.create` |
| Platform | `allow_internet_access=False` | Egress orthogonal to argv allow |

This is **not** a full agent framework and **not** an LLM loop. The quickstart
`tool_agent_loop.py` is a reference (hardcoded proposals) only.

## Use cases

- Agent hosts that should refuse illegal tools before paying for a MicroVM
- Defense-in-depth demos: host gate + documented guest profile + airgap
- Teaching the difference between argv policy and guest confinement

## Resources (suggestions)

- Writable layer: **1G** is enough for this toolbox profile
- Single sandbox, short `timeout` (60s) for demos
- No GPU / no large package installs

## Build

```bash
docker build -t agent-tool-allowlist-sandbox:latest .
# push to a registry your Cubelet nodes can pull
```

## Register template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Wait until the template is **READY**, then set `CUBE_TEMPLATE_ID`.

## Configure & run

```bash
pip install -r requirements.txt
cp .env.example .env   # fill E2B_API_URL / E2B_API_KEY / CUBE_TEMPLATE_ID

python verify_template.py
# → host_deny bash, then tool-profile + echo, TEMPLATE_VERIFY_OK
```

Host-only gate / threat model / unittest (no this image required):

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_limits.py
python -m unittest test_tool_allowlist.py -v
python tool_allowlist_deny.py
```

Against the registered template (same env):

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_allow.py
python tool_agent_loop.py   # reference loop, not an LLM agent
```

## Known limitations

- Base image still contains a shell; this Dockerfile does **not** prove
  bash-free confinement. Host allowlisting remains mandatory.
- `curl` is installed so airgap probes in the quickstart loop can run when
  temporarily allowlisted; default host gate still denies `curl`.
- Allowlisted `echo` + `>` can write arbitrary guest paths — out of scope for
  the argv gate (see quickstart threat model).
- Not a replacement for `sandbox-code` data-science / interpreter workloads.

[中文说明](README_zh.md)
