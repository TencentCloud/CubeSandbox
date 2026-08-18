/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_flusher -- drains the overlay into S3, one read-modify-write per chunk
 *
 *   === Why this fixes a correctness bug, not just a performance one ===
 *
 *   The direct-to-S3 write path ran one read-modify-write per *incoming write*.
 *   Eight concurrent 128 KiB writes into the same 1 MiB chunk therefore ran
 *   eight RMW cycles against a chunk that did not exist yet, each PUTting an
 *   object holding only its own eighth. Seven eighths of the data was lost.
 *
 *   Here the writes have already been acknowledged from the WAL and accumulated
 *   in the overlay, and the flusher runs *one* RMW per chunk with a per-chunk
 *   single-flight rule. The race is gone by construction: there is never more
 *   than one writer of a given S3 object.
 *
 *   === Division of labour ===
 *
 *   This file owns queueing, concurrency, single flight and WAL truncation. It
 *   does *not* know about S3: the actual upload is a callback supplied by
 *   s3_bs_dev, which owns the client, the key layout and the chunk map. Keeping
 *   the split means the scheduling logic can be unit tested with a fake
 *   uploader, with no credentials and no network.
 *
 *   === Threading ===
 *
 *   Owner thread only, like every other per-lvstore component. The poller is
 *   registered on the thread that calls s3_flusher_create().
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/thread.h"
#include "spdk/util.h"

#include "s3lvol/s3_flusher.h"
#include "s3lvol/s3_wal.h"

struct s3_flusher {
	struct s3_overlay          *overlay;

	/* Optional: without a WAL there is nothing to truncate, which is the
	 * mode the unit test uses. */
	struct s3_wal              *wal;

	s3_flusher_upload_fn        upload_fn;
	void                       *upload_ctx;

	uint32_t                    max_concurrent;
	uint32_t                    in_flight;

	struct spdk_poller *poller;

	/* Set by destroy/drain so no new upload is started. */
	bool                        stopping;

	s3_flusher_cb drain_cb;
	void *drain_arg;

	/* Tick at which a drain gives up on dirty data. */
	uint64_t drain_deadline;

	/* Highest seq already reported to the WAL, so truncation is only
	 * attempted when it would actually make progress. */
	uint64_t                    truncated_seq;

	/* One super update at a time: they are not ordered against each other
	 * and the later one would win anyway. */
	bool                        super_sync_active;
	bool                        super_sync_again;

	struct s3_flusher_stats     stats;
};

/* One chunk being uploaded. The view has to outlive the call, so it lives
 * here rather than on the stack of the caller. */
struct flush_req {
	struct s3_flusher              *flusher;
	struct s3_overlay_flush_view    view;
};

static void flusher_check_drain(struct s3_flusher *f);

/* ==========================================================================
 * WAL truncation
 * ========================================================================== */

static void
flusher_super_synced(void *cb_arg, int status)
{
	struct s3_flusher *f = cb_arg;

	f->super_sync_active = false;

	if (status != 0) {
		/* Not fatal: the positions stay in memory and the next attempt
		 * persists them. Recovery just rescans from an older point. */
		SPDK_WARNLOG("Failed to persist WAL positions after truncation: "
			     "%d\n", status);
	}

	if (f->super_sync_again && !f->stopping) {
		f->super_sync_again = false;
		f->super_sync_active = true;
		s3_wal_sync_super(f->wal, f->truncated_seq, flusher_super_synced, f);
	}

	/* A drain parked on super_sync_active can only resume here. The poller and
	 * every upload completion also re-check, but there is no reason to wait for
	 * the next one when the very state the drain was waiting on just changed. */
	flusher_check_drain(f);
}

/* Report progress to the WAL so it can release segments.
 *
 * The safe point is "one below the oldest seq the overlay still holds": every
 * entry older than that has been uploaded. When the overlay is empty the whole
 * log has been consumed, so everything issued so far is safe. */
static void
flusher_advance_wal(struct s3_flusher *f)
{
	uint64_t min_seq;
	uint64_t safe_seq;

	if (!f->wal) {
		return;
	}

	min_seq = s3_overlay_min_seq(f->overlay);
	if (min_seq == UINT64_MAX) {
		uint64_t next = s3_wal_get_next_seq(f->wal);

		safe_seq = next ? next - 1 : 0;
	} else {
		safe_seq = min_seq - 1;
	}

	if (safe_seq <= f->truncated_seq) {
		return;
	}
	f->truncated_seq = safe_seq;

	s3_wal_truncate_to_seq(f->wal, safe_seq);

	/* Persisting matters: the WAL may now reuse those segments, and replay
	 * starts from the position recorded in the super. If that position still
	 * pointed into a reused segment, recovery would scan bytes belonging to a
	 * later lap. */
	if (f->super_sync_active) {
		f->super_sync_again = true;
		return;
	}
	f->super_sync_active = true;
	s3_wal_sync_super(f->wal, safe_seq, flusher_super_synced, f);
}

