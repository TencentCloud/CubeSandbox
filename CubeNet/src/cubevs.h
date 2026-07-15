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
#define NSEC_PER_SEC			1000000000ULL
#define DNS_QUERY_TRACK_TTL_NS		(10ULL * NSEC_PER_SEC)

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
const volatile __u32 nodenic_ifindex    = 2;
const volatile __u32 nodenic_macaddr_p1 = 0x68005452;	/* 52:54:00:68:dd:16 */
const volatile __u16 nodenic_macaddr_p2 = 0x16dd;

/* MAC address of the Node gateway (next hop) */
const volatile __u32 nodegw_macaddr_p1  = 0x4732eefe;	/* fe:ee:32:47:6b:93 */
const volatile __u16 nodegw_macaddr_p2  = 0x936b;

struct mvm_meta {
	__u32 version;
	__u32 ip;
	__u8 uuid[64];
	__u8 dns_policy_flags;
	__u8 reserved[55];
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

struct dns_allow_key {
	__u32 prefixlen;
	char name[MAX_DNS_NAME_LEN];
};

struct dns_allow_value {
	__u32 name_len;
	__u8 flags;
	__u8 reserved[3];
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
	__u8 reserved[7];
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

struct net_policy_value_v2 {
	__u64 expires_at_ns;
	__u8 flags;
	__u8 reserved[7];
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
	__u8 reserved[34];
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
	int r[sizeof(struct net_policy_value_v2) == 16 ? 1 : -1] = {};
	int f[sizeof(struct dns_allow_key) == MAX_DNS_NAME_LEN + 4 ? 1 : -1] = {};
	int g[sizeof(struct dns_allow_value) == 8 ? 1 : -1] = {};
	int h[sizeof(struct dns_query_track_key) == 24 ? 1 : -1] = {};
	int i[sizeof(struct dns_query_track_value) == 16 ? 1 : -1] = {};
	int l[sizeof(struct mvm_port) == 8 ? 1 : -1] = {};
	int n[sizeof(struct session_key) % 20 == 0 ? 1 : -1] = {};
	int o[sizeof(struct nat_session) % 64 == 0 ? 1 : -1] = {};
	int p[sizeof(struct ingress_session) % 16 == 0 ? 1 : -1] = {};
	int q[sizeof(struct snat_ip) % 16 == 0 ? 1 : -1] = {};

	return b[0] + d[0] + r[0] + f[0] + g[0] + h[0] + i[0] + l[0] + n[0] + o[0] + p[0] + q[0];
}

static __always_inline __attribute__((used)) __u32 __btf_pin(void)
{
	return __builtin_btf_type_id(*(struct lpm_key *)0, BPF_TYPE_ID_LOCAL) +
	       __builtin_btf_type_id(*(struct net_policy_value_v2 *)0, BPF_TYPE_ID_LOCAL) +
	       __builtin_btf_type_id(*(struct dns_allow_value *)0, BPF_TYPE_ID_LOCAL);
}

#endif /* __CUBEVS_H */
