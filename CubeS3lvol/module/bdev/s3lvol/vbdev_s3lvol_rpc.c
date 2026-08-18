/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_rpc -- bespoke JSON-RPC
 *
 *   === Why bespoke instead of the built-in bdev_lvol_* ===
 *
 *   Every built-in lvol RPC goes through vbdev_lvol_store_first() walking
 *   g_spdk_lvol_pairs (vbdev_lvol.c:20), which is a static private list with
 *   no way for external code to insert entries.
 *
 *   Meanwhile nvmf_* only recognises names in the global bdev registry, so
 *   **only the lvstore/lvol stretch has to be built here**; the nvmf side is
 *   reused with zero code.
 *
 *   Every method is registered under the rcow_ prefix (rcow_create_lvstore,
 *   rcow_attach_lvstore, rcow_create_lvol, ...), not the bdev_s3lvol_ names
 *   this design once called them. The full set, all registered at runtime via
 *   SPDK_RPC_REGISTER:
 *
 *     rcow_create_lvstore / rcow_attach_lvstore / rcow_delete_lvstore
 *     rcow_unload_lvstore / rcow_flush_lvstore / rcow_checkpoint_lvstore
 *     rcow_get_lvstores / rcow_add_cos_config
 *     rcow_create_lvol / rcow_delete_lvol / rcow_resize_lvol
 *     rcow_create_snapshot / rcow_create_clone
 *     rcow_export_snapshot / rcow_get_snapshot_status / rcow_import_lvol
 *     rcow_release_export / rcow_get_imports / rcow_decouple_lvol
 *     rcow_get_decouple
 *     rcow_active_bdev / rcow_deactive_bdev / rcow_get_bdev
 *
 *   Credentials do not travel as RPC parameters -- they are read from the
 *   environment (S3_AUTH_ENV), so secrets never land in RPC logs or shell
 *   history.
 */

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/json.h"
#include "spdk/jsonrpc.h"
#include "spdk/log.h"
#include "spdk/lvol.h"
#include "spdk/nvmf_spec.h"
#include "spdk/rpc.h"
#include "spdk/string.h"
#include "spdk/util.h"

#include "spdk_internal/lvolstore.h"

#include "s3lvol/s3_export.h"
#include "s3lvol/s3_spawner.h"
#include "s3lvol/s3_types.h"

#include "vbdev_s3lvol.h"

SPDK_LOG_REGISTER_COMPONENT(s3lvol_rpc)

/* Sizes that name a volume or store capacity are taken in GiB, and the parameter
 * always says so: capacity_gib, size_gib. Both convert with << 30.
 *
 * The unit is in the name on purpose. These started out in bytes, and switching
 * to GiB under the same name would have been the dangerous kind of change: the
 * JSON decoders are strict about unknown keys but say nothing about a value's
 * magnitude, so an un-updated caller would have gone on passing 17179869184 and
 * got a volume 2^30 times too large -- successfully, because thin volumes are
 * never checked against the store's capacity (blobstore.c:2292 only consults
 * free_clusters for thick blobs). Renaming turns that into Invalid parameters.
 *
 * Sizes that are *not* capacities keep their own units: cluster_size and
 * chunk_size are bytes (they are MiB-scale, GiB would be meaningless), and
 * journal_size_mb / wal_size_mb already carry theirs.
 *
 * Replies stay in bytes. They report what was actually reached, and blobstore
 * rounds up to a cluster boundary, so GiB could not express it. The reply field
 * is `size`, never `size_gib`, which keeps the two apart. */
#define RCOW_MAX_GIB (UINT64_MAX >> 30)
#define RCOW_GIB_TO_BYTES(gib) ((uint64_t)(gib) << 30)

/* Shared by the three RPCs that take one. Returns NULL when the value is usable,
 * otherwise a description of what is wrong with it, written into the caller's
 * buffer.
 *
 * It used to answer the request itself, which no longer works: its callers do not
 * agree on a response format any more. rcow_create_lvstore is an operator RPC and
 * still replies with a JSON-RPC error; the lvol RPCs reply with
 * {bool_value:false, string_value:...}. So the message comes back rather than
 * going out, and each caller wraps it in its own shape. */
static const char *
rcow_gib_problem(const char *param, uint64_t gib, char *buf, size_t buflen)
{
	if (gib == 0) {
		snprintf(buf, buflen, "%s must be at least 1", param);
		return buf;
	}
	if (gib > RCOW_MAX_GIB) {
		snprintf(buf, buflen, "%s %" PRIu64 " does not fit in a byte count",
			 param, gib);
		return buf;
	}
	return NULL;
}

/* ==========================================================================
 * Unified lvol RPC response helpers
 *
 * Every RPC an external caller uses replies with the same two-field object,
 * whether it succeeds or fails:
 *
 *   success  {"bool_value":true,  "string_value":"vol0"}
 *   failure  {"bool_value":false, "string_value":"lvol not found"}
 *
 * This is by design: the callers parse a single shape, and the bool tells them
 * which branch applies. The string is what was produced on success -- a volume
 * name, an export uuid, or a serialised JSON document when there is more than a
 * name to report -- and the error description on failure.
 *
 * Note that a failure is still a JSON-RPC *success*: the reply carries a result,
 * not an error member. That is what makes the shape uniform, and it is why the
 * client in test/tools/s3lvol_rpc.py maps bool_value:false back onto a non-zero
 * exit status -- the transport no longer distinguishes the two, so something has
 * to.
 *
 * The lvstore RPCs are deliberately left out. They are operator entry points
 * driven by rcow_start.sh, they report structured facts (uuid, cluster counts,
 * write-path counters) that no envelope would improve, and a start script that
 * has to unwrap and re-parse them gains nothing.
 * ========================================================================== */

static void
rpc_lvol_write_response(struct spdk_jsonrpc_request *request, bool ok,
			const char *str)
{
	struct spdk_json_write_ctx *w;

	w = spdk_jsonrpc_begin_result(request);
	spdk_json_write_object_begin(w);
	spdk_json_write_named_bool(w, "bool_value", ok);
	spdk_json_write_named_string(w, "string_value", str);
	spdk_json_write_object_end(w);
	spdk_jsonrpc_end_result(request, w);
}

static void
rpc_lvol_respond_ok(struct spdk_jsonrpc_request *request, const char *name)
{
	rpc_lvol_write_response(request, true, name);
}

static void
rpc_lvol_respond_err(struct spdk_jsonrpc_request *request, int err,
		     const char *msg)
{
	char buf[256];
	const char *sys;

	if (msg && msg[0] != '\0') {
		snprintf(buf, sizeof(buf), "%s", msg);
	} else {
		sys = spdk_strerror(-err);
		snprintf(buf, sizeof(buf), "%s", sys);
	}
	rpc_lvol_write_response(request, false, buf);
}

/* The formatted form. Most failures here name the object they are about, and
 * spdk_jsonrpc_send_error_response_fmt has no counterpart once the error is
 * carried in a result. */
static void __attribute__((format(printf, 2, 3)))
rpc_lvol_respond_errf(struct spdk_jsonrpc_request *request, const char *fmt, ...)
{
	char buf[512];
	va_list ap;

	va_start(ap, fmt);
	vsnprintf(buf, sizeof(buf), fmt, ap);
	va_end(ap);

	rpc_lvol_write_response(request, false, buf);
}

/* --------------------------------------------------------------------------
 * Structured answers, carried inside string_value
 *
 * Four of these RPCs have more than a name to report: rcow_get_bdev,
 * rcow_get_decouple and rcow_get_imports answer with a list, and
 * rcow_active_bdev with the placement it picked. The unified reply has one string
 * to put that in, so the document is serialised and handed over as that string;
 * the caller parses it a second time.
 *
 * That double encoding is the price of one shape for every reply, and it is paid
 * here rather than by adding a third field for structured payloads -- a caller
 * that must branch on whether the payload sits in string_value or somewhere else
 * has not been given a uniform interface at all.
 *
 * The document is built with the ordinary spdk_json writer against a growing
 * buffer, so the escaping of the result into string_value is done by
 * spdk_json_write_named_string and not by hand.
 * -------------------------------------------------------------------------- */

struct rpc_json_buf {
	char   *data;
	size_t  len;
	size_t  cap;
	bool    failed;
};

static int
rpc_json_buf_append(void *cb_ctx, const void *data, size_t size)
{
	struct rpc_json_buf *buf = cb_ctx;
	size_t need = buf->len + size + 1;
	char *bigger;

	if (buf->failed) {
		return -1;
	}

	if (need > buf->cap) {
		size_t cap = buf->cap ? buf->cap : 1024;

		while (cap < need) {
			cap *= 2;
		}
		bigger = realloc(buf->data, cap);
		if (!bigger) {
			buf->failed = true;
			return -1;
		}
		buf->data = bigger;
		buf->cap  = cap;
	}

	memcpy(buf->data + buf->len, data, size);
	buf->len += size;
	/* NUL kept in step so the buffer can be handed straight to a %s. */
	buf->data[buf->len] = '\0';
	return 0;
}

/* Returns NULL when the writer could not be created, in which case nothing has
 * been said to the request yet and the caller must answer. */
static struct spdk_json_write_ctx *
rpc_json_buf_begin(struct rpc_json_buf *buf)
{
	memset(buf, 0, sizeof(*buf));
	return spdk_json_write_begin(rpc_json_buf_append, buf, 0);
}

/* Close the document and reply with it. Frees the buffer either way, so callers
 * never have to. */
static void
rpc_json_buf_respond(struct spdk_jsonrpc_request *request,
		     struct spdk_json_write_ctx *w, struct rpc_json_buf *buf)
{
	int rc;

	rc = spdk_json_write_end(w);
	if (rc != 0 || buf->failed || buf->data == NULL) {
		rpc_lvol_respond_errf(request,
				      "the reply could not be assembled (out of "
				      "memory)");
	} else {
		rpc_lvol_respond_ok(request, buf->data);
	}

	free(buf->data);
	buf->data = NULL;
}

/* ==========================================================================
 * rcow_create_lvstore
 * ========================================================================== */

struct rpc_create_lvstore {
	char       *lvs_name;
	char       *ns_name;
	uint64_t    capacity_gib;
	uint32_t    cluster_size;
	uint32_t    chunk_size;
	bool        force;

	/* Local bdev carrying the metadata journal and the WAL.
	 *
	 * Optional only in the sense that the code runs without it: leaving it out
	 * selects the direct-to-S3 write path, which is neither crash safe nor
	 * correct under concurrent partial-chunk writes. Anything but a smoke test
	 * should pass it. */
	char       *wal_bdev;
	char       *cache_bdev;
	uint32_t    journal_size_mb;
	uint32_t    wal_size_mb;

	/* Seconds between automatic checkpoints; 0 takes the default. Bounds how
	 * much journal a crash has to replay. */
	uint32_t    checkpoint_interval_sec;
};

static void
free_rpc_create_lvstore(struct rpc_create_lvstore *req)
{
	free(req->lvs_name);
	free(req->ns_name);
	free(req->wal_bdev);
	free(req->cache_bdev);
}

static const struct spdk_json_object_decoder rpc_create_lvstore_decoders[] = {
	{"lvs_name",     offsetof(struct rpc_create_lvstore, lvs_name),     spdk_json_decode_string, true},
	{"namespace",    offsetof(struct rpc_create_lvstore, ns_name),    spdk_json_decode_string, false},
	{"capacity_gib", offsetof(struct rpc_create_lvstore, capacity_gib), spdk_json_decode_uint64, false},
	{"cluster_size", offsetof(struct rpc_create_lvstore, cluster_size), spdk_json_decode_uint32, true},
	{"chunk_size",   offsetof(struct rpc_create_lvstore, chunk_size),   spdk_json_decode_uint32, true},
	{"wal_bdev",     offsetof(struct rpc_create_lvstore, wal_bdev),     spdk_json_decode_string, true},
	{"cache_bdev",   offsetof(struct rpc_create_lvstore, cache_bdev),   spdk_json_decode_string, true},
	{"journal_size_mb", offsetof(struct rpc_create_lvstore, journal_size_mb), spdk_json_decode_uint32, true},
	{"wal_size_mb",  offsetof(struct rpc_create_lvstore, wal_size_mb),  spdk_json_decode_uint32, true},
	{"force",        offsetof(struct rpc_create_lvstore, force),        spdk_json_decode_bool,   true},
	{"checkpoint_interval_sec", offsetof(struct rpc_create_lvstore, checkpoint_interval_sec), spdk_json_decode_uint32, true},
};

static void
rpc_create_lvstore_cb(void *cb_arg, struct s3lvol_lvstore *lvs, int lvserrno)
{
	struct spdk_jsonrpc_request *request = cb_arg;
	struct spdk_json_write_ctx *w;
	struct spdk_lvol_store *store;

	if (lvserrno != 0) {
		spdk_jsonrpc_send_error_response(request, lvserrno,
						 spdk_strerror(-lvserrno));
		return;
	}

	store = s3lvol_lvstore_get_lvs(lvs);

	w = spdk_jsonrpc_begin_result(request);
	spdk_json_write_object_begin(w);
	spdk_json_write_named_string(w, "lvs_name", s3lvol_lvstore_get_name(lvs));
	spdk_json_write_named_uuid(w, "uuid", &store->uuid);
	spdk_json_write_named_uint64(w, "cluster_size",
				     spdk_bs_get_cluster_size(store->blobstore));
	spdk_json_write_named_uint64(w, "free_clusters",
				     spdk_bs_free_cluster_count(store->blobstore));
	spdk_json_write_object_end(w);
	spdk_jsonrpc_end_result(request, w);
}

