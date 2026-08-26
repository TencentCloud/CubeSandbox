/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_lvstore -- the lvstore and lvol lifecycle
 *
 *   Chain: s3_client_get_or_create -> s3_bs_dev_create -> spdk_lvs_init ->
 *   spdk_lvol_create -> vbdev_s3lvol_bdev_register
 *
 *   === Why maintain our own lvstore list ===
 *
 *   The upstream lvstore<->bdev pairing table g_spdk_lvol_pairs is static
 *   private (vbdev_lvol.c:20); no external API can insert entries, so the
 *   built-in bdev_lvol_* RPCs are unavailable to us. We keep our own copy for
 *   the bespoke RPCs to look up.
 *
 *   === Why not use the upstream vbdev_lvs_create() ===
 *
 *   It assumes the lvstore sits on a real bdev: it builds its own bs_dev with
 *   spdk_bdev_create_bs_dev_ext() and then unconditionally dereferences
 *   bs_dev->get_base_bdev() (vbdev_lvol.c:286). Our bs_dev has no bdev
 *   underneath; get_base_bdev is NULL and it would segfault. spdk_lvs_init()
 *   itself only needs a struct spdk_bs_dev *, so the wrapper is bypassed and
 *   it is called directly.
 */

#include "spdk/stdinc.h"
#include "spdk/bdev_module.h"
#include "spdk/blob.h"
#include "spdk/log.h"
#include "spdk/lvol.h"
#include "spdk/string.h"
#include "spdk/thread.h"

#include "spdk_internal/lvolstore.h"

#include "s3lvol/s3_bs_dev.h"
#include "s3lvol/s3_checkpoint.h"
#include "s3lvol/s3_chunk_map.h"
#include "s3lvol/s3_client.h"
#include "s3lvol/s3_export.h"
#include "s3lvol/s3_owner.h"
#include "s3lvol/s3_journal.h"
#include "s3lvol/s3_local_dev.h"
#include "s3lvol/s3_types.h"
#include "s3lvol/s3_wal.h"

#include "vbdev_s3lvol.h"

struct s3lvol_lvstore {
	struct spdk_lvol_store  *lvs;
	struct spdk_bs_dev      *bs_dev;
	struct s3_client        *client;
	char                    *name;

	/* The namespace this lvstore was created in. An import defaults to the
	 * same namespace, which is the common case. The COS target is resolved
	 * from this name through rcow_namespace_to_target(). */
	char                    *ns_name;

	/* Auto-generated blobstore name (bstore_XXXXXXXX). Used for spdk_lvs_init
	 * and spdk_lvs_load_ext; the user never sees it. */
	char                    *bs_name;

	/* Local device carrying the metadata journal and the WAL.
	 *
	 * All NULL when the lvstore was created without a wal_bdev, which leaves
	 * it in the direct-to-S3 mode: no crash safety and no protection against
	 * concurrent partial-chunk writes. Only useful for smoke testing against
	 * S3 without a local disk. */
	struct s3_local_dev     *local_dev;
	struct s3_journal       *journal;
	struct s3_wal           *wal;

	/* True once<name>/meta/owner in S3 says this process owns the lvstore.
	 *
	 * Tracked rather than assumed because the marker must only be deleted by
	 * whoever actually wrote it. Releasing one we never acquired would clear
	 * another process's claim -- the exact thing this is meant to prevent. */
	bool                    owner_held;

	TAILQ_ENTRY(s3lvol_lvstore) link;
};

static TAILQ_HEAD(, s3lvol_lvstore) g_lvstores = TAILQ_HEAD_INITIALIZER(g_lvstores);

/* State carried across the create *or* attach chain. The lvstore is only
 * published in g_lvstores once blobstore is up.
 *
 * Both chains share this, and with it the whole unwind (lvs_setup_fail and
 * friends). That is not just to save code: the unwind is the part that is easy to
 * get wrong -- what has to be released depends on how far the chain got, and the
 * bs_dev teardown is asynchronous -- and having two copies of it would mean the
 * rarely exercised one silently rots.
 *
 * The caller's opts do not outlive the first callback, so whatever later steps
 * need is copied in here. */
struct lvs_setup_ctx {
	struct s3lvol_lvstore   *lvs;
	s3lvol_lvs_op_cb         cb_fn;
	void                    *cb_arg;

	struct s3_lvs_opts       opts;
	char                    *wal_bdev_name;
	char                    *cache_bdev_name;
	uint64_t                 journal_size;
	uint64_t                 wal_size;

	/* Attach only. */
	bool                     attaching;
	/* Take the lvstore over even if S3 says somebody else holds it. */
	bool                     force;
	/* Journal LSN to replay from, i.e. what the last checkpoint covered. */
	uint64_t                 from_lsn;
	/* uuid recorded on the local device; all-zeroes when the create never got
	 * as far as writing it back. */
	struct spdk_uuid         expect_uuid;
	/* Create only: s3_head() writes the object size here. Unused -- only the
	 * status matters -- but it has to outlive the async call, so it cannot be
	 * a local in lvs_create_prefix_check(). */
	uint64_t                 prefix_probe_size;
	/* Attach only: outstanding spdk_lvol_open() calls, plus one self
	 * reference so an inline completion cannot finish the round early. */
	uint32_t                 lvols_pending;
	uint32_t                 lvols_ok;
	uint32_t                 lvols_failed;

	/* Carried through the unwind: the bs_dev teardown is asynchronous, so the
	 * failure that started it has to be remembered somewhere. */
	int                      status;
};

/* ==========================================================================
 * Destroy (rcow_delete_lvstore)
 *
 * Unloads the lvstore and then deletes the S3 objects it owned, as opposed to
 * unload, which keeps everything and expects a later attach.
 *
 * The object list is derived rather than listed: s3_list_objects() is still
 * -ENOTSUP, so the data objects come from walking the chunk map (which names
 * every chunk this lvstore has written) and the metadata objects are the four
 * fixed keys, plus one manifest per registered export.
 *
 * What it does *not* cover is orphans -- an object whose mapping never made it
 * into the chunk map, e.g. a create-once delete that failed, or a flush that
 * was cut short before its journal record landed. Those are exactly what GC is
 * for, and until GC exists a destroy can leave some behind. Said plainly here
 * rather than implied, because "delete" reads like it reclaims everything.
 *
 * === Why the unload comes first===
 *
 * The obvious order -- delete, then unload -- is wrong twice over, and the first
 * version of this code had both faults:
 *
 *   - Unload *reads* what it is about to rewrite. Deleting first turns those
 *     metadata reads into 404s, the chunk map is then reported as inconsistent
 *     ("the chunk map entry is wrong"), and spdk_lvs_unload() fails with
 *     -ENOENT. The lvstore is then left in the registry with its data already
 *     gone: unloadable, unattachable, and still listed.
 *
 *   - Unload *writes*. Blobstore flushes its final metadata during unload, those
 *     writes go through create-once like any other, and each one lands as a new
 *     object with a new uuid. Anything enumerated before the unload therefore
 *     misses them -- one leaked object per destroy, every time.
 *
 * So: unload first, take the object list from the final chunk map through
 * s3_bs_dev_set_reap_cb() (the one instant it is both complete and still
 * readable), and delete afterwards. Two consequences follow from the unload
 * having already finished when the deletes are issued:
 *
 *   - The lvstore is freed by then, so nothing here may touch it afterwards.
 *     Everything needed later -- its name, its client -- is taken up front, and
 *     the client is held through a reference of our own.
 *
 *   - A failed unload is now recoverable: nothing has been deleted yet, so the
 *     destroy simply reports the failure and leaves the lvstore intact.
 * ========================================================================== */

/* Same bound as the one s3_bs_dev.c uses for the keys it builds. */
#define LVS_DESTROY_KEY_MAX 512

struct lvs_destroy_ctx {
	/* Copies, because the unload frees the lvstore before the deletes run. */
	char                   *lvs_name;
	struct s3_client*client;

	spdk_lvs_op_complete    cb_fn;
	void                   *cb_arg;

	/* The keys to delete, collected in two stages: the fixed metadata and the
	 * export manifests up front (the export registry is freed with the
	 * lvstore), the data objects from the final chunk map during the unload. */
	char                **keys;
	uint32_t                n_keys;
	uint32_t                cap_keys;

	uint32_t                inflight;
	bool                    issued_all;

	/* Set once the unload succeeded, which is what makes the deletes safe to
	 * issue and the bstore entry safe to remove. */
	bool                    unloaded;

	/* Set when a key could not be recorded. Kept apart from delete_status
	 * because the object is not merely undeleted, it is unaccounted for: no
	 * retry of this destroy will find it, only GC will. */
	bool                    lost_keys;

	/* First delete error. Does not stop the remaining deletes: the lvstore is
	 * already unloaded, so stopping would leave strictly more behind. */
	int                     delete_status;
};

static void lvs_destroy_finish(struct lvs_destroy_ctx *ctx, int status);

/* Record one key for later deletion. */
static int
lvs_destroy_add_key(struct lvs_destroy_ctx *ctx, const char *key)
{
	char *dup;

	if (ctx->n_keys == ctx->cap_keys) {
		uint32_t cap = ctx->cap_keys ? ctx->cap_keys * 2 : 64;
		char **grown = realloc(ctx->keys, cap * sizeof(*grown));

		if (!grown) {
			return -ENOMEM;
		}
		ctx->keys = grown;
		ctx->cap_keys = cap;
	}

	dup = strdup(key);
	if (!dup) {
		return -ENOMEM;
	}
	ctx->keys[ctx->n_keys++] = dup;
	return 0;
}

static void
lvs_destroy_add_key_checked(struct lvs_destroy_ctx *ctx, const char *key)
{
	if (lvs_destroy_add_key(ctx, key) != 0) {
		ctx->lost_keys = true;
	}
}

static void
lvs_destroy_delete_done(void *cb_arg, int status)
{
	struct lvs_destroy_ctx *ctx = cb_arg;

	/* A missing object is the expected outcome for anything already deleted --
	 * the owner marker in particular, which the unload releases on its own. */
	if (status != 0 && status != -ENOENT && ctx->delete_status == 0) {
		ctx->delete_status = status;
	}

	assert(ctx->inflight > 0);
	ctx->inflight--;

	if (ctx->issued_all && ctx->inflight == 0) {
		lvs_destroy_finish(ctx, ctx->delete_status);
	}
}

/* Terminal step: report, drop the client reference and free the context.
 *
 * The bstore.json entry goes last on the success path, so a destroy interrupted
 * halfway still leaves a record of what needs cleaning up. It is kept entirely
 * when the unload failed, because the lvstore is still there. */
static void
lvs_destroy_finish(struct lvs_destroy_ctx *ctx, int status)
{
	uint32_t i;

	/* Only when the lvstore really went away. A failed unload leaves it
	 * loaded and its data intact, and the entry is what a recovery script
	 * needs to find it again. */
	if (ctx->unloaded) {
		bstore_remove_entry(ctx->lvs_name);
	}

	if (!ctx->unloaded) {
		SPDK_ERRLOG("lvstore '%s' not destroyed: the unload failed (%s); "
			    "no object was deleted and the lvstore is still "
			    "loaded\n", ctx->lvs_name, spdk_strerror(-status));
	} else if (ctx->delete_status != 0 || ctx->lost_keys) {
		SPDK_WARNLOG("lvstore '%s' destroyed, but %s; the remaining "
			     "objects need GC\n", ctx->lvs_name,
			     ctx->lost_keys ? "some keys could not be recorded"
					    : "some objects could not be deleted");
	} else {
		SPDK_NOTICELOG("lvstore '%s' destroyed: %" PRIu32 " objects "
			       "deleted\n", ctx->lvs_name, ctx->n_keys);
	}

	if (ctx->cb_fn) {
		ctx->cb_fn(ctx->cb_arg, status);
	}

	for (i = 0; i < ctx->n_keys; i++) {
		free(ctx->keys[i]);
	}
	free(ctx->keys);
	if (ctx->client) {
		s3_client_put(ctx->client);
	}
	free(ctx->lvs_name);
	free(ctx);
}

/* Called from the bs_dev teardown with the final chunk map, just before it is
 * freed. Only collects: the deletes cannot be issued until the unload has
 * finished, and this runs in the middle of it. */
static int
lvs_destroy_reap_one(void *cb_arg, uint64_t chunk_index,
		     const struct spdk_uuid *uuid, uint32_t valid_bytes,
		     uint32_t flags, uint64_t gen)
{
	struct lvs_destroy_ctx *ctx = cb_arg;
	char key[LVS_DESTROY_KEY_MAX];

	s3_chunk_data_key(ctx->lvs_name, uuid, key, sizeof(key));
	lvs_destroy_add_key_checked(ctx, key);
	return 0;
}

static void
lvs_destroy_reap(void *cb_arg, struct s3_chunk_map *map)
{
	struct lvs_destroy_ctx *ctx = cb_arg;

	if (!map) {
		return;
	}
	s3_chunk_map_foreach(map, lvs_destroy_reap_one, ctx);
}

static void
lvs_destroy_unload_done(void *cb_arg, int lvserrno)
{
	struct lvs_destroy_ctx *ctx = cb_arg;
	uint32_t i;

	if (lvserrno != 0) {
		/* Nothing has been deleted, so the lvstore is intact and staying:
		 * s3lvol_lvstore_unload() leaves it in the registry on failure. */
		lvs_destroy_finish(ctx, lvserrno);
		return;
	}

	ctx->unloaded = true;

	/* The lvstore is gone from here on -- only ctx->lvs_name and ctx->client
	 * remain valid. */
	if (ctx->n_keys == 0) {
		lvs_destroy_finish(ctx, 0);
		return;
	}

	for (i = 0; i < ctx->n_keys; i++) {
		int rc;

		ctx->inflight++;
		rc = s3_delete(ctx->client, ctx->keys[i],
			       lvs_destroy_delete_done, ctx);
		if (rc != 0) {
			/* Never queued, so no callback is coming for it. */
			if (ctx->delete_status == 0) {
				ctx->delete_status = rc;
			}
			ctx->inflight--;
		}
	}

	/* Set last: until every delete is queued, a completion that drains
	 * inflight to zero must not finish the destroy. */
	ctx->issued_all = true;
	if (ctx->inflight == 0) {
		lvs_destroy_finish(ctx, ctx->delete_status);
	}
}

