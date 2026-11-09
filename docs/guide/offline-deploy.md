# Offline Deployment

> **When to use this guide:** your target nodes **have no public internet access** (intranet data centers, isolated network segments, restricted environments), and you need to deploy CubeSandbox offline. Such nodes usually sit on an intranet with mirror sources configured.

**Offline deployment works.** Prepare the items below, then install as you normally would. The install steps are identical to a normal deployment.

| What to prepare | How | Where |
| --- | --- | --- |
| The bundle (plus the optional PVM kernel package) | Download from Releases on a public-network machine, copy it over | Step 1 |
| OS packages (Docker, Compose, …) | Skip if an **internal mirror** exists; manual install only without one | Step 2 |
| Container images (**the core**) | Pull and pack on a public-network machine then `docker load`, or push to an **internal registry** | Step 3 |
| Template source images (optional) | Push to the **internal registry**, reference it when creating templates | Step 5 |

With the first three prepared, install via Step 4; push the template source images after the install (Step 5).

::: tip Scope
This guide covers the **one-click deployment** (single-node, bare-metal, and multi-node cluster). Two deployment paths are out of scope:

- **Terraform Tencent Cloud deployer**: it creates cloud resources and needs Tencent Cloud API access.
- **Kubernetes (Helm) deployment**: the cluster must reach a container registry holding the component images. With an internal registry (e.g. Harbor), follow the [Kubernetes deployment guide](./kubernetes/) and point the chart at it.
:::

::: details Why offline works (the mechanics)
The one-click installer supports offline installs natively:

- Pre-loaded container images are used as-is — no script changes needed.
- The bundle ships every version-locked artifact (component binaries, `vmlinux`, guest image, `mkcert`).
- A normal deployment needs the **public internet** for exactly three things: the bundle, missing OS packages, and container images. They map to Steps 1–3 — whatever your internal mirrors cover stays on the intranet; prepare the rest in advance.
:::

## Before you start: host requirements

