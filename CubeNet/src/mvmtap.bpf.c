// SPDX-License-Identifier: GPL-2.0
/* Copyright (c) 2022 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "cubevs.h"
#include "l2l3.h"
#include "nat.h"
#include "icmp.h"
#include "jhash.h"
#include "map.h"
#include "skb.h"
#include "tcp.h"
#include "udp.h"
#include "dns_query.h"

/*
 * Handle ARP request and send ARP reply
 * This function performs ARP proxy (ARP spoofing) to answer ARP requests
 * from Sandbox with the gateway MAC address.
 *
 * Returns:
 *   TC_ACT_SHOT - if the packet should be dropped
 *   >= 0        - if the packet was handled (ARP reply sent)
 */
static __always_inline int handle_arp(struct __sk_buff *skb, __u32 ifindex)
{
	union macaddr *macaddr, tmp_macaddr;
	struct ethhdr *eth;
	struct arphdr_eth *arp;
	void *data, *data_end;
	__u32 len, ip;
	long err;

	/* Pull ARP packet headers */
	len = sizeof(struct ethhdr) + sizeof(struct arphdr_eth);
	err = bpf_skb_pull_data(skb, len);
	if (err)
		return TC_ACT_SHOT;

	data = (void *)(__u64)skb->data;
	data_end = (void *)(__u64)skb->data_end;

	if (data + len > data_end)
		return TC_ACT_SHOT;

	eth = data;
	arp = (struct arphdr_eth *)(eth + 1);

	/* Only handle Ethernet/IPv4 ARP requests */
	/* clang-format off */
	if (arp->ar_hrd != bpf_htons(ARPHRD_ETHER) ||
	    arp->ar_pro != bpf_htons(ETH_P_IP) ||
	    arp->ar_hln != ETH_ALEN ||
	    arp->ar_pln != sizeof(__be32) ||
	    arp->ar_op != bpf_htons(ARPOP_REQUEST))
		return TC_ACT_SHOT;
	/* clang-format on */

	/* Build ARP reply */
	arp->ar_op = bpf_htons(ARPOP_REPLY);

	ip = arp->ar_sip;
	arp->ar_sip = arp->ar_tip;
	arp->ar_tip = ip;

	macaddr = (union macaddr *)arp->ar_sha;
	tmp_macaddr.p1 = macaddr->p1;
	tmp_macaddr.p2 = macaddr->p2;
	/* Use gateway MAC as the sender (ARP proxy) */
	macaddr->p1 = cubegw0_macaddr_p1;
	macaddr->p2 = cubegw0_macaddr_p2;
	macaddr = (union macaddr *)arp->ar_tha;
	macaddr->p1 = tmp_macaddr.p1;
	macaddr->p2 = tmp_macaddr.p2;

	/* Update Ethernet header */
	macaddr = (union macaddr *)eth->h_source;
	tmp_macaddr.p1 = macaddr->p1;
	tmp_macaddr.p2 = macaddr->p2;
	macaddr->p1 = cubegw0_macaddr_p1;
	macaddr->p2 = cubegw0_macaddr_p2;
	macaddr = (union macaddr *)eth->h_dest;
	macaddr->p1 = tmp_macaddr.p1;
	macaddr->p2 = tmp_macaddr.p2;

	/* Send the reply back to the same interface */
	return bpf_redirect(ifindex, 0);
}

static __always_inline bool should_do_nat(const struct iphdr *l3)
{
	__u16 frag_off;

	/* Support TCP, UDP, and ICMP */
	if (l3->protocol != IPPROTO_TCP && l3->protocol != IPPROTO_UDP && l3->protocol != IPPROTO_ICMP)
		return false;

	frag_off = l3->frag_off;
	if ((frag_off & IP_FLAG_MF) || (frag_off & IP_FRAG_OFF_MASK))
		return false;

	return true;
}

/*
 * Check whether a TCP flow should be redirected to the L7 proxy.
 *
 * Looks up allow_out_v2 for the given ifindex/daddr and returns true iff
 * the entry carries NET_POLICY_FLAG_L7_REQUIRED and the destination port
 * is 80 or 443. This is a fast, self-contained lookup — the general
 * egress policy check (allow / deny) is enforced later inside
 * create_nat_session().
 */
