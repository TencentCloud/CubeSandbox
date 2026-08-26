/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   S3 client connectivity test
 *
 *   Verifies that lib/s3bsdev/s3_client_aws.c can really talk to S3. Flow:
 *     global_init -> client_get_or_create
 *       -> PUT                                  expect 0
 *       -> HEAD, check size                    expect 0 and matching size
 *       -> GET, byte-for-byte compare          expect 0
 *       -> GET sub-range (offset read)         expect 0 and matching content
 *       -> PUT same key with if_none_match     expect -EEXIST
 *       -> COPY to a second key + HEAD confirm expect 0
 *       -> DELETE_BATCH the two keys           expect 0
 *       -> HEAD the deleted key                expect -ENOENT
 *     -> client_put -> global_fini
 *
 *   Note: every s3_client interface is asynchronous, and the callback fires
 *   directly on a CRT I/O thread today (the "callbacks not bounced" state).
 *   So a pthread mutex + cond is used to wait here, **without bringing in an
 *   SPDK reactor** -- keeping this test a plain userspace program.
 *
 *   Credentials are read from the environment only; no secret is accepted on
 *   the command line (avoiding ps / shell history).
 *
 *   Usage:
 *     export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
 *     ./s3_client_test --endpoint s3.example.com --bucket mybucket \
 *                      [--region us-east-1] [--prefix s3lvol-test/] \
 *                      [--path-style] [--no-tls]
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"

#include "s3lvol/s3_client.h"
#include "s3lvol/s3_spawner.h"

#include <getopt.h>

#define TEST_OBJECT_SIZE  (256 * 1024)   /* 256 KiB: big enough to trigger multiple CRT chunks without being slow */
#define TEST_TIMEOUT_SEC  60

/* ==========================================================================
 * Async -> sync: a one-shot completion latch
 * ========================================================================== */

struct completion {
	pthread_mutex_t   mutex;
	pthread_cond_t    cond;
	bool              done;
	int               status;
	uint64_t          bytes;
};

static void
completion_init(struct completion *c)
{
	pthread_mutex_init(&c->mutex, NULL);
	pthread_cond_init(&c->cond, NULL);
	c->done   = false;
	c->status = 0;
	c->bytes  = 0;
}

static void
completion_fini(struct completion *c)
{
	pthread_mutex_destroy(&c->mutex);
	pthread_cond_destroy(&c->cond);
}

/* Runs on a CRT I/O thread */
static void
op_complete(void *cb_arg, int status)
{
	struct completion *c = cb_arg;

	pthread_mutex_lock(&c->mutex);
	c->status = status;
	c->done   = true;
	pthread_cond_signal(&c->cond);
	pthread_mutex_unlock(&c->mutex);
}

static void
get_complete(void *cb_arg, uint64_t bytes_read, int status)
{
	struct completion *c = cb_arg;

	pthread_mutex_lock(&c->mutex);
	c->bytes  = bytes_read;
	c->status = status;
	c->done   = true;
	pthread_cond_signal(&c->cond);
	pthread_mutex_unlock(&c->mutex);
}

/**
 * Wait for completion. Returns the status the callback carried; -ETIMEDOUT on
 * timeout.
 */
static int
completion_wait(struct completion *c)
{
	struct timespec deadline;
	int rc = 0;

	clock_gettime(CLOCK_REALTIME, &deadline);
	deadline.tv_sec += TEST_TIMEOUT_SEC;

	pthread_mutex_lock(&c->mutex);
	while (!c->done && rc == 0) {
		rc = pthread_cond_timedwait(&c->cond, &c->mutex, &deadline);
	}
	if (!c->done) {
		pthread_mutex_unlock(&c->mutex);
		fprintf(stderr, "  !! wait timed out (%d seconds) -- the callback "
			"was never invoked\n",
			TEST_TIMEOUT_SEC);
		return -ETIMEDOUT;
	}
	rc = c->status;
	pthread_mutex_unlock(&c->mutex);
	return rc;
}

/* ==========================================================================
 * Test scoreboard
 * ========================================================================== */

static int g_pass;
static int g_fail;

