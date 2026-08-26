// SPDX-License-Identifier: GPL-2.0
/* Copyright (c) 2022 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "cubevs.h"
#include "map.h"
#include "nat.h"
#include "skb.h"
#include "tcp.h"
#include "tcp_reset.h"

/* Update a CubeEgress TCP session from the original reply tuple before DNAT
 * rewrites the sandbox-assigned destination address to mvm_inner_ip. A missing
 * or non-L7 session is intentionally non-blocking.
 */
/* Update a CubeEgress TCP session from the original reply tuple before DNAT
 * rewrites the sandbox-assigned destination address to mvm_inner_ip.
 *
 * Returns the reset-redirect action when the reply belongs to a stale
 * (pre-rollback) L7 session — a RST is reflected to the proxy and the session
 * pair removed so the proxied connection is torn down instead of desyncing —
 * or 0 to continue normal delivery. A missing or non-L7 session is
 * intentionally non-blocking (returns 0).
 */
static __always_inline int update_l7_session_from_cubedev(struct __sk_buff *skb)
{
	struct session_key key = {};
	struct nat_session *sess;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct tcphdr *l4;
	__u64 now;

	if (!__pull_headers(skb, &l2, &l3, &l4))
		return 0;
	if (l3->protocol != IPPROTO_TCP)
		return 0;

	key.src_ip = l3->saddr;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = 0;
	key.protocol = IPPROTO_TCP;
	sess = lookup_session(&key);
	if (!sess || sess->packet_class != L7PROXY_PACKET)
		return 0;

	if (session_is_stale(sess)) {
		delete_session_pair(&key, sess);
		return tcp_send_reset(skb, skb->ingress_ifindex, l3->saddr);
	}

	now = bpf_ktime_get_ns();
	update_session(IP_CT_DIR_REPLY, sess, now, l4->syn, l4->ack,
		       l4->fin, l4->rst);
	return 0;
}

/* This filter will be attached to the egress path of cube-dev device.
 * It performs a DNAT and then redirect the traffics to Sandbox TAP devices.
 */
SEC("tc")
int from_envoy(struct __sk_buff *skb)
{
	__u32 *ifindex, daddr;
	struct ethhdr *l2;
	struct iphdr *l3;
	long err;
	int ret;

	if (skb->protocol != bpf_htons(ETH_P_IP))
		return TC_ACT_OK;

	/* A stale (pre-rollback) L7 session gets its RST reflected to the proxy;
	 * stop before delivering the reply to the guest. */
	ret = update_l7_session_from_cubedev(skb);
	if (ret)
		return ret;

	ret = pull_headers(skb, &l2, &l3);
	if (ret != TC_ACT_OK)
		return ret;

	daddr = l3->daddr;

	/* NAT and redirect */
	err = dnat(skb, l3, mvm_inner_ip);
	if (err)
		return TC_ACT_SHOT;

	ret = pull_headers(skb, &l2, &l3);
	if (ret != TC_ACT_OK)
		return ret;

	/* Keep IP_TRANSPARENT proxy replies sourced from the original remote IP.
	 * Only gateway-originated overlay traffic needs to be SNATed to the
	 * sandbox gateway address.
	 */
	if (l3->saddr == cubegw0_ip) {
		err = snat(skb, l3, mvm_gateway_ip);
		if (err)
			return TC_ACT_SHOT;
	}

	ifindex = bpf_map_lookup_elem(&mvmip_to_ifindex, &daddr);
	if (!ifindex)
		return TC_ACT_SHOT;

	return bpf_redirect(*ifindex, 0);
}

char __license[] SEC("license") = "Dual BSD/GPL";
