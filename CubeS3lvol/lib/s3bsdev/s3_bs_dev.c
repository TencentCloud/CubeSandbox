/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_bs_dev -- lets the blobstore treat S3 as a raw disk
 *
 *   Implements struct spdk_bs_dev (spdk/blob.h); the blobstore is completely
 *   unaware of S3. Every read/write it issues is a linear LBA, including its
 *   own super block and metadata pages -- all of it goes through the same
 *   chunk -> S3 path with no special handling (P1 reuse principle).
 *
 *   === Two write paths ===
 *
 *   Which one is active depends on whether a WAL has been attached with
 *   s3_bs_dev_attach_wal().
 *
 *   *With a WAL.* A write is appended to the log, recorded in the overlay and
 *   acknowledged from there. The flusher later turns the overlay into S3
 *   objects with one read-modify-write per chunk. Reads consult the
 *   overlay first, so an acknowledged write is visible immediately even though
 *   S3 still holds the previous version.
 *
 *   *Without a WAL (direct to S3).* Every write runs its own
 *   read-modify-write straight against S3. Kept because it needs no local bdev,
 *   which is what the S3-only integration test uses, but **it is not correct
 *   under concurrency**: N concurrent writes into one chunk each read the chunk
 *   before any of them wrote it, so N-1 of them are lost. That is the defect
 *   that made the WAL path necessary. Do not use this mode for anything that
 *   matters.
 *
 *   The chunk map is persisted separately, through the metadata journal
 *   (s3_chunk_map_set_journal); without a journal it degrades to memory-only and
 *   the lvstore cannot be re-attached after a restart.
 *
 *   === Three-level mapping ===
 *
 *       LBA --shift--> chunk_index --table--> uuid --concat--> S3 key
 *
 *   Only the middle level needs a table lookup. Chunks are fixed size, so the
 *   first level is plain division.
 *
 *   === Why RMW is needed ===
 *
 *   A chunk is the granularity of an S3 object (1 MiB by default), while the
 *   blobstore issues writes far smaller than that (a metadata page is a single
 *   4 KiB block). S3 objects cannot be partially updated, so a write that does
 *   not fill a chunk must read-modify-write: GET the whole chunk -> modify in
 *   memory -> PUT a new object.
 *
 *   For small metadata writes the read-modify-write turns a 4 KiB write into a
 *   1 MiB read plus a 1 MiB write. On the WAL path that cost is paid once per
 *   chunk per flush instead of once per write, which is what removes the
 *   amplification.
 *
 *   === Thread model ===
 *
 *   One bs_dev belongs to one lvstore, and all I/O runs on its owner thread.
 *   The s3_client callbacks already bounce back to the submitting thread (see
 *   s3_client.h), so the completion logic here needs no extra locking, and
 *   neither does the chunk map.
 */

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/string.h"
#include "spdk/thread.h"
#include "spdk/util.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_bs_dev.h"
#include "s3lvol/s3_cache.h"
#include "s3lvol/s3_checkpoint.h"
#include "s3lvol/s3_chunk_map.h"
#include "s3lvol/s3_client.h"
#include "s3lvol/s3_flusher.h"
#include "s3lvol/s3_overlay.h"
#include "s3lvol/s3_types.h"
#include "s3lvol/s3_wal.h"

/* How often a write parked by backpressure is retried. Short enough that a
 * transient stall is not noticeable, long enough not to spin. */
#define S3_RETRY_POLL_US 200

/* How often to look at whether a checkpoint is due. Coarse on purpose: the check
 * itself is two integer reads, but the work it starts uploads the whole chunk
 * map, so polling faster would only add jitter. */
#define S3_CKPT_POLL_US  (1000 * 1000)

/* Fraction of the journal region that must be in use before a checkpoint is
 * taken.
 *
 * *A percentage, not an absolute threshold.* The region is sized per disk
 * -- 256 MiB by default, 16 MiB in one of the tests -- and an absolute threshold
 * larger than the region means the ring fills before a checkpoint is ever
 * triggered, at which point appends fail with -ENOSPC and the lvstore goes
 * read-only. 50% leaves the whole second half as headroom for the upload, which
 * is a network round trip and can be slow. */
#define S3_CKPT_TRIGGER_PCT 50

/* How long a committed mapping may stay only in the journal, when nothing else
 * triggers a checkpoint.
 *
 * The usage trigger above bounds *space*, which is not the same as bounding
 * *recovery time*. A 256 MiB region holds on the order of two million records, so
 * a workload writing a few chunks a second takes days to reach 50% -- and a crash
 * anywhere in those days replays everything since the last checkpoint. This
 * trigger converts that into a fixed bound: replay never covers more than roughly
 * this much wall-clock time of writes.
 *
 * Cheap when idle, because "no mapping committed since the last checkpoint" is
 * checked separately and short-circuits: a quiet lvstore does not PUT a snapshot
 * every interval, it does nothing at all. */
#define S3_CKPT_DEFAULT_INTERVAL_SEC 60

/* ==========================================================================
 * Internal structures
 * ========================================================================== */

/* An operation parked until backpressure clears.
 *
 * blobstore has no retry path of its own: a non-zero status from a bs_dev write
 * is a hard I/O error that propagates all the way to the NVMe host. So -EAGAIN
 * from the WAL has to be absorbed here rather than reported upwards. */
struct s3_retry_item {
	void (*retry_fn)(void *arg);
	void *arg;

	STAILQ_ENTRY(s3_retry_item) link;
};

struct s3_ctx {
	/* Must be the first member: blobstore gets &ctx->bs_dev, and we recover
	 * ctx from pointer equality. */
	struct spdk_bs_dev       bs_dev;

	struct s3_client        *client;
	struct s3_chunk_map     *chunk_map;

	/* The S3 key prefix, shaped like "lvs-name" -- the keys built from it
	 * are "<prefix>/data/<uuid>". */
	char                    *prefix;

	uint32_t                 chunk_size;
	uint32_t                 chunk_shift;
	uint64_t                 capacity_bytes;

	/* The thread that owns this bs_dev; used to verify I/O stays on it. */
	struct spdk_thread      *owner_thread;

	/* Number of I/Os still in flight. Used only to assert at destroy time
	 * that the blobstore has drained -- destroy is a synchronous interface
	 * with no deferred free (see s3_bs_dev_destroy). */
	uint64_t                 inflight;

	/* Non-zero while a submit function (s3_bs_io_submit, s3_unmap_submit) is
	 * on the call stack. A sub-operation that finishes synchronously -- a read
	 * answered from the overlay, an unmapped chunk, a range past the object's
	 * end -- would otherwise hand its completion to blobstore from inside the
	 * submit path it was issued from. Blobstore's metadata recovery reads one
	 * md page from the previous page's completion callback
	 * (bs_load_replay_md_cpl), so such completions recurse md_len levels deep
	 * through the same stack and overflow it. While this is set, completions
	 * are bounced through the message queue instead, which turns the recursion
	 * into a loop over fresh stack frames.
	 *
	 * A counter rather than a flag, so nesting cannot clear it early, and it
	 * lives on ctx rather than on the per-I/O objects because those are freed
	 * by the very put() that has to happen before it can be decremented. ctx
	 * outlives all of them: destroy() refuses to free it while inflight > 0. */
	uint32_t                 submit_depth;

	/* ---- The WAL write path. All NULL in direct-to-S3 mode. ---- */

	/* Not owned here: the caller opens, replays and closes it. */
	struct s3_wal *wal;

	struct s3_overlay *overlay;
	struct s3_flusher *flusher;

	/* Local device, for the checkpoint path: it owns the super block that
	 * records checkpoint_gen / checkpoint_lsn, and its super block is also
	 * where the lvstore uuid stamped into a snapshot comes from. NULL in
	 * direct-to-S3 mode. */
	struct s3_local_dev *local_dev;

	/* Local read cache for chunk objects. NULL unless someone called
	 * s3_bs_dev_attach_cache(), and never required: every path that consults it
	 * treats "no cache" and "miss" identically, so nothing here has to be
	 * conditional beyond the null check itself. */
	struct s3_cache *cache;

	/* Registered only while teardown is waiting for a cache fill to land. See
	 * s3_bs_dev_teardown(). */
	struct spdk_poller *cache_quiesce_poller;

	/* Checkpoint state. Registered together with the journal.
	 *
	 * ckpt_in_flight enforces one at a time. That is not throttling: two
	 * concurrent checkpoints PUT the same key, so which one survives is
	 * undefined, while the super block would name the LSN of one of them --
	 * leaving the journal truncated past what the surviving snapshot covers. */
	struct spdk_poller *ckpt_poller;
	bool ckpt_in_flight;

	/* Set the moment destroy() is entered, and never cleared: teardown is one
	 * way. Two things consult it -- ckpt_start() refuses to begin anything new,
	 * and ckpt_finish() knows that a teardown is waiting for it. */
	bool destroying;

	uint64_t ckpt_gen;
	uint64_t ckpt_lsn_pending;
	uint64_t ckpt_lsn_done;
	uint64_t ckpt_done_count;
	uint64_t ckpt_failed_count;

	/* Time-based trigger. interval_sec is kept alongside the tsc form purely
	 * so stats can report what was configured without dividing by a clock
	 * rate that only this file knows.
	 *
	 * ckpt_last_tsc is updated on every *attempt* that got as far as starting,
	 * successful or not, so a persistently failing upload retries once per
	 * interval rather than once per poll. The usage trigger is unaffected: when
	 * the region is genuinely filling up, retrying every poll is the right
	 * behaviour and that condition stays true on its own. */
	uint32_t ckpt_interval_sec;
	uint64_t ckpt_interval_tsc;
	uint64_t ckpt_last_tsc;

	/* Set when a checkpoint was asked for explicitly rather than by the
	 * poller, so the requester can be told when it finished. */
	s3_bs_dev_cb ckpt_user_cb;
	void *ckpt_user_arg;

	/* Registered only while something is parked, so an idle lvstore does not
	 * carry a poller of its own. */
	struct spdk_poller *retry_poller;
	STAILQ_HEAD(, s3_retry_item) retry_q;
	uint32_t retry_count;

	/* Fired once the context is fully released, so the caller knows when it may
	 * close the journal and the local device. Needed because destroy() is
	 * asynchronous on the WAL path. */
	s3_bs_dev_cb destroy_cb;
	void *destroy_arg;

	/* Fired with the final chunk map, just before it is freed. See
	 * s3_bs_dev_set_reap_cb(). */
	s3_bs_dev_reap_cb reap_cb;
	void *reap_arg;

	/* Diagnostics */
	uint64_t                 rmw_count;      /* writes that needed read-modify-write */
	uint64_t                 zero_fill_count;/* zero-filled because the chunk was unallocated */

	/* WAL path counters */
	uint64_t wal_writes;   /* writes acknowledged from the log */
	uint64_t wal_retries;  /* writes parked by backpressure */
	uint64_t overlay_hits; /* reads served without touching S3 */
};

/* Context for one bs_dev I/O.
 *
 * A single blobstore I/O can span several chunks, one S3 operation each, so this
 * both splits and aggregates: blobstore is called back once num_pending reaches
 * zero.
 *
 * === Threading ===
 *
 * blobstore may call bs_dev entry points from *any* thread holding a channel --
 * that is the whole point of create_channel/destroy_channel in the vtable. nvmf
 * in particular runs I/O on a dedicated poll group thread
 * (spdk/module/event/subsystems/nvmf/nvmf_tgt.c:226 creates one per core), which
 * is never the thread that created the lvstore.
 *
 * But s3_ctx and the chunk map are single-owner-thread with no locking, so the
 * work is bounced onto owner_thread and the completion is bounced back. Both
 * directions are mandatory:
 *
 *   - forward, because ctx->inflight, the chunk map and the S3 client are all
 *     unsynchronized owner-thread state;
 *   - backward, because blobstore takes its request set from a per-channel free
 *     list and returns it with an unlocked TAILQ_INSERT_TAIL
 *     (spdk/lib/blob/request.c:66, per-channel req_mem in blobstore.c:3667), so
 *     completing on the wrong thread corrupts that list.
 *
 * Two message hops per I/O is negligible here: every I/O already costs an S3
 * round trip measured in milliseconds.
 */
struct s3_bs_io {
	struct s3_ctx                   *ctx;
	struct spdk_bs_dev_cb_args      *cb_args;

	/* Thread that submitted, and where the completion must be delivered. */
	struct spdk_thread              *submit_thread;

	/* Saved so the split loop can run later on the owner thread. */
	void                            *payload;
	uint64_t                         lba;
	uint32_t                         lba_count;

	/* Number of split sub-operations, plus the aggregation state. */
	uint32_t                         num_pending;
	int                              status;

	/* Whether the submit phase has finished. Stops the first sub-operation
	 * from completing the whole I/O while the split is still in progress. */
	bool                             submit_done;

	bool                             is_write;
};

/* One operation on a single chunk. */
struct s3_chunk_io {
	struct s3_bs_io         *bs_io;

	uint64_t                 chunk_index;

	/* Absolute LBA range of this slice. Always block aligned -- blobstore
	 * issues I/O in whole bs_dev blocks -- which is what lets the WAL and the
	 * overlay address it in blocks. */
	uint64_t lba;
	uint32_t nblocks;

	/* seq the WAL assigned to this slice, needed to order overlay updates. */
	uint64_t wal_seq;

	/* byte range within the chunk */
	uint32_t                 offset_in_chunk;
	uint32_t                 length;

	/* where this range sits in the blobstore's payload */
	void                    *user_buf;

	/* chunk-sized buffer for RMW / whole-chunk I/O. A whole-chunk write can
	 * use user_buf directly, in which case this is NULL. */
	void                    *chunk_buf;

	/* the new uuid on the write path */
	struct spdk_uuid         new_uuid;

	/* the old object replaced by an overwrite, deleted on completion */
	struct spdk_uuid         old_uuid;
	bool                     has_old;

	/* Which object this GET is reading, and whether a 404 has already sent it
	 * back for a second attempt.
	 *
	 * The read path needs these because a whole S3 round trip sits between
	 * looking the uuid up and the GET completing, and the flusher can overwrite
	 * the same chunk in that window: create-once PUTs a new uuid, inserts it
	 * into the chunk map, and then deletes the old object right away, without
	 * waiting (s3_flush_map_updated). The uuid in hand no longer names anything,
	 * so the GET comes back 404.
	 *
	 * That is not lost data, only a read that finished a step too late -- the new
	 * object is certainly there, because the PUT precedes the insert. So on a 404
	 * look the mapping up again: if the uuid moved, reread the new one. The
	 * second lookup is pure memory, far cheaper than handing an I/O error up.
	 *
	 * Retry once. A second 404 is not this race but a chunk map that really does
	 * name an absent object, and that has to be reported rather than retried
	 * forever. */
	struct spdk_uuid         read_uuid;
	bool                     reread;

	/* valid_bytes of the object named by read_uuid, i.e. how much of the chunk
	 * exists in S3. Recorded at submit time so the completion can tell whether
	 * the whole object ended up in the caller's buffer and is therefore free to
	 * hand to the cache. */
	uint32_t                 read_valid_bytes;

