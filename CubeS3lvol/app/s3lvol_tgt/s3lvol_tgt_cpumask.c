/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#include "s3lvol_tgt_cpumask.h"

#include <ctype.h>
#include <errno.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int
select_reactors(const cpu_set_t *cpuset, size_t cpuset_size, size_t num_cpus,
		size_t reactor_count, size_t *highest, size_t *second)
{
	int found = 0;

	if (cpuset == NULL || reactor_count == 0) {
		return -EINVAL;
	}

	if (num_cpus > CPU_SETSIZE) {
		num_cpus = CPU_SETSIZE;
	}
	for (size_t cpu = num_cpus; cpu > 0; cpu--) {
		size_t id = cpu - 1;

		if (!CPU_ISSET_S(id, cpuset_size, cpuset)) {
			continue;
		}
		if (found == 0) {
			*highest = id;
			found = 1;
		} else if (reactor_count > 1) {
			*second = id;
			found = 2;
			break;
		}
	}

	return found == 0 ? -ENODEV : found;
}

int
s3lvol_tgt_lcore_map_from_set(const cpu_set_t *cpuset, size_t cpuset_size,
			     size_t num_cpus, size_t reactor_count,
			     char *lcore_map, size_t lcore_map_size)
{
	size_t highest = 0, second = 0;
	int found, written;

	if (lcore_map == NULL || lcore_map_size == 0) {
		return -EINVAL;
	}

	found = select_reactors(cpuset, cpuset_size, num_cpus, reactor_count,
				&highest, &second);
	if (found < 0) {
		return found;
	}

	if (found == 1) {
		written = snprintf(lcore_map, lcore_map_size, "0@%zu", highest);
	} else {
		written = snprintf(lcore_map, lcore_map_size, "0@%zu,1@%zu",
				   second, highest);
	}
	if (written < 0 || (size_t)written >= lcore_map_size) {
		return -ENOSPC;
	}

	return 0;
}

int
s3lvol_tgt_lcore_map_from_affinity(size_t reactor_count, char *lcore_map,
				  size_t lcore_map_size,
				  struct s3lvol_tgt_cpu_selection *selection)
{
	long configured;
	size_t num_cpus;

	if (reactor_count == 0 || selection == NULL) {
		return -EINVAL;
	}

	selection->original = NULL;
	selection->reactors = NULL;
	selection->background = NULL;
	selection->cpuset_size = 0;
	configured = sysconf(_SC_NPROCESSORS_CONF);
	if (configured < 1) {
		configured = CPU_SETSIZE;
	}
	num_cpus = (size_t)configured;
	if (num_cpus < CPU_SETSIZE) {
		num_cpus = CPU_SETSIZE;
	}

	for (;;) {
		cpu_set_t *allowed;
		size_t cpuset_size;
		size_t highest = 0, second = 0;
		int found, rc, saved_errno;

		cpuset_size = CPU_ALLOC_SIZE(num_cpus);
		allowed = CPU_ALLOC(num_cpus);
		if (allowed == NULL) {
			return -ENOMEM;
		}
		CPU_ZERO_S(cpuset_size, allowed);

		if (sched_getaffinity(0, cpuset_size, allowed) == 0) {
			rc = s3lvol_tgt_lcore_map_from_set(allowed, cpuset_size,
							 num_cpus, reactor_count,
							 lcore_map, lcore_map_size);
			if (rc != 0) {
				CPU_FREE(allowed);
				return rc;
			}

			found = select_reactors(allowed, cpuset_size, num_cpus,
						reactor_count, &highest, &second);
			selection->original = CPU_ALLOC(num_cpus);
			selection->reactors = CPU_ALLOC(num_cpus);
			selection->background = CPU_ALLOC(num_cpus);
			if (selection->original == NULL || selection->reactors == NULL ||
			    selection->background == NULL) {
				CPU_FREE(selection->background);
				CPU_FREE(selection->reactors);
				CPU_FREE(selection->original);
				CPU_FREE(allowed);
				selection->background = NULL;
				selection->reactors = NULL;
				selection->original = NULL;
				return -ENOMEM;
			}
			memcpy(selection->original, allowed, cpuset_size);
			CPU_ZERO_S(cpuset_size, selection->reactors);
			CPU_SET_S(highest, cpuset_size, selection->reactors);
			if (found == 2) {
				CPU_SET_S(second, cpuset_size, selection->reactors);
			}
			memcpy(selection->background, allowed, cpuset_size);
			CPU_CLR_S(highest, cpuset_size, selection->background);
			if (found == 2) {
				CPU_CLR_S(second, cpuset_size, selection->background);
			}
			if (CPU_COUNT_S(cpuset_size, selection->background) == 0) {
				memcpy(selection->background, allowed, cpuset_size);
			}
			CPU_FREE(allowed);
			selection->cpuset_size = cpuset_size;
			return 0;
		}

		saved_errno = errno;
		CPU_FREE(allowed);
		if (saved_errno != EINVAL) {
			return -saved_errno;
		}
		if (num_cpus > SIZE_MAX / 2) {
			return -EOVERFLOW;
		}
		num_cpus *= 2;
	}
}

