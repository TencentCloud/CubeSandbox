/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol -- module registration and global initialisation
 *
 *   This file owns two things that must happen at module init and can only
 *   happen at this layer:
 *
 *   1. **Start the thread spawner and compute its cpuset.**
 *      cpuset = cores this process may use - this process's reactor cores.
 *      Computing the
 *      complement needs spdk_env_get_core_mask(), which is an env-layer API,
 *      while lib/s3bsdev must stay unit-testable outside the app framework,
 *      so the computation lives in the module layer and the cpuset is injected
 *      into s3_spawner_start().
 *
 *   2. **Initialise CRT.** s3_crt_global_init() must be called after the
 *      spawner -- it pthread_creates the event loop / resolver / logger
 *      threads internally, and those threads would inherit the reactor's
 *      single-core affinity if created on a reactor.
 */

#include "spdk/stdinc.h"
#include "spdk/bdev_module.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"

#include "s3lvol/s3_client.h"
#include "s3lvol/s3_spawner.h"

#include "vbdev_s3lvol.h"

/* Number of CRT event loop threads. Kept below the reactor core count -- they
 * only do network I/O and do not need one thread per core. */
#define S3LVOL_CRT_THREADS 4

static bool g_s3lvol_initialized = false;

/* The CPUs this process was allowed to run on before DPDK EAL pinned the main
 * thread to the master lcore. Taken from a constructor because by module_init
 * sched_getaffinity(0) only reports that one core.
 *
 * It matters whenever the target runs under a cpuset smaller than the machine:
 * a Kubernetes container with a CPU limit, a systemd unit with CPUAffinity= or
 * AllowedCPUs=, a taskset launch. "Every CPU minus the reactors" is then mostly
 * CPUs the kernel will not grant, and when none of them is allowed
 * pthread_setaffinity_np fails with EINVAL. The spawner then keeps the main
 * lcore's affinity, and every CRT event loop, the resolver and the logger end
 * up on the core that also hosts a reactor (#2288). */
static cpu_set_t g_s3lvol_startup_cpus;
static bool g_s3lvol_startup_cpus_valid;

__attribute__((constructor)) static void
s3lvol_capture_startup_cpus(void)
{
	CPU_ZERO(&g_s3lvol_startup_cpus);
	g_s3lvol_startup_cpus_valid =
		sched_getaffinity(0, sizeof(g_s3lvol_startup_cpus),
				  &g_s3lvol_startup_cpus) == 0 &&
		CPU_COUNT(&g_s3lvol_startup_cpus) > 0;
}

static void
s3lvol_format_cpuset(const cpu_set_t *set, char *buf, size_t len)
{
	size_t off = 0;
	int i, start = -1;

	buf[0] = '\0';
	for (i = 0; i <= CPU_SETSIZE && off < len; i++) {
		bool in = i < CPU_SETSIZE && CPU_ISSET(i, set);

		if (in && start < 0) {
			start = i;
		} else if (!in && start >= 0) {
			int n = start == i - 1 ?
				snprintf(buf + off, len - off, "%s%d", off ? "," : "", start) :
				snprintf(buf + off, len - off, "%s%d-%d", off ? "," : "",
					 start, i - 1);

			if (n < 0) {
				break;
			}
			off += (size_t)n;
			start = -1;
		}
	}
	if (buf[0] == '\0') {
		snprintf(buf, len, "none");
	}
}

/* Compute "the set of cores allowed for background threads" =
 * cores this process may use - reactor cores.
 *
 * "May use" is the affinity captured before EAL init. If that is unavailable,
 * fall back to every configured core, as before: the kernel intersects the
 * request with the cgroup cpuset, which is correct whenever the two overlap.
 */