	/* A cache read was already attempted for this slice. Stops a device error
	 * from bouncing between the cache and the fallback: on failure the entry is
	 * dropped, but a slot pinned by another reader survives, and without this
	 * the retry could hit it again. */
	bool                     cache_tried;
};

#define S3_KEY_MAX 512

/* ==========================================================================
 * Helpers
 * ========================================================================== */

static void s3_chunk_io_finish(struct s3_chunk_io *cio, int status);

static inline uint32_t
s3_blocks_per_chunk(const struct s3_ctx *ctx)
{
	return 1u << (ctx->chunk_shift - S3LVOL_BLOCK_SHIFT);
}

static void
s3_data_key(const struct s3_ctx *ctx, const struct spdk_uuid *uuid,
	    char *out, size_t out_len)
{
	/* Format lives in s3_chunk_map.c, because export and GC name the same
	 * objects and a second copy of it here would be free to drift. */
	s3_chunk_data_key(ctx->prefix, uuid, out, out_len);
}

/* ==========================================================================
 * Backpressure parking
 * ========================================================================== */

static int s3_retry_poll(void *arg);

/* Park an operation and arrange for it to be retried.
 *
 * The poller is registered lazily and torn down again once the queue drains, so
 * the common case costs nothing. */
static int
s3_retry_queue(struct s3_ctx *ctx, void (*retry_fn)(void *), void *arg)
{
	struct s3_retry_item *item;

	item = calloc(1, sizeof(*item));
	if (!item) {
		return -ENOMEM;
	}
	item->retry_fn = retry_fn;
	item->arg = arg;

	STAILQ_INSERT_TAIL(&ctx->retry_q, item, link);
	ctx->retry_count++;

	if (!ctx->retry_poller) {
		ctx->retry_poller = SPDK_POLLER_REGISTER(s3_retry_poll, ctx,
							S3_RETRY_POLL_US);
	}

	return 0;
}

static int
s3_retry_poll(void *arg)
{
	struct s3_ctx *ctx = arg;
	struct s3_retry_item *item;

	item = STAILQ_FIRST(&ctx->retry_q);
	if (!item) {
		/* Unregistering the poller currently running is supported and is
		 * the point: an lvstore that is not backpressured should not be
		 * polled at all. */
		spdk_poller_unregister(&ctx->retry_poller);
		return SPDK_POLLER_IDLE;
	}

	/* One per tick. Retrying the whole queue at once would just re-park most
	 * of it, and this way the ordering between parked writes is preserved. */
	STAILQ_REMOVE_HEAD(&ctx->retry_q, link);
	assert(ctx->retry_count > 0);
	ctx->retry_count--;

	item->retry_fn(item->arg);
	free(item);

	return SPDK_POLLER_BUSY;
}

/* Overlay whatever has not reached S3 yet on top of the base data just read.
 *
 * Every read completion path has to call this, including the zero-fill ones: an
 * unallocated chunk still reads as zero *plus* anything acknowledged from the
 * WAL since. */
static void
s3_read_apply_overlay(struct s3_chunk_io *cio)
{
	struct s3_ctx *ctx = cio->bs_io->ctx;

	if (ctx->overlay) {
		s3_overlay_apply(ctx->overlay, cio->lba, cio->nblocks,
				 cio->user_buf);
	}
}

/* Hand the completion to blobstore and release the context.
 *
 * Must run on the thread that submitted -- see the threading note on
 * struct s3_bs_io. bs_io stays alive until here precisely so that the bounce
 * needs no extra allocation. */
static void
s3_bs_io_deliver(void *arg)
{
	struct s3_bs_io *bs_io = arg;
	struct spdk_bs_dev_cb_args *cb_args = bs_io->cb_args;
	int status = bs_io->status;

	free(bs_io);

	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, status);
}

/* Finish one bs_dev I/O. Called on the owner thread. */
static void
s3_bs_io_complete(struct s3_bs_io *bs_io)
{
	struct s3_ctx *ctx = bs_io->ctx;
	int rc;

	/* inflight is owner-thread state, so drop it before any bounce. */
	assert(ctx->inflight > 0);
	ctx->inflight--;

	if (bs_io->submit_thread == spdk_get_thread() && ctx->submit_depth == 0) {
		s3_bs_io_deliver(bs_io);
		return;
	}

	/* Either a completion from another thread, or one reached from inside a
	 * submit function -- every sub-op finished synchronously, so the last put
	 * completed the I/O before submission returned. In the latter case,
	 * delivering inline would call blobstore's completion from its own submit
	 * call path; blobstore's metadata recovery chains the next md page read
	 * from that completion (bs_load_replay_md_cpl), so a run of
	 * synchronously-completing reads recurses until the stack overflows.
	 * Bouncing through the queue makes the chain a loop over fresh stack
	 * frames.
	 *
	 * The cost is one message hop on every fully-synchronous I/O, which
	 * includes reads answered entirely from the overlay -- the hot path for
	 * just-written data. Paid unconditionally on purpose: bouncing only past
	 * some depth would keep the recursion and merely move the overflow to
	 * whatever lvstore is large enough to reach it. */
	rc = spdk_thread_send_msg(bs_io->submit_thread, s3_bs_io_deliver, bs_io);
	if (rc != 0) {
		/* Practically unreachable: SPDK aborts internally when it cannot
		 * allocate a message. Deliberately not falling back to an inline
		 * call, which would corrupt blobstore's per-channel request list
		 * and produce a far harder failure than a leak.
		 */
		SPDK_ERRLOG("Failed to bounce completion to the submitting thread: "
			    "%d (leaking the I/O context)\n", rc);
	}
}

/* Decrement the sub-op count; when it hits zero and the submit phase has
 * ended, complete the whole I/O. */
static void
s3_bs_io_put(struct s3_bs_io *bs_io)
{
	assert(bs_io->num_pending > 0);
	bs_io->num_pending--;

	if (bs_io->num_pending == 0 && bs_io->submit_done) {
		s3_bs_io_complete(bs_io);
	}
}

/* ==========================================================================
 * Read path
 * ========================================================================== */

static int s3_chunk_read_submit(struct s3_chunk_io *cio);
static int s3_chunk_write_submit(struct s3_chunk_io *cio);

/* Is a 404 worth one more read?
 *
 * The only legitimate 404 is a read that finished late: the chunk was overwritten
 * by a flush while the GET was in flight and the old object has been deleted. The
 * test for that is that the chunk map now names a different uuid than the one that
 * was read -- if it moved, the chunk really was overwritten, and the new object is
 * there, because the PUT precedes the insert (see s3_flush_put).
 *
 * An unchanged uuid is not this race: the chunk map names an object that does not
 * exist, which is a genuine integrity problem and has to surface. */
static bool
s3_chunk_read_should_reread(struct s3_chunk_io *cio)
{
	struct s3_ctx *ctx = cio->bs_io->ctx;
	char read_str[SPDK_UUID_STRING_LEN];
	char now_str[SPDK_UUID_STRING_LEN];
	struct spdk_uuid now;
	int rc;

	spdk_uuid_fmt_lower(read_str, sizeof(read_str), &cio->read_uuid);

	if (cio->reread) {
		SPDK_ERRLOG("Chunk %" PRIu64 " is still missing after a reread (%s)\n",
			    cio->chunk_index, read_str);
		return false;
	}

	rc = s3_chunk_map_lookup(ctx->chunk_map, cio->chunk_index, &now, NULL);
	if (rc != 0) {
		SPDK_ERRLOG("Chunk %" PRIu64 " read %s and got 404, and the map now has "
			    "no entry for it (%s)\n", cio->chunk_index, read_str,
			    spdk_strerror(-rc));
		return false;
	}

	spdk_uuid_fmt_lower(now_str, sizeof(now_str), &now);

	if (spdk_uuid_compare(&now, &cio->read_uuid) == 0) {
		/* Not the read-versus-flush race: the mapping still names the object
		 * that is not there. Something published an entry for an object that
		 * never made it to S3, or deleted one that was still mapped. */
		SPDK_ERRLOG("Chunk %" PRIu64 " maps to %s, which is not in S3, and the "
			    "mapping did not change under us. The chunk map entry is "
			    "wrong.\n", cio->chunk_index, now_str);
		return false;
	}

	SPDK_NOTICELOG("Chunk %" PRIu64 " was overwritten while being read (%s -> "
		       "%s); rereading\n", cio->chunk_index, read_str, now_str);
	return true;
}

static void
s3_chunk_read_done(void *cb_arg, uint64_t bytes_read, int status)
{
	struct s3_chunk_io *cio = cb_arg;
	struct s3_ctx *ctx = cio->bs_io->ctx;

	if (status == -ENOENT && s3_chunk_read_should_reread(cio)) {
		int rc;

		cio->reread = true;
		rc = s3_chunk_read_submit(cio);
		if (rc == 0) {
			return;
		}
		/* The reread would not even submit; report the original failure. */
		status = rc;
	}

	if (status == 0 && cio->chunk_buf) {
		/* A read through chunk_buf (partial read): copy the wanted range
		 * back into the user buffer. */
		uint64_t avail = bytes_read > cio->offset_in_chunk
				 ? bytes_read - cio->offset_in_chunk : 0;
		uint32_t copy_len = (uint32_t)spdk_min(avail, cio->length);

		if (copy_len > 0) {
			memcpy(cio->user_buf,
			       (uint8_t *)cio->chunk_buf + cio->offset_in_chunk,
			       copy_len);
		}
		/* The object is shorter than the request (the chunk was only
		 * partially written) -- zero the tail. This is normal, not an
		 * error: the blobstore expects unwritten regions to read as
		 * zeroes. */
		if (copy_len < cio->length) {
			memset((uint8_t *)cio->user_buf + copy_len, 0,
			       cio->length - copy_len);
		}
	} else if (status == 0 && bytes_read < cio->length) {
		/* The direct-to-user_buf path zero-fills too. */
		memset((uint8_t *)cio->user_buf + bytes_read, 0,
		       cio->length - bytes_read);
	}

	if (status == 0) {
		/* Before the overlay is applied, and that ordering is the whole
		 * reason this is done here rather than after.
		 *
		 * At this point user_buf holds exactly the bytes of the object
		 * named by read_uuid. One line further down the overlay merges
		 * acknowledged-but-unflushed writes on top, and caching *that*
		 * under read_uuid would be a lie: the entry would be tagged with an
		 * immutable object's uuid while holding content that object does
		 * not have. A later reader resolving the same uuid would get data
		 * from the future. Populating first keeps the tag honest and costs
		 * nothing -- no copy, since the cache takes its own.
		 *
		 * Whatever this read fetched, however little, and wherever in the
		 * chunk it sits: the cache tracks residency per block. Requiring a
		 * whole object here is what used to keep a mounted filesystem's
		 * cache empty forever -- ext4 reads ahead 128 KiB against a 1 MiB
		 * chunk, so the condition never held and reads of the same blocks
		 * went to S3 every single time.
		 *
		 * The chunk_buf path is excluded, and not because it could not be
		 * cached: it is the read-modify-write base, whose bytes the same
		 * flush is about to PUT under a new uuid and cache from there. */
		if (ctx->cache && !cio->chunk_buf && cio->read_valid_bytes > 0 &&
		    bytes_read > 0) {
			s3_cache_populate(ctx->cache, cio->chunk_index,
					  &cio->read_uuid, cio->offset_in_chunk,
					  cio->user_buf,
					  (uint32_t)spdk_min(bytes_read,
							     cio->length),
					  cio->read_valid_bytes);
		}

		s3_read_apply_overlay(cio);
	}

	s3_chunk_io_finish(cio, status);
}

/* A read served from the local cache instead of S3.
 *
 * The cache has already zero filled anything past the object's end, so the only
 * thing left is what every read completion has to do: merge the overlay. */
static void
s3_chunk_cache_read_done(void *cb_arg, int status)
{
	struct s3_chunk_io *cio = cb_arg;

	if (status != 0) {
		/* The data is still in S3, so this is not the user's problem. The
		 * entry has been dropped by the cache, so the retry will miss and
		 * go to S3 -- and cache_tried makes sure it cannot come back here
		 * even if the slot survived because another reader had it pinned. */
		int rc = s3_chunk_read_submit(cio);

		if (rc == 0) {
			return;
		}
		SPDK_ERRLOG("Chunk %" PRIu64 ": the cache read failed (%d) and the "
			    "fallback to S3 would not submit (%d)\n",
			    cio->chunk_index, status, rc);
		s3_chunk_io_finish(cio, rc);
		return;
	}

	s3_read_apply_overlay(cio);
	s3_chunk_io_finish(cio, 0);
}

static int
s3_chunk_read_submit(struct s3_chunk_io *cio)
{
	struct s3_ctx *ctx = cio->bs_io->ctx;
	struct spdk_uuid uuid;
	uint32_t valid_bytes = 0;
	char key[S3_KEY_MAX];
	int rc;

	/* Fully covered by data that has not been flushed yet: S3 does not have
	 * to be touched at all, and must not be trusted here anyway -- it still
	 * holds the previous version of the chunk. */
	if (ctx->overlay &&
	    s3_overlay_covers(ctx->overlay, cio->lba, cio->nblocks)) {
		s3_overlay_apply(ctx->overlay, cio->lba, cio->nblocks,
				 cio->user_buf);
		ctx->overlay_hits++;
		s3_chunk_io_finish(cio, 0);
		return 0;
	}

	rc = s3_chunk_map_lookup(ctx->chunk_map, cio->chunk_index, &uuid,
				 &valid_bytes);
	if (rc == -ENOENT) {
		/* Never written: all zeroes. The blobstore depends on this
		 * semantics -- a freshly created blob must read as zero, or
		 * metadata parsing would read garbage. */
		memset(cio->user_buf, 0, cio->length);
		ctx->zero_fill_count++;
		s3_read_apply_overlay(cio);
		s3_chunk_io_finish(cio, 0);
		return 0;
	}
	if (rc != 0) {
		return rc;
	}

	/* The request lies entirely past the written range -- all zeroes again,
	 * no GET needed. */
	if (cio->offset_in_chunk >= valid_bytes) {
		memset(cio->user_buf, 0, cio->length);
		ctx->zero_fill_count++;
		s3_read_apply_overlay(cio);
		s3_chunk_io_finish(cio, 0);
		return 0;
	}

	s3_data_key(ctx, &uuid, key, sizeof(key));

	/* Note which object is being read, so a 404 can be told apart from a chunk
	 * the flusher overwrote in the meantime. */
	spdk_uuid_copy(&cio->read_uuid, &uuid);
	cio->read_valid_bytes = valid_bytes;

	/* Local copy of this exact object version? Then S3 does not have to be
	 * touched.
	 *
	 * Placed after the overlay check rather than before it because the overlay
	 * is the only one of the two that can hold data S3 has not got yet -- the
	 * cache holds copies of objects, so it can never be *newer*. Which is also
	 * why it is safe here without any ordering against the flusher: the entry
	 * is tagged with the uuid this read resolved from the chunk map, and a uuid
	 * names immutable bytes.
	 *
	 * The reread path skips the cache: a reread happens because the object went
	 * away underneath, and the point of it is to consult S3 again. */
	if (ctx->cache && !cio->cache_tried && !cio->reread) {
		cio->cache_tried = true;

		rc = s3_cache_read(ctx->cache, cio->chunk_index, &uuid,
				   cio->offset_in_chunk, cio->length,
				   cio->user_buf, s3_chunk_cache_read_done, cio);
		if (rc == 0) {
			return 0;
		}
		/* -ENOENT is a miss, which is not an error and not worth reporting:
		 * fall through to the GET. s3_cache_read promises nothing else. */
	}

	/* Read straight into user_buf: the range GET returns exactly the wanted
	 * range, no staging needed. It may read short only when zero-filling past
	 * the object's end is required, which s3_chunk_read_done handles. */
	uint32_t want = (uint32_t)spdk_min(cio->length,
					   valid_bytes - cio->offset_in_chunk);

	rc = s3_get_range(ctx->client, key, cio->offset_in_chunk, want,
			  cio->user_buf, s3_chunk_read_done, cio);
	if (rc != 0) {
		return rc;
	}
	/* the want < length remainder is zero-filled in the completion */

	return 0;
}

