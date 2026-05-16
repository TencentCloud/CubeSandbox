# LlamaIndex + CubeSandbox Integration Example

## Overview

This example demonstrates how to integrate Cube Sandbox as a secure code execution backend for LlamaIndex agents in RAG (Retrieval-Augmented Generation) workflows.

## Features

- **MicroVM-level isolation**: Code runs in a fully isolated virtual machine
- **Network security policies**: Configurable network access control to prevent data exfiltration
- **RAG data processing**: Safely process external data sources
- **Stateless/Stateful**: Supports single executions and multi-turn conversations

## Installation

```bash
pip install -r requirements.txt
```

## Configuration

1. Copy the environment file:
   ```bash
   cp .env.example .env
   ```

2. Edit `.env` with your configuration:
   ```env
   # Cube Sandbox API URL
   E2B_API_URL=http://127.0.0.1:3000

   # Cube Sandbox Template ID (required)
   CUBE_TEMPLATE_ID=<your-template-id>

   # OpenAI API Key for LLM
   OPENAI_API_KEY=sk-...
   ```

## Quick Start

```python
from llamaindex_integration import create_cube_tool, create_rag_agent

# Create Cube Sandbox tool
tool = create_cube_tool(
    template_id="tpl-xxxx",
    api_url="http://127.0.0.1:3000",
)

# Create agent with code execution capabilities
agent = create_rag_agent(
    template_id="tpl-xxxx",
    api_url="http://127.0.0.1:3000",
)

# Use agent to process tasks
response = agent.chat("Download a webpage and compute word frequency")
```

## Environment Setup

### Option 1: Development Environment (Recommended)

```bash
# Clone repository
git clone https://github.com/TencentCloud/CubeSandbox.git

# Navigate to dev environment
cd CubeSandbox/dev-env

# Prepare VM image (one-time only)
./prepare_image.sh

# Start VM
./run_vm.sh

# In a new terminal, login to VM
cd CubeSandbox/dev-env && ./login.sh

# Inside VM: Install Cube Sandbox
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash

# Create code interpreter template
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999

# Monitor template creation
cubemastercli tpl watch --job-id <job_id>

# Note the output template_id
```

### Option 2: Direct Install (Linux Server)

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

## References

- [Cube Sandbox GitHub](https://github.com/TencentCloud/CubeSandbox)
- [LlamaIndex Documentation](https://docs.llamaindex.ai/)
- [Python SDK](https://github.com/TencentCloud/CubeSandbox/tree/main/sdk/python)
