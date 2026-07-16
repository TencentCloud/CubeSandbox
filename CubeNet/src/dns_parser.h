// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#ifndef __DNS_PARSER_H
#define __DNS_PARSER_H

#include <vmlinux.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#include "cubevs.h"

#define DNS_PORT		bpf_htons(53)
#define DNS_HDR_LEN		12
#define DNS_MAX_LABEL_LEN	63
#define DNS_MAX_NAME_LEN	MAX_DNS_NAME_LEN
#define DNS_FLAG_QR		0x8000
#define DNS_FLAG_OPCODE_MASK	0x7800
#define DNS_FLAG_AA		0x0400
#define DNS_FLAG_TC		0x0200
#define DNS_FLAG_RD		0x0100
#define DNS_FLAG_RA		0x0080
#define DNS_FLAG_Z_RESERVED	0x0040
#define DNS_RCODE_MASK		0x000f
#define DNS_OPCODE_STANDARD	0
#define DNS_RCODE_NOERROR	0
#define DNS_COMPRESS_PTR_MASK	0xc0
#define DNS_TYPE_A		1
#define DNS_CLASS_IN		1
#define DNS_IPV4_RDATA_LEN	4
#define DNS_MAX_RESPONSE_ANSWERS	8
#define DNS_MAX_RDATA_LEN	256
#define DNS_QNAME_HASH_OFFSET	14695981039346656037ULL
#define DNS_QNAME_HASH_PRIME	1099511628211ULL

#if (DNS_MAX_NAME_LEN & (DNS_MAX_NAME_LEN - 1)) != 0
#error "DNS_MAX_NAME_LEN must be a power of two"
#endif

struct dns_wire_header {
	__be16 id;
	__be16 flags;
	__be16 qdcount;
	__be16 ancount;
	__be16 nscount;
	__be16 arcount;
} __attribute__((packed));

struct dns_question_footer {
	__be16 qtype;
	__be16 qclass;
} __attribute__((packed));

struct dns_rr_header {
	__be16 type;
	__be16 klass;
	__be32 ttl;
	__be16 rdlength;
} __attribute__((packed));

/* Compute the DNS payload offset and reject UDP/53 packets too short for DNS. */
static __always_inline bool dns_payload_offset(struct iphdr *l3, struct udphdr *udp,
					       __u32 *dns_off)
{
	__u32 ip_hlen;
	__u32 ip_len;
	__u16 udp_len;

	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;
	if (ip_hlen < sizeof(struct iphdr) || ip_hlen > 60)
		return false;

	ip_len = bpf_ntohs(l3->tot_len);
	udp_len = bpf_ntohs(udp->len);
	if (ip_len < ip_hlen + udp_len)
		return false;
	if (udp_len < sizeof(struct udphdr) + DNS_HDR_LEN)
		return false;

	*dns_off = sizeof(struct ethhdr) + ip_hlen + sizeof(struct udphdr);
	return true;
}

/* Parse a plain DNS QNAME from the Question section.
 *
 * The query Question section and normal response echo do not need compressed
 * names. Answer owner names are only skipped on the response path.
 */
static __always_inline char dns_qname_lower(char c)
{
	if (c >= 'A' && c <= 'Z')
		return c + ('a' - 'A');
	return c;
}

static __always_inline bool dns_flags_are_standard(__u16 flags)
{
	if ((flags & DNS_FLAG_OPCODE_MASK) != DNS_OPCODE_STANDARD)
		return false;
	if (flags & DNS_FLAG_TC)
		return false;
	if (flags & DNS_FLAG_Z_RESERVED)
		return false;

	return (flags & DNS_RCODE_MASK) == DNS_RCODE_NOERROR;
}

static __always_inline bool dns_query_header_is_supported(const struct dns_wire_header *hdr,
							  __u16 flags)
{
	if (!dns_flags_are_standard(flags))
		return false;
	if (flags & (DNS_FLAG_AA | DNS_FLAG_RA))
		return false;
	if (bpf_ntohs(hdr->qdcount) != 1)
		return false;
	if (hdr->ancount != 0 || hdr->nscount != 0)
		return false;

	return true;
}

