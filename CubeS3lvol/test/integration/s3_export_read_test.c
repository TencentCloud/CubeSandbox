/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * Whole-object reads and per-key single-flight in s3_export_bs_dev.
 *
 * S3 is stubbed but asynchronous: reads stay queued until complete_get() is
 * called, which makes two overlapping reads observable before the first GET
 * finishes. A real SPDK thread is used because the device registration and
 * completion-thread contract are part of the code under test.
 */

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"

#include "s3lvol/s3_export.h"
#include "s3lvol/s3_cache.h"
#include "s3lvol/s3_client.h"

#define CHUNK_SIZE (1024 * 1024)
#define NUM_CHUNKS 256
#define TEST_UUID  "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
#define MAX_GETS   512

struct fake_get {
	char key[S3_EXPORT_KEY_MAX];
	uint64_t offset;
	uint64_t len;
	void *buf;
	s3_get_cb cb;
	void *cb_arg;
	bool done;
};

static struct fake_get g_gets[MAX_GETS];
static uint32_t g_ngets;
static s3_op_cb g_head_cb;
static void *g_head_arg;
static uint64_t *g_head_size;
static uint32_t g_nheads;
static int g_pass, g_fail;
static uint32_t g_token_callbacks;
static int g_fail_next_range;
static uint32_t g_client_puts;
static char g_cached_key[S3_EXPORT_KEY_MAX];
static char g_cached_endpoint[S3_EXPORT_ENDPOINT_MAX];
static char g_cached_bucket[S3_EXPORT_BUCKET_MAX];
static uint8_t *g_cached_object;
static uint32_t g_cached_object_len;
static bool g_fail_cache_read;

static void
token_granted(void *cb_arg)
{
	uint32_t *callbacks = cb_arg;

	(*callbacks)++;
}

int
s3_get_range(struct s3_client *client, const char *key, uint64_t offset,
	     uint64_t len, void *buf, s3_get_cb cb, void *cb_arg)
{
	struct fake_get *get;

	(void)client;
	(void)key;
	if (g_fail_next_range != 0) {
		int rc = g_fail_next_range;

		g_fail_next_range = 0;
		return rc;
	}
	if (g_ngets == MAX_GETS) {
		return -ENOSPC;
	}
	get = &g_gets[g_ngets++];
	snprintf(get->key, sizeof(get->key), "%s", key);
	get->offset = offset;
	get->len = len;
	get->buf = buf;
	get->cb = cb;
	get->cb_arg = cb_arg;
	return 0;
}

int
s3_head(struct s3_client *client, const char *key, uint64_t *size,
	s3_op_cb cb, void *cb_arg)
{
	(void)client;
	(void)key;
	g_nheads++;
	g_head_cb = cb;
	g_head_arg = cb_arg;
	g_head_size = size;
	return 0;
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
	g_client_puts++;
}

const char *
s3_client_bucket(const struct s3_client *client)
{
	(void)client;
	return "test-bucket";
}

struct spdk_io_channel *
s3_cache_get_io_channel(struct s3_cache *cache)
{
	(void)cache;
	return NULL;
}

void
s3_cache_object_populate(struct s3_cache *cache,
			 const struct s3_cache_object_id *id,
			 uint32_t offset, const void *buf, uint32_t length,
			 uint32_t object_valid_bytes)
{
	(void)cache;
	if (offset != 0 || length != object_valid_bytes) {
		return;
	}
	free(g_cached_object);
	g_cached_object = malloc(object_valid_bytes);
	if (!g_cached_object) {
		g_cached_object_len = 0;
		return;
	}
	memcpy(g_cached_object, buf, object_valid_bytes);
	g_cached_object_len = object_valid_bytes;
	snprintf(g_cached_endpoint, sizeof(g_cached_endpoint), "%s", id->endpoint);
	snprintf(g_cached_bucket, sizeof(g_cached_bucket), "%s", id->bucket);
	snprintf(g_cached_key, sizeof(g_cached_key), "%s", id->key);
}

