/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Spawner affinity verification -- needs no S3 backend and no credentials
 *
 *   === What this test proves ===
 *
 *   The [12b] check in s3_client_test passes "for free" on this machine: that
 *   process is never pinned to a core, so even with no spawner at all every
 *   thread has all cores available. It only guards against regressions; it
 *   cannot prove the spawner works.
 *
 *   This test builds the real scenario: **pin the main thread to a single
 *   core** (simulating an SPDK reactor pinned down by DPDK), then compare the
 *   two paths --
 *
 *     A. plain pthread_create          -> child inherits the single-core pin
 *                                         (reproduces the problem)
 *     B. via s3_spawner_pthread_create -> child gets the wide affinity
 *                                         (verifies the fix)
 *
 *   If A and B come out the same, the spawner is not doing anything.
 *
 *   Usage:
 *     ./s3_spawner_test
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"

#include "s3lvol/s3_spawner.h"

static int g_pass;
static int g_fail;

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

/* Report the size of one's own allowed CPU set */
static void *
report_affinity(void *arg)
{
	int *out = arg;
	cpu_set_t set;

	CPU_ZERO(&set);
	if (pthread_getaffinity_np(pthread_self(), sizeof(set), &set) != 0) {
		*out = -1;
	} else {
		*out = CPU_COUNT(&set);
	}
	return NULL;
}

/* For the async variant: detach itself, then write the result into a shared
 * slot and signal it */
struct async_slot {
	int             cpus;
	bool            done;
	pthread_mutex_t mutex;
	pthread_cond_t  cond;
};

static void *
report_affinity_async(void *arg)
{
	struct async_slot *slot = arg;
	cpu_set_t set;
	int n = -1;

	pthread_detach(pthread_self());

	CPU_ZERO(&set);
	if (pthread_getaffinity_np(pthread_self(), sizeof(set), &set) == 0) {
		n = CPU_COUNT(&set);
	}

	pthread_mutex_lock(&slot->mutex);
	slot->cpus = n;
	slot->done = true;
	pthread_cond_signal(&slot->cond);
	pthread_mutex_unlock(&slot->mutex);
	return NULL;
}

/* Simulates third-party library initialisation that spawns threads of its own
 * (CRT is exactly like this) */
struct nested_ctx {
	int self_cpus;
	int child_cpus;
};

static void *
nested_init(void *arg)
{
	struct nested_ctx *ctx = arg;
	cpu_set_t set;
	pthread_t child;

	CPU_ZERO(&set);
	ctx->self_cpus = (pthread_getaffinity_np(pthread_self(), sizeof(set),
						 &set) == 0)
			 ? CPU_COUNT(&set) : -1;

	/* The point: use a **bare** pthread_create here, mimicking what a
	 * third-party library does. It should inherit the spawner's wide
	 * affinity, not the original caller's single-core pin. */
	ctx->child_cpus = -1;
	if (pthread_create(&child, NULL, report_affinity, &ctx->child_cpus) == 0) {
		pthread_join(child, NULL);
	}
	return ctx;
}

