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
#include "tcp_reset.h"
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

/* Primary IPv4 prefix of the node NIC. Direct mode only. */
static __always_inline bool direct_egress_is_onlink(__u32 daddr)
{
	return egress_redirect_flags == 0 &&
	       (daddr & nodenic_netmask) == (nodenic_ip & nodenic_netmask);
}

static __always_inline bool direct_neighbor_is_zero(const struct direct_neighbor *neighbor)
{
	const union macaddr *macaddr = (const union macaddr *)neighbor->addr;

	return macaddr->p1 == 0 && macaddr->p2 == 0;
}

/* Resolve the external L2 addresses for an on-link direct-egress destination.
 *
 * The kernel neighbor table is the only source of truth for the MAC:
 *   - a fresh positive entry in direct_neigh is used as-is (no fib lookup);
 *   - a fresh negative entry (recent fib failure) skips fib and falls back;
 *   - otherwise bpf_fib_lookup() refreshes the cache, and on failure the cache
 *     is invalidated and the gateway MAC is used as a fallback.
 *
 * There is no packet drop or packet-into-ARP conversion: an unresolved on-link
 * destination is forwarded to the gateway (zero loss on hairpin networks; a
 * bounded blackhole elsewhere until the scanner's trigger makes the kernel ARP
 * resolve and the next fib refresh caches the MAC). It only rewrites the skb's
 * L2 addresses; the packet is always forwarded.
 */
static __always_inline void prepare_egress_l2(struct __sk_buff *skb,
					      struct ethhdr *l2, __u32 daddr)
{
	struct bpf_fib_lookup fib = {
		.family = AF_INET,
		.ifindex = nodenic_ifindex,
		.ipv4_src = nodenic_ip,
		.ipv4_dst = daddr,
	};
	struct direct_neighbor pending = {};
	struct direct_neighbor *neighbor;
	union macaddr *mac;
	__u64 now;
	long err;

	if (!direct_egress_is_onlink(daddr)) {
		set_mac_pair(l2, egress_smacaddr_p1, egress_smacaddr_p2,
			     egress_dmacaddr_p1, egress_dmacaddr_p2);
		return;
	}

	now = bpf_ktime_get_ns();
	neighbor = bpf_map_lookup_elem(&direct_neigh, &daddr);
	if (!neighbor) {
		/* Untracked destination: only register for the scanner, never
		 * disturb its scheduling fields. Seed last_used_ns so the
		 * scanner's GC (idle > GC_AFTER) never reclaims a freshly
		 * created entry in the window before the end-of-function
		 * touch, while the datapath still holds its pointer. */
		pending.last_used_ns = now;
		bpf_map_update_elem(&direct_neigh, &daddr, &pending, BPF_NOEXIST);
		neighbor = bpf_map_lookup_elem(&direct_neigh, &daddr);
	}
	if (!neighbor) {
		/* Map full or vanished: untracked, pure gateway fallback. */
		set_mac_pair(l2, egress_smacaddr_p1, egress_smacaddr_p2,
			     egress_dmacaddr_p1, egress_dmacaddr_p2);
		return;
	}

