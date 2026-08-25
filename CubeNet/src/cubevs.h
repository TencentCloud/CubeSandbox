// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2022 Cube Authors */
#ifndef __CUBEVS_H
#define __CUBEVS_H

/* https://elixir.bootlin.com/linux/v5.4.217/source/include/uapi/linux/pkt_cls.h#L33 */
#define TC_ACT_OK			0

/* https://elixir.bootlin.com/linux/v5.4.222/source/include/uapi/linux/pkt_cls.h#L35 */
#define TC_ACT_SHOT			2

/* https://elixir.bootlin.com/linux/v5.4.217/source/include/uapi/linux/if_ether.h#L52 */
#define ETH_P_IP			0x0800	/* Internet Protocol packet */
/* https://elixir.bootlin.com/linux/v5.4.217/source/include/uapi/linux/if_ether.h#L54 */
#define ETH_P_ARP			0x0806	/* Address Resolution packet */

#define ETH_ALEN			6

/* https://elixir.bootlin.com/linux/v5.4.217/source/include/uapi/linux/if_arp.h#L105 */
/* ARP protocol opcodes */
#define ARPOP_REQUEST			1	/* ARP request */
#define ARPOP_REPLY			2	/* ARP reply */

/* https://elixir.bootlin.com/linux/v5.4.217/source/include/uapi/linux/if_arp.h#L29 */
/* ARP hardware types */
#define ARPHRD_ETHER			1	/* Ethernet */

/* https://elixir.bootlin.com/linux/v5.4.217/source/include/linux/socket.h#L172 */
#define AF_INET				2

#define MAX_ENTRIES			8192
#define MAX_IP_RULE_ENTRIES		8192
#define MAX_DOMAIN_RULE_ENTRIES		1024
#define MAX_PORTS			65536
#define MAX_SESSIONS			1048576
#define MAX_SNAT_IPS			4
#define MAX_PORT_START			30000
#define MAX_DNS_QUERY_TRACK_ENTRIES	65536
#define MAX_DNS_NAME_LEN		256
#define DNS_POLICY_FLAG_LEARNING_ENABLED	1
#define NET_POLICY_FLAG_L7_REQUIRED	1
/* Set alongside NET_POLICY_FLAG_L7_REQUIRED when a domain is present in BOTH
 * a plain (L3) allow_out rule and an L7 rule. Tells dns_learn_response_ip to
 * learn the plain /32 any-port entry in addition to the L7 (ip, port)/48
 * entries, so non-rule ports keep plain SNAT access while the rule's ports are
 * L7-intercepted. Without it an L7 rule silently narrows a same-domain plain
 * allow_out to only the rule's ports.
 */
#define NET_POLICY_FLAG_L3_ALLOWED	2
#define NSEC_PER_SEC			1000000000ULL

/* L7 scheme values embedded in dns_allow_value / net_policy_value_v2 per-port
 * entries. Used by the eBPF datapath to compute skb->mark so CubeEgress's
 * iptables TPROXY rules can steer HTTP vs HTTPS traffic to distinct listeners
 * without depending on the destination port number. Keep in sync with
 * cubevs/cubevs.go (L7SchemeHTTP / L7SchemeHTTPS).
 */
#define L7_SCHEME_NONE	0
#define L7_SCHEME_HTTP	1
#define L7_SCHEME_HTTPS	2

/* Maximum number of (port, scheme) tuples a single L7 rule host may declare.
 * Bounded so the map value size stays small and BPF verifier can unroll the
 * lookup loop. Users needing more should merge rules or reuse ports.
 */
#define MAX_L7_PORTS_PER_HOST	8

/* skb->mark encoding for L7 redirect. The high 16 bits carry a cube-owned
 * prefix (0xCE?? masked by cube_l7_mark_mask) so cube marks do not collide
 * with unrelated mark bits users may set elsewhere. iptables uses
 * `-m mark --mark VAL/MASK` to match on cube-owned bits only.
 *
 * These are const volatile globals (not macros) so a deployment can override
 * them at load time from userspace (rewriteConstants, sourced from the same
 * install-time config the iptables init script reads), keeping the dataplane
 * and iptables in lock-step. Defaults match the shipped values.
 */
