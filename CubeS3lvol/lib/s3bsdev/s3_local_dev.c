/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_local_dev -- super block and region layout on the local bdev 
 *
 *   Design notes live in include/s3lvol/s3_local_dev.h.
 */

#include "spdk/stdinc.h"
#include "spdk/bdev.h"
#include "spdk/crc32.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/thread.h"

#include "s3lvol/s3_local_dev.h"

struct s3_local_dev {
	/* WAL bdev: super + journal + WAL ring. In a single-bdev layout the
	 * cache lives here too. */
	struct spdk_bdev_desc   *wal_desc;
	struct spdk_io_channel  *wal_ch;

	/* Cache bdev in a dual-bdev layout; NULL when single. */
	struct spdk_bdev_desc   *cache_desc;
	struct spdk_io_channel  *cache_ch;

	bool                     dual_bdev;

	struct s3_region         regions[S3_REGION_COUNT];
	struct s3_super_block    super;

	/* Thread that opened the bdevs. Channels are per-thread, so using them
	 * from another thread is a bug. */
	struct spdk_thread      *owner_thread;
};

/* ==========================================================================
 * Super block CRC
 * ========================================================================== */

static uint32_t
s3_super_calc_crc(const struct s3_super_block *sb)
{
	struct s3_super_block tmp;

	/* Treat the crc field itself as zero, otherwise verification could
	 * never reproduce the same value. */
	memcpy(&tmp, sb, sizeof(tmp));
	tmp.crc = 0;

	return spdk_crc32c_update(&tmp, sizeof(tmp), ~0u);
}

/* ==========================================================================
 * There are no synchronous I/O helpers here, and none should be added back
 *
 * This file used to have a sync_write()/sync_read() pair that submitted a bdev
 * I/O and then spun in place: `while (!done) spdk_thread_poll()`. Both are
 * gone.
 *
 * They were removed not because they were slow, but because *nesting
 * spdk_thread_poll() corrupts the spdk_thread state machine*. The full
 * analysis is in the header comment of include/s3lvol/s3_journal.h; in short:
 *
 *   - The outer reactor is already iterating active_pollers; the inner poll
 *     runs that same set again. A poller's state gets flipped to RUNNING by
 *     the inner pass, and when the outer pass resumes it hits
 *     `default: assert(false)` in thread_execute_poller(). Release builds are
 *     compiled with -DNDEBUG, so the assert vanishes and it degrades into
 *     silent state-machine corruption.
 *   - Completion on the local bdev itself depends on a poller (aio relies on
 *     bdev_aio_group_poll). If the nesting happens inside that poller's own
 *     call stack it is already RUNNING, so the inner pass cannot run it --
 *     the completion never arrives and the while loop spins forever.
 *
 * One might argue that format/open are only called from the control plane,
 * which is not a reactor callback, so it would be safe there. Three reasons
 * not to keep them anyway:
 *
 *   1. update_checkpoint() genuinely runs on the data plane (when a
 *      checkpoint completes) and shared the same helpers -- keeping them
 *      guarantees the data plane ends up using them.
 *   2. RPC handlers themselves run on a reactor (spdk_rpc_accept is a poller
 *      in app.c), so "control plane == not a reactor" does not hold in SPDK.
 *   3. With a synchronous variant within reach, someone will eventually call
 *      it from the data plane, and this class of bug does not show up in unit
 *      tests -- a unit test has a single thread and no other pollers, which
 *      happens to be the only situation where nested polling is safe.
 * ========================================================================== */


/* ==========================================================================
 * Opening and closing bdevs
 * ========================================================================== */

static void
s3_local_dev_event_cb(enum spdk_bdev_event_type type, struct spdk_bdev *bdev,
		      void *event_ctx)
{
	(void)event_ctx;

	/* REMOVE means the underlying bdev was yanked. We cannot clean up in
	 * place here -- the journal or WAL may still have I/O in flight. Log
	 * it and let the upper layer finish through the normal detach path. */
	SPDK_WARNLOG("Local bdev '%s' event %d; s3lvol may lose its WAL/cache\n",
		     spdk_bdev_get_name(bdev), type);
}

