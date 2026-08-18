/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_journal -- change log for the chunk map 
 *
 *   The design rationale, including why this is fully asynchronous and why the
 *   synchronous version was deleted, lives in the header comment of
 *   include/s3lvol/s3_journal.h.
 *
 *   Three things to keep an eye on in this implementation:
 *
 *   1. *block_buf is shared mutable state and must not be touched while a
 *      write is in flight.* Filling a slot therefore only happens inside
 *      journal_kick(), which is where "no write in flight" is guaranteed;
 *      append() only assigns the LSN, computes the CRC and puts the request on
 *      the waiting queue.
 *      Keep it to that single path -- do not add a fast path for "nothing is
 *      in flight right now". That yields two slot-filling implementations, and
 *      they very easily disagree about when cur_count advances.
 *   2. *LSNs are assigned in append(), not in kick().* That makes queue order
 *      equal to LSN order, which is what lets replay scan in physical order.
 *   3. When a block write completes, every request in that batch gets its
 *      callback -- their records all live in the same block, so one write made
 *      all of them durable at once.
 */

#include "spdk/stdinc.h"
#include "spdk/bdev.h"
#include "spdk/crc32.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/thread.h"
#include "spdk/util.h"

#include "s3lvol/s3_journal.h"

#define S3_JOURNAL_BLOCK_SIZE 4096

/* One pending append.
 *
 * The record travels with the request until journal_kick() copies it into a
 * slot in block_buf. That way append() never touches block_buf, whether or not
 * a write is currently in flight. */
struct journal_append_req {
	struct s3_journal_record         rec;
	s3_journal_cb                    cb_fn;
	void                            *cb_arg;
	TAILQ_ENTRY(journal_append_req)  link;
};

struct s3_journal {
	struct s3_local_dev     *local_dev;
	struct spdk_bdev_desc   *desc;
	struct spdk_io_channel  *ch;

	uint64_t                 region_offset;
	uint64_t                 region_size;

	/* LSN for the next record. Monotonic and never wraps -- what wraps is
	 * the write position, not the LSN. */
	uint64_t                 next_lsn;

	/* The block being filled: its index within the region plus how many
	 * record slots are already used. */
	uint64_t                 cur_block;
	uint32_t                 cur_count;

	/* In-memory mirror of the current block. An append modifies this and
	 * then rewrites the whole block, so the block never has to be read back
	 * first (no read-modify-write). */
	void                    *block_buf;

	/* A block write is in flight. Modifying block_buf during this window is
	 * *forbidden*. */
	bool                     write_in_flight;

	/* Requests already filled into block_buf and covered by the write
	 * currently in flight. They all get their callbacks when it completes. */
	TAILQ_HEAD(, journal_append_req) writing;

	/* Requests not yet assigned a slot: they arrived while a write was in
	 * flight, or the previous block had no room left. */
	TAILQ_HEAD(, journal_append_req) waiting;

	/* LSN already covered by a checkpoint. The basis for truncation, and
	 * also what makes wrapping around safe. */
	uint64_t                 truncated_lsn;

	/* Highest LSN stored in each block of the region, indexed by block.
	 *
	 * *Not persisted* -- the replay scan rebuilds it, exactly like the WAL's
	 * seg_max_seq[]. It exists so that reusing a block can be checked
	 * exactly: "is everything in the block I am about to overwrite already
	 * covered by a checkpoint?". One u64 per 4 KiB block, so a 256 MiB
	 * journal costs 512 KiB. */
	uint64_t                *block_max_lsn;

	uint64_t                 blocks_written;

	/* Replaying or formatting: appends are rejected and kick() must not
	 * touch block_buf. */
	bool                     busy;
};

static uint64_t
journal_blocks_total(const struct s3_journal *j)
{
	return j->region_size / S3_JOURNAL_BLOCK_SIZE;
}

/* ==========================================================================
 * Record CRC
 * ========================================================================== */

static uint32_t
record_calc_crc(const struct s3_journal_record *rec)
{
	struct s3_journal_record tmp;

	memcpy(&tmp, rec, sizeof(tmp));
	tmp.crc = 0;

	return spdk_crc32c_update(&tmp, sizeof(tmp), ~0u);
}