static __always_inline bool should_redirect_to_l7_proxy(__u32 ifindex, __u32 daddr,
							const struct tcphdr *l4)
{
	struct lpm_key key = { .prefixlen = 32, .ip = daddr };
	struct net_policy_value_v2 *value;
	void *inner_map;

	if (l4->dest != bpf_htons(80) && l4->dest != bpf_htons(443))
		return false;

	inner_map = bpf_map_lookup_elem(&allow_out_v2, &ifindex);
	if (!inner_map)
		return false;

	value = bpf_map_lookup_elem(inner_map, &key);
	if (!value)
		return false;
	if (value->expires_at_ns != 0 && value->expires_at_ns <= bpf_ktime_get_ns())
		return false;

	return value->flags & NET_POLICY_FLAG_L7_REQUIRED;
}

enum tcp_nat_result {
	TCP_NAT_DROP = 0,
	TCP_NAT_OK,
	TCP_NAT_RESET,
};

/* do_tcp_nat() returns a 64-bit value that encodes both the status enum
 * (low 32 bits) and the destination ifindex (upper 32 bits). This avoids
 * passing the ifindex through a stack pointer arg, which older BPF
 * verifiers do not track across subprog calls.
 */
#define TCP_NAT_PACK(ifindex, status) \
	((((__u64)(ifindex)) << 32) | (__u32)(status))
#define TCP_NAT_STATUS(ret)	((enum tcp_nat_result)((__u32)(ret)))
#define TCP_NAT_IFINDEX(ret)	((__u32)((__u64)(ret) >> 32))

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

/* Update the IP tot_len field together with the IP header checksum.
 * Uses bpf_l3_csum_replace for the incremental csum update and
 * bpf_skb_store_bytes to write the new value, instead of touching the
 * packet pointer directly.
 */
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

/* Set the TCP checksum field of a freshly written TCP header.
 *
 * Pre-condition: the TCP header at @tcp_csum_off..+sizeof(*tcp) is already
 * present in skb with the check field cleared to 0. This helper folds the
 * IPv4 pseudo-header (saddr, daddr, proto, length) and the on-wire TCP
 * header bytes into the checksum via bpf_l4_csum_replace.
 *
 * Per the bpf-helpers(7) man page, calling bpf_l4_csum_replace with
 * from = 0 makes the helper recompute the csum against `to` only — i.e.
 * accumulates `to` into the existing in-place checksum. We start from a
 * zeroed check field and add each 32-bit word in turn.
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

static __always_inline int tcp_reply_reset(struct __sk_buff *skb, __u32 ifindex)
{
	struct tcphdr new_tcp = {};
	struct ethhdr *l2;
	struct iphdr *l3;
	struct tcphdr *l4;
	__be32 old_saddr, old_daddr, new_saddr, new_daddr;
	__be16 old_tot_len, new_tot_len;
	__u32 seq, ack_seq, new_skb_len;
	__u32 seg_len, tcp_off, tcp_csum_off;
	__u16 ip_hlen, new_ip_len;
	long err;

	/* bpf_skb_change_tail() may fail on GSO skbs or leave segmentation
	 * state inconsistent. Fall back to drop instead of sending RST.
	 */
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

	new_saddr = l3->daddr;
	new_daddr = mvm_inner_ip;
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
	/* Build the new TCP header with check = 0; tcp_ipv4_set_checksum()
	 * folds the pseudo-header and TCP header words into the checksum
	 * incrementally via bpf_l4_csum_replace.
	 */

	new_ip_len = ip_hlen + sizeof(new_tcp);
	new_skb_len = sizeof(struct ethhdr) + new_ip_len;
	if (bpf_skb_change_tail(skb, new_skb_len, 0))
		return TC_ACT_SHOT;

	/* bpf_skb_change_tail invalidates all packet pointers. */
	if (!__pull_headers(skb, &l2, &l3, &l4))
		return TC_ACT_SHOT;

	/* Snapshot old IP header fields and rewrite the L2 MAC pair before
	 * touching the packet via BPF helpers — bpf_skb_store_bytes() and
	 * bpf_l{3,4}_csum_replace() invalidate l2/l3/l4 pointers.
	 */
	old_saddr = l3->saddr;
	old_daddr = l3->daddr;
	old_tot_len = l3->tot_len;
	new_tot_len = bpf_htons(new_ip_len);
	tcp_off = sizeof(struct ethhdr) + ip_hlen;
	tcp_csum_off = TCP_CSUM_OFF(ip_hlen);
	set_mac_pair(l2, cubegw0_macaddr_p1, cubegw0_macaddr_p2,
		     mvm_macaddr_p1, mvm_macaddr_p2);

	/* Write the new TCP header (with check = 0). */
	err = bpf_skb_store_bytes(skb, tcp_off, &new_tcp, sizeof(new_tcp), 0);
	if (err)
		return TC_ACT_SHOT;

	/* Update IP tot_len + IP csum. */
	err = rewrite_l3_tot_len(skb, old_tot_len, new_tot_len);
	if (err)
		return TC_ACT_SHOT;

	/* Update IP saddr + IP csum. */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_saddr, new_saddr,
				  sizeof(new_saddr));
	if (err)
		return TC_ACT_SHOT;
	err = bpf_skb_store_bytes(skb, IP_SADDR_OFF, &new_saddr,
				  sizeof(new_saddr), 0);
	if (err)
		return TC_ACT_SHOT;

	/* Update IP daddr + IP csum. */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_daddr, new_daddr,
				  sizeof(new_daddr));
	if (err)
		return TC_ACT_SHOT;
	err = bpf_skb_store_bytes(skb, IP_DADDR_OFF, &new_daddr,
				  sizeof(new_daddr), 0);
	if (err)
		return TC_ACT_SHOT;

	/* Compute the TCP checksum into the (currently zero) check field. */
	err = tcp_ipv4_set_checksum(skb, tcp_csum_off, new_saddr, new_daddr,
				    &new_tcp);
	if (err)
		return TC_ACT_SHOT;

	return bpf_redirect(ifindex, 0);
}

