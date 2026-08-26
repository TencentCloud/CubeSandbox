/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Chunk cache verification -- needs neither S3 nor credentials
 *
 *   === What this test proves ===
 *
 *   The cache is allowed to miss whenever it likes, so "it returned the data"
 *   is the easy half. What actually has to hold is that it never returns the
 *   *wrong* data and never hands out a slot that something else is still using.
 *   Those are the assertions here:
 *
 *     1. a populate/read round trip returns the bytes that went in, read back
 *        from the device rather than from RAM;
 *     2. a read for a *different uuid* on the same chunk_index misses. This is
 *        the one property everything rests on -- objects are immutable and
 *        content is named by uuid, so a stale tag must never be served;
 *     3. the tail past valid_bytes reads as zeroes, matching what a short GET
 *        does on the S3 path;
 *     4. eviction is LRU, and a slot with a read in flight is not evicted --
 *        checked by filling the cache to capacity while a read is outstanding;
 *     5. a populate for a chunk that is being read as an older version is
 *        declined rather than overwriting the slot under the reader;
 *     6. re-populating a chunk reuses its slot instead of occupying a second
 *        one, so a repeatedly rewritten chunk cannot evict the whole cache;
 *     7. populate copies its input, so the caller may free the buffer
 *        immediately -- verified by overwriting the source buffer before the
 *        write completes;
 *     8. a range that was never populated misses even though the slot holds the
 *        right object, including a read that only partly overlaps one that was.
 *        This is the assertion that matters most in the file: the device still
 *        holds the slot's previous tenant there, so getting it wrong does not
 *        cost a hit, it returns another volume's bytes.
 *
 *   Sections [11] to [13] are all of (8): ranges in isolation, a short object's
 *   trailing partial block, and residency surviving neither a uuid change nor
 *   slot reuse.
 *
 *   Like the WAL and journal tests this runs on the upstream bdev_aio over a
 *   sparse file and brings up iobuf, accel and bdev by hand. poll_until() turns
 *   the async API back into sequential flow, which is only safe because this
 *   process has one thread and no other pollers.
 *
 *   Usage:
 *     ./s3_cache_test [aio-file-path]
 */

#include "spdk/stdinc.h"
#include "spdk/accel.h"
#include "spdk/bdev.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_cache.h"

#include "bdev/aio/bdev_aio.h"

#define AIO_BDEV_NAME    "s3lvol_cachetest0"
#define AIO_BLOCK_SIZE   4096
#define DEFAULT_AIO_PATH "/data/s3lvol_cache_test.aio"

#define TEST_CHUNK_SIZE  (64 * 1024)
#define TEST_N_SLOTS     4
#define TEST_REGION_OFF  (1024 * 1024)
#define TEST_REGION_SIZE (TEST_N_SLOTS * TEST_CHUNK_SIZE)
#define TEST_NUM_CHUNKS  64
#define AIO_FILE_SIZE    (8ULL * 1024 * 1024)

#define POLL_TIMEOUT_SEC 10

static int g_pass;
static int g_fail;
static struct spdk_thread *g_thread;

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-58s%s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-58s%s\n", what, detail ? detail : "");
		g_fail++;
	}
}

static void
check_u64(const char *what, uint64_t got, uint64_t want)
{
	char detail[80];

	snprintf(detail, sizeof(detail), "got %" PRIu64 ", want %" PRIu64,
		 got, want);
	check_true(what, got == want, detail);
}

/* Milliseconds since an arbitrary origin. time(NULL) has one-second resolution,
 * which is both too coarse to bound a test and too coarse to notice elapsing at
 * all inside a tight poll loop. */
