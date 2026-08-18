/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *Overlay + flusher unit test -- no S3, no credentials, no local bdev
 *
 *   === What this is actually for ===
 *
 *   The centrepiece is section [4]. It reproduces the exact shape of the
 *   data-loss bug of the direct-to-S3 path: eight concurrent 128 KiB writes
 *   landing in one 1 MiB chunk. On the old path each of them
 *   ran its own read-modify-write against a chunk that did not exist yet, so
 *   seven eighths of the data was lost and only the first 128 KiB of every
 *   megabyte survived. Here the same eight writes are accumulated in the overlay
 *   and one flush round has to produce all1 MiB, byte for byte.
 *
 *   The rest covers the properties the flush handshake depends on and that are
 *   easy to get subtly wrong: a write arriving mid-flush must not be dropped, a
 *   failed flush must lose nothing, an unmap must invalidate a flush already
 *   under way, and one chunk must never have two uploads outstanding.
 *
 *   The upload itself is faked. That is on purpose: the scheduling and
 *   bookkeeping is what has the sharp edges, and testing it without a network
 *   makes it runnable in CI. The S3 side is exercised by s3_bs_dev_test.
 *
 *   A real spdk_thread is required because the flusher registers a poller.
 *   DPDK comes up with --no-huge so no hugepages and no root are needed.
 *
 *   Usage:
 *     ./s3_flush_test
 */

#include "spdk/stdinc.h"
#include "spdk/env.h"
#include "spdk/log.h"
#include "spdk/thread.h"

#include "s3lvol/s3_flusher.h"
#include "s3lvol/s3_overlay.h"

#define BLOCK_SIZE 4096
#define CHUNK_SIZE (1024 * 1024)
#define BLOCKS_PER_CHUNK (CHUNK_SIZE / BLOCK_SIZE)
#define TEST_CHUNKS 16
#define TOTAL_BLOCKS ((uint64_t)TEST_CHUNKS * BLOCKS_PER_CHUNK)

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

/* Fill a buffer with a per-block pattern that depends on both the block index
 * and a tag, so a wrong block or a stale version is immediately visible. */
static void
fill_pattern(void *buf, uint32_t nblocks, uint64_t first_block, uint8_t tag)
{
	uint8_t *p = buf;

	for (uint32_t b = 0; b < nblocks; b++) {
		for (uint32_t i = 0; i < BLOCK_SIZE; i++) {
			p[(size_t)b * BLOCK_SIZE + i] =
				(uint8_t)(tag ^ (first_block + b) ^ (i & 0xFF));
		}
	}
}

static bool
check_pattern(const void *buf, uint32_t nblocks, uint64_t first_block, uint8_t tag)
{
	const uint8_t *p = buf;

	for (uint32_t b = 0; b < nblocks; b++) {
		for (uint32_t i = 0; i < BLOCK_SIZE; i++) {
			uint8_t want = (uint8_t)(tag ^ (first_block + b) ^ (i & 0xFF));

			if (p[(size_t)b * BLOCK_SIZE + i] != want) {
				return false;
			}
		}
	}
	return true;
}

static bool
buf_is_zero(const void *buf, size_t len)
{
	const uint8_t *p = buf;

	for (size_t i = 0; i < len; i++) {
		if (p[i] != 0) {
			return false;
		}
	}
	return true;
}

/* ==========================================================================
 * Fake uploader
 *
 * Records what each flush round would have PUT and, unless auto_complete is set,
 * parks the completion so the test controls exactly when a round finishes. That
 * is what makes "write during flush" and "two rounds on one chunk" testable.
 * ========================================================================== */

#define FAKE_MAX_PENDING 32

struct fake_pending {
	uint64_t chunk_index;
	s3_flusher_upload_cb cb;
	void *cb_arg;
};

struct fake_uploader {
	struct s3_overlay *ov;

	struct fake_pending pending[FAKE_MAX_PENDING];
	uint32_t n_pending;

	uint32_t started;
	uint32_t completed;
	uint32_t max_in_flight;

	uint32_t rounds[TEST_CHUNKS];

	/* Merged image of the most recent round per chunk, i.e. what would have
	 * been written to S3, plus its length. */
	uint8_t *image[TEST_CHUNKS];
	uint32_t image_len[TEST_CHUNKS];

