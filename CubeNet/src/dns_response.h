// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#ifndef __DNS_RESPONSE_H
#define __DNS_RESPONSE_H

#include "dns_parser.h"
#include "map.h"

/* Inline twins of the dns_parser.h helpers used on the response path.
 *
 * The query pipeline calls the __noinline originals from a program that has
 * other bpf-to-bpf calls — those can sit in their own verifier frames. The
 * response path, however, runs from a SEC("tc") tail-called program that
 * must contain zero bpf-to-bpf calls so it is allowed to bpf_tail_call into
 * the UDP NAT finish program on kernel 5.4. We keep duplicate __always_inline
 * copies here so we can have it both ways without breaking the query path.
 */

static __always_inline bool dns_skip_name_inline(struct __sk_buff *skb, __u32 *cursor)
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

static __always_inline bool dns_hash_qname_inline(struct __sk_buff *skb, __u32 *cursor,
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

		label_remaining--;
	}

	return false;

read_footer:
	if (!dns_read_question_footer(skb, off, question))
		return false;

	*cursor = off + sizeof(*question);
	*qname_hash = hash;
	return true;
}

/* Check whether DNS response learning is enabled for this sandbox. */
static __always_inline bool dns_response_learning_enabled(__u32 ifindex)
{
	struct mvm_meta *mvm_meta;

	mvm_meta = bpf_map_lookup_elem(&ifindex_to_mvmmeta, &ifindex);
	return mvm_meta && (mvm_meta->dns_policy_flags & DNS_POLICY_FLAG_LEARNING_ENABLED);
}

/* Add an IPv4 A-record address as a temporary DNS-learned allow_out_v2 entry. */
static __always_inline void dns_learn_response_ip(__u32 ifindex, __u32 ip, __u32 ttl,
						  __u8 flags)
{
	struct lpm_key key = { .prefixlen = 32, .ip = ip };
	struct net_policy_value_v2 value = {
		.expires_at_ns = bpf_ktime_get_ns() + ((__u64)ttl * NSEC_PER_SEC),
		.flags = flags,
	};
	struct net_policy_value_v2 *old_value;
	void *inner_map;

	inner_map = bpf_map_lookup_elem(&allow_out_v2, &ifindex);
	if (!inner_map)
		return;

	/* DNS learning must not downgrade flags or shorten static allow rules. */
	old_value = bpf_map_lookup_elem(inner_map, &key);
	if (old_value) {
		value.flags |= old_value->flags;
		if (old_value->expires_at_ns == 0)
			value.expires_at_ns = 0;
	}

	bpf_map_update_elem(inner_map, &key, &value, BPF_ANY);
}

/* Return true when an answer RR carries an IN A record payload. */
static __always_inline bool dns_response_record_is_ipv4_a(const struct dns_rr_header *rr,
							  __u16 rdlength)
{
	return rr->type == bpf_htons(DNS_TYPE_A) &&
	       rr->klass == bpf_htons(DNS_CLASS_IN) &&
	       rdlength == DNS_IPV4_RDATA_LEN;
}

/* Parse one answer RR and learn its IPv4 address when it is an A record.
 *
 * Marked __always_inline (and using the *_inline DNS-name helpers above) so
 * the calling SEC("tc") program contains no bpf-to-bpf calls and can issue
 * bpf_tail_call on kernel 5.4.
 */
static __always_inline bool dns_process_response_answer(struct __sk_buff *skb,
							__u32 *cursor, __u32 ifindex,
							__u8 flags)
{
	struct dns_rr_header rr;
	__u16 rdlength;
	__u32 ip;
	__u32 ttl;

	if (!dns_skip_name_inline(skb, cursor))
		return false;
	if (bpf_skb_load_bytes(skb, *cursor, &rr, sizeof(rr)))
		return false;
	*cursor += sizeof(rr);

	rdlength = bpf_ntohs(rr.rdlength);
	if (dns_response_record_is_ipv4_a(&rr, rdlength)) {
		if (bpf_skb_load_bytes(skb, *cursor, &ip, sizeof(ip)))
			return false;
		ttl = bpf_ntohl(rr.ttl);
		if (ttl < 300) ttl = 300;
		dns_learn_response_ip(ifindex, ip, ttl, flags);
	}

	/* Keep cursor advancement bounded even for unsupported RR types. */
	if (rdlength > DNS_MAX_RDATA_LEN)
		return false;

	*cursor += rdlength;
	return true;
}

/* Lookup the pending DNS query that authorizes this response. */
static __always_inline struct dns_query_track_value *dns_lookup_response_query(
	__u32 ifindex, __u32 server_ip, __u16 source_port, __be16 dns_id,
	__u64 qname_hash, struct dns_query_track_key *track_key)
{
	track_key->ifindex = ifindex;
	track_key->server_ip = server_ip;
	track_key->source_port = source_port;
	track_key->dns_id = dns_id;
	track_key->qname_hash = qname_hash;
	return bpf_map_lookup_elem(&dns_query_track, track_key);
}

/* Response hook for DNS replies returning to a sandbox.
 *
 * The path learns IPv4 A records into allow_out_v2 as temporary DNS-learned IP
 * policy entries. It intentionally preserves the existing filtering semantics.
 *
 * Marked __always_inline so the calling SEC("tc") program contains no
 * bpf-to-bpf calls; kernel 5.4 forbids mixing tail calls with bpf-to-bpf
 * calls in the same program, and we need the tail call to hand the packet
 * off to the post-DNS UDP NAT finish program.
 */
static __always_inline void dns_handle_response(struct __sk_buff *skb, __u32 dns_off,
						__u32 ifindex, __u32 server_ip,
						__u16 source_port)
{
	struct dns_query_track_value *query;
	struct dns_query_track_key track_key = {};
	struct dns_wire_header hdr;
	struct dns_question_footer question;
	__u32 cursor = dns_off + DNS_HDR_LEN;
	__u64 qname_hash = 0;
	__u64 now;
	__u16 ancount;
	__u16 flags;
	int i;

	if (!dns_response_learning_enabled(ifindex))
		return;

	if (!dns_read_response_header(skb, dns_off, &hdr, &flags))
		return;

	if (bpf_ntohs(hdr.qdcount) != 1)
		return;
	if (!dns_hash_qname_inline(skb, &cursor, &question, &qname_hash))
		return;
	if (!dns_question_footer_is_ipv4_a(&question))
		return;

	query = dns_lookup_response_query(ifindex, server_ip, source_port, hdr.id,
						  qname_hash, &track_key);
	if (!query)
		return;

	now = bpf_ktime_get_ns();
	if (query->expires_at_ns <= now)
		goto delete_query;
	if (!dns_response_header_is_supported(&hdr, flags, &ancount))
		goto delete_query;

#pragma clang loop unroll(disable)
	for (i = 0; i < DNS_MAX_RESPONSE_ANSWERS; i++) {
		if (i >= ancount)
			break;
		if (!dns_process_response_answer(skb, &cursor, ifindex, query->flags))
			goto delete_query;
	}

delete_query:
	bpf_map_delete_elem(&dns_query_track, &track_key);
}

#endif /* __DNS_RESPONSE_H */
