/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   What this node has published, and what that obliges it to keep
 *
 *   === Why an export has to be remembered at all ===
 *
 *   A zero-copy export names the live objects of a snapshot. Those objects stay
 * readable for exactly as long as the snapshot exists, because it is the
 *   snapshot's existence that keeps them in the chunk map and therefore out of
 *   reach of GC. So the export places an obligation on this node: do not delete
 *   that snapshot out from under the importer.
 *
 *   An obligation nobody records is one that survives no restart. Hence this
 *   registry, one object per lvstore, read at attach and rewritten whenever an
 * export appears or goes away. Without it, a restart would forget which
 *   snapshots are spoken for, and the first delete of one would break a volume
 *   running on another machine -- with nothing on this side indicating why.
 *
 *   === How that obligation ends ===
 *
 *   An export remains importable for as long as its snapshot exists. The owner
 *   ends that lifetime explicitly by deleting the snapshot; the delete path
 *   first releases every export that has no live reader. Importers renew a lease
 *   while an esnap clone still reads through the export, so a delete requested
 *   during restore is deferred until that dependency clears.
 */

#include "spdk/stdinc.h"
#include "spdk/json.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/util.h"

#include "spdk_internal/lvolstore.h"

#include "s3lvol/s3_export.h"

#include "vbdev_s3lvol.h"
#include "vbdev_s3lvol_json.h"

/* Same reasoning as the import cap: the registry is read into a fixed decoder
 * table, and a node with more live exports than this has a different problem.
 *
 * Raised from 64 because that was not true in practice. An export leaves the
 * registry when it is explicitly released or when its snapshot is deleted. */
#define S3LVOL_MAX_EXPORTS 256

/* One in-flight lease check. Defined below, next to the machinery that uses it;
 * named here because an export points at the check it is waiting for. */
struct export_lease_get_ctx;

struct s3lvol_export {
	struct s3lvol_lvstore     *lvs;

	char         uuid_str[SPDK_UUID_STRING_LEN];

	/* The snapshot this pins. Stored by name because that is what the delete
	 * path has in hand. snapshot_uuid is the identity: blobstore reuses blob
	 * ids after a delete, so name+blob_id can match a replacement volume.
	 * Empty on registries written before the field existed; those fall back
	 * to blob_id, then to the name. */
	char   snapshot[SPDK_LVOL_NAME_MAX];
	char         snapshot_uuid[SPDK_UUID_STRING_LEN];
	uint64_t    blob_id;

	enum s3_export_layout      layout;
	/* Retained when loading old registries and reporting the compatible RPC
	 * shape. It no longer controls export lifetime. */
	uint64_t        expires_at;
	uint32_t                generation;

	/* Liveness lease cache. The importer renews
	 * <this lvstore>/meta/exports/<uuid>.lease in the source bucket; this
	 * node reads it back so the delete path can tell "an importer is still
	 * reading" from "nobody currently holds the export". Three fields, one poller:
	 *
	 *   lease_checked    a first GET has completed (404 or not) -- until it
	 *                    has, the delete path must assume the lease may exist,
	 *                    i.e. behave as if an importer is reading.
	 *   lease_updated_at the importer's updated_at, or 0 if the object does
	 *                    not exist (no importer ever, or legacy export).
	 *   lease_renew_s    the importer's renew interval in seconds, so the
	 *                    grace period is 3x *its* cadence, not a guess.
	 *   lease_absent     the last completed check was a definite miss
	 *                    (404 or an empty/unreadable body), not a failed
	 *                    HEAD. export_lease_failed() never sets this: a
	 *                    bucket this node cannot reach must not look like
	 *                    "nobody imported", or the reaper would delete a
	 *                    live export's manifest during an outage.
	 *   lease_absent_at  when the *first* miss in this absence was submitted.
	 *                    Later 404s keep this timestamp: the grace is from
	 *                    the start of absence, not the last poll.
	 *
	 * lease_poller refreshes these on the lvstore's thread. Dense exports get
	 * no poller: they hold their own copies and pin nothing, so there is no
	 * snapshot to protect.
	 *
	 * lease_fetch is the check currently in flight, if any. It exists because
	 * this struct can be freed while a HEAD or GET is outstanding -- release
	 * and lvstore unload both call s3lvol_export_forget() with no regard for
	 * S3 requests -- and the completion would then write into freed memory.
	 * s3lvol_export_lease_stop() disowns it by clearing its back pointer, so
	 * the completion knows to drop the result instead.
	 *
	 * lease_failures counts consecutive failures that are neither success nor
	 * 404, so a bucket this node cannot reach does not pin every snapshot for
	 * ever: see the fallback in export_lease_failed(). */
	bool                       lease_checked;
	bool                       lease_absent;
	uint64_t                   lease_absent_at;
	uint64_t                   lease_updated_at;
	uint32_t                   lease_renew_s;
	uint32_t                   lease_failures;
	struct spdk_poller        *lease_poller;
	struct export_lease_get_ctx *lease_fetch;

	/* The other direction: leases this export *writes*, because its manifest
	 * references somebody else's objects.
	 *
	 * A derived export -- one published from a volume imported from elsewhere --
	 * names prefixes belonging to the nodes it passed through. Those nodes decide
	 * independently whether to keep their snapshots, and they decide by lease. So
	 * until now a derived export that nobody had imported yet was protected by
	 * nothing: its own reader would have renewed, but there was no reader, and
	 * the exporting node upstream would let its lease go stale and delete the
	 * objects. What was left was a manifest that looked perfectly valid and
	 * resolved to nothing.
	 *
	 * So the export renews on its own behalf, from the moment it is published.
	 * "E pins A while E is alive" rather than "while somebody reads
	 * E", which is the difference between a publish-then-leave flow working and
	 * silently rotting.
	 *
	 * An export whose sources are all its own has none. A derived one holds them
	 * until explicit release, snapshot-delete release, or materialisation.
	 * Held keys cost one PUT each per interval and are visible in the log.
	 *
	 * One client and one poller for all of them, since every source of a manifest
	 * shares its endpoint, bucket and region -- the writer refuses to reference
	 * anything else. */
	char                     (*src_lease_keys)[S3_EXPORT_KEY_MAX];
	uint32_t                   num_src_leases;
	struct spdk_poller        *src_lease_poller;
	struct s3_client          *src_lease_client;
	uint64_t                   src_lease_interval_us;

	/* This export was created by a build whose importers renew a lease.
	 *
	 * It is what makes "no lease object" readable. Without it the absence is
	 * ambiguous -- nobody ever imported, or an importer from before leases
	 * existed is reading right now and leaves no trace -- and the only safe
	 * reading of an ambiguous answer is the second one, which means the
	 * export pins its snapshot until somebody releases it by hand. With it,
	 * an absent or stale lease is evidence that an explicit snapshot delete
	 * may release the export internally.
	 *
	 * Written into the registry, so it survives a restart; absent from
	 * entries written by older builds, which therefore keep the old
	 * behaviour. */
	bool                       lease_aware;

	/* A reap of this entry is in flight. The reaper is a poller, the release
	 * it starts is asynchronous, and the entry stays in the list until that
	 * release completes -- so without this the next tick would start a second
	 * release of the same export. */
	bool                       reaping;

	TAILQ_ENTRY(s3lvol_export) link;
};

static TAILQ_HEAD(, s3lvol_export) g_exports = TAILQ_HEAD_INITIALIZER(g_exports);

/* Reaps entries whose snapshot is gone; see the reaping section below. */
static struct spdk_poller *g_exports_reaper;
static void exports_reaper_sync(void);

/* Liveness lease machinery. Declared here because s3lvol_export_add()
 * starts the watch, while the implementations live further down where the
 * s3 client calls they use are defined. */
static void s3lvol_export_lease_start(struct s3lvol_export *exp);
static void s3lvol_export_lease_stop(struct s3lvol_export *exp);

/* ==========================================================================
 * Queries
 * ========================================================================== */

static void
exports_registry_key(const char *prefix, char *out, size_t out_len)
{
	snprintf(out, out_len, "%s/meta/exports.json", prefix);
}

