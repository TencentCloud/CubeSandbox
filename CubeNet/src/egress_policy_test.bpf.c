// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#include "session.h"

struct egress_policy_case {
	__u32 ifindex;
	__u32 daddr;
	__u16 dport;
	__u8 verdict;
	__u8 reserved[5];
};

SEC("tc")
int test_classify_egress_flow(struct __sk_buff *skb)
{
	struct egress_policy_case tc = {};

	if (bpf_skb_load_bytes(skb, 0, &tc, sizeof(tc)))
		return TC_ACT_SHOT;

	tc.verdict = classify_egress_flow(tc.ifindex, tc.daddr, tc.dport);
	if (bpf_skb_store_bytes(skb, 0, &tc, sizeof(tc), 0))
		return TC_ACT_SHOT;

	return TC_ACT_OK;
}

char __license[] SEC("license") = "Dual BSD/GPL";
