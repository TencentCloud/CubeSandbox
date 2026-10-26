# Cube Sandbox Dev Environment

[中文文档](README_zh.md)

> A throwaway OpenCloudOS 9 VM for hacking on Cube Sandbox without touching
> your host.

## What is this

Three small scripts that create and drive a disposable `OpenCloudOS 9` VM on
your Linux host, with SSH forwarded back to localhost:

```text
SSH : 127.0.0.1:10022 -> guest:22
```

The VM ships with exactly two guest-side tweaks: the root disk is grown to
100G and SELinux is set to `permissive`. Everything else — installing Cube
Sandbox, Docker, or any other tool — you do yourself inside the guest.

Use this when you want to:

- Try Cube Sandbox end-to-end on a Linux laptop without polluting your host
- Get a root shell on a clean OpenCloudOS 9 machine and install whatever
  you need in it

**If you already have a cloud server, you don't need dev-env**: install
Cube Sandbox on it directly with the PVM deployment from the
[Quick Start](../docs/guide/quickstart.md).

**Do not use this as a production deployment**. For production, see
[`deploy/one-click/`](../deploy/one-click/).

## Prerequisites

- Linux x86_64 or aarch64 (ARM64) host with KVM enabled (`/dev/kvm` exists)
- Nested virtualization enabled on the host (Cube Sandbox runs MicroVMs
  inside the guest, so the guest needs `/dev/kvm` too)
- Host packages: `qemu-system-x86_64` (or `qemu-system-aarch64` on ARM64),
  `qemu-img`, `curl`, `ssh`, `scp`, `setsid`, `python3`
  - On aarch64 the VM boots with QEMU's `virt` machine and UEFI firmware, so
    EDK2/AAVMF (`QEMU_EFI.fd`, e.g. the `qemu-efi-aarch64` package) is
    required as well. The scripts detect the host architecture; override with
    `TARGET_ARCH` if needed.

Quick sanity check:

```bash
ls -l /dev/kvm
cat /sys/module/kvm_intel/parameters/nested   # or kvm_amd, expect Y / 1
```

## Quickstart

### Step 1 &nbsp; Create the VM &nbsp; *(one-off, ~10 min)*

```bash
./create_vm.sh
```

Downloads the OpenCloudOS 9 cloud image, resizes it to 100G, boots it once to
grow the guest root filesystem and set SELinux to permissive, then shuts the
VM down cleanly.

You only need this on first setup or after deleting `.workdir/`.

### Step 2 &nbsp; Boot the VM &nbsp; *(terminal A)*

```bash
./run_vm.sh
```

QEMU's serial console stays attached to this terminal. Do not quit QEMU with
`Ctrl+a` then `x`; that is abrupt and can corrupt the guest. In another
terminal run `./login.sh`, then run `poweroff` inside the guest. After the
guest shuts down, `run_vm.sh` in this terminal usually exits on its own.

### Step 3 &nbsp; Log in &nbsp; *(terminal B)*

```bash
./login.sh
```

You land in a root shell inside the guest. Password handling is automated
(`opencloudos` / `opencloudos`).

### Step 4 &nbsp; Install what you need &nbsp; *(inside the guest)*

The guest is a plain OpenCloudOS 9 machine — use `dnf`, `curl`, `docker`, or
anything else. For example, the one-click Cube Sandbox installer:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

From China you can use the Tencent Cloud mirror instead:

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```

## Move files in and out of the VM

SSH is forwarded to `127.0.0.1:10022` and password authentication is on, so
plain `scp` / `rsync` / `ssh` work from the host. The guest password is
`opencloudos`.

**Host → guest**

```bash
# single file
scp -P 10022 ./local-file opencloudos@127.0.0.1:/tmp/

# directory (recursive)
scp -P 10022 -r ./local-dir opencloudos@127.0.0.1:/tmp/

# incremental sync of a big tree (only changed files)
rsync -avz -e 'ssh -p 10022' ./_output/bin/ opencloudos@127.0.0.1:/tmp/bin/
```

`scp` and `rsync` connect as the unprivileged `opencloudos` user, so they
cannot write to `/usr/local/bin` and friends directly. Stage in `/tmp` and
move with `sudo`:

```bash
scp -P 10022 ./cubelet opencloudos@127.0.0.1:/tmp/
ssh -p 10022 opencloudos@127.0.0.1 \
  'sudo install -m 0755 /tmp/cubelet /usr/local/bin/cubelet'
```

**Guest → host**

```bash
# single file into the current directory
scp -P 10022 opencloudos@127.0.0.1:/data/log/cubelet.log ./