/* Assertions that are known not to hold on this backend, as opposed to broken
 * ones. There is exactly one so far -- COS ignores If-None-Match: * -- and it
 * needs a category of its own for a practical reason: counted as a failure it
 * makes this suite permanently red, and a suite that is always red gets ignored,
 * which costs far more than the assertion is worth.
 *
 * Not skipped, though. The check still runs and the result is still reported,
 * because "the backend started supporting it" is worth hearing about: it would
 * mean create-once could rely on the server instead of on uuid uniqueness. So an
 * unexpected pass is called out rather than quietly counted as a pass. */
static int g_xfail;
static int g_xpass;

static void
check(const char *what, int got, int want)
{
	if (got == want) {
		printf("  [PASS] %-42s rc=%d\n", what, got);
		g_pass++;
	} else {
		printf("  [FAIL] %-42s rc=%d (want %d: %s)\n",
		       what, got, want, strerror(want < 0 ? -want : want));
		g_fail++;
	}
}

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-42s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-42s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

/* Like check(), for an assertion this backend is known not to satisfy.
 *
 * \param why  what the backend does instead, printed either way -- on the
 *             expected failure so the line explains itself, and on an
 *             unexpected pass so it is obvious which assumption just changed.
 */
static void
check_xfail(const char *what, int got, int want, const char *why)
{
	if (got == want) {
		printf("  [XPASS] %-41s rc=%d -- expected to fail here; %s\n",
		       what, got, why);
		g_xpass++;
	} else {
		printf("  [xfail] %-41s rc=%d (wanted %d) -- known: %s\n",
		       what, got, want, why);
		g_xfail++;
	}
}

/* ==========================================================================
 * Synchronous wrappers
 * ========================================================================== */

static int
sync_put(struct s3_client *client, const char *key, void *data, size_t len,
	 bool if_none_match)
{
	struct completion comp;
	struct iovec iov = { .iov_base = data, .iov_len = len };
	int rc;

	completion_init(&comp);
	rc = s3_put(client, key, &iov, 1, if_none_match, op_complete, &comp);
	if (rc == 0) {
		rc = completion_wait(&comp);
	}
	completion_fini(&comp);
	return rc;
}

static int
sync_head(struct s3_client *client, const char *key, uint64_t *out_size)
{
	struct completion comp;
	int rc;

	completion_init(&comp);
	rc = s3_head(client, key, out_size, op_complete, &comp);
	if (rc == 0) {
		rc = completion_wait(&comp);
	}
	completion_fini(&comp);
	return rc;
}

static int
sync_get(struct s3_client *client, const char *key, uint64_t offset,
	 uint64_t len, void *buf, uint64_t *out_bytes)
{
	struct completion comp;
	int rc;

	completion_init(&comp);
	rc = s3_get_range(client, key, offset, len, buf, get_complete, &comp);
	if (rc == 0) {
		rc = completion_wait(&comp);
	}
	if (out_bytes) {
		*out_bytes = comp.bytes;
	}
	completion_fini(&comp);
	return rc;
}

static int
sync_copy(struct s3_client *client, const char *src_bucket,
	  const char *src_key, const char *dst_key)
{
	struct completion comp;
	int rc;

	completion_init(&comp);
	rc = s3_copy_object(client, src_bucket, src_key, dst_key,
			    op_complete, &comp);
	if (rc == 0) {
		rc = completion_wait(&comp);
	}
	completion_fini(&comp);
	return rc;
}

static int
sync_delete(struct s3_client *client, const char *key)
{
	struct completion comp;
	int rc;

	completion_init(&comp);
	rc = s3_delete(client, key, op_complete, &comp);
	if (rc == 0) {
		rc = completion_wait(&comp);
	}
	completion_fini(&comp);
	return rc;
}

static int
sync_delete_batch(struct s3_client *client, const char **keys, uint32_t count)
{
	struct completion comp;
	int rc;

	completion_init(&comp);
	rc = s3_delete_batch(client, keys, count, op_complete, &comp);
	if (rc == 0) {
		rc = completion_wait(&comp);
	}
	completion_fini(&comp);
	return rc;
}

/* ==========================================================================
 * Data fill and verification
 * ========================================================================== */

