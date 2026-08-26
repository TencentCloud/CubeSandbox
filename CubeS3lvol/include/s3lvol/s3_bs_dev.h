/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_bs_dev: maps a linear LBA space onto a local bdev (WAL/Cache) + S3
 *   objects
 *
 *   === Core role ===
 *   Implements struct spdk_bs_dev (defined in the SPDK public header
 *   include/spdk/blob.h) as the blobstore's underlying "virtual raw
 *   disk". The blobstore is completely unaware of S3; every read/write it
 *   issues is a linear LBA, including the blobstore's own metadata (super
 *   block / metadata blob / blob metadata pages) -- all of it persists through
 *   the same WAL -> chunk -> S3 path with no special handling (keeping the P1
 *   reuse principle).
 *
 *   === Three-level mapping ===
 *     LBA --[shift, compute]--> chunk_index --[chunk_map, table]--> uuid --[concat, compute]--> S3 key
 *   Only the middle level needs a table lookup.
 */

#ifndef S3LVOL_BS_DEV_H
#define S3LVOL_BS_DEV_H

#include "spdk/blob.h"
#include "spdk/bdev.h"
#include "s3lvol/s3_types.h"
#include "s3lvol/s3_client.h"
#include "s3lvol/s3_flusher.h"
#include "s3lvol/s3_journal.h"
#include "s3lvol/s3_wal.h"

struct s3_ctx;             /* opaque; the internal context, see s3_bs_dev_internal.h */
struct s3_export_manifest; /* opaque; see s3_export.h */
struct s3_chunk_map;       /* opaque, see s3_chunk_map.h */

typedef void (*s3_bs_dev_cb)(void *cb_arg, int status);

/* ==========================================================================
 * Read-write s3_bs_dev (carries the lvstore proper)
 * ========================================================================== */

/**
 * Create an s3_bs_dev.
 *
 * Created by vbdev_s3lvol **before** calling spdk_lvs_init / spdk_lvs_load_ext.
 *
 * Capacity semantics (important): S3 has no physical capacity concept;
 * capacity is the **logical provisioning upper bound**.
 *   - create path: from the RPC's --capacity
 *   - attach path: must be read back from the S3-persisted metadata and match
 *     the create-time value, or the cluster count rebuilt by spdk_bs_load
 *     conflicts with the size recorded in the super block.
 * Internally sets base.blocklen = 4096, base.blockcnt = capacity / 4096, with
 * blockcnt rounded down to cluster_size alignment.
 *
 * \param wal_desc    the opened WAL bdev descriptor
 * \param cache_desc  the opened cache bdev descriptor; same as wal_desc for
 *                    the single-bdev layout
 * \param out         the resulting bs_dev, ready to pass to spdk_lvs_init /
 *                    spdk_lvs_load_ext
 */
int s3_bs_dev_create(const struct s3_lvs_opts *opts,
		     struct spdk_bdev_desc *wal_desc,
		     struct spdk_bdev_desc *cache_desc,
		     struct s3_client *client,
		     uint64_t capacity_bytes,
		     struct spdk_bs_dev **out);

/**
 * Retrieve the internal context from a bs_dev (for flusher / checkpoint / GC
 * and other submodules).
 */
struct s3_ctx *s3_bs_dev_get_ctx(struct spdk_bs_dev *bs_dev);

/* ==========================================================================
 * WAL write path
 *
 * Until a WAL is attached the bs_dev writes straight to S3, which needs no local
 * device but *is not correct under concurrency* -- see the header comment in
 * s3_bs_dev.c. Attaching a WAL switches the write path to
 * "append, acknowledge, flush later" and turns on the read overlay.
 * ========================================================================== */