void
s3lvol_lvstore_destroy(struct s3lvol_lvstore *lvs,
		       spdk_lvs_op_complete cb_fn, void *cb_arg)
{
	static const char * const meta_suffix[] = {
		"owner", "checkpoint", "exports.json", "imports.json",
	};
	struct lvs_destroy_ctx *ctx;
	const struct s3_target *target;
	struct s3lvol_export *exp;
	uint32_t n_exports = 0;
	size_t n;
	int rc;

	if (!lvs || !lvs->lvs) {
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

	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;
	ctx->lvs_name = strdup(lvs->name);
	if (!ctx->lvs_name) {
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, -ENOMEM);
		}
		return;
	}

	/* A reference of our own, because the deletes outlive the lvstore: the
	 * unload frees it, and with it its reference to this same pooled client.
	 * Resolved through the namespace while the lvstore is still here to name
	 * it. */
	target = rcow_namespace_to_target(lvs->ns_name);
	if (!target) {
		SPDK_ERRLOG("lvstore '%s': namespace '%s' is no longer "
			    "configured, so its objects cannot be deleted\n",
			    lvs->name, lvs->ns_name ? lvs->ns_name : "(null)");
		rc = -ENODEV;
		goto err;
	}
	rc = s3_client_get_or_create(target, &ctx->client);
	if (rc != 0) {
		SPDK_ERRLOG("lvstore '%s': could not hold an S3 client for the "
			    "deletes: %s\n", lvs->name, spdk_strerror(-rc));
		goto err;
	}

	/* The fixed metadata keys. */
	for (n = 0; n < SPDK_COUNTOF(meta_suffix); n++) {
		char key[LVS_DESTROY_KEY_MAX];

		snprintf(key, sizeof(key), "%s/meta/%s", lvs->name,
			 meta_suffix[n]);
		lvs_destroy_add_key_checked(ctx, key);
	}

	/* Export manifests, collected now because the registry holding them is
	 * freed with the lvstore. Anything still importing one of these on another
	 * node is about to lose the objects behind it -- there is no way to ask,
	 * since the reference lives in the other node's registry, so this is left
	 * as the caller's responsibility and merely stated in the log. */
	for (exp = s3lvol_export_first(lvs); exp != NULL;
	     exp = s3lvol_export_next(exp)) {
		struct s3lvol_export_entry e;
		char key[LVS_DESTROY_KEY_MAX];

		s3lvol_export_get(exp, &e);
		snprintf(key, sizeof(key), "%s/exports/%s.json",
			 lvs->name, e.export_uuid);
		lvs_destroy_add_key_checked(ctx, key);
		n_exports++;
	}
	if (n_exports > 0) {
		SPDK_WARNLOG("lvstore '%s': deleting %" PRIu32 " export "
			     "manifest(s); any node still importing one of them "
			     "will start failing reads\n", lvs->name, n_exports);
	}

	/* The data objects come later, from the final chunk map. Registered before
	 * the unload because that is what triggers it. */
	if (lvs->bs_dev) {
		s3_bs_dev_set_reap_cb(lvs->bs_dev, lvs_destroy_reap, ctx);
	}

	s3lvol_lvstore_unload(lvs, lvs_destroy_unload_done, ctx);
	return;

err:
	free(ctx->lvs_name);
	free(ctx);
	if (cb_fn) {
		cb_fn(cb_arg, rc);
	}
}

/* ==========================================================================
 * Lookup and iteration
 * ========================================================================== */

struct s3lvol_lvstore *
s3lvol_lvstore_pick_one(void)
{
	struct s3lvol_lvstore *lvs, *found = NULL;

	TAILQ_FOREACH(lvs, &g_lvstores, link) {
		if (found) {
			return NULL;	/* more than one */
		}
		found = lvs;
	}
	return found;
}

unsigned
s3lvol_lvstore_count(void)
{
	struct s3lvol_lvstore *lvs;
	unsigned n = 0;

	TAILQ_FOREACH(lvs, &g_lvstores, link) {
		n++;
	}
	return n;
}

struct s3lvol_lvstore *
s3lvol_lvstore_find(const char *name)
{
	struct s3lvol_lvstore *lvs;

	if (!name) {
		return NULL;
	}
	TAILQ_FOREACH(lvs, &g_lvstores, link) {
		if (strcmp(lvs->name, name) == 0) {
			return lvs;
		}
	}
	return NULL;
}

struct s3lvol_lvstore *
s3lvol_lvstore_first(void)
{
	return TAILQ_FIRST(&g_lvstores);
}

struct s3lvol_lvstore *
s3lvol_lvstore_next(struct s3lvol_lvstore *prev)
{
	return prev ? TAILQ_NEXT(prev, link) : NULL;
}

struct s3lvol_lvstore *
s3lvol_lvstore_find_by_lvs(struct spdk_lvol_store *store)
{
	struct s3lvol_lvstore *lvs;

	if (!store) {
		return NULL;
	}
	/* Returns NULL while an lvstore is still loading -- lvs->lvs is only filled
	 * in once spdk_lvs_load_ext() completes, and the esnap callback fires before
	 * that. Callers that run during load must not depend on this; it is here so
	 * they can name the lvstore in a message when it happens to be known. */
	TAILQ_FOREACH(lvs, &g_lvstores, link) {
		if (lvs->lvs == store) {
			return lvs;
		}
	}
	return NULL;
}

struct spdk_lvol_store *
s3lvol_lvstore_get_lvs(struct s3lvol_lvstore *lvs)
{
	return lvs ? lvs->lvs : NULL;
}

struct s3_client *
s3lvol_lvstore_get_client(struct s3lvol_lvstore *lvs)
{
	return lvs ? lvs->client : NULL;
}

struct spdk_bs_dev *
s3lvol_lvstore_get_bs_dev(struct s3lvol_lvstore *lvs)
{
	return lvs ? lvs->bs_dev : NULL;
}

const char *
s3lvol_lvstore_get_namespace(struct s3lvol_lvstore *lvs)
{
	return lvs ? lvs->ns_name : NULL;
}

const char *
s3lvol_lvstore_get_name(struct s3lvol_lvstore *lvs)
{
	return lvs ? lvs->name : NULL;
}

/* ==========================================================================
 * lvstore creation
 *
 * The chain, when a wal_bdev was given:
 *
 *   s3_client_get_or_create
 *   -> s3_local_dev_format ... super block and region layout
 *   -> s3_journal_create ... format the metadata journal
 *   -> s3_wal_create ... format the write-ahead log
 *   -> s3_bs_dev_create
 *   -> s3_bs_dev_attach_journal ... chunk map changes become durable
 *   -> s3_bs_dev_attach_wal ... writes are acknowledged from the log
 *   -> spdk_lvs_init
 *
 * Without a wal_bdev the middle steps are skipped and the bs_dev writes
 * straight to S3 (the direct-to-S3 behaviour).
 *
 * Every step is asynchronous, hence the callback chain. The whole thing runs on
 * the thread that called s3lvol_lvstore_create(), which becomes the lvstore's
 * owner thread -- the journal, the WAL, the overlay and the flusher all assume
 * that and hold no locks.
 * ========================================================================== */

/* Release the local device stack.
 *
 * The order is forced by who writes to what:
 *
 *   bs_dev (its flusher writes the WAL, its chunk map writes the journal)
 *   -> journal
 *   -> local_dev
 *
 * So this may only run once the bs_dev is completely gone. On the WAL path that
 * is *not* when spdk_lvs_unload() completes -- see s3_bs_dev_set_destroy_cb(). */
static void
s3lvol_lvstore_release_local(struct s3lvol_lvstore *lvs)
{
	/* Closed by the bs_dev, which owns the flusher that writes it. */
	lvs->wal = NULL;

	if (lvs->journal) {
		s3_journal_destroy(lvs->journal);
		lvs->journal = NULL;
	}
	if (lvs->local_dev) {
		s3_local_dev_close(lvs->local_dev);
		lvs->local_dev = NULL;
	}
}

static void
s3lvol_lvstore_free(struct s3lvol_lvstore *lvs)
{
	if (!lvs) {
		return;
	}
	/* Cached import manifests go with the lvstore. The registry object in S3
	 * stays: it belongs to the lvstore, and the next attach needs it. */
	s3lvol_xfer_lvstore_fini(lvs);
	s3lvol_xfer_exports_fini(lvs);
	s3lvol_lvstore_release_local(lvs);
	if (lvs->client) {
		s3_client_put(lvs->client);
	}
	/* The bstore.json entry deliberately stays.
	 *
	 * This runs on unload as well as on teardown, and an unload keeps the S3
	 * data: the lvstore is meant to be attachable again afterwards. The entry
	 * is what tells a recovery script that it exists and which namespace and
	 * WAL bdev to attach it with, so dropping it here would leave the data
	 * with nothing pointing at it. Only rcow_delete_lvstore, which destroys
	 * the data, removes the entry. */
	free(lvs->bs_name);
	free(lvs->ns_name);
	free(lvs->name);
	free(lvs);
}

static void
lvs_setup_ctx_free(struct lvs_setup_ctx *ctx)
{
	free(ctx->wal_bdev_name);
	free(ctx->cache_bdev_name);
	free(ctx);
}

static void lvs_setup_report(struct lvs_setup_ctx *ctx);

/* The owner marker is gone; carry on failing. */
static void
lvs_setup_owner_released(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	/* s3_owner_release() has already logged anything worth logging, and there
	 * is nothing useful to do about it here -- the setup is failing either
	 * way, and the original error is the one worth reporting. */
	(void)status;

	lvs_setup_report(ctx);
}

/* Terminal step of both chains: publish the lvstore, or tear everything down. */
static void
lvs_setup_report(struct lvs_setup_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	s3lvol_lvs_op_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;
	int status = ctx->status;
	bool ctx_attaching = ctx->attaching;

	/* Claimed the lvstore in S3 but could not bring it up. Give the claim back
	 * before unwinding: leaving it behind would make the next attach demand
	 * force=true for no reason, and force=true is supposed to mean "I have
	 * checked that the other owner is really gone".
	 *
	 * Clearing owner_held first is what stops this from recursing: the release
	 * completion comes back here and takes the normal path. */
	if (status != 0 && lvs->owner_held) {
		lvs->owner_held = false;
		s3_owner_release(lvs->client, lvs->name,
				 lvs_setup_owner_released, ctx);
		return;
	}

	lvs_setup_ctx_free(ctx);

	if (status != 0) {
		s3lvol_lvstore_free(lvs);
		if (cb_fn) {
			cb_fn(cb_arg, NULL, status);
		}
		return;
	}

	TAILQ_INSERT_TAIL(&g_lvstores, lvs, link);

	SPDK_NOTICELOG("lvstore '%s' %s: cluster=%" PRIu64 " bytes, "
		       "%" PRIu64 " free clusters, write path=%s\n",
		       lvs->name, ctx_attaching ? "attached" : "ready",
		       spdk_bs_get_cluster_size(lvs->lvs->blobstore),
		       spdk_bs_free_cluster_count(lvs->lvs->blobstore),
		       lvs->wal ? "WAL" : "direct-to-S3");

	if (cb_fn) {
		cb_fn(cb_arg, lvs, 0);
	}
}

/* The bs_dev finished tearing itself down after a failed create or attach. Only
 * now is it safe to release the journal and the local device. */
static void
lvs_setup_bs_dev_gone(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	(void)status;
	ctx->lvs->bs_dev = NULL;
	lvs_setup_report(ctx);
}

static void
lvs_setup_wal_closed(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to close the WAL while unwinding: %d\n", status);
	}
	ctx->lvs->wal = NULL;
	lvs_setup_report(ctx);
}

/* Failure anywhere in the chain.
 *
 * Three cases, by how far we got:
 *   - a bs_dev exists: it owns the WAL now, and its teardown is asynchronous
 *   - a WAL exists but was never attached: close it here
 *   - neither: report directly
 */
static void
lvs_setup_fail(struct lvs_setup_ctx *ctx, int status)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;

	ctx->status = status;

	if (lvs->bs_dev) {
		s3_bs_dev_set_destroy_cb(lvs->bs_dev, lvs_setup_bs_dev_gone, ctx);
		lvs->bs_dev->destroy(lvs->bs_dev);
		return;
	}
	if (lvs->wal) {
		s3_wal_close(lvs->wal, lvs_setup_wal_closed, ctx);
		return;
	}

	lvs_setup_report(ctx);
}

/* Last step of a successful create: record which lvstore this local device
 * belongs to, so a later attach can refuse to pair it with a different one.
 *
 * A failure here does not fail the create. The lvstore is up and correct; only
 * the attach-time check degrades from "name and uuid" to "name only". Unwinding
 * a working lvstore over a bookkeeping write would be the worse trade. */
static void
lvs_create_uuid_recorded(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_WARNLOG("lvstore '%s': could not record its uuid on the "
			     "local device (%d); a later attach will only be able "
			     "to check the name\n", ctx->lvs->name, status);
	}

	ctx->status = 0;
	lvs_setup_report(ctx);
}

