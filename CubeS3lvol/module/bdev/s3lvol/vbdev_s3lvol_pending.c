/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Deletes that were asked for and could not be carried out yet
 *
 *   === Why an intent has to be recorded at all ===
 *
 *   `rcow_delete_lvol` on a snapshot is deferred while something still reads or
 *   mutates it: an export has a live lease or a miss younger than the source
 *   grace, it has a local esnap reader, it has more than one clone, or a
 *   decouple is copying through it. Those blockers clear without another delete
 *   request, at which point the original request should finish.
 *
 *   Without a record, that request is gone the moment the RPC answers. The
 *   caller has to remember it and come back, and nothing on this node can say
 *   which snapshots someone has already tried to remove. So a refusal that is
 *   expected to resolve records the intent here, and two things consume it:
 *   `rcow_get_lvstores` reports it (the PEND column), and the poller below
 *   completes the delete once the blocker is gone.
 *
 *   === Why the poller does not act on every kind of blocker ===
 *
 *   A lease-aware export becomes releasable when no importer renews it (the miss
 *   is older than the source grace) and no local esnap clone still reads through
 *   it. The poller then re-enters the normal delete path, which releases the
 *   export internally before destroying the snapshot. A pre-lease export is
 *   the same once a delete has been recorded: absence cannot prove an old
 *   importer has stopped, but the recorded intent is the revocation.
 *
 *   === Why the marks are keyed by uuid, and the queue is unbounded ===
 *
 *   A name is unique only inside one loaded lvstore and is reusable: delete a
 *   snapshot and create another by the same name, or unload and attach again,
 *   and a name-keyed mark would name a different object than the delete was
 *   refused for -- which the poller would then delete. A lvol uuid is generated
 *   once and never reused, so a mark can only ever mean the object it was
 *   recorded for. The lvstore uuid groups them so a teardown can drop them all.
 *
 *   The list is not capped. A cap would have to either refuse to record an
 *   intent (losing a delete the caller did ask for) or evict an older one
 *   (same, silently); a node with a lot of blocked deletes has a real backlog to
 *   report, not a reason to start forgetting. Each entry is one small
 *   allocation, and entries leave on completion or cancellation.
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/thread.h"
#include "spdk/util.h"

#include "spdk_internal/lvolstore.h"

#include "s3lvol/s3_export.h"
#include "s3lvol/s3_client.h"

#include "vbdev_s3lvol.h"
#include "vbdev_s3lvol_json.h"
#include "pending_persist_ctl.h"

/* How often the queue is re-examined.
 *
 * A deleted clone therefore takes up to this long to trigger the snapshot's
 * removal. That is the accepted latency: the delete is a background intent, the
 * caller was told it was deferred, and both querying and cancelling it are
 * immediate. Polling faster would spend blobstore lookups on a queue that is
 * almost always empty and almost never urgent. */
#define S3LVOL_PENDING_POLL_US (60ULL * SPDK_SEC_TO_USEC)

/* Deletes started per tick.
 *
 * The queue is walked in full each time, but only this many destroys are
 * launched, so a backlog that has just become deletable drains over several
 * ticks instead of arriving at blobstore and S3 as one burst. */
#define S3LVOL_PENDING_BATCH 16

struct s3lvol_pending_delete {
	struct spdk_uuid           lvs_uuid;
	struct spdk_uuid           lvol_uuid;

	/* Reporting only -- every lookup goes through the uuids. Kept because an
	 * entry has to be printable while its lvstore is not loaded, which is
	 * exactly when the names cannot be resolved. */
	char                       lvs_name[SPDK_LVS_NAME_MAX];
	char                       name[SPDK_LVOL_NAME_MAX];

	uint64_t                   enqueued_at;
	enum s3lvol_pending_reason reason;

	TAILQ_ENTRY(s3lvol_pending_delete) link;
};

static TAILQ_HEAD(, s3lvol_pending_delete) g_pending_deletes =
	TAILQ_HEAD_INITIALIZER(g_pending_deletes);

struct pending_persist {
	struct spdk_uuid           lvs_uuid;
	struct pending_persist_ctl ctl;
	/* Unload dropped the marks while a PUT was still in flight. The
	 * completion writes an empty registry and then frees this, instead of
	 * freeing under the PUT or letting that completion run against a later
	 * attach of the same uuid. */
	bool                       dying;
	struct s3_client          *client;
	char                       key[S3_EXPORT_KEY_MAX];
	TAILQ_ENTRY(pending_persist) link;
};

static TAILQ_HEAD(, pending_persist) g_pending_persist =
	TAILQ_HEAD_INITIALIZER(g_pending_persist);

static struct spdk_poller *g_pending_poller;

static int pending_poll(void *arg);
static void pending_persist(const struct spdk_uuid *lvs_uuid);
static struct pending_persist *pending_persist_find(const struct spdk_uuid *lvs_uuid);
static void pending_persist_drop(struct pending_persist *p);

/* ==========================================================================
 * Reasons
 * ========================================================================== */

const char *
s3lvol_pending_reason_str(enum s3lvol_pending_reason reason)
{
	switch (reason) {
	case S3LVOL_PENDING_EXPORT:
		return "export";
	case S3LVOL_PENDING_EXPORT_LEGACY:
		return "export_legacy";
	case S3LVOL_PENDING_EXPORT_INFLIGHT:
		return "export_inflight";
	case S3LVOL_PENDING_CLONE_COUNT:
		return "clone_count";
	case S3LVOL_PENDING_DECOUPLE:
		return "decouple";
	case S3LVOL_PENDING_FAILED:
		return "failed";
	default:
		return "unknown";
	}
}