static void
rpc_rcow_create_lvstore(struct spdk_jsonrpc_request *request,
			       const struct spdk_json_val *params)
{
	struct rpc_create_lvstore req = {0};
	struct s3_lvs_opts opts = {0};
	const struct s3_target *tgt;
	const char *problem;
	char errbuf[128];
	int rc;

	if (spdk_json_decode_object(params, rpc_create_lvstore_decoders,
				    SPDK_COUNTOF(rpc_create_lvstore_decoders),
				    &req)) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "Invalid parameters");
		goto cleanup;
	}

	problem = rcow_gib_problem("capacity_gib", req.capacity_gib, errbuf,
				   sizeof(errbuf));
	if (problem) {
		spdk_jsonrpc_send_error_response(request,
						 SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 problem);
		goto cleanup;
	}

	/* When the caller does not supply a name the module generates one. The
	 * auto-generated name is what goes into bstore.json and is returned to the
	 * caller so it can be passed to subsequent lvol operations. */
	if (!req.lvs_name) {
		char generated[32];

		bstore_generate_bs_name(generated, sizeof(generated));
		/* Overwrite the bstore_ prefix with s3lvol_ to keep the two
		 * namespaces apart. */
		memcpy(generated, "s3lvol_", 7);
		req.lvs_name = strdup(generated);
		if (!req.lvs_name) {
			spdk_jsonrpc_send_error_response(request, -ENOMEM,
							 "out of memory");
			goto cleanup;
		}
	}

	tgt = rcow_namespace_to_target(req.ns_name);
	if (!tgt) {
		spdk_jsonrpc_send_error_response_fmt(request, -ENOENT,
						     "namespace '%s' not found",
						     req.ns_name);
		goto cleanup;
	}

	/* One blobstore per node for now. The data structures and RPCs are
	 * written to handle more-than-one, this is a policy limit only.
	 *
	 * The limit is on *creating* one, not on how many may be loaded: attach
	 * is deliberately unrestricted, because moving a volume between lvstores
	 * needs both ends loaded at once, and run_export_test.sh does exactly
	 * that when it builds a clone chain across the two. Do not "make this
	 * consistent" by adding the same check to attach.
	 *
	 * Counting rather than asking pick_one(), which returns NULL both for
	 * none and for more-than-one: as `pick_one() != NULL` this fired at
	 * exactly one and let everything through from two upwards, so a third
	 * lvstore could be created via the very path that two loaded ones had
	 * opened. */
	if (s3lvol_lvstore_count() > 0) {
		spdk_jsonrpc_send_error_response(request, -EBUSY,
			"a blobstore already exists; only one per node is "
			"currently supported. Delete the existing one first.");
		goto cleanup;
	}

	opts.target           = *tgt;
	opts.target.auth_mode = S3_AUTH_ENV;
	opts.ns_name        = req.ns_name;
	opts.lvs_name         = req.lvs_name;
	opts.capacity_bytes   = RCOW_GIB_TO_BYTES(req.capacity_gib);
	opts.cluster_size     = req.cluster_size;
	opts.chunk_size       = req.chunk_size;
	opts.force            = req.force;

	opts.wal_bdev_name   = req.wal_bdev;
	opts.cache_bdev_name = req.cache_bdev;
	opts.journal_size_mb = req.journal_size_mb;
	opts.wal_size_mb     = req.wal_size_mb;
	opts.checkpoint_interval_sec = req.checkpoint_interval_sec;

	rc = s3lvol_lvstore_create(&opts, rpc_create_lvstore_cb, request);
	if (rc != 0) {
		spdk_jsonrpc_send_error_response(request, rc, spdk_strerror(-rc));
	}

cleanup:
	free_rpc_create_lvstore(&req);
}
SPDK_RPC_REGISTER("rcow_create_lvstore", rpc_rcow_create_lvstore,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_attach_lvstore
 *
 * Bring an existing lvstore back up, replaying whatever the last process did not
 * finish. This is the other half of the WAL's purpose: without it, a target that
 * was killed leaves acknowledged writes sitting in the log with no way to get
 * them into S3.
 *
 * Deliberately *not* merged into create with an "if it exists, attach" flag.
 * Create formats the local device and initialises a blobstore; attach must do
 * neither. A single entry point that guesses which one was meant would, on a
 * misdetection, format the disk holding the only copy of unflushed data.
 *
 * Note which parameters are absent: capacity and chunk_size. Both are read back
 * from the local device, because they have to match what the lvstore was created
 * with and there is no reason to let an operator retype them.
 * ========================================================================== */

struct rpc_attach_lvstore {
	char *lvs_name;
	char *ns_name;
	char *wal_bdev;
	char *cache_bdev;
	bool  force;

	/* Not read back from the local device on purpose: the interval is a policy
	 * of this process, not a property of the lvstore, so an attach may
	 * legitimately run with a different one from the create. */
	uint32_t checkpoint_interval_sec;
};

static void
free_rpc_attach_lvstore(struct rpc_attach_lvstore *req)
{
	free(req->lvs_name);
	free(req->ns_name);
	free(req->wal_bdev);
	free(req->cache_bdev);
}

static const struct spdk_json_object_decoder rpc_attach_lvstore_decoders[] = {
	{"lvs_name",   offsetof(struct rpc_attach_lvstore, lvs_name),   spdk_json_decode_string, false},
	{"namespace",  offsetof(struct rpc_attach_lvstore, ns_name),  spdk_json_decode_string, false},
	{"wal_bdev",   offsetof(struct rpc_attach_lvstore, wal_bdev),   spdk_json_decode_string, false},
	{"cache_bdev", offsetof(struct rpc_attach_lvstore, cache_bdev), spdk_json_decode_string, true},
	{"force",      offsetof(struct rpc_attach_lvstore, force),      spdk_json_decode_bool,   true},
	{"checkpoint_interval_sec", offsetof(struct rpc_attach_lvstore, checkpoint_interval_sec), spdk_json_decode_uint32, true},
};

static void
rpc_attach_lvstore_cb(void *cb_arg, struct s3lvol_lvstore *lvs, int lvserrno)
{
	struct spdk_jsonrpc_request *request = cb_arg;
	struct spdk_json_write_ctx *w;
	struct spdk_lvol_store *store;
	struct spdk_lvol *lvol;

	if (lvserrno != 0) {
		spdk_jsonrpc_send_error_response(request, lvserrno,
						 spdk_strerror(-lvserrno));
		return;
	}

	store = s3lvol_lvstore_get_lvs(lvs);

	/* Report the lvols that came back, with their bdev names: the caller has
	 * to re-export them over nvmf and has no other way to learn what was
	 * found on the recovered lvstore. */
	w = spdk_jsonrpc_begin_result(request);
	spdk_json_write_object_begin(w);
	spdk_json_write_named_string(w, "lvs_name", s3lvol_lvstore_get_name(lvs));
	spdk_json_write_named_uuid(w, "uuid", &store->uuid);
	spdk_json_write_named_uint64(w, "cluster_size",
				     spdk_bs_get_cluster_size(store->blobstore));
	spdk_json_write_named_uint64(w, "free_clusters",
				     spdk_bs_free_cluster_count(store->blobstore));
	spdk_json_write_named_array_begin(w, "lvols");
	TAILQ_FOREACH(lvol, &store->lvols, link) {
		spdk_json_write_object_begin(w);
		spdk_json_write_named_string(w, "name", lvol->name);
		if (lvol->bdev) {
			spdk_json_write_named_string(w, "bdev_name", lvol->bdev->name);
			spdk_json_write_named_uint64(w, "num_blocks",
						     lvol->bdev->blockcnt);
		}
		spdk_json_write_named_uuid(w, "uuid", &lvol->uuid);
		spdk_json_write_object_end(w);
	}
	spdk_json_write_array_end(w);
	spdk_json_write_object_end(w);
	spdk_jsonrpc_end_result(request, w);
}

static void
rpc_rcow_attach_lvstore(struct spdk_jsonrpc_request *request,
			       const struct spdk_json_val *params)
{
	struct rpc_attach_lvstore req = {0};
	struct s3_lvs_opts opts = {0};
	const struct s3_target *tgt;
	int rc;

	if (spdk_json_decode_object(params, rpc_attach_lvstore_decoders,
				    SPDK_COUNTOF(rpc_attach_lvstore_decoders),
				    &req)) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "Invalid parameters");
		goto cleanup;
	}

	tgt = rcow_namespace_to_target(req.ns_name);
	if (!tgt) {
		spdk_jsonrpc_send_error_response_fmt(request, -ENOENT,
						     "namespace '%s' not found",
						     req.ns_name);
		goto cleanup;
	}

	opts.target           = *tgt;
	opts.target.auth_mode = S3_AUTH_ENV;
	opts.ns_name        = req.ns_name;
	opts.lvs_name         = req.lvs_name;
	opts.wal_bdev_name   = req.wal_bdev;
	opts.cache_bdev_name = req.cache_bdev;
	opts.force           = req.force;
	opts.checkpoint_interval_sec = req.checkpoint_interval_sec;

	rc = s3lvol_lvstore_attach(&opts, rpc_attach_lvstore_cb, request);
	if (rc != 0) {
		spdk_jsonrpc_send_error_response(request, rc, spdk_strerror(-rc));
	}

