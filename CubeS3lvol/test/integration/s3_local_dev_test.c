/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Super block validation check -- needs a bdev, no S3, no credentials
 *
 *   === What this test proves ===
 *
 *   s3_local_dev_open() is the gate every attach passes through, and the only
 *   thing standing between a corrupt local disk and the journal replay that
 *   would write foreign data into a live lvstore. Its four rejections -- bad
 *   magic, bad CRC, unsupported version, and a dual-bdev layout mismatch --
 *   each pair a *specific* error code with a *specific* on-disk corruption, so
 *   an operator error and a flipped bit are told apart rather than both turning
 *   into "the disk looks wrong".
 *
 *   The rejections live in an I/O callback, so they only run against a real
 *   device. The test formats an aio bdev, plants each corruption by rewriting
 *   the super block's bytes directly on the backing file, reopens, and asserts
 *   the exact errno. A corrupt disk that is *accepted* would not fail the open;
 *   it would fail days later as a replay that mistook someone else's mappings
 *   for this lvstore's.
 *
 *   === How the corruption is planted ===
 *
 *   The super block is the first 4 KiB of the backing file. The test reads it
 *   straight off the filesystem (buffered, so it is independent of the aio
 *   module), flips the field under test, fsyncs, then lets s3_local_dev_open()
 *   read it back through the bdev. A direct-write to the file rather than
 *   through any s3lvol API is the point: corruption is an outside force, not a
 *   path the module itself takes.
 *
 *   Usage:
 *     ./s3_local_dev_test [aio-file-path]
 */

#include "spdk/stdinc.h"
#include "spdk/accel.h"
#include "spdk/bdev.h"
#include "spdk/crc32.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"

#include "s3lvol/s3_local_dev.h"

#include "bdev/aio/bdev_aio.h"

#define AIO_BDEV_NAME    "s3lvol_sb0"
#define AIO_BLOCK_SIZE   4096
#define AIO_FILE_SIZE    (128ULL * 1024 * 1024)
#define DEFAULT_AIO_PATH "/data/s3lvol_local_dev_test.aio"

static int g_pass;
static int g_fail;

static struct spdk_thread *g_thread;

static void
check_int(const char *what, int got, int want)
{
	if (got == want) {
		printf("  [PASS] %-50s rc=%d\n", what, got);
		g_pass++;
	} else {
		printf("  [FAIL] %-50s rc=%d (want %d)\n", what, got, want);
		g_fail++;
	}
}

/* ==========================================================================
 * Async -> sync (single thread, every callback runs on it)
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

struct open_ctx {
	bool               done;
	int                status;
	struct s3_local_dev *dev;
};

static void
local_dev_open_cb(void *cb_arg, struct s3_local_dev *dev, int status)
{
	struct open_ctx *ctx = cb_arg;

	ctx->dev    = dev;
	ctx->status = status;
	ctx->done   = true;
}

/* ==========================================================================
 * aio backing file + framework
 * ========================================================================== */

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
		close(fd);
		unlink(path);
		return rc;
	}
	close(fd);
	return 0;
}

