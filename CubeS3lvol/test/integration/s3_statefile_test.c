/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   State file crash-safety check -- no S3, no credentials, no DPDK, no threads
 *
 *   === What this test proves ===
 *
 *   vbdev_s3lvol_statefile.c exists for one reader: recovery after a crash. The
 *   files it writes (/data/cubelet/rcow/bstore.json and
 *   /data/cubelet/rcow/active_lvols) are
 *   JSON, and a torn in-place write leaves a *prefix* of a JSON object -- which
 *   is a syntax error, not an object with one entry missing. The whole point of
 *   the temp-file + fsync + rename + dir-fsync sequence is that a reader can
 *   never see that state.
 *
 *   The interesting assertions are therefore not "write then read returns the
 *   bytes" -- that either works or fails obviously -- but the two crash shapes
 *   that would otherwise go unnoticed:
 *
 *     - a leftover `<path>.tmp` from a crashed write must not change what a
 *       read of the target returns. The tmp holds the half-written new content;
 *       the target still holds the intact old content, and that is what must be
 *       served.
 *     - a read of a file that is too short to be a meaningful object (the "{}"
 *       minimum, or a torn write by an older in-place build) is refused rather
 *       than handed to the JSON parser as nothing.
 *
 *   Plus the pure-file error paths and the path-resolution policy, which carry
 *   the "a corrupt registry must not silently become an empty one" rule.
 *
 *   Usage:
 *     ./s3_statefile_test
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"

#include "vbdev_s3lvol.h"

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
check_str(const char *what, const char *got, const char *want)
{
	char detail[256];

	snprintf(detail, sizeof(detail), "got '%s', want '%s'", got, want);
	check_true(what, got && want && strcmp(got, want) == 0, detail);
}

/* Write a file straight to the filesystem, bypassing the atomic writer. Used to
 * plant the torn-write artefacts the test reasons about. */
static int
raw_write(const char *path, const char *content, size_t len)
{
	FILE *fp = fopen(path, "wb");

	if (!fp) {
		return -errno;
	}
	if (len > 0 && fwrite(content, 1, len, fp) != len) {
		fclose(fp);
		return -EIO;
	}
	fclose(fp);
	return 0;
}

static bool
file_exists(const char *path)
{
	return access(path, F_OK) == 0;
}

