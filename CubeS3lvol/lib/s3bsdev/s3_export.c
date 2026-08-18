/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Export a snapshot to S3
 *
 *   Two halves, with nothing shared but the manifest:
 *
 *     1. the manifest: build, seal, serialize, parse, and the key layout
 *     2. the copy engine: read the snapshot through blobstore, upload the
 *        chunks that are not all zeroes, then upload the manifest
 *
 *   The reader lives in s3_export_bs_dev.c and only ever needs half 1.
 *
 *   === Why the manifest goes last ===
 *
 *   "The manifest exists" is the only signal an importer gets, and it has to
 *   mean "every chunk this claims is readable". So the manifest is uploaded
 *   after the last chunk PUT completes, and never before. A failed export
 *   therefore leaves no manifest at all -- the chunks that did land are
 *   unreferenced, and GC drops the prefix wholesale.
 *
 *   Note what this does *not* need: the source's chunk map, the journal, a
 *   checkpoint, or the WAL. Reads go through blobstore, so whatever combination
 *   of overlay, local log and S3 currently holds the data is its problem. The
 *   caller still has to drain before exporting, but only so that the snapshot's
 *   own metadata is durable -- not for the manifest to be valid.
 */

#include "spdk/stdinc.h"
#include "spdk/base64.h"
#include "spdk/blob.h"
#include "spdk/crc32.h"
#include "spdk/env.h"
#include "spdk/json.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/util.h"

#include "s3lvol/s3_export.h"

SPDK_LOG_REGISTER_COMPONENT(s3lvol_export)

/* ==========================================================================
 * Manifest
 * ========================================================================== */

static size_t
bitmap_bytes(uint64_t num_chunks)
{
	return (size_t)((num_chunks + 7) / 8);
}

/* Wire form of one ref: 16 bytes of uuid, and nothing else.
 *
 * valid_bytes used to live here, as four bytes that were almost always the same
 * four bytes -- a chunk is partial only at the tail of whatever wrote it. It now
 * travels as one bit per chunk in the `full` bitmap plus a four-byte entry for
 * each chunk that bit does not cover. On a filled volume that is a quarter off
 * the largest part of the manifest, and since a zero-copy handoff no longer moves
 * any data, the manifest is what its latency is made of.
 *
 * There is deliberately no reserved padding. Room for future per-chunk flags
 * belongs in another bitmap alongside `full`, which costs a bit rather than four
 * bytes and which an old reader refuses via the version rather than ignoring. */
#define S3_EXPORT_REF_WIRE_SIZE 16

/* One partial-length exception. */
#define S3_EXPORT_PARTIAL_WIRE_SIZE 4

static void
pack_ref(uint8_t *out, const struct s3_export_ref *ref)
{
	memcpy(out, &ref->uuid, sizeof(ref->uuid));
}

static void
unpack_ref(struct s3_export_ref *ref, const uint8_t *in)
{
	memcpy(&ref->uuid, in, sizeof(ref->uuid));
}

static void
pack_u32(uint8_t *out, uint32_t v)
{
	out[0] = (uint8_t)(v);
	out[1] = (uint8_t)(v >> 8);
	out[2] = (uint8_t)(v >> 16);
	out[3] = (uint8_t)(v >> 24);
}

static uint32_t
unpack_u32(const uint8_t *in)
{
	return (uint32_t)in[0] | ((uint32_t)in[1] << 8) |
	       ((uint32_t)in[2] << 16) | ((uint32_t)in[3] << 24);
}

static bool
bitmap_test(const uint8_t *bm, uint64_t index)
{
	return (bm[index / 8] & (1u << (index % 8))) != 0;
}

static void
bitmap_set(uint8_t *bm, uint64_t index)
{
	bm[index / 8] |= (uint8_t)(1u << (index % 8));
}

static void
bitmap_clear(uint8_t *bm, uint64_t index)
{
	bm[index / 8] &= (uint8_t)~(1u << (index % 8));
}

/* Chunks that are present but not full, i.e. how many exceptions the partials
 * table carries. Derived rather than stored, for the same reason the ref count
 * is: a stored count that disagrees with the bitmaps would be consumed in the
 * wrong order, and every entry after the discrepancy would truncate the wrong
 * chunk. */
static uint64_t
count_partials(const struct s3_export_manifest *m)
{
	uint64_t n = 0;
	uint64_t i;

	if (!m->full) {
		return 0;
	}
	for (i = 0; i < m->num_chunks; i++) {
		if (bitmap_test(m->present, i) && !bitmap_test(m->full, i)) {
			n++;
		}
	}
	return n;
}

static const char *
layout_to_str(enum s3_export_layout layout)
{
	return layout == S3_EXPORT_LAYOUT_REF ? S3_EXPORT_LAYOUT_REF_STR :
	       S3_EXPORT_LAYOUT_DENSE_STR;
}

int
s3_export_manifest_create(const char *uuid_str, uint64_t size_bytes,
			  uint32_t chunk_size, enum s3_export_layout layout,
			  struct s3_export_manifest **out)
{
	struct s3_export_manifest *m;

	if (!uuid_str || !out || size_bytes == 0 || chunk_size == 0) {
		return -EINVAL;
	}
	if (layout != S3_EXPORT_LAYOUT_REF && layout != S3_EXPORT_LAYOUT_DENSE) {
		return -EINVAL;
	}
	if ((chunk_size & (chunk_size - 1)) != 0 || chunk_size < S3LVOL_BLOCK_SIZE) {
		SPDK_ERRLOG("chunk_size %u must be a power of two and at least %u\n",
			    chunk_size, S3LVOL_BLOCK_SIZE);
		return -EINVAL;
	}
	/* Refused rather than rounded: a partial last chunk would either be an
	 * object shorter than every other one, or a read past the end of the
	 * snapshot. Both are avoidable, because chunk_size equals cluster_size in
	 * every configuration that exists and blob sizes are whole clusters. */
	if (size_bytes % chunk_size != 0) {
		SPDK_ERRLOG("size %" PRIu64 " is not a whole number of %u-byte chunks\n",
			    size_bytes, chunk_size);
		return -EINVAL;
	}

	m = calloc(1, sizeof(*m));
	if (!m) {
		return -ENOMEM;
	}

	m->version      = S3_EXPORT_VERSION;
	m->layout       = layout;
	m->size_bytes   = size_bytes;
	m->chunk_size   = chunk_size;
	m->block_size   = S3LVOL_BLOCK_SIZE;
	m->num_chunks   = size_bytes / chunk_size;
	m->refcnt       = 1;
	snprintf(m->uuid_str, sizeof(m->uuid_str), "%s", uuid_str);

