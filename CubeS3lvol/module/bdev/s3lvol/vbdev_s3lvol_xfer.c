/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_xfer -- moving a volume between lvstores and between nodes
 *
 *   Three operations, and one callback that is easy to overlook:
 *
 *     export   snapshot the volume, copy the snapshot into export objects,
 *              upload the manifest
 *     import   fetch the manifest, remember it, create an esnap clone whose
 *              read-only parent is that export
 *     release  delete an export's objects once nobody imports it any more
 *
 *     esnap_bs_dev_create   blobstore asking, during load, for the read-only
 *              parent of an esnap clone it just found. *Synchronously.*
 *
 *   That last one is the reason for the imports registry. blobstore persists the
 *   esnap id (here: the export uuid) inside the clone's metadata, and on every
 *   attach it hands that id back and demands a bs_dev in return, right then. It
 *   cannot be told to wait for an S3 GET -- and waiting for one on a reactor
 *   thread that has to poll for its own completion is a deadlock, not a delay.
 *   So every manifest this lvstore might be asked for is fetched *before*
 *   spdk_lvs_load_ext() runs, in one GET of one object.
 *
 *   === What is deliberately not done here ===
 *
 *   No drain, no flush, no checkpoint before an export. The reference design
 *   requires all three, because there the manifest names the source's live
 *   chunk objects and those must already be in S3. In this implementation an
 *   export is a copy read out through blobstore, so whether the source's own
 *   data currently sits in the WAL, the overlay or S3 is blobstore's business,
 *   and the export's validity does not depend on it.
 *
 *   No IO quiescing either. The snapshot is what makes the copy a consistent
 *   point in time; stopping the writer is the orchestrator's job.
 */

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/json.h"
#include "spdk/log.h"
#include "spdk/lvol.h"
#include "spdk/string.h"
#include "spdk/thread.h"
#include "spdk/util.h"
#include "spdk/uuid.h"

#include "spdk_internal/lvolstore.h"

#include "s3lvol/s3_export.h"

#include "vbdev_s3lvol.h"

/* Imports are held in memory and in one S3 object per lvstore. The cap exists
 * because that object is read into a fixed decoder table; it is far above what
 * a node would plausibly have esnap clones of, and it fails loudly. */
#define S3LVOL_MAX_IMPORTS 64

struct s3lvol_import {
	struct s3lvol_lvstore      *lvs;
	struct s3_export_manifest  *m;

	/* Client options for reaching the *source* bucket. Not part of the
	 * manifest: the manifest describes where the data is, these describe how
	 * this node talks to it. */
	bool                        path_style;
	bool                        verify_tls;

	/* Liveness lease.
	 *
	 * While this lvstore reads the export, the lease object under
	 * <src-prefix>/meta/exports/<uuid>.lease is refreshed every
	 * lease_interval_us. The source node consults it at delete time: fresh
	 * means an importer is still reading, so the snapshot behind the export
	 * must not be deleted. NULL poller means no lease -- a dense export pins
	 * nothing, so it has nothing to protect.
	 *
	 * The lease writes to the *source* bucket, so it needs its own client:
	 * s3lvol_lvstore_get_client() reaches this lvstore's bucket, which is
	 * only the same one when the import stays within a bucket. */
	struct spdk_poller          *lease_poller;
	struct s3_client            *lease_client;
	char                         lease_key[S3_EXPORT_KEY_MAX];
	uint64_t                     lease_interval_us;

	TAILQ_ENTRY(s3lvol_import)  link;
};

static TAILQ_HEAD(, s3lvol_import) g_imports = TAILQ_HEAD_INITIALIZER(g_imports);

/* Lease machinery. Declared here because import_add() starts the lease,
 * while the implementations live further down next to import_build_target(),
 * which they use. */
static void import_lease_start(struct s3lvol_import *imp);
static void import_lease_stop(struct s3lvol_import *imp);
static int import_lease_renew(void *arg);
static void import_lease_put_done(void *cb_arg, int status);
static void import_build_target(const struct s3lvol_import *imp,
				struct s3_target *out);

/* ==========================================================================
 * Registry
 * ========================================================================== */

static void
imports_registry_key(const char *prefix, char *out, size_t out_len)
{
	snprintf(out, out_len, "%s/meta/imports.json", prefix);
}

static struct s3lvol_import *
import_find(struct s3lvol_lvstore *lvs, const char *uuid_str)
{
	struct s3lvol_import *imp;

	TAILQ_FOREACH(imp, &g_imports, link) {
		if (imp->lvs == lvs && strcmp(imp->m->uuid_str, uuid_str) == 0) {
			return imp;
		}
	}
	return NULL;
}

/* By uuid alone, across every lvstore. Only the esnap callback wants this: see
 * the comment there for why the lvstore cannot be identified at that point. */
static struct s3lvol_import *
import_find_any(const char *uuid_str)
{
	struct s3lvol_import *imp;

	TAILQ_FOREACH(imp, &g_imports, link) {
		if (strcmp(imp->m->uuid_str, uuid_str) == 0) {
			return imp;
		}
	}
	return NULL;
}

struct s3lvol_import *
s3lvol_import_first(struct s3lvol_lvstore *lvs)
{
	struct s3lvol_import *imp;

	TAILQ_FOREACH(imp, &g_imports, link) {
		if (imp->lvs == lvs) {
			return imp;
		}
	}
	return NULL;
}

struct s3lvol_import *
s3lvol_import_next(struct s3lvol_import *prev)
{
	struct s3lvol_import *imp = prev;

	while ((imp = TAILQ_NEXT(imp, link)) != NULL) {
		if (imp->lvs == prev->lvs) {
			return imp;
		}
	}
	return NULL;
}

const struct s3_export_manifest *
s3lvol_import_get_manifest(const struct s3lvol_import *imp)
{
	return imp->m;
}

static struct s3lvol_import *
import_add(struct s3lvol_lvstore *lvs, struct s3_export_manifest *m,
	   bool path_style, bool verify_tls)
{
	struct s3lvol_import *imp;

	imp = calloc(1, sizeof(*imp));
	if (!imp) {
		return NULL;
	}
	imp->lvs        = lvs;
	imp->m          = m;
	imp->path_style = path_style;
	imp->verify_tls = verify_tls;
	s3_export_manifest_ref(m);
	TAILQ_INSERT_TAIL(&g_imports, imp, link);

	/* Both callers want the lease: a fresh import starts one so the source
	 * knows it is being read, and an attach resurrecting the entry starts
	 * one again, because the clones that entry describes are reading again
	 * whether or not this process is the one that imported them. */
	import_lease_start(imp);

	return imp;
}

/* ==========================================================================
 * Liveness lease
 *
 * The importer's half of the cross-node protection: while a volume here reads
 * an export, keep a lease object in the *source* bucket fresh. The source
 * node will refuse to delete the snapshot behind a fresh lease; a stale or
 * missing lease is what "safe to delete" means over there.
 *
 * Deliberately fire-and-forget. A lost PUT is tolerated by the grace period
 * the source applies (3x the renew interval), and the poller just tries again
 * next period. There is no state to keep straight between PUTs, which is the
 * whole point of this design.
 * ========================================================================== */

static void
import_lease_put_done(void *cb_arg, int status)
{
	char *uuid_str = cb_arg;

	/* No state to update. A failure means the lease went stale for a while,
	 * which the source's grace period exists to absorb. Logged at debug level
	 * so a *sustained* failure is still visible in the noise. cb_arg is a
	 * strdup()'d copy precisely so this can run after import_remove() freed
	 * the import -- a PUT can still be in flight when that happens. */
	if (status != 0) {
		SPDK_WARNLOG("lease renew failed for export %s: %s\n",
			     uuid_str, spdk_strerror(-status));
	}
	free(uuid_str);
}

static int
import_lease_renew(void *arg)
{
	struct s3lvol_import *imp = arg;
	char body[128];
	struct iovec iov;
	uint64_t now = (uint64_t)time(NULL);
	char *uuid_str;

	/* The body is a tiny JSON document. The source needs three things from
	 * it: that the object was recently written (updated_at), who wrote it
	 * (importer_id, for an operator looking at the bucket), and how often it
	 * is renewed (renew_s). The last one is how the source computes its grace
	 * period without guessing: it cannot derive the renew interval from its
	 * own TTL, because the importer's interval depends on the remaining TTL
	 * *at import time*, which the source does not know. */
	snprintf(body, sizeof(body), "{\"importer_id\":\"%s\",\"updated_at\":%"
		 PRIu64 ",\"renew_s\":%lu}",
		 s3lvol_lvstore_get_name(imp->lvs), now,
		 (unsigned long)(imp->lease_interval_us / SPDK_SEC_TO_USEC));
	iov.iov_base = body;
	iov.iov_len  = strlen(body);

	/* Owned by the completion callback, not by this stack frame: s3_put()
	 * copies the body but not cb_arg, and the callback outlives the poller. */
	uuid_str = strdup(imp->m->uuid_str);
	if (!uuid_str) {
		return SPDK_POLLER_IDLE;
	}

	if (s3_put(imp->lease_client, imp->lease_key, &iov, 1, false,
		   import_lease_put_done, uuid_str) != 0) {
		/* The next poller tick retries; nothing else to do. */
		SPDK_WARNLOG("lease renew submit failed for export %s\n",
			     imp->m->uuid_str);
		free(uuid_str);
	}

	/* Fixed period, not re-armed by the PUT: the poller keeps ticking, and
	 * several PUTs in flight for the same key are harmless (same content,
	 * last writer wins). The interval is a third of the TTL, so the grace
	 * period at the source swallows a lost PUT with room to spare. */
	return SPDK_POLLER_IDLE;
}

static void
import_lease_stop(struct s3lvol_import *imp)
{
	if (imp->lease_poller) {
		spdk_poller_unregister(&imp->lease_poller);
		imp->lease_poller = NULL;
	}
	if (imp->lease_client) {
		s3_client_put(imp->lease_client);
		imp->lease_client = NULL;
	}
}

/* Start the lease for an import, if it has one. A dense export pins nothing and
 * has no TTL, so it gets no lease. Failure is reported and otherwise ignored:
 * a lease is an extra signal the source may consult, never a contract the
 * import depends on -- the import must not fail because the lease could not
 * start. */
static void
import_lease_start(struct s3lvol_import *imp)
{
	const struct s3_export_manifest *m = imp->m;
	struct s3_target target;
	uint64_t now, remaining, ttl;
	int rc;

	if (m->layout != S3_EXPORT_LAYOUT_REF || m->expires_at == 0) {
		return;
	}

	import_build_target(imp, &target);
	rc = s3_client_get_or_create(&target, &imp->lease_client);
	if (rc != 0) {
		SPDK_WARNLOG("No S3 client for the lease of export %s: %s. "
			     "The source may delete its snapshot while this "
			     "import still reads it.\n", m->uuid_str,
			     spdk_strerror(-rc));
		return;
	}

	snprintf(imp->lease_key, sizeof(imp->lease_key), "%s/meta/exports/%s.lease",
		 m->src.prefix, m->uuid_str);

	/* Renew at a third of the remaining TTL, floored at one second so a
	 * short-lived export is still protected. An export already past its
	 * deadline gets the same floor: renewing can only make the source
	 * *less* likely to delete, which is the safe direction. */
	now = (uint64_t)time(NULL);
	remaining = (m->expires_at > now) ? (m->expires_at - now) : 0;
	ttl = (remaining / 3) ? (remaining / 3) : 1;
	imp->lease_interval_us = ttl * SPDK_SEC_TO_USEC;

	imp->lease_poller = SPDK_POLLER_REGISTER(import_lease_renew, imp,
						 imp->lease_interval_us);
	if (!imp->lease_poller) {
		SPDK_WARNLOG("Could not start the lease poller for export %s\n",
			     m->uuid_str);
		s3_client_put(imp->lease_client);
		imp->lease_client = NULL;
		return;
	}

	SPDK_NOTICELOG("export %s lease renewing every %" PRIu64 " second(s)\n",
		       m->uuid_str, ttl);
}

static void
import_remove(struct s3lvol_import *imp)
{
	import_lease_stop(imp);
	TAILQ_REMOVE(&g_imports, imp, link);
	s3_export_manifest_unref(imp->m);
	free(imp);
}

void
s3lvol_xfer_lvstore_fini(struct s3lvol_lvstore *lvs)
{
	struct s3lvol_import *imp, *tmp;

	/* Only the cache goes away. The registry object in S3 stays exactly as it
	 * is, because it describes the lvstore, not this process -- the next attach
	 * needs every entry of it to be able to open the clones again. */
	TAILQ_FOREACH_SAFE(imp, &g_imports, link, tmp) {
		if (imp->lvs == lvs) {
			import_remove(imp);
		}
	}
}

/* ==========================================================================
 * Registry persistence
 * ========================================================================== */

struct json_buf {
	char*data;
	size_t  len;
	size_t  cap;
};

static int
json_buf_append(void *cb_ctx, const void *data, size_t size)
{
	struct json_buf *buf = cb_ctx;

	if (buf->len + size + 1 > buf->cap) {
		size_t cap = buf->cap ? buf->cap * 2 : 4096;
		char *grown;

		while (cap < buf->len + size + 1) {
			cap *= 2;
		}
		grown = realloc(buf->data, cap);
		if (!grown) {
			return -ENOMEM;
		}
		buf->data = grown;
		buf->cap = cap;
	}
	memcpy(buf->data + buf->len, data, size);
	buf->len += size;
	buf->data[buf->len] = '\0';
	return 0;
}

/* The registry embeds each manifest as a JSON *string*, not as a nested object.
 * That keeps one parser for manifests -- the same bytes an importer downloaded
 * are the bytes stored and re-parsed here, so a manifest cannot mean one thing
 * in the export and another in the registry. */
