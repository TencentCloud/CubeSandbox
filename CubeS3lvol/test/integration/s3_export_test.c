/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Export manifest unit test -- no S3, no credentials, no DPDK, no threads
 *
 *   === What this is actually for ===
 *
 *   The manifest is the entire contract between two nodes. Everything else about
 *   an export can be verified by looking at the volume that comes out the other
 *   end; the manifest is the one part where being *subtly* wrong produces an lvol
 *   that reads plausibly and is not what was exported.
 *
 *   So the interesting sections are not the round trip -- that either works or
 *   fails obviously -- but section [4], which corrupts a well-formed manifest in
 *   the ways that would otherwise go unnoticed:
 *
 *     - a flipped bit in the presence bitmap turns a chunk into a hole. A hole
 *       reads as zeroes, with no request, no error and nothing in a log.
 *     - a bumped version or an unknown layout means the file means something
 *       else. A future zero-copy layout names the *source's* chunk objects; read
 *       as if it were this one, every read would land on an unrelated object.
 *     - a truncated body, which is what a half-written manifest looks like.
 *
 *   Each has to be rejected, and rejected before anything is built from it. The
 *   crc and the present count exist for exactly this, and this test is what keeps
 *   them honest -- an export failure this catches would otherwise surface as
 *   "the resumed sandbox has holes in its filesystem".
 *
 *   Usage:
 *     ./s3_export_test
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"

#include "s3lvol/s3_export.h"
/* For s3_chunk_data_key(), which is how the read path turns a ref into the key it
 * GETs -- case [13] composes one the same way to check the prefix ends up in it. */
#include "s3lvol/s3_chunk_map.h"

#define CHUNK_SIZE (1024 * 1024)

/* The version a freshly written manifest carries, which is what the strings these
 * cases patch actually contain.
 *
 * Deliberately not S3_EXPORT_VERSION any more: a manifest is written at the oldest
 * version that can describe it, so serialize() emits 2 for the single-source
 * exports every one of these cases builds. Tying this to S3_EXPORT_VERSION made
 * the substitutions silently match nothing the moment the constant moved to 3, and
 * check_rejected() then reported "could not build the case" rather than passing
 * vacuously -- which is the only reason it was noticed. */
#define STRINGIFY_(x) #x
#define STRINGIFY(x)  STRINGIFY_(x)
#define WRITTEN_VERSION 2
#define VERSION_FIELD "\"version\":" STRINGIFY(WRITTEN_VERSION)

