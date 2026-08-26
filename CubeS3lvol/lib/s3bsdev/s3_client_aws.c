/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   S3 client implementation based on aws-c-s3 (AWS CRT).
 *
 *   This is the only translation unit allowed to include <aws/...> headers.
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"

#include <aws/s3/s3.h>
#include <aws/s3/s3_client.h>
#include <aws/auth/signing_config.h>
#include <aws/auth/credentials.h>
#include <aws/http/connection.h>
#include <aws/http/request_response.h>
#include <aws/io/event_loop.h>
#include <aws/common/atomics.h>
#include <aws/common/error.h>
#include <aws/io/channel_bootstrap.h>
#include <aws/io/stream.h>
#include <aws/io/tls_channel_handler.h>
#include <aws/io/tls_channel_handler.h>

#include "s3lvol/s3_client.h"
#include "s3lvol/s3_spawner.h"

/* ==========================================================================
 * Internal structures
 * ========================================================================== */

/* Per-endpoint client pool entry.
 * Multiple lvstores targeting the same endpoint share one aws_s3_client.
 * Protected by refcnt: last s3_client_put() triggers destroy. */
#define S3_MAX_ENDPOINTS        128

/* Upper bound on keys per s3_delete_batch() call. Kept at the historical
 * DeleteObjects limit even though the batch is now a fan-out of single-key
 * DELETEs (see DELETE BATCH below): it caps how many concurrent requests one
 * call can inject into the CRT connection pool. */
#define S3_MAX_KEYS_PER_DELETE  1000

#define S3_MAX_ENDPOINT_LEN     256
#define S3_MAX_REGION_LEN       64
#define S3_MAX_BUCKET_LEN       128
#define S3_MAX_KEY_LEN          1024

/* Global CRT resources shared across all endpoints.
 * Created once at s3_crt_global_init(), torn down at s3_crt_global_fini(). */
struct app_context {
	struct aws_allocator             *allocator;
	struct aws_event_loop_group      *event_loop_group;
	struct aws_host_resolver         *resolver;
	struct aws_client_bootstrap      *bootstrap;
	struct aws_tls_ctx               *tls_ctx;
	struct aws_tls_connection_options tls_connection_options;
	struct aws_logger                 logger;
};

struct s3_client {
	struct app_context              *app_ctx;
	struct aws_s3_client            *aws_client;
	struct aws_signing_config_aws    signing_config;

	/* Referenced by signing_config. aws_s3_init_default_signing_config()
	 * only stores the pointer — it does not take ownership — so we must
	 * release it ourselves once aws_client is gone. */
	struct aws_credentials_provider *creds_provider;

	char                             endpoint[S3_MAX_ENDPOINT_LEN];
	char                             region[S3_MAX_REGION_LEN];
	char                             bucket[S3_MAX_BUCKET_LEN];
	bool                             use_path_style;
	bool                             verify_tls;

	uint32_t                         refcnt;

	/* Counters are bumped from CRT IO threads, several concurrently (a
	 * 1000-key s3_delete_batch() fan-out completes on many threads at
	 * once), and ->inflight is additionally bumped by the submitting
	 * thread. Plain ++ loses counts, so keep them atomic and assemble a
	 * snapshot in s3_client_get_stats(). */
	struct {
		struct aws_atomic_var    get_ops;
		struct aws_atomic_var    put_ops;
		struct aws_atomic_var    head_ops;
		struct aws_atomic_var    delete_ops;
		struct aws_atomic_var    copy_ops;
		struct aws_atomic_var    bytes_read;
		struct aws_atomic_var    bytes_written;
		struct aws_atomic_var    errors_4xx;
		struct aws_atomic_var    errors_5xx;
		struct aws_atomic_var    retries;
		struct aws_atomic_var    inflight;
	} stats;
};

/* Shorthand for the counter updates below. */
#define S3_STAT_INC(client, field)         \
	aws_atomic_fetch_add(&(client)->stats.field, 1)
#define S3_STAT_DEC(client, field)         \
	aws_atomic_fetch_sub(&(client)->stats.field, 1)
#define S3_STAT_ADD(client, field, n)      \
	aws_atomic_fetch_add(&(client)->stats.field, (size_t)(n))
#define S3_STAT_GET(client, field)         \
	aws_atomic_load_int(&(client)->stats.field)

/* Per-request context. Allocated at submit, freed in the finish callback.
 *
 * IMPORTANT: nothing here may point into caller-owned memory that could be
 * freed before the callback runs. Payloads are always copied into ->buf. */
struct s3_delete_batch_ctx;

struct s3_request {
	struct s3_client               *client;

	/* Captured at submit rather than reached through ->client at completion.
	 *
	 * A request can outlive its client. s3_client_put() frees the client as soon
	 * as its refcount hits zero, but aws_s3_client_release() is asynchronous, and
	 * the completion path has one more hop after CRT is done with it: the user
	 * callback is bounced to->owner_thread via spdk_thread_send_msg(). At
	 * shutdown that message can still be queued when the last lvstore drops the
	 * client, and the reactor then runs it against freed memory.
	 *
	 * Observed as a segfault at process exit, one run in five, dereferencing
	 *->client->app_ctx->allocator in s3_request_complete_on_owner(). The
	 * allocator itself was never the problem -- app_ctx is &g_app_ctx, a static --
	 * so holding it directly removes the only reason completion has to touch the
	 * client at all.
	 *
	 * This does not license outliving the client on purpose: an in-flight request
	 * whose client is gone still delivers a callback to a caller that may itself
	 * have been torn down. See the contract note above s3_request_invoke_cb(). */
	struct aws_allocator            *allocator;

	char                             key_buf[S3_MAX_KEY_LEN];
	struct aws_byte_cursor           key;

	/* GET: destination range within the caller's buffer.
	 * PUT: ->buf is our private copy of the request body, released in the
	 * finish callback. */
	uint64_t                         offset;
	uint64_t                         len;
	void                            *buf;

	/* GET: bytes written into ->buf, counted rather than watermarked -- see
	 * s3_get_body_callback().
	 * PUT: total body size (captured at submit time so the callback never
	 * has to walk the caller's iov). */
	uint64_t                         bytes_read;

	bool                             if_none_match;

	/* HEAD: where to store Content-Length (may be NULL) */
	uint64_t                        *out_size;

	/* COPY: source location, kept for error messages */
	char                             src_bucket[S3_MAX_BUCKET_LEN];
	char                             src_key[S3_MAX_KEY_LEN];

	/* DELETE_BATCH fan-out: the shared batch this single-key DELETE belongs
	 * to (NULL for standalone deletes). See struct s3_delete_batch_ctx. */
	struct s3_delete_batch_ctx      *batch;

	union {
		s3_op_cb   op_cb;
		s3_get_cb  get_cb;
	} cb;
	void                            *cb_arg;

	int                              status;

	/* true for s3_get_range() (callback carries a byte count), false for
	 * every other op. Selects which arm of ->cb to invoke. */
	bool                             is_get;

	/* SPDK thread that submitted the request, captured at submit time.
	 *
	 * CRT completes requests on its own IO threads, but callers touch
	 * blobstore/bdev state that is only safe on the owning SPDK thread, so
	 * the user callback is bounced back here via spdk_thread_send_msg()
	 * . See s3_request_complete().
	 *
	 * NULL when submitted from a non-SPDK thread (unit tests, tools); the
	 * callback then runs inline on the CRT thread. */
	struct spdk_thread              *owner_thread;
};

/* ==========================================================================
 * Global state
 * ========================================================================== */

static struct app_context g_app_ctx;
static bool g_app_ctx_initialized = false;

/* Pool of clients, indexed by endpoint. Protected by: single SPDK thread
 * (all init/IO calls happen on the same reactor thread). */
static struct s3_client *g_clients[S3_MAX_ENDPOINTS];
static uint32_t g_num_clients = 0;

/* Defined next to s3_client_put(), which is where the reasoning about the
 * deferred free lives. Declared here because aws_s3_client_new() needs it. */
static void s3_client_shutdown_complete(void *user_data);

/* ==========================================================================
 * Static HTTP header constants (avoids per-request allocation)
 * ========================================================================== */

static const struct aws_byte_cursor g_host_header_name =
	AWS_BYTE_CUR_INIT_FROM_STRING_LITERAL("Host");
static const struct aws_byte_cursor g_content_length_header_name =
	AWS_BYTE_CUR_INIT_FROM_STRING_LITERAL("content-length");
static const struct aws_byte_cursor g_slash_cur =
	AWS_BYTE_CUR_INIT_FROM_STRING_LITERAL("/");

/* ==========================================================================
 * Error mapping: HTTP status / CRT error → errno 
 *
 * HTTP status takes priority when available (it is far more informative than
 * the CRT error code, which collapses most 4xx/5xx into
 * AWS_ERROR_S3_INVALID_RESPONSE_STATUS).
 * ========================================================================== */

static int
http_status_to_errno(int http_status)
{
	switch (http_status) {
	case 200:
	case 204:
	case 206:
		return 0;
	case 304:
		return -EEXIST;    /* Not Modified (If-None-Match hit) */
	case 400:
		return -EINVAL;
	case 401:
	case 403:
		return -EACCES;
	case 404:
		return -ENOENT;
	case 409:
		return -EBUSY;     /* Conflict / OperationAborted */
	case 412:
		return -EEXIST;    /* Precondition Failed (If-None-Match: *) */
	case 416:
		return -ERANGE;    /* Requested Range Not Satisfiable */
	case 429:
		return -EAGAIN;    /* SlowDown / TooManyRequests */
	case 500:
	case 502:
	case 503:
		return -EIO;
	case 504:
		return -ETIMEDOUT;
	default:
		return -EIO;
	}
}