static int
imports_serialize(struct s3lvol_lvstore *lvs, char **out, size_t *out_len)
{
	struct json_buf buf = {0};
	struct spdk_json_write_ctx *w;
	struct s3lvol_import *imp;
	int rc;

	w = spdk_json_write_begin(json_buf_append, &buf, 0);
	if (!w) {
		return -ENOMEM;
	}

	spdk_json_write_object_begin(w);
	spdk_json_write_named_uint32(w, "version", S3_EXPORT_VERSION);
	spdk_json_write_named_array_begin(w, "imports");

	TAILQ_FOREACH(imp, &g_imports, link) {
		char *manifest_json = NULL;
		size_t manifest_len = 0;

		if (imp->lvs != lvs) {
			continue;
		}
		rc = s3_export_manifest_serialize(imp->m, &manifest_json, &manifest_len);
		if (rc != 0) {
			spdk_json_write_end(w);
			free(buf.data);
			return rc;
		}
		spdk_json_write_object_begin(w);
		spdk_json_write_named_bool(w, "path_style", imp->path_style);
		spdk_json_write_named_bool(w, "verify_tls", imp->verify_tls);
		spdk_json_write_named_string(w, "manifest", manifest_json);
		spdk_json_write_object_end(w);
		free(manifest_json);
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

struct import_entry_json {
	bool  path_style;
	bool  verify_tls;
	char *manifest;
};

static const struct spdk_json_object_decoder import_entry_decoders[] = {
	{"path_style", offsetof(struct import_entry_json, path_style), spdk_json_decode_bool,   true},
	{"verify_tls", offsetof(struct import_entry_json, verify_tls), spdk_json_decode_bool,   true},
	{"manifest",   offsetof(struct import_entry_json, manifest),   spdk_json_decode_string, false},
};

struct entries_holder {
	struct import_entry_json e[S3LVOL_MAX_IMPORTS];
	size_t                   n;
};

struct imports_json {
	uint32_t              version;
	struct entries_holder entries;
};

static int
decode_import_entry(const struct spdk_json_val *val, void *out)
{
	return spdk_json_decode_object(val, import_entry_decoders,
				       SPDK_COUNTOF(import_entry_decoders), out);
}

static int
decode_import_entries(const struct spdk_json_val *val, void *out)
{
	struct entries_holder *h = out;

	return spdk_json_decode_array(val, decode_import_entry, h->e,
				      S3LVOL_MAX_IMPORTS, &h->n, sizeof(h->e[0]));
}

static const struct spdk_json_object_decoder imports_decoders[] = {
	{"version", offsetof(struct imports_json, version), spdk_json_decode_uint32, false},
	{"imports", offsetof(struct imports_json, entries), decode_import_entries,false},
};

static int
imports_parse(struct s3lvol_lvstore *lvs, const void *json, size_t len)
{
	struct imports_json j = {0};
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

	/* Counted without DECODE_IN_PLACE, and this is not a detail.
	 *
	 * spdk_json_parse() unescapes in place whenever that flag is set, whether or
	 * not it was given anywhere to store the values -- json_decode_string()
	 * checks the flag alone. So counting with the flag set rewrites the buffer,
	 * and the second pass then parses something that is no longer the document:
	 * every \" inside a string has become ", which ends that string early.
	 *
	 * This registry is exactly the case that trips over it, because it carries
	 * each manifest as a JSON *string* and those are nothing but escapes. */
	num_values = spdk_json_parse(copy, len, NULL, 0, NULL, 0);
	if (num_values <= 0) {
		SPDK_ERRLOG("imports registry of '%s' is not valid JSON\n",
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
		SPDK_ERRLOG("imports registry of '%s' did not parse on the second "
			    "pass (%zd)\n", s3lvol_lvstore_get_name(lvs), num_values);
		rc = -EINVAL;
		goto out;
	}
	if (spdk_json_decode_object(values, imports_decoders,
				    SPDK_COUNTOF(imports_decoders), &j) != 0) {
		SPDK_ERRLOG("imports registry of '%s' could not be decoded\n",
			    s3lvol_lvstore_get_name(lvs));
		rc = -EINVAL;
		goto out;
	}

	rc = 0;
	for (i = 0; i < j.entries.n; i++) {
		struct import_entry_json *e = &j.entries.e[i];
		struct s3_export_manifest *m = NULL;

		rc = s3_export_manifest_parse(e->manifest, strlen(e->manifest), &m);
		if (rc != 0) {
			/* Fatal on purpose. A manifest we cannot parse is a clone we
			 * cannot open, and continuing would load the lvstore with an
			 * esnap clone whose parent silently reads as zeroes. */
			SPDK_ERRLOG("imports registry of '%s': entry %zu is unusable "
				    "(%s); the esnap clone using it cannot be "
				    "opened\n", s3lvol_lvstore_get_name(lvs), i,
				    spdk_strerror(-rc));
			break;
		}
		if (!import_add(lvs, m, e->path_style, e->verify_tls)) {
			s3_export_manifest_unref(m);
			rc = -ENOMEM;
			break;
		}
		s3_export_manifest_unref(m);   /* the registry holds its own ref */
	}

	SPDK_NOTICELOG("lvstore '%s': %zu imported export(s) in the registry\n",
		       s3lvol_lvstore_get_name(lvs), j.entries.n);
out:
	for (i = 0; i < j.entries.n; i++) {
		free(j.entries.e[i].manifest);
	}
	free(values);
	free(copy);
	return rc;
}

/* ---- load ---- */

struct imports_load_ctx {
	struct s3lvol_lvstore *lvs;
	spdk_lvs_op_complete   cb_fn;
	void                  *cb_arg;
	char                   key[S3_EXPORT_KEY_MAX];
	uint64_t               size;
	char  *body;
};

static void
imports_load_done(struct imports_load_ctx *ctx, int status)
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
imports_load_got_body(void *cb_arg, uint64_t bytes_read, int status)
{
	struct imports_load_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to read '%s': %s\n", ctx->key,
			    spdk_strerror(-status));
		imports_load_done(ctx, status);
		return;
	}

	imports_load_done(ctx, imports_parse(ctx->lvs, ctx->body, bytes_read));
}

static void
imports_load_head_done(void *cb_arg, int status)
{
	struct imports_load_ctx *ctx = cb_arg;
	int rc;

	if (status == -ENOENT) {
		/* No imports were ever made. The overwhelmingly common case, and
		 * not an error: this runs on every attach. */
		imports_load_done(ctx, 0);
		return;
	}
	if (status != 0) {
		SPDK_ERRLOG("Failed to look up '%s': %s\n", ctx->key,
			    spdk_strerror(-status));
		imports_load_done(ctx, status);
		return;
	}
	if (ctx->size == 0) {
		imports_load_done(ctx, 0);
		return;
	}
	if (ctx->size > 64u * 1024 * 1024) {
		/* Bounded because this is read into one allocation, and because a
		 * registry that big is not a registry -- it is a sign the key is being
		 * shared with something else. */
		SPDK_ERRLOG("'%s' is %" PRIu64 " bytes, which is not a plausible "
			    "imports registry\n", ctx->key, ctx->size);
		imports_load_done(ctx, -EINVAL);
		return;
	}

	ctx->body = malloc(ctx->size);
	if (!ctx->body) {
		imports_load_done(ctx, -ENOMEM);
		return;
	}

	rc = s3_get_range(s3lvol_lvstore_get_client(ctx->lvs), ctx->key, 0, ctx->size,
			  ctx->body, imports_load_got_body, ctx);
	if (rc != 0) {
		imports_load_done(ctx, rc);
	}
}

int
s3lvol_xfer_imports_load(struct s3lvol_lvstore *lvs, spdk_lvs_op_complete cb_fn,
			 void *cb_arg)
{
	struct imports_load_ctx *ctx;
	int rc;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->lvs= lvs;
	ctx->cb_fn= cb_fn;
	ctx->cb_arg = cb_arg;
	imports_registry_key(s3lvol_lvstore_get_name(lvs), ctx->key, sizeof(ctx->key));

	/* HEAD then GET, because aranged GET needs a length and this object has no
	 * fixed size. Two round trips once per attach is not worth optimising. */
	rc = s3_head(s3lvol_lvstore_get_client(lvs), ctx->key, &ctx->size,
		     imports_load_head_done, ctx);
	if (rc != 0) {
		free(ctx);
	}
	return rc;
}

/* ---- save ---- */

struct imports_save_ctx {
	spdk_lvs_op_complete cb_fn;
	void                *cb_arg;
	char                *json;
	struct iovec         iov;
	char                 key[S3_EXPORT_KEY_MAX];
};

static void
imports_save_done(void *cb_arg, int status)
{
	struct imports_save_ctx *ctx = cb_arg;
	spdk_lvs_op_complete cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to write '%s': %s. The esnap clones of this "
			    "lvstore may not be openable after a restart.\n",
			    ctx->key, spdk_strerror(-status));
	}

	free(ctx->json);
	free(ctx);

	if (cb_fn) {
		cb_fn(user_arg, status);
	}
}

static int
imports_save(struct s3lvol_lvstore *lvs, spdk_lvs_op_complete cb_fn, void *cb_arg)
{
	struct imports_save_ctx *ctx;
	size_t len;
	int rc;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;
	imports_registry_key(s3lvol_lvstore_get_name(lvs), ctx->key, sizeof(ctx->key));

	rc = imports_serialize(lvs, &ctx->json, &len);
	if (rc != 0) {
		free(ctx);
		return rc;
	}
	ctx->iov.iov_base = ctx->json;
	ctx->iov.iov_len = len;

	rc = s3_put(s3lvol_lvstore_get_client(lvs), ctx->key, &ctx->iov, 1, false,
		    imports_save_done, ctx);
	if (rc != 0) {
		free(ctx->json);
		free(ctx);
	}
	return rc;
}

/* ==========================================================================
 * The esnap callback
 * ========================================================================== */

static void
import_build_target(const struct s3lvol_import *imp, struct s3_target *out)
{
	const struct s3_export_manifest *m = imp->m;

	memset(out, 0, sizeof(*out));
	/* Casting away const: s3_target holds char * but s3_client_get_or_create
	 * only reads and copies them. */
	out->endpoint       = (char *)m->src.endpoint;
	out->region         = (char *)m->src.region;
	out->bucket         = (char *)m->src.bucket;
	out->prefix         = (char *)m->src.prefix;
	out->use_path_style = imp->path_style;
	out->verify_tls     = imp->verify_tls;
	/* Credentials come from the environment, exactly as everywhere else: the
	 * importing node authenticates as itself, and the manifest never carries a
	 * secret. */
	out->auth_mode      = S3_AUTH_ENV;
}

int
s3lvol_esnap_dev_create(void *bs_ctx, void *blob_ctx, struct spdk_blob *blob,
			const void *esnap_id, uint32_t id_len,
			struct spdk_bs_dev **bs_dev)
{
	struct spdk_lvol_store *store = bs_ctx;
	struct s3lvol_lvstore *lvs;
	struct s3lvol_import *imp;
	struct s3_client *client = NULL;
	struct s3_target target;
	char uuid_str[SPDK_UUID_STRING_LEN];
	int rc;

	if (!store || !esnap_id || id_len == 0 || id_len >= sizeof(uuid_str)) {
		return -EINVAL;
	}
	memcpy(uuid_str, esnap_id, id_len);
	uuid_str[id_len] = '\0';

	/* Only for the log line. This runs during load, when the lvstore wrapper is
	 * not yet associated with the blobstore-level one, so it is usually NULL. */
	lvs = s3lvol_lvstore_find_by_lvs(store);

	/* Looked up by uuid across every lvstore, deliberately. The identity of the
	 * lvstore asking is not available during load, and it does not matter: an
	 * export uuid is globally unique, so at most one manifest can answer to it,
	 * and two lvstores importing the same export hold identical copies. */
	imp = import_find_any(uuid_str);
	if (!imp) {
		/* Either the registry object was lost, or the export was released while
		 * a clone still depended on it -- which is what release refuses to do
		 * while an import is on record, so this means the registry and the blob
		 * metadata disagree. */
		SPDK_ERRLOG("lvstore '%s': no manifest for esnap parent '%s'. That "
			    "clone cannot be opened. If the export was released early, "
			    "it has to be recreated with the same uuid.\n",
			    lvs ? s3lvol_lvstore_get_name(lvs) : "(loading)", uuid_str);
		return -ENOENT;
	}

	import_build_target(imp, &target);
	rc = s3_client_get_or_create(&target, &client);
	if (rc != 0) {
		SPDK_ERRLOG("No S3 client for the source of '%s': %s\n", uuid_str,
			    spdk_strerror(-rc));
		return rc;
	}

	/* Ownership of the client reference moves into the bs_dev, which releases
	 * it from destroy(). */
	rc = s3_export_bs_dev_create(client, imp->m, bs_dev);
	if (rc != 0) {
		s3_client_put(client);
	}
	return rc;
}

/* ==========================================================================
 * Export
 * ========================================================================== */

/* Exports that have been handed a uuid but whose manifest is not yet durable.
 *
 * Exists so rcow_get_snapshot_status can answer INPROGRESS: the registry only
 * learns an export once it is durable, so without this a status query during the
 * drain/walk would report NONE for an export that is about to exist.
 *
 * Keyed by uuid alone: generated uuids are unique, and an explicitly supplied
 * one is the caller's promise that it is unique. */
struct export_inflight {
	struct s3lvol_lvstore       *lvs;
	char                         uuid_str[SPDK_UUID_STRING_LEN];
	char                         snapshot_name[SPDK_LVOL_NAME_MAX];
	TAILQ_ENTRY(export_inflight) link;
};

static TAILQ_HEAD(, export_inflight) g_inflight =
	TAILQ_HEAD_INITIALIZER(g_inflight);

static struct export_inflight *
export_inflight_find(const char *uuid)
{
	struct export_inflight *inf;

	TAILQ_FOREACH(inf, &g_inflight, link) {
		if (strcmp(inf->uuid_str, uuid) == 0) {
			return inf;
		}
	}
	return NULL;
}

/* The in-flight export of a given snapshot, if any.
 *
 * The inverse lookup of export_inflight_find: an export is identified by uuid,
 * but the idempotence question is asked by snapshot. Only auto-generated
 * exports go through here; one with an explicit export_id is its own key. */
static struct export_inflight *
export_inflight_find_by_snapshot(struct s3lvol_lvstore *lvs,
				 const char *snapshot_name)
{
	struct export_inflight *inf;

	if (!lvs || !snapshot_name) {
		return NULL;
	}

	TAILQ_FOREACH(inf, &g_inflight, link) {
		if (inf->lvs == lvs &&
		    strcmp(inf->snapshot_name, snapshot_name) == 0) {
			return inf;
		}
	}
	return NULL;
}

/* Registered before the drain starts, so an export is observable for its whole
 * life. Reports failure rather than carrying on without a record: an export
 * missing from here reads as NONE, i.e. as one that failed, while it is in fact
 * still running -- and nothing downstream could tell the difference. */
static int
export_inflight_add(struct s3lvol_lvstore *lvs, const char *uuid,
		    const char *snapshot_name)
{
	struct export_inflight *inf;

	inf = calloc(1, sizeof(*inf));
	if (!inf) {
		SPDK_ERRLOG("Cannot record export %s as in progress; refusing to "
			    "start it rather than run it unobservably\n", uuid);
		return -ENOMEM;
	}
	inf->lvs = lvs;
	snprintf(inf->uuid_str, sizeof(inf->uuid_str), "%s", uuid);
	snprintf(inf->snapshot_name, sizeof(inf->snapshot_name), "%s",
		 snapshot_name);
	TAILQ_INSERT_TAIL(&g_inflight, inf, link);
	return 0;
}

static void
export_inflight_remove(const char *uuid)
{
	struct export_inflight *inf = export_inflight_find(uuid);

	if (inf) {
		TAILQ_REMOVE(&g_inflight, inf, link);
		free(inf);
	}
}

bool
s3lvol_export_inflight_pinning(struct s3lvol_lvstore *lvs,
			       const char *snapshot_name)
{
	struct export_inflight *inf;

	if (!lvs || !snapshot_name) {
		return false;
	}

	TAILQ_FOREACH(inf, &g_inflight, link) {
		if (inf->lvs == lvs &&
		    strcmp(inf->snapshot_name, snapshot_name) == 0) {
			return true;
		}
	}
	return false;
}

/* A drain runs one at a time per lvstore (s3_flusher_drain answers -EBUSY while
 * another one is in progress), so two exports of the same lvstore started close
 * together have the second one refused. Retry instead of failing: the drain in
 * progress is what the second export needed anyway, and it finishes in seconds.
 *
 * The window matters because the alternative is silent from the caller's side.
 * rcow_export_snapshot answers with the uuid before the drain runs, so a drain
 * that fails takes the export down asynchronously -- and export_report() then
 * drops the in-flight marker, leaving nothing in the registry, so asking
 * rcow_get_snapshot_status about that uuid reads "does not exist". Observed in
 * production: two exports 8 ms apart, both refused, the third one 13 s later
 * succeeded.
 *
 * The retry budget is a worst-case bound, not a target. A drain is held not
 * only by another export but also by a decouple that is still materialising:
 * a full-volume import measured 3m43s, and the materialisation always finishes
 * -- so the window is 30 minutes, retried every 500ms, and only an lvstore
 * that never drains reports a failure.
 */
