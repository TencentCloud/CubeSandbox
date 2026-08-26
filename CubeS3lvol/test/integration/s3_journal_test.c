/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   chunk map persistence check -- needs neither S3 nor credentials
 *
 *   === What this test proves ===
 *
 *   The hardest limitation left over from the early design was that the chunk
 *   map lived only in memory: stop the process and every object in S3 becomes
 *   an orphan. This test verifies that the journal actually solves that.
 *
 *   The core assertion is not "the journal can be written" but *the mapping
 *   survives a full destroy / rebuild cycle*:
 *
 *     1. format the local bdev (super + journal regions)
 *     2. build a chunk map, attach the journal, write a batch of mappings
 *     3. *destroy the map and journal objects* (simulating process exit)
 *     4. reopen the journal and replay
 *     5. compare entry by entry: uuid / valid_bytes / gen / flags all match
 *
 *   === Fully asynchronous API ===
 *
 *   The journal / local_dev / chunk map interfaces are all callback based (there
 *   are no synchronous variants; see include/s3lvol/s3_journal.h for why nested
 *   polling corrupts the reactor state machine). This test wraps them back into
 *   sequential control flow with poll_until(). That is acceptable *here* because
 *   the test has a single thread, no other pollers, and calls in from main's
 *   ordinary control flow -- the one situation where nested polling is safe.
 *   *Production code must not copy this pattern.*
 *
 *   === The local device is an aio bdev ===
 *
 *   A sparse file is created under /data with ftruncate and handed to the
 *   upstream bdev_aio module. No hand-written in-memory bdev, because:
 *
 *   - the journal and local_dev go through spdk_bdev_write/read, and only a real
 *     bdev module also exercises 4 KiB alignment, O_DIRECT and
 *     required_alignment;
 *   - "persistence" is only convincing when verified against a device that
 *     really reaches the filesystem;
 *   - it is one less bdev implementation to maintain.
 *
 *   The cost is having to bring up three framework layers by hand, in order
 *   (a bdev channel acquires an accel channel, and accel depends on iobuf).
 *   That sequence is taken from spdk/test/lvol/esnap/esnap.c.
 *
 *   Usage:
 *     ./s3_journal_test [aio-file-path]
 *
 *   Defaults to /data/s3lvol_journal_test.aio, which is removed on exit.
 */

#include "spdk/stdinc.h"
#include "spdk/accel.h"
#include "spdk/bdev.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_chunk_map.h"
#include "s3lvol/s3_journal.h"
#include "s3lvol/s3_local_dev.h"

/* bdev_aio's header is not part of the installed includes; -I$(SPDK_ROOT)/module
 * makes it reachable. Use the real header rather than copying the prototype --
 * a copy drifts from upstream. */
#include "bdev/aio/bdev_aio.h"

#define AIO_BDEV_NAME    "s3lvol_wal0"
#define AIO_BLOCK_SIZE   4096
#define AIO_FILE_SIZE    (128ULL * 1024 * 1024)
#define DEFAULT_AIO_PATH "/data/s3lvol_journal_test.aio"

static int g_pass;
static int g_fail;

static struct spdk_thread *g_thread;

static void
check(const char *what, int got, int want)
{
	if (got == want) {
		printf("  [PASS] %-48s rc=%d\n", what, got);
		g_pass++;
	} else {
		printf("  [FAIL] %-48s rc=%d (expected %d)\n", what, got, want);
		g_fail++;
	}
}

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-48s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-48s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

/* ==========================================================================
 * Asynchronous -> synchronous
 *
 * There is only one thread and every callback runs on it, so polling until the
 * flag is set is enough. The timeout exists because "the async chain broke
 * halfway" is the most common failure mode for this kind of code, and a hang is
 * much harder to diagnose than an error.
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

/* Callback for local_dev format/open */
struct open_ctx {
	bool                done;
	int                   status;
	struct s3_local_dev  *dev;
};

static void
local_dev_open_cb(void *cb_arg, struct s3_local_dev *dev, int status)
{
	struct open_ctx *ctx = cb_arg;

	ctx->dev    = dev;
	ctx->status = status;
	ctx->done   = true;
}

/* Callback for journal create */
struct jcreate_ctx {
	bool               done;
	int                status;
	struct s3_journal *journal;
};

static void
journal_create_cb(void *cb_arg, struct s3_journal *journal, int status)
{
	struct jcreate_ctx *ctx = cb_arg;

	ctx->journal = journal;
	ctx->status  = status;
	ctx->done    = true;
}

/* Callback for chunk map insert/remove */
struct cm_ctx {
	bool             done;
	int              status;
	struct spdk_uuid old_uuid;
};

static void
chunk_map_cb(void *cb_arg, const struct spdk_uuid *old_uuid, int status)
{
	struct cm_ctx *ctx = cb_arg;

	spdk_uuid_copy(&ctx->old_uuid, old_uuid);
	ctx->status = status;
	ctx->done   = true;
}

/* Synchronous wrapper around insert: submit, then poll until the callback. */
static int
sync_insert(struct s3_chunk_map *map, uint64_t chunk_index,
	    const struct spdk_uuid *uuid, uint32_t valid_bytes,
	    struct spdk_uuid *out_old)
{
	struct cm_ctx ctx = {0};

