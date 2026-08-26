/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#include "spdk/stdinc.h"
#include "spdk/crc32.h"
#include "spdk/log.h"
#include "spdk/util.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_checkpoint.h"
#include "s3lvol/s3_chunk_map.h"

/* Same prefix rule as the data objects and the owner marker. */
#define CKPT_KEY_FMT    "%s/meta/checkpoint"
#define CKPT_KEY_MAX    256

/* Refuse to load anything absurd rather than trying to allocate it. A 1 PiB
 * lvstore with 1 MiB chunks has 2^30 chunks, so 40 GiB of entries -- far beyond
 * anything this is designed for. 4 GiB is a generous ceiling that still fails
 * fast on a corrupt length. */
#define CKPT_MAX_BYTES  (4ULL * 1024 * 1024 * 1024)

struct ckpt_ctx {
	struct s3_client        *client;
	char                     key[CKPT_KEY_MAX];

	/* The serialised object. Its lifetime must cover the whole PUT/GET,
	 * hence living here rather than on the stack. */
	void                    *buf;
	uint64_t                 buf_len;
	struct iovec             iov;

	/* Load only. */
	struct s3_chunk_map     *map;
	uint64_t                 obj_size;

	s3_checkpoint_cb         cb;
	s3_checkpoint_load_cb    load_cb;
	void                    *cb_arg;
};

static uint32_t
ckpt_header_crc(const struct s3_ckpt_header *hdr)
{
	struct s3_ckpt_header tmp;

	memcpy(&tmp, hdr, sizeof(tmp));
	tmp.crc = 0;
	return spdk_crc32c_update(&tmp, sizeof(tmp), 0);
}

/* ==========================================================================
 * save
 * ========================================================================== */

struct ckpt_serialise_ctx {
	struct s3_ckpt_entry    *entries;
	uint64_t                 capacity;
	uint64_t                 count;
	bool                     overflow;
};

static int
ckpt_collect(void *cb_arg, uint64_t chunk_index, const struct spdk_uuid *uuid,
	     uint32_t valid_bytes, uint32_t flags, uint64_t gen)
{
	struct ckpt_serialise_ctx *sctx = cb_arg;
	struct s3_ckpt_entry *e;

	if (sctx->count >= sctx->capacity) {
		/* The buffer was sized from s3_chunk_map_get_allocated(), so this
		 * means the map grew between sizing and walking -- which cannot
		 * happen on one thread with no yield in between. Stop rather than
		 * write past the end, and let the caller fail the checkpoint: a
		 * truncated snapshot that still got its LSN stamped would be worse
		 * than no snapshot. */
		sctx->overflow = true;
		return -ENOSPC;
	}

	e = &sctx->entries[sctx->count++];
	e->chunk_index = chunk_index;
	spdk_uuid_copy(&e->uuid, uuid);
	e->valid_bytes = valid_bytes;
	e->flags       = flags;
	e->gen         = gen;
	return 0;
}

static void
ckpt_ctx_free(struct ckpt_ctx *ctx)
{
	free(ctx->buf);
	free(ctx);
}

static void
ckpt_put_done(void *cb_arg, int status)
{
	struct ckpt_ctx *ctx = cb_arg;
	s3_checkpoint_cb cb_fn = ctx->cb;
	void *user_arg = ctx->cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to upload the checkpoint '%s': %d\n",
			    ctx->key, status);
	}

	ckpt_ctx_free(ctx);

	if (cb_fn) {
		cb_fn(user_arg, status);
	}
}

