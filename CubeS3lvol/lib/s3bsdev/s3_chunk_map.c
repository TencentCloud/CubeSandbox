/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Chunk map -- maps LBA ranges to S3 objects 
 *
 *   === Choice of data structure ===
 *
 *   A dense array indexed directly by chunk_index, not an RB tree or a hash.
 *   Reasons:
 *   - Chunks are fixed size (S3_CHUNK_SIZE), so LBA -> chunk_index is plain
 *     division and no range lookup is needed at all;
 *   - blobstore I/O is extremely localized (metadata pages, sequential cluster
 *     writes), and an array's cache behaviour beats a tree's by a wide margin;
 *   - a 1 TiB lvstore with 4 MiB chunks is 256 K entries at ~56 bytes each,
 *     roughly 14 MiB, which is acceptable.
 *
 *   Sparseness is expressed by "the entry is empty" (all-zero uuid), so no
 *   separate presence structure is needed: a chunk that was never written reads
 *   back as zeroes, which is exactly the semantics blobstore expects.
 *
 *   === Two states: committed and pending ===
 *
 *   Now that insert/remove are asynchronous (memory is mutated only after the
 *   journal is durable), several operations can be in flight on the same chunk.
 *   Each entry therefore tracks two states:
 *
 *     committed -- the durable mapping. lookup() only consults this, because an
 *                  in-flight write has not completed as far as blobstore is
 *                  concerned.
 *     pending   -- "what this will become once every submitted operation has
 *                  taken effect". *Used solely to compute old_uuid and the
 *                  generation.*
 *
 *   Why pending is required: old_uuid has to be determined *at submit time* and
 *   cannot wait for the callback. Take a run of overwrites A->B->C on one chunk.
 *   If old_uuid came from committed state, both later operations would receive
 *   A -- so A gets deleted twice while B is never deleted and leaks forever.
 *   Taking it from pending yields A and then B, so every superseded object is
 *   handed back exactly once.
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"
#include "spdk/uuid.h"
#include "spdk/util.h"

#include "s3lvol/s3_chunk_map.h"
#include "s3lvol/s3_journal.h"

/* Mapping entry for a single chunk.
 *
 * Note this stores the uuid rather than the full S3 key: the key can be
 * reconstructed as <prefix>/data/<uuid>, and16 bytes of uuid is far cheaper
 * than the ~100 bytes a key would take.
 */
struct chunk_entry {
	/* ---- committed: the durable mapping, which is what lookup reads ---- */
	struct spdk_uuid        uuid;

	/* Bytes of the chunk that have actually been written. Distinguishes "the
	 * whole chunk was written" from "only a prefix was" -- for the latter,
	 * reads past the end must be zero-filled rather than GETting a range
	 * that does not exist. */
	uint32_t                valid_bytes;

	/* How many times this chunk has been overwritten. write-new-then-swap
	 * picks a fresh uuid each time, and gen lets journal replay and
	 * diagnostics tell old from new. */
	uint32_t                generation;

	/* enum s3_chunk_flags */
	uint32_t                flags;

	/* LSN of the journal record this entry's committed state came from, so
	 * that replay can reject a record older than what the entry already
	 * holds. *This is what makes replay independent of the order records are
	 * read in*, which the journal cannot guarantee once the ring wraps: after
	 * a wrap, physical block order runs newest-first, and "last writer wins"
	 * would then hand the entry back to a superseded object that the
	 * create-once path has already deleted -- a 404 on the next read.
	 *
	 * Not persisted, and does not need to be. A checkpoint's entries are
	 * applied with lsn 0 and replay only feeds records above the
	 * checkpoint's LSN, so starting from 0 cannot reject anything that should
	 * have been applied; only the records' order relative to each other
	 * matters, and that is exactly what this compares.
	 *
	 * Why not reuse `generation`: apply_remove() carries no gen and zeroes it,
	 * so an update that followed a remove would look older than the remove.
	 * The LSN has no such hole -- it is monotonic across both ops. */
	uint64_t                applied_lsn;

	/* ---- pending: end state of the submitted-but-not-durable chain ---- */

	/* The uuid this chunk will be bound to once every in-flight operation
	 * has taken effect. Meaningless when inflight == 0 (committed governs
	 * then). */
	struct spdk_uuid        pending_uuid;
	uint32_t                pending_gen;

