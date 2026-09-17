// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#include "dns_response.h"

/* Replaces the retired dns_learn test. The datapath no longer writes
 * allow_out_v3 rows, so what is worth pinning down here is the *decision*:
 * which replies get handed to user space (and are therefore withheld from the
 * sandbox until the learner has run) and which stay on the ordinary
 * reverse-NAT path.
 */

struct dns_upload_case {
	__u32 dns_off;
	__u32 ifindex;
	__u32 server_ip; /* network byte order */
	__u16 source_port; /* network byte order */
	__u16 _pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct dns_upload_case);
} test_upload_case SEC(".maps");

/* Mirrors the decide-then-deliver shape of dns_response_finish_prog: on the
 * upload path it also emits the frame on dns_events, so the test can read the
 * record back and confirm the packet really does reach user space.
 *
 * Returns TC_ACT_SHOT when the reply was uploaded, TC_ACT_OK when it would be
 * forwarded normally, and TC_ACT_SHOT+1 if the harness failed to stage a case.
 */
SEC("tc")
int test_dns_upload(struct __sk_buff *skb)
{
	struct dns_upload_case *tc;
	__u32 key = 0;

	tc = bpf_map_lookup_elem(&test_upload_case, &key);
	if (!tc)
		return TC_ACT_SHOT + 1; /* distinct from both verdicts */

	if (dns_response_parse_and_track(skb, tc->dns_off, tc->ifindex,
				       tc->server_ip, tc->source_port)) {
		dns_forward_response_to_user(skb, tc->ifindex);
		return TC_ACT_SHOT;
	}

	return TC_ACT_OK;
}

char __license[] SEC("license") = "Dual BSD/GPL";