static int
crt_error_to_errno(int aws_error, int http_status)
{
	if (aws_error == AWS_ERROR_SUCCESS) {
		return 0;
	}

	/* Prefer the HTTP status when the server actually responded */
	if (http_status >= 300) {
		return http_status_to_errno(http_status);
	}

	switch (aws_error) {
	/* Throttling — caller may retry */
	case AWS_ERROR_S3_SLOW_DOWN:
		return -EAGAIN;

	/* Timeouts */
	case AWS_ERROR_S3_REQUEST_TIMEOUT:
	case AWS_IO_TLS_NEGOTIATION_TIMEOUT:
		return -ETIMEDOUT;

	/* Cancellation */
	case AWS_ERROR_S3_CANCELED:
	case AWS_ERROR_S3_PAUSED:
		return -ECANCELED;

	/* Range problems */
	case AWS_ERROR_S3_INVALID_RANGE_HEADER:
	case AWS_ERROR_S3_MULTIRANGE_HEADER_UNSUPPORTED:
		return -ERANGE;

	/* Data integrity */
	case AWS_ERROR_S3_RESPONSE_CHECKSUM_MISMATCH:
	case AWS_ERROR_S3_CHECKSUM_CALCULATION_FAILED:
		return -EBADMSG;

	/* Object changed underneath us — violates create-once (P2) */
	case AWS_ERROR_S3_OBJECT_MODIFIED:
	case AWS_ERROR_S3_FILE_MODIFIED:
		return -ESTALE;

	/* Connectivity */
	case AWS_ERROR_HTTP_CONNECTION_CLOSED:
	case AWS_ERROR_HTTP_SERVER_CLOSED:
		return -ECONNRESET;
	case AWS_IO_DNS_INVALID_NAME:
	case AWS_IO_DNS_NO_ADDRESS_FOR_HOST:
		return -EADDRNOTAVAIL;
	case AWS_IO_DNS_QUERY_FAILED:
		return -EHOSTUNREACH;

	/* TLS */
	case AWS_IO_TLS_ERROR_NEGOTIATION_FAILURE:
	case AWS_IO_TLS_ERROR_ALERT_RECEIVED:
	case AWS_IO_TLS_ERROR_NOT_NEGOTIATED:
		return -EPROTO;
	case AWS_IO_TLS_CERTIFICATE_EXPIRED:
	case AWS_IO_TLS_UNKNOWN_ROOT_CERTIFICATE:
	case AWS_IO_TLS_NO_ROOT_CERTIFICATE_FOUND:
		return -EACCES;

	/* Memory */
	case AWS_ERROR_OOM:
	case AWS_ERROR_S3_EXCEEDS_MEMORY_LIMIT:
	case AWS_ERROR_S3_BUFFER_ALLOCATION_FAILED:
		return -ENOMEM;

	default:
		return -EIO;
	}
}

/* Update per-client error counters based on HTTP status */
static void
s3_stats_record_error(struct s3_client *client, int http_status)
{
	if (http_status >= 400 && http_status < 500) {
		S3_STAT_INC(client, errors_4xx);
	} else if (http_status >= 500) {
		S3_STAT_INC(client, errors_5xx);
	}
}

/* ==========================================================================
 * Completion: bounce the user callback onto the submitting SPDK thread
 * 
 *
 * CRT finishes requests on its own IO threads. Callers, however, act on
 * blobstore / bdev / channel state that is only safe to touch from the SPDK
 * thread that owns it — calling them straight from the CRT thread produces
 * exactly the sort of cross-thread corruption that is near-impossible to
 * diagnose after the fact.
 *
 * So the finish callbacks release everything CRT-side and then hand the
 * request to s3_request_complete(), which either
 *   - forwards it to req->owner_thread via spdk_thread_send_msg(), or
 *   - runs it inline when there is no owner thread (submitted from a plain
 *     pthread: unit tests, CLI tools).
 *
 * Note spdk_thread_send_msg() cannot fail gracefully: every error path inside
 * it calls abort(), including sending to a thread already marked EXITED. So
 * there is no "delivery failed" branch to write here — the caller is instead
 * required to outlive its own in-flight requests. That is the same contract
 * SPDK imposes on bdev IO, and s3_client.h documents it.
 * ========================================================================== */

static void
s3_request_invoke_cb(struct s3_request *req)
{
	if (req->is_get) {
		if (req->cb.get_cb) {
			req->cb.get_cb(req->cb_arg, req->bytes_read, req->status);
		}
	} else {
		if (req->cb.op_cb) {
			req->cb.op_cb(req->cb_arg, req->status);
		}
	}
}

/* Runs on req->owner_thread. */
static void
s3_request_complete_on_owner(void *ctx)
{
	struct s3_request *req = ctx;
	struct aws_allocator *allocator = req->allocator;

	s3_request_invoke_cb(req);
	aws_mem_release(allocator, req);
}

/**
 * Finish a request: deliver the user callback and free the request.
 *
 * Takes ownership of \c req — it must not be touched after this returns.
 * All CRT-side resources (meta request, private buffers) must already be
 * released by the caller, because the request may outlive this call by one
 * message hop.
 */
static void
s3_request_complete(struct s3_request *req)
{
	if (req->owner_thread == NULL) {
		/* Submitted off a SPDK thread; nothing to bounce to. */
		struct aws_allocator *allocator = req->allocator;

		s3_request_invoke_cb(req);
		aws_mem_release(allocator, req);
		return;
	}

	if (req->owner_thread == spdk_get_thread()) {
		/* Already home. Can happen if CRT completes inline on the
		 * submitting thread; skip the pointless round trip. */
		struct aws_allocator *allocator = req->allocator;

		s3_request_invoke_cb(req);
		aws_mem_release(allocator, req);
		return;
	}

	spdk_thread_send_msg(req->owner_thread, s3_request_complete_on_owner, req);
}

/* ==========================================================================
 * Helper: build S3 Host header value
 *
 * NOTE: caller supplies the buffer — a static buffer would race between
 * CRT IO threads and the SPDK reactor.
 * ========================================================================== */

static void
s3_client_hostname(const struct s3_client *client, char *buf, size_t buf_len)
{
	if (client->use_path_style) {
		snprintf(buf, buf_len, "%s", client->endpoint);
	} else {
		/* virtual-hosted: Host = bucket.endpoint */
		snprintf(buf, buf_len, "%s.%s", client->bucket, client->endpoint);
	}
}

/* ==========================================================================
 * Helper: copy a key into the request, refusing truncation.
 *
 * A silently truncated key would target the WRONG object — for a storage
 * backend that means data corruption, not just a failed request. Always fail
 * loudly instead.
 * ========================================================================== */

static int
s3_request_set_key(struct s3_request *req, const char *key)
{
	size_t len = strlen(key);

	if (len == 0) {
		SPDK_ERRLOG("S3 key must not be empty\n");
		return -EINVAL;
	}
	if (len >= sizeof(req->key_buf)) {
		SPDK_ERRLOG("S3 key too long: %zu bytes (max %zu)\n",
			    len, sizeof(req->key_buf) - 1);
		return -ENAMETOOLONG;
	}

	memcpy(req->key_buf, key, len + 1);
	req->key = aws_byte_cursor_from_array(req->key_buf, len);
	return 0;
}

/* ==========================================================================
 * Helper: build the request path
 *
 * virtual-hosted style: /key
 * path-style:           /bucket/key
 * ========================================================================== */

static int
s3_set_request_path(struct aws_http_message *message,
		    struct aws_allocator *allocator,
		    const struct s3_client *client,
		    struct aws_byte_cursor key)
{
	struct aws_byte_buf path_buf;
	int rc;

	if (client->use_path_style) {
		struct aws_byte_cursor bucket_cur = aws_byte_cursor_from_c_str(client->bucket);
		if (aws_byte_buf_init(&path_buf, allocator,
				      1 + bucket_cur.len + 1 + key.len)) {
			return AWS_OP_ERR;
		}
		aws_byte_buf_append_dynamic(&path_buf, &g_slash_cur);
		aws_byte_buf_append_dynamic(&path_buf, &bucket_cur);
	} else {
		if (aws_byte_buf_init(&path_buf, allocator, 1 + key.len)) {
			return AWS_OP_ERR;
		}
	}
	aws_byte_buf_append_dynamic(&path_buf, &g_slash_cur);
	aws_byte_buf_append_dynamic(&path_buf, &key);

	struct aws_byte_cursor path_cur = aws_byte_cursor_from_buf(&path_buf);
	rc = aws_http_message_set_request_path(message, path_cur);
	aws_byte_buf_clean_up(&path_buf);
	return rc;
}

/* ==========================================================================
 * Helper: set common request headers (Host)
 * ========================================================================== */

static void
s3_set_common_headers(struct aws_http_message *message, const struct s3_client *client)
{
	char host_buf[S3_MAX_ENDPOINT_LEN + S3_MAX_BUCKET_LEN + 2];

	s3_client_hostname(client, host_buf, sizeof(host_buf));

	struct aws_http_header host_header = {
		.name  = g_host_header_name,
		.value = aws_byte_cursor_from_c_str(host_buf),
	};
	aws_http_message_add_header(message, host_header);
}

/* ==========================================================================
 * Global CRT lifecycle 
 * ========================================================================== */

