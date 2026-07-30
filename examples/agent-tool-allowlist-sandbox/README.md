# Agent Tool Allowlist Sandbox

[中文文档](README_zh.md)

Host-side argv tool allowlist + a small BYOI toolbox image for
[#645](https://github.com/TencentCloud/CubeSandbox/issues/645).

**What you get:** refuse illegal tool commands on the host *before*
`Sandbox.create` / `commands.run`, then run allowlisted tools inside a
MicroVM built from this directory's Dockerfile. Stack with
`allow_internet_access=False`.

**What this is not:** guest confinement, bash-free images, or an LLM agent.
`tool_agent_loop.py` uses fixed proposals.

## Use cases

- Agent hosts that should never start a sandbox for `bash` / `curl` probes
- Teaching the host-policy vs sandbox-workload split (E2B-style)
- A minimal BYOI profile file (`/etc/cube-sandbox/tool-profile.txt`) aligned
  with the default allowlist

## Prerequisites

- Running Cube Sandbox cluster + `cubemastercli`
- Docker (to build the image)
- Python 3.10+

```bash
pip install -r requirements.txt
cp .env.example .env
```

## Quick start

### 1 — Build image

```bash
docker build -t agent-tool-allowlist-sandbox:latest .

# optional guest curl for the airgap turn in tool_agent_loop.py
# docker build --build-arg INSTALL_CURL=1 -t agent-tool-allowlist-sandbox:latest .
```

Local envd probe (same idea as `cubesandbox-base-nginx`):

```bash
docker run --rm -d --name agent-tool-box \
  -p 49983:49983 agent-tool-allowlist-sandbox:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f agent-tool-box
```

### 2 — Register template

Push/tag so the node can pull, then:

```bash
cubemastercli tpl create-from-image \
  --image <registry-or-local>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Put the READY template id into `.env` as `CUBE_TEMPLATE_ID`.
Resources: 1G writable layer is enough; no GPU.

### 3 — Host-only checks (no cluster)

```bash
python tool_allowlist_limits.py
python -m unittest test_tool_allowlist.py -v
python tool_allowlist_deny.py
```

### 4 — Cluster demos

```bash
python verify_template.py          # TEMPLATE_VERIFY_OK
python tool_allowlist_allow.py     # allowlisted echo + artifact under airgap
python tool_agent_loop.py          # AGENT_LOOP_OK (fixed proposals)
```

## How it works

```
propose command
    │
    ▼
tool_allowlist.assert_allowlisted   ← host only
    │ deny → never Sandbox.create / commands.run
    ▼ allow
Sandbox.create(template=this BYOI, allow_internet_access=False)
    │
    ▼
sandbox.commands.run(command)
```

Guest image ships `/etc/cube-sandbox/tool-profile.txt` listing the default
toolbox names. The gate itself still lives on the host; the file is intent /
documentation inside the VM, not enforcement.

## Directory

```
├── Dockerfile                 # cubesandbox-base + tool-profile
├── tool_allowlist.py          # host argv gate
├── tool_allowlist_limits.py   # threat model (host-only)
├── tool_allowlist_deny.py     # deny before create
├── tool_allowlist_allow.py    # allow + airgap
├── tool_agent_loop.py         # reference loop (not an LLM)
├── test_tool_allowlist.py     # unittest
├── verify_template.py         # cluster smoke for this image
├── env_utils.py
├── requirements.txt
└── .env.example
```

## Limits

- Base image still has a shell; profile ≠ confinement.
- Allowlisting `cat` still permits `cat /etc/passwd` through this gate —
  rely on MicroVM isolation + least privilege.
- `echo … > file` remains allowlisted (arbitrary guest write residual).
- Growing the allowlist needs `extra_binaries` **and**
  `allow_unsafe_allowlist_extension=True`.
- `enable_code_execution=True` is an explicit interpreter escalation.
- Default build does not apt-install curl; base may still ship it.
- `create-from-image` needs an image reference the node can pull.
