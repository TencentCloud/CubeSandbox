# cube-sandbox RL SWE-bench Example

[中文文档](README_zh.md)

Automate SWE-bench coding tasks in isolated sandboxes using cube-sandbox (E2B compatible) + mini-swe-agent.

> For the detailed requirements document, see [docs/PRD.md](docs/PRD.md)

## Prerequisites

- Python 3.10+
- Docker (for building envd-injected images)
- cube-sandbox platform access (API URL + API Key)
- At least one LLM API Key (Gemini / TokenHub / OpenAI, etc.)

## Quick Start

### Step 1: Install Dependencies

```bash
pip install -r requirements.txt
```

### Step 2: Install mini-swe-agent E2B Patch

mini-swe-agent does not support E2B environments by default. You need to install the patch:

```bash
bash mini-swe-agent-patch/install.sh
```

The patch overwrites 3 files into mini-swe-agent's site-packages to add E2B environment support. See [mini-swe-agent-patch/README.md](mini-swe-agent-patch/README.md) for details.

### Step 3: Configure Environment Variables

```bash
cp .env.example .env
```

Edit `.env` and fill in the actual values:

```bash
# HuggingFace mirror (use hf-mirror.com in China to avoid timeouts)
HF_ENDPOINT="https://hf-mirror.com"

# cube-sandbox platform connection info
E2B_API_URL="http://<your-cube-sandbox-ip>:3000"
E2B_API_KEY="<your-api-key>"

# SWE-bench image template ID (obtained in Step 4)
CUBE_TEMPLATE_ID="<your-template-id>"

# SSL certificate (required for self-hosted platforms, only applies to E2B SDK connections)
CUBE_SSL_CERT_FILE="/etc/pki/tls/cert.pem"

# LLM API Key (choose one or more)
GEMINI_API_KEY="<your-gemini-key>"
TOKENHUB_API_KEY="<your-tokenhub-key>"
OPENAI_API_KEY="<your-openai-key>"
```

### Step 4: Prepare the SWE-bench Image Template

We provide a pre-built public image (with envd already injected) that can be used directly:

```
cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest
```

> This image is publicly accessible and does not require manual building.

Use `cubemastercli` to create a sandbox template from the image:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --cpu 4000 \
  --memory 8192 \
  --probe 49983
```

| Parameter | Description |
|-----------|-------------|
| `--image` | SWE-bench image address with envd injected |
| `--writable-layer-size` | Writable layer size (space for file modifications inside sandbox) |
| `--expose-port` | envd gRPC port (fixed at 49983) |
| `--cpu` | CPU quota (millicores, 4000 = 4 cores) |
| `--memory` | Memory quota (MB, 8192 = 8G) |
| `--probe` | Health check port (same as expose-port) |

Add the output `template_id` to `.env` as `CUBE_TEMPLATE_ID`.

<details>
<summary>Custom Image: Inject envd into an Original SWE-bench Image</summary>

If you need to use images for other SWE-bench problems, you can inject envd via the script:

```bash
bash scripts/inject-envd.sh swebench/sweb.eval.x86_64.django_1776_django-13447:latest
```

The script will build a new image with envd. After pushing it to the image registry, use the `cubemastercli` command above to create a template.

</details>

### Step 5: Run the Evaluation

Solve django__django-13447 using Gemini 3 Flash:

```bash
bash scripts/run-swebench.sh \
  --model gemini/gemini-3-flash-preview \
  --instance django__django-13447 \
  --config configs/e2b-swebench.yaml
```

Results (trajectory + patch) are saved in the `results/` directory.

## Multi-Model Switching

### Gemini Models (Direct Connection)

```bash
# Set GEMINI_API_KEY
bash scripts/run-swebench.sh \
  --model gemini/gemini-3-pro-preview \
  --config configs/e2b-swebench.yaml \
  --instance django__django-13447
```

### TokenHub Models

```bash
# All TokenHub models require setting OPENAI_API_KEY
export OPENAI_API_KEY=$TOKENHUB_API_KEY

# GLM-5
bash scripts/run-swebench.sh \
  --model openai/glm-5 \
  --config configs/e2b-tokenhub.yaml \
  --instance django__django-13447

# MiniMax M2.7
bash scripts/run-swebench.sh \
  --model openai/minimax-m2.7 \
  --config configs/e2b-tokenhub.yaml \
  --instance django__django-13447

