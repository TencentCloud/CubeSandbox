# Bare-Metal / Physical Machine Deployment

> **Use case:** You already have an x86_64 or aarch64 (ARM64) Linux machine with KVM support (`/dev/kvm` available), such as a physical machine, bare-metal server, or a cloud VM with nested virtualization enabled.
>
> If you're on an **ordinary cloud VM** without `/dev/kvm`, you don't need bare-metal — use PVM to enable KVM on standard cloud VMs, see [Quick Start](./quickstart.md).

::: warning Production Use
If you plan to use Cube Sandbox in a production environment, please refer to the [Network Hardening](./network-hardening.md) guide to secure your deployment before exposing services to untrusted networks.
:::

## Prerequisites

- **x86_64** or **aarch64** (ARM64) Linux machine
- `/dev/kvm` present and read/writable (`ls -la /dev/kvm`)
- **Root access**
- **Docker** installed and running
- Internet access (for downloading release packages and Docker images)
- RAM ≥ 8 GB, free disk ≥ 50 GB

::: warning Run all commands as root
Every command in this guide must be executed as **root**. Switch to root first:

```bash
sudo su root
```

:::

## Step 1: Install

### x86_64 (AMD64)

Run as root:

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```

### ARM64 (aarch64) Hosts

::: warning online-install.sh ARM64 support coming soon
The `online-install.sh` one-command installer currently auto-discovers **x86_64** packages only. ARM64 support in `online-install.sh` will be available in an upcoming release. For now, ARM64 users should follow the manual steps below.
:::

**Step 1:** Go to the release page for your region, find the latest release that includes ARM64 assets, and download the `cube-sandbox-one-click-*-arm64.tar.gz` package:

| Platform | Release Page |
|---|---|
| GitHub | [TencentCloud/CubeSandbox/releases](https://github.com/TencentCloud/CubeSandbox/releases) |
| CNB (China) | [CubeSandbox/CubeSandbox/-/releases](https://cnb.cool/CubeSandbox/CubeSandbox/-/releases) |

**Step 2:** Extract and run the installer:

```bash
# Replace <version> with the actual version you downloaded (e.g. v0.5.0-rc3)
tar -xzf cube-sandbox-one-click-<version>-arm64.tar.gz
cd cube-sandbox-one-click-<version>-arm64
chmod +x install.sh
./install.sh
```

::: details What gets installed
- E2B-compatible REST API listening on port `3000`
- CubeMaster, Cubelet, network-agent, CubeShim running as host processes
- MySQL and Redis managed via Docker Compose
- CubeProxy providing TLS (mkcert) and CoreDNS domain routing (`cube.app`)
:::

## Step 2: Create a Template

After installation, create a code interpreter template using a pre-built image:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

> **Registry note:** For users in China, use `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`. For international access, use `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`.

Monitor the build progress:

```bash
cubemastercli tpl watch --job-id <job_id>
```

⚠️ Note: the image is large; downloading, extracting, and building the template may take a while. Please be patient.

Wait for the command above to finish — the template status should become `READY`.

Take note of the **template ID** (`template_id`) from the output; you'll need it in the next step.

For a full walkthrough and additional parameters, see [Create Templates from OCI Image](./tutorials/template-from-image.md).

## Step 3: Run Your First Agent Code

Install Python SDK:

```bash
yum install -y python3 python3-pip
pip config set global.index-url https://mirrors.ustc.edu.cn/pypi/simple

pip install e2b-code-interpreter
```

Set environment variables:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<your-template-id>"
export SSL_CERT_FILE="/root/.local/share/mkcert/rootCA.pem"
```

| Variable | Description |
|------|------|
| `E2B_API_URL` | Points the E2B SDK to your local Cube Sandbox instead of the E2B cloud service |
| `E2B_API_KEY` | Required by the SDK; use any placeholder string for local deployment |
| `CUBE_TEMPLATE_ID` | The template ID obtained in Step 2 |
| `SSL_CERT_FILE` | Path to the mkcert CA root certificate, required for sandbox HTTPS connections |

Run code in an isolated sandbox:

```python
import os
from e2b_code_interpreter import Sandbox  # Use the E2B SDK directly!

# CubeSandbox seamlessly handles all requests under the hood
with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox, safely isolated!')")
    print(result)
```

For more end-to-end examples, see [Examples](./tutorials/examples.md).

## Next Steps

- [Create Templates from OCI Image](./tutorials/template-from-image.md) — Customize sandbox environments
- [Multi-Node Cluster](./multi-node-deploy.md) — Scale across multiple machines
- [HTTPS & Domain Resolution](./https-and-domain.md) — TLS configuration options
- [Authentication](./authentication.md) — Enable API authentication