/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Process-wide LRU of immutable export objects. Rationale is in
 *   include/s3lvol/s3_export_obj_cache.h; this file is the mechanism.
 */

#include "s3lvol/s3_export_obj_cache.h"

#include <errno.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/queue.h>

#define CACHE_BUCKETS 256
#define CACHE_BLOCK_SHIFT 12

struct cache_slot {
	char                         key[S3_EXPORT_OBJ_CACHE_KEY_MAX];
	uint32_t                     chunk_size;
	uint8_t                     *buf;
	uint8_t                     *present;
	uint32_t                     present_bytes;
	TAILQ_ENTRY(cache_slot)      hash_link;
	TAILQ_ENTRY(cache_slot)      lru_link;
};

TAILQ_HEAD(cache_slot_list, cache_slot);

struct export_obj_cache {
	pthread_mutex_t              lock;
	struct cache_slot_list       buckets[CACHE_BUCKETS];
	struct cache_slot_list       lru;
	uint32_t                     nslots;
	uint32_t                     max_slots;
	struct s3_export_obj_cache_stats stats;
};

static struct export_obj_cache g_cache;
static pthread_once_t g_once = PTHREAD_ONCE_INIT;

static uint32_t
key_hash(const char *key)
{
	uint32_t h = 5381;
	unsigned char c;

	while ((c = (unsigned char)*key++) != 0) {
		h = ((h << 5) + h) + c;
	}
	return h % CACHE_BUCKETS;
}

static uint32_t
present_bytes_for(uint32_t chunk_size)
{
	uint32_t blocks = chunk_size >> CACHE_BLOCK_SHIFT;

	return (blocks + 7) / 8;
}

static bool
range_present(const struct cache_slot *slot, uint32_t offset, uint32_t length)
{
	uint32_t first;
	uint32_t last;
	uint32_t bit;

	if (length == 0) {
		return true;
	}
	if (offset + length > slot->chunk_size) {
		return false;
	}
	first = offset >> CACHE_BLOCK_SHIFT;
	last = (offset + length - 1) >> CACHE_BLOCK_SHIFT;
	for (bit = first; bit <= last; bit++) {
		if ((slot->present[bit / 8] & (1u << (bit % 8))) == 0) {
			return false;
		}
	}
	return true;
}

static void
range_mark(struct cache_slot *slot, uint32_t offset, uint32_t length)
{
	uint32_t first;
	uint32_t last;
	uint32_t bit;

	if (length == 0) {
		return;
	}
	first = offset >> CACHE_BLOCK_SHIFT;
	last = (offset + length - 1) >> CACHE_BLOCK_SHIFT;
	for (bit = first; bit <= last; bit++) {
		slot->present[bit / 8] |= (uint8_t)(1u << (bit % 8));
	}
}

static void
slot_free(struct cache_slot *slot)
{
	free(slot->buf);
	free(slot->present);
	free(slot);
}

static void
cache_init(void)
{
	uint32_t i;

	memset(&g_cache, 0, sizeof(g_cache));
	pthread_mutex_init(&g_cache.lock, NULL);
	for (i = 0; i < CACHE_BUCKETS; i++) {
		TAILQ_INIT(&g_cache.buckets[i]);
	}
	TAILQ_INIT(&g_cache.lru);
	g_cache.max_slots = S3_EXPORT_OBJ_CACHE_SLOTS_DEFAULT;
}

static struct export_obj_cache *
cache_get(void)
{
	pthread_once(&g_once, cache_init);
	return &g_cache;
}

static struct cache_slot *
slot_find(struct export_obj_cache *cache, const char *key)
{
	struct cache_slot *slot;

	TAILQ_FOREACH(slot, &cache->buckets[key_hash(key)], hash_link) {
		if (strcmp(slot->key, key) == 0) {
			return slot;
		}
	}
	return NULL;
}

static void
slot_touch(struct export_obj_cache *cache, struct cache_slot *slot)
{
	TAILQ_REMOVE(&cache->lru, slot, lru_link);
	TAILQ_INSERT_TAIL(&cache->lru, slot, lru_link);
}

static void
slot_unlink(struct export_obj_cache *cache, struct cache_slot *slot)
{
	TAILQ_REMOVE(&cache->buckets[key_hash(slot->key)], slot, hash_link);
	TAILQ_REMOVE(&cache->lru, slot, lru_link);
	cache->nslots--;
}