static int g_pass;
static int g_fail;

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("\t[PASS] %s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("\t[FAIL] %s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

static void
check_u64(const char *what, uint64_t got, uint64_t want)
{
	char detail[128];

	snprintf(detail, sizeof(detail), "(got %" PRIu64 ", want %" PRIu64 ")",
		 got, want);
	check_true(what, got == want, detail);
}

static void
check_int(const char *what, int got, int want)
{
	char detail[128];

	snprintf(detail, sizeof(detail), "(got %d, want %d)", got, want);
	check_true(what, got == want, detail);
}

static void
check_str(const char *what, const char *got, const char *want)
{
	char detail[512];

	snprintf(detail, sizeof(detail), "(got '%s', want '%s')", got, want);
	check_true(what, strcmp(got, want) == 0, detail);
}

static const char *TEST_UUID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301";

/* A manifest with a deliberately awkward pattern: the first chunk, the last
 * chunk, and one in the middle, so an off-by-one at either end of the bitmap
 * shows up rather than cancelling out. */
static struct s3_export_manifest *
build_manifest(uint64_t size_bytes)
{
	struct s3_export_manifest *m = NULL;
	int rc;

	rc = s3_export_manifest_create(TEST_UUID, size_bytes, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	if (rc != 0) {
		return NULL;
	}

	s3_export_manifest_set_present(m, 0);
	s3_export_manifest_set_present(m, 5);
	s3_export_manifest_set_present(m, m->num_chunks - 1);

	m->cluster_size = CHUNK_SIZE;
	m->created_at = 1770000000;
	snprintf(m->src.endpoint, sizeof(m->src.endpoint), "cos.ap-nanjing.myqcloud.com");
	snprintf(m->src.region, sizeof(m->src.region), "ap-nanjing");
	snprintf(m->src.bucket, sizeof(m->src.bucket), "test-bucket-1250000000");
	snprintf(m->src.prefix, sizeof(m->src.prefix), "srclvs");
	snprintf(m->src.lvs_name, sizeof(m->src.lvs_name), "srclvs");
	snprintf(m->src.snapshot, sizeof(m->src.snapshot), "vol0-export-3f2504e0");
	m->src.blob_id = 0x100000042ULL;
	snprintf(m->src.snapshot_uuid, sizeof(m->src.snapshot_uuid), "%s",
		 "3f2b1c8e-5a47-4d19-9e6f-70c4a2b8d531");

	s3_export_manifest_seal(m);
	return m;
}

/* ==========================================================================
 * [1] Geometry
 * ========================================================================== */

static void
test_geometry(void)
{
	struct s3_export_manifest *m = NULL;
	int rc;

	printf("\n[1] geometry and what create() refuses\n");

	rc = s3_export_manifest_create(TEST_UUID, 64 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	check_int("a 64 MiB export is accepted", rc, 0);
	if (rc == 0) {
		check_u64("num_chunks", m->num_chunks, 64);
		check_u64("size_bytes", m->size_bytes, 64ULL * CHUNK_SIZE);
		check_u64("block_size", m->block_size, 4096);
		/* 2, not S3_EXPORT_VERSION: a manifest is created at the oldest
		 * version that can describe it, and is only raised to 3 by
		 * serialize() if it ends up with more than one source. Writing 3
		 * unconditionally would make every ordinary export unreadable to
		 * binaries that have not been upgraded, for no gain. */
		check_u64("a fresh manifest is written as version 2", m->version, 2);
		check_u64("a fresh manifest has no chunks", m->present_chunks, 0);
		check_str("the uuid is kept verbatim", m->uuid_str, TEST_UUID);
		s3_export_manifest_unref(m);
		m = NULL;
	}

	/* Refused rather than rounded up. A partial last chunk would mean either an
	 * object shorter than all the others or a read past the end of the
	 * snapshot, and both are avoidable: blob sizes are whole clusters. */
	rc = s3_export_manifest_create(TEST_UUID, 64 * CHUNK_SIZE + 4096, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	check_true("a size that is not a whole number of chunks is refused",
		   rc == -EINVAL, NULL);

	/* Chunk indexing is a shift, here and in the chunk map. */
	rc = s3_export_manifest_create(TEST_UUID, 3 * 1000 * 1000, 1000 * 1000,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	check_true("a chunk size that is not a power of two is refused",
		   rc == -EINVAL, NULL);

	rc = s3_export_manifest_create(TEST_UUID, 8192, 512,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	check_true("a chunk size below the block size is refused", rc == -EINVAL, NULL);

	rc = s3_export_manifest_create(TEST_UUID, 0, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	check_true("a zero-length export is refused", rc == -EINVAL, NULL);
}

/* ==========================================================================
 * [2] The bitmap
 * ========================================================================== */

static void
test_bitmap(void)
{
	struct s3_export_manifest *m = NULL;
	int rc;

	printf("\n[2] the presence bitmap\n");

	/* 70 chunks: not a multiple of 8, so the last byte is partial. That is
	 * where a popcount over whole bytes would count bits that do not exist. */
	rc = s3_export_manifest_create(TEST_UUID, 70 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	if (rc != 0) {
		check_true("create for the bitmap test", false, NULL);
		return;
	}

	s3_export_manifest_set_present(m, 0);
	s3_export_manifest_set_present(m, 7);
	s3_export_manifest_set_present(m, 8);
	s3_export_manifest_set_present(m, 69);
	s3_export_manifest_seal(m);

	check_u64("present_chunks after four set bits", m->present_chunks, 4);
	check_true("chunk 0 is present", s3_export_manifest_is_present(m, 0), NULL);
	check_true("chunk 7 is present", s3_export_manifest_is_present(m, 7), NULL);
	check_true("chunk 8 is present", s3_export_manifest_is_present(m, 8), NULL);
	check_true("chunk 69 is present", s3_export_manifest_is_present(m, 69), NULL);
	check_true("chunk 1 is a hole", !s3_export_manifest_is_present(m, 1), NULL);
	check_true("chunk 68 is a hole", !s3_export_manifest_is_present(m, 68), NULL);

	/* Out of range reads as absent rather than reading past the allocation.
	 * The bs_dev checks its own range first, so this is the second line of
	 * defence, not the first. */
	check_true("a chunk past the end is absent",
		   !s3_export_manifest_is_present(m, 70), NULL);
	check_true("a chunk far past the end is absent",
		   !s3_export_manifest_is_present(m, 1000000), NULL);

	check_true("chunks 1..6 are all zeroes",
		   s3_export_manifest_range_is_zeroes(m, 1, 6), NULL);
	check_true("a range containing chunk 7 is not zeroes",
		   !s3_export_manifest_range_is_zeroes(m, 1, 7), NULL);
	check_true("chunks 9..68 are all zeroes",
		   s3_export_manifest_range_is_zeroes(m, 9, 60), NULL);
	check_true("a range ending at chunk 69 is not zeroes",
		   !s3_export_manifest_range_is_zeroes(m, 9, 61), NULL);

	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [3] Round trip
 * ========================================================================== */

static void
test_round_trip(void)
{
	struct s3_export_manifest *m, *parsed = NULL;
	char *json = NULL;
	size_t len = 0;
	uint64_t i;
	bool bitmaps_match = true;
	int rc;

	printf("\n[3] serialize, then parse\n");

	m = build_manifest(64 * CHUNK_SIZE);
	if (!m) {
		check_true("build the manifest", false, NULL);
		return;
	}

	rc = s3_export_manifest_serialize(m, &json, &len);
	check_int("serialize", rc, 0);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		return;
	}
	check_true("the JSON is not empty", len > 0 && json[0] == '{', NULL);

	/* A 64 MiB export must not need a large manifest: the bitmap is what keeps
	 * this from scaling with the volume. If this ever fails because keys were
	 * added per chunk, that is a design change, not a threshold to raise. */
	check_true("the manifest is compact", len < 2048, NULL);

	rc = s3_export_manifest_parse(json, len, &parsed);
	check_int("parse", rc, 0);
	if (rc != 0) {
		printf("\t---- the JSON was: %.*s\n", (int)len, json);
		free(json);
		s3_export_manifest_unref(m);
		return;
	}

	check_u64("layout survives", parsed->layout, S3_EXPORT_LAYOUT_DENSE);
	check_str("uuid survives", parsed->uuid_str, m->uuid_str);
	check_u64("size_bytes survives", parsed->size_bytes, m->size_bytes);
	check_u64("chunk_size survives", parsed->chunk_size, m->chunk_size);
	check_u64("cluster_size survives", parsed->cluster_size, m->cluster_size);
	check_u64("num_chunks survives", parsed->num_chunks, m->num_chunks);
	check_u64("present_chunks survives", parsed->present_chunks, m->present_chunks);
	check_u64("crc32c survives", parsed->crc32c, m->crc32c);
	check_u64("created_at survives", parsed->created_at, m->created_at);
	check_str("endpoint survives", parsed->src.endpoint, m->src.endpoint);
	check_str("region survives", parsed->src.region, m->src.region);
	check_str("bucket survives", parsed->src.bucket, m->src.bucket);
	check_str("prefix survives", parsed->src.prefix, m->src.prefix);
	check_str("lvs_name survives", parsed->src.lvs_name, m->src.lvs_name);
	check_str("snapshot survives", parsed->src.snapshot, m->src.snapshot);
	check_u64("blob_id survives", parsed->src.blob_id, m->src.blob_id);
	/* The identity of the source snapshot, as opposed to blob_id, which
	 * blobstore reuses after a delete. An import that degenerates into a
	 * local clone decides on this field, so losing it in a round trip would
	 * silently push every self-import back onto the esnap path. */
	check_str("snapshot_uuid survives", parsed->src.snapshot_uuid,
		  m->src.snapshot_uuid);

	for (i = 0; i < m->num_chunks; i++) {
		if (s3_export_manifest_is_present(m, i) !=
		    s3_export_manifest_is_present(parsed, i)) {
			bitmaps_match = false;
			break;
		}
	}
	check_true("every chunk's presence survives", bitmaps_match, NULL);

	free(json);
	s3_export_manifest_unref(parsed);
	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [4] Manifests that must be refused
 *
 * The point of the whole exercise. Each of these parses as JSON, so nothing but
 * an explicit check stands between it and a volume full of the wrong bytes.
 * ========================================================================== */

/* Replaces the first occurrence of \c from with \c to. Both are JSON fragments,
 * so this is exact-text surgery on a document we produced. */
static char *
tamper(const char *json, const char *from, const char *to, size_t *out_len)
{
	const char *at = strstr(json, from);
	size_t prefix, result_len;
	char *out;

	if (!at) {
		return NULL;
	}
	prefix = (size_t)(at - json);
	result_len = strlen(json) - strlen(from) + strlen(to);

	out = malloc(result_len + 1);
	if (!out) {
		return NULL;
	}
	memcpy(out, json, prefix);
	memcpy(out + prefix, to, strlen(to));
	strcpy(out + prefix + strlen(to), at + strlen(from));

	*out_len = result_len;
	return out;
}

static void
check_rejected(const char *what, const char *json, const char *from, const char *to)
{
	struct s3_export_manifest *parsed = NULL;
	char *broken;
	size_t len = 0;
	int rc;

	broken = tamper(json, from, to, &len);
	if (!broken) {
		check_true(what, false, "(could not build the case)");
		return;
	}

	rc = s3_export_manifest_parse(broken, len, &parsed);
	check_true(what, rc != 0 && parsed == NULL, NULL);
	if (rc == 0) {
		s3_export_manifest_unref(parsed);
	}
	free(broken);
}

static void
test_rejections(void)
{
	struct s3_export_manifest *m, *parsed = NULL;
	char *json = NULL;
	size_t len = 0;
	int rc;

	printf("\n[4] manifests that must be refused\n");

	m = build_manifest(64 * CHUNK_SIZE);
	if (!m || s3_export_manifest_serialize(m, &json, &len) != 0) {
		check_true("build a manifest to corrupt", false, NULL);
		s3_export_manifest_unref(m);
		return;
	}

	/* The crc's whole reason for existing. A single wrong byte in the bitmap
	 * silently converts chunks into holes, and a hole reads as zeroes without
	 * an error anywhere. */
	check_rejected("a bitmap that disagrees with its crc is refused", json,
		       "\"crc32c\":", "\"crc32c\":123456789,\"unused\":");

	/* The second cross-check. Catches the case where the crc happens to be
	 * recomputed over a modified bitmap -- i.e. a manifest rewritten by
	 * something that did not understand it. */
	check_rejected("a present count that disagrees with the bitmap is refused",
		       json, "\"present_chunks\":3", "\"present_chunks\":2");

	/* Reading a future layout as this one would resolve every chunk to an
	 * unrelated object, which is worse than failing. */
	check_rejected("an unknown layout is refused", json,
		       "\"layout\":\"dense\"", "\"layout\":\"uuid\"");
	/* Above the accepted range: a future layout read as this one would resolve
	 * every chunk somewhere unrelated. */
	check_rejected("a newer version is refused", json,
		       VERSION_FIELD, "\"version\":999");
	/* Below it. Version 1's ref entries are 24 bytes against version 2's 16, so
	 * reading one as the other names a different object for every chunk past the
	 * first and returns plausible data from the wrong place -- which is why 1 is
	 * refused rather than merely deprecated. */
	check_rejected("version 1 is refused", json,
		       VERSION_FIELD, "\"version\":1");

	check_rejected("a block size other than 4 KiB is refused", json,
		       "\"block_size\":4096", "\"block_size\":512");
	check_rejected("a num_chunks that contradicts the geometry is refused", json,
		       "\"num_chunks\":64", "\"num_chunks\":63");
	check_rejected("a size that is not a whole number of chunks is refused", json,
		       "\"size_bytes\":67108864", "\"size_bytes\":67112960");
	check_rejected("a missing bitmap is refused", json,
		       "\"present\":", "\"absent\":");
	check_rejected("a missing uuid is refused", json,
		       "\"export_uuid\":", "\"uuid\":");
	check_rejected("a bitmap that is too short is refused", json,
		       "\"present\":\"", "\"present\":\"A");

	/* What half a manifest looks like. A completed PUT cannot be short, so this
	 * is really about a truncated read or a partially written local copy. */
	rc = s3_export_manifest_parse(json, len / 2, &parsed);
	check_true("a truncated manifest is refused", rc != 0 && parsed == NULL, NULL);

	rc = s3_export_manifest_parse(json, 0, &parsed);
	check_true("an empty manifest is refused", rc != 0, NULL);

	rc = s3_export_manifest_parse("not json at all", 15, &parsed);
	check_true("a non-JSON body is refused", rc != 0, NULL);

	/* And the control: the untampered document still parses, so the rejections
	 * above are about what was changed and not about the harness. */
	rc = s3_export_manifest_parse(json, len, &parsed);
	check_int("the original still parses", rc, 0);
	s3_export_manifest_unref(parsed);

	free(json);
	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [5] Key layout
 *
 * These strings are an on-the-wire format: an importer built by another version
 * derives the same keys from (uuid, index), and GC recognises the prefix. They
 * cannot change without changing both.
 * ========================================================================== */

static void
test_keys(void)
{
	char key[512];

	printf("\n[5] key layout\n");

	/* Bucket-level, deliberately: the uuid has to be a complete address for an
	 * importer, and an importer does not know the exporting lvstore's name -- that
	 * is the other machine's local naming. */
	s3_export_manifest_key(TEST_UUID, key, sizeof(key));
	check_str("manifest key", key,
		  "exports/3f2504e0-4f89-11d3-9a0c-0305e82c3301.json");
	check_true("the manifest key carries no lvstore prefix",
		   key[0] == 'e', NULL);

	/* The chunks stay under the source's prefix. They are the source's data, a
	 * zero-copy export writes none of them, and where they are comes out of the
	 * manifest rather than out of a caller's parameter. */
	s3_export_chunk_prefix("srclvs", TEST_UUID, key, sizeof(key));
	check_str("chunk prefix", key,
		  "srclvs/exports/3f2504e0-4f89-11d3-9a0c-0305e82c3301/");

	/* Fixed width and lower case hex, so a listing of the prefix comes back in
	 * chunk order. */
	s3_export_chunk_key("srclvs", TEST_UUID, 0, key, sizeof(key));
	check_str("chunk 0", key,
		  "srclvs/exports/3f2504e0-4f89-11d3-9a0c-0305e82c3301/0000000000000000");

	s3_export_chunk_key("srclvs", TEST_UUID, 255, key, sizeof(key));
	check_str("chunk 255", key,
		  "srclvs/exports/3f2504e0-4f89-11d3-9a0c-0305e82c3301/00000000000000ff");

	s3_export_chunk_key("srclvs", TEST_UUID, 1048576, key, sizeof(key));
	check_str("chunk 1048576", key,
		  "srclvs/exports/3f2504e0-4f89-11d3-9a0c-0305e82c3301/0000000000100000");

	/* The manifest sits *beside* the chunk prefix, not inside it. That is what
	 * lets a release delete the manifest first and still enumerate the chunks,
	 * and what lets GC treat a prefix whose manifest is missing as garbage. */
	s3_export_manifest_key(TEST_UUID, key, sizeof(key));
	check_true("the manifest is not under the chunk prefix",
		   strstr(key, "3f2504e0-4f89-11d3-9a0c-0305e82c3301/") == NULL, NULL);
}

/* ==========================================================================
 * [6] Sparse and dense extremes
 * ========================================================================== */

static void
test_extremes(void)
{
	struct s3_export_manifest *m = NULL, *parsed = NULL;
	char *json = NULL;
	size_t len = 0;
	uint64_t i;
	int rc;

	printf("\n[6] a wholly sparse and a wholly dense export\n");

	/* An export of a volume that was never written. Legal, and it must survive
	 * a round trip -- an importer of it gets a volume of zeroes, not an error. */
	rc = s3_export_manifest_create(TEST_UUID, 16 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	if (rc == 0) {
		s3_export_manifest_seal(m);
		rc = s3_export_manifest_serialize(m, &json, &len);
		check_int("an empty export serializes", rc, 0);
		if (rc == 0) {
			rc = s3_export_manifest_parse(json, len, &parsed);
			check_int("an empty export parses", rc, 0);
			if (rc == 0) {
				check_u64("it has no chunks", parsed->present_chunks, 0);
				check_true("all of it reads as zeroes",
					   s3_export_manifest_range_is_zeroes(parsed, 0, 16),
					   NULL);
				s3_export_manifest_unref(parsed);
				parsed = NULL;
			}
			free(json);
			json = NULL;
		}
		s3_export_manifest_unref(m);
		m = NULL;
	}

	rc = s3_export_manifest_create(TEST_UUID, 16 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	if (rc == 0) {
		for (i = 0; i < 16; i++) {
			s3_export_manifest_set_present(m, i);
		}
		s3_export_manifest_seal(m);
		check_u64("a fully allocated export counts every chunk",
			  m->present_chunks, 16);
		check_true("none of it reads as zeroes",
			   !s3_export_manifest_range_is_zeroes(m, 0, 16), NULL);

		rc = s3_export_manifest_serialize(m, &json, &len);
		if (rc == 0) {
			rc = s3_export_manifest_parse(json, len, &parsed);
			check_int("a fully allocated export round trips", rc, 0);
			if (rc == 0) {
				check_u64("with all its chunks",
					  parsed->present_chunks, 16);
				s3_export_manifest_unref(parsed);
			}
			free(json);
		}
		s3_export_manifest_unref(m);
	}
}

/* ==========================================================================
 * [7] References
 * ========================================================================== */

static void
test_refcount(void)
{
	struct s3_export_manifest *m = NULL;
	int rc;

	printf("\n[7] reference counting\n");

	rc = s3_export_manifest_create(TEST_UUID, CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_DENSE, &m);
	if (rc != 0) {
		check_true("create", false, NULL);
		return;
	}

	/* One reference per bs_dev built from it plus one for the registry, and a
	 * bs_dev is destroyed by blobstore long after the import request is gone.
	 * If this were an owner pointer, releasing an import would free a manifest
	 * that a live esnap parent is still reading. */
	check_u64("a fresh manifest has one reference", m->refcnt, 1);
	s3_export_manifest_ref(m);
	check_u64("ref", m->refcnt, 2);
	s3_export_manifest_ref(m);
	check_u64("ref again", m->refcnt, 3);
	s3_export_manifest_unref(m);
	s3_export_manifest_unref(m);
	check_u64("unref twice", m->refcnt, 1);

	/* Frees; anything after this would be a use after free, which is the point
	 * of running this under a sanitizer occasionally. */
	s3_export_manifest_unref(m);
	check_true("the last unref frees it", true, NULL);

	s3_export_manifest_unref(NULL);
	check_true("unref(NULL) is a no-op", true, NULL);
}

/* ==========================================================================
 * [8] Ref layout
 *
 * The zero-copy manifest: rather than naming export-private copies, each chunk
 * names the object the source lvstore currently holds it in. What is new here is
 * that a uuid and a valid_bytes have to survive per chunk, and that the two
 * layouts must not be mistakable for one another -- a ref manifest read as dense
 * would resolve every chunk to an export-private key that was never written, and
 * a dense one read as ref would have nothing to resolve at all.
 * ========================================================================== */

static void
fill_uuid(struct spdk_uuid *uuid, uint8_t tag)
{
	memset(uuid, tag, sizeof(*uuid));
}

static void
test_ref_layout(void)
{
	struct s3_export_manifest *m = NULL, *parsed = NULL;
	struct spdk_uuid u0, u5, u63;
	const struct s3_export_ref *ref;
	char *json = NULL;
	size_t len = 0;
	int rc;

	printf("\n[8] ref layout\n");

	rc = s3_export_manifest_create(TEST_UUID, 64 * CHUNK_SIZE, CHUNK_SIZE,
				   S3_EXPORT_LAYOUT_REF, &m);
	check_int("a ref manifest is created", rc, 0);
	if (rc != 0) {
		return;
	}

	fill_uuid(&u0, 0xa0);
	fill_uuid(&u5, 0xa5);
	fill_uuid(&u63, 0xaf);

	check_int("set_ref chunk 0",
		  s3_export_manifest_set_ref(m, 0, &u0, CHUNK_SIZE), 0);
	/* A partially written chunk: its object holds 4 KiB and the rest of the
	 * chunk reads as zeroes. This is the case a dense export cannot produce and
	 * a ref export cannot avoid. */
	check_int("set_ref chunk 5, partly written",
		  s3_export_manifest_set_ref(m, 5, &u5, 4096), 0);
	check_int("set_ref the last chunk",
		  s3_export_manifest_set_ref(m, 63, &u63, CHUNK_SIZE), 0);

	check_true("set_ref marks the chunk present",
		   s3_export_manifest_is_present(m, 5), NULL);
	check_true("a chunk with no ref is still a hole",
		   !s3_export_manifest_is_present(m, 6), NULL);
	check_true("get_ref on a hole returns nothing",
		   s3_export_manifest_get_ref(m, 6) == NULL, NULL);

	/* valid_bytes has to describe a readable part of the chunk. Zero would name
	 * an object nothing can be read out of; more than a chunk would have the
	 * reader range past its end. */
	check_true("valid_bytes of 0 is refused",
		   s3_export_manifest_set_ref(m, 7, &u0, 0) == -EINVAL, NULL);
	check_true("valid_bytes past the chunk size is refused",
		   s3_export_manifest_set_ref(m, 7, &u0, CHUNK_SIZE + 1) == -EINVAL,
		   NULL);
	check_true("a rejected set_ref leaves the chunk a hole",
		   !s3_export_manifest_is_present(m, 7), NULL);
	check_true("an index past the end is refused",
		   s3_export_manifest_set_ref(m, 64, &u0, CHUNK_SIZE) == -EINVAL, NULL);

	s3_export_manifest_seal(m);
	check_u64("three refs, three present chunks", m->present_chunks, 3);

	rc = s3_export_manifest_serialize(m, &json, &len);
	check_int("a ref manifest serializes", rc, 0);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		return;
	}

	rc = s3_export_manifest_parse(json, len, &parsed);
	check_int("a ref manifest parses", rc, 0);
	if (rc != 0) {
		printf("\t---- the JSON was: %.*s\n", (int)len, json);
		free(json);
		s3_export_manifest_unref(m);
		return;
	}

	check_u64("the layout survives", parsed->layout, S3_EXPORT_LAYOUT_REF);
	check_u64("present_chunks survives", parsed->present_chunks, 3);

	ref = s3_export_manifest_get_ref(parsed, 0);
	check_true("chunk 0 has a ref", ref != NULL, NULL);
	if (ref) {
		check_true("chunk 0's uuid survives",
			   spdk_uuid_compare(&ref->uuid, &u0) == 0, NULL);
		check_u64("chunk 0's valid_bytes survives", ref->valid_bytes,
			  CHUNK_SIZE);
	}

	/* The load-bearing one. Refs are packed in bitmap order and their count is
	 * not stored, so an off-by-one anywhere in that walk surfaces here as one
	 * chunk carrying another chunk's uuid -- which reads back as entirely valid
	 * data from the wrong place. */
	ref = s3_export_manifest_get_ref(parsed, 5);
	check_true("chunk 5 has a ref", ref != NULL, NULL);
	if (ref) {
		check_true("chunk 5 kept its own uuid, not a neighbour's",
			   spdk_uuid_compare(&ref->uuid, &u5) == 0, NULL);
		check_u64("chunk 5's partial valid_bytes survives", ref->valid_bytes,
			  4096);
	}

	ref = s3_export_manifest_get_ref(parsed, 63);
	check_true("the last chunk has a ref", ref != NULL, NULL);
	if (ref) {
		check_true("the last chunk's uuid survives",
			   spdk_uuid_compare(&ref->uuid, &u63) == 0, NULL);
	}

	/* Layout confusion, both directions. Each of these parses as JSON. */
	check_rejected("a ref manifest with its refs removed is refused", json,
		   "\"refs\":", "\"unused\":");
	check_rejected("a ref manifest relabelled dense is refused", json,
		   "\"layout\":\"ref\"", "\"layout\":\"dense\"");
	check_rejected("refs that decode to the wrong length are refused", json,
		       "\"refs\":\"", "\"refs\":\"AAAA");

	/* The full bitmap and the partials table are what turn 16 packed bytes per
	 * chunk back into a length. Losing either one does not make the manifest
	 * unreadable -- it makes every partial chunk read as if it were whole, i.e.
	 * a range request past the end of an object that is shorter than the
	 * manifest implies. So both are refused rather than defaulted. */
	check_rejected("a ref manifest with no full bitmap is refused", json,
		       "\"full\":", "\"unused\":");
	check_rejected("a ref manifest with no partials table is refused", json,
		       "\"partials\":", "\"unused\":");
	check_rejected("a full bitmap of the wrong length is refused", json,
		       "\"full\":\"", "\"full\":\"AAAA");
	check_rejected("a partials table of the wrong length is refused", json,
		       "\"partials\":\"", "\"partials\":\"AAAA");

	free(json);
	json = NULL;
	s3_export_manifest_unref(parsed);
	s3_export_manifest_unref(m);

	/* A dense manifest must refuse ref operations, and must refuse to be read
	 * with a ref table bolted on. */
	rc = s3_export_manifest_create(TEST_UUID, CHUNK_SIZE, CHUNK_SIZE,
				    S3_EXPORT_LAYOUT_DENSE, &m);
	if (rc != 0) {
		return;
	}
	check_true("set_ref on a dense manifest is refused",
		   s3_export_manifest_set_ref(m, 0, &u0, CHUNK_SIZE) == -EINVAL, NULL);
	check_true("get_ref on a dense manifest returns nothing",
		   s3_export_manifest_get_ref(m, 0) == NULL, NULL);

	s3_export_manifest_set_present(m, 0);
	s3_export_manifest_seal(m);
	if (s3_export_manifest_serialize(m, &json, &len) == 0) {
		check_rejected("a dense manifest carrying refs is refused", json,
			       "\"present\":",
			     "\"refs\":\"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\",\"present\":");
		/* A dense manifest has no lengths to reconstruct -- it uploads whole
		 * chunks. One carrying the machinery for it was written by something
		 * with two ideas about what the file is. */
		check_rejected("a dense manifest carrying a full bitmap is refused",
			       json, "\"present\":", "\"full\":\"AA==\",\"present\":");
		free(json);
	}
	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [9] The crc has to cover the refs
 *
 * Two manifests differing by one uuid and nothing else must not agree on their
 * checksum. If they did, a corrupted ref table would validate, and a corrupted
 * ref reads somebody else's object -- successfully.
 * ========================================================================== */

static void
test_ref_crc(void)
{
	struct s3_export_manifest *a = NULL, *b = NULL;
	struct spdk_uuid u1, u2;
	int rc;

	printf("\n[9] the crc covers the refs\n");

	rc = s3_export_manifest_create(TEST_UUID, 4 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_REF, &a);
	if (rc != 0) {
		check_true("create a", false, NULL);
		return;
	}
	rc = s3_export_manifest_create(TEST_UUID, 4 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_REF, &b);
	if (rc != 0) {
		check_true("create b", false, NULL);
		s3_export_manifest_unref(a);
		return;
	}

	fill_uuid(&u1, 0x11);
	fill_uuid(&u2, 0x22);

	s3_export_manifest_set_ref(a, 2, &u1, CHUNK_SIZE);
	s3_export_manifest_set_ref(b, 2, &u2, CHUNK_SIZE);
	s3_export_manifest_seal(a);
	s3_export_manifest_seal(b);

	check_true("same bitmap, different uuid, different crc",
		   a->crc32c != b->crc32c, NULL);

	/* valid_bytes as well: it decides where zero-filling starts, so a corrupted
	 * one silently truncates a chunk. */
	s3_export_manifest_set_ref(b, 2, &u1, 4096);
	s3_export_manifest_seal(b);
	check_true("same uuid, different valid_bytes, different crc",
		   a->crc32c != b->crc32c, NULL);

	s3_export_manifest_set_ref(b, 2, &u1, CHUNK_SIZE);
	s3_export_manifest_seal(b);
	check_u64("identical refs, identical crc", b->crc32c, a->crc32c);

	s3_export_manifest_unref(a);
	s3_export_manifest_unref(b);
}

/* ==========================================================================
 * [10] The shape every real export has: no partial chunks
 *
 * valid_bytes only ever falls short of a chunk at the tail of whatever wrote it,
 * so on a volume filled in the usual way every present chunk is full and the
 * partials table is empty. That is the case the encoding was designed around --
 * it is what makes a ref 16 bytes instead of 20 -- so it is worth pinning down
 * that an empty table round-trips rather than being treated as a missing one.
 * ========================================================================== */

static void
test_ref_all_full(void)
{
	struct s3_export_manifest *m = NULL, *parsed = NULL;
	const struct s3_export_ref *ref;
	struct spdk_uuid u;
	char *json = NULL;
	size_t len = 0;
	uint64_t i;
	int rc;

	printf("\n[10] a ref manifest with no partial chunks\n");

	rc = s3_export_manifest_create(TEST_UUID, 8 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_REF, &m);
	if (rc != 0) {
		check_true("create", false, NULL);
		return;
	}

	for (i = 0; i < 8; i++) {
		fill_uuid(&u, (uint8_t)(0xc0 + i));
		check_int("set_ref a whole chunk",
			  s3_export_manifest_set_ref(m, i, &u, CHUNK_SIZE), 0);
	}

	s3_export_manifest_seal(m);
	check_u64("all eight chunks are present", m->present_chunks, 8);

	rc = s3_export_manifest_serialize(m, &json, &len);
	check_int("it serializes", rc, 0);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		return;
	}

	/* The point of the full bitmap: nothing is spent on lengths at all. */
	check_true("the partials table is empty",
		   strstr(json, "\"partials\":\"\"") != NULL, json);

	rc = s3_export_manifest_parse(json, len, &parsed);
	check_int("it parses", rc, 0);
	if (rc == 0) {
		check_u64("present_chunks survives", parsed->present_chunks, 8);
		check_u64("the crc survives", parsed->crc32c, m->crc32c);

		for (i = 0; i < 8; i++) {
			ref = s3_export_manifest_get_ref(parsed, i);
			if (!ref) {
				check_true("every chunk has a ref", false, NULL);
				break;
			}
			fill_uuid(&u, (uint8_t)(0xc0 + i));
			if (spdk_uuid_compare(&ref->uuid, &u) != 0 ||
			    ref->valid_bytes != CHUNK_SIZE) {
				check_true("every chunk kept its own uuid and a "
					   "whole-chunk length", false, NULL);
				break;
			}
		}
		if (i == 8) {
			check_true("every chunk kept its own uuid and a whole-chunk "
				   "length", true, NULL);
		}
		s3_export_manifest_unref(parsed);
	}

	free(json);
	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [11] A manifest captured off the wire
 *
 * Byte for byte what a dense export wrote into an imports registry during a real
 * run against COS. It is here because a manifest that this code produced and
 * could not read back is the failure that costs a destination lvstore its ability
 * to load at all -- the esnap parent is demanded synchronously, so an unreadable
 * manifest is not a degraded import, it is an lvstore that will not attach.
 * ========================================================================== */

static void
test_captured_manifest(void)
{
	static const char captured[] =
		"{\"version\":2,\"layout\":\"dense\","
		"\"export_uuid\":\"7fd623b9-0b9b-4e0d-b668-b77fa7b464da\","
		"\"generation\":0,\"created_at\":1785901312,\"expires_at\":0,"
		"\"source\":{\"endpoint\":\"cos.ap-nanjing.myqcloud.com\","
		"\"region\":\"ap-nanjing\",\"bucket\":\"cube-cow-1253970226\","
		"\"prefix\":\"expsrc\",\"lvs_name\":\"expsrc\","
		"\"snapshot\":\"vol0-snap1\",\"blob_id\":4294967299},"
		"\"size_bytes\":67108864,\"cluster_size\":4194304,"
		"\"chunk_size\":4194304,\"block_size\":4096,\"num_chunks\":16,"
		"\"present_chunks\":2,\"crc32c\":3550388836,\"present\":\"DAA=\"}";
	struct s3_export_manifest *m = NULL;
	int rc;

	printf("\n[11] a manifest captured off the wire\n");

	rc = s3_export_manifest_parse(captured, strlen(captured), &m);
	check_int("a real dense manifest parses", rc, 0);
	if (rc != 0) {
		return;
	}

	check_u64("num_chunks survives", m->num_chunks, 16);
	check_u64("present_chunks survives", m->present_chunks, 2);
	check_true("chunk 2 is present", s3_export_manifest_is_present(m, 2), NULL);
	check_true("chunk 3 is present", s3_export_manifest_is_present(m, 3), NULL);
	check_true("chunk 0 is a hole", !s3_export_manifest_is_present(m, 0), NULL);

	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [12] A version 2 ref manifest, read by a binary that can write version 3
 *
 * The compatibility direction that matters. Version 3 appends src_idx to the crc,
 * so a reader that stages the crc by S3_EXPORT_VERSION rather than by the
 * manifest's own version recomputes every version 2 ref manifest with a stage it
 * does not carry, and reports corruption on all of them -- including the ones the
 * same binary wrote before it was upgraded.
 *
 * Dense manifests would not have caught it: the extra stage only exists for ref.
 * The manifest below is built by the writer, so its crc is genuine rather than
 * asserted, and parse then has to agree with it.
 * ========================================================================== */

static void
test_v2_ref_compat(void)
{
	struct s3_export_manifest *m = NULL;
	struct s3_export_manifest *parsed = NULL;
	struct spdk_uuid u = {0};
	char *json = NULL;
	size_t len = 0;
	int rc;

	printf("\n[12] a version 2 ref manifest read by a version 3 binary\n");

	rc = s3_export_manifest_create(TEST_UUID, 8 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_REF, &m);
	check_int("a ref manifest is created", rc, 0);
	if (rc != 0) {
		return;
	}

	/* One whole chunk and one partial, so the crc covers both the ref table and
	 * the partial-length exceptions -- the two stages version 2 has. */
	fill_uuid(&u, 0x11);
	s3_export_manifest_set_ref(m, 1, &u, CHUNK_SIZE);
	fill_uuid(&u, 0x22);
	s3_export_manifest_set_ref(m, 5, &u, CHUNK_SIZE / 2);

	check_u64("written as version 2", m->version, 2);
	check_u64("with one source", m->num_srcs, 1);
	check_true("and no per-chunk source table", m->src_idx == NULL, NULL);

	/* serialize() does not seal, and pack_all_refs() sizes its buffer from
	 * present_chunks, so an unsealed manifest asserts rather than producing a
	 * short one. Every other caller seals too. */
	s3_export_manifest_seal(m);

	rc = s3_export_manifest_serialize(m, &json, &len);
	check_int("it serializes", rc, 0);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		return;
	}
	check_true("the json says version 2", strstr(json, "\"version\":2") != NULL,
		   json);
	check_true("and carries no source table",
		   strstr(json, "\"srcs\"") == NULL, json);

	rc = s3_export_manifest_parse(json, len, &parsed);
	check_int("a version 2 ref manifest parses", rc, 0);
	if (rc == 0) {
		/* The assertion this case exists for: the crc has to verify, which it
		 * only does if seal() staged it the way version 2 wrote it. */
		check_u64("its crc verifies", parsed->crc32c, m->crc32c);
		check_u64("version is kept, not overwritten", parsed->version, 2);
		check_u64("a source table was synthesised", parsed->num_srcs, 1);
		check_str("naming its own prefix", parsed->srcs[0].prefix,
			  m->src.prefix);
		check_true("with src_idx still absent", parsed->src_idx == NULL, NULL);
		check_u64("present chunks survive", parsed->present_chunks, 2);
		s3_export_manifest_unref(parsed);
	}

	free(json);
	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [13] A version 3 manifest: chunks from more than one prefix
 *
 * The format the whole v3 step exists for. What is asserted is that a round trip
 * preserves which prefix each chunk resolves against -- because getting that wrong
 * does not fail, it reads another chunk's object and returns bytes that look
 * perfectly good.
 * ========================================================================== */

static void
test_v3_multi_source(void)
{
	struct s3_export_manifest *m = NULL, *parsed = NULL;
	struct spdk_uuid u;
	uint8_t own = 0xff, parent = 0xff;
	char *json = NULL;
	size_t len = 0;
	int rc;

	printf("\n[13] a version 3 manifest with two sources\n");

	rc = s3_export_manifest_create(TEST_UUID, 8 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_REF, &m);
	check_int("a ref manifest is created", rc, 0);
	if (rc != 0) {
		return;
	}
	/* The manifest's own prefix. Set directly, as every other case here does --
	 * there is no setter, and add_src() below has to agree with it. */
	snprintf(m->src.prefix, sizeof(m->src.prefix), "lvs-b");
	snprintf(m->src.lvs_name, sizeof(m->src.lvs_name), "lvs-b");

	/* Entry 0 is this export's own prefix and must already be there. */
	rc = s3_export_manifest_add_src(m, "lvs-b", NULL, NULL, &own);
	check_int("adding its own prefix succeeds", rc, 0);
	check_u64("and resolves to entry 0", own, 0);
	check_u64("without growing the table", m->num_srcs, 1);

	rc = s3_export_manifest_add_src(m, "lvs-a", "exp-a-uuid", "snap-a-uuid",
					&parent);
	check_int("adding a parent prefix succeeds", rc, 0);
	check_u64("as entry 1", parent, 1);
	check_u64("the table has two entries", m->num_srcs, 2);

	/* Idempotent: a derived export names the same parent for every chunk it
	 * inherited, so the writer must be able to ask per chunk. */
	rc = s3_export_manifest_add_src(m, "lvs-a", NULL, NULL, &parent);
	check_int("asking again succeeds", rc, 0);
	check_u64("gives the same index", parent, 1);
	check_u64("and does not grow the table", m->num_srcs, 2);

	/* Two chunks of its own, two inherited. */
	fill_uuid(&u, 0x11);
	s3_export_manifest_set_ref(m, 0, &u, CHUNK_SIZE);
	fill_uuid(&u, 0x22);
	s3_export_manifest_set_ref(m, 1, &u, CHUNK_SIZE / 2);
	fill_uuid(&u, 0x33);
	s3_export_manifest_set_ref(m, 4, &u, CHUNK_SIZE);
	check_int("chunk 4 takes the parent source",
		  s3_export_manifest_set_chunk_src(m, 4, parent), 0);
	fill_uuid(&u, 0x44);
	s3_export_manifest_set_ref(m, 7, &u, CHUNK_SIZE);
	check_int("chunk 7 takes the parent source",
		  s3_export_manifest_set_chunk_src(m, 7, parent), 0);

	check_int("a source nobody added is refused",
		  s3_export_manifest_set_chunk_src(m, 2, 9), -EINVAL);

	check_str("chunk 0 resolves to its own prefix",
		  s3_export_manifest_chunk_prefix(m, 0), "lvs-b");
	check_str("chunk 4 resolves to the parent",
		  s3_export_manifest_chunk_prefix(m, 4), "lvs-a");

	s3_export_manifest_seal(m);
	rc = s3_export_manifest_serialize(m, &json, &len);
	check_int("it serializes", rc, 0);
	if (rc != 0) {
		s3_export_manifest_unref(m);
		return;
	}
	/* Raised by serialize, not by create: the version follows the source count. */
	check_u64("serializing raised it to version 3", m->version, 3);
	check_true("the json says version 3", strstr(json, "\"version\":3") != NULL,
		   json);
	check_true("it carries a source table", strstr(json, "\"srcs\"") != NULL, json);
	check_true("and per-chunk indices", strstr(json, "\"src_idx\"") != NULL, json);
	/* Entry 0 is never written: it mirrors "source", and a second copy would be a
	 * second place for the same prefix to be wrong. So the table starts at the
	 * parent, and "lvs-b" appears only inside "source". */
	check_true("the source table starts at entry 1",
		   strstr(json, "\"srcs\":[{\"prefix\":\"lvs-a\"") != NULL, json);
	check_true("entry 0 is not repeated in it",
		   strstr(json, "{\"prefix\":\"lvs-b\"") == NULL, json);

	rc = s3_export_manifest_parse(json, len, &parsed);
	check_int("it parses back", rc, 0);
	if (rc == 0) {
		check_u64("as version 3", parsed->version, 3);
		check_u64("with both sources", parsed->num_srcs, 2);
		check_str("entry 0 synthesised from source", parsed->srcs[0].prefix,
			  "lvs-b");
		check_str("entry 1 read from the wire", parsed->srcs[1].prefix, "lvs-a");
		check_str("with its export uuid", parsed->srcs[1].export_uuid,
			  "exp-a-uuid");
		check_str("and its snapshot uuid", parsed->srcs[1].snapshot_uuid,
			  "snap-a-uuid");
		check_u64("the crc verifies", parsed->crc32c, m->crc32c);

		check_str("chunk 0 still resolves to its own prefix",
			  s3_export_manifest_chunk_prefix(parsed, 0), "lvs-b");
		check_str("chunk 1 too",
			  s3_export_manifest_chunk_prefix(parsed, 1), "lvs-b");
		check_str("chunk 4 still resolves to the parent",
			  s3_export_manifest_chunk_prefix(parsed, 4), "lvs-a");
		check_str("chunk 7 too",
			  s3_export_manifest_chunk_prefix(parsed, 7), "lvs-a");

		/* The keys themselves, composed the way export_chunk_key() composes
		 * them. Worth asserting separately from the prefix: this is the value a
		 * GET is issued against, and if two chunks from different sources came
		 * out under one prefix the read would still succeed -- on another
		 * chunk's object. */
		{
			const struct s3_export_ref *r0, *r4;
			char k0[512], k4[512];

			r0 = s3_export_manifest_get_ref(parsed, 0);
			r4 = s3_export_manifest_get_ref(parsed, 4);
			check_true("both chunks have refs", r0 && r4, NULL);
			if (r0 && r4) {
				s3_chunk_data_key(s3_export_manifest_chunk_prefix(parsed, 0),
						  &r0->uuid, k0, sizeof(k0));
				s3_chunk_data_key(s3_export_manifest_chunk_prefix(parsed, 4),
						  &r4->uuid, k4, sizeof(k4));
				check_true("chunk 0's key is under its own prefix",
					   strncmp(k0, "lvs-b/", 6) == 0, k0);
				check_true("chunk 4's key is under the parent's prefix",
					   strncmp(k4, "lvs-a/", 6) == 0, k4);
			}
		}
		s3_export_manifest_unref(parsed);
	}

	/* A source table without indices, or indices without a table, describes
	 * chunks nobody can resolve; both halves have to arrive. */
	check_rejected("a source table without indices is refused", json,
		       "\"src_idx\"", "\"src_idx_unused\"");

	free(json);
	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [14] The source limit
 *
 * Reaching it must be reported, not wrapped: a 17th source silently becoming
 * source 1 would resolve those chunks against the wrong prefix.
 * ========================================================================== */

static void
test_src_limit(void)
{
	struct s3_export_manifest *m = NULL;
	char prefix[32];
	uint8_t idx = 0;
	int rc, i;

	printf("\n[14] the source limit\n");

	rc = s3_export_manifest_create(TEST_UUID, 8 * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_REF, &m);
	if (rc != 0) {
		check_int("a ref manifest is created", rc, 0);
		return;
	}
	snprintf(m->src.prefix, sizeof(m->src.prefix), "lvs0");
	rc = s3_export_manifest_add_src(m, "lvs0", NULL, NULL, &idx);
	check_int("entry 0 is its own prefix", rc, 0);

	/* Entry 0 exists already, so MAX_SOURCES-1 more fit. */
	for (i = 1; i < S3_EXPORT_MAX_SOURCES; i++) {
		snprintf(prefix, sizeof(prefix), "lvs%d", i);
		rc = s3_export_manifest_add_src(m, prefix, NULL, NULL, &idx);
		if (rc != 0) {
			break;
		}
	}
	check_int("filling the table succeeds", rc, 0);
	check_u64("it holds exactly the limit", m->num_srcs, S3_EXPORT_MAX_SOURCES);

	rc = s3_export_manifest_add_src(m, "one-too-many", NULL, NULL, &idx);
	check_int("one more is refused with -E2BIG", rc, -E2BIG);
	check_u64("and the table is unchanged", m->num_srcs, S3_EXPORT_MAX_SOURCES);

	s3_export_manifest_unref(m);
}

/* ==========================================================================
 * [15] Inheriting from the export a volume reads through
 *
 * The write side of a handoff that is itself derived from an import. What has to
 * come out right is not just "the chunks are there": each one has to be recorded
 * against the prefix that really holds it, and against the export whose lease
 * protects it -- which for a parent that was itself derived is the grandparent's,
 * not the parent's. Getting either wrong produces a manifest that reads fine on
 * the machine that wrote it and fails, or serves another volume's data, elsewhere.
 * ========================================================================== */

/* A ref manifest standing in for one published by another node. */
static struct s3_export_manifest *
make_parent(const char *uuid, const char *prefix, const char *snap_uuid,
	    uint64_t chunks)
{
	struct s3_export_manifest *p = NULL;
	uint8_t idx;
	int rc;

	rc = s3_export_manifest_create(uuid, chunks * CHUNK_SIZE, CHUNK_SIZE,
				       S3_EXPORT_LAYOUT_REF, &p);
	if (rc != 0) {
		return NULL;
	}
	snprintf(p->src.prefix, sizeof(p->src.prefix), "%s", prefix);
	snprintf(p->src.lvs_name, sizeof(p->src.lvs_name), "%s", prefix);
	snprintf(p->src.snapshot_uuid, sizeof(p->src.snapshot_uuid), "%s", snap_uuid);
	snprintf(p->src.bucket, sizeof(p->src.bucket), "bkt");
	snprintf(p->src.endpoint, sizeof(p->src.endpoint), "ep");
	if (s3_export_manifest_add_src(p, prefix, NULL, NULL, &idx) != 0) {
		s3_export_manifest_unref(p);
		return NULL;
	}
	return p;
}

static void
test_inherit(void)
{
	struct s3_export_manifest *p = NULL, *g = NULL, *c = NULL;
	uint64_t named = 0, bytes = 0;
	struct spdk_uuid u;
	uint8_t idx;
	int rc;

	printf("\n[15] inheriting from the export a volume reads through\n");

	/* The parent: 8 chunks, owning 0,1,2 and 5, with the rest holes. */
	p = make_parent("11111111-1111-1111-1111-111111111111", "lvs-p",
			"snap-p-uuid", 8);
	c = make_parent("22222222-2222-2222-2222-222222222222", "lvs-c",
			"snap-c-uuid", 8);
	check_true("the manifests are built", p && c, NULL);
	if (!p || !c) {
		goto out;
	}

	fill_uuid(&u, 0xA0);
	s3_export_manifest_set_ref(p, 0, &u, CHUNK_SIZE);
	fill_uuid(&u, 0xA1);
	s3_export_manifest_set_ref(p, 1, &u, CHUNK_SIZE);
	fill_uuid(&u, 0xA2);
	s3_export_manifest_set_ref(p, 2, &u, CHUNK_SIZE / 4);
	fill_uuid(&u, 0xA5);
	s3_export_manifest_set_ref(p, 5, &u, CHUNK_SIZE);

	/* The child wrote chunk 1 since the import, and chunk 6 which the parent
	 * never had. */
	fill_uuid(&u, 0xB1);
	s3_export_manifest_set_ref(c, 1, &u, CHUNK_SIZE);
	fill_uuid(&u, 0xB6);
	s3_export_manifest_set_ref(c, 6, &u, CHUNK_SIZE);

	rc = s3_export_manifest_inherit(c, p, &named, &bytes);
	check_int("inheriting succeeds", rc, 0);
	/* 0, 2 and 5. Not 1 -- the child rewrote it -- and not the holes. */
	check_u64("it names the parent's chunks the child lacks", named, 3);
	check_u64("and their bytes", bytes, CHUNK_SIZE + CHUNK_SIZE / 4 + CHUNK_SIZE);

	/* Sealed first because present_chunks is derived there, not maintained by
	 * set_ref -- the same reason the writer seals before serializing. */
	s3_export_manifest_seal(c);
	check_u64("the child now has five chunks", c->present_chunks, 5);
	check_true("a hole in the parent stays a hole",
		   !s3_export_manifest_is_present(c, 3), NULL);

	/* The ordering rule, which is the one that silently serves stale data when
	 * broken: the chunk the child rewrote must still be the child's. */
	check_str("the rewritten chunk stays under the child's prefix",
		  s3_export_manifest_chunk_prefix(c, 1), "lvs-c");
	check_true("and keeps the child's object",
		   s3_export_manifest_get_ref(c, 1)->uuid.u.raw[0] == 0xB1, NULL);

	check_str("an inherited chunk moves to the parent's prefix",
		  s3_export_manifest_chunk_prefix(c, 0), "lvs-p");
	check_true("carrying the parent's object",
		   s3_export_manifest_get_ref(c, 0)->uuid.u.raw[0] == 0xA0, NULL);
	check_u64("and its partial length",
		  s3_export_manifest_get_ref(c, 2)->valid_bytes, CHUNK_SIZE / 4);

	/* The lease. An importer of the child must renew against the parent's own
	 * export, because that is the node holding these objects. */
	check_u64("one prefix was added", c->num_srcs, 2);
	check_str("named as the parent's prefix", c->srcs[1].prefix, "lvs-p");
	check_str("governed by the parent's export",
		  c->srcs[1].export_uuid, "11111111-1111-1111-1111-111111111111");
	check_str("and its snapshot", c->srcs[1].snapshot_uuid, "snap-p-uuid");

	/* Transitivity. A grandchild inheriting from the child must end up pointing
	 * at lvs-p directly for those chunks -- not at lvs-c, which does not have
	 * them -- and must renew the *parent's* lease, not the child's. This is what
	 * lets the middle node be shut down. */
	g = make_parent("33333333-3333-3333-3333-333333333333", "lvs-g",
			"snap-g-uuid", 8);
	check_true("a grandchild is built", g != NULL, NULL);
	if (!g) {
		goto out;
	}
	rc = s3_export_manifest_inherit(g, c, &named, &bytes);
	check_int("it inherits from the child", rc, 0);
	check_u64("taking everything the child had", named, 5);

	check_str("a chunk the child owned comes from the child",
		  s3_export_manifest_chunk_prefix(g, 1), "lvs-c");
	check_str("a chunk the child inherited comes from the grandparent, not the "
		  "child", s3_export_manifest_chunk_prefix(g, 0), "lvs-p");
	check_u64("so the grandchild names three prefixes", g->num_srcs, 3);

	/* Which is the point: no entry anywhere refers to a manifest, so nothing has
	 * to be fetched to resolve a chunk and no node in the history has to be
	 * running. */
	for (idx = 1; idx < g->num_srcs; idx++) {
		const char *pfx = g->srcs[idx].prefix;
		const char *want = (strcmp(pfx, "lvs-p") == 0)
				   ? "11111111-1111-1111-1111-111111111111"
				   : "22222222-2222-2222-2222-222222222222";

		check_str("each prefix is governed by the export that holds it",
			  g->srcs[idx].export_uuid, want);
	}

	/* A local write_zeroes leaves present clear -- that is also how an
	 * untouched hole is spelled -- so inherit used to restore the parent's
	 * object. resolved is what the walk sets for that case. */
	{
		struct s3_export_manifest *z;

		z = make_parent("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "lvs-z",
				"snap-z-uuid", 8);
		check_true("a child with a local zero is built", z != NULL, NULL);
		if (z) {
			fill_uuid(&u, 0xB1);
			s3_export_manifest_set_ref(z, 1, &u, CHUNK_SIZE);
			s3_export_manifest_set_resolved(z, 0);

			rc = s3_export_manifest_inherit(z, p, &named, &bytes);
			check_int("inherit still succeeds after a local zero", rc, 0);
			/* 2 and 5. Not 0 -- zeroed -- and not 1 -- rewritten. */
			check_u64("the zeroed chunk is not inherited", named, 2);
			check_true("and stays a hole on the child",
				   !s3_export_manifest_is_present(z, 0), NULL);
			check_true("while the parent's other objects still are",
				   s3_export_manifest_is_present(z, 2) &&
				   s3_export_manifest_is_present(z, 5), NULL);
			check_true("and the walk's resolved bit is what blocked it",
				   s3_export_manifest_is_resolved(z, 0), NULL);

			s3_export_manifest_seal(z);
			{
				char *json = NULL;
				size_t len = 0;

				rc = s3_export_manifest_serialize(z, &json, &len);
				check_int("the hole serializes", rc, 0);
				if (rc == 0 && json) {
					struct s3_export_manifest *parsed = NULL;

					rc = s3_export_manifest_parse(json, len, &parsed);
					check_int("and parses", rc, 0);
					if (rc == 0) {
						check_true("the wire form has no object at 0",
							   !s3_export_manifest_is_present(
								   parsed, 0), NULL);
						check_true("resolved does not travel",
							   !s3_export_manifest_is_resolved(
								   parsed, 0), NULL);
						s3_export_manifest_unref(parsed);
					}
					free(json);
				}
			}
			s3_export_manifest_unref(z);
		}
	}

	/* A parent shorter than the child: the volume grew after the import, so the
	 * chunks past its end were never the parent's to describe. */
	{
		struct s3_export_manifest *small, *big;

		small = make_parent("44444444-4444-4444-4444-444444444444", "lvs-s",
				    "snap-s-uuid", 2);
		big   = make_parent("55555555-5555-5555-5555-555555555555", "lvs-t",
				    "snap-t-uuid", 8);
		if (small && big) {
			fill_uuid(&u, 0xC0);
			s3_export_manifest_set_ref(small, 0, &u, CHUNK_SIZE);
			fill_uuid(&u, 0xC1);
			s3_export_manifest_set_ref(small, 1, &u, CHUNK_SIZE);

			rc = s3_export_manifest_inherit(big, small, &named, NULL);
			check_int("a shorter parent is accepted", rc, 0);
			check_u64("contributing only what it covers", named, 2);
			check_true("and nothing past its end",
				   !s3_export_manifest_is_present(big, 2), NULL);
		}
		s3_export_manifest_unref(small);
		s3_export_manifest_unref(big);
	}

	/* More distinct prefixes than a manifest can name. This is the degradation
	 * path, and what it has to do is *stop*: -E2BIG travels back to the writer,
	 * which exports by copying instead. Copying can express any history in one
	 * prefix, so nothing is lost but the speed.
	 *
	 * Asserted on inherit() rather than only on add_src() -- case [14] already
	 * covers the table filling up -- because a limit that is enforced but not
	 * propagated is the dangerous shape: the writer would carry on and publish a
	 * manifest missing exactly the chunks that did not fit, which reads as
	 * zeroes on the importer. */
	{
		struct s3_export_manifest *wide = NULL, *heir = NULL;
		char pfx[32];
		uint8_t widx;
		uint64_t k;

		/* A parent that has been handed on until its table is full, with a
		 * chunk under each prefix so every one of them has to be carried. */
		wide = make_parent("88888888-8888-8888-8888-888888888888", "lvs-w",
				   "snap-w-uuid", S3_EXPORT_MAX_SOURCES + 4);
		heir = make_parent("99999999-9999-9999-9999-999999999999", "lvs-h",
				   "snap-h-uuid", S3_EXPORT_MAX_SOURCES + 4);
		if (wide && heir) {
			for (k = 0; k < S3_EXPORT_MAX_SOURCES; k++) {
				fill_uuid(&u, (uint8_t)(0xD0 + k));
				s3_export_manifest_set_ref(wide, k, &u, CHUNK_SIZE);
				if (k == 0) {
					continue;
				}
				snprintf(pfx, sizeof(pfx), "lvs-w%u", (unsigned)k);
				rc = s3_export_manifest_add_src(wide, pfx, "e", "s",
								&widx);
				if (rc != 0) {
					break;
				}
				rc = s3_export_manifest_set_chunk_src(wide, k, widx);
				if (rc != 0) {
					break;
				}
			}
			check_int("a parent can be built at the source limit", rc, 0);
			check_u64("with a full table", wide->num_srcs,
				  S3_EXPORT_MAX_SOURCES);

			/* The heir already holds its own prefix at entry 0, so the
			 * parent's own prefix plus its 15 others need one slot more
			 * than remain. */
			rc = s3_export_manifest_inherit(heir, wide, NULL, NULL);
			check_int("inheriting more prefixes than fit gives -E2BIG",
				  rc, -E2BIG);
			check_u64("having filled the table and stopped there",
				  heir->num_srcs, S3_EXPORT_MAX_SOURCES);
		}
		s3_export_manifest_unref(wide);
		s3_export_manifest_unref(heir);
	}

	/* Different chunk sizes make chunk indices incomparable, so there is nothing
	 * to carry across without re-cutting the data. */
	{
		struct s3_export_manifest *other = NULL;

		rc = s3_export_manifest_create("66666666-6666-6666-6666-666666666666",
					       8 * CHUNK_SIZE * 2, CHUNK_SIZE * 2,
					       S3_EXPORT_LAYOUT_REF, &other);
		if (rc == 0) {
			check_int("a parent of another chunk size is refused",
				  s3_export_manifest_inherit(c, other, NULL, NULL),
				  -EINVAL);
			s3_export_manifest_unref(other);
		}
	}

	/* A copied export keys its objects exports/<uuid>/chunk-N, which a source
	 * entry -- a bare prefix -- cannot address. */
	{
		struct s3_export_manifest *dense = NULL;

		rc = s3_export_manifest_create("77777777-7777-7777-7777-777777777777",
					       8 * CHUNK_SIZE, CHUNK_SIZE,
					       S3_EXPORT_LAYOUT_DENSE, &dense);
		if (rc == 0) {
			check_int("a copied parent is refused",
				  s3_export_manifest_inherit(c, dense, NULL, NULL),
				  -EINVAL);
			s3_export_manifest_unref(dense);
		}
	}

	/* Where a reference cannot be expressed at all, and the writer has to copy.
	 *
	 * A source entry is a bare prefix sharing endpoint, bucket and region with
	 * `source`, so a parent reachable any other way cannot be named. Nothing
	 * here can be reached by the end-to-end suite -- it would need a second
	 * bucket -- and the failure it prevents is the quiet kind: a manifest naming
	 * a prefix in the wrong bucket resolves to whatever happens to be at that
	 * key there, which on a shared endpoint is another volume's data. */
	{
		struct s3_export_source dst;
		struct s3_export_manifest *dense = NULL;

		memset(&dst, 0, sizeof(dst));
		snprintf(dst.bucket, sizeof(dst.bucket), "bkt");
		snprintf(dst.endpoint, sizeof(dst.endpoint), "ep");

		/* p was built by make_parent() with bucket "bkt", endpoint "ep" and
		 * an empty region, so it matches and is inheritable. */
		check_int("a parent in the same place is inheritable",
			  s3_export_manifest_inheritable(p, &dst, "test"), 0);

		snprintf(dst.bucket, sizeof(dst.bucket), "other-bucket");
		check_int("a parent in another bucket is refused with -ENOTSUP",
			  s3_export_manifest_inheritable(p, &dst, "test"), -ENOTSUP);

		snprintf(dst.bucket, sizeof(dst.bucket), "bkt");
		snprintf(dst.endpoint, sizeof(dst.endpoint), "other-endpoint");
		check_int("a parent behind another endpoint is refused",
			  s3_export_manifest_inheritable(p, &dst, "test"), -ENOTSUP);

		/* Region carries no address by itself, but the client is built per
		 * endpoint/bucket/region, so a manifest that changed it silently would
		 * be resolved by a client the importer never meant to use. */
		snprintf(dst.endpoint, sizeof(dst.endpoint), "ep");
		snprintf(dst.region, sizeof(dst.region), "elsewhere");
		check_int("and so is one in another region",
			  s3_export_manifest_inheritable(p, &dst, "test"), -ENOTSUP);

		/* A copied export keys its objects exports/<uuid>/chunk-N, which a
		 * bare prefix cannot address however reachable the bucket is. */
		dst.region[0] = '\0';
		rc = s3_export_manifest_create("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					       8 * CHUNK_SIZE, CHUNK_SIZE,
					       S3_EXPORT_LAYOUT_DENSE, &dense);
		if (rc == 0) {
			snprintf(dense->src.bucket, sizeof(dense->src.bucket), "bkt");
			snprintf(dense->src.endpoint, sizeof(dense->src.endpoint), "ep");
			check_int("a copied parent is refused even in the right bucket",
				  s3_export_manifest_inheritable(dense, &dst, "test"),
				  -ENOTSUP);
			s3_export_manifest_unref(dense);
		}
	}

	/* And the whole thing has to survive the wire, since that is the only way the
	 * importing node ever sees it. */
	{
		char *json = NULL;
		size_t len = 0;
		struct s3_export_manifest *parsed = NULL;

		s3_export_manifest_seal(g);
		rc = s3_export_manifest_serialize(g, &json, &len);
		check_int("the grandchild serializes", rc, 0);
		if (rc == 0) {
			rc = s3_export_manifest_parse(json, len, &parsed);
			check_int("and parses back", rc, 0);
			if (rc == 0) {
				check_u64("with its three sources", parsed->num_srcs, 3);
				check_str("chunk 0 still points past the middleman",
					  s3_export_manifest_chunk_prefix(parsed, 0),
					  "lvs-p");
				check_str("and chunk 1 at the middleman",
					  s3_export_manifest_chunk_prefix(parsed, 1),
					  "lvs-c");
				s3_export_manifest_unref(parsed);
			}
			free(json);
		}
	}

out:
	s3_export_manifest_unref(g);
	s3_export_manifest_unref(c);
	s3_export_manifest_unref(p);
}

int
main(int argc, char **argv)
{
	spdk_log_open(NULL);
	/* Every rejection below logs why. Quiet by default so a passing run is
	 * readable; -v when a case is being investigated. */
	if (argc > 1 && strcmp(argv[1], "-v") == 0) {
		spdk_log_set_print_level(SPDK_LOG_DEBUG);
	} else {
		spdk_log_set_print_level(SPDK_LOG_WARN);
	}

	printf("=== s3lvol export manifest test ===\n");

	test_geometry();
	test_bitmap();
	test_round_trip();
	test_rejections();
	test_keys();
	test_extremes();
	test_refcount();
	test_ref_layout();
	test_ref_crc();
	test_ref_all_full();
	test_captured_manifest();
	test_v2_ref_compat();
	test_v3_multi_source();
	test_src_limit();
	test_inherit();

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	spdk_log_close();

	return g_fail == 0 ? 0 : 1;
}
