# Quick Start

Get a fully functional Cube Sandbox running in four steps — no source build required.

The steps below guide you through provisioning a cloud server, enabling KVM via PVM, and installing Cube Sandbox on that server.

⚠️ Follow this guide step by step — you can be up and running with Cube Sandbox in just a few minutes!

::: tip Already have a server with KVM enabled?
If you already have an x86_64 Linux server with KVM enabled (bare-metal or physical machine), skip to [Bare-Metal Deployment](./bare-metal-deploy.md) to install directly without PVM.
:::

## Prerequisites

- **x86_64** cloud server (any standard cloud VM works — `/dev/kvm` not required)
- **Root access**
- Internet access (for downloading release packages and Docker images)

### 🖥 Supported Systems

CubeSandbox binaries are built on **Ubuntu 22.04 (glibc 2.35)** — your system **must have glibc ≥ 2.35**, or the binaries won't run.

| OS | Status | Notes |
|---|---|---|
| 🏆 **OpenCloudOS 9 / TencentOS 4** | ✅ Recommended | Best compatibility, XFS by default, production-ready |
| Ubuntu 24.04 | ✅ Tested | glibc 2.39 — works. Requires manual [XFS setup →](https://github.com/TencentCloud/CubeSandbox/issues/311) |
| Ubuntu 22.04 | ✅ Tested | glibc 2.35 — works. Requires manual [XFS setup →](https://github.com/TencentCloud/CubeSandbox/issues/311) |
| Other RPM-based (CentOS, RHEL, etc.) | ⚠️ Check glibc | Must have glibc ≥ 2.35 and XFS for `/data/cubelet` |
| Debian / WSL | ⚠️ Check glibc | Same requirements; see [XFS FAQ →](https://github.com/TencentCloud/CubeSandbox/issues/311) |

> ℹ️ **Why XFS?** CubeSandbox relies on XFS reflink for Copy-on-Write snapshots. Ubuntu / Debian / WSL default to ext4 — you must mount an XFS filesystem at `/data/cubelet`. See [FAQ #311](https://github.com/TencentCloud/CubeSandbox/issues/311) for step-by-step instructions.

::: warning 💾 Disk Space
**`/data/cubelet` must have at least 50 GB** of available disk space for the sandbox image and writable layers. If you plan to build multiple templates or custom images, **200 GB or more is recommended**.
:::

## Step 1: Provision a Cloud Server & Install the PVM Kernel

### Provision a Cloud Server

Provision an **x86_64** cloud server — no special requirements.

**Recommended OS: OpenCloudOS 9** (RPM-based). Cube Sandbox's PVM host kernel is built on the OpenCloudOS kernel, so OpenCloudOS 9 offers the best compatibility without distribution-specific adjustments. Ubuntu, Debian, CentOS, and other mainstream distributions are also supported.

| Config | CPU | RAM | Disk |
| --- | --- | --- | --- |
| Functional experience | ≥ 4 cores | ≥ 8 GB | ≥ 50 GB |
| Recommended | 32 cores | 64 GB | ≥ 200 GB |

::: warning Run all commands as root
Every command in this guide must be executed as **root**. Switch to root first:

```bash
sudo su root
```

:::

### Install the PVM Host Kernel

Go to the [CubeSandbox GitHub Releases](https://github.com/TencentCloud/CubeSandbox/releases) page, open the latest release that includes PVM kernel attachments, **right-click the matching attachment → Copy Link Address**, then download with `wget`.

Choose the format for your Linux distribution:

#### RPM-based (OpenCloudOS, RHEL, CentOS, TencentOS, Fedora)

In the release attachments, find `kernel-*cube.pvm.host*.x86_64.rpm`, right-click and copy the download link:

```bash
# Replace the URL below with the actual download link you copied from the Releases page
wget "<kernel rpm download link>"

# Use --oldpackage if the host already has a newer kernel version
rpm -ivh --oldpackage kernel-*.rpm
```

Set the PVM kernel as the default boot entry:

```bash
# List installed kernels and find the index of the PVM kernel
grubby --info=ALL | grep -E "^kernel|^index"

# Replace <index> with the number from the output above for the PVM kernel
grubby --set-default-index=<index>

# Confirm the change
grubby --default-kernel
```

Configure kernel boot parameters:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/pvm/grub/host_grub_config.sh | bash
```

#### DEB-based (Ubuntu, Debian)

In the release attachments, find `linux-image-*cube.pvm.host*_amd64.deb`, right-click and copy the download link:

```bash
# Replace the URL below with the actual download link you copied from the Releases page
wget "<linux-image deb download link>"

dpkg -i linux-image-*cube.pvm.host*.deb
```

Set the PVM kernel as the default boot entry:

```bash
# List installed kernels to find the PVM kernel version string
ls /boot/vmlinuz-*

# Point GRUB default to the PVM kernel (replace with the actual version string from above)
KVER="$(ls /boot/vmlinuz-*cube.pvm.host* | sed 's|/boot/vmlinuz-||' | tail -1)"
sed -i "s|^GRUB_DEFAULT=.*|GRUB_DEFAULT=\"Advanced options for Ubuntu>Ubuntu, with Linux ${KVER}\"|" \
  /etc/default/grub
```

Configure kernel boot parameters (the script internally calls `update-grub` to apply the GRUB changes above):

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/pvm/grub/host_grub_config.sh | bash
```

### Reboot & Verify

```bash
reboot
```

After rebooting, confirm you're running the PVM kernel and the KVM module is loaded:

```bash
# Verify kernel version
uname -r
# Expected output contains: cube.pvm.host

# Load the PVM KVM module
modprobe kvm_pvm

# Confirm the module is loaded
lsmod | grep kvm
# Expected output includes kvm_pvm
```

Enable `kvm_pvm` to load automatically at boot:

```bash
echo 'kvm_pvm' > /etc/modules-load.d/kvm-pvm.conf
```

::: details What is PVM? (Technical background)
PVM (Pagetable-based Virtual Machine) is a **page-table-based nested virtualization framework** built on top of KVM. Unlike traditional nested virtualization, PVM does not rely on the host hypervisor exposing Intel VT-x / AMD-V hardware virtualization extensions to the guest. Instead, it performs privilege-level switching and memory virtualization within the guest kernel layer through shared memory regions and shadow page tables — completely transparent to the host hypervisor.

Tencent Cloud has deployed PVM instances at scale in production, with reliability validated extensively. Improvements have been upstreamed to the [OpenCloudOS kernel](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git).

For complete PVM deployment details, see [PVM Deployment](./pvm-deploy.md).
:::

## Step 2: Install

Run as root:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | CUBE_PVM_ENABLE=1 bash
```

::: tip Skipping Pre-download Preflight Checks
The online installer script performs lightweight pre-download checks for OS, memory, KVM, and `/data/cubelet` filesystem (requires XFS) to save your time.
If you need to bypass these early checks (e.g. in custom test environments), set the `ONE_CLICK_SKIP_PRECHECK=1` environment variable or pass the `--skip-precheck` argument:
```bash
# Option A: via environment variable
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | ONE_CLICK_SKIP_PRECHECK=1 CUBE_PVM_ENABLE=1 bash

# Option B: via argument
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | CUBE_PVM_ENABLE=1 bash -s -- --skip-precheck
```
⚠️ Note: Skipping the prechecks only affects the download behavior of `online-install.sh`. The authoritative system constraints in `install.sh` are still strictly enforced during actual deployment to ensure stability.
:::

::: details What gets installed
- E2B-compatible REST API listening on port `3000`
- CubeMaster, Cubelet, network-agent, CubeShim running as host processes
- MySQL and Redis managed via Docker Compose
- CubeProxy providing TLS (mkcert) and CoreDNS domain routing (`cube.app`)
:::

## Step 3: Create a Template

After installation, create a code interpreter template using a pre-built image:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

> **Registry note:** Use `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` (recommended for international access). If you are in mainland China, use `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` instead.

Then monitor the build progress:

```bash
cubemastercli tpl watch --job-id <job_id>
```

⚠️ Note: the image is large; downloading, extracting, and building the template may take a while. Please be patient.

Wait for the command above to finish — the template status should become `READY`.

Take note of the **template ID** (`template_id`) from the output; you'll need it in the next step.

For the full template creation workflow and more options, see [Creating Templates from OCI Images](./tutorials/template-from-image.md).

## Step 4: Run Your First Agent Code

Install the Python SDK:

```bash
yum install -y python3 python3-pip

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
| `CUBE_TEMPLATE_ID` | The template ID obtained in Step 3 |
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

- [Create Templates from OCI Images](./tutorials/template-from-image.md) — Customize sandbox environments
- [Bare-Metal Deployment](./bare-metal-deploy.md) — Already have a KVM-enabled server? Install directly
- [Multi-Node Cluster](./multi-node-deploy.md) — Scale across multiple machines
- [HTTPS & Domain Resolution](./https-and-domain.md) — TLS configuration options
- [Authentication](./authentication.md) — Enable API authentication

## Appendix: Build from Source

The steps above use a prebuilt release bundle. If you need to customize components, use a specific commit, or contribute to development, you can build the bundle yourself. See [Self-Build Deployment](./self-build-deploy.md) for full instructions.