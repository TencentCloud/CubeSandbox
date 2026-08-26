/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Thread spawner implementation. The design rationale lives in
 *   include/s3lvol/s3_spawner.h.
 *
 *   Ported from spdk/module/bdev/erofs/bdev_erofs.c (verified in production).
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/queue_extras.h"

#include "s3lvol/s3_spawner.h"

enum spawner_req_type {
	SPAWNER_REQ_CREATE_THREAD,
	SPAWNER_REQ_RUN_TASK,
};

struct spawner_request {
	enum spawner_req_type   type;

	/* true means the request is heap-allocated and owned by the spawner: the
	 * submitter does not wait, and the spawner frees it when done. See
	 * s3_spawner_pthread_create_async(). */
	bool                    async;

	/* CREATE_THREAD */
	pthread_t              *thread_out;
	void                  *(*start_routine)(void *);

	/* async only: called on the spawner thread when pthread_create fails. */
	void                  (*err_cb)(void *ctx, int err);
	void                   *err_ctx;

	/* RUN_TASK */
	void                  *(*task_fn)(void *);
	void                   *task_ret;

	void                   *arg;
	int                     result;
	bool                    done;
	pthread_mutex_t         done_mutex;
	pthread_cond_t          done_cond;
	TAILQ_ENTRY(spawner_request) link;
};

static cpu_set_t       g_cpuset;
static bool            g_cpuset_valid = false;

static pthread_t       g_spawner_thread;
static bool            g_spawner_started = false;
static bool            g_spawner_stop = false;
static pthread_mutex_t g_spawner_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t  g_spawner_cond = PTHREAD_COND_INITIALIZER;
static TAILQ_HEAD(, spawner_request) g_spawner_queue =
	TAILQ_HEAD_INITIALIZER(g_spawner_queue);

/* On Linux, pthread_create children inherit the parent's /proc/self/comm.
 * Third-party libraries (CRT EventLoop and so on) may not rename themselves in
 * time; without handling, ps would show a pile of threads all named
 * "s3_spawner". So the name is temporarily changed around pthread_create. */
#define SPAWNER_THREAD_NAME   "s3_spawner"
#define SPAWNER_NEUTRAL_NAME  "thread"

static void *
spawner_thread_main(void *arg)
{
	(void)arg;

	pthread_setname_np(pthread_self(), SPAWNER_THREAD_NAME);

	/* Set affinity on itself so every child it creates inherits the same set. */
	if (g_cpuset_valid) {
		int rc = pthread_setaffinity_np(pthread_self(),
						sizeof(cpu_set_t), &g_cpuset);
		if (rc != 0) {
			/* Not fatal: falls back to inheriting the caller's affinity,
			 * degraded performance but still functional. */
			SPDK_WARNLOG("spawner: pthread_setaffinity_np failed: %s "
				     "(background threads may land on reactor cores)\n",
				     strerror(rc));
		}
	}

	for (;;) {
		struct spawner_request *req;

		pthread_mutex_lock(&g_spawner_mutex);
		while (!g_spawner_stop && TAILQ_EMPTY(&g_spawner_queue)) {
			pthread_cond_wait(&g_spawner_cond, &g_spawner_mutex);
		}
		if (g_spawner_stop && TAILQ_EMPTY(&g_spawner_queue)) {
			pthread_mutex_unlock(&g_spawner_mutex);
			break;
		}
		req = TAILQ_FIRST(&g_spawner_queue);
		TAILQ_REMOVE(&g_spawner_queue, req, link);
		pthread_mutex_unlock(&g_spawner_mutex);

		switch (req->type) {
		case SPAWNER_REQ_CREATE_THREAD:
			pthread_setname_np(pthread_self(), SPAWNER_NEUTRAL_NAME);
			if (req->async) {
				/* Nobody holds the pthread_t, so the thread must
				 * detach itself. */
				pthread_t tid;
				req->result = pthread_create(&tid, NULL,
							     req->start_routine,
							     req->arg);
			} else {
				req->result = pthread_create(req->thread_out, NULL,
							     req->start_routine,
							     req->arg);
			}
			pthread_setname_np(pthread_self(), SPAWNER_THREAD_NAME);
			break;

		case SPAWNER_REQ_RUN_TASK:
			/* The task may pthread_create directly; it needs the
			 * neutral name too. */
			pthread_setname_np(pthread_self(), SPAWNER_NEUTRAL_NAME);
			req->task_ret = req->task_fn(req->arg);
			pthread_setname_np(pthread_self(), SPAWNER_THREAD_NAME);
			req->result = 0;
			break;
		}

		if (req->async) {
			if (req->result != 0 && req->err_cb) {
				req->err_cb(req->err_ctx, req->result);
			}
			free(req);
		} else {
			pthread_mutex_lock(&req->done_mutex);
			req->done = true;
			pthread_cond_signal(&req->done_cond);
			pthread_mutex_unlock(&req->done_mutex);
		}
	}

	return NULL;
}

