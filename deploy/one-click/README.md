# Cube Sandbox One-Click

This directory is used to build and deliver the single-machine one-click release package for `cube-sandbox`.

## Directory Overview

- `build-release-bundle-builder.sh`: Recommended entry point. Compiles the components needed by one-click inside a builder image, then continues the release package assembly on the host machine.
- `build-vm-assets.sh`: Builds `containerd-shim-cube-rs`, `cube-runtime`, and `cube-agent`; injects `cube-agent` into the guest image as `/sbin/init`; and collects the guest kernel.
- `build-release-bundle.sh`: Low-level packaging entry point. Consumes either the source tree or `ONE_CLICK_*_BIN` pre-built artifacts, assembles `sandbox-package`, and produces the final release package.
- `config-cube.toml`: Default one-click runtime configuration template.
- `support/`: `docker compose` templates for MySQL/Redis, installed to `/usr/local/services/cubetoolbox/support/` on the target machine; `support/bin/mkcert` is the bundled mkcert binary.
- `cubeproxy/`: Compose template, `global.conf` template, and CoreDNS template for `cube proxy`.
- `webui/`: Nginx runtime files for the dashboard, installed to `/usr/local/services/cubetoolbox/webui/` on the target machine.
- `install.sh`: Entry point for installing and starting the control node on the target machine (defaults to all-in-one mode).
- `install-compute.sh`: Entry point for installing a compute node on the target machine.
- `down.sh`: Stops the services and dependencies installed by one-click.
- `smoke.sh`: Runs basic health checks.
- `env.example`: Shared environment variable template for both the build machine and the target machine.
- `lib/common.sh`: Common shell utility functions.
- `scripts/one-click/`: Validation and maintenance helpers used by the systemd-managed deployment after installation.
- `terraform/tencentcloud/`: Terraform deployer for a **clustered** CubeSandbox on Tencent Cloud (TKE control plane + CVM compute nodes). `create.sh` is the entry point; `destroy.sh` tears everything down. These files are shipped both at the release-bundle top level and inside `sandbox-package` (see "Tencent Cloud Cluster Deployment").

## Build Inputs

The required fixed kernel artifact is the ordinary guest kernel `vmlinux`. A PVM guest kernel can also be packaged as `vmlinux-pvm`:

- `vmlinux`
- `vmlinux-pvm` (optional)

By default they are placed under `assets/kernel-artifacts/`, but can be overridden via environment variables:

```bash
export ONE_CLICK_CUBE_KERNEL_VMLINUX=/abs/path/to/vmlinux
export ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX=/abs/path/to/vmlinux-pvm
```

The installed runtime still uses `cube-kernel-scf/vmlinux` as the active guest kernel path. The package stores the ordinary guest kernel as `vmlinux-bm` and keeps `vmlinux` as a symlink: by default it points to `vmlinux-bm`; if the target machine sets `CUBE_PVM_ENABLE=1` during installation, the installer points it to `vmlinux-pvm`.

The guest image no longer depends on a local zip file. Instead, it is generated locally from `deploy/guest-image/Dockerfile` during the one-click release package build. Common override parameters:

```bash
export ONE_CLICK_GUEST_IMAGE_DOCKERFILE=/abs/path/to/cube-sandbox/deploy/guest-image/Dockerfile
# Optional; defaults to the directory containing the Dockerfile
export ONE_CLICK_GUEST_IMAGE_CONTEXT_DIR=/abs/path/to/cube-sandbox/deploy/guest-image
# Optional; defaults to cube-sandbox-guest-image:one-click
export ONE_CLICK_GUEST_IMAGE_REF=cube-sandbox-guest-image:one-click
# Optional; defaults to the current repository revision
export ONE_CLICK_GUEST_IMAGE_VERSION=custom-guest-image-version
```

## Building the Release Package

It is recommended to copy the environment template first:

```bash
cd deploy/one-click
cp env.example .env
```

Run the following from the repository root on the host machine (recommended):

```bash
./deploy/one-click/build-release-bundle-builder.sh
```

This entry point will:

- Compile `cubemaster`, `cubemastercli`, `cubelet`, `cubecli`, `cube-api`, `network-agent`, `cube-agent`, `containerd-shim-cube-rs`, and `cube-runtime` inside a container using the root-level builder image.
- Run `go mod download` for `CubeMaster` and `Cubelet` inside the builder. The first build will fetch Go modules online; subsequent builds reuse the module cache under the builder's HOME directory.
- Place the pre-built artifacts in `deploy/one-click/.work/prebuilt/`.
- Return to the host machine and call `build-release-bundle.sh` to build the WebUI static assets, continue with guest image generation, and finish final packaging.

