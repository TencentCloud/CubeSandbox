# CubeTemplateCenter Design

CubeTemplateCenter (TC) is the standalone template build service split out of
CubeMaster. This document describes the architecture as implemented; code
comments reference sections here as `design §x.y`.

## 1. Overview

Historically CubeMaster built template ext4 images in-process: it pulled the
source OCI image, unpacked layers, ran `mkfs.ext4`, uploaded the artifact, and
distributed it to nodes — all inside the same process that serves the sandbox
control plane. TC takes over the data-plane half of that work.

The split exists for three reasons:

- **Privilege isolation.** Building needs root (`umoci unpack` preserving
  uid/gid, `mkfs.ext4`, loop devices). Keeping that out of the process that
  answers public API calls shrinks the blast radius.
- **Resource isolation.** A build spikes CPU/IO; the control plane's latency
  SLO should not depend on how many builds are running.
- **Independent lifecycle.** TC runs with node-local state (singleton by
  default; multi-replica in S3-backed mode, §9.1); CubeMaster
  is stateless and horizontally scalable (see §9).

## 2. Process topology

### 2.1 Processes

- **CubeMaster** — control plane: persists job rows, forwards builds to TC,
  receives status callbacks, distributes artifacts to nodes, serves all
  template writes and cached reads.
- **CubeTemplateCenter** — data plane: pulls images, builds ext4, uploads to
  S3, reports status back. Singleton by default; multiple replicas are
  supported in S3-backed mode (§9.1).

### 2.2 Communication

- CubeMaster → TC: internal HTTP `POST /tc/api/v1/build` (submit),
  `POST /tc/api/v1/artifact/delete` (physical delete).
- TC → CubeMaster: `POST /internal/template/jobs/:job_id/status` (status
  callback, authenticated — §6.1).
- Both share CubeDB and the artifact directory (§9.7).

### 2.3 Ownership split

CubeMaster owns every business-state write (job rows, definitions, replicas,
aliases) and every cubelet RPC. TC owns the artifact's physical data (local
ext4 file, S3 object) and nothing else. TC never initializes the worker
(cubelet) grpc conn pool, so any handler that can issue a cubelet RPC —
create-from-snapshot, delete, redo resume — is served by CubeMaster and is not
even routed on TC (see §4).

## 3. API and job model

### 3.1 Alias semantics

A template alias is an optional stable name (`[a-z0-9-]`, max 64 chars) that
sandbox create requests may reference instead of the generated `tpl-*` id.
`PUT /cube/template/:template_id/alias` with an absent / null / `""` alias
clears it.

### 3.2 Build job states

`t_cube_template_image_job.status`: PENDING → RUNNING → BUILT | FAILED.
Phase (`phase`) tracks the pipeline step (PULLING / BUILDING / UPLOADING /
DISTRIBUTING). BUILT is terminal for the build itself; the resume pipeline
(§3.5) then registers the artifact and distributes it.

### 3.3 Error mapping

CubeMaster also exposes `POST /cube/template/migrate` (CLI spelling: `tpl merge`) to migrate a legacy READY template artifact from CubeMaster local disk into TC-managed storage. In operational documentation, the wording should stay consistent: **`tpl merge` solves historical artifact storage convergence, while `tpl redo` solves node-side redistribution / rebuild when needed.** The typical case is that historical artifacts still live on local disk and the cluster later enables `s3Backed=true`, so those artifacts must be migrated from local disk into S3-backed storage. If the same maintenance window also needs to repopulate target nodes, run `tpl redo` after `tpl merge` completes.

The HTTP layer maps domain errors to API codes: `ErrTemplateIDRequired`,
`ErrDuplicateTemplate`, `ErrNoTemplateNodes` → params error;
`ErrTemplateStoreNotInitialized` → DB error; not-found → 130404. The staged
split moves further error translation into the store layer over time.

### 3.4 Redo

`POST /cube/template/redo` resumes a FAILED job. An artifact left READY by a
job that failed during distribution is reused instead of rebuilt (reusing
PENDING/BUILDING artifacts would read a half-written ext4). If the original artifact is no longer reusable, redo falls back to a full rebuild from `source_image_ref`.

### 3.5 Resume pipeline

On a BUILT callback CubeMaster runs three resume steps: (1) register the
remote-built artifact, (2) distribute to target nodes, (3) finalize the job
row. Distribution resolving zero target nodes fails the job with
`ErrNoTemplateNodes` and intentionally does NOT write definition/replica rows.

