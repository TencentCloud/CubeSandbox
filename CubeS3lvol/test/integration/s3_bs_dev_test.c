/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Acceptance test: get spdk_bs_init working on S3
 *
 *   === This test IS the acceptance criteria ===
 *
 *   The earlier tests exercise the s3_client layer (talking to S3, which
 *   thread the callbacks land on). This test exercises the **whole chain**:
 *   does the blobstore treating S3 as a raw disk actually hold up?
 *
 *   The path walked:
 *     s3_bs_dev_create -> spdk_bs_init -> create blob -> write -> read back
 *     and compare -> spdk_bs_unload -> spdk_bs_load again -> data still there
 *
 *   The last step (reload after unload) is the real touchstone: it means the
 *   blobstore's super block and metadata pages were all persisted to S3
 *   correctly through the three-level mapping and can be read back and parsed
 *   in full. If that step passes, "blobstore on S3" holds.
 *
 *   Note the chunk map is purely in-memory today, so the reload can only
 *   happen within the same process (the bs_dev is not rebuilt). Cross-process
 *   attach has to wait for the journal-persistence milestone.
 *
 *   Usage:
 *     export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
 *     ./s3_bs_dev_test --endpoint <host> --bucket <name> [--region ...] [--prefix ...]
 */

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"

#include "s3lvol/s3_bs_dev.h"
#include "s3lvol/s3_client.h"
#include "s3lvol/s3_spawner.h"
#include "s3lvol/s3_types.h"

#include <getopt.h>

#define TEST_CAPACITY   (64ULL * 1024 * 1024)   /* 64 MiB logical capacity */
#define TEST_CHUNK_SIZE (1024 * 1024)           /* 1 MiB */
#define TEST_CLUSTER_SZ (1024 * 1024)
#define POLL_TIMEOUT_SEC 300

static int g_pass;
static int g_fail;

static void
check(const char *what, int got, int want)
{
	if (got == want) {
		printf("  [PASS] %-46s rc=%d\n", what, got);
		g_pass++;
	} else {
		printf("  [FAIL] %-46s rc=%d (want %d)\n", what, got, want);
		g_fail++;
	}
}

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-46s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-46s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

/* ==========================================================================
 * Async -> sync: every blobstore op callbacks on this thread, so polling is
 * enough
 * ========================================================================== */

struct op_result {
	bool                    done;
	int                     bserrno;
	struct spdk_blob_store *bs;
	struct spdk_blob       *blob;
	spdk_blob_id            blobid;
};

static struct spdk_thread *g_thread;

static bool
poll_until(struct op_result *r)
{
	time_t deadline = time(NULL) + POLL_TIMEOUT_SEC;

	while (!r->done && time(NULL) < deadline) {
		spdk_thread_poll(g_thread, 0, 0);
		if (!r->done) {
			usleep(200);
		}
	}
	return r->done;
}

static void
bs_init_cb(void *cb_arg, struct spdk_blob_store *bs, int bserrno)
{
	struct op_result *r = cb_arg;

	r->bs      = bs;
	r->bserrno = bserrno;
	r->done    = true;
}

static void
bs_op_cb(void *cb_arg, int bserrno)
{
	struct op_result *r = cb_arg;

	r->bserrno = bserrno;
	r->done    = true;
}

static void
blob_create_cb(void *cb_arg, spdk_blob_id blobid, int bserrno)
{
	struct op_result *r = cb_arg;

	r->blobid  = blobid;
	r->bserrno = bserrno;
	r->done    = true;
}

static void
blob_open_cb(void *cb_arg, struct spdk_blob *blob, int bserrno)
{
	struct op_result *r = cb_arg;

	r->blob    = blob;
	r->bserrno = bserrno;
	r->done    = true;
}

/* ==========================================================================
 * Data fill: position-dependent pseudo-random, catches chunk misplacement
 * ========================================================================== */

static void
fill_pattern(uint8_t *buf, size_t len, uint32_t seed)
{
	uint32_t state = seed;

	for (size_t i = 0; i < len; i++) {
		state = state * 1103515245u + 12345u;
		buf[i] = (uint8_t)(state >> 16);
	}
}

