# Development Environment (QEMU VM)

If all you have is a **local machine** (a Linux laptop, workstation or
WSL 2) and no dedicated cloud server, but you still want to try Cube
Sandbox or hack on it, you can run Cube Sandbox **inside a disposable
OpenCloudOS 9 virtual machine** on that machine.

The `dev-env/` directory at the repository root scripts the whole
flow: one-off VM creation, boot, and auto-login.

Three commands and you have a root shell on a clean OpenCloudOS 9 machine
that you can install anything into.

::: tip Should you use dev-env?
- **Already have a cloud server?** You don't need this. Use the PVM
  deployment in [Quick Start](./quickstart.md): it installs a full Cube
  Sandbox on an ordinary cloud server (no `/dev/kvm` required), which is
  both faster and closer to production. Details in
  [PVM Deployment](./pvm-deploy.md).
- **Only have a local machine** (with KVM and nested virtualization)?
  Carry on — that is exactly what `dev-env/` is for.
:::

## Prerequisites

The scripts in this guide must run on one of the following hosts:

- **WSL 2 on Windows** (requires Windows 11 22H2+ with WSL nested
  virtualization enabled)
- **A Linux physical machine**
- **A Linux VM running locally, with nested virtualization enabled**

If your development machine is itself a cloud server, use the PVM
deployment in [Quick Start](./quickstart.md) instead of nesting another
VM inside it.

In all three cases, the host must be able to use KVM — `/dev/kvm` must
exist and be read/writable.

Cube Sandbox runs another layer of KVM MicroVMs inside the guest, so
the host **must** support nested virtualization — otherwise the guest
will have no usable `/dev/kvm` and sandbox creation will fail.

For detailed software dependencies and how to verify / enable nested
KVM, see the "Host self-check" section further down.

## Quick start

Clone the repository and enter `dev-env/`:

```bash
git clone https://github.com/tencentcloud/CubeSandbox.git
cd CubeSandbox/dev-env
```

Three commands total. The first two run in one terminal, the third in
a **second terminal**.

### Step 1: Create the VM (one-off)

```bash
./create_vm.sh
```

This downloads the official OpenCloudOS 9 qcow2 from the Tencent
mirror, resizes the disk to 100 GB, boots the VM once to grow the guest
root filesystem and set SELinux to `permissive`, then shuts it down
cleanly.

You only need to run this once per fresh image — or again after
deleting the generated `.workdir/` directory.

### Step 2: Boot the VM

```bash
./run_vm.sh
```

The QEMU serial console is attached to the current terminal. Do not power
the VM off with `Ctrl+a` then `x`; that is abrupt and can corrupt the
guest. Instead, log in from another terminal with `./login.sh` and run
`poweroff` inside the guest.

### Step 3: Log in (in a new terminal)

```bash
./login.sh
```

`login.sh` logs you into the VM as root (password handling is automated),
because installing Cube Sandbox and most other tooling needs root.

### Step 4: Install what you need inside the VM

The guest is a plain OpenCloudOS 9 machine: `dnf`, `curl`, `docker` and
friends all work. To install Cube Sandbox, run the standard one-click
installer:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

::: tip Use the Tencent Cloud mirror from China
```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```
:::

When the installer finishes, follow the regular
[Quick Start](./quickstart.md) to create a template and run your first
sandbox in the VM.

## Move files between host and guest

SSH is forwarded to `127.0.0.1:10022` and password authentication is
enabled, so `scp`, `rsync` and `ssh` work out of the box. The guest
password is `opencloudos`.

**Host → guest**

```bash
# single file
scp -P 10022 ./local-file opencloudos@127.0.0.1:/tmp/

# directory (recursive)
scp -P 10022 -r ./local-dir opencloudos@127.0.0.1:/tmp/

# incremental sync of a large tree
rsync -avz -e 'ssh -p 10022' ./_output/bin/ opencloudos@127.0.0.1:/tmp/bin/
```

`scp` and `rsync` connect as the unprivileged `opencloudos` user, so
they cannot write to `/usr/local/bin` and similar paths directly. Stage
in `/tmp` and move with `sudo`:

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

