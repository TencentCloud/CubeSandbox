# Cube Sandbox One-Click

This directory is used to build and deliver the single-machine one-click release package for `cube-sandbox`.

## Directory Overview

- `build-release-bundle-builder.sh`: Recommended entry point. Compiles the components needed by one-click inside a builder image, then continues the release package assembly on the host machine.
- `build-vm-assets.sh`: Builds `containerd-shim-cube-rs`, `cube-runtime`, guest image (with `cube-init` as `/sbin/init`), and independent `cube-agent.ext4`; collects the guest kernel.
- `build-guest-image.sh`: Builds guest OS image with lightweight `cube-init` only (no baked-in agent).
- `build-agent-ext4.sh`: Builds independent `cube-agent/cube-agent.ext4` (+ `version`) for virtio-pmem1.
- `build-release-bundle.sh`: Low-level packaging entry point. Consumes either the source tree or `ONE_CLICK_*_BIN` pre-built artifacts, assembles `sandbox-package`, and produces the final release package.
- `config-cube.toml`: Default one-click runtime configuration template.
- `support/`: `docker compose` templates for MySQL/Redis/MinIO, installed to `/usr/local/services/cubetoolbox/support/` on the target machine; `support/bin/mkcert` is the bundled mkcert binary.
- `cubeproxy/`: Compose template, `global.conf` template, and CoreDNS template for `cube proxy`.
- `webui/`: Nginx runtime files for the dashboard, installed to `/usr/local/services/cubetoolbox/webui/` on the target machine.
- `install.sh`: Entry point for installing and starting the control node on the target machine (defaults to all-in-one mode).
- `install-compute.sh`: Entry point for installing a compute node on the target machine.
- `down.sh`: Stops the services and dependencies installed by one-click.
- `smoke.sh`: Runs basic health checks.
- `env.example`: Target-machine environment variable template.
- `build.env.example`: Build-machine environment variable template for assembling the release bundle.
- `lib/common.sh`: Common shell utility functions.
- `scripts/one-click/`: Validation and maintenance helpers used by the systemd-managed deployment after installation.
- `terraform/tencentcloud/`: Terraform deployer for a **clustered** CubeSandbox on Tencent Cloud (TKE control plane + CVM compute nodes). `create.sh` is the entry point; `destroy.sh` tears everything down. These files are shipped both at the release-bundle top level and inside `sandbox-package` (see "Tencent Cloud Cluster Deployment").

## Root Makefile Targets (agent-independent pmem)

From the repository root, these targets build via the unified builder image (`make builder-image` first if needed):

```bash
make cube-init          # guest PID1 binary → _output/bin/cube-init (alias: guest-init)
make agent-ext4         # independent plane file → _output/cube-agent/{cube-agent.ext4,version}
                        # (alias: cube-agent-ext4)
make pmem-assets        # cube-init + agent-ext4
make help               # list all root targets
```

For the full runtime layout (shim + guest image + agent.ext4 + kernel), use `./build-vm-assets.sh` or the release-bundle entry points below.

## Supported Operating Systems

- Build / deployment host: Linux is recommended. macOS is supported for the Tencent Cloud Terraform deployer (`terraform/tencentcloud/create.sh` and `destroy.sh`), including the default macOS Bash 3.2 environment.
- Windows: native `cmd.exe` / PowerShell execution is not supported. Use WSL2 (Ubuntu or another Linux distribution) when running the shell scripts from Windows.
- Target machines: the one-click runtime expects Linux with systemd and Docker/containerd support. The Tencent Cloud Terraform deployer creates Linux CVMs/TKE resources and configures them through SSH.

## Build Inputs

The required fixed kernel artifact is the ordinary guest kernel `vmlinux`. A PVM guest kernel can also be packaged as `vmlinux-pvm`:

- `vmlinux`
- `vmlinux-pvm` (optional)

By default they are placed under `assets/kernel-artifacts/`, but can be overridden via environment variables:

```bash
export ONE_CLICK_CUBE_KERNEL_VMLINUX=/abs/path/to/vmlinux
export ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX=/abs/path/to/vmlinux-pvm
```

Kernel multi-version inventory (content-addressed):

- On install, `vmlinux-bm` / `vmlinux-pvm` are each inventoried under `component_versions/cube-kernel-scf/sha256-<12 hex>/`
- Each inventory dir contains: `vmlinux-bm|pvm`, `vmlinux` symlink, `variant` (`bm`/`pvm`), and `version` (`sha256:<64 hex>` for shim)
- `KERNEL_TAG` / `PVM_KERNEL_TAG` are **not** inventory directory names; `release-manifest` `kernel.version` / `pvm_version` also record content short hashes
- Ensure maps the digest from Master identity onto that short key

The installed runtime still uses `cube-kernel-scf/vmlinux` as the active guest kernel path. The package stores `vmlinux-bm` and keeps `vmlinux` as a symlink: by default it points to `vmlinux-bm`; if the target machine sets `CUBE_PVM_ENABLE=1` during installation, the installer points it to `vmlinux-pvm`. `CUBE_PVM_ENABLE` is an installer toggle: on upgrades, `CUBE_PVM_ENABLE=0|1 ./install.sh` or the key appearing in the bundle `.env` always wins (even for `0`, the default), while a full `cp env.example .env` resets it to `0`.

