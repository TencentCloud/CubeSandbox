#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  rcow_common.sh -- configuration and helpers shared by the rcow control scripts
#
#  Sourced by rcow_start.sh, rcow_stop.sh and rcow_recovery.sh. Not runnable on
#  its own.
#
#  === Why these scripts exist at all ===
#
#  s3lvol_tgt comes up empty: no bdevs, no NVMf transport, no subsystems, no
#  lvstore. Every one of those has to be re-established after each boot, in an
#  order where two of the steps are dangerous to get wrong:
#
#    - create vs. attach of the lvstore. Create formats the WAL device; attach
#      replays it. Choosing create when an lvstore already exists destroys the
#      only copy of writes that were acknowledged but not yet in S3. The choice
#      is therefore made from /data/cubelet/rcow/bstore.json and from nothing
#      else -- never from "the attach failed, so let us try create".
#
#    - reproducing the host-side namespace layout. Which volume sat on which
#      subsystem and nsid is recorded in /data/cubelet/rcow/active_lvols,
#      because it
#      cannot be recomputed: the nsid of a volume that was moved is not its
#      hash, and the host's /dev/nvmeXnY numbering follows discovery order.
#
#  === The subsystem grid ===
#
#  RCOW_NUM_SUBSYS subsystems, each created with max_namespaces =
#  RCOW_NS_PER_SUBSYS, all listening on the same address and port, all connected
#  by the initiator at startup. Both numbers must match RCOW_NUM_SUBSYS and
#  RCOW_NS_PER_SUBSYS in module/bdev/s3lvol/vbdev_s3lvol.h, and the NQN pattern
#  must match RCOW_NQN_PREFIX plus a two-digit index there: the module derives
#  the NQN it attaches to from the same arithmetic, and a mismatch shows up as
#  "subsystem N does not exist" from rcow_active_bdev.
#
#  The -m flag on nvmf_create_subsystem is not optional. Without it a subsystem
#  defaults to 32 namespaces and any nsid above that is refused with "NSID
#  greater than maximum not allowed" -- a message that only appears in the target
#  log, while the RPC just reports a failed add.

# Guard against being sourced twice: rcow_recovery.sh may hand over to
# rcow_start.sh, and re-running the derivations below would be harmless but the
# guard makes it obvious that it does not happen.
if [ -n "${RCOW_COMMON_SOURCED:-}" ]; then
	return 0
fi
RCOW_COMMON_SOURCED=1

RCOW_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RCOW_REPO_ROOT="${RCOW_REPO_ROOT:-$(cd "${RCOW_SCRIPT_DIR}/.." && pwd)}"

# --------------------------------------------------------------------------
# Two layouts, one set of scripts
#
# These run both from a source checkout and from a release package, whose layout
# is flat and has no SPDK tree beside it:
#
#   repo:      scripts/rcow_*.sh   app/s3lvol_tgt/s3lvol_tgt   scripts/rpc.py -> $SPDK_ROOT/scripts/rpc.py
#   package:   scripts/rcow_*.sh   bin/s3lvol_tgt              scripts/rpc.py -> scripts/spdk_rpc.py
#
# Detected rather than substituted at packaging time. Rewriting these paths while
# building the package would mean the scripts that ship are not the scripts that
# were tested, and the difference would be in exactly the code that finds
# everything else -- so any mistake in it lands on the deployment machine, where
# it is hardest to look into. A branch here is testable from the repository.
#
# The marker is bin/s3lvol_tgt: a package always has one, and a checkout never
# does (the binary lives in app/s3lvol_tgt/). Every value below stays overridable
# from the environment, so neither layout has to be guessed at when it matters.
# --------------------------------------------------------------------------
if [ -x "${RCOW_REPO_ROOT}/bin/s3lvol_tgt" ]; then
	RCOW_LAYOUT="package"
else
	RCOW_LAYOUT="repo"
fi

if [ "${RCOW_LAYOUT}" = "package" ]; then
	RCOW_TGT_BIN="${RCOW_TGT_BIN:-${RCOW_REPO_ROOT}/bin/s3lvol_tgt}"
	RCOW_RPC_PY="${RCOW_RPC_PY:-${RCOW_SCRIPT_DIR}/s3lvol_rpc.py}"
	RCOW_SPDK_RPC_PY="${RCOW_SPDK_RPC_PY:-${RCOW_SCRIPT_DIR}/rpc.py}"

	# SPDK's rpc.py finds its library with
	#   sys.path.insert(0, os.path.dirname(__file__) + '/../python')
	# which in a package points at <root>/python -- not where it is. Rather than
	# patch an upstream file, or lay the package out around that one line, the
	# path is supplied the way Python is meant to take it. rpc.py's own insert
	# stays harmless: a non-existent directory on sys.path is not an error.
	if [ -d "${RCOW_SCRIPT_DIR}/python" ]; then
		PYTHONPATH="${RCOW_SCRIPT_DIR}/python${PYTHONPATH:+:${PYTHONPATH}}"
		export PYTHONPATH
	fi
else
	# Each fallback prints at most one line. && and || have equal precedence,
	# so an ungrouped `echo /opt && pwd` would still run pwd in the current
	# directory and bake a two-line SPDK_ROOT.
	SPDK_ROOT="${SPDK_ROOT:-$(cd "${RCOW_REPO_ROOT}/deps/spdk" 2>/dev/null && pwd \
		|| { [ -f /opt/s3lvol-spdk/scripts/rpc.py ] && echo /opt/s3lvol-spdk; } \
		|| { cd "${RCOW_REPO_ROOT}/../spdk" 2>/dev/null && pwd; } \
		|| echo /data/home/cow/spdk)}"

	RCOW_TGT_BIN="${RCOW_TGT_BIN:-${RCOW_REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt}"
	RCOW_RPC_PY="${RCOW_RPC_PY:-${RCOW_REPO_ROOT}/test/tools/s3lvol_rpc.py}"
	# Same launcher as the package: it applies the 3.8 argparse shim and
	# then runs $SPDK_ROOT/scripts/rpc.py. Calling the SPDK file directly
	# would blow up on Ubuntu 20.04 (no BooleanOptionalAction).
	RCOW_SPDK_RPC_PY="${RCOW_SPDK_RPC_PY:-${RCOW_SCRIPT_DIR}/rpc.py}"
	export SPDK_ROOT
fi

RCOW_RPC_SOCK="${RCOW_RPC_SOCK:-/var/run/s3lvol.sock}"

# --------------------------------------------------------------------------
# Configuration. Everything is overridable from the environment so that a second
# instance, or a test, does not need a modified copy of these scripts.
#
# The paths that differ between the two layouts are set above; what follows is
# the same either way.
# --------------------------------------------------------------------------

# Read for the S3 endpoint, region, bucket list and credentials. Credentials
# leave this file only through the environment of the target process.
RCOW_S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"

# The local device behind the metadata journal and the WAL. Fixed rather than
# discovered: it is the one piece of state that must not move between boots, and
# an lvstore attached against the wrong image replays someone else's log.
RCOW_WAL_IMG="${RCOW_WAL_IMG:-/data/cubelet/rcow/wal_bdev.img}"
RCOW_WAL_BDEV="${RCOW_WAL_BDEV:-rcow_wal0}"

# The lvstore name, which is *also* its key prefix in the bucket: s3_bs_dev_create()
# does `prefix = lvs_name` (s3_bs_dev.c:2267). Two nodes with the same name and the
# same bucket therefore share one prefix, and since `<prefix>/meta/checkpoint` has a
# fixed name, each would overwrite the other's chunk map -- silently, because the
# data objects are uuid-named and never collide, they just stop being reachable.
#
# So the default has to differ per node. It is derived rather than set, at the
# bottom of this file: doing it here is not possible, because the derivation needs
# rcow_bstore_entry() and the logging helpers, both defined further down.
#
# Set RCOW_LVS_NAME in the environment to pin an explicit name; nothing below
# touches it then. The tests all do this, which is also how they stay isolated.
RCOW_LVS_NAME="${RCOW_LVS_NAME:-}"
if [ -n "${RCOW_LVS_NAME}" ]; then
	RCOW_LVS_NAME_EXPLICIT=1
else
	RCOW_LVS_NAME_EXPLICIT=0
fi

# Only consulted when there is no lvstore to attach. capacity is the logical
# size of the blobstore, which is thin, so it costs nothing in S3 until written;
# journal and WAL sizes are carved out of the local image at create time and read
# back from it on every later attach.
RCOW_CAPACITY_GB="${RCOW_CAPACITY_GB:-16384}"
RCOW_JOURNAL_MB="${RCOW_JOURNAL_MB:-1024}"
RCOW_WAL_MB="${RCOW_WAL_MB:-32768}"
RCOW_CKPT_INTERVAL_SEC="${RCOW_CKPT_INTERVAL_SEC:-60}"

# Must agree with vbdev_s3lvol.h; see the header comment.
RCOW_NUM_SUBSYS="${RCOW_NUM_SUBSYS:-32}"
RCOW_NS_PER_SUBSYS="${RCOW_NS_PER_SUBSYS:-64}"
RCOW_NQN_PREFIX="${RCOW_NQN_PREFIX:-nqn.2026-08.io.spdk:rcow-}"

RCOW_LISTEN_ADDR="${RCOW_LISTEN_ADDR:-127.0.0.1}"
RCOW_LISTEN_PORT="${RCOW_LISTEN_PORT:-4420}"
RCOW_IO_QUEUES="${RCOW_IO_QUEUES:-4}"

# The NVMf model number, pinned. Left unset, the model is SPDK's default and
# moves with every SPDK upgrade. The host identifies a disk by its namespace
# UUID -- which the lvol uuid fixes -- so a changed model is not expected to
# matter, but it is the only part of the host's view not already pinned by NQN,
# serial or namespace UUID, and pinning it is what lets a cross-SPDK hot
# upgrade keep every other variable out of the equation.
RCOW_MODEL="${RCOW_MODEL:-CubeS3lvol}"

# Initiator timeouts. Both go to `nvme connect` on a fresh start and to sysfs on
# an upgrade (rcow_tune_initiator_timeouts). reconnect_delay is how long after
# the target is ready the host takes to notice; 1 is the kernel's floor, and no
# user-space action can wake the timer sooner. ctrl_loss_tmo is the rollback
# budget -- what lets a failed upgrade be retried by hand instead of ending in
# deleted controllers and EIO.
RCOW_RECONNECT_DELAY="${RCOW_RECONNECT_DELAY:-1}"
RCOW_CTRL_LOSS_TMO="${RCOW_CTRL_LOSS_TMO:-1800}"
# What the budget is really worth when the kernel does not export ctrl_loss_tmo
# at all (pre-5.7). It then uses its 600s default no matter what is configured,
# so a deadline drawn from RCOW_CTRL_LOSS_TMO would be a promise the kernel does
# not keep.
RCOW_MIN_CTRL_LOSS_TMO="${RCOW_MIN_CTRL_LOSS_TMO:-300}"

# The transport's max_io_size, set to the chunk size (S3LVOL_DEFAULT_CHUNK_SIZE,
# 1 MiB) instead of the TCP default of 128 KiB.
#
# It is a transport option, not a subsystem one, and it is what the host is told:
# ctrlr.c:3264 derives MDTS from it as log2(max_io_size / 4096), so the initiator's
# max_hw_sectors_kb follows. At the default the kernel cuts every 1 MiB request
# into eight 128 KiB commands; at 1 MiB it sends one, and because the bdev sets
# split_on_optimal_io_boundary with a 1 MiB boundary, that one command lands
# inside a single chunk -- one S3 object -- rather than straddling two.
#
# Upper bound: nvmf refuses the transport outright if
# max_io_size / iobuf.large_bufsize > SPDK_NVMF_MAX_SGL_ENTRIES (tcp.c:835),
# which with the 132 KiB default large_bufsize and 16 entries allows just over
# 2 MiB. 1 MiB gives 7 of the 16.
#
# What this does *not* do: make one host I/O one bdev I/O. io_unit_size stays at
# 128 KiB, so nvmf still hands over eight buffers, and the bdev layer splits them
# into eight single-segment children because s3_bs_dev only accepts iovcnt == 1
# (max_num_segments = 1, vbdev_s3lvol_lvol.c:364). The win here is on the wire and
# in chunk alignment. Collapsing it further would need either io_unit_size at
# 1 MiB -- which requires raising iobuf's large_bufsize and costs
# num_shared_buffers x 1 MiB of memory per poll group -- or multi-iovec support in
# s3_bs_dev. Neither is done here.
RCOW_MAX_IO_SIZE="${RCOW_MAX_IO_SIZE:-1048576}"

