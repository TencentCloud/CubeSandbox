/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol internal interface
 *
 *   Layering boundary: **the global bdev registry is the sole contract**.
 *
 *     lib/blob          reused verbatim, zero changes
 *     lib/lvol          reused verbatim, zero changes -- spdk_lvs_init takes a
 *                       bs_dev directly
 *     module/bdev/lvol  not reusable -- g_spdk_lvol_pairs is static
 *     module/bdev/s3lvol  this module: registers lvols into the global bdev
 *                       table
 *     lib/nvmf          reused verbatim, zero changes -- only recognises
 *                       bdev_name
 */

#ifndef VBDEV_S3LVOL_H
#define VBDEV_S3LVOL_H

#include "spdk/stdinc.h"
#include "spdk/bdev_module.h"
#include "spdk/lvol.h"

#include "s3lvol/s3_bs_dev.h"
#include "s3lvol/s3_types.h"

struct spdk_lvol;
struct s3lvol_lvstore;

/**
 * Get this vbdev module's handle, needed when registering bdevs.
 */
struct spdk_bdev_module *vbdev_s3lvol_get_module(void);

/* ==========================================================================
 * lvol → bdev（vbdev_s3lvol_lvol.c）
 * ========================================================================== */

/**
 * Register an already-opened lvol as an spdk_bdev.
 *
 * The bdev name is fixed to `<lvs_name>/<lvol->name>` (prefix mandatory).
 * Snapshots register through the same path -- their read-only-ness is
 * expressed by io_type_supported, and nvmf presents them as read-only
 * namespaces automatically.
 *
 * \return 0 on success; -EEXIST if a bdev with the same name already exists;
 *         other negative errno
 */
int vbdev_s3lvol_bdev_register(struct spdk_lvol *lvol, const char *lvs_name);

/**
 * Unregister the bdev for an lvol. Asynchronous.
 */
void vbdev_s3lvol_bdev_unregister(struct spdk_lvol *lvol,
				  spdk_bdev_unregister_cb cb_fn, void *cb_arg);

/* ==========================================================================
 * lvstore lifecycle (vbdev_s3lvol_lvstore.c)
 * ========================================================================== */

typedef void (*s3lvol_lvs_op_cb)(void *cb_arg, struct s3lvol_lvstore *lvs,
				 int lvolerrno);

/**
 * Create a new lvstore on S3.
 *
 * Internally: s3_client_get_or_create -> s3_bs_dev_create -> spdk_lvs_init.
 *
 * **Does not go through the upstream vbdev_lvs_create()** -- that function
 * builds its own bs_dev from the bdev name and unconditionally dereferences
 * bs_dev->get_base_bdev() (vbdev_lvol.c:286), while our bs_dev has no bdev
 * underneath. spdk_lvs_init() itself only needs a bs_dev, so it is called
 * directly.
 */
int s3lvol_lvstore_create(const struct s3_lvs_opts *opts,
			  s3lvol_lvs_op_cb cb_fn, void *cb_arg);

/**
 * Re-attach an existing lvstore (including crash recovery).
 *
 * Internally: s3_local_dev_open -> s3_journal_open -> s3_wal_open ->
 * s3_bs_dev_create -> attach journal -> journal replay -> attach WAL ->
 * WAL replay -> spdk_lvs_load_ext -> register a bdev per lvol.
 *
 * \c opts must name the same wal_bdev the lvstore was created with; it is not
 * optional. The chunk map lives in that device's journal and cannot be rebuilt
 * from S3, so without it every object in the bucket is an orphan.
 *
 * Capacity and chunk size are *read back from the local device* and whatever the
 * caller put in \c opts for them is ignored: handing blobstore a different
 * geometry than it was created with either fails the load or invites a grow, and
 * a mistyped RPC parameter should not be able to do either.
 *
 * Fails with -EINVAL when the local device belongs to a different lvstore.
 */
int s3lvol_lvstore_attach(const struct s3_lvs_opts *opts,
			  s3lvol_lvs_op_cb cb_fn, void *cb_arg);

/**
 * Unload an lvstore (without deleting the S3 data).
 *
 * The caller must first unmount all nvmf namespaces of this lvstore's
 * lvols.
 *
 * On the WAL path this also pushes everything acknowledged so far into S3 and
 * closes the log, so it may take as long as an S3 round trip per dirty chunk.
 */
void s3lvol_lvstore_unload(struct s3lvol_lvstore *lvs,
			   spdk_lvs_op_complete cb_fn, void *cb_arg);

/**
 * Unload the lvstore, then delete the S3 objects it owned.
 *
 * The counterpart to unload: where unload leaves everything in place for a later
 * attach, this reclaims it. The bstore.json entry is removed last, so a destroy
 * that is interrupted still leaves a record of what needs cleaning up.
 *
 * The unload deliberately comes first. It both reads and rewrites the metadata,
 * so deleting beforehand makes it fail on its own 404s, and its final writes
 * create objects that an earlier enumeration would have missed. The object list
 * is therefore taken from the chunk map as the bs_dev is torn down.
 *
 * The objects deleted are the ones the lvstore knows it owns: every chunk named
 * by the chunk map, the four fixed metadata keys, and one manifest per export.
 * Orphans -- objects whose mapping never reached the chunk map -- are not found
 * this way and need GC.
 *
 * A failed unload deletes nothing and leaves the lvstore loaded, with its
 * bstore.json entry intact.
 */
void s3lvol_lvstore_destroy(struct s3lvol_lvstore *lvs,
			    spdk_lvs_op_complete cb_fn, void *cb_arg);

/**
 * Push everything acknowledged so far into S3, without unloading.
 *
 * Mainly a test hook: afterwards the read overlay is empty, so a subsequent read
 * has to be served from S3. That is what distinguishes "the data is in this
 * process" from "the data is in the object store".
 */
/**
 * Do a checkpoint of the chunk map right away, without waiting for the
 * journal to reach its trigger threshold.
 *
 * One use is operational: a checkpoint before a restart shortens recovery
 * time markedly. Another is testing -- automatic triggering requires the
 * journal to be half full, which at the default 256 MiB means millions of
 * chunk uploads and is unreachable in practice, so this is the only entry
 * point that can exercise that path.
 *
 * Reports success when there are no new changes (the "the journal can be
 * truncated" state the caller wants already holds); reports -EBUSY instead
 * of queuing when one is already running.
 */
void s3lvol_lvstore_checkpoint(struct s3lvol_lvstore *lvs,
			       spdk_lvs_op_complete cb_fn, void *cb_arg);

void s3lvol_lvstore_flush(struct s3lvol_lvstore *lvs,
			  spdk_lvs_op_complete cb_fn, void *cb_arg);

/**
 * Snapshot the write-path counters (WAL, overlay, flusher).
 */
void s3lvol_lvstore_get_stats(struct s3lvol_lvstore *lvs,
			      struct s3_bs_dev_stats *out);

/**
 * Find a created lvstore by name.
 */
struct s3lvol_lvstore *s3lvol_lvstore_find(const char *name);

/** Return the one and only lvstore, or NULL when none or more-than-one exist. */
struct s3lvol_lvstore *s3lvol_lvstore_pick_one(void);

/**
 * How many lvstores are loaded right now.
 *
 * Distinct from s3lvol_lvstore_pick_one(), which conflates "none" and
 * "more than one" into NULL. That is the right answer for callers asking "which
 * lvstore did the caller mean", and the wrong one for a policy check: a guard
 * written as `pick_one() != NULL` only fires at exactly one and lets everything
 * through once two are loaded.
 */
