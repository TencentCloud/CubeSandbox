/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_wal -- write-ahead log on the local bdev 
 *
 *   Design rationale, the disk format and the durability contract are in
 *   include/s3lvol/s3_wal.h. Things to keep in mind while reading this file:
 *
 *   1. *One batch in flight at a time.* Not throttling -- it keeps the log
 *      physically append-ordered, which is what lets recovery scan linearly
 *      instead of sorting by seq. It also means the batch buffer is only ever
 *      touched between batches, so it needs no double buffering.
 *   2. *A batch is durable only after write plus flush.* The two are treated as
 *      one commit unit and callers are acknowledged from the flush completion,
 *      never from the write completion. On aio the flush is fsync(); on nvme it
 *      is a Flush command.
 *   3. *seq is assigned when an op is queued*, so queue order equals seq order.
 *      Same reasoning as the journal.
 *   4. Batch atomicity comes from S3_WAL_F_LAST_IN_BATCH plus the two CRCs.
 *      Recovery accepts a batch only when it is closed and fully verified, so a
 *      torn tail is discarded whole rather than half-applied (W4).
 */

#include "spdk/stdinc.h"
#include "spdk/bdev.h"
#include "spdk/crc32.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/thread.h"
#include "spdk/util.h"

#include "s3lvol/s3_wal.h"

enum s3_wal_state {
	S3_WAL_RUNNING,
	S3_WAL_BACKPRESSURE,
	S3_WAL_CLOSING,
	S3_WAL_FAILED,
};

/* One queued append. The payload is copied into the batch buffer when the batch
 * is assembled, so the caller's buffer does not have to outlive this call. */
struct s3_wal_op {
	struct s3_wal_entry_hdr      hdr;

	/* Copy of the caller's data, or NULL for metadata-only entries. Owned
	 * here because callers are explicitly not required to keep theirs. */
	void                        *payload;
	uint32_t                     payload_len;

	s3_wal_cb                    cb_fn;
	void                        *cb_arg;

	STAILQ_ENTRY(s3_wal_op)      link;
};

struct s3_wal {
	struct s3_local_dev         *local_dev;
	struct spdk_bdev_desc       *desc;
	struct spdk_io_channel      *ch;

	/* Absolute byte offset of the WAL region on the bdev. */
	uint64_t     region_offset;
	uint64_t                     region_size;

	/* Absolute offset of segment 0, i.e. just past the A/B super slots. */
	uint64_t                     ring_offset;

	uint32_t                     seg_size;
	uint32_t                     seg_count;

	/* Highest seq written into each segment, indexed by segment number.
	 *
	 * Truncation needs it: the flusher reports "everything up to seq X is in
	 * S3", and a segment may only be released when *all* of its entries are
	 * below that. Without this the only safe answer is "release nothing",
	 * which is what the first version did.
	 *
	 * Not persisted. After a crash the ring is rescanned from ckpt_head
	 * anyway, and the scan rebuilds it. */
	uint64_t *seg_max_seq;

	/* Ring positions relative to ring_offset. */
	uint64_t                     head;
	uint64_t                     tail;

	uint64_t                     seq_next;
	uint64_t                     epoch;

	uint64_t                     ckpt_head;
	uint64_t                     ckpt_seq;

	uint64_t                     batch_id_next;

	/* Which super slot to write next, and the generation stamped on it. */
	uint32_t                     super_slot;
	uint64_t                     slot_gen;

	/* Queued ops, and the ops carried by the batch currently in flight. */
	STAILQ_HEAD(, s3_wal_op)     pending;
	STAILQ_HEAD(, s3_wal_op)     writing;

	uint32_t                     pending_ops;
	uint64_t                     pending_bytes;
	uint64_t                     pending_since_tsc;

	/* 4 KiB aligned, DMA safe, allocated once (I5). */
	void                        *batch_buf;

	/* Length of the batch in flight, needed to advance head on completion. */
	uint64_t                     batch_len;

	/* Highest seq carried by the batch in flight, recorded into
	 * seg_max_seq[] once it is durable. */
	uint64_t                     batch_max_seq;

	bool                         batch_in_flight;

	/* Drives the batch age trigger and nothing else. */
	struct spdk_poller          *batch_poller;

	/* Scratch for super reads and writes. */
	void                        *super_buf;

	enum s3_wal_state            state;

	/* Set while formatting or replaying: appends are refused. */
	bool                         busy;

	struct s3_wal_stats          stats;
};

/* ==========================================================================
 * Geometry helpers
 * ========================================================================== */

/* Usable ring capacity. The trailing slack is excluded so that a batch never
 * has to straddle the end of the region. */
static uint64_t
wal_capacity(const struct s3_wal *wal)
{
	return (uint64_t)wal->seg_size * wal->seg_count;
}

static uint64_t
wal_used(const struct s3_wal *wal)
{
	/* head only ever moves forward and tail follows it, so the difference is
	 * the outstanding byte count even though both wrap. */
	return wal->head - wal->tail;
}

/* Absolute bdev offset for a logical ring position. */
static uint64_t
wal_disk_offset(const struct s3_wal *wal, uint64_t ring_pos)
{
	return wal->ring_offset + (ring_pos % wal_capacity(wal));
}

static uint64_t
wal_seg_of(const struct s3_wal *wal, uint64_t ring_pos)
{
	return (ring_pos % wal_capacity(wal)) / wal->seg_size;
}

/* Bytes left in the segment containing ring_pos. */
static uint64_t
wal_seg_remaining(const struct s3_wal *wal, uint64_t ring_pos)
{
	uint64_t off_in_seg = (ring_pos % wal_capacity(wal)) % wal->seg_size;

	return wal->seg_size - off_in_seg;
}

/* seq packs the epoch so a stale log cannot be mistaken for a current one. */
static uint64_t
wal_make_seq(const struct s3_wal *wal, uint64_t local)
{
	return (wal->epoch << 48) | (local & 0xFFFFFFFFFFFFULL);
}

/* Remember the highest seq landing in the segment that holds ring_pos.
 *
 * A batch never straddles a segment boundary (wal_kick() closes the segment out
 * first), so one call per batch is enough. */
static void
wal_record_seg_seq(struct s3_wal *wal, uint64_t ring_pos, uint64_t seq)
{
	uint64_t seg;

	if (!wal->seg_max_seq || seq == 0) {
		return;
	}

	seg = wal_seg_of(wal, ring_pos);
	if (seq > wal->seg_max_seq[seg]) {
		wal->seg_max_seq[seg] = seq;
	}
}

static uint32_t
wal_hdr_crc(const struct s3_wal_entry_hdr *hdr)
{
	struct s3_wal_entry_hdr tmp = *hdr;

	tmp.crc_hdr = 0;

	return spdk_crc32c_update(&tmp, sizeof(tmp), ~0u);
}

static uint32_t
wal_super_crc(const struct s3_wal_super *sb)
{
	struct s3_wal_super tmp = *sb;

	tmp.crc = 0;

	return spdk_crc32c_update(&tmp, sizeof(tmp), ~0u);
}

/* ==========================================================================
 * Backpressure (W5)
 * ========================================================================== */

