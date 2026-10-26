// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2022 Cube Authors */
#ifndef __NAT_H
#define __NAT_H

#include <vmlinux.h>
#include "cubevs.h"

enum nat_dir {
	NAT_DIR_SRC = 0,
	NAT_DIR_DST = 1,
};

static __always_inline long nat_rewrite(struct __sk_buff *skb, struct iphdr *l3,
					__u32 new_ip, enum nat_dir dir)
{
	bool first_frag, no_frag;
	__u32 offset, old_ip;
	__u16 frag_off;
	short ip_hlen;
	long err = -1;
	__u64 flags;

	old_ip = (dir == NAT_DIR_SRC) ? l3->saddr : l3->daddr;
	frag_off = l3->frag_off;
	no_frag = (frag_off & IP_FLAG_MF) == 0 && (frag_off & IP_FRAG_OFF_MASK) == 0;
	first_frag = (frag_off & IP_FLAG_MF) && (frag_off & IP_FRAG_OFF_MASK) == 0;

	if (l3->protocol != IPPROTO_ICMP && (no_frag || first_frag)) {
		/* Update L4 csum */
		ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
		ip_hlen <<= 2;

		switch (l3->protocol) {
		case IPPROTO_TCP:
			offset = TCP_CSUM_OFF(ip_hlen);
			flags = BPF_F_PSEUDO_HDR | sizeof(l3->saddr);
			break;
		case IPPROTO_UDP:
			offset = UDP_CSUM_OFF(ip_hlen);
			flags = BPF_F_MARK_MANGLED_0 | BPF_F_PSEUDO_HDR | sizeof(l3->saddr);
			break;
		default:
			return err;
		}

		err = bpf_l4_csum_replace(skb, offset, old_ip, new_ip, flags);
		if (err)
			return err;
	}

	/* Update L3 csum */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_ip, new_ip, sizeof(old_ip));
	if (err)
		return err;

	flags = 0;
	err = bpf_skb_store_bytes(skb,
				  (dir == NAT_DIR_SRC) ? IP_SADDR_OFF : IP_DADDR_OFF,
				  &new_ip, sizeof(new_ip), flags);
	if (err)
		return err;

	return 0;
}

#define snat(skb, l3, src_ip) nat_rewrite(skb, l3, src_ip, NAT_DIR_SRC)
#define dnat(skb, l3, dst_ip) nat_rewrite(skb, l3, dst_ip, NAT_DIR_DST)

#endif /* __NAT_H */
