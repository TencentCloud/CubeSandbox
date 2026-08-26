/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   S3 client abstract interface
 *
 *   === Key design constraint ===
 *   This header **must not expose any aws-c-* type**. struct s3_client is an
 *   opaque pointer; everything CRT-related is confined to
 *   lib/s3bsdev/s3_client_aws.c. Replacing the SDK later then touches exactly
 *   one file.
 *
 *   Every interface is asynchronous: the actual HTTP I/O is carried by CRT's
 *   event_loop_group threads, and on completion the callback is bounced back
 *   to the submitting SPDK thread via spdk_thread_send_msg. The caller is
 *   never blocked on a reactor.
 */

#ifndef S3LVOL_CLIENT_H
#define S3LVOL_CLIENT_H

#include "s3lvol/s3_types.h"

struct s3_client;        /* opaque */
struct s3_request;       /* opaque */

/**
 * === Callback thread semantics ===
 *
 * Every callback runs on the **SPDK thread that submitted the request**. The
 * implementation records `spdk_get_thread()` at submission time, and once CRT
 * finishes the request on its own I/O threads it bounces the callback back with
 * `spdk_thread_send_msg()`. So a callback may safely touch blobstore / bdev /
 * channel state.
 *
 * Exception: if the submission was not on an SPDK thread (a plain pthread,
 * such as unit tests and command-line tools), the callback runs in place on
 * the CRT thread -- in that scenario there is no SPDK state to protect anyway.
 *
 * **The caller must outlive its own in-flight requests.** There is no graceful
 * failure for `spdk_thread_send_msg()`: sending a message to an exited thread
 * `abort()`s directly. So the thread that submitted a request must not exit
 * before every callback has fired, and the `cb_arg` used in callbacks must not
 * be freed early. This matches SPDK's requirement for bdev I/O.
 *
 * One consequence: **a callback may fire at any moment after the submitting
 * function returns**, and may only be processed at the submitting thread's next
 * poll. Never assume it has already run.
 */

/**
 * Request completion callback.
 *
 * \param cb_arg  caller context
 * \param status  0 on success; negative errno on failure (CRT error codes are
 *                mapped). Common values: -ENOENT (404), -EACCES (403),
 *                -EIO (5xx), -ETIMEDOUT.
 */
typedef void (*s3_op_cb)(void *cb_arg, int status);

/**
 * Callback carrying the number of bytes read (for GET).
 *
 * \param bytes_read  bytes actually read
 */
typedef void (*s3_get_cb)(void *cb_arg, uint64_t bytes_read, int status);

/* ==========================================================================
 * Lifecycle
 * ========================================================================== */

/**
 * Process-level CRT initialisation. Called once at SPDK subsystem init.
 * Creates the event_loop_group / host_resolver / client_bootstrap / tls_ctx
 * internally and hooks the aws_allocator onto SPDK's memory pool.
 */
int s3_crt_global_init(uint32_t event_loop_threads);

void s3_crt_global_fini(void);

/**
 * Create (or reuse from the pool) an S3 client.
 *
 * Clients are pooled by **endpoint** and are independent of bucket
 * -- lvstores pointing at the same endpoint with different
 * buckets share one aws_s3_client. A refcnt is maintained internally,
 * decremented on detach, and only at zero is the client really destroyed.
 */
int s3_client_get_or_create(const struct s3_target *target, struct s3_client **out);

void s3_client_put(struct s3_client *client);

/* ==========================================================================
 * Object operations
 *
 * Every write follows the create-once semantics: objects
 * are named by uuid, are never modified after being written, and are only ever
 * deleted. Never overwrite. Every PUT is therefore naturally idempotent and
 * can be retried safely.
 * ========================================================================== */

/**
 * Read a range of an object. Internally uses CRT's auto-ranged GET; large
 * objects are split into pieces and fetched concurrently.
 *
 * \param key     the object key (relative to the bucket, no leading '/')
 * \param offset  offset within the object
 * \param len     bytes to read
 * \param buf     destination buffer; the caller must keep it alive until the
 *                callback fires
 */
int s3_get_range(struct s3_client *client, const char *key,
		 uint64_t offset, uint64_t len, void *buf,
		 s3_get_cb cb, void *cb_arg);