# iobuf: how many pieces a host I/O arrives in.
#
# The knob for this is not the transport's io_unit_size -- lib/nvmf/tcp.c does not
# reference that option at all in this version. What decides it is iobuf's buffer
# size (transport.c:209 copies it into the transport, and :922-931 does the
# arithmetic):
#
#     io_unit_size = length > small_bufsize ? large_bufsize : small_bufsize
#     num_buffers  = ceil(length / io_unit_size)
#
# With the 132 KiB default, a 1 MiB read arrives as 8 buffers, so 8 iovecs, and
# because the bdev sets max_num_segments = 1 (s3_bs_dev only accepts iovcnt == 1)
# the bdev layer splits it into 8 children -- 8 S3 object operations for one host
# I/O. Measured with bdev_get_iostat: read_ops +8. At 1 MiB it is one buffer, one
# bdev I/O, one object.
#
# The price is in that same expression: **anything larger than small_bufsize
# (8 KiB) takes a whole large buffer**. A 16 KiB read now pins 1 MiB instead of
# 132 KiB. iobuf has exactly two size classes, so this cannot be shaped further
# without moving small_bufsize up as well, which would then waste on 4 KiB I/O.
# The pool count is what bounds the total: exhausting it makes requests wait in
# iobuf (transport.c:943 passes an entry, so they queue and are retried), which
# costs latency, not correctness.
#
# 512 x 1 MiB = 512 MiB, against the 1024 x 132 KiB = 132 MiB it replaces. Keep
# RCOW_TGT_MEM_MB in step: the pools come out of the DPDK allocation.
#
# Set through iobuf_set_options, which is a *startup* RPC
# (module/event/subsystems/iobuf/iobuf_rpc.c:58), hence --wait-for-rpc and the
# explicit framework_start_init in rcow_apply_startup_opts().
RCOW_IOBUF_LARGE_BUFSIZE="${RCOW_IOBUF_LARGE_BUFSIZE:-1048576}"
RCOW_IOBUF_LARGE_POOL="${RCOW_IOBUF_LARGE_POOL:-512}"

# Host-side readahead for every activated volume, in KiB. Matches the chunk size
# for the same reason RCOW_MAX_IO_SIZE does. 0 disables the tuning entirely.
#
# **Exported to the target**, which is what actually applies it: the module sets
# readahead the moment a volume resolves to a device (get_bdev_write_one), which
# is earlier than any script can manage and needs no cooperation from whoever
# activated the volume. Exporting rather than only setting is the same bargain as
# S3LVOL_ACTIVE_FILE above -- with two independent values, the two layers would
# write different numbers to the same sysfs file and silently undo each other.
# rcow_tune_readahead() below then only has to cover volumes that were already
# active before the target learned this.
#
# Why it is worth the attention: what a cold read costs is one S3 request, near
# enough regardless of how many bytes it asks for. Measured on this stack, at a
# queue depth of 64:
#
#     bs=128k   797 IOPS    99.6 MB/s
#     bs=1M     252 IOPS   251.7 MB/s
#
# Same order of requests, eight times the data. So a sequential reader that lets
# the kernel read ahead a whole chunk pays one request where it used to pay
# eight. With the kernel default of 128 KiB against a 1 MiB chunk, a single
# threaded sequential read (md5sum, tar, cp) measured 6.1 MB/s; at 1024 KiB the
# same read measured 29.4 MB/s.
#
# It also compounds with the chunk cache: a 1 MiB aligned read populates a whole
# slot in one go (s3_cache.c), so the *second* pass over the same data is served
# locally -- 481 MB/s measured, against 342 with the kernel default.
#
# The cost is read amplification on random small reads -- 4 KiB asked for, up to
# 1 MiB fetched. That is a real cost for a random-only workload; set this to 128
# for the kernel default, or 0 to leave devices strictly alone. Neither is a
# correctness risk, and the cache's residency bitmap means the extra bytes are
# kept rather than discarded.
RCOW_READ_AHEAD_KB="${RCOW_READ_AHEAD_KB:-1024}"
export S3LVOL_READ_AHEAD_KB="${RCOW_READ_AHEAD_KB}"

# Whole-object native cache slots kept in anonymous RAM per lvstore. At the
# default 1 MiB chunk size, 1024 slots are 1 GiB of resident memory because the
# cache deliberately uses MAP_POPULATE. Zero keeps the disk cache but disables
# this RAM tier; the RPC enforces the hard maximum of 8192.
RCOW_CACHE_HOT_BUFS="${RCOW_CACHE_HOT_BUFS:-1024}"

# Default -m: last two CPUs from this process's Cpus_allowed_list (CPU0 stays
# free for housekeeping when the set is 0..N-1). An explicit RCOW_TGT_CPUMASK
# always wins. See rcow_cpumask.sh.
# shellcheck source=./rcow_cpumask.sh
. "${RCOW_SCRIPT_DIR}/rcow_cpumask.sh"
RCOW_TGT_CPUMASK="${RCOW_TGT_CPUMASK:-$(rcow_default_tgt_cpumask)}"
RCOW_TGT_MEM_MB="${RCOW_TGT_MEM_MB:-16384}"

# Run without hugepages, deliberately, rather than as a fallback for a node
# nobody has tuned.
#
# What hugepages buy DPDK is TLB coverage and physically contiguous memory that
# can be pinned and handed to a device for DMA. This target needs neither:
#
#   - the only local block device is bdev_aio, which reaches the disk through
#     read/write syscalls, so the kernel copies from ordinary process memory and
#     no IOVA is involved;
#   - the network side is nvmf_tcp over kernel sockets, again syscalls;
#   - S3 traffic goes through aws-c-s3, whose buffers are its own malloc'd
#     memory and never DPDK's.
#
# So nothing in this data path programs a device with a physical address, which
# is the one thing --no-huge takes away (DPDK cannot resolve IOVAs for
# non-hugepage memory, so a VFIO/uio driver -- bdev_nvme on a real PCIe SSD,
# virtio -- would not work). Adding such a bdev is the moment to revisit this.
#
# What it costs is TLB pressure on the DPDK heap, which here is mostly the iobuf
# pools. Worth measuring before assuming it matters, and easy to switch back:
# RCOW_NO_HUGE=0 with hugepages reserved.
#
# -s is meaningful only together with --no-huge: it caps the heap that gets
# malloc'd up front. With hugepages, DPDK takes what the reservation allows.
RCOW_NO_HUGE="${RCOW_NO_HUGE:-1}"

RCOW_RUN_DIR="${RCOW_RUN_DIR:-/var/tmp/rcow}"
RCOW_PIDFILE="${RCOW_PIDFILE:-${RCOW_RUN_DIR}/s3lvol_tgt.pid}"

# The intent marker, "<boot_id> <pid> [<candidate>]", read only by
# rcow_hot_marker_consume().
# It answers one question -- was the last stop asked for by an upgrade? -- and
# it must be impossible for a stale one to answer yes, because RCOW_RUN_DIR is
# not tmpfs and the file outlives a reboot.
#
# The candidate is the binary the upgrade intends to start, and it travels here
# rather than through the stop's environment because this file's writer is the
# only side that knows it: the stop runs before the versioned directory is
# switched into place, so at that point RCOW_TGT_BIN still names the outgoing
# binary and comparing it would compare the running build with itself.
RCOW_HOT_MARKER="${RCOW_RUN_DIR}/hot-restart"
# The layout captured before the target is killed, which rcow_verify_active
# --expect compares the post-upgrade layout against.
RCOW_HOT_SNAPSHOT="${RCOW_RUN_DIR}/hot-upgrade.snapshot"

# Log directory separate from the run directory: the pid lives in a tmpfs location
# and the log is persistent under /data.
RCOW_LOG_DIR="${RCOW_LOG_DIR:-/data/log/rcow}"
RCOW_LOG="${RCOW_LOG:-${RCOW_LOG_DIR}/s3lvol_tgt.log}"

# Written by the module, and now genuinely configuration rather than a declaration
# of something compiled in: the module resolves both paths through the environment
# (S3LVOL_ACTIVE_FILE / S3LVOL_BSTORE_FILE, see vbdev_s3lvol_statefile.c) and falls
# back to these same defaults when they are unset.
#
# Exported, not just set, because the target inherits them -- without that the
# scripts would be reading one file while the module wrote another, which is worse
# than having no setting at all. The names differ because the module has no business
# knowing about the "rcow" deployment flavour; this is where the two meet.
RCOW_ACTIVE_FILE="${RCOW_ACTIVE_FILE:-/data/cubelet/rcow/active_lvols}"
RCOW_BSTORE_FILE="${RCOW_BSTORE_FILE:-/data/cubelet/rcow/bstore.json}"
export S3LVOL_ACTIVE_FILE="${RCOW_ACTIVE_FILE}"
export S3LVOL_BSTORE_FILE="${RCOW_BSTORE_FILE}"

# The state files decide attach-vs-create and the host namespace layout, so
# they must survive a reboot. /var/tmp would be cleaned by systemd-tmpfiles
# and is world-writable; /data/cubelet/rcow/ is where the WAL image already
# lives (install.sh creates it for the systemd path). Bare-metal runs that
# never went through install.sh still need it, so create it here, idempotently.
mkdir -p "$(dirname "${RCOW_ACTIVE_FILE}")"

# The replay plan: a copy of the registry taken before it is cleared. Owned by
# these scripts, not by the module.
RCOW_REPLAY_FILE="${RCOW_REPLAY_FILE:-${RCOW_ACTIVE_FILE}.replay}"

RCOW_STOP_TIMEOUT="${RCOW_STOP_TIMEOUT:-120}"
RCOW_RPC_TIMEOUT="${RCOW_RPC_TIMEOUT:-300}"

# --------------------------------------------------------------------------
# Output
# --------------------------------------------------------------------------

