# Agent Tool Allowlist Sandbox

[中文文档](README_zh.md)

Host-side argv tool allowlist + a small BYOI toolbox image for
[#645](https://github.com/TencentCloud/CubeSandbox/issues/645).

**What you get:** refuse illegal tool commands on the host *before*
`Sandbox.create` / `commands.run`, then run allowlisted tools inside a
MicroVM from this directory's Dockerfile. Stacks with airgap, CIDR
`allow_out`, pause/resume, and parallel sandboxes.

**What this is not:** guest confinement, bash-free images, or an LLM agent.
`tool_agent_loop.py` uses fixed proposals.

## Use cases

- Agent hosts that should never start a sandbox for `bash` / `curl` probes
- Teaching host-policy vs sandbox-workload (E2B-style) on Cube
- Minimal BYOI profile (`/etc/cube-sandbox/tool-profile.txt`) aligned with
  the default allowlist
- Differentiated stacks: checkpoint, restricted egress, multi-sandbox fan-out

## Prerequisites

- Running Cube Sandbox cluster + `cubemastercli`
- Docker (to build the image)
- Python 3.10+

```bash
pip install -r requirements.txt
cp .env.example .env
```

## Quick start

### 1 — Build & register

```bash
docker build -t agent-tool-allowlist-sandbox:latest .
# optional: --build-arg INSTALL_CURL=1  (airgap curl probe in egress/loop)

cubemastercli tpl create-from-image \
  --image <registry-or-local>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Put the READY template id into `.env` as `CUBE_TEMPLATE_ID`. Resources: 1G
writable layer; no GPU.

Local envd probe:

```bash
docker run --rm -d --name agent-tool-box -p 49983:49983 agent-tool-allowlist-sandbox:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f agent-tool-box
```

### 2 — Run demos

| Step | Command | Expect | Needs cluster |
|------|---------|--------|---------------|
| Limits | `python tool_allowlist_limits.py` | `LIMITS_DEMO_OK` | no |
| Unit tests | `python -m unittest test_tool_allowlist.py -v` | OK | no |
| Deny | `python tool_allowlist_deny.py` | `denied_as_expected` | no |
| Template smoke | `python verify_template.py` | `TEMPLATE_VERIFY_OK` | yes |
| Allow + airgap | `python tool_allowlist_allow.py` | echo + artifact | yes |
| Reference loop | `python tool_agent_loop.py` | `AGENT_LOOP_OK` | yes |
| Checkpoint | `python tool_allowlist_checkpoint.py` | `CHECKPOINT_OK` | yes |
| Egress stack | `python tool_allowlist_egress.py` | `EGRESS_STACK_OK` | yes |
| Fan-out | `python tool_allowlist_fanout.py` | `FANOUT_OK` | yes |

`ALLOWLIST_FANOUT_N` defaults to `2` (max `4`).

## How it works

```
propose command
    │
    ▼
tool_allowlist.assert_allowlisted   ← host only
    │ deny → never Sandbox.create / commands.run
    ▼ allow
Sandbox.create(this BYOI, network/lifecycle options…)
    │
    ▼
sandbox.commands.run(command)
```

Guest `tool-profile.txt` documents intended toolbox names; enforcement stays
on the host.

## Directory

```
├── Dockerfile
├── tool_allowlist.py
├── tool_allowlist_limits.py / deny.py / allow.py
├── tool_allowlist_checkpoint.py   # pause/resume + gate
├── tool_allowlist_egress.py       # argv ⊥ CIDR allow_out
├── tool_allowlist_fanout.py       # parallel sandboxes
├── tool_agent_loop.py
├── test_tool_allowlist.py
├── verify_template.py
├── env_utils.py
├── requirements.txt
└── .env.example
```

## Limits

- Base image still has a shell; profile ≠ confinement.
- Allowlisting `cat` still permits `cat /etc/passwd` through this gate.
- `echo … > file` remains allowlisted (arbitrary guest write residual).
- Growing the allowlist needs `extra_binaries` **and**
  `allow_unsafe_allowlist_extension=True`.
- `enable_code_execution=True` is an explicit interpreter escalation.
- Default build does not apt-install curl; base may still ship it.
- `create-from-image` needs an image reference the node can pull.
- Fan-out creates real VMs — keep `N` small on shared clusters.
