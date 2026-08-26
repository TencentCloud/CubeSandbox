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
 *   === Why it also has to expire ===
 *
 *   The other half of the obligation is the escape from it. An importer that
 *   never arrives, or that died between importing and inflating, would otherwise
 *   pin a snapshot forever, and this node cannot tell that case apart from an
 *   importer that is merely slow. So an export carries a deadline, an importer
 *   renews it while it still needs it, and once it lapses this node is free.
 *
 *   Deleting a pinned snapshot before the deadline is not refused outright: the
 *   objects are copied into the export's own prefix first, which turns the
 *   reference into a self-contained copy and leaves the importer working. That
 *   part lives in the delete path, and this file is what tells it that there is
 *   something to do.
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
 * table, and a node with more live exports than this has a different problem. */
#define S3LVOL_MAX_EXPORTS 64

/* One in-flight lease check. Defined below, next to the machinery that uses it;
 * named here because an export points at the check it is waiting for. */
struct export_lease_get_ctx;

struct s3lvol_export {
	struct s3lvol_lvstore     *lvs;

	char         uuid_str[SPDK_UUID_STRING_LEN];

	/* The snapshot this pins. Stored by name because that is what the delete
	 * path has in hand; blob_id is carried alongside so a rename cannot silently
	 * detach an export from what it actually references. */
	char   snapshot[SPDK_LVOL_NAME_MAX];
	uint64_t    blob_id;

	enum s3_export_layout      layout;
	uint64_t        expires_at;
	uint32_t                generation;

	/* Liveness lease cache. The importer renews
	 * <this lvstore>/meta/exports/<uuid>.lease in the source bucket; this
	 * node reads it back so the delete path can tell "an importer is still
	 * reading" from "the TTL passed". Three fields, one poller:
	 *
	 *   lease_checked    a first GET has completed (404 or not) -- until it
	 *                    has, the delete path must assume the lease may exist,
	 *                    i.e. behave as if an importer is reading.
	 *   lease_updated_at the importer's updated_at, or 0 if the object does
	 *                    not exist (no importer ever, or legacy export).
	 *   lease_renew_s    the importer's renew interval in seconds, so the
	 *                    grace period is 3x *its* cadence, not a guess.
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
	uint64_t                   lease_updated_at;
	uint32_t                   lease_renew_s;
	uint32_t                   lease_failures;
	struct spdk_poller        *lease_poller;
	struct export_lease_get_ctx *lease_fetch;

	TAILQ_ENTRY(s3lvol_export) link;
};

static TAILQ_HEAD(, s3lvol_export) g_exports = TAILQ_HEAD_INITIALIZER(g_exports);

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

bool
s3lvol_export_is_expired(const struct s3lvol_export *exp)
{
	/* A dense export pins nothing -- its objects are its own -- so it never
	 * expires. Only a reference has a deadline attached. */
	if (exp->layout != S3_EXPORT_LAYOUT_REF || exp->expires_at == 0) {
		return false;
	}
	return (uint64_t)time(NULL) >= exp->expires_at;
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
 * Legacy fallback: an export with no lease object (never imported, or created
 * before this design) keeps the old TTL semantics -- deletable once expires_at
 * passes. The defect stays bounded to pre-existing data rather than changing
 * behaviour for it.
 *
 * Until the first GET completes (lease_checked == false), the export is
 * reported as pinning. The importer may have just written its first lease and
 * the poller may not have seen it yet; refusing the delete is the safe
 * direction, and the window is normally one poll interval. A bucket that cannot
 * be read at all would make that window unbounded, so repeated failures fall
 * back to the TTL -- see S3LVOL_LEASE_MAX_FAILURES.
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
 * this many failures the export falls back to the TTL semantics it had before
 * leases existed, and says so once. */
#define S3LVOL_LEASE_MAX_FAILURES 5

/* Grace period assumed when a lease carries updated_at but no renew_s.
 *
 * That means a newer source reading an older importer's lease. Deriving the
 * interval from the remaining TTL is wrong here: this situation is most likely
 * *after* the TTL has passed, which is exactly when the lease matters, and the
 * derivation would then collapse to the one-second floor -- far too tight for
 * cross-node clock skew plus S3 latency, so a live importer would look stale.
 * A fixed, generous value is the safe reading of an ambiguous object. */
#define S3LVOL_LEASE_DEFAULT_GRACE_SEC 60

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
};

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

	if (!exp->lease_checked) {
		SPDK_ERRLOG("The lease of export %s has been unreadable %u times "
			    "(%s). Falling back to the deadline in the manifest, "
			    "which is what decided this before leases existed -- an "
			    "importer reading this export is no longer protected by "
			    "it until the bucket is reachable again.\n",
			    exp->uuid_str, exp->lease_failures,
			    spdk_strerror(-status));
	}
	exp->lease_checked = true;
	exp->lease_updated_at = 0;
}