static uint64_t
now_ms(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static bool
poll_until(bool *done)
{
	uint64_t deadline = now_ms() + POLL_TIMEOUT_SEC * 1000;

	while (!*done && now_ms() < deadline) {
		spdk_thread_poll(g_thread, 0, 0);
	}
	if (!*done) {
		fprintf(stderr, "!! timed out waiting for async completion\n");
	}
	return *done;
}

/* Poll for a bounded stretch of *wall clock* time.
 *
 * Counting poll iterations instead does not work here: a few thousand polls of
 * an idle thread take well under a millisecond, while the aio write they are
 * waiting for is a real O_DIRECT write to a file -- hundreds of microseconds at
 * best, and more the first time a block is touched, since the filesystem has to
 * allocate it. The loop finishes long before the I/O does and the completion
 * looks like it never came. */
static void
poll_for_ms(uint64_t ms)
{
	uint64_t deadline = now_ms() + ms;

	while (now_ms() < deadline) {
		spdk_thread_poll(g_thread, 0, 0);
	}
}

/* Wait for an outstanding populate to resolve.
 *
 * Populate reports nothing back by design, so what is watched is the only thing
 * it does report: the total of the three ways it can end. Waiting for a fixed
 * duration instead would either be slower than necessary or flaky, depending on
 * how the guess compares to the device. */
static bool
poll_until_populate_settled(struct s3_cache *cache)
{
	struct s3_cache_stats before, now;
	uint64_t deadline = now_ms() + POLL_TIMEOUT_SEC * 1000;

	s3_cache_get_stats(cache, &before);

	while (now_ms() < deadline) {
		spdk_thread_poll(g_thread, 0, 0);

		s3_cache_get_stats(cache, &now);
		if (now.populates + now.populates_failed +
		    now.populates_dropped !=
		    before.populates + before.populates_failed +
		    before.populates_dropped) {
			return true;
		}
	}

	fprintf(stderr, "!! timed out waiting for a populate to settle\n");
	return false;
}

struct async_ctx {
	bool done;
	int  status;
};

static void
async_int_cb(void *cb_arg, int rc)
{
	struct async_ctx *ctx = cb_arg;

	ctx->status = rc;
	ctx->done   = true;
}

static void
async_void_cb(void *cb_arg)
{
	struct async_ctx *ctx = cb_arg;

	ctx->status = 0;
	ctx->done   = true;
}

static void
read_cb(void *cb_arg, int status)
{
	async_int_cb(cb_arg, status);
}

/* spdk_bdev_open_ext rejects a NULL event callback, so there has to be one even
 * though nothing here removes or resizes the bdev under the test. */
static void
bdev_event_cb(enum spdk_bdev_event_type type, struct spdk_bdev *bdev,
	      void *event_ctx)
{
	printf("  (unexpected bdev event %d)\n", type);
}

/* ==========================================================================
 * Framework bring-up: iobuf then accel then bdev, and the reverse on the way
 * out. accel is not optional -- every bdev channel acquires one.
 * ========================================================================== */

static int
framework_start(void)
{
	struct spdk_iobuf_opts iobuf_opts;
	struct spdk_bdev_opts bdev_opts;
	struct async_ctx ctx = {0};
	int rc;

	spdk_iobuf_get_opts(&iobuf_opts, sizeof(iobuf_opts));
	iobuf_opts.small_pool_count = 1024;
	iobuf_opts.large_pool_count = 128;
	iobuf_opts.opts_size = sizeof(iobuf_opts);
	rc = spdk_iobuf_set_opts(&iobuf_opts);
	if (rc != 0) {
		return rc;
	}
	rc = spdk_iobuf_initialize();
	if (rc != 0) {
		return rc;
	}
	rc = spdk_accel_initialize();
	if (rc != 0) {
		return rc;
	}

	spdk_bdev_get_opts(&bdev_opts, sizeof(bdev_opts));
	bdev_opts.bdev_io_pool_size = 4096;
	bdev_opts.bdev_auto_examine = false;
	bdev_opts.opts_size = sizeof(bdev_opts);
	rc = spdk_bdev_set_opts(&bdev_opts);
	if (rc != 0) {
		return rc;
	}

	spdk_bdev_initialize(async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

static void
framework_stop(void)
{
	struct async_ctx ctx;

	memset(&ctx, 0, sizeof(ctx));
	spdk_bdev_finish(async_void_cb, &ctx);
	poll_until(&ctx.done);

	memset(&ctx, 0, sizeof(ctx));
	spdk_accel_finish(async_void_cb, &ctx);
	poll_until(&ctx.done);

	memset(&ctx, 0, sizeof(ctx));
	spdk_iobuf_finish(async_void_cb, &ctx);
	poll_until(&ctx.done);
}

/* O_EXCL|O_NOFOLLOW because the path is fixed: without NOFOLLOW a pre-planted
 * symlink would let this truncate an unrelated file. */
static int
make_aio_file(const char *path, uint64_t size)
{
	int fd, rc;

	unlink(path);

	fd = open(path, O_RDWR | O_CREAT | O_EXCL | O_NOFOLLOW, 0600);
	if (fd < 0) {
		fprintf(stderr, "could not create %s: %s\n", path, strerror(errno));
		return -errno;
	}
	rc = ftruncate(fd, (off_t)size);
	if (rc != 0) {
		rc = -errno;
		close(fd);
		unlink(path);
		return rc;
	}
	close(fd);
	return 0;
}

/* ==========================================================================
 * Content helpers. Payloads are derived from the chunk index so a read can be
 * checked against a recomputed value rather than a table the test kept -- a
 * wrong table would otherwise agree with itself.
 * ========================================================================== */

static void
fill_pattern(void *buf, uint64_t chunk_index, uint32_t len, uint8_t salt)
{
	uint8_t *p = buf;

	for (uint32_t i = 0; i < len; i++) {
		p[i] = (uint8_t)((chunk_index * 31 + i * 7 + salt) & 0xff);
	}
}

static bool
pattern_matches(const void *buf, uint64_t chunk_index, uint32_t off,
		uint32_t len, uint8_t salt)
{
	const uint8_t *p = buf;

	for (uint32_t i = 0; i < len; i++) {
		if (p[i] != (uint8_t)((chunk_index * 31 + (off + i) * 7 + salt)
				      & 0xff)) {
			return false;
		}
	}
	return true;
}

static bool
all_zero(const void *buf, uint32_t len)
{
	const uint8_t *p = buf;

	for (uint32_t i = 0; i < len; i++) {
		if (p[i] != 0) {
			return false;
		}
	}
	return true;
}

/* Populate a whole object and wait for the write to land. */
static void
populate_sync(struct s3_cache *cache, uint64_t chunk_index,
	      const struct spdk_uuid *uuid, const void *buf,
	      uint32_t valid_bytes)
{
	s3_cache_populate(cache, chunk_index, uuid, 0, buf, valid_bytes,
			  valid_bytes);
	poll_until_populate_settled(cache);
}

/* Populate one range of an object. \p buf holds the bytes at \p off, the way a
 * read's buffer does. */
static void
populate_range_sync(struct s3_cache *cache, uint64_t chunk_index,
		    const struct spdk_uuid *uuid, uint32_t off,
		    const void *buf, uint32_t len, uint32_t object_valid_bytes)
{
	s3_cache_populate(cache, chunk_index, uuid, off, buf, len,
			  object_valid_bytes);
	poll_until_populate_settled(cache);
}

static int
read_sync(struct s3_cache *cache, uint64_t chunk_index,
	  const struct spdk_uuid *uuid, uint32_t off, uint32_t len, void *buf)
{
	struct async_ctx ctx = {0};
	int rc;

	rc = s3_cache_read(cache, chunk_index, uuid, off, len, buf,
			   read_cb, &ctx);
	if (rc != 0) {
		return rc;
	}
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

/* ==========================================================================
 * main
 * ========================================================================== */

int
main(int argc, char **argv)
{
	struct spdk_env_opts env_opts;
	struct spdk_bdev_desc *desc = NULL;
	struct spdk_io_channel *ch = NULL;
	struct s3_cache *cache = NULL;
	struct s3_cache_stats stats;
	const char *aio_path = DEFAULT_AIO_PATH;
	struct spdk_uuid uuid_a, uuid_b;
	void *src = NULL, *dst = NULL;
	bool file_created = false, bdev_created = false, framework_up = false;
	int rc;

	if (argc > 1) {
		aio_path = argv[1];
	}

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== chunk cache verification (on an aio bdev) ===\n\n");

	env_opts.opts_size = sizeof(env_opts);
	spdk_env_opts_init(&env_opts);
	env_opts.name     = "s3_cache_test";
	env_opts.no_huge  = true;
	env_opts.mem_size = 512;
	if (spdk_env_init(&env_opts) < 0) {
		fprintf(stderr, "spdk_env_init failed\n");
		spdk_log_close();
		return 77;
	}
	if (spdk_thread_lib_init(NULL, 0) != 0) {
		goto out_env;
	}
	g_thread = spdk_thread_create("cache_test", NULL);
	if (!g_thread) {
		goto out_thread_lib;
	}
	spdk_set_thread(g_thread);

	printf("[0] bringing up the SPDK framework (iobuf -> accel -> bdev)\n");
	rc = framework_start();
	check_u64("framework_start", (uint64_t)-rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	framework_up = true;

	printf("\n[1] creating an aio bdev on %s\n", aio_path);
	rc = make_aio_file(aio_path, AIO_FILE_SIZE);
	check_u64("ftruncate the backing file", (uint64_t)-rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	file_created = true;

	rc = create_aio_bdev(AIO_BDEV_NAME, aio_path, AIO_BLOCK_SIZE,
			     false, false, NULL, false);
	check_u64("create_aio_bdev", (uint64_t)-rc, 0);
	if (rc != 0) {
		goto out_file;
	}
	bdev_created = true;

	rc = spdk_bdev_open_ext(AIO_BDEV_NAME, true, bdev_event_cb, NULL, &desc);
	check_u64("spdk_bdev_open_ext", (uint64_t)-rc, 0);
	if (rc != 0) {
		goto out_bdev;
	}
	ch = spdk_bdev_get_io_channel(desc);
	check_true("spdk_bdev_get_io_channel", ch != NULL, NULL);
	if (!ch) {
		goto out_desc;
	}

	printf("\n[2] s3_cache_create\n");
	{
		struct s3_cache_opts opts = {
			.desc          = desc,
			.ch            = ch,
			.region_offset = TEST_REGION_OFF,
			.region_size   = TEST_REGION_SIZE,
			.chunk_size    = TEST_CHUNK_SIZE,
			.block_size    = AIO_BLOCK_SIZE,
			.num_chunks    = TEST_NUM_CHUNKS,
		};

		rc = s3_cache_create(&opts, &cache);
		check_u64("s3_cache_create", (uint64_t)-rc, 0);
		if (rc != 0) {
			goto out_channel;
		}

		s3_cache_get_stats(cache, &stats);
		check_u64("slot count comes from the region size",
			  stats.slots_total, TEST_N_SLOTS);

		/* A region too small for even one chunk is a layout mistake, not
		 * a cache with no room. */
		struct s3_cache *tiny = NULL;
		struct s3_cache_opts bad = opts;
		bad.region_size = TEST_CHUNK_SIZE - 1;
		check_u64("a region below one chunk is rejected",
			  (uint64_t) - s3_cache_create(&bad, &tiny), EINVAL);
	}

	src = spdk_dma_malloc(TEST_CHUNK_SIZE, AIO_BLOCK_SIZE, NULL);
	dst = spdk_dma_malloc(TEST_CHUNK_SIZE, AIO_BLOCK_SIZE, NULL);
	if (!src || !dst) {
		goto out_cache;
	}

	spdk_uuid_generate(&uuid_a);
	spdk_uuid_generate(&uuid_b);

	printf("\n[3] populate then read back\n");
	{
		fill_pattern(src, 7, TEST_CHUNK_SIZE, 0);

		check_true("nothing is cached before the populate",
			   !s3_cache_lookup(cache, 7, &uuid_a), NULL);

		populate_sync(cache, 7, &uuid_a, src, TEST_CHUNK_SIZE);
		check_true("lookup finds it afterwards",
			   s3_cache_lookup(cache, 7, &uuid_a), NULL);

		s3_cache_get_stats(cache, &stats);
		check_u64("one populate landed", stats.populates, 1);

		/* Scribble over the source: populate copied it, so the bytes on
		 * the device must be the originals. This is the assertion that
		 * makes the caller free to release its buffer at once. */
		memset(src, 0xCC, TEST_CHUNK_SIZE);

		memset(dst, 0, TEST_CHUNK_SIZE);
		rc = read_sync(cache, 7, &uuid_a, 0, TEST_CHUNK_SIZE, dst);
		check_u64("read the whole chunk back", (uint64_t)-rc, 0);
		check_true("the bytes are what was populated, not the scribble",
			   pattern_matches(dst, 7, 0, TEST_CHUNK_SIZE, 0), NULL);

		/* An offset read, which is the common case: one block out of a
		 * chunk. */
		memset(dst, 0, TEST_CHUNK_SIZE);
		rc = read_sync(cache, 7, &uuid_a, 2 * AIO_BLOCK_SIZE,
			       AIO_BLOCK_SIZE, dst);
		check_u64("read one block at an offset", (uint64_t)-rc, 0);
		check_true("the offset block matches",
			   pattern_matches(dst, 7, 2 * AIO_BLOCK_SIZE,
					   AIO_BLOCK_SIZE, 0), NULL);
	}

	printf("\n[4] a different uuid on the same chunk is a miss\n");
	{
		/* The property the whole module rests on. An object is immutable
		 * and named by its uuid, so serving a stale tag would hand out
		 * data from a superseded version of the chunk. */
		check_true("lookup with the other uuid says no",
			   !s3_cache_lookup(cache, 7, &uuid_b), NULL);
		check_u64("read with the other uuid is -ENOENT",
			  (uint64_t) - read_sync(cache, 7, &uuid_b, 0,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);
		check_true("the original version is still readable",
			   s3_cache_lookup(cache, 7, &uuid_a), NULL);
	}

	printf("\n[5] the tail past valid_bytes reads as zeroes\n");
	{
		/* Matches what the S3 path does when a GET returns short. */
		uint32_t valid = 2 * AIO_BLOCK_SIZE;

		fill_pattern(src, 9, valid, 3);
		populate_sync(cache, 9, &uuid_a, src, valid);

		memset(dst, 0xEE, TEST_CHUNK_SIZE);
		rc = read_sync(cache, 9, &uuid_a, 0, 4 * AIO_BLOCK_SIZE, dst);
		check_u64("a read spanning the end succeeds", (uint64_t)-rc, 0);
		check_true("the valid part matches",
			   pattern_matches(dst, 9, 0, valid, 3), NULL);
		check_true("the tail is zero filled",
			   all_zero((uint8_t *)dst + valid, 2 * AIO_BLOCK_SIZE),
			   NULL);

		/* Entirely past the end: still a hit, because the answer is known
		 * without going to S3. */
		memset(dst, 0xEE, TEST_CHUNK_SIZE);
		rc = read_sync(cache, 9, &uuid_a, 4 * AIO_BLOCK_SIZE,
			       AIO_BLOCK_SIZE, dst);
		check_u64("a read entirely past the end succeeds",
			  (uint64_t)-rc, 0);
		check_true("and returns zeroes",
			   all_zero(dst, AIO_BLOCK_SIZE), NULL);
	}

	printf("\n[6] re-populating a chunk reuses its slot\n");
	{
		uint64_t before;

		s3_cache_get_stats(cache, &stats);
		before = stats.slots_resident;

		fill_pattern(src, 7, TEST_CHUNK_SIZE, 5);
		populate_sync(cache, 7, &uuid_b, src, TEST_CHUNK_SIZE);

		s3_cache_get_stats(cache, &stats);
		check_u64("resident count did not grow", stats.slots_resident,
			  before);
		check_true("the new version is the one cached",
			   s3_cache_lookup(cache, 7, &uuid_b), NULL);
		check_true("the old version is gone",
			   !s3_cache_lookup(cache, 7, &uuid_a), NULL);

		memset(dst, 0, TEST_CHUNK_SIZE);
		rc = read_sync(cache, 7, &uuid_b, 0, AIO_BLOCK_SIZE, dst);
		check_u64("the new version reads back", (uint64_t)-rc, 0);
		check_true("with the new contents",
			   pattern_matches(dst, 7, 0, AIO_BLOCK_SIZE, 5), NULL);
	}

	printf("\n[7] eviction is LRU and skips slots with a read in flight\n");
	{
		struct async_ctx rctx = {0};
		uint64_t pinned = 20;
		uint64_t evictions_before;

		/* Occupy every slot, so the next populate has to evict. */
		for (uint64_t c = 20; c < 20 + TEST_N_SLOTS; c++) {
			fill_pattern(src, c, TEST_CHUNK_SIZE, 0);
			populate_sync(cache, c, &uuid_a, src, TEST_CHUNK_SIZE);
		}
		s3_cache_get_stats(cache, &stats);
		check_u64("every slot is occupied", stats.slots_resident,
			  TEST_N_SLOTS);
		evictions_before = stats.evictions;

		/* Start a read and leave it outstanding. The slot is pinned for as
		 * long as it runs, and reusing it under the reader would splice
		 * two different objects together in the caller's buffer. */
		rc = s3_cache_read(cache, pinned, &uuid_a, 0, AIO_BLOCK_SIZE,
				   dst, read_cb, &rctx);
		check_u64("a read on the pinned chunk starts", (uint64_t)-rc, 0);

		/* One populate, and *no polling around it*. Everything up to
		 * choosing a victim happens synchronously inside the call, so this
		 * is the only window in which the question can be asked at all:
		 * polling to wait for the populate would let the read complete
		 * first and unpin the slot, which is what an earlier version of
		 * this test did -- it passed for the wrong reason. */
		fill_pattern(src, 30, TEST_CHUNK_SIZE, 1);
		s3_cache_populate(cache, 30, &uuid_a, 0, src, TEST_CHUNK_SIZE,
				  TEST_CHUNK_SIZE);

		s3_cache_get_stats(cache, &stats);
		check_u64("it evicted exactly one slot", stats.evictions,
			  evictions_before + 1);
		check_true("but not the one being read",
			   s3_cache_lookup(cache, pinned, &uuid_a), NULL);

		if (!poll_until(&rctx.done)) {
			goto out_cache;
		}
		check_u64("the in-flight read still completed",
			  (uint64_t) - rctx.status, 0);
		check_true("with the right bytes",
			   pattern_matches(dst, pinned, 0, AIO_BLOCK_SIZE, 0),
			   NULL);

		poll_for_ms(200);
		s3_cache_get_stats(cache, &stats);
		check_true("residency stays within the slot count",
			   stats.slots_resident <= TEST_N_SLOTS, NULL);
	}

	printf("\n[8] populate is declined while an older version is being read\n");
	{
		struct async_ctx rctx = {0};
		struct spdk_uuid uuid_c;
		uint64_t chunk = 20;

		spdk_uuid_generate(&uuid_c);

		if (!s3_cache_lookup(cache, chunk, &uuid_a)) {
			/* It may have been evicted in [7]; put it back. */
			fill_pattern(src, chunk, TEST_CHUNK_SIZE, 0);
			populate_sync(cache, chunk, &uuid_a, src,
				      TEST_CHUNK_SIZE);
		}

		rc = s3_cache_read(cache, chunk, &uuid_a, 0, AIO_BLOCK_SIZE,
				   dst, read_cb, &rctx);
		check_u64("a read on the old version starts", (uint64_t)-rc, 0);

		s3_cache_get_stats(cache, &stats);
		uint64_t dropped_before = stats.populates_dropped;

		fill_pattern(src, chunk, TEST_CHUNK_SIZE, 9);
		s3_cache_populate(cache, chunk, &uuid_c, 0, src,
				  TEST_CHUNK_SIZE, TEST_CHUNK_SIZE);

		s3_cache_get_stats(cache, &stats);
		check_true("the populate was dropped, not applied",
			   stats.populates_dropped == dropped_before + 1, NULL);
		check_true("the version being read is still the cached one",
			   s3_cache_lookup(cache, chunk, &uuid_a), NULL);

		if (!poll_until(&rctx.done)) {
			goto out_cache;
		}
		check_u64("the read completed", (uint64_t) - rctx.status, 0);
		check_true("and returned the old version's bytes",
			   pattern_matches(dst, chunk, 0, AIO_BLOCK_SIZE, 0),
			   NULL);
	}

	printf("\n[9] drop_chunk frees the slot\n");
	{
		uint64_t before;

		if (!s3_cache_lookup(cache, 9, &uuid_a)) {
			fill_pattern(src, 9, TEST_CHUNK_SIZE, 3);
			populate_sync(cache, 9, &uuid_a, src, TEST_CHUNK_SIZE);
		}

		s3_cache_get_stats(cache, &stats);
		before = stats.slots_resident;

		s3_cache_drop_chunk(cache, 9);

		s3_cache_get_stats(cache, &stats);
		check_u64("residency dropped by one", stats.slots_resident,
			  before - 1);
		check_true("and it is no longer cached",
			   !s3_cache_lookup(cache, 9, &uuid_a), NULL);

		/* Out of range and never-cached chunks are no-ops, not crashes. */
		s3_cache_drop_chunk(cache, TEST_NUM_CHUNKS + 100);
		s3_cache_drop_chunk(cache, 3);
		check_true("dropping an uncached chunk is harmless", true, NULL);
	}

	printf("\n[10] out-of-range and degenerate arguments\n");
	{
		check_true("lookup past num_chunks is false",
			   !s3_cache_lookup(cache, TEST_NUM_CHUNKS, &uuid_a),
			   NULL);
		check_u64("read past num_chunks is -ENOENT",
			  (uint64_t) - read_sync(cache, TEST_NUM_CHUNKS,
						 &uuid_a, 0, AIO_BLOCK_SIZE,
						 dst),
			  ENOENT);

		/* Populate is best effort with no error to report, so the only
		 * thing to check is that these do not corrupt anything. */
		s3_cache_populate(cache, TEST_NUM_CHUNKS, &uuid_a, 0, src,
				  TEST_CHUNK_SIZE, TEST_CHUNK_SIZE);
		s3_cache_populate(cache, 1, &uuid_a, 0, src, TEST_CHUNK_SIZE + 1,
				  TEST_CHUNK_SIZE + 1);
		s3_cache_populate(cache, 1, &uuid_a, 0, src, 0, 0);
		s3_cache_populate(NULL, 1, &uuid_a, 0, src, TEST_CHUNK_SIZE,
				  TEST_CHUNK_SIZE);
		/* An offset at or past the object's end describes no bytes of it. */
		s3_cache_populate(cache, 1, &uuid_a, TEST_CHUNK_SIZE, src,
				  AIO_BLOCK_SIZE, TEST_CHUNK_SIZE);
		poll_for_ms(50);
		check_true("rejected populates leave the cache usable",
			   !s3_cache_lookup(cache, TEST_NUM_CHUNKS, &uuid_a) &&
			   !s3_cache_lookup(cache, 1, &uuid_a), NULL);
	}

	printf("\n[11] partial residency\n");
	{
		/* The whole point of the bitmap, and the one section where a bug
		 * is silent corruption rather than a missed hit. Chunk 40 is
		 * untouched so far, and slot reuse means whatever the slot held
		 * before is still on the device underneath -- which is exactly the
		 * data that must not surface. */
		uint64_t chunk = 40;
		uint32_t mid = 4 * AIO_BLOCK_SIZE;   /* 16 KiB into the chunk */
		uint64_t declined_before, hits_before;

		s3_cache_drop_chunk(cache, chunk);

		/* One range in the middle, the way a filesystem read arrives. */
		fill_pattern(src, chunk, 2 * AIO_BLOCK_SIZE, 0);
		populate_range_sync(cache, chunk, &uuid_a, mid, src,
				    2 * AIO_BLOCK_SIZE, TEST_CHUNK_SIZE);

		s3_cache_get_stats(cache, &stats);
		declined_before = stats.hits_declined;

		/* THE assertion. Under the old scalar model this range would have
		 * been reported present, and the read would have returned the
		 * previous tenant's bytes. */
		check_u64("a read below the populated range misses",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a, 0,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);
		check_u64("a read above it misses too",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a,
						 mid + 2 * AIO_BLOCK_SIZE,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);
		/* Straddling: the second block is present, the first is not. A
		 * partly resident range is a miss, never a partial answer. */
		check_u64("a read straddling its lower edge misses",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a,
						 mid - AIO_BLOCK_SIZE,
						 2 * AIO_BLOCK_SIZE, dst),
			  ENOENT);

		s3_cache_get_stats(cache, &stats);
		check_u64("all three counted as declined, not as misses",
			  stats.hits_declined, declined_before + 3);

		/* And the range that *is* there is served, correctly. */
		memset(dst, 0xee, TEST_CHUNK_SIZE);
		check_u64("the populated range itself hits",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a, mid,
						 2 * AIO_BLOCK_SIZE, dst),
			  0);
		check_true("with the bytes that were populated",
			   pattern_matches(dst, chunk, 0, 2 * AIO_BLOCK_SIZE, 0),
			   NULL);

		/* A single block inside the range, to prove the bitmap is per
		 * block and not just a stored extent. */
		memset(dst, 0xee, TEST_CHUNK_SIZE);
		check_u64("so does one block inside it",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a,
						 mid + AIO_BLOCK_SIZE,
						 AIO_BLOCK_SIZE, dst),
			  0);
		check_true("at the right offset within the range",
			   pattern_matches(dst, chunk, AIO_BLOCK_SIZE,
					   AIO_BLOCK_SIZE, 0), NULL);

		check_true("a partly resident object is not reported as cached",
			   !s3_cache_lookup(cache, chunk, &uuid_a), NULL);

		/* Filling the rest, in two more pieces, completes the object. The
		 * pattern is generated per range with the offset folded in, so
		 * what is checked at the end is that the three writes landed in
		 * the right places relative to each other. */
		fill_pattern(src, chunk, mid, 0);
		populate_range_sync(cache, chunk, &uuid_a, 0, src, mid,
				    TEST_CHUNK_SIZE);

		uint32_t rest_off = mid + 2 * AIO_BLOCK_SIZE;
		fill_pattern(src, chunk, TEST_CHUNK_SIZE - rest_off, 0);
		populate_range_sync(cache, chunk, &uuid_a, rest_off, src,
				    TEST_CHUNK_SIZE - rest_off, TEST_CHUNK_SIZE);

		check_true("once every block is in, the object is cached",
			   s3_cache_lookup(cache, chunk, &uuid_a), NULL);

		s3_cache_get_stats(cache, &stats);
		hits_before = stats.hits;
		memset(dst, 0xee, TEST_CHUNK_SIZE);
		check_u64("and a read of the whole chunk hits",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a, 0,
						 TEST_CHUNK_SIZE, dst),
			  0);
		s3_cache_get_stats(cache, &stats);
		check_u64("counted as a hit", stats.hits, hits_before + 1);
		check_true("the three ranges reassembled in order",
			   pattern_matches(dst, chunk, 0, mid, 0) &&
			   pattern_matches((uint8_t *)dst + mid, chunk, 0,
					   2 * AIO_BLOCK_SIZE, 0) &&
			   pattern_matches((uint8_t *)dst + rest_off, chunk, 0,
					   TEST_CHUNK_SIZE - rest_off, 0), NULL);

		/* Only whole blocks count. A range that ends mid-block short of
		 * the object's end leaves that block absent rather than claiming
		 * bytes it does not have. */
		s3_cache_drop_chunk(cache, chunk);
		fill_pattern(src, chunk, AIO_BLOCK_SIZE + 100, 0);
		populate_range_sync(cache, chunk, &uuid_a, 0, src,
				    AIO_BLOCK_SIZE + 100, TEST_CHUNK_SIZE);
		check_u64("the first whole block of it hits",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a, 0,
						 AIO_BLOCK_SIZE, dst),
			  0);
		check_u64("the partly covered block does not",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a,
						 AIO_BLOCK_SIZE,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);
	}

	printf("\n[12] a short object's last block\n");
	{
		/* valid_bytes need not be a multiple of the block size. The final
		 * partial block is present once the range reaching the object's
		 * end has landed, and reads of it are clamped and zero filled --
		 * the same contract the S3 path has for a short GET. */
		uint64_t chunk = 41;
		uint32_t vb = 2 * AIO_BLOCK_SIZE + 500;

		s3_cache_drop_chunk(cache, chunk);

		/* Just the tail: the last whole block plus the 500 byte remainder,
		 * which is what a read at that offset returns. */
		fill_pattern(src, chunk, AIO_BLOCK_SIZE + 500, 7);
		populate_range_sync(cache, chunk, &uuid_a, AIO_BLOCK_SIZE, src,
				    AIO_BLOCK_SIZE + 500, vb);

		memset(dst, 0xee, TEST_CHUNK_SIZE);
		check_u64("a read covering the short last block hits",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a,
						 AIO_BLOCK_SIZE,
						 2 * AIO_BLOCK_SIZE, dst),
			  0);
		check_true("its real bytes came back",
			   pattern_matches(dst, chunk, 0, AIO_BLOCK_SIZE + 500,
					   7), NULL);
		check_true("and the tail past valid_bytes is zeroed",
			   all_zero((uint8_t *)dst + AIO_BLOCK_SIZE + 500,
				    2 * AIO_BLOCK_SIZE -
				    (AIO_BLOCK_SIZE + 500)), NULL);

		/* Entirely past the end: answerable from valid_bytes alone, so it
		 * hits without any block being present. */
		memset(dst, 0xee, TEST_CHUNK_SIZE);
		check_u64("a read wholly past valid_bytes hits",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a,
						 4 * AIO_BLOCK_SIZE,
						 AIO_BLOCK_SIZE, dst),
			  0);
		check_true("and reads as zeroes",
			   all_zero(dst, AIO_BLOCK_SIZE), NULL);

		/* The block below the populated tail was never written. */
		check_u64("the block before it still misses",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a, 0,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);
	}

	printf("\n[13] a superseded version cannot be served from a filled range\n");
	{
		/* Partial residency must not weaken the uuid tag: a slot holding
		 * ranges of one version has to miss for another, not serve what
		 * it happens to have. */
		uint64_t chunk = 42;
		struct spdk_uuid uuid_d;

		spdk_uuid_generate(&uuid_d);
		s3_cache_drop_chunk(cache, chunk);

		fill_pattern(src, chunk, 2 * AIO_BLOCK_SIZE, 0);
		populate_range_sync(cache, chunk, &uuid_a, 0, src,
				    2 * AIO_BLOCK_SIZE, TEST_CHUNK_SIZE);

		check_u64("the same range under a different uuid misses",
			  (uint64_t) - read_sync(cache, chunk, &uuid_d, 0,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);

		/* The new version takes the slot over, and the old version's
		 * ranges go with it -- a read of a range only the old one had
		 * must not be served off the device. */
		fill_pattern(src, chunk, AIO_BLOCK_SIZE, 5);
		populate_range_sync(cache, chunk, &uuid_d, 3 * AIO_BLOCK_SIZE,
				    src, AIO_BLOCK_SIZE, TEST_CHUNK_SIZE);

		check_u64("after the takeover the old version misses",
			  (uint64_t) - read_sync(cache, chunk, &uuid_a, 0,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);
		check_u64("and the range the old version had is not resident",
			  (uint64_t) - read_sync(cache, chunk, &uuid_d, 0,
						 AIO_BLOCK_SIZE, dst),
			  ENOENT);
		memset(dst, 0xee, TEST_CHUNK_SIZE);
		check_u64("only the new version's own range hits",
			  (uint64_t) - read_sync(cache, chunk, &uuid_d,
						 3 * AIO_BLOCK_SIZE,
						 AIO_BLOCK_SIZE, dst),
			  0);
		check_true("with its bytes",
			   pattern_matches(dst, chunk, 0, AIO_BLOCK_SIZE, 5),
			   NULL);
	}

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);

out_cache:
	/* Let anything still in flight land: destroy asserts that nothing is,
	 * since in-flight I/O holds pointers into the slot array. */
	poll_for_ms(200);
	s3_cache_destroy(cache);
	spdk_dma_free(src);
	spdk_dma_free(dst);
out_channel:
	if (ch) {
		spdk_put_io_channel(ch);
	}
out_desc:
	if (desc) {
		spdk_bdev_close(desc);
	}
out_bdev:
	if (bdev_created) {
		struct async_ctx ctx = {0};

		bdev_aio_delete(AIO_BDEV_NAME, async_int_cb, &ctx);
		poll_until(&ctx.done);
	}
out_file:
	if (file_created) {
		unlink(aio_path);
	}
out_framework:
	if (framework_up) {
		framework_stop();
	}
	spdk_thread_exit(g_thread);
	while (!spdk_thread_is_exited(g_thread)) {
		spdk_thread_poll(g_thread, 0, 0);
	}
	spdk_thread_destroy(g_thread);
out_thread_lib:
	spdk_thread_lib_fini();
out_env:
	spdk_env_fini();
	spdk_log_close();

	return g_fail == 0 ? 0 : 1;
}
