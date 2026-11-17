// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#ifndef __METRICS_H
#define __METRICS_H

#include "map.h"

static __always_inline void update_metrics(__u32 sandbox_ip, __u64 bytes,
					   __u32 direction, __u32 reason)
{
	struct sandbox_metrics *value;
	bool ingress = direction == SANDBOX_METRIC_INGRESS;

	value = bpf_map_lookup_elem(&sandbox_metrics, &sandbox_ip);
	if (!value)
		return;

	switch (reason) {
	case SANDBOX_METRIC_FORWARDED:
		if (ingress) {
			__sync_fetch_and_add(&value->ingress_packets, 1);
			__sync_fetch_and_add(&value->ingress_bytes, bytes);
		} else {
			__sync_fetch_and_add(&value->egress_packets, 1);
			__sync_fetch_and_add(&value->egress_bytes, bytes);
		}
		break;
	case SANDBOX_METRIC_DROP:
		if (ingress) {
			__sync_fetch_and_add(&value->ingress_drop_packets, 1);
			__sync_fetch_and_add(&value->ingress_drop_bytes, bytes);
		} else {
			__sync_fetch_and_add(&value->egress_drop_packets, 1);
			__sync_fetch_and_add(&value->egress_drop_bytes, bytes);
		}
		break;
	case SANDBOX_METRIC_SNAT_ALLOC_FAIL:
		__sync_fetch_and_add(&value->snat_alloc_failures, 1);
		break;
	default:
		return;
	}
}

#endif /* __METRICS_H */
