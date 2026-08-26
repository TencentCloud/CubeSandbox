# test/

Tests are in three layers with increasing dependencies. After changing code, run
them in this order -- the earlier ones are faster and need less environment.

| Directory | Contents | Needs | Time |
|------|------|----------|------|
| `integration/` | assertion-style tests per module, linked directly against `libs3bsdev.a` | the `make check` batch needs no credentials and no root | about 1 minute |
| `dataplane/` | end-to-end: target + nvmf + real S3 + real block devices | root, a real bucket and credentials, `nvme-cli`, `fio` | 3-6 minutes each |
| `tools/` | small tools shared by the two layers above and by hand debugging | -- | -- |

## Running everything with one command

```sh
make check           # 23 suites, 928 assertions, about 6 minutes (= test/run_all.sh)
make check-offline   # only the suites needing no credentials and no root: 11, 491 assertions, about 40 seconds
test/run_all.sh --list          # show what would run and what the environment has
test/run_all.sh --no-dataplane  # both integration layers, no dataplane
```

The reason `run_all.sh` exists is that the suites' preconditions had drifted
apart: ten integration tests run anywhere, two need real credentials, ten
dataplane scripts need root + credentials + a writable `/data` + exclusive use
of the machine's nvme stack; and the arguments differ too (seven take
`-e/-b/-r`, six read `s3.cfg` themselves). So "run the tests" had become
"remember twenty-two invocations", and in practice meant running only the two
or three related to whatever had just changed.

A few design decisions, each corresponding to a way a run can "look green while
testing nothing":

- **Skipped is not passed.** Anything that cannot run is reported as SKIP with
  the reason, counted separately in the summary, and **exit code `2` is reserved
  for "everything passed but some suites could not run because of the
  environment"** -- distinct from `0`, because letting them share an exit code is
  exactly how a CI job goes green without having tested anything.
- **Build first.** The dataplane scripts refuse to run against a binary older
  than its sources (`tools/check_binary_fresh.sh`); not building first turns the
  whole second half of a stale tree into skips.
- **Serial.** Every dataplane script assumes exclusive use of this machine:
  fixed RPC socket, nvme connects, and the `bstore.json` and
  `rcow_active_lvols` paths compiled into the module. Two at once do not fail
  cleanly; they interleave.
- **A failure does not stop the run.** A full pass takes minutes, and stopping
  at the first red throws away "did one thing break or ten" -- the most useful
  piece of information.
- **Order from cheap to invasive**, with activation and control at the end: they
  are the most sensitive to leftover global state, so they are also the best
  final word on whether everything before them cleaned up.

One real trap, hit on the second run: **a failing dataplane script deliberately
keeps its S3 prefix and its `bstore.json` entry** (for diagnosis), so the next
run of the same script finds its lvstore already recorded, takes the attach path
instead of create, and fails in ways that have nothing to do with the original
problem ("it did not take the create path", "could not create ctl-a").
`run_all.sh` now prints the cleanup commands right after the failure summary.

## integration/

```sh
make -C test/integration check
```

Runs the ten suites that need no S3 credentials (spawner / thread bounce /
journal / wal / cache / flush / export / statefile / local_dev / checkpoint),
currently 491 assertions. `journal` and `wal` create aio images under `/tmp`,
`cache` and `local_dev` under `/data`; none of them touches the network.
`cache` does not assert "the data comes back" -- a read cache is allowed to
miss -- but that it never returns wrong data and never reuses a slot someone
else is still using: an old uuid must miss, eviction must leave pinned slots
alone, and populate must yield while an old version is being read. It waits for
I/O with **wall time** rather than poll counts; see HANDOFF 5.20 for why.
`cache`'s `[11]`-`[13]` sections are the **resident bitmap**, the most expensive
thing in the whole suite to get wrong: a slot can hold only part of an object,
and the blocks it does not hold keep **the previous tenant's bytes**. So the
three sections pin down, respectively: unfilled ranges must miss (including
reads that only partially overlap), the whole-block/half-block boundary of a
short object's tail, and old ranges being unreadable after a uuid change or slot
reuse. Background in HANDOFF 7.26.
`journal`'s `[12]` section builds another journal with **only 4 blocks** (4x64=256
records wrap it around), because the default 16 MiB journal needs 262144 records
to wrap once -- no real machine can be pushed into that path.
`export` only tests the manifest and does not even start DPDK -- the point is
that "a parseable but wrong manifest must be rejected", because that is the only
failure mode without a sound: a bad uuid still GETs content back, and what you
read is someone else's data. One of its cases uses a real manifest captured from
a live machine, not a constructed one.