struct s3lvol_export *
s3lvol_export_find(struct s3lvol_lvstore *lvs, const char *uuid_str)
{
	struct s3lvol_export *exp;

	TAILQ_FOREACH(exp, &g_exports, link) {
		if (exp->lvs == lvs && strcmp(exp->uuid_str, uuid_str) == 0) {
			return exp;
		}
	}
	return NULL;
}

/* ==========================================================================
 * Liveness lease cache
 *
 * The importer renews <this lvstore>/meta/exports/<uuid>.lease in the source
 * bucket. The delete path needs to know whether an importer is still reading,
 * and it needs that answer synchronously -- s3lvol_export_pinning() returns a
 * pointer from the in-memory registry and s3lvol_lvol_destroy uses it inline,
 * so a HEAD to S3 here would force changing the destroy contract to async.
 *
 * Hence the cache: a low-frequency poller per export re-reads the lease body
 * on the lvstore's thread, and the delete path consults the cached fields.
 * Grace period W = 3x the importer's renew interval, from the body -- a lost
 * renew PUT (or clock skew) must not turn a live importer into a deletable
 * snapshot. The mis-delete window is "importer stopped renewing, then someone
 * deleted within W", which is the accepted, tunable bound.
 *
 * A lease-aware export with a confirmed miss that is older than MIN_GRACE has
 * no current reader, but remains importable until its snapshot is explicitly
 * deleted. A miss younger than that still pins: the control plane may already
 * have admitted an importer whose first lease PUT has not landed. For a
 * registry entry created before leases existed, absence is still reported as
 * LEGACY (status cannot prove the reader is gone) but does not pin: the
 * snapshot delete is the revocation.
 *
 * Until the first GET completes (lease_checked == false), the export is
 * reported as pinning. The importer may have just written its first lease and
 * the poller may not have seen it yet; refusing the delete is the safe
 * direction, and the window is normally one poll interval. A bucket that cannot
 * be read remains conservatively pinned.
 * ========================================================================== */

/* How many consecutive unreadable lease checks before this node stops treating
 * "cannot tell" as "an importer is reading".
 *
 * The conservative answer to a failed check is to refuse the delete, and for a
 * transient error that is right -- one lost HEAD must not open the window this
 * whole design exists to close. But a bucket this node genuinely cannot reach
 * (credentials rotated, endpoint firewalled, prefix permissions changed) would
 * then pin every exported snapshot for as long as the outage lasts, with no way
 * to clean up and nothing in the logs pointing at the lease as the reason. After
 * this many failures the export remains conservatively pinned and says so once. */
#define S3LVOL_LEASE_MAX_FAILURES 5

/* The shortest grace period this node will apply, and the one it assumes when a
 * lease carries updated_at but no renew_s.
 *
 * Both cases and the reasoning behind the value live with the constant in
 * vbdev_s3lvol.h, next to the renew floor it is tied to. */

static void
s3lvol_export_lease_key(struct s3lvol_export *exp, char *out, size_t out_len)
{
	snprintf(out, out_len, "%s/meta/exports/%s.lease",
		 s3lvol_lvstore_get_name(exp->lvs), exp->uuid_str);
}

struct export_lease_get_ctx {
	/* NULL once the export has been forgotten: the completion then has
	 * nothing to write to and only frees itself. */
	struct s3lvol_export *exp;
	char                  key[S3_EXPORT_KEY_MAX];
	char                 *body;
	uint64_t              size;
	/* time(NULL) at s3_head, retained for diagnostics. */
	uint64_t              submitted_at;
};

/* Record that the last completed lease lookup found no current reader.
 *
 * lease_absent_at is the first miss in this absence, not the most recent one.
 * Refreshing it on every 20 s poll would keep miss_age under one interval and
 * the grace would never elapse. After that first miss is older than grace, a
 * newly admitted importer is unprotected until the next poll sees the PUT. */
static void
export_lease_note_absent(struct s3lvol_export *exp, uint64_t submitted_at)
{
	if (!exp->lease_absent || exp->lease_absent_at == 0) {
		exp->lease_absent_at = submitted_at;
	}
	exp->lease_checked = true;
	exp->lease_absent = true;
	exp->lease_updated_at = 0;
	exp->lease_failures = 0;
	exp->lease_renew_s = 0;
}

static void
export_lease_get_done(struct export_lease_get_ctx *ctx)
{
	if (ctx->exp) {
		assert(ctx->exp->lease_fetch == ctx);
		ctx->exp->lease_fetch = NULL;
	}
	free(ctx->body);
	free(ctx);
}

/* A check that could not be completed for a reason other than "no such object".
 *
 * Counted rather than ignored. Staying silent leaves lease_checked false, and
 * s3lvol_export_pinning() then refuses the delete indefinitely -- correct for a
 * blip, wrong for an outage, and indistinguishable from the outside. */
static void
export_lease_failed(struct s3lvol_export *exp, int status)
{
	if (exp->lease_failures < UINT32_MAX) {
		exp->lease_failures++;
	}

	if (exp->lease_failures < S3LVOL_LEASE_MAX_FAILURES) {
		SPDK_WARNLOG("Could not read the lease of export %s: %s. Treating "
			     "the export as still being read for now.\n",
			     exp->uuid_str, spdk_strerror(-status));
		return;
	}

	if (exp->lease_failures == S3LVOL_LEASE_MAX_FAILURES) {
		SPDK_ERRLOG("The lease of export %s has been unreadable %u times "
			    "(%s). Keeping the export pinned until the bucket is "
			    "reachable again; deleting on an unknown lease could "
			    "break a live importer.\n",
			    exp->uuid_str, exp->lease_failures,
			    spdk_strerror(-status));
	}
	exp->lease_checked = false;
	exp->lease_updated_at = 0;
}

/* The body is a tiny JSON document. Pull out updated_at and renew_s without a
 * full JSON parse: the object is written by our own renewer with a fixed shape,
 * so a scan for the two field names is all that is needed, and anything else
 * degrades to "no recognised lease". */
static void
export_lease_got_body(void *cb_arg, uint64_t bytes_read, int status)
{
	struct export_lease_get_ctx *ctx = cb_arg;
	struct s3lvol_export *exp = ctx->exp;
	char *e;
	uint64_t updated_at = 0;
	uint32_t renew_s = 0;

	/* Forgotten while this was in flight: release or an lvstore unload freed
	 * the export. Nothing to record. */
	if (!exp) {
		export_lease_get_done(ctx);
		return;
	}

	if (status != 0) {
		if (status == -ENOENT) {
			/* Deleted between the HEAD and the GET -- release does
			 * exactly this. */
			export_lease_note_absent(exp, ctx->submitted_at);
		} else {
			export_lease_failed(exp, status);
		}
		export_lease_get_done(ctx);
		return;
	}

	/* NUL-terminate before strstr(): the object is not stored with one, and
	 * the buffer is exactly its size. */
	if (bytes_read >= ctx->size) {
		bytes_read = ctx->size - 1;
	}
	ctx->body[bytes_read] = '\0';

	if ((e = strstr(ctx->body, "\"updated_at\":")) != NULL) {
		updated_at = strtoull(e + strlen("\"updated_at\":"), NULL, 10);
	}
	if ((e = strstr(ctx->body, "\"renew_s\":")) != NULL) {
		renew_s = (uint32_t)strtoul(e + strlen("\"renew_s\":"), NULL, 10);
	}

	/* A lease that carries no updated_at is not one we recognise. */
	if (updated_at == 0) {
		export_lease_note_absent(exp, ctx->submitted_at);
		export_lease_get_done(ctx);
		return;
	}

	exp->lease_checked = true;
	exp->lease_updated_at = updated_at;
	exp->lease_failures = 0;
	exp->lease_absent = false;
	exp->lease_absent_at = 0;

	/* One key can have several writers, so retain the largest reported cadence.
	 * Current importers and derived exports both use 20 seconds; the high-water
	 * behavior preserves safety when reading leases written by older builds. */
	if (updated_at != 0) {
		if (renew_s > exp->lease_renew_s) {
			exp->lease_renew_s = renew_s;
		}
	} else {
		exp->lease_renew_s = 0;
	}

	export_lease_get_done(ctx);
}