If the build machine already has a complete toolchain, or you want to specify `ONE_CLICK_*_BIN` manually, you can invoke the low-level entry point directly:

```bash
./deploy/one-click/build-release-bundle.sh
```

Regardless of which entry point is used, `CubeMaster` / `Cubelet` no longer depend on the `vendor/` directory in the repository; dependencies are resolved at build time via Go modules.

The WebUI build runs on the build machine during final packaging and requires `npm`. The target machine does not build a WebUI image; it mounts the packaged `webui/dist` directory into a standard nginx container. To reuse an already built dashboard, set:

```bash
export ONE_CLICK_WEB_DIST_DIR=/abs/path/to/web/dist
```

### Go Modules Dependency Download

- `go mod download` is executed the first time `CubeMaster` and `Cubelet` are built.
- The build machine must be able to reach the relevant module sources. If you are behind a private network, configure `GOPROXY`, `GOPRIVATE`, and private repository credentials in advance.
- The recommended entry point persists the builder HOME to a host-side cache directory, so subsequent builds on the same machine typically do not require a full re-download.
- `cubelog` is still referenced as a local module via `../cubelog` and is not downloaded from a remote source.

On success, the following file will be generated:

```bash
deploy/one-click/dist/cube-sandbox-one-click-<version>.tar.gz
```

The release package contains:

- `sandbox-package.tar.gz`
- `release-manifest.json`
- `CubeAPI/bin/cube-api`
- `containerd-shim-cube-rs`, `cube-runtime`
- Locally built `cube-image/cube-guest-image-cpu.img`
- `cubeproxy/` directory and its build context
- `support/` directory and its compose templates
- `webui/` directory, its compose template, nginx configuration, and built `web/dist` assets
- `cube-kernel-scf.zip` packaged on the fly from the ordinary/PVM guest kernel artifacts
- `install.sh` / `install-compute.sh` / `down.sh` / `smoke.sh` ready to run on the target machine

During installation, the top-level `release-manifest.json` is copied to:

```bash
/usr/local/services/cubetoolbox/release-manifest.json
```

When `VERSION.txt` declares `manifest=release-manifest.json`, `install.sh`
validates that the manifest is present and parseable before it starts replacing
the existing installation.

## Configuration Mapping

One-click does not create an extra global `configs/` layer on the target machine; instead, files are placed directly into each component's native configuration paths:

- `configs/single-node/cubemaster.yaml` → `CubeMaster/conf.yaml`
- `Cubelet/config/` → `Cubelet/config/`
- `Cubelet/dynamicconf/` → `Cubelet/dynamicconf/`
- `configs/single-node/network-agent.yaml` → `network-agent/network-agent.yaml`
- `CubeAPI/bin/cube-api` → `/usr/local/services/cubetoolbox/CubeAPI/bin/cube-api`
- `support/` → `/usr/local/services/cubetoolbox/support/`
- `cubeproxy/` → `/usr/local/services/cubetoolbox/cubeproxy/`
- `webui/` → `/usr/local/services/cubetoolbox/webui/`

`Cubelet` uses the existing `dynamicconf/conf.yaml` from the repository as-is. At runtime, `network-agent` preferentially reads the network plugin configuration from `Cubelet/config/config.toml` via `--cubelet-config` to stay consistent with `Cubelet`'s network parameters. `cube-api` reads environment variables directly from `.one-click.env` on startup, listening on `0.0.0.0:3000` by default and forwarding to the local `cubemaster`. MySQL/Redis are always deployed to `/usr/local/services/cubetoolbox/support` and run in Docker containers managed by dedicated systemd services on the target machine. `cube proxy` is always deployed to `/usr/local/services/cubetoolbox/cubeproxy`, built locally from the bundled build context, and managed by systemd. WebUI is deployed to `/usr/local/services/cubetoolbox/webui`, listens on `12088` by default, serves the packaged `webui/dist` directory through a standard nginx container, and proxies `/cubeapi` to CubeAPI through Docker `host-gateway` under systemd management.

## Target Machine Installation

