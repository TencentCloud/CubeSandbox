# blobstore

Shared immutable-blob storage for CubeSandbox. Used by
[CubeOps](../../CubeOps) (component warehouse) and
[CubeTemplateCenter](../../CubeTemplateCenter) / [CubeMaster](../../CubeMaster)
(template rootfs artifacts).

The core package (`Open` / drivers) does **not** read environment variables.
Callers parse `CUBE_S3_*` / `CUBE_OPS_S3_*` / `CUBE_*_STORE_BACKEND` and pass a
`Config`. CubeMaster and CubeTemplateCenter share the artifact-store parse in
the optional [`configenv`](configenv) subpackage (`CUBE_S3_*` /
`CUBE_ARTIFACT_*`). CubeOps keeps its own `CUBE_OPS_*` + YAML path. The core
package must not import `configenv`.

## Drivers

Blank-import a driver so it registers itself:

```go
import (
    "github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
    _ "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/fs"
    _ "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/s3"
)

store, err := blobstore.Open(ctx, cfg)
```

| Driver | Name | Use |
| --- | --- | --- |
| `driver/s3` | `s3` | MinIO / COS / any S3-compatible bucket |
| `driver/fs` | `fs` | Local disk, PVC, NFS, CFS |
| `memory` | `memory` | Tests |

`fs` can mint HMAC GET URLs that `gateway.Handler` serves. Cubelet only
needs a normal HTTP GET, so nodes do not change when switching backends.
Production defaults stay on `s3`; opt in to `fs` via
`CUBE_OPS_STORE_BACKEND` / `CUBE_ARTIFACT_STORE_BACKEND`.

## S3 bucket addressing

The S3 driver accepts an optional `addressing_style` in `Config.Extra`, or
`AddressingStyle` in `s3.Options`:

| Value | Behavior |
| --- | --- |
| `auto` | Let minio-go choose using its endpoint and bucket heuristics. |
| `path` | Force path-style URLs: `https://s3.example.com/bucket/key`. |
| `virtual` | Force virtual-hosted-style URLs: `https://bucket.s3.example.com/key`. |

A non-empty explicit value takes precedence over the legacy `path_style`
(`s3.Options.PathStyle`) boolean. Invalid explicit values fail S3 store
initialization. When unset or empty, the old behavior is preserved: `true`
forces path style, while `false` selects minio-go auto detection. A raw
`Config.Extra` with neither key retains its existing path-style default.
Existing MinIO deployments therefore need no configuration change.

`auto` does **not** guarantee virtual-hosted addressing for COS or OBS. If
an endpoint requires virtual-hosted requests, set `virtual` explicitly;
there are no provider-domain special cases. Use a service endpoint without
the bucket name or a path prefix. Virtual-hosted requests require DNS and
TLS to work for the resulting bucket-prefixed hostname.

The same policy configures both data requests against `endpoint` and signed
GET URLs against `node_endpoint` (which defaults to `endpoint`). With `auto`,
minio-go can make a different choice for each endpoint; an explicit `path`
or `virtual` applies to both.

Callers expose the option as follows:

| Caller | Configuration | Legacy boolean |
| --- | --- | --- |
| CubeOps | YAML `s3.addressing_style` / env `CUBE_OPS_S3_ADDRESSING_STYLE` | YAML `s3.path_style` / env `CUBE_OPS_S3_PATH_STYLE` |
| CubeMaster / CubeTemplateCenter | Env `CUBE_S3_ADDRESSING_STYLE` | Env `CUBE_S3_USE_PATH_STYLE` |
| Helm, CubeOps | `cubeOps.s3.addressingStyle` | `cubeOps.s3.pathStyle` |
| Helm, template artifacts | `controlPlane.artifactStore.s3AddressingStyle` | Existing `CUBE_S3_USE_PATH_STYLE` configuration |

Both Helm values default to empty, preserving the legacy behavior. The artifact
Helm value applies when `controlPlane.artifactStore.s3Backed=true`; a matching
`CUBE_S3_ADDRESSING_STYLE` entry in `global.env` overrides the chart-generated
value. CubeOps environment variables override the corresponding YAML fields.
For example, for a COS or OBS bucket that requires virtual-hosted requests:

```yaml
# CubeOps YAML: replace these example endpoint, bucket, and region values.
# Supply credentials through CUBE_OPS_S3_ACCESS_KEY_ID and
# CUBE_OPS_S3_SECRET_ACCESS_KEY using your deployment's secret mechanism.
s3:
  endpoint: "https://s3.example.com" # Provider's regional S3 endpoint, no bucket.
  bucket: "your-bucket-name"
  region: "your-region"
  addressing_style: "virtual"
```

Set `CUBE_S3_ADDRESSING_STYLE=virtual` alongside the existing `CUBE_S3_*`
connection settings in **both** CubeMaster and CubeTemplateCenter when their
template artifacts use the same kind of endpoint. These settings control
the shared blobstore for component packages and template artifacts; the S3
Volume plugin and S3lvol/SPDK storage have separate configurations.