#define EXPORT_DRAIN_RETRY_US    (500 * 1000)
#define EXPORT_DRAIN_MAX_RETRIES (30 * 60 * 1000 / 500)

struct export_ctx {
	struct s3lvol_lvstore    *lvs;
	struct spdk_lvol         *snapshot;

	struct spdk_io_channel   *channel;
	struct s3lvol_export_info info;

	/* How long this node promises to keep the snapshot for the importer.
	 * Only meaningful for a zero-copy export; a copy owes nobody anything. */
	uint64_t expires_at;

	/* Set only while waiting to retry a drain that was refused. */
	struct spdk_poller       *drain_retry_poller;
	uint32_t                  drain_retries;

	s3lvol_export_cb          cb_fn;
	void                     *cb_arg;
};

static void export_copy(struct export_ctx *ctx);
static void export_ref(struct export_ctx *ctx);
static void export_drain(struct export_ctx *ctx);
static void export_drained(void *cb_arg, int status);

static void
export_report(struct export_ctx *ctx, int status)
{
	s3lvol_export_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;
	struct s3lvol_export_info info = ctx->info;

	/* One line per failed export, whatever failed, naming the uuid.
	 *
	 * Nothing else guarantees that. rcow_export_snapshot hands the uuid back as
	 * soon as the export starts and passes no callback, so an export that dies
	 * afterwards dies asynchronously, and all the caller ever sees is
	 * rcow_get_snapshot_status calling the uuid unknown -- the in-flight marker
	 * is dropped just below and nothing was recorded. That says the export
	 * failed but not why, and several of the paths into here logged nothing at
	 * all: the channel allocation, export_fill_src, the registry save, the
	 * submit of either engine. On a production node that leaves no way to find
	 * out what happened.
	 *
	 * The step that failed often logs something more specific as well. The two
	 * are not redundant: that one says which step, this one says which export it
	 * took down. */
	if (status != 0) {
		SPDK_ERRLOG("Export %s of snapshot '%s' failed: %s. The uuid is dead -- "
			    "nothing was recorded under it, so asking about it reports no "
			    "such export; re-export to try again.\n",
			    ctx->info.export_uuid, ctx->info.snapshot_name,
			    spdk_strerror(-status));
	}

	if (ctx->channel) {
		spdk_bs_free_io_channel(ctx->channel);
	}

	/* Only set when a drain retry was pending, which cannot be the case here --
	 * the retry path either resubmits or reports, never both. Unregistered
	 * defensively so a future path out cannot leak a poller. */
	spdk_poller_unregister(&ctx->drain_retry_poller);

	/* Drop the in-flight marker no matter how it ended. From here the uuid is
	 * either a durable export (the registry has it) or gone (NONE). */
	export_inflight_remove(ctx->info.export_uuid);

	free(ctx);

	if (cb_fn) {
		cb_fn(cb_arg, status == 0 ? &info : NULL, status);
	}
}

static void
export_recorded(void *cb_arg, int status)
{
	struct export_ctx *ctx = cb_arg;

	if (status != 0) {
		/* The manifest is up, but this node has no durable record that it owes
		 * the snapshot to anybody. Reported as a failed export, because the
		 * alternative is telling the caller it worked and then deleting that
		 * snapshot after the next restart. The manifest left behind expires on
		 * its own. */
		SPDK_ERRLOG("Export %s is published but could not be recorded; treating "
			    "the export as failed\n", ctx->info.export_uuid);
	}
	export_report(ctx, status);
}

static void
export_copy_done(void *cb_arg, struct s3_export_manifest *m, int status)
{
	struct export_ctx *ctx = cb_arg;
	struct s3lvol_export *exp;
	int rc;

	if (status != 0) {
		s3_export_manifest_unref(m);
		export_report(ctx, status);
		return;
	}

	ctx->info.size_bytes = m->size_bytes;
	ctx->info.num_chunks = m->num_chunks;
	ctx->info.present_chunks = m->present_chunks;
	ctx->info.chunk_size = m->chunk_size;
	ctx->info.zero_copy = (m->layout == S3_EXPORT_LAYOUT_REF);
	ctx->info.expires_at = m->expires_at;

	/* Recorded once the manifest is durable, and before the caller is told the
	 * export exists. In between is the only window in which this node could
	 * forget the obligation across a restart, and it is one PUT wide. */
	exp = s3lvol_export_add(ctx->lvs, m, ctx->info.snapshot_name);
	s3_export_manifest_unref(m);

	if (!exp) {
		export_report(ctx, -ENOMEM);
		return;
	}

	rc = s3lvol_export_registry_save(ctx->lvs, export_recorded, ctx);
	if (rc != 0) {
		s3lvol_export_forget(exp);
		export_report(ctx, rc);
	}
}

/* Everything the manifest says about where it came from. Both engines record the
 * same thing, so it is filled in once. */
static int
export_fill_src(struct export_ctx *ctx, struct s3_export_source *src)
{
	const char *ns = s3lvol_lvstore_get_namespace(ctx->lvs);
	const struct s3_target *tgt = rcow_namespace_to_target(ns);

	/* The manifest records where the source lives, and that record is what the
	 * *importing* node feeds to s3_client_get_or_create() through
	 * import_build_target() every time it reopens the clone. An empty endpoint
	 * here would therefore produce a manifest that fails much later, on another
	 * node, at attach time -- so refuse to write one rather than paper over it. */
	if (!tgt || !tgt->endpoint || !tgt->bucket) {
		SPDK_ERRLOG("lvstore '%s': namespace '%s' does not resolve to a COS "
			    "target, so the manifest could not say where the exported "
			    "objects live\n",
			    s3lvol_lvstore_get_name(ctx->lvs), ns ? ns : "(none)");
		return -ENOENT;
	}

	snprintf(src->endpoint, sizeof(src->endpoint), "%s", tgt->endpoint);
	snprintf(src->region, sizeof(src->region), "%s",
		 tgt->region ? tgt->region : "");
	snprintf(src->bucket, sizeof(src->bucket), "%s", tgt->bucket);
	snprintf(src->prefix, sizeof(src->prefix), "%s",
		 s3lvol_lvstore_get_name(ctx->lvs));
	snprintf(src->lvs_name, sizeof(src->lvs_name), "%s",
		 s3lvol_lvstore_get_name(ctx->lvs));
	snprintf(src->snapshot, sizeof(src->snapshot), "%s", ctx->snapshot->name);
	src->blob_id = spdk_blob_get_id(ctx->snapshot->blob);
	/* The identity of the source, as opposed to blob_id, which blobstore hands
	 * back out after a delete. See the field's comment in s3_export.h. */
	snprintf(src->snapshot_uuid, sizeof(src->snapshot_uuid), "%s",
		 ctx->snapshot->uuid_str);

	snprintf(ctx->info.snapshot_name, sizeof(ctx->info.snapshot_name), "%s",
		 ctx->snapshot->name);
	snprintf(ctx->info.url, sizeof(ctx->info.url), "s3://%s/%s/exports/%s.json",
		 src->bucket, src->prefix, ctx->info.export_uuid);
	return 0;
}

/* The open blob of the lvol holding this id, or NULL if no lvol of this store
 * has it. The lvol store keeps a blob open for every lvol it loaded, which is
 * what lets the chain below be assembled without opening anything. */
static struct spdk_blob *
blob_by_id(struct spdk_lvol_store *store, spdk_blob_id id)
{
	struct spdk_lvol *lvol;

	TAILQ_FOREACH(lvol, &store->lvols, link) {
		if (lvol->blob_id == id) {
			return lvol->blob;
		}
	}
	return NULL;
}

/* Collect the snapshot's clone chain, nearest first.
 *
 * This exists because a snapshot rarely owns all of its own data. Taking a second
 * snapshot of a volume hands the new one only the clusters written since the
 * first: blobstore moves the cluster map across and leaves everything older in
 * the older snapshot. So the ordinary `lvol -> snap1 -> snap2` produces a snap2
 * that owns almost nothing, and naming only its clusters would leave the importer
 * reading zeroes wherever snap1 still holds the data.
 *
 * Every layer resolves through the same chunk map -- one bs_dev, one address
 * space -- so this costs a list lookup per layer and no I/O at all.
 *
 * Failures here are all routing answers, not errors: the copy engine reads through
 * blobstore, which flattens the chain by itself, so it can handle every case this
 * declines.
 */
static int
export_build_chain(struct s3lvol_lvstore *lvs, struct spdk_blob *snapshot,
		   struct spdk_blob **chain, uint32_t max, uint32_t *out_len)
{
	struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(lvs);
	struct spdk_blob *cur = snapshot;
	uint32_t n = 0;

	while (true) {
		spdk_blob_id pid;

		if (n == max) {
			SPDK_NOTICELOG("Snapshot chain is deeper than %u; exporting by "
				       "copying instead\n", max);
			return -E2BIG;
		}
		chain[n++] = cur;

		/* Checked before asking for a parent snapshot, because
		 * spdk_blob_get_parent_snapshot() only knows about blobs of this
		 * blobstore and answers SPDK_BLOBID_INVALID for an external one -- which
		 * is indistinguishable from the end of the chain. Getting this order
		 * wrong would end the walk early and silently drop everything the esnap
		 * parent holds.
		 *
		 * The parent's data is under another lvstore's prefix, so this chunk map
		 * cannot resolve it at all. That is also why it matters whether the
		 * importing node still reads through to an export: a volume with no
		 * parent can be handed off zero-copy again. */
		if (spdk_blob_is_esnap_clone(cur)) {
			SPDK_NOTICELOG("Snapshot chain reaches an external snapshot, whose "
				       "data is not in this lvstore's chunk map; exporting "
				       "by copying instead.\n");
			return -ENOTSUP;
		}

		pid = spdk_blob_get_parent_snapshot(store->blobstore,
						spdk_blob_get_id(cur));
		if (pid == SPDK_BLOBID_INVALID) {
			break;
		}

		cur = blob_by_id(store, pid);
		if (!cur) {
			/* Should not happen: every blob in an lvstore belongs to an lvol,
			 * and the store holds them open. Reported rather than worked
			 * around, because the alternative is a manifest missing whatever
			 * that layer held. */
			SPDK_ERRLOG("Snapshot chain names blob %" PRIu64 ", which no lvol "
				    "of '%s' has open; exporting by copying instead\n",
				    pid, s3lvol_lvstore_get_name(lvs));
			return -ENOENT;
		}
	}

	*out_len = n;
	return 0;
}

/* Name the objects the snapshot already occupies, and upload only the manifest.
 *
 * This is the path a handoff is supposed to take: no data is read and none is
 * written, so what it costs is a round trip rather than the size of the volume.
 *
 * A failure to assemble the chain, or -ENOTSUP from the walk, is not a failure --
 * it is a routing answer. Copying is correct in every case either of them
 * declines. */
static void
export_ref(struct export_ctx *ctx)
{
	struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(ctx->lvs);
	struct spdk_blob *chain[S3LVOL_DEFAULT_MAX_CHAIN_DEPTH];
	struct s3_export_ref_opts opts = {0};
	uint32_t chain_len = 0;
	int rc;

	rc = export_build_chain(ctx->lvs, ctx->snapshot->blob, chain,
				SPDK_COUNTOF(chain), &chain_len);
	if (rc != 0) {
		export_copy(ctx);
		return;
	}

	opts.bs_dev = s3lvol_lvstore_get_bs_dev(ctx->lvs);
	opts.chain = chain;
	opts.chain_len = chain_len;
	opts.client = s3lvol_lvstore_get_client(ctx->lvs);
	opts.prefix = s3lvol_lvstore_get_name(ctx->lvs);
	opts.uuid_str = ctx->info.export_uuid;
	opts.cluster_size = (uint32_t)spdk_bs_get_cluster_size(store->blobstore);
	opts.expires_at = ctx->expires_at;
	rc = export_fill_src(ctx, &opts.src);
	if (rc != 0) {
		export_report(ctx, rc);
		return;
	}

	rc = s3_export_run_ref(&opts, export_copy_done, ctx);
	if (rc == -ENOTSUP) {
		export_copy(ctx);
		return;
	}
	if (rc == -EIO) {
		/* The lvstore was flushed before this walk, so a cluster with no
		 * committed mapping means something is wrong with the chunk map rather
		 * than with the ordering. Copying still works -- it reads through
		 * blobstore, which assembles from the WAL and the overlay too. */
		SPDK_ERRLOG("Export %s: a chunk has no committed mapping even after "
			    "flushing '%s'. Exporting by copying instead.\n",
			    ctx->info.export_uuid, s3lvol_lvstore_get_name(ctx->lvs));
		export_copy(ctx);
		return;
	}
	if (rc != 0) {
		export_report(ctx, rc);
	}
}

/* The one thing an export genuinely has to wait for.
 *
 * It is tempting to walk first and flush only if some cluster turns out to have no
 * mapping yet -- the snapshot is usually taken well before the export, so the
 * periodic flush has usually dealt with it. That does not work, and the way it
 * fails is quiet.
 *
 * s3_chunk_map_lookup() shows committed mappings, where committed means the
 * journal record is durable -- not that the object is in S3 and holds the whole
 * chunk. A chunk written in several pieces gets an entry as soon as the first
 * piece is flushed, and its valid_bytes grows as the rest arrive. So a walk that
 * runs while the flusher is still working finds every lookup succeeding and
 * records uuids for objects that are still in flight, or that will never exist at
 * all because the next flush of that chunk supersedes them. The importer then
 * reads a 404, or a chunk that is short.
 *
 * Measured: an 8 MiB region exported mid-flush produced a manifest naming 8
 * objects holding 2.75 MiB, one of which was never uploaded under that uuid.
 * Nothing failed on the exporting side.
 *
 * There is no cheap test for "this entry is finished", because a partially written
 * chunk is perfectly legitimate -- valid_bytes below chunk_size is the normal
 * state of a sparse volume, not a symptom. So the flush is unconditional, and it
 * is what the handoff pays for the guarantee that every uuid it names is durable.
 *
 * After the snapshot, not before: a write landing between a flush and the snapshot
 * would be in the snapshot with nothing to name it. */
static void
export_drain(struct export_ctx *ctx)
{
	s3lvol_lvstore_flush(ctx->lvs, export_drained, ctx);
}

/* One-shot: unregisters itself, then resubmits the drain. */
static int
export_drain_retry(void *arg)
{
	struct export_ctx *ctx = arg;

	spdk_poller_unregister(&ctx->drain_retry_poller);
	export_drain(ctx);
	return SPDK_POLLER_BUSY;
}

