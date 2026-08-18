/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_overlay -- RAM view of WAL-durable, not-yet-in-S3 data
 *
 *   Rationale and the flush handshake are in include/s3lvol/s3_overlay.h. Notes
 *   that only matter while reading the code:
 *
 *   1. *The chunk index is a dense pointer array*, not a hash. Same reasoning as
 *      the chunk map: chunks are fixed size so the index is plain division, and
 *      8 bytes per chunk (1 TiB of 1 MiB chunks is 8 MiB) buys O(1) lookup with
 *      no collision handling. The per-chunk struct itself is allocated lazily,
 *      so an idle overlay costs only the array.
 *   2. *Three queues.* ripe_q and aging_q together hold the chunks the flusher
 *      should pick up, split by whether the hold-back policy is still waiting on
 *      them (see below); live_q holds every chunk holding anything, and exists
 *      purely so min_seq can be computed by walking the live chunks instead of
 *      the whole address space.
 *   3. A chunk with a flush in progress is deliberately *not* on either dirty
 *      queue even when new writes have re-dirtied it. It is re-queued by
 *      flush_end. That keeps per-chunk single flight without the flusher having
 *      to skip entries it just popped.
 *
 *   === Why the dirty set is two queues and not one ===
 *
 *   It started as one FIFO ordered by the time a chunk became dirty, with
 *   next_dirty looking only at the head -- the head is the oldest, so if it is
 *   too young to flush then so is everything behind it.
 *
 *   That reasoning is sound for the age and wrong for everything else, because
 *   fullness is not ordered by dirty time. One partially dirty chunk at the head
 *   holds up every full chunk behind it, and full is the condition worth acting
 *   on: a full chunk uploads without reading the old object back first.
 *
 *   Measured, writing 32 MiB sequentially to a fresh volume: 32 chunks filled
 *   completely and became eligible immediately, and all 32 waited 45 seconds --
 *   the whole hold-back age -- behind a single chunk holding two blocks of
 *   blobstore metadata. The burst that followed was 32 "full" flushes right
 *   after the one "aged" flush that unblocked them. A filesystem writes metadata
 *   constantly and those chunks are almost never full, so the head is very often
 *   exactly this kind of blocker.
 *
 *   So chunks are kept in two queues instead: ripe_q for the ones the policy is
 *   done with, aging_q in dirty-time order for the rest. next_dirty drains
 *   ripe_q first and only then considers the age of aging_q's head. Both are
 *   still O(1) and nothing scans -- a chunk moves between them at the moment its
 *   dirty count crosses the threshold, which is already a place doing per-chunk
 *   bookkeeping.
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/util.h"

#include "s3lvol/s3_overlay.h"

enum overlay_block_state {
	OVERLAY_ABSENT = 0,   /* S3 is authoritative */
	OVERLAY_DATA,
	OVERLAY_ZERO,
};

struct overlay_block {
	/* One block of data, NULL unless state is OVERLAY_DATA. */
	void            *data;

	/* seq of the WAL entry this content came from. */
	uint64_t         seq;

	uint8_t          state;

	/* Claimed by the flush round currently in progress. */
	bool             flushing;
};

struct overlay_chunk {
	uint64_t         index;

	/* Bumped on drop so an in-flight flush can tell its data was discarded. */
	uint64_t         epoch;

	uint32_t         n_present;
	uint32_t         n_dirty;
	uint32_t         n_flushing;

	/* Minimum seq over present blocks; UINT64_MAX when none. */
	uint64_t         min_seq;

	/* When this chunk first became dirty with nothing already in flight,
	 * i.e. how long the flusher has been holding it back.
	 *
	 * *Set only when the chunk goes from clean to dirty*, never refreshed by
	 * a later write. Refreshing it would starve exactly the chunk that needs
	 * flushing most: one that is written continuously would have its clock
	 * reset before the age ever elapsed, and would sit in RAM until the high
	 * water mark forced it out. */
	uint64_t         dirty_since_tsc;

	bool             queued;
	/* Which of the two dirty queues, meaningful only while queued. */
	bool             ripe;
	bool             flush_active;

