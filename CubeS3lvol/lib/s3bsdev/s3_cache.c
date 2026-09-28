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
#define S3_CACHE_OBJECT_HASH_MAX (1U << 20)

struct cache_hot;
struct cache_alias;
TAILQ_HEAD(cache_alias_list, cache_alias);

enum cache_slot_kind {
	CACHE_SLOT_FREE = 0,
	CACHE_SLOT_NATIVE,
	CACHE_SLOT_OBJECT,
};

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

	enum cache_slot_kind kind;
	bool             resident;
	bool             on_lru;
	struct cache_hot *hot;

	/* OBJECT slots are addressed by this full immutable identity. Hashing is
	 * only an index; all strings are compared before a hit is accepted. */
	uint64_t          object_hash;
	char             *object_endpoint;
	char             *object_bucket;
	char             *object_key;
	struct cache_slot *object_hash_next;
	struct cache_alias_list aliases;

	TAILQ_ENTRY(cache_slot) link;
};

/* A CopyObject destination identity that names the immutable bytes in an
 * OBJECT slot. The hash is by destination chunk: only its newest uuid can be
 * useful, and replacing it bounds stale metadata after rewrites/reimports. */
struct cache_alias {
	uint64_t                 dest_chunk_index;
	struct spdk_uuid         dest_uuid;
	struct cache_slot       *slot;
	struct cache_alias      *hash_next;
	TAILQ_ENTRY(cache_alias) lru_link;
	TAILQ_ENTRY(cache_alias) slot_link;
};

struct cache_hot {
	void             *buf;
	struct cache_slot *slot;
	struct spdk_uuid  uuid;
	uint64_t          chunk_index;
	uint32_t          valid_bytes;
	uint32_t          pins;
	bool              resident;
	bool              on_lru;
	TAILQ_ENTRY(cache_hot) link;
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
	uint32_t                 write_len;
	uint64_t                 device_offset;
	bool                     object_fill;
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
	uint32_t                 bytes;
	bool                     object_read;
	bool                     alias_read;
};

struct s3_cache {
	struct spdk_bdev_desc   *desc;
	struct spdk_io_channel  *ch;
	struct spdk_thread      *owner_thread;
	pthread_mutex_t          lock;

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
	/* Imported objects are always lower priority than native entries. */
	TAILQ_HEAD(, cache_slot) object_lru;
	TAILQ_HEAD(, cache_slot) free_slots;

	struct cache_slot      **object_hash;
	uint32_t                 object_hash_buckets;
	uint64_t                 object_resident;
	uint32_t                 object_fills_in_flight;
	bool                     object_io_stopped;
	struct cache_alias     **alias_hash;
	uint32_t                 alias_hash_buckets;
	uint64_t                 alias_count;
	uint64_t                 alias_limit;
	struct cache_alias_list  alias_lru;

	TAILQ_HEAD(, cache_hot) hot_lru;
	TAILQ_HEAD(, cache_hot) hot_free;
	struct cache_hot       *hot;
	void                   *hot_map;
	size_t                  hot_map_len;
	uint32_t               *chunk_to_hot;
	uint32_t                hot_count;
	uint32_t                hot_resident;

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

static uint64_t
object_id_hash(const struct s3_cache_object_id *id)
{
	const char *parts[] = {id->endpoint, id->bucket, id->key};
	uint64_t h = UINT64_C(1469598103934665603);

	for (size_t p = 0; p < SPDK_COUNTOF(parts); p++) {
		for (const unsigned char *s = (const unsigned char *)parts[p]; *s; s++) {
			h ^= *s;
			h *= UINT64_C(1099511628211);
		}
		/* Keep ("ab", "c") distinct from ("a", "bc"). */
		h ^= 0xff;
		h *= UINT64_C(1099511628211);
	}
	return h;
}

static bool
object_id_valid(const struct s3_cache_object_id *id)
{
	return id && id->endpoint && id->endpoint[0] != '\0' &&
	       id->bucket && id->bucket[0] != '\0' &&
	       id->key && id->key[0] != '\0';
}

static bool
slot_object_matches(const struct cache_slot *slot,
		    const struct s3_cache_object_id *id, uint64_t hash)
{
	return slot->kind == CACHE_SLOT_OBJECT && slot->object_hash == hash &&
	       strcmp(slot->object_endpoint, id->endpoint) == 0 &&
	       strcmp(slot->object_bucket, id->bucket) == 0 &&
	       strcmp(slot->object_key, id->key) == 0;
}

static struct cache_slot *
object_find_locked(struct s3_cache *cache, const struct s3_cache_object_id *id,
		   uint64_t hash)
{
	struct cache_slot *slot;
	uint32_t bucket;

	if (!cache->object_hash_buckets) {
		return NULL;
	}
	bucket = (uint32_t)hash & (cache->object_hash_buckets - 1);
	for (slot = cache->object_hash[bucket]; slot; slot = slot->object_hash_next) {
		if (slot_object_matches(slot, id, hash)) {
			return slot;
		}
	}
	return NULL;
}

static void
object_index_insert_locked(struct s3_cache *cache, struct cache_slot *slot)
{
	uint32_t bucket = (uint32_t)slot->object_hash &
			  (cache->object_hash_buckets - 1);

	slot->object_hash_next = cache->object_hash[bucket];
	cache->object_hash[bucket] = slot;
}

static void
object_index_remove_locked(struct s3_cache *cache, struct cache_slot *slot)
{
	struct cache_slot **link;
	uint32_t bucket;

	if (slot->kind != CACHE_SLOT_OBJECT || !cache->object_hash_buckets) {
		return;
	}
	bucket = (uint32_t)slot->object_hash & (cache->object_hash_buckets - 1);
	for (link = &cache->object_hash[bucket]; *link; link = &(*link)->object_hash_next) {
		if (*link == slot) {
			*link = slot->object_hash_next;
			break;
		}
	}
	slot->object_hash_next = NULL;
}

static uint64_t
alias_hash(uint64_t chunk_index)
{
	/* SplitMix64 finalizer: chunk indices are often sequential. */
	uint64_t x = chunk_index + UINT64_C(0x9e3779b97f4a7c15);

	x = (x ^ (x >> 30)) * UINT64_C(0xbf58476d1ce4e5b9);
	x = (x ^ (x >> 27)) * UINT64_C(0x94d049bb133111eb);
	return x ^ (x >> 31);
}

static struct cache_alias *
alias_find_chunk_locked(struct s3_cache *cache, uint64_t chunk_index)
{
	struct cache_alias *alias;
	uint32_t bucket;

	if (!cache->alias_hash_buckets) {
		return NULL;
	}
	bucket = (uint32_t)alias_hash(chunk_index) &
		 (cache->alias_hash_buckets - 1);
	for (alias = cache->alias_hash[bucket]; alias;
	     alias = alias->hash_next) {
		if (alias->dest_chunk_index == chunk_index) {
			return alias;
		}
	}
	return NULL;
}

static struct cache_alias *
alias_find_locked(struct s3_cache *cache, uint64_t chunk_index,
		  const struct spdk_uuid *uuid)
{
	struct cache_alias *alias = alias_find_chunk_locked(cache, chunk_index);

	if (!alias || spdk_uuid_compare(&alias->dest_uuid, uuid) != 0 ||
	    !alias->slot || alias->slot->kind != CACHE_SLOT_OBJECT ||
	    !alias->slot->resident) {
		return NULL;
	}
	return alias;
}

static void
alias_remove_locked(struct s3_cache *cache, struct cache_alias *alias,
		    bool evicted)
{
	struct cache_alias **link;
	uint32_t bucket;

	bucket = (uint32_t)alias_hash(alias->dest_chunk_index) &
		 (cache->alias_hash_buckets - 1);
	for (link = &cache->alias_hash[bucket]; *link;
	     link = &(*link)->hash_next) {
		if (*link == alias) {
			*link = alias->hash_next;
			break;
		}
	}
	TAILQ_REMOVE(&cache->alias_lru, alias, lru_link);
	TAILQ_REMOVE(&alias->slot->aliases, alias, slot_link);
	assert(cache->alias_count > 0);
	cache->alias_count--;
	if (evicted) {
		cache->stats.object_alias_evictions++;
	}
	free(alias);
}

static void
alias_insert_locked(struct s3_cache *cache, struct cache_alias *alias)
{
	uint32_t bucket = (uint32_t)alias_hash(alias->dest_chunk_index) &
			  (cache->alias_hash_buckets - 1);

	alias->hash_next = cache->alias_hash[bucket];
	cache->alias_hash[bucket] = alias;
	TAILQ_INSERT_TAIL(&cache->alias_lru, alias, lru_link);
	TAILQ_INSERT_TAIL(&alias->slot->aliases, alias, slot_link);
	cache->alias_count++;
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
		if (slot->kind == CACHE_SLOT_OBJECT) {
			TAILQ_REMOVE(&cache->object_lru, slot, link);
		} else {
			TAILQ_REMOVE(&cache->lru, slot, link);
		}
		slot->on_lru = false;
	}
}

