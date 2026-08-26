/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Checkpoint validation check -- no S3, no credentials, no DPDK, no threads
 *
 *   === What this test proves ===
 *
 *   A checkpoint is a full snapshot of the chunk map, and it is the thing the
 *   journal gets truncated against. A checkpoint that is *subtly* wrong does not
 *   fail to load; it loads, and then the journal is truncated to its LSN, and
 *   every mapping it silently lost becomes an orphan that reads as zeroes. So
 *   the load path's rejections -- bad magic, bad version, header CRC, geometry
 *   mismatch, size mismatch, entry CRC -- are the difference between "recover
 *   from the journal" and "recover from nothing".
 *
 *   The load path needs an s3_client, so this test supplies its own: s3_head and
 *   s3_get_range are stubbed to hand back an in-memory object synchronously (the
 *   same "callback runs in place on the submitting thread" contract the real
 *   client has for non-SPDK-thread callers). That makes the whole
 *   s3_checkpoint_load() resolve inside the call, so the corruption cases are
 *   plain assertions on the returned status.
 *
 *   The object under test is built by hand -- a well-formed header plus three
 *   entries, both CRCs computed correctly -- and each corruption flips exactly
 *   one thing, so the specific errno is what is being asserted, not merely
 *   "something failed".
 *
 *   Usage:
 *     ./s3_checkpoint_test
 */

#include "spdk/stdinc.h"
#include "spdk/crc32.h"
#include "spdk/log.h"
#include "spdk/util.h"

#include "s3lvol/s3_checkpoint.h"
#include "s3lvol/s3_chunk_map.h"

static int g_pass;
static int g_fail;

static void
check_int(const char *what, int got, int want)
{
	if (got == want) {
		printf("  [PASS] %-52s rc=%d\n", what, got);
		g_pass++;
	} else {
		printf("  [FAIL] %-52s rc=%d (want %d)\n", what, got, want);
		g_fail++;
	}
}

static void
check_true(const char *what, bool ok, const char *detail)
{
	if (ok) {
		printf("  [PASS] %-52s %s\n", what, detail ? detail : "");
		g_pass++;
	} else {
		printf("  [FAIL] %-52s %s\n", what, detail ? detail : "");
		g_fail++;
	}
}

static void
check_u64(const char *what, uint64_t got, uint64_t want)
{
	char detail[96];

	snprintf(detail, sizeof(detail), "got %" PRIu64 ", want %" PRIu64, got, want);
	if (got == want) {
		printf("  [PASS] %-52s %s\n", what, detail);
		g_pass++;
	} else {
		printf("  [FAIL] %-52s %s\n", what, detail);
		g_fail++;
	}
}

/* ==========================================================================
 * Stub s3_client
 *
 * s3_checkpoint_load() only needs s3_head (to learn the size) and s3_get_range
 * (to fetch it). s3_put / s3_delete are required by the save / delete entry
 * points, which share this translation unit; they are stubbed but never reached
 * here. Defining all four keeps s3_client_aws.o out of the link entirely, which
 * is what lets this test run with no CRT and no credentials.
 * ========================================================================== */

static const uint8_t *g_obj;         /* the object bytes get_range returns */
static size_t         g_obj_len;     /* how many of them actually exist */
static uint64_t       g_head_size;   /* what s3_head reports */
static int            g_head_status; /* what s3_head reports as status */

int
s3_head(struct s3_client *client, const char *key, uint64_t *out_size,
	s3_op_cb cb, void *cb_arg)
{
	(void)client;
	(void)key;
	if (out_size) {
		*out_size = g_head_size;
	}
	cb(cb_arg, g_head_status);
	return 0;
}

int
s3_get_range(struct s3_client *client, const char *key, uint64_t offset,
	     uint64_t len, void *buf, s3_get_cb cb, void *cb_arg)
{
	uint64_t n;

	(void)client;
	(void)key;
	(void)offset;
	n = spdk_min(len, (uint64_t)g_obj_len);
	if (buf && n) {
		memcpy(buf, g_obj, n);
	}
	cb(cb_arg, n, 0);
	return 0;
}

int
s3_put(struct s3_client *client, const char *key, struct iovec *iov, int iovcnt,
       bool if_none_match, s3_op_cb cb, void *cb_arg)
{
	(void)client;
	(void)key;
	(void)iov;
	(void)iovcnt;
	(void)if_none_match;
	if (cb) {
		cb(cb_arg, 0);
	}
	return 0;
}