	TAILQ_ENTRY(overlay_chunk) dirty_link;
	TAILQ_ENTRY(overlay_chunk) live_link;

	struct overlay_block blocks[];
};

struct s3_overlay {
	uint32_t         block_size;
	uint32_t         chunk_size;
	uint32_t         blocks_per_chunk;

	uint64_t         num_chunks;
	struct overlay_chunk **chunks;

	/* Chunks the policy is done holding back, in no particular order: they are
	 * all equally ready and nothing distinguishes them. */
	TAILQ_HEAD(, overlay_chunk) ripe_q;
	/* The rest, oldest first, so the head is the one closest to ageing out. */
	TAILQ_HEAD(, overlay_chunk) aging_q;
	TAILQ_HEAD(, overlay_chunk) live_q;

	uint64_t         bytes;
	uint64_t         max_bytes;
	uint64_t         live_chunks;
	uint64_t         dirty_chunks;

	uint32_t         flushes_active;

	/* Resolved policy: no zeroes, so the hot path does no defaulting. */
	uint64_t         flush_max_age_tsc;
	uint32_t         flush_full_blocks;   /* full_pct as a block count */
	uint64_t         flush_high_bytes;    /* high_pct as a byte count */

	struct s3_overlay_stats stats;
};

/* ==========================================================================
 * Chunk lifecycle
 * ========================================================================== */

static struct overlay_chunk *
overlay_chunk_get(struct s3_overlay *ov, uint64_t chunk_index)
{
	if (chunk_index >= ov->num_chunks) {
		return NULL;
	}
	return ov->chunks[chunk_index];
}

static struct overlay_chunk *
overlay_chunk_create(struct s3_overlay *ov, uint64_t chunk_index)
{
	struct overlay_chunk *c;
	size_t sz;

	sz = sizeof(*c) + (size_t)ov->blocks_per_chunk * sizeof(struct overlay_block);

	c = calloc(1, sz);
	if (!c) {
		return NULL;
	}

	c->index   = chunk_index;
	c->min_seq = UINT64_MAX;

	ov->chunks[chunk_index] = c;
	TAILQ_INSERT_TAIL(&ov->live_q, c, live_link);
	ov->live_chunks++;

	return c;
}

/* Whether the chunk is dirty enough that a flush would not have to read the old
 * object back first. Reaching this is what the hold-back is waiting for. */
static inline bool
overlay_chunk_is_full(const struct s3_overlay *ov, const struct overlay_chunk *c)
{
	return c->n_dirty >= ov->flush_full_blocks;
}

static void
overlay_chunk_enqueue(struct s3_overlay *ov, struct overlay_chunk *c)
{
	bool ripe;

	/* A chunk being flushed stays off the queues; flush_end re-queues it. */
	if (c->flush_active || c->n_dirty == 0) {
		return;
	}

	ripe = overlay_chunk_is_full(ov, c);

	if (c->queued) {
		/* Already waiting. The one thing that can have changed is that the
		 * write which led here filled it up, in which case it graduates to
		 * ripe_q -- this is the promotion that keeps a full chunk from
		 * waiting behind a partial one. Nothing ever moves the other way:
		 * a chunk's dirty count only grows until it is flushed. */
		if (ripe && !c->ripe) {
			TAILQ_REMOVE(&ov->aging_q, c, dirty_link);
			TAILQ_INSERT_TAIL(&ov->ripe_q, c, dirty_link);
			c->ripe = true;
		}
		return;
	}

	if (ripe) {
		TAILQ_INSERT_TAIL(&ov->ripe_q, c, dirty_link);
	} else {
		/* Tail, so aging_q stays ordered by the time each chunk became
		 * dirty and its head is always the one closest to ageing out. */
		TAILQ_INSERT_TAIL(&ov->aging_q, c, dirty_link);
	}
	c->ripe   = ripe;
	c->queued = true;
	ov->dirty_chunks++;

	/* Only when the clock is not already running, so that a chunk written to
	 * again and again cannot keep pushing its own deadline out. It is cleared
	 * when the chunk stops being dirty (flush_begin, or the last block going
	 * away), which is what makes "not already running" mean "clean until
	 * now".
	 *
	 * A chunk dirtied while its own flush is in flight takes its timestamp
	 * here, at flush_end, rather than at the write -- the branch above sends
	 * it home while flush_active. That is at most one round late and keeps the
	 * timestamp in one place. */
	if (c->dirty_since_tsc == 0) {
		c->dirty_since_tsc = spdk_get_ticks();
	}
}

