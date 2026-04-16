# Self-Build Deployment

> If you prefer to get started without building from source, see [Quick Start](./quickstart).

This guide walks you through building a Cube Sandbox release bundle from source and deploying it on a single bare-metal server. Self-build deployment is intended for **evaluation, development, and testing** purposes, and is also the starting point if you need to customize components or add compute nodes.

After deployment, you will have a fully functional Cube Sandbox instance with:

- E2B-compatible REST API on port `3000`
- CubeMaster, Cubelet, network-agent, and CubeShim running as host processes
- MySQL and Redis managed via Docker Compose
- CubeProxy with TLS (mkcert) and CoreDNS for `cube.app` domain routing

## Prerequisites

### Hardware

- **Physical machine or bare-metal server** (nested virtualization is not supported)
- **x86_64** architecture
- **KVM enabled** — verify with `ls /dev/kvm`
- Recommended: 8+ CPU cores, 16+ GB RAM

### Software (Target Machine)

| Requirement | Notes |
|-------------|-------|
| Linux | OpenCloudOS 9 (recommended) or Ubuntu 22.04+ |
| Docker | Must be installed and running |
| root access | `install.sh` requires root privileges |
| DNS routing | `systemd-resolved` (preferred) or `NetworkManager + dnsmasq` |
| `tar`, `rg`, `ss` | Required by install script |

### Software (Build Machine)

| Requirement | Notes |
|-------------|-------|
| Docker | For running the builder container |
| `make` | For building the builder image |
| `tar`, `python3`, `truncate`, `ldd`, `mkfs.ext4` | For guest image generation and packaging |

> The build machine and target machine can be the same physical host.

### Network

- Internet access is required to pull `mysql:8.0` and `redis:7-alpine` Docker images.
- `mkcert` binary is bundled inside the release package and installed automatically when not already present on the target machine.
- CubeProxy image build uses Alpine and PyPI mirrors (configurable).

## Step 1: Build the Release Bundle

These steps are performed on the **build machine**.

### 1.1 Prepare the Kernel

Obtain a compiled `vmlinux` kernel file (either compile it yourself or use a prebuilt one) and place it in the designated directory:

```bash
cp /path/to/vmlinux deploy/one-click/assets/kernel-artifacts/
```

The default expected filename is `vmlinux`. You can override the path via the `ONE_CLICK_CUBE_KERNEL_VMLINUX` environment variable.

### 1.2 Run the Build

From the repository root:

```bash
cd cube-sandbox
./deploy/one-click/build-release-bundle-builder.sh
```

This script will:

1. Build or reuse the `cube-sandbox-builder` Docker image
2. Compile all components inside the builder container (CubeMaster, Cubelet, cube-api, network-agent, cube-agent, CubeShim, cube-runtime)
3. Build the guest VM image on the host
4. Package everything into a release tarball

### 1.3 Locate the Output

On success, the release bundle is created at:

```
deploy/one-click/dist/cube-sandbox-one-click-<version>.tar.gz
```

The `<version>` is derived from the current Git commit ID.

The bundle contains:

- All compiled binaries (cubemaster, cubelet, cube-api, network-agent, containerd-shim-cube-rs, cube-runtime)
- Guest VM image (`cube-guest-image-cpu.img`)
- Kernel package (`cube-kernel-scf.zip`)
- CubeProxy and CoreDNS Docker Compose templates
- MySQL/Redis Docker Compose templates
- Installation scripts (`install.sh`, `install-compute.sh`, `down.sh`, `smoke.sh`)
- Environment template (`env.example`)

## Step 2: Deploy to the Target Machine

### 2.1 Upload and Extract

