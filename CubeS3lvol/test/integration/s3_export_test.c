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

#define CHUNK_SIZE (1024 * 1024)

/* So that the "a newer version is refused" case follows S3_EXPORT_VERSION
 * instead of having to be edited every time it is bumped -- which is exactly
 * what went wrong the first time it was. */
#define STRINGIFY_(x) #x
#define STRINGIFY(x)  STRINGIFY_(x)
#define VERSION_FIELD "\"version\":" STRINGIFY(S3_EXPORT_VERSION)

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
		check_u64("version", m->version, S3_EXPORT_VERSION);
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
	check_rejected("a newer version is refused", json,
		       VERSION_FIELD, "\"version\":999");

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

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	spdk_log_close();

	return g_fail == 0 ? 0 : 1;
}
