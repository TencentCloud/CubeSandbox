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
| `incremental` | Anonymous CoW and swapped pages identified via `/proc/self/pagemap`. A verified XFS clone can also receive resident file-backed pages during Pause (see below). | Capture changes relative to an existing base image. Pages made private since the original restore can appear in each subsequent incremental snapshot. |
| `soft-dirty` | Only the pages written **since the previous `soft-dirty` snapshot** (a true delta). Detected via `/proc/self/clear_refs` + pagemap bit 55. | Repeated incremental snapshots on a long-running VM, where most pages are not touched between cycles. |

### Prefetching pages on incremental restore

Set `/etc/cube/populate-read` on the capture host to a positive integer to
record restore hints. On the restore host, the same setting controls the
maximum number of workers per VM. For example, `4` enables up to four.
Missing, invalid and zero values disable hints on that host.

For an incremental snapshot of a VM restored from a private memory image
with dirty logging disabled, the VMM can classify anonymous or swapped
pages and resident file-backed pages in one pagemap scan. It stores the
two range lists as optional write and read hints in `state.json`.
During `pause2snapshot`, the incremental writer can reuse the required
anonymous and swapped ranges from that same frozen guest scan. If the scan
fails or its guest mappings no longer match, the writer scans again for
the required pages. Exceeding the 65,536-range hint limit drops the optional
hints and retains the complete required write set.

Fast restore prepares private copies for write hints first, then faults in
read hints within the remaining budget. Together they can use up to
256 MiB and 128 workers per VM. This can increase private memory use and
move I/O into the restore call. Missing or unusable hints and unsupported
`MADV_POPULATE_READ` or `MADV_POPULATE_WRITE` leave the remaining pages to
demand paging. The existing whole-memory restore option `prefault=on`
takes precedence.

### Keeping resident pages warm across Pause on XFS

Set `/etc/cube/snapshot-resident-pages` to `1` on the capture host before
starting the VMM to enable resident-page writes during incremental
`pause2snapshot`. A missing file or any other value disables these writes.
This switch works independently of `/etc/cube/populate-read`. When both
switches are off, incremental Pause uses its regular pagemap scan.

Cubelet owns the destination clone exclusively until publication, and the
private restore source must stay immutable. The VMM checks the source
mappings, destination inode, clean page caches and shared XFS data extents
across the full file before writing. This check requires Linux 6.5 or later
for `cachestat`. A target that aliases the live regular-file restore source,
including a hard link, fails before writing. Unverifiable clones, non-XFS
files, DAX and realtime inodes use the regular incremental write path.

On a verified clone, the writer combines required pages and up to 256 MiB
in 8,192 optional resident ranges, including resident zero pages backed by
sparse holes. It writes in destination offset order, merging adjacent
ranges within each guest mapping. All bytes come from the frozen guest.
These buffered writes warm the destination inode's page cache. Writing a
shared extent can break reflink sharing, and writing a hole can allocate
blocks. The extra blocks still occupy disk space after writeback until
they are freed or shared again.

A write error aborts Pause before publication. Success means the filesystem
accepted the bytes into its cache. Cubelet's XFS Pause path publishes the
snapshot after syncing the VM configuration and state metadata. The
incremental memory writer does not sync the memory image, so a writeback
error or power loss can occur after Pause returns.

On successful completion, the VMM's `SNAPSHOT_RESIDENT` log reports
`written` and `ranges` for the extra resident bytes and ranges only.
`us` measures planning and writing both required and resident ranges,
and `verify_us` covers the preceding clone checks. `combined_writes`
counts the logical output ranges after merging. A single range may take
multiple writes to complete. If verification fails, the log includes
`written=0`, `verify_us` and the reason.

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