static bool
record_is_valid(const struct s3_journal_record *rec)
{
	/* An all-zero block means "never written" -- LSNs start at 1, so
	 * lsn == 0 marks an empty slot. */
	if (rec->lsn == 0) {
		return false;
	}
	if (rec->op < S3_JOURNAL_OP_CHUNK_UPDATE ||
	    rec->op > S3_JOURNAL_OP_CHECKPOINT) {
		return false;
	}
	return record_calc_crc(rec) == rec->crc;
}

/* ==========================================================================
 * Append path: queue -> fill slots -> whole-block write -> complete a batch
 * ========================================================================== */

static void journal_kick(struct s3_journal *j);

/* Fire the callbacks for a batch of requests and free them.
 *
 * The caller is responsible for detaching the list first, because a callback
 * may well call append() again, which mutates waiting/writing -- iterating a
 * list while it is being modified would run off the end. */
static void
journal_complete_batch(struct journal_append_req *first, int status)
{
	struct journal_append_req *req = first, *next;

	while (req) {
		next = TAILQ_NEXT(req, link);

		if (req->cb_fn) {
			req->cb_fn(req->cb_arg, status);
		}
		free(req);

		req = next;
	}
}

/* Fail the entire waiting queue. Used when writing cannot continue (wrap
 * blocked, submission failed) -- requests must never be left on the queue
 * waiting for a callback that will never come. */
static void
journal_fail_waiting(struct s3_journal *j, int status)
{
	struct journal_append_req *batch = TAILQ_FIRST(&j->waiting);

	TAILQ_INIT(&j->waiting);
	journal_complete_batch(batch, status);
}

static void
journal_write_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct s3_journal *j = cb_arg;
	struct journal_append_req *batch;
	int status = success ? 0 : -EIO;

	spdk_bdev_free_io(bdev_io);

	/* Detach this batch -- all of their records are in the block that just
	 * hit the disk. */
	batch = TAILQ_FIRST(&j->writing);
	TAILQ_INIT(&j->writing);

	j->write_in_flight = false;

	if (status != 0) {
		/* The write failed, so this batch is not durable.
		 *
		 * *Neither the slots in block_buf nor cur_count are rolled
		 * back.* The next round keeps filling after cur_count, so the
		 * failed records stay in the buffer and will be written again by
		 * the next block write. That looks like "reported failure but
		 * persisted anyway", yet it is self-consistent: the caller saw a
		 * failure and therefore did not update its in-memory mapping, so
		 * during replay the record simply restores one extra mapping --
		 * and replay is idempotent anyway (an extra mapping only delays
		 * GC of an object that was going to be collected).
		 *
		 * Rolling back is the dangerous choice: decrementing cur_count
		 * would let the next record reuse the same slot, leaving an LSN
		 * hole on disk. Replay stops at the first invalid record, so
		 * every durable record after that hole would be discarded as
		 * "crash debris". */
		SPDK_ERRLOG("Journal block %" PRIu64 " write failed: %d\n",
			    j->cur_block, status);
	} else {
		j->blocks_written++;
	}

	journal_complete_batch(batch, status);

	/* A callback may have appended more, which then batches into the next
	 * round -- hence kick() comes after complete_batch(). */
	journal_kick(j);
}

/* Fill as many waiting requests as fit into the current block, then issue one
 * write.
 *
 * Idempotent: returns immediately when a write is in flight, when busy, or when
 * nobody is waiting. */