static void
export_lease_head_done(void *cb_arg, int status)
{
	struct export_lease_get_ctx *ctx = cb_arg;
	struct s3lvol_export *exp = ctx->exp;
	int rc;

	if (!exp) {
		export_lease_get_done(ctx);
		return;
	}

	if (status == -ENOENT) {
		export_lease_note_absent(exp, ctx->submitted_at);
		export_lease_get_done(ctx);
		return;
	}
	if (status != 0) {
		export_lease_failed(exp, status);
		export_lease_get_done(ctx);
		return;
	}
	/* Empty or implausible: not a lease we can read, and not an error either. */
	if (ctx->size == 0 || ctx->size > 4096) {
		export_lease_note_absent(exp, ctx->submitted_at);
		export_lease_get_done(ctx);
		return;
	}

	/* One extra byte for the terminator the object does not carry. */
	ctx->body = malloc(ctx->size + 1);
	if (!ctx->body) {
		export_lease_get_done(ctx);
		return;
	}

	rc = s3_get_range(s3lvol_lvstore_get_client(exp->lvs), ctx->key,
			  0, ctx->size, ctx->body, export_lease_got_body, ctx);
	if (rc != 0) {
		export_lease_failed(exp, rc);
		export_lease_get_done(ctx);
	}
}

static int
export_lease_renew(void *arg)
{
	struct s3lvol_export *exp = arg;
	struct export_lease_get_ctx *ctx;
	int rc;

	/* One check at a time. A slow bucket must not queue a check per tick,
	 * and the completion writes to fields a second check would race on. */
	if (exp->lease_fetch) {
		return SPDK_POLLER_IDLE;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return SPDK_POLLER_IDLE;
	}
	ctx->exp = exp;
	ctx->submitted_at = (uint64_t)time(NULL);
	s3lvol_export_lease_key(exp, ctx->key, sizeof(ctx->key));
	exp->lease_fetch = ctx;

	rc = s3_head(s3lvol_lvstore_get_client(exp->lvs), ctx->key, &ctx->size,
		     export_lease_head_done, ctx);
	if (rc != 0) {
		export_lease_failed(exp, rc);
		export_lease_get_done(ctx);
	}
	return SPDK_POLLER_IDLE;
}

static void
s3lvol_export_lease_stop(struct s3lvol_export *exp)
{
	if (exp->lease_poller) {
		spdk_poller_unregister(&exp->lease_poller);
		exp->lease_poller = NULL;
	}

	/* Disown a check still in flight rather than waiting for it. The S3
	 * request cannot be cancelled and its completion runs on this thread
	 * later, by which time this struct may be freed -- clearing the back
	 * pointer is what tells that completion to drop the result. The ctx
	 * itself is freed by its own completion. */
	if (exp->lease_fetch) {
		exp->lease_fetch->exp = NULL;
		exp->lease_fetch = NULL;
	}
}

static void
s3lvol_export_lease_start(struct s3lvol_export *exp)
{
	uint64_t interval;

	if (exp->layout != S3_EXPORT_LAYOUT_REF || exp->lease_poller) {
		return;
	}

	/* Poll at the same fixed cadence importers renew. The grace period is still
	 * derived from the importer's reported renew_s, not this local timer. */
	interval = S3LVOL_LEASE_RENEW_MIN_SEC;

	exp->lease_poller = SPDK_POLLER_REGISTER(export_lease_renew, exp,
						 interval * SPDK_SEC_TO_USEC);
	if (!exp->lease_poller) {
		SPDK_WARNLOG("Could not start the lease poller for export %s\n",
			     exp->uuid_str);
		return;
	}

	SPDK_NOTICELOG("watching lease of export %s every %" PRIu64
		       " second(s)\n", exp->uuid_str, interval);

	/* Check once now, rather than at the end of the first interval. */
	export_lease_renew(exp);
}

/* ==========================================================================
 * Leases this export writes, rather than reads
 *
 * A derived export names prefixes belonging to other nodes. Each of those decides
 * on its own whether to keep the snapshot behind them, and decides by reading a
 * lease -- so somebody has to write one, or the objects this manifest references
 * are deleted while it still names them.
 *
 * The importer of a derived export does write them (see import_lease_start), and
 * that covers an export somebody is reading. It does not cover one that has been
 * published and not yet imported, which is exactly the state a
 * publish-then-hand-over flow leaves behind. So the export renews for itself, from
 * publication until it is released.
 * ========================================================================== */

static void
export_src_lease_put_done(void *cb_arg, int status)
{
	char *key = cb_arg;

	/* Fire and forget, like every other lease PUT: a lost one is absorbed by the
	 * grace period at the other end and the next tick tries again. The key rather
	 * than the export uuid, because an export may renew several and the one that
	 * fails is the prefix whose node may now delete data this manifest needs. */
	if (status != 0) {
		SPDK_WARNLOG("source lease renew failed for '%s': %s\n",
			     key, spdk_strerror(-status));
	}
	free(key);
}

static int
export_src_lease_renew(void *arg)
{
	struct s3lvol_export *exp = arg;
	char body[160];
	struct iovec iov;
	uint64_t now = (uint64_t)time(NULL);
	uint32_t claim;
	uint32_t i;

	/* Derived exports renew upstream at the same fixed cadence as importers.
	 * Several writers can share one lease key, so the source still keeps the
	 * largest renew_s it has observed. */
	claim = S3LVOL_LEASE_RENEW_MIN_SEC;

	/* Same document an importer writes, so the reading side needs no new case.
	 * importer_id says which node is holding the reference and why -- an operator
	 * looking at a lease that will not go away needs to know it belongs to an
	 * export rather than to a volume, since the way to clear it is
	 * rcow_release_export and not deleting anything. */
	snprintf(body, sizeof(body),
		 "{\"importer_id\":\"export:%s\",\"updated_at\":%" PRIu64
		 ",\"renew_s\":%lu}",
		 exp->uuid_str, now, (unsigned long)claim);
	iov.iov_base = body;
	iov.iov_len  = strlen(body);

	for (i = 0; i < exp->num_src_leases; i++) {
		char *key;

		/* Owned by the completion: s3_put copies the body but not cb_arg, and
		 * the callback outlives both the poller and, after forget(), the array
		 * these were copied from. */
		key = strdup(exp->src_lease_keys[i]);
		if (!key) {
			continue;
		}
		/* Each submitted independently: they protect different nodes'
		 * snapshots, so one unreachable prefix must not take the rest stale
		 * with it. */
		if (s3_put(exp->src_lease_client, key, &iov, 1, false,
			   export_src_lease_put_done, key) != 0) {
			SPDK_WARNLOG("source lease renew submit failed for '%s'\n", key);
			free(key);
		}
	}

	return SPDK_POLLER_IDLE;
}

static void
export_src_lease_stop(struct s3lvol_export *exp)
{
	if (exp->src_lease_poller) {
		spdk_poller_unregister(&exp->src_lease_poller);
		exp->src_lease_poller = NULL;
	}
	if (exp->src_lease_client) {
		s3_client_put(exp->src_lease_client);
		exp->src_lease_client = NULL;
	}
	/* Safe with PUTs in flight: each carries its own copy of the key, which is
	 * why the renew strdup()s rather than passing these. */
	free(exp->src_lease_keys);
	exp->src_lease_keys = NULL;
	exp->num_src_leases = 0;
}

/* Bring the poller up for keys already in exp->src_lease_keys. Shared by the
 * publish path, which derives the keys from a manifest, and the attach path, which
 * reads them back from the registry -- the difference between the two is only
 * where the keys came from. \p src describes where to reach them. */