/* Real init body. MUST run on the spawner thread — see s3_crt_global_init(). */
static int
s3_crt_global_init_body(uint32_t event_loop_threads)
{
	struct aws_allocator *allocator;

	if (g_app_ctx_initialized) {
		SPDK_WARNLOG("CRT global context already initialized\n");
		return 0;
	}

	if (event_loop_threads == 0) {
		event_loop_threads = 4;
	}

	allocator = aws_default_allocator();
	aws_s3_library_init(allocator);

	/* Logger: errors only by default.
	 *
	 * S3LVOL_CRT_LOG_LEVEL raises it for debugging. TRACE dumps every
	 * request header and the canonical string SigV4 signed, which is the
	 * only practical way to tell "we never sent the header" apart from
	 * "the server ignored it" — worth its keep. */
	enum aws_log_level level = AWS_LOG_LEVEL_ERROR;
	const char *level_env = getenv("S3LVOL_CRT_LOG_LEVEL");

	if (level_env) {
		if (!strcasecmp(level_env, "trace")) {
			level = AWS_LOG_LEVEL_TRACE;
		} else if (!strcasecmp(level_env, "debug")) {
			level = AWS_LOG_LEVEL_DEBUG;
		} else if (!strcasecmp(level_env, "info")) {
			level = AWS_LOG_LEVEL_INFO;
		} else if (!strcasecmp(level_env, "warn")) {
			level = AWS_LOG_LEVEL_WARN;
		} else if (!strcasecmp(level_env, "error")) {
			level = AWS_LOG_LEVEL_ERROR;
		} else if (!strcasecmp(level_env, "none")) {
			level = AWS_LOG_LEVEL_NONE;
		} else {
			SPDK_WARNLOG("Ignoring unknown S3LVOL_CRT_LOG_LEVEL='%s' "
				     "(want trace|debug|info|warn|error|none)\n",
				     level_env);
		}
	}

	struct aws_logger_standard_options logger_options = {
		.file  = stderr,
		.level = level,
	};
	aws_logger_init_standard(&g_app_ctx.logger, allocator, &logger_options);
	aws_logger_set(&g_app_ctx.logger);

	/* Event loop group */
	g_app_ctx.event_loop_group = aws_event_loop_group_new_default(
		allocator, event_loop_threads, NULL);
	if (!g_app_ctx.event_loop_group) {
		SPDK_ERRLOG("Failed to create CRT event loop group\n");
		goto err_elg;
	}

	/* Host resolver */
	struct aws_host_resolver_default_options resolver_options = {
		.el_group   = g_app_ctx.event_loop_group,
		.max_entries = 128,
	};
	g_app_ctx.resolver = aws_host_resolver_new_default(
		allocator, &resolver_options);
	if (!g_app_ctx.resolver) {
		SPDK_ERRLOG("Failed to create CRT host resolver\n");
		goto err_resolver;
	}

	/* Client bootstrap */
	struct aws_client_bootstrap_options bootstrap_options = {
		.event_loop_group = g_app_ctx.event_loop_group,
		.host_resolver    = g_app_ctx.resolver,
	};
	g_app_ctx.bootstrap = aws_client_bootstrap_new(
		allocator, &bootstrap_options);
	if (!g_app_ctx.bootstrap) {
		SPDK_ERRLOG("Failed to create CRT client bootstrap\n");
		goto err_bootstrap;
	}

	/* TLS context (shared). Created once, used by all clients that enable TLS. */
	struct aws_tls_ctx_options tls_ctx_options;
	aws_tls_ctx_options_init_default_client(&tls_ctx_options, allocator);
	g_app_ctx.tls_ctx = aws_tls_client_ctx_new(allocator, &tls_ctx_options);
	aws_tls_ctx_options_clean_up(&tls_ctx_options);
	if (!g_app_ctx.tls_ctx) {
		SPDK_ERRLOG("Failed to create CRT TLS context\n");
		goto err_tls;
	}

	aws_tls_connection_options_init_from_ctx(
		&g_app_ctx.tls_connection_options, g_app_ctx.tls_ctx);

	g_app_ctx.allocator = allocator;
	g_app_ctx_initialized = true;

	SPDK_NOTICELOG("CRT global init done: %u IO threads\n", event_loop_threads);
	return 0;

err_tls:
	aws_client_bootstrap_release(g_app_ctx.bootstrap);
err_bootstrap:
	aws_host_resolver_release(g_app_ctx.resolver);
err_resolver:
	aws_event_loop_group_release(g_app_ctx.event_loop_group);
err_elg:
	aws_logger_set(NULL);
	aws_logger_clean_up(&g_app_ctx.logger);
	aws_s3_library_clean_up();
	memset(&g_app_ctx, 0, sizeof(g_app_ctx));
	return -1;
}

/* Trampoline: s3_spawner_run_task() takes void *(*)(void *). */
struct crt_init_ctx {
	uint32_t event_loop_threads;
	int      rc;
};

static void *
s3_crt_global_init_on_spawner(void *arg)
{
	struct crt_init_ctx *ctx = arg;

	ctx->rc = s3_crt_global_init_body(ctx->event_loop_threads);
	return ctx;
}

int
s3_crt_global_init(uint32_t event_loop_threads)
{
	struct crt_init_ctx ctx = {
		.event_loop_threads = event_loop_threads,
		.rc                 = -1,
	};

	/* Must run on the spawner thread.
	 *
	 * CRT creates threads in three places inside the body — the event loop
	 * group, the host resolver, and the standard logger's background thread
	 * — all via plain pthread_create(). On Linux a child inherits the
	 * creator's CPU affinity, and our caller is a SPDK reactor pinned by
	 * DPDK to a single core. Calling the body directly would therefore pile
	 * every CRT IO thread onto that one core, alongside a busy-polling
	 * reactor already burning 100% of it.
	 *
	 * The spawner runs with the reactor cores excluded, so threads created
	 * on it inherit a sane mask. See include/s3lvol/s3_spawner.h for why we
	 * do not use spdk_call_unaffinitized() instead. */
	if (!s3_spawner_is_started()) {
		SPDK_ERRLOG("CRT init requires the thread spawner; call "
			    "s3_spawner_start() first (module init does this). "
			    "Refusing to init on the calling thread — CRT IO "
			    "threads would inherit the reactor's core binding.\n");
		return -EPERM;
	}

	if (s3_spawner_run_task(s3_crt_global_init_on_spawner, &ctx) == NULL) {
		SPDK_ERRLOG("Failed to dispatch CRT init to the spawner\n");
		return -EIO;
	}

	return ctx.rc;
}

/* Real teardown body. Runs on the spawner when one exists — see
 * s3_crt_global_fini(). */
static void
s3_crt_global_fini_body(void)
{
	if (!g_app_ctx_initialized) {
		return;
	}

	/* Anything still here was never released by its owner. Release it, but do not
	 * free it: s3_client_shutdown_complete() does that, and it will run before
	 * aws_event_loop_group_release() below joins the CRT threads. Freeing here
	 * too would be a double free. */
	for (uint32_t i = 0; i < g_num_clients; i++) {
		if (!g_clients[i]) {
			continue;
		}
		SPDK_WARNLOG("Releasing leaked client for endpoint %s\n",
			     g_clients[i]->endpoint);
		if (g_clients[i]->aws_client) {
			aws_s3_client_release(g_clients[i]->aws_client);
		} else {
			/* No aws_client means no shutdown callback will ever arrive. */
			aws_credentials_provider_release(g_clients[i]->creds_provider);
			free(g_clients[i]);
		}
		g_clients[i] = NULL;
	}
	g_num_clients = 0;

	aws_tls_connection_options_clean_up(&g_app_ctx.tls_connection_options);
	if (g_app_ctx.tls_ctx) {
		aws_tls_ctx_release(g_app_ctx.tls_ctx);
	}
	aws_client_bootstrap_release(g_app_ctx.bootstrap);
	aws_host_resolver_release(g_app_ctx.resolver);
	aws_event_loop_group_release(g_app_ctx.event_loop_group);
	aws_s3_library_clean_up();

	/* Detach before tearing the logger down: aws_logger_clean_up() joins
	 * the background log thread, and anything logging afterwards would
	 * touch freed state. */
	aws_logger_set(NULL);
	aws_logger_clean_up(&g_app_ctx.logger);

	memset(&g_app_ctx, 0, sizeof(g_app_ctx));
	g_app_ctx_initialized = false;

	SPDK_NOTICELOG("CRT global fini done\n");
}

static void *
s3_crt_global_fini_on_spawner(void *arg)
{
	(void)arg;
	s3_crt_global_fini_body();
	/* Non-NULL so callers can distinguish "ran" from "spawner refused". */
	return (void *)(uintptr_t)1;
}

void
s3_crt_global_fini(void)
{
	if (!g_app_ctx_initialized) {
		return;
	}

	/* Tear down on the spawner when one is available.
	 *
	 * aws_event_loop_group_release() and aws_logger_clean_up() join the CRT
	 * threads that s3_crt_global_init() created. Joining them from a reactor
	 * would stall that reactor for the duration; doing it on the spawner —
	 * the thread that created them — keeps teardown symmetric with init and
	 * off the polling cores.
	 *
	 * If the spawner is already gone (shutdown ordering, or a unit test that
	 * never started one), clean up inline: leaking the CRT context, and the
	 * threads with it, would be worse than a briefly blocked caller. */
	if (s3_spawner_is_started()) {
		s3_spawner_run_task(s3_crt_global_fini_on_spawner, NULL);
		return;
	}

	s3_crt_global_fini_body();
}

/* ==========================================================================
 * Client lifecycle: endpoint-based pooling with refcounting 
 * ========================================================================== */

