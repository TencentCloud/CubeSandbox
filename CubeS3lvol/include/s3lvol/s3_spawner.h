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
 *   `lib/s3bsdev/` must be unit-testable outside the app framework. Normally
 *   `s3lvol_tgt` injects the physical-CPU complement before DPDK narrows the
 *   calling thread's affinity. The module layer supplies a fixed-size fallback.
 */

#ifndef S3LVOL_SPAWNER_H
#define S3LVOL_SPAWNER_H

#include "spdk/stdinc.h"

#include <sched.h>

/**
 * Start the spawner thread.
 *
 * \param cpuset  the set of cores background threads may run on. Typically
 *                "all cores minus the reactor cores of this process". The
 *                target normally preconfigures the physical CPU set; the module
 *                argument is a fallback.
 *                NULL uses a set previously supplied by
 *                s3_spawner_set_cpuset(); without one it means no affinity is
 *                set and the caller's affinity is inherited. A copy is taken
 *                internally; the caller does not need to keep it alive.
 *
 * \return 0 on success; negative errno on failure. Repeated calls return 0
 *         (idempotent).
 */
int s3_spawner_start(const cpu_set_t *cpuset);

/**
 * Preconfigure the affinity used by a later s3_spawner_start().
 *
 * This preserves a dynamically sized scheduler affinity before DPDK pins the
 * calling thread. Every later start uses this set instead of its cpuset
 * argument; stop does not discard it. A later call replaces the saved set
 * while the spawner is stopped.
 *
 * \return 0 on success; -EINVAL for NULL, zero-sized, or empty sets; -ENOMEM
 *         on allocation failure; -EBUSY once the spawner has started.
 */
int s3_spawner_set_cpuset(const cpu_set_t *cpuset, size_t cpuset_size);

/**
 * Whether a preconfigured set has been supplied.
 */
bool s3_spawner_has_cpuset(void);

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
