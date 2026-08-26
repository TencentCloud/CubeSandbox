/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_wal -- write-ahead log on the local bdev 
 *
 *   === Why this exists ===
 *
 *   Two independent reasons, and the second one was only discovered by
 *   measurement:
 *
 *   1. Performance (the original motivation): writing straight to S3 turns
 *      every 4 KiB metadata write into a 1 MiB GET plus a 1 MiB PUT.
 *   2. *Correctness*: the direct-to-S3 path loses data under concurrent
 *      partial-chunk writes. chunk_size is 1 MiB, nvmf splits
 *      a 1 MiB write into eight concurrent 128 KiB commands, and all eight run
 *      a read-modify-write cycle against a chunk that does not exist yet. Each
 *      zero-fills a buffer, fills in only its own 128 KiB and PUTs a new
 *      object, so seven eighths of the data is lost. A classic lost update.
 *
 *   The WAL removes the race by construction rather than by locking: user
 *   writes land in the log and are acknowledged from there, and the flusher
 *   later performs *one* read-modify-write per chunk. Concurrent writes to one
 *   chunk are coalesced instead of racing.
 *
 *   === Durability contract ===
 *
 *   A write is acknowledged only once its batch is durable on the local bdev.
 *   After a crash, replaying the WAL restores every acknowledged write (INV1,
 *   W3). An unacknowledged batch may be lost *in its entirety*; a half-applied
 *   batch must never become visible (W4).
 *
 *   === Disk layout ===
 *
 *     +--------+-----------+-----------+ ... +-----------+------------+
 *     | super| segment 0 | segment 1 |     | segment K | tail slack |
 *     | 8 KiB  | seg_size  | seg_size  |     | seg_size  | 2*seg_size |
 *     +--------+-----------+-----------+ ... +-----------+------------+
 *
 *   The super occupies 8 KiB as two 4 KiB A/B slots, written alternately; the
 *   newer valid slot wins on open. A single slot would leave a window where a
 *   crash during the super update loses the whole log.
 *
 *   Segments are the *reclamation* unit: the ring advances a whole segment at a
 *   time, never byte by byte. That keeps truncation trivially safe -- a segment
 *   is either entirely consumed by the flusher or it is not -- and avoids tearing
 *   at the reuse boundary. The tail slack exists so a batch never has to be
 *   split across the end of the region.
 *
 *   === Alignment ===
 *
 *   Entries are variable length and deliberately *not* block aligned; alignment
 *   is a property of the batch. The five invariants  are:
 *
 *     I1 the WAL region base is 4 KiB aligned (guaranteed by the formatter)
 *     I2 seg_size is a multiple of 4 KiB (validated here)
 *     I3 head always lands on a 4 KiB boundary after a batch
 *     I4 every batch buffer length is a multiple of 4 KiB (pad, plus an END
 *        sentinel when there is room for a header)
 *     I5 every batch buffer address is 4 KiB aligned (spdk_dma_malloc)
 *
 *   With all five holding, the bdev only ever sees aligned I/O and never sees
 *   entry boundaries, so O_DIRECT and NVMe PRP requirements are satisfied
 *   without the caller having to align anything.
 *
 *   === Threading ===
 *
 *   Same contract as the journal and the chunk map: one WAL belongs to one
 *   lvstore and is only touched on that lvstore's owner thread. No internal
 *   locking. Callers arriving on another thread must bounce first -- see the
 *   threading note in s3_bs_dev.c for why that is mandatory rather than
 *   optional.
 */

#ifndef S3LVOL_WAL_H
#define S3LVOL_WAL_H

#include "spdk/stdinc.h"
#include "spdk/assert.h"

#include "s3lvol/s3_local_dev.h"

/* Block size every WAL offset and length is expressed in multiples of. */
#define S3_WAL_BLOCK_SIZE 4096

/* Two 4 KiB A/B slots. */
#define S3_WAL_SUPER_SIZE (2 * S3_WAL_BLOCK_SIZE)
#define S3_WAL_SUPER_SLOT_SIZE S3_WAL_BLOCK_SIZE

#define S3_WAL_SUPER_MAGIC   0x5341573153444B53ULL  /* "SKDS1WAS" little endian */
#define S3_WAL_SUPER_VERSION 1

#define S3_WAL_ENTRY_MAGIC 0x454C4157u  /* 'W','A','L','E' little endian */

/* Defaults. seg_size must stay a multiple of S3_WAL_BLOCK_SIZE (I2). */
#define S3_WAL_DEFAULT_SEG_SIZE (16 * 1024 * 1024)

/* Batch triggers . Whichever fires first assembles the next batch. */
#define S3_WAL_BATCH_TARGET_BYTES (128 * 1024)
#define S3_WAL_BATCH_MAX_OPS      64
#define S3_WAL_BATCH_MAX_US       200

/* Upper bound on one batch, and therefore on the buffer allocated up front.
 * A single op may not exceed this either. */
#define S3_WAL_BATCH_BUF_SIZE (1024 * 1024)

