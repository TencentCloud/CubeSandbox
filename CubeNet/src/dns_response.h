// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#ifndef __DNS_RESPONSE_H
#define __DNS_RESPONSE_H

#include "dns_parser.h"
#include "map.h"

/* Check whether DNS response learning is enabled for this sandbox. */
static __always_inline bool dns_response_learning_enabled(__u32 ifindex)
{
	struct mvm_meta *mvm_meta;

	mvm_meta = bpf_map_lookup_elem(&ifindex_to_mvmmeta, &ifindex);
	return dns_policy_learning_enabled(mvm_meta);
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

/* Parse one answer RR and learn its IPv4 address when it is an A record. */
static __noinline bool dns_process_response_answer(struct __sk_buff *skb,
						   __u32 *cursor, __u32 ifindex,
						   __u8 flags)
{
	struct dns_rr_header rr;
	__u16 rdlength;
	__u32 ip;
	__u32 ttl;

	if (!dns_skip_name(skb, cursor))
		return false;
	if (bpf_skb_load_bytes(skb, *cursor, &rr, sizeof(rr)))
		return false;
	*cursor += sizeof(rr);

	rdlength = bpf_ntohs(rr.rdlength);
	if (dns_response_record_is_ipv4_a(&rr, rdlength)) {
		if (bpf_skb_load_bytes(skb, *cursor, &ip, sizeof(ip)))
			return false;
		ttl = bpf_ntohl(rr.ttl);
		dns_learn_response_ip(ifindex, ip, ttl, flags);
	}

	/* Keep cursor advancement bounded even for unsupported RR types. */
	if (rdlength > DNS_MAX_RDATA_LEN)
		return false;

	*cursor += rdlength;
	return true;
}

/* Lookup the pending DNS query that authorizes this response. */
static __always_inline struct dns_query_track_value *dns_lookup_response_query(__u32 ifindex,
									 __u32 server_ip,
									 __u16 source_port,
									 __be16 dns_id,
									 __u64 qname_hash,
									 struct dns_query_track_key *track_key)
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
 */
static __noinline void dns_handle_response(struct __sk_buff *skb, __u32 dns_off,
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
	if (!dns_hash_qname(skb, &cursor, &question, &qname_hash))
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
