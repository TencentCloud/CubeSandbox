/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_nvmf -- attach and detach a bdev as an NVMf namespace
 *
 *   Wraps the pause / mutate / resume dance that spdk_nvmf_subsystem_add_ns_ext()
 *   and spdk_nvmf_subsystem_remove_ns() require. Both must be called while the
 *   subsystem is paused, and both pause and resume are asynchronous, so a single
 *   logical operation is a three-callback chain.
 *
 *   === Why the failure paths get so much attention ===
 *
 *   A subsystem left paused is not a failed operation, it is 64 unusable
 *   volumes: pausing freezes the admin queue for the whole subsystem, and the
 *   other namespaces on it are collateral. So every path that pauses must reach
 *   a resume, including the ones that failed, and the resume itself has to be
 *   handled when it fails.
 *
 *   The structure is lifted from SPDK's own rpc_nvmf_subsystem_add_ns (see
 *   lib/nvmf/nvmf_rpc.c), including its failback: if the namespace was added but
 *   the resume then failed, the namespace is removed again and a second resume
 *   attempted, because reporting success for a namespace nobody can reach is
 *   worse than reporting the failure.
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"
#include "spdk/nvmf.h"
#include "spdk/string.h"

#include "vbdev_s3lvol.h"

SPDK_LOG_REGISTER_COMPONENT(s3lvol_nvmf)

struct nvmf_op_ctx {
	struct spdk_nvmf_subsystem *subsystem;

	/* Copied: the caller's strings need not outlive the first callback. */
	char      bdev_name[SPDK_LVOL_NAME_MAX + SPDK_LVS_NAME_MAX + 2];
	uint32_t  nsid;
	bool      removing;

	/* Result of the mutation, reported after the resume completes. */
	int       status;

	s3lvol_nvmf_op_cb cb_fn;
	void             *cb_arg;
};

void
s3lvol_nvmf_subsys_nqn(uint32_t index, char *out, size_t out_len)
{
	snprintf(out, out_len, RCOW_NQN_PREFIX "%02" PRIu32, index);
}

/* --------------------------------------------------------------------------
 * Completion
 * -------------------------------------------------------------------------- */

static void
nvmf_op_done(struct nvmf_op_ctx *ctx, int status)
{
	s3lvol_nvmf_op_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;
	uint32_t nsid = ctx->nsid;

	free(ctx);

	if (cb_fn) {
		cb_fn(cb_arg, nsid, status);
	}
}

/* The subsystem is running again. Report whatever the mutation did. */
static void
nvmf_op_resumed(struct spdk_nvmf_subsystem *subsystem, void *cb_arg, int status)
{
	struct nvmf_op_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("subsystem '%s' failed to resume: %s. It is left paused; "
			    "every namespace on it is now unreachable\n",
			    spdk_nvmf_subsystem_get_nqn(subsystem),
			    spdk_strerror(-status));
		nvmf_op_done(ctx, status);
		return;
	}

	nvmf_op_done(ctx, ctx->status);
}

/* The namespace was added but the subsystem could not be resumed, so the
 * addition has been undone. This is the second resume attempt; there is nothing
 * further to try after it. */
static void
nvmf_op_failback_resumed(struct spdk_nvmf_subsystem *subsystem, void *cb_arg,
			 int status)
{
	struct nvmf_op_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("subsystem '%s' could not be resumed even after rolling "
			    "back nsid %" PRIu32 "; it stays paused\n",
			    spdk_nvmf_subsystem_get_nqn(subsystem), ctx->nsid);
	}

	/* Report the resume failure, not the add: the namespace is gone. */
	nvmf_op_done(ctx, ctx->status != 0 ? ctx->status : -EIO);
}