int
s3_delete(struct s3_client *client, const char *key, s3_op_cb cb, void *cb_arg)
{
	(void)client;
	(void)key;
	if (cb) {
		cb(cb_arg, 0);
	}
	return 0;
}

/* ==========================================================================
 * Building a well-formed checkpoint object
 * ========================================================================== */

#define TEST_CHUNK_SIZE (1024 * 1024)
#define TEST_NUM_CHUNKS 64
/* total_blocks for s3_chunk_map_create: the device capacity in 4 KiB blocks, so
 * that chunk_index 0 / 10 / 20 in the snapshot all fall inside the map. */
#define TEST_TOTAL_BLOCKS (TEST_NUM_CHUNKS * TEST_CHUNK_SIZE / 4096)

/* Reproduce the module's header CRC: crc field zeroed, seed 0. */
static uint32_t
ckpt_hdr_crc(const struct s3_ckpt_header *h)
{
	struct s3_ckpt_header t;

	memcpy(&t, h, sizeof(t));
	t.crc = 0;
	return spdk_crc32c_update(&t, sizeof(t), 0);
}

struct ckpt_blob {
	struct s3_ckpt_header hdr;
	struct s3_ckpt_entry entries[3];
};

static void
make_uuid(struct spdk_uuid *u, uint32_t tag)
{
	uint8_t *raw = (uint8_t *)u;

	memset(u, 0, sizeof(*u));
	raw[0] = 0xC7;
	raw[1] = (uint8_t)(tag & 0xff);
	raw[2] = (uint8_t)((tag >> 8) & 0xff);
	raw[15] = 0x71;
}

static void
build_ckpt(struct ckpt_blob *b)
{
	memset(b, 0, sizeof(*b));
	b->hdr.magic          = S3_CKPT_MAGIC;
	b->hdr.version        = S3_CKPT_VERSION;
	b->hdr.chunk_size     = TEST_CHUNK_SIZE;
	/* lvs_uuid stays all-zero, which the loader tolerates. */
	b->hdr.checkpoint_lsn = 42;
	b->hdr.gen            = 7;
	b->hdr.num_entries    = 3;

	for (uint32_t i = 0; i < 3; i++) {
		b->entries[i].chunk_index = i * 10;
		make_uuid(&b->entries[i].uuid, i + 1);
		b->entries[i].valid_bytes = (i + 1) * 4096;
		b->entries[i].flags       = S3_CHUNK_IN_S3;
		b->entries[i].gen         = 7;
	}

	b->hdr.entries_crc = spdk_crc32c_update(b->entries,
						3 * sizeof(struct s3_ckpt_entry),
						0);
	b->hdr.crc = ckpt_hdr_crc(&b->hdr);
}

/* ==========================================================================
 * The load callback, filled synchronously by the stubs
 * ========================================================================== */

struct load_result {
	uint64_t lsn;
	uint64_t gen;
	int      status;
};

static void
load_cb(void *cb_arg, uint64_t lsn, uint64_t gen, int status)
{
	struct load_result *r = cb_arg;

	r->lsn    = lsn;
	r->gen    = gen;
	r->status = status;
}

/* Run one load against a fresh map and report the result. */
static struct load_result
run_load(const struct ckpt_blob *blob, size_t blob_len, struct s3_chunk_map *map)
{
	struct load_result r = {0};

	g_obj = (const uint8_t *)blob;
	g_obj_len = blob_len;
	g_head_size = blob_len;
	g_head_status = 0;

	s3_checkpoint_load((struct s3_client *)0x1, "ckpt_test", map, load_cb, &r);
	return r;
}