	/* How many operations are in flight on this chunk. */
	uint32_t                inflight;
};

struct s3_chunk_map {
	struct chunk_entry     *entries;
	uint64_t                num_chunks;

	uint32_t                chunk_size;
	uint32_t                block_size;

	/* Optional. Once attached, every insert/remove writes the journal before
	 * mutating memory. NULL means memory-only mode (no persistence; used by
	 * unit tests). */
	struct s3_journal      *journal;

	/* Statistics, counted against committed state. */
	uint64_t                allocated_chunks;

	/* Table-wide count of in-flight operations. Used by destroy() to assert
	 * that everything has drained. */
	uint64_t                inflight_total;

	/* Highest journal LSN whose effect is present in the committed state
	 * above.
	 *
	 * This is the number a checkpoint must be stamped with, and it is kept
	 * here rather than read from the journal because here it is true *by
	 * construction*: it only moves when a commit happens, so "the snapshot
	 * covers exactly up to applied_lsn" cannot drift. Asking the journal for
	 * its next_lsn instead would include records that were queued but never
	 * reached disk, and truncating to such an LSN would drop mappings the
	 * snapshot never captured.
	 *
	 * Journal durability is a strict LSN prefix -- one block write in flight
	 * at a time, LSNs assigned in queue order -- so a single number is enough;
	 * there are no holes below it. */
	uint64_t                applied_lsn;
};

/* One in-flight insert / remove. */
struct chunk_map_req {
	struct s3_chunk_map    *map;
	uint64_t                chunk_index;

	struct spdk_uuid        uuid;         /* new object an insert will bind */
	struct spdk_uuid        old_uuid;     /* superseded object, computed at submit */
	uint32_t                valid_bytes;
	uint32_t                gen;
	bool                    is_remove;

	/* LSN the journal assigned this record, captured at queue time. Recorded
	 * into the map on commit so a checkpoint can be stamped with the LSN its
	 * snapshot actually covers. */
	uint64_t                lsn;

	s3_chunk_map_cb         cb_fn;
	void                   *cb_arg;
};

static inline bool
chunk_entry_is_empty(const struct chunk_entry *e)
{
	return spdk_uuid_is_null(&e->uuid);
}

/* The entry's "latest intent" uuid/gen: pending when operations are in flight,
 * committed otherwise.
 *
 * Both old_uuid and the generation must be derived from this, or a run of
 * overwrites leaks superseded objects (see the file header comment). */
static void
chunk_entry_latest(const struct chunk_entry *e, struct spdk_uuid *out_uuid,
		   uint32_t *out_gen)
{
	if (e->inflight > 0) {
		spdk_uuid_copy(out_uuid, &e->pending_uuid);
		*out_gen = e->pending_gen;
	} else {
		spdk_uuid_copy(out_uuid, &e->uuid);
		*out_gen = e->generation;
	}
}

void
s3_chunk_data_key(const char *prefix, const struct spdk_uuid *uuid,
		  char *out, size_t out_len)
{
	char uuid_str[SPDK_UUID_STRING_LEN];

	spdk_uuid_fmt_lower(uuid_str, sizeof(uuid_str), uuid);
	snprintf(out, out_len, "%s/data/%s", prefix, uuid_str);
}

int
s3_chunk_map_create(uint64_t total_blocks, uint32_t block_size,
		    uint32_t chunk_size, struct s3_chunk_map **out_map)
{
	struct s3_chunk_map *map;
	uint64_t num_chunks;
	uint64_t total_bytes;

	if (!out_map || total_blocks == 0) {
		return -EINVAL;
	}
	if (block_size == 0 || chunk_size == 0) {
		return -EINVAL;
	}
	/* chunk_size must be a whole multiple of block_size, otherwise the
	 * LBA<->chunk conversion produces a half block straddling a boundary and
	 * the mapping logic cannot stay self-consistent. */
	if (chunk_size % block_size != 0) {
		SPDK_ERRLOG("chunk_size %u is not a multiple of block_size %u\n",
			    chunk_size, block_size);
		return -EINVAL;
	}

	total_bytes = total_blocks * block_size;
	num_chunks  = spdk_divide_round_up(total_bytes, chunk_size);

	map = calloc(1, sizeof(*map));
	if (!map) {
		return -ENOMEM;
	}

	map->entries = calloc(num_chunks, sizeof(struct chunk_entry));
	if (!map->entries) {
		free(map);
		return -ENOMEM;
	}

	map->num_chunks = num_chunks;
	map->chunk_size = chunk_size;
	map->block_size = block_size;

	SPDK_NOTICELOG("Chunk map created: %" PRIu64 " chunks x %u bytes "
		       "(%" PRIu64 " blocks x %u bytes), %zu bytes of index\n",
		       num_chunks, chunk_size, total_blocks, block_size,
		       num_chunks * sizeof(struct chunk_entry));

	*out_map = map;
	return 0;
}