/* Whether the poller may finish this delete on its own.
 *
 * Only blockers that clear without another lifecycle decision. A live lease
 * going stale is one; a pre-lease export is another, because the recorded
 * snapshot delete is itself the revocation. A destroy that already failed
 * asynchronously is excluded.
 *
 * This decides how the *intent* is reported. Whether a delete actually goes
 * ahead is decided again, against the live pin state, in
 * pending_lvol_deletable(). */
static bool
pending_reason_auto(enum s3lvol_pending_reason reason)
{
	return reason == S3LVOL_PENDING_EXPORT ||
	       reason == S3LVOL_PENDING_EXPORT_LEGACY ||
	       reason == S3LVOL_PENDING_EXPORT_INFLIGHT ||
	       reason == S3LVOL_PENDING_CLONE_COUNT ||
	       reason == S3LVOL_PENDING_DECOUPLE;
}

/* ==========================================================================
 * The queue
 * ========================================================================== */

static struct s3lvol_pending_delete *
pending_delete_find(const struct spdk_uuid *lvs_uuid,
		    const struct spdk_uuid *lvol_uuid)
{
	struct s3lvol_pending_delete *pd;

	TAILQ_FOREACH(pd, &g_pending_deletes, link) {
		if (spdk_uuid_compare(&pd->lvs_uuid, lvs_uuid) == 0 &&
		    spdk_uuid_compare(&pd->lvol_uuid, lvol_uuid) == 0) {
			return pd;
		}
	}
	return NULL;
}

/* The poller exists only while there is something to poll for.
 *
 * Registered on the first entry and unregistered on the last, so an idle node
 * runs no timer at all. Both are called from paths that already run on the
 * thread the RPCs run on, which is the thread the destroys have to be issued
 * from. */
static void
pending_poller_sync(void)
{
	bool want = !TAILQ_EMPTY(&g_pending_deletes);

	if (want && !g_pending_poller) {
		g_pending_poller = SPDK_POLLER_REGISTER(pending_poll, NULL,
						       S3LVOL_PENDING_POLL_US);
		if (!g_pending_poller) {
			SPDK_WARNLOG("could not start the pending-delete poller; "
				     "queued deletes will only complete when "
				     "retried explicitly\n");
		}
	} else if (!want && g_pending_poller) {
		spdk_poller_unregister(&g_pending_poller);
		g_pending_poller = NULL;
	}
}

void
s3lvol_snapshot_pending_set(const struct spdk_uuid *lvs_uuid,
			    const struct spdk_uuid *lvol_uuid,
			    const char *lvs_name, const char *name,
			    enum s3lvol_pending_reason reason)
{
	struct s3lvol_pending_delete *pd;

	if (!lvs_uuid || !lvol_uuid) {
		return;
	}

	pd = pending_delete_find(lvs_uuid, lvol_uuid);
	if (pd) {
		/* Asking again is not a second intent, but the blocker may have
		 * changed between the two attempts -- and it is the current one
		 * that decides whether the poller may finish the job. */
		if (pd->reason != reason) {
			pd->reason = reason;
			pending_persist(lvs_uuid);
		}
		return;
	}

	pd = calloc(1, sizeof(*pd));
	if (!pd) {
		SPDK_ERRLOG("failed to allocate pending-delete mark for '%s'\n",
			    name ? name : "(unnamed)");
		return;
	}
	spdk_uuid_copy(&pd->lvs_uuid, lvs_uuid);
	spdk_uuid_copy(&pd->lvol_uuid, lvol_uuid);
	snprintf(pd->lvs_name, sizeof(pd->lvs_name), "%s", lvs_name ? lvs_name : "");
	snprintf(pd->name, sizeof(pd->name), "%s", name ? name : "");
	pd->enqueued_at = (uint64_t)time(NULL);
	pd->reason = reason;
	TAILQ_INSERT_TAIL(&g_pending_deletes, pd, link);

	SPDK_NOTICELOG("delete of '%s' is pending: %s%s\n", pd->name,
		       s3lvol_pending_reason_str(reason),
		       pending_reason_auto(reason)
		       ? " (will complete once the blocker clears)"
		       : " (needs an explicit retry)");

	pending_poller_sync();
	pending_persist(lvs_uuid);
}

bool
s3lvol_snapshot_pending_test(const struct spdk_uuid *lvs_uuid,
			     const struct spdk_uuid *lvol_uuid)
{
	if (!lvs_uuid || !lvol_uuid) {
		return false;
	}
	return pending_delete_find(lvs_uuid, lvol_uuid) != NULL;
}

bool
s3lvol_snapshot_pending_deferred(const struct spdk_uuid *lvs_uuid,
				 const struct spdk_uuid *lvol_uuid)
{
	struct s3lvol_pending_delete *pd;

	if (!lvs_uuid || !lvol_uuid) {
		return false;
	}
	pd = pending_delete_find(lvs_uuid, lvol_uuid);
	return pd && pending_reason_auto(pd->reason);
}

void
s3lvol_snapshot_pending_clear(const struct spdk_uuid *lvs_uuid,
			      const struct spdk_uuid *lvol_uuid)
{
	struct s3lvol_pending_delete *pd;

	if (!lvs_uuid || !lvol_uuid) {
		return;
	}
	pd = pending_delete_find(lvs_uuid, lvol_uuid);
	if (pd) {
		TAILQ_REMOVE(&g_pending_deletes, pd, link);
		free(pd);
		pending_poller_sync();
		pending_persist(lvs_uuid);
	}
}