unsigned s3lvol_lvstore_count(void);

/**
 * Find the wrapper around a given blobstore-level lvstore.
 *
 * Exists for the esnap callback, which blobstore invokes *during load* with only
 * the spdk_lvol_store in hand -- at a moment when the wrapper is not yet in the
 * public registry, so a lookup by name is not an option.
 */
struct s3lvol_lvstore *s3lvol_lvstore_find_by_lvs(struct spdk_lvol_store *store);

struct spdk_lvol_store *s3lvol_lvstore_get_lvs(struct s3lvol_lvstore *lvs);
const char             *s3lvol_lvstore_get_name(struct s3lvol_lvstore *lvs);

/**
 * The S3 client this lvstore reads and writes through. Its bucket and prefix are
 * fixed for the lifetime of the lvstore.
 */
struct s3_client       *s3lvol_lvstore_get_client(struct s3lvol_lvstore *lvs);

/**
 * The bs_dev underneath, for operations that need the chunk map rather than
 * blobstore -- a zero-copy export turns blob offsets into objects through it.
 */
struct spdk_bs_dev     *s3lvol_lvstore_get_bs_dev(struct s3lvol_lvstore *lvs);

/**
 * Return the namespace this lvstore was created in.
 *
 * Every lvstore belongs to a namespace, which maps to an S3 target through
 * rcow_namespace_to_target(). An import defaults to the same namespace, which is
 * the common case.
 */
const char *s3lvol_lvstore_get_namespace(struct s3lvol_lvstore *lvs);

/* ==========================================================================
 * Namespace registry
 *
 * A startup script populates this once, and every lvstore operation afterwards
 * only names the namespace rather than repeating the endpoint / bucket /
 * region / TLS settings.
 * ========================================================================== */

/**
 * Register a namespace that maps to an S3 bucket.
 *
 * \param name      identifier used by create_lvstore / attach_lvstore
 * \param target    S3 connection details; only endpoint/bucket/region/
 *                  use_path_style/verify_tls are read, credentials come from
 *                  the environment
 * \return 0 on success, -EEXIST when the name is already taken.
 */
int rcow_namespace_add(const char *name, const struct s3_target *target);

/**
 * Resolve a namespace name to an S3 target.
 *
 * \return the registered target, or NULL when the namespace is unknown.
 */
const struct s3_target *rcow_namespace_to_target(const char *name);

typedef void (*rcow_ns_iter_fn)(const char *name, const struct s3_target *target,
				void *ctx);

void rcow_namespace_for_each(rcow_ns_iter_fn fn, void *ctx);

/* ==========================================================================
 * State files
 *
 * The small JSON files this module keeps outside S3. Both are read by exactly
 * one consumer -- recovery after a crash -- which is why the write has to be
 * atomic: a truncated JSON object parses as nothing, not as one entry fewer.
 * ========================================================================== */

/** Read a state file whole. Returns NULL when absent, empty or unreadable. */
char *s3lvol_statefile_read(const char *path);

/**
 * Replace a state file's contents atomically.
 *
 * Writes a sibling temp file, fsyncs it, renames over the target and fsyncs the
 * directory, so a reader sees either the whole old file or the whole new one.
 */
int s3lvol_statefile_write(const char *path, const char *content);

/** Remove a state file. Absent is success. */
int s3lvol_statefile_remove(const char *path);

/**
 * Resolve a state file's path, letting the environment override the default.
 *
 * The two paths used to be compile-time constants, which made them shared by
 * everything running on the host: the test suites write to the same
 * rcow_active_lvols a production instance is using, and two of them removed it
 * outright in cleanup. That is not a hypothetical -- it happened, and the
 * registry of a live instance went with it.
 *
 * @param env_name   Environment variable consulted first, e.g. S3LVOL_ACTIVE_FILE.
 * @param fallback   Used when the variable is unset, empty, or not an absolute
 *                   path. A relative path is rejected rather than resolved,
 *                   because the target's working directory is not something a
 *                   state file's location should depend on.
 *
 * Resolved once per variable and cached; the result is owned by this module and
 * stays valid for the process's lifetime, so later changes to the environment
 * have no effect. That is deliberate: the path is read on every write, and a
 * value that could change under a running instance would split its state across
 * two files.
 */
const char *s3lvol_statefile_path(const char *env_name, const char *fallback);

/* ==========================================================================
 * NVMf namespace attach / decouple
 *
 * add_ns and remove_ns both require a paused subsystem, and pause and resume are
 * both asynchronous, so each of these is a three-callback chain internally.
 *
 * Pausing freezes the admin queue for the whole subsystem, which means a
 * subsystem left paused takes all RCOW_NS_PER_SUBSYS of its namespaces down with
 * it. Every failure path here therefore drives through to a resume.
 * ========================================================================== */

/** NQN prefix; the subsystem index is appended as two digits. */
#define RCOW_NQN_PREFIX "nqn.2026-08.io.spdk:rcow-"

/**
 * \param nsidthe namespace that was added or removed, 0 when nothing was
 * \param status 0, or a negative errno
 */
typedef void (*s3lvol_nvmf_op_cb)(void *cb_arg, uint32_t nsid, int status);

/** Build the NQN of subsystem \c index. */
void s3lvol_nvmf_subsys_nqn(uint32_t index, char *out, size_t out_len);

/** True when that subsystem has been created on the target. */
bool s3lvol_nvmf_subsys_exists(uint32_t index);

/**
 * Expose \c bdev_name as namespace \c nsid of \c nqn.
 *
 * \c nsid is honoured exactly, not treated as a hint: recovery has to reproduce
 * the previous layout. The namespace uuid is left to default so that it comes
 * from the bdev, which is what lets the host find a volume by its lvol uuid.
 *
 * The subsystem must have been created with max_namespaces >= nsid.
 */
int s3lvol_nvmf_add_ns(const char *nqn, const char *bdev_name, uint32_t nsid,
		s3lvol_nvmf_op_cb cb_fn, void *cb_arg);

/** Remove namespace \c nsid from \c nqn. */
int s3lvol_nvmf_remove_ns(const char *nqn, uint32_t nsid,
			  s3lvol_nvmf_op_cb cb_fn, void *cb_arg);

/**
 * Find the host block device carrying the namespace with this uuid.
 *
 * Scans /sys/block, so it assumes the initiator is this same machine -- the one
 * place in the module that reaches across to the host side.
 *
 * Matched by uuid, never derived from nsid: the host numbers namespaces in
 * discovery order, so nsid 7 and nsid 42 can be n1 and n2 in either arrangement
 * depending on the order they were attached.
 *
 * \return 0 with \c out filled, -ENOENT when no device carries that uuid (which
 * is the normal state for a second or two after add_ns, until the host has
 * rescanned), or another negative errno.
 */
int s3lvol_nvmf_resolve_device(const char *uuid_str, char *out, size_t out_len);

/**
 * True when \p dev is the live host node for \p uuid_str.
 *
 * sysfs can publish a namespace before udev creates (or replaces) /dev, and
 * after deactive then reactivate at the same nsid the leftover node can still
 * belong to the previous occupant. Ready means: it is a block device, its
 * major:minor matches /sys/block/<name>/dev, and that sysfs directory names
 * this uuid.
 */
bool s3lvol_nvmf_device_is_ready(const char *dev, const char *uuid_str);

