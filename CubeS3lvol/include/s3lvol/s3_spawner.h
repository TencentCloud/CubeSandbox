/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Thread spawner -- keeps threads created by third-party libraries from
 *   inheriting the reactor's CPU affinity
 *
 *   === Why it is needed ===
 *
 *   On Linux, pthread_create() children inherit the parent's CPU affinity.
 *   Whatever calls s3_crt_global_init() is necessarily an SPDK reactor thread,
 *   which DPDK pins to a single core in the coremask. The event_loop /
 *   host_resolver / background-log threads that CRT spawns internally then all
 *   pile up on that one core -- the same core a 100% busy-polling reactor
 *   already occupies.
 *
 *   === Why not spdk_call_unaffinitized() ===
 *
 *   That API temporarily widens the affinity of the *caller (the reactor
 *   itself)* to all cores and restores it after the callback. The problem is
 *   that it touches the reactor's own state: if the restore ever fails to take
 *   effect on some code path, the reactor permanently drifts off its pinned
 *   core, and such a scene is very hard to diagnose. The in-house
 *   `spdk/module/bdev/erofs/bdev_erofs.c` has hit this in practice.
 *
 *   === This approach ===
 *
 *   Keep one long-lived spawner thread whose affinity is the set of cores
 *   allowed for background threads. When a thread needs to be created, or an
 *   initialisation that itself creates threads needs to run, the request is
 *   handed to the spawner. Child threads naturally inherit the spawner's wide
 *   affinity. **The reactor never touches its own affinity.**
 *
 *   Ported from the spawner in `spdk/module/bdev/erofs/bdev_erofs.c`
 *   (verified in production).
 *
 *   === Layering constraint ===
 *
 *   The cpuset is **injected by the caller**; this file never calls
 *   `spdk_app_get_core_mask()` -- that is a spdk_event-layer API, and
 *   `lib/s3bsdev/` must be unit-testable outside the app framework. The logic
 *   that computes the complement of the reactor coremask lives in
 *   `module/bdev/s3lvol/`.
 */

#ifndef S3LVOL_SPAWNER_H
#define S3LVOL_SPAWNER_H

#include "spdk/stdinc.h"

#include <sched.h>

/**
 * Start the spawner thread.
 *
 * \param cpuset  the set of cores background threads may run on. Typically
 *                "all cores minus the reactor cores of this process", computed
 *                by the module layer with spdk_app_get_core_mask().
 *                NULL means no affinity is set (inherits the caller's -- for
 *                unit tests only; NULL on a production path is a no-op that
 *                fixes nothing). A copy is taken internally; the caller does
 *                not need to keep it alive.
 *
 * \return 0 on success; negative errno on failure. Repeated calls return 0
 *         (idempotent).
 */
int s3_spawner_start(const cpu_set_t *cpuset);

/**
 * Stop the spawner thread and wake/cancel every queued request.
 * Idempotent. Submitting after a stop fails fast.
 */
void s3_spawner_stop(void);

/**
 * Run task_fn(arg) on the spawner thread and wait for its return value.
 *
 * Meant for initialisation that **itself creates threads** (CRT global init,
 * gRPC client create, ...), so those internal threads do not inherit the
 * reactor's CPU affinity. A drop-in replacement for
 * `spdk_call_unaffinitized()`.
 *
 * \return task_fn's return value; NULL when the spawner is not started or has
 *         been stopped.
 *
 * \note the caller **blocks** until the task finishes. Do not run long tasks
 *       on a reactor through this.
 */
void *s3_spawner_run_task(void *(*task_fn)(void *), void *arg);

/**
 * Create a thread via the spawner, so the new thread inherits the wide
 * affinity. Behaviour matches `pthread_create()`: 0 on success.
 */
int s3_spawner_pthread_create(pthread_t *thread,
			      void *(*start_routine)(void *), void *arg);

/**
 * The fire-and-forget variant of `s3_spawner_pthread_create()`.
 *
 * Returns immediately after submitting; the reactor does not block on the
 * spawner round trip. The actual pthread_create happens on the spawner in the
 * background. **Nobody joins the new thread; it must
 * `pthread_detach(pthread_self())` itself.**
 *
 * \param err_cb  optional. Called on the spawner thread when pthread_create
 *                fails, with err a positive errno. Not called on success.
 *
 * \return 0 when submitted (note: does not mean the thread was created);
 *         negative errno when the submission itself failed, in which case
 *         err_cb is **not** called.
 */
int s3_spawner_pthread_create_async(void *(*start_routine)(void *), void *arg,
				    void (*err_cb)(void *ctx, int err),
				    void *err_ctx);

/**
 * Whether the spawner is started.
 */
bool s3_spawner_is_started(void);

#endif /* S3LVOL_SPAWNER_H */
