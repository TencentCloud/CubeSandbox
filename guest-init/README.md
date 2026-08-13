# cube-init

Lightweight Guest PID 1 for Cube Sandbox MicroVMs.

Source directory: `guest-init/` (crate/package name `cube-init`, binary `cube-init`).

## Role

Guest Image embeds `cube-init` as `/sbin/init`. On boot it:

1. Verifies it is PID 1
2. Mounts base filesystems (`/proc`, `/sys`, `/dev/pts`, `/run`)
3. Mounts cgroups: if kernel cmdline has `agent.unified_cgroup_hierarchy=true`
   (CubeShim default), mounts **cgroup2** at `/sys/fs/cgroup`; otherwise mounts
   cgroup v1 subsystems (same semantics as historical cube-agent-as-PID1)
4. Sets `wrapper_mode=on`
5. Mounts `/dev/pmem1` → `/run/support` (`ext4,ro,dax`)
6. `execvp("/run/support/cube-agent")`

`/dev/pmem1` is the independent `cube-agent.ext4` plane file injected by CubeShim.

### What cube-init does **not** do

Open-source `cube-init` does **not** perform SysCtrl snapshot handshake
(`snapshot-mode` → write `SYS_START` / poll `SYS_RESTORE` on PIO `0x680` or
MMIO `0x0903_0000`).

Historical open-source path used `cube-agent` itself as PID 1 and never did
that handshake either. Product templates are created via **APP snapshot**
(pause a running sandbox); cold boot and restore do not require guest-init
to signal `SysStart`. Agent still notifies `VsockServerReady` over SysCtrl
after it starts.

Closed-source `CubeRuntime` init keeps the handshake for its cold-boot OS
template (`do_snapshot`) path; that is out of scope for open-source guest-init.

## Build

```bash
# From repo root (Docker builder):
make cube-init
# or:
make guest-init

# cube-init + independent cube-agent.ext4:
make pmem-assets

# Or locally with musl:
cd guest-init && make
```

The binary is injected into the guest image by `deploy/one-click/build-guest-image.sh`.
Independent agent plane file: `make agent-ext4` → `_output/cube-agent/cube-agent.ext4`.

## License

Apache-2.0
