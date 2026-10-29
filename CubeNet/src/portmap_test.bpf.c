// SPDX-License-Identifier: GPL-2.0
/* Copyright (c) 2026 Cube Authors */
/* Test harness for the same-host port-mapping hairpin paths in from_cube().
 *
 * The production mvmtap.bpf.c is included with bpf_redirect() replaced by a
 * recorder: under BPF_PROG_TEST_RUN a real redirect only surfaces as
 * TC_ACT_REDIRECT, which cannot tell the test where the packet would have
 * gone. The recorder captures (ifindex, flags) into redirect_log so tests can
 * assert the full forwarding decision, and returns TC_ACT_REDIRECT without
 * redirecting (the test-run skb has no meaningful redirect targets). Packet
 * rewrites are asserted via data_out, session state via the maps.
 */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "cubevs.h"

struct redirect_record {
	__u64 flags;
	__u32 ifindex;
	__u32 calls;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct redirect_record);
} redirect_log SEC(".maps");

static __always_inline long cube_test_redirect(__u32 ifindex, __u64 flags)
{
	struct redirect_record *rec;
	__u32 zero = 0;

	rec = bpf_map_lookup_elem(&redirect_log, &zero);
	if (rec) {
		rec->flags = flags;
		rec->ifindex = ifindex;
		rec->calls++;
	}
	return TC_ACT_REDIRECT;
}

#define bpf_redirect cube_test_redirect
#include "mvmtap.bpf.c"
#undef bpf_redirect

/* probe_ifindex records the ifindices the kernel assigns to a test-run skb
 * so the Go test can key ifindex_to_mvmmeta correctly. */
SEC("tc")
int probe_ifindex(struct __sk_buff *skb)
{
	struct redirect_record *rec;
	__u32 zero = 0;

	rec = bpf_map_lookup_elem(&redirect_log, &zero);
	if (rec) {
		rec->ifindex = skb->ingress_ifindex;
		rec->flags = skb->ifindex;
		rec->calls = 0;
	}
	return TC_ACT_OK;
}