void
s3_chunk_map_destroy(struct s3_chunk_map *map)
{
	if (!map) {
		return;
	}

	/* Freeing while operations are in flight means a journal callback touches
	 * freed memory. The caller must drain first. Assert rather than tolerate
	 * silently -- this class of use-after-free is extremely hard to track
	 * down in production. */
	assert(map->inflight_total == 0);
	if (map->inflight_total != 0) {
		SPDK_ERRLOG("s3_chunk_map_destroy() called with %" PRIu64
			    " operations still in flight; leaking the map to "
			    "avoid use-after-free\n", map->inflight_total);
		return;
	}

	free(map->entries);
	free(map);
}

uint64_t
s3_chunk_map_get_num_chunks(const struct s3_chunk_map *map)
{
	return map ? map->num_chunks : 0;
}

uint64_t
s3_chunk_map_get_applied_lsn(const struct s3_chunk_map *map)
{
	return map ? map->applied_lsn : 0;
}

struct s3_journal *
s3_chunk_map_get_journal(const struct s3_chunk_map *map)
{
	return map ? map->journal : NULL;
}

uint32_t
s3_chunk_map_get_chunk_size(const struct s3_chunk_map *map)
{
	return map ? map->chunk_size : 0;
}

void
s3_chunk_map_set_applied_lsn(struct s3_chunk_map *map, uint64_t lsn)
{
	if (map) {
		map->applied_lsn = lsn;
	}
}

uint64_t
s3_chunk_map_get_allocated(const struct s3_chunk_map *map)
{
	return map ? map->allocated_chunks : 0;
}

int
s3_chunk_map_lookup(struct s3_chunk_map *map, uint64_t chunk_index,
		    struct spdk_uuid *out_uuid, uint32_t *out_valid_bytes)
{
	struct chunk_entry *e;

	if (!map || chunk_index >= map->num_chunks) {
		return -EINVAL;
	}

	/* Committed state only -- an in-flight write (journal not yet durable)
	 * has not completed as far as blobstore is concerned, and exposing it
	 * would hand out a mapping that may never take effect. */
	e = &map->entries[chunk_index];
	if (chunk_entry_is_empty(e)) {
		return -ENOENT;
	}

	if (out_uuid) {
		spdk_uuid_copy(out_uuid, &e->uuid);
	}
	if (out_valid_bytes) {
		*out_valid_bytes = e->valid_bytes;
	}
	return 0;
}

/* ==========================================================================
 * Asynchronous insert / remove
 * ========================================================================== */

/* The journal record is durable (or immediately, in memory-only mode): apply the
 * change to committed state. */
static void
chunk_map_req_commit(struct chunk_map_req *req)
{
	struct s3_chunk_map *map = req->map;
	struct chunk_entry *e = &map->entries[req->chunk_index];
	bool was_empty = chunk_entry_is_empty(e);

	/* Guarded with a comparison rather than assigned outright. In memory-only
	 * mode there is no journal and lsn stays 0, and a failed append leaves a
	 * gap -- neither should be able to move this backwards. */
	if (req->lsn > map->applied_lsn) {
		map->applied_lsn = req->lsn;
	}

	/* Kept up to date on the running path too, not just during replay. It is
	 * not read here -- a commit is an acknowledged intent and applies
	 * unconditionally -- but keeping it true means the field always answers
	 * "which record did this entry's state come from", rather than only doing
	 * so until the first write after an attach. */
	if (req->lsn > e->applied_lsn) {
		e->applied_lsn = req->lsn;
	}

	if (req->is_remove) {
		if (!was_empty) {
			assert(map->allocated_chunks > 0);
			map->allocated_chunks--;
		}
		spdk_uuid_set_null(&e->uuid);
		e->valid_bytes = 0;
		e->generation  = 0;
		e->flags       = 0;
		return;
	}

	if (was_empty) {
		map->allocated_chunks++;
	}
	spdk_uuid_copy(&e->uuid, &req->uuid);
	e->valid_bytes = req->valid_bytes;
	e->generation  = req->gen;
	/* The PUT to S3 already completed (the caller invokes this module from
	 * write_done), so mark IN_S3 -- this is what establishes INV2. */
	e->flags       = S3_CHUNK_IN_S3;
}