static __always_inline bool dns_response_header_is_supported(const struct dns_wire_header *hdr,
							     __u16 flags, __u16 *ancount)
{
	if (!dns_flags_are_standard(flags))
		return false;
	if (bpf_ntohs(hdr->qdcount) != 1)
		return false;

	*ancount = bpf_ntohs(hdr->ancount);
	return *ancount != 0;
}

/* Read a DNS request header and reject packets that are already responses. */
static __always_inline bool dns_read_query_header(struct __sk_buff *skb, __u32 dns_off,
						  struct dns_wire_header *hdr, __u16 *flags)
{
	if (bpf_skb_load_bytes(skb, dns_off, hdr, sizeof(*hdr)))
		return false;

	*flags = bpf_ntohs(hdr->flags);
	return !(*flags & DNS_FLAG_QR);
}

/* Read a DNS response header and reject packets that are not responses. */
static __always_inline bool dns_read_response_header(struct __sk_buff *skb, __u32 dns_off,
						     struct dns_wire_header *hdr, __u16 *flags)
{
	if (bpf_skb_load_bytes(skb, dns_off, hdr, sizeof(*hdr)))
		return false;

	*flags = bpf_ntohs(hdr->flags);
	return (*flags & DNS_FLAG_QR) != 0;
}

/* Skip a DNS name, accepting normal labels and answer-section compression. */
static __always_inline bool dns_skip_name(struct __sk_buff *skb, __u32 *cursor)
{
	__u32 off = *cursor;
	int i;

#pragma clang loop unroll(disable)
	for (i = 0; i < DNS_MAX_NAME_LEN; i++) {
		__u8 c;

		if (bpf_skb_load_bytes(skb, off, &c, sizeof(c)))
			return false;
		off++;

		if ((c & DNS_COMPRESS_PTR_MASK) == DNS_COMPRESS_PTR_MASK) {
			if (bpf_skb_load_bytes(skb, off, &c, sizeof(c)))
				return false;
			off++;
			*cursor = off;
			return true;
		}
		if ((c & DNS_COMPRESS_PTR_MASK) != 0 || c > DNS_MAX_LABEL_LEN)
			return false;
		if (c == 0) {
			*cursor = off;
			return true;
		}
		off += c;
	}

	return false;
}

static __always_inline bool dns_read_question_footer(struct __sk_buff *skb, __u32 cursor,
						     struct dns_question_footer *question)
{
	return bpf_skb_load_bytes(skb, cursor, question, sizeof(*question)) == 0;
}

static __always_inline void dns_hash_qname_byte(__u64 *hash, __u8 byte)
{
	*hash ^= byte;
	*hash *= DNS_QNAME_HASH_PRIME;
}

/* Hash one plain DNS QNAME exactly as it appears on the wire. */
static __noinline bool dns_hash_qname(struct __sk_buff *skb, __u32 *cursor,
				      struct dns_question_footer *question,
				      __u64 *qname_hash)
{
	__u32 label_remaining = 0;
	__u64 hash = DNS_QNAME_HASH_OFFSET;
	__u32 off = *cursor;
	int i;

#pragma clang loop unroll(disable)
	for (i = 0; i < DNS_MAX_NAME_LEN; i++) {
		__u8 c;

		if (bpf_skb_load_bytes(skb, off, &c, sizeof(c)))
			return false;
		dns_hash_qname_byte(&hash, c);
		off++;

		if (label_remaining == 0) {
			if (c == 0)
				goto read_footer;
			if ((c & DNS_COMPRESS_PTR_MASK) != 0 || c > DNS_MAX_LABEL_LEN)
				return false;
			label_remaining = c;
			continue;
		}

		/* Re-bound label_remaining for the verifier on older kernels
		 * that lose unsigned precision across the loop-back edge and
		 * track it as a signed value, causing state explosion.
		 */
		label_remaining = (label_remaining - 1) & DNS_MAX_LABEL_LEN;
	}

	return false;

read_footer:
	if (!dns_read_question_footer(skb, off, question))
		return false;

	*cursor = off + sizeof(*question);
	*qname_hash = hash;
	return true;
}

static __always_inline bool dns_question_footer_is_ipv4_a(const struct dns_question_footer *question)
{
	return question->qtype == bpf_htons(DNS_TYPE_A) && question->qclass == bpf_htons(DNS_CLASS_IN);
}

#endif /* __DNS_PARSER_H */
