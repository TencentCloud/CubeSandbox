/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#include "spdk/stdinc.h"
#include "spdk/json.h"

#include "vbdev_s3lvol.h"

static int g_pass;
static int g_fail;

static void
expect(const char *name, bool ok)
{
	printf("\t[%s] %s\n", ok ? "PASS" : "FAIL", name);
	ok ? g_pass++ : g_fail++;
}

static bool
valid_json(const char *text)
{
	return spdk_json_parse((void *)text, strlen(text), NULL, 0, NULL, 0) > 0;
}

int
main(void)
{
	char dir[] = "/tmp/s3lvol_bstore.XXXXXX";
	char moved[96], path[128];
	char *text, *before;
	int rc;

	printf("=== s3_bstore_test ===\n");
	if (!mkdtemp(dir)) {
		perror("mkdtemp");
		return 1;
	}
	snprintf(path, sizeof(path), "%s/bstore.json", dir);
	setenv("S3LVOL_BSTORE_FILE", path, 1);

	rc = bstore_save_entry("lv\"\\\n", "bs\"\\", "bucket\nx", "wal\\x");
	text = s3lvol_statefile_read(path);
	expect("writer escapes names and fields",
	       rc == 0 && text && valid_json(text) &&
	       strstr(text, "lv\\\"\\\\\\n") != NULL &&
	       strstr(text, "bucket\\nx") != NULL);
	free(text);

	rc = bstore_save_entry("second", "bstore_2", "bucket2", "wal2");
	text = s3lvol_statefile_read(path);
	expect("parser preserves escaped existing entry",
	       rc == 0 && text && valid_json(text) &&
	       strstr(text, "lv\\\"\\\\\\n") != NULL &&
	       strstr(text, "\"second\"") != NULL);
	free(text);

	before = strdup("{\"broken\":");
	s3lvol_statefile_write(path, before);
	rc = bstore_save_entry("third", "bstore_3", "bucket3", "wal3");
	text = s3lvol_statefile_read(path);
	expect("corrupt registry is not overwritten",
	       rc == -EINVAL && text && strcmp(text, before) == 0);
	free(text);
	free(before);

	s3lvol_statefile_write(path, "x");
	rc = bstore_save_entry("third", "bstore_3", "bucket3", "wal3");
	expect("short corrupt registry is not treated as absent", rc == -EINVAL);

	snprintf(moved, sizeof(moved), "%s.moved", dir);
	rc = rename(dir, moved);
	rc = rc == 0 ? bstore_save_entry("third", "bstore_3", "bucket3", "wal3") : rc;
	expect("statefile write failure propagates", rc != 0);

	unlink(path);
	snprintf(path, sizeof(path), "%s/bstore.json", moved);
	unlink(path);
	rmdir(moved);
	printf("=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
