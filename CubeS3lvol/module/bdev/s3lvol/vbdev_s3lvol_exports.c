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
#include "export_lease_term.h"

/* Same reasoning as the import cap: the registry is read into a fixed decoder
 * table, and a node with more live exports than this has a different problem.
 *
 * Raised from 64 because that was not true in practice. An export leaves the
 * registry when it is released, and a template-heavy node accumulates entries
 * whose importers are long gone but which nobody released -- crossing the cap
 * then made the whole lvstore un-attachable, live exports and all. The entries
 * do stop pinning their snapshots once their lease goes stale (see the lease
 * machinery below), so this bounds a registry nobody is maintaining rather
 * than a working set. */
#define S3LVOL_MAX_EXPORTS 256

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
	 *   lease_absent     the last completed check was a definite miss
	 *                    (404 or an empty/unreadable body), not a failed
	 *                    HEAD. export_lease_failed() never sets this: a
	 *                    bucket this node cannot reach must not look like
	 *                    "nobody imported", or the reaper would delete a
	 *                    live export's manifest during an outage.
	 *   lease_absent_at  when that miss's check was submitted. Terminal reap
	 *                    requires this >= expires_at: a 404 from before the
	 *                    deadline does not speak for an importer that PUTs
	 *                    the lease in the remaining window.
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

	/* This export was created by a build whose importers renew a lease.
	 *
	 * It is what makes "no lease object" readable. Without it the absence is
	 * ambiguous -- nobody ever imported, or an importer from before leases
	 * existed is reading right now and leaves no trace -- and the only safe
	 * reading of an ambiguous answer is the second one, which means the
	 * export pins its snapshot until somebody releases it by hand. With it,
	 * an absent lease past the deadline means exactly one thing: no importer
	 * ever came. That is the common case for an export nobody consumed, and
	 * it is the one this flag lets the delete path finish on its own.
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

/* The shortest grace period this node will apply, and the one it assumes when a
 * lease carries updated_at but no renew_s.
 *
 * Both cases and the reasoning behind the value live with the constant, in
 * vbdev_s3lvol.h, next to the renew floor it is tied to. The second case is a
 * newer source reading an older importer's lease: deriving a grace from the
 * remaining TTL is wrong there, because that situation is most likely *after* the
 * deadline has passed -- which is exactly when the lease matters -- and the
 * derivation would collapse to a second or two. */

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
	/* time(NULL) at s3_head, not at completion: a check that left before
	 * expires_at cannot confirm the lease is gone after it. */
	uint64_t              submitted_at;
};

/* A lease-aware reference export whose lease object is confirmed gone, after
 * the deadline. No importer can appear later -- they must write the lease
 * before they can read -- so watching it, and keeping the registry entry, is
 * a leak. Distinct from lease_updated_at == 0: that is also the fallback
 * after S3LVOL_LEASE_MAX_FAILURES, which must not reap. The miss itself has
 * to post-date the deadline; see export_lease_miss_confirms_gone(). */
static bool
export_lease_is_terminal(const struct s3lvol_export *exp)
{
	return exp->layout == S3_EXPORT_LAYOUT_REF
	       && exp->lease_aware
	       && export_lease_miss_confirms_gone(exp->lease_absent,
						   exp->lease_absent_at,
						   exp->expires_at,
						   (uint64_t)time(NULL));
}

static void
export_lease_note_absent(struct s3lvol_export *exp, uint64_t submitted_at)
{
	exp->lease_checked = true;
	exp->lease_absent = true;
	exp->lease_absent_at = submitted_at;
	exp->lease_updated_at = 0;
	exp->lease_failures = 0;
	exp->lease_renew_s = 0;
}

