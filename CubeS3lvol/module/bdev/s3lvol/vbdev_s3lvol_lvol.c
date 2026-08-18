/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_lvol -- registers spdk_lvols into the global bdev table
 *
 *   === Why module/bdev/lvol cannot be reused ===
 *
 *   The core point is not "a few NULL checks"; the entry points are closed to out-of-tree code:
 *
 *   - Every built-in lvol RPC goes through vbdev_lvol_store_first() walking
 *     g_spdk_lvol_pairs (vbdev_lvol.c:20), which is a **static private list** --
 *     no external API can insert entries into it.
 *   - _create_lvol_disk() starts with vbdev_get_lvs_bdev_by_lvs() and
 *     assert(false)s when the lookup fails (the struct lvol_store_bdev
 *     machinery at vbdev_lvol.c:202 requires a real spdk_bdev); our lvstore
 *     sits on s3_bs_dev, with no bdev underneath.
 *
 *   Conversely, nvmf only recognises a name in the global bdev registry
 *   (spdk_bdev_open_ext_v2, lib/nvmf/subsystem.c:2596). So this file's whole
 *   reason for being is: **register lvols as bdevs, thereby getting the entire
 *   nvmf ecosystem for free.**
 *
 *   Note lib/lvol and lib/blob are **reused verbatim, zero modifications** --
 *   snapshots, clones and tree management all go through the public
 *   spdk_lvol_* API, and none of it needs rewriting here.
 *
 *   === Handling iovcnt ===
 *
 *   s3_bs_dev only supports iovcnt == 1 (multiple segments return -ENOTSUP).
 *   Here max_num_segments = 1 makes the bdev layer split multi-segment I/O
 *   into single-segment sub-I/Os (bdev.c:3331 splits on this), rather than
 *   assembling iovecs inside the bs_dev.
 *
 *   The read path is safe by construction (spdk_bdev_io_get_buf hands out a
 *   single contiguous buffer); the write path is spdk_blob_io_writev_ext
 *   passing bdev_io's iovs through unchanged, so the constraint point is the
 *   bdev layer. The -ENOTSUP in the bs_dev must **stay as an assertion**: only
 *   I/O that goes through the public spdk_bdev_* API is bound by
 *   max_num_segments.
 */

#include "spdk/stdinc.h"
#include "spdk/bdev_module.h"
#include "spdk/blob.h"
#include "spdk/log.h"
#include "spdk/lvol.h"
#include "spdk/string.h"
#include "spdk/thread.h"

#include "spdk_internal/lvolstore.h"

#include "vbdev_s3lvol.h"

/* ==========================================================================
 * Completion callbacks
 * ========================================================================== */

static void
s3lvol_io_complete(void *cb_arg, int bserrno)
{
	struct spdk_bdev_io *bdev_io = cb_arg;
	enum spdk_bdev_io_status status;

	if (bserrno == 0) {
		status = SPDK_BDEV_IO_STATUS_SUCCESS;
	} else if (bserrno == -ENOMEM) {
		/* Let the bdev layer queue and retry rather than surfacing the
		 * error upwards. */
		status = SPDK_BDEV_IO_STATUS_NOMEM;
	} else {
		status = SPDK_BDEV_IO_STATUS_FAILED;
	}

	spdk_bdev_io_complete(bdev_io, status);
}

/* ==========================================================================
 * Data plane
 * ========================================================================== */

static void
s3lvol_submit_read(struct spdk_io_channel *ch, struct spdk_bdev_io *bdev_io)
{
	struct spdk_lvol *lvol = bdev_io->bdev->ctxt;
	struct spdk_blob_ext_io_opts *opts = (struct spdk_blob_ext_io_opts *)
					     bdev_io->driver_ctx;

	memset(opts, 0, sizeof(*opts));
	opts->size = sizeof(*opts);
	opts->memory_domain     = bdev_io->u.bdev.memory_domain;
	opts->memory_domain_ctx = bdev_io->u.bdev.memory_domain_ctx;

	spdk_blob_io_readv_ext(lvol->blob, ch, bdev_io->u.bdev.iovs,
			       bdev_io->u.bdev.iovcnt,
			       bdev_io->u.bdev.offset_blocks,
			       bdev_io->u.bdev.num_blocks,
			       s3lvol_io_complete, bdev_io, opts);
}

/* A read needs its buffer first. The bdev layer hands out a single contiguous
 * region -- which is also why the read path is naturally iovcnt == 1, unrelated
 * to max_num_segments. */
static void
s3lvol_get_buf_cb(struct spdk_io_channel *ch, struct spdk_bdev_io *bdev_io,
		  bool success)
{
	if (!success) {
		spdk_bdev_io_complete(bdev_io, SPDK_BDEV_IO_STATUS_FAILED);
		return;
	}
	s3lvol_submit_read(ch, bdev_io);
}