static void
export_drained(void *cb_arg, int status)
{
	struct export_ctx *ctx = cb_arg;

	/* Somebody else's drain is running. Wait for it and ask again: a drain that
	 * completes is exactly what this export was waiting for, so the retry
	 * normally succeeds on the first attempt. Bounded so an lvstore that never
	 * finishes draining reports a failure instead of retrying forever. */
	if (status == -EBUSY && ctx->drain_retries < EXPORT_DRAIN_MAX_RETRIES) {
		if (ctx->drain_retries == 0) {
			/* Once, not once per attempt: fifty lines a tenth of a second
			 * apart would bury whatever else the node is saying. One line is
			 * enough to tell an export that is waiting from one that is
			 * stuck, and to say when the waiting started. */
			SPDK_NOTICELOG("Export %s of snapshot '%s' is waiting for another "
				       "drain of lvstore '%s' to finish\n",
				       ctx->info.export_uuid, ctx->info.snapshot_name,
				       s3lvol_lvstore_get_name(ctx->lvs));
		}
		ctx->drain_retries++;
		ctx->drain_retry_poller = SPDK_POLLER_REGISTER(export_drain_retry, ctx,
							      EXPORT_DRAIN_RETRY_US);
		if (ctx->drain_retry_poller) {
			return;
		}
		/* Could not even wait; fall through and report the -EBUSY. */
		SPDK_ERRLOG("Export %s could not schedule a drain retry\n",
			    ctx->info.export_uuid);
	}

	if (status != 0) {
		char waited[80] = "";

		/* How long it waited, not just that it did: "busy" after five seconds
		 * of retrying is a different problem from "busy" on the first ask. */
		if (ctx->drain_retries > 0) {
			snprintf(waited, sizeof(waited),
				 " (still busy after %" PRIu32 " ms across %" PRIu32
				 " retr%s)",
				 ctx->drain_retries * (EXPORT_DRAIN_RETRY_US / 1000),
				 ctx->drain_retries,
				 ctx->drain_retries == 1 ? "y" : "ies");
		}
		SPDK_ERRLOG("Export %s could not drain lvstore '%s' before exporting "
			    "snapshot '%s': %s%s\n",
			    ctx->info.export_uuid, s3lvol_lvstore_get_name(ctx->lvs),
			    ctx->info.snapshot_name, spdk_strerror(-status), waited);
		export_report(ctx, status);
		return;
	}

	if (ctx->drain_retries > 0) {
		SPDK_NOTICELOG("Export %s drained '%s' after %" PRIu32 " retr%s "
			       "(%" PRIu32 " ms)\n",
			       ctx->info.export_uuid,
			       s3lvol_lvstore_get_name(ctx->lvs), ctx->drain_retries,
			       ctx->drain_retries == 1 ? "y" : "ies",
			       ctx->drain_retries * (EXPORT_DRAIN_RETRY_US / 1000));
	}

	export_ref(ctx);
}

static void
export_copy(struct export_ctx *ctx)
{
	struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(ctx->lvs);
	struct s3_export_opts opts = {0};
	uint64_t cluster_size = spdk_bs_get_cluster_size(store->blobstore);
	int rc;

	/* Export chunks are clusters. Not "the lvstore's S3 chunk size": the export
	 * does not reference the source's objects, so its own slicing is free to be
	 * whatever makes the copy simplest, and a cluster is the unit blobstore
	 * allocates in -- which is what makes every chunk either wholly allocated
	 * or wholly a hole. */
	if ((cluster_size & (cluster_size - 1)) != 0) {
		SPDK_ERRLOG("lvstore '%s' has a cluster size of %" PRIu64 " which is "
			    "not a power of two; export needs one\n",
			    s3lvol_lvstore_get_name(ctx->lvs), cluster_size);
		export_report(ctx, -ENOTSUP);
		return;
	}

	ctx->channel = spdk_bs_alloc_io_channel(store->blobstore);
	if (!ctx->channel) {
		export_report(ctx, -ENOMEM);
		return;
	}

	opts.client= s3lvol_lvstore_get_client(ctx->lvs);
	opts.prefix       = s3lvol_lvstore_get_name(ctx->lvs);
	opts.uuid_str     = ctx->info.export_uuid;
	opts.blob         = ctx->snapshot->blob;
	opts.channel      = ctx->channel;
	opts.chunk_size   = (uint32_t)cluster_size;
	opts.cluster_size = (uint32_t)cluster_size;

	rc = export_fill_src(ctx, &opts.src);
	if (rc != 0) {
		export_report(ctx, rc);
		return;
	}

	rc = s3_export_run(&opts, export_copy_done, ctx);
	if (rc != 0) {
		export_report(ctx, rc);
	}
}
int
s3lvol_lvol_export(struct s3lvol_lvstore *lvs, struct spdk_lvol *snapshot,
		   const char *export_uuid, uint32_t ttl_sec,
		   char *uuid_out, size_t uuid_out_len,
		   s3lvol_export_cb cb_fn, void *cb_arg)
{
	struct export_ctx *ctx;
	struct spdk_uuid uuid;

	if (!lvs || !snapshot || !snapshot->blob) {
		return -EINVAL;
	}
	if (snapshot->lvol_store != s3lvol_lvstore_get_lvs(lvs)) {
		SPDK_ERRLOG("lvol '%s' does not belong to lvstore '%s'\n",
			    snapshot->name, s3lvol_lvstore_get_name(lvs));
		return -EINVAL;
	}

	/* Only a snapshot, deliberately.
	 *
	 * A writable volume has no consistent point in time to describe, and the
	 * uuids a zero-copy export records stop being true the moment somebody
	 * writes to those clusters. This used to snapshot on the caller's behalf,
	 * which put a blob freeze and three metadata syncs inside the handoff, gave
	 * the export a snapshot nobody asked for, and left it behind on every
	 * failure path. Whoever schedules the handoff already knows when the volume
	 * is quiet; taking the snapshot is their call, and taking it early is what
	 * makes the drain below usually unnecessary. */
	if (!spdk_blob_is_read_only(snapshot->blob)) {
		SPDK_ERRLOG("'%s' is a writable volume; export takes a snapshot. "
			    "Create one first.\n", snapshot->name);
		return -EINVAL;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		/* Logged, like every other refusal here, because the caller is only
		 * handed the errno: rcow_export_snapshot answers with whatever
		 * spdk_strerror makes of it, and "Cannot allocate memory" on its own
		 * does not say which export never started. */
		SPDK_ERRLOG("Out of memory starting an export of snapshot '%s'\n",
			    snapshot->name);
		return -ENOMEM;
	}
	ctx->lvs      = lvs;
	ctx->snapshot = snapshot;
	ctx->cb_fn    = cb_fn;
	ctx->cb_arg   = cb_arg;

	/* Filled in here rather than in export_fill_src, which only runs once the
	 * drain is past: everything that reports a failure names the snapshot, and
	 * the drain is one of the things that can fail. */
	snprintf(ctx->info.snapshot_name, sizeof(ctx->info.snapshot_name), "%s",
		 snapshot->name);

	/* Recorded as an absolute deadline, not a duration: the manifest outlives
	 * this process, and "an hour from whenever you happen to read this" is not a
	 * promise anybody can keep. 0 means never, which is what a dense export gets
	 * since it pins nothing. */
	if (ttl_sec != 0) {
		ctx->expires_at = (uint64_t)time(NULL) + ttl_sec;
	}

	if (export_uuid && export_uuid[0] != '\0') {
		if (spdk_uuid_parse(&uuid, export_uuid) != 0) {
			SPDK_ERRLOG("export_id '%s' is not a uuid\n", export_uuid);
			free(ctx);
			return -EINVAL;
		}
		snprintf(ctx->info.export_uuid, sizeof(ctx->info.export_uuid), "%s",
			 export_uuid);
	} else {
		/* Without an export_id the call is idempotent per snapshot: if the
		 * snapshot already has an export in flight, answer that uuid and
		 * start nothing. Two concurrent exports of one snapshot would both
		 * drain it and publish a near-identical manifest, for no caller
		 * that asked for two. An explicit export_id keeps its own, stricter
		 * semantics below (same uuid refused). */
		struct export_inflight *inf;

		inf = export_inflight_find_by_snapshot(lvs, snapshot->name);
		if (inf) {
			if (uuid_out && uuid_out_len > 0) {
				snprintf(uuid_out, uuid_out_len, "%s",
					 inf->uuid_str);
			}
			free(ctx);
			return 0;
		}

		spdk_uuid_generate(&uuid);
		spdk_uuid_fmt_lower(ctx->info.export_uuid,
				    sizeof(ctx->info.export_uuid), &uuid);
	}

	/* An explicit export_id makes an export idempotent, which also makes it
	 * possible to ask for one that is already running. Two exports sharing a
	 * uuid would write the same manifest key and leave the in-flight record
	 * ambiguous, so the second one is refused instead. */
	if (export_inflight_find(ctx->info.export_uuid)) {
		SPDK_ERRLOG("export %s is already in progress\n",
			    ctx->info.export_uuid);
		free(ctx);
		return -EEXIST;
	}

	/* Registered before the drain, which can complete synchronously: the export
	 * has to be observable, and the snapshot protected, from here on. */
	if (export_inflight_add(lvs, ctx->info.export_uuid, snapshot->name) != 0) {
		SPDK_ERRLOG("Export %s of snapshot '%s' could not be registered as in "
			    "flight; out of memory\n", ctx->info.export_uuid,
			    snapshot->name);
		free(ctx);
		return -ENOMEM;
	}

	/* Hand the uuid back before the export finishes: the caller owns the
	 * identifier from this moment and may poll it with rcow_get_snapshot_status
	 * while the drain/walk runs in the background. Read from ctx before the
	 * drain, which may free it. */
	if (uuid_out && uuid_out_len > 0) {
		snprintf(uuid_out, uuid_out_len, "%s", ctx->info.export_uuid);
	}

	export_drain(ctx);
	return 0;
}

/* Whether the snapshot an export names can be deleted right now.
 *
 * Mirrors every refusal in s3lvol_lvol_destroy, in the same order, because a
 * "YES" that the delete path then rejects is worse than no answer at all:
 *
 *   1. an export still being written names it
 *   2. a live zero-copy export references it (a dense one owns its own copies,
 *      and an expired reference no longer pins anything)
 *   3. a decouple is running on it (action_in_progress)
 *   4. it is active, i.e. exported over NVMf -- rcow_delete_lvol refuses that
 *      before it gets anywhere near the blob
 *   5. it has more than one clone -- blobstore merges a snapshot into one only
 *
 * The export and active checks come before the "no blob" shortcut on purpose: a
 * deactivated snapshot has no open blob but can still be pinned by an export, and
 * reporting YES for it made --retry-pending attempt a delete that then failed
 * with EBUSY. Only a snapshot that is genuinely absent blocks nothing.
 *
 * Computed on the spot every time; nothing here is cached. */
static bool
export_snapshot_deletable(struct s3lvol_lvstore *lvs, const char *snapshot_name)
{
	struct s3lvol_lvstore *owner;
	struct spdk_lvol *lvol;
	size_t clone_count = 0;
	int rc;

	lvol = s3lvol_lvol_find(lvs, snapshot_name);
	if (!lvol) {
		/* Gone: nothing left to refuse. */
		return true;
	}

	owner = s3lvol_lvstore_of_lvol(lvol);
	if (!owner) {
		return false;
	}

	if (s3lvol_export_inflight_pinning(owner, lvol->name)) {
		return false;
	}

	if (s3lvol_export_pinning(owner, lvol->name)) {
		return false;
	}

	/* Both of these refuse in s3lvol_lvol_destroy() / rpc_rcow_delete_lvol()
	 * without ever looking at the blob, so they have to be checked even when
	 * the blob is closed. */
	if (lvol->action_in_progress) {
		return false;
	}

	if (s3lvol_active_load() == 0 && s3lvol_active_find(lvol->name)) {
		return false;
	}

	if (!lvol->blob) {
		/* Closed: the clone count below is unreadable, and every blocker
		 * that does not need the blob has been checked above. */
		return true;
	}

	/* ids == NULL makes spdk_blob_get_clones report the count in *clone_count
	 * and return -ENOMEM; both 0 and -ENOMEM leave the count valid. */
	rc = spdk_blob_get_clones(lvol->lvol_store->blobstore, lvol->blob_id,
				  NULL, &clone_count);
	if (rc != 0 && rc != -ENOMEM) {
		/* Unknown is treated as not deletable: better a false "no" than
		 * deleting a snapshot blobstore cannot merge. */
		return false;
	}
	return clone_count <= 1;
}

int
s3lvol_export_query(const char *export_uuid,
		    enum s3lvol_export_state *state, bool *deletable)
{
	struct export_inflight *inf;
	struct s3lvol_export *exp;
	struct s3lvol_lvstore *lvs;

	if (!export_uuid || export_uuid[0] == '\0' || !state || !deletable) {
		return -EINVAL;
	}

	/* In flight before durable: the registry has not learned it yet. */
	inf = export_inflight_find(export_uuid);
	if (inf) {
		*state = S3LVOL_EXPORT_STATE_INPROGRESS;
		/* Not merely advisory: s3lvol_lvol_destroy consults the same
		 * in-flight record and refuses too. */
		*deletable = false;
		return 0;
	}

	for (lvs = s3lvol_lvstore_first(); lvs; lvs = s3lvol_lvstore_next(lvs)) {
		struct s3lvol_export_entry entry;

		exp = s3lvol_export_find(lvs, export_uuid);
		if (exp) {
			s3lvol_export_get(exp, &entry);
			*state = S3LVOL_EXPORT_STATE_DONE;
			*deletable = export_snapshot_deletable(lvs, entry.snapshot);
			return 0;
		}
	}

	*state = S3LVOL_EXPORT_STATE_NONE;
	*deletable = true;
	return 0;
}

/* The export state to report for a snapshot rather than for one uuid.
 *
 * A snapshot can be exported more than once, so the states are folded together:
 * an export still being written outranks a finished one, since that is the state
 * the caller has to wait out. NONE means no export names this snapshot -- for a
 * snapshot that exists, an answer rather than an error. */
static enum s3lvol_export_state
snapshot_export_state(struct s3lvol_lvstore *lvs, const char *snapshot_name)
{
	struct s3lvol_export *exp;
	bool found = false;

	if (s3lvol_export_inflight_pinning(lvs, snapshot_name)) {
		return S3LVOL_EXPORT_STATE_INPROGRESS;
	}

	/* Not s3lvol_export_pinning: that one answers whether the snapshot is held,
	 * so it skips dense and expired exports. Those are still exports that were
	 * made, hence still DONE. */
	for (exp = s3lvol_export_first(lvs); exp; exp = s3lvol_export_next(exp)) {
		struct s3lvol_export_entry entry;

		s3lvol_export_get(exp, &entry);
		if (strcmp(entry.snapshot, snapshot_name) == 0) {
			found = true;
			break;
		}
	}

	return found ? S3LVOL_EXPORT_STATE_DONE : S3LVOL_EXPORT_STATE_NONE;
}

int
s3lvol_snapshot_query_lvol(struct spdk_lvol *lvol,
			   enum s3lvol_export_state *state, bool *deletable,
			   bool *pending)
{
	struct s3lvol_lvstore *owner;

	if (!lvol || !state || !deletable || !pending) {
		return -EINVAL;
	}

	owner = s3lvol_lvstore_of_lvol(lvol);
	if (!owner) {
		return -ENODEV;
	}

	*state = snapshot_export_state(owner, lvol->name);
	*deletable = export_snapshot_deletable(owner, lvol->name);
	*pending = lvol->lvol_store != NULL &&
		   s3lvol_snapshot_pending_test(&lvol->lvol_store->uuid,
						&lvol->uuid);
	return 0;
}

int
s3lvol_snapshot_query(const char *snapshot_name,
		      enum s3lvol_export_state *state, bool *deletable,
		      bool *pending)
{
	struct spdk_lvol *lvol;

	if (!snapshot_name || snapshot_name[0] == '\0') {
		return -EINVAL;
	}

	/* Searched across every loaded lvstore for the same reason the uuid form is:
	 * the caller names a volume, not a place to look for one. */
	lvol = s3lvol_lvol_find_any(snapshot_name);
	if (!lvol) {
		return -ENODEV;
	}

	return s3lvol_snapshot_query_lvol(lvol, state, deletable, pending);
}

/* ==========================================================================
 * Import
 * ========================================================================== */

struct import_ctx {
	struct s3lvol_lvstore     *lvs;
	char                       lvol_name[SPDK_LVOL_NAME_MAX];
	char                       uuid_str[SPDK_UUID_STRING_LEN];