static void
export_lease_stop_if_terminal(struct s3lvol_export *exp)
{
	if (!export_lease_is_terminal(exp) || !exp->lease_poller) {
		return;
	}
	SPDK_NOTICELOG("lease of export %s is absent past the deadline; "
		       "stopping the watch\n", exp->uuid_str);
	s3lvol_export_lease_stop(exp);
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
			export_lease_note_absent(exp, ctx->submitted_at);
		} else {
			export_lease_failed(exp, status);
		}
		export_lease_get_done(ctx);
		export_lease_stop_if_terminal(exp);
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
	if (updated_at == 0) {
		export_lease_note_absent(exp, ctx->submitted_at);
		export_lease_get_done(ctx);
		export_lease_stop_if_terminal(exp);
		return;
	}

	exp->lease_checked = true;
	exp->lease_updated_at = updated_at;
	exp->lease_renew_s = (updated_at != 0) ? renew_s : 0;
	exp->lease_failures = 0;
	exp->lease_absent = false;
	exp->lease_absent_at = 0;

	export_lease_get_done(ctx);
	export_lease_stop_if_terminal(exp);
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
		export_lease_stop_if_terminal(exp);
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
		export_lease_note_absent(exp, ctx->submitted_at);
		export_lease_get_done(ctx);
		export_lease_stop_if_terminal(exp);
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

	/* Deadline passed and a check submitted at or after it missed: further
	 * HEADs cannot change that, and they are the 404 storm a restart used to
	 * amplify. The reaper collects the entry; this just stops asking. A miss
	 * from before the deadline is not enough -- an importer may still PUT. */
	if (export_lease_is_terminal(exp)) {
		export_lease_stop_if_terminal(exp);
		return SPDK_POLLER_IDLE;
	}

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
	uint64_t remaining, interval;

	if (exp->layout != S3_EXPORT_LAYOUT_REF || exp->lease_poller) {
		return;
	}

	/* Poll at the cadence the importer is expected to renew at, so a stopped
	 * renewer is noticed within one grace period. The importer's interval is
	 * max(S3LVOL_LEASE_RENEW_MIN_SEC, remaining_ttl/3) computed at import
	 * time; this is the same formula against this node's clock, which is
	 * close enough -- and the grace period is 3x the *importer's* reported
	 * interval, not this poll interval, so the safety margin does not depend
	 * on the two clocks agreeing exactly.
	 *
	 * The floor has to be the same one, or the mirror stops being a mirror:
	 * without it an export past its deadline polls every second for the life
	 * of the import, which is a GET per second per export and buys nothing --
	 * the lease it is reading cannot go stale any sooner than the grace period
	 * allows. */
	remaining = (exp->expires_at > (uint64_t)time(NULL))
		    ? (exp->expires_at - (uint64_t)time(NULL)) : 0;
	interval = remaining / 3;
	if (interval < S3LVOL_LEASE_RENEW_MIN_SEC) {
		interval = S3LVOL_LEASE_RENEW_MIN_SEC;
	}

	exp->lease_poller = SPDK_POLLER_REGISTER(export_lease_renew, exp,
						 interval * SPDK_SEC_TO_USEC);
	if (!exp->lease_poller) {
		SPDK_WARNLOG("Could not start the lease poller for export %s\n",
			     exp->uuid_str);
		return;
	}

	SPDK_NOTICELOG("watching lease of export %s every %" PRIu64
		       " second(s)\n", exp->uuid_str, interval);

	/* And check once now, rather than at the end of the first interval.
	 *
	 * The interval is a third of the TTL, which is twenty minutes at the
	 * default hour -- so without this, "has anybody imported this" stays
	 * unanswered for twenty minutes after the export was published, and every
	 * delete in that window is held on the possibility of an importer. The
	 * first answer is the one that matters most: an export nobody consumed can
	 * only be recognised by its lease being absent. */
	export_lease_renew(exp);
}