int
s3_cache_object_read_on_channel(struct s3_cache *cache,
				struct spdk_io_channel *channel,
				const struct s3_cache_object_id *id,
				uint32_t object_valid_bytes, uint32_t offset,
				uint32_t length, void *buf,
				s3_cache_read_cb cb_fn, void *cb_arg)
{
	uint32_t readable;

	(void)cache;
	(void)channel;
	if (!g_cached_object || object_valid_bytes != g_cached_object_len ||
	    strcmp(id->endpoint, g_cached_endpoint) != 0 ||
	    strcmp(id->bucket, g_cached_bucket) != 0 ||
	    strcmp(id->key, g_cached_key) != 0 || offset > object_valid_bytes) {
		return -ENOENT;
	}
	if (g_fail_cache_read) {
		g_fail_cache_read = false;
		cb_fn(cb_arg, -EIO);
		return 0;
	}
	readable = spdk_min(length, object_valid_bytes - offset);
	memcpy(buf, g_cached_object + offset, readable);
	if (readable < length) {
		memset((uint8_t *)buf + readable, 0, length - readable);
	}
	cb_fn(cb_arg, 0);
	return 0;
}

static void
check_true(const char *what, bool ok)
{
	if (ok) {
		g_pass++;
		printf("\t[PASS] %s\n", what);
	} else {
		g_fail++;
		printf("\t[FAIL] %s\n", what);
	}
}

static void
check_u64(const char *what, uint64_t got, uint64_t want)
{
	if (got == want) {
		g_pass++;
		printf("\t[PASS] %s (got %" PRIu64 ")\n", what, got);
	} else {
		g_fail++;
		printf("\t[FAIL] %s (got %" PRIu64 ", want %" PRIu64 ")\n",
		       what, got, want);
	}
}

static void
complete_get(uint32_t index, int status, uint64_t bytes)
{
	struct fake_get *get = &g_gets[index];
	uint64_t i, n = spdk_min(bytes, get->len);

	assert(!get->done);
	get->done = true;
	if (status == 0) {
		for (i = 0; i < n; i++) {
			((uint8_t *)get->buf)[i] = (uint8_t)((get->offset + i) & 0xff);
		}
	}
	get->cb(get->cb_arg, bytes, status);
}

static void
complete_get_data(uint32_t index, const void *data, uint64_t bytes)
{
	struct fake_get *get = &g_gets[index];

	assert(!get->done);
	assert(bytes <= get->len);
	memcpy(get->buf, data, bytes);
	get->done = true;
	get->cb(get->cb_arg, bytes, 0);
}

static void
complete_outstanding(void)
{
	uint32_t i;

	for (i = 0; i < g_ngets; i++) {
		if (!g_gets[i].done) {
			complete_get(i, 0, g_gets[i].len);
		}
	}
}

struct read_result {
	struct spdk_bs_dev_cb_args cb_args;
	bool done;
	int status;
	uint8_t *buf;
	uint32_t len;
};

static void
read_done(struct spdk_io_channel *channel, void *cb_arg, int status)
{
	struct read_result *r = cb_arg;

	(void)channel;
	r->status = status;
	r->done = true;
}

static void
submit_read(struct spdk_bs_dev *dev, struct read_result *r,
	    uint64_t byte_offset, uint32_t length)
{
	memset(r, 0, sizeof(*r));
	r->buf = malloc(length);
	r->len = length;
	memset(r->buf, 0xcc, length);
	r->cb_args.cb_fn = read_done;
	r->cb_args.cb_arg = r;
	dev->read(dev, NULL, r->buf, byte_offset / S3LVOL_BLOCK_SIZE,
		  length / S3LVOL_BLOCK_SIZE, &r->cb_args);
}