### 3.6 Job idempotency

Invariant **I1**: at most one active (PENDING/RUNNING) job exists per template
spec. The first-create job JSON and a REDO job's RequestJSON are written in
the same DB transaction as the state flip, so a crash cannot leave a row that
disagrees with its payload. COMMIT / SNAPSHOT_* / LEGACY jobs are excluded
from the from-image idempotency window.

## 4. Routing split

Public `/cube/template*` routes on CubeMaster:

| Route | Served by | Why |
| --- | --- | --- |
| POST `/cube/template`, `/from-image`, `/redo`; DELETE; GET; PUT alias | CubeMaster (local) | cubelet RPCs, process-local caches, or forward-build env |
| GET `/cube/template/build/:id/status`, GET `/from-image` | CubeMaster (local) | plain DB reads on master-owned rows; proxying made create-200/poll-502 when TC was down |
| GET/POST `/cube/template/compat` | proxied to TC | uncached DB read/write |
| GET/HEAD `/cube/template/artifact/download` | proxied to TC | file serving / S3 redirect |
| GET `/cube/template/artifact/...` (rootfs, CA) | CubeMaster (local) | cubelet pulls from the shared disk via master |

`RegisterTemplateRoutes` (the TC process) registers only the uncached pure-DB
and file-serving subset. The two lists are pinned by contract tests
(`TestCubeRoutesTemplateWritesStayOnCubeMaster`,
`TestCubeRoutesTemplateProxyRemainder`,
`TestTemplateCenterRoutesExcludeWritesAndCachedReads`).

## 5. Configuration

### 5.1 Naming

Everything TC reads is spelled `CUBE_TEMPLATE_CENTER_*`. CubeMaster reads
`CUBE_TEMPLATE_CENTER_ADDR` (where to submit builds) and
`CUBE_TEMPLATE_CALLBACK_TOKEN` (§6.1).

### 5.2 Address wiring

- Helm: `cube.templateCenterEndpoint` renders the in-cluster ClusterIP address
  into master's `CUBE_TEMPLATE_CENTER_ADDR` env and `template_center_addr` in
  conf.yaml. TC gets `CUBE_MASTER_ADDR` the same way.
- one-click: `cubemaster-start.sh` defaults `CUBE_TEMPLATE_CENTER_ADDR` to
  `http://127.0.0.1:8090`; TC's conf.yaml gets `__CUBETEMPLATECENTER_*__`
  placeholders resolved at install.

### 5.3 Backward compatibility window

Pre-rename spellings (`CUBE_TC_*`, `CUBE_MASTER_*`, `CUBEMASTER_*`) still work
as fallbacks and log a deprecation notice (retrievable via
`tcconfig.Warnings()`). The window exists because deployment scripts that were
not updated in lockstep would otherwise silently write artifacts to the wrong
directory, surfacing much later as download 404s. New deployments should use
only the `CUBE_TEMPLATE_CENTER_*` names.

## 6. Security

### 6.1 Callback authentication

The status callback payload is trusted wholesale by the resume pipeline — a
forged BUILT report's artifact id/sha becomes the rootfs nodes boot from. The
endpoint therefore requires a shared secret: TC sends
`X-Cube-Template-Callback-Token`, CubeMaster compares it against
`CUBE_TEMPLATE_CALLBACK_TOKEN` in constant time and rejects mismatches with
401. Both ends fail closed when the variable is unset (503) — Helm
(auto-generated `cube-template-callback-token` Secret key), one-click
(generated into `.one-click.env`), and terraform (`random_password`) all wire
it by default, so unset means misconfiguration; the only exception is the
explicit single-binary-dev opt-in `CUBE_TEMPLATE_CALLBACK_INSECURE_NO_TOKEN=true`.
Both the Master callback route and TC's internal routes are registered
outside `GinRequestMiddleware`, so `checkAuth` can never answer them with an
HTTP-200 business error that a `StatusCode == 200` client would read as
success.

### 6.2 TC build endpoint

TC's internal `/tc/api/v1/*` endpoints require the same shared token (see
§6.1) and must additionally stay on a cluster-internal / VPC-internal address
(the chart renders the optional CLB as internal-only; one-click binds TC to
loopback by default).

