// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#ifndef __TCP_RESET_H
#define __TCP_RESET_H

#include "cubevs.h"
#include "l2l3.h"
#include "skb.h"

/* Shared helpers for building a TCP RST from a triggering packet. These were
 * moved out of mvmtap.bpf.c so the reverse (peer/proxy-facing) programs can
 * reflect a reset, not just the guest-facing one. */

static __always_inline bool tcp_segment_len(const struct iphdr *l3, const struct tcphdr *l4,
					    __u32 *seg_len)
{
	__u16 ip_hlen, tcp_hlen, total_len;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	tcp_hlen = BPF_CORE_READ_BITFIELD(l4, doff);
	tcp_hlen <<= 2;
	total_len = bpf_ntohs(l3->tot_len);
	if (ip_hlen < sizeof(struct iphdr) || tcp_hlen < sizeof(struct tcphdr) ||
	    total_len < ip_hlen + tcp_hlen)
		return false;

	*seg_len = total_len - ip_hlen - tcp_hlen;
	if (l4->syn)
		(*seg_len)++;
	if (l4->fin)
		(*seg_len)++;

	return true;
}

/* tcp_ipv4_set_checksum computes the TCP checksum (pseudo-header + TCP header)
 * into the check field, which the caller must have written as zero.
 */
static __always_inline int tcp_ipv4_set_checksum(struct __sk_buff *skb,
						 __u32 tcp_csum_off,
						 __be32 saddr, __be32 daddr,
						 const struct tcphdr *tcp)
{
	const __u32 *words = (const __u32 *)tcp;
	/* zero | proto | length, in network byte order */
	__be32 proto_len = bpf_htonl(((__u32)IPPROTO_TCP << 16) | sizeof(*tcp));
	__u64 ph_flags = BPF_F_PSEUDO_HDR | sizeof(__u32);
	__u64 hdr_flags = sizeof(__u32);
	long err;

	/* Pseudo-header words */
	err = bpf_l4_csum_replace(skb, tcp_csum_off, 0, saddr, ph_flags);
	if (err)
		return err;
	err = bpf_l4_csum_replace(skb, tcp_csum_off, 0, daddr, ph_flags);
	if (err)
		return err;
	err = bpf_l4_csum_replace(skb, tcp_csum_off, 0, proto_len, ph_flags);
	if (err)
		return err;

	/* TCP header words: source|dest, seq, ack_seq, doff/flags/window,
	 * check|urg_ptr. The check word currently contains 0 (caller wrote
	 * a zeroed check field), so adding it is a no-op for the csum.
	 */
	err = bpf_l4_csum_replace(skb, tcp_csum_off, 0, words[0], hdr_flags);
	if (err)
		return err;
	err = bpf_l4_csum_replace(skb, tcp_csum_off, 0, words[1], hdr_flags);
	if (err)
		return err;
	err = bpf_l4_csum_replace(skb, tcp_csum_off, 0, words[2], hdr_flags);
	if (err)
		return err;
	err = bpf_l4_csum_replace(skb, tcp_csum_off, 0, words[3], hdr_flags);
	if (err)
		return err;
	return bpf_l4_csum_replace(skb, tcp_csum_off, 0, words[4], hdr_flags);
}

static __always_inline int rewrite_l3_tot_len(struct __sk_buff *skb,
					      __be16 old_tot_len, __be16 new_tot_len)
{
	long err;

	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_tot_len, new_tot_len,
				  sizeof(new_tot_len));
	if (err)
		return err;

	return bpf_skb_store_bytes(skb, IP_TOT_LEN_OFF, &new_tot_len,
				   sizeof(new_tot_len), 0);
}

/* tcp_send_reset builds a TCP RST from the triggering packet and redirects it
 * out out_ifindex.
 *
 * The reply mirrors the trigger except for its destination: new_saddr is the
 * address the sender talked to (the trigger's destination), ports and MACs are
 * swapped, and new_daddr is supplied by the caller. Sequence numbers follow
 * RFC 793/5961: if the trigger had ACK set, the RST's seq is that ack value
 * (which the sender accepts as within its window); otherwise the RST carries
 * ACK = trigger.seq + seg_len. Returns the bpf_redirect action, or TC_ACT_SHOT
 * when the packet can't be safely rewritten (GSO, fragmented, or a RST).
 *
 * Callers pick new_daddr per direction: peer/proxy-facing paths (nodenic /
 * localgw) pass the trigger's source (l3->saddr, i.e. back to the sender);
 * guest-facing paths (mvmtap) pass mvm_inner_ip, because from_cube rewrites the
 * source to the sandbox's real IP before the reset, so the trigger's source is
 * no longer the guest's TAP-side address.
 */