/* Every mark belonging to one lvstore, dropped in one go.
 *
 * Called from the unload / destroy / free paths: past a teardown the marks name
 * lvols that no longer exist, and an lvstore that is attached again can hand the
 * same names to different objects. Marks that outlive their lvstore are how the
 * poller could end up deleting something nobody asked it to. */
void
s3lvol_snapshot_pending_clear_lvs(const struct spdk_uuid *lvs_uuid)
{
	struct s3lvol_pending_delete *pd, *tmp;

	if (!lvs_uuid) {
		return;
	}
	TAILQ_FOREACH_SAFE(pd, &g_pending_deletes, link, tmp) {
		if (spdk_uuid_compare(&pd->lvs_uuid, lvs_uuid) == 0) {
			TAILQ_REMOVE(&g_pending_deletes, pd, link);
			free(pd);
		}
	}
	pending_poller_sync();

	{
		struct pending_persist *p, *ptmp;

		TAILQ_FOREACH_SAFE(p, &g_pending_persist, link, ptmp) {
			if (spdk_uuid_compare(&p->lvs_uuid, lvs_uuid) != 0) {
				continue;
			}
			if (p->ctl.in_flight || p->ctl.dirty) {
				/* A PUT is still using this entry. Keep it so the
				 * completion cannot run against a later attach, and
				 * ask for one more write of the now-empty queue. */
				p->dying = true;
				p->ctl.dirty = true;
				if (!p->ctl.in_flight) {
					pending_persist(lvs_uuid);
				}
			} else {
				pending_persist_drop(p);
			}
		}
	}
}

int
s3lvol_pending_foreach(s3lvol_pending_cb cb, void *cb_arg)
{
	struct s3lvol_pending_delete *pd, *tmp;
	unsigned n = 0;

	if (!cb) {
		return -EINVAL;
	}

	/* _SAFE so a callback that cancels the entry it is looking at -- which is
	 * the natural way to write "drop everything matching X" -- does not walk
	 * off a freed link. */
	TAILQ_FOREACH_SAFE(pd, &g_pending_deletes, link, tmp) {
		struct s3lvol_pending_entry e = {
			.lvs_uuid    = pd->lvs_uuid,
			.lvol_uuid   = pd->lvol_uuid,
			.lvs_name    = pd->lvs_name,
			.lvol_name   = pd->name,
			.enqueued_at = pd->enqueued_at,
			.reason      = pd->reason,
			.deferred    = pending_reason_auto(pd->reason),
		};

		cb(cb_arg, &e);
		n++;
	}
	return (int)n;
}

/* ==========================================================================
 * The poller
 * ========================================================================== */

struct pending_destroy_ctx {
	struct spdk_uuid lvs_uuid;
	struct spdk_uuid lvol_uuid;
	char             name[SPDK_LVOL_NAME_MAX];
};

static void
pending_destroy_done(void *cb_arg, int lvolerrno)
{
	struct pending_destroy_ctx *ctx = cb_arg;

	if (lvolerrno == 0) {
		/* s3lvol_lvol_destroyed() has already dropped the mark; this is
		 * the line that says the *queue* is what finished it. */
		SPDK_NOTICELOG("pending delete of '%s' completed\n", ctx->name);
	} else {
		/* Keep the intent. The destroy/release path records FAILED for a
		 * non-reference error (which stops automatic retries), or refreshes
		 * the current blocker when a race returns -EBUSY. Clearing here would
		 * lose a delete the caller was already told was deferred. */
		SPDK_ERRLOG("pending delete of '%s' failed: %s. The intent remains "
			    "queued for inspection or an explicit retry.\n",
			    ctx->name, spdk_strerror(-lvolerrno));
	}
	free(ctx);
}

/* Whether this lvol can be deleted right now.
 *
 * Deliberately the same predicates, in the same order, as the refusals in
 * s3lvol_lvol_destroy(): a deferred delete has to be exactly as safe as
 * deleting on the spot would have been, and the way to guarantee that is to ask
 * the same questions rather than a summary of them. */
static bool
pending_lvol_deletable(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
		       const char **why)
{
	/* A mounted namespace is never touched from here. The delete would block
	 * in bdev unregister until the host disconnects, and the host did not ask
	 * for that -- the caller has to deactivate first. */
	if (s3lvol_active_find(lvol->name)) {
		*why = "the volume is active";
		return false;
	}
	if (s3lvol_export_inflight_pinning(lvs, lvol->name)) {
		*why = "an export is still publishing";
		return false;
	}
	if (s3lvol_snapshot_exports_have_local_readers(lvs, lvol->name)) {
		*why = "a local esnap clone still reads an export of it";
		return false;
	}

	/* A stale or missing lease, and a pre-lease export, let the pending
	 * delete proceed: the recorded intent is the revocation. */
	switch (s3lvol_export_pin_state(lvs, lvol->name)) {
	case S3LVOL_EXPORT_PIN_LEASE:
		*why = "an importer may still be reading an export of it";
		return false;
	case S3LVOL_EXPORT_PIN_LEGACY:
	case S3LVOL_EXPORT_PIN_STALE:
	case S3LVOL_EXPORT_PIN_NONE:
	default:
		break;
	}

	if (lvol->blob) {
		size_t clone_count = 0;
		int rc;

		rc = spdk_blob_get_clones(lvol->lvol_store->blobstore,
					  lvol->blob_id, NULL, &clone_count);
		if (rc != 0 && rc != -ENOMEM) {
			*why = "the clone count is unreadable";
			return false;
		}
		if (clone_count > 1) {
			*why = "it still has more than one clone";
			return false;
		}
	}
	if (lvol->action_in_progress) {
		*why = "an operation is in progress on it";
		return false;
	}
	return true;
}

