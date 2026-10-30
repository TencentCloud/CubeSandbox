/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/* Persisted registries are not bounded in-memory APIs. */

#include "spdk/stdinc.h"
#include "spdk/json.h"

#include "vbdev_s3lvol_exports_json.h"
#include "vbdev_s3lvol_imports_json.h"

#define NUM_ENTRIES 364

static int g_pass;
static int g_fail;

static void
result(const char *name, bool ok)
{
	printf("\t[%s] %s\n", ok ? "PASS" : "FAIL", name);
	if (ok) {
		g_pass++;
	} else {
		g_fail++;
	}
}

static int
build_exports(size_t num_entries, uint32_t version, bool malformed_last,
	      const char *layout, char **json_out, size_t *len_out)
{
	size_t cap = num_entries * 256 + 64;
	char *json;
	size_t len = 0;
	size_t i;
	int rc;

	json = calloc(1, cap);
	if (!json) {
		return -ENOMEM;
	}
	rc = snprintf(json, cap, "{\"version\":%" PRIu32 ",\"exports\":[", version);
	if (rc < 0 || (size_t)rc >= cap) {
		free(json);
		return -EOVERFLOW;
	}
	len = (size_t)rc;
	for (i = 0; i < num_entries; i++) {
		const char *layout_field = malformed_last && i + 1 == num_entries ?
					   "" : ",\"layout\":\"%s\"";

		rc = snprintf(json + len, cap - len,
			      "%s{\"export_uuid\":\"export-%zu\","
			      "\"snapshot\":\"snapshot-%zu\"",
			      i == 0 ? "" : ",", i, i);
		if (rc < 0 || (size_t)rc >= cap - len) {
			free(json);
			return -EOVERFLOW;
		}
		len += (size_t)rc;
		if (layout_field[0] != '\0') {
			rc = snprintf(json + len, cap - len, layout_field, layout);
			if (rc < 0 || (size_t)rc >= cap - len) {
				free(json);
				return -EOVERFLOW;
			}
			len += (size_t)rc;
		}
		rc = snprintf(json + len, cap - len,
			      ",\"blob_id\":%zu,\"src_leases\":[\"lease-%zu\"]}",
			      i, i);
		if (rc < 0 || (size_t)rc >= cap - len) {
			free(json);
			return -EOVERFLOW;
		}
		len += (size_t)rc;
	}
	rc = snprintf(json + len, cap - len, "]}");
	if (rc < 0 || (size_t)rc >= cap - len) {
		free(json);
		return -EOVERFLOW;
	}
	len += (size_t)rc;
	*json_out = json;
	*len_out = len;
	return 0;
}

static int
build_imports(size_t num_entries, uint32_t version, bool malformed_last,
	      char **json_out, size_t *len_out)
{
	size_t cap = num_entries * 128 + 64, len;
	char *json = calloc(1, cap);
	size_t i;
	int rc;

	if (!json) {
		return -ENOMEM;
	}
	rc = snprintf(json, cap, "{\"version\":%" PRIu32 ",\"imports\":[", version);
	if (rc < 0 || (size_t)rc >= cap) {
		free(json);
		return -EOVERFLOW;
	}
	len = (size_t)rc;
	for (i = 0; i < num_entries; i++) {
		rc = snprintf(json + len, cap - len,
			      "%s{\"path_style\":true,\"verify_tls\":false%s}",
			      i == 0 ? "" : ",",
			      malformed_last && i + 1 == num_entries ?
			      "" : ",\"manifest\":\"manifest\"");
		if (rc < 0 || (size_t)rc >= cap - len) {
			free(json);
			return -EOVERFLOW;
		}
		len += (size_t)rc;
	}
	rc = snprintf(json + len, cap - len, "]}");
	if (rc < 0 || (size_t)rc >= cap - len) {
		free(json);
		return -EOVERFLOW;
	}
	*json_out = json;
	*len_out = len + (size_t)rc;
	return 0;
}

