/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Last-mutation-wins for pending-delete registry PUTs
 *
 *   The production path only has one PUT in flight. These sequences are what
 *   used to complete in reverse order when each mutation submitted its own.
 */

#include <stdio.h>

#include "pending_persist_ctl.h"

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
	struct pending_persist_ctl c;
	int puts;

	printf("=== pending-delete registry persist coalescing ===\n");

	printf("\n[1] set then set: second waits, one resubmit\n");
	c = (struct pending_persist_ctl){0};
	puts = 0;
	check_true("first set submits", pending_persist_begin(&c));
	puts++;
	check_true("second set coalesces", !pending_persist_begin(&c));
	check_true("completion asks for a follow-up", pending_persist_end(&c));
	check_true("pending_persist begin submits PUT #2",
		   pending_persist_begin(&c));
	puts++;
	check_true("second completion is idle", !pending_persist_end(&c));
	check_true("two PUTs, latest state twice", puts == 2);

	printf("\n[2] set then clear: cancel is what lands\n");
	c = (struct pending_persist_ctl){0};
	check_true("set submits", pending_persist_begin(&c));
	check_true("clear coalesces", !pending_persist_begin(&c));
	check_true("completion asks for a follow-up", pending_persist_end(&c));
	check_true("begin submits the clear", pending_persist_begin(&c));
	check_true("then idle", !pending_persist_end(&c));

	printf("\n[3] clear then set: re-queue is what lands\n");
	c = (struct pending_persist_ctl){0};
	check_true("clear submits", pending_persist_begin(&c));
	check_true("set coalesces", !pending_persist_begin(&c));
	check_true("completion asks for a follow-up", pending_persist_end(&c));
	check_true("begin submits the set", pending_persist_begin(&c));
	check_true("then idle", !pending_persist_end(&c));

	printf("\n[4] a lone mutation is one PUT\n");
	c = (struct pending_persist_ctl){0};
	check_true("submits", pending_persist_begin(&c));
	check_true("completes without a follow-up", !pending_persist_end(&c));

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
