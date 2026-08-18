/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_cache -- local read cache for S3 chunk objects
 *
 *   === What it is for ===
 *
 *   A read that misses everything local costs an S3 GET, tens of milliseconds.
 *   The case that matters is the one right after a flush: the flusher assembles
 *   the whole chunk in order to PUT it, and on success s3_overlay drops the blocks
 *   it had been serving reads from (overlay_block_release in s3_overlay.c). The
 *   read latency for that data goes from ~0 to a full S3 round trip at the instant
 *   the upload succeeds, even though the bytes were local a moment before. A
 *   filesystem notices at once: inode and directory blocks are written and read
 *   straight back.
 *
 *   So the flush hands its buffer over on the way past, for the price of one local
 *   write and no extra request. Every read that had to go to S3 does the same with
 *   whatever it fetched. Those are the two populate sites.
 *
 *   Note there is nothing to gain from also populating when a read-modify-write
 *   flush reads its base object back: those bytes are merged into the new object
 *   and cached under the new uuid by the flush that read them. Caching them
 *   separately under the base uuid would cost a second chunk-sized write to store
 *   an object the same flush is about to delete, in a slot the new version takes
 *   over as soon as the PUT lands.
 *
 *   === Partial residency, and why a slot needs a bitmap ===
 *
 *   A slot holds one object, but not necessarily all of it. A read miss caches
 *   exactly the range it fetched, which is what a filesystem read looks like:
 *   ext4 reads ahead 128 KiB against a chunk of 1 MiB, so requiring a whole
 *   object before anything could be cached meant a mounted volume never
 *   populated from reads at all and stayed cold forever, no matter how many
 *   times the same blocks were read.
 *
 *   Which range is present therefore cannot be a scalar. It used to be:
 *   valid_bytes alone, read as "[0, valid_bytes) is here". Filling a slot from
 *   a read of [512 KiB, 640 KiB) under that model has no honest encoding --
 *   setting valid_bytes to 640 KiB claims the first 512 KiB as well, and a read
 *   of it would be served whatever the slot's previous tenant left on the
 *   device. Silent corruption, which is far worse than a miss.
 *
 *   So residency is a bitmap of block_size units, and valid_bytes keeps only the
 *   meaning it always had for the *object*: its length, past which reads are
 *   zeroes. A read is a hit when every block it covers is set (a request that is
 *   only partly resident is a miss, not a split I/O), or when it lies entirely
 *   past valid_bytes, which is answerable from the length alone.
 *
 *   The last block of an object may be partly filled -- valid_bytes need not be
 *   a multiple of block_size -- and is set once the range that reaches the
 *   object's end has landed. Reads clamp to valid_bytes and zero the rest, so
 *   the bytes past the end are never served from the device.
 *
 *   === Why entries can never go stale ===
 *
 *   Objects are immutable: every flush generates a fresh uuid and PUTs a new key
 *   (create-once, P2 -- s3_bs_dev.c s3_flush_put / s3_chunk_write_put). A uuid
 *   therefore names one immutable byte sequence for as long as it exists.
 *
 *   So a cache entry tagged with the uuid it came from *cannot be wrong*. There
 *   is no invalidation protocol, no coherence window, and no ordering
 *   requirement against the chunk map or the journal. When a chunk is rewritten,
 *   the reader resolves chunk_index to a new uuid, the tag no longer matches, and
 *   the entry is simply a miss. That is the single property this whole module
 *   rests on; anything that makes object contents mutable invalidates it.
 *
 *   === Why nothing here needs to survive a crash ===
 *
 *   Every byte is a copy of an object that is already durable in S3, so the
 *   cache is never the only holder of anything. The index is therefore RAM-only
 *   and a restart comes up cold. That is a warm-up cost, not a correctness
 *   property, and adding a persistent index later needs no change to what is
 *   written here: it would be a new region in the super block (which records
 *   regions explicitly for exactly this reason, see s3_local_dev.h), not a
 *   different layout of this one.
 *
 *   This is also what keeps the module out of the write path. It holds *clean
 *   data only*: nothing is ever dirty here, so there is no flush order to
 *   respect and no way for a bug in this file to lose an acknowledged write. The
 *   worst it can do is serve a miss.
 *
 *   === On-disk layout ===
 *
 *   The cache region is a flat array of chunk-sized slots:
 *
 *     slot i  ->  region.offset + i * chunk_size
 *
 *   Slot to content is decided at runtime and held in RAM only, so there is
 *   nothing on disk to keep consistent. A block of a slot that no populate has
 *   written holds whatever the slot's previous tenant left; the residency bitmap
 *   is what keeps it from ever being read.
 *
 *   === Threading ===
 *
 *   Owner thread only, like the rest of s3_ctx. Asserted, not assumed.
 */

#ifndef S3LVOL_CACHE_H
#define S3LVOL_CACHE_H

#include "spdk/stdinc.h"
#include "spdk/bdev.h"
#include "spdk/uuid.h"

/* Concurrent populates.
 *
 * Populate copies into a staging buffer of its own rather than borrowing the
 * caller's (see s3_cache_populate), so this bounds both the memory that costs
 * and how much of the local device's queue depth cache fills may take from the
 * WAL. Filling the cache is never more important than acknowledging a write. */
#define S3_CACHE_STAGING_BUFS   4

struct s3_cache;

typedef void (*s3_cache_read_cb)(void *cb_arg, int status);