The guest image no longer depends on a local zip file. By default it is generated locally from `deploy/guest-image/Dockerfile` during the one-click release package build. Common override parameters:

```bash
export ONE_CLICK_GUEST_IMAGE_DOCKERFILE=/abs/path/to/cube-sandbox/deploy/guest-image/Dockerfile
# Optional; defaults to the directory containing the Dockerfile
export ONE_CLICK_GUEST_IMAGE_CONTEXT_DIR=/abs/path/to/cube-sandbox/deploy/guest-image
# Optional; defaults to cube-sandbox-guest-image:one-click
export ONE_CLICK_GUEST_IMAGE_REF=cube-sandbox-guest-image:one-click
# Optional; defaults to the current repository revision
export ONE_CLICK_GUEST_IMAGE_VERSION=custom-guest-image-version
# Optional; reuse a prebuilt cube-guest-image-*.tar.gz (same layout as the
# Release / docker asset). When set, local docker/mkfs rebuild is skipped.
export ONE_CLICK_GUEST_IMAGE_TAR=/abs/path/to/cube-guest-image-amd64.tar.gz
```

## Building the Release Package

It is recommended to copy the build environment template first:

```bash
cd deploy/one-click
cp build.env.example build.env
```

Run the following from the repository root on the host machine (recommended):

```bash
./deploy/one-click/build-release-bundle-builder.sh
```

To embed a default `envd` binary into the packaged `cubemastercli`, prepare the binary on the build host and pass `ENVD_LOCAL_PATH` to the recommended builder entry point:

```bash
ENVD_LOCAL_PATH=/abs/path/to/envd \
./deploy/one-click/build-release-bundle-builder.sh
```

When this variable is set, the host wrapper copies the file into `deploy/one-click/.work/envd`; the builder container then builds `cubemastercli` with that file embedded. If `ENVD_LOCAL_PATH` is omitted, the packaged `cubemastercli` does not include a default `envd`, and template builds that opt in to envd injection must pass `--envd-path` at runtime.

This entry point will:

- Compile `cubemaster`, `cubemastercli`, `cubelet`, `cubecli`, `cube-api`, `cube-agent`, `containerd-shim-cube-rs`, and `cube-runtime` inside a container using the root-level builder image. The network runtime is embedded in `cubelet` and no standalone network runtime binary is built.
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
- `cubelog` is still referenced as a local module via `../pkgs/CubeLog` and is not downloaded from a remote source.

On success, the following file will be generated:

```bash
deploy/one-click/dist/cube-sandbox-one-click-<version>.tar.gz
```

The release package contains:

