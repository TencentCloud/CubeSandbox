# CodeBuddy CLI × CubeSandbox Example

[中文文档](README_zh.md)

This directory provides an integration example for running [CodeBuddy CLI](https://www.codebuddy.ai/docs/cli/overview), Tencent Cloud's AI coding assistant, inside a CubeSandbox MicroVM. CodeBuddy is installed into a sandbox-code base image, and the `e2b-code-interpreter` Python SDK is used to create sandboxes and execute CodeBuddy in three modes: native sandbox, in-sandbox, and HTTP API.

```
User / CI Pipeline
    │
    ▼
e2b-code-interpreter SDK (Python)
    │  REST API
    ▼
CubeAPI (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           envd (PID 1)
                               │
                           codebuddy CLI
                               │
                           LLM API (egress)
```

## Prerequisites

- **Python 3.10+** — the `e2b-code-interpreter` SDK requires Python 3.10 or newer
- **CubeSandbox platform** — deployed and CubeAPI reachable (see the [CubeSandbox Quick Start](https://github.com/TencentCloud/CubeSandbox))
- **CodeBuddy API key** — generate at [https://www.codebuddy.ai/profile/keys](https://www.codebuddy.ai/profile/keys)
- **Docker** — required to build the sandbox image and register the template

## Quick Start

### 1. Build the Docker image and register the template (in-sandbox / HTTP API only)

```bash
./build_template.sh --registry <your-registry>
```

This will:
1. Build the Docker image with Node.js 22 + CodeBuddy CLI on top of the official `sandbox-code` base image
2. Push it to your container registry
3. Register it as a CubeSandbox template via `cubemastercli tpl create-from-image`

Copy the **template ID** printed at the end — you will need it for the `.env` file.

> **For native sandbox mode**, you can skip this step — no custom image build is required. Just use the default `sandbox-code` template.

### 2. Install Python dependencies

```bash
pip install -r requirements.txt
```

### 3. Configure environment variables

```bash
cp .env.example .env
```

Edit `.env` with your values:

| Variable | Description |
|----------|-------------|
| `CODEBUDDY_API_KEY` | CodeBuddy CLI API key |
| `E2B_API_URL` | CubeAPI URL, e.g. `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI auth key (any non-empty string) |
| `CUBE_TEMPLATE_ID` | Sandbox template ID from step 1 (not needed for native sandbox — uses default template) |
| `CUBE_SSL_CERT_FILE` | (Optional) Path to Cube CA bundle for HTTPS |

### 4. Run the demo

```bash
# Native sandbox mode (recommended — no image build, no auth token needed)
python demo.py --native-sandbox
python demo.py --native-sandbox --prompt "What files are in /workspace?"

# In-sandbox mode (runs inside the MicroVM, requires CODEBUDDY_AUTH_TOKEN for enterprise/CI accounts)
python demo.py
python demo.py --prompt "What files are in /workspace?"

# HTTP API mode
python demo.py --http-api
```

## Demo Modes

| Mode | Command | Description |
|------|---------|-------------|
| Native sandbox | `python demo.py --native-sandbox` | CodeBuddy runs on YOUR machine (already authenticated); tool calls routed to sandbox. This avoids the authentication issue inside the sandbox. |
| In-sandbox | `python demo.py` | Run `codebuddy -p` inside the MicroVM, parse JSON output |
| HTTP API | `python demo.py --http-api` | Start `codebuddy --serve`, call the REST chat API (same auth requirement as In-Sandbox mode) |

### Mode Comparison

| Mode | Recommendation | Verified | Needs AUTH_TOKEN? |
|------|---------------|----------|-------------------|
| Native sandbox | ✅ Recommended | ✅ Verified | No — just `CODEBUDDY_API_KEY` |
| In-sandbox | ⚠️ Enterprise/CI only | Infrastructure verified; conversation not verified | Needs `CODEBUDDY_AUTH_TOKEN` |

### Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--prompt` | `List the files in /workspace and describe what you see.` | Prompt sent to CodeBuddy (in-sandbox mode) |
| `--template` | `CUBE_TEMPLATE_ID` | Sandbox template ID |
| `--timeout` | `300` | Sandbox timeout in seconds |
| `--max-turns` | `10` | Maximum CodeBuddy tool-call turns |
| `--http-api` | — | Run HTTP API mode |
| `--native-sandbox` | — | Run native sandbox mode (CodeBuddy on host, tool calls in MicroVM) |

### Core Code

The key SDK calls used in this example:

```python
from e2b_code_interpreter import Sandbox
import os

# Create a sandbox from the CodeBuddy template
sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"])

# Run CodeBuddy in-sandbox mode
result = sb.commands.run('codebuddy -p "list files" --output-format json', user="root")
print(result.stdout)

# Start CodeBuddy in HTTP API mode
sb.commands.run('nohup codebuddy --serve --port 8080 > /tmp/codebuddy.log 2>&1 &', user="root")

# File operations
sb.files.write("/workspace/note.txt", "hello", user="root")
content = sb.files.read("/workspace/note.txt", user="root")

# Cleanup
sb.kill()
```

## Template Build

The [`Dockerfile`](Dockerfile) layers Node.js 22 and the CodeBuddy CLI on top of the official CubeSandbox `sandbox-code` base image:

```dockerfile
FROM cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

# Install Node.js 22 LTS via NodeSource
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get update && \
    apt-get install -y --no-install-recommends nodejs && \
    rm -rf /var/lib/apt/lists/*

# Install CodeBuddy CLI globally
RUN npm install -g @tencent-ai/codebuddy-code@latest

# Create workspace directory
RUN mkdir -p /workspace
WORKDIR /workspace
```

The [`build_template.sh`](build_template.sh) script automates the build, push, and registration:

```bash
./build_template.sh --registry <your-registry> [--image-name codebuddy-sandbox] [--tag latest]
```

The template exposes two ports:
- **49983** — envd agent (required by CubeSandbox for all templates)
- **8080** — CodeBuddy HTTP API server

### In-Sandbox Mode (⚠️ Enterprise/CI only)

CodeBuddy runs INSIDE the sandbox. The SDK integration, file upload,
script execution and codebuddy version check have been verified.
The actual codebuddy conversation inside the sandbox requires
CODEBUDDY_AUTH_TOKEN (currently available to enterprise/CI accounts only).
Without this credential, the conversation step could not be verified.

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `codebuddy: command not found` | Image build failed or Node.js not installed | Rebuild image, check `docker build` output |
| `CODEBUDDY_API_KEY not set` | Missing environment variable | Set `CODEBUDDY_API_KEY` in `.env` or pass via sandbox env |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS without CA certificate | Set `CUBE_SSL_CERT_FILE` to the Cube CA bundle path |
| `Template not found` | Wrong `CUBE_TEMPLATE_ID` | Run `cubemastercli tpl list` to verify |
| HTTP API not ready | Server startup is slow | Check `/tmp/codebuddy.log` inside the sandbox |
| LLM API timeout | Network egress blocked | Configure CubeEgress allowlist for the CodeBuddy API domain |

## Directory Structure

```
codebuddy-integration/
├── README.md              # English documentation (this file)
├── README_zh.md           # Chinese documentation
├── Dockerfile             # Image: sandbox-code + Node.js + codebuddy-code
├── build_template.sh      # Build image & register as CubeSandbox template
├── demo.py                # Integration demo (native sandbox + in-sandbox + HTTP API)
├── .env.example           # Environment variable template
└── requirements.txt       # Python dependencies
```

## Related Documents

- [CodeBuddy CLI × CubeSandbox Integration Guide](../../docs/guide/integrations/codebuddy.md)
- [CodeBuddy CLI Documentation](https://www.codebuddy.ai/docs/cli/overview)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
