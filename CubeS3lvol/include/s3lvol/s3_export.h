/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Cross-node transfer: export a snapshot, import it as an external clone
 *
 *   === What an export is ===
 *
 *   A self-contained, immutable, read-only copy of one snapshot, living in S3
 *   under its own prefix, described by a single manifest object:
 *
 *     <prefix>/exports/<export-uuid>.json        the manifest
 *     <prefix>/exports/<export-uuid>/<idx>       one object per non-zero chunk
 *
 *   The importing node needs nothing but those objects: no access to the source
 *   lvstore's chunk map, no agreement on cluster size, and no coordination with
 *   the source node. Once the manifest is up, the source may delete the
 *   snapshot, the volume, or the whole lvstore.
 *
 *   === Why it is a copy and not a reference ===
 *
 *   The design document has the manifest point straight at the source's live
 *   chunk objects, which would make an export pure metadata. That needs a
 *   translation from "offset inside this blob" to "LBA on the bs_dev", and no
 *   public blobstore API offers one -- the cluster table is private. Theways to
 *   obtain it anyway (reading blobstore's private structs, or probing the bs_dev
 *   with a one-block read) all fail by producing a *wrong* uuid, and a wrong
 *   uuid still reads back perfectly good bytes belonging to something else. That
 *   is a silent-corruption failure mode, so v1 pays for a copy instead.
 *
 *   The copy also keeps garbage collection trivial: export objects live under
 *   their own prefix, so nothing in <prefix>/data/ becomes live by being
 *   exported, and the design's cross-node pin / refcount machinery is not needed
 *   at all.
 *
 *   Cost, stated plainly: an export reads and re-uploads the allocated bytes of
 *   the snapshot once, and doubles their storage for as long as the export
 *   lives. The manifest carries a `layout` field so a future zero-copy variant
 *   (or a server-side CopyObject one) can be added without changing importers.
 */

#ifndef S3LVOL_EXPORT_H
#define S3LVOL_EXPORT_H

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_client.h"
#include "s3lvol/s3_types.h"

/* Bumped only for changes an old reader must refuse. Additive fields do not
 * bump it -- unknown members are ignored on parse.
 *
 * 2: the ref table shed its per-chunk valid_bytes. A version 1 reader handed a
 *    version 2 manifest would consume 24 bytes per ref out of a 16-byte-per-ref
 *    table, so every chunk past the first would name another chunk's object --
 *    which reads back as valid data from the wrong place. Hence a bump.
 *
 * 3: per-chunk sources (srcs[] plus src_idx[]), so that a snapshot whose chunks
 *    come partly from another export can be described at all. A version 2 reader
 *    would ignore src_idx and resolve every chunk against the single `src`,
 *    returning another object's bytes for the ones that belong elsewhere. Hence
 *    a bump.
 */
#define S3_EXPORT_VERSION       3

/* The oldest a reader accepts. Writing is always at S3_EXPORT_VERSION; reading
 * has to span the range, because manifests already in a bucket outlive the binary
 * that wrote them -- including ones this binary wrote before being upgraded.
 *
 * 1 is excluded rather than merely old: its 24-byte ref entries read as 16-byte
 * ones name a different object for every chunk past the first, and return
 * plausible data from the wrong place. That is the failure a version exists to
 * prevent, so it stays refused. */
#define S3_EXPORT_VERSION_MIN   2

/* How many prefixes one manifest may reference. A derived export adds its own
 * prefix to the ones it inherited, so this bounds the length of a derivation
 * lineage: A publishes, B imports and republishes, C imports and republishes.
 *
 * Explicit, and much smaller than the 255 a one-byte index would allow, because
 * the ceiling has to be reachable in a test. What happens past it is a fallback
 * to copying -- correct, but it turns an O(1) publish into an O(size) one, and a
 * limit that can only be hit by a lineage nobody is watching is a limit nobody
 * finds out about until it costs them a volume. */
#define S3_EXPORT_MAX_SOURCES   16

/* How a manifest names the bytes it describes.
 *
 * A reader that meets a layout it does not know must fail, not guess: the two
 * below name *different objects* for the same chunk, so reading one as the other
 * resolves every chunk to something unrelated -- and unrelated bytes come back
 * without an error. Hence a hard check rather than a hint. */