int
s3_client_get_or_create(const struct s3_target *target, struct s3_client **out)
{
	uint32_t i;

	if (!g_app_ctx_initialized) {
		SPDK_ERRLOG("CRT not initialized, call s3_crt_global_init() first\n");
		return -EINVAL;
	}

	if (!target || !target->endpoint || !target->region || !target->bucket) {
		SPDK_ERRLOG("s3_target missing required fields\n");
		return -EINVAL;
	}

	/* Pooled by endpoint *and bucket*: a struct s3_client carries the bucket,
	 * and the bucket is what builds the request host (or the leading path
	 * segment). Sharing one across buckets would silently send every request to
	 * whichever bucket happened to be created first -- harmless until export /
	 * import started reading a second bucket on the same endpoint, at which
	 * point it would read the wrong data rather than fail.
	 *
	 * The aws_s3_client underneath really is per endpoint, so this is one
	 * connection pool per (endpoint, bucket) rather than per endpoint. That is a
	 * few more sockets for a correctness property worth having. */
	for (i = 0; i < g_num_clients; i++) {
		if (g_clients[i] &&
		    strcmp(g_clients[i]->endpoint, target->endpoint) == 0 &&
		    strcmp(g_clients[i]->bucket, target->bucket) == 0 &&
		    g_clients[i]->use_path_style == target->use_path_style) {
			g_clients[i]->refcnt++;
			SPDK_NOTICELOG("Reusing S3 client for %s/%s (refcnt=%u)\n",
				       target->endpoint, target->bucket,
				       g_clients[i]->refcnt);
			*out = g_clients[i];
			return 0;
		}
	}

	if (g_num_clients >= S3_MAX_ENDPOINTS) {
		SPDK_ERRLOG("Maximum number of S3 endpoints (%u) reached\n", S3_MAX_ENDPOINTS);
		return -ENOSPC;
	}

	/* Create new client */
	struct s3_client *client = calloc(1, sizeof(*client));
	if (!client) {
		return -ENOMEM;
	}

	client->app_ctx = &g_app_ctx;
	snprintf(client->endpoint, sizeof(client->endpoint), "%s", target->endpoint);
	snprintf(client->region, sizeof(client->region), "%s", target->region);
	snprintf(client->bucket, sizeof(client->bucket), "%s", target->bucket);
	client->use_path_style = target->use_path_style;
	client->verify_tls = target->verify_tls;
	client->refcnt = 1;
	memset(&client->stats, 0, sizeof(client->stats));
	aws_atomic_init_int(&client->stats.get_ops, 0);
	aws_atomic_init_int(&client->stats.put_ops, 0);
	aws_atomic_init_int(&client->stats.head_ops, 0);
	aws_atomic_init_int(&client->stats.delete_ops, 0);
	aws_atomic_init_int(&client->stats.copy_ops, 0);
	aws_atomic_init_int(&client->stats.bytes_read, 0);
	aws_atomic_init_int(&client->stats.bytes_written, 0);
	aws_atomic_init_int(&client->stats.errors_4xx, 0);
	aws_atomic_init_int(&client->stats.errors_5xx, 0);
	aws_atomic_init_int(&client->stats.retries, 0);
	aws_atomic_init_int(&client->stats.inflight, 0);

	/* Build credentials provider based on auth mode  */
	struct aws_credentials_provider *creds_provider = NULL;

	switch (target->auth_mode) {
	case S3_AUTH_STATIC:
		if (target->access_key && target->secret_key) {
			struct aws_credentials_provider_static_options static_opts = {
				.access_key_id     = aws_byte_cursor_from_c_str(target->access_key),
				.secret_access_key = aws_byte_cursor_from_c_str(target->secret_key),
			};
			if (target->session_token) {
				static_opts.session_token =
					aws_byte_cursor_from_c_str(target->session_token);
			}
			creds_provider = aws_credentials_provider_new_static(
				client->app_ctx->allocator, &static_opts);
		}
		break;

	case S3_AUTH_ENV: {
		struct aws_credentials_provider_environment_options env_opts = {0};
		creds_provider = aws_credentials_provider_new_environment(
			client->app_ctx->allocator, &env_opts);
		break;
	}

	case S3_AUTH_FILE: {
		struct aws_credentials_provider_profile_options profile_opts = {0};
		if (target->profile) {
			profile_opts.profile_name_override =
				aws_byte_cursor_from_c_str(target->profile);
		}
		profile_opts.bootstrap = client->app_ctx->bootstrap;
		profile_opts.tls_ctx = client->app_ctx->tls_ctx;
		creds_provider = aws_credentials_provider_new_profile(
			client->app_ctx->allocator, &profile_opts);
		break;
	}

	case S3_AUTH_IAM:
	case S3_AUTH_STS: {
		/* chain_default walks: env → profile → IMDS/ECS container role.
		 * This covers EC2/EKS IAM roles and also honours AWS_ROLE_ARN +
		 * AWS_WEB_IDENTITY_TOKEN_FILE for STS AssumeRoleWithWebIdentity,
		 * which is how EKS IRSA works in practice. */
		struct aws_credentials_provider_chain_default_options chain_opts = {
			.bootstrap = client->app_ctx->bootstrap,
			.tls_ctx   = client->app_ctx->tls_ctx,
		};
		creds_provider = aws_credentials_provider_new_chain_default(
			client->app_ctx->allocator, &chain_opts);
		if (target->auth_mode == S3_AUTH_STS && target->role_arn) {
			SPDK_WARNLOG("Explicit role_arn=%s ignored; relying on "
				     "chain_default (set AWS_ROLE_ARN + "
				     "AWS_WEB_IDENTITY_TOKEN_FILE instead)\n",
				     target->role_arn);
		}
		break;
	}

	default: {
		SPDK_WARNLOG("Unknown auth mode %d, trying environment\n",
			     (int)target->auth_mode);
		struct aws_credentials_provider_environment_options env_opts = {0};
		creds_provider = aws_credentials_provider_new_environment(
			client->app_ctx->allocator, &env_opts);
		break;
	}
	}

	if (!creds_provider) {
		SPDK_ERRLOG("Failed to create credentials provider for endpoint %s\n",
			    target->endpoint);
		free(client);
		return -EACCES;
	}

	/* Signing config */
	aws_s3_init_default_signing_config(&client->signing_config,
		aws_byte_cursor_from_c_str(client->region), creds_provider);
	client->signing_config.flags.use_double_uri_encode = false;
	/* signing_config only borrows the provider — we stay responsible for
	 * releasing it in s3_client_put(). */
	client->creds_provider = creds_provider;

	/* S3 client config
	 *
	 * === Why a request must never be allowed to hang forever ===
	 *
	 * Every read that misses the overlay is an S3 GET, and that GET is what a
	 * guest's read is waiting on. If the GET never completes, neither does the
	 * bdev_io, and from there everything jams in sequence: the guest read hangs,
	 * rcow_unload_lvstore cannot finish because I/O is still in flight,
	 * and SIGTERM appears to be ignored because spdk_app_stop() is itself
	 * waiting for that I/O. A single silently-dropped connection therefore takes
	 * the whole lvstore down until someone SIGKILLs it.
	 *
	 * That is not hypothetical -- a 4k randrw run stalled exactly this way, and
	 * the kernel messages it produced ("no available path - failing I/O") were
	 * a consequence of the SIGKILL, not of the original fault.
	 *
	 * Two mechanisms below, because they cover different failures.
	 */

	/* 1. Throughput monitoring: abort a request that has effectively stopped
	 *    moving data.
	 *
	 * This is the one that catches a half-open connection -- the peer is gone,
	 * the socket is still open, and the request would otherwise sit there until
	 * TCP keepalive noticed.
	 *
	 * 1 KiB/s over a 10 s window is deliberately far below anything healthy:
	 * the intent is to catch "stopped", not "slow". Short requests cannot be
	 * killed by it either, since it only fires after the window has elapsed.
	 */
	struct aws_http_connection_monitoring_options monitoring = {
		.minimum_throughput_bytes_per_second = 1024,
		.allowable_throughput_failure_interval_seconds = 10,
	};

	/* 2. TCP keepalive, tightened from what this used to be.
	 *
	 * It was 7200 s idle with 9 probes 75 s apart, i.e. a dead peer stayed
	 * undetected for over two hours. For a block device that is indistinguishable
	 * from a hang. 15 s idle, 3 probes, 45 s to give up: a stuck read now fails
	 * in about a minute worst case, and a failed read is something the guest can
	 * retry -- an infinite one is not.
	 */
	struct aws_s3_tcp_keep_alive_options keep_alive = {
		.keep_alive_interval_sec      = 15,
		.keep_alive_timeout_sec       = 45,
		.keep_alive_max_failed_probes = 3,
	};

	struct aws_s3_client_config client_config = {
		.client_bootstrap       = client->app_ctx->bootstrap,
		.tcp_keep_alive_options = &keep_alive,
		.monitoring_options     = &monitoring,
		.region                 = aws_byte_cursor_from_c_str(client->region),
		.signing_config         = &client->signing_config,
		.memory_limit_in_bytes  = 4ULL * 1024 * 1024 * 1024,
		.throughput_target_gbps = 30.0,
	};

	/* part_size is left at CRT's default (8 MiB). Worth knowing that this is what
	 * decides whether a GET is split at all: a read shorter than it arrives as one
	 * part, and s3_get_body_callback() then cannot tell a truncated body from a
	 * complete one by position alone. It counts bytes rather than tracking the
	 * furthest one for that reason, so lowering this stays correct. */

	if (client->verify_tls && client->app_ctx->tls_ctx) {
		client_config.tls_connection_options =
			&client->app_ctx->tls_connection_options;
		client_config.tls_mode = AWS_MR_TLS_ENABLED;
	} else {
		client_config.tls_mode = AWS_MR_TLS_DISABLED;
	}

	/* Freeing the client is deferred to this callback rather than done when the
	 * last reference drops -- see s3_client_put(). */
	client_config.shutdown_callback = s3_client_shutdown_complete;
	client_config.shutdown_callback_user_data = client;

	client->aws_client = aws_s3_client_new(client->app_ctx->allocator, &client_config);
	if (!client->aws_client) {
		SPDK_ERRLOG("Failed to create aws_s3_client for endpoint %s\n",
			    target->endpoint);
		aws_credentials_provider_release(client->creds_provider);
		free(client);
		return -ENOMEM;
	}

	g_clients[g_num_clients++] = client;

	SPDK_NOTICELOG("Created S3 client for endpoint %s, bucket %s, path_style=%d, tls=%d\n",
		       target->endpoint, target->bucket,
		       target->use_path_style, target->verify_tls);
	*out = client;
	return 0;
}

/* Runs on a CRT thread once the S3 client has finished shutting down, which is
 * after every meta request it owned has completed and every finish callback has
 * returned.
 *
 * That is the only point at which freeing the client is safe. Finish callbacks
 * dereference req->client to record statistics -- get_ops, inflight, bytes_read --
 * and aws_s3_client_release() is asynchronous, so freeing right after it returns
 * leaves those callbacks writing into freed memory. */
static void
s3_client_shutdown_complete(void *user_data)
{
	struct s3_client *client = user_data;

	/* After the aws_client is gone, not before: it signs requests with this. */
	aws_credentials_provider_release(client->creds_provider);
	free(client);
}

void
s3_client_put(struct s3_client *client)
{
	uint32_t i;

	if (!client) {
		return;
	}

	assert(client->refcnt > 0);
	client->refcnt--;

	if (client->refcnt > 0) {
		SPDK_NOTICELOG("S3 client %s refcnt decremented to %u\n",
			       client->endpoint, client->refcnt);
		return;
	}

	SPDK_NOTICELOG("Destroying S3 client for endpoint %s\n", client->endpoint);

	/* Out of the pool here, on this thread: g_clients is protected by everything
	 * touching it running on the same reactor thread, whereas the free below
	 * happens on a CRT thread. Removing it now also stops
	 * s3_client_get_or_create() from handing out a client that is being torn
	 * down. */
	for (i = 0; i < g_num_clients; i++) {
		if (g_clients[i] == client) {
			g_clients[i] = g_clients[g_num_clients - 1];
			g_clients[g_num_clients - 1] = NULL;
			g_num_clients--;
			break;
		}
	}

	if (!client->aws_client) {
		/* Half-built, from a failed create. Nothing is in flight and no shutdown
		 * callback will ever arrive, so this is the only chance to free it. */
		aws_credentials_provider_release(client->creds_provider);
		free(client);
		return;
	}

	/* Release only. The free happens in s3_client_shutdown_complete(), once CRT
	 * confirms nothing is still running against this client. */
	aws_s3_client_release(client->aws_client);
}