	m->present = calloc(1, bitmap_bytes(m->num_chunks));
	if (!m->present) {
		free(m);
		return -ENOMEM;
	}

	if (layout == S3_EXPORT_LAYOUT_REF) {
		/* Indexed by chunk rather than packed by presence: the walk that fills
		 * this in visits chunks in whatever order blobstore reports them, and
		 * packing on the fly would make the writer responsible for maintaining
		 * an order the reader depends on. Packing happens once, at serialize. */
		m->refs = calloc(m->num_chunks, sizeof(*m->refs));
		if (!m->refs) {
			free(m->present);
			free(m);
			return -ENOMEM;
		}
		/* Allocated with the manifest rather than at seal time, because seal is
		 * called from paths that cannot report a failure. */
		m->full = calloc(1, bitmap_bytes(m->num_chunks));
		if (!m->full) {
			free(m->refs);
			free(m->present);
			free(m);
			return -ENOMEM;
		}
	}

	*out = m;
	return 0;
}

void
s3_export_manifest_ref(struct s3_export_manifest *m)
{
	if (m) {
		m->refcnt++;
	}
}

void
s3_export_manifest_unref(struct s3_export_manifest *m)
{
	if (!m) {
		return;
	}
	assert(m->refcnt > 0);
	if (--m->refcnt > 0) {
		return;
	}
	free(m->present);
	free(m->full);
	free(m->refs);
	free(m);
}

void
s3_export_manifest_set_present(struct s3_export_manifest *m, uint64_t chunk_index)
{
	if (!m || chunk_index >= m->num_chunks) {
		return;
	}
	m->present[chunk_index / 8] |= (uint8_t)(1u << (chunk_index % 8));
}

int
s3_export_manifest_set_ref(struct s3_export_manifest *m, uint64_t chunk_index,
			   const struct spdk_uuid *uuid, uint32_t valid_bytes)
{
	if (!m || !uuid || chunk_index >= m->num_chunks) {
		return -EINVAL;
	}
	if (m->layout != S3_EXPORT_LAYOUT_REF || !m->refs) {
		return -EINVAL;
	}
	if (valid_bytes == 0 || valid_bytes > m->chunk_size) {
		/* Zero would describe an object nobody can read anything out of, and
		 * more than a chunk would have the reader range past the object's end.
		 * Either means the caller's idea of the chunk map disagrees with this
		 * manifest's geometry, which is worth stopping for. */
		SPDK_ERRLOG("chunk %" PRIu64 " has valid_bytes %u, outside 1..%u\n",
			    chunk_index, valid_bytes, m->chunk_size);
		return -EINVAL;
	}

	m->refs[chunk_index].uuid = *uuid;
	m->refs[chunk_index].valid_bytes = valid_bytes;
	s3_export_manifest_set_present(m, chunk_index);
	return 0;
}

const struct s3_export_ref *
s3_export_manifest_get_ref(const struct s3_export_manifest *m, uint64_t chunk_index)
{
	if (!m || !m->refs || chunk_index >= m->num_chunks) {
		return NULL;
	}
	if (!s3_export_manifest_is_present(m, chunk_index)) {
		return NULL;
	}
	return &m->refs[chunk_index];
}

bool
s3_export_manifest_is_present(const struct s3_export_manifest *m, uint64_t chunk_index)
{
	if (!m || chunk_index >= m->num_chunks) {
		return false;
	}
	return (m->present[chunk_index / 8] & (1u << (chunk_index % 8))) != 0;
}

bool
s3_export_manifest_range_is_zeroes(const struct s3_export_manifest *m,
				   uint64_t chunk_index, uint64_t num_chunks)
{
	uint64_t i;

	for (i = 0; i < num_chunks; i++) {
		if (s3_export_manifest_is_present(m, chunk_index + i)) {
			return false;
		}
	}
	return true;
}

void
s3_export_manifest_seal(struct s3_export_manifest *m)
{
	size_t nbytes = bitmap_bytes(m->num_chunks);
	uint64_t count = 0;
	uint64_t i;
	size_t b;

	for (b = 0; b < nbytes; b++) {
		count += (uint64_t)__builtin_popcount(m->present[b]);
	}
	m->present_chunks = count;
	m->crc32c = spdk_crc32c_update(m->present, nbytes, 0);

	if (m->layout != S3_EXPORT_LAYOUT_REF || !m->refs || !m->full) {
		return;
	}

	/* The full bitmap is derived here rather than maintained by set_ref, so that
	 * it cannot drift from the valid_bytes it summarises. Bits outside the
	 * present set are cleared: they describe nothing, and leaving whatever was
	 * there would let two manifests that mean the same thing disagree on their
	 * crc -- which the importer would report as corruption. */
	for (i = 0; i < m->num_chunks; i++) {
		if (bitmap_test(m->present, i) &&
		    m->refs[i].valid_bytes == m->chunk_size) {
			bitmap_set(m->full, i);
		} else {
			bitmap_clear(m->full, i);
		}
	}
	m->crc32c = spdk_crc32c_update(m->full, nbytes, m->crc32c);

	/* The uuids and then the exceptions, each in exactly the order it is
	 * serialized in. Leaving any of it out would mean a corrupted uuid reads
	 * somebody else's object with no complaint anywhere, or a corrupted length
	 * silently truncates a chunk -- the bitmaps would still check out, so the
	 * manifest would look entirely valid.
	 *
	 * Fed one record at a time rather than packing into a buffer first, because
	 * sealing happens on paths that are not allowed to fail and a 16 MiB
	 * allocation for a 1 TiB export can. */
	for (i = 0; i < m->num_chunks; i++) {
		uint8_t wire[S3_EXPORT_REF_WIRE_SIZE];

		if (!bitmap_test(m->present, i)) {
			continue;
		}
		pack_ref(wire, &m->refs[i]);
		m->crc32c = spdk_crc32c_update(wire, sizeof(wire), m->crc32c);
	}

	for (i = 0; i < m->num_chunks; i++) {
		uint8_t wire[S3_EXPORT_PARTIAL_WIRE_SIZE];

		if (!bitmap_test(m->present, i) || bitmap_test(m->full, i)) {
			continue;
		}
		pack_u32(wire, m->refs[i].valid_bytes);
		m->crc32c = spdk_crc32c_update(wire, sizeof(wire), m->crc32c);
	}
}

/* ==========================================================================
 * Key layout
 * ========================================================================== */

