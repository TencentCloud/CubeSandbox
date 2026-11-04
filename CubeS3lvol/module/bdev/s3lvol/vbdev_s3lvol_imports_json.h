/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#ifndef VBDEV_S3LVOL_IMPORTS_JSON_H
#define VBDEV_S3LVOL_IMPORTS_JSON_H

#include "spdk/util.h"

#include "s3lvol/s3_export.h"
#include "vbdev_s3lvol_json.h"

struct import_entry_json {
	bool  path_style;
	bool  verify_tls;
	char *manifest;
};

struct import_entries_holder {
	struct import_entry_json *e;
	size_t                    n;
	size_t                    capacity;
	int                       status;
};

struct imports_json {
	uint32_t                     version;
	struct import_entries_holder entries;
};

static const struct spdk_json_object_decoder s3lvol_import_entry_decoders[] = {
	{"path_style", offsetof(struct import_entry_json, path_style), spdk_json_decode_bool, true},
	{"verify_tls", offsetof(struct import_entry_json, verify_tls), spdk_json_decode_bool, true},
	{"manifest", offsetof(struct import_entry_json, manifest), spdk_json_decode_string, false},
};

static inline int
s3lvol_decode_import_entry(const struct spdk_json_val *val, void *out)
{
	return spdk_json_decode_object(val, s3lvol_import_entry_decoders,
				       SPDK_COUNTOF(s3lvol_import_entry_decoders), out);
}

static inline int
s3lvol_decode_import_entries(const struct spdk_json_val *val, void *out)
{
	struct import_entries_holder *h = out;

	return s3lvol_json_decode_array_dynamic(val, s3lvol_decode_import_entry,
					       sizeof(h->e[0]), (void **)&h->e,
					       &h->n, &h->capacity, &h->status);
}

static const struct spdk_json_object_decoder s3lvol_imports_decoders[] = {
	{"version", offsetof(struct imports_json, version), spdk_json_decode_uint32, false},
	{"imports", offsetof(struct imports_json, entries), s3lvol_decode_import_entries, false},
};

static inline int
s3lvol_imports_json_validate(const struct imports_json *json)
{
	return json->version == S3_EXPORT_VERSION ? 0 : -EPROTONOSUPPORT;
}

static inline void
s3lvol_imports_json_free(struct imports_json *json)
{
	size_t i;

	for (i = 0; i < json->entries.capacity; i++) {
		free(json->entries.e[i].manifest);
	}
	free(json->entries.e);
	memset(&json->entries, 0, sizeof(json->entries));
}

#endif /* VBDEV_S3LVOL_IMPORTS_JSON_H */