/* ==========================================================================
 * GET OBJECT 
 * ========================================================================== */

static int
s3_get_body_callback(struct aws_s3_meta_request *meta_request,
		     const struct aws_byte_cursor *body,
		     uint64_t range_start, void *user_data)
{
	struct s3_request *req = user_data;
	const uint8_t *src = body->ptr;
	size_t src_len = body->len;
	uint64_t chunk_offset = range_start;
	uint64_t copy_offset;
	uint64_t req_end = req->offset + req->len;

	/* Map this chunk into the user buffer.
	 *
	 * range_start is the object-relative offset of this chunk. When a Range
	 * header is set, CRT reports offsets relative to the object, not to the
	 * range — so we rebase onto req->offset here.
	 *
	 * NOTE: body is const; we advance local copies (src/src_len) instead of
	 * mutating the cursor.
	 */
	if (chunk_offset < req->offset) {
		uint64_t skip = req->offset - chunk_offset;
		if (skip >= src_len) {
			return AWS_OP_SUCCESS;   /* entirely before our range */
		}
		src += skip;
		src_len -= skip;
		chunk_offset = req->offset;
	}

	if (chunk_offset >= req_end) {
		return AWS_OP_SUCCESS;           /* entirely past our range */
	}

	/* Clamp the tail so we never write beyond the caller's buffer */
	if (chunk_offset + src_len > req_end) {
		src_len = (size_t)(req_end - chunk_offset);
	}

	copy_offset = chunk_offset - req->offset;
	memcpy((uint8_t *)req->buf + copy_offset, src, src_len);

	/* Counted, not watermarked.
	 *
	 * A high-water mark -- bytes_read = max(bytes_read, copy_offset + src_len) --
	 * reports the far edge of what arrived rather than how much of it did, so a
	 * missing chunk in the middle is invisible: the caller is told the full length
	 * and reads whatever the buffer held where the gap is. For a materialisation
	 * buffer that is zeroes, and zeroes that pass as data are exactly the failure
	 * the short-read check in s3_export_bs_dev.c exists to catch -- it compares
	 * this number against what it asked for, and a watermark makes that comparison
	 * always succeed.
	 *
	 * A truncated object still reports correctly, which is what the owner marker
	 * read depends on: S3 shortens a range that runs past the end rather than
	 * failing, so the body simply stops early and the sum is the object's length --
	 * the same answer the watermark gave, since that body starts at the beginning.
	 *
	 * The sum cannot over-count. CRT delivers parts strictly in order and exactly
	 * once: s_s3_meta_request_body_streaming_pop_next_synced() will not release
	 * part N+1 until part N has gone out (it compares part_number against
	 * next_streaming_part and returns NULL otherwise), which is also why CRT's own
	 * recv_file path can fwrite() without seeking. A part that had to be retried is
	 * retried before it is ever delivered, so a body callback is only reached by
	 * data that is final. Overlapping chunks would break the sum, and nothing
	 * produces them: the part ranges CRT computes tile the object range exactly,
	 * and the two clamps above only ever shrink a chunk to the caller's range.
	 *
	 * Checked rather than asserted, because the cost of being wrong is a byte count
	 * larger than the buffer it describes -- which a caller would read as "all of
	 * it arrived". Cancelling the request turns a broken assumption into an error
	 * the caller sees, where an assert would either abort the process or, with
	 * NDEBUG, do nothing at all.
	 */
	if (src_len > req->len - req->bytes_read) {
		SPDK_ERRLOG("GetObject %s: body chunks overlap or exceed the range asked "
			    "for (offset %" PRIu64 ", length %" PRIu64 "): %zu more byte(s) "
			    "after %" PRIu64 " already delivered. Cancelling rather than "
			    "reporting a count the buffer cannot hold.\n",
			    req->key_buf, req->offset, req->len, src_len, req->bytes_read);
		return aws_raise_error(AWS_ERROR_S3_CANCELED);
	}
	req->bytes_read += src_len;

	return AWS_OP_SUCCESS;
}

static void
s3_get_finished(struct aws_s3_meta_request *meta_request,
		const struct aws_s3_meta_request_result *result,
		void *user_data)
{
	struct s3_request *req = user_data;

	if (result->error_code != AWS_ERROR_SUCCESS) {
		SPDK_ERRLOG("GetObject failed: key=%s, error=%d (%s), http=%d\n",
			    req->key_buf, result->error_code,
			    aws_error_str(result->error_code),
			    result->response_status);
		req->status = crt_error_to_errno(result->error_code,
						result->response_status);
		s3_stats_record_error(req->client, result->response_status);
		req->bytes_read = 0;
	} else {
		req->status = 0;
		S3_STAT_ADD(req->client, bytes_read, req->bytes_read);
	}

	S3_STAT_INC(req->client, get_ops);
	assert(S3_STAT_GET(req->client, inflight) > 0);
	S3_STAT_DEC(req->client, inflight);

	/* Release CRT state before the callback may hop threads. */
	aws_s3_meta_request_release(meta_request);

	s3_request_complete(req);
}

