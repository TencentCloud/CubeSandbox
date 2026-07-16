// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2026 Cube Authors */
#ifndef __DNS_QUERY_H
#define __DNS_QUERY_H

#include "dns_parser.h"
#include "l2l3.h"
#include "map.h"
#include "skb.h"

#ifndef barrier_var
#define barrier_var(var) asm volatile("" : "+r"(var))
#endif

/* Internal query hook return value: keep the caller's normal packet path. */
#define CUBE_DNS_PASS	-1

#define DNS_TAIL_CALL_PARSE		0
#define DNS_TAIL_CALL_REVERSE		1
#define DNS_TAIL_CALL_FINISH		2
#define DNS_TAIL_CALL_RESPONSE		3
#define DNS_TAIL_CALL_RESPONSE_FINISH	4

/* Query name parsing and reversing are split into fixed-size chunks so each
 * tail-called program stays small and verifier-friendly. With the current
 * DNS_MAX_NAME_LEN=256 and DNS_PARSE_CHUNK_SIZE=64, a maximum-length name needs
 * at most four parse chunks and four reverse chunks. DNS_MAX_NAME_LEN is a
 * power of two, which lets chunk helpers mask dynamic indexes safely.
 */
#define DNS_PARSE_CHUNK_SIZE	64

/* Chunked QNAME parsing -------------------------------------------------- */

/* Parse one bounded chunk of the DNS QNAME into a lower-case dotted name. */
static __always_inline void dns_parse_query_name_chunk(struct __sk_buff *skb,
						       struct dns_query_state *state)
{
	int i;

#pragma clang loop unroll(disable)
	for (i = 0; i < DNS_PARSE_CHUNK_SIZE; i++) {
		__u8 c;
		__u32 dst;

		if (state->failed || state->done)
			return;
		if (state->dotted_len >= DNS_MAX_NAME_LEN) {
			state->failed = true;
			return;
		}

		if (bpf_skb_load_bytes(skb, state->cursor, &c, sizeof(c))) {
			state->failed = true;
			return;
		}
		state->cursor++;

		if (state->label_remaining == 0) {
			/* At a label boundary, only plain DNS labels are accepted. */
			if (c == 0) {
				state->done = true;
				return;
			}
			if ((c & DNS_COMPRESS_PTR_MASK) != 0 || c > DNS_MAX_LABEL_LEN) {
				state->failed = true;
				return;
			}
			if (state->dotted_len != 0) {
				if (state->dotted_len >= DNS_MAX_NAME_LEN) {
					state->failed = true;
					return;
				}
				dst = state->dotted_len & (DNS_MAX_NAME_LEN - 1);
				state->name[dst] = '.';
				state->dotted_len++;
			}
			if (state->dotted_len + c >= DNS_MAX_NAME_LEN) {
				state->failed = true;
				return;
			}
			state->label_remaining = c;
			continue;
		}

		dst = state->dotted_len & (DNS_MAX_NAME_LEN - 1);
		state->name[dst] = dns_qname_lower(c);
		state->dotted_len++;
		state->label_remaining--;
	}
}

/* Reverse one bounded chunk so the parsed name can be used as an LPM key. */
static __always_inline bool dns_reverse_query_name_chunk(struct dns_query_state *state,
							 struct dns_allow_key *question)
{
	int i;

	if (state->dotted_len == 0 || state->dotted_len >= DNS_MAX_NAME_LEN)
		return false;
	question->prefixlen = (state->dotted_len + 1) * 8;

#pragma clang loop unroll(disable)
	for (i = 0; i < DNS_PARSE_CHUNK_SIZE; i++) {
		__u32 src;
		__u32 dst;

		if (state->reverse_pos >= state->dotted_len) {
			dst = state->dotted_len & (DNS_MAX_NAME_LEN - 1);
			question->name[dst] = 0;
			return true;
		}

		/* Mask dynamic indexes after barrier_var to keep verifier bounds tight. */
		src = state->dotted_len - 1 - state->reverse_pos;
		barrier_var(src);
		src &= DNS_MAX_NAME_LEN - 1;
		dst = state->reverse_pos & (DNS_MAX_NAME_LEN - 1);
		question->name[dst] = state->name[src];
		state->reverse_pos++;
	}

	return false;
}

/* Allow-list matching ---------------------------------------------------- */

/* Match the reversed DNS question against the per-sandbox DNS allow trie. */
static __noinline struct dns_allow_value *dns_allow_match_value(void *inner_map,
								const struct dns_allow_key *question)
{
	struct dns_allow_value *value;
	__u32 name_len = question->prefixlen >> 3;

	if (question->prefixlen == 0 || question->prefixlen > DNS_MAX_NAME_LEN * 8 ||
	    (question->prefixlen & 7) != 0)
		return NULL;

	value = bpf_map_lookup_elem(inner_map, question);
	if (!value || value->name_len == 0 || value->name_len > name_len)
		return NULL;

	return value;
}