static void
nvmf_op_add_resumed(struct spdk_nvmf_subsystem *subsystem, void *cb_arg,
		    int status)
{
	struct nvmf_op_ctx *ctx = cb_arg;
	int rc;

	if (status == 0) {
		nvmf_op_done(ctx, ctx->status);
		return;
	}

	/* Resume failed after a successful add. Take the namespace back out, so
	 * that a retry is not blocked by a half-applied nsid, then try once more
	 * to get the subsystem running. */
	if (ctx->status == 0 && ctx->nsid != 0) {
		SPDK_ERRLOG("subsystem '%s' failed to resume after adding nsid "
			    "%" PRIu32 "; rolling the namespace back\n",
			    spdk_nvmf_subsystem_get_nqn(subsystem), ctx->nsid);

		if (spdk_nvmf_subsystem_remove_ns(subsystem, ctx->nsid) != 0) {
			SPDK_ERRLOG("rollback of nsid %" PRIu32 " failed too\n",
				    ctx->nsid);
		}
		ctx->status = status;

		rc = spdk_nvmf_subsystem_resume(subsystem, nvmf_op_failback_resumed,
						ctx);
		if (rc != 0) {
			SPDK_ERRLOG("subsystem '%s' stays paused: %s\n",
				    spdk_nvmf_subsystem_get_nqn(subsystem),
				    spdk_strerror(-rc));
			nvmf_op_done(ctx, status);
		}
		return;
	}

	nvmf_op_done(ctx, status);
}

/* --------------------------------------------------------------------------
 * Paused-state mutations
 * -------------------------------------------------------------------------- */

static void
nvmf_op_paused(struct spdk_nvmf_subsystem *subsystem, void *cb_arg, int status)
{
	struct nvmf_op_ctx *ctx = cb_arg;
	spdk_nvmf_subsystem_state_change_done resume_cb;
	int rc;

	if (status != 0) {
		/* Never entered the paused state, so no resume is owed. */
		SPDK_ERRLOG("subsystem '%s' failed to pause: %s\n",
			    spdk_nvmf_subsystem_get_nqn(subsystem),
			    spdk_strerror(-status));
		nvmf_op_done(ctx, status);
		return;
	}

	if (ctx->removing) {
		ctx->status = spdk_nvmf_subsystem_remove_ns(subsystem, ctx->nsid);
		if (ctx->status != 0) {
			SPDK_ERRLOG("subsystem '%s': cannot remove nsid %" PRIu32
				    ": %s\n", spdk_nvmf_subsystem_get_nqn(subsystem),
				    ctx->nsid, spdk_strerror(-ctx->status));
		}
		/* Nothing to roll back on the remove path: a removal that failed
		 * changed nothing. */
		resume_cb = nvmf_op_resumed;
	} else {
		struct spdk_nvmf_ns_opts ns_opts;

		spdk_nvmf_ns_opts_get_defaults(&ns_opts, sizeof(ns_opts));

		/* The requested nsid, not a suggestion: recovery has to reproduce the
		 * exact layout, and add_ns_ext() returns 0 rather than relocating if
		 * the id is taken or out of range. Note that a subsystem created
		 * without max_namespaces >= this value rejects it with "NSID greater
		 * than maximum not allowed", which only shows up in the target log. */
		ns_opts.nsid = ctx->nsid;

		/* uuid is left at its default so that add_ns_ext() takes it from the
		 * bdev. That is what makes the host expose the lvol uuid, which is the
		 * only stable way back from a volume to its device path. */
		ctx->nsid = spdk_nvmf_subsystem_add_ns_ext(subsystem, ctx->bdev_name,
							&ns_opts, sizeof(ns_opts),
							   NULL);
		if (ctx->nsid == 0) {
			SPDK_ERRLOG("subsystem '%s': cannot add bdev '%s' as nsid "
				    "%" PRIu32 " (taken, out of range, or bdev "
				    "missing)\n",
				    spdk_nvmf_subsystem_get_nqn(subsystem),
				    ctx->bdev_name, ns_opts.nsid);
			ctx->status = -EINVAL;
		}
		resume_cb = nvmf_op_add_resumed;
	}

	rc = spdk_nvmf_subsystem_resume(subsystem, resume_cb, ctx);
	if (rc != 0) {
		/* The resume could not even be submitted. The subsystem stays
		 * paused -- this is the one outcome with no recovery from here. */
		SPDK_ERRLOG("subsystem '%s' stays paused, resume could not be "
			    "submitted: %s\n",
			    spdk_nvmf_subsystem_get_nqn(subsystem),
			    spdk_strerror(-rc));
		nvmf_op_done(ctx, rc);
	}
}

