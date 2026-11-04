/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * NVMf state-machine unit tests. The NVMf API is stubbed so callback timing and
 * synchronous submission failures are deterministic and need no target/DPDK.
 */

#include "spdk/stdinc.h"
#include "spdk/bdev.h"
#include "spdk/nvmf.h"

#include "vbdev_s3lvol.h"

#define MAX_SUBSYSTEMS 2

struct spdk_nvmf_tgt {
	int unused;
};

struct spdk_nvmf_ns {
	struct spdk_bdev *bdev;
	struct spdk_nvmf_ns_opts opts;
};

struct spdk_nvmf_subsystem {
	char nqn[SPDK_NVMF_NQN_MAX_LEN + 1];
	struct spdk_nvmf_ns ns;
	bool has_ns;
	spdk_nvmf_subsystem_state_change_done pause_cb;
	void *pause_arg;
	spdk_nvmf_subsystem_state_change_done resume_cb;
	void *resume_arg;
	unsigned resume_submits;
};

static struct spdk_nvmf_tgt g_tgt;
static struct spdk_nvmf_subsystem g_subsystems[MAX_SUBSYSTEMS];
static struct spdk_bdev g_bdev = {.name = "lvs/vol"};
static size_t g_subsystem_count;
static unsigned g_fail_resume_submits;
static int g_pass, g_fail;

static void
check(bool ok, const char *what)
{
	if (ok) {
		printf("  [PASS] %s\n", what);
		g_pass++;
	} else {
		printf("  [FAIL] %s\n", what);
		g_fail++;
	}
}

static void
reset_subsystems(size_t count)
{
	size_t i;

	memset(g_subsystems, 0, sizeof(g_subsystems));
	g_subsystem_count = count;
	g_fail_resume_submits = 0;
	for (i = 0; i < count; i++) {
		snprintf(g_subsystems[i].nqn, sizeof(g_subsystems[i].nqn),
			 RCOW_NQN_PREFIX "%02zu", i);
		g_subsystems[i].ns.bdev = &g_bdev;
		g_subsystems[i].ns.opts.nsid = 7;
	}
}

static void
complete_pause(struct spdk_nvmf_subsystem *subsystem, int status)
{
	spdk_nvmf_subsystem_state_change_done cb = subsystem->pause_cb;
	void *arg = subsystem->pause_arg;

	subsystem->pause_cb = NULL;
	cb(subsystem, arg, status);
}

static void
complete_resume(struct spdk_nvmf_subsystem *subsystem, int status)
{
	spdk_nvmf_subsystem_state_change_done cb = subsystem->resume_cb;
	void *arg = subsystem->resume_arg;

	subsystem->resume_cb = NULL;
	cb(subsystem, arg, status);
}

struct completion {
	bool called;
	uint32_t nsid;
	int status;
};

static void
op_done(void *arg, uint32_t nsid, int status)
{
	struct completion *done = arg;

	done->called = true;
	done->nsid = nsid;
	done->status = status;
}

static void
state_done(void *arg, int status)
{
	struct completion *done = arg;

	done->called = true;
	done->status = status;
}

/* ---- NVMf/bdev stubs used by vbdev_s3lvol_nvmf.c ---- */

struct spdk_nvmf_tgt *
spdk_nvmf_get_first_tgt(void)
{
	return &g_tgt;
}

struct spdk_nvmf_subsystem *
spdk_nvmf_tgt_find_subsystem(struct spdk_nvmf_tgt *tgt, const char *nqn)
{
	size_t i;

	(void)tgt;
	for (i = 0; i < g_subsystem_count; i++) {
		if (strcmp(g_subsystems[i].nqn, nqn) == 0) {
			return &g_subsystems[i];
		}
	}
	return NULL;
}

struct spdk_nvmf_subsystem *
spdk_nvmf_subsystem_get_first(struct spdk_nvmf_tgt *tgt)
{
	(void)tgt;
	return g_subsystem_count ? &g_subsystems[0] : NULL;
}

struct spdk_nvmf_subsystem *
spdk_nvmf_subsystem_get_next(struct spdk_nvmf_subsystem *subsystem)
{
	size_t index = (size_t)(subsystem - g_subsystems) + 1;

	return index < g_subsystem_count ? &g_subsystems[index] : NULL;
}

const char *
spdk_nvmf_subsystem_get_nqn(const struct spdk_nvmf_subsystem *subsystem)
{
	return subsystem->nqn;
}

int
spdk_nvmf_subsystem_pause(struct spdk_nvmf_subsystem *subsystem, uint32_t nsid,
			  spdk_nvmf_subsystem_state_change_done cb_fn,
			  void *cb_arg)
{
	(void)nsid;
	subsystem->pause_cb = cb_fn;
	subsystem->pause_arg = cb_arg;
	return 0;
}

