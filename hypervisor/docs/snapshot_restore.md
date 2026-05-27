# Snapshot and Restore

The goal for the snapshot/restore feature is to provide the user with the
ability to take a snapshot of a previously paused virtual machine. This
snapshot can be used as the base for creating new identical virtual machines,
without the need to boot them from scratch. The restore codepath takes the
snapshot and creates the exact same virtual machine, restoring the previously
saved states. The new virtual machine is restored in a paused state, as it was
before the snapshot was performed.

## Snapshot a Cloud Hypervisor VM

First thing, we must run a Cloud Hypervisor VM:

```bash
./cloud-hypervisor \
    --api-socket /tmp/cloud-hypervisor.sock \
    --cpus boot=4 \
    --memory size=4G \
    --kernel vmlinux \
    --cmdline "root=/dev/vda1 console=hvc0 rw" \
    --disk path=focal-server-cloudimg-amd64.raw
```

At any point in time when the VM is running, one might choose to pause it:

```bash
./ch-remote --api-socket=/tmp/cloud-hypervisor.sock pause
```

Once paused, the VM can be safely snapshot into the specified directory and
using the following command:

```bash
./ch-remote --api-socket=/tmp/cloud-hypervisor.sock snapshot file:///home/foo/snapshot
```

Given the directory was present on the system, the snapshot will succeed and
it should contain the following files:

```bash
ll /home/foo/snapshot/
total 4194536
drwxrwxr-x  2 foo bar       4096 Jul 22 11:50 ./
drwxr-xr-x 47 foo bar       4096 Jul 22 11:47 ../
-rw-------  1 foo bar       1084 Jul 22 11:19 config.json
-rw-------  1 foo bar 4294967296 Jul 22 11:19 memory-ranges
-rw-------  1 foo bar     217853 Jul 22 11:19 state.json
```

`config.json` contains the virtual machine configuration. It is used to create
a similar virtual machine with the correct amount of CPUs, RAM, and other
expected devices. It is stored in a human readable format so that it could be
modified between the snapshot and restore phases to achieve some very special
use cases. But for most cases, manually modifying the configuration should not
be needed.

`memory-ranges` stores the content of the guest RAM.

`state.json` contains the virtual machine state. It is used to restore each
component in the state it was left before the snapshot occurred.

## Restore a Cloud Hypervisor VM

Given that one has access to an existing snapshot in `/home/foo/snapshot`,
it is possible to create a new VM based on this snapshot with the following 
command:

```bash
./cloud-hypervisor \
    --api-socket /tmp/cloud-hypervisor.sock \
    --restore source_url=file:///home/foo/snapshot
```

Or using two different commands from two terminals:

```bash
# First terminal
./cloud-hypervisor --api-socket /tmp/cloud-hypervisor.sock

# Second terminal
./ch-remote --api-socket=/tmp/cloud-hypervisor.sock restore source_url=file:///home/foo/snapshot
```

Remember the VM is restored in a `paused` state, which was the VM's state when
it was snapshot. For this reason, one must explicitly `resume` the VM before to
start using it.

```bash
./ch-remote --api-socket=/tmp/cloud-hypervisor.sock resume
```

At this point, the VM is fully restored and is identical to the VM which was
snapshot earlier.

## Limitations

VFIO devices and Intel SGX are out of scope.

## Snapshot types

The `--snapshot-type` flag of the `snapshot` and `pause2snapshot` ch-remote
subcommands selects how guest RAM is captured. The other snapshot artifacts
(`config.json`, `state.json`) are unaffected.

| Type | What gets saved to `memory-ranges` | When to use |
|------|------------------------------------|-------------|
| `full` | Every byte of guest RAM. | First snapshot after VM start, or any time you need a self-contained image. |
| `incremental` | Only **CoW anonymous** pages (pages the guest has written to since the original `MAP_PRIVATE` mmap was set up). Detected via `/proc/self/pagemap` + `/proc/kpageflags`. | Cheap "second" snapshot when a base file already exists. The set of saved pages monotonically grows over the VM's lifetime — every page ever touched is in the delta. |
| `soft-dirty` | Only the pages written **since the previous `soft-dirty` snapshot** (a true delta). Detected via `/proc/self/clear_refs` + pagemap bit 55. | Repeated incremental snapshots on a long-running VM, where most pages are not touched between cycles. |

### Soft-dirty details

- The first `soft-dirty` snapshot taken after VM start writes a full base
  image and arms the kernel tracker.
- Each subsequent `soft-dirty` snapshot writes only the pages whose PTE has
  been marked soft-dirty since the previous call, then re-arms the tracker.
- Requires the host kernel to be built with `CONFIG_MEM_SOFT_DIRTY=y`. On
  kernels without this support, `soft-dirty` requests are silently downgraded
  to the `incremental` (pagemap_anon) path and a debug log is emitted.
- A runtime failure of the `clear_refs` write (e.g. seccomp blocking it)
  also degrades subsequent snapshots to `incremental` for the rest of the
  VM's lifetime, with a warning log.

