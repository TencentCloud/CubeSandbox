/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Process-wide read cache for imported export objects
 *
 *   Cross-node FromSnap creates a new esnap clone every time. The parent
 *   s3_export_bs_dev dies with that clone, so a per-device working set cannot
 *   help the next restore of the same snapshot on the same node.
 *
 *   Objects are immutable and named by their S3 key. This cache is keyed by
 *   that key, lives for the process, and is not torn down when an import is
 *   destroyed. A later import of the same snapshot on this node can restore
 *   without repeating the GETs.
 *
 *   It holds clean data only. A miss is always safe; the worst it can do is
 *   fetch again. Residency is a bitmap of 4 KiB units inside a chunk-sized
 *   slot, because restore I/O is 4 KiB faults, not whole objects.
 */

#ifndef S3LVOL_EXPORT_OBJ_CACHE_H
#define S3LVOL_EXPORT_OBJ_CACHE_H

#include <stdint.h>
#include <stddef.h>

#define S3_EXPORT_OBJ_CACHE_SLOTS_DEFAULT 1024
#define S3_EXPORT_OBJ_CACHE_BLOCK_SIZE    4096
#define S3_EXPORT_OBJ_CACHE_KEY_MAX       512

struct s3_export_obj_cache_stats {
	uint64_t hits;
	uint64_t misses;
	uint64_t populates;
	uint64_t evicts;
};

int s3_export_obj_cache_copy(const char *key, uint32_t chunk_size,
			     uint32_t offset, uint32_t length, void *dst);

void s3_export_obj_cache_populate(const char *key, uint32_t chunk_size,
				  uint32_t offset, uint32_t length,
				  const void *src);

void s3_export_obj_cache_stats(struct s3_export_obj_cache_stats *out);

/* Test helper: drop every slot and zero the counters. */
void s3_export_obj_cache_reset(void);

#endif /* S3LVOL_EXPORT_OBJ_CACHE_H */