static void
chunk_map_req_finish(struct chunk_map_req *req, int status)
{
	struct s3_chunk_map *map = req->map;
	struct chunk_entry *e = &map->entries[req->chunk_index];
	s3_chunk_map_cb cb_fn = req->cb_fn;
	void *cb_arg = req->cb_arg;
	struct spdk_uuid old_uuid;

	if (status == 0) {
		chunk_map_req_commit(req);
	}

	/* The in-flight count must drop on both success and failure, otherwise
	 * destroy() never sees the table drain.
	 *
	 * On failure, *pending_uuid is deliberately not rolled back*: the S3
	 * object for that submitted operation really does exist (the PUT
	 * succeeded, only the journal write did not), so having a later
	 * operation pick it up as its old_uuid and delete it is exactly what we
	 * want -- it is an orphan already. */
	assert(e->inflight > 0);
	e->inflight--;
	assert(map->inflight_total > 0);
	map->inflight_total--;

	spdk_uuid_copy(&old_uuid, &req->old_uuid);
	free(req);

	if (cb_fn) {
		cb_fn(cb_arg, &old_uuid, status);
	}
}

static void
chunk_map_journal_done(void *cb_arg, int status)
{
	struct chunk_map_req *req = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to journal chunk %" PRIu64 " %s: %d — the "
			    "in-memory map is left untouched, so the new object "
			    "becomes an orphan for GC\n",
			    req->chunk_index,
			    req->is_remove ? "removal" : "update", status);
	}

	chunk_map_req_finish(req, status);
}

/* Submit an operation: compute old_uuid and gen, advance pending, then write the
 * journal.
 *
 * Ownership of req transfers to this function; it frees req on every path. */
static void
chunk_map_submit(struct chunk_map_req *req)
{
	struct s3_chunk_map *map = req->map;
	struct chunk_entry *e = &map->entries[req->chunk_index];
	struct spdk_uuid latest;
	uint32_t latest_gen;

	chunk_entry_latest(e, &latest, &latest_gen);

	/* old_uuid comes from the "latest intent" rather than committed state --
	 * see the file header comment. */
	spdk_uuid_copy(&req->old_uuid, &latest);

	if (req->is_remove) {
		if (spdk_uuid_is_null(&latest)) {
			/* Already empty, possibly because an earlier in-flight
			 * operation cleared it. */
			s3_chunk_map_cb cb_fn = req->cb_fn;
			void *cb_arg = req->cb_arg;

			free(req);
			if (cb_fn) {
				struct spdk_uuid null_uuid;

				spdk_uuid_set_null(&null_uuid);
				cb_fn(cb_arg, &null_uuid, -ENOENT);
			}
			return;
		}
		spdk_uuid_set_null(&e->pending_uuid);
		e->pending_gen = 0;
	} else {
		req->gen = spdk_uuid_is_null(&latest) ? 0 : latest_gen + 1;
		spdk_uuid_copy(&e->pending_uuid, &req->uuid);
		e->pending_gen = req->gen;
	}

	e->inflight++;
	map->inflight_total++;

	if (!map->journal) {
		/* Memory-only mode: there is no journal to wait for, so this takes
		 * effect immediately. The callback runs before this function
		 * returns, which the header documents as possible. */
		chunk_map_req_finish(req, 0);
		return;
	}

	/* Journal first, memory second.
	 *
	 * The other way around leaves a crash window where memory was updated but
	 * the disk was not; recovery then comes up missing a mapping and the
	 * corresponding S3 object becomes an unreachable orphan (the data is
	 * there but cannot be retrieved). The reverse case -- journal written,
	 * memory not updated -- merely replays one extra record, which is
	 * idempotent and harmless. */
	if (req->is_remove) {
		s3_journal_append_remove(map->journal, req->chunk_index,
					 &req->lsn, chunk_map_journal_done, req);
	} else {
		s3_journal_append_update(map->journal, req->chunk_index,
					 &req->uuid, req->valid_bytes, req->gen,
					 S3_CHUNK_IN_S3,
					 &req->lsn, chunk_map_journal_done, req);
	}
}

