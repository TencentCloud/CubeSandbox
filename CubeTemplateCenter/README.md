# CubeTemplateCenter

The standalone process that builds templates: pulls the image, runs the build in an envd sandbox, produces the rootfs ext4, computes the fingerprint, and reports the result back to CubeMaster. CubeMaster handles the rest: job persistence, the public API, and cross-node distribution.

The route layer is shared from `CubeMaster/pkg/service/httpservice` (`RegisterTemplateRoutes`); the build and storage logic lives in TC's own `pkg/build`, `pkg/image`, and `pkg/s3store`.

## Split with CubeMaster

TC owns the build (pull, ext4, fingerprint, S3 upload) and the artifact data (local file + S3 object).
CubeMaster owns the template API, job state, cross-node distribution, and deletion orchestration.
Compat matrix and download routes: CubeMaster reverse-proxies to TC (302 to S3 for downloads).

## Run

No `-conf` flag; config is located via env var:

```bash
export CUBE_TEMPLATE_CENTER_CONFIG_PATH=/path/to/conf.yaml
export CUBE_MASTER_ADDR=http://127.0.0.1:8089
./templatecenter
```

Listens on `:8090` by default (CubeMaster uses `:8089`). Bind address and port come from `common.http_bind` and `common.http_port` in conf.yaml.

## Deploy

**Kubernetes (recommended)**, plain Helm install (TC is a default control-plane component and deploys automatically with `controlPlane.enabled=true`; there is no separate switch):

```bash
helm upgrade --install cube deploy/kubernetes/chart -n cube-system
```

Conf, both addresses, PVC, and same-node affinity are wired automatically.

**Bare metal / one-click**: `cube-sandbox-cube-templatecenter.service` is part of the default control-plane stack (the control target `Wants=` it and install.sh enables it), so template builds work out of the box. The default address `http://127.0.0.1:8090` is already exported by `cubemaster-start.sh`; only override `CUBE_TEMPLATE_CENTER_ADDR` in `.one-click.env` for a split deployment.

## Usage scenarios (topology matrix)

| Scenario | TC replicas | Artifact store | OK? |
|---|---|---|---|
| Bare metal / one-click co-located | 1 | host disk shared with CubeMaster | ✅ |
| One-click multi-node (1 control + N compute) | 1 on the control node | host disk on the control node | ✅ — set `ONE_CLICK_DEPLOY_ROLE=compute` on every worker node |
| K8s single replica | 1 | node-local disk (PVC recommended; emptyDir loses artifacts on restart) | ✅ — **keep at 1 replica**; forcing multi-replica here is broken (each replica only has its own builds, any load balancer routes downloads to non-builders → 404) |
| K8s multi-replica TC | ≥2 | **requires** `s3Backed=true` **or** ReadWriteMany | ✅ |
| K8s multi-replica master only | 1 | node-local disk | ✅ — downloads proxy to the single TC |

**Not supported** — every TC replica must see the same bytes, and a load
balancer that points external traffic at "any" replica only works when the
artifact store is shared. The chart refuses these on `helm install`:

- **K8s + Internal CLB + multi-replica TC + node-local disk.** The CLB
  load-balances external requests across TC replicas, but each replica's
  builds only live on its own disk → 404 on any non-builder replica.
  Fix: `s3Backed=true`, ReadWriteMany, or `templateCenter.replicas=1`.
- **K8s + multi-replica master + default ReadWriteOnce artifact PVC.** The
  second master replica stays `Pending` forever. Fix:
  `master.persistence.enabled=false` or ReadWriteMany.
- **One-click multi-control-plane** (full install on two hosts, each with
  its own storage). The installer only models control + compute, never
  two controls. Migrate to Helm + `s3Backed=true` first.

## API

Internal endpoints (called by CubeMaster):

| Method | Path | Purpose |
|---|---|---|
| POST | `/tc/api/v1/build` | submit a build job |
| POST | `/tc/api/v1/artifact/delete` | delete artifact data (local file / S3 object) |

Public routes reverse-proxied from CubeMaster (actually served by TC):

| Method | Path | Purpose |
|---|---|---|
| GET / HEAD | `/cube/template/artifact/download` | artifact download (streams from local disk; 302 to the presigned URL for S3 artifacts) |
| GET / POST | `/cube/template/compat` | template compat matrix read/write |

Build-status polls (`/cube/template/build/:build_id/status`, `/cube/template/from-image?job_id=`) stay local on CubeMaster (they read the job rows Master writes); TC registers them too for direct-to-TC access.

Probes and metrics: `GET /health`, `GET /metrics` (Prometheus).

When a build finishes, TC reports back: `POST $CUBE_MASTER_ADDR/internal/template/jobs/:job_id/status`.

## Layout

```
pkg/tcconfig/     env-var reading
pkg/build/        build execution + status reporting + artifact deletion
pkg/image/        image pull and ext4 production
pkg/s3store/      S3/MinIO artifact store
pkg/lock/         cross-replica DB session locks (build dedup / reconciler mutual exclusion)
pkg/cube_egress_ca/  bakes the CubeEgress root CA into template rootfs
pkg/reconcile/    job reconciliation
pkg/api/          internal endpoints (/tc/api/v1/*)
pkg/httpservice/  gin server (health/metrics + proxied route registration)
```

---

[中文文档](README_zh.md)