int
main(void)
{
	char dir[] = "/tmp/s3lvol_statefile_test.XXXXXX";
	char path[512];
	/* One byte more than path, for the ".tmp" the writer appends. */
	char tmppath[sizeof(path) + 8];
	char *got;
	const char *content_a;
	const char *content_b;
	int rc;

	spdk_log_set_print_level(SPDK_LOG_ERROR);
	spdk_log_open(NULL);

	printf("=== state file crash-safety check ===\n\n");

	if (!mkdtemp(dir)) {
		fprintf(stderr, "mkdtemp failed: %s\n", strerror(errno));
		spdk_log_close();
		return 1;
	}
	snprintf(path, sizeof(path), "%s/state.json", dir);
	snprintf(tmppath, sizeof(tmppath), "%s.tmp", path);

	/* ---------- [1] write / read round trip ---------- */
	printf("[1] write / read round trip\n");
	content_a = "{\"a\":1,\"b\":2}";
	rc = s3lvol_statefile_write(path, content_a);
	check_int("write", rc, 0);
	got = s3lvol_statefile_read(path);
	check_str("read returns the bytes written", got, content_a);
	free(got);

	/* The temp file must be gone after a successful write: it was renamed over
	 * the target, not left behind. */
	check_true("no temp file is left after a clean write",
		   !file_exists(tmppath), NULL);

	/* ---------- [2] overwrite ---------- */
	printf("\n[2] overwrite\n");
	content_b = "{\"x\":\"completely different\"}";
	rc = s3lvol_statefile_write(path, content_b);
	check_int("overwrite", rc, 0);
	got = s3lvol_statefile_read(path);
	check_str("read returns the new bytes", got, content_b);
	check_true("the new content fully replaced the old (no residue)",
		   got && strcmp(got, content_a) != 0, NULL);
	free(got);

	/* ---------- [3] crash shape 1: a leftover tmp must not be served ----------
	 *
	 * Simulate a crash that happened after the temp file was written but before
	 * the rename: the target still holds the intact old content, and a half-
	 * written tmp sits next to it. A read must return the old content, never
	 * the tmp's prefix. */
	printf("\n[3] a leftover half-written tmp does not leak into reads\n");
	rc = raw_write(tmppath, "{\"x\":\"completely diff", 23);   /* torn prefix */
	check_int("plant a torn tmp file", rc, 0);
	got = s3lvol_statefile_read(path);
	check_str("a read still returns the intact target content", got, content_b);
	free(got);

	/* And a subsequent write renames over it cleanly, leaving no tmp. */
	rc = s3lvol_statefile_write(path, content_a);
	check_int("a write after the crash artefact succeeds", rc, 0);
	check_true("and removes the leftover tmp", !file_exists(tmppath), NULL);
	got = s3lvol_statefile_read(path);
	check_str("the target now holds the freshly written content", got, content_a);
	free(got);

	/* ---------- [4] crash shape 2: a too-short file is refused ---------- */
	printf("\n[4] a file too short to be a meaningful object is refused\n");
	rc = raw_write(path, "", 0);
	check_int("plant an empty file", rc, 0);
	got = s3lvol_statefile_read(path);
	check_true("read of an empty file returns NULL", got == NULL, NULL);
	free(got);

	rc = raw_write(path, "{}", 2);   /* exactly the minimum, refused by <= */
	check_int("plant a 2-byte file", rc, 0);
	got = s3lvol_statefile_read(path);
	check_true("read of a 2-byte file returns NULL", got == NULL, NULL);
	free(got);

	rc = raw_write(path, "{", 1);   /* the classic torn in-place write */
	check_int("plant a 1-byte torn prefix", rc, 0);
	got = s3lvol_statefile_read(path);
	check_true("read of a torn 1-byte prefix returns NULL", got == NULL, NULL);
	free(got);

	/* ---------- [5] absent and removal ---------- */
	printf("\n[5] absent and removal\n");
	unlink(path);
	got = s3lvol_statefile_read(path);
	check_true("read of an absent file returns NULL", got == NULL, NULL);
	free(got);

	rc = s3lvol_statefile_remove(path);
	check_int("removing an already-absent file is success (idempotent)", rc, 0);

	rc = s3lvol_statefile_write(path, content_a);
	check_int("write again", rc, 0);
	rc = s3lvol_statefile_remove(path);
	check_int("remove", rc, 0);
	got = s3lvol_statefile_read(path);
	check_true("read after remove returns NULL", got == NULL, NULL);
	free(got);

	/* ---------- [6] write to a directory that does not exist ---------- */
	printf("\n[6] a failed write leaves nothing behind\n");
	{
		char badpath[600];

		snprintf(badpath, sizeof(badpath), "%s/no/such/dir/state.json", dir);
		rc = s3lvol_statefile_write(badpath, content_a);
		check_true("writing to a nonexistent directory fails",
			   rc != 0, NULL);
		check_true("and leaves no target file", !file_exists(badpath), NULL);
	}

	/* ---------- [7] path resolution ---------- */
	printf("\n[7] path resolution (env override, rejection, caching)\n");
	{
		/* Each call below passes a distinct string literal, because the cache
		 * keys on the literal's address. Four distinct names, four slots. */
		const char *p;

		unsetenv("S3LVOL_TF_PATH_A");
		unsetenv("S3LVOL_TF_PATH_B");
		unsetenv("S3LVOL_TF_PATH_C");
		unsetenv("S3LVOL_TF_PATH_D");

		/* Unset -> fallback. */
		p = s3lvol_statefile_path("S3LVOL_TF_PATH_A", "/tmp/fallback-a");
		check_str("unset env uses the fallback", p, "/tmp/fallback-a");

		/* An absolute override wins. */
		setenv("S3LVOL_TF_PATH_B", "/var/lib/rcow/override.json", 1);
		p = s3lvol_statefile_path("S3LVOL_TF_PATH_B", "/tmp/fallback-b");
		check_str("an absolute override is honoured", p,
			  "/var/lib/rcow/override.json");

		/* A relative override is rejected: the location must not depend on the
		 * target's working directory. */
		setenv("S3LVOL_TF_PATH_C", "relative/state.json", 1);
		p = s3lvol_statefile_path("S3LVOL_TF_PATH_C", "/tmp/fallback-c");
		check_str("a relative override is rejected, fallback used", p,
			  "/tmp/fallback-c");

		/* An empty override means unset. */
		setenv("S3LVOL_TF_PATH_D", "", 1);
		p = s3lvol_statefile_path("S3LVOL_TF_PATH_D", "/tmp/fallback-d");
		check_str("an empty override uses the fallback", p, "/tmp/fallback-d");

		/* The resolution is cached: the same literal returns the same pointer
		 * even after the environment changes, so a running instance cannot
		 * split its state across two files. */
		setenv("S3LVOL_TF_PATH_B", "/var/lib/rcow/changed.json", 1);
		p = s3lvol_statefile_path("S3LVOL_TF_PATH_B", "/tmp/fallback-b");
		check_str("a later env change does not move a cached path", p,
			  "/var/lib/rcow/override.json");
	}

	unlink(path);
	unlink(tmppath);
	rmdir(dir);

	spdk_log_close();

	printf("\n=== result: %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail == 0 ? 0 : 1;
}
