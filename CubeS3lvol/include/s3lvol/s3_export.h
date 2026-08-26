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
 *    which reads back as valid data from the wrong place. Hence a bump. */
#define S3_EXPORT_VERSION       2

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

	/* Over the bitmap, and for a ref manifest over the full bitmap, the packed
	 * uuids and the partial-length exceptions too, in that order. */
	uint32_t crc32c;

	/* Manifests are shared: the import cache holds one reference, and every
	 * s3_export_bs_dev built from it holds another. The bs_dev outlives the
	 * import RPC and may outlive a release, so this cannot be an owner
	 * pointer. */
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
 * Record which object a chunk of a ref export lives in, and mark it present.
 *
 * \return -EINVAL on a dense manifest or an index out of range. Refused rather
 * than ignored: a silently dropped ref is a chunk that reads as zeroes.
 */
int s3_export_manifest_set_ref(struct s3_export_manifest *m, uint64_t chunk_index,
			       const struct spdk_uuid *uuid, uint32_t valid_bytes);

/**
 * The ref for one chunk, or NULL if this is not a ref manifest, the index is out
 * of range, or the chunk is a hole.
 */
const struct s3_export_ref *s3_export_manifest_get_ref(
	const struct s3_export_manifest *m, uint64_t chunk_index);

bool s3_export_manifest_is_present(const struct s3_export_manifest *m,
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

	struct s3_export_source src;
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
 * A chain whose last layer is an esnap clone must not be passed: the data it
 * inherits lives under another lvstore's prefix, so this chunk map cannot resolve
 * it and the manifest would be short exactly where that parent held data. The
 * caller detects that while assembling the chain and routes to the copy engine,
 * which reads through blobstore and flattens everything.
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
 * Takes a reference on \c m and on \c client, releasing both from destroy().
 * Note that destroy() is called by blobstore, at a time the importer does not
 * control -- which is why neither may be owned by the import request.
 *
 * \c client must address the *source* bucket. Reads are plain ranged GETs, so
 * nothing else about the source lvstore has to be reachable.
 */
int s3_export_bs_dev_create(struct s3_client *client, struct s3_export_manifest *m,
			    struct spdk_bs_dev **out);

#endif /* S3LVOL_EXPORT_H */