	if (now < neighbor->valid_until_ns) {
		if (!direct_neighbor_is_zero(neighbor)) {
			/* Positive cache hit: use the cached MAC, no fib. */
			mac = (union macaddr *)neighbor->addr;
			set_mac_pair(l2, nodenic_macaddr_p1, nodenic_macaddr_p2,
				     mac->p1, mac->p2);
		} else {
			/* Negative cache hit (recent fib failure): skip fib. */
			set_mac_pair(l2, egress_smacaddr_p1, egress_smacaddr_p2,
				     egress_dmacaddr_p1, egress_dmacaddr_p2);
		}
	} else {
		err = bpf_fib_lookup(skb, &fib, sizeof(fib),
				     BPF_FIB_LOOKUP_DIRECT | BPF_FIB_LOOKUP_OUTPUT);
		if (err == BPF_FIB_LKUP_RET_SUCCESS &&
		    fib.ifindex == nodenic_ifindex) {
			const union macaddr *dmac = (const union macaddr *)fib.dmac;
			const union macaddr *smac = (const union macaddr *)fib.smac;

			/* Cache the MAC; valid_until is written ONLY on a fib
			 * result, never renewed on a cache hit. */
			mac = (union macaddr *)neighbor->addr;
			mac->p1 = dmac->p1;
			mac->p2 = dmac->p2;
			neighbor->valid_until_ns = now + DIRECT_NEIGH_CACHE_TTL_NS;
			neighbor->fib_ok = 1;
			set_mac_pair(l2, smac->p1, smac->p2, dmac->p1, dmac->p2);
		} else {
			/* Invalidate the cache and fall back to the gateway. A
			 * short negative TTL keeps a dead destination from
			 * triggering a fib lookup on every packet. */
			mac = (union macaddr *)neighbor->addr;
			mac->p1 = 0;
			mac->p2 = 0;
			neighbor->valid_until_ns = now + DIRECT_NEIGH_NEG_TTL_NS;
			neighbor->fib_ok = 0;
			set_mac_pair(l2, egress_smacaddr_p1, egress_smacaddr_p2,
				     egress_dmacaddr_p1, egress_dmacaddr_p2);
		}
	}

	/* Throttle last_used writes to 1s granularity; never renew valid_until. */
	if (now > neighbor->last_used_ns + DIRECT_NEIGH_TOUCH_THROTTLE_NS)
		neighbor->last_used_ns = now;
}

/* Egress flow classification now lives in classify_egress_flow() (session.h),
 * which merges the former l7_scheme_for_flow() and session_policy_allowed()
 * into a single policy verdict (reject / accept-SNAT / accept-HTTP /
 * accept-HTTPS). It is applied once, when a new flow is created, and the
 * result is cached in nat_session for reuse on every later packet.
 */