static void
wal_update_state(struct s3_wal *wal)
{
	uint64_t cap = wal_capacity(wal);
	uint64_t used = wal_used(wal);

	if (wal->state == S3_WAL_CLOSING || wal->state == S3_WAL_FAILED) {
		return;
	}

	if (wal->state == S3_WAL_RUNNING) {
		if (used * 100 >= cap * S3_WAL_BACKPRESSURE_ON_PCT) {
			wal->state = S3_WAL_BACKPRESSURE;
			wal->stats.backpressure_events++;
			SPDK_WARNLOG("WAL entering backpressure: %" PRIu64 "/%" PRIu64
				     " bytes used\n", used, cap);
		}
		return;
	}

	/* Hysteresis on the way out, so a log hovering at the threshold does not
	 * flap between states on every append. */
	if (used * 100 <= cap * S3_WAL_BACKPRESSURE_OFF_PCT) {
		wal->state = S3_WAL_RUNNING;
		SPDK_NOTICELOG("WAL leaving backpressure: %" PRIu64 "/%" PRIu64
			       " bytes used\n", used, cap);
	}
}

bool
s3_wal_is_backpressured(const struct s3_wal *wal)
{
	return wal && wal->state == S3_WAL_BACKPRESSURE;
}

/* ==========================================================================
 * Batch assembly and submission
 * ========================================================================== */

static void wal_kick(struct s3_wal *wal);

static void
wal_complete_ops(struct s3_wal_op *first, int status)
{
	struct s3_wal_op *op = first, *next;

	while (op) {
		next = STAILQ_NEXT(op, link);

		if (op->cb_fn) {
			op->cb_fn(op->cb_arg, status);
		}
		if (op->payload) {
			free(op->payload);
		}
		free(op);

		op = next;
	}
}

static void
wal_fail_pending(struct s3_wal *wal, int status)
{
	struct s3_wal_op *first = STAILQ_FIRST(&wal->pending);

	STAILQ_INIT(&wal->pending);
	wal->pending_ops = 0;
	wal->pending_bytes = 0;

	wal_complete_ops(first, status);
}

/* The flush completed, so the batch is durable: acknowledge its ops. */
static void
wal_flush_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct s3_wal *wal = cb_arg;
	struct s3_wal_op *batch;
	int status = success ? 0 : -EIO;

	spdk_bdev_free_io(bdev_io);

	batch = STAILQ_FIRST(&wal->writing);
	STAILQ_INIT(&wal->writing);

	wal->batch_in_flight = false;

	if (status == 0) {
		/* head advances only now. A batch that failed leaves head where
		 * it was so the space is reused rather than leaving a hole that
		 * recovery would stop at. */
		wal_record_seg_seq(wal, wal->head, wal->batch_max_seq);
		wal->head += wal->batch_len;
		wal->stats.batches++;
		wal->stats.bytes_written += wal->batch_len;
	} else {
		SPDK_ERRLOG("WAL batch flush failed: %d\n", status);
		wal->state = S3_WAL_FAILED;
	}

	wal->batch_len = 0;

	wal_update_state(wal);
	wal_complete_ops(batch, status);

	wal_kick(wal);
}

/* The data reached the device but is not necessarily durable yet, so flush
 * before acknowledging anyone. */
static void
wal_write_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct s3_wal *wal = cb_arg;
	struct s3_wal_op *batch;
	int rc;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		batch = STAILQ_FIRST(&wal->writing);
		STAILQ_INIT(&wal->writing);

		wal->batch_in_flight = false;
		wal->batch_len = 0;
		wal->state = S3_WAL_FAILED;

		SPDK_ERRLOG("WAL batch write failed\n");
		wal_complete_ops(batch, -EIO);
		wal_kick(wal);
		return;
	}

	/* write + flush is one commit unit: nobody is acknowledged in between. */
	rc = spdk_bdev_flush(wal->desc, wal->ch, 0, 0, wal_flush_done, wal);
	if (rc != 0) {
		batch = STAILQ_FIRST(&wal->writing);
		STAILQ_INIT(&wal->writing);

		wal->batch_in_flight = false;
		wal->batch_len = 0;

		SPDK_ERRLOG("Failed to submit WAL flush: %d\n", rc);
		wal_complete_ops(batch, rc);
		wal_kick(wal);
	}
}

/* Write an END sentinel covering the rest of the current segment.
 *
 * Used when the next op does not fit: rather than splitting an entry across the
 * segment boundary, the remainder is closed off and the op starts the next
 * segment. Recovery treats END as "jump to the next segment". */
static uint64_t
wal_emit_end_sentinel(struct s3_wal *wal, uint64_t remaining)
{
	struct s3_wal_entry_hdr *hdr = wal->batch_buf;

	assert(remaining >= sizeof(*hdr));
	assert(remaining <= S3_WAL_BATCH_BUF_SIZE);

	memset(wal->batch_buf, 0, remaining);

	hdr->magic       = S3_WAL_ENTRY_MAGIC;
	hdr->type        = S3_WAL_END;
	hdr->flags       = S3_WAL_F_LAST_IN_BATCH;
	hdr->hdr_len     = (uint16_t)sizeof(*hdr);
	hdr->seq         = wal_make_seq(wal, wal->seq_next++);
	hdr->batch_id    = wal->batch_id_next++;
	hdr->payload_len = 0;
	hdr->crc_hdr     = wal_hdr_crc(hdr);

	wal->batch_max_seq = hdr->seq;

	return remaining;
}

/* Would writing \c len bytes at head run into the region the flusher has not
 * released yet?
 *
 * Backpressure (85%) normally keeps this from ever being true, but it only gates
 * *new* appends: ops already queued still have to be written. Refusing here is
 * the last line of defence -- overwriting live entries loses acknowledged
 * writes, which is unrecoverable, whereas failing the batch merely fails I/O
 * that has not been acknowledged. */
static bool
wal_would_overrun(const struct s3_wal *wal, uint64_t len)
{
	return wal_used(wal) + len > wal_capacity(wal);
}

/* Assemble as many pending ops as fit and submit one batch.
 *
 * Idempotent: returns immediately when a batch is in flight, when busy, or when
 * nothing is queued. */