	struct s3_target           src;
	char                       endpoint[S3_EXPORT_ENDPOINT_MAX];
	char                       region[S3_EXPORT_NAME_MAX];
	char                       bucket[S3_EXPORT_BUCKET_MAX];
	bool                       path_style;
	bool                       verify_tls;

	struct s3_client          *client;
	char                       key[S3_EXPORT_KEY_MAX];
	uint64_t                   size;
	char                      *body;

	struct s3_export_manifest *m;
	struct s3lvol_import      *imp;
	bool                       decouple;

	s3lvol_lvol_op_cb          cb_fn;
	void                      *cb_arg;
};

static void
import_report(struct import_ctx *ctx, struct spdk_lvol *lvol, int status)
{
	s3lvol_lvol_op_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;

	if (status != 0 && ctx->imp) {
		/* Undo the registry entry. Leaving it would be harmless at runtime --
		 * it is only a cached manifest -- but it would advertise an import
		 * that does not exist, and release_export refuses to run while one
		 * is advertised. */
		import_remove(ctx->imp);
		if (imports_save(ctx->lvs, NULL, NULL) != 0) {
			SPDK_WARNLOG("Could not rewrite the imports registry of '%s' "
				     "after a failed import; it now lists an export "
				     "nothing uses\n",
				     s3lvol_lvstore_get_name(ctx->lvs));
		}
	}

	if (ctx->client) {
		s3_client_put(ctx->client);
	}
	s3_export_manifest_unref(ctx->m);
	free(ctx->body);
	free(ctx);

	if (cb_fn) {
		cb_fn(cb_arg, lvol, status);
	}
}

static void
import_clone_done(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct import_ctx *ctx = cb_arg;
	int rc;

	if (lvolerrno != 0) {
		SPDK_ERRLOG("Failed to create an esnap clone of '%s': %s\n",
			    ctx->uuid_str, spdk_strerror(-lvolerrno));
		import_report(ctx, NULL, lvolerrno);
		return;
	}

	rc = vbdev_s3lvol_bdev_register(lvol, s3lvol_lvstore_get_name(ctx->lvs));
	if (rc != 0) {
		SPDK_ERRLOG("Imported lvol '%s' exists but its bdev could not be "
			    "registered (%d). The clone is intact -- re-attach the "
			    "lvstore to expose it.\n", lvol->name, rc);
		/* Deliberately does not remove the registry entry: the clone is real
		 * and its blob metadata now names this export, so the manifest has to
		 * stay reachable or the next attach fails. */
		ctx->imp = NULL;
		import_report(ctx, NULL, rc);
		return;
	}

	SPDK_NOTICELOG("Imported export %s as lvol '%s': %" PRIu64 " bytes, "
		       "%" PRIu64 " of %" PRIu64 " chunk(s) present\n",
		       ctx->uuid_str, lvol->name, ctx->m->size_bytes,
		       ctx->m->present_chunks, ctx->m->num_chunks);

	/* Started before answering, so that a caller who asked for it cannot see the
	 * import succeed and then find no decouple in the list. Not waited for: the
	 * volume is usable now, and the decouple is about the export rather than about
	 * availability. Its outcome goes to the log -- an import that reported success
	 * did succeed, whatever becomes of the copying afterwards. */
	if (ctx->decouple) {
		int drc = s3lvol_lvol_decouple(ctx->lvs, lvol, NULL, NULL);

		if (drc != 0) {
			SPDK_WARNLOG("Imported '%s' but could not start decoupling it from "
				     "%s: %s. The volume works; it still reads through to "
				     "the export.\n", lvol->name, ctx->uuid_str,
				     spdk_strerror(-drc));
		}
	}

	import_report(ctx, lvol, 0);
}

static void
import_registry_saved(void *cb_arg, int status)
{
	struct import_ctx *ctx = cb_arg;
	int rc;

	if (status != 0) {
		import_report(ctx, NULL, status);
		return;
	}

	/* Only now: the registry has to be durable *before* a blob exists that
	 * refers to this export. The other order leaves a clone that no attach can
	 * open, because the esnap id in its metadata resolves to nothing. */
	rc = spdk_lvol_create_esnap_clone(ctx->uuid_str,
					 (uint32_t)strlen(ctx->uuid_str),
					 ctx->m->size_bytes,
					 s3lvol_lvstore_get_lvs(ctx->lvs),
					 ctx->lvol_name, import_clone_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("spdk_lvol_create_esnap_clone failed for '%s': %s\n",
			    ctx->lvol_name, spdk_strerror(-rc));
		import_report(ctx, NULL, rc);
	}
}

/* The local-clone path's completion. Separate from import_clone_done() because
 * almost everything that one does does not apply: there is no manifest-derived
 * chunk count to report, no import registry entry to roll back, and no decouple to
 * kick off. What remains is reporting. */
static void
import_local_clone_done(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct import_ctx *ctx = cb_arg;

	if (lvolerrno != 0) {
		SPDK_ERRLOG("cloning '%s' locally for export %s failed: %s\n",
			    ctx->m->src.snapshot, ctx->uuid_str,
			    spdk_strerror(-lvolerrno));
		import_report(ctx, NULL, lvolerrno);
		return;
	}

	SPDK_NOTICELOG("Imported export %s as lvol '%s': local clone of snapshot "
		       "'%s', no dependency on the export\n", ctx->uuid_str,
		       lvol->name, ctx->m->src.snapshot);

	import_report(ctx, lvol, 0);
}

/* Is this export's source the very snapshot this lvstore still holds?
 *
 * Exporting and importing within one lvstore turns out to be a normal thing to do
 * -- it is how you get a writable copy of a snapshot through the same API used for
 * a handoff -- and doing it through an esnap clone is both slower and more fragile
 * than it needs to be. A same-bucket export is REF layout by default, so the
 * manifest references the source's *live* chunk objects rather than copies of them;
 * measured, a self-import built that way stops reading after the lvstore is
 * unloaded and attached again. A local clone has no such dependency: blobstore
 * refuses to delete a snapshot that has clones, so the parent is pinned by the
 * clone relationship itself rather than by an export that can be released or can
 * expire (s3lvol_export_pinning() stops pinning once a TTL passes).
 *
 * The export side is deliberately untouched: whether a manifest will be consumed
 * here or on another node is the caller's business and cannot be known when it is
 * written. So the degeneration happens here, on the import, where the answer is
 * observable.
 *
 * Returns the local parent to clone, or NULL to take the esnap path.
 *
 * All three conditions matter:
 *
 *   - endpoint, bucket and prefix must all match. The prefix alone is the lvstore
 *     name, and the same name can exist in another bucket or behind another
 *     endpoint; matching on it alone would clone the wrong volume's snapshot.
 *   - the named snapshot must still exist here and still be read-only.
 *     s3lvol_lvol_create_clone() requires a read-only parent, and a writable one
 *     would mean parent and clone could both modify shared clusters.
 *   - the blob id must match. This is the one that is easy to leave out and worst
 *     to get wrong: a snapshot can be deleted and another created under the same
 *     name, and then the name resolves to a blob that is not what was exported.
 *     Cloning it would hand back data the caller never asked for, silently. The
 *     name is a label; the blob id is the identity.
 *
 * Any condition failing means the esnap path, which is always correct -- just more
 * expensive. That covers the case the DENSE layout exists for: the source snapshot
 * is gone, and the export's own copy of the data is the only thing left.
 */
static struct spdk_lvol *
import_local_parent(struct import_ctx *ctx)
{
	const struct s3_export_source *src = &ctx->m->src;
	const struct s3_target *self_tgt;
	struct spdk_lvol *parent;
	const char *self_ns;

	if (src->snapshot[0] == '\0' || src->prefix[0] == '\0') {
		/* An older manifest, from before these were recorded. Nothing to
		 * match against, so there is nothing to prove. */
		return NULL;
	}

	if (strcmp(src->prefix, s3lvol_lvstore_get_name(ctx->lvs)) != 0) {
		return NULL;
	}

	self_ns = s3lvol_lvstore_get_namespace(ctx->lvs);
	self_tgt = self_ns ? rcow_namespace_to_target(self_ns) : NULL;
	if (!self_tgt || !self_tgt->bucket || !self_tgt->endpoint) {
		return NULL;
	}
	if (strcmp(src->bucket, self_tgt->bucket) != 0 ||
	    strcmp(src->endpoint, self_tgt->endpoint) != 0) {
		/* Same lvstore name, different bucket or endpoint: a different
		 * lvstore that happens to share a name. */
		return NULL;
	}

	parent = s3lvol_lvol_find(ctx->lvs, src->snapshot);
	if (!parent || !parent->blob) {
		SPDK_NOTICELOG("export %s came from this lvstore, but its snapshot "
			       "'%s' is gone; reading through the export instead\n",
			       ctx->uuid_str, src->snapshot);
		return NULL;
	}
	if (!spdk_blob_is_read_only(parent->blob)) {
		SPDK_WARNLOG("export %s names '%s' as its source, but that lvol is "
			     "writable now; reading through the export instead\n",
			     ctx->uuid_str, src->snapshot);
		return NULL;
	}
	/* Identity, and the reason it is the uuid rather than the blob id.
	 *
	 * blob_id looked like an identity and is not one: blobstore derives it from
	 * the lowest free metadata page, so deleting a snapshot and creating another
	 * returns the same id. A snapshot deleted and recreated under the same name
	 * matched on name *and* blob_id here, and this function cloned the
	 * replacement -- silently, with the right name and a plausible size. The
	 * test caught it; the reasoning that led to checking blob_id did not.
	 *
	 * An empty recorded uuid means the manifest predates the field, and that is
	 * "cannot prove identity", not "matches". Taking the esnap path then is the
	 * safe answer: correct, just more expensive. */
	if (src->snapshot_uuid[0] == '\0') {
		SPDK_NOTICELOG("export %s predates snapshot uuids being recorded, so "
			       "'%s' cannot be identified with certainty; reading "
			       "through the export instead\n", ctx->uuid_str,
			       src->snapshot);
		return NULL;
	}
	if (strcmp(parent->uuid_str, src->snapshot_uuid) != 0) {
		SPDK_WARNLOG("export %s names snapshot '%s' with uuid %s, but the lvol "
			     "with that name here is %s; it was replaced, so reading "
			     "through the export instead\n", ctx->uuid_str,
			     src->snapshot, src->snapshot_uuid, parent->uuid_str);
		return NULL;
	}

	/* Corroboration only. With the uuid matched this should never fire, so if it
	 * does, something is wrong that guessing about is not going to fix. */
	if (src->blob_id != 0 && spdk_blob_get_id(parent->blob) != src->blob_id) {
		SPDK_ERRLOG("export %s: snapshot '%s' matches by uuid %s but not by "
			    "blob id (have 0x%" PRIx64 ", recorded 0x%" PRIx64 "); "
			    "refusing to clone it locally\n", ctx->uuid_str,
			    src->snapshot, src->snapshot_uuid,
			    spdk_blob_get_id(parent->blob), src->blob_id);
		return NULL;
	}

	return parent;
}

static void
import_got_manifest(void *cb_arg, uint64_t bytes_read, int status)
{
	struct import_ctx *ctx = cb_arg;
	struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(ctx->lvs);
	uint64_t cluster_size = spdk_bs_get_cluster_size(store->blobstore);
	struct spdk_lvol *parent;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("Failed to read the manifest '%s': %s\n", ctx->key,
			    spdk_strerror(-status));
		import_report(ctx, NULL, status);
		return;
	}

	rc = s3_export_manifest_parse(ctx->body, bytes_read, &ctx->m);
	if (rc != 0) {
		import_report(ctx, NULL, rc);
		return;
	}

	/* The one hard geometry check, and it is about *this* lvstore rather than
	 * the source: spdk_lvol_create_esnap_clone() requires the parent's size to
	 * be a whole number of the destination's clusters.
	 *
	 * The source's cluster size is deliberately not required to match. The
	 * manifest is self-describing and the reader slices its own chunks, so
	 * nothing downstream cares what the exporting node's clusters were --
	 * unlike the zero-copy design, where a mismatch would have misaddressed
	 * every object. */
	if (ctx->m->size_bytes % cluster_size != 0) {
		SPDK_ERRLOG("export %s is %" PRIu64 " bytes, not a multiple of this "
			    "lvstore's %" PRIu64 "-byte cluster\n", ctx->uuid_str,
			    ctx->m->size_bytes, cluster_size);
		import_report(ctx, NULL, -EINVAL);
		return;
	}
	if (ctx->m->cluster_size != 0 && ctx->m->cluster_size != cluster_size) {
		SPDK_WARNLOG("export %s came from an lvstore with %u-byte clusters, "
			     "this one has %" PRIu64 "; that is supported but the "
			     "clone's copy-on-write granularity will differ from the "
			     "source's\n", ctx->uuid_str, ctx->m->cluster_size,
			     cluster_size);
	}

	/* Before the registry: a local clone records nothing there, which is the
	 * point. Nothing has been written or allocated yet either, so taking this
	 * branch leaves no trace of the esnap path having been considered. */
	parent = import_local_parent(ctx);
	if (parent) {
		SPDK_NOTICELOG("export %s is this lvstore's own snapshot '%s'; "
			       "cloning it locally instead of reading through the "
			       "export\n", ctx->uuid_str, ctx->m->src.snapshot);

		if (ctx->decouple) {
			/* Not an error, and not silently ignored either. There is no
			 * export to decouple from: the clone's parent is a local
			 * snapshot, and breaking that link is what
			 * rcow_decouple_lvol does not do -- it copies out what an
			 * export holds. A local clone is already independent of S3
			 * in the sense the caller cares about. */
			SPDK_NOTICELOG("ignoring decouple for '%s': a local clone "
				       "has no export to decouple from\n",
				       ctx->lvol_name);
		}

		rc = s3lvol_lvol_create_clone(ctx->lvs, parent, ctx->lvol_name,
					      import_local_clone_done, ctx);
		if (rc != 0) {
			SPDK_ERRLOG("cannot clone '%s' locally: %s\n",
				    ctx->m->src.snapshot, spdk_strerror(-rc));
			import_report(ctx, NULL, rc);
		}
		return;
	}

	/* A second volume from the same export is a normal thing to want: one export
	 * of a sealed rootfs, several sandboxes started from it on this node. The
	 * registry is keyed by (lvstore, export uuid) and holds a cached manifest, so
	 * one entry already describes any number of clones -- what it records is that
	 * this lvstore depends on the export, not which volume does.
	 *
	 * So the entry is shared rather than duplicated, and the S3 object is left
	 * alone: it is already durable and its contents would not change. That also
	 * skips a PUT on every import after the first.
	 *
	 * ctx->imp stays NULL on this path, which is the existing signal to
	 * import_report() that it must not undo the entry -- the volume that imported
	 * first still reads through it, and removing it would leave that volume
	 * unopenable on the next attach.
	 */
	if (import_find(ctx->lvs, ctx->uuid_str)) {
		SPDK_NOTICELOG("lvstore '%s' already has export %s on record; "
			       "reusing it for '%s'\n",
			       s3lvol_lvstore_get_name(ctx->lvs), ctx->uuid_str,
			       ctx->lvol_name);
		import_registry_saved(ctx, 0);
		return;
	}

	ctx->imp = import_add(ctx->lvs, ctx->m, ctx->path_style, ctx->verify_tls);
	if (!ctx->imp) {
		import_report(ctx, NULL, -ENOMEM);
		return;
	}

	rc = imports_save(ctx->lvs, import_registry_saved, ctx);
	if (rc != 0) {
		import_report(ctx, NULL, rc);
	}
}

