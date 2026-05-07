# PVM Deployment

> **When to use this guide:** Your cloud server does not expose `/dev/kvm` (nested virtualization is blocked by the cloud provider). If your machine already has KVM support, refer to [Quick Start](./quickstart.md) or [Self-Build Deployment](./self-build-deploy.md) instead.

PVM enables you to deploy Cube Sandbox on an **ordinary cloud server**, with all sandbox instances running inside PVM-backed Micro-VMs.

Compared to a standard deployment, PVM adds only two extra steps:

1. Install the PVM host kernel and reboot
2. Pass `CUBE_PVM_ENABLE=1` when running the Cube Sandbox installer

::: tip Production-ready
Tencent Cloud has deployed PVM instances at scale in production environments, with reliability validated in production. The improvements have been open-sourced in the [OpenCloudOS kernel](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git).
:::

::: details What is PVM? (Technical background)
PVM (Pagetable-based Virtual Machine) is a **page-table-based nested virtualization framework** built on top of KVM. Unlike conventional nested virtualization, PVM does not require the host hypervisor to expose hardware virtualization extensions (Intel VT-x / AMD-V) to the guest. Instead, it uses a minimal shared memory region between the guest and the guest hypervisor, combined with an efficient shadow page table design, to handle privilege-level transitions and memory virtualization — all transparently to the host hypervisor.

