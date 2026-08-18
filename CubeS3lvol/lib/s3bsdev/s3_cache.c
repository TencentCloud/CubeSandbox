/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Local read cache for S3 chunk objects. Rationale and invariants are in
 *   include/s3lvol/s3_cache.h; this file is the mechanism.
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"
#include "spdk/util.h"

#include "s3lvol/s3_cache.h"

#define S3_CACHE_NO_SLOT        UINT32_MAX

struct cache_slot {
	/* Which object this holds. Only meaningful while resident. */
	struct spdk_uuid uuid;
	uint64_t         chunk_index;

	/* Length of the object, not of what is present here: reads past it are
	 * zeroes. Set when the slot is claimed, from the chunk map rather than
	 * from anything on the device, so it is trustworthy even before the first
	 * fill lands and stays so if one fails. */
	uint32_t         valid_bytes;

	/* Which blocks of the object are actually on the device. Points into
	 * cache->bitmaps; see the header for why this cannot be a scalar. */
	uint64_t        *bitmap;
	uint32_t         filled_blocks;   /* popcount of bitmap, maintained */

	/* In-flight reads. A pinned slot is off the LRU entirely rather than
	 * skipped during eviction, so that taking the LRU head is always a valid
	 * choice and eviction never has to scan. */
	uint32_t         pins;

	/* Populate writes in flight. A count rather than a flag because partial
	 * residency makes concurrent fills of *different* ranges of the same
	 * object both normal and useful -- a sequential read stream produces one
	 * per request. They need no exclusion against each other: a range is only
	 * marked present once its write has landed, so a read can never be served
	 * from a range still on its way. Fills of a *different* uuid are a
	 * different matter and stand aside (see s3_cache_populate). */
	uint32_t         fills;

	bool             resident;
	bool             on_lru;

	TAILQ_ENTRY(cache_slot) link;
};

struct cache_staging {
	void                    *buf;
	TAILQ_ENTRY(cache_staging) link;
};

/* One in-flight populate. */
struct cache_fill {
	struct s3_cache         *cache;
	struct cache_slot       *slot;
	struct cache_staging    *staging;

	/* The block range this fill makes present, to be marked on success. */
	uint32_t                 first_block;
	uint32_t                 n_blocks;
	uint32_t                 bytes;      /* real object bytes, for stats */
};

/* One in-flight read. */
struct cache_read {
	struct s3_cache         *cache;
	struct cache_slot       *slot;
	s3_cache_read_cb         cb_fn;
	void                    *cb_arg;

	/* Bytes past the object's end that have to be zeroed once the device read
	 * lands, and where they start in the caller's buffer. */
	void                    *buf;
	uint32_t                 zero_from;
	uint32_t                 zero_len;
};

struct s3_cache {
	struct spdk_bdev_desc   *desc;
	struct spdk_io_channel  *ch;
	struct spdk_thread      *owner_thread;

	uint64_t                 region_offset;
	uint32_t                 chunk_size;
	uint32_t                 block_size;
	uint32_t                 blocks_per_chunk;
	uint32_t                 bitmap_words;

	uint64_t                 n_slots;
	struct cache_slot       *slots;

	/* Residency bitmaps for every slot, one allocation: bitmap_words per
	 * slot, handed out at create time so a slot never has to compute or
	 * allocate its own. */
	uint64_t                *bitmaps;

	/* Dense chunk_index -> slot, same shape as the chunk map's own array.
	 * S3_CACHE_NO_SLOT when not cached. A hash table would save memory on a
	 * sparsely touched volume, but this is 4 bytes per chunk against the
	 * chunk map's ~32 and it makes lookup a single load. */
	uint64_t                 num_chunks;
	uint32_t                *chunk_to_slot;

	/* Resident, unpinned, no fill in flight. Head is the coldest. */
	TAILQ_HEAD(, cache_slot) lru;
	TAILQ_HEAD(, cache_slot) free_slots;

	TAILQ_HEAD(, cache_staging) staging_free;
	struct cache_staging    *staging;