/* The body is a tiny JSON document. Pull out updated_at and renew_s without a
 * full JSON parse: the object is written by our own renewer with a fixed shape,
 * so a scan for the two field names is all that is needed, and anything else
 * (older format, foreign writer) degrades to "no lease" -- which the legacy
 * TTL fallback then handles. */
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
			 * exactly this. No lease means the TTL decides. */
			exp->lease_checked = true;
			exp->lease_updated_at = 0;
			exp->lease_failures = 0;
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

	/* A lease that carries no updated_at is not a lease we recognise. Treat
	 * it as absent: legacy TTL fallback, which is exactly what a pre-lease
	 * object would need. */
	exp->lease_checked = true;
	exp->lease_updated_at = updated_at;
	exp->lease_renew_s = (updated_at != 0) ? renew_s : 0;
	exp->lease_failures = 0;

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
		exp->lease_checked = true;
		exp->lease_updated_at = 0;
		exp->lease_failures = 0;
		export_lease_get_done(ctx);
		return;
	}
	if (status != 0) {
		export_lease_failed(exp, status);
		export_lease_get_done(ctx);
		return;
	}
	/* Empty or implausible: not a lease we can read, and not an error either.
	 * The TTL decides, as it does for an export nobody ever imported. */
	if (ctx->size == 0 || ctx->size > 4096) {
		exp->lease_checked = true;
		exp->lease_updated_at = 0;
		exp->lease_failures = 0;
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
	uint64_t remaining, interval;

	if (exp->layout != S3_EXPORT_LAYOUT_REF || exp->lease_poller) {
		return;
	}

	/* Poll at the cadence the importer is expected to renew at, so a stopped
	 * renewer is noticed within one grace period. The importer's interval is
	 * max(1 s, remaining_ttl/3) computed at import time; this is the same
	 * formula against this node's clock, which is close enough -- and the
	 * grace period is 3x the *importer's* reported interval, not this poll
	 * interval, so the safety margin does not depend on the two clocks
	 * agreeing exactly. */
	remaining = (exp->expires_at > (uint64_t)time(NULL))
		    ? (exp->expires_at - (uint64_t)time(NULL)) : 0;
	interval = (remaining / 3) ? (remaining / 3) : 1;

	exp->lease_poller = SPDK_POLLER_REGISTER(export_lease_renew, exp,
						 interval * SPDK_SEC_TO_USEC);
	if (!exp->lease_poller) {
		SPDK_WARNLOG("Could not start the lease poller for export %s\n",
			     exp->uuid_str);
		return;
	}

	SPDK_NOTICELOG("watching lease of export %s every %" PRIu64
		       " second(s)\n", exp->uuid_str, interval);
}

