---
aside: false
---

# Cube + JuiceFS Cross-Node Storage Architecture

This page describes CubeSandbox's JuiceFS-based cross-node storage architecture and its primary flows.

> For deployment, see [JuiceFS service configuration](../guide/cross-node-snapshot/juicefs.md).
>
> For performance data, see [JuiceFS performance benchmarks](/blog/posts/2026-10-28-cubesandbox-juicefs-perf-benchmark).

## Goals

Cube's core storage solution currently uses the XFS filesystem. XFS separates metadata from data, so cloning copies only inode and data-block references rather than the blocks themselves, making clones fast.

The JuiceFS solution keeps this overall file-operation model while adding cross-node capability. It relies on these JuiceFS characteristics:

- **POSIX-compatible distributed filesystem:** JuiceFS exposes POSIX file-operation interfaces.
- **Distributed design:** multiple nodes can mount the same volume, with strong consistency ensuring they see the same file view.
- **Metadata and data separation:** supports fast reflink-style cloning similar to XFS.

> For a detailed introduction, see the [official JuiceFS documentation](https://juicefs.com/docs/community/introduction/).

## Architecture

<JuiceFSArchitecture mode="architecture" />

The diagram above shows Cube's JuiceFS architecture. The Redis metadata engine and MinIO object storage are on the control node, although their configuration can point to other machines. A JuiceFS client runs on every node, and every node mounts the same JuiceFS volume. All nodes see the same file view on that volume: for example, a file created by node A under `/data/cubelet/jfs` is also visible to node B under `/data/cubelet/jfs`.

Each node has separate `raw` read caches and `rawstaging` write caches for performance. The cache directories can use any filesystem; only `/data/cubelet/jfs` uses the JuiceFS volume (JuiceFS filesystem) as its backend.

## Design flows

<JuiceFSArchitecture mode="flows" />

> The flow diagrams above cover template creation, sandbox startup, and pause and restore. Select a step to see its active components, files, and data path.
>
> Sandbox startup and pause and restore can also switch between local and cross-node scenarios.

Thanks to JuiceFS's separation of metadata and data, JuiceFS can use `copy_file_range` to clone rootfs and memory files quickly.

Thanks to JuiceFS's distributed design, files created on node A are also visible on node B. During cross-node restore, node B can therefore clone them as though they were local files.