static __always_inline struct snat_ip *pick_snat_ip_port(__u32 mvm_ip, const struct session_key *ekey,
							 __u16 *selected_port)
{
	static const int max_retries = 10;
	struct ingress_session isess = {
		.version = ekey->version,
		.vm_ip = ekey->src_ip,
		.vm_port = ekey->src_port,
	};
	struct session_key ikey = {};
	struct snat_ip *snat_ip;
	__u16 snat_port;
	__u32 index;
	int i;

	index = jhash_1word(mvm_ip, HASH_SEED) % MAX_SNAT_IPS;
	snat_ip = bpf_map_lookup_elem(&snat_iplist, &index);
	if (!snat_ip)
		return NULL;

	ikey.src_ip = ekey->dst_ip;
	ikey.dst_ip = snat_ip->ip;
	ikey.src_port = ekey->dst_port;
	ikey.version = 0;
	ikey.protocol = ekey->protocol;
	for (i = 0; i < max_retries; i++) {
		bpf_spin_lock(&snat_ip->lock);
		snat_port = snat_ip->max_port;
		if (snat_ip->max_port == 0xffff)
			snat_ip->max_port = MAX_PORT_START;
		else
			snat_ip->max_port++;
		bpf_spin_unlock(&snat_ip->lock);

		ikey.dst_port = bpf_htons(snat_port);
		/* update with BPF_NOEXIST to take the slot without race */
		if (!bpf_map_update_elem(&ingress_sessions, &ikey, &isess, BPF_NOEXIST)) {
			/* at this point, we have ingress session created */
			*selected_port = bpf_htons(snat_port);
			return snat_ip;
		}
	}

	return NULL;
}

static __always_inline void del_session(struct session_key *ekey, struct nat_session *sess)
{
	struct session_key ikey = {
		.src_ip = ekey->dst_ip,
		.dst_ip = sess->node_ip,
		.src_port = ekey->dst_port,
		.dst_port = sess->node_port,
		.version = 0,
		.protocol = ekey->protocol,
	};

	bpf_map_delete_elem(&egress_sessions, ekey);
	bpf_map_delete_elem(&ingress_sessions, &ikey);
}

/* Returns the destination ifindex on success, or 0 on failure.
 * Returning the value (rather than writing through a pointer arg) avoids
 * "invalid read from stack" errors on older BPF verifiers that do not
 * propagate subprog pointer-arg writes back to the caller's stack slot.
 */
