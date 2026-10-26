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

#include "s3lvol/s3_build_info.h"

/* Answered before spdk_app_start so it works on a machine with no hugepages and
 * no privileges: the upgrade orchestrator runs it on the candidate binary, which
 * by definition is not the one running yet. */
static bool
s3lvol_tgt_wants_build_info(int argc, char **argv)
{
	int i;

	for (i = 1; i < argc; i++) {
		if (strcmp(argv[i], "--print-build-info") == 0) {
			return true;
		}
	}
	return false;
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
	int rc;

	if (s3lvol_tgt_wants_build_info(argc, argv)) {
		char buf[S3LVOL_BUILD_INFO_MAX];

		rc = s3lvol_build_info_json(buf, sizeof(buf));
		if (rc < 0) {
			/* Silence here would be read as "the binary cannot describe
			 * itself", the same thing the version gate reports for a
			 * renderer failure. */
			fprintf(stderr, "s3lvol_tgt: the build information does not "
				"fit in %zu bytes\n", sizeof(buf));
			return 1;
		}
		printf("%s\n", buf);
		return 0;
	}

	spdk_app_opts_init(&opts, sizeof(opts));
	opts.name     = "s3lvol_tgt";
	opts.rpc_addr = "/var/run/s3lvol.sock";

	rc = spdk_app_parse_args(argc, argv, &opts, NULL, NULL, NULL, NULL);
	if (rc != SPDK_APP_PARSE_ARGS_SUCCESS) {
		return rc == SPDK_APP_PARSE_ARGS_HELP ? 0 : 1;
	}

	rc = spdk_app_start(&opts, s3lvol_tgt_started, NULL);
	if (rc) {
		SPDK_ERRLOG("spdk_app_start failed: %d\n", rc);
	}

	spdk_app_fini();
	return rc;
}