/* Parse the single DNS question and report whether it is an IN A query without
 * building an allow-list key. Non-A or malformed questions should bypass DNS
 * domain filtering entirely.
 */
static __always_inline bool dns_parse_query_question(struct __sk_buff *skb,
						     __u32 dns_off, bool *is_ipv4_a)
{
	struct dns_question_footer question;
	__u32 cursor = dns_off + DNS_HDR_LEN;

	*is_ipv4_a = false;
	if (!dns_skip_name(skb, &cursor))
		return false;
	if (!dns_read_question_footer(skb, cursor, &question))
		return false;

	*is_ipv4_a = dns_question_footer_is_ipv4_a(&question);
	return true;
}

/* Only well-formed standard IN A queries enter DNS domain filtering. */
static __always_inline bool dns_query_should_filter_ipv4_a(struct __sk_buff *skb,
							   __u32 dns_off,
							   const struct dns_wire_header *hdr,
							   __u16 flags)
{
	bool is_ipv4_a = false;

	if (!dns_query_header_is_supported(hdr, flags))
		return false;
	if (!dns_parse_query_question(skb, dns_off, &is_ipv4_a))
		return false;

	return is_ipv4_a;
}

/* Track an allowed IPv4 A query so the response can inherit its L7 flags. */
static __always_inline void dns_track_allowed_query(struct __sk_buff *skb,
						    const struct dns_query_state *state,
						    __u8 flags, __u64 qname_hash)
{
	struct dns_query_track_key track_key = {};
	struct dns_query_track_value track_value = {};
	struct dns_wire_header hdr;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct udphdr *udp;

	if (!__pull_headers_udp(skb, &l2, &l3, &udp))
		return;
	if (bpf_skb_load_bytes(skb, state->dns_off, &hdr, sizeof(hdr)))
		return;

	track_key.ifindex = state->ifindex;
	track_key.server_ip = l3->daddr;
	track_key.source_port = udp->source;
	track_key.dns_id = hdr.id;
	track_key.qname_hash = qname_hash;
	track_value.flags = flags;
	track_value.expires_at_ns = bpf_ktime_get_ns() + DNS_QUERY_TRACK_TTL_NS;

	bpf_map_update_elem(&dns_query_track, &track_key, &track_value, BPF_ANY);
}

/* Query entry ------------------------------------------------------------ */

/* Reset parser state before the tail-call query pipeline starts. */
static __always_inline void dns_init_query_state(struct dns_query_state *state,
						 __u32 dns_off, __u32 ifindex,
						 __u16 flags)
{
	state->dns_off = dns_off;
	state->ifindex = ifindex;
	state->flags = flags;
	state->cursor = dns_off + DNS_HDR_LEN;
	state->label_remaining = 0;
	state->dotted_len = 0;
	state->reverse_pos = 0;
	state->failed = false;
	state->done = false;
}

/* Query hook for sandbox-originated UDP/53 traffic.
 *
 * Each sandbox owns one precreated DNS allow LPM trie via dns_allow[ifindex].
 * Callers only invoke this hook when per-sandbox metadata flags in
 * ifindex_to_mvmmeta enable DNS policy processing. The DNS allow trie stores
 * only reversed lower-case domain rules and their rule-specific flags.
 */
static __always_inline int dns_handle_query(struct __sk_buff *skb, __u32 dns_off,
					    __u32 ifindex)
{
	struct dns_query_state *state;
	struct dns_wire_header hdr;
	__u16 flags;
	__u32 key = 0;
	void *inner_map;

	if (!dns_read_query_header(skb, dns_off, &hdr, &flags))
		return CUBE_DNS_PASS;

	inner_map = bpf_map_lookup_elem(&dns_allow, &ifindex);
	if (!inner_map)
		return CUBE_DNS_PASS;

	if (!dns_query_should_filter_ipv4_a(skb, dns_off, &hdr, flags))
		return CUBE_DNS_PASS;

	state = bpf_map_lookup_elem(&dns_query_state, &key);
	if (!state)
		return CUBE_DNS_PASS;

	dns_init_query_state(state, dns_off, ifindex, flags);

	/* The tail-call programs parse, reverse, match, and then finish NAT. */
	bpf_tail_call(skb, &dns_tail_calls, DNS_TAIL_CALL_PARSE);
	return TC_ACT_SHOT;
}

#endif /* __DNS_QUERY_H */
