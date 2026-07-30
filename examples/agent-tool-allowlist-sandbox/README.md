# Agent Tool Allowlist Sandbox

[中文文档](README_zh.md)

Host argv tool allowlist + a BYOI image that ships a real **guest tool runner**
(`cube-tool`) for [#645](https://github.com/TencentCloud/CubeSandbox/issues/645).

**What you get**
- Host refuses illegal tools before `Sandbox.create` / `commands.run`
- Image installs `/usr/local/bin/cube-tool` + `/etc/cube-sandbox/tool-profile.txt`
  + `/workspace` — guest re-checks the profile (not just a text file on disk)
- Stacks with airgap, CIDR `allow_out`, pause/resume, and parallel sandboxes

**What this is not:** kernel jail, bash-free base, or an LLM agent.

## Use cases

- Agent hosts that should prefer `cube-tool <name> …` over raw `bash`/`curl`
- Defense-in-depth: host allowlists `cube-tool`; guest refuses off-profile names
- Differentiated stacks: checkpoint / egress / multi-sandbox fan-out

## Prerequisites

- Cube Sandbox cluster + `cubemastercli` + Docker + Python 3.10+

```bash
pip install -r requirements.txt
cp .env.example .env
```

## Quick start

### 1 — Build & register

```bash
docker build -t agent-tool-allowlist-sandbox:latest .
# optional: --build-arg INSTALL_CURL=1

# local image smoke (no cluster)
docker run --rm agent-tool-allowlist-sandbox:latest \
  cube-tool echo build-ok

cubemastercli tpl create-from-image \
  --image <registry-or-local>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

`--probe 49983 --probe-path /health` targets **envd** from `cubesandbox-base`
(inherited entrypoint); this Dockerfile only `EXPOSE`s that port and does not
add its own health server.

Put READY template id into `.env` as `CUBE_TEMPLATE_ID`.

### 2 — Run demos

| Step | Command | Expect | Cluster |
|------|---------|--------|---------|
| Limits | `python tool_allowlist_limits.py` | `LIMITS_DEMO_OK` | no |
| Unit tests | `python -m unittest test_tool_allowlist.py -v` | OK | no |
| Deny | `python tool_allowlist_deny.py` | denied | no |
| Template smoke | `python verify_template.py` | `TEMPLATE_VERIFY_OK` | yes |
| Guest runner | `python tool_allowlist_guest_runner.py` | `GUEST_RUNNER_OK` | yes |
| Allow + airgap | `python tool_allowlist_allow.py` | echo + artifact | yes |
| Loop | `python tool_agent_loop.py` | `AGENT_LOOP_OK` | yes |
| Checkpoint | `python tool_allowlist_checkpoint.py` | `CHECKPOINT_OK` | yes |
| Egress stack | `python tool_allowlist_egress.py` | `EGRESS_STACK_OK` | yes |
| Fan-out | `python tool_allowlist_fanout.py` | `FANOUT_OK` | yes |

## How it works

```
propose: cube-tool echo hi
    │
    ▼
host assert_allowlisted   (argv0 must be allowlisted; prefer cube-tool)
    │
    ▼
Sandbox.create(this BYOI)
    │
    ▼
/usr/local/bin/cube-tool  → checks tool-profile.txt → exec echo
```

Bare allowlisted tools (`echo`, `cat`, …) still pass the host gate for demos;
prefer `cube-tool` so the guest profile is enforced.

## Directory

```
├── Dockerfile                 # installs cube-tool + profile + /workspace
├── tool-profile.txt           # guest allowlist (copied into the image)
├── guest/cube-tool            # in-guest runner
├── workspace/                 # default WORKDIR in the image
├── tool_allowlist.py          # host argv gate
├── tool_allowlist_*.py        # demos
├── verify_template.py
└── …
```

## Limits

- Base image still has a shell; callers that bypass `cube-tool` are out of scope
  for the guest wrapper.
- Allowlisting bare `cat` still permits `cat /etc/passwd` through the host gate.
- Documented residuals (host gate is not a shell): `echo … > file` (guest write),
  `cat < /etc/passwd` (input redirect), and glob chars `*` / `?` (guest shell
  may expand them before the binary runs) — not treated as chaining metas here.
- Growing the allowlist needs `extra_binaries` + `allow_unsafe_allowlist_extension=True`.
- Default build does not apt-install curl.
- Fan-out creates real VMs — keep `N` small.
