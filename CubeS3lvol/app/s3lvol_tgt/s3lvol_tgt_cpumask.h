/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#ifndef S3LVOL_TGT_CPUMASK_H
#define S3LVOL_TGT_CPUMASK_H

#include <sched.h>
#include <stdbool.h>
#include <stddef.h>

struct s3lvol_tgt_cpu_selection {
	cpu_set_t *original;
	cpu_set_t *reactors;
	cpu_set_t *background;
	size_t cpuset_size;
};

int s3lvol_tgt_lcore_map_from_set(const cpu_set_t *cpuset, size_t cpuset_size,
					 size_t num_cpus, size_t reactor_count,
					 char *lcore_map, size_t lcore_map_size);
int s3lvol_tgt_lcore_map_from_affinity(size_t reactor_count, char *lcore_map,
					      size_t lcore_map_size,
					      struct s3lvol_tgt_cpu_selection *selection);
int s3lvol_tgt_capture_affinity(struct s3lvol_tgt_cpu_selection *selection);
int s3lvol_tgt_background_from_options(const cpu_set_t *allowed,
				      size_t cpuset_size, size_t num_cpus,
				      const char *reactor_mask, const char *lcore_map,
				      cpu_set_t *reactors, cpu_set_t *background);
bool s3lvol_tgt_cpu_selection_shares_reactors(
	const struct s3lvol_tgt_cpu_selection *selection);
void s3lvol_tgt_cpu_selection_fini(struct s3lvol_tgt_cpu_selection *selection);

#endif /* S3LVOL_TGT_CPUMASK_H */