enum tcp_nat_result {
	TCP_NAT_DROP = 0,
	TCP_NAT_OK,
	TCP_NAT_RESET,
	TCP_L7PROXY_OK,
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

/* Reserve the reverse-flow key for an L7 proxy session. L7 traffic is not
 * source-NATed, so the reply tuple is the exact reverse of the sandbox's
 * original tuple. create_nat_session() later inserts the matching egress value
 * and rolls this reservation back if that insertion fails.
 */
static __always_inline bool create_l7_ingress_session(const struct session_key *ekey)
{
	struct ingress_session isess = {
		.version = ekey->version,
		.vm_ip = ekey->src_ip,
		.vm_port = ekey->src_port,
	};
	struct session_key ikey = {
		.src_ip = ekey->dst_ip,
		.dst_ip = ekey->src_ip,
		.src_port = ekey->dst_port,
		.dst_port = ekey->src_port,
		.version = 0,
		.protocol = ekey->protocol,
	};

	return bpf_map_update_elem(&ingress_sessions, &ikey, &isess,
				   BPF_NOEXIST) == 0;
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
	__u32 policy_version;
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
	/* Read the generation once, before any classification: userspace can bump
	 * it mid-packet, and judging under one generation while stamping another
	 * would retire the re-check that the newer generation is owed.
	 */
	policy_version = mvm_meta->policy_version;
	/* Use ICMP identifier as the "port" identifier in the session key */
	key.src_ip = mvm_meta->ip;
	key.dst_ip = l3->daddr;
	key.src_port = l4->un.echo.id; /* identifier (network byte order) */
	key.dst_port = 0;
	key.version = mvm_meta->version;
	key.protocol = IPPROTO_ICMP;

	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (sess) {
		/* revoked by a policy update: retire the pair and drop, since
		 * there is nothing to reset on ICMP
		 */
		if (session_policy_revoked(sess, policy_version, skb->ingress_ifindex,
					   key.dst_ip, key.dst_port)) {
			del_session(&key, sess);
			return 0;
		}
		update_icmp_session(IP_CT_DIR_ORIGINAL, sess, now);
		goto do_nat;
	}

	/* create new session */
	if (classify_egress_flow(skb->ingress_ifindex, key.dst_ip,
				 key.dst_port) == FLOW_REJECT)
		return 0;
	snat_ip = pick_snat_ip_port(mvm_meta->ip, &key, &snat_id);
	if (!snat_ip || !snat_ip->ip || !snat_id)
		return 0;
	ok = create_icmp_sessions(skb, &key, now, skb->ingress_ifindex, snat_ip, snat_id,
				  policy_version);
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
	__u32 policy_version;
	__u16 ip_hlen;
	__u16 snat_port;
	__u64 flags;
	__u64 now;
	long err;
	bool ok;

	if (!__pull_headers_udp(skb, &l2, &l3, &l4))
		return 0;

	now = bpf_ktime_get_ns();
	/* See do_icmp_nat(): one read per packet, taken before any classification. */
	policy_version = mvm_meta->policy_version;
	key.src_ip = mvm_meta->ip;
	key.dst_ip = l3->daddr;
	key.src_port = l4->source;
	key.dst_port = l4->dest;
	key.version = mvm_meta->version;
	key.protocol = IPPROTO_UDP;

	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (sess) {
		/* revoked by a policy update: retire the pair and drop, since
		 * there is nothing to reset on UDP
		 */
		if (session_policy_revoked(sess, policy_version, skb->ingress_ifindex,
					   key.dst_ip, key.dst_port)) {
			del_session(&key, sess);
			return 0;
		}
		update_udp_session(IP_CT_DIR_ORIGINAL, sess, now);
		goto do_nat;
	}

	/* create new session */
	if (classify_egress_flow(skb->ingress_ifindex, key.dst_ip,
				 key.dst_port) == FLOW_REJECT)
		return 0;
	snat_ip = pick_snat_ip_port(mvm_meta->ip, &key, &snat_port);
	if (!snat_ip || !snat_ip->ip || !snat_port)
		return 0;
	ok = create_udp_sessions(skb, &key, now, skb->ingress_ifindex, snat_ip, snat_port,
				 policy_version);
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
	struct snat_ip l7_endpoint = {};
	bool syn, ack, fin, rst;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct tcphdr *l4;
	__u32 policy_version;
	__u16 ip_hlen;
	__u16 snat_port;
	__u64 flags;
	__u64 now;
	__u32 l7_mark;
	__u8 packet_class = SNAT_PACKET;
	__u8 l7_scheme = L7_SCHEME_NONE;
	__u8 verdict = FLOW_SNAT;
	bool create_snat = false;
	long err;
	bool ok;

	if (!__pull_headers(skb, &l2, &l3, &l4))
		return TCP_NAT_DROP;

	now = bpf_ktime_get_ns();
	/* See do_icmp_nat(): one read per packet, taken before any classification. */
	policy_version = mvm_meta->policy_version;
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
				/* L7 sessions use an identity reverse tuple, so immediately
				 * replacing a terminal session would let delayed packets from
				 * the old connection mutate the new session. Keep the old pair
				 * until the userspace reaper removes it and reject premature
				 * tuple reuse. Ordinary SNAT sessions remain safe to recreate
				 * because they allocate a fresh reverse-side source port.
				 */
				if (sess->packet_class == L7PROXY_PACKET)
					return TCP_NAT_PACK(0, TCP_NAT_RESET);

				/* guest kernel reuse source port too fast */
				del_session(&key, sess);
				goto do_create;
			}

			goto do_update;
		}