static void
import_head_done(void *cb_arg, int status)
{
	struct import_ctx *ctx = cb_arg;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("Cannot read the manifest '%s' from bucket '%s': %s. "
			    "Check the uuid, the source prefix, and that this node's "
			    "credentials can read that bucket.\n",
			    ctx->key, ctx->bucket, spdk_strerror(-status));
		import_report(ctx, NULL, status);
		return;
	}
	if (ctx->size == 0 || ctx->size > 64u * 1024 * 1024) {
		SPDK_ERRLOG("manifest '%s' has an implausible size of %" PRIu64
			    " bytes\n", ctx->key, ctx->size);
		import_report(ctx, NULL, -EINVAL);
		return;
	}

	ctx->body = malloc(ctx->size);
	if (!ctx->body) {
		import_report(ctx, NULL, -ENOMEM);
		return;
	}

	rc = s3_get_range(ctx->client, ctx->key, 0, ctx->size, ctx->body,
			  import_got_manifest, ctx);
	if (rc != 0) {
		import_report(ctx, NULL, rc);
	}
}

int
s3lvol_lvol_import(struct s3lvol_lvstore *lvs, const struct s3lvol_import_opts *opts,
		   s3lvol_lvol_op_cb cb_fn, void *cb_arg)
{
	struct import_ctx *ctx;
	struct spdk_uuid uuid;
	int rc;

	if (!lvs || !opts || !opts->lvol_name || !opts->export_uuid) {
		return -EINVAL;
	}
	if (spdk_uuid_parse(&uuid, opts->export_uuid) != 0) {
		SPDK_ERRLOG("export_uuid '%s' is not a uuid\n", opts->export_uuid);
		return -EINVAL;
	}
	if (s3lvol_lvol_find(lvs, opts->lvol_name)) {
		SPDK_ERRLOG("lvstore '%s' already has an lvol named '%s'\n",
			    s3lvol_lvstore_get_name(lvs), opts->lvol_name);
		return -EEXIST;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->lvs    = lvs;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;
	ctx->decouple = opts->decouple;
	snprintf(ctx->lvol_name, sizeof(ctx->lvol_name), "%s", opts->lvol_name);
	snprintf(ctx->uuid_str, sizeof(ctx->uuid_str), "%s", opts->export_uuid);

	/* Resolve the namespace the manifest lives in. When the caller does not say,
	 * it is this lvstore's own -- the manifest sits at the top of the bucket, so
	 * within one bucket the uuid is the whole address. Only a handoff that crosses
	 * buckets needs to name one. */
	const char *src_ns = opts->src_namespace;
	const struct s3_target *src_tgt;

	if (!src_ns) {
		src_ns = s3lvol_lvstore_get_namespace(lvs);
	}
	src_tgt = rcow_namespace_to_target(src_ns);
	if (!src_tgt) {
		free(ctx);
		return -ENOENT;
	}

	snprintf(ctx->endpoint, sizeof(ctx->endpoint), "%s",
		 src_tgt->endpoint ? src_tgt->endpoint : "");
	snprintf(ctx->region, sizeof(ctx->region), "%s",
		 src_tgt->region ? src_tgt->region : "");
	snprintf(ctx->bucket, sizeof(ctx->bucket), "%s",
		 src_tgt->bucket ? src_tgt->bucket : "");
	ctx->path_style = src_tgt->use_path_style;
	ctx->verify_tls = src_tgt->verify_tls;

	/* No prefix: the client is used for the manifest, whose key is bucket-level.
	 * Where the *data* lives comes out of the manifest itself, which is what makes
	 * the exporting lvstore's name none of the importer's business. */
	ctx->src.endpoint       = ctx->endpoint;
	ctx->src.region         = ctx->region;
	ctx->src.bucket         = ctx->bucket;
	ctx->src.prefix         = NULL;
	ctx->src.use_path_style = ctx->path_style;
	ctx->src.verify_tls     = ctx->verify_tls;
	ctx->src.auth_mode      = S3_AUTH_ENV;

	rc = s3_client_get_or_create(&ctx->src, &ctx->client);
	if (rc != 0) {
		free(ctx);
		return rc;
	}

	s3_export_manifest_key(ctx->uuid_str, ctx->key, sizeof(ctx->key));

	rc = s3_head(ctx->client, ctx->key, &ctx->size, import_head_done, ctx);
	if (rc != 0) {
		s3_client_put(ctx->client);
		free(ctx);
	}
	return rc;
}

/* ==========================================================================
 * Decouple
 *
 * The importer's way out of depending on the exporting node: materialise what the
 * export actually holds, then stop being its clone. Both halves need an API the
 * upstream blobstore does not have, which is what patches/0002 and 0003 add.
 *
 * === Not spdk_lvol_decouple_parent() ===
 *
 * The name is deliberate -- this is the same intent -- but it is not that call and
 * cannot be. For an esnap clone spdk_lvol_decouple_parent() is identical to a full
 * inflate: bs_inflate_blob_open_cpl() overrides allocate_all to true for such a
 * blob (blobstore.c:7287), so "decouple, keeping it thin" is exactly the thing the
 * public API cannot express.
 *
 * === Why not inflate ===
 *
 * spdk_bs_inflate_blob() would do this in one call, and it was what this used to
 * do. For an esnap clone it allocates *every* cluster -- blobstore forces
 * allocate_all and bs_cluster_needs_allocation() returns true for all of them --
 * so the cost follows the provisioned size rather than the data, and the volume
 * comes out thick. A 16 TiB volume holding 10 GiB is16.7 million cluster
 * allocations and needs 16 TiB of free clusters to succeed at all.
 *
 * Here the manifest says which chunks exist, so only the clusters covering them
 * are copied. Everything else stays a hole, and the volume stays thin. The cost
 * is the data, once.
 *
 * === What it costs the volume ===
 *
 * Nothing, in availability terms: the volume is readable and writable throughout.
 * blobstore's IO path does not consult locked_operation_in_progress, and the
 * copies run on their own channel. What a caller does notice is that snapshot,
 * clone, resize and delete are refused while this runs -- see the
 * action_in_progress check in the delete and derive paths -- and that the last
 * step freezes IO for the duration of one metadata write.
 *
 * === Interrupted runs ===
 *
 * Resumable, and that is not incidental: a crash halfway leaves a volume that is
 * still an esnap clone, with the clusters it did materialise already allocated,
 * and a registry entry that still names the export. Running decouple again skips
 * those clusters (spdk_blob_materialize_cluster() reports an allocated cluster as
 * done) and finishes the rest. The one order that matters is that the parent is
 * cleared *last*: clearing it first would turn every unfinished cluster into
 * zeroes, silently.
 * ========================================================================== */

struct s3lvol_decouple {
	struct s3lvol_lvstore     *lvs;
	struct spdk_lvol          *lvol;
	char                       lvol_name[SPDK_LVOL_NAME_MAX];
	char                       uuid_str[SPDK_UUID_STRING_LEN];

	/* Referenced for the duration. The registry entry it came from is dropped at
	 * the end of a successful run, and that drops the registry's own reference. */
	struct s3_export_manifest *m;
	struct spdk_io_channel    *channel;

	uint64_t                   cluster_size;
	uint32_t                   chunk_size;

	uint64_t                   num_clusters;
	uint64_t                   cluster;   /* next one to consider */
	uint64_t                   total;     /* how many need materialising */
	uint64_t                   done;

	int                        status;

	spdk_lvol_op_complete      cb_fn;
	void                      *cb_arg;

	TAILQ_ENTRY(s3lvol_decouple) link;
};

static TAILQ_HEAD(, s3lvol_decouple) g_decouples = TAILQ_HEAD_INITIALIZER(g_decouples);

/* True when the manifest has no object anywhere under this cluster, i.e. the
 * parent would serve it as zeroes and there is nothing to copy.
 *
 * The two sizes are deliberately not assumed to be equal: an export carries its
 * own chunk size and an lvstore its own cluster size, and import only requires
 * that the volume be a whole number of clusters. So the range is computed in
 * chunk indices from the cluster's byte range, which is correct either way round.
 */
static bool
decouple_cluster_is_hole(const struct s3lvol_decouple *d, uint64_t cluster)
{
	uint64_t first = (cluster * d->cluster_size) / d->chunk_size;
	uint64_t last = ((cluster + 1) * d->cluster_size - 1) / d->chunk_size;

	return s3_export_manifest_range_is_zeroes(d->m, first, last - first + 1);
}

static void decouple_start_next_queued(void);

static void
decouple_finish(struct s3lvol_decouple *d)
{
	spdk_lvol_op_complete cb_fn = d->cb_fn;
	void *cb_arg = d->cb_arg;
	int status = d->status;

	if (status == 0) {
		SPDK_NOTICELOG("lvol '%s' no longer reads export %s: %" PRIu64
			       " cluster(s) materialised\n", d->lvol_name, d->uuid_str,
			       d->done);
	} else {
		SPDK_ERRLOG("Decoupling lvol '%s' from export %s failed after %" PRIu64
			    " of %" PRIu64 " cluster(s): %s. The volume still reads "
			    "through to the export and can be decoupled again.\n",
			    d->lvol_name, d->uuid_str, d->done, d->total,
			    spdk_strerror(-status));
	}

	if (d->lvol) {
		d->lvol->action_in_progress = false;
	}
	if (d->channel) {
		spdk_bs_free_io_channel(d->channel);
	}
	s3_export_manifest_unref(d->m);
	TAILQ_REMOVE(&g_decouples, d, link);
	free(d);

	/* After the removal, so decouple_find_blocker() no longer answers with the run
	 * that has just ended, and before this run's own callback -- a caller that
	 * decouples in a callback then queues behind the right thing. */
	decouple_start_next_queued();

	if (cb_fn) {
		cb_fn(cb_arg, status);
	}
}

static void
decouple_cleared(void *cb_arg, int bserrno)
{
	struct s3lvol_decouple *d = cb_arg;

	if (bserrno != 0) {
		d->status = bserrno;
		decouple_finish(d);
		return;
	}

	/* Only now is the export unreferenced, so only now may the registry stop
	 * carrying its manifest -- which is also what lets release_export delete the
	 * objects. Ordered this way because the reverse leaves a blob naming an
	 * export that no attach can resolve. */
	s3lvol_imports_recheck(d->lvs, d->uuid_str);
	decouple_finish(d);
}

static void decouple_next(struct s3lvol_decouple *d);

static void
decouple_next_msg(void *arg)
{
	decouple_next(arg);
}

static void
decouple_cluster_done(void *cb_arg, int bserrno)
{
	struct s3lvol_decouple *d = cb_arg;

	if (bserrno != 0) {
		d->status = bserrno;
		decouple_finish(d);
		return;
	}

	/* Success from spdk_blob_materialize_cluster() means the cluster is allocated
	 * and holds the parent's bytes, so this asks the blob whether that is true
	 * before counting it. Not a redundant assertion: it used to be untrue.
	 * materialize_cluster() hands blobstore a zero-length read as the operation to
	 * re-execute once a cluster is in place, and bs_allocate_and_copy_cluster()
	 * queues that operation, doing nothing else, whenever the channel already has
	 * an allocation in flight -- which two decouples on one lvstore always do,
	 * since a channel is per thread and per blobstore. The re-executed zero-length
	 * read then completed with 0 having copied nothing, every cluster was counted,
	 * and the volume was detached from an export it had never read: an unmountable
	 * filesystem out of a decouple that reported success.
	 *
	 * That is fixed where it belongs, in the API. This stays because the cost of
	 * being wrong here is a volume whose data is gone for good, while the cost of
	 * the check is one array lookup per cluster. */
	uint64_t io_units_per_cluster = d->cluster_size / S3LVOL_BLOCK_SIZE;
	uint64_t cluster = d->cluster - 1;
	uint64_t first_io_unit = cluster * io_units_per_cluster;

	if (spdk_blob_get_next_allocated_io_unit(d->lvol->blob,
						first_io_unit) != first_io_unit) {
		SPDK_ERRLOG("Decoupling lvol '%s' from export %s: cluster %" PRIu64
			    " reported materialised but is not allocated, so nothing "
			    "was copied into it. Refusing to go on; the volume still "
			    "reads through the export.\n",
			    d->lvol_name, d->uuid_str, cluster);
		d->status = -EIO;
		decouple_finish(d);
		return;
	}

	d->done++;

	/* Bounced rather than continued inline. spdk_blob_materialize_cluster()
	 * completes synchronously for a cluster that is already allocated, which is
	 * the normal case when resuming an interrupted run, so a direct call would
	 * recurse once per cluster and run out of stack on a large volume. */
	spdk_thread_send_msg(spdk_get_thread(), decouple_next_msg, d);
}

static void
decouple_next(struct s3lvol_decouple *d)
{
	uint64_t cluster;

	while (d->cluster < d->num_clusters && decouple_cluster_is_hole(d, d->cluster)) {
		d->cluster++;
	}

	if (d->cluster >= d->num_clusters) {
		struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(d->lvs);

		/* Every cluster that was supposed to be copied has to have been copied
		 * before the tie to the export is cut, because cutting it is the point
		 * of no return: after that the volume reads its own clusters and there
		 * is nothing left to fall back to. A cluster that was skipped, or one
		 * whose copy quietly did nothing, becomes zeroes that cannot afterwards
		 * be told apart from a hole the export really had.
		 *
		 * d->total was counted from the manifest before any copying started and
		 * d->done counts the completed ones, so they must agree. They can only
		 * disagree through a bug -- the loop above walks every cluster exactly
		 * once and any error finishes the run instead of arriving here -- which
		 * is the reason to check: the alternative to failing here is silent,
		 * permanent data loss, and the volume is still usable as an esnap clone
		 * if this refuses.
		 */
		if (d->done != d->total) {
			SPDK_ERRLOG("Refusing to detach lvol '%s' from export %s: %"
				    PRIu64 " of %" PRIu64 " cluster(s) with data were "
				    "materialised. Cutting the link now would turn the "
				    "rest into zeroes for good. The volume still reads "
				    "through the export; decouple it again.\n",
				    d->lvol_name, d->uuid_str, d->done, d->total);
			d->status = -EIO;
			decouple_finish(d);
			return;
		}

		spdk_bs_blob_clear_external_parent(store->blobstore,
						   spdk_blob_get_id(d->lvol->blob),
						   decouple_cleared, d);
		return;
	}

	cluster = d->cluster++;

	spdk_blob_materialize_cluster(d->lvol->blob, d->channel, cluster,
				      decouple_cluster_done, d);
}

static struct s3lvol_decouple *
decouple_find(const struct spdk_lvol *lvol)
{
	struct s3lvol_decouple *d;

	TAILQ_FOREACH(d, &g_decouples, link) {
		if (d->lvol == lvol) {
			return d;
		}
	}
	return NULL;
}

/* Whatever a newly requested decouple has to wait for, or NULL if it may start.
 *
 * Two separate reasons, both of which end in the same queue.
 *
 * The same export, on any lvstore. Materialising means fetching every chunk the
 * export holds, so N decouples of one export pull the same gigabytes N times over,
 * competing for the same client and the same bandwidth, and each finishes later
 * than it would have alone. Across lvstores too: what is being rationed is the
 * transfer of a particular export's objects, and two lvstores importing the same
 * export fetch the very same keys.
 *
 * The same lvstore, whatever the export. This one is not about bandwidth but about
 * blobstore: cluster allocation is serialised per io channel, a channel is per
 * thread per blobstore, and every decouple runs on the same thread. So
 * bs_allocate_and_copy_cluster() queues -- "Queue the user op to block other
 * incoming operations" -- for the whole duration of another cluster's allocation,
 * including its read from S3. Two decouples of one lvstore therefore never overlap
 * inside blobstore no matter what they are told to do.
 *
 * Letting them try anyway is worse than making them wait, which is why this
 * function reports the second reason at all. Measured on 43 clusters running
 * underneath 700 on one lvstore: 71 seconds instead of the 3 it takes alone, with
 * its first cluster losing the channel over 500 times in a row. Nothing was
 * gained by the overlap -- the work was serialised regardless -- and what was lost
 * was any bound on how long the smaller volume takes, since it is only ever
 * offered the channel when the larger one happens to be between clusters.
 *
 * Correctness does not depend on this. spdk_blob_materialize_cluster() re-offers a
 * cluster that lost the channel, so the data arrives either way; before that fix
 * it did not, and the volume was detached from an export it had never read. What
 * this adds is that the waiting is now visible in rcow_get_decouple, bounded by
 * the queue rather than by a retry counter, and in the order the requests came in.
 */
static struct s3lvol_decouple *
decouple_find_blocker(const struct s3lvol_lvstore *lvs, const char *uuid_str)
{
	struct s3lvol_decouple *d;

	TAILQ_FOREACH(d, &g_decouples, link) {
		if (strcmp(d->uuid_str, uuid_str) == 0 || d->lvs == lvs) {
			return d;
		}
	}
	return NULL;
}

/* Volumes waiting for their turn.
 *
 * Queued rather than refused, because "import N volumes and decouple them" is one
 * operation from the caller's point of view. Refusing all but the first would leave
 * the rest esnap clones for good unless the caller noticed and asked again -- which
 * means polling rcow_get_decouple and retrying, for a wait it did not ask to
 * manage. So the wait is kept here, and rcow_get_decouple reports queued volumes
 * alongside running ones.
 *
 * FIFO, and released one entry at a time by whatever finishes: see
 * decouple_find_blocker() for what counts as being in the way.
 *
 * Deliberately *not* holding action_in_progress while queued. That flag blocks
 * delete, snapshot and resize, and a queued volume may sit behind a decouple that
 * runs for minutes; making those fail for the whole wait would be worse than the
 * duplicate fetching this avoids. The consequence is that a queued volume can be
 * deleted, and dequeue_lvol() below is how that is noticed.
 */
struct decouple_queued {
	struct s3lvol_lvstore        *lvs;
	struct spdk_lvol             *lvol;
	char                          uuid_str[SPDK_UUID_STRING_LEN];
	spdk_lvol_op_complete         cb_fn;
	void                         *cb_arg;
	TAILQ_ENTRY(decouple_queued)  link;
};

static TAILQ_HEAD(, decouple_queued) g_decouple_queue =
	TAILQ_HEAD_INITIALIZER(g_decouple_queue);

static struct decouple_queued *
decouple_queued_find(const struct spdk_lvol *lvol)
{
	struct decouple_queued *q;

	TAILQ_FOREACH(q, &g_decouple_queue, link) {
		if (q->lvol == lvol) {
			return q;
		}
	}
	return NULL;
}

/* Whether anything may derive from this volume.
 *
 * A volume that is *queued* to be decoupled does not hold action_in_progress --
 * deliberately, so that it can still be deleted while it waits -- but
 * snapshotting it is exactly as destructive as snapshotting a decouple that has
 * started: spdk_bs_create_snapshot() hands the external snapshot identity to the
 * new snapshot, and the queued decouple then fails its detach with "blob is not
 * a clone of an external snapshot" after materialising every cluster it had to
 * copy. Refused here, in the same place the action_in_progress check lives, so
 * the caller hears about it at the same time. */
bool
s3lvol_lvol_decouple_pending(const struct spdk_lvol *lvol)
{
	return decouple_queued_find(lvol) != NULL || decouple_find(lvol) != NULL;
}

/* Start materialising now. The caller has already established that this volume is
 * an esnap clone, writable, not already running or queued, and that nothing else
 * is decoupling the same export. */
static int
decouple_start(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
	       const char *uuid_str, spdk_lvol_op_complete cb_fn, void *cb_arg)
{
	struct s3lvol_import *imp;
	struct s3lvol_decouple *d;
	uint64_t i;

	d = calloc(1, sizeof(*d));
	if (!d) {
		return -ENOMEM;
	}
	d->lvs    = lvs;
	d->lvol   = lvol;
	d->cb_fn  = cb_fn;
	d->cb_arg = cb_arg;
	snprintf(d->uuid_str, sizeof(d->uuid_str), "%s", uuid_str);
	snprintf(d->lvol_name, sizeof(d->lvol_name), "%s", lvol->name);

	/* The manifest is the whole point: without its bitmap there is no way to tell
	 * a hole from data, and the only safe reading of "no manifest" is to refuse
	 * rather than to treat everything as a hole. */
	imp = import_find(lvs, d->uuid_str);
	if (!imp) {
		SPDK_ERRLOG("lvstore '%s' has no manifest for export %s, which lvol "
			    "'%s' reads through to; cannot tell its holes from its "
			    "data\n", s3lvol_lvstore_get_name(lvs), d->uuid_str,
			    lvol->name);
		free(d);
		return -ENOENT;
	}
	d->m = imp->m;
	s3_export_manifest_ref(d->m);

	d->chunk_size   = d->m->chunk_size;
	d->cluster_size = spdk_bs_get_cluster_size(
				  s3lvol_lvstore_get_lvs(lvs)->blobstore);
	d->num_clusters = spdk_blob_get_num_clusters(lvol->blob);

	if (d->chunk_size == 0 || d->cluster_size == 0) {
		SPDK_ERRLOG("export %s has a zero chunk size\n", d->uuid_str);
		s3_export_manifest_unref(d->m);
		free(d);
		return -EINVAL;
	}

	/* Counted up front so progress can be reported as a fraction. It is a pass
	 * over a bitmap, not over the data. */
	for (i = 0; i < d->num_clusters; i++) {
		if (!decouple_cluster_is_hole(d, i)) {
			d->total++;
		}
	}

	/* Cross-check against the manifest's own count, which is arrived at a
	 * different way: present_chunks is what the exporter wrote down, while
	 * d->total comes from walking the bitmap here. They measure different units
	 * -- chunks against clusters -- so they are not required to be equal, but a
	 * volume whose manifest says it holds data while this walk found none is a
	 * disagreement about the bitmap itself, and materialising nothing would
	 * then be indistinguishable from a volume that really is empty.
	 *
	 * Only this one direction is worth refusing. The reverse, more clusters than
	 * chunks, is ordinary whenever a cluster is smaller than a chunk. */
	if (d->total == 0 && d->m->present_chunks != 0) {
		SPDK_ERRLOG("export %s claims %" PRIu64 " chunk(s) hold data, but none "
			    "of the %" PRIu64 " cluster(s) of lvol '%s' maps to any of "
			    "them. Refusing to decouple: it would look like success and "
			    "leave the volume empty.\n", d->uuid_str,
			    d->m->present_chunks, d->num_clusters, lvol->name);
		s3_export_manifest_unref(d->m);
		free(d);
		return -EIO;
	}

	d->channel = spdk_bs_alloc_io_channel(s3lvol_lvstore_get_lvs(lvs)->blobstore);
	if (!d->channel) {
		s3_export_manifest_unref(d->m);
		free(d);
		return -ENOMEM;
	}

	lvol->action_in_progress = true;
	TAILQ_INSERT_TAIL(&g_decouples, d, link);

	SPDK_NOTICELOG("Decoupling lvol '%s' from export %s: %" PRIu64 " of %" PRIu64
		       " cluster(s) hold data\n", d->lvol_name, d->uuid_str, d->total,
		       d->num_clusters);

	decouple_next(d);
	return 0;
}

/* Hand the turn to the next volume that can take it, if any.
 *
 * Called once a decouple has left g_decouples, so decouple_find_blocker() no
 * longer sees it. Walked in insertion order and stopped at the first entry that
 * can start, which is both fair and the reason this cannot simply start the head:
 * the head may still be blocked by a different decouple that is running, while a
 * later entry is free to go.
 *
 * One at a time on purpose. Starting every unblocked entry would recreate exactly
 * what the queue exists to prevent -- several decouples of one export fetching the
 * same objects, or several on one lvstore fighting over a blobstore channel that
 * serialises them anyway.
 *
 * A queued volume that cannot be started -- deleted meanwhile, or out of memory --
 * gets its callback with the error and the next one is tried, so one bad entry
 * cannot strand the rest of the queue.
 */
static void
decouple_start_next_queued(void)
{
	struct decouple_queued *q, *tmp;

	TAILQ_FOREACH_SAFE(q, &g_decouple_queue, link, tmp) {
		spdk_lvol_op_complete cb_fn;
		void *cb_arg;
		int rc;

		if (decouple_find_blocker(q->lvs, q->uuid_str) != NULL) {
			continue;
		}

		TAILQ_REMOVE(&g_decouple_queue, q, link);
		cb_fn  = q->cb_fn;
		cb_arg = q->cb_arg;

		rc = decouple_start(q->lvs, q->lvol, q->uuid_str, cb_fn, cb_arg);
		if (rc == 0) {
			free(q);
			return;
		}

		SPDK_ERRLOG("Queued decouple of lvol '%s' from export %s could not be "
			    "started: %s\n", q->lvol->name, q->uuid_str,
			    spdk_strerror(-rc));
		free(q);
		if (cb_fn) {
			cb_fn(cb_arg, rc);
		}
	}
}

/* Forget a queued decouple because its volume is going away.
 *
 * A queued volume does not hold action_in_progress, so nothing stops it being
 * deleted while it waits. Without this the queue would keep a pointer to a freed
 * lvol and hand it to decouple_start() later. */
void
s3lvol_decouple_dequeue_lvol(struct spdk_lvol *lvol)
{
	struct decouple_queued *q = decouple_queued_find(lvol);
	spdk_lvol_op_complete cb_fn;
	void *cb_arg;

	if (!q) {
		return;
	}

	SPDK_NOTICELOG("lvol '%s' was waiting to be decoupled from export %s and is "
		       "going away; dropping it from the queue\n", lvol->name,
		       q->uuid_str);
	TAILQ_REMOVE(&g_decouple_queue, q, link);
	cb_fn  = q->cb_fn;
	cb_arg = q->cb_arg;
	free(q);

	if (cb_fn) {
		cb_fn(cb_arg, -ECANCELED);
	}
}

int
s3lvol_lvol_decouple(struct s3lvol_lvstore *lvs, struct spdk_lvol *lvol,
		   spdk_lvol_op_complete cb_fn, void *cb_arg)
{
	char esnap_uuid[SPDK_UUID_STRING_LEN] = {0};
	struct s3lvol_decouple *running;
	struct decouple_queued *q;
	const void *esnap_id = NULL;
	size_t id_len = 0;

	if (!lvs || !lvol || !lvol->blob) {
		return -EINVAL;
	}

	if (!spdk_blob_is_esnap_clone(lvol->blob)) {
		SPDK_ERRLOG("lvol '%s' does not read through to an export; there is "
			    "nothing to decouple from\n", lvol->name);
		return -EINVAL;
	}

	/* A snapshot cannot be decoupled: materialising a cluster is a write, and its
	 * metadata is read-only. It is also not what the caller wants -- the way to
	 * free a snapshot of an imported volume from the export is to delete it, or
	 * to decouple the volume it was taken from before taking it. */
	if (spdk_blob_is_read_only(lvol->blob)) {
		SPDK_ERRLOG("lvol '%s' is read-only (a snapshot?) and cannot be "
			    "decoupled\n", lvol->name);
		return -EPERM;
	}

	if (decouple_find(lvol)) {
		SPDK_ERRLOG("lvol '%s' is already being decoupled\n", lvol->name);
		return -EBUSY;
	}
	if (decouple_queued_find(lvol)) {
		SPDK_ERRLOG("lvol '%s' is already queued to be decoupled\n",
			    lvol->name);
		return -EBUSY;
	}
	if (lvol->action_in_progress) {
		SPDK_ERRLOG("another operation is in progress on lvol '%s'\n",
			    lvol->name);
		return -EBUSY;
	}

	if (spdk_blob_get_esnap_id(lvol->blob, &esnap_id, &id_len) != 0 ||
	    id_len >= SPDK_UUID_STRING_LEN) {
		SPDK_ERRLOG("lvol '%s' has an external snapshot id this module did not "
			    "write\n", lvol->name);
		return -EINVAL;
	}
	memcpy(esnap_uuid, esnap_id, id_len);

	/* Something is already in the way -- the same export's objects being fetched,
	 * or another decouple on this lvstore, which blobstore would serialise anyway.
	 * Wait for it; see the comment on decouple_find_blocker() for why each of
	 * those is a reason, and struct decouple_queued for why this waits rather than
	 * refusing. */
	running = decouple_find_blocker(lvs, esnap_uuid);
	if (!running) {
		return decouple_start(lvs, lvol, esnap_uuid, cb_fn, cb_arg);
	}

	q = calloc(1, sizeof(*q));
	if (!q) {
		return -ENOMEM;
	}
	q->lvs    = lvs;
	q->lvol   = lvol;
	q->cb_fn  = cb_fn;
	q->cb_arg = cb_arg;
	snprintf(q->uuid_str, sizeof(q->uuid_str), "%s", esnap_uuid);
	TAILQ_INSERT_TAIL(&g_decouple_queue, q, link);

	SPDK_NOTICELOG("lvol '%s' is queued to be decoupled from export %s, behind "
		       "'%s' (%s)\n", lvol->name, esnap_uuid, running->lvol_name,
		       strcmp(running->uuid_str, esnap_uuid) == 0
		       ? "same export" : "same lvstore");
	return 0;
}

struct s3lvol_decouple *
s3lvol_decouple_first(void)
{
	return TAILQ_FIRST(&g_decouples);
}

struct s3lvol_decouple *
s3lvol_decouple_next(struct s3lvol_decouple *prev)
{
	return TAILQ_NEXT(prev, link);
}

/* The queue, reported alongside the running ones so that "is this export done
 * being materialised" stays a single question. A caller that waits for
 * rcow_get_decouple to empty would otherwise see the list go empty between one
 * volume finishing and the next starting, and conclude the work was over. */
struct decouple_queued *
s3lvol_decouple_queued_first(void)
{
	return TAILQ_FIRST(&g_decouple_queue);
}

struct decouple_queued *
s3lvol_decouple_queued_next(struct decouple_queued *prev)
{
	return TAILQ_NEXT(prev, link);
}

void
s3lvol_decouple_queued_get(const struct decouple_queued *q,
			   struct s3lvol_decouple_info *out)
{
	out->lvs_name       = s3lvol_lvstore_get_name(q->lvs);
	out->lvol_name      = q->lvol ? q->lvol->name : "";
	out->export_uuid    = q->uuid_str;
	/* Nothing has been counted yet: the totals come from a pass over the
	 * manifest that decouple_start() does, and this one has not started. Zero and
	 * zero reads as "queued", which is what it is. */
	out->clusters_total = 0;
	out->clusters_done  = 0;
}

void
s3lvol_decouple_get(const struct s3lvol_decouple *d, struct s3lvol_decouple_info *out)
{
	out->lvs_name       = s3lvol_lvstore_get_name(d->lvs);
	out->lvol_name      = d->lvol_name;
	out->export_uuid    = d->uuid_str;
	out->clusters_total = d->total;
	out->clusters_done  = d->done;
}

/* ==========================================================================
 * Release
 *
 * Deletes an export's objects. The manifest goes first, on purpose:
 *
 *   - manifest first, then chunks: a crash in between leaves chunk objects that
 *     no manifest references, which is precisely what GC is for;
 *   - chunks first, then manifest: a crash in between leaves a manifest that
 *     promises objects that are gone. An import of it would succeed and then
 *     fail on the first read of a hole that is not a hole.
 * ========================================================================== */

/* The volume that still reads through to this export, or NULL.
 *
 * Every blob of the lvstore is asked, rather than the imports registry, because
 * the dependency moves and the registry does not follow it. Taking a snapshot of
 * an imported clone hands the esnap parent to the *snapshot* -- blobstore moves
 * the parent link across and makes the clone a clone of that snapshot -- so after
 * deleting the clone the registry entry says nothing about who reads the export.
 * Asking the blobs cannot drift: it is their metadata that names the export, and
 * it is their reads that break when the objects go away.
 *
 * Only open blobs are visible here, which is the right scope: an lvol whose blob
 * could not be opened is not in this list, and the usual reason an esnap clone
 * cannot be opened is that its export is already unreachable. */
static struct spdk_lvol *
export_reader_find(struct s3lvol_lvstore *lvs, const char *uuid_str)
{
	struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(lvs);
	size_t uuid_len = strlen(uuid_str);
	struct spdk_lvol *lvol;

	if (!store) {
		return NULL;
	}

	TAILQ_FOREACH(lvol, &store->lvols, link) {
		const void *esnap_id = NULL;
		size_t id_len = 0;

		if (!lvol->blob || !spdk_blob_is_esnap_clone(lvol->blob)) {
			continue;
		}
		if (spdk_blob_get_esnap_id(lvol->blob, &esnap_id, &id_len) != 0) {
			continue;
		}
		if (id_len == uuid_len && memcmp(esnap_id, uuid_str, id_len) == 0) {
			return lvol;
		}
	}
	return NULL;
}

/* How many volumes of one lvstore read through to an export, rather than just
 * whether any does.
 *
 * Since several volumes can be imported from one export, "still in use" is a
 * number, and a refusal that says which single volume it tripped over sends the
 * caller round the loop once per volume with no idea how many are left. */
static uint32_t
export_reader_count(struct s3lvol_lvstore *lvs, const char *uuid_str,
		    struct spdk_lvol **first)
{
	struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(lvs);
	size_t uuid_len = strlen(uuid_str);
	struct spdk_lvol *lvol;
	uint32_t count = 0;

	if (!store) {
		return 0;
	}

	TAILQ_FOREACH(lvol, &store->lvols, link) {
		const void *esnap_id = NULL;
		size_t id_len = 0;

		if (!lvol->blob || !spdk_blob_is_esnap_clone(lvol->blob)) {
			continue;
		}
		if (spdk_blob_get_esnap_id(lvol->blob, &esnap_id, &id_len) != 0) {
			continue;
		}
		if (id_len == uuid_len && memcmp(esnap_id, uuid_str, id_len) == 0) {
			if (count == 0 && first) {
				*first = lvol;
			}
			count++;
		}
	}
	return count;
}

/* Drop this lvstore's registry entry for an export nothing reads any more.
 *
 * Called when a volume stops reading through to an export -- today that means a
 * delete. Nothing in the delete path itself knows the blob was the last reader,
 * so without this the entry outlives its clone. At runtime such an entry is
 * harmless, a cached manifest no esnap callback asks for; what it breaks is
 * release, which would go on refusing for a clone that is not there.
 *
 * The timing is the point: this has to happen while the lvstore is loaded, which
 * is exactly when the last reader goes away. Deferring it to release does not
 * work -- by then the importing lvstore has usually been unloaded, and unload
 * drops the in-memory entries without rewriting the object, so there would be
 * nothing left to notice.
 *
 * Failing to rewrite the registry is reported and otherwise ignored: the entry is
 * stale, not wrong, and the next attach recovers by finding no clone that names
 * the export. */
void
s3lvol_imports_recheck(struct s3lvol_lvstore *lvs, const char *uuid_str)
{
	struct s3lvol_import *imp;

	if (!lvs || !uuid_str || uuid_str[0] == '\0') {
		return;
	}

	imp = import_find(lvs, uuid_str);
	if (!imp || export_reader_find(lvs, uuid_str)) {
		return;
	}

	SPDK_NOTICELOG("lvstore '%s' no longer has a volume reading export %s; "
		       "dropping the registry entry\n",
		       s3lvol_lvstore_get_name(lvs), uuid_str);
	import_remove(imp);
	if (imports_save(lvs, NULL, NULL) != 0) {
		SPDK_WARNLOG("Could not rewrite the imports registry of '%s'\n",
			     s3lvol_lvstore_get_name(lvs));
	}
}

struct release_ctx {
	struct s3lvol_lvstore     *lvs;
	char                   uuid_str[SPDK_UUID_STRING_LEN];
	char                       key[S3_EXPORT_KEY_MAX];
	uint64_t                   size;
	char                      *body;
	struct s3_export_manifest *m;

	/* Chunk deletions in flight, plus a self reference held while the loop
	 * runs. */
	uint32_t                   pending;
	uint64_t                   deleted;
	int                        status;

	spdk_lvol_op_complete      cb_fn;
	void                      *cb_arg;
};

static void
release_finish(struct release_ctx *ctx)
{
	spdk_lvol_op_complete cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;
	int status = ctx->status;

	if (status == 0) {
		SPDK_NOTICELOG("Released export %s: %" PRIu64 " object(s) deleted\n",
			       ctx->uuid_str, ctx->deleted);
	}

	s3_export_manifest_unref(ctx->m);
	free(ctx->body);
	free(ctx);

	if (cb_fn) {
		cb_fn(cb_arg, status);
	}
}

static void
release_registry_saved(void *cb_arg, int status)
{
	struct release_ctx *ctx = cb_arg;

	if (status != 0) {
		/* The objects are gone but this node still lists the export on disk, so
		 * after a restart it would go on refusing to delete a snapshot for an
		 * importer that has nothing left to read. Reported, because the operator
		 * needs to know the obligation outlived the export. */
		SPDK_ERRLOG("Export %s was released but the registry of '%s' could not "
			    "be rewritten: %s. This node may still refuse to delete "
			    "snapshot '%s'.\n", ctx->uuid_str,
			    s3lvol_lvstore_get_name(ctx->lvs), spdk_strerror(-status),
			    ctx->m ? ctx->m->src.snapshot : "?");
		ctx->status = status;
	}

	/* The release is authoritative: this node verified that no volume anywhere
	 * still reads the export (readers == 0 check above), so no importer can be
	 * renewing its lease. Delete it rather than waiting for it to go stale --
	 * a stale lease costs the grace period (3x renew interval) before the
	 * snapshot becomes deletable again, and nothing needs that delay now.
	 *
	 * Fire-and-forget on purpose. If the delete fails, the lease simply goes
	 * stale in a few renew intervals and the next release or the source's
	 * TTL path deals with it -- there is no correctness question either way,
	 * only how soon a following snapshot delete is allowed. */
	char lease_key[S3_EXPORT_KEY_MAX];

	snprintf(lease_key, sizeof(lease_key), "%s/meta/exports/%s.lease",
		 s3lvol_lvstore_get_name(ctx->lvs), ctx->uuid_str);
	if (s3_delete(s3lvol_lvstore_get_client(ctx->lvs), lease_key,
		      NULL, NULL) != 0) {
		SPDK_WARNLOG("Could not submit the delete of lease '%s' of "
			     "export %s; it will go stale on its own\n",
			     lease_key, ctx->uuid_str);
	}

	release_finish(ctx);
}

static void
release_report(struct release_ctx *ctx)
{
	struct s3lvol_export *exp;

	if (ctx->status != 0) {
		release_finish(ctx);
		return;
	}

	/* The obligation ends with the objects. Without this the entry would outlive
	 * the export it describes, and s3lvol_export_pinning() would keep refusing to
	 * delete a snapshot nobody is reading any more -- until the deadline, or
	 * forever if there was none. */
	exp = s3lvol_export_find(ctx->lvs, ctx->uuid_str);
	if (!exp) {
		release_finish(ctx);
		return;
	}

	s3lvol_export_forget(exp);
	if (s3lvol_export_registry_save(ctx->lvs, release_registry_saved, ctx) != 0) {
		SPDK_ERRLOG("Export %s was released but the registry of '%s' could not "
			    "be rewritten\n", ctx->uuid_str,
			    s3lvol_lvstore_get_name(ctx->lvs));
		release_finish(ctx);
	}
}

static void
release_put(struct release_ctx *ctx)
{
	assert(ctx->pending > 0);
	if (--ctx->pending == 0) {
		release_report(ctx);
	}
}

static void
release_chunk_deleted(void *cb_arg, int status)
{
	struct release_ctx *ctx = cb_arg;

	if (status != 0 && ctx->status == 0) {
		/* Reported, but the export is gone either way: its manifest was
		 * deleted first, so whatever is left is unreferenced and GC will
		 * take it. */
		SPDK_WARNLOG("Some objects of export %s could not be deleted (%s); "
			     "they are unreferenced and left to GC\n",
			     ctx->uuid_str, spdk_strerror(-status));
		ctx->status = 0;
	} else if (status == 0) {
		ctx->deleted++;
	}
	release_put(ctx);
}

/* Delete the objects this export owns.
 *
 * A ref export owns none: its manifest names the source lvstore's live chunk
 * objects, which belong to the snapshot and stay in the chunk map. The keys built
 * below would not touch them -- they address the export's own prefix, which is
 * empty -- but issuing one pointless DELETE per present chunk is not a harmless
 * no-op at a million chunks. What actually ends a ref export is the manifest going
 * away, plus the registry entry that stops pinning the snapshot.
 */
static void
release_delete_chunks(struct release_ctx *ctx)
{
	uint64_t i;

	if (ctx->m->layout == S3_EXPORT_LAYOUT_REF) {
		release_report(ctx);
		return;
	}

	ctx->pending = 1;    /* self reference for the duration of the loop */

	for (i = 0; i < ctx->m->num_chunks; i++) {
		char key[S3_EXPORT_KEY_MAX];
		int rc;

		if (!s3_export_manifest_is_present(ctx->m, i)) {
			continue;
		}
		s3_export_chunk_key(ctx->m->src.prefix, ctx->uuid_str, i, key,
				    sizeof(key));

		ctx->pending++;
		rc = s3_delete(s3lvol_lvstore_get_client(ctx->lvs), key,
			       release_chunk_deleted, ctx);
		if (rc != 0) {
			SPDK_WARNLOG("Could not submit a delete of '%s': %s\n", key,
				     spdk_strerror(-rc));
			ctx->pending--;
		}
	}

	release_put(ctx);
}

static void
release_manifest_deleted(void *cb_arg, int status)
{
	struct release_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to delete the manifest '%s': %s. Nothing else "
			    "was deleted -- the export is still importable.\n",
			    ctx->key, spdk_strerror(-status));
		ctx->status = status;
		release_report(ctx);
		return;
	}

	ctx->deleted++;
	release_delete_chunks(ctx);
}