struct s3_cache_opts {
	/* Local device region to use, and how to reach it. Channels are
	 * per-thread; this one must belong to the calling thread, which is also
	 * the thread every later call has to come from. */
	struct spdk_bdev_desc   *desc;
	struct spdk_io_channel  *ch;
	uint64_t                 region_offset;
	uint64_t                 region_size;

	uint32_t                 chunk_size;
	uint32_t                 block_size;

	/* Size of the chunk index space, i.e. the same num_chunks the chunk map
	 * was created with. Used for a dense chunk_index -> slot array. */
	uint64_t                 num_chunks;
};

struct s3_cache_stats {
	uint64_t hits;
	uint64_t misses;

	/* The slot held this exact object but not every block the read wanted.
	 * Counted apart from a plain miss because the two say different things:
	 * misses means the object is not here, this means it is here and being
	 * filled in a piece at a time. A high ratio against hits is the signal
	 * that reads are arriving in a smaller granularity than they repeat in. */
	uint64_t hits_declined;

	uint64_t populates;
	uint64_t populates_dropped;   /* no staging buffer or no slot free */
	uint64_t populates_failed;    /* the local write failed */
	uint64_t evictions;

	uint64_t bytes_served;        /* read from the local device */
	uint64_t bytes_populated;

	uint64_t slots_total;
	uint64_t slots_resident;      /* slots holding an object, whole or partly */
	uint64_t bytes_resident;      /* how much of those objects is actually here */
};

/**
 * Create a cache over a region of the local device.
 *
 * Fails with -EINVAL if the region cannot hold at least one chunk. A caller that
 * does not want a cache passes no opts at all rather than a tiny region.
 */
int s3_cache_create(const struct s3_cache_opts *opts, struct s3_cache **out);

void s3_cache_destroy(struct s3_cache *cache);

/**
 * Whether this exact object version is cached and readable right now.
 *
 * "Readable" means the whole object: with partial residency a slot can hold this
 * uuid and still miss a given read, so this answers a coarser question than
 * s3_cache_read() and is only meant for tests and diagnostics. The read path
 * must call s3_cache_read() and act on its return code -- there is nothing to
 * gain from asking first, and a true here does not promise a hit.
 */
bool s3_cache_lookup(struct s3_cache *cache, uint64_t chunk_index,
		     const struct spdk_uuid *uuid);

/**
 * Serve a read from the local device.
 *
 * \param offset_in_chunk must be block aligned, as must \p length: this reads
 *        the local bdev directly and the caller's offsets come from blobstore io
 *        units, which are the block size.
 *
 * The tail past the object's length is zero filled, matching what the S3 path
 * does when a GET returns short (s3_chunk_read_done).
 *
 * \return 0 with \p cb_fn to follow, or -ENOENT when this range of this version
 *         is not cached, in which case the callback does *not* run and the caller
 *         must fall back to S3. Never fails for any other reason: a cache that
 *         cannot answer is a miss, not an error.
 */
int s3_cache_read(struct s3_cache *cache, uint64_t chunk_index,
		  const struct spdk_uuid *uuid, uint32_t offset_in_chunk,
		  uint32_t length, void *buf,
		  s3_cache_read_cb cb_fn, void *cb_arg);

/**
 * Offer part or all of a chunk's contents for caching.
 *
 * Best effort and fire and forget: there is no callback and no error to handle,
 * because every outcome other than "cached" is just a future miss. Callers
 * should not check anything.
 *
 * \param offset_in_chunk where \p buf sits inside the object. Must be block
 *        aligned; a read's offset always is, and the flusher's is zero.
 * \param buf     \p length bytes of the object starting at \p offset_in_chunk.
 *                **Copied**, so the caller may free it as soon as this returns.
 *                Copying rather than borrowing is deliberate: the local write
 *                outlives this call, and the two natural callers (the flush
 *                buffer and the read's user buffer) are both freed or handed
 *                back by their own completion paths. Making them keep a buffer
 *                alive for an unrelated best-effort write is how the cache would
 *                end up owning a use-after-free in someone else's code.
 * \param length  how much of the object \p buf holds. Need not be block aligned
 *                only when it reaches the object's end, which is the one case a
 *                short GET produces.
 * \param object_valid_bytes length of the whole object, which may be less than
 *        chunk_size and is not the same thing as \p length. Reads past it are
 *        answered with zeroes, so it has to describe the object rather than this
 *        fragment of it.
 */
void s3_cache_populate(struct s3_cache *cache, uint64_t chunk_index,
		       const struct spdk_uuid *uuid, uint32_t offset_in_chunk,
		       const void *buf, uint32_t length,
		       uint32_t object_valid_bytes);

/**
 * Drop whatever is cached for a chunk, freeing the slot.
 *
 * Not needed for correctness -- an unmapped chunk has no uuid for a reader to
 * ask for, so its entry can never be returned -- but without it the slot stays
 * occupied until it reaches the end of the LRU.
 */
void s3_cache_drop_chunk(struct s3_cache *cache, uint64_t chunk_index);

/**
 * Whether the cache has no I/O of its own outstanding.
 *
 * Needed for teardown. A fill is a write to the local device that holds a
 * pointer into the cache's internal state while it runs, and it is fire and
 * forget, so there is no completion for a caller to wait on -- this is how it
 * finds out. Reads are covered too, though a caller draining its own I/O has
 * already accounted for those.
 */
bool s3_cache_is_quiesced(const struct s3_cache *cache);

void s3_cache_get_stats(struct s3_cache *cache, struct s3_cache_stats *stats);

#endif /* S3LVOL_CACHE_H */