/* Largest payload a single append may carry: the batch buffer minus one entry
 * header, rounded down to a block.
 *
 * Callers have to split against this themselves. It is deliberately not hidden
 * inside the WAL: a chunk-sized write (1 MiB) does *not* fit, so the split has
 * to happen where the LBA range is still known, and silently accepting an
 * oversized append would fail the write at submit time instead. */
#define S3_WAL_MAX_PAYLOAD \
	(((S3_WAL_BATCH_BUF_SIZE - 64) / S3_WAL_BLOCK_SIZE) * S3_WAL_BLOCK_SIZE)

/* Backpressure thresholds as a percentage of usable capacity (W5). */
#define S3_WAL_BACKPRESSURE_ON_PCT  85
#define S3_WAL_BACKPRESSURE_OFF_PCT 65

enum s3_wal_entry_type {
	S3_WAL_WRITE        = 0,
	S3_WAL_WRITE_ZEROES = 1,
	S3_WAL_UNMAP        = 2,
	S3_WAL_BARRIER      = 3,
	S3_WAL_END          = 0xFF,   /* sentinel filling out a segment */
};

enum s3_wal_entry_flags {
	S3_WAL_F_COMPRESSED     = 1u << 0,
	S3_WAL_F_LAST_IN_BATCH = 1u << 1,
};

/* On-disk entry header. Exactly 64 bytes; the payload follows immediately.
 *
 * *Field order and size are frozen once shipped* -- existing logs have to stay
 * readable. New fields come out of the reserved area. */
struct s3_wal_entry_hdr {
	uint32_t magic;         /* S3_WAL_ENTRY_MAGIC */
	uint8_t  type;          /* enum s3_wal_entry_type */
	uint8_t  flags;         /* enum s3_wal_entry_flags */
	uint16_t hdr_len;       /* header length including padding */

	uint64_t seq;           /* epoch << 48 | local, monotonic, never reset */
	uint64_t batch_id;      /* shared by every entry of one batch */

	uint64_t lba;           /* start LBA, 4 KiB granularity */
	uint32_t nblocks;       /* number of 4 KiB blocks */
	uint32_t payload_len;   /* body bytes, excluding header padding */

	uint64_t chunk_index;   /* precomputed so the flusher does not have to */

	uint32_t crc_hdr;       /* covers this header with crc_hdr itself as 0 */
	uint32_t crc_payload;   /* covers payload_len bytes of body */

	uint8_t  reserved[8];   /* pads to 64; take new fields from here */
} __attribute__((packed));

SPDK_STATIC_ASSERT(sizeof(struct s3_wal_entry_hdr) == 64,
		   "WAL entry header must stay 64 bytes");

/* On-disk super block, one per A/B slot. */
struct s3_wal_super {
	uint64_t magic;
	uint32_t version;
	uint32_t seg_size;

	uint32_t seg_count;
	uint32_t reserved0;

	/* Bumped on every open so a stale log cannot be confused with a fresh
	 * one. It is also the high 16 bits of every seq. */
	uint64_t epoch;

	/* Ring positions, relative to the first segment, in bytes. */
	uint64_t head;          /* where the next append goes */
	uint64_t tail;          /* everything before this is consumed */

	/* Position and seq recorded by the last checkpoint. Replay starts here. */
	uint64_t ckpt_head;
	uint64_t ckpt_seq;

	uint64_t seq_next;      /* next seq to hand out */

	/* Monotonic counter identifying which slot is newer. */
	uint64_t slot_gen;

	uint32_t crc;           /* covers this struct with crc as 0 */
	uint32_t reserved1;
} __attribute__((packed));

SPDK_STATIC_ASSERT(sizeof(struct s3_wal_super) <= S3_WAL_SUPER_SLOT_SIZE,
		   "WAL super must fit in one 4 KiB slot");

struct s3_wal;

typedef void (*s3_wal_cb)(void *cb_arg, int status);
typedef void (*s3_wal_open_cb)(void *cb_arg, struct s3_wal *wal, int status);

struct s3_wal_opts {
	/* 0 selects S3_WAL_DEFAULT_SEG_SIZE. Must be a multiple of 4 KiB. */
	uint32_t seg_size;
};

/**
 * Format a fresh WAL in the local bdev's WAL region.
 *
 * Writes both super slots and zeroes the head of the first segment so a later
 * scan can tell "no entries" from "unreadable". *Destroys any existing log.*
 */
void s3_wal_create(struct s3_local_dev *local_dev, const struct s3_wal_opts *opts,
		   s3_wal_open_cb cb_fn, void *cb_arg);

/**
 * Open an existing WAL.
 *
 * Reads both super slots and takes the newer valid one. Does *not* replay:
 * call s3_wal_replay() next, which also positions the append cursor.
 *
 * status is -EILSEQ when neither slot is valid, and -EPROTO on a version this
 * build does not understand.
 */