static void
wal_kick(struct s3_wal *wal)
{
	uint64_t seg_left;
	uint64_t used = 0;
	uint8_t *cursor;
	struct s3_wal_op *op;
	int rc;

	if (wal->batch_in_flight || wal->busy) {
		return;
	}
	if (wal->state == S3_WAL_FAILED) {
		wal_fail_pending(wal, -EIO);
		return;
	}
	if (STAILQ_EMPTY(&wal->pending)) {
		return;
	}

	seg_left = wal_seg_remaining(wal, wal->head);

	/* Does the first op even fit in what is left of this segment? If not,
	 * close the segment out and let the next kick start the new one. */
	op = STAILQ_FIRST(&wal->pending);
	if (sizeof(op->hdr) + op->payload_len > seg_left) {
		if (seg_left < sizeof(struct s3_wal_entry_hdr)) {
			/* Not even room for a sentinel. Skip the remainder;
			 * recovery stops on the unreadable bytes and moves on
			 * via the segment jump. */
			wal->head += seg_left;
			wal_kick(wal);
			return;
		}
		if (wal_would_overrun(wal, seg_left)) {
			SPDK_ERRLOG("WAL is full: cannot close the segment "
				    "(%" PRIu64 "/%" PRIu64 " bytes used); the "
				    "flusher is not keeping up\n",
				    wal_used(wal), wal_capacity(wal));
			wal_fail_pending(wal, -ENOSPC);
			return;
		}
		used = wal_emit_end_sentinel(wal, seg_left);
		wal->batch_len = used;
		wal->batch_in_flight = true;

		/* No user ops ride along, so writing stays empty and the flush
		 * completion simply advances head. */
		rc = spdk_bdev_write(wal->desc, wal->ch, wal->batch_buf,
				     wal_disk_offset(wal, wal->head), used,
				     wal_write_done, wal);
		if (rc != 0) {
			wal->batch_in_flight = false;
			wal->batch_len = 0;
			SPDK_ERRLOG("Failed to submit WAL segment close: %d\n", rc);
		}
		return;
	}

	cursor = wal->batch_buf;
	wal->batch_max_seq = 0;

	while ((op = STAILQ_FIRST(&wal->pending)) != NULL) {
		uint64_t need = sizeof(op->hdr) + op->payload_len;

		/* Stop at the batch buffer limit, the segment boundary, or the
		 * op-count trigger. Whatever is left stays queued for the next
		 * batch.
		 *
		 * The limit is checked against the *padded* length: the batch is
		 * rounded up to a block before being written (I4), so comparing
		 * the unpadded length would let the padding run off the end of
		 * the buffer. */
		if (SPDK_ALIGN_CEIL(used + need, S3_WAL_BLOCK_SIZE) >
		    S3_WAL_BATCH_BUF_SIZE ||
		    used + need > seg_left) {
			break;
		}

		STAILQ_REMOVE_HEAD(&wal->pending, link);
		wal->pending_ops--;
		wal->pending_bytes -= op->payload_len;

		op->hdr.crc_payload = op->payload_len ?
			spdk_crc32c_update(op->payload, op->payload_len, ~0u) : 0;
		op->hdr.flags &= (uint8_t)~S3_WAL_F_LAST_IN_BATCH;
		op->hdr.crc_hdr = wal_hdr_crc(&op->hdr);

		memcpy(cursor, &op->hdr, sizeof(op->hdr));
		if (op->payload_len) {
			memcpy(cursor + sizeof(op->hdr), op->payload, op->payload_len);
		}

		cursor += need;
		used += need;

		if (op->hdr.seq > wal->batch_max_seq) {
			wal->batch_max_seq = op->hdr.seq;
		}

		STAILQ_INSERT_TAIL(&wal->writing, op, link);
	}

	if (used == 0) {
		/* Nothing fit, which can only mean a single op larger than the
		 * batch buffer got queued. Rejected at submit time, so treat it
		 * as a bug rather than silently stalling. */
		SPDK_ERRLOG("WAL batch assembly made no progress; op too large\n");
		wal_fail_pending(wal, -EINVAL);
		return;
	}

	/* Mark the final entry and recompute its header CRC. Recovery uses this
	 * flag to tell a complete batch from a torn tail (W4). */
	struct s3_wal_op *last = NULL, *iter;

	STAILQ_FOREACH(iter, &wal->writing, link) {
		last = iter;
	}
	assert(last != NULL);

	last->hdr.flags |= S3_WAL_F_LAST_IN_BATCH;
	last->hdr.crc_hdr = wal_hdr_crc(&last->hdr);

	/* The copy already in the buffer has the old flags, so patch it
	 * in place rather than reassembling. */
	memcpy(cursor - (sizeof(last->hdr) + last->payload_len),
		   &last->hdr, sizeof(last->hdr));

	/* Pad up to a 4 KiB boundary (I4). The tail is zeroed so recovery reads
	 * a zero magic and stops there. */
	uint64_t padded = SPDK_ALIGN_CEIL(used, S3_WAL_BLOCK_SIZE);

	if (padded > used) {
		memset((uint8_t *)wal->batch_buf + used, 0, padded - used);
	}
	used = padded;

	wal->batch_len = used;
	wal->batch_in_flight = true;

	if (wal_would_overrun(wal, used)) {
		struct s3_wal_op *batch = STAILQ_FIRST(&wal->writing);

		STAILQ_INIT(&wal->writing);
		wal->batch_in_flight = false;
		wal->batch_len = 0;

		SPDK_ERRLOG("WAL is full: %" PRIu64 "/%" PRIu64 " bytes used, "
			    "batch of %" PRIu64 " bytes rejected; the flusher is "
			    "not keeping up\n", wal_used(wal), wal_capacity(wal), used);
		wal_complete_ops(batch, -ENOSPC);
		return;
	}

	rc = spdk_bdev_write(wal->desc, wal->ch, wal->batch_buf,
			     wal_disk_offset(wal, wal->head), used,
			     wal_write_done, wal);
	if (rc != 0) {
		struct s3_wal_op *batch = STAILQ_FIRST(&wal->writing);

		STAILQ_INIT(&wal->writing);
		wal->batch_in_flight = false;
		wal->batch_len = 0;

		SPDK_ERRLOG("Failed to submit WAL batch write: %d\n", rc);
		wal_complete_ops(batch, rc);
	}
}

/* Fires the age trigger. Registered once; it does nothing unless ops have been
 * waiting longer than the target. */
static int
wal_batch_poller(void *arg)
{
	struct s3_wal *wal = arg;
	uint64_t age_us;

	if (wal->batch_in_flight || wal->busy || STAILQ_EMPTY(&wal->pending)) {
		return SPDK_POLLER_IDLE;
	}

	age_us = (spdk_get_ticks() - wal->pending_since_tsc) * 1000000 /
		 spdk_get_ticks_hz();

	if (age_us < S3_WAL_BATCH_MAX_US) {
		return SPDK_POLLER_IDLE;
	}

	wal_kick(wal);
	return SPDK_POLLER_BUSY;
}