/**
 * Write an object. Internally uses CRT's auto-MPU; large objects are uploaded
 * in multiple parts automatically.
 *
 * \param iov    the source data; must stay alive until the callback fires
 * \param if_none_match  when true, adds `If-None-Match: *`, i.e. "write only if
 *               the key does not exist". A rejection returns -EEXIST.
 *
 *   **Do not treat this as a mutual-exclusion mechanism.** It is a
 *   "nice to have" extra check, not a correctness dependency:
 *   - Tencent COS is measured to **ignore the header outright**: it returns
 *     200 for an existing key and overwrites the object (CRT trace confirmed
 *     the header made it into the request and SigV4 SignedHeaders; the server
 *     just does not support it). AWS S3 only returns 412 since 2024-11.
 *   - Object uniqueness is guaranteed by **naming**: blobstore names carry a
 *     uuid attribute, so simultaneous creation on different nodes cannot
 *     collide.
 *
 *   So a caller passing if_none_match=true **must tolerate a return of 0** and
 *   must not infer "I am the first creator" from it.
 */
int s3_put(struct s3_client *client, const char *key,
	   struct iovec *iov, int iovcnt, bool if_none_match,
	   s3_op_cb cb, void *cb_arg);

/**
 * Query whether an object exists and its size.
 * Used for the create/attach branch decision (HEAD meta/super) and as a sanity
 * check before import.
 */
int s3_head(struct s3_client *client, const char *key,
	    uint64_t *out_size, s3_op_cb cb, void *cb_arg);

/**
 * Delete an object.
 *
 * `cb` may be NULL, meaning fire-and-forget: no interest in when it completes
 * or what the result is. Overwrite replacing an old chunk and unmap releasing
 * a chunk are such scenarios -- the mapping is already persisted, an object
 * that fails to delete merely leaves an orphan, correctness is unaffected, and
 * there is no reason to wait another round trip for it. Failures are still
 * logged (`s3_delete_finished` prints the key and HTTP status), so "nobody
 * took the result" is not "nobody can see it".
 *
 * 404 counts as success: DELETE is idempotent, and GC retrying an already
 * reclaimed key is normal.
 *
 * The return value only reflects whether the **submission** succeeded. Check it
 * even for fire-and-forget: this function once went silently inert because it
 * rejected NULL callbacks, the callers dropped the return value, and every
 * overwrite leaked an object for two months without anyone noticing.
 */
int s3_delete(struct s3_client *client, const char *key,
	      s3_op_cb cb, void *cb_arg);

/**
 * Batch delete. Used by GC.
 */
int s3_delete_batch(struct s3_client *client, const char **keys, uint32_t count,
		    s3_op_cb cb, void *cb_arg);

/**
 * Server-side copy (CopyObject); data never crosses the client's network.
 *
 * Purpose: because cluster_size == chunk_size == 1 MiB in this design, clusters
 * not modified by inflate / decouple can be copied server-side directly, saving
 * all data-plane traffic and leaving only control-plane RTT.
 *
 * **There is currently no caller, and that is deliberate** (2026-08-05). It was
 * meant for export "materialisation" (copying the objects into the exports
 * prefix before deleting a snapshot referenced by a zero-copy export); that
 * approach was rejected -- the reasoning and the alternative are in the header
 * comment of lib/s3bsdev/s3_gc.c. The inflate optimisation above still holds;
 * it just is not done yet.
 *
 * Before actually using it, know one trap: CopyObject returns **HTTP 200 with
 * `<Error>` in the body**. A DEFAULT-type meta request probably does not parse
 * the body, so after every object copy a HEAD verification is required -- the
 * status code alone is not enough.
 */
int s3_copy_object(struct s3_client *client,
		   const char *src_bucket, const char *src_key,
		   const char *dst_key,
		   s3_op_cb cb, void *cb_arg);

/**
 * List objects. Used by GC scans and export manifest enumeration.
 */
int s3_list_objects(struct s3_client *client, const char *prefix,
		    const char *continuation_token,
		    void (*entry_cb)(void *ctx, const char *key, uint64_t size),
		    void *entry_ctx, s3_op_cb cb, void *cb_arg);

/* ==========================================================================
 * Observability
 * ========================================================================== */

struct s3_client_stats {
	uint64_t  get_ops;
	uint64_t  put_ops;
	uint64_t  head_ops;
	uint64_t  delete_ops;
	uint64_t  copy_ops;
	uint64_t  bytes_read;
	uint64_t  bytes_written;
	uint64_t  errors_4xx;
	uint64_t  errors_5xx;
	uint64_t  retries;
	uint64_t  inflight;
};

void s3_client_get_stats(struct s3_client *client, struct s3_client_stats *stats);

#endif /* S3LVOL_CLIENT_H */