## 7. Reconciliation

### 7.1 Progress snapshots

Live pull progress goes to Redis; the durable terminal snapshot is flushed
through the status callback.

### 7.2 Stale build detection

The TC-side reconciler (pkg/reconcile) periodically scans for jobs whose
progress reports stopped arriving and marks them FAILED.

### 7.3 Abandoned builds

A TC restart abandons in-memory build state. The reconciler sweeps jobs stuck
in RUNNING beyond the staleness threshold (default cadence 10 minutes,
configurable via `CUBE_TEMPLATE_CENTER_RECONCILE_*`) and fails them; the
client must retry (redo reuses a READY artifact when one exists, §3.4). This
is mandatory, not optional: without it a crashed build would wedge the
template forever behind invariant I1 (§3.6).

## 8. Health and readiness

- CubeMaster: `GET /notify/health` — no DB or nodemeta dependency.
- TC: `GET /health` — ready when the store is attached AND `nodemeta` has
  initialized. `nodemeta.Ready()` reports "Init completed", NOT "the release
  manifest exists": the manifest (`release-manifest.json`) is only shipped by
  the one-click bundle, and CubeMaster runs fine without it in Kubernetes, so
  its absence must not fail readiness. Declared component versions are a
  compat-scan input only.

## 9. Storage and concurrency

### 9.1 Replica count

With `artifactStore.s3Backed=true` the durable copy lives in S3/MinIO and
local disk is per-Pod build scratch, so multiple TC (and CubeMaster)
replicas are supported: duplicate builds of the same spec are coordinated
through DB session locks (§9.2, keyed by the template-spec fingerprint) and
the losing replica reuses the winner's READY row instead of rebuilding.