/* Queue an op and trigger a batch when any threshold is met. */
static void
wal_submit(struct s3_wal *wal, enum s3_wal_entry_type type, uint64_t lba,
	   uint32_t nblocks, const void *payload, uint32_t payload_len,
	   uint64_t chunk_index, uint64_t *out_seq, s3_wal_cb cb_fn, void *cb_arg)
{
	struct s3_wal_op *op;

	if (wal->busy) {
		SPDK_ERRLOG("WAL is busy (formatting or replaying); append rejected\n");
		if (cb_fn) {
			cb_fn(cb_arg, -EBUSY);
		}
		return;
	}
	if (wal->state == S3_WAL_FAILED) {
		if (cb_fn) {
			cb_fn(cb_arg, -EIO);
		}
		return;
	}
	if (wal->state == S3_WAL_CLOSING) {
		if (cb_fn) {
			cb_fn(cb_arg, -ESHUTDOWN);
		}
		return;
	}
	/* Backpressure is a retry signal, not a failure: blobstore will queue
	 * and resubmit (W5). */
	if (wal->state == S3_WAL_BACKPRESSURE) {
		if (cb_fn) {
			cb_fn(cb_arg, -EAGAIN);
		}
		return;
	}
	if (payload_len > S3_WAL_MAX_PAYLOAD) {
		SPDK_ERRLOG("WAL op too large: %u bytes of payload, limit is %d "
			    "(the caller has to split)\n",
			    payload_len, S3_WAL_MAX_PAYLOAD);
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	op = calloc(1, sizeof(*op));
	if (!op) {
		if (cb_fn) {
			cb_fn(cb_arg, -ENOMEM);
		}
		return;
	}

	if (payload_len) {
		/* Copied because the caller is explicitly allowed to reuse its
		 * buffer as soon as this returns. */
		op->payload = malloc(payload_len);
		if (!op->payload) {
			free(op);
			if (cb_fn) {
				cb_fn(cb_arg, -ENOMEM);
			}
			return;
		}
		memcpy(op->payload, payload, payload_len);
	}
	op->payload_len = payload_len;
	op->cb_fn       = cb_fn;
	op->cb_arg      = cb_arg;

	op->hdr.magic       = S3_WAL_ENTRY_MAGIC;
	op->hdr.type        = (uint8_t)type;
	op->hdr.hdr_len     = (uint16_t)sizeof(op->hdr);
	op->hdr.seq         = wal_make_seq(wal, wal->seq_next++);
	op->hdr.lba         = lba;
	op->hdr.nblocks     = nblocks;
	op->hdr.payload_len = payload_len;
	op->hdr.chunk_index = chunk_index;

	if (out_seq) {
		*out_seq = op->hdr.seq;
	}

	if (STAILQ_EMPTY(&wal->pending)) {
		wal->pending_since_tsc = spdk_get_ticks();
	}
	STAILQ_INSERT_TAIL(&wal->pending, op, link);
	wal->pending_ops++;
	wal->pending_bytes += payload_len;
	wal->stats.appends++;

	if (wal->pending_bytes >= S3_WAL_BATCH_TARGET_BYTES ||
	    wal->pending_ops >= S3_WAL_BATCH_MAX_OPS) {
		wal_kick(wal);
	}
}

void
s3_wal_append_write(struct s3_wal *wal, uint64_t lba, uint32_t nblocks,
		    const void *payload, uint64_t chunk_index,
		    uint64_t *out_seq, s3_wal_cb cb_fn, void *cb_arg)
{
	if (!wal || !payload || nblocks == 0) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	wal_submit(wal, S3_WAL_WRITE, lba, nblocks, payload,
		   nblocks * S3_WAL_BLOCK_SIZE, chunk_index, out_seq,
		   cb_fn, cb_arg);
}

void
s3_wal_append_zeroes(struct s3_wal *wal, uint64_t lba, uint32_t nblocks,
		     uint64_t chunk_index, uint64_t *out_seq,
		     s3_wal_cb cb_fn, void *cb_arg)
{
	if (!wal || nblocks == 0) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	wal_submit(wal, S3_WAL_WRITE_ZEROES, lba, nblocks, NULL, 0, chunk_index,
		   out_seq, cb_fn, cb_arg);
}

void
s3_wal_append_unmap(struct s3_wal *wal, uint64_t lba, uint32_t nblocks,
		    uint64_t chunk_index, uint64_t *out_seq,
		    s3_wal_cb cb_fn, void *cb_arg)
{
	if (!wal || nblocks == 0) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	wal_submit(wal, S3_WAL_UNMAP, lba, nblocks, NULL, 0, chunk_index,
		   out_seq, cb_fn, cb_arg);
}

void
s3_wal_append_barrier(struct s3_wal *wal, uint64_t *out_seq,
		      s3_wal_cb cb_fn, void *cb_arg)
{
	if (!wal) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	wal_submit(wal, S3_WAL_BARRIER, 0, 0, NULL, 0, 0, out_seq, cb_fn, cb_arg);

	/* A barrier is latency sensitive and tiny, so do not make it wait for
	 * the size trigger. */
	wal_kick(wal);
}

/* ==========================================================================
 * Super block
 * ========================================================================== */

static void
wal_fill_super(const struct s3_wal *wal, struct s3_wal_super *sb, uint64_t ckpt_seq)
{
	memset(sb, 0, sizeof(*sb));

	sb->magic     = S3_WAL_SUPER_MAGIC;
	sb->version   = S3_WAL_SUPER_VERSION;
	sb->seg_size  = wal->seg_size;
	sb->seg_count = wal->seg_count;
	sb->epoch     = wal->epoch;
	sb->head      = wal->head;
	sb->tail      = wal->tail;
	sb->ckpt_head = wal->ckpt_head;
	sb->ckpt_seq  = ckpt_seq;
	sb->seq_next  = wal->seq_next;
	sb->slot_gen  = wal->slot_gen;
	sb->crc       = wal_super_crc(sb);
}

struct wal_super_ctx {
	struct s3_wal *wal;
	s3_wal_cb      cb_fn;
	void          *cb_arg;
};

static void
wal_super_written(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct wal_super_ctx *ctx = cb_arg;
	s3_wal_cb cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;

	spdk_bdev_free_io(bdev_io);
	free(ctx);

	if (!success) {
		SPDK_ERRLOG("Failed to persist the WAL super block\n");
	}

	if (cb_fn) {
		cb_fn(user_arg, success ? 0 : -EIO);
	}
}

void
s3_wal_sync_super(struct s3_wal *wal, uint64_t ckpt_seq, s3_wal_cb cb_fn,
		  void *cb_arg)
{
	struct wal_super_ctx *ctx;
	struct s3_wal_super sb;
	uint64_t off;
	int rc;

	if (!wal) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		if (cb_fn) {
			cb_fn(cb_arg, -ENOMEM);
		}
		return;
	}
	ctx->wal    = wal;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;

	/* Alternate slots so a crash mid-update still leaves the previous copy
	 * intact. slot_gen is what identifies the newer one on open. */
	wal->slot_gen++;
	wal->ckpt_seq = ckpt_seq;
	wal_fill_super(wal, &sb, ckpt_seq);

	off = wal->region_offset + wal->super_slot * S3_WAL_SUPER_SLOT_SIZE;
	wal->super_slot ^= 1;

	memset(wal->super_buf, 0, S3_WAL_SUPER_SLOT_SIZE);
	memcpy(wal->super_buf, &sb, sizeof(sb));

	rc = spdk_bdev_write(wal->desc, wal->ch, wal->super_buf, off,
			     S3_WAL_SUPER_SLOT_SIZE, wal_super_written, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit WAL super write: %d\n", rc);
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, rc);
		}
	}
}

/* ==========================================================================
 * Allocation, create and open
 * ========================================================================== */

/* Derive and validate the ring geometry.
 *
 * Kept separate from wal_alloc() because of a chicken-and-egg problem: on open
 * the real seg_size lives in the super block, which cannot be read until the
 * descriptor and a buffer exist. Deriving geometry from the default first and
 * fixing it up later does not work either, because the default may not even fit
 * the region -- which is exactly how this was found. A 16 MiB WAL region holding
 * a log formatted with 2 MiB segments failed to open, because open assumed the
 * 16 MiB default and concluded the region could not hold three segments. */