static void
journal_kick(struct s3_journal *j)
{
	uint64_t block_off;
	int rc;

	if (j->write_in_flight || j->busy) {
		return;
	}
	if (TAILQ_EMPTY(&j->waiting)) {
		return;
	}

	/* Current block is full, so move to the next one. Nothing is in flight
	 * at this point, which is what makes the memset safe. */
	if (j->cur_count >= S3_JOURNAL_RECORDS_PER_BLOCK) {
		uint64_t next_block = j->cur_block + 1;

		if (next_block >= journal_blocks_total(j)) {
			next_block = 0;
		}

		/* Reusing a block destroys whatever records are in it, so it is
		 * only allowed once a checkpoint covers all of them.
		 *
		 * *This used to test `truncated_lsn == 0`*, i.e. only "has any
		 * checkpoint ever completed". That is not enough and lost data: one
		 * checkpoint early in the lvstore's life set truncated_lsn to
		 * something small, and from then on every wrap passed the test --
		 * including wraps over records far newer than that checkpoint. Those
		 * records were overwritten while still being the only copy of their
		 * mapping, which orphans live objects.
		 *
		 * block_max_lsn[] makes the test exact. It is not persisted -- the
		 * replay scan rebuilds it, exactly like the WAL's seg_max_seq[]. */
		if (j->block_max_lsn[next_block] > j->truncated_lsn) {
			SPDK_ERRLOG("Journal is full: reusing block %" PRIu64
				    " would destroy records up to LSN %" PRIu64
				    " but only %" PRIu64 " is covered by a "
				    "checkpoint. Refusing to overwrite them.\n",
				    next_block, j->block_max_lsn[next_block],
				    j->truncated_lsn);
			journal_fail_waiting(j, -ENOSPC);
			return;
		}

		if (next_block == 0) {
			SPDK_NOTICELOG("Journal wrapped around (truncated_lsn=%"
				       PRIu64 ")\n", j->truncated_lsn);
		}
		j->cur_block = next_block;
		j->cur_count = 0;
		memset(j->block_buf, 0, S3_JOURNAL_BLOCK_SIZE);
	}

	/* Fill slots: everything that fits in this block goes in, the rest stays
	 * on the waiting queue for the next round. */
	while (!TAILQ_EMPTY(&j->waiting) &&
	       j->cur_count < S3_JOURNAL_RECORDS_PER_BLOCK) {
		struct journal_append_req *req = TAILQ_FIRST(&j->waiting);
		struct s3_journal_record *slot =
			(struct s3_journal_record *)j->block_buf + j->cur_count;

		memcpy(slot, &req->rec, sizeof(req->rec));
		/* LSNs are assigned in queue order, so the last record filled into
		 * a block always carries its highest LSN. */
		j->block_max_lsn[j->cur_block] = req->rec.lsn;
		j->cur_count++;

		TAILQ_REMOVE(&j->waiting, req, link);
		TAILQ_INSERT_TAIL(&j->writing, req, link);
	}

	block_off = j->region_offset + j->cur_block * S3_JOURNAL_BLOCK_SIZE;

	/* Whole-block write. block_buf holds the complete contents of this block
	 * (including records in it that are already durable), so there is no
	 * need to read it back first -- no read-modify-write. */
	j->write_in_flight = true;
	rc = spdk_bdev_write(j->desc, j->ch, j->block_buf, block_off,
			     S3_JOURNAL_BLOCK_SIZE, journal_write_done, j);
	if (rc != 0) {
		struct journal_append_req *batch;

		j->write_in_flight = false;

		batch = TAILQ_FIRST(&j->writing);
		TAILQ_INIT(&j->writing);

		SPDK_ERRLOG("Failed to submit journal block write: %d\n", rc);
		journal_complete_batch(batch, rc);

		/* Submission failures are almost always -ENOMEM (bdev_io pool
		 * exhausted). Whatever is still waiting is deliberately not
		 * retried here -- that would spin. It gets picked up by the next
		 * append, by which point the pool usually has room again. If no
		 * append ever arrives, destroy()'s assert surfaces them instead
		 * of hanging silently. */
		return;
	}
}

/* Assign the LSN, compute the CRC, queue the request. *Does not touch
 * block_buf.* */
static void
journal_append(struct s3_journal *j, const struct s3_journal_record *rec_in,
	       uint64_t *out_lsn, s3_journal_cb cb_fn, void *cb_arg)
{
	struct journal_append_req *req;

	if (j->busy) {
		/* Appends are rejected during replay and formatting. This is a
		 * caller sequencing error rather than a runtime fault -- report
		 * it loudly, because queueing would disguise it as an
		 * occasional latency spike. */
		SPDK_ERRLOG("Journal is busy (replaying or formatting); "
			    "append rejected\n");
		if (cb_fn) {
			cb_fn(cb_arg, -EBUSY);
		}
		return;
	}

	req = calloc(1, sizeof(*req));
	if (!req) {
		if (cb_fn) {
			cb_fn(cb_arg, -ENOMEM);
		}
		return;
	}

	req->rec     = *rec_in;
	req->rec.lsn = j->next_lsn++;
	req->rec.crc = record_calc_crc(&req->rec);
	req->cb_fn   = cb_fn;
	req->cb_arg  = cb_arg;

	/* Handed back *now*, at queue time, not at completion.
	 *
	 * The chunk map keeps it so that it can record, when it commits, which
	 * LSN its in-memory state now includes. A checkpoint needs exactly that
	 * number: taking it from next_lsn instead would cover records that were
	 * queued but never made it to disk, and truncating the journal to such an
	 * LSN would drop mappings the snapshot never captured -- turning live
	 * objects into orphans. */
	if (out_lsn) {
		*out_lsn = req->rec.lsn;
	}

	TAILQ_INSERT_TAIL(&j->waiting, req, link);

	journal_kick(j);
}