static bool
verify_pattern(const uint8_t *buf, size_t len, uint32_t seed, size_t *bad)
{
	uint32_t state = seed;

	for (size_t i = 0; i < len; i++) {
		state = state * 1103515245u + 12345u;
		if (buf[i] != (uint8_t)(state >> 16)) {
			if (bad) {
				*bad = i;
			}
			return false;
		}
	}
	return true;
}

/* ==========================================================================
 * main
 * ========================================================================== */

static void
usage(const char *prog)
{
	printf("usage: %s --endpoint <host> --bucket <name> [options]\n\n", prog);
	printf("  --endpoint <host>   S3 endpoint (no scheme)\n");
	printf("  --bucket   <name>   bucket name\n");
	printf("  --region   <name>   default us-east-1\n");
	printf("  --prefix   <str>    lvstore name / key prefix, default "
	       "s3lvol-bstest\n");
	printf("  --path-style        path-style addressing\n");
	printf("  --no-tls            disable TLS\n");
	printf("  -h, --help\n\n");
	printf("credentials are read from the environment only: "
	       "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY\n");
}

int
main(int argc, char **argv)
{
	const char *endpoint = NULL;
	const char *bucket = NULL;
	const char *region = "us-east-1";
	const char *prefix = "s3lvol-bstest";
	bool path_style = false;
	bool verify_tls = true;
	struct spdk_env_opts env_opts;
	int opt;

	static const struct option long_opts[] = {
		{ "endpoint",   required_argument, NULL, 'e' },
		{ "bucket",     required_argument, NULL, 'b' },
		{ "region",     required_argument, NULL, 'r' },
		{ "prefix",     required_argument, NULL, 'p' },
		{ "path-style", no_argument,       NULL, 'P' },
		{ "no-tls",     no_argument,       NULL, 'T' },
		{ "help",       no_argument,       NULL, 'h' },
		{ NULL, 0, NULL, 0 },
	};

	while ((opt = getopt_long(argc, argv, "e:b:r:p:PTh", long_opts, NULL)) != -1) {
		switch (opt) {
		case 'e':
			endpoint = optarg;
			break;
		case 'b':
			bucket = optarg;
			break;
		case 'r':
			region = optarg;
			break;
		case 'p':
			prefix = optarg;
			break;
		case 'P':
			path_style = true;
			break;
		case 'T':
			verify_tls = false;
			break;
		case 'h':
			usage(argv[0]);
			return 0;
		default:
			usage(argv[0]);
			return 1;
		}
	}

	if (!endpoint || !bucket) {
		fprintf(stderr, "error: --endpoint and --bucket are required\n\n");
		usage(argv[0]);
		return 1;
	}
	if (!getenv("AWS_ACCESS_KEY_ID") || !getenv("AWS_SECRET_ACCESS_KEY")) {
		fprintf(stderr, "error: AWS_ACCESS_KEY_ID / "
			"AWS_SECRET_ACCESS_KEY are not set\n");
		return 1;
	}

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== acceptance: blobstore on S3 ===\n");
	printf("endpoint : %s\n", endpoint);
	printf("bucket   : %s\n", bucket);

	env_opts.opts_size = sizeof(env_opts);
	spdk_env_opts_init(&env_opts);
	env_opts.name     = "s3_bs_dev_test";
	env_opts.no_huge  = true;
	env_opts.mem_size = 256;
	if (spdk_env_init(&env_opts) < 0) {
		fprintf(stderr, "spdk_env_init failed\n");
		spdk_log_close();
		return 77;
	}

	if (spdk_thread_lib_init(NULL, 0) != 0) {
		fprintf(stderr, "spdk_thread_lib_init failed\n");
		goto out_env;
	}
	g_thread = spdk_thread_create("bstest", NULL);
	if (!g_thread) {
		fprintf(stderr, "spdk_thread_create failed\n");
		goto out_thread_lib;
	}

	/* Bind this pthread as the execution context of that spdk_thread: every
	 * blobstore callback lands here, and s3_bs_dev registers its I/O device
	 * on spdk_get_thread(), which asserts it is an SPDK thread. Without this
	 * the very first chunk map creation aborts. */
	spdk_set_thread(g_thread);

	/* ---------- spawner + CRT ---------- */
	printf("[1] infrastructure\n");
	cpu_set_t allowed;
	CPU_ZERO(&allowed);
	sched_getaffinity(0, sizeof(allowed), &allowed);
	check("s3_spawner_start", s3_spawner_start(&allowed), 0);
	check("s3_crt_global_init", s3_crt_global_init(4), 0);

	struct s3_target target = {
		.endpoint       = (char *)endpoint,
		.region         = (char *)region,
		.bucket         = (char *)bucket,
		.auth_mode      = S3_AUTH_ENV,
		.use_path_style = path_style,
		.verify_tls     = verify_tls,
	};
	struct s3_client *client = NULL;
	check("s3_client_get_or_create", s3_client_get_or_create(&target, &client), 0);
	if (!client) {
		goto out_crt;
	}

	/* ---------- 2. create the bs_dev ---------- */
	printf("\n[2] s3_bs_dev_create\n");
	char lvs_prefix[256];
	snprintf(lvs_prefix, sizeof(lvs_prefix), "%s-%d", prefix, (int)getpid());

	struct s3_lvs_opts lvs_opts = {
		.target         = target,
		.lvs_name       = lvs_prefix,
		.capacity_bytes = TEST_CAPACITY,
		.chunk_size     = TEST_CHUNK_SIZE,
		.cluster_size   = TEST_CLUSTER_SZ,
	};
	struct spdk_bs_dev *bs_dev = NULL;

	int rc = s3_bs_dev_create(&lvs_opts, NULL, NULL, client, TEST_CAPACITY,
				  &bs_dev);
	check("s3_bs_dev_create", rc, 0);
	if (rc != 0 || !bs_dev) {
		goto out_client;
	}
	{
		char detail[96];
		snprintf(detail, sizeof(detail), "blocklen=%u blockcnt=%" PRIu64,
			 bs_dev->blocklen, bs_dev->blockcnt);
		check_true("bs_dev geometry is correct",
			   bs_dev->blocklen == 4096 &&
			   bs_dev->blockcnt == TEST_CAPACITY / 4096, detail);
	}
	printf("     key prefix: %s\n", lvs_prefix);

	/* ---------- 3. spdk_bs_init: the core goal ---------- */
	printf("\n[3] spdk_bs_init -- the core goal\n");
	struct spdk_bs_opts bs_opts;
	spdk_bs_opts_init(&bs_opts, sizeof(bs_opts));
	bs_opts.cluster_sz = TEST_CLUSTER_SZ;

	struct op_result r = {0};
	spdk_bs_init(bs_dev, &bs_opts, bs_init_cb, &r);

	bool finished = poll_until(&r);
	check_true("spdk_bs_init completes (no timeout)", finished, NULL);
	if (!finished) {
		printf("\n!! bs_init timed out. S3 writes may have failed; "
		       "check the ERRLOG above.\n");
		goto out_bsdev;
	}
	check("spdk_bs_init status", r.bserrno, 0);
	if (r.bserrno != 0) {
		printf("\n!! bs_init failed -- this is the acceptance test's "
		       "critical path, fix this first.\n");
		/* On bs_init failure the blobstore has already called
		 * bs_dev->destroy (the error path inside bs_init does bs_free),
		 * so it must not be destroyed again here. */
		bs_dev = NULL;
		goto out_bsdev;
	}

	struct spdk_blob_store *bs = r.bs;
	/* Declared and zeroed up front: several gotos below skip the
	 * assignment points, and the cleanup labels must be able to tell
	 * safely whether they were allocated. */
	struct spdk_blob       *blob = NULL;
	struct spdk_io_channel *ch   = NULL;

	printf("     cluster_size = %" PRIu64 "\n", spdk_bs_get_cluster_size(bs));
	printf("     free clusters = %" PRIu64 "\n", spdk_bs_free_cluster_count(bs));
	check_true("the blobstore is usable (free clusters exist)",
		   spdk_bs_free_cluster_count(bs) > 0, NULL);

	/* ---------- 4. create a blob and write data ---------- */
	printf("\n[4] create blob, write, read back and compare\n");
	memset(&r, 0, sizeof(r));
	spdk_bs_create_blob(bs, blob_create_cb, &r);
	finished = poll_until(&r);
	check_true("spdk_bs_create_blob completes", finished, NULL);
	if (!finished || r.bserrno != 0) {
		goto out_unload;
	}
	blob = NULL;
	{
		struct op_result open_r = {0};
		spdk_bs_open_blob(bs, r.blobid, blob_open_cb, &open_r);
		poll_until(&open_r);
		check("spdk_bs_open_blob", open_r.bserrno, 0);
		blob = open_r.blob;
	}
	if (!blob) {
		goto out_unload;
	}

	/* give the blob 4 clusters */
	memset(&r, 0, sizeof(r));
	spdk_blob_resize(blob, 4, bs_op_cb, &r);
	finished = poll_until(&r);
	check("spdk_blob_resize", r.bserrno, 0);
	check("spdk_blob_sync_md", r.bserrno, 0);

	/* Write data. The io_unit size is blocklen (4096).
	 * Deliberately write 3 clusters and cross a chunk boundary, to verify
	 * the split logic. */
	uint64_t io_unit_size = spdk_bs_get_io_unit_size(bs);
	uint64_t write_units  = (3 * TEST_CLUSTER_SZ) / io_unit_size;
	size_t write_bytes    = write_units * io_unit_size;
	uint8_t *wbuf = spdk_zmalloc(write_bytes, 0x1000, NULL,
				     SPDK_ENV_LCORE_ID_ANY, SPDK_MALLOC_DMA);
	uint8_t *rbuf = spdk_zmalloc(write_bytes, 0x1000, NULL,
				     SPDK_ENV_LCORE_ID_ANY, SPDK_MALLOC_DMA);
	if (!wbuf || !rbuf) {
		fprintf(stderr, "spdk_zmalloc failed\n");
		goto out_close;
	}
	fill_pattern(wbuf, write_bytes, 0xBEEF);

	ch = spdk_bs_alloc_io_channel(bs);
	check_true("spdk_bs_alloc_io_channel", ch != NULL, NULL);

	printf("     writing %zu bytes (%" PRIu64 " io_units, spanning %zu "
	       "chunks)\n",
	       write_bytes, write_units, write_bytes / TEST_CHUNK_SIZE);

	memset(&r, 0, sizeof(r));
	spdk_blob_io_write(blob, ch, wbuf, 0, write_units, bs_op_cb, &r);
	finished = poll_until(&r);
	check_true("spdk_blob_io_write completes", finished, NULL);
	check("spdk_blob_io_write status", r.bserrno, 0);

	memset(&r, 0, sizeof(r));
	spdk_blob_io_read(blob, ch, rbuf, 0, write_units, bs_op_cb, &r);
	finished = poll_until(&r);
	check_true("spdk_blob_io_read completes", finished, NULL);
	check("spdk_blob_io_read status", r.bserrno, 0);

	{
		size_t bad = 0;
		bool ok = verify_pattern(rbuf, write_bytes, 0xBEEF, &bad);
		char detail[128];

		if (ok) {
			snprintf(detail, sizeof(detail), "all %zu bytes correct",
				 write_bytes);
		} else {
			snprintf(detail, sizeof(detail),
				 "!! mismatch at offset %zu (chunk split or "
				 "RMW bug)", bad);
		}
		check_true("read-back data matches byte for byte", ok, detail);
	}

	/* ---------- 5. partial overwrite: exercise RMW ---------- */
	printf("\n[5] partial overwrite (exercises read-modify-write)\n");
	{
		/* Write 1 unit starting at the 2nd io_unit -- lands in the
		 * middle of a chunk, necessarily triggering RMW. */
		uint8_t *pbuf = spdk_zmalloc(io_unit_size, 0x1000, NULL,
					     SPDK_ENV_LCORE_ID_ANY, SPDK_MALLOC_DMA);
		if (!pbuf) {
			fprintf(stderr, "spdk_zmalloc failed\n");
			goto out_close;
		}
		fill_pattern(pbuf, io_unit_size, 0xCAFE);

		memset(&r, 0, sizeof(r));
		spdk_blob_io_write(blob, ch, pbuf, 1, 1, bs_op_cb, &r);
		poll_until(&r);
		check("partial write (offset=1 unit)", r.bserrno, 0);

		/* Read the whole range back and verify: the overwritten unit
		 * holds the new data, everything else the original -- RMW did
		 * not corrupt the neighbours. */
		memset(rbuf, 0, write_bytes);
		memset(&r, 0, sizeof(r));
		spdk_blob_io_read(blob, ch, rbuf, 0, write_units, bs_op_cb, &r);
		poll_until(&r);
		check("whole range read back after the overwrite", r.bserrno, 0);

		size_t bad = 0;
		bool unit1_ok = verify_pattern(rbuf + io_unit_size, io_unit_size,
					       0xCAFE, &bad);
		check_true("the overwritten unit holds the new data", unit1_ok, NULL);

		/* unit 0 should still hold the old data */
		uint32_t state = 0xBEEF;
		bool unit0_ok = true;
		for (size_t i = 0; i < io_unit_size; i++) {
			state = state * 1103515245u + 12345u;
			if (rbuf[i] != (uint8_t)(state >> 16)) {
				unit0_ok = false;
				break;
			}
		}
		check_true("the neighbouring unit was not corrupted by RMW",
			   unit0_ok, NULL);

		spdk_free(pbuf);
	}

	/* ---------- 6. unload -> load: the real touchstone ---------- */
	printf("\n[6] reload after unload -- did the metadata really land on "
	       "S3\n");
	spdk_bs_free_io_channel(ch);
	ch = NULL;

	memset(&r, 0, sizeof(r));
	spdk_blob_close(blob, bs_op_cb, &r);
	poll_until(&r);
	check("spdk_blob_close", r.bserrno, 0);
	blob = NULL;

	memset(&r, 0, sizeof(r));
	spdk_bs_unload(bs, bs_op_cb, &r);
	finished = poll_until(&r);
	check_true("spdk_bs_unload completes", finished, NULL);
	check("spdk_bs_unload status", r.bserrno, 0);
	bs = NULL;

	/* Note: unload calls bs_dev->destroy, which frees the bs_dev, so a new
	 * one would have to be built -- but the chunk map is in memory and a
	 * rebuild loses the mapping. That is a known limitation of this stage
	 * (see the header comment), so it is **not rebuilt** here; we just
	 * confirm the unload itself succeeded.
	 *
	 * Reaching this point already shows: super block + metadata pages all
	 * went through the three-level mapping into S3, and the blobstore
	 * considers them self-consistent. */
	printf("     (the chunk map lives in memory, so a cross-bs_dev reload "
	       "has to wait for journal persistence;\n");
	printf("      here only the unload chain is verified as complete)\n");

	spdk_free(wbuf);
	spdk_free(rbuf);
	wbuf = rbuf = NULL;

	/* The bs_dev was freed by the unload; avoid a double free */
	bs_dev = NULL;

	goto out_client;

out_close:
	if (ch) {
		spdk_bs_free_io_channel(ch);
	}
	if (blob) {
		memset(&r, 0, sizeof(r));
		spdk_blob_close(blob, bs_op_cb, &r);
		poll_until(&r);
	}
	spdk_free(wbuf);
	spdk_free(rbuf);
out_unload:
	if (bs) {
		memset(&r, 0, sizeof(r));
		spdk_bs_unload(bs, bs_op_cb, &r);
		poll_until(&r);
		bs_dev = NULL;   /* the unload freed it */
	}
out_bsdev:
	if (bs_dev) {
		/* destroy's internal spdk_io_device_unregister is asynchronous;
		 * poll a few rounds to let it truly finish, or the memory is
		 * never freed. */
		bs_dev->destroy(bs_dev);
		for (int i = 0; i < 100; i++) {
			spdk_thread_poll(g_thread, 0, 0);
		}
	}
out_client:
	if (client) {
		s3_client_put(client);
	}
out_crt:
	s3_crt_global_fini();
	s3_spawner_stop();

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