# whole directory
scp -P 10022 -r opencloudos@127.0.0.1:/data/log ./guest-logs/

# or stream a command's output straight to a host file
ssh -p 10022 opencloudos@127.0.0.1 'sudo journalctl -u cubelet --no-pager' > cubelet-journal.log
```

**Reaching services inside the guest**

Only SSH is forwarded by default. To reach a port that a service inside the
guest is listening on, either boot with `EXTRA_FORWARDS` (see below) or open
an ad-hoc tunnel:

```bash
ssh -N -L 13000:127.0.0.1:3000 -p 10022 opencloudos@127.0.0.1
```

## Forward extra guest ports

`run_vm.sh` forwards only SSH unless you ask for more. `EXTRA_FORWARDS` takes
space separated `HOST_PORT:GUEST_PORT` pairs:

```bash
# Cube API on host 13000, CubeProxy HTTPS on host 11443
EXTRA_FORWARDS="13000:3000 11443:443" ./run_vm.sh
```

Every forward binds to `127.0.0.1` on the host. Invalid entries abort the boot
before QEMU starts.

## Reference

### File layout

```text
dev-env/
├── README.md / README_zh.md
├── create_vm.sh            # Step 1: download image, resize, first-boot init
├── run_vm.sh               # Step 2: boot the VM
├── login.sh                # Step 3: SSH in and switch to root
└── internal/               # Run inside the guest by create_vm.sh
    ├── grow_rootfs.sh         # grow rootfs to qcow2 virtual size
    └── setup_selinux.sh       # SELinux -> permissive (docker bind mount)
```

Generated artifacts (qcow2, pid file, serial log) live in `.workdir/`.

### Environment variables

#### `create_vm.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `IMAGE_URL` | OpenCloudOS 9.6 | Override the source qcow2 URL. |
| `IMAGE_PATH` | `.workdir/<image name>` | Full path to the VM disk image. |
| `TARGET_SIZE` | `100G` | Final qcow2 virtual size. |
| `SSH_PORT` | `10022` | Host port forwarded to guest 22. |
| `VM_USER` / `VM_PASSWORD` | `opencloudos` | Guest credentials used by the provisioners. |
| `FORCE_KILL_ON_EXIT` | `0` | On failure, kill a still-running QEMU instead of leaving it for inspection. |

#### `run_vm.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `VM_MEMORY_MB` | `8192` | Guest RAM. |
| `VM_CPUS` | `4` | Guest vCPUs. |
| `SSH_PORT` | `10022` | Host -> guest SSH. |
| `EXTRA_FORWARDS` | *(empty)* | Space separated `HOST_PORT:GUEST_PORT` pairs to forward in addition to SSH. |
| `REQUIRE_NESTED_KVM` | `1` | Refuse to boot if host nested KVM is off. `0` to bypass (sandboxes won't run). |
| `VM_BACKGROUND` | `0` | `1` starts QEMU with `-daemonize` instead of attaching the serial console. |

#### `login.sh`

| Variable | Default | Description |
|----------|---------|-------------|
| `LOGIN_AS_ROOT` | `1` | `0` keeps you as the regular user. |

### Common SSH overrides (apply to all scripts)

```bash
VM_USER=opencloudos VM_PASSWORD=opencloudos SSH_HOST=127.0.0.1 SSH_PORT=10022
```

## Reset / clean up

Stop any running `run_vm.sh`, delete `dev-env/.workdir/`, then run
`./create_vm.sh` again. The VM is disposable by design — rebuild it whenever
the installed state becomes unusable.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| No `/dev/kvm` inside the guest | Nested KVM disabled on the host | Enable nested virtualization on the host, then reboot the VM |
| `./login.sh` fails to connect | VM not booted yet, or host port 10022 is busy | Check that `./run_vm.sh` is still running, or set `SSH_PORT` |
| `df -h /` inside the guest is still small | The grow step did not complete | Inspect `.workdir/qemu-serial.log`, then `scp -P 10022 internal/grow_rootfs.sh opencloudos@127.0.0.1:/tmp/` and run it with `sudo` in the guest |
| `cube-sandbox-mysql` keeps restarting with `Permission denied` | Guest SELinux is still enforcing | In the guest: `sudo setenforce 0 && sudo sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config && sudo docker restart cube-sandbox-mysql` |
| `EXTRA_FORWARDS` port already in use | Another host service binds that port | Pick another host port, e.g. `EXTRA_FORWARDS="23000:3000"` |

## Notes

This directory is a **development environment**. It is intentionally
single-node, password-authenticated, and disposable. Do not use it to host
real workloads.