rcow_log()  { printf '[rcow %s] %s\n' "$(date +%H:%M:%S)" "$*"; }
rcow_warn() { printf '[rcow %s] WARNING: %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
rcow_err()  { printf '[rcow %s] ERROR: %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
rcow_die()  { rcow_err "$*"; exit 1; }

rcow_step() { printf '\n[rcow %s] ==== %s\n' "$(date +%H:%M:%S)" "$*"; }

rcow_need_root()
{
	if [ "$(id -u)" -ne 0 ]; then
		rcow_die "must run as root: the target needs to lock memory and \
nvme connect needs /dev/nvme-fabrics"
	fi
}

rcow_need_cmd()
{
	command -v "$1" >/dev/null 2>&1 || rcow_die "$1 is not installed; $2"
}

# nvme connect writes its connect string to /dev/nvme-fabrics, and without the
# in-kernel nvme-tcp transport loaded that write fails -- silently, because
# rcow_connect_all drops connect's stderr. The symptom is then "32 failed" with
# no cause, so the module is ensured here instead. Loading is idempotent and
# harmless, and only root can; rcow_need_root has already run by the time this
# is called.
rcow_need_nvme_tcp()
{
	[ -d /sys/module/nvme_tcp ] && return 0
	modprobe nvme-tcp 2>/dev/null && return 0
	rcow_die "the kernel nvme-tcp transport is not loaded and \
modprobe nvme-tcp failed. Load it (modprobe nvme-tcp), then retry -- without it \
every nvme connect fails and the initiator reports every subsystem as failed"
}

# Catch missing DT_NEEDED libraries before launch. OpenSSL is linked
# statically, so this is the remaining system set (libuuid, libaio, libnuma,
# glibc, ...). A bare loader error in the log does not name the binary.
rcow_check_tgt_deps()
{
	local missing
	missing="$(ldd "${RCOW_TGT_BIN}" 2>/dev/null | grep -i 'not found' || true)"
	[ -n "${missing}" ] || return 0

	rcow_err "the target binary has missing shared libraries:"
	printf '%s\n' "${missing}" | sed 's/^/    /' >&2
	rcow_die "refusing to start with missing shared libraries"
}

rcow_ensure_run_dir()
{
	mkdir -p "${RCOW_RUN_DIR}" 2>/dev/null ||
		rcow_die "cannot create ${RCOW_RUN_DIR}"
}

# Make one file durable, its directory entry included.
#
# Deliberately not `sync`: that walks every block device in the system
# (sync_bdevs), and the devices a replay is about to bring back are exactly the
# ones that cannot take writes yet -- their controllers are still retrying, and
# the listeners that let them in are added after the replay. A host-wide sync
# therefore parks on the very I/O it is here to restore, and stays parked until
# ctrl_loss_tmo expires. What has to survive a crash here is one small file.
rcow_fsync_file()
{
	python3 - "$1" <<-'PY' || return 1
	import errno, os, sys

	path = os.path.abspath(sys.argv[1])
	try:
	    fd = os.open(path, os.O_RDONLY)
	except OSError as e:
	    # The caller names the step that failed; this says why.
	    print("%s: %s" % (path, e.strerror), file=sys.stderr)
	    sys.exit(1)
	try:
	    os.fsync(fd)
	finally:
	    os.close(fd)

	# The name needs flushing as much as the contents: the file arrives by
	# rename.
	try:
	    dfd = os.open(os.path.dirname(path), os.O_RDONLY)
	except OSError:
	    sys.exit(0)
	try:
	    os.fsync(dfd)
	except OSError as e:
	    # Not every filesystem can sync a directory, and one that cannot is not
	    # a reason to refuse the upgrade.
	    if e.errno != errno.EINVAL:
	        raise
	finally:
	    os.close(dfd)
	PY
}

# --------------------------------------------------------------------------
# s3.cfg
#
# Parsed with sed rather than sourced. The file is TOML-shaped and holds the
# account credentials; running it as shell would execute whatever ends up in it,
# and it is written by a different component than this one.
# --------------------------------------------------------------------------

rcow_cfg_get()
{
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\1/p" \
		"${RCOW_S3_CFG}" 2>/dev/null | head -1 | tr -d '\r'
}

# buckets is a list -- the config already allows several buckets even
# though only the first carries an lvstore today. All of them are registered as
# namespaces, so that a volume imported from another bucket can be reached
# without a config change.
rcow_s3_buckets()
{
	sed -n 's/^[[:space:]]*buckets[[:space:]]*=[[:space:]]*\[\(.*\)\].*/\1/p' \
		"${RCOW_S3_CFG}" 2>/dev/null | head -1 | tr ',' '\n' |
		sed -n 's/^[[:space:]]*"\([^"]*\)".*/\1/p' | tr -d '\r'
}

# Into the environment and nowhere else: an RPC parameter would put the secret
# into the target's log, and a command-line argument would put it into ps output
# and the shell history.
rcow_load_credentials()
{
	local id key

	id="$(rcow_cfg_get access_key_id)"
	key="$(rcow_cfg_get secret_access_key)"

	if [ -z "${id}" ] || [ -z "${key}" ]; then
		return 1
	fi

	export AWS_ACCESS_KEY_ID="${id}"
	export AWS_SECRET_ACCESS_KEY="${key}"
	return 0
}

# Append --path-style / --no-tls onto the named bash array (caller must pass a
# hardcoded identifier such as s3_flags or S3_ADDR_FLAGS, never user input).
rcow_s3_addr_flags()
{
	local dest="$1"

	case "${dest}" in
	''|*[!A-Za-z0-9_]*|[0-9]*)
		return 0
		;;
	esac
	[ "$(rcow_cfg_get path_style)" = "true" ] && eval "${dest}+=(--path-style)"
	[ "$(rcow_cfg_get no_tls)" = "true" ] && eval "${dest}+=(--no-tls)"
	return 0
}

# --------------------------------------------------------------------------
# RPC
#
# Two clients on purpose: SPDK's rpc.py knows nothing about the rcow_* methods
# (they are registered in this repo, not in SPDK), and s3lvol_rpc.py deliberately
# knows nothing about argument shapes. Standard methods go through the first, our
# own through the second.
# --------------------------------------------------------------------------

rcow_rpc()
{
	local method="$1"; shift
	python3 "${RCOW_RPC_PY}" --sock "${RCOW_RPC_SOCK}" \
		--timeout "${RCOW_RPC_TIMEOUT}" "${method}" "${1:-}"
}

rcow_srpc()
{
	python3 "${RCOW_SPDK_RPC_PY}" -s "${RCOW_RPC_SOCK}" \
		-t "${RCOW_RPC_TIMEOUT}" "$@"
}

# Many standard calls in one python process, stopping at the first failure.
# 96 separate rpc.py invocations for the subsystem grid cost most of a minute in
# interpreter startup alone.
rcow_srpc_script()
{
	python3 "${RCOW_SPDK_RPC_PY}" -s "${RCOW_RPC_SOCK}" -t "${RCOW_RPC_TIMEOUT}"
}

rcow_json_get()
{
	python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get(sys.argv[2], ""))' \
		"$1" "$2" 2>/dev/null
}

rcow_nqn()
{
	printf '%s%02d' "${RCOW_NQN_PREFIX}" "$1"
}

# --------------------------------------------------------------------------
# Target process
#
# === Identified by its executable, never by its name ===
#
# SPDK renames the main thread as soon as the first reactor starts, so a live
# target's /proc/<pid>/comm reads "reactor_0" and nothing matches "s3lvol_tgt".
# Both obvious checks therefore return nothing on a running target:
#
#   pgrep -x s3lvol_tgt          -> no match
#   grep s3lvol_tgt /proc/N/comm -> no match
#
# Which is not a cosmetic problem. Written that way, the "is another target
# already running" check in rcow_start.sh silently became a no-op: measured, it
# let a second target start on top of a live one, and the newcomer took over
# /var/run/s3lvol.sock (the RPC server unlinks the path before binding), leaving the
# first process alive, holding the lvstore and the WAL image, and unreachable.
# Two targets one aio bdev apart from writing the same WAL is not a state
# anything recovers from.
#
# /proc/<pid>/exe survives the rename, so that is what is compared.
#
# With one wrinkle: the kernel appends " (deleted)" to that link once the binary
# has been replaced, which `make` does on every build. Left unhandled it makes
# the identity check fail against exactly the target this script is most likely
# to be asked to stop -- the one running the binary that was just rebuilt -- and
# a failed identity check reads as "no target is running". Measured: build,
# rcow_stop (which reported nothing running and then deleted the pidfile),
# rcow_start (whose RPC server unlinked the socket the live target was listening
# on), and the first target was left holding the lvstore and the WAL with no
# path left to reach it. The suffix is therefore stripped before comparing.
#
# And the exact comparison is not enough by itself, because an upgrade is exactly
# the case where the old binary's path stops resolving: the process was started
# from a versioned directory that is later renamed away, so readlink -f of its
# exe lands on nothing and only the kernel's " (deleted)" name is left. A
# basename sweep therefore follows the exact one. This function is the guard
# against a second target over a live WAL, and the second target most worth
# catching is the old one the upgrade means to leave behind.
# --------------------------------------------------------------------------

# Resolve /proc/<pid>/exe to a comparable path, or nothing.
rcow_pid_exe()
{
	local pid="$1" exe

	exe="$(readlink "/proc/${pid}/exe" 2>/dev/null)" || return 1
	[ -n "${exe}" ] || return 1

	# Strip the kernel's marker for an unlinked binary. Only as a suffix: a
	# path may legitimately contain the word.
	exe="${exe% (deleted)}"

	printf '%s' "$(readlink -f "${exe}" 2>/dev/null || printf '%s' "${exe}")"
}

rcow_pid_is_target()
{
	local pid="$1" exe want

	want="$(readlink -f "${RCOW_TGT_BIN}" 2>/dev/null)" || return 1
	exe="$(rcow_pid_exe "${pid}")" || return 1
	[ -n "${exe}" ] && [ "${exe}" = "${want}" ]
}

# Every running instance of the target binary, one pid per line. Needs root to
# read other users' /proc/<pid>/exe, which these scripts require anyway.
#
# Two passes, because one comparison cannot cover both a normally-running target
# and one whose binary was swapped away. The exact pass is the accurate one; the
# fallback catches the case it misses, where the same binary sits in the
# directory the previous version unpacked into. It matches on the basename *and*
# on this host's RPC socket, so a target started by hand from a build tree is
# not mistaken for one of ours. A pid found by both is reported once.
rcow_target_instances()
{
	local want d pid exe raw p dup
	local -a pids=()

	want="$(readlink -f "${RCOW_TGT_BIN}" 2>/dev/null)"

	if [ -n "${want}" ]; then
		for d in /proc/[0-9]*; do
			pid="${d#/proc/}"
			exe="$(rcow_pid_exe "${pid}")" || continue
			[ "${exe}" = "${want}" ] && pids+=("${pid}")
		done
	fi

	for d in /proc/[0-9]*; do
		pid="${d#/proc/}"

		dup=0
		if [ "${#pids[@]}" -gt 0 ]; then
			for p in "${pids[@]}"; do
				[ "${p}" = "${pid}" ] && { dup=1; break; }
			done
		fi
		[ "${dup}" -eq 1 ] && continue

		raw="$(readlink "/proc/${pid}/exe" 2>/dev/null)" || continue
		raw="${raw% (deleted)}"
		[ "${raw##*/}" = "s3lvol_tgt" ] || continue

		# The basename alone would also catch a target someone started by hand
		# from a build tree, and refusing to start over a stranger is a failure
		# no operator can act on. Everything this host's scripts start carries
		# its own -r, so requiring it keeps the moved-binary case without
		# matching an unrelated instance.
		tr '\0' '\n' <"/proc/${pid}/cmdline" 2>/dev/null |
			grep -qxF -- "${RCOW_RPC_SOCK}" || continue

		pids+=("${pid}")
	done

	if [ "${#pids[@]}" -gt 0 ]; then
		printf '%s\n' "${pids[@]}"
	fi
	return 0
}

# Echo the pid of the running target, or nothing. The identity check is not
# decoration: a pidfile outlives the process it names, and a recycled pid would
# make rcow_start.sh refuse to start and rcow_stop.sh signal a stranger.
rcow_target_pid()
{
	local pid

	[ -r "${RCOW_PIDFILE}" ] || return 1
	pid="$(tr -dc '0-9' <"${RCOW_PIDFILE}")"
	[ -n "${pid}" ] || return 1
	kill -0 "${pid}" 2>/dev/null || return 1
	rcow_pid_is_target "${pid}" || return 1

	printf '%s' "${pid}"
}

rcow_target_alive()
{
	rcow_target_pid >/dev/null
}