void
s3_journal_append_update(struct s3_journal *journal, uint64_t chunk_index,
			 const struct spdk_uuid *uuid, uint32_t valid_bytes,
			 uint64_t gen, uint32_t flags, uint64_t *out_lsn,
			 s3_journal_cb cb_fn, void *cb_arg)
{
	struct s3_journal_record rec = {0};

	if (!journal || !uuid) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	rec.op          = S3_JOURNAL_OP_CHUNK_UPDATE;
	rec.chunk_index = chunk_index;
	rec.valid_bytes = valid_bytes;
	rec.gen         = gen;
	rec.flags       = flags;
	spdk_uuid_copy(&rec.uuid, uuid);

	journal_append(journal, &rec, out_lsn, cb_fn, cb_arg);
}

void
s3_journal_append_remove(struct s3_journal *journal, uint64_t chunk_index,
			 uint64_t *out_lsn, s3_journal_cb cb_fn, void *cb_arg)
{
	struct s3_journal_record rec = {0};

	if (!journal) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	rec.op          = S3_JOURNAL_OP_CHUNK_REMOVE;
	rec.chunk_index = chunk_index;

	journal_append(journal, &rec, out_lsn, cb_fn, cb_arg);
}

/* ==========================================================================
 * Create / open / destroy
 * ========================================================================== */

static int
journal_alloc(struct s3_local_dev *local_dev, struct s3_journal **out)
{
	const struct s3_region *region;
	struct s3_journal *j;

	region = s3_local_dev_get_region(local_dev, S3_REGION_JOURNAL);
	if (!region || !region->valid) {
		SPDK_ERRLOG("No journal region in the local layout\n");
		return -EINVAL;
	}
	if (region->size < S3_JOURNAL_BLOCK_SIZE * 2) {
		SPDK_ERRLOG("Journal region too small: %" PRIu64 " bytes\n",
			    region->size);
		return -ENOSPC;
	}

	j = calloc(1, sizeof(*j));
	if (!j) {
		return -ENOMEM;
	}

	j->local_dev     = local_dev;
	j->desc          = s3_local_dev_get_desc(local_dev, S3_REGION_JOURNAL);
	j->ch            = s3_local_dev_get_channel(local_dev, S3_REGION_JOURNAL);
	j->region_offset = region->offset;
	j->region_size   = region->size;

	TAILQ_INIT(&j->writing);
	TAILQ_INIT(&j->waiting);

	j->block_buf = spdk_dma_zmalloc(S3_JOURNAL_BLOCK_SIZE, 4096, NULL);
	if (!j->block_buf) {
		free(j);
		return -ENOMEM;
	}

	j->block_max_lsn = calloc(journal_blocks_total(j),
				  sizeof(*j->block_max_lsn));
	if (!j->block_max_lsn) {
		spdk_dma_free(j->block_buf);
		free(j);
		return -ENOMEM;
	}

	*out = j;
	return 0;
}

static void
journal_free(struct s3_journal *j)
{
	if (!j) {
		return;
	}
	if (j->block_buf) {
		spdk_dma_free(j->block_buf);
	}
	free(j->block_max_lsn);
	free(j);
}

struct journal_create_ctx {
	struct s3_journal       *journal;
	s3_journal_create_cb     cb_fn;
	void                    *cb_arg;
};

static void
journal_create_cleared(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct journal_create_ctx *ctx = cb_arg;
	struct s3_journal *j = ctx->journal;
	s3_journal_create_cb cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;

	spdk_bdev_free_io(bdev_io);
	free(ctx);

	if (!success) {
		SPDK_ERRLOG("Failed to initialize journal head\n");
		journal_free(j);
		cb_fn(user_arg, NULL, -EIO);
		return;
	}

	j->busy = false;

	SPDK_NOTICELOG("Journal created: %" PRIu64 " MiB, %" PRIu64 " blocks, "
		       "%zu records/block\n",
		       j->region_size / (1024 * 1024), journal_blocks_total(j),
		       S3_JOURNAL_RECORDS_PER_BLOCK);

	cb_fn(user_arg, j, 0);
}