`statefile` only tests file I/O, no network, no DPDK. The point is not "write
then read returns the bytes" -- that either works or is red on the spot -- but
two crash shapes: a leftover partial `.tmp` must not leak into reads (simulated
crash before rename; the target file is still the intact old content), and an
over-short file (the classic in-place-write crash remnant) must be rejected
rather than handed to the JSON parser as an empty object. Plus the env-override
policy: absolute paths take effect, relative paths are refused, and the parsed
result is cached (one instance must not split its state across two files).

`local_dev` tests the super block's four rejection paths, each with its exact
errno: bad magic / bad CRC are `-EILSEQ`, an unknown version is `-EPROTO`, and
reopening a dual-bdev layout without its cache bdev is `-EINVAL`. Corruption is
planted by **directly rewriting the first 4 KiB of the backing file** --
corruption is external, not a path the module walks itself. The version and
dual-bdev cases recompute the CRC so only the intended check fires, instead of
"anything broken reports the same error". A trap was stepped in here: `struct
s3_super_block` is about 200 bytes but a disk block is a whole 4 KiB, and using
the struct as a buffer for read/write would overflow the stack.

`checkpoint` tests the rejection paths of loading a checkpoint. It stubs
`s3_head`/`s3_get_range`/`s3_put`/`s3_delete` so the load completes
synchronously, and excludes `s3_client_aws.o` from the link entirely -- but it
still links the bdev/thread libraries, because `s3_chunk_map.o`'s insert/remove
reference `s3_journal_append_*`, which drags in `s3_journal.o` and
`s3_local_dev.o`. Objects are hand-constructed with both CRCs correct, then
corrupted field by field: bad magic / bad version / header CRC / geometry
mismatch / size mismatch / entry CRC, each with its exact errno, while the valid
one loads with the right LSN and generation.

`AWS_INSTALL_DIR` usually does not need to be given: `mk/s3lvol.common.mk`
probes `/usr/local/aws`, `/opt/aws`, `/usr/local` (the criterion is whether
`include/aws/s3/s3_client.h` is present). Passing an explicit empty value still
means "use the system library".

The two that need real S3 run separately:

```sh
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
./test/integration/s3_client_test --endpoint <host> --bucket <name>
./test/integration/s3_bs_dev_test  --endpoint <host> --bucket <name>
```

`s3_client_test` has 1 **xfail**: COS ignores `If-None-Match: *` (HANDOFF 5.5).
It still runs and still prints its result, it just does not count as a failure
-- otherwise this suite would be red forever, and a permanently red test is no
test at all. If it actually passes, that is reported as **XPASS** and listed
separately: it means the backend grew a capability (create-once could then rely
on the server instead of on uuid uniqueness), which is something to follow up,
not a number to bump.

## dataplane/

```sh
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
./test/dataplane/run_dataplane_test.sh -e cos.ap-nanjing.myqcloud.com -b <bucket> -r ap-nanjing
./test/dataplane/run_recovery_test.sh  -e cos.ap-nanjing.myqcloud.com -b <bucket> -r ap-nanjing
./test/dataplane/run_snapshot_test.sh  -e cos.ap-nanjing.myqcloud.com -b <bucket> -r ap-nanjing
./test/dataplane/run_export_test.sh    -e cos.ap-nanjing.myqcloud.com -b <bucket> -r ap-nanjing
./test/dataplane/run_selfimport_test.sh    # reads /data/cubelet/s3.cfg, no arguments
./test/dataplane/run_snapdelete_test.sh    # same
./test/dataplane/run_activation_test.sh    # same
./test/dataplane/run_fs_test.sh            # same; really does mkfs.xfs + mount
./test/dataplane/run_guards_test.sh        # same; the two accidental-deletion guards
./test/dataplane/run_control_test.sh       # same; drives the scripts under scripts/
```