cleanup:
	free_rpc_attach_lvstore(&req);
}
SPDK_RPC_REGISTER("rcow_attach_lvstore", rpc_rcow_attach_lvstore,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_create_lvol
 *
 * Two parameters, both required: lvol_name and size_gib.
 *
 * === Why the size is in GiB ===
 *
 * Volumes here are provisioned in GiB, so bytes meant every caller wrote out
 * 17179869184 and every reader had to divide. The name carries the unit, which
 * also keeps it distinct from rcow_resize_lvol's `size` -- that one is still in
 * bytes, and two parameters both called `size` with different units would be a
 * trap. Renaming rather than redefining `size` is deliberate: the decoder is
 * strict, so an old caller passing `size` gets Invalid parameters instead of a
 * volume 2^30 times too big.
 *
 * === Why there is no lvs_name ===
 *
 * There was a field for it, and a branch using it, but the decoder never listed
 * it -- so it could not be passed (the strict decoder rejected the whole
 * request), req.lvs_name was always NULL, and the branch was dead code. This RPC
 * has therefore only ever worked against a single lvstore, which is what
 * rcow_start.sh creates. Removing the field makes that real instead of implied,
 * and the error below now names the actual constraint rather than telling the
 * caller to pass a parameter that does not exist.
 *
 * === Why there is no thin_provision ===
 *
 * It defaulted to true and nothing ever passed false. On an S3 backend thin is
 * the only sensible choice: an unwritten chunk is simply an absent object that
 * reads as zeroes, so thick provisioning would mean writing zeroes to object
 * storage to reserve space that costs nothing to leave unreserved.
 * ========================================================================== */

struct rpc_create_lvol {
	char       *lvol_name;
	uint64_t    size_gib;
};

static void
free_rpc_create_lvol(struct rpc_create_lvol *req)
{
	free(req->lvol_name);
}

static const struct spdk_json_object_decoder rpc_create_lvol_decoders[] = {
	{"lvol_name", offsetof(struct rpc_create_lvol, lvol_name), spdk_json_decode_string, false},
	{"size_gib",  offsetof(struct rpc_create_lvol, size_gib),  spdk_json_decode_uint64, false},
};

static void
rpc_create_lvol_cb(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct spdk_jsonrpc_request *request = cb_arg;

	if (lvolerrno != 0) {
		rpc_lvol_respond_err(request, lvolerrno, NULL);
		return;
	}

	/* Returns the lvol name; the caller takes it to rcow_active_bdev --
	 * NOT nvmf_subsystem_add_ns, which bypasses the active volume registry
	 * and would not be replayed on crash recovery. */
	rpc_lvol_respond_ok(request, lvol->name);
}

/* GiB -> bytes; the ceiling and the zero check are shared with the other two,
 * see rcow_gib_problem. There is no upper bound beyond what fits in a byte count:
 * thin volumes may exceed the lvstore's capacity on purpose (blobstore only
 * checks free clusters for thick blobs, blobstore.c:2292), so no smaller limit
 * would be honest. */
static void
rpc_rcow_create_lvol(struct spdk_jsonrpc_request *request,
			    const struct spdk_json_val *params)
{
	struct rpc_create_lvol req = {0};
	struct s3lvol_lvstore *lvs;
	const char *problem;
	char errbuf[128];
	int rc;

	if (spdk_json_decode_object(params, rpc_create_lvol_decoders,
				    SPDK_COUNTOF(rpc_create_lvol_decoders),
				    &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters: expected "
				     "{\"lvol_name\":<string>,\"size_gib\":<number>}");
		goto cleanup;
	}

	problem = rcow_gib_problem("size_gib", req.size_gib, errbuf, sizeof(errbuf));
	if (problem) {
		rpc_lvol_respond_err(request, 0, problem);
		goto cleanup;
	}

	lvs = s3lvol_lvstore_pick_one();
	if (!lvs) {
		rpc_lvol_respond_err(request, 0,
			"this RPC creates volumes in the one lvstore that exists; "
			"there are currently none, or more than one (it takes no "
			"lvs_name, see the comment above rpc_rcow_create_lvol)");
		goto cleanup;
	}

	rc = s3lvol_lvol_create(lvs, req.lvol_name,
				RCOW_GIB_TO_BYTES(req.size_gib), true,
				rpc_create_lvol_cb, request);
	if (rc != 0) {
		rpc_lvol_respond_err(request, rc, NULL);
	}

cleanup:
	free_rpc_create_lvol(&req);
}
SPDK_RPC_REGISTER("rcow_create_lvol", rpc_rcow_create_lvol,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_create_snapshot / rcow_create_clone
 *
 * Neither copies data in S3 -- a snapshot freezes the origin's cluster list into
 * a read-only blob, a clone points at the parent's clusters, and the existing
 * chunk objects keep being referenced. What that means for the reply is that
 * these return as soon as the metadata is written, not after any transfer.
 * ========================================================================== */

struct rpc_lvol_derive {
	char *lvs_name;
	char *lvol_name;
	char *new_name;
};

static void
free_rpc_lvol_derive(struct rpc_lvol_derive *req)
{
	free(req->lvs_name);
	free(req->lvol_name);
	free(req->new_name);
}

static const struct spdk_json_object_decoder rpc_create_snapshot_decoders[] = {
	{"lvol_name",     offsetof(struct rpc_lvol_derive, lvol_name), spdk_json_decode_string, false},
	{"snapshot_name", offsetof(struct rpc_lvol_derive, new_name),  spdk_json_decode_string, false},
};

static const struct spdk_json_object_decoder rpc_create_clone_decoders[] = {
	{"snapshot_name", offsetof(struct rpc_lvol_derive, lvol_name), spdk_json_decode_string, false},
	{"clone_name",    offsetof(struct rpc_lvol_derive, new_name),  spdk_json_decode_string, false},
};

/* The bdev name is "<lvs>/<lvol>", so no standalone "name" field is needed.
 * Also, callers should not hand-assemble a bdev name for nvmf -- the correct
 * entry point is rcow_active_bdev, which takes the lvol name (not the bdev
 * name). */
static void
rpc_derive_lvol_cb(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct spdk_jsonrpc_request *request = cb_arg;

	if (lvolerrno != 0) {
		rpc_lvol_respond_err(request, lvolerrno, NULL);
		return;
	}

	rpc_lvol_respond_ok(request, lvol->name);
}

/* Shared by both: decode, resolve, dispatch. The only differences are the decoder
 * table and which of s3lvol_lvol_create_{snapshot,clone} to call, so keeping them
 * in one function keeps the error handling from diverging. */
static void
rpc_derive_lvol(struct spdk_jsonrpc_request *request,
		const struct spdk_json_val *params,
		const struct spdk_json_object_decoder *decoders,
		size_t decoder_count, bool snapshot)
{
	struct rpc_lvol_derive req = {0};
	struct s3lvol_lvstore *lvs;
	struct spdk_lvol *lvol;
	int rc;

	if (spdk_json_decode_object(params, decoders, decoder_count, &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	lvol = s3lvol_lvol_find_any(req.lvol_name);
	if (!lvol) {
		rpc_lvol_respond_errf(request, "lvol '%s' not found in any lvstore",
				      req.lvol_name);
		goto cleanup;
	}
	lvs = s3lvol_lvstore_of_lvol(lvol);
	if (!lvs) {
		rpc_lvol_respond_err(request, 0, "internal: lvol without lvstore");
		goto cleanup;
	}

	if (snapshot) {
		rc = s3lvol_lvol_create_snapshot(lvs, lvol, req.new_name,
						 rpc_derive_lvol_cb, request);
	} else {
		rc = s3lvol_lvol_create_clone(lvs, lvol, req.new_name,
					      rpc_derive_lvol_cb, request);
	}
	if (rc != 0) {
		rpc_lvol_respond_err(request, rc, NULL);
	}

cleanup:
	free_rpc_lvol_derive(&req);
}

static void
rpc_rcow_create_snapshot(struct spdk_jsonrpc_request *request,
				const struct spdk_json_val *params)
{
	rpc_derive_lvol(request, params, rpc_create_snapshot_decoders,
			SPDK_COUNTOF(rpc_create_snapshot_decoders), true);
}
SPDK_RPC_REGISTER("rcow_create_snapshot", rpc_rcow_create_snapshot,
		  SPDK_RPC_RUNTIME)

static void
rpc_rcow_create_clone(struct spdk_jsonrpc_request *request,
			     const struct spdk_json_val *params)
{
	rpc_derive_lvol(request, params, rpc_create_clone_decoders,
			SPDK_COUNTOF(rpc_create_clone_decoders), false);
}
SPDK_RPC_REGISTER("rcow_create_clone", rpc_rcow_create_clone,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_resize_lvol / rcow_delete_lvol
 *
 * Both address an lvol the same way the create does: lvs_name + lvol_name.
 * The early design sketch said "bdev", i.e. the "<lvs>/<lvol>" bdev name, but
 * that would be a second naming scheme in the same RPC family for no gain --
 * the bdev name is derived from exactly these two fields.
 * ========================================================================== */

struct rpc_lvol_name {
	char     *lvs_name;
	char     *lvol_name;
	uint64_t  size_gib;
};

static void
free_rpc_lvol_name(struct rpc_lvol_name *req)
{
	free(req->lvs_name);
	free(req->lvol_name);
}

static const struct spdk_json_object_decoder rpc_resize_lvol_decoders[] = {
	{"lvol_name", offsetof(struct rpc_lvol_name, lvol_name), spdk_json_decode_string, false},
	{"size_gib",  offsetof(struct rpc_lvol_name, size_gib),  spdk_json_decode_uint64, false},
};

static const struct spdk_json_object_decoder rpc_delete_lvol_decoders[] = {
	{"lvol_name", offsetof(struct rpc_lvol_name, lvol_name), spdk_json_decode_string, false},
};

/* Resolve lvs_name + lvol_name, answering the request itself on failure.
 *
 * Returns NULL when it has already sent an error response, so callers must not
 * touch the request afterwards. */
static struct spdk_lvol *
rpc_lookup_lvol(struct spdk_jsonrpc_request *request,
		const struct rpc_lvol_name *req)
{
	struct s3lvol_lvstore *lvs;
	struct spdk_lvol *lvol;

	if (req->lvs_name) {
		/* Explicit -- look up in the named lvstore. */
		lvs = s3lvol_lvstore_find(req->lvs_name);
		if (!lvs) {
			rpc_lvol_respond_errf(request, "lvstore '%s' not found",
					      req->lvs_name);
			return NULL;
		}

		lvol = s3lvol_lvol_find(lvs, req->lvol_name);
		if (!lvol) {
			rpc_lvol_respond_errf(request, "lvstore '%s' has no lvol '%s'",
					      req->lvs_name, req->lvol_name);
			return NULL;
		}
		return lvol;
	}

	/* No lvs_name -- find the lvol by scanning all lvstores. */
	lvol = s3lvol_lvol_find_any(req->lvol_name);
	if (!lvol) {
		rpc_lvol_respond_errf(request, "lvol '%s' not found in any lvstore",
				      req->lvol_name);
		return NULL;
	}
	return lvol;
}

/* The context has the lvol because the response is the lvol's name -- nothing
 * about block counts or sizing, which is consistent with the other lvol RPCs. */
struct rpc_resize_ctx {
	struct spdk_jsonrpc_request *request;
	struct spdk_lvol            *lvol;
};

static void
rpc_resize_lvol_cb(void *cb_arg, int lvolerrno)
{
	struct rpc_resize_ctx *ctx = cb_arg;
	struct spdk_jsonrpc_request *request = ctx->request;
	struct spdk_lvol *lvol = ctx->lvol;

	lvol->action_in_progress = false;

	if (lvolerrno != 0) {
		rpc_lvol_respond_err(request, lvolerrno, NULL);
		free(ctx);
		return;
	}

	rpc_lvol_respond_ok(request, lvol->name);
	free(ctx);
}

static void
rpc_rcow_resize_lvol(struct spdk_jsonrpc_request *request,
			    const struct spdk_json_val *params)
{
	struct rpc_lvol_name req = {0};
	struct rpc_resize_ctx *ctx;
	struct spdk_lvol *lvol;
	const char *problem;
	char errbuf[128];
	int rc;

	if (spdk_json_decode_object(params, rpc_resize_lvol_decoders,
				    SPDK_COUNTOF(rpc_resize_lvol_decoders), &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters: expected "
				     "{\"lvol_name\":<string>,\"size_gib\":<number>}");
		goto cleanup;
	}

	/* Shrinking is refused further down (-ENOTSUP), so the only thing checked
	 * here is that the number itself is usable. */
	problem = rcow_gib_problem("size_gib", req.size_gib, errbuf, sizeof(errbuf));
	if (problem) {
		rpc_lvol_respond_err(request, 0, problem);
		goto cleanup;
	}

	lvol = rpc_lookup_lvol(request, &req);
	if (!lvol) {
		goto cleanup;
	}

	/* One at a time. Two concurrent resizes would each read the blob's cluster
	 * count in their own completion, and whichever finished second would tell
	 * the bdev layer a size derived from a blob the other had already changed.
	 *
	 * spdk_lvol::action_in_progress exists for this and is unused upstream, so
	 * it only ever means "an s3lvol RPC is working on this lvol". */
	if (lvol->action_in_progress) {
		rpc_lvol_respond_err(request, 0,
				     "another operation is in progress on this lvol");
		goto cleanup;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}
	ctx->request = request;
	ctx->lvol = lvol;
	lvol->action_in_progress = true;

	rc = s3lvol_lvol_resize(lvol, RCOW_GIB_TO_BYTES(req.size_gib),
				rpc_resize_lvol_cb, ctx);
	if (rc != 0) {
		lvol->action_in_progress = false;
		free(ctx);
		rpc_lvol_respond_err(request, rc, NULL);
	}

cleanup:
	free_rpc_lvol_name(&req);
}
SPDK_RPC_REGISTER("rcow_resize_lvol", rpc_rcow_resize_lvol,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * lvol op callback for delete
 *
 * These were sharing rpc_lvstore_op_cb with the lvstore-level operations, but
 * the unified lvol response format wants the lvol name in the reply. A small
 * context carries it past the async boundary.
 * ========================================================================== */

struct rpc_lvol_op_ctx {
	struct spdk_jsonrpc_request *request;
	char                        *lvol_name;

	/* Set by the delete path. The volume is gone, so its activation record
	 * (if any) must go too -- otherwise the next recovery would try to
	 * re-attach a namespace for a volume that no longer exists. release_export
	 * reuses this callback but must not touch the registry. */
	bool                         drop_active;

	/* Which RPC this is, for the completion log: delete_lvol names a volume,
	 * release_export names an export uuid. */
	bool                         is_delete;
};

static void
rpc_lvol_op_cb(void *cb_arg, int lvolerrno)
{
	struct rpc_lvol_op_ctx *ctx = cb_arg;
	struct spdk_jsonrpc_request *request = ctx->request;
	char *name = ctx->lvol_name;
	const char *op = ctx->is_delete ? "rcow_delete_lvol" : "rcow_release_export";

	if (lvolerrno != 0) {
		/* The asynchronous failure -- the unregister, or spdk_lvol_destroy /
		 * the registry save -- lands here. The synchronous refusal paths
		 * (active check, export pins, clone count) log before returning;
		 * this covers the rest, and names the same thing the caller asked
		 * about. */
		SPDK_ERRLOG("%s '%s' failed: %s\n", op, name,
			    spdk_strerror(-lvolerrno));
		rpc_lvol_respond_err(request, lvolerrno, NULL);
		free(name);
		free(ctx);
		return;
	}

	/* Operation-level completion, complementing the module-level "Deleted lvol
	 * '<lvs>/<name>'" from s3lvol_lvol_destroyed: that one confirms the blob
	 * is gone, this one says the RPC finished -- including the active-record
	 * cleanup and the reply that the caller saw. */
	SPDK_NOTICELOG("%s '%s' completed\n", op, name);

	if (ctx->drop_active && s3lvol_active_load() == 0) {
		/* -ENOENT is fine: a volume that was never activated has no
		 * record to drop. */
		s3lvol_active_remove(name);
	}

	rpc_lvol_respond_ok(request, name);
	free(name);
	free(ctx);
}

/* Deletes the lvol *and its data*, unlike unload_lvstore. The S3 objects behind
 * it are removed too: the delete truncates the blob, and because every lvol is
 * created with LVOL_CLEAR_WITH_UNMAP the truncated clusters arrive at the bs_dev
 * as unmaps, each of which deletes the object it mapped (s3_unmap_chunk_removed).
 *
 * Two limits on that: the deletes are fire-and-forget -- s3_delete() is not
 * waited on, so an object can still be in S3 when this RPC answers -- and a
 * delete that fails to submit is logged and left for GC, which does not exist
 * yet. Either way the objects become orphans only when the delete did not
 * happen; the normal path reclaims them. Worth knowing before running this
 * against a large volume: the deletes land as a burst of requests. */
static void
rpc_rcow_delete_lvol(struct spdk_jsonrpc_request *request,
			    const struct spdk_json_val *params)
{
	struct rpc_lvol_name req = {0};
	struct rpc_lvol_op_ctx *ctx;
	struct spdk_lvol *lvol;
	int rc;

	if (spdk_json_decode_object(params, rpc_delete_lvol_decoders,
				    SPDK_COUNTOF(rpc_delete_lvol_decoders), &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	/* One line that a request even arrived, before anything can refuse it.
	 * The delete is async -- the RPC answers once the destroy completes -- so
	 * without this, a delete that is still in flight or was refused by a
	 * check below leaves no trace on the target. */
	SPDK_NOTICELOG("rcow_delete_lvol '%s' requested\n", req.lvol_name);

	lvol = rpc_lookup_lvol(request, &req);
	if (!lvol) {
		goto cleanup;
	}

	/* Deleting a volume the host is using would tear the namespace out from
	 * under it. The bdev layer can do that -- unregister notifies every open
	 * descriptor, and nvmf answers by removing the namespace -- but nobody
	 * should get there by accident: the I/O already in flight is aborted and
	 * the device disappears without the caller asking. Refuse while the
	 * volume is in the active registry, and name the way out. */
	if (s3lvol_active_load() != 0) {
		SPDK_WARNLOG("rcow_delete_lvol '%s' refused: the active registry "
			     "could not be read\n", req.lvol_name);
		rpc_lvol_respond_err(request, 0,
				     "the active registry could not be read");
		goto cleanup;
	}
	if (s3lvol_active_find(lvol->name)) {
		SPDK_WARNLOG("rcow_delete_lvol '%s' refused: volume is active "
			     "(exported over NVMf); run rcow_deactive_bdev first\n",
			     lvol->name);
		rpc_lvol_respond_err(request, -EBUSY,
				     "volume is active (exported over NVMf); "
				     "run rcow_deactive_bdev first");
		goto cleanup;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}
	ctx->request     = request;
	ctx->lvol_name   = strdup(lvol->name);
	ctx->drop_active = true;
	ctx->is_delete   = true;
	if (!ctx->lvol_name) {
		free(ctx);
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}

	rc = s3lvol_lvol_destroy(lvol, rpc_lvol_op_cb, ctx);
	if (rc != 0) {
		free(ctx->lvol_name);
		free(ctx);
		rpc_lvol_respond_err(request, rc, NULL);
	}

cleanup:
	free_rpc_lvol_name(&req);
}
SPDK_RPC_REGISTER("rcow_delete_lvol", rpc_rcow_delete_lvol,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_unload_lvstore
 *
 * Unload, not delete: the S3 data stays. Deliberately named after what it does --
 * "delete" would suggest the objects go away.
 *
 * This is also the only way to shut down cleanly on the WAL path: it drains
 * everything acknowledged so far into S3 and closes the log. Killing the process
 * instead leaves the tail of the log unconsumed, which is safe but needs the
 * attach path to recover.
 * ========================================================================== */

struct rpc_lvstore_name {
	char *lvs_name;
};

static const struct spdk_json_object_decoder rpc_lvstore_name_decoders[] = {
	{"lvs_name", offsetof(struct rpc_lvstore_name, lvs_name), spdk_json_decode_string, false},
};

static void
rpc_lvstore_op_cb(void *cb_arg, int lvserrno)
{
	struct spdk_jsonrpc_request *request = cb_arg;

	if (lvserrno != 0) {
		spdk_jsonrpc_send_error_response(request, lvserrno,
						 spdk_strerror(-lvserrno));
		return;
	}
	spdk_jsonrpc_send_bool_response(request, true);
}

static void
rpc_rcow_unload_lvstore(struct spdk_jsonrpc_request *request,
			       const struct spdk_json_val *params)
{
	struct rpc_lvstore_name req = {0};
	struct s3lvol_lvstore *lvs;

	if (spdk_json_decode_object(params, rpc_lvstore_name_decoders,
				    SPDK_COUNTOF(rpc_lvstore_name_decoders), &req)) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "Invalid parameters");
		goto cleanup;
	}

	lvs = s3lvol_lvstore_find(req.lvs_name);
	if (!lvs) {
		spdk_jsonrpc_send_error_response_fmt(request, -ENODEV,
						     "lvstore '%s' not found",
						     req.lvs_name);
		goto cleanup;
	}

	s3lvol_lvstore_unload(lvs, rpc_lvstore_op_cb, request);

cleanup:
	free(req.lvs_name);
}
SPDK_RPC_REGISTER("rcow_unload_lvstore", rpc_rcow_unload_lvstore,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_delete_lvstore
 *
 * Fully destroy an lvstore and remove it from bstore.json.
 * ========================================================================== */

struct rpc_delete_lvstore {
	char *lvs_name;
};

static void
free_rpc_delete_lvstore(struct rpc_delete_lvstore *req)
{
	free(req->lvs_name);
}

static const struct spdk_json_object_decoder rpc_delete_lvstore_decoders[] = {
	{"lvs_name", offsetof(struct rpc_delete_lvstore, lvs_name),
	 spdk_json_decode_string, false},
};

static void
rpc_rcow_delete_lvstore_cb(void *cb_arg, int lvserrno)
{
	struct spdk_jsonrpc_request *request = cb_arg;

	if (lvserrno != 0) {
		spdk_jsonrpc_send_error_response(request, lvserrno,
						 spdk_strerror(-lvserrno));
		return;
	}

	spdk_jsonrpc_send_bool_response(request, true);
}

static void
rpc_rcow_delete_lvstore(struct spdk_jsonrpc_request *request,
			const struct spdk_json_val *params)
{
	struct rpc_delete_lvstore req = {0};
	struct s3lvol_lvstore *lvs;

	if (spdk_json_decode_object(params, rpc_delete_lvstore_decoders,
				    SPDK_COUNTOF(rpc_delete_lvstore_decoders),
				    &req)) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "Invalid parameters");
		goto cleanup;
	}

	lvs = s3lvol_lvstore_find(req.lvs_name);
	if (!lvs) {
		/* Not loaded here, so there is nothing to delete objects from -- but
		 * a stale entry may still name it, and leaving that behind would send
		 * a recovery script after an lvstore that is not coming back. */
		bstore_remove_entry(req.lvs_name);
		spdk_jsonrpc_send_bool_response(request, true);
		goto cleanup;
	}

	s3lvol_lvstore_destroy(lvs, rpc_rcow_delete_lvstore_cb, request);

cleanup:
	free_rpc_delete_lvstore(&req);
}
SPDK_RPC_REGISTER("rcow_delete_lvstore", rpc_rcow_delete_lvstore,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_flush_lvstore
 *
 * Wait until everything acknowledged so far is in S3, without unloading.
 *
 * Its reason for existing is testing: once this returns, the read overlay is
 * empty, so any subsequent read has to come from S3. Without it a verify pass
 * could be satisfied entirely out of RAM and would prove nothing about the
 * objects actually written.
 * ========================================================================== */

static void
rpc_rcow_flush_lvstore(struct spdk_jsonrpc_request *request,
			      const struct spdk_json_val *params)
{
	struct rpc_lvstore_name req = {0};
	struct s3lvol_lvstore *lvs;

	if (spdk_json_decode_object(params, rpc_lvstore_name_decoders,
				    SPDK_COUNTOF(rpc_lvstore_name_decoders), &req)) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "Invalid parameters");
		goto cleanup;
	}

	lvs = s3lvol_lvstore_find(req.lvs_name);
	if (!lvs) {
		spdk_jsonrpc_send_error_response_fmt(request, -ENODEV,
						     "lvstore '%s' not found",
						     req.lvs_name);
		goto cleanup;
	}

	s3lvol_lvstore_flush(lvs, rpc_lvstore_op_cb, request);

cleanup:
	free(req.lvs_name);
}
SPDK_RPC_REGISTER("rcow_flush_lvstore", rpc_rcow_flush_lvstore,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_checkpoint_lvstore
 *
 * Snapshot the chunk map to S3 now and truncate the journal, without waiting for
 * the journal to reach its trigger threshold.
 *
 * Operationally this bounds recovery time before a planned restart: attach has to
 * replay everything the last checkpoint does not cover, so taking one first makes
 * the restart much faster.
 *
 * It is also the only practical way to exercise the checkpoint path. The automatic
 * trigger fires at 50% of the journal region, which for the 256 MiB default means
 * on the order of two million chunk uploads -- unreachable in a test, and the
 * first run of the recovery test duly showed "No checkpoint" on every attach.
 * ========================================================================== */

static void
rpc_rcow_checkpoint_lvstore(struct spdk_jsonrpc_request *request,
				   const struct spdk_json_val *params)
{
	struct rpc_lvstore_name req = {0};
	struct s3lvol_lvstore *lvs;

	if (spdk_json_decode_object(params, rpc_lvstore_name_decoders,
				    SPDK_COUNTOF(rpc_lvstore_name_decoders), &req)) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "Invalid parameters");
		goto cleanup;
	}

	lvs = s3lvol_lvstore_find(req.lvs_name);
	if (!lvs) {
		spdk_jsonrpc_send_error_response_fmt(request, -ENODEV,
						     "lvstore '%s' not found",
						     req.lvs_name);
		goto cleanup;
	}

	s3lvol_lvstore_checkpoint(lvs, rpc_lvstore_op_cb, request);

cleanup:
	free(req.lvs_name);
}
SPDK_RPC_REGISTER("rcow_checkpoint_lvstore",
		  rpc_rcow_checkpoint_lvstore, SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_add_cos_config
 *
 * Register a namespace that maps to a COS bucket. Meant to be called by a
 * startup script once per bucket before any lvstore is created or attached.
 *
 * The namespace name is typically the bucket name, which makes it easy to read
 * and keeps the mapping obvious. Later rcow_namespace_to_target() will be
 * replaced by whatever mapping service is used.
 *
 * Credentials are never passed through this RPC -- they come from the
 * environment (S3_AUTH_ENV), the same as before.
 * ========================================================================== */

struct rpc_add_cos_config {
	char *ns_name;
	char *endpoint;
	char *bucket;
	char *region;
	bool  path_style;
	bool  no_tls;
};

static void
free_rpc_add_cos_config(struct rpc_add_cos_config *req)
{
	free(req->ns_name);
	free(req->endpoint);
	free(req->bucket);
	free(req->region);
}

static const struct spdk_json_object_decoder rpc_add_cos_config_decoders[] = {
	{"namespace",  offsetof(struct rpc_add_cos_config, ns_name),  spdk_json_decode_string, false},
	{"endpoint",   offsetof(struct rpc_add_cos_config, endpoint),   spdk_json_decode_string, false},
	{"bucket",     offsetof(struct rpc_add_cos_config, bucket),     spdk_json_decode_string, false},
	{"region",     offsetof(struct rpc_add_cos_config, region),     spdk_json_decode_string, true},
	{"path_style", offsetof(struct rpc_add_cos_config, path_style), spdk_json_decode_bool,   true},
	{"no_tls",     offsetof(struct rpc_add_cos_config, no_tls),     spdk_json_decode_bool,   true},
};

static void
rpc_rcow_add_cos_config(struct spdk_jsonrpc_request *request,
			const struct spdk_json_val *params)
{
	struct rpc_add_cos_config req = {0};
	struct s3_target target = {0};
	int rc;

	if (spdk_json_decode_object(params, rpc_add_cos_config_decoders,
				    SPDK_COUNTOF(rpc_add_cos_config_decoders),
				    &req)) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "Invalid parameters");
		goto cleanup;
	}

	/* A literal rather than a strdup: rcow_namespace_add() copies whatever it
	 * is given, so there is nothing to own here, and a failed strdup would
	 * otherwise have handed it a NULL region. */
	target.endpoint       = req.endpoint;
	target.bucket         = req.bucket;
	target.region         = req.region ? req.region : (char *)"us-east-1";
	target.auth_mode      = S3_AUTH_ENV;
	target.use_path_style = req.path_style;
	target.verify_tls     = !req.no_tls;

	rc = rcow_namespace_add(req.ns_name, &target);
	if (rc != 0) {
		spdk_jsonrpc_send_error_response(request, rc, spdk_strerror(-rc));
	} else {
		struct spdk_json_write_ctx *w = spdk_jsonrpc_begin_result(request);
		spdk_json_write_object_begin(w);
		spdk_json_write_named_string(w, "namespace", req.ns_name);
		spdk_json_write_named_bool(w, "added", true);
		spdk_json_write_object_end(w);
		spdk_jsonrpc_end_result(request, w);
	}

cleanup:
	free_rpc_add_cos_config(&req);
}
SPDK_RPC_REGISTER("rcow_add_cos_config", rpc_rcow_add_cos_config, SPDK_RPC_RUNTIME)

/* ==========================================================================
 * rcow_get_lvstores
 * ========================================================================== */

static void
rpc_rcow_get_lvstores(struct spdk_jsonrpc_request *request,
			     const struct spdk_json_val *params)
{
	struct spdk_json_write_ctx *w;
	struct s3lvol_lvstore *lvs;

	if (params) {
		spdk_jsonrpc_send_error_response(request, SPDK_JSONRPC_ERROR_INVALID_PARAMS,
						 "method takes no parameters");
		return;
	}

	w = spdk_jsonrpc_begin_result(request);
	spdk_json_write_array_begin(w);

	for (lvs = s3lvol_lvstore_first(); lvs != NULL;
	     lvs = s3lvol_lvstore_next(lvs)) {
		struct spdk_lvol_store *store = s3lvol_lvstore_get_lvs(lvs);
		struct s3_bs_dev_stats stats = {0};
		struct spdk_lvol *lvol;

		spdk_json_write_object_begin(w);
		spdk_json_write_named_string(w, "lvs_name", s3lvol_lvstore_get_name(lvs));
		spdk_json_write_named_string(w, "namespace",
					     s3lvol_lvstore_get_namespace(lvs));
		spdk_json_write_named_uuid(w, "uuid", &store->uuid);
		spdk_json_write_named_uint64(w, "cluster_size",
					     spdk_bs_get_cluster_size(store->blobstore));
		spdk_json_write_named_uint64(w, "total_clusters",
					     spdk_bs_total_data_cluster_count(store->blobstore));
		spdk_json_write_named_uint64(w, "free_clusters",
					     spdk_bs_free_cluster_count(store->blobstore));

		/* Write-path counters. The test asserts on these: wal_writes > 0
		 * proves the log was actually used rather than silently bypassed,
		 * and chunks_flushed > 0 proves the data reached S3. */
		s3lvol_lvstore_get_stats(lvs, &stats);
		spdk_json_write_named_object_begin(w, "write_path");
		spdk_json_write_named_bool(w, "wal_attached", stats.wal_attached);
		spdk_json_write_named_uint64(w, "wal_writes", stats.wal_writes);
		spdk_json_write_named_uint64(w, "wal_retries", stats.wal_retries);
		spdk_json_write_named_uint64(w, "overlay_hits", stats.overlay_hits);
		spdk_json_write_named_uint64(w, "overlay_bytes", stats.overlay_bytes);
		spdk_json_write_named_uint64(w, "overlay_chunks",
					     stats.overlay_live_chunks);
		spdk_json_write_named_uint64(w, "chunks_flushed", stats.chunks_flushed);
		spdk_json_write_named_uint64(w, "flush_failures", stats.flush_failures);
		spdk_json_write_named_uint64(w, "rmw_count", stats.rmw_count);
		spdk_json_write_named_uint64(w, "allocated_chunks",
					     stats.allocated_chunks);
		/* Checkpoint state. journal_used vs journal_capacity is what tells
		 * an operator whether the lvstore is heading for the -ENOSPC that
		 * an untruncatable journal ends in. */
		spdk_json_write_named_uint64(w, "ckpt_done", stats.ckpt_done);
		spdk_json_write_named_uint64(w, "ckpt_failed", stats.ckpt_failed);
		spdk_json_write_named_uint64(w, "ckpt_gen", stats.ckpt_gen);
		spdk_json_write_named_uint64(w, "ckpt_lsn", stats.ckpt_lsn);
		spdk_json_write_named_uint32(w, "ckpt_interval_sec",
					     stats.ckpt_interval_sec);
		spdk_json_write_named_uint64(w, "journal_used",
					     stats.journal_used_bytes);
		spdk_json_write_named_uint64(w, "journal_capacity",
					     stats.journal_capacity_bytes);

		/* Why chunks were flushed. Mostly "full" is healthy; a steady
		 * stream of "forced" says the overlay cap cannot keep up. */
		spdk_json_write_named_uint64(w, "flushed_full",
					     stats.overlay_flushed_full);
		spdk_json_write_named_uint64(w, "flushed_aged",
					     stats.overlay_flushed_aged);
		spdk_json_write_named_uint64(w, "flushed_forced",
					     stats.overlay_flushed_forced);

		/* Local read cache. hits vs misses is the number that matters: a
		 * hit is a read that cost a local round trip instead of an S3 one.
		 * hits_declined is the object being here but not the blocks this
		 * read wanted, which is what partial residency trades for filling
		 * from reads at all. Reported inside write_path because that is
		 * where the flusher counters already are, and the cache is filled
		 * by the flusher as well as by reads. */
		spdk_json_write_named_bool(w, "cache_attached",
					   stats.cache_attached);
		if (stats.cache_attached) {
			spdk_json_write_named_uint64(w, "cache_hits",
						     stats.cache_hits);
			spdk_json_write_named_uint64(w, "cache_misses",
						     stats.cache_misses);
			spdk_json_write_named_uint64(w, "cache_hits_declined",
						     stats.cache_hits_declined);
			spdk_json_write_named_uint64(w, "cache_populates",
						     stats.cache_populates);
			spdk_json_write_named_uint64(w, "cache_populates_dropped",
						     stats.cache_populates_dropped);
			spdk_json_write_named_uint64(w, "cache_evictions",
						     stats.cache_evictions);
			spdk_json_write_named_uint64(w, "cache_bytes_served",
						     stats.cache_bytes_served);
			spdk_json_write_named_uint64(w, "cache_bytes_populated",
						     stats.cache_bytes_populated);
			spdk_json_write_named_uint64(w, "cache_slots_total",
						     stats.cache_slots_total);
			spdk_json_write_named_uint64(w, "cache_slots_resident",
						     stats.cache_slots_resident);
			spdk_json_write_named_uint64(w, "cache_bytes_resident",
						     stats.cache_bytes_resident);
		}
		spdk_json_write_object_end(w);

		spdk_json_write_named_array_begin(w, "lvols");
		TAILQ_FOREACH(lvol, &store->lvols, link) {
			spdk_json_write_object_begin(w);
			spdk_json_write_named_string(w, "name", lvol->name);
			if (lvol->bdev) {
				spdk_json_write_named_string(w, "bdev_name", lvol->bdev->name);
			}
			spdk_json_write_named_uuid(w, "uuid", &lvol->uuid);
			spdk_json_write_object_end(w);
		}
		spdk_json_write_array_end(w);

		spdk_json_write_object_end(w);
	}

	spdk_json_write_array_end(w);
	spdk_jsonrpc_end_result(request, w);
}
SPDK_RPC_REGISTER("rcow_get_lvstores", rpc_rcow_get_lvstores,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * Cross-node transfer: export / import / release
 *
 * The pause and resume entry points of a sandbox. Naming follows the rest of
 * this file -- lvs_name + lvol_name, never a bdev name -- and the source of an
 * import is given field by field rather than as an export_url: the url cannot
 * carry an endpoint, so it would have to be accompanied by one anyway, and half
 * a url plus a field is worse than fields.
 *
 * The url *is* reported back by the export, because it is the human-readable
 * handle to what was just produced.
 * ========================================================================== */

struct rpc_export_lvol {
	char *lvs_name;
	char *snapshot_name;
	char *export_id;

	/* How long the source promises to keep the snapshot, in seconds. 0 asks for
	 * the default; a zero-copy export without a deadline would pin a snapshot for
	 * as long as this node lives. */
	uint32_t ttl_sec;
};

static void
free_rpc_export_lvol(struct rpc_export_lvol *req)
{
	free(req->lvs_name);
	free(req->snapshot_name);
	free(req->export_id);
}

static const struct spdk_json_object_decoder rpc_export_lvol_decoders[] = {
	{"snapshot_name", offsetof(struct rpc_export_lvol, snapshot_name), spdk_json_decode_string, false},
	{"export_id",     offsetof(struct rpc_export_lvol, export_id),     spdk_json_decode_string, true},
	{"ttl_sec",    offsetof(struct rpc_export_lvol, ttl_sec),       spdk_json_decode_uint32, true},
};

static void
rpc_rcow_export_snapshot(struct spdk_jsonrpc_request *request,
			      const struct spdk_json_val *params)
{
	struct rpc_export_lvol req = {0};
	struct s3lvol_lvstore *lvs;
	struct spdk_lvol *lvol;
	char uuid[SPDK_UUID_STRING_LEN];
	int rc;

	if (spdk_json_decode_object(params, rpc_export_lvol_decoders,
				    SPDK_COUNTOF(rpc_export_lvol_decoders), &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	lvol = s3lvol_lvol_find_any(req.snapshot_name);
	if (!lvol) {
		rpc_lvol_respond_errf(request,
				      "snapshot '%s' not found in any lvstore",
				      req.snapshot_name);
		goto cleanup;
	}
	lvs = s3lvol_lvstore_of_lvol(lvol);
	if (!lvs) {
		rpc_lvol_respond_err(request, 0, "internal: lvol without lvstore");
		goto cleanup;
	}

	/* The uuid is answered the moment it is known; the export itself keeps
	 * running in the background and is polled with rcow_get_snapshot_status.
	 * There is no completion callback: a later failure surfaces there as
	 * NONE. */
	rc = s3lvol_lvol_export(lvs, lvol, req.export_id,
			req.ttl_sec ? req.ttl_sec : S3LVOL_EXPORT_DEFAULT_TTL_SEC,
			uuid, sizeof(uuid), NULL, NULL);
	if (rc != 0) {
		/* Named rather than left as a bare errno. The failure reaches the
		 * caller as whatever spdk_strerror makes of rc, and "Invalid argument"
		 * by itself says neither which snapshot nor which of the several
		 * reasons it was -- and a client that only reads stdout sees nothing at
		 * all, since the message travels in the failed reply. The target log
		 * carries the detail; this at least says what the call was about. */
		rpc_lvol_respond_errf(request,
				      "could not start exporting snapshot '%s': %s "
				      "(the target log says why)",
				      req.snapshot_name, spdk_strerror(-rc));
		goto cleanup;
	}

	rpc_lvol_respond_ok(request, uuid);

cleanup:
	free_rpc_export_lvol(&req);
}
SPDK_RPC_REGISTER("rcow_export_snapshot", rpc_rcow_export_snapshot,
		  SPDK_RPC_RUNTIME)

struct rpc_export_status {
	char *export_uuid;
	char *snapshot_name;
};

static void
free_rpc_export_status(struct rpc_export_status *req)
{
	free(req->export_uuid);
	free(req->snapshot_name);
}

/* Both optional, exactly one required -- see the note on the handler. */
static const struct spdk_json_object_decoder rpc_export_status_decoders[] = {
	{"export_uuid", offsetof(struct rpc_export_status, export_uuid),
	 spdk_json_decode_string, true},
	{"snapshot_name", offsetof(struct rpc_export_status, snapshot_name),
	 spdk_json_decode_string, true},
};

static const char *
export_state_str(enum s3lvol_export_state state)
{
	switch (state) {
	case S3LVOL_EXPORT_STATE_INPROGRESS:
		return "INPROGRESS";
	case S3LVOL_EXPORT_STATE_DONE:
		return "DONE";
	case S3LVOL_EXPORT_STATE_NONE:
	default:
		return "NONE";
	}
}

/* Status of one export, or of a snapshot regardless of whether it has one.
 *
 * Takes either export_uuid or snapshot_name. The snapshot form exists because a
 * snapshot that was never exported still has a deletable worth asking about, and
 * there is no uuid to ask with; it also covers a snapshot exported more than
 * once, which no single uuid describes.
 *
 * A failed reply means the named thing does not exist, in both forms. What
 * differs is what was named: an uuid matching no export is refused, while a
 * snapshot that exists and has never been exported is reported as NONE. So an
 * export that failed asynchronously reads as a refusal when asked for by uuid,
 * and as NONE when asked for by snapshot -- both true, from either end. */
static void
rpc_rcow_get_snapshot_status(struct spdk_jsonrpc_request *request,
			     const struct spdk_json_val *params)
{
	struct rpc_export_status req = {0};
	struct rpc_json_buf buf;
	struct spdk_json_write_ctx *w;
	enum s3lvol_export_state state;
	bool deletable;
	int rc;

	if (spdk_json_decode_object(params, rpc_export_status_decoders,
				    SPDK_COUNTOF(rpc_export_status_decoders),
				    &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	/* Refusing both rather than picking one: they can name different things, and
	 * answering about only one of them silently would be worse than saying no. */
	if (req.export_uuid && req.snapshot_name) {
		rpc_lvol_respond_err(request, 0,
				     "export_uuid and snapshot_name are mutually "
				     "exclusive");
		goto cleanup;
	}
	if (!req.export_uuid && !req.snapshot_name) {
		rpc_lvol_respond_err(request, 0,
				     "either export_uuid or snapshot_name is "
				     "required");
		goto cleanup;
	}

	if (req.export_uuid) {
		rc = s3lvol_export_query(req.export_uuid, &state, &deletable);
	} else {
		rc = s3lvol_snapshot_query(req.snapshot_name, &state, &deletable);
		if (rc == -ENODEV) {
			rpc_lvol_respond_errf(request,
					      "snapshot '%s' not found in any "
					      "lvstore", req.snapshot_name);
			goto cleanup;
		}
	}
	if (rc != 0) {
		rpc_lvol_respond_err(request, rc, NULL);
		goto cleanup;
	}

	/* Only the uuid form refuses NONE: there the caller named an export, and
	 * nothing by that name exists. The snapshot form has already established
	 * that the snapshot is there, so NONE describes it. */
	if (state == S3LVOL_EXPORT_STATE_NONE && req.export_uuid) {
		rpc_lvol_respond_errf(request, "export '%s' does not exist",
				      req.export_uuid);
		goto cleanup;
	}

	w = rpc_json_buf_begin(&buf);
	if (!w) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}

	spdk_json_write_object_begin(w);
	spdk_json_write_named_string(w, "export_status",
				     export_state_str(state));
	spdk_json_write_named_string(w, "deletable",
				     deletable ? "YES" : "NO");
	spdk_json_write_object_end(w);
	rpc_json_buf_respond(request, w, &buf);

cleanup:
	free_rpc_export_status(&req);
}
SPDK_RPC_REGISTER("rcow_get_snapshot_status", rpc_rcow_get_snapshot_status,
		  SPDK_RPC_RUNTIME)

struct rpc_import_lvol {
	char *lvs_name;
	char *lvol_name;
	char *export_uuid;
	char *src_namespace;

	/* Decouple in the background once the clone exists, instead of leaving the
	 * volume reading through to the export until somebody asks. The import still
	 * answers as soon as the volume is usable.
	 *
	 * On by default. A volume left reading through to the export keeps depending
	 * on it past the export's TTL -- which the importer does not renew -- so the
	 * source could delete the snapshot out from under it. Reading through is the
	 * explicit opt-out, for callers that will manage that dependency themselves. */
	bool  decouple;
};

static void
free_rpc_import_lvol(struct rpc_import_lvol *req)
{
	free(req->lvs_name);
	free(req->lvol_name);
	free(req->export_uuid);
	free(req->src_namespace);
}

static const struct spdk_json_object_decoder rpc_import_lvol_decoders[] = {
	{"lvol_name",     offsetof(struct rpc_import_lvol, lvol_name),     spdk_json_decode_string, false},
	{"export_uuid",   offsetof(struct rpc_import_lvol, export_uuid),   spdk_json_decode_string, false},
	{"src_namespace", offsetof(struct rpc_import_lvol, src_namespace), spdk_json_decode_string, true},
	{"lvs_name",      offsetof(struct rpc_import_lvol, lvs_name),      spdk_json_decode_string, true},
	{"decouple",      offsetof(struct rpc_import_lvol, decouple),      spdk_json_decode_bool,   true},
};

/* The lvstore a call means when it did not say. Sends the error response itself
 * and answers NULL, so callers can just return.
 *
 * Refusing when several are loaded rather than picking the first is the whole
 * point: for a release, the wrong lvstore means deleting objects that belong to
 * somebody else's export. Which is also why the message says what to do about it
 * -- the old one claimed no lvstore was available, when the truth was there were
 * too many to choose from. */
static struct s3lvol_lvstore *
rpc_lvstore_for(struct spdk_jsonrpc_request *request, const char *lvs_name)
{
	struct s3lvol_lvstore *lvs;

	if (lvs_name != NULL) {
		lvs = s3lvol_lvstore_find(lvs_name);
		if (!lvs) {
			rpc_lvol_respond_errf(request, "lvstore '%s' is not loaded",
					      lvs_name);
		}
		return lvs;
	}

	lvs = s3lvol_lvstore_pick_one();
	if (!lvs) {
		if (s3lvol_lvstore_first() != NULL) {
			rpc_lvol_respond_err(request, 0,
					"more than one lvstore is loaded; say which one "
					"with lvs_name");
		} else {
			rpc_lvol_respond_err(request, 0,
					"no lvstore is loaded; attach or create one first");
		}
	}
	return lvs;
}

static void
rpc_import_lvol_cb(void *cb_arg, struct spdk_lvol *lvol, int lvolerrno)
{
	struct spdk_jsonrpc_request *request = cb_arg;
	struct spdk_json_write_ctx *w;
	const char *mode;

	if (lvolerrno != 0) {
		rpc_lvol_respond_err(request, lvolerrno, NULL);
		return;
	}

	/* The one reply in this file with a third field, and the reason is that an
	 * import no longer has one implementation. When the export turns out to
	 * name a snapshot this lvstore still holds, it degenerates into a local
	 * clone: no import registry entry, nothing for rcow_release_export to be
	 * held up by, and no dependency on the export surviving. That is a
	 * different object with different obligations, so a caller keeping track of
	 * what depends on what has to be able to tell -- and the alternative,
	 * inferring it from whether the volume later shows up in rcow_get_imports,
	 * is a question nobody should have to ask.
	 *
	 * Additive, so anything reading bool_value/string_value is unaffected.
	 *
	 * Read off the blob rather than passed down from the decision: this cannot
	 * disagree with what was actually built. */
	mode = spdk_blob_is_esnap_clone(lvol->blob) ? "esnap" : "local_clone";

	w = spdk_jsonrpc_begin_result(request);
	spdk_json_write_object_begin(w);
	spdk_json_write_named_bool(w, "bool_value", true);
	spdk_json_write_named_string(w, "string_value", lvol->name);
	spdk_json_write_named_string(w, "mode", mode);
	spdk_json_write_object_end(w);
	spdk_jsonrpc_end_result(request, w);
}

static void
rpc_rcow_import_lvol(struct spdk_jsonrpc_request *request,
			    const struct spdk_json_val *params)
{
	/* decouple defaults to true; the decoder writes the field only when the
	 * call says so, so an absent flag keeps the default while an explicit false
	 * overrides it. */
	struct rpc_import_lvol req = { .decouple = true };
	struct s3lvol_import_opts opts = {0};
	struct s3lvol_lvstore *lvs;
	int rc;

	if (spdk_json_decode_object(params, rpc_import_lvol_decoders,
				    SPDK_COUNTOF(rpc_import_lvol_decoders), &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	lvs = rpc_lvstore_for(request, req.lvs_name);
	if (!lvs) {
		goto cleanup;
	}

	opts.lvol_name     = req.lvol_name;
	opts.export_uuid   = req.export_uuid;
	opts.src_namespace = req.src_namespace;
	opts.decouple      = req.decouple;

	rc = s3lvol_lvol_import(lvs, &opts, rpc_import_lvol_cb, request);
	if (rc != 0) {
		rpc_lvol_respond_err(request, rc, NULL);
	}

cleanup:
	free_rpc_import_lvol(&req);
}
SPDK_RPC_REGISTER("rcow_import_lvol", rpc_rcow_import_lvol,
		  SPDK_RPC_RUNTIME)

/* Stop an imported volume from reading through to its export.
 *
 * Answers as soon as the copying has started, not when it has finished: it moves
 * as much data as the export holds, and a JSON-RPC call is the wrong place to wait
 * for that. Progress is in rcow_get_decouple, and the outcome is in the log.
 *
 * The volume is readable and writable throughout. Snapshot, clone, resize and
 * delete of it are refused until this finishes. */
static void
rpc_rcow_decouple_lvol(struct spdk_jsonrpc_request *request,
			    const struct spdk_json_val *params)
{
	struct rpc_lvol_name req = {0};
	struct s3lvol_lvstore *lvs;
	struct spdk_lvol *lvol;
	int rc;

	if (spdk_json_decode_object(params, rpc_delete_lvol_decoders,
				    SPDK_COUNTOF(rpc_delete_lvol_decoders), &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	lvol = s3lvol_lvol_find_any(req.lvol_name);
	if (!lvol) {
		rpc_lvol_respond_errf(request, "lvol '%s' not found in any lvstore",
				      req.lvol_name);
		goto cleanup;
	}
	lvs = s3lvol_lvstore_of_lvol(lvol);
	if (!lvs) {
		rpc_lvol_respond_err(request, 0, "internal: lvol without lvstore");
		goto cleanup;
	}

	rc = s3lvol_lvol_decouple(lvs, lvol, NULL, NULL);
	if (rc != 0) {
		rpc_lvol_respond_err(request, rc, NULL);
		goto cleanup;
	}

	rpc_lvol_respond_ok(request, lvol->name);

cleanup:
	free_rpc_lvol_name(&req);
}
SPDK_RPC_REGISTER("rcow_decouple_lvol", rpc_rcow_decouple_lvol,
		  SPDK_RPC_RUNTIME)

/* The decouples running right now, with how far each has got.
 *
 * The list arrives as a JSON array in string_value, so the caller parses the
 * reply twice; see the note on rpc_json_buf.
 *
 * clusters_total is what the export's manifest says holds data, not the size of
 * the volume -- the whole point of a decouple is that those two differ. An empty
 * array means nothing is copying, which is also the answer once a decouple has
 * finished; whether it finished or failed is in the log, and whether it took
 * effect is visible as the volume no longer appearing in rcow_get_imports. */
static void
rpc_rcow_get_decouple(struct spdk_jsonrpc_request *request,
			   const struct spdk_json_val *params)
{
	struct rpc_json_buf buf;
	struct spdk_json_write_ctx *w;
	struct s3lvol_decouple *d;

	/* Takes no parameters, but an empty object is accepted as well as none: some
	 * clients always send one. Anything with a key in it is a typo worth
	 * reporting rather than ignoring. */
	if (params != NULL && spdk_json_decode_object(params, NULL, 0, NULL)) {
		rpc_lvol_respond_err(request, 0,
				     "rcow_get_decouple takes no parameters");
		return;
	}

	w = rpc_json_buf_begin(&buf);
	if (!w) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		return;
	}

	spdk_json_write_array_begin(w);

	for (d = s3lvol_decouple_first(); d != NULL; d = s3lvol_decouple_next(d)) {
		struct s3lvol_decouple_info info;

		s3lvol_decouple_get(d, &info);

		spdk_json_write_object_begin(w);
		spdk_json_write_named_string(w, "lvs_name", info.lvs_name);
		spdk_json_write_named_string(w, "lvol_name", info.lvol_name);
		spdk_json_write_named_string(w, "export_uuid", info.export_uuid);
		spdk_json_write_named_uint64(w, "clusters_total", info.clusters_total);
		spdk_json_write_named_uint64(w, "clusters_done", info.clusters_done);
		spdk_json_write_named_bool(w, "queued", false);
		spdk_json_write_object_end(w);
	}

	/* Queued volumes are part of the same answer: only one volume materialises a
	 * given export at a time, so a caller that waits for this list to empty is
	 * waiting for all of them. Distinguished by "queued" rather than by absence,
	 * so a client can tell "waiting" from "0 of 0 copied". */
	struct decouple_queued *q;

	for (q = s3lvol_decouple_queued_first(); q != NULL;
	     q = s3lvol_decouple_queued_next(q)) {
		struct s3lvol_decouple_info info;

		s3lvol_decouple_queued_get(q, &info);

		spdk_json_write_object_begin(w);
		spdk_json_write_named_string(w, "lvs_name", info.lvs_name);
		spdk_json_write_named_string(w, "lvol_name", info.lvol_name);
		spdk_json_write_named_string(w, "export_uuid",
					     info.export_uuid);
		spdk_json_write_named_uint64(w, "clusters_total",
					     info.clusters_total);
		spdk_json_write_named_uint64(w, "clusters_done",
					     info.clusters_done);
		spdk_json_write_named_bool(w, "queued", true);
		spdk_json_write_object_end(w);
	}

	spdk_json_write_array_end(w);
	rpc_json_buf_respond(request, w, &buf);
}
SPDK_RPC_REGISTER("rcow_get_decouple", rpc_rcow_get_decouple,
		  SPDK_RPC_RUNTIME)

struct rpc_export_id {
	char *lvs_name;
	char *export_uuid;
};

static const struct spdk_json_object_decoder rpc_export_id_decoders[] = {
	{"export_uuid", offsetof(struct rpc_export_id, export_uuid), spdk_json_decode_string, false},
	{"lvs_name",    offsetof(struct rpc_export_id, lvs_name),    spdk_json_decode_string, true},
};

/* Delete an export's objects. Fails with -EBUSY while a clone in this process
 * still reads through to it; across nodes there is nothing to check, which is
 * why this is called by the importer once its clone no longer needs the
 * export.
 *
 * lvs_name is what makes that check worth anything. The check can only see clones
 * in lvstores that are loaded, so it only means something when the importing
 * lvstore is one of them -- and then there is more than one lvstore loaded, which
 * is exactly when a call without lvs_name has to be refused. */
static void
rpc_rcow_release_export(struct spdk_jsonrpc_request *request,
			       const struct spdk_json_val *params)
{
	struct rpc_export_id req = {0};
	struct rpc_lvol_op_ctx *ctx;
	struct s3lvol_lvstore *lvs;
	int rc;

	if (spdk_json_decode_object(params, rpc_export_id_decoders,
				    SPDK_COUNTOF(rpc_export_id_decoders), &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	lvs = rpc_lvstore_for(request, req.lvs_name);
	if (!lvs) {
		goto cleanup;
	}

	/* The reply names what was released, which is the export uuid rather than
	 * an lvol -- the context field is called lvol_name because the delete path
	 * uses it for one, and both only ever need "the thing this call was
	 * about". */
	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}
	ctx->request   = request;
	ctx->lvol_name = strdup(req.export_uuid);
	if (!ctx->lvol_name) {
		free(ctx);
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}

	rc = s3lvol_export_release(lvs, req.export_uuid, rpc_lvol_op_cb, ctx);
	if (rc != 0) {
		free(ctx->lvol_name);
		free(ctx);
		rpc_lvol_respond_err(request, rc, NULL);
	}

cleanup:
	free(req.lvs_name);
	free(req.export_uuid);
}
SPDK_RPC_REGISTER("rcow_release_export", rpc_rcow_release_export,
		  SPDK_RPC_RUNTIME)

/* What this node currently reads through to.
 *
 * The list arrives as a JSON array in string_value; see the note on rpc_json_buf.
 *
 * There is no bdev_s3lvol_list_exports counterpart yet: listing what a *bucket*
 * holds needs s3_list_objects(), which is still -ENOTSUP. This one is answered
 * from the in-memory registry, and it is the interesting direction anyway --
 * "which of my volumes still depend on somebody else". */
static void
rpc_rcow_get_imports(struct spdk_jsonrpc_request *request,
			    const struct spdk_json_val *params)
{
	struct rpc_lvstore_name req = {0};
	struct s3lvol_lvstore *lvs;
	struct rpc_json_buf buf;
	struct spdk_json_write_ctx *w;

	if (params && spdk_json_decode_object(params, rpc_lvstore_name_decoders,
					      SPDK_COUNTOF(rpc_lvstore_name_decoders),
					      &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		free(req.lvs_name);
		return;
	}

	w = rpc_json_buf_begin(&buf);
	if (!w) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		free(req.lvs_name);
		return;
	}

	spdk_json_write_array_begin(w);

	for (lvs = s3lvol_lvstore_first(); lvs != NULL; lvs = s3lvol_lvstore_next(lvs)) {
		struct s3lvol_import *imp;

		if (req.lvs_name && strcmp(req.lvs_name, s3lvol_lvstore_get_name(lvs)) != 0) {
			continue;
		}
		for (imp = s3lvol_import_first(lvs); imp != NULL;
		     imp = s3lvol_import_next(imp)) {
			const struct s3_export_manifest *m = s3lvol_import_get_manifest(imp);

			spdk_json_write_object_begin(w);
			spdk_json_write_named_string(w, "lvs_name",
						     s3lvol_lvstore_get_name(lvs));
			spdk_json_write_named_string(w, "export_uuid", m->uuid_str);
			spdk_json_write_named_uint64(w, "size_bytes", m->size_bytes);
			spdk_json_write_named_uint32(w, "chunk_size", m->chunk_size);
			spdk_json_write_named_uint64(w, "num_chunks", m->num_chunks);
			spdk_json_write_named_uint64(w, "present_chunks",
						     m->present_chunks);
			spdk_json_write_named_object_begin(w, "source");
			spdk_json_write_named_string(w, "endpoint", m->src.endpoint);
			spdk_json_write_named_string(w, "bucket", m->src.bucket);
			spdk_json_write_named_string(w, "prefix", m->src.prefix);
			spdk_json_write_named_string(w, "lvs_name", m->src.lvs_name);
			spdk_json_write_named_string(w, "snapshot", m->src.snapshot);
			spdk_json_write_object_end(w);
			spdk_json_write_object_end(w);
		}
	}

	spdk_json_write_array_end(w);
	rpc_json_buf_respond(request, w, &buf);
	free(req.lvs_name);
}
SPDK_RPC_REGISTER("rcow_get_imports", rpc_rcow_get_imports,
		  SPDK_RPC_RUNTIME)

/* ==========================================================================
 * Activation: exposing an lvol or snapshot as an NVMe namespace
 *
 * rcow_active_bdev      attach a volume to its subsystem
 * rcow_deactive_bdev    detach it again
 * rcow_get_bdev         report the host device path it landed on
 *
 * Placement is derived rather than chosen by the caller: the subsystem is
 * crc32c(name) % RCOW_NUM_SUBSYS and the nsid is the lowest free slot in it.
 * Both can be overridden, which is what recovery does -- it has to reproduce the
 * previous layout exactly rather than let it be recomputed.
 * ========================================================================== */

struct rpc_active_bdev {
	char     *device_name;
	/* UINT32_MAX means "not given". Zero cannot serve as the sentinel because
	 * subsystem indices are 0-based, so subsys 0 is a real placement -- one in
	 * every RCOW_NUM_SUBSYS volumes hashes there, and recovery has to be able
	 * to ask for it explicitly. nsid needs no such trick: NVMe namespaceids
	 * start at 1. */
	uint32_t  subsys;
	uint32_t  nsid;
};

#define RCOW_SUBSYS_UNSET UINT32_MAX

static const struct spdk_json_object_decoder rpc_active_bdev_decoders[] = {
	{"device_name", offsetof(struct rpc_active_bdev, device_name), spdk_json_decode_string, false},
	/* Recovery supplies both; ordinary activation supplies neither. */
	{"subsys",      offsetof(struct rpc_active_bdev, subsys),      spdk_json_decode_uint32, true},
	{"nsid",        offsetof(struct rpc_active_bdev, nsid),        spdk_json_decode_uint32, true},
};

struct active_bdev_ctx {
	struct spdk_jsonrpc_request *request;
	char      name[SPDK_LVOL_NAME_MAX];
	char      uuid[SPDK_UUID_STRING_LEN];
	char      nqn[SPDK_NVMF_NQN_MAX_LEN + 1];
	uint32_t  subsys;
};

/* The placement, as the document that goes into string_value. Shared by the two
 * paths that report one -- a fresh attach and a repeat call on something already
 * active -- so the two cannot drift apart.
 *
 * already_active is written only when it is true, which keeps an ordinary
 * activation's payload exactly what it was before the envelope existed. */
static void
rpc_active_respond(struct spdk_jsonrpc_request *request, const char *name,
		   const char *uuid, const char *nqn, uint32_t subsys,
		   uint32_t nsid, bool already_active)
{
	struct rpc_json_buf buf;
	struct spdk_json_write_ctx *w;

	w = rpc_json_buf_begin(&buf);
	if (!w) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		return;
	}

	spdk_json_write_object_begin(w);
	spdk_json_write_named_string(w, "device_name", name);
	spdk_json_write_named_string(w, "uuid", uuid);
	spdk_json_write_named_string(w, "nqn", nqn);
	spdk_json_write_named_uint32(w, "subsys", subsys);
	spdk_json_write_named_uint32(w, "nsid", nsid);
	if (already_active) {
		spdk_json_write_named_bool(w, "already_active", true);
	}
	spdk_json_write_object_end(w);

	rpc_json_buf_respond(request, w, &buf);
}

/* The namespace is attached; record it and report. The device path is left out:
 * the host needs a moment to notice a new namespace, and waiting for it here
 * would stall the reactor for the duration. Callers poll rcow_get_bdev. */
static void
rpc_active_bdev_attached(void *cb_arg, uint32_t nsid, int status)
{
	struct active_bdev_ctx *ctx = cb_arg;
	int rc;

	if (status != 0) {
		rpc_lvol_respond_errf(ctx->request,
				      "could not attach '%s' to %s: %s",
				      ctx->name, ctx->nqn, spdk_strerror(-status));
		free(ctx);
		return;
	}

	/* Persisted before answering. A namespace that is live but unrecorded gets
	 * silently dropped by the next recovery, after the caller was told it was
	 * activated. */
	rc = s3lvol_active_add(ctx->name, ctx->uuid, ctx->subsys, nsid);
	if (rc != 0) {
		SPDK_ERRLOG("'%s' is attached as %s nsid %" PRIu32 " but could not "
			    "be recorded: %s. Decoupling again so the two do not "
			    "disagree\n", ctx->name, ctx->nqn, nsid,
			    spdk_strerror(-rc));
		s3lvol_nvmf_remove_ns(ctx->nqn, nsid, NULL, NULL);
		rpc_lvol_respond_errf(ctx->request,
				      "could not record '%s' in the active "
				      "registry: %s", ctx->name, spdk_strerror(-rc));
		free(ctx);
		return;
	}

	rpc_active_respond(ctx->request, ctx->name, ctx->uuid, ctx->nqn,
			   ctx->subsys, nsid, false);

	SPDK_NOTICELOG("activated '%s' as %s nsid %" PRIu32 "\n", ctx->name,
		       ctx->nqn, nsid);
	free(ctx);
}

static void
rpc_rcow_active_bdev(struct spdk_jsonrpc_request *request,
		     const struct spdk_json_val *params)
{
	struct rpc_active_bdev req = { .subsys = RCOW_SUBSYS_UNSET };
	struct active_bdev_ctx *ctx = NULL;
	const struct s3lvol_active_entry *existing;
	struct s3lvol_lvstore *lvs;
	struct spdk_lvol *lvol;
	char bdev_name[SPDK_LVS_NAME_MAX + SPDK_LVOL_NAME_MAX + 2];
	uint32_t subsys, nsid;
	int rc;

	if (spdk_json_decode_object(params, rpc_active_bdev_decoders,
				    SPDK_COUNTOF(rpc_active_bdev_decoders), &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	if (s3lvol_active_load() != 0) {
		rpc_lvol_respond_err(request, 0,
				     "the active registry could not be read");
		goto cleanup;
	}

	lvol = s3lvol_lvol_find_any(req.device_name);
	if (!lvol) {
		rpc_lvol_respond_errf(request, "no such lvol or snapshot: %s",
				      req.device_name);
		goto cleanup;
	}

	lvs = s3lvol_lvstore_of_lvol(lvol);
	if (!lvs) {
		rpc_lvol_respond_errf(request, "'%s' has no lvstore",
				      req.device_name);
		goto cleanup;
	}

	/* Already active: report where it is rather than adding a second namespace
	 * for the same volume. Activation has to be repeatable, because a caller
	 * that timed out will retry. */
	existing = s3lvol_active_find(req.device_name);
	if (existing) {
		char nqn[SPDK_NVMF_NQN_MAX_LEN + 1];

		if (strcmp(existing->uuid, lvol->uuid_str) != 0) {
			/* Same name, different volume: the recorded namespace still
			 * refers to whatever was there before. */
			rpc_lvol_respond_errf(request,
				"'%s' is recorded as active with uuid %s but the "
				"volume of that name now has uuid %s; deactivate "
				"the stale entry first",
				req.device_name, existing->uuid, lvol->uuid_str);
			goto cleanup;
		}

		s3lvol_nvmf_subsys_nqn(existing->subsys, nqn, sizeof(nqn));

		/* A caller that named a placement is not asking "is it up?", it is
		 * asserting where it belongs -- which is what recovery does. Returning
		 * success while it sits somewhere else would tell the caller the layout
		 * was reproduced when it was not. Moving it silently would be worse
		 * still: the host would see the namespace vanish and reappear
		 * elsewhere. So neither; say what is wrong and let the caller decide. */
		if ((req.subsys != RCOW_SUBSYS_UNSET && req.subsys != existing->subsys) ||
		    (req.nsid != 0 && req.nsid != existing->nsid)) {
			rpc_lvol_respond_errf(request,
				"'%s' is already active at subsys %" PRIu32 " nsid "
				"%" PRIu32 ", not at the requested subsys %" PRIu32
				" nsid %" PRIu32 "; deactivate it first to move it",
				existing->name, existing->subsys, existing->nsid,
				req.subsys == RCOW_SUBSYS_UNSET ? existing->subsys
								: req.subsys,
				req.nsid ? req.nsid : existing->nsid);
			goto cleanup;
		}

		rpc_active_respond(request, existing->name, existing->uuid, nqn,
				   existing->subsys, existing->nsid, true);
		goto cleanup;
	}

	subsys = (req.subsys != RCOW_SUBSYS_UNSET) ? req.subsys
						: s3lvol_active_hash_subsys(req.device_name);
	if (subsys >= RCOW_NUM_SUBSYS) {
		rpc_lvol_respond_errf(request,
				      "subsys %" PRIu32 " is out of range (0..%d)",
				      subsys, RCOW_NUM_SUBSYS - 1);
		goto cleanup;
	}

	if (!s3lvol_nvmf_subsys_exists(subsys)) {
		rpc_lvol_respond_errf(request,
			"subsystem %" PRIu32 " does not exist; the startup script "
			"is supposed to have created all %d of them", subsys,
			RCOW_NUM_SUBSYS);
		goto cleanup;
	}

	if (req.nsid != 0) {
		/* Recovery path: that exact slot or nothing. Falling back to a free
		 * one would change the host-side layout, which is precisely what this
		 * parameter exists to preserve. */
		const struct s3lvol_active_entry *holder;

		if (req.nsid > RCOW_NS_PER_SUBSYS) {
			rpc_lvol_respond_errf(request,
				"nsid %" PRIu32 " is out of range (1..%d)",
				req.nsid, RCOW_NS_PER_SUBSYS);
			goto cleanup;
		}
		holder = s3lvol_active_find_by_nsid(subsys, req.nsid);
		if (holder) {
			rpc_lvol_respond_errf(request,
				"subsys %" PRIu32 " nsid %" PRIu32 " is already held "
				"by '%s'", subsys, req.nsid, holder->name);
			goto cleanup;
		}
		nsid = req.nsid;
	} else {
		nsid = s3lvol_active_alloc_nsid(subsys);
		if (nsid == 0) {
			rpc_lvol_respond_errf(request,
				"subsystem %" PRIu32 " has no free namespace slot "
				"(%d in use)", subsys, RCOW_NS_PER_SUBSYS);
			goto cleanup;
		}
	}

	rc = snprintf(bdev_name, sizeof(bdev_name), "%s/%s",
		      s3lvol_lvstore_get_name(lvs), lvol->name);
	if (rc < 0 || (size_t)rc >= sizeof(bdev_name)) {
		rpc_lvol_respond_err(request, 0, "bdev name too long");
		goto cleanup;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}
	ctx->request = request;
	ctx->subsys  = subsys;
	snprintf(ctx->name, sizeof(ctx->name), "%s", req.device_name);
	snprintf(ctx->uuid, sizeof(ctx->uuid), "%s", lvol->uuid_str);
	s3lvol_nvmf_subsys_nqn(subsys, ctx->nqn, sizeof(ctx->nqn));

	rc = s3lvol_nvmf_add_ns(ctx->nqn, bdev_name, nsid,
				rpc_active_bdev_attached, ctx);
	if (rc != 0) {
		rpc_lvol_respond_errf(request,
				      "could not start the attach of '%s': %s",
				      req.device_name, spdk_strerror(-rc));
		free(ctx);
	}

cleanup:
	free(req.device_name);
}
SPDK_RPC_REGISTER("rcow_active_bdev", rpc_rcow_active_bdev, SPDK_RPC_RUNTIME)

/* -------------------------------------------------------------------------- */

struct rpc_deactive_bdev {
	char *device_name;
};

static const struct spdk_json_object_decoder rpc_deactive_bdev_decoders[] = {
	{"device_name", offsetof(struct rpc_deactive_bdev, device_name), spdk_json_decode_string, false},
};

struct deactive_ctx {
	struct spdk_jsonrpc_request *request;
	char name[SPDK_LVOL_NAME_MAX];
};

static void
rpc_deactive_detached(void *cb_arg, uint32_t nsid, int status)
{
	struct deactive_ctx *ctx = cb_arg;
	int rc;

	if (status != 0) {
		rpc_lvol_respond_errf(ctx->request, "could not detach '%s': %s",
				      ctx->name, spdk_strerror(-status));
		free(ctx);
		return;
	}

	/* Dropped only after the namespace is really gone, so that an interrupted
	 * deactivation leaves an entry which still describes reality. */
	rc = s3lvol_active_remove(ctx->name);
	if (rc != 0 && rc != -ENOENT) {
		SPDK_ERRLOG("'%s' was decoupled but its registry entry could not be "
			    "removed: %s. The next recovery will try to reattach "
			    "it\n", ctx->name, spdk_strerror(-rc));
		rpc_lvol_respond_errf(ctx->request,
				      "'%s' was decoupled but the registry could "
				      "not be updated: %s", ctx->name,
				      spdk_strerror(-rc));
		free(ctx);
		return;
	}

	SPDK_NOTICELOG("deactivated '%s'\n", ctx->name);
	rpc_lvol_respond_ok(ctx->request, ctx->name);
	free(ctx);
}

static void
rpc_rcow_deactive_bdev(struct spdk_jsonrpc_request *request,
			 const struct spdk_json_val *params)
{
	struct rpc_deactive_bdev req = {};
	const struct s3lvol_active_entry *entry;
	struct deactive_ctx *ctx;
	char nqn[SPDK_NVMF_NQN_MAX_LEN + 1];
	uint32_t nsid;
	int rc;

	if (spdk_json_decode_object(params, rpc_deactive_bdev_decoders,
				    SPDK_COUNTOF(rpc_deactive_bdev_decoders),
				    &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	if (s3lvol_active_load() != 0) {
		rpc_lvol_respond_err(request, 0,
				     "the active registry could not be read");
		goto cleanup;
	}

	entry = s3lvol_active_find(req.device_name);
	if (!entry) {
		/* Not active is the desired end state, so this succeeds. An error here
		 * would force every teardown script to tell "was not active" apart
		 * from "could not be deactivated". */
		SPDK_NOTICELOG("'%s' is not active; nothing to do\n", req.device_name);
		rpc_lvol_respond_ok(request, req.device_name);
		goto cleanup;
	}

	s3lvol_nvmf_subsys_nqn(entry->subsys, nqn, sizeof(nqn));
	nsid = entry->nsid;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}
	ctx->request = request;
	snprintf(ctx->name, sizeof(ctx->name), "%s", req.device_name);

	rc = s3lvol_nvmf_remove_ns(nqn, nsid, rpc_deactive_detached, ctx);
	if (rc != 0) {
		rpc_lvol_respond_errf(request,
				      "could not start the detach of '%s': %s",
				      req.device_name, spdk_strerror(-rc));
		free(ctx);
	}

cleanup:
	free(req.device_name);
}
SPDK_RPC_REGISTER("rcow_deactive_bdev", rpc_rcow_deactive_bdev,
		  SPDK_RPC_RUNTIME)

/* -------------------------------------------------------------------------- */

/* The device path is neither stored nor predicted -- it is looked up by matching
 * the lvol uuid against the namespace uuid the host publishes in sysfs (see the
 * header of vbdev_s3lvol_active.c). That lookup only succeeds once the host has
 * processed the AEN add_ns sent it, which is why rcow_active_bdev cannot answer
 * with a path (see rpc_active_bdev_attached) and why this RPC used to hand back
 * an empty device_path for the first few milliseconds after an activation.
 *
 * Measured, calling both RPCs over one persistent socket: 8-24 ms between the
 * rcow_active_bdev reply and a resolvable path, and 4 out of 5 activations saw
 * at least one empty answer. So every caller had to write a retry loop, and
 * each of them had to know a second thing that is easy to miss: a resolvable
 * path means *sysfs* has the namespace, while the /dev node is created
 * afterwards by udev. rcow_verify_active in rcow_common.sh records a dd that
 * hit ENOENT on a path it had just been told was present.
 *
 * The wait therefore happens here, once, and it waits for the /dev node rather
 * than for sysfs: a non-empty device_path from this RPC is a path that can be
 * opened.
 *
 * === Why a thread and not a poller ===
 *
 * One resolve is an opendir("/sys/block") plus an fopen per nvme device, and a
 * host connected to 32 subsystems has a good many. Retrying that every 20 ms
 * for up to five seconds on the reactor would spend the reactor's time on
 * blocking filesystem I/O. The retry loop runs on a spawner thread instead,
 * which is also kept off the reactor's core (see s3_spawner.h).
 *
 * What that thread may touch is deliberately narrow: sysfs and /dev, nothing
 * else. The active registry is an unlocked global list, so everything that
 * reads it -- the load, the lookup, the iteration -- happens on the RPC thread
 * first, and the fields the wait needs are copied out. The reply is written
 * back on the RPC thread too. jsonrpc does allow a response from any thread
 * ("Might be called from any thread", jsonrpc_internal.h), but the buffer
 * helpers above make no such promise, and there is nothing to gain by
 * finding out. */

#define GET_BDEV_WAIT_MS_DEFAULT 5000
#define GET_BDEV_POLL_US         (20 * 1000)

/* A path is "/dev/nvme31n64" and change; PATH_MAX here would make the snapshot
 * of a full registry (32 x 64 namespaces) eight megabytes. */
#define GET_BDEV_PATH_MAX        64

/* Wait threads in flight. Read and written only on the thread that serves the
 * RPCs, so no lock is needed. The cap exists because a caller is free to ask
 * about many volumes at once: without it, one thread per call. Past the cap the
 * answer is the unwaited one, which is exactly what this RPC always gave. */
#define GET_BDEV_MAX_WAITERS     16
static uint32_t g_get_bdev_waiters;

struct rpc_get_bdev {
	char     *device_name;
	uint32_t  wait_ms;
};

static const struct spdk_json_object_decoder rpc_get_bdev_decoders[] = {
	/* Absent means "all of them", which is what a recovery check wants. */
	{"device_name", offsetof(struct rpc_get_bdev, device_name), spdk_json_decode_string, true},
	/* 0 asks for the historical behaviour: answer with whatever resolves
	 * right now, empty path included. */
	{"wait_ms", offsetof(struct rpc_get_bdev, wait_ms), spdk_json_decode_uint32, true},
};

/* One registry entry, copied off the unlocked global list so the wait thread
 * can work on it. */
struct get_bdev_entry {
	char     name[SPDK_LVOL_NAME_MAX];
	char     uuid[SPDK_UUID_STRING_LEN];
	uint32_t subsys;
	uint32_t nsid;
	/* Empty until resolved. */
	char     path[GET_BDEV_PATH_MAX];
};

struct get_bdev_ctx {
	struct spdk_jsonrpc_request *request;
	/* Where the reply has to be written; the wait runs elsewhere. */
	struct spdk_thread          *thread;
	struct get_bdev_entry       *entries;
	size_t                       count;
	/* A named query answers with the object, an unnamed one with an array. */
	bool                         single;
	uint32_t                     wait_ms;
};

/* Resolve one entry, reporting success only once the /dev node is there.
 *
 * sysfs carries the namespace first and udev creates the node afterwards, so a
 * path taken straight from sysfs can still fail to open -- which is the whole
 * reason this RPC does the waiting rather than its callers.
 *
 * Touches sysfs and /dev only, no shared state: safe on the wait thread. */
static bool
get_bdev_resolve(struct get_bdev_entry *e)
{
	char dev[GET_BDEV_PATH_MAX];

	if (e->path[0] != '\0') {
		return true;
	}
	if (s3lvol_nvmf_resolve_device(e->uuid, dev, sizeof(dev)) != 0) {
		return false;
	}
	if (access(dev, F_OK) != 0) {
		return false;
	}

	snprintf(e->path, sizeof(e->path), "%s", dev);

	/* Logged only on the first resolution, not on every poll: the wait loop
	 * calls this every GET_BDEV_POLL_US, and a line per pass for up to five
	 * seconds would bury whatever else the node is saying. This is the one
	 * line that matters -- where an active lvol landed, which is what a
	 * caller polling for the path is waiting to learn. */
	SPDK_NOTICELOG("rcow_get_bdev: lvol '%s' resolved to %s (subsys %u, nsid %u)\n",
		       e->name, e->path, e->subsys, e->nsid);
	return true;
}

/* How many are still unresolved. Zero means the answer is complete. */
static size_t
get_bdev_resolve_all(struct get_bdev_ctx *ctx)
{
	size_t pending = 0, i;

	for (i = 0; i < ctx->count; i++) {
		if (!get_bdev_resolve(&ctx->entries[i])) {
			pending++;
		}
	}
	return pending;
}

static void
get_bdev_write_one(struct spdk_json_write_ctx *w, const struct get_bdev_entry *e)
{
	char nqn[SPDK_NVMF_NQN_MAX_LEN + 1];
	const char *leaf;
	uint32_t ra_kb;

	s3lvol_nvmf_subsys_nqn(e->subsys, nqn, sizeof(nqn));

	spdk_json_write_object_begin(w);
	spdk_json_write_named_string(w, "device_name", e->name);
	spdk_json_write_named_string(w, "uuid", e->uuid);
	spdk_json_write_named_string(w, "nqn", nqn);
	spdk_json_write_named_uint32(w, "subsys", e->subsys);
	spdk_json_write_named_uint32(w, "nsid", e->nsid);

	/* Empty rather than absent when the wait ran out: the field is always
	 * there so a caller can test it without special-casing, and an empty
	 * string cannot be mistaken for a device. */
	spdk_json_write_named_string(w, "device_path", e->path);

	if (e->path[0] != '\0') {
		/* Tuning the device here, in what is otherwise a query, is
		 * deliberate: this is the one point every consumer necessarily
		 * passes through, since asking here is the only way to learn the
		 * path at all. rcow_active_bdev cannot do it -- it answers before
		 * the host has even noticed the namespace. The startup script
		 * tunes devices too (rcow_tune_readahead), which covers a replay
		 * where nobody asks; the two agree on the value and each is
		 * idempotent, so whichever runs first is fine.
		 *
		 * Cheap on the repeat calls a caller may still make:
		 * set_readahead reads the current value and returns without
		 * writing when it already matches, and never overwrites a value
		 * somebody set deliberately. */
		leaf = strrchr(e->path, '/');
		ra_kb = s3lvol_nvmf_readahead_kb();
		if (leaf && leaf[1] != '\0' && ra_kb > 0) {
			s3lvol_nvmf_set_readahead(leaf + 1, ra_kb);
		}
	}

	spdk_json_write_object_end(w);
}

/* Answer and dispose of the context. RPC thread only. */
static void
get_bdev_respond(struct get_bdev_ctx *ctx)
{
	struct rpc_json_buf buf;
	struct spdk_json_write_ctx *w;
	size_t i;

	w = rpc_json_buf_begin(&buf);
	if (!w) {
		rpc_lvol_respond_err(ctx->request, -ENOMEM, NULL);
		goto out;
	}

	if (ctx->single) {
		get_bdev_write_one(w, &ctx->entries[0]);
	} else {
		spdk_json_write_array_begin(w);
		for (i = 0; i < ctx->count; i++) {
			get_bdev_write_one(w, &ctx->entries[i]);
		}
		spdk_json_write_array_end(w);
	}
	rpc_json_buf_respond(ctx->request, w, &buf);

out:
	free(ctx->entries);
	free(ctx);
}

/* Back on the RPC thread, whether the wait found everything or gave up. */
static void
get_bdev_waited(void *arg)
{
	struct get_bdev_ctx *ctx = arg;

	assert(g_get_bdev_waiters > 0);
	g_get_bdev_waiters--;
	get_bdev_respond(ctx);
}

/* Hand the context back for the reply. A thread that has already gone is the
 * only way this fails, i.e. the process is on its way down; leaking the
 * context then is preferable to writing a response from here. */
static void
get_bdev_hand_back(struct get_bdev_ctx *ctx)
{
	if (spdk_thread_send_msg(ctx->thread, get_bdev_waited, ctx) != 0) {
		SPDK_ERRLOG("could not return the rcow_get_bdev reply to its "
			    "thread; the request is left unanswered\n");
	}
}

static void *
get_bdev_wait_thread(void *arg)
{
	struct get_bdev_ctx *ctx = arg;
	uint32_t waited_ms = 0;

	/* Nobody joins this thread -- see s3_spawner_pthread_create_async. */
	pthread_detach(pthread_self());

	while (get_bdev_resolve_all(ctx) > 0 && waited_ms < ctx->wait_ms) {
		usleep(GET_BDEV_POLL_US);
		waited_ms += GET_BDEV_POLL_US / 1000;
	}

	get_bdev_hand_back(ctx);
	return NULL;
}

/* The thread could not be created. Answer with what resolved already rather
 * than failing the query: an unwaited answer is still a valid one. Runs on the
 * spawner, so it goes back through the same hand-off. */
static void
get_bdev_spawn_failed(void *cb_ctx, int err)
{
	struct get_bdev_ctx *ctx = cb_ctx;

	SPDK_WARNLOG("could not spawn the rcow_get_bdev wait thread (%s); "
		     "answering without waiting\n", spdk_strerror(err));
	get_bdev_hand_back(ctx);
}

/* Where the active volumes landed on the host.
 *
 * Named: one object. Unnamed: an array of them. Either way the document travels
 * as a JSON string in string_value, so a caller parses the reply twice -- see the
 * note on rpc_json_buf. */
static void
rpc_rcow_get_bdev(struct spdk_jsonrpc_request *request,
		  const struct spdk_json_val *params)
{
	struct rpc_get_bdev req = { .wait_ms = GET_BDEV_WAIT_MS_DEFAULT };
	struct get_bdev_ctx *ctx = NULL;
	const struct s3lvol_active_entry *e;
	size_t n = 0, i;

	if (params && spdk_json_decode_object(params, rpc_get_bdev_decoders,
					      SPDK_COUNTOF(rpc_get_bdev_decoders),
					      &req)) {
		rpc_lvol_respond_err(request, 0, "Invalid parameters");
		goto cleanup;
	}

	if (s3lvol_active_load() != 0) {
		rpc_lvol_respond_err(request, 0,
				     "the active registry could not be read");
		goto cleanup;
	}

	/* A name that is not in the registry is an error, not an empty answer:
	 * the caller is asking where a volume is, and "nowhere" means it was
	 * never activated. Checked before anything is allocated. */
	if (req.device_name) {
		if (!s3lvol_active_find(req.device_name)) {
			rpc_lvol_respond_errf(request, "'%s' is not active",
					      req.device_name);
			goto cleanup;
		}
		n = 1;
	} else {
		for (e = s3lvol_active_first(); e != NULL;
		     e = s3lvol_active_next(e)) {
			n++;
		}
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		rpc_lvol_respond_err(request, -ENOMEM, NULL);
		goto cleanup;
	}
	if (n > 0) {
		ctx->entries = calloc(n, sizeof(*ctx->entries));
		if (!ctx->entries) {
			free(ctx);
			ctx = NULL;
			rpc_lvol_respond_err(request, -ENOMEM, NULL);
			goto cleanup;
		}
	}
	ctx->request = request;
	ctx->thread  = spdk_get_thread();
	ctx->single  = (req.device_name != NULL);
	ctx->wait_ms = req.wait_ms;

	/* The snapshot. Taken here, on the thread that owns the registry, and
	 * by value: the entries are links in an unlocked global list that
	 * another RPC may add to or remove from while the wait runs. */
	if (req.device_name) {
		e = s3lvol_active_find(req.device_name);
		memcpy(ctx->entries[0].name, e->name, sizeof(ctx->entries[0].name));
		memcpy(ctx->entries[0].uuid, e->uuid, sizeof(ctx->entries[0].uuid));
		ctx->entries[0].subsys = e->subsys;
		ctx->entries[0].nsid   = e->nsid;
		ctx->count = 1;
	} else {
		i = 0;
		for (e = s3lvol_active_first(); e != NULL && i < n;
		     e = s3lvol_active_next(e), i++) {
			memcpy(ctx->entries[i].name, e->name,
			       sizeof(ctx->entries[i].name));
			memcpy(ctx->entries[i].uuid, e->uuid,
			       sizeof(ctx->entries[i].uuid));
			ctx->entries[i].subsys = e->subsys;
			ctx->entries[i].nsid   = e->nsid;
		}
		ctx->count = i;
	}

	/* The common case by a wide margin: everything is already up, so answer
	 * without a thread. This is also what a caller asking wait_ms=0 gets,
	 * and what happens once the cap on concurrent waits is reached. */
	if (get_bdev_resolve_all(ctx) == 0 || ctx->wait_ms == 0 ||
	    g_get_bdev_waiters >= GET_BDEV_MAX_WAITERS) {
		get_bdev_respond(ctx);
		ctx = NULL;
		goto cleanup;
	}

	/* Counted before the thread exists, so the cap holds even if several
	 * requests arrive back to back; get_bdev_waited drops it again on
	 * either path out. */
	g_get_bdev_waiters++;
	if (s3_spawner_pthread_create_async(get_bdev_wait_thread, ctx,
					    get_bdev_spawn_failed, ctx) != 0) {
		/* Not even submitted, so get_bdev_spawn_failed will not run. */
		g_get_bdev_waiters--;
		get_bdev_respond(ctx);
	}
	ctx = NULL;

cleanup:
	free(req.device_name);
}
SPDK_RPC_REGISTER("rcow_get_bdev", rpc_rcow_get_bdev, SPDK_RPC_RUNTIME)