/**
 * True when udev has no events left to apply.
 *
 * A ready node is only stable once udev is done: a pending REMOVE for the
 * previous occupant of the same nsid can still unlink /dev after the checks
 * above pass. Steady state answers from a single access(2), so callers can
 * test this before deciding to wait at all.
 */
bool s3lvol_nvmf_udev_settled(void);

/**
 * Readahead, in KiB, to apply to a freshly discovered host device.
 *
 * The chunk size, and for the same reason the transport's max_io_size is: what a
 * cold read costs on this stack is one S3 request, near enough regardless of how
 * many bytes it asks for. Measured at queue depth 64 against uncached data,
 * 128 KiB reads gave 797 IOPS / 99.6 MB/s and 1 MiB reads 252 IOPS /
 * 251.7 MB/s -- the same order of requests for eight times the data. With the
 * kernel default of 128 KiB against a 1 MiB chunk, a sequential reader spends
 * eight requests where one would do, and a single threaded one has no other way
 * to get queue depth.
 *
 * Deliberately a compile-time constant rather than an RPC parameter: it is a
 * property of the chunk size, not of a volume. The operator-facing knob is
 * RCOW_READ_AHEAD_KB in scripts/rcow_common.sh, which can override this per
 * device afterwards -- including setting it back to the kernel default for a
 * purely random workload, where reading ahead a whole chunk is waste.
 */
#define RCOW_DEFAULT_READ_AHEAD_KB 1024

/* The kernel's own default, which is what "nobody has touched this device" looks
 * like. Used to tell an untuned device from one somebody set deliberately; see
 * s3lvol_nvmf_set_readahead(). */
#define S3LVOL_KERNEL_DEFAULT_READ_AHEAD_KB 128

/* Overrides RCOW_DEFAULT_READ_AHEAD_KB for this target. 0 disables the tuning
 * altogether, leaving every device as the kernel made it.
 *
 * An environment variable rather than an RPC parameter because it is a property
 * of the deployment, not of a volume, and because it has to agree with the
 * operator-facing RCOW_READ_AHEAD_KB in scripts/rcow_common.sh -- which exports
 * it, so the two cannot drift. Making that the single source of truth is what
 * keeps the two layers from writing different values to the same sysfs file and
 * silently undoing each other. */
#define S3LVOL_READ_AHEAD_ENV "S3LVOL_READ_AHEAD_KB"

/**
 * The readahead this target applies to newly discovered devices, in KiB.
 *
 * S3LVOL_READ_AHEAD_ENV if set and valid, otherwise
 * RCOW_DEFAULT_READ_AHEAD_KB. Resolved once and cached: it cannot change
 * without a restart, and this is called on every device lookup.
 *
 * \return the value, or 0 when the tuning is disabled.
 */
uint32_t s3lvol_nvmf_readahead_kb(void);

/**
 * Set a host block device's readahead, given the sysfs leaf name (e.g. "nvme0n1").
 *
 * **Only ever moves a device off the kernel default.** A device already at \c kb
 * is left alone, and so is one at any *other* non-default value: that means
 * somebody chose it on purpose -- the operator through RCOW_READ_AHEAD_KB, or a
 * tuning layer above -- and overwriting it would make their setting silently
 * temporary, undone by the next lookup of the device. This is what makes the
 * function safe to call on every lookup.
 *
 * Best effort: this is a performance knob, and every failure mode -- no such
 * attribute, a read-only /sys, the device already gone -- leaves a working
 * volume. Callers are not expected to check, which is why nothing is logged at
 * error level.
 *
 * \return true when this call left the device at \c kb, false when it did not --
 *         including the deliberate "somebody else set it" case, which is not an
 *         error. For tests and logging, not for error handling.
 */
bool s3lvol_nvmf_set_readahead(const char *leaf, uint32_t kb);

/* ==========================================================================
 * rcow_active_lvols registry
 *
 * Which lvol or snapshot is exposed through which NVMf subsystem and namespace
 * ID. Read after a restart to rebuild the host-side layout exactly as it was.
 *
 * The device path is deliberately absent: the host numbers namespaces in
 * discovery order, not by nsid (measured -- nsid 7 became n1 and nsid 42 became
 * n2), so a stored path would be wrong after the first replay in a different
 * order. Paths are resolved on demand from the uuid instead.
 *
 * Read-only-ness is absent for a different reason: there is nowhere for it to
 * go. struct spdk_nvmf_ns_opts has no such field and lib/nvmf implements no
 * write-protected namespace at all, so a snapshot reaches the host as a writable
 * device either way -- writes are refused by this module and surface as an I/O
 * error. Recording a flag that no layer consults would only imply a protection
 * that does not exist; see the comment above the io_type_supported handler in
 * vbdev_s3lvol_lvol.c.
 * ========================================================================== */

/** Subsystems pre-created at startup, and namespaces allowed in each. */
#define RCOW_NUM_SUBSYS     32
#define RCOW_NS_PER_SUBSYS  64

struct s3lvol_active_entry {
	char     name[SPDK_LVOL_NAME_MAX];
	char     uuid[SPDK_UUID_STRING_LEN];
	uint32_t subsys;
	uint32_t nsid;
};

/**
 * Read the registry into memory. Idempotent.
 *
 * Fails with -EINVAL when the file exists but does not parse, rather than
 * silently starting from empty: an unreadable registry means the previous
 * host-side layout is unknown, and carrying on would quietly hand out
 * different device paths than before.
 */
int s3lvol_active_load(void);

/** Which subsystem a name belongs on: crc32c(name) % RCOW_NUM_SUBSYS. */
uint32_t s3lvol_active_hash_subsys(const char *name);

/** Lowest free nsid in a subsystem, or 0 when it is full. */
uint32_t s3lvol_active_alloc_nsid(uint32_t subsys);

const struct s3lvol_active_entry *s3lvol_active_find(const char *name);
const struct s3lvol_active_entry *s3lvol_active_find_by_nsid(uint32_t subsys,
							uint32_t nsid);

/** Add or update an entry and persist. Rolls back in memory if the write fails. */
int s3lvol_active_add(const char *name, const char *uuid, uint32_t subsys,
		      uint32_t nsid);

/** Remove an entry and persist. -ENOENT when it was not there. */
int s3lvol_active_remove(const char *name);

const struct s3lvol_active_entry *s3lvol_active_first(void);
const struct s3lvol_active_entry *s3lvol_active_next(
	const struct s3lvol_active_entry *prev);

/* ==========================================================================
 * bstore.json registry
 *
 * Maps user-visible lvstore names to the auto-generated blobstore name,
 * namespace and WAL bdev. A recovery script reads this file to re-issue
 * the correct attach calls.
 * ========================================================================== */

/** Generate a blobstore name: bstore_ followed by 8 hex chars (4 random bytes). */
void bstore_generate_bs_name(char *out, size_t out_size);

/** Save or update one entry. */
int bstore_save_entry(const char *lvs_name, const char *bs_name,
		      const char *ns_name, const char *wal_bdev);

/** Remove one entry. */
int bstore_remove_entry(const char *lvs_name);

/**
 * Iterate over all created lvstores.
 */
struct s3lvol_lvstore *s3lvol_lvstore_first(void);
struct s3lvol_lvstore *s3lvol_lvstore_next(struct s3lvol_lvstore *prev);

/* ==========================================================================
 * lvol lifecycle (vbdev_s3lvol_lvstore.c)
 * ========================================================================== */

