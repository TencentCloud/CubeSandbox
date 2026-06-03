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
#define MAX_PORTS			65536
#define MAX_SESSIONS			1048576
#define MAX_SNAT_IPS			4
#define MAX_PORT_START			30000

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
	__u8 reserved[56];
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

struct mvm_port {
	__u32 ifindex;
	__u16 listen_port;
	__u16 reserved;
};

/* The size of this structure must be a multiple of 4 */
struct csum_buff {
	__u32 addr;
	__u16 port;
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

/* static assert, make sure size of structs are expected
 */
static __always_inline int _()
{
	int b[sizeof(struct mvm_meta) == 128 ? 1 : -1] = {};
	int d[sizeof(struct lpm_key) == 8 ? 1 : -1] = {};
	int l[sizeof(struct mvm_port) == 8 ? 1 : -1] = {};
	int m[sizeof(struct csum_buff) % 4 == 0 ? 1 : -1] = {};
	int n[sizeof(struct session_key) % 20 == 0 ? 1 : -1] = {};
	int o[sizeof(struct nat_session) % 64 == 0 ? 1 : -1] = {};
	int p[sizeof(struct ingress_session) % 16 == 0 ? 1 : -1] = {};
	int q[sizeof(struct snat_ip) % 16 == 0 ? 1 : -1] = {};

	return b[0] + d[0] + l[0] + m[0] + n[0] + o[0] + p[0] + q[0];
}

static __always_inline __u16 csum_fold(__wsum sum)
{
	sum = (sum & 0xffff) + (sum >> 16);
	return ~((sum & 0xffff) + (sum >> 16));
}

#endif /* __CUBEVS_H */