static void
export_src_lease_arm(struct s3lvol_export *exp, const struct s3_export_source *src)
{
	struct s3_target target = {0};
	uint32_t j;
	int rc;

	if (exp->num_src_leases == 0) {
		free(exp->src_lease_keys);
		exp->src_lease_keys = NULL;
		return;
	}

	/* Every source shares the manifest's endpoint, bucket and region, so one
	 * client serves all of them -- the writer refuses to reference another
	 * bucket, which is what makes that true. */
	target.endpoint  = (char *)src->endpoint;
	target.region    = (char *)src->region;
	target.bucket    = (char *)src->bucket;
	target.auth_mode = S3_AUTH_ENV;
	rc = s3_client_get_or_create(&target, &exp->src_lease_client);
	if (rc != 0) {
		SPDK_WARNLOG("export %s: no S3 client for its source leases: %s. The "
			     "nodes it references may delete their snapshots.\n",
			     exp->uuid_str, spdk_strerror(-rc));
		export_src_lease_stop(exp);
		return;
	}

	/* Write often enough that one lost PUT is covered by the next one within
	 * the source's grace window. */
	exp->src_lease_interval_us = S3LVOL_LEASE_RENEW_MIN_SEC * SPDK_SEC_TO_USEC;
	exp->src_lease_poller = SPDK_POLLER_REGISTER(export_src_lease_renew, exp,
						     exp->src_lease_interval_us);
	if (!exp->src_lease_poller) {
		SPDK_WARNLOG("export %s: could not start its source lease poller\n",
			     exp->uuid_str);
		export_src_lease_stop(exp);
		return;
	}

	/* Now, not one interval from now. At publish there may be no upstream lease
	 * at all yet; at attach, whatever was there has been going stale for however
	 * long this node was down, and an absent or stale lease is what lets the
	 * other end delete. */
	export_src_lease_renew(exp);

	SPDK_NOTICELOG("export %s renews %u upstream lease(s) every %u second(s) "
		       "because it references other prefixes; rcow_release_export is "
		       "what stops it\n", exp->uuid_str, exp->num_src_leases,
		       S3LVOL_LEASE_RENEW_MIN_SEC);
	for (j = 0; j < exp->num_src_leases; j++) {
		SPDK_NOTICELOG("export %s upstream lease [%u]: %s\n", exp->uuid_str, j,
			       exp->src_lease_keys[j]);
	}
}

/* Start renewing on behalf of \p m 's sources. Does nothing for a manifest that
 * references only its own prefix, which is every non-derived export. */
static void
export_src_lease_start(struct s3lvol_export *exp, const struct s3_export_manifest *m)
{
	uint32_t j;

	/* num_srcs <= 1 means entry 0 only, i.e. this lvstore's own prefix, whose
	 * objects this node already owns. Nothing to ask anybody else for. */
	if (m->layout != S3_EXPORT_LAYOUT_REF || m->num_srcs <= 1) {
		return;
	}

	exp->src_lease_keys = calloc(m->num_srcs, sizeof(*exp->src_lease_keys));
	if (!exp->src_lease_keys) {
		SPDK_WARNLOG("export %s: no memory for its source leases; the nodes it "
			     "references may delete their snapshots\n", exp->uuid_str);
		return;
	}

	/* From entry 1: entry 0 is this export's own prefix. Each later entry names
	 * the export that governs it, and the lease goes *there* rather than to
	 * whoever this manifest was derived from -- that middleman may be gone, and
	 * the objects are not its to protect. */
	for (j = 1; j < m->num_srcs; j++) {
		if (m->srcs[j].export_uuid[0] == '\0') {
			SPDK_WARNLOG("export %s names prefix '%s' with no export uuid, "
				     "so nothing can renew its lease; that node may "
				     "delete the snapshot behind those chunks\n",
				     exp->uuid_str, m->srcs[j].prefix);
			continue;
		}
		snprintf(exp->src_lease_keys[exp->num_src_leases],
			 sizeof(exp->src_lease_keys[0]), "%s/meta/exports/%s.lease",
			 m->srcs[j].prefix, m->srcs[j].export_uuid);
		exp->num_src_leases++;
	}

	export_src_lease_arm(exp, &m->src);
}

/* One export's verdict on one snapshot. Split out of s3lvol_export_pinning()
 * because two callers need different amounts of detail from the same decision:
 * the delete path only asks "may I delete this now", while the pending-delete
 * poller also has to know *why* the answer is yes.
 *
 * That distinction is the whole point. A lease that has gone stale is positive
 * evidence -- an importer wrote it and stopped renewing, so nobody is reading --
 * and a delete on that basis is safe to carry out unattended. A legacy export
 * predates trustworthy leases, so status cannot prove a reader has gone; the
 * snapshot delete itself is still the revocation. */
static enum s3lvol_export_pin
export_pin_verdict(const struct s3lvol_export *exp)
{
	uint64_t now, grace;

	/* A dense export holds copies, so it does not pin the snapshot. */
	if (exp->layout != S3_EXPORT_LAYOUT_REF) {
		return S3LVOL_EXPORT_PIN_NONE;
	}

	/* Before the first GET completes, assume the lease may exist and an
	 * importer may be reading it: refuse, and let the next poll decide. */
	if (!exp->lease_checked) {
		return S3LVOL_EXPORT_PIN_LEASE;
	}

	/* A lease-aware exporter can distinguish a live reader from no reader.
	 * A confirmed miss therefore lets an explicit snapshot delete release the
	 * export -- but only once the miss is older than MIN_GRACE. Until then the
	 * first lease PUT of an importer that was just admitted may still be in
	 * flight. It does not expire or reap the export by itself: while the
	 * snapshot exists the export remains a valid target for a later import.
	 *
	 * For an entry written before leases existed, absence remains LEGACY for
	 * status. The delete path does not wait for a lease that will never appear. */
	if (exp->lease_updated_at == 0) {
		if (exp->lease_aware) {
			uint64_t miss_age;

			if (!exp->lease_absent) {
				return S3LVOL_EXPORT_PIN_LEASE;
			}
			now = (uint64_t)time(NULL);
			if (exp->lease_absent_at == 0 || now < exp->lease_absent_at) {
				return S3LVOL_EXPORT_PIN_LEASE;
			}
			miss_age = now - exp->lease_absent_at;
			if (miss_age < S3LVOL_LEASE_MIN_GRACE_SEC) {
				return S3LVOL_EXPORT_PIN_LEASE;
			}
			return S3LVOL_EXPORT_PIN_STALE;
		}
		return S3LVOL_EXPORT_PIN_LEGACY;
	}

	now = (uint64_t)time(NULL);
	grace = 3 * (uint64_t)exp->lease_renew_s;
	if (grace < S3LVOL_LEASE_MIN_GRACE_SEC) {
		/* Either no renew_s at all (an older importer), or one small enough
		 * that honouring it would delete data under a reader that is merely
		 * slow. See the constant. */
		grace = S3LVOL_LEASE_MIN_GRACE_SEC;
	}
	/* Guard against a lease stamped in the future -- a skewed importer clock
	 * would otherwise make now - updated_at wrap and read as ancient. */
	if (exp->lease_updated_at > now || now - exp->lease_updated_at < grace) {
		return S3LVOL_EXPORT_PIN_LEASE;
	}
	return S3LVOL_EXPORT_PIN_STALE;
}

/* Whether this live lvol is the snapshot the registry entry was published from.
 *
 * Prefer snapshot_uuid when the entry has one. blob_id is not identity
 * (blobstore hands the same id back after a delete); it is only the fallback
 * for registries that predate the uuid field. Empty uuid is not a refusal:
 * treating it as "cannot prove" would publish a second export of a still-live
 * snapshot after an upgrade. */
static bool
export_live_lvol_matches(const struct s3lvol_export *exp, const struct spdk_lvol *lvol)
{
	if (!lvol) {
		return false;
	}
	if (exp->snapshot_uuid[0] != '\0') {
		return strcmp(lvol->uuid_str, exp->snapshot_uuid) == 0;
	}
	if (exp->blob_id != 0) {
		return lvol->blob_id == exp->blob_id;
	}
	return true;
}

/* Whether this registry entry is an export of the live snapshot named here. */
static bool
export_names_snapshot(const struct s3lvol_export *exp, struct s3lvol_lvstore *lvs,
		       const char *snapshot_name)
{
	if (exp->lvs != lvs || strcmp(exp->snapshot, snapshot_name) != 0) {
		return false;
	}
	return export_live_lvol_matches(exp, s3lvol_lvol_find(lvs, snapshot_name));
}

static bool
export_snapshot_alive(const struct s3lvol_export *exp)
{
	if (!exp || !exp->lvs) {
		return false;
	}
	return export_names_snapshot(exp, exp->lvs, exp->snapshot);
}

