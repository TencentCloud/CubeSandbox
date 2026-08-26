# Agent Tool Allowlist Sandbox

[中文文档](README_zh.md)

Bring-your-own-image **restricted toolbox** template for
[#645](https://github.com/TencentCloud/CubeSandbox/issues/645): a MicroVM that
ships `/usr/local/bin/cube-tool` plus a real guest workload (`toolbox-hello`).

Host scripts refuse non-allowlisted argv before `Sandbox.create`. Prefer
`cube-tool <name>` so the guest re-checks `/etc/cube-sandbox/tool-profile.txt`.

**What this is not:** kernel jail, bash-free base, language runtime, or an LLM agent.

## Prerequisites

- Cube Sandbox cluster + `cubemastercli` on `$PATH`
- Docker (build host) and a registry the Cube nodes can pull
- Python 3.10+ for the host driver

```bash
cd examples/agent-tool-allowlist-sandbox
pip install -r requirements.txt
cp .env.example .env
```

## 1. Build the template image

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/agent-tool-allowlist-sandbox:latest \
  .

docker push <your-registry>/agent-tool-allowlist-sandbox:latest
```

Local image smoke (no cluster):

```bash
docker run --rm <your-registry>/agent-tool-allowlist-sandbox:latest \
  cube-tool toolbox-hello
# expect WORKLOAD_OK
```

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health

cubemastercli tpl watch --job-id <job_id>
```

`--probe 49983 --probe-path /health` targets **envd** from `cubesandbox-base`
(inherited entrypoint). This Dockerfile only `EXPOSE`s that port.

Put the READY template id into `.env` as `CUBE_TEMPLATE_ID` (also set
`E2B_API_URL`).

## 3. Configure the host driver

```bash
# .env
E2B_API_URL=http://<node>:3000
CUBE_TEMPLATE_ID=tpl-...
```

## 4. Run (happy path)

```bash
python run.py
```

Expect: host deny for `bash` → MicroVM → `cube-tool toolbox-hello` prints
`WORKLOAD_OK` → artifact `/workspace/out/hello.txt` → guest deny for
`cube-tool bash` → **`RUN_OK`**.

`verify_template.py` is a thin alias of the same path.

## Resources

| Item | Suggestion |
|------|------------|
| Writable layer | `--writable-layer-size 1G` |
| Ports | expose/probe `49983` (envd) |
| CPU/mem | default template quotas enough for the hello workload |
| Fan-out (extras) | keep `N≤2` on shared nodes |

## Directory

```
├── Dockerfile              # installs cube-tool + toolbox-hello + profile
├── guest/cube-tool         # in-guest profile runner
├── guest/toolbox-hello     # perceptible workload binary
├── tool-profile.txt        # guest allowlist
├── workspace/              # default WORKDIR (+ out/)
├── run.py                  # happy path
├── tool_allowlist.py       # host argv gate
├── test_tool_allowlist.py  # host-only unit tests
└── extras/                 # optional stacks (checkpoint / egress / fan-out / …)
```

## Advanced (optional)

See [`extras/README.md`](extras/README.md). Examples:

```bash
python extras/tool_allowlist_limits.py    # LIMITS_DEMO_OK (host-only)
python extras/tool_allowlist_deny.py
python extras/tool_allowlist_checkpoint.py
```

## Limits

- Base image still has a shell; callers that bypass `cube-tool` are out of scope
  for the guest wrapper.
- Allowlisting bare `cat` still permits `cat /etc/passwd` through the host gate.
- Simple redirects / globs remain residuals; bash process substitution and
  `/dev/tcp` / `/dev/udp` are refused by the host gate.
- Growing the allowlist needs `extra_binaries` + `allow_unsafe_allowlist_extension=True`.
- `cubesandbox-base` already includes `curl`.