Copy the tarball to the target machine and extract it:

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
```

### 2.2 Configure Environment Variables

```bash
cp env.example .env
```

Most variables have sensible defaults for single-node deployment. The installer auto-detects the node IP from `eth0`. If your primary network interface uses a different name, or you want to pin a specific IP, set it explicitly in `.env`:

```bash
CUBE_SANDBOX_NODE_IP=<your-node-ip>
```

See [Configuration Reference](#configuration-reference) below for a full list of configurable parameters.

### 2.3 Install

#### Control Node

```bash
sudo ./install.sh
```

The install script will:

1. Optionally configure a Docker registry mirror (if `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1`)
2. Extract the sandbox package to `/usr/local/services/cubetoolbox` (configurable)
3. Create required log and data directories
4. Symlink CubeShim binaries to `/usr/local/bin/`
5. Install bundled `mkcert` (if not already present), generate TLS certificates for `cube.app`
6. Start MySQL and Redis via Docker Compose
7. Build and start the CubeProxy container
8. Start CoreDNS and configure host DNS routing for `cube.app`
9. Start host processes: network-agent, cubemaster, cube-api, cubelet
10. Run a health check (if `ONE_CLICK_RUN_QUICKCHECK=1`)

After installation, the installer symlinks `cubemastercli` and `cubecli` into `/usr/local/bin`.

#### Adding Compute Nodes (Multi-Node Cluster)

To scale beyond a single machine, you can add compute-only nodes that register to this control node. See the [Multi-Node Cluster Deployment](./multi-node-deploy) guide for full instructions.

## Verifying the Deployment

### Health Check

```bash
sudo ./smoke.sh
```

This runs `quickcheck.sh` and verifies that the `cube-api /health` endpoint is responding.

For compute-node health checks, see [Multi-Node Cluster Deployment — Verifying the Deployment](./multi-node-deploy#verifying-the-deployment).

### Test with E2B SDK

Set the following environment variables on your client machine:

```bash
export CUBE_TEMPLATE_ID=<your-template-id>
export E2B_API_URL=http://<target-host>:3000
export E2B_API_KEY=dummy
export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

| Variable | Description |
|----------|-------------|
| `CUBE_TEMPLATE_ID` | Sandbox template ID, required by all examples |
| `E2B_API_URL` | Cube API address; without this the SDK will contact the official E2B cloud |
| `E2B_API_KEY` | The SDK requires a non-empty value; any string works for local deployments |
| `SSL_CERT_FILE` | Path to the mkcert CA root certificate, needed for HTTPS connections |

**Run code**

```python
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox!')")
    print(result)
```

**Run a shell command**

```python
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.commands.run("echo hello cube")
    print(result.stdout)
```

**Read a file inside the sandbox**

```python
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    content = sandbox.files.read("/etc/hosts")
    print(content)
```

For more examples, see the example scripts under `CubeAPI/examples/` in the repository.

## Common Operations

### Stop All Services

```bash
sudo ./down.sh
```

This stops all host processes (cubelet, cubemaster, cube-api, network-agent), Docker containers (CubeProxy, CoreDNS, MySQL, Redis), and rolls back the `cube.app` DNS routing configuration.

### Reinstall

To reinstall over an existing deployment, simply run `install.sh` again. The script automatically stops the existing deployment before installing.

### View Logs

| Component | Log Path |
|-----------|----------|
| cube-api | `/data/log/CubeAPI/` |
| CubeMaster | `/data/log/CubeMaster/` |
| Cubelet | `/data/log/Cubelet/` |
| CubeShim | `/data/log/CubeShim/` |
| Hypervisor (VMM) | `/data/log/CubeVmm/` |
| CubeProxy | `/data/log/cube-proxy/` |
| Runtime PID files | `/var/run/cube-sandbox-one-click/` |
| Process stdout/stderr | `/var/log/cube-sandbox-one-click/` |

## Configuration Reference

All configuration is managed through the `.env` file. Below is the full parameter reference.

### Build-time Options

| Variable | Default | Description |
|----------|---------|-------------|
| `BUILDER_IMAGE` | `cube-sandbox-builder:latest` | Docker image used for compilation |
| `ONE_CLICK_CUBEMASTER_BUILD_MODE` | `local` | Build mode for CubeMaster (`local` = compile from source) |
| `ONE_CLICK_CUBELET_BUILD_MODE` | `local` | Build mode for Cubelet |
| `ONE_CLICK_CUBE_API_BUILD_MODE` | `local` | Build mode for cube-api |
| `ONE_CLICK_NETWORK_AGENT_BUILD_MODE` | `local` | Build mode for network-agent |
| `ONE_CLICK_CUBE_AGENT_BUILD_MODE` | `local` | Build mode for cube-agent |
| `ONE_CLICK_CUBE_SHIM_BUILD_MODE` | `local` | Build mode for CubeShim |
| `ONE_CLICK_CUBE_KERNEL_VMLINUX` | `assets/kernel-artifacts/vmlinux` | Path to the vmlinux kernel file |

You can also point to prebuilt binaries to skip compilation:

| Variable | Description |
|----------|-------------|
| `ONE_CLICK_CUBEMASTER_BIN` | Path to prebuilt cubemaster binary |
| `ONE_CLICK_CUBEMASTERCLI_BIN` | Path to prebuilt cubemastercli binary |
| `ONE_CLICK_CUBELET_BIN` | Path to prebuilt cubelet binary |
| `ONE_CLICK_CUBECLI_BIN` | Path to prebuilt cubecli binary |
| `ONE_CLICK_CUBE_API_BIN` | Path to prebuilt cube-api binary |
| `ONE_CLICK_NETWORK_AGENT_BIN` | Path to prebuilt network-agent binary |
| `ONE_CLICK_CUBE_AGENT_BIN` | Path to prebuilt cube-agent binary |
| `ONE_CLICK_CUBESHIM_BIN` | Path to prebuilt containerd-shim-cube-rs binary |
| `ONE_CLICK_CUBE_RUNTIME_BIN` | Path to prebuilt cube-runtime binary |

### Target Machine Options

| Variable | Default | Description |
|----------|---------|-------------|
| `ONE_CLICK_DEPLOY_ROLE` | `control` | Deployment role: `control` for single-node (default). For compute-only nodes, see [Multi-Node Cluster Deployment](./multi-node-deploy) |
| `ONE_CLICK_CONTROL_PLANE_IP` | empty | Compute-node mode only. See [Multi-Node Cluster Deployment](./multi-node-deploy#step-2-configure-environment-variables) |
| `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` | empty | Compute-node mode only. See [Multi-Node Cluster Deployment](./multi-node-deploy#step-2-configure-environment-variables) |
| `CUBE_SANDBOX_NODE_IP` | auto-detected from `eth0` | Node's primary network interface IP. Auto-detected if unset; set explicitly if your interface differs. |
| `ONE_CLICK_INSTALL_PREFIX` | `/usr/local/services/cubetoolbox` | Installation directory |
| `ONE_CLICK_RUN_QUICKCHECK` | `1` | Run health check after installation |
| `ONE_CLICK_RUNTIME_DIR` | `/var/run/cube-sandbox-one-click` | PID and runtime files directory |
| `ONE_CLICK_LOG_DIR` | `/var/log/cube-sandbox-one-click` | Process stdout/stderr log directory |

### Database and Cache

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBE_SANDBOX_MYSQL_CONTAINER` | `cube-sandbox-mysql` | MySQL container name |
| `CUBE_SANDBOX_REDIS_CONTAINER` | `cube-sandbox-redis` | Redis container name |
| `CUBE_SANDBOX_MYSQL_PORT` | `3306` | MySQL port |
| `CUBE_SANDBOX_REDIS_PORT` | `6379` | Redis port |
| `CUBE_SANDBOX_MYSQL_ROOT_PASSWORD` | `cube_root` | MySQL root password |
| `CUBE_SANDBOX_MYSQL_DB` | `cube_mvp` | MySQL database name |
| `CUBE_SANDBOX_MYSQL_USER` | `cube` | MySQL user |
| `CUBE_SANDBOX_MYSQL_PASSWORD` | `cube_pass` | MySQL user password |
| `CUBE_SANDBOX_REDIS_PASSWORD` | `ceuhvu123` | Redis password |

### CubeProxy and DNS

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBE_PROXY_ENABLE` | `1` | Enable CubeProxy (must be `1` for one-click) |
| `CUBE_PROXY_HOST_PORT` | `443` | CubeProxy listen port |
| `CUBE_PROXY_DNS_ENABLE` | `1` | Enable CoreDNS (must be `1` for one-click) |
| `CUBE_PROXY_DNS_ANSWER_IP` | `${CUBE_SANDBOX_NODE_IP}` | IP returned by CoreDNS for `cube.app` |
| `CUBE_PROXY_COREDNS_BIND_ADDR` | `127.0.0.54` | CoreDNS bind address |
| `ONE_CLICK_MKCERT_BIN` | `assets/bin/mkcert` (bundled) | Override path to mkcert binary at build time |
| `ALPINE_MIRROR_URL` | Tsinghua mirror | Alpine package mirror for CubeProxy build |
| `PIP_INDEX_URL` | Tsinghua mirror | PyPI mirror for CubeProxy build |

### Process Addresses

| Variable | Default | Description |
|----------|---------|-------------|
| `CUBEMASTER_ADDR` | `127.0.0.1:8089` | CubeMaster listen address |
| `NETWORK_AGENT_HEALTH_ADDR` | `127.0.0.1:19090` | network-agent health endpoint |
| `CUBE_API_BIND` | `0.0.0.0:3000` | cube-api listen address |
| `CUBE_API_HEALTH_ADDR` | `127.0.0.1:3000` | cube-api health check address |
| `CUBE_API_SANDBOX_DOMAIN` | `cube.app` | Sandbox domain for CubeProxy routing |

### Docker Mirror (Optional)

| Variable | Default | Description |
|----------|---------|-------------|
| `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR` | `1` | Enable Tencent Cloud Docker registry mirror |
| `ONE_CLICK_TENCENT_DOCKER_MIRROR_URL` | `https://mirror.ccs.tencentyun.com` | Mirror URL |

## Installed Directory Structure

After installation, the deployment is located at `/usr/local/services/cubetoolbox/` (default):

```
/usr/local/services/cubetoolbox/
├── CubeAPI/bin/cube-api                  # E2B-compatible API server
├── CubeMaster/
│   ├── bin/cubemaster                # Orchestration service
│   ├── bin/cubemastercli             # CLI tool
│   └── conf.yaml                     # CubeMaster configuration
├── Cubelet/
│   ├── bin/cubelet                   # Node agent
│   ├── bin/cubecli                   # CLI tool
│   ├── config/                       # Cubelet configuration
│   └── dynamicconf/                  # Dynamic configuration
├── network-agent/
│   ├── bin/network-agent             # Network orchestration service
│   └── network-agent.yaml            # Configuration
├── cube-shim/bin/
│   ├── containerd-shim-cube-rs       # containerd shim
│   └── cube-runtime                  # Runtime binary
├── cube-image/
│   └── cube-guest-image-cpu.img      # Guest VM image
├── cube-kernel-scf/                  # Kernel artifacts
├── cubeproxy/                        # CubeProxy Docker Compose and configs
├── coredns/                          # CoreDNS Docker Compose
├── support/                          # MySQL/Redis Docker Compose + bundled mkcert
├── sql/                              # Database schema and seed data
├── scripts/one-click/                # Runtime management scripts
└── .one-click.env                    # Active environment configuration
```

## Troubleshooting

### Docker Not Installed or Not Running

```
Error: required command not found: docker
```

Install Docker following the [official documentation](https://docs.docker.com/engine/install/) and ensure the Docker daemon is running:

```bash
sudo systemctl start docker
sudo systemctl enable docker
```

### KVM Not Available

If `/dev/kvm` does not exist, KVM is not enabled. Ensure you are running on a physical machine (not a VM) and that the kernel supports KVM:

```bash
# Check KVM support
lsmod | grep kvm
```

For Intel CPUs, you need `kvm_intel`; for AMD, `kvm_amd`.

### DNS Routing Failure

The install script configures `cube.app` split DNS via `systemd-resolved` or `NetworkManager + dnsmasq`. If neither is available, installation will fail.

**Workaround**: Manually add DNS entries for `*.cube.app` pointing to `CUBE_SANDBOX_NODE_IP` in `/etc/hosts` or your DNS server.

### mkcert Not Working

The `mkcert` binary is bundled inside the release package and automatically installed to `/usr/local/bin/mkcert` during deployment. If you need to use a different version, either pre-install `mkcert` to the system PATH before running `install.sh`, or override the binary at build time via the `ONE_CLICK_MKCERT_BIN` environment variable.

### MySQL/Redis Container Startup Failure

Check Docker logs:

```bash
docker logs cube-sandbox-mysql
docker logs cube-sandbox-redis
```

Common causes:
- Port conflicts (3306 or 6379 already in use)
- Insufficient disk space for Docker volumes
- Docker daemon issues

### ripgrep (`rg`) Not Installed

```
Error: required command not found: rg
```

Install ripgrep:

```bash
# Ubuntu/Debian
sudo apt-get install ripgrep

# CentOS/RHEL
sudo yum install ripgrep
```

## Known Limitations

- If `vmlinux` is missing from `assets/kernel-artifacts/`, the build will fail immediately.
- If the build machine's `mkfs.ext4` does not support the `-d` flag, guest image generation will fail.
- `cube-snapshot/spec.json` is not a mandatory artifact in the current one-click release; missing it will cause a warning but not block startup.
- Environments lacking both `systemd-resolved` and `NetworkManager` are not currently supported for automatic DNS configuration.