	uint64_t                 resident;
	uint64_t                 resident_blocks;
	uint32_t                 fills_in_flight;
	uint32_t                 reads_in_flight;

	struct s3_cache_stats    stats;
};

/* ==========================================================================
 * Residency bitmap
 *
 * Plain and unvectorised on purpose: the ranges are a handful of blocks and the
 * cost that matters in this file is the device round trip, not this.
 * ========================================================================== */

static inline bool
bitmap_test_range(const uint64_t *bm, uint32_t first, uint32_t count)
{
	for (uint32_t b = first; b < first + count; b++) {
		if (!(bm[b / 64] & (1ULL << (b % 64)))) {
			return false;
		}
	}
	return true;
}

/* Returns how many bits went from clear to set, so the caller can keep a
 * popcount without rescanning. */
static inline uint32_t
bitmap_set_range(uint64_t *bm, uint32_t first, uint32_t count)
{
	uint32_t newly = 0;

	for (uint32_t b = first; b < first + count; b++) {
		uint64_t mask = 1ULL << (b % 64);

		if (!(bm[b / 64] & mask)) {
			bm[b / 64] |= mask;
			newly++;
		}
	}
	return newly;
}

static inline void
bitmap_clear_all(uint64_t *bm, uint32_t words)
{
	memset(bm, 0, (size_t)words * sizeof(*bm));
}

/* ==========================================================================
 * Slot bookkeeping
 * ========================================================================== */

static inline uint64_t
slot_offset(const struct s3_cache *cache, const struct cache_slot *slot)
{
	return cache->region_offset +
	       (uint64_t)(slot - cache->slots) * cache->chunk_size;
}

static void
slot_lru_remove(struct s3_cache *cache, struct cache_slot *slot)
{
	if (slot->on_lru) {
		TAILQ_REMOVE(&cache->lru, slot, link);
		slot->on_lru = false;
	}
}

/* Warmest end. Called when a slot becomes readable or stops being read. */
static void
slot_lru_touch(struct s3_cache *cache, struct cache_slot *slot)
{
	slot_lru_remove(cache, slot);
	if (slot->resident && slot->fills == 0 && slot->pins == 0) {
		TAILQ_INSERT_TAIL(&cache->lru, slot, link);
		slot->on_lru = true;
	}
}

/* Detach a slot from its chunk and return it to the free list. */
static void
slot_release(struct s3_cache *cache, struct cache_slot *slot)
{
	assert(slot->pins == 0);
	assert(slot->fills == 0);

	slot_lru_remove(cache, slot);

	if (slot->resident) {
		assert(cache->chunk_to_slot[slot->chunk_index] ==
		       (uint32_t)(slot - cache->slots));
		cache->chunk_to_slot[slot->chunk_index] = S3_CACHE_NO_SLOT;
		cache->resident--;
		slot->resident = false;
	}

	assert(cache->resident_blocks >= slot->filled_blocks);
	cache->resident_blocks -= slot->filled_blocks;

	/* Clearing the bitmap is not bookkeeping, it is the safety property: the
	 * device still holds this object's bytes, and the next tenant of the slot
	 * must not be able to serve them as its own. */
	bitmap_clear_all(slot->bitmap, cache->bitmap_words);
	slot->filled_blocks = 0;

	spdk_uuid_set_null(&slot->uuid);
	slot->valid_bytes = 0;
	TAILQ_INSERT_HEAD(&cache->free_slots, slot, link);
}

/* A slot to put a new object in, or NULL when everything is either pinned or
 * being filled. Returning NULL is normal under load and simply means the
 * populate is dropped. */
static struct cache_slot *
slot_acquire(struct s3_cache *cache)
{
	struct cache_slot *slot = TAILQ_FIRST(&cache->free_slots);

	if (slot) {
		TAILQ_REMOVE(&cache->free_slots, slot, link);
		return slot;
	}

	/* Evict the coldest. Everything on the LRU is unpinned and not filling by
	 * construction, so the head is always safe to take. */
	slot = TAILQ_FIRST(&cache->lru);
	if (!slot) {
		return NULL;
	}

	slot_release(cache, slot);
	cache->stats.evictions++;

	slot = TAILQ_FIRST(&cache->free_slots);
	assert(slot != NULL);
	TAILQ_REMOVE(&cache->free_slots, slot, link);

	return slot;
}