int
s3_spawner_start(const cpu_set_t *cpuset)
{
	int rc;

	pthread_mutex_lock(&g_spawner_mutex);
	if (g_spawner_started) {
		pthread_mutex_unlock(&g_spawner_mutex);
		return 0;
	}

	if (cpuset) {
		memcpy(&g_cpuset, cpuset, sizeof(g_cpuset));
		g_cpuset_valid = true;
		if (CPU_COUNT(&g_cpuset) == 0) {
			/* An empty set makes pthread_setaffinity_np fail, and
			 * semantically no core is usable. Treat it as "no
			 * affinity" and warn -- usually it means the reactors
			 * occupy every available core. */
			SPDK_WARNLOG("spawner: empty cpuset (reactors may occupy "
				     "every available core); not setting affinity\n");
			g_cpuset_valid = false;
		}
	} else {
		g_cpuset_valid = false;
	}

	g_spawner_stop = false;
	rc = pthread_create(&g_spawner_thread, NULL, spawner_thread_main, NULL);
	if (rc != 0) {
		pthread_mutex_unlock(&g_spawner_mutex);
		SPDK_ERRLOG("Failed to create spawner thread: %s\n", strerror(rc));
		return -rc;
	}
	/* Affinity is set by spawner_thread_main() itself at its entry. */
	g_spawner_started = true;
	pthread_mutex_unlock(&g_spawner_mutex);

	SPDK_NOTICELOG("Thread spawner started (%d cores allowed)\n",
		       g_cpuset_valid ? CPU_COUNT(&g_cpuset) : -1);
	return 0;
}

void
s3_spawner_stop(void)
{
	struct spawner_request *req, *tmp;
	TAILQ_HEAD(, spawner_request) drain_list =
		TAILQ_HEAD_INITIALIZER(drain_list);

	pthread_mutex_lock(&g_spawner_mutex);
	if (!g_spawner_started) {
		pthread_mutex_unlock(&g_spawner_mutex);
		return;
	}
	g_spawner_stop = true;
	pthread_cond_broadcast(&g_spawner_cond);
	pthread_mutex_unlock(&g_spawner_mutex);

	pthread_join(g_spawner_thread, NULL);

	/* Move the residual requests to a local list under the lock first, then
	 * wake waiters / call user callbacks after releasing it. err_cb may
	 * submit new requests or touch g_spawner_mutex indirectly; calling it
	 * under the lock would self-deadlock. */
	pthread_mutex_lock(&g_spawner_mutex);
	TAILQ_FOREACH_SAFE(req, &g_spawner_queue, link, tmp) {
		TAILQ_REMOVE(&g_spawner_queue, req, link);
		TAILQ_INSERT_TAIL(&drain_list, req, link);
	}
	g_spawner_started = false;
	g_spawner_stop = false;
	pthread_mutex_unlock(&g_spawner_mutex);

	TAILQ_FOREACH_SAFE(req, &drain_list, link, tmp) {
		TAILQ_REMOVE(&drain_list, req, link);
		req->result = -1;
		req->task_ret = NULL;
		if (req->async) {
			if (req->err_cb) {
				req->err_cb(req->err_ctx, ECANCELED);
			}
			free(req);
		} else {
			pthread_mutex_lock(&req->done_mutex);
			req->done = true;
			pthread_cond_signal(&req->done_cond);
			pthread_mutex_unlock(&req->done_mutex);
		}
	}

	SPDK_NOTICELOG("Thread spawner stopped\n");
}