struct s3lvol_export *
s3lvol_export_pinning(struct s3lvol_lvstore *lvs, const char *snapshot_name)
{
	struct s3lvol_export *exp;

	if (!lvs || !snapshot_name) {
		return NULL;
	}

	TAILQ_FOREACH(exp, &g_exports, link) {
		uint64_t now, grace;

		if (exp->lvs != lvs || strcmp(exp->snapshot, snapshot_name) != 0) {
			continue;
		}
		/* A dense export holds copies, so it does not pin the snapshot.
		 * Reported as "not pinning", which lets the delete proceed. */
		if (exp->layout != S3_EXPORT_LAYOUT_REF) {
			continue;
		}

		/* Lease first. Once the first GET has completed, a lease that
		 * exists decides the question on its own: fresh means an importer
		 * is still reading, stale means nobody has renewed in 3 intervals
		 * and the snapshot can go. Before that first GET, assume the
		 * lease may exist (safe direction). */
		if (exp->lease_checked) {
			if (exp->lease_updated_at != 0) {
				now = (uint64_t)time(NULL);
				grace = 3 * (uint64_t)exp->lease_renew_s;
				if (grace == 0) {
					/* A lease with no renew_s: an older
					 * importer. See the constant for why this
					 * is a fixed value and not derived from
					 * the deadline. */
					grace = S3LVOL_LEASE_DEFAULT_GRACE_SEC;
				}
				/* Guard against a lease stamped in the future --
				 * a skewed importer clock would otherwise make
				 * now - updated_at wrap and read as ancient. */
				if (exp->lease_updated_at > now ||
				    now - exp->lease_updated_at < grace) {
					return exp;
				}
				/* Stale lease: no importer has renewed. The TTL
				 * has almost certainly passed too (the lease is
				 * younger than the TTL), but even if not, nobody
				 * is reading, so the delete may proceed. */
				continue;
			}
			/* No lease object. Legacy: TTL decides, as it always
			 * has. */
			if (s3lvol_export_is_expired(exp)) {
				continue;
			}
			return exp;
		}

		/* First GET not done yet. Behave as if an importer may be
		 * reading: refuse, and let the next poll decide. */
		return exp;
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

void
s3lvol_export_get(const struct s3lvol_export *exp, struct s3lvol_export_entry *out)
{
	out->export_uuid = exp->uuid_str;
	out->snapshot    = exp->snapshot;
	out->blob_id     = exp->blob_id;
	out->expires_at  = exp->expires_at;
	out->generation  = exp->generation;
	out->is_ref      = (exp->layout == S3_EXPORT_LAYOUT_REF);
	out->expired     = s3lvol_export_is_expired(exp);
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
	snprintf(exp->uuid_str, sizeof(exp->uuid_str), "%s", m->uuid_str);
	snprintf(exp->snapshot, sizeof(exp->snapshot), "%s", snapshot_name);

	TAILQ_INSERT_TAIL(&g_exports, exp, link);

	/* Both ways an export appears need the lease watched: a fresh export
	 * because the first importer may arrive any moment (and the safe
	 * direction before the first lease GET is to refuse deletes anyway),
	 * and an attach resurrecting the registry because the importers it
	 * describes may still be reading. */
	s3lvol_export_lease_start(exp);

	return exp;
}

void
s3lvol_export_forget(struct s3lvol_export *exp)
{
	s3lvol_export_lease_stop(exp);
	TAILQ_REMOVE(&g_exports, exp, link);
	free(exp);
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
		spdk_json_write_named_uint64(w, "blob_id", exp->blob_id);
		spdk_json_write_named_string(w, "layout",
					     exp->layout == S3_EXPORT_LAYOUT_REF ?
					   S3_EXPORT_LAYOUT_REF_STR :
					     S3_EXPORT_LAYOUT_DENSE_STR);
		spdk_json_write_named_uint64(w, "expires_at", exp->expires_at);
		spdk_json_write_named_uint32(w, "generation", exp->generation);
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

struct export_entry_json {
	char *export_uuid;
	char    *snapshot;
	char    *layout;
	uint64_t blob_id;
	uint64_t expires_at;
	uint32_t generation;
};

static const struct spdk_json_object_decoder export_entry_decoders[] = {
	{"export_uuid", offsetof(struct export_entry_json, export_uuid), spdk_json_decode_string, false},
	{"snapshot",  offsetof(struct export_entry_json, snapshot),    spdk_json_decode_string, false},
	{"layout",      offsetof(struct export_entry_json, layout),      spdk_json_decode_string, false},
	{"blob_id",     offsetof(struct export_entry_json, blob_id),     spdk_json_decode_uint64, true},
	{"expires_at",  offsetof(struct export_entry_json, expires_at),  spdk_json_decode_uint64, true},
	{"generation",  offsetof(struct export_entry_json, generation),  spdk_json_decode_uint32, true},
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
		exp->layout     = strcmp(e->layout, S3_EXPORT_LAYOUT_REF_STR) == 0 ?
				  S3_EXPORT_LAYOUT_REF : S3_EXPORT_LAYOUT_DENSE;
		snprintf(exp->uuid_str, sizeof(exp->uuid_str), "%s", e->export_uuid);
		snprintf(exp->snapshot, sizeof(exp->snapshot), "%s", e->snapshot);
		TAILQ_INSERT_TAIL(&g_exports, exp, link);

		if (exp->layout == S3_EXPORT_LAYOUT_REF) {
			SPDK_NOTICELOG("lvstore '%s' still owes export %s the snapshot "
				       "'%s'%s\n", s3lvol_lvstore_get_name(lvs),
				       exp->uuid_str, exp->snapshot,
				       s3lvol_export_is_expired(exp) ?
				       " (expired, no longer pinned)" : "");
		}
	}

	SPDK_NOTICELOG("lvstore '%s': %zu export(s) in the registry\n",
		       s3lvol_lvstore_get_name(lvs), j.entries.n);
out:
	for (i = 0; i < j.entries.n; i++) {
		free(j.entries.e[i].export_uuid);
		free(j.entries.e[i].snapshot);
		free(j.entries.e[i].layout);
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