int
main(void)
{
	struct ckpt_blob blob;
	struct s3_chunk_map *map = NULL;
	struct load_result r;
	int rc;

	spdk_log_set_print_level(SPDK_LOG_ERROR);
	spdk_log_open(NULL);

	printf("=== checkpoint validation check ===\n\n");

	build_ckpt(&blob);

	rc = s3_chunk_map_create(TEST_TOTAL_BLOCKS, 4096, TEST_CHUNK_SIZE, &map);
	if (rc != 0) {
		fprintf(stderr, "s3_chunk_map_create failed: %d\n", rc);
		spdk_log_close();
		return 1;
	}

	/* ---------- [1] a well-formed checkpoint loads ---------- */
	printf("[1] the honest round trip\n");
	r = run_load(&blob, sizeof(blob), map);
	check_int("status", r.status, 0);
	check_u64("the covered LSN comes back", r.lsn, 42);
	check_u64("the generation comes back", r.gen, 7);
	{
		struct spdk_uuid want;
		struct spdk_uuid got;
		uint32_t vb = 0;

		make_uuid(&want, 2);
		rc = s3_chunk_map_lookup(map, 10, &got, &vb);
		check_int("entry 10 is now mapped", rc, 0);
		if (rc == 0) {
			check_true("with the uuid from the snapshot",
				   spdk_uuid_compare(&got, &want) == 0, NULL);
			check_u64("and its valid_bytes", vb, 2 * 4096);
		}
	}
	check_u64("the map's applied LSN is the snapshot's",
		  s3_chunk_map_get_applied_lsn(map), 42);

	/* ---------- [2] no checkpoint is not an error ---------- */
	printf("\n[2] a missing checkpoint means \"replay from the start\"\n");
	g_head_status = -ENOENT;
	{
		struct load_result r2 = {0};

		s3_checkpoint_load((struct s3_client *)0x1, "ckpt_test", map,
				   load_cb, &r2);
		check_int("status is success", r2.status, 0);
		check_u64("with LSN 0", r2.lsn, 0);
	}
	g_head_status = 0;

	/* ---------- [3] the rejections, each with its exact errno ---------- */
	printf("\n[3] corrupting one thing at a time and expecting the refusal\n");
	{
		struct ckpt_blob b;

		/* (a) too short for a header. */
		r = run_load(&blob, 10, map);
		check_int("a body shorter than the header is -EILSEQ",
			  r.status, -EILSEQ);

		/* (b) an implausibly large object. */
		r = run_load(&blob, 8ULL * 1024 * 1024 * 1024, map);
		check_int("an implausible size is -EILSEQ", r.status, -EILSEQ);

		/* (c) bad magic. */
		memcpy(&b, &blob, sizeof(b));
		b.hdr.magic = 0xDEADBEEFDEADBEEFULL;
		r = run_load(&b, sizeof(b), map);
		check_int("a bad magic is -EILSEQ", r.status, -EILSEQ);

		/* (d) unsupported version, CRC recomputed so only this fires. */
		memcpy(&b, &blob, sizeof(b));
		b.hdr.version = 99;
		b.hdr.crc = ckpt_hdr_crc(&b.hdr);
		r = run_load(&b, sizeof(b), map);
		check_int("an unsupported version is -EPROTO", r.status, -EPROTO);

		/* (e) header CRC mismatch. */
		memcpy(&b, &blob, sizeof(b));
		b.hdr.checkpoint_lsn = 43;   /* flip a field, do not touch the crc */
		r = run_load(&b, sizeof(b), map);
		check_int("a header CRC mismatch is -EILSEQ", r.status, -EILSEQ);

		/* (f) geometry mismatch -- every chunk_index would mean something
		 * else. CRC recomputed so only this fires. */
		memcpy(&b, &blob, sizeof(b));
		b.hdr.chunk_size = 512;
		b.hdr.crc = ckpt_hdr_crc(&b.hdr);
		r = run_load(&b, sizeof(b), map);
		check_int("a chunk_size mismatch is -EINVAL", r.status, -EINVAL);

		/* (g) size mismatch -- the header claims three entries but only two
		 * arrive. */
		memcpy(&b, &blob, sizeof(b));
		r = run_load(&b, sizeof(b.hdr) + 2 * sizeof(struct s3_ckpt_entry),
			     map);
		check_int("fewer bytes than the entry count promises is -EILSEQ",
			  r.status, -EILSEQ);

		/* (h) entry CRC mismatch -- the header is fine, one entry is
		 * corrupted. */
		memcpy(&b, &blob, sizeof(b));
		b.entries[1].valid_bytes ^= 0x1;
		r = run_load(&b, sizeof(b), map);
		check_int("an entry CRC mismatch is -EILSEQ", r.status, -EILSEQ);
	}

	s3_chunk_map_destroy(map);

	spdk_log_close();

	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