static int
pending_poll(void *arg)
{
	struct pending_destroy_ctx *batch[S3LVOL_PENDING_BATCH];
	struct spdk_lvol *lvols[S3LVOL_PENDING_BATCH];
	struct s3lvol_lvstore *lvs;
	unsigned n = 0;
	unsigned i;

	/* Pass one collects, pass two destroys. The destroy path unregisters a
	 * bdev and frees the lvol from its completion, which can reach back into
	 * the very list being walked here -- so nothing is destroyed while
	 * iterating. */
	for (lvs = s3lvol_lvstore_first(); lvs && n < S3LVOL_PENDING_BATCH;
	     lvs = s3lvol_lvstore_next(lvs)) {
		struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(lvs);
		struct spdk_lvol *lvol;

		if (!store) {
			continue;	/* still loading */
		}

		TAILQ_FOREACH(lvol, &store->lvols, link) {
			struct s3lvol_pending_delete *pd;
			const char *why = NULL;

			if (n == S3LVOL_PENDING_BATCH) {
				break;
			}

			pd = pending_delete_find(&store->uuid, &lvol->uuid);
			if (!pd) {
				continue;
			}

			/* An export-blocked entry may have been recorded before
			 * the first lease GET answered, when it was not yet
			 * knowable whether the export has a lease at all. Once it
			 * is, correct the record -- otherwise
			 * rcow_get_pending_deletes would keep promising that an
			 * intent completes itself when it never will, or the
			 * reverse. */
			if (pd->reason == S3LVOL_PENDING_EXPORT ||
			    pd->reason == S3LVOL_PENDING_EXPORT_LEGACY) {
				enum s3lvol_pending_reason was = pd->reason;

				pd->reason =
					s3lvol_export_pin_state(lvs, lvol->name) ==
					S3LVOL_EXPORT_PIN_LEGACY
					? S3LVOL_PENDING_EXPORT_LEGACY
					: S3LVOL_PENDING_EXPORT;
				if (pd->reason != was) {
					pending_persist(&store->uuid);
				}
			}

			if (!pending_reason_auto(pd->reason)) {
				continue;
			}
			if (!pending_lvol_deletable(lvs, lvol, &why)) {
				SPDK_DEBUGLOG(vbdev_s3lvol,
					      "pending delete of '%s' still waiting: %s\n",
					      lvol->name, why);
				continue;
			}

			batch[n] = calloc(1, sizeof(*batch[n]));
			if (!batch[n]) {
				break;
			}
			spdk_uuid_copy(&batch[n]->lvs_uuid, &store->uuid);
			spdk_uuid_copy(&batch[n]->lvol_uuid, &lvol->uuid);
			snprintf(batch[n]->name, sizeof(batch[n]->name), "%s",
				 lvol->name);
			lvols[n] = lvol;
			n++;
		}
	}

	for (i = 0; i < n; i++) {
		int rc;

		SPDK_NOTICELOG("completing the pending delete of '%s'\n",
			       batch[i]->name);

		rc = s3lvol_lvol_destroy(lvols[i], pending_destroy_done, batch[i]);
		if (rc != 0) {
			/* A synchronous refusal means a blocker appeared between
			 * the check above and this call, or one the check cannot
			 * see. The destroy has re-recorded the mark itself, so the
			 * entry simply stays and the next tick tries again. */
			SPDK_DEBUGLOG(vbdev_s3lvol,
				      "pending delete of '%s' not started: %s\n",
				      batch[i]->name, spdk_strerror(-rc));
			free(batch[i]);
		}
	}

	return n ? SPDK_POLLER_BUSY : SPDK_POLLER_IDLE;
}

/* ==========================================================================
 * Persistence
 *
 * One object per lvstore, next to the exports registry it is modelled on. The
 * queue has to survive a restart for the same reason it exists at all: a delete
 * that is waiting for a clone to go away would otherwise be forgotten by the
 * next attach, and the caller was told it did not need to do anything else.
 *
 * Unlike the exports and imports registries, a failure here is never fatal.
 * Those two protect data -- forgetting an export lets a delete break a live
 * importer, forgetting an import opens a clone against nothing -- while the
 * worst case here is a leaked or forgotten *intent*: the snapshot is still
 * there, and the delete can be asked for again. So every path below logs and
 * carries on, and an lvstore whose pending registry is unreadable still
 * attaches.
 * ========================================================================== */

/* Generous, and deliberately not the queue's limit -- there is none (see the
 * header). It caps only what one *file* may describe, so a corrupt or absurd
 * object cannot make the decoder allocate without bound. Reaching it means the
 * tail of the queue is not restored; the deletes it names have to be reissued,
 * which is the same cost as the crash window. */
#define S3LVOL_PENDING_MAX_PERSISTED 4096

#define S3LVOL_PENDING_VERSION 1

static void
pending_registry_key(const char *prefix, char *out, size_t out_len)
{
	snprintf(out, out_len, "%s/meta/pending-deletes.json", prefix);
}

