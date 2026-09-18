/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Process-wide export object cache -- no S3, no DPDK, no threads
 *
 *   The property that matters is that a populate survives a later "import
 *   gone" (reset of no device state, because there is none). A second copy of
 *   the same key must hit. A range that was never populated must miss, even
 *   on a slot that holds neighbouring blocks of the same object.
 *
 *   Usage:
 *     ./s3_export_obj_cache_test
 */

#include <errno.h>
#include <inttypes.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "s3lvol/s3_export_obj_cache.h"

#define CHUNK_SIZE (1024 * 1024)
#define BLOCK_SIZE S3_EXPORT_OBJ_CACHE_BLOCK_SIZE

static int g_pass;
static int g_fail;

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("\t[PASS] %s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("\t[FAIL] %s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

static void
check_int(const char *what, int got, int want)
{
	char detail[128];

	snprintf(detail, sizeof(detail), "(got %d, want %d)", got, want);
	check_true(what, got == want, detail);
}

static void
check_u64(const char *what, uint64_t got, uint64_t want)
{
	char detail[128];

	snprintf(detail, sizeof(detail), "(got %" PRIu64 ", want %" PRIu64 ")",
		 got, want);
	check_true(what, got == want, detail);
}

int
main(void)
{
	uint8_t src[BLOCK_SIZE];
	uint8_t dst[BLOCK_SIZE];
	uint8_t other[BLOCK_SIZE];
	struct s3_export_obj_cache_stats stats;
	uint32_t i;
	char key[64];
	int rc;

	memset(src, 0x5a, sizeof(src));
	memset(other, 0x3c, sizeof(other));

	printf("[1] miss then populate then hit\n");
	s3_export_obj_cache_reset();
	memset(dst, 0, sizeof(dst));
	rc = s3_export_obj_cache_copy("obj-a", CHUNK_SIZE, 0, sizeof(src), dst);
	check_int("cold copy misses", rc, -ENOENT);
	s3_export_obj_cache_populate("obj-a", CHUNK_SIZE, 0, sizeof(src), src);
	rc = s3_export_obj_cache_copy("obj-a", CHUNK_SIZE, 0, sizeof(src), dst);
	check_int("populated copy hits", rc, 0);
	check_true("hit returns the populated bytes",
		   memcmp(dst, src, sizeof(src)) == 0, NULL);

	printf("\n[2] destroy of the importer does not drop the object\n");
	/* There is no per-device state to tear down. reset() is the test hook
	 * for dropping the cache; not calling it is the production destroy path. */
	memset(dst, 0, sizeof(dst));
	rc = s3_export_obj_cache_copy("obj-a", CHUNK_SIZE, 0, sizeof(src), dst);
	check_int("copy after import destroy still hits", rc, 0);
	check_true("second restore reads the first restore's object",
		   memcmp(dst, src, sizeof(src)) == 0, NULL);

	printf("\n[3] a different object misses\n");
	rc = s3_export_obj_cache_copy("obj-b", CHUNK_SIZE, 0, sizeof(src), dst);
	check_int("other key misses", rc, -ENOENT);

	printf("\n[4] unpopulated neighbour range of the same object misses\n");
	memset(dst, 0xff, sizeof(dst));
	rc = s3_export_obj_cache_copy("obj-a", CHUNK_SIZE, BLOCK_SIZE,
				      sizeof(dst), dst);
	check_int("unpopulated 4KiB misses", rc, -ENOENT);
	check_true("a miss does not scribble the destination",
		   dst[0] == 0xff && dst[sizeof(dst) - 1] == 0xff, NULL);

	printf("\n[5] populating the neighbour does not change the first range\n");
	s3_export_obj_cache_populate("obj-a", CHUNK_SIZE, BLOCK_SIZE,
				     sizeof(other), other);
	memset(dst, 0, sizeof(dst));
	rc = s3_export_obj_cache_copy("obj-a", CHUNK_SIZE, 0, sizeof(src), dst);
	check_int("first range still hits", rc, 0);
	check_true("first range is still 0x5a",
		   memcmp(dst, src, sizeof(src)) == 0, NULL);
	rc = s3_export_obj_cache_copy("obj-a", CHUNK_SIZE, BLOCK_SIZE,
				      sizeof(other), dst);
	check_int("second range hits", rc, 0);
	check_true("second range is 0x3c",
		   memcmp(dst, other, sizeof(other)) == 0, NULL);

	printf("\n[6] LRU evicts the coldest key once the slot cap is full\n");
	s3_export_obj_cache_reset();
	src[0] = 1;
	s3_export_obj_cache_populate("keep-me", BLOCK_SIZE, 0, sizeof(src), src);
	for (i = 0; i < S3_EXPORT_OBJ_CACHE_SLOTS_DEFAULT; i++) {
		src[0] = (uint8_t)(i + 2);
		snprintf(key, sizeof(key), "fill-%u", i);
		s3_export_obj_cache_populate(key, BLOCK_SIZE, 0, sizeof(src), src);
	}
	rc = s3_export_obj_cache_copy("keep-me", BLOCK_SIZE, 0, sizeof(src), dst);
	check_int("untouched first key was evicted", rc, -ENOENT);
	src[0] = (uint8_t)(S3_EXPORT_OBJ_CACHE_SLOTS_DEFAULT + 1);
	rc = s3_export_obj_cache_copy("fill-1023", BLOCK_SIZE, 0, sizeof(src), dst);
	check_int("newest fill still hits", rc, 0);

	printf("\n[7] reset drops hits and the objects\n");
	s3_export_obj_cache_stats(&stats);
	check_true("evicts were recorded", stats.evicts >= 1, NULL);
	s3_export_obj_cache_reset();
	rc = s3_export_obj_cache_copy("fill-1023", CHUNK_SIZE, 0, sizeof(src), dst);
	check_int("reset misses", rc, -ENOENT);
	s3_export_obj_cache_stats(&stats);
	check_u64("reset zeroes hits", stats.hits, 0);
	check_u64("reset zeroes populates", stats.populates, 0);

	printf("\n%d passed, %d failed\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