/* One export's verdict on one snapshot. Split out of s3lvol_export_pinning()
 * because two callers need different amounts of detail from the same decision:
 * the delete path only asks "may I delete this now", while the pending-delete
 * poller also has to know *why* the answer is yes.
 *
 * That distinction is the whole point. A lease that has gone stale is positive
 * evidence -- an importer wrote it and stopped renewing, so nobody is reading --
 * and a delete on that basis is safe to carry out unattended. An export with no
 * lease object at all is the legacy case, where the only signal is a TTL that
 * lapses on its own whether or not somebody is still reading; that one has to
 * stay a decision a human (or an explicit retry) makes, until GC treats a
 * reference manifest as live. Collapsing both into "not pinned" is what would
 * turn expiry into the automatic path to deleting data underneath a reader. */
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

	/* No lease object. Two very different situations, and the flag is what
	 * tells them apart.
	 *
	 * On a lease-aware export any importer would have written one, so an
	 * absent lease means nobody ever imported -- but only once the deadline
	 * has passed. Before that, the export is a standing promise to an
	 * importer the control plane may not have started yet: the lease is
	 * written when the esnap clone exists, so an import that is under way has
	 * not written one either, and deleting the snapshot on the strength of
	 * "no lease" would break exactly the importer the TTL exists to admit.
	 *
	 * Past the deadline, STALE only if the miss itself was observed on a
	 * check submitted at or after expires_at. A 404 from before the
	 * deadline is not evidence that nobody imported: the control plane can
	 * still PUT a lease in the remaining window, and the reaper must not
	 * drop that import's manifest. Until a post-deadline check misses,
	 * keep LEASE -- refuse the unattended delete.
	 *
	 * Without the flag the absence stays ambiguous (an importer from before
	 * leases existed reads without writing one), so it remains LEGACY: the TTL
	 * still lets a delete through, but never one nobody asked for twice. */
	if (exp->lease_updated_at == 0) {
		if (exp->lease_aware) {
			if (exp->lease_absent) {
				return export_lease_miss_confirms_gone(
					       true, exp->lease_absent_at,
					       exp->expires_at,
					       (uint64_t)time(NULL))
				       ? S3LVOL_EXPORT_PIN_STALE
				       : S3LVOL_EXPORT_PIN_LEASE;
			}
			return s3lvol_export_is_expired(exp)
			       ? S3LVOL_EXPORT_PIN_STALE
			       : S3LVOL_EXPORT_PIN_LEASE;
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

		if (exp->lvs != lvs || strcmp(exp->snapshot, snapshot_name) != 0) {
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
		if (exp->lvs != lvs || strcmp(exp->snapshot, snapshot_name) != 0) {
			continue;
		}

		switch (export_pin_verdict(exp)) {
		case S3LVOL_EXPORT_PIN_LEASE:
			/* An importer is reading, or may be. */
			return exp;
		case S3LVOL_EXPORT_PIN_LEGACY:
			/* No lease: the TTL decides, as it always has. */
			if (!s3lvol_export_is_expired(exp)) {
				return exp;
			}
			continue;
		case S3LVOL_EXPORT_PIN_STALE:
			/* Nobody has renewed in three intervals. The TTL has
			 * almost certainly passed too (the lease is younger
			 * than the TTL), but even if not, nobody is reading. */
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

void
s3lvol_export_get(const struct s3lvol_export *exp, struct s3lvol_export_entry *out)
{
	struct spdk_lvol *lvol;

	out->export_uuid = exp->uuid_str;
	out->snapshot    = exp->snapshot;
	out->blob_id     = exp->blob_id;
	out->expires_at  = exp->expires_at;
	out->generation  = exp->generation;
	out->is_ref      = (exp->layout == S3_EXPORT_LAYOUT_REF);
	out->expired     = s3lvol_export_is_expired(exp);
	out->lease_aware = exp->lease_aware;
	out->lease_checked = exp->lease_checked;
	out->lease_absent = exp->lease_absent;
	out->lease_watch = exp->lease_poller != NULL;
	out->lease_updated_at = exp->lease_updated_at;
	out->lease_renew_s = exp->lease_renew_s;
	out->pin = export_pin_verdict(exp);
	out->reaping = exp->reaping;

	lvol = s3lvol_lvol_find(exp->lvs, exp->snapshot);
	out->snapshot_alive = lvol &&
			      (exp->blob_id == 0 || lvol->blob_id == exp->blob_id);
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

	TAILQ_INSERT_TAIL(&g_exports, exp, link);

	/* Both ways an export appears need the lease watched: a fresh export
	 * because the first importer may arrive any moment (and the safe
	 * direction before the first lease GET is to refuse deletes anyway),
	 * and an attach resurrecting the registry because the importers it
	 * describes may still be reading. */
	s3lvol_export_lease_start(exp);
	exports_reaper_sync();

	return exp;
}

void
s3lvol_export_forget(struct s3lvol_export *exp)
{
	s3lvol_export_lease_stop(exp);
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
 * The other terminal state is a lease-aware export nobody imported, once its
 * deadline has passed. The lease object is confirmed absent (HEAD 404), and an
 * importer has to write one before it can read, so nothing will appear later.
 * The snapshot may still be here -- a control plane that treats a refused
 * delete as success never comes back -- but the registry entry is already
 * garbage, and leaving it up is a HEAD against a missing key for the life of
 * the process, resurrected on every attach.
 *
 * They used to leave only by an explicit rcow_release_export. A control plane
 * that deletes snapshots but never releases therefore accumulates entries, and
 * that is not merely untidy: the registry is read into a fixed decoder table at
 * attach (S3LVOL_MAX_EXPORTS), so crossing it makes the whole lvstore
 * un-attachable, live exports and all.
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
 * an awkward margin. An unimported export past its deadline is noticed on the
 * next tick after the lease check, without waiting for a delete that may
 * never come. */
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
		/* No manifest to delete. The export is dead twice over then, and
		 * the release stopped short of dropping the entry -- which is the
		 * right call for a release somebody asked for (it says so and
		 * changes nothing), and the wrong one here, where the entry is
		 * precisely what is being collected. Drop it directly. */
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

/* Whether the snapshot this export references still exists.
 *
 * Matched on blob id when the registry has one, so a snapshot deleted and
 * recreated under the same name reads as gone -- which it is, as far as this
 * export is concerned: the objects its manifest names went with the original
 * blob. Entries written before blob_id was recorded fall back to the name. */
static bool
export_snapshot_alive(struct s3lvol_export *exp)
{
	struct spdk_lvol *lvol = s3lvol_lvol_find(exp->lvs, exp->snapshot);

	if (!lvol) {
		return false;
	}
	if (exp->blob_id != 0 && lvol->blob_id != exp->blob_id) {
		return false;
	}
	return true;
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
			if (export_lease_is_terminal(exp)) {
				/* Even if this tick's batch is full, the watch
				 * has nothing left to learn. */
				export_lease_stop_if_terminal(exp);
			}
			if (n >= S3LVOL_EXPORT_REAP_BATCH) {
				continue;
			}
			if (export_snapshot_alive(exp) &&
			    !export_lease_is_terminal(exp)) {
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

		if (export_snapshot_alive(dead[i])) {
			SPDK_NOTICELOG("reaping export %s: nobody imported it "
				       "past the deadline\n", ctx->uuid_str);
		} else {
			SPDK_NOTICELOG("reaping export %s: the snapshot it "
				       "referenced is gone\n", ctx->uuid_str);
		}

		rc = s3lvol_export_release(ctx->lvs, ctx->uuid_str, export_reaped,
					   ctx);
		if (rc != 0) {
			/* -EBUSY: a volume in this process still reads through
			 * the export. Leave it and look again -- for a
			 * snapshot-gone export that is an esnap clone of a
			 * deleted snapshot; for an unimported one past its
			 * deadline, a same-host reader that has not gone yet. */
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
		/* Only written when true. An older build ignores the unknown
		 * field, and this build reads its absence as "not lease-aware",
		 * which is what an entry written by an older build means. */
		if (exp->lease_aware) {
			spdk_json_write_named_bool(w, "lease_aware", true);
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

struct export_entry_json {
	char *export_uuid;
	char    *snapshot;
	char    *layout;
	uint64_t blob_id;
	uint64_t expires_at;
	uint32_t generation;
	bool     lease_aware;
};

static const struct spdk_json_object_decoder export_entry_decoders[] = {
	{"export_uuid", offsetof(struct export_entry_json, export_uuid), spdk_json_decode_string, false},
	{"snapshot",  offsetof(struct export_entry_json, snapshot),    spdk_json_decode_string, false},
	{"layout",      offsetof(struct export_entry_json, layout),      spdk_json_decode_string, false},
	{"blob_id",     offsetof(struct export_entry_json, blob_id),     spdk_json_decode_uint64, true},
	{"expires_at",  offsetof(struct export_entry_json, expires_at),  spdk_json_decode_uint64, true},
	{"generation",  offsetof(struct export_entry_json, generation),  spdk_json_decode_uint32, true},
	{"lease_aware", offsetof(struct export_entry_json, lease_aware), spdk_json_decode_bool, true},
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
		 * With the poller running, a lease that is absent or stale lets the
		 * delete through. A lease-aware entry whose lease is a confirmed
		 * miss past the deadline is then reaped -- releasing the export
		 * is what takes the entry out of the registry, whether that
		 * release is asked for or collected.
		 *
		 * Deliberately *not* a filter on expires_at. That stamp is written
		 * once at export creation and never refreshed, while the importer
		 * renews its lease past the manifest TTL for as long as it is
		 * reading -- s3lvol_export_pinning() ignores expires_at whenever a
		 * fresh lease exists for precisely that reason. Dropping entries by
		 * TTL here would forget a live importer's pin, and a later delete of
		 * that snapshot would leave it reading holes. */
		s3lvol_export_lease_start(exp);

		if (exp->layout == S3_EXPORT_LAYOUT_REF) {
			SPDK_NOTICELOG("lvstore '%s' still owes export %s the snapshot "
				       "'%s'%s\n", s3lvol_lvstore_get_name(lvs),
				       exp->uuid_str, exp->snapshot,
				       s3lvol_export_is_expired(exp) ?
				       " (deadline passed; the lease decides)" : "");
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