static int
framework_start(void)
{
	struct spdk_iobuf_opts iobuf_opts;
	struct spdk_bdev_opts bdev_opts;
	struct async_ctx ctx = {0};
	int rc;

	spdk_iobuf_get_opts(&iobuf_opts, sizeof(iobuf_opts));
	iobuf_opts.small_pool_count = 1024;
	iobuf_opts.large_pool_count = 64;
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

/* ==========================================================================
 * Reading and rewriting the super block straight off the backing file
 * ========================================================================== */

/* Same CRC the module computes: crc field zeroed, then CRC32C with ~0u seed.
 * Copied rather than exported because the module keeps it private -- and a test
 * that reproduced the CRC from the on-disk header instead of recomputing it
 * would be checking nothing. */
static uint32_t
super_crc(const struct s3_super_block *sb)
{
	struct s3_super_block tmp;

	memcpy(&tmp, sb, sizeof(tmp));
	tmp.crc = 0;
	return spdk_crc32c_update(&tmp, sizeof(tmp), ~0u);
}

/* Overwrite the super block (the first 4 KiB) with `sb` and fsync it.
 *
 * The struct is only ~200 bytes but the on-disk block is a full 4 KiB; the
 * struct is copied into a zeroed 4 KiB buffer so the rest of the block is
 * cleared rather than carrying whatever the previous tenant left. */
static int
rewrite_super(const char *aio_path, const struct s3_super_block *sb)
{
	uint8_t buf[S3_SUPER_SIZE];
	int fd;
	ssize_t n;

	memset(buf, 0, sizeof(buf));
	memcpy(buf, sb, sizeof(*sb));

	fd = open(aio_path, O_RDWR);
	if (fd < 0) {
		return -errno;
	}
	n = pwrite(fd, buf, S3_SUPER_SIZE, 0);
	if (n != (ssize_t)S3_SUPER_SIZE) {
		close(fd);
		return -EIO;
	}
	if (fsync(fd) != 0) {
		close(fd);
		return -errno;
	}
	close(fd);
	return 0;
}

static int
read_super(const char *aio_path, struct s3_super_block *sb)
{
	uint8_t buf[S3_SUPER_SIZE];
	int fd;
	ssize_t n;

	fd = open(aio_path, O_RDONLY);
	if (fd < 0) {
		return -errno;
	}
	n = pread(fd, buf, S3_SUPER_SIZE, 0);
	close(fd);
	if (n != (ssize_t)S3_SUPER_SIZE) {
		return -EIO;
	}
	memcpy(sb, buf, sizeof(*sb));
	return 0;
}

/* Open the device and report the status. On success the dev is closed again
 * immediately -- the corruption cases assert on the error, not on a usable dev. */
static int
try_open(const char *wal_name)
{
	struct open_ctx ctx = {0};

	s3_local_dev_open(wal_name, NULL, local_dev_open_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	if (ctx.status == 0 && ctx.dev) {
		s3_local_dev_close(ctx.dev);
	}
	return ctx.status;
}

int
main(int argc, char **argv)
{
	struct spdk_env_opts env_opts;
	const char *aio_path = DEFAULT_AIO_PATH;
	struct s3_super_block orig;
	bool file_created = false;
	bool bdev_created = false;
	bool framework_up = false;
	int rc;

	if (argc > 1) {
		aio_path = argv[1];
	}

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== super block validation check ===\n\n");

	env_opts.opts_size = sizeof(env_opts);
	spdk_env_opts_init(&env_opts);
	env_opts.name     = "s3_local_dev_test";
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
	g_thread = spdk_thread_create("sb_test", NULL);
	if (!g_thread) {
		goto out_thread_lib;
	}
	spdk_set_thread(g_thread);

	/* ---------- [0] framework up ---------- */
	printf("[0] bringing up the SPDK framework\n");
	rc = framework_start();
	check_int("framework_start", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	framework_up = true;

	/* ---------- [1] backing file + bdev ---------- */
	printf("\n[1] aio bdev\n");
	rc = make_aio_file(aio_path, AIO_FILE_SIZE);
	check_int("create the backing file", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	file_created = true;

	rc = create_aio_bdev(AIO_BDEV_NAME, aio_path, AIO_BLOCK_SIZE,
			     false, false, NULL, false);
	check_int("create_aio_bdev", rc, 0);
	if (rc != 0) {
		goto out_file;
	}
	bdev_created = true;

	/* ---------- [2] format and reopen: the honest round trip ---------- */
	printf("\n[2] format, close, reopen\n");
	{
		struct open_ctx octx = {0};
		struct s3_local_dev_format_opts fopts = {
			.wal_bdev_name  = AIO_BDEV_NAME,
			.lvs_name       = "sb_test",
			.capacity_bytes = 256ULL * 1024 * 1024,
			.chunk_size     = 1024 * 1024,
			.journal_size   = 16 * 1024 * 1024,
			.wal_size       = 64 * 1024 * 1024,
		};

		s3_local_dev_format(&fopts, local_dev_open_cb, &octx);
		if (!poll_until(&octx.done)) {
			goto out_bdev;
		}
		check_int("s3_local_dev_format", octx.status, 0);
		if (octx.status != 0) {
			goto out_bdev;
		}
		s3_local_dev_close(octx.dev);
	}

	rc = try_open(AIO_BDEV_NAME);
	check_int("a freshly formatted disk reopens cleanly", rc, 0);

	/* ---------- [3] the four rejections ---------- */
	printf("\n[3] planting corruption and expecting the exact refusal\n");
	rc = read_super(aio_path, &orig);
	check_int("read the pristine super block back", rc, 0);
	if (rc != 0) {
		goto out_bdev;
	}

	{
		struct s3_super_block sb;

		/* (a) bad magic -- the disk never held this layout. */
		memcpy(&sb, &orig, sizeof(sb));
		sb.magic = 0xDEADBEEFDEADBEEFULL;
		rc = rewrite_super(aio_path, &sb);
		check_int("plant a bad magic", rc, 0);
		if (rc == 0) {
			check_int("bad magic is refused with -EILSEQ",
				  try_open(AIO_BDEV_NAME), -EILSEQ);
		}

		/* (b) CRC mismatch -- a bit flipped anywhere, magic untouched. */
		memcpy(&sb, &orig, sizeof(sb));
		sb.capacity_bytes ^= (1ULL << 40);   /* flip a bit far from magic */
		rc = rewrite_super(aio_path, &sb);
		check_int("plant a bad CRC", rc, 0);
		if (rc == 0) {
			check_int("a CRC mismatch is refused with -EILSEQ",
				  try_open(AIO_BDEV_NAME), -EILSEQ);
		}

		/* (c) unsupported version -- CRC recomputed, so only the version
		 * check can fire. */
		memcpy(&sb, &orig, sizeof(sb));
		sb.version = 9999;
		sb.crc = super_crc(&sb);
		rc = rewrite_super(aio_path, &sb);
		check_int("plant an unsupported version", rc, 0);
		if (rc == 0) {
			check_int("an unsupported version is refused with -EPROTO",
				  try_open(AIO_BDEV_NAME), -EPROTO);
		}

		/* (d) dual-bdev mismatch -- the disk says cache is on its own device
		 * but the caller passed none. CRC recomputed so only this fires. */
		memcpy(&sb, &orig, sizeof(sb));
		sb.dual_bdev = 1;
		sb.crc = super_crc(&sb);
		rc = rewrite_super(aio_path, &sb);
		check_int("plant a dual-bdev marker", rc, 0);
		if (rc == 0) {
			check_int("a dual-bdev layout with no cache bdev is refused "
				  "with -EINVAL",
				  try_open(AIO_BDEV_NAME), -EINVAL);
		}

		/* Restore the pristine block and confirm the rejections were about
		 * the planted bytes, not the test corrupting something else. */
		rc = rewrite_super(aio_path, &orig);
		check_int("restore the pristine super block", rc, 0);
		if (rc == 0) {
			check_int("and it reopens cleanly again",
				  try_open(AIO_BDEV_NAME), 0);
		}
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
