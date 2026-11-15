/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/* Exercise the real private release/retry chain; no S3 credentials required. */
#include "../../lib/s3bsdev/s3_bs_dev.c"

#define TEST_BLOCK 4096
static unsigned retry_calls;
static uint8_t new_data[TEST_BLOCK];

static void
publish_block(struct s3_ctx *ctx, const void *data, uint64_t wal_seq)
{
	struct s3_writeback_wal_op *op = calloc(1, sizeof(*op));
	uint64_t seq;
	assert(op != NULL);
	op->ctx = ctx;
	assert(s3_writeback_publish(ctx->writeback, S3_WRITEBACK_WRITE,
		0, 1, data, TEST_BLOCK, 0, &seq, &op->record) == 0);
	op->writeback_seq = seq;
	op->overlay_refs = 1;
	op->wal_finished = true;
	op->submitting = true;
	assert(s3_overlay_write_durable_ref(ctx->overlay, 0, 1,
		op->record.payload, wal_seq, s3_writeback_overlay_release, op) == 0);
	op->submitting = false;
	op->overlay_published = true;
	s3_writeback_op_maybe_release(op);
}

static void
retry_write(void *arg)
{
	retry_calls++;
	publish_block(arg, new_data, 2);
}

int main(void)
{
	struct s3_ctx ctx = {};
	struct s3_overlay_flush_view view;
	struct s3_retry_item *item;
	struct spdk_env_opts env;
	struct s3_writeback_opts opts = {
		.capacity_bytes = 64 * 1024,
		.record_alignment = 64,
		.flags = S3_WRITEBACK_OPEN_CREATE,
	};
	char name[96];
	uint8_t old_data[TEST_BLOCK], readback[TEST_BLOCK];

	env.opts_size = sizeof(env);
	spdk_env_opts_init(&env);
	env.name = "s3_overlay_retry_test";
	env.no_huge = true;
	env.mem_size = 64;
	if (spdk_env_init(&env) < 0) return 77;
	snprintf(name, sizeof(name), "overlay-retry-%ld", (long)getpid());
	opts.lvstore_name = name;
	assert(s3_writeback_open(&opts, &ctx.writeback) == 0);
	assert(s3_overlay_create(256, TEST_BLOCK, 1024 * 1024, 0,
		&ctx.overlay) == 0);
	STAILQ_INIT(&ctx.retry_q);
	memset(old_data, 0xA1, sizeof(old_data));
	memset(new_data, 0xB2, sizeof(new_data));
	publish_block(&ctx, old_data, 1);
	assert(s3_overlay_flush_begin(ctx.overlay, 0, &view) == 0);
	/* A backpressured write is already queued when the flush completes. */
	item = calloc(1, sizeof(*item));
	assert(item != NULL);
	item->retry_fn = retry_write;
	item->arg = &ctx;
	STAILQ_INSERT_TAIL(&ctx.retry_q, item, link);
	ctx.retry_count = 1;
	s3_overlay_flush_end(ctx.overlay, 0, true);
	/* Retrying inside flush_end re-enters a half-released overlay block. */
	assert(retry_calls == 0);
	s3_retry_run(&ctx); /* the next retry-poller turn */
	assert(retry_calls == 1 && ctx.retry_count == 0);
	assert(s3_overlay_apply(ctx.overlay, 0, 1, readback) == 1);
	assert(memcmp(readback, new_data, TEST_BLOCK) == 0);
	assert(s3_overlay_flush_begin(ctx.overlay, 0, &view) == 0);
	s3_overlay_flush_end(ctx.overlay, 0, true);
	assert(s3_overlay_get_bytes(ctx.overlay) == 0);
	s3_overlay_destroy(ctx.overlay);
	s3_writeback_close(ctx.writeback);
	assert(s3_writeback_cleanup(name) == 0);
	spdk_env_fini();
	puts("PASS: overlay release defers retry; replacement data survives");
	return 0;
}
