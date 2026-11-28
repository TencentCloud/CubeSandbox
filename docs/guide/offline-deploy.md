# Offline Deployment

## 1. Offline Preparation

Before installing on the target nodes, complete the following preparation steps across a **connected staging machine** and your **target hosts**:

| Preparation Item | Required? | Where to Perform | Section |
| --- | --- | --- | --- |
| Verify target host prerequisites | Required | Target nodes | [1.1 Verify Host Prerequisites](#host-requirements) |
| Download release bundle (and optional PVM kernel) | Required | Connected machine -> Target nodes | [1.2 Download Offline Release Artifacts](#download-artifacts) |
| Prepare & install host OS packages | Required if no internal mirror | Connected machine -> Target nodes | [1.3 Pre-install Host OS Packages](#preinstall-os-packages) |
| Prepare & import container images (**core**) | Required | Connected machine -> Internal registry / Target nodes | [1.4 Prepare Container Images](#prepare-container-images) |
| Push template source images to internal registry | Required for offline template builds | Connected machine -> Internal registry | [1.5 Prepare Template Source Images](#prepare-template-images) |

### 1.1 Verify Host Prerequisites {#host-requirements}

Target host requirements are identical to an online deployment (see [Quick Start — Prerequisites](./quickstart.md#prerequisites)):

- **Architecture & OS**: `x86_64` or `aarch64` Linux, Linux kernel **>= 5.4** (`uname -r`), `glibc >= 2.31`, with `root` access.
- **Memory**: at least **8 GB** RAM (enforced by a hard preflight check; installation aborts below this threshold).
- **Disk**: `/data/cubelet` must reside on an **XFS** filesystem with at least **50 GB** free space.
- **Virtualization (KVM / PVM)**:
  - Bare-metal hosts provide native `/dev/kvm`.
  - `x86_64` cloud VMs without `/dev/kvm` require the PVM host kernel package (see [1.2](#download-artifacts) and [2.1](#install-pvm-kernel)).
  - `aarch64` (ARM64) machines do not support PVM and must already expose `/dev/kvm`.
- **Multi-node cluster requirements**: keep time synchronized across control and compute nodes (e.g., internal `chrony` / NTP so clock skew stays well within the 15-second heartbeat window) and verify internal port reachability (see [Multi-Node Cluster](./multi-node-deploy.md)).

### 1.2 Download Offline Release Artifacts {#download-artifacts}

On a machine with public internet access (or from an internal artifact mirror/relay), download:

1. **One-click release bundle**: `cube-sandbox-one-click-<version>-<arch>.tar.gz`
   - Match `<arch>` to your target nodes (`amd64` or `arm64`); download channels are listed in [Downloads & Releases](./downloads.md).
   - Control nodes and compute nodes reuse the **same** bundle.
2. **PVM host kernel package (RPM / DEB, optional)**:
   - Only needed for `x86_64` cloud VMs without `/dev/kvm`.
   - Download the package matching your Linux distribution from the latest `kernel-release-*` release.

Transfer the downloaded files to your target nodes via `scp`, an internal artifact server, or removable media.

### 1.3 Pre-install Host OS Packages (No Internal Mirror Only) {#preinstall-os-packages}

**If your target nodes already have an internal apt / dnf / yum mirror that includes standard Docker Engine and Docker Compose, skip directly to [1.4](#prepare-container-images).**

::: warning Confirm Docker is in your internal mirror before skipping
Many internal OS mirrors only carry base distro packages and **omit Docker Engine and Docker Compose**. If you skip this step and Docker is missing from the internal mirror, the installer will attempt an online installation and fail.
:::

When no internal mirror is available (or it lacks the packages below), download the offline packages on a connected machine running the same OS distribution, transfer them to the target nodes, and install them beforehand:

::: details OS package checklist (click to expand)

**Required on every node:**

- **Docker Engine and Docker Compose**: as long as both `docker` and `docker compose version` (or `docker-compose`) work, `install.sh` skips online Docker installation. Verify with:

  ```bash
  docker version && docker compose version   # or: docker-compose --version
  ```

  > ⚠️ **Snap Docker is rejected** (its confinement cannot read `/usr/local/services`, and the installer aborts if detected). Install `docker-ce` or `docker.io` from standard packages instead.

- **Basic CLI tools**: `tar`, `ss`, `ip` (`iproute2`), `bash`, `curl`, `grep`, `sed`, `pgrep` (`procps`), `awk`, `date`, `openssl`
- **Storage dependencies**: `e2fsprogs`, `util-linux` (required by the default `cubecow` storage backend for `mkfs.ext4`, `mount`, `umount`, `losetup`)

**Required only when applicable:**

- `dnsmasq`: **mandatory** when the host does not run `systemd-resolved` / `resolvectl`; otherwise the DNS fallback attempts to install it online.
- `s3fs` + `jq`: dependencies for S3 volume mounts. The installer attempts best-effort installation and only logs a warning on failure, so pre-install them in offline environments to avoid volume attach errors later.
- `python3`, `nvme-cli`, `libaio`, `libnuma`, `libuuid`: only required when enabling CubeS3lvol (`ONE_CLICK_ENABLE_S3LVOL=1`).
- `logrotate`: install if omitted from a minimal OS image.

:::

### 1.4 Prepare Container Images (Core) {#prepare-container-images}

Supplying the required container images is the most important part of offline preparation. Choose either approach based on cluster size:

- **Approach A: Push to an internal registry (e.g., Harbor — recommended for multi-node clusters)**
  - Pull the images on a connected machine and push them to your internal registry (or transfer via tarball to an internal relay host and push from there).
  - Point the override variables in `.env` to the internal registry (and run `docker login` on target hosts if authentication is enabled). The installer pulls directly from the internal registry with no per-node `docker load`.
- **Approach B: Air-gapped `docker save` / `docker load` (no internal registry, single-node, or small clusters)**
  - On a connected host with the **same CPU architecture (`amd64` / `arm64`)** as the target, run `docker pull` -> `docker save`, transfer the `.tar` archive to each target node, and run `docker load`.

#### Required Images by Node Role

**1. Control node / All-in-One single node (`./install.sh`):**

| Image | Default Reference | `.env` Override Variable | Notes |
| --- | --- | --- | --- |
| MySQL | `cube-sandbox-image.tencentcloudcr.com/opensource/mysql:8.0` | `CUBE_SANDBOX_MYSQL_IMAGE` | Skip if using external MySQL / PostgreSQL |
| Redis | `cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine` | `CUBE_SANDBOX_REDIS_IMAGE` | Skip if using external Redis |
| MinIO | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/minio:<release>` | `CUBE_SANDBOX_MINIO_IMAGE` | Enabled by default; skip if bundled MinIO is disabled |
| CoreDNS | `cube-sandbox-image.tencentcloudcr.com/opensource/coredns/coredns:1.14.2` | `CUBE_PROXY_COREDNS_IMAGE` | Required |
| WebUI (OpenResty) | `cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat` | `WEB_UI_IMAGE` | Required |
| cube-proxy | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-proxy:<tag>` | `CUBE_SANDBOX_CUBE_PROXY_IMAGE` | Required |
| cube-lifecycle-manager | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-lifecycle-manager:<tag>` | `CUBE_SANDBOX_CUBE_LCM_IMAGE` | Required |
| cube-egress | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress:<tag>` | `CUBE_SANDBOX_CUBE_EGRESS_IMAGE` | Required |

**2. Compute node (`./install-compute.sh`):**

| Image | Default Reference | `.env` Override Variable | Notes |
| --- | --- | --- | --- |
| cube-egress | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress:<tag>` | `CUBE_SANDBOX_CUBE_EGRESS_IMAGE` | **Required** (transparent egress proxy runs on every compute node) |
| JuiceFS mount | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/juicefs:ce-v1.4.1` | `CUBE_JFS_IMAGE` | Only when `ONE_CLICK_ENABLE_JFS=1`; `MIRROR=cn` selects the `-cn` host |

> **Resolve exact `<tag>` / `<release>` values:** extract the release bundle and run the following command from the bundle root:
>
> ```bash
> grep -rhE '_IMAGE.*_DEFAULT="' scripts/
> ```
>
> This prints the exact default `-int` and `-cn` references for `cube-proxy`, `cube-lifecycle-manager`, `cube-egress`, `minio`, and `juicefs`.

#### Approach B (`docker save` / `docker load`) Example

```bash
# 1. On a connected machine with the same architecture as the target, pull each required image
docker pull <image-ref-1>
docker pull <image-ref-2>
# ...

# 2. Pack per node role (compute nodes only need cube-egress, keeping transfers small)
docker save -o cube-images-control.tar <image-1> <image-2> ...
docker save -o cube-images-compute.tar <cube-egress-image>

# 3. Transfer to the target node and load into local Docker
docker load -i cube-images-control.tar

# 4. Verify on the target node (repository and tag must match exactly)
docker images
```

::: tip Keep image references strictly consistent
- If you pulled and loaded the `-cn` default references, set `MIRROR=cn` in `.env`. If you pulled `-int`, leave `MIRROR` unset.
- If you pushed images to an internal registry or renamed them, configure the matching override variables from the tables above in `.env`.
:::

### 1.5 Prepare Template Source Images (Optional, Required for Offline Templates) {#prepare-template-images}

If you plan to build sandbox templates offline (for example, the official code-interpreter image `sandbox-code:latest`), push your template source images into an **internal container registry** ahead of time:

- **How it works**: `CubeMaster` / `CubeTemplateCenter` pulls the source OCI image from the registry during template build and distributes the built artifacts to compute nodes over the intranet. Compute nodes never pull the template source image directly.
- **Custom template images**: building a Dockerfile offline typically requires `apt` / `pip`. Either point package managers at internal mirrors, or build the complete image on a connected machine and push it to your internal registry (see [Local & Remote Image Practice — Troubleshooting](./tutorials/template-build-practice.md#troubleshooting)).

Example using `sandbox-code:latest`:

```bash
# 1. Pull and save on a connected machine
docker pull cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
docker save -o sandbox-code.tar cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

# 2. Transfer to a host that can reach the internal registry (e.g., the control node), load, tag, and push
docker load -i sandbox-code.tar
docker tag cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  <internal-registry>/cube/sandbox-code:latest
docker push <internal-registry>/cube/sandbox-code:latest
```

---

## 2. How to Deploy

Once all items in Section 1 are ready, run the standard installation workflow on the offline target hosts. **All commands below must be executed as `root`**:

```bash
sudo su root
```

### 2.1 (x86_64 Cloud VMs Without `/dev/kvm` Only) Install the PVM Host Kernel {#install-pvm-kernel}

If your target node already has `/dev/kvm` (bare metal or KVM-enabled ARM64 hosts), skip ahead to [2.2](#deploy-control-node).

For `x86_64` cloud VMs without `/dev/kvm`, install the PVM host kernel package (RPM / DEB) transferred in [1.2](#download-artifacts), reboot into the new kernel, and verify it is active before continuing (see [PVM Deployment — Step 1](./pvm-deploy.md#step-1-install-the-pvm-host-kernel)).

### 2.2 Deploy the Control Node (or All-in-One Single Node) {#deploy-control-node}

1. **Extract the offline bundle and create `.env`:**

```bash
tar -xzf cube-sandbox-one-click-<version>-<arch>.tar.gz
cd cube-sandbox-one-click-<version>-<arch>
cp env.example .env
```

2. **Configure `.env` for your offline environment:**

Adjust the node IP, external MySQL/Redis settings (if used), PVM toggle, and image references as needed.

- **If you pre-loaded default images via `docker load`**:
  - Loaded `-int` images: no image override needed.
  - Loaded `-cn` images: uncomment and set `MIRROR=cn` in `.env`.
- **If you use an internal registry**: set the override variables in `.env` (example):

```bash
# Optional explicit node private IP (auto-detected from eth0 when unset)
CUBE_SANDBOX_NODE_IP=10.0.0.10

# Set to 1 on x86_64 cloud VMs running the PVM host kernel
# CUBE_PVM_ENABLE=1

# Override container images with your internal registry references
CUBE_SANDBOX_MYSQL_IMAGE=harbor.internal/cube/mysql:8.0
CUBE_SANDBOX_REDIS_IMAGE=harbor.internal/cube/redis:7-alpine
CUBE_SANDBOX_MINIO_IMAGE=harbor.internal/cube/minio:<release>
CUBE_PROXY_COREDNS_IMAGE=harbor.internal/cube/coredns:1.14.2
WEB_UI_IMAGE=harbor.internal/cube/openresty:1.21.4.1-6-alpine-fat
CUBE_SANDBOX_CUBE_PROXY_IMAGE=harbor.internal/cube/cube-proxy:<tag>
CUBE_SANDBOX_CUBE_LCM_IMAGE=harbor.internal/cube/cube-lifecycle-manager:<tag>
CUBE_SANDBOX_CUBE_EGRESS_IMAGE=harbor.internal/cube/cube-egress:<tag>
```

3. **Run the installer:**

```bash
./install.sh
```

Match the rest of your workflow to your host type (replacing only the online install command with the offline bundle steps above):
- **Cloud VM without `/dev/kvm` (PVM)**: follow [Quick Start](./quickstart.md).
- **Bare metal / KVM-enabled hosts**: follow [Bare-Metal Deployment](./bare-metal-deploy.md).

::: tip Expected log output during offline installs
When the upstream registry is unreachable, `cube-proxy`, `cube-lifecycle-manager`, and `cube-egress` each log `WARN: pull failed but local image exists; using cached copy` and proceed with the cached local image, while the other containers reuse theirs silently. **This is expected behavior, not an error.** Only `pull failed ... and no local copy is cached` indicates a missing or mismatched image — recheck [1.4](#prepare-container-images).
:::

### 2.3 (Optional) Add Offline Compute Nodes {#deploy-compute-node}

To scale into a multi-node cluster, reuse the same release bundle on each compute node (after pre-loading `cube-egress` or configuring internal registry access):

```bash
tar -xzf cube-sandbox-one-click-<version>-<arch>.tar.gz
cd cube-sandbox-one-click-<version>-<arch>
cp env.example .env
```

Edit `.env` to set the compute role, control-plane IP, S3 credentials, and image reference (for full options and how to copy `CUBE_S3_*` from the control node, see [Multi-Node Cluster](./multi-node-deploy.md)):

```bash
ONE_CLICK_DEPLOY_ROLE=compute
CUBE_SANDBOX_NODE_IP=10.0.0.11
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.10

# Match your prepared image source (either MIRROR=cn or an explicit internal registry reference)
# MIRROR=cn
# CUBE_SANDBOX_CUBE_EGRESS_IMAGE=harbor.internal/cube/cube-egress:<tag>
```

Run the compute-node installer:

```bash
./install-compute.sh
```

### 2.4 Verify Installation and Create Your First Offline Template {#verify-and-create-template}

1. **Run the built-in end-to-end smoke check:**

From the extracted bundle directory on the control node:

```bash
./smoke.sh
```

Once verified, the services are available at:
- **Web Console**: `http://<control-node-ip>:12088`
- **E2B-compatible API**: `http://<control-node-ip>:3000` (see [Quick Start — Step 4](./quickstart.md#step-4-run-your-first-agent-code))

2. **Create a sandbox template from the internal registry (optional):**

Reference the template source image pushed in [1.5](#prepare-template-images) (see [Quick Start — Step 3](./quickstart.md#step-3-create-a-template)):

```bash
cubemastercli tpl create-from-image \
  --image <internal-registry>/cube/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Two common internal-registry details:
- **Authenticated registry**: pass `--registry-username <user> --registry-password <pass>`.
- **Plain HTTP registry (no TLS)**: the image reference must start with `http://`, for example `http://harbor.internal:5000/ns/app:tag`.

> If you do not run an internal registry and want to build templates directly from a local image, see the [run-code-env-inheritance example](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/run-code-env-inheritance/README.md).

### 2.5 Offline Upgrades and Uninstall {#upgrade-and-uninstall}

- **Offline upgrade**: upgrading requires **a new release bundle + a new image set** (component image tags advance with each release). Repeat [1.2](#download-artifacts) and [1.4](#prepare-container-images) for the target version, then run from the new bundle directory:

  ```bash
  ./install.sh --mode=upgrade
  ```

  > For `.env` three-way merge semantics and toggle behavior during upgrades, see [one-click README — Target Machine Installation](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/one-click/README.md).

- **Uninstall**: identical to an online deployment:

  ```bash
  /usr/local/services/cubetoolbox/scripts/one-click/uninstall.sh
  ```

---

## 3. Troubleshooting

| Symptom | Likely Cause | Fix |
| --- | --- | --- |
| `pull failed for <ref> and no local copy is cached` | The loaded image name/tag does not match what the installer resolves (wrong registry variant or tag) | Compare `docker images` against the bundle's defaults (`env.example` or the `grep` command in [1.4](#prepare-container-images)); verify `MIRROR` and override variables in `.env` |
| `docker` missing; installer runs `apt-get` / `yum install docker` and fails | Internal OS mirror does not carry Docker and it was not pre-installed | Add Docker to the internal mirror, or pre-install Docker Engine + Compose per [1.3](#preinstall-os-packages) |
| Installer aborts over Snap Docker | `snap install docker` confinement cannot read `/usr/local/services` | Run `sudo snap remove docker`, then install `docker-ce` / `docker.io` via OS packages |
| Installer aborts while installing `dnsmasq` | Host lacks `systemd-resolved` and the internal mirror cannot install `dnsmasq` | Add `dnsmasq` to the internal mirror or pre-install it offline; or use an OS with `systemd-resolved` |
| Template build stuck in `phase: PULLING` | The `--image` reference is missing from the internal registry, or `CubeMaster` / `CubeTemplateCenter` cannot reach it | Confirm the image was pushed and the reference is exact; include the `http://` prefix for plain HTTP registries; pass `--registry-username` / `--registry-password` if auth is required |
