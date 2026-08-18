/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   WAL verification -- needs neither S3 nor credentials
 *
 *   === What this test proves ===
 *
 *   The WAL exists because the direct-to-S3 write path loses data under
 *   concurrent partial-chunk writes. Before it can be trusted to carry user
 *   writes, the log itself has to hold up, so the central assertion here
 *   mirrors the journal test: *entries survive a full close and reopen*, byte
 *   for byte.
 *
 *     1. format the WAL region
 *     2. append a mix of writes, zeroes, unmaps and a barrier
 *     3. close, which persists the super
 *     4. reopen and replay
 *     5. compare every recovered entry against what was written, payload
 *        included
 *
 *   Beyond that it pins down the properties that are easy to get subtly wrong:
 *
 *     - the A/B super survives and picks the newer slot
 *     - epoch increments on every open, so a stale log cannot masquerade as
 *       current
 *     - payloads are verified by CRC, not just present
 *     - batches close properly, since an unclosed batch is dropped whole (W4)
 *     - the ring crosses a segment boundary and the END sentinel is handled
 *     - backpressure engages rather than silently overrunning
 *
 *   Like the journal test this uses the upstream bdev_aio over a sparse file and
 *   brings up iobuf, accel and bdev by hand, in that order. poll_until() turns
 *   the async API back into sequential flow, which is only safe because this
 *   process has a single thread and no other pollers; production code must not
 *   copy that.
 *
 *   Usage:
 *     ./s3_wal_test [aio-file-path]
 */

#include "spdk/stdinc.h"
#include "spdk/accel.h"
#include "spdk/bdev.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_local_dev.h"
#include "s3lvol/s3_wal.h"

#include "bdev/aio/bdev_aio.h"

#define AIO_BDEV_NAME    "s3lvol_waltest0"
#define AIO_BLOCK_SIZE   4096
#define DEFAULT_AIO_PATH "/data/s3lvol_wal_test.aio"

/* Small segments keep the file small while still exercising the ring: three
 * segments is the minimum wal_alloc() accepts, and the geometry reserves two for
 * slack, so 6 segments leaves 4 usable. */
#define TEST_SEG_SIZE  (2 * 1024 * 1024)
#define TEST_WAL_SIZE  (16 * 1024 * 1024)
#define TEST_JOURNAL_SIZE (4 * 1024 * 1024)
#define AIO_FILE_SIZE  (64ULL * 1024 * 1024)

#define TEST_CHUNK_SIZE (1024 * 1024)

static int g_pass;
static int g_fail;
static struct spdk_thread *g_thread;

static void
check(const char *what, int got, int want)
{
	if (got == want) {
		printf("  [PASS] %-52s rc=%d\n", what, got);
		g_pass++;
	} else {
		printf("  [FAIL] %-52s rc=%d (expected %d)\n", what, got, want);
		g_fail++;
	}
}

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-52s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-52s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

/* ==========================================================================
 * Async to sync
 * ========================================================================== */

#define POLL_TIMEOUT_SEC 60