static void
overlay_chunk_dequeue(struct s3_overlay *ov, struct overlay_chunk *c)
{
	if (!c->queued) {
		return;
	}
	/* Not a conditional expression around one TAILQ_REMOVE: the two heads are
	 * distinct anonymous struct types, so the compiler rejects mixing them. */
	if (c->ripe) {
		TAILQ_REMOVE(&ov->ripe_q, c, dirty_link);
	} else {
		TAILQ_REMOVE(&ov->aging_q, c, dirty_link);
	}
	c->queued = false;
	c->ripe   = false;
	assert(ov->dirty_chunks > 0);
	ov->dirty_chunks--;
}

/* Release the chunk once it holds nothing and nobody refers to it. */
static void
overlay_chunk_maybe_free(struct s3_overlay *ov, struct overlay_chunk *c)
{
	if (c->n_present > 0 || c->flush_active || c->queued) {
		return;
	}

	TAILQ_REMOVE(&ov->live_q, c, live_link);
	assert(ov->live_chunks > 0);
	ov->live_chunks--;

	ov->chunks[c->index] = NULL;
	free(c);
}

/* ==========================================================================
 * Block accounting
 * ========================================================================== */

static void
overlay_block_release(struct s3_overlay *ov, struct overlay_chunk *c,
		      struct overlay_block *b)
{
	if (b->state == OVERLAY_ABSENT) {
		return;
	}

	if (b->data) {
		free(b->data);
		b->data = NULL;
		assert(ov->bytes >= ov->block_size);
		ov->bytes -= ov->block_size;
	}

	if (b->flushing) {
		assert(c->n_flushing > 0);
		c->n_flushing--;
	} else {
		assert(c->n_dirty > 0);
		c->n_dirty--;
		if (c->n_dirty == 0) {
			/* Clean again, so the next write starts a fresh clock. */
			c->dirty_since_tsc = 0;
		}
	}
	assert(c->n_present > 0);
	c->n_present--;

	b->state    = OVERLAY_ABSENT;
	b->flushing = false;
	b->seq      = 0;
}

/* Recompute min_seq from scratch. Only called when blocks go away, which is
 * rare compared to writes, so an incremental scheme is not worth the risk of
 * getting it subtly wrong. */
static void
overlay_chunk_recalc_min_seq(struct s3_overlay *ov, struct overlay_chunk *c)
{
	uint64_t min = UINT64_MAX;

	for (uint32_t i = 0; i < ov->blocks_per_chunk; i++) {
		if (c->blocks[i].state != OVERLAY_ABSENT &&
		    c->blocks[i].seq < min) {
			min = c->blocks[i].seq;
		}
	}
	c->min_seq = min;
}

/* ==========================================================================
 * Create / destroy
 * ========================================================================== */

int
s3_overlay_create(uint64_t total_blocks, uint32_t block_size,
		  uint32_t chunk_size, uint64_t max_bytes,
		  struct s3_overlay **out)
{
	struct s3_overlay *ov;
	uint64_t num_chunks;

	if (!out || total_blocks == 0 || block_size == 0 || chunk_size == 0) {
		return -EINVAL;
	}
	if (chunk_size % block_size != 0) {
		SPDK_ERRLOG("chunk_size %u is not a multiple of block_size %u\n",
			    chunk_size, block_size);
		return -EINVAL;
	}

	num_chunks = spdk_divide_round_up(total_blocks * (uint64_t)block_size,
					  chunk_size);

	ov = calloc(1, sizeof(*ov));
	if (!ov) {
		return -ENOMEM;
	}

	ov->chunks = calloc(num_chunks, sizeof(*ov->chunks));
	if (!ov->chunks) {
		free(ov);
		return -ENOMEM;
	}