typedef void (*s3lvol_lvol_op_cb)(void *cb_arg, struct spdk_lvol *lvol,
				  int lvolerrno);

/**
 * Create an lvol and register it as a bdev automatically.
 *
 * \param thin_provision  thin provisioning. Recommended on an S3 backend --
 *                        unwritten chunks occupy no object and read as zero.
 */
int s3lvol_lvol_create(struct s3lvol_lvstore *lvs, const char *name,
		       uint64_t size_bytes, bool thin_provision,
		       s3lvol_lvol_op_cb cb_fn, void *cb_arg);

/**
 * Take a read-only snapshot of an lvol and register it as a bdev.
 *
 * **No data is copied on the S3 side**: the origin blob's cluster list is
 * frozen into a read-only blob, the origin becomes copy-on-write, and the
 * existing chunk objects stay referenced by the snapshot. The origin's blob
 * id and size are unchanged, so its bdev needs no change.
 *
 * The snapshot itself is read-only; `io_type_supported` turns off write-type
 * I/O accordingly.
 *
 * Returns `-EPERM` for an lvol that is already read-only -- its content can
 * never change again, and a clone is the way to get a new name.
 *
 * **A failed registration does not delete the snapshot.** By then the origin
 * is already its clone, and the blobstore refuses to delete a snapshot that
 * still has a clone; the data is intact, and re-attaching the lvstore restores
 * the missing bdev.
 */
/* True while the lvol is in the decouple queue, running or waiting its turn.
 * A snapshot or clone must not be taken of such a volume while this holds: the
 * snapshot would take the external snapshot identity with it, and the decouple
 * would then fail its detach after materialising the data. create_snapshot
 * cancels the decouple rather than refusing -- see s3lvol_decouple_cancel(). */
bool s3lvol_lvol_decouple_pending(const struct spdk_lvol *lvol);

/**
 * Stop decoupling this lvol so that a snapshot may be taken of it.
 *
 * \return 0  nothing to cancel, or done synchronously -- carry on immediately
 *         1  under way; cb_fn is called once the decouple has stopped
 *         <0 error (-EBUSY if a cancellation is already pending)
 */
int s3lvol_decouple_cancel(struct spdk_lvol *lvol, spdk_lvol_op_complete cb_fn,
			   void *cb_arg);

/**
 * Create a read-only snapshot of an lvol, and register it as a bdev.
 *
 * If a decouple is in flight on \p lvol it is cancelled first, which makes this
 * asynchronous even before the snapshot itself starts; \p out_cancelled_decouple,
 * when not NULL, is set synchronously to say whether that happened, so a caller
 * can report that the volume it asked to be decoupled no longer will be.
 */
int s3lvol_lvol_create_snapshot(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
				const char *snapshot_name,
				bool *out_cancelled_decouple,
				s3lvol_lvol_op_cb cb_fn, void *cb_arg);

/**
 * Create a writable clone with a snapshot as parent, and register it as a
 * bdev.
 *
 * Again no data is copied: the new blob's extent table points at the
 * snapshot's clusters, and only a write triggers CoW to allocate a new
 * cluster.
 *
 * **Only read-only lvols can be cloned**, or `-EINVAL` is returned: a
 * simultaneously writable parent and clone means either side can modify a
 * shared cluster, and neither's data is defined any more.
 */
int s3lvol_lvol_create_clone(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
			     const char *clone_name,
			     s3lvol_lvol_op_cb cb_fn, void *cb_arg);

/**
 * Delete an lvol (unregistering its bdev too).
 *
 * Returns -EBUSY when the bdev is still claimed (e.g. attached to an nvmf
 * namespace); never forces the removal.
 */
int s3lvol_lvol_destroy(struct spdk_lvol *lvol,
			spdk_lvol_op_complete cb_fn, void *cb_arg);

/**
 * Find an lvol by name within an lvstore.
 *
 * Searches the blobstore's own list of opened lvols, so snapshots and clones
 * are found too -- they differ from ordinary volumes only in the blob's
 * read-only bit and parent/child relationships.
 *
 * \return the lvol, or NULL (does not exist / not opened)
 */
struct spdk_lvol *s3lvol_lvol_find(struct s3lvol_lvstore *lvs, const char *name);

/** Find an lvol by name across all lvstores. Returns NULL if not found or if
 *  the name is ambiguous (found in more than one lvstore). */
struct spdk_lvol *s3lvol_lvol_find_any(const char *name);

/** Return the s3lvol_lvstore that owns an lvol, or NULL. */
struct s3lvol_lvstore *s3lvol_lvstore_of_lvol(struct spdk_lvol *lvol);

/**
 * Resize an lvol, notifying the bdev layer of the new blockcnt.
 *
 * **Only grows.** The actual size rounds up to a cluster boundary, so a
 * request "just slightly larger" may be a no-op -- which still counts as
 * success, since the state the caller wanted already holds.
 *
 * Shrinking returns `-ENOTSUP`; a snapshot (read-only blob) returns `-EPERM`;
 * the reasoning for both is in the comment block in
 * vbdev_s3lvol_lvstore.c.
 *
 * The capacity ceiling is enforced by the blobstore itself: a non-thin volume
 * past the remaining clusters gets `-ENOSPC`. Thin volumes may be
 * over-provisioned beyond the lvstore capacity (same as upstream), at the cost
 * of erroring only when they fill up.
 */
int s3lvol_lvol_resize(struct spdk_lvol *lvol, uint64_t size_bytes,
		       spdk_lvol_op_complete cb_fn, void *cb_arg);

/* ==========================================================================
 * Cross-node migration: export / import (vbdev_s3lvol_xfer.c)
 * ========================================================================== */

struct s3_export_manifest;
struct s3lvol_import;

#define S3LVOL_EXPORT_URL_MAX 512

struct s3lvol_export_info {
	char     export_uuid[SPDK_UUID_STRING_LEN];
	char     snapshot_name[SPDK_LVOL_NAME_MAX];
	char     url[S3LVOL_EXPORT_URL_MAX];
	uint64_t size_bytes;
	uint64_t num_chunks;
	uint64_t present_chunks;
	uint32_t chunk_size;

	/* True when the export references the source's live objects instead of
	 * carrying copies. The caller needs to know: a zero-copy export costs the
	 * source nothing to produce, and obliges it to keep the snapshot. */
	bool     zero_copy;

	/* Kept in the wire format for compatibility. New exports use 0 because
	 * their lifetime follows the source snapshot. */
	uint64_t expires_at;
};

typedef void (*s3lvol_export_cb)(void *cb_arg, const struct s3lvol_export_info *info,
				 int status);