/* ==========================================================================
 * Write path with a WAL: append to the log and acknowledge from there
 * ========================================================================== */

static void s3_chunk_wal_submit(void *arg);

/* The log entry is durable. Make it visible to reads, then acknowledge.
 *
 * The order matters: acknowledging first would leave a window in which a read of
 * an already acknowledged write still returns the old S3 object. */
static void
s3_chunk_wal_done(void *cb_arg, int status)
{
	struct s3_chunk_io *cio = cb_arg;
	struct s3_ctx *ctx = cio->bs_io->ctx;
	int rc;

	if (status == -EAGAIN) {
		/* Backpressure is a retry signal, not a failure (W5). */
		ctx->wal_retries++;
		s3_flusher_kick(ctx->flusher);
		if (s3_retry_queue(ctx, s3_chunk_wal_submit, cio) == 0) {
			return;
		}
		s3_chunk_io_finish(cio, -ENOMEM);
		return;
	}
	if (status != 0) {
		s3_chunk_io_finish(cio, status);
		return;
	}

	rc = s3_overlay_write(ctx->overlay, cio->lba, cio->nblocks,
			      cio->user_buf, cio->wal_seq);
	if (rc != 0) {
		/* The entry is already durable, so replay will bring this write
		 * back even though blobstore is about to be told it failed.
		 * Reporting the failure is still the right call: claiming success
		 * for data that reads cannot see would be silent corruption. */
		SPDK_ERRLOG("Failed to record chunk %" PRIu64 " in the overlay: "
			    "%d (the write is in the log but not yet visible)\n",
			    cio->chunk_index, rc);
		s3_chunk_io_finish(cio, rc);
		return;
	}

	ctx->wal_writes++;
	s3_flusher_kick(ctx->flusher);

	s3_chunk_io_finish(cio, 0);
}

/* Submit one slice to the log.
 *
 * Never fails synchronously: the slice is either completed through
 * s3_chunk_wal_done() or parked for a retry. */
static void
s3_chunk_wal_submit(void *arg)
{
	struct s3_chunk_io *cio = arg;
	struct s3_ctx *ctx = cio->bs_io->ctx;

	/* The overlay holds every acknowledged write until the flusher gets to
	 * it, so it needs a cap of its own -- the WAL's high-water mark bounds
	 * log space, not RAM. */
	if (s3_overlay_is_full(ctx->overlay)) {
		ctx->wal_retries++;
		s3_flusher_kick(ctx->flusher);
		if (s3_retry_queue(ctx, s3_chunk_wal_submit, cio) == 0) {
			return;
		}
		s3_chunk_io_finish(cio, -ENOMEM);
		return;
	}

	s3_wal_append_write(ctx->wal, cio->lba, cio->nblocks, cio->user_buf,
			    cio->chunk_index, &cio->wal_seq,
			    s3_chunk_wal_done, cio);
}

/* ==========================================================================
 * Write path (direct to S3; RMW handles writes shorter than a chunk)
 * ========================================================================== */

static void
s3_chunk_old_deleted(void *cb_arg, int status)
{
	struct s3_chunk_io *cio = cb_arg;

	if (status != 0) {
		/* A failed delete of the old object does not affect the
		 * correctness of this write -- the new object is already in place
		 * and the mapping updated. The old object becomes an orphan for
		 * GC. Log only. */
		SPDK_WARNLOG("Failed to delete superseded chunk object: %d "
			     "(leaked, GC will collect)\n", status);
	}

	s3_bs_io_put(cio->bs_io);
	free(cio->chunk_buf);
	free(cio);
}

/* The mapping is durable: delete the object it superseded.
 *
 * old_uuid is computed by the chunk map at submit time, which guarantees that
 * for a run of overwrites on the same chunk every superseded object is handed
 * back exactly once (see the header comment in s3_chunk_map.c). */
static void
s3_chunk_map_updated(void *cb_arg, const struct spdk_uuid *old_uuid, int status)
{
	struct s3_chunk_io *cio = cb_arg;
	struct s3_ctx *ctx = cio->bs_io->ctx;
	int rc;

	if (status != 0) {
		/* The mapping did not persist, which means the data is in S3 but
		 * nothing will be able to find it after a restart. This must be
		 * reported upwards: blobstore has to see the write as failed
		 * rather than believe it succeeded and then lose data on a crash.
		 * The new object is now an orphan and is left to GC. */
		SPDK_ERRLOG("Failed to update chunk map for chunk %" PRIu64
			    ": %d\n", cio->chunk_index, status);
		s3_chunk_io_finish(cio, status);
		return;
	}

	spdk_uuid_copy(&cio->old_uuid, old_uuid);
	cio->has_old = !spdk_uuid_is_null(&cio->old_uuid);

	if (cio->has_old) {
		char old_key[S3_KEY_MAX];

		s3_data_key(ctx, &cio->old_uuid, old_key, sizeof(old_key));
		rc = s3_delete(ctx->client, old_key, s3_chunk_old_deleted, cio);
		if (rc == 0) {
			/* Finish only once the delete completes, so the inflight
			 * count covers the whole chain. */
			return;
		}
		SPDK_WARNLOG("Failed to submit delete for superseded chunk: %d "
			     "(leaked, GC will collect)\n", rc);
	}

	s3_chunk_io_finish(cio, 0);
}

/* The PUT of the new object completed: update the mapping (asynchronously,
 * waiting for the journal to become durable), after which
 * s3_chunk_map_updated() deletes the old object. */
static void
s3_chunk_write_done(void *cb_arg, int status)
{
	struct s3_chunk_io *cio = cb_arg;
	struct s3_ctx *ctx = cio->bs_io->ctx;
	uint32_t valid_bytes;

	if (status != 0) {
		s3_chunk_io_finish(cio, status);
		return;
	}

	/* How far into the chunk has been written. On the RMW path the whole
	 * chunk was PUT, but only the bytes up to offset+length are meaningful. */
	valid_bytes = cio->offset_in_chunk + cio->length;

	s3_chunk_map_insert(ctx->chunk_map, cio->chunk_index, &cio->new_uuid,
			    valid_bytes, s3_chunk_map_updated, cio);
}

static int
s3_chunk_write_put(struct s3_chunk_io *cio)
{
	struct s3_ctx *ctx = cio->bs_io->ctx;
	struct iovec iov;
	char key[S3_KEY_MAX];

	/* create-once (P2): every write is a new uuid, never overwrite an
	 * existing object. */
	spdk_uuid_generate(&cio->new_uuid);
	s3_data_key(ctx, &cio->new_uuid, key, sizeof(key));

	if (cio->chunk_buf) {
		/* RMW: PUT the whole chunk together */
		iov.iov_base = cio->chunk_buf;
		iov.iov_len  = cio->offset_in_chunk + cio->length;
	} else {
		/* A write starting at the chunk's beginning: PUT the user data
		 * directly */
		iov.iov_base = cio->user_buf;
		iov.iov_len  = cio->length;
	}

	/* if_none_match=false: the uuid is freshly generated, so no key
	 * collision is possible, and COS ignores the header anyway (see
	 * s3_client.h). */
	return s3_put(ctx->client, key, &iov, 1, false,
		      s3_chunk_write_done, cio);
}

/* RMW's read phase done: merge the user data into the chunk buffer, then PUT. */
static void
s3_chunk_rmw_read_done(void *cb_arg, uint64_t bytes_read, int status)
{
	struct s3_chunk_io *cio = cb_arg;
	int rc;

	/* The same race as on the read path: the GET that fetches the old contents
	 * can also run into the flusher overwriting and deleting the old object. The
	 * retry has to go through write_submit rather than read_submit -- this is the
	 * read half of a write, and chunk_buf is already allocated, so starting over
	 * from read_submit would lose it. */
	if (status == -ENOENT && s3_chunk_read_should_reread(cio)) {
		cio->reread = true;
		free(cio->chunk_buf);
		cio->chunk_buf = NULL;
		rc = s3_chunk_write_submit(cio);
		if (rc == 0) {
			return;
		}
		status = rc;
	}

	if (status != 0) {
		s3_chunk_io_finish(cio, status);
		return;
	}

	/* When the read returned less than offset, the hole in between must be
	 * zeroed -- otherwise uninitialised memory would be PUT. calloc already
	 * cleared it; this makes the dependency explicit. */
	if (bytes_read < cio->offset_in_chunk) {
		memset((uint8_t *)cio->chunk_buf + bytes_read, 0,
		       cio->offset_in_chunk - bytes_read);
	}

	memcpy((uint8_t *)cio->chunk_buf + cio->offset_in_chunk,
	       cio->user_buf, cio->length);

	rc = s3_chunk_write_put(cio);
	if (rc != 0) {
		s3_chunk_io_finish(cio, rc);
	}
}

static int
s3_chunk_write_submit(struct s3_chunk_io *cio)
{
	struct s3_ctx *ctx = cio->bs_io->ctx;
	struct spdk_uuid uuid;
	uint32_t valid_bytes = 0;
	char key[S3_KEY_MAX];
	int rc;

	/* A write starting at the chunk's beginning: nothing before it to read,
	 * PUT directly.
	 *
	 * Note this does not require filling the whole chunk -- an object shorter
	 * than the chunk is allowed; valid_bytes records the actual length and
	 * reads zero-fill the tail. */
	if (cio->offset_in_chunk == 0) {
		return s3_chunk_write_put(cio);
	}

	/* Not starting at the beginning: RMW needed, to preserve the existing
	 * [0, offset) data. */
	ctx->rmw_count++;

	cio->chunk_buf = calloc(1, ctx->chunk_size);
	if (!cio->chunk_buf) {
		return -ENOMEM;
	}

	rc = s3_chunk_map_lookup(ctx->chunk_map, cio->chunk_index, &uuid,
				 &valid_bytes);
	if (rc == -ENOENT || valid_bytes == 0) {
		/* The chunk does not exist yet: the leading part is already
		 * zeroes (calloc guarantees it), so one GET is saved. */
		memcpy((uint8_t *)cio->chunk_buf + cio->offset_in_chunk,
		       cio->user_buf, cio->length);
		return s3_chunk_write_put(cio);
	}
	if (rc != 0) {
		return rc;
	}

	/* Only [0, offset) needs to be read back -- the bytes past offset are
	 * about to be overwritten, so reading them would be waste. */
	uint32_t need = (uint32_t)spdk_min(cio->offset_in_chunk, valid_bytes);

	s3_data_key(ctx, &uuid, key, sizeof(key));
	/* As on the read path: this GET can also hit the flusher deleting
	 * the old object underneath it. */
	spdk_uuid_copy(&cio->read_uuid, &uuid);
	return s3_get_range(ctx->client, key, 0, need, cio->chunk_buf,
			    s3_chunk_rmw_read_done, cio);
}

/* ==========================================================================
 * Sub-operation completion
 * ========================================================================== */

static void
s3_chunk_io_finish(struct s3_chunk_io *cio, int status)
{
	struct s3_bs_io *bs_io = cio->bs_io;

	if (status != 0 && bs_io->status == 0) {
		bs_io->status = status;
	}

	free(cio->chunk_buf);
	free(cio);

	s3_bs_io_put(bs_io);
}

/* ==========================================================================
 * I/O splitting: cut one LBA request spanning several chunks into one
 * sub-operation per chunk
 * ========================================================================== */

/* Split the request and hand each chunk to S3. Runs on the owner thread. */
static void
s3_bs_io_submit(void *arg)
{
	struct s3_bs_io *bs_io = arg;
	struct s3_ctx *ctx = bs_io->ctx;
	uint64_t remaining_bytes;
	uint64_t cur_lba;
	uint8_t *cur_buf;

	/* Everything below touches unsynchronized owner-thread state: inflight,
	 * the chunk map, and the S3 client. */
	assert(ctx->owner_thread == NULL || ctx->owner_thread == spdk_get_thread());

	ctx->inflight++;

	/* Hold a self-reference so a sub-operation completing synchronously cannot
	 * free bs_io while the split is still running -- the rest of the loop
	 * would then touch freed memory. Released once submission ends. */
	bs_io->num_pending = 1;
	ctx->submit_depth++;

	remaining_bytes = (uint64_t)bs_io->lba_count << S3LVOL_BLOCK_SHIFT;
	cur_lba = bs_io->lba;
	cur_buf = bs_io->payload;

	while (remaining_bytes > 0) {
		struct s3_chunk_io *cio;
		uint32_t offset_in_chunk;
		uint32_t this_len;
		int rc;

		offset_in_chunk = s3_lba_offset_in_chunk(cur_lba, ctx->chunk_shift);
		this_len = (uint32_t)spdk_min(remaining_bytes,
					      ctx->chunk_size - offset_in_chunk);

		/* A slice also has to fit one log entry. A chunk-sized write does
		 * not, so the split happens here where the LBA range is still
		 * known rather than inside the WAL. Both halves land in the same
		 * chunk and are merged by the overlay, so this costs nothing
		 * beyond one extra entry. */
		if (bs_io->is_write && ctx->wal) {
			this_len = (uint32_t)spdk_min(this_len, S3_WAL_MAX_PAYLOAD);
		}

		cio = calloc(1, sizeof(*cio));
		if (!cio) {
			if (bs_io->status == 0) {
				bs_io->status = -ENOMEM;
			}
			break;
		}

		cio->bs_io           = bs_io;
		cio->chunk_index     = s3_lba_to_chunk_index(cur_lba, ctx->chunk_shift);
		cio->offset_in_chunk = offset_in_chunk;
		cio->length          = this_len;
		cio->user_buf        = cur_buf;
		cio->lba             = cur_lba;
		cio->nblocks         = this_len >> S3LVOL_BLOCK_SHIFT;

		/* The WAL and the overlay address data in blocks, so a slice that
		 * is not a whole number of blocks could not be logged. blobstore
		 * never issues one; assert rather than silently mis-split. */
		assert((this_len & (S3LVOL_BLOCK_SIZE - 1)) == 0);

		bs_io->num_pending++;

		if (bs_io->is_write && ctx->wal) {
			/* Completes or parks on its own; never reports an error
			 * back to this loop. */
			s3_chunk_wal_submit(cio);
			rc = 0;
		} else {
			rc = bs_io->is_write ? s3_chunk_write_submit(cio)
					     : s3_chunk_read_submit(cio);
		}
		if (rc != 0) {
			SPDK_ERRLOG("Failed to submit chunk %s for chunk %" PRIu64 ": %d\n",
				    bs_io->is_write ? "write" : "read",
				    cio->chunk_index, rc);
			/* Not handed to s3_client yet, so clean up here. */
			s3_chunk_io_finish(cio, rc);
			break;
		}

		remaining_bytes -= this_len;
		cur_lba += this_len >> S3LVOL_BLOCK_SHIFT;
		cur_buf += this_len;
	}

	/* Release the self-reference. Completes the I/O if every sub-operation has
	 * already finished. */
	bs_io->submit_done = true;
	s3_bs_io_put(bs_io);
	/* bs_io may be freed by now. ctx is not: destroy() refuses to free it
	 * while inflight > 0, and this I/O still counts until its completion is
	 * delivered. */
	assert(ctx->submit_depth > 0);
	ctx->submit_depth--;
}