static struct cache_slot *
slot_for_chunk(struct s3_cache *cache, uint64_t chunk_index)
{
	uint32_t idx;

	if (chunk_index >= cache->num_chunks) {
		return NULL;
	}

	idx = cache->chunk_to_slot[chunk_index];
	if (idx == S3_CACHE_NO_SLOT) {
		return NULL;
	}

	assert(idx < cache->n_slots);
	return &cache->slots[idx];
}

/* ==========================================================================
 * Create / destroy
 * ========================================================================== */

int
s3_cache_create(const struct s3_cache_opts *opts, struct s3_cache **out)
{
	struct s3_cache *cache;
	uint64_t n_slots;
	uint32_t i;

	if (!opts || !out || !opts->desc || !opts->ch) {
		return -EINVAL;
	}
	if (opts->chunk_size == 0 || opts->block_size == 0 ||
	    opts->chunk_size % opts->block_size != 0) {
		return -EINVAL;
	}
	if (opts->num_chunks == 0) {
		return -EINVAL;
	}

	n_slots = opts->region_size / opts->chunk_size;
	if (n_slots == 0) {
		/* Not "a cache with no room": a caller that does not want a cache
		 * should not create one, and silently accepting a region too small
		 * to hold anything would hide a layout mistake. */
		return -EINVAL;
	}
	if (n_slots >= S3_CACHE_NO_SLOT) {
		/* The dense index stores slot numbers in 32 bits. */
		n_slots = S3_CACHE_NO_SLOT - 1;
	}

	cache = calloc(1, sizeof(*cache));
	if (!cache) {
		return -ENOMEM;
	}

	cache->desc          = opts->desc;
	cache->ch            = opts->ch;
	cache->owner_thread  = spdk_get_thread();
	cache->region_offset = opts->region_offset;
	cache->chunk_size    = opts->chunk_size;
	cache->block_size    = opts->block_size;
	cache->n_slots       = n_slots;
	cache->num_chunks    = opts->num_chunks;

	cache->blocks_per_chunk = opts->chunk_size / opts->block_size;
	cache->bitmap_words     = spdk_divide_round_up(cache->blocks_per_chunk, 64);

	TAILQ_INIT(&cache->lru);
	TAILQ_INIT(&cache->free_slots);
	TAILQ_INIT(&cache->staging_free);

	cache->slots = calloc(n_slots, sizeof(*cache->slots));
	cache->bitmaps = calloc(n_slots, (size_t)cache->bitmap_words *
				sizeof(*cache->bitmaps));
	cache->chunk_to_slot = malloc(opts->num_chunks *
				      sizeof(*cache->chunk_to_slot));
	cache->staging = calloc(S3_CACHE_STAGING_BUFS, sizeof(*cache->staging));
	if (!cache->slots || !cache->bitmaps || !cache->chunk_to_slot ||
	    !cache->staging) {
		s3_cache_destroy(cache);
		return -ENOMEM;
	}

	for (uint64_t s = 0; s < n_slots; s++) {
		cache->slots[s].bitmap = cache->bitmaps +
					 s * cache->bitmap_words;
		TAILQ_INSERT_TAIL(&cache->free_slots, &cache->slots[s], link);
	}
	for (uint64_t c = 0; c < opts->num_chunks; c++) {
		cache->chunk_to_slot[c] = S3_CACHE_NO_SLOT;
	}

	/* DMA aligned, because these go straight to the local bdev. A plain
	 * malloc'd buffer would make the bdev layer bounce every write through
	 * iobuf, and a chunk is larger than iobuf's large buffer by default. */
	for (i = 0; i < S3_CACHE_STAGING_BUFS; i++) {
		cache->staging[i].buf = spdk_dma_malloc(opts->chunk_size,
							opts->block_size, NULL);
		if (!cache->staging[i].buf) {
			s3_cache_destroy(cache);
			return -ENOMEM;
		}
		TAILQ_INSERT_TAIL(&cache->staging_free, &cache->staging[i], link);
	}

	cache->stats.slots_total = n_slots;

	SPDK_NOTICELOG("Chunk cache: %" PRIu64 " slots of %u KiB at offset %"
		       PRIu64 " (%" PRIu64 " MiB), %" PRIu64 " chunk index "
		       "entries, %u blocks per slot\n",
		       n_slots, opts->chunk_size / 1024, opts->region_offset,
		       (n_slots * opts->chunk_size) / (1024 * 1024),
		       opts->num_chunks, cache->blocks_per_chunk);

	*out = cache;
	return 0;
}

