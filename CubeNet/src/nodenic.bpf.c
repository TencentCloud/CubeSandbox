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

static int udp_nat_session(struct __sk_buff *skb, struct ethhdr *l2, struct iphdr *l3, struct udphdr *l4)
{
	__u32 old_daddr, new_daddr, udp_csum_off;
	__u16 old_dport, new_dport, old_csum;
	struct session_key key = {};
	struct nat_session *sess;
	__u16 ip_hlen;
	__u64 flags;
	__u64 now;
	long err;

	key.src_ip = l3->saddr;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = 0;
	key.protocol = IPPROTO_UDP;
	sess = lookup_session(&key);
	if (!sess)
		return TC_ACT_OK;

	/* update session */
	now = bpf_ktime_get_ns();
	update_udp_session(IP_CT_DIR_REPLY, sess, now);

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

	dport = l4->dest;
	mvm_port = bpf_map_lookup_elem(&remote_port_mapping, &dport);
	if (mvm_port)
		return tcp_nat_proxy(skb, l2, l3, l4, mvm_port);

	return tcp_nat_session(skb, l2, l3, l4);
}

/* This filter will be attached to the ingress path of host NIC.
 * It performs NAT and then redirect the traffics to Sandbox TAP devices.
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

char __license[] SEC("license") = "Dual BSD/GPL";