- `run_dataplane_test.sh` -- single process: create -> write -> flush ->
  disconnect/reconnect -> read back from S3 -> fio verify -> resize -> delete
  lvol. Proves the write path and the read path against each other.
- `run_recovery_test.sh` -- three processes: attach after a clean unload, attach
  after SIGKILL, verify both data segments. Also covers the owner-marker contract
  and checkpoint (including the timer trigger). The last step specifically hunts
  the race between unload and the checkpoint poller: several tens of MiB are
  written before unloading to stretch the flusher's drain to seconds, so the
  window after destroy necessarily crosses the 1 s poll period -- it asserts that
  the log shows no checkpoint start after the final "Destroying s3_bs_dev". This
  one reproduced a use-after-free once (HANDOFF 7.16).
- `run_snapshot_test.sh` -- snapshots and clones: the original volume, a
  snapshot and a clone each get their own NVMe namespace, and the three must
  **not affect one another**. The core assertion is that isolation: write A ->
  snapshot -> overwrite the original with B, the snapshot must still read A;
  after cloning the snapshot, the clone initially reads A (read-through to the
  parent), and after writing C to the clone the three read C / A / B. Also
  verifies that writes to a snapshot are refused, and that deleting a snapshot
  with clones is refused (`-EBUSY`).
- `run_export_test.sh` -- cross-lvstore export/import (the sandbox pause/resume).
  Two lvstores in one process: two prefixes in the same bucket, each with its own
  local device -- the only thing they share is S3, and two prefixes are enough to
  test that. The assertions are ordered by importance: the imported volume reads
  the source's data -> writing it does not affect the source -> **after unload +
  attach of the target lvstore, the clone still opens and the data is unchanged**
  (this one tests the imports registry and the esnap callback: the blobstore
  asks for the parent **synchronously** during load) -> decouple, then release
  the export, **and after the release an unload + attach still opens the
  volume** (this one specifically guards that decouple must change the
  **metadata**: swapping only the in-memory back_bs_dev would pass every
  assertion before it and die exactly here). Also verifies that a zero-copy
  export **produces nothing besides the manifest** (sparsity becomes a property
  of the manifest: 8 MiB of data in a 64 MiB volume must name 8 chunks, not 64),
  and that release is refused while the import is running.

  Two paths that are easy to miss are in there too: **a snapshot inherits the
  esnap dependency**, so after snapshotting an imported clone and deleting the
  clone, release must still be refused (otherwise the objects would be deleted
  from under a live snapshot); and the background form of `import --decouple`,
  verifying that import returns before the copy finishes and that the volume
  reads its data with no parent once it is done.

  **Before step 7's restart, a checkpoint is explicitly taken.** Before that,
  every run of this script produced `No checkpoint` plus `ckpt lsn 0` in the
  logs: the checkpoint poller's interval is 60 s while a run takes about 40 s,
  so "replay with a checkpoint" was never tested for the export/import scenario.
  What it tests is concrete: the clone must open during load, and after a
  checkpoint its mapping comes from the checkpoint **plus** the tail of the
  journal, not from one whole journal. The assertion requires `skipped` and
  `ckpt lsn` to be **non-zero at the same time** -- looking at only one of them
  is also satisfied by the empty-journal path.

  **Step 11 specifically tests the clone chain**, because `lvol -> snap1 ->
  snap2` is the real usage pattern and it is where zero-copy breaks most
  silently: snapshots after the first hold only the delta, the parent layers
  still hold most of the data. Three assertions -- (a) still zero-copy; (b) the
  manifest names **more chunks than snap2 owns itself**; (c) both regions read
  back on the B side. **(b) is the real sentinel**: when the chain breaks, (c)
  fails too, but as "import succeeded, reads fine, the inherited half is zero" --
  while (b) points straight at the parent layer not having been resolved.

  **Step 13 prints the latency.** The point of zero-copy is that these numbers
  do not grow with volume size, so they are printed even when everything is
  green -- a handoff that is "actually still copying data" fails no assertion
  and only shows up here, as the number going from milliseconds to seconds. The
  RPC client's python startup overhead (about 37 ms on this machine) is measured
  as a baseline first and subtracted, otherwise it dwarfs the thing being
  measured. It also prints whether this export triggered a drain, the only
  latency component that varies with dirty data.

  `rcow_export_snapshot` now **returns the export uuid immediately; it does not
  mean the export is finished** (the manifest is published asynchronously), so
  the latency is settled only after `rcow_get_snapshot_status` polls to `DONE`
  -- still measuring the full "initiate the export to it being really done".
  `rcow_get_snapshot_status` takes `export_uuid` or `snapshot_name` (exactly
  one; giving both is refused) and on success returns `export_status`
  (INPROGRESS / DONE / NONE) and `deletable` (YES / NO, computed on the spot:
  NO while an export is in progress, when the snapshot is referenced by a
  zero-copy export, or when it has more than one clone).

  The two forms differ only in what "does not exist" means: queried by uuid, an
  export that matches nothing follows the failure path (`bool_value` false,
  `string_value` is `export '<uuid>' does not exist`); queried by snapshot name,
  only a missing snapshot fails, while a snapshot that exists but was never
  exported returns `export_status: NONE` -- which is the reason the snapshot
  form exists: a never-exported snapshot still needs its `deletable` asked, and
  there is no uuid to ask with.