static __always_inline __u32 do_icmp_nat(struct __sk_buff *skb, struct mvm_meta *mvm_meta)
{
	__u32 old_saddr, new_saddr, icmp_csum_off;
	__u16 old_id, new_id;
	struct session_key key = {};
	struct nat_session *sess;
	struct snat_ip *snat_ip;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct icmphdr *l4;
	__u16 ip_hlen;
	__u16 snat_id;
	__u64 flags;
	__u64 now;
	long err;
	bool ok;

	if (!__pull_headers_icmp(skb, &l2, &l3, &l4))
		return 0;

	/* Only handle Echo Request outbound; drop other ICMP types */
	if (l4->type != ICMP_ECHO)
		return 0;

	now = bpf_ktime_get_ns();
	/* Use ICMP identifier as the "port" identifier in the session key */
	key.src_ip = mvm_meta->ip;
	key.dst_ip = l3->daddr;
	key.src_port = l4->un.echo.id; /* identifier (network byte order) */
	key.dst_port = 0;
	key.version = mvm_meta->version;
	key.protocol = IPPROTO_ICMP;

	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (sess) {
		update_icmp_session(IP_CT_DIR_ORIGINAL, sess, now);
		goto do_nat;
	}

	/* create new session */
	snat_ip = pick_snat_ip_port(mvm_meta->ip, &key, &snat_id);
	if (!snat_ip || !snat_ip->ip || !snat_id)
		return 0;
	ok = create_icmp_sessions(skb, &key, now, skb->ingress_ifindex, snat_ip, snat_id);
	if (!ok)
		return 0;
	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (!sess)
		return 0;

do_nat:
	old_saddr = l3->saddr;
	new_saddr = sess->node_ip;
	old_id = l4->un.echo.id;
	new_id = sess->node_port;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	icmp_csum_off = ICMP_CSUM_OFF(ip_hlen);

	/* update L2 first: csum/store helpers may invalidate packet pointers */
	set_mac_pair(l2, egress_smacaddr_p1, egress_smacaddr_p2,
		     egress_dmacaddr_p1, egress_dmacaddr_p2);

	/* update ICMP csum: ICMP has no pseudo-header, so no BPF_F_PSEUDO_HDR.
	 * Only the echo identifier change affects the csum (IP saddr is not
	 * covered by ICMP checksum).
	 */
	flags = sizeof(old_id);
	err = bpf_l4_csum_replace(skb, icmp_csum_off, old_id, new_id, flags);
	if (err)
		return 0;

	/* write the new ICMP echo identifier */
	err = bpf_skb_store_bytes(skb, ICMP_ECHO_ID_OFF(ip_hlen), &new_id, sizeof(new_id), 0);
	if (err)
		return 0;

	/* update IP csum and write new saddr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_saddr, new_saddr, sizeof(old_saddr));
	if (err)
		return 0;

	err = bpf_skb_store_bytes(skb, IP_SADDR_OFF, &new_saddr, sizeof(new_saddr), 0);
	if (err)
		return 0;

	return sess->node_ifindex;
}

/* Core UDP NAT implementation as a forced-inline helper.
 *
 * Returns the destination ifindex on success, or 0 on failure. Returning a
 * value (rather than writing through a pointer arg) avoids "invalid read
 * from stack" errors on older BPF verifiers that don't propagate subprog
 * pointer-arg writes back to the caller's stack slot.
 *
 * Inlining this body matters for from_cube(), which already contains a
 * bpf_tail_call() (via the inlined dns_handle_query). Older kernels reject
 * "tail_calls in programs with bpf-to-bpf calls", so from_cube() must have
 * no subprog calls.
 */
static __always_inline __u32 do_udp_nat_inline(struct __sk_buff *skb,
					       struct mvm_meta *mvm_meta)
{
	__u32 old_saddr, new_saddr, udp_csum_off;
	__u16 old_sport, new_sport, old_csum;
	struct session_key key = {};
	struct nat_session *sess;
	struct snat_ip *snat_ip;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct udphdr *l4;
	__u16 ip_hlen;
	__u16 snat_port;
	__u64 flags;
	__u64 now;
	long err;
	bool ok;

	if (!__pull_headers_udp(skb, &l2, &l3, &l4))
		return 0;

	now = bpf_ktime_get_ns();
	key.src_ip = mvm_meta->ip;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = mvm_meta->version;
	key.protocol = IPPROTO_UDP;

	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (sess) {
		update_udp_session(IP_CT_DIR_ORIGINAL, sess, now);
		goto do_nat;
	}

	/* create new session */
	snat_ip = pick_snat_ip_port(mvm_meta->ip, &key, &snat_port);
	if (!snat_ip || !snat_ip->ip || !snat_port)
		return 0;
	ok = create_udp_sessions(skb, &key, now, skb->ingress_ifindex, snat_ip, snat_port);
	if (!ok)
		return 0;
	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (!sess)
		return 0;

do_nat:
	old_saddr = l3->saddr;
	new_saddr = sess->node_ip;
	old_sport = l4->source;
	new_sport = sess->node_port;
	old_csum = l4->check;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	udp_csum_off = UDP_CSUM_OFF(ip_hlen);

	/* update L2 first: csum/store helpers may invalidate packet pointers */
	set_mac_pair(l2, egress_smacaddr_p1, egress_smacaddr_p2,
		     egress_dmacaddr_p1, egress_dmacaddr_p2);

	/* update UDP csum only if it was non-zero (UDP csum is optional over IPv4).
	 * BPF_F_MARK_MANGLED_0 keeps a 0 csum (= disabled) intact in case the
	 * incremental update would yield 0; the helper rewrites it to 0xffff.
	 * IP saddr is part of UDP pseudo-header, so BPF_F_PSEUDO_HDR is required.
	 */
	if (old_csum) {
		flags = BPF_F_PSEUDO_HDR | BPF_F_MARK_MANGLED_0 | sizeof(old_saddr);
		err = bpf_l4_csum_replace(skb, udp_csum_off, old_saddr, new_saddr, flags);
		if (err)
			return 0;

		/* port is not part of pseudo-header */
		flags = BPF_F_MARK_MANGLED_0 | sizeof(old_sport);
		err = bpf_l4_csum_replace(skb, udp_csum_off, old_sport, new_sport, flags);
		if (err)
			return 0;
	}

	/* write new UDP source port */
	err = bpf_skb_store_bytes(skb, UDP_SRC_OFF(ip_hlen), &new_sport, sizeof(new_sport), 0);
	if (err)
		return 0;

	/* update IP csum and write new saddr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_saddr, new_saddr, sizeof(old_saddr));
	if (err)
		return 0;

	err = bpf_skb_store_bytes(skb, IP_SADDR_OFF, &new_saddr, sizeof(new_saddr), 0);
	if (err)
		return 0;

	return sess->node_ifindex;
}

/* Non-inline wrapper used by dns_finish.
 *
 * dns_finish reaches the UDP NAT path with a verifier state that already
 * carries the dns_hash_qname loop complexity. Inlining the NAT body there
 * causes the verifier to blow past its 1M-insn complexity limit on 5.4
 * kernels. Keeping a real subprog isolates the verification cost.
 *
 * __noinline + noinline attribute force the compiler to keep this as a
 * real bpf-to-bpf call even with only one caller.
 */
static __noinline __attribute__((noinline)) __u32 do_udp_nat(struct __sk_buff *skb,
							     struct mvm_meta *mvm_meta)
{
	return do_udp_nat_inline(skb, mvm_meta);
}

/* Inline version: redirects on UDP NAT success. Used by from_cube(), which
 * cannot make bpf-to-bpf calls (see do_udp_nat_inline()'s comment).
 */
static __always_inline int finish_udp_nat_inline(struct __sk_buff *skb,
						 struct mvm_meta *mvm_meta)
{
	__u32 dst_ifindex = do_udp_nat_inline(skb, mvm_meta);

	if (dst_ifindex)
		return bpf_redirect(dst_ifindex, egress_redirect_flags);

	return TC_ACT_SHOT;
}

/* Subprog-based version used by dns_finish. */
static __always_inline int finish_udp_nat(struct __sk_buff *skb, struct mvm_meta *mvm_meta)
{
	__u32 dst_ifindex = do_udp_nat(skb, mvm_meta);

	if (dst_ifindex)
		return bpf_redirect(dst_ifindex, egress_redirect_flags);

	return TC_ACT_SHOT;
}

/* Returns a packed value: see TCP_NAT_PACK / TCP_NAT_STATUS / TCP_NAT_IFINDEX.
 * Returning the ifindex via the upper bits (rather than through a pointer
 * arg) avoids "invalid read from stack" errors on older BPF verifiers that
 * do not propagate subprog pointer-arg writes back to the caller's stack.
 */
static __always_inline __u64 do_tcp_nat(struct __sk_buff *skb, struct mvm_meta *mvm_meta)
{
	__u32 old_saddr, new_saddr, tcp_csum_off;
	__u16 old_sport, new_sport;
	struct session_key key = {};
	struct nat_session *sess;
	struct snat_ip *snat_ip;
	bool syn, ack, fin, rst;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct tcphdr *l4;
	__u16 ip_hlen;
	__u16 snat_port;
	__u64 flags;
	__u64 now;
	long err;
	bool ok;

	if (!__pull_headers(skb, &l2, &l3, &l4))
		return TCP_NAT_DROP;

	now = bpf_ktime_get_ns();
	syn = l4->syn;
	ack = l4->ack;
	fin = l4->fin;
	rst = l4->rst;
	key.src_ip = mvm_meta->ip;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = mvm_meta->version;
	key.protocol = l3->protocol;
	if (syn && !ack && !fin && !rst) {
		/* retransmission */
		sess = bpf_map_lookup_elem(&egress_sessions, &key);
		if (sess) {
			if (sess->state == TCP_CONNTRACK_CLOSE || sess->state == TCP_CONNTRACK_TIME_WAIT) {
				/* guest kernel reuse source port too fast */
				del_session(&key, sess);
				goto do_create;
			}

			goto do_update;
		}
do_create:
		/* create new session */
		snat_ip = pick_snat_ip_port(mvm_meta->ip, &key, &snat_port);
		if (!snat_ip || !snat_ip->ip || !snat_port)
			return TCP_NAT_DROP;
		ok = create_new_sessions(skb, &key, now, skb->ingress_ifindex, snat_ip, snat_port);
		if (!ok) {
			/* Preserve RST-on-deny: create_nat_session stamps
			 * skb->cb when the failure is due to net policy.
			 */
			if (nat_cb_get(skb) == NAT_CB_DENIED_BY_POLICY)
				return TCP_NAT_PACK(0, TCP_NAT_RESET);
			return TCP_NAT_DROP;
		}
		sess = bpf_map_lookup_elem(&egress_sessions, &key);
		if (!sess)
			return TCP_NAT_DROP;
		goto do_nat;
	} else {
		/* lookup existing session */
		sess = bpf_map_lookup_elem(&egress_sessions, &key);
		if (!sess)
			return rst ? TCP_NAT_DROP : TCP_NAT_RESET;
	}

do_update:
	/* update session */
	update_session(IP_CT_DIR_ORIGINAL, sess, now, syn, ack, fin, rst);

do_nat:
	old_saddr = l3->saddr;
	new_saddr = sess->node_ip;
	old_sport = l4->source;
	new_sport = sess->node_port;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	tcp_csum_off = TCP_CSUM_OFF(ip_hlen);

	/* update L2 first: csum/store helpers may invalidate packet pointers */
	set_mac_pair(l2, egress_smacaddr_p1, egress_smacaddr_p2,
		     egress_dmacaddr_p1, egress_dmacaddr_p2);

	/* update TCP csum: IP saddr is part of pseudo-header, so BPF_F_PSEUDO_HDR */
	flags = BPF_F_PSEUDO_HDR | sizeof(old_saddr);
	err = bpf_l4_csum_replace(skb, tcp_csum_off, old_saddr, new_saddr, flags);
	if (err)
		return TCP_NAT_DROP;

	/* update TCP csum for port change (not part of pseudo-header) */
	flags = sizeof(old_sport);
	err = bpf_l4_csum_replace(skb, tcp_csum_off, old_sport, new_sport, flags);
	if (err)
		return TCP_NAT_DROP;

	/* write new TCP source port */
	err = bpf_skb_store_bytes(skb, TCP_SRC_OFF(ip_hlen), &new_sport, sizeof(new_sport), 0);
	if (err)
		return TCP_NAT_DROP;

	/* update IP csum and write new saddr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_saddr, new_saddr, sizeof(old_saddr));
	if (err)
		return TCP_NAT_DROP;

	err = bpf_skb_store_bytes(skb, IP_SADDR_OFF, &new_saddr, sizeof(new_saddr), 0);
	if (err)
		return TCP_NAT_DROP;

	return TCP_NAT_PACK(sess->node_ifindex, TCP_NAT_OK);
}

static __always_inline bool dns_policy_enabled(const struct mvm_meta *mvm_meta)
{
	return mvm_meta && mvm_meta->dns_policy_flags;
}

/* Parse one DNS QNAME chunk and dispatch to reverse or finish stage. */
SEC("tc")
int dns_parse_chunk(struct __sk_buff *skb)
{
	struct dns_query_state *state;
	__u32 key = 0;

	state = bpf_map_lookup_elem(&dns_query_state, &key);
	if (!state)
		return TC_ACT_SHOT;

	dns_parse_query_name_chunk(skb, state);
	if (state->failed)
		goto finish;
	if (state->done) {
		if (state->label_remaining != 0 || state->dotted_len == 0 ||
		    state->dotted_len >= DNS_MAX_NAME_LEN)
			state->failed = true;
		goto reverse;
	}

	bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_PARSE);
	state->failed = true;
	goto finish;

reverse:
	bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_REVERSE);
	state->failed = true;

finish:
	bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_FINISH);
	return TC_ACT_SHOT;
}