After copying `cube-sandbox-one-click-<version>.tar.gz` to the target machine:

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
sudo ./install.sh
```

The one-click installation path is fixed at `/usr/local/services/cubetoolbox`.

New one-click installations are managed by systemd only:

- control node: `cube-sandbox-control.target`
- compute node: `cube-sandbox-compute.target`

The installer copies the unit files into `/etc/systemd/system/` and runs `enable --now` for the selected role automatically. Legacy shell up/down scripts are kept only as a short-term upgrade bridge for older pre-systemd installs and are not part of the runtime interface for new installations.

Common commands:

```bash
sudo ./smoke.sh
sudo ./down.sh
```

After a control-node installation, open the dashboard at:

```bash
http://<target-host>:12088
```

Before installation, you can explicitly set the current node's internal IP in `.env`. If not set, `install.sh` will attempt to auto-detect the IPv4 address of `eth0`:

```bash
# CUBE_SANDBOX_NODE_IP=10.0.0.10
```

If `CUBE_SANDBOX_NODE_IP` is explicitly set, the installation script will use that value directly; otherwise, the auto-detected node IP is persisted in the runtime environment and used to render `cube proxy` / DNS addresses.

### Digital Assistant Environment Variables

The Digital Assistant (AgentHub) uses MySQL through CubeAPI to persist assistant instances, snapshots, templates, and operation history. In one-click deployments, `DATABASE_URL` is generated automatically from `CUBE_SANDBOX_MYSQL_HOST`, `CUBE_SANDBOX_MYSQL_PORT`, `CUBE_SANDBOX_MYSQL_USER`, `CUBE_SANDBOX_MYSQL_PASSWORD`, and `CUBE_SANDBOX_MYSQL_DB` when it is not set explicitly:

```bash
# Optional; generated by one-click when omitted.
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

Before creating or reconfiguring OpenClaw-based digital assistants, configure the LLM API key (and provider, base URL, model) on the **AgentHub settings** page in the WebUI.

### Compute Node Installation

If the first machine has already been deployed as a combined control + compute node, the same release package can be reused on a second machine as a compute-only node:

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
```

Set at minimum the following in `.env`:

```bash
ONE_CLICK_DEPLOY_ROLE=compute
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.11
```

If you need to explicitly specify the compute node IP, or if the default NIC on the target machine is not `eth0`, also set:

```bash
CUBE_SANDBOX_NODE_IP=10.0.0.12
```

Then run:

```bash
sudo ./install-compute.sh
```

In compute node mode, the installer will:

- Install `Cubelet`, `network-agent`, `cube-shim`, `cube-image`, `cube-kernel-scf`, `cube-egress`, the required scripts, and `docker`.
- Start `network-agent` and `cubelet`, and bring up `cube-egress` via `cube-sandbox-compute.target` (the transparent egress MITM proxy, run as a docker container, which enforces per-sandbox egress policy).
- Before `cube-egress` starts, pull the MITM root CA (cert + key) from the control node's `/cube/ca/<file>` endpoint so it matches the CA baked into templates — templates then trust the leaf certs the compute-node `cube-egress` signs.
- Point `Cubelet`'s `meta_server_endpoint` to `ONE_CLICK_CONTROL_PLANE_IP:8089`.
- Automatically register the node via the control node's `/internal/meta` API.

Notes:

- All compute nodes must have `Cubelet` listening on the same gRPC port as configured on the control node (default `9999`).
- `CUBE_SANDBOX_NODE_IP` is used both as the one-click configuration value and as the `Cubelet` node registration IP.
- The control node must be able to reach port `9999/tcp` on all compute nodes; compute nodes must be able to reach port `8089/tcp` on the control node.

MySQL/Redis dependencies are deployed by default to:

```bash
/usr/local/services/cubetoolbox/support
```

During installation, runtime files are prepared in this directory and the following containers are managed individually by systemd:

- `mysql:8.0`
- `redis:7-alpine`

### Using an external MySQL / Redis

To point CubeSandbox at an existing MySQL/Redis server instead of the bundled
local containers, set the following in `.env` before running `install.sh`
(see `env.example`):

```bash
# External MySQL (any subset of the credential fields may be overridden)
CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
CUBE_EXTERNAL_MYSQL_PORT=3306
CUBE_EXTERNAL_MYSQL_USER=cube
CUBE_EXTERNAL_MYSQL_PASSWORD=cube_pass
CUBE_EXTERNAL_MYSQL_DB=cube_mvp