	bool auto_complete;
	int forced_status;
};

static void
fake_upload(void *vctx, const struct s3_overlay_flush_view *view,
	    s3_flusher_upload_cb cb, void *cb_arg)
{
	struct fake_uploader *f = vctx;
	uint64_t idx = view->chunk_index;

	f->started++;
	f->rounds[idx]++;

	/* Start from zeroes, as the real path does for a chunk with no existing
	 * object, then let the overlay merge itself in. */
	memset(f->image[idx], 0, CHUNK_SIZE);
	s3_overlay_flush_merge(f->ov, view, f->image[idx]);
	f->image_len[idx] = view->end_offset;

	if (f->auto_complete) {
		f->completed++;
		cb(cb_arg, f->forced_status);
		return;
	}

	assert(f->n_pending < FAKE_MAX_PENDING);
	f->pending[f->n_pending].chunk_index = idx;
	f->pending[f->n_pending].cb = cb;
	f->pending[f->n_pending].cb_arg = cb_arg;
	f->n_pending++;

	if (f->n_pending > f->max_in_flight) {
		f->max_in_flight = f->n_pending;
	}
}

/* Complete every parked round. The list is taken first: a completion kicks the
 * flusher, which may start new rounds and append to it. */
static void
fake_complete_all(struct fake_uploader *f, int status)
{
	struct fake_pending batch[FAKE_MAX_PENDING];
	uint32_t n = f->n_pending;

	memcpy(batch, f->pending, sizeof(batch[0]) * n);
	f->n_pending = 0;

	for (uint32_t i = 0; i < n; i++) {
		f->completed++;
		batch[i].cb(batch[i].cb_arg, status);
	}
}

/* ==========================================================================
 * Sections
 * ========================================================================== */

/* The hold-back policy: when a dirty chunk becomes eligible for flushing.
 *
 * What is worth asserting here is not that a full chunk gets flushed -- that is
 * the easy half -- but the three ways a chunk can leave the queue *early*, and
 * that a partially written chunk does not. Getting the last one wrong is
 * invisible: everything still works, it just uploads a 1 MiB object per 4 KiB
 * write, exactly as before the policy existed.
 */