/**
 * Attach an opened WAL and start the flusher.
 *
 * Call order at startup matters:
 *
 *   1. s3_bs_dev_create()
 *   2. s3_bs_dev_attach_journal()  -- before replaying the journal
 *   3. s3_bs_dev_attach_wal()      -- the overlay has to exist before replay
 *   4. s3_journal_replay() then s3_wal_replay(), feeding the records to
 *      s3_bs_dev_journal_apply() and s3_bs_dev_wal_apply()
 *   5. spdk_lvs_init() / spdk_lvs_load_ext()
 *
 * Replay has to come before blobstore is loaded: blobstore reads its super block
 * immediately, and that read must already see the recovered data.
 *
 * *Ownership of closing the WAL transfers here.* The flusher writes the log
 * (truncation, super sync), so the log has to outlive the flusher, and only this
 * module knows when that is; bs_dev->destroy closes it. The caller still owns the
 * local device the log lives on, and learns when it may release it through
 * s3_bs_dev_set_destroy_cb().
 *
 * Must be called on the lvstore's owner thread.
 *
 * \return 0, -EEXIST when a WAL is already attached, or a negative errno.
 */
int s3_bs_dev_attach_wal(struct spdk_bs_dev *bs_dev, struct s3_wal *wal,
			 const struct s3_flusher_opts *opts);

/**
 * Start using the local device's cache region as a read cache.
 *
 * Entirely optional, and optional at run time too: without it every read that
 * misses the overlay goes to S3, which is what happened before the cache
 * existed. Nothing else changes behaviour.
 *
 * Call it after s3_bs_dev_attach_journal(), which is where the local device
 * arrives. Order against the WAL and against replay does not matter -- the cache
 * holds copies of S3 objects and takes no part in making a write durable -- but
 * before the lvstore is loaded is the useful moment, since that is the first
 * thing to read.
 *
 * \return 0, or -ENOTSUP when the disk has no cache region (an older layout, or
 *         a region too small for one chunk), which callers should treat as
 *         "no cache" rather than as a failure. -EEXIST if already attached.
 */
int s3_bs_dev_attach_cache(struct spdk_bs_dev *bs_dev);

/**
 * Register a callback for "this bs_dev is completely gone".
 *
 * *Required on the WAL path.* bs_dev->destroy() cannot block, but on the WAL path
 * it still has work to do -- blobstore's own unload writes are acknowledged from
 * the log, so they have to be flushed to S3 and the log closed. destroy()
 * therefore returns immediately and finishes in the background, which means
 * spdk_lvs_unload()'s completion does *not* mean the bs_dev is done with the
 * journal and the local device. This callback does.
 *
 * Set it before calling spdk_lvs_unload().
 */
void s3_bs_dev_set_destroy_cb(struct spdk_bs_dev *bs_dev, s3_bs_dev_cb cb_fn,
			      void *cb_arg);

/**
 * Called with the final chunk map, immediately before it is destroyed.
 *
 * The one moment the map is both complete and still readable. Unloading is what
 * makes it complete: blobstore writes its last metadata during unload, those
 * writes go through create-once like any other, and each one puts a new uuid in
 * the map. A caller that walks the map before unloading therefore misses them --
 * and one that waits for s3_bs_dev_set_destroy_cb() is too late, because the map
 * is freed on the line before that callback fires.
 *
 * Exists for destroy: reclaiming an lvstore's objects means naming every one of
 * them, and walking the map earlier names all but the last few. Deleting first
 * and unloading afterwards is not an alternative -- unload *reads* what it is
 * about to rewrite, so the deletes would turn its own metadata reads into 404s.
 *
 * The map must not be retained past the callback; copy what is needed.
 */
typedef void (*s3_bs_dev_reap_cb)(void *cb_arg, struct s3_chunk_map *map);

void s3_bs_dev_set_reap_cb(struct spdk_bs_dev *bs_dev, s3_bs_dev_reap_cb cb_fn,
			   void *cb_arg);

/**
 * Apply one replayed WAL entry. Pass this as s3_wal_replay()'s apply callback.
 */
int s3_bs_dev_wal_apply(struct spdk_bs_dev *bs_dev,
			const struct s3_wal_entry_hdr *hdr, const void *payload);