# External Redis
CUBE_EXTERNAL_REDIS_HOST=10.0.0.21
CUBE_EXTERNAL_REDIS_PORT=6379
CUBE_EXTERNAL_REDIS_PASSWORD=ceuhvu123
```

When `CUBE_EXTERNAL_MYSQL_HOST` (and/or `CUBE_EXTERNAL_REDIS_HOST`) is set, `install.sh`:

- patches `CubeMaster/conf.yaml` with the external MySQL/Redis endpoint;
- writes `DATABASE_URL` (CubeAPI) and `CUBE_PROXY_REDIS_*` (cube proxy) to `.one-click.env` so every service consumes the external endpoint;
- masks the corresponding `cube-sandbox-mysql.service` / `cube-sandbox-redis.service` so the local container is never started; and
- makes `quickcheck.sh` and `up-support.sh` skip lifecycle management of the now-external dependency. (`down-support.sh` has no external-dep awareness and still issues a `docker compose down`, but this is a harmless no-op because the local containers were never started for the external dependency.)

The external MySQL must already grant the configured user access to the target
database. CubeMaster runs its own embedded schema migrations on first start.

`cube proxy` and its DNS resolution are mandatory capabilities in one-click. The following two values in `.env` must remain `1`:

```bash
CUBE_PROXY_ENABLE=1
CUBE_PROXY_DNS_ENABLE=1
```

Other common parameters:

```bash
CUBE_PROXY_HTTPS_PORT=443
CUBE_PROXY_HTTP_PORT=80
# Deprecated: CUBE_PROXY_HOST_PORT is ignored; configure CUBE_PROXY_HTTP_PORT instead.
CUBE_PROXY_CERT_DIR=/usr/local/services/cubetoolbox/cubeproxy/certs
CUBE_PROXY_DNS_ANSWER_IP="${CUBE_SANDBOX_NODE_IP}"
WEB_UI_ENABLE=1
WEB_UI_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat
WEB_UI_HOST_PORT=12088
WEB_UI_UPSTREAM=http://host.docker.internal:3000
CUBE_API_BIND=0.0.0.0:3000
CUBE_API_HEALTH_ADDR=127.0.0.1:3000
CUBE_API_SANDBOX_DOMAIN=cube.app
```

During installation, the following steps are performed:

- If `mkcert` is not already installed on the system, it is copied from the bundled `support/bin/mkcert` to `/usr/local/bin/mkcert`. Then `mkcert -install` is run on the host under `CUBE_PROXY_CERT_DIR` (default `/usr/local/services/cubetoolbox/cubeproxy/certs/`) to generate `cube.app+3.pem` and `cube.app+3-key.pem`.
- Runtime configuration and rendered files are prepared under `/usr/local/services/cubetoolbox/support/`, `cubeproxy/`, `coredns/`, and `webui/`.
- `cubeproxy/global.conf` is rendered using `CUBE_SANDBOX_NODE_IP`.
- `cube-sandbox-*.service|target|timer` unit files are installed under `/etc/systemd/system/`, and both host processes and Docker containers are managed uniformly by systemd.
- MySQL, Redis, cube proxy, WebUI, and CoreDNS still run in Docker, but their lifecycle is managed directly by dedicated systemd services instead of relying on runtime `docker compose up -d`.
- If `resolvectl` is available, one-click creates a dedicated dummy link (default `cube-dns0`) with a local address, binds CoreDNS to `169.254.254.53` on that link by default, and routes `cube.app` through the link without affecting the host's default public DNS path. If `resolvectl` is unavailable on the target machine, the installer falls back to `NetworkManager + dnsmasq`: it still creates the same dummy link, asks `dnsmasq` to additionally listen on `169.254.254.53`, takes `/etc/resolv.conf` ownership away from NetworkManager (`rc-manager=unmanaged`) and rewrites it to point at the same non-loopback IP. This keeps the host resolver symmetrical with the `systemd-resolved` path and avoids the Docker daemon's silent fallback to public DNS (`8.8.8.8`) that happens when `/etc/resolv.conf` contains only loopback nameservers — without it, every container on the host (including `docker build`'s `apk update` step) ends up using DNS servers that internal machines cannot reach.
- Host processes `network-agent`, `cubemaster`, `cube-api`, and `cubelet` are started through systemd, and `quickcheck.sh` verifies both unit state and service health.
- A standard WebUI nginx container is started under `/usr/local/services/cubetoolbox/webui/`. It mounts `webui/dist` as read-only static content, publishes `WEB_UI_HOST_PORT` (`12088` by default), maps `host.docker.internal` to Docker `host-gateway`, and verifies `/cubeapi/v1/health` through the nginx reverse proxy.

Stopping one-click will simultaneously stop MySQL/Redis under `/usr/local/services/cubetoolbox/support`, WebUI, `cube proxy` / `CoreDNS`, and the host processes `network-agent` / `cubemaster` / `cube-api` / `cubelet`, and will roll back the host DNS routing configuration for `cube.app`.

After deployment, to point the E2B official SDK to the one-click node, set the following on the client side:

```bash
export E2B_API_URL=http://<target-host>:3000
export E2B_API_KEY=e2b_000000
```

## Pre-Installation Preflight Checklist

`install.sh` / `install-compute.sh` performs a one-time preflight check early in the startup process to ensure dependencies fail fast rather than partway through.

### Compute Role (`install-compute.sh`)

Required commands:

- `docker` (cube-egress runs as a docker container; the installer installs it automatically — this is a hard prerequisite, so in offline/air-gapped environments where automatic installation isn't possible, install Docker beforehand)
- `tar`
- `ss`
- `bash`
- `curl`
- `grep`
- `sed`
- `pgrep`
- `date`

Conditional commands:

- If `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1` is enabled and `/etc/docker/daemon.json` already exists, `python3` is required.
- If the packaged `Cubelet/config/config.toml` enables `storage_backend = "cubecow"`, one-click also checks:
  `mkfs.ext4`, `mount`, `umount`, `losetup`

Recommended packages to satisfy the cubecow command set:

- Debian / Ubuntu: `e2fsprogs`, `util-linux`
- OpenCloudOS / RHEL / CentOS: `e2fsprogs`, `util-linux`

Example install commands:

```bash
# Debian / Ubuntu
sudo apt-get update
sudo apt-get install -y e2fsprogs util-linux