# Is the lvstore this host runs on loaded in the target?
#
# rcow_get_lvstores lists what the running process holds, so a named entry is the
# attach having completed. Nothing weaker answers the question: the process
# answers RPCs -- rcow_get_bdev in particular -- before it has attached
# anything, because the device paths in that answer come from the registry and
# the host's sysfs, which both survive a hot restart.
rcow_lvstore_attached()
{
	local out

	out="$(RCOW_RPC_TIMEOUT=10 rcow_rpc rcow_get_lvstores 2>/dev/null)" || return 1
	printf '%s' "${out}" | python3 -c '
import json, sys
name = sys.argv[1]
try:
    stores = json.load(sys.stdin)
except ValueError:
    sys.exit(1)
sys.exit(0 if any(st.get("lvs_name") == name for st in stores) else 1)
' "${RCOW_LVS_NAME}"
}

# Serving, as opposed to merely running: the process is up and its lvstore is
# attached. Waiting on the process alone is what let an upgrade report success
# for a replacement that was still attaching, and would go on to crash-loop.
rcow_target_ready()
{
	[ -n "$(rcow_target_instances)" ] || return 1
	rcow_lvstore_attached
}

# Record "<boot_id> <pid>" of the live target, for a stop an upgrade is about to
# ask for. Written by the orchestrator; rcow_upgrade.sh deliberately does not,
# so only an intent to hot-restart can make the marker read as one.
rcow_hot_marker_write()
{
	local pid boot candidate="${1:-}"

	pid="$(rcow_target_pid)" || {
		rcow_err "no live target to name in ${RCOW_HOT_MARKER}"
		return 1
	}
	boot="$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"
	[ -n "${boot}" ] || {
		rcow_err "cannot read /proc/sys/kernel/random/boot_id"
		return 1
	}

	rcow_ensure_run_dir
	printf '%s %s%s\n' "${boot}" "${pid}" "${candidate:+ ${candidate}}" \
		>"${RCOW_HOT_MARKER}" || {
		rcow_err "cannot write ${RCOW_HOT_MARKER}"
		return 1
	}
	return 0
}

# Consume the marker and answer whether it is ours: this boot, and a pid that is
# still the live target.
#
#   0  honoured: this boot, and a pid that is still the live target. The marker
#      is left for rcow_upgrade.sh to clear once that target is gone
#   1  nothing to honour -- no marker, a malformed one, or one from another boot
#   2  this boot and this marker, but the pid it names is not a running target
#
# The caller has to tell 1 from 2. A 1 means nobody asked for a hot restart; a 2
# may be a live intent this host cannot confirm, and answering it by falling
# through to the planned path takes the outage this mechanism exists to remove.
#
# The marker outlives the answer that recognises it. Reading it is not spending
# it: the stop that gets a 0 still has to run rcow_upgrade.sh, and that can
# refuse and leave the target running -- in which case the intent is unspent and
# the next stop must see it again, or a retry would silently become the planned
# teardown the marker was written to avoid. rcow_upgrade.sh clears it once the
# target it names is gone. The other two answers remove it here, because a file
# from another boot, or a malformed one, is stale by construction.
#
# The identity check is rcow_target_instances rather than rcow_pid_is_target
# because an upgrade has already been staged by the time it writes the marker,
# and the strict path comparison is exactly the one that fails there.
#
# Both fields are needed. boot_id alone misses a marker whose target died before
# a reboot and whose pid was reused after it; the pid alone misses a file that
# outlived the boot it names, because RCOW_RUN_DIR is not tmpfs.
#
# RCOW_HOT_CANDIDATE is set to the candidate the writer recorded, or empty when
# it recorded none.
rcow_hot_marker_consume()
{
	local boot pid candidate now

	RCOW_HOT_CANDIDATE=""

	if [ ! -s "${RCOW_HOT_MARKER}" ]; then
		rm -f "${RCOW_HOT_MARKER}"
		return 1
	fi

	read -r boot pid candidate _ <"${RCOW_HOT_MARKER}"
	now="$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"

	if [ -z "${boot}" ] || [ -z "${pid}" ] || [ -z "${now}" ] ||
		[ "${boot}" != "${now}" ]; then
		rm -f "${RCOW_HOT_MARKER}"
		return 1
	fi

	if ! printf '%s\n' "$(rcow_target_instances)" | grep -qxF -- "${pid}"; then
		rcow_warn "${RCOW_HOT_MARKER} names pid ${pid}, which is not a running \
target; leaving the marker in place rather than discarding the intent"
		return 2
	fi

	RCOW_HOT_CANDIDATE="${candidate}"
	return 0
}

rcow_hot_marker_clear()
{
	rm -f "${RCOW_HOT_MARKER}" 2>/dev/null || true
	return 0
}

# --------------------------------------------------------------------------
# Version gate
#
# === What it guards ===
#
# A hot restart hands the same on-disk state to a different binary. The local
# superblock, WAL super and checkpoint each carry a version and each fails hard
# with -EPROTO on one they do not know, so those bumps are loud. The journal is
# the exception: it has no version field, only an op value, and a reader that
# meets an op it does not know treats it as the end of the log. For a torn tail
# that is exactly right, and for a log written by a *newer* build it is silent
# truncation of acknowledged writes.
#
# So the gate runs before the kill and refuses, rather than letting the new
# binary decide: it is the only place the two formats are both known.
#
# === Why the comparison takes two documents ===
#
# One from the running target (rcow_get_build_info) and one from the candidate
# binary (s3lvol_tgt --print-build-info). Nothing here touches the target, so
# every refusal is testable with synthetic documents and no second build tree.
#
# rcow_version_gate_compare <running-json-file> <candidate-json-file>
#
# 0 = a hot upgrade is allowed, 1 = refused, with every reason on stderr.
# spdk_version is reported and never compared: an SPDK bump moves the nvmf
# firmware revision and the default model number, and the host identifies a disk
# by namespace UUID, so drift there is expected -- refusing on it would forbid
# the cross-SPDK upgrade the design allows.
rcow_version_gate_compare()
{
	local running="${1:-}" candidate="${2:-}"

	if [ ! -s "${running}" ]; then
		rcow_err "version gate: no running build-info document at \
'${running}'"
		return 1
	fi
	if [ ! -s "${candidate}" ]; then
		rcow_err "version gate: no candidate build-info document at \
'${candidate}'"
		return 1
	fi

	python3 - "${running}" "${candidate}" <<'PY'
import json, sys

# journal_op_max is here because the journal has no version field: see the note
# above this function.
EQUAL = ("super_version", "wal_version", "ckpt_version", "journal_op_max",
         "num_subsys", "ns_per_subsys", "nqn_prefix")

def load(path, side):
    try:
        with open(path) as f:
            d = json.load(f)
    except Exception as exc:
        print("%s build-info is unreadable (%s)" % (side, exc), file=sys.stderr)
        sys.exit(1)
    if not isinstance(d, dict):
        print("%s build-info is not a JSON object" % side, file=sys.stderr)
        sys.exit(1)
    return d

run = load(sys.argv[1], "the running target's")
new = load(sys.argv[2], "the candidate binary's")

bad = []
for field in EQUAL:
    absent = [side for side, doc in (("candidate", new), ("running", run))
              if field not in doc]
    if absent:
        # A field missing from both sides would compare equal and pass. That is
        # the worst answer a gate can give -- it reports the upgrade as checked
        # when nothing was compared -- so absence is always a refusal.
        bad.append("%s is missing from the %s build-info"
                   % (field, " and ".join(absent)))
    elif new[field] != run[field]:
        bad.append("%s differs: candidate %r, running %r"
                   % (field, new[field], run[field]))

# A build's export_version_max is its current export version, so the running
# side's max is the version the candidate has to accept.
lo, hi = new.get("export_version_min"), new.get("export_version_max")
cur = run.get("export_version_max")
if lo is None or hi is None:
    bad.append("export_version_min/max is missing from the candidate build-info")
elif cur is None:
    bad.append("export_version_max is missing from the running build-info")
elif not (lo <= cur <= hi):
    # Names the field, like every other refusal here: the caller has to know
    # which knob to move.
    bad.append("export_version: the running version %r is outside the "
               "candidate's [%r, %r]" % (cur, lo, hi))

print("version gate: running %s (%s) spdk %s -> candidate %s (%s) spdk %s"
      % (run.get("s3lvol_version", "?"), run.get("git_commit", "?"),
         run.get("spdk_version", "?"), new.get("s3lvol_version", "?"),
         new.get("git_commit", "?"), new.get("spdk_version", "?")),
      file=sys.stderr)

for reason in bad:
    print(reason, file=sys.stderr)
sys.exit(1 if bad else 0)
PY
}

# Gather both documents and compare them. Refuses, before anything is killed,
# when the running target cannot describe itself: a target older than this
# mechanism has no way to report its formats, so it cannot be hot-upgraded and
# must go through a cold stop -- the mechanism cannot bootstrap itself.
rcow_version_gate_check()
{
	local candidate="${1:-}" running_doc candidate_doc rc=0

	if [ -z "${candidate}" ]; then
		rcow_err "version gate: no candidate binary given"
		return 1
	fi
	if [ ! -x "${candidate}" ]; then
		rcow_err "version gate: '${candidate}' is not an executable"
		return 1
	fi

	rcow_ensure_run_dir
	running_doc="$(mktemp "${RCOW_RUN_DIR}/gate-running.XXXXXX")" || {
		rcow_err "version gate: cannot stage a document in ${RCOW_RUN_DIR}"
		return 1
	}
	candidate_doc="${running_doc}.candidate"

	if ! rcow_rpc rcow_get_build_info >"${running_doc}" 2>/dev/null; then
		rcow_err "version gate: the running target does not answer \
rcow_get_build_info. A target older than this mechanism cannot be hot-upgraded, \
because nothing can learn its on-disk formats before it is killed"
		rm -f "${running_doc}" "${candidate_doc}"
		return 1
	fi
	if ! "${candidate}" --print-build-info >"${candidate_doc}" 2>/dev/null; then
		rcow_err "version gate: '${candidate} --print-build-info' failed; \
refusing to hot-upgrade to a binary that cannot describe itself"
		rm -f "${running_doc}" "${candidate_doc}"
		return 1
	fi

	rcow_version_gate_compare "${running_doc}" "${candidate_doc}" || rc=1
	rm -f "${running_doc}" "${candidate_doc}"
	return "${rc}"
}

# Start the target detached and echo its pid.
#
# setsid, and stdin from /dev/null, because the target has to outlive the shell
# that started it: as an ordinary background job it stays in the caller's process
# group and session, and then it goes away with the ssh session, or with whatever
# cleans that group up -- observed, and it takes the WAL tail with it.
#
# The pid comes from the process itself rather than from $!. setsid execs in
# place when the calling shell has job control off and forks when it does not, so
# $! is the target in one case and a wrapper that has already exited in the
# other. Having the child announce its own pid before it execs removes the guess.
rcow_start_target_detached()
{
	local deadline pid

	rm -f "${RCOW_PIDFILE}"

	setsid bash -c '
		printf "%s\n" "$$" >"$1"
		shift
		exec "$@"
	' _ "${RCOW_PIDFILE}" "$@" </dev/null >>"${RCOW_LOG}" 2>&1 &

	deadline=$((SECONDS + 10))
	while [ "${SECONDS}" -lt "${deadline}" ]; do
		if [ -r "${RCOW_PIDFILE}" ]; then
			pid="$(tr -dc '0-9' <"${RCOW_PIDFILE}")"
			if [ -n "${pid}" ] && rcow_pid_is_target "${pid}"; then
				printf '%s' "${pid}"
				return 0
			fi
		fi
		sleep 0.1
	done

	return 1
}