void
s3_export_manifest_key(const char *uuid_str, char *out, size_t out_len)
{
	snprintf(out, out_len, "%s/%s.json", S3_EXPORTS_DIR, uuid_str);
}

void
s3_export_chunk_prefix(const char *prefix, const char *uuid_str,
		       char *out, size_t out_len)
{
	snprintf(out, out_len, "%s/exports/%s/", prefix, uuid_str);
}

void
s3_export_chunk_key(const char *prefix, const char *uuid_str,
		    uint64_t chunk_index, char *out, size_t out_len)
{
	/* Fixed width, so the listing order of a prefix is the chunk order.
	 * Nothing depends on that yet; it costs nothing and makes a bucket
	 * browser useful. */
	snprintf(out, out_len, "%s/exports/%s/%016" PRIx64, prefix, uuid_str,
		 chunk_index);
}

/* ==========================================================================
 * Serialization
 * ========================================================================== */

struct json_buf {
	char   *data;
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

/* Pack the refs of every present chunk, ascending. The reader walks the bitmap
 * and consumes these in the same order, so the count is implied rather than
 * repeated -- and cross-checked against present_chunks, which is not. */
static int
pack_all_refs(const struct s3_export_manifest *m, uint8_t **out, size_t *out_len)
{
	uint8_t *buf;
	size_t len;
	uint64_t i;
	size_t at = 0;

	len = (size_t)m->present_chunks * S3_EXPORT_REF_WIRE_SIZE;
	buf = malloc(len ? len : 1);
	if (!buf) {
		return -ENOMEM;
	}

	for (i = 0; i < m->num_chunks; i++) {
		if (!s3_export_manifest_is_present(m, i)) {
			continue;
		}
		pack_ref(buf + at, &m->refs[i]);
		at += S3_EXPORT_REF_WIRE_SIZE;
	}
	assert(at == len);

	*out = buf;
	*out_len = len;
	return 0;
}

/* The valid_bytes of every chunk that is present but not full, ascending. Empty
 * whenever the volume was written in whole chunks, which is the common case. */
static int
pack_all_partials(const struct s3_export_manifest *m, uint8_t **out, size_t *out_len)
{
	uint8_t *buf;
	size_t len;
	uint64_t i;
	size_t at = 0;

	len = (size_t)count_partials(m) * S3_EXPORT_PARTIAL_WIRE_SIZE;
	buf = malloc(len ? len : 1);
	if (!buf) {
		return -ENOMEM;
	}

	for (i = 0; i < m->num_chunks; i++) {
		if (!bitmap_test(m->present, i) || bitmap_test(m->full, i)) {
			continue;
		}
		pack_u32(buf + at, m->refs[i].valid_bytes);
		at += S3_EXPORT_PARTIAL_WIRE_SIZE;
	}
	assert(at == len);

	*out = buf;
	*out_len = len;
	return 0;
}

/* base64 of a byte range, or NULL on allocation failure. Zero length yields an
 * empty string, which is what an all-full manifest's partials table is. */
static char *
encode_b64(const void *data, size_t len)
{
	char *b64;

	b64 = calloc(1, spdk_base64_get_encoded_strlen(len) + 1);
	if (!b64) {
		return NULL;
	}
	if (len != 0 && spdk_base64_encode(b64, data, len) != 0) {
		free(b64);
		return NULL;
	}
	return b64;
}

int
s3_export_manifest_serialize(struct s3_export_manifest *m, char **out, size_t *out_len)
{
	struct json_buf buf = {0};
	struct spdk_json_write_ctx *w;
	char *b64 = NULL;
	char *full_b64 = NULL;
	char *refs_b64 = NULL;
	char *partials_b64 = NULL;
	uint8_t *packed = NULL;
	size_t packed_len = 0;
	size_t nbytes;
	int rc;

	if (!m || !out || !out_len) {
		return -EINVAL;
	}

	nbytes = bitmap_bytes(m->num_chunks);
	b64 = encode_b64(m->present, nbytes);
	if (!b64) {
		return -ENOMEM;
	}

	if (m->layout == S3_EXPORT_LAYOUT_REF) {
		full_b64 = encode_b64(m->full, nbytes);
		if (!full_b64) {
			rc = -ENOMEM;
			goto err;
		}

		rc = pack_all_refs(m, &packed, &packed_len);
		if (rc != 0) {
			goto err;
		}
		refs_b64 = encode_b64(packed, packed_len);
		free(packed);
		if (!refs_b64) {
			rc = -ENOMEM;
			goto err;
		}

		rc = pack_all_partials(m, &packed, &packed_len);
		if (rc != 0) {
			goto err;
		}
		partials_b64 = encode_b64(packed, packed_len);
		free(packed);
		if (!partials_b64) {
			rc = -ENOMEM;
			goto err;
		}
	}

	w = spdk_json_write_begin(json_buf_append, &buf, 0);
	if (!w) {
		rc = -ENOMEM;
		goto err;
	}

	spdk_json_write_object_begin(w);
	spdk_json_write_named_uint32(w, "version", m->version);
	spdk_json_write_named_string(w, "layout", layout_to_str(m->layout));
	spdk_json_write_named_string(w, "export_uuid", m->uuid_str);
	spdk_json_write_named_uint32(w, "generation", m->generation);
	spdk_json_write_named_uint64(w, "created_at", m->created_at);
	spdk_json_write_named_uint64(w, "expires_at", m->expires_at);

	spdk_json_write_named_object_begin(w, "source");
	spdk_json_write_named_string(w, "endpoint", m->src.endpoint);
	spdk_json_write_named_string(w, "region", m->src.region);
	spdk_json_write_named_string(w, "bucket", m->src.bucket);
	spdk_json_write_named_string(w, "prefix", m->src.prefix);
	spdk_json_write_named_string(w, "lvs_name", m->src.lvs_name);
	spdk_json_write_named_string(w, "snapshot", m->src.snapshot);
	spdk_json_write_named_uint64(w, "blob_id", m->src.blob_id);
	if (m->src.snapshot_uuid[0] != '\0') {
		spdk_json_write_named_string(w, "snapshot_uuid",
					     m->src.snapshot_uuid);
	}
	spdk_json_write_object_end(w);

	spdk_json_write_named_uint64(w, "size_bytes", m->size_bytes);
	spdk_json_write_named_uint32(w, "cluster_size", m->cluster_size);
	spdk_json_write_named_uint32(w, "chunk_size", m->chunk_size);
	spdk_json_write_named_uint32(w, "block_size", m->block_size);
	spdk_json_write_named_uint64(w, "num_chunks", m->num_chunks);
	spdk_json_write_named_uint64(w, "present_chunks", m->present_chunks);
	spdk_json_write_named_uint32(w, "crc32c", m->crc32c);
	spdk_json_write_named_string(w, "present", b64);
	if (refs_b64) {
		spdk_json_write_named_string(w, "full", full_b64);
		spdk_json_write_named_string(w, "refs", refs_b64);
		spdk_json_write_named_string(w, "partials", partials_b64);
	}
	spdk_json_write_object_end(w);

	rc = spdk_json_write_end(w);
	free(partials_b64);
	free(refs_b64);
	free(full_b64);
	free(b64);

	if (rc != 0 || buf.data == NULL) {
		free(buf.data);
		return rc != 0 ? rc : -ENOMEM;
	}

	*out = buf.data;
	*out_len = buf.len;
	return 0;
err:
	free(partials_b64);
	free(refs_b64);
	free(full_b64);
	free(b64);
	return rc;
}

/* ==========================================================================
 * Parsing
 * ========================================================================== */

struct source_json {
	char    *endpoint;
	char    *region;
	char    *bucket;
	char    *prefix;
	char    *lvs_name;
	char    *snapshot;
	uint64_t blob_id;
	char    *snapshot_uuid;
};

static const struct spdk_json_object_decoder source_decoders[] = {
	{"endpoint", offsetof(struct source_json, endpoint), spdk_json_decode_string, true},
	{"region",   offsetof(struct source_json, region),   spdk_json_decode_string, true},
	{"bucket",   offsetof(struct source_json, bucket),   spdk_json_decode_string, true},
	{"prefix",   offsetof(struct source_json, prefix),   spdk_json_decode_string, true},
	{"lvs_name", offsetof(struct source_json, lvs_name), spdk_json_decode_string, true},
	{"snapshot", offsetof(struct source_json, snapshot), spdk_json_decode_string, true},
	{"blob_id",  offsetof(struct source_json, blob_id),  spdk_json_decode_uint64, true},
	{"snapshot_uuid", offsetof(struct source_json, snapshot_uuid), spdk_json_decode_string, true},
};

struct manifest_json {
	uint32_t          version;
	char             *layout;
	char             *export_uuid;
	uint64_t          created_at;
	struct source_json src;
	uint64_t          size_bytes;
	uint32_t          cluster_size;
	uint32_t          chunk_size;
	uint32_t          block_size;
	uint64_t          num_chunks;
	uint64_t          present_chunks;
	uint32_t          crc32c;
	char             *present;
	char *refs;
	char             *full;
	char             *partials;
	uint32_t generation;
	uint64_t expires_at;
};

static int
decode_source(const struct spdk_json_val *val, void *out)
{
	return spdk_json_decode_object(val, source_decoders,
				       SPDK_COUNTOF(source_decoders), out);
}

static const struct spdk_json_object_decoder manifest_decoders[] = {
	{"version",        offsetof(struct manifest_json, version),        spdk_json_decode_uint32, false},
	{"layout",         offsetof(struct manifest_json, layout),         spdk_json_decode_string, false},
	{"export_uuid",    offsetof(struct manifest_json, export_uuid),    spdk_json_decode_string, false},
	{"created_at",     offsetof(struct manifest_json, created_at),     spdk_json_decode_uint64, true},
	{"source",         offsetof(struct manifest_json, src),            decode_source,           true},
	{"size_bytes",     offsetof(struct manifest_json, size_bytes),     spdk_json_decode_uint64, false},
	{"cluster_size",   offsetof(struct manifest_json, cluster_size),   spdk_json_decode_uint32, true},
	{"chunk_size",     offsetof(struct manifest_json, chunk_size),     spdk_json_decode_uint32, false},
	{"block_size",     offsetof(struct manifest_json, block_size),     spdk_json_decode_uint32, false},
	{"num_chunks",     offsetof(struct manifest_json, num_chunks),     spdk_json_decode_uint64, false},
	{"present_chunks", offsetof(struct manifest_json, present_chunks), spdk_json_decode_uint64, false},
	{"crc32c",         offsetof(struct manifest_json, crc32c),         spdk_json_decode_uint32, false},
	{"present",        offsetof(struct manifest_json, present),        spdk_json_decode_string, false},
	/* Optional here, required below once the layout is known: a dense manifest
	 * must not carry refs and a ref manifest cannot do without them, and a
	 * decoder table cannot express "depends on another member".
	 *
	 * `partials` is optional in the same sense but also legitimately empty --
	 * a volume written in whole chunks has no exceptions to record -- so its
	 * presence is what is checked, not its length. */
	{"refs", offsetof(struct manifest_json, refs), spdk_json_decode_string, true},
	{"full",     offsetof(struct manifest_json, full),     spdk_json_decode_string, true},
	{"partials", offsetof(struct manifest_json, partials), spdk_json_decode_string, true},
	/* Absent from the manifests written before ref layout existed, hence
	 * optional: those are generation 0 and never expire, which is what a
	 * zeroed struct already says. */
	{"generation", offsetof(struct manifest_json, generation), spdk_json_decode_uint32, true},
	{"expires_at", offsetof(struct manifest_json, expires_at), spdk_json_decode_uint64, true},
};

static void
free_manifest_json(struct manifest_json *j)
{
	free(j->layout);
	free(j->export_uuid);
	free(j->present);
	free(j->refs);
	free(j->full);
	free(j->partials);
	free(j->src.endpoint);
	free(j->src.region);
	free(j->src.bucket);
	free(j->src.prefix);
	free(j->src.lvs_name);
	free(j->src.snapshot);
	free(j->src.snapshot_uuid);
}

static int
layout_from_str(const char *s, enum s3_export_layout *out)
{
	if (strcmp(s, S3_EXPORT_LAYOUT_REF_STR) == 0) {
		*out = S3_EXPORT_LAYOUT_REF;
		return 0;
	}
	if (strcmp(s, S3_EXPORT_LAYOUT_DENSE_STR) == 0) {
		*out = S3_EXPORT_LAYOUT_DENSE;
		return 0;
	}
	return -ENOTSUP;
}

/* Decode base64 into an exactly-sized buffer.
 *
 * "Exactly" is the point. spdk_base64_get_decoded_len() rounds up to the next
 * three bytes, so a table one entry short still has room for the entry it is
 * missing; only comparing the decoded length against what the bitmaps imply
 * catches it. And a table of the wrong length is the interesting corruption --
 * consumed in the wrong order, every entry after the discrepancy belongs to
 * another chunk, and that reads back as perfectly valid data from the wrong
 * place. */
static int
decode_b64_exact(const char *b64, uint8_t **out, size_t expected, const char *what)
{
	uint8_t *buf;
	size_t room;
	int rc;

	*out = NULL;
	if (expected == 0) {
		return 0;
	}

	room = spdk_base64_get_decoded_len(strlen(b64));
	if (room < expected) {
		SPDK_ERRLOG("export manifest %s has room for %zu byte(s), needs %zu\n",
			    what, room, expected);
		return -EINVAL;
	}

	buf = malloc(room);
	if (!buf) {
		return -ENOMEM;
	}

	rc = spdk_base64_decode(buf, &room, b64);
	if (rc != 0 || room != expected) {
		SPDK_ERRLOG("export manifest %s does not decode (%d, %zu vs %zu)\n",
			    what, rc, room, expected);
		free(buf);
		return -EINVAL;
	}

	*out = buf;
	return 0;
}

/* Rebuild the ref table from the packed uuids, the full bitmap and the partial
 * lengths. The manifest's present bitmap must already be decoded, and
 * present_chunks must already reflect it: both tables are counted from it rather
 * than from anything the file claims. */
static int
unpack_all_refs(struct s3_export_manifest *m, const char *refs_b64,
		const char *full_b64, const char *partials_b64)
{
	uint8_t *refs = NULL;
	uint8_t *full = NULL;
	uint8_t *partials = NULL;
	size_t nbytes = bitmap_bytes(m->num_chunks);
	uint64_t num_partials = 0;
	uint64_t i;
	size_t at = 0;
	size_t part_at = 0;
	int rc;

	/* The full bitmap first: how long the partials table has to be is derived
	 * from it, so it has to be trustworthy before that length is computed. */
	rc = decode_b64_exact(full_b64, &full, nbytes, "full bitmap");
	if (rc != 0) {
		goto out;
	}
	memcpy(m->full, full, nbytes);

	for (i = 0; i < m->num_chunks; i++) {
		if (bitmap_test(m->present, i) && !bitmap_test(m->full, i)) {
			num_partials++;
		}
	}

	rc = decode_b64_exact(refs_b64, &refs,
			      (size_t)m->present_chunks * S3_EXPORT_REF_WIRE_SIZE,
			      "ref table");
	if (rc != 0) {
		goto out;
	}
	rc = decode_b64_exact(partials_b64, &partials,
			      (size_t)num_partials * S3_EXPORT_PARTIAL_WIRE_SIZE,
			      "partial length table");
	if (rc != 0) {
		goto out;
	}

	for (i = 0; i < m->num_chunks; i++) {
		if (!bitmap_test(m->present, i)) {
			continue;
		}
		unpack_ref(&m->refs[i], refs + at);
		at += S3_EXPORT_REF_WIRE_SIZE;

		if (bitmap_test(m->full, i)) {
			m->refs[i].valid_bytes = m->chunk_size;
			continue;
		}
		m->refs[i].valid_bytes = unpack_u32(partials + part_at);
		part_at += S3_EXPORT_PARTIAL_WIRE_SIZE;

		/* Checked here rather than trusted, because valid_bytes is what
		 * decides where the reader stops issuing range requests and starts
		 * zero-filling. A zero would name an object nothing can be read out
		 * of; chunk_size would contradict the bit that put it in this table
		 * at all, and seal() would then derive a different full bitmap and
		 * fail the crc for a reason that names nothing useful. */
		if (m->refs[i].valid_bytes == 0 ||
		    m->refs[i].valid_bytes >= m->chunk_size) {
			SPDK_ERRLOG("export manifest chunk %" PRIu64 " is listed as "
				    "partial with valid_bytes %u, outside 1..%u\n",
				    i, m->refs[i].valid_bytes, m->chunk_size - 1);
			rc = -EINVAL;
			goto out;
		}
	}
out:
	free(partials);
	free(refs);
	free(full);
	return rc;
}

static void
copy_field(char *dst, size_t dst_len, const char *src)
{
	snprintf(dst, dst_len, "%s", src ? src : "");
}

int
s3_export_manifest_parse(const void *json, size_t len, struct s3_export_manifest **out)
{
	struct manifest_json j = {0};
	enum s3_export_layout layout;
	struct spdk_json_val *values = NULL;
	struct s3_export_manifest *m = NULL;
	uint8_t *decoded = NULL;
	char *copy = NULL;
	size_t nbytes;
	ssize_t num_values;
	int rc;

	if (!json || len == 0 || !out) {
		return -EINVAL;
	}

	/* spdk_json_parse() unescapes strings in place, so it gets a copy of its
	 * own: the caller's buffer is very likely the S3 response body, which it
	 * may well want to log after this fails. */
	copy = malloc(len);
	if (!copy) {
		return -ENOMEM;
	}
	memcpy(copy, json, len);

	/* Counted without DECODE_IN_PLACE. The flag makes spdk_json_parse() unescape
	 * in place regardless of whether it was given anywhere to store values, so
	 * counting with it set rewrites the buffer and the second pass parses a
	 * document that is no longer there. A manifest happens to contain nothing
	 * escaped -- base64 and plain strings only -- so this was harmless here, but
	 * the imports registry embeds whole manifests as JSON strings and was not. */
	num_values = spdk_json_parse(copy, len, NULL, 0, NULL, 0);
	if (num_values <= 0) {
		SPDK_ERRLOG("export manifest is not valid JSON (%zd)\n", num_values);
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
		SPDK_ERRLOG("export manifest did not parse on the second pass (%zd)\n",
			    num_values);
		rc = -EINVAL;
		goto out;
	}

	if (spdk_json_decode_object(values, manifest_decoders,
				    SPDK_COUNTOF(manifest_decoders), &j) != 0) {
		SPDK_ERRLOG("export manifest is missing required members\n");
		rc = -EINVAL;
		goto out;
	}

	/* Refusing an unknown version or layout is the point of having them: a
	 * future zero-copy layout would name the source's own chunk objects, and
	 * reading it as if it were dense would produce an lvol full of the wrong
	 * data rather than an error. */
	if (j.version != S3_EXPORT_VERSION) {
		SPDK_ERRLOG("export manifest version %u is not supported (want %u)\n",
			    j.version, S3_EXPORT_VERSION);
		rc = -ENOTSUP;
		goto out;
	}
	rc = layout_from_str(j.layout, &layout);
	if (rc != 0) {
		SPDK_ERRLOG("export manifest layout '%s' is not supported\n", j.layout);
		goto out;
	}
	if ((layout == S3_EXPORT_LAYOUT_REF) != (j.refs != NULL)) {
		/* A ref manifest without refs would read as all holes -- every chunk
		 * silently zero. A dense one carrying refs means whoever wrote it had two
		 * ideas about what the file is, and there is no way to tell which of them
		 * the data follows. */
		SPDK_ERRLOG("export manifest is '%s' but %s a ref table\n", j.layout,
			    j.refs ? "carries" : "has no");
		rc = -EINVAL;
		goto out;
	}
	if ((layout == S3_EXPORT_LAYOUT_REF) &&
	    (j.full == NULL || j.partials == NULL)) {
		/* Both are what turn the packed uuids back into lengths. Without the
		 * full bitmap every chunk's valid_bytes is unknown, and guessing
		 * chunk_size would range past the end of every partially written
		 * object. */
		SPDK_ERRLOG("export manifest is 'ref' but has no %s\n",
			    j.full == NULL ? "full bitmap" : "partial length table");
		rc = -EINVAL;
		goto out;
	}
	if ((layout != S3_EXPORT_LAYOUT_REF) &&
	    (j.full != NULL || j.partials != NULL)) {
		SPDK_ERRLOG("export manifest is '%s' but carries a %s\n", j.layout,
			    j.full != NULL ? "full bitmap" : "partial length table");
		rc = -EINVAL;
		goto out;
	}
	if (j.block_size != S3LVOL_BLOCK_SIZE) {
		SPDK_ERRLOG("export manifest block size %u != %u\n",
			    j.block_size, S3LVOL_BLOCK_SIZE);
		rc = -EINVAL;
		goto out;
	}

	rc = s3_export_manifest_create(j.export_uuid, j.size_bytes, j.chunk_size,
				 layout, &m);
	if (rc != 0) {
		goto out;
	}
	if (m->num_chunks != j.num_chunks) {
		SPDK_ERRLOG("export manifest claims %" PRIu64 " chunks, geometry says "
			    "%" PRIu64 "\n", j.num_chunks, m->num_chunks);
		rc = -EINVAL;
		goto out;
	}

	nbytes = bitmap_bytes(m->num_chunks);
	rc = decode_b64_exact(j.present, &decoded, nbytes, "present bitmap");
	if (rc != 0) {
		goto out;
	}
	memcpy(m->present, decoded, nbytes);

	if (layout == S3_EXPORT_LAYOUT_REF) {
		/* After the bitmap and before the final seal: how many refs and how
		 * many partials there should be comes from the bitmaps, and this seal
		 * is what makes present_chunks reflect the bitmap just decoded. Its
		 * crc is discarded -- the full bitmap is still zeroed at this point. */
		s3_export_manifest_seal(m);
		rc = unpack_all_refs(m, j.refs, j.full, j.partials);
		if (rc != 0) {
			goto out;
		}
	}

	/* The two cross-checks. Everything above catches a manifest that is
	 * malformed; these catch one that is well formed and wrong, which is the
	 * dangerous kind -- a flipped bit in the bitmap turns a chunk into a hole
	 * that reads as zeroes with no error anywhere. */
	s3_export_manifest_seal(m);
	if (m->crc32c != j.crc32c) {
		SPDK_ERRLOG("export manifest bitmap crc mismatch: got %#x, want %#x\n",
			    m->crc32c, j.crc32c);
		rc = -EIO;
		goto out;
	}
	if (m->present_chunks != j.present_chunks) {
		SPDK_ERRLOG("export manifest claims %" PRIu64 " present chunks, bitmap "
			    "has %" PRIu64 "\n", j.present_chunks, m->present_chunks);
		rc = -EIO;
		goto out;
	}

	m->created_at   = j.created_at;
	m->generation   = j.generation;
	m->expires_at   = j.expires_at;
	m->cluster_size = j.cluster_size;
	copy_field(m->src.endpoint, sizeof(m->src.endpoint), j.src.endpoint);
	copy_field(m->src.region,   sizeof(m->src.region),   j.src.region);
	copy_field(m->src.bucket,   sizeof(m->src.bucket),   j.src.bucket);
	copy_field(m->src.prefix,   sizeof(m->src.prefix),   j.src.prefix);
	copy_field(m->src.lvs_name, sizeof(m->src.lvs_name), j.src.lvs_name);
	copy_field(m->src.snapshot, sizeof(m->src.snapshot), j.src.snapshot);
	m->src.blob_id = j.src.blob_id;
	copy_field(m->src.snapshot_uuid, sizeof(m->src.snapshot_uuid),
		   j.src.snapshot_uuid);

	*out = m;
	m = NULL;
	rc = 0;
out:
	s3_export_manifest_unref(m);
	free_manifest_json(&j);
	free(decoded);
	free(values);
	free(copy);
	return rc;
}

/* ==========================================================================
 * Publishing
 * ========================================================================== */

struct publish_ctx {
	struct s3_export_manifest *m;
	char            *json;
	struct iovec       iov;
	char           key[S3_EXPORT_KEY_MAX];
	s3_export_cb            cb_fn;
	void  *cb_arg;
};

static void
publish_done(void *cb_arg, int status)
{
	struct publish_ctx *ctx = cb_arg;
	struct s3_export_manifest *m = ctx->m;
	s3_export_cb cb_fn = ctx->cb_fn;
	void *user_arg = ctx->cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to upload the manifest '%s': %s\n", ctx->key,
			    spdk_strerror(-status));
	}

	free(ctx->json);
	free(ctx);

	if (status != 0) {
		s3_export_manifest_unref(m);
		m = NULL;
	}
	if (cb_fn) {
		cb_fn(user_arg, m, status);
	} else {
		s3_export_manifest_unref(m);
	}
}

