// SPDX-License-Identifier: GPL-2.0
/* Copyright (c) 2022 Cube Authors */
#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "cubevs.h"
#include "icmp.h"
#include "jhash.h"
#include "l2l3.h"
#include "map.h"
#include "skb.h"
#include "tcp.h"
#include "udp.h"
#include "dns_query.h"
#include "dns_response.h"

static int tcp_nat_proxy(struct __sk_buff *skb, struct ethhdr *l2, struct iphdr *l3, struct tcphdr *l4,
			 struct mvm_port *mvm_port)
{
	__u32 old_daddr, new_daddr, tcp_csum_off;
	__u16 old_dport, new_dport;
	__u16 ip_hlen;
	__u64 flags;
	long err;

	old_daddr = l3->daddr;
	new_daddr = mvm_inner_ip;
	old_dport = l4->dest;
	new_dport = mvm_port->listen_port;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	tcp_csum_off = TCP_CSUM_OFF(ip_hlen);

	/* update L2 first: csum/store helpers may invalidate packet pointers */
	set_mac_pair(l2, cubegw0_macaddr_p1, cubegw0_macaddr_p2,
		     mvm_macaddr_p1, mvm_macaddr_p2);

	/* update TCP csum: IP daddr is part of pseudo-header, so BPF_F_PSEUDO_HDR */
	flags = BPF_F_PSEUDO_HDR | sizeof(old_daddr);
	err = bpf_l4_csum_replace(skb, tcp_csum_off, old_daddr, new_daddr, flags);
	if (err)
		return TC_ACT_OK;

	/* update TCP csum for port change (not part of pseudo-header) */
	flags = sizeof(old_dport);
	err = bpf_l4_csum_replace(skb, tcp_csum_off, old_dport, new_dport, flags);
	if (err)
		return TC_ACT_OK;

	/* write new TCP destination port */
	err = bpf_skb_store_bytes(skb, TCP_DST_OFF(ip_hlen), &new_dport, sizeof(new_dport), 0);
	if (err)
		return TC_ACT_OK;

	/* update IP csum and write new daddr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_daddr, new_daddr, sizeof(old_daddr));
	if (err)
		return TC_ACT_OK;

	err = bpf_skb_store_bytes(skb, IP_DADDR_OFF, &new_daddr, sizeof(new_daddr), 0);
	if (err)
		return TC_ACT_OK;

	return bpf_redirect(mvm_port->ifindex, 0);
}

static __always_inline struct nat_session *lookup_session(const struct session_key *ikey)
{
	struct ingress_session *isess;
	struct session_key key = {};

	isess = bpf_map_lookup_elem(&ingress_sessions, ikey);
	if (!isess)
		return NULL;

	key.src_ip = isess->vm_ip;
	key.dst_ip = ikey->src_ip;
	key.src_port = isess->vm_port;
	key.dst_port = ikey->src_port;
	key.version = isess->version;
	key.protocol = ikey->protocol;
	return bpf_map_lookup_elem(&egress_sessions, &key);
}

static int tcp_nat_session(struct __sk_buff *skb, struct ethhdr *l2, struct iphdr *l3, struct tcphdr *l4)
{
	__u32 old_daddr, new_daddr, tcp_csum_off;
	__u16 old_dport, new_dport;
	struct session_key key = {};
	struct nat_session *sess;
	bool syn, ack, fin, rst;
	__u16 ip_hlen;
	__u64 flags;
	__u64 now;
	long err;

	key.src_ip = l3->saddr;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = 0;
	key.protocol = l3->protocol;
	sess = lookup_session(&key);
	if (!sess)
		return TC_ACT_OK;

	/* update session */
	now = bpf_ktime_get_ns();
	syn = l4->syn;
	ack = l4->ack;
	fin = l4->fin;
	rst = l4->rst;
	update_session(IP_CT_DIR_REPLY, sess, now, syn, ack, fin, rst);

	old_daddr = l3->daddr;
	new_daddr = mvm_inner_ip;
	old_dport = l4->dest;
	new_dport = sess->vm_port;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	tcp_csum_off = TCP_CSUM_OFF(ip_hlen);

	/* update L2 first: csum/store helpers may invalidate packet pointers */
	set_mac_pair(l2, cubegw0_macaddr_p1, cubegw0_macaddr_p2,
		     mvm_macaddr_p1, mvm_macaddr_p2);