static void
s3lvol_lvs_init_cb(void *cb_arg, struct spdk_lvol_store *lvol_store, int lvserrno)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;

	if (lvserrno != 0) {
		SPDK_ERRLOG("Failed to init lvstore '%s': %s\n",
			    lvs->name, spdk_strerror(-lvserrno));
		/* spdk_lvs_init's failure path already called bs_dev->destroy(),
		 * so it must not be called again. But on the WAL path that
		 * teardown is asynchronous, and the destroy callback registered
		 * before spdk_lvs_init() is what tells us when the
		 * journal and the local device are free. */
		ctx->status = lvserrno;
		if (lvs->bs_dev) {
			return;
		}
		lvs_setup_report(ctx);
		return;
	}

	lvs->lvs = lvol_store;

	/* The create context is about to go away, so drop its destroy callback;
	 * the unload path registers its own. */
	s3_bs_dev_set_destroy_cb(lvs->bs_dev, NULL, NULL);

	/* Record the auto-generated blobstore name in /data/cubelet/rcow/bstore.json, so a
	 * recovery script can find it later. A failure here is logged but not
	 * fatal: the lvstore is up. */
	bstore_save_entry(lvs->name, lvs->bs_name, lvs->ns_name,
			  ctx->wal_bdev_name);

	if (lvs->local_dev) {
		/* Only knowable now: blobstore generates the uuid and refuses to
		 * take one from outside, so the disk was formatted with zeroes
		 * there. */
		s3_local_dev_set_lvs_uuid(lvs->local_dev, &lvol_store->uuid,
					  lvs_create_uuid_recorded, ctx);
		return;
	}

	ctx->status = 0;
	lvs_setup_report(ctx);
}

static void
lvs_create_start_blobstore(struct lvs_setup_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	struct spdk_lvs_opts lvs_opts;
	int rc;

	rc = s3_bs_dev_create(&ctx->opts, NULL, NULL, lvs->client,
			      ctx->opts.capacity_bytes, &lvs->bs_dev);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to create bs_dev for '%s': %d\n", lvs->name, rc);
		lvs_setup_fail(ctx, rc);
		return;
	}

	if (lvs->journal) {
		rc = s3_bs_dev_attach_journal(lvs->bs_dev, lvs->journal,
					      lvs->local_dev);
		if (rc != 0) {
			SPDK_ERRLOG("Failed to attach the journal to '%s': %d\n",
				    lvs->name, rc);
			lvs_setup_fail(ctx, rc);
			return;
		}

		/* Deliberately not checked: a missing or unusable cache region
		 * means reads go to S3, which is how this worked before the cache
		 * existed. It is not a reason to fail creating the lvstore. */
		(void)s3_bs_dev_attach_cache(lvs->bs_dev);
	}

	if (lvs->wal) {
		/* From here on the bs_dev owns closing the WAL. */
		rc = s3_bs_dev_attach_wal(lvs->bs_dev, lvs->wal, NULL);
		if (rc != 0) {
			SPDK_ERRLOG("Failed to attach the WAL to '%s': %d\n",
				    lvs->name, rc);
			lvs_setup_fail(ctx, rc);
			return;
		}
	}

	spdk_lvs_opts_init(&lvs_opts);

	/* The cluster size follows the device's chunk size unless it was asked for
	 * explicitly.
	 *
	 * Two reasons, and the second one is why the default had to change. A cluster
	 * that spans several chunks makes copy-on-write read every one of them: a
	 * write to an unallocated cluster pulls the whole cluster through the back
	 * device, which at a 4 MiB cluster over 1 MiB chunks is four GETs and four
	 * PUTs for one write. And a zero-copy export needs one blob cluster to be
	 * exactly one chunk map entry, so any other ratio sends every handoff down
	 * the copying path -- which is what SPDK's 4 MiB default silently did, while
	 * the device defaulted to 1 MiB chunks.
	 *
	 * An explicit cluster size is still honoured, because an lvstore that will
	 * never be exported may legitimately want a coarser one; it just gets told
	 * what it is giving up. */
	if (ctx->opts.cluster_size) {
		uint32_t chunk_size = s3_bs_dev_get_chunk_size(lvs->bs_dev);

		lvs_opts.cluster_sz = ctx->opts.cluster_size;
		if (chunk_size != 0 && lvs_opts.cluster_sz != chunk_size) {
			SPDK_WARNLOG("lvstore '%s' has a %u-byte cluster over %u-byte "
				     "chunks. Exports of it cannot be zero-copy, and every "
				     "copy-on-write will touch %" PRIu64 " chunk(s).\n", lvs->name,
				     lvs_opts.cluster_sz, chunk_size,
				     spdk_divide_round_up(lvs_opts.cluster_sz, chunk_size));
		}
	} else {
		lvs_opts.cluster_sz = s3_bs_dev_get_chunk_size(lvs->bs_dev);
	}
	/* The blobstore name is auto-generated so the user never has to pick one
	 * that does not already exist on the local device. The user-visible name
	 * (lvs->name) stays as-is for RPC addressing, S3 prefix, and bdev naming. */
	char generated[32];

	bstore_generate_bs_name(generated, sizeof(generated));
	lvs->bs_name = strdup(generated);
	if (!lvs->bs_name) {
		lvs_setup_fail(ctx, -ENOMEM);
		return;
	}
	snprintf(lvs_opts.name, sizeof(lvs_opts.name), "%s", generated);

	/* Registered on the create path too, not only on load. A brand new lvstore
	 * has no esnap clones, but it can acquire one the moment an import runs
	 * against it, and blobstore asks this callback for the parent while creating
	 * the clone -- not only while loading it. spdk_lvs_init() passes it straight
	 * through to spdk_bs_opts, with esnap_ctx set to the lvol store, so the
	 * callback sees the same arguments as on the load path.
	 *
	 * One asymmetry upstream, in case someone reaches for those APIs later: init
	 * does *not* also record it in lvs->esnap_bs_dev_create, which is what
	 * spdk_lvol_set_external_parent() and spdk_lvs_notify_hotplug() call. Neither
	 * is used here; calling one against a freshly created lvstore would find a
	 * NULL there. */
	lvs_opts.esnap_bs_dev_create = s3lvol_esnap_dev_create;

	/* Registered before spdk_lvs_init() because *both* of its failure paths
	 * destroy the bs_dev, and that teardown is asynchronous. */
	s3_bs_dev_set_destroy_cb(lvs->bs_dev, lvs_setup_bs_dev_gone, ctx);

	rc = spdk_lvs_init(lvs->bs_dev, &lvs_opts, s3lvol_lvs_init_cb, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("spdk_lvs_init failed for '%s': %d\n", lvs->name, rc);
		/* Synchronous failure: spdk_lvs_init never took the bs_dev, so we
		 * destroy it ourselves. */
		lvs_setup_fail(ctx, rc);
	}
}

static void
lvs_create_wal_done(void *cb_arg, struct s3_wal *wal, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to format the WAL on '%s': %d\n",
			    ctx->wal_bdev_name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	ctx->lvs->wal = wal;
	lvs_create_start_blobstore(ctx);
}

static void
lvs_create_journal_done(void *cb_arg, struct s3_journal *journal, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to format the journal on '%s': %d\n",
			    ctx->wal_bdev_name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	ctx->lvs->journal = journal;

	s3_wal_create(ctx->lvs->local_dev, NULL, lvs_create_wal_done, ctx);
}

static void
lvs_create_local_dev_done(void *cb_arg, struct s3_local_dev *dev, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to format the local device '%s': %d\n",
			    ctx->wal_bdev_name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	ctx->lvs->local_dev = dev;

	s3_journal_create(dev, lvs_create_journal_done, ctx);
}

/* Common prologue of both chains: allocate the lvstore and the setup context,
 * take a copy of everything the callbacks will still need, and get the S3 client.
 *
 * Nothing here is asynchronous, so a failure is reported through the return value
 * and neither the lvstore nor the context exists afterwards. On success the
 * caller owns *ctx and must finish through lvs_setup_report() or
 * lvs_setup_fail(). */
static int
lvs_setup_begin(const struct s3_lvs_opts *opts, s3lvol_lvs_op_cb cb_fn,
		void *cb_arg, struct lvs_setup_ctx **out)
{
	struct s3lvol_lvstore *lvs;
	struct lvs_setup_ctx *ctx;
	int rc;

	lvs = calloc(1, sizeof(*lvs));
	ctx = calloc(1, sizeof(*ctx));
	if (!lvs || !ctx) {
		free(ctx);
		free(lvs);
		return -ENOMEM;
	}
	ctx->lvs = lvs;
	ctx->cb_fn = cb_fn;
	ctx->cb_arg = cb_arg;

	lvs->name = strdup(opts->lvs_name);
	if (!lvs->name) {
		free(ctx);
		free(lvs);
		return -ENOMEM;
	}

	/* Copied here rather than read back from the client later: the caller's
	 * opts do not survive the first callback, and an import that defaults to
	 * the same namespace needs it long afterwards. */
	lvs->ns_name = strdup(opts->ns_name ? opts->ns_name : "");
	if (!lvs->ns_name) {
		rc = -ENOMEM;
		goto err;
	}

	/* The chain outlives the caller's opts, so copy what later steps need.
	 * s3_bs_dev_create() only reads lvs_name and chunk_size, and lvs_name has
	 * to point at our own copy. */
	ctx->opts = *opts;
	ctx->opts.lvs_name = lvs->name;
	ctx->opts.wal_bdev_name = NULL;
	ctx->opts.cache_bdev_name = NULL;

	if (opts->wal_bdev_name) {
		ctx->wal_bdev_name = strdup(opts->wal_bdev_name);
		if (!ctx->wal_bdev_name) {
			rc = -ENOMEM;
			goto err;
		}
	}
	if (opts->cache_bdev_name) {
		ctx->cache_bdev_name = strdup(opts->cache_bdev_name);
		if (!ctx->cache_bdev_name) {
			rc = -ENOMEM;
			goto err;
		}
	}
	ctx->journal_size = (uint64_t)opts->journal_size_mb * 1024 * 1024;
	ctx->wal_size = (uint64_t)opts->wal_size_mb * 1024 * 1024;

	rc = s3_client_get_or_create(&opts->target, &lvs->client);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to get S3 client for %s: %d\n",
			    opts->target.endpoint, rc);
		goto err;
	}

	*out = ctx;
	return 0;

err:
	lvs_setup_ctx_free(ctx);
	s3lvol_lvstore_free(lvs);
	return rc;
}

/* Defined with the attach chain below; both chains resume through the shared
 * owner callback. */
static void lvs_attach_after_owner(struct lvs_setup_ctx *ctx);

/* Everything the create chain does after it owns the lvstore. */
static void
lvs_create_after_owner(struct lvs_setup_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	struct s3_local_dev_format_opts fopts = {0};

	if (!ctx->wal_bdev_name) {
		/* No local device: the direct-to-S3 mode. Loud about it, because
		 * that mode loses concurrent partial-chunk writes and survives
		 * no crash. It is a smoke-test mode, not a usable
		 * configuration. */
		SPDK_WARNLOG("lvstore '%s' created without a wal_bdev: writes go "
			     "straight to S3, which is neither crash safe nor "
			     "correct under concurrent partial-chunk writes\n",
			     lvs->name);
		lvs_create_start_blobstore(ctx);
		return;
	}

	/* The lvstore uuid does not exist yet -- blobstore generates it and will
	 * not accept one from outside -- so the disk is formatted with zeroes
	 * there and the real value is written back in s3lvol_lvs_init_cb().
	 * Capacity and name *are* recorded now: the attach path reads them back
	 * instead of asking the operator to repeat them. */
	fopts.wal_bdev_name   = ctx->wal_bdev_name;
	fopts.cache_bdev_name = ctx->cache_bdev_name;
	fopts.lvs_name        = lvs->name;
	fopts.capacity_bytes  = ctx->opts.capacity_bytes;
	fopts.chunk_size      = ctx->opts.chunk_size ? ctx->opts.chunk_size :
				S3LVOL_DEFAULT_CHUNK_SIZE;
	fopts.journal_size    = ctx->journal_size;
	fopts.wal_size        = ctx->wal_size;

	s3_local_dev_format(&fopts, lvs_create_local_dev_done, ctx);
}

/* Does this prefix already hold a blobstore?
 *
 * The owner marker answers "is somebody writing to it right now", which is not the
 * same question: a clean unload releases the marker (lvs_unload_maybe_finish), so
 * an lvstore that was stopped properly leaves no trace in it at all. Creating then
 * proceeds and lays a fresh blobstore super block over live data. Two ways in:
 *
 *   - the local bstore.json is gone or was never written on this host, so
 *     rcow_start.sh cannot tell that the prefix is in use and falls back to create;
 *   - two nodes derive the same lvstore name, which is a key prefix, and the first
 *     one to have used it was cleanly stopped.
 *
 * Neither is exotic. The first one happened here.
 *
 * `<prefix>/meta/checkpoint` is the marker used because it is the only object with
 * a deterministic name that outlives a clean shutdown: data objects are uuid-named
 * (and s3_list_objects() is still -ENOTSUP, so they cannot be enumerated), and
 * meta/owner is by design transient.
 *
 * What this does not cover: an lvstore created and cleanly stopped before its first
 * checkpoint ever ran. Its prefix holds nothing but uuid-named data objects, and
 * nothing here can find them. Closing that needs either a working list or an
 * identity object written at create time -- the latter is a small change, but it
 * adds an object whose lifecycle has to be right in every delete path, and getting
 * that wrong turns "create refuses over live data" into "create refuses forever".
 * Left alone deliberately; the window is a create followed by a clean stop with no
 * checkpoint in between.
 *
 * --force skips the check, and has to: it is the documented way to take over a
 * prefix, and it is also the only escape if a checkpoint object is somehow left
 * behind by a prefix that is genuinely dead.
 */
