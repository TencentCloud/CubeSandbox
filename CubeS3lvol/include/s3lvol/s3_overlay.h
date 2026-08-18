/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_overlay -- RAM view of data that is durable in the WAL but not yet in S3
 *   (the RAM half of the chunk cache)
 *
 *   === Why this exists ===
 *
 *   The WAL makes a write durable without putting it in S3. Between the
 *   acknowledgement and the flusher's PUT there is a window in which S3 still
 *   holds the previous version of the chunk. A read served straight from S3
 *   during that window returns stale data, which is strictly worse than the
 *   lost-update bug the WAL was introduced to fix: losing an unacknowledged
 *   write is a durability question, whereas returning an old version of an
 *   *acknowledged* write breaks read-after-write outright.
 *
 *   So the overlay is not an optimisation. *It must go live in the same change
 *   as the flusher.* A flusher without an overlay opens exactly the window
 *   described above.
 *
 *   === What it stores ===
 *
 *   Blocks, keyed by LBA, grouped by chunk:
 *
 *     chunk_index -> [ block 0 | block 1 | ... | block N-1 ]
 *
 *   Grouping by chunk is what lets the flusher do *one* read-modify-write per
 *   chunk however many small writes landed in it -- which is the property that
 *   removes the concurrent-RMW race by construction rather than by locking.
 *
 *   A block is in one of three states: absent (S3 is authoritative), holding
 *   data, or known to be zero. Zero has to be represented explicitly: after a
 *   write_zeroes, "absent" would fall through to whatever S3 still has, which
 *   is the old non-zero content.
 *
 *   === Ordering ===
 *
 *   Every write carries the seq the WAL assigned it, and a block only ever moves
 *   forward: a write whose seq is below the seq already recorded is dropped.
 *   That makes the overlay insensitive to the order in which writes are applied,
 *   which matters for replay -- recovery may re-apply entries that the flusher
 *   already consumed, and after the ring wraps the physical scan order is not
 *   the seq order.
 *
 *   === Flush handshake ===
 *
 *   A flush must not lose writes that arrive while it is running, and must not
 *   drop blocks it has not actually uploaded. Both come from one flag per
 *   block:
 *
 *     flush_begin  marks every dirty block "flushing" and reports the view
 *     writes       arriving now clear the flag and re-dirty the block
 *     flush_end    drops the blocks still marked flushing (success) or
 *                  re-dirties them (failure)
 *
 *   So a block written during a flush is kept and flushed again next round,
 *   and a failed flush loses nothing.
 *
 *   === Threading ===
 *
 *   Same contract as the chunk map, the journal and the WAL: one overlay belongs
 *   to one lvstore and is only touched on that lvstore's owner thread. No
 *   internal locking.
 */

#ifndef S3LVOL_OVERLAY_H
#define S3LVOL_OVERLAY_H

#include "spdk/stdinc.h"

/* Cap on the data held in RAM. Past this the caller must apply backpressure
 * instead of growing without bound: the overlay only shrinks when the flusher
 * completes a chunk, so an unbounded overlay turns a slow S3 into an OOM.
 *
 * 4 GiB rather than the 256 MiB this started at, because the overlay is what
 * decides how long a write may stay out of S3. The WAL bounds durability and has
 * 32 GiB to play with; the overlay bounds *readability* -- a block not held here
 * would have to be served from S3, which still has the previous version -- so it
 * is the overlay that runs out first and forces a flush. Holding a chunk back
 * until it is worth uploading (see s3_overlay_set_flush_policy) is only possible
 * within this budget: at 50 MiB/s of incoming writes, 256 MiB is five seconds.
 *
 * This is ordinary malloc'd memory, not part of the DPDK allocation, so it shows
 * up as process RSS. It is a ceiling and not a reservation -- an idle lvstore
 * holds nothing.
 */
#define S3_OVERLAY_DEFAULT_MAX_BYTES (4ULL * 1024 * 1024 * 1024)

/* Flush policy defaults; see s3_overlay_set_flush_policy().
 *
 * The age is 45s rather than a round 60s to stay clear of the checkpoint poller,
 * which runs on a 60s period (S3_CKPT_DEFAULT_INTERVAL_SEC). Two 60s cycles beat
 * against each other and periodically put a batch of flushes and a checkpoint on
 * the owner thread in the same tick.
 */
#define S3_OVERLAY_FLUSH_MAX_AGE_US   (45ULL * 1000 * 1000)

