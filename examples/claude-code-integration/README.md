# Claude Code + CubeSandbox Integration

[中文文档](./README_zh.md)

Run [Claude Code](https://docs.anthropic.com/en/docs/claude-code) — a terminal-based AI coding agent — inside **CubeSandbox** MicroVMs for isolated, reproducible, and secure development.

```
Host machine
    │  Python SDK (e2b-code-interpreter)
    ▼
CubeAPI (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           Claude Code CLI
                               │
                           npm / Node.js
```

## Key Features

| Feature | Description |
|---------|-------------|
| **Isolated execution** | Claude Code runs in a dedicated MicroVM — separate kernel, filesystem, and network |
| **E2B compatible** | Uses standard E2B SDK — works with any E2B-compatible client |
| **Snapshot persistence** | Pause/resume Claude Code sessions with full state preserved |
| **Secure key injection** | CubeEgress injects API keys on the wire — sandbox never sees real credentials |
| **Network policy** | Default-deny egress with fine-grained LLM API host allowlisting |

## Prerequisites

- Running CubeSandbox deployment
- Python 3.8+ with `e2b-code-interpreter`
- A CubeSandbox code template (see Step 1 below)

## Quick Start

### Step 1 — Build the Template

Build a sandbox image with Claude Code pre-installed:

```bash
# Build from Dockerfile
docker build -t claude-code-sandbox:v1 .

# Tag and push to your registry, then create template
cubemastercli tpl create-from-image \
  --image claude-code-sandbox:v1 \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --probe 49999
```

> Or use the pre-built `sandbox-code` image and install Claude Code at sandbox creation time (see Option B below).

### Step 2 — Configure Environment

```bash
cp .env.example .env
# Edit .env with your API key and template ID
```

### Step 3 — Run Claude Code

```bash
# Simple one-shot run (open egress)
python run_claude_code.py "Write a Python function that prints Fibonacci numbers"

# With secure network policy (recommended for production)
python network_policy.py "Explain what a Unix pipe is in one paragraph"

# Pause/resume for long-running sessions
python resume_claude_code.py "Create a simple web server in Python"
```

## Usage Modes

### Mode A: One-shot execution

Claude Code runs a single prompt and exits. Suitable for CI/CD pipelines, code generation, and batch processing.

```bash
python run_claude_code.py "Refactor the code in /workspace/src/main.py to use async/await"
```

### Mode B: Session persistence (pause/resume)

Start a Claude Code session, pause the sandbox when done, and resume later with full context preserved. Ideal for long-running development tasks.

```bash
# First session
python resume_claude_code.py "Set up a new Python project with tests"

# Output: Sandbox paused. Resume later with:
#   python resume_claude_code.py --resume-from <sandbox-id>

# Resume session
python resume_claude_code.py --resume-from <sandbox-id> "Add a new feature to the project"
```

### Mode C: Secure egress (recommended)

Sandbox has no internet access — only the LLM API host is allowed through CubeEgress, which injects real credentials on the wire.

```bash
python network_policy.py "Analyze the security of this code: ..."
```

## Directory Structure

```
claude-code-integration/
├── Dockerfile                  # Build sandbox image with Claude Code
├── run_claude_code.py          # One-shot execution
├── resume_claude_code.py       # Pause/resume session persistence
├── network_policy.py           # Default-deny egress + key injection
├── env_utils.py                # Environment & credential helpers
├── _common.py                  # Shared sandbox setup and command helpers
├── mcp_server.py               # Optional MCP tool server
├── sandbox_exec.py             # Standalone sandbox execution helper
├── tests/                      # Automated tests for helpers and MCP handling
├── requirements.txt            # Python dependencies
├── .env.example                # Configuration template
├── .gitignore
├── README.md                   # This file
└── README_zh.md                # Chinese documentation
```

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `claude: command not found` | Claude Code not installed in template | Rebuild Dockerfile or install in sandbox init |
| `ANTHROPIC_AUTH_TOKEN not set` | Missing `.env` config | Run `cp .env.example .env` and fill in your key |
| SSL certificate error | CubeProxy HTTPS without CA cert | Set `SSL_CERT_FILE` or `NODE_EXTRA_CA_CERTS` |
| Connection timeout to LLM API | Egress policy blocks API host | Verify `resolve_llm_host()` returns correct hostname |
| `Template not found` | Wrong template ID | Check `cubemastercli tpl list` |
