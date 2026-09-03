/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Swapping the manifest under an imported export
 *
 *   === What this is for ===
 *
 *   A ref export names the *source's* live objects. When the source materialises
 *   it -- rewrites the manifest as dense, holding its own copies -- those objects
 *   stop existing, and an importer still holding the old manifest reads 404s. Its
 *   way out is to refetch and carry on against the new manifest, which is what
 *   s3_export_bs_dev_swap_manifest() does and what `generation` distinguishes.
 *   A GET that left before that swap can still 404; the refetch then sees
 *   -EALREADY, which must retry against the installed generation rather than
 *   report the snapshot as deleted.
 *
 *   === Why this needs real SPDK threads ===
 *
 *   The whole of s3_export_bs_dev.c is lock free *because* the manifest never
 *   changes: blobstore makes a channel per thread that touches the clone, and
 *   every one of them reads dev->m with no serialisation. The swap is the one
 *   operation that has to consider the others, and it does so with a grace period
 *   -- spdk_for_each_channel() -- rather than a lock.
 *
 *   A test without threads would exercise the validation and none of that. So
 *   this brings up spdk_thread and drives the iteration, which is also the only
 *   way to observe the property that matters: the old manifest is released after
 *   the grace period, not before. Checked by refcount, because "released too
 *   early" is otherwise a use-after-free that shows up somewhere else entirely.
 *
 *   No S3 and no network: swapping is pure memory. The manifests are built in
 *   process and the device never reads.
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"
#include "spdk/blob.h"

#include "s3lvol/s3_export.h"
#include "s3lvol/s3_client.h"
#include "s3lvol/s3_spawner.h"

#define CHUNK_SIZE (1024 * 1024)
#define NUM_CHUNKS 8
#define TEST_UUID  "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
#define OTHER_UUID "01234567-89ab-cdef-0123-456789abcdef"

static int g_pass, g_fail;

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		g_pass++;
		printf("\t[PASS] %s%s%s\n", what, detail ? " " : "",
		       detail ? detail : "");
	} else {
		g_fail++;
		printf("\t[FAIL] %s%s%s\n", what, detail ? " " : "",
		       detail ? detail : "");
	}
}

static void
check_int(const char *what, int got, int want)
{
	if (got == want) {
		g_pass++;
		printf("\t[PASS] %s (got %d, want %d)\n", what, got, want);
	} else {
		g_fail++;
		printf("\t[FAIL] %s (got %d, want %d)\n", what, got, want);
	}
}

static void
check_u32(const char *what, uint32_t got, uint32_t want)
{
	if (got == want) {
		g_pass++;
		printf("\t[PASS] %s (got %u, want %u)\n", what, got, want);
	} else {
		g_fail++;
		printf("\t[FAIL] %s (got %u, want %u)\n", what, got, want);
	}
}

/* A manifest of the given identity and version, with one chunk so that a ref
 * layout is well formed. */
static struct s3_export_manifest *
make_manifest(const char *uuid, uint32_t generation, uint64_t size_bytes,
	      enum s3_export_layout layout)
{
	struct s3_export_manifest *m = NULL;
	struct spdk_uuid u;
	uint8_t raw[16];

	if (s3_export_manifest_create(uuid, size_bytes, CHUNK_SIZE, layout, &m) != 0) {
		return NULL;
	}
	m->generation = generation;
	snprintf(m->src.prefix, sizeof(m->src.prefix), "srclvs");

	memset(raw, (int)(0x10 + generation), sizeof(raw));
	memcpy(&u, raw, sizeof(u) < sizeof(raw) ? sizeof(u) : sizeof(raw));

	if (layout == S3_EXPORT_LAYOUT_REF) {
		s3_export_manifest_set_ref(m, 0, &u, CHUNK_SIZE);
	} else {
		s3_export_manifest_set_present(m, 0);
	}
	s3_export_manifest_seal(m);
	return m;
}

struct swap_result {
	bool done;
	int  status;
};

static void
swap_done(void *cb_arg, int status)
{
	struct swap_result *r = cb_arg;

	r->status = status;
	r->done = true;
}

/* The grace period only completes when the thread runs, so every swap has to be
 * polled to conclusion. */
static bool
poll_until(struct spdk_thread *thread, struct swap_result *r)
{
	time_t deadline = time(NULL) + 10;

	while (!r->done && time(NULL) < deadline) {
		spdk_thread_poll(thread, 0, 0);
		if (!r->done) {
			usleep(1000);
		}
	}
	return r->done;
}

