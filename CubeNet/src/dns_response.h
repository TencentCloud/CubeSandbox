// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#ifndef __DNS_RESPONSE_H
#define __DNS_RESPONSE_H

#include "dns_parser.h"
#include "map.h"

/* Inline twin of the dns_parser.h QNAME hash.
 *
 * The query pipeline calls the __noinline original from a program that has
 * other bpf-to-bpf calls — those can sit in their own verifier frames. The
 * response path, however, runs from a SEC("tc") tail-called program that must
 * contain zero bpf-to-bpf calls so it is allowed to bpf_tail_call into the UDP
 * NAT finish program on kernel 5.4. We keep a duplicate __always_inline copy
 * here so we can have it both ways without breaking the query path.
 */
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

/* Parse a DNS reply far enough to find the pending query it answers, retire
 * that query, and report whether the reply must be handed to user space for
 * learning.
 *
 * Returns true when the reply answers a query we tracked and is small enough
 * to upload; the caller then owns delivering the frame (user space re-injects
 * it after the allow_out_v3 write, so the sandbox cannot observe a resolved
 * address before it is permitted). Everything else — untracked, malformed,
 * non-A, oversized, learning disabled — returns false and takes the ordinary
 * reverse-NAT path, so an unrelated DNS reply is never delayed.
 *
 * Marked __always_inline (and using the *_inline QNAME helper above) so the
 * calling SEC("tc") program contains no bpf-to-bpf calls and can issue
 * bpf_tail_call on kernel 5.4.
 */
static __always_inline bool dns_response_parse_and_track(struct __sk_buff *skb, __u32 dns_off,
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

	if (!dns_response_learning_enabled(ifindex))
		return false;

	if (!dns_read_response_header(skb, dns_off, &hdr, &flags))
		return false;

	if (bpf_ntohs(hdr.qdcount) != 1)
		return false;
	if (!dns_hash_qname_inline(skb, &cursor, &question, &qname_hash))
		return false;
	if (!dns_question_footer_is_ipv4_a(&question))
		return false;

	query = dns_lookup_response_query(ifindex, server_ip, source_port, hdr.id,
					  qname_hash, &track_key);
	if (!query)
		return false;

	/* From here on the query is spent whatever we decide. This keeps the
	 * pre-existing behaviour of the handler this replaced, which retired the
	 * entry on every one of the paths below (its `delete_query` label): a
	 * pending query is one-shot, and leaving it behind would let a second
	 * reply carrying the same DNS id be learned from.
	 */
	bpf_map_delete_elem(&dns_query_track, &track_key);

	now = bpf_ktime_get_ns();
	if (query->expires_at_ns <= now)
		return false;
	if (!dns_response_header_is_supported(&hdr, flags, &ancount))
		return false;
	/* Oversized replies stay on the datapath rather than being truncated
	 * into an unparseable upload; see DNS_EVENT_MAX_FRAME.
	 */
	if (skb->len > DNS_EVENT_MAX_FRAME)
		return false;

	return true;
}

/* Hand the (already reverse-NATed) frame to user space on dns_events.
 *
 * The frame is uploaded verbatim after NAT, so what user space injects onto the
 * TAP is byte-for-byte what bpf_redirect() would have delivered. The prefix
 * carries the ifindex because the post-NAT destination is mvm_inner_ip, a
 * node-wide constant that cannot identify the sandbox.
 *
 * The data argument only carries the prefix; the packet rides along because the
 * upper 32 bits of flags ask bpf_perf_event_output to append that many bytes of
 * packet data to the sample. A record is therefore [prefix][frame], and
 * frame_len is what bounds the frame — perf rounds the sample up to 8-byte
 * alignment, so the record can carry a few bytes of padding past its end.
 */
static __always_inline void dns_forward_response_to_user(struct __sk_buff *skb, __u32 ifindex)
{
	struct dns_event_prefix prefix = {};
	__u32 frame_len = skb->len;
	__u64 flags;

	if (frame_len > DNS_EVENT_MAX_FRAME)
		frame_len = DNS_EVENT_MAX_FRAME;
	prefix.frame_len = (__u16)frame_len;
	prefix.ifindex = ifindex;

	flags = BPF_F_CURRENT_CPU | ((__u64)frame_len << 32);
	bpf_perf_event_output(skb, &dns_events, flags, &prefix, sizeof(prefix));
}

#endif /* __DNS_RESPONSE_H */
