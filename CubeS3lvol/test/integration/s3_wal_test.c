/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   WAL verification -- needs neither S3 nor credentials
 *
 *   === What this test proves ===
 *
 *   Current writes live in the process-shared write-back ring. The disk WAL
 *   still has to replay payload records written by pre-shm releases, so this
 *   test installs an old-format fixture directly and verifies that it survives
 *   open and replay byte for byte.
 *
 *     1. format and close the WAL region
 *     2. install old-format writes, zeroes, unmaps and a barrier
 *     3. reopen and replay
 *     5. compare every recovered entry against what was written, payload
 *        included
 *
 *   Beyond that it pins down the properties that are easy to get subtly wrong:
 *
 *     - the A/B super survives and picks the newer slot
 *     - epoch increments on every open, so a stale log cannot masquerade as
 *       current
 *     - payloads are verified by CRC, not just present
 *     - batches close properly, since an unclosed batch is dropped whole (W4)
 *     - the ring crosses a segment boundary and the END sentinel is handled
 *     - backpressure engages rather than silently overrunning
 *
 *   Like the journal test this uses the upstream bdev_aio over a sparse file and
 *   brings up iobuf, accel and bdev by hand, in that order. poll_until() turns
 *   the async API back into sequential flow, which is only safe because this
 *   process has a single thread and no other pollers; production code must not
 *   copy that.
 *
 *   Usage:
 *     ./s3_wal_test [aio-file-path]
 */

#include "spdk/stdinc.h"
#include "spdk/accel.h"
#include "spdk/bdev.h"
#include "spdk/crc32.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_local_dev.h"
#include "s3lvol/s3_wal.h"

#include "bdev/aio/bdev_aio.h"

#define AIO_BDEV_NAME    "s3lvol_waltest0"
#define AIO_BLOCK_SIZE   4096
#define DEFAULT_AIO_PATH "/data/s3lvol_wal_test.aio"

/* Small segments keep the file small while still exercising the ring: three
 * segments is the minimum wal_alloc() accepts, and the geometry reserves two for
 * slack, so 6 segments leaves 4 usable. */
#define TEST_SEG_SIZE  (2 * 1024 * 1024)
#define TEST_WAL_SIZE  (16 * 1024 * 1024)
#define TEST_JOURNAL_SIZE (4 * 1024 * 1024)
#define AIO_FILE_SIZE  (64ULL * 1024 * 1024)

#define TEST_CHUNK_SIZE (1024 * 1024)
#define NUM_BARRIERS 500

static int g_pass;
static int g_fail;
static struct spdk_thread *g_thread;

