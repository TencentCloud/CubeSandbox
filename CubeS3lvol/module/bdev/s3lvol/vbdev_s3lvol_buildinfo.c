/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_buildinfo -- what this build is, as one JSON object
 *
 *   === Two callers, one renderer ===
 *
 *   The upgrade gate asks two questions: what the *running* target is
 *   (rcow_get_build_info) and what the *candidate* binary is
 *   (s3lvol_tgt --print-build-info). Both need the same answer, so both go
 *   through s3lvol_build_info_json(); a second copy of the key set is a second
 *   place for the two sides of the comparison to disagree.
 *
 *   === Why it is written by hand ===
 *
 *   Assembled with snprintf and no SPDK writer, so the candidate-side caller
 *   can link it without bringing up SPDK: --print-build-info has to work before
 *   the process exists as an SPDK app. The values are all compile-time
 *   constants, so there is nothing here that a writer would make safer.
 *
 *   === Truncation is a failure, not a shorter answer ===
 *
 *   snprintf reports the length the document *would* have, so a length that
 *   does not fit is detectable and must be reported as such. Returning a
 *   truncated buffer would hand the gate a smaller but well-formed object,
 *   which is worse than an error: the caller cannot tell it apart from a real
 *   answer whose fields happen to be absent.
 */

#include <errno.h>
#include <stdio.h>

#include "s3lvol/s3_build_info.h"
#include "s3lvol/s3_checkpoint.h"
#include "s3lvol/s3_export.h"
#include "s3lvol/s3_journal.h"
#include "s3lvol/s3_local_dev.h"
#include "s3lvol/s3_wal.h"

#include "vbdev_s3lvol.h"

int
s3lvol_build_info_json(char *buf, size_t len)
{
	int n;

	/* export_version_max is S3_EXPORT_VERSION: there is no _MAX macro, the
	 * current version *is* the max. */
	n = snprintf(buf, len,
		     "{\"s3lvol_version\":\"%s\",\"git_commit\":\"%s\","
		     "\"spdk_version\":\"%s\",\"rpc_api_version\":%d,"
		     "\"super_version\":%d,\"wal_version\":%d,"
		     "\"ckpt_version\":%d,\"journal_op_max\":%d,"
		     "\"export_version_min\":%d,\"export_version_max\":%d,"
		     "\"num_subsys\":%d,\"ns_per_subsys\":%d,"
		     "\"nqn_prefix\":\"%s\"}",
		     S3LVOL_VERSION, S3LVOL_GIT_COMMIT, S3LVOL_SPDK_VERSION,
		     S3LVOL_RPC_API_VERSION,
		     S3_SUPER_VERSION, S3_WAL_SUPER_VERSION, S3_CKPT_VERSION,
		     S3_JOURNAL_OP_MAX,
		     S3_EXPORT_VERSION_MIN, S3_EXPORT_VERSION,
		     RCOW_NUM_SUBSYS, RCOW_NS_PER_SUBSYS, RCOW_NQN_PREFIX);

	if (n < 0 || (size_t)n >= len) {
		return -ENOSPC;
	}
	return n;
}