void
s3_cache_destroy(struct s3_cache *cache)
{
	if (!cache) {
		return;
	}

	/* In-flight I/O holds a pointer into slots[] and a staging buffer, so
	 * tearing down under it would be a use-after-free. Callers reach this
	 * only after the flusher and the read path are quiesced. */
	assert(cache->fills_in_flight == 0);
	assert(cache->reads_in_flight == 0);

	if (cache->staging) {
		for (uint32_t i = 0; i < S3_CACHE_STAGING_BUFS; i++) {
			spdk_dma_free(cache->staging[i].buf);
		}
		free(cache->staging);
	}
	free(cache->chunk_to_slot);
	free(cache->bitmaps);
	free(cache->slots);
	free(cache);
}

/* ==========================================================================
 * Read
 * ========================================================================== */

bool
s3_cache_lookup(struct s3_cache *cache, uint64_t chunk_index,
		const struct spdk_uuid *uuid)
{
	struct cache_slot *slot;

	if (!cache || !uuid) {
		return false;
	}
	assert(cache->owner_thread == spdk_get_thread());

	slot = slot_for_chunk(cache, chunk_index);
	if (!slot || !slot->resident) {
		return false;
	}
	if (spdk_uuid_compare(&slot->uuid, uuid) != 0) {
		return false;
	}

	/* Whole object present. Anything less is a legitimate cache state but not
	 * something this coarse question can report, so say no rather than let a
	 * caller read "cached" as "will hit". */
	return slot->filled_blocks ==
	       spdk_divide_round_up(slot->valid_bytes, cache->block_size);
}

static void
cache_read_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct cache_read *rd = cb_arg;
	struct s3_cache *cache = rd->cache;
	struct cache_slot *slot = rd->slot;
	int status = success ? 0 : -EIO;

	spdk_bdev_free_io(bdev_io);

	if (success && rd->zero_len) {
		memset((uint8_t *)rd->buf + rd->zero_from, 0, rd->zero_len);
	}

	assert(slot->pins > 0);
	slot->pins--;
	slot_lru_touch(cache, slot);

	cache->reads_in_flight--;

	if (!success) {
		/* The data is still in S3, so this is recoverable -- but not here:
		 * the caller has already been told the read is under way and only
		 * it knows how to reissue. Drop the entry so the retry misses
		 * rather than hitting the same bad slot again. */
		cache->stats.populates_failed++;
		if (slot->pins == 0 && slot->fills == 0) {
			slot_release(cache, slot);
		}
	}

	rd->cb_fn(rd->cb_arg, status);
	free(rd);
}