enum s3_export_layout {
	/* Zero copy, and the default. Each chunk names the live object of the
	 * source lvstore's chunk map: <src.prefix>/data/<chunk uuid>. Creating one
	 * moves no data, which is what makes a cross-node handoff cost a drain plus
	 * a single PUT instead of a pass over the volume.
	 *
	 * The price is a dependency. Those objects stay readable only while the
	 * source keeps the exported snapshot, because it is the snapshot's existence
	 * that keeps them in the source's chunk map and therefore out of reach of
	 * its GC. When the source wants the snapshot gone, it materialises the
	 * export into the layout below rather than breaking the importer. */
	S3_EXPORT_LAYOUT_REF   = 0,

	/* Self contained. Each chunk is an export-private copy at
	 * <src.prefix>/exports/<export uuid>/<index>; nothing outside that prefix is
	 * referenced, so the source may delete the snapshot, the volume, or the
	 * whole lvstore.
	 *
	 * Produced either by exporting across buckets or regions, where there is
	 * nothing to reference in the first place, or by materialising a ref
	 * export. */
	S3_EXPORT_LAYOUT_DENSE = 1,
};

#define S3_EXPORT_LAYOUT_REF_STR   "ref"
#define S3_EXPORT_LAYOUT_DENSE_STR "dense"

#define S3_EXPORT_ENDPOINT_MAX  256
#define S3_EXPORT_BUCKET_MAX    128
#define S3_EXPORT_PREFIX_MAX    128
#define S3_EXPORT_NAME_MAX      128
#define S3_EXPORT_KEY_MAX       512

/* Where manifests live, at the top of the bucket. Reserved as an lvstore name for
 * that reason. See s3_export_manifest_key(). */
#define S3_EXPORTS_DIR          "exports"

/* One chunk of a ref-layout export. This is the in-memory form; on the wire a
 * ref is 16 bytes of uuid and nothing else, with valid_bytes reconstructed from
 * the `full` bitmap plus a table of the exceptions. See s3_export.c. */
struct s3_export_ref {
	struct spdk_uuid uuid;

	/* How much of the chunk has ever been written. Reads past this are
	 * zero-filled rather than ranged against bytes that do not exist.
	 *
	 * A dense export needs no equivalent because it uploads whole chunks. A ref
	 * export cannot: it names the live object, and that object is exactly as
	 * long as the writes which produced it. Dropping this field would turn the
	 * tail of a partially written chunk into a failed range request -- or into
	 * whatever the object store decides to return for it. */
	uint32_t         valid_bytes;
};
/* Where the data came from. Everything here except bucket/prefix is advisory:
 * an importer is told the endpoint and region so it can build a client, and the
 * lvstore/snapshot names purely so a human can tell what an export is. No
 * credentials, ever -- the importer authenticates as itself . */
struct s3_export_source {
	char     endpoint[S3_EXPORT_ENDPOINT_MAX];
	char     region[S3_EXPORT_NAME_MAX];
	char     bucket[S3_EXPORT_BUCKET_MAX];
	char     prefix[S3_EXPORT_PREFIX_MAX];
	char     lvs_name[S3_EXPORT_NAME_MAX];
	char     snapshot[S3_EXPORT_NAME_MAX];
	uint64_t blob_id;
	/* The source snapshot's lvol uuid, which is what identifies it.
	 *
	 * blob_id is not an identity. Blobstore derives it from the lowest free
	 * metadata page (bs_page_to_blobid over find_first_clear(used_md_pages)),
	 * so deleting a snapshot and creating another frees the page and hands the
	 * same id straight back. A snapshot deleted and recreated under the same
	 * name is therefore liable to match on *both* name and blob_id while being
	 * an entirely different volume -- measured, not theorised: it is what
	 * run_selfimport_test.sh step [4] caught.
	 *
	 * Empty in manifests written before this field existed. A reader that needs
	 * to prove identity must treat empty as "cannot prove" rather than as a
	 * match; the JSON decoder marks it optional so those manifests still parse.
	 */
	char     snapshot_uuid[SPDK_UUID_STRING_LEN];
};

/* One prefix a ref manifest resolves chunks against. Version 3 and later.
 *
 * Deliberately not a whole struct s3_export_source: every entry shares the
 * endpoint, bucket and region of `src`, because the writer refuses to reference
 * another bucket and falls back to copying instead. That keeps a reader to one S3
 * client and keeps the pin enforceable -- a reference into a bucket this node may
 * not even be configured for could not be honoured. */
struct s3_export_src_entry {
	char prefix[S3_EXPORT_PREFIX_MAX];

	/* Whose lease governs these objects, so an importer knows what to renew:
	 * <prefix>/meta/exports/<uuid>.lease. Empty for entry 0, whose lease is this
	 * manifest's own.
	 *
	 * This is what keeps the delete path unchanged across a derivation. An
	 * importer of a derived export renews on the *original* export directly, so
	 * the original's source refuses to delete its snapshot whether or not the
	 * intermediate node is still running -- which is what publish-then-leave
	 * requires. */
	char export_uuid[SPDK_UUID_STRING_LEN];

