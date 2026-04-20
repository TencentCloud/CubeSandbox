# Cube Sandbox Dev Environment

[中文文档](README_zh.md)

A disposable `OpenCloudOS 9` VM for developing and trying `Cube Sandbox`
without touching the host.

The VM uses the official OpenCloudOS cloud image from Tencent's mirror,
grown to 100G virtual disk, with SSH and the Cube API port forwarded to
the host:

```text
SSH      : 127.0.0.1:2222 -> guest:22
Cube API : 127.0.0.1:3000 -> guest:3000
```

## Prerequisites

- Linux x86_64 host with KVM enabled (`/dev/kvm` exists)
- Nested virtualization enabled on the host (required for Cube Sandbox
  to run MicroVMs inside the guest)
- `qemu-system-x86_64`, `qemu-img`, `curl`, `ssh`, `scp`, `setsid`

Quick sanity check:

```bash
ls -l /dev/kvm
cat /sys/module/kvm_intel/parameters/nested   # or kvm_amd
```

## Start the Dev Environment

Only three steps. Run the first two in one terminal, and the third in a
new terminal.

### 1. Prepare the image (one-off)

```bash
./prepare_image.sh
```

Downloads the OpenCloudOS 9 qcow2, resizes it to 100G, and runs the
guest-side setup (root filesystem growth, SELinux, PATH, login banner,
and so on), then shuts the VM down.

You only need to run this when setting things up for the first time or
after deleting `.workdir/`.

### 2. Boot the VM

```bash
./run_vm.sh
```

Keeps the QEMU serial console attached to this terminal. Detach with
`Ctrl+a` then `x` to power off the VM.

### 3. Log in (in a new terminal)

```bash
./login.sh
```

This SSHes in as `opencloudos` and drops you straight into a root shell
via `sudo -i`. Password handling is automated; you don't have to type
`opencloudos` each time.

To stay as the regular user instead of switching to root:

```bash
LOGIN_AS_ROOT=0 ./login.sh
```

## Files

```text
dev-env/
├── README.md
├── README_zh.md
├── prepare_image.sh   # one-off: download + resize + grow root fs + relax SELinux + install banner
├── run_vm.sh          # day-to-day: boot the VM
├── login.sh           # day-to-day: SSH in and switch to root
├── internal/          # helper scripts invoked inside the guest by prepare_image.sh
│   ├── grow_rootfs.sh     # grow the guest root filesystem to match the qcow2 virtual size
│   ├── setup_selinux.sh   # flip guest SELinux to permissive (docker bind-mount compatibility)
│   ├── setup_path.sh      # add /usr/local/{sbin,bin} to login PATH and sudo secure_path
│   └── setup_banner.sh    # install the login banner under /etc/profile.d/
└── .gitignore         # ignores .workdir/
```

Generated artifacts (image, pid file, serial log) live in `.workdir/`.

## Install Cube Sandbox Inside the VM

After logging in, run the official one-click installer:

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

Confirm KVM is usable from inside the guest (required for sandboxes to
actually boot):

```bash
ls -l /dev/kvm
egrep -c '(vmx|svm)' /proc/cpuinfo
```

Because host `:3000` is forwarded to guest `:3000`, you can point the
SDK at the dev environment from your host:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="dummy"
export CUBE_TEMPLATE_ID="<template-id>"
```

## Common Overrides

All three scripts accept environment variables:

```bash
# Only download + resize the qcow2 this run, skip the auto-grow workflow.
AUTO_BOOT=0 ./prepare_image.sh

# Boot with more resources or a different forwarded port.
VM_MEMORY_MB=16384 VM_CPUS=8 CUBE_API_PORT=13000 ./run_vm.sh

# Boot without requiring nested KVM (OS boot only, sandboxes will not run).
REQUIRE_NESTED_KVM=0 ./run_vm.sh

# Log in as the regular user instead of root.
LOGIN_AS_ROOT=0 ./login.sh
```

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|--------------|-----|
| No `/dev/kvm` inside the guest | Nested KVM disabled on the host | Enable nested virtualization on the host and reboot the VM |
| `./login.sh` fails to connect | VM not booted yet, or host port 2222 is busy | Check that `./run_vm.sh` is still running, or change `SSH_PORT` |
| `df -h /` inside the guest is still small | `prepare_image.sh` never finished the auto-grow step | Inspect `.workdir/qemu-serial.log`, then `scp internal/grow_rootfs.sh` into the guest and run it manually |
| Host port 3000 already taken | Some other service binds `3000` | Start with `CUBE_API_PORT=13000 ./run_vm.sh` |

## Note

This directory is a **development environment** for trying and developing
Cube Sandbox. It is not a production deployment method.