- `run_selfimport_test.sh` -- 28 assertions. `export_snapshot` + `import_lvol`
  inside the same lvstore now degenerates into a **local clone** (the RPC reply's
  `mode` field says whether it is `local_clone` or `esnap`). `export_snapshot`
  was not changed at all: the manifest cannot know at write time whether it will
  be consumed locally or on another machine, so the decision has to sit on the
  import side.

  The degeneration holds only when all three criteria match: `endpoint`+`bucket`
  +`prefix` all equal, the snapshot still exists and is read-only, and the
  `snapshot_uuid` is the same. **The point of the test is not the happy path but
  that the inverse of each criterion falls back to esnap** -- the first version
  used `blob_id` as identity and step [4] caught it immediately cloning a
  recreated same-name volume: the blobstore derives blob ids from the lowest
  free md page, so delete-then-create hands out the same id. Identity is the
  lvol uuid.

  Step [4] judges by **content** rather than just the mode, but it cannot assert
  "reads the exported data": creating a "same name, different blob" requires
  deleting the original snapshot, which frees the objects the REF export
  references. So it asserts "**not** the replaced data" -- reading empty is the
  correct result.

  Why the degeneration is worth it: a local clone's parent is pinned by the
  blobstore (a snapshot with a clone cannot be deleted), while an esnap clone's
  parent is pinned by the export, which can be released and also expires (the
  REF default TTL is only 3600 s). So it is safer, not just faster.

- `run_snapdelete_test.sh` -- 23 assertions. **Deleting a snapshot while its
  source volume is still alive**, and the boundaries of that.

  Key fact: snapshotting turns the original blob into the new snapshot's clone,
  and snapshotting again inserts into that chain, so for an lvol L with
  s0/s1/s2 the shape is `s0 <- s1 <- s2 <- L`, and **every snapshot's
  clone_count is exactly 1**. `bs_is_blob_deletable()` (`blobstore.c:8704`)
  refuses unconditionally only above **>1**; at exactly 1 it **merges the
  snapshot into that clone**, and `spdk_lvol_destroy()` cooperates by looking up
  the clone's lvol first so it can fix it up. This suite exists because the
  pre-check once had the threshold at `>0`, making the most ordinary "delete an
  old snapshot" report EBUSY, and the error was mistaken for a blobstore
  limitation (see HANDOFF 7.22).

  **It asserts data, not return codes**, because deletion is a merge: a test
  that only looks at status codes is blind to "returns 0 but dropped the merged
  cluster", and that fault would only surface weeks later. The layout is
  deliberately arranged so a wrong merge cannot look right -- the volume is four
  4 MiB regions, region N is written only before snapshot N, so each region
  exists in only one link of the chain; region 0 has to walk the whole chain to
  be read, so a merge that drops a cluster breaks region 0/1 first, not the
  region 3 the volume itself wrote. All four regions are re-read after every
  deletion.

  Also covered: a surviving snapshot still reads the history it captured
  (regions 0-2 present, region 3 zero); a snapshot with 2 clones must be
  refused, and **the refusal happens before any unwinding** (data unchanged +
  volume still writable); and the lvstore can still unload -- that is the whole
  reason the pre-check exists, a refusal that comes too late leaves an lvol that
  cannot be closed.