static void
s3_bs_dev_rw(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
	     void *payload, uint64_t lba, uint32_t lba_count,
	     struct spdk_bs_dev_cb_args *cb_args, bool is_write)
{
	struct s3_ctx *ctx = (struct s3_ctx *)dev;
	struct s3_bs_io *bs_io;
	int rc;

	(void)channel;

	/* Validation and allocation are safe on any thread: blockcnt is immutable
	 * once the bs_dev exists and no shared state is touched. The early-error
	 * callbacks below therefore run inline on the calling thread, which is
	 * exactly where blobstore expects them. */
	if (lba + lba_count > dev->blockcnt) {
		SPDK_ERRLOG("IO out of range: lba=%" PRIu64 " count=%u blockcnt=%" PRIu64 "\n",
			    lba, lba_count, dev->blockcnt);
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -EINVAL);
		return;
	}
	if (lba_count == 0) {
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, 0);
		return;
	}

	bs_io = calloc(1, sizeof(*bs_io));
	if (!bs_io) {
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -ENOMEM);
		return;
	}
	bs_io->ctx           = ctx;
	bs_io->cb_args       = cb_args;
	bs_io->is_write      = is_write;
	bs_io->payload       = payload;
	bs_io->lba           = lba;
	bs_io->lba_count     = lba_count;
	bs_io->submit_thread = spdk_get_thread();

	if (ctx->owner_thread == NULL ||
	    ctx->owner_thread == bs_io->submit_thread) {
		s3_bs_io_submit(bs_io);
		return;
	}

	rc = spdk_thread_send_msg(ctx->owner_thread, s3_bs_io_submit, bs_io);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to bounce IO to the owner thread: %d\n", rc);
		free(bs_io);
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, rc);
	}
}

/* ==========================================================================
 * spdk_bs_dev callbacks
 * ========================================================================== */

static void
s3_bs_dev_read(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
	       void *payload, uint64_t lba, uint32_t lba_count,
	       struct spdk_bs_dev_cb_args *cb_args)
{
	s3_bs_dev_rw(dev, channel, payload, lba, lba_count, cb_args, false);
}

static void
s3_bs_dev_write(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		void *payload, uint64_t lba, uint32_t lba_count,
		struct spdk_bs_dev_cb_args *cb_args)
{
	s3_bs_dev_rw(dev, channel, payload, lba, lba_count, cb_args, true);
}

/* The iovec variants: start with the most naive implementation -- convert
 * each segment into a single-buffer I/O.
 *
 * The blobstore's readv/writev are mainly for large data-plane I/Os on blobs;
 * the early goal was only to make bs_init work, and the metadata path uses
 * read/write. Optimise into real scatter-gather once there is a performance
 * requirement. */
static void
s3_bs_dev_readv(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
		struct spdk_bs_dev_cb_args *cb_args)
{
	if (iovcnt == 1) {
		s3_bs_dev_rw(dev, channel, iov[0].iov_base, lba, lba_count,
			     cb_args, false);
		return;
	}

	/* Multiple segments are not supported yet: a silent error would become
	 * data corruption, so an explicit error is safer. */
	SPDK_ERRLOG("readv with iovcnt=%d not supported yet\n", iovcnt);
	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -ENOTSUP);
}

static void
s3_bs_dev_writev(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		 struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
		 struct spdk_bs_dev_cb_args *cb_args)
{
	if (iovcnt == 1) {
		s3_bs_dev_rw(dev, channel, iov[0].iov_base, lba, lba_count,
			     cb_args, true);
		return;
	}

	SPDK_ERRLOG("writev with iovcnt=%d not supported yet\n", iovcnt);
	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -ENOTSUP);
}

static void
s3_bs_dev_readv_ext(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		    struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
		    struct spdk_bs_dev_cb_args *cb_args,
		    struct spdk_blob_ext_io_opts *io_opts)
{
	(void)io_opts;
	s3_bs_dev_readv(dev, channel, iov, iovcnt, lba, lba_count, cb_args);
}

static void
s3_bs_dev_writev_ext(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		     struct iovec *iov, int iovcnt, uint64_t lba, uint32_t lba_count,
		     struct spdk_bs_dev_cb_args *cb_args,
		     struct spdk_blob_ext_io_opts *io_opts)
{
	(void)io_opts;
	s3_bs_dev_writev(dev, channel, iov, iovcnt, lba, lba_count, cb_args);
}

/* unmap / write_zeroes.
 *
 * With a WAL both are logged and then applied to the overlay: a chunk covered
 * end to end has its mapping dropped, while a partial range is recorded as
 * zeroes for the flusher to write out. Logging is what makes the clearing
 * survive a crash -- replaying only the writes would bring back data the caller
 * discarded.
 *
 * Without a WAL only fully covered chunks are handled: zeroing part of an S3
 * object needs a read-modify-write that path does not implement, so a partial
 * range reports success without doing anything. That does not corrupt anything
 * blobstore relies on -- it uses unmap to reclaim space, not to zero data -- but
 * it does leave the old bytes readable.
 *
 * Releasing a mapping is asynchronous (it waits for the journal to become
 * durable) while one unmap may span several chunks, so a refcount is used to
 * call back into blobstore only after all of them finish. */
struct s3_unmap_ctx {
	struct s3_ctx                *ctx;
	struct spdk_bs_dev_cb_args      *cb_args;

	/* Thread that submitted, and where the completion must be delivered.
	 * Same reasoning as struct s3_bs_io. */
	struct spdk_thread              *submit_thread;

	/* Saved so the loop can run later on the owner thread. */
	uint64_t                         lba;
	uint64_t                         lba_count;

	/* Outstanding removes plus one self-reference. The self-reference is
	 * required: without it, a chunk that completes inline (-ENOENT, or the
	 * memory-only mode) would drive the count to zero while the loop has not
	 * yet submitted the remaining chunks. */
	uint32_t                         refcnt;

	/* First error encountered. -ENOENT does not count -- it just means this
	 * chunk had no mapping to begin with. */
	int                              status;

	/* write_zeroes rather than unmap.
	 *
	 * The two share this whole path because on a fully covered chunk they are
	 * the same operation: drop the mapping, delete the object, and an
	 * unallocated chunk reads back as zeroes either way.
	 *
	 * They part company on a partial range, which is why this flag exists.
	 * unmap is advisory -- ignoring it is allowed, the caller only loses the
	 * space. write_zeroes is not: the caller is told those blocks now read as
	 * zero, and code above us relies on it. blobstore zeroes metadata pages
	 * 4 KiB at a time (blob_persist_zero_pages), far below the 1 MiB chunk, so
	 * partial write_zeroes is the common case, not a corner. */
	bool                             zeroes;
};

/* Deliver the unmap completion. Must run on the submitting thread, for the same
 * per-channel request list reason described on struct s3_bs_io. */
static void
s3_unmap_deliver(void *arg)
{
	struct s3_unmap_ctx *uctx = arg;
	struct spdk_bs_dev_cb_args *cb_args = uctx->cb_args;
	int status = uctx->status;

	free(uctx);

	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, status);
}

static void
s3_unmap_put(struct s3_unmap_ctx *uctx)
{
	int rc;

	assert(uctx->refcnt > 0);
	if (--uctx->refcnt > 0) {
		return;
	}

	if (uctx->submit_thread == spdk_get_thread() && uctx->ctx->submit_depth == 0) {
		s3_unmap_deliver(uctx);
		return;
	}

	rc = spdk_thread_send_msg(uctx->submit_thread, s3_unmap_deliver, uctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to bounce unmap completion to the submitting "
			    "thread: %d (leaking the context)\n", rc);
	}
}

static void
s3_unmap_chunk_removed(void *cb_arg, const struct spdk_uuid *old_uuid, int status)
{
	struct s3_unmap_ctx *uctx = cb_arg;
	int rc;

	if (status == 0) {
		char key[S3_KEY_MAX];

		/* Best effort delete; on failure GC picks it up. Not waited on --
		 * unmap semantics do not require the object to vanish
		 * immediately.
		 *
		 * The submit result is checked even though the outcome is not:
		 * this call passed NULL for years while s3_delete() rejected NULL
		 * callbacks, so it always failed here and never deleted anything. */
		s3_data_key(uctx->ctx, old_uuid, key, sizeof(key));
		rc = s3_delete(uctx->ctx->client, key, NULL, NULL);
		if (rc != 0) {
			SPDK_WARNLOG("Could not submit delete for unmapped chunk "
				     "(key=%s): %s\n", key, spdk_strerror(-rc));
		}
	} else if (status != -ENOENT) {
		/* The journal write failed, so the mapping is still there and this
		 * unmap did not take effect. Report it upwards: if blobstore
		 * believes the space was reclaimed, reading stale data later
		 * becomes a correctness problem. */
		if (uctx->status == 0) {
			uctx->status = status;
		}
	}

	s3_unmap_put(uctx);
}

/* One chunk's share of an unmap on the WAL path.
 *
 * Unmap and write_zeroes have to go through the log for the same reason writes
 * do: after a crash, replay has to reproduce the fact that a range was cleared.
 * Replaying only the writes would resurrect data the caller discarded. */
struct s3_unmap_io {
	struct s3_unmap_ctx *uctx;

	uint64_t chunk_index;
	uint64_t lba;
	uint32_t nblocks;
	uint64_t wal_seq;

	/* The range covers the chunk end to end, so the mapping can be dropped
	 * instead of writing an object full of zeroes. */
	bool whole;
};

static void s3_unmap_wal_submit(void *arg);

static void
s3_unmap_wal_done(void *cb_arg, int status)
{
	struct s3_unmap_io *uio = cb_arg;
	struct s3_unmap_ctx *uctx = uio->uctx;
	struct s3_ctx *ctx = uctx->ctx;
	int rc;

	if (status == -EAGAIN) {
		s3_flusher_kick(ctx->flusher);
		if (s3_retry_queue(ctx, s3_unmap_wal_submit, uio) == 0) {
			return;
		}
		status = -ENOMEM;
	}
	if (status != 0) {
		if (uctx->status == 0) {
			uctx->status = status;
		}
		free(uio);
		s3_unmap_put(uctx);
		return;
	}

	if (uio->whole) {
		/* Everything the overlay held for this chunk is void now, and the
		 * epoch bump makes a flush that already collected it give up
		 * instead of publishing unmapped content. */
		s3_overlay_drop_chunk(ctx->overlay, uio->chunk_index);
		/* Not needed for correctness -- an unmapped chunk has no uuid left
		 * for a reader to ask for, so the entry could never be returned --
		 * but it frees the slot now instead of at the end of the LRU. A
		 * discard of a large range would otherwise leave the cache full of
		 * entries for chunks that no longer exist. */
		s3_cache_drop_chunk(ctx->cache, uio->chunk_index);
		s3_chunk_map_remove(ctx->chunk_map, uio->chunk_index,
				    s3_unmap_chunk_removed, uctx);
		/* The reference passes to s3_unmap_chunk_removed(). */
		free(uio);
		return;
	}

	/* Partial cover: record zeroes and let the flusher rewrite the object.
	 * The direct-to-S3 path silently skipped this case, which left the old
	 * bytes readable; here it can be done properly for the cost of one
	 * flush. */
	rc = s3_overlay_write(ctx->overlay, uio->lba, uio->nblocks, NULL,
			      uio->wal_seq);
	if (rc != 0 && uctx->status == 0) {
		uctx->status = rc;
	}
	s3_flusher_kick(ctx->flusher);

	free(uio);
	s3_unmap_put(uctx);
}

static void
s3_unmap_wal_submit(void *arg)
{
	struct s3_unmap_io *uio = arg;
	struct s3_ctx *ctx = uio->uctx->ctx;

	if (uio->whole) {
		s3_wal_append_unmap(ctx->wal, uio->lba, uio->nblocks,
				    uio->chunk_index, &uio->wal_seq,
				    s3_unmap_wal_done, uio);
	} else {
		s3_wal_append_zeroes(ctx->wal, uio->lba, uio->nblocks,
				     uio->chunk_index, &uio->wal_seq,
				     s3_unmap_wal_done, uio);
	}
}

