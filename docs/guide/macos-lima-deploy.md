# macOS (Apple Silicon) with Lima

> **When to use this guide:** you want a single-node Cube Sandbox on an Apple Silicon Mac for local development or testing. For servers, see [Quick Start](./quickstart.md).

Cube Sandbox needs Linux with KVM. On an M3 or newer Mac, Apple's Virtualization.framework can give a Linux VM nested virtualization, so the VM has `/dev/kvm` and runs the standard arm64 one-click install unchanged. Sandboxes are real KVM microVMs restored from snapshots, just as on a server. The one difference is the extra virtualization layer.

```
macOS ──vz (nested virt)──▶ Ubuntu 24.04 arm64 VM ──KVM──▶ Cube microVMs
  :13000 ─────────────────▶ CubeAPI :3000
  :18080 ─────────────────▶ CubeProxy :80   (Host: <port>-<sandboxID>.cube.app)
```

The scripts are in [`deploy/macos-lima/`](https://github.com/TencentCloud/CubeSandbox/tree/master/deploy/macos-lima).

## Prerequisites

- **Apple Silicon M3 or newer, macOS 15 or newer.** M1 and M2 have no nested virtualization, so the VM never gets `/dev/kvm`.
- **Lima 2.x** (`brew install lima`). Lima 1.0.x builds made with an SDK older than macOS 15 refuse `nestedVirtualization`, even on capable hardware.
- About 16 GiB of RAM and 120 GiB of (sparse) disk for the VM. The installer requires at least 7.5 GB of guest memory.

## Bring it up

From a checkout of this repository, on the Mac:

```bash
deploy/macos-lima/up.sh
```

This runs the following steps:

1. Creates the VM from `lima.yaml`: vz with `nestedVirtualization`, 8 vCPU and 16 GiB, with CubeAPI and CubeProxy forwarded to host ports 13000 and 18080.
2. Prepares the guest with `vm-setup.sh`: Docker, an XFS (reflink) loop file at `/data/cubelet`, and the builder settings.
3. Builds the arm64 one-click bundle from the committed `HEAD` with `build-bundle.sh`. It uses the same pinned kernel, guest image and builder image as the arm64 release job. The first build takes about 30 minutes; later builds take about 5.
4. Installs the bundle with `install.sh`, using the settings described in [Lima-specific settings](#lima-specific-settings).

To install a released arm64 bundle instead of building one:

```bash
deploy/macos-lima/up.sh --bundle https://github.com/TencentCloud/CubeSandbox/releases/download/<tag>/cube-sandbox-one-click-<version>-arm64.tar.gz
```

Running `up.sh` again upgrades the VM in place. `--reinstall` wipes Cube's state first. On a mainland China network, set `CUBE_LIMA_CN_MIRRORS=1` so in-VM builds fetch Go modules and crates from mirrors.

## Create a template and a sandbox

Inside the VM (`limactl shell cube`), create a template as in [Quick Start](./quickstart.md#step-3-create-a-template). Use a multi-arch (arm64) image, and probe envd (port 49983):

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G --expose-port 49999 --expose-port 49983 --probe 49983
cubemastercli tpl watch --job-id <job_id>
```

Why envd and not 49999: the template build boots the VM cold, and the readiness probe gets about 30 s. Under nested virtualization the code interpreter on port 49999 can take longer than that to start, and the build then fails with `connection refused` on `:49999/health`. envd is up as soon as the guest is.

From the Mac, any E2B-compatible SDK works against `E2B_API_URL=http://127.0.0.1:13000`. Sandbox ports are reachable through CubeProxy at `127.0.0.1:18080` with `Host: <port>-<sandboxID>.cube.app`.

`bench.sh` measures sequential create, readiness through CubeProxy, and delete:

```bash
limactl shell cube -- bash ~/cube/src/deploy/macos-lima/bench.sh <template_id> 10
```

## Lima-specific settings

`install.sh` applies these settings for you:

- `CUBE_SANDBOX_NETWORK_CIDR=10.88.0.0/20`: Lima's user network (`192.168.5.0/24`) falls inside the default `192.168.0.0/18`, and the installer refuses overlapping networks.
- `no_proxy` includes the node IP: Lima passes the host's `http_proxy` into the guest, and the installer's readiness checks must reach local endpoints directly.

## Measured on an M5 Pro

The host was an M5 Pro running macOS 26.6.2 with Lima 2.2.0. The guest had 8 vCPU and 16 GiB.

| Step | Time |
| --- | --- |
| Cold boot to agent ready (template builds only) | usually 9–12 s, up to 38 s seen |
| Template build from `sandbox-code:latest` (image cached) | about 55 s |
| Sandbox create (snapshot restore), probe on envd | 1.0–1.6 s |
| Delete | about 0.7 s |

## Limitations

- Guests are arm64, so template images must include `linux/arm64`.
- A cold boot under nested virtualization takes about 10 s or more. Snapshot restores are fast, so this only affects template builds.
- Development and testing only: one node, behind Lima's NAT.