enum s3lvol_export_pin
s3lvol_export_pin_state(struct s3lvol_lvstore *lvs, const char *snapshot_name)
{
	enum s3lvol_export_pin worst = S3LVOL_EXPORT_PIN_NONE;
	struct s3lvol_export *exp;

	if (!lvs || !snapshot_name) {
		return S3LVOL_EXPORT_PIN_NONE;
	}

	/* Several exports can name the same snapshot, and the most restrictive
	 * verdict wins: one live reader is enough to pin it, and one export whose
	 * liveness is unknowable is enough to keep the delete a decision. */
	TAILQ_FOREACH(exp, &g_exports, link) {
		enum s3lvol_export_pin v;

		if (!export_names_snapshot(exp, lvs, snapshot_name)) {
			continue;
		}
		v = export_pin_verdict(exp);
		if (v == S3LVOL_EXPORT_PIN_LEASE) {
			return S3LVOL_EXPORT_PIN_LEASE;
		}
		if (v > worst) {
			worst = v;
		}
	}
	return worst;
}

struct s3lvol_export *
s3lvol_export_pinning(struct s3lvol_lvstore *lvs, const char *snapshot_name)
{
	struct s3lvol_export *exp;

	if (!lvs || !snapshot_name) {
		return NULL;
	}

	TAILQ_FOREACH(exp, &g_exports, link) {
		if (!export_names_snapshot(exp, lvs, snapshot_name)) {
			continue;
		}

		switch (export_pin_verdict(exp)) {
		case S3LVOL_EXPORT_PIN_LEASE:
			/* An importer is reading, or may be. */
			return exp;
		case S3LVOL_EXPORT_PIN_LEGACY:
			/* Status still says liveness is unknown. An explicit snapshot
			 * delete is the revocation, same as STALE — rcow_get_snapshot_status
			 * reports deletable YES, and the poller will finish a recorded
			 * intent. A pre-lease importer cannot be observed. */
			continue;
		case S3LVOL_EXPORT_PIN_STALE:
			/* No lease within grace, or nobody has renewed within grace. */
			continue;
		case S3LVOL_EXPORT_PIN_NONE:
		default:
			continue;
		}
	}
	return NULL;
}

struct s3lvol_export *
s3lvol_export_first(struct s3lvol_lvstore *lvs)
{
	struct s3lvol_export *exp;

	TAILQ_FOREACH(exp, &g_exports, link) {
		if (exp->lvs == lvs) {
			return exp;
		}
	}
	return NULL;
}

struct s3lvol_export *
s3lvol_export_next(struct s3lvol_export *prev)
{
	struct s3lvol_export *exp = prev;

	while ((exp = TAILQ_NEXT(exp, link)) != NULL) {
		if (exp->lvs == prev->lvs) {
			return exp;
		}
	}
	return NULL;
}

struct s3lvol_export *
s3lvol_export_first_ref_for_snapshot(struct s3lvol_lvstore *lvs,
				      const char *snapshot_name)
{
	struct s3lvol_export *exp;

	if (!lvs || !snapshot_name) {
		return NULL;
	}
	TAILQ_FOREACH(exp, &g_exports, link) {
		if (!exp->reaping && exp->layout == S3_EXPORT_LAYOUT_REF &&
		    export_names_snapshot(exp, lvs, snapshot_name)) {
			return exp;
		}
	}
	return NULL;
}

struct s3lvol_export *
s3lvol_export_find_for_snapshot(struct s3lvol_lvstore *lvs,
				 const char *snapshot_name)
{
	struct s3lvol_export *exp;

	if (!lvs || !snapshot_name) {
		return NULL;
	}
	TAILQ_FOREACH(exp, &g_exports, link) {
		if (!exp->reaping &&
		    export_names_snapshot(exp, lvs, snapshot_name)) {
			return exp;
		}
	}
	return NULL;
}

void
s3lvol_export_get(const struct s3lvol_export *exp, struct s3lvol_export_entry *out)
{
	out->export_uuid = exp->uuid_str;
	out->snapshot    = exp->snapshot;
	out->snapshot_uuid = exp->snapshot_uuid;
	out->blob_id     = exp->blob_id;
	out->expires_at  = exp->expires_at;
	out->generation  = exp->generation;
	out->is_ref      = (exp->layout == S3_EXPORT_LAYOUT_REF);
	out->lease_aware = exp->lease_aware;
	out->lease_checked = exp->lease_checked;
	out->lease_absent = exp->lease_absent;
	out->lease_watch = exp->lease_poller != NULL;
	out->lease_updated_at = exp->lease_updated_at;
	out->lease_renew_s = exp->lease_renew_s;
	out->pin = export_pin_verdict(exp);
	out->reaping = exp->reaping;

	out->snapshot_alive = export_snapshot_alive(exp);
}

/* ==========================================================================
 * Mutation
 * ========================================================================== */

struct s3lvol_export *
s3lvol_export_add(struct s3lvol_lvstore *lvs, const struct s3_export_manifest *m,
		  const char *snapshot_name)
{
	struct s3lvol_export *exp;

	exp = calloc(1, sizeof(*exp));
	if (!exp) {
		return NULL;
	}
	exp->lvs   = lvs;
	exp->layout     = m->layout;
	exp->blob_id    = m->src.blob_id;
	exp->expires_at = m->expires_at;
	exp->generation = m->generation;
	/* Created by this build, so any importer of it renews a lease. */
	exp->lease_aware = true;
	snprintf(exp->uuid_str, sizeof(exp->uuid_str), "%s", m->uuid_str);
	snprintf(exp->snapshot, sizeof(exp->snapshot), "%s", snapshot_name);
	if (m->src.snapshot_uuid[0] != '\0') {
		snprintf(exp->snapshot_uuid, sizeof(exp->snapshot_uuid), "%s",
			 m->src.snapshot_uuid);
	}

	TAILQ_INSERT_TAIL(&g_exports, exp, link);

	/* Both ways an export appears need the lease watched: a fresh export
	 * because the first importer may arrive any moment (and the safe
	 * direction before the first lease GET is to refuse deletes anyway),
	 * and an attach resurrecting the registry because the importers it
	 * describes may still be reading. */
	s3lvol_export_lease_start(exp);
	/* And, if this export references anybody else's prefixes, start renewing
	 * *their* leases too -- from now rather than from the first import. See the
	 * fields' comment in struct s3lvol_export. */
	export_src_lease_start(exp, m);
	exports_reaper_sync();

	return exp;
}

void
s3lvol_export_forget(struct s3lvol_export *exp)
{
	s3lvol_export_lease_stop(exp);
	/* The only thing that stops the upstream renewals. Deliberately: nothing can
	 * tell "nobody will ever import this" from "nobody has yet", so releasing the
	 * export is the caller's statement that the reference is finished with. */
	export_src_lease_stop(exp);
	TAILQ_REMOVE(&g_exports, exp, link);
	free(exp);
	exports_reaper_sync();
}

/* ==========================================================================
 * Reaping exports nothing can serve any more
 *
 * A reference export is its snapshot: the manifest names the snapshot's live
 * chunk objects, and deleting the snapshot releases them. So once the snapshot
 * is gone the export cannot be imported by anyone, ever -- what is left is a
 * registry entry and a manifest object that nothing will read.
 *
 * A live snapshot keeps every export made from it importable, even if nobody
 * has imported yet. The normal snapshot delete path releases those exports
 * first; this reaper only repairs entries whose snapshot is already gone.
 *
 * Reaping is exactly a release, reusing the same path: it deletes the manifest,
 * drops the entry and rewrites the registry. For a reference layout that is all
 * a release does anyway -- it owns no objects of its own (see
 * release_delete_chunks) -- so nothing that belongs to anyone else is touched.
 * The snapshot is not deleted.
 *
 * Only reference exports. A dense export is self-contained: its snapshot
 * disappearing says nothing about whether it can still be imported, and
 * deleting its manifest would destroy a working export. Entries without
 * lease_aware are left alone on an absent lease: a pre-lease importer reads
 * without writing one, so 404 is not evidence.
 * ========================================================================== */

/* The same cadence as the pending-delete poller, and for the same reason:
 * nothing here is urgent -- the cost of a dead entry lingering is one row in a
 * registry -- but a completed delete is what makes a snapshot-gone export
 * reapable, so a matching period keeps that step from trailing the first by
 * an awkward margin. */
#define S3LVOL_EXPORT_REAP_US (60ULL * SPDK_SEC_TO_USEC)

/* Reaps started per tick, so a node that just deleted a thousand snapshots does
 * not answer with a thousand simultaneous manifest deletes and registry
 * rewrites. */
