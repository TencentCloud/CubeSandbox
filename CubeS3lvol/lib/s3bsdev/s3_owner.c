/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#include "spdk/stdinc.h"
#include "spdk/json.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/util.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_owner.h"

/* Same layout rule as the data objects (<prefix>/data/<uuid>): the prefix is the
 * lvstore name. */
#define OWNER_KEY_FMT   "%s/meta/owner"

/* Enough for "<lvs_name>/meta/owner". The lvstore name is bounded by
 * S3_LVS_NAME_MAX (64) but this file must not depend on s3_local_dev.h, so give
 * it room for any sane name and check at runtime. */
#define OWNER_KEY_MAX   256

struct owner_ctx {
	struct s3_client       *client;
	char                    key[OWNER_KEY_MAX];
	bool                    force;

	/* What we intend to write. */
	struct s3_owner_info    self;

	/* Read buffer, reused for both the "who is there" read and the read-back.
	 * Its lifetime has to cover the whole s3_get_range, hence living here. */
	char                   *buf;

	/* The document we PUT. Kept alive for the same reason. */
	char                   *doc;
	struct iovec            iov;

	struct s3_owner_info    holder;
	bool                    holder_valid;

	/* Distinguishes the read that looks for an existing owner from the one
	 * that confirms our own write landed. */
	bool                    verifying;

	s3_owner_acquire_cb     acq_cb;
	s3_owner_cb             cb;
	void                   *cb_arg;
};

/* ==========================================================================
 * Encoding / decoding the document
 * ========================================================================== */

/* Sanitise anything that goes into the JSON as a string.
 *
 * The hostname is the only field that is not a number or a uuid, and it comes
 * from the OS rather than from us. A quote or a backslash in it would produce a
 * document that no longer parses -- or worse, one that parses into something
 * else. Restricting it to an obviously safe set is cheaper than escaping, and a
 * mangled hostname in a diagnostic field costs nothing. */
static void
sanitise_ident(const char *in, char *out, size_t out_len)
{
	size_t i;

	if (out_len == 0) {
		return;
	}

	for (i = 0; i + 1 < out_len && in && in[i] != '\0'; i++) {
		unsigned char c = (unsigned char)in[i];

		if (isalnum(c) || c == '.' || c == '-' || c == '_') {
			out[i] = (char)c;
		} else {
			out[i] = '_';
		}
	}
	out[i] = '\0';
}

static int
owner_encode(const struct s3_owner_info *info, char **out_doc)
{
	char uuid_str[SPDK_UUID_STRING_LEN];
	char nonce_str[SPDK_UUID_STRING_LEN];
	char *doc;
	int len;

	spdk_uuid_fmt_lower(uuid_str, sizeof(uuid_str), &info->lvs_uuid);
	spdk_uuid_fmt_lower(nonce_str, sizeof(nonce_str), &info->nonce);

	doc = calloc(1, S3_OWNER_DOC_MAX);
	if (!doc) {
		return -ENOMEM;
	}

	len = snprintf(doc, S3_OWNER_DOC_MAX,
		       "{\"node_id\":\"%s\",\"pid\":%" PRId32 ","
		       "\"attach_ts\":%" PRIu64 ",\"lvs_uuid\":\"%s\","
		       "\"nonce\":\"%s\"}",
		       info->node_id, info->pid, info->attach_ts,
		       uuid_str, nonce_str);
	if (len < 0 || len >= S3_OWNER_DOC_MAX) {
		free(doc);
		return -ENAMETOOLONG;
	}

	*out_doc = doc;
	return len;
}

struct owner_decoded {
	char *node_id;
	uint32_t pid;
	uint64_t attach_ts;
	char *lvs_uuid;
	char *nonce;
};

static const struct spdk_json_object_decoder owner_decoders[] = {
	{"node_id",   offsetof(struct owner_decoded, node_id),   spdk_json_decode_string, true},
	{"pid",       offsetof(struct owner_decoded, pid),       spdk_json_decode_uint32, true},
	{"attach_ts", offsetof(struct owner_decoded, attach_ts), spdk_json_decode_uint64, true},
	{"lvs_uuid",  offsetof(struct owner_decoded, lvs_uuid),  spdk_json_decode_string, true},
	{"nonce",     offsetof(struct owner_decoded, nonce),     spdk_json_decode_string, true},
};