/* Release the mappings. Runs on the owner thread. */
static void
s3_unmap_submit(void *arg)
{
	struct s3_unmap_ctx *uctx = arg;
	struct s3_ctx *ctx = uctx->ctx;
	uint32_t blocks_per_chunk = s3_blocks_per_chunk(ctx);
	uint64_t cur = uctx->lba;
	uint64_t end = uctx->lba + uctx->lba_count;

	assert(ctx->owner_thread == NULL || ctx->owner_thread == spdk_get_thread());
	ctx->submit_depth++;

	while (cur < end) {
		uint64_t chunk_index = s3_lba_to_chunk_index(cur, ctx->chunk_shift);
		uint64_t chunk_start = s3_chunk_index_to_lba(chunk_index, ctx->chunk_shift);
		uint64_t chunk_end   = chunk_start + blocks_per_chunk;
		bool whole = (cur == chunk_start && end >= chunk_end);

		if (ctx->wal) {
			struct s3_unmap_io *uio = calloc(1, sizeof(*uio));

			if (!uio) {
				if (uctx->status == 0) {
					uctx->status = -ENOMEM;
				}
				break;
			}
			uio->uctx        = uctx;
			uio->chunk_index = chunk_index;
			uio->lba         = cur;
			uio->nblocks     = (uint32_t)(spdk_min(end, chunk_end) - cur);
			uio->whole       = whole;

			uctx->refcnt++;
			s3_unmap_wal_submit(uio);
		} else if (whole) {
			/* Only release the mapping when the chunk is fully
			 * covered; a partial cover would need a read-modify-write
			 * that this path does not implement. */
			uctx->refcnt++;
			s3_chunk_map_remove(ctx->chunk_map, chunk_index,
					    s3_unmap_chunk_removed, uctx);
		} else if (uctx->zeroes) {
			/* Partial write_zeroes with no WAL. Whether this is a
			 * problem depends entirely on whether the chunk holds
			 * anything yet.
			 *
			 * Unmapped: it already reads back as zeroes, so zeroing
			 * part of it is a no-op and reporting success is honest.
			 * This is the common case and the reason it must not be an
			 * error -- spdk_bs_init() clears its metadata region before
			 * anything has been allocated (observed: 68 blocks of
			 * chunk 0), and failing that would break creating an
			 * lvstore at all.
			 *
			 * Mapped: the object holds real bytes, and zeroing part of
			 * it needs a read-modify-write this path does not
			 * implement. Reported rather than silently completed,
			 * because the caller is being promised those blocks now read
			 * as zero -- and when the caller is blobstore clearing a
			 * metadata page, believing that wrongly surfaces later as
			 * corruption a long way from here.
			 *
			 * Only reachable in direct-to-S3 mode; with a WAL the range
			 * is logged as zeroes above. An lvstore without a WAL cannot
			 * be attached anyway (its chunk map lives in the journal),
			 * so requiring a WAL is a better fix than implementing the
			 * read-modify-write here. */
			struct spdk_uuid existing;
			uint32_t valid_bytes;

			if (s3_chunk_map_lookup(ctx->chunk_map, chunk_index,
						&existing, &valid_bytes) == 0) {
				SPDK_ERRLOG("Partial write_zeroes on mapped chunk %"
					    PRIu64 " (lba %" PRIu64 ", %" PRIu32
					    " blocks) needs a WAL: the "
					    "read-modify-write is not implemented "
					    "in direct-to-S3 mode\n",
					    chunk_index, cur,
					    (uint32_t)(spdk_min(end, chunk_end) - cur));
				if (uctx->status == 0) {
					uctx->status = -ENOTSUP;
				}
			}
		}
		cur = chunk_end;
	}

	s3_unmap_put(uctx);
	/* uctx may be freed by now; ctx is not (see s3_bs_io_submit). */
	assert(ctx->submit_depth > 0);
	ctx->submit_depth--;
}

static void
s3_unmap_common(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		uint64_t lba, uint64_t lba_count,
		struct spdk_bs_dev_cb_args *cb_args, bool zeroes)
{
	struct s3_ctx *ctx = (struct s3_ctx *)dev;
	struct s3_unmap_ctx *uctx;
	int rc;

	(void)channel;

	uctx = calloc(1, sizeof(*uctx));
	if (!uctx) {
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, -ENOMEM);
		return;
	}
	uctx->ctx           = ctx;
	uctx->cb_args       = cb_args;
	uctx->lba           = lba;
	uctx->lba_count     = lba_count;
	uctx->submit_thread = spdk_get_thread();
	uctx->refcnt        = 1;   /* self-reference, released when the loop ends */
	uctx->zeroes        = zeroes;

	if (ctx->owner_thread == NULL ||
	    ctx->owner_thread == uctx->submit_thread) {
		s3_unmap_submit(uctx);
		return;
	}

	rc = spdk_thread_send_msg(ctx->owner_thread, s3_unmap_submit, uctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to bounce unmap to the owner thread: %d\n", rc);
		free(uctx);
		cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, rc);
	}
}

static void
s3_bs_dev_unmap(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		uint64_t lba, uint64_t lba_count,
		struct spdk_bs_dev_cb_args *cb_args)
{
	/* Advisory. A partial range that cannot be honoured is dropped rather than
	 * reported: the caller only loses the chance to reclaim that space, and
	 * nothing above depends on the blocks having changed. */
	s3_unmap_common(dev, channel, lba, lba_count, cb_args, false);
}

static void
s3_bs_dev_write_zeroes(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		       uint64_t lba, uint64_t lba_count,
		       struct spdk_bs_dev_cb_args *cb_args)
{
	/* Shares unmap's path because on a fully covered chunk the two are the
	 * same operation -- drop the mapping, delete the object, and an
	 * unallocated chunk reads back as zeroes.
	 *
	 * The flag matters for a partial range: unlike unmap this is a promise
	 * that those blocks now read as zero, so it may not be quietly skipped.
	 * With a WAL the range is logged as zeroes; without one it fails. */
	s3_unmap_common(dev, channel, lba, lba_count, cb_args, true);
}

static void
s3_bs_dev_flush(struct spdk_bs_dev *dev, struct spdk_io_channel *channel,
		struct spdk_bs_dev_cb_args *cb_args)
{
	(void)dev;
	(void)channel;

	/* Nothing to do in either mode, for different reasons.
	 *
	 * Direct to S3: a PUT is durable by the time it completes.
	 * With a WAL: a write is only acknowledged once its batch is durable on
	 * the local bdev, so everything blobstore believes it wrote already is.
	 *
	 * This deliberately does *not* wait for the flusher. flush only has to
	 * guarantee durability, and blobstore issues it often (persisting
	 * metadata, unloading); forcing a full S3 round trip for every one of
	 * those would be pointless. Waiting for S3 durability is a separate
	 * operation -- s3_bs_dev_drain(). */
	cb_args->cb_fn(cb_args->channel, cb_args->cb_arg, 0);
}

/* Per-channel state. Currently empty -- the S3 client manages its own
 * connection pool, the chunk map is accessed single-threaded, and nothing
 * needs to be isolated per channel.
 *
 * But the channel itself **must exist**: the blobstore's bs_channel_create()
 * treats a create_channel() that returns NULL as failure everywhere
 * (blobstore.c:3674), so create_channel cannot be left NULL the way
 * zeroes.c / blob_bs_dev.c do -- those are only back_bs_devs, which the
 * blobstore never creates channels for. We are the primary bs_dev.
 *
 * blob_bdev gets its channel from spdk_bdev_get_io_channel(); there is no
 * bdev underneath us, so we register our own io_device. */
struct s3_bs_channel {
	struct s3_ctx   *ctx;
};

static int
s3_bs_channel_create_cb(void *io_device, void *ctx_buf)
{
	struct s3_bs_channel *ch = ctx_buf;

	ch->ctx = io_device;
	return 0;
}

static void
s3_bs_channel_destroy_cb(void *io_device, void *ctx_buf)
{
	(void)io_device;
	(void)ctx_buf;
}

static struct spdk_io_channel *
s3_bs_dev_create_channel(struct spdk_bs_dev *dev)
{
	struct s3_ctx *ctx = (struct s3_ctx *)dev;

	return spdk_get_io_channel(ctx);
}

static void
s3_bs_dev_destroy_channel(struct spdk_bs_dev *dev, struct spdk_io_channel *channel)
{
	(void)dev;

	if (channel) {
		spdk_put_io_channel(channel);
	}
}

/* ctx may only be freed after the io_device unregister completes --
 * spdk_io_device_unregister is asynchronous and must first wait for all
 * channels to be returned. */
static void
s3_bs_dev_free_cb(void *io_device)
{
	struct s3_ctx *ctx = io_device;
	s3_bs_dev_cb cb_fn = ctx->destroy_cb;
	void *cb_arg = ctx->destroy_arg;

	/* Nothing may still be able to reach ctx, the chunk map or the journal. A
	 * checkpoint is the one thing that can be outstanding without holding a
	 * reference or counting towards ctx->inflight, and its completion *writes*
	 * through those pointers, so getting this wrong corrupts memory rather than
	 * merely reading stale bytes. s3_bs_dev_teardown() waits for it; this is the
	 * assertion that it did. */
	if (ctx->ckpt_in_flight) {
		SPDK_ERRLOG("s3_bs_dev for '%s' freed with checkpoint gen=%" PRIu64
			    " still in flight; its completion will write through "
			    "freed memory\n", ctx->prefix, ctx->ckpt_gen);
		assert(false);
	}

	/* Before the map goes: this is the only point where it reflects every
	 * object the lvstore owns, blobstore's unload writes included. Destroy
	 * uses it to build the delete list. */
	if (ctx->reap_cb) {
		ctx->reap_cb(ctx->reap_arg, ctx->chunk_map);
	}

	s3_overlay_destroy(ctx->overlay);
	/* After the overlay and before the chunk map, though the order does not
	 * matter here: by now nothing can reach the cache, and teardown has already
	 * waited for its writes. */
	s3_cache_destroy(ctx->cache);
	s3_chunk_map_destroy(ctx->chunk_map);
	free(ctx->prefix);
	free(ctx);

	/* Last thing, and the reason this callback exists: the caller may now
	 * release the journal and the local device, which the chunk map and the
	 * WAL were still writing to a moment ago. */
	if (cb_fn) {
		cb_fn(cb_arg, 0);
	}
}

/* Anything still parked was never acknowledged, and blobstore is gone, so there
 * is nobody left to complete it to. */
static void
s3_bs_dev_drop_retries(struct s3_ctx *ctx)
{
	struct s3_retry_item *item;

	/* The checkpoint poller is already gone -- destroy() unregisters it before
	 * anything else, because everything a checkpoint touches is torn down here.
	 * Unregistering is idempotent, so this stays as the belt to that braces. */
	if (ctx->ckpt_poller) {
		spdk_poller_unregister(&ctx->ckpt_poller);
	}

	if (ctx->retry_poller) {
		spdk_poller_unregister(&ctx->retry_poller);
	}
	while ((item = STAILQ_FIRST(&ctx->retry_q)) != NULL) {
		STAILQ_REMOVE_HEAD(&ctx->retry_q, link);
		free(item);
	}
	ctx->retry_count = 0;
}

static void
s3_bs_dev_wal_closed(void *cb_arg, int status)
{
	struct s3_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to close the WAL: %d — the log stays on disk "
			    "and will be replayed on the next attach\n", status);
	}
	ctx->wal = NULL;

	spdk_io_device_unregister(ctx, s3_bs_dev_free_cb);
}

static void
s3_bs_dev_flusher_drained(void *cb_arg, int status)
{
	struct s3_ctx *ctx = cb_arg;
	struct s3_wal *wal = ctx->wal;

	if (status != 0) {
		/* -ETIMEDOUT means dirty chunks are left behind. Not a loss --
		 * they are in the log -- but say so, because until they are
		 * replayed S3 does not have them. */
		SPDK_WARNLOG("Flusher did not fully drain before destroy: %d\n",
			     status);
	}

	/* Order matters: the flusher writes the WAL (truncation, super sync), so
	 * it has to be gone before the log is closed. */
	s3_flusher_destroy(ctx->flusher);
	ctx->flusher = NULL;

	s3_bs_dev_drop_retries(ctx);

	if (wal) {
		s3_wal_close(wal, s3_bs_dev_wal_closed, ctx);
		return;
	}

	spdk_io_device_unregister(ctx, s3_bs_dev_free_cb);
}

static void s3_bs_dev_teardown(struct s3_ctx *ctx);

/* Wait out an in-flight cache fill, then resume teardown.
 *
 * Registered only when there is something to wait for; see the call site for why
 * waiting is necessary at all. */
static int
s3_cache_quiesce_poll(void *arg)
{
	struct s3_ctx *ctx = arg;

	if (!s3_cache_is_quiesced(ctx->cache)) {
		return SPDK_POLLER_BUSY;
	}

	spdk_poller_unregister(&ctx->cache_quiesce_poller);
	s3_bs_dev_teardown(ctx);

	return SPDK_POLLER_BUSY;
}

/* Teardown proper, split out of destroy() because it can have to wait.
 *
 * Called once from destroy() and once more from ckpt_finish() if a checkpoint was
 * still running then. Everything downstream of here frees the chunk map and hands
 * the journal and the local device back to the caller, so nothing that touches
 * those may still be outstanding when it runs.
 */
static void
s3_bs_dev_teardown(struct s3_ctx *ctx)
{
	/* A checkpoint holds no reference of its own: it reaches ctx, the chunk map,
	 * the journal and the local device through this pointer, and its last step
	 * (s3_journal_truncate) runs after two asynchronous hops -- an S3 PUT and a
	 * super block write. Freeing while it is in flight means those land on freed
	 * memory, which is a write, not a read: it corrupts whatever took the place.
	 *
	 * So wait. ckpt_finish() calls back here. Waiting rather than cancelling
	 * because the snapshot is already durable by then and the super block may
	 * already name it; letting it finish is what keeps the journal's truncation
	 * point consistent with what the super block claims. */
	if (ctx->ckpt_in_flight) {
		SPDK_NOTICELOG("Destroying s3_bs_dev for '%s': waiting for the "
			       "checkpoint gen=%" PRIu64 " that is still in flight\n",
			       ctx->prefix, ctx->ckpt_gen);
		return;
	}

	/* A cache fill is a write to the local device, and it holds a pointer into
	 * the cache's slot array while it runs. Two things downstream of here would
	 * break under it: the cache is freed in s3_bs_dev_free_cb, and the caller
	 * closes the local device's descriptor as soon as destroy_cb fires -- and
	 * closing a descriptor with I/O outstanding on it is not allowed.
	 *
	 * In practice the WAL close that follows takes a write plus a flush and the
	 * fill would land inside it, but "usually long enough" is not a lifetime
	 * rule. New fills cannot start, because s3_flush_map_updated stops
	 * populating once ctx->destroying is set. */
	if (ctx->cache && !s3_cache_is_quiesced(ctx->cache)) {
		if (!ctx->cache_quiesce_poller) {
			SPDK_NOTICELOG("Destroying s3_bs_dev for '%s': waiting for "
				       "a cache fill to land\n", ctx->prefix);
			ctx->cache_quiesce_poller =
				SPDK_POLLER_REGISTER(s3_cache_quiesce_poll, ctx,
						     S3_RETRY_POLL_US);
			if (ctx->cache_quiesce_poller) {
				return;
			}
			/* No poller to be had. Falling through would free the
			 * cache under its own write; leaking it is survivable,
			 * since it holds nothing but copies. */
			SPDK_ERRLOG("Could not register the cache quiesce poller "
				    "for '%s'; leaking the cache rather than "
				    "freeing it under an in-flight write\n",
				    ctx->prefix);
			ctx->cache = NULL;
		} else {
			return;
		}
	}

	if (ctx->flusher) {
		/* blobstore writes its final metadata while unloading, and on the
		 * WAL path those writes are acknowledged from the log, so there is
		 * normally still work outstanding right here.
		 *
		 * destroy() cannot block, so finish asynchronously instead.
		 * Nothing references this bs_dev any more once blobstore has let
		 * go of it, so it can safely stay alive until S3 has the data --
		 * which is what INV2 requires. The caller learns when that is done
		 * through s3_bs_dev_set_destroy_cb(). */
		s3_flusher_drain(ctx->flusher, 0, s3_bs_dev_flusher_drained, ctx);
		return;
	}

	s3_bs_dev_drop_retries(ctx);

	/* Asynchronous: s3_bs_dev_free_cb fires only after all channels are
	 * returned. */
	spdk_io_device_unregister(ctx, s3_bs_dev_free_cb);
}

