/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * Process-shared write-back ring.
 *
 * One owner thread in one process may use a ring at a time.  The module has no
 * internal locking; attach rejects a live owner PID where practical.  Records
 * and the mapped header contain offsets and scalar values only, never pointers.
 */

#ifndef S3LVOL_WRITEBACK_H
#define S3LVOL_WRITEBACK_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define S3_WRITEBACK_DEFAULT_CAPACITY (UINT64_C(4) * 1024 * 1024 * 1024)
#define S3_WRITEBACK_DEFAULT_ALIGNMENT 64u
#define S3_WRITEBACK_SHM_NAME_MAX      64u
#define S3_WRITEBACK_INSTANCE_ID_SIZE  16u

enum s3_writeback_record_type {
	S3_WRITEBACK_WRITE = 1,
	S3_WRITEBACK_ZEROES = 2,
	S3_WRITEBACK_UNMAP = 3,
};

enum s3_writeback_open_flags {
	/* Create and initialise the object if it does not already exist. */
	S3_WRITEBACK_OPEN_CREATE = 1u << 0,
};

struct s3_writeback_opts {
	const char *lvstore_name;
	/* Zero selects S3_WRITEBACK_DEFAULT_CAPACITY. */
	uint64_t capacity_bytes;
	/* Zero selects S3_WRITEBACK_DEFAULT_ALIGNMENT. Must be a power of two
	 * from 64 through 4096. Payloads start at this alignment. */
	uint32_t record_alignment;
	/* Optional persistent lvstore incarnation (normally its UUID). */
	const uint8_t *instance_id;
	uint32_t flags;
};

struct s3_writeback;

struct s3_writeback_record {
	/* Opaque logical ring position used for O(1) completion. */
	uint64_t position;
	uint64_t seq;
	/* WAL sequence assigned after enqueue; zero until the WAL accepts it. */
	uint64_t wal_seq;
	uint64_t lba;
	uint64_t chunk_index;
	uint32_t nblocks;
	uint32_t payload_len;
	uint32_t crc32c;
	int32_t outcome;
	enum s3_writeback_record_type type;
	const void *payload;
	bool completed;
};

struct s3_writeback_iter {
	uint64_t position;
};

struct s3_writeback_stats {
	uint64_t capacity_bytes;
	uint64_t occupied_bytes;
	uint64_t free_bytes;
	uint64_t pending_records;
	uint64_t completed_records;
	uint64_t publishes;
	uint64_t full_events;
	uint64_t next_seq;
	uint64_t global_reserved_bytes;
	uint64_t global_limit_bytes;
	uint32_t global_active_rings;
};

/* Derive the deterministic object leaf (with a leading slash) used below the
 * current UID's private /dev/shm directory. */
int s3_writeback_shm_name(const char *lvstore_name, char *name, size_t name_size);

/*
 * Create or attach the ring and claim its process owner slot.
 *
 * Existing incompatible or corrupt mappings are rejected, never reformatted.
 * capacity_bytes and record_alignment must match when non-zero on reopen.
 */
int s3_writeback_open(const struct s3_writeback_opts *opts,
		      struct s3_writeback **out);

/*
 * Release this process's mapping. This never unlinks the shm object, even when
 * empty; crash recovery therefore does not depend on graceful shutdown.
 */
void s3_writeback_close(struct s3_writeback *writeback);

/*
 * Unlink an existing ring only if it is valid, empty, and has no live owner.
 * A stale owner PID left by a crashed process is accepted.
 */
int s3_writeback_cleanup(const char *lvstore_name);

/*
 * Permanently discard a ring, including pending recovery records.
 *
 * This is only for deleting the lvstore itself. The object is validated,
 * matched to the expected lvstore incarnation, and locked first, so a live
 * owner or a newer same-name lvstore is never unlinked. Ordinary unload and
 * error cleanup must use s3_writeback_cleanup() and preserve non-empty rings.
 */
int s3_writeback_discard(const char *lvstore_name,
			 const uint8_t *instance_id);

/*
 * Copy and checksum a record before release-publishing it. WRITE requires a
 * non-NULL payload and payload_len; ZEROES and UNMAP require neither.
 * Returns -ENOSPC when the complete aligned record (and wrap padding) does not
 * fit. A successful return makes out_seq durable in the shm mapping.
 */
int s3_writeback_publish(struct s3_writeback *writeback,
			 enum s3_writeback_record_type type,
			 uint64_t lba, uint32_t nblocks,
			 const void *payload, uint32_t payload_len,
			 uint64_t chunk_index, uint64_t *out_seq,
			 struct s3_writeback_record *out_record);

/* Iterate non-completed records in sequence order. The returned payload points
 * into the mapping and remains valid until the record is reclaimed or close. */
void s3_writeback_iter_begin(const struct s3_writeback *writeback,
			     struct s3_writeback_iter *iter);
int s3_writeback_iter_next(const struct s3_writeback *writeback,
			   struct s3_writeback_iter *iter,
			   struct s3_writeback_record *record);

/* O(1) completion for records returned by publish or iteration. */
int s3_writeback_mark_record_complete(struct s3_writeback *writeback,
				      const struct s3_writeback_record *record,
				      int32_t outcome);

/* Persist the WAL identity before the corresponding host write is acknowledged. */
int s3_writeback_set_wal_seq(struct s3_writeback *writeback,
			     const struct s3_writeback_record *record,
			     uint64_t wal_seq);

/* Reclaim only the contiguous completed prefix. Returns records reclaimed. */
uint64_t s3_writeback_reclaim(struct s3_writeback *writeback);

int s3_writeback_get_stats(const struct s3_writeback *writeback,
			   struct s3_writeback_stats *stats);

/* Whether ring occupancy warrants forcing held-back partial chunks out of the
 * overlay so contiguous completed records can be reclaimed. */
bool s3_writeback_should_force_flush(const struct s3_writeback *writeback);

/* Collect distinct chunks referenced by at most max_records of the oldest
 * pending records, in ring order. Payload CRCs are not rescanned:
 * publish/open already validated them. */
int s3_writeback_collect_head_chunks(const struct s3_writeback *writeback,
				     uint64_t *chunks, uint32_t max_chunks,
				     uint32_t max_records, uint32_t *out_count);

/* Bind an initially-zero identity before the lvstore becomes externally
 * writable. Blobstore initialization may already have published metadata. */
int s3_writeback_bind_instance(struct s3_writeback *writeback,
			       const uint8_t *instance_id);

#endif /* S3LVOL_WRITEBACK_H */