	/* update TCP csum: IP daddr is part of pseudo-header, so BPF_F_PSEUDO_HDR */
	flags = BPF_F_PSEUDO_HDR | sizeof(old_daddr);
	err = bpf_l4_csum_replace(skb, tcp_csum_off, old_daddr, new_daddr, flags);
	if (err)
		return TC_ACT_OK;

	/* update TCP csum for port change (not part of pseudo-header) */
	flags = sizeof(old_dport);
	err = bpf_l4_csum_replace(skb, tcp_csum_off, old_dport, new_dport, flags);
	if (err)
		return TC_ACT_OK;

	/* write new TCP destination port */
	err = bpf_skb_store_bytes(skb, TCP_DST_OFF(ip_hlen), &new_dport, sizeof(new_dport), 0);
	if (err)
		return TC_ACT_OK;

	/* update IP csum and write new daddr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_daddr, new_daddr, sizeof(old_daddr));
	if (err)
		return TC_ACT_OK;

	err = bpf_skb_store_bytes(skb, IP_DADDR_OFF, &new_daddr, sizeof(new_daddr), 0);
	if (err)
		return TC_ACT_OK;

	return bpf_redirect(sess->vm_ifindex, 0);
}

/* Rewrite an ingress UDP packet into the sandbox's reverse-NAT form.
 *
 * Marked __always_inline because both the from_world UDP path and the DNS
 * response tail-called program share this exact rewrite. The non-inline
 * variant would create deep verifier paths in two SEC programs at once.
 */
static __always_inline int udp_nat_rewrite(struct __sk_buff *skb,
					   struct ethhdr *l2,
					   struct iphdr *l3,
					   struct udphdr *l4,
					   const struct nat_session *sess)
{
	__u32 old_daddr, new_daddr, udp_csum_off;
	__u16 old_dport, new_dport, old_csum;
	__u16 ip_hlen;
	__u64 flags;
	long err;

	old_daddr = l3->daddr;
	new_daddr = mvm_inner_ip;
	old_dport = l4->dest;
	new_dport = sess->vm_port;
	old_csum = l4->check;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	udp_csum_off = UDP_CSUM_OFF(ip_hlen);

	/* update L2 first: csum/store helpers may invalidate packet pointers */
	set_mac_pair(l2, cubegw0_macaddr_p1, cubegw0_macaddr_p2,
		     mvm_macaddr_p1, mvm_macaddr_p2);

	/* update UDP csum only if it was non-zero (UDP csum is optional over IPv4).
	 * BPF_F_MARK_MANGLED_0 keeps a 0 csum (= disabled) intact in case the
	 * incremental update would yield 0; the helper rewrites it to 0xffff.
	 * IP daddr is part of UDP pseudo-header, so BPF_F_PSEUDO_HDR is required.
	 */
	if (old_csum) {
		flags = BPF_F_PSEUDO_HDR | BPF_F_MARK_MANGLED_0 | sizeof(old_daddr);
		err = bpf_l4_csum_replace(skb, udp_csum_off, old_daddr, new_daddr, flags);
		if (err)
			return TC_ACT_OK;

		/* port is not part of pseudo-header */
		flags = BPF_F_MARK_MANGLED_0 | sizeof(old_dport);
		err = bpf_l4_csum_replace(skb, udp_csum_off, old_dport, new_dport, flags);
		if (err)
			return TC_ACT_OK;
	}

	/* write new UDP destination port */
	err = bpf_skb_store_bytes(skb, UDP_DST_OFF(ip_hlen), &new_dport, sizeof(new_dport), 0);
	if (err)
		return TC_ACT_OK;

	/* update IP csum and write new daddr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_daddr, new_daddr, sizeof(old_daddr));
	if (err)
		return TC_ACT_OK;

	err = bpf_skb_store_bytes(skb, IP_DADDR_OFF, &new_daddr, sizeof(new_daddr), 0);
	if (err)
		return TC_ACT_OK;

	return bpf_redirect(sess->vm_ifindex, 0);
}

static int udp_nat_session(struct __sk_buff *skb, struct ethhdr *l2, struct iphdr *l3, struct udphdr *l4)
{
	struct dns_response_state *rstate;
	struct session_key key = {};
	struct nat_session *sess;
	__u32 scratch_key = 0;
	__u32 dns_off;
	__u64 now;

	key.src_ip = l3->saddr;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = 0;
	key.protocol = IPPROTO_UDP;
	sess = lookup_session(&key);
	if (!sess)
		return TC_ACT_OK;

	/* DNS replies need IP-learning before reverse NAT. The handler walks
	 * up to DNS_MAX_RESPONSE_ANSWERS RRs and inlining it here pushes the
	 * from_world verifier graph past the 1M insn budget, so we hand the
	 * packet off to a tail-called program that finishes UDP NAT itself.
	 *
	 * bpf_tail_call invalidates packet pointers, so we MUST NOT touch
	 * l2/l3/l4 after attempting the tail call. If the tail call succeeds
	 * we never return; if it fails (slot unpopulated) we drop the packet
	 * — the sandbox will retry. Doing reverse NAT here would require
	 * re-pulling and re-looking up after the tail call, which the
	 * verifier on kernel 5.4 cannot prove safe within the 1M insn budget.
	 */
	if (l4->source == DNS_PORT && dns_payload_offset(l3, l4, &dns_off)) {
		rstate = bpf_map_lookup_elem(&dns_response_state, &scratch_key);
		if (rstate) {
			rstate->dns_off = dns_off;
			rstate->ifindex = sess->vm_ifindex;
			rstate->server_ip = l3->saddr;
			rstate->source_port = sess->vm_port;
			rstate->reserved = 0;
			bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_RESPONSE);
			/* Tail call failed (slot unpopulated): drop the packet.
			 * Packet pointers are considered invalidated by the
			 * verifier after bpf_tail_call, so we cannot continue
			 * the reverse-NAT path here.
			 */
			return TC_ACT_OK;
		}
	}