static int
wal_setup_geometry(struct s3_wal *wal, uint32_t seg_size)
{
	uint64_t ring_bytes;

	if (seg_size == 0) {
		seg_size = S3_WAL_DEFAULT_SEG_SIZE;
	}
	/* I2. Without this every derived offset would drift out of alignment. */
	if (seg_size % S3_WAL_BLOCK_SIZE != 0) {
		SPDK_ERRLOG("WAL seg_size %u is not a multiple of %d\n",
			    seg_size, S3_WAL_BLOCK_SIZE);
		return -EINVAL;
	}
	if (seg_size < S3_WAL_BATCH_BUF_SIZE) {
		SPDK_ERRLOG("WAL seg_size %u is smaller than the batch buffer %d\n",
			    seg_size, S3_WAL_BATCH_BUF_SIZE);
		return -EINVAL;
	}

	ring_bytes = wal->region_size - S3_WAL_SUPER_SIZE;

	/* Reserve two segments of slack so a batch never straddles the end. */
	if (ring_bytes < (uint64_t)seg_size * 3) {
		SPDK_ERRLOG("WAL region %" PRIu64 " bytes cannot hold 3 segments of "
			    "%u bytes\n", wal->region_size, seg_size);
		return -ENOSPC;
	}

	wal->seg_size  = seg_size;
	wal->seg_count = (uint32_t)(ring_bytes / seg_size) - 2;

	/* Sized here rather than in wal_alloc() because seg_count is only known
	 * now. open() calls this twice (default first, then the on-disk value),
	 * so the previous array has to go. */
	free(wal->seg_max_seq);
	wal->seg_max_seq = calloc(wal->seg_count, sizeof(*wal->seg_max_seq));
	if (!wal->seg_max_seq) {
		return -ENOMEM;
	}

	return 0;
}

static int
wal_alloc(struct s3_local_dev *local_dev, struct s3_wal **out)
{
	const struct s3_region *region;
	struct s3_wal *wal;

	region = s3_local_dev_get_region(local_dev, S3_REGION_WAL);
	if (!region || !region->valid) {
		SPDK_ERRLOG("No WAL region in the local layout\n");
		return -EINVAL;
	}
	if (region->size <= S3_WAL_SUPER_SIZE) {
		SPDK_ERRLOG("WAL region too small: %" PRIu64 " bytes\n", region->size);
		return -ENOSPC;
	}

	wal = calloc(1, sizeof(*wal));
	if (!wal) {
		return -ENOMEM;
	}

	wal->local_dev      = local_dev;
	wal->desc           = s3_local_dev_get_desc(local_dev, S3_REGION_WAL);
	wal->ch             = s3_local_dev_get_channel(local_dev, S3_REGION_WAL);
	wal->region_offset  = region->offset;
	wal->region_size    = region->size;
	wal->ring_offset    = region->offset + S3_WAL_SUPER_SIZE;
	wal->state          = S3_WAL_RUNNING;

	STAILQ_INIT(&wal->pending);
	STAILQ_INIT(&wal->writing);

	/* I5: DMA safe and 4 KiB aligned. Plain malloc would not satisfy
	 * O_DIRECT on every kernel path, nor NVMe PRP construction. */
	wal->batch_buf = spdk_dma_zmalloc(S3_WAL_BATCH_BUF_SIZE, S3_WAL_BLOCK_SIZE,
					  NULL);
	if (!wal->batch_buf) {
		free(wal);
		return -ENOMEM;
	}

	wal->super_buf = spdk_dma_zmalloc(S3_WAL_SUPER_SLOT_SIZE, S3_WAL_BLOCK_SIZE,
					  NULL);
	if (!wal->super_buf) {
		spdk_dma_free(wal->batch_buf);
		free(wal);
		return -ENOMEM;
	}

	*out = wal;
	return 0;
}

static void
wal_free(struct s3_wal *wal)
{
	if (!wal) {
		return;
	}
	if (wal->batch_poller) {
		spdk_poller_unregister(&wal->batch_poller);
	}
	if (wal->batch_buf) {
		spdk_dma_free(wal->batch_buf);
	}
	if (wal->super_buf) {
		spdk_dma_free(wal->super_buf);
	}
	free(wal->seg_max_seq);
	free(wal);
}

static void
wal_start_poller(struct s3_wal *wal)
{
	/* Polled at a quarter of the age target so the trigger is reasonably
	 * accurate without spinning. */
	wal->batch_poller = SPDK_POLLER_REGISTER(wal_batch_poller, wal,
						 S3_WAL_BATCH_MAX_US / 4);
}

struct wal_open_ctx {
	struct s3_wal   *wal;
	s3_wal_open_cb   cb_fn;
	void            *cb_arg;
	void            *buf;
	bool             formatting;
};

static void
wal_open_finish(struct wal_open_ctx *ctx, int status)
{
	struct s3_wal *wal = ctx->wal;
	s3_wal_open_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;

	if (ctx->buf) {
		spdk_dma_free(ctx->buf);
	}
	free(ctx);

	if (status != 0) {
		wal_free(wal);
		cb_fn(cb_arg, NULL, status);
		return;
	}

	wal->busy = false;
	wal_start_poller(wal);

	cb_fn(cb_arg, wal, 0);
}

static void
wal_create_head_cleared(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct wal_open_ctx *ctx = cb_arg;
	struct s3_wal *wal = ctx->wal;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		SPDK_ERRLOG("Failed to clear the WAL head\n");
		wal_open_finish(ctx, -EIO);
		return;
	}

	SPDK_NOTICELOG("WAL created: %u segments x %u MiB = %" PRIu64 " MiB usable, "
		       "epoch %" PRIu64 "\n",
		       wal->seg_count, wal->seg_size / (1024 * 1024),
		       wal_capacity(wal) / (1024 * 1024), wal->epoch);

	wal_open_finish(ctx, 0);
}

static void
wal_create_super_written(void *cb_arg, int status)
{
	struct wal_open_ctx *ctx = cb_arg;
	struct s3_wal *wal = ctx->wal;
	int rc;

	if (status != 0) {
		wal_open_finish(ctx, status);
		return;
	}

	/* Zero the first block of segment 0 so a scan sees a zero magic and
	 * concludes "empty" rather than reading stale bytes. Only one block is
	 * cleared: recovery stops at the first unreadable entry, so there is no
	 * need to erase gigabytes at format time. */
	memset(wal->batch_buf, 0, S3_WAL_BLOCK_SIZE);

	rc = spdk_bdev_write(wal->desc, wal->ch, wal->batch_buf, wal->ring_offset,
			     S3_WAL_BLOCK_SIZE, wal_create_head_cleared, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit WAL head clear: %d\n", rc);
		wal_open_finish(ctx, rc);
	}
}

