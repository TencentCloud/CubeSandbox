/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_journal -- change log for the chunk map
 *
 *   === This is a change log, not a periodic flush ===
 *
 *   The rule is explicit: *every* modification of the chunk map is appended to
 *   the journal immediately, and the journal is durable as soon as it hits the
 *   local bdev. It is not a batch that gets flushed later.
 *
 *   That means journal write latency lands directly in the user write path
 *   (chunk map changes happen after the flusher completes its PUT), so this
 *   has to be a sequential append with as few I/Os as possible.
 *
 *   === Why it is needed at all ===
 *
 *   The chunk map maps chunk_index -> S3 object uuid, and the uuid is freshly
 *   generated on every write-new-then-swap (create-once, P2), so it *cannot be
 *   recomputed*. Losing the map means every object in S3 becomes an orphan:
 *   the data is still there, but nothing knows which object backs which LBA
 *   range.
 *
 *   This is exactly the hard limitation left over from the early design: the
 *   chunk map was memory-only, so stopping the process lost the data.
 *
 *   === Division of labour with checkpoints ===
 *
 *   The journal records *increments* and grows without bound, so it needs a
 *   periodic checkpoint that snapshots the whole table to S3, after which the
 *   journal can be truncated up to the LSN the snapshot covers.
 *
 *     journal     written on every change, local bdev, per-record durability
 *     checkpoint  triggered by 60 s / 128 MiB of journal / 100k changes,
 *                 stored in S3, full-table snapshot
 *
 *   Recovery order : rebuild the table from the checkpoint, then replay
 *   the journal records whose LSN is greater than checkpoint_lsn.
 *
 *   === Fully asynchronous: there is no synchronous API, and none should be
 *       added back ===
 *
 *   Everything that touches the bdev is callback-based. *There used to be a
 *   synchronous implementation* (submit via spdk_bdev_write() then spin in
 *   place on `while (!done) spdk_thread_poll()`); it has been deleted.
 *
 *   It was not deleted because it was slow. It was deleted because *nesting
 *   spdk_thread_poll() corrupts the spdk_thread state machine*:
 *
 *   The journal is written from inside a chunk map change, and chunk map
 *   changes are triggered by S3 PUT completions (s3_chunk_write_done() in
 *   s3_bs_dev.c). Those have already bounced onto the owner thread, which in
 *   production is a reactor message callback. Calling spdk_thread_poll() there
 *   makes the reactor re-enter itself:
 *
 *     - The outer frame is iterating active_pollers; the inner poll runs that
 *       same set again. A poller's state is flipped to RUNNING by the inner
 *       pass, and when the outer pass resumes it hits `default: assert(false)`
 *       in thread_execute_poller(). Release builds are compiled with -DNDEBUG
 *       (spdk.common.mk:297), so the assert disappears and it becomes silent
 *       state-machine corruption.
 *     - The inner poll also drains *other* messages, including other S3
 *       completions, which call back into the chunk map and nest another level.
 *       Stack depth ends up proportional to the number of concurrent I/Os.
 *     - Completion on the local bdev itself depends on a poller (aio relies on
 *       bdev_aio_group_poll). If the nesting happens inside that poller's own
 *       call stack it is already RUNNING, so the inner pass cannot run it --
 *       the completion never arrives and the `while` spins forever.
 *
 *   Unit tests did not catch this because a unit test has a single thread, no
 *   other pollers, and calls in from main's ordinary control flow -- which
 *   happens to be the only place nested polling is safe. *Do not use "the unit
 *   test passes" as evidence that it is safe.*
 *
 *   === Write strategy: batch within a block, one write in flight ===
 *
 *   A record is 64 bytes and a 4 KiB block holds 64 of them. The full contents
 *   of the current block are mirrored in memory, so an append fills a slot and
 *   rewrites the whole block -- having the mirror is what avoids a
 *   read-modify-write.
 *
 *   Only *one block write may be in flight at a time* (write_in_flight). This
 *   is not throttling, it is required for correctness: block_buf is shared
 *   mutable state, so modifying it while a write is in flight means the DMA
 *   reads the post-modification bytes. Worse, if that modification rolled over
 *   to a new block (memset), the in-flight write would push a block of zeroes
 *   and destroy records that were already durable.
 *
 *   Appends that arrive while a write is in flight go on the waiting queue and
 *   are filled into the same block once it completes. So heavier load means
 *   bigger batches, which actually lowers write amplification.
 *
 *   LSNs are assigned *when a record is queued*, not when the callback fires,
 *   so queue order is LSN order and replay can simply scan in physical order.
 *
 *   === Threading ===
 *
 *   Same as the chunk map: one journal belongs to one lvstore and is only
 *   touched on that lvstore's owner thread. No internal locking.
 */