static void
test_flush_policy(void)
{
	struct s3_overlay *ov = NULL;
	struct s3_overlay_flush_policy policy;
	struct s3_overlay_stats stats;
	uint8_t *blk = malloc(BLOCK_SIZE);
	uint8_t *full = malloc(CHUNK_SIZE);
	uint64_t idx = 0;
	uint64_t forced_before;
	int rc;

	printf("\n[10] flush policy: holding a chunk back\n");

	if (!blk || !full) {
		check_true("malloc", false, NULL);
		goto out;
	}
	memset(blk, 0x5A, BLOCK_SIZE);
	memset(full, 0x5B, CHUNK_SIZE);

	/* A small cap, so that the high water mark can be reached with a couple of
	 * chunks rather than gigabytes: 1% of the default 4 GiB is still 40 MiB. */
	rc = s3_overlay_create(TOTAL_BLOCKS, BLOCK_SIZE, CHUNK_SIZE,
			       4 * 1024 * 1024, &ov);
	check_true("s3_overlay_create", rc == 0 && ov != NULL, NULL);
	if (rc != 0) {
		goto out;
	}

	/* An age long enough that it cannot elapse during the test, so that
	 * anything handed out is handed out for a reason other than age. */
	policy.max_age_us = 3600ULL * 1000 * 1000;
	policy.full_pct   = 100;
	policy.high_pct   = 75;
	s3_overlay_set_flush_policy(ov, &policy);

	/* One block of 256: dirty, but not worth uploading. */
	rc = s3_overlay_write(ov, 0, 1, blk, 1);
	check_true("a one-block write leaves the chunk dirty",
		   rc == 0 && s3_overlay_has_dirty(ov), NULL);
	check_true("next_dirty holds it back with -EAGAIN",
		   s3_overlay_next_dirty(ov, false, &idx) == -EAGAIN, NULL);
	check_true("the chunk is still queued after being held back",
		   s3_overlay_has_dirty(ov), NULL);

	/* force is what a drain uses, and it must override the hold-back --
	 * otherwise unload, checkpoint and export all wait for the age to
	 * elapse and then time out. */
	check_true("force=true hands the same chunk out",
		   s3_overlay_next_dirty(ov, true, &idx) == 0 && idx == 0, NULL);

	/* Filling the chunk completely is the case the policy is waiting for: it
	 * is the one that lets the flush skip reading the old object. */
	rc = s3_overlay_write(ov, BLOCKS_PER_CHUNK, BLOCKS_PER_CHUNK, full, 2);
	check_true("a full-chunk write is accepted", rc == 0, NULL);
	check_true("a full chunk is handed out without force",
		   s3_overlay_next_dirty(ov, false, &idx) == 0 &&
		   idx == 1, NULL);

	s3_overlay_get_stats(ov, &stats);
	check_u64("it was counted as flushed-because-full", stats.flushed_full, 1);
	check_u64("and not as aged", stats.flushed_aged, 0);

	/* A chunk one block short of full stays put: the threshold must round up,
	 * because covers_prefix is all-or-nothing and a threshold that rounded
	 * down would give up the very saving it is waiting for. */
	rc = s3_overlay_write(ov, 2 * BLOCKS_PER_CHUNK, BLOCKS_PER_CHUNK - 1,
			      full, 3);
	check_true("a chunk one block short is held back",
		   rc == 0 &&
		   s3_overlay_next_dirty(ov, false, &idx) == -EAGAIN, NULL);

	/* The high water mark overrides everything: past it, holding data back
	 * only brings s3_overlay_is_full() and retried writes closer. Set it low
	 * enough that what is already held exceeds it. */
	s3_overlay_get_stats(ov, &stats);
	forced_before = stats.flushed_forced;

	policy.high_pct = 1;
	s3_overlay_set_flush_policy(ov, &policy);
	check_true("past the high water mark it is handed out anyway",
		   s3_overlay_next_dirty(ov, false, &idx) == 0 && idx == 2, NULL);

	/* A delta, not an absolute. The force=true call earlier in this section
	 * counts as forced too, deliberately: the three reasons have to add up to
	 * what the flusher reports, so a drain cannot be attributed to nothing. An
	 * absolute here would be asserting how many times this test forced. */
	s3_overlay_get_stats(ov, &stats);
	check_u64("counted as forced", stats.flushed_forced - forced_before, 1);

	/* An empty queue reports -ENOENT rather than -EAGAIN: a caller has to be
	 * able to tell "idle" from "waiting". */
	check_true("an empty queue is -ENOENT, not -EAGAIN",
		   s3_overlay_next_dirty(ov, false, &idx) == -ENOENT, NULL);

	/* A full chunk must not wait behind a partial one.
	 *
	 * This is the case a single dirty-time-ordered queue got wrong, and it cost
	 * 45 seconds of held-back data in practice: writing 32 MiB sequentially left
	 * one chunk of blobstore metadata at the head, two blocks dirty and never
	 * filling, and the 32 full chunks behind it all waited out the age before
	 * anything moved.
	 *
	 * A fresh overlay with room to spare, because the point is that *neither*
	 * escape hatch fires: the age is an hour and the high water mark has to stay
	 * out of reach, which it would not with the few MiB already dirtied above. */
	s3_overlay_destroy(ov);
	ov = NULL;

	rc = s3_overlay_create(TOTAL_BLOCKS, BLOCK_SIZE, CHUNK_SIZE,
			       64 * 1024 * 1024, &ov);
	check_true("a second overlay for the queue-order case", rc == 0, NULL);
	if (rc != 0) {
		goto out;
	}
	policy.high_pct = 75;
	s3_overlay_set_flush_policy(ov, &policy);

	rc = s3_overlay_write(ov, 10 * BLOCKS_PER_CHUNK, 1, blk, 10);
	check_true("a partial chunk is dirtied first", rc == 0, NULL);
	rc = s3_overlay_write(ov, 11 * BLOCKS_PER_CHUNK, BLOCKS_PER_CHUNK, full,
			      11);
	check_true("then a full one behind it", rc == 0, NULL);

	check_true("the full chunk is handed out, not the older partial one",
		   s3_overlay_next_dirty(ov, false, &idx) == 0 && idx == 11, NULL);
	check_true("and the partial one is still held back",
		   s3_overlay_next_dirty(ov, false, &idx) == -EAGAIN, NULL);

	/* Filling a chunk that is *already* queued has to promote it, which is a
	 * different path: enqueue returns early for a chunk it has seen before. */
	rc = s3_overlay_write(ov, 10 * BLOCKS_PER_CHUNK, BLOCKS_PER_CHUNK, full,
			      12);
	check_true("filling an already-queued chunk promotes it",
		   rc == 0 &&
		   s3_overlay_next_dirty(ov, false, &idx) == 0 && idx == 10, NULL);

	s3_overlay_destroy(ov);
out:
	free(blk);
	free(full);
}