#define S3LVOL_EXPORT_REAP_BATCH 8

struct export_reap_ctx {
	struct s3lvol_lvstore *lvs;
	char                   uuid_str[SPDK_UUID_STRING_LEN];
};

static void
export_reaped(void *cb_arg, int lvolerrno)
{
	struct export_reap_ctx *ctx = cb_arg;
	struct s3lvol_export *exp;

	if (lvolerrno == 0) {
		SPDK_NOTICELOG("export %s reaped\n", ctx->uuid_str);
		goto out;
	}

	if (lvolerrno == -ENOENT) {
		/* Release now forgets on a missing manifest, so this is only a
		 * fallback if that path still reported ENOENT. Drop the entry. */
		exp = s3lvol_export_find(ctx->lvs, ctx->uuid_str);
		if (exp) {
			s3lvol_export_forget(exp);
			if (s3lvol_export_registry_save(ctx->lvs, NULL, NULL) != 0) {
				SPDK_WARNLOG("Dropped dead export %s but could not "
					     "rewrite the registry of '%s'\n",
					     ctx->uuid_str,
					     s3lvol_lvstore_get_name(ctx->lvs));
			}
		}
		SPDK_NOTICELOG("export %s reaped: neither its snapshot nor its "
			       "manifest is left\n", ctx->uuid_str);
		goto out;
	}

	/* The entry keeps its reaping flag, so it is not tried again: something
	 * about this export needs looking at, and a poller repeating the same
	 * failure every minute would only fill the log. rcow_release_export still
	 * works by hand. */
	SPDK_WARNLOG("export %s could not be reaped: %s. It will not be retried; "
		     "release it explicitly if it is really dead.\n",
		     ctx->uuid_str, spdk_strerror(-lvolerrno));
out:
	free(ctx);
}

static int
exports_reap(void *arg)
{
	struct s3lvol_export *dead[S3LVOL_EXPORT_REAP_BATCH];
	struct s3lvol_lvstore *lvs;
	unsigned n = 0;
	unsigned i;

	/* Iterating the *lvstores* rather than g_exports, because only a loaded
	 * lvstore can answer whether a snapshot exists. An lvstore reaches
	 * g_lvstores only once its blobstore is up and its lvols are open, so
	 * this cannot mistake a half-loaded store for one whose snapshots are all
	 * gone -- which would reap every export it has. */
	for (lvs = s3lvol_lvstore_first(); lvs;
	     lvs = s3lvol_lvstore_next(lvs)) {
		struct s3lvol_export *exp;

		if (!s3lvol_lvstore_get_lvs(lvs)) {
			continue;
		}

		for (exp = s3lvol_export_first(lvs); exp;
		     exp = s3lvol_export_next(exp)) {
			if (exp->layout != S3_EXPORT_LAYOUT_REF || exp->reaping) {
				continue;
			}
			if (n >= S3LVOL_EXPORT_REAP_BATCH) {
				continue;
			}
			if (export_snapshot_alive(exp)) {
				continue;
			}
			dead[n++] = exp;
		}
	}

	/* Collected first: the release is asynchronous but can fail inline, and
	 * its completion removes the entry from the list being walked. */
	for (i = 0; i < n; i++) {
		struct export_reap_ctx *ctx;
		int rc;

		ctx = calloc(1, sizeof(*ctx));
		if (!ctx) {
			continue;
		}
		ctx->lvs = dead[i]->lvs;
		snprintf(ctx->uuid_str, sizeof(ctx->uuid_str), "%s",
			 dead[i]->uuid_str);
		dead[i]->reaping = true;

		SPDK_NOTICELOG("reaping export %s: the snapshot it referenced "
			       "is gone\n", ctx->uuid_str);

		rc = s3lvol_export_release(ctx->lvs, ctx->uuid_str, export_reaped,
					   ctx);
		if (rc != 0) {
			/* -EBUSY: a volume in this process still reads through
			 * the export. Leave it and look again. */
			dead[i]->reaping = false;
			SPDK_DEBUGLOG(vbdev_s3lvol,
				      "export %s not reaped this time: %s\n",
				      ctx->uuid_str, spdk_strerror(-rc));
			free(ctx);
		}
	}

	return n ? SPDK_POLLER_BUSY : SPDK_POLLER_IDLE;
}

/* Runs only while this node has exports at all, like the pending-delete poller:
 * a target with nothing published should have no timer ticking. */
static void
exports_reaper_sync(void)
{
	bool want = !TAILQ_EMPTY(&g_exports);

	if (want && !g_exports_reaper) {
		g_exports_reaper = SPDK_POLLER_REGISTER(exports_reap, NULL,
							S3LVOL_EXPORT_REAP_US);
		if (!g_exports_reaper) {
			SPDK_WARNLOG("could not start the export reaper; dead "
				     "export entries will have to be released by "
				     "hand\n");
		}
	} else if (!want && g_exports_reaper) {
		spdk_poller_unregister(&g_exports_reaper);
		g_exports_reaper = NULL;
	}
}

void
s3lvol_export_set_materialised(struct s3lvol_export *exp, uint32_t generation)
{
	exp->layout = S3_EXPORT_LAYOUT_DENSE;
	exp->generation = generation;
	/* A deadline was only ever about how long this node would hold a snapshot
	 * for somebody else. There is no snapshot involved any more. */
	exp->expires_at = 0;
	/* Nor a snapshot to protect, so the lease watch stops with it. */
	s3lvol_export_lease_stop(exp);
	/* And it references nobody else's objects any more: materialising uploaded
	 * copies of everything it used to point at, including whatever it inherited
	 * from another prefix. Holding those upstream leases now would keep another
	 * node's snapshot alive for data this export no longer needs. */
	export_src_lease_stop(exp);
}

void
s3lvol_export_set_local_ref(struct s3lvol_export *exp, uint32_t generation)
{
	/* The replacement manifest is still a reference export: its objects belong
	 * to the local snapshot, so the ordinary export lease continues to pin that
	 * snapshot. What changed is that no chunk points at another lvstore any
	 * more, hence only the upstream leases are retired. */
	assert(exp->layout == S3_EXPORT_LAYOUT_REF);
	exp->generation = generation;
	export_src_lease_stop(exp);
}

void
s3lvol_xfer_exports_fini(struct s3lvol_lvstore *lvs)
{
	struct s3lvol_export *exp, *tmp;

	/* Only the memory. The registry object describes the lvstore, not this
	 * process, and the next attach needs every entry of it to know what it is
	 * still obliged to keep. */
	TAILQ_FOREACH_SAFE(exp, &g_exports, link, tmp) {
		if (exp->lvs == lvs) {
			s3lvol_export_forget(exp);
		}
	}
}

/* ==========================================================================
 * Persistence
 * ========================================================================== */

