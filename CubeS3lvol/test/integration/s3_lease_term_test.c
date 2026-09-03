/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * A historical lease 404 must not confirm "gone" once the deadline passes.
 * Both orders: the reaper looking first, and a post-deadline HEAD completing
 * first. No S3, no SPDK.
 */

#include "export_lease_term.h"

#include <stdio.h>
#include <stdbool.h>

static int g_pass, g_fail;

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

int
main(void)
{
	const uint64_t expires = 100;

	printf("[1] a 404 from before the deadline is not terminal after it\n");
	check_true("reaper first: expired, last miss at 50",
		   !export_lease_miss_confirms_gone(true, 50, expires, 200));
	check_true("still inside the TTL",
		   !export_lease_miss_confirms_gone(true, 50, expires, 90));

	printf("[2] a miss submitted at or after the deadline is terminal\n");
	check_true("HEAD first: miss submitted at 150, now 200",
		   export_lease_miss_confirms_gone(true, 150, expires, 200));
	check_true("miss submitted on the deadline",
		   export_lease_miss_confirms_gone(true, 100, expires, 100));

	printf("[3] no miss, or no deadline, never confirms gone\n");
	check_true("lease present",
		   !export_lease_miss_confirms_gone(false, 150, expires, 200));
	check_true("no expires_at",
		   !export_lease_miss_confirms_gone(true, 150, 0, 200));
	check_true("absent_at unset",
		   !export_lease_miss_confirms_gone(true, 0, expires, 200));

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail ? 1 : 0;
}