#ifndef S3LVOL_JOURNAL_H
#define S3LVOL_JOURNAL_H

#include "spdk/stdinc.h"
#include "spdk/assert.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_local_dev.h"

/* Journal record types. *Values must never change once shipped* -- records on
 * existing disks have to stay recognizable. */
enum s3_journal_op {
	/* A chunk is bound to a new object. Written after the flusher's PUT
	 * completes, which is what establishes INV2. */
	S3_JOURNAL_OP_CHUNK_UPDATE = 1,

	/* A chunk is unbound (unmap, or a write_zeroes covering a whole
	 * chunk). */
	S3_JOURNAL_OP_CHUNK_REMOVE = 2,

	/* Marks the completion of a checkpoint. Seeing one during replay means
	 * everything before it is covered by the snapshot -- redundant
	 * insurance, since the authoritative value is checkpoint_lsn in the
	 * super block. */
	S3_JOURNAL_OP_CHECKPOINT   = 3,
};

/* A single journal record. Fixed at 64 bytes so a sequential scan never has to
 * parse a length first.
 *
 * The cost of a fixed size is some waste (CHUNK_REMOVE does not use uuid); the
 * benefit is that replay stays simple and one corrupt record does not make
 * every following record unparseable. For a structure that gets replayed often,
 * robustness matters more than density.
 *
 * *Landing on exactly 64 bytes is deliberate*: 4096 / 64 = 64 divides evenly,
 * so a record never straddles a block boundary and a failed block write only
 * damages itself. The fields add up to 56 bytes, hence the explicit 8 bytes of
 * padding -- take new fields out of the padding rather than growing the total.
 */
struct s3_journal_record {
	uint64_t         lsn;           /* monotonic; the ordering key for replay */
	uint32_t         op;            /* enum s3_journal_op */
	uint32_t         valid_bytes;   /* CHUNK_UPDATE: valid bytes within the chunk */

	uint64_t         chunk_index;
	uint64_t         gen;           /* chunk generation, for diagnostics */

	struct spdk_uuid uuid;          /* CHUNK_UPDATE: uuid of the new object */

	uint32_t         flags;         /* chunk state: IN_S3 / DIRTY_LOCAL / ... */
	uint32_t         crc;           /* covers the rest of the record, itself as 0 */

	uint8_t          pad[8];        /* pads to 64; take new fields from here */
} __attribute__((packed));

SPDK_STATIC_ASSERT(sizeof(struct s3_journal_record) == 64,
		   "journal record must stay 64 bytes so 4096 divides evenly");

/* How many records fit in a 4 KiB block. The journal writes whole blocks and a
 * record never straddles a boundary, so a failed block write only affects that
 * block. */
#define S3_JOURNAL_RECORDS_PER_BLOCK (4096 / sizeof(struct s3_journal_record))

struct s3_journal;

typedef void (*s3_journal_cb)(void *cb_arg, int status);

typedef void (*s3_journal_create_cb)(void *cb_arg, struct s3_journal *journal,
				     int status);

/**
 * Create (format) a fresh journal in the local bdev's journal region.
 *
 * Zeroes the head of the region so a later scan can correctly recognize "no
 * records here". Asynchronous: when the callback fires the head is durable and
 * \c journal is usable (NULL on failure).
 */
void s3_journal_create(struct s3_local_dev *local_dev,
		       s3_journal_create_cb cb_fn, void *cb_arg);

/**
 * Open an existing journal in preparation for replay.
 *
 * *This one is synchronous because it issues no I/O* -- it only reads
 * checkpoint_lsn from the super block that local_dev already holds in memory.
 * The actual disk scan happens in s3_journal_replay().
 */
