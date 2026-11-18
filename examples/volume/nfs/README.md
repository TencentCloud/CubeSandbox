# NFS volume plugin

[中文](README.zh.md)

The `nfs` binary Volume plugin lets sandboxes on different Workers share an NFSv3 directory. Cubelet mounts each volume on demand and exposes it to the sandbox through virtiofs. Sandboxes in the same Cubelet namespace on one Worker share the underlying mount. Cubelet unmounts it when the last sandbox detaches.

## Build and configure

Build the binary for the target Linux architecture from this directory.

```bash
go build -o cube-volume-nfs ./cmd/cube-volume-nfs
```

Install the binary on CubeMaster and every eligible Cubelet. The one-click and Kubernetes release bundles do not include this plugin; build and install it separately. Each Cubelet needs the Linux NFS client tools, permission to mount filesystems, and network access to the NFS server and `mountd`.

Add the plugin to `conf.yaml` on CubeMaster.

CubeMaster uses the first configured volume plugin as the default driver. Keep an existing default first unless all callers using the default can provide an explicit, pre-provisioned NFS volume name. The example below assumes callers select `driver="nfs"` explicitly.

```yaml
volume_plugins:
  - name: nfs
    type: binary
    binary_path: /opt/cube-plugins/cube-volume-nfs
```

Add the plugin to `config.toml` on Cubelet.

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name = "nfs"
  type = "binary"
  binary_path = "/opt/cube-plugins/cube-volume-nfs"
```

Mount the same NFS export on every CubeMaster and point `CUBE_NFS_CONTROL_ROOT` at its absolute local path, such as `/mnt/nfs-volumes`. CubeMaster needs permission to create binding files using hard links in the provisioned volume directories.

Set `CUBE_NFS_EXPORT` in the Cubelet process environment, for example `nfs.example:/exports/volumes`. The plugin mounts `<export>/<volume-id>` on demand, checks the binding saved by CubeMaster, and shares only its `data` directory with the sandbox. The export must allow Workers to mount these subdirectories with read-write access. Make `mount.nfs` and `umount` available in the Cubelet process's `PATH`. See the [Volume plugin guide](../../../docs/guide/volume-plugin.md) for deployment and SDK setup.

## Register and mount a volume

An explicit volume name is required and must match the provisioned directory name. Use 1 to 128 ASCII letters, digits, underscores, or hyphens. Omitting the name makes CubeMaster generate a UUID, for which no directory has been provisioned. Before registering a volume, provision its `data` directory on the NFS server and assign the intended filesystem permissions. For this example, use `/exports/volumes/session-1/data` and register `session-1`.

This plugin assumes one trust domain per export. The first successful Create claims an unbound directory; the binding prevents later registrations from reusing it, but does not establish the first caller's entitlement to that directory. The plugin does not enforce tenant ownership. Only trusted operators should register volumes in a shared export. If callers are mutually untrusted, an external authorization layer must restrict both registration and mounting to their assigned directories before they reach the Volume API.

```python
from cubesandbox import Sandbox, Volume, VolumeMount

volume = Volume.create("session-1", driver="nfs")

with Sandbox.create(volume_mounts={"/workspace": volume}) as writer:
    writer.files.write("/workspace/example.txt", "shared data")
    with Sandbox.create(
        volume_mounts={"/workspace": VolumeMount(volume, read_only=True)},
    ) as reader:
        print(reader.files.read("/workspace/example.txt"))
```

The underlying mount uses `rw,nosuid,nodev`. Before reusing a mount, the plugin checks its source and access mode. Cubelet enforces read-only access per sandbox through the bind mount and virtiofs backend. Write access depends on the NFS export, UID and GID mapping, filesystem permissions, ACLs, and `root_squash`. External applications can use the same NFS directory. Keep parent directories under platform control and restrict direct network access to the export from sandbox workloads.

Use a test volume to verify cross-Worker reads and writes, write rejection from a read-only sandbox, and data persistence after the sandboxes exit.

Missing provisioned directories, invalid IDs, and an already-claimed directory return `error_kind: "invalid_argument"` (HTTP 400 through CubeMaster). A broken control-root configuration or filesystem I/O failure remains an internal error. Public hook errors omit server filesystem paths and mount-helper output. The plugin writes diagnostic details to stderr; the binary drivers may discard stderr after a successful process exit, so operators may need to capture it explicitly for troubleshooting.

The plugin uses a foreground hard NFS mount. As documented in [nfs(5)](https://man7.org/linux/man-pages/man5/nfs.5.html), `retry=0` stops mount retries after the first failed attempt for both foreground and background mounts; it does not limit the duration of an individual attempt or later filesystem I/O. Each hook has a 20-second context, and external helpers are cancelled when it expires. This is not a hard deadline for filesystem operations: a server outage can block application I/O, host-side path checks, or a helper in uninterruptible kernel I/O until the server recovers. Attach-failure cleanup uses a separate 3-second context. Plan server availability and outage recovery accordingly.

## Unregister and retain data

After all sandboxes detach, call `Volume.destroy(volume.volume_id)` to unregister the volume. The NFS directory remains with its binding. A later registration of the same ID is rejected while that directory is still in place.

To reuse an ID, the storage administrator must retire the previous directory and provision a fresh `<volume-id>/data` path. Move retained data and its binding together.

### Recover a failed registration

Create can publish the binding before CubeMaster commits the volume record. If that database update fails, rollback invokes Destroy, which deliberately retains both the data and binding. Retrying registration then reports that the directory is already claimed.

Before recovery, stop concurrent registration attempts for the ID and confirm that CubeMaster has no volume record or attachments on any Worker. A storage administrator can then retire the directory with its binding, provision a fresh `<volume-id>/data` path, and migrate the intended data before registering the same explicit name again. Do not remove the binding from an active volume.

## Verification

Run `go test ./...` from this directory for binding, mount metadata, reference-count, detach, and error-classification regressions. These tests use temporary directories and injected mount operations; they do not mount an NFS server.

The shared `tests/e2e/sdk_compat/cases/volume` CRUD case creates a randomly named volume and cannot run unchanged with only `SDK_E2E_VOLUME_DRIVER=nfs`. Prepare a dedicated `<volume-id>/data` directory first and use that explicit ID in a fixture, following the SDK example above. Verify writes from one sandbox, reads from another Worker, rejected writes through `VolumeMount(..., read_only=True)`, and persistence after detaching both. Unregister the test volume afterward and retain or retire its directory with the binding. Provision a fresh directory before repeating the registration test.