int
s3_export_manifest_publish(struct s3_client *client,
			   struct s3_export_manifest *m,
			   s3_export_cb cb, void *cb_arg)
{
	struct publish_ctx *ctx;
	size_t len;
	int rc;

	if (!client || !m) {
		return -EINVAL;
	}

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}
	ctx->cb_fn = cb;
	ctx->cb_arg = cb_arg;
	ctx->m = m;
	s3_export_manifest_ref(m);

	/* Sealed here rather than by the caller. The checksum and the present count
	 * have to describe the bytes that are about to be uploaded, and a caller that
	 * forgot would produce a manifest whose own cross-checks fail on import --
	 * discovered on the other machine, hours later. */
	s3_export_manifest_seal(m);

	rc = s3_export_manifest_serialize(m, &ctx->json, &len);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		free(ctx);
		return rc;
	}

	ctx->iov.iov_base = ctx->json;
	ctx->iov.iov_len = len;
	s3_export_manifest_key(m->uuid_str, ctx->key, sizeof(ctx->key));

	rc = s3_put(client, ctx->key, &ctx->iov, 1, false, publish_done, ctx);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		free(ctx->json);
		free(ctx);
	}
	return rc;
}

/* ==========================================================================
 * The copy engine
 * ========================================================================== */

struct s3_export_ctx;