/* Warmest end. Called when a slot becomes readable or stops being read. */
static void
slot_lru_touch(struct s3_cache *cache, struct cache_slot *slot)
{
	slot_lru_remove(cache, slot);
	if (slot->resident && slot->fills == 0 && slot->pins == 0) {
		if (slot->kind == CACHE_SLOT_OBJECT) {
			TAILQ_INSERT_TAIL(&cache->object_lru, slot, link);
		} else {
			TAILQ_INSERT_TAIL(&cache->lru, slot, link);
		}
		slot->on_lru = true;
	}
}

static void
hot_lru_remove(struct s3_cache *cache, struct cache_hot *hot)
{
	if (hot->on_lru) {
		TAILQ_REMOVE(&cache->hot_lru, hot, link);
		hot->on_lru = false;
	}
}

static void
hot_lru_touch(struct s3_cache *cache, struct cache_hot *hot)
{
	hot_lru_remove(cache, hot);
	if (hot->resident && hot->pins == 0) {
		TAILQ_INSERT_TAIL(&cache->hot_lru, hot, link);
		hot->on_lru = true;
	}
}

static struct cache_hot *
hot_for_chunk(struct s3_cache *cache, uint64_t chunk_index)
{
	uint32_t idx;

	if (!cache->chunk_to_hot || chunk_index >= cache->num_chunks) {
		return NULL;
	}
	idx = cache->chunk_to_hot[chunk_index];
	if (idx == S3_CACHE_NO_SLOT) {
		return NULL;
	}
	assert(idx < cache->hot_count);
	return &cache->hot[idx];
}

/* Detach without waiting for readers. A reader pins the hot entry itself, so
 * its mmap range cannot be reused until the final pin is dropped. */
static void
hot_detach(struct s3_cache *cache, struct cache_hot *hot)
{
	struct cache_slot *slot = hot->slot;

	hot_lru_remove(cache, hot);
	if (!hot->resident) {
		return;
	}
	assert(cache->chunk_to_hot[hot->chunk_index] ==
	       (uint32_t)(hot - cache->hot));
	cache->chunk_to_hot[hot->chunk_index] = S3_CACHE_NO_SLOT;
	if (slot) {
		assert(slot->hot == hot);
		slot->hot = NULL;
	}
	hot->slot = NULL;
	hot->resident = false;
	spdk_uuid_set_null(&hot->uuid);
	hot->valid_bytes = 0;
	assert(cache->hot_resident > 0);
	cache->hot_resident--;
	if (hot->pins == 0) {
		TAILQ_INSERT_HEAD(&cache->hot_free, hot, link);
	}
}

/* Reserve an unattached entry. The caller pins it to the disk slot before
 * dropping cache->lock so a concurrent populate sees the reservation; the
 * copy still runs unlocked, and chunk_to_hot is only written on publish. */
static struct cache_hot *
hot_acquire(struct s3_cache *cache)
{
	struct cache_hot *hot = TAILQ_FIRST(&cache->hot_free);

	if (hot) {
		TAILQ_REMOVE(&cache->hot_free, hot, link);
		return hot;
	}

	hot = TAILQ_FIRST(&cache->hot_lru);
	if (!hot) {
		return NULL;
	}
	hot_detach(cache, hot);
	TAILQ_REMOVE(&cache->hot_free, hot, link);
	cache->stats.hot_evictions++;
	return hot;
}