static void
check(const char *what, int got, int want)
{
	if (got == want) {
		printf("  [PASS] %-52s rc=%d\n", what, got);
		g_pass++;
	} else {
		printf("  [FAIL] %-52s rc=%d (expected %d)\n", what, got, want);
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

/* ==========================================================================
 * Async to sync
 * ========================================================================== */

#define POLL_TIMEOUT_SEC 60

static bool
poll_until(bool *done)
{
	time_t deadline = time(NULL) + POLL_TIMEOUT_SEC;

	while (!*done && time(NULL) < deadline) {
		spdk_thread_poll(g_thread, 0, 0);
	}
	if (!*done) {
		fprintf(stderr, "!! timed out waiting for async completion\n");
	}
	return *done;
}

struct async_ctx {
	bool done;
	int  status;
};

static void
async_int_cb(void *cb_arg, int rc)
{
	struct async_ctx *ctx = cb_arg;

	ctx->status = rc;
	ctx->done   = true;
}

static void
async_void_cb(void *cb_arg)
{
	struct async_ctx *ctx = cb_arg;

	ctx->status = 0;
	ctx->done   = true;
}

static void
bdev_io_done(struct spdk_bdev_io *bdev_io, bool success, void *cb_arg)
{
	struct async_ctx *ctx = cb_arg;

	spdk_bdev_free_io(bdev_io);
	ctx->status = success ? 0 : -EIO;
	ctx->done = true;
}

static int
sync_bdev_write(struct spdk_bdev_desc *desc, struct spdk_io_channel *ch,
		void *buf, uint64_t offset, uint64_t len)
{
	struct async_ctx ctx = {0};
	int rc;

	rc = spdk_bdev_write(desc, ch, buf, offset, len, bdev_io_done, &ctx);
	if (rc != 0) {
		return rc;
	}
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

static int
sync_bdev_flush(struct spdk_bdev_desc *desc, struct spdk_io_channel *ch)
{
	struct async_ctx ctx = {0};
	int rc;

	rc = spdk_bdev_flush(desc, ch, 0, 0, bdev_io_done, &ctx);
	if (rc != 0) {
		return rc;
	}
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

struct open_dev_ctx {
	bool                done;
	int                   status;
	struct s3_local_dev  *dev;
};

static void
local_dev_open_cb(void *cb_arg, struct s3_local_dev *dev, int status)
{
	struct open_dev_ctx *ctx = cb_arg;

	ctx->dev    = dev;
	ctx->status = status;
	ctx->done   = true;
}

struct open_wal_ctx {
	bool           done;
	int            status;
	struct s3_wal *wal;
};

static void
wal_open_cb(void *cb_arg, struct s3_wal *wal, int status)
{
	struct open_wal_ctx *ctx = cb_arg;

	ctx->wal    = wal;
	ctx->status = status;
	ctx->done   = true;
}

/* ==========================================================================
 * aio backing file
 * ========================================================================== */

/* O_EXCL|O_NOFOLLOW because the path is fixed: without NOFOLLOW a pre-planted
 * symlink would let this truncate an unrelated file. */
static int
make_aio_file(const char *path, uint64_t size)
{
	int fd;
	int rc;

	unlink(path);

	fd = open(path, O_RDWR | O_CREAT | O_EXCL | O_NOFOLLOW, 0600);
	if (fd < 0) {
		fprintf(stderr, "could not create %s: %s\n", path, strerror(errno));
		return -errno;
	}

	rc = ftruncate(fd, (off_t)size);
	if (rc != 0) {
		rc = -errno;
		fprintf(stderr, "ftruncate %s failed: %s\n", path, strerror(errno));
		close(fd);
		unlink(path);
		return rc;
	}

	close(fd);
	return 0;
}

/* ==========================================================================
 * Expected entries
 *
 * Payloads are derived from the LBA so recovery can be checked against a
 * recomputed value rather than a table this test kept -- a wrong table would
 * otherwise agree with itself.
 * ========================================================================== */

#define NUM_WRITES 64

/* Frozen pre-shm wire definitions. Do not replace these with current public
 * structs or enums: the fixture must remain unchanged if production format
 * declarations are accidentally edited. */
#define LEGACY_WAL_SUPER_MAGIC   UINT64_C(0x5341573153444B53)
#define LEGACY_WAL_SUPER_VERSION 1u
#define LEGACY_WAL_ENTRY_MAGIC   UINT32_C(0x454C4157)
#define LEGACY_WAL_WRITE         0u
#define LEGACY_WAL_WRITE_ZEROES  1u
#define LEGACY_WAL_UNMAP         2u
#define LEGACY_WAL_BARRIER       3u
#define LEGACY_WAL_LAST_IN_BATCH (1u << 1)

struct legacy_wal_entry_hdr {
	uint32_t magic;
	uint8_t type;
	uint8_t flags;
	uint16_t hdr_len;
	uint64_t seq;
	uint64_t batch_id;
	uint64_t lba;
	uint32_t nblocks;
	uint32_t payload_len;
	uint64_t chunk_index;
	uint32_t crc_hdr;
	uint32_t crc_payload;
	uint8_t reserved[8];
} __attribute__((packed));

struct legacy_wal_super {
	uint64_t magic;
	uint32_t version;
	uint32_t seg_size;
	uint32_t seg_count;
	uint32_t reserved0;
	uint64_t epoch;
	uint64_t head;
	uint64_t tail;
	uint64_t ckpt_head;
	uint64_t ckpt_seq;
	uint64_t seq_next;
	uint64_t slot_gen;
	uint32_t crc;
	uint32_t reserved1;
} __attribute__((packed));

SPDK_STATIC_ASSERT(sizeof(struct legacy_wal_entry_hdr) == 64,
		   "released WAL entry header size changed");
SPDK_STATIC_ASSERT(sizeof(struct legacy_wal_super) == 88,
		   "released WAL super size changed");

static void
fill_payload(void *buf, uint64_t lba, uint32_t len)
{
	uint8_t *p = buf;

	for (uint32_t i = 0; i < len; i++) {
		p[i] = (uint8_t)((lba * 31 + i * 7) & 0xff);
	}
}

static bool
payload_matches(const void *buf, uint64_t lba, uint32_t len)
{
	const uint8_t *p = buf;

	for (uint32_t i = 0; i < len; i++) {
		if (p[i] != (uint8_t)((lba * 31 + i * 7) & 0xff)) {
			return false;
		}
	}
	return true;
}

static uint32_t
fixture_hdr_crc(const struct legacy_wal_entry_hdr *hdr)
{
	struct legacy_wal_entry_hdr tmp = *hdr;

	tmp.crc_hdr = 0;
	return spdk_crc32c_update(&tmp, sizeof(tmp), ~0u);
}

static uint32_t
fixture_super_crc(const struct legacy_wal_super *sb)
{
	struct legacy_wal_super tmp = *sb;

	tmp.crc = 0;
	return spdk_crc32c_update(&tmp, sizeof(tmp), ~0u);
}

static uint8_t *
fixture_add_entry(uint8_t *cursor, uint8_t type, uint64_t seq,
		  uint64_t batch_id, uint64_t lba, uint32_t nblocks,
		  uint64_t chunk_index, bool last)
{
	struct legacy_wal_entry_hdr hdr = {
		.magic = LEGACY_WAL_ENTRY_MAGIC,
		.type = type,
		.flags = last ? LEGACY_WAL_LAST_IN_BATCH : 0,
		.hdr_len = sizeof(hdr),
		.seq = seq,
		.batch_id = batch_id,
		.lba = lba,
		.nblocks = nblocks,
		.payload_len = type == LEGACY_WAL_WRITE ?
			       nblocks * S3_WAL_BLOCK_SIZE : 0,
		.chunk_index = chunk_index,
	};

	if (hdr.payload_len != 0) {
		fill_payload(cursor + sizeof(hdr), lba, hdr.payload_len);
		hdr.crc_payload = spdk_crc32c_update(cursor + sizeof(hdr),
						    hdr.payload_len, ~0u);
	}
	hdr.crc_hdr = fixture_hdr_crc(&hdr);
	memcpy(cursor, &hdr, sizeof(hdr));
	return cursor + sizeof(hdr) + hdr.payload_len;
}

enum fixture_damage {
	FIXTURE_OK,
	FIXTURE_TORN_TAIL,
	FIXTURE_BAD_MAGIC,
	FIXTURE_BAD_HEADER_CRC,
	FIXTURE_BAD_PAYLOAD_CRC,
	FIXTURE_BAD_LENGTH,
	FIXTURE_BAD_FIRST_AFTER_CKPT,
	FIXTURE_BAD_COMMITTED_TAIL,
	FIXTURE_TORN_OPEN_BATCH,
	FIXTURE_YOUNG_FIRST,
	FIXTURE_FORMAT_STALE,
	FIXTURE_NEXT_SEGMENT,
	FIXTURE_NEXT_SEGMENT_OTHER_EPOCH,
};

/* Install bytes produced by the pre-shm WAL format without retaining its
 * obsolete append API in the production library. */
static int
install_legacy_fixture(struct s3_local_dev *local_dev, enum fixture_damage damage)
{
	const struct s3_region *region =
		s3_local_dev_get_region(local_dev, S3_REGION_WAL);
	struct spdk_bdev_desc *desc =
		s3_local_dev_get_desc(local_dev, S3_REGION_WAL);
	struct spdk_io_channel *ch =
		s3_local_dev_get_channel(local_dev, S3_REGION_WAL);
	uint32_t seg_count;
	const size_t first_batch_len =
		SPDK_ALIGN_CEIL(NUM_WRITES *
				(sizeof(struct legacy_wal_entry_hdr) +
				 S3_WAL_BLOCK_SIZE),
				S3_WAL_BLOCK_SIZE);
	const size_t second_batch_len =
		SPDK_ALIGN_CEIL(3 * sizeof(struct legacy_wal_entry_hdr),
				S3_WAL_BLOCK_SIZE);
	const size_t ring_len = first_batch_len + second_batch_len;
	const size_t write_len =
		damage == FIXTURE_NEXT_SEGMENT ||
		damage == FIXTURE_NEXT_SEGMENT_OTHER_EPOCH ?
		TEST_SEG_SIZE + S3_WAL_BLOCK_SIZE :
		damage == FIXTURE_TORN_OPEN_BATCH ?
		SPDK_ALIGN_CEIL(ring_len + sizeof(struct legacy_wal_entry_hdr) +
				S3_WAL_BLOCK_SIZE, S3_WAL_BLOCK_SIZE) :
		ring_len + (damage == FIXTURE_TORN_TAIL ? S3_WAL_BLOCK_SIZE : 0);
	/* Corruption fixtures deliberately leave the persisted head stale. A
	 * valid later batch still proves the damaged bytes are in the live log.
	 * An open torn batch starts at ring_len, which must stay outside head. */
	const uint64_t super_head =
		damage == FIXTURE_TORN_OPEN_BATCH ? ring_len :
		damage == FIXTURE_BAD_COMMITTED_TAIL ? ring_len :
		(damage >= FIXTURE_BAD_MAGIC &&
		 damage != FIXTURE_TORN_OPEN_BATCH ? 0 : ring_len);
	uint8_t *ring = spdk_dma_zmalloc(write_len, S3_WAL_BLOCK_SIZE, NULL);
	uint8_t *supers = spdk_dma_zmalloc(S3_WAL_SUPER_SIZE,
					   S3_WAL_BLOCK_SIZE, NULL);
	uint8_t *cursor;
	uint64_t seq = (UINT64_C(1) << 48) | 1;
	int rc;

	if (!region || !desc || !ch) {
		rc = -EINVAL;
		goto out;
	}
	if (!ring || !supers) {
		rc = -ENOMEM;
		goto out;
	}
	seg_count =
		(uint32_t)((region->size - S3_WAL_SUPER_SIZE) / TEST_SEG_SIZE) - 2;

	cursor = ring;
	if (damage == FIXTURE_YOUNG_FIRST || damage == FIXTURE_FORMAT_STALE ||
	    damage == FIXTURE_NEXT_SEGMENT ||
	    damage == FIXTURE_NEXT_SEGMENT_OTHER_EPOCH) {
		if (damage == FIXTURE_YOUNG_FIRST) {
			(void)fixture_add_entry(ring, LEGACY_WAL_WRITE, seq++, 1,
						0, 1, 0, true);
			((struct legacy_wal_entry_hdr *)ring)->magic ^= 1;
			(void)fixture_add_entry(ring + 2 * S3_WAL_BLOCK_SIZE,
						LEGACY_WAL_BARRIER, seq++, 2,
						0, 0, 0, true);
		} else if (damage != FIXTURE_FORMAT_STALE) {
			(void)fixture_add_entry(ring, LEGACY_WAL_BARRIER, seq++, 1,
						0, 0, 0, true);
			((struct legacy_wal_entry_hdr *)(ring +
							 S3_WAL_BLOCK_SIZE))->magic =
				0x11111111;
			(void)fixture_add_entry(
				ring + TEST_SEG_SIZE, LEGACY_WAL_BARRIER,
				damage == FIXTURE_NEXT_SEGMENT_OTHER_EPOCH ?
				(UINT64_C(3) << 48) | 2 : seq++,
				2, 0, 0, 0, true);
		} else {
			(void)fixture_add_entry(ring + S3_WAL_BLOCK_SIZE,
						LEGACY_WAL_BARRIER,
						(UINT64_C(1) << 48) | 5, 1,
						0, 0, 0, true);
		}
	} else {
	for (uint64_t i = 0; i < NUM_WRITES; i++) {
		cursor = fixture_add_entry(cursor, LEGACY_WAL_WRITE, seq++, 1,
					   i, 1,
					   i / 256, i + 1 == NUM_WRITES);
	}
	cursor = ring + first_batch_len;
	cursor = fixture_add_entry(cursor, LEGACY_WAL_WRITE_ZEROES, seq++, 2,
				   1000, 2, 1000 / 256, false);
	cursor = fixture_add_entry(cursor, LEGACY_WAL_UNMAP, seq++, 2,
				   2000, 4, 2000 / 256, false);
	(void)fixture_add_entry(cursor, LEGACY_WAL_BARRIER, seq++, 2,
				0, 0, 0, true);
	}

	if (damage >= FIXTURE_BAD_MAGIC && damage < FIXTURE_TORN_OPEN_BATCH) {
		const size_t entry_len =
			sizeof(struct legacy_wal_entry_hdr) + S3_WAL_BLOCK_SIZE;
		size_t bad_off = entry_len;
		struct legacy_wal_entry_hdr *bad;

		if (damage == FIXTURE_BAD_FIRST_AFTER_CKPT) {
			bad_off = 0;
		} else if (damage == FIXTURE_BAD_COMMITTED_TAIL) {
			bad_off = first_batch_len +
				  2 * sizeof(struct legacy_wal_entry_hdr);
		}
		bad = (struct legacy_wal_entry_hdr *)(ring + bad_off);

		switch (damage) {
		case FIXTURE_BAD_MAGIC:
			bad->magic ^= 1;
			break;
		case FIXTURE_BAD_HEADER_CRC:
			bad->crc_hdr ^= 1;
			break;
		case FIXTURE_BAD_PAYLOAD_CRC:
			ring[entry_len + sizeof(*bad)] ^= 1;
			break;
		case FIXTURE_BAD_LENGTH:
			bad->payload_len = TEST_SEG_SIZE;
			bad->crc_hdr = fixture_hdr_crc(bad);
			break;
		case FIXTURE_BAD_FIRST_AFTER_CKPT:
		case FIXTURE_BAD_COMMITTED_TAIL:
			bad->magic ^= 1;
			break;
		case FIXTURE_OK:
		case FIXTURE_TORN_TAIL:
		case FIXTURE_TORN_OPEN_BATCH:
		case FIXTURE_YOUNG_FIRST:
		case FIXTURE_FORMAT_STALE:
		case FIXTURE_NEXT_SEGMENT:
		case FIXTURE_NEXT_SEGMENT_OTHER_EPOCH:
			break;
		}
	}

	if (damage == FIXTURE_TORN_TAIL) {
		struct legacy_wal_entry_hdr torn = {
			.magic = LEGACY_WAL_ENTRY_MAGIC,
			.type = LEGACY_WAL_WRITE,
			.hdr_len = sizeof(torn),
			.seq = seq,
			.batch_id = 3,
			.lba = 3000,
			.nblocks = 1,
			.payload_len = S3_WAL_BLOCK_SIZE,
			.chunk_index = 3000 / 256,
			/* Deliberately not the CRC of the partial payload below. */
			.crc_payload = 0x12345678,
		};

		torn.crc_hdr = fixture_hdr_crc(&torn);
		memcpy(ring + ring_len, &torn, sizeof(torn));
		fill_payload(ring + ring_len + sizeof(torn), torn.lba,
			     S3_WAL_BLOCK_SIZE - sizeof(torn));
	}

	if (damage == FIXTURE_TORN_OPEN_BATCH) {
		struct legacy_wal_entry_hdr torn = {
			.magic = LEGACY_WAL_ENTRY_MAGIC,
			.type = LEGACY_WAL_WRITE,
			.hdr_len = sizeof(torn),
			.seq = seq + 1,
			.batch_id = 3,
			.lba = 3000,
			.nblocks = 1,
			.payload_len = S3_WAL_BLOCK_SIZE,
			.chunk_index = 3000 / 256,
			.crc_payload = 0x12345678,
		};
		uint8_t *open_entry;

		open_entry = fixture_add_entry(ring + ring_len,
					       LEGACY_WAL_WRITE_ZEROES, seq, 3,
					       4000, 1, 4000 / 256, false);
		torn.crc_hdr = fixture_hdr_crc(&torn);
		memcpy(open_entry, &torn, sizeof(torn));
		fill_payload(open_entry + sizeof(torn), torn.lba,
			     S3_WAL_BLOCK_SIZE - sizeof(torn));
	}

	for (uint32_t slot = 0; slot < 2; slot++) {
		struct legacy_wal_super sb = {
			.magic = LEGACY_WAL_SUPER_MAGIC,
			.version = LEGACY_WAL_SUPER_VERSION,
			.seg_size = TEST_SEG_SIZE,
			.seg_count = seg_count,
			.epoch = 1,
			.head = super_head,
			.tail = 0,
			.ckpt_head = 0,
			.ckpt_seq = damage == FIXTURE_BAD_FIRST_AFTER_CKPT ?
				    (UINT64_C(1) << 48) : 0,
			.seq_next = 1 + NUM_WRITES + 3,
			.slot_gen = 10 + slot,
		};

		sb.crc = fixture_super_crc(&sb);
		memcpy(supers + slot * S3_WAL_SUPER_SLOT_SIZE, &sb, sizeof(sb));
	}

	rc = sync_bdev_write(desc, ch, ring,
			     region->offset + S3_WAL_SUPER_SIZE, write_len);
	if (rc == 0) {
		rc = sync_bdev_write(desc, ch, supers, region->offset,
				     S3_WAL_SUPER_SIZE);
	}
	if (rc == 0) {
		rc = sync_bdev_flush(desc, ch);
	}
out:
	spdk_dma_free(ring);
	spdk_dma_free(supers);
	return rc;
}

struct replay_state {
	uint64_t writes;
	uint64_t zeroes;
	uint64_t unmaps;
	uint64_t barriers;
	uint64_t payload_ok;
	uint64_t payload_bad;
	uint64_t zero_batch_ids;
	uint64_t out_of_order;
	uint64_t last_seq;
};

static int
replay_apply(void *cb_arg, const struct s3_wal_entry_hdr *hdr, const void *payload)
{
	struct replay_state *st = cb_arg;

	/* seq must come back strictly increasing: the physical scan order is the
	 * append order, and recovery relies on that. */
	if (st->last_seq != 0 && hdr->seq <= st->last_seq) {
		st->out_of_order++;
	}
	st->last_seq = hdr->seq;
	if (hdr->batch_id == 0) {
		st->zero_batch_ids++;
	}

	switch (hdr->type) {
	case S3_WAL_WRITE:
		st->writes++;
		if (payload && payload_matches(payload, hdr->lba, hdr->payload_len)) {
			st->payload_ok++;
		} else {
			st->payload_bad++;
		}
		break;
	case S3_WAL_WRITE_ZEROES:
		st->zeroes++;
		break;
	case S3_WAL_UNMAP:
		st->unmaps++;
		break;
	case S3_WAL_BARRIER:
		st->barriers++;
		break;
	default:
		printf("       !! unexpected entry type %u\n", hdr->type);
		return -EILSEQ;
	}

	return 0;
}

/* ==========================================================================
 * Framework bring-up: iobuf then accel then bdev, and the reverse on the way
 * out. accel is not optional -- every bdev channel acquires one.
 * ========================================================================== */

static int
framework_start(void)
{
	struct spdk_iobuf_opts iobuf_opts;
	struct spdk_bdev_opts bdev_opts;
	struct async_ctx ctx = {0};
	int rc;

	spdk_iobuf_get_opts(&iobuf_opts, sizeof(iobuf_opts));
	iobuf_opts.small_pool_count = 1024;
	iobuf_opts.large_pool_count = 128;
	iobuf_opts.opts_size = sizeof(iobuf_opts);
	rc = spdk_iobuf_set_opts(&iobuf_opts);
	if (rc != 0) {
		return rc;
	}

	rc = spdk_iobuf_initialize();
	if (rc != 0) {
		return rc;
	}

	rc = spdk_accel_initialize();
	if (rc != 0) {
		return rc;
	}

	spdk_bdev_get_opts(&bdev_opts, sizeof(bdev_opts));
	bdev_opts.bdev_io_pool_size = 4096;
	bdev_opts.bdev_auto_examine = false;
	bdev_opts.opts_size = sizeof(bdev_opts);
	rc = spdk_bdev_set_opts(&bdev_opts);
	if (rc != 0) {
		return rc;
	}

	spdk_bdev_initialize(async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

static void
framework_stop(void)
{
	struct async_ctx ctx;

	memset(&ctx, 0, sizeof(ctx));
	spdk_bdev_finish(async_void_cb, &ctx);
	poll_until(&ctx.done);

	memset(&ctx, 0, sizeof(ctx));
	spdk_accel_finish(async_void_cb, &ctx);
	poll_until(&ctx.done);

	memset(&ctx, 0, sizeof(ctx));
	spdk_iobuf_finish(async_void_cb, &ctx);
	poll_until(&ctx.done);
}

/* Synchronous wrappers */

static int
sync_close(struct s3_wal *wal)
{
	struct async_ctx ctx = {0};

	s3_wal_close(wal, async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

static int
sync_replay(struct s3_wal *wal, struct replay_state *st)
{
	struct async_ctx ctx = {0};

	s3_wal_replay(wal, replay_apply, st, async_int_cb, &ctx);
	if (!poll_until(&ctx.done)) {
		return -ETIMEDOUT;
	}
	return ctx.status;
}

/* ==========================================================================
 * main
 * ========================================================================== */

int
main(int argc, char **argv)
{
	struct spdk_env_opts env_opts;
	struct s3_local_dev *local_dev = NULL;
	struct s3_wal *wal = NULL;
	const char *aio_path = DEFAULT_AIO_PATH;
	struct s3_wal_opts wal_opts = { .seg_size = TEST_SEG_SIZE };
	bool file_created = false;
	bool bdev_created = false;
	bool framework_up = false;
	int rc;

	if (argc > 1) {
		aio_path = argv[1];
	}

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== WAL verification (on an aio bdev) ===\n\n");

	env_opts.opts_size = sizeof(env_opts);
	spdk_env_opts_init(&env_opts);
	env_opts.name     = "s3_wal_test";
	env_opts.no_huge  = true;
	env_opts.mem_size = 512;
	if (spdk_env_init(&env_opts) < 0) {
		fprintf(stderr, "spdk_env_init failed\n");
		spdk_log_close();
		return 77;
	}
	if (spdk_thread_lib_init(NULL, 0) != 0) {
		goto out_env;
	}
	g_thread = spdk_thread_create("wal_test", NULL);
	if (!g_thread) {
		goto out_thread_lib;
	}
	spdk_set_thread(g_thread);

	printf("[0] bringing up the SPDK framework (iobuf -> accel -> bdev)\n");
	rc = framework_start();
	check("framework_start", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	framework_up = true;

	printf("\n[1] creating an aio bdev on %s\n", aio_path);
	rc = make_aio_file(aio_path, AIO_FILE_SIZE);
	check("ftruncate the backing file", rc, 0);
	if (rc != 0) {
		goto out_framework;
	}
	file_created = true;

	rc = create_aio_bdev(AIO_BDEV_NAME, aio_path, AIO_BLOCK_SIZE,
			     false, false, NULL, false);
	check("create_aio_bdev", rc, 0);
	if (rc != 0) {
		goto out_file;
	}
	bdev_created = true;

	printf("\n[2] s3_local_dev_format\n");
	{
		struct open_dev_ctx octx = {0};
		struct s3_local_dev_format_opts fopts = {
			.wal_bdev_name  = AIO_BDEV_NAME,
			.lvs_name       = "wal_test",
			.capacity_bytes = 256ULL * 1024 * 1024,
			.chunk_size     = TEST_CHUNK_SIZE,
			.journal_size   = TEST_JOURNAL_SIZE,
			.wal_size       = TEST_WAL_SIZE,
		};

		s3_local_dev_format(&fopts, local_dev_open_cb, &octx);
		if (!poll_until(&octx.done)) {
			goto out_bdev;
		}
		check("s3_local_dev_format", octx.status, 0);
		if (octx.status != 0) {
			goto out_bdev;
		}
		local_dev = octx.dev;
	}

	printf("\n[3] s3_wal_create\n");
	{
		struct open_wal_ctx wctx = {0};

		s3_wal_create(local_dev, &wal_opts, wal_open_cb, &wctx);
		if (!poll_until(&wctx.done)) {
			goto out_local;
		}
		check("s3_wal_create", wctx.status, 0);
		if (wctx.status != 0) {
			goto out_local;
		}
		wal = wctx.wal;
	}

	printf("\n[4] installing a pre-shm compatibility fixture\n");
	rc = sync_close(wal);
	wal = NULL;
	check("close the freshly formatted WAL", rc, 0);
	if (rc == 0) {
		rc = install_legacy_fixture(local_dev, FIXTURE_OK);
	}
	check("install old WRITE/ZEROES/UNMAP records", rc, 0);

	printf("\n[5] reopening and replaying the compatibility fixture\n");
	{
		struct open_wal_ctx wctx = {0};

		s3_wal_open(local_dev, wal_open_cb, &wctx);
		if (!poll_until(&wctx.done)) {
			goto out_local;
		}
		check("s3_wal_open", wctx.status, 0);
		if (wctx.status != 0) {
			goto out_local;
		}
		wal = wctx.wal;
	}

	{
		struct replay_state st = {0};

		rc = sync_replay(wal, &st);
		check("s3_wal_replay", rc, 0);

		char detail[160];
		snprintf(detail, sizeof(detail),
			 "%" PRIu64 " writes, %" PRIu64 " zeroes, %" PRIu64
			 " unmaps, %" PRIu64 " barriers",
			 st.writes, st.zeroes, st.unmaps, st.barriers);
		check_true("every entry came back", st.writes == NUM_WRITES &&
			   st.zeroes == 1 && st.unmaps == 1 && st.barriers == 1,
			   detail);

		snprintf(detail, sizeof(detail), "%" PRIu64 " ok, %" PRIu64 " bad",
			 st.payload_ok, st.payload_bad);
		check_true("every payload survived byte for byte",
			   st.payload_ok == NUM_WRITES && st.payload_bad == 0,
			   detail);

		snprintf(detail, sizeof(detail), "%" PRIu64 " out of order",
			 st.out_of_order);
		check_true("seq came back strictly increasing",
			   st.out_of_order == 0, detail);
	}

	printf("\n[6] appending a current barrier after legacy replay\n");
	{
		uint64_t seq_before = s3_wal_get_next_seq(wal);
		struct async_ctx ctx = {0};

		s3_wal_append_barrier(wal, NULL, async_int_cb, &ctx);
		poll_until(&ctx.done);
		check("append barrier after replay", ctx.status, 0);

		check_true("seq advanced", s3_wal_get_next_seq(wal) > seq_before, NULL);

		rc = sync_close(wal);
		wal = NULL;
		check("close again", rc, 0);

		struct open_wal_ctx wctx = {0};
		s3_wal_open(local_dev, wal_open_cb, &wctx);
		poll_until(&wctx.done);
		if (wctx.status == 0) {
			struct replay_state st = {0};

			wal = wctx.wal;
			sync_replay(wal, &st);

			char detail[128];
			snprintf(detail, sizeof(detail),
				 "%" PRIu64 " legacy writes, %" PRIu64 " barriers",
				 st.writes, st.barriers);
			check_true("the second replay sees legacy data and new barrier",
				   st.writes == NUM_WRITES && st.barriers == 2, detail);
		} else {
			check("reopen after the second close", wctx.status, 0);
		}
	}

	printf("\n[7] crossing a segment boundary with current barriers\n");
	{
		int failed = 0;

		for (uint64_t i = 0; i < NUM_BARRIERS; i++) {
			struct async_ctx ctx = {0};

			s3_wal_append_barrier(wal, NULL, async_int_cb, &ctx);
			if (!poll_until(&ctx.done) || ctx.status != 0) {
				printf("       !! barrier %" PRIu64 " failed: %d\n",
				       i, ctx.status);
				failed++;
				break;
			}
		}
		check_true("barriers succeeded across the boundary",
			   failed == 0, NULL);
	}

	printf("\n[8] replaying across the segment boundary\n");
	{
		rc = sync_close(wal);
		wal = NULL;
		check("close before the boundary replay", rc, 0);

		struct open_wal_ctx wctx = {0};
		s3_wal_open(local_dev, wal_open_cb, &wctx);
		poll_until(&wctx.done);
		check("reopen", wctx.status, 0);

		if (wctx.status == 0) {
			struct replay_state st = {0};

			wal = wctx.wal;
			rc = sync_replay(wal, &st);
			check("replay across the boundary", rc, 0);

			char detail[160];
			snprintf(detail, sizeof(detail),
				 "%" PRIu64 " writes, %" PRIu64 " barriers, %"
				 PRIu64 " bad payloads, %" PRIu64
				 " zero batch IDs",
				 st.writes, st.barriers, st.payload_bad,
				 st.zero_batch_ids);
			check_true("entries survived the segment rollover",
				   st.writes == NUM_WRITES &&
				   st.barriers == 2 + NUM_BARRIERS &&
				   st.payload_bad == 0 &&
				   st.zero_batch_ids == 0, detail);
		}
	}

	printf("\n[9] barriers become no-ops after memory-only sequences start\n");
	if (wal) {
		struct s3_wal_stats before = {0}, after = {0};
		struct async_ctx ctx = {0};
		uint64_t mem_seq, barrier_seq = UINT64_MAX;
		uint64_t next_after_mem;
		uint64_t disk_limit = s3_wal_disk_seq_limit(wal);

		/* Replay bumped the epoch, so disk_seq_limit stays in the
		 * on-disk epoch and never jumps into the new epoch's sequence
		 * space where older shm write-back records would look
		 * checkpointed. */
		s3_wal_get_stats(wal, &before);
		mem_seq = s3_wal_alloc_mem_seq(wal);
		next_after_mem = s3_wal_get_next_seq(wal);
		s3_wal_append_barrier(wal, &barrier_seq, async_int_cb, &ctx);
		s3_wal_get_stats(wal, &after);

		check_true("disk_seq_limit stays below new-epoch sequences",
			   disk_limit != 0 &&
			   (disk_limit >> 48) < (mem_seq >> 48), NULL);
		check_true("memory-only sequence allocated", mem_seq != 0, NULL);
		check_true("memory-mode barrier completes synchronously",
			   ctx.done && ctx.status == 0, NULL);
		check_true("memory-mode barrier has no disk sequence",
			   barrier_seq == 0, NULL);
		check_true("memory-mode barrier consumes no sequence or WAL append",
			   s3_wal_get_next_seq(wal) == next_after_mem &&
			   after.appends == before.appends, NULL);
	}

	if (wal) {
		/* Ignore the status here: teardown, not a check. */
		sync_close(wal);
		wal = NULL;
	}

	printf("\n[10] accepting a torn tail but rejecting middle corruption\n");
	{
		static const struct {
			enum fixture_damage damage;
			const char *name;
		} cases[] = {
			{FIXTURE_BAD_MAGIC, "middle magic corruption"},
			{FIXTURE_BAD_HEADER_CRC, "middle header CRC corruption"},
			{FIXTURE_BAD_PAYLOAD_CRC, "middle payload CRC corruption"},
			{FIXTURE_BAD_LENGTH, "middle length corruption"},
			{FIXTURE_BAD_FIRST_AFTER_CKPT,
			 "first post-checkpoint corruption with a later valid entry"},
			{FIXTURE_BAD_COMMITTED_TAIL,
			 "final corruption below the durable head"},
		};
		struct open_wal_ctx wctx = {0};
		struct replay_state st = {0};

		rc = install_legacy_fixture(local_dev, FIXTURE_TORN_TAIL);
		check("install fixture with torn tail", rc, 0);
		if (rc == 0) {
			s3_wal_open(local_dev, wal_open_cb, &wctx);
			poll_until(&wctx.done);
			check("open fixture with torn tail", wctx.status, 0);
			wal = wctx.wal;
		}
		if (wal) {
			rc = sync_replay(wal, &st);
			check("torn tail is recoverable", rc, 0);
			check_true("committed batches before torn tail survived",
				   st.writes == NUM_WRITES && st.zeroes == 1 &&
				   st.unmaps == 1 && st.barriers == 1, NULL);
			sync_close(wal);
			wal = NULL;
		}

		for (size_t i = 0; i < SPDK_COUNTOF(cases); i++) {
			memset(&wctx, 0, sizeof(wctx));
			memset(&st, 0, sizeof(st));
			rc = install_legacy_fixture(local_dev, cases[i].damage);
			check("install middle-corruption fixture", rc, 0);
			if (rc != 0) {
				continue;
			}
			s3_wal_open(local_dev, wal_open_cb, &wctx);
			poll_until(&wctx.done);
			check("open middle-corruption fixture", wctx.status, 0);
			if (wctx.status != 0) {
				continue;
			}
			wal = wctx.wal;
			rc = sync_replay(wal, &st);
			check(cases[i].name, rc, -EILSEQ);
			sync_close(wal);
			wal = NULL;
		}
	}

	printf("\n[11] torn open batches stay recoverable across super sync\n");
	{
		struct open_wal_ctx wctx = {0};
		struct replay_state st = {0};

		rc = install_legacy_fixture(local_dev, FIXTURE_TORN_OPEN_BATCH);
		check("install open torn batch", rc, 0);
		if (rc == 0) {
			s3_wal_open(local_dev, wal_open_cb, &wctx);
			poll_until(&wctx.done);
			check("open open torn batch", wctx.status, 0);
			wal = wctx.wal;
		}
		if (wal) {
			rc = sync_replay(wal, &st);
			check("first replay drops the open batch", rc, 0);
			check_true("open batch was not applied",
				   st.writes == NUM_WRITES && st.zeroes == 1 &&
				   st.unmaps == 1 && st.barriers == 1, NULL);
			rc = sync_close(wal);
			check("sync head behind the dropped batch", rc, 0);
			wal = NULL;
			memset(&wctx, 0, sizeof(wctx));
			memset(&st, 0, sizeof(st));
			s3_wal_open(local_dev, wal_open_cb, &wctx);
			poll_until(&wctx.done);
			check("reopen after the dropped batch", wctx.status, 0);
			wal = wctx.wal;
			if (wal) {
				rc = sync_replay(wal, &st);
				check("second replay still accepts the torn batch",
				      rc, 0);
				check_true("second replay still drops the open batch",
					   st.writes == NUM_WRITES && st.zeroes == 1,
					   NULL);
				sync_close(wal);
				wal = NULL;
			}
		}
	}

	printf("\n[12] later entries and format leftovers\n");
	{
		static const struct {
			enum fixture_damage damage;
			const char *name;
			int want;
			uint64_t barriers;
		} cases[] = {
			{FIXTURE_YOUNG_FIRST,
			 "corrupt first entry with a later same-epoch entry",
			 -EILSEQ, 0},
			{FIXTURE_FORMAT_STALE,
			 "zero header ignores a later same-epoch leftover",
			 0, 0},
			{FIXTURE_NEXT_SEGMENT,
			 "torn tail followed by the next segment",
			 -EILSEQ, 0},
			{FIXTURE_NEXT_SEGMENT_OTHER_EPOCH,
			 "next segment from another epoch is not corruption",
			 0, 1},
		};

		for (size_t i = 0; i < SPDK_COUNTOF(cases); i++) {
			struct open_wal_ctx wctx = {0};
			struct replay_state st = {0};

			rc = install_legacy_fixture(local_dev, cases[i].damage);
			check("install probe fixture", rc, 0);
			if (rc != 0) {
				continue;
			}
			s3_wal_open(local_dev, wal_open_cb, &wctx);
			poll_until(&wctx.done);
			check("open probe fixture", wctx.status, 0);
			if (wctx.status != 0) {
				continue;
			}
			wal = wctx.wal;
			rc = sync_replay(wal, &st);
			check(cases[i].name, rc, cases[i].want);
			if (cases[i].want == 0) {
				check_true("probe fixture applied only the committed prefix",
					   st.barriers == cases[i].barriers, NULL);
			}
			if (cases[i].damage == FIXTURE_NEXT_SEGMENT_OTHER_EPOCH &&
			    rc == 0) {
				rc = sync_close(wal);
				check("sync after a foreign next segment", rc, 0);
				wal = NULL;
				memset(&wctx, 0, sizeof(wctx));
				memset(&st, 0, sizeof(st));
				s3_wal_open(local_dev, wal_open_cb, &wctx);
				poll_until(&wctx.done);
				check("reopen after a foreign next segment",
				      wctx.status, 0);
				wal = wctx.wal;
				if (wal) {
					rc = sync_replay(wal, &st);
					check("foreign next segment stays a torn tail",
					      rc, 0);
					check_true("the committed prefix is still there",
						   st.barriers == 1, NULL);
				}
			}
			if (wal) {
				sync_close(wal);
				wal = NULL;
			}
		}
	}
out_local:
	if (local_dev) {
		s3_local_dev_close(local_dev);
		local_dev = NULL;
	}
out_bdev:
	if (bdev_created) {
		struct async_ctx ctx = {0};

		bdev_aio_delete(AIO_BDEV_NAME, async_int_cb, &ctx);
		poll_until(&ctx.done);
	}
out_file:
	if (file_created) {
		unlink(aio_path);
	}
out_framework:
	if (framework_up) {
		framework_stop();
	}
	if (g_thread) {
		spdk_thread_exit(g_thread);
		while (!spdk_thread_is_exited(g_thread)) {
			spdk_thread_poll(g_thread, 0, 0);
		}
		spdk_thread_destroy(g_thread);
		spdk_set_thread(NULL);
	}
out_thread_lib:
	spdk_thread_lib_fini();
out_env:
	spdk_env_fini();
	spdk_log_close();

	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