Without S3 the artifact store is node-local. Multi-replica is then ALLOWED
but degraded, and the chart prints an install-notes warning: a download may
land on a master that never built the artifact (foreign-artifact retry) and
TC replicas duplicate builds of the same spec. The one combination validate
still rejects is a mounted ReadWriteOnce artifact PVC with master.replicas>1
— it cannot attach on several nodes, so the extra replica stays Pending
forever (a scheduling failure, not a degradation). A ReadWriteMany claim
(CFS/NFS — TC mounts the master's claim by default) is genuinely shared and
is neither rejected nor warned.

### 9.2 DB locks

Cross-process mutual exclusion (e.g. artifact claim during build) uses MySQL
named locks via `GET_LOCK`, replacing the per-process
`sync.Map[artifactID]*sync.Mutex` which cannot span CubeMaster and TC. Lock
names are normalized to stay under MySQL's 64-character limit.

### 9.3 Orphan GC

A background sweep on CubeMaster removes artifacts whose reference count
dropped to zero (template deleted, build failed) past their GC deadline,
running the same three-phase cleanup as online deletion (§9.5).

S3 artifacts still referenced by templates can independently evict worker `.ext4`
cache files. See [Worker rootfs cache GC](./rootfs-cache-gc); definitions,
snapshots and remote objects remain intact.

### 9.4 Job ownership

The jobs table has no owner column: after a TC restart any TC instance
reconciles every stale job (the sweep itself is singleton per tick via the
session lock, §9.2, regardless of replica count). Progress reports refresh
the staleness clock.

### 9.5 Artifact deletion

Three-phase last-owner cleanup (`cleanupArtifactFully`): Phase 1 (short TX,
row FOR UPDATE) counts remaining references and marks the row
CLEANUP_PENDING; Phase 2 (no lock, idempotent) destroys ext4 on placement
nodes; Phase 3 (short TX) re-checks references and status, drops placement
rows, then notifies TC to remove the physical data. Only TC deletes the S3
object / local file / artifact row — CubeMaster doing it used to leak S3
objects with no CLEANUP_PENDING row left for the backstop.

### 9.6 Node placements

`ArtifactNodePlacement` rows record which nodes hold a copy; they drive both the
per-node physical deletion performed while removing an artifact (the second
stage of the three-phase cleanup in §9.5) and download locality.

Deleting an artifact means telling every node that holds a copy to remove its
local ext4. A node that is already gone can never acknowledge that request, so
the whole deletion would sit in CLEANUP_PENDING and keep both the artifact row
and the artifact fingerprint it pins alive.

A node absent from the node view, or still registered with a heartbeat expired
for 15 minutes (the async operation timeout), therefore stops taking part in the
current cleanup pass: no destroy request is sent to it, and its placement row is
kept with `deferred_at` set. Rows for every other node are removed as usual, and
TC is then asked to delete the artifact so the fingerprint is released. Each
artifact GC pass retries deferred rows once their node is reachable again, using
the node's current address, and deletes a row only after a confirmed destroy. A
node that never recovers keeps its row. Legacy `ip:` ids do not participate.

**Scope and limits**:
- This mechanism covers deferred cleanup of `ArtifactNodePlacement` rows.
  When template/snapshot deletion meets a node unreachable past the grace
  period, it sends no Cubelet cleanup request, marks that node's replica row
  `cleanup_required=true` and keeps it, then continues with artifact and
  metadata cleanup, so the delete call can succeed first. The template's local
  data on that node is not yet confirmed deleted: once the node returns, the
  orphan cleanup resends the request at the node's current address and deletes
  the row only after confirmation. A node that never returns keeps the row,
  visible to operators. After the node returns, the orphan cleanup (every 5
  minutes) and the deferred artifact GC (every 10 minutes) converge within
  about 10 minutes. The user guide and the API 204 description state this
  contract.
- A `cleanup_required=true` replica row whose template and snapshot records are
  both gone only tracks node-side data and no longer counts as an artifact
  reference, so it cannot block artifact finalisation or flip it back to
  `READY`. Replicas whose template record still exists count as before.
- The replica row kept for an unreachable node is marked
  `cleanup_required=true`, meaning its node-side data still needs cleanup. The
  same flag also marks failed replicas for redo. Deletion clears it on every
  node that confirmed cleanup, so after a delete only deferred nodes keep a
  flagged row. Paths that clean every node before removing metadata (create
  rollback and rebuilding a FAILED template) clear the flag once all nodes
  confirm. A replica row awaiting cleanup is never scheduled. Do not reuse a
  deleted template id before those nodes return: the orphan cleanup replays the
  request by template id.
- An unreachable node without a replica row (e.g. a locator that only comes from
  a snapshot or job record) has nowhere to record the pending cleanup, so the
  Cubelet request is still sent and a failure fails the delete, which can be
  retried. Redo never skips unreachable nodes. The orphan cleanup skips the
  request only for nodes unreachable past grace, keeping their
  `t_cube_template_replica` cleanup records for retry after the nodes return;
  inside grace it still sends the request.
- For a node still in the view, only `HeartbeatReceivedAt` decides: the
  server-side time CubeOps last persisted its heartbeat, kept as Unix seconds in
  `t_cube_node_status.heartbeat_received_unix` and free of time zones. An
  unknown receive time fails closed. `Healthy` and `UnhealthyReason` follow the
  Cubelet wall clock and play no part: a node whose clock runs ahead keeps
  reporting Healthy long after it stops.
- CubeMaster never subtracts a CubeOps timestamp from its own clock. Each
  replica records the local time the value last changed and ages the node from
  there. It compares whole seconds, because rebuilding a snapshot from the DB
  drops sub-second precision. Clock skew between CubeOps replicas and
  CubeMaster therefore causes no false verdict. After a restart, CubeMaster
  ages each node from when it first sees it, delaying the verdict by at most
  one grace period.
- A node view without a successful sync for 3 minutes (longer than the
  worst-case sync) yields no verdict, and the first successful sync after such
  a gap restarts aging for every node. Neither a CubeOps outage nor its
  recovery therefore causes a false verdict. When over half of the nodes show
  no new receive time for a minute, CubeMaster logs a WARN, at most once a
  minute.
- The heartbeat branch requires `cube_ops_addr`: neither the DB fallback loader
  nor an older CubeOps supplies receive times, so only view absence applies.
- Legacy `ip:` ids come from the 20260623145000 backfill of replica rows
  without `node_id`. They are never treated as unreachable: an artifact with
  only `ip:` placements on a retired node stays CLEANUP_PENDING. These are a
  closed historical set; verify with
  `SELECT COUNT(*) FROM t_cube_artifact_node_placement WHERE node_id LIKE 'ip:%'`
  and clean up manually if needed.
- User-visible delay: 15m grace + up to one 10m GC cadence, i.e. at most ~25
  minutes after the node becomes unreachable before the artifact finalises.

Deferred rows are never reaped automatically: the row is the only record that
the node may still hold the files, so dropping it by age would silently leak
them if the node returns. Once a node is confirmed gone for good, an operator
can release its rows with `DELETE FROM t_cube_artifact_node_placement WHERE
deferred_at > 0 AND node_id = '<node-id>'`. **Back up first, use a controlled
CLI, and watch relevant metrics.**

Both timers, view absence and heartbeat expiry, live only in process memory
and run on the local clock. A Master restart restarts them, and HA replicas may
reach the verdict at different times. Deferral is idempotent (`deferred_at = 0`
guard), so differing verdicts cannot corrupt data, and a restart delays the
verdict by at most one grace period. Logs record the start and elapsed time.

**Upgrade order**: the `deferred_at` column is added by migration
`20261112071208_artifact_placement_deferred_at.sql`. Apply the migration before
rolling out new CubeMaster / CubeTemplateCenter binaries; otherwise the GC scan
and the TC backstop will fail with an unknown-column error. With
`CUBE_AUTO_MIGRATION=false`, run the migration SQL manually before starting the
new binaries. The heartbeat receive time is added by
`20261119125300_node_heartbeat_received_unix.sql`; apply it and finish upgrading
all CubeOps replicas before relying on this branch. Existing rows stay 0 rather
than receiving a time-zone-ambiguous backfill. Live nodes recover on their next
heartbeat; nodes already permanently unreachable before the migration remain
fail-closed while present in the view. During a rolling upgrade the receive time
can be stale or missing: an old replica neither reports it nor advances it on
conflict, so a later DB rebuild can surface a stale value. This may postpone
deferral or cause a false unreachable; do not rely on the verdict until the
rollout completes. To roll back, revert CubeMaster, CubeTemplateCenter and
every CubeOps replica before running the Down migrations: with the new binaries
still running, dropping `deferred_at` breaks the GC scan and the TC backstop,
and dropping `heartbeat_received_unix` fails every heartbeat write, so nodes
turn unhealthy and unschedulable.

### 9.7 Shared artifact store

TC writes the ext4 and CubeMaster serves it to cubelet over
`/cube/template/artifact/download` — both processes must see the same
directory. one-click: same host filesystem. Helm: the same PVC (ReadWriteOnce
is per-node, so the chart pins TC to master's node via required podAffinity;
`colocateWithMaster=false` requires a genuinely shared ReadWriteMany volume).
terraform: shared CFS when enabled, otherwise pod-local emptyDir with
single-replica co-location. Because the directory is shared, a path recorded
at build time remains valid for the reconciler even if TC has restarted since.

When the object store is the durable copy, the ext4 TC writes is not one: it is
only a build intermediate. TC therefore deletes it at build cleanup, so a build
host stops accumulating one artifact per build:

- Upload succeeded (`s3` backend): the copy is removed right after the BUILT
  report. CubeMaster streams such an artifact from the store, so nothing ever
  reads the local file again.
- Upload failed: the build reports FAILED and the copy is removed too. Reporting
  BUILT instead (the old behaviour) left the row with no storage backend, so the
  artifact was served from TC's disk — which holds only while CubeMaster shares
  that replica's artifact directory, not with a remote Master or a second
  replica.

Two cases keep the file. With `CUBE_ARTIFACT_STORE_BACKEND=fs` the store root
may be this very directory, so the file *is* the durable copy. And when the
upload succeeded but no download URL could be recorded, CubeMaster answers 502
for that row (it never falls back to TC's disk), so the bytes are kept as the
only recoverable copy instead of being deleted into nothing.

## 10. Known limitations and follow-ups

Released with full awareness; each item lists the planned follow-up.

- **37-day presigned URLs are not refreshed.** Artifacts uploaded to S3 get a
  long-lived presigned URL recorded at build time; there is no refresh job, so
  a template older than the presign TTL needs a redo to mint a new URL.
- **envd-era redo goes straight to FAILED.** Redoing a template built by the
  legacy in-process builder has no reusable artifact record and fails fast
  rather than rebuilding; recreate such templates from the source image.
- **TC still depends on CubeMaster packages.** TC reuses CubeMaster's config
  loader, log, recov, nodemeta and templatecenter store packages instead of a
  shared library, which is why TC's go.mod carries a `replace` on CubeMaster.
  Extracting the shared pieces into `pkgs/` is follow-up work.