# OpenCloudOS / RHEL / CentOS
sudo dnf install -y e2fsprogs util-linux || \
sudo yum install -y e2fsprogs util-linux
```

### Control Role (`install.sh`, default)

Required commands:

- `docker`
- `tar`
- `ss`
- `bash`
- `curl`
- `grep`
- `sed`
- `pgrep`
- `date`
- `ip`
- `awk`

One-of-two commands:

- Certificate preparation: `mkcert` (bundled in the release package; auto-installed from the package if not present on the system).
- DNS split routing: `resolvectl`, or `systemctl + NetworkManager`.
- If `dnsmasq` is missing and the `NetworkManager` fallback path is taken, one of the following package managers is also required: `dnf` / `yum` / `apt-get`.

Conditional commands:

- If `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1` is enabled and `/etc/docker/daemon.json` already exists, `python3` is required.
- If the packaged `Cubelet/config/config.toml` enables `storage_backend = "cubecow"`, one-click also checks:
  `mkfs.ext4`, `mount`, `umount`, `losetup`

Recommended packages to satisfy the cubecow command set:

- Debian / Ubuntu: `e2fsprogs`, `util-linux`
- OpenCloudOS / RHEL / CentOS: `e2fsprogs`, `util-linux`

Example install commands:

```bash
# Debian / Ubuntu
sudo apt-get update
sudo apt-get install -y e2fsprogs util-linux