struct s3_export_slot {
	struct s3_export_ctx *ctx;
	void                 *buf;
	struct iovec          iov;
	uint64_t              chunk_index;
	char                  key[S3_EXPORT_KEY_MAX];
};

struct s3_export_ctx {
	struct s3_client          *client;
	struct spdk_blob          *blob;
	struct spdk_io_channel    *channel;

	char                       prefix[S3_EXPORT_PREFIX_MAX];
	char                       uuid_str[SPDK_UUID_STRING_LEN];

	uint32_t                   chunk_size;
	uint32_t                   blocks_per_chunk;
	uint64_t                   num_chunks;
	uint64_t                   next_chunk;

	uint32_t                   max_inflight;
	uint32_t                   inflight;

	/* Free slot stack: indices into slots[]. */
	struct s3_export_slot     *slots;
	uint32_t                  *free_slots;
	uint32_t                   num_free;

	/* First error wins; later ones are logged by whoever produced them. */
	int                        status;

	/* Set while the submit loop is running, so a completion that fires inline
	 * does not start a second loop underneath it. */
	bool                       pumping;

	uint64_t                   zero_chunks;
	uint64_t                   put_chunks;

	struct s3_export_manifest *m;
	char                      *manifest_json;
	struct iovec               manifest_iov;
	char                       manifest_key[S3_EXPORT_KEY_MAX];

