# CubeTemplateCenter

The standalone process that builds templates: pulls the image, runs the build in an envd sandbox, produces the rootfs ext4, computes the fingerprint, and reports the result back to CubeMaster. CubeMaster handles the rest: job persistence, the public API, and cross-node distribution.

The logic lives in `CubeMaster/pkg/templatecenter`; TC just runs it as its own process.

## Split with CubeMaster

| Responsibility | CubeMaster | TC |
|---|---|---|
| Template API (`/cube/template*`) | serves it | no |
| Job persistence / state machine | owns it | no |
| Build (pull, ext4, fingerprint) | local by default | takes over when enabled |
| Artifact to Cubelet | reads shared disk | writes only |
| Cross-node distribution / redo | owns it | no |

The artifact never crosses the network: both mount the same disk (`/data/CubeMaster/storage`); TC writes, CubeMaster reads. So they must be co-located and single-replica.

## Configuration

One switch, two addresses:

| What | Where | Value |
|---|---|---|
| Use TC for builds | CubeMaster `conf.yaml` | `templatecenter_enabled: true/false`, default false |
| CubeMaster finds TC | env var | `CUBE_TEMPLATE_CENTER_ADDR`, e.g. `http://127.0.0.1:8090` |
| TC reports to CubeMaster | env var | `CUBE_MASTER_ADDR`, e.g. `http://127.0.0.1:8089` |

Addresses change with the deployment, so they are env vars, not conf keys. While the switch is false, neither address is read.

## Run

No `-conf` flag; config is located via env var:

```bash
export CUBE_TEMPLATE_CENTER_CONFIG_PATH=/path/to/conf.yaml
export CUBE_MASTER_ADDR=http://127.0.0.1:8089
./templatecenter
```

Listens on `:8090` by default (CubeMaster uses `:8089`). Bind address and port come from `common.http_bind` and `common.http_port` in conf.yaml.

## Deploy

**Kubernetes (recommended)**, one Helm flag:

```bash
helm upgrade --install cube deploy/kubernetes/chart \
  -n cube-system --set controlPlane.templateCenter.enabled=true
```

Conf, both addresses, PVC, and same-node affinity are wired automatically.

**Bare metal / one-click**: `cube-sandbox-cubetemplatecenter.service` is installed but not started. To enable: set `templatecenter_enabled: true` in CubeMaster's `conf.yaml`, set `CUBE_TEMPLATE_CENTER_ADDR` in `.one-click.env`, restart both services.

**Why single-replica**: artifacts live on a node-local disk with no cross-node sharing. A second replica can't read the first one's files, can't take over its build, and races it on the same directory. For availability, scale CubeMaster.

## API

Called mainly by CubeMaster:

| Method | Path | Purpose |
|---|---|---|
| POST | `/tc/api/v1/build` | submit a build job |
| GET | `/health` | probe |
| GET | `/metrics` | Prometheus metrics |

When a build finishes, TC reports back: `POST $CUBE_MASTER_ADDR/internal/template/jobs/:job_id/status`.

## Layout

```
pkg/tcconfig/     env-var reading
pkg/build/        build execution + status reporting
pkg/reconcile/    job reconciliation
pkg/httpservice/  gin server (template routes only)
```

---

[中文](README.md)