/* Parse a document read back from S3.
 *
 * Every field is optional and a parse failure is not fatal to the caller: the
 * point of reading is to find out *whether* somebody is there, and a marker we
 * cannot read still means somebody wrote one. Returning -EINVAL here would turn
 * "an older or corrupt marker" into "attach is impossible", which is worse than
 * reporting an unknown holder. */
static int
owner_decode(const char *buf, size_t len, struct s3_owner_info *out)
{
	struct owner_decoded dec = {0};
	struct spdk_json_val *values = NULL;
	char *copy;
	ssize_t nvals;
	int rc = -EINVAL;

	memset(out, 0, sizeof(*out));

	/* spdk_json_parse rewrites the buffer in place (it unescapes strings), so
	 * it must not see the caller's. */
	copy = malloc(len + 1);
	if (!copy) {
		return -ENOMEM;
	}
	memcpy(copy, buf, len);
	copy[len] = '\0';

	nvals = spdk_json_parse(copy, len, NULL, 0, NULL,
				SPDK_JSON_PARSE_FLAG_DECODE_IN_PLACE);
	if (nvals <= 0) {
		goto out;
	}

	values = calloc((size_t)nvals, sizeof(*values));
	if (!values) {
		rc = -ENOMEM;
		goto out;
	}

	nvals = spdk_json_parse(copy, len, values, (size_t)nvals, NULL,
				SPDK_JSON_PARSE_FLAG_DECODE_IN_PLACE);
	if (nvals <= 0) {
		goto out;
	}

	if (spdk_json_decode_object(values, owner_decoders,
				    SPDK_COUNTOF(owner_decoders), &dec) != 0) {
		goto out;
	}

	if (dec.node_id) {
		snprintf(out->node_id, sizeof(out->node_id), "%s", dec.node_id);
	}
	out->pid = (int32_t)dec.pid;
	out->attach_ts = dec.attach_ts;
	if (dec.lvs_uuid) {
		spdk_uuid_parse(&out->lvs_uuid, dec.lvs_uuid);
	}
	if (dec.nonce) {
		spdk_uuid_parse(&out->nonce, dec.nonce);
	}
	rc = 0;

out:
	free(dec.node_id);
	free(dec.lvs_uuid);
	free(dec.nonce);
	free(values);
	free(copy);
	return rc;
}

void
s3_owner_info_str(const struct s3_owner_info *info, char *out, size_t out_len)
{
	char uuid_str[SPDK_UUID_STRING_LEN];

	if (!out || out_len == 0) {
		return;
	}
	if (!info) {
		snprintf(out, out_len, "<unknown holder>");
		return;
	}

	spdk_uuid_fmt_lower(uuid_str, sizeof(uuid_str), &info->lvs_uuid);

	snprintf(out, out_len,
		 "node=%s pid=%" PRId32 " since=%" PRIu64 " (unix) lvs_uuid=%s",
		 info->node_id[0] ? info->node_id : "?", info->pid,
		 info->attach_ts, uuid_str);
}

/* ==========================================================================
 * acquire
 * ========================================================================== */

static void owner_do_put(struct owner_ctx *ctx);
static void owner_do_read(struct owner_ctx *ctx, bool verifying);

static void
owner_acquire_finish(struct owner_ctx *ctx, int status)
{
	s3_owner_acquire_cb cb_fn = ctx->acq_cb;
	void *cb_arg = ctx->cb_arg;
	struct s3_owner_info holder = ctx->holder;
	bool holder_valid = ctx->holder_valid;

	free(ctx->buf);
	free(ctx->doc);
	free(ctx);

	if (cb_fn) {
		cb_fn(cb_arg, (status == -EBUSY && holder_valid) ? &holder : NULL,
		      status);
	}
}