static void
release_got_manifest(void *cb_arg, uint64_t bytes_read, int status)
{
	struct release_ctx *ctx = cb_arg;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("Failed to read the manifest '%s': %s\n", ctx->key,
			    spdk_strerror(-status));
		ctx->status = status;
		release_report(ctx);
		return;
	}

	rc = s3_export_manifest_parse(ctx->body, bytes_read, &ctx->m);
	if (rc != 0) {
		/* The chunk keys come out of the bitmap, so without a readable
		 * manifest there is no list of what to delete. Left to GC, which
		 * needs no manifest to decide a prefix is garbage. */
		SPDK_ERRLOG("Manifest '%s' is unreadable, so the objects of export %s "
			    "cannot be enumerated. Delete the prefix by hand or wait "
			    "for GC.\n", ctx->key, ctx->uuid_str);
		ctx->status = rc;
		release_report(ctx);
		return;
	}

	rc = s3_delete(s3lvol_lvstore_get_client(ctx->lvs), ctx->key,
		       release_manifest_deleted, ctx);
	if (rc != 0) {
		ctx->status = rc;
		release_report(ctx);
	}
}

static void
release_head_done(void *cb_arg, int status)
{
	struct release_ctx *ctx = cb_arg;
	int rc;

	if (status != 0) {
		SPDK_ERRLOG("No manifest for export %s under prefix '%s': %s\n",
			    ctx->uuid_str, s3lvol_lvstore_get_name(ctx->lvs),
			    spdk_strerror(-status));
		ctx->status = status;
		release_report(ctx);
		return;
	}

	ctx->body = malloc(ctx->size);
	if (!ctx->body) {
		ctx->status = -ENOMEM;
		release_report(ctx);
		return;
	}

	rc = s3_get_range(s3lvol_lvstore_get_client(ctx->lvs), ctx->key, 0, ctx->size,
			  ctx->body, release_got_manifest, ctx);
	if (rc != 0) {
		ctx->status = rc;
		release_report(ctx);
	}
}