/**
 * Freeze a volume and publish it to S3 as a self-contained read-only export.
 *
 * Describes \c snapshot as a set of S3 objects and uploads a manifest naming
 * them.
 *
 * \c snapshot must be read-only, i.e. an actual snapshot. A writable volume has
 * no consistent point in time to describe, and the object uuids a zero-copy
 * export records stop being true the moment somebody writes to those clusters.
 * Taking the snapshot is the caller's job, and taking it early is what usually
 * saves the export a drain.
 *
 * Normally this moves no data at all: the manifest names the objects the
 * snapshot's clusters already occupy, across the whole clone chain, so the cost
 * is one PUT rather than the size of the volume. It falls back to copying when
 * the geometry forbids a reference or when the chain reaches an external
 * snapshot, whose data is not in this lvstore's chunk map.
 *
 * A zero-copy export obliges this node to keep \c snapshot until the export is
 * released or the snapshot owner explicitly deletes it -- it is the snapshot's
 * existence that keeps those objects out of reach of GC. Ancestors of it may
 * still be deleted freely: blobstore
 * merges a deleted snapshot's clusters into its only clone without moving them,
 * so the objects the manifest names stay exactly where they are.
 *
 * \param export_uuid    NULL to generate one, or to reuse the snapshot's existing
 *                       export. Supplying it is how a caller asks for a specific
 *                       identifier; if this snapshot is already exported under a
 *                       different uuid the call is refused.
 * \param uuid_out       optional buffer receiving the uuid as soon as it is
 *                       known, before the export completes. \p uuid_out_len
 *                       is its size in bytes. A snapshot already exported (or
 *                       with an export still in flight) fills this with that
 *                       uuid and starts nothing.
 */
int s3lvol_lvol_export(struct s3lvol_lvstore *lvs, struct spdk_lvol *snapshot,
		       const char *export_uuid, char *uuid_out, size_t uuid_out_len,
		       s3lvol_export_cb cb_fn, void *cb_arg);

/* The states an export can be observed in. Queried live, never stored.
 *
 * NONE means no export was found. Whether that is an answer or an error depends
 * on what was named: rcow_get_snapshot_status refuses an uuid that matches
 * nothing, but reports NONE for a snapshot that exists and has never been
 * exported. In both forms a failed reply means the named thing does not exist. */
enum s3lvol_export_state {
	S3LVOL_EXPORT_STATE_NONE = 0,   /* no such export (yet, or any more) */
	S3LVOL_EXPORT_STATE_INPROGRESS, /* uuid handed out, manifest not durable */
	S3LVOL_EXPORT_STATE_DONE,       /* manifest durable and importable */
};

/**
 * Observe an export by uuid across every loaded lvstore.
 *
 * The state is derived on the spot: an export whose manifest is not yet
 * published reports INPROGRESS, a recorded one DONE, anything else NONE.
 *
 * \c deletable answers whether the snapshot behind the export may be deleted
 * right now. It is computed on the spot from the current export pin and clone
 * count (the query does not HEAD S3; lease liveness is the last poll). False
 * while the export is still in progress, while a live or in-grace lease pins
 * it, or while the snapshot has more than one clone (blobstore can merge a
 * snapshot into only one clone).
 *
 * \return 0 on success, -EINVAL for a NULL/bad argument.
 */
int s3lvol_export_query(const char *export_uuid,
			enum s3lvol_export_state *state, bool *deletable);

/**
 * The same observation, for a snapshot rather than for one export uuid.
 *
 * Exists because a snapshot that was never exported still has a \c deletable
 * worth asking about, and there is no uuid to ask with. A snapshot exported more
 * than once reports the state that has to be waited out: INPROGRESS if any export
 * of it is still being written, DONE if any finished one names it, NONE if none
 * does.
 *
 * \c deletable is computed exactly as in the uuid form, against the same rules
 * the delete path applies.
 *
 * \return 0 on success, -EINVAL for a NULL/bad argument, -ENODEV when no loaded
 *         lvstore has a volume by that name.
 */
int s3lvol_snapshot_query(const char *snapshot_name,
			  enum s3lvol_export_state *state, bool *deletable,
			  bool *pending);

/**
 * The same three answers for an lvol the caller already holds.
 *
 * Preferred wherever the lvol is in hand: s3lvol_snapshot_query() re-resolves
 * the name through s3lvol_lvol_find_any(), which walks every loaded lvstore --
 * O(N) per call, so O(N^2) over a listing loop -- and answers NULL when the same
 * name exists in more than one lvstore, silently dropping the fields for every
 * lvol sharing it.
 *
 * \return 0 on success, -EINVAL for a NULL argument, -ENODEV when the lvol is
 *         not on a loaded s3lvol lvstore.
 */
int s3lvol_snapshot_query_lvol(struct spdk_lvol *lvol,
			       enum s3lvol_export_state *state, bool *deletable,
			       bool *pending);

/**
 * How many layers a zero-copy export of \p lvol would have to walk.
 *
 * The same walk export_build_chain() performs, counting rather than collecting,
 * so the number answers a question with a consequence: past
 * S3LVOL_DEFAULT_MAX_CHAIN_DEPTH that export stops being zero-copy and becomes a
 * full copy of the volume -- and the resulting dense export is permanent, since
 * the reaper only ever collects reference exports (their snapshot going away is
 * what makes them collectable, which says nothing about a self-contained one).
 *
 * Which is why this is reported rather than merely bounded. The fallback to
 * copying is correct and silent, so without a number in hand there is no way to
 * tell a node approaching it from one nowhere near, and the first evidence would
 * be the duplicate objects after it happened.
 *
 * Includes \p lvol itself, so a volume with no parent is 1. Counts through an
 * esnap clone to the clone and stops there: what lies beyond is another
 * lvstore's, and the export names it out of the parent manifest rather than
 * walking it. Not capped -- how far past a threshold a chain is, is the useful
 * part.
 *
 * \return the depth, or 0 if the lvol has no open blob (a deactivated volume
 *         cannot be asked, exactly as the cluster counts cannot).
 */
uint32_t s3lvol_lvol_chain_depth(struct s3lvol_lvstore *lvs,
				 struct spdk_lvol *lvol);

/**
 * Why a delete could not be carried out when it was asked for.
 *
 * The distinction that matters is whether the blocker clears on its own, which
 * is what decides if the poller may finish the job -- see
 * vbdev_s3lvol_pending.c. EXPORT does: an importer's lease goes stale once it
 * stops renewing, and that is positive evidence nobody is reading any more.
 * EXPORT_LEGACY is a compatibility reason for restored queue entries whose
 * export predates leases. The recorded snapshot-delete intent authorises
 * releasing that old export; the poller may finish it.
 */
enum s3lvol_pending_reason {
	S3LVOL_PENDING_EXPORT,		/* an export pins it; its lease will say when */
	S3LVOL_PENDING_EXPORT_LEGACY,	/* a pre-lease export; delete intent releases it */
	S3LVOL_PENDING_EXPORT_INFLIGHT,	/* an export is publishing right now */
	S3LVOL_PENDING_CLONE_COUNT,	/* more than one clone */
	S3LVOL_PENDING_DECOUPLE,	/* a decouple is running on it */
	S3LVOL_PENDING_FAILED,		/* the destroy failed asynchronously */
};

const char *s3lvol_pending_reason_str(enum s3lvol_pending_reason reason);

/**
 * Record / test / clear the "a delete of this snapshot was attempted and could
 * not complete" mark.
 *
 * The mark is reported by rcow_get_lvstores (the PEND column) and by
 * rcow_get_pending_deletes, and a poller completes the delete once the blocker
 * clears -- for the reasons that clear on their own. Marks are also written to
 * `<prefix>/meta/pending-deletes.json` and restored on attach.
 *
 * Keyed by (lvstore uuid, lvol uuid) rather than by name: a name is unique only
 * inside one loaded lvstore and is reusable, so a name-keyed mark can end up
 * pointing at an object the delete was never refused for. The names are carried
 * for reporting only.
 *
 * Recording an intent that is already recorded updates its reason -- the
 * blocker may differ from the one the first attempt hit -- and never enqueues
 * twice.
 */