static void
s3_local_dev_free(struct s3_local_dev *dev)
{
	if (!dev) {
		return;
	}
	if (dev->cache_ch) {
		spdk_put_io_channel(dev->cache_ch);
	}
	if (dev->cache_desc) {
		spdk_bdev_close(dev->cache_desc);
	}
	if (dev->wal_ch) {
		spdk_put_io_channel(dev->wal_ch);
	}
	if (dev->wal_desc) {
		spdk_bdev_close(dev->wal_desc);
	}
	free(dev);
}

static int
s3_local_dev_open_bdevs(struct s3_local_dev *dev, const char *wal_name,
			const char *cache_name)
{
	int rc;

	rc = spdk_bdev_open_ext(wal_name, true, s3_local_dev_event_cb, dev,
				&dev->wal_desc);
	if (rc != 0) {
		SPDK_ERRLOG("Could not open WAL bdev '%s': %s\n",
			    wal_name, spdk_strerror(-rc));
		return rc;
	}
	dev->wal_ch = spdk_bdev_get_io_channel(dev->wal_desc);
	if (!dev->wal_ch) {
		SPDK_ERRLOG("Could not get IO channel for '%s'\n", wal_name);
		return -ENOMEM;
	}

	/* The block size must divide 4 KiB evenly -- both the super block and
	 * the journal are organized in 4 KiB blocks (P4 fixes block_size). */
	struct spdk_bdev *bdev = spdk_bdev_desc_get_bdev(dev->wal_desc);
	uint32_t bs = spdk_bdev_get_block_size(bdev);

	if (S3_SUPER_SIZE % bs != 0) {
		SPDK_ERRLOG("WAL bdev '%s' block size %u does not divide "
			    "%d\n", wal_name, bs, S3_SUPER_SIZE);
		return -EINVAL;
	}

	if (cache_name && strcmp(cache_name, wal_name) != 0) {
		rc = spdk_bdev_open_ext(cache_name, true, s3_local_dev_event_cb,
					dev, &dev->cache_desc);
		if (rc != 0) {
			SPDK_ERRLOG("Could not open cache bdev '%s': %s\n",
				    cache_name, spdk_strerror(-rc));
			return rc;
		}
		dev->cache_ch = spdk_bdev_get_io_channel(dev->cache_desc);
		if (!dev->cache_ch) {
			return -ENOMEM;
		}
		dev->dual_bdev = true;
	}

	return 0;
}

/* ==========================================================================
 * Formatting
 * ========================================================================== */

static uint64_t
bdev_capacity(struct spdk_bdev_desc *desc)
{
	struct spdk_bdev *bdev = spdk_bdev_desc_get_bdev(desc);

	return spdk_bdev_get_num_blocks(bdev) * spdk_bdev_get_block_size(bdev);
}

/* Context shared by format's two writes.
 *
 * The dev is fully built before the first write is submitted (bdevs opened,
 * layout computed, super block assembled), but it is *not handed to the caller
 * until the callback fires* -- on a mid-way failure this context frees it, so
 * the caller either gets a usable dev or NULL. */
struct local_dev_format_ctx {
	struct s3_local_dev     *dev;
	void                    *buf;
	s3_local_dev_open_cb     cb_fn;
	void                    *cb_arg;
	char                    *wal_name;
	char                    *cache_name;
};

static void
format_ctx_finish(struct local_dev_format_ctx *ctx, int status)
{
	struct s3_local_dev *dev = ctx->dev;
	s3_local_dev_open_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;

	if (ctx->buf) {
		spdk_dma_free(ctx->buf);
	}
	free(ctx->wal_name);
	free(ctx->cache_name);
	free(ctx);

	if (status != 0) {
		s3_local_dev_free(dev);
		cb_fn(cb_arg, NULL, status);
		return;
	}

	cb_fn(cb_arg, dev, 0);
}