# Apply the options that can only be set before the subsystems come up, then
# release the target to finish initialising.
#
# The target is started with --wait-for-rpc for exactly this: iobuf_set_options is
# a startup RPC, and the pools it sizes are allocated during subsystem init, so
# there is no later moment at which it could take effect. Everything else this
# module configures (transport, subsystems, bdevs) has to come after
# framework_start_init.
rcow_apply_startup_opts()
{
	local mib=$((RCOW_IOBUF_LARGE_BUFSIZE * RCOW_IOBUF_LARGE_POOL / 1024 / 1024))

	if ! rcow_srpc iobuf_set_options \
			--large-bufsize "${RCOW_IOBUF_LARGE_BUFSIZE}" \
			--large-pool-count "${RCOW_IOBUF_LARGE_POOL}" >/dev/null 2>&1; then
		rcow_err "iobuf_set_options failed (large-bufsize \
${RCOW_IOBUF_LARGE_BUFSIZE}, large-pool-count ${RCOW_IOBUF_LARGE_POOL})"
		return 1
	fi
	rcow_log "iobuf: large buffers ${RCOW_IOBUF_LARGE_BUFSIZE} B x \
${RCOW_IOBUF_LARGE_POOL} = ${mib} MiB, so a${RCOW_MAX_IO_SIZE} B host I/O \
arrives in one piece"

	# Not "start", despite the name: this is what runs subsystem init, which
	# --wait-for-rpc held back. It answers only when init is done, so a failure
	# here is a failure to initialise, not a failure to submit.
	if ! RCOW_RPC_TIMEOUT=60 rcow_srpc framework_start_init >/dev/null 2>&1; then
		rcow_err "framework_start_init failed; the target is up but has no \
subsystems"
		return 1
	fi

	return 0
}

# A target that answers RPCs is the only definition of "up" worth using: the
# process exists well before the RPC server does.
rcow_wait_rpc()
{
	local deadline=$((SECONDS + ${1:-30}))
	local pid="${2:-}"

	local last_err=""

	while [ "${SECONDS}" -lt "${deadline}" ]; do
		if last_err="$(RCOW_RPC_TIMEOUT=5 rcow_srpc spdk_get_version 2>&1)"; then
			return 0
		fi
		if [ -n "${pid}" ] && ! kill -0 "${pid}" 2>/dev/null; then
			[ -n "${last_err}" ] && rcow_err "last rpc.py output: ${last_err}"
			return 1
		fi
		sleep 0.2
	done
	if [ -n "${last_err}" ]; then
		rcow_err "rpc.py did not succeed; last output:"
		printf '%s\n' "${last_err}" | sed 's/^/    /' >&2
	fi
	return 1
}

# SIGTERM, then SIGKILL once the deadline passes. Reported separately because the
# difference matters: a target that had to be killed did not close its log, and
# the next attach will have a tail to replay.
rcow_kill_target()
{
	local pid="$1" timeout="${2:-30}" deadline

	kill -TERM "${pid}" 2>/dev/null || return 0

	deadline=$((SECONDS + timeout))
	while [ "${SECONDS}" -lt "${deadline}" ]; do
		kill -0 "${pid}" 2>/dev/null || return 0
		sleep 0.2
	done

	rcow_warn "pid ${pid} ignored SIGTERM for ${timeout}s; sending SIGKILL. \
The WAL tail will be replayed by the next attach"
	kill -KILL "${pid}" 2>/dev/null
	sleep 1
	kill -0 "${pid}" 2>/dev/null && return 1
	return 0
}

rcow_log_tail()
{
	if [ -r "${RCOW_LOG}" ]; then
		rcow_err "last ${1:-25} lines of ${RCOW_LOG}:"
		tail -n "${1:-25}" "${RCOW_LOG}" | sed 's/^/    /' >&2
	fi
}

rcow_log_size()
{
	stat -c %s "${RCOW_LOG}" 2>/dev/null || printf '0'
}

# --------------------------------------------------------------------------
# The owner marker
#
# An lvstore's prefix in S3 carries <lvs>/meta/owner, written on attach and
# removed on unload, and an attach that finds somebody else's marker fails with
# -EBUSY. It deliberately does not expire: expiry would need a liveness judgement
# that has no correct answer from the S3 side, and getting it wrong means two
# processes writing the same objects.
#
# Which leaves a crash needing a decision that only something outside S3 can
# make. On a single-owner node that decision is mechanical:
#
#   the marker names this host, and the pid it names is not running a target
#   -> the holder is a process that died, and forcing is safe
#
#   anything else -> refuse
#
# The holder's identity reaches only the target log (s3_owner.c:332 formats it
# there; the RPC returns a bare -EBUSY), so it is read from the log -- but only
# from the bytes appended by the attempt just made, so that a line left by an
# earlier attach cannot be mistaken for this one's. If the line is not found, the
# answer is "refuse": an unconfirmed force is exactly what the marker exists to
# prevent.
#
# Echoes the reasoning either way, so that the caller can log it.
# --------------------------------------------------------------------------
rcow_owner_is_stale()
{
	local since="${1:-0}" line node pid me

	line="$(tail -c "+$((since + 1))" "${RCOW_LOG}" 2>/dev/null |
		grep -o 'is already held by node=[^ ]* pid=[0-9]*' | tail -1)"

	if [ -z "${line}" ]; then
		printf 'the target log does not say who holds the marker'
		return 1
	fi

	node="${line#*node=}"; node="${node%% *}"
	pid="${line##*pid=}"
	me="$(hostname)"

	if [ "${node}" != "${me}" ]; then
		printf "the marker names node '%s', not this one (%s)" "${node}" "${me}"
		return 1
	fi
	if [ -z "${pid}" ] || [ "${pid}" = "0" ]; then
		printf 'the marker names no pid'
		return 1
	fi
	if rcow_pid_is_target "${pid}"; then
		printf 'pid %s on this node is still running a target' "${pid}"
		return 1
	fi

	printf "it was left by this node's pid %s, which is not running" "${pid}"
	return 0
}

# --------------------------------------------------------------------------
# NVMf subsystems
# --------------------------------------------------------------------------

rcow_subsys_count()
{
	rcow_srpc nvmf_get_subsystems 2>/dev/null | python3 -c '
import json, sys
try:
    subs = json.load(sys.stdin)
except ValueError:
    print(0)
    sys.exit(0)
print(sum(1 for s in subs if s.get("nqn", "").startswith(sys.argv[1])))
' "${RCOW_NQN_PREFIX}"
}

# Create the whole grid in one pass. Existing subsystems are left alone so that
# the step is repeatable; nvmf_create_subsystem on an existing NQN fails, and a
# failure there is indistinguishable from a real one.
#
# Listeners are deliberately not added here -- see rcow_add_listeners(), which
# has to run only once the namespaces are in place.
rcow_create_subsystems()
{
	local existing i nqn created=0
	local script=""

	# The grid is exported with -a (any host): on a non-loopback listener
	# that is an unauthenticated block device on the network. Refuse here
	# (fail closed) unless the listener is loopback -- install.sh preflight
	# already rejects this, this guard covers a manually edited runtime env.
	case "${RCOW_LISTEN_ADDR}" in
	127.*|localhost|::1) ;;
	*)
		rcow_err "RCOW_LISTEN_ADDR=${RCOW_LISTEN_ADDR} is not loopback but subsystems are exported with allow_any_host (-a); refusing to expose an unauthenticated block device. Keep 127.0.0.1 (or add a hostnqn allowlist to the export path first)"
		return 1
		;;
	esac

	existing="$(rcow_srpc nvmf_get_subsystems 2>/dev/null | python3 -c '
import json, sys
try:
    subs = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for s in subs:
    print(s.get("nqn", ""))
')"

	# The transport is per process, so it is gone with every restart. Creating
	# it twice logs an ERROR about a transport that already exists, which reads
	# like a failure in a recovery log, so it is only attempted when absent.
	#
	# An already-running transport is left as it is rather than adjusted: its
	# options are fixed at creation. If it was created elsewhere with the
	# default 128 KiB max_io_size, that is worth knowing about, hence the check.
	if ! rcow_srpc nvmf_get_transports 2>/dev/null | grep -qi '"TCP"'; then
		rcow_srpc nvmf_create_transport -t TCP -i "${RCOW_MAX_IO_SIZE}" \
			>/dev/null 2>&1 ||
			{ rcow_err "nvmf_create_transport -t TCP -i ${RCOW_MAX_IO_SIZE} \
failed. max_io_size divided by iobuf's large_bufsize must not exceed 16, the \
SGL entry limit (tcp.c:835)"; return 1; }
	else
		local have_mis
		have_mis="$(rcow_srpc nvmf_get_transports 2>/dev/null | python3 -c '
import json, sys
try:
    for t in json.load(sys.stdin):
        if t.get("trtype", "").upper() == "TCP":
            print(t.get("max_io_size", 0))
            break
except Exception:
    pass
' 2>/dev/null)"
		if [ -n "${have_mis}" ] && [ "${have_mis}" != "${RCOW_MAX_IO_SIZE}" ]; then
			rcow_warn "the TCP transport already exists with max_io_size \
${have_mis}, not ${RCOW_MAX_IO_SIZE}. Transport options are fixed at creation, \
so this run keeps the existing one"
		fi
	fi

	for i in $(seq 0 $((RCOW_NUM_SUBSYS - 1))); do
		nqn="$(rcow_nqn "${i}")"
		if printf '%s\n' "${existing}" | grep -qxF "${nqn}"; then
			continue
		fi

		# -a: any host may connect. The initiator is this same machine over
		#     loopback, and a hostnqn allowlist here would have to be kept
		#     in step with /etc/nvme/hostnqn on every boot.
		# -m: see the header comment -- without it nsid > 32 is refused.
		# -d: the model is pinned rather than left to SPDK's default, so it
		#     cannot drift when SPDK is upgraded.
		script="${script}nvmf_create_subsystem ${nqn} -a -s $(printf 'RCOW%014d' "${i}") -d ${RCOW_MODEL} -m ${RCOW_NS_PER_SUBSYS}
"
		created=$((created + 1))
	done

	if [ "${created}" -eq 0 ]; then
		rcow_log "all ${RCOW_NUM_SUBSYS} subsystems already exist"
		return 0
	fi

	if ! printf '%s' "${script}" | rcow_srpc_script; then
		rcow_err "creating the subsystem grid failed after $(rcow_subsys_count) of ${RCOW_NUM_SUBSYS}"
		return 1
	fi

	rcow_log "created ${created} subsystem(s); ${RCOW_NUM_SUBSYS} now exist, \
${RCOW_NS_PER_SUBSYS} namespace slots each"
	return 0
}

# Make the grid reachable. Split from rcow_create_subsystems() because of when
# this has to run rather than what it does: a listener is the moment a subsystem
# becomes reachable, and the host names its devices from what it finds at that
# moment.
#
# The Y in /dev/nvmeXnY is allocated by the host when it discovers a namespace;
# it is not the nsid -- a volume activated on nsid 1 has been observed coming up
# as nvme19n2. Open the listeners before the namespaces exist and the host
# attaches to empty subsystems, then learns of each namespace from an AEN, in
# whatever order the replay happened to add them. Open them afterwards and the
# host's first scan sees the finished layout and walks it in nsid order.
#
# That is also what makes a recovery follow the same sequence as a fresh start.
# An ordering that differs between the two is the one thing certain to move
# device names across a restart.
#
# Idempotent for the same reason as the grid above: adding a listener that is
# already there fails, and that failure is indistinguishable from a real one.
rcow_add_listeners()
{
	local existing i nqn added=0
	local script=""

	existing="$(rcow_srpc nvmf_get_subsystems 2>/dev/null | python3 -c '
import json, sys
addr, port = sys.argv[1], sys.argv[2]
try:
    subs = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for s in subs:
    for l in s.get("listen_addresses") or []:
        if (l.get("trtype", "").upper() == "TCP" and
                l.get("traddr", "") == addr and
                str(l.get("trsvcid", "")) == port):
            print(s.get("nqn", ""))
            break
' "${RCOW_LISTEN_ADDR}" "${RCOW_LISTEN_PORT}")"

	for i in $(seq 0 $((RCOW_NUM_SUBSYS - 1))); do
		nqn="$(rcow_nqn "${i}")"
		if printf '%s\n' "${existing}" | grep -qxF "${nqn}"; then
			continue
		fi

		script="${script}nvmf_subsystem_add_listener ${nqn} -t tcp -a ${RCOW_LISTEN_ADDR} -s ${RCOW_LISTEN_PORT}
"
		added=$((added + 1))
	done

	if [ "${added}" -eq 0 ]; then
		rcow_log "all ${RCOW_NUM_SUBSYS} subsystems already listen on \
${RCOW_LISTEN_ADDR}:${RCOW_LISTEN_PORT}"
		return 0
	fi

	if ! printf '%s' "${script}" | rcow_srpc_script; then
		rcow_err "adding the listeners failed; ${added} of ${RCOW_NUM_SUBSYS} \
were missing on ${RCOW_LISTEN_ADDR}:${RCOW_LISTEN_PORT}"
		return 1
	fi

	rcow_log "added ${added} listener(s); all ${RCOW_NUM_SUBSYS} subsystems \
now listen on ${RCOW_LISTEN_ADDR}:${RCOW_LISTEN_PORT}"
	return 0
}