void
s3_journal_create(struct s3_local_dev *local_dev, s3_journal_create_cb cb_fn,
		  void *cb_arg)
{
	struct journal_create_ctx *ctx;
	struct s3_journal *j = NULL;
	int rc;

	assert(cb_fn != NULL);

	if (!local_dev) {
		cb_fn(cb_arg, NULL, -EINVAL);
		return;
	}

	rc = journal_alloc(local_dev, &j);
	if (rc != 0) {
		cb_fn(cb_arg, NULL, rc);
		return;
	}

	/* LSNs start at 1 -- 0 is reserved to mean "empty record" (an all-zero
	 * block). */
	j->next_lsn      = 1;
	j->cur_block     = 0;
	j->cur_count     = 0;
	j->truncated_lsn = 0;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		journal_free(j);
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	ctx->journal = j;
	ctx->cb_fn   = cb_fn;
	ctx->cb_arg  = cb_arg;

	/* Clear the first block so replay knows the journal is empty.
	 * The whole region is deliberately not cleared: it can be hundreds of
	 * MiB, which would make formatting needlessly slow -- replay stops at
	 * the first invalid record and never reads the garbage beyond it. */
	memset(j->block_buf, 0, S3_JOURNAL_BLOCK_SIZE);

	/* No appends until the zeroing is durable. */
	j->busy = true;

	rc = spdk_bdev_write(j->desc, j->ch, j->block_buf, j->region_offset,
			     S3_JOURNAL_BLOCK_SIZE, journal_create_cleared, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit journal initialization: %d\n", rc);
		free(ctx);
		journal_free(j);
		cb_fn(cb_arg, NULL, rc);
		return;
	}
}

int
s3_journal_open(struct s3_local_dev *local_dev, struct s3_journal **out)
{
	const struct s3_super_block *sb;
	struct s3_journal *j;
	int rc;

	if (!local_dev || !out) {
		return -EINVAL;
	}

	rc = journal_alloc(local_dev, &j);
	if (rc != 0) {
		return rc;
	}

	sb = s3_local_dev_get_super(local_dev);
	j->truncated_lsn = sb->checkpoint_lsn;

	/* next_lsn / cur_block / cur_count have to be determined by scanning,
	 * which happens in s3_journal_replay() -- it positions the write cursor
	 * as a side effect. Seed them with something safe in case the caller
	 * writes without replaying first: starting from the beginning still
	 * cannot damage data already covered by a checkpoint (that data is in
	 * S3 already). */
	j->next_lsn  = sb->checkpoint_lsn + 1;
	j->cur_block = 0;
	j->cur_count = 0;

	*out = j;
	return 0;
}

void
s3_journal_destroy(struct s3_journal *journal)
{
	if (!journal) {
		return;
	}

	/* Freeing while requests are in flight means their bdev completion
	 * callbacks touch freed memory. The caller must drain first (wait for
	 * every append callback). Assert rather than tolerate silently -- this
	 * class of use-after-free is extremely hard to track down in
	 * production. */
	assert(!journal->write_in_flight);
	assert(TAILQ_EMPTY(&journal->writing));
	assert(TAILQ_EMPTY(&journal->waiting));

	if (journal->write_in_flight || !TAILQ_EMPTY(&journal->writing) ||
	    !TAILQ_EMPTY(&journal->waiting)) {
		SPDK_ERRLOG("s3_journal_destroy() called with pending appends; "
			    "leaking the journal to avoid use-after-free\n");
		return;
	}

	journal_free(journal);
}