int
spdk_nvmf_subsystem_resume(struct spdk_nvmf_subsystem *subsystem,
			   spdk_nvmf_subsystem_state_change_done cb_fn,
			   void *cb_arg)
{
	subsystem->resume_submits++;
	if (g_fail_resume_submits > 0) {
		g_fail_resume_submits--;
		return -EAGAIN;
	}
	subsystem->resume_cb = cb_fn;
	subsystem->resume_arg = cb_arg;
	return 0;
}

void
spdk_nvmf_ns_opts_get_defaults(struct spdk_nvmf_ns_opts *opts, size_t opts_size)
{
	memset(opts, 0, opts_size);
}

uint32_t
spdk_nvmf_subsystem_add_ns_ext(struct spdk_nvmf_subsystem *subsystem,
			       const char *bdev_name,
			       const struct spdk_nvmf_ns_opts *opts,
			       size_t opts_size, const char *ptpl_file)
{
	(void)opts_size;
	(void)ptpl_file;
	if (subsystem->has_ns || strcmp(bdev_name, g_bdev.name) != 0) {
		return 0;
	}
	subsystem->ns.bdev = &g_bdev;
	subsystem->ns.opts = *opts;
	subsystem->has_ns = true;
	return opts->nsid;
}

int
spdk_nvmf_subsystem_remove_ns(struct spdk_nvmf_subsystem *subsystem,
			      uint32_t nsid)
{
	if (!subsystem->has_ns || subsystem->ns.opts.nsid != nsid) {
		return -ENOENT;
	}
	subsystem->has_ns = false;
	return 0;
}

struct spdk_nvmf_ns *
spdk_nvmf_subsystem_get_ns(struct spdk_nvmf_subsystem *subsystem, uint32_t nsid)
{
	return subsystem->has_ns && subsystem->ns.opts.nsid == nsid ?
	       &subsystem->ns : NULL;
}

struct spdk_nvmf_ns *
spdk_nvmf_subsystem_get_first_ns(struct spdk_nvmf_subsystem *subsystem)
{
	return subsystem->has_ns ? &subsystem->ns : NULL;
}

struct spdk_nvmf_ns *
spdk_nvmf_subsystem_get_next_ns(struct spdk_nvmf_subsystem *subsystem,
				struct spdk_nvmf_ns *prev_ns)
{
	(void)subsystem;
	(void)prev_ns;
	return NULL;
}

struct spdk_bdev *
spdk_nvmf_ns_get_bdev(struct spdk_nvmf_ns *ns)
{
	return ns->bdev;
}

void
spdk_nvmf_ns_get_opts(const struct spdk_nvmf_ns *ns,
		      struct spdk_nvmf_ns_opts *opts, size_t opts_size)
{
	memcpy(opts, &ns->opts,
	       opts_size < sizeof(ns->opts) ? opts_size : sizeof(ns->opts));
}

const char *
spdk_bdev_get_name(const struct spdk_bdev *bdev)
{
	return bdev->name;
}

static void
test_resume_all_waits(void)
{
	struct completion done = {};
	int rc;

	printf("[1] resume-all completion\n");
	reset_subsystems(2);
	rc = s3lvol_nvmf_resume_all(state_done, &done);
	check(rc == 0 && !done.called, "does not complete when first resume is pending");
	complete_resume(&g_subsystems[0], 0);
	check(!done.called && g_subsystems[1].resume_cb != NULL,
	      "submits the second resume after the first completes");
	complete_resume(&g_subsystems[1], -EIO);
	check(done.called && done.status == -EIO,
	      "reports an asynchronous subsystem failure");

	done = (struct completion){};
	reset_subsystems(0);
	rc = s3lvol_nvmf_resume_all(state_done, &done);
	check(rc == 0 && done.called && done.status == 0,
	      "no subsystems completes successfully");

	done = (struct completion){};
	reset_subsystems(1);
	g_fail_resume_submits = 1;
	rc = s3lvol_nvmf_resume_all(state_done, &done);
	check(rc == 0 && done.called && done.status == -EAGAIN,
	      "synchronous resume submission failure reaches completion");
}

static void
test_add_submit_failure_rolls_back(void)
{
	struct completion done = {};
	struct spdk_nvmf_subsystem *subsystem;
	int rc;

	printf("\n[2] add rollback on synchronous resume submission failure\n");
	reset_subsystems(1);
	subsystem = &g_subsystems[0];
	g_fail_resume_submits = 1;
	rc = s3lvol_nvmf_add_ns(subsystem->nqn, g_bdev.name, 7, op_done, &done);
	check(rc == 0 && subsystem->pause_cb != NULL, "add pause submitted");
	complete_pause(subsystem, 0);
	check(!subsystem->has_ns && subsystem->resume_cb != NULL,
	      "failed resume submission rolls namespace back and retries resume");
	complete_resume(subsystem, 0);
	check(done.called && done.status == -EAGAIN && !subsystem->has_ns,
	      "failed add completes with restored namespace layout");

	done = (struct completion){};
	rc = s3lvol_nvmf_add_ns(subsystem->nqn, g_bdev.name, 7, op_done, &done);
	complete_pause(subsystem, 0);
	complete_resume(subsystem, 0);
	check(rc == 0 && done.called && done.status == 0 && subsystem->has_ns,
	      "retry add succeeds idempotently");
}