static void
lvs_create_prefix_checked(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	if (status == 0) {
		SPDK_ERRLOG("refusing to create lvstore '%s': the key prefix "
			    "'%s/' in this bucket already holds a blobstore "
			    "(found %s/meta/checkpoint)\n",
			    ctx->lvs->name, ctx->lvs->name, ctx->lvs->name);
		SPDK_ERRLOG("creating over it would write a fresh super block on "
			    "top of live data. Attach it instead, or pick another "
			    "lvstore name; pass force=true only if you know this "
			    "prefix is dead\n");
		lvs_setup_fail(ctx, -EEXIST);
		return;
	}

	if (status != -ENOENT) {
		/* Anything other than a clean 404 leaves the question open, and
		 * the destructive answer is the wrong default: a 403 on a bucket
		 * that does hold a blobstore would otherwise be read as "empty".
		 * force=true still gets through, since that is what it is for. */
		SPDK_ERRLOG("refusing to create lvstore '%s': cannot tell whether "
			    "the prefix '%s/' is already in use (HEAD "
			    "%s/meta/checkpoint failed: %s)\n",
			    ctx->lvs->name, ctx->lvs->name, ctx->lvs->name,
			    spdk_strerror(-status));
		lvs_setup_fail(ctx, status);
		return;
	}

	lvs_create_after_owner(ctx);
}

static void
lvs_create_prefix_check(struct lvs_setup_ctx *ctx)
{
	char key[S3_LVS_NAME_MAX + sizeof("/meta/checkpoint")];
	int rc;

	if (ctx->force) {
		SPDK_WARNLOG("lvstore '%s': force=true, so not checking whether the "
			     "key prefix '%s/' already holds a blobstore\n",
			     ctx->lvs->name, ctx->lvs->name);
		lvs_create_after_owner(ctx);
		return;
	}

	rc = snprintf(key, sizeof(key), "%s/meta/checkpoint", ctx->lvs->name);
	if (rc < 0 || (size_t)rc >= sizeof(key)) {
		/* lvs_name_ok() and the RPC layer bound the name well below this,
		 * so reaching here means one of those grew a hole. */
		SPDK_ERRLOG("lvstore '%s': name too long to build the checkpoint "
			    "key\n", ctx->lvs->name);
		lvs_setup_fail(ctx, -ENAMETOOLONG);
		return;
	}

	rc = s3_head(ctx->lvs->client, key, &ctx->prefix_probe_size,
		     lvs_create_prefix_checked, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("lvstore '%s': cannot probe the prefix: %s\n",
			    ctx->lvs->name, spdk_strerror(-rc));
		lvs_setup_fail(ctx, rc);
	}
}

/* The lvstore is ours (or it is not, and we stop here).
 *
 * Shared by both chains, which is why it dispatches on ctx->attaching: the check
 * itself and its failure handling are identical, only what comes next differs. */
static void
lvs_setup_owner_acquired(void *cb_arg, const struct s3_owner_info *holder,
			 int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;

	if (status != 0) {
		/* s3_owner_acquire() has already logged who holds it and what to
		 * do about it, in more detail than is available here. */
		(void)holder;
		lvs_setup_fail(ctx, status);
		return;
	}

	ctx->lvs->owner_held = true;

	if (ctx->attaching) {
		lvs_attach_after_owner(ctx);
	} else {
		lvs_create_prefix_check(ctx);
	}
}

/* An lvstore's name is its key prefix in the bucket, so it cannot collide with the
 * one place that is addressed without a prefix: export manifests live at
 * `exports/<uuid>.json`, which is what lets an importer find one knowing only the
 * uuid (see s3_export_manifest_key()). An lvstore called "exports" would write its
 * own metadata into that space.
 *
 * Checked on attach as well as create. A bucket written by an older build could
 * hold such an lvstore, and attaching it would be the same collision. */
static int
lvs_name_ok(const char *name)
{
	if (strcmp(name, S3_EXPORTS_DIR) == 0) {
		SPDK_ERRLOG("'%s' is reserved: export manifests live under that "
			    "prefix so that an importer needs nothing but the export "
			    "uuid to find one\n", S3_EXPORTS_DIR);
		return -EINVAL;
	}
	return 0;
}

int
s3lvol_lvstore_create(const struct s3_lvs_opts *opts,
		      s3lvol_lvs_op_cb cb_fn, void *cb_arg)
{
	struct lvs_setup_ctx *ctx;
	int rc;

	if (!opts || !opts->lvs_name || opts->capacity_bytes == 0) {
		return -EINVAL;
	}
	rc = lvs_name_ok(opts->lvs_name);
	if (rc != 0) {
		return rc;
	}
	if (s3lvol_lvstore_find(opts->lvs_name)) {
		SPDK_ERRLOG("lvstore '%s' already exists\n", opts->lvs_name);
		return -EEXIST;
	}

	rc = lvs_setup_begin(opts, cb_fn, cb_arg, &ctx);
	if (rc != 0) {
		return rc;
	}
	ctx->force = opts->force;

	/* Claim the lvstore before touching anything in S3.
	 *
	 * Create checks this too, not just attach: an existing marker under this
	 * name means another process is actively writing to the same key prefix,
	 * and creating over it would have blobstore lay a fresh super block on top
	 * of live data. The uuid is all-zeroes because blobstore has not generated
	 * one yet -- it is a diagnostic field in the marker, nothing keys off it. */
	s3_owner_acquire(ctx->lvs->client, ctx->lvs->name, NULL, ctx->force,
			 lvs_setup_owner_acquired, ctx);
	return 0;
}

/* ==========================================================================
 * lvstore attach -- re-hang an existing lvstore (including crash recovery)
 *
 * The chain mirrors create, with _open in place of _create and two replays
 * inserted:
 *
 *   s3_client_get_or_create
 *   -> s3_local_dev_open ....... read the layout, capacity and identity back
 *   -> s3_journal_open ......... synchronous, reads checkpoint_lsn only
 *   -> s3_wal_open ............. picks the newer of the two super slots
 *   -> s3_bs_dev_create ........ geometry comes from the super block, not the RPC
 *   -> s3_bs_dev_attach_journal
 *   -> s3_journal_replay ....... rebuild chunk_index -> S3 object
 *   -> s3_bs_dev_attach_wal .... the overlay has to exist before the WAL replay
 *   -> s3_wal_replay ........... re-apply writes that never reached S3
 *   -> spdk_lvs_load_ext
 *   -> register a bdev for every lvol that was found
 *
 * === Why the order is exactly this ===
 *
 * *Both replays must precede spdk_lvs_load_ext().* blobstore reads its super
 * block as its very first action, and that read has to already see the recovered
 * state -- there is no way to tell blobstore "reload, the device changed".
 *
 * *The journal must precede the WAL.* The journal says which S3 objects exist;
 * the WAL then re-applies the writes that had not reached them yet. Replaying in
 * the other order would let a journal record overwrite a newer mapping.
 *
 * *attach_wal must precede the WAL replay* because replayed entries land in the
 * overlay, which attach_wal creates.
 *
 * === Identity checking, and what it cannot cover ===
 *
 * Pairing the wrong local device with an lvstore is destructive: replay would
 * write another lvstore's data into this one. Two checks:
 *
 *   1. lvs_name, compared right after s3_local_dev_open, i.e. *before* any
 *      replay. This catches the plausible operator error (pointing attach at a
 *      leftover image from a different lvstore) at zero cost.
 *   2. lvs_uuid, compared once spdk_lvs_load_ext() has run -- the uuid only
 *      exists inside blobstore, so it cannot be checked earlier. This catches
 *      the case the name misses: the lvstore was destroyed and recreated under
 *      the same name while this disk was kept.
 *
 * The second check is therefore *late*: by the time it fires, the WAL has been
 * replayed into the overlay and the flusher may already have uploaded some of
 * it. Closing that window needs identity metadata in S3 that can be read before
 * blobstore is loaded (the meta/owner object is the natural place), which does
 * not exist yet. Until then a uuid mismatch is reported as a failed attach with
 * a loud log, not as silent tolerance.
 * ========================================================================== */

/* Everything that had to be opened has been. Finish the attach. */
static void
lvs_attach_lvols_done(struct lvs_setup_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;

	SPDK_NOTICELOG("lvstore '%s': %u lvol(s) exported as bdevs%s\n",
		       lvs->name, ctx->lvols_ok,
		       ctx->lvols_failed ? " (some could not be opened or "
		       "registered)" : "");

	if (spdk_uuid_is_null(&ctx->expect_uuid) && lvs->local_dev) {
		/* Repair the pairing record left incomplete by an interrupted
		 * create, so the next attach gets the stronger check. Failure is
		 * not fatal for the same reason it is not on the create path. */
		s3_local_dev_set_lvs_uuid(lvs->local_dev, &lvs->lvs->uuid,
					  lvs_create_uuid_recorded, ctx);
		return;
	}

	ctx->status = 0;
	lvs_setup_report(ctx);
}

/* One lvol's blob is open (or failed to open). Give it a bdev.
 *
 * A failure here does not fail the attach. An lvol without a bdev is invisible
 * to nvmf, which matters -- but refusing the whole lvstore over one bad lvol
 * would take the healthy ones down with it, so log, count and carry on. Upstream
 * vbdev_lvol makes the same call in _vbdev_lvs_examine_finish. */
static void
lvs_attach_lvol_opened(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;
	int rc;

	/* NULL lvol means this is the self reference being released by
	 * lvs_attach_open_lvols() after it finished submitting, not a real
	 * completion. Without this it would register a NULL lvol and count a
	 * phantom failure. */
	if (lvol == NULL) {
		goto out;
	}

	if (lvolerrno != 0) {
		SPDK_ERRLOG("lvstore '%s': could not open lvol '%s': %s\n",
			    lvs->name, lvol->name, spdk_strerror(-lvolerrno));

		/* Drop it from the lvstore rather than leaving it behind.
		 *
		 * This is not tidiness: an lvol whose blob never opened has
		 * lvol->blob == NULL, and the unload path would call
		 * spdk_lvol_close() on it, which fails with -EINVAL and makes the
		 * whole lvstore impossible to unload. Upstream removes and frees
		 * it for the same reason.
		 *
		 * Upstream also retries -ENOMEM via retry_open_lvols. Not doing
		 * that here: it would mean draining a retry queue inside the
		 * attach chain, and an lvol short of memory at attach time is
		 * better reported than silently deferred. */
		TAILQ_REMOVE(&lvs->lvs->lvols, lvol, link);
		if (lvs->lvs->lvol_count > 0) {
			lvs->lvs->lvol_count--;
		}
		free(lvol);
		ctx->lvols_failed++;
		goto out;
	}

	/* Only now is this safe: vbdev_s3lvol_bdev_register() needs lvol->blob for
	 * the block count, and spdk_lvs_load() leaves every blob closed -- it
	 * walks them with the blobstore iterator, which closes each one as it
	 * moves on. Registering straight after the load is what returned -EINVAL
	 * on the first live run. */
	rc = vbdev_s3lvol_bdev_register(lvol, lvs->name);
	if (rc != 0) {
		SPDK_ERRLOG("lvstore '%s': could not register a bdev for lvol "
			    "'%s': %d\n", lvs->name, lvol->name, rc);
		/* Left in the list on purpose: the blob *is* open, so the unload
		 * path's spdk_lvol_close() is both correct and necessary. */
		ctx->lvols_failed++;
		goto out;
	}

	ctx->lvols_ok++;

out:
	assert(ctx->lvols_pending > 0);
	if (--ctx->lvols_pending > 0) {
		return;
	}

	lvs_attach_lvols_done(ctx);
}

/* Open every lvol the load found, then export each as a bdev.
 *
 * spdk_lvs_load() only rebuilds the lvol *list*; it leaves the blobs closed.
 * So this is not an optional extra step -- without it every lvol has a NULL
 * blob and nothing can be exported. */
static void
lvs_attach_open_lvols(struct lvs_setup_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	struct spdk_lvol *lvol, *tmp;

	/* Self reference, so an open that completes inline cannot run the
	 * finish before the loop below has submitted everything. */
	ctx->lvols_pending = 1;

	/* _SAFE because the failure path above removes entries from this list. */
	TAILQ_FOREACH_SAFE(lvol, &lvs->lvs->lvols, link, tmp) {
		ctx->lvols_pending++;
		spdk_lvol_open(lvol, lvs_attach_lvol_opened, ctx);
	}

	lvs_attach_lvol_opened(ctx, NULL, 0);
}

/* spdk_lvs_unload() calls its callback unconditionally on the -EBUSY paths, so a
 * NULL one would segfault. Used where there is genuinely nothing to do with the
 * result: the unwind is driven by the bs_dev destroy callback instead. */
static void
lvs_unload_ignored(void *cb_arg, int lvserrno)
{
	(void)cb_arg;

	if (lvserrno != 0) {
		SPDK_ERRLOG("Failed to unload the lvstore while unwinding: %s\n",
			    spdk_strerror(-lvserrno));
	}
}