static void
test_overlay_basics(void)
{
	struct s3_overlay *ov = NULL;
	struct s3_overlay_stats stats;
	uint8_t *blk = malloc(BLOCK_SIZE);
	uint8_t *out = malloc(2 * BLOCK_SIZE);
	uint64_t idx = 0;
	int rc;

	printf("\n[3] overlay basics\n");

	rc = s3_overlay_create(TOTAL_BLOCKS, BLOCK_SIZE, CHUNK_SIZE, 0, &ov);
	check_true("s3_overlay_create", rc == 0 && ov != NULL, NULL);
	if (rc != 0) {
		goto out;
	}

	check_true("nothing dirty on a fresh overlay",
		   !s3_overlay_has_dirty(ov), NULL);
	check_true("min_seq is UINT64_MAX when empty",
		   s3_overlay_min_seq(ov) == UINT64_MAX, NULL);

	/* One data block. */
	fill_pattern(blk, 1, 0, 0xA1);
	rc = s3_overlay_write(ov, 0, 1, blk, 100);
	check_true("write one block", rc == 0, NULL);
	check_true("that block is covered", s3_overlay_covers(ov, 0, 1), NULL);
	check_true("the next block is not", !s3_overlay_covers(ov, 1, 1), NULL);
	check_u64("bytes held", s3_overlay_get_bytes(ov), BLOCK_SIZE);
	check_u64("min_seq", s3_overlay_min_seq(ov), 100);

	/* apply must replace only the covered block. */
	memset(out, 0x5A, 2 * BLOCK_SIZE);
	check_u64("apply covers one of two blocks",
		  s3_overlay_apply(ov, 0, 2, out), 1);
	check_true("block 0 came from the overlay",
		   check_pattern(out,1, 0, 0xA1), NULL);
	check_true("block 1 was left alone",
		   out[BLOCK_SIZE] == 0x5A, NULL);

	/* A zero record is distinct from "absent": it has to override the base. */
	rc = s3_overlay_write(ov, 1, 1, NULL, 101);
	check_true("write_zeroes record", rc == 0, NULL);
	check_true("the zeroed block is covered", s3_overlay_covers(ov, 1, 1), NULL);
	memset(out, 0x5A, 2 * BLOCK_SIZE);
	s3_overlay_apply(ov, 0, 2, out);
	check_true("apply zeroed block 1", buf_is_zero(out + BLOCK_SIZE, BLOCK_SIZE),
		   NULL);
	check_u64("a zero record costs no memory", s3_overlay_get_bytes(ov),
		  BLOCK_SIZE);

	/* Older writes must not overwrite newer content: this is what makes
	 * replay order independent. */
	fill_pattern(blk, 1, 0, 0xB2);
	rc = s3_overlay_write(ov, 0, 1, blk, 99);
	check_true("stale write accepted without error", rc == 0, NULL);
	memset(out, 0, BLOCK_SIZE);
	s3_overlay_apply(ov, 0, 1, out);
	check_true("stale write did not take effect",
		   check_pattern(out, 1, 0, 0xA1), NULL);

	s3_overlay_get_stats(ov, &stats);
	check_u64("one block dropped as stale", stats.blocks_dropped, 1);

	/* next_dirty hands out the chunk exactly once.
	 *
	 * force=true here and everywhere else in this file: one block of a 1 MiB
	 * chunk is neither full nor 45s old, so the flush policy would hold it
	 * back and these assertions are not about the policy. It has its own
	 * section below. */
	check_true("chunk 0 is queued", s3_overlay_has_dirty(ov), NULL);
	check_true("next_dirty returns chunk 0",
		   s3_overlay_next_dirty(ov, true, &idx) == 0 && idx == 0, NULL);
	check_true("queue is empty afterwards",
		   s3_overlay_next_dirty(ov, true, &idx) == -ENOENT, NULL);

	/* A dropped chunk forgets everything. The epoch is checked in section [5]
	 * instead: it is only comparable while a flush round is open, and with no
	 * round open the chunk struct itself goes away here. */
	{
		s3_overlay_drop_chunk(ov, 0);
		check_true("nothing covered after drop",
			   !s3_overlay_covers(ov, 0, 1), NULL);
		check_u64("no bytes held after drop", s3_overlay_get_bytes(ov), 0);
		check_true("chunk is no longer live",
			   !s3_overlay_chunk_is_live(ov, 0), NULL);
	}

	s3_overlay_destroy(ov);
out:
	free(blk);
	free(out);
}