/* --------------------------------------------------------------------------
 * Entry points
 * -------------------------------------------------------------------------- */

static struct spdk_nvmf_subsystem *
nvmf_find_subsystem(const char *nqn)
{
	struct spdk_nvmf_tgt *tgt;

	/* The first target is the one an SPDK app creates for itself; this module
	 * does not create targets of its own. */
	tgt = spdk_nvmf_get_first_tgt();
	if (!tgt) {
		SPDK_ERRLOG("no NVMf target exists yet\n");
		return NULL;
	}

	return spdk_nvmf_tgt_find_subsystem(tgt, nqn);
}

static int
nvmf_op_start(const char *nqn, const char *bdev_name, uint32_t nsid,
	      bool removing, s3lvol_nvmf_op_cb cb_fn, void *cb_arg)
{
	struct spdk_nvmf_subsystem *subsystem;
	struct nvmf_op_ctx *ctx;
	int rc;

	subsystem = nvmf_find_subsystem(nqn);
	if (!subsystem) {
		SPDK_ERRLOG("no such NVMf subsystem: %s\n", nqn);
		return -ENODEV;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}

	ctx->subsystem = subsystem;
	ctx->nsid= nsid;
	ctx->removing  = removing;
	ctx->cb_fn     = cb_fn;
	ctx->cb_arg    = cb_arg;
	if (bdev_name) {
		snprintf(ctx->bdev_name, sizeof(ctx->bdev_name), "%s", bdev_name);
	}

	/* Pause the namespace being operated on, not "the subsystem".
	 *
	 * nsid is not a hint here, and 0 is not a wildcard. From nvmf.h:
	 *
	 *   In a paused state, all admin queues are frozen across the whole
	 *   subsystem. If a namespace ID is provided, all commands to that
	 *   namespace are quiesced [...]
	 *   \param nsid The namespace to pause. If 0, pause no namespaces.
	 *
	 * This used to pass 0, on the assumption that it meant "all of them". It
	 * means the opposite, and the consequence was not subtle: nvmf.c:2026 only
	 * marks a namespace PAUSING when `nsid - 1 < num_ns`, which is false for 0,
	 * so nothing was quiesced, the drain loop had no namespace to wait on, and
	 * the pause completed immediately while the host kept reading. remove_ns
	 * then ran and the resume released the namespace's bdev channel with I/O
	 * still on it, which is an assert in the bdev layer:
	 *
	 *   bdev_channel_destroy_resource: Assertion `TAILQ_EMPTY(&ch->io_submitted)'
	 *
	 * It took a host read in flight to show up, so it survived every test that
	 * activated and deactivated an idle volume, and it was the udev probe of a
	 * freshly appeared device that finally raced it.
	 *
	 * Per-namespace is also the granularity we want rather than a compromise:
	 * adding volume 37 to a subsystem must not stall the 36 already serving I/O
	 * from it. SPDK's own add_ns and remove_ns RPCs pass the namespace id for
	 * the same reason (nvmf_rpc.c:1293 and :1434); only the listener and ANA
	 * RPCs, which touch no namespace I/O, pass 0.
	 *
	 * On the add path the namespace does not exist yet, so there is nothing to
	 * drain -- but ns_info[nsid - 1] does exist (the array is sized by the
	 * subsystem's max_nsid), so the call is well defined and costs a state flag.
	 */
	rc = spdk_nvmf_subsystem_pause(subsystem, nsid, nvmf_op_paused, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("subsystem '%s': pause could not be submitted: %s\n",
			    nqn, spdk_strerror(-rc));
		free(ctx);
		return rc;
	}

	return 0;
}

int
s3lvol_nvmf_add_ns(const char *nqn, const char *bdev_name, uint32_t nsid,
		   s3lvol_nvmf_op_cb cb_fn, void *cb_arg)
{
	if (!nqn || !bdev_name || nsid == 0) {
		return -EINVAL;
	}
	return nvmf_op_start(nqn, bdev_name, nsid, false, cb_fn, cb_arg);
}

