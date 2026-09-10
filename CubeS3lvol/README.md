# s3lvol

Block storage on S3: an NVMe/TCP target that keeps a volume's data in object
storage, with the local disk used only for the WAL and the metadata journal.

This directory is a **self-contained release package**: `bin/s3lvol_tgt` links
SPDK, DPDK and AWS CRT statically, so no SPDK source tree is needed on the
machine. Build information is in `VERSION`.

## Building a release package

The package is produced from the source checkout with `make_release.sh`:

```sh
./make_release.sh                        # build, verify, tar
./make_release.sh --version 1.2.0        # default is a git describe
./make_release.sh --outdir /tmp/rel      # default is ./release
./make_release.sh --no-tar               # leave the directory, skip the tarball
./make_release.sh --skip-build           # package whatever is already built
./make_release.sh --skip-smoke           # package, skip the runtime smoke tests
```

What it produces, under `release/s3lvol-<version>/` (plus a tarball unless
`--no-tar`):

```text
s3lvol-<version>/
├── bin/s3lvol_tgt
├── scripts/
│   ├── rcow_start.sh rcow_stop.sh rcow_recovery.sh rcow_common.sh
│   ├── rcow_purge.sh  s3lvol_rpc.py  s3_prefix_rm.py
│   ├── rpc.py         # this repo's launcher (3.8 argparse shim)
│   ├── rpc_compat.py  # BooleanOptionalAction backfill for Python 3.8
│   ├── spdk_rpc.py    # SPDK's rpc.py, unmodified
│   └── python/spdk/   # what SPDK rpc.py imports
├── VERSION            # what this package is and what it was built from
└── README.md
```

Notes for a fresh build machine:

- The script needs an SPDK checkout at `deps/spdk` or `../spdk` (or `SPDK_ROOT`
  in the environment) to copy `spdk_rpc.py` and its library from; it refuses to
  run without one. `setup_dep.sh` arranges this. `scripts/rpc.py` in the package
  is this repo's launcher: it backfills `argparse.BooleanOptionalAction` so the
  unmodified SPDK client can run on Python 3.8 (Ubuntu 20.04).
- Layout detection and `rpc.py --help` always run. A separate **EAL smoke**
  starts `bin/s3lvol_tgt` to prove DPDK initialises. On a restricted CI node that
  cannot run DPDK, pass `--skip-smoke` to skip only that EAL check.
- Both build types are recorded in `VERSION`. The development defaults are not a
  release build; for something meant to be deployed:

```sh
AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --force aws
make S3LVOL_BUILD_TYPE=release && ./make_release.sh
```

## What the machine needs first

**System packages**

```sh
# CentOS / TencentOS
yum install -y nvme-cli python3 libuuid libaio numactl-libs

# Debian / Ubuntu
apt install -y nvme-cli python3 libuuid1 libaio1 libnuma1
```