# DeepSeek V3.2 (via TokenHub)
bash scripts/run-swebench.sh \
  --model openai/deepseek-v3.2 \
  --config configs/e2b-tokenhub.yaml \
  --instance django__django-13447
```

### TokenHub Thinking Models (Thinking Must Be Disabled)

```bash
export OPENAI_API_KEY=$TOKENHUB_API_KEY

# Kimi K2.5
bash scripts/run-swebench.sh \
  --model openai/kimi-k2.5 \
  --config configs/e2b-kimi.yaml \
  --instance django__django-13447

# DeepSeek R1-0528
bash scripts/run-swebench.sh \
  --model openai/deepseek-r1-0528 \
  --config configs/e2b-kimi.yaml \
  --instance django__django-13447

# Hunyuan 2.0 Thinking
bash scripts/run-swebench.sh \
  --model openai/hunyuan-2.0-thinking-20251109 \
  --config configs/e2b-kimi.yaml \
  --instance django__django-13447
```

### DeepSeek Direct Connection

```bash
# Set DEEPSEEK_API_KEY

# DeepSeek Chat
bash scripts/run-swebench.sh \
  --model deepseek/deepseek-chat \
  --config configs/e2b-deepseek.yaml \
  --instance django__django-13447

# DeepSeek Reasoner (Thinking Must Be Disabled)
bash scripts/run-swebench.sh \
  --model deepseek/deepseek-reasoner \
  --config configs/e2b-deepseek-reasoner.yaml \
  --instance django__django-13447
```

### MiniMax Direct Connection

Connect directly to the [MiniMax API](https://platform.minimax.io/docs/api-reference/text-openai-api) without going through TokenHub. MiniMax exposes an OpenAI-compatible endpoint, so set `OPENAI_API_KEY` to your MiniMax key — `configs/e2b-minimax.yaml` already points `api_base` at `https://api.minimax.io/v1`.

```bash
# Set OPENAI_API_KEY=<your-minimax-key> in .env (or export it)

# MiniMax-M2.7 (peak performance)
bash scripts/run-swebench.sh \
  --model openai/MiniMax-M2.7 \
  --config configs/e2b-minimax.yaml \
  --instance django__django-13447

# MiniMax-M2.7-highspeed (faster, same performance)
bash scripts/run-swebench.sh \
  --model openai/MiniMax-M2.7-highspeed \
  --config configs/e2b-minimax.yaml \
  --instance django__django-13447
```

## Concurrent Evaluation

Use `scripts/run-concurrent.py` for multi-model, multi-instance concurrent evaluation with sandbox pre-creation and TUI real-time monitoring.

### Single Model Concurrency

```bash
# GLM-5-Turbo with 10 concurrent sandboxes on the same problem
export OPENAI_API_KEY=$TOKENHUB_API_KEY

python scripts/run-concurrent.py swebench \
  -m openai/glm-5-turbo \
  --instances django__django-13447 \
  --repeat 10 \
  --pre-create --pre-create-workers 10 \
  -w 10
```

### Multi-Model Concurrency

```bash
# 5 models x 2 repeats = 10 tasks running simultaneously
python scripts/run-concurrent.py swebench \
  -m openai/glm-5,openai/glm-5-turbo,openai/kimi-k2.5,openai/deepseek-v3.2,openai/minimax-m2.7 \
  --instances django__django-13447 \
  --repeat 2 \
  --pre-create --pre-create-workers 10 \
  -w 10
```

### All TokenHub Models Concurrency

```bash
# "tokenhub" keyword expands to all 7 models, 20 repeats each = 140 tasks
python scripts/run-concurrent.py swebench \
  -m tokenhub \
  --instances django__django-13447 \
  --repeat 20 \
  --pre-create --pre-create-workers 50 \
  -w 140 \
  --max-rows 0
```

### Sandbox-Only Stress Test

```bash
# Skip LLM calls, only stress test sandbox creation/destruction
python scripts/run-concurrent.py swebench \
  -m tokenhub \
  --instances django__django-13447 \
  --repeat 15 \
  --pre-create --pre-create-workers 50 \
  -w 105 \
  --sandbox-only --max-rows 0
```

### Common Parameters