int
s3lvol_nvmf_remove_ns(const char *nqn, uint32_t nsid, s3lvol_nvmf_op_cb cb_fn,
		      void *cb_arg)
{
	if (!nqn || nsid == 0) {
		return -EINVAL;
	}
	return nvmf_op_start(nqn, NULL, nsid, true, cb_fn, cb_arg);
}

bool
s3lvol_nvmf_subsys_exists(uint32_t index)
{
	char nqn[SPDK_NVMF_NQN_MAX_LEN + 1];

	s3lvol_nvmf_subsys_nqn(index, nqn, sizeof(nqn));
	return nvmf_find_subsystem(nqn) != NULL;
}

/* --------------------------------------------------------------------------
 * Host-side device lookup
 *
 * This is the one part of the module that reaches across to the host: NQN and
 * nsid are target-side names, /dev/nvmeXnY is allocated by the host kernel, and
 * nothing on the target knows the mapping. It works here only because the target
 * and the initiator are the same machine.
 *
 * The lookup is by uuid rather than by nsid, and that is not a stylistic choice.
 * Measured on a live target: nsid 7 appeared as /dev/nvme0n1 and nsid 42 as
 * /dev/nvme0n2 -- the host numbers namespaces in discovery order. Reactivate
 * them in the opposite order and the two paths trade places, so any arithmetic
 * from nsid to a device name is wrong as soon as anything is detached and
 * reattached. The uuid, on the other hand, is the lvol's own and travels
 * unchanged: vbdev_s3lvol_lvol.c assigns bdev->uuid = lvol->uuid, add_ns takes
 * the namespace uuid from the bdev, and the host publishes it in sysfs.
 * -------------------------------------------------------------------------- */

/* Read a one-line sysfs attribute, trimming the newline. */
static bool
sysfs_read_line(const char *dir, const char *attr, char *out, size_t out_len)
{
	char path[PATH_MAX];
	FILE *fp;
	size_t len;

	if (snprintf(path, sizeof(path), "%s/%s", dir, attr) >= (int)sizeof(path)) {
		return false;
	}

	fp = fopen(path, "r");
	if (!fp) {
		return false;
	}
	if (!fgets(out, (int)out_len, fp)) {
		fclose(fp);
		return false;
	}
	fclose(fp);

	len = strlen(out);
	while (len > 0 && (out[len - 1] == '\n' || out[len - 1] == '\r')) {
		out[--len] = '\0';
	}
	return len > 0;
}

int
s3lvol_nvmf_resolve_device(const char *uuid_str, char *out, size_t out_len)
{
	DIR *d;
	struct dirent *ent;
	int rc = -ENOENT;

	if (!uuid_str || !uuid_str[0] || !out || out_len == 0) {
		return -EINVAL;
	}

	d = opendir("/sys/block");
	if (!d) {
		SPDK_ERRLOG("cannot read /sys/block: %s\n", strerror(errno));
		return -errno;
	}

	while ((ent = readdir(d)) != NULL) {
		char dir[PATH_MAX];
		char value[SPDK_UUID_STRING_LEN + 16];

		if (strncmp(ent->d_name, "nvme", 4) != 0) {
			continue;
		}

		/* Skip the per-controller sibling. A connected subsystem shows up
		 * twice -- as nvme0n1 and as nvme0c0n1 -- carrying the same uuid,
		 * and the c-form is the hidden path underneath the multipath device,
		 * not something to hand out for mounting. Without this filter the
		 * scan returns whichever readdir happens to yield first. */
		if (strchr(ent->d_name + 4, 'c') != NULL) {
			continue;
		}

		if (snprintf(dir, sizeof(dir), "/sys/block/%s", ent->d_name) >=
		    (int)sizeof(dir)) {
			continue;
		}

		/* uuid is the direct match. wwid is the fallback for kernels that do
		 * not publish uuid separately; it reads "uuid.<the uuid>". */
		if (sysfs_read_line(dir, "uuid", value, sizeof(value)) &&
		    strcasecmp(value, uuid_str) == 0) {
			snprintf(out, out_len, "/dev/%s", ent->d_name);
			rc = 0;
			break;
		}
		if (sysfs_read_line(dir, "wwid", value, sizeof(value)) &&
		    strcasestr(value, uuid_str) != NULL) {
			snprintf(out, out_len, "/dev/%s", ent->d_name);
			rc = 0;
			break;
		}
	}

	closedir(d);
	return rc;
}