int
s3_cache_read(struct s3_cache *cache, uint64_t chunk_index,
	      const struct spdk_uuid *uuid, uint32_t offset_in_chunk,
	      uint32_t length, void *buf, s3_cache_read_cb cb_fn, void *cb_arg)
{
	struct cache_slot *slot;
	struct cache_read *rd;
	uint32_t readable, read_len;
	uint32_t first_block, n_blocks;
	int rc;

	if (!cache || !uuid || !buf || !cb_fn || length == 0) {
		return -ENOENT;
	}
	assert(cache->owner_thread == spdk_get_thread());
	assert(offset_in_chunk % cache->block_size == 0);
	assert(length % cache->block_size == 0);
	assert(offset_in_chunk + length <= cache->chunk_size);

	slot = slot_for_chunk(cache, chunk_index);
	if (!slot || !slot->resident) {
		cache->stats.misses++;
		return -ENOENT;
	}
	if (spdk_uuid_compare(&slot->uuid, uuid) != 0) {
		/* An older version of this chunk. Useless -- nothing reads a
		 * superseded object -- but left in place: it will be reused by the
		 * next populate for this same chunk_index. */
		cache->stats.misses++;
		return -ENOENT;
	}

	/* Past the object's end reads as zeroes, the same way a short GET does on
	 * the S3 path. */
	readable = offset_in_chunk < slot->valid_bytes
		   ? spdk_min(length, slot->valid_bytes - offset_in_chunk) : 0;

	if (readable == 0) {
		/* Entirely past the end. Answerable from valid_bytes alone, so it
		 * is a hit no matter which blocks are present -- and valid_bytes
		 * came from the chunk map, not from the device. Reported through
		 * the callback rather than a return code so the caller has one
		 * completion path. */
		memset(buf, 0, length);
		cache->stats.hits++;
		cb_fn(cb_arg, 0);
		return 0;
	}

	/* Every block the device is about to be asked for has to be present. A
	 * partly resident range is a miss: serving it would mean splitting the
	 * read between here and S3, and the S3 path already handles the whole
	 * thing in one request.
	 *
	 * The range is [offset_in_chunk, offset_in_chunk + readable), clamped to
	 * the object, so the last block may be a partial one at the object's end.
	 * That block is marked present by the fill that reached the end, and the
	 * bytes past valid_bytes inside it are zeroed below rather than served. */
	first_block = offset_in_chunk / cache->block_size;
	n_blocks = (offset_in_chunk + readable - 1) / cache->block_size + 1 -
		   first_block;

	if (!bitmap_test_range(slot->bitmap, first_block, n_blocks)) {
		cache->stats.hits_declined++;
		return -ENOENT;
	}

	rd = calloc(1, sizeof(*rd));
	if (!rd) {
		cache->stats.misses++;
		return -ENOENT;
	}

	/* Round up to the block size: the device cannot read a partial block, and
	 * the extra bytes land inside the caller's buffer within `length` (which
	 * is block aligned and no smaller than readable) and are then zeroed. */
	read_len = spdk_divide_round_up(readable, cache->block_size) *
		   cache->block_size;

	rd->cache     = cache;
	rd->slot      = slot;
	rd->cb_fn     = cb_fn;
	rd->cb_arg    = cb_arg;
	rd->buf       = buf;
	rd->zero_from = readable;
	rd->zero_len  = length - readable;

	slot->pins++;
	/* Off the LRU while pinned, so eviction never has to look at pins. */
	slot_lru_remove(cache, slot);
	cache->reads_in_flight++;

	rc = spdk_bdev_read(cache->desc, cache->ch, buf,
			    slot_offset(cache, slot) + offset_in_chunk,
			    read_len, cache_read_done, rd);
	if (rc != 0) {
		slot->pins--;
		slot_lru_touch(cache, slot);
		cache->reads_in_flight--;
		free(rd);
		/* Almost always -ENOMEM from the bdev_io pool. Reported as a miss
		 * so the caller goes to S3 instead of failing the user's read. */
		cache->stats.misses++;
		return -ENOENT;
	}

	cache->stats.hits++;
	cache->stats.bytes_served += readable;

	return 0;
}

/* ==========================================================================
 * Populate
 * ========================================================================== */

