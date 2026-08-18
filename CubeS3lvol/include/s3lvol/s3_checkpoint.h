/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

/**
 * \file
 * s3_checkpoint -- a full snapshot of the chunk map
 *
 * === Why it must exist ===
 *
 * The journal records **deltas** and only ever grows. Without a checkpoint,
 * once it fills up the only option is to refuse writes (`journal_kick()`
 * returns `-ENOSPC` -- **deliberately**: never silently overwrite records not
 * covered by a snapshot). In other words, without this module any lvstore that
 * receives writes long enough becomes read-only.
 *
 * A checkpoint snapshots the whole table to S3, after which the journal can be
 * truncated to the LSN the snapshot covers, and its space is reused on ring
 * wraparound.
 *
 *   journal     written on every change, local disk, persisted record by record
 *   checkpoint  triggered by journal usage ratio, stored on S3, full-table
 *               snapshot
 *
 * === Recovery order ===
 *
 * **Rebuild the whole table from the snapshot first, then replay the journal
 * records with LSN > checkpoint_lsn.** Getting the order wrong, or skipping
 * the first step, permanently loses every mapping <= checkpoint_lsn -- they
 * are no longer in the journal (truncated), only in the snapshot.
 *
 * === Which LSN a snapshot covers: only applied_lsn will do ===
 *
 * The snapshot must be stamped with `s3_chunk_map_get_applied_lsn()`, **not**
 * the journal's `next_lsn`. The latter includes records that were queued but
 * may never have landed on disk; stamping and truncating with it would erase
 * mappings the snapshot never captured, turning live objects into orphans.
 *
 * applied_lsn only advances on commit, so "the snapshot covers up to
 * applied_lsn" holds by construction.
 *
 * === Crash consistency: the three steps cannot be reordered ===
 *
 *   1. PUT the snapshot to S3
 *   2. update the super block's checkpoint_gen / checkpoint_lsn
 *   3. only then `s3_journal_truncate()`
 *
 * A crash between any two steps may only cause "**more** replay", never
 * "less":
 *
 *   crash after 1: the super block still has the old lsn, replay from the old
 *                  point. The snapshot was written for nothing; harmless.
 *   crash after 2: the journal was not truncated, all records remain, replay
 *                  from the new point; correct.
 *   crash after 3: normal.
 *
 * The reverse (truncate before PUT) loses mappings on any crash.
 *
 * === Single-flight ===
 *
 * Only one checkpoint may be in progress at a time. This is not rate limiting:
 * two concurrent checkpoints PUT to the same key, and whichever lands last is
 * undecided, while the lsn in the super block may correspond to the other one
 * -- the journal then gets truncated to a point its snapshot does not cover.
 */

#ifndef S3LVOL_S3_CHECKPOINT_H
#define S3LVOL_S3_CHECKPOINT_H

#include "spdk/stdinc.h"
#include "spdk/assert.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_client.h"

#ifdef __cplusplus
extern "C" {
#endif

struct s3_chunk_map;

/* "S3LVCKPT" */
#define S3_CKPT_MAGIC       0x53334C56434B5054ULL
#define S3_CKPT_VERSION     1

/* On-disk (in-S3) header. Field order must not change once shipped; append at
 * the tail and bump the version. */
struct s3_ckpt_header {
	uint64_t         magic;
	uint32_t         version;

	/* Geometry this snapshot was taken with. Loading it against a different
	 * chunk_size would silently reinterpret every chunk_index. */
	uint32_t         chunk_size;

	/* Which lvstore this snapshot belongs to. All-zero is tolerated: a create
	 * that died before recording its uuid leaves that. */
	struct spdk_uuid lvs_uuid;

	/* The LSN this snapshot covers, i.e. the chunk map's applied_lsn at the
	 * moment it was serialised. Journal replay resumes from here. */
	uint64_t         checkpoint_lsn;

	/* Monotonic checkpoint counter, for diagnostics and for spotting a
	 * snapshot older than the super block claims. */
	uint64_t         gen;

	uint64_t         num_entries;

	/* CRC32C over the entry array. One CRC for the whole array rather than
	 * per entry: the object is read in one go, so per-entry CRCs would cost
	 * 4 bytes each and buy nothing -- a partial object is useless either
	 * way. */
	uint32_t         entries_crc;

	/* CRC32C over this header, itself counted as zero. */
	uint32_t         crc;
} __attribute__((packed));

SPDK_STATIC_ASSERT(sizeof(struct s3_ckpt_header) == 64,
		   "checkpoint header must stay 64 bytes");

/* One mapping. gen is carried even though it is only diagnostic today: a future
 * scheme may derive object uuids from (lvs_uuid, chunk_index, gen), and
 * dropping it here would make that impossible to adopt later without a format
 * change. */
struct s3_ckpt_entry {
	uint64_t         chunk_index;
	struct spdk_uuid uuid;
	uint32_t         valid_bytes;
	uint32_t         flags;
	uint64_t         gen;
} __attribute__((packed));

SPDK_STATIC_ASSERT(sizeof(struct s3_ckpt_entry) == 40,
		   "checkpoint entry must stay 40 bytes");

typedef void (*s3_checkpoint_cb)(void *cb_arg, int status);

/**
 * \param lsn     LSN the loaded snapshot covers; 0 when there was none
 * \param gen     its generation; 0 when there was none
 */
typedef void (*s3_checkpoint_load_cb)(void *cb_arg, uint64_t lsn, uint64_t gen,
				      int status);

/**
 * Snapshot \c map to `<lvs_name>/meta/checkpoint`.
 *
 * *Serialisation is synchronous and happens before this returns*, so the snapshot
 * and \c lsn are a matched pair regardless of what the caller does afterwards.
 * Only the PUT is asynchronous.
 *
 * \c lsn must be s3_chunk_map_get_applied_lsn(map), sampled in the same
 * synchronous stretch as this call. Nothing here can verify that, hence the
 * emphasis.
 *
 * *This does not update the super block and does not truncate the journal.* The
 * caller does both, in that order, after the callback reports success -- see the
 * ordering note at the top of this file.
 */
void s3_checkpoint_save(struct s3_client *client, const char *lvs_name,
			const struct spdk_uuid *lvs_uuid,
			struct s3_chunk_map *map, uint64_t lsn, uint64_t gen,
			s3_checkpoint_cb cb_fn, void *cb_arg);

/**
 * Load the snapshot and apply it to \c map.
 *
 * A missing object is *not an error*: it means no checkpoint has ever completed,
 * which is the normal state of a young lvstore. The callback then reports
 * status 0 with lsn 0, and the caller replays the journal from the beginning.
 *
 * \c map should be empty. Entries are applied with
 * s3_chunk_map_apply_update(), so a non-empty map would be merged into rather
 * than replaced.
 */
void s3_checkpoint_load(struct s3_client *client, const char *lvs_name,
			struct s3_chunk_map *map,
			s3_checkpoint_load_cb cb_fn, void *cb_arg);

/**
 * Delete the snapshot. For destroying an lvstore; not part of normal operation.
 */
void s3_checkpoint_delete(struct s3_client *client, const char *lvs_name,
			  s3_checkpoint_cb cb_fn, void *cb_arg);

#ifdef __cplusplus
}
#endif

#endif /* S3LVOL_S3_CHECKPOINT_H */
