# Soft-delete tombstone purge

CubeSandbox uses GORM *soft delete* on many tables: a row is "deleted" by
setting its `deleted_at` column, and GORM then hides it from normal queries.
Nothing reclaimed those tombstoned rows, so the tables grew without bound and
stale tombstones kept occupying **unique keys** (the soft-delete + unique-key
trap). The tombstone purger is the scheduled janitor that fixes this.

## What it does

A single shared package, `CubeDB/tombstone`, runs in **both** binaries that
share the database:

- **CubeMaster** — purges `t_cube_*` tombstone tables.
- **CubeOps** — purges `t_agenthub_*` tombstone tables.

Each component registers the purger at startup. On a fixed interval it takes a
cluster-wide advisory lock (so only one HA replica works per tick), then for
each configured table hard-deletes rows whose `deleted_at` is older than the
retention window. The pass is **bounded** (`max_per_pass`) so a large backlog
drains across many ticks instead of one long, lock-holding transaction. The
hard-delete is cross-dialect (the active dialector quotes identifiers) and
**resurrect-race safe** — the `DELETE` re-checks the tombstone predicate, so a
row an application UPSERT resurrects (`deleted_at = NULL`) between select and
delete is never hard-deleted.

## Configuration

Both components expose a `soft_delete_purge` block. All fields are optional;
defaults are safe and the feature is **off by default** (opt-in): the purge is
irreversible, so set `enable: true` explicitly.

```yaml
soft_delete_purge:
  enable: false       # default-off: this purge is irreversible — opt in explicitly
  dry_run: false      # select + log counts but issue no DELETE (safe first rollout)
  retention: 168h     # rows with deleted_at older than now-retention are purged.
                      #   <=0 -> 7d default; values in (0, 1h) clamped up to 1h.
  interval: 1h        # time between passes.
                      #   <=0 -> 1h default; values in (0, 1m) clamped up to 1m.
```

`max_per_pass` (5000) and the batch size (500) are package constants, not
configurable.

## Tables covered

Only tables with a *verified* soft-delete producer and **no** dedicated
reclamation lifecycle are purged. The classification was derived by auditing
every `.Delete()` call site.

**CubeMaster** (always): `t_cube_sandbox_spec`, `t_cube_template_replica`.
**CubeMaster** (default only — see precedence below): `t_cube_instance_info`,
`t_cube_instance_userdata`.
**CubeOps**: `t_agenthub_instance`, `t_agenthub_snapshot`, `t_agenthub_template`.

**Excluded** (owned lifecycle / not soft-delete):

- `t_cube_rootfs_artifact`, `t_cube_artifact_node_placement` — have a
  resurrection dance + the dedicated `artifact_gc`.
- `t_cube_snapshot_runtime_ref` — append-only history; `deleted_at` is never
  written (its growth is a separate problem).
- Hard-delete-only tables (`t_cube_template_definition`,
  `t_cube_template_image_job`, `t_cube_volume`, …).

A leading `deleted_at` index is added (migration `20260731120000`) to every
purge target that lacked one, so the `deleted_at < cutoff` scan is indexed.

## Precedence: `disable_hard_delete` vs `soft_delete_purge`

`common.disable_hard_delete` (CubeMaster) exists so an operator can **retain**
instance records for audit/recovery: when set, `t_cube_instance_info` is
soft-deleted instead of hard-deleted (`t_cube_instance_userdata` is always
soft-deleted).

**`disable_hard_delete` takes precedence over `soft_delete_purge` for instance
records.** When `disable_hard_delete: true`, `t_cube_instance_info` and
`t_cube_instance_userdata` are **exempt** from purge — otherwise the purger
would hard-delete exactly the records the operator chose to keep. The purge
table list is built accordingly in `cubeMasterPurgeTables(disableHardDelete)`.

| `disable_hard_delete` | `soft_delete_purge` | instance records |
|---|---|---|
| `false` (default) | on | `userdata` purged (always soft-deleted); `instance_info` is hard-deleted on the delete path (no tombstone) |
| `false` | off | `userdata` tombstones accumulate (no purge) |
| `true` | on | **retained** — instance tables exempt from purge |
| `true` | off | retained — no purge anyway |

The other CubeMaster tables (`sandbox_spec`, `template_replica`) are always
purged; they are not instance records and have no retain semantics.

## Operational notes

- **Off by default.** The purge is irreversible, so it requires an explicit
  opt-in (`soft_delete_purge.enable: true`); an upgrade must not silently
  hard-delete tombstones that were previously retained forever. Once enabled,
  the first pass reclaims the accumulated backlog incrementally
  (`max_per_pass` per tick).
- **Dry run first.** On a large existing backlog, set `dry_run: true` for one
  interval to observe the would-be-purged counts before enabling deletion.
- **Out-of-band DDL.** Deployments with `CUBE_AUTO_MIGRATION=false` must apply
  the `deleted_at` indexes from migration `20260731120000` out-of-band;
  otherwise the purge is a full table scan.
- **PostgreSQL.** The migration's plain `CREATE INDEX` takes an ACCESS EXCLUSIVE
  lock (blocking writes) while each index builds — acceptable for today's small,
  non-primary PG deployments, but apply the migration during a low-traffic or
  maintenance window on larger PostgreSQL deployments. After a large catch-up
  pass, expect a brief autovacuum spike on the affected tables (standard for
  mass `DELETE`).