static void
s3lvol_submit_write(struct spdk_io_channel *ch, struct spdk_bdev_io *bdev_io)
{
	struct spdk_lvol *lvol = bdev_io->bdev->ctxt;
	struct spdk_blob_ext_io_opts *opts = (struct spdk_blob_ext_io_opts *)
					     bdev_io->driver_ctx;

	memset(opts, 0, sizeof(*opts));
	opts->size = sizeof(*opts);
	opts->memory_domain     = bdev_io->u.bdev.memory_domain;
	opts->memory_domain_ctx = bdev_io->u.bdev.memory_domain_ctx;

	spdk_blob_io_writev_ext(lvol->blob, ch, bdev_io->u.bdev.iovs,
				bdev_io->u.bdev.iovcnt,
				bdev_io->u.bdev.offset_blocks,
				bdev_io->u.bdev.num_blocks,
				s3lvol_io_complete, bdev_io, opts);
}

static void
s3lvol_submit_request(struct spdk_io_channel *ch, struct spdk_bdev_io *bdev_io)
{
	struct spdk_lvol *lvol = bdev_io->bdev->ctxt;

	switch (bdev_io->type) {
	case SPDK_BDEV_IO_TYPE_READ:
		spdk_bdev_io_get_buf(bdev_io, s3lvol_get_buf_cb,
				     bdev_io->u.bdev.num_blocks *
				     bdev_io->bdev->blocklen);
		break;
	case SPDK_BDEV_IO_TYPE_WRITE:
		s3lvol_submit_write(ch, bdev_io);
		break;
	case SPDK_BDEV_IO_TYPE_UNMAP:
		spdk_blob_io_unmap(lvol->blob, ch, bdev_io->u.bdev.offset_blocks,
				   bdev_io->u.bdev.num_blocks,
				   s3lvol_io_complete, bdev_io);
		break;
	case SPDK_BDEV_IO_TYPE_WRITE_ZEROES:
		spdk_blob_io_write_zeroes(lvol->blob, ch,
					  bdev_io->u.bdev.offset_blocks,
					  bdev_io->u.bdev.num_blocks,
					  s3lvol_io_complete, bdev_io);
		break;
	case SPDK_BDEV_IO_TYPE_FLUSH:
		/* Complete inline. Two independent reasons, both of which the
		 * first version got wrong by calling spdk_blob_sync_md() here.
		 *
		 * 1. It is not allowed from this thread. sync_md is a metadata
		 *    operation, and blobstore asserts every one of them runs on
		 *    bs->md_thread (blob_verify_md_op, blobstore.c:89). A FLUSH
		 *    arrives on whichever nvmf poll group owns the qpair, never
		 *    on the lvstore's own thread, so the target aborted as soon
		 *    as a host ran mkfs / mount:
		 *      blob_verify_md_op: Assertion
		 *      `spdk_get_thread() == blob->bs->md_thread' failed.
		 *
		 * 2. There is nothing left to make durable. Everything this
		 *    bdev has acknowledged is already persistent:
		 *      - on the WAL path a write is acknowledged only after its
		 *        batch is durable on the local bdev (s3_wal.c commits
		 *        write + flush as one unit);
		 *      - writing straight to S3, a PUT is durable when it
		 *        completes;
		 *      - blob metadata for a newly allocated cluster is written
		 *        out by blobstore itself (blob_insert_cluster_msg)
		 *        before the write that triggered it is acknowledged.
		 *    That is the same reasoning as s3_bs_dev_flush(): "FLUSH does not
		 *    flush blobstore metadata".
		 *
		 * Getting data all the way to S3 is a separate, far more
		 * expensive operation (checkpoint / drain), deliberately not
		 * wired to fsync. */
		spdk_bdev_io_complete(bdev_io, SPDK_BDEV_IO_STATUS_SUCCESS);
		break;
	case SPDK_BDEV_IO_TYPE_RESET:
		/* There is no hardware state to reset. Must report success, or
		 * nvmf's error recovery would stall. */
		spdk_bdev_io_complete(bdev_io, SPDK_BDEV_IO_STATUS_SUCCESS);
		break;
	default:
		SPDK_INFOLOG(vbdev_s3lvol, "unsupported I/O type %d\n", bdev_io->type);
		spdk_bdev_io_complete(bdev_io, SPDK_BDEV_IO_STATUS_FAILED);
		break;
	}
}