static int
parse_values(char *json, size_t json_len, struct spdk_json_val **values_out)
{
	struct spdk_json_val *values;
	ssize_t num_values;

	num_values = spdk_json_parse(json, json_len, NULL, 0, NULL, 0);
	if (num_values <= 0) {
		return -EINVAL;
	}
	values = calloc((size_t)num_values, sizeof(*values));
	if (!values) {
		return -ENOMEM;
	}
	num_values = spdk_json_parse(json, json_len, values, (size_t)num_values,
				     NULL, SPDK_JSON_PARSE_FLAG_DECODE_IN_PLACE);
	if (num_values <= 0) {
		free(values);
		return -EINVAL;
	}
	*values_out = values;
	return 0;
}

static bool
decode_exports(size_t expected, uint32_t version, bool malformed,
	       const char *layout)
{
	struct exports_json decoded = {0};
	struct spdk_json_val *values = NULL;
	char *json = NULL;
	size_t json_len;
	int rc;

	rc = build_exports(expected, version, malformed, layout, &json, &json_len);
	if (rc != 0 || parse_values(json, json_len, &values) != 0) {
		goto out;
	}
	rc = spdk_json_decode_object(values, s3lvol_exports_decoders,
				     SPDK_COUNTOF(s3lvol_exports_decoders), &decoded);
	if (malformed) {
		rc = rc != 0 && decoded.entries.status == -EINVAL ? 0 : -EINVAL;
		goto out;
	}
	if (rc != 0 || decoded.entries.status != 0 ||
	    decoded.entries.n != expected || decoded.entries.capacity != expected) {
		rc = -EINVAL;
		goto out;
	}
	rc = s3lvol_exports_json_validate(&decoded);

out:
	s3lvol_exports_json_free(&decoded);
	free(values);
	free(json);
	return rc == 0;
}

static bool
decode_imports(size_t expected, uint32_t version, bool malformed)
{
	struct imports_json decoded = {0};
	struct spdk_json_val *values = NULL;
	char *json = NULL;
	size_t json_len;
	int rc;

	rc = build_imports(expected, version, malformed, &json, &json_len);
	if (rc != 0 || parse_values(json, json_len, &values) != 0) {
		goto out;
	}
	rc = spdk_json_decode_object(values, s3lvol_imports_decoders,
				     SPDK_COUNTOF(s3lvol_imports_decoders), &decoded);
	if (malformed) {
		rc = rc != 0 && decoded.entries.status == -EINVAL ? 0 : -EINVAL;
		goto out;
	}
	if (rc != 0 || decoded.entries.status != 0 ||
	    decoded.entries.n != expected || decoded.entries.capacity != expected) {
		rc = -EINVAL;
		goto out;
	}
	rc = s3lvol_imports_json_validate(&decoded);

out:
	s3lvol_imports_json_free(&decoded);
	free(values);
	free(json);
	return rc == 0;
}

int
main(void)
{
	static const size_t counts[] = {0, 64, 65, NUM_ENTRIES};
	char name[64];
	size_t i;

	printf("=== s3_dynamic_json_test ===\n");
	for (i = 0; i < SPDK_COUNTOF(counts); i++) {
		snprintf(name, sizeof(name), "exports decode %zu entries", counts[i]);
		result(name, decode_exports(counts[i], S3_EXPORT_VERSION, false, "ref"));
		snprintf(name, sizeof(name), "imports decode %zu entries", counts[i]);
		result(name, decode_imports(counts[i], S3_EXPORT_VERSION, false));
	}
	result("exports partial decode cleanup",
	       decode_exports(NUM_ENTRIES, S3_EXPORT_VERSION, true, "ref"));
	result("imports partial decode cleanup",
	       decode_imports(NUM_ENTRIES, S3_EXPORT_VERSION, true));
	result("exports reject unsupported version",
	       !decode_exports(1, S3_EXPORT_VERSION + 1, false, "ref"));
	result("imports reject unsupported version",
	       !decode_imports(1, S3_EXPORT_VERSION + 1, false));
	result("exports reject unknown layout",
	       !decode_exports(1, S3_EXPORT_VERSION, false, "mystery"));
	printf("=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