/* ==========================================================================
 * Upload scheduling
 * ========================================================================== */

static void
flusher_upload_done(void *cb_arg, int status)
{
	struct flush_req *req = cb_arg;
	struct s3_flusher *f = req->flusher;
	uint64_t chunk_index = req->view.chunk_index;

	assert(f->in_flight > 0);
	f->in_flight--;

	if (status == 0) {
		f->stats.chunks_flushed++;
		f->stats.blocks_flushed += req->view.nblocks;
	} else {
		f->stats.failures++;
		SPDK_ERRLOG("Failed to flush chunk %" PRIu64 " to S3: %d "
			    "(kept dirty, will retry)\n", chunk_index, status);
	}

	/* Releases the claimed blocks on success, or re-dirties and re-queues
	 * them on failure. Either way it must happen before truncation, which
	 * reads the overlay's oldest seq. */
	s3_overlay_flush_end(f->overlay, chunk_index, status == 0);

	free(req);

	if (status == 0) {
		flusher_advance_wal(f);
	}

	s3_flusher_kick(f);
}

static void
flusher_start_one(struct s3_flusher *f, uint64_t chunk_index)
{
	struct flush_req *req;
	int rc;

	req = calloc(1, sizeof(*req));
	if (!req) {
		/* The chunk is off the queue, so put it back by leaving it dirty
		 * and letting the next write or the poller re-queue it. Nothing
		 * was claimed yet, so no data is at risk. */
		SPDK_ERRLOG("Out of memory starting a chunk flush\n");
		return;
	}
	req->flusher = f;

	rc = s3_overlay_flush_begin(f->overlay, chunk_index, &req->view);
	if (rc != 0) {
		/* -ENOENT: the chunk was unmapped or already flushed between
		 * being queued and now. -EBUSY cannot happen -- a chunk with a
		 * live flush is never on the queue. */
		if (rc != -ENOENT) {
			SPDK_ERRLOG("Unexpected flush_begin(%" PRIu64 ") result: "
				    "%d\n", chunk_index, rc);
		}
		free(req);
		return;
	}

	f->in_flight++;
	f->stats.rounds++;

	f->upload_fn(f->upload_ctx, &req->view, flusher_upload_done, req);
}

void
s3_flusher_kick(struct s3_flusher *f)
{
	bool force;

	if (!f) {
		return;
	}

	/* Two reasons to set the overlay's hold-back policy aside.
	 *
	 * A drain, because "everything acknowledged is in S3" is not something
	 * waiting can satisfy -- and the drain deadline (30s) is shorter than the
	 * default hold-back age (45s), so honouring the policy here would make
	 * every unload, checkpoint and export time out instead of flushing.
	 *
	 * A backpressured WAL, because the log is only truncated once the data has
	 * reached S3 (flusher_advance_wal below). Holding a chunk back therefore
	 * holds on to log space, and the WAL refusing writes is a worse outcome
	 * than an early upload. In the shipped configuration the overlay's own
	 * high water mark is reached long first -- 4 GiB of RAM against 32 GiB of
	 * log -- so this is the guard for a configuration where it is not. */
	force = (f->drain_cb != NULL) || s3_wal_is_backpressured(f->wal);

	while (!f->stopping && f->in_flight < f->max_concurrent) {
		uint64_t chunk_index;

		if (s3_overlay_next_dirty(f->overlay, force, &chunk_index) != 0) {
			/* -ENOENT nothing dirty, -EAGAIN dirty but held back. The
			 * poller comes round again either way. */
			break;
		}
		flusher_start_one(f, chunk_index);
	}

	flusher_check_drain(f);
}

static int
flusher_poll(void *arg)
{
	struct s3_flusher *f = arg;

	if (f->in_flight == 0 && !s3_overlay_has_dirty(f->overlay)) {
		flusher_check_drain(f);
		return SPDK_POLLER_IDLE;
	}

	/* A drain blocked on an unreachable S3 only makes progress through its
	 * deadline, so it has to be re-checked on every tick, not just when an
	 * upload completes. */
	flusher_check_drain(f);

	s3_flusher_kick(f);
	return SPDK_POLLER_BUSY;
}

/* ==========================================================================
 * Drain
 * ========================================================================== */