bool
s3_spawner_is_started(void)
{
	bool started;

	pthread_mutex_lock(&g_spawner_mutex);
	started = g_spawner_started;
	pthread_mutex_unlock(&g_spawner_mutex);
	return started;
}

static void
spawner_submit_and_wait(struct spawner_request *req)
{
	pthread_mutex_init(&req->done_mutex, NULL);
	pthread_cond_init(&req->done_cond, NULL);
	req->done = false;

	pthread_mutex_lock(&g_spawner_mutex);
	if (!g_spawner_started || g_spawner_stop) {
		/* Already shutting down: fail fast, do not hang the caller. */
		pthread_mutex_unlock(&g_spawner_mutex);
		req->result = -1;
		req->task_ret = NULL;
		pthread_mutex_destroy(&req->done_mutex);
		pthread_cond_destroy(&req->done_cond);
		return;
	}
	TAILQ_INSERT_TAIL(&g_spawner_queue, req, link);
	pthread_cond_signal(&g_spawner_cond);
	pthread_mutex_unlock(&g_spawner_mutex);

	pthread_mutex_lock(&req->done_mutex);
	while (!req->done) {
		pthread_cond_wait(&req->done_cond, &req->done_mutex);
	}
	pthread_mutex_unlock(&req->done_mutex);

	pthread_mutex_destroy(&req->done_mutex);
	pthread_cond_destroy(&req->done_cond);
}

void *
s3_spawner_run_task(void *(*task_fn)(void *), void *arg)
{
	struct spawner_request req = {
		.type     = SPAWNER_REQ_RUN_TASK,
		.task_fn  = task_fn,
		.arg      = arg,
		.task_ret = NULL,
		.result   = -1,
	};

	if (!task_fn) {
		return NULL;
	}
	if (!s3_spawner_is_started()) {
		SPDK_ERRLOG("spawner not started; refusing to run task on the "
			    "calling thread (its children would inherit the "
			    "reactor's CPU affinity)\n");
		return NULL;
	}

	spawner_submit_and_wait(&req);
	return req.task_ret;
}

int
s3_spawner_pthread_create(pthread_t *thread, void *(*start_routine)(void *),
			  void *arg)
{
	struct spawner_request req = {
		.type          = SPAWNER_REQ_CREATE_THREAD,
		.thread_out    = thread,
		.start_routine = start_routine,
		.arg           = arg,
		.result        = -1,
	};

	if (!thread || !start_routine) {
		return -EINVAL;
	}
	if (!s3_spawner_is_started()) {
		SPDK_ERRLOG("spawner not started\n");
		return -EPERM;
	}

	spawner_submit_and_wait(&req);
	return req.result;
}

int
s3_spawner_pthread_create_async(void *(*start_routine)(void *), void *arg,
				void (*err_cb)(void *ctx, int err), void *err_ctx)
{
	struct spawner_request *req;

	if (!start_routine) {
		return -EINVAL;
	}
	if (!s3_spawner_is_started()) {
		SPDK_ERRLOG("spawner not started\n");
		return -EPERM;
	}

	req = calloc(1, sizeof(*req));
	if (!req) {
		return -ENOMEM;
	}
	req->type          = SPAWNER_REQ_CREATE_THREAD;
	req->async         = true;
	req->start_routine = start_routine;
	req->arg           = arg;
	req->err_cb        = err_cb;
	req->err_ctx       = err_ctx;
	req->result        = -1;

	pthread_mutex_lock(&g_spawner_mutex);
	if (!g_spawner_started || g_spawner_stop) {
		pthread_mutex_unlock(&g_spawner_mutex);
		free(req);
		return -EPERM;
	}
	TAILQ_INSERT_TAIL(&g_spawner_queue, req, link);
	pthread_cond_signal(&g_spawner_cond);
	pthread_mutex_unlock(&g_spawner_mutex);

	return 0;
}
