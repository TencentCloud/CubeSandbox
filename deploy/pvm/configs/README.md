# PVM Kernel Configs

This directory stores the vetted kernel `.config` files consumed by the PVM
build scripts:

- `pvm_host` is used by `deploy/pvm/build-pvm-host-kernel-pkg.sh` to build the
  host kernel package that enables `KVM_PVM`.
- `pvm_guest` is used by `deploy/pvm/build-pvm-guest-vmlinux.sh` to build the
  guest `vmlinux` consumed by CubeSandbox.

The configs are kept in-tree so the default build path is reproducible and
works in offline or air-gapped environments. They are derived from the PVM
kernel tree used by these scripts (`https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git`,
tag `6.6.69-1.cubesandbox`) and the upstream reference configs published by
the virt-pvm project:

- `https://raw.githubusercontent.com/virt-pvm/misc/refs/heads/main/pvm-host-6.12.33.config`
- `https://raw.githubusercontent.com/virt-pvm/misc/refs/heads/main/pvm-guest-6.12.33.config`

The build scripts prefer these local files via `LOCAL_CONFIG_FILE`. If a user
forces the `CONFIG_URL` fallback, the downloaded file is verified against the
default `CONFIG_SHA256` before it is applied.

To refresh either config, update it against the target PVM kernel branch, boot
test it on the intended host or guest environment, replace the corresponding
file here, and update the default `CONFIG_SHA256` in the matching build script.
