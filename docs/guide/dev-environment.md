# Development Environment (QEMU VM)

If you only have a laptop or a cloud VM — not a dedicated bare-metal
server — but you still want to try Cube Sandbox or hack on it, you can
run Cube Sandbox **inside a disposable OpenCloudOS 9 virtual machine**
on your host.

The `dev-env/` directory at the repository root scripts the whole
flow: one-off image preparation, VM boot, and auto-login.

Three commands and you have a working Cube Sandbox playground.

## Prerequisites

The scripts in this guide must run on one of the following hosts:

- **WSL 2 on Windows** (requires Windows 11 22H2+ with WSL nested
  virtualization enabled)
- **A Linux physical machine**
- **A Linux VM with nested virtualization enabled** (cloud VM or local
  VM both work)

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

### Step 1: Prepare the image (one-off)

```bash
./prepare_image.sh
```

This downloads the official OpenCloudOS 9 qcow2 from the Tencent
mirror and runs initialisation.

You only need to run this once per fresh image — or again after
deleting the generated `.workdir/` directory.

### Step 2: Boot the VM

```bash
./run_vm.sh
```

The QEMU serial console is attached to the current terminal. Press
`Ctrl+a` then `x` to power the VM off.

### Step 3: Log in (in a new terminal)

```bash
./login.sh
```

`login.sh` logs you into the VM as root — Cube Sandbox's installer
requires root.

### Install Cube Sandbox inside the VM

Once you are in the root shell, run the standard one-click installer:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

::: tip Use the Tencent Cloud mirror from China
```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```
:::

When the installer finishes, follow the regular
[Quick Start](./quickstart) to create a template and run your first
sandbox in the VM.

---

Everything below is supplementary material and troubleshooting — you
can skip it if the happy path above worked.

## When to use this

- You want a clean OpenCloudOS 9 environment to try Cube Sandbox.
- You only have a laptop or cloud VM with KVM and nested virtualization,
  not a physical server.
- You want to iterate on Cube Sandbox without polluting your host.

::: warning Not a production deployment method
This is explicitly a **development / evaluation** environment. For
production, use [Quick Start](./quickstart) or
[Multi-Node Cluster](./multi-node-deploy) on bare metal.
:::

## Host self-check

Software dependencies required on the host:

- Linux x86_64 with KVM enabled (`/dev/kvm` exists)
- Nested virtualization enabled
- `qemu-system-x86_64`, `qemu-img`, `curl`, `ssh`, `scp`, `setsid`

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

`run_vm.sh` configures these forwards automatically:

| Host | Guest | Purpose |
|------|-------|---------|
| `127.0.0.1:10022` | `:22` | SSH into the dev VM |
| `127.0.0.1:13000` | `:3000` | Cube Sandbox E2B-compatible API |

## What prepare_image.sh does inside the guest

- Grow the root partition and filesystem to use the full 100 GB disk.
- Flip SELinux to `permissive` (both at runtime and in
  `/etc/selinux/config`). Cube Sandbox's MySQL container bind-mounts
  `/docker-entrypoint-initdb.d` from the host; with enforcing SELinux
  and `container-selinux` policies in place, the container process gets
  denied and MySQL keeps restarting.
- Ensure `/usr/local/{sbin,bin}` are on the login `PATH` **and** on
  sudo's `secure_path`, so `cubemastercli` and friends work both from a
  login shell and from `sudo`.
- Install a welcome banner at `/etc/profile.d/cubesandbox-banner.sh`,
  so every login shell greets you with
  `Welcome to the Cube Sandbox development environment!`.

## Common overrides

All three scripts accept environment variables:

```bash
# Download + resize only, skip the in-guest auto-grow workflow.
AUTO_BOOT=0 ./prepare_image.sh

# Boot with more resources, or change the forwarded Cube API port.
VM_MEMORY_MB=16384 VM_CPUS=8 CUBE_API_PORT=23000 ./run_vm.sh

# Boot without requiring nested KVM (OS will boot but sandboxes won't run).
REQUIRE_NESTED_KVM=0 ./run_vm.sh

# Log in as the regular user instead of root.
LOGIN_AS_ROOT=0 ./login.sh
```

Defaults for `run_vm.sh`: 4 CPUs, 8192 MB RAM, SSH forwarded on
`127.0.0.1:10022`, Cube API forwarded on `127.0.0.1:13000` (guest `:3000`).

## Reset / clean up

- To reset the VM state, stop any running `run_vm.sh`, delete
  `dev-env/.workdir/`, then run `./prepare_image.sh` again.
- The dev VM is disposable by design. You are expected to rebuild it
  whenever the installed state becomes unusable.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| No `/dev/kvm` inside the guest | Host does not have nested KVM enabled | Enable nested virtualization on the host and reboot the VM |
| `./login.sh` cannot connect | VM not booted yet, or host port `10022` is busy | Confirm `./run_vm.sh` is still running; or change `SSH_PORT` |
| `cube-sandbox-mysql` keeps restarting with `Permission denied` | Guest SELinux is still enforcing | Re-run `./prepare_image.sh`, or inside the guest: `setenforce 0 && sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config && docker restart cube-sandbox-mysql` |
| `df -h /` inside the guest is still small | The auto-grow step did not complete | Inspect `.workdir/qemu-serial.log`, then `scp internal/grow_rootfs.sh` into the guest and run it manually |
| Host port `13000` is already taken | Another service is bound to `13000` | Use `CUBE_API_PORT=23000 ./run_vm.sh` and update `E2B_API_URL` accordingly |

## Directory layout

```text
dev-env/
├── prepare_image.sh   # one-off: download + resize + guest-side init
├── run_vm.sh          # day-to-day: boot the VM
├── login.sh           # day-to-day: SSH in and switch to root
├── internal/          # helper scripts invoked inside the guest
│   ├── grow_rootfs.sh
│   ├── setup_selinux.sh
│   ├── setup_path.sh
│   └── setup_banner.sh
├── README.md
└── README_zh.md
```

For a short overview, see the
[`dev-env/README.md`](https://github.com/tencentcloud/CubeSandbox/tree/master/dev-env)
in the repository.