void
s3_chunk_map_insert(struct s3_chunk_map *map, uint64_t chunk_index,
		    const struct spdk_uuid *uuid, uint32_t valid_bytes,
		    s3_chunk_map_cb cb_fn, void *cb_arg)
{
	struct chunk_map_req *req;
	struct spdk_uuid null_uuid;

	spdk_uuid_set_null(&null_uuid);

	if (!map || !uuid || chunk_index >= map->num_chunks) {
		if (cb_fn) {
			cb_fn(cb_arg, &null_uuid, -EINVAL);
		}
		return;
	}
	if (valid_bytes > map->chunk_size) {
		SPDK_ERRLOG("valid_bytes %u exceeds chunk_size %u\n",
			    valid_bytes, map->chunk_size);
		if (cb_fn) {
			cb_fn(cb_arg, &null_uuid, -EINVAL);
		}
		return;
	}
	if (spdk_uuid_is_null(uuid)) {
		/* An all-zero uuid is the internal marker for "empty entry", so it
		 * cannot be accepted as a legitimate value. */
		SPDK_ERRLOG("refusing to insert a null uuid at chunk %" PRIu64 "\n",
			    chunk_index);
		if (cb_fn) {
			cb_fn(cb_arg, &null_uuid, -EINVAL);
		}
		return;
	}

	req = calloc(1, sizeof(*req));
	if (!req) {
		if (cb_fn) {
			cb_fn(cb_arg, &null_uuid, -ENOMEM);
		}
		return;
	}

	req->map         = map;
	req->chunk_index = chunk_index;
	req->valid_bytes = valid_bytes;
	req->is_remove   = false;
	req->cb_fn       = cb_fn;
	req->cb_arg      = cb_arg;
	spdk_uuid_copy(&req->uuid, uuid);

	chunk_map_submit(req);
}

void
s3_chunk_map_remove(struct s3_chunk_map *map, uint64_t chunk_index,
		    s3_chunk_map_cb cb_fn, void *cb_arg)
{
	struct chunk_map_req *req;
	struct spdk_uuid null_uuid;

	spdk_uuid_set_null(&null_uuid);

	if (!map || chunk_index >= map->num_chunks) {
		if (cb_fn) {
			cb_fn(cb_arg, &null_uuid, -EINVAL);
		}
		return;
	}

	req = calloc(1, sizeof(*req));
	if (!req) {
		if (cb_fn) {
			cb_fn(cb_arg, &null_uuid, -ENOMEM);
		}
		return;
	}

	req->map         = map;
	req->chunk_index = chunk_index;
	req->is_remove   = true;
	req->cb_fn       = cb_fn;
	req->cb_arg      = cb_arg;

	chunk_map_submit(req);
}

/* ==========================================================================
 * Journal replay entry points
 *
 * These differ from insert/remove in that they do not write the journal (it is
 * what is being replayed) and do not return the old uuid (there is no GC during
 * replay; cleaning up old objects is left to GC's own scan). Hence both are
 * synchronous.
 *
 * === Both are ordering-independent ===
 *
 * A record whose LSN is not newer than what the entry already holds is dropped.
 * That makes replay idempotent and, more importantly, indifferent to the order
 * records arrive in -- which the journal cannot control once the ring wraps,
 * because it is read in physical block order and after a wrap the newest records
 * sit in the lowest-numbered blocks.
 *
 * lsn == 0 means "no journal record behind this change" and always applies. Two
 * callers rely on it: restoring a checkpoint, whose entries carry one LSN for the
 * snapshot as a whole (s3_checkpoint.c:326), and the WAL's unmap replay
 * (s3_bs_dev.c:2399), which must be able to undo a mapping the journal restored.
 * ========================================================================== */

/* Should this record be applied, given what the entry already holds?
 *
 * Kept separate because both apply functions need exactly the same rule and the
 * one place it can go wrong -- treating lsn 0 as "older than everything" and so
 * silently dropping every checkpoint entry -- is not visible at either call
 * site. */
static bool
chunk_entry_accepts(const struct chunk_entry *e, uint64_t lsn)
{
	if (lsn == 0) {
		return true;
	}
	return lsn > e->applied_lsn;
}