/* Detach a slot from its chunk and return it to the free list. */
static void
slot_release(struct s3_cache *cache, struct cache_slot *slot)
{
	assert(slot->pins == 0);
	assert(slot->fills == 0);

	slot_lru_remove(cache, slot);
	if (slot->hot) {
		/* Disk and RAM capacity have independent LRUs. Dropping a disk
		 * slot only removes the back-reference; the hot object stays
		 * addressable through chunk_to_hot. */
		assert(slot->hot->slot == slot);
		slot->hot->slot = NULL;
		slot->hot = NULL;
	}

	if (slot->resident) {
		if (slot->kind == CACHE_SLOT_OBJECT) {
			struct cache_alias *alias;

			while ((alias = TAILQ_FIRST(&slot->aliases)) != NULL) {
				alias_remove_locked(cache, alias, true);
			}
			object_index_remove_locked(cache, slot);
			free(slot->object_endpoint);
			free(slot->object_bucket);
			free(slot->object_key);
			slot->object_endpoint = NULL;
			slot->object_bucket = NULL;
			slot->object_key = NULL;
			slot->object_hash = 0;
			assert(cache->object_resident > 0);
			cache->object_resident--;
		} else {
			assert(slot->kind == CACHE_SLOT_NATIVE);
			assert(cache->chunk_to_slot[slot->chunk_index] ==
			       (uint32_t)(slot - cache->slots));
			cache->chunk_to_slot[slot->chunk_index] = S3_CACHE_NO_SLOT;
		}
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
	slot->kind = CACHE_SLOT_FREE;
	TAILQ_INSERT_HEAD(&cache->free_slots, slot, link);
}

/* A slot to put a new object in, or NULL when everything is either pinned or
 * being filled. Returning NULL is normal under load and simply means the
 * populate is dropped. */
static struct cache_slot *
slot_acquire_native(struct s3_cache *cache)
{
	struct cache_slot *slot = TAILQ_FIRST(&cache->free_slots);
	bool object_victim = false;

	if (slot) {
		TAILQ_REMOVE(&cache->free_slots, slot, link);
		return slot;
	}

	/* Imported objects are opportunistic occupants. Native data gets the
	 * whole cache capacity and reclaims object slots before native ones. */
	slot = TAILQ_FIRST(&cache->object_lru);
	if (slot) {
		object_victim = true;
		cache->stats.object_evictions++;
	} else {
		slot = TAILQ_FIRST(&cache->lru);
	}
	if (!slot) {
		return NULL;
	}

	slot_release(cache, slot);
	if (!object_victim) {
		cache->stats.evictions++;
	}

	slot = TAILQ_FIRST(&cache->free_slots);
	assert(slot != NULL);
	TAILQ_REMOVE(&cache->free_slots, slot, link);

	return slot;
}

static struct cache_slot *
slot_acquire_object(struct s3_cache *cache)
{
	struct cache_slot *slot = TAILQ_FIRST(&cache->free_slots);

	if (slot) {
		TAILQ_REMOVE(&cache->free_slots, slot, link);
		return slot;
	}

	/* Never displace native data for an export-parent optimisation. */
	slot = TAILQ_FIRST(&cache->object_lru);
	if (!slot) {
		return NULL;
	}
	slot_release(cache, slot);
	cache->stats.object_evictions++;
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
	if (pthread_mutex_init(&cache->lock, NULL) != 0) {
		free(cache);
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
	TAILQ_INIT(&cache->object_lru);
	TAILQ_INIT(&cache->free_slots);
	TAILQ_INIT(&cache->alias_lru);
	TAILQ_INIT(&cache->hot_lru);
	TAILQ_INIT(&cache->hot_free);
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
		TAILQ_INIT(&cache->slots[s].aliases);
		TAILQ_INSERT_TAIL(&cache->free_slots, &cache->slots[s], link);
	}
	for (uint64_t c = 0; c < opts->num_chunks; c++) {
		cache->chunk_to_slot[c] = S3_CACHE_NO_SLOT;
	}

	/* Optional secondary index. Failure only disables cross-import sharing;
	 * the native cache must still attach and preserve its old behaviour. */
	cache->object_hash_buckets = 64;
	while (cache->object_hash_buckets < n_slots * 2 &&
	       cache->object_hash_buckets < S3_CACHE_OBJECT_HASH_MAX) {
		cache->object_hash_buckets <<= 1;
	}
	cache->object_hash = calloc(cache->object_hash_buckets,
				    sizeof(*cache->object_hash));
	if (!cache->object_hash) {
		SPDK_WARNLOG("Could not allocate imported-object cache index; "
			    "native chunk cache remains enabled\n");
		cache->object_hash_buckets = 0;
	} else {
		cache->alias_hash_buckets = cache->object_hash_buckets;
		cache->alias_hash = calloc(cache->alias_hash_buckets,
					   sizeof(*cache->alias_hash));
		if (!cache->alias_hash) {
			SPDK_WARNLOG("Could not allocate CopyObject cache alias "
				    "index; imported-object cache remains enabled\n");
			cache->alias_hash_buckets = 0;
		} else {
			cache->alias_limit = n_slots;
		}
	}

	if (opts->hot_bufs > S3_CACHE_HOT_BUFS_MAX ||
	    (opts->hot_bufs != 0 &&
	     opts->chunk_size > SIZE_MAX / opts->hot_bufs)) {
		s3_cache_destroy(cache);
		return -EINVAL;
	}
	if (opts->hot_bufs != 0) {
		size_t map_len = (size_t)opts->hot_bufs * opts->chunk_size;
		void *map = mmap(NULL, map_len, PROT_READ | PROT_WRITE,
				 MAP_PRIVATE | MAP_ANONYMOUS | MAP_POPULATE, -1, 0);
		struct cache_hot *hot = calloc(opts->hot_bufs, sizeof(*hot));
		uint32_t *hot_index = malloc(opts->num_chunks *
					     sizeof(*hot_index));

		if (map == MAP_FAILED || !hot || !hot_index) {
			SPDK_WARNLOG("Could not allocate %u x %u-byte RAM cache "
				    "slots; continuing with disk cache only\n",
				    opts->hot_bufs, opts->chunk_size);
			if (map != MAP_FAILED) {
				munmap(map, map_len);
			}
			free(hot);
			free(hot_index);
		} else {
			cache->hot = hot;
			cache->hot_map = map;
			cache->hot_map_len = map_len;
			cache->chunk_to_hot = hot_index;
			cache->hot_count = opts->hot_bufs;
			for (uint64_t c = 0; c < opts->num_chunks; c++) {
				hot_index[c] = S3_CACHE_NO_SLOT;
			}
			for (i = 0; i < opts->hot_bufs; i++) {
				hot[i].buf = (uint8_t *)map +
					     (size_t)i * opts->chunk_size;
				TAILQ_INSERT_TAIL(&cache->hot_free, &hot[i], link);
			}
		}
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
	cache->stats.hot_slots_total = cache->hot_count;

	SPDK_NOTICELOG("Chunk cache: %" PRIu64 " disk slots and %u RAM slots "
		       "of %u KiB at offset %"
		       PRIu64 " (%" PRIu64 " MiB), %" PRIu64 " chunk index "
		       "entries, %u blocks per slot\n",
		       n_slots, cache->hot_count, opts->chunk_size / 1024,
		       opts->region_offset,
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
	for (uint32_t i = 0; i < cache->hot_count; i++) {
		assert(cache->hot[i].pins == 0);
	}

	if (cache->staging) {
		for (uint32_t i = 0; i < S3_CACHE_STAGING_BUFS; i++) {
			spdk_dma_free(cache->staging[i].buf);
		}
		free(cache->staging);
	}
	if (cache->slots) {
		for (uint64_t s = 0; s < cache->n_slots; s++) {
			free(cache->slots[s].object_endpoint);
			free(cache->slots[s].object_bucket);
			free(cache->slots[s].object_key);
		}
	}
	while (!TAILQ_EMPTY(&cache->alias_lru)) {
		alias_remove_locked(cache, TAILQ_FIRST(&cache->alias_lru), false);
	}
	free(cache->alias_hash);
	free(cache->object_hash);
	free(cache->chunk_to_slot);
	free(cache->bitmaps);
	free(cache->slots);
	if (cache->hot_map) {
		munmap(cache->hot_map, cache->hot_map_len);
	}
	free(cache->chunk_to_hot);
	free(cache->hot);
	pthread_mutex_destroy(&cache->lock);
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
	struct cache_hot *hot;
	struct cache_alias *alias;

	if (!cache || !uuid) {
		return false;
	}

	pthread_mutex_lock(&cache->lock);
	hot = hot_for_chunk(cache, chunk_index);
	if (hot && spdk_uuid_compare(&hot->uuid, uuid) == 0) {
		pthread_mutex_unlock(&cache->lock);
		return true;
	}
	slot = slot_for_chunk(cache, chunk_index);
	if (!slot || !slot->resident ||
	    spdk_uuid_compare(&slot->uuid, uuid) != 0) {
		alias = cache->object_io_stopped ? NULL :
			alias_find_locked(cache, chunk_index, uuid);
		if (!alias) {
			pthread_mutex_unlock(&cache->lock);
			return false;
		}
		slot = alias->slot;
	}

	/* Whole object present. Anything less is a legitimate cache state but not
	 * something this coarse question can report, so say no rather than let a
	 * caller read "cached" as "will hit". */
	bool hit = slot->filled_blocks ==
		   spdk_divide_round_up(slot->valid_bytes, cache->block_size);
	if (!hit && !cache->object_io_stopped) {
		/* A partial native entry does not hide a complete CopyObject
		 * source alias; read-ahead admission should avoid the same
		 * unnecessary destination GET as the demand path. */
		alias = alias_find_locked(cache, chunk_index, uuid);
		if (alias) {
			slot = alias->slot;
			hit = slot->filled_blocks ==
			      spdk_divide_round_up(slot->valid_bytes,
						   cache->block_size);
		}
	}
	pthread_mutex_unlock(&cache->lock);
	return hit;
}

static void
cache_read_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct cache_read *rd = cb_arg;
	struct s3_cache *cache = rd->cache;
	struct cache_slot *slot = rd->slot;
	int status = success ? 0 : -EIO;

	spdk_bdev_free_io(bdev_io);

	pthread_mutex_lock(&cache->lock);
	if (success && rd->zero_len) {
		memset((uint8_t *)rd->buf + rd->zero_from, 0, rd->zero_len);
	}

	assert(slot->pins > 0);
	slot->pins--;
	slot_lru_touch(cache, slot);

	cache->reads_in_flight--;

	if (rd->object_read) {
		if (success) {
			cache->stats.object_hits++;
			cache->stats.object_bytes_served += rd->bytes;
			if (rd->alias_read) {
				cache->stats.object_alias_hits++;
			}
		} else {
			cache->stats.object_misses++;
			if (rd->alias_read) {
				cache->stats.object_alias_misses++;
			}
		}
	}

	if (!success) {
		/* The data is still in S3, so this is recoverable -- but not here:
		 * the caller has already been told the read is under way and only
		 * it knows how to reissue. Drop the entry so the retry misses
		 * rather than hitting the same bad slot again. */
		if (slot->pins == 0 && slot->fills == 0) {
			slot_release(cache, slot);
		}
	}
	pthread_mutex_unlock(&cache->lock);

	rd->cb_fn(rd->cb_arg, status);
	free(rd);
}

int
s3_cache_read_on_channel(struct s3_cache *cache, struct spdk_io_channel *channel,
			 uint64_t chunk_index, const struct spdk_uuid *uuid,
			 uint32_t offset_in_chunk, uint32_t length, void *buf,
			 s3_cache_read_cb cb_fn, void *cb_arg)
{
	struct cache_slot *slot;
	struct cache_hot *hot;
	struct cache_alias *alias = NULL;
	struct cache_alias *alias_candidate;
	struct cache_read *rd;
	uint32_t readable, read_len;
	uint32_t first_block, n_blocks;
	uint64_t device_offset;
	bool used_alias;
	int rc;

	if (!cache || !uuid || !buf || !cb_fn || length == 0) {
		return -ENOENT;
	}
	assert(offset_in_chunk % cache->block_size == 0);
	assert(length % cache->block_size == 0);
	assert(offset_in_chunk + length <= cache->chunk_size);

	pthread_mutex_lock(&cache->lock);
	hot = hot_for_chunk(cache, chunk_index);
	if (hot && spdk_uuid_compare(&hot->uuid, uuid) == 0) {
		readable = offset_in_chunk < hot->valid_bytes
			   ? spdk_min(length, hot->valid_bytes - offset_in_chunk) : 0;
		if (readable == 0) {
			memset(buf, 0, length);
			cache->stats.hits++;
			pthread_mutex_unlock(&cache->lock);
			cb_fn(cb_arg, 0);
			return 0;
		}

		hot->pins++;
		hot_lru_remove(cache, hot);
		cache->reads_in_flight++;
		cache->stats.hits++;
		cache->stats.ram_hits++;
		cache->stats.bytes_served += readable;
		cache->stats.ram_bytes_served += readable;
		pthread_mutex_unlock(&cache->lock);

		memcpy(buf, (uint8_t *)hot->buf + offset_in_chunk, readable);
		if (readable < length) {
			memset((uint8_t *)buf + readable, 0, length - readable);
		}

		pthread_mutex_lock(&cache->lock);
		assert(hot->pins > 0);
		hot->pins--;
		assert(cache->reads_in_flight > 0);
		cache->reads_in_flight--;
		if (hot->resident) {
			hot_lru_touch(cache, hot);
		} else if (hot->pins == 0) {
			TAILQ_INSERT_HEAD(&cache->hot_free, hot, link);
		}
		pthread_mutex_unlock(&cache->lock);
		cb_fn(cb_arg, 0);
		return 0;
	}

	slot = slot_for_chunk(cache, chunk_index);
	if (!slot || !slot->resident ||
	    spdk_uuid_compare(&slot->uuid, uuid) != 0) {
		/* An older version of this chunk. Useless -- nothing reads a
		 * superseded object -- but left in place. A CopyObject alias may
		 * still name these exact bytes through its stable source key. */
		alias = cache->object_io_stopped ? NULL :
			alias_find_locked(cache, chunk_index, uuid);
		if (!alias) {
			cache->stats.misses++;
			alias_candidate = cache->object_io_stopped ? NULL :
				alias_find_chunk_locked(cache, chunk_index);
			if (alias_candidate) {
				cache->stats.object_alias_misses++;
			}
			pthread_mutex_unlock(&cache->lock);
			return -ENOENT;
		}
		slot = alias->slot;
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
		if (alias) {
			cache->stats.object_hits++;
			cache->stats.object_alias_hits++;
			TAILQ_REMOVE(&cache->alias_lru, alias, lru_link);
			TAILQ_INSERT_TAIL(&cache->alias_lru, alias, lru_link);
		}
		pthread_mutex_unlock(&cache->lock);
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
		/* A partial native entry need not force S3 if CopyObject also
		 * aliased a complete source object for this destination uuid. */
		if (!alias && !cache->object_io_stopped) {
			alias = alias_find_locked(cache, chunk_index, uuid);
			if (alias) {
				slot = alias->slot;
				readable = offset_in_chunk < slot->valid_bytes
					   ? spdk_min(length, slot->valid_bytes -
						      offset_in_chunk) : 0;
				first_block = offset_in_chunk / cache->block_size;
				n_blocks = readable == 0 ? 0 :
					(offset_in_chunk + readable - 1) /
					cache->block_size + 1 - first_block;
			}
		}
		if (!alias || (readable != 0 &&
		    !bitmap_test_range(slot->bitmap, first_block, n_blocks))) {
			cache->stats.hits_declined++;
			if (alias) {
				cache->stats.object_hits_declined++;
				cache->stats.object_alias_misses++;
			} else if (!cache->object_io_stopped &&
				   alias_find_chunk_locked(cache, chunk_index)) {
				cache->stats.object_alias_misses++;
			}
			pthread_mutex_unlock(&cache->lock);
			return -ENOENT;
		}
		if (readable == 0) {
			memset(buf, 0, length);
			cache->stats.hits++;
			cache->stats.object_hits++;
			cache->stats.object_alias_hits++;
			TAILQ_REMOVE(&cache->alias_lru, alias, lru_link);
			TAILQ_INSERT_TAIL(&cache->alias_lru, alias, lru_link);
			pthread_mutex_unlock(&cache->lock);
			cb_fn(cb_arg, 0);
			return 0;
		}
	}
	if (!channel) {
		cache->stats.misses++;
		if (alias) {
			cache->stats.object_misses++;
			cache->stats.object_alias_misses++;
		}
		pthread_mutex_unlock(&cache->lock);
		return -ENOENT;
	}

	rd = calloc(1, sizeof(*rd));
	if (!rd) {
		cache->stats.misses++;
		if (alias) {
			cache->stats.object_misses++;
			cache->stats.object_alias_misses++;
		}
		pthread_mutex_unlock(&cache->lock);
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
	rd->bytes     = readable;
	rd->object_read = alias != NULL;
	rd->alias_read = alias != NULL;
	used_alias = alias != NULL;

	slot->pins++;
	/* Off the LRU while pinned, so eviction never has to look at pins. */
	slot_lru_remove(cache, slot);
	cache->reads_in_flight++;
	cache->stats.hits++;
	cache->stats.disk_hits++;
	cache->stats.bytes_served += readable;
	if (alias) {
		TAILQ_REMOVE(&cache->alias_lru, alias, lru_link);
		TAILQ_INSERT_TAIL(&cache->alias_lru, alias, lru_link);
	}
	device_offset = slot_offset(cache, slot) + offset_in_chunk;
	pthread_mutex_unlock(&cache->lock);

	rc = spdk_bdev_read(cache->desc, channel, buf,
			    device_offset,
			    read_len, cache_read_done, rd);
	if (rc != 0) {
		pthread_mutex_lock(&cache->lock);
		assert(slot->pins > 0);
		slot->pins--;
		slot_lru_touch(cache, slot);
		cache->reads_in_flight--;
		/* Almost always -ENOMEM from the bdev_io pool. Reported as a miss
		 * so the caller goes to S3 instead of failing the user's read. */
		assert(cache->stats.hits > 0);
		assert(cache->stats.disk_hits > 0);
		assert(cache->stats.bytes_served >= readable);
		cache->stats.hits--;
		cache->stats.disk_hits--;
		cache->stats.bytes_served -= readable;
		cache->stats.misses++;
		if (used_alias) {
			cache->stats.object_misses++;
			cache->stats.object_alias_misses++;
		}
		pthread_mutex_unlock(&cache->lock);
		free(rd);
		return -ENOENT;
	}

	return 0;
}

int
s3_cache_read(struct s3_cache *cache, uint64_t chunk_index,
	      const struct spdk_uuid *uuid, uint32_t offset_in_chunk,
	      uint32_t length, void *buf, s3_cache_read_cb cb_fn, void *cb_arg)
{
	if (!cache) {
		return -ENOENT;
	}
	assert(cache->owner_thread == spdk_get_thread());
	return s3_cache_read_on_channel(cache, cache->ch, chunk_index, uuid,
					offset_in_chunk, length, buf,
					cb_fn, cb_arg);
}

struct spdk_io_channel *
s3_cache_get_io_channel(struct s3_cache *cache)
{
	return cache ? spdk_bdev_get_io_channel(cache->desc) : NULL;
}

int
s3_cache_object_read_on_channel(struct s3_cache *cache,
				struct spdk_io_channel *channel,
				const struct s3_cache_object_id *id,
				uint32_t object_valid_bytes,
				uint32_t offset_in_object, uint32_t length,
				void *buf, s3_cache_read_cb cb_fn, void *cb_arg)
{
	struct cache_slot *slot;
	struct cache_read *rd;
	uint64_t hash;
	uint32_t readable, read_len, first_block, n_blocks;
	uint64_t device_offset;
	int rc;

	if (!cache || !object_id_valid(id) || !buf || !cb_fn || length == 0 ||
	    object_valid_bytes == 0 || object_valid_bytes > cache->chunk_size ||
	    offset_in_object % cache->block_size != 0 ||
	    length % cache->block_size != 0 ||
	    offset_in_object > cache->chunk_size ||
	    length > cache->chunk_size - offset_in_object) {
		return -ENOENT;
	}

	hash = object_id_hash(id);
	pthread_mutex_lock(&cache->lock);
	if (cache->object_io_stopped) {
		cache->stats.object_misses++;
		pthread_mutex_unlock(&cache->lock);
		return -ENOENT;
	}
	slot = object_find_locked(cache, id, hash);
	if (!slot || !slot->resident || slot->valid_bytes != object_valid_bytes) {
		cache->stats.object_misses++;
		pthread_mutex_unlock(&cache->lock);
		return -ENOENT;
	}

	readable = offset_in_object < slot->valid_bytes
		   ? spdk_min(length, slot->valid_bytes - offset_in_object) : 0;
	if (readable == 0) {
		memset(buf, 0, length);
		cache->stats.object_hits++;
		pthread_mutex_unlock(&cache->lock);
		cb_fn(cb_arg, 0);
		return 0;
	}

	first_block = offset_in_object / cache->block_size;
	n_blocks = (offset_in_object + readable - 1) / cache->block_size + 1 -
		   first_block;
	if (!bitmap_test_range(slot->bitmap, first_block, n_blocks)) {
		cache->stats.object_hits_declined++;
		pthread_mutex_unlock(&cache->lock);
		return -ENOENT;
	}
	if (!channel) {
		cache->stats.object_misses++;
		pthread_mutex_unlock(&cache->lock);
		return -ENOENT;
	}

	rd = calloc(1, sizeof(*rd));
	if (!rd) {
		cache->stats.object_misses++;
		pthread_mutex_unlock(&cache->lock);
		return -ENOENT;
	}
	read_len = spdk_divide_round_up(readable, cache->block_size) *
		   cache->block_size;
	rd->cache = cache;
	rd->slot = slot;
	rd->cb_fn = cb_fn;
	rd->cb_arg = cb_arg;
	rd->buf = buf;
	rd->zero_from = readable;
	rd->zero_len = length - readable;
	rd->bytes = readable;
	rd->object_read = true;

	slot->pins++;
	slot_lru_remove(cache, slot);
	cache->reads_in_flight++;
	device_offset = slot_offset(cache, slot) + offset_in_object;
	pthread_mutex_unlock(&cache->lock);

	rc = spdk_bdev_read(cache->desc, channel, buf, device_offset, read_len,
			    cache_read_done, rd);
	if (rc != 0) {
		pthread_mutex_lock(&cache->lock);
		assert(slot->pins > 0);
		slot->pins--;
		slot_lru_touch(cache, slot);
		assert(cache->reads_in_flight > 0);
		cache->reads_in_flight--;
		cache->stats.object_misses++;
		pthread_mutex_unlock(&cache->lock);
		free(rd);
		return -ENOENT;
	}
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

	pthread_mutex_lock(&cache->lock);
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

		if (fill->object_fill) {
			cache->stats.object_populates++;
			cache->stats.object_bytes_populated += fill->bytes;
		} else {
			cache->stats.populates++;
			cache->stats.bytes_populated += fill->bytes;
		}
		slot_lru_touch(cache, slot);
	} else {
		if (fill->object_fill) {
			cache->stats.object_populates_failed++;
		} else {
			cache->stats.populates_failed++;
		}

		/* Nothing to undo: the range was never marked present, so the
		 * half-written blocks are already unreadable. Only a slot that
		 * ended up holding nothing at all is worth reclaiming -- keeping
		 * it would occupy a slot that can never produce a hit. */
		if (slot->fills == 0 && slot->pins == 0 && !slot->hot &&
		    slot->filled_blocks == 0) {
			slot_release(cache, slot);
		} else {
			slot_lru_touch(cache, slot);
		}
	}

	TAILQ_INSERT_HEAD(&cache->staging_free, fill->staging, link);
	cache->fills_in_flight--;
	if (fill->object_fill) {
		assert(cache->object_fills_in_flight > 0);
		cache->object_fills_in_flight--;
	}
	pthread_mutex_unlock(&cache->lock);

	free(fill);
}

static void
cache_disk_fill_abort(struct cache_fill *fill)
{
	struct s3_cache *cache = fill->cache;
	struct cache_slot *slot = fill->slot;
	bool object_fill = fill->object_fill;

	pthread_mutex_lock(&cache->lock);
	assert(cache->fills_in_flight > 0);
	assert(slot->fills > 0);
	cache->fills_in_flight--;
	if (fill->object_fill) {
		assert(cache->object_fills_in_flight > 0);
		cache->object_fills_in_flight--;
	}
	slot->fills--;
	if (slot->fills == 0 && slot->pins == 0 && !slot->hot &&
	    slot->filled_blocks == 0) {
		slot_release(cache, slot);
	} else {
		slot_lru_touch(cache, slot);
	}
	TAILQ_INSERT_HEAD(&cache->staging_free, fill->staging, link);
	free(fill);
	if (object_fill) {
		cache->stats.object_populates_dropped++;
	} else {
		cache->stats.populates_dropped++;
	}
	pthread_mutex_unlock(&cache->lock);
}

static void
cache_submit_disk_fill(void *arg)
{
	struct cache_fill *fill = arg;
	struct s3_cache *cache = fill->cache;
	int rc;

	assert(cache->owner_thread == NULL ||
	       cache->owner_thread == spdk_get_thread());
	rc = spdk_bdev_write(cache->desc, cache->ch, fill->staging->buf,
			     fill->device_offset, fill->write_len,
			     cache_fill_done, fill);
	if (rc != 0) {
		cache_disk_fill_abort(fill);
	}
}

void
s3_cache_populate(struct s3_cache *cache, uint64_t chunk_index,
		  const struct spdk_uuid *uuid, uint32_t offset_in_chunk,
		  const void *buf, uint32_t length, uint32_t object_valid_bytes)
{
	struct cache_slot *slot;
	struct cache_staging *staging = NULL;
	struct cache_fill *fill = NULL;
	struct cache_hot *hot = NULL;
	uint32_t first_block, last_block, n_blocks;
	uint32_t end_byte, write_len;
	uint64_t device_offset;
	bool whole_object;
	bool disk_present = false;
	bool hot_present = false;
	bool hot_publishing = false;
	int rc;

	if (!cache || !uuid || !buf || length == 0 || object_valid_bytes == 0) {
		return;
	}
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
	whole_object = offset_in_chunk == 0 && end_byte == object_valid_bytes;

	pthread_mutex_lock(&cache->lock);
	hot = hot_for_chunk(cache, chunk_index);
	hot_present = whole_object && hot &&
		      spdk_uuid_compare(&hot->uuid, uuid) == 0;
	hot = NULL;
	slot = slot_for_chunk(cache, chunk_index);
	if (slot) {
		/* Unpublished hot is on the slot, not chunk_to_hot, so lookups
		 * cannot memcpy a half-written buf. A second populate of the
		 * same uuid must not acquire another. */
		hot_publishing = whole_object && slot->hot &&
				 !slot->hot->resident &&
				 spdk_uuid_compare(&slot->hot->uuid, uuid) == 0;
		if (slot->resident && spdk_uuid_compare(&slot->uuid, uuid) == 0) {
			/* Same object. Adding a range to it is the normal case
			 * now, so this is where a sequential read stream lands
			 * repeatedly -- but skip what is already here rather than
			 * rewrite it. In-flight ranges are not tracked, so an
			 * overlapping concurrent fill can still write the same
			 * bytes twice; harmless, since the same uuid at the same
			 * offset is by definition the same data. */
			disk_present = bitmap_test_range(slot->bitmap, first_block,
						 n_blocks);
			if (disk_present && (!whole_object || hot_present ||
					     hot_publishing)) {
				pthread_mutex_unlock(&cache->lock);
				return;
			}
		} else if (slot->fills > 0 || slot->pins > 0) {
			/* A different version, with I/O in flight against the
			 * slot. Taking it over would write under a reader or let
			 * a landing fill mark ranges of the wrong object. */
			cache->stats.populates_dropped++;
			pthread_mutex_unlock(&cache->lock);
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
		slot = slot_acquire_native(cache);
		if (!slot) {
			cache->stats.populates_dropped++;
			pthread_mutex_unlock(&cache->lock);
			return;
		}
	}

	/* Claim the slot before either copy. valid_bytes is set now rather than on
	 * completion because it describes the object, not what has landed: it
	 * comes from the chunk map, and a read entirely past it is answerable
	 * without any block being present. */
	if (!slot->resident) {
		spdk_uuid_copy(&slot->uuid, uuid);
		slot->chunk_index = chunk_index;
		slot->kind        = CACHE_SLOT_NATIVE;
		slot->resident    = true;
		cache->chunk_to_slot[chunk_index] = (uint32_t)(slot - cache->slots);
		cache->resident++;
	}
	slot->valid_bytes = object_valid_bytes;

	/* Only whole immutable objects enter the RAM tier. The hot identity is
	 * independent of the disk slot, so disk pressure cannot evict RAM. */
	if (hot_present) {
		struct cache_hot *existing = hot_for_chunk(cache, chunk_index);

		assert(existing != NULL);
		if (!existing->slot) {
			existing->slot = slot;
			slot->hot = existing;
		}
	} else if (whole_object && !hot_publishing) {
		struct cache_hot *old = hot_for_chunk(cache, chunk_index);

		if (old) {
			hot_detach(cache, old);
		}
		hot = hot_acquire(cache);
		if (hot) {
			assert(hot->slot == NULL);
			assert(hot->pins == 0);
			assert(slot->hot == NULL);
			hot->pins = 1; /* private populate reservation */
			spdk_uuid_copy(&hot->uuid, uuid);
			hot->chunk_index = chunk_index;
			hot->valid_bytes = object_valid_bytes;
			hot->resident = false;
			hot->slot = slot;
			slot->hot = hot;
			slot->pins++;
			slot_lru_remove(cache, slot);
		}
	}

	if (!disk_present) {
		staging = TAILQ_FIRST(&cache->staging_free);
		if (staging) {
			fill = calloc(1, sizeof(*fill));
		}
		if (staging && fill) {
			TAILQ_REMOVE(&cache->staging_free, staging, link);
			fill->cache       = cache;
			fill->slot        = slot;
			fill->staging     = staging;
			fill->first_block = first_block;
			fill->n_blocks    = n_blocks;
			fill->bytes       = end_byte - offset_in_chunk;
			fill->write_len   = 0;
			fill->device_offset = 0;
			slot->fills++;
			slot_lru_remove(cache, slot);
			cache->fills_in_flight++;
		} else {
			free(fill);
			fill = NULL;
			cache->stats.populates_dropped++;
		}
	}

	if (!hot && !fill) {
		if (slot->fills == 0 && slot->pins == 0 && !slot->hot &&
		    slot->filled_blocks == 0) {
			slot_release(cache, slot);
		} else {
			slot_lru_touch(cache, slot);
		}
		pthread_mutex_unlock(&cache->lock);
		return;
	}

	write_len = n_blocks * cache->block_size;
	device_offset = slot_offset(cache, slot) +
			(uint64_t)first_block * cache->block_size;
	if (fill) {
		fill->write_len = write_len;
		fill->device_offset = device_offset;
	}
	pthread_mutex_unlock(&cache->lock);

	if (hot) {
		memcpy(hot->buf, buf, object_valid_bytes);

		pthread_mutex_lock(&cache->lock);
		assert(slot->pins > 0);
		assert(hot->pins == 1);
		assert(hot->slot == slot);
		assert(slot->hot == hot);
		assert(!hot->resident);
		assert(spdk_uuid_compare(&hot->uuid, uuid) == 0);
		hot->valid_bytes = object_valid_bytes;
		hot->resident = true;
		cache->chunk_to_hot[chunk_index] =
			(uint32_t)(hot - cache->hot);
		hot->pins = 0;
		cache->hot_resident++;
		slot->pins--;
		hot_lru_touch(cache, hot);
		slot_lru_touch(cache, slot);
		pthread_mutex_unlock(&cache->lock);
	}

	if (!fill) {
		return;
	}

	/* Staging remains the only DMA source. RAM hot entries are plain mmap
	 * memory and may be evicted independently while this write is in flight. */
	memcpy(staging->buf, buf, end_byte - offset_in_chunk);
	if (write_len > end_byte - offset_in_chunk) {
		memset((uint8_t *)staging->buf + (end_byte - offset_in_chunk), 0,
		       write_len - (end_byte - offset_in_chunk));
	}
	if (cache->owner_thread == NULL ||
	    cache->owner_thread == spdk_get_thread()) {
		cache_submit_disk_fill(fill);
	} else {
		rc = spdk_thread_send_msg(cache->owner_thread,
					  cache_submit_disk_fill, fill);
		if (rc != 0) {
			cache_disk_fill_abort(fill);
		}
	}
}

void
s3_cache_object_populate(struct s3_cache *cache,
			 const struct s3_cache_object_id *id,
			 uint32_t offset_in_object, const void *buf,
			 uint32_t length, uint32_t object_valid_bytes)
{
	struct cache_slot *slot;
	struct cache_staging *staging;
	struct cache_fill *fill;
	uint64_t hash;
	uint32_t first_block, last_block, n_blocks, end_byte, write_len;
	int rc;

	if (!cache || !object_id_valid(id) || !buf || length == 0 ||
	    object_valid_bytes == 0 || object_valid_bytes > cache->chunk_size ||
	    offset_in_object % cache->block_size != 0 ||
	    offset_in_object >= object_valid_bytes ||
	    cache->object_hash_buckets == 0) {
		return;
	}

	end_byte = offset_in_object +
		   spdk_min(length, object_valid_bytes - offset_in_object);
	first_block = offset_in_object / cache->block_size;
	if (end_byte >= object_valid_bytes) {
		last_block = (object_valid_bytes - 1) / cache->block_size;
	} else {
		if (end_byte < cache->block_size + offset_in_object) {
			return;
		}
		last_block = end_byte / cache->block_size - 1;
	}
	if (last_block < first_block) {
		return;
	}
	n_blocks = last_block - first_block + 1;
	hash = object_id_hash(id);

	pthread_mutex_lock(&cache->lock);
	if (cache->object_io_stopped) {
		cache->stats.object_populates_dropped++;
		pthread_mutex_unlock(&cache->lock);
		return;
	}
	slot = object_find_locked(cache, id, hash);
	if (slot) {
		if (slot->valid_bytes != object_valid_bytes) {
			cache->stats.object_populates_dropped++;
			pthread_mutex_unlock(&cache->lock);
			return;
		}
		if (bitmap_test_range(slot->bitmap, first_block, n_blocks)) {
			pthread_mutex_unlock(&cache->lock);
			return;
		}
	} else {
		char *endpoint = strdup(id->endpoint);
		char *bucket = strdup(id->bucket);
		char *key = strdup(id->key);

		if (!endpoint || !bucket || !key) {
			free(endpoint);
			free(bucket);
			free(key);
			cache->stats.object_populates_dropped++;
			pthread_mutex_unlock(&cache->lock);
			return;
		}
		slot = slot_acquire_object(cache);
		if (!slot) {
			free(endpoint);
			free(bucket);
			free(key);
			cache->stats.object_populates_dropped++;
			pthread_mutex_unlock(&cache->lock);
			return;
		}
		slot->kind = CACHE_SLOT_OBJECT;
		slot->resident = true;
		slot->valid_bytes = object_valid_bytes;
		slot->object_hash = hash;
		slot->object_endpoint = endpoint;
		slot->object_bucket = bucket;
		slot->object_key = key;
		object_index_insert_locked(cache, slot);
		cache->resident++;
		cache->object_resident++;
	}

	if (cache->object_fills_in_flight >= S3_CACHE_OBJECT_FILLS_MAX) {
		cache->stats.object_populates_dropped++;
		if (slot->fills == 0 && slot->pins == 0 &&
		    slot->filled_blocks == 0) {
			slot_release(cache, slot);
		}
		pthread_mutex_unlock(&cache->lock);
		return;
	}

	staging = TAILQ_FIRST(&cache->staging_free);
	fill = staging ? calloc(1, sizeof(*fill)) : NULL;
	if (!staging || !fill) {
		free(fill);
		cache->stats.object_populates_dropped++;
		if (slot->fills == 0 && slot->pins == 0 &&
		    slot->filled_blocks == 0) {
			slot_release(cache, slot);
		}
		pthread_mutex_unlock(&cache->lock);
		return;
	}

	TAILQ_REMOVE(&cache->staging_free, staging, link);
	fill->cache = cache;
	fill->slot = slot;
	fill->staging = staging;
	fill->first_block = first_block;
	fill->n_blocks = n_blocks;
	fill->bytes = end_byte - offset_in_object;
	fill->write_len = n_blocks * cache->block_size;
	fill->device_offset = slot_offset(cache, slot) +
			      (uint64_t)first_block * cache->block_size;
	fill->object_fill = true;
	slot->fills++;
	slot_lru_remove(cache, slot);
	cache->fills_in_flight++;
	cache->object_fills_in_flight++;
	write_len = fill->write_len;
	pthread_mutex_unlock(&cache->lock);

	memcpy(staging->buf, buf, end_byte - offset_in_object);
	if (write_len > end_byte - offset_in_object) {
		memset((uint8_t *)staging->buf + end_byte - offset_in_object, 0,
		       write_len - (end_byte - offset_in_object));
	}
	if (cache->owner_thread == NULL ||
	    cache->owner_thread == spdk_get_thread()) {
		cache_submit_disk_fill(fill);
	} else {
		rc = spdk_thread_send_msg(cache->owner_thread,
					  cache_submit_disk_fill, fill);
		if (rc != 0) {
			cache_disk_fill_abort(fill);
		}
	}
}

void
s3_cache_object_alias(struct s3_cache *cache,
		      const struct s3_cache_object_id *source,
		      uint64_t dest_chunk_index,
		      const struct spdk_uuid *dest_uuid,
		      uint32_t object_valid_bytes)
{
	struct cache_alias *alias, *old;
	struct cache_slot *slot;
	uint64_t hash;

	if (!cache || !object_id_valid(source) || !dest_uuid ||
	    spdk_uuid_is_null(dest_uuid) ||
	    dest_chunk_index >= cache->num_chunks ||
	    object_valid_bytes == 0 || object_valid_bytes > cache->chunk_size ||
	    cache->alias_hash_buckets == 0) {
		return;
	}

	alias = calloc(1, sizeof(*alias));
	if (!alias) {
		return;
	}
	hash = object_id_hash(source);

	pthread_mutex_lock(&cache->lock);
	if (cache->object_io_stopped) {
		pthread_mutex_unlock(&cache->lock);
		free(alias);
		return;
	}
	slot = object_find_locked(cache, source, hash);
	if (!slot || !slot->resident ||
	    slot->valid_bytes != object_valid_bytes) {
		pthread_mutex_unlock(&cache->lock);
		free(alias);
		return;
	}

	old = alias_find_chunk_locked(cache, dest_chunk_index);
	if (old &&
	    spdk_uuid_compare(&old->dest_uuid, dest_uuid) == 0 &&
	    old->slot == slot) {
		TAILQ_REMOVE(&cache->alias_lru, old, lru_link);
		TAILQ_INSERT_TAIL(&cache->alias_lru, old, lru_link);
		pthread_mutex_unlock(&cache->lock);
		free(alias);
		return;
	}
	if (old) {
		/* Rebinding one destination chunk replaces stale identity; it is
		 * not pressure against the bounded alias metadata pool. */
		alias_remove_locked(cache, old, false);
	}
	while (cache->alias_count >= cache->alias_limit) {
		old = TAILQ_FIRST(&cache->alias_lru);
		if (!old) {
			break;
		}
		alias_remove_locked(cache, old, true);
	}

	alias->dest_chunk_index = dest_chunk_index;
	spdk_uuid_copy(&alias->dest_uuid, dest_uuid);
	alias->slot = slot;
	alias_insert_locked(cache, alias);
	cache->stats.object_alias_registers++;
	pthread_mutex_unlock(&cache->lock);
}

void
s3_cache_stop_object_io(struct s3_cache *cache)
{
	if (!cache) {
		return;
	}

	pthread_mutex_lock(&cache->lock);
	cache->object_io_stopped = true;
	pthread_mutex_unlock(&cache->lock);
}

void
s3_cache_drop_chunk(struct s3_cache *cache, uint64_t chunk_index)
{
	struct cache_slot *slot;
	struct cache_hot *hot;

	if (!cache) {
		return;
	}
	assert(cache->owner_thread == spdk_get_thread());

	pthread_mutex_lock(&cache->lock);
	hot = hot_for_chunk(cache, chunk_index);
	if (hot) {
		hot_detach(cache, hot);
	}
	slot = slot_for_chunk(cache, chunk_index);
	if (!slot) {
		pthread_mutex_unlock(&cache->lock);
		return;
	}
	if (slot->fills > 0 || slot->pins > 0) {
		/* I/O in flight owns the slot. Leaving it alone is safe: the uuid
		 * tag means nothing can read it as a newer version, and it will be
		 * reused or evicted later. */
		pthread_mutex_unlock(&cache->lock);
		return;
	}

	slot_release(cache, slot);
	pthread_mutex_unlock(&cache->lock);
}

bool
s3_cache_is_quiesced(const struct s3_cache *cache)
{
	if (!cache) {
		return true;
	}

	pthread_mutex_lock((pthread_mutex_t *)&cache->lock);
	bool quiesced = cache->fills_in_flight == 0 &&
			 cache->reads_in_flight == 0;
	pthread_mutex_unlock((pthread_mutex_t *)&cache->lock);
	return quiesced;
}

void
s3_cache_get_stats(struct s3_cache *cache, struct s3_cache_stats *stats)
{
	if (!cache || !stats) {
		return;
	}

	pthread_mutex_lock(&cache->lock);
	cache->stats.slots_resident = cache->resident;
	cache->stats.bytes_resident = cache->resident_blocks *
				      cache->block_size;
	cache->stats.hot_slots_resident = cache->hot_resident;
	cache->stats.object_slots_resident = cache->object_resident;
	cache->stats.object_aliases_resident = cache->alias_count;
	*stats = cache->stats;
	pthread_mutex_unlock(&cache->lock);
}