	ov->block_size       = block_size;
	ov->chunk_size       = chunk_size;
	ov->blocks_per_chunk = chunk_size / block_size;
	ov->num_chunks       = num_chunks;
	ov->max_bytes        = max_bytes ? max_bytes : S3_OVERLAY_DEFAULT_MAX_BYTES;

	TAILQ_INIT(&ov->ripe_q);
	TAILQ_INIT(&ov->aging_q);
	TAILQ_INIT(&ov->live_q);

	/* Resolves the defaults, and logs them. Must come after max_bytes and
	 * blocks_per_chunk, both of which it converts into absolute thresholds. */
	s3_overlay_set_flush_policy(ov, NULL);

	SPDK_NOTICELOG("Overlay created: %" PRIu64 " chunks x %u bytes, "
		       "%u blocks per chunk, %" PRIu64 " MiB cap\n",
		       num_chunks, chunk_size, ov->blocks_per_chunk,
		       ov->max_bytes / (1024 * 1024));

	*out = ov;
	return 0;
}

void
s3_overlay_destroy(struct s3_overlay *ov)
{
	struct overlay_chunk *c, *tmp;

	if (!ov) {
		return;
	}

	/* A flush in progress means an upload completion is still going to call
	 * back in here. Same reasoning as s3_chunk_map_destroy(): leak rather
	 * than hand out freed memory. */
	assert(ov->flushes_active == 0);
	if (ov->flushes_active != 0) {
		SPDK_ERRLOG("s3_overlay_destroy() called with %u flushes in "
			    "flight; leaking the overlay to avoid "
			    "use-after-free\n", ov->flushes_active);
		return;
	}

	if (ov->bytes > 0) {
		/* Not lost -- it is still in the WAL and will be replayed -- but
		 * it did not reach S3, so say so rather than exiting quietly. */
		SPDK_WARNLOG("Overlay destroyed holding %" PRIu64 " bytes across "
			     "%" PRIu64 " chunks that never reached S3; they stay "
			     "in the WAL for replay\n", ov->bytes, ov->live_chunks);
	}

	TAILQ_FOREACH_SAFE(c, &ov->live_q, live_link, tmp) {
		for (uint32_t i = 0; i < ov->blocks_per_chunk; i++) {
			free(c->blocks[i].data);
		}
		TAILQ_REMOVE(&ov->live_q, c, live_link);
		free(c);
	}

	free(ov->chunks);
	free(ov);
}

/* ==========================================================================
 * Write
 * ========================================================================== */

