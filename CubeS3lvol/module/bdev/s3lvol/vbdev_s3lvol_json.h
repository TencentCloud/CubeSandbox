/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Growable buffer for spdk_json_write_ctx.
 *
 *   spdk_json_write_begin() writes through a callback and does not offer a "into
 *   a string" mode, so every producer of a JSON object here needs the same six
 *   lines. There are three of them now (import registry, export registry, the
 *   export manifest), which was two too many copies of a realloc loop.
 */

#ifndef VBDEV_S3LVOL_JSON_H
#define VBDEV_S3LVOL_JSON_H

#include "spdk/stdinc.h"

struct s3lvol_json_buf {
	char   *data;
	size_t  len;
	size_t  cap;
};

/**
 * spdk_json_write_cb over a malloc'd buffer. The result is NUL terminated, so it
 * can be handed to anything expecting a string as well as to s3_put() with an
 * explicit length. The caller frees \c buf->data.
 */
static inline int
s3lvol_json_buf_append(void *cb_ctx, const void *data, size_t size)
{
	struct s3lvol_json_buf *buf = cb_ctx;

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

#endif /* VBDEV_S3LVOL_JSON_H */
