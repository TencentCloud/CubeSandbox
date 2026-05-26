# RollbackSnapshot

Roll back a running sandbox VM to a previously taken snapshot.

## Overview

The caller pauses the current VM, snapshots it to a temporary path, then resumes from a
caller-supplied snapshot URL. On success the sandbox continues running from the target
snapshot state. The temporary pause snapshot is discarded automatically.

**Precondition:** the sandbox must be in `Normal` (running) state. The operation will
fail if the sandbox is already paused, pausing, or has pending exec tasks.

## Trigger

This action is invoked via the **containerd Shim v2 `UpdateContainer` RPC**
([spec](https://github.com/containerd/containerd/blob/main/core/runtime/v2/README.md)).
The caller sets custom `annotations` on the `UpdateContainerRequest`; CubeShim's
`update_ext::update_route()` dispatches on `cube.shimapi.update.action`.

```
caller  ──UpdateContainerRequest { id, annotations }──▶  CubeShim (ttrpc)
                                                              │
                                                              └─▶ update_ext::update_route()
                                                                        │
                                                                        └─▶ RollbackSnapshot handler
```

| Annotation Key | Required | Value |
|----------------|----------|-------|
| `cube.shimapi.update.action` | Yes | `"RollbackSnapshot"` |
| `cube.shimapi.update.rollback.restore_config` | Yes | JSON-encoded `RestoreConfig` (see below) |

## `RestoreConfig` Schema

```jsonc
{
  // [Required] URL of the snapshot to restore from.
  // Supported schemes: file:// (e.g. "file:///data/snapshots/my-snap")
  "source_url": "file:///data/snapshots/my-snap",

  // [Optional] Read memory range data from a separate existing memory blob
  // path instead of source_url. Other snapshot data (config, state JSON) is
  // still read from source_url. Bare /dev paths are preferred; file:// URLs
  // remain compatible.
  "memory_vol_url": "/dev/vdb",

  // [Optional] Pre-fault memory pages after restore (default: false).
  // When true, all memory pages are brought in immediately instead of on-demand.
  "prefault": false,

  // [Optional] Enable dirty-log tracking after restore (default: false).
  "dirty_log": false,

  // [Optional] Replace block devices after restore.
  "disks": [ /* DiskConfig, see below */ ],

  // [Optional] Replace network interfaces after restore.
  "net": [ /* NetConfig, see below */ ],

  // [Optional] Replace virtio-fs mounts after restore.
  "fs": [ /* FsConfig, see below */ ],

  // [Optional] Replace vsock device after restore.
  "vsock": { /* VsockConfig, see below */ },

  // [Optional] Replace persistent-memory devices after restore.
  "pmem": [ /* PmemConfig, see below */ ]
}
```

---

## Device Config Schemas

### `DiskConfig`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `path` | `string \| null` | `null` | Path to the disk image file |
| `readonly` | `bool` | `false` | Mount disk as read-only |
| `direct` | `bool` | `false` | Use `O_DIRECT` I/O |
| `iommu` | `bool` | `false` | Enable IOMMU for this device |
| `num_queues` | `uint` | `1` | Number of virtio queues |
| `queue_size` | `uint` | `128` | Depth of each queue |
| `vhost_user` | `bool` | `false` | Use vhost-user backend |
| `vhost_socket` | `string \| null` | `null` | Path to vhost-user socket |
| `rate_limiter_config` | `RateLimiterConfig \| null` | `null` | I/O rate limiting (see below) |
| `id` | `string \| null` | `null` | Logical device identifier |
| `pci_segment` | `uint` | `0` | PCI segment number |

### `NetConfig`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tap` | `string \| null` | `null` | Host tap device name |
| `ip` | `string` | `"192.168.249.1"` | Guest-facing IP address |
| `mask` | `string` | `"255.255.255.0"` | Subnet mask |
| `mac` | `string` | random | Guest MAC address |
| `host_mac` | `string \| null` | `null` | Host-side MAC address |
| `mtu` | `uint \| null` | `null` | MTU override |
| `iommu` | `bool` | `false` | Enable IOMMU |
| `num_queues` | `uint` | `2` | Number of virtio queues |
| `queue_size` | `uint` | `256` | Depth of each queue |
| `vhost_user` | `bool` | `false` | Use vhost-user backend |
| `vhost_socket` | `string \| null` | `null` | Path to vhost-user socket |
| `vhost_mode` | `string` | `"Client"` | `"Client"` or `"Server"` |
| `id` | `string \| null` | `null` | Logical device identifier |
| `rate_limiter_config` | `RateLimiterConfig \| null` | `null` | Network rate limiting |
| `pci_segment` | `uint` | `0` | PCI segment number |

### `FsConfig` (virtio-fs)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tag` | `string` | **required** | virtiofs mount tag seen by the guest |
| `socket` | `string` | `""` | Path to vhost-user-fs socket |
| `num_queues` | `uint` | `1` | Number of virtio queues |
| `queue_size` | `uint` | `1024` | Depth of each queue |
| `id` | `string \| null` | `null` | Logical device identifier |
| `pci_segment` | `uint` | `0` | PCI segment number |
| `backendfs_config` | `BackendFsConfig \| null` | `null` | Built-in virtiofsd backend config (see below) |
| `rate_limiter_config` | `RateLimiterConfig \| null` | `null` | I/O rate limiting |

#### `BackendFsConfig` (nested in `FsConfig`)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `shared_dir` | `string` | `""` | Host directory to share |
| `thread_pool_size` | `uint` | system default | Worker thread pool size |
| `xattr` | `bool` | `false` | Enable extended attributes |
| `posix_acl` | `bool` | `false` | Enable POSIX ACLs |
| `xattrmap` | `string \| null` | `null` | xattr mapping rules |
| `announce_submounts` | `bool` | `false` | Announce submount info to guest |
| `cache` | `uint` | system default | Cache policy (0=none, 1=auto, 2=always) |
| `no_readdirplus` | `bool` | `false` | Disable READDIRPLUS |
| `writeback` | `bool` | `false` | Enable writeback cache |
| `allow_direct_io` | `bool` | `false` | Allow guest to use direct I/O |
| `read_only` | `bool` | `false` | Mount as read-only |
| `rlimit_nofile` | `uint` | system default | `RLIMIT_NOFILE` for the backend |
| `killpriv_v2` | `bool` | `false` | Enable FUSE_HANDLE_KILLPRIV_V2 |
| `security_label` | `bool` | `false` | Enable security label support |

### `VsockConfig`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cid` | `uint64` | **required** | Guest CID (Context ID) |
| `socket` | `string` | **required** | Path to the host-side vsock socket |
| `iommu` | `bool` | `false` | Enable IOMMU |
| `id` | `string \| null` | `null` | Logical device identifier |
| `pci_segment` | `uint` | `0` | PCI segment number |
| `muxer_epoll_nested` | `bool` | `false` | Enable nested epoll in vsock muxer |

### `PmemConfig`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `file` | `string` | **required** | Path to the pmem backing file |
| `size` | `uint64 \| null` | `null` | Size in bytes (auto-detect if null) |
| `iommu` | `bool` | `false` | Enable IOMMU |
| `discard_writes` | `bool` | `false` | Discard write operations |
| `id` | `string \| null` | `null` | Logical device identifier |
| `pci_segment` | `uint` | `0` | PCI segment number |

### `RateLimiterConfig` (shared)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `bandwidth` | `TokenBucketConfig \| null` | `null` | Byte-rate limit |
| `ops` | `TokenBucketConfig \| null` | `null` | IOPS limit |

#### `TokenBucketConfig`

| Field | Type | Description |
|-------|------|-------------|
| `size` | `uint64` | Bucket capacity (bytes for bandwidth, ops count for ops) |
| `one_time_burst` | `uint64 \| null` | Initial burst allowance (optional) |
| `refill_time` | `uint64` | Refill interval in milliseconds |

---

## Examples

### Minimal — restore from snapshot, no device replacement

```json
{
  "source_url": "file:///data/snapshots/my-snap"
}
```

### With separate memory volume

```json
{
  "source_url": "file:///data/snapshots/my-snap",
  "memory_vol_url": "/dev/vdb",
  "prefault": true
}
```

### Full — restore with device replacement

```json
{
  "source_url": "file:///data/snapshots/my-snap",
  "memory_vol_url": "/dev/vdb",
  "prefault": false,
  "dirty_log": false,
  "disks": [
    {
      "path": "/data/rootfs-new.qcow2",
      "readonly": false,
      "id": "rootfs"
    }
  ],
  "net": [
    {
      "tap": "tap0",
      "mac": "52:54:00:ab:cd:ef",
      "id": "eth0"
    }
  ],
  "fs": [
    {
      "tag": "workdir",
      "socket": "/run/virtiofsd/workdir.sock",
      "id": "fs0"
    }
  ]
}
```

---

## cube-runtime CLI

The `cube-runtime snapshot` command also gains a new flag in this feature:

| Flag | Default | Description |
|------|---------|-------------|
| `--memory-vol <path-or-url>` | (none) | Write memory range data to a separate existing volume or file (for example `/dev/vdb`, compatible with `file:///dev/vdb`). Other snapshot data (config, state JSON) is still written to the snapshot path. |

Example:
```bash
cube-runtime snapshot \
  --path /data/snapshots/my-snap \
  --memory-vol /dev/vdb \
  --vm-id <sandbox_id>
```

---

## Error Cases

| Error message | Cause |
|---------------|-------|
| `missing annotation: cube.shimapi.update.rollback.restore_config` | The restore_config annotation was not provided |
| `invalid cube.shimapi.update.rollback.restore_config: ...` | The annotation value is not valid JSON or missing required fields |
| `sandbox not running, cannot rollback` | Sandbox is not in `Normal` state |
| `sandbox pause forbidding, terminate exec tasks first` | There are active exec tasks blocking the pause |
| `rollback snapshot failed: ...` | Lower-level hypervisor error during pause/restore |
