/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * Template preheat against a real shared object cache.
 *
 * S3 GETs are stubbed and stay outstanding until the test completes them, so
 * several imports can be observed sharing one admission window. The cache
 * itself is the production object lane on an aio bdev: after preheat drains,
 * concurrent reads of those objects must be served from the local device and
 * must not issue another GET.
 *
 * Each export stands in for one imported template. The import RPC is not
 * linked here; what this runs is the dataplane that RPC starts.
 */

#include "spdk/stdinc.h"
#include "spdk/accel.h"
#include "spdk/bdev.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_cache.h"
#include "s3lvol/s3_chunk_map.h"
#include "s3lvol/s3_client.h"
#include "s3lvol/s3_export.h"

#include "bdev/aio/bdev_aio.h"

#define AIO_BDEV_NAME   "s3lvol_preheat0"
#define AIO_BLOCK_SIZE  4096
#define DEFAULT_AIO_PATH "/data/s3lvol_preheat_test.aio"
#define AIO_FILE_SIZE   (8ULL * 1024 * 1024)
#define REGION_OFF      (1ULL * 1024 * 1024)
#define CHUNK_SIZE      (64 * 1024)
#define N_SLOTS         64
#define REGION_SIZE     ((uint64_t)N_SLOTS * CHUNK_SIZE)
#define N_CHUNKS        12
#define N_EXPORTS       4
#define N_READS         8
#define MAX_GETS        128
#define POLL_TIMEOUT_SEC 10
#define ENDPOINT        "cos.example.test"
#define BUCKET          "preheat-bucket"

struct fake_get {
	char key[S3_EXPORT_KEY_MAX];
	uint64_t len;
	void *buf;
	s3_get_cb cb;
	void *cb_arg;
	bool done;
};

struct read_slot {
	bool done;
	int status;
};

struct export_case {
	struct spdk_bs_dev *dev;
	struct spdk_io_channel *ch;
	char prefix[32];
	struct spdk_uuid uuids[N_CHUNKS];
};

static struct fake_get g_gets[MAX_GETS];
static uint32_t g_ngets;
static struct spdk_thread *g_thread;
static int g_pass;
static int g_fail;

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-62s%s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-62s%s\n", what, detail ? detail : "");
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

