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
#include "s3lvol/s3_client.h"

#define CHUNK_SIZE (1024 * 1024)
#define NUM_CHUNKS 160
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
					      i == 3 ? 64 * 1024 : CHUNK_SIZE) != 0) {
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
	struct read_result destroy_seq_a, destroy_seq_b;
	struct read_result oom;
	struct read_result many[65];
	char *replacement_json = NULL;
	size_t replacement_len = 0;
	uint32_t i, first;
	int rc;

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);
	printf("=== s3lvol export whole-object read test ===\n");

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
	rc = s3_export_bs_dev_create((struct s3_client *)(uintptr_t)1, m, &dev);
	check_true("export device created", rc == 0);
	if (rc != 0) {
		goto out_manifest;
	}

	printf("\n[1] overlapping reads share one whole-object GET\n");
	submit_read(dev, &a, 4 * 1024, 4 * 1024);
	submit_read(dev, &b, 8 * 1024, 4 * 1024);
	check_u64("one GET is in flight", g_ngets, 1);
	check_u64("GET starts at object offset zero", g_gets[0].offset, 0);
	check_u64("GET covers the whole object", g_gets[0].len, CHUNK_SIZE);
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
	check_u64("LRU hit submits no GET", g_ngets, 1);
	check_true("LRU slice is correct",
		   buffer_has_pattern(&hit, 12 * 1024, hit.len));

	printf("\n[3] a short REF object is fetched only to valid_bytes\n");
	submit_read(dev, &short_ref, 3ULL * CHUNK_SIZE + 60 * 1024, 8 * 1024);
	check_u64("short object adds one GET", g_ngets, 2);
	check_u64("GET is clamped to valid_bytes", g_gets[1].len, 64 * 1024);
	complete_get(1, 0, 64 * 1024);
	check_true("short-object read completes", short_ref.done && short_ref.status == 0);
	check_true("bytes inside the object are copied",
		   buffer_has_pattern(&short_ref, 60 * 1024, 4 * 1024));
	check_true("bytes past valid_bytes are zero",
		   short_ref.buf[4 * 1024] == 0 && short_ref.buf[short_ref.len - 1] == 0);

	printf("\n[4] local admission queues beyond 64 without exact GETs\n");
	first = g_ngets;
	for (i = 0; i < 65; i++) {
		submit_read(dev, &many[i], (uint64_t)(i * 2 + 2) * CHUNK_SIZE,
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

	printf("\n[5] a waiter completes on its own SPDK thread\n");
	first = g_ngets;
	submit_read(dev, &cross_a, 132ULL * CHUNK_SIZE, 4 * 1024);
	spdk_set_thread(thread2);
	submit_read(dev, &cross_b, 132ULL * CHUNK_SIZE + 4 * 1024, 4 * 1024);
	spdk_set_thread(thread);
	check_u64("cross-thread reads share one GET", g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("GET owner's read completes first", cross_a.done && !cross_b.done);
	spdk_set_thread(thread2);
	spdk_thread_poll(thread2, 0, 0);
	check_true("waiter completes after its thread runs",
		   cross_b.done && cross_b.status == 0);
	spdk_set_thread(thread);

	printf("\n[6] a successful short GET fails every waiter\n");
	first = g_ngets;
	free(many[0].buf);
	submit_read(dev, &many[0], 134ULL * CHUNK_SIZE + 4 * 1024, 4 * 1024);
	check_u64("short-read case has an outstanding GET", g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE - 4096);
	check_true("short successful response becomes EIO",
		   many[0].done && many[0].status == -EIO);

	printf("\n[7] sequential demand starts eight low-priority prefetches\n");
	first = g_ngets;
	submit_read(dev, &seq_a, 140ULL * CHUNK_SIZE, 4 * 1024);
	complete_get(first, 0, CHUNK_SIZE);
	first = g_ngets;
	submit_read(dev, &seq_b, 141ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("one demand plus eight prefetch GETs are submitted",
		  g_ngets - first, 9);
	submit_read(dev, &prefetch_hit, 142ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("demand joins the prefetched object", g_ngets - first, 9);
	complete_get(first, 0, CHUNK_SIZE);
	complete_get(first + 1, 0, CHUNK_SIZE);
	check_true("joined prefetch completes the demand",
		   prefetch_hit.done && prefetch_hit.status == 0);
	for (i = 2; i < 9; i++) {
		complete_get(first + i, 0, CHUNK_SIZE);
	}

	printf("\n[8] a shared 404 refetches and retries on the new generation\n");
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
	submit_read(dev, &missing_a, 134ULL * CHUNK_SIZE, 4 * 1024);
	submit_read(dev, &missing_b, 134ULL * CHUNK_SIZE + 4 * 1024, 4 * 1024);
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

	/* Chunk 132 still has a successful REF-layout object in the working set.
	 * The dense generation names another key, so it must not hit that entry. */
	first = g_ngets;
	submit_read(dev, &post_swap, 132ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("post-swap read does not hit the old-key LRU entry",
		  g_ngets, first + 1);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("post-swap read completes from the new key",
		   post_swap.done && post_swap.status == 0);

	printf("\n[9] staging OOM after admission falls back to an exact GET\n");
	g_fail_next_range = -ENOMEM;
	first = g_ngets;
	submit_read(dev, &oom, 148ULL * CHUNK_SIZE + 8 * 1024, 4 * 1024);
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

	printf("\n[10] destroy waits for in-flight prefetch GETs\n");
	first = g_ngets;
	submit_read(dev, &destroy_seq_a, 150ULL * CHUNK_SIZE, 4 * 1024);
	complete_get(first, 0, CHUNK_SIZE);
	first = g_ngets;
	submit_read(dev, &destroy_seq_b, 151ULL * CHUNK_SIZE, 4 * 1024);
	check_u64("destroy case starts demand plus eight prefetches",
		  g_ngets - first, 9);
	complete_get(first, 0, CHUNK_SIZE);
	check_true("destroy-case demand completes", destroy_seq_b.done);
	dev->destroy(dev);
	for (i = 1; i < 9; i++) {
		complete_get(first + i, 0, CHUNK_SIZE);
	}
	for (i = 0; i < 100; i++) {
		spdk_thread_poll(thread, 0, 0);
	}
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
	free(oom.buf);
	free(destroy_seq_a.buf);
	free(destroy_seq_b.buf);
	for (i = 0; i < 65; i++) {
		free(many[i].buf);
	}

	printf("\n[11] process-wide whole-GET budget is exactly 256\n");
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