- `sandbox-package.tar.gz`
- `release-manifest.json`
- `CubeAPI/bin/cube-api`
- `containerd-shim-cube-rs`, `cube-runtime`
- Locally built `cube-image/cube-guest-image-cpu.img` (with `cube-init` as `/sbin/init`)
- Independent `cube-agent/cube-agent.ext4` (+ `cube-agent/version`)
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
  - `cubelet_conf.default_timeout_insec`: cluster default sandbox idle TTL when the client omits `timeout`; unset or `<= 0` means **no cluster-wide idle timeout** (shipped default `-1`). See [lifecycle — Operational Notes](../../docs/guide/lifecycle.md#cluster-default-idle-timeout-default_timeout_insec).
- `Cubelet/config/` → `Cubelet/config/`
- `Cubelet/dynamicconf/` → `Cubelet/dynamicconf/`
- `CUBE_L7_MARK_{HTTP,HTTPS,MASK}` (env) → `/etc/cubeegress/l7-marks.conf` — the L7 egress skb->mark values shared by Cubelet's embedded network runtime eBPF dataplane (which stamps `skb->mark`) and the `cube-proxy-iptables-init` TPROXY rules (which match it). Both read the same file, and both validate the values (`HTTP != HTTPS`, bits confined to the mask). See `env.example` for the shipped defaults and how to override them.
- `CubeAPI/bin/cube-api` → `/usr/local/services/cubetoolbox/CubeAPI/bin/cube-api`
- `support/` → `/usr/local/services/cubetoolbox/support/`
- `cubeproxy/` → `/usr/local/services/cubetoolbox/cubeproxy/`
- `webui/` → `/usr/local/services/cubetoolbox/webui/`

`Cubelet` uses the existing `dynamicconf/conf.yaml` from the repository as-is, and its embedded network runtime reads the network plugin configuration from `Cubelet/config/config.toml` directly. `cube-api` and `cubeops` read environment variables from `.one-click.env` on startup. CubeOps warehouse knobs are `CUBE_OPS_WAREHOUSE_*` (timeouts, GitHub/CNB allow-lists and tokens) plus `CUBE_OPS_S3_*`; by default the warehouse reuses the volume MinIO/S3 connection with a dedicated `cube-ops` bucket, so no extra S3 setup is needed. There is no CubeOps YAML file in the one-click layout. `cube-api` listens on `0.0.0.0:3000` by default and forwards to the local `cubemaster`. MySQL/Redis are always deployed to `/usr/local/services/cubetoolbox/support` and run in Docker containers managed by dedicated systemd services on the target machine. `cube proxy` is always deployed to `/usr/local/services/cubetoolbox/cubeproxy`, built locally from the bundled build context, and managed by systemd. WebUI is deployed to `/usr/local/services/cubetoolbox/webui`, listens on `12088` by default, serves the packaged `webui/dist` directory through a standard nginx container, and proxies `/cubeapi` to CubeAPI through Docker `host-gateway` under systemd management.

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

### CubeS3lvol stop/upgrade semantics

CubeS3lvol (s3lvol) is managed as a `Wants=` member of the `cube-sandbox-*`
role target:

- **Stopping** (`down.sh` / `systemctl stop cube-sandbox-{control,compute}.target`):
  the s3lvol unit goes through `cube-s3lvol-stop.sh`'s **conditional unload** —
  when the target process is alive it runs the full `rcow_stop.sh` (disconnect
  initiators -> flush/unload lvstore -> stop the target); when the target has
  already crashed it only clears target-side residue and **never disconnects
  the NVMf initiators**. `down.sh` only stops services, it does **not delete
  any data** (`/data/cubelet/rcow/wal_bdev.img` and the bstore metadata are
  kept); the next start recovers via attach/replay.
- **Upgrading** (`install.sh` upgrade mode): the old `CubeS3lvol/` directory is
  replaced (the new binary takes effect), then the target restarts with the
  role target. `wal_bdev.img` is **never overwritten** (created only on first
  install; its size fixes the journal/WAL layout), and the `RCOW_*` settings in
  `.one-click.env` are merged and kept across the upgrade.
- **Enable/disable**: preferred `ONE_CLICK_ENABLE_S3LVOL=0|1 ./install.sh`
  (honored on upgrade as well). Or put only that key in the bundle `.env`
  and re-run `install.sh`. Do not `cp env.example .env` as a full copy
  before upgrade — that resets this switch to `0`. Hand-editing
  `.one-click.env` is no longer required. `systemctl enable/disable
  cube-sandbox-s3lvol.service` still works as a direct systemd toggle.
  `CUBE_PVM_ENABLE` follows the same rule: appearing in `.env` or the
  process environment always counts as an explicit choice (so a full
  `cp env.example .env` before an upgrade also resets it to `0`).
- **S3 backend**: when enabled, `install.sh` writes `/data/cubelet/s3.cfg`
  from `CUBE_S3_*` (bundled MinIO fill, or the operator's external store).
  s3lvol uses its own bucket (`CUBE_S3LVOL_BUCKET`, default `cube-s3lvol`)
  so it never shares prefixes with the volume plugin's `cube-volumes`.
  The supervisor idempotently creates that bucket with a stdlib SigV4
  tool — no awscli. A hand-written `s3.cfg` without the one-click sentinel
  is left alone. There is **no fallback** from the old `/data/cubelet/cos.cfg`;
  rename it and switch to the new field names.

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

If the control node runs bundled MinIO (or any S3 backend), copy `CUBE_S3_*` from
that node's runtime env file so the volume plugin reaches the same store:

```bash
# Run on the control node; paste the output into the compute node's .env
grep '^CUBE_S3_' /usr/local/services/cubetoolbox/.one-click.env
```

Compute nodes never deploy MinIO; they only consume `CUBE_S3_*`. These values are
optional but strongly recommended: if missing, the installer prints a prominent
warning and continues; the S3 volume plugin stays disabled until configured. Allow
TCP 9000 from compute to the control node when using bundled MinIO.

If you need to explicitly specify the compute node IP, or if the default NIC on the target machine is not `eth0`, also set:

```bash
CUBE_SANDBOX_NODE_IP=10.0.0.12
```

Then run:

```bash
sudo ./install-compute.sh
```

In compute node mode, the installer will:

- Install `Cubelet` with the embedded network runtime, `cube-shim`, `cube-image`, `cube-kernel-scf`, `cube-egress`, the required scripts, and `docker`.
- Start `cubelet`, and bring up `cube-egress` via `cube-sandbox-compute.target` (the transparent egress MITM proxy, run as a docker container, which enforces per-sandbox egress policy).
- Before `cube-egress` starts, pull the MITM root CA (cert + key) from the control node's `/cube/ca/<file>` endpoint so it matches the CA baked into templates — templates then trust the leaf certs the compute-node `cube-egress` signs.
- Point `Cubelet`'s `meta_server_endpoint` to `ONE_CLICK_CONTROL_PLANE_IP:3010` (CubeOps node-agent).
- Automatically register the node via the control node's `/internal/v1/node-agent` API.

Notes:

- All compute nodes must have `Cubelet` listening on the same gRPC port as configured on the control node (default `9999`).
- `CUBE_SANDBOX_NODE_IP` is used both as the one-click configuration value and as the `Cubelet` node registration IP.
- The control node must be able to reach port `9999/tcp` on all compute nodes; compute nodes must be able to reach port `8089/tcp` on the control node (and `9000/tcp` when using bundled MinIO as the S3 volume backend).

MySQL/Redis dependencies are deployed by default to:

```bash
/usr/local/services/cubetoolbox/support
```

During installation, runtime files are prepared in this directory and the following containers are managed individually by systemd:

- `mysql:8.0`
- `redis:7-alpine`
- `minio` (S3-compatible volume backend; explicit via `CUBE_SANDBOX_MINIO_ENABLED`, default on)

### Using an external MySQL / PostgreSQL / Redis

To point CubeSandbox at an existing MySQL, PostgreSQL, or Redis server instead
of the bundled local containers, set the following in `.env` before running
`install.sh` (see `env.example`). `CUBE_DATABASE_DRIVER` mirrors Helm
`database.driver`: `mysql` (default) or `postgres` (always external).

```bash
# External MySQL (default driver; any subset of the credential fields may be overridden)
# CUBE_DATABASE_DRIVER=mysql
CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
CUBE_EXTERNAL_MYSQL_PORT=3306
CUBE_EXTERNAL_MYSQL_USER=cube
CUBE_EXTERNAL_MYSQL_PASSWORD=cube_pass
CUBE_EXTERNAL_MYSQL_DB=cube_mvp

# External PostgreSQL (one-click never ships a local PostgreSQL)
# CUBE_DATABASE_DRIVER=postgres
# CUBE_EXTERNAL_POSTGRES_HOST=10.0.0.20
# CUBE_EXTERNAL_POSTGRES_PORT=5432
# CUBE_EXTERNAL_POSTGRES_USER=cube
# CUBE_EXTERNAL_POSTGRES_PASSWORD=cube_pass
# CUBE_EXTERNAL_POSTGRES_DB=cube_mvp

# External Redis
CUBE_EXTERNAL_REDIS_HOST=10.0.0.21
CUBE_EXTERNAL_REDIS_PORT=6379
CUBE_EXTERNAL_REDIS_PASSWORD=ceuhvu123
```

When `CUBE_EXTERNAL_MYSQL_HOST`, `CUBE_EXTERNAL_POSTGRES_HOST` (with
`CUBE_DATABASE_DRIVER=postgres`), and/or `CUBE_EXTERNAL_REDIS_HOST` is set,
`install.sh`:

- patches `CubeMaster/conf.yaml` with the external endpoint and sets
  `instance_db_config.driver` for SQL engines;
- writes `DATABASE_URL` (`mysql://` or `postgresql://`) and `CUBE_PROXY_REDIS_*`
  to `.one-click.env` so every service consumes the external endpoint;
- masks the corresponding `cube-sandbox-mysql.service` / `cube-sandbox-redis.service`
  so the local container is never started; and
- makes `quickcheck.sh` and `up-support.sh` skip lifecycle management of the
  now-external dependency. (`down-support.sh` has no external-dep awareness and
  still issues a `docker compose down`, but this is a harmless no-op because the
  local containers were never started for the external dependency.)

The external database must already grant the configured user access to the
target database. CubeMaster runs its own embedded schema migrations on first
start.

### Bundled MinIO vs the S3 volume plugin

`CUBE_SANDBOX_MINIO_*` only deploys the MinIO container. The S3 volume plugin
always reads `CUBE_S3_*` and writes `volume-s3.conf` from those values.

The bundled MinIO occupies two host ports (both overridable via environment
variables):

| Port | In container | Host mapping | Bind address |
| ---- | ------------ | ------------ | ------------ |
| S3 API | `9000` | `CUBE_SANDBOX_MINIO_API_PORT` (default `9000`) | `CUBE_SANDBOX_MINIO_API_BIND`, default `CUBE_SANDBOX_NODE_IP` (falls back to `127.0.0.1` when no node IP is detected) |
| Web console | `9001` | `CUBE_SANDBOX_MINIO_CONSOLE_PORT` (default `9001`) | always `127.0.0.1`, localhost only |

The S3 API is published on the node IP by default so compute-node Cubelets can
reach it directly. To keep S3 local-only, set
`CUBE_SANDBOX_MINIO_API_BIND=127.0.0.1` — at the cost of compute nodes losing
access to the bundled MinIO (use an external S3 store instead). The console
port is always bound to `127.0.0.1` and is never exposed.

On a control node with `CUBE_SANDBOX_MINIO_ENABLED=1` (default), `install.sh`
starts MinIO, generates a 24-character random password if you left it empty,
then **fills `CUBE_S3_*`** from that MinIO (`http://<node-ip>:9000`, the MinIO
user/password, path-style s3fs options) and persists both families in
`.one-click.env`. Do not set `CUBE_S3_ENDPOINT` yourself while MinIO is
enabled. A later upgrade may reload that filled local endpoint from
`.one-click.env`; that is expected and allowed. Only a different,
operator-supplied external store requires `CUBE_SANDBOX_MINIO_ENABLED=0`.

Other MinIO deployment parameters: default user `cubeminio`
(`CUBE_SANDBOX_MINIO_ROOT_USER`; the password must be at least 8 characters or
MinIO refuses to start), bucket `cube-volumes` (`CUBE_SANDBOX_MINIO_BUCKET`),
data volume `cube-sandbox-minio-data` (`CUBE_SANDBOX_MINIO_VOLUME`, mounted at
`/data`), container name `cube-sandbox-minio` (`CUBE_SANDBOX_MINIO_CONTAINER`),
and image `CUBE_SANDBOX_MINIO_IMAGE` (default selected by `MIRROR=cn|int`).
MinIO runs under `cube-sandbox-minio.service`; after startup, readiness is
verified via `curl http://<node-ip>:9000/minio/health/live` (a `200` response
means it is healthy).

To use an existing S3-compatible store instead, set
`CUBE_SANDBOX_MINIO_ENABLED=0` and `CUBE_S3_*` before `install.sh`:

```bash
CUBE_SANDBOX_MINIO_ENABLED=0
CUBE_S3_ENDPOINT=https://s3.example.com
CUBE_S3_ACCESS_KEY_ID=...
CUBE_S3_SECRET_ACCESS_KEY=...
CUBE_S3_BUCKET=cube-volumes
# CUBE_S3_REGION=us-east-1
# CUBE_S3_S3FS_EXTRA_OPTS=-ouse_path_request_style
```

Compute nodes never run MinIO. Copy the filled `CUBE_S3_*` values from the
control node's runtime env file into the compute `.env`:

```bash
# Run on the control node; paste the output into the compute node's .env
grep '^CUBE_S3_' /usr/local/services/cubetoolbox/.one-click.env
```

These values are optional but strongly recommended — if missing, the installer
warns and continues; the S3 volume plugin stays disabled until configured. Allow
TCP 9000 from compute to the control node if using bundled MinIO:

```bash
ONE_CLICK_DEPLOY_ROLE=compute
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.11
CUBE_S3_ENDPOINT=http://10.0.0.11:9000
CUBE_S3_ACCESS_KEY_ID=cubeminio
CUBE_S3_SECRET_ACCESS_KEY=<from control .one-click.env>
CUBE_S3_BUCKET=cube-volumes
CUBE_S3_S3FS_EXTRA_OPTS=-ouse_path_request_style
```

`cube proxy` and its DNS resolution are mandatory capabilities in one-click. The following two values in `.env` must remain `1`:

```bash
CUBE_PROXY_ENABLE=1
CUBE_PROXY_DNS_ENABLE=1
```

Other common parameters:

```bash
CUBE_PROXY_HTTPS_PORT=443
CUBE_PROXY_HTTP_PORT=80
CUBE_PROXY_GRPC_PORT=9090
CUBE_EGRESS_ADMIN_PORT=9091
CUBE_EGRESS_PROXY_READ_TIMEOUT=300s
CUBE_EGRESS_PROXY_SEND_TIMEOUT=300s
# Deprecated: CUBE_PROXY_HOST_PORT is ignored; configure CUBE_PROXY_HTTP_PORT instead.
CUBE_PROXY_CERT_DIR=/usr/local/services/cubetoolbox/cubeproxy/certs
CUBE_PROXY_DNS_ANSWER_IP="${CUBE_SANDBOX_NODE_IP}"
WEB_UI_ENABLE=1
WEB_UI_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat
WEB_UI_HOST_PORT=12088
WEB_UI_UPSTREAM=http://host.docker.internal:3010
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
- If `resolvectl` is available, one-click creates a dedicated dummy link (default `cube-dns0`) with a local address, binds CoreDNS to `169.254.254.53` on that link by default, and routes `cube.app` through the link without affecting the host's default public DNS path. If `resolvectl` is unavailable on the target machine, the installer falls back to `NetworkManager + dnsmasq`: it still creates the same dummy link, asks `dnsmasq` to additionally listen on `169.254.254.53`, takes `/etc/resolv.conf` ownership away from NetworkManager (`rc-manager=unmanaged`) and rewrites it to point at the same non-loopback IP. This keeps the host resolver symmetrical with the `systemd-resolved` path and avoids the Docker daemon's silent fallback to public DNS (`8.8.8.8`) that happens when `/etc/resolv.conf` contains only loopback nameservers — without it, every container on the host (including `docker build`'s `apk update` step) ends up using DNS servers that internal machines cannot reach. On hosts where NetworkManager initializes its `dnsmasq` plugin but never spawns the child process (for example bonded interfaces managed via `ifcfg` + `assume`), set `CUBE_PROXY_DNSMASQ_MODE=standalone` so the DNS scripts launch and own `dnsmasq` directly instead of relying on the NetworkManager plugin; the client-facing resolver layout (dummy link, listen addresses, entry IP) is otherwise identical.
- Host processes `cubemaster`, `cube-api`, and `cubelet` are started through systemd, and `quickcheck.sh` verifies both unit state and service health.
- A standard WebUI nginx container is started under `/usr/local/services/cubetoolbox/webui/`. It mounts `webui/dist` as read-only static content, publishes `WEB_UI_HOST_PORT` (`12088` by default), maps `host.docker.internal` to Docker `host-gateway`, and verifies `/health` through the nginx reverse proxy (served by CubeOps).

Stopping one-click will simultaneously stop MySQL/Redis under `/usr/local/services/cubetoolbox/support`, WebUI, `cube proxy` / `CoreDNS`, and the host processes `cubemaster` / `cube-api` / `cubelet`, and will roll back the host DNS routing configuration for `cube.app`.

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
- If `ONE_CLICK_ENABLE_S3LVOL=1` and the package ships `CubeS3lvol/bin/s3lvol_tgt`, one-click also checks:
  `nvme` (nvme-cli), `python3`, `truncate`, remaining `s3lvol_tgt` shared libraries (`ldd`; OpenSSL is static), and (x86_64) `avx2` in `/proc/cpuinfo`. Release `s3lvol_tgt` is built for Haswell/AVX2, not the packager's native CPU. `install.sh` installs `nvme-cli` via the system package manager when `nvme` is missing. `python3` must be able to run `CubeS3lvol/scripts/rpc.py --help` (Python 3.8 is enough; the packaged launcher supplies the 3.9 `argparse` bits SPDK's client needs).

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

Additional packages for `ONE_CLICK_ENABLE_S3LVOL=1` (libraries only; `nvme-cli` is installed by `install.sh`):

```bash
# Debian / Ubuntu
sudo apt-get install -y python3 libaio1 libnuma1 uuid-runtime

# OpenCloudOS / RHEL / CentOS
sudo dnf install -y python3 libaio libnuma libuuid || \
sudo yum install -y python3 libaio libnuma libuuid
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
- DNS split routing: `resolvectl`, or (for the default `networkmanager` dnsmasq fallback) `systemctl + NetworkManager`. The `standalone` dnsmasq mode (`CUBE_PROXY_DNSMASQ_MODE=standalone`) does not require a loaded/restartable `NetworkManager`.
- If `dnsmasq` is missing and either dnsmasq fallback path is taken (`networkmanager` or `standalone`), one of the following package managers is also required: `dnf` / `yum` / `apt-get`.

Conditional commands:

- If `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1` is enabled and `/etc/docker/daemon.json` already exists, `python3` is required.
- If the packaged `Cubelet/config/config.toml` enables `storage_backend = "cubecow"`, one-click also checks:
  `mkfs.ext4`, `mount`, `umount`, `losetup`
- If `ONE_CLICK_ENABLE_S3LVOL=1` and the package ships `CubeS3lvol/bin/s3lvol_tgt`, one-click also checks:
  `nvme` (nvme-cli), `python3`, `truncate`, remaining `s3lvol_tgt` shared libraries (`ldd`; OpenSSL is static), and (x86_64) `avx2` in `/proc/cpuinfo`. Release `s3lvol_tgt` is built for Haswell/AVX2, not the packager's native CPU. `install.sh` installs `nvme-cli` via the system package manager when `nvme` is missing. `python3` must be able to run `CubeS3lvol/scripts/rpc.py --help` (Python 3.8 is enough; the packaged launcher supplies the 3.9 `argparse` bits SPDK's client needs).

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

Additional packages for `ONE_CLICK_ENABLE_S3LVOL=1` (libraries only; `nvme-cli` is installed by `install.sh`):

```bash
# Debian / Ubuntu
sudo apt-get install -y python3 libaio1 libnuma1 uuid-runtime

# OpenCloudOS / RHEL / CentOS
sudo dnf install -y python3 libaio libnuma libuuid || \
sudo yum install -y python3 libaio libnuma libuuid
```

## Prerequisites

> **Security**: All core services bind `0.0.0.0` by default. Before deploying on
> a machine reachable from untrusted networks, review the
> [Network Hardening Guide](../../docs/guide/network-hardening.md) for bind-address
> configuration, firewall rules, and credential rotation.

- The target machine requires `root` privileges.
- The target machine preferentially uses `systemd-resolved` / `resolvectl` for split DNS of `cube.app`. The current implementation creates a dedicated dummy link (default `cube-dns0`), assigns it a local `/32` address, binds CoreDNS to `169.254.254.53` on that link by default, and attaches that address plus `~cube.app` to the link. If that capability is unavailable, the installation script will fall back to `NetworkManager + dnsmasq`: the same dummy link is created and `dnsmasq` is configured (via `listen-address` / `bind-interfaces`) to listen on both `127.0.0.1` and `169.254.254.53`. `/etc/resolv.conf` is then written by the installer (NetworkManager runs with `rc-manager=unmanaged`) to point at `169.254.254.53`, so host applications and Docker containers see the same non-loopback resolver. When NetworkManager loads its `dnsmasq` plugin but never spawns the child (for example bonded interfaces managed via `ifcfg` + `assume`), set `CUBE_PROXY_DNSMASQ_MODE=standalone` in `.one-click.env` so the DNS scripts start and manage `dnsmasq` directly.
- The target machine pulls `mysql:8.0` and `redis:7-alpine` from the internet by default.
- The `mkcert` binary is bundled in the release package (`support/bin/mkcert`). If `mkcert` is not pre-installed on the system, it is automatically copied from the package to `/usr/local/bin/mkcert` — no internet download required.
- The S3 volume plugin (`{CubeMaster,Cubelet}/plugin/cube-volume-s3`) is a static Go binary with a built-in S3 client, compiled at pack time from `examples/volume/s3`. Control nodes need no S3 command line tool; nodes that mount volumes still need `s3fs`. Ship a prebuilt binary with `ONE_CLICK_VOLUME_S3_BIN`.
- TLS certificates and private keys for `cube proxy` are stored on the host under `CUBE_PROXY_CERT_DIR` and mounted read-only into the container via `docker compose`. After updating certificates, simply restart `cube-proxy` or reload nginx inside the container — no image rebuild required.
- The recommended entry point `build-release-bundle-builder.sh` requires the host machine to have `docker`, `make`, `tar`, `python3`, `truncate`, `ldd`, `mkfs.ext4`, and similar tools.
- The recommended entry point only runs component compilation inside the builder; guest image generation and final packaging are still performed on the host machine.
- If invoking the low-level entry point `build-release-bundle.sh` directly, the build machine must also have local toolchains such as `go`, `cargo`, and `make` installed, depending on the build mode.
- If using the low-level entry point directly or running the recommended entry point for the first time, the build machine must be able to download Go modules from the internet. Configure a usable `GOPROXY` in advance for restricted network environments.
- If the VM path is enabled, the target machine must still satisfy the runtime permission requirements for the Cubelet embedded network runtime, tap interfaces, routing, etc.

## Known Limitations

- If `vmlinux` is missing from `assets/kernel-artifacts/`, `build-vm-assets.sh` and `build-release-bundle.sh` will fail immediately. `vmlinux-pvm` is optional at build time, but installation with `CUBE_PVM_ENABLE=1` requires it to be present in the package. The installed `cube-kernel-scf/vmlinux` path is an active symlink to `vmlinux-bm` or `vmlinux-pvm`. The `cube-kernel-scf.zip` in the release package is generated automatically during the packaging phase.
- If the `deploy/guest-image/Dockerfile` build fails, or the build machine's `mkfs.ext4` does not support the `-d` flag, guest image generation will fail immediately.
- `cube-snapshot/spec.json` is not a mandatory artifact in the current first release of one-click. If absent, the related plugin degrades to a warning rather than blocking the basic startup.
- The default `NetworkManager + dnsmasq` fallback relies on NetworkManager to spawn the `dnsmasq` child. On hosts where NetworkManager initializes the plugin but never spawns it (for example bonded interfaces managed via `ifcfg` + `assume`), set `CUBE_PROXY_DNSMASQ_MODE=standalone` so the DNS scripts launch and manage `dnsmasq` themselves. Standalone mode does not require a restartable `NetworkManager`, but on hosts with no resolver manager at all you must ensure nothing else overwrites `/etc/resolv.conf` afterwards. In this mode `dnsmasq` runs as a bare child that systemd does not supervise, so if it later crashes nothing restarts it automatically; recover with `systemctl restart cube-sandbox-dns`.

## DNS Troubleshooting

- Inspect the current split-DNS state: `resolvectl status`
- Verify the host stub resolver path: `dig +tcp +timeout=3 docker.cnb.cool @127.0.0.53`
- Verify the local CoreDNS path: on the `systemd-resolved` path and on both `dnsmasq` fallback paths (`NetworkManager`-managed or `standalone`), the client entry point is the same dummy-link IP, so run `dig +tcp +timeout=3 foo.cube.app @169.254.254.53`. CoreDNS itself stays bound to `127.0.0.54` internally; only the `systemd-resolved` path talks to CoreDNS directly, while the fallback paths go through `dnsmasq`.
- Verify the host stub resolver path also routes through the new entry point: `cat /etc/resolv.conf` should show `nameserver 169.254.254.53` on both paths.
- Verify the container view: `docker run --rm alpine cat /etc/resolv.conf` should also show `nameserver 169.254.254.53`. If it shows `nameserver 8.8.8.8` instead, the host's `/etc/resolv.conf` regressed to a loopback nameserver and Docker fell back to its built-in public DNS.
- On the `systemd-resolved` path, the local CoreDNS address should appear only on the dedicated dummy link, not on the default network interface.

## Tencent Cloud Cluster Deployment (Terraform)

> Full guide (architecture, resource list, TKE / PrivateDNS / CFS preflight, E2B and `*.cube.app` DNS, capacity planning, hardening, troubleshooting): [Tencent Cloud Cluster Deployment (Terraform)](../../docs/guide/tencentcloud-terraform-deploy.md).

In addition to the single-machine `install.sh`, the release bundle ships a
Terraform-based deployer that stands up a **clustered** CubeSandbox on Tencent
Cloud: a managed TKE control plane running `cubemaster` / `cube-api` /
`cube-proxy` / `cube-webui`, backed by cloud MySQL + Redis, with one or more CVM
PVM compute nodes. A jumpserver (SSH on port `443`) is the build host and bastion
for the otherwise-private VPC.

The default deployment mode (matching `env.example` / `variables.tf`) uses **public pre-built images** (`TENCENTCLOUD_USE_TCR=false`) with no image build on the jumpserver; cubemaster defaults to **single replica** with **no CFS** (`TENCENTCLOUD_USE_CFS=false`, Pod-local storage). Set `TENCENTCLOUD_USE_CFS=true` and raise `TENCENTCLOUD_CUBEMASTER_REPLICAS` to create a CFS share for multi-replica cubemaster at `/data/CubeMaster/storage`.

`cube-proxy` runs a **single replica** by default
(`TENCENTCLOUD_CUBE_PROXY_REPLICAS=1`). Auto-pause/auto-resume only works
correctly with one replica, because each sidecar sweeper only sees traffic
hitting its own pod. To scale beyond 1 replica the front-end LB must hash on
SandboxID (session affinity); otherwise auto-pause/auto-resume will misfire.

### Pre-deployment setup (summary)

Before the first `create.sh` apply:

1. **TKE service role authorization** (required): log in to the [TKE console](https://console.cloud.tencent.com/tke2) and complete service authorization. Docs: [Service authorization role permissions](https://cloud.tencent.com/document/product/457/43416). Sub-accounts also need [TKE preset policy authorization](https://cloud.tencent.com/document/product/457/46033).
2. **Private DNS** (as needed): required for `USE_TCR=true` or E2B SDK access to `*.cube.app`. Console: [DNSPod Private DNS](https://console.dnspod.cn/privateDNS). Docs: [Private DNS product overview](https://cloud.tencent.com/document/product/1338/50527).
3. **CFS** (as needed): only when `TENCENTCLOUD_USE_CFS=true` and cubemaster runs multiple replicas. Console: [CFS](https://console.cloud.tencent.com/cfs). Docs: [CFS quick start](https://cloud.tencent.com/document/product/582/9132).

> **TKE workers and PVM compute nodes are separate:** `TENCENTCLOUD_TKE_NODE_COUNT` controls TKE workers (control-plane Pods); `TENCENTCLOUD_COMPUTE_NODE_COUNT` controls PVM compute nodes (Cubelet / sandboxes). Both default to `2` but serve different roles.

> **E2B SDK:** the cluster deployment does not include single-machine CoreDNS split DNS. Besides `E2B_API_URL`, you must configure `*.cube.app` resolution (Private DNS or equivalent). See the [full guide — E2B and the cube.app domain](../../docs/guide/tencentcloud-terraform-deploy.md#e2b-and-the-cubeapp-domain).

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
- **Default mode** (`TENCENTCLOUD_USE_TCR=false`): pull public pre-built images and deploy TKE addons and CVM compute nodes.
- **TCR mode** (`TENCENTCLOUD_USE_TCR=true`): create TCR, build and push the four component images on the jumpserver, then deploy TKE addons and compute nodes. Default creates 2 compute nodes; use `TENCENTCLOUD_COMPUTE_NODE_COUNT` to adjust.

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
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_NODE_COUNT=2          # CVM PVM compute nodes (default 2)
export TENCENTCLOUD_TKE_NODE_COUNT=2              # TKE worker nodes (default 2)
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_USE_TCR=false                 # default: public pre-built images
export TENCENTCLOUD_USE_CFS=false                 # default: no CFS, cubemaster single replica
export TENCENTCLOUD_CUBE_IMAGE_TAG=v0.7.0
```

For non-interactive / CI runs, also set these (without a TTY the interactive
menus fall back to defaults, so set them explicitly to stay in control). The
password variables are the exception: a non-interactive run refuses to start
with the built-in, publicly-known demo passwords and requires them to be set —
or set `TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1` to opt into the insecure
defaults for a throwaway sandbox.

```bash
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_LOCAL_BUNDLE=/path/to/cube-sandbox-one-click-<version>.tar.gz  # auto-detected when run from inside an extracted bundle
export TENCENTCLOUD_PVM_KERNEL_VMLINUX=/path/to/vmlinux-pvm  # only needed if the bundle ships no vmlinux-pvm
export TENCENTCLOUD_MYSQL_PASSWORD=...      # required for non-interactive runs (no insecure fallback)
export TENCENTCLOUD_REDIS_PASSWORD=...      # required for non-interactive runs
export TENCENTCLOUD_CUBE_PASSWORD=...       # required for non-interactive runs
export TENCENTCLOUD_BUILD_IMAGES=0          # TCR mode: reuse already-pushed images
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
> [Redis recycle bin](https://console.cloud.tencent.com/redis/recycle),
> [CFS file systems](https://console.cloud.tencent.com/cfs) (if `USE_CFS=true` was enabled).
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
  (VPC / subnet / NAT) → **(when `USE_TCR=true`)** TCR → CVMs (jump-server + compute) →
  **(TCR mode)** image build/push on the jump-server → MySQL / Redis → **(when `USE_CFS=true`)**
  CFS shared storage → TKE cluster + Kubernetes addons → health checks → compute-node setup.
  The Kubernetes provider is only engaged after the TKE API server exists. On teardown,
  if CFS was created, it is destroyed before its subnet (its NFS mount target is an ENI in that subnet).
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