	s3_export_cb               cb_fn;
	void                      *cb_arg;
};

static void export_pump(struct s3_export_ctx *ctx);

static bool
buf_is_zero(const void *buf, size_t len)
{
	const uint64_t *words = buf;
	size_t i;

	/* len is a whole number of 8 bytes: chunk_size is a power of two no
	 * smaller than 4 KiB. */
	for (i = 0; i < len / sizeof(*words); i++) {
		if (words[i] != 0) {
			return false;
		}
	}
	return true;
}

static void
export_ctx_free(struct s3_export_ctx *ctx)
{
	uint32_t i;

	if (ctx->slots) {
		for (i = 0; i < ctx->max_inflight; i++) {
			spdk_free(ctx->slots[i].buf);
		}
		free(ctx->slots);
	}
	free(ctx->free_slots);
	free(ctx->manifest_json);
	s3_export_manifest_unref(ctx->m);
	free(ctx);
}

static void
export_report(struct s3_export_ctx *ctx)
{
	struct s3_export_manifest *m = NULL;
	s3_export_cb cb_fn = ctx->cb_fn;
	void *cb_arg = ctx->cb_arg;
	int status = ctx->status;

	if (status == 0) {
		/* Hand our reference to the caller. */
		m = ctx->m;
		ctx->m = NULL;
	}

	export_ctx_free(ctx);

	if (cb_fn) {
		cb_fn(cb_arg, m, status);
	} else {
		s3_export_manifest_unref(m);
	}
}