# --------------------------------------------------------------------------
# Host side
# --------------------------------------------------------------------------

rcow_connected_nqns()
{
	local f
	for f in /sys/class/nvme/nvme*/subsysnqn; do
		[ -r "${f}" ] && cat "${f}"
	done 2>/dev/null
}

# Connect every subsystem at startup, empty ones included. A namespace added
# later arrives as an AEN on an existing controller, which the host picks up on
# its own; connecting on demand instead would mean a fabric login in the latency
# path of every volume attach.
rcow_connect_all()
{
	local present i nqn already=0 connected=0 failed=0

	present="$(rcow_connected_nqns)"

	for i in $(seq 0 $((RCOW_NUM_SUBSYS - 1))); do
		nqn="$(rcow_nqn "${i}")"
		if printf '%s\n' "${present}" | grep -qxF "${nqn}"; then
			already=$((already + 1))
			continue
		fi
		# The timeouts are set at connect time for a fresh start; the upgrade
		# path writes the same values to sysfs on controllers that already
		# exist (rcow_tune_initiator_timeouts), because connect options are
		# fixed for a connection's lifetime. fast_io_fail_tmo is deliberately
		# never passed: enabled, it turns every pause into a fast failure,
		# which is the opposite of what this whole path is for.
		if nvme connect -t tcp -a "${RCOW_LISTEN_ADDR}" \
				-s "${RCOW_LISTEN_PORT}" -n "${nqn}" \
				-i "${RCOW_IO_QUEUES}" \
				--reconnect-delay "${RCOW_RECONNECT_DELAY}" \
				--ctrl-loss-tmo "${RCOW_CTRL_LOSS_TMO}" >/dev/null 2>&1; then
			connected=$((connected + 1))
		else
			rcow_warn "nvme connect failed for ${nqn}"
			failed=$((failed + 1))
		fi
	done

	rcow_log "initiator: ${connected} connected, ${already} already up, \
${failed} failed (${RCOW_IO_QUEUES} io queues each)"
	[ "${failed}" -eq 0 ]
}

rcow_disconnect_all()
{
	local i nqn gone=0

	# Let an in-flight re-probe finish first. Disconnecting under one leaves
	# "Buffer I/O error ... async page read" in dmesg, which is teardown noise
	# that reads exactly like data loss.
	udevadm settle --timeout=5 >/dev/null 2>&1 || true

	for i in $(seq 0 $((RCOW_NUM_SUBSYS - 1))); do
		nqn="$(rcow_nqn "${i}")"
		if nvme disconnect -n "${nqn}" >/dev/null 2>&1; then
			gone=$((gone + 1))
		fi
	done

	rcow_log "initiator: disconnected ${gone} subsystem(s)"
	return 0
}

# Write the initiator timeouts onto the controllers that already exist, for an
# upgrade that is about to kill the target while leaving the connections up.
#
# === Why the write order is load-bearing ===
#
# What bounds the retry budget is not ctrl_loss_tmo's value but the count
# max_reconnects, and only the ctrl_loss_tmo store recomputes it:
#
#     opts->max_reconnects = DIV_ROUND_UP(ctrl_loss_tmo, opts->reconnect_delay);
#
# The reconnect_delay store changes the delay and leaves the count alone. So the
# delay has to be written first and the timeout second, so the recompute sees
# the new delay. The other order leaves the old count in place and collapses the
# budget -- a 1800s setting under the default 10s delay becomes 180s -- silently,
# and at exactly the moment the budget is needed. The read-back is what makes a
# wrong order observable: the ctrl_loss_tmo show method returns
# max_reconnects * reconnect_delay, so the count is visible in the value.
#
# === Why reconnect_delay is clamped to >= 1 ===
#
# Its store is kstrtou32 with no lower bound, so 0 is accepted. Zero is a
# zero-delay retry storm that spends max_reconnects in milliseconds, and it makes
# the kernel divide by zero on the next ctrl_loss_tmo write. It never reaches
# sysfs from here.
#
# Return codes: 0 tuned, 1 refused (bad input, a failed assertion, or an enabled
# fast_io_fail_tmo), 2 the kernel exports neither attribute (pre-5.7), so the
# host runs on its own default however this is configured: a rollback deadline
# derived from this system's ctrl_loss_tmo has to start from
# RCOW_MIN_CTRL_LOSS_TMO instead.
rcow_tune_initiator_timeouts()
{
	local delay="${RCOW_RECONNECT_DELAY}"
	local tmo="${RCOW_CTRL_LOSS_TMO}"
	local soft=0
	local d nqn rd clt fif got_rd got_clt exp tuned=0
	local -a ctrls=()

	while [ "$#" -gt 0 ]; do
		case "$1" in
		--reconnect-delay) shift; delay="${1:-}" ;;
		--ctrl-loss-tmo)   shift; tmo="${1:-}" ;;
		--soft)            soft=1 ;;
		*) rcow_err "rcow_tune_initiator_timeouts: unknown argument: $1"
		   return 1 ;;
		esac
		shift
	done

	case "${delay}" in
	''|*[!0-9]*)
		if [ "${soft}" -eq 1 ]; then
			rcow_warn "reconnect_delay='${delay}' is not a positive \
integer; clamping to 1 (--soft)"
			delay=1
		else
			rcow_err "refusing reconnect_delay='${delay}': it must be a \
positive integer. sysfs accepts 0, and 0 is a retry storm plus a divide-by-zero \
in the kernel's next ctrl_loss_tmo write"
			return 1
		fi
		;;
	*)
		if [ "${delay}" -lt 1 ]; then
			if [ "${soft}" -eq 1 ]; then
				rcow_warn "reconnect_delay=0 clamped to 1 (--soft)"
				delay=1
			else
				rcow_err "refusing reconnect_delay=0: sysfs accepts \
it, and it is a retry storm plus a divide-by-zero in the kernel's next \
ctrl_loss_tmo write"
				return 1
			fi
		fi
		;;
	esac

	case "${tmo}" in
	''|*[!0-9]*)
		rcow_err "refusing ctrl_loss_tmo='${tmo}': it must be a positive \
integer"
		return 1
		;;
	esac
	if [ "${tmo}" -lt 1 ]; then
		rcow_err "refusing ctrl_loss_tmo=0"
		return 1
	fi

	# Only the controllers this deployment created. A controller somebody else
	# connected must not be re-tuned from here.
	for d in /sys/class/nvme/nvme*; do
		[ -d "${d}" ] || continue
		nqn="$(cat "${d}/subsysnqn" 2>/dev/null)" || continue
		case "${nqn}" in
		"${RCOW_NQN_PREFIX}"*) ctrls+=("${d}") ;;
		esac
	done

	if [ "${#ctrls[@]}" -eq 0 ]; then
		rcow_log "no ${RCOW_NQN_PREFIX}* controller to tune"
		return 0
	fi

	# Per-controller files, but they arrive with the kernel, so probing one
	# answers for all.
	if [ ! -e "${ctrls[0]}/reconnect_delay" ] &&
	   [ ! -e "${ctrls[0]}/ctrl_loss_tmo" ]; then
		rcow_warn "this kernel does not export reconnect_delay / \
ctrl_loss_tmo (pre-5.7); the real rollback budget is the kernel's 600s default, \
not ${tmo}s"
		return 2
	fi

	# The count the kernel will hold for this (delay, tmo), and so the value
	# the read-back must equal.
	exp=$(( (tmo + delay - 1) / delay * delay ))

	for d in "${ctrls[@]}"; do
		rd="${d}/reconnect_delay"
		clt="${d}/ctrl_loss_tmo"

		# Enabled, fast_io_fail_tmo fails every I/O the moment the
		# transport drops -- the exact opposite of pausing until the target
		# comes back.
		if [ -e "${d}/fast_io_fail_tmo" ]; then
			fif="$(cat "${d}/fast_io_fail_tmo" 2>/dev/null)"
			case "${fif}" in
			''|0|off|-1|none) ;;
			*)
				rcow_err "${d}/fast_io_fail_tmo is '${fif}'; it \
must stay disabled or every pause becomes a fast failure"
				return 1
				;;
			esac
		fi

		# reconnect_delay first -- see the header. The read-back below
		# detects a swapped order.
		if ! printf '%s\n' "${delay}" >"${rd}" 2>/dev/null; then
			rcow_err "could not write reconnect_delay=${delay} to ${rd}"
			return 1
		fi
		if ! printf '%s\n' "${tmo}" >"${clt}" 2>/dev/null; then
			rcow_err "could not write ctrl_loss_tmo=${tmo} to ${clt}"
			return 1
		fi

		got_rd="$(cat "${rd}" 2>/dev/null)"
		got_clt="$(cat "${clt}" 2>/dev/null)"

		if [ "${got_rd}" != "${delay}" ]; then
			rcow_err "reconnect_delay on ${d} read back as \
'${got_rd}', not '${delay}': the store did not keep what it accepted"
			return 1
		fi
		if [ "${got_clt}" != "${exp}" ]; then
			rcow_err "ctrl_loss_tmo on ${d} read back as '${got_clt}', \
expected '${exp}' (= max_reconnects * reconnect_delay). Either the write order \
was wrong or the kernel disagreed; the retry budget is not what was asked for"
			return 1
		fi

		tuned=$((tuned + 1))
	done

	rcow_log "initiator: tuned ${tuned} controller(s) to \
reconnect_delay=${delay}s, ctrl_loss_tmo=${tmo}s"
	return 0
}

# --------------------------------------------------------------------------
# bstore.json
# --------------------------------------------------------------------------

# Echo "<namespace> <wal_bdev>" for an lvstore, or nothing when it is not
# recorded. Nothing is what decides on create rather than attach, so it is read
# from this file only.
rcow_bstore_entry()
{
	[ -s "${RCOW_BSTORE_FILE}" ] || return 1

	python3 - "${RCOW_BSTORE_FILE}" "$1" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
e = d.get(sys.argv[2])
if not isinstance(e, dict):
    sys.exit(1)
print("%s %s" % (e.get("ns_name", ""), e.get("wal_bdev", "")))
PY
}

# --------------------------------------------------------------------------
# The active registry
# --------------------------------------------------------------------------

# One line per entry: name, subsys, nsid, uuid, tab separated. Tabs because a
# volume name may contain almost anything else.
rcow_registry_tsv()
{
	[ -s "$1" ] || return 0

	python3 - "$1" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception as e:
    print("cannot parse %s: %s" % (sys.argv[1], e), file=sys.stderr)
    sys.exit(1)
for name, e in d.items():
    print("%s\t%s\t%s\t%s" % (name, e.get("subsys"), e.get("nsid"),
                              e.get("uuid", "")))
PY
}