static void
submit_read_on_channel(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		       struct read_result *r, uint64_t byte_offset,
		       uint32_t length)
{
	memset(r, 0, sizeof(*r));
	r->buf = malloc(length);
	r->len = length;
	memset(r->buf, 0xcc, length);
	r->cb_args.cb_fn = read_done;
	r->cb_args.cb_arg = r;
	dev->read(dev, channel, r->buf, byte_offset / S3LVOL_BLOCK_SIZE,
		  length / S3LVOL_BLOCK_SIZE, &r->cb_args);
}

static bool
buffer_has_pattern(const struct read_result *r, uint64_t byte_offset,
		   uint32_t pattern_len)
{
	uint32_t i;

	for (i = 0; i < pattern_len; i++) {
		if (r->buf[i] != (uint8_t)((byte_offset + i) & 0xff)) {
			return false;
		}
	}
	return true;
}

static struct s3_export_manifest *
make_manifest(void)
{
	struct s3_export_manifest *m = NULL;
	struct spdk_uuid uuid;
	uint8_t raw[16];
	uint8_t src_idx;
	uint32_t i;

	if (s3_export_manifest_create(TEST_UUID,
				      (uint64_t)NUM_CHUNKS * CHUNK_SIZE,
				      CHUNK_SIZE, S3_EXPORT_LAYOUT_REF, &m) != 0) {
		return NULL;
	}
	snprintf(m->src.prefix, sizeof(m->src.prefix), "read-test");
	if (s3_export_manifest_add_src(m, m->src.prefix, NULL, NULL, &src_idx) != 0) {
		s3_export_manifest_unref(m);
		return NULL;
	}
	for (i = 0; i < NUM_CHUNKS; i++) {
		memset(raw, (int)(i + 1), sizeof(raw));
		memcpy(&uuid, raw, sizeof(uuid));
		if (s3_export_manifest_set_ref(m, i, &uuid,
					      i == 20 ? 64 * 1024 : CHUNK_SIZE) != 0) {
			s3_export_manifest_unref(m);
			return NULL;
		}
	}
	s3_export_manifest_seal(m);
	return m;
}