void
s3_wal_create(struct s3_local_dev *local_dev, const struct s3_wal_opts *opts,
	      s3_wal_open_cb cb_fn, void *cb_arg)
{
	struct wal_open_ctx *ctx;
	struct s3_wal *wal = NULL;
	uint32_t seg_size = opts ? opts->seg_size : 0;
	int rc;

	assert(cb_fn != NULL);

	if (!local_dev) {
		cb_fn(cb_arg, NULL, -EINVAL);
		return;
	}

	rc = wal_alloc(local_dev, &wal);
	if (rc != 0) {
		cb_fn(cb_arg, NULL, rc);
		return;
	}

	rc = wal_setup_geometry(wal, seg_size);
	if (rc != 0) {
		wal_free(wal);
		cb_fn(cb_arg, NULL, rc);
		return;
	}

	/* A fresh log starts at epoch 1: epoch 0 would make the top bits of seq
	 * zero, which is indistinguishable from an unwritten entry. */
	wal->epoch         = 1;
	wal->seq_next      = 1;
	wal->batch_id_next = 1;
	wal->head          = 0;
	wal->tail          = 0;
	wal->ckpt_head     = 0;
	wal->ckpt_seq      = 0;
	wal->super_slot    = 0;
	wal->slot_gen      = 0;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		wal_free(wal);
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	ctx->wal        = wal;
	ctx->cb_fn      = cb_fn;
	ctx->cb_arg     = cb_arg;
	ctx->formatting = true;

	/* Refuse appends until the super and head are on disk. */
	wal->busy = true;

	s3_wal_sync_super(wal, 0, wal_create_super_written, ctx);
}

static void
wal_open_super_read(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct wal_open_ctx *ctx = cb_arg;
	struct s3_wal *wal = ctx->wal;
	struct s3_wal_super *slots = ctx->buf;
	struct s3_wal_super *best = NULL;
	int best_idx = -1;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		SPDK_ERRLOG("Failed to read the WAL super block\n");
		wal_open_finish(ctx, -EIO);
		return;
	}

	/* Pick the newer valid slot. Both are checked because a crash during an
	 * update can leave one of them torn. */
	for (int i = 0; i < 2; i++) {
		struct s3_wal_super *sb =
			(struct s3_wal_super *)((uint8_t *)slots +
						i * S3_WAL_SUPER_SLOT_SIZE);

		if (sb->magic != S3_WAL_SUPER_MAGIC) {
			continue;
		}
		if (wal_super_crc(sb) != sb->crc) {
			SPDK_WARNLOG("WAL super slot %d has a bad CRC; ignoring\n", i);
			continue;
		}
		if (!best || sb->slot_gen > best->slot_gen) {
			best = sb;
			best_idx = i;
		}
	}

	if (!best) {
		SPDK_ERRLOG("No valid WAL super block; format it first\n");
		wal_open_finish(ctx, -EILSEQ);
		return;
	}
	if (best->version != S3_WAL_SUPER_VERSION) {
		SPDK_ERRLOG("Unsupported WAL version %u (this build understands %d)\n",
			    best->version, S3_WAL_SUPER_VERSION);
		wal_open_finish(ctx, -EPROTO);
		return;
	}
	/* Adopt the on-disk seg_size, then re-derive seg_count and check it
	 * agrees. A disagreement means the WAL region was resized underneath an
	 * existing log, and every offset derived from seg_count would then point
	 * somewhere else, so refuse rather than silently misread the log. */
	if (wal_setup_geometry(wal, best->seg_size) != 0) {
		wal_open_finish(ctx, -EPROTO);
		return;
	}
	if (best->seg_count != wal->seg_count) {
		SPDK_ERRLOG("WAL segment count mismatch: on disk %u, the region now "
			    "yields %u -- was the WAL region resized?\n",
			    best->seg_count, wal->seg_count);
		wal_open_finish(ctx, -EPROTO);
		return;
	}

	wal->head       = best->head;
	wal->tail       = best->tail;
	wal->ckpt_head  = best->ckpt_head;
	wal->ckpt_seq   = best->ckpt_seq;
	wal->seq_next   = best->seq_next;
	wal->slot_gen   = best->slot_gen;
	/* Write to the slot that is not the current newest. */
	wal->super_slot = (uint32_t)(best_idx ^ 1);

	/* Bump the epoch so entries written from now on cannot be confused with
	 * anything left over from the previous run (step 6). */
	wal->epoch      = best->epoch + 1;
	wal->batch_id_next = 1;

	SPDK_NOTICELOG("WAL opened: epoch %" PRIu64 " -> %" PRIu64
		       ", head=%" PRIu64 ", tail=%" PRIu64 ", ckpt_seq=%" PRIu64 "\n",
		       best->epoch, wal->epoch, wal->head, wal->tail, wal->ckpt_seq);

	wal_open_finish(ctx, 0);
}

void
s3_wal_open(struct s3_local_dev *local_dev, s3_wal_open_cb cb_fn, void *cb_arg)
{
	struct wal_open_ctx *ctx;
	struct s3_wal *wal = NULL;
	int rc;

	assert(cb_fn != NULL);

	if (!local_dev) {
		cb_fn(cb_arg, NULL, -EINVAL);
		return;
	}

	/* Geometry is deliberately left unset here: the real seg_size lives in the
	 * super block, so wal_setup_geometry() runs in the read completion. */
	rc = wal_alloc(local_dev, &wal);
	if (rc != 0) {
		cb_fn(cb_arg, NULL, rc);
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		wal_free(wal);
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	ctx->wal    = wal;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;

	ctx->buf = spdk_dma_zmalloc(S3_WAL_SUPER_SIZE, S3_WAL_BLOCK_SIZE, NULL);
	if (!ctx->buf) {
		wal_open_finish(ctx, -ENOMEM);
		return;
	}

	wal->busy = true;

	rc = spdk_bdev_read(wal->desc, wal->ch, ctx->buf, wal->region_offset,
			    S3_WAL_SUPER_SIZE, wal_open_super_read, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit WAL super read: %d\n", rc);
		wal_open_finish(ctx, rc);
	}
}

/* ==========================================================================
 * Close
 * ========================================================================== */

struct wal_close_ctx {
	struct s3_wal *wal;
	s3_wal_cb      cb_fn;
	void          *cb_arg;
};

static void
wal_close_super_done(void *cb_arg, int status)
{
	struct wal_close_ctx *ctx = cb_arg;
	struct s3_wal *wal = ctx->wal;
	s3_wal_cb cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;

	free(ctx);
	wal_free(wal);

	if (cb_fn) {
		cb_fn(user_arg, status);
	}
}

void
s3_wal_close(struct s3_wal *wal, s3_wal_cb cb_fn, void *cb_arg)
{
	struct wal_close_ctx *ctx;

	if (!wal) {
		if (cb_fn) {
			cb_fn(cb_arg, 0);
		}
		return;
	}

	/* Releasing with work outstanding means a bdev completion would touch
	 * freed memory. Same reasoning as s3_journal_destroy(). */
	assert(!wal->batch_in_flight);
	assert(STAILQ_EMPTY(&wal->pending));
	assert(STAILQ_EMPTY(&wal->writing));

	if (wal->batch_in_flight || !STAILQ_EMPTY(&wal->pending) ||
	    !STAILQ_EMPTY(&wal->writing)) {
		SPDK_ERRLOG("s3_wal_close() called with work still outstanding; "
			    "leaking the WAL to avoid use-after-free\n");
		if (cb_fn) {
			cb_fn(cb_arg, -EBUSY);
		}
		return;
	}

	wal->state = S3_WAL_CLOSING;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		wal_free(wal);
		if (cb_fn) {
			cb_fn(cb_arg, -ENOMEM);
		}
		return;
	}
	ctx->wal    = wal;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;

	/* Persist the final positions so the next open does not rescan from an
	 * old checkpoint. */
	s3_wal_sync_super(wal, wal->ckpt_seq, wal_close_super_done, ctx);
}