# "<name>\t<uuid>" for every lvol and snapshot the target currently has. Used to
# tell a replay of the recorded volume from a replay onto a different volume that
# has since taken its name.
rcow_volume_uuids()
{
	rcow_rpc rcow_get_lvstores 2>/dev/null | python3 -c '
import json, sys
try:
    stores = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for st in stores:
    for lv in st.get("lvols", []):
        print("%s\t%s" % (lv.get("name", ""), lv.get("uuid", "")))
'
}

# Keep only the named entries in a registry file; remove the file when none are
# left. Used to shrink the replay plan to what still has to be done.
rcow_registry_keep()
{
	local file="$1"; shift
	local tmp="${file}.tmp.$$"

	if [ "$#" -eq 0 ]; then
		rm -f "${file}"
		return 0
	fi

	if python3 - "${file}" "${tmp}" "$@" <<'PY'
import json, os, sys
src, dst, keep = sys.argv[1], sys.argv[2], set(sys.argv[3:])
try:
    d = json.load(open(src))
except Exception:
    d = {}
d = {k: v for k, v in d.items() if k in keep}
if not d:
    sys.exit(2)
with open(dst, "w") as f:
    json.dump(d, f, indent=2)
    f.flush()
    os.fsync(f.fileno())
PY
	then
		mv -f "${tmp}" "${file}"
	else
		rm -f "${tmp}" "${file}"
	fi
	return 0
}

# Re-attach every volume the previous process had exposed, at exactly the
# subsystem and nsid it was on.
#
# === Why the registry is moved aside first ===
#
# rcow_active_bdev short-circuits when the name is already in the registry: it
# reports where the volume is and attaches nothing. That is correct inside the
# process that attached it -- activation has to be idempotent, because a caller
# that timed out will retry -- and wrong for a process that has just started,
# where the entry describes a namespace that no longer exists. Replaying against
# a populated registry would therefore report every volume as restored while
# attaching none of them.
#
# So the plan is a copy and the live registry is deleted before the first
# activation call of this process. The module reads the file lazily, on the first
# activation RPC, which is what makes the ordering enough -- nothing has read it
# yet at this point.
#
# The copy is what makes the deletion safe: dying between the two leaves the plan
# behind, and the next run finds it and repeats. Dying part way through leaves
# the plan holding what is left.
#
# === Why the activation calls are batched ===
#
# One rcow_active_bdev per volume is one python interpreter and one round-trip
# per volume, all inside the pause window. rcow_active_bdev_batch restores them
# in a single round-trip. It does
# not shorten any one namespace's pause: the nvmf pause names a single nsid, so
# each volume is added under its own pause and volumes on one subsystem pay one
# pause each. Only the round-trip is saved.
#
# The batch is not transactional -- earlier successes stay and the failures are
# named. That is deliberate: the caller's retry is idempotent, and rolling a
# successful attach back would be worse.
rcow_replay_registry()
{
	local plan="${RCOW_REPLAY_FILE}"
	local uuid_map name subsys nsid want_uuid have_uuid out payload res first row
	local -a accepted=()
	local -a pending=()
	local done_n=0 refused_n=0 failed_n=0

	if [ -s "${plan}" ]; then
		rcow_warn "${plan} exists: a previous replay did not finish. \
Continuing from it"
	elif [ -s "${RCOW_ACTIVE_FILE}" ]; then
		if ! cp -f "${RCOW_ACTIVE_FILE}" "${plan}.tmp" ||
		   ! mv -f "${plan}.tmp" "${plan}"; then
			rcow_err "could not stage the replay plan at ${plan}; \
refusing to touch ${RCOW_ACTIVE_FILE}"
			rm -f "${plan}.tmp"
			return 1
		fi
		if ! rcow_fsync_file "${plan}"; then
			rcow_err "could not make ${plan} durable; refusing to \
touch ${RCOW_ACTIVE_FILE}"
			return 1
		fi
	else
		rcow_log "nothing was recorded as active; no replay needed"
		return 0
	fi

	if ! rcow_registry_tsv "${plan}" >/dev/null; then
		rcow_err "${plan} does not parse. It is not deleted -- move it aside \
by hand once you have decided what it should have said"
		return 1
	fi

	rm -f "${RCOW_ACTIVE_FILE}"

	uuid_map="$(mktemp "${RCOW_RUN_DIR}/uuids.XXXXXX")" || return 1
	rcow_volume_uuids >"${uuid_map}"

	while IFS=$'\t' read -r name subsys nsid want_uuid; do
		[ -n "${name}" ] || continue

		have_uuid="$(awk -F'\t' -v n="${name}" '$1 == n { print $2; exit }' \
			"${uuid_map}")"

		# Dropped rather than retried: neither of these becomes true later,
		# and keeping them in the plan would mean it never completes.
		if [ -z "${have_uuid}" ]; then
			rcow_warn "'${name}' was active at subsys ${subsys} nsid \
${nsid} but no longer exists in the lvstore; not restored"
			refused_n=$((refused_n + 1))
			continue
		fi
		if [ "${have_uuid}" != "${want_uuid}" ]; then
			rcow_warn "'${name}' was active with uuid ${want_uuid} but the \
volume of that name now has uuid ${have_uuid}. A different volume has taken the \
name; not restored, because the host would mount it believing it was the old one"
			refused_n=$((refused_n + 1))
			continue
		fi

		# subsys and nsid are carried along explicitly. Letting them be
		# recomputed would put a volume that had been moved somewhere else,
		# and the whole point of the registry is that the layout comes back
		# unchanged.
		accepted+=("${name}"$'\t'"${subsys}"$'\t'"${nsid}")
	done < <(rcow_registry_tsv "${plan}")

	rm -f "${uuid_map}"

	if [ "${#accepted[@]}" -gt 0 ]; then
		# python builds the payload: a volume name can hold a quote or a
		# backslash, and the printf-concatenated form this replaces would then
		# produce malformed JSON. rcow_rpc forwards exactly one argument, so
		# the payload has to be one string.
		payload="$(printf '%s\n' "${accepted[@]}" | python3 -c '
import json, sys
vols = []
for line in sys.stdin:
    line = line.rstrip("\n")
    if not line:
        continue
    name, subsys, nsid = line.split("\t")
    vols.append({"device_name": name, "subsys": int(subsys),
                 "nsid": int(nsid)})
print(json.dumps({"volumes": vols}))
')"

		# A payload that could not be built is treated as a reply that could
		# not be read: every volume goes back to pending. It is not sent at
		# all, because an empty params argument reaches the target as "no
		# params" and its refusal would read as the target's fault.
		if [ -z "${payload}" ]; then
			out="the batch payload could not be built"
			res="PARSE_FAIL"
		else
			out="$(rcow_rpc rcow_active_bdev_batch "${payload}" 2>&1)"

			# The reply travels as string_value on both streams, so what is
			# parsed below is the target's own result object whatever the exit
			# status was.
			res="$(printf '%s' "${out}" | python3 -c '
import json, sys
try:
    r = json.load(sys.stdin)
    if not isinstance(r, dict) or not {"restored", "already_active",
                                       "failed"} <= set(r):
        raise ValueError
except Exception:
    print("PARSE_FAIL")
    sys.exit(0)
print("%d\t%d" % (int(r.get("restored", 0)),
                  int(r.get("already_active", 0))))
for f in r.get("failed") or []:
    print("F\t%s" % (f.get("device_name", "")))
')"
		fi

		case "${res}" in
		PARSE_FAIL*|'')
			# Not the batch object: an older target answers -32601, one
			# that cannot run answers a transport error, and neither text
			# is a result. Every volume in the batch goes back to pending;
			# dropping one because the reply was unreadable is the exact
			# failure this path exists to prevent, and the retry is
			# idempotent.
			rcow_warn "the rcow_active_bdev_batch reply could not be read \
as a result (${out}); treating all ${#accepted[@]} volume(s) as pending rather \
than dropping any"
			for row in "${accepted[@]}"; do
				pending+=("${row%%$'\t'*}")
			done
			failed_n=$((failed_n + ${#accepted[@]}))
			;;
		*)
			first="$(printf '%s\n' "${res}" | head -n 1)"
			done_n=$(( ${first%%$'\t'*} + ${first##*$'\t'} ))
			while IFS=$'\t' read -r row name; do
				[ "${row}" = "F" ] || continue
				pending+=("${name}")
				failed_n=$((failed_n + 1))
			done < <(printf '%s\n' "${res}" | tail -n +2)
			;;
		esac
	fi

	if [ "${#pending[@]}" -eq 0 ]; then
		rcow_registry_keep "${plan}"
	else
		rcow_registry_keep "${plan}" "${pending[@]}"
	fi

	rcow_log "replay: ${done_n} restored, ${refused_n} refused, \
${failed_n} failed"

	if [ "${failed_n}" -gt 0 ]; then
		rcow_err "${failed_n} volume(s) could not be restored; they are still \
listed in ${plan} and will be retried by the next start or by rcow_recovery.sh"
		return 1
	fi
	return 0
}

# Every recorded volume has to resolve to a block device that can actually be
# opened. An entry that does not is the failure this whole mechanism exists to
# prevent: the registry says the volume is exposed and the host cannot see it.
#
# === Why the path is tested with -b and not merely for being non-empty ===
#
# The path is not stored or predicted; the module looks it up by matching the lvol
# uuid against the namespace uuid in sysfs (vbdev_s3lvol_active.c). So a non-empty
# answer means *sysfs* has the namespace -- and the device node under /dev is
# created afterwards, by udev. The gap between the two is small and easy to miss:
# it took running the whole test suite back to back, on a busier machine than a
# single run leaves behind, for a dd to hit "No such file or directory" on a path
# this function had just declared present.
#
# Which matters beyond the tests, because rcow_start.sh calls this after a replay
# and logs "all N active volume(s) resolve to a block device" on the strength of
# it. Anything reading that line and opening the device would race udev.
#
# With --expect the same poll is a precondition rather than the answer: once the
# devices exist, the live layout is compared byte for byte against the snapshot
# rcow_upgrade.sh took before the upgrade. The upgrade is only allowed to be
# invisible, so a volume that moved subsystem, changed nsid, came back under a
# different device node, or gained company is as much a failure as one that did
# not come back at all -- which is why an extra live entry fails too.
rcow_verify_active()
{
	local timeout="30" expect="" live_tmp
	local deadline missing total out name path

	while [ "$#" -gt 0 ]; do
		case "$1" in
		--expect) shift; expect="${1:-}"
			# An empty value would read as "no comparison asked for" below, and
			# the caller asking for one is exactly the caller that must not get
			# it skipped.
			[ -n "${expect}" ] || rcow_die "--expect needs a snapshot path" ;;
		*) timeout="$1" ;;
		esac
		shift
	done
	[ -n "${timeout}" ] || timeout=30

	deadline=$((SECONDS + timeout))
	while :; do
		out="$(rcow_rpc rcow_get_bdev '{}' 2>/dev/null)" || return 1

		missing=""
		while IFS=$'\t' read -r name path; do
			[ -n "${name}" ] || continue
			if [ -z "${path}" ]; then
				missing="${missing}${name}: no device path yet"$'\n'
			elif [ ! -b "${path}" ]; then
				missing="${missing}${name}: ${path} is in sysfs but not yet a \
block device"$'\n'
			fi
		done < <(printf '%s' "${out}" | python3 -c '
import json, sys
try:
    entries = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for e in entries:
    print("%s\t%s" % (e.get("device_name", "?"), e.get("device_path") or ""))
')

		total="$(printf '%s' "${out}" | python3 -c \
			'import json,sys; print(len(json.load(sys.stdin)))' 2>/dev/null || echo 0)"

		[ -z "${missing}" ] && break
		[ "${SECONDS}" -ge "${deadline}" ] && break
		sleep 1
	done

	if [ -n "${missing}" ]; then
		rcow_err "these active volumes did not become usable block devices \
within ${timeout}s:"
		printf '%s' "${missing}" | sed 's/^/    /' >&2
		return 1
	fi

	rcow_log "all ${total} active volume(s) resolve to an openable block device"

	if [ -z "${expect}" ]; then
		return 0
	fi

	# python rather than bash: comparing two JSON documents field by field in
	# shell is a bug waiting to happen, and the differing rows have to name
	# which field moved.
	live_tmp="$(mktemp "${RCOW_RUN_DIR}/verify.XXXXXX")" || {
		rcow_err "could not stage the live layout for comparison"
		return 1
	}
	printf '%s' "${out}" >"${live_tmp}"

	if ! python3 - "${expect}" "${live_tmp}" <<'PY'
import json, sys

FIELDS = ("device_name", "uuid", "subsys", "nsid", "device_path")

def load(path):
    with open(path) as f:
        return json.load(f)

def keyed(entries):
    return {e.get("device_name", ""): e for e in entries}

try:
    want = keyed(load(sys.argv[1]))
    live = keyed(load(sys.argv[2]))
except Exception as e:
    print("cannot compare layouts: %s" % e, file=sys.stderr)
    sys.exit(2)

bad = 0
for name in sorted(want):
    if name not in live:
        print("%s: in the pre-upgrade snapshot but missing now" % name,
              file=sys.stderr)
        bad += 1
        continue
    for f in FIELDS:
        if want[name].get(f) != live[name].get(f):
            print("%s: %s changed: %r -> %r" % (
                name, f, want[name].get(f), live[name].get(f)),
                file=sys.stderr)
            bad += 1
for name in sorted(live):
    if name not in want:
        print("%s: appeared after the upgrade; it was not in the snapshot"
              % name, file=sys.stderr)
        bad += 1

sys.exit(1 if bad else 0)
PY
	then
		rm -f "${live_tmp}"
		rcow_err "the live layout does not match the pre-upgrade snapshot \
${expect}; the volumes above differ"
		return 1
	fi
	rm -f "${live_tmp}"

	rcow_log "layout matches ${expect}: every volume is on the same subsystem, \
nsid and device"
	return 0
}

# Apply RCOW_READ_AHEAD_KB to every active volume's block device.
#
# **Normally redundant, and kept for the cases where it is not.** The target
# applies the same value itself, from S3LVOL_READ_AHEAD_KB which this file
# exports, as soon as a volume resolves to a device -- so an ordinary activation
# is tuned before anyone can use it, with no cooperation from the caller. What
# this covers is volumes that were already active when that value changed, and
# hosts where the target was started without this environment.
#
# Best effort throughout: this is a performance knob, and a volume that could not
# be tuned still works. So a missing sysfs file, a device that disappeared
# between the listing and the write, or a read-only /sys are all warnings at
# most -- never a reason to fail a start that otherwise succeeded.
#
# Safe and idempotent to re-run. Does nothing when the tuning is disabled with 0.
rcow_tune_readahead()
{
	local out name path want cur sysfile msg tuned=0 skipped=0 failed=0

	want="${RCOW_READ_AHEAD_KB}"

	case "${want}" in
	''|*[!0-9]*)
		rcow_warn "RCOW_READ_AHEAD_KB='${want}' is not a number; \
leaving readahead alone"
		return 0
		;;
	0)
		# Explicitly disabled. Silent: an operator who set 0 does not need
		# to be told about it on every start.
		return 0
		;;
	esac

	out="$(rcow_rpc rcow_get_bdev '{}' 2>/dev/null)" || {
		rcow_warn "could not list active volumes; readahead left at the \
kernel default"
		return 0
	}

	while IFS=$'\t' read -r name path; do
		[ -n "${name}" ] || continue
		[ -n "${path}" ] || { skipped=$((skipped + 1)); continue; }

		# /sys/class/block/<leaf> rather than /sys/block/<leaf>: the latter
		# does not exist for a partition, and while nothing here hands out
		# partitions today, the class path is correct for both.
		sysfile="/sys/class/block/${path##*/}/queue/read_ahead_kb"

		if [ ! -w "${sysfile}" ]; then
			skipped=$((skipped + 1))
			continue
		fi

		cur="$(cat "${sysfile}" 2>/dev/null || echo)"
		[ "${cur}" = "${want}" ] && { tuned=$((tuned + 1)); continue; }

		if printf '%s\n' "${want}" >"${sysfile}" 2>/dev/null; then
			tuned=$((tuned + 1))
		else
			failed=$((failed + 1))
		fi
	done < <(printf '%s' "${out}" | python3 -c '
import json, sys
try:
    entries = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for e in entries:
    print("%s\t%s" % (e.get("device_name", "?"), e.get("device_path") or ""))
')

	if [ "${tuned}" -eq 0 ] && [ "${failed}" -eq 0 ] && [ "${skipped}" -eq 0 ]; then
		return 0
	fi

	msg="readahead ${want} KiB: ${tuned} volume(s) set"
	[ "${skipped}" -gt 0 ] && msg="${msg}, ${skipped} skipped"
	[ "${failed}" -gt 0 ] && msg="${msg}, ${failed} failed"
	rcow_log "${msg}"

	if [ "${failed}" -gt 0 ]; then
		rcow_warn "some volumes kept the kernel default readahead; \
sequential reads on those will issue one S3 request per 128 KiB"
	fi

	return 0
}

# --------------------------------------------------------------------------
# Resolving the lvstore name
#
# Placed at the very bottom because it needs rcow_bstore_entry() and the logging
# helpers; the variable itself is declared up with the rest of the configuration.
#
# Why per-node at all: the name is the key prefix (see the comment at the
# declaration), so nodes sharing a bucket must not share a name. Nothing in the
# module enforces that -- the owner marker under `<prefix>/meta/owner` catches the
# cases that happen one after another (a second node attaching while the first
# holds it, or after the first crashed), but s3_owner.h is explicit that it cannot
# catch two nodes acquiring at the same time, and a clean shutdown *releases* the
# marker, so it is not there to catch a same-named node starting later. Per §1.3 of
# the design doc, single-writer is an upper-layer guarantee. This is that layer
# doing its part.
#
# A truncated digest of the identity hostname is what makes it per-node: stable
# across reboots, needs no coordination, and does not leak the hostname into
# bucket keys. DNS names still use hostname -s (the label before the first
# dot) so an existing one-click prefix survives a domain-suffix change.
# Addresses and dotted hostnames with a numeric short name (192.0.2.48,
# 192.0.2.48.internal -> short name "192", one prefix per /8) hash in full.
#
# On collisions, honestly: 4 bytes is 32 bits, so for N nodes in one bucket the
# birthday probability is roughly N^2 / 2^33 -- about 1 in 8600 at a thousand
# nodes, and about 1.2% at ten thousand. That is not negligible at scale, and a
# collision is exactly the corruption described above. Two things to know:
#
#   - it is not silent if the colliding node is the *second* one to start, because
#     the owner marker is present and attach refuses with -EBUSY;
#   - it *is* silent when the first node is cleanly stopped and the second starts
#     afterwards with no bstore.json entry of its own, which is the create path.
#     Closing that needs a "is this prefix empty" check before create; it is not
#     implemented, and widening the hash does not substitute for it.
#
# Use RCOW_LVS_NAME to pin a name from the orchestrator if you have real node ids;
# a name you assign beats a hash of a hostname you happen to have.
# --------------------------------------------------------------------------

# What a node created before the name became per-node has its data under.
RCOW_LVS_NAME_LEGACY="${RCOW_LVS_NAME_LEGACY:-rcow}"

# First 4 bytes of the digest, as 8 hex characters -- the same shape as the
# generated bs_name in vbdev_s3lvol_bstore.c, which takes 8 hex characters of a
# uuid. sha256 rather than md5 only to keep a non-cryptographic identifier clear of
# crypto-policy arguments; nothing here depends on the digest's strength.
#
# printf, not echo: the digest must be of the hostname alone, with no trailing
# newline, or it changes depending on who recomputes it.
#
# rcow_node_hash [id]: hash an explicit identity, or this machine's
# rcow_identity_host when no argument is given.

# IPv4 (four decimal fields) or IPv6 (hostnames cannot contain ':').
# Deliberately loose: octets are not range-checked.
rcow_is_ip_hostname()
{
	local s="${1:-}"
	local oct

	[ -n "${s}" ] || return 1
	case "${s}" in
	*:*)
		return 0
		;;
	esac

	local IFS='.'
	# shellcheck disable=SC2086
	set -- ${s}
	[ "$#" -eq 4 ] || return 1
	for oct; do
		case "${oct}" in
		''|*[!0-9]*)
			return 1
			;;
		esac
	done
	return 0
}