int
main(void)
{
	struct spdk_env_opts opts;
	struct spdk_thread *thread = NULL, *thread2 = NULL;
	struct s3_export_manifest *m = NULL;
	struct s3_export_manifest *replacement = NULL;
	struct spdk_bs_dev *dev = NULL;
	struct read_result a, b, hit, retained, short_ref, cross_a, cross_b;
	struct read_result missing_a, missing_b, post_swap;
	struct read_result seq_a, seq_b, prefetch_hit;
	struct read_result direct, direct_join, joined_slice;
	struct read_result destroy_seq_a, destroy_seq_b, destroy_cross;
	struct read_result oom;
	struct read_result many[65];
	char *replacement_json = NULL;
	size_t replacement_len = 0;
	uint32_t i, first;
	int rc;

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);
	printf("=== s3lvol export whole-object read test ===\n");
	/* This suite explicitly verifies the userspace prefetch state machine.
	 * Production normally exports 1024 KiB host readahead, which suppresses
	 * that redundant layer; do not let the developer's environment silently
	 * turn the requests asserted below off. */
	setenv("S3LVOL_READ_AHEAD_KB", "0", 1);

	opts.opts_size = sizeof(opts);
	spdk_env_opts_init(&opts);
	opts.name = "s3_export_read_test";
	opts.no_huge = true;
	opts.mem_size = 64;
	if (spdk_env_init(&opts) < 0) {
		fprintf(stderr, "spdk_env_init failed; skipping\n");
		spdk_log_close();
		return 77;
	}
	rc = spdk_thread_lib_init(NULL, 0);
	check_true("spdk_thread_lib_init", rc == 0);
	if (rc != 0) {
		goto out_env;
	}
	thread = spdk_thread_create("export_read", NULL);
	check_true("spdk_thread_create", thread != NULL);
	if (!thread) {
		goto out_lib;
	}
	spdk_set_thread(thread);
	thread2 = spdk_thread_create("export_waiter", NULL);
	check_true("second spdk_thread_create", thread2 != NULL);
	if (!thread2) {
		goto out_thread;
	}

	m = make_manifest();
	check_true("manifest created", m != NULL);
	if (!m) {
		goto out_thread;
	}
	rc = s3_export_bs_dev_create((struct s3_client *)(uintptr_t)1, m, NULL, &dev);
	check_true("export device created", rc == 0);
	if (rc != 0) {
		goto out_manifest;
	}

	printf("\n[1] overlapping reads share one whole-object GET\n");
	submit_read(dev, &a, 4 * 1024, 4 * 1024);
	submit_read(dev, &b, 8 * 1024, 4 * 1024);
	check_u64("demand plus eight prefetches are in flight", g_ngets, 9);
	check_u64("GET starts at object offset zero", g_gets[0].offset, 0);
	check_u64("demand GET covers the whole object", g_gets[0].len, CHUNK_SIZE);
	check_true("both reads wait", !a.done && !b.done);
	complete_get(0, 0, CHUNK_SIZE);
	check_true("both reads complete", a.done && b.done);
	check_true("first slice has the requested bytes",
		   buffer_has_pattern(&a, 4 * 1024, a.len));
	check_true("second slice has the requested bytes",
		   buffer_has_pattern(&b, 8 * 1024, b.len));

	printf("\n[2] the completed object remains in the small RAM LRU\n");
	submit_read(dev, &hit, 12 * 1024, 4 * 1024);
	check_true("LRU hit completes synchronously", hit.done && hit.status == 0);
	check_u64("LRU hit submits no GET", g_ngets, 9);
	check_true("LRU slice is correct",
		   buffer_has_pattern(&hit, 12 * 1024, hit.len));
	complete_outstanding();

	printf("\n[3] a sole whole-object read is filled directly\n");
	first = g_ngets;
	submit_read(dev, &direct, 18ULL * CHUNK_SIZE, CHUNK_SIZE);
	check_u64("whole-object demand adds one GET", g_ngets, first + 1);
	check_true("GET targets the caller's buffer",
		   g_gets[first].buf == direct.buf);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("direct whole-object read completes",
		   direct.done && direct.status == 0);
	check_true("direct whole-object bytes are correct",
		   buffer_has_pattern(&direct, 18ULL * CHUNK_SIZE, direct.len));

	printf("\n[4] a waiter joining a direct GET safely restores staging\n");
	first = g_ngets;
	submit_read(dev, &direct_join, 151ULL * CHUNK_SIZE, CHUNK_SIZE);
	submit_read(dev, &joined_slice, 151ULL * CHUNK_SIZE + 4 * 1024,
		    4 * 1024);
	check_u64("joined whole-object demand still uses one GET",
		  g_ngets, first + 1);
	check_true("the GET initially targets the full request",
		   g_gets[first].buf == direct_join.buf);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("full request and joined slice both complete",
		   direct_join.done && direct_join.status == 0 &&
		   joined_slice.done && joined_slice.status == 0);
	check_true("joined slice receives the requested bytes",
		   buffer_has_pattern(&joined_slice,
				      151ULL * CHUNK_SIZE + 4 * 1024,
				      joined_slice.len));

	printf("\n[5] a short REF object is fetched only to valid_bytes\n");
	first = g_ngets;
	submit_read(dev, &short_ref, 20ULL * CHUNK_SIZE + 60 * 1024, 8 * 1024);
	check_u64("short object adds one GET", g_ngets, first + 1);
	check_u64("GET is clamped to valid_bytes", g_gets[first].len, 64 * 1024);
	complete_get(first, 0, 64 * 1024);
	check_true("short-object read completes", short_ref.done && short_ref.status == 0);
	check_true("bytes inside the object are copied",
		   buffer_has_pattern(&short_ref, 60 * 1024, 4 * 1024));
	check_true("bytes past valid_bytes are zero",
		   short_ref.buf[4 * 1024] == 0 && short_ref.buf[short_ref.len - 1] == 0);

	printf("\n[6] local admission queues beyond 64 without exact GETs\n");
	first = g_ngets;
	for (i = 0; i < 65; i++) {
		submit_read(dev, &many[i], (uint64_t)(2 * i + 22) * CHUNK_SIZE,
			    4 * 1024);
	}
	check_u64("only 64 whole GETs are active", g_ngets - first, 64);
	for (i = 0; i < 64; i++) {
		check_u64("staged request is a whole object",
			  g_gets[first + i].len, CHUNK_SIZE);
	}
	for (i = 0; i < 64; i++) {
		complete_get(first + i, 0, g_gets[first + i].len);
	}
	check_u64("the queued request starts after a slot is released",
		  g_ngets - first, 65);
	check_u64("the queued request is also whole-object",
		  g_gets[first + 64].len, CHUNK_SIZE);
	complete_get(first + 64, 0, CHUNK_SIZE);
	for (i = 0; i < 65; i++) {
		check_true("admitted read completes successfully",
			   many[i].done && many[i].status == 0);
	}
	first = g_ngets;
	submit_read(dev, &retained, 4 * 1024, 4 * 1024);
	check_u64("the larger READY LRU retains the oldest object", g_ngets, first);
	check_true("retained object is served from RAM",
		   retained.done && retained.status == 0);

	printf("\n[7] a waiter completes on its own SPDK thread\n");
	first = g_ngets;
	submit_read(dev, &cross_a, 200ULL * CHUNK_SIZE, 4 * 1024);
	spdk_set_thread(thread2);
	submit_read(dev, &cross_b, 200ULL * CHUNK_SIZE + 4 * 1024, 4 * 1024);
	spdk_set_thread(thread);
	check_u64("cross-thread reads share one GET", g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("GET owner's read completes first", cross_a.done && !cross_b.done);
	spdk_set_thread(thread2);
	spdk_thread_poll(thread2, 0, 0);
	check_true("waiter completes after its thread runs",
		   cross_b.done && cross_b.status == 0);
	spdk_set_thread(thread);

	printf("\n[8] a successful short GET fails every waiter\n");
	first = g_ngets;
	free(many[0].buf);
	submit_read(dev, &many[0], 202ULL * CHUNK_SIZE + 4 * 1024, 4 * 1024);
	check_u64("short-read case has an outstanding GET", g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE - 4096);
	check_true("short successful response becomes EIO",
		   many[0].done && many[0].status == -EIO);

	printf("\n[9] sequential demand starts eight low-priority prefetches\n");
	first = g_ngets;
	submit_read(dev, &seq_a, 210ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("a jump does not prefetch", g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE);
	first = g_ngets;
	submit_read(dev, &seq_b, 211ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("one demand plus eight prefetch GETs are submitted",
		  g_ngets - first, 9);
	submit_read(dev, &prefetch_hit, 212ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("demand joins the prefetched object", g_ngets - first, 9);
	complete_get(first, 0, CHUNK_SIZE);
	complete_get(first + 1, 0, CHUNK_SIZE);
	check_true("joined prefetch completes the demand",
		   prefetch_hit.done && prefetch_hit.status == 0);
	for (i = 2; i < 9; i++) {
		complete_get(first + i, 0, CHUNK_SIZE);
	}
	first = g_ngets;
	free(seq_b.buf);
	submit_read(dev, &seq_b, 211ULL * CHUNK_SIZE + 8 * 1024, 4 * 1024);
	check_true("READY prefetch is served from RAM",
		   seq_b.done && seq_b.status == 0);
	check_u64("READY prefetch submits no GET", g_ngets, first);

	printf("\n[10] a shared 404 refetches and retries on the new generation\n");
	rc = s3_export_manifest_create(TEST_UUID,
				       (uint64_t)NUM_CHUNKS * CHUNK_SIZE,
				       CHUNK_SIZE, S3_EXPORT_LAYOUT_DENSE,
				       &replacement);
	check_true("replacement manifest created", rc == 0);
	if (rc == 0) {
		replacement->generation = 1;
		snprintf(replacement->src.prefix, sizeof(replacement->src.prefix),
			 "read-test");
		for (i = 0; i < NUM_CHUNKS; i++) {
			s3_export_manifest_set_present(replacement, i);
		}
		s3_export_manifest_seal(replacement);
		rc = s3_export_manifest_serialize(replacement, &replacement_json,
						  &replacement_len);
		check_true("replacement manifest serialized", rc == 0);
	}
	first = g_ngets;
	submit_read(dev, &missing_a, 230ULL * CHUNK_SIZE, 4 * 1024);
	submit_read(dev, &missing_b, 230ULL * CHUNK_SIZE + 4 * 1024, 4 * 1024);
	check_u64("missing reads share one object GET", g_ngets, first + 1);
	complete_get(first, -ENOENT, 0);
	spdk_thread_poll(thread, 0, 0);
	check_u64("both waiters share one manifest HEAD", g_nheads, 1);
	check_true("reads wait for the refetch", !missing_a.done && !missing_b.done);
	*g_head_size = replacement_len;
	g_head_cb(g_head_arg, 0);
	check_u64("manifest GET follows HEAD", g_ngets, first + 2);
	complete_get_data(first + 1, replacement_json, replacement_len);
	for (i = 0; i < 4; i++) {
		spdk_thread_poll(thread, 0, 0);
	}
	check_u64("both reads retry through one new object GET", g_ngets, first + 3);
	check_true("new generation changed the object key",
		   strcmp(g_gets[first].key, g_gets[first + 2].key) != 0);
	complete_get(first + 2, 0, CHUNK_SIZE);
	check_true("both retried reads complete",
		   missing_a.done && missing_a.status == 0 &&
		   missing_b.done && missing_b.status == 0);
	complete_outstanding();

	/* Chunk 200 still has a successful REF-layout object in the working set.
	 * The dense generation names another key, so it must not hit that entry. */
	first = g_ngets;
	submit_read(dev, &post_swap, 200ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("post-swap read does not hit the old-key LRU entry",
		  g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("post-swap read completes from the new key",
		   post_swap.done && post_swap.status == 0);

	printf("\n[11] staging OOM after admission falls back to an exact GET\n");
	g_fail_next_range = -ENOMEM;
	first = g_ngets;
	submit_read(dev, &oom, 220ULL * CHUNK_SIZE + 8 * 1024, 4 * 1024);
	check_u64("failed whole GET is replaced by one exact range GET",
		  g_ngets, first + 1);
	check_u64("exact fallback starts at the requested offset",
		  g_gets[first].offset, 8 * 1024);
	check_u64("exact fallback length is the slice", g_gets[first].len, 4 * 1024);
	complete_get(first, 0, 4 * 1024);
	check_true("OOM fallback completes the read",
		   oom.done && oom.status == 0);
	check_true("exact fallback copied the requested bytes",
		   buffer_has_pattern(&oom, 8 * 1024, oom.len));

	printf("\n[12] destroy waits for blobstore reads and prefetch GETs\n");
	first = g_ngets;
	submit_read(dev, &destroy_seq_a, 240ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("destroy case jump submits only demand", g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("destroy-case jump completes", destroy_seq_a.done);
	first = g_ngets;
	submit_read(dev, &destroy_seq_b, 241ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("destroy case starts demand plus eight prefetches",
		  g_ngets - first, 9);
	spdk_set_thread(thread2);
	submit_read(dev, &destroy_cross, 241ULL * CHUNK_SIZE + 4 * 1024,
		    4 * 1024);
	spdk_set_thread(thread);
	check_u64("cross-thread read joins destroy-case demand",
		  g_ngets - first, 9);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("owner completes while cross-thread read is queued",
		   destroy_seq_b.done && !destroy_cross.done);
	dev->destroy(dev);
	for (i = 1; i < 9; i++) {
		complete_get(first + i, 0, CHUNK_SIZE);
	}
	for (i = 0; i < 100; i++) {
		spdk_thread_poll(thread, 0, 0);
	}
	check_u64("device stays alive for queued blobstore completion",
		  g_client_puts, 0);
	spdk_set_thread(thread2);
	spdk_thread_poll(thread2, 0, 0);
	check_true("queued blobstore read completes on its thread",
		   destroy_cross.done && destroy_cross.status == 0);
	for (i = 0; i < 100; i++) {
		spdk_set_thread(thread);
		spdk_thread_poll(thread, 0, 0);
		spdk_set_thread(thread2);
		spdk_thread_poll(thread2, 0, 0);
	}
	spdk_set_thread(thread);
	check_u64("device is released after the last read callback", g_client_puts, 1);
	dev = NULL;

	free(a.buf);
	free(b.buf);
	free(hit.buf);
	free(retained.buf);
	free(short_ref.buf);
	free(cross_a.buf);
	free(cross_b.buf);
	free(missing_a.buf);
	free(missing_b.buf);
	free(post_swap.buf);
	free(seq_a.buf);
	free(seq_b.buf);
	free(prefetch_hit.buf);
	free(direct.buf);
	free(direct_join.buf);
	free(joined_slice.buf);
	free(oom.buf);
	free(destroy_seq_a.buf);
	free(destroy_seq_b.buf);
	free(destroy_cross.buf);
	for (i = 0; i < 65; i++) {
		free(many[i].buf);
	}

	printf("\n[13] process-wide whole-GET budget is exactly 256\n");
	bool all_immediate = true;
	for (i = 0; i < S3_WHOLE_GET_MAX_INFLIGHT; i++) {
		rc = s3_whole_get_token_acquire(false, token_granted,
						&g_token_callbacks);
		all_immediate &= rc == 1;
	}
	check_true("all 256 tokens are admitted immediately", all_immediate);
	rc = s3_whole_get_token_acquire(false, token_granted,
					&g_token_callbacks);
	check_true("the 257th token waits", rc == 0 && g_token_callbacks == 0);
	s3_whole_get_token_release();
	check_u64("a release transfers ownership to the queued request",
		  g_token_callbacks, 1);
	for (i = 0; i < S3_WHOLE_GET_MAX_INFLIGHT; i++) {
		s3_whole_get_token_release();
	}

	g_token_callbacks = 0;
	all_immediate = true;
	for (i = 0; i < S3_WHOLE_GET_MAX_INFLIGHT - 1; i++) {
		all_immediate &=
			s3_whole_get_token_acquire(false, token_granted,
						   &g_token_callbacks) == 1;
	}
	check_true("255 priority-test tokens are immediate", all_immediate);
	check_true("prefetch does not take or queue for the last token",
		   s3_whole_get_token_acquire(true, token_granted,
					      &g_token_callbacks) == -EAGAIN);
	check_true("demand can take the reserved last token",
		   s3_whole_get_token_acquire(false, token_granted,
					      &g_token_callbacks) == 1);
	for (i = 0; i < S3_WHOLE_GET_MAX_INFLIGHT; i++) {
		s3_whole_get_token_release();
	}

	g_token_callbacks = 0;
	all_immediate = true;
	for (i = 0; i < S3_WHOLE_GET_MAX_INFLIGHT; i++) {
		all_immediate &=
			s3_whole_get_token_acquire(false, token_granted,
						   &g_token_callbacks) == 1;
	}
	check_true("256 bounce-test tokens are immediate", all_immediate);
	spdk_set_thread(thread2);
	check_true("cross-thread token request queues",
		   s3_whole_get_token_acquire(false, token_granted,
					      &g_token_callbacks) == 0);
	spdk_set_thread(thread);
	s3_whole_get_token_release();
	check_u64("grant waits for the requesting thread to poll",
		  g_token_callbacks, 0);
	spdk_set_thread(thread2);
	spdk_thread_poll(thread2, 0, 0);
	check_u64("grant is delivered on the requesting thread",
		  g_token_callbacks, 1);
	spdk_set_thread(thread);
	for (i = 0; i < S3_WHOLE_GET_MAX_INFLIGHT; i++) {
		s3_whole_get_token_release();
	}

	printf("\n[14] a second export device reuses the lvstore object cache\n");
	{
		struct spdk_bs_dev *first_dev = NULL, *second_dev = NULL;
		struct spdk_io_channel *first_ch = NULL, *second_ch = NULL;
		struct read_result first_read = {0}, second_read = {0};
		struct read_result fallback_read = {0};
		struct s3_cache *fake_cache = (struct s3_cache *)(uintptr_t)1;
		uint64_t byte_offset = 150ULL * CHUNK_SIZE;

		free(g_cached_object);
		g_cached_object = NULL;
		g_cached_object_len = 0;
		g_cached_key[0] = '\0';

		rc = s3_export_bs_dev_create((struct s3_client *)(uintptr_t)1, m,
					     fake_cache, &first_dev);
		check_true("first cached export device is created", rc == 0);
		if (rc == 0) {
			first_ch = first_dev->create_channel(first_dev);
			first = g_ngets;
			submit_read_on_channel(first_dev, first_ch, &first_read,
					       byte_offset, CHUNK_SIZE);
			check_u64("cold first device submits one S3 GET", g_ngets,
				  first + 1);
			complete_get(first, 0, CHUNK_SIZE);
			check_true("cold read completes and populates shared cache",
				   first_read.done && first_read.status == 0 &&
				   g_cached_object_len == CHUNK_SIZE);
			first_dev->destroy_channel(first_dev, first_ch);
			first_dev->destroy(first_dev);
			spdk_thread_poll(thread, 0, 0);
		}

		rc = s3_export_bs_dev_create((struct s3_client *)(uintptr_t)1, m,
					     fake_cache, &second_dev);
		check_true("second cached export device is created", rc == 0);
		if (rc == 0) {
			second_ch = second_dev->create_channel(second_dev);
			first = g_ngets;
			submit_read_on_channel(second_dev, second_ch, &second_read,
					       byte_offset, CHUNK_SIZE);
			check_true("second device reads immediately from shared cache",
				   second_read.done && second_read.status == 0);
			check_u64("shared-cache hit submits no S3 GET", g_ngets,
				  first);
			check_true("shared-cache bytes match the immutable object",
				   buffer_has_pattern(&second_read, byte_offset,
						      CHUNK_SIZE));
			g_fail_cache_read = true;
			first = g_ngets;
			submit_read_on_channel(second_dev, second_ch, &fallback_read,
					       byte_offset, CHUNK_SIZE);
			check_u64("cache I/O failure falls back to one S3 GET",
				  g_ngets, first + 1);
			complete_get(first, 0, CHUNK_SIZE);
			check_true("S3 fallback preserves the user read",
				   fallback_read.done &&
				   fallback_read.status == 0 &&
				   buffer_has_pattern(&fallback_read, byte_offset,
						      CHUNK_SIZE));
			second_dev->destroy_channel(second_dev, second_ch);
			second_dev->destroy(second_dev);
			spdk_thread_poll(thread, 0, 0);
		}
		free(first_read.buf);
		free(second_read.buf);
		free(fallback_read.buf);
		free(g_cached_object);
		g_cached_object = NULL;
	}

out_manifest:
	free(replacement_json);
	s3_export_manifest_unref(replacement);
	s3_export_manifest_unref(m);
out_thread:
	if (thread2) {
		spdk_set_thread(thread2);
		spdk_thread_exit(thread2);
		while (!spdk_thread_is_exited(thread2)) {
			spdk_thread_poll(thread2, 0, 0);
		}
		spdk_thread_destroy(thread2);
	}
	spdk_set_thread(thread);
	spdk_thread_exit(thread);
	while (!spdk_thread_is_exited(thread)) {
		spdk_thread_poll(thread, 0, 0);
	}
	spdk_thread_destroy(thread);
	spdk_set_thread(NULL);
out_lib:
	spdk_thread_lib_fini();
out_env:
	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	spdk_log_close();
	return g_fail == 0 ? 0 : 1;
}