static void
export_manifest_uploaded(void *cb_arg, int status)
{
	struct s3_export_ctx *ctx = cb_arg;

	if (status != 0) {
		SPDK_ERRLOG("Failed to upload export manifest '%s': %s\n",
			    ctx->manifest_key, spdk_strerror(-status));
		ctx->status = status;
	} else {
		SPDK_NOTICELOG("Export %s complete: %" PRIu64 " chunk object(s), "
			       "%" PRIu64 " sparse, %" PRIu64 " bytes logical\n",
			       ctx->uuid_str, ctx->put_chunks, ctx->zero_chunks,
			       ctx->m->size_bytes);
	}

	export_report(ctx);
}

static void
export_finish(struct s3_export_ctx *ctx)
{
	size_t len;
	int rc;

	if (ctx->status != 0) {
		SPDK_ERRLOG("Export %s failed after %" PRIu64 " of %" PRIu64
			    " chunks: %s. No manifest was written, so the objects "
			    "left behind are unreferenced.\n",
			    ctx->uuid_str, ctx->next_chunk, ctx->num_chunks,
			    spdk_strerror(-ctx->status));
		export_report(ctx);
		return;
	}

	s3_export_manifest_seal(ctx->m);
	ctx->m->created_at = (uint64_t)time(NULL);

	rc = s3_export_manifest_serialize(ctx->m, &ctx->manifest_json, &len);
	if (rc != 0) {
		ctx->status = rc;
		export_report(ctx);
		return;
	}

	ctx->manifest_iov.iov_base = ctx->manifest_json;
	ctx->manifest_iov.iov_len = len;
	s3_export_manifest_key(ctx->uuid_str, ctx->manifest_key,
			       sizeof(ctx->manifest_key));

	rc = s3_put(ctx->client, ctx->manifest_key, &ctx->manifest_iov, 1, false,
		    export_manifest_uploaded, ctx);
	if (rc != 0) {
		ctx->status = rc;
		export_report(ctx);
	}
}

static void
export_slot_release(struct s3_export_slot *slot)
{
	struct s3_export_ctx *ctx = slot->ctx;

	ctx->free_slots[ctx->num_free++] = (uint32_t)(slot - ctx->slots);
	ctx->inflight--;
}

static void
export_chunk_uploaded(void *cb_arg, int status)
{
	struct s3_export_slot *slot = cb_arg;
	struct s3_export_ctx *ctx = slot->ctx;

	if (status != 0) {
		SPDK_ERRLOG("Failed to upload export chunk %" PRIu64 " ('%s'): %s\n",
			    slot->chunk_index, slot->key, spdk_strerror(-status));
		if (ctx->status == 0) {
			ctx->status = status;
		}
	} else {
		/* Marked only once the object is durable, so the bitmap can never
		 * promise a chunk that is not there. */
		s3_export_manifest_set_present(ctx->m, slot->chunk_index);
		ctx->put_chunks++;
	}

	export_slot_release(slot);
	export_pump(ctx);
}

