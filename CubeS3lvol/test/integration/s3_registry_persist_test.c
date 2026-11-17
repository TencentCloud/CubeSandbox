/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/* Deterministic whole-registry persistence state machine; no S3 or SPDK. */

#include <errno.h>
#include <stdio.h>
#include <string.h>

#include "registry_persist_ctl.h"

#define MAX_WAITERS 16

struct waiter {
	uint64_t seq;
	int      status;
	bool     done;
};

struct model {
	struct registry_persist_ctl ctl;
	struct waiter waiters[MAX_WAITERS];
	unsigned nwaiters;
	unsigned puts;
	unsigned active_puts;
	unsigned max_active_puts;
	uint64_t snapshots[MAX_WAITERS];
};

static int g_pass;
static int g_fail;

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

static unsigned
mutate(struct model *m)
{
	struct waiter *w = &m->waiters[m->nwaiters++];

	w->seq = registry_persist_mutate(&m->ctl);
	return m->nwaiters - 1;
}

static bool
submit(struct model *m)
{
	if (!registry_persist_start(&m->ctl)) {
		return false;
	}
	m->snapshots[m->puts++] = m->ctl.flight_seq;
	m->active_puts++;
	if (m->active_puts > m->max_active_puts) {
		m->max_active_puts = m->active_puts;
	}
	return true;
}

static void
complete(struct model *m, int status)
{
	uint64_t boundary;
	unsigned i;

	m->active_puts--;
	boundary = registry_persist_complete(&m->ctl);
	for (i = 0; i < m->nwaiters; i++) {
		if (!m->waiters[i].done && m->waiters[i].seq <= boundary) {
			m->waiters[i].done = true;
			m->waiters[i].status = status;
		}
	}
}

static void
test_reverse_completion_is_impossible(void)
{
	struct model m = {0};
	unsigned first = mutate(&m);
	unsigned second;

	printf("\n[1] old snapshots cannot complete after new snapshots\n");
	check_true("first mutation submits", submit(&m));
	second = mutate(&m);
	check_true("second mutation coalesces", !submit(&m));
	complete(&m, 0);
	check_true("first waiter completes at first boundary", m.waiters[first].done);
	check_true("second waiter remains pending", !m.waiters[second].done);
	check_true("follow-up submits", submit(&m));
	complete(&m, 0);
	check_true("second waiter completes after follow-up", m.waiters[second].done);
	check_true("snapshots are ordered", m.puts == 2 &&
		   m.snapshots[0] < m.snapshots[1]);
	check_true("only one PUT was ever active", m.max_active_puts == 1);
}

static void
test_continuous_mutations_coalesce(void)
{
	struct model m = {0};
	unsigned i;

	printf("\n[2] continuous mutations need one follow-up\n");
	mutate(&m);
	check_true("initial PUT submits", submit(&m));
	for (i = 0; i < 8; i++) {
		mutate(&m);
		check_true("mutation coalesces while busy", !submit(&m));
	}
	complete(&m, 0);
	check_true("one follow-up captures all mutations", submit(&m));
	check_true("follow-up boundary is latest mutation",
		   m.ctl.flight_seq == m.ctl.mutation_seq);
	complete(&m, 0);
	check_true("exactly two PUTs", m.puts == 2);
	for (i = 0; i < m.nwaiters; i++) {
		check_true("all waiters completed", m.waiters[i].done);
	}
}

static void
test_first_put_failure(void)
{
	struct model m = {0};
	unsigned first = mutate(&m);
	unsigned later;

	printf("\n[3] first PUT failure respects waiter boundary\n");
	submit(&m);
	later = mutate(&m);
	complete(&m, -EIO);
	check_true("covered waiter receives failure",
		   m.waiters[first].done && m.waiters[first].status == -EIO);
	check_true("later waiter is not failed by old snapshot",
		   !m.waiters[later].done);
	check_true("later mutation remains retryable", registry_persist_pending(&m.ctl));
	submit(&m);
	complete(&m, 0);
	check_true("later waiter succeeds on follow-up",
		   m.waiters[later].done && m.waiters[later].status == 0);
}

static void
test_followup_failure(void)
{
	struct model m = {0};
	unsigned first = mutate(&m);
	unsigned second;

	printf("\n[4] follow-up failure reaches only follow-up waiters\n");
	submit(&m);
	second = mutate(&m);
	complete(&m, 0);
	submit(&m);
	complete(&m, -ENOSPC);
	check_true("first waiter keeps success",
		   m.waiters[first].done && m.waiters[first].status == 0);
	check_true("follow-up waiter gets exact failure",
		   m.waiters[second].done && m.waiters[second].status == -ENOSPC);
	check_true("failed boundary is settled until another mutation",
		   !registry_persist_pending(&m.ctl));
}

static void
test_waiter_completion_boundary(void)
{
	struct model m = {0};
	unsigned a = mutate(&m);
	unsigned b = mutate(&m);
	unsigned c;

	printf("\n[5] waiters finish only when their snapshot lands\n");
	submit(&m);
	c = mutate(&m);
	complete(&m, 0);
	check_true("same-snapshot waiter A completes", m.waiters[a].done);
	check_true("same-snapshot waiter B completes", m.waiters[b].done);
	check_true("post-submit waiter C does not complete", !m.waiters[c].done);
	submit(&m);
	complete(&m, 0);
	check_true("waiter C completes with containing snapshot", m.waiters[c].done);
}

int
main(void)
{
	printf("=== imports/exports registry persistence ===\n");
	test_reverse_completion_is_impossible();
	test_continuous_mutations_coalesce();
	test_first_put_failure();
	test_followup_failure();
	test_waiter_completion_boundary();
	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