/* ==========================================================================
 * Replay
 *
 * Reads block by block, asynchronously, and is *indifferent to the order records
 * come back in*. That matters because physical order stops matching LSN order the
 * moment the ring wraps: block 0 then holds the newest records and the blocks
 * after it the oldest. Two things make it safe rather than merely tolerable:
 *
 *   - the chunk map rejects a record no newer than what an entry already holds
 *     (s3_chunk_map.c, chunk_entry_accepts()), so replaying newest-first ends at
 *     the same state as oldest-first;
 *   - the write cursor is placed at the block holding the highest LSN, not at
 *     wherever the scan happened to stop.
 *
 * This used to scan in physical order and stop at the first invalid record, with
 * a comment saying out-of-order replay after a wrap was deliberately unhandled
 * because checkpoints were assumed frequent enough to truncate the journal first.
 * That assumption does not hold: the wrap check only proves that *the one block
 * about to be reused* is covered by a checkpoint, so a legal wrap can coexist
 * with older, uncovered records elsewhere in the ring. Three things then went
 * wrong, all silent:
 *
 *   1. records were applied newest-first, and "last writer wins" handed a chunk
 *      back to an object the create-once path had already deleted -- a 404 on
 *      the next read of that chunk;
 *   2. the scan stopped at the first empty slot of the newest block, so every
 *      block physically after it went unread; mappings that lived only there
 *      came back as "chunk never written", i.e. reads returned zeroes;
 *   3. the cursor ended up at the last block scanned, so the next append tried
 *      to reuse a block full of the newest records. The wrap check caught that
 *      one, so it failed with -ENOSPC instead of corrupting -- a journal that
 *      refuses writes while it still has room.
 *
 * === Why it is still allowed to stop early ===
 *
 * Scanning the whole region on every attach would cost a full sequential read of
 * the journal (64 MiB by default, up to 256). It stops at the first *entirely
 * empty* block instead, which is sound because such a block cannot exist past
 * live records:
 *
 *   - never wrapped: blocks fill in ascending order, so an empty block is
 *     followed only by empty ones;
 *   - wrapped: every block in the ring has been written at least once, and a
 *     block being reused is rewritten whole from a zeroed buffer
 *     (journal_kick()), so it holds at least the one record that caused the
 *     reuse. No empty block remains.
 *
 * A record that fails its CRC is a torn write, which can only be in the block
 * written last. It ends that block's records but not the scan; the append it
 * belonged to never completed, so no caller was ever told that mapping was
 * durable.
 * ========================================================================== */

struct journal_replay_ctx {
	struct s3_journal       *journal;

	uint64_t                from_lsn;
	s3_journal_apply_cb      apply_fn;
	void    *apply_arg;
	s3_journal_cb            done_fn;
	void                    *done_arg;

	void                    *buf;

	uint64_t                 blk;           /* which block is being read */
	uint64_t                 total_blocks;

	uint64_t                 applied;
	uint64_t                 skipped;
	uint64_t                 max_lsn;

	/* Where appends resume: the block holding the highest LSN seen, and how
	 * many valid records it starts with. Chosen by LSN rather than by scan
	 * position because after a wrap the two disagree, and appending at the
	 * wrong place either overwrites the newest records or wastes the ring. */
	uint64_t                 tail_block;
	uint64_t                 tail_lsn;
	uint32_t                 tail_count;

	int                      status;
};

static void journal_replay_read_next(struct journal_replay_ctx *rctx);
static void journal_replay_finish(struct journal_replay_ctx *rctx, int status);

/* The single exit point for replay: release the buffer, clear busy, fire done.
 *
 * There is exactly one of these because teardown is reached three ways (no tail
 * block needed / tail block read succeeded / tail block read failed to submit
 * or execute) -- writing it out three times would inevitably miss a free or
 * forget to clear busy. */
static void
journal_replay_done(struct journal_replay_ctx *rctx, int status)
{
	struct s3_journal *j = rctx->journal;
	s3_journal_cb done_fn = rctx->done_fn;
	void *done_arg = rctx->done_arg;
	uint64_t applied = rctx->applied, skipped = rctx->skipped;
	uint64_t from_lsn = rctx->from_lsn;

	spdk_dma_free(rctx->buf);
	free(rctx);

	j->busy = false;

	SPDK_NOTICELOG("Journal replay done: %" PRIu64 " applied, %" PRIu64
		       " skipped (<= ckpt lsn %" PRIu64 "), next_lsn=%" PRIu64
		       ", write pos block=%" PRIu64 " slot=%u\n",
		       applied, skipped, from_lsn, j->next_lsn,
		       j->cur_block, j->cur_count);

	if (done_fn) {
		done_fn(done_arg, status);
	}

	/* Appends are rejected during replay (-EBUSY), so there is no backlog to
	 * kick here. If the caller appends from within done_fn, that path kicks
	 * on its own. */
}

