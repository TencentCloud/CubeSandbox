/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_flusher -- pushes overlay data to S3, one read-modify-write per chunk
 *
 *   Design notes are at the top of lib/s3bsdev/s3_flusher.c. The short version:
 *   user writes are acknowledged from the WAL and accumulate in the overlay, and
 *   this component turns them into S3 objects with *at most one writer per
 *   object at a time*. That per-chunk single-flight rule is what removes the
 *   concurrent read-modify-write race.
 *
 *   The upload itself is injected as a callback so this file does not depend on
 *   the S3 client or the chunk map, which keeps it unit testable without
 *   credentials.
 */

#ifndef S3LVOL_FLUSHER_H
#define S3LVOL_FLUSHER_H

#include "spdk/stdinc.h"

#include "s3lvol/s3_overlay.h"

/* Chunks uploaded at once. Each one is an S3 round trip measured in tens to
 * hundreds of milliseconds, so concurrency is the main lever on drain rate:
 * eight in flight at ~170 ms is only ~48 MiB/s of 1 MiB objects. Thirty-two
 * keeps the per-chunk single-flight rule and is still well inside the CRT
 * client pool. */
#define S3_FLUSHER_DEFAULT_MAX_CONCURRENT 32

/* Tick interval: roughly a millisecond. */
#define S3_FLUSHER_DEFAULT_POLL_US 1000

/* How long a drain waits for dirty data before giving up.
 *
 * A drain must not be able to hang shutdown. If S3 is unreachable the flusher
 * retries forever, which is right while running and wrong while exiting: the
 * data is already durable in the log, so giving up costs nothing but a replay on
 * the next attach. In-flight uploads are still waited for -- those are bounded by
 * the S3 client's own timeouts. */
#define S3_FLUSHER_DRAIN_TIMEOUT_US (30ULL * 1000 * 1000)

/* Default deadline for a suspend asked to be bounded. Used by the hot prepare,
 * which cannot afford to wait on one slow upload for the length of a stop. */
#define S3_FLUSHER_SUSPEND_TIMEOUT_US (60ULL * 1000 * 1000)

/* Ask s3_flusher_suspend() for no deadline at all.
 *
 * Different from passing 0, which selects the default above. The attach hold
 * wants "wait as long as it takes": its own resume fallback already bounds the
 * hold, and failing an attach over a slow upload would be worse than waiting. */
#define S3_FLUSHER_NO_SUSPEND_TIMEOUT UINT64_MAX

/* How long attach leaves its flusher hold armed before releasing it on its own.
 *
 * A grace, not a hold deadline: the hold itself is unbounded (the attach waits
 * for it), and this is the backstop for the case nothing else releases it --
 * a stalled blobstore metadata write whose only way out is the flusher. Kept
 * separate from S3_FLUSHER_DRAIN_TIMEOUT_US even though the value matches, and
 * from S3_FLUSHER_SUSPEND_TIMEOUT_US, because the three mean different things.
 */
#define S3_FLUSHER_ATTACH_GRACE_US (30ULL * 1000 * 1000)

struct s3_flusher;
struct s3_wal;

typedef void (*s3_flusher_cb)(void *cb_arg, int status);

/**
 * Completion of one chunk upload. Must be called exactly once per upload_fn
 * invocation, and may be called before upload_fn returns.
 */
typedef void (*s3_flusher_upload_cb)(void *cb_arg, int status);

/**
 * Upload one chunk.
 *
 * \c view describes what to write and stays valid until the completion fires.
 * The implementation is expected to:
 *
 *   1. read the current object back when \c view->covers_prefix is false,
 *   2. merge with s3_overlay_flush_merge(),
 *   3. PUT a new object and publish it in the chunk map,
 *   4. *re-check s3_overlay_chunk_epoch() against view->epoch before
 *      publishing* -- a mismatch means the chunk was unmapped meanwhile and
 *      publishing would resurrect it.
 *
 * A non-zero status leaves the data dirty and it is retried later, so a
 * transient S3 failure costs nothing but time.
 */