static void
format_journal_cleared(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct local_dev_format_ctx *ctx = cb_arg;
	struct s3_local_dev *dev = ctx->dev;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		SPDK_ERRLOG("Failed to clear journal head\n");
		format_ctx_finish(ctx, -EIO);
		return;
	}

	SPDK_NOTICELOG("Formatted local layout on '%s'%s%s:\n",
		       ctx->wal_name,
		       dev->dual_bdev ? " + " : "",
		       dev->dual_bdev && ctx->cache_name ? ctx->cache_name : "");
	SPDK_NOTICELOG("  super   @ %" PRIu64 " (%" PRIu64 " B)\n",
		       dev->regions[S3_REGION_SUPER].offset,
		       dev->regions[S3_REGION_SUPER].size);
	SPDK_NOTICELOG("  journal @ %" PRIu64 " (%" PRIu64 " MiB)\n",
		       dev->regions[S3_REGION_JOURNAL].offset,
		       dev->regions[S3_REGION_JOURNAL].size / (1024 * 1024));
	SPDK_NOTICELOG("  wal     @ %" PRIu64 " (%" PRIu64 " MiB)\n",
		       dev->regions[S3_REGION_WAL].offset,
		       dev->regions[S3_REGION_WAL].size / (1024 * 1024));
	SPDK_NOTICELOG("  cache   @ %" PRIu64 " (%" PRIu64 " MiB)%s\n",
		       dev->regions[S3_REGION_CACHE].offset,
		       dev->regions[S3_REGION_CACHE].size / (1024 * 1024),
		       dev->dual_bdev ? " [separate bdev]" : "");

	format_ctx_finish(ctx, 0);
}

static void
format_super_written(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct local_dev_format_ctx *ctx = cb_arg;
	struct s3_local_dev *dev = ctx->dev;
	int rc;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		SPDK_ERRLOG("Failed to write super block\n");
		format_ctx_finish(ctx, -EIO);
		return;
	}

	/* Zero the journal head so a scan can tell "there are no records here".
	 * Only the first block is cleared -- replay stops at the first invalid
	 * record, so there is no need to wipe the whole region (it can be
	 * hundreds of MiB, which would make formatting very slow). */
	memset(ctx->buf, 0, S3_SUPER_SIZE);

	rc = spdk_bdev_write(dev->wal_desc, dev->wal_ch, ctx->buf,
			     dev->regions[S3_REGION_JOURNAL].offset,
			     S3_SUPER_SIZE, format_journal_cleared, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit journal head clear: %d\n", rc);
		format_ctx_finish(ctx, rc);
	}
}