void
s3_checkpoint_save(struct s3_client *client, const char *lvs_name,
		   const struct spdk_uuid *lvs_uuid,
		   struct s3_chunk_map *map, uint64_t lsn, uint64_t gen,
		   s3_checkpoint_cb cb_fn, void *cb_arg)
{
	struct ckpt_ctx *ctx;
	struct ckpt_serialise_ctx sctx = {0};
	struct s3_ckpt_header *hdr;
	uint64_t allocated;
	uint64_t total;
	int rc;

	assert(cb_fn != NULL);

	if (!client || !lvs_name || !map) {
		cb_fn(cb_arg, -EINVAL);
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		cb_fn(cb_arg, -ENOMEM);
		return;
	}
	ctx->client = client;
	ctx->cb     = cb_fn;
	ctx->cb_arg = cb_arg;

	rc = snprintf(ctx->key, sizeof(ctx->key), CKPT_KEY_FMT, lvs_name);
	if (rc < 0 || (size_t)rc >= sizeof(ctx->key)) {
		free(ctx);
		cb_fn(cb_arg, -ENAMETOOLONG);
		return;
	}

	allocated = s3_chunk_map_get_allocated(map);
	total = sizeof(*hdr) + allocated * sizeof(struct s3_ckpt_entry);
	if (total > CKPT_MAX_BYTES) {
		SPDK_ERRLOG("Checkpoint for '%s' would be %" PRIu64 " bytes, "
			    "which exceeds the %llu byte ceiling\n",
			    lvs_name, total, CKPT_MAX_BYTES);
		free(ctx);
		cb_fn(cb_arg, -EFBIG);
		return;
	}

	ctx->buf = calloc(1, total);
	if (!ctx->buf) {
		free(ctx);
		cb_fn(cb_arg, -ENOMEM);
		return;
	}
	ctx->buf_len = total;

	/* Everything from here to the end of this function is synchronous, which
	 * is what makes the snapshot and `lsn` a matched pair: nothing can commit
	 * a chunk map change in between. Do not introduce a yield here. */
	hdr = ctx->buf;
	sctx.entries  = (struct s3_ckpt_entry *)((char *)ctx->buf + sizeof(*hdr));
	sctx.capacity = allocated;

	s3_chunk_map_foreach(map, ckpt_collect, &sctx);
	if (sctx.overflow) {
		SPDK_ERRLOG("Checkpoint for '%s' overflowed its buffer (expected "
			    "%" PRIu64 " entries); refusing to upload a partial "
			    "snapshot\n", lvs_name, allocated);
		ckpt_ctx_free(ctx);
		cb_fn(cb_arg, -EOVERFLOW);
		return;
	}

	hdr->magic       = S3_CKPT_MAGIC;
	hdr->version     = S3_CKPT_VERSION;
	hdr->chunk_size  = s3_chunk_map_get_chunk_size(map);
	if (lvs_uuid) {
		spdk_uuid_copy(&hdr->lvs_uuid, lvs_uuid);
	}
	hdr->checkpoint_lsn = lsn;
	hdr->gen            = gen;
	hdr->num_entries    = sctx.count;
	hdr->entries_crc    = spdk_crc32c_update(
		sctx.entries, sctx.count * sizeof(struct s3_ckpt_entry), 0);
	hdr->crc = ckpt_header_crc(hdr);

	/* The walk skips empty entries, so fewer than `allocated` is possible if
	 * the accounting ever drifts. Shrink the upload rather than padding with
	 * zeroes, which would fail the entry CRC on load. */
	ctx->buf_len = sizeof(*hdr) + sctx.count * sizeof(struct s3_ckpt_entry);

	ctx->iov.iov_base = ctx->buf;
	ctx->iov.iov_len  = ctx->buf_len;

	SPDK_NOTICELOG("Checkpoint gen=%" PRIu64 " for '%s': %" PRIu64 " mappings, "
		       "%" PRIu64 " KiB, covers LSN %" PRIu64 "\n",
		       gen, lvs_name, sctx.count, ctx->buf_len / 1024, lsn);

	rc = s3_put(client, ctx->key, &ctx->iov, 1, false, ckpt_put_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit the checkpoint upload for '%s': "
			    "%d\n", lvs_name, rc);
		ckpt_ctx_free(ctx);
		cb_fn(cb_arg, rc);
	}
}

/* ==========================================================================
 * load
 * ========================================================================== */

static void
ckpt_load_finish(struct ckpt_ctx *ctx, uint64_t lsn, uint64_t gen, int status)
{
	s3_checkpoint_load_cb cb_fn = ctx->load_cb;
	void *user_arg = ctx->cb_arg;

	ckpt_ctx_free(ctx);

	if (cb_fn) {
		cb_fn(user_arg, lsn, gen, status);
	}
}