PVM was originally proposed in the paper [*PVM: Efficient Shadow Paging for Deploying Secure Containers in Cloud-native Environment*](https://dl.acm.org/doi/10.1145/3600006.3613158). Tencent Cloud has since made extensive improvements in features, performance, and bug fixes, and has open-sourced the work in the [OpenCloudOS kernel](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git) for the community. Over the years, we have deployed a large number of PVM instances in Tencent Cloud production environments, and their reliability has been validated in production.
:::

## Prerequisites

- **x86_64** Linux server (cloud VM or physical machine)
- **Root access**
- **No `/dev/kvm` required** — PVM provides KVM capability after the kernel switch
- All other requirements are the same as [Quick Start](./quickstart.md#prerequisites) (≥ 8 GB RAM, XFS-capable storage for `/data/cubelet`, etc.)

::: warning Run all commands as root
Every command in this guide must be run as **root**. Switch to the root user first:

```bash
sudo su root
```

Then run all subsequent commands directly in the root shell.
:::

## Step 1: Install the PVM Host Kernel

Go to the [CubeSandbox GitHub Releases](https://github.com/TencentCloud/CubeSandbox/releases) page, open the latest release that includes PVM kernel assets, then **right-click each asset → Copy link address** and paste the URL into the `wget` commands below.

Choose the package format that matches your Linux distribution:

### RPM-based (OpenCloudOS, RHEL, CentOS, TencentOS, Fedora)

Find the following file in the release assets, right-click it to copy its download link:

- `kernel-*cube.pvm.host*.x86_64.rpm` (kernel package)

```bash
# Replace the URLs below with the actual download links copied from the Releases page
wget "<kernel rpm download URL>"

# --oldpackage skips the version check if a newer kernel is already installed
rpm -ivh --oldpackage kernel-*.rpm
```

Set the PVM kernel as the default boot entry:

```bash
# List installed kernels and find the index of the PVM kernel
grubby --info=ALL | grep -E "^kernel|^index"

# Replace <index> with the number shown for the PVM kernel
grubby --set-default-index=<index>

# Verify
grubby --default-kernel
```

Configure the required kernel boot parameters:

```bash
bash <(curl -fsSL \
  https://raw.githubusercontent.com/TencentCloud/CubeSandbox/master/deploy/pvm/grub/host_grub_config.sh)
```

### DEB-based (Ubuntu, Debian)

Find the following file in the release assets, right-click it to copy its download link:

- `linux-image-*cube.pvm.host*_amd64.deb` (kernel package)

```bash
# Replace the URLs below with the actual download links copied from the Releases page
wget "<linux-image deb download URL>"

dpkg -i linux-image-*cube.pvm.host*.deb
```

Set the PVM kernel as the default boot entry:

```bash
# List installed kernels and note the PVM kernel version string
ls /boot/vmlinuz-*

# Point GRUB to the PVM kernel (the version string is read automatically)
KVER="$(ls /boot/vmlinuz-*cube.pvm.host* | sed 's|/boot/vmlinuz-||' | tail -1)"
sed -i "s|^GRUB_DEFAULT=.*|GRUB_DEFAULT=\"Advanced options for Ubuntu>Ubuntu, with Linux ${KVER}\"|" \
  /etc/default/grub
```

Configure the required kernel boot parameters (the script also calls `update-grub`, applying the default-entry change above):

```bash
bash <(curl -fsSL \
  https://raw.githubusercontent.com/TencentCloud/CubeSandbox/master/deploy/pvm/grub/host_grub_config.sh)
```

### Reboot and Verify

```bash
reboot
```

After rebooting, confirm you are running the PVM kernel and that the KVM module loads successfully:

```bash
# Confirm the kernel version
uname -r
# Expected output contains: cube.pvm.host

# Load the PVM KVM module
modprobe kvm_pvm

# Confirm the module is loaded
lsmod | grep kvm
# Expected output includes kvm_pvm
```

Configure `kvm_pvm` to load automatically on boot:

```bash
echo 'kvm_pvm' > /etc/modules-load.d/kvm-pvm.conf
```

::: tip Already have Cube Sandbox installed?
If you have a running Cube Sandbox installation using the standard kernel, **you do not need to reinstall it**. Simply reboot into the PVM kernel and proceed to [Step 2](#step-2-install-cube-sandbox-with-pvm-enabled).
:::

## Step 2: Install Cube Sandbox with PVM Enabled

::: tip Why `CUBE_PVM_ENABLE=1`?
The release bundle ships two guest kernels: a standard one (`vmlinux`) and a PVM-optimized one (`vmlinux-pvm`). Setting `CUBE_PVM_ENABLE=1` tells the installer to use the PVM guest kernel as the active runtime kernel. Without this flag, the standard guest kernel is used and PVM has no effect.
:::

### Option A: Online Install (Recommended)

The online install script downloads the release bundle to a temporary directory before running `install.sh`. Because the temporary directory contains no `.env` file, the `CUBE_PVM_ENABLE=1` environment variable takes effect directly:

```bash
# Using GitHub (default)
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh \
  | CUBE_PVM_ENABLE=1 bash
```

```bash
# Using the CN mirror for faster downloads in mainland China
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh \
  | CUBE_PVM_ENABLE=1 MIRROR=cn bash
```

To explicitly set the node IP (recommended on machines with multiple network interfaces):

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh \
  | CUBE_PVM_ENABLE=1 bash -s -- --node-ip=<your-server-ip>
```

### Option B: Manual Download

Download `cube-sandbox-one-click-<sha>.tar.gz` from [GitHub Releases](https://github.com/TencentCloud/CubeSandbox/releases), extract it, and pass the environment variable inline:

```bash
tar -xzf cube-sandbox-one-click-<sha>.tar.gz
cd cube-sandbox-one-click-<sha>

# The extracted directory contains no .env file, so the env variable takes effect
CUBE_PVM_ENABLE=1 ./install.sh
```

::: warning env file override pitfall
If you followed the developer guide and ran `cp env.example .env`, a `.env` file now exists in the directory with the default value `CUBE_PVM_ENABLE=0`. The installer `source`s this file, which **overrides** any same-named variable exported from the parent shell, silently disabling PVM.

To fix this, update the `.env` file before running the installer:

```bash
sed -i 's/^CUBE_PVM_ENABLE=0/CUBE_PVM_ENABLE=1/' .env
grep CUBE_PVM_ENABLE .env   # Verify: expected CUBE_PVM_ENABLE=1

./install.sh
```
:::

### Confirm a Successful Install

The install log should contain:

```
[one-click] CUBE_PVM_ENABLE=1, installed PVM guest kernel as .../cube-kernel-scf/vmlinux
```

## Step 3: Verify the PVM Environment

```bash
# Confirm PVM is enabled in the runtime configuration
grep CUBE_PVM_ENABLE /usr/local/services/cubetoolbox/.one-click.env
# Expected: CUBE_PVM_ENABLE=1

# Confirm the KVM device is available
ls -la /dev/kvm

# Confirm the PVM KVM module is loaded
lsmod | grep kvm_pvm
```

## Step 4: Create a Template and Get Started

With PVM up and running, the rest of the process is identical to a standard deployment. Follow [Step 3: Create a Template](./quickstart.md#step-3-create-a-template) in the Quick Start guide to create your first template and run a sandbox.

## Troubleshooting

**Q1: Install log shows `using ordinary guest kernel` instead of the PVM guest kernel**

**A:** This is almost always caused by a `.env` file containing `CUBE_PVM_ENABLE=0` overriding the environment variable. See the warning in [Option B](#option-b-manual-download) above.

---

**Q2: `lsmod | grep kvm_pvm` returns no output, or `/dev/kvm` does not exist**

**A:** Run `uname -r` to confirm you rebooted into the PVM kernel (the version string should contain `cube.pvm.host`). Once confirmed, run `modprobe kvm_pvm` manually. If it still fails, verify the kernel packages are installed correctly:

```bash
# RPM-based
rpm -qa | grep cube.pvm

# DEB-based
dpkg -l | grep cube.pvm
```