The host requirements are identical to a normal deployment (see [Quick Start — Prerequisites](./quickstart.md#prerequisites)):

- x86_64 or aarch64 Linux, glibc ≥ 2.31, root access
- At least 8 GB RAM (a hard installer check — the install aborts below it)
- XFS at `/data/cubelet`, at least 50 GB
- KVM available: native on bare metal; x86_64 cloud VMs can install the PVM kernel first (Step 1, no aarch64 support — ARM machines must already have KVM)

## Step 1: Download the release artifacts

**What to do:** on a public-network machine, download the artifacts below and copy them to the target node. An internal artifact relay or a mirrored Releases channel works just as well.

1. **The one-click bundle**: `cube-sandbox-one-click-<version>-<arch>.tar.gz`. Pick the `<arch>` that matches the target node (`amd64` / `arm64`); for download channels see [Downloads & Releases](./downloads.md).
2. **The PVM host kernel package** (rpm/deb): only if the target is a cloud VM without `/dev/kvm`. Download it from the newest `kernel-release-*` release, install and reboot per [PVM Deployment — Step 1](./pvm-deploy.md#step-1-install-the-pvm-host-kernel), then come back to this guide.

Move the files to the target node with scp, a USB drive, or similar.

## Step 2: Pre-install the OS packages (no internal mirror only)

**Have an internal OS mirror? Skip to Step 3.**

Only a machine with no internal mirror at all needs this step: download the packages below on a public-network machine, copy them over, and install.

::: details Package list (click to expand)

Before skipping, confirm the mirror carries Docker and Compose — many internal mirrors omit them.

**Every node needs:**

- **Docker Engine and Compose.** As long as both `docker` and `docker compose version` (or `docker-compose`) work, `install.sh` skips the online install. Verify after installing:

  ```bash
  docker version && docker compose version   # or: docker-compose --version
  ```

  > ⚠️ Snap Docker is rejected (it cannot read `/usr/local/services`). Install docker-ce / docker.io from packages instead.

- Basic commands: `tar`, `ss`, `ip` (`iproute2`), `bash`, `curl`, `grep`, `sed`, `pgrep` (`procps`), `awk`, `date`, `openssl`
- `e2fsprogs` + `util-linux`: the default `cubecow` storage backend needs `mkfs.ext4`, `mount`, `umount`, `losetup`

**Only when applicable:**

- `dnsmasq`: required when the host has no `systemd-resolved`/`resolvectl` — otherwise the DNS fallback installs it automatically
- `s3fs` + `jq`: S3 volume mount dependencies. The installer tries to auto-install them; a failure stays silent until volume attach — install them in advance
- `python3`, `nvme-cli`, `libaio`, `libnuma`, `libuuid`: only if you enable CubeS3lvol (`ONE_CLICK_ENABLE_S3LVOL=1`)
- `logrotate`: add it if your minimal image does not ship it

:::

## Step 3: Pre-load the container images

**What to do:** get the images from Step 3.1 onto the target node's local Docker. Pick either path:

- **Hand-carry per node (the example below)**: on a public-access machine of the **same architecture** as the target, `docker pull` → `docker save` → `docker load` on the target.
- **Via an internal registry (recommended for many nodes)**: push the images to the internal registry (e.g. Harbor) and point the override variables at it. The installer pulls from there directly — no per-node loading.

Commands for the hand-carry path:

```bash
# Public-network machine: pull every image from the list
docker pull <image-ref>

# Pack per node role (keeps transfers small)
docker save -o cube-images-control.tar <image-1> <image-2> ...

# Target node
docker load -i cube-images-control.tar
```

### 3.1 Which images each node needs

**Control node** (the default all-in-one `install.sh`, or every control node of a cluster):

| Image | Default reference | Override variable |
| --- | --- | --- |
| MySQL | `cube-sandbox-image.tencentcloudcr.com/opensource/mysql:8.0` | `CUBE_SANDBOX_MYSQL_IMAGE` |
| Redis | `cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine` | `CUBE_SANDBOX_REDIS_IMAGE` |
| MinIO (S3 backend, on by default) | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/minio:<release>` | `CUBE_SANDBOX_MINIO_IMAGE` |
| CoreDNS | `cube-sandbox-image.tencentcloudcr.com/opensource/coredns/coredns:1.14.2` | `CUBE_PROXY_COREDNS_IMAGE` |
| WebUI (nginx) | `cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat` | `WEB_UI_IMAGE` |
| cube-proxy | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-proxy:<tag>` | `CUBE_SANDBOX_CUBE_PROXY_IMAGE` |
| cube-lifecycle-manager | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-lifecycle-manager:<tag>` | `CUBE_SANDBOX_CUBE_LCM_IMAGE` |
| cube-egress | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress:<tag>` | `CUBE_SANDBOX_CUBE_EGRESS_IMAGE` |

**Compute node** (`install-compute.sh`):

| Image | Default reference | Notes |
| --- | --- | --- |
| cube-egress | same as above | mandatory — the egress MITM proxy runs on every node |
| JuiceFS mount | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/juicefs:ce-v1.4.1` | only if `ONE_CLICK_ENABLE_JFS=1`; `MIRROR=cn` selects the `-cn` host |

The `<tag>` / `<release>` placeholders for cube-proxy / cube-lifecycle-manager / cube-egress / MinIO resolve with this command, run from the extracted bundle root:

```bash
grep -rhE '_IMAGE.*_DEFAULT="' scripts/
```

### 3.2 The reference must match exactly

- If you pulled the `-cn` references, set `MIRROR=cn` in `.env`; if you pulled `-int`, leave `MIRROR` unset.
- For an internal registry or your own image names, point the override variables above at them in `.env`.

### 3.3 Verify on the target node

```bash
docker images   # every reference from the list must appear, same name and tag
```

## Step 4: Install — exactly like a normal deployment

**What to do:** install exactly as you would online, except you run the transferred bundle instead of the online installer command.

**Every command must run as root.** Switch to root first:

```bash
sudo su root
```

Extract the bundle and install:

```bash
tar -xzf cube-sandbox-one-click-<version>-<arch>.tar.gz
cd cube-sandbox-one-click-<version>-<arch>
cp env.example .env
# edit .env if needed (node IP, external MySQL/Redis, MIRROR, image overrides…)
./install.sh
```

Pick your scenario:

- **Cloud VM without `/dev/kvm` (PVM):** follow [Quick Start](./quickstart.md), swapping only its Step 2 (online install) for the bundle command above — everything else stays as written.
- **Bare metal / machines with KVM:** see [Bare-Metal Deployment](./bare-metal-deploy.md).
- **Adding compute nodes:** see [Multi-Node Cluster](./multi-node-deploy.md). The bundle is reused as-is; a compute node only needs the cube-egress image pre-loaded (Step 3.1).

After the install, verify as usual:

```bash
./smoke.sh
```

The dashboard is at `http://<target-host>:12088` and the E2B-compatible API is on port `3000` ([Quick Start — Step 4](./quickstart.md#step-4-run-your-first-agent-code)).

::: tip What the logs look like offline
When the registry is unreachable, the three component images each log `WARN: pull failed but local image exists; using cached copy` and use your pre-loaded image; the other services use theirs silently. **All expected, not errors.** Only `pull failed … and no local copy is cached` means an image was never loaded — go back to Step 3.
:::

## Step 5 (after install, optional): Push template source images into the internal registry

**What to do:** if you want to create templates offline, push the template source images to your **internal registry**, then reference that registry when creating templates.

Why: CubeMaster pulls the source image from a registry when building a template — an internal registry is all it needs, no public internet. Multi-node clusters need nothing extra: CubeMaster distributes artifacts over the intranet; compute nodes never pull the source image.

Two steps:

1. Push the source image into the internal registry. Pull and pack it on a public-network machine, then transfer it to a machine that can reach the internal registry (the control node, for example), and load / tag / push there — the official code-interpreter image for example:

   ```bash
   # Public-network machine
   docker pull cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
   docker save -o sandbox-code.tar cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

   # Machine that can reach the internal registry
   docker load -i sandbox-code.tar
   docker tag cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
     <internal-registry>/cube/sandbox-code:latest
   docker push <internal-registry>/cube/sandbox-code:latest
   ```

2. Create the template with the internal registry reference (see [Quick Start — Step 3](./quickstart.md#step-3-create-a-template)):

   ```bash
   cubemastercli tpl create-from-image \
     --image <internal-registry>/cube/sandbox-code:latest \
     --writable-layer-size 1G \
     --expose-port 49999 \
     --expose-port 49983 \
     --probe 49999
   ```

Two details:

- If the internal registry requires authentication, add `--registry-username` / `--registry-password`.
- If it serves plain HTTP (no TLS), the reference must carry an `http://` prefix, e.g. `http://harbor.internal:5000/ns/app:tag`.

> You can also skip the internal registry and use a local image — full recipe in the [run-code-env-inheritance example](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/run-code-env-inheritance/README.md).

## Upgrades, template builds, and uninstall

- **Upgrading = a new bundle + a new image set** (tags advance with each release). Repeat Step 1 and Step 3 for the new version, then run `./install.sh --mode=upgrade` (see [one-click README — Target Machine Installation](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/one-click/README.md)).
- **Building custom template images offline:** a Dockerfile build usually needs `apt`/`pip`. Point those at your internal mirrors, or bake a complete image while still on a public network and push it into the internal registry as in Step 5 (recipes in [Local & Remote Image Practice — Troubleshooting](./tutorials/template-build-practice.md#troubleshooting)).
- **Uninstall** is unchanged: `/usr/local/services/cubetoolbox/scripts/one-click/uninstall.sh`.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `pull failed for <ref> and no local copy is cached` | The loaded image reference does not match what the installer resolves (wrong tag or registry variant) | Compare `docker images` with the bundle's default references (`env.example`, or the grep output from Step 3.1); check `MIRROR` consistency (Step 3.2) |
| `docker` missing; installer runs `apt-get/yum install docker` and fails | The internal mirror does not carry Docker, and it was not pre-installed | Add Docker to the internal mirror, or pre-install Docker Engine + Compose (Step 2) |
| Installer aborts over snap Docker | `snap install docker` sandboxing cannot read `/usr/local/services` | `sudo snap remove docker`, install docker-ce from packages |
| Install aborts installing `dnsmasq` | Host lacks `systemd-resolved` and the internal mirror cannot install `dnsmasq` | Add `dnsmasq` to the internal mirror or pre-install it; or use an OS with `systemd-resolved` |
| Template build stuck in `phase: PULLING` | The `--image` reference is missing from the internal registry, or CubeMaster cannot reach it | Confirm the image was pushed and the reference is right; check the `http://` prefix for HTTP registries; add `--registry-username` / `--registry-password` if the registry requires auth |