/* Reverse one DNS QNAME chunk into the trie lookup key. */
SEC("tc")
int dns_rev_chunk(struct __sk_buff *skb)
{
	struct dns_allow_key *question;
	struct dns_query_state *state;
	__u32 key = 0;

	state = bpf_map_lookup_elem(&dns_query_state, &key);
	question = bpf_map_lookup_elem(&dns_query_scratch, &key);
	if (!state || !question)
		return TC_ACT_SHOT;

	if (state->failed || dns_reverse_query_name_chunk(state, question))
		goto finish;

	bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_REVERSE);
	state->failed = true;

finish:
	bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_FINISH);
	return TC_ACT_SHOT;
}

/* Finish DNS query filtering and continue UDP NAT for allowed queries. */
SEC("tc")
int dns_finish(struct __sk_buff *skb)
{
	struct dns_allow_value *matched;
	struct dns_allow_key *question;
	struct dns_query_state *state;
	struct mvm_meta *mvm_meta;
	struct dns_question_footer question_footer;
	__u64 qname_hash = 0;
	__u32 key = 0;
	__u32 ifindex;
	__u32 question_cursor;
	void *inner_map;

	state = bpf_map_lookup_elem(&dns_query_state, &key);
	question = bpf_map_lookup_elem(&dns_query_scratch, &key);
	if (!state || !question)
		return TC_ACT_SHOT;
	ifindex = state->ifindex;

	mvm_meta = bpf_map_lookup_elem(&ifindex_to_mvmmeta, &ifindex);
	if (!mvm_meta)
		return TC_ACT_SHOT;
	if (!dns_policy_enabled(mvm_meta))
		return finish_udp_nat(skb, mvm_meta);

	inner_map = bpf_map_lookup_elem(&dns_allow, &ifindex);
	if (!inner_map)
		return finish_udp_nat(skb, mvm_meta);

	question_cursor = state->dns_off + DNS_HDR_LEN;
	if (state->failed)
		return finish_udp_nat(skb, mvm_meta);
	if (!dns_hash_qname(skb, &question_cursor, &question_footer,
					&qname_hash))
		return finish_udp_nat(skb, mvm_meta);

	matched = dns_allow_match_value(inner_map, question);
	if (!matched)
		return finish_udp_nat(skb, mvm_meta);

	dns_track_allowed_query(skb, state, matched->flags, qname_hash);
	return finish_udp_nat(skb, mvm_meta);
}

