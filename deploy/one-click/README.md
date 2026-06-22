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
- `sql/`: MySQL initialization schema and seed data.

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

The default installation path is `/usr/local/services/cubetoolbox`.

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

If `CUBE_SANDBOX_NODE_IP` is explicitly set, the installation script will use that value directly; otherwise, the auto-detected node IP is written to MySQL's `t_cube_host_info.ip` and `t_cube_sub_host_info.host_ip`, and used to render `cube proxy` / DNS addresses.

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

- Install only `Cubelet`, `network-agent`, `cube-shim`, `cube-image`, `cube-kernel-scf`, and the required scripts.
- Start only `network-agent` and `cubelet`.
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
database; CubeMaster runs its own schema migrations on first start, and the
single-node seed rows are applied through a host-side `mysql` client (install
the MySQL client package on the control node when seeding an external DB).

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
CUBE_PROXY_CERT_DIR="${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs"
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
