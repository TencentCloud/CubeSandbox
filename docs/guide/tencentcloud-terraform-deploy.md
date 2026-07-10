# Tencent Cloud Cluster Deployment (Terraform)

This guide explains how to use the Terraform deployer shipped in the release bundle to stand up a **clustered** Cube Sandbox on Tencent Cloud in one shot: a managed TKE control plane running `cube-master` / `cube-api` / `cube-proxy` / `cube-lifecycle-manager` / `cube-webui`, backed by cloud MySQL + Redis, with one or more CVM PVM compute nodes. A jumpserver (SSH on port `443`) acts as the build host and the bastion for the otherwise-private VPC.

::: tip Network Hardening
Network hardening for the cluster deployment is handled by **Tencent Cloud security groups**: the deployer creates **4 per-role security groups** (jumpserver / compute / TKE pod / CLB), each opening only the ingress that role actually needs on a least-privilege basis (compute and TKE nodes get no public ingress at all), and assigns no public IP to compute nodes. **The default is internal mode** (`TENCENTCLOUD_ENABLE_PUBLIC_NETWORK='false'`): the three user-facing services — WebUI / cube-api / cube-proxy — are fronted by **VPC-internal CLBs**, reachable only from inside the VPC (via the jumpserver / VPN) with no public exposure. To allow public access, explicitly set `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK='true'` to switch them to public CLBs. To tighten further, adjust the inbound/outbound rules of each individual group (`cubesandbox-sg-jumpserver` / `cubesandbox-sg-compute` / `cubesandbox-sg-tke-pod` / `cubesandbox-sg-clb`) as needed in the [Tencent Cloud security group console](https://console.cloud.tencent.com/vpc/securitygroup). **When public mode is enabled**, also see [Hardening the Public-Facing Services](#hardening-the-public-facing-services).
:::

::: tip When to use this
This deployment uses cloud resources to **quickly stand up a highly-available CubeSandbox service**: all cloud resources default to pay-as-you-go billing (see [Billing Mode](#billing-mode) below) and can be released in one shot with `destroy.sh`. For long-term use, switch to **prepaid (monthly/yearly subscription)** resources for better cost savings (see [Billing Mode](#billing-mode)). If you only need a single-machine deployment for validation, see the earlier deployment guides: [PVM Deployment](./pvm-deploy.md) or [Bare-Metal Deployment](./bare-metal-deploy.md).

**Note**: The default configuration is a **POC / functional validation** setup (2× `SA9.MEDIUM8` compute nodes, single-replica control plane, no CFS). It can host only a limited number of sandboxes. For production or load testing, adjust compute node and TKE worker specs and counts — see [Node Specifications & Capacity Planning](#node-specifications--capacity-planning) and [Default Deployment Mode](#default-deployment-mode).
:::

## Architecture Overview

```
                          Internet
                            │
               ┌────────────┴────────────────────┐
               │                                 │
      ┌────────┴────────┐             ┌──────────┴─────────┐
      │  Jumpserver CVM │             │ CLB (internal/     │
      │  (public IP)    │             │      public)       │
      │  SSH:443        │             │  cube-api :3000    │
      │  build & push   │             │  cube-proxy :80/443 │
      └────────┬────────┘             │  cube-webui :80    │
               │                      └──────────┬─────────┘
               │                                 │
  ┌────────────┼─────────────────────────────────┼──────────┐
  │            │            VPC internal          │           │
  │  ┌─────────┴────────┐       ┌───────────────┴─────┐    │
  │  │ Compute CVM ×N   │       │  TKE managed cluster      │
  │  │  Cubelet         │       │  cube-master (×1)         │
  │  │  network-agent   │       │  cube-api (×1)            │
  │  │  CubeEgress      │       │  cube-proxy (×1)          │
  │  └──────────────────┘       │  cube-lifecycle-manager   │
  │                             │  cube-webui (×1)          │
  │                             └───┬─────────────┬─────────┘
  │                                 │             │         │
  │                    ┌────────────┴───┐ (opt.)  │         │
  │                    │  CFS (NFS)     │         │         │
  │                    │  shared store  │         │         │
  │                    └────────────────┘         │         │
  │                                               │         │
  │                         ┌─────────────────────┴───┐     │
  │                         │  Cloud DB: MySQL + Redis │     │
  │                         └─────────────────────────┘     │
  │                                                         │
  │  ┌──────────────┐       ┌──────────────┐                │
  │  │ TCR (opt.)   │       │  NAT + EIP   │→ public egress │
  │  └──────────────┘       └──────────────┘                │
  └─────────────────────────────────────────────────────────┘
```

| Component | Form | Notes |
|-----------|------|-------|
| Jumpserver | CVM (public IP, SSH 443) | Build host (TCR mode) and bastion into the private VPC |
| Load balancer | CLB (internal/public) | Fronts `cube-api` / `cube-proxy` / `cube-webui`; internal mode by default, user traffic entry point |
| Control plane | Managed TKE cluster | Runs `cube-master` / `cube-api` / `cube-proxy` / `cube-lifecycle-manager` / `cube-webui` |
| Compute node | CVM PVM | Runs `Cubelet` / `network-agent` / `CubeEgress`; **actually hosts sandboxes** |
| Database | Cloud MySQL 8.0 + Redis 7.0 | VPC-internal only, no public access |
| Shared storage | CFS (General Standard NFS, **optional**) | When `USE_CFS=true` and cubemaster has multiple replicas, ReadWriteMany share for `/data/CubeMaster/storage` |
| Registry | TCR (basic, **optional**) | When `USE_TCR=true`; the jumpserver builds and pushes component images |
| Egress | NAT gateway + EIP | The whole VPC reaches the internet through NAT |

::: info TKE workers and PVM compute nodes are separate resources
- **`TENCENTCLOUD_TKE_NODE_COUNT`**: number of TKE **workers** running control-plane Pods (cubemaster / cube-api / etc.).
- **`TENCENTCLOUD_COMPUTE_NODE_COUNT`**: number of **PVM compute nodes** running Cubelet; **sandboxes execute here**.
Both default to `2` but serve completely different roles.
:::

## Default Deployment Mode

Matching `env.example` / `variables.tf`, the **default is public images + single-replica control plane + no CFS** — a POC configuration:

| Setting | Default | Notes |
|---------|---------|-------|
| `TENCENTCLOUD_USE_TCR` | `false` | No TCR; uses public pre-built images, no build on the jumpserver |
| `TENCENTCLOUD_USE_CFS` | `false` | No CFS; cubemaster uses Pod-local storage |
| `TENCENTCLOUD_CUBEMASTER_REPLICAS` etc. | `1` | Control-plane components default to single replica |
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | `2` | PVM compute nodes |
| `TENCENTCLOUD_TKE_NODE_COUNT` | `2` | TKE workers (`worker_config.count`) |
| `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK` | `false` | cube-api / cube-proxy / cube-webui use VPC-internal CLBs |

**Advanced modes:**

- `TENCENTCLOUD_USE_TCR=true`: create TCR and build/push the five component images on the jumpserver.
- `TENCENTCLOUD_USE_CFS=true` with `TENCENTCLOUD_CUBEMASTER_REPLICAS>1`: create CFS for cubemaster multi-replica shared storage.
- `TENCENTCLOUD_CUBE_PROXY_REPLICAS>1`: run cube-proxy with multiple replicas behind the same CLB.

`cube-proxy` supports **multiple replicas**. The default remains a single replica (`TENCENTCLOUD_CUBE_PROXY_REPLICAS=1`) for small deployments, but you can increase it when proxy traffic grows. Each cube-proxy replica registers its admin endpoint in Redis, and the standalone `cube-lifecycle-manager` discovers replicas from that registry for auto-pause / auto-resume coordination, so no static replica list is required.

## Resources Created by the Default Configuration

The table below lists cloud resources created under the **default configuration** (region `ap-guangzhou`, zone `ap-guangzhou-6`, 2 compute nodes, 2 TKE workers, no CFS/TCR). All resources are **pay-as-you-go** (see [Billing Mode](#billing-mode)).

| Resource | Count | Spec / Configuration |
|----------|-------|----------------------|
| VPC | 1 | CIDR `10.0.0.0/16` |
| Subnet | 1 | `10.0.1.0/24` (primary zone; extra /24 subnets only when a role lands in another zone) |
| NAT gateway + EIP | 1 + 1 | 200 Mbps bandwidth, pay-by-traffic |
| Route table entry | 1 | `0.0.0.0/0` → NAT gateway |
| Security group | 4 | Per-role (jumpserver / compute / TKE pod / CLB), least privilege, see below |
| SSH key pair | 1 | Auto-generated under `terraform/tencentcloud/.ssh/` |
| Jumpserver CVM | 1 | `SA9.MEDIUM4` (2C4G), 50GB general-purpose SSD system disk, 200 Mbps public bandwidth, SSH on port 443 |
| Compute CVM | 2 | `SA9.MEDIUM8` (4C8G), 50GB system disk + **200GB CBS data disk** (XFS, `/data/cubelet`), **no public IP** |
| TKE managed cluster | 1 | Managed **L5**, Kubernetes `1.34.1`, Pod CIDR `10.200.0.0/16`, Service CIDR `192.168.0.0/20`, VPC-internal API only |
| TKE worker nodes | 2 | `SA9.LARGE8` (4C8G), created via `worker_config.count` (**no separate node pool**) |
| Cloud MySQL | 1 | 8.0 InnoDB universal, 4GB memory / 200GB storage, multi-AZ (when the region has ≥2 zones) / semi-sync, intranet 3306 only |
| Cloud Redis | 1 | 7.0 standard architecture (master/replica), 1GB memory, port 6379, intranet only |
| CFS file system | 0 (default) | 1 General Standard NFS when `USE_CFS=true` |
| TCR registry | 0 (default) | Basic + namespace + VPC attachment when `USE_TCR=true` |
| OS image | — | OpenCloudOS Server 9 (public image, reused by all CVMs) |

::: tip OS image
All CVMs (jumpserver / compute / TKE workers) default to the **OpenCloudOS Server 9** public image; override with `TENCENTCLOUD_IMAGE_NAME`.
:::

### Security Group Ingress Ports

The deployer creates **4 per-role security groups** on a **least-privilege** basis; each opens only the ingress that role actually needs, so compromising one role never inherits the inbound surface of the others.

**1. `cubesandbox-sg-jumpserver` (jumpserver)**

| Port / Range | Source | Purpose |
|--------------|--------|---------|
| TCP 443 | `0.0.0.0/0` | Jumpserver SSH (cloud-init moves sshd to 443) |
| ALL | `10.0.0.0/16` | VPC-internal traffic |

**2. `cubesandbox-sg-compute` (compute nodes)** — no public ingress

| Port / Range | Source | Purpose |
|--------------|--------|---------|
| ALL | TKE Pod CIDR | cube-proxy (pod) → all ports on compute nodes (sandbox dynamic ports 20000-29999) |
| ALL | `10.0.0.0/16` | VPC-internal traffic (jumpserver management, cube-master scheduling) |

**3. `cubesandbox-sg-tke-pod` (TKE workers)** — no public ingress

| Port / Range | Source | Purpose |
|--------------|--------|---------|
| ALL | TKE Pod CIDR | Pod-to-pod communication |
| ALL | `10.0.0.0/16` | VPC-internal (CLB health checks, jumpserver management, CFS NFS) |

**4. `cubesandbox-sg-clb` (load balancers)**

The source for the 80 / 443 / 3000 entrypoints below depends on `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK`: in the **default internal mode (`false`)** the source is the VPC CIDR `10.0.0.0/16` (VPC-internal only); in **public mode (`true`)** the source is `0.0.0.0/0` (open to the internet).

| Port / Range | Source (internal / public mode) | Purpose |
|--------------|--------|---------|
| TCP 80 | `10.0.0.0/16` / `0.0.0.0/0` | CLB for cube-proxy + cube-webui (HTTP) |
| TCP 443 | `10.0.0.0/16` / `0.0.0.0/0` | CLB for cube-proxy (HTTPS) |
| TCP 3000 | `10.0.0.0/16` / `0.0.0.0/0` | CLB for cube-api |
| TCP 8089 | `10.0.0.0/16` (always VPC-internal only) | Internal CLB for cube-master (unaffected by the flag) |

Egress allows all (`0.0.0.0/0` ALL) by default on every group. Databases, the TKE API server, etc. are **not exposed publicly** — VPC-internal access only.

### Hardening the Public-Facing Services

> This section applies only when **public mode is enabled** (`TENCENTCLOUD_ENABLE_PUBLIC_NETWORK='true'`). In the default internal mode none of the three services are exposed publicly, so you can skip it.

With public mode enabled, `cubesandbox-sg-clb` opens three public entrypoints to `0.0.0.0/0`: WebUI (80), cube-proxy (80 / 443), and cube-api (3000). Their security models differ, so harden each one separately:

- **WebUI (CLB 80):** the WebUI console currently ships **without any authentication or access control** — anyone who can reach it can operate sandboxes. Strongly consider creating a **dedicated security group** for the WebUI CLB and configuring a **strict source-IP allowlist** (only your office / management egress IPs) instead of reusing the publicly-open `cubesandbox-sg-clb`. Create the group in the [Tencent Cloud security group console](https://console.cloud.tencent.com/vpc/securitygroup) and bind it to the WebUI CLB instance.
- **cube-api (CLB 3000):** cube-api **lets every request through without credential checks** by default. Before exposing it publicly, enable **Auth Callback** authentication to delegate authorization decisions to your own auth service — see [Authentication](./authentication.md).
- **cube-proxy (CLB 80 / 443):** cube-proxy is the public ingress for sandbox traffic and is public-facing by design. To restrict public access to sandboxes, see [Restrict Public Access](./restrict-public-access.md) to enable mechanisms such as per-sandbox inbound tokens.

## Prerequisites

The machine running `create.sh` only needs the following — **no pre-installed Terraform required**:

- **Tencent Cloud API credentials:** create an API key pair in the [CAM console](https://console.cloud.tencent.com/cam/capi) and export `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY`.
- **Local tools:** `ssh`, `scp`, `nc`, plus network access to the Tencent Cloud APIs. `terraform` and `jq` are auto-installed when missing — into `/usr/local/bin` when writable (e.g. running as root), otherwise into a local `.bin/`. `terraform` is fetched from the HashiCorp release site (needs `curl`/`wget` + `unzip`); `jq` comes from the system package manager or a static binary from GitHub.
- `mkcert` / `openssl` are **not** needed locally — the cube-proxy certificate is produced on the jumpserver.
- **Build / deploy host:** Linux recommended; `create.sh` / `destroy.sh` also support macOS (including Bash 3.2). Use WSL2 on Windows.

### Terraform Provider Init Acceleration (Recommended)

By default, `terraform init` downloads providers from `registry.terraform.io` and GitHub. In mainland China networks, installing the `tencentcloudstack/tencentcloud` provider may time out while fetching checksums or release packages, with errors such as `failed to retrieve authentication checksums` or `Client.Timeout exceeded while awaiting headers`. Tencent Cloud provides a Terraform provider mirror that can be enabled through the Terraform CLI configuration file. See Tencent Cloud's [Init acceleration guide](https://cloud.tencent.com/document/product/1653/82912).

On Linux / macOS, create or update `~/.terraformrc` for the current user:

```hcl
provider_installation {
  network_mirror {
    url = "https://mirrors.tencent.com/terraform/"
    include = ["registry.terraform.io/tencentcloudstack/*"]
  }

  direct {
    exclude = ["registry.terraform.io/tencentcloudstack/*"]
  }
}
```

### Pre-deployment setup (service activation & authorization)

Complete these **before the first `create.sh` apply** to avoid half-created resources mid-run.

#### 1. Account basics

- [Register a Tencent Cloud account](https://cloud.tencent.com/register) and complete **real-name verification**.
- For sub-accounts, grant permissions to create VPC / CVM / TKE / CDB / Redis / CLB resources. See [Authorize with TKE preset policies](https://cloud.tencent.com/document/product/457/46033) to attach policies such as `QcloudTKEFullAccess`.

#### 2. TKE service role authorization (required)

Before first use of TKE you must grant **service roles**, otherwise cluster creation, CLB-type Services, etc. will fail.

1. Log in to the [TKE console](https://console.cloud.tencent.com/tke2) and complete service authorization when prompted.
2. Confirm roles such as `TKE_QCSRole` / `IPAMDofTKE_QCSRole` are authorized.

Official docs: [Service authorization role permissions](https://cloud.tencent.com/document/product/457/43416), [TKE quick start](https://cloud.tencent.com/document/product/457/6759).

#### 3. Private DNS (as needed)

| Scenario | Required? |
|----------|-----------|
| Default `USE_TCR=false` | Usually not |
| `USE_TCR=true` | Recommended |
| E2B SDK access to `*.cube.app` | **Yes** (see [E2B and the cube.app domain](#e2b-and-the-cubeapp-domain)) |

Without activation the API may return `ResourceNotFound.ServiceNotSubscribed` (Private DNS service not subscribed).

Activation: [DNSPod Private DNS](https://console.dnspod.cn/privateDNS) → accept agreement → activate. Docs: [Private DNS product overview](https://cloud.tencent.com/document/product/1338/50527), [Activate Private DNS](https://cloud.tencent.com/document/product/1338/50533).

#### 4. Cloud File Storage CFS (as needed)

Only when `TENCENTCLOUD_USE_CFS=true` and cubemaster runs multiple replicas.

1. Log in to the [CFS console](https://console.cloud.tencent.com/cfs) and **activate CFS** when prompted.
2. Terraform creates the file system during apply (no manual creation needed).

Docs: [CFS quick start](https://cloud.tencent.com/document/product/582/9132). If the account is not activated the console may show no instances; prefer running `destroy.sh`'s CFS stage on teardown.

#### 5. Other service preflight

| Service | Console |
|---------|---------|
| VPC / NAT | [VPC](https://console.cloud.tencent.com/vpc) |
| CVM | [CVM](https://console.cloud.tencent.com/cvm) |
| TKE | [TKE](https://console.cloud.tencent.com/tke2) |
| MySQL / Redis | [MySQL](https://console.cloud.tencent.com/cdb) / [Redis](https://console.cloud.tencent.com/redis) |
| CLB | [CLB](https://console.cloud.tencent.com/clb) |
| TCR (`USE_TCR=true`) | [TCR](https://console.cloud.tencent.com/tcr) |

## Quick Start

The deployer is surfaced at the **top level** of the extracted bundle, so you can run it directly after extracting:

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>

# Copy the environment template, fill in your credentials (TENCENTCLOUD_SECRET_ID / TENCENTCLOUD_SECRET_KEY) and edit the rest as needed
cp terraform/tencentcloud/env.example terraform/tencentcloud/.env
# $EDITOR terraform/tencentcloud/.env

./terraform/tencentcloud/create.sh
```

`create.sh` auto-loads the `.env` next to it (filling in only variables not already set in the current shell), so credentials can either go straight into `.env` or be injected via `export` — the latter takes precedence and overrides the matching value in `.env`:

```bash
export TENCENTCLOUD_SECRET_ID="your-secret-id"
export TENCENTCLOUD_SECRET_KEY="your-secret-key"
```

See [Configuration](#configuration) below for the meaning and defaults of each `.env` option; anything left unset falls back to the defaults.

`create.sh` runs entirely from the extracted bundle and automatically:

1. Auto-detects the local bundle (the outer `cube-sandbox-one-click-<version>.tar.gz`, or re-packs the extracted directory if the tarball is gone) and uses it as the **offline source** for component images and compute-node installation. When detected — or set via `TENCENTCLOUD_LOCAL_BUNDLE=/path/to.tar.gz` — no public download is required; otherwise the jumpserver falls back to an **online install** (needs public network).
2. Generates an SSH key pair under `terraform/tencentcloud/.ssh/` if none exists.
3. Generates the cube-proxy TLS certificate (`cube.app` / `*.cube.app`) on the jumpserver with the bundled `mkcert`, downloading it to `terraform/tencentcloud/cubeproxy-certs/` for the Secret mount.
4. **Default mode** (`USE_TCR=false`): pull public pre-built images and deploy TKE addons and CVM compute nodes (2 by default).
5. **TCR mode** (`USE_TCR=true`): create TCR, build and push the five component images on the jumpserver, then deploy TKE and compute nodes.

## Configuration

Common environment variables (matching the `create.sh` / `variables.tf` defaults) are listed in `terraform/tencentcloud/env.example`, which you can copy to `.env` and fill in:

```bash
export TENCENTCLOUD_REGION=ap-guangzhou
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_NODE_COUNT=2              # CVM PVM compute nodes
export TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE=200        # CBS data disk per compute node (GB)
export TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY=1s
export TENCENTCLOUD_TKE_NODE_COUNT=2                 # TKE worker nodes (control-plane Pods)
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_USE_TCR=false                    # default: public pre-built images
export TENCENTCLOUD_USE_CFS=false                    # default: no CFS
export TENCENTCLOUD_CUBE_IMAGE_TAG=v0.5.1-rc5
export TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS=1
```

### Common Variables

> When running `create.sh` interactively, the "Deployment configuration" step walks you through most of the settings below (region / VPC / instance types / passwords / image tag, etc.), including a `[y/N]` prompt for **whether to enable public-network mode** (default N = internal). Set any of these variables beforehand to skip the matching prompt; a non-interactive run (no TTY) silently uses the defaults.

| Variable | Default | Description |
|----------|---------|-------------|
| `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY` | none | **Required.** Tencent Cloud API credentials |
| `TENCENTCLOUD_REGION` | `ap-guangzhou` | Region |
| `TENCENTCLOUD_AVAILABILITY_ZONE` | `ap-guangzhou-6` | Primary zone (subnet / MySQL / Redis / TKE control plane) |
| `TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE` | `SA9.MEDIUM4` | Jumpserver instance type |
| `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` | `SA9.MEDIUM8` | Preferred compute-node instance type |
| `TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE` | `SA9.LARGE8` | TKE worker instance type (4C8G) |
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | `2` | **PVM compute nodes** (run Cubelet / sandboxes) |
| `TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE` | `200` | CBS data disk size per compute node (GB, XFS, mounted at `/data/cubelet`). Sandbox image templates, snapshots and runtime data on the compute node all live under this directory — size it to your actual needs |
| `TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY` | `1s` | Cubelet node status/resource reporting interval to CubeMaster. `create.sh` patches `Cubelet/config/config.toml` on each compute node |
| `TENCENTCLOUD_TKE_NODE_COUNT` | `2` | **TKE workers** (run control-plane Pods; maps to `worker_config.count`) |
| `TENCENTCLOUD_USE_TCR` | `false` | When `true`, create TCR and build/push images on the jumpserver; when `false`, use public pre-built images |
| `TENCENTCLOUD_USE_CFS` | `false` | When `true` and cubemaster has multiple replicas, create CFS NFS shared storage |
| `TENCENTCLOUD_TKE_CLUSTER_VERSION` | `1.34.1` | TKE Kubernetes version |
| `TENCENTCLOUD_MYSQL_PASSWORD` | insecure demo value | MySQL root password (change for real use) |
| `TENCENTCLOUD_REDIS_PASSWORD` | insecure demo value | Redis password (change for real use) |
| `TENCENTCLOUD_CUBE_DB` / `TENCENTCLOUD_CUBE_USER` / `TENCENTCLOUD_CUBE_PASSWORD` | `cube_mvp` / `cube` / demo | Application DB name / account / password |
| `TENCENTCLOUD_CUBEMASTER_REPLICAS` | `1` | cube-master replica count |
| `TENCENTCLOUD_CUBE_API_REPLICAS` | `1` | cube-api replica count |
| `TENCENTCLOUD_CUBE_PROXY_REPLICAS` | `1` | cube-proxy replica count. Values greater than `1` are supported; each replica registers in Redis for cube-lifecycle-manager discovery |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS` | `1` | cube-lifecycle-manager replica count. Keep `1` unless CLM HA behavior has been validated for your deployment |
| `TENCENTCLOUD_CUBE_WEBUI_REPLICAS` | `1` | cube-webui replica count |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DEFAULT_IDLE_TIMEOUT` | `5m` | Default idle timeout used when lifecycle metadata omits `TimeoutSeconds` |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_HEARTBEAT_TTL` | `15s` | TTL for cube-proxy Redis registry heartbeats |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DISCOVERY_REFRESH` | `3s` | Redis discovery scan interval for cube-lifecycle-manager |
| `TENCENTCLOUD_CUBE_PROXY_HEARTBEAT_INTERVAL_MS` | `5000` | cube-proxy registry heartbeat interval in milliseconds |
| `TENCENTCLOUD_CUBE_ADMIN_TOKEN` | empty | Shared token for cube-lifecycle-manager -> cube-proxy `/admin/*` calls. Leave empty to auto-generate; custom values must be at least 16 characters |
| `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK` | `false` | Network exposure mode for cube-api / cube-proxy / cube-webui. **Default `false`**: VPC-internal CLBs, reachable only from inside the VPC (via jumpserver / VPN). Set to `true` for public CLBs reachable from the internet, with the security group opening `0.0.0.0/0` accordingly. cube-master always stays VPC-internal. Read [Hardening the Public-Facing Services](#hardening-the-public-facing-services) before enabling |

### Non-interactive / CI runs

Without a TTY the interactive menus fall back to defaults, so set them explicitly to stay in control. The password variables are the exception: a non-interactive run **refuses** to start with the built-in, publicly-known demo passwords and requires them to be set — or set `TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1` to opt into the insecure defaults for a throwaway sandbox.

```bash
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_LOCAL_BUNDLE=/path/to/cube-sandbox-one-click-<version>.tar.gz  # auto-detected inside an extracted bundle
export TENCENTCLOUD_PVM_KERNEL_VMLINUX=/path/to/vmlinux-pvm  # only if the bundle ships no vmlinux-pvm
export TENCENTCLOUD_MYSQL_PASSWORD=...    # required for non-interactive runs (no insecure fallback)
export TENCENTCLOUD_REDIS_PASSWORD=...    # required for non-interactive runs
export TENCENTCLOUD_CUBE_PASSWORD=...     # required for non-interactive runs
export TENCENTCLOUD_BUILD_IMAGES=0        # TCR mode: reuse already-pushed images
```

More advanced toggles (`TENCENTCLOUD_VERBOSE`, `TENCENTCLOUD_REINSTALL`, `TENCENTCLOUD_RESET_DB`, SSH port/key paths, etc.) are documented in the `create.sh` header comments.

## Node Specifications & Capacity Planning

### Default Configuration

The default deployment provisions **2× 4C8G compute nodes** (`SA9.MEDIUM8`, 200GB data disk each) and **2 TKE workers** — suitable for POC / functional validation with limited sandbox capacity. Default specs per role:

| Role | Default Type | Specs | Count |
|------|-------------|-------|-------|
| Compute node (PVM) | `SA9.MEDIUM8` | 4C8G + 200GB data disk | 2 |
| TKE Worker | `SA9.LARGE8` | 4C8G | 2 (`worker_config.count`) |
| Jumpserver | `SA9.MEDIUM4` | 2C4G | 1 |
| Control-plane Pods | — | cubemaster / cube-api / cube-proxy / cube-lifecycle-manager / cube-webui ×1 each | on TKE workers |

::: warning
The default configuration is only suitable for functional validation and small-scale evaluation. For large-scale production usage, you **must adjust the compute node and TKE node specs and counts**, otherwise you will encounter CPU/memory exhaustion, Pod scheduling failures, and sandbox creation timeouts.
:::

### Adjusting Compute Nodes

Compute nodes are the machines that actually run sandbox containers. Use the following environment variables to adjust their specs, count, and disk size:

| Environment Variable | Description |
|---------------------|-------------|
| `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` | Compute node instance type |
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | Number of compute nodes |
| `TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE` | Data disk size (GB) |

**Example: scale up specs and count** (`.env` configuration):

```bash
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='SA9.4XLARGE32'
TENCENTCLOUD_COMPUTE_NODE_COUNT='4'
TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE='500'
```

**Example: use higher specs** (`.env` configuration):

```bash
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='SA9.16XLARGE128'
TENCENTCLOUD_COMPUTE_NODE_COUNT='8'
TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE='1000'
```

::: tip Bare Metal Servers
For stronger compute isolation and performance (avoiding virtualization overhead), you can use [Tencent Cloud Bare Metal Servers](https://cloud.tencent.com/product/cbm) as compute nodes. Simply set `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` to a bare metal instance type (e.g. `BMS5.12XLARGE192`) — the rest of the deployment process remains the same.
:::

**Example: heterogeneous mix** (using Terraform variables directly, per-node instance types):

```bash
export TF_VAR_compute_node_count=5
export TF_VAR_compute_instance_types='["SA9.8XLARGE64","SA9.8XLARGE64","SA9.4XLARGE32","SA9.4XLARGE32","SA9.4XLARGE32"]'
export TF_VAR_compute_availability_zones='["ap-guangzhou-6","ap-guangzhou-7","ap-guangzhou-6","ap-guangzhou-7","ap-guangzhou-3"]'
export TF_VAR_compute_data_disk_size=1000
```

### Adjusting TKE Worker Nodes

TKE Workers run cube-master, cube-api, cube-proxy, cube-webui and other control-plane Pods. At larger scale, control-plane request volume and scheduling pressure increase significantly, requiring more resources.

| Environment Variable | Description |
|---------------------|-------------|
| `TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE` | TKE Worker node instance type |
| `TENCENTCLOUD_TKE_NODE_COUNT` | Number of TKE Worker nodes |

**Example: upgrade TKE Worker specs**:

```bash
TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE='SA9.2XLARGE16'
TENCENTCLOUD_TKE_NODE_COUNT='4'
```

Also consider increasing control-plane replica counts for higher throughput and availability (multi-replica cubemaster requires `TENCENTCLOUD_USE_CFS=true`):

```bash
TENCENTCLOUD_USE_CFS=true
TENCENTCLOUD_CUBEMASTER_REPLICAS='3'
TENCENTCLOUD_CUBE_API_REPLICAS='3'
TENCENTCLOUD_CUBE_WEBUI_REPLICAS='2'
```

### Full Example: Production Configuration

```bash
# --- Compute nodes
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='SA9.8XLARGE64'
TENCENTCLOUD_COMPUTE_NODE_COUNT='6'
TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE='1000'

# --- TKE control plane
TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE='SA9.2XLARGE16'
TENCENTCLOUD_TKE_NODE_COUNT='4'

# --- Control-plane replicas (requires CFS)
TENCENTCLOUD_USE_CFS=true
TENCENTCLOUD_CUBEMASTER_REPLICAS='3'
TENCENTCLOUD_CUBE_API_REPLICAS='3'
TENCENTCLOUD_CUBE_WEBUI_REPLICAS='2'

# --- Jumpserver can stay at default (build-only, no runtime load)
TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE='SA9.MEDIUM4'
```


## Billing Mode

::: warning Currently pay-as-you-go (POSTPAID) only
All cloud resources created by this deployer are **hard-coded to pay-as-you-go** billing. There is currently **no** environment or Terraform variable to switch to prepaid (monthly/yearly subscription, PREPAID).
:::

The charge type of each resource is fixed as follows:

| Resource | Charge field | Value | Meaning |
|----------|--------------|-------|---------|
| Jumpserver CVM | `instance_charge_type` | `POSTPAID_BY_HOUR` | Pay by hour |
| Compute CVM | `instance_charge_type` | `POSTPAID_BY_HOUR` | Pay by hour |
| Cloud MySQL | `charge_type` | `POSTPAID` | Pay-as-you-go |
| Cloud Redis | `charge_type` | `POSTPAID` | Pay-as-you-go |
| NAT gateway EIP | `internet_charge_type` | `TRAFFIC_POSTPAID_BY_HOUR` | Pay by traffic |
| CLB (cube-proxy) | annotation | `TRAFFIC_POSTPAID_BY_HOUR` | Pay by traffic (only in public mode; the annotation is absent in internal mode) |

Pay-as-you-go is the default because this deployment targets **fast evaluation**: everything can be fully released with `destroy.sh` (`terraform destroy`) when you are done, avoiding ongoing charges.

### Switching to prepaid (subscription)

For confirmed long-term use you can manually edit the charge fields of the matching resources in `terraform/tencentcloud/main.tf` to get better pricing. For example:

```hcl
# CVM → prepaid
resource "tencentcloud_instance" "compute" {
  instance_charge_type                    = "PREPAID"
  instance_charge_type_prepaid_period     = 1                       # 1 month
  instance_charge_type_prepaid_renew_flag = "NOTIFY_AND_AUTO_RENEW" # auto-renew on expiry
  # ...
}

# MySQL → prepaid
resource "tencentcloud_mysql_instance" "mysql" {
  charge_type     = "PREPAID"
  prepaid_period  = 1   # 1 month
  auto_renew_flag = 1   # auto-renew
  # ...
}

# Redis → prepaid
resource "tencentcloud_redis_instance" "redis" {
  charge_type     = "PREPAID"
  prepaid_period  = 1
  auto_renew_flag = 1
  # ...
}
```

::: danger Prepaid caveats
- Prepaid resources are **not** auto-refunded/released by `destroy.sh` (`terraform destroy`); you must let them expire or refund them manually.
- After switching, `destroy.sh` may error or leave residual resources that must be cleaned up by hand in the [Tencent Cloud console](https://console.cloud.tencent.com/).
- Only switch when long-term use is confirmed; keep the default pay-as-you-go for evaluation/validation.
:::

## Cost Estimate

List prices vary by region, spec tier, promotions, and over time, so this guide does not embed concrete numbers. Use Tencent Cloud's official price calculators and estimate against the specs in [Resources Created by the Default Configuration](#resources-created-by-the-default-configuration):

- [Tencent Cloud price calculator (main entry)](https://buy.cloud.tencent.com/price)
- [CVM pricing](https://buy.cloud.tencent.com/price/cvm) — jumpserver, compute node, TKE workers
- [TKE pricing overview](https://www.tencentcloud.com/document/product/457/45157) — managed cluster management fee
- [Cloud MySQL pricing](https://buy.cloud.tencent.com/price/cdb) / [Cloud Redis pricing](https://buy.cloud.tencent.com/price/redis)
- [NAT gateway](https://www.tencentcloud.com/document/product/1015) / [CLB](https://www.tencentcloud.com/document/product/214) / [EIP](https://www.tencentcloud.com/document/product/1199) pricing
- [CFS pricing](https://buy.cloud.tencent.com/price/cfs) / [TCR pricing](https://www.tencentcloud.com/document/product/1051)

::: tip Controlling cost
All resources default to pay-as-you-go (billed by the second, settled hourly). Run `destroy.sh` right after evaluation to avoid idle charges; for long-term use, switch to prepaid as described in [Billing Mode](#billing-mode).
:::

## Deployment Flow (Phased, Fail-Fast)

Resources are created in this order; the run stops at the first failed stage:

> Network (VPC / subnet / NAT) → **(when `USE_TCR=true`)** TCR → CVMs (jumpserver + compute) → **(TCR mode)** image build/push on the jumpserver → MySQL / Redis → **(when `USE_CFS=true`)** CFS shared storage → TKE cluster + Kubernetes addons → health checks → compute-node setup.

- The Kubernetes provider is only engaged after the TKE API server exists.
- On teardown, **if CFS was created**, the share is destroyed before its subnet (its NFS mount target is an ENI in that subnet).
- Terraform state lives locally under `terraform/tencentcloud/` (`*.tfstate`, gitignored — no remote backend). Keep that directory and the generated `.env` so a later `destroy.sh` or re-run can find and manage the same resources.
- Resolved selections are saved to `terraform/tencentcloud/.env` and auto-loaded on the next run; explicit environment variables always win.

## Retrying After a Partial Failure

If a stage fails part-way (e.g. an instance type / availability zone sold out in the chosen region/zone, an account quota limit, or a transient API error), you do **not** have to destroy everything and start over:

- Fix the cause — most often by **changing configuration**: pick a different `TENCENTCLOUD_AVAILABILITY_ZONE` / `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` / `TENCENTCLOUD_REGION`, raise the quota, set a password, etc. — then simply **re-run `./terraform/tencentcloud/create.sh`**.
- On a re-run, `create.sh` reloads the saved selections from `.env`, reconciles state with what already exists in the cloud (refreshing and importing stateful resources rather than recreating them), and **continues from where it left off**. Existing compute nodes are kept (it never scales down).
- Availability genuinely varies by region **and** availability zone — a type offered in one zone may be unavailable in another. The interactive zone / instance-type menus are queried live for your region, and the final choice is validated at apply time.
- Only run `destroy.sh` when you actually want to tear the deployment down; it is not required between ordinary retries.

## E2B and the cube.app Domain

Single-machine one-click install configures `*.cube.app` resolution via CoreDNS + split DNS on the host. **The Terraform cluster deployment does not include this DNS setup by default**; to create/access sandboxes through the official E2B SDK you must configure domain resolution separately (usually requires [Private DNS](#3-private-dns-as-needed)).

### Background

- cube-proxy terminates TLS for `cube.app` / `*.cube.app` (certificates generated on the jumpserver with mkcert by `create.sh`, or replaced with BYO certs).
- The E2B SDK accesses sandboxes at URLs like `https://<sandbox-id>.cube.app`.
- With the default `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK=false`, the cube-proxy CLB is a **VPC-internal VIP** reachable only from inside the VPC (or via jumpserver / VPN).

### Getting CLB addresses

After deployment, from `terraform/tencentcloud/`:

```bash
cd terraform/tencentcloud
terraform output tke_cube_proxy_clb_ip    # cube-proxy
terraform output tke_cube_api_clb_ip      # cube-api (E2B_API_URL)
terraform output tke_cube_webui_clb_ip    # WebUI
terraform output tke_cubemaster_clb_ip    # cube-master (VPC-internal)
terraform output jumpserver_ssh_command   # jumpserver SSH command
```

`create.sh` also prints these entrypoints when it finishes.

### Option 1: Private DNS (recommended, VPC-internal access)

For clients inside the VPC or connected via jumpserver / VPN:

1. Log in to [DNSPod Private DNS](https://console.dnspod.cn/privateDNS) and create a private zone `cube.app`.
2. Add an **A record**: host `*`, value = the internal VIP from `terraform output tke_cube_proxy_clb_ip`; optionally add host `@` pointing to the same VIP.
3. **Associate the zone with the CubeSandbox VPC** (confirm the VPC ID in the [VPC console](https://console.cloud.tencent.com/vpc)).
4. Verify from inside the VPC (jumpserver or compute node):

```bash
dig +short test.cube.app
curl -k https://test.cube.app/   # self-signed cert: use -k or trust the mkcert CA
```

### Option 2: /etc/hosts (testing only)

Manually bind a few sandbox hostnames on the machine running the E2B SDK (must be able to route to the cube-proxy CLB internal VIP). Wildcard coverage is incomplete — **not suitable for concurrent multi-sandbox tests**.

### Option 3: Public CLB + public DNS

1. Set `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK=true` and redeploy (rebuilds CLBs; VIPs change).
2. Point `cube.app` / `*.cube.app` in public DNS to the public CLB VIP.
3. Replace certificates in `terraform/tencentcloud/cubeproxy-certs/` with a public CA-signed cert (see [Advanced: Bring Your Own cube-proxy TLS Certificate](#advanced-bring-your-own-cube-proxy-tls-certificate)).
4. Read [Hardening the Public-Facing Services](#hardening-the-public-facing-services) before enabling.

### E2B SDK environment variables

```bash
export E2B_API_URL=http://<cube-api-clb-vip>:3000
export E2B_API_KEY=e2b_000000
```

**Note:** sandbox traffic itself goes through `*.cube.app` → cube-proxy, so **cube.app DNS resolution and TLS certificates** are required — configuring `E2B_API_URL` alone is not enough. See also [Connecting to an Existing Cluster](./connect-existing-cluster.md).

| Capability | Single-machine one-click | Terraform cluster |
|------------|--------------------------|-------------------|
| `*.cube.app` resolution | Automatic (CoreDNS + split DNS) | **Manual** Private DNS / hosts / public DNS |
| TLS certificates | Host mkcert | Jumpserver mkcert → K8s Secret mount |
| cube-api address | `http://<host>:3000` | cube-api CLB internal / public VIP |

## Verifying the Deployment

After it finishes, `create.sh` runs health checks and prints the access entrypoints (CLB addresses for cube-api, cube-webui, etc.). You can also enter the VPC through the jumpserver to troubleshoot:

```bash
# create.sh prints a command like the following
ssh -i terraform/tencentcloud/.ssh/id_rsa -p 443 -o StrictHostKeyChecking=no root@<jumpserver_public_ip>
```

## Tearing Everything Down

```bash
./terraform/tencentcloud/destroy.sh
```

`destroy.sh` also needs `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY` and reuses the selections `create.sh` saved to `terraform/tencentcloud/.env`. It runs without extra prompting — running `destroy.sh` itself confirms the teardown.

::: danger Avoid unexpected billing
If `destroy.sh` cannot remove every resource (for example MySQL/Redis stuck in the recycle bin / isolated state, or leftovers Terraform can no longer see), log in to the Tencent Cloud console and delete the remaining resources by hand so you are not billed for orphans:

- [VPC / network](https://console.cloud.tencent.com/vpc)
- [MySQL recycle bin](https://console.cloud.tencent.com/cdb/recycle)
- [Redis recycle bin](https://console.cloud.tencent.com/redis/recycle)
- [CFS file systems](https://console.cloud.tencent.com/cfs) (if `USE_CFS=true` was enabled)

`destroy.sh` also prints these same links when a teardown step fails or a recycle-bin cleanup is not confirmed.
:::

## Advanced: Bring Your Own cube-proxy TLS Certificate

`cube-proxy` terminates TLS for `cube.app` / `*.cube.app`, and its bundled nginx config hard-codes the certificate paths `…/certs/cube.app+3.pem` and `…/certs/cube.app+3-key.pem`:

- **By default**, `create.sh` generates a **self-signed** pair on the jumpserver with the bundled `mkcert` (SANs: `cube.app`, `*.cube.app`, `localhost`, `127.0.0.1`), downloads it to `terraform/tencentcloud/cubeproxy-certs/`, and Terraform packs every file in that directory into the `cubeproxy-certs` Secret (a Secret, not a ConfigMap, because it holds the TLS private key), mounted read-only into the cube-proxy pod at `/usr/local/openresty/nginx/certs/`.
- **Bring your own certificate:** before running `create.sh`, drop your PEM cert + key into `terraform/tencentcloud/cubeproxy-certs/`, named exactly `cube.app+3.pem` and `cube.app+3-key.pem` (the names nginx expects) and covering the `cube.app` and `*.cube.app` SANs. `create.sh` reuses existing files instead of generating new ones, so a CA-signed certificate is used as-is, with no self-signed warning.
- **Rotate a certificate:** replace the two files and re-run `create.sh`; the deploy stage refreshes the `cubeproxy-certs` Secret and restarts cube-proxy to pick up the new material. The self-signed default trips browsers/clients with an "untrusted CA" warning, so replace it for any non-throwaway use.

## Troubleshooting

For common deployment issues (Docker, KVM, DNS, quotas, etc.), see [Troubleshooting — Deployment](./troubleshooting/deployment.md).