static void
cache_fill_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct cache_fill *fill = cb_arg;
	struct s3_cache *cache = fill->cache;
	struct cache_slot *slot = fill->slot;

	spdk_bdev_free_io(bdev_io);

	assert(slot->fills > 0);
	slot->fills--;

	if (success) {
		/* Marking the range present only now is what makes concurrent
		 * fills safe without any exclusion between them: until this
		 * point a read of this range misses and goes to S3. */
		uint32_t newly = bitmap_set_range(slot->bitmap, fill->first_block,
						  fill->n_blocks);

		slot->filled_blocks += newly;
		cache->resident_blocks += newly;

		cache->stats.populates++;
		cache->stats.bytes_populated += fill->bytes;
		slot_lru_touch(cache, slot);
	} else {
		cache->stats.populates_failed++;

		/* Nothing to undo: the range was never marked present, so the
		 * half-written blocks are already unreadable. Only a slot that
		 * ended up holding nothing at all is worth reclaiming -- keeping
		 * it would occupy a slot that can never produce a hit. */
		if (slot->fills == 0 && slot->pins == 0 &&
		    slot->filled_blocks == 0) {
			slot_release(cache, slot);
		} else {
			slot_lru_touch(cache, slot);
		}
	}

	TAILQ_INSERT_HEAD(&cache->staging_free, fill->staging, link);
	cache->fills_in_flight--;

	free(fill);
}