| Parameter | Description |
|-----------|-------------|
| `-m` | Model name (comma-separated for multiple, or `tokenhub` for all TokenHub models) |
| `--repeat N` | Repeat N times per model |
| `--pre-create` | Multi-process sandbox pre-creation, tasks connect directly on start |
| `--pre-create-workers N` | Pre-creation concurrency |
| `-w N` | Task execution concurrency |
| `--step-limit N` | Limit Agent maximum steps |
| `--sandbox-only` | Skip LLM, sandbox-only stress test |
| `--max-rows N` | TUI display rows (`0` to show all) |

> For detailed parameter descriptions, see [scripts/README.md](scripts/README.md)

## SSL Certificate Configuration (Self-Hosted Platforms)

If cube-sandbox uses mkcert certificates, you need to install the root CA:

```bash
# Retrieve root CA from cube-sandbox node
ssh <cube-node> 'cat $(mkcert -CAROOT)/rootCA.pem' \
  > /etc/pki/ca-trust/source/anchors/cube-rootCA.pem

# Install to system trust chain
sudo update-ca-trust

# Set in .env (note: use CUBE_SSL_CERT_FILE, not SSL_CERT_FILE)
CUBE_SSL_CERT_FILE="/etc/pki/tls/cert.pem"
```

> **Note**: Use `CUBE_SSL_CERT_FILE` instead of `SSL_CERT_FILE` to avoid globally overriding Python's CA certificate bundle, which would cause SSL verification failures when accessing public sites like HuggingFace. The script will automatically set it as `SSL_CERT_FILE` when connecting to cube-sandbox.

## Directory Structure

```
cube-sandbox-rl-example/
├── README.md                        # English documentation (this file)
├── README_zh.md                     # Chinese documentation
├── docs/
│   └── PRD.md                       # Product requirements document (with RL vision)
├── mini-swe-agent-patch/            # mini-swe-agent E2B adaptation code
│   ├── README.md                    # Patch description
│   ├── install.sh                   # One-click install script
│   ├── environments/
│   │   ├── __init__.py              # Register e2b environment type
│   │   └── extra/
│   │       └── e2b.py               # E2BEnvironment implementation
│   └── run/benchmarks/
│       └── swebench.py              # SWE-bench entry point (with e2b support)
├── configs/
│   ├── e2b-swebench.yaml            # Base config (Gemini and other direct models)
│   ├── e2b-tokenhub.yaml            # TokenHub model config
│   ├── e2b-kimi.yaml                # Kimi K2.5 specific config
│   ├── e2b-deepseek.yaml            # DeepSeek Chat config
│   ├── e2b-deepseek-reasoner.yaml   # DeepSeek Reasoner config
│   └── e2b-minimax.yaml             # MiniMax direct API config
├── scripts/
│   ├── setup-env.sh                 # One-click environment setup
│   ├── inject-envd.sh               # Inject envd into SWE-bench image
│   ├── run-swebench.sh              # Run single SWE-bench evaluation
│   ├── run-concurrent.py            # Concurrent evaluation (multi-model/multi-instance/pre-creation)
│   ├── run-all-tokenhub.sh          # Run all TokenHub models in parallel
│   └── check-instances.sh           # Check/clean sandbox instances
├── envd-inject/
│   └── Dockerfile                   # envd injection Dockerfile
├── .env.example                     # Environment variable template
└── requirements.txt                 # Python dependencies
```

## Verified Model Performance

| Model | Steps | Cost | Duration | Result |
|-------|-------|------|----------|--------|
| DeepSeek Chat | 35 | $0.02 | 253s | Passed |
| DeepSeek Reasoner | 75 | $0.05 | 389s | Passed |
| DeepSeek V3.2 | - | - | - | Pending |
| DeepSeek R1-0528 | - | - | - | Pending |
| Kimi K2.5 | 42 | $0.93 | 179s | Passed |
| Gemini 3 Pro | 22 | $0.26 | 224s | Passed |
| Gemini 3 Flash | 46 | $0.19 | 278s | Passed |
| MiniMax M2.7 | 56 | $1.62 | 363s | Passed |
| GLM-5 | 41 | $0.75 | 400s | Passed |
| Hunyuan 2.0 Thinking | - | - | - | Pending |

> Test case: django__django-13447, Environment: cube-sandbox E2B sandbox