do_create:
	/* Classify the flow with the unified egress policy. The verdict is
	 * cached in nat_session and reused for every later packet.
	 */
	verdict = classify_egress_flow(skb->ingress_ifindex, key.dst_ip,
				       key.dst_port);
	switch (verdict) {
	case FLOW_HTTP:
	case FLOW_HTTPS:
		if (!create_l7_ingress_session(&key))
			return TCP_NAT_DROP;
		l7_endpoint.ifindex = cubegw0_ifindex;
		l7_endpoint.ip = key.src_ip;
		snat_ip = &l7_endpoint;
		snat_port = key.src_port;
		packet_class = L7PROXY_PACKET;
		l7_scheme = (verdict == FLOW_HTTP) ? L7_SCHEME_HTTP :
						     L7_SCHEME_HTTPS;
		break;
	case FLOW_REJECT:
		/* Denied by egress policy: signal the caller so it can emit an
		 * RST, exactly as the old create_nat_session path did.
		 */
		nat_cb_set(skb, NAT_CB_DENIED_BY_POLICY);
		return TCP_NAT_PACK(0, TCP_NAT_RESET);
	case FLOW_SNAT:
	default:
		create_snat = true;
		goto prepare_snat;
	}
create_session:
	ok = create_new_sessions(skb, &key, now, skb->ingress_ifindex,
				 snat_ip, snat_port, packet_class, l7_scheme,
				 policy_version);
	if (!ok)
		return TCP_NAT_DROP;
	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (!sess)
		return TCP_NAT_DROP;
	goto do_nat;
	} else {
		/* lookup existing session */
		sess = bpf_map_lookup_elem(&egress_sessions, &key);
		if (!sess) {
			/* No session: the flow was never authorized, or it was retired
			 * (reaped, or revoked by a policy update). Either way there is no
			 * record that this connection is allowed, so answer like every
			 * other unreachable TCP packet here instead of trusting that a
			 * live proxy socket implies a past authorization.
			 */
			return rst ? TCP_NAT_DROP : TCP_NAT_RESET;
		}
	}

do_update:
	/* A policy update revoked this flow: retire the session pair so the tuple is
	 * free for an immediate reconnect, and answer with an RST like every other
	 * unreachable TCP packet here, so the guest learns now instead of stalling
	 * until its retransmit timer gives up. Never RST an RST, or two peers that
	 * both consider the flow dead would trade resets forever.
	 *
	 * Taken before the L7 branch below so proxied flows are revocable too, and
	 * before prepare_egress_l2() because resolving L2 for a flow that is about to
	 * be retired is wasted work -- and a cold ARP miss would consume this packet
	 * as a probe, deferring the revocation by one more packet.
	 */
	if (session_policy_revoked(sess, policy_version, skb->ingress_ifindex,
				   key.dst_ip, key.dst_port)) {
		del_session(&key, sess);
		return rst ? TCP_NAT_DROP : TCP_NAT_RESET;
	}

	if (sess->packet_class == L7PROXY_PACKET)
		goto update_existing;

prepare_snat:
	/* Resolve external L2 before allocating or mutating TCP session state.
	 * prepare_egress_l2 never drops the packet (unresolved on-link falls back
	 * to the gateway MAC), so session state can be installed right after. */
	prepare_egress_l2(skb, l2, key.dst_ip);

	if (create_snat) {
		snat_ip = pick_snat_ip_port(mvm_meta->ip, &key, &snat_port);
		if (!snat_ip || !snat_ip->ip || !snat_port)
			return TCP_NAT_DROP;
		goto create_session;
	}

	/* prepare_egress_l2() may update the neighbor map. Reacquire the session
	 * value before updating it or using it for the NAT rewrite.
	 */
	sess = bpf_map_lookup_elem(&egress_sessions, &key);
	if (!sess || sess->packet_class == L7PROXY_PACKET)
		return TCP_NAT_DROP;

update_existing:
	/* update session */
	update_session(IP_CT_DIR_ORIGINAL, sess, now, syn, ack, fin, rst);