	s3_chunk_map_insert(map, chunk_index, uuid, valid_bytes,
			    chunk_map_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	if (out_old) {
		spdk_uuid_copy(out_old, &ctx.old_uuid);
	}
	return ctx.status;
}

static int
sync_remove(struct s3_chunk_map *map, uint64_t chunk_index,
	    struct spdk_uuid *out_old)
{
	struct cm_ctx ctx = {0};

	s3_chunk_map_remove(map, chunk_index, chunk_map_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	if (out_old) {
		spdk_uuid_copy(out_old, &ctx.old_uuid);
	}
	return ctx.status;
}

/* ==========================================================================
 * aio backing file
 * ========================================================================== */

/* A sparse file is fine -- the journal only writes the blocks it actually uses,
 * so there is no need to really occupy 128 MiB.
 *
 * O_NOFOLLOW + 0600: the path is fixed, so without NOFOLLOW someone could leave
 * a symlink there beforehand and we would truncate somebody else's file. */
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
 * Test data: deterministic uuids so replay can be compared entry by entry
 * ========================================================================== */

#define NUM_CHUNKS_TESTED 200
#define TEST_CHUNK_SIZE   (1024 * 1024)
#define TEST_CAPACITY     ((uint64_t)NUM_CHUNKS_TESTED * TEST_CHUNK_SIZE)

struct expected_entry {
	struct spdk_uuid uuid;
	uint32_t         valid_bytes;
	bool             present;
};

static struct expected_entry g_expected[NUM_CHUNKS_TESTED];

/* Derive a deterministic uuid from chunk_index, so after replay it can be
 * recomputed independently instead of trusting a table the test itself kept
 * (a wrong table would go unnoticed). */
static void
make_uuid(struct spdk_uuid *uuid, uint64_t chunk_index, uint32_t round)
{
	uint8_t *raw = (uint8_t *)uuid;

	memset(uuid, 0, sizeof(*uuid));
	raw[0] = 0xA5;
	raw[1] = (uint8_t)(chunk_index & 0xff);
	raw[2] = (uint8_t)((chunk_index >> 8) & 0xff);
	raw[3] = (uint8_t)round;
	raw[15] = 0x5A;   /* make sure it is never all-zero */
}

/* ==========================================================================
 * Replay callback
 * ========================================================================== */

struct replay_ctx {
	struct s3_chunk_map *map;
	uint64_t             updates;
	uint64_t             removes;
};

static int
replay_apply(void *cb_arg, const struct s3_journal_record *rec)
{
	struct replay_ctx *ctx = cb_arg;

	switch (rec->op) {
	case S3_JOURNAL_OP_CHUNK_UPDATE:
		ctx->updates++;
		return s3_chunk_map_apply_update(ctx->map, rec->chunk_index,
						 &rec->uuid, rec->valid_bytes,
						 rec->gen, rec->flags, rec->lsn);
	case S3_JOURNAL_OP_CHUNK_REMOVE:
		ctx->removes++;
		return s3_chunk_map_apply_remove(ctx->map, rec->chunk_index,
						 rec->lsn);
	case S3_JOURNAL_OP_CHECKPOINT:
		return 0;
	default:
		printf("       !! unknown op %u\n", rec->op);
		return -EILSEQ;
	}
}

/* Synchronous wrapper around replay */
static int
sync_replay(struct s3_journal *journal, uint64_t from_lsn,
	    struct replay_ctx *rctx)
{
	struct async_ctx ctx = {0};