static struct s3lvol_lvstore *
pending_lvs_by_uuid(const struct spdk_uuid *lvs_uuid)
{
	struct s3lvol_lvstore *lvs;

	for (lvs = s3lvol_lvstore_first(); lvs; lvs = s3lvol_lvstore_next(lvs)) {
		struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(lvs);

		/* Uuid, not name: unload can be followed by a same-name attach
		 * with a new blobstore uuid, and a late GET of the old registry
		 * must not populate that wrapper. */
		if (store && spdk_uuid_compare(&store->uuid, lvs_uuid) == 0) {
			return lvs;
		}
	}
	return NULL;
}

static enum s3lvol_pending_reason
pending_reason_parse(const char *s)
{
	if (!s) {
		return S3LVOL_PENDING_FAILED;
	}
	if (strcmp(s, "export") == 0) {
		return S3LVOL_PENDING_EXPORT;
	}
	if (strcmp(s, "export_inflight") == 0) {
		return S3LVOL_PENDING_EXPORT_INFLIGHT;
	}
	if (strcmp(s, "clone_count") == 0) {
		return S3LVOL_PENDING_CLONE_COUNT;
	}
	if (strcmp(s, "decouple") == 0) {
		return S3LVOL_PENDING_DECOUPLE;
	}
	/* Including "failed" and anything a newer version wrote: an unrecognised
	 * blocker is one this build cannot reason about, and treating it as
	 * needing an explicit retry is the direction that cannot delete
	 * something by accident. */
	return S3LVOL_PENDING_FAILED;
}