const volatile __u32 cube_l7_mark_mask = 0xFFFF0000u;
const volatile __u32 cube_l7_mark_http = 0xCE010000u;
const volatile __u32 cube_l7_mark_https = 0xCE020000u;
#define DNS_QUERY_TRACK_TTL_NS		(10ULL * NSEC_PER_SEC)
#define DIRECT_NEIGH_PROBE_INTERVAL_NS		(1ULL * NSEC_PER_SEC)
#define DIRECT_NEIGH_REVALIDATE_INTERVAL_NS	(5ULL * 60 * NSEC_PER_SEC)

/* https://en.wikipedia.org/wiki/IPv4#Header
 *
 * +---+---+---+---+---+---+---+---+---+---+---+---+---+---+---+---+
 * | 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10| 11| 12| 13| 14| 15|
 * +---+---+---+---------------------------------------------------+
 * | RS| DF| MF|                  Fragment Offset                  |
 * +---+---+---+---------------------------------------------------+
 */
#define IP_FLAG_MF			bpf_ntohs(0x2000)
#define IP_FRAG_OFF_MASK		bpf_ntohs(0x1fff)

/* This is a combination of eBPF, SCF and 00700. :) */
#define HASH_SEED			0xebcf0700

/* We manipulate the packet headers only */
#define SKB_HDRS_LEN			(sizeof(struct ethhdr) + sizeof(struct iphdr))

/* Offsets to the start of the packet */
#define IP_CSUM_OFF			(sizeof(struct ethhdr) + offsetof(struct iphdr, check))
#define IP_TOT_LEN_OFF			(sizeof(struct ethhdr) + offsetof(struct iphdr, tot_len))
#define IP_SADDR_OFF			(sizeof(struct ethhdr) + offsetof(struct iphdr, saddr))
#define IP_DADDR_OFF			(sizeof(struct ethhdr) + offsetof(struct iphdr, daddr))
#define TCP_CSUM_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct tcphdr, check))
#define TCP_SRC_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct tcphdr, source))
#define TCP_DST_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct tcphdr, dest))
#define UDP_CSUM_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct udphdr, check))
#define UDP_SRC_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct udphdr, source))
#define UDP_DST_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct udphdr, dest))
#define ICMP_CSUM_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct icmphdr, checksum))
#define ICMP_ECHO_ID_OFF(LEN)		(sizeof(struct ethhdr) + LEN + offsetof(struct icmphdr, un.echo.id))

/* Current network namespace */
#define BPF_F_CURRENT_NETNS		(-1L)

/* IP and MAC address inside MVMs */
const volatile __u32 mvm_inner_ip       = 0x0644fea9;	/* 169.254.68.6, network byte order */
const volatile __u32 mvm_macaddr_p1     = 0xfc6f9020;	/* 20:90:6f:fc:fc:fc */
const volatile __u16 mvm_macaddr_p2     = 0xfcfc;

/* next hop of MVM */
const volatile __u32 mvm_gateway_ip     = 0x0544fea9;	/* 169.254.68.5, network byte order */

/* Ifindex, IP and MAC address of the cube-dev device (serve as gateway for MVM) */
const volatile __u32 cubegw0_ip         = 0x017100cb;	/* 203.0.113.1, network byte order */
const volatile __u32 cubegw0_ifindex    = 216;
const volatile __u32 cubegw0_macaddr_p1 = 0xcf6f9020;	/* 20:90:6f:cf:cf:cf */
const volatile __u16 cubegw0_macaddr_p2 = 0xcfcf;

/* L2 rewrite and redirect flags for ordinary egress traffic.
 * direct mode: src=node MAC, dst=node gateway MAC, redirect flags=0.
 * custom mode: src=MVM MAC, dst=cube-router MAC, redirect flags=BPF_F_INGRESS.
 */
const volatile __u32 egress_smacaddr_p1     = 0xfc6f9020;	/* 20:90:6f:fc:fc:fc */
const volatile __u16 egress_smacaddr_p2     = 0xfcfc;
const volatile __u32 egress_dmacaddr_p1     = 0xcf6f9022;	/* 22:90:6f:cf:cf:cf */
const volatile __u16 egress_dmacaddr_p2     = 0xcfcf;
const volatile __u64 egress_redirect_flags  = BPF_F_INGRESS;