# Identity string to hash, given the full hostname and hostname -s.
rcow_identity_host_from()
{
	local full="${1:-}"
	local short="${2:-}"

	if [ -z "${full}" ] && [ -z "${short}" ]; then
		return 1
	fi
	if rcow_is_ip_hostname "${full}"; then
		printf '%s' "${full}"
		return 0
	fi
	# hostname -s of 192.0.2.48.internal is "192" -- the same collision as a
	# bare IPv4, with a search domain appended.
	case "${short}" in
	''|*[!0-9]*)
		;;
	*)
		case "${full}" in
		*.*)
			printf '%s' "${full}"
			return 0
			;;
		esac
		;;
	esac
	if [ -n "${short}" ]; then
		printf '%s' "${short}"
		return 0
	fi
	printf '%s' "${full}"
}

rcow_identity_host()
{
	local full short

	full="$(hostname 2>/dev/null || true)"
	short="$(hostname -s 2>/dev/null || true)"
	rcow_identity_host_from "${full}" "${short}"
}

rcow_node_hash()
{
	local host

	command -v sha256sum >/dev/null 2>&1 || {
		rcow_err "sha256sum is missing, so the per-node lvstore name cannot be"
		rcow_err "derived; set RCOW_LVS_NAME explicitly"
		return 1
	}

	if [ "$#" -ge 1 ]; then
		host="${1}"
	else
		host="$(rcow_identity_host)" || true
	fi
	if [ -z "${host}" ]; then
		# Refusing rather than falling back to a constant: a fallback would give
		# every affected node the same prefix, which is the failure this whole
		# mechanism exists to prevent.
		rcow_err "identity hostname is empty, so a per-node lvstore name cannot"
		rcow_err "be derived; set RCOW_LVS_NAME explicitly"
		return 1
	fi

	printf '%s' "${host}" | sha256sum | cut -c1-8
}

rcow_resolve_lvs_name()
{
	local derived hash

	if [ "${RCOW_LVS_NAME_EXPLICIT}" -eq 1 ]; then
		return 0
	fi

	hash="$(rcow_node_hash)" || return 1
	derived="rcow-${hash}"

	# Adopting the legacy name when it is the one with data. Without this, the
	# first start after this change looks for an entry that cannot exist, finds
	# none, and takes the create path -- which formats the WAL and lays a fresh
	# super block over a prefix that still holds a live lvstore. The upgrade has
	# to be silent-by-default in the other direction: keep using what is there.
	if rcow_bstore_entry "${derived}" >/dev/null 2>&1; then
		RCOW_LVS_NAME="${derived}"
	elif rcow_bstore_entry "${RCOW_LVS_NAME_LEGACY}" >/dev/null 2>&1; then
		RCOW_LVS_NAME="${RCOW_LVS_NAME_LEGACY}"
		rcow_warn "using the pre-existing lvstore '${RCOW_LVS_NAME_LEGACY}' \
rather than this node's derived name '${derived}'"
		rcow_warn "that entry in ${RCOW_BSTORE_FILE} is the only record of where \
the data is; renaming would orphan the prefix, so it is kept"
		rcow_warn "to migrate: create '${derived}' on a node whose bucket has no \
'${RCOW_LVS_NAME_LEGACY}' prefix, or move the volumes with export/import"
	else
		# Nothing recorded either way: a genuinely new node.
		RCOW_LVS_NAME="${derived}"
	fi

	return 0
}

rcow_resolve_lvs_name || exit 1