static int
s3lvol_build_spawner_cpuset(cpu_set_t *out)
{
	cpu_set_t usable;
	char allowed_str[256], reactor_str[256];
	cpu_set_t reactors;
	uint32_t core;
	long nprocs;

	CPU_ZERO(out);
	CPU_ZERO(&usable);
	CPU_ZERO(&reactors);

	if (g_s3lvol_startup_cpus_valid) {
		memcpy(&usable, &g_s3lvol_startup_cpus, sizeof(usable));
	} else {
		nprocs = sysconf(_SC_NPROCESSORS_CONF);
		if (nprocs <= 0) {
			SPDK_ERRLOG("sysconf(_SC_NPROCESSORS_CONF) failed: %ld\n",
				    nprocs);
			return -EINVAL;
		}
		SPDK_WARNLOG("startup CPU affinity unknown; computing background "
			     "cores from all %ld configured CPUs\n", nprocs);
		for (long i = 0; i < nprocs && i < CPU_SETSIZE; i++) {
			CPU_SET((int)i, &usable);
		}
	}

	memcpy(out, &usable, sizeof(usable));

	/* carve out the reactor cores */
	SPDK_ENV_FOREACH_CORE(core) {
		if (core < CPU_SETSIZE) {
			CPU_CLR(core, out);
			CPU_SET(core, &reactors);
		}
	}

	if (CPU_COUNT(out) == 0) {
		/* Every CPU this process may use runs a reactor. Spread the
		 * background threads over all of them rather than leaving them
		 * on the main lcore: sharing is unavoidable, piling onto one
		 * reactor is not. */
		s3lvol_format_cpuset(&usable, allowed_str, sizeof(allowed_str));
		s3lvol_format_cpuset(&reactors, reactor_str, sizeof(reactor_str));
		SPDK_ERRLOG("no CPU left for S3 background threads: allowed CPUs "
			    "%s are all reactor cores (%s). CRT I/O threads will "
			    "compete with the reactors; give the target more CPUs "
			    "than its reactor mask\n", allowed_str, reactor_str);
		memcpy(out, &usable, sizeof(usable));
	}

	return 0;
}

static int
vbdev_s3lvol_init(void)
{
	cpu_set_t cpuset;
	char cpus_str[256];
	uint32_t core;
	int num_reactors = 0;
	int rc;

	if (g_s3lvol_initialized) {
		return 0;
	}

	rc = s3lvol_build_spawner_cpuset(&cpuset);
	if (rc != 0) {
		return rc;
	}

	SPDK_ENV_FOREACH_CORE(core) {
		num_reactors++;
	}

	s3lvol_format_cpuset(&cpuset, cpus_str, sizeof(cpus_str));
	SPDK_NOTICELOG("s3lvol: %d reactor cores, %d cores left for background "
		       "threads (%s)\n", num_reactors, CPU_COUNT(&cpuset), cpus_str);

	/* The spawner must start first: CRT init runs on it, or CRT's I/O
	 * threads inherit the reactor's single-core affinity. */
	rc = s3_spawner_start(&cpuset);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to start thread spawner: %d\n", rc);
		return rc;
	}

	rc = s3_crt_global_init(S3LVOL_CRT_THREADS);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to init AWS CRT: %d\n", rc);
		s3_spawner_stop();
		return rc;
	}

	g_s3lvol_initialized = true;
	return 0;
}

static void
vbdev_s3lvol_fini(void)
{
	if (!g_s3lvol_initialized) {
		return;
	}

	s3_crt_global_fini();
	s3_spawner_stop();
	g_s3lvol_initialized = false;
}

static int
vbdev_s3lvol_get_ctx_size(void)
{
	/* Keep in sync with struct s3lvol_bdev_io in vbdev_s3lvol_lvol.c: it
	 * holds a single spdk_blob_ext_io_opts. */
	return (int)sizeof(struct spdk_blob_ext_io_opts);
}

static struct spdk_bdev_module s3lvol_if = {
	.name            = "s3lvol",
	.module_init     = vbdev_s3lvol_init,
	.module_fini     = vbdev_s3lvol_fini,
	.get_ctx_size    = vbdev_s3lvol_get_ctx_size,
};

SPDK_BDEV_MODULE_REGISTER(s3lvol, &s3lvol_if)

struct spdk_bdev_module *
vbdev_s3lvol_get_module(void)
{
	return &s3lvol_if;
}

/* The log component. SPDK_INFOLOG(vbdev_s3lvol, ...) in vbdev_s3lvol_lvol.c
 * uses it; the registration lives here because this file is the module's
 * "primary" translation unit. */
SPDK_LOG_REGISTER_COMPONENT(vbdev_s3lvol)