int
s3_overlay_write(struct s3_overlay *ov, uint64_t lba, uint32_t nblocks,
		 const void *data, uint64_t seq)
{
	const uint8_t *src = data;
	uint64_t cur = lba;
	uint32_t left = nblocks;

	if (!ov || nblocks == 0) {
		return -EINVAL;
	}

	ov->stats.writes++;

	while (left > 0) {
		uint64_t chunk_index = cur / ov->blocks_per_chunk;
		uint32_t first = (uint32_t)(cur % ov->blocks_per_chunk);
		uint32_t this_blocks = spdk_min(left, ov->blocks_per_chunk - first);
		struct overlay_chunk *c;

		if (chunk_index >= ov->num_chunks) {
			SPDK_ERRLOG("Overlay write out of range: lba %" PRIu64 "\n", cur);
			return -EINVAL;
		}

		c = overlay_chunk_get(ov, chunk_index);
		if (!c) {
			c = overlay_chunk_create(ov, chunk_index);
			if (!c) {
				return -ENOMEM;
			}
		}

		for (uint32_t i = 0; i < this_blocks; i++) {
			struct overlay_block *b = &c->blocks[first + i];
			void *nbuf = NULL;

			/* Monotonic per block: this is what makes replay order
			 * independent (see the header). */
			if (b->state != OVERLAY_ABSENT && b->seq > seq) {
				ov->stats.blocks_dropped++;
				continue;
			}

			/* Allocate before touching any state. Failing halfway
			 * through the accounting would leave the block claiming
			 * content it does not have, and a block that wrongly reads
			 * as zero is worse than a failed write. */
			if (src && !b->data) {
				nbuf = malloc(ov->block_size);
				if (!nbuf) {
					overlay_chunk_enqueue(ov, c);
					return -ENOMEM;
				}
			}

			if (b->state == OVERLAY_ABSENT) {
				c->n_present++;
				c->n_dirty++;
			} else if (b->flushing) {
				/* Written while being flushed: it must survive
				 * flush_end, so take it back out of the flush. */
				b->flushing = false;
				assert(c->n_flushing > 0);
				c->n_flushing--;
				c->n_dirty++;
			}

			if (src) {
				if (nbuf) {
					b->data = nbuf;
					ov->bytes += ov->block_size;
				}
				memcpy(b->data, src + (size_t)i * ov->block_size,
				       ov->block_size);
				b->state = OVERLAY_DATA;
			} else {
				/* write_zeroes: drop the buffer, keep the fact. */
				if (b->data) {
					free(b->data);
					b->data = NULL;
					assert(ov->bytes >= ov->block_size);
					ov->bytes -= ov->block_size;
				}
				b->state = OVERLAY_ZERO;
			}

			b->seq = seq;
			if (seq < c->min_seq) {
				c->min_seq = seq;
			}
			ov->stats.blocks_written++;
		}

		if (ov->bytes > ov->stats.peak_bytes) {
			ov->stats.peak_bytes = ov->bytes;
		}

		overlay_chunk_enqueue(ov, c);

		if (src) {
			src += (size_t)this_blocks * ov->block_size;
		}
		cur  += this_blocks;
		left -= this_blocks;
	}

	return 0;
}

void
s3_overlay_drop_chunk(struct s3_overlay *ov, uint64_t chunk_index)
{
	struct overlay_chunk *c;

	if (!ov) {
		return;
	}

	c = overlay_chunk_get(ov, chunk_index);
	if (!c) {
		return;
	}

	for (uint32_t i = 0; i < ov->blocks_per_chunk; i++) {
		overlay_block_release(ov, c, &c->blocks[i]);
	}

	c->min_seq = UINT64_MAX;

	/* Invalidate any flush that already collected this chunk: publishing it
	 * afterwards would resurrect data the caller just unmapped. */
	c->epoch++;

	overlay_chunk_dequeue(ov, c);
	overlay_chunk_maybe_free(ov, c);
}

/* ==========================================================================
 * Read
 * ========================================================================== */

uint32_t
s3_overlay_apply(struct s3_overlay *ov, uint64_t lba, uint32_t nblocks, void *buf)
{
	uint8_t *dst = buf;
	uint64_t cur = lba;
	uint32_t left = nblocks;
	uint32_t applied = 0;

	if (!ov || !buf || nblocks == 0) {
		return 0;
	}

	while (left > 0) {
		uint64_t chunk_index = cur / ov->blocks_per_chunk;
		uint32_t first       = (uint32_t)(cur % ov->blocks_per_chunk);
		uint32_t this_blocks = spdk_min(left, ov->blocks_per_chunk - first);
		struct overlay_chunk *c = overlay_chunk_get(ov, chunk_index);

		if (c) {
			for (uint32_t i = 0; i < this_blocks; i++) {
				struct overlay_block *b = &c->blocks[first + i];
				uint8_t *slot = dst + (size_t)i * ov->block_size;

				if (b->state == OVERLAY_DATA) {
					memcpy(slot, b->data, ov->block_size);
					applied++;
				} else if (b->state == OVERLAY_ZERO) {
					memset(slot, 0, ov->block_size);
					applied++;
				}
			}
		}

		dst  += (size_t)this_blocks * ov->block_size;
		cur  += this_blocks;
		left -= this_blocks;
	}

	return applied;
}