int
s3lvol_tgt_capture_affinity(struct s3lvol_tgt_cpu_selection *selection)
{
	long configured;
	size_t num_cpus;

	if (selection == NULL) {
		return -EINVAL;
	}
	selection->original = NULL;
	selection->reactors = NULL;
	selection->background = NULL;
	selection->cpuset_size = 0;
	configured = sysconf(_SC_NPROCESSORS_CONF);
	num_cpus = configured > 0 ? (size_t)configured : CPU_SETSIZE;
	if (num_cpus < CPU_SETSIZE) {
		num_cpus = CPU_SETSIZE;
	}

	for (;;) {
		size_t cpuset_size = CPU_ALLOC_SIZE(num_cpus);
		cpu_set_t *original = CPU_ALLOC(num_cpus);
		int saved_errno;

		if (original == NULL) {
			return -ENOMEM;
		}
		CPU_ZERO_S(cpuset_size, original);
		if (sched_getaffinity(0, cpuset_size, original) == 0) {
			selection->original = original;
			selection->cpuset_size = cpuset_size;
			return 0;
		}
		saved_errno = errno;
		CPU_FREE(original);
		if (saved_errno != EINVAL) {
			return -saved_errno;
		}
		if (num_cpus > SIZE_MAX / 2) {
			return -EOVERFLOW;
		}
		num_cpus *= 2;
	}
}

static int
parse_cpu_id(const char **cursor, size_t num_cpus, size_t *cpu)
{
	char *end;
	unsigned long value;

	while (isspace((unsigned char)**cursor)) {
		(*cursor)++;
	}
	errno = 0;
	value = strtoul(*cursor, &end, 10);
	if (end == *cursor || errno != 0 || value >= num_cpus) {
		return -EINVAL;
	}
	*cursor = end;
	*cpu = (size_t)value;
	return 0;
}

static int
parse_cpu_set(const char **cursor, size_t num_cpus, cpu_set_t *cpuset,
	      size_t cpuset_size)
{
	bool parenthesized;

	while (isspace((unsigned char)**cursor)) {
		(*cursor)++;
	}
	parenthesized = **cursor == '(';
	if (parenthesized) {
		(*cursor)++;
	}
	for (;;) {
		size_t first, last;
		int rc;

		rc = parse_cpu_id(cursor, num_cpus, &first);
		if (rc != 0) {
			return rc;
		}
		last = first;
		while (isspace((unsigned char)**cursor)) {
			(*cursor)++;
		}
		if (**cursor == '-') {
			(*cursor)++;
			rc = parse_cpu_id(cursor, num_cpus, &last);
			if (rc != 0) {
				return rc;
			}
		}
		if (first <= last) {
			for (size_t cpu = first; cpu <= last; cpu++) {
				CPU_SET_S(cpu, cpuset_size, cpuset);
			}
		} else {
			for (size_t cpu = last; cpu <= first; cpu++) {
				CPU_SET_S(cpu, cpuset_size, cpuset);
			}
		}
		while (isspace((unsigned char)**cursor)) {
			(*cursor)++;
		}
		if (!parenthesized || **cursor == ')') {
			if (parenthesized) {
				(*cursor)++;
			}
			return 0;
		}
		if (**cursor != ',') {
			return -EINVAL;
		}
		(*cursor)++;
	}
}

static int
reactors_from_lcore_map(const char *lcore_map, size_t num_cpus,
			cpu_set_t *reactors, size_t cpuset_size)
{
	const char *cursor = lcore_map;

	while (*cursor != '\0') {
		cpu_set_t *lcores, *cpus;
		int rc;

		lcores = CPU_ALLOC(num_cpus);
		cpus = CPU_ALLOC(num_cpus);
		if (lcores == NULL || cpus == NULL) {
			CPU_FREE(cpus);
			CPU_FREE(lcores);
			return -ENOMEM;
		}
		CPU_ZERO_S(cpuset_size, lcores);
		CPU_ZERO_S(cpuset_size, cpus);
		rc = parse_cpu_set(&cursor, num_cpus, lcores, cpuset_size);
		if (rc == 0 && *cursor == '@') {
			cursor++;
			rc = parse_cpu_set(&cursor, num_cpus, cpus, cpuset_size);
		} else if (rc == 0) {
			memcpy(cpus, lcores, cpuset_size);
		}
		if (rc == 0) {
			for (size_t cpu = 0; cpu < num_cpus; cpu++) {
				if (CPU_ISSET_S(cpu, cpuset_size, cpus)) {
					CPU_SET_S(cpu, cpuset_size, reactors);
				}
			}
		}
		CPU_FREE(cpus);
		CPU_FREE(lcores);
		if (rc != 0) {
			return rc;
		}
		while (isspace((unsigned char)*cursor)) {
			cursor++;
		}
		if (*cursor == '\0') {
			break;
		}
		if (*cursor != ',') {
			return -EINVAL;
		}
		cursor++;
	}
	return CPU_COUNT_S(cpuset_size, reactors) == 0 ? -EINVAL : 0;
}