do_nat:
	if (sess->packet_class == L7PROXY_PACKET) {
		if (sess->l7_scheme == L7_SCHEME_HTTP)
			l7_mark = cube_l7_mark_http;
		else if (sess->l7_scheme == L7_SCHEME_HTTPS)
			l7_mark = cube_l7_mark_https;
		else
			return TCP_NAT_DROP;

		skb->mark = (skb->mark & ~cube_l7_mark_mask) | l7_mark;
		return TCP_NAT_PACK(sess->node_ifindex, TCP_L7PROXY_OK);
	}

	old_saddr = l3->saddr;
	new_saddr = sess->node_ip;
	old_sport = l4->source;
	new_sport = sess->node_port;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	tcp_csum_off = TCP_CSUM_OFF(ip_hlen);

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

	inner_map = bpf_map_lookup_elem(&dns_allow_v2, &ifindex);
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

	dns_track_allowed_query(skb, state, matched, qname_hash);
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
	struct mvm_port mvm_port = {};
	struct nat_session *sess;
	struct session_key pmkey = {};
	struct mvm_meta *mvm_meta;
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

			/* A port_mapping session whose gen differs from the VM's current
			 * mvm_meta->version is stale (the sandbox was rolled back); reset
			 * the guest side so the application unblocks. A miss forwards
			 * statelessly (today's behaviour). port_mapping sessions have no
			 * ingress entry. */
			port_mapping_key(&pmkey, l3->daddr, l4->dest, l4->source);
			sess = bpf_map_lookup_elem(&egress_sessions, &pmkey);
			if (sess && session_is_stale(sess)) {
				bpf_map_delete_elem(&egress_sessions, &pmkey);
				return tcp_send_reset(skb, skb->ingress_ifindex, mvm_inner_ip);
			}

			err = snat_tcp(skb, ifindex, l2, l3, l4, l4->source, *host_port);
			if (err)
				return TC_ACT_SHOT;

			return bpf_redirect(nodenic_ifindex, 0);
		}
	}

	ret = pull_headers(skb, &l2, &l3);
	if (ret != TC_ACT_OK)
		return ret;

	if (!should_do_nat(l3))
		return TC_ACT_SHOT;

	if (l3->daddr == nodenic_ip) {
		/* This branch bypasses do_*_nat() and therefore the policy
		 * check applied there. Enforce the unified egress policy
		 * inline. TCP callers get an RST to match the guest-visible
		 * behavior of the do_tcp_nat() path; UDP/ICMP silently drop.
		 * dport is 0 because the original check was port-agnostic.
		 */
		switch (classify_egress_flow(ifindex, daddr, 0)) {
		case FLOW_REJECT:
			if (proto == IPPROTO_TCP)
				return tcp_send_reset(skb, skb->ingress_ifindex, mvm_inner_ip);
			return TC_ACT_SHOT;
		default:
			return bpf_redirect(cubegw0_ifindex, BPF_F_INGRESS);
		}
	}

	if (proto == IPPROTO_TCP) {
		tcp_ret = do_tcp_nat(skb, mvm_meta);
		if (TCP_NAT_STATUS(tcp_ret) == TCP_NAT_OK)
			return bpf_redirect(TCP_NAT_IFINDEX(tcp_ret), egress_redirect_flags);
		if (TCP_NAT_STATUS(tcp_ret) == TCP_L7PROXY_OK)
			return bpf_redirect(TCP_NAT_IFINDEX(tcp_ret), BPF_F_INGRESS);
		if (TCP_NAT_STATUS(tcp_ret) == TCP_NAT_RESET)
			return tcp_send_reset(skb, skb->ingress_ifindex, mvm_inner_ip);
		return TC_ACT_SHOT;
	}

	/* Unresolved on-link destinations fall back to the gateway MAC inside
	 * prepare_egress_l2 (no drop/probe), so the packet always forwards. */
	prepare_egress_l2(skb, l2, daddr);

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
