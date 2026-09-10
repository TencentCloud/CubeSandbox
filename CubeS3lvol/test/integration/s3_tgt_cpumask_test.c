/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#include "s3lvol_tgt_cpumask.h"

#include <errno.h>
#include <limits.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static int g_pass, g_fail;

static void
check_map(const char *what, const size_t *cpus, size_t count, size_t num_cpus,
	  size_t reactor_count, const char *expected)
{
	cpu_set_t *set;
	size_t set_size;
	char lcore_map[64];
	int before, rc;

	set_size = CPU_ALLOC_SIZE(num_cpus);
	set = CPU_ALLOC(num_cpus);
	if (set == NULL) {
		perror("CPU_ALLOC");
		g_fail++;
		return;
	}
	CPU_ZERO_S(set_size, set);
	for (size_t i = 0; i < count; i++) {
		CPU_SET_S(cpus[i], set_size, set);
	}
	before = CPU_COUNT_S(set_size, set);

	rc = s3lvol_tgt_lcore_map_from_set(set, set_size, num_cpus, reactor_count,
						 lcore_map, sizeof(lcore_map));
	if (rc == 0 && strcmp(lcore_map, expected) == 0 &&
	    CPU_COUNT_S(set_size, set) == before) {
		g_pass++;
		printf("\t[PASS] %s -> %s\n", what, lcore_map);
	} else {
		g_fail++;
		printf("\t[FAIL] %s: rc=%d map='%s', expected '%s'\n",
		       what, rc, rc == 0 ? lcore_map : "", expected);
	}
	CPU_FREE(set);
}

static int
get_affinity(cpu_set_t **set_out, size_t *num_cpus_out, size_t *set_size_out)
{
	struct s3lvol_tgt_cpu_selection selection = {};
	int rc;

	rc = s3lvol_tgt_capture_affinity(&selection);
	if (rc != 0) {
		return rc;
	}
	*set_out = selection.original;
	*num_cpus_out = selection.cpuset_size * CHAR_BIT;
	*set_size_out = selection.cpuset_size;
	selection.original = NULL;
	s3lvol_tgt_cpu_selection_fini(&selection);
	return 0;
}

