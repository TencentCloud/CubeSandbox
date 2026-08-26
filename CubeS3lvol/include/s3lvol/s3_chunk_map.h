/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Chunk map -- maps LBA ranges to S3 objects
 *
 *   An s3lvol address space is cut into fixed-size chunks, and each chunk backs
 *   one S3 object (key = <prefix>/data/<uuid>). This module owns only the
 *   "chunk_index -> uuid" mapping; it never touches S3 and issues no I/O, which
 *   is what lets it be unit tested without CRT.
 *
 *   === Persistence ===
 *
 *   Once a journal is attached (s3_chunk_map_set_journal), every insert and
 *   remove writes its record to the journal before mutating memory, so the
 *   mapping survives a process restart. Without a journal it degrades to
 *   memory-only, which is still useful for unit tests and for cases that do not
 *   need persistence.
 *
 *   === Threading ===
 *
 *   *No internal locking.* The caller must guarantee that a given map is only
 *   accessed from one SPDK thread. s3_bs_dev satisfies this naturally: one
 *   bs_dev belongs to one lvstore and all its I/O runs on that lvstore's owner
 *   thread (callbacks are already bounced there; see s3_client.h).
 */

#ifndef S3LVOL_CHUNK_MAP_H
#define S3LVOL_CHUNK_MAP_H

#include "spdk/stdinc.h"
#include "spdk/uuid.h"

/* The *only* definition of enum s3_chunk_flags lives here.
 * Do not add a second copy to this file -- there used to be one, and the two
 * disagreed on the bit values (S3_CHUNK_IN_S3 was 1<<0 in one and 1<<1 in the
 * other), which would give the flags written into the journal different
 * meanings in different translation units. */
#include "s3lvol/s3_types.h"

struct s3_chunk_map;
struct s3_journal;

/**
 * Completion callback for insert / remove.
 *
 * \param old_uuid  The object that was superseded, or all-zero when there was
 *                  none (test with spdk_uuid_is_null()). *The caller is
 *                  responsible for deleting it* -- objects are create-once,
 *                  so an old object cannot be modified in place. The
 *                  chunk map never touches S3, so it does not do this for you.
 *
 * When the callback fires: status == 0 means the journal record is *durable*
 * and the in-memory mapping has been updated. status != 0 means neither took
 * effect -- the new object is an orphan and is left to GC.
 */
typedef void (*s3_chunk_map_cb)(void *cb_arg, const struct spdk_uuid *old_uuid,
				int status);

/**
 * Build the S3 key of a chunk object.
 *
 * The single place this format is written down. It is deliberately not private to
 * the bs_dev: an export names these same objects when it hands a snapshot to
 * another node, and GC matches this prefix when deciding what is a data object.
 * Three copies of a format string that must agree is two too many -- and a
 * disagreement would not fail, it would read or delete the wrong key.
 */
void s3_chunk_data_key(const char *prefix, const struct spdk_uuid *uuid,
		char *out, size_t out_len);

/**
 * Create the mapping table.
 *
 * \param total_blocks  total device blocks (as blobstore sees it)
 * \param block_size    block size; must divide chunk_size evenly
 * \param chunk_size    bytes carried by a single S3 object
 *
 * \return 0 on success, negative errno on failure
 */
int s3_chunk_map_create(uint64_t total_blocks, uint32_t block_size,
			uint32_t chunk_size, struct s3_chunk_map **out_map);

/**
 * Release the mapping table.
 *
 * *The caller must drain first*: no insert or remove may still be outstanding
 * (callback not yet fired). When something is pending this asserts and refuses
 * to free, so a journal callback cannot touch freed memory.
 */
void s3_chunk_map_destroy(struct s3_chunk_map *map);

/**
 * Attach a journal, after which every insert / remove appends a journal record
 * before mutating memory.
 *
 * *Without a journal the mapping is memory-only* and is lost when the process
 * exits.
 *
 * The order is deliberate: journal first, memory second. The other way around
 * leaves a crash window where memory was updated but the disk was not, so
 * recovery comes up missing a mapping and the corresponding S3 object becomes
 * an orphan nobody can reach. The reverse case (journal written, memory not
 * updated) merely replays one extra record, which is idempotent and harmless.
 */
void s3_chunk_map_set_journal(struct s3_chunk_map *map, struct s3_journal *journal);

/**
 * Restore one mapping from a replayed journal record.
 *
 * Differs from s3_chunk_map_insert() in that it *does not write the journal*
 * (that is what is being replayed) and does not return the old uuid (there is
 * no GC during replay). Hence it is synchronous.
 */
/**
 * \param lsn  LSN of the journal record being replayed, or 0 for a change that
 *             has no journal record behind it (the WAL's unmap path). A non-zero
 *             value advances the map's applied LSN, which is what a checkpoint
 *             gets stamped with.
 */