static void
ckpt_get_done(void *cb_arg, uint64_t bytes_read, int status)
{
	struct ckpt_ctx *ctx = cb_arg;
	const struct s3_ckpt_header *hdr = ctx->buf;
	const struct s3_ckpt_entry *entries;
	uint64_t expect_bytes;
	uint64_t i;
	uint32_t crc;

	if (status != 0) {
		SPDK_ERRLOG("Failed to download the checkpoint '%s': %d\n",
			    ctx->key, status);
		ckpt_load_finish(ctx, 0, 0, status);
		return;
	}

	if (bytes_read < sizeof(*hdr)) {
		SPDK_ERRLOG("Checkpoint '%s' is only %" PRIu64 " bytes, too short "
			    "for a header\n", ctx->key, bytes_read);
		ckpt_load_finish(ctx, 0, 0, -EILSEQ);
		return;
	}

	if (hdr->magic != S3_CKPT_MAGIC) {
		SPDK_ERRLOG("Checkpoint '%s' has a bad magic\n", ctx->key);
		ckpt_load_finish(ctx, 0, 0, -EILSEQ);
		return;
	}
	if (hdr->version != S3_CKPT_VERSION) {
		SPDK_ERRLOG("Checkpoint '%s' is version %u, this build "
			    "understands %d\n", ctx->key, hdr->version,
			    S3_CKPT_VERSION);
		ckpt_load_finish(ctx, 0, 0, -EPROTO);
		return;
	}
	if (ckpt_header_crc(hdr) != hdr->crc) {
		SPDK_ERRLOG("Checkpoint '%s' header CRC mismatch\n", ctx->key);
		ckpt_load_finish(ctx, 0, 0, -EILSEQ);
		return;
	}

	/* Geometry has to match, or every chunk_index in here means something
	 * else. */
	if (hdr->chunk_size != s3_chunk_map_get_chunk_size(ctx->map)) {
		SPDK_ERRLOG("Checkpoint '%s' was taken with chunk_size %u but this "
			    "lvstore uses %u -- refusing to apply it\n",
			    ctx->key, hdr->chunk_size,
			    s3_chunk_map_get_chunk_size(ctx->map));
		ckpt_load_finish(ctx, 0, 0, -EINVAL);
		return;
	}

	expect_bytes = sizeof(*hdr) +
		       hdr->num_entries * sizeof(struct s3_ckpt_entry);
	if (bytes_read != expect_bytes) {
		SPDK_ERRLOG("Checkpoint '%s' says %" PRIu64 " entries (%" PRIu64
			    " bytes) but %" PRIu64 " bytes arrived\n",
			    ctx->key, hdr->num_entries, expect_bytes, bytes_read);
		ckpt_load_finish(ctx, 0, 0, -EILSEQ);
		return;
	}

	entries = (const struct s3_ckpt_entry *)((const char *)ctx->buf +
						 sizeof(*hdr));
	crc = spdk_crc32c_update(entries,
				 hdr->num_entries * sizeof(struct s3_ckpt_entry),
				 0);
	if (crc != hdr->entries_crc) {
		/* Refuse the whole thing. A partially valid snapshot is worse than
		 * none: the caller would replay the journal from this snapshot's
		 * LSN and never notice the mappings that are missing. */
		SPDK_ERRLOG("Checkpoint '%s' entry CRC mismatch; refusing to apply "
			    "a snapshot that may be missing mappings\n", ctx->key);
		ckpt_load_finish(ctx, 0, 0, -EILSEQ);
		return;
	}

	for (i = 0; i < hdr->num_entries; i++) {
		const struct s3_ckpt_entry *e = &entries[i];
		int rc;

		/* lsn 0: the map's applied_lsn is set from the header once, below,
		 * rather than per entry -- individual entries have no LSN of their
		 * own. */
		rc = s3_chunk_map_apply_update(ctx->map, e->chunk_index, &e->uuid,
					       e->valid_bytes, e->gen, e->flags,
					       0);
		if (rc != 0) {
			SPDK_ERRLOG("Checkpoint '%s': entry %" PRIu64 " (chunk %"
				    PRIu64 ") could not be applied: %d\n",
				    ctx->key, i, e->chunk_index, rc);
			ckpt_load_finish(ctx, 0, 0, rc);
			return;
		}
	}

	s3_chunk_map_set_applied_lsn(ctx->map, hdr->checkpoint_lsn);

	SPDK_NOTICELOG("Loaded checkpoint gen=%" PRIu64 " from '%s': %" PRIu64
		       " mappings, covers LSN %" PRIu64 "\n",
		       hdr->gen, ctx->key, hdr->num_entries, hdr->checkpoint_lsn);

	ckpt_load_finish(ctx, hdr->checkpoint_lsn, hdr->gen, 0);
}

