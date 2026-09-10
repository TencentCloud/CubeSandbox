/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3lvol_tgt -- the SPDK target carrying the s3lvol module
 *
 *   === Why a private app instead of spdk_tgt ===
 *
 *   An out-of-tree bdev module registers itself through the
 *   SPDK_BDEV_MODULE_REGISTER constructor. LD_PRELOADing into a stock spdk_tgt
 *   is not dependable -- the module's constructor has to run before the bdev
 *   subsystem initialises, and the linker may drop object files nobody
 *   references. The official out-of-tree example (test/external_code) also
 *   statically links the module into its own app rather than preloading.
 *
 *   This file therefore does exactly one thing: bring up the SPDK app
 *   framework. The s3lvol module comes in by linking libs3lvol_bdev.a
 *   (--whole-archive), and its constructors register automatically.
 *
 *   Once up it is a standard SPDK target, operable through rpc.py. The s3lvol
 *   methods are registered under the rcow_ prefix; use test/tools/s3lvol_rpc.py
 *   for those (plain spdk rpc.py does not know them):
 *
 *     # create an lvstore (credentials are read from the environment;
 *     # lvs_name is required, namespace / capacity_gib / wal_bdev optional)
 *     test/tools/s3lvol_rpc.py rcow_create_lvstore '{"lvs_name":"s3lvs"}'
 *     # create an lvol -> registers the "s3lvs/vol0" bdev automatically.
 *     # Takes no lvs_name: it operates on the one lvstore that exists.
 *     test/tools/s3lvol_rpc.py rcow_create_lvol '{"lvol_name":"vol0","size_gib":1}'
 *     # the built-in nvmf RPCs are reused unchanged
 *     rpc.py nvmf_create_transport -t TCP
 *     rpc.py nvmf_create_subsystem nqn.2026-08.io.spdk:s3 -a -s SPDK00000000000001
 *     rpc.py nvmf_subsystem_add_ns nqn.2026-08.io.spdk:s3 s3lvs/vol0
 *     rpc.py nvmf_subsystem_add_listener nqn.2026-08.io.spdk:s3 \
 *            -t tcp -a 127.0.0.1 -s 4420
 *     # mount over loopback
 *     nvme connect -t tcp -a 127.0.0.1 -s 4420 -n nqn.2026-08.io.spdk:s3
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/event.h"
#include "spdk/log.h"

#include "s3lvol/s3_spawner.h"

#include "s3lvol_tgt_cpumask.h"

static struct s3lvol_tgt_cpu_selection g_cpu_selection;

static void
s3lvol_tgt_restore_original_affinity(void)
{
	if (g_cpu_selection.original == NULL) {
		return;
	}
	if (sched_setaffinity(0, g_cpu_selection.cpuset_size,
			      g_cpu_selection.original) != 0) {
		SPDK_WARNLOG("could not restore startup affinity: %s\n",
			     strerror(errno));
	}
}

static void
s3lvol_tgt_started(void *arg1)
{
	SPDK_NOTICELOG("s3lvol target ready - use rpc.py to create lvstores\n");
	SPDK_NOTICELOG("credentials come from AWS_ACCESS_KEY_ID / "
		       "AWS_SECRET_ACCESS_KEY in this process's environment\n");
}

int
main(int argc, char **argv)
{
	struct spdk_app_opts opts = {};
	char lcore_map[64];
	int rc;

	spdk_app_opts_init(&opts, sizeof(opts));
	opts.name     = "s3lvol_tgt";
	opts.rpc_addr = "/var/run/s3lvol.sock";

	rc = spdk_app_parse_args(argc, argv, &opts, NULL, NULL, NULL, NULL);
	if (rc != SPDK_APP_PARSE_ARGS_SUCCESS) {
		return rc == SPDK_APP_PARSE_ARGS_HELP ? 0 : 1;
	}

	if (opts.reactor_mask == NULL && opts.lcore_map == NULL) {
		rc = s3lvol_tgt_lcore_map_from_affinity(2,
						    lcore_map, sizeof(lcore_map),
						    &g_cpu_selection);
		if (rc != 0) {
			if (rc == -ENODEV) {
				fprintf(stderr, "process affinity contains no CPU below "
					"CPU_SETSIZE for a DPDK reactor\n");
			} else {
				fprintf(stderr, "could not select reactor CPUs from process "
					"affinity: %s\n", strerror(-rc));
			}
			return 1;
		}
		opts.lcore_map = lcore_map;
		fprintf(stderr, "automatic reactor lcore map: %s\n", lcore_map);
	} else {
		size_t num_cpus;

		rc = s3lvol_tgt_capture_affinity(&g_cpu_selection);
		if (rc != 0) {
			fprintf(stderr, "could not read process affinity: %s\n",
				strerror(-rc));
			return 1;
		}
		num_cpus = g_cpu_selection.cpuset_size * CHAR_BIT;
		g_cpu_selection.reactors = CPU_ALLOC(num_cpus);
		g_cpu_selection.background = CPU_ALLOC(num_cpus);
		if (g_cpu_selection.reactors == NULL ||
		    g_cpu_selection.background == NULL) {
			s3lvol_tgt_cpu_selection_fini(&g_cpu_selection);
			return 1;
		}
		rc = s3lvol_tgt_background_from_options(
			g_cpu_selection.original, g_cpu_selection.cpuset_size, num_cpus,
			opts.reactor_mask, opts.lcore_map, g_cpu_selection.reactors,
			g_cpu_selection.background);
		if (rc != 0) {
			fprintf(stderr, "could not derive background CPUs from explicit "
				"SPDK placement: %s\n", strerror(-rc));
			s3lvol_tgt_cpu_selection_fini(&g_cpu_selection);
			return 1;
		}
	}

	if (s3lvol_tgt_cpu_selection_shares_reactors(&g_cpu_selection)) {
		fprintf(stderr, "warning: reactors occupy every available CPU; "
			"background threads will share them\n");
	}
	rc = s3_spawner_set_cpuset(g_cpu_selection.background,
				   g_cpu_selection.cpuset_size);
	if (rc != 0) {
		fprintf(stderr, "could not preserve background CPU affinity: %s\n",
			strerror(-rc));
		s3lvol_tgt_cpu_selection_fini(&g_cpu_selection);
		return 1;
	}

	rc = spdk_app_start(&opts, s3lvol_tgt_started, NULL);
	if (rc) {
		SPDK_ERRLOG("spdk_app_start failed: %d\n", rc);
	}

	s3lvol_tgt_restore_original_affinity();
	spdk_app_fini();
	s3lvol_tgt_cpu_selection_fini(&g_cpu_selection);
	return rc;
}