static int
slot_evict_one(struct export_obj_cache *cache)
{
	struct cache_slot *slot;

	slot = TAILQ_FIRST(&cache->lru);
	if (!slot) {
		return -ENOMEM;
	}
	slot_unlink(cache, slot);
	slot_free(slot);
	cache->stats.evicts++;
	return 0;
}

static struct cache_slot *
slot_acquire(struct export_obj_cache *cache, const char *key,
	     uint32_t chunk_size)
{
	struct cache_slot *slot;
	uint32_t present_bytes;

	slot = slot_find(cache, key);
	if (slot) {
		if (slot->chunk_size != chunk_size) {
			slot_unlink(cache, slot);
			slot_free(slot);
			slot = NULL;
		} else {
			slot_touch(cache, slot);
			return slot;
		}
	}

	while (cache->nslots >= cache->max_slots) {
		if (slot_evict_one(cache) != 0) {
			return NULL;
		}
	}

	present_bytes = present_bytes_for(chunk_size);
	slot = calloc(1, sizeof(*slot));
	if (!slot) {
		return NULL;
	}
	snprintf(slot->key, sizeof(slot->key), "%s", key);
	slot->chunk_size = chunk_size;
	slot->buf = calloc(1, chunk_size);
	slot->present = calloc(1, present_bytes);
	slot->present_bytes = present_bytes;
	if (!slot->buf || !slot->present) {
		slot_free(slot);
		return NULL;
	}

	TAILQ_INSERT_HEAD(&cache->buckets[key_hash(key)], slot, hash_link);
	TAILQ_INSERT_TAIL(&cache->lru, slot, lru_link);
	cache->nslots++;
	return slot;
}

int
s3_export_obj_cache_copy(const char *key, uint32_t chunk_size,
			 uint32_t offset, uint32_t length, void *dst)
{
	struct export_obj_cache *cache;
	struct cache_slot *slot;
	int rc = -ENOENT;

	if (!key || key[0] == '\0' || !dst || chunk_size == 0 ||
	    (chunk_size & (S3_EXPORT_OBJ_CACHE_BLOCK_SIZE - 1)) != 0) {
		return -EINVAL;
	}
	if (length == 0) {
		return 0;
	}
	if (offset + length > chunk_size) {
		return -EINVAL;
	}

	cache = cache_get();
	pthread_mutex_lock(&cache->lock);
	slot = slot_find(cache, key);
	if (slot && slot->chunk_size == chunk_size &&
	    range_present(slot, offset, length)) {
		memcpy(dst, slot->buf + offset, length);
		slot_touch(cache, slot);
		cache->stats.hits++;
		rc = 0;
	} else {
		cache->stats.misses++;
	}
	pthread_mutex_unlock(&cache->lock);
	return rc;
}

void
s3_export_obj_cache_populate(const char *key, uint32_t chunk_size,
			     uint32_t offset, uint32_t length,
			     const void *src)
{
	struct export_obj_cache *cache;
	struct cache_slot *slot;

	if (!key || key[0] == '\0' || !src || chunk_size == 0 || length == 0 ||
	    (chunk_size & (S3_EXPORT_OBJ_CACHE_BLOCK_SIZE - 1)) != 0 ||
	    offset + length > chunk_size) {
		return;
	}

	cache = cache_get();
	pthread_mutex_lock(&cache->lock);
	slot = slot_acquire(cache, key, chunk_size);
	if (slot) {
		memcpy(slot->buf + offset, src, length);
		range_mark(slot, offset, length);
		cache->stats.populates++;
	}
	pthread_mutex_unlock(&cache->lock);
}

void
s3_export_obj_cache_stats(struct s3_export_obj_cache_stats *out)
{
	struct export_obj_cache *cache;

	if (!out) {
		return;
	}
	cache = cache_get();
	pthread_mutex_lock(&cache->lock);
	*out = cache->stats;
	pthread_mutex_unlock(&cache->lock);
}

void
s3_export_obj_cache_reset(void)
{
	struct export_obj_cache *cache;
	struct cache_slot *slot;
	uint32_t i;

	cache = cache_get();
	pthread_mutex_lock(&cache->lock);
	for (i = 0; i < CACHE_BUCKETS; i++) {
		while ((slot = TAILQ_FIRST(&cache->buckets[i])) != NULL) {
			slot_unlink(cache, slot);
			slot_free(slot);
		}
	}
	memset(&cache->stats, 0, sizeof(cache->stats));
	pthread_mutex_unlock(&cache->lock);
}