# OpenCloudOS / RHEL / CentOS
sudo dnf install -y e2fsprogs util-linux || \
sudo yum install -y e2fsprogs util-linux
```

## Prerequisites

> **Security**: All core services bind `0.0.0.0` by default. Before deploying on
> a machine reachable from untrusted networks, review the
> [Network Hardening Guide](https://cubesandbox.com/guide/network-hardening.html) for bind-address
> configuration, firewall rules, and credential rotation.

- The target machine requires `root` privileges.
- The target machine preferentially uses `systemd-resolved` / `resolvectl` for split DNS of `cube.app`. The current implementation creates a dedicated dummy link (default `cube-dns0`), assigns it a local `/32` address, binds CoreDNS to `169.254.254.53` on that link by default, and attaches that address plus `~cube.app` to the link. If that capability is unavailable, the installation script will fall back to `NetworkManager + dnsmasq`: the same dummy link is created and `dnsmasq` is configured (via `listen-address` / `bind-interfaces`) to listen on both `127.0.0.1` and `169.254.254.53`. `/etc/resolv.conf` is then written by the installer (NetworkManager runs with `rc-manager=unmanaged`) to point at `169.254.254.53`, so host applications and Docker containers see the same non-loopback resolver.
- The target machine pulls `mysql:8.0` and `redis:7-alpine` from the internet by default.
- The `mkcert` binary is bundled in the release package (`support/bin/mkcert`). If `mkcert` is not pre-installed on the system, it is automatically copied from the package to `/usr/local/bin/mkcert` — no internet download required.
- TLS certificates and private keys for `cube proxy` are stored on the host under `CUBE_PROXY_CERT_DIR` and mounted read-only into the container via `docker compose`. After updating certificates, simply restart `cube-proxy` or reload nginx inside the container — no image rebuild required.
- The recommended entry point `build-release-bundle-builder.sh` requires the host machine to have `docker`, `make`, `tar`, `python3`, `truncate`, `ldd`, `mkfs.ext4`, and similar tools.
- The recommended entry point only runs component compilation inside the builder; guest image generation and final packaging are still performed on the host machine.
- If invoking the low-level entry point `build-release-bundle.sh` directly, the build machine must also have local toolchains such as `go`, `cargo`, and `make` installed, depending on the build mode.
- If using the low-level entry point directly or running the recommended entry point for the first time, the build machine must be able to download Go modules from the internet. Configure a usable `GOPROXY` in advance for restricted network environments.
- If the VM path is enabled, the target machine must still satisfy the runtime permission requirements for `network-agent`, tap interfaces, routing, etc.

## Known Limitations

- If `vmlinux` is missing from `assets/kernel-artifacts/`, `build-vm-assets.sh` and `build-release-bundle.sh` will fail immediately. `vmlinux-pvm` is optional at build time, but installation with `CUBE_PVM_ENABLE=1` requires it to be present in the package. The installed `cube-kernel-scf/vmlinux` path is an active symlink to `vmlinux-bm` or `vmlinux-pvm`. The `cube-kernel-scf.zip` in the release package is generated automatically during the packaging phase.
- If the `deploy/guest-image/Dockerfile` build fails, or the build machine's `mkfs.ext4` does not support the `-d` flag, guest image generation will fail immediately.
- `cube-snapshot/spec.json` is not a mandatory artifact in the current first release of one-click. If absent, the related plugin degrades to a warning rather than blocking the basic startup.
- If the target machine has neither `systemd-resolved` / `resolvectl` nor a restartable `NetworkManager`, one-click will currently report an error, as a third host DNS solution for such environments has not yet been integrated.

## DNS Troubleshooting

- Inspect the current split-DNS state: `resolvectl status`
- Verify the host stub resolver path: `dig +tcp +timeout=3 docker.cnb.cool @127.0.0.53`
- Verify the local CoreDNS path: on both the `systemd-resolved` path and the `NetworkManager + dnsmasq` fallback path, the client entry point is the same dummy-link IP, so run `dig +tcp +timeout=3 foo.cube.app @169.254.254.53`. CoreDNS itself stays bound to `127.0.0.54` internally; only the `systemd-resolved` path talks to CoreDNS directly, while the fallback path goes through `dnsmasq`.
- Verify the host stub resolver path also routes through the new entry point: `cat /etc/resolv.conf` should show `nameserver 169.254.254.53` on both paths.
- Verify the container view: `docker run --rm alpine cat /etc/resolv.conf` should also show `nameserver 169.254.254.53`. If it shows `nameserver 8.8.8.8` instead, the host's `/etc/resolv.conf` regressed to a loopback nameserver and Docker fell back to its built-in public DNS.
- On the `systemd-resolved` path, the local CoreDNS address should appear only on the dedicated dummy link, not on the default network interface.

## Tencent Cloud Cluster Deployment (Terraform)

In addition to the single-machine `install.sh`, the release bundle ships a
Terraform-based deployer that stands up a **clustered** CubeSandbox on Tencent
Cloud: a managed TKE control plane running `cubemaster` / `cube-api` /
`cube-proxy` / `cube-webui`, backed by cloud MySQL + Redis, with one or more CVM
PVM compute nodes. A jumpserver (SSH on port `443`) is the build host and bastion
for the otherwise-private VPC.

`cubemaster` runs multiple replicas that share their `/data/CubeMaster/storage`
directory through a CFS (Cloud File Storage, "通用标准型" / General Standard) NFS
share mounted ReadWriteMany — an elastic, pay-as-you-go file system provisioned
before the addons so all replicas read/write the same template / snapshot /
runtime state.

The deployer is surfaced at the **top level** of the extracted bundle, so right
after extracting the package you can run it directly:

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>

export TENCENTCLOUD_SECRET_ID="your-secret-id"
export TENCENTCLOUD_SECRET_KEY="your-secret-key"

./terraform/tencentcloud/create.sh
```

`create.sh` runs entirely from the extracted bundle:

- It auto-detects the local bundle (the outer `cube-sandbox-one-click-<version>.tar.gz`,
  or re-packs the extracted directory if the tarball is gone) and uses it as the
  offline source for component images and compute-node installation. When a local
  bundle is detected or set via `TENCENTCLOUD_LOCAL_BUNDLE=/path/to.tar.gz`, no
  public download is required; otherwise the jumpserver falls back to an **online
  install** (it downloads `online-install.sh` and the package), which needs public
  network access.