void s3lvol_snapshot_pending_set(const struct spdk_uuid *lvs_uuid,
				 const struct spdk_uuid *lvol_uuid,
				 const char *lvs_name,
				 const char *snapshot_name,
				 enum s3lvol_pending_reason reason);
bool s3lvol_snapshot_pending_test(const struct spdk_uuid *lvs_uuid,
				  const struct spdk_uuid *lvol_uuid);
void s3lvol_snapshot_pending_clear(const struct spdk_uuid *lvs_uuid,
				   const struct spdk_uuid *lvol_uuid);

/**
 * Whether a recorded intent is one the poller will finish on its own.
 *
 * This is what rcow_delete_lvol reports as \c deferred: the delete was accepted
 * as an intent and needs nothing further from the caller. A live-lease pin
 * answers false until the lease goes stale; a pre-lease export is deferred
 * because the recorded intent is the revocation.
 */
bool s3lvol_snapshot_pending_deferred(const struct spdk_uuid *lvs_uuid,
				      const struct spdk_uuid *lvol_uuid);

/** One entry of the pending-delete queue, as handed to s3lvol_pending_foreach(). */
struct s3lvol_pending_entry {
	struct spdk_uuid           lvs_uuid;
	struct spdk_uuid           lvol_uuid;
	const char                *lvs_name;
	const char                *lvol_name;
	uint64_t                   enqueued_at;
	enum s3lvol_pending_reason reason;
	bool                       deferred;	/* the poller will complete it */
};

typedef void (*s3lvol_pending_cb)(void *cb_arg,
				  const struct s3lvol_pending_entry *entry);

/**
 * Walk the pending-delete queue.
 *
 * Safe for the callback to cancel the entry it is looking at.
 *
 * \return the number of entries visited, or -EINVAL for a NULL callback.
 */
int s3lvol_pending_foreach(s3lvol_pending_cb cb, void *cb_arg);

/**
 * Restore this lvstore's queue from `<prefix>/meta/pending-deletes.json`.
 *
 * Fire and forget, and deliberately so: unlike the exports and imports
 * registries, nothing here protects data -- the worst case is a delete the
 * caller has to ask for again -- so a failure is logged and the attach carries
 * on rather than being held up or refused. Entries land in the queue as the
 * load completes, and the poller re-checks them from scratch; nothing is
 * trusted about whether they are still deletable.
 *
 * The load does not keep a pointer to the wrapper. HEAD/GET callbacks
 * re-resolve the live lvstore by uuid and drop the body if it is gone (or has
 * been replaced by a same-name store with a different uuid). An extra S3
 * client reference keeps CRT alive if unload races the request.
 */
void s3lvol_pending_load(struct s3lvol_lvstore *lvs);

/**
 * Park pending-delete registry HEAD or GET completions until
 * s3lvol_pending_load_release(). Tests only: production never holds attach.
 *
 * \param stage  "head", "get", or "none" / NULL.
 * \return 0, or -EINVAL for an unknown stage.
 */
int s3lvol_pending_load_hold(const char *stage);

unsigned s3lvol_pending_load_parked_count(void);

const char *s3lvol_pending_load_hold_name(void);

unsigned s3lvol_pending_load_release(void);

/**
 * Drop every pending-delete mark belonging to one lvstore.
 *
 * Called from the unload / destroy / free paths. Past a teardown the marks name
 * lvols that no longer exist, and an lvstore attached again can give the same
 * names to different objects -- a mark that outlives its lvstore is how
 * --retry-pending could delete something nobody asked it to.
 */
void s3lvol_snapshot_pending_clear_lvs(const struct spdk_uuid *lvs_uuid);

/**
 * Whether an export that has not published its manifest yet names this snapshot.
 *
 * The registry only learns an export once it is durable, so this is the only way
 * to see one that is still running -- which matters because it holds a bare
 * spdk_lvol pointer it keeps using after the drain. The delete path consults it
 * for exactly that reason.
 */
bool s3lvol_export_inflight_pinning(struct s3lvol_lvstore *lvs,
				    const char *snapshot_name);

/* The two ends of the lease clock, together because they are one decision.
 *
 * An importer renews every RENEW_MIN seconds; the source treats a lease as fresh
 * for 3x the cadence the importer reports, but never less than MIN_GRACE. The
 * two constants deliberately satisfy 3 * RENEW_MIN == MIN_GRACE.
 *
 * Why the source clamps at all, rather than believing renew_s: the verdict it
 * feeds, STALE, is carried out by a poller with nobody watching, and renew_s is a
 * number the importer chose; the floor keeps ordinary WAN/object-store jitter
 * from looking like a dead reader.
 *
 * The direction of the error is what settles the values. Too long only delays
 * reclaiming a snapshot nobody is reading; too short deletes one somebody is. */
#define S3LVOL_LEASE_RENEW_MIN_SEC 20
#define S3LVOL_LEASE_MIN_GRACE_SEC (3 * S3LVOL_LEASE_RENEW_MIN_SEC)

struct s3lvol_import_opts {
	const char *lvol_name;/* name of the clone to create here */
	const char *export_uuid;

	/* The namespace holding the manifest. NULL means this lvstore's own, which
	 * covers everything but a handoff between buckets: the manifest's key is
	 * bucket-level, so within a bucket the uuid is the whole address. Where the
	 * data lives is read out of the manifest, not configured here. */
	const char *src_namespace;

	/* Start a decouple in the background as soon as the clone exists, so the
	 * volume stops depending on the exporting node without anybody having to
	 * come back and ask. The import itself does not wait for it: the volume is
	 * readable and writable from the moment it is created, and the decouple is
	 * about the *export*, not about availability.
	 *
	 * On by default. An import that stays reading through keeps depending on the
	 * export and continuously renews a lease at the source. The opt-out exists
	 * for callers that intentionally retain that dependency.
	 * It is a copy of everything the export holds, and it runs whenever it runs;
	 * the volume is usable either way.
	 *
	 * No effect when the import degenerates into a local clone: there is no
	 * export to decouple from. Logged and ignored, not an error -- a caller that
	 * always sets it is asking for independence from the export, and a local
	 * clone already has that. */
	bool        decouple;
};

/**
 * Create a writable clone of an export.
 *
 * Two implementations behind one call, chosen by what the manifest turns out to
 * name. The caller asks for "a writable copy of that export" either way.
 *
 * 1. The export names a snapshot *this* lvstore still holds, unchanged --
 *    matching endpoint, bucket, prefix, name, and blob id. Then this is a plain
 *    local clone of that snapshot. No import registry entry, no dependency on the
 *    export, nothing read from S3 beyond the manifest that established the fact.
 *    Exporting and re-importing inside one lvstore is a normal way to get a
 *    writable copy of a snapshot, and it should not cost more than a clone.
 *
 *    Worth knowing: it is *safer*, not just cheaper. An esnap clone's parent is
 *    protected by a distributed lease, while a local clone's parent is pinned
 *    directly by blobstore.
 *
 * 2. Anything else -- another node's export, another bucket, or a snapshot that
 *    is gone or has been replaced -- is an esnap clone that reads through to the
 *    export. Metadata only: nothing is transferred, the clone reads through for
 *    what it has not written and copies on first write, which is what makes
 *    resuming a volume on another node a matter of one manifest fetch.
 *
 * In case 2 the manifest is recorded in this lvstore's own registry in S3
 * *before* the clone exists, because the reverse order can leave a clone that no
 * later attach can open. Case 1 writes no registry entry at all.
 *
 * `opts->decouple` applies to case 2 only. In case 1 there is no export to
 * decouple from, and it is logged and ignored rather than failed.
 *
 * The rcow_import_lvol RPC reports which happened in a `mode` field
 * ("local_clone" or "esnap"), read back off the resulting blob. A caller tracking
 * what depends on what needs it: only case 2 appears in rcow_get_imports and only
 * case 2 holds up rcow_release_export.
 *
 * Nothing about export_snapshot changes. Whether a manifest will be consumed here
 * or on another node cannot be known when it is written, so the choice belongs to
 * the import, where the answer is observable.
 */