	/* Identity of the snapshot behind that export. Present for the same reason
	 * s3_export_source::snapshot_uuid is: blob ids are handed back after a
	 * delete, so a snapshot deleted and recreated under one name can match on
	 * both name and blob_id while being an entirely different volume. Empty
	 * means "cannot prove", never "matches". */
	char snapshot_uuid[SPDK_UUID_STRING_LEN];
};

struct s3_export_manifest {
	uint32_t version;
	enum s3_export_layout layout;

	/* Bumped every time the manifest is rewritten in place, which is what
	 * materialising a ref export does. It is how an importer holding a stale
	 * copy can tell that refetching gave it something new -- see the 404
	 * handling in s3_export_bs_dev.c. */
	uint32_t generation;

	/* When the source stops honouring this export, in unix seconds; 0 means
	 * never. Not a courtesy: without it an importer that never arrives, or that
	 * died, pins a snapshot on the source forever, and the source cannot tell
	 * that case apart from a slow one. An importer renews while it still needs
	 * the export. */
	uint64_t expires_at;

	char     uuid_str[SPDK_UUID_STRING_LEN];
	uint64_t created_at;                /* unix seconds, advisory */

	struct s3_export_source src;

	/* Logical size of the snapshot. An importer must check this against
	 * *its own* cluster size, which is what spdk_lvol_create_esnap_clone()
	 * demands it be a multiple of. */
	uint64_t size_bytes;

	/* Source-side geometry. cluster_size is advisory (the importer's may
	 * differ); chunk_size is not -- it is how the export objects are cut,
	 * so reads areranged against it. */
	uint32_t cluster_size;
	uint32_t chunk_size;
	uint32_t block_size;

	uint64_t num_chunks;                    /* size_bytes / chunk_size */
	uint64_t present_chunks;                /* popcount(present) */

	/* One bit per chunk: set means "an object exists for this chunk".
	 * A clear bit reads as zeroes without any S3 request, which is what
	 * keeps sparse volumes sparse across an export. */
	uint8_t *present;

	/* Construction only, never serialized, not part of the crc.
	 *
	 * `present` cannot say "this chunk is already settled as zeroes": that
	 * is also how an untouched hole is spelled, and inherit() fills those
	 * from the parent. A local write_zeroes / unmap drops the mapping and
	 * leaves the cluster allocated, so the walk sees a hole and, without
	 * this bit, the parent object comes back -- stale data where the volume
	 * reads as zero.
	 *
	 * The walk sets this when is_zeroes() says the cluster holds no data.
	 * inherit() and later layers then skip it the same way they skip a
	 * present chunk. On the wire the chunk stays a hole, which is what the
	 * importer uses for zeroes. */
	uint8_t *resolved;

	/* REF layout only. One bit per chunk: set means "this chunk's object holds
	 * a whole chunk_size", i.e. valid_bytes needs no separate entry.
	 *
	 * This exists because valid_bytes is four bytes that are almost always the
	 * same four bytes. A chunk is partially written only at the tail of the
	 * writes that produced it, so on any volume that has been filled in the
	 * usual way the exceptions number in the handful while the rule applies to
	 * every chunk. Spending a bit on the rule and four bytes on each exception
	 * takes a quarter off the largest part of the manifest, and the manifest is
	 * what a handoff's latency is now made of.
	 *
	 * Bits are meaningful only where `present` is set; seal() clears the rest so
	 * that two manifests describing the same thing cannot differ in their crc. */
	uint8_t *full;

	/* REF layout only: num_chunks entries, meaningful exactly where the bitmap
	 * is set. NULL for a dense manifest, whose keys are derived from the chunk
	 * index and which therefore needs nothing here. */
	struct s3_export_ref *refs;

	/* Which prefix each chunk's object lives under. REF layout only.
	 *
	 * A snapshot taken on an imported volume owns some of its chunks and reads
	 * the rest through the export it came from, so one prefix cannot describe
	 * it. srcs[0] is always this export's own -- src.prefix -- which is what
	 * lets the read path treat a single-source manifest as the ordinary case of
	 * a multi-source one rather than a separate shape.
	 *
	 * A version 2 manifest has neither of these on the wire; parse synthesises
	 * the one-entry table from src so that everything downstream sees one shape.
	 * num_srcs is therefore >= 1 for any REF manifest, and 0 for a dense one. */
	struct s3_export_src_entry *srcs;
	uint32_t                    num_srcs;