static void
journal_replay_tail_loaded(struct spdk_bdev_io *bdev_io, bool success,
			   void *cb_arg)
{
	struct journal_replay_ctx *rctx = cb_arg;
	int status = rctx->status;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		/* Without the tail block in memory, appending is unsafe: the next
		 * whole-block rewrite would zero out the durable records already
		 * in that block. Report an error so the caller abandons the
		 * attach instead of running with a journal that eats records. */
		SPDK_ERRLOG("Failed to load current journal block; "
			    "appending would clobber durable records\n");
		if (status == 0) {
			status = -EIO;
		}
	}

	journal_replay_done(rctx, status);
}

static void
journal_replay_finish(struct journal_replay_ctx *rctx, int status)
{
	struct s3_journal *j = rctx->journal;
	uint64_t off;
	int rc;

	/* Position the write cursor so appends continue from here. tail_* names
	 * the block with the highest LSN, which after a wrap is not the last block
	 * scanned -- appending there would try to reuse the newest records. */
	j->cur_block = rctx->tail_block;
	j->cur_count = rctx->tail_count;
	j->next_lsn  = rctx->max_lsn + 1;

	rctx->status = status;

	/* The current block is partially full: subsequent appends rewrite it
	 * whole, so the buffer must contain the records already in that block or
	 * they would be wiped. */
	if (rctx->tail_count > 0 &&
	    rctx->tail_count < S3_JOURNAL_RECORDS_PER_BLOCK) {
		off = j->region_offset + j->cur_block * S3_JOURNAL_BLOCK_SIZE;

		rc = spdk_bdev_read(j->desc, j->ch, j->block_buf, off,
				    S3_JOURNAL_BLOCK_SIZE,
				    journal_replay_tail_loaded, rctx);
		if (rc == 0) {
			return;
		}
		SPDK_ERRLOG("Failed to submit read of current journal block: %d\n",
			    rc);
		if (rctx->status == 0) {
			rctx->status = rc;
		}
		journal_replay_done(rctx, rctx->status);
		return;
	}

	/* Either an empty block, or the previous block filled up exactly.
	 *
	 * In the "filled up" case cur_count is left at the full value --
	 * journal_kick() sees that and rolls over to the next block, including
	 * the wrap check. That logic exists in exactly one place. */
	memset(j->block_buf, 0, S3_JOURNAL_BLOCK_SIZE);

	journal_replay_done(rctx, rctx->status);
}

static void
journal_replay_block_read(struct spdk_bdev_io *bdev_io, bool success,
			  void *cb_arg)
{
	struct journal_replay_ctx *rctx = cb_arg;
	struct s3_journal *j = rctx->journal;
	struct s3_journal_record *recs = rctx->buf;
	uint32_t valid_prefix = 0;
	uint64_t blk_max_lsn = 0;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		SPDK_ERRLOG("Failed to read journal block %" PRIu64 "\n",
			    rctx->blk);
		journal_replay_finish(rctx, -EIO);
		return;
	}

	for (uint32_t i = 0; i < S3_JOURNAL_RECORDS_PER_BLOCK; i++) {
		struct s3_journal_record *rec = &recs[i];
		int rc;

		if (!record_is_valid(rec)) {
			/* This block's records end here. lsn == 0 is an unused
			 * slot; a CRC mismatch is a torn write. Either way there
			 * is nothing usable in this slot, and slots after it in
			 * this block are zeroes -- a block is always rewritten
			 * whole from a zeroed buffer. */
			break;
		}

		valid_prefix = i + 1;
		if (rec->lsn > blk_max_lsn) {
			blk_max_lsn = rec->lsn;
		}

		if (rec->lsn <= rctx->from_lsn) {
			/* Already covered by a checkpoint. */
			rctx->skipped++;
		} else {
			rc = rctx->apply_fn(rctx->apply_arg, rec);
			if (rc != 0) {
				/* Not survivable: the record is well formed but
				 * does not fit this map (a chunk index beyond
				 * capacity, say), so the recovered mapping would
				 * be incomplete in a way nothing downstream can
				 * detect. Abandon the attach. */
				SPDK_ERRLOG("Journal replay stopped at lsn=%"
					    PRIu64": apply failed with %d\n",
					    rec->lsn, rc);
				journal_replay_finish(rctx, rc);
				return;
			}
			rctx->applied++;
		}

		if (rec->lsn > rctx->max_lsn) {
			rctx->max_lsn = rec->lsn;
		}
	}

	/* Rebuilt from the scan, because it is not on disk. Without it the wrap
	 * check in journal_kick() would read zero for every block and overwrite
	 * records no checkpoint ever covered. It answers "what is physically in
	 * this block", so records skipped as already checkpointed count too. */
	j->block_max_lsn[rctx->blk] = blk_max_lsn;

	if (blk_max_lsn > rctx->tail_lsn) {
		rctx->tail_lsn   = blk_max_lsn;
		rctx->tail_block = rctx->blk;
		rctx->tail_count = valid_prefix;
	}

	if (valid_prefix == 0) {
		/* An entirely empty block, which cannot be followed by live
		 * records -- see the section comment. Everything worth reading has
		 * been read. */
		journal_replay_finish(rctx, 0);
		return;
	}

	rctx->blk++;
	journal_replay_read_next(rctx);
}

