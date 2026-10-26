# Downloads & Releases

All CubeSandbox release artifacts live on one [Releases page](https://github.com/TencentCloud/CubeSandbox/releases). Find your scenario below and jump to the matching section:

| Your scenario | What you download manually | Section |
| --- | --- | --- |
| Standard deployment ([Quick Start](./quickstart.md), [bare-metal](./bare-metal-deploy.md)) | Nothing | [Standard deployment](#standard-deployment-nothing-to-download) |
| PVM deployment (cloud server without `/dev/kvm`) | The PVM host kernel package (rpm/deb) | [PVM deployment](#pvm-deployment-download-the-host-kernel-package) |
| Building the bundle yourself / swapping kernel or image | Guest kernels, guest image | [Self-build](#self-build-downloading-guest-kernels-and-image) |

::: tip Just following the Quick Start?
You don't need this page — the [online installer](./quickstart.md) downloads the one-click bundle and resolves all artifacts automatically.
:::

## Standard deployment: nothing to download

Install online with one command (see [Quick Start](./quickstart.md)):

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

To download the full bundle manually (`cube-sandbox-one-click-<version>-<arch>.tar.gz`, arch is `amd64` / `arm64`):

```bash
wget https://github.com/TencentCloud/CubeSandbox/releases/download/v0.6.0/cube-sandbox-one-click-v0.6.0-amd64.tar.gz
```

## PVM deployment: download the host kernel package

The host kernel package is the only artifact you download by hand for a PVM deployment — install it, reboot into the PVM kernel, and the machine gains KVM capability. The PVM *guest* kernel (`vmlinux-pvm`) already ships inside the one-click bundle and is activated by `CUBE_PVM_ENABLE=1` at install time. For the install, GRUB, and reboot steps see [PVM Deployment — Step 1](./pvm-deploy.md#step-1-install-the-pvm-host-kernel); this section only covers getting the package.

The kernel package is published on dedicated `kernel-release-*` Releases — download it from the release page:

1. Open the [GitHub Releases page](https://github.com/TencentCloud/CubeSandbox/releases?q=kernel-release-&expanded=true) (or the [CNB mirror](https://cnb.cool/CubeSandbox/CubeSandbox/-/releases) for mainland China — filter by `kernel-release` there), and open the newest `kernel-release-*` release
2. Download the **main package** for your distribution:
   - RPM-based (OpenCloudOS, RHEL, CentOS, TencentOS, Fedora): `kernel-*opencloudos9.cubesandbox.pvm.host*.x86_64.rpm`
   - DEB-based (Ubuntu, Debian): `linux-image-*opencloudos9.cubesandbox.pvm.host*_amd64.deb`

<small>Optional assets like `kernel-headers-*` and `-dbg` are not needed.</small>

If you have the GitHub CLI installed, you can download by glob without hunting for the exact filename — see the [appendix](#appendix-url-patterns-latest-tags-and-version-pinning).

> 📌 **Shortcut for OpenCloudOS 9 users**
> The kernel package is also on the official OpenCloudOS yum repository — `dnf install` installs it directly, no manual rpm download needed, and the whole deployment takes about 5 commands.
> 👉 [One-command CubeSandbox deployment on OpenCloudOS 9 — walkthrough (Chinese)](https://mp.weixin.qq.com/s/oGAaUpze_uB_uzyvuYJSIg)

## Self-build: downloading guest kernels and image

You only need these when [building the release bundle yourself](./self-build-deploy.md) or replacing components (the one-click bundle already ships both):

```bash
# Guest kernels (published under kernel-release-*, filenames are stable)
wget "https://github.com/TencentCloud/CubeSandbox/releases/download/kernel-release-260812-1/vmlinux-amd64"    # bare-metal/KVM; use vmlinux-arm64 on aarch64
wget "https://github.com/TencentCloud/CubeSandbox/releases/download/kernel-release-260812-1/vmlinux-pvm-amd64" # PVM guest kernel (x86_64 only)

# Guest image (published under guest-image-*, contains cube-guest-image-cpu.img)
wget https://github.com/TencentCloud/CubeSandbox/releases/download/guest-image-260820-1/cube-guest-image-amd64.tar.gz
tar -xzf cube-guest-image-amd64.tar.gz
```

<small>The tags in these URLs advance with each release (filenames are stable — just swap the tag); find the newest ones on the Releases page filtered by [`kernel-release`](https://github.com/TencentCloud/CubeSandbox/releases?q=kernel-release-&expanded=true) or [`guest-image`](https://github.com/TencentCloud/CubeSandbox/releases?q=guest-image-&expanded=true).</small>

For a self-build, place `vmlinux` into `deploy/one-click/assets/kernel-artifacts/` — see [Self-Build Deployment — 1.1 Prepare the Kernel](./self-build-deploy.md#_1-1-prepare-the-kernel).

## Appendix: URL patterns, latest tags, and version pinning

**URL patterns** — every asset is identical on both channels; swap the hostname to switch:

| Channel | URL pattern |
| --- | --- |
| GitHub (canonical) | `https://github.com/TencentCloud/CubeSandbox/releases/download/<tag>/<asset>` |
| CNB mirror (mainland China) | `https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/<tag>/<asset>` |

**Using `gh` so you don't have to hunt for filenames** — download by pattern, or list the kernel-release tags:

```bash
# List the newest kernel-release tags
gh release list --repo TencentCloud/CubeSandbox --limit 30 | grep kernel-release

# Download by glob (replace <tag> with the newest tag found above; no exact
# filename needed)
gh release download <tag> --repo TencentCloud/CubeSandbox \
  --pattern 'kernel-6*.x86_64.rpm'   # or: --pattern 'linux-image-6*_amd64.deb'
```

**Version pinning** — every `v*` product release records the kernel / guest-image tags it was built with in [`deploy/release-assets.yaml`](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/release-assets.yaml) (also recorded in the bundle's `release-manifest.json`), which answers "which kernel is inside my bundle". Dedicated tags are released on their own cadence, so the pin may lag behind the newest tag — for the **PVM host kernel, always take the newest `kernel-release-*`** (it carries kernel security fixes); there is no need to match a product version.

(Product releases up to `v0.6.0` also attached kernel/image assets directly to the `v*` release — a legacy layout that no longer applies.)