`ldd bin/s3lvol_tgt` lists the exact system libraries required. OpenSSL is
linked statically into the binary (the Ubuntu 20.04 builder's 1.1 `.a` files),
so the target does not need `libssl.so.1.1` or `compat-openssl11`.

**CPU ISA (x86_64).** Release binaries are built for **Haswell / AVX2**, not
the packager's CPU. `setup_dep.sh` passes `--target-arch=haswell` to SPDK
(aarch64: `armv8.2-a+crypto`) so DPDK does not bake in VAES / VPCLMULQDQ from
`-march=native`. Override with `SPDK_TARGET_ARCH` or replace the whole
`./configure` line with `SPDK_CONFIGURE_ARGS`. `make_release.sh` refuses a
`native` tree. The host needs `avx2` in `/proc/cpuinfo`.

**Two pieces of per-machine state**, neither of them in the package, because they
cannot be copied from another machine:

| Path | What it is | Why it is not packaged |
|---|---|---|
| `/data/cubelet/s3.cfg` | S3 endpoint, region, bucket, credentials | Holds secrets, and differs per machine |
| `/data/cubelet/rcow/wal_bdev.img` | Backing file for the WAL and metadata journal | **Its size fixes the journal/WAL layout**; a copied image is an lvstore whose log belongs to another node |

Create the WAL image, sized to leave room for `RCOW_JOURNAL_MB` +
`RCOW_WAL_MB` + `RCOW_CACHE_MB`. The defaults are 1024 + 32768 + 490496 MiB
(512 GiB total: the journal and WAL segments plus the chunk cache, which is
whatever is left after the WAL ring). The one-click install (`install.sh`)
creates it with exactly that default size, so a deployed node gets it
automatically:

```sh
mkdir -p /data/cubelet/rcow
truncate -s 512G /data/cubelet/rcow/wal_bdev.img
```

The s3lvol start/stop scripts **deliberately do not** create it for you: an
empty file where the old one used to be looks to the attach path like "an
lvstore with nothing to replay", which is silent data loss. The size is a
per-node contract: it fixes the journal/WAL layout, and a node that ever
starts with a different size than it was first created with has already
forfeited its previous state.

## Start and stop

```sh
scripts/rcow_start.sh          # start target, create/attach lvstore, export nvmf, connect host
scripts/rcow_stop.sh           # reverse order: disconnect, flush, unload, stop process
scripts/rcow_recovery.sh       # use this after an unclean exit
scripts/rcow_purge.sh          # delete the whole lvstore back to a clean state (irreversible)
```

`rcow_start.sh` is idempotent: if already running it tells you and exits rather
than starting a second instance.

Credentials are read from `s3.cfg` and passed on **only through the target
process's environment** — never on the command line, never into logs.

one-click writes this file from `CUBE_S3_*` when `ONE_CLICK_ENABLE_S3LVOL=1`.
If you write it by hand, values must be double-quoted and `endpoint` must
**not** include a scheme (`http://` / `https://` become `no_tls` instead):

```
access_key_id="..."
secret_access_key="..."
endpoint="10.0.0.11:9000"
region="us-east-1"
buckets=["cube-s3lvol"]
path_style="true"
no_tls="true"
```

There is no fallback from the old `/data/cubelet/cos.cfg` name or field
names (`secretid` / `cos_endpoint` / …). Rename the file and switch keys.

## Environment variables

Everything in the scripts can be overridden from the environment. The commonly
used ones:

| Variable | Default | Meaning |
|---|---|---|
| `RCOW_S3_CFG` | `/data/cubelet/s3.cfg` | |
| `RCOW_WAL_IMG` | `/data/cubelet/rcow/wal_bdev.img` | |
| `RCOW_LVS_NAME` | derived `rcow-<identity-hash>`; a pre-existing `rcow` entry is honoured | lvstore name, also the prefix in S3 |
| `RCOW_CAPACITY_GB` | `16384` | used only at first create; thin, unused space costs nothing |
| `RCOW_CACHE_MB` | `490496` | chunk cache on the WAL image; only matters before the first start |
| `RCOW_LISTEN_ADDR` / `RCOW_LISTEN_PORT` | `127.0.0.1` / `4420` | |
| `RCOW_TGT_CPUMASK` | empty (automatic) | exact SPDK mask/list override; automatic mode maps two reactors to the two highest allowed CPU IDs below `CPU_SETSIZE` |
| `RCOW_NO_HUGE` | `1` | no hugepages by default, a deliberate choice |
| `RCOW_RPC_SOCK` | `/var/run/s3lvol.sock` | |

### Node identity and the owner marker

An lvstore's S3 prefix carries a `<lvs>/meta/owner` marker, written on attach
and removed on unload. After a crash, `rcow_recovery.sh` force-takes the
marker only when it names **this hostname** and the recorded pid is **not
running locally**. That is safe against a different node, but it trusts the
hostname: two machines with the same hostname (containers, cloned disks)
each consider the other's marker stale and force-take a volume the other is
still serving, which can corrupt data.

The lvstore name is derived from the same hostname (`rcow-<identity-hash>`),
so duplicate hostnames also collide on the S3 prefix itself. The identity
string is `hostname -s` for a DNS name (`worker1.example.com` → `worker1`),
so an existing prefix survives a domain-suffix change. An address is not an
FQDN: `hostname -s` of `192.0.2.48` is `192`, which would give every node
in a /8 the same prefix. IPv4, IPv6, and a purely numeric short name of a
dotted hostname (`192.0.2.48.internal`) are hashed in full.

Changing the identity string changes the prefix. A node whose hostname is
already an IP, and that already created a store under the old `hostname -s`
hash, looks like a new machine on upgrade (create formats the WAL; the old
S3 prefix is orphaned unless you pin `RCOW_LVS_NAME` to the old name).

When nodes have a real unique id (an inventory id, a Kubernetes node name,
an instance id), pin `RCOW_LVS_NAME` to `rcow-<hash of that full id>`
instead of relying on `hostname -s`, and never let two nodes share a
hostname. The Helm sidecar does this automatically from the full
`spec.nodeName`.

`RCOW_NO_HUGE=1` is a **choice, not a concession**: nothing on this data path
needs DMA (the local disk goes through `bdev_aio` read/write syscalls, the
network through nvmf_tcp sockets, and S3 traffic lives in memory CRT mallocs
itself), and the only capability `--no-huge` drops is "DPDK resolving IOVAs for
memory". Setting it to `0` switches to hugepages, in which case the script fails
outright rather than silently degrading if `nr_hugepages=0`.

## Operations

All of this project's RPCs go through `scripts/s3lvol_rpc.py` (raw JSON-RPC);
SPDK's own go through `scripts/rpc.py` (the launcher, which runs `spdk_rpc.py`).
The two are not interchangeable: SPDK's client only knows methods it has a
Python wrapper for, while `rcow_*` are registered by this project and it refuses
them directly.

**Volume lifecycle** (the first lvstore is created by `rcow_start.sh`):

```sh
scripts/s3lvol_rpc.py rcow_create_lvol     '{"lvol_name":"vol0","size_gib":100}'
scripts/s3lvol_rpc.py rcow_create_snapshot '{"lvol_name":"vol0","snapshot_name":"snap0"}'
scripts/s3lvol_rpc.py rcow_create_clone    '{"snapshot_name":"snap0","clone_name":"clone0"}'
scripts/s3lvol_rpc.py rcow_resize_lvol     '{"lvol_name":"vol0","size_gib":200}'
scripts/s3lvol_rpc.py rcow_delete_lvol     '{"lvol_name":"clone0"}'
scripts/s3lvol_rpc.py rcow_get_lvstores    # list all lvstores and lvols
```

(`rcow_pending_load_hold` exists only so tests can park pending-delete registry
HEAD/GET around unload; it is not an operations RPC.)

The parameter names are deliberately chosen; the easy mistakes to make when
copying them:

- `create_lvol` has **no `lvs_name`** — it operates on the single lvstore that
  exists.
- `create_snapshot` takes `lvol_name` as the source and `snapshot_name` as the
  target; snapshots are read-only. If the source is still decoupling from an
  import, the snapshot keeps that external parent and the reply includes
  `decouple_cancelled: true`.
- `create_clone` takes `snapshot_name` (which **must be a read-only snapshot**)
  as the source and `clone_name` as the target.
- `resize_lvol` can only grow, never shrink.
- `delete_lvol` also clears the volume's activation record, so the next restart
  does not try to restore it. A snapshot that is still pinned is recorded as an
  intent rather than carried out. When the blocker is one the poller can finish
  on its own, the reply is the usual success envelope plus `deferred: true`. A
  live or still-unknown lease still returns EBUSY; a stale lease-aware export
  and a pre-lease (`export_legacy`) export are released internally by the delete.
  `rcow_cancel_pending_delete` withdraws it.

**Exporting a snapshot** (cross-node transfer):

```sh
scripts/s3lvol_rpc.py rcow_export_snapshot '{"snapshot_name":"snap0"}'
```

`ttl_sec` is accepted for compatibility but ignored. New reference manifests use
`expires_at: 0`; their lifetime follows the source snapshot.

Rolling upgrade: bring **importers** to this build before sources that publish
`expires_at: 0`. An older importer skips writing a lease for that shape, so a
source-first upgrade would treat a still-reading node as absent after the
grace window. Same-version importers renew every 20 s. A confirmed miss younger
than 60 s still pins — that covers a first lease PUT that arrives in the first
minute after publish (or after a live lease disappears). Once the export has
been idle longer than that, the miss is already older than the grace: a newly
admitted importer is unprotected until the source's next lease poll (20 s),
when the source first sees the PUT. The delete path uses that cached
poll; it does not HEAD S3 itself.

`rcow_export_snapshot` returns an **export uuid immediately**; the export itself
keeps running in the background. The uuid is not a completion signal — poll it:

```sh
scripts/s3lvol_rpc.py rcow_get_snapshot_status '{"export_uuid":"<uuid>"}'
# -> {"export_status":"INPROGRESS","deletable":"NO","delete_pending":false}
#    ... then, eventually
# -> {"export_status":"DONE","deletable":"YES","delete_pending":false}
#    (when no live/unknown reader or other blocker remains)
```

`rcow_get_snapshot_status` returns three fields:

- `export_status`: `INPROGRESS` / `DONE` / `NONE`.
- `deletable`: `YES` / `NO`, computed on the spot each time from the cached
  lease poll (the query itself does not HEAD S3) and mirroring what
  `rcow_delete_lvol` would actually refuse: a snapshot is **not
  deletable** while an export is in progress, has a live lease, has a miss
  younger than the source grace (`S3LVOL_LEASE_MIN_GRACE_SEC`), or is read by a
  local esnap clone; also while a decouple runs, while active over NVMf, or when
  it has more than one clone. An idle lease-aware ref export, and a pre-lease
  legacy export, are released internally by the delete — `deletable: YES` in
  those cases is the signal that an operator delete will revoke the export even
  if a pre-lease reader cannot be observed.
- `delete_pending`: whether a delete was asked for and refused — see
  [Retrying a refused snapshot delete](#retrying-a-refused-snapshot-delete---retry-pending).
  Only meaningful when the query names a `snapshot_name`; queried by
  `export_uuid` it is always `false`, because an export names a snapshot but is
  not one.

It accepts exactly one of two keys: `export_uuid`, or `snapshot_name` for a
snapshot that may never have been exported (such a snapshot reports `NONE`).
Querying by an `export_uuid` that matches no export is an error; a snapshot that
exists but was never exported is reported as `NONE`, which is how an export that
failed asynchronously surfaces.

For the volume to be mounted by the host it still has to be activated over nvmf,
then asked for its device path; to stop serving it, deactivate it:

```sh
scripts/s3lvol_rpc.py rcow_active_bdev   '{"device_name":"vol0"}'
scripts/s3lvol_rpc.py rcow_get_bdev      '{"device_name":"vol0"}'   # returns device_path
scripts/s3lvol_rpc.py rcow_deactive_bdev '{"device_name":"vol0"}'   # detach it from its subsystem again
```

`rcow_active_bdev` answers as soon as the namespace is attached on the target,
which is before the host has processed the notification — so the device does not
exist yet at that point. `rcow_get_bdev` closes that gap: it waits (up to 5 s by
default) until the path resolves **and its `/dev` node exists**, so a non-empty
`device_path` is a path that can be opened. Measured without the wait, 4 out of
5 activations returned an empty path when asked immediately.

No retry loop is needed in the caller, therefore. `{"wait_ms":0}` asks for the
old behaviour — answer with whatever resolves right now — which is what a pure
status query wants; any other value sets the timeout. On timeout the reply is an
empty `device_path` rather than an error, so a caller that does poll needs no
change.

`device_name` in `rcow_active_bdev` / `rcow_deactive_bdev` / `rcow_get_bdev` is
the **lvol name**, not the `<lvs>/<lvol>` bdev name. `rcow_deactive_bdev` is
idempotent: deactivating a volume that is not active succeeds, because "not
active" is the desired end state.

Logs default to `/data/log/rcow/s3lvol_tgt.log` (overridable with `RCOW_LOG`, or
`RCOW_LOG_DIR`). The CRT log level is set with
`S3LVOL_CRT_LOG_LEVEL=trace|debug|info|warn|error|none`; `trace` prints every
request header and the canonical string of the SigV4 signature.

`[ERROR] ... response status=404` is **normal**: s3lvol uses a GET's 404 to
determine whether an object exists.

## Environment Requirements

### NVMe kernel driver / NVMe-oF support

s3lvol exposes volumes over NVMe-oF (nvmf-tcp): the SPDK-backed `s3lvol_tgt`
is the user-space *target*, but the *host* side that attaches the volumes needs
the kernel NVMe fabric support:

- the `nvme-fabrics` and `nvme-tcp` kernel modules must be loadable/loaded
  (`modprobe nvme-tcp`), so that `nvme connect` produces a block device under
  `/dev/nvme*n*`;
- the kernel must be built with `CONFIG_NVME_TCP` / `CONFIG_NVME_FABRICS`
  (a 5.x+ distro kernel normally is);
- the target itself does **not** need the in-kernel `nvmet`: SPDK's user-space
  nvmf target provides it.

The dataplane regression suite (`make check`) actually attaches loopback
NVMe-oF devices, so it only runs on a host with this support (and it needs
`root`).

### nvme-cli

The `nvme` command-line tool (package `nvme-cli`) is a hard dependency of the
test scripts: `nvme connect/disconnect/list` are used to attach and detach
volumes. Install it with:

```sh
# Debian/Ubuntu
apt-get install nvme-cli
# RHEL/CentOS
yum install nvme-cli
```

### Preflight: is the NVMe fabric stack loaded?

Before running the suite (or attaching any volume), confirm the host side of
the fabric is actually available:

```sh
# 1. kernel NVMe-over-TCP driver (produces /dev/nvme*n* block devices)
modprobe nvme-tcp                 # idempotent; fails if the module is missing
lsmod | grep nvme                 # should list nvme_tcp / nvme_fabrics

# 2. nvme-cli (used by the test scripts / attach tooling)
which nvme                        # must resolve, e.g. /usr/sbin/nvme
```

If `modprobe nvme-tcp` fails, the kernel was built without NVMe-over-TCP
(`CONFIG_NVME_TCP` missing), the host cannot attach any volume, and the
dataplane regression suite cannot run here — check the kernel config before
proceeding.

### Object store

A COS- or MinIO-compatible bucket is required at runtime; the credentials,
endpoint and bucket are read from the COS config file (see the config section).
For a local MinIO the config must also set `path_style = "true"` and
`no_tls = "true"` so the S3 client talks plain HTTP with path-style URLs.

### Busy-polling threads and CPU affinity

`s3lvol_tgt` runs SPDK reactor threads in busy-poll mode: they spin at 100% of
the cores they are pinned to and never sleep. Unless an explicit SPDK reactor
mask or lcore map is supplied (`-m`, `--lcores`, or `RCOW_TGT_CPUMASK`), the
target maps two dense SPDK lcores to the two highest-numbered logical CPUs below
`CPU_SETSIZE` in its effective scheduler affinity at startup. Higher CPU IDs can
still host background/CRT threads, but DPDK cannot select them as reactors.
That affinity already reflects `taskset`, cpuset/cgroup, and container runtime
restrictions; if only one supported CPU is available, the target starts one
reactor, and if none is available startup fails.

Background/CRT threads are restricted to the startup affinity minus the reactor
CPUs. If the reactors consume every allowed CPU, they instead share the full
affinity and the target logs a warning; leave at least one additional CPU allowed
when reactor/background separation is required.

The selected CPUs are fully consumed by the target, but selecting them does
**not** isolate them from other workloads or guarantee separate physical cores;
on SMT hosts the two highest logical CPUs may be sibling threads on one core.
Keep other application/business processes off them with `taskset`/`numactl`
(or a cpuset/cgroup), or set an exact
SPDK mask/list such as `RCOW_TGT_CPUMASK=0x30` or `[4,5]` for pre-isolated
cores. Existing one-click installations retain any persisted mask (including
the former `0x3` default); remove or empty `RCOW_TGT_CPUMASK` before upgrading
to opt into automatic selection.

## LIMITATIONS and TODOs

### Deleting snapshots / lvols

- Deleting a snapshot also releases its zero-copy reference exports. If an
  importer still has a live lease, or a local esnap clone still reads through
  one, the delete is recorded as pending and completes after those readers stop.
  Callers do not need to sequence `rcow_release_export` themselves. Dense exports
  keep their independent copies and remain valid.
- An **active** (attached) lvol cannot be deleted; deactivate it first
  (`rcow_deactive_bdev`).
- The delete-time cluster-count log line needs the blob to still be open: if
  the lvol was deactivated before deletion the blob is closed and the *"Deleted
  lvol ..."* log line simply omits the counts (they are not recoverable once
  the blob is gone).

### Snapshotting / cloning an imported lvol

- An lvol that was **imported from an export** cannot be snapshotted or cloned
  while it is still queued for decoupling: `derive_check` refuses with *"is
  queued to be decoupled; a snapshot or clone would take its external snapshot
  while the decouple still reads through it"*. Wait for the decouple to finish
  (`rcow_get_decouple` status -- its list emptying is the signal) before taking
  the snapshot or clone.

## Retrying a refused snapshot delete (`--retry-pending`)

A snapshot that is pinned — an export names it, it has clones, or a decouple is
running — cannot be deleted: `rcow_delete_lvol` refuses, and the refusal records
a **pending-delete mark** for the snapshot. The mark is the *only* record that a
delete was asked for: from the target's side every delete arrives as the same
RPC, so there is no way to tell a user's delete from an automation's. The mark
therefore is what "the user asked for this delete" means.

Marks are keyed by **(lvstore uuid, lvol uuid)**, not by name: a name is unique
only inside one loaded lvstore and is reusable, so a name-keyed mark could end up
pointing at a snapshot the delete was never refused for.

`rcow_get_lvstores` reports the mark per snapshot (`delete_pending`), together
with whether the snapshot is deletable *right now* (`deletable`).

Most recorded intents are finished by a **background poller** (~60 s, 16 deletes
per tick) once the blocker is gone. `--retry-pending` is for the cases the
poller will not touch, and for not waiting a minute:

```sh
test/tools/s3lvol_rpc.py --retry-pending
```

It reads `rcow_get_lvstores`, finds every snapshot that **both** carries the
pending-delete mark **and** is currently deletable, and calls `rcow_delete_lvol`
for each — passing the snapshot's uuid alongside its name, so a delete meant for
one object cannot land on a later one that reused the name:

- A snapshot without the mark is **never touched** — snapshots a test or another
  node created, or that were never deleted on purpose, are left alone.
- A snapshot that is still pinned reports `deletable: NO` and is deliberately
  skipped; run `--retry-pending` again once the blocker clears.
- Each successful delete clears the mark on the target; a delete refused again
  is reported and does not stop the others.

The command takes no method argument. Its exit status is 0 when every
marked-and-deletable snapshot went through, 1 if any delete failed, and it
prints `no pending snapshot deletes to retry` when there is nothing to do:

```sh
$ test/tools/s3lvol_rpc.py --retry-pending
no pending snapshot deletes to retry
```

### What the poller completes, and what still needs a human

The poller finishes deletes whose blockers clear without anyone deciding
anything:

- a **leased** export (`export`) — the importer stops renewing, the lease goes
  stale, that is evidence nobody is reading;
- a **pre-lease** export (`export_legacy`) once a snapshot delete has been
  recorded — the intent is the revocation (including marks restored from an
  older build);
- an export still **publishing** (`export_inflight`);
- extra **clones** (`clone_count`);
- a running **decouple** (`decouple`).

It will **not** complete:

- a destroy that already **failed asynchronously** (`failed`). Retrying it every
  minute would only repeat the same failure.

If the poller itself failed to register, queued deletes also wait for an
explicit retry (a log line says so).

### Persistence and the crash window

Marks are written to `<prefix>/meta/pending-deletes.json` and restored when the
lvstore attaches, so a restart does not forget a delete the caller was told not
to reissue.

The RPC answers **before** that PUT. If the process crashes (or the PUT fails)
in the window after the mark is in memory and before the object lands, the
intent is gone after the next attach and the delete has to be asked for again
— the same class of window the exports registry accepts, and cheaper: the
snapshot is still there.

Unload or destroy drops the in-memory queue for that lvstore (marks must not
outlive the lvols they name). The S3 object is reloaded on the next attach of
the same prefix; a failed or missing registry is logged and attach continues.

### Cancelling a pending delete (`rcow_cancel_pending_delete`)

The mark is an intent, not a lock. Withdraw it with:

```sh
scripts/s3lvol_rpc.py rcow_cancel_pending_delete '{"lvol_name":"snap0"}'
# optional lvs_name when the same snapshot name exists on more than one lvstore
scripts/s3lvol_rpc.py rcow_cancel_pending_delete '{"lvs_name":"vs0","lvol_name":"snap0"}'
```

`lvol_name` is required; `lvs_name` is optional and only disambiguates. The
RPC is **idempotent**: cancelling a name that was never queued, or that the
poller has already finished, still succeeds — the caller asked for "this
snapshot will not be deleted behind my back", and that is the state they get.
The snapshot itself was never touched, only the intent.

The same crash window applies as for recording: the RPC answers, then the
registry PUT runs. A crash before that PUT lands can restore the mark on the
next attach.

If the poller has **already submitted** `s3lvol_lvol_destroy`, cancel only
drops the mark. It does not abort the in-flight destroy; the snapshot may
still go away. Check `rcow_get_lvstores` afterwards if that race matters.

### What this is not

- **Not every refusal records a mark.** Recorded are the blockers
  `s3lvol_lvol_destroy()` itself identifies — an export pin (published or still
  in flight), more than one clone, a running decouple — plus an asynchronous
  destroy failure. Not recorded: the RPC-layer refusals that run before it (an
  NVMf-active volume, an unreadable active registry), and the case where
  `spdk_blob_get_clones()` answers an unknown error. A bdev unregister that fails
  **is** recorded as `failed`. Those unrecorded refusals are either the caller's
  own precondition to fix (deactivate first) or a failure that has to be looked
  at rather than retried blindly.

  The recording does not re-check that the volume is a snapshot: with the blob
  closed that is not reliably answerable, and only the three blockers above get
  that far anyway. In practice a mark therefore means "a delete was asked for
  and refused because something still referenced the volume".
- **`delete_pending` is a snapshot notion.** `rcow_get_snapshot_status` reports
  it when queried by `snapshot_name`; queried by `export_uuid` it is always
  `false`, because an export names a snapshot but is not one.
- **The cluster path does not use it.** Cubelet's own delete
  (`S3Cow.DeleteByKind`) still treats a refused snapshot delete as success, and
  it does not run `--retry-pending`. This is an operator tool for the node; the
  object leak on the cluster path is a separate change.

`deletable: YES` means every blocker the delete path checks is clear right now —
export pins (live lease or a miss younger than grace, from the last lease poll),
a running decouple, an NVMf-active volume, and more than one clone. A local
esnap reader of an export of the snapshot also keeps it `NO`.
It is a snapshot of the current state, not a promise: something can pin the
snapshot again between the query and the delete, and a retry that is refused
again is reported by `--retry-pending` (exit status 1) rather than hidden.