void s3_wal_open(struct s3_local_dev *local_dev, s3_wal_open_cb cb_fn, void *cb_arg);

/**
 * Flush anything still pending, persist the super and release the WAL.
 *
 * *The caller must have drained all appends* -- no callback may still be
 * outstanding. Asserts otherwise, for the same use-after-free reason as
 * s3_journal_destroy().
 */
void s3_wal_close(struct s3_wal *wal, s3_wal_cb cb_fn, void *cb_arg);

/**
 * Append a data write.
 *
 * \c payload must hold nblocks * 4096 bytes and is copied into the batch
 * buffer, so it does not need to stay alive and does not need to be aligned.
 * Copying is what decouples caller alignment from the bdev's requirements.
 * \c payload_len may not exceed S3_WAL_MAX_PAYLOAD.
 *
 * *The write is durable when the callback fires*, not when this returns.
 *
 * \param out_seq Optional. Receives the seq assigned to this entry, *before the
 *callback fires*. Whoever tracks how far the log has been
 *                consumed needs it, and it cannot be recovered afterwards.
 *
 * The callback may run before this function returns when submission fails
 * outright. status is -EAGAIN while the WAL is applying backpressure: the
 * caller must retry rather than treat it as an error (W5).
 */
void s3_wal_append_write(struct s3_wal *wal, uint64_t lba, uint32_t nblocks,
			 const void *payload, uint64_t chunk_index,
			 uint64_t *out_seq, s3_wal_cb cb_fn, void *cb_arg);

/**
 * Append a metadata-only entry. WRITE_ZEROES and UNMAP carry no payload.
 */
void s3_wal_append_zeroes(struct s3_wal *wal, uint64_t lba, uint32_t nblocks,
			  uint64_t chunk_index, uint64_t *out_seq,
			  s3_wal_cb cb_fn, void *cb_arg);

void s3_wal_append_unmap(struct s3_wal *wal, uint64_t lba, uint32_t nblocks,
			 uint64_t chunk_index, uint64_t *out_seq,
			 s3_wal_cb cb_fn, void *cb_arg);

/**
 * Append a checkpoint barrier and report the seq it was given.
 *
 * Recovery uses the barrier to pin down "everything before this was already
 * checkpointed", which makes the replay window precise instead of conservative.
 */
void s3_wal_append_barrier(struct s3_wal *wal, uint64_t *out_seq,
			   s3_wal_cb cb_fn, void *cb_arg);

/**
 * Replay from the last checkpoint position.
 *
 * \c apply_fn is called once per accepted entry, in seq order, synchronously.
 * Returning non-zero stops the replay and is reported through \c done_fn.
 *
 * Only *closed* batches are accepted: a batch is applied only if every one of
 * its entries verified and the last one carries S3_WAL_F_LAST_IN_BATCH. A
 * trailing unclosed batch is discarded whole, which is what gives W4. Payload
 * is NULL for entries that carry none.
 *
 * Also positions head and seq_next, so appends may start right afterwards.
 */
typedef int (*s3_wal_replay_cb)(void *cb_arg, const struct s3_wal_entry_hdr *hdr,
				const void *payload);

void s3_wal_replay(struct s3_wal *wal, s3_wal_replay_cb apply_fn, void *apply_arg,
		   s3_wal_cb done_fn, void *done_arg);

/**
 * Release every segment whose entries all have seq <= \c safe_seq.
 *
 * Called by the flusher once it has pushed those writes to S3. Advances tail a
 * whole segment at a time, which is the only granularity at which reuse is
 * safe, and moves the replay start position with it. Does no I/O: the new
 * positions reach the disk with the next s3_wal_sync_super().
 *
 * *The caller must persist the super before relying on the space being reusable
 * across a crash*, otherwise recovery would still scan from the old position,
 * which by then holds bytes from a later lap.
 */
void s3_wal_truncate_to_seq(struct s3_wal *wal, uint64_t safe_seq);

/**
 * Persist the current ring positions into the super (alternating A/B slots).
 *
 * Worth calling after a checkpoint so recovery does not have to rescan from an
 * ancient position.
 */
void s3_wal_sync_super(struct s3_wal *wal, uint64_t ckpt_seq,
		       s3_wal_cb cb_fn, void *cb_arg);

/* Backpressure is on: callers should stop submitting and retry later (W5). */
bool s3_wal_is_backpressured(const struct s3_wal *wal);

uint64_t s3_wal_get_used_bytes(const struct s3_wal *wal);
uint64_t s3_wal_get_next_seq(const struct s3_wal *wal);
uint64_t s3_wal_get_epoch(const struct s3_wal *wal);

struct s3_wal_stats {
	uint64_t appends;
	uint64_t batches;
	uint64_t bytes_written;
	uint64_t backpressure_events;
	uint64_t replayed_entries;
	uint64_t dropped_batches;
	uint64_t segments_released;
};

void s3_wal_get_stats(const struct s3_wal *wal, struct s3_wal_stats *out);

#endif /* S3LVOL_WAL_H */