bool
s3_overlay_covers(struct s3_overlay *ov, uint64_t lba, uint32_t nblocks)
{
	uint64_t cur = lba;
	uint32_t left = nblocks;

	if (!ov || nblocks == 0) {
		return false;
	}

	while (left > 0) {
		uint64_t chunk_index = cur / ov->blocks_per_chunk;
		uint32_t first       = (uint32_t)(cur % ov->blocks_per_chunk);
		uint32_t this_blocks = spdk_min(left, ov->blocks_per_chunk - first);
		struct overlay_chunk *c = overlay_chunk_get(ov, chunk_index);

		if (!c) {
			return false;
		}
		for (uint32_t i = 0; i < this_blocks; i++) {
			if (c->blocks[first + i].state == OVERLAY_ABSENT) {
				return false;
			}
		}

		cur  += this_blocks;
		left -= this_blocks;
	}

	return true;
}

bool
s3_overlay_chunk_is_live(struct s3_overlay *ov, uint64_t chunk_index)
{
	struct overlay_chunk *c = ov ? overlay_chunk_get(ov, chunk_index) : NULL;

	return c && c->n_present > 0;
}

uint64_t
s3_overlay_chunk_epoch(struct s3_overlay *ov, uint64_t chunk_index)
{
	struct overlay_chunk *c = ov ? overlay_chunk_get(ov, chunk_index) : NULL;

	/* A chunk that no longer exists cannot have been dropped since a flush
	 * started, because a flush keeps its chunk alive. Reporting 0 for it is
	 * therefore unambiguous. */
	return c ? c->epoch : 0;
}

/* ==========================================================================
 * Flush handshake
 * ========================================================================== */

bool
s3_overlay_has_dirty(const struct s3_overlay *ov)
{
	return ov && (!TAILQ_EMPTY(&ov->ripe_q) || !TAILQ_EMPTY(&ov->aging_q));
}

/* Whether the oldest chunk still being held back should be let go anyway.
 *
 * Only ever asked about aging_q's head. Fullness is not considered here: a chunk
 * that filled up was moved to ripe_q when it did, and next_dirty drains that
 * queue first, so anything still on aging_q is by definition not full. */
static bool
overlay_aging_head_expired(struct s3_overlay *ov, struct overlay_chunk *c)
{
	/* Over the high water mark: stop holding anything back. Checked first
	 * because it is the one condition about the overlay as a whole rather
	 * than this chunk, and because getting it wrong ends in
	 * s3_overlay_is_full() and retried writes. */
	if (ov->bytes >= ov->flush_high_bytes) {
		ov->stats.flushed_forced++;
		return true;
	}

	/* Held long enough. Bounds both the replay after a crash and how long a
	 * write that will never be followed by another can sit in RAM. */
	if (spdk_get_ticks() - c->dirty_since_tsc >= ov->flush_max_age_tsc) {
		ov->stats.flushed_aged++;
		return true;
	}

	return false;
}

void
s3_overlay_set_flush_policy(struct s3_overlay *ov,
			    const struct s3_overlay_flush_policy *policy)
{
	uint64_t age_us  = S3_OVERLAY_FLUSH_MAX_AGE_US;
	uint32_t full    = S3_OVERLAY_FLUSH_FULL_PCT;
	uint32_t high    = S3_OVERLAY_FLUSH_HIGH_PCT;

	if (!ov) {
		return;
	}
	if (policy) {
		if (policy->max_age_us) {
			age_us = policy->max_age_us;
		}
		if (policy->full_pct) {
			full = spdk_min(policy->full_pct, 100);
		}
		if (policy->high_pct) {
			high = spdk_min(policy->high_pct, 100);
		}
	}

	ov->flush_max_age_tsc = age_us * (spdk_get_ticks_hz() / 1000000);

	/* Rounded up, so full_pct == 100 needs every block and not one fewer:
	 * covers_prefix is all-or-nothing and a threshold that rounded down would
	 * flush one block short of it every time. */
	ov->flush_full_blocks = (uint32_t)(((uint64_t)ov->blocks_per_chunk * full
					    + 99) / 100);
	if (ov->flush_full_blocks == 0) {
		ov->flush_full_blocks = 1;
	}

	ov->flush_high_bytes = ov->max_bytes / 100 * high;

	SPDK_NOTICELOG("Overlay flush policy: hold a dirty chunk for up to %"
		       PRIu64 " ms, or until %u/%u blocks are dirty; flush "
		       "everything past %" PRIu64 " KiB held (cap %" PRIu64
		       " MiB)\n",
		       age_us / 1000, ov->flush_full_blocks,
		       ov->blocks_per_chunk,
		       ov->flush_high_bytes / 1024,
		       ov->max_bytes / (1024 * 1024));
}

