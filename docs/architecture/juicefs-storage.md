---
aside: false
---

# Cube + JuiceFS Cross-Node Storage Architecture

This page embeds the JuiceFS topology and core flows from `jfs-data.html` as native documentation diagrams. See [JuiceFS service configuration](../guide/cross-node-snapshot/juicefs.md) for deployment and [cross-node restore requirements](../guide/cross-node-snapshot/requirements.md) for placement gates.

## Goals

The default `xfs` backend provides fast local reflink clones, but its templates, pause packages, and snapshots remain on their origin node. The JuiceFS backend preserves Cube's file-based CoW model while adding:

- one shared POSIX filesystem for templates, rootfs files, memory images, and snapshots;
- `copy_file_range` clones that copy JuiceFS slice references only;
- durable blocks in object storage and metadata/reference counts in Redis;
- node-local read cache and writeback staging;
- a `remote_status=ready` gate before a package can restore on another node.

## Architecture

<JuiceFSArchitecture mode="architecture" />

Cubelet, the VMM, and a JuiceFS client run on each compute node. All nodes mount the same JuiceFS volume, share directories, inodes, slices, and reference counts through Redis, and persist blocks in MinIO, S3, or COS. Each node's `raw` read cache and `rawstaging` write cache remain local and do not migrate with a sandbox.

CubeMaster manages template jobs, placement, and remote readiness. TemplateCenter pulls OCI images and creates ext4 artifacts. These control-plane components do not proxy rootfs or memory data.

## Design flows

<JuiceFSArchitecture mode="flows" />

The flow view covers template creation, sandbox startup, and pause and restore. Select a step to see its active components, files, and data path. Sandbox startup and pause and restore can also switch between local and cross-node scenarios.

Templates and derived files can reference the same object blocks. `copy_file_range` primarily copies slice references, and only modified regions create new slices and blocks. Pause packages contain complete rootfs, memory, and metadata files without Cube-managed `.delta` files.

Before a cross-node restore, CubeMaster waits until the package's staging blocks are uploaded and `remote_status=ready`. The target then clones the shared files, maps memory with `MAP_PRIVATE`, and fetches object blocks on demand when its local cache misses.

## Dirty-memory write granularity

JuiceFS organizes files into 64 MiB chunks. A continuous write creates a slice, and slices are split into object blocks no larger than 4 MiB. Writing every 4 KiB dirty page separately creates thousands of slices and Redis transactions and can trigger compaction. `jfs-data.html` measured about 38 MiB of dirty pages:

| Method | Pause | Bytes written | New slices | Redis transactions | Compaction rewrite |
|---|---:|---:|---:|---:|---:|
| 4 KiB page writes | 15.0 s | 20.6 MB | 2588 | 2739 | 1416 MB |
| 4 MiB windows | 1.4–2.5 s | ~750 MB | 53–88 | — | 0 |
| 1 MiB windows | 1.33 s | 241 MB | 48 | 151 | 0 |

The current VMM expands dirty ranges on FUSE targets to 1 MiB-aligned windows and merges adjacent windows. This accepts bounded write amplification in exchange for far fewer slices, Redis round trips, and compactions. XFS keeps page-granular writes.

## Consistency and lifecycle

- **Shared visibility:** every node sees one file tree through the same metadata engine.
- **Data readiness:** `remote_status=ready` means package writeback blocks have left origin staging.
- **Origin first:** local restore remains preferred so it can reuse page cache, read cache, and staging.
- **Private import:** cross-node restore creates target-owned rootfs and memory clones instead of running directly from the origin package path.
- **Cache isolation:** local caches are neither shared nor durable; losing one does not affect uploaded data.
- **Compatibility gate:** shared storage does not replace VM compatibility checks for CPU, kernel, and KVM.

## Trade-offs

| Advantage | Cost or limitation |
|---|---|
| Shared POSIX view keeps Cube changes in the storage backend | FUSE, Redis, and object storage all participate in I/O |
| Metadata-only clones keep one copy of templates and snapshots | Small random writes create slices and require controlled write granularity |
| Local cache accelerates common templates and restores | Cross-node cold reads still depend on object-store latency and bandwidth |
| Writeback shortens the foreground write path | Cross-node use must wait for staging upload and `ready` |
| Object storage supplies capacity and durability | The metadata engine is performance-critical for clone-heavy workloads |

For JuiceFS internals, see the official [architecture](https://juicefs.com/docs/community/architecture/), [clone semantics](https://juicefs.com/docs/community/guide/clone/), and [cache guide](https://juicefs.com/docs/community/guide/cache/).
