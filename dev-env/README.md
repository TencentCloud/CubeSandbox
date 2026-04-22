# Cube Sandbox Dev Environment

[中文文档](README_zh.md)

> A throwaway VM for hacking on Cube Sandbox without touching your host.

## What is this

A set of shell scripts that spin up a disposable `OpenCloudOS 9` VM on your
Linux host, with SSH and the Cube API port-forwarded back to localhost:

```text
SSH      : 127.0.0.1:10022 -> guest:22
Cube API : 127.0.0.1:13000 -> guest:3000
Cube HTTP: 127.0.0.1:11080 -> guest:80
Cube TLS : 127.0.0.1:11443 -> guest:443
```

Use this when you want to:

- Try Cube Sandbox end-to-end on a Linux laptop without polluting your host
- Iterate on the source code in this repo and see your changes running
  inside a real Cube Sandbox installation

**Do not use this as a production deployment**. For production, see
[`deploy/one-click/`](../deploy/one-click/).

## Prerequisites

- Linux x86_64 host with KVM enabled (`/dev/kvm` exists)
- Nested virtualization enabled on the host (Cube Sandbox runs MicroVMs
  inside the guest, so the guest needs `/dev/kvm` too)
- Host packages: `qemu-system-x86_64`, `qemu-img`, `curl`, `ssh`, `scp`,
  `setsid`, `python3`, `rg`

Quick sanity check:

```bash
ls -l /dev/kvm
cat /sys/module/kvm_intel/parameters/nested   # or kvm_amd, expect Y / 1
```

## Quickstart

Five steps. Run them in order.

### Step 1 &nbsp; Prepare the VM image &nbsp; *(one-off, ~10 min)*

```bash
./prepare_image.sh
```

Downloads the OpenCloudOS 9 cloud image, resizes it to 100G, and runs
the in-guest setup (grow rootfs, relax SELinux, fix PATH, install login
banner, install the autostart systemd unit). When it finishes the VM is
shut down.

You only need this on first setup or after deleting `.workdir/`.

### Step 2 &nbsp; Boot the VM &nbsp; *(terminal A)*

```bash
./run_vm.sh
```

QEMU's serial console stays attached to this terminal. Do not quit QEMU
with `Ctrl+a` then `x`; that is abrupt and can corrupt the guest. In
another terminal run `./login.sh`, then run `poweroff` inside the guest.
After the guest shuts down, `run_vm.sh` in this terminal usually exits on
its own.

### Step 3 &nbsp; Log in &nbsp; *(terminal B)*

```bash
./login.sh
```

You land in a root shell inside the guest. Password handling is
automated.

### Step 4 &nbsp; Install Cube Sandbox inside the VM &nbsp; *(once per fresh VM)*

Inside the guest shell from Step 3:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

When this finishes you should see the four core processes alive
(`network-agent`, `cubemaster`, `cube-api`, `cubelet`).

### Step 5 &nbsp; Verify &nbsp; *(inside the VM)*

```bash
curl -sf http://127.0.0.1:3000/health && echo OK
```

You should see `OK`. Cube Sandbox is now running.

## Make it survive a reboot &nbsp; *(one-off, strongly recommended)*

By default the cube components are launched as bare processes — they do
**not** come back after the VM reboots. To let `systemd` bring them up
on every boot, run **on the host** after Step 5:

```bash
./cube-autostart.sh            # default subcommand: enable
```

This asks for confirmation, then enables `cube-sandbox-oneclick.service`
inside the guest. From now on every boot will run `up-with-deps.sh`,
which brings MySQL/Redis, cube-proxy, coredns, network-agent,
cubemaster, cube-api and cubelet up together.

Other subcommands:

```bash
./cube-autostart.sh status     # show is-enabled / is-active
./cube-autostart.sh disable    # roll back
```

## Develop: edit code, push to VM, see results

This is the main reason `dev-env/` exists.

After editing code in this repo on your host, push your changes into the
running VM with one command:

```bash
./sync_to_vm.sh
```

What it does, in order:

1. Runs `make all` in the repo root
2. Backs up old binaries inside the guest as `*.bak` (only the most
   recent backup is kept)
3. Copies the new binaries to the matching paths under
   `/usr/local/services/cubetoolbox/`
4. Restarts `cube-sandbox-oneclick.service`
5. Runs `quickcheck.sh`; if it fails, **automatically restores the
   `.bak` files** and restarts again

Common shortcuts:

```bash
# Skip the local build, reuse what's already in _output/bin/
BUILD=0 ./sync_to_vm.sh

# Sync only specific components
COMPONENTS="cubemaster cubelet" ./sync_to_vm.sh

# Push arbitrary files (no service restart)
MODE=files FILES="./configs/foo.toml" REMOTE_DIR=/tmp ./sync_to_vm.sh

# Use the official manual-release flow instead of raw binaries
MODE=release ./sync_to_vm.sh
```

Prerequisite: Step 4 finished and (recommended) `./cube-autostart.sh`
has been run.

