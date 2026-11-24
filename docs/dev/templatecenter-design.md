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
| GET/HEAD `/cube/template/artifact/download` | proxied to TC | user-facing file serving / S3 proxy |
| GET/HEAD `/cube/template/artifact/direct-download` | CubeMaster or TC, outside API-key middleware | Cubelet-only artifact pull authenticated by a short-lived artifact capability |
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
- Direct artifact pulls use `CUBE_TEMPLATE_CENTER_NODE_ADDR` or
  `common.template_center_node_addr` when the node-reachable TC address differs
  from the Master-to-TC build address. If unset, CubeMaster falls back to
  `CUBE_TEMPLATE_CENTER_ADDR`; operators must ensure Cubelet nodes can reach it.

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

### 6.3 Cubelet artifact delivery

Before each per-node `CreateImage`, CubeMaster selects exactly one data source
and puts only that source's URL into the node request. S3-backed artifacts use a
fresh presigned GET URL when Master can sign the object. TC direct-download is
an optional rollout-gated source; when disabled/unavailable, Master serves the
artifact through its compatible download endpoint. S3/TC/Master direct URLs use
a configurable short-lived authorization. `common.artifact_download_capability_ttl`
defaults to one hour and is bounded to 24 hours; the one-hour default allows
large artifacts on slower node links while keeping leaked links time-limited.
If TC's node-facing address is configured and the artifact is eligible for TC,
CubeMaster selects that direct endpoint after S3 presigning is unavailable. The
TC/Master HMAC capability is bound to artifact ID and expiry, uses the shared
callback secret with a separate domain separator, and is sent only in
`X-Cube-Artifact-Token`, never in the URL.

The two download routes accept deliberately different credentials, because
they sit on different sides of the user API-key middleware:

| Route | Mounted | Accepts |
| --- | --- | --- |
| `/cube/template/artifact/download` | inside API-key auth | long-lived `DownloadToken` or a capability |
| `/cube/template/artifact/direct-download` | outside API-key auth | capability only |

The long-lived `DownloadToken` is a bearer credential with no expiry: it is
stored in plaintext on the artifact row, handed to every Cubelet that ever
pulled that artifact, and only rotates on re-finalize. Behind the API-key
middleware that is acceptable, since the caller is already authenticated.
Accepting it on `direct-download` would mean a single leaked token grants
permanent access to the artifact's bytes with no second factor, so the route
refuses it — `CubeMaster/pkg/service/httpservice/cube/routes.go` marks the
request capability-only, and `validRootfsArtifactDownloadToken` honors the
mark. CubeTemplateCenter mounts the same registration function, so both ends
of the direct path enforce this. Nothing regresses: when capability signing is
unavailable CubeMaster emits `ArtifactSourceLegacy` and strips the direct
annotations entirely, so a Cubelet never presents a long-lived token to
`direct-download` in the first place.

CubeMaster always preserves the stable Master artifact URL and legacy query
token in the existing annotations, and adds the selected S3/TC/Master direct
endpoint and short capability in separate additive annotations. New Cubelets
prefer the direct endpoint when present. Older Cubelets ignore the unknown
annotations and continue downloading from CubeMaster, so a partial Cubelet
rollout needs no version threshold. S3 remains selectable independently. TC
direct-download is selected from the existing node-facing TC address and
artifact storage metadata. S3 and TC remain mutually exclusive per-node
choices.

`common.artifact_download_direct_enabled` (default true, overridable by
`CUBEMASTER_ARTIFACT_DOWNLOAD_DIRECT_ENABLED`) is the single rollout switch for
all of it. When false, CubeMaster skips both the presign and the TC/Master
direct endpoint and emits only the legacy proxied annotations, which is the
pre-feature behavior; a Cubelet never has to be aware of the switch. Because
the proxied route is always valid, the switch is a bandwidth/safety trade and
not a functional toggle. Artifact store identity does require one additive
column (`bucket`, migration `20261119140000_rootfs_artifact_store_identity`);
historical rows are deliberately left empty there, so they stay reusable and
deletable without inheriting the current deployment's bucket.

Cubelet downloads the ext4 directly from the selected source, then verifies
both the expected SHA-256 and byte size before atomically publishing the cache
file. Direct S3 requires node egress to the object endpoint; direct TC requires
Cubelet reachability to the node-facing TC address. Expired presigns/capability
tokens are renewed by a new Master distribution attempt. Legacy persisted
requests keep the stable Master download path for compatibility.

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