/* ==========================================================================
 * Replay
 *
 * Scans forward from ckpt_head. Entries are buffered per batch_id and only
 * applied once the batch is closed, which is what makes a torn tail vanish
 * whole instead of half-applying (W4).
 * ========================================================================== */

struct wal_replay_ctx {
	struct s3_wal      *wal;

	s3_wal_replay_cb    apply_fn;
	void               *apply_arg;
	s3_wal_cb           done_fn;
	void               *done_arg;

	/* One segment is read at a time: entries may straddle blocks, so a
	 * whole segment in memory removes all the boundary special cases. */
	void               *buf;
	uint32_t            buf_len;

	uint64_t            scan_pos;      /* logical ring position being read */
	uint64_t            scanned_bytes;
	uint64_t            max_seq;

	/* Entries of the batch currently being collected. */
	struct s3_wal_entry_hdr *batch_hdrs;
	void                **batch_payloads;
	uint32_t                 batch_count;
	uint32_t                 batch_cap;
	uint64_t                 batch_id;

	uint64_t            applied;
	uint64_t            dropped;

	int                 status;
	bool                stop;
};

static void wal_replay_read_segment(struct wal_replay_ctx *rctx);

static void
wal_replay_reset_batch(struct wal_replay_ctx *rctx)
{
	rctx->batch_count = 0;
	rctx->batch_id = 0;
}

static void
wal_replay_done(struct wal_replay_ctx *rctx, int status)
{
	struct s3_wal *wal = rctx->wal;
	s3_wal_cb done_fn = rctx->done_fn;
	void *done_arg = rctx->done_arg;

	/* An unclosed trailing batch is discarded, which is exactly W4. */
	if (rctx->batch_count > 0) {
		SPDK_NOTICELOG("WAL replay dropping an unclosed trailing batch of "
			       "%u entries\n", rctx->batch_count);
		rctx->dropped++;
	}

	wal->head     = rctx->scan_pos;
	wal->seq_next = (rctx->max_seq & 0xFFFFFFFFFFFFULL) + 1;

	wal->stats.replayed_entries += rctx->applied;
	wal->stats.dropped_batches  += rctx->dropped;

	SPDK_NOTICELOG("WAL replay done: %" PRIu64 " entries applied, %" PRIu64
		       " batches dropped, head=%" PRIu64 ", seq_next=%" PRIu64 "\n",
		       rctx->applied, rctx->dropped, wal->head, wal->seq_next);

	free(rctx->batch_hdrs);
	free(rctx->batch_payloads);
	spdk_dma_free(rctx->buf);
	free(rctx);

	wal->busy = false;
	wal_update_state(wal);

	if (done_fn) {
		done_fn(done_arg, status);
	}
}

/* Apply a closed batch in seq order. */
static int
wal_replay_flush_batch(struct wal_replay_ctx *rctx)
{
	int rc = 0;

	for (uint32_t i = 0; i < rctx->batch_count; i++) {
		rc = rctx->apply_fn(rctx->apply_arg, &rctx->batch_hdrs[i],
				    rctx->batch_payloads[i]);
		if (rc != 0) {
			SPDK_ERRLOG("WAL replay stopped: apply failed with %d at "
				    "seq %" PRIu64 "\n", rc, rctx->batch_hdrs[i].seq);
			break;
		}
		rctx->applied++;
	}

	wal_replay_reset_batch(rctx);
	return rc;
}

static int
wal_replay_stash(struct wal_replay_ctx *rctx, const struct s3_wal_entry_hdr *hdr,
		 void *payload)
{
	if (rctx->batch_count == rctx->batch_cap) {
		uint32_t cap = rctx->batch_cap ? rctx->batch_cap * 2 : 64;
		struct s3_wal_entry_hdr *h;
		void **p;

		h = realloc(rctx->batch_hdrs, cap * sizeof(*h));
		if (!h) {
			return -ENOMEM;
		}
		rctx->batch_hdrs = h;

		p = realloc(rctx->batch_payloads, cap * sizeof(*p));
		if (!p) {
			return -ENOMEM;
		}
		rctx->batch_payloads = p;

		rctx->batch_cap = cap;
	}

	rctx->batch_hdrs[rctx->batch_count] = *hdr;
	rctx->batch_payloads[rctx->batch_count] = payload;
	rctx->batch_count++;

	return 0;
}

static void
wal_replay_segment_read(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct wal_replay_ctx *rctx = cb_arg;
	struct s3_wal *wal = rctx->wal;
	uint8_t *base = rctx->buf;
	uint64_t off_in_seg;
	uint64_t seg_start;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		SPDK_ERRLOG("Failed to read a WAL segment during replay\n");
		wal_replay_done(rctx, -EIO);
		return;
	}

	seg_start = rctx->scan_pos - (rctx->scan_pos % wal->seg_size);
	off_in_seg = rctx->scan_pos - seg_start;

	while (off_in_seg + sizeof(struct s3_wal_entry_hdr) <= wal->seg_size) {
		struct s3_wal_entry_hdr *hdr =
			(struct s3_wal_entry_hdr *)(base + off_in_seg);
		void *payload = NULL;
		uint64_t total;

		if (hdr->magic != S3_WAL_ENTRY_MAGIC) {
			/* Zero or garbage: this is the end of the log. */
			rctx->stop = true;
			break;
		}
		if (wal_hdr_crc(hdr) != hdr->crc_hdr) {
			SPDK_NOTICELOG("WAL replay stopping at a bad header CRC "
				       "(seq %" PRIu64 ")\n", hdr->seq);
			rctx->stop = true;
			break;
		}

		if (hdr->type == S3_WAL_END) {
			/* Segment closed early: jump to the next one. */
			rctx->scan_pos = seg_start + wal->seg_size;
			rctx->scanned_bytes += wal->seg_size - off_in_seg;
			if (hdr->seq > rctx->max_seq) {
				rctx->max_seq = hdr->seq;
			}
			wal_record_seg_seq(wal, seg_start, hdr->seq);
			wal_replay_read_segment(rctx);
			return;
		}

		total = sizeof(*hdr) + hdr->payload_len;
		if (off_in_seg + total > wal->seg_size) {
			/* Header claims a payload that runs past the segment,
			 * which can only be corruption. */
			SPDK_NOTICELOG("WAL replay stopping: entry at %" PRIu64
				       " overruns its segment\n", rctx->scan_pos);
			rctx->stop = true;
			break;
		}

		if (hdr->payload_len) {
			payload = base + off_in_seg + sizeof(*hdr);
			if (spdk_crc32c_update(payload, hdr->payload_len, ~0u) !=
			    hdr->crc_payload) {
				SPDK_NOTICELOG("WAL replay stopping at a bad payload "
					       "CRC (seq %" PRIu64 ")\n", hdr->seq);
				rctx->stop = true;
				break;
			}
		}

		/* A new batch_id means the previous batch never closed. */
		if (rctx->batch_count > 0 && hdr->batch_id != rctx->batch_id) {
			SPDK_NOTICELOG("WAL replay dropping an unclosed batch %" PRIu64
				       "\n", rctx->batch_id);
			rctx->dropped++;
			wal_replay_reset_batch(rctx);
		}
		rctx->batch_id = hdr->batch_id;

		if (wal_replay_stash(rctx, hdr, payload) != 0) {
			wal_replay_done(rctx, -ENOMEM);
			return;
		}

		if (hdr->seq > rctx->max_seq) {
			rctx->max_seq = hdr->seq;
		}

		/* Rebuild what truncation needs. The scan is the only place this
		 * can come from -- seg_max_seq[] is not persisted. */
		wal_record_seg_seq(wal, seg_start, hdr->seq);

		off_in_seg += total;
		rctx->scan_pos = seg_start + off_in_seg;

		if (hdr->flags & S3_WAL_F_LAST_IN_BATCH) {
			int rc = wal_replay_flush_batch(rctx);

			if (rc != 0) {
				wal_replay_done(rctx, rc);
				return;
			}
			/* head must land on a block boundary, matching how the
			 * batch was padded on the way out (I3). */
			off_in_seg = SPDK_ALIGN_CEIL(off_in_seg, S3_WAL_BLOCK_SIZE);
			rctx->scan_pos = seg_start + off_in_seg;
		}
	}

	if (rctx->stop || off_in_seg >= wal->seg_size) {
		if (rctx->stop) {
			wal_replay_done(rctx, 0);
			return;
		}
		rctx->scan_pos = seg_start + wal->seg_size;
		wal_replay_read_segment(rctx);
		return;
	}

	wal_replay_done(rctx, 0);
}