static void
export_chunk_read(void *cb_arg, int bserrno)
{
	struct s3_export_slot *slot = cb_arg;
	struct s3_export_ctx *ctx = slot->ctx;
	int rc;

	if (bserrno != 0) {
		SPDK_ERRLOG("Failed to read chunk %" PRIu64 " of the snapshot being "
			    "exported: %s\n", slot->chunk_index, spdk_strerror(-bserrno));
		if (ctx->status == 0) {
			ctx->status = bserrno;
		}
		export_slot_release(slot);
		export_pump(ctx);
		return;
	}

	/* An all-zero chunk is left out of the export entirely: the reader treats
	 * a clear bit as zeroes, so this is what keeps a thin volume thin. It also
	 * covers the holes of a clone, whose reads come back zero-filled without
	 * any S3 traffic. */
	if (buf_is_zero(slot->buf, ctx->chunk_size)) {
		ctx->zero_chunks++;
		export_slot_release(slot);
		export_pump(ctx);
		return;
	}

	s3_export_chunk_key(ctx->prefix, ctx->uuid_str, slot->chunk_index,
			    slot->key, sizeof(slot->key));
	slot->iov.iov_base = slot->buf;
	slot->iov.iov_len = ctx->chunk_size;

	rc = s3_put(ctx->client, slot->key, &slot->iov, 1, false,
		    export_chunk_uploaded, slot);
	if (rc != 0) {
		SPDK_ERRLOG("Failed to submit export chunk %" PRIu64 ": %s\n",
			    slot->chunk_index, spdk_strerror(-rc));
		if (ctx->status == 0) {
			ctx->status = rc;
		}
		export_slot_release(slot);
		export_pump(ctx);
	}
}

static void
export_start_one(struct s3_export_ctx *ctx)
{
	struct s3_export_slot *slot;

	slot = &ctx->slots[ctx->free_slots[--ctx->num_free]];
	slot->chunk_index = ctx->next_chunk++;
	ctx->inflight++;

	spdk_blob_io_read(ctx->blob, ctx->channel, slot->buf,
			  slot->chunk_index * ctx->blocks_per_chunk,
			  ctx->blocks_per_chunk, export_chunk_read, slot);
}

/* Keep max_inflight operations going, and notice when there is nothing left.
 *
 * Re-entrant by design: every completion path ends here, and a completion may
 * fire inline from inside the submit loop. The `pumping` guard makes that
 * harmless -- the inner call returns immediately and the outer loop, which
 * re-tests its conditions each time round, picks up the freed slot. */
static void
export_pump(struct s3_export_ctx *ctx)
{
	if (ctx->pumping) {
		return;
	}
	ctx->pumping = true;
	while (ctx->status == 0 && ctx->next_chunk < ctx->num_chunks &&
	       ctx->num_free > 0) {
		export_start_one(ctx);
	}
	ctx->pumping = false;

	/* Nothing outstanding means either everything was submitted and completed,
	 * or an error stopped the loop; both are the end. It cannot mean "idle with
	 * work left", because the loop only exits on error, on exhaustion, or with
	 * every slot busy. */
	if (ctx->inflight == 0) {
		export_finish(ctx);
	}
}

int
s3_export_run(const struct s3_export_opts *opts, s3_export_cb cb, void *cb_arg)
{
	struct s3_export_ctx *ctx;
	uint64_t size_bytes;
	uint32_t i;
	int rc;

	if (!opts || !opts->client || !opts->blob || !opts->channel ||
	    !opts->prefix || !opts->uuid_str) {
		return -EINVAL;
	}

	/* The blob decides how big the export is; opts carries geometry, not size.
	 * Asking the caller for both invites a mismatch that would show up as a
	 * short or over-long export. */
	size_bytes = spdk_blob_get_num_io_units(opts->blob) * S3LVOL_BLOCK_SIZE;

	ctx = calloc(1, sizeof(*ctx));
	if (!ctx) {
		return -ENOMEM;
	}

	ctx->client       = opts->client;
	ctx->blob         = opts->blob;
	ctx->channel      = opts->channel;
	ctx->chunk_size   = opts->chunk_size ? opts->chunk_size : S3LVOL_DEFAULT_CHUNK_SIZE;
	ctx->max_inflight = opts->max_inflight ? opts->max_inflight :
			    S3_EXPORT_DEFAULT_INFLIGHT;
	ctx->cb_fn        = cb;
	ctx->cb_arg       = cb_arg;
	snprintf(ctx->prefix, sizeof(ctx->prefix), "%s", opts->prefix);
	snprintf(ctx->uuid_str, sizeof(ctx->uuid_str), "%s", opts->uuid_str);

	rc = s3_export_manifest_create(ctx->uuid_str, size_bytes, ctx->chunk_size,
				       S3_EXPORT_LAYOUT_DENSE, &ctx->m);
	if (rc != 0) {
		export_ctx_free(ctx);
		return rc;
	}

	ctx->m->src          = opts->src;
	ctx->m->cluster_size = opts->cluster_size;
	ctx->num_chunks= ctx->m->num_chunks;
	ctx->blocks_per_chunk = ctx->chunk_size / S3LVOL_BLOCK_SIZE;

	if (ctx->max_inflight > ctx->num_chunks) {
		ctx->max_inflight = (uint32_t)ctx->num_chunks;
	}

	ctx->slots = calloc(ctx->max_inflight, sizeof(*ctx->slots));
	ctx->free_slots = calloc(ctx->max_inflight, sizeof(*ctx->free_slots));
	if (!ctx->slots || !ctx->free_slots) {
		export_ctx_free(ctx);
		return -ENOMEM;
	}

	for (i = 0; i < ctx->max_inflight; i++) {
		/* DMA capable and block aligned: a read may be served from the
		 * local device the WAL and the cache live on, which goes through
		 * spdk_bdev_read() and can require both. */
		ctx->slots[i].buf = spdk_zmalloc(ctx->chunk_size, S3LVOL_BLOCK_SIZE,
						 NULL, SPDK_ENV_SOCKET_ID_ANY,
						 SPDK_MALLOC_DMA);
		if (!ctx->slots[i].buf) {
			export_ctx_free(ctx);
			return -ENOMEM;
		}
		ctx->slots[i].ctx = ctx;
		ctx->free_slots[i] = i;
	}
	ctx->num_free = ctx->max_inflight;

	SPDK_NOTICELOG("Exporting %" PRIu64 " bytes as %s: %" PRIu64 " chunk(s) of "
		       "%u bytes, %u in flight\n", size_bytes, ctx->uuid_str,
		       ctx->num_chunks, ctx->chunk_size, ctx->max_inflight);

	export_pump(ctx);
	return 0;
}