void
s3_local_dev_format(const struct s3_local_dev_format_opts *opts,
		    s3_local_dev_open_cb cb_fn, void *cb_arg)
{
	struct local_dev_format_ctx *ctx;
	struct s3_local_dev *dev;
	uint64_t wal_cap, offset;
	uint64_t journal_size, wal_size;
	int rc;

	assert(cb_fn != NULL);

	if (!opts || !opts->wal_bdev_name || !opts->lvs_name ||
	    opts->capacity_bytes == 0) {
		cb_fn(cb_arg, NULL, -EINVAL);
		return;
	}
	/* Truncating the name would produce a disk that fails its own attach
	 * check for no visible reason, so refuse it here. */
	if (strlen(opts->lvs_name) >= S3_LVS_NAME_MAX) {
		SPDK_ERRLOG("lvstore name '%s' does not fit in the super block "
			    "(max %d characters)\n", opts->lvs_name,
			    S3_LVS_NAME_MAX - 1);
		cb_fn(cb_arg, NULL, -ENAMETOOLONG);
		return;
	}

	journal_size = opts->journal_size ? opts->journal_size :
		       S3_JOURNAL_DEFAULT_SIZE;
	wal_size = opts->wal_size ? opts->wal_size : S3_WAL_DEFAULT_SIZE;

	/* Round both up to 4 KiB so every region offset below stays block
	 * aligned. */
	journal_size = SPDK_ALIGN_CEIL(journal_size, S3_SUPER_SIZE);
	wal_size     = SPDK_ALIGN_CEIL(wal_size, S3_SUPER_SIZE);

	dev = calloc(1, sizeof(*dev));
	if (!dev) {
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	dev->owner_thread = spdk_get_thread();

	rc = s3_local_dev_open_bdevs(dev, opts->wal_bdev_name,
				     opts->cache_bdev_name);
	if (rc != 0) {
		s3_local_dev_free(dev);
		cb_fn(cb_arg, NULL, rc);
		return;
	}

	wal_cap = bdev_capacity(dev->wal_desc);

	/* Layout: super -> journal -> WAL ring -> (single bdev only) cache */
	offset = 0;
	dev->regions[S3_REGION_SUPER].offset = offset;
	dev->regions[S3_REGION_SUPER].size   = S3_SUPER_SIZE;
	offset += S3_SUPER_SIZE;

	dev->regions[S3_REGION_JOURNAL].offset = offset;
	dev->regions[S3_REGION_JOURNAL].size   = journal_size;
	offset += journal_size;

	dev->regions[S3_REGION_WAL].offset = offset;
	dev->regions[S3_REGION_WAL].size   = wal_size;
	offset += wal_size;

	if (offset > wal_cap) {
		SPDK_ERRLOG("WAL bdev '%s' too small: need %" PRIu64 " bytes for "
			    "super+journal+WAL, have %" PRIu64 "\n",
			    opts->wal_bdev_name, offset, wal_cap);
		s3_local_dev_free(dev);
		cb_fn(cb_arg, NULL, -ENOSPC);
		return;
	}

	if (dev->dual_bdev) {
		dev->regions[S3_REGION_CACHE].offset = 0;
		dev->regions[S3_REGION_CACHE].size   = bdev_capacity(dev->cache_desc);
	} else {
		/* Single bdev: the cache takes whatever is left after the WAL
		 * ring. */
		dev->regions[S3_REGION_CACHE].offset = offset;
		dev->regions[S3_REGION_CACHE].size   = wal_cap - offset;
		if (dev->regions[S3_REGION_CACHE].size < opts->chunk_size) {
			SPDK_WARNLOG("No room left for chunk cache on '%s' "
				     "(%" PRIu64 " bytes); cache disabled\n",
				     opts->wal_bdev_name,
				     dev->regions[S3_REGION_CACHE].size);
			dev->regions[S3_REGION_CACHE].size = 0;
		}
	}

	for (int i = 0; i < S3_REGION_COUNT; i++) {
		dev->regions[i].valid = dev->regions[i].size > 0;
	}

	/* Assemble the super block */
	memset(&dev->super, 0, sizeof(dev->super));
	dev->super.magic      = S3_SUPER_MAGIC;
	dev->super.version    = S3_SUPER_VERSION;
	dev->super.block_size = 4096;
	dev->super.chunk_size = opts->chunk_size;
	dev->super.dual_bdev  = dev->dual_bdev ? 1 : 0;
	dev->super.capacity_bytes = opts->capacity_bytes;
	snprintf(dev->super.lvs_name, sizeof(dev->super.lvs_name), "%s",
		 opts->lvs_name);
	/* lvs_uuid stays all-zeroes: blobstore generates it and does not accept
	 * one from outside, so it is written back once spdk_lvs_init() has run.
	 * Zero means "unknown" to the attach path, which is the honest state for
	 * a disk formatted but never paired. */
	for (int i = 0; i < S3_REGION_COUNT; i++) {
		dev->super.regions[i].offset = dev->regions[i].offset;
		dev->super.regions[i].size   = dev->regions[i].size;
	}
	/* A fresh disk has no checkpoint yet. LSN starts at 0 and the journal
	 * is written from the beginning. */
	dev->super.checkpoint_gen = 0;
	dev->super.checkpoint_lsn = 0;
	dev->super.crc = s3_super_calc_crc(&dev->super);

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		s3_local_dev_free(dev);
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	ctx->dev    = dev;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;
	/* The bdev names are only used for the summary log. *Keep our own
	 * copies*: now that this is asynchronous the callback can run long
	 * after the caller returned, by which point the caller's strings (an
	 * RPC-decoded name, say) are already freed. */
	ctx->wal_name = strdup(opts->wal_bdev_name);
	if (opts->cache_bdev_name) {
		ctx->cache_name = strdup(opts->cache_bdev_name);
	}
	if (!ctx->wal_name || (opts->cache_bdev_name && !ctx->cache_name)) {
		format_ctx_finish(ctx, -ENOMEM);
		return;
	}

	ctx->buf = spdk_dma_zmalloc(S3_SUPER_SIZE, 4096, NULL);
	if (!ctx->buf) {
		format_ctx_finish(ctx, -ENOMEM);
		return;
	}
	memcpy(ctx->buf, &dev->super, sizeof(dev->super));

	rc = spdk_bdev_write(dev->wal_desc, dev->wal_ch, ctx->buf,
			     dev->regions[S3_REGION_SUPER].offset, S3_SUPER_SIZE,
			     format_super_written, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit super block write: %d\n", rc);
		format_ctx_finish(ctx, rc);
	}
}

/* ==========================================================================
 * Opening an existing layout
 * ========================================================================== */

struct local_dev_open_ctx {
	struct s3_local_dev     *dev;
	void                    *buf;
	s3_local_dev_open_cb     cb_fn;
	void                    *cb_arg;
	char                    *wal_name;
	bool                     want_dual;
};

static void
open_ctx_finish(struct local_dev_open_ctx *ctx, int status)
{
	struct s3_local_dev *dev = ctx->dev;
	s3_local_dev_open_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;

	if (ctx->buf) {
		spdk_dma_free(ctx->buf);
	}
	free(ctx->wal_name);
	free(ctx);

	if (status != 0) {
		s3_local_dev_free(dev);
		cb_fn(cb_arg, NULL, status);
		return;
	}

	cb_fn(cb_arg, dev, 0);
}

static void
open_super_read(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct local_dev_open_ctx *ctx = cb_arg;
	struct s3_local_dev *dev = ctx->dev;
	struct s3_super_block *sb;
	uint32_t crc;

	spdk_bdev_free_io(bdev_io);

	if (!success) {
		SPDK_ERRLOG("Failed to read super block from '%s'\n", ctx->wal_name);
		open_ctx_finish(ctx, -EIO);
		return;
	}

	sb = ctx->buf;
	if (sb->magic != S3_SUPER_MAGIC) {
		SPDK_ERRLOG("'%s' has no s3lvol super block (magic %" PRIx64", "
			    "expected %" PRIx64 ") - format it first\n",
			    ctx->wal_name, sb->magic, (uint64_t)S3_SUPER_MAGIC);
		open_ctx_finish(ctx, -EILSEQ);
		return;
	}

	crc = s3_super_calc_crc(sb);
	if (crc != sb->crc) {
		SPDK_ERRLOG("Super block CRC mismatch on '%s' (got %08x, "
			    "computed %08x) - the layout is corrupt\n",
			    ctx->wal_name, sb->crc, crc);
		open_ctx_finish(ctx, -EILSEQ);
		return;
	}

	if (sb->version != S3_SUPER_VERSION) {
		SPDK_ERRLOG("Unsupported super block version %u on '%s' "
			    "(this build understands %d)\n",
			    sb->version, ctx->wal_name, S3_SUPER_VERSION);
		open_ctx_finish(ctx, -EPROTO);
		return;
	}

	/* The on-disk layout says dual-bdev but the caller did not pass a cache
	 * bdev -- the cache region would point at a device that is not open.
	 * Fail loudly instead of blowing up at runtime. */
	if (sb->dual_bdev && !ctx->want_dual) {
		SPDK_ERRLOG("'%s' was formatted with a separate cache bdev; "
			    "pass it too\n", ctx->wal_name);
		open_ctx_finish(ctx, -EINVAL);
		return;
	}
	if (!sb->dual_bdev && ctx->want_dual) {
		SPDK_ERRLOG("'%s' was formatted as a single-bdev layout; "
			    "do not pass a cache bdev\n", ctx->wal_name);
		open_ctx_finish(ctx, -EINVAL);
		return;
	}

	memcpy(&dev->super, sb, sizeof(dev->super));
	/* A CRC-valid super block should already carry a terminator, but the name
	 * is fed to strcmp() and to log formatting, so make it impossible for a
	 * non-terminated one to read past the field. */
	dev->super.lvs_name[sizeof(dev->super.lvs_name) - 1] = '\0';
	for (int i = 0; i < S3_REGION_COUNT; i++) {
		dev->regions[i].offset = sb->regions[i].offset;
		dev->regions[i].size   = sb->regions[i].size;
		dev->regions[i].valid  = sb->regions[i].size > 0;
	}

	SPDK_NOTICELOG("Opened local layout on '%s': lvs='%s', capacity=%" PRIu64
		       " MiB, chunk=%u, ckpt_gen=%" PRIu64 ", ckpt_lsn=%" PRIu64
		       ", journal=%" PRIu64 " MiB\n",
		       ctx->wal_name, dev->super.lvs_name,
		       dev->super.capacity_bytes / (1024 * 1024),
		       dev->super.chunk_size,
		       dev->super.checkpoint_gen, dev->super.checkpoint_lsn,
		       dev->regions[S3_REGION_JOURNAL].size / (1024 * 1024));

	open_ctx_finish(ctx, 0);
}

void
s3_local_dev_open(const char *wal_bdev_name, const char *cache_bdev_name,
		  s3_local_dev_open_cb cb_fn, void *cb_arg)
{
	struct local_dev_open_ctx *ctx;
	struct s3_local_dev *dev;
	int rc;

	assert(cb_fn != NULL);

	if (!wal_bdev_name) {
		cb_fn(cb_arg, NULL, -EINVAL);
		return;
	}

	dev = calloc(1, sizeof(*dev));
	if (!dev) {
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	dev->owner_thread = spdk_get_thread();

	rc = s3_local_dev_open_bdevs(dev, wal_bdev_name, cache_bdev_name);
	if (rc != 0) {
		s3_local_dev_free(dev);
		cb_fn(cb_arg, NULL, rc);
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		s3_local_dev_free(dev);
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	ctx->dev = dev;
	ctx->cb_fn = cb_fn;
	ctx->cb_arg = cb_arg;
	/* open_bdevs() already set dual_bdev based on whether the caller passed
	 * a cache bdev, but the validation below compares it against the
	 * on-disk value -- stash it here because the error paths free dev. */
	ctx->want_dual = dev->dual_bdev;
	/* Keep our own copy of the name: the callback may run long after the
	 * caller returned. */
	ctx->wal_name = strdup(wal_bdev_name);
	if (!ctx->wal_name) {
		open_ctx_finish(ctx, -ENOMEM);
		return;
	}

	ctx->buf = spdk_dma_zmalloc(S3_SUPER_SIZE, 4096, NULL);
	if (!ctx->buf) {
		open_ctx_finish(ctx, -ENOMEM);
		return;
	}

	rc = spdk_bdev_read(dev->wal_desc, dev->wal_ch, ctx->buf, 0,
			    S3_SUPER_SIZE, open_super_read, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit super block read: %d\n", rc);
		open_ctx_finish(ctx, rc);
	}
}

void
s3_local_dev_close(struct s3_local_dev *dev)
{
	s3_local_dev_free(dev);
}

/* ==========================================================================
 * Accessors
 * ========================================================================== */

const struct s3_region *
s3_local_dev_get_region(struct s3_local_dev *dev, enum s3_region_id id)
{
	if (!dev || id >= S3_REGION_COUNT) {
		return NULL;
	}
	return &dev->regions[id];
}

struct spdk_bdev_desc *
s3_local_dev_get_desc(struct s3_local_dev *dev, enum s3_region_id id)
{
	if (!dev) {
		return NULL;
	}
	if (id == S3_REGION_CACHE && dev->dual_bdev) {
		return dev->cache_desc;
	}
	return dev->wal_desc;
}

struct spdk_io_channel *
s3_local_dev_get_channel(struct s3_local_dev *dev, enum s3_region_id id)
{
	if (!dev) {
		return NULL;
	}
	if (id == S3_REGION_CACHE && dev->dual_bdev) {
		return dev->cache_ch;
	}
	return dev->wal_ch;
}

const struct s3_super_block *
s3_local_dev_get_super(struct s3_local_dev *dev)
{
	return dev ? &dev->super : NULL;
}

/* ==========================================================================
 * Rewriting the super block in place
 *
 * Two callers update fields after formatting -- the checkpoint pointer and the
 * lvstore uuid -- and both need the same "recompute the CRC, write the whole
 * 4 KiB block, report through a callback" sequence. Sharing it keeps the CRC
 * step from being forgotten in one of them, which would produce a disk that
 * fails its own validation on the next open.
 * ========================================================================== */

struct super_write_ctx {
	void            *buf;
	const char      *what;      /* string literal, for the error message */
	s3_local_dev_cb  cb_fn;
	void            *cb_arg;
};

static void
super_write_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct super_write_ctx *ctx = cb_arg;
	s3_local_dev_cb cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;
	const char *what = ctx->what;

	spdk_bdev_free_io(bdev_io);

	spdk_dma_free(ctx->buf);
	free(ctx);

	if (!success) {
		SPDK_ERRLOG("Failed to persist %s in the super block\n", what);
	}

	if (cb_fn) {
		cb_fn(user_arg, success ? 0 : -EIO);
	}
}

/* Persist dev->super as it currently stands. The caller has already modified the
 * in-memory copy; this recomputes the CRC over it and writes it out. */
static void
super_write(struct s3_local_dev *dev, const char *what,
	    s3_local_dev_cb cb_fn, void *cb_arg)
{
	struct super_write_ctx *ctx;
	int rc;

	dev->super.crc = s3_super_calc_crc(&dev->super);

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		if (cb_fn) {
			cb_fn(cb_arg, -ENOMEM);
		}
		return;
	}
	ctx->what   = what;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;

	ctx->buf = spdk_dma_zmalloc(S3_SUPER_SIZE, 4096, NULL);
	if (!ctx->buf) {
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, -ENOMEM);
		}
		return;
	}
	memcpy(ctx->buf, &dev->super, sizeof(dev->super));

	rc = spdk_bdev_write(dev->wal_desc, dev->wal_ch, ctx->buf,
			     dev->regions[S3_REGION_SUPER].offset, S3_SUPER_SIZE,
			     super_write_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit the %s super block write: %d\n",
			    what, rc);
		spdk_dma_free(ctx->buf);
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, rc);
		}
	}
}