/* Read-only-ness is expressed through here, with no extra flag (copying what
 * vbdev_lvol.c:842 does).
 *
 *   - `nvmf_subsystem_add_ns` has no read-only-like parameter (only nsid /
 *     nguid / eui64 / uuid / anagrpid / ptpl-file);
 *   - the NVMe protocol does have such a bit -- Identify Namespace's
 *     `nsattr.cwp` (Currently Write Protected) -- but `lib/nvmf/ctrlr.c`
 *     never sets `nsattr`; it only fills `nsfeat`'s optperf and
 *     ns_atomic_write_unit;
 *   - there is no read-only namespace implementation anywhere in `lib/nvmf/`.
 *
 * So the host kernel **believes the snapshot is writable** (`blockdev --getro`
 * is 0, mkfs / mount are not blocked), writes flow down normally, and are
 * rejected here, surfacing on the host as an I/O error. The data is safe --
 * the interception point is this layer, and the snapshot's clusters are still
 * being read by the clone, so letting a write through would corrupt the clone
 * too -- but **an accidental write fails; it is not prevented in advance**.
 * Making the host aware would require adding nsattr.cwp reporting to SPDK's
 * nvmf, which is upstream's business; until then snapshots should be consumed
 * read-only by the upper orchestration layer (mount -o ro).
 *
 * `run_snapshot_test.sh` pins this real behaviour down: it asserts both that
 * writes are rejected and that the host still sees a writable device -- so
 * nobody assumes a protection layer that does not exist. */
static bool
s3lvol_io_type_supported(void *ctx, enum spdk_bdev_io_type io_type)
{
	struct spdk_lvol *lvol = ctx;

	switch (io_type) {
	case SPDK_BDEV_IO_TYPE_WRITE:
	case SPDK_BDEV_IO_TYPE_UNMAP:
	case SPDK_BDEV_IO_TYPE_WRITE_ZEROES:
		return !spdk_blob_is_read_only(lvol->blob);
	case SPDK_BDEV_IO_TYPE_READ:
	case SPDK_BDEV_IO_TYPE_RESET:
	case SPDK_BDEV_IO_TYPE_FLUSH:
		return true;
	default:
		return false;
	}
}

static struct spdk_io_channel *
s3lvol_get_io_channel(void *ctx)
{
	struct spdk_lvol *lvol = ctx;

	return spdk_lvol_get_io_channel(lvol);
}

/* The lvol is closed; the bdev can go.
 *
 * Freeing the bdev here rather than letting the bdev layer do it: the layer does
 * not own bdev->name or bdev->product_name, and it only guarantees the struct is
 * unreferenced once destruct completes. Upstream vbdev_lvol frees its wrapper in
 * exactly this callback. */
static void
s3lvol_lvol_closed(void *cb_arg, int lvolerrno)
{
	struct spdk_bdev *bdev = cb_arg;
	char *name = bdev->name;
	char *product_name = bdev->product_name;

	if (lvolerrno != 0) {
		SPDK_ERRLOG("Failed to close lvol behind bdev '%s': %d\n",
			    name, lvolerrno);
	}

	spdk_bdev_destruct_done(bdev, lvolerrno);

	free(product_name);
	free(name);
	free(bdev);
}

static int
s3lvol_destruct(void *ctx)
{
	struct spdk_lvol *lvol = ctx;
	struct spdk_bdev *bdev = lvol->bdev;

	/* The bdev layer has already guaranteed there is no in-flight I/O.
	 *
	 * Closing the lvol here is what makes spdk_lvs_unload() possible at all:
	 * it refuses to unload while any lvol still holds a reference, and an
	 * open bdev is exactly such a reference. The first version left the close
	 * to "destroy_lvol / lvstore unload", which meant unloading always failed
	 * with -EBUSY. Upstream vbdev_lvol does the same thing here
	 * (vbdev_lvol_unregister).
	 *
	 * Returning 1 means the destruct is asynchronous; the bdev layer waits
	 * for spdk_bdev_destruct_done(). */
	lvol->bdev = NULL;

	spdk_lvol_close(lvol, s3lvol_lvol_closed, bdev);
	return 1;
}

static int
s3lvol_dump_info_json(void *ctx, struct spdk_json_write_ctx *w)
{
	struct spdk_lvol *lvol = ctx;

	spdk_json_write_named_object_begin(w, "s3lvol");
	spdk_json_write_named_uuid(w, "lvol_uuid", &lvol->uuid);
	spdk_json_write_named_string(w, "lvol_name", lvol->name);
	spdk_json_write_named_bool(w, "is_thin_provisioned",
				   spdk_blob_is_thin_provisioned(lvol->blob));
	spdk_json_write_named_bool(w, "is_snapshot",
				   spdk_blob_is_snapshot(lvol->blob));
	spdk_json_write_named_bool(w, "is_clone",
				   spdk_blob_is_clone(lvol->blob));
	spdk_json_write_named_bool(w, "is_read_only",
				   spdk_blob_is_read_only(lvol->blob));
	spdk_json_write_object_end(w);

	return 0;
}

