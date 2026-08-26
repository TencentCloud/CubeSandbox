/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Callback thread bounce verification -- needs a real SPDK thread, no S3
 *   backend
 *
 *   === Why it has to be its own test ===
 *
 *   s3_client_test submits from plain pthreads, so req->owner_thread is always
 *   NULL and it takes the "run the callback in place" branch -- **the bounce
 *   path is never exercised**. Verifying the bounce requires a real
 *   spdk_thread.
 *
 *   This test uses spdk_thread_lib_init to bring up a real SPDK thread (no
 *   reactor framework needed, no hugepages either -- DPDK comes up with
 *   --no-huge), submits requests on it, and then confirms:
 *     1. the callback really runs on the submitting thread, not on a CRT I/O
 *        thread;
 *     2. the callback is not synchronous -- it is only processed by polling;
 *     3. the submitting thread's id and the callback thread's id match.
 *
 *   No real S3 is touched: the endpoint is deliberately unreachable so
 *   requests fail fast. **The bounce mechanism is independent of request
 *   success** -- the failure path bounces too, which this test exercises in
 *   the same pass.
 *
 *   Usage:
 *     ./s3_thread_bounce_test
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"

#include "s3lvol/s3_client.h"
#include "s3lvol/s3_spawner.h"

static int g_pass;
static int g_fail;

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-44s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-44s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

/* ==========================================================================
 * The callback under observation
 * ========================================================================== */

struct bounce_probe {
	/* recorded at submission */
	struct spdk_thread *submit_thread;
	pthread_t           submit_tid;

	/* recorded at callback time */
	struct spdk_thread *cb_thread;
	pthread_t           cb_tid;
	int                 cb_status;

	/* volatile: read by this thread when polling, written by a CRT thread
	 * on the "run in place" degraded path. On the normal bounce path both
	 * sides are the same thread, but the degraded path must still be
	 * observable. */
	volatile bool       done;

	/* whether it finished before the first poll -- proves the callback is
	 * not synchronous */
	bool                done_before_poll;
};

static void
bounce_op_cb(void *cb_arg, int status)
{
	struct bounce_probe *p = cb_arg;

	p->cb_thread = spdk_get_thread();
	p->cb_tid    = pthread_self();
	p->cb_status = status;
	p->done      = true;
}

static void
bounce_get_cb(void *cb_arg, uint64_t bytes_read, int status)
{
	(void)bytes_read;
	bounce_op_cb(cb_arg, status);
}

/* ==========================================================================
 * main
 * ========================================================================== */

#define POLL_TIMEOUT_SEC 60

/* Poll until the probe is done or the timeout elapses. Returns whether it
 * finished. */
static bool
poll_until_done(struct spdk_thread *thread, struct bounce_probe *p)
{
	time_t deadline = time(NULL) + POLL_TIMEOUT_SEC;

	while (!p->done && time(NULL) < deadline) {
		spdk_thread_poll(thread, 0, 0);
		if (!p->done) {
			usleep(1000);
		}
	}
	return p->done;
}

