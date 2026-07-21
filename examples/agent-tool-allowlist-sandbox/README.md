# Agent Tool Allowlist Sandbox

[中文文档](README_zh.md)

Run **allowlisted agent tool commands** inside a Cube Sandbox MicroVM, return
stdout / small artifacts, and **fail fast** when the agent asks for a
non-allowlisted command.

This is **not** a general code interpreter, Jupyter runtime, or full agent host
(OpenClaw / OpenAI Agents). The gate lives on the **host** (SDK caller) so the
security narrative stays honest: the MicroVM still isolates execution, while the
example shows how an agent should refuse arbitrary tools before they are sent.

## 1. How it differs

**One-liner:** not another language runtime or full Agent host — a **host-side argv
allowlist gate** for agent tool calls, then MicroVM execution + artifact
readback; non-allowlisted calls **fail fast without creating a sandbox**.

### In-repo examples

| Example | Focus | Allowlist meaning |
|---------|-------|-------------------|
| [`code-sandbox-quickstart`](../code-sandbox-quickstart) | Arbitrary `run_code` / `commands.run` | None (any command) |
| [`openai-agents-code-interpreter`](../openai-agents-code-interpreter) | LLM + data analysis / code interpreter | N/A (interpreter workload) |
| [`openclaw-integration`](../openclaw-integration) / [`openai-agents-example`](../openai-agents-example) | Full agent hosting / orchestration | N/A |
| [`network-policy`](../network-policy) / `network_allowlist.py` | **Egress CIDR** allow/deny | Network destinations |
| **This example** | **Tool argv** allow/deny + artifact readback | First argv token (binary name) |

### Related [#645](https://github.com/TencentCloud/CubeSandbox/issues/645) PR themes (do not overlap)

| Theme (examples) | What those PRs add | What this example does instead |
|------------------|--------------------|--------------------------------|
| Language / web runtimes (e.g. Node #732, Go/Rust/Java #735/#755/#782, C++ #876, Ruby #926) | New language image + run that stack in a sandbox | Reuse official `sandbox-code`; no new language Dockerfile |
| Interpreters / notebooks (e.g. Jupyter ML #1025, RCA interpreter #745) | Arbitrary or analysis-oriented code execution | Tool-command subset only; deny non-allowlisted tools |
| Agent frameworks (e.g. LangGraph #710) | End-to-end agent orchestration | Gate pattern only — not a framework integration |
| Platform demos (egress / DB / snapshot, e.g. #748, #979, #1004, #738) | CIDR policy, stateful services, snapshot cache | **Argv** allowlist (not CIDR); keep the demo minimal |

Re-check open #645 PRs for titles containing `allowlist` / `tool-allowlist` before
opening a PR, in case a same-direction submission appears later.

## 2. Prerequisites

- A running Cube Sandbox deployment (see [dev environment](../../docs/zh/guide/dev-environment.md))
- Python 3.8+

```bash
pip install -r requirements.txt
```

## 3. Create the template

Reuse the official code image (same as the quickstart):

```bash
# Mainland China registry
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999

# International registry (if outside CN)
# cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
```

Note the printed `template_id`.

## 4. Configure environment

```bash
cp .env.example .env
# set E2B_API_URL and CUBE_TEMPLATE_ID
```

For the official QEMU `dev-env`, the host-forwarded API is typically
`http://127.0.0.1:13000`. Full E2B command traffic needs `*.cube.app` DNS
(CoreDNS inside the guest). Prefer running these scripts **inside the
dev VM**, or point a wildcard resolver at the CubeProxy forward ports
(`11080`/`11443` on the host).

## 5. Run

### Allowlisted tool (success)

```bash
python run_allowlisted.py
```

Expected:

```text
agent-tool-allowlist-ok
artifact: artifact-ok
```

### Non-allowlisted tool (must fail on host)

```bash
python run_denied.py
```

Expected:

```text
denied_as_expected: command not on tool allowlist: 'bash' ...
```

No sandbox is created on the deny path.

## 6. Default allowlist

See `allowlist.py` (`DEFAULT_ALLOWED_BINARIES`). The demo allows a small set of
read-only / reporting tools (`echo`, `uname`, `ls`, `cat`, `python3`, …) and
rejects path-style binaries (`/bin/bash`) and shells like `bash` / `curl`.

Tighten or extend the set for your agent; keep it explicit in code reviews.

## 7. Limitations (read before production)

- Host-side gate only — a caller that bypasses `assert_allowlisted` can still
  send any command to the API. Pair with network policy and least-privilege
  credentials in real agents.
- Not a substitute for guest-side seccomp / AppArmor.
- Does not implement unauthorized network scanning or exploit tooling.

## 8. Directory

```text
agent-tool-allowlist-sandbox/
├── README.md
├── README_zh.md
├── allowlist.py           # host-side argv gate
├── run_allowlisted.py     # allow path + artifact readback
├── run_denied.py          # deny path (no sandbox)
├── env_utils.py
├── requirements.txt
└── .env.example
```