static void
s3lvol_lvs_load_cb(void *cb_arg, struct spdk_lvol_store *lvol_store, int lvserrno)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;

	if (lvserrno != 0) {
		SPDK_ERRLOG("Failed to load lvstore '%s': %s\n",
			    lvs->name, spdk_strerror(-lvserrno));
		/* Every failure path inside spdk_lvs_load_ext() ends in
		 * spdk_bs_load()/spdk_bs_unload() destroying the bs_dev, exactly
		 * like spdk_lvs_init(). So do not destroy it again; wait for the
		 * destroy callback, which is what says the journal and the local
		 * device may be released. */
		ctx->status = lvserrno;
		if (lvs->bs_dev) {
			return;
		}
		lvs_setup_report(ctx);
		return;
	}

	/* Second identity check. It can only happen here: the uuid lives in
	 * blobstore's own metadata.
	 *
	 * All-zeroes means the create crashed between formatting the disk and
	 * recording the uuid, which is a legitimate state -- accept it and repair
	 * it below rather than refuse an otherwise healthy pair. */
	if (!spdk_uuid_is_null(&ctx->expect_uuid) &&
	    spdk_uuid_compare(&ctx->expect_uuid, &lvol_store->uuid) != 0) {
		char want[SPDK_UUID_STRING_LEN], got[SPDK_UUID_STRING_LEN];

		spdk_uuid_fmt_lower(want, sizeof(want), &ctx->expect_uuid);
		spdk_uuid_fmt_lower(got, sizeof(got), &lvol_store->uuid);

		SPDK_ERRLOG("local device '%s' belongs to lvstore uuid %s, but "
			    "'%s' in the bucket is %s -- the lvstore was most "
			    "likely destroyed and recreated under the same name "
			    "while this device was kept. Refusing to attach; the "
			    "log on this device has already been replayed into "
			    "memory but the device itself is untouched.\n",
			    ctx->wal_bdev_name, want, lvs->name, got);

		lvs->lvs = NULL;
		ctx->status = -EINVAL;
		/* Unloading is what destroys the bs_dev, and the destroy callback
		 * is still ours, so the unwind continues from there.
		 * spdk_lvs_unload() also frees lvol_store, hence not storing it. */
		if (spdk_lvs_unload(lvol_store, lvs_unload_ignored, NULL) != 0) {
			/* It refused, so no bs_dev teardown is coming from it and
			 * nothing else will ever complete the unwind. */
			lvs_setup_fail(ctx, -EINVAL);
		}
		return;
	}

	lvs->lvs = lvol_store;

	/* The create context is about to go away, so drop its destroy callback;
	 * the unload path registers its own. */
	s3_bs_dev_set_destroy_cb(lvs->bs_dev, NULL, NULL);

	/* Recover the generated blobstore name from blobstore's own metadata: an
	 * attach never generated one, and spdk_lvs_load_ext() has just read it
	 * back from the super block. Without this lvs->bs_name stays NULL for the
	 * whole life of an attached lvstore. */
	if (!lvs->bs_name) {
		lvs->bs_name = strdup(lvol_store->name);
	}

	/* Re-record the entry rather than assume one is there.
	 *
	 * An attach is the point where this process learns that the lvstore exists
	 * and which namespace and WAL bdev reach it, which is exactly what the
	 * entry holds -- so writing it here makes the file self-healing if it was
	 * lost, and refreshes wal_bdev when an attach uses a different one from
	 * the create. */
	bstore_save_entry(lvs->name, lvs->bs_name, lvs->ns_name,
			  ctx->wal_bdev_name);

	/* Opening the lvols is asynchronous, so the rest of the attach continues
	 * from lvs_attach_lvols_done(). */
	lvs_attach_open_lvols(ctx);
}

/* The imports registry is in memory; blobstore may now ask for esnap parents. */
static void lvs_attach_exports_loaded(void *cb_arg, int status);

static void
lvs_attach_imports_loaded(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("Failed to read the imports registry of '%s': %s. Not "
			    "loading: an esnap clone whose manifest is unknown would "
			    "either fail to open or, worse, be opened against nothing.\n",
			    lvs->name, spdk_strerror(-status));
		lvs_setup_fail(ctx, status);
		return;
	}

	/* And what this lvstore has published. Not needed by blobstore, so the
	 * ordering here is arbitrary -- but it has to be in memory before any RPC can
	 * delete an lvol, because it is what says which snapshots are spoken for. */
	rc = s3lvol_xfer_exports_load(lvs, lvs_attach_exports_loaded, ctx);
	if (rc != 0) {
		lvs_setup_fail(ctx, rc);
	}
}

static void
lvs_attach_exports_loaded(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;
	struct spdk_lvs_opts lvs_opts;

	if (status != 0) {
		SPDK_ERRLOG("Failed to read the imports registry of '%s': %s. Not "
			    "loading: an esnap clone whose manifest is unknown would "
			    "either fail to open or, worse, be opened against nothing.\n",
			lvs->name, spdk_strerror(-status));
		lvs_setup_fail(ctx, status);
		return;
	}

	/* Registered before spdk_lvs_load_ext() because all of its failure paths
	 * destroy the bs_dev, and that teardown is asynchronous. */
	s3_bs_dev_set_destroy_cb(lvs->bs_dev, lvs_setup_bs_dev_gone, ctx);

	/* The only member that matters on load: the name, the uuid and the cluster
	 * size all come from blobstore's own metadata, and lvs_load() reads nothing
	 * else out of these opts.
	 *
	 * esnap_bs_dev_create is *not* optional. Without it blobstore refuses to
	 * open any esnap clone it finds, which is the whole lvstore failing to load
	 * -- and that is the good outcome; the alternative would be clones that
	 * read as zeroes. */
	spdk_lvs_opts_init(&lvs_opts);
	lvs_opts.esnap_bs_dev_create = s3lvol_esnap_dev_create;

	spdk_lvs_load_ext(lvs->bs_dev, &lvs_opts, s3lvol_lvs_load_cb, ctx);
}

static void
lvs_attach_start_blobstore(struct lvs_setup_ctx *ctx)
{
	int rc;

	/* Fetch the manifests *first*. blobstore asks for the parent of each esnap
	 * clone synchronously while loading, and that can only be answered out of
	 * memory: waiting there for an S3 GET would deadlock the very thread that
	 * has to poll for its completion. */
	rc = s3lvol_xfer_imports_load(ctx->lvs, lvs_attach_imports_loaded, ctx);
	if (rc != 0) {
		lvs_setup_fail(ctx, rc);
	}
}

static void
lvs_attach_wal_replayed(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3_wal_stats st = {0};

	if (status != 0) {
		SPDK_ERRLOG("WAL replay failed on '%s': %d\n",
			    ctx->wal_bdev_name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	s3_wal_get_stats(ctx->lvs->wal, &st);
	SPDK_NOTICELOG("lvstore '%s': replayed %" PRIu64 " WAL entries, "
		       "dropped %" PRIu64 " unclosed batch(es)\n",
		       ctx->lvs->name, st.replayed_entries,
		       st.dropped_batches);

	lvs_attach_start_blobstore(ctx);
}

static void
lvs_attach_journal_replayed(void *cb_arg, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("Journal replay failed on '%s': %d\n",
			    ctx->wal_bdev_name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	/* From here on the bs_dev owns closing the WAL. It also has to happen
	 * before the replay below: replayed entries go into the overlay, which
	 * this call creates. */
	rc = s3_bs_dev_attach_wal(lvs->bs_dev, lvs->wal, NULL);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to attach the WAL to '%s': %d\n",
			    lvs->name, rc);
		lvs_setup_fail(ctx, rc);
		return;
	}

	s3_wal_replay(lvs->wal, (s3_wal_replay_cb)s3_bs_dev_wal_apply, lvs->bs_dev,
		      lvs_attach_wal_replayed, ctx);
}

static void lvs_attach_checkpoint_loaded(void *cb_arg, uint64_t lsn,
					 uint64_t gen, int status);

static void
lvs_attach_wal_opened(void *cb_arg, struct s3_wal *wal, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("Failed to open the WAL on '%s': %d\n",
			    ctx->wal_bdev_name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	lvs->wal = wal;

	rc = s3_bs_dev_create(&ctx->opts, NULL, NULL, lvs->client,
			      ctx->opts.capacity_bytes, &lvs->bs_dev);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to create bs_dev for '%s': %d\n", lvs->name, rc);
		lvs_setup_fail(ctx, rc);
		return;
	}

	rc = s3_bs_dev_attach_journal(lvs->bs_dev, lvs->journal,
					      lvs->local_dev);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to attach the journal to '%s': %d\n",
			    lvs->name, rc);
		lvs_setup_fail(ctx, rc);
		return;
	}

	/* Deliberately not checked; see the create path. Doing it here rather than
	 * after replay only means the cache is available to the very first read,
	 * which is blobstore's super block. */
	(void)s3_bs_dev_attach_cache(lvs->bs_dev);

	/* Rebuild the table from the snapshot before replaying anything.
	 *
	 * *Skipping this loses data as soon as any checkpoint has ever run.* Once
	 * checkpoint_lsn > 0 the journal no longer holds the records at or below
	 * it -- they were truncated -- so those mappings exist only in the
	 * snapshot. Replaying the journal alone would come up with a table missing
	 * everything older than the last checkpoint, and every S3 object behind
	 * those mappings would be unreachable. */
	s3_checkpoint_load(lvs->client, lvs->name,
			   s3_bs_dev_get_chunk_map(lvs->bs_dev),
			   lvs_attach_checkpoint_loaded, ctx);
}

static void
lvs_attach_checkpoint_loaded(void *cb_arg, uint64_t lsn, uint64_t gen,
			     int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;

	if (status != 0) {
		SPDK_ERRLOG("Failed to load the checkpoint for '%s': %d\n",
			    lvs->name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	/* Replay resumes from what the snapshot actually covers, not from what the
	 * super block claims.
	 *
	 * They agree in normal operation, because a checkpoint uploads the snapshot
	 * before updating the super block. A disagreement means a crash landed
	 * between those two steps, and then the *snapshot* is the authority on what
	 * has been restored -- resuming from the larger of the two would skip
	 * journal records the snapshot never captured. */
	if (ctx->from_lsn != lsn) {
		SPDK_NOTICELOG("lvstore '%s': the super block records checkpoint "
			       "LSN %" PRIu64 " but the snapshot in S3 covers %"
			       PRIu64 "; replaying from the snapshot's value\n",
			       lvs->name, ctx->from_lsn, lsn);
	}
	if (lsn > ctx->from_lsn) {
		/* The snapshot is *ahead* of the super block: the upload landed and
		 * the super block update did not. Harmless -- the journal still
		 * holds everything from the super block's LSN onwards, so replaying
		 * from the snapshot's higher LSN skips nothing. */
		SPDK_NOTICELOG("lvstore '%s': checkpoint gen=%" PRIu64 " is newer "
			       "than the super block; a crash interrupted the last "
			       "checkpoint after the upload\n", lvs->name, gen);
	}
	ctx->from_lsn = lsn;

	/* The journal first: it says which S3 objects exist, and the WAL replay
	 * then re-applies whatever had not reached them. */
	s3_journal_replay(lvs->journal, ctx->from_lsn,
			  (s3_journal_apply_cb)s3_bs_dev_journal_apply,
			  lvs->bs_dev, lvs_attach_journal_replayed, ctx);
}

/* Everything the attach chain does after it owns the lvstore. */
static void
lvs_attach_after_owner(struct lvs_setup_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	int rc;

	rc = s3_journal_open(lvs->local_dev, &lvs->journal);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to open the journal on '%s': %d\n",
			    ctx->wal_bdev_name, rc);
		lvs_setup_fail(ctx, rc);
		return;
	}

	s3_wal_open(lvs->local_dev, lvs_attach_wal_opened, ctx);
}

static void
lvs_attach_local_dev_opened(void *cb_arg, struct s3_local_dev *dev, int status)
{
	struct lvs_setup_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;
	const struct s3_super_block *sb;

	if (status != 0) {
		SPDK_ERRLOG("Failed to open the local device '%s': %d\n",
			    ctx->wal_bdev_name, status);
		lvs_setup_fail(ctx, status);
		return;
	}

	lvs->local_dev = dev;
	sb = s3_local_dev_get_super(dev);

	/* First identity check, and the only one that happens before any replay.
	 * The S3 key prefix is derived from the name, so a mismatch means this
	 * disk's log describes a different lvstore and replaying it would write
	 * foreign data into this one. */
	if (strcmp(sb->lvs_name, lvs->name) != 0) {
		SPDK_ERRLOG("local device '%s' belongs to lvstore '%s', not '%s' "
			    "-- refusing to attach\n",
			    ctx->wal_bdev_name, sb->lvs_name, lvs->name);
		lvs_setup_fail(ctx, -EINVAL);
		return;
	}
	if (sb->capacity_bytes == 0) {
		SPDK_ERRLOG("local device '%s' records no capacity; it was "
			    "formatted by an older build and cannot be attached\n",
			    ctx->wal_bdev_name);
		lvs_setup_fail(ctx, -EPROTO);
		return;
	}

	/* Geometry comes from the disk, never from the RPC. Handing blobstore a
	 * different capacity than it was created with either fails the load
	 * (-EILSEQ, when smaller) or quietly invites a grow (when larger), and a
	 * typo in an RPC parameter should not be able to do either. */
	ctx->opts.capacity_bytes = sb->capacity_bytes;
	ctx->opts.chunk_size     = sb->chunk_size;
	ctx->from_lsn            = sb->checkpoint_lsn;
	spdk_uuid_copy(&ctx->expect_uuid, &sb->lvs_uuid);

	/* Claim the lvstore now: after the local device has been read, because the
	 * marker records which lvstore uuid we think we are attaching, but *before*
	 * the WAL is attached and the flusher starts uploading. Anything later
	 * would mean a second owner had already written to S3 by the time it was
	 * told it was not the owner. */
	s3_owner_acquire(lvs->client, lvs->name, &sb->lvs_uuid, ctx->force,
			 lvs_setup_owner_acquired, ctx);
}

int
s3lvol_lvstore_attach(const struct s3_lvs_opts *opts, s3lvol_lvs_op_cb cb_fn,
		      void *cb_arg)
{
	struct lvs_setup_ctx *ctx;
	int rc;

	if (!opts || !opts->lvs_name) {
		return -EINVAL;
	}
	rc = lvs_name_ok(opts->lvs_name);
	if (rc != 0) {
		return rc;
	}
	/* A local device is not optional here. Without one there is no journal,
	 * so the chunk map cannot be rebuilt, and every object in the bucket is
	 * an orphan: the data is there but nothing maps an LBA to it. Attaching
	 * "without a WAL" would produce an lvstore that reads as zeroes. */
	if (!opts->wal_bdev_name) {
		SPDK_ERRLOG("attaching lvstore '%s' needs the wal_bdev it was "
			    "created with: the chunk map lives in that device's "
			    "journal and cannot be reconstructed from S3\n",
			    opts->lvs_name);
		return -EINVAL;
	}
	if (s3lvol_lvstore_find(opts->lvs_name)) {
		SPDK_ERRLOG("lvstore '%s' is already attached\n", opts->lvs_name);
		return -EEXIST;
	}

	rc = lvs_setup_begin(opts, cb_fn, cb_arg, &ctx);
	if (rc != 0) {
		return rc;
	}
	ctx->attaching = true;
	ctx->force = opts->force;

	s3_local_dev_open(ctx->wal_bdev_name, ctx->cache_bdev_name,
			  lvs_attach_local_dev_opened, ctx);
	return 0;
}

