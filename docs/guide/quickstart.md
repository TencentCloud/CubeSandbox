# Quick Start

Get a fully functional Cube Sandbox running in four steps — no source build required.

The steps below boot a **disposable Linux VM** on your development machine (WSL / Linux) and install Cube Sandbox inside that VM.
Follow this guide step by step — you can be up and running with Cube Sandbox in just a few minutes!

::: tip Already have a bare-metal server?
If you already have an x86_64 Linux bare-metal server with KVM enabled, you can **skip Step 1** and run the Step 2 installer directly on that server.
:::

## Prerequisites

Any one of the following hosts works:

- **WSL 2 on Windows** (Windows 11 22H2+, with nested virtualization enabled in WSL)
- **An x86_64 Linux physical machine**
- **A Linux VM with nested virtualization enabled** (e.g. Ubuntu 22.04 on VMware with "Virtualize Intel VT-x/EPT or AMD-V/RVI" enabled in the VM's CPU settings)
- **An x86_64 bare-metal Linux server**

Common requirements:

1. The Linux environment can use KVM (`/dev/kvm` exists and is read/writable)
2. **Docker** and **QEMU** installed and running in the Linux environment
3. Internet access (to clone the repo, download the release bundle, and pull Docker images)

## Step 1: Boot the Development VM

Clone the repository and change into `dev-env/`:

```bash
git clone https://github.com/tencentcloud/CubeSandbox.git
cd CubeSandbox/dev-env
```

Three commands total. The first two run in one terminal, the third in
a **second terminal**.

> Before running the commands below, make sure `qemu`, `qemu-img`, and `ripgrep` are installed on your Linux machine.

```bash
./prepare_image.sh   # one-off: download + init the OpenCloudOS 9 image
./run_vm.sh          # boot the VM; keep this terminal open (Ctrl+a x to power off)
```

In a second terminal:

```bash
cd CubeSandbox/dev-env
./login.sh           # SSH into the VM as root
```

All the following steps run **inside this VM** — `login.sh` drops you
straight into a root shell where Cube Sandbox will be installed.

For host self-check (nested KVM, required packages), port mappings,
environment overrides, and troubleshooting, see
[Development Environment (QEMU VM)](./dev-environment.md).

## Step 2: Install

Run the following command **inside the dev VM** as root:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

::: details What gets installed
- E2B-compatible REST API listening on port `3000`
- CubeMaster, Cubelet, network-agent, and CubeShim running as host processes
- MySQL and Redis managed via Docker Compose
- CubeProxy with TLS (mkcert) and CoreDNS for `cube.app` domain routing
:::

After installation completes, the installer symlinks `cubemastercli` and `cubecli` into `/usr/local/bin`.

## Step 3: Create a Template

Create a code-interpreter template from the prebuilt image:

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Then run the following command to monitor the build progress:

```bash
cubemastercli tpl watch --job-id <job_id>
```

**⚠ The image is fairly large** — downloading, extracting, and building the template may take a while; please be patient.


Wait for the command above to finish and the template status to reach `READY`.

Note the **template ID** (`template_id`) from the output — you will need it in the next step.

For the full template creation workflow and more options, see [Creating Templates from OCI Images](./tutorials/template-from-image.md).

## Step 4: Run Your First Agent

Install the Python SDK:

```bash
yum install -y python3 python3-pip
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
| `CUBE_TEMPLATE_ID` | The template ID obtained in Step 3 |
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


For more end-to-end walkthroughs, see [Examples](./tutorials/examples.md).

## Next Steps

- [Creating Templates from OCI Images](./tutorials/template-from-image.md) — customize your sandbox environment
- [Multi-Node Cluster Deployment](./multi-node-deploy.md) — scale to multiple machines
- [CubeProxy TLS](./cubeproxy-tls.md) — TLS configuration options
- [Authentication](./authentication.md) — enable API authentication

## Appendix: Build from Source

The steps above use a prebuilt release bundle. If you need to customize components, use a specific commit, or contribute to development, you can build the bundle yourself. See [Self-Build Deployment](./self-build-deploy.md) for full instructions.