static int
pending_serialize(struct s3lvol_lvstore *lvs, const struct spdk_uuid *lvs_uuid,
		  char **out, size_t *out_len)
{
	struct s3lvol_json_buf buf = {0};
	struct spdk_json_write_ctx *w;
	struct s3lvol_pending_delete *pd;
	int rc;

	w = spdk_json_write_begin(s3lvol_json_buf_append, &buf, 0);
	if (!w) {
		return -ENOMEM;
	}

	spdk_json_write_object_begin(w);
	spdk_json_write_named_uint32(w, "version", S3LVOL_PENDING_VERSION);
	spdk_json_write_named_array_begin(w, "pending_deletes");

	TAILQ_FOREACH(pd, &g_pending_deletes, link) {
		char uuid_str[SPDK_UUID_STRING_LEN];

		if (spdk_uuid_compare(&pd->lvs_uuid, lvs_uuid) != 0) {
			continue;
		}

		spdk_uuid_fmt_lower(uuid_str, sizeof(uuid_str), &pd->lvol_uuid);

		spdk_json_write_object_begin(w);
		/* The uuid is the identity; the name is written so an entry whose
		 * lvstore is not loaded can still be reported. */
		spdk_json_write_named_string(w, "lvol_uuid", uuid_str);
		spdk_json_write_named_string(w, "lvol_name", pd->name);
		spdk_json_write_named_string(w, "lvs_name",
					     lvs ? s3lvol_lvstore_get_name(lvs) : "");
		spdk_json_write_named_uint64(w, "enqueued_at", pd->enqueued_at);
		spdk_json_write_named_string(w, "reason",
					     s3lvol_pending_reason_str(pd->reason));
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

/* ---- save ---- */

struct pending_save_ctx {
	char              *json;
	struct iovec       iov;
	char               key[S3_EXPORT_KEY_MAX];
	struct spdk_uuid   lvs_uuid;
};

static struct pending_persist *
pending_persist_find(const struct spdk_uuid *lvs_uuid)
{
	struct pending_persist *p;

	TAILQ_FOREACH(p, &g_pending_persist, link) {
		if (spdk_uuid_compare(&p->lvs_uuid, lvs_uuid) == 0) {
			return p;
		}
	}
	return NULL;
}

static struct pending_persist *
pending_persist_get(const struct spdk_uuid *lvs_uuid)
{
	struct pending_persist *p = pending_persist_find(lvs_uuid);

	if (p) {
		return p;
	}
	p = calloc(1, sizeof(*p));
	if (!p) {
		return NULL;
	}
	spdk_uuid_copy(&p->lvs_uuid, lvs_uuid);
	TAILQ_INSERT_TAIL(&g_pending_persist, p, link);
	return p;
}

static void
pending_persist_drop(struct pending_persist *p)
{
	TAILQ_REMOVE(&g_pending_persist, p, link);
	s3_client_put(p->client);
	free(p);
}

static void
pending_persist_bind(struct pending_persist *p, struct s3lvol_lvstore *lvs)
{
	if (!p->client) {
		p->client = s3lvol_lvstore_get_client(lvs);
		s3_client_get(p->client);
	}
	if (p->key[0] == '\0') {
		pending_registry_key(s3lvol_lvstore_get_name(lvs), p->key,
				      sizeof(p->key));
	}
}

static void
pending_save_done(void *cb_arg, int status)
{
	struct pending_save_ctx *ctx = cb_arg;
	struct pending_persist *p;

	if (status != 0) {
		SPDK_WARNLOG("Failed to write '%s': %s. Deletes queued since the "
			     "last successful write will be forgotten if this node "
			     "restarts, and have to be asked for again.\n",
			     ctx->key, spdk_strerror(-status));
	}

	p = pending_persist_find(&ctx->lvs_uuid);
	free(ctx->json);
	free(ctx);

	if (p && pending_persist_end(&p->ctl)) {
		pending_persist(&p->lvs_uuid);
		return;
	}
	if (p && p->dying) {
		pending_persist_drop(p);
	}
}

/* Answer first, persist after.
 *
 * The RPC has already told the caller the delete was accepted, and waiting for
 * S3 before answering would put a round trip on the one path that is supposed to
 * be a registry write. The window that opens is "enqueued, crashed before the
 * PUT landed", whose cost is one delete the caller has to reissue -- the same
 * window the exports registry accepts, and there the stakes are higher.
 *
 * One PUT per lvstore. A later mutation while a PUT is in flight marks the
 * registry dirty; completion serialises the *current* marks, so a slow set(A)
 * cannot land after cancel(A). */
static void
pending_persist(const struct spdk_uuid *lvs_uuid)
{
	struct s3lvol_lvstore *lvs = pending_lvs_by_uuid(lvs_uuid);
	struct pending_persist *p;
	struct pending_save_ctx *ctx;
	struct s3_client *client;
	size_t len;
	int rc;

	p = lvs ? pending_persist_get(lvs_uuid) : pending_persist_find(lvs_uuid);
	if (!p) {
		return;
	}

	if (!lvs) {
		/* Unload dropped the marks. A PUT still in flight has to land an
		 * empty registry so a later attach does not restore them; anything
		 * else has nothing to write. */
		if (!p->dying || !p->client || p->key[0] == '\0') {
			p->ctl.in_flight = false;
			p->ctl.dirty = false;
			if (p->dying) {
				pending_persist_drop(p);
			}
			return;
		}
	} else {
		if (p->dying) {
			p->dying = false;
		}
		pending_persist_bind(p, lvs);
	}

	if (!pending_persist_begin(&p->ctl)) {
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		p->ctl.in_flight = false;
		return;
	}
	spdk_uuid_copy(&ctx->lvs_uuid, lvs_uuid);
	snprintf(ctx->key, sizeof(ctx->key), "%s", p->key);

	rc = pending_serialize(lvs, lvs_uuid, &ctx->json, &len);
	if (rc != 0) {
		free(ctx);
		if (pending_persist_end(&p->ctl)) {
			pending_persist(lvs_uuid);
		}
		return;
	}
	ctx->iov.iov_base = ctx->json;
	ctx->iov.iov_len = len;

	client = lvs ? s3lvol_lvstore_get_client(lvs) : p->client;
	rc = s3_put(client, ctx->key, &ctx->iov, 1, false, pending_save_done, ctx);
	if (rc != 0) {
		SPDK_WARNLOG("Could not submit the pending-delete registry write "
			     "for '%s': %s\n", ctx->key, spdk_strerror(-rc));
		free(ctx->json);
		free(ctx);
		if (pending_persist_end(&p->ctl)) {
			pending_persist(lvs_uuid);
		}
	}
}

/* ---- load ---- */

struct pending_entry_json {
	char    *lvol_uuid;
	char    *lvol_name;
	char    *lvs_name;
	char    *reason;
	uint64_t enqueued_at;
};

static const struct spdk_json_object_decoder pending_entry_decoders[] = {
	{"lvol_uuid", offsetof(struct pending_entry_json, lvol_uuid), spdk_json_decode_string, false},
	{"lvol_name", offsetof(struct pending_entry_json, lvol_name), spdk_json_decode_string, true},
	{"lvs_name", offsetof(struct pending_entry_json, lvs_name), spdk_json_decode_string, true},
	{"reason", offsetof(struct pending_entry_json, reason), spdk_json_decode_string, true},
	{"enqueued_at", offsetof(struct pending_entry_json, enqueued_at), spdk_json_decode_uint64, true},
};

struct pending_entries_holder {
	struct pending_entry_json e[S3LVOL_PENDING_MAX_PERSISTED];
	size_t                    n;
};

struct pending_json {
	uint32_t                      version;
	struct pending_entries_holder entries;
};

static int
decode_pending_entry(const struct spdk_json_val *val, void *out)
{
	return spdk_json_decode_object(val, pending_entry_decoders,
				       SPDK_COUNTOF(pending_entry_decoders), out);
}

static int
decode_pending_entries(const struct spdk_json_val *val, void *out)
{
	struct pending_entries_holder *h = out;

	return spdk_json_decode_array(val, decode_pending_entry, h->e,
				      S3LVOL_PENDING_MAX_PERSISTED, &h->n,
				      sizeof(h->e[0]));
}

static const struct spdk_json_object_decoder pending_decoders[] = {
	{"version", offsetof(struct pending_json, version), spdk_json_decode_uint32, false},
	{"pending_deletes", offsetof(struct pending_json, entries), decode_pending_entries, false},
};

static int
pending_parse(struct s3lvol_lvstore *lvs, const void *json, size_t len)
{
	struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(lvs);
	const char *lvs_name = s3lvol_lvstore_get_name(lvs);
	struct pending_json j = {0};
	struct spdk_json_val *values = NULL;
	char *copy = NULL;
	ssize_t num_values;
	size_t i;
	unsigned restored = 0;
	int rc;

	if (!store) {
		return -EINVAL;
	}

	copy = malloc(len);
	if (!copy) {
		return -ENOMEM;
	}
	memcpy(copy, json, len);

	/* Counted without DECODE_IN_PLACE for the reason spelled out in the
	 * exports registry: that flag unescapes into the buffer even when there
	 * is nowhere to put the values, so the second pass would parse a
	 * document the first one had already rewritten. */
	num_values = spdk_json_parse(copy, len, NULL, 0, NULL, 0);
	if (num_values <= 0) {
		SPDK_WARNLOG("pending-delete registry of '%s' is not valid JSON; "
			     "ignoring it\n", lvs_name);
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
		SPDK_WARNLOG("pending-delete registry of '%s' did not parse on the "
			     "second pass (%zd); ignoring it\n", lvs_name, num_values);
		rc = -EINVAL;
		goto out;
	}
	if (spdk_json_decode_object(values, pending_decoders,
				    SPDK_COUNTOF(pending_decoders), &j) != 0) {
		SPDK_WARNLOG("pending-delete registry of '%s' could not be decoded; "
			     "ignoring it\n", lvs_name);
		rc = -EINVAL;
		goto out;
	}
	if (j.version != S3LVOL_PENDING_VERSION) {
		/* Ignored rather than refused. The exports registry fails the
		 * attach on an unknown version because acting without it risks
		 * data; here it would refuse to attach an otherwise healthy
		 * lvstore over a queue of intents. */
		SPDK_WARNLOG("pending-delete registry of '%s' is version %" PRIu32
			     ", this build understands %d; ignoring it. Queued "
			     "deletes have to be asked for again.\n",
			     lvs_name, j.version, S3LVOL_PENDING_VERSION);
		rc = 0;
		goto out;
	}

	for (i = 0; i < j.entries.n; i++) {
		struct pending_entry_json *e = &j.entries.e[i];
		struct s3lvol_pending_delete *pd;
		struct spdk_uuid lvol_uuid;

		if (spdk_uuid_parse(&lvol_uuid, e->lvol_uuid) != 0) {
			SPDK_WARNLOG("pending-delete registry of '%s' has an entry "
				     "with an unparseable uuid '%s'; skipping it\n",
				     lvs_name, e->lvol_uuid);
			continue;
		}
		if (pending_delete_find(&store->uuid, &lvol_uuid)) {
			continue;	/* already recorded in this run */
		}

		pd = calloc(1, sizeof(*pd));
		if (!pd) {
			break;
		}
		spdk_uuid_copy(&pd->lvs_uuid, &store->uuid);
		spdk_uuid_copy(&pd->lvol_uuid, &lvol_uuid);
		snprintf(pd->lvs_name, sizeof(pd->lvs_name), "%s", lvs_name);
		snprintf(pd->name, sizeof(pd->name), "%s",
			 e->lvol_name ? e->lvol_name : "");
		pd->enqueued_at = e->enqueued_at;
		pd->reason = pending_reason_parse(e->reason);
		TAILQ_INSERT_TAIL(&g_pending_deletes, pd, link);
		restored++;
	}

	if (restored) {
		SPDK_NOTICELOG("lvstore '%s' has %u delete%s still pending\n",
			       lvs_name, restored, restored == 1 ? "" : "s");
		pending_poller_sync();
	}
	rc = 0;

out:
	for (i = 0; i < j.entries.n; i++) {
		free(j.entries.e[i].lvol_uuid);
		free(j.entries.e[i].lvol_name);
		free(j.entries.e[i].lvs_name);
		free(j.entries.e[i].reason);
	}
	free(values);
	free(copy);
	return rc;
}

enum pending_load_hold_stage {
	PENDING_LOAD_HOLD_NONE = 0,
	PENDING_LOAD_HOLD_HEAD,
	PENDING_LOAD_HOLD_GET,
};

struct pending_load_ctx {
	struct s3_client          *client;
	struct spdk_uuid           lvs_uuid;
	char                       lvs_name[SPDK_LVS_NAME_MAX];
	char                      *body;
	uint64_t                   size;
	char                       key[S3_EXPORT_KEY_MAX];
	int                        parked_status;
	uint64_t                   parked_bytes;
	bool                       parked_get;
	bool                       parked;
	TAILQ_ENTRY(pending_load_ctx) park_link;
};

static enum pending_load_hold_stage g_pending_load_hold;
static TAILQ_HEAD(, pending_load_ctx) g_pending_load_parked =
	TAILQ_HEAD_INITIALIZER(g_pending_load_parked);

static void pending_load_continue_head(struct pending_load_ctx *ctx, int status);
static void pending_load_continue_get(struct pending_load_ctx *ctx, int status,
				       uint64_t bytes_read);

static struct s3lvol_lvstore *
pending_load_live_lvs(const struct pending_load_ctx *ctx)
{
	return pending_lvs_by_uuid(&ctx->lvs_uuid);
}

static void
pending_load_unpark(struct pending_load_ctx *ctx)
{
	if (!ctx->parked) {
		return;
	}
	TAILQ_REMOVE(&g_pending_load_parked, ctx, park_link);
	ctx->parked = false;
}

static void
pending_load_park(struct pending_load_ctx *ctx, int status, uint64_t bytes,
		  bool is_get)
{
	ctx->parked_status = status;
	ctx->parked_bytes = bytes;
	ctx->parked_get = is_get;
	if (!ctx->parked) {
		TAILQ_INSERT_TAIL(&g_pending_load_parked, ctx, park_link);
		ctx->parked = true;
	}
	SPDK_NOTICELOG("pending-delete load %s parked for '%s'\n",
		       is_get ? "GET" : "HEAD", ctx->lvs_name);
}

static void
pending_load_done(struct pending_load_ctx *ctx, int status)
{
	if (status != 0) {
		SPDK_WARNLOG("pending-delete registry of '%s' was not loaded (%s); "
			     "queued deletes have to be asked for again\n",
			     ctx->lvs_name, spdk_strerror(-status));
	}
	pending_load_unpark(ctx);
	if (ctx->client) {
		s3_client_put(ctx->client);
	}
	free(ctx->body);
	free(ctx);
}

static void
pending_load_continue_get(struct pending_load_ctx *ctx, int status,
			    uint64_t bytes_read)
{
	struct s3lvol_lvstore *lvs;

	if (status != 0) {
		pending_load_done(ctx, status);
		return;
	}

	lvs = pending_load_live_lvs(ctx);
	if (!lvs) {
		SPDK_NOTICELOG("pending-delete load dropped for '%s': lvstore gone\n",
			       ctx->lvs_name);
		pending_load_done(ctx, 0);
		return;
	}
	pending_load_done(ctx, pending_parse(lvs, ctx->body, bytes_read));
}

static void
pending_load_got_body(void *cb_arg, uint64_t bytes_read, int status)
{
	struct pending_load_ctx *ctx = cb_arg;

	if (g_pending_load_hold == PENDING_LOAD_HOLD_GET) {
		pending_load_park(ctx, status, bytes_read, true);
		return;
	}
	pending_load_continue_get(ctx, status, bytes_read);
}

static void
pending_load_continue_head(struct pending_load_ctx *ctx, int status)
{
	struct s3lvol_lvstore *lvs;
	int rc;

	if (status == -ENOENT || (status == 0 && ctx->size == 0)) {
		pending_load_done(ctx, 0);
		return;
	}
	if (status != 0) {
		pending_load_done(ctx, status);
		return;
	}

	lvs = pending_load_live_lvs(ctx);
	if (!lvs) {
		SPDK_NOTICELOG("pending-delete load dropped after HEAD for '%s': "
			       "lvstore gone\n", ctx->lvs_name);
		pending_load_done(ctx, 0);
		return;
	}

	if (ctx->size > 64u * 1024 * 1024) {
		SPDK_WARNLOG("'%s' is %" PRIu64 " bytes, which is not a plausible "
			     "pending-delete registry; ignoring it\n",
			     ctx->key, ctx->size);
		pending_load_done(ctx, -EINVAL);
		return;
	}

	ctx->body = malloc(ctx->size);
	if (!ctx->body) {
		pending_load_done(ctx, -ENOMEM);
		return;
	}

	rc = s3_get_range(ctx->client, ctx->key, 0, ctx->size, ctx->body,
			   pending_load_got_body, ctx);
	if (rc != 0) {
		pending_load_done(ctx, rc);
	}
}

static void
pending_load_head_done(void *cb_arg, int status)
{
	struct pending_load_ctx *ctx = cb_arg;

	if (g_pending_load_hold == PENDING_LOAD_HOLD_HEAD) {
		pending_load_park(ctx, status, 0, false);
		return;
	}
	pending_load_continue_head(ctx, status);
}

int
s3lvol_pending_load_hold(const char *stage)
{
	if (!stage || stage[0] == '\0' || strcmp(stage, "none") == 0) {
		g_pending_load_hold = PENDING_LOAD_HOLD_NONE;
		return 0;
	}
	if (strcmp(stage, "head") == 0) {
		g_pending_load_hold = PENDING_LOAD_HOLD_HEAD;
		return 0;
	}
	if (strcmp(stage, "get") == 0) {
		g_pending_load_hold = PENDING_LOAD_HOLD_GET;
		return 0;
	}
	return -EINVAL;
}

const char *
s3lvol_pending_load_hold_name(void)
{
	switch (g_pending_load_hold) {
	case PENDING_LOAD_HOLD_HEAD:
		return "head";
	case PENDING_LOAD_HOLD_GET:
		return "get";
	case PENDING_LOAD_HOLD_NONE:
	default:
		return "none";
	}
}

unsigned
s3lvol_pending_load_parked_count(void)
{
	struct pending_load_ctx *ctx;
	unsigned n = 0;

	TAILQ_FOREACH(ctx, &g_pending_load_parked, park_link) {
		n++;
	}
	return n;
}

unsigned
s3lvol_pending_load_release(void)
{
	struct pending_load_ctx *ctx;
	unsigned n = 0;

	g_pending_load_hold = PENDING_LOAD_HOLD_NONE;
	while ((ctx = TAILQ_FIRST(&g_pending_load_parked)) != NULL) {
		bool is_get = ctx->parked_get;
		int status = ctx->parked_status;
		uint64_t bytes = ctx->parked_bytes;

		pending_load_unpark(ctx);
		n++;
		if (is_get) {
			pending_load_continue_get(ctx, status, bytes);
		} else {
			pending_load_continue_head(ctx, status);
		}
	}
	return n;
}

void
s3lvol_pending_load(struct s3lvol_lvstore *lvs)
{
	struct pending_load_ctx *ctx;
	struct spdk_lvol_store *store;
	struct s3_client *client;
	const char *name;
	int rc;

	if (!lvs) {
		return;
	}
	store = s3lvol_lvstore_get_lvs(lvs);
	client = s3lvol_lvstore_get_client(lvs);
	name = s3lvol_lvstore_get_name(lvs);
	if (!store || !client || !name) {
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return;
	}
	ctx->client = client;
	s3_client_get(client);
	ctx->lvs_uuid = store->uuid;
	snprintf(ctx->lvs_name, sizeof(ctx->lvs_name), "%s", name);
	pending_registry_key(name, ctx->key, sizeof(ctx->key));

	rc = s3_head(client, ctx->key, &ctx->size, pending_load_head_done, ctx);
	if (rc != 0) {
		pending_load_done(ctx, rc);
	}
}