/**
 * Wait until everything acknowledged so far has reached S3 (INV2).
 *
 * Worth doing before spdk_lvs_unload() so most of the work is out of the way
 * while blobstore is still up, but it is not sufficient on its own: unloading
 * writes more metadata, which destroy() then has to flush. Without a WAL this
 * completes immediately, since a finished write is already in S3.
 */
void s3_bs_dev_drain(struct spdk_bs_dev *bs_dev, s3_bs_dev_cb cb_fn, void *cb_arg);

/**
 * Nudge the flusher. Only useful for tests and for shutdown paths that want to
 * start uploads without waiting for the next tick.
 */
void s3_bs_dev_kick_flusher(struct spdk_bs_dev *bs_dev);

/* ==========================================================================
 * Chunk map persistence
 * ========================================================================== */

/**
 * Attach the metadata journal, so every chunk mapping change is recorded before
 * it takes effect.
 *
 * The chunk map is internal to this module, which is why the journal is handed
 * in here rather than wired up by the caller. Without it the map is memory-only
 * and the lvstore cannot be re-attached after a restart.
 */
/**
 * The chunk map behind this bs_dev.
 *
 * Exposed for the checkpoint path, which snapshots and restores the table. The
 * map is *owned by the bs_dev* -- do not destroy it, and only touch it on the
 * lvstore's owner thread.
 */
struct s3_chunk_map *s3_bs_dev_get_chunk_map(struct spdk_bs_dev *bs_dev);

/**
 * Bytes carried by one S3 object on this device.
 *
 * For export, which has to agree with the chunk map on where chunk boundaries
 * are before it can name objects by index.
 */
uint32_t s3_bs_dev_get_chunk_size(struct spdk_bs_dev *bs_dev);

/**
 * Take a checkpoint now, without waiting for either automatic trigger.
 *
 * Checkpoints normally happen on their own, for one of two reasons: the journal
 * passed its usage threshold, or the configured interval elapsed
 * (s3_lvs_opts::checkpoint_interval_sec, 60 s by default). This is for the cases
 * that cannot wait for either -- bounding recovery time immediately before a
 * planned restart, and tests that want a deterministic point rather than one
 * dependent on the clock.
 *
 * Reports success when there is nothing new to snapshot -- the caller's intent is
 * "make sure the journal can be truncated", and it already can. Reports -EBUSY
 * when one is already running, rather than queueing: success would imply the
 * caller's LSN is covered, and the running one may have sampled an earlier LSN.
 */
void s3_bs_dev_checkpoint(struct spdk_bs_dev *bs_dev, s3_bs_dev_cb cb_fn,
			  void *cb_arg);

/**
 * \param local_dev  owns the super block that records checkpoint_gen /
 *                   checkpoint_lsn, and supplies the lvstore uuid stamped into
 *                   snapshots. Passing it also starts the checkpoint poller;
 *                   NULL leaves the journal untruncatable, which means the
 *                   lvstore turns read-only once the region fills.
 */
int s3_bs_dev_attach_journal(struct spdk_bs_dev *bs_dev, struct s3_journal *journal,
			     struct s3_local_dev *local_dev);

/**
 * Apply one replayed journal record. Pass this as s3_journal_replay()'s apply
 * callback, *before* the WAL is replayed: the journal describes which S3 objects
 * exist, and the WAL then re-applies whatever had not reached them yet.
 */
int s3_bs_dev_journal_apply(struct spdk_bs_dev *bs_dev,
			    const struct s3_journal_record *rec);

/* ==========================================================================
 * Diagnostics
 * ========================================================================== */

struct s3_bs_dev_stats {
	bool     wal_attached;

	uint64_t allocated_chunks;
	uint64_t rmw_count;       /* uploads that had to read the old object */
	uint64_t zero_fill_count; /* reads of an unallocated range */