	/* update session */
	now = bpf_ktime_get_ns();
	update_udp_session(IP_CT_DIR_REPLY, sess, now);

	return udp_nat_rewrite(skb, l2, l3, l4, sess);
}

static int icmp_nat_session(struct __sk_buff *skb, struct ethhdr *l2, struct iphdr *l3, struct icmphdr *l4)
{
	__u32 old_daddr, new_daddr, icmp_csum_off;
	__u16 old_id, new_id;
	struct session_key key = {};
	struct nat_session *sess;
	__u16 ip_hlen;
	__u64 flags;
	__u64 now;
	long err;

	/* Only handle Echo Reply inbound */
	if (l4->type != ICMP_ECHOREPLY)
		return TC_ACT_OK;

	/* ingress key: src=remote, dst=node_ip, src_port=0, dst_port=identifier */
	key.src_ip = l3->saddr;
	key.dst_ip = l3->daddr;
	key.src_port = 0;
	key.dst_port = l4->un.echo.id; /* the SNAT identifier we assigned */
	key.version = 0;
	key.protocol = IPPROTO_ICMP;
	sess = lookup_session(&key);
	if (!sess)
		return TC_ACT_OK;

	/* update session */
	now = bpf_ktime_get_ns();
	update_icmp_session(IP_CT_DIR_REPLY, sess, now);

	old_daddr = l3->daddr;
	new_daddr = mvm_inner_ip;
	old_id = l4->un.echo.id;
	new_id = sess->vm_port;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	icmp_csum_off = ICMP_CSUM_OFF(ip_hlen);

	/* update L2 first: csum/store helpers may invalidate packet pointers */
	set_mac_pair(l2, cubegw0_macaddr_p1, cubegw0_macaddr_p2,
		     mvm_macaddr_p1, mvm_macaddr_p2);

	/* update ICMP csum: ICMP has no pseudo-header, so no BPF_F_PSEUDO_HDR.
	 * Only the echo identifier change affects the csum (IP daddr is not
	 * covered by ICMP checksum).
	 */
	flags = sizeof(old_id);
	err = bpf_l4_csum_replace(skb, icmp_csum_off, old_id, new_id, flags);
	if (err)
		return TC_ACT_OK;

	/* write the restored ICMP echo identifier */
	err = bpf_skb_store_bytes(skb, ICMP_ECHO_ID_OFF(ip_hlen), &new_id, sizeof(new_id), 0);
	if (err)
		return TC_ACT_OK;

	/* update IP csum and write new daddr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, old_daddr, new_daddr, sizeof(old_daddr));
	if (err)
		return TC_ACT_OK;

	err = bpf_skb_store_bytes(skb, IP_DADDR_OFF, &new_daddr, sizeof(new_daddr), 0);
	if (err)
		return TC_ACT_OK;

	return bpf_redirect(sess->vm_ifindex, 0);
}

static int do_icmp_nat(struct __sk_buff *skb)
{
	struct ethhdr *l2;
	struct iphdr *l3;
	struct icmphdr *l4;

	if (!__pull_headers_icmp(skb, &l2, &l3, &l4))
		return TC_ACT_OK;

	return icmp_nat_session(skb, l2, l3, l4);
}

static int do_udp_nat(struct __sk_buff *skb)
{
	struct ethhdr *l2;
	struct iphdr *l3;
	struct udphdr *l4;

	if (!__pull_headers_udp(skb, &l2, &l3, &l4))
		return TC_ACT_OK;

	return udp_nat_session(skb, l2, l3, l4);
}

static int do_tcp_nat(struct __sk_buff *skb)
{
	struct mvm_port *mvm_port;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct tcphdr *l4;
	__u16 dport;

	if (!__pull_headers(skb, &l2, &l3, &l4))
		return TC_ACT_OK;

	/* Port mapping is exposed only on the configured primary NIC. The same
	 * from_world program also runs on cube-router egress, where port mapping
	 * must be skipped and only reverse NAT sessions are relevant.
	 */
	if (skb->ifindex == nodenic_ifindex) {
		dport = l4->dest;
		mvm_port = bpf_map_lookup_elem(&remote_port_mapping, &dport);
		if (mvm_port)
			return tcp_nat_proxy(skb, l2, l3, l4, mvm_port);
	}

	return tcp_nat_session(skb, l2, l3, l4);
}