# stream a command's output straight into a host file
ssh -p 10022 opencloudos@127.0.0.1 'sudo journalctl -u cubelet --no-pager' > cubelet-journal.log
```

**Reaching services inside the guest**

Only SSH is forwarded by default. Either boot with `EXTRA_FORWARDS`
(next section) or open an ad-hoc tunnel:

```bash
ssh -N -L 13000:127.0.0.1:3000 -p 10022 opencloudos@127.0.0.1
```

## Forward extra guest ports

`run_vm.sh` forwards only SSH unless you ask for more. `EXTRA_FORWARDS`
takes space separated `HOST_PORT:GUEST_PORT` pairs and binds them to
`127.0.0.1` on the host:

```bash
# Cube API on host 13000, CubeProxy HTTPS on host 11443
EXTRA_FORWARDS="13000:3000 11443:443" ./run_vm.sh
```

Invalid entries abort the boot before QEMU starts.

## cubecow storage in dev-env

Cubelet uses the reflink-only `cubecow` storage backend by default. The dev
VM only needs a reflink-capable filesystem (e.g. XFS with `-m reflink=1`,
or Btrfs) at the path used by `data_path`; no LVM / dm-thin tooling or extra
raw disk is required. The default settings under
`[plugins."io.cubelet.internal.v1.storage".cow.*]` create reflink volumes
under `<data_path>/../cubecow-reflink`.

## What create_vm.sh does inside the guest

Only two things, both applied on the one-off first boot:

- Grow the root partition and filesystem to use the full 100 GB disk.
- Flip SELinux to `permissive` (both at runtime and in
  `/etc/selinux/config`). Cube Sandbox's MySQL container bind-mounts
  `/docker-entrypoint-initdb.d` from the host; with enforcing SELinux
  and `container-selinux` policies in place, the container process gets
  denied and MySQL keeps restarting.

Nothing else is automated: there is no login banner, no `PATH` or
`secure_path` tweak, no systemd autostart unit, and no binary sync or
log-collection helper. Install and configure whatever you need inside
the guest yourself, and move files with `scp` / `rsync`.

---

Everything below is supplementary material and troubleshooting — you
can skip it if the happy path above worked.

## When to use this

- You want a clean OpenCloudOS 9 environment to try Cube Sandbox.
- You only have a local machine with KVM and nested virtualization, and
  no cloud server to deploy on.
- You want to iterate on Cube Sandbox without polluting your host.

::: warning Not a production deployment method
This is explicitly a **development / evaluation** environment. For
production, use [Quick Start](./quickstart.md) or
[Multi-Node Cluster](./multi-node-deploy.md) on bare metal.
:::

## Host self-check

Software dependencies required on the host:

- Linux x86_64 or aarch64 (ARM64) with KVM enabled (`/dev/kvm` exists)
- Nested virtualization enabled
- `qemu-system-x86_64` (or `qemu-system-aarch64` on ARM64), `qemu-img`, `curl`, `ssh`, `scp`, `setsid`, `python3`
  - On aarch64 the dev-env VM boots with QEMU's `virt` machine and UEFI firmware, so the EDK2/AAVMF firmware (`QEMU_EFI.fd`, e.g. the `qemu-efi-aarch64` package) must also be installed. The scripts auto-detect the host architecture; override with `TARGET_ARCH` if needed.

Quick sanity check:

```bash
ls -l /dev/kvm

# Intel
cat /sys/module/kvm_intel/parameters/nested
# AMD
cat /sys/module/kvm_amd/parameters/nested
```

If the `nested` parameter reads `N` or `0`, enable it on the host
before continuing. Example for Intel:

```bash
echo 'options kvm_intel nested=1' | sudo tee /etc/modprobe.d/kvm.conf
sudo modprobe -r kvm_intel && sudo modprobe kvm_intel
```

## Host ↔ guest port map

`run_vm.sh` always forwards SSH, plus anything you list in
`EXTRA_FORWARDS`:

| Host | Guest | Purpose |
|------|-------|---------|
| `127.0.0.1:10022` | `:22` | SSH into the dev VM |
| *(from `EXTRA_FORWARDS`)* | *(your choice)* | Anything else you want to reach, e.g. `13000:3000` for the Cube Sandbox E2B-compatible API |

## Common overrides

The three scripts accept environment variables:

```bash
# Boot with more resources (still SSH-only forwarding).
VM_MEMORY_MB=16384 VM_CPUS=8 ./run_vm.sh

# Also forward the Cube API and CubeProxy HTTPS to the host.
EXTRA_FORWARDS="13000:3000 11443:443" ./run_vm.sh

# Boot without requiring nested KVM (OS will boot but sandboxes won't run).
REQUIRE_NESTED_KVM=0 ./run_vm.sh

# Log in as the regular user instead of root.
LOGIN_AS_ROOT=0 ./login.sh

# Create a bigger disk image (default 100G).
TARGET_SIZE=200G ./create_vm.sh
```

Defaults for `run_vm.sh`: 4 CPUs, 8192 MB RAM, SSH forwarded on
`127.0.0.1:10022`, no other forwards.

## Reset / clean up

- To reset the VM state, stop any running `run_vm.sh`, delete
  `dev-env/.workdir/`, then run `./create_vm.sh` again.
- The dev VM is disposable by design. You are expected to rebuild it
  whenever the installed state becomes unusable.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| No `/dev/kvm` inside the guest | Host does not have nested KVM enabled | Enable nested virtualization on the host and reboot the VM |
| `./login.sh` cannot connect | VM not booted yet, or host port `10022` is busy | Confirm `./run_vm.sh` is still running; or change `SSH_PORT` |
| `cube-sandbox-mysql` keeps restarting with `Permission denied` | Guest SELinux is still enforcing | Inside the guest: `sudo setenforce 0 && sudo sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config && sudo docker restart cube-sandbox-mysql` |
| `df -h /` inside the guest is still small | The auto-grow step did not complete | Inspect `.workdir/qemu-serial.log`, then `scp -P 10022 internal/grow_rootfs.sh opencloudos@127.0.0.1:/tmp/` and run it with `sudo` in the guest |
| `EXTRA_FORWARDS` port is already taken | Another service is bound to that host port | Pick a different host port, e.g. `EXTRA_FORWARDS="23000:3000"` |
| Booting with an invalid `EXTRA_FORWARDS` aborts | Entry is not `HOST_PORT:GUEST_PORT` | Use space separated numeric pairs, e.g. `EXTRA_FORWARDS="13000:3000"` |

## Directory layout

```text
dev-env/
├── create_vm.sh       # one-off: download + resize + first-boot guest init
├── run_vm.sh          # day-to-day: boot the VM
├── login.sh           # day-to-day: SSH in and switch to root
├── internal/          # helper scripts invoked inside the guest
│   ├── grow_rootfs.sh
│   └── setup_selinux.sh
├── README.md
└── README_zh.md
```

For a short overview, see the
[`dev-env/README.md`](https://github.com/tencentcloud/CubeSandbox/tree/master/dev-env)
in the repository.
