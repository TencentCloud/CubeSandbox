/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   spdk_thread stub -- used only by test/integration
 *
 *   === Why it is needed ===
 *
 *   lib/s3bsdev references three SPDK symbols: spdk_log, spdk_get_thread and
 *   spdk_thread_send_msg. spdk_log links on its own, but the other two live in
 *   libspdk_thread.a, and linking that library drags in spdk_env_dpdk and with
 *   it the whole DPDK EAL, which needs hugepages and root to run. That is a
 *   pointless burden for these purely userspace tests.
 *
 *   === Why stubbing is correct, not papering over a problem ===
 *
 *   These two symbols only do real work when the submitting thread is an SPDK
 *   thread:
 *
 *   - `spdk_get_thread()` returns NULL on non-SPDK threads anyway -- that is the
 *     semantics in a real SPDK process too, so the stub fabricates nothing.
 *   - The tests submit from plain pthreads, so req->owner_thread is always NULL,
 *     and `s3_request_complete()` takes the "run the callback in place" branch.
 *     `spdk_thread_send_msg()` is therefore never called at runtime; it is a
 *     symbol needed at link time only, which is why the stub aborts: if it is
 *     ever reached, that means the dispatch logic in s3_request_complete() is
 *     wrong, and crashing loudly beats silently dropping a callback.
 *
 *   In other words, these tests cover the "no owner thread" path. The bounce
 *   path (owner_thread != NULL) itself is not covered here; exercising it needs
 *   a real SPDK reactor.
 */

#include "spdk/thread.h"
#include "spdk/log.h"

struct spdk_thread *
spdk_get_thread(void)
{
	return NULL;
}

int
spdk_thread_send_msg(const struct spdk_thread *thread, spdk_msg_fn fn, void *ctx)
{
	(void)thread;
	(void)fn;
	(void)ctx;

	/* Unreachable: owner_thread is always NULL (see the header comment), so
	 * the caller never takes the branch that needs send_msg. If this is ever
	 * reached, the dispatch logic in s3_request_complete() is wrong -- crash
	 * rather than silently dropping the callback.
	 *
	 * Note the real spdk_thread_send_msg() also aborts on every failure path
	 * and never returns an error, so this behaviour matches the real thing. */
	SPDK_ERRLOG("spdk_thread_send_msg stub reached — the test submitted "
		    "from a real SPDK thread? That needs a real reactor, "
		    "not this stub.\n");
	abort();
}