## Collect logs from the VM

```bash
./copy_logs.sh
```

Tarballs `/data/log` from inside the guest and drops it next to this
README as `data-log-<timestamp>.tar.gz`.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| No `/dev/kvm` inside the guest | Nested KVM disabled on the host | Enable nested virtualization on the host, then reboot the VM |
| `./login.sh` fails to connect | VM not booted yet, or host port 10022 is busy | Check that `./run_vm.sh` is still running, or set `SSH_PORT` |
| `df -h /` inside the guest is still small | `prepare_image.sh` never finished the auto-grow step | Inspect `.workdir/qemu-serial.log`, then `scp internal/grow_rootfs.sh` into the guest and run it manually |
| Host port 13000 / 11080 / 11443 already taken | Some other service binds the forwarded dev-env ports | Start with `CUBE_API_PORT=23000 CUBE_PROXY_HTTP_PORT=21080 CUBE_PROXY_HTTPS_PORT=21443 ./run_vm.sh` |
| Cube components gone after VM reboot | Autostart not enabled | Run `./cube-autostart.sh` once |
| `sync_to_vm.sh` rolled back | `quickcheck` failed with new binaries | Check `/data/log/` in the guest, fix the bug, then re-run `sync_to_vm.sh` |

## Reference

### File layout

```text
dev-env/
├── README.md / README_zh.md
├── prepare_image.sh        # Step 1
├── run_vm.sh               # Step 2
├── login.sh                # Step 3
├── cube-autostart.sh       # enable / disable / status the systemd autostart unit
├── sync_to_vm.sh           # Develop loop
├── copy_logs.sh            # Pull /data/log from the guest
└── internal/               # Run inside the guest by prepare_image.sh
    ├── grow_rootfs.sh         # grow rootfs to qcow2 virtual size
    ├── setup_selinux.sh       # SELinux -> permissive (docker bind mount)
    ├── setup_path.sh          # /usr/local/{sbin,bin} on PATH
    ├── setup_banner.sh        # /etc/profile.d/ login banner
    └── setup_autostart.sh     # install cube-sandbox-oneclick.service (NOT enabled)
```

Generated artifacts (qcow2, pid file, serial log) live in `.workdir/`.

### Environment variables

#### `prepare_image.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTO_BOOT` | `1` | Boot the VM and run guest-side setup. `0` skips it (download + resize only). |
| `SETUP_AUTOSTART` | `1` | Install the systemd autostart unit (still **not** enabled). `0` skips. |
| `IMAGE_URL` | OpenCloudOS 9 | Override the source qcow2 URL. |
| `TARGET_SIZE` | `100G` | Final qcow2 virtual size. |
| `SSH_PORT` | `10022` | Host port forwarded to guest 22. |

#### `run_vm.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `VM_MEMORY_MB` | `8192` | Guest RAM. |
| `VM_CPUS` | `4` | Guest vCPUs. |
| `SSH_PORT` | `10022` | Host -> guest SSH. |
| `CUBE_API_PORT` | `13000` | Host -> guest Cube API. |
| `CUBE_PROXY_HTTP_PORT` | `11080` | Host -> guest CubeProxy HTTP (`guest:80`). |
| `CUBE_PROXY_HTTPS_PORT` | `11443` | Host -> guest CubeProxy HTTPS (`guest:443`). |
| `REQUIRE_NESTED_KVM` | `1` | Refuse to boot if host nested KVM is off. `0` to bypass (sandboxes won't run). |

#### `login.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `LOGIN_AS_ROOT` | `1` | `0` keeps you as the regular user. |

#### `cube-autostart.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `ASSUME_YES` | `0` | `1` skips the interactive confirmation. |
| `STOP_NOW` | `1` | `disable` only: `0` disables on next boot but leaves running services up. |
| `UNIT_NAME` | `cube-sandbox-oneclick.service` | Override the unit name. |

Subcommands: `enable` (default), `disable`, `status`.

#### `sync_to_vm.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `MODE` | `binaries` | `binaries` / `release` / `files`. |
| `BUILD` | `1` | `0` skips `make all` / `make manual-release`. |
| `RESTART` | `1` | `0` skips the remote `systemctl restart`. |
| `COMPONENTS` | (all) | `binaries` mode: subset of binaries to push. |
| `FILES` | — | `files` mode: paths to scp. |
| `REMOTE_DIR` | `/tmp` | `files` mode: destination dir in the guest. |

#### `copy_logs.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `REMOTE_LOG_DIR` | `/data/log` | Directory to archive inside the guest. |
| `OUTPUT_DIR` | `dev-env/` | Where the tarball lands on the host. |

### Common SSH overrides (apply to all helper scripts)

```bash
VM_USER=opencloudos VM_PASSWORD=opencloudos SSH_HOST=127.0.0.1 SSH_PORT=10022
```

## Notes

This directory is a **development environment**. It is intentionally
single-node, password-authenticated, and disposable. Do not use it to
host real workloads.