	/* num_chunks entries, meaningful exactly where `present` is set: an index
	 * into srcs[]. Kept separate from refs[] rather than added to
	 * struct s3_export_ref so that a ref stays 16 bytes on the wire and v2's
	 * packing is untouched. NULL means every chunk resolves to srcs[0], which is
	 * how a v2 manifest arrives. */
	uint8_t *src_idx;

	/* Over the bitmap, and for a ref manifest over the full bitmap, the packed
	 * uuids and the partial-length exceptions too, in that order -- and, from
	 * version 3, src_idx after those. The stage list follows `version` rather
	 * than S3_EXPORT_VERSION: recomputing a v2 manifest's crc with the v3 stages
	 * would report corruption on every manifest already in a bucket. */
	uint32_t crc32c;

	/* Manifests are shared: the import cache holds one reference, and every
	 * s3_export_bs_dev built from it holds another. The bs_dev outlives the
	 * import RPC and may outlive a release, so this cannot be an owner
	 * pointer. Touched from nvmf I/O threads as well as the swap thread, so
	 * ref/unref are atomic. */
	uint32_t refcnt;
};

/* ==========================================================================
 * Manifest construction and access
 * ========================================================================== */

/**
 * Build an empty manifest (no chunks present) for a snapshot of \c size_bytes.
 *
 * \c size_bytes must be a whole number of \c chunk_size, and \c chunk_size a
 * power of two no smaller than S3LVOL_BLOCK_SIZE -- the same constraint the
 * chunk map imposes, for the same reason: index arithmetic is a shift.
 *
 * \c layout decides whether set_present() or set_ref() applies afterwards. It is
 * an argument rather than something set later, because it changes what the rest
 * of the manifest means.
 *
 * Returned with refcnt 1.
 */
int s3_export_manifest_create(const char *uuid_str, uint64_t size_bytes,
			      uint32_t chunk_size, enum s3_export_layout layout,
			      struct s3_export_manifest **out);

void s3_export_manifest_ref(struct s3_export_manifest *m);

/**
 * Drop a reference; frees at zero. NULL is accepted.
 */
void s3_export_manifest_unref(struct s3_export_manifest *m);

void s3_export_manifest_set_present(struct s3_export_manifest *m, uint64_t chunk_index);

/**
 * Claim a chunk as locally settled zeroes without marking it present.
 *
 * Used by the zero-copy walk so inherit() does not restore a parent object over
 * a write_zeroes / unmap. Does not belong on the wire: a hole already reads as
 * zeroes for the importer.
 */
void s3_export_manifest_set_resolved(struct s3_export_manifest *m, uint64_t chunk_index);

/**
 * Record which object a chunk of a ref export lives in, and mark it present.
 *
 * \return -EINVAL on a dense manifest or an index out of range. Refused rather
 * than ignored: a silently dropped ref is a chunk that reads as zeroes.
 */
int s3_export_manifest_set_ref(struct s3_export_manifest *m, uint64_t chunk_index,
			       const struct spdk_uuid *uuid, uint32_t valid_bytes);

/**
 * Add a prefix this manifest may resolve chunks against, or find the existing
 * entry for it, and answer its index.
 *
 * Idempotent on \p prefix: a derived export names the same parent prefix for
 * however many chunks it inherited, so the caller can ask per chunk without
 * having to keep track. The identity fields are taken from the first call that
 * supplies them and are not overwritten afterwards, so a later call may pass NULL.
 *
 * \param export_uuid whose lease governs those objects, so an importer knows what
 *                    to renew. NULL or empty for a prefix that needs no lease.
 * \param snapshot_uuid identity of the snapshot behind it; NULL if unknown.
 * \param out_idx receives the index to store in the chunk's slot.
 *
 * \return 0 on success, -E2BIG once S3_EXPORT_MAX_SOURCES prefixes are named --
 * which the caller is expected to treat as "export by copying instead", the same
 * as any other reason a reference cannot be expressed.
 */
int s3_export_manifest_add_src(struct s3_export_manifest *m, const char *prefix,
			       const char *export_uuid, const char *snapshot_uuid,
			       uint8_t *out_idx);

/**
 * Record that a chunk's object lives under source \p src_idx rather than under
 * this export's own prefix. Marks the chunk present, like set_ref.
 *
 * Kept separate from set_ref so that the common single-source writer needs no
 * per-chunk source argument at all, and so that a chunk cannot be given a source
 * without a ref: this refuses an index no add_src() has handed out.
 */
int s3_export_manifest_set_chunk_src(struct s3_export_manifest *m,
				     uint64_t chunk_index, uint8_t src_idx);

