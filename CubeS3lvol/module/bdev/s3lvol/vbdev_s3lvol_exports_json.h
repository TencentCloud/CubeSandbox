/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#ifndef VBDEV_S3LVOL_EXPORTS_JSON_H
#define VBDEV_S3LVOL_EXPORTS_JSON_H

#include "spdk/util.h"

#include "s3lvol/s3_export.h"
#include "vbdev_s3lvol_json.h"

struct src_leases_holder {
	char  *k[S3_EXPORT_MAX_SOURCES];
	size_t n;
};

struct export_entry_json {
	char    *export_uuid;
	char    *snapshot;
	char    *snapshot_uuid;
	char    *layout;
	uint64_t blob_id;
	uint64_t expires_at;
	uint32_t generation;
	bool     lease_aware;
	struct src_leases_holder src_leases;
};

struct export_entries_holder {
	struct export_entry_json *e;
	size_t                    n;
	size_t                    capacity;
	int                       status;
};

struct exports_json {
	uint32_t                     version;
	struct export_entries_holder entries;
};

static inline int
s3lvol_decode_src_leases(const struct spdk_json_val *val, void *out)
{
	struct src_leases_holder *h = out;

	return spdk_json_decode_array(val, spdk_json_decode_string, h->k,
				      S3_EXPORT_MAX_SOURCES, &h->n, sizeof(h->k[0]));
}

static const struct spdk_json_object_decoder s3lvol_export_entry_decoders[] = {
	{"export_uuid", offsetof(struct export_entry_json, export_uuid), spdk_json_decode_string, false},
	{"snapshot", offsetof(struct export_entry_json, snapshot), spdk_json_decode_string, false},
	{"snapshot_uuid", offsetof(struct export_entry_json, snapshot_uuid), spdk_json_decode_string, true},
	{"layout", offsetof(struct export_entry_json, layout), spdk_json_decode_string, false},
	{"blob_id", offsetof(struct export_entry_json, blob_id), spdk_json_decode_uint64, true},
	{"expires_at", offsetof(struct export_entry_json, expires_at), spdk_json_decode_uint64, true},
	{"generation", offsetof(struct export_entry_json, generation), spdk_json_decode_uint32, true},
	{"lease_aware", offsetof(struct export_entry_json, lease_aware), spdk_json_decode_bool, true},
	{"src_leases", offsetof(struct export_entry_json, src_leases), s3lvol_decode_src_leases, true},
};

static inline int
s3lvol_decode_export_entry(const struct spdk_json_val *val, void *out)
{
	return spdk_json_decode_object(val, s3lvol_export_entry_decoders,
				       SPDK_COUNTOF(s3lvol_export_entry_decoders), out);
}

static inline int
s3lvol_decode_export_entries(const struct spdk_json_val *val, void *out)
{
	struct export_entries_holder *h = out;

	return s3lvol_json_decode_array_dynamic(val, s3lvol_decode_export_entry,
					       sizeof(h->e[0]), (void **)&h->e,
					       &h->n, &h->capacity, &h->status);
}

static const struct spdk_json_object_decoder s3lvol_exports_decoders[] = {
	{"version", offsetof(struct exports_json, version), spdk_json_decode_uint32, false},
	{"exports", offsetof(struct exports_json, entries), s3lvol_decode_export_entries, false},
};

static inline int
s3lvol_exports_json_validate(const struct exports_json *json)
{
	size_t i;

	if (json->version != S3_EXPORT_VERSION) {
		return -EPROTONOSUPPORT;
	}
	for (i = 0; i < json->entries.n; i++) {
		const char *layout = json->entries.e[i].layout;

		if (strcmp(layout, S3_EXPORT_LAYOUT_REF_STR) != 0 &&
		    strcmp(layout, S3_EXPORT_LAYOUT_DENSE_STR) != 0) {
			return -EINVAL;
		}
	}
	return 0;
}

static inline void
s3lvol_exports_json_free(struct exports_json *json)
{
	size_t i;

	for (i = 0; i < json->entries.capacity; i++) {
		size_t k;

		free(json->entries.e[i].export_uuid);
		free(json->entries.e[i].snapshot);
		free(json->entries.e[i].snapshot_uuid);
		free(json->entries.e[i].layout);
		for (k = 0; k < json->entries.e[i].src_leases.n; k++) {
			free(json->entries.e[i].src_leases.k[k]);
		}
	}
	free(json->entries.e);
	memset(&json->entries, 0, sizeof(json->entries));
}

#endif /* VBDEV_S3LVOL_EXPORTS_JSON_H */
