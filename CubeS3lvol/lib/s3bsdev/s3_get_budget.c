/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * Process-wide admission control for whole-object GETs.
 *
 * Exact range GETs deliberately do not use this budget: they are the
 * forward-progress path when a caller cannot allocate whole-object staging.
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/thread.h"

#include "s3lvol/s3_client.h"

struct s3_get_token_waiter {
	s3_get_token_cb cb_fn;
	void           *cb_arg;
	struct spdk_thread *origin;
	TAILQ_ENTRY(s3_get_token_waiter) link;
};

TAILQ_HEAD(s3_get_token_waiters, s3_get_token_waiter);

static pthread_mutex_t g_token_lock = PTHREAD_MUTEX_INITIALIZER;
static struct s3_get_token_waiters g_demand_waiters =
	TAILQ_HEAD_INITIALIZER(g_demand_waiters);
static uint32_t g_tokens_in_use;

static void
s3_get_token_deliver(void *arg)
{
	struct s3_get_token_waiter *waiter = arg;
	s3_get_token_cb cb_fn = waiter->cb_fn;
	void *cb_arg = waiter->cb_arg;

	free(waiter);
	cb_fn(cb_arg);
}

int
s3_whole_get_token_acquire(bool low_priority, s3_get_token_cb cb_fn,
			   void *cb_arg)
{
	struct s3_get_token_waiter *waiter = NULL;
	bool immediate;

	if (!cb_fn) {
		return -EINVAL;
	}

	pthread_mutex_lock(&g_token_lock);
	immediate = g_tokens_in_use < S3_WHOLE_GET_MAX_INFLIGHT &&
		    (!low_priority ||
		     (g_tokens_in_use + 1 < S3_WHOLE_GET_MAX_INFLIGHT &&
		      TAILQ_EMPTY(&g_demand_waiters)));
	if (immediate) {
		g_tokens_in_use++;
		pthread_mutex_unlock(&g_token_lock);
		return 1;
	}
	/* Read-ahead never queues: dropping it leaves both the global wait list
	 * and the last token available to demand. */
	if (low_priority) {
		pthread_mutex_unlock(&g_token_lock);
		return -EAGAIN;
	}
	pthread_mutex_unlock(&g_token_lock);

	waiter = calloc(1, sizeof(*waiter));
	if (!waiter) {
		return -ENOMEM;
	}
	waiter->cb_fn = cb_fn;
	waiter->cb_arg = cb_arg;
	waiter->origin = spdk_get_thread();

	/* A completion may have freed a token while calloc ran.  Recheck so a
	 * demand does not sleep in the queue with capacity already idle. */
	pthread_mutex_lock(&g_token_lock);
	if (g_tokens_in_use < S3_WHOLE_GET_MAX_INFLIGHT) {
		g_tokens_in_use++;
		pthread_mutex_unlock(&g_token_lock);
		free(waiter);
		return 1;
	}
	TAILQ_INSERT_TAIL(&g_demand_waiters, waiter, link);
	pthread_mutex_unlock(&g_token_lock);
	return 0;
}

void
s3_whole_get_token_release(void)
{
	struct s3_get_token_waiter *waiter;
	int rc;

	for (;;) {
		pthread_mutex_lock(&g_token_lock);
		assert(g_tokens_in_use > 0);
		waiter = TAILQ_FIRST(&g_demand_waiters);
		if (waiter) {
			TAILQ_REMOVE(&g_demand_waiters, waiter, link);
		} else {
			g_tokens_in_use--;
		}
		pthread_mutex_unlock(&g_token_lock);

		if (!waiter) {
			return;
		}
		if (!waiter->origin || waiter->origin == spdk_get_thread()) {
			s3_get_token_deliver(waiter);
			return;
		}

		rc = spdk_thread_send_msg(waiter->origin, s3_get_token_deliver, waiter);
		if (rc == 0) {
			return;
		}
		/*
		 * The origin has stopped accepting work.  Do not invoke the grant
		 * callback here: dest fills mutate owner-thread-only state, and an
		 * inline grant would race that thread.  The queued I/O is already
		 * stranded with its origin; recycle the token to the next waiter.
		 */
		SPDK_ERRLOG("whole-GET token delivery failed: %s\n",
			    spdk_strerror(-rc));
		free(waiter);
	}
}