static void
test_remove_submit_failure_rolls_back(void)
{
	struct completion done = {};
	struct spdk_nvmf_subsystem *subsystem;
	int rc;

	printf("\n[3] remove rollback on synchronous resume submission failure\n");
	reset_subsystems(1);
	subsystem = &g_subsystems[0];
	subsystem->has_ns = true;
	g_fail_resume_submits = 1;
	rc = s3lvol_nvmf_remove_ns(subsystem->nqn, 7, op_done, &done);
	check(rc == 0 && subsystem->pause_cb != NULL, "remove pause submitted");
	complete_pause(subsystem, 0);
	check(subsystem->has_ns && subsystem->resume_cb != NULL,
	      "failed resume submission restores namespace and retries resume");
	complete_resume(subsystem, 0);
	check(done.called && done.status == -EAGAIN && subsystem->has_ns,
	      "failed remove completes with original namespace restored");

	done = (struct completion){};
	rc = s3lvol_nvmf_remove_ns(subsystem->nqn, 7, op_done, &done);
	complete_pause(subsystem, 0);
	complete_resume(subsystem, 0);
	check(rc == 0 && done.called && done.status == 0 && !subsystem->has_ns,
	      "retry remove succeeds idempotently");
}

static void
test_remove_async_failure_rolls_back(void)
{
	struct completion done = {};
	struct spdk_nvmf_subsystem *subsystem;
	int rc;

	printf("\n[4] remove rollback on asynchronous resume failure\n");
	reset_subsystems(1);
	subsystem = &g_subsystems[0];
	subsystem->has_ns = true;
	rc = s3lvol_nvmf_remove_ns(subsystem->nqn, 7, op_done, &done);
	check(rc == 0 && subsystem->pause_cb != NULL, "remove pause submitted");
	complete_pause(subsystem, 0);
	check(!subsystem->has_ns && subsystem->resume_cb != NULL,
	      "remove stays provisional while resume is pending");
	complete_resume(subsystem, -EIO);
	check(subsystem->has_ns && subsystem->resume_cb != NULL,
	      "asynchronous resume failure restores namespace and retries resume");
	complete_resume(subsystem, 0);
	check(done.called && done.status == -EIO && subsystem->has_ns,
	      "failed remove completes with original namespace restored");

	done = (struct completion){};
	rc = s3lvol_nvmf_remove_ns(subsystem->nqn, 7, op_done, &done);
	complete_pause(subsystem, 0);
	complete_resume(subsystem, 0);
	check(rc == 0 && done.called && done.status == 0 && !subsystem->has_ns,
	      "retry after asynchronous failure succeeds");
}

static void
test_add_async_failure_rolls_back(void)
{
	struct completion done = {};
	struct spdk_nvmf_subsystem *subsystem;
	int rc;

	printf("\n[5] add rollback on asynchronous resume failure\n");
	reset_subsystems(1);
	subsystem = &g_subsystems[0];
	rc = s3lvol_nvmf_add_ns(subsystem->nqn, g_bdev.name, 7, op_done, &done);
	check(rc == 0 && subsystem->pause_cb != NULL, "add pause submitted");
	complete_pause(subsystem, 0);
	check(subsystem->has_ns && subsystem->resume_cb != NULL,
	      "add stays provisional while resume is pending");
	complete_resume(subsystem, -EIO);
	check(!subsystem->has_ns && subsystem->resume_cb != NULL,
	      "asynchronous resume failure removes namespace and retries resume");
	complete_resume(subsystem, 0);
	check(done.called && done.status == -EIO && !subsystem->has_ns,
	      "failed add completes with original namespace layout restored");

	done = (struct completion){};
	rc = s3lvol_nvmf_add_ns(subsystem->nqn, g_bdev.name, 7, op_done, &done);
	complete_pause(subsystem, 0);
	complete_resume(subsystem, 0);
	check(rc == 0 && done.called && done.status == 0 && subsystem->has_ns,
	      "retry after asynchronous add failure succeeds");
}

int
main(void)
{
	test_resume_all_waits();
	test_add_submit_failure_rolls_back();
	test_remove_submit_failure_rolls_back();
	test_remove_async_failure_rolls_back();
	test_add_async_failure_rolls_back();
	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail ? 1 : 0;
}