int
s3_overlay_next_dirty(struct s3_overlay *ov, bool force,
		      uint64_t *out_chunk_index)
{
	struct overlay_chunk *c;

	if (!ov || !out_chunk_index) {
		return -EINVAL;
	}

	/* Ripe first, always. These are the chunks that upload without reading the
	 * old object back, so they are both the cheapest work available and the
	 * work the hold-back was waiting to get. */
	c = TAILQ_FIRST(&ov->ripe_q);
	if (c) {
		ov->stats.flushed_full++;
		goto take;
	}

	/* Nothing ripe, so consider the oldest chunk still being held back. Only
	 * the head, which keeps this O(1) and is sound now that fullness cannot be
	 * hiding further down: aging_q is ordered by dirty time, so if its head has
	 * not aged out, nothing behind it has either. */
	c = TAILQ_FIRST(&ov->aging_q);
	if (!c) {
		return -ENOENT;
	}

	if (force) {
		/* Counted here rather than in overlay_aging_head_expired, which
		 * force skips entirely: without this a drain would hand out chunks
		 * attributed to no reason at all, and the three counters would not
		 * add up to what the flusher reports. */
		ov->stats.flushed_forced++;
	} else if (!overlay_aging_head_expired(ov, c)) {
		return -EAGAIN;
	}

take:
	overlay_chunk_dequeue(ov, c);
	*out_chunk_index = c->index;

	return 0;
}

int
s3_overlay_flush_begin(struct s3_overlay *ov, uint64_t chunk_index,
		       struct s3_overlay_flush_view *view)
{
	struct overlay_chunk *c;
	uint32_t last =0;
	uint32_t claimed = 0;
	uint64_t max_seq = 0;
	bool have_last = false;

	if (!ov || !view) {
		return -EINVAL;
	}

	c = overlay_chunk_get(ov, chunk_index);
	if (!c) {
		return -ENOENT;
	}
	/* Per-chunk single flight. This is the serialisation that turns N
	 * concurrent partial writes into one read-modify-write.
	 *
	 * Checked before the dirty count on purpose: a chunk with a live flush
	 * has n_dirty == 0 until new writes arrive, and reporting that as "not
	 * dirty" would hide the real reason. */
	if (c->flush_active) {
		return -EBUSY;
	}
	if (c->n_dirty == 0) {
		return -ENOENT;
	}

	/* The flusher takes chunks off the queue itself, but do not rely on it:
	 * leaving a chunk queued with nothing dirty makes has_dirty() lie. */
	overlay_chunk_dequeue(ov, c);

	for (uint32_t i = 0; i < ov->blocks_per_chunk; i++) {
		struct overlay_block *b = &c->blocks[i];

		if (b->state == OVERLAY_ABSENT || b->flushing) {
			continue;
		}
		b->flushing = true;
		claimed++;
		last = i;
		have_last = true;
		if (b->seq > max_seq) {
			max_seq = b->seq;
		}
	}

	assert(claimed == c->n_dirty);
	assert(have_last);
	(void)have_last;

	c->n_flushing += claimed;
	c->n_dirty     = 0;
	/* Nothing is dirty any more, so the hold-back clock stops. Writes
	 * arriving during the round restart it from flush_end. */
	c->dirty_since_tsc = 0;
	c->flush_active = true;
	ov->flushes_active++;
	ov->stats.flushes_begun++;

	view->chunk_index = chunk_index;
	view->epoch       = c->epoch;
	view->max_seq     = max_seq;
	view->nblocks     = claimed;
	view->end_offset  = (last + 1) * ov->block_size;
	/* Every block below end_offset claimed means the old object contributes
	 * nothing and can be skipped entirely -- no GET, no read amplification. */
	view->covers_prefix = (claimed == last + 1);

	return 0;
}

