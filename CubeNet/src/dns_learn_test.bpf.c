// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#include "dns_response.h"

/* Single-slot store for the dns_query_track_value under test. The dataplane
 * reads it as a map-value pointer (mirroring production, where the query is a
 * dns_query_track map value) so the verifier accepts the per-port loop's
 * bounded offsets — a stack copy would risk rejection as a variable-offset
 * stack access when the loop is not fully unrolled.
 */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct dns_query_track_value);
} test_query_store SEC(".maps");

struct dns_learn_case {
	__u32 ifindex;
	__u32 ip;  /* network byte order */
	__u32 ttl; /* seconds */
};

SEC("tc")
int test_dns_learn(struct __sk_buff *skb)
{
	struct dns_learn_case tc = {};
	__u32 qkey = 0;
	struct dns_query_track_value *query;

	if (bpf_skb_load_bytes(skb, 0, &tc, sizeof(tc)))
		return TC_ACT_SHOT;

	query = bpf_map_lookup_elem(&test_query_store, &qkey);
	if (!query)
		return TC_ACT_SHOT;

	dns_learn_response_ip(tc.ifindex, tc.ip, tc.ttl, query);
	return TC_ACT_OK;
}

char __license[] SEC("license") = "Dual BSD/GPL";