/**
 * Whether \p parent can be referenced by an export published to \p dst, or whether
 * the caller has to copy instead.
 *
 * Separate from the walk because it is a question about two manifests and nothing
 * else -- no blob, no device -- which is also what makes the degradation decision
 * testable on its own. The walk asks it before doing any work.
 *
 * \return 0 when a reference can be expressed; -ENOTSUP when it cannot, which is a
 * routing answer rather than a failure: the copy engine reads through blobstore
 * and expresses any history under one prefix. -EINVAL on a NULL argument.
 */
int s3_export_manifest_inheritable(const struct s3_export_manifest *parent,
				   const struct s3_export_source *dst,
				   const char *uuid_str);

/**
 * Fill \p m 's absent chunks from \p parent, recording each against the prefix that
 * actually holds it.
 *
 * This is how a snapshot taken on an imported volume is handed on without copying:
 * the clusters written since the import are in the local chunk map and the caller's
 * walk finds them, while everything older belongs to the export the volume reads
 * through and lives under its prefix.
 *
 * **Call after everything local has been recorded.** A chunk already present, or
 * one the walk marked resolved as local zeroes, is skipped, never overwritten.
 * That is what makes a rewrite since the import, or a local write_zeroes, win
 * over the imported object. Called too early, it would serve the data as it was
 * before the import -- correct-looking and wrong.
 *
 * Flattening, not chaining: the prefix is taken from \p parent per chunk, so a
 * parent that was itself derived contributes its grandparent's prefix for the
 * chunks it inherited. A manifest therefore never refers to another manifest,
 * resolution stays one hop however many times a volume has been handed on, and no
 * intermediate node has to still exist. For the same reason the lease identity is
 * carried across rather than replaced: an importer renews against the node that
 * holds the objects, not against the middleman.
 *
 * Both manifests must be ref layout and share a chunk size, since a chunk index
 * means a different byte range under a different one. Only the chunks the two have
 * in common are considered -- a volume that grew since the import has chunks the
 * parent never described.
 *
 * \return 0, with \p out_named and \p out_bytes set when non-NULL; -E2BIG once
 * more distinct prefixes are needed than a manifest can name, which the caller is
 * expected to treat as "export by copying instead"; -EINVAL on a mismatch of
 * layout or chunk size.
 */
int s3_export_manifest_inherit(struct s3_export_manifest *m,
			       const struct s3_export_manifest *parent,
			       uint64_t *out_named, uint64_t *out_bytes);

/** Which prefix a chunk resolves against; "" if the manifest cannot say. */
const char *s3_export_manifest_chunk_prefix(const struct s3_export_manifest *m,
					    uint64_t chunk_index);

/**
 * The ref for one chunk, or NULL if this is not a ref manifest, the index is out
 * of range, or the chunk is a hole.
 */
const struct s3_export_ref *s3_export_manifest_get_ref(
	const struct s3_export_manifest *m, uint64_t chunk_index);

bool s3_export_manifest_is_present(const struct s3_export_manifest *m,
				   uint64_t chunk_index);

bool s3_export_manifest_is_resolved(const struct s3_export_manifest *m,
				    uint64_t chunk_index);

/**
 * True when no chunk in the range has an object, i.e. the whole range reads as
 * zeroes. Used for the bs_dev's is_zeroes and to skip work.
 */
bool s3_export_manifest_range_is_zeroes(const struct s3_export_manifest *m,
					uint64_t chunk_index, uint64_t num_chunks);

/**
 * Recompute present_chunks and crc32c. Must be called before serializing;
 * serializing an unsealed manifest would record a checksum of stale data.
 */
void s3_export_manifest_seal(struct s3_export_manifest *m);

/* ==========================================================================
 * Manifest serialization
 * ========================================================================== */

/**
 * Render the manifest as JSON. The caller frees \c *out with free().
 *
 * JSON rather than msgpack: SPDK already carries a writer and a parser, and
 * one dependency avoided is worth more than the bytes saved on an object that
 * is read once per import.
 */
int s3_export_manifest_serialize(struct s3_export_manifest *m,
				 char **out, size_t *out_len);

/**
 * Parse a manifest, validating it.
 *
 * \c json need not be NUL terminated and is *not* modified.
 *
 * Rejects: a version or layout it does not know, a size that is not a whole
 * number of chunks, a block size other than 4 KiB, a bitmap of the wrong
 * length, a crc mismatch, and a present_chunks that disagrees with the bitmap.
 * The last two are the only defence against a manifest that parses but means
 * something else -- there is no ETag check here because a completed PUT cannot
 * be short, and a truncated body fails to parse.
 */