/* Copy everything the chunk currently holds into the buffer.
 *
 * Note this includes blocks that were *re-dirtied after* flush_begin, not just
 * the ones the round claimed. That matters: a write arriving mid-round takes its
 * block back out of the round (so flush_end cannot drop content that was never
 * uploaded), and if merge skipped those blocks the object being written would
 * have a hole where the previous version used to be. Uploading the newer content
 * instead costs nothing -- the block stays dirty either way and is uploaded again
 * next round -- and the object is never wrong in the meantime. */
void
s3_overlay_flush_merge(struct s3_overlay *ov,
		       const struct s3_overlay_flush_view *view,
		       void *chunk_buf)
{
	struct overlay_chunk *c;
	uint8_t *base = chunk_buf;

	if (!ov || !view || !chunk_buf) {
		return;
	}

	c = overlay_chunk_get(ov, view->chunk_index);
	if (!c) {
		return;
	}

	for (uint32_t i = 0; i < ov->blocks_per_chunk; i++) {
		struct overlay_block *b = &c->blocks[i];

		if (b->state == OVERLAY_DATA) {
			memcpy(base + (size_t)i * ov->block_size, b->data,
			       ov->block_size);
		} else if (b->state == OVERLAY_ZERO) {
			memset(base + (size_t)i * ov->block_size, 0,
			       ov->block_size);
		}
	}
}

void
s3_overlay_flush_end(struct s3_overlay *ov, uint64_t chunk_index, bool success)
{
	struct overlay_chunk *c;

	if (!ov) {
		return;
	}

	c = overlay_chunk_get(ov, chunk_index);
	if (!c) {
		/* Only reachable if the chunk was freed while a flush was live,
		 * which overlay_chunk_maybe_free() refuses to do. */
		SPDK_ERRLOG("Overlay flush_end for unknown chunk %" PRIu64 "\n",
			    chunk_index);
		return;
	}

	assert(c->flush_active);
	assert(ov->flushes_active > 0);

	for (uint32_t i = 0; i < ov->blocks_per_chunk; i++) {
		struct overlay_block *b = &c->blocks[i];

		if (!b->flushing) {
			continue;
		}
		if (success) {
			overlay_block_release(ov, c, b);
		} else {
			/* Back to dirty: a failed upload must lose nothing. */
			b->flushing = false;
			assert(c->n_flushing > 0);
			c->n_flushing--;
			c->n_dirty++;
		}
	}

	if (!success) {
		ov->stats.flushes_failed++;
	}

	c->flush_active = false;
	ov->flushes_active--;

	overlay_chunk_recalc_min_seq(ov, c);

	/* Writes that arrived during the flush, or everything if it failed. */
	overlay_chunk_enqueue(ov, c);
	overlay_chunk_maybe_free(ov, c);
}

uint64_t
s3_overlay_min_seq(const struct s3_overlay *ov)
{
	struct overlay_chunk *c;
	uint64_t min = UINT64_MAX;

	if (!ov) {
		return UINT64_MAX;
	}

	/* Walks live chunks only. That set is bounded by max_bytes / chunk_size
	 * (a few hundred at the default sizes), which is why this can be a scan
	 * rather than another index to keep consistent. */
	TAILQ_FOREACH(c, &ov->live_q, live_link) {
		if (c->min_seq < min) {
			min = c->min_seq;
		}
	}

	return min;
}

/* ==========================================================================
 * Accessors
 * ========================================================================== */

bool
s3_overlay_is_full(const struct s3_overlay *ov)
{
	return ov && ov->bytes >= ov->max_bytes;
}

uint64_t
s3_overlay_get_bytes(const struct s3_overlay *ov)
{
	return ov ? ov->bytes : 0;
}

uint64_t
s3_overlay_get_live_chunks(const struct s3_overlay *ov)
{
	return ov ? ov->live_chunks : 0;
}

void
s3_overlay_get_stats(const struct s3_overlay *ov, struct s3_overlay_stats *out)
{
	if (!ov || !out) {
		return;
	}
	*out = ov->stats;
}

SPDK_LOG_REGISTER_COMPONENT(s3_overlay)