int s3lvol_lvol_import(struct s3lvol_lvstore *lvs,
		       const struct s3lvol_import_opts *opts,
		       s3lvol_lvol_op_cb cb_fn, void *cb_arg);

/**
 * Stop an imported volume from reading through to its export, keeping it thin.
 *
 * Copies out only the clusters the export's manifest says hold data, then clears
 * the external snapshot parent. The volume stays thin provisioned, so the cost is
 * the data the export holds rather than the volume's provisioned size -- which is
 * what an inflate of an esnap clone would charge, since blobstore treats every
 * one of its clusters as needing allocation.
 *
 * Same intent as spdk_lvol_decouple_parent(), but not that call: for an esnap
 * clone blobstore turns it into a full inflate, so "keeping it thin" is precisely
 * what the public API cannot do.
 *
 * The volume stays readable and writable throughout. What is refused while this
 * runs is snapshot, clone, resize and delete of the same volume, and the final
 * metadata write freezes IO briefly.
 *
 * Safe to run again after an interrupted attempt: clusters already materialised
 * are skipped, and the parent is only cleared once everything else is done.
 *
 * On success the import of the export is dropped from the registry, which is what
 * allows the export to be released.
 *
 * \param cb_fn May be NULL, for a caller that starts this and does not wait --
 * progress is then observable through s3lvol_decouple_first() and the result
 * through the log.
 *
 * Decouples do not run concurrently with one another when they would contend, and
 * this queues rather than refusing in that case, answering 0. Two things count as
 * contention. The same export, because materialising fetches everything it holds
 * and doing that twice at once only makes both slower. The same lvstore, whatever
 * the export, because blobstore serialises cluster allocation per io channel and a
 * channel is per thread per blobstore -- overlapping decouples of one lvstore are
 * therefore serialised inside blobstore regardless, and letting them try means the
 * smaller one can be pushed back indefinitely by the larger. Queueing makes the
 * order first-come-first-served and the wait visible.
 *
 * Different lvstores are genuinely concurrent: different blobstores, different
 * channels, nothing shared but the S3 client.
 *
 * A queued volume stays usable and keeps reading through the export until its turn
 * comes, and it may be deleted while it waits, in which case its callback gets
 * -ECANCELED.
 *
 * \return 0 when the decouple has started *or been queued*, -EINVAL if the volume
 * is not an esnap clone, -EPERM if it is read-only, -EBUSY if another operation is
 * in progress on it or it is already running or queued, -ENOENT if this lvstore
 * has no manifest for the export it reads.
 */
int s3lvol_lvol_decouple(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
		       spdk_lvol_op_complete cb_fn, void *cb_arg);

/* Drop a volume from the decouple queue because it is being deleted.
 *
 * A queued volume does not hold action_in_progress -- it may wait minutes behind
 * a large decouple, and blocking delete for that long would be worse than the
 * duplicate fetching the queue avoids. So a delete can race the queue, and this
 * is what the delete path calls to keep it from handing a freed lvol to the
 * materialiser. A no-op when the volume is not queued. */
void s3lvol_decouple_dequeue_lvol(struct spdk_lvol *lvol);

/* Try the next queued decouple, if any. Called when an action_in_progress
 * holder that is not itself a decouple (resize, a failed snapshot-delete
 * release) has cleared the flag: decouple_start() refuses those with -EBUSY
 * and leaves the entry queued, so something has to look again. */
void s3lvol_decouple_kick_queue(void);

/* A decouple in flight. clusters_done counts the clusters copied so far, out of the
 * clusters_total the manifest says hold data -- not out of the volume's size. */
struct s3lvol_decouple;

struct s3lvol_decouple_info {
	const char *lvs_name;
	const char *lvol_name;
	const char *export_uuid;
	uint64_t    clusters_total;
	uint64_t    clusters_done;
};

struct s3lvol_decouple *s3lvol_decouple_first(void);
struct s3lvol_decouple *s3lvol_decouple_next(struct s3lvol_decouple *prev);
void s3lvol_decouple_get(const struct s3lvol_decouple *d,
		       struct s3lvol_decouple_info *out);

/* Volumes waiting their turn, iterated the same way and reported through the same
 * struct, with clusters_total and clusters_done both zero -- nothing has been
 * counted for them yet. Listed together with the running ones so that a caller
 * waiting for the work to be finished can wait for the list to empty; separating
 * them would make it see an empty list in the gap between one volume finishing and
 * the next starting. */
struct decouple_queued;

struct decouple_queued *s3lvol_decouple_queued_first(void);
struct decouple_queued *s3lvol_decouple_queued_next(struct decouple_queued *prev);
void s3lvol_decouple_queued_get(const struct decouple_queued *q,
				struct s3lvol_decouple_info *out);

/**
 * Drop this lvstore's imports registry entry for \c export_uuid if no volume of
 * the lvstore reads through to it any more.
 *
 * Call this whenever a volume stops being an esnap clone of an export -- today
 * that means after deleting one. It must run while the lvstore is loaded: unload
 * discards the in-memory entries without rewriting the object, so an entry that
 * outlives its last reader can no longer be recognised as stale afterwards, and
 * release_export would keep refusing on account of a clone that is gone.
 *
 * A no-op when something still reads the export, so it is always safe to call.
 */
void s3lvol_imports_recheck(struct s3lvol_lvstore *lvs, const char *export_uuid);

/**
 * Delete an export's objects.
 *
 * Refuses with -EBUSY while a volume in this process still reads through to it.
 * That is the only protection available: the exporting node cannot know who else
 * imported, which is why the importer is the one that asks for the release, once
 * its clone no longer needs the export.
 */
int s3lvol_export_release(struct s3lvol_lvstore *lvs, const char *export_uuid,
			  spdk_lvol_op_complete cb_fn, void *cb_arg);
int s3lvol_export_release_for_delete(struct s3lvol_lvstore *lvs,
				     const char *export_uuid,
				     spdk_lvol_op_complete cb_fn, void *cb_arg);

/**
 * Fetch this lvstore's imports registry into memory.
 *
 * **Must complete before spdk_lvs_load_ext().** blobstore asks for the parent of
 * each esnap clone synchronously while loading, and that request can only be
 * answered from a cache -- waiting for an S3 GET there would deadlock the thread
 * that has to poll for it.
 *
 * A missing registry object is success: it means nothing was ever imported.
 */
int s3lvol_xfer_imports_load(struct s3lvol_lvstore *lvs,
			     spdk_lvs_op_complete cb_fn, void *cb_arg);

/**
 * Drop this lvstore's cached manifests. The registry object in S3 is untouched --
 * it describes the lvstore, not this process.
 */
void s3lvol_xfer_lvstore_fini(struct s3lvol_lvstore *lvs);

/**
 * blobstore's request for the read-only parent of an esnap clone. Register it in
 * spdk_lvs_opts::esnap_bs_dev_create for *both* init and load; without it an
 * lvstore holding esnap clones cannot be loaded.
 */