static __always_inline int tcp_send_reset(struct __sk_buff *skb, __u32 out_ifindex,
					  __be32 new_daddr)
{
	struct tcphdr new_tcp = {};
	union macaddr old_smac, old_dmac;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct tcphdr *l4;
	__be32 old_saddr, old_daddr, new_saddr;
	__be16 old_tot_len, new_tot_len;
	__u32 seq, ack_seq, new_skb_len;
	__u32 seg_len, tcp_off, tcp_csum_off;
	__u16 ip_hlen, new_ip_len;
	long err;

	/* bpf_skb_change_tail() may fail on GSO skbs or leave segmentation
	 * state inconsistent. Fall back to drop instead of sending RST. */
	if (skb->gso_segs)
		return TC_ACT_SHOT;

	if (!__pull_headers(skb, &l2, &l3, &l4))
		return TC_ACT_SHOT;

	if ((l3->frag_off & IP_FLAG_MF) || (l3->frag_off & IP_FRAG_OFF_MASK))
		return TC_ACT_SHOT;

	/* Never send a reset in response to a reset. */
	if (l4->rst)
		return TC_ACT_SHOT;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	seq = l4->seq;
	ack_seq = l4->ack_seq;
	if (!tcp_segment_len(l3, l4, &seg_len))
		return TC_ACT_SHOT;

	/* The reply comes from the address the sender talked to; new_daddr is the
	 * caller-chosen destination. */
	new_saddr = l3->daddr;
	new_tcp.source = l4->dest;
	new_tcp.dest = l4->source;
	new_tcp.doff = sizeof(new_tcp) >> 2;
	new_tcp.rst = 1;
	if (l4->ack) {
		new_tcp.seq = ack_seq;
	} else {
		new_tcp.ack_seq = bpf_htonl(bpf_ntohl(seq) + seg_len);
		new_tcp.ack = 1;
	}

	new_ip_len = ip_hlen + sizeof(new_tcp);
	new_skb_len = sizeof(struct ethhdr) + new_ip_len;
	if (bpf_skb_change_tail(skb, new_skb_len, 0))
		return TC_ACT_SHOT;

	/* bpf_skb_change_tail invalidates all packet pointers. */
	if (!__pull_headers(skb, &l2, &l3, &l4))
		return TC_ACT_SHOT;

	/* Snapshot fields and swap MACs before helper calls invalidate the
	 * packet pointers: the reply leaves with src = our interface MAC and
	 * dst = the last-hop sender's MAC. */
	old_saddr = l3->saddr;
	old_daddr = l3->daddr;
	old_tot_len = l3->tot_len;
	old_smac = *(union macaddr *)l2->h_source;
	old_dmac = *(union macaddr *)l2->h_dest;
	new_tot_len = bpf_htons(new_ip_len);
	tcp_off = sizeof(struct ethhdr) + ip_hlen;
	tcp_csum_off = TCP_CSUM_OFF(ip_hlen);
	set_mac_pair(l2, old_dmac.p1, old_dmac.p2, old_smac.p1, old_smac.p2);

	/* Write the new TCP header (with check = 0). */
	err = bpf_skb_store_bytes(skb, tcp_off, &new_tcp, sizeof(new_tcp), 0);
	if (err)
		return TC_ACT_SHOT;
	err = rewrite_l3_tot_len(skb, old_tot_len, new_tot_len);
	if (err)
		return TC_ACT_SHOT;
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_saddr, new_saddr,
				  sizeof(new_saddr));
	if (err)
		return TC_ACT_SHOT;
	err = bpf_skb_store_bytes(skb, IP_SADDR_OFF, &new_saddr,
				  sizeof(new_saddr), 0);
	if (err)
		return TC_ACT_SHOT;
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_daddr, new_daddr,
				  sizeof(new_daddr));
	if (err)
		return TC_ACT_SHOT;
	err = bpf_skb_store_bytes(skb, IP_DADDR_OFF, &new_daddr,
				  sizeof(new_daddr), 0);
	if (err)
		return TC_ACT_SHOT;
	err = tcp_ipv4_set_checksum(skb, tcp_csum_off, new_saddr, new_daddr,
				    &new_tcp);
	if (err)
		return TC_ACT_SHOT;

	return bpf_redirect(out_ifindex, 0);
}

#endif /* __TCP_RESET_H */