static void
check_affinity(void)
{
	cpu_set_t *original = NULL, *narrowed = NULL;
	size_t num_cpus = 0, set_size = 0;
	struct s3lvol_tgt_cpu_selection pair_selection = {};
	struct s3lvol_tgt_cpu_selection single_selection = {};
	char expected[64], lcore_map[64], single_expected[64], single_map[64];
	ssize_t highest = -1, second = -1;
	bool pair_sets_ok = false, single_sets_ok = false;
	int rc, single_rc = -EINVAL;

	rc = get_affinity(&original, &num_cpus, &set_size);
	if (rc != 0) {
		errno = -rc;
		perror("sched_getaffinity");
		g_fail++;
		return;
	}
	for (size_t cpu = num_cpus; cpu > 0; cpu--) {
		size_t id = cpu - 1;

		if (!CPU_ISSET_S(id, set_size, original)) {
			continue;
		}
		if (highest < 0) {
			highest = (ssize_t)id;
		} else {
			second = (ssize_t)id;
			break;
		}
	}
	if (highest < 0) {
		g_fail++;
		printf("\t[FAIL] current affinity is empty\n");
		goto out;
	}

	narrowed = CPU_ALLOC(num_cpus);
	if (narrowed == NULL) {
		perror("CPU_ALLOC");
		g_fail++;
		goto out;
	}
	CPU_ZERO_S(set_size, narrowed);
	CPU_SET_S((size_t)highest, set_size, narrowed);
	if (second >= 0) {
		CPU_SET_S((size_t)second, set_size, narrowed);
		snprintf(expected, sizeof(expected), "0@%zd,1@%zd", second, highest);
	} else {
		snprintf(expected, sizeof(expected), "0@%zd", highest);
	}
	if (sched_setaffinity(0, set_size, narrowed) != 0) {
		perror("sched_setaffinity");
		g_fail++;
		goto out;
	}

	rc = s3lvol_tgt_lcore_map_from_affinity(2, lcore_map, sizeof(lcore_map),
						   &pair_selection);
	if (rc == 0) {
		pair_sets_ok = CPU_ISSET_S((size_t)highest,
						 pair_selection.cpuset_size,
						 pair_selection.reactors) &&
			       (second < 0 || CPU_ISSET_S((size_t)second,
							 pair_selection.cpuset_size,
							 pair_selection.reactors)) &&
			       CPU_COUNT_S(pair_selection.cpuset_size,
					   pair_selection.background) > 0;
	}

	CPU_ZERO_S(set_size, narrowed);
	CPU_SET_S((size_t)highest, set_size, narrowed);
	snprintf(single_expected, sizeof(single_expected), "0@%zd", highest);
	if (sched_setaffinity(0, set_size, narrowed) != 0) {
		perror("single sched_setaffinity");
		single_rc = -errno;
	} else {
		single_rc = s3lvol_tgt_lcore_map_from_affinity(2, single_map,
								 sizeof(single_map),
								 &single_selection);
	}
	if (single_rc == 0) {
		single_sets_ok = CPU_ISSET_S((size_t)highest,
					   single_selection.cpuset_size,
					   single_selection.reactors) &&
				 CPU_COUNT_S(single_selection.cpuset_size,
					     single_selection.background) == 1;
	}

	if (sched_setaffinity(0, set_size, original) != 0) {
		perror("restore sched_setaffinity");
		g_fail++;
		goto out;
	}
	if (rc == 0 && strcmp(lcore_map, expected) == 0 && pair_sets_ok &&
	    single_rc == 0 && strcmp(single_map, single_expected) == 0 &&
	    single_sets_ok) {
		g_pass++;
		printf("\t[PASS] effective affinity -> %s and %s\n",
		       lcore_map, single_map);
	} else {
		g_fail++;
		printf("\t[FAIL] effective affinity: rc=%d map='%s', expected '%s'; "
		       "single rc=%d map='%s', expected '%s'\n",
		       rc, rc == 0 ? lcore_map : "", expected, single_rc,
		       single_rc == 0 ? single_map : "", single_expected);
	}

out:
	s3lvol_tgt_cpu_selection_fini(&single_selection);
	s3lvol_tgt_cpu_selection_fini(&pair_selection);
	CPU_FREE(narrowed);
	CPU_FREE(original);
}

static void
check_lcore_limit(void)
{
	const size_t pair[] = {4, 31};

	check_map("one available lcore", pair, 2, 64, 1, "0@31");
}

static void
check_explicit_options(void)
{
	cpu_set_t allowed, reactors, background;
	int rc;

	CPU_ZERO(&allowed);
	CPU_SET(4, &allowed);
	CPU_SET(9, &allowed);
	CPU_SET(31, &allowed);

	rc = s3lvol_tgt_background_from_options(&allowed, sizeof(allowed),
						 CPU_SETSIZE, "0x210 ", NULL,
						 &reactors, &background);
	if (rc == 0 && CPU_COUNT(&background) == 1 && CPU_ISSET(31, &background)) {
		g_pass++;
		printf("\t[PASS] explicit hex mask -> background CPU 31\n");
	} else {
		g_fail++;
		printf("\t[FAIL] explicit hex mask returned %d\n", rc);
	}

	rc = s3lvol_tgt_background_from_options(&allowed, sizeof(allowed),
						 CPU_SETSIZE, "[4,9]", NULL,
						 &reactors, &background);
	if (rc == 0 && CPU_COUNT(&background) == 1 && CPU_ISSET(31, &background)) {
		g_pass++;
		printf("\t[PASS] explicit CPU list -> background CPU 31\n");
	} else {
		g_fail++;
		printf("\t[FAIL] explicit CPU list returned %d\n", rc);
	}

	rc = s3lvol_tgt_background_from_options(&allowed, sizeof(allowed),
						 CPU_SETSIZE, NULL,
						 "0@9,1@31", &reactors, &background);
	if (rc == 0 && CPU_COUNT(&background) == 1 && CPU_ISSET(4, &background)) {
		g_pass++;
		printf("\t[PASS] explicit lcore map -> background CPU 4\n");
	} else {
		g_fail++;
		printf("\t[FAIL] explicit lcore map returned %d\n", rc);
	}

	rc = s3lvol_tgt_background_from_options(&allowed, sizeof(allowed),
						 CPU_SETSIZE, NULL,
						 " (1-0)@(9-4)", &reactors, &background);
	if (rc == 0 && CPU_COUNT(&background) == 1 && CPU_ISSET(31, &background)) {
		g_pass++;
		printf("\t[PASS] grouped lcore map -> background CPU 31\n");
	} else {
		g_fail++;
		printf("\t[FAIL] grouped lcore map returned %d\n", rc);
	}

	CPU_ZERO(&allowed);
	CPU_SET(4, &allowed);
	CPU_SET(9, &allowed);
	rc = s3lvol_tgt_background_from_options(&allowed, sizeof(allowed),
						 CPU_SETSIZE, "[4,9]", NULL,
						 &reactors, &background);
	{
		struct s3lvol_tgt_cpu_selection selection = {
			.reactors = &reactors,
			.background = &background,
			.cpuset_size = sizeof(background),
		};

		if (rc == 0 &&
		    s3lvol_tgt_cpu_selection_shares_reactors(&selection)) {
			g_pass++;
			printf("\t[PASS] explicit full mask reports reactor sharing\n");
		} else {
			g_fail++;
			printf("\t[FAIL] explicit full mask sharing returned %d\n", rc);
		}
	}
}