static void
s3_bs_dev_destroy(struct spdk_bs_dev *dev)
{
	struct s3_ctx *ctx = (struct s3_ctx *)dev;
	struct s3_flusher_stats fstats = {};
	struct s3_cache_stats cstats = {};

	if (ctx->flusher) {
		s3_flusher_get_stats(ctx->flusher, &fstats);
	}
	if (ctx->cache) {
		s3_cache_get_stats(ctx->cache, &cstats);
	}

	SPDK_NOTICELOG("Destroying s3_bs_dev: %" PRIu64 " chunks allocated, "
		       "%" PRIu64 " RMW, %" PRIu64 " zero-fills, "
		       "%" PRIu64 " WAL writes (%" PRIu64 " parked), "
		       "%" PRIu64 " overlay reads, %" PRIu64 " chunks flushed, "
		       "cache %" PRIu64 " hits / %" PRIu64 " misses (%" PRIu64
		       " filled, %" PRIu64 " evicted)\n",
		       s3_chunk_map_get_allocated(ctx->chunk_map),
		       ctx->rmw_count, ctx->zero_fill_count,
		       ctx->wal_writes, ctx->wal_retries, ctx->overlay_hits,
		       fstats.chunks_flushed,
		       cstats.hits, cstats.misses, cstats.populates,
		       cstats.evictions);

	/* First thing, before any of the waiting below: the poller must not start a
	 * checkpoint from here on. Teardown is asynchronous -- a flusher drain and a
	 * WAL close, hundreds of milliseconds of reactor time -- and the poller kept
	 * running through all of it, so an interval that came due in that window
	 * started a checkpoint against a device being freed. That is how this was
	 * found: a segfault in s3_journal_truncate() with the journal pointer reading
	 * as "cos.ap-n", i.e. freed memory already reused for an endpoint string. */
	ctx->destroying = true;
	if (ctx->ckpt_poller) {
		spdk_poller_unregister(&ctx->ckpt_poller);
	}

	/* blobstore guarantees none of its own I/O is outstanding here ("once all
	 * references to it during unload callback context have been completed").
	 * Assert that premise: freeing while a request is in flight would corrupt
	 * its completion, and leaking is the lesser evil.
	 *
	 * Note inflight reaching zero is not by itself proof that every completion
	 * has been handed back: s3_bs_io_complete() decrements it before bouncing
	 * the delivery through the message queue. What makes this safe is that
	 * blobstore only learns an I/O finished from that delivery, so any unload
	 * it starts is queued behind it on this same thread.
	 *
	 * Chunk uploads are a different matter -- those are ours rather than
	 * blobstore's, and they are drained just below. */
	if (ctx->inflight > 0) {
		SPDK_ERRLOG("s3_bs_dev destroyed with %" PRIu64 " IO still in "
			    "flight — blobstore should have drained them first. "
			    "Freeing now would corrupt their completions.\n",
			    ctx->inflight);
		assert(false);
		return;
	}

	/* Parked unmaps do not count into inflight (they carry their own refcount),
	 * so they are asserted separately: a parked I/O here means one escaped
	 * blobstore's drain, and drop_retries() below would otherwise silently
	 * discard it. The retry poller only exists while something is parked, so a
	 * zero count also proves it is already unregistered. */
	if (ctx->retry_count > 0) {
		SPDK_ERRLOG("s3_bs_dev destroyed with %" PRIu32 " IO still parked "
			    "for retry — blobstore should have drained them first. "
			    "Freeing now would corrupt their completions.\n",
			    ctx->retry_count);
		assert(false);
		return;
	}

	s3_bs_dev_teardown(ctx);
}

static bool
s3_bs_dev_is_zeroes(struct spdk_bs_dev *dev, uint64_t lba, uint64_t lba_count)
{
	struct s3_ctx *ctx = (struct s3_ctx *)dev;
	uint32_t blocks_per_chunk = s3_blocks_per_chunk(ctx);
	uint64_t cur = lba;
	uint64_t end = lba + lba_count;

	/* "Is zero" can only be claimed when every chunk the range covers is
	 * unallocated. Claiming wrongly makes the blobstore skip CoW and lose
	 * data outright -- so any uncertainty returns false. */
	while (cur < end) {
		uint64_t chunk_index = s3_lba_to_chunk_index(cur, ctx->chunk_shift);
		uint64_t chunk_start = s3_chunk_index_to_lba(chunk_index, ctx->chunk_shift);

		if (s3_chunk_map_lookup(ctx->chunk_map, chunk_index, NULL, NULL) == 0) {
			return false;
		}
		/* Unmapped in S3 but holding not-yet-flushed data is *not* zero.
		 * Missing this would let blobstore skip copy-on-write over data
		 * that was acknowledged only moments ago. */
		if (s3_overlay_chunk_is_live(ctx->overlay, chunk_index)) {
			return false;
		}
		cur = chunk_start + blocks_per_chunk;
	}

	return true;
}

static bool
s3_bs_dev_is_range_valid(struct spdk_bs_dev *dev, uint64_t lba, uint64_t lba_count)
{
	/* lba_count == 0 counts as invalid: the blobstore uses this interface to
	 * validate ranges, and an empty range is meaningless. */
	if (lba_count == 0) {
		return false;
	}
	return lba + lba_count <= dev->blockcnt;
}

static bool
s3_bs_dev_translate_lba(struct spdk_bs_dev *dev, uint64_t lba, uint64_t *base_lba)
{
	(void)dev;
	(void)lba;
	(void)base_lba;

	/* Our data is not on any block device -- it lives in S3 objects, spread
	 * out by the three-level mapping. Returning true would let
	 * blob_can_copy() believe it can move data "inside the device" through
	 * bs_dev->copy; that device does not exist, and any base_lba we filled
	 * in would be fake.
	 *
	 * Returning false merely gives up CoW's fast path; the blobstore falls
	 * back to a regular read + write, which we implement correctly.
	 *
	 * (We also have no ->copy implemented, so short-circuit evaluation means
	 * this is never reached today. It is still false on purpose, in case
	 * someone later fills in copy and forgets to revisit this.) */
	return false;
}

static bool
s3_bs_dev_is_degraded(struct spdk_bs_dev *dev)
{
	(void)dev;
	/* Transient faults on the S3 side are absorbed by CRT retries; when it is
	 * really unavailable, I/O errors out directly. There is currently no
	 * "degraded but usable" intermediate state. */
	return false;
}

/* ==========================================================================
 * Flusher: turn overlay content into S3 objects
 *
 * One read-modify-write per chunk per round, and never two rounds on the same
 * chunk at once -- the flusher guarantees that. Those two properties together
 * are what make concurrent partial writes safe, replacing the per-write RMW
 * that lost data.
 * ========================================================================== */

struct s3_flush_ctx {
	struct s3_ctx *ctx;

	/* Owned by the flusher; valid until the completion is invoked. */
	const struct s3_overlay_flush_view *view;

	s3_flusher_upload_cb cb_fn;
	void *cb_arg;

	/* Chunk-sized staging buffer: old object first, overlay merged on top. */
	void *chunk_buf;

	/* Length of the object being written. */
	uint32_t put_len;

	/* valid_bytes of the object being superseded, 0 when there is none. */
	uint32_t base_valid;
	struct spdk_uuid base_uuid;
	bool has_base;

	struct spdk_uuid new_uuid;
};

static void
s3_flush_finish(struct s3_flush_ctx *fc, int status)
{
	s3_flusher_upload_cb cb_fn = fc->cb_fn;
	void *cb_arg = fc->cb_arg;

	free(fc->chunk_buf);
	free(fc);

	cb_fn(cb_arg, status);
}

static void
s3_flush_map_updated(void *cb_arg, const struct spdk_uuid *old_uuid, int status)
{
	struct s3_flush_ctx *fc = cb_arg;
	struct s3_ctx *ctx = fc->ctx;

	if (status != 0) {
		/* The data reached S3 but nothing will find it after a restart,
		 * so the round counts as failed: the overlay keeps the blocks and
		 * tries again. The object just written is an orphan for GC. */
		SPDK_ERRLOG("Failed to publish chunk %" PRIu64 " in the chunk "
			    "map: %d\n", fc->view->chunk_index, status);
		s3_flush_finish(fc, status);
		return;
	}

	/* The chunk is in S3 and published, and its whole content is still in the
	 * buffer that was uploaded. Hand it to the cache before that buffer goes.
	 *
	 * This is the populate worth having. Without it a chunk's read latency
	 * *jumps* the instant its flush succeeds: the overlay releases the blocks it
	 * was serving reads from (s3_overlay.c, on flush success), so the next read
	 * of data that was local a moment ago costs a full S3 round trip. A
	 * filesystem notices immediately -- inode blocks and directory blocks are
	 * written and then read back straight away.
	 *
	 * Free, apart from one local write: no GET, and the merge that produced
	 * these bytes had to happen anyway.
	 *
	 * Skipped while tearing down. Teardown waits for cache fills to land before
	 * freeing anything (s3_bs_dev_teardown), and starting new ones underneath it
	 * would make that wait unbounded. */
	if (ctx->cache && !ctx->destroying) {
		s3_cache_populate(ctx->cache, fc->view->chunk_index,
				  &fc->new_uuid, 0, fc->chunk_buf, fc->put_len,
				  fc->put_len);
	}

	if (!spdk_uuid_is_null(old_uuid)) {
		char old_key[S3_KEY_MAX];
		int rc;

		/* Best effort and deliberately not waited on: the mapping is
		 * already durable, so a failed delete only leaves an orphan for
		 * GC, while waiting would add a round trip to every flush.
		 *
		 * "Not waited on" is not the same as "not checked". This is the
		 * hottest delete in the system -- every overwrite of a flushed
		 * chunk goes through it -- and it spent months failing at the
		 * submit call, because s3_delete() rejected the NULL callback and
		 * nobody looked at the return value. Every overwritten chunk leaked
		 * its old object. */
		s3_data_key(ctx, old_uuid, old_key, sizeof(old_key));
		rc = s3_delete(ctx->client, old_key, NULL, NULL);
		if (rc != 0) {
			SPDK_WARNLOG("Could not submit delete for the superseded "
				     "object (key=%s): %s\n",
				     old_key, spdk_strerror(-rc));
		}
	}

	s3_flush_finish(fc, 0);
}

static void
s3_flush_put_done(void *cb_arg, int status)
{
	struct s3_flush_ctx *fc = cb_arg;
	struct s3_ctx *ctx = fc->ctx;
	int rc;

	if (status != 0) {
		s3_flush_finish(fc, status);
		return;
	}

	/* The chunk may have been unmapped while this was uploading. Publishing
	 * now would resurrect discarded data, so drop the new object instead. */
	if (s3_overlay_chunk_epoch(ctx->overlay, fc->view->chunk_index) !=
	    fc->view->epoch) {
		char key[S3_KEY_MAX];

		SPDK_NOTICELOG("Chunk %" PRIu64 " was unmapped while flushing; "
			       "discarding the object just written\n",
			       fc->view->chunk_index);
		s3_data_key(ctx, &fc->new_uuid, key, sizeof(key));
		rc = s3_delete(ctx->client, key, NULL, NULL);
		if (rc != 0) {
			/* Nothing references it, so this is a leak rather than a
			 * correctness problem -- but it is worth saying so. */
			SPDK_WARNLOG("Could not submit delete for the discarded "
				     "object (key=%s): %s\n",
				     key, spdk_strerror(-rc));
		}
		s3_flush_finish(fc, 0);
		return;
	}

	s3_chunk_map_insert(ctx->chunk_map, fc->view->chunk_index, &fc->new_uuid,
			    fc->put_len, s3_flush_map_updated, fc);
}

static void
s3_flush_put(struct s3_flush_ctx *fc)
{
	struct s3_ctx *ctx = fc->ctx;
	struct iovec iov;
	char key[S3_KEY_MAX];
	int rc;

	/* Overlay goes on last so it wins over whatever the old object held. */
	s3_overlay_flush_merge(ctx->overlay, fc->view, fc->chunk_buf);

	/* create-once (P2): a fresh uuid every time, never an in-place update. */
	spdk_uuid_generate(&fc->new_uuid);
	s3_data_key(ctx, &fc->new_uuid, key, sizeof(key));

	iov.iov_base = fc->chunk_buf;
	iov.iov_len = fc->put_len;

	rc = s3_put(ctx->client, key, &iov, 1, false, s3_flush_put_done, fc);
	if (rc != 0) {
		s3_flush_finish(fc, rc);
	}
}

/* The base object was not there when the flush went to read it.
 *
 * Same race as on the read path, in the one remaining place that reads a data
 * object: another flush of this chunk published a new object and deleted the one
 * this round had looked up. The round fails and the flusher keeps the chunk dirty
 * and retries, which is already correct -- the retry looks the mapping up again.
 * What was missing is any statement that this is what happened, so a bare 404 in
 * the log could not be told apart from a chunk map naming an absent object.
 *
 * An unchanged mapping is that other thing, and has to be reported as such. */
static void
s3_flush_base_missing(struct s3_flush_ctx *fc)
{
	struct s3_ctx *ctx = fc->ctx;
	char base_str[SPDK_UUID_STRING_LEN];
	char now_str[SPDK_UUID_STRING_LEN];
	struct spdk_uuid now;
	int rc;

	spdk_uuid_fmt_lower(base_str, sizeof(base_str), &fc->base_uuid);

	rc = s3_chunk_map_lookup(ctx->chunk_map, fc->view->chunk_index, &now, NULL);
	if (rc != 0) {
		SPDK_ERRLOG("Chunk %" PRIu64 " base object %s is gone and the map now "
			    "has no entry for the chunk (%s)\n",
			    fc->view->chunk_index, base_str, spdk_strerror(-rc));
		return;
	}

	spdk_uuid_fmt_lower(now_str, sizeof(now_str), &now);

	if (spdk_uuid_compare(&now, &fc->base_uuid) == 0) {
		SPDK_ERRLOG("Chunk %" PRIu64 " maps to %s, which is not in S3, and the "
			    "mapping did not change under the flush. The chunk map "
			    "entry is wrong.\n", fc->view->chunk_index, now_str);
		return;
	}

	SPDK_NOTICELOG("Chunk %" PRIu64 " was overwritten while its base was being "
		       "read (%s -> %s); this flush round will be retried\n",
		       fc->view->chunk_index, base_str, now_str);
}

static void
s3_flush_base_read_done(void *cb_arg, uint64_t bytes_read, int status)
{
	struct s3_flush_ctx *fc = cb_arg;

	if (status != 0) {
		if (status == -ENOENT) {
			s3_flush_base_missing(fc);
		}
		s3_flush_finish(fc, status);
		return;
	}

	/* A short read means the object is smaller than the map claimed. The
	 * buffer is zeroed, which is what reading that range would have given
	 * anyway, so carry on -- but say so, because it means the two disagree. */
	if (bytes_read < fc->base_valid) {
		SPDK_WARNLOG("Chunk %" PRIu64 " base object returned %" PRIu64
			     " bytes, the map said %u\n", fc->view->chunk_index,
			     bytes_read, fc->base_valid);
	}

	s3_flush_put(fc);
}