static uint64_t
now_ms(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static void
poll_for_ms(uint64_t ms)
{
	uint64_t deadline = now_ms() + ms;

	while (now_ms() < deadline) {
		spdk_thread_poll(g_thread, 0, 0);
	}
}

static bool
poll_until_reads(struct read_slot *slots, uint32_t n)
{
	uint64_t deadline = now_ms() + POLL_TIMEOUT_SEC * 1000;

	while (now_ms() < deadline) {
		uint32_t i;
		bool all = true;

		spdk_thread_poll(g_thread, 0, 0);
		for (i = 0; i < n; i++) {
			if (!slots[i].done) {
				all = false;
				break;
			}
		}
		if (all) {
			return true;
		}
	}
	fprintf(stderr, "!! timed out waiting for reads\n");
	return false;
}

/* The bytes a completed GET stored, derived from the object key so a later
 * read can be checked without a side table. */
static void
paint(void *buf, const char *key, uint32_t len)
{
	uint8_t *p = buf;
	uint32_t i;
	uint8_t salt = 0;

	for (i = 0; key[i] != '\0'; i++) {
		salt = (uint8_t)(salt * 33 + (uint8_t)key[i]);
	}
	for (i = 0; i < len; i++) {
		p[i] = (uint8_t)(salt + i * 17);
	}
}

int
s3_get_range(struct s3_client *client, const char *key, uint64_t offset,
	     uint64_t len, void *buf, s3_get_cb cb, void *cb_arg)
{
	struct fake_get *get;

	(void)client;
	(void)offset;
	if (g_ngets == MAX_GETS || len > UINT32_MAX) {
		return -ENOSPC;
	}
	get = &g_gets[g_ngets++];
	snprintf(get->key, sizeof(get->key), "%s", key);
	get->len = len;
	get->buf = buf;
	get->cb = cb;
	get->cb_arg = cb_arg;
	get->done = false;
	return 0;
}

int
s3_head(struct s3_client *client, const char *key, uint64_t *size,
	s3_op_cb cb, void *cb_arg)
{
	(void)client;
	(void)key;
	(void)size;
	(void)cb;
	(void)cb_arg;
	return -ENOTSUP;
}

int
s3_put(struct s3_client *client, const char *key, struct iovec *iov, int iovcnt,
       bool if_none_match, s3_op_cb cb, void *cb_arg)
{
	(void)client;
	(void)key;
	(void)iov;
	(void)iovcnt;
	(void)if_none_match;
	(void)cb;
	(void)cb_arg;
	return -ENOTSUP;
}

void
s3_client_put(struct s3_client *client)
{
	(void)client;
}

const char *
s3_client_bucket(const struct s3_client *client)
{
	(void)client;
	return BUCKET;
}

static void
complete_get(uint32_t index)
{
	struct fake_get *get = &g_gets[index];

	if (get->done) {
		return;
	}
	get->done = true;
	paint(get->buf, get->key, (uint32_t)get->len);
	get->cb(get->cb_arg, get->len, 0);
}

/* Completes the GETs that exist now. A completion may admit the next object,
 * and that new GET stays outstanding for the caller to observe or drain. */
static void
complete_outstanding(void)
{
	uint32_t n = g_ngets;
	uint32_t i;

	for (i = 0; i < n; i++) {
		complete_get(i);
	}
}

static bool
gets_pending(void)
{
	uint32_t i;

	for (i = 0; i < g_ngets; i++) {
		if (!g_gets[i].done) {
			return true;
		}
	}
	return false;
}

static bool
drain_preheat(struct s3_cache *cache)
{
	uint64_t deadline = now_ms() + POLL_TIMEOUT_SEC * 1000;

	while (now_ms() < deadline) {
		uint32_t before = g_ngets;

		if (gets_pending()) {
			complete_outstanding();
		}
		poll_for_ms(20);
		if (!gets_pending() && g_ngets == before &&
		    s3_cache_is_quiesced(cache)) {
			return true;
		}
	}
	fprintf(stderr, "!! timed out draining preheat (%u GETs)\n", g_ngets);
	return false;
}

static uint32_t
count_prefix(const char *prefix, uint32_t begin, uint32_t end)
{
	size_t len = strlen(prefix);
	uint32_t n = 0;
	uint32_t i;

	for (i = begin; i < end && i < g_ngets; i++) {
		if (strncmp(g_gets[i].key, prefix, len) == 0) {
			n++;
		}
	}
	return n;
}

static void
read_done(struct spdk_io_channel *channel, void *arg, int bserrno)
{
	struct read_slot *slot = arg;

	(void)channel;

	slot->status = bserrno;
	slot->done = true;
}

struct async_ctx {
	bool done;
	int status;
};

static void
async_int_cb(void *cb_arg, int rc)
{
	struct async_ctx *ctx = cb_arg;

	ctx->status = rc;
	ctx->done = true;
}

static void
async_void_cb(void *cb_arg)
{
	struct async_ctx *ctx = cb_arg;

	ctx->status = 0;
	ctx->done = true;
}

static bool
poll_until(bool *done)
{
	uint64_t deadline = now_ms() + POLL_TIMEOUT_SEC * 1000;

	while (!*done && now_ms() < deadline) {
		spdk_thread_poll(g_thread, 0, 0);
	}
	return *done;
}

static void
bdev_event_cb(enum spdk_bdev_event_type type, struct spdk_bdev *bdev,
	      void *event_ctx)
{
	(void)bdev;
	(void)event_ctx;
	printf("  (unexpected bdev event %d)\n", type);
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

static int
make_export(struct s3_cache *cache, struct s3_client *client,
	    unsigned index, struct export_case *out)
{
	struct s3_export_manifest *m = NULL;
	char uuid_str[SPDK_UUID_STRING_LEN];
	uint8_t src_idx = 0;
	uint32_t chunk;
	int rc;

	memset(out, 0, sizeof(*out));
	snprintf(out->prefix, sizeof(out->prefix), "tpl-%u/", index);
	snprintf(uuid_str, sizeof(uuid_str),
		 "00000000-0000-4000-8000-%012x", index + 1);

	rc = s3_export_manifest_create(uuid_str, (uint64_t)N_CHUNKS * CHUNK_SIZE,
				       CHUNK_SIZE, S3_EXPORT_LAYOUT_REF, &m);
	if (rc != 0) {
		return rc;
	}
	snprintf(m->src.prefix, sizeof(m->src.prefix), "%s", out->prefix);
	snprintf(m->src.endpoint, sizeof(m->src.endpoint), "%s", ENDPOINT);
	rc = s3_export_manifest_add_src(m, m->src.prefix, NULL, NULL, &src_idx);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		return rc;
	}
	for (chunk = 0; chunk < N_CHUNKS; chunk++) {
		char text[SPDK_UUID_STRING_LEN];

		snprintf(text, sizeof(text),
			 "%08x-0000-4000-8000-000000000000",
			 (index << 16) | chunk);
		rc = spdk_uuid_parse(&out->uuids[chunk], text);
		if (rc != 0) {
			s3_export_manifest_unref(m);
			return rc;
		}
		rc = s3_export_manifest_set_ref(m, chunk, &out->uuids[chunk],
						CHUNK_SIZE);
		if (rc != 0) {
			s3_export_manifest_unref(m);
			return rc;
		}
		rc = s3_export_manifest_set_chunk_src(m, chunk, src_idx);
		if (rc != 0) {
			s3_export_manifest_unref(m);
			return rc;
		}
	}
	s3_export_manifest_seal(m);
	rc = s3_export_bs_dev_create(client, m, cache, &out->dev);
	s3_export_manifest_unref(m);
	if (rc != 0) {
		return rc;
	}
	out->ch = out->dev->create_channel(out->dev);
	if (!out->ch) {
		out->dev->destroy(out->dev);
		out->dev = NULL;
		return -ENOMEM;
	}
	return 0;
}

static void
drop_export(struct export_case *exp)
{
	if (!exp->dev) {
		return;
	}
	if (exp->ch) {
		exp->dev->destroy_channel(exp->dev, exp->ch);
		exp->ch = NULL;
	}
	exp->dev->destroy(exp->dev);
	exp->dev = NULL;
	poll_for_ms(20);
}

/* Submit \p n reads of distinct chunks and do not poll until every one has
 * been accepted. A disk hit completes only on a later poll, so none of them
 * may already be done: they are in flight together. */
static void
cold_reads(struct s3_cache *cache, struct export_case *exp, uint32_t n,
	   const char *label)
{
	struct read_slot slots[N_READS];
	struct spdk_bs_dev_cb_args args[N_READS];
	uint8_t *bufs[N_READS];
	struct s3_cache_stats before, after;
	uint32_t gets_before = g_ngets;
	uint32_t i;
	bool accepted = true;
	bool still_open = true;
	char what[80];

	memset(slots, 0, sizeof(slots));
	memset(args, 0, sizeof(args));
	memset(bufs, 0, sizeof(bufs));
	s3_cache_get_stats(cache, &before);
	for (i = 0; i < n; i++) {
		uint64_t lba = (uint64_t)i * (CHUNK_SIZE / AIO_BLOCK_SIZE);

		bufs[i] = spdk_dma_zmalloc(AIO_BLOCK_SIZE, AIO_BLOCK_SIZE, NULL);
		if (!bufs[i]) {
			accepted = false;
			break;
		}
		/* The device keeps this pointer until the read completes, so it
		 * has to outlive the submit loop. */
		args[i].channel = exp->ch;
		args[i].cb_fn = read_done;
		args[i].cb_arg = &slots[i];
		exp->dev->read(exp->dev, exp->ch, bufs[i], lba, 1, &args[i]);
		if (slots[i].done) {
			still_open = false;
		}
	}
	snprintf(what, sizeof(what), "%s reads are accepted together", label);
	check_true(what, accepted && still_open, NULL);
	snprintf(what, sizeof(what), "%s reads issue no GET", label);
	check_u64(what, g_ngets, gets_before);

	check_true("cold reads complete", poll_until_reads(slots, n), NULL);
	s3_cache_get_stats(cache, &after);
	snprintf(what, sizeof(what), "%s reads hit the object cache", label);
	check_u64(what, after.object_hits - before.object_hits, n);
	snprintf(what, sizeof(what), "%s reads miss nothing", label);
	check_u64(what, after.object_misses - before.object_misses, 0);
	for (i = 0; i < n; i++) {
		char key[S3_EXPORT_KEY_MAX];
		uint8_t expect[AIO_BLOCK_SIZE];

		s3_chunk_data_key(exp->prefix, &exp->uuids[i], key, sizeof(key));
		paint(expect, key, AIO_BLOCK_SIZE);
		snprintf(what, sizeof(what), "%s chunk %u bytes match the GET",
			 label, i);
		check_true(what,
			   slots[i].done && slots[i].status == 0 &&
			   bufs[i] && memcmp(bufs[i], expect, AIO_BLOCK_SIZE) == 0,
			   NULL);
		spdk_dma_free(bufs[i]);
	}
	snprintf(what, sizeof(what), "%s completion still issues no GET", label);
	check_u64(what, g_ngets, gets_before);
}

int
main(int argc, char **argv)
{
	const char *aio_path = argc > 1 ? argv[1] : DEFAULT_AIO_PATH;
	struct spdk_env_opts env_opts = {};
	struct spdk_bdev_desc *desc = NULL;
	struct spdk_io_channel *ch = NULL;
	struct s3_cache *cache = NULL;
	struct s3_client *client = (struct s3_client *)(uintptr_t)1;
	struct export_case one = {0};
	struct export_case many[N_EXPORTS] = {{0}};
	bool framework_up = false;
	bool file_created = false;
	bool bdev_created = false;
	uint32_t base;
	unsigned i;
	int rc;

	env_opts.opts_size = sizeof(env_opts);
	spdk_env_opts_init(&env_opts);
	env_opts.name = "s3_preheat_test";
	env_opts.no_huge = true;
	env_opts.mem_size = 512;
	if (spdk_env_init(&env_opts) < 0) {
		fprintf(stderr, "spdk_env_init failed\n");
		return 77;
	}
	spdk_log_open(NULL);
	if (spdk_thread_lib_init(NULL, 0) != 0) {
		goto out_env;
	}
	g_thread = spdk_thread_create("preheat_test", NULL);
	if (!g_thread) {
		goto out_thread_lib;
	}
	spdk_set_thread(g_thread);

	rc = framework_start();
	if (rc != 0) {
		fprintf(stderr, "framework_start: %d\n", rc);
		goto out_thread;
	}
	framework_up = true;

	rc = make_aio_file(aio_path, AIO_FILE_SIZE);
	if (rc != 0) {
		goto out_framework;
	}
	file_created = true;
	rc = create_aio_bdev(AIO_BDEV_NAME, aio_path, AIO_BLOCK_SIZE,
			     false, false, NULL, false);
	if (rc != 0) {
		fprintf(stderr, "create_aio_bdev: %d\n", rc);
		goto out_file;
	}
	bdev_created = true;
	rc = spdk_bdev_open_ext(AIO_BDEV_NAME, true, bdev_event_cb, NULL, &desc);
	if (rc != 0) {
		goto out_bdev;
	}
	ch = spdk_bdev_get_io_channel(desc);
	if (!ch) {
		goto out_desc;
	}

	{
		struct s3_cache_opts opts = {
			.desc = desc,
			.ch = ch,
			.region_offset = REGION_OFF,
			.region_size = REGION_SIZE,
			.chunk_size = CHUNK_SIZE,
			.block_size = AIO_BLOCK_SIZE,
			.num_chunks = N_SLOTS,
			.hot_bufs = 0,
		};
		struct s3_cache_stats stats;

		rc = s3_cache_create(&opts, &cache);
		if (rc != 0) {
			fprintf(stderr, "s3_cache_create: %d\n", rc);
			goto out_channel;
		}
		s3_cache_get_stats(cache, &stats);
		check_u64("object cache has a slot per preheat object",
			  stats.slots_total, N_SLOTS);
	}

	printf("\n[1] one import preheats, then concurrent cold reads hit\n");
	rc = make_export(cache, client, 0, &one);
	check_u64("template export is created", (uint64_t)-rc, 0);
	if (rc == 0) {
		base = g_ngets;
		rc = s3_export_bs_dev_preheat(one.dev);
		check_u64("preheat starts", (uint64_t)-rc, 0);
		check_u64("first wave stops at the per-export cap",
			  g_ngets - base, S3_EXPORT_PREHEAT_MAX);
		check_true("preheat drains onto the local cache",
			   drain_preheat(cache), NULL);
		check_u64("every present object was fetched once",
			  g_ngets - base, N_CHUNKS);
		cold_reads(cache, &one, N_READS, "post-preheat");
		drop_export(&one);
	}

	printf("\n[2] four imports preheat together, then cold reads hit\n");
	base = g_ngets;
	for (i = 0; i < N_EXPORTS; i++) {
		rc = make_export(cache, client, i + 1, &many[i]);
		check_u64("concurrent template export is created",
			  (uint64_t)-rc, 0);
		if (rc != 0) {
			break;
		}
		rc = s3_export_bs_dev_preheat(many[i].dev);
		check_u64("concurrent preheat starts", (uint64_t)-rc, 0);
	}
	if (i == N_EXPORTS) {
		check_u64("four exports share the global GET window",
			  g_ngets - base,
			  (uint64_t)N_EXPORTS * S3_EXPORT_PREHEAT_MAX);
		for (i = 0; i < N_EXPORTS; i++) {
			char what[80];

			snprintf(what, sizeof(what),
				 "export %u holds its own cap, not the global one",
				 i);
			check_u64(what,
				  count_prefix(many[i].prefix, base, g_ngets),
				  S3_EXPORT_PREHEAT_MAX);
		}
		check_true("concurrent preheat drains onto one cache",
			   drain_preheat(cache), NULL);
		check_u64("each import fetched its objects once",
			  g_ngets - base, (uint64_t)N_EXPORTS * N_CHUNKS);
		for (i = 0; i < N_EXPORTS; i++) {
			char label[32];

			snprintf(label, sizeof(label), "export %u", i);
			cold_reads(cache, &many[i], N_READS, label);
		}
	}
	for (i = 0; i < N_EXPORTS; i++) {
		drop_export(&many[i]);
	}

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);

	poll_for_ms(50);
	s3_cache_destroy(cache);
	cache = NULL;
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
out_thread:
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