void
s3_cache_populate(struct s3_cache *cache, uint64_t chunk_index,
		  const struct spdk_uuid *uuid, uint32_t offset_in_chunk,
		  const void *buf, uint32_t length, uint32_t object_valid_bytes)
{
	struct cache_slot *slot;
	struct cache_staging *staging;
	struct cache_fill *fill;
	uint32_t first_block, last_block, n_blocks;
	uint32_t end_byte, write_len;
	int rc;

	if (!cache || !uuid || !buf || length == 0 || object_valid_bytes == 0) {
		return;
	}
	assert(cache->owner_thread == spdk_get_thread());
	assert(offset_in_chunk % cache->block_size == 0);

	if (chunk_index >= cache->num_chunks ||
	    object_valid_bytes > cache->chunk_size ||
	    offset_in_chunk >= object_valid_bytes) {
		return;
	}

	/* A caller may hand over more than the object holds -- a read's buffer is
	 * padded with zeroes past the end (s3_chunk_read_done) -- and those bytes
	 * are not part of the object. */
	end_byte = offset_in_chunk + spdk_min(length,
					      object_valid_bytes - offset_in_chunk);

	/* Which whole blocks this makes present. A trailing partial block only
	 * counts when it is the object's own last one: then there are no further
	 * bytes to wait for and reads clamp to valid_bytes anyway. Anywhere else a
	 * partial block would leave a hole that no bit can describe, so it is
	 * dropped and refetched. */
	first_block = offset_in_chunk / cache->block_size;
	if (end_byte >= object_valid_bytes) {
		last_block = (object_valid_bytes - 1) / cache->block_size;
	} else {
		if (end_byte < cache->block_size + offset_in_chunk) {
			return;   /* not even one whole block */
		}
		last_block = end_byte / cache->block_size - 1;
	}
	if (last_block < first_block) {
		return;
	}
	n_blocks = last_block - first_block + 1;

	slot = slot_for_chunk(cache, chunk_index);
	if (slot) {
		if (slot->resident && spdk_uuid_compare(&slot->uuid, uuid) == 0) {
			/* Same object. Adding a range to it is the normal case
			 * now, so this is where a sequential read stream lands
			 * repeatedly -- but skip what is already here rather than
			 * rewrite it. In-flight ranges are not tracked, so an
			 * overlapping concurrent fill can still write the same
			 * bytes twice; harmless, since the same uuid at the same
			 * offset is by definition the same data. */
			if (bitmap_test_range(slot->bitmap, first_block,
					      n_blocks)) {
				return;
			}
		} else if (slot->fills > 0 || slot->pins > 0) {
			/* A different version, with I/O in flight against the
			 * slot. Taking it over would write under a reader or let
			 * a landing fill mark ranges of the wrong object. */
			cache->stats.populates_dropped++;
			return;
		} else {
			/* Reuse it for the new version: one slot per chunk_index
			 * keeps a rewritten chunk from occupying two. */
			slot_release(cache, slot);
			slot = TAILQ_FIRST(&cache->free_slots);
			assert(slot != NULL);
			TAILQ_REMOVE(&cache->free_slots, slot, link);
		}
	} else {
		slot = slot_acquire(cache);
		if (!slot) {
			cache->stats.populates_dropped++;
			return;
		}
	}

	staging = TAILQ_FIRST(&cache->staging_free);
	if (!staging) {
		/* At the concurrency limit. Dropping is the right answer: a queue
		 * here would grow without bound behind a slow device, and the data
		 * is in S3 either way. */
		if (!slot->resident) {
			TAILQ_INSERT_HEAD(&cache->free_slots, slot, link);
		}
		cache->stats.populates_dropped++;
		return;
	}

	fill = calloc(1, sizeof(*fill));
	if (!fill) {
		if (!slot->resident) {
			TAILQ_INSERT_HEAD(&cache->free_slots, slot, link);
		}
		cache->stats.populates_dropped++;
		return;
	}

	TAILQ_REMOVE(&cache->staging_free, staging, link);

	/* The device writes whole blocks, so the tail of a trailing partial block
	 * goes out too. It is never read back -- reads clamp to valid_bytes -- but
	 * zero it rather than ship whatever the previous tenant of the staging
	 * buffer left. */
	write_len = n_blocks * cache->block_size;
	memcpy(staging->buf, buf, end_byte - offset_in_chunk);
	if (write_len > end_byte - offset_in_chunk) {
		memset((uint8_t *)staging->buf + (end_byte - offset_in_chunk), 0,
		       write_len - (end_byte - offset_in_chunk));
	}

	/* Claim the slot before the write. valid_bytes is set now rather than on
	 * completion because it describes the object, not what has landed: it
	 * comes from the chunk map, and a read entirely past it is answerable
	 * without any block being present. */
	if (!slot->resident) {
		spdk_uuid_copy(&slot->uuid, uuid);
		slot->chunk_index = chunk_index;
		slot->resident    = true;
		cache->chunk_to_slot[chunk_index] = (uint32_t)(slot - cache->slots);
		cache->resident++;
	}
	slot->valid_bytes = object_valid_bytes;

	fill->cache       = cache;
	fill->slot        = slot;
	fill->staging     = staging;
	fill->first_block = first_block;
	fill->n_blocks    = n_blocks;
	fill->bytes       = end_byte - offset_in_chunk;

	slot->fills++;
	/* Off the LRU while a fill is in flight, for the same reason as a pin:
	 * eviction takes the head without looking. */
	slot_lru_remove(cache, slot);
	cache->fills_in_flight++;

	rc = spdk_bdev_write(cache->desc, cache->ch, staging->buf,
			     slot_offset(cache, slot) +
			     (uint64_t)first_block * cache->block_size,
			     write_len, cache_fill_done, fill);
	if (rc != 0) {
		cache->fills_in_flight--;
		slot->fills--;
		if (slot->fills == 0 && slot->pins == 0 &&
		    slot->filled_blocks == 0) {
			slot_release(cache, slot);
		} else {
			slot_lru_touch(cache, slot);
		}
		TAILQ_INSERT_HEAD(&cache->staging_free, staging, link);
		free(fill);
		cache->stats.populates_dropped++;
	}
}

void
s3_cache_drop_chunk(struct s3_cache *cache, uint64_t chunk_index)
{
	struct cache_slot *slot;

	if (!cache) {
		return;
	}
	assert(cache->owner_thread == spdk_get_thread());

	slot = slot_for_chunk(cache, chunk_index);
	if (!slot) {
		return;
	}
	if (slot->fills > 0 || slot->pins > 0) {
		/* I/O in flight owns the slot. Leaving it alone is safe: the uuid
		 * tag means nothing can read it as a newer version, and it will be
		 * reused or evicted later. */
		return;
	}

	slot_release(cache, slot);
}

bool
s3_cache_is_quiesced(const struct s3_cache *cache)
{
	if (!cache) {
		return true;
	}

	return cache->fills_in_flight == 0 && cache->reads_in_flight == 0;
}

void
s3_cache_get_stats(struct s3_cache *cache, struct s3_cache_stats *stats)
{
	if (!cache || !stats) {
		return;
	}

	cache->stats.slots_resident = cache->resident;
	cache->stats.bytes_resident = cache->resident_blocks *
				      cache->block_size;
	*stats = cache->stats;
}