/* Ifindex, IP and MAC address of Node itself */
const volatile __u32 nodenic_ip         = 0x020a8709;	/* 9.135.10.2, network byte order */
const volatile __u32 nodenic_netmask    = 0x00ffffff;	/* 255.255.255.0, packet-byte layout */
const volatile __u32 nodenic_ifindex    = 2;
const volatile __u32 nodenic_macaddr_p1 = 0x68005452;	/* 52:54:00:68:dd:16 */
const volatile __u16 nodenic_macaddr_p2 = 0x16dd;

/* MAC address of the Node gateway (next hop) */
const volatile __u32 nodegw_macaddr_p1  = 0x4732eefe;	/* fe:ee:32:47:6b:93 */
const volatile __u16 nodegw_macaddr_p2  = 0x936b;

/* policy_version is the per-sandbox network-policy generation. It is bumped by
 * userspace after a policy update has been fully applied, and every packet on
 * an established flow compares it against the generation cached in nat_session
 * to decide whether the flow must be re-evaluated.
 *
 * No value is reserved: this is a plain counter compared for equality. A fresh
 * TAP starts at 0, which is also what metadata and sessions written before this
 * field existed carry, so an upgrade finds them in agreement and re-checks
 * nothing -- those flows were admitted under a policy that has not changed.
 * Wrapping takes 2^32 updates to one sandbox and would at worst let a session
 * that outlived all of them skip one re-check, so it is not special-cased.
 *
 * Do NOT reuse the `version` field above for this: it is part of session_key,
 * so bumping it would orphan every live session for this TAP.
 */
struct mvm_meta {
	__u32 version;
	__u32 ip;
	__u8 uuid[64];
	__u8 dns_policy_flags;
	__u8 reserved0[3];	/* aligns policy_version; reserved starts at an odd offset */
	__u32 policy_version;
	__u8 reserved[48];
};

/* https://elixir.bootlin.com/linux/v5.4.217/source/include/uapi/linux/if_arp.h#L144 */
/* Linux kernel defines struct arphdr ONLY, we need the Ethernet part */
struct arphdr_eth {
	__be16 ar_hrd;			/* format of hardware address */
	__be16 ar_pro;			/* format of protocol address */
	unsigned char ar_hln;		/* length of hardware address */
	unsigned char ar_pln;		/* length of protocol address */
	__be16 ar_op;			/* ARP opcode (command) */
	unsigned char ar_sha[ETH_ALEN];	/* sender hardware address */
	__be32 ar_sip;			/* sender IP address */
	unsigned char ar_tha[ETH_ALEN];	/* target hardware address */
	__be32 ar_tip;			/* target IP address */
} __attribute__((packed));

struct arp_packet {
	struct ethhdr eth;
	struct arphdr_eth arp;
} __attribute__((packed));

struct direct_neighbor {
	unsigned char addr[ETH_ALEN];
	__u16 reserved;
	__u64 next_probe_at_ns;
};

union macaddr {
	struct {
		__u32 p1;
		__u16 p2;
	};
	__u8 addr[6];
} __attribute__((packed));

struct lpm_key {
	__u32 prefixlen;
	__u32 ip;
};

/* LPM key for allow_out_v3. Carries the destination IP (32-bit, network
 * byte order) and an optional destination port (16-bit, network byte order)
 * so a single longest-prefix lookup resolves:
 *   - exact (ip, port): prefixlen = 48  (ip[4] + port[2])
 *   - ip only (any port): prefixlen = 32  (ip[4], port ignored)
 *   - ip/mask subnet:        prefixlen < 32  (only top bits of ip matter)
 * The struct is padded to 12 bytes (8-byte data payload) so the trie's
 * word-wise compare stays 4-byte aligned on every kernel. Insert and
 * lookup MUST both fill ip/port from network-byte-order bytes (matching
 * iphdr->daddr / tcphdr->dest), never from a host-byte-order integer
 * shift — otherwise the exact (ip, port) match silently fails.
 */
struct lpm_key_v3 {
	__u32 prefixlen;
	__u32 ip;     /* network byte order */
	__u16 port;   /* network byte order; 0 when key is ip-only/subnet */
	__u16 _pad;   /* 0; keeps the data payload at 8 bytes */
};

struct dns_allow_key {
	__u32 prefixlen;
	char name[MAX_DNS_NAME_LEN];
};

