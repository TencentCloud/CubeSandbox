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
