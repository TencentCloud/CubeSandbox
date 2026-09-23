# Agent platform integration: manual freeze / resume

This guide is for **agent orchestrators** (not end-user SDK demos) that expose
`sandbox_freeze` / `sandbox_connect_by_id` style tools on top of CubeSandbox.

## Mental model: three independent S3 layers

| Layer | What it is | What it preserves |
|-------|------------|-------------------|
| **Volume** (`cube-volume-s3` + MinIO) | Per-user mount at e.g. `/home/user/persistent` | Files across **kill + recreate** |
| **s3lvol** (`ONE_CLICK_ENABLE_S3LVOL`) | Cluster snapshot backend | Cross-node pause/snapshot |
| **Template `--backend s3`** | Template registration flag | Cross-node pause/snapshot (`xfs` templates pause on-node too; S3 is for cross-node — see [Cross-node snapshots](cross-node-snapshot.md)) |

A Volume mount does **not** replace instance freeze. Freeze keeps the **same sandbox
ID** (memory + disk state of that instance). Volume keeps **files** when the
instance is destroyed and a new one is created.

## Critical: `pause()` does not stop the idle timer

With the default `on_timeout="kill"`, a sandbox keeps its creation-time idle
deadline even after `pause()`. A paused sandbox can still be **destroyed** when
that deadline expires.

**Symptom:** `POST /pause` succeeds, CubeMaster logs show `action=pause`, but
`connect` / `GET /sandboxes/{id}` returns 404 an hour later.

**Mitigations (pick one or combine):**

1. **Extend retention** with `POST /sandboxes/{id}/timeout` and
   `{"timeout": <seconds>}` (e.g. `{"timeout": 86400}` for 24h). Apply it
   **before** `pause` on a running sandbox, or **immediately after** pause
   returns — the idle sweeper runs every few seconds and still evaluates the
   pre-pause deadline in the gap between the two calls.
2. **On create**, use `lifecycle.on_timeout="pause"` plus a long `timeout`, or
   `timeout=NEVER_TIMEOUT` when policy allows.
3. **Use `POST /sandboxes/{id}/connect`** before talking to envd when resuming
   a paused sandbox (see below).

## Recommended control-plane flow

### Freeze (agent tool)

```text
1. POST /sandboxes/{id}/timeout  {"timeout": 86400}   # while still running
2. POST /sandboxes/{id}/pause          # wait until state=paused
3. Drop in-memory handles; keep sandboxId in your user binding store
```

Before step 3, confirm the pause landed: `GET /sandboxes/{id}` should show
`state="paused"`. This applies whether you call the REST API directly or through
a supported SDK (including E2B-compat `sandbox.pause()`).

### Resume (`connect_by_id`)

```text
1. GET  /sandboxes/{id}                # optional: check state
2. POST /sandboxes/{id}/connect        # auto-resumes paused → running
3. POST /sandboxes/{id}/timeout {"timeout": 300}   # optional: set interactive idle window
4. Connect envd / data plane (commands, files, VNC)
```

Step 3 is required when you need a **shorter** idle window after resume: `connect(timeout=…)`
only **extends** the existing deadline (e.g. after a 24h freeze, `connect(timeout=300)` still
leaves ~24h). Call `POST /timeout` after connect to replace it with the interactive window you want.

Calling envd or the data plane directly on a **paused** instance often fails
before the control plane has resumed it — commonly a **same-node 504** against
a stale proxy backend, **503 + Retry-After** while pause is in flight, or
**410 Gone** after the sandbox was killed. Always go through
`POST /sandboxes/{id}/connect` first (see [Lifecycle](lifecycle.md)).

### Volume permissions (s3fs)

The S3 volume plugin does not set s3fs `uid`/`gid`/`umask`; guest-visible
ownership depends on s3fs defaults. Use the supported deploy knobs — the
installer generates `volume-s3.conf` for you and rewrites it on upgrade, so
**do not hand-edit that file** (see [S3 Volume](./s3-volume.md)):

```bash
# one-click (.one-click.env)
# install.sh only adds -ouse_path_request_style when this var is empty — keep it for bundled MinIO.
CUBE_S3_S3FS_EXTRA_OPTS='-ouse_path_request_style -ouid=1000 -ogid=1000 -oumask=022'
```

```yaml
# Helm (values.yaml) — only when minio.enabled=false and volumeS3.endpoint / existingSecret is set
volumeS3:
  extraOpts: "-ouse_path_request_style -ouid=1000 -ogid=1000 -oumask=022"
```

With the chart-managed MinIO, `volumeS3.extraOpts` is ignored (the chart hardcodes
S3FS options). Point an external MinIO backend at the same `-ouse_path_request_style`
token.

For [manual plugin deploy](./s3-volume.md#manual-deploy-from-scratch-not-one-click-not-helm)
only, set `S3FS_EXTRA_OPTS` in `volume-s3.conf`. As a fallback, run post-mount
`chown`/`chmod` in your agent bootstrap or template entrypoint. See also
[Host mount permissions](troubleshooting/host-mount-permissions.md) for the
related ownership troubleshooting pattern.

## Operational checklist

- [ ] s3lvol healthy on all compute nodes before `tpl create-from-image --backend s3`
  (on Kubernetes, enabling s3lvol recreates the Big Pod and interrupts sandboxes on that node
  when using **Pod network** — `cubeNode.hostNetwork: false`; see
  [Kubernetes Upgrade](kubernetes/upgrade.md))
- [ ] S3 templates **READY** on all nodes
  (`cubemastercli tpl redo --template-id <id> --failed-only --node <node-ip>` if one node FAILED)
- [ ] Separate buckets: user volumes vs s3lvol snapshot storage
- [ ] Agent binding TTL ≤ paused retention timeout (avoid binding to destroyed IDs)

## See also

- [Lifecycle](lifecycle.md) — `timeout`, `on_timeout`, `connect(timeout=…)`
- [Cross-node snapshots](cross-node-snapshot.md)
- [S3 Volume](./s3-volume.md)
