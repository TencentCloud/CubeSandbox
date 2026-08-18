/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3lvol common type definitions
 */

#ifndef S3LVOL_TYPES_H
#define S3LVOL_TYPES_H

#include "spdk/stdinc.h"
#include "spdk/uuid.h"

/* ==========================================================================
 * Fixed constants (design principle P4: a single block granularity end to end)
 * ========================================================================== */

/* The block size is fixed at 4 KiB and is not configurable.
 * It runs through blobstore page / s3_bs_dev block / NVMe LBA size / user I/O
 * boundaries alike. */
#define S3LVOL_BLOCK_SIZE       4096
#define S3LVOL_BLOCK_SHIFT      12

/* Defaults; each can be overridden through the create_lvstore RPC */
#define S3LVOL_DEFAULT_CHUNK_SIZE    (1024 * 1024)   /* 1 MiB, the LBA range one S3 object carries */
#define S3LVOL_DEFAULT_CLUSTER_SIZE  (1024 * 1024)   /* 1 MiB, blobstore CoW granularity */

/* Snapshot chain depth limits */
#define S3LVOL_DEFAULT_MAX_CHAIN_DEPTH   32
#define S3LVOL_DEFAULT_SOFT_CHAIN_DEPTH  24

/* ==========================================================================
 * S3 target
 * The bucket is an lvstore-level property, bound at create/attach time and
 * immutable for the lifetime of the lvstore.
 * ========================================================================== */

enum s3_auth_mode {
	S3_AUTH_STATIC = 0,   /* access_key + secret_key passed explicitly */
	S3_AUTH_ENV,          /* AWS_* environment variables */
	S3_AUTH_IAM,          /* EC2/EKS IAM role, auto-refreshed by CRT */
	S3_AUTH_STS,          /* AssumeRole */
	S3_AUTH_FILE,         /* ~/.aws/credentials */
};

struct s3_target {
	char                *endpoint;
	char                *region;
	char                *bucket;
	char                *prefix;

	enum s3_auth_mode    auth_mode;
	char                *access_key;
	char                *secret_key;
	char                *session_token;
	char                *profile;
	char                *role_arn;

	bool                 use_path_style;
	bool                 verify_tls;
	char                *ca_bundle_path;

	bool                 enable_sse;
	char                *sse_kms_key_id;
};

/* ==========================================================================
 * lvstore create/attach options
 * ========================================================================== */

struct s3_lvs_opts {
	struct s3_target     target;

	/* Namespace this lvstore lives in; resolved to \c target by the caller
	 * through rcow_namespace_to_target(). Not named "namespace" because this
	 * header is reachable from C++ translation units, where that is a keyword. */
	const char          *ns_name;
	const char          *lvs_name;

	/* The local bdev that carries WAL / chunk cache.
	 * When wal_bdev is NULL it shares the device with cache_bdev
	 * (single-bdev layout). */
	const char          *wal_bdev_name;
	const char          *cache_bdev_name;

	uint64_t             capacity_bytes;   /* logical provisioned capacity; required on create, read back from metadata on attach */
	uint32_t             chunk_size;
	uint32_t             cluster_size;

	uint32_t             wal_size_mb;
	uint32_t             journal_size_mb;
	uint32_t             cache_size_mb;
	uint32_t             uploader_threads;

	/* Longest a committed mapping may stay only in the journal, in seconds.
	 * 0 selects the built-in default (S3_CKPT_DEFAULT_INTERVAL_SEC).
	 *
	 * This is a recovery-time bound, not a space bound: the usage-based
	 * trigger already keeps the journal from filling, but under a light write
	 * load it can take days to reach, and a crash then has to replay all of
	 * it. Not persisted -- it is a policy of the running process, so an
	 * attach may legitimately pick a different one from the create. */
	uint32_t             checkpoint_interval_sec;

	bool                 enable_compression;
	bool                 auto_create_bucket;

	/* Ignore an existing owner marker on S3 and take over forcibly.
	 *
	 * It exists because of crash residue: the marker **deliberately does not
	 * expire**, so it is still there after a process is killed, and it is the
	 * normal starting point of the recovery flow. The semantics are "I have
	 * confirmed the other side is really gone" -- if it is actually still
	 * alive, two processes will write to the same keys at once. */
	bool                 force;
};

/* ==========================================================================
 * lvstore runtime states
 * ========================================================================== */

enum s3_lvstore_state {
	S3_LVS_NOT_PRESENT = 0,
	S3_LVS_CREATING,
	S3_LVS_BS_LOADING,
	S3_LVS_ONLINE,
	S3_LVS_DRAINING,
	S3_LVS_DETACHED,
	S3_LVS_FAILED_CLEANUP,
};

/* ==========================================================================
 * chunk_map entry
 * The chunk_index -> chunk_uuid mapping, the only level of the three-level
 * mapping chain that needs a table lookup.
 * ========================================================================== */

enum s3_chunk_flags {
	S3_CHUNK_ZERO        = 1u << 0,   /* unallocated; reads are zeroes, no S3 request */
	S3_CHUNK_IN_S3       = 1u << 1,   /* confirmed to exist on S3 */
	S3_CHUNK_DIRTY_LOCAL = 1u << 2,   /* data is in WAL / cache, not yet PUT to S3 */
	S3_CHUNK_UPLOADING   = 1u << 3,   /* PUT in flight */
};

struct s3_chunk_map_entry {
	struct spdk_uuid     uuid;
	uint64_t             gen;
	uint16_t             flags;
};

/* ==========================================================================
 * fork modes
 * ========================================================================== */

enum s3lvol_fork_mode {
	/* Pure clone: millisecond-class, shares the parent's chunks, accepts
	 * clone_count = N fan-out. The default and recommended mode for 1:N
	 * forks. */
	S3LVOL_FORK_SHARED = 0,

	/* clone + inflate, producing a fully independent volume. Expensive
	 * (full materialisation + cross-S3 traffic); use only when the
	 * dependency really must be cut. */
	S3LVOL_FORK_DETACHED,

	/* clone with background progressive inflate. v2 implementation. */
	S3LVOL_FORK_LAZY_DETACH,
};

/* ==========================================================================
 * chain-depth governance modes
 * ========================================================================== */

enum s3lvol_rectify_mode {
	/* Three phases: first delete the deletable intermediate snapshots (pure
	 * metadata, zero data movement), then decouple if that is not enough.
	 * Default. */
	S3LVOL_RECTIFY_AUTO = 0,

	/* Delete intermediate snapshots only; no data movement at all */
	S3LVOL_RECTIFY_DELETE_ONLY,

	S3LVOL_RECTIFY_DECOUPLE,
	S3LVOL_RECTIFY_INFLATE,
};

#endif /* S3LVOL_TYPES_H */