static int
parse_hex_mask(const char *mask, size_t num_cpus, cpu_set_t *reactors,
	       size_t cpuset_size)
{
	size_t bit = 0, len;
	bool found = false;

	if (mask[0] == '0' && (mask[1] == 'x' || mask[1] == 'X')) {
		mask += 2;
	}
	len = strlen(mask);
	while (len > 0 && isspace((unsigned char)mask[len - 1])) {
		len--;
	}
	for (; len > 0; len--) {
		unsigned char ch = (unsigned char)mask[len - 1];
		int nibble;

		if (ch == ',') {
			continue;
		}
		if (ch >= '0' && ch <= '9') {
			nibble = ch - '0';
		} else if (ch >= 'a' && ch <= 'f') {
			nibble = ch - 'a' + 10;
		} else if (ch >= 'A' && ch <= 'F') {
			nibble = ch - 'A' + 10;
		} else {
			return -EINVAL;
		}
		found = true;
		for (unsigned int offset = 0; offset < 4; offset++, bit++) {
			if ((nibble & (1U << offset)) != 0 && bit < num_cpus) {
				CPU_SET_S(bit, cpuset_size, reactors);
			}
		}
	}
	return found ? 0 : -EINVAL;
}

static int
parse_mask_list(const char *mask, size_t num_cpus, cpu_set_t *reactors,
		size_t cpuset_size)
{
	const char *cursor = mask + 1;

	for (;;) {
		size_t first, last;
		int rc = parse_cpu_id(&cursor, num_cpus, &first);

		if (rc != 0) {
			return rc;
		}
		last = first;
		while (isspace((unsigned char)*cursor)) {
			cursor++;
		}
		if (*cursor == '-') {
			cursor++;
			rc = parse_cpu_id(&cursor, num_cpus, &last);
			if (rc != 0 || last < first) {
				return -EINVAL;
			}
		}
		for (size_t cpu = first; cpu <= last; cpu++) {
			CPU_SET_S(cpu, cpuset_size, reactors);
		}
		while (isspace((unsigned char)*cursor)) {
			cursor++;
		}
		if (*cursor == ']') {
			cursor++;
			while (isspace((unsigned char)*cursor)) {
				cursor++;
			}
			return *cursor == '\0' ? 0 : -EINVAL;
		}
		if (*cursor != ',') {
			return -EINVAL;
		}
		cursor++;
	}
}

int
s3lvol_tgt_background_from_options(const cpu_set_t *allowed,
				  size_t cpuset_size, size_t num_cpus,
				  const char *reactor_mask, const char *lcore_map,
				  cpu_set_t *reactors, cpu_set_t *background)
{
	int rc;

	if (allowed == NULL || reactors == NULL || background == NULL ||
	    num_cpus == 0 || (reactor_mask == NULL) == (lcore_map == NULL)) {
		return -EINVAL;
	}
	CPU_ZERO_S(cpuset_size, reactors);
	if (reactor_mask != NULL) {
		while (isspace((unsigned char)*reactor_mask)) {
			reactor_mask++;
		}
		rc = *reactor_mask == '[' ?
		     parse_mask_list(reactor_mask, num_cpus, reactors, cpuset_size) :
		     parse_hex_mask(reactor_mask, num_cpus, reactors, cpuset_size);
	} else {
		rc = reactors_from_lcore_map(lcore_map, num_cpus, reactors, cpuset_size);
	}
	if (rc == 0 && CPU_COUNT_S(cpuset_size, reactors) == 0) {
		rc = -EINVAL;
	}
	if (rc == 0) {
		memcpy(background, allowed, cpuset_size);
		for (size_t cpu = 0; cpu < num_cpus; cpu++) {
			if (CPU_ISSET_S(cpu, cpuset_size, reactors)) {
				CPU_CLR_S(cpu, cpuset_size, background);
			}
		}
		if (CPU_COUNT_S(cpuset_size, background) == 0) {
			memcpy(background, allowed, cpuset_size);
		}
	}
	return rc;
}

bool
s3lvol_tgt_cpu_selection_shares_reactors(
	const struct s3lvol_tgt_cpu_selection *selection)
{
	if (selection == NULL || selection->reactors == NULL ||
	    selection->background == NULL) {
		return false;
	}

	for (size_t cpu = 0; cpu < selection->cpuset_size * CHAR_BIT; cpu++) {
		if (CPU_ISSET_S(cpu, selection->cpuset_size, selection->reactors) &&
		    CPU_ISSET_S(cpu, selection->cpuset_size, selection->background)) {
			return true;
		}
	}
	return false;
}

void
s3lvol_tgt_cpu_selection_fini(struct s3lvol_tgt_cpu_selection *selection)
{
	if (selection == NULL) {
		return;
	}

	CPU_FREE(selection->background);
	CPU_FREE(selection->reactors);
	CPU_FREE(selection->original);
	selection->background = NULL;
	selection->reactors = NULL;
	selection->original = NULL;
	selection->cpuset_size = 0;
}