/* Per-host L7 port entry. Attached inline to both dns_allow_value and
 * net_policy_value_v2 so the datapath can pick the right scheme for a given
 * destination port without a second map lookup. port is in network byte order
 * (matches tcphdr->dest), scheme is one of L7_SCHEME_HTTP / L7_SCHEME_HTTPS.
 */
struct l7_port_entry {
	__u16 port;   /* network byte order */
	__u8 scheme;
	__u8 _pad;
};

/* dns_allow_value carries the L7 policy attached to a matched DNS name.
 * port_count = 0 is the "unspecified" case: the datapath applies the default
 * port set {80/http, 443/https} for backward compatibility with rules that
 * omit port. port_count > 0 restricts L7 handling to the listed (port, scheme)
 * tuples only.
 */
struct dns_allow_value {
	__u32 name_len;
	__u8 flags;
	__u8 port_count;
	__u8 reserved[2];
	struct l7_port_entry ports[MAX_L7_PORTS_PER_HOST];
};

struct dns_query_track_key {
	__u32 ifindex;
	__u32 server_ip;
	__u16 source_port;
	__u16 dns_id;
	__u32 reserved;
	__u64 qname_hash;
};

struct dns_query_track_value {
	__u64 expires_at_ns;
	__u8 flags;
	__u8 port_count;
	__u8 reserved[6];
	struct l7_port_entry ports[MAX_L7_PORTS_PER_HOST];
};

/* Per-packet query parser state shared by the DNS tail-call pipeline. */
struct dns_query_state {
	__u32 dns_off;
	__u32 ifindex;
	__u16 flags;
	__u32 cursor;
	__u32 label_remaining;
	__u32 dotted_len;
	__u32 reverse_pos;
	bool failed;
	bool done;
	char name[MAX_DNS_NAME_LEN];
};

/* net_policy_value_v2 stores the per-sandbox allow_out_v2 verdict. This is the
 * legacy 16-byte layout read only when migrating a pre-v3 allow_out_v2 map to
 * allow_out_v3; the current dataplane uses net_policy_value_v3.
 */
struct net_policy_value_v2 {
	__u64 expires_at_ns;
	__u8 flags;
	__u8 reserved[7];
};

/* Per-sandbox allow_out_v3 verdict. Unlike v2, the port lives in the
 * LPM key (see lpm_key_v3), so the value no longer needs the 8-tuple
 * (port, scheme) array: the scheme is resolved at insert time and
 * stored here directly. A zero expires_at_ns is a static entry; a
 * non-zero expires_at_ns is a temporary DNS-learned entry.
 */
struct net_policy_value_v3 {
	__u64 expires_at_ns;
	__u8 flags;
	__u8 scheme;   /* L7_SCHEME_* */
	/* prefixlen of the lpm_key_v3 this value was written under. LPM trie
	 * lookups are longest-prefix, so a lookup for key K may return an
	 * entry written under a SHORTER covering key; writers that mean to
	 * merge with an existing entry for the EXACT same key must compare
	 * this field against their key's prefixlen first.
	 */
	__u8 key_prefixlen;
	__u8 reserved[5];
};

struct mvm_port {
	__u32 ifindex;
	__u16 listen_port;
	__u16 reserved;
};

struct session_key {
	__u32 src_ip;
	__u32 dst_ip;
	__u16 src_port;
	__u16 dst_port;
	__u32 version;	/* 0 for ingress session */
	__u8 protocol;
	__u8 reserved[3];
};

struct nat_session {
	__u64 access_time;	/* stored in nanoseconds, div is expensive */
	__u32 node_ifindex;
	__u32 node_ip;
	__u32 vm_ifindex;
	__u32 vm_ip;
	__u16 node_port;
	__u16 vm_port;
	__u8 state;
	__u8 active_close;
	__u8 packet_class;	/* SNAT_PACKET or L7PROXY_PACKET */
	__u8 l7_scheme;		/* L7_SCHEME_*; NONE for non-L7 sessions */
	__u32 policy_version;	/* mvm_meta.policy_version at create / last re-check */
	__u8 reserved[28];
};

struct ingress_session {
	__u32 version;
	__u32 vm_ip;
	__u16 vm_port;
	__u16 reserved[3];
};

