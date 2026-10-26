// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#include "cubevs.h"

struct l7_mark_case {
	__u32 mark;
};

SEC("tc")
int test_l7_mark(struct __sk_buff *skb)
{
	struct l7_mark_case tc = {};

	if (bpf_skb_load_bytes(skb, 0, &tc, sizeof(tc)))
		return TC_ACT_SHOT;

	/* Mirror mvmtap.bpf.c's L7 stamp: keep the low (user) bits of skb->mark,
	 * then OR in the configured cube HTTP mark.
	 */
	tc.mark = (tc.mark & ~cube_l7_mark_mask) | cube_l7_mark_http;

	if (bpf_skb_store_bytes(skb, 0, &tc, sizeof(tc), 0))
		return TC_ACT_SHOT;
	return TC_ACT_OK;
}

char __license[] SEC("license") = "Dual BSD/GPL";