static void
owner_read_done(void *cb_arg, uint64_t bytes_read, int status)
{
	struct owner_ctx *ctx = cb_arg;
	struct s3_owner_info got;
	char desc[256];

	if (status == -ENOENT) {
		if (ctx->verifying) {
			/* Our own marker vanished between writing and reading it
			 * back. Somebody is actively managing this lvstore, which
			 * is exactly what must not happen. */
			SPDK_ERRLOG("owner marker for '%s' disappeared right after "
				    "it was written; another process is touching "
				    "this lvstore\n", ctx->key);
			owner_acquire_finish(ctx, -EBUSY);
			return;
		}
		/* Nobody there. */
		owner_do_put(ctx);
		return;
	}

	if (status != 0) {
		SPDK_ERRLOG("Failed to read the owner marker '%s': %d\n",
			    ctx->key, status);
		owner_acquire_finish(ctx, status);
		return;
	}

	if (bytes_read == 0 || bytes_read > S3_OWNER_DOC_MAX) {
		SPDK_ERRLOG("Owner marker '%s' has an implausible size %" PRIu64
			    "\n", ctx->key, bytes_read);
		owner_acquire_finish(ctx, -EIO);
		return;
	}

	if (owner_decode(ctx->buf, bytes_read, &got) != 0) {
		/* Unreadable, but present. Treated as "occupied by someone
		 * unknown" rather than as a hard error -- see owner_decode. */
		if (ctx->verifying) {
			SPDK_ERRLOG("Could not parse the owner marker '%s' we just "
				    "wrote; treating it as contended\n", ctx->key);
			owner_acquire_finish(ctx, -EBUSY);
			return;
		}
		SPDK_WARNLOG("Owner marker '%s' exists but could not be parsed; "
			     "treating it as held by an unknown owner\n", ctx->key);
		memset(&ctx->holder, 0, sizeof(ctx->holder));
		ctx->holder_valid = false;
		if (ctx->force) {
			owner_do_put(ctx);
			return;
		}
		owner_acquire_finish(ctx, -EBUSY);
		return;
	}

	if (ctx->verifying) {
		/* The whole point of the read-back: confirm the marker still says
		 * what we wrote. If a concurrent acquire overwrote it, its nonce
		 * is there instead of ours and we must not proceed.
		 *
		 * This narrows the race, it does not remove it -- see the header. */
		if (spdk_uuid_compare(&got.nonce, &ctx->self.nonce) != 0) {
			s3_owner_info_str(&got, desc, sizeof(desc));
			SPDK_ERRLOG("Lost the race for '%s': the marker now belongs "
				    "to %s\n", ctx->key, desc);
			ctx->holder = got;
			ctx->holder_valid = true;
			owner_acquire_finish(ctx, -EBUSY);
			return;
		}
		owner_acquire_finish(ctx, 0);
		return;
	}

	/* Somebody holds it. */
	ctx->holder = got;
	ctx->holder_valid = true;
	s3_owner_info_str(&got, desc, sizeof(desc));

	if (!ctx->force) {
		SPDK_ERRLOG("'%s' is already held by %s -- refusing to attach. "
			    "If that owner is gone (a crash, say), confirm it and "
			    "retry with force=true.\n", ctx->key, desc);
		owner_acquire_finish(ctx, -EBUSY);
		return;
	}

	/* Overwriting somebody else's marker leaves a trace on purpose. */
	SPDK_WARNLOG("force=true: overwriting the owner marker '%s', which was "
		     "held by %s. If that owner is still running, both processes "
		     "are now writing to the same objects.\n", ctx->key, desc);
	owner_do_put(ctx);
}

static void
owner_do_read(struct owner_ctx *ctx, bool verifying)
{
	int rc;

	ctx->verifying = verifying;

	/* A fixed-length range read rather than HEAD-then-GET.
	 *
	 * S3 truncates a range whose end is past the object instead of failing,
	 * so one round trip gets both "does it exist" (-ENOENT) and the content.
	 * The HEAD was only ever needed to learn the size. */
	rc = s3_get_range(ctx->client, ctx->key, 0, S3_OWNER_DOC_MAX, ctx->buf,
			  owner_read_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit the owner marker read for '%s': %d\n",
			    ctx->key, rc);
		owner_acquire_finish(ctx, rc);
	}
}

static void
owner_put_done(void *cb_arg, int status)
{
	struct owner_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to write the owner marker '%s': %d\n",
			    ctx->key, status);
		owner_acquire_finish(ctx, status);
		return;
	}

	/* Read it back. Without this, two processes that both saw "nobody there"
	 * would both carry on. */
	owner_do_read(ctx, true);
}