/* s3_flusher_upload_fn: upload one chunk. */
static void
s3_flush_upload(void *vctx, const struct s3_overlay_flush_view *view,
		s3_flusher_upload_cb cb_fn, void *cb_arg)
{
	struct s3_ctx *ctx = vctx;
	struct s3_flush_ctx *fc;
	struct spdk_uuid uuid;
	uint32_t valid_bytes = 0;
	char key[S3_KEY_MAX];
	int rc;

	assert(ctx->owner_thread == NULL || ctx->owner_thread == spdk_get_thread());

	fc = calloc(1, sizeof(*fc));
	if (!fc) {
		cb_fn(cb_arg, -ENOMEM);
		return;
	}
	fc->ctx = ctx;
	fc->view = view;
	fc->cb_fn = cb_fn;
	fc->cb_arg = cb_arg;

	fc->chunk_buf = calloc(1, ctx->chunk_size);
	if (!fc->chunk_buf) {
		free(fc);
		cb_fn(cb_arg, -ENOMEM);
		return;
	}

	rc = s3_chunk_map_lookup(ctx->chunk_map, view->chunk_index, &uuid,
				 &valid_bytes);
	if (rc == 0) {
		fc->has_base = true;
		fc->base_valid = valid_bytes;
		spdk_uuid_copy(&fc->base_uuid, &uuid);
	} else if (rc != -ENOENT) {
		s3_flush_finish(fc, rc);
		return;
	}

	/* The object may never get shorter: valid_bytes going backwards would
	 * hide data that is already in S3. That was the other half of the 7.3b
	 * defect, where valid_bytes came from whichever write finished last. */
	fc->put_len = spdk_max(view->end_offset, fc->base_valid);

	/* Read the old object only when it still contributes something: a hole
	 * below end_offset, or bytes beyond it. */
	if (fc->has_base &&
	    !(view->covers_prefix && view->end_offset >= fc->base_valid)) {
		ctx->rmw_count++;

		s3_data_key(ctx, &fc->base_uuid, key, sizeof(key));
		rc = s3_get_range(ctx->client, key, 0, fc->base_valid,
				  fc->chunk_buf, s3_flush_base_read_done, fc);
		if (rc != 0) {
			s3_flush_finish(fc, rc);
		}
		return;
	}

	/* Nothing to preserve: either the chunk is new, or this flush covers
	 * everything the object held. calloc already zeroed the rest. */
	s3_flush_put(fc);
}

/* ==========================================================================
 * Creation
 * ========================================================================== */

int
s3_bs_dev_create(const struct s3_lvs_opts *opts,
		 struct spdk_bdev_desc *wal_desc,
		 struct spdk_bdev_desc *cache_desc,
		 struct s3_client *client,
		 uint64_t capacity_bytes,
		 struct spdk_bs_dev **out)
{
	struct s3_ctx *ctx;
	uint32_t chunk_size;
	uint64_t blockcnt;
	int rc;

	/* The direct-to-S3 path has no WAL / cache yet. The parameters are
	 * accepted but unused to keep the interface stable. */
	(void)wal_desc;
	(void)cache_desc;

	if (!opts || !client || !out) {
		return -EINVAL;
	}
	if (capacity_bytes == 0) {
		SPDK_ERRLOG("capacity_bytes must be non-zero\n");
		return -EINVAL;
	}

	chunk_size = opts->chunk_size ? opts->chunk_size : S3LVOL_DEFAULT_CHUNK_SIZE;

	/* chunk_size must be a power of two: the first level of the three-level
	 * mapping uses shifts (s3_lba_to_chunk_index), and a non-power-of-two
	 * would compute wrong indices. */
	if ((chunk_size & (chunk_size - 1)) != 0) {
		SPDK_ERRLOG("chunk_size %u is not a power of two\n", chunk_size);
		return -EINVAL;
	}
	if (chunk_size < S3LVOL_BLOCK_SIZE) {
		SPDK_ERRLOG("chunk_size %u is smaller than block size %u\n",
			    chunk_size, S3LVOL_BLOCK_SIZE);
		return -EINVAL;
	}

	blockcnt = capacity_bytes / S3LVOL_BLOCK_SIZE;
	if (blockcnt == 0) {
		SPDK_ERRLOG("capacity %" PRIu64 " is smaller than one block\n",
			    capacity_bytes);
		return -EINVAL;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}

	ctx->client         = client;
	ctx->chunk_size     = chunk_size;
	ctx->chunk_shift    = (uint32_t)spdk_u32log2(chunk_size);
	ctx->capacity_bytes = capacity_bytes;
	ctx->owner_thread   = spdk_get_thread();

	ctx->ckpt_interval_sec = opts->checkpoint_interval_sec ?
				 opts->checkpoint_interval_sec :
				 S3_CKPT_DEFAULT_INTERVAL_SEC;
	ctx->ckpt_interval_tsc = ctx->ckpt_interval_sec * spdk_get_ticks_hz();

	STAILQ_INIT(&ctx->retry_q);

	ctx->prefix = strdup(opts->lvs_name ? opts->lvs_name : "s3lvol");
	if (!ctx->prefix) {
		free(ctx);
		return -ENOMEM;
	}

	rc = s3_chunk_map_create(blockcnt, S3LVOL_BLOCK_SIZE, chunk_size,
				 &ctx->chunk_map);
	if (rc != 0) {
		free(ctx->prefix);
		free(ctx);
		return rc;
	}

	/* Register an io_device so create_channel can obtain a channel. The ctx
	 * pointer is the io_device handle -- same address as bs_dev, unique. */
	spdk_io_device_register(ctx, s3_bs_channel_create_cb,
				s3_bs_channel_destroy_cb,
				sizeof(struct s3_bs_channel), ctx->prefix);

	ctx->bs_dev.blocklen        = S3LVOL_BLOCK_SIZE;
	ctx->bs_dev.blockcnt        = blockcnt;

	ctx->bs_dev.create_channel  = s3_bs_dev_create_channel;
	ctx->bs_dev.destroy_channel = s3_bs_dev_destroy_channel;
	ctx->bs_dev.destroy         = s3_bs_dev_destroy;
	ctx->bs_dev.read            = s3_bs_dev_read;
	ctx->bs_dev.write           = s3_bs_dev_write;
	ctx->bs_dev.readv           = s3_bs_dev_readv;
	ctx->bs_dev.writev          = s3_bs_dev_writev;
	ctx->bs_dev.readv_ext       = s3_bs_dev_readv_ext;
	ctx->bs_dev.writev_ext      = s3_bs_dev_writev_ext;
	ctx->bs_dev.flush           = s3_bs_dev_flush;
	ctx->bs_dev.write_zeroes    = s3_bs_dev_write_zeroes;
	ctx->bs_dev.unmap           = s3_bs_dev_unmap;
	ctx->bs_dev.is_zeroes       = s3_bs_dev_is_zeroes;
	ctx->bs_dev.is_range_valid  = s3_bs_dev_is_range_valid;
	ctx->bs_dev.translate_lba   = s3_bs_dev_translate_lba;
	ctx->bs_dev.is_degraded     = s3_bs_dev_is_degraded;
	/* copy is deliberately left NULL: see the comment in
	 * s3_bs_dev_translate_lba. */

	SPDK_NOTICELOG("s3_bs_dev created: prefix=%s capacity=%" PRIu64 " bytes "
		       "(%" PRIu64 " blocks x %u), chunk=%u bytes, %" PRIu64 " chunks\n",
		       ctx->prefix, capacity_bytes, blockcnt, S3LVOL_BLOCK_SIZE,
		       chunk_size, s3_chunk_map_get_num_chunks(ctx->chunk_map));

	*out = &ctx->bs_dev;
	return 0;
}

struct s3_ctx *
s3_bs_dev_get_ctx(struct spdk_bs_dev *bs_dev)
{
	return (struct s3_ctx *)bs_dev;
}

/* ==========================================================================
 * WAL attachment
 * ========================================================================== */

int
s3_bs_dev_attach_wal(struct spdk_bs_dev *bs_dev, struct s3_wal *wal,
		     const struct s3_flusher_opts *opts)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;
	int rc;

	if (!ctx || !wal) {
		return -EINVAL;
	}
	if (ctx->wal) {
		return -EEXIST;
	}
	/* The overlay, the flusher and the poller are all owner-thread state. */
	assert(ctx->owner_thread == NULL || ctx->owner_thread == spdk_get_thread());

	rc = s3_overlay_create(bs_dev->blockcnt, S3LVOL_BLOCK_SIZE,
			       ctx->chunk_size, 0, &ctx->overlay);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to create the overlay: %d\n", rc);
		return rc;
	}

	rc = s3_flusher_create(ctx->overlay, wal, s3_flush_upload, ctx, opts,
			       &ctx->flusher);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to start the flusher: %d\n", rc);
		s3_overlay_destroy(ctx->overlay);
		ctx->overlay = NULL;
		return rc;
	}

	ctx->wal = wal;

	SPDK_NOTICELOG("s3_bs_dev '%s' switched to the WAL write path\n",
		       ctx->prefix);

	return 0;
}

int
s3_bs_dev_attach_cache(struct spdk_bs_dev *bs_dev)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;
	const struct s3_region *region;
	struct s3_cache_opts opts = {};
	int rc;

	if (!ctx) {
		return -EINVAL;
	}
	if (ctx->cache) {
		return -EEXIST;
	}
	if (!ctx->local_dev) {
		/* Direct-to-S3 mode, or attach_journal has not run yet. */
		return -ENOTSUP;
	}
	assert(ctx->owner_thread == NULL || ctx->owner_thread == spdk_get_thread());

	region = s3_local_dev_get_region(ctx->local_dev, S3_REGION_CACHE);
	if (!region || !region->valid || region->size < ctx->chunk_size) {
		/* A disk formatted before the region existed, or one whose cache
		 * region is too small to hold a single chunk. Reported distinctly
		 * from an error so the caller can carry on without a cache. */
		SPDK_NOTICELOG("No usable cache region on the local device for "
			       "'%s'; reads will always go to S3\n", ctx->prefix);
		return -ENOTSUP;
	}

	opts.desc          = s3_local_dev_get_desc(ctx->local_dev, S3_REGION_CACHE);
	opts.ch            = s3_local_dev_get_channel(ctx->local_dev,
						      S3_REGION_CACHE);
	opts.region_offset = region->offset;
	opts.region_size   = region->size;
	opts.chunk_size    = ctx->chunk_size;
	opts.block_size    = S3LVOL_BLOCK_SIZE;
	opts.num_chunks    = s3_chunk_map_get_num_chunks(ctx->chunk_map);

	if (!opts.desc || !opts.ch) {
		SPDK_ERRLOG("The local device has a cache region but no descriptor "
			    "or channel for it\n");
		return -ENOTSUP;
	}

	rc = s3_cache_create(&opts, &ctx->cache);
	if (rc != 0) {
		/* Not fatal: an lvstore without a cache is the behaviour that
		 * shipped before there was one. Say so and carry on. */
		SPDK_WARNLOG("Could not create the chunk cache for '%s': %s; "
			     "reads will always go to S3\n",
			     ctx->prefix, spdk_strerror(-rc));
		ctx->cache = NULL;
		return rc;
	}

	return 0;
}

void
s3_bs_dev_set_destroy_cb(struct spdk_bs_dev *bs_dev, s3_bs_dev_cb cb_fn,
			 void *cb_arg)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	if (!ctx) {
		return;
	}

	/* Deliberately overwrites rather than chaining: the create path hands the
	 * slot over to the unload path once the lvstore is published, and only one
	 * of them can be waiting at a time. */
	ctx->destroy_cb = cb_fn;
	ctx->destroy_arg = cb_arg;
}

void
s3_bs_dev_set_reap_cb(struct spdk_bs_dev *bs_dev, s3_bs_dev_reap_cb cb_fn,
		      void *cb_arg)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	if (!ctx) {
		return;
	}

	ctx->reap_cb = cb_fn;
	ctx->reap_arg = cb_arg;
}

void
s3_bs_dev_drain(struct spdk_bs_dev *bs_dev, s3_bs_dev_cb cb_fn, void *cb_arg)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	if (!ctx || !ctx->flusher) {
		/* Direct-to-S3 mode: a completed write is already in S3. */
		if (cb_fn) {
			cb_fn(cb_arg, 0);
		}
		return;
	}

	assert(ctx->owner_thread == NULL || ctx->owner_thread == spdk_get_thread());

	s3_flusher_drain(ctx->flusher, 0, cb_fn, cb_arg);
}

int
s3_bs_dev_wal_apply(struct spdk_bs_dev *bs_dev,
		    const struct s3_wal_entry_hdr *hdr, const void *payload)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	if (!ctx || !hdr) {
		return -EINVAL;
	}
	if (!ctx->overlay) {
		SPDK_ERRLOG("WAL replay without an attached overlay\n");
		return -EINVAL;
	}

	switch (hdr->type) {
	case S3_WAL_WRITE:
		/* Widened to uint64_t: nblocks is uint32_t, so a corrupted
		 * header with nblocks == 2^20 would wrap nblocks * BLOCK_SIZE
		 * (== 2^32) to 0 in uint32_t arithmetic and sneak past the
		 * length check with a zero payload. */
		if (!payload ||
		    hdr->payload_len != (uint64_t)hdr->nblocks * S3LVOL_BLOCK_SIZE) {
			SPDK_ERRLOG("WAL write entry at seq %" PRIu64 " has %u "
				    "payload bytes for %u blocks\n",
				    hdr->seq, hdr->payload_len, hdr->nblocks);
			return -EILSEQ;
		}
		return s3_overlay_write(ctx->overlay, hdr->lba, hdr->nblocks,
					payload, hdr->seq);

	case S3_WAL_WRITE_ZEROES:
		return s3_overlay_write(ctx->overlay, hdr->lba, hdr->nblocks,
					NULL, hdr->seq);

	case S3_WAL_UNMAP:
		/* Replay runs in ascending seq order, so dropping here and having
		 * a later entry rewrite the chunk converges on the right content.
		 *
		 * The mapping is cleared too, without going through the journal:
		 * the journal has already been replayed and may still hold a
		 * mapping this unmap superseded. If a newer write did get flushed
		 * after the unmap, its journal record is undone here but its WAL
		 * entry follows at a higher seq, so the flusher republishes it. */
		s3_overlay_drop_chunk(ctx->overlay, hdr->chunk_index);
		/* lsn 0: this comes from the WAL, which has no journal record
		 * behind it, so it must not move the map's applied LSN. */
		return s3_chunk_map_apply_remove(ctx->chunk_map, hdr->chunk_index,
						 0);

	case S3_WAL_BARRIER:
	case S3_WAL_END:
		return 0;

	default:
		SPDK_WARNLOG("Ignoring unknown WAL entry type %u at seq %" PRIu64
			     "\n", hdr->type, hdr->seq);
		return 0;
	}
}

struct s3_chunk_map *
s3_bs_dev_get_chunk_map(struct spdk_bs_dev *bs_dev)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	return ctx ? ctx->chunk_map : NULL;
}

uint32_t
s3_bs_dev_get_chunk_size(struct spdk_bs_dev *bs_dev)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	return ctx ? (1u << ctx->chunk_shift) : 0;
}