static const struct spdk_bdev_fn_table s3lvol_fn_table = {
	.destruct          = s3lvol_destruct,
	.submit_request    = s3lvol_submit_request,
	.io_type_supported = s3lvol_io_type_supported,
	.get_io_channel    = s3lvol_get_io_channel,
	.dump_info_json    = s3lvol_dump_info_json,
	/* get_memory_domains is deliberately not implemented: the upstream
	 * implementation depends on get_base_bdev (vbdev_lvol.c:1023), and we
	 * have no base bdev. The callback is optional at the bdev layer
	 * (bdev.c:8481 has a NULL check). */
};

/* ==========================================================================
 * Registration / unregistration
 * ========================================================================== */

int
vbdev_s3lvol_bdev_register(struct spdk_lvol *lvol, const char *lvs_name)
{
	struct spdk_bdev *bdev;
	struct spdk_blob_store *bs;
	/* "<lvs>/<lvol>" + NUL. Both names are capped at 64
	 * (SPDK_LVS_NAME_MAX / SPDK_LVOL_NAME_MAX); this is plenty. */
	char bdev_name[SPDK_LVS_NAME_MAX + SPDK_LVOL_NAME_MAX + 2];
	uint64_t cluster_sz;
	int rc;

	if (!lvol || !lvol->blob || !lvs_name) {
		return -EINVAL;
	}

	/* The name is forced to <lvs_name>/<lvol_name>. bdev names are globally
	 * unique; lvs_name uniqueness is guaranteed by the lvstore registry. */
	rc = snprintf(bdev_name, sizeof(bdev_name), "%s/%s", lvs_name, lvol->name);
	if (rc < 0 || (size_t)rc >= sizeof(bdev_name)) {
		SPDK_ERRLOG("bdev name '%s/%s' too long\n", lvs_name, lvol->name);
		return -ENAMETOOLONG;
	}

	/* Check for a duplicate before registering; a clash returns -EEXIST. */
	if (spdk_bdev_get_by_name(bdev_name) != NULL) {
		SPDK_ERRLOG("bdev '%s' already exists\n", bdev_name);
		return -EEXIST;
	}

	bs = lvol->lvol_store->blobstore;
	cluster_sz = spdk_bs_get_cluster_size(bs);

	bdev = calloc(1, sizeof(*bdev));
	if (!bdev) {
		return -ENOMEM;
	}

	bdev->name = strdup(bdev_name);
	bdev->product_name = strdup("s3lvol");
	if (!bdev->name || !bdev->product_name) {
		free(bdev->product_name);
		free(bdev->name);
		free(bdev);
		return -ENOMEM;
	}

	bdev->ctxt     = lvol;
	bdev->fn_table = &s3lvol_fn_table;
	bdev->module   = vbdev_s3lvol_get_module();

	bdev->blocklen = spdk_bs_get_io_unit_size(bs);
	bdev->blockcnt = spdk_blob_get_num_io_units(lvol->blob);
	bdev->uuid     = lvol->uuid;

	bdev->required_alignment = 0;   /* no underlying bdev; the S3 path needs no alignment */
	bdev->write_cache        = 0;

	/* Split I/O at cluster boundaries: an I/O within one cluster never
	 * crosses a blob's extent, and so is never broken into multiple chunk
	 * operations in the bs_dev. */
	bdev->split_on_optimal_io_boundary = true;
	bdev->optimal_io_boundary = cluster_sz / bdev->blocklen;

	/* Key: s3_bs_dev only supports iovcnt == 1; let the bdev layer split for
	 * us. */
	bdev->max_num_segments = 1;

	rc = spdk_bdev_register(bdev);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to register bdev %s: %s\n",
			    bdev_name, spdk_strerror(-rc));
		free(bdev->product_name);
		free(bdev->name);
		free(bdev);
		return rc;
	}

	lvol->bdev = bdev;

	SPDK_NOTICELOG("Registered bdev '%s': %" PRIu64 " x %u bytes = %" PRIu64
		       " MiB%s (optimal_io_boundary=%u, max_num_segments=%u)\n",
		       bdev_name, bdev->blockcnt, bdev->blocklen,
		       (bdev->blockcnt * bdev->blocklen) / (1024 * 1024),
		       spdk_blob_is_read_only(lvol->blob) ? " [read-only]" : "",
		       bdev->optimal_io_boundary, bdev->max_num_segments);

	return 0;
}

void
vbdev_s3lvol_bdev_unregister(struct spdk_lvol *lvol,
			     spdk_bdev_unregister_cb cb_fn, void *cb_arg)
{
	if (!lvol || !lvol->bdev) {
		if (cb_fn) {
			cb_fn(cb_arg, 0);
		}
		return;
	}

	/* Asynchronous. The bdev's destruct first closes the lvol, then frees the
	 * bdev struct (see s3lvol_destruct / s3lvol_lvol_closed). */
	spdk_bdev_unregister(lvol->bdev, cb_fn, cb_arg);
}