int s3lvol_esnap_dev_create(void *bs_ctx, void *blob_ctx, struct spdk_blob *blob,
			    const void *esnap_id, uint32_t id_len,
			    struct spdk_bs_dev **bs_dev);

/* ==========================================================================
 * Exports published by this node (vbdev_s3lvol_exports.c)
 *
 * A zero-copy export points at a snapshot's live objects, so it creates an
 * obligation for this node: until the importer is done, that snapshot must
 * not be deleted -- the snapshot is what keeps those objects in the chunk map
 * and out of GC's reach. An obligation nobody recorded does not survive a
 * restart, so this table is persisted to S3 and read back on attach.
 * ========================================================================== */

struct s3lvol_export;

/**
 * Why the snapshot is (or is not) held, for callers that need more than
 * yes/no.
 *
 * Ordered by how restrictive the answer is, so aggregating several exports over
 * one snapshot is a max().
 *
 * The distinction that matters is STALE versus LEGACY. STALE is evidence that
 * no importer currently holds the export. LEGACY predates leases, so reader
 * liveness is unknown: status still reports it, but an explicit snapshot delete
 * is the revocation (the same statement Cubelet used to make with
 * rcow_release_export).
 */
enum s3lvol_export_pin {
	S3LVOL_EXPORT_PIN_NONE,		/* no reference export names it */
	S3LVOL_EXPORT_PIN_STALE,	/* named, but its lease says nobody reads */
	S3LVOL_EXPORT_PIN_LEGACY,	/* named, no trustworthy lease evidence */
	S3LVOL_EXPORT_PIN_LEASE,	/* an importer is reading, or may be */
};

static inline const char *
s3lvol_export_pin_str(enum s3lvol_export_pin pin)
{
	switch (pin) {
	case S3LVOL_EXPORT_PIN_STALE:
		return "stale";
	case S3LVOL_EXPORT_PIN_LEGACY:
		return "legacy";
	case S3LVOL_EXPORT_PIN_LEASE:
		return "lease";
	case S3LVOL_EXPORT_PIN_NONE:
	default:
		return "none";
	}
}

struct s3lvol_export_entry {
	const char *export_uuid;
	const char *snapshot;
	const char *snapshot_uuid;
	uint64_t    blob_id;
	uint64_t    expires_at;
	uint32_t    generation;
	bool    is_ref;
	bool    lease_aware;
	bool    lease_checked;
	bool    lease_absent;
	bool    lease_watch;
	bool    snapshot_alive;
	bool    reaping;
	uint64_t    lease_updated_at;
	uint32_t    lease_renew_s;
	enum s3lvol_export_pin pin;
};

/**
 * The export that still pins \c snapshot_name, or NULL if it may be deleted.
 *
 * Only a reference export with a live or still-unknown reader counts. A
 * confirmed lease miss older than the source grace, and a pre-lease (LEGACY)
 * export, do not pin: they remain importable until the snapshot is deleted, and
 * that delete releases them internally.
 */
struct s3lvol_export *s3lvol_export_pinning(struct s3lvol_lvstore *lvs,
					    const char *snapshot_name);

enum s3lvol_export_pin s3lvol_export_pin_state(struct s3lvol_lvstore *lvs,
					       const char *snapshot_name);

struct s3lvol_export *s3lvol_export_find(struct s3lvol_lvstore *lvs,
					 const char *uuid_str);

struct s3lvol_export *s3lvol_export_add(struct s3lvol_lvstore *lvs,
					const struct s3_export_manifest *m,
					const char *snapshot_name);
void s3lvol_export_forget(struct s3lvol_export *exp);
void s3lvol_export_set_materialised(struct s3lvol_export *exp, uint32_t generation);
void s3lvol_export_set_local_ref(struct s3lvol_export *exp, uint32_t generation);

struct s3lvol_export *s3lvol_export_first(struct s3lvol_lvstore *lvs);
struct s3lvol_export *s3lvol_export_next(struct s3lvol_export *prev);
/** First REF export of this live snapshot (uuid, else blob_id). Skips reaper. */
struct s3lvol_export *s3lvol_export_first_ref_for_snapshot(
	struct s3lvol_lvstore *lvs, const char *snapshot_name);
/** Published export of this live snapshot, including a materialised rewrite
 *  of the same uuid. Skips only the reaper. Release in flight is -EBUSY above. */
struct s3lvol_export *s3lvol_export_find_for_snapshot(
	struct s3lvol_lvstore *lvs, const char *snapshot_name);
bool s3lvol_snapshot_exports_have_local_readers(struct s3lvol_lvstore *lvs,
						 const char *snapshot_name);
void s3lvol_export_get(const struct s3lvol_export *exp,
		     struct s3lvol_export_entry *out);

/**
 * Write the registry out. Every change to it has to be followed by this, or a
 * restart forgets an obligation that another machine is depending on.
 */
int s3lvol_export_registry_save(struct s3lvol_lvstore *lvs,
				spdk_lvs_op_complete cb_fn, void *cb_arg);

/**
 * Read the registry at attach. A missing object means nothing was ever exported.
 *
 * Unlike the imports registry this is not needed before spdk_lvs_load_ext() --
 * nothing blobstore does depends on it. It is needed before the first delete of
 * an lvol, which in practice means before any RPC is served.
 */
int s3lvol_xfer_exports_load(struct s3lvol_lvstore *lvs,
			     spdk_lvs_op_complete cb_fn, void *cb_arg);

void s3lvol_xfer_exports_fini(struct s3lvol_lvstore *lvs);

/**
 * Turn a reference export into a copied one, so this node stops owing anybody
 * its snapshot.
 *
 * A reference export names this lvstore's live chunk objects, which is why it
 * pins the snapshot behind it: those objects cannot be reclaimed while an
 * importer may read them. Snapshot deletion waits for readers and then releases
 * the ref export. Materialising is the alternative when the export itself must
 * survive: copy the snapshot and publish the manifest as dense with `generation`
 * bumped.
 *
 * Afterwards the export owes nothing. It holds copies, so the pin and lease
 * watch go away -- see
 * s3lvol_export_set_materialised().
 *
 * **Importers are not told, and do not have to be.** The manifest is replaced in
 * place, so an importer holding the old one keeps reading until the objects it
 * names disappear; the 404 then makes it refetch, find the higher generation, and
 * carry on against the copies. That is why the ordering here is not negotiable:
 * the new manifest has to be published before the old objects can be deleted, or
 * a reader hits a 404 and refetches into the manifest that sent it there.
 *
 * This does not delete the old chunk objects. They belong to the snapshot's chunk
 * map, not to the export, and they go when the snapshot does -- which is now
 * allowed to happen.
 *
 * \return 0 with the callback pending; -ENOENT if no such export; -EALREADY if it
 * is already a copy; -EBUSY if a materialisation of it is already running.
 */
int s3lvol_export_materialise(struct s3lvol_lvstore *lvs, const char *export_uuid,
			      spdk_lvol_op_complete cb_fn, void *cb_arg);

struct s3lvol_import *s3lvol_import_first(struct s3lvol_lvstore *lvs);
struct s3lvol_import *s3lvol_import_next(struct s3lvol_import *prev);
const struct s3_export_manifest *s3lvol_import_get_manifest(
	const struct s3lvol_import *imp);

#endif /* VBDEV_S3LVOL_H */