/* The regression test for the direct-to-S3 lost-update bug. */
static void
test_concurrent_partial_writes(void)
{
	struct s3_overlay *ov = NULL;
	struct s3_overlay_flush_view view;
	uint8_t *piece = malloc(128 * 1024);
	uint8_t *image = malloc(CHUNK_SIZE);
	const uint32_t pieces = 8;
	const uint32_t blocks_per_piece = (128 * 1024) / BLOCK_SIZE;
	bool all_ok = true;
	int rc;

	printf("\n[4] eight concurrent 128 KiB writes into one 1 MiB chunk\n");

	rc = s3_overlay_create(TOTAL_BLOCKS, BLOCK_SIZE, CHUNK_SIZE, 0, &ov);
	if (rc != 0) {
		check_true("s3_overlay_create", false, NULL);
		goto out;
	}

	/* This is what nvmf does to a 1 MiB write: split it into eight commands
	 * that all arrive before any of them completes. */
	for (uint32_t p = 0; p < pieces; p++) {
		uint64_t lba = (uint64_t)p * blocks_per_piece;

		fill_pattern(piece, blocks_per_piece, lba, (uint8_t)(0x10 + p));
		rc = s3_overlay_write(ov, lba, blocks_per_piece, piece,
				      1000 + p);
		if (rc != 0) {
			all_ok = false;
		}
	}
	check_true("all eight writes recorded", all_ok, NULL);
	check_u64("one chunk is live", s3_overlay_get_live_chunks(ov), 1);
	check_u64("1 MiB held", s3_overlay_get_bytes(ov), CHUNK_SIZE);

	rc = s3_overlay_flush_begin(ov, 0, &view);
	check_true("flush_begin", rc == 0, NULL);
	check_u64("one round claims all256 blocks", view.nblocks,
		  BLOCKS_PER_CHUNK);
	check_u64("end_offset is the whole chunk", view.end_offset, CHUNK_SIZE);
	check_true("prefix fully covered, so no GET is needed",
		   view.covers_prefix, NULL);
	check_u64("max_seq is the newest write", view.max_seq, 1000 + pieces - 1);

	/* The assertion that matters: the object about to be written holds every
	 * one of the eight pieces. The old path produced only the first. */
	memset(image, 0, CHUNK_SIZE);
	s3_overlay_flush_merge(ov, &view, image);

	all_ok = true;
	for (uint32_t p = 0; p < pieces; p++) {
		uint64_t lba = (uint64_t)p * blocks_per_piece;

		if (!check_pattern(image + (size_t)lba * BLOCK_SIZE,
				   blocks_per_piece, lba, (uint8_t)(0x10 + p))) {
			printf("\t   piece %u is wrong\n", p);
			all_ok = false;
		}
	}
	check_true("merged object contains all eight pieces", all_ok,
		   "(this is the 7.3b lost update)");

	s3_overlay_flush_end(ov, 0, true);
	check_u64("nothing held after a successful flush",
		  s3_overlay_get_bytes(ov), 0);
	check_true("nothing dirty either", !s3_overlay_has_dirty(ov), NULL);
	check_true("min_seq back to empty",
		   s3_overlay_min_seq(ov) == UINT64_MAX, NULL);

	s3_overlay_destroy(ov);
out:
	free(piece);
	free(image);
}