void
s3_chunk_map_set_journal(struct s3_chunk_map *map, struct s3_journal *journal)
{
	if (map) {
		map->journal = journal;
	}
}

int
s3_chunk_map_apply_update(struct s3_chunk_map *map, uint64_t chunk_index,
			  const struct spdk_uuid *uuid, uint32_t valid_bytes,
			  uint64_t gen, uint32_t flags, uint64_t lsn)
{
	struct chunk_entry *e;

	if (!map || !uuid) {
		return -EINVAL;
	}
	if (chunk_index >= map->num_chunks) {
		/* A chunk_index in the journal beyond the current capacity means
		 * either the lvstore was shrunk or this journal does not belong
		 * to this map. It must not be ignored silently -- that would
		 * leave the recovered mapping incomplete. */
		SPDK_ERRLOG("Journal record for chunk %" PRIu64 " is out of range "
			    "(map has %" PRIu64 " chunks)\n",
			    chunk_index, map->num_chunks);
		return -ERANGE;
	}
	if (spdk_uuid_is_null(uuid)) {
		SPDK_ERRLOG("Journal record for chunk %" PRIu64 " has a null uuid\n",
			    chunk_index);
		return -EILSEQ;
	}

	e = &map->entries[chunk_index];

	/* Advanced before the staleness test, and for stale records too: the
	 * question it answers is "how far has the scan got", which is true of a
	 * record that was read and rejected just as much as of one that was
	 * applied. Leaving it behind would let a checkpoint be stamped with an LSN
	 * lower than the records it actually covers. */
	if (lsn > map->applied_lsn) {
		map->applied_lsn = lsn;
	}

	if (!chunk_entry_accepts(e, lsn)) {
		/* Superseded: a newer record for this chunk has already been
		 * applied, and this one would move the mapping backwards to an
		 * object that no longer exists. */
		return 0;
	}

	if (chunk_entry_is_empty(e)) {
		map->allocated_chunks++;
	}

	spdk_uuid_copy(&e->uuid, uuid);
	e->valid_bytes = valid_bytes;
	e->generation  = (uint32_t)gen;
	e->flags       = flags;
	if (lsn > e->applied_lsn) {
		e->applied_lsn = lsn;
	}

	return 0;
}

int
s3_chunk_map_apply_remove(struct s3_chunk_map *map, uint64_t chunk_index,
			  uint64_t lsn)
{
	struct chunk_entry *e;

	if (!map) {
		return -EINVAL;
	}
	if (chunk_index >= map->num_chunks) {
		SPDK_ERRLOG("Journal record for chunk %" PRIu64 " is out of range "
			    "(map has %" PRIu64 " chunks)\n",
			    chunk_index, map->num_chunks);
		return -ERANGE;
	}

	/* Advanced here rather than at the exits below, because there are several
	 * successful ones: the record counts as applied even when it turns out to
	 * be a no-op or is rejected as stale. */
	if (lsn > map->applied_lsn) {
		map->applied_lsn = lsn;
	}

	e = &map->entries[chunk_index];

	if (!chunk_entry_accepts(e, lsn)) {
		/* A newer record already decided what this chunk maps to;
		 * replaying an older removal would drop a live mapping. */
		return 0;
	}
	if (lsn > e->applied_lsn) {
		e->applied_lsn = lsn;
	}

	if (chunk_entry_is_empty(e)) {
		/* Replay asked to remove something already empty -- idempotent,
		 * not an error. This happens when the same record is replayed
		 * twice (a crash before truncation). */
		return 0;
	}

	spdk_uuid_set_null(&e->uuid);
	e->valid_bytes = 0;
	e->generation  = 0;
	e->flags       = 0;
	assert(map->allocated_chunks > 0);
	map->allocated_chunks--;

	return 0;
}

void
s3_chunk_map_foreach(struct s3_chunk_map *map,
		     s3_chunk_map_iter_cb cb, void *cb_arg)
{
	uint64_t i;

	if (!map || !cb) {
		return;
	}

	for (i = 0; i < map->num_chunks; i++) {
		struct chunk_entry *e = &map->entries[i];

		if (chunk_entry_is_empty(e)) {
			continue;
		}
		if (cb(cb_arg, i, &e->uuid, e->valid_bytes, e->flags,
		       e->generation) != 0) {
			break;
		}
	}
}