int s3_export_manifest_parse(const void *json, size_t len,
			     struct s3_export_manifest **out);

/* ==========================================================================
 * Key layout
 * ========================================================================== */

/**
 * Key of an export's manifest: `exports/<uuid>.json`, at the top of the bucket
 * rather than under the exporting lvstore's prefix.
 *
 * That is what makes the uuid a complete address. The manifest carries its own
 * source (bucket, prefix, endpoint), so everything else an importer needs follows
 * from finding it -- but only if finding it does not already require knowing the
 * exporting lvstore's name. Under `<lvs>/exports/` it did, and the importer has no
 * way to know that name: it is the *other* machine's local naming.
 *
 * The chunk objects stay under the source's prefix. They are the source's data and
 * a zero-copy export does not write any; where they live comes from the manifest.
 *
 * Consequence: `exports/` is a reserved top-level name, checked when an lvstore is
 * created.
 */
void s3_export_manifest_key(const char *uuid_str, char *out, size_t out_len);

/**
 * Key of one chunk object.
 *
 * Deterministic on purpose (index, not a fresh uuid): a retried export
 * rewrites the same keys with the same bytes, and an export that died halfway
 * leaves a prefix whose manifest is missing, which GC can drop wholesale.
 */
void s3_export_chunk_key(const char *prefix, const char *uuid_str,
			 uint64_t chunk_index, char *out, size_t out_len);

/**
 * Prefix shared by all of one export's chunk objects, i.e. what has to be
 * deleted to release it.
 */
void s3_export_chunk_prefix(const char *prefix, const char *uuid_str,
			    char *out, size_t out_len);

/* ==========================================================================
 * A side: run an export
 * ========================================================================== */

struct s3_export_opts {
	struct s3_client       *client;
	const char             *prefix;      /* destination prefix, i.e. lvs_name */
	const char             *uuid_str;    /* export uuid, already generated */

	/* The snapshot to copy. Must be read-only: nothing here stops a writer,
	 * and a concurrent write would make the export a torn mixture of two
	 * points in time. */
	struct spdk_blob       *blob;
	struct spdk_io_channel *channel;

	uint32_t                chunk_size;
	uint32_t                cluster_size;

	/* Chunks in flight. Each costs one chunk_size buffer plus at most one S3
	 * request, and the whole point of having more than one is that an export
	 * is otherwise a serial chain of round trips. 0 takes the default. */
	uint32_t                max_inflight;

	/* Which version of this export's manifest this run publishes. 0 for a new
	 * export, which is every ordinary one.
	 *
	 * Non-zero only when *replacing* a manifest that importers may already
	 * hold: materialising a reference export rewrites it in place, and the
	 * higher generation is the only way a reader can tell the manifest it just
	 * refetched from the one that sent it looking. Publishing a replacement at
	 * the same generation would have that reader conclude its objects were
	 * deleted rather than copied. */
	uint32_t                generation;

	struct s3_export_source src;
};

#define S3_EXPORT_DEFAULT_INFLIGHT 16

/**
 * \param m  the manifest that was uploaded, with one reference handed to the
 *           callback (unref it), or NULL on failure.
 */
typedef void (*s3_export_cb)(void *cb_arg, struct s3_export_manifest *m, int status);

/**
 * Seal a manifest and upload it.
 *
 * The last step of every path that produces one -- a ref export, a dense export,
 * and materialising the first into the second -- because "the manifest exists" is
 * the only signal an importer gets and it must mean that everything the manifest
 * claims is already readable. Keeping that in one function is what keeps the
 * ordering from having to be remembered three times.
 *
 * Takes a reference on \c m and hands it to the callback on success; on failure
 * the callback receives NULL and the reference is dropped here.
 */
int s3_export_manifest_publish(struct s3_client *client,
			       struct s3_export_manifest *m,
			       s3_export_cb cb, void *cb_arg);

/**
 * Copy the snapshot into export objects and, once every one of them is durable,
 * upload the manifest.
 *
 * That order is the whole contract: the manifest existing means every chunk it
 * claims is readable. An importer therefore never has to wonder whether the
 * source finished. If this fails, the manifest is absent and whatever chunks
 * did land are garbage that GC removes with the rest of the prefix.
 *
 * Chunks that read as all zeroes are not uploaded, so a thin volume stays thin.
 * Chunks that are uploaded are uploaded *whole* -- the reader zero-fills beyond
 * the end of the snapshot, so a fixed object size removes any need for the
 * valid_bytes bookkeeping the live chunk map has to do.
 *
 * Runs on the calling thread and must be given a channel for that thread.
 */