- `run_fs_test.sh` -- the only suite that uses a volume as a **disk**:
  `rcow_create_lvol` -> `rcow_active_bdev` -> `mkfs.xfs` -> mount, write 47
  files -> freeze, snapshot -> mount the snapshot and compare md5 per file. The
  other suites speak in dd/fio/blkdiscard (aligned, direct, known offsets); a
  filesystem is none of those: small unaligned metadata writes, hundreds of
  segments per request, and **an NVMe FLUSH per journal commit**. The last one
  is not hypothetical -- `SPDK_BDEV_IO_TYPE_FLUSH` was originally implemented as
  `spdk_blob_sync_md()`, and the host's `mkfs.xfs` aborted the target (HANDOFF
  7.17) while 15 suites / 643 assertions were green, because none of them used a
  volume the way a block disk is used. Section [12] now greps for
  `blob_verify_md_op` by name.

  The core assertion is at **file granularity, not block granularity**: the
  file set and contents read from the frozen snapshot mounted elsewhere must be
  **exactly identical** to the original volume at the freeze instant, with none
  of the later changes visible. "Exactly identical" is a `diff` of two
  manifests (each file's md5, sorted), not a spot check; and the manifests are
  asserted non-empty first -- comparing two empty directories also succeeds and
  proves nothing. The changes are deliberately in three classes because they
  reach the snapshot differently: creating a file occupies clusters the snapshot
  never had; deleting frees clusters the snapshot still references; and
  **in-place overwrite** is the one that must go through CoW and must not modify
  a shared cluster.

  The other half of the value is **how to mount the snapshot**, which was
  measured and guessed wrong twice: snapshots refuse writes at the bdev layer,
  but nvmf has no place to report that (`blockdev --getro` is always 0;
  `run_snapshot_test.sh` section [8] pins that value from the other side). XFS
  decides whether it may write during mount by looking at the **block device's**
  read-only flag -- `xlog_clear_stale_blocks()` in `xlog_find_tail()` writes,
  and only skips it when `xfs_readonly_buftarg()` is true. So with the default
  flags that write always happens and is always refused, and mount(8) reports it
  as the deeply misleading `can't read superblock` (kernel side: `log recovery
  write I/O error`). **Note "recovery" has nothing to do with whether the log is
  dirty -- it happens even when the log is clean**, which is where the first two
  versions fell in. The three-row conclusion is asserted, and both failures
  match specific kernel messages (asserting only "it failed" is worthless -- any
  unrelated fault fails too):

  | snapshot taken from | `blockdev --setro` | `mount -o ro,nouuid` |
  | --- | --- | --- |
  | unmounted filesystem | no | EIO, `can't read superblock` |
  | unmounted filesystem | yes | mounts, `Ending clean mount` |
  | mounted, frozen filesystem | yes | honestly refused: `recovery required on read-only device`, needs `-o norecovery` |

  So the operational rule is: **`blockdev --setro` before mounting a
  snapshot**; when the snapshot comes from a live filesystem, add
  `-o norecovery` -- `xfs_freeze` does not cover the log (`xfs_quiesce_attr()`
  only forces the log and reclaims inodes; it does not write an unmount
  record), and a read-only device cannot replay it. Both cases need `nouuid`:
  the snapshot has the same UUID as the original volume, and XFS refuses a
  duplicate mount while the original is mounted. `xfs_repair -n` is run only on
  the clean-log snapshot -- files can all read correctly while the metadata is
  still inconsistent, which is the fault that surfaces weeks later; with a dirty
  log `-n` refuses to replay and reports a free-block count it cannot reconcile,
  which is complaining about the log, not the snapshot.

- `run_guards_test.sh` -- 24 assertions, two guards against "creating a live
  lvstore out from under", both added after a near-miss.

  **Guard 1: the state file paths became configuration.** `/var/tmp/bstore.json`
  and `/var/tmp/rcow_active_lvols` were compile-time constants shared by the
  whole machine -- including the test suites, two of which `rm -f` the active
  registry in their cleanup. Not hypothetical: it really deleted a running
  instance's registry once. Now the module resolves them through
  `S3LVOL_ACTIVE_FILE` / `S3LVOL_BSTORE_FILE` (`vbdev_s3lvol_statefile.c`), and
  `rcow_common.sh` maps and exports them from the `RCOW_*` variables. This suite
  asserts: the override takes effect (entries written to the private path,
  **nothing** written to `/var/tmp`), the historical defaults still hold when
  the variables are unset (production relies on this), and after the run the
  host's two files are **byte-for-byte unchanged**.

  Plus one more: **the one-blobstore-per-node policy does not fail open.** It
  used to be written as `pick_one() != NULL`, and since `pick_one()` returns
  NULL for both 0 and >1 matches, it only blocked at exactly one and let two
  through -- and two is reachable (`attach` is deliberately unrestricted;
  moving volumes across lvstores needs both loaded). It now counts. **Note: the
  restriction is on create, not on how many are loaded at once -- do not add the
  same check to `attach`**, `run_export_test.sh` needs two loaded at once to
  build its clone chain.

  **Guard 2: create refuses a prefix that already has a blobstore.** The
  lvstore name is the key prefix in the bucket (`prefix = lvs_name` in
  `s3_bs_dev_create`), and under the prefix `meta/checkpoint` has a fixed name,
  so building another blobstore on top would overwrite the previous one's chunk
  map -- silently, because data objects are uuid-named and never collide; they
  just stop being reachable. The owner marker does not stop this: it answers
  "is anyone writing right now", and **a clean unload releases it**, so a
  properly shut-down lvstore leaves no trace there. Two ordinary entry points:
  the local `bstore.json` is lost and `rcow_start.sh` cannot see the prefix is
  in use and falls back to create; or two nodes derive the same name while the
  first one shut down cleanly. Create now HEADs `<prefix>/meta/checkpoint`
  first.

  What this suite pins is the **boundary** of that guard: refused when a
  checkpoint exists; **still refused after a clean unload** (exactly what the
  owner marker cannot see, and the whole reason the guard exists); `force=true`
  lets it through (taking the prefix over must stay possible, and the log says
  the check was skipped); **attach is unaffected** -- otherwise "create was
  refused" would be indistinguishable from "this lvstore is dead", which is the
  most common secondary failure a guard like this causes. The window the guard
  cannot see is written in the code comment: a clean shutdown after create but
  before any checkpoint leaves a prefix holding only uuid-named objects, which
  `s3_list_objects()` still cannot list (`-ENOTSUP`).

- `run_control_test.sh` -- drives `scripts/rcow_{start,stop,recovery}.sh`, so it
  tests **order** rather than individual RPCs. 48 assertions in nine sections:
  one start/stop round; hot-plugging a namespace after connect (AEN; the host
  has to discover it on its own); after a clean restart the volume returns to
  the same subsystem/nsid with unchanged data; **removing a namespace while a
  cold read is in flight**; after SIGKILL, recovery itself confirms the owner
  marker is dead before force-attaching; a registry entry pointing at a volume
  that no longer exists being refused and removed from the registry; and
  "process loaded the registry but mounted no namespace" being reported as
  inconsistent rather than healthy.

  **Section [5b] (deactivate with reads in flight) corresponds to a real
  crash**: in `spdk_nvmf_subsystem_pause()`, nsid **0 means "pause no
  namespace at all"** (the `nvmf.h` wording), and the module used to pass 0 --
  so nothing was quiesced before remove_ns, and when resume released the bdev
  channel the reads were still in flight, and the bdev layer's
  `assert(TAILQ_EMPTY(&ch->io_submitted))` aborted the process. Deactivating an
  idle volume never hit it -- what first hit it was udev probing the new device.
  So this section deliberately runs a 32 MiB cold read (the data is in S3, the
  overlay cannot answer it, so I/O is guaranteed in flight).

  The last section is simultaneously the regression sentinel for another crash:
  `spdk_json_next()` returns **NULL** at the end of an object, and the registry
  parse loop tested `it < end` (NULL satisfies it) before dereferencing, so the
  **first `rcow_get_bdev` after a restart segfaulted every time**. No other path
  reaches that code -- the registry is lazy-loaded (first activation-style RPC
  reads it), and the other tests either start without the file or (like replay)
  delete it before the first call.

  Its isolation differs from the other scripts: the lvstore name, the WAL image
  and the run directory are test-specific, but the `bstore.json` and
  `rcow_active_lvols` paths are **compiled into the module**, shared by the
  whole machine. So the script refuses to run if `rcow_active_lvols` is
  non-empty at the start (something on this machine is actively serving
  volumes), and only deletes its own entries at the end. The run directory is
  deleted whole at the start of every round -- the target log is append-only and
  the final check greps it: an assert left by the previous round would make the
  next round red while **the round that actually produced it shows all green**,
  wrong on both ends.

Common conventions of the dataplane scripts (seven take `-e/-b/-r`
arguments, the other six read `/data/cubelet/s3.cfg` themselves):

| Environment variable | Effect |
|----------|------|
| `S3LVOL_KEEP_S3` | keep the S3 objects even on success |
| `S3LVOL_KEEP_LOGS` | keep the target logs even on success |
| `S3LVOL_TGT_CPUMASK` | the target's `-m`, default `0x3` |
| `S3LVOL_WAL_FILE` / `S3LVOL_WAL_MB` / `S3LVOL_JOURNAL_MB` | location and layout of the local device image |
| `S3LVOL_SRC_WAL_FILE` / `S3LVOL_DST_WAL_FILE` | only `run_export_test.sh`: the two lvstores' own device images |
| `S3LVOL_CKPT_INTERVAL_SEC` | checkpoint interval for the recovery test's third attach, default 5 s |

**On success the scripts clean up their S3 objects and local images themselves;
on failure everything is kept**, and the deletion commands are printed -- on a
failure those objects and WAL images are the only evidence, so do not delete
them in a hurry.

**The bucket must be dedicated.** Cleanup deletes by prefix (`<lvs_name>/`), and
`-p ''` can delete a whole bucket; do not point it at a bucket with other data.

## tools/

### `s3lvol_rpc.py` -- send one JSON-RPC

s3lvol's RPC methods are defined in this repository, and SPDK's `scripts/rpc.py`
does not know them, so custom methods always go through this. On success the
`result` is printed to stdout; on failure the `error` goes to stderr and the
exit status is 1.

```sh
test/tools/s3lvol_rpc.py rcow_get_lvstores
test/tools/s3lvol_rpc.py rcow_checkpoint_lvstore '{"lvs_name":"dpvs"}'
test/tools/s3lvol_rpc.py --sock /var/run/s3lvol.sock --timeout 60 bdev_get_bdevs
```

Two modes take no method of their own:

```sh
test/tools/s3lvol_rpc.py --ls              # rcow_get_lvstores as a table,
                                           # incl. the DEL / PEND columns
test/tools/s3lvol_rpc.py --retry-pending   # re-issue the snapshot deletes that
                                           # were refused and are now deletable
```

`--retry-pending` acts on the pending-delete marks a refused snapshot delete
leaves behind (`PEND` in `--ls`). The marks are in the target's memory only,
nothing retries them automatically, and there is no cancel -- see
[Retrying a refused snapshot delete](../README.md#retrying-a-refused-snapshot-delete---retry-pending)
in the main README for the full contract.

Especially useful when debugging a hung target: a process a test script left
behind can be asked for its state directly.

### `s3_prefix_rm.py` -- delete S3 objects by prefix
`rcow_unload_lvstore` deliberately does not delete objects (the next attach
needs them), so every test round leaves some behind; a crashed round also
leaves an owner marker, which makes the **next create report -EBUSY directly**.
This script is the cleanup.

Hand-written SigV4, standard library only -- the test machines have neither
aws-cli nor boto3, and the situations that need cleanup are often exactly the
ones with no target running (the process already crashed). Credentials are read
from the environment only.

```sh
test/tools/s3_prefix_rm.py -e cos.ap-nanjing.myqcloud.com -b <bucket> -r ap-nanjing -p rcvs/ --list
test/tools/s3_prefix_rm.py -e cos.ap-nanjing.myqcloud.com -b <bucket> -r ap-nanjing -p rcvs/
test/tools/s3_prefix_rm.py -e <minio-host> -b <bucket> --path-style -p ''
```

### `check_binary_fresh.sh` -- refuse to run tests against a stale binary

The five dataplane scripts that start a target call it first
(dataplane/recovery/snapshot/export/activation). It fails immediately if the
sources are newer than `app/s3lvol_tgt/s3lvol_tgt`.

The check exists because it really happened once: the top-level `Makefile`'s
default target did not include `app/` then, so `make` rebuilt only the
libraries, the binary stayed put, and a whole round of 39/39 + 32/32 + 32/32 +
55/55 went green -- against a binary from three hours earlier that did not even
contain the fix being verified. Nothing in a green run shows that; it was found
only through a function name that had been renamed long before in the logs.

**Green from a stale binary is worse than red**: it is evidence pointing the
wrong way, and people will take it for a conclusion. The `Makefile` is fixed,
but there is more than one road to that state (`make shared`, an interrupted
build, rebuilding a different tree), so the check lives on the test side -- the
side where the wrong conclusion would be produced.

It compares mtimes only. If you genuinely need the old binary (say, bisecting),
`S3LVOL_SKIP_FRESH_CHECK=1`.

### `check_layering.sh` -- the three layering hard rules

The integration tests assume `lib/` links on its own; this check enforces the
three layering rules (HANDOFF section 8) that keep it that way. It runs as part
of `make check` / `make check-rules`.

## Known traps when debugging a failure

- **The target will not start, reports `Cannot create lock on core 0`**: the
  previous round's target is still alive. `pkill -f s3lvol_tgt`.
- **`pgrep -x s3lvol_tgt` does not find a target that is clearly running**:
  once SPDK starts its reactors it renames the main thread `reactor_0`, so
  `/proc/<pid>/comm` has no `s3lvol_tgt` in it. Match against `/proc/<pid>/exe`
  instead (`rcow_target_instances()` does). This is not trivia: `rcow_start.sh`
  used `pgrep -x` to decide "is a target already running", the check silently
  stopped working, a second target was let in, and the new process took over
  `/var/run/s3lvol.sock` (the RPC server unlinks it before binding) -- the first
  process stayed alive, holding the WAL image and unreachable.
- **create reports `-EBUSY`**: S3 still holds someone else's owner marker
  (usually the previous round that was killed). After confirming no process is
  running, clear it with `s3_prefix_rm.py`, or attach with `force: true`.
  `scripts/rcow_start.sh` auto-confirms and retries when the marker names this
  node and that pid is not running.
- **Changed a header but the behaviour looks unchanged**: it used to be possible
  (the Makefile did not track header dependencies, so the same struct had
  different layouts in different `.o` files). `-MMD -MP` has fixed it; if still
  in doubt, `make clean` once rules it out.
- **Changed code but the behaviour looks unchanged, and the script reports a
  stale binary**: do the `make` it tells you to. Note the top-level default
  target now builds the binary too; `make shared` builds only the libraries and
  does not update `s3lvol_tgt`.
