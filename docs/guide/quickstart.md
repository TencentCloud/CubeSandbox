# Quick Start

Get a fully functional Cube Sandbox running in three steps — no source build required.

::: tip No bare-metal machine?
If you only have a laptop or a cloud VM (with KVM + nested
virtualization), use the
[Development Environment (QEMU VM)](./dev-environment) guide first — it
spins up a disposable OpenCloudOS 9 VM, and the rest of this Quick
Start works inside that VM.
:::

## Prerequisites

- A **bare-metal Linux server** (x86_64) with KVM enabled (`/dev/kvm` exists)
- **Docker** installed and running
- Internet access (to download the release bundle and pull Docker images)

## Step 1: Install

Run the following command on the target machine as root (or with `sudo`):

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

::: tip Node IP auto-detection
The installer automatically detects the node IP from the `eth0` interface. If your primary network interface has a different name, or you want to pin a specific IP, pass it explicitly:

```bash
CUBE_SANDBOX_NODE_IP=<your-node-ip> bash <(curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh)
```
:::

::: tip China mainland mirror
If GitHub downloads are slow, set `MIRROR=cn` to pull the release bundle from the CDN:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```
:::

::: details What gets installed
- E2B-compatible REST API listening on port `3000`
- CubeMaster, Cubelet, network-agent, and CubeShim running as host processes
- MySQL and Redis managed via Docker Compose
- CubeProxy with TLS (mkcert) and CoreDNS for `cube.app` domain routing
:::

After installation completes, the installer symlinks `cubemastercli` and `cubecli` into `/usr/local/bin`.

## Step 2: Create a Template

Create a code-interpreter template from the prebuilt image:

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49983
```

Monitor the build until the status reaches `READY`:

```bash
cubemastercli tpl watch --job-id <job_id>
```

Note the **template ID** from the output — you will need it in the next step.

For the full template creation workflow and more options, see [Creating Templates from OCI Images](./tutorials/template-from-image).

## Step 3: Run Your First Agent

Install the Python SDK:

```bash
pip install e2b-code-interpreter
```

Set environment variables:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="dummy"
export CUBE_TEMPLATE_ID="<your-template-id>"
export SSL_CERT_FILE="$(mkcert -CAROOT)/rootCA.pem"
```

| Variable | Description |
|----------|-------------|
| `E2B_API_URL` | Points the E2B SDK to your local Cube Sandbox instead of the E2B cloud |
| `E2B_API_KEY` | The SDK requires a non-empty value; any string works |
| `CUBE_TEMPLATE_ID` | The template ID obtained in Step 2 |
| `SSL_CERT_FILE` | mkcert CA root certificate for HTTPS connections to the sandbox |

Run code inside an isolated sandbox:

```python
import os
from e2b_code_interpreter import Sandbox  # drop-in E2B SDK

# Cube Sandbox transparently intercepts all requests
with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox, safely isolated!')")
    print(result)
```

You can also run shell commands and work with files:

```python
import os
from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    # Run a shell command
    result = sandbox.commands.run("echo hello cube")
    print(result.stdout)

    # Read a file inside the sandbox
    content = sandbox.files.read("/etc/hosts")
    print(content)
```

For more end-to-end walkthroughs, see [Examples](./tutorials/examples).

## Next Steps

- [Creating Templates from OCI Images](./tutorials/template-from-image) — customize your sandbox environment
- [Multi-Node Cluster Deployment](./multi-node-deploy) — scale to multiple machines
- [CubeProxy TLS](./cubeproxy-tls) — TLS configuration options
- [Authentication](./authentication) — enable API authentication

## Appendix: Build from Source

The steps above use a prebuilt release bundle. If you need to customize components, use a specific commit, or contribute to development, you can build the bundle yourself. See [Self-Build Deployment](./self-build-deploy) for full instructions.