int s3_export_run(const struct s3_export_opts *opts, s3_export_cb cb, void *cb_arg);

/* ==========================================================================
 * A side: run a zero-copy export
 * ========================================================================== */

struct s3_export_ref_opts {
	/* The source lvstore's device. Two things come from it: the chunk map, which
	 * is where an LBA turns into an object uuid, and the chunk geometry. */
	struct spdk_bs_dev *bs_dev;

	/* The snapshot's clone chain, nearest first: chain[0] is the snapshot being
	 * exported, chain[1] its parent, and so on to the blob that has none.
	 *
	 * A chain rather than a single blob because a snapshot of a volume that has
	 * been snapshotted before owns only the clusters written since the previous
	 * one -- blobstore hands the cluster map to the new snapshot and leaves the
	 * older data where it is. Walking chain[0] alone would leave every inherited
	 * chunk out of the manifest, and a chunk left out reads as zeroes on the
	 * importing node. Since `lvol -> snap1 -> snap2` is the normal way to use
	 * this, that is the common case rather than a corner.
	 *
	 * All of it resolves through one chunk map: every layer's clusters live in
	 * the same bs_dev address space, so a parent's object is not "somebody
	 * else's" -- it sits in the same <prefix>/data/ as the rest.
	 *
	 * The caller assembles this because it is the layer that can: the lvol store
	 * already holds an open blob for every lvol, so walking the chain costs a
	 * lookup rather than an open, and this function stays synchronous.
	 *
	 * Order is load bearing. Nearest first is what makes "first layer to claim a
	 * chunk wins" resolve to the same cluster blobstore would reach by reading
	 * through the chain; reversed, an ancestor's stale data would overwrite what
	 * a descendant rewrote.
	 */
	struct spdk_blob  **chain;
	uint32_t            chain_len;

	struct s3_client   *client;
	const char         *prefix;
	const char         *uuid_str;

	uint32_t        cluster_size;
	uint64_t    expires_at;

	/* Version to publish. 0 for a new export; old+1 when an already-published
	 * reference export is rewritten after its snapshot becomes fully local.
	 * Readers use the increase to distinguish the replacement manifest from
	 * the stale one that sent them to an object which has since disappeared. */
	uint32_t        generation;

	struct s3_export_source src;

	/* The manifest of the export that the last layer of the chain reads through,
	 * when that layer is an esnap clone. NULL for every export that is not
	 * derived from an import, which is the ordinary case.
	 *
	 * This is what lets a snapshot taken on an imported volume be handed on
	 * without copying. The clusters written since the import are in this
	 * lvstore's chunk map and the walk finds them; everything older belongs to
	 * the export the clone reads through, lives under *its* prefix, and cannot
	 * be resolved by this chunk map at all. Naming those chunks out of this
	 * manifest is the alternative to reading and re-uploading them.
	 *
	 * Flattened, not chained: each inherited chunk is recorded against the
	 * prefix that actually holds it, which for a parent that was itself derived
	 * means the grandparent's prefix rather than the parent's. So a manifest
	 * never refers to another manifest, resolution stays one hop no matter how
	 * many times a volume has been handed on, and no node in the history has to
	 * still be alive.
	 *
	 * The caller must have checked that this export is reachable the same way
	 * `src` is -- same endpoint, bucket and region -- because a source entry
	 * carries only a prefix and shares the rest with `src`. It must also be a
	 * ref export of the same chunk size. Where any of that does not hold there
	 * is nothing to express and the caller uses the copying engine. */
	const struct s3_export_manifest *parent;
};

/**
 * Describe a snapshot by naming the objects it already occupies.
 *
 * No data is read and none is written except the manifest itself: the walk asks
 * blobstore which clusters each layer of the chain has allocated, turns each into
 * a device LBA, and looks that up in the chunk map. All of it is memory, so the
 * cost of an export becomes one PUT.
 *
 * Layers are visited nearest first and a chunk already named is never revisited,
 * so each chunk resolves to the cluster blobstore itself would reach by reading
 * through the chain. See the note on `chain` above -- getting that order wrong
 * silently serves stale data.
 *
 * **The caller must have drained first.** A cluster that blobstore has allocated
 * but whose data is still in the WAL or the overlay has no committed mapping yet,
 * and this fails rather than leaving that chunk out -- omitting it would produce
 * an importer that reads zeroes where there is data, with nothing to indicate it.
 *
 * Requires chunk_size == cluster_size, so that one blob cluster is exactly one
 * chunk map entry. Callers that cannot satisfy that use s3_export_run() instead,
 * which references nothing and therefore does not care. -ENOTSUP is a routing
 * decision, not a failure: the caller is expected to try the copying engine.
 *
 * A chain whose last layer is an esnap clone needs `parent` set to the manifest
 * that layer reads through. The data it inherits lives under another lvstore's
 * prefix, so this chunk map cannot resolve it, and without the parent manifest the
 * result would be short exactly where that parent held data -- reading as zeroes
 * on the importing node, with nothing to say so. Passing an esnap chain with no
 * parent is refused rather than silently truncated.
 */