- It generates an SSH key pair under `terraform/tencentcloud/.ssh/` if none exists.
- It generates the cube-proxy CLB's TLS certificate (`cube.app` / `*.cube.app`)
  on the jumpserver using the bundled `mkcert` (shipped inside
  `assets/package/sandbox-package.tar.gz`, i.e. `sandbox-package/support/bin/mkcert`
  once that inner package is extracted; the same flow as
  `scripts/one-click/up-cube-proxy.sh`), keeping a copy under
  `/root/cubeproxy-certs` on the jumpserver and downloading it to
  `terraform/tencentcloud/cubeproxy-certs/` for the Secret mount.
- It builds and pushes the four component images to the TCR instance it creates,
  then deploys the TKE addons and the CVM compute nodes. At least one compute node
  is always created by `create.sh`; use `TENCENTCLOUD_COMPUTE_NODE_COUNT` to change
  the count.

cube-webui's nginx config (`webui-nginx.conf`) is not maintained separately: it
is derived from the canonical `deploy/one-click/webui/nginx.conf` (placed there
by the bundle build, or copied by `create.sh` when run from the source tree).

Requirements on the machine running `create.sh`: `ssh`, `scp`, `nc`, and network
access to the Tencent Cloud APIs. `terraform` and `jq` are auto-installed if
missing — `terraform` from the HashiCorp release site (needs `curl`/`wget` +
`unzip`), `jq` from the system package manager or, failing that, a static binary
from GitHub. `mkcert`/`openssl` are not required locally — certificates are
produced on the jumpserver.

Common environment overrides (these match the `create.sh` and `variables.tf`
defaults):

```bash
export TENCENTCLOUD_REGION=ap-guangzhou
export TENCENTCLOUD_COMPUTE_NODE_COUNT=2    # CVM PVM compute nodes (default 1)
export TENCENTCLOUD_TKE_NODE_COUNT=2        # TKE worker nodes (default 2)
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=S5.2XLARGE16
export TENCENTCLOUD_CUBE_IMAGE_TAG=latest   # shared tag for the four images
```

For non-interactive / CI runs, also set these (without a TTY the interactive
menus fall back to defaults, so set them explicitly to stay in control). The
password variables are the exception: a non-interactive run refuses to start
with the built-in, publicly-known demo passwords and requires them to be set —
or set `TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1` to opt into the insecure
defaults for a throwaway sandbox.

```bash
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-3    # else first queried zone / <region>-3
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=S5.2XLARGE16  # else the preferred default
export TENCENTCLOUD_LOCAL_BUNDLE=/path/to/cube-sandbox-one-click-<version>.tar.gz  # auto-detected when run from inside an extracted bundle
export TENCENTCLOUD_PVM_KERNEL_VMLINUX=/path/to/vmlinux-pvm  # only needed if the bundle ships no vmlinux-pvm
export TENCENTCLOUD_MYSQL_PASSWORD=...      # required for non-interactive runs (no insecure fallback)
export TENCENTCLOUD_REDIS_PASSWORD=...      # required for non-interactive runs
export TENCENTCLOUD_CUBE_PASSWORD=...       # required for non-interactive runs
export TENCENTCLOUD_BUILD_IMAGES=0          # reuse already-pushed images
```

Tear everything down with:

```bash
./terraform/tencentcloud/destroy.sh
```

`destroy.sh` also needs `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY` and
reuses the selections saved in `terraform/tencentcloud/.env` from `create.sh`. It
runs without prompting — running `destroy.sh` itself confirms the teardown.