int
main(void)
{
	struct spdk_env_opts opts;
	struct spdk_thread *thread = NULL;
	cpu_set_t allowed;
	int rc;

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== callback thread bounce verification ===\n\n");

	/* ---------- DPDK env: --no-huge avoids the hugepages requirement
	 * ---------- */
	printf("[1] SPDK env init (--no-huge)\n");
	opts.opts_size = sizeof(opts);
	spdk_env_opts_init(&opts);
	opts.name     = "s3_thread_bounce_test";
	opts.no_huge  = true;
	/* Keep the memory small; this test moves no data.
	 * Do not set iova_mode yourself -- under no_huge SPDK appends
	 * --iova-mode=va automatically, and setting it again makes DPDK report
	 * "should not occur multiple times". */
	opts.mem_size = 64;

	if (spdk_env_init(&opts) < 0) {
		fprintf(stderr, "spdk_env_init failed -- this test needs DPDK "
			"to initialise.\n");
		fprintf(stderr, "If the environment forbids it (no permissions "
			"etc.), skip this test.\n");
		spdk_log_close();
		return 77;   /* 77 = skip, the automake convention */
	}
	check_true("spdk_env_init", true, "no_huge=true");

	/* ---------- SPDK thread lib ---------- */
	printf("\n[2] spdk_thread_lib_init + create a real SPDK thread\n");
	rc = spdk_thread_lib_init(NULL, 0);
	check_true("spdk_thread_lib_init", rc == 0, NULL);
	if (rc != 0) {
		goto out_env;
	}

	thread = spdk_thread_create("bounce_test", NULL);
	check_true("spdk_thread_create", thread != NULL, NULL);
	if (!thread) {
		goto out_thread_lib;
	}

	/* Bind this pthread as the execution context of that spdk_thread.
	 * From here on spdk_get_thread() on this pthread returns thread. */
	spdk_set_thread(thread);
	check_true("spdk_get_thread() returns the thread just created",
		   spdk_get_thread() == thread, NULL);

	/* ---------- spawner + CRT ---------- */
	printf("\n[3] spawner and CRT init\n");
	CPU_ZERO(&allowed);
	if (sched_getaffinity(0, sizeof(allowed), &allowed) != 0) {
		fprintf(stderr, "sched_getaffinity failed\n");
		goto out_thread;
	}
	rc = s3_spawner_start(&allowed);
	check_true("s3_spawner_start", rc == 0, NULL);
	if (rc != 0) {
		goto out_thread;
	}

	rc = s3_crt_global_init(2);
	check_true("s3_crt_global_init", rc == 0, NULL);
	if (rc != 0) {
		goto out_spawner;
	}

	/* Point at an unreachable endpoint: the request fails, but the bounce
	 * mechanism is independent of success. 127.0.0.1:1 (connection
	 * refused) is far faster than waiting for a DNS timeout. */
	struct s3_target target = {
		.endpoint       = (char *)"127.0.0.1:1",
		.region         = (char *)"us-east-1",
		.bucket         = (char *)"bouncetest",
		.auth_mode      = S3_AUTH_STATIC,
		.access_key     = (char *)"test",
		.secret_key     = (char *)"test",
		.use_path_style = true,
		.verify_tls     = false,
	};
	struct s3_client *client = NULL;

	rc = s3_client_get_or_create(&target, &client);
	check_true("s3_client_get_or_create", rc == 0 && client != NULL, NULL);
	if (rc != 0 || !client) {
		goto out_fini;
	}

	/* ---------- 4. HEAD: verify the callback bounces back to this thread
	 * ---------- */
	printf("\n[4] s3_head -- the callback should bounce back to the "
	       "submitting thread\n");
	{
		struct bounce_probe p = {0};
		p.submit_thread = spdk_get_thread();
		p.submit_tid    = pthread_self();

		uint64_t size = 0;
		rc = s3_head(client, "bounce/probe-head", &size, bounce_op_cb, &p);
		check_true("s3_head submission succeeds", rc == 0, NULL);

		if (rc == 0) {
			/* Key assertion 1: the callback must **not** have run
			 * by the time submission returns. If it has, the
			 * callback is synchronous / never went through the
			 * message queue. */
			p.done_before_poll = p.done;
			check_true("the callback has not run when submission "
				   "returns (it went through the message queue)",
				   !p.done_before_poll,
				   p.done_before_poll ? "!! callback is "
				   "synchronous" : "not run");

			bool finished = poll_until_done(thread, &p);
			check_true("the callback runs after polling", finished,
				   finished ? NULL : "!! timed out, callback "
				   "did not bounce back");

			if (finished) {
				/* Key assertion 2: the callback's spdk_thread
				 * matches the submitting one */
				check_true("the callback runs on the submitting "
					   "spdk_thread",
					   p.cb_thread == p.submit_thread, NULL);

				/* Key assertion 3: the pthread is the same too --
				 * ruling out "another pthread happened to be
				 * bound to the same spdk_thread" */
				char detail[96];
				snprintf(detail, sizeof(detail),
					 "submit=%lx callback=%lx",
					 (unsigned long)p.submit_tid,
					 (unsigned long)p.cb_tid);
				check_true("callback and submission are on the "
					   "same pthread",
					   pthread_equal(p.submit_tid, p.cb_tid),
					   detail);

				/* The request itself is expected to fail (the
				 * endpoint is unreachable), which is exactly
				 * what proves **the failure path bounces
				 * too** */
				snprintf(detail, sizeof(detail), "status=%d",
					 p.cb_status);
				check_true("the failure path bounces as well "
					   "(status != 0)",
					   p.cb_status != 0, detail);
			}
		}
	}

	/* ---------- 5. GET: verify the byte-carrying callback bounces too
	 * ---------- */
	printf("\n[5] s3_get_range -- get_cb bounces as well\n");
	{
		struct bounce_probe p = {0};
		char buf[4096];

		p.submit_thread = spdk_get_thread();
		p.submit_tid    = pthread_self();

		rc = s3_get_range(client, "bounce/probe-get", 0, sizeof(buf), buf,
				  bounce_get_cb, &p);
		check_true("s3_get_range submission succeeds", rc == 0, NULL);

		if (rc == 0) {
			check_true("the callback has not run when submission "
				   "returns", !p.done, NULL);
			bool finished = poll_until_done(thread, &p);
			check_true("the callback runs after polling", finished, NULL);
			if (finished) {
				check_true("get_cb also runs on the submitting "
					   "thread",
					   p.cb_thread == p.submit_thread &&
					   pthread_equal(p.submit_tid, p.cb_tid),
					   NULL);
			}
		}
	}

	/* ---------- 6. delete_batch: the fan-out aggregate callback bounces
	 * too ---------- */
	printf("\n[6] s3_delete_batch -- fan-out aggregate callback bounce\n");
	{
		struct bounce_probe p = {0};
		const char *keys[4] = {
			"bounce/b0", "bounce/b1", "bounce/b2", "bounce/b3",
		};

		p.submit_thread = spdk_get_thread();
		p.submit_tid    = pthread_self();

		rc = s3_delete_batch(client, keys, 4, bounce_op_cb, &p);
		check_true("s3_delete_batch submission succeeds", rc == 0, NULL);

		if (rc == 0) {
			check_true("the aggregate callback has not run when "
				   "submission returns", !p.done, NULL);
			bool finished = poll_until_done(thread, &p);
			check_true("the aggregate callback runs after polling",
				   finished, NULL);
			if (finished) {
				/* This is the easiest one to get wrong: the
				 * batch completes when the **last sub-request
				 * to land** finishes, and that is some
				 * arbitrary CRT I/O thread. The bounce back
				 * must come from batch->owner_thread. */
				check_true("the aggregate callback runs on the "
					   "submitting thread (not the last "
					   "CRT thread)",
					   p.cb_thread == p.submit_thread &&
					   pthread_equal(p.submit_tid, p.cb_tid),
					   NULL);
			}
		}
	}

	/* ---------- 7. stats ---------- */
	printf("\n[7] stats (the atomic counters should converge exactly)\n");
	{
		struct s3_client_stats stats;
		s3_client_get_stats(client, &stats);
		printf("     head=%" PRIu64 " get=%" PRIu64 " delete=%" PRIu64
		       " inflight=%" PRIu64 "\n",
		       stats.head_ops, stats.get_ops, stats.delete_ops,
		       stats.inflight);
		check_true("inflight has returned to zero",
			   stats.inflight == 0, NULL);
		/* a batch of 4 keys -> 4 DeleteObject calls */
		char detail[64];
		snprintf(detail, sizeof(detail), "delete_ops=%" PRIu64 " (want 4)",
			 stats.delete_ops);
		check_true("the fan-out delete_ops exactly equals the key count",
			   stats.delete_ops == 4, detail);
	}

	s3_client_put(client);

out_fini:
	printf("\n[8] teardown\n");
	s3_crt_global_fini();
out_spawner:
	s3_spawner_stop();
out_thread:
	if (thread) {
		spdk_thread_exit(thread);
		while (!spdk_thread_is_exited(thread)) {
			spdk_thread_poll(thread, 0, 0);
		}
		spdk_thread_destroy(thread);
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
