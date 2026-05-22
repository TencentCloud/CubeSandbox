# Bare-Metal / Physical Machine Deployment

> **Use case:** You already have an x86_64 or aarch64 Linux machine with KVM support (`/dev/kvm` available), such as a physical machine, bare-metal server, or a cloud VM with nested virtualization enabled.
>
> If you're on an **ordinary cloud VM** without `/dev/kvm`, you don't need bare-metal — use PVM to enable KVM on standard cloud VMs, see [Quick Start](./quickstart.md). Note that PVM is only available on x86_64; on aarch64 you must use a host with native `/dev/kvm`.

## Prerequisites

- **x86_64** or **aarch64** Linux machine (see [aarch64 notes](#aarch64-notes) below)
- `/dev/kvm` present and read/writable (`ls -la /dev/kvm`)
- **Root access**
- **Docker** installed and running
- Internet access (for downloading release packages and Docker images)
- RAM ≥ 8 GB, free disk ≥ 50 GB

### aarch64 notes

Cube Sandbox provides **preliminary aarch64 support**. The runtime still requires `/dev/kvm` on the host (no PVM fallback), and you must override the support container images to multi-arch tags so that they pull the arm64 variants.

Before running `install.sh` from the cube-sandbox-one-click bundle, append the following overrides to your `.env`:

```bash
cat <<EOF >> .env
CUBE_SANDBOX_MYSQL_IMAGE="mysql:8.0"
CUBE_SANDBOX_REDIS_IMAGE="redis:7-alpine"
CUBE_PROXY_COREDNS_IMAGE="coredns/coredns:1.14.2"
WEB_UI_IMAGE="openresty/openresty:1.21.4.1-6-alpine-fat"
EOF
```

The default tags in `env.example` point at the `cube-sandbox-image.tencentcloudcr.com/opensource/...` mirror, which currently only ships x86_64 manifests. The upstream Docker Hub tags above provide multi-arch manifests and resolve to the correct arm64 image automatically.

**Validated aarch64 hosts:**

- HUAWEI Kunpeng 920 (bare-metal Linux)

This is currently the only aarch64 platform under active testing. Other arm64 hosts that natively expose `/dev/kvm` are expected to work, but have not been validated by the project yet — please file an issue if you try one and hit problems.

**Known limitations on aarch64:**

- **No PVM support.** PVM (Pagetable-based Virtual Machine) is x86_64-only and there is no plan to port it to aarch64. On aarch64 you must run on a host that natively exposes `/dev/kvm` (bare-metal Arm server, Arm cloud bare-metal, or any environment that surfaces ARM virtualization extensions to the kernel).

::: warning Run all commands as root
Every command in this guide must be executed as **root**. Switch to root first:

```bash
sudo su root
```

:::

## Step 1: Install

Run as root:

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
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
export E2B_API_KEY="dummy"
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