static void
test_flush_handshake(void)
{
	struct s3_overlay *ov = NULL;
	struct s3_overlay_flush_view view;
	struct s3_overlay_stats stats;
	uint8_t *blk = malloc(BLOCK_SIZE);
	uint8_t *image = malloc(CHUNK_SIZE);
	uint64_t idx = 0;
	int rc;

	printf("\n[5] flush handshake\n");

	rc = s3_overlay_create(TOTAL_BLOCKS, BLOCK_SIZE, CHUNK_SIZE, 0, &ov);
	if (rc != 0) {
		check_true("s3_overlay_create", false, NULL);
		goto out;
	}

	/* --- a write arriving mid-flush must survive it --- */
	fill_pattern(blk, 1, 0, 0xC1);
	s3_overlay_write(ov, 0, 1, blk, 10);
	s3_overlay_next_dirty(ov, true, &idx);
	rc = s3_overlay_flush_begin(ov, 0, &view);
	check_true("flush_begin claims the block", rc == 0, NULL);

	check_true("a second round on the same chunk is refused",
		   s3_overlay_flush_begin(ov, 0, &view) == -EBUSY, NULL);

	fill_pattern(blk, 1, 0, 0xC2);
	s3_overlay_write(ov, 0, 1, blk, 11);

	memset(image, 0, CHUNK_SIZE);
	s3_overlay_flush_merge(ov, &view, image);
	check_true("the round writes the newer version, so no hole is published",
		   check_pattern(image, 1, 0, 0xC2), NULL);

	s3_overlay_flush_end(ov, 0, true);
	check_true("the newer write was not dropped with the round",
		   s3_overlay_covers(ov, 0, 1), NULL);
	check_true("and it is queued again", s3_overlay_has_dirty(ov), NULL);
	memset(image, 0, BLOCK_SIZE);
	s3_overlay_apply(ov, 0, 1, image);
	check_true("reads see the newer version",
		   check_pattern(image, 1, 0, 0xC2), NULL);
	check_u64("min_seq is the surviving write", s3_overlay_min_seq(ov), 11);

	/* --- a failed round must lose nothing --- */
	s3_overlay_next_dirty(ov, true, &idx);
	rc = s3_overlay_flush_begin(ov, 0, &view);
	check_true("flush_begin again", rc == 0, NULL);
	s3_overlay_flush_end(ov, 0, false);
	check_true("data still there after a failed round",
		   s3_overlay_covers(ov, 0, 1), NULL);
	check_true("and re-queued for another attempt",
		   s3_overlay_has_dirty(ov), NULL);
	s3_overlay_get_stats(ov, &stats);
	check_u64("one failure counted", stats.flushes_failed, 1);

	/* --- an unmap during a round must invalidate it --- */
	s3_overlay_next_dirty(ov, true, &idx);
	rc = s3_overlay_flush_begin(ov, 0, &view);
	check_true("flush_begin once more", rc == 0, NULL);
	s3_overlay_drop_chunk(ov, 0);
	check_true("epoch no longer matches the round",
		   s3_overlay_chunk_epoch(ov, 0) != view.epoch,
		   "(the uploader must not publish)");
	s3_overlay_flush_end(ov, 0, true);
	check_true("nothing left after unmap plus flush_end",
		   !s3_overlay_has_dirty(ov), NULL);
	check_u64("no bytes held", s3_overlay_get_bytes(ov), 0);

	/* --- a partial round reports covers_prefix correctly --- */
	fill_pattern(blk, 1, 4, 0xD1);
	s3_overlay_write(ov, 4, 1, blk, 20);
	s3_overlay_next_dirty(ov, true, &idx);
	rc = s3_overlay_flush_begin(ov, 0, &view);
	check_true("flush_begin on a hole", rc == 0, NULL);
	check_u64("end_offset reaches past the written block", view.end_offset,
		  5 * BLOCK_SIZE);
	check_true("covers_prefix is false, so the old object is needed",
		   !view.covers_prefix, NULL);
	s3_overlay_flush_end(ov, 0, true);

	s3_overlay_destroy(ov);
out:
	free(blk);
	free(image);
}

/* Drain completion probe. */
struct drain_probe {
	int status;
	bool done;
};

static void
drain_cb(void *arg, int status)
{
	struct drain_probe *p = arg;

	p->status = status;
	p->done = true;
}

