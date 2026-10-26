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