struct snat_ip {
	struct bpf_spin_lock lock;	/* guard max_port */
	__u32 ifindex;
	__u32 ip;
	__u16 max_port;			/* the next port to be used */
	__u16 reserved;
};

/* Tail-call state for DNS response handling on the ingress UDP NAT path.
 *
 * The response handler is split into its own tail-called program to keep the
 * from_world verifier complexity within the 1M instruction budget. We stash
 * the values the caller already derived (DNS payload offset, target sandbox
 * ifindex, DNS server IP, sandbox-side port) so the tail-called program can
 * re-pull headers, learn A records, and finish UDP NAT without re-deriving
 * them from scratch.
 */
struct dns_response_state {
	__u32 dns_off;
	__u32 ifindex;		/* sandbox tap ifindex (sess->vm_ifindex) */
	__u32 server_ip;	/* DNS server IP (l3->saddr in network byte order) */
	__u16 source_port;	/* sandbox-side UDP port (sess->vm_port in nbo) */
	__u16 reserved;
};

/* skb->cb[0] is reserved as a per-invocation NAT status word used by
 * create_nat_session() to communicate the failure reason back to callers
 * in from_cube(). skb->cb[] is 5 * u32 scratch that survives across
 * bpf-to-bpf calls within a single program invocation, so this works even
 * when the session helpers are compiled as subprogs.
 */
#define NAT_CB_STATUS_INDEX		0
#define NAT_CB_OK			0
#define NAT_CB_DENIED_BY_POLICY		1

static __always_inline void nat_cb_set(struct __sk_buff *skb, __u32 status)
{
	skb->cb[NAT_CB_STATUS_INDEX] = status;
}

static __always_inline __u32 nat_cb_get(const struct __sk_buff *skb)
{
	return skb->cb[NAT_CB_STATUS_INDEX];
}

/* static assert, make sure size of structs are expected
 */
static __always_inline int _()
{
	int b[sizeof(struct mvm_meta) == 128 ? 1 : -1] = {};
	int d[sizeof(struct lpm_key) == 8 ? 1 : -1] = {};
	int dv3[sizeof(struct lpm_key_v3) == 12 ? 1 : -1] = {};
	int r[sizeof(struct net_policy_value_v2) == 16 ? 1 : -1] = {};
	int rv3[sizeof(struct net_policy_value_v3) == 16 ? 1 : -1] = {};
	int f[sizeof(struct dns_allow_key) == MAX_DNS_NAME_LEN + 4 ? 1 : -1] = {};
	int g[sizeof(struct dns_allow_value) == 40 ? 1 : -1] = {};
	int h[sizeof(struct dns_query_track_key) == 24 ? 1 : -1] = {};
	int i[sizeof(struct dns_query_track_value) == 48 ? 1 : -1] = {};
	int l[sizeof(struct mvm_port) == 8 ? 1 : -1] = {};
	int n[sizeof(struct session_key) % 20 == 0 ? 1 : -1] = {};
	int o[sizeof(struct nat_session) == 64 ? 1 : -1] = {};
	int p[sizeof(struct ingress_session) % 16 == 0 ? 1 : -1] = {};
	int q[sizeof(struct snat_ip) % 16 == 0 ? 1 : -1] = {};
	int s[sizeof(struct l7_port_entry) == 4 ? 1 : -1] = {};

	return b[0] + d[0] + dv3[0] + r[0] + rv3[0] + f[0] + g[0] + h[0] + i[0] + l[0] + n[0] + o[0] + p[0] + q[0] + s[0];
}

static __always_inline __attribute__((used)) __u32 __btf_pin(void)
{
	return __builtin_btf_type_id(*(struct lpm_key *)0, BPF_TYPE_ID_LOCAL) +
	       __builtin_btf_type_id(*(struct net_policy_value_v2 *)0, BPF_TYPE_ID_LOCAL) +
	       __builtin_btf_type_id(*(struct lpm_key_v3 *)0, BPF_TYPE_ID_LOCAL) +
	       __builtin_btf_type_id(*(struct net_policy_value_v3 *)0, BPF_TYPE_ID_LOCAL) +
	       __builtin_btf_type_id(*(struct dns_allow_key *)0, BPF_TYPE_ID_LOCAL) +
	       __builtin_btf_type_id(*(struct dns_allow_value *)0, BPF_TYPE_ID_LOCAL);
}

#endif /* __CUBEVS_H */
