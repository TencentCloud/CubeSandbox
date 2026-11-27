# Writable Disk Reclaim

Writable disk reclaim releases unused storage behind a sandbox's writable root disk after files are deleted. It is useful for long-running sandboxes that frequently create and remove temporary files, build outputs, or caches.

Reclaim is disabled by default and configured when building a template. Every sandbox created from that template uses the same setting. The setting applies to the writable root disk, while read-only images and attached data volumes keep their existing behavior.

Reclaim adds storage I/O and can increase the time spent deleting files or synchronizing writes, especially on remote storage. Enable it for workloads where reducing storage use is worth that cost; short-lived sandboxes with little file churn can keep the default.

## Choose a storage backend

The writable root filesystem in the guest must be ext4. The selected nodes must have a runtime version that supports writable disk reclaim and the corresponding storage backend configured.

| Template backend | Setup | How space is released |
| --- | --- | --- |
| `xfs` | Writable images stored on an XFS filesystem on the host. | Unused ranges are punched out of the image, returning unshared blocks to the host filesystem. |
| `jfs` | A configured [JuiceFS service](./cross-node-snapshot/juicefs.md). | JuiceFS removes the file mapping; object storage usage falls as its background compaction and cleanup release unreferenced data. |
| `s3` | A configured [CubeS3lvol service](./cross-node-snapshot/cubes3lvol.md) with block-device discard support. | CubeS3lvol processes discard requests and releases backing chunks and objects according to its storage policy. |

When reclaim is enabled, a node that cannot provide the required storage capability rejects sandbox creation. Prepare the storage services and upgrade the target runtimes before building an enabled template.

## Enable reclaim when building a template

The examples use the `xfs` backend and a base image with `envd` listening on port `49983`. For a configured JuiceFS or CubeS3lvol deployment, select `jfs` or `s3` instead.

### CLI

Add `--writable-disk-reclaim` to the template build command:

```bash
cubemastercli tpl create-from-image \
  --image ghcr.io/tencentcloud/cubesandbox-base:latest \
  --backend xfs \
  --writable-layer-size 2G \
  --writable-disk-reclaim \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Wait for the returned build job to finish:

```bash
cubemastercli tpl watch --job-id <job_id>
```

Once the template is `READY`, use its template ID to create sandboxes as usual. See [Create Templates from OCI Images](./tutorials/template-from-image.md) for the complete build workflow.

### API

Set `writableDiskReclaim` to `true` in a `POST /templates` request:

```json
{
  "image": "ghcr.io/tencentcloud/cubesandbox-base:latest",
  "backend": "xfs",
  "writableLayerSize": "2G",
  "writableDiskReclaim": true,
  "exposedPorts": [49983],
  "probePort": 49983,
  "probePath": "/health"
}
```

Omit the field or set it to `false` to build a template with reclaim disabled.

### SDKs

Use the corresponding template build option:

| SDK | Option |
| --- | --- |
| [Python](./tutorials/sdk/python.md) | `Template.build(..., writable_disk_reclaim=True)` |
| [Node.js](./tutorials/sdk/nodejs.md) | `Template.build({ ..., writableDiskReclaim: true })` |
| [Go](./tutorials/sdk/go.md) | `BuildTemplateOptions.WritableDiskReclaim`, a `*bool` pointing to `true` |

For example, in Go, add the field to the options passed to `client.BuildTemplate`:

```go
enabled := true
opts := cubesandbox.BuildTemplateOptions{
    Image:               "ghcr.io/tencentcloud/cubesandbox-base:latest",
    WritableLayerSize:   "2G",
    WritableDiskReclaim: &enabled,
    // Keep the ports, probe, and other options used by your template.
}
```

Go's `nil` omits the option; a pointer to `false` explicitly disables it.

## Use and change the setting

After a sandbox starts, normal file deletion triggers reclaim through the guest filesystem. There is no separate reclaim API or periodic trim job to configure.

The policy stays with the template. A sandbox create request cannot override it, and rebuilding an existing template keeps its stored policy. To enable or disable reclaim for a workload, build a new template with the desired setting and use that template for new sandboxes.

Pause/resume and snapshot-based clones preserve the disk's reclaim setting. See [Snapshot, Rollback & Clone](./snapshot-rollback-clone.md) for those workflows.

## Understand storage usage

Reclaim changes the backing storage allocation, not the disk's configured capacity. A writable disk configured as `2G` still has the same capacity after reclaim; the released backing space becomes available according to the storage backend.

Space still referenced by a snapshot or clone remains in use until those references are released. On JuiceFS and CubeS3lvol, object storage cleanup can also happen after the file deletion completes, so storage usage may fall later.

Measure the storage you want to save: allocated image bytes on XFS, or backing object usage on JuiceFS and CubeS3lvol. The guest's free-space report and the host image's logical file size do not show how much backing storage has been released.