	s3_journal_replay(journal, from_lsn, replay_apply, rctx,
			  async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

/* ==========================================================================
 * Bringing the SPDK framework up and down by hand
 *
 * The order matters and must not be swapped:
 *   iobuf  <- accel registers an iobuf module on top of it
 *   accel  <- every bdev channel acquires an accel channel
 *   bdev
 * Teardown runs in reverse.
 * ========================================================================== */

static int
framework_start(void)
{
	struct spdk_iobuf_opts iobuf_opts;
	struct spdk_bdev_opts bdev_opts;
	struct async_ctx ctx = {0};
	int rc;

	/* The default iobuf pools are 64 MiB + 135 MiB, which is too tight under
	 * --no-huge with512 MiB. Every I/O in this test is a single 4 KiB
	 * segment, so only a handful of buffers are ever needed. */
	spdk_iobuf_get_opts(&iobuf_opts, sizeof(iobuf_opts));
	iobuf_opts.small_pool_count = 1024;
	iobuf_opts.large_pool_count = 64;
	iobuf_opts.opts_size = sizeof(iobuf_opts);
	rc = spdk_iobuf_set_opts(&iobuf_opts);
	if (rc != 0) {
		fprintf(stderr, "spdk_iobuf_set_opts failed: %d\n", rc);
		return rc;
	}

	rc = spdk_iobuf_initialize();
	if (rc != 0) {
		fprintf(stderr, "spdk_iobuf_initialize failed: %d\n", rc);
		return rc;
	}

	rc = spdk_accel_initialize();
	if (rc != 0) {
		fprintf(stderr, "spdk_accel_initialize failed: %d\n", rc);
		return rc;
	}

	spdk_bdev_get_opts(&bdev_opts, sizeof(bdev_opts));
	/* 64 K bdev_io structures by default is tens of MiB of pointless
	 * overhead under --no-huge. */
	bdev_opts.bdev_io_pool_size = 4096;
	/* auto_examine would make every bdev module linked in examine this aio
	 * disk (gpt / lvol / ...). All that is wanted here is a raw device, so
	 * examine only drags in unrelated dependencies and noise. */
	bdev_opts.bdev_auto_examine = false;
	bdev_opts.opts_size = sizeof(bdev_opts);
	rc = spdk_bdev_set_opts(&bdev_opts);
	if (rc != 0) {
		fprintf(stderr, "spdk_bdev_set_opts failed: %d\n", rc);
		return rc;
	}

	spdk_bdev_initialize(async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		fprintf(stderr, "spdk_bdev_initialize timed out\n");
		return -ETIMEDOUT;
	}
	if (ctx.status != 0) {
		fprintf(stderr, "spdk_bdev_initialize failed: %d\n", ctx.status);
		return ctx.status;
	}

	return 0;
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

/* ==========================================================================
 * Wrap-around
 *
 * Everything above runs on a journal far bigger than the number of records the
 * test writes, so no block is ever reused. This section uses a four-block
 * journal instead -- 4 x 64 = 256 records to go once round -- which puts the wrap
 * within reach of a few hundred synchronous inserts.
 *
 * What needs guarding is not the wrap but *replay after one*. Records are read
 * in physical block order, and after a wrap that order runs newest-first, so
 * three properties matter, none of them exercised anywhere else:
 *
 *   (c) a record no newer than what an entry already holds must be dropped.
 *       Otherwise "last writer wins" moves the mapping back to an object the
 *       create-once path has already deleted, and the next read of that chunk is
 *       a 404.
 *   (d) blocks physically after the newest one must still be read. Otherwise
 *       mappings that live only there come back as "chunk never written", and
 *       reads of them return zeroes.
 *   (e) the write cursor must land on the block holding the highest LSN, not on
 *       the last block scanned. Otherwise the next append tries to reuse a block
 *       full of the newest records.
 *
 * Plus the two that bound the ring itself:
 *
 *   (a) filling the ring with no checkpoint behind it must fail with -ENOSPC
 *       rather than overwrite records that are still the only copy of a mapping;
 *   (b) a checkpoint covering the block about to be reused must let it proceed.
 *
 * Every insert is awaited individually, so record k lands in block k / 64 at
 * slot k % 64 and the LSNs are dense. That is what makes the placement below
 * predictable enough to assert on.
 * ========================================================================== */

#define WRAP_BDEV_NAME  "s3lvol_wrap0"
#define WRAP_FILE_SIZE  (8ULL * 1024 * 1024)
#define WRAP_BLOCKS     4
#define WRAP_PER_BLOCK  ((uint64_t)S3_JOURNAL_RECORDS_PER_BLOCK)
#define WRAP_RING       (WRAP_BLOCKS * WRAP_PER_BLOCK)
#define WRAP_CHUNKS     (WRAP_RING + 64)

/* Both sit in blocks 1 and 2, i.e. physically *after* the block that gets
 * reused. X is rewritten after the wrap, so replay has to prefer the newer
 * record; Y never is, so replay has to reach it at all. */
#define WRAP_CHUNK_X    (WRAP_PER_BLOCK + 37)
#define WRAP_CHUNK_Y    (2 * WRAP_PER_BLOCK + 11)

/* One record per chunk, one chunk per 4 KiB, so chunk_index == block index and
 * the arithmetic above is the whole story. */
#define WRAP_CHUNK_SIZE 4096

static void
test_journal_wrap(const char *base_path)
{
	char aio_path[512];
	struct s3_local_dev *dev = NULL;
	struct s3_journal *journal = NULL;
	struct s3_chunk_map *map = NULL;
	struct spdk_uuid uuid, x_new, got;
	uint64_t k, next_lsn_before = 0;
	bool file_created = false, bdev_created = false;
	char detail[160];
	int rc;

	snprintf(aio_path, sizeof(aio_path), "%s.wrap", base_path);

	printf("\n[12] journal wrap-around: refusal, reuse, and replay across it\n");

	rc = make_aio_file(aio_path, WRAP_FILE_SIZE);
	check("a second backing file for a small journal", rc, 0);
	if (rc != 0) {
		return;
	}
	file_created = true;

	rc = create_aio_bdev(WRAP_BDEV_NAME, aio_path, AIO_BLOCK_SIZE,
			     false, false, NULL, false);
	check("create_aio_bdev for the wrap test", rc, 0);
	if (rc != 0) {
		goto out_file;
	}
	bdev_created = true;

	{
		struct open_ctx octx = {0};
		struct s3_local_dev_format_opts fopts = {
			.wal_bdev_name  = WRAP_BDEV_NAME,
			.lvs_name       = "wrap_test",
			.capacity_bytes = WRAP_CHUNKS * WRAP_CHUNK_SIZE,
			.chunk_size     = WRAP_CHUNK_SIZE,
			.journal_size   = WRAP_BLOCKS * WRAP_PER_BLOCK *
					  sizeof(struct s3_journal_record),
			.wal_size       = 1024 * 1024,
		};

		s3_local_dev_format(&fopts, local_dev_open_cb, &octx);
		if (!poll_until(&octx.done)) {
			goto out_bdev;
		}
		check("format with a four-block journal", octx.status, 0);
		if (octx.status != 0) {
			goto out_bdev;
		}
		dev = octx.dev;
	}

	{
		struct jcreate_ctx jctx = {0};

		s3_journal_create(dev, journal_create_cb, &jctx);
		if (!poll_until(&jctx.done)) {
			goto out_dev;
		}
		check("s3_journal_create (small journal)", jctx.status, 0);
		if (jctx.status != 0) {
			goto out_dev;
		}
		journal = jctx.journal;
	}

	snprintf(detail, sizeof(detail), "%" PRIu64 " bytes = %d blocks x %"
		 PRIu64 " records", s3_journal_get_capacity_bytes(journal),
		 WRAP_BLOCKS, WRAP_PER_BLOCK);
	check_true("the ring is only four blocks wide",
		   s3_journal_get_capacity_bytes(journal) ==
		   WRAP_BLOCKS * WRAP_PER_BLOCK *
		   sizeof(struct s3_journal_record), detail);

	rc = s3_chunk_map_create(WRAP_CHUNKS, 4096, WRAP_CHUNK_SIZE, &map);
	check("chunk map for the wrap test", rc, 0);
	if (rc != 0) {
		goto out_journal;
	}
	s3_chunk_map_set_journal(map, journal);

	/* ---- fill the ring exactly ---- */
	for (k = 0; k < WRAP_RING; k++) {
		make_uuid(&uuid, k, 0);
		rc = sync_insert(map, k, &uuid, WRAP_CHUNK_SIZE, NULL);
		if (rc != 0) {
			printf("       !! insert %" PRIu64 " failed: %d\n", k, rc);
			break;
		}
	}
	snprintf(detail, sizeof(detail), "%" PRIu64 " of %" PRIu64 " records",
		 k, (uint64_t)WRAP_RING);
	check_true("the ring fills to the last slot without complaint",
		   k == WRAP_RING, detail);
	if (k != WRAP_RING) {
		goto out_map;
	}

	/* ---- (a) one more, with nothing checkpointed: must be refused ---- */
	make_uuid(&uuid, WRAP_RING, 0);
	rc = sync_insert(map, WRAP_RING, &uuid, WRAP_CHUNK_SIZE, NULL);
	snprintf(detail, sizeof(detail), "rc=%d", rc);
	check_true("appending past a ring no checkpoint covers gives -ENOSPC",
		   rc == -ENOSPC, detail);

	/* Refusal has to mean the records are still there, which is the whole
	 * point of it -- so the ring must not have advanced. */
	snprintf(detail, sizeof(detail), "used=%" PRIu64 " capacity=%" PRIu64,
		 s3_journal_get_used_bytes(journal),
		 s3_journal_get_capacity_bytes(journal));
	check_true("and leaves the ring full rather than partly overwritten",
		   s3_journal_get_used_bytes(journal) ==
		   s3_journal_get_capacity_bytes(journal), detail);

	/* ---- (b) a checkpoint covering block 0 lets it be reused ---- */
	/* Exactly block 0: its highest LSN is WRAP_PER_BLOCK, and block 1 starts
	 * one above that, so this authorises one block of reuse and no more. */
	s3_journal_truncate(journal, WRAP_PER_BLOCK);

	rc = sync_insert(map, WRAP_RING, &uuid, WRAP_CHUNK_SIZE, NULL);
	check("the same append, once a checkpoint covers block 0", rc, 0);
	if (rc != 0) {
		goto out_map;
	}

	/* ---- rewrite X after the wrap ---- */
	/* Block 0 is left partly filled on purpose: its first empty slot is what
	 * used to end the scan, leaving blocks 1 to 3 unread. */
	make_uuid(&x_new, WRAP_CHUNK_X, 1);
	rc = sync_insert(map, WRAP_CHUNK_X, &x_new, WRAP_CHUNK_SIZE, NULL);
	check("rewriting a chunk whose old record is in block 1", rc, 0);
	if (rc != 0) {
		goto out_map;
	}

	next_lsn_before = s3_journal_get_next_lsn(journal);

	/* ---- replay it back from a fresh journal and an empty map ---- */
	s3_chunk_map_set_journal(map, NULL);
	s3_chunk_map_destroy(map);
	map = NULL;
	s3_journal_destroy(journal);
	journal = NULL;

	rc = s3_journal_open(dev, &journal);
	check("reopening the wrapped journal", rc, 0);
	if (rc != 0) {
		goto out_dev;
	}
	rc = s3_chunk_map_create(WRAP_CHUNKS, 4096, WRAP_CHUNK_SIZE, &map);
	check("a fresh chunk map to replay into", rc, 0);
	if (rc != 0) {
		goto out_journal;
	}

	{
		struct replay_ctx rctx = { .map = map };

		/* from_lsn is the checkpoint that authorised the wrap, which is
		 * what the super block would carry in production. */
		rc = sync_replay(journal, WRAP_PER_BLOCK, &rctx);
		check("replaying across the wrap", rc, 0);

		snprintf(detail, sizeof(detail), "%" PRIu64 " applied, %" PRIu64
			 " skipped", rctx.updates, rctx.removes);
		check_true("replay ran to completion", rc == 0, detail);
	}

	/* ---- (c) the newer record wins, whatever order it was read in ---- */
	rc = s3_chunk_map_lookup(map, WRAP_CHUNK_X, &got, NULL);
	{
		struct spdk_uuid x_old;
		bool is_new = (rc == 0 && spdk_uuid_compare(&got, &x_new) == 0);
		bool is_old;

		make_uuid(&x_old, WRAP_CHUNK_X, 0);
		is_old = (rc == 0 && spdk_uuid_compare(&got, &x_old) == 0);

		snprintf(detail, sizeof(detail), "chunk %" PRIu64 ": %s",
			 (uint64_t)WRAP_CHUNK_X,
			 is_new ? "the post-wrap record" :
			 is_old ? "the PRE-wrap record -- mapping went backwards" :
			 "neither uuid");
		check_true("a chunk rewritten after the wrap keeps the new object",
			   is_new, detail);
	}

	/* ---- (d) blocks after the reused one were read at all ---- */
	rc = s3_chunk_map_lookup(map, WRAP_CHUNK_Y, &got, NULL);
	{
		struct spdk_uuid y_want;

		make_uuid(&y_want, WRAP_CHUNK_Y, 0);
		snprintf(detail, sizeof(detail), "chunk %" PRIu64 ", rc=%d",
			 (uint64_t)WRAP_CHUNK_Y, rc);
		check_true("a chunk recorded only in a later block survives",
			   rc == 0 && spdk_uuid_compare(&got, &y_want) == 0,
			   detail);
	}

	/* Not just those two: every record above the checkpoint has to be back.
	 * Blocks 1 to 3 hold chunks WRAP_PER_BLOCK..WRAP_RING-1, and block 0's
	 * reused slots add chunk WRAP_RING. Chunks 0..WRAP_PER_BLOCK-1 are
	 * deliberately absent -- their records are below the checkpoint, so in
	 * production they would come from the checkpoint rather than the journal. */
	{
		uint64_t missing = 0, wrong = 0;

		for (k = WRAP_PER_BLOCK; k < WRAP_RING; k++) {
			struct spdk_uuid want;

			if (k == WRAP_CHUNK_X) {
				continue;   /* asserted above, with a newer uuid */
			}
			make_uuid(&want, k, 0);
			if (s3_chunk_map_lookup(map, k, &got, NULL) != 0) {
				missing++;
			} else if (spdk_uuid_compare(&got, &want) != 0) {
				wrong++;
			}
		}
		snprintf(detail, sizeof(detail), "%" PRIu64 " missing, %" PRIu64
			 " with the wrong uuid, of %" PRIu64 " checked",
			 missing, wrong, WRAP_RING - WRAP_PER_BLOCK - 1);
		check_true("every mapping above the checkpoint came back",
			   missing == 0 && wrong == 0, detail);

		snprintf(detail, sizeof(detail), "allocated=%" PRIu64
			 ", expected %" PRIu64,
			 s3_chunk_map_get_allocated(map),
			 WRAP_RING - WRAP_PER_BLOCK + 1);
		check_true("and nothing extra did",
			   s3_chunk_map_get_allocated(map) ==
			   WRAP_RING - WRAP_PER_BLOCK + 1, detail);
	}

	snprintf(detail, sizeof(detail), "%" PRIu64 " before, %" PRIu64 " after",
		 next_lsn_before, s3_journal_get_next_lsn(journal));
	check_true("the LSN resumes where it was, not from the last block read",
		   s3_journal_get_next_lsn(journal) == next_lsn_before, detail);

	/* ---- (e) the cursor is on the newest block, so appends still fit ---- */
	/* Block 0 has two records and 62 free slots, and this journal was opened
	 * fresh so nothing is checkpointed as far as it knows. Landing the cursor
	 * on the last block scanned (block 3, full) would send the next append
	 * looking to reuse block 0, which the wrap check refuses -- a journal that
	 * rejects writes while most of a block stands empty. */
	s3_chunk_map_set_journal(map, journal);
	make_uuid(&uuid, WRAP_RING + 1, 0);
	rc = sync_insert(map, WRAP_RING + 1, &uuid, WRAP_CHUNK_SIZE, NULL);
	snprintf(detail, sizeof(detail), "rc=%d", rc);
	check_true("appending after the replay lands in the reused block",
		   rc == 0, detail);

out_map:
	if (map) {
		s3_chunk_map_set_journal(map, NULL);
		s3_chunk_map_destroy(map);
	}
out_journal:
	if (journal) {
		s3_journal_destroy(journal);
	}
out_dev:
	if (dev) {
		s3_local_dev_close(dev);
	}
out_bdev:
	if (bdev_created) {
		struct async_ctx ctx = {0};

		bdev_aio_delete(WRAP_BDEV_NAME, async_int_cb, &ctx);
		if (!poll_until(&ctx.done)) {
			fprintf(stderr, "bdev_aio_delete(wrap) timed out\n");
		}
	}
out_file:
	if (file_created) {
		unlink(aio_path);
	}
}

/* ==========================================================================
 * main
 * ========================================================================== */

int
main(int argc, char **argv)
{
	struct spdk_env_opts env_opts;
	struct s3_local_dev *local_dev = NULL;
	struct s3_journal *journal = NULL;
	struct s3_chunk_map *map = NULL;
	const char *aio_path = DEFAULT_AIO_PATH;
	bool file_created = false;
	bool bdev_created = false;
	bool framework_up = false;
	int rc;

	if (argc > 1) {
		aio_path = argv[1];
	}

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== chunk map persistence check (journal on aio bdev) ===\n\n");

	env_opts.opts_size = sizeof(env_opts);
	spdk_env_opts_init(&env_opts);
	env_opts.name     = "s3_journal_test";
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
	/* The first thread created becomes the app thread, which
	 * spdk_bdev_initialize() asserts on. */
	g_thread = spdk_thread_create("journal_test", NULL);
	if (!g_thread) {
		goto out_thread_lib;
	}
	spdk_set_thread(g_thread);

	/* ---------- 0. bring up iobuf / accel / bdev ---------- */
	printf("[0] bringing up the SPDK framework (iobuf -> accel -> bdev)\n");
	rc = framework_start();
	check("framework_start", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	framework_up = true;

	/* ---------- 1. create an aio bdev as the local device ---------- */
	/* 128 MiB is plenty for a 16 MiB journal plus a 64 MiB WAL. */
	printf("\n[1] creating an aio bdev on %s (%" PRIu64 " MiB, used as the WAL bdev)\n",
	       aio_path, (uint64_t)(AIO_FILE_SIZE / (1024 * 1024)));
	rc = make_aio_file(aio_path, AIO_FILE_SIZE);
	check("ftruncate the backing file", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	file_created = true;

	rc = create_aio_bdev(AIO_BDEV_NAME, aio_path, AIO_BLOCK_SIZE,
			     false /* readonly */, false /* fallocate */,
			     NULL /* uuid */, false /* nowait */);
	check("create_aio_bdev", rc, 0);
	if (rc != 0) {
		goto out_file;
	}
	bdev_created = true;

	/* ---------- 2. format the layout ---------- */
	printf("\n[2] s3_local_dev_format (single-bdev layout)\n");
	{
		struct open_ctx octx = {0};
		/* 16 MiB for the journal and 64 MiB for the WAL both fit on a
		 * 128 MiB disk; whatever is left goes to the cache. */
		struct s3_local_dev_format_opts fopts = {
			.wal_bdev_name  = AIO_BDEV_NAME,
			.lvs_name       = "journal_test",
			.capacity_bytes = 256ULL * 1024 * 1024,
			.chunk_size     = TEST_CHUNK_SIZE,
			.journal_size   = 16 * 1024 * 1024,
			.wal_size       = 64 * 1024 * 1024,
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

	{
		const struct s3_region *jr =
			s3_local_dev_get_region(local_dev, S3_REGION_JOURNAL);
		char detail[96];
		snprintf(detail, sizeof(detail), "journal @ %" PRIu64 ", %" PRIu64 " MiB",
			 jr->offset, jr->size / (1024 * 1024));
		check_true("journal region was allocated", jr && jr->valid, detail);
	}

	/* ---------- 3. create the journal and the chunk map ---------- */
	printf("\n[3] s3_journal_create + chunk map\n");
	{
		struct jcreate_ctx jctx = {0};

		s3_journal_create(local_dev, journal_create_cb, &jctx);
		if (!poll_until(&jctx.done)) {
			goto out_local;
		}
		check("s3_journal_create", jctx.status, 0);
		if (jctx.status != 0) {
			goto out_local;
		}
		journal = jctx.journal;
	}

	rc = s3_chunk_map_create(TEST_CAPACITY / 4096, 4096, TEST_CHUNK_SIZE, &map);
	check("s3_chunk_map_create", rc, 0);
	if (rc != 0) {
		goto out_journal;
	}
	s3_chunk_map_set_journal(map, journal);

	/* ---------- 4. write the mappings ---------- */
	printf("\n[4] writing %d mappings (durable once the callback fires)\n",
	       NUM_CHUNKS_TESTED);
	{
		int failed = 0;

		for (uint64_t i = 0; i < NUM_CHUNKS_TESTED; i++) {
			struct spdk_uuid uuid;
			uint32_t vb = (uint32_t)((i + 1) * 4096) % TEST_CHUNK_SIZE;

			if (vb == 0) {
				vb = 4096;
			}
			make_uuid(&uuid, i, 0);

			rc = sync_insert(map, i, &uuid, vb, NULL);
			if (rc != 0) {
				printf("       !! insert chunk %" PRIu64 " failed: %d\n", i, rc);
				failed++;
				break;
			}
			spdk_uuid_copy(&g_expected[i].uuid, &uuid);
			g_expected[i].valid_bytes = vb;
			g_expected[i].present = true;
		}
		check_true("all inserts succeeded", failed == 0, NULL);
		char detail[64];
		snprintf(detail, sizeof(detail), "allocated=%" PRIu64,
			 s3_chunk_map_get_allocated(map));
		check_true("allocated count is correct",
			   s3_chunk_map_get_allocated(map) == NUM_CHUNKS_TESTED,
			   detail);
	}

	/* Overwrite some entries to verify gen increments and old uuids come back */
	printf("\n[5] overwriting the first 20 (gen increment + old uuid handback)\n");
	{
		int old_returned = 0;

		for (uint64_t i = 0; i < 20; i++) {
			struct spdk_uuid uuid, old, want_old;

			make_uuid(&uuid, i, 1);
			rc = sync_insert(map, i, &uuid, TEST_CHUNK_SIZE, &old);
			if (rc != 0) {
				break;
			}
			/* The old uuid should be the round 0 one */
			make_uuid(&want_old, i, 0);
			if (spdk_uuid_compare(&old, &want_old) == 0) {
				old_returned++;
			}
			spdk_uuid_copy(&g_expected[i].uuid, &uuid);
			g_expected[i].valid_bytes = TEST_CHUNK_SIZE;
		}
		check("overwrite", rc, 0);
		char detail[64];
		snprintf(detail, sizeof(detail), "%d/20 handed back correctly",
			 old_returned);
		check_true("old uuid handed back to the caller (for GC)",
			   old_returned == 20, detail);
	}

	/* Remove a few to verify removals are journaled too */
	printf("\n[6] removing chunks 50-59 (removals must persist as well)\n");
	for (uint64_t i = 50; i < 60; i++) {
		rc = sync_remove(map, i, NULL);
		if (rc != 0) {
			break;
		}
		g_expected[i].present = false;
	}
	check("s3_chunk_map_remove", rc, 0);

	{
		uint64_t used = s3_journal_get_used_bytes(journal);
		uint64_t lsn  = s3_journal_get_next_lsn(journal);
		char detail[96];
		snprintf(detail, sizeof(detail), "used=%" PRIu64 " B, next_lsn=%" PRIu64,
			 used, lsn);
		/* 200 inserts + 20 overwrites + 10 removes = 230 records, LSNs
		 * starting at 1 */
		check_true("journal LSN matches the record count", lsn == 231, detail);
	}

	/* ---------- 7. several operations in flight on the same chunk ---------- */
	/* This is the new hazard introduced by going asynchronous: if old_uuid came
	 * from committed state rather than the "latest intent", a run of overwrites
	 * A->B->C would hand back A twice -- A gets deleted twice while B leaks
	 * forever. Deliberately does not poll between submissions: all three go out
	 * first and are awaited together. */
	printf("\n[7] three overwrites of one chunk, not awaited individually "
	       "(old_uuid chain)\n");
	{
		const uint64_t ci = 100;
		struct spdk_uuid u1, u2, u3;
		struct cm_ctx c1 = {0}, c2 = {0}, c3 = {0};

		make_uuid(&u1, ci, 11);
		make_uuid(&u2, ci, 12);
		make_uuid(&u3, ci, 13);

		s3_chunk_map_insert(map, ci, &u1, 4096, chunk_map_cb, &c1);
		s3_chunk_map_insert(map, ci, &u2, 8192, chunk_map_cb, &c2);
		s3_chunk_map_insert(map, ci, &u3, 12288, chunk_map_cb, &c3);

		poll_until(&c1.done);
		poll_until(&c2.done);
		poll_until(&c3.done);

		check_true("all three concurrent inserts succeeded",
			   c1.status == 0 && c2.status == 0 && c3.status == 0, NULL);

		/* c1's old uuid is the round 0 one written in [4]; c2's is u1and
		 * c3's is u2. Every superseded object is handed back exactly once,
		 * with no duplicates and nothing missed. */
		struct spdk_uuid want0;
		make_uuid(&want0, ci, 0);
		check_true("1st received the pre-existing uuid",
			   spdk_uuid_compare(&c1.old_uuid, &want0) == 0, NULL);
		check_true("2nd received the uuid written by the 1st",
			   spdk_uuid_compare(&c2.old_uuid, &u1) == 0, NULL);
		check_true("3rd received the uuid written by the 2nd",
			   spdk_uuid_compare(&c3.old_uuid, &u2) == 0, NULL);

		/* The committed state must end up as the last write */
		struct spdk_uuid got;
		uint32_t got_vb = 0;
		rc = s3_chunk_map_lookup(map, ci, &got, &got_vb);
		check_true("final mapping is the last write",
			   rc == 0 && spdk_uuid_compare(&got, &u3) == 0 &&
			   got_vb == 12288, NULL);

		spdk_uuid_copy(&g_expected[ci].uuid, &u3);
		g_expected[ci].valid_bytes = 12288;
		g_expected[ci].present = true;
	}

	/* ---------- 8. simulate process exit ---------- */
	printf("\n[8] destroying the map and journal (simulates process exit)\n");
	s3_chunk_map_destroy(map);
	map = NULL;
	s3_journal_destroy(journal);
	journal = NULL;
	check_true("objects destroyed; every in-memory mapping is gone", true, NULL);

	/* ---------- 9. reopen and replay ---------- */
	printf("\n[9] reopening the journal and replaying\n");
	rc = s3_journal_open(local_dev, &journal);
	check("s3_journal_open", rc, 0);
	if (rc != 0) {
		goto out_local;
	}

	rc = s3_chunk_map_create(TEST_CAPACITY / 4096, 4096, TEST_CHUNK_SIZE, &map);
	check("rebuild an empty chunk map", rc, 0);
	if (rc != 0) {
		goto out_journal;
	}

	{
		struct replay_ctx rctx = { .map = map };
		const struct s3_super_block *sb = s3_local_dev_get_super(local_dev);

		rc = sync_replay(journal, sb->checkpoint_lsn, &rctx);
		check("s3_journal_replay", rc, 0);

		char detail[96];
		snprintf(detail, sizeof(detail), "%" PRIu64 " updates + %" PRIu64
			 " removes", rctx.updates, rctx.removes);
		/* 200 + 20 + 3 (the concurrent writes in [7]) = 223 updates,
		 * 10 removes */
		check_true("replayed record count matches what was written",
			   rctx.updates == 223 && rctx.removes == 10, detail);
	}

	/* ---------- 10. entry-by-entry comparison: the core assertion ---------- */
	printf("\n[10] comparing the recovered mapping entry by entry\n");
	{
		int mismatch = 0;
		int checked = 0;
		uint64_t expect_alloc = 0;

		for (uint64_t i = 0; i < NUM_CHUNKS_TESTED; i++) {
			struct spdk_uuid got;
			uint32_t got_vb = 0;

			rc = s3_chunk_map_lookup(map, i, &got, &got_vb);

			if (!g_expected[i].present) {
				if (rc != -ENOENT) {
					printf("       !! chunk %" PRIu64 " should have "
					       "been removed but was found (rc=%d)\n",
					       i, rc);
					mismatch++;
				}
				continue;
			}
			expect_alloc++;

			if (rc != 0) {
				printf("       !! chunk %" PRIu64 " is missing (rc=%d)\n",
				       i, rc);
				mismatch++;
				continue;
			}
			if (spdk_uuid_compare(&got, &g_expected[i].uuid) != 0) {
				printf("       !! chunk %" PRIu64 " uuid mismatch\n", i);
				mismatch++;
				continue;
			}
			if (got_vb != g_expected[i].valid_bytes) {
				printf("       !! chunk %" PRIu64 " valid_bytes %u != %u\n",
				       i, got_vb, g_expected[i].valid_bytes);
				mismatch++;
				continue;
			}
			checked++;
		}

		char detail[96];
		snprintf(detail, sizeof(detail), "%d identical, %d mismatched",
			 checked, mismatch);
		check_true("recovered mapping matches the writes, entry by entry",
			   mismatch == 0, detail);

		snprintf(detail, sizeof(detail), "allocated=%" PRIu64 ", expected %" PRIu64,
			 s3_chunk_map_get_allocated(map), expect_alloc);
		check_true("allocated count is correct after replay",
			   s3_chunk_map_get_allocated(map) == expect_alloc, detail);
	}

	/* ---------- 11. appending after replay; LSNs must not go backwards ------- */
	printf("\n[11] appending after replay (LSN must continue where it left off)\n");
	{
		uint64_t lsn_before = s3_journal_get_next_lsn(journal);
		struct spdk_uuid uuid;
		/* Use a chunk from the range removed in [6]: it is within the map's
		 * capacity and genuinely empty, so this insert takes the
		 * "fresh allocation" path. */
		const uint64_t new_chunk = 55;

		s3_chunk_map_set_journal(map, journal);
		make_uuid(&uuid, new_chunk, 2);
		rc = sync_insert(map, new_chunk, &uuid, 4096, NULL);
		check("insert after replay", rc, 0);

		uint64_t lsn_after = s3_journal_get_next_lsn(journal);
		char detail[96];
		snprintf(detail, sizeof(detail), "%" PRIu64 " -> %" PRIu64,
			 lsn_before, lsn_after);
		check_true("LSN strictly increases", lsn_after == lsn_before + 1, detail);

		/* Replay once more to confirm the new record really is on disk.
		 * Order matters: destroy the old journal first (it cannot be destroyed
		 * with a write in flight, and sync_insert already waited for the
		 * callback), then reopen. */
		struct s3_chunk_map *map2 = NULL;

		rc = s3_chunk_map_create(TEST_CAPACITY / 4096, 4096,
					 TEST_CHUNK_SIZE, &map2);
		if (rc == 0) {
			struct replay_ctx rctx2 = { .map = map2 };
			struct s3_journal *j2 = NULL;

			/* The map still references the journal; detach it first or the
			 * map would be left holding a dangling pointer. */
			s3_chunk_map_set_journal(map, NULL);
			s3_journal_destroy(journal);
			journal = NULL;

			if (s3_journal_open(local_dev, &j2) == 0) {
				const struct s3_super_block *sb =
					s3_local_dev_get_super(local_dev);
				struct spdk_uuid got;
				int lrc;

				sync_replay(j2, sb->checkpoint_lsn, &rctx2);

				lrc = s3_chunk_map_lookup(map2, new_chunk, &got, NULL);
				check_true("second replay sees the newly appended record",
					   lrc == 0 &&
					   spdk_uuid_compare(&got, &uuid) == 0,
					   NULL);
				journal = j2;
			}
			s3_chunk_map_destroy(map2);
		}
	}

	/* ---------- 12. wrap-around ---------- */
	/* Runs on its own device and its own four-block journal, because reaching a
	 * wrap on the 16 MiB journal above would take 262144 records. */
	test_journal_wrap(aio_path);

out_journal:
	if (journal) {
		if (map) {
			/* Drop the reference first so the map is not left holding a
			 * dangling journal pointer. */
			s3_chunk_map_set_journal(map, NULL);
		}
		s3_journal_destroy(journal);
	}
	if (map) {
		s3_chunk_map_destroy(map);
	}
out_local:
	/* The descriptor must be closed before deleting the aio bdev -- while a
	 * descriptor is still open spdk_bdev_unregister() waits, and by that point
	 * nobody is polling any more. */
	if (local_dev) {
		s3_local_dev_close(local_dev);
		local_dev = NULL;
	}
out_bdev:
	if (bdev_created) {
		struct async_ctx ctx = {0};

		bdev_aio_delete(AIO_BDEV_NAME, async_int_cb, &ctx);
		if (!poll_until(&ctx.done)) {
			fprintf(stderr, "bdev_aio_delete timed out\n");
		}
		bdev_created = false;
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
		g_thread = NULL;
	}
out_thread_lib:
	spdk_thread_lib_fini();
out_env:
	spdk_env_fini();
	spdk_log_close();

	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