static void
wal_replay_read_segment(struct wal_replay_ctx *rctx)
{
	struct s3_wal *wal = rctx->wal;
	uint64_t seg_start;
	int rc;

	/* Stop after a full lap: without this aring whose entries are all valid
	 * would loop forever. */
	if (rctx->scanned_bytes >= wal_capacity(wal)) {
		SPDK_NOTICELOG("WAL replay scanned the whole ring\n");
		wal_replay_done(rctx, 0);
		return;
	}

	seg_start = rctx->scan_pos - (rctx->scan_pos % wal->seg_size);
	rctx->scanned_bytes += wal->seg_size;

	rc = spdk_bdev_read(wal->desc, wal->ch, rctx->buf,
			    wal_disk_offset(wal, seg_start), wal->seg_size,
			    wal_replay_segment_read, rctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit a WAL segment read: %d\n", rc);
		wal_replay_done(rctx, rc);
	}
}

void
s3_wal_replay(struct s3_wal *wal, s3_wal_replay_cb apply_fn, void *apply_arg,
	      s3_wal_cb done_fn, void *done_arg)
{
	struct wal_replay_ctx *rctx;

	if (!wal || !apply_fn) {
		if (done_fn) {
			done_fn(done_arg, -EINVAL);
		}
		return;
	}

	rctx = calloc(1, sizeof(*rctx));
	if (!rctx) {
		if (done_fn) {
			done_fn(done_arg, -ENOMEM);
		}
		return;
	}

	/* A whole segment at a time, so an entry never straddles the buffer. */
	rctx->buf = spdk_dma_zmalloc(wal->seg_size, S3_WAL_BLOCK_SIZE, NULL);
	if (!rctx->buf) {
		free(rctx);
		if (done_fn) {
			done_fn(done_arg, -ENOMEM);
		}
		return;
	}
	rctx->buf_len = wal->seg_size;

	rctx->wal       = wal;
	rctx->apply_fn  = apply_fn;
	rctx->apply_arg = apply_arg;
	rctx->done_fn   = done_fn;
	rctx->done_arg  = done_arg;
	rctx->scan_pos  = wal->ckpt_head;
	rctx->max_seq   = wal->ckpt_seq;

	/* No appends while head and the scan cursor are both moving. */
	wal->busy = true;

	wal_replay_read_segment(rctx);
}

/* ==========================================================================
 * Truncation
 * ========================================================================== */

void
s3_wal_truncate_to_seq(struct s3_wal *wal, uint64_t safe_seq)
{
	uint32_t released = 0;

	if (!wal || !wal->seg_max_seq) {
		return;
	}

	/* Never truncate during a replay.
	 *
	 * This is not caution, it is required. seg_max_seq[] is not persisted --
	 * it is rebuilt *by* the replay -- so while the replay is running it still
	 * reads as zero for every segment not yet scanned. The test below would
	 * therefore accept every closed segment and tail would run past data that
	 * has not been applied yet, taking ckpt_head with it. Nothing is
	 * physically overwritten (appends are refused while busy), but a crash in
	 * that window would lose acknowledged writes, because recovery would
	 * restart from the advanced ckpt_head.
	 *
	 * The situation is reachable: the flusher is already running while the WAL
	 * replay is in flight, since s3_bs_dev_attach_wal() has to precede it so
	 * that the overlay exists. Skipping a round costs nothing -- the flusher
	 * calls this again on its next tick.
	 */
	if (wal->busy) {
		return;
	}

	/* Segment granularity is the whole point: a segment is either entirely
	 * consumed or it is not, so reuse never has to reason about a partially
	 * live segment.
	 *
	 * Two conditions have to hold before tail may pass a segment:
	 *
	 *   1. the segment is closed, i.e. head has moved past its end. The
	 *      segment head currently sits in is still being appended to.
	 *   2. every entry in it is at or below safe_seq, which the flusher
	 *      defines as "already in S3".
	 *
	 * Getting this wrong in the optimistic direction overwrites acknowledged
	 * writes and is unrecoverable; getting it wrong the other way only wastes
	 * space. Hence the conservative pair of checks and the early exit on the
	 * first segment that fails either. */
	while (wal->tail + wal->seg_size <= wal->head) {
		uint64_t seg = wal_seg_of(wal, wal->tail);

		if (wal->seg_max_seq[seg] > safe_seq) {
			break;
		}

		wal->seg_max_seq[seg] = 0;
		wal->tail += wal->seg_size;
		released++;
	}

	if (released == 0) {
		return;
	}

	/* Replay must start where the live data now starts. Leaving ckpt_head
	 * behind would make recovery scan segments that have since been reused,
	 * i.e. bytes from a later lap of the ring. */
	wal->ckpt_head = wal->tail;
	wal->stats.segments_released += released;

	wal_update_state(wal);
}

/* ==========================================================================
 * Accessors
 * ========================================================================== */

uint64_t
s3_wal_get_used_bytes(const struct s3_wal *wal)
{
	return wal ? wal_used(wal) : 0;
}

uint64_t
s3_wal_get_next_seq(const struct s3_wal *wal)
{
	return wal ? wal_make_seq(wal, wal->seq_next) : 0;
}

uint64_t
s3_wal_get_epoch(const struct s3_wal *wal)
{
	return wal ? wal->epoch : 0;
}

void
s3_wal_get_stats(const struct s3_wal *wal, struct s3_wal_stats *out)
{
	if (!wal || !out) {
		return;
	}
	*out = wal->stats;
}

SPDK_LOG_REGISTER_COMPONENT(s3_wal)
