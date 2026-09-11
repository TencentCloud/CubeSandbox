/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Active-registry nsid allocation -- no S3, no DPDK init
 *
 *   Auto-alloc picks the free nsid unused for the longest, so a just-vacated
 *   slot is not reused while never-used or older-freed ids remain. The Linux
 *   NVMe host logs "identifiers changed for nsid N" and may never publish a
 *   /dev node if the UUID changes in place.
 *
 *   Usage:
 *     ./s3_active_nsid_test
 */

#include "spdk/stdinc.h"

#include "vbdev_s3lvol.h"

static int g_pass;
static int g_fail;
static char g_dir[64];
static char g_file[128];

static void
pass(const char *what)
{
	g_pass++;
	printf("\t[PASS] %s\n", what);
}

static void
fail(const char *what)
{
	g_fail++;
	printf("\t[FAIL] %s\n", what);
}

static void
expect_eq(const char *what, uint32_t got, uint32_t want)
{
	if (got == want) {
		pass(what);
	} else {
		g_fail++;
		printf("\t[FAIL] %s: got %" PRIu32 ", want %" PRIu32 "\n",
		       what, got, want);
	}
}

static const char *
uuid_for(uint32_t n)
{
	static char buf[SPDK_UUID_STRING_LEN];

	snprintf(buf, sizeof(buf), "00000000-0000-0000-0000-%012" PRIx32, n);
	return buf;
}

static int
add_at(const char *name, uint32_t subsys, uint32_t nsid)
{
	return s3lvol_active_add(name, uuid_for(nsid), subsys, nsid);
}

int
main(void)
{
	uint32_t nsid, i;
	char name[32];
	int rc;

	snprintf(g_dir, sizeof(g_dir), "/tmp/s3lvol_nsid.XXXXXX");
	if (mkdtemp(g_dir) == NULL) {
		perror("mkdtemp");
		return 1;
	}
	snprintf(g_file, sizeof(g_file), "%s/active_lvols", g_dir);
	if (setenv("S3LVOL_ACTIVE_FILE", g_file, 1) != 0) {
		perror("setenv");
		return 1;
	}

	printf("=== s3_active_nsid_test ===\n");

	nsid = s3lvol_active_alloc_nsid(0);
	expect_eq("empty subsystem starts at nsid 1", nsid, 1);
	rc = add_at("vol-a", 0, nsid);
	if (rc != 0) {
		fail("add vol-a");
	} else {
		pass("add vol-a at nsid 1");
	}

	rc = s3lvol_active_remove("vol-a");
	if (rc != 0) {
		fail("remove vol-a");
	} else {
		pass("remove vol-a");
	}

	nsid = s3lvol_active_alloc_nsid(0);
	expect_eq("never-used nsid beats the one just freed", nsid, 2);

	rc = add_at("vol-b", 0, nsid);
	if (rc != 0) {
		fail("add vol-b");
	}
	nsid = s3lvol_active_alloc_nsid(0);
	expect_eq("next auto-alloc still prefers a never-used nsid", nsid, 3);

	s3lvol_active_note_nsid(0, 10);
	nsid = s3lvol_active_alloc_nsid(0);
	expect_eq("explicit placement does not steal a never-used nsid", nsid, 4);

	/* Occupy every remaining never-used slot. In-flight allocs already
	 * touched 3 and 4; 10 was noted. Leave 1 and 2 as the only holes by
	 * filling 3..64 (2 is vol-b). */
	for (i = 3; i <= RCOW_NS_PER_SUBSYS; i++) {
		snprintf(name, sizeof(name), "fill-%u", i);
		if (add_at(name, 0, i) != 0) {
			fail("fill remaining nsids");
			break;
		}
	}

	s3lvol_active_remove("vol-b");
	/* Free: 1 (freed first) and 2 (freed just now). LRU is 1. */
	nsid = s3lvol_active_alloc_nsid(0);
	expect_eq("among freed slots, picks the one idle longest", nsid, 1);

	if (add_at("vol-1", 0, 1) != 0) {
		fail("add vol-1");
	}
	nsid = s3lvol_active_alloc_nsid(0);
	expect_eq("only-free-slot may reuse the remaining cooled-down nsid", nsid, 2);

	if (add_at("vol-2", 0, 2) != 0) {
		fail("add vol-2");
	}
	nsid = s3lvol_active_alloc_nsid(0);
	expect_eq("full subsystem returns 0", nsid, 0);

	nsid = s3lvol_active_alloc_nsid(RCOW_NUM_SUBSYS);
	expect_eq("out-of-range subsys returns 0", nsid, 0);

	/* Fill another subsystem, free three slots in order, and check LRU. */
	for (i = 1; i <= RCOW_NS_PER_SUBSYS; i++) {
		snprintf(name, sizeof(name), "s1-%u", i);
		if (add_at(name, 1, i) != 0) {
			fail("fill subsys 1");
			break;
		}
	}
	s3lvol_active_remove("s1-10");
	s3lvol_active_remove("s1-20");
	s3lvol_active_remove("s1-5");
	nsid = s3lvol_active_alloc_nsid(1);
	expect_eq("LRU among three freed nsids is the earliest free", nsid, 10);
	if (add_at("s1-10", 1, 10) != 0) {
		fail("re-add s1-10");
	}
	nsid = s3lvol_active_alloc_nsid(1);
	expect_eq("second LRU is the next-earliest free", nsid, 20);

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail ? 1 : 0;
}