int
s3_get_range(struct s3_client *client, const char *key,
	     uint64_t offset, uint64_t len, void *buf,
	     s3_get_cb cb, void *cb_arg)
{
	struct s3_request *req;
	char range_hdr[128];
	int rc;

	if (!client || !key || !buf || !cb) {
		return -EINVAL;
	}

	if (len == 0) {
		/* Nothing to read — complete synchronously rather than issuing
		 * a request that would produce a malformed Range header. */
		cb(cb_arg, 0, 0);
		return 0;
	}

	req = aws_mem_calloc(client->app_ctx->allocator, 1, sizeof(*req));
	if (!req) {
		return -ENOMEM;
	}

	req->client  = client;
	req->allocator = client->app_ctx->allocator;
	req->offset  = offset;
	req->len     = len;
	req->buf     = buf;
	req->cb.get_cb = cb;
	req->cb_arg  = cb_arg;
	req->is_get  = true;
	req->owner_thread = spdk_get_thread();

	rc = s3_request_set_key(req, key);
	if (rc) {
		aws_mem_release(client->app_ctx->allocator, req);
		return rc;
	}

	struct aws_s3_meta_request_options options = {
		.type           = AWS_S3_META_REQUEST_TYPE_GET_OBJECT,
		.user_data      = req,
		.signing_config = &client->signing_config,
		.body_callback  = s3_get_body_callback,
		.finish_callback = s3_get_finished,
	};

	options.message = aws_http_message_new_request(client->app_ctx->allocator);
	if (!options.message) {
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	s3_set_common_headers(options.message, client);
	aws_http_message_set_request_method(options.message, aws_http_method_get);
	rc = s3_set_request_path(options.message, client->app_ctx->allocator,
				 client, req->key);
	if (rc != AWS_OP_SUCCESS) {
		aws_http_message_release(options.message);
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	/* Range header: always set, since len > 0 is guaranteed here.
	 *
	 * CRT may split this into several part-range GETs internally, issued in
	 * parallel, when the range is longer than its part size (8 MiB by default).
	 * Their bodies are still delivered in order and once each, which is what lets
	 * s3_get_body_callback() add up what it copies. */
	struct aws_http_header range_header;
	snprintf(range_hdr, sizeof(range_hdr), "bytes=%" PRIu64 "-%" PRIu64,
		 offset, offset + len - 1);
	range_header.name  = aws_byte_cursor_from_c_str("Range");
	range_header.value = aws_byte_cursor_from_c_str(range_hdr);
	aws_http_message_add_header(options.message, range_header);

	S3_STAT_INC(client, inflight);

	struct aws_s3_meta_request *meta_req =
		aws_s3_client_make_meta_request(client->aws_client, &options);
	aws_http_message_release(options.message);

	if (!meta_req) {
		SPDK_ERRLOG("GetObject: failed to create meta request for key=%s: %s\n",
			    key, aws_error_str(aws_last_error()));
		S3_STAT_DEC(client, inflight);
		aws_mem_release(client->app_ctx->allocator, req);
		return -EIO;
	}
	/* Do NOT touch req past this point — the finish callback may have
	 * already run and freed it. */

	return 0;
}

/* ==========================================================================
 * PUT OBJECT 
 * ========================================================================== */

static void
s3_put_finished(struct aws_s3_meta_request *meta_request,
		const struct aws_s3_meta_request_result *result,
		void *user_data)
{
	struct s3_request *req = user_data;

	if (result->error_code != AWS_ERROR_SUCCESS) {
		req->status = crt_error_to_errno(result->error_code,
						result->response_status);
		s3_stats_record_error(req->client, result->response_status);

		/* 412 Precondition Failed / 409 Conflict on If-None-Match: *
		 * means the object already exists. Expected during concurrent
		 * create  — log at a lower level. */
		if (req->if_none_match && req->status == -EEXIST) {
			SPDK_NOTICELOG("PutObject: key=%s already exists "
				       "(If-None-Match rejected, http=%d)\n",
				       req->key_buf, result->response_status);
		} else {
			SPDK_ERRLOG("PutObject failed: key=%s, error=%d (%s), http=%d\n",
				    req->key_buf, result->error_code,
				    aws_error_str(result->error_code),
				    result->response_status);
		}
	} else {
		req->status = 0;
		/* bytes_read holds the total payload size, captured at submit
		 * time — do NOT walk req->iov here, the caller may have already
		 * freed it. */
		S3_STAT_ADD(req->client, bytes_written, req->bytes_read);
	}

	S3_STAT_INC(req->client, put_ops);
	assert(S3_STAT_GET(req->client, inflight) > 0);
	S3_STAT_DEC(req->client, inflight);

	/* Release CRT state before the callback may hop threads. */
	aws_s3_meta_request_release(meta_request);

	/* Free our private copy of the body */
	if (req->buf) {
		aws_mem_release(req->allocator, req->buf);
		req->buf = NULL;
	}

	s3_request_complete(req);
}

int
s3_put(struct s3_client *client, const char *key,
       struct iovec *iov, int iovcnt, bool if_none_match,
       s3_op_cb cb, void *cb_arg)
{
	struct s3_request *req;
	size_t total_size = 0;
	size_t body_pos;
	char content_length_str[32];
	int rc;

	if (!client || !key || !iov || iovcnt <= 0 || !cb) {
		return -EINVAL;
	}

	/* Calculate total payload size */
	for (int i = 0; i < iovcnt; i++) {
		total_size += iov[i].iov_len;
	}

	req = aws_mem_calloc(client->app_ctx->allocator, 1, sizeof(*req));
	if (!req) {
		return -ENOMEM;
	}

	req->client   = client;
	req->allocator = client->app_ctx->allocator;
	req->if_none_match = if_none_match;
	req->cb.op_cb = cb;
	req->cb_arg   = cb_arg;
	req->owner_thread = spdk_get_thread();
	/* bytes_read doubles as "total payload size" for PUT — the finish
	 * callback must not dereference the caller's iov. */
	req->bytes_read = total_size;

	rc = s3_request_set_key(req, key);
	if (rc) {
		aws_mem_release(client->app_ctx->allocator, req);
		return rc;
	}

	/* Copy the body into our own buffer. The caller is allowed to free iov
	 * as soon as this function returns, but CRT reads the stream
	 * asynchronously (and may re-read it on retry). */
	if (total_size > 0) {
		struct aws_byte_buf body_buf;
		if (aws_byte_buf_init(&body_buf, client->app_ctx->allocator, total_size)) {
			aws_mem_release(client->app_ctx->allocator, req);
			return -ENOMEM;
		}
		body_pos = 0;
		for (int i = 0; i < iovcnt; i++) {
			memcpy(body_buf.buffer + body_pos, iov[i].iov_base, iov[i].iov_len);
			body_pos += iov[i].iov_len;
		}
		body_buf.len = total_size;
		req->buf = body_buf.buffer;
	}

	struct aws_s3_meta_request_options options = {
		.type            = AWS_S3_META_REQUEST_TYPE_PUT_OBJECT,
		.user_data       = req,
		.signing_config  = &client->signing_config,
		.finish_callback = s3_put_finished,
	};

	options.message = aws_http_message_new_request(client->app_ctx->allocator);
	if (!options.message) {
		if (req->buf) {
			aws_mem_release(client->app_ctx->allocator, req->buf);
		}
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	s3_set_common_headers(options.message, client);

	/* Content-Length */
	snprintf(content_length_str, sizeof(content_length_str), "%zu", total_size);
	struct aws_http_header cl_header = {
		.name  = g_content_length_header_name,
		.value = aws_byte_cursor_from_c_str(content_length_str),
	};
	aws_http_message_add_header(options.message, cl_header);

	/* If-None-Match for create-once protection  */
	if (if_none_match) {
		struct aws_http_header inm_header = {
			.name  = aws_byte_cursor_from_c_str("If-None-Match"),
			.value = aws_byte_cursor_from_c_str("*"),
		};
		aws_http_message_add_header(options.message, inm_header);
	}

	aws_http_message_set_request_method(options.message, aws_http_method_put);
	rc = s3_set_request_path(options.message, client->app_ctx->allocator,
				 client, req->key);
	if (rc != AWS_OP_SUCCESS) {
		aws_http_message_release(options.message);
		if (req->buf) {
			aws_mem_release(client->app_ctx->allocator, req->buf);
		}
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	/* Body stream */
	if (total_size > 0 && req->buf) {
		struct aws_byte_cursor body_cursor =
			aws_byte_cursor_from_array(req->buf, total_size);
		struct aws_input_stream *stream =
			aws_input_stream_new_from_cursor(client->app_ctx->allocator, &body_cursor);
		if (!stream) {
			SPDK_ERRLOG("PutObject: failed to create body stream for key=%s\n", key);
			aws_http_message_release(options.message);
			aws_mem_release(client->app_ctx->allocator, req->buf);
			aws_mem_release(client->app_ctx->allocator, req);
			return -ENOMEM;
		}
		aws_http_message_set_body_stream(options.message, stream);
		aws_input_stream_release(stream);
	}

	S3_STAT_INC(client, inflight);

	struct aws_s3_meta_request *meta_req =
		aws_s3_client_make_meta_request(client->aws_client, &options);
	aws_http_message_release(options.message);

	if (!meta_req) {
		SPDK_ERRLOG("PutObject: failed to create meta request for key=%s: %s\n",
			    key, aws_error_str(aws_last_error()));
		S3_STAT_DEC(client, inflight);
		if (req->buf) {
			aws_mem_release(client->app_ctx->allocator, req->buf);
		}
		aws_mem_release(client->app_ctx->allocator, req);
		return -EIO;
	}
	/* Do NOT touch req past this point. */

	return 0;
}

/* ==========================================================================
 * HEAD OBJECT
 * ========================================================================== */

/* Parse Content-Length out of the response headers.
 *
 * NOTE: aws_s3_meta_request_result has no `response_headers` field — only
 * `error_response_headers` (populated on failure). Success-path headers are
 * only available via headers_callback, so that is where we capture the size.
 */
static int
s3_head_headers_callback(struct aws_s3_meta_request *meta_request,
			 const struct aws_http_headers *headers,
			 int response_status, void *user_data)
{
	struct s3_request *req = user_data;
	struct aws_byte_cursor cl_name = aws_byte_cursor_from_c_str("content-length");
	struct aws_byte_cursor value;

	if (!req->out_size) {
		return AWS_OP_SUCCESS;
	}

	if (aws_http_headers_get(headers, cl_name, &value) == AWS_OP_SUCCESS) {
		/* value.ptr is NOT null-terminated — copy into a local buffer
		 * before calling strtoull. */
		char num_buf[32];
		size_t n = value.len < sizeof(num_buf) - 1 ? value.len : sizeof(num_buf) - 1;
		memcpy(num_buf, value.ptr, n);
		num_buf[n] = '\0';
		*req->out_size = strtoull(num_buf, NULL, 10);
	} else {
		/* Header absent (shouldn't happen for HeadObject) */
		aws_reset_error();
		*req->out_size = 0;
	}

	return AWS_OP_SUCCESS;
}

static void
s3_head_finished(struct aws_s3_meta_request *meta_request,
		 const struct aws_s3_meta_request_result *result,
		 void *user_data)
{
	struct s3_request *req = user_data;

	if (result->error_code != AWS_ERROR_SUCCESS) {
		req->status = crt_error_to_errno(result->error_code,
						result->response_status);
		s3_stats_record_error(req->client, result->response_status);

		/* 404 is a normal, expected answer for "does this key exist?" —
		 * used by the create/attach branch decision . */
		if (req->status != -ENOENT) {
			SPDK_ERRLOG("HeadObject failed: key=%s, error=%d (%s), http=%d\n",
				    req->key_buf, result->error_code,
				    aws_error_str(result->error_code),
				    result->response_status);
		}
	} else {
		req->status = 0;
	}

	S3_STAT_INC(req->client, head_ops);
	assert(S3_STAT_GET(req->client, inflight) > 0);
	S3_STAT_DEC(req->client, inflight);

	/* Release CRT state before the callback may hop threads. */
	aws_s3_meta_request_release(meta_request);

	s3_request_complete(req);
}

int
s3_head(struct s3_client *client, const char *key,
	uint64_t *out_size, s3_op_cb cb, void *cb_arg)
{
	struct s3_request *req;
	int rc;

	if (!client || !key || !cb) {
		return -EINVAL;
	}

	req = aws_mem_calloc(client->app_ctx->allocator, 1, sizeof(*req));
	if (!req) {
		return -ENOMEM;
	}

	req->client    = client;
	req->allocator = client->app_ctx->allocator;
	req->out_size  = out_size;
	req->cb.op_cb  = cb;
	req->cb_arg    = cb_arg;
	req->owner_thread = spdk_get_thread();

	if (out_size) {
		*out_size = 0;
	}

	rc = s3_request_set_key(req, key);
	if (rc) {
		aws_mem_release(client->app_ctx->allocator, req);
		return rc;
	}

	struct aws_s3_meta_request_options options = {
		.type             = AWS_S3_META_REQUEST_TYPE_DEFAULT,
		.operation_name   = aws_byte_cursor_from_c_str("HeadObject"),
		.user_data        = req,
		.signing_config   = &client->signing_config,
		.headers_callback = s3_head_headers_callback,
		.finish_callback  = s3_head_finished,
	};

	options.message = aws_http_message_new_request(client->app_ctx->allocator);
	if (!options.message) {
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	s3_set_common_headers(options.message, client);
	aws_http_message_set_request_method(options.message, aws_http_method_head);
	rc = s3_set_request_path(options.message, client->app_ctx->allocator,
				 client, req->key);
	if (rc != AWS_OP_SUCCESS) {
		aws_http_message_release(options.message);
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	S3_STAT_INC(client, inflight);

	struct aws_s3_meta_request *meta_req =
		aws_s3_client_make_meta_request(client->aws_client, &options);
	aws_http_message_release(options.message);

	if (!meta_req) {
		SPDK_ERRLOG("HeadObject: failed to create meta request for key=%s: %s\n",
			    key, aws_error_str(aws_last_error()));
		S3_STAT_DEC(client, inflight);
		aws_mem_release(client->app_ctx->allocator, req);
		return -EIO;
	}
	/* Do NOT touch req past this point. */

	return 0;
}

/* ==========================================================================
 * DELETE OBJECT
 * ========================================================================== */

/* Forward decl: batch fan-out completion accounting (see DELETE BATCH). */
static void s3_delete_batch_one_done(struct s3_delete_batch_ctx *batch, int status);

static void
s3_delete_finished(struct aws_s3_meta_request *meta_request,
		   const struct aws_s3_meta_request_result *result,
		   void *user_data)
{
	struct s3_request *req = user_data;

	if (result->error_code != AWS_ERROR_SUCCESS &&
	    result->response_status != 404) {
		/* 404 on delete is success — DELETE is idempotent, and GC may
		 * legitimately retry a key that was already collected . */
		SPDK_ERRLOG("DeleteObject failed: key=%s, error=%d (%s), http=%d\n",
			    req->key_buf, result->error_code,
			    aws_error_str(result->error_code),
			    result->response_status);
		req->status = crt_error_to_errno(result->error_code,
						result->response_status);
		s3_stats_record_error(req->client, result->response_status);
	} else {
		req->status = 0;
	}

	S3_STAT_INC(req->client, delete_ops);
	assert(S3_STAT_GET(req->client, inflight) > 0);
	S3_STAT_DEC(req->client, inflight);

	/* Release CRT state before anything may hop threads. */
	aws_s3_meta_request_release(meta_request);

	if (req->batch) {
		/* Batch fan-out: the user callback belongs to the batch, not to
		 * this individual key, and only the last child fires it. The
		 * bounce to the submitting thread happens inside the batch (see
		 * s3_delete_batch_one_done), so this request is done for good. */
		struct s3_delete_batch_ctx *batch = req->batch;
		int status = req->status;

		aws_mem_release(req->allocator, req);
		s3_delete_batch_one_done(batch, status);
		return;
	}

	s3_request_complete(req);
}

/* Submit a single-key DeleteObject.
 *
 * Shared by s3_delete() and the s3_delete_batch() fan-out. On success the
 * request is owned by CRT and must not be touched again; on failure the caller
 * still owns `req` and is responsible for releasing it.
 */
static int
s3_delete_submit_one(struct s3_client *client, struct s3_request *req)
{
	int rc;

	struct aws_s3_meta_request_options options = {
		.type            = AWS_S3_META_REQUEST_TYPE_DEFAULT,
		.operation_name  = aws_byte_cursor_from_c_str("DeleteObject"),
		.user_data       = req,
		.signing_config  = &client->signing_config,
		.finish_callback = s3_delete_finished,
	};

	options.message = aws_http_message_new_request(client->app_ctx->allocator);
	if (!options.message) {
		return -ENOMEM;
	}

	s3_set_common_headers(options.message, client);
	aws_http_message_set_request_method(options.message, aws_http_method_delete);
	rc = s3_set_request_path(options.message, client->app_ctx->allocator,
				 client, req->key);
	if (rc != AWS_OP_SUCCESS) {
		aws_http_message_release(options.message);
		return -ENOMEM;
	}

	S3_STAT_INC(client, inflight);

	struct aws_s3_meta_request *meta_req =
		aws_s3_client_make_meta_request(client->aws_client, &options);
	aws_http_message_release(options.message);

	if (!meta_req) {
		SPDK_ERRLOG("DeleteObject: failed to create meta request for key=%s: %s\n",
			    req->key_buf, aws_error_str(aws_last_error()));
		S3_STAT_DEC(client, inflight);
		return -EIO;
	}
	/* Do NOT touch req past this point. */

	return 0;
}

int
s3_delete(struct s3_client *client, const char *key,
	  s3_op_cb cb, void *cb_arg)
{
	struct s3_request *req;
	int rc;

	/* cb may be NULL: see the header. This used to reject it, which silently
	 * disabled every fire-and-forget delete in the tree -- the callers passed
	 * NULL and ignored the return value, so their objects were never deleted
	 * and nothing said so. s3_request_invoke_cb() has always checked the
	 * pointer, so the completion path needs nothing. */
	if (!client || !key) {
		return -EINVAL;
	}

	req = aws_mem_calloc(client->app_ctx->allocator, 1, sizeof(*req));
	if (!req) {
		return -ENOMEM;
	}

	req->client    = client;
	req->allocator = client->app_ctx->allocator;
	req->cb.op_cb  = cb;
	req->cb_arg    = cb_arg;
	req->owner_thread = spdk_get_thread();

	rc = s3_request_set_key(req, key);
	if (rc) {
		aws_mem_release(client->app_ctx->allocator, req);
		return rc;
	}

	rc = s3_delete_submit_one(client, req);
	if (rc) {
		aws_mem_release(client->app_ctx->allocator, req);
		return rc;
	}

	return 0;
}

/* ==========================================================================
 * DELETE BATCH (GC)
 *
 * Implemented as a fan-out of N single-key DeleteObject requests rather than
 * one DeleteObjects (POST /?delete) call. Rationale:
 *
 *   - DeleteObjects returns HTTP 200 even when individual keys failed; the
 *     per-key <Error> entries live in the XML response body. Getting real
 *     per-key status therefore requires accumulating and parsing that XML,
 *     which CRT's AWS_S3_META_REQUEST_TYPE_DEFAULT does not hand us for
 *     successful responses (same limitation that blocks s3_list_objects).
 *     Until that is solved, a 200 tells us almost nothing.
 *   - DeleteObjects also mandates a Content-MD5 header that SigV4 payload
 *     signing does not substitute for — verified against COS, which rejects
 *     the request with 400 otherwise.
 *   - Single-key DELETE gives an unambiguous per-key HTTP status, is
 *     idempotent (404 counts as success), and CRT already pipelines the
 *     requests over the shared connection pool.
 *
 * Cost: N round trips instead of 1. For GC that is acceptable — it runs in
 * the background and CRT issues the deletes concurrently. If a backend-tuned
 * fast path is ever needed, add DeleteObjects back behind a capability flag
 * *and* parse the response body; do not reintroduce it as a blind 200 check.
 * ========================================================================== */

/* Shared context for one s3_delete_batch() call.
 *
 * Lifetime: created before any child request is submitted, destroyed by
 * whichever child completes last. `remaining` is the ref count.
 *
 * Threading: children complete on arbitrary CRT IO threads, concurrently, so
 * `remaining` and `first_error` must be atomic. Everything else is written
 * once before submission and only read afterwards.
 */
struct s3_delete_batch_ctx {
	struct s3_client        *client;

	/* Held directly for the same reason struct s3_request does: the batch is
	 * destroyed by whichever child completes last, and that can be one
	 * spdk_thread_send_msg() hop after the client was freed. */
	struct aws_allocator    *allocator;

	s3_op_cb                 cb;
	void                    *cb_arg;

	uint32_t                 total;

	/* Outstanding children. Hits zero exactly once → fire user callback. */
	struct aws_atomic_var    remaining;

	/* First non-zero status reported by any child, as a negated errno.
	 * Stored negated (i.e. positive) because aws_atomic_var holds size_t;
	 * 0 means "no error yet". */
	struct aws_atomic_var    first_error;

	/* Count of failed keys, for the summary log line. */
	struct aws_atomic_var    failed;

	/* Aggregate status, settled by the last child before the callback is
	 * delivered. Only read after `remaining` hits zero, so plain int. */
	int                      final_status;

	struct spdk_thread      *owner_thread;
};

static void
s3_delete_batch_ctx_destroy(struct s3_delete_batch_ctx *batch)
{
	aws_mem_release(batch->allocator, batch);
}

/* Runs on batch->owner_thread. */
static void
s3_delete_batch_complete_on_owner(void *ctx)
{
	struct s3_delete_batch_ctx *batch = ctx;
	s3_op_cb cb     = batch->cb;
	void    *cb_arg = batch->cb_arg;
	int      status = batch->final_status;

	s3_delete_batch_ctx_destroy(batch);

	if (cb) {
		cb(cb_arg, status);
	}
}

/* Called once per child completion, from a CRT IO thread. */
static void
s3_delete_batch_one_done(struct s3_delete_batch_ctx *batch, int status)
{
	if (status != 0) {
		size_t expected = 0;

		aws_atomic_fetch_add(&batch->failed, 1);
		/* Keep the first error only; later ones are usually noise from
		 * the same root cause. */
		aws_atomic_compare_exchange_int(&batch->first_error, &expected,
						(size_t)(-status));
	}

	/* fetch_sub returns the value *before* the subtraction, so the child
	 * that sees 1 is the last one. */
	if (aws_atomic_fetch_sub(&batch->remaining, 1) != 1) {
		return;
	}

	/* Last child: settle up and fire the user callback exactly once. */
	size_t err    = aws_atomic_load_int(&batch->first_error);
	size_t failed = aws_atomic_load_int(&batch->failed);

	batch->final_status = err ? -(int)err : 0;

	if (failed) {
		SPDK_ERRLOG("Delete batch: %zu of %u keys failed, "
			    "first error=%d\n", failed, batch->total,
			    batch->final_status);
	}

	/* Bounce to the submitting thread, same contract as s3_request_complete().
	 * Note the batch must stay alive across the hop, so it is freed on the
	 * far side rather than here. */
	if (batch->owner_thread == NULL ||
	    batch->owner_thread == spdk_get_thread()) {
		s3_delete_batch_complete_on_owner(batch);
		return;
	}

	spdk_thread_send_msg(batch->owner_thread,
			     s3_delete_batch_complete_on_owner, batch);
}

int
s3_delete_batch(struct s3_client *client, const char **keys, uint32_t count,
		s3_op_cb cb, void *cb_arg)
{
	struct s3_delete_batch_ctx *batch;
	uint32_t i;

	if (!client || !keys || count == 0 || !cb) {
		return -EINVAL;
	}

	if (count > S3_MAX_KEYS_PER_DELETE) {
		SPDK_ERRLOG("Delete batch: %u keys exceeds max %u\n",
			    count, S3_MAX_KEYS_PER_DELETE);
		return -EINVAL;
	}

	/* Validate every key up front. Submitting a partial batch and then
	 * failing would leave the caller unable to tell what happened: it gets
	 * an error return *and* callbacks for the keys already in flight. */
	for (i = 0; i < count; i++) {
		size_t len;

		if (!keys[i]) {
			SPDK_ERRLOG("Delete batch: keys[%u] is NULL\n", i);
			return -EINVAL;
		}
		len = strlen(keys[i]);
		if (len == 0) {
			SPDK_ERRLOG("Delete batch: keys[%u] is empty\n", i);
			return -EINVAL;
		}
		if (len >= S3_MAX_KEY_LEN) {
			SPDK_ERRLOG("Delete batch: keys[%u] too long: %zu bytes "
				    "(max %d)\n", i, len, S3_MAX_KEY_LEN - 1);
			return -ENAMETOOLONG;
		}
	}

	batch = aws_mem_calloc(client->app_ctx->allocator, 1, sizeof(*batch));
	if (!batch) {
		return -ENOMEM;
	}

	batch->client       = client;
	batch->allocator    = client->app_ctx->allocator;
	batch->cb           = cb;
	batch->cb_arg       = cb_arg;
	batch->total        = count;
	batch->owner_thread = spdk_get_thread();

	/* Start at count + 1: the extra reference is held by this function so
	 * the batch cannot be completed-and-freed by a fast child while we are
	 * still submitting the rest. Released at the end of the submit loop.
	 *
	 * NOTE: we deliberately do NOT stash `keys` — the caller may free the
	 * array as soon as we return. Each key is copied into its own request. */
	aws_atomic_init_int(&batch->remaining, (size_t)count + 1);
	aws_atomic_init_int(&batch->first_error, 0);
	aws_atomic_init_int(&batch->failed, 0);

	for (i = 0; i < count; i++) {
		struct s3_request *req;
		int rc;

		req = aws_mem_calloc(client->app_ctx->allocator, 1, sizeof(*req));
		if (!req) {
			rc = -ENOMEM;
			goto submit_failed;
		}

		req->client       = client;
		req->allocator = client->app_ctx->allocator;
		req->batch        = batch;
		req->owner_thread = batch->owner_thread;
		/* cb/cb_arg stay NULL: completion is reported to the batch,
		 * which owns the user callback. */

		rc = s3_request_set_key(req, keys[i]);
		if (rc == 0) {
			rc = s3_delete_submit_one(client, req);
		}

		if (rc != 0) {
			aws_mem_release(client->app_ctx->allocator, req);
			goto submit_failed;
		}
		/* req is owned by CRT now; its completion will decrement. */
		continue;

submit_failed:
		/* This child never reached CRT, so nothing will ever decrement
		 * for it. Account for it here — and for every key we have not
		 * even attempted yet — so the count still converges. */
		SPDK_ERRLOG("Delete batch: failed to submit key %u/%u (%s): %d\n",
			    i + 1, count, keys[i], rc);
		for (uint32_t j = i; j < count; j++) {
			s3_delete_batch_one_done(batch, rc);
		}
		break;
	}

	/* Drop our submit-time reference. If every child already finished, this
	 * is what fires the user callback — possibly on this very thread. */
	s3_delete_batch_one_done(batch, 0);

	return 0;
}

/* ==========================================================================
 * COPY OBJECT (server-side)
 * ========================================================================== */

static void
s3_copy_finished(struct aws_s3_meta_request *meta_request,
		 const struct aws_s3_meta_request_result *result,
		 void *user_data)
{
	struct s3_request *req = user_data;

	if (result->error_code != AWS_ERROR_SUCCESS) {
		SPDK_ERRLOG("CopyObject failed: %s/%s -> %s, error=%d (%s), http=%d\n",
			    req->src_bucket, req->src_key, req->key_buf,
			    result->error_code, aws_error_str(result->error_code),
			    result->response_status);
		req->status = crt_error_to_errno(result->error_code,
						result->response_status);
		s3_stats_record_error(req->client, result->response_status);
	} else {
		/* NOTE: CopyObject can return 200 with an <Error> in the body
		 * (S3 keeps the connection alive during a long server-side
		 * copy). Callers that must be certain should HEAD the
		 * destination afterwards. */
		req->status = 0;
	}

	S3_STAT_INC(req->client, copy_ops);
	assert(S3_STAT_GET(req->client, inflight) > 0);
	S3_STAT_DEC(req->client, inflight);

	/* Release CRT state before the callback may hop threads. */
	aws_s3_meta_request_release(meta_request);

	s3_request_complete(req);
}

int
s3_copy_object(struct s3_client *client,
	       const char *src_bucket, const char *src_key,
	       const char *dst_key,
	       s3_op_cb cb, void *cb_arg)
{
	struct s3_request *req;
	/* "/" + bucket + "/" + key + NUL */
	char copy_source[S3_MAX_BUCKET_LEN + S3_MAX_KEY_LEN + 3];
	int rc;

	if (!client || !src_bucket || !src_key || !dst_key || !cb) {
		return -EINVAL;
	}

	if (strlen(src_bucket) >= S3_MAX_BUCKET_LEN ||
	    strlen(src_key) >= S3_MAX_KEY_LEN ||
	    strlen(dst_key) >= S3_MAX_KEY_LEN) {
		SPDK_ERRLOG("CopyObject: bucket or key too long "
			    "(bucket max %d, key max %d)\n",
			    S3_MAX_BUCKET_LEN - 1, S3_MAX_KEY_LEN - 1);
		return -ENAMETOOLONG;
	}

	req = aws_mem_calloc(client->app_ctx->allocator, 1, sizeof(*req));
	if (!req) {
		return -ENOMEM;
	}

	req->client    = client;
	req->allocator = client->app_ctx->allocator;
	req->cb.op_cb  = cb;
	req->cb_arg    = cb_arg;
	req->owner_thread = spdk_get_thread();

	rc = s3_request_set_key(req, dst_key);
	if (rc) {
		aws_mem_release(client->app_ctx->allocator, req);
		return rc;
	}

	/* Lengths already validated above, so these cannot truncate */
	snprintf(req->src_bucket, sizeof(req->src_bucket), "%s", src_bucket);
	snprintf(req->src_key, sizeof(req->src_key), "%s", src_key);

	/* x-amz-copy-source: /src_bucket/src_key */
	snprintf(copy_source, sizeof(copy_source), "/%s/%s", src_bucket, src_key);

	struct aws_s3_meta_request_options options = {
		.type            = AWS_S3_META_REQUEST_TYPE_DEFAULT,
		.operation_name  = aws_byte_cursor_from_c_str("PutObjectCopy"),
		.user_data       = req,
		.signing_config  = &client->signing_config,
		.finish_callback = s3_copy_finished,
	};

	options.message = aws_http_message_new_request(client->app_ctx->allocator);
	if (!options.message) {
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	s3_set_common_headers(options.message, client);

	/* x-amz-copy-source header */
	struct aws_http_header copy_src_header = {
		.name  = aws_byte_cursor_from_c_str("x-amz-copy-source"),
		.value = aws_byte_cursor_from_c_str(copy_source),
	};
	aws_http_message_add_header(options.message, copy_src_header);

	aws_http_message_set_request_method(options.message, aws_http_method_put);
	rc = s3_set_request_path(options.message, client->app_ctx->allocator,
				 client, req->key);
	if (rc != AWS_OP_SUCCESS) {
		aws_http_message_release(options.message);
		aws_mem_release(client->app_ctx->allocator, req);
		return -ENOMEM;
	}

	S3_STAT_INC(client, inflight);

	struct aws_s3_meta_request *meta_req =
		aws_s3_client_make_meta_request(client->aws_client, &options);
	aws_http_message_release(options.message);

	if (!meta_req) {
		SPDK_ERRLOG("CopyObject: failed to create meta request for %s -> %s: %s\n",
			    src_key, dst_key, aws_error_str(aws_last_error()));
		S3_STAT_DEC(client, inflight);
		aws_mem_release(client->app_ctx->allocator, req);
		return -EIO;
	}
	/* Do NOT touch req past this point. */

	return 0;
}

/* ==========================================================================
 * LIST OBJECTS (GC scan, export manifest)
 *
 * NOT YET IMPLEMENTED — deliberately returns -ENOTSUP rather than silently
 * succeeding without invoking entry_cb.
 *
 * Two problems must be solved together:
 *
 *  1. Response body access. For AWS_S3_META_REQUEST_TYPE_DEFAULT, CRT delivers
 *     the success-path body through body_callback, but only in chunks with no
 *     framing guarantees. ListObjectsV2 XML must therefore be accumulated
 *     across callbacks and parsed once complete (aws_xml_parser from
 *     aws-c-common, or CRT's own s3_list_objects.c helper if the linked
 *     version exposes aws_s3_paginator).
 *
 *  2. Query parameter encoding. prefix and continuation-token both require
 *     RFC 3986 percent-encoding. Continuation tokens are base64 and routinely
 *     contain '+', '/' and '=' — pasting them raw into the path produces
 *     wrong results that look like premature end-of-listing, which is a
 *     particularly nasty failure mode for GC (it would under-collect
 *     silently). Use aws_byte_buf_append_encoding_uri_param().
 *
 * Callers (s3_gc.c, s3_export.c) are not written yet, so failing loudly here
 * is strictly better than returning 0 and never calling entry_cb.
 * ========================================================================== */

int
s3_list_objects(struct s3_client *client, const char *prefix,
		const char *continuation_token,
		void (*entry_cb)(void *ctx, const char *key, uint64_t size),
		void *entry_ctx, s3_op_cb cb, void *cb_arg)
{
	if (!client || !cb) {
		return -EINVAL;
	}

	SPDK_ERRLOG("s3_list_objects() not implemented yet "
		    "(prefix=%s) — needs XML accumulation + URI encoding, "
		    "see the comment above\n",
		    prefix ? prefix : "(null)");

	return -ENOTSUP;
}

/* ==========================================================================
 * Stats 
 * ========================================================================== */

void
s3_client_get_stats(struct s3_client *client, struct s3_client_stats *out)
{
	if (!client || !out) {
		return;
	}

	/* Snapshot, not an atomic view of the whole struct: the counters are
	 * read one at a time, so a caller can observe e.g. get_ops already
	 * bumped while bytes_read is not. Fine for metrics; do not build
	 * invariants on cross-field consistency. */
	out->get_ops       = S3_STAT_GET(client, get_ops);
	out->put_ops       = S3_STAT_GET(client, put_ops);
	out->head_ops      = S3_STAT_GET(client, head_ops);
	out->delete_ops    = S3_STAT_GET(client, delete_ops);
	out->copy_ops      = S3_STAT_GET(client, copy_ops);
	out->bytes_read    = S3_STAT_GET(client, bytes_read);
	out->bytes_written = S3_STAT_GET(client, bytes_written);
	out->errors_4xx    = S3_STAT_GET(client, errors_4xx);
	out->errors_5xx    = S3_STAT_GET(client, errors_5xx);
	out->retries       = S3_STAT_GET(client, retries);
	out->inflight      = S3_STAT_GET(client, inflight);
}