int
main(void)
{
	cpu_set_t original, single, allowed;
	int total_cpus;
	int pin_cpu = -1;

	spdk_log_open(NULL);

	printf("=== spawner affinity verification ===\n\n");

	CPU_ZERO(&original);
	if (sched_getaffinity(0, sizeof(original), &original) != 0) {
		fprintf(stderr, "sched_getaffinity failed: %s\n", strerror(errno));
		return 1;
	}
	total_cpus = CPU_COUNT(&original);
	printf("usable cores: %d\n", total_cpus);

	if (total_cpus < 2) {
		printf("\nOnly %d core(s) usable; cannot distinguish \"pinned to "
		       "one core\" from \"all cores\".\n", total_cpus);
		printf("Skipping the test (not counted as a failure).\n");
		spdk_log_close();
		return 0;
	}

	/* Pick the first usable core as the "reactor core" */
	for (int i = 0; i < CPU_SETSIZE; i++) {
		if (CPU_ISSET(i, &original)) {
			pin_cpu = i;
			break;
		}
	}
	CPU_ZERO(&single);
	CPU_SET(pin_cpu, &single);

	/* The cores the spawner is allowed to use = all usable cores minus the
	 * "reactor core", exactly what the module layer would compute with
	 * spdk_app_get_core_mask() later. */
	memcpy(&allowed, &original, sizeof(allowed));
	CPU_CLR(pin_cpu, &allowed);

	printf("simulated reactor pin: CPU %d\n", pin_cpu);
	printf("cores allowed to the spawner: %d\n\n", CPU_COUNT(&allowed));

	/* ---------- start the spawner (main thread not pinned yet) ---------- */
	printf("[1] s3_spawner_start\n");
	int rc = s3_spawner_start(&allowed);
	check_true("s3_spawner_start succeeds", rc == 0, NULL);
	if (rc != 0) {
		spdk_log_close();
		return 1;
	}

	/* ---------- pin the main thread to one core, simulating an SPDK
	 * reactor ---------- */
	printf("\n[2] pin the main thread to CPU %d (simulating a DPDK-pinned "
	       "reactor)\n", pin_cpu);
	if (pthread_setaffinity_np(pthread_self(), sizeof(single), &single) != 0) {
		fprintf(stderr, "pthread_setaffinity_np failed: %s\n",
			strerror(errno));
		s3_spawner_stop();
		spdk_log_close();
		return 1;
	}
	{
		cpu_set_t verify;
		CPU_ZERO(&verify);
		pthread_getaffinity_np(pthread_self(), sizeof(verify), &verify);
		char detail[64];
		snprintf(detail, sizeof(detail), "allowed = %d cores",
			 CPU_COUNT(&verify));
		check_true("main thread is pinned to a single core",
			   CPU_COUNT(&verify) == 1, detail);
	}

	/* ---------- A. bare pthread_create: should reproduce the problem
	 * ---------- */
	printf("\n[3] control: plain pthread_create (expected to inherit the "
	       "single core = the problem)\n");
	{
		pthread_t t;
		int cpus = -1;
		rc = pthread_create(&t, NULL, report_affinity, &cpus);
		check_true("bare pthread_create succeeds", rc == 0, NULL);
		if (rc == 0) {
			pthread_join(t, NULL);
		}
		char detail[80];
		snprintf(detail, sizeof(detail), "child allowed = %d cores", cpus);
		check_true("a bare pthread_create child inherits the single-core pin",
			   cpus == 1, detail);
		if (cpus != 1) {
			printf("       (the child did not inherit the pin -- this "
			       "platform behaves differently than expected,\n");
			printf("        so the comparison below is of limited "
			       "value)\n");
		}
	}

	/* ---------- B. through the spawner: should get the wide affinity
	 * ---------- */
	printf("\n[4] experiment: s3_spawner_pthread_create\n");
	{
		pthread_t t;
		int cpus = -1;
		rc = s3_spawner_pthread_create(&t, report_affinity, &cpus);
		check_true("s3_spawner_pthread_create succeeds", rc == 0, NULL);
		if (rc == 0) {
			pthread_join(t, NULL);
		}
		char detail[80];
		snprintf(detail, sizeof(detail),
			 "child allowed = %d cores (want %d)",
			 cpus, CPU_COUNT(&allowed));
		check_true("a thread created via the spawner escapes the "
			   "single-core pin",
			   cpus == CPU_COUNT(&allowed), detail);
	}

	/* ---------- C. the async variant ---------- */
	printf("\n[5] s3_spawner_pthread_create_async\n");
	{
		struct async_slot slot = { .cpus = -1, .done = false };
		pthread_mutex_init(&slot.mutex, NULL);
		pthread_cond_init(&slot.cond, NULL);
		rc = s3_spawner_pthread_create_async(report_affinity_async, &slot,
						     NULL, NULL);
		check_true("submission succeeds", rc == 0, NULL);

		if (rc == 0) {
			pthread_mutex_lock(&slot.mutex);
			while (!slot.done) {
				pthread_cond_wait(&slot.cond, &slot.mutex);
			}
			pthread_mutex_unlock(&slot.mutex);
		}
		{
			char detail[80];
			snprintf(detail, sizeof(detail),
				 "child allowed = %d cores (want %d)",
				 slot.cpus, CPU_COUNT(&allowed));
			check_true("a thread created via async also escapes the "
				   "single-core pin",
				   slot.done && slot.cpus == CPU_COUNT(&allowed),
				   detail);
		}
		pthread_mutex_destroy(&slot.mutex);
		pthread_cond_destroy(&slot.cond);
	}

	/* ---------- D. run_task: nested creation ---------- */
	printf("\n[6] s3_spawner_run_task -- bare pthread_create inside "
	       "(simulating CRT init)\n");
	{
		struct nested_ctx ctx = { .self_cpus = -1, .child_cpus = -1 };
		void *ret = s3_spawner_run_task(nested_init, &ctx);

		check_true("run_task returns non-NULL", ret == &ctx, NULL);

		char detail[80];
		snprintf(detail, sizeof(detail), "task allowed = %d cores "
			 "(want %d)",
			 ctx.self_cpus, CPU_COUNT(&allowed));
		check_true("the task itself runs on the wide affinity",
			   ctx.self_cpus == CPU_COUNT(&allowed), detail);

		snprintf(detail, sizeof(detail), "nested child allowed = %d "
			 "cores (want %d)",
			 ctx.child_cpus, CPU_COUNT(&allowed));
		check_true("a bare pthread_create inside the task is wide too",
			   ctx.child_cpus == CPU_COUNT(&allowed), detail);
	}

	/* ---------- E. the main thread's affinity is untouched ----------
	 *
	 * This is the key advantage of the spawner over
	 * spdk_call_unaffinitized(): the latter temporarily changes the
	 * caller's own affinity, and a failed restore leaves the reactor
	 * permanently off its pinned core. The spawner never touches the
	 * caller.
	 */
	printf("\n[7] the main thread's affinity is untainted (the spawner's "
	       "key advantage over unaffinitized)\n");
	{
		cpu_set_t after;
		CPU_ZERO(&after);
		pthread_getaffinity_np(pthread_self(), sizeof(after), &after);
		char detail[96];
		snprintf(detail, sizeof(detail),
			 "still %d core(s), still CPU %d",
			 CPU_COUNT(&after), pin_cpu);
		check_true("the main thread is still pinned to its original core",
			   CPU_COUNT(&after) == 1 && CPU_ISSET(pin_cpu, &after),
			   detail);
	}

	/* ---------- F. submissions are refused after stop ---------- */
	printf("\n[8] submissions should be refused after s3_spawner_stop\n");
	s3_spawner_stop();
	check_true("is_started is false after stop",
		   !s3_spawner_is_started(), NULL);
	{
		pthread_t t;
		rc = s3_spawner_pthread_create(&t, report_affinity, NULL);
		check_true("pthread_create fails rather than hangs after stop",
			   rc != 0, NULL);

		void *ret = s3_spawner_run_task(nested_init, NULL);
		check_true("run_task returns NULL after stop", ret == NULL, NULL);
	}

	printf("\n[9] repeated start/stop is idempotent\n");
	check_true("stop is idempotent (a second call does not crash)",
		   (s3_spawner_stop(), true), NULL);
	rc = s3_spawner_start(&allowed);
	check_true("start works again after stop", rc == 0, NULL);
	rc = s3_spawner_start(&allowed);
	check_true("repeated start returns 0", rc == 0, NULL);
	s3_spawner_stop();

	/* Restore the main thread's affinity, to avoid affecting anything
	 * that follows (though this process is about to exit anyway) */
	pthread_setaffinity_np(pthread_self(), sizeof(original), &original);

	spdk_log_close();

	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