/* Reproducible pseudo-random fill: position-dependent, so it catches bugs
 * like "chunks assembled in the wrong order" -- a buffer of all zeroes or a
 * single repeated byte cannot. */
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
verify_pattern(const uint8_t *buf, size_t len, uint32_t seed, size_t skip,
	       size_t *bad_offset)
{
	uint32_t state = seed;

	for (size_t i = 0; i < skip + len; i++) {
		state = state * 1103515245u + 12345u;
		if (i >= skip) {
			uint8_t want = (uint8_t)(state >> 16);
			if (buf[i - skip] != want) {
				if (bad_offset) {
					*bad_offset = i;
				}
				return false;
			}
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
	printf("required:\n");
	printf("  --endpoint <host>   S3 endpoint (no scheme), e.g. s3.us-east-1.amazonaws.com\n");
	printf("  --bucket   <name>   bucket name\n");
	printf("options:\n");
	printf("  --region   <name>   default us-east-1\n");
	printf("  --prefix   <str>    test object key prefix, default "
	       "s3lvol-conntest/\n");
	printf("  --path-style        use path-style addressing (often needed for "
	       "MinIO / Ceph RGW)\n");
	printf("  --no-tls            disable TLS (plaintext HTTP, local testing "
	       "only)\n");
	printf("  --threads  <n>      CRT event loop thread count, default 4\n");
	printf("  -h, --help\n\n");
	printf("credentials are read from the environment only (S3_AUTH_ENV):\n");
	printf("  AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY [/ AWS_SESSION_TOKEN]\n");
}

int
main(int argc, char **argv)
{
	const char *endpoint = NULL;
	const char *bucket   = NULL;
	const char *region   = "us-east-1";
	const char *prefix   = "s3lvol-conntest/";
	bool path_style  = false;
	bool verify_tls  = true;
	uint32_t threads = 4;

	static const struct option long_opts[] = {
		{ "endpoint",   required_argument, NULL, 'e' },
		{ "bucket",     required_argument, NULL, 'b' },
		{ "region",     required_argument, NULL, 'r' },
		{ "prefix",     required_argument, NULL, 'p' },
		{ "path-style", no_argument,       NULL, 'P' },
		{ "no-tls",     no_argument,       NULL, 'T' },
		{ "threads",    required_argument, NULL, 't' },
		{ "help",       no_argument,       NULL, 'h' },
		{ NULL,         0,                 NULL,  0  },
	};
	int opt;

	while ((opt = getopt_long(argc, argv, "e:b:r:p:PTt:h", long_opts, NULL)) != -1) {
		switch (opt) {
		case 'e': endpoint = optarg; break;
		case 'b': bucket   = optarg; break;
		case 'r': region   = optarg; break;
		case 'p': prefix   = optarg; break;
		case 'P': path_style = true; break;
		case 'T': verify_tls = false; break;
		case 't': threads = (uint32_t)strtoul(optarg, NULL, 10); break;
		case 'h': usage(argv[0]); return 0;
		default:  usage(argv[0]); return 1;
		}
	}

	if (!endpoint || !bucket) {
		fprintf(stderr, "error: --endpoint and --bucket are required\n\n");
		usage(argv[0]);
		return 1;
	}

	if (!getenv("AWS_ACCESS_KEY_ID") || !getenv("AWS_SECRET_ACCESS_KEY")) {
		fprintf(stderr, "error: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY "
			"are not set\n");
		fprintf(stderr, "      this program reads credentials from the "
			"environment only; no secret on the command line.\n");
		return 1;
	}

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== S3 client connectivity test ===\n");
	printf("endpoint   : %s\n", endpoint);
	printf("bucket     : %s\n", bucket);
	printf("region     : %s\n", region);
	printf("prefix     : %s\n", prefix);
	printf("path_style : %s\n", path_style ? "yes" : "no");
	printf("tls        : %s\n", verify_tls ? "on" : "off");
	printf("object size: %d bytes\n\n", TEST_OBJECT_SIZE);

	/* ---------- 0. start the thread spawner ----------
	 *
	 * s3_crt_global_init() now refuses to initialise without a spawner --
	 * see include/s3lvol/s3_spawner.h: threads pthread_created inside CRT
	 * inherit the creator's CPU affinity, which in a real SPDK process
	 * means piling onto the reactor cores.
	 *
	 * This test is not an SPDK process and has no reactors to avoid, so it
	 * passes "every core this process can use". sched_getaffinity rather
	 * than the _SC_NPROCESSORS_CONF total: the former reflects the limits
	 * cgroup / taskset impose, the latter does not.
	 */
	printf("[0] s3_spawner_start\n");
	cpu_set_t allowed;
	CPU_ZERO(&allowed);
	if (sched_getaffinity(0, sizeof(allowed), &allowed) != 0) {
		fprintf(stderr, "sched_getaffinity failed: %s\n", strerror(errno));
		goto out_log;
	}
	int rc = s3_spawner_start(&allowed);
	check("s3_spawner_start", rc, 0);
	if (rc != 0) {
		goto out_log;
	}
	printf("     allowed cores: %d\n", CPU_COUNT(&allowed));

	/* ---------- 1. CRT global init ---------- */
	printf("[1] s3_crt_global_init\n");
	rc = s3_crt_global_init(threads);
	check("s3_crt_global_init", rc, 0);
	if (rc != 0) {
		goto out_spawner;
	}

	/* ---------- 2. create a client ---------- */
	printf("[2] s3_client_get_or_create\n");
	struct s3_target target = {
		.endpoint       = (char *)endpoint,
		.region         = (char *)region,
		.bucket         = (char *)bucket,
		.auth_mode      = S3_AUTH_ENV,
		.use_path_style = path_style,
		.verify_tls     = verify_tls,
	};
	struct s3_client *client = NULL;

	rc = s3_client_get_or_create(&target, &client);
	check("s3_client_get_or_create", rc, 0);
	if (rc != 0 || !client) {
		goto out_fini;
	}

	/* Fetching the same endpoint again should hit the pool and return the
	 * same pointer */
	struct s3_client *client2 = NULL;
	rc = s3_client_get_or_create(&target, &client2);
	check("s3_client_get_or_create (reuse)", rc, 0);
	check_true("the same endpoint returns the same client", client2 == client,
		   client2 == client ? "pool hit" : "!! not reused, pool logic "
		   "is broken");
	if (client2) {
		s3_client_put(client2);   /* drop the extra reference */
	}

	/* ---------- 3. prepare data and keys ---------- */
	uint8_t *wbuf = malloc(TEST_OBJECT_SIZE);
	uint8_t *rbuf = malloc(TEST_OBJECT_SIZE);
	if (!wbuf || !rbuf) {
		fprintf(stderr, "memory allocation failed\n");
		free(wbuf);
		free(rbuf);
		goto out_put;
	}
	fill_pattern(wbuf, TEST_OBJECT_SIZE, 0xC0FFEE);

	char key_a[512];
	char key_b[512];
	/* the pid in the key keeps concurrent runs from colliding */
	snprintf(key_a, sizeof(key_a), "%sobj-%d-a.bin", prefix, (int)getpid());
	snprintf(key_b, sizeof(key_b), "%sobj-%d-b.bin", prefix, (int)getpid());
	printf("    key A: %s\n", key_a);
	printf("    key B: %s\n\n", key_b);

	/* ---------- 4. PUT ---------- */
	printf("[3] PUT %d bytes\n", TEST_OBJECT_SIZE);
	rc = sync_put(client, key_a, wbuf, TEST_OBJECT_SIZE, false);
	check("s3_put", rc, 0);
	if (rc != 0) {
		/* If the first step does not work, everything after is noise */
		printf("\n!! PUT failed, skipping the rest. Check endpoint / "
		       "credentials / bucket permissions.\n");
		goto out_data;
	}

	/* ---------- 5. HEAD, check size ---------- */
	printf("[4] HEAD, check size\n");
	uint64_t size = 0;
	rc = sync_head(client, key_a, &size);
	check("s3_head", rc, 0);
	if (rc == 0) {
		char detail[64];
		snprintf(detail, sizeof(detail), "size=%" PRIu64, size);
		check_true("Content-Length == bytes written",
			   size == TEST_OBJECT_SIZE, detail);
	}

	/* ---------- 6. full GET, byte-by-byte compare ---------- */
	printf("[5] GET whole object and compare\n");
	uint64_t bytes = 0;
	memset(rbuf, 0, TEST_OBJECT_SIZE);
	rc = sync_get(client, key_a, 0, TEST_OBJECT_SIZE, rbuf, &bytes);
	check("s3_get_range(0, all)", rc, 0);
	if (rc == 0) {
		char detail[80];
		snprintf(detail, sizeof(detail), "bytes_read=%" PRIu64, bytes);
		check_true("bytes_read == object size",
			   bytes == TEST_OBJECT_SIZE, detail);

		size_t bad = 0;
		bool ok = verify_pattern(rbuf, TEST_OBJECT_SIZE, 0xC0FFEE, 0, &bad);
		if (ok) {
			check_true("content matches byte for byte", true,
				   "all 256 KiB correct");
		} else {
			snprintf(detail, sizeof(detail),
				 "!! wrong data at offset %zu (chunk "
				 "reassembly may be misplaced)", bad);
			check_true("content matches byte for byte", false, detail);
		}
	}

	/* ---------- 7. sub-range GET ---------- */
	/* This step specifically exercises the rebase logic in
	 * s3_get_body_callback: the range_start CRT reports is an offset within
	 * the object, not within the range. Getting it wrong fails here for
	 * sure. */
	printf("[6] GET sub-range (offset 100 KiB, length 64 KiB)\n");
	const uint64_t sub_off = 100 * 1024;
	const uint64_t sub_len = 64 * 1024;
	bytes = 0;
	memset(rbuf, 0, TEST_OBJECT_SIZE);
	rc = sync_get(client, key_a, sub_off, sub_len, rbuf, &bytes);
	check("s3_get_range(100K, 64K)", rc, 0);
	if (rc == 0) {
		char detail[96];
		snprintf(detail, sizeof(detail), "bytes_read=%" PRIu64, bytes);
		check_true("bytes_read == requested length", bytes == sub_len, detail);

		size_t bad = 0;
		bool ok = verify_pattern(rbuf, sub_len, 0xC0FFEE, sub_off, &bad);
		if (ok) {
			check_true("sub-range content matches", true,
				   "offset rebase correct");
		} else {
			snprintf(detail, sizeof(detail),
				 "!! wrong data at object offset %zu -- check "
				 "the range_start rebase",
				 bad);
			check_true("sub-range content matches", false, detail);
		}
	}

	/* ---------- 8. conditional-write probe: If-None-Match ----------
	 *
	 * This one is **expected to fail on COS** (xfail): it is a backend
	 * capability probe, not a regression test. create-once's correctness does
	 * not depend on conditional writes (blobstore names carry a uuid, and
	 * uniqueness is guaranteed by naming); see the s3_put comment in
	 * s3_client.h. It is kept so that swapping backends shows at once whether
	 * the peer supports it -- so it **still runs**, just without counting as
	 * a failure; an unexpected pass is reported as XPASS rather than quietly
	 * going green.
	 */
	printf("[7] PUT same key again (if_none_match=true) -- backend "
	       "capability probe\n");
	rc = sync_put(client, key_a, wbuf, TEST_OBJECT_SIZE, true);
	check_xfail("s3_put(if_none_match) should be -EEXIST", rc, -EEXIST,
		    "COS ignores If-None-Match: * and overwrites");
	if (rc == 0) {
		printf("       -> this backend ignores If-None-Match: * "
		       "(COS does).\n");
		printf("          That does not affect correctness: uniqueness is "
		       "guaranteed by the uuid in blobstore names,\n");
		printf("          and the conditional write is only extra "
		       "insurance.\n");
	}

	/* ---------- 9. server-side COPY ---------- */
	printf("[8] CopyObject A -> B\n");
	rc = sync_copy(client, bucket, key_a, key_b);
	check("s3_copy_object", rc, 0);
	if (rc == 0) {
		uint64_t bsize = 0;
		rc = sync_head(client, key_b, &bsize);
		check("HEAD the target key", rc, 0);
		if (rc == 0) {
			char detail[64];
			snprintf(detail, sizeof(detail), "size=%" PRIu64, bsize);
			check_true("size matches after the copy",
				   bsize == TEST_OBJECT_SIZE, detail);
		}
	}

	/* ---------- 10. HEAD a nonexistent key ---------- */
	printf("[9] HEAD of a nonexistent key should be -ENOENT\n");
	char key_missing[512];
	snprintf(key_missing, sizeof(key_missing), "%sno-such-object-%d.bin",
		 prefix, (int)getpid());
	uint64_t dummy = 0;
	rc = sync_head(client, key_missing, &dummy);
	check("s3_head(nonexistent) should be -ENOENT", rc, -ENOENT);

	/* ---------- 11. DELETE + DELETE_BATCH ---------- */
	printf("[10] cleanup: DELETE A, DELETE_BATCH [A, B]\n");
	rc = sync_delete(client, key_a);
	check("s3_delete(A)", rc, 0);

	/* A is already deleted; feeding it to the batch again exercises DELETE's
	 * idempotency in passing (GC retries) */
	const char *batch[2] = { key_a, key_b };
	rc = sync_delete_batch(client, batch, 2);
	check("s3_delete_batch([A,B])", rc, 0);

	printf("[11] HEAD of a deleted key should be -ENOENT\n");
	dummy = 0;
	rc = sync_head(client, key_a, &dummy);
	check("s3_head(deleted A) should be -ENOENT", rc, -ENOENT);
	dummy = 0;
	rc = sync_head(client, key_b, &dummy);
	check("s3_head(deleted B) should be -ENOENT", rc, -ENOENT);

	/* ---------- 11b. batch-delete fan-out behaviour ----------
	 *
	 * s3_delete_batch is a fan-out of N single-key DeleteObjects (not one
	 * DeleteObjects call), so this has to be checked specifically:
	 *   - the reference count converges exactly once under concurrent
	 *     callbacks from several CRT I/O threads
	 *   - the user callback is invoked exactly once (a second invocation
	 *     would signal the completion latch twice, or step on a freed batch)
	 *   - all-nonexistent keys also count as success (idempotent; a GC
	 *     rescan will run into them)
	 */
	printf("[11b] batch-delete fan-out: 32 keys\n");
	{
		enum { FANOUT_N = 32 };
		char  fkey[FANOUT_N][512];
		const char *fkeys[FANOUT_N];
		int put_ok = 0;

		for (int i = 0; i < FANOUT_N; i++) {
			snprintf(fkey[i], sizeof(fkey[i]), "%sfanout-%d-%02d.bin",
				 prefix, (int)getpid(), i);
			fkeys[i] = fkey[i];
		}

		/* Deliberately write only half; the other half does not exist --
		 * mixing 200 and 404 in one batch verifies that 404 is not
		 * mistaken for a failure. */
		for (int i = 0; i < FANOUT_N / 2; i++) {
			if (sync_put(client, fkeys[i], "x", 1, false) == 0) {
				put_ok++;
			}
		}
		char detail[80];
		snprintf(detail, sizeof(detail), "seeded %d objects", put_ok);
		check_true("fan-out pre-PUT", put_ok == FANOUT_N / 2, detail);

		rc = sync_delete_batch(client, fkeys, FANOUT_N);
		check("s3_delete_batch(32, half nonexistent)", rc, 0);

		/* Spot check: written and unwritten alike must be gone after the
		 * delete */
		dummy = 0;
		rc = sync_head(client, fkeys[0], &dummy);
		check("HEAD of the first key after fan-out should be -ENOENT",
		      rc, -ENOENT);
		dummy = 0;
		rc = sync_head(client, fkeys[FANOUT_N - 1], &dummy);
		check("HEAD of the last key after fan-out should be -ENOENT",
		      rc, -ENOENT);

		/* A batch where everything is already gone: DELETE is idempotent,
		 * so the whole batch should succeed */
		rc = sync_delete_batch(client, fkeys, FANOUT_N);
		check("s3_delete_batch(all already gone) should be idempotently "
		      "successful", rc, 0);
	}

	printf("[11c] argument validation\n");
	{
		/* An invalid key must be rejected before any request is
		 * submitted -- otherwise the caller would receive an error return
		 * plus callbacks for keys already in flight, with no way to tell
		 * which state it is in. */
		const char *bad_keys[3] = { key_a, NULL, key_b };
		struct completion comp;

		completion_init(&comp);
		rc = s3_delete_batch(client, bad_keys, 3, op_complete, &comp);
		check("s3_delete_batch(NULL key) should be -EINVAL", rc, -EINVAL);
		check_true("no callback on rejection", !comp.done,
			   comp.done ? "!! the callback was invoked" : "not "
			   "invoked");
		completion_fini(&comp);

		completion_init(&comp);
		rc = s3_delete_batch(client, batch, 0, op_complete, &comp);
		check("s3_delete_batch(count=0) should be -EINVAL", rc, -EINVAL);
		completion_fini(&comp);
	}

	/* ---------- 12. stats ---------- */
	printf("\n[12] s3_client_get_stats\n");
	struct s3_client_stats stats;
	s3_client_get_stats(client, &stats);
	printf("     get=%" PRIu64 " put=%" PRIu64 " head=%" PRIu64
	       " delete=%" PRIu64 " copy=%" PRIu64 "\n",
	       stats.get_ops, stats.put_ops, stats.head_ops,
	       stats.delete_ops, stats.copy_ops);
	printf("     bytes_read=%" PRIu64 " bytes_written=%" PRIu64 "\n",
	       stats.bytes_read, stats.bytes_written);
	printf("     errors_4xx=%" PRIu64 " errors_5xx=%" PRIu64
	       " inflight=%" PRIu64 "\n",
	       stats.errors_4xx, stats.errors_5xx, stats.inflight);
	check_true("all requests have converged (inflight == 0)",
		   stats.inflight == 0, NULL);

	/* ---------- 12b. CRT thread affinity ----------
	 *
	 * This is the sole reason the spawner exists; it must be verified or
	 * the change is as good as not made.
	 *
	 * Inspect each thread's allowed CPU set (/proc/<tid>/status's
	 * Cpus_allowed_list is awkward to parse, and pthread_getaffinity_np
	 * cannot reach other threads; reading the processor field of
	 * /proc/self/task/<tid>/stat is not enough either -- that is only
	 * "where it last ran"). What matters is the **size of the allowed
	 * set**: if a CRT thread inherited the reactor's single-core pin, its
	 * allowed set has exactly 1 core.
	 *
	 * This test process is not pinned, so in the normal case every thread's
	 * allowed set equals the process's full usable set. The real regression
	 * signal is "some thread is down to 1 core".
	 */
	printf("\n[12b] CRT thread affinity (the spawner's core purpose)\n");
	{
		DIR *d = opendir("/proc/self/task");
		int total = 0, pinned_single = 0, worst = INT_MAX;

		if (d) {
			struct dirent *e;
			while ((e = readdir(d)) != NULL) {
				if (e->d_name[0] == '.') {
					continue;
				}
				char path[320];
				snprintf(path, sizeof(path),
					 "/proc/self/task/%s/status", e->d_name);
				FILE *f = fopen(path, "r");
				if (!f) {
					continue;   /* the thread may have just exited */
				}
				char line[512];
				while (fgets(line, sizeof(line), f)) {
					unsigned long long mask;
					if (sscanf(line, "Cpus_allowed: %llx", &mask) == 1) {
						int n = __builtin_popcountll(mask);
						total++;
						if (n == 1) {
							pinned_single++;
						}
						if (n < worst) {
							worst = n;
						}
						break;
					}
				}
				fclose(f);
			}
			closedir(d);
		}

		char detail[128];
		snprintf(detail, sizeof(detail),
			 "%d threads, smallest allowed set %d cores",
			 total, worst == INT_MAX ? -1 : worst);
		check_true("no thread is pinned to a single core",
			   total > 0 && pinned_single == 0, detail);
		if (pinned_single > 0) {
			printf("       !! %d thread(s) have only 1 core -- CRT "
			       "threads may\n", pinned_single);
			printf("          have inherited the reactor's pin. Check "
			       "whether s3_spawner took effect.\n");
		}
	}

out_data:
	free(wbuf);
	free(rbuf);
out_put:
	printf("\n[13] s3_client_put\n");
	s3_client_put(client);
out_fini:
	printf("[14] s3_crt_global_fini\n");
	s3_crt_global_fini();
out_spawner:
	printf("[15] s3_spawner_stop\n");
	s3_spawner_stop();
out_log:
	spdk_log_close();

	/* The summary line keeps the shape every other suite here uses, because
	 * test/run_all.sh parses it. xfail and xpass are reported alongside rather
	 * than folded in: an expected failure is not a pass, and hiding it in the
	 * pass count would lose the one thing it is there to tell us. */
	if (g_xfail || g_xpass) {
		printf("\n[xfail] %d expected failure(s), %d unexpected pass(es)\n",
		       g_xfail, g_xpass);
	}
	if (g_xpass) {
		printf("        an XPASS means the backend gained a capability -- "
		       "worth acting on, not ignoring\n");
	}
	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);

	/* An unexpected pass does not fail the run. It is news, not a defect, and
	 * turning it into a red suite would push whoever sees it to delete the
	 * probe rather than look into it. */
	return g_fail == 0 ? 0 : 1;
}