static void
ckpt_head_done(void *cb_arg, int status)
{
	struct ckpt_ctx *ctx = cb_arg;
	int rc;

	if (status == -ENOENT) {
		/* No checkpoint has ever completed. Normal for a young lvstore --
		 * the caller replays the journal from the beginning. */
		SPDK_NOTICELOG("No checkpoint at '%s'; the journal will be "
			       "replayed from the start\n", ctx->key);
		ckpt_load_finish(ctx, 0, 0, 0);
		return;
	}
	if (status != 0) {
		SPDK_ERRLOG("Failed to stat the checkpoint '%s': %d\n",
			    ctx->key, status);
		ckpt_load_finish(ctx, 0, 0, status);
		return;
	}

	if (ctx->obj_size < sizeof(struct s3_ckpt_header) ||
	    ctx->obj_size > CKPT_MAX_BYTES) {
		SPDK_ERRLOG("Checkpoint '%s' has an implausible size %" PRIu64
			    "\n", ctx->key, ctx->obj_size);
		ckpt_load_finish(ctx, 0, 0, -EILSEQ);
		return;
	}

	ctx->buf = calloc(1, ctx->obj_size);
	if (!ctx->buf) {
		ckpt_load_finish(ctx, 0, 0, -ENOMEM);
		return;
	}
	ctx->buf_len = ctx->obj_size;

	/* HEAD first, then a sized GET. Unlike the owner marker -- which has a
	 * known smallceiling and can be read with one fixed-length range request
	 * -- a snapshot scales with the number of live chunks, so its size has to
	 * be learned before allocating. */
	rc = s3_get_range(ctx->client, ctx->key, 0, ctx->obj_size, ctx->buf,
			  ckpt_get_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit the checkpoint download for '%s': "
			    "%d\n", ctx->key, rc);
		ckpt_load_finish(ctx, 0, 0, rc);
	}
}

void
s3_checkpoint_load(struct s3_client *client, const char *lvs_name,
		   struct s3_chunk_map *map,
		   s3_checkpoint_load_cb cb_fn, void *cb_arg)
{
	struct ckpt_ctx *ctx;
	int rc;

	assert(cb_fn != NULL);

	if (!client || !lvs_name || !map) {
		cb_fn(cb_arg, 0, 0, -EINVAL);
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		cb_fn(cb_arg, 0, 0, -ENOMEM);
		return;
	}
	ctx->client  = client;
	ctx->map     = map;
	ctx->load_cb = cb_fn;
	ctx->cb_arg  = cb_arg;

	rc = snprintf(ctx->key, sizeof(ctx->key), CKPT_KEY_FMT, lvs_name);
	if (rc < 0 || (size_t)rc >= sizeof(ctx->key)) {
		free(ctx);
		cb_fn(cb_arg, 0, 0, -ENAMETOOLONG);
		return;
	}

	rc = s3_head(client, ctx->key, &ctx->obj_size, ckpt_head_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit the checkpoint stat for '%s': %d\n",
			    lvs_name, rc);
		free(ctx);
		cb_fn(cb_arg, 0, 0, rc);
	}
}

/* ==========================================================================
 * delete
 * ========================================================================== */

static void
ckpt_delete_done(void *cb_arg, int status)
{
	struct ckpt_ctx *ctx = cb_arg;
	s3_checkpoint_cb cb_fn = ctx->cb;
	void *user_arg = ctx->cb_arg;

	if (status != 0 && status != -ENOENT) {
		SPDK_WARNLOG("Failed to delete the checkpoint '%s': %d\n",
			     ctx->key, status);
	}

	ckpt_ctx_free(ctx);

	if (cb_fn) {
		cb_fn(user_arg, status == -ENOENT ? 0 : status);
	}
}

void
s3_checkpoint_delete(struct s3_client *client, const char *lvs_name,
		     s3_checkpoint_cb cb_fn, void *cb_arg)
{
	struct ckpt_ctx *ctx;
	int rc;

	if (!client || !lvs_name) {
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
	ctx->client = client;
	ctx->cb     = cb_fn;
	ctx->cb_arg = cb_arg;

	rc = snprintf(ctx->key, sizeof(ctx->key), CKPT_KEY_FMT, lvs_name);
	if (rc < 0 || (size_t)rc >= sizeof(ctx->key)) {
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, -ENAMETOOLONG);
		}
		return;
	}

	rc = s3_delete(client, ctx->key, ckpt_delete_done, ctx);
	if (rc != 0) {
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, rc);
		}
	}
}