	uint64_t wal_writes;      /* writes acknowledged from the log */
	uint64_t wal_retries;     /* writes parked by backpressure */
	uint64_t overlay_hits;    /* reads served without touching S3 */
	uint32_t retry_queued;    /* writes parked right now */

	uint64_t overlay_bytes;
	uint64_t overlay_live_chunks;

	uint64_t chunks_flushed;
	uint64_t flush_failures;
	uint32_t flush_in_flight;

	/* Checkpoints. Exposed mainly so a test can assert that truncation is
	 * actually happening -- without it, "the journal never filled up" is
	 * indistinguishable from "the test never wrote enough". */
	uint64_t ckpt_done;
	uint64_t ckpt_failed;
	uint64_t ckpt_lsn;        /* LSN the last completed checkpoint covers */
	uint64_t ckpt_gen;
	uint32_t ckpt_interval_sec; /* time-based trigger currently in effect */
	uint64_t journal_used_bytes;
	uint64_t journal_capacity_bytes;

	/* Why the flusher was given a chunk. See s3_overlay_set_flush_policy: a
	 * write-heavy run should be mostly "full", a trickle mostly "aged", and a
	 * steady stream of "forced" means the overlay cap is too small for the
	 * flush rate S3 can sustain. */
	uint64_t overlay_flushed_full;
	uint64_t overlay_flushed_aged;
	uint64_t overlay_flushed_forced;

	/* Local read cache. All zero when there is none.
	 *
	 * cache_hits against cache_misses is the number worth watching: a hit is
	 * a read that cost a local round trip instead of an S3 one. A high
	 * populate_dropped means slots or staging buffers are scarce, i.e. the
	 * cache is too small or too slow for the flush rate.
	 *
	 * cache_hits_declined is the third outcome: the object was here but not
	 * the blocks this read wanted. Some of it is unavoidable while a chunk
	 * fills in a piece at a time, but a lot of it against few hits means
	 * reads repeat at a coarser granularity than they arrive in. */
	bool     cache_attached;
	uint64_t cache_hits;
	uint64_t cache_misses;
	uint64_t cache_hits_declined;
	uint64_t cache_populates;
	uint64_t cache_populates_dropped;
	uint64_t cache_evictions;
	uint64_t cache_bytes_served;
	uint64_t cache_bytes_populated;
	uint64_t cache_slots_total;
	uint64_t cache_slots_resident;
	uint64_t cache_bytes_resident;
};

/**
 * Snapshot the counters. Cheap; safe to call from an RPC handler on the owner
 * thread.
 */
void s3_bs_dev_get_stats(struct spdk_bs_dev *bs_dev, struct s3_bs_dev_stats *out);

/* ==========================================================================
 * Read-only s3_export_bs_dev (the parent of a cross-node external snapshot)
 *
 * See s3lvol/s3_export.h -- it is not part of this module: read-only, no WAL /
 * flusher / chunk_map, and its data source is an export manifest rather than
 * this lvstore's mapping table. Putting it here would make "does this bs_dev
 * have a chunk map" something you have to read code to find out.
 * ========================================================================== */

/* ==========================================================================
 * LBA <-> chunk conversion (pure arithmetic, no table)
 * ========================================================================== */

static inline uint64_t
s3_lba_to_chunk_index(uint64_t lba, uint32_t chunk_shift)
{
	return lba >> (chunk_shift - S3LVOL_BLOCK_SHIFT);
}

static inline uint64_t
s3_chunk_index_to_lba(uint64_t chunk_index, uint32_t chunk_shift)
{
	return chunk_index << (chunk_shift - S3LVOL_BLOCK_SHIFT);
}

static inline uint32_t
s3_lba_offset_in_chunk(uint64_t lba, uint32_t chunk_shift)
{
	uint64_t blocks_per_chunk = 1ULL << (chunk_shift - S3LVOL_BLOCK_SHIFT);

	return (uint32_t)((lba & (blocks_per_chunk - 1)) << S3LVOL_BLOCK_SHIFT);
}

#endif /* S3LVOL_BS_DEV_H */