> **⚠ Avoid unexpected billing:** if `destroy.sh` cannot remove every resource
> (for example MySQL/Redis stuck in the recycle bin / isolated state, or
> leftovers Terraform can no longer see), log in to the Tencent Cloud console and
> delete the remaining resources by hand so you are not billed for orphans:
> [VPC / network](https://console.cloud.tencent.com/vpc),
> [MySQL recycle bin](https://console.cloud.tencent.com/cdb/recycle),
> [Redis recycle bin](https://console.cloud.tencent.com/redis/recycle).
> `destroy.sh` also prints these same links when a teardown step fails or a
> recycle-bin cleanup is not confirmed.

The same files are also embedded inside `assets/package/sandbox-package.tar.gz`
(consumed by the jumpserver-side `build_images.sh`); the top-level copy simply
makes the deployer reachable without first extracting the inner package.

### Environment requirements & how Terraform is used

`create.sh` drives Terraform from your local machine; you do not need a
pre-installed Terraform:

- **Credentials:** export `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY`
  (create an API key pair at <https://console.cloud.tencent.com/cam/capi>). The
  common `TENCENTCLOUD_*` variables are listed in
  `terraform/tencentcloud/env.example`; advanced toggles are documented in the
  `create.sh` header comments.
- **Local tools:** `ssh`, `scp`, `nc`, plus network access to the Tencent Cloud
  APIs. `terraform` and `jq` are auto-installed when missing — into
  `/usr/local/bin` when it is writable (e.g. running as root), otherwise into a
  local `.bin/`. `terraform` is fetched from the HashiCorp release site (needs
  `curl`/`wget` + `unzip`); `jq` comes from the system package manager, falling
  back to a static binary from GitHub.
  `mkcert` / `openssl` are **not** needed locally — the cube-proxy certificate is
  produced on the jumpserver.
- **Terraform state lives locally** under `terraform/tencentcloud/` (`*.tfstate`,
  gitignored — there is no remote backend). Keep that directory and the generated
  `.env`, so a later `destroy.sh` or re-run can find and manage the same
  resources. Do not run `create.sh` from a throwaway copy and then expect a
  different copy to clean it up.
- **Phased, fail-fast apply:** resources are created in order — network
  (VPC / subnet / NAT) → TCR → CVMs (jump-server + compute) → image build/push on
  the jump-server → MySQL / Redis → CFS shared storage → TKE cluster + Kubernetes
  addons → health checks → compute-node setup. The Kubernetes provider is only
  engaged after the TKE API server exists. On teardown the CFS share is destroyed
  before its subnet (its NFS mount target is an ENI in that subnet).
- Resolved selections are saved to `terraform/tencentcloud/.env` and auto-loaded
  on the next run; explicit environment variables always win.

### Retrying after a partial failure

If a stage fails part-way (for example an instance type or availability zone that
is sold out in the chosen region/zone, an account quota limit, or a transient API
error), you do **not** have to destroy everything and start over:

- Fix the cause — most often by **changing configuration**: pick a different
  `TENCENTCLOUD_AVAILABILITY_ZONE` / `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` /
  `TENCENTCLOUD_REGION`, raise the quota, set a password, etc. — then simply
  **re-run `./terraform/tencentcloud/create.sh`**.
- On a re-run, `create.sh` reloads the saved selections from `.env`, reconciles
  state with what already exists in the cloud (refreshing and importing stateful
  resources rather than recreating them), and **continues from where it left
  off**. Existing compute nodes are kept (it never scales down).
- Availability genuinely varies by region **and** availability zone — a type
  offered in one zone may be unavailable in another. The interactive zone /
  instance-type menus are queried live for your region, and the final choice is
  validated at apply time.
- Only run `destroy.sh` when you actually want to tear the deployment down; it is
  not required between ordinary retries.

### Advanced: cube-proxy TLS certificates (bring your own)

`cube-proxy` terminates TLS for `cube.app` / `*.cube.app`, and its bundled nginx
config hard-codes the certificate paths `…/certs/cube.app+3.pem` and
`…/certs/cube.app+3-key.pem`:

- By default, `create.sh` (`prepare_cubeproxy_certs`) generates a **self-signed**
  pair on the jumpserver with the bundled `mkcert` (SANs: `cube.app`,
  `*.cube.app`, `localhost`, `127.0.0.1`), downloads it to
  `terraform/tencentcloud/cubeproxy-certs/`, and Terraform packs every file in
  that directory into the `cubeproxy-certs` Secret (a Secret, not a ConfigMap,
  because it holds the TLS private key), mounted read-only into the cube-proxy
  pod at `/usr/local/openresty/nginx/certs/`.
- **Bring your own certificate:** before running `create.sh`, drop your PEM cert +
  key into `terraform/tencentcloud/cubeproxy-certs/`, named exactly
  `cube.app+3.pem` and `cube.app+3-key.pem` (the names nginx expects) and covering
  the `cube.app` and `*.cube.app` SANs. `create.sh` reuses existing files instead
  of generating new ones, so a CA-signed certificate (for example a real domain
  mapped onto `cube.app`) is used as-is, with no self-signed warning.
- **Rotate a certificate:** replace the two files and re-run `create.sh`; the
  deploy stage refreshes the `cubeproxy-certs` Secret and restarts cube-proxy
  to pick up the new material. The self-signed default trips browsers/clients with
  an "untrusted CA" warning, so replace it for any non-throwaway use.