static void
owner_do_put(struct owner_ctx *ctx)
{
	int len;
	int rc;

	/* if_none_match is deliberately false. COS ignores it and returns 0
	 * regardless, so passing true would look like an atomic create-if-absent
	 * while providing none of it -- the read-back below is what this relies
	 * on instead. */
	len = owner_encode(&ctx->self, &ctx->doc);
	if (len < 0) {
		owner_acquire_finish(ctx, len);
		return;
	}

	ctx->iov.iov_base = ctx->doc;
	ctx->iov.iov_len = (size_t)len;

	rc = s3_put(ctx->client, ctx->key, &ctx->iov, 1, false,
		    owner_put_done, ctx);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit the owner marker write for '%s': "
			    "%d\n", ctx->key, rc);
		owner_acquire_finish(ctx, rc);
	}
}

void
s3_owner_acquire(struct s3_client *client, const char *lvs_name,
		 const struct spdk_uuid *lvs_uuid, bool force,
		 s3_owner_acquire_cb cb_fn, void *cb_arg)
{
	struct owner_ctx *ctx;
	char host[S3_OWNER_NODE_ID_MAX] = {0};
	int rc;

	assert(cb_fn != NULL);

	if (!client || !lvs_name || lvs_name[0] == '\0') {
		cb_fn(cb_arg, NULL, -EINVAL);
		return;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}
	ctx->client = client;
	ctx->force  = force;
	ctx->acq_cb = cb_fn;
	ctx->cb_arg = cb_arg;

	rc = snprintf(ctx->key, sizeof(ctx->key), OWNER_KEY_FMT, lvs_name);
	if (rc < 0 || (size_t)rc >= sizeof(ctx->key)) {
		free(ctx);
		cb_fn(cb_arg, NULL, -ENAMETOOLONG);
		return;
	}

	if (gethostname(host, sizeof(host) - 1) != 0) {
		snprintf(host, sizeof(host), "unknown");
	}
	sanitise_ident(host, ctx->self.node_id, sizeof(ctx->self.node_id));
	ctx->self.pid = (int32_t)getpid();
	ctx->self.attach_ts = (uint64_t)time(NULL);
	if (lvs_uuid) {
		spdk_uuid_copy(&ctx->self.lvs_uuid, lvs_uuid);
	}
	spdk_uuid_generate(&ctx->self.nonce);

	ctx->buf = calloc(1, S3_OWNER_DOC_MAX);
	if (!ctx->buf) {
		free(ctx);
		cb_fn(cb_arg, NULL, -ENOMEM);
		return;
	}

	owner_do_read(ctx, false);
}

/* ==========================================================================
 * release
 * ========================================================================== */

static void
owner_delete_done(void *cb_arg, int status)
{
	struct owner_ctx *ctx = cb_arg;
	s3_owner_cb cb_fn = ctx->cb;
	void *user_arg = ctx->cb_arg;

	if (status != 0 && status != -ENOENT) {
		/* Worth a line, not worth failing over: a marker left behind only
		 * means the next attach needs force=true. */
		SPDK_WARNLOG("Failed to remove the owner marker '%s' (%d); the "
			     "next attach will need force=true\n",
			     ctx->key, status);
	}

	free(ctx->buf);
	free(ctx->doc);
	free(ctx);

	if (cb_fn) {
		cb_fn(user_arg, status == -ENOENT ? 0 : status);
	}
}

void
s3_owner_release(struct s3_client *client, const char *lvs_name,
		 s3_owner_cb cb_fn, void *cb_arg)
{
	struct owner_ctx *ctx;
	int rc;

	if (!client || !lvs_name || lvs_name[0] == '\0') {
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

	rc = snprintf(ctx->key, sizeof(ctx->key), OWNER_KEY_FMT, lvs_name);
	if (rc < 0 || (size_t)rc >= sizeof(ctx->key)) {
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, -ENAMETOOLONG);
		}
		return;
	}

	rc = s3_delete(ctx->client, ctx->key, owner_delete_done, ctx);
	if (rc != 0) {
		SPDK_WARNLOG("Failed to submit the owner marker delete for '%s': "
			     "%d\n", ctx->key, rc);
		free(ctx);
		if (cb_fn) {
			cb_fn(cb_arg, rc);
		}
	}
}