/* ==========================================================================
 * lvstore unload
 *
 * Two things have to happen in order, and neither is optional on the WAL path:
 *
 *   1. everything acknowledged so far reaches S3 (INV2), and
 *   2. the journal and the local device are only released once the bs_dev is
 *      completely done with them.
 *
 * The subtlety is that spdk_lvs_unload() writes blobstore's final metadata,
 * *those writes are acknowledged from the log*, and bs_dev->destroy() cannot
 * block. So destroy() finishes in the background and reports through its own
 * callback -- spdk_lvs_unload()'s completion is not the end of the story.
 *
 * The two completions can arrive in either order, so this rendezvouses on both.
 * ========================================================================== */

struct lvs_unload_ctx {
	struct s3lvol_lvstore *lvs;
	spdk_lvs_op_complete cb_fn;
	void *cb_arg;

	bool lvs_done;
	bool bs_dev_done;
	int status;

	/* Outstanding lvol bdev unregisters, plus one self reference so an inline
	 * completion cannot finish the round early. */
	uint32_t lvols_pending;

	/* Set while spdk_lvs_unload() is on the stack. It reports its -EBUSY
	 * failures *both* through the callback and through the return value, so
	 * without this the error would be handled twice -- which is exactly the
	 * double free that segfaulted the target on the first live run. */
	bool in_submit;
	bool cb_ran;
};

static void lvs_unload_maybe_finish(struct lvs_unload_ctx *ctx);

/* The claim in S3 is gone; finish the unload. */
static void
lvs_unload_owner_released(void *cb_arg, int status)
{
	struct lvs_unload_ctx *ctx = cb_arg;

	/* Already logged by s3_owner_release(), and deliberately not turned into
	 * an unload failure: a marker left behind only costs the next attach a
	 * force=true, whereas failing here would leave a half-unloaded lvstore. */
	(void)status;

	lvs_unload_maybe_finish(ctx);
}

static void
lvs_unload_maybe_finish(struct lvs_unload_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	spdk_lvs_op_complete cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;
	int status = ctx->status;

	if (!ctx->lvs_done || !ctx->bs_dev_done) {
		return;
	}

	/* Give the claim back only now. Everything that writes to S3 has stopped:
	 * the flusher was drained, blobstore has unloaded and the bs_dev is gone.
	 * Releasing earlier would advertise the lvstore as free while this process
	 * was still writing to it.
	 *
	 * Clearing owner_held first is what stops this from recursing. */
	if (lvs->owner_held) {
		lvs->owner_held = false;
		s3_owner_release(lvs->client, lvs->name,
				 lvs_unload_owner_released, ctx);
		return;
	}

	free(ctx);

	SPDK_NOTICELOG("lvstore '%s' unloaded\n", lvs->name);

	TAILQ_REMOVE(&g_lvstores, lvs, link);
	/* Frees the journal and closes the local device, which is safe now that
	 * the bs_dev is gone. */
	s3lvol_lvstore_free(lvs);

	if (cb_fn) {
		cb_fn(user_arg, status);
	}
}

static void
lvs_unload_bs_dev_gone(void *cb_arg, int status)
{
	struct lvs_unload_ctx *ctx = cb_arg;

	(void)status;
	ctx->lvs->bs_dev = NULL;
	ctx->bs_dev_done = true;
	lvs_unload_maybe_finish(ctx);
}

/* Nothing was torn down, so no destroy callback is coming. Drop it anyway: if
 * the bs_dev does get destroyed later it must not call into a freed context. */
static void
lvs_unload_report_error(struct lvs_unload_ctx *ctx, int status)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	spdk_lvs_op_complete cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;

	if (lvs->bs_dev) {
		s3_bs_dev_set_destroy_cb(lvs->bs_dev, NULL, NULL);
	}

	/* The lvstore stays in the registry: an unload that failed leaves it
	 * usable, and removing it would turn it into an unreachable object. */
	free(ctx);

	if (cb_fn) {
		cb_fn(user_arg, status);
	}
}

static void
s3lvol_lvs_unload_cb(void *cb_arg, int lvserrno)
{
	struct lvs_unload_ctx *ctx = cb_arg;
	struct s3lvol_lvstore *lvs = ctx->lvs;

	if (lvserrno != 0) {
		SPDK_ERRLOG("Failed to unload lvstore '%s': %s\n",
			    lvs->name, spdk_strerror(-lvserrno));

		/* spdk_lvs_unload() reports its -EBUSY failures through *both* the
		 * callback and the return value, so the submitting frame is still
		 * on the stack and owns the cleanup. Handling it here as well is
		 * what double-freed the context on the first live run. */
		if (ctx->in_submit) {
			ctx->cb_ran = true;
			ctx->status = lvserrno;
			return;
		}

		lvs_unload_report_error(ctx, lvserrno);
		return;
	}

	lvs->lvs = NULL;
	ctx->lvs_done = true;
	lvs_unload_maybe_finish(ctx);
}

static void
lvs_unload_do_unload(struct lvs_unload_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	int rc;

	if (ctx->status != 0) {
		/* An lvol failed to close, so the lvstore is still busy and
		 * unloading it would only fail with -EBUSY. */
		SPDK_ERRLOG("lvstore '%s': not every lvol could be closed (%s); "
			    "not unloading\n", lvs->name, spdk_strerror(-ctx->status));
		if (ctx->status == -EINVAL) {
			/* From spdk_lvol_close() with ref_count already 0. The
			 * "has no bdev" notices just above name the candidates. */
			SPDK_ERRLOG("lvstore '%s': -EINVAL here means an lvol was "
				    "already closed; see the 'has no bdev' notices "
				    "above for which\n", lvs->name);
		}
		lvs_unload_report_error(ctx, ctx->status);
		return;
	}

	ctx->in_submit = true;
	rc = spdk_lvs_unload(lvs->lvs, s3lvol_lvs_unload_cb, ctx);
	ctx->in_submit = false;

	if (rc == 0) {
		return;
	}

	SPDK_ERRLOG("spdk_lvs_unload failed for '%s': %d\n", lvs->name, rc);
	lvs_unload_report_error(ctx, ctx->cb_ran ? ctx->status : rc);
}

/* One lvol bdev is gone. Its destruct closed the lvol, which is what releases
 * the reference spdk_lvs_unload() refuses to unload over. */
static void
lvs_unload_lvol_unregistered(void *cb_arg, int bdeverrno)
{
	struct lvs_unload_ctx *ctx = cb_arg;

	if (bdeverrno != 0 && ctx->status == 0) {
		ctx->status = bdeverrno;
	}

	assert(ctx->lvols_pending > 0);
	if (--ctx->lvols_pending > 0) {
		return;
	}

	lvs_unload_do_unload(ctx);
}

/* Step 2: close every lvol.
 *
 * spdk_lvs_unload() rejects an lvstore with open lvols, and an lvol stays open
 * for as long as its bdev is registered. Upstream vbdev_lvol does the same
 * (_vbdev_lvs_remove). The first live run failed here: the script deleted the
 * nvmf namespace but the bdev was still registered, so unload returned -EBUSY. */
static void
lvs_unload_close_lvols(struct lvs_unload_ctx *ctx)
{
	struct s3lvol_lvstore *lvs = ctx->lvs;
	struct spdk_lvol *lvol, *tmp;

	/* Self reference, so an unregister that completes inline cannot run the
	 * next step before the loop has finished submitting. */
	ctx->lvols_pending = 1;

	TAILQ_FOREACH_SAFE(lvol, &lvs->lvs->lvols, link, tmp) {
		ctx->lvols_pending++;

		if (lvol->bdev != NULL) {
			spdk_bdev_unregister(lvol->bdev,
					     lvs_unload_lvol_unregistered, ctx);
		} else {
			/* No bdev but possibly still open (a failed registration,
			 * or a future internal lvol). Same callback shape.
			 *
			 * Logged because this is the branch that produces the -22
			 * below: spdk_lvol_close() answers -EINVAL when ref_count is
			 * already 0, i.e. when the lvol is closed as well as
			 * bdev-less. Without the name, that error identifies nothing
			 * and the only way to find the lvol is by elimination. */
			SPDK_NOTICELOG("lvstore '%s': lvol '%s' has no bdev; closing "
				       "it directly\n", lvs->name, lvol->name);
			spdk_lvol_close(lvol, lvs_unload_lvol_unregistered, ctx);
		}
	}

	lvs_unload_lvol_unregistered(ctx, 0);
}

/* Step 1: get as much as possible into S3 while blobstore is still up. What
 * unloading itself writes is handled by the bs_dev teardown. */
static void
lvs_unload_drained(void *cb_arg, int status)
{
	struct lvs_unload_ctx *ctx = cb_arg;

	if (status != 0) {
		/* Carry on regardless. Refusing to unload because S3 is unreachable
		 * would leave the process unable to shut down, and the data is
		 * durable in the log either way -- it just has to be replayed. */
		SPDK_WARNLOG("lvstore '%s': not everything reached S3 before "
			     "unload (%d); the rest stays in the WAL\n",
			     ctx->lvs->name, status);
	}

	lvs_unload_close_lvols(ctx);
}

void
s3lvol_lvstore_unload(struct s3lvol_lvstore *lvs,
		      spdk_lvs_op_complete cb_fn, void *cb_arg)
{
	struct lvs_unload_ctx *ctx;

	if (!lvs || !lvs->lvs) {
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
	ctx->lvs = lvs;
	ctx->cb_fn = cb_fn;
	ctx->cb_arg = cb_arg;

	/* Registered before anything else: spdk_lvs_unload() destroys the bs_dev
	 * internally, and this is the only signal for when that finished. */
	s3_bs_dev_set_destroy_cb(lvs->bs_dev, lvs_unload_bs_dev_gone, ctx);

	s3_bs_dev_drain(lvs->bs_dev, lvs_unload_drained, ctx);
}

/* Push everything acknowledged so far into S3 without unloading.
 *
 * Exists for testing: after this returns the overlay is empty, so a subsequent
 * read has to come from S3 rather than from RAM. That is the difference between
 * "the data is in the process" and "the data is in the object store". */
void
s3lvol_lvstore_flush(struct s3lvol_lvstore *lvs, spdk_lvs_op_complete cb_fn,
		     void *cb_arg)
{
	if (!lvs || !lvs->bs_dev) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	s3_bs_dev_drain(lvs->bs_dev, (s3_bs_dev_cb)cb_fn, cb_arg);
}

void
s3lvol_lvstore_checkpoint(struct s3lvol_lvstore *lvs, spdk_lvs_op_complete cb_fn,
			  void *cb_arg)
{
	if (!lvs || !lvs->bs_dev) {
		if (cb_fn) {
			cb_fn(cb_arg, -EINVAL);
		}
		return;
	}

	s3_bs_dev_checkpoint(lvs->bs_dev, (s3_bs_dev_cb)cb_fn, cb_arg);
}

void
s3lvol_lvstore_get_stats(struct s3lvol_lvstore *lvs, struct s3_bs_dev_stats *out)
{
	if (!lvs || !lvs->bs_dev || !out) {
		return;
	}
	s3_bs_dev_get_stats(lvs->bs_dev, out);
}

/* ==========================================================================
 * lvol creation
 * ========================================================================== */

struct lvol_create_ctx {
	struct s3lvol_lvstore   *lvs;
	s3lvol_lvol_op_cb        cb_fn;
	void                    *cb_arg;
};

static void
s3lvol_lvol_create_cb(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct lvol_create_ctx *ctx = cb_arg;
	s3lvol_lvol_op_cb cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;
	const char *lvs_name = ctx->lvs->name;
	int rc;

	free(ctx);

	if (lvolerrno != 0) {
		SPDK_ERRLOG("Failed to create lvol: %s\n", spdk_strerror(-lvolerrno));
		if (cb_fn) {
			cb_fn(user_arg, NULL, lvolerrno);
		}
		return;
	}

	/* Register as a bdev -- this step is the whole point of the layer; nvmf
	 * only recognises names in the global bdev table. */
	rc = vbdev_s3lvol_bdev_register(lvol, lvs_name);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to register bdev for lvol '%s': %d\n",
			    lvol->name, rc);
		/* Destroy the lvol on registration failure, or an inaccessible
		 * lvol would be left behind. */
		spdk_lvol_destroy(lvol, (spdk_lvol_op_complete)NULL, NULL);
		if (cb_fn) {
			cb_fn(user_arg, NULL, rc);
		}
		return;
	}

	if (cb_fn) {
		cb_fn(user_arg, lvol, 0);
	}
}

