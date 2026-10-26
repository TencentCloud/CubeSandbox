// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "tcp.h"

struct tcp_update_step {
	__u8 dir;
	__u8 syn;
	__u8 ack;
	__u8 fin;
	__u8 rst;
	__u8 reserved[3];
};

struct tcp_update_case {
	__u64 access_time;
	__u64 now_ns;
	__u8 state;
	__u8 active_close;
	__u8 step_count;
	__u8 reserved[5];
	struct tcp_update_step steps[2];
};

#define APPLY_TCP_STEP(DIR, SESS, NOW, STEP) do { \
	if ((STEP).rst) \
		update_session((DIR), (SESS), (NOW), false, false, false, true); \
	else if ((STEP).syn && (STEP).ack) \
		update_session((DIR), (SESS), (NOW), true, true, false, false); \
	else if ((STEP).syn) \
		update_session((DIR), (SESS), (NOW), true, false, false, false); \
	else if ((STEP).fin) \
		update_session((DIR), (SESS), (NOW), false, false, true, false); \
	else if ((STEP).ack) \
		update_session((DIR), (SESS), (NOW), false, true, false, false); \
	else \
		update_session((DIR), (SESS), (NOW), false, false, false, false); \
} while (0)

SEC("tc")
int test_update_session(struct __sk_buff *skb)
{
	struct tcp_update_case tc = {};
	struct nat_session sess = {};
	int i;

	if (bpf_skb_load_bytes(skb, 0, &tc, sizeof(tc)))
		return TC_ACT_SHOT;
	if (tc.state > TCP_CONNTRACK_SYN_SENT2 || tc.step_count > 2)
		return TC_ACT_SHOT;

	sess.access_time = tc.access_time;
	sess.state = tc.state;
	sess.active_close = tc.active_close;

#pragma unroll
	for (i = 0; i < 2; i++) {
		if (i < tc.step_count) {
			if (tc.steps[i].dir == IP_CT_DIR_ORIGINAL)
				APPLY_TCP_STEP(IP_CT_DIR_ORIGINAL, &sess, tc.now_ns + i,
					       tc.steps[i]);
			else if (tc.steps[i].dir == IP_CT_DIR_REPLY)
				APPLY_TCP_STEP(IP_CT_DIR_REPLY, &sess, tc.now_ns + i,
					       tc.steps[i]);
			else
				return TC_ACT_SHOT;
		}
	}

	tc.access_time = sess.access_time;
	tc.state = sess.state;
	tc.active_close = sess.active_close;
	if (bpf_skb_store_bytes(skb, 0, &tc, sizeof(tc), 0))
		return TC_ACT_SHOT;

	return TC_ACT_OK;
}

char __license[] SEC("license") = "Dual BSD/GPL";