int s3_journal_open(struct s3_local_dev *local_dev, struct s3_journal **out);

/**
 * Release a journal.
 *
 * *The caller must drain first*: no append may still be outstanding (callback
 * not yet fired). Otherwise the completion of an in-flight bdev write touches
 * freed memory. Asserts when anything is pending.
 */
void s3_journal_destroy(struct s3_journal *journal);

/**
 * Append a chunk update record.
 *
 * *The record is durable once the callback fires.* The caller (the chunk map
 * write path) must wait for it before treating the mapping as persisted --
 * otherwise a crash leaves the freshly written S3 object orphaned.
 *
 * The callback *may run before this function returns* (when submission itself
 * fails). Callers must tolerate that and must not touch context that the
 * callback may already have freed.
 */
/**
 * \param out_lsn  Optional; receives the record's LSN *immediately*, at queue
 *                 time, before this function returns. The chunk map keeps it so
 *                 that it can record which LSN its committed state includes,
 *                 which is the number a checkpoint has to be stamped with.
 */
void s3_journal_append_update(struct s3_journal *journal,
			      uint64_t chunk_index,
			      const struct spdk_uuid *uuid,
			      uint32_t valid_bytes, uint64_t gen, uint32_t flags,
			      uint64_t *out_lsn,
			      s3_journal_cb cb_fn, void *cb_arg);

/**
 * Append a chunk removal record. Same semantics as above.
 */
void s3_journal_append_remove(struct s3_journal *journal, uint64_t chunk_index,
			      uint64_t *out_lsn,
			      s3_journal_cb cb_fn, void *cb_arg);

/**
 * Replay every record with LSN > \c from_lsn.
 *
 * \c apply_fn is invoked once per valid record (synchronously; the caller
 * applies it to the chunk map). A record with a bad CRC *stops* replay and is
 * reported as success -- that normally means the process crashed mid-write, so
 * nothing after it can be trusted. What was replayed up to that point is
 * complete and consistent.
 *
 * Replay also positions the append cursor, so appends may start immediately
 * afterwards.
 *
 * Asynchronous: the result is delivered through \c done_fn.
 */
typedef int (*s3_journal_apply_cb)(void *cb_arg,
				   const struct s3_journal_record *rec);

void s3_journal_replay(struct s3_journal *journal, uint64_t from_lsn,
		       s3_journal_apply_cb apply_fn, void *apply_arg,
		       s3_journal_cb done_fn, void *done_arg);

/**
 * Truncate up to the given LSN (called once a checkpoint completes).
 *
 * Afterwards s3_journal_replay(from_lsn = lsn) will not see earlier records.
 *
 * *Issues no I/O*, hence synchronous: truncation just records "nothing before
 * this LSN is needed any more". Physical space is reclaimed when the ring
 * wraps.
 */
void s3_journal_truncate(struct s3_journal *journal, uint64_t lsn);

/**
 * Bytes currently in use, i.e. how much a checkpoint would reclaim.
 *
 * The checkpoint trigger is a *fraction of capacity* rather than an absolute
 * threshold -- see s3_journal_get_capacity_bytes().
 */
uint64_t s3_journal_get_used_bytes(struct s3_journal *journal);

/**
 * Total size of the journal region.
 *
 * Exists so the checkpoint trigger can be a percentage of it. An absolute
 * threshold does not work: the region is sized per-disk (256 MiB by default, but
 * a test uses 16), and a threshold larger than the region means the ring wraps
 * before a checkpoint is ever triggered -- at which point appends start failing
 * with -ENOSPC because there is no snapshot to fall back on.
 */
uint64_t s3_journal_get_capacity_bytes(struct s3_journal *journal);

/**
 * The LSN the next record will use.
 *
 * Note that LSNs are assigned when a record is *queued*, so this includes
 * records that are submitted but whose callbacks have not fired yet. It answers
 * "what will the next record get", not "how far has durability reached".
 */
uint64_t s3_journal_get_next_lsn(struct s3_journal *journal);

#endif /* S3LVOL_JOURNAL_H */