/* Fraction of a chunk that has to be dirty for it to be flushed before it ages
 * out, in percent. Reaching 100 is what makes a flush skip the read half of its
 * read-modify-write, so the threshold exists to wait for that -- but only for as
 * long as the age allows.
 */
#define S3_OVERLAY_FLUSH_FULL_PCT     100

/* Fraction of max_bytes past which the age is ignored and everything dirty is
 * flushed. Below the hard cap of s3_overlay_is_full(), so that the flusher
 * starts catching up before writes have to be retried. */
#define S3_OVERLAY_FLUSH_HIGH_PCT     75

struct s3_overlay;

/* What one flush round covers. Produced by s3_overlay_flush_begin() and handed
 * to whoever performs the upload; it stays valid until s3_overlay_flush_end(). */
struct s3_overlay_flush_view {
	uint64_t chunk_index;

	/* Bumped whenever the chunk is dropped (unmap). The uploader must
	 * re-check it before publishing the new object: if it changed, the data
	 * this flush carries has been discarded and publishing it would
	 * resurrect unmapped content.
	 *
	 * Only comparable while the round is open, which is the only time it is
	 * used. Once the round ends the chunk may be released and a later one
	 * starts counting from zero again -- a chunk with a live flush is never
	 * released, so the comparison cannot go wrong where it matters. */
	uint64_t epoch;

	/* Highest seq in this flush. Once uploaded, every WAL entry up to here
	 * for this chunk has reached S3. */
	uint64_t max_seq;

	/* Bytes from the start of the chunk up to the end of the last block
	 * being flushed. The uploaded object is never shorter than this. */
	uint32_t end_offset;

	uint32_t nblocks;

	/* Every block in [0, end_offset) is part of this flush, so the old
	 * object does not have to be read back first. */
	bool     covers_prefix;
};

struct s3_overlay_stats {
	uint64_t writes;/* s3_overlay_write() calls */
	uint64_t blocks_written;
	uint64_t blocks_dropped;    /* dropped as stale by the seq guard */
	uint64_t flushes_begun;
	uint64_t flushes_failed;
	uint64_t peak_bytes;

	/* Why chunks were handed to the flusher. Together these say whether the
	 * policy is doing what it is meant to: a healthy write-heavy run is
	 * mostly "full", a trickle is mostly "aged", and a steady stream of
	 * "high_water" means max_bytes is too small for the flush rate S3 can
	 * sustain. */
	uint64_t flushed_full;      /* the chunk reached full_pct */
	uint64_t flushed_aged;      /* it sat dirty for max_age_us */
	uint64_t flushed_forced;    /* high water mark, or a drain */
};

/**
 * Create an overlay for an address space of \c total_blocks blocks.
 *
 * \param max_bytes 0 selects S3_OVERLAY_DEFAULT_MAX_BYTES.
 */
int s3_overlay_create(uint64_t total_blocks, uint32_t block_size,
		      uint32_t chunk_size, uint64_t max_bytes,
		      struct s3_overlay **out);

/**
 * Release the overlay and everything it holds.
 *
 * *Data still present here has not reached S3.* It is still in the WAL, so it
 * is not lost, but it will have to be replayed. Asserts that no flush is in
 * flight, for the same use-after-free reason as s3_chunk_map_destroy().
 */
void s3_overlay_destroy(struct s3_overlay *ov);

/**
 * Record a durable write.
 *
 * \param data NULL records "these blocks are zero" (write_zeroes); otherwise it
 *             must hold nblocks * block_size bytes, which are copied.
 * \param seq  the seq the WAL assigned. Writes older than what a block already
 *             holds are ignored.
 *
 * Returns -ENOMEM if a block buffer could not be allocated, in which case the
 * range was applied only in part.
 */
int s3_overlay_write(struct s3_overlay *ov, uint64_t lba, uint32_t nblocks,
		     const void *data, uint64_t seq);

/**
 * Forget everything a chunk holds, and bump its epoch so a flush already under
 * way cannot publish what it collected. Used by unmap.
 */
void s3_overlay_drop_chunk(struct s3_overlay *ov, uint64_t chunk_index);

/**
 * Overlay whatever is present on top of \c buf, which must already hold the
 * base (S3 content, or zeroes for an unallocated chunk).
 *
 * \return number of blocks that were overlaid.
 */
uint32_t s3_overlay_apply(struct s3_overlay *ov, uint64_t lba, uint32_t nblocks,
			  void *buf);

/**
 * True when every block of the range is present, so the base does not have to
 * be read at all.
 */
bool s3_overlay_covers(struct s3_overlay *ov, uint64_t lba, uint32_t nblocks);