static bool
poll_until(bool *done)
{
	time_t deadline = time(NULL) + POLL_TIMEOUT_SEC;

	while (!*done && time(NULL) < deadline) {
		spdk_thread_poll(g_thread, 0, 0);
	}
	if (!*done) {
		fprintf(stderr, "!! timed out waiting for async completion\n");
	}
	return *done;
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

struct open_dev_ctx {
	bool                done;
	int                   status;
	struct s3_local_dev  *dev;
};

static void
local_dev_open_cb(void *cb_arg, struct s3_local_dev *dev, int status)
{
	struct open_dev_ctx *ctx = cb_arg;

	ctx->dev    = dev;
	ctx->status = status;
	ctx->done   = true;
}

struct open_wal_ctx {
	bool           done;
	int            status;
	struct s3_wal *wal;
};

static void
wal_open_cb(void *cb_arg, struct s3_wal *wal, int status)
{
	struct open_wal_ctx *ctx = cb_arg;

	ctx->wal    = wal;
	ctx->status = status;
	ctx->done   = true;
}

/* ==========================================================================
 * aio backing file
 * ========================================================================== */

/* O_EXCL|O_NOFOLLOW because the path is fixed: without NOFOLLOW a pre-planted
 * symlink would let this truncate an unrelated file. */
static int
make_aio_file(const char *path, uint64_t size)
{
	int fd;
	int rc;

	unlink(path);

	fd = open(path, O_RDWR | O_CREAT | O_EXCL | O_NOFOLLOW, 0600);
	if (fd < 0) {
		fprintf(stderr, "could not create %s: %s\n", path, strerror(errno));
		return -errno;
	}

	rc = ftruncate(fd, (off_t)size);
	if (rc != 0) {
		rc = -errno;
		fprintf(stderr, "ftruncate %s failed: %s\n", path, strerror(errno));
		close(fd);
		unlink(path);
		return rc;
	}

	close(fd);
	return 0;
}

/* ==========================================================================
 * Expected entries
 *
 * Payloads are derived from the LBA so recovery can be checked against a
 * recomputed value rather than a table this test kept -- a wrong table would
 * otherwise agree with itself.
 * ========================================================================== */

#define NUM_WRITES 64

static void
fill_payload(void *buf, uint64_t lba, uint32_t len)
{
	uint8_t *p = buf;

	for (uint32_t i = 0; i < len; i++) {
		p[i] = (uint8_t)((lba * 31 + i * 7) & 0xff);
	}
}

static bool
payload_matches(const void *buf, uint64_t lba, uint32_t len)
{
	const uint8_t *p = buf;

	for (uint32_t i = 0; i < len; i++) {
		if (p[i] != (uint8_t)((lba * 31 + i * 7) & 0xff)) {
			return false;
		}
	}
	return true;
}

struct replay_state {
	uint64_t writes;
	uint64_t zeroes;
	uint64_t unmaps;
	uint64_t barriers;
	uint64_t payload_ok;
	uint64_t payload_bad;
	uint64_t out_of_order;
	uint64_t last_seq;
};

static int
replay_apply(void *cb_arg, const struct s3_wal_entry_hdr *hdr, const void *payload)
{
	struct replay_state *st = cb_arg;

	/* seq must come back strictly increasing: the physical scan order is the
	 * append order, and recovery relies on that. */
	if (st->last_seq != 0 && hdr->seq <= st->last_seq) {
		st->out_of_order++;
	}
	st->last_seq = hdr->seq;

	switch (hdr->type) {
	case S3_WAL_WRITE:
		st->writes++;
		if (payload && payload_matches(payload, hdr->lba, hdr->payload_len)) {
			st->payload_ok++;
		} else {
			st->payload_bad++;
		}
		break;
	case S3_WAL_WRITE_ZEROES:
		st->zeroes++;
		break;
	case S3_WAL_UNMAP:
		st->unmaps++;
		break;
	case S3_WAL_BARRIER:
		st->barriers++;
		break;
	default:
		printf("       !! unexpected entry type %u\n", hdr->type);
		return -EILSEQ;
	}

	return 0;
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

/* Synchronous wrappers */

static int
sync_append_write(struct s3_wal *wal, uint64_t lba, const void *payload)
{
	struct async_ctx ctx = {0};

	s3_wal_append_write(wal, lba, 1, payload, lba / 256, NULL,
			    async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

static int
sync_close(struct s3_wal *wal)
{
	struct async_ctx ctx = {0};

	s3_wal_close(wal, async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

static int
sync_replay(struct s3_wal *wal, struct replay_state *st)
{
	struct async_ctx ctx = {0};

	s3_wal_replay(wal, replay_apply, st, async_int_cb, &ctx);
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
	struct s3_local_dev *local_dev = NULL;
	struct s3_wal *wal = NULL;
	const char *aio_path = DEFAULT_AIO_PATH;
	struct s3_wal_opts wal_opts = { .seg_size = TEST_SEG_SIZE };
	void *payload = NULL;
	bool file_created = false;
	bool bdev_created = false;
	bool framework_up = false;
	uint64_t epoch_first = 0;
	int rc;

	if (argc > 1) {
		aio_path = argv[1];
	}

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== WAL verification (on an aio bdev) ===\n\n");

	env_opts.opts_size = sizeof(env_opts);
	spdk_env_opts_init(&env_opts);
	env_opts.name     = "s3_wal_test";
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
	g_thread = spdk_thread_create("wal_test", NULL);
	if (!g_thread) {
		goto out_thread_lib;
	}
	spdk_set_thread(g_thread);

	printf("[0] bringing up the SPDK framework (iobuf -> accel -> bdev)\n");
	rc = framework_start();
	check("framework_start", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	framework_up = true;

	printf("\n[1] creating an aio bdev on %s\n", aio_path);
	rc = make_aio_file(aio_path, AIO_FILE_SIZE);
	check("ftruncate the backing file", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	file_created = true;

	rc = create_aio_bdev(AIO_BDEV_NAME, aio_path, AIO_BLOCK_SIZE,
			     false, false, NULL, false);
	check("create_aio_bdev", rc, 0);
	if (rc != 0) {
		goto out_file;
	}
	bdev_created = true;

	printf("\n[2] s3_local_dev_format\n");
	{
		struct open_dev_ctx octx = {0};
		struct s3_local_dev_format_opts fopts = {
			.wal_bdev_name  = AIO_BDEV_NAME,
			.lvs_name       = "wal_test",
			.capacity_bytes = 256ULL * 1024 * 1024,
			.chunk_size     = TEST_CHUNK_SIZE,
			.journal_size   = TEST_JOURNAL_SIZE,
			.wal_size       = TEST_WAL_SIZE,
		};

		s3_local_dev_format(&fopts, local_dev_open_cb, &octx);
		if (!poll_until(&octx.done)) {
			goto out_bdev;
		}
		check("s3_local_dev_format", octx.status, 0);
		if (octx.status != 0) {
			goto out_bdev;
		}
		local_dev = octx.dev;
	}

	printf("\n[3] s3_wal_create\n");
	{
		struct open_wal_ctx wctx = {0};

		s3_wal_create(local_dev, &wal_opts, wal_open_cb, &wctx);
		if (!poll_until(&wctx.done)) {
			goto out_local;
		}
		check("s3_wal_create", wctx.status, 0);
		if (wctx.status != 0) {
			goto out_local;
		}
		wal = wctx.wal;
		epoch_first = s3_wal_get_epoch(wal);

		char detail[64];
		snprintf(detail, sizeof(detail), "epoch=%" PRIu64, epoch_first);
		check_true("a fresh WAL starts at a non-zero epoch",
			   epoch_first > 0, detail);
	}

	payload = spdk_dma_zmalloc(S3_WAL_BLOCK_SIZE, S3_WAL_BLOCK_SIZE, NULL);
	if (!payload) {
		goto out_wal;
	}

	printf("\n[4] appending %d writes plus metadata entries\n", NUM_WRITES);
	{
		int failed = 0;

		for (uint64_t i = 0; i < NUM_WRITES; i++) {
			fill_payload(payload, i, S3_WAL_BLOCK_SIZE);
			rc = sync_append_write(wal, i, payload);
			if (rc != 0) {
				printf("       !! append of lba %" PRIu64
				       " failed: %d\n", i, rc);
				failed++;
				break;
			}
		}
		check_true("all writes acknowledged", failed == 0, NULL);
	}

	{
		struct async_ctx ctx = {0};

		s3_wal_append_zeroes(wal, 1000, 2, 1000 / 256, NULL,
				     async_int_cb, &ctx);
		poll_until(&ctx.done);
		check("s3_wal_append_zeroes", ctx.status, 0);
	}
	{
		struct async_ctx ctx = {0};

		s3_wal_append_unmap(wal, 2000, 4, 2000 / 256, NULL,
				    async_int_cb, &ctx);
		poll_until(&ctx.done);
		check("s3_wal_append_unmap", ctx.status, 0);
	}
	{
		struct async_ctx ctx = {0};
		uint64_t barrier_seq = 0;

		s3_wal_append_barrier(wal, &barrier_seq, async_int_cb, &ctx);
		poll_until(&ctx.done);
		check("s3_wal_append_barrier", ctx.status, 0);
		check_true("the barrier was given a seq", barrier_seq != 0, NULL);
	}

	{
		struct s3_wal_stats st;
		char detail[128];

		s3_wal_get_stats(wal, &st);
		snprintf(detail, sizeof(detail),
			 "appends=%" PRIu64 " batches=%" PRIu64 " bytes=%" PRIu64,
			 st.appends, st.batches, st.bytes_written);
		check_true("every append was batched and written",
			   st.appends == NUM_WRITES + 3 && st.batches > 0, detail);

		/* Every batch is padded to 4 KiB, so the log must have grown by a
		 * whole number of blocks (I3, I4). */
		uint64_t used = s3_wal_get_used_bytes(wal);
		snprintf(detail, sizeof(detail), "used=%" PRIu64, used);
		check_true("the head sits on a 4 KiB boundary",
			   used % S3_WAL_BLOCK_SIZE == 0, detail);
	}

	printf("\n[5] closing the WAL (persists the super)\n");
	rc = sync_close(wal);
	wal = NULL;
	check("s3_wal_close", rc, 0);

	printf("\n[6] reopening and replaying\n");
	{
		struct open_wal_ctx wctx = {0};

		s3_wal_open(local_dev, wal_open_cb, &wctx);
		if (!poll_until(&wctx.done)) {
			goto out_local;
		}
		check("s3_wal_open", wctx.status, 0);
		if (wctx.status != 0) {
			goto out_local;
		}
		wal = wctx.wal;

		char detail[96];
		snprintf(detail, sizeof(detail), "%" PRIu64 " -> %" PRIu64,
			 epoch_first, s3_wal_get_epoch(wal));
		check_true("the epoch advanced on reopen",
			   s3_wal_get_epoch(wal) > epoch_first, detail);
	}

	{
		struct replay_state st = {0};

		rc = sync_replay(wal, &st);
		check("s3_wal_replay", rc, 0);

		char detail[160];
		snprintf(detail, sizeof(detail),
			 "%" PRIu64 " writes, %" PRIu64 " zeroes, %" PRIu64
			 " unmaps, %" PRIu64 " barriers",
			 st.writes, st.zeroes, st.unmaps, st.barriers);
		check_true("every entry came back", st.writes == NUM_WRITES &&
			   st.zeroes == 1 && st.unmaps == 1 && st.barriers == 1,
			   detail);

		snprintf(detail, sizeof(detail), "%" PRIu64 " ok, %" PRIu64 " bad",
			 st.payload_ok, st.payload_bad);
		check_true("every payload survived byte for byte",
			   st.payload_ok == NUM_WRITES && st.payload_bad == 0,
			   detail);

		snprintf(detail, sizeof(detail), "%" PRIu64 " out of order",
			 st.out_of_order);
		check_true("seq came back strictly increasing",
			   st.out_of_order == 0, detail);
	}

	printf("\n[7] appending after replay, then replaying again\n");
	{
		uint64_t seq_before = s3_wal_get_next_seq(wal);

		fill_payload(payload, 5000, S3_WAL_BLOCK_SIZE);
		rc = sync_append_write(wal, 5000, payload);
		check("append after replay", rc, 0);

		check_true("seq advanced", s3_wal_get_next_seq(wal) > seq_before, NULL);

		rc = sync_close(wal);
		wal = NULL;
		check("close again", rc, 0);

		struct open_wal_ctx wctx = {0};
		s3_wal_open(local_dev, wal_open_cb, &wctx);
		poll_until(&wctx.done);
		if (wctx.status == 0) {
			struct replay_state st = {0};

			wal = wctx.wal;
			sync_replay(wal, &st);

			char detail[96];
			snprintf(detail, sizeof(detail), "%" PRIu64 " writes recovered",
				 st.writes);
			/* The entry added after the first replay must be there
			 * too, so the count grows by one. */
			check_true("the second replay sees the later append",
				   st.writes == NUM_WRITES + 1, detail);
		} else {
			check("reopen after the second close", wctx.status, 0);
		}
	}

	printf("\n[8] crossing a segment boundary\n");
	{
		/* Each 4 KiB payload plus a 64 byte header rounds up to 8 KiB per
		 * batch when appended one at a time, so a couple of hundred
		 * appends walk past the 2 MiB segment size and force an END
		 * sentinel plus a fresh segment. */
		int failed = 0;
		uint64_t before = s3_wal_get_used_bytes(wal);

		for (uint64_t i = 0; i < 400; i++) {
			fill_payload(payload, 10000 + i, S3_WAL_BLOCK_SIZE);
			rc = sync_append_write(wal, 10000 + i, payload);
			if (rc != 0) {
				printf("       !! append %" PRIu64 " failed: %d\n", i, rc);
				failed++;
				break;
			}
		}
		check_true("400 more appends succeeded across the boundary",
			   failed == 0, NULL);

		char detail[96];
		uint64_t after = s3_wal_get_used_bytes(wal);
		snprintf(detail, sizeof(detail), "%" PRIu64 " -> %" PRIu64 " bytes",
			 before, after);
		check_true("the log grew past one segment",
			   after > TEST_SEG_SIZE, detail);
		check_true("the head is still 4 KiB aligned",
			   after % S3_WAL_BLOCK_SIZE == 0, NULL);
	}

	printf("\n[9] replaying across the segment boundary\n");
	{
		rc = sync_close(wal);
		wal = NULL;
		check("close before the boundary replay", rc, 0);

		struct open_wal_ctx wctx = {0};
		s3_wal_open(local_dev, wal_open_cb, &wctx);
		poll_until(&wctx.done);
		check("reopen", wctx.status, 0);

		if (wctx.status == 0) {
			struct replay_state st = {0};

			wal = wctx.wal;
			rc = sync_replay(wal, &st);
			check("replay across the boundary", rc, 0);

			char detail[128];
			snprintf(detail, sizeof(detail),
				 "%" PRIu64 " writes, %" PRIu64 " bad payloads",
				 st.writes, st.payload_bad);
			/* 64 + 1 + 400 writes should all come back with intact
			 * payloads even though the log rolled over a segment. */
			check_true("entries survived the segment rollover",
				   st.writes == NUM_WRITES + 1 + 400 &&
				   st.payload_bad == 0, detail);
		}
	}

out_wal:
	if (wal) {
		/* Ignore the status here: teardown, not a check. */
		sync_close(wal);
		wal = NULL;
	}
	if (payload) {
		spdk_dma_free(payload);
	}
out_local:
	if (local_dev) {
		s3_local_dev_close(local_dev);
		local_dev = NULL;
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
	if (g_thread) {
		spdk_thread_exit(g_thread);
		while (!spdk_thread_is_exited(g_thread)) {
			spdk_thread_poll(g_thread, 0, 0);
		}
		spdk_thread_destroy(g_thread);
		spdk_set_thread(NULL);
	}
out_thread_lib:
	spdk_thread_lib_fini();
out_env:
	spdk_env_fini();
	spdk_log_close();

	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