int
s3lvol_export_release(struct s3lvol_lvstore *lvs, const char *export_uuid,
		      spdk_lvol_op_complete cb_fn, void *cb_arg)
{
	struct s3lvol_lvstore *other;
	struct release_ctx *ctx;
	int rc;

	if (!lvs || !export_uuid) {
		return -EINVAL;
	}

	/* Refuse while a volume *in this process* still reads through to it. Deleting
	 * the objects under a live esnap clone would leave it reading holes that are
	 * not holes, and after a restart it would not open at all.
	 *
	 * Every reader is counted before refusing, not just the first one found. One
	 * export commonly has several volumes imported from it, and a message naming
	 * only one of them makes the caller delete that volume, retry, and be refused
	 * again -- once per volume, with no idea how many are left. The count says how
	 * far there is to go.
	 *
	 * This is not a cross-node guarantee and cannot be: the exporting node has
	 * no way to know who imported. That is what release_export exists for --
	 * the importer says when it is done. */
	struct spdk_lvol *first_reader = NULL;
	struct s3lvol_lvstore *first_lvs = NULL;
	uint32_t readers = 0;

	for (other = s3lvol_lvstore_first(); other;
	     other = s3lvol_lvstore_next(other)) {
		struct spdk_lvol *reader = NULL;
		uint32_t n = export_reader_count(other, export_uuid, &reader);

		if (n == 0) {
			continue;
		}
		readers += n;
		if (!first_reader) {
			first_reader = reader;
			first_lvs = other;
		}
	}

	if (readers != 0) {
		SPDK_ERRLOG("export %s is still the parent of %u volume(s), "
			    "including '%s' in lvstore '%s'; delete them first\n",
			    export_uuid, readers, first_reader->name,
			    s3lvol_lvstore_get_name(first_lvs));
		return -EBUSY;
	}

	/* Belt and braces for the entry s3lvol_imports_recheck() should already have
	 * dropped: a crash between the last reader going away and the registry being
	 * rewritten leaves one behind, and it is this call that keeps such a leftover
	 * from being mistaken for a live import later on. */
	for (other = s3lvol_lvstore_first(); other; other = s3lvol_lvstore_next(other)) {
		s3lvol_imports_recheck(other, export_uuid);
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->lvs    = lvs;
	ctx->cb_fn  = cb_fn;
	ctx->cb_arg = cb_arg;
	snprintf(ctx->uuid_str, sizeof(ctx->uuid_str), "%s", export_uuid);
	s3_export_manifest_key(ctx->uuid_str, ctx->key, sizeof(ctx->key));

	rc = s3_head(s3lvol_lvstore_get_client(lvs), ctx->key, &ctx->size,
		     release_head_done, ctx);
	if (rc != 0) {
		free(ctx);
	}
	return rc;
}
