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

`driver/s3` never writes bucket-level lifecycle configuration during `Prepare`.
Bucket lifecycle policies remain owned by the bucket administrator: configure
object-expiration rules externally on the bucket (or delete objects explicitly)
if needed. `driver/s3`'s `GC` only aborts stale multipart uploads (on MinIO,
stale multipart cleanup relies on MinIO's built-in `api stale_uploads_expiry`,
default 24h). When upgrading an existing bucket, review any lifecycle rules
(such as `blobstore-0`, `blobstore-1`) that older versions may have installed.

## S3 bucket addressing

The S3 driver accepts an optional `addressing_style` in `Config.Extra`, or
`AddressingStyle` in `s3.Options`:

| Value | Behavior |
| --- | --- |
| `auto` | Let minio-go choose using its endpoint and bucket heuristics. |
| `path` | Force path-style URLs: `https://s3.example.com/bucket/key`. |
| `virtual` | Force virtual-hosted-style URLs: `https://bucket.s3.example.com/key`. |

A non-empty explicit value takes precedence over the legacy `path_style`
(`s3.Options.PathStyle`) boolean. Invalid explicit values make `blobstore.Open`
return an error. When unset or empty, the old behavior is preserved: `true`
forces path style, while `false` selects minio-go auto detection. A raw
`Config.Extra` with neither key retains its existing path-style default.
Existing MinIO deployments therefore need no configuration change.

CubeOps, CubeMaster, and CubeTemplateCenter fail startup if the active S3
backend has an invalid explicit addressing style. Helm and the one-click
installer reject invalid values earlier.

`auto` does **not** guarantee virtual-hosted addressing for COS or OBS. If
an endpoint requires virtual-hosted requests, set `virtual` explicitly;
there are no provider-domain special cases. Use a service endpoint without
the bucket name or a path prefix. Virtual-hosted requests require DNS and
TLS to work for the resulting bucket-prefixed hostname. Use a DNS hostname
for both endpoints. The driver rejects IP literals in `virtual` mode. With
a dotted bucket name, a single-label wildcard certificate such as
`*.s3.example.com` does not cover the resulting hostname. Use a certificate
that covers the full hostname, choose a bucket name without dots, or use
path style if the provider supports it.

The same policy configures both data requests against `endpoint` and signed
GET URLs against `node_endpoint` (which defaults to `endpoint`). With `auto`,
minio-go can make a different choice for each endpoint; an explicit `path`
or `virtual` applies to both.

Callers expose the option as follows:

| Caller | Configuration | Legacy boolean | Default lookup |
| --- | --- | --- | --- |
| CubeOps | YAML `s3.addressing_style` / env `CUBE_OPS_S3_ADDRESSING_STYLE` | YAML `s3.path_style` / env `CUBE_OPS_S3_PATH_STYLE` | Path |
| CubeMaster / CubeTemplateCenter | Env `CUBE_S3_ADDRESSING_STYLE` | Env `CUBE_S3_USE_PATH_STYLE` | Auto |
| Helm, CubeOps | `cubeOps.s3.addressingStyle` | `cubeOps.s3.pathStyle` | Path |
| Helm, template artifacts | `controlPlane.artifactStore.s3AddressingStyle` | `CUBE_S3_USE_PATH_STYLE` | Path with chart-injected bundled MinIO, Auto with chart-injected external S3 unless `volumeS3.pathStyle=true` |

Both Helm values default to empty, preserving the legacy behavior. The artifact
Helm value applies when `controlPlane.artifactStore.s3Backed=true`; a matching
`CUBE_S3_ADDRESSING_STYLE` entry in `global.env` overrides the chart-generated
value. Entries in `controlPlane.master.env` and `controlPlane.templateCenter.env`
take precedence over `global.env` for their respective services. Non-empty
CubeOps environment values override the corresponding YAML fields.

In one-click deployments, set `CUBE_S3_ADDRESSING_STYLE` in `.env`; CubeOps
inherits it when `CUBE_OPS_S3_ADDRESSING_STYLE` is empty after trimming
whitespace. The installer persists the keys that are set to `.one-click.env`,
and the warehouse keeps its separate bucket.
The effective explicit style takes precedence over `CUBE_OPS_S3_PATH_STYLE`
and the old path-style flag derived from `CUBE_S3_S3FS_EXTRA_OPTS`. The
one-click CubeOps launcher warns when the legacy path-style setting
conflicts with an explicit `auto` or `virtual` style.
On upgrade, explicit `.env` values take precedence over installer environment
values, including empty values. If neither supplies a key, its previous runtime
value is preserved.

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