static void
flusher_check_drain(struct s3_flusher *f)
{
	s3_flusher_cb cb_fn;
	void *cb_arg;
	int status = 0;

	if (!f->drain_cb) {
		return;
	}

	/* Uploads already running are always waited for: their completions touch
	 * the flusher, so cutting them loose would be a use-after-free. */
	if (f->in_flight > 0) {
		return;
	}

	/* A super-sync completion also writes into the flusher, so it is waited for
	 * here for exactly the same reason. It matters for two distinct bugs: the
	 * completion would land on a freed flusher, and a later s3_wal_close()'s own
	 * super-sync would race this one for the shared super buffer. */
	if (f->super_sync_active) {
		return;
	}

	if (s3_overlay_has_dirty(f->overlay)) {
		if (spdk_get_ticks() < f->drain_deadline) {
			return;
		}
		/* Out of time. Stop starting rounds and report it -- the data is
		 * durable in the log, so the cost is a replay, not a loss. */
		f->stopping = true;
		status = -ETIMEDOUT;
		SPDK_WARNLOG("Flusher drain timed out with dirty chunks left; "
			     "they stay in the WAL for replay\n");
	}

	cb_fn  = f->drain_cb;
	cb_arg = f->drain_arg;
	f->drain_cb  = NULL;
	f->drain_arg = NULL;

	cb_fn(cb_arg, status);
}

void
s3_flusher_drain(struct s3_flusher *f, uint64_t timeout_us,
		 s3_flusher_cb cb_fn, void *cb_arg)
{
	if (!f) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}
	if (f->drain_cb) {
		/* One drain at a time. A second caller is told -EBUSY rather than
		 * queued: the drain already running is the one it wanted, so waiting
		 * for it and asking again gets the same answer as a waiter list
		 * would, without one. Export does exactly that (export_drained);
		 * shutdown has no competitor to wait for. */
		if (cb_fn) {
			cb_fn(cb_arg, -EBUSY);
		}
		return;
	}

	if (timeout_us == 0) {
		timeout_us = S3_FLUSHER_DRAIN_TIMEOUT_US;
	}
	f->drain_deadline = spdk_get_ticks() +
			    (timeout_us * spdk_get_ticks_hz()) / 1000000;

	f->drain_cb  = cb_fn;
	f->drain_arg = cb_arg;

	/* Push everything through rather than waiting for the poller tick. */
	s3_flusher_kick(f);
}

/* ==========================================================================
 * Create / destroy
 * ========================================================================== */

int
s3_flusher_create(struct s3_overlay *overlay, struct s3_wal *wal,
		  s3_flusher_upload_fn upload_fn, void *upload_ctx,
		  const struct s3_flusher_opts *opts, struct s3_flusher **out)
{
	struct s3_flusher *f;
	uint32_t period_us;

	if (!overlay || !upload_fn || !out) {
		return -EINVAL;
	}

	f = calloc(1, sizeof(*f));
	if (!f) {
		return -ENOMEM;
	}

	f->overlay    = overlay;
	f->wal        = wal;
	f->upload_fn  = upload_fn;
	f->upload_ctx = upload_ctx;

	f->max_concurrent = (opts && opts->max_concurrent) ?
			    opts->max_concurrent : S3_FLUSHER_DEFAULT_MAX_CONCURRENT;
	period_us = (opts && opts->poll_period_us) ?
		    opts->poll_period_us : S3_FLUSHER_DEFAULT_POLL_US;

	f->poller = SPDK_POLLER_REGISTER(flusher_poll, f, period_us);
	if (!f->poller) {
		free(f);
		return -ENOMEM;
	}

	SPDK_NOTICELOG("Flusher started: %u concurrent chunk uploads, %u us tick\n",
		       f->max_concurrent, period_us);

	*out = f;
	return 0;
}

void
s3_flusher_destroy(struct s3_flusher *f)
{
	if (!f) {
		return;
	}

	f->stopping = true;

	/* An upload completion would call back into a freed flusher. The caller
	 * has to drain first; same reasoning as s3_chunk_map_destroy(). */
	assert(f->in_flight == 0);
	if (f->in_flight != 0) {
		SPDK_ERRLOG("s3_flusher_destroy() called with %u uploads in "
			    "flight; leaking the flusher to avoid "
			    "use-after-free\n", f->in_flight);
		/* The flusher is leaked, so the poller is of no use any more: it
		 * would spin forever on a flusher that never drains. */
		spdk_poller_unregister(&f->poller);
		return;
	}

	/* A super-sync completion also writes into the flusher. Drain waits for it,
	 * so assert it never leaks through here -- but leak rather than free if it
	 * somehow does, mirroring the upload check above. */
	assert(!f->super_sync_active);
	if (f->super_sync_active) {
		SPDK_ERRLOG("s3_flusher_destroy() called with a super-sync in "
			    "flight; leaking the flusher to avoid "
			    "use-after-free\n");
		spdk_poller_unregister(&f->poller);
		return;
	}

	if (s3_overlay_has_dirty(f->overlay)) {
		SPDK_WARNLOG("Flusher destroyed with dirty chunks left; that data "
			     "is still in the WAL and will be replayed\n");
	}

	spdk_poller_unregister(&f->poller);
	free(f);
}

void
s3_flusher_get_stats(const struct s3_flusher *f, struct s3_flusher_stats *out)
{
	if (!f || !out) {
		return;
	}
	*out = f->stats;
	out->in_flight = f->in_flight;
}

SPDK_LOG_REGISTER_COMPONENT(s3_flusher)
