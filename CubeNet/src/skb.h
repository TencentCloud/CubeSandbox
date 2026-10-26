// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2022 Cube Authors */
#ifndef __SKB_H
#define __SKB_H

static __always_inline int pull_headers(struct __sk_buff *skb, struct ethhdr **l2, struct iphdr **l3)
{
	void *data, *data_end;
	__u32 len;
	long err;

	/* Make sure all the headers are in SKB linear section,
	 * so that we don't have to use bpf_skb_load_bytes later.
	 */
	err = bpf_skb_pull_data(skb, SKB_HDRS_LEN);
	if (err)
		return TC_ACT_SHOT;

	data_end = (void *)(__u64)skb->data_end;
	data = (void *)(__u64)skb->data;

	len = sizeof(struct ethhdr) + sizeof(struct iphdr);
	if (data + len > data_end)
		return TC_ACT_SHOT;

	*l2 = data;
	*l3 = (struct iphdr *)(*l2 + 1);

	return TC_ACT_OK;
}

/**
 * DEFINE_PULL_L4_HEADERS - generate a pull_headers function for a given L4 protocol
 * @name:     function name to generate
 * @proto_id: IPPROTO_* constant
 * @l4_type:  L4 header struct type (e.g. tcphdr, udphdr, icmphdr)
 */
#define DEFINE_PULL_L4_HEADERS(name, proto_id, l4_type)					\
static __always_inline bool name(struct __sk_buff *skb, struct ethhdr **l2_ptr,		\
				 struct iphdr **l3_ptr, struct l4_type **l4_ptr)		\
{											\
	void *data, *data_end;								\
	struct ethhdr *l2;								\
	struct iphdr *l3;								\
	struct l4_type *l4;								\
	__u16 ip_hlen;									\
	long err;									\
											\
	/* pull l2/l3 headers */							\
	err = bpf_skb_pull_data(skb, sizeof(struct ethhdr) + sizeof(struct iphdr));	\
	if (err)									\
		return false;								\
											\
	data_end = (void *)(__u64)skb->data_end;					\
	data = (void *)(__u64)skb->data;						\
											\
	l2 = (void *)data;								\
	l3 = (void *)(l2 + 1);								\
	if ((void *)(l3 + 1) > data_end)						\
		return false;								\
											\
	if (l3->protocol != proto_id)							\
		return false;								\
											\
	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);					\
	ip_hlen <<= 2;									\
	if (ip_hlen < sizeof(struct iphdr) || ip_hlen > 60)				\
		return false;								\
											\
	/* pull l2/l3/l4 headers */							\
	err = bpf_skb_pull_data(skb, sizeof(struct ethhdr) + ip_hlen +			\
				sizeof(struct l4_type));					\
	if (err)									\
		return false;								\
											\
	/* the pull right above invalidates data/data_end */				\
	data_end = (void *)(__u64)skb->data_end;					\
	data = (void *)(__u64)skb->data;						\
											\
	l2 = (void *)data;								\
	l3 = (void *)(l2 + 1);								\
	if ((void *)(l3 + 1) > data_end)						\
		return false;								\
											\
	/* Re-read ip_hlen from the refreshed packet data so the verifier can		\
	 * track the range of the l4 pointer offset after bpf_skb_pull_data.		\
	 */										\
	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);					\
	ip_hlen <<= 2;									\
	if (ip_hlen < sizeof(struct iphdr) || ip_hlen > 60)				\
		return false;								\
											\
	l4 = (void *)(data + sizeof(*l2) + ip_hlen);					\
	if ((void *)(l4 + 1) > data_end)						\
		return false;								\
											\
	*l2_ptr = l2;									\
	*l3_ptr = l3;									\
	*l4_ptr = l4;									\
											\
	return true;									\
}

DEFINE_PULL_L4_HEADERS(__pull_headers, IPPROTO_TCP, tcphdr)
DEFINE_PULL_L4_HEADERS(__pull_headers_udp, IPPROTO_UDP, udphdr)
DEFINE_PULL_L4_HEADERS(__pull_headers_icmp, IPPROTO_ICMP, icmphdr)

#endif /* __SKB_H */