/* This filter is shared by the primary NIC ingress and cube-router egress.
 * It performs reverse NAT and redirects matching traffic to Sandbox TAPs.
 */
SEC("tc")
int from_world(struct __sk_buff *skb)
{
	struct ethhdr *l2;
	struct iphdr *l3;
	int ret;

	if (skb->protocol != bpf_htons(ETH_P_IP))
		return TC_ACT_OK;

	ret = pull_headers(skb, &l2, &l3);
	if (ret != TC_ACT_OK)
		return TC_ACT_OK;

	if (l3->protocol == IPPROTO_TCP)
		return do_tcp_nat(skb);

	if (l3->protocol == IPPROTO_UDP)
		return do_udp_nat(skb);

	if (l3->protocol == IPPROTO_ICMP)
		return do_icmp_nat(skb);

	return TC_ACT_OK;
}

/* Tail-called from udp_nat_session when an ingress UDP packet is a DNS reply.
 *
 * Owns just the heavy DNS answer-RR walk. All DNS helpers it calls are
 * __always_inline so this program contains no bpf-to-bpf calls, which is a
 * hard requirement on kernel 5.4: programs that mix bpf-to-bpf calls with
 * tail calls are rejected. After learning A records we tail-call the UDP
 * NAT finish program; splitting the work this way keeps each program's
 * verifier graph well under the 1M instruction budget.
 */
SEC("tc")
int dns_handle_response_prog(struct __sk_buff *skb)
{
	struct dns_response_state *rstate;
	__u32 scratch_key = 0;

	rstate = bpf_map_lookup_elem(&dns_response_state, &scratch_key);
	if (!rstate)
		return TC_ACT_OK;

	/* Learn A records into allow_out_v2 before reverse NAT. */
	dns_handle_response(skb, rstate->dns_off, rstate->ifindex,
			    rstate->server_ip, rstate->source_port);

	bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_RESPONSE_FINISH);
	/* Tail call failed (slot unpopulated): drop, the sandbox will retry. */
	return TC_ACT_OK;
}

/* Tail-called from dns_handle_response_prog to finish ingress UDP NAT.
 *
 * Pointers and map element references from the previous tail call did not
 * survive, so we re-pull headers and re-look-up the session here.
 */
SEC("tc")
int dns_response_finish_prog(struct __sk_buff *skb)
{
	struct session_key key = {};
	struct nat_session *sess;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct udphdr *l4;
	__u64 now;

	if (!__pull_headers_udp(skb, &l2, &l3, &l4))
		return TC_ACT_OK;

	key.src_ip = l3->saddr;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = 0;
	key.protocol = IPPROTO_UDP;
	sess = lookup_session(&key);
	if (!sess)
		return TC_ACT_OK;

	now = bpf_ktime_get_ns();
	update_udp_session(IP_CT_DIR_REPLY, sess, now);

	return udp_nat_rewrite(skb, l2, l3, l4, sess);
}

char __license[] SEC("license") = "Dual BSD/GPL";