typedef void (*s3_flusher_upload_fn)(void *ctx,
				     const struct s3_overlay_flush_view *view,
				     s3_flusher_upload_cb cb_fn, void *cb_arg);

struct s3_flusher_opts {
	uint32_t max_concurrent;   /* 0 selects the default */
	uint32_t poll_period_us;   /* 0 selects the default */
};

struct s3_flusher_stats {
	uint64_t rounds;           /* flush rounds started */
	uint64_t chunks_flushed;
	uint64_t blocks_flushed;
	uint64_t failures;
	uint32_t in_flight;
};

/**
 * Start the flusher. The poller is registered on the calling thread, which must
 * be the lvstore's owner thread.
 *
 * \param wal may be NULL, in which case no truncation is attempted. Used by the
 *            unit test, which has an overlay but no log.
 */
int s3_flusher_create(struct s3_overlay *overlay, struct s3_wal *wal,
		      s3_flusher_upload_fn upload_fn, void *upload_ctx,
		      const struct s3_flusher_opts *opts,
		      struct s3_flusher **out);

/**
 * Stop and release. *The caller must drain first* -- this asserts that no
 * upload is in flight, because a completion would otherwise touch freed memory.
 */
void s3_flusher_destroy(struct s3_flusher *f);

/**
 * Start uploads for whatever is dirty, up to the concurrency limit. Cheap and
 * idempotent; call it whenever new data lands rather than waiting for the tick.
 */
void s3_flusher_kick(struct s3_flusher *f);

/**
 * Fire the callback once nothing is dirty and nothing is in flight, i.e. once
 * everything acknowledged so far is in S3 (INV2).
 *
 * \param timeout_us 0 selects S3_FLUSHER_DRAIN_TIMEOUT_US. Once it expires no
 *                   new round is started and the callback reports -ETIMEDOUT,
 *                   but uploads already running are still waited for -- their
 *                   completions would otherwise touch freed memory.
 *
 * One drain at a time; a second concurrent call gets -EBUSY.
 *
 * A drain supersedes a scheduling hold, whether established or still pending: a
 * pending suspend is cancelled and its callback reports -ECANCELED, and the
 * drain then provides the in_flight == 0 boundary that a caller destroying the
 * flusher needs.
 */
void s3_flusher_drain(struct s3_flusher *f, uint64_t timeout_us,
		      s3_flusher_cb cb_fn, void *cb_arg);

/**
 * Stop starting uploads and complete after current uploads and WAL super
 * updates finish. Dirty overlay data remains protected by the WAL. The pause is
 * reversible with s3_flusher_resume().
 *
 * \param timeout_us 0 selects S3_FLUSHER_SUSPEND_TIMEOUT_US;
 *                   S3_FLUSHER_NO_SUSPEND_TIMEOUT waits without one.
 *
 * \b Status: the callback reports 0 only when the hold is real -- uploads are
 * gated and stay gated until s3_flusher_resume(). Every other outcome leaves
 * them ungated, so the caller must not treat it as held:
 *   - -ETIMEDOUT  the deadline passed; the hold was abandoned.
 *   - -ECANCELED  a resume arrived while the hold was still pending, or a drain
 *                 took the flusher over. Either way it is released again.
 * In every case uploads already running are still waited for before the
 * callback fires -- their completions would otherwise touch freed memory.
 *
 * The callback must not re-enter this flusher (a drain or a destroy from inside
 * it is not refused while the suspend is unwinding).
 */
void s3_flusher_suspend(struct s3_flusher *f, uint64_t timeout_us,
			s3_flusher_cb cb_fn, void *cb_arg);

/** Re-enable uploads after a completed suspend. */
void s3_flusher_resume(struct s3_flusher *f);

void s3_flusher_get_stats(const struct s3_flusher *f, struct s3_flusher_stats *out);

#endif /* S3LVOL_FLUSHER_H */