/**
 * True when the chunk holds anything at all. is_zeroes() must consult this:
 * claiming an overlaid chunk reads as zero makes blobstore skip copy-on-write.
 */
bool s3_overlay_chunk_is_live(struct s3_overlay *ov, uint64_t chunk_index);

uint64_t s3_overlay_chunk_epoch(struct s3_overlay *ov, uint64_t chunk_index);

/* Backpressure: the caller must stop submitting and retry later. */
bool s3_overlay_is_full(const struct s3_overlay *ov);

uint64_t s3_overlay_get_bytes(const struct s3_overlay *ov);
uint64_t s3_overlay_get_live_chunks(const struct s3_overlay *ov);
void s3_overlay_get_stats(const struct s3_overlay *ov, struct s3_overlay_stats *out);

/* ==========================================================================
 * Flusher-facing interface
 * ========================================================================== */

bool s3_overlay_has_dirty(const struct s3_overlay *ov);

/* When to stop holding a dirty chunk back. Zero in any field selects its
 * default. */
struct s3_overlay_flush_policy {
	uint64_t max_age_us;   /* S3_OVERLAY_FLUSH_MAX_AGE_US */
	uint32_t full_pct;     /* S3_OVERLAY_FLUSH_FULL_PCT */
	uint32_t high_pct;     /* S3_OVERLAY_FLUSH_HIGH_PCT */
};

/**
 * Change when dirty chunks become eligible for flushing.
 *
 * Uploading a chunk the moment it is first written is the worst case for
 * everything downstream: a 4 KiB write turns into a 1 MiB GET plus a 1 MiB PUT
 * (the chunk is not fully covered, so the old object has to be read back and
 * merged), one journal record, and one chunk-sized buffer assembled on the owner
 * thread. Waiting until the chunk is fully dirty removes the GET entirely and
 * amortises the rest over however many writes landed in the meantime.
 *
 * What makes the wait safe is that the data is already durable in the WAL before
 * any of this: holding it back costs a longer replay after a crash, not a lost
 * write. What makes it bounded is max_age_us and high_pct.
 */
void s3_overlay_set_flush_policy(struct s3_overlay *ov,
				 const struct s3_overlay_flush_policy *policy);

/**
 * Take the next chunk that needs flushing off the queue.
 *
 * \param force ignore the policy and take the oldest dirty chunk, whatever its
 *              age or fill. Required by a drain: "everything acknowledged is in
 *              S3" cannot be satisfied by waiting.
 *
 * \return 0 with \c out_chunk_index set; -ENOENT when nothing is dirty;
 *         **-EAGAIN when something is dirty but the policy says to keep holding
 *         it**. A caller that treats -EAGAIN as -ENOENT still behaves correctly,
 *         it just loses the distinction between "idle" and "waiting".
 */
int s3_overlay_next_dirty(struct s3_overlay *ov, bool force,
			  uint64_t *out_chunk_index);

/**
 * Claim every dirty block of a chunk for one flush round.
 *
 * \return 0 and fills \c view; -ENOENT nothing dirty; -EBUSY a flush is already
 *         running on this chunk (per-chunk single flight is what serialises the
 *         read-modify-write).
 */
int s3_overlay_flush_begin(struct s3_overlay *ov, uint64_t chunk_index,
			   struct s3_overlay_flush_view *view);

/**
 * Copy everything the chunk holds into a chunk-sized buffer, at the blocks'
 * offsets. Blocks known to be zero are written as zeroes; blocks the overlay
 * does not hold are left untouched, so the caller may pre-fill them from the old
 * object.
 *
 * Blocks re-dirtied after flush_begin() are included as well, so the object
 * being written never has a hole where a previous version used to be. They stay
 * dirty regardless and are uploaded again next round.
 */
void s3_overlay_flush_merge(struct s3_overlay *ov,
			    const struct s3_overlay_flush_view *view,
			    void *chunk_buf);

/**
 * End the flush round. On success the claimed blocks are released; on failure
 * they go back to being dirty and the chunk is re-queued.
 */
void s3_overlay_flush_end(struct s3_overlay *ov, uint64_t chunk_index,
			  bool success);

/**
 * Lowest seq still held anywhere, i.e. the oldest WAL entry the flusher has not
 * yet consumed. UINT64_MAX when nothing is held, meaning the whole log is
 * consumed.
 */
uint64_t s3_overlay_min_seq(const struct s3_overlay *ov);

#endif /* S3LVOL_OVERLAY_H */