int s3_chunk_map_apply_update(struct s3_chunk_map *map, uint64_t chunk_index,
			      const struct spdk_uuid *uuid, uint32_t valid_bytes,
			      uint64_t gen, uint32_t flags, uint64_t lsn);

int s3_chunk_map_apply_remove(struct s3_chunk_map *map, uint64_t chunk_index,
			      uint64_t lsn);

uint64_t s3_chunk_map_get_num_chunks(const struct s3_chunk_map *map);
uint64_t s3_chunk_map_get_allocated(const struct s3_chunk_map *map);

/**
 * Highest journal LSN whose effect is present in the committed state.
 *
 * *This is the LSN a checkpoint must be stamped with.* It is true by
 * construction -- it only moves when a commit happens -- so the snapshot and the
 * LSN cannot disagree. The journal's next_lsn must not be used instead: it counts
 * records that were queued but may never have reached disk, and truncating to
 * that would discard mappings the snapshot never captured, orphaning live
 * objects.
 */
uint64_t s3_chunk_map_get_applied_lsn(const struct s3_chunk_map *map);

/**
 * Chunk size the table was created with.
 *
 * A checkpoint records it and refuses to load against a different one: every
 * chunk_index in a snapshot means something else if the chunk size changed.
 */
uint32_t s3_chunk_map_get_chunk_size(const struct s3_chunk_map *map);

/**
 * The journal this table appends to, or NULL in memory-only mode.
 *
 * For the checkpoint path, which has to truncate the same journal the table
 * writes to and should not be handed a second reference to it -- two callers
 * disagreeing about which journal a table uses is not a state worth making
 * representable.
 */
struct s3_journal *s3_chunk_map_get_journal(const struct s3_chunk_map *map);

/**
 * Declare that the committed state now corresponds to \c lsn.
 *
 * Only for restoring from a checkpoint, where the snapshot as a whole carries one
 * LSN and the individual entries have none. Everywhere else the applied LSN moves
 * on its own, per commit -- *do not use this to paper over a missing LSN*, which
 * would let the journal be truncated past mappings that were never captured.
 */
void s3_chunk_map_set_applied_lsn(struct s3_chunk_map *map, uint64_t lsn);

/**
 * Look up the object a chunk currently maps to.
 *
 * Only *committed* mappings are visible: an insert or remove still in flight
 * (journal not yet durable) is not. That is the correct behaviour -- as far as
 * blobstore is concerned those writes have not completed.
 *
 * \param out_valid_bytes  Optional. How many bytes of the chunk have ever been
 *                         written -- reads beyond that must be zero-filled
 *                         rather than GETting a range that does not exist.
 *
 * \return 0 found; -ENOENT the chunk was never written (reads should return all
 *         zeroes); -EINVAL bad arguments
 */
int s3_chunk_map_lookup(struct s3_chunk_map *map, uint64_t chunk_index,
			struct spdk_uuid *out_uuid, uint32_t *out_valid_bytes);

/**
 * Bind a chunk to a new object.
 *
 * Asynchronous: the journal is written first, and when the callback fires the
 * record is durable and memory has been updated. The superseded object's uuid
 * is handed back through the callback (see s3_chunk_map_cb).
 *
 * The callback *may run before this function returns* (bad arguments, the
 * memory-only mode, or a journal submission failure). Callers must tolerate
 * that inline completion.
 *
 * Several inserts/removes may be in flight on the same chunk at once: they take
 * effect in submit order, and each one receives "the uuid bound by the previous
 * operation" as its old_uuid, so no object is missed and none is deleted twice.
 */
void s3_chunk_map_insert(struct s3_chunk_map *map, uint64_t chunk_index,
			 const struct spdk_uuid *uuid, uint32_t valid_bytes,
			 s3_chunk_map_cb cb_fn, void *cb_arg);

/**
 * Unbind a chunk. The old uuid is likewise handed back for the caller to delete.
 *
 * A status of -ENOENT means it was already empty, including the case where a
 * previously submitted in-flight operation had already cleared it.
 */
void s3_chunk_map_remove(struct s3_chunk_map *map, uint64_t chunk_index,
			 s3_chunk_map_cb cb_fn, void *cb_arg);

/**
 * Iterate every non-empty entry. Returning non-zero from the callback stops the
 * walk early.
 *
 * Used to enumerate live objects for GC, and later to serialize checkpoints.
 */
typedef int (*s3_chunk_map_iter_cb)(void *cb_arg, uint64_t chunk_index,
				    const struct spdk_uuid *uuid,
				    uint32_t valid_bytes, uint32_t flags,
				    uint64_t gen);

void s3_chunk_map_foreach(struct s3_chunk_map *map,
			  s3_chunk_map_iter_cb cb, void *cb_arg);

#endif /* S3LVOL_CHUNK_MAP_H */