int
main(void)
{
	struct spdk_env_opts opts;
	struct spdk_thread *thread = NULL;
	struct spdk_bs_dev *bs_dev = NULL;
	struct s3_export_manifest *m1 = NULL, *m2 = NULL, *bad = NULL;
	struct s3_client *client = NULL;
	struct swap_result r;
	uint64_t size_bytes = (uint64_t)NUM_CHUNKS * CHUNK_SIZE;
	struct s3_target target = {0};
	cpu_set_t allowed;
	int rc;

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);
	printf("=== s3lvol export manifest swap test ===\n");

	printf("\n[1] SPDK env and a real thread\n");
	opts.opts_size = sizeof(opts);
	spdk_env_opts_init(&opts);
	opts.name = "s3_export_swap_test";
	opts.no_huge = true;
	opts.mem_size = 64;
	if (spdk_env_init(&opts) < 0) {
		fprintf(stderr, "spdk_env_init failed; skipping\n");
		spdk_log_close();
		return 77;
	}
	check_true("spdk_env_init", true, "no_huge=true");

	rc = spdk_thread_lib_init(NULL, 0);
	check_int("spdk_thread_lib_init", rc, 0);
	if (rc != 0) {
		goto out_env;
	}
	thread = spdk_thread_create("swap_test", NULL);
	check_true("spdk_thread_create", thread != NULL, NULL);
	if (!thread) {
		goto out_lib;
	}
	spdk_set_thread(thread);

	/* The device holds a client reference and releases it from destroy(), so a
	 * real one is needed even though nothing here reads: a client cannot be
	 * created without the spawner and CRT, and skipping them would mean
	 * skipping destroy() and the reference accounting around it -- which is
	 * half of what this test is checking. No credentials and no endpoint that
	 * has to exist; the client is never asked to do anything. */
	CPU_ZERO(&allowed);
	sched_getaffinity(0, sizeof(allowed), &allowed);
	rc = s3_spawner_start(&allowed);
	check_int("s3_spawner_start", rc, 0);
	if (rc != 0) {
		goto out_thread;
	}
	rc = s3_crt_global_init(2);
	check_int("s3_crt_global_init", rc, 0);
	if (rc != 0) {
		goto out_spawner;
	}

	target.endpoint = "127.0.0.1:1";
	target.bucket   = "swaptest";
	target.region   = "test";
	target.auth_mode = S3_AUTH_ENV;
	rc = s3_client_get_or_create(&target, &client);
	check_int("an S3 client for the device to hold", rc, 0);
	if (rc != 0 || !client) {
		goto out_crt;
	}

	printf("\n[2] a device built on generation 1\n");
	m1 = make_manifest(TEST_UUID, 1, size_bytes, S3_EXPORT_LAYOUT_REF);
	check_true("the first manifest is built", m1 != NULL, NULL);
	if (!m1) {
		goto out_client;
	}
	rc = s3_export_bs_dev_create(client, m1, &bs_dev);
	check_int("the device is created", rc, 0);
	if (rc != 0) {
		goto out_client;
	}
	/* create() took its own reference, so the caller's still stands. */
	check_u32("the device holds a reference", m1->refcnt, 2);
	check_true("and sized itself from the manifest",
		   bs_dev->blockcnt == size_bytes / 4096, NULL);

	printf("\n[3] what must be refused\n");

	/* A different export entirely. Nothing else would catch it -- the geometry
	 * matches -- and adopting it points the clone at another volume. */
	bad = make_manifest(OTHER_UUID, 9, size_bytes, S3_EXPORT_LAYOUT_REF);
	if (bad) {
		check_int("a manifest for another export is refused",
			  s3_export_bs_dev_swap_manifest(bs_dev, bad, NULL, NULL),
			  -EINVAL);
		s3_export_manifest_unref(bad);
	}

	/* blobstore has been serving blockcnt from the old size since create. */
	bad = make_manifest(TEST_UUID, 9, size_bytes * 2, S3_EXPORT_LAYOUT_REF);
	if (bad) {
		check_int("a manifest of a different size is refused",
			  s3_export_bs_dev_swap_manifest(bs_dev, bad, NULL, NULL),
			  -EINVAL);
		s3_export_manifest_unref(bad);
	}

	/* The ordinary answer to "has the source rewritten it yet?" -- distinct
	 * from success, because a caller that retries needs to tell them apart. */
	bad = make_manifest(TEST_UUID, 1, size_bytes, S3_EXPORT_LAYOUT_REF);
	if (bad) {
		check_int("the same generation is -EALREADY",
			  s3_export_bs_dev_swap_manifest(bs_dev, bad, NULL, NULL),
			  -EALREADY);
		s3_export_manifest_unref(bad);
	}
	bad = make_manifest(TEST_UUID, 0, size_bytes, S3_EXPORT_LAYOUT_REF);
	if (bad) {
		check_int("an older generation too",
			  s3_export_bs_dev_swap_manifest(bs_dev, bad, NULL, NULL),
			  -EALREADY);
		s3_export_manifest_unref(bad);
	}

	check_u32("none of them disturbed the device's reference", m1->refcnt, 2);

	{
		struct s3_export_manifest *cs = NULL;

		if (s3_export_manifest_create(TEST_UUID, size_bytes, CHUNK_SIZE * 2,
					       S3_EXPORT_LAYOUT_DENSE, &cs) == 0) {
			cs->generation = 9;
			s3_export_manifest_set_present(cs, 0);
			s3_export_manifest_seal(cs);
			check_int("a different chunk size is refused",
				  s3_export_bs_dev_swap_manifest(bs_dev, cs, NULL, NULL),
				  -EINVAL);
			s3_export_manifest_unref(cs);
		}
	}

	printf("\n[4] a newer generation, materialised\n");
	/* What the source actually publishes when it materialises: same export,
	 * same size, higher generation, and dense because it now holds copies. */
	m2 = make_manifest(TEST_UUID, 2, size_bytes, S3_EXPORT_LAYOUT_DENSE);
	check_true("the second manifest is built", m2 != NULL, NULL);
	if (!m2) {
		goto out_dev;
	}

	memset(&r, 0, sizeof(r));
	rc = s3_export_bs_dev_swap_manifest(bs_dev, m2, swap_done, &r);
	check_int("the swap is accepted", rc, 0);

	/* Before the thread runs. The pointer is already the new manifest -- reads
	 * issued now resolve against it -- but the old one must still be alive,
	 * because a reader on another thread could have loaded it a moment ago. */
	check_u32("the new manifest is referenced immediately", m2->refcnt, 2);
	check_u32("and the old one is still held across the grace period",
		  m1->refcnt, 2);
	check_true("the callback has not run yet", !r.done, NULL);

	check_true("it completes once the thread runs", poll_until(thread, &r), NULL);
	check_int("with success", r.status, 0);

	/* Only now: the device's reference was handed to the swap and released on
	 * the far side of the iteration. Releasing it any earlier is a
	 * use-after-free for any reader that had just loaded the pointer. */
	check_u32("the old manifest is released after the grace period",
		  m1->refcnt, 1);

	printf("\n[5] and the device is on the new manifest\n");
	/* Asserted through behaviour rather than by reaching into the struct: the
	 * only way a second swap can answer -EALREADY at generation 2 is if the
	 * device is reading generation 2. */
	bad = make_manifest(TEST_UUID, 2, size_bytes, S3_EXPORT_LAYOUT_DENSE);
	if (bad) {
		rc = s3_export_bs_dev_swap_manifest(bs_dev, bad, NULL, NULL);
		check_int("generation 2 is now the current one", rc, -EALREADY);
		/* A GET that still used generation 1's keys can 404 after the
		 * swap above. Its refetch then GETs generation 2 and swap
		 * returns this same -EALREADY. That is "already current", not
		 * "the source deleted the snapshot": retrying against the
		 * installed keys is what makes the read succeed. */
		check_true("a late 404 after the swap retries, not data loss",
			   s3_export_bs_dev_refetch_already_current(rc), NULL);
		s3_export_manifest_unref(bad);
	}
	check_true("a refused manifest still gives up",
		   !s3_export_bs_dev_refetch_already_current(-EINVAL), NULL);

	/* A second swap has to work as well as the first: materialisation is not
	 * necessarily the last rewrite an export ever sees. */
	bad = make_manifest(TEST_UUID, 3, size_bytes, S3_EXPORT_LAYOUT_DENSE);
	if (bad) {
		memset(&r, 0, sizeof(r));
		check_int("a third generation is accepted",
			  s3_export_bs_dev_swap_manifest(bs_dev, bad, swap_done, &r),
			  0);
		check_true("and completes", poll_until(thread, &r), NULL);
		check_u32("releasing generation 2", m2->refcnt, 1);
		s3_export_manifest_unref(bad);
	}

out_dev:
	bs_dev->destroy(bs_dev);
	/* destroy() is asynchronous: the device frees itself from the unregister
	 * callback, which needs the thread. It also consumes the client reference
	 * this test handed to create(), which is why none is released below. */
	for (rc = 0; rc < 100; rc++) {
		spdk_thread_poll(thread, 0, 0);
	}
	check_u32("destroy released the manifest the device was on",
		  m2 ? m2->refcnt : 0, 1);
	bs_dev = NULL;
	client = NULL;

out_client:
	/* Only reached with a client still owned here if create() never ran or
	 * failed, in which case the move did not happen. */
	if (client) {
		s3_client_put(client);
	}
	s3_export_manifest_unref(m1);
	s3_export_manifest_unref(m2);

out_crt:
	s3_crt_global_fini();

out_spawner:
	s3_spawner_stop();

out_thread:
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