int
s3lvol_lvol_create(struct s3lvol_lvstore *lvs, const char *name,
		   uint64_t size_bytes, bool thin_provision,
		   s3lvol_lvol_op_cb cb_fn, void *cb_arg)
{
	struct lvol_create_ctx *ctx;
	int rc;

	if (!lvs || !lvs->lvs || !name || size_bytes == 0) {
		return -EINVAL;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->lvs    = lvs;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;

	/* clear_method is UNMAP: on an S3 backend, "clear to zero" means
	 * unmap the chunk -- an unallocated region reads as zero, with no need
	 * to write zero bytes. */
	rc = spdk_lvol_create(lvs->lvs, name, size_bytes, thin_provision,
			      LVOL_CLEAR_WITH_UNMAP,
			      s3lvol_lvol_create_cb, ctx);
	if (rc != 0) {
		free(ctx);
		return rc;
	}

	return 0;
}

/* ==========================================================================
 * Snapshots and clones
 *
 * On an S3 backend neither of these **moves data**: a snapshot freezes the
 * origin blob's cluster list into a read-only blob and the origin becomes
 * CoW; a clone's new blob points straight at the parent's clusters. The
 * existing chunk objects stay referenced, and only new writes produce new
 * objects.
 *
 * === Why CoW cannot rewrite a shared chunk ===
 *
 * This was verified in the blobstore before touching it, because guessing
 * wrong silently corrupts snapshots:
 *
 *   blob_can_copy() requires `blob->bs->dev->copy != NULL` **and**
 *   `translate_lba()` to return true. We satisfy neither (copy left NULL,
 *   translate_lba always returns false; the reasons are in the
 *   s3_bs_dev_translate_lba comment), so the "move data inside the device"
 *   fast path can never be entered.
 *
 * The remaining regular path allocates a cluster_sz buffer, reads the whole
 * cluster from the back_bs_dev, then writes to
 *   `bs_cluster_to_lba(bs, ctx->new_cluster)` --
 *   **the write target is a freshly allocated cluster**, landing on a new LBA,
 *   a new chunk, a new uuid, a new S3 object. Rewriting the parent's chunk in
 *   place is impossible on this path.
 *
 * The cost is amplification: a write landing on an unallocated cluster first
 * reads a whole cluster. At 1 MiB chunks and 4 MiB clusters that is 4 GETs
 * plus 4 PUTs. Correct but not cheap; worth benchmarking on a clone before
 * discussing performance.
 *
 * === I/O on the origin volume is frozen during the snapshot ===
 *
 * `spdk_bs_create_snapshot()` freezes the origin blob's I/O internally. If an
 * S3 request is stuck and never returns at that moment, the snapshot stalls
 * with it. This is not solvable here, but it should be the first thing
 * considered when diagnosing.
 * ========================================================================== */

/* Completion callback for snapshot / clone.
 *
 * The only difference from create is what happens when registration fails --
 * and that difference is mandatory. A failed create deletes the lvol; that
 * must not happen here: once the snapshot exists, **the origin is already its
 * clone**, and the blobstore refuses to delete a snapshot that still has a
 * clone. Copying create's behaviour would only produce a failing cleanup plus
 * a misleading error log.
 *
 * So the snapshot is kept and the registration failure is reported honestly.
 * The data is intact; only a bdev is missing, and re-attaching the lvstore
 * registers it. */
static void
s3lvol_lvol_derive_cb(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct lvol_create_ctx *ctx = cb_arg;
	s3lvol_lvol_op_cb cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;
	const char *lvs_name = ctx->lvs->name;
	int rc;

	free(ctx);

	if (lvolerrno != 0) {
		SPDK_ERRLOG("Failed to create snapshot/clone: %s\n",
			    spdk_strerror(-lvolerrno));
		if (cb_fn) {
			cb_fn(user_arg, NULL, lvolerrno);
		}
		return;
	}

	rc = vbdev_s3lvol_bdev_register(lvol, lvs_name);
	if (rc != 0) {
		SPDK_ERRLOG("lvol '%s' was created but its bdev could not be "
			    "registered (%d). The data is intact -- re-attach the "
			    "lvstore to expose it.\n", lvol->name, rc);
		if (cb_fn) {
			cb_fn(user_arg, NULL, rc);
		}
		return;
	}

	if (cb_fn) {
		cb_fn(user_arg, lvol, 0);
	}
}

/* The lvol must belong to lvs: the caller looked up both separately, and
 * without this check a cross-lvstore combination would travel all the way to
 * the blobstore before exploding. */
static int
derive_check(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol, const char *name)
{
	if (!lvs || !lvs->lvs || !lvol || !name || name[0] == '\0') {
		return -EINVAL;
	}
	if (lvol->lvol_store != lvs->lvs) {
		SPDK_ERRLOG("lvol '%s' does not belong to lvstore '%s'\n",
			    lvol->name, lvs->name);
		return -EINVAL;
	}

	/* A decouple in flight is the case this is really about. Snapshotting an lvol
	 * halfway through one hands the esnap parent to the snapshot, and the decouple
	 * would then go on materialising and clearing the parent of a blob that no
	 * longer has it -- while the snapshot keeps reading an export everybody has
	 * stopped accounting for. blobstore's own locked_operation_in_progress does
	 * not cover this: a decouple deliberately does not hold it, so that the volume
	 * stays usable while the data is copied. */
	if (lvol->action_in_progress) {
		SPDK_ERRLOG("another operation is in progress on lvol '%s'\n",
			    lvol->name);
		return -EBUSY;
	}

	/* A queued decouple deliberately does not hold action_in_progress -- a
	 * queued volume must remain deletable -- but snapshotting it is just as
	 * destructive: the snapshot takes the external snapshot identity with it,
	 * and the queued decouple then fails its detach with "blob is not a clone
	 * of an external snapshot" after materialising the data. */
	if (s3lvol_lvol_decouple_pending(lvol)) {
		SPDK_ERRLOG("lvol '%s' is queued to be decoupled; a snapshot or "
			    "clone would take its external snapshot while the "
			    "decouple still reads through it\n", lvol->name);
		return -EBUSY;
	}
	return 0;
}

static struct lvol_create_ctx *
derive_ctx_alloc(struct s3lvol_lvstore *lvs, s3lvol_lvol_op_cb cb_fn, void *cb_arg)
{
	struct lvol_create_ctx *ctx = calloc(1, sizeof(*ctx));

	if (ctx) {
		ctx->lvs    = lvs;
		ctx->cb_fn  = cb_fn;
		ctx->cb_arg = cb_arg;
	}
	return ctx;
}

int
s3lvol_lvol_create_snapshot(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
			    const char *snapshot_name,
			    s3lvol_lvol_op_cb cb_fn, void *cb_arg)
{
	struct lvol_create_ctx *ctx;
	int rc;

	rc = derive_check(lvs, lvol, snapshot_name);
	if (rc != 0) {
		return rc;
	}

	/* Snapshotting a read-only blob is pointless: its content will never
	 * change again, and a clone is how to get another name. The blobstore
	 * would refuse too, but refusing here gives a reason one can
	 * articulate. */
	if (lvol->blob && spdk_blob_is_read_only(lvol->blob)) {
		SPDK_ERRLOG("lvol '%s' is already read-only (a snapshot?); "
			    "clone it instead\n", lvol->name);
		return -EPERM;
	}

	ctx = derive_ctx_alloc(lvs, cb_fn, cb_arg);
	if (!ctx) {
		return -ENOMEM;
	}

	spdk_lvol_create_snapshot(lvol, snapshot_name, s3lvol_lvol_derive_cb, ctx);
	return 0;
}

int
s3lvol_lvol_create_clone(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
			 const char *clone_name,
			 s3lvol_lvol_op_cb cb_fn, void *cb_arg)
{
	struct lvol_create_ctx *ctx;
	int rc;

	rc = derive_check(lvs, lvol, clone_name);
	if (rc != 0) {
		return rc;
	}

	/* Only snapshots can be cloned. The blobstore's
	 * spdk_bs_create_clone() also requires the parent to be read-only --
	 * otherwise clone and parent are simultaneously writable, anyone can
	 * modify a shared cluster, and neither side's data is defined any more.
	 * Blocking here is what lets the error message say why. */
	if (!lvol->blob || !spdk_blob_is_read_only(lvol->blob)) {
		SPDK_ERRLOG("lvol '%s' is not a snapshot; only a read-only lvol "
			    "can be cloned\n", lvol->name);
		return -EINVAL;
	}

	ctx = derive_ctx_alloc(lvs, cb_fn, cb_arg);
	if (!ctx) {
		return -ENOMEM;
	}

	spdk_lvol_create_clone(lvol, clone_name, s3lvol_lvol_derive_cb, ctx);
	return 0;
}

/* ==========================================================================
 * lvol deletion
 * ========================================================================== */

struct lvol_destroy_ctx {
	struct spdk_lvol        *lvol;

	/* Where to look afterwards, and for what. All three are read *before* the
	 * blob goes away: once the destroy succeeds there is no lvol left to ask
	 * its name or what it used to read through to, and the registry entry that
	 * has to be dropped is keyed by exactly that. */
	struct s3lvol_lvstore   *owner;
	char                     name[SPDK_LVOL_NAME_MAX];
	char                     esnap_uuid[SPDK_UUID_STRING_LEN];

	/* Cluster counts read before the destroy: the blob must still be open
	 * to read them, and is gone by the time the success callback runs, so
	 * the reclaimed counts can only be reported from there. */
	uint64_t                 allocated_clusters;
	uint64_t                 total_clusters;
	bool                     have_cluster_counts;

	spdk_lvol_op_complete    cb_fn;
	void                    *cb_arg;
};

/* Deleting the last volume that read an export is what ends this node's
 * dependency on it. The imports registry has to hear about that here, while the
 * lvstore is still loaded -- see s3lvol_imports_recheck(). */
static void
s3lvol_lvol_destroyed(void *cb_arg, int lvolerrno)
{
	struct lvol_destroy_ctx *ctx = cb_arg;

	if (lvolerrno == 0) {
		/* The one line a search can find for a successful delete. Nothing else
		 * prints it: the unregister is silent on success, the object deletes
		 * that follow are fire-and-forget and log only their failures, and the
		 * RPC answer travels to the caller, not the log. So "did the delete
		 * actually go through" was previously answerable only by asking again
		 * or counting objects; a production post-mortem needs better. The
		 * reclaimed cluster counts are printed here, not before the delete,
		 * so the claim only ever accompanies an actual success. */
		if (ctx->have_cluster_counts) {
			SPDK_NOTICELOG("Deleted lvol '%s/%s', reclaimed %" PRIu64
				       " of %" PRIu64 " clusters\n",
				       ctx->owner ? s3lvol_lvstore_get_name(ctx->owner) : "(null)",
				       ctx->name,
				       ctx->allocated_clusters, ctx->total_clusters);
		} else {
			SPDK_NOTICELOG("Deleted lvol '%s/%s'\n",
				       ctx->owner ? s3lvol_lvstore_get_name(ctx->owner) : "(null)",
				       ctx->name);
		}

		if (ctx->esnap_uuid[0] != '\0') {
			s3lvol_imports_recheck(ctx->owner, ctx->esnap_uuid);
		}
	} else {
		/* The refusal paths in s3lvol_lvol_destroy() log before returning, but
		 * an asynchronous failure -- the unregister, or spdk_lvol_destroy --
		 * lands here instead and would otherwise be silent. */
		SPDK_ERRLOG("Failed to delete lvol '%s/%s': %s\n",
			    ctx->owner ? s3lvol_lvstore_get_name(ctx->owner) : "(null)",
			    ctx->name, spdk_strerror(-lvolerrno));
	}

	if (ctx->cb_fn) {
		ctx->cb_fn(ctx->cb_arg, lvolerrno);
	}
	free(ctx);
}

static void
s3lvol_lvol_bdev_unregistered(void *cb_arg, int bdeverrno)
{
	struct lvol_destroy_ctx *ctx = cb_arg;

	if (bdeverrno != 0) {
		SPDK_ERRLOG("Failed to unregister bdev: %d\n", bdeverrno);
		if (ctx->cb_fn) {
			ctx->cb_fn(ctx->cb_arg, bdeverrno);
		}
		free(ctx);
		return;
	}

	/* The bdev is unregistered; the lvol can be deleted now. */
	spdk_lvol_destroy(ctx->lvol, s3lvol_lvol_destroyed, ctx);
}

int
s3lvol_lvol_destroy(struct spdk_lvol *lvol,
		    spdk_lvol_op_complete cb_fn, void *cb_arg)
{
	struct s3lvol_lvstore *owner;
	struct s3lvol_export *pin;

	/* A snapshot another machine is reading through must not go away. Deleting it
	 * would release its clusters, and the objects behind them would then be
	 * deleted by an overwrite or by GC -- leaving an importer reading holes that
	 * are not holes, and unable to open its clone at all after a restart.
	 *
	 * Refused rather than materialised, and that is now a decision rather than a
	 * gap (2026-08-05). Materialising -- server-side copying the objects into the
	 * export's own prefix and rewriting the manifest as dense -- founders on the
	 * rewrite: an importer caches the manifest verbatim in its imports registry
	 * and reloads it from there on attach, never re-fetching, so it would go on
	 * reading data/<uuid> keys that are about to be collected. Teaching it to
	 * refresh means giving up the immutable manifest that s3_export_bs_dev.c's
	 * lock-free reads are built on.
	 *
	 * The cheap answer is for GC to treat a ref manifest's references as live,
	 * which needs no copying and no importer changes -- see the header of
	 * lib/s3bsdev/s3_gc.c. Until that exists, the honest answer is that the export
	 * has to be released or left to expire.
	 *
	 * Note that expiry reaches the same hazard by itself: s3lvol_export_pinning()
	 * reports an expired export as not pinning, so this path opens up on its own
	 * once the TTL passes. It has not bitten anyone only because GC does not exist
	 * yet and nothing collects the orphans. */
	owner = lvol ? s3lvol_lvstore_find_by_lvs(lvol->lvol_store) : NULL;

	/* An export that has not published its manifest yet is not in the registry,
	 * so the pin check below cannot see it -- and it is holding a bare
	 * spdk_lvol pointer it dereferences after the drain completes.
	 *
	 * That window existed while rcow_export_snapshot answered only on
	 * completion, but the caller was blocked on the reply and had no chance to
	 * act on it. Now that the uuid comes back immediately, the caller is
	 * explicitly free to do other things while the export runs, so the window
	 * has to be closed here rather than merely reported by
	 * rcow_get_snapshot_status. */
	if (owner && s3lvol_export_inflight_pinning(owner, lvol->name)) {
		SPDK_ERRLOG("lvol '%s' is being exported right now; wait for "
			    "rcow_get_snapshot_status to stop reporting INPROGRESS "
			    "before deleting it\n", lvol->name);
		return -EBUSY;
	}

	pin = owner ? s3lvol_export_pinning(owner, lvol->name) : NULL;
	if (pin) {
		struct s3lvol_export_entry info;

		s3lvol_export_get(pin, &info);
		SPDK_ERRLOG("lvol '%s' is the snapshot behind export %s, which another "
			    "node may be reading through. Release that export, or wait "
			    "for it to expire, before deleting this.\n",
			    lvol->name, info.export_uuid);
		return -EBUSY;
	}

	struct lvol_destroy_ctx *ctx;

	if (!lvol) {
		return -EINVAL;
	}

	/* A snapshot with *more than one* clone cannot be deleted, and that has to be
	 * established here rather than left to blobstore.
	 *
	 * The order further down is: unregister the bdev, and destroy the lvol from
	 * the unregister's callback. blobstore's own refusal therefore arrives after
	 * the bdev is already gone and the lvol already closed, with no way back --
	 * the delete reports failure, and what it leaves behind is a snapshot that
	 * cannot be activated any more, in an lvstore that can no longer be unloaded
	 * at all, because spdk_lvol_close() on an already-closed lvol answers -EINVAL
	 * and lvs_unload_close_lvols() treats that as "not every lvol could be
	 * closed".
	 *
	 * A failed delete has to leave the volume exactly as it was. Checking first
	 * is what does that; rolling the unregister back afterwards would mean
	 * re-registering a bdev whose consumers have already been told it is gone.
	 * This mirrors the export pin check above, which refuses before touching
	 * anything for the same reason.
	 *
	 * The threshold is > 1, not > 0, and that distinction was originally wrong
	 * here. bs_is_blob_deletable() (blobstore.c:8714) refuses unconditionally
	 * only above one clone; at exactly one it sets *update_clone and *merges* the
	 * snapshot into that clone, and spdk_lvol_destroy() cooperates by looking the
	 * clone's lvol up beforehand so it can be fixed up afterwards. Refusing at
	 * one clone therefore rejected something the layers below implement, and
	 * since every snapshot of a live volume has exactly one clone, it made
	 * snapshots undeletable until the volume itself was deleted -- turning a
	 * chain of N snapshots into something that could only be dismantled from the
	 * leaf end. That was a regression, not a safety property.
	 *
	 * Everything else bs_is_blob_deletable() can refuse depends on open_ref,
	 * which is not knowable from here and which the normal path handles by
	 * closing the lvol first. Only the unconditional case is worth pre-empting.
	 *
	 * Querying with ids == NULL asks for the count alone: blobstore then always
	 * takes its -ENOMEM branch after filling *count in, and answers 0 with
	 * count == 0 when the blob is not a snapshot at all. So no separate
	 * spdk_blob_is_snapshot() test is needed. lvol->blob_id rather than
	 * spdk_blob_get_id(lvol->blob), because blob_id stays valid even once the
	 * blob is closed -- which is what upstream does at lvol.c:1584. */
	if (lvol->blob) {
		struct spdk_blob_store *bs = lvol->lvol_store->blobstore;
		size_t clone_count = 0;
		int rc;

		rc = spdk_blob_get_clones(bs, lvol->blob_id, NULL, &clone_count);
		if (rc != 0 && rc != -ENOMEM) {
			/* Not reachable today -- the function returns only those two
			 * -- but treating an unknown answer as "go ahead and format"
			 * is the wrong default if that ever changes. */
			SPDK_ERRLOG("cannot tell how many clones snapshot '%s' has "
				    "(%s); refusing to delete it\n", lvol->name,
				    spdk_strerror(-rc));
			return -EBUSY;
		}
		if (clone_count > 1) {
			SPDK_ERRLOG("lvol '%s' is a snapshot with %zu clones; "
				    "blobstore can only merge a snapshot into a "
				    "single clone, so delete the others first\n",
				    lvol->name, clone_count);
			return -EBUSY;
		}
	}

	/* Nothing upstream stops a delete from running under a decouple:
	 * spdk_lvol_destroy() does not look at action_in_progress, unlike
	 * spdk_lvol_open() and the lvstore unload paths. Left unchecked, the copy loop
	 * would carry on against a blob that has been freed. */
	if (lvol->action_in_progress) {
		SPDK_ERRLOG("another operation is in progress on lvol '%s'; it cannot "
			    "be deleted yet\n", lvol->name);
		return -EBUSY;
	}

	/* Past the check above, so this volume is not materialising anything -- but it
	 * may be *waiting* to. A queued decouple deliberately does not hold
	 * action_in_progress, since it can sit behind a run that takes minutes, so the
	 * delete is allowed and the queue entry has to go instead. Done here rather
	 * than in the completion path because the queue holds this lvol pointer, and
	 * by then it is freed. */
	s3lvol_decouple_dequeue_lvol(lvol);

	/* Section 12.0.4 constraint 1 requires -EBUSY while the bdev is still
	 * claimed, and never a forced removal.
	 *
	 * But SPDK has no public interface to query claim state (enum
	 * spdk_bdev_claim_type lives only in bdev_module.h, with no getter), and
	 * the upstream vbdev_lvol simply calls spdk_bdev_unregister
	 * (vbdev_lvol.c:686).
	 *
	 * Today we rely on the bdev layer's own protection:
	 * spdk_bdev_unregister() waits for every open descriptor to close before
	 * actually destructing, so an unreleased claim never silently loses
	 * data. Giving an explicit -EBUSY plus a "please
	 * nvmf_subsystem_remove_ns" hint at the RPC layer would require keeping
	 * our own record of namespace attachments -- deferred to the RPC-layer
	 * implementation.
	 */
	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->lvol   = lvol;
	ctx->owner  = owner;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;

	/* Copied now: the destroy frees the lvol and its name with it. */
	snprintf(ctx->name, sizeof(ctx->name), "%s", lvol->name);

	/* Recorded now, for the same reason the owner is: after the destroy the blob
	 * is gone and the esnap id with it. An id that is not a NUL-terminated uuid
	 * string is not one of ours, so it is left empty rather than guessed at. */
	if (lvol->blob && spdk_blob_is_esnap_clone(lvol->blob)) {
		const void *esnap_id = NULL;
		size_t id_len = 0;

		if (spdk_blob_get_esnap_id(lvol->blob, &esnap_id, &id_len) == 0 &&
		    id_len < sizeof(ctx->esnap_uuid)) {
			memcpy(ctx->esnap_uuid, esnap_id, id_len);
			ctx->esnap_uuid[id_len] = '\0';
		}
	}

	/* Capture the cluster counts before the destroy: the blob must still be
	 * open to read them, and is gone by the time s3lvol_lvol_destroyed()
	 * runs. Printing them from the success callback (rather than here) keeps
	 * "reclaimed" honest when an asynchronous destroy fails. */
	if (lvol->blob != NULL) {
		ctx->allocated_clusters =
			spdk_blob_get_num_allocated_clusters(lvol->blob);
		ctx->total_clusters =
			spdk_blob_get_num_clusters(lvol->blob);
		ctx->have_cluster_counts = true;
	}

	if (lvol->bdev) {
		vbdev_s3lvol_bdev_unregister(lvol, s3lvol_lvol_bdev_unregistered, ctx);
		return 0;
	}

	spdk_lvol_destroy(lvol, s3lvol_lvol_destroyed, ctx);
	return 0;
}

/* ==========================================================================
 * lvol lookup
 * ========================================================================== */

struct spdk_lvol *
s3lvol_lvol_find(struct s3lvol_lvstore *lvs, const char *name)
{
	struct spdk_lvol *lvol;

	if (!lvs || !lvs->lvs || !name) {
		return NULL;
	}

	/* The blobstore's lvols list holds **every opened** lvol, snapshots and
	 * clones included (they differ from ordinary volumes only in the blob's
	 * read-only bit and parent/child relationships). So this single lookup
	 * serves resize / delete / snapshot / clone alike; no separate tables
	 * needed. */
	TAILQ_FOREACH(lvol, &lvs->lvs->lvols, link) {
		if (strcmp(lvol->name, name) == 0) {
			return lvol;
		}
	}

	return NULL;
}

struct spdk_lvol *
s3lvol_lvol_find_any(const char *name)
{
	struct s3lvol_lvstore *lvs;
	struct spdk_lvol *found = NULL;

	if (!name) {
		return NULL;
	}

	TAILQ_FOREACH(lvs, &g_lvstores, link) {
		struct spdk_lvol *lvol = s3lvol_lvol_find(lvs, name);

		if (lvol) {
			if (found) {
				return NULL; /* ambiguous */
			}
			found = lvol;
		}
	}
	return found;
}

struct s3lvol_lvstore *
s3lvol_lvstore_of_lvol(struct spdk_lvol *lvol)
{
	struct s3lvol_lvstore *lvs;

	if (!lvol || !lvol->lvol_store) {
		return NULL;
	}

	TAILQ_FOREACH(lvs, &g_lvstores, link) {
		if (lvs->lvs == lvol->lvol_store) {
			return lvs;
		}
	}
	return NULL;
}

/* ==========================================================================
 * lvol resize
 *
 * **Only growth is supported.** Shrinking is doable at the blobstore layer,
 * but is actively refused here:
 *
 *   1. It irreversibly loses data, and an RPC is a too-easy entry point for a
 *      mistyped argument;
 *   2. whether the chunks past the new boundary should have their S3 objects
 *      deleted depends on whether the blobstore passes unmap down to the
 *      bs_dev when releasing clusters (lvols are created with
 *      LVOL_CLEAR_WITH_UNMAP here, which suggests it does), but that chain is
 *      **unverified**. Guessing wrong orphans the objects, and reclaiming
 *      orphans relies on s3_gc.c, which does not exist yet -- i.e. paying
 *      storage forever.
 *
 * Reconsider lifting the guard once GC has landed and a real machine confirms
 * unmap propagates.
 * ========================================================================== */

struct lvol_resize_ctx {
	struct spdk_lvol        *lvol;
	spdk_lvol_op_complete    cb_fn;
	void                    *cb_arg;
};

static void
s3lvol_lvol_resize_cb(void *cb_arg, int lvolerrno)
{
	struct lvol_resize_ctx *ctx = cb_arg;
	struct spdk_lvol *lvol = ctx->lvol;
	spdk_lvol_op_complete cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;
	uint64_t total_size;
	int rc = lvolerrno;

	free(ctx);

	if (rc != 0) {
		SPDK_ERRLOG("Failed to resize lvol '%s': %s\n",
			    lvol->name, spdk_strerror(-rc));
		goto out;
	}

	/* The blob is the source of truth, so the new bdev size is read back from
	 * it rather than taken from what the caller asked for: spdk_lvol_resize()
	 * rounds up to a whole number of clusters. */
	total_size = spdk_blob_get_num_clusters(lvol->blob) *
		     spdk_bs_get_cluster_size(lvol->lvol_store->blobstore);

	rc = spdk_bdev_notify_blockcnt_change(lvol->bdev,
					      total_size / lvol->bdev->blocklen);
	if (rc != 0) {
		/* The blob grew but the bdev did not, so the two now disagree and
		 * the extra space is unreachable. Reported rather than papered
		 * over; a re-attach rebuilds blockcnt from the blob and fixes it. */
		SPDK_ERRLOG("lvol '%s' was resized to %" PRIu64 " bytes but the bdev "
			    "could not be notified (%d); re-attach the lvstore to "
			    "resynchronise\n", lvol->name, total_size, rc);
		goto out;
	}

	SPDK_NOTICELOG("Resized lvol '%s' to %" PRIu64 " MiB (%" PRIu64 " blocks)\n",
		       lvol->name, total_size / (1024 * 1024),
		       lvol->bdev->blockcnt);

out:
	if (cb_fn) {
		cb_fn(user_arg, rc);
	}
}

int
s3lvol_lvol_resize(struct spdk_lvol *lvol, uint64_t size_bytes,
		   spdk_lvol_op_complete cb_fn, void *cb_arg)
{
	struct lvol_resize_ctx *ctx;
	uint64_t cluster_sz, current_size;

	if (!lvol || !lvol->blob || size_bytes == 0) {
		return -EINVAL;
	}
	/* Upstream asserts on this. Here it is a real possibility rather than a
	 * programming error: a snapshot whose bdev registration failed is still
	 * in the lvstore's list and can be named in an RPC. */
	if (!lvol->bdev) {
		SPDK_ERRLOG("lvol '%s' has no bdev; nothing to resize\n", lvol->name);
		return -EINVAL;
	}
	if (spdk_blob_is_read_only(lvol->blob)) {
		/* Snapshots are read-only by construction. Growing one would make
		 * the clones that share its clusters disagree about where their
		 * backing device ends. */
		SPDK_ERRLOG("lvol '%s' is read-only (a snapshot?) and cannot be "
			    "resized\n", lvol->name);
		return -EPERM;
	}

	cluster_sz = spdk_bs_get_cluster_size(lvol->lvol_store->blobstore);
	current_size = spdk_blob_get_num_clusters(lvol->blob) * cluster_sz;

	/* Compared after rounding up, so asking for one byte more than the current
	 * size is a no-op rather than an error -- the request is already satisfied,
	 * which is what the caller wanted. */
	if (spdk_divide_round_up(size_bytes, cluster_sz) * cluster_sz < current_size) {
		SPDK_ERRLOG("lvol '%s' is %" PRIu64 " MiB; shrinking it to %" PRIu64
			    " MiB is not supported\n", lvol->name,
			    current_size / (1024 * 1024), size_bytes / (1024 * 1024));
		return -ENOTSUP;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->lvol   = lvol;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;

	spdk_lvol_resize(lvol, size_bytes, s3lvol_lvol_resize_cb, ctx);
	return 0;
}