static int
exports_serialize(struct s3lvol_lvstore *lvs, char **out, size_t *out_len)
{
	struct s3lvol_json_buf buf = {0};
	struct spdk_json_write_ctx *w;
	struct s3lvol_export *exp;
	int rc;

	w = spdk_json_write_begin(s3lvol_json_buf_append, &buf, 0);
	if (!w) {
		return -ENOMEM;
	}

	spdk_json_write_object_begin(w);
	spdk_json_write_named_uint32(w, "version", S3_EXPORT_VERSION);
	spdk_json_write_named_array_begin(w, "exports");

	TAILQ_FOREACH(exp, &g_exports, link) {
		if (exp->lvs != lvs) {
			continue;
		}
		spdk_json_write_object_begin(w);
		spdk_json_write_named_string(w, "export_uuid", exp->uuid_str);
		spdk_json_write_named_string(w, "snapshot", exp->snapshot);
		if (exp->snapshot_uuid[0] != '\0') {
			spdk_json_write_named_string(w, "snapshot_uuid",
						     exp->snapshot_uuid);
		}
		spdk_json_write_named_uint64(w, "blob_id", exp->blob_id);
		spdk_json_write_named_string(w, "layout",
					     exp->layout == S3_EXPORT_LAYOUT_REF ?
					   S3_EXPORT_LAYOUT_REF_STR :
					     S3_EXPORT_LAYOUT_DENSE_STR);
		spdk_json_write_named_uint64(w, "expires_at", exp->expires_at);
		spdk_json_write_named_uint32(w, "generation", exp->generation);
		/* Only written when true. An older build ignores the unknown
		 * field, and this build reads its absence as "not lease-aware",
		 * which is what an entry written by an older build means. */
		if (exp->lease_aware) {
			spdk_json_write_named_bool(w, "lease_aware", true);
		}
		/* The upstream leases this export renews, written as the keys rather
		 * than as the prefixes they came from.
		 *
		 * Persisted because without them a restart forgets an obligation to
		 * *another node*: the entry comes back, its own lease is watched again,
		 * and nothing renews upstream -- so the node this manifest references
		 * sees a stale lease and deletes the snapshot behind chunks this export
		 * still names. Exactly the failure this whole mechanism exists to
		 * prevent, arriving by way of a restart.
		 *
		 * Keys and not prefixes so that reloading needs no manifest: the
		 * manifest is on S3 and fetching it at attach would make the registry
		 * load depend on a GET per derived export. What is stored is precisely
		 * what has to be written.
		 *
		 * Absent for a non-derived export, which is the common case, and absent
		 * in anything an older build wrote -- read as "renews nothing", which is
		 * what such an entry meant. */
		if (exp->num_src_leases > 0) {
			uint32_t k;

			spdk_json_write_named_array_begin(w, "src_leases");
			for (k = 0; k < exp->num_src_leases; k++) {
				spdk_json_write_string(w, exp->src_lease_keys[k]);
			}
			spdk_json_write_array_end(w);
		}
		spdk_json_write_object_end(w);
	}

	spdk_json_write_array_end(w);
	spdk_json_write_object_end(w);

	rc = spdk_json_write_end(w);
	if (rc != 0 || !buf.data) {
		free(buf.data);
		return rc != 0 ? rc : -ENOMEM;
	}

	*out = buf.data;
	*out_len = buf.len;
	return 0;
}

/* The upstream lease keys of one entry. A fixed table rather than allocations,
 * because the whole registry is decoded into fixed tables and one exception would
 * be one more thing to free on every error path here. */
struct src_leases_holder {
	char  *k[S3_EXPORT_MAX_SOURCES];
	size_t n;
};

static int
decode_src_leases(const struct spdk_json_val *val, void *out)
{
	struct src_leases_holder *h = out;

	return spdk_json_decode_array(val, spdk_json_decode_string, h->k,
				      S3_EXPORT_MAX_SOURCES, &h->n, sizeof(h->k[0]));
}

/* Resume renewing the upstream leases of an export read back from the registry.
 *
 * The keys are stored verbatim, so nothing has to be derived -- but the endpoint,
 * bucket and region to reach them do, and there is no manifest here to read them
 * from. They come from this lvstore's own namespace, which is correct because a
 * manifest may not reference another bucket: the writer refuses, so every source
 * of it is necessarily in the same place as the export itself. If that rule ever
 * relaxes, this is one of the places that has to learn about it. */
static void
export_src_lease_restart(struct s3lvol_export *exp,
			 const struct src_leases_holder *stored)
{
	struct s3_export_source src = {0};
	const struct s3_target *tgt;
	const char *ns;
	size_t i;

	if (stored->n == 0) {
		return;
	}

	ns  = s3lvol_lvstore_get_namespace(exp->lvs);
	tgt = rcow_namespace_to_target(ns);
	if (!tgt || !tgt->endpoint || !tgt->bucket) {
		SPDK_WARNLOG("export %s references other prefixes but namespace '%s' "
			     "does not resolve, so its upstream leases cannot be "
			     "renewed; those nodes may delete their snapshots\n",
			     exp->uuid_str, ns ? ns : "(none)");
		return;
	}

	exp->src_lease_keys = calloc(stored->n, sizeof(*exp->src_lease_keys));
	if (!exp->src_lease_keys) {
		SPDK_WARNLOG("export %s: no memory to resume its source leases\n",
			     exp->uuid_str);
		return;
	}
	for (i = 0; i < stored->n; i++) {
		snprintf(exp->src_lease_keys[i], sizeof(exp->src_lease_keys[0]), "%s",
			 stored->k[i]);
	}
	exp->num_src_leases = (uint32_t)stored->n;

	snprintf(src.endpoint, sizeof(src.endpoint), "%s", tgt->endpoint);
	snprintf(src.region, sizeof(src.region), "%s", tgt->region ? tgt->region : "");
	snprintf(src.bucket, sizeof(src.bucket), "%s", tgt->bucket);
	export_src_lease_arm(exp, &src);
}

struct export_entry_json {
	char *export_uuid;
	char    *snapshot;
	char    *snapshot_uuid;
	char    *layout;
	uint64_t blob_id;
	uint64_t expires_at;
	uint32_t generation;
	bool     lease_aware;
	struct src_leases_holder src_leases;
};

static const struct spdk_json_object_decoder export_entry_decoders[] = {
	{"export_uuid", offsetof(struct export_entry_json, export_uuid), spdk_json_decode_string, false},
	{"snapshot",  offsetof(struct export_entry_json, snapshot),    spdk_json_decode_string, false},
	{"snapshot_uuid", offsetof(struct export_entry_json, snapshot_uuid), spdk_json_decode_string, true},
	{"layout",      offsetof(struct export_entry_json, layout),      spdk_json_decode_string, false},
	{"blob_id",     offsetof(struct export_entry_json, blob_id),     spdk_json_decode_uint64, true},
	{"expires_at",  offsetof(struct export_entry_json, expires_at),  spdk_json_decode_uint64, true},
	{"generation",  offsetof(struct export_entry_json, generation),  spdk_json_decode_uint32, true},
	{"lease_aware", offsetof(struct export_entry_json, lease_aware), spdk_json_decode_bool, true},
	/* Optional: absent for a non-derived export and for anything an older build
	 * wrote, both of which mean "renews nothing upstream". */
	{"src_leases",  offsetof(struct export_entry_json, src_leases),  decode_src_leases, true},
};

struct export_entries_holder {
	struct export_entry_json e[S3LVOL_MAX_EXPORTS];
	size_t           n;
};

struct exports_json {
	uint32_t           version;
	struct export_entries_holder entries;
};

static int
decode_export_entry(const struct spdk_json_val *val, void *out)
{
	return spdk_json_decode_object(val, export_entry_decoders,
				       SPDK_COUNTOF(export_entry_decoders), out);
}

static int
decode_export_entries(const struct spdk_json_val *val, void *out)
{
	struct export_entries_holder *h = out;

	return spdk_json_decode_array(val, decode_export_entry, h->e,
				      S3LVOL_MAX_EXPORTS, &h->n, sizeof(h->e[0]));
}

static const struct spdk_json_object_decoder exports_decoders[] = {
	{"version", offsetof(struct exports_json, version), spdk_json_decode_uint32, false},
	{"exports", offsetof(struct exports_json, entries), decode_export_entries,   false},
};