int s3_export_run_ref(const struct s3_export_ref_opts *opts,
		      s3_export_cb cb, void *cb_arg);

/* ==========================================================================
 * B side: the read-only bs_dev over an export 
 * ========================================================================== */

/**
 * Wrap a manifest as a read-only spdk_bs_dev, suitable as an esnap parent.
 *
 * Implements read / readv / readv_ext / is_zeroes / is_range_valid and the
 * channel pair; every write-side entry point reports -EROFS. blobstore does not
 * write to a back device, so those exist to turn a bug into an error instead of
 * a corruption.
 *
 * The two arguments are held differently, which is easy to get wrong in both
 * directions:
 *
 *   \c m is *referenced* -- the caller keeps its own reference and must still
 *   release it.
 *
 *   \c client is *consumed* -- the caller's reference moves in here, and
 *   destroy() releases it. Putting it as well is a double free. On failure the
 *   move does not happen and the caller still owns it.
 *
 * Note that destroy() is called by blobstore, at a time the importer does not
 * control -- which is why neither may be owned by the import request.
 *
 * \c client must address the *source* bucket. Reads are plain ranged GETs, so
 * nothing else about the source lvstore has to be reachable.
 */
int s3_export_bs_dev_create(struct s3_client *client, struct s3_export_manifest *m,
			    struct spdk_bs_dev **out);

/** Called once the swap is complete and the old manifest has been released. */
typedef void (*s3_export_bs_dev_swap_cb)(void *cb_arg, int status);

/**
 * Called on the swapping thread immediately after the new manifest is published,
 * before the grace period that releases the previous one.
 *
 * The import registry uses this so derive/decouple and a later attach see the
 * same generation the data plane is already reading.
 */
typedef void (*s3_export_bs_dev_on_swap_fn)(void *arg, struct s3_export_manifest *m);

void s3_export_bs_dev_set_on_swap(struct spdk_bs_dev *bs_dev,
				  s3_export_bs_dev_on_swap_fn fn, void *arg);

/**
 * Point this device at a newer manifest for the same export.
 *
 * A ref export names the source's live objects, so when the source materialises
 * it -- rewrites the manifest as dense, holding its own copies -- those objects
 * go and an importer still on the old manifest reads 404s. Refetching and calling
 * this is the way out, and `generation` is how the two are told apart.
 *
 * **Asynchronous, and the reason is not I/O.** Every other thread reads the
 * manifest with no serialisation, which is what keeps this device lock free, so
 * the old one cannot be released until each of them has been through its event
 * loop once. The pointer is swapped before returning -- reads issued after this
 * call resolve against \p m -- but the callback is what says the previous
 * manifest is gone.
 *
 * Refuses rather than adopts a manifest that is not a strictly newer version of
 * the same thing:
 *
 * \return 0 and the callback runs; -EINVAL for a different export uuid, a
 * changed size, or a changed chunk size, any of which would have the clone
 * reading something blobstore did not size it for; -EALREADY when \p m is no
 * newer. That is not a failure:
 * it is both "the source has not rewritten it yet" and "this generation is
 * already installed" (another refetch swapped while a GET still used the old
 * keys). A 404-driven refetch must retry waiters on -EALREADY, not treat it as
 * the source having deleted the snapshot; see
 * s3_export_bs_dev_refetch_already_current().
 */
int s3_export_bs_dev_swap_manifest(struct spdk_bs_dev *bs_dev,
				   struct s3_export_manifest *m,
				   s3_export_bs_dev_swap_cb cb, void *cb_arg);

/**
 * True when swap_manifest() returned -EALREADY: the device is already on the
 * generation that was just fetched.
 *
 * A refetch started from a 404 must then retry waiters against the current
 * keys. A GET issued before a swap can 404 after it; the follow-up refetch
 * sees -EALREADY even though those keys now exist under the new manifest.
 * If the objects are genuinely gone, the retry 404s once and the I/O's
 * retried flag stops another refetch.
 */
static inline bool
s3_export_bs_dev_refetch_already_current(int swap_rc)
{
	return swap_rc == -EALREADY;
}

#endif /* S3LVOL_EXPORT_H */
