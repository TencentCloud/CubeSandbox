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
yum install -y nvme-cli python3 openssl libuuid libaio numactl-libs

# Debian / Ubuntu
apt install -y nvme-cli python3 libssl1.1 libuuid1 libaio1 libnuma1
```

`ldd bin/s3lvol_tgt` lists the exact system libraries required. **Watch out for
openssl**: the package is built against the build machine's `libssl.so.1.1`, and
the target's major version must match (1.1 and 3.x are not interchangeable).

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
| `RCOW_LVS_NAME` | derived `rcow-<hostname-hash>`; a pre-existing `rcow` entry is honoured | lvstore name, also the prefix in S3 |
| `RCOW_CAPACITY_GB` | `16384` | used only at first create; thin, unused space costs nothing |
| `RCOW_CACHE_MB` | `490496` | chunk cache on the WAL image; only matters before the first start |
| `RCOW_LISTEN_ADDR` / `RCOW_LISTEN_PORT` | `127.0.0.1` / `4420` | |
| `RCOW_TGT_CPUMASK` | `0x3` | |
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

The lvstore name is derived from the same hostname (`rcow-<hostname-hash>`),
so duplicate hostnames also collide on the S3 prefix itself. When nodes have
a real unique id (an inventory id, a Kubernetes node name, an instance id),
pin `RCOW_LVS_NAME` to it instead of relying on the hostname-derived
default, and never let two nodes share a hostname.

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

The parameter names are deliberately chosen; the easy mistakes to make when
copying them:

- `create_lvol` has **no `lvs_name`** — it operates on the single lvstore that
  exists.
- `create_snapshot` takes `lvol_name` as the source and `snapshot_name` as the
  target; snapshots are read-only.
- `create_clone` takes `snapshot_name` (which **must be a read-only snapshot**)
  as the source and `clone_name` as the target.
- `resize_lvol` can only grow, never shrink.
- `delete_lvol` also clears the volume's activation record, so the next restart
  does not try to restore it.

**Exporting a snapshot** (cross-node transfer):

```sh
scripts/s3lvol_rpc.py rcow_export_snapshot '{"snapshot_name":"snap0"}'
```

`rcow_export_snapshot` returns an **export uuid immediately**; the export itself
keeps running in the background. The uuid is not a completion signal — poll it:

```sh
scripts/s3lvol_rpc.py rcow_get_snapshot_status '{"export_uuid":"<uuid>"}'
# -> {"export_status":"INPROGRESS","deletable":"NO"}  ... then, eventually
# -> {"export_status":"DONE","deletable":"NO"}          (still pinned while exported)
```

`rcow_get_snapshot_status` returns two fields:

- `export_status`: `INPROGRESS` / `DONE` / `NONE`.
- `deletable`: `YES` / `NO`, computed on the spot each time (never cached) and
  mirroring what `rcow_delete_lvol` would actually refuse: a snapshot is **not
  deletable** while an export is in progress or when it has more than one clone.

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
the cores they are pinned to and never sleep. The current deployment starts
with **2 reactors on 2 dedicated cores** (e.g. `-m 0x3` pins them to CPU 0 and
CPU 1). Those two cores are fully consumed by the target, so **other
(application/business) processes must be kept off them** — pin them elsewhere
with `taskset`/`numactl` (or a cpuset/cgroup) so the target's request latency
is not disturbed by scheduler contention.

## LIMITATIONS and TODOs

### Deleting snapshots / lvols

- A snapshot that is **behind an export** cannot be deleted while the export is
  alive: `s3lvol_lvol_destroy` refuses with *"the snapshot behind export
  \<uuid\>, which another node may be reading through. Release that export, or
  wait for it to expire, before deleting this."* — call `rcow_release_export`
  first (or wait for the export TTL to lapse).
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