uint32_t
s3lvol_nvmf_readahead_kb(void)
{
	static uint32_t cached;
	static bool resolved;
	const char *env;

	if (resolved) {
		return cached;
	}

	cached = RCOW_DEFAULT_READ_AHEAD_KB;
	env = getenv(S3LVOL_READ_AHEAD_ENV);
	if (env && env[0]) {
		char *end = NULL;
		unsigned long v = strtoul(env, &end, 10);

		if (end && *end == '\0' && v <= 32768) {
			/* 0 is meaningful: leave every device alone. */
			cached = (uint32_t)v;
		} else {
			SPDK_WARNLOG("%s='%s' is not a readahead in KiB; using %d\n",
				     S3LVOL_READ_AHEAD_ENV, env,
				     RCOW_DEFAULT_READ_AHEAD_KB);
		}
	}

	resolved = true;
	return cached;
}

bool
s3lvol_nvmf_set_readahead(const char *leaf, uint32_t kb)
{
	char path[PATH_MAX];
	char cur[32];
	uint32_t cur_kb = 0;
	bool have_cur = false;
	FILE *fp;

	if (!leaf || !leaf[0] || kb == 0) {
		return false;
	}

	/* Reject anything that could climb out of the directory. leaf comes from
	 * readdir on /sys/block in the only caller today, but this is a public
	 * function taking a string that ends up in a path, and the whole point of
	 * it is to write to that path. */
	if (strchr(leaf, '/') != NULL || strcmp(leaf, ".") == 0 ||
	    strcmp(leaf, "..") == 0) {
		return false;
	}

	if (snprintf(path, sizeof(path),
		     "/sys/block/%s/queue/read_ahead_kb", leaf) >=
	    (int)sizeof(path)) {
		return false;
	}

	fp = fopen(path, "r");
	if (fp) {
		if (fgets(cur, sizeof(cur), fp)) {
			cur_kb = (uint32_t)strtoul(cur, NULL, 10);
			have_cur = true;
		}
		fclose(fp);
	}

	if (have_cur) {
		if (cur_kb == kb) {
			/* The common case: every later lookup of a volume that was
			 * tuned on the first one. */
			return true;
		}

		/* Anything other than the kernel default means somebody decided
		 * this on purpose -- the operator, through RCOW_READ_AHEAD_KB, or
		 * a tuning layer above. Overwriting it would make that setting
		 * silently temporary, undone by the next lookup of the device,
		 * and 128 is a legitimate choice for a purely random workload.
		 *
		 * So this function only ever moves a device *off* the kernel
		 * default. That is what makes it safe to call on every lookup. */
		if (cur_kb != S3LVOL_KERNEL_DEFAULT_READ_AHEAD_KB) {
			SPDK_INFOLOG(s3lvol_nvmf, "%s: leaving readahead at %"
				     PRIu32 " KiB, which is not the kernel "
				     "default and so was set deliberately\n",
				     leaf, cur_kb);
			return false;
		}
	}

	fp = fopen(path, "w");
	if (!fp) {
		/* Not an error worth logging at error level: a volume with the
		 * kernel default readahead works, it just reads cold data more
		 * slowly. */
		SPDK_INFOLOG(s3lvol_nvmf, "cannot set readahead on %s: %s\n",
			     leaf, strerror(errno));
		return false;
	}

	if (fprintf(fp, "%" PRIu32 "\n", kb) < 0) {
		fclose(fp);
		return false;
	}
	if (fclose(fp) != 0) {
		return false;
	}

	SPDK_NOTICELOG("%s: readahead set to %" PRIu32 " KiB\n", leaf, kb);
	return true;
}