static void
journal_replay_read_next(struct journal_replay_ctx *rctx)
{
	struct s3_journal *j = rctx->journal;
	uint64_t off;
	int rc;

	if (rctx->blk >= rctx->total_blocks) {
		/* Every block held records, so the ring has been all the way round
		 * at least once. The cursor is already at the block with the
		 * highest LSN; whether there is room to append is journal_kick()'s
		 * question, answered per block against the checkpoint. */
		journal_replay_finish(rctx, 0);
		return;
	}

	off = j->region_offset + rctx->blk * S3_JOURNAL_BLOCK_SIZE;

	rc = spdk_bdev_read(j->desc, j->ch, rctx->buf, off,
			    S3_JOURNAL_BLOCK_SIZE,
			    journal_replay_block_read, rctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit journal block %" PRIu64
			    " read: %d\n", rctx->blk, rc);
		journal_replay_finish(rctx, rc);
	}
}

void
s3_journal_replay(struct s3_journal *journal, uint64_t from_lsn,
		  s3_journal_apply_cb apply_fn, void *apply_arg,
		  s3_journal_cb done_fn, void *done_arg)
{
	struct journal_replay_ctx *rctx;

	if (!journal || !apply_fn) {
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

	rctx->buf = spdk_dma_zmalloc(S3_JOURNAL_BLOCK_SIZE, 4096, NULL);
	if (!rctx->buf) {
		free(rctx);
		if (done_fn) {
			done_fn(done_arg, -ENOMEM);
		}
		return;
	}

	rctx->journal      = journal;
	rctx->from_lsn     = from_lsn;
	rctx->apply_fn     = apply_fn;
	rctx->apply_arg    = apply_arg;
	rctx->done_fn      = done_fn;
	rctx->done_arg     = done_arg;
	rctx->max_lsn      = from_lsn;
	rctx->total_blocks = journal_blocks_total(journal);

	/* No appends during replay -- it moves cur_block and block_buf. */
	journal->busy = true;

	journal_replay_read_next(rctx);
}

/* ==========================================================================
 * Truncation
 * ========================================================================== */

void
s3_journal_truncate(struct s3_journal *journal, uint64_t lsn)
{
	if (!journal) {
		return;
	}

	/* Truncation merely records "nothing before this LSN is needed any
	 * more"; it does not erase anything. Erasing hundreds of MiB would be
	 * pointless I/O, since replay skips those records via from_lsn anyway.
	 * Physical space is reclaimed when the ring wraps. Hence no I/O here,
	 * which is why this one is synchronous. */
	journal->truncated_lsn = lsn;

	SPDK_DEBUGLOG(s3_journal, "Journal truncated to lsn=%" PRIu64 "\n", lsn);
}

/* ==========================================================================
 * Accessors
 * ========================================================================== */

uint64_t
s3_journal_get_used_bytes(struct s3_journal *journal)
{
	if (!journal) {
		return 0;
	}
	return journal->cur_block * S3_JOURNAL_BLOCK_SIZE +
	       journal->cur_count * sizeof(struct s3_journal_record);
}

uint64_t
s3_journal_get_capacity_bytes(struct s3_journal *journal)
{
	return journal ? journal->region_size : 0;
}

uint64_t
s3_journal_get_next_lsn(struct s3_journal *journal)
{
	return journal ? journal->next_lsn : 0;
}

SPDK_LOG_REGISTER_COMPONENT(s3_journal)