static void
check_errors(void)
{
	cpu_set_t set;
	char lcore_map[4];
	int rc;

	CPU_ZERO(&set);
	if (s3lvol_tgt_lcore_map_from_set(NULL, sizeof(set), CPU_SETSIZE, 128,
						 lcore_map, sizeof(lcore_map)) == -EINVAL &&
	    s3lvol_tgt_lcore_map_from_set(&set, sizeof(set), CPU_SETSIZE, 0,
						 lcore_map, sizeof(lcore_map)) == -EINVAL &&
	    s3lvol_tgt_lcore_map_from_set(&set, sizeof(set), CPU_SETSIZE, 128,
						 NULL, sizeof(lcore_map)) == -EINVAL &&
	    s3lvol_tgt_lcore_map_from_set(&set, sizeof(set), CPU_SETSIZE, 128,
						 lcore_map, 0) == -EINVAL) {
		g_pass++;
		printf("\t[PASS] invalid arguments are rejected\n");
	} else {
		g_fail++;
		printf("\t[FAIL] invalid argument handling\n");
	}

	rc = s3lvol_tgt_lcore_map_from_set(&set, sizeof(set), CPU_SETSIZE, 128,
						 lcore_map, sizeof(lcore_map));
	if (rc == -ENODEV) {
		g_pass++;
		printf("\t[PASS] empty affinity is rejected\n");
	} else {
		g_fail++;
		printf("\t[FAIL] empty affinity returned %d\n", rc);
	}

	CPU_SET(4, &set);
	{
		cpu_set_t reactors, background;

		rc = s3lvol_tgt_background_from_options(&set, sizeof(set), CPU_SETSIZE,
								 "0x0", NULL, &reactors,
								 &background);
		if (rc == -EINVAL) {
			g_pass++;
			printf("\t[PASS] empty reactor mask is rejected\n");
		} else {
			g_fail++;
			printf("\t[FAIL] empty reactor mask returned %d\n", rc);
		}
	}

	CPU_SET(123, &set);
	rc = s3lvol_tgt_lcore_map_from_set(&set, sizeof(set), CPU_SETSIZE, 128,
						 lcore_map, sizeof(lcore_map));
	if (rc == -ENOSPC) {
		g_pass++;
		printf("\t[PASS] short output buffer is rejected\n");
	} else {
		g_fail++;
		printf("\t[FAIL] short output buffer returned %d\n", rc);
	}
}

int
main(void)
{
	const size_t sparse[] = {1, 8, 11};
	const size_t pair[] = {4, 31};
	const size_t single[] = {7};
	const size_t mixed[] = {4, 31, CPU_SETSIZE + 3, CPU_SETSIZE + 19};

	check_map("sparse set", sparse, 3, 64, 128, "0@8,1@11");
	check_map("two CPUs", pair, 2, 64, 128, "0@4,1@31");
	check_map("one CPU", single, 1, 64, 128, "0@7");
	check_map("IDs above CPU_SETSIZE are skipped", mixed, 4,
		  CPU_SETSIZE + 64, 128, "0@4,1@31");
	check_affinity();
	check_lcore_limit();
	check_explicit_options();
	check_errors();

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail ? 1 : 0;
}