void
s3_local_dev_update_checkpoint(struct s3_local_dev *dev, uint64_t gen,
			       uint64_t lsn, s3_local_dev_cb cb_fn, void *cb_arg)
{
	if (!dev) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	dev->super.checkpoint_gen = gen;
	dev->super.checkpoint_lsn = lsn;

	/* Updating the super block is the last step of a checkpoint, and it
	 * doubles as the proof that "the journal may now be truncated". The
	 * caller must wait for this callback (i.e. for durability) before
	 * truncating -- otherwise a crash pairs an old checkpoint with an
	 * already-truncated journal, which loses metadata.
	 *
	 * This used to be a synchronous write. Going async costs the caller a
	 * continuation, and buys the ability to call this safely from the data
	 * plane, which is where checkpoint completion runs. */
	super_write(dev, "the checkpoint pointer", cb_fn, cb_arg);
}

void
s3_local_dev_set_lvs_uuid(struct s3_local_dev *dev,
			  const struct spdk_uuid *lvs_uuid,
			  s3_local_dev_cb cb_fn, void *cb_arg)
{
	if (!dev || !lvs_uuid) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	spdk_uuid_copy(&dev->super.lvs_uuid, lvs_uuid);
	super_write(dev, "the lvstore uuid", cb_fn, cb_arg);
}