static int
exports_parse(struct s3lvol_lvstore *lvs, const void *json, size_t len)
{
	struct exports_json j = {0};
	struct spdk_json_val *values = NULL;
	char *copy = NULL;
	ssize_t num_values;
	size_t i;
	int rc;

	copy = malloc(len);
	if (!copy) {
		return -ENOMEM;
	}
	memcpy(copy, json, len);

	/* Counted without DECODE_IN_PLACE: that flag makes spdk_json_parse() unescape
	 * in place even when it has nowhere to put the values, so counting with it
	 * set would rewrite the buffer and the second pass would parse a document
	 * that no longer exists. Nothing in this registry is escaped today, which is
	 * the only reason it worked -- see the imports registry for what happens when
	 * something is. */
	num_values = spdk_json_parse(copy, len, NULL, 0, NULL, 0);
	if (num_values <= 0) {
		SPDK_ERRLOG("exports registry of '%s' is not valid JSON\n",
			    s3lvol_lvstore_get_name(lvs));
		rc = -EINVAL;
		goto out;
	}
	values = calloc((size_t)num_values, sizeof(*values));
	if (!values) {
		rc = -ENOMEM;
		goto out;
	}
	num_values = spdk_json_parse(copy, len, values, (size_t)num_values, NULL,
				     SPDK_JSON_PARSE_FLAG_DECODE_IN_PLACE);
	if (num_values <= 0) {
		SPDK_ERRLOG("exports registry of '%s' did not parse on the second "
			    "pass (%zd)\n", s3lvol_lvstore_get_name(lvs), num_values);
		rc = -EINVAL;
		goto out;
	}
	if (spdk_json_decode_object(values, exports_decoders,
				    SPDK_COUNTOF(exports_decoders), &j) != 0) {
		SPDK_ERRLOG("exports registry of '%s' could not be decoded\n",
			    s3lvol_lvstore_get_name(lvs));
		rc = -EINVAL;
		goto out;
	}

	rc = 0;
	for (i = 0; i < j.entries.n; i++) {
		struct export_entry_json *e = &j.entries.e[i];
		struct s3lvol_export *exp;

		exp = calloc(1, sizeof(*exp));
		if (!exp) {
			rc = -ENOMEM;
			break;
		}
		exp->lvs      = lvs;
		exp->blob_id    = e->blob_id;
		exp->expires_at = e->expires_at;
		exp->generation = e->generation;
		exp->lease_aware = e->lease_aware;
		exp->layout     = strcmp(e->layout, S3_EXPORT_LAYOUT_REF_STR) == 0 ?
				  S3_EXPORT_LAYOUT_REF : S3_EXPORT_LAYOUT_DENSE;
		snprintf(exp->uuid_str, sizeof(exp->uuid_str), "%s", e->export_uuid);
		snprintf(exp->snapshot, sizeof(exp->snapshot), "%s", e->snapshot);
		if (e->snapshot_uuid && e->snapshot_uuid[0] != '\0') {
			snprintf(exp->snapshot_uuid, sizeof(exp->snapshot_uuid), "%s",
				 e->snapshot_uuid);
		}

		TAILQ_INSERT_TAIL(&g_exports, exp, link);

		/* Watch the lease, exactly as s3lvol_export_add() does for an export
		 * created here: the importers this registry describes may still be
		 * reading, and only the lease can say so.
		 *
		 * Without this the entry sits at lease_checked == false for ever --
		 * nothing ever performs the first GET -- and s3lvol_export_pinning()
		 * takes its "assume an importer may be reading" branch on every
		 * query. Safe, but permanent: an export whose importer went away
		 * years ago still pins its snapshot, and the registry only grows.
		 * With the poller running, an absent/stale lease lets an explicit
		 * snapshot delete release the export. It does not reap the export:
		 * while the snapshot exists the manifest remains importable.
		 * expires_at is retained only for old registry compatibility. */
		s3lvol_export_lease_start(exp);

		/* And resume renewing upstream, which is an obligation to another
		 * node rather than to this one. Without it a restart quietly hands
		 * whoever this export references permission to delete the snapshot
		 * behind chunks it still names.
		 *
		 * Restarted from the stored keys rather than from the manifest, so the
		 * registry load needs no S3 GET per derived export. */
		export_src_lease_restart(exp, &e->src_leases);

		if (exp->layout == S3_EXPORT_LAYOUT_REF) {
			SPDK_NOTICELOG("lvstore '%s' still owes export %s the snapshot "
				       "'%s'\n", s3lvol_lvstore_get_name(lvs),
				       exp->uuid_str, exp->snapshot);
		}
	}

	SPDK_NOTICELOG("lvstore '%s': %zu export(s) in the registry\n",
		       s3lvol_lvstore_get_name(lvs), j.entries.n);
out:
	for (i = 0; i < j.entries.n; i++) {
		size_t k;

		free(j.entries.e[i].export_uuid);
		free(j.entries.e[i].snapshot);
		free(j.entries.e[i].snapshot_uuid);
		free(j.entries.e[i].layout);
		/* spdk_json_decode_string strdup()s into the table, so each element is
		 * its own allocation. The restart above copied what it needed. */
		for (k = 0; k < j.entries.e[i].src_leases.n; k++) {
			free(j.entries.e[i].src_leases.k[k]);
		}
	}
	free(values);
	free(copy);
	return rc;
}

/* ---- load ---- */

struct exports_load_ctx {
	struct s3lvol_lvstore *lvs;
	spdk_lvs_op_complete   cb_fn;
	void        *cb_arg;
	char  key[S3_EXPORT_KEY_MAX];
	uint64_t               size;
	char         *body;
};

static void
exports_load_done(struct exports_load_ctx *ctx, int status)
{
	spdk_lvs_op_complete cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;

	free(ctx->body);
	free(ctx);

	if (cb_fn) {
		cb_fn(cb_arg, status);
	}
}

static void
exports_load_got_body(void *cb_arg, uint64_t bytes_read, int status)
{
	struct exports_load_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to read '%s': %s\n", ctx->key,
			    spdk_strerror(-status));
		exports_load_done(ctx, status);
		return;
	}

	exports_load_done(ctx, exports_parse(ctx->lvs, ctx->body, bytes_read));
}

static void
exports_load_head_done(void *cb_arg, int status)
{
	struct exports_load_ctx *ctx = cb_arg;
	int rc;

	if (status == -ENOENT) {
		/* Nothing was ever exported. The common case, and not an error. */
		exports_load_done(ctx, 0);
		return;
	}
	if (status != 0) {
		SPDK_ERRLOG("Failed to look up '%s': %s\n", ctx->key,
			    spdk_strerror(-status));
		exports_load_done(ctx, status);
		return;
	}
	if (ctx->size == 0) {
		exports_load_done(ctx, 0);
		return;
	}
	if (ctx->size > 64u * 1024 * 1024) {
		SPDK_ERRLOG("'%s' is %" PRIu64 " bytes, which is not a plausible "
			    "exports registry\n", ctx->key, ctx->size);
		exports_load_done(ctx, -EINVAL);
		return;
	}

	ctx->body = malloc(ctx->size);
	if (!ctx->body) {
		exports_load_done(ctx, -ENOMEM);
		return;
	}

	rc = s3_get_range(s3lvol_lvstore_get_client(ctx->lvs), ctx->key, 0, ctx->size,
			  ctx->body, exports_load_got_body, ctx);
	if (rc != 0) {
		exports_load_done(ctx, rc);
	}
}

int
s3lvol_xfer_exports_load(struct s3lvol_lvstore *lvs, spdk_lvs_op_complete cb_fn,
			 void *cb_arg)
{
	struct exports_load_ctx *ctx;
	int rc;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->lvs    = lvs;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;
	exports_registry_key(s3lvol_lvstore_get_name(lvs), ctx->key, sizeof(ctx->key));

	rc = s3_head(s3lvol_lvstore_get_client(lvs), ctx->key, &ctx->size,
		     exports_load_head_done, ctx);
	if (rc != 0) {
		free(ctx);
	}
	return rc;
}

/* ---- save ---- */

struct exports_save_ctx {
	spdk_lvs_op_complete cb_fn;
	void        *cb_arg;
	char    *json;
	struct iovec       iov;
	char                 key[S3_EXPORT_KEY_MAX];
};

static void
exports_save_done(void *cb_arg, int status)
{
	struct exports_save_ctx *ctx = cb_arg;
	spdk_lvs_op_complete cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to write '%s': %s. After a restart this node will "
			    "not know it owes a snapshot to an importer, and deleting "
			    "that snapshot would break it.\n",
			    ctx->key, spdk_strerror(-status));
	}

	free(ctx->json);
	free(ctx);

	if (cb_fn) {
		cb_fn(user_arg, status);
	}
}

int
s3lvol_export_registry_save(struct s3lvol_lvstore *lvs, spdk_lvs_op_complete cb_fn,
			    void *cb_arg)
{
	struct exports_save_ctx *ctx;
	size_t len;
	int rc;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;
	exports_registry_key(s3lvol_lvstore_get_name(lvs), ctx->key, sizeof(ctx->key));

	rc = exports_serialize(lvs, &ctx->json, &len);
	if (rc != 0) {
		free(ctx);
		return rc;
	}
	ctx->iov.iov_base = ctx->json;
	ctx->iov.iov_len = len;

	rc = s3_put(s3lvol_lvstore_get_client(lvs), ctx->key, &ctx->iov, 1, false,
		    exports_save_done, ctx);
	if (rc != 0) {
		free(ctx->json);
		free(ctx);
	}
	return rc;
}