/* ==========================================================================
 * Checkpoints: bounding the journal
 *
 * The journal records increments and only grows. Once it fills, appends fail with
 * -ENOSPC -- deliberately, since overwriting records no snapshot covers would
 * orphan live objects -- and the lvstore effectively goes read-only. A checkpoint
 * snapshots the whole chunk map to S3 so the journal can be truncated.
 *
 * *The three steps must stay in this order:*
 *
 *   1. upload the snapshot
 *   2. update the super block's checkpoint_gen / checkpoint_lsn
 *   3. only then truncate the journal
 *
 * A crash between any two of them can only cause *more* replay, never less:
 * after 1, the super block still names the old LSN, so recovery starts there and
 * the new snapshot is simply ignored; after 2, the journal is intact so recovery
 * starts from the new LSN and finds everything. Doing it the other way round --
 * truncating first -- loses mappings on any crash.
 *
 * Driven from a poller rather than from the append path on purpose. The snapshot
 * has to be serialised in one synchronous stretch together with the LSN it is
 * stamped with, and a poller is a context where no chunk map commit can be
 * half-finished. Triggering from inside a journal completion would sample an LSN
 * whose commit had not run yet.
 * ========================================================================== */

static void
ckpt_finish(struct s3_ctx *ctx, int status)
{
	s3_bs_dev_cb user_cb = ctx->ckpt_user_cb;
	void *user_arg = ctx->ckpt_user_arg;

	if (status == 0) {
		ctx->ckpt_done_count++;
	} else {
		ctx->ckpt_failed_count++;
	}

	/* Cleared before the callback: it may start another checkpoint. */
	ctx->ckpt_user_cb = NULL;
	ctx->ckpt_user_arg = NULL;
	ctx->ckpt_in_flight = false;

	if (user_cb) {
		user_cb(user_arg, status);
	}

	/* Last, because it ends in ctx being freed. A destroy that arrived while this
	 * checkpoint was running parked itself here rather than tearing down around
	 * it -- see s3_bs_dev_teardown(). */
	if (ctx->destroying) {
		s3_bs_dev_teardown(ctx);
	}
}

static void
ckpt_super_updated(void *cb_arg, int status)
{
	struct s3_ctx *ctx = cb_arg;

	if (status != 0) {
		/* The snapshot is in S3 but the super block does not point at it.
		 * *Do not truncate*: recovery will start from the old LSN, so the
		 * journal records between the two are still needed. The upload was
		 * wasted, nothing is lost, and the next round tries again. */
		SPDK_ERRLOG("Checkpoint for '%s': the snapshot uploaded but the "
			    "super block update failed (%d); not truncating the "
			    "journal\n", ctx->prefix, status);
		ckpt_finish(ctx, status);
		return;
	}

	/* Now, and only now, the space is reclaimable. */
	s3_journal_truncate(s3_chunk_map_get_journal(ctx->chunk_map),
			    ctx->ckpt_lsn_pending);
	ctx->ckpt_lsn_done = ctx->ckpt_lsn_pending;

	SPDK_NOTICELOG("Checkpoint gen=%" PRIu64 " for '%s' complete: journal "
		       "truncated to LSN %" PRIu64 "\n",
		       ctx->ckpt_gen, ctx->prefix, ctx->ckpt_lsn_pending);

	ckpt_finish(ctx, 0);
}

static void
ckpt_saved(void *cb_arg, int status)
{
	struct s3_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Checkpoint for '%s' failed to upload: %d\n",
			    ctx->prefix, status);
		ckpt_finish(ctx, status);
		return;
	}

	s3_local_dev_update_checkpoint(ctx->local_dev, ctx->ckpt_gen,
				       ctx->ckpt_lsn_pending,
				       ckpt_super_updated, ctx);
}

/* Start a checkpoint, or explain why not.
 *
 * Two conditions trigger one automatically, and they bound different things:
 *
 *   - the journal is at least S3_CKPT_TRIGGER_PCT full ... bounds *space*, i.e.
 *     stops the region from filling and turning the lvstore read-only;
 *   - the last one was more than ckpt_interval_tsc ago ... bounds *recovery
 *     time*, i.e. stops a slow writer from accumulating days of replay in a
 *     region that is nowhere near full.
 *
 * \c forced skips both but nothing else -- an explicit request still cannot run
 * two at once, and still has nothing to do when no new mapping has been
 * committed.
 *
 * Returns 0 when one was started, -EAGAIN when there is nothing worth doing, or
 * another negative errno when it cannot be done at all. */
static int
ckpt_start(struct s3_ctx *ctx, bool forced)
{
	struct s3_journal *journal;
	const struct s3_super_block *sb;
	uint64_t used, capacity, lsn, now;
	const char *reason;
	bool due_space, due_age;

	if (!ctx->local_dev) {
		/* Direct-to-S3 mode: no journal to bound, nothing to snapshot. */
		return -ENOTSUP;
	}
	/* The poller is unregistered before this can be reached through it, so the
	 * caller left here is an explicit request that raced the unload. Refused for
	 * the same reason the poller was stopped: everything a checkpoint reaches
	 * through ctx is being freed. */
	if (ctx->destroying) {
		return -ESHUTDOWN;
	}
	if (ctx->ckpt_in_flight) {
		return -EBUSY;
	}

	journal = s3_chunk_map_get_journal(ctx->chunk_map);
	if (!journal) {
		return -ENOTSUP;
	}

	capacity = s3_journal_get_capacity_bytes(journal);
	used = s3_journal_get_used_bytes(journal);
	if (capacity == 0) {
		return -ENOTSUP;
	}

	now = spdk_get_ticks();
	due_space = used * 100 >= capacity * S3_CKPT_TRIGGER_PCT;
	due_age = ctx->ckpt_interval_tsc != 0 &&
		  now - ctx->ckpt_last_tsc >= ctx->ckpt_interval_tsc;

	if (!forced && !due_space && !due_age) {
		return -EAGAIN;
	}

	/* Sampled here and handed straight to s3_checkpoint_save(), which
	 * serialises synchronously. *Nothing may yield between these two lines*,
	 * or the snapshot would no longer match the LSN it is stamped with. */
	lsn = s3_chunk_map_get_applied_lsn(ctx->chunk_map);
	if (lsn <= ctx->ckpt_lsn_done) {
		/* Everything in the journal is already covered. Another snapshot
		 * would not move truncation forward, so there is nothing useful to
		 * do -- and if this persists while the region fills, the append path
		 * reports -ENOSPC, which is the honest outcome.
		 *
		 * Checked *after* the triggers rather than before so that this,
		 * and not the interval, is what an idle lvstore hits: the timer is
		 * left expired, so the first write after a quiet spell is
		 * snapshotted promptly instead of waiting out a fresh interval. */
		return -EAGAIN;
	}

	sb = s3_local_dev_get_super(ctx->local_dev);

	ctx->ckpt_in_flight = true;
	ctx->ckpt_lsn_pending = lsn;
	ctx->ckpt_gen++;
	ctx->ckpt_last_tsc = now;

	if (forced) {
		reason = "requested";
	} else if (due_space) {
		/* Reported in preference to the interval when both fired: it is the
		 * one that means the lvstore is close to going read-only. */
		reason = "journal usage";
	} else {
		reason = "interval";
	}

	SPDK_NOTICELOG("Checkpoint gen=%" PRIu64 " for '%s' (%s): journal %" PRIu64
		       "%% full (%" PRIu64 "/%" PRIu64 " KiB), covering LSN %"
		       PRIu64 "\n",
		       ctx->ckpt_gen, ctx->prefix, reason,
		       used * 100 / capacity, used / 1024, capacity / 1024, lsn);

	s3_checkpoint_save(ctx->client, ctx->prefix, &sb->lvs_uuid,
			   ctx->chunk_map, lsn, ctx->ckpt_gen, ckpt_saved, ctx);
	return 0;
}

static int
s3_ckpt_poll(void *arg)
{
	struct s3_ctx *ctx = arg;

	return ckpt_start(ctx, false) == 0 ? SPDK_POLLER_BUSY : SPDK_POLLER_IDLE;
}

void
s3_bs_dev_checkpoint(struct spdk_bs_dev *bs_dev, s3_bs_dev_cb cb_fn, void *cb_arg)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;
	int rc;

	if (!ctx) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	if (ctx->ckpt_in_flight) {
		/* Not queued behind the running one: the caller asked for "a
		 * checkpoint now" and one is being taken, but reporting success
		 * would imply their LSN is covered when it may not be. */
		if (cb_fn) {
			cb_fn(cb_arg, -EBUSY);
		}
		return;
	}

	ctx->ckpt_user_cb = cb_fn;
	ctx->ckpt_user_arg = cb_arg;

	rc = ckpt_start(ctx, true);
	if (rc != 0) {
		ctx->ckpt_user_cb = NULL;
		ctx->ckpt_user_arg = NULL;

		if (rc == -EAGAIN) {
			/* Nothing new to snapshot. Success: the caller's intent
			 * ("make sure the journal can be truncated") is already
			 * satisfied. */
			SPDK_NOTICELOG("Checkpoint for '%s' skipped: nothing has "
				       "changed since LSN %" PRIu64 "\n",
				       ctx->prefix, ctx->ckpt_lsn_done);
			rc = 0;
		}
		if (cb_fn) {
			cb_fn(cb_arg, rc);
		}
	}
}

int
s3_bs_dev_attach_journal(struct spdk_bs_dev *bs_dev, struct s3_journal *journal,
			 struct s3_local_dev *local_dev)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	if (!ctx) {
		return -EINVAL;
	}

	/* The chunk map lives inside this module, so the journal has to be handed
	 * in through here rather than wired up by the caller. Without it the map
	 * is memory-only and the lvstore cannot be re-attached after a restart. */
	s3_chunk_map_set_journal(ctx->chunk_map, journal);

	ctx->local_dev = local_dev;

	/* Resume the counter from what the disk already records, so gen stays
	 * monotonic across restarts and a stale snapshot is recognisable. */
	if (local_dev) {
		const struct s3_super_block *sb = s3_local_dev_get_super(local_dev);

		ctx->ckpt_gen = sb->checkpoint_gen;
		ctx->ckpt_lsn_done = sb->checkpoint_lsn;

		/* The interval runs from here, not from the epoch: an lvstore that
		 * has just been attached should not snapshot in its first second
		 * merely because the timer looks expired. Replay has already put the
		 * recovered mappings in the map, so an epoch start would fire
		 * immediately on every single attach. */
		ctx->ckpt_last_tsc = spdk_get_ticks();

		if (!ctx->ckpt_poller) {
			ctx->ckpt_poller = SPDK_POLLER_REGISTER(s3_ckpt_poll, ctx,
								S3_CKPT_POLL_US);
			if (!ctx->ckpt_poller) {
				/* Without it the journal is never truncated and the
				 * lvstore becomes read-only once the region fills.
				 * That is a real failure, not a degradation. */
				SPDK_ERRLOG("Failed to start the checkpoint poller "
					    "for '%s'\n", ctx->prefix);
				return -ENOMEM;
			}

			SPDK_NOTICELOG("Checkpoint poller for '%s': every %u s, or "
				       "when the journal passes %u%% full\n",
				       ctx->prefix, ctx->ckpt_interval_sec,
				       S3_CKPT_TRIGGER_PCT);
		}
	}

	return 0;
}

int
s3_bs_dev_journal_apply(struct spdk_bs_dev *bs_dev,
			const struct s3_journal_record *rec)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	if (!ctx || !rec) {
		return -EINVAL;
	}

	switch (rec->op) {
	case S3_JOURNAL_OP_CHUNK_UPDATE:
		return s3_chunk_map_apply_update(ctx->chunk_map, rec->chunk_index,
						 &rec->uuid, rec->valid_bytes,
						 rec->gen, rec->flags, rec->lsn);
	case S3_JOURNAL_OP_CHUNK_REMOVE:
		return s3_chunk_map_apply_remove(ctx->chunk_map, rec->chunk_index,
						 rec->lsn);
	default:
		/* Checkpoint markers and anything a newer build added: the record
		 * is well formed, it just carries nothing for the chunk map. */
		return 0;
	}
}

void
s3_bs_dev_get_stats(struct spdk_bs_dev *bs_dev, struct s3_bs_dev_stats *out)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;
	struct s3_flusher_stats fstats = {};

	if (!ctx || !out) {
		return;
	}

	memset(out, 0, sizeof(*out));

	out->wal_attached = (ctx->wal != NULL);
	out->rmw_count        = ctx->rmw_count;
	out->zero_fill_count  = ctx->zero_fill_count;
	out->wal_writes       = ctx->wal_writes;
	out->wal_retries      = ctx->wal_retries;
	out->overlay_hits     = ctx->overlay_hits;
	out->retry_queued     = ctx->retry_count;
	out->allocated_chunks = s3_chunk_map_get_allocated(ctx->chunk_map);

	out->ckpt_done   = ctx->ckpt_done_count;
	out->ckpt_failed = ctx->ckpt_failed_count;
	out->ckpt_lsn    = ctx->ckpt_lsn_done;
	out->ckpt_gen    = ctx->ckpt_gen;
	out->ckpt_interval_sec = ctx->ckpt_interval_sec;
	{
		struct s3_journal *j = s3_chunk_map_get_journal(ctx->chunk_map);

		out->journal_used_bytes = s3_journal_get_used_bytes(j);
		out->journal_capacity_bytes = s3_journal_get_capacity_bytes(j);
	}

	out->overlay_bytes       = s3_overlay_get_bytes(ctx->overlay);
	out->overlay_live_chunks = s3_overlay_get_live_chunks(ctx->overlay);
	{
		struct s3_overlay_stats ostats = {};

		s3_overlay_get_stats(ctx->overlay, &ostats);
		out->overlay_flushed_full   = ostats.flushed_full;
		out->overlay_flushed_aged   = ostats.flushed_aged;
		out->overlay_flushed_forced = ostats.flushed_forced;
	}

	if (ctx->flusher) {
		s3_flusher_get_stats(ctx->flusher, &fstats);
		out->chunks_flushed  = fstats.chunks_flushed;
		out->flush_failures  = fstats.failures;
		out->flush_in_flight = fstats.in_flight;
	}

	if (ctx->cache) {
		struct s3_cache_stats cstats = {};

		s3_cache_get_stats(ctx->cache, &cstats);
		out->cache_attached           = true;
		out->cache_hits               = cstats.hits;
		out->cache_misses             = cstats.misses;
		out->cache_hits_declined      = cstats.hits_declined;
		out->cache_populates          = cstats.populates;
		out->cache_populates_dropped  = cstats.populates_dropped;
		out->cache_evictions          = cstats.evictions;
		out->cache_bytes_served       = cstats.bytes_served;
		out->cache_bytes_populated    = cstats.bytes_populated;
		out->cache_slots_total        = cstats.slots_total;
		out->cache_slots_resident     = cstats.slots_resident;
		out->cache_bytes_resident     = cstats.bytes_resident;
	}
}

void
s3_bs_dev_kick_flusher(struct spdk_bs_dev *bs_dev)
{
	struct s3_ctx *ctx = (struct s3_ctx *)bs_dev;

	if (ctx && ctx->flusher) {
		s3_flusher_kick(ctx->flusher);
	}
}
