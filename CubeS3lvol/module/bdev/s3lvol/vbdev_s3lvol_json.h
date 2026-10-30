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
#include "spdk/json.h"

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

/**
 * Decode a JSON array into a right-sized heap allocation.
 *
 * SPDK's array decoder requires the caller to provide a maximum element count.
 * Persisted registries do not have an intrinsic count limit, so using a fixed
 * table makes a valid object permanently unreadable as soon as it grows past
 * that implementation detail. The allocation is retained on decode failure:
 * element decoders may already have allocated fields, and the caller needs the
 * capacity to release every partially decoded slot.
 */
static inline int
s3lvol_json_decode_array_dynamic(const struct spdk_json_val *val,
				 spdk_json_decode_fn decode_func,
				 size_t element_size, void **items,
				 size_t *count, size_t *capacity,
				 int *status)
{
	size_t n;
	int rc;

	*items = NULL;
	*count = 0;
	*capacity = 0;
	*status = 0;

	n = spdk_json_array_count(val);
	if (n > SIZE_MAX / element_size) {
		*status = -EOVERFLOW;
		return -1;
	}
	if (n == 0) {
		rc = spdk_json_decode_array(val, decode_func, NULL, 0, count,
					    element_size);
		*status = rc == 0 ? 0 : -EINVAL;
		return rc;
	}

	*items = calloc(n, element_size);
	if (!*items) {
		*status = -ENOMEM;
		return -1;
	}
	*capacity = n;
	rc = spdk_json_decode_array(val, decode_func, *items, n, count,
				    element_size);
	*status = rc == 0 ? 0 : -EINVAL;
	return rc;
}

#endif /* VBDEV_S3LVOL_JSON_H */