/* Make every dirty block eligible for flushing at once.
 *
 * The flusher sections below are about scheduling -- the concurrency cap, one
 * round per chunk, retry, drain -- and they each dirty a single block. Under the
 * shipped policy that block would be held for 45s and every one of those
 * assertions would see an idle flusher.
 *
 * The high water mark is the deterministic way to say "now": the overlay is
 * created with a 256 KiB cap, so 1% of it is under one block and anything dirty
 * is already past it. An age of 1 us would work too, but it would be a race
 * against how long the test itself takes to reach the kick.
 */
static void
flush_without_holding_back(struct s3_overlay *ov)
{
	struct s3_overlay_flush_policy p = {
		.max_age_us = 3600ULL * 1000 * 1000,
		.full_pct   = 100,
		.high_pct   = 1,
	};

	s3_overlay_set_flush_policy(ov, &p);
}

static void
test_flusher(void)
{
	struct s3_overlay *ov = NULL;
	struct s3_flusher *fl = NULL;
	struct fake_uploader fake;
	struct s3_flusher_opts opts = {};
	struct s3_flusher_stats fstats;
	uint8_t *blk = malloc(BLOCK_SIZE);
	int rc;

	printf("\n[6] flusher scheduling\n");

	memset(&fake, 0, sizeof(fake));
	for (uint32_t i = 0; i < TEST_CHUNKS; i++) {
		fake.image[i] = malloc(CHUNK_SIZE);
	}

	rc = s3_overlay_create(TOTAL_BLOCKS, BLOCK_SIZE, CHUNK_SIZE,
			       256 * 1024, &ov);
	if (rc != 0) {
		check_true("s3_overlay_create", false, NULL);
		goto out;
	}
	flush_without_holding_back(ov);
	fake.ov = ov;

	/* Two at a time, so the limit is observable. */
	opts.max_concurrent = 2;
	opts.poll_period_us = 100;
	rc = s3_flusher_create(ov, NULL, fake_upload, &fake, &opts, &fl);
	check_true("s3_flusher_create", rc == 0 && fl != NULL, NULL);
	if (rc != 0) {
		goto out;
	}

	/* Five dirty chunks, uploads parked so the concurrency cap is visible. */
	for (uint32_t c = 0; c < 5; c++) {
		fill_pattern(blk, 1, c * BLOCKS_PER_CHUNK, (uint8_t)(0x40 + c));
		s3_overlay_write(ov, (uint64_t)c * BLOCKS_PER_CHUNK, 1, blk,
				 200 + c);
	}
	s3_flusher_kick(fl);

	check_u64("only max_concurrent rounds started", fake.started, 2);
	s3_flusher_get_stats(fl, &fstats);
	check_u64("in_flight matches", fstats.in_flight, 2);

	/* Draining the parked rounds must pull the rest through. */
	while (fake.n_pending > 0) {
		fake_complete_all(&fake, 0);
	}
	check_u64("every chunk was uploaded", fake.started, 5);
	check_true("the cap was never exceeded", fake.max_in_flight <= 2,
		   NULL);

	rc = 0;
	for (uint32_t c = 0; c < 5; c++) {
		if (fake.rounds[c] != 1) {
			rc = -1;
		}
		if (!check_pattern(fake.image[c], 1, c * BLOCKS_PER_CHUNK,
				   (uint8_t)(0x40 + c))) {
			rc = -1;
		}
	}
	check_true("one round per chunk, each with the right content", rc == 0,
		   NULL);
	check_true("nothing dirty left", !s3_overlay_has_dirty(ov), NULL);

	/* --- per-chunk single flight --- */
	printf("\n[7] one chunk never has two uploads outstanding\n");
	memset(fake.rounds, 0, sizeof(fake.rounds));
	fake.started = 0;

	fill_pattern(blk, 1, 0, 0x71);
	s3_overlay_write(ov, 0, 1, blk, 300);
	s3_flusher_kick(fl);
	check_u64("first round started", fake.started, 1);

	/* A second write to the same chunk while its upload is outstanding. */
	fill_pattern(blk, 1, 1, 0x72);
	s3_overlay_write(ov, 1, 1, blk, 301);
	s3_flusher_kick(fl);
	check_u64("no second round while the first is in flight", fake.started, 1);

	fake_complete_all(&fake, 0);
	check_u64("the second round starts once the first finishes",
		  fake.started, 2);
	check_u64("both rounds were on the same chunk", fake.rounds[0], 2);

	fake_complete_all(&fake, 0);
	check_true("chunk is clean now", !s3_overlay_has_dirty(ov), NULL);

	/* --- a failed upload is retried --- */
	printf("\n[8] a failed upload is retried, not dropped\n");
	memset(fake.rounds, 0, sizeof(fake.rounds));
	fake.started = 0;

	fill_pattern(blk, 1, 2 * BLOCKS_PER_CHUNK, 0x81);
	s3_overlay_write(ov, 2 * BLOCKS_PER_CHUNK, 1, blk, 400);
	s3_flusher_kick(fl);
	fake_complete_all(&fake, -EIO);

	/* The completion path kicks the flusher, so the retry is already running by
	 * the time we get here -- the chunk is in flight again rather than queued. */
	check_u64("a new round started right away", fake.started, 2);
	s3_flusher_get_stats(fl, &fstats);
	check_u64("failure counted", fstats.failures, 1);

	fake_complete_all(&fake, 0);
	check_u64("retried once", fake.rounds[2], 2);
	check_true("and it is clean now", !s3_overlay_has_dirty(ov), NULL);
	check_true("the retried upload still had the data",
		   check_pattern(fake.image[2], 1, 2 * BLOCKS_PER_CHUNK, 0x81),
		   NULL);

	/* --- drain --- */
	printf("\n[9] drain waits for everything to reach S3\n");
	{
		struct drain_probe probe = { .status = -1, .done = false };

		fill_pattern(blk, 1, 3 * BLOCKS_PER_CHUNK, 0x91);
		s3_overlay_write(ov, 3 * BLOCKS_PER_CHUNK, 1, blk, 500);

		s3_flusher_drain(fl, 0, drain_cb, &probe);

		check_true("drain does not complete while work is outstanding",
			   !probe.done, NULL);

		while (fake.n_pending > 0) {
			fake_complete_all(&fake, 0);
		}

		check_true("drain completed", probe.done, NULL);
		check_true("drain reported success", probe.status == 0, NULL);
	}

	s3_flusher_destroy(fl);
	s3_overlay_destroy(ov);

out:
	free(blk);
	for (uint32_t i = 0; i < TEST_CHUNKS; i++) {
		free(fake.image[i]);
	}
}