/* This filter will be attached to the ingress path of Sandbox TAP devices.
 * It performs a SNAT/VXLAN-ENCAP and redirects the packets to target devices.
 */
SEC("tc")
int from_cube(struct __sk_buff *skb)
{
	__u32 daddr, ifindex, dst_ifindex;
	__u64 tcp_ret;
	struct bpf_sock_tuple tuple = {};
	struct mvm_port mvm_port = {};
	struct mvm_meta *mvm_meta;
	struct bpf_sock *sk;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct tcphdr *l4;
	struct udphdr *udp;
	__u16 *host_port;
	__u32 dns_off;
	__u8 proto;
	long err;
	int ret;

	skb->queue_mapping = 0;

	/* We handle ETH_P_IP/ETH_P_ARP protocols ONLY */
	if (skb->protocol != bpf_htons(ETH_P_IP)) {
		/* Handle ARP requests with ARP proxy */
		if (skb->protocol == bpf_htons(ETH_P_ARP))
			return handle_arp(skb, skb->ingress_ifindex);
		return TC_ACT_SHOT;
	}

	ifindex = skb->ingress_ifindex;
	mvm_meta = bpf_map_lookup_elem(&ifindex_to_mvmmeta, &ifindex);
	if (!mvm_meta)
		return TC_ACT_SHOT;

	ret = pull_headers(skb, &l2, &l3);
	if (ret != TC_ACT_OK)
		return ret;

	daddr = l3->daddr;
	proto = l3->protocol;

	err = snat(skb, l3, mvm_meta->ip);
	if (err)
		return TC_ACT_SHOT;

	if (daddr == mvm_gateway_ip) {
		/* Filter traffic to cubegw0:
		 * allow ICMP, allow TCP non-SYN, drop everything else.
		 */
		switch (proto) {
		case IPPROTO_ICMP:
			break;
		case IPPROTO_TCP:
			if (!__pull_headers(skb, &l2, &l3, &l4))
				return TC_ACT_SHOT;
			if (l4->syn && !l4->ack)
				return TC_ACT_SHOT;
			break;
		default:
			return TC_ACT_SHOT;
		}

		ret = pull_headers(skb, &l2, &l3);
		if (ret != TC_ACT_OK)
			return ret;

		err = dnat(skb, l3, cubegw0_ip);
		if (err)
			return TC_ACT_SHOT;

		return bpf_redirect(cubegw0_ifindex, BPF_F_INGRESS);
	}

	if (proto == IPPROTO_TCP) {
		if (!__pull_headers(skb, &l2, &l3, &l4))
			return TC_ACT_SHOT;

		mvm_port.ifindex = ifindex;
		mvm_port.listen_port = l4->source;
		host_port = bpf_map_lookup_elem(&local_port_mapping, &mvm_port);
		if (host_port) {
			if (l4->syn && !l4->ack)
				return TC_ACT_SHOT;

			err = snat_tcp(skb, ifindex, l2, l3, l4, l4->source, *host_port);
			if (err)
				return TC_ACT_SHOT;

			return bpf_redirect(nodenic_ifindex, 0);
		}
	}

	if (proto == IPPROTO_TCP &&
	    __pull_headers(skb, &l2, &l3, &l4) &&
	    (l4->dest == bpf_htons(80) || l4->dest == bpf_htons(443))) {
		tuple.ipv4.saddr = mvm_meta->ip;
		tuple.ipv4.daddr = daddr;
		tuple.ipv4.sport = l4->source;
		tuple.ipv4.dport = l4->dest;
		sk = bpf_skc_lookup_tcp(skb, &tuple, sizeof(tuple.ipv4), BPF_F_CURRENT_NETNS, 0);
		if (sk) {
			__u32 state = sk->state;

			bpf_sk_release(sk);
			if (state == BPF_TCP_ESTABLISHED)
				return bpf_redirect(cubegw0_ifindex, BPF_F_INGRESS);
		}
	}

	ret = pull_headers(skb, &l2, &l3);
	if (ret != TC_ACT_OK)
		return ret;

	if (!should_do_nat(l3))
		return TC_ACT_SHOT;

	if (l3->daddr == nodenic_ip) {
		/* This branch bypasses do_*_nat() and therefore the policy
		 * check inside create_nat_session(). Enforce policy inline.
		 * TCP callers get an RST to match the guest-visible behavior
		 * of the do_tcp_nat() path; UDP/ICMP silently drop.
		 */
		if (!session_policy_allowed(ifindex, daddr)) {
			if (proto == IPPROTO_TCP)
				return tcp_reply_reset(skb, ifindex);
			return TC_ACT_SHOT;
		}
		return bpf_redirect(cubegw0_ifindex, BPF_F_INGRESS);
	}

	if (proto == IPPROTO_TCP) {
		if (!__pull_headers(skb, &l2, &l3, &l4))
			return TC_ACT_SHOT;
		if (should_redirect_to_l7_proxy(ifindex, daddr, l4))
			return bpf_redirect(cubegw0_ifindex, BPF_F_INGRESS);
		tcp_ret = do_tcp_nat(skb, mvm_meta);
		if (TCP_NAT_STATUS(tcp_ret) == TCP_NAT_OK)
			return bpf_redirect(TCP_NAT_IFINDEX(tcp_ret), egress_redirect_flags);
		if (TCP_NAT_STATUS(tcp_ret) == TCP_NAT_RESET)
			return tcp_reply_reset(skb, ifindex);
	}

	if (proto == IPPROTO_UDP) {
		if (!__pull_headers_udp(skb, &l2, &l3, &udp))
			return TC_ACT_SHOT;

		if (udp->dest == DNS_PORT && dns_policy_enabled(mvm_meta) &&
		    dns_payload_offset(l3, udp, &dns_off)) {
			ret = dns_handle_query(skb, dns_off, ifindex);
			if (ret != CUBE_DNS_PASS)
				return ret;
		}

		return finish_udp_nat_inline(skb, mvm_meta);
	}

	if (proto == IPPROTO_ICMP) {
		dst_ifindex = do_icmp_nat(skb, mvm_meta);
		if (dst_ifindex)
			return bpf_redirect(dst_ifindex, egress_redirect_flags);
	}

	return TC_ACT_SHOT;
}

char __license[] SEC("license") = "Dual BSD/GPL";