`ArtifactNodePlacement` rows record which nodes hold a copy; they drive both
Phase 2 deletion and download locality.

A node that stays absent from the node view for 15 minutes (the async
operation timeout; health is ignored, only absence counts) is skipped in Phase
2 so a dead node cannot pin the artifact in CLEANUP_PENDING forever. Its row is
kept with `deferred_at` set while the other rows are dropped and TC is
notified. Each artifact GC pass retries deferred rows whose node is back in
the view, using the node's current address, and deletes a row only after a
confirmed destroy. A node that never returns keeps its row. Legacy `ip:` ids
are never treated as absent.

**Scope and limits**:
- This mechanism covers deferred cleanup of `ArtifactNodePlacement` rows.
- crashed/unhealthy-but-registered nodes are out of scope: only complete
  disappearance from the node view counts as absence, health is ignored.
- Legacy `ip:` ids come from the 20260623145000 backfill of replica rows
  without `node_id`. They are never treated as absent: an artifact with only
  `ip:` placements on a retired node stays CLEANUP_PENDING. These are a closed
  historical set; verify with
  `SELECT COUNT(*) FROM t_cube_artifact_node_placement WHERE node_id LIKE 'ip:%'`
  and clean up manually if needed.
- User-visible delay: 15m grace + up to one 10m GC cadence, i.e. at most ~25
  minutes after the node disappears before the artifact finalises.

Deferred rows are never reaped automatically: the row is the only record that
the node may still hold the files, so dropping it by age would silently leak
them if the node returns. Once a node is confirmed gone for good, an operator
can release its rows with `DELETE FROM t_cube_artifact_node_placement WHERE
deferred_at > 0 AND node_id = '<node-id>'`. **Back up first, use a controlled
CLI, and watch relevant metrics.**

The absence start time is kept in process memory only: a Master restart resets
the timer, and HA replicas keep independent clocks. Deferral is idempotent
(`deferred_at = 0` guard), so replicas disagreeing on the exact moment is safe;
a restart merely postpones convergence by up to one grace period. The deferral
log records the absence start and elapsed time for observability.

**Upgrade order**: the `deferred_at` column is added by migration
`20261112071208_artifact_placement_deferred_at.sql`. Apply the migration before
rolling out new CubeMaster / CubeTemplateCenter binaries; otherwise the GC scan
and the TC backstop will fail with an unknown-column error. With
`CUBE_AUTO_MIGRATION=false`, run the migration SQL manually before starting the
new binaries.

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

#### Reclaiming the loser of a build race

A build that loses the fingerprint race has already uploaded its object under
its own artifact id, and adopting the winner means that id never gets a row.
Since the artifact GC walks rows, an object with no row would never become a GC
candidate — one full rootfs leaked per lost race. Master therefore asks TC to
delete the reported object after an adopt, and only then. The guards are
deliberately asymmetric, because a wrong delete destroys a live template while
a skipped delete only wastes storage: the winner's own bytes are never touched
(same artifact id or same object key), a report without a complete store
identity is treated as "nothing was uploaded", and an artifact id that still
has a row in any state is left to the GC that owns it. A failed reclaim is
logged with the object key for manual cleanup and never fails the job.

## 10. Known limitations and follow-ups

Released with full awareness; each item lists the planned follow-up.

- ~~**37-day presigned URLs are not refreshed.**~~ Resolved by §6.3: the URL
  recorded at build time is no longer what a Cubelet downloads. Each
  `CreateImage` mints a fresh presign bounded by
  `common.artifact_download_capability_ttl`, so a template older than any
  presign TTL no longer needs a redo. What remains stored on the row is the
  stable `storage_backend` / `bucket` / `object_key` identity, not a credential.
- **envd-era redo goes straight to FAILED.** Redoing a template built by the
  legacy in-process builder has no reusable artifact record and fails fast
  rather than rebuilding; recreate such templates from the source image.
- **TC still depends on CubeMaster packages.** TC reuses CubeMaster's config
  loader, log, recov, nodemeta and templatecenter store packages instead of a
  shared library, which is why TC's go.mod carries a `replace` on CubeMaster.
  Extracting the shared pieces into `pkgs/` is follow-up work.