/* ==========================================================================
 * main
 * ========================================================================== */

int
main(void)
{
	struct spdk_env_opts opts;
	struct spdk_thread *thread = NULL;
	int rc;

	spdk_log_set_print_level(SPDK_LOG_NOTICE);
	spdk_log_open(NULL);

	printf("=== overlay + flusher unit test ===\n\n");

	printf("[1] SPDK env (--no-huge)\n");
	opts.opts_size = sizeof(opts);
	spdk_env_opts_init(&opts);
	opts.name = "s3_flush_test";
	opts.no_huge = true;
	opts.mem_size = 64;

	if (spdk_env_init(&opts) < 0) {
		fprintf(stderr, "spdk_env_init failed; skipping.\n");
		spdk_log_close();
		return 77;   /* automake convention for "skipped" */
	}
	check_true("spdk_env_init", true, "no_huge=true");

	printf("\n[2] spdk_thread for the flusher poller\n");
	rc = spdk_thread_lib_init(NULL, 0);
	check_true("spdk_thread_lib_init", rc == 0, NULL);
	if (rc != 0) {
		goto out_env;
	}

	thread = spdk_thread_create("flush_test", NULL);
	check_true("spdk_thread_create", thread != NULL, NULL);
	if (!thread) {
		goto out_thread_lib;
	}
	spdk_set_thread(thread);

	test_overlay_basics();
	test_concurrent_partial_writes();
	test_flush_handshake();
	test_flusher();
	test_flush_policy();

	spdk_thread_exit(thread);
	while (!spdk_thread_is_exited(thread)) {
		spdk_thread_poll(thread, 0, 0);
	}
	spdk_thread_destroy(thread);
	thread = NULL;

out_thread_lib:
	spdk_thread_lib_fini();
out_env:
	spdk_env_fini();

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	spdk_log_close();

	return g_fail == 0 ? 0 : 1;
}
