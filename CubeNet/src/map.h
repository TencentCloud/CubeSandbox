// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2022 Cube Authors */
#ifndef __MAP_H
#define __MAP_H

#include "cubevs.h"

/* MVM IP to ifindex (managed by upper layer)
 *
 * key:   IP address in network byte order assigned to MVM
 * value: ifindex of the TAP device assigned to MVM
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__type(value, __u32);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} mvmip_to_ifindex SEC(".maps");

/* ifindex to MVM metadata (managed by upper layer), we use IP/tunnel group ID only
 *
 * key:   ifindex of the TAP device assigned to MVM
 * value: tunnel group ID, ID and IP address in network byte order assigned to MVM
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__type(value, struct mvm_meta);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} ifindex_to_mvmmeta SEC(".maps");

/* host port (for remote access from CubeProxy) to MVM port mapping
 *
 * key:   host port
 * value: MVM ifindex + MVM listen port
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_PORTS);
	__type(key, __u16);
	__type(value, struct mvm_port);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} remote_port_mapping SEC(".maps");

/* MVM port (for NAT) to host port mapping
 *
 * key:   MVM ifindex + MVM listen port
 * value: host port
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_PORTS);
	__type(key, struct mvm_port);
	__type(value, __u16);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} local_port_mapping SEC(".maps");

/* Egress session table
 *
 * key:   5-tuple for egress packet
 * value: session
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_SESSIONS);
	__type(key, struct session_key);
	__type(value, struct nat_session);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} egress_sessions SEC(".maps");

/* Ingress session table
 *
 * key:   5-tuple for ingress packet
 * value: used to construct lookup key for egress_sessions
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_SESSIONS);
	__type(key, struct session_key);
	__type(value, struct ingress_session);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} ingress_sessions SEC(".maps");

/* SNAT IP list
 *
 * key:   index for hash(MVM_IP)
 * value: SNAT IP and its ifindex, max_port
 */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, MAX_SNAT_IPS);
	__type(key, __u32);
	__type(value, struct snat_ip);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} snat_iplist SEC(".maps");

/* Egress allow list v2 (hash of maps)
 *
 * key:   ifindex of the TAP device
 * value: fd of inner LPM trie map (destination IP allow list)
 *
 * Inner values use net_policy_value_v2. A zero expires_at_ns means a static
 * allow entry; a non-zero expires_at_ns means a temporary DNS-learned entry.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__array(values, struct {
		__uint(type, BPF_MAP_TYPE_LPM_TRIE);
		__uint(max_entries, MAX_IP_RULE_ENTRIES);
		__type(key, struct lpm_key);
		__type(value, struct net_policy_value_v2);
		__uint(map_flags, BPF_F_NO_PREALLOC);
	});
} allow_out_v2 SEC(".maps");

/* Egress deny list (hash of maps)
 *
 * key:   ifindex of the TAP device
 * value: fd of inner LPM trie map (destination IP deny list)
 *
 * If the inner map exists for a given ifindex and the destination IP
 * matches an entry, the packet is denied.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__array(values, struct {
		__uint(type, BPF_MAP_TYPE_LPM_TRIE);
		__uint(max_entries, MAX_IP_RULE_ENTRIES);
		__type(key, struct lpm_key);
		__type(value, __u32);
		__uint(map_flags, BPF_F_NO_PREALLOC);
	});
} deny_out SEC(".maps");

/* DNS policy rules (hash of maps)
 *
 * key:   ifindex of the TAP device
 * value: fd of inner LPM trie map for this sandbox's DNS policy rules
 *
 * Inner keys are reversed lower-case domain name prefixes. DNS policy mode is
 * stored in ifindex_to_mvmmeta, while dns_allow stores only domain rules.
 * Exact rule "qq.com" is encoded as "moc.qq\0" with the trailing NUL included
 * in prefixlen. Wildcard rule "*.qq.com" is encoded as "moc.qq." without NUL,
 * so only subdomains such as "a.qq.com" can match it.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__array(values, struct {
		__uint(type, BPF_MAP_TYPE_LPM_TRIE);
		__uint(max_entries, MAX_DOMAIN_RULE_ENTRIES);
		__type(key, struct dns_allow_key);
		__type(value, struct dns_allow_value);
		__uint(map_flags, BPF_F_NO_PREALLOC);
	});
} dns_allow SEC(".maps");

/* Pending DNS queries waiting for responses.
 *
 * key:   sandbox ifindex + DNS server IP + sandbox UDP source port + DNS id
 *        + raw DNS QNAME hash
 * value: L7 flags inherited from dns_allow and pending expiration time
 */
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_DNS_QUERY_TRACK_ENTRIES);
	__type(key, struct dns_query_track_key);
	__type(value, struct dns_query_track_value);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} dns_query_track SEC(".maps");

/* Per-CPU scratch space for DNS query parsing.
 *
 * Store parsed QNAMEs directly as LPM keys so they stay out of caller stack.
 */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct dns_allow_key);
} dns_query_scratch SEC(".maps");

/* Tail-call state for chunked DNS query parsing. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct dns_query_state);
} dns_query_state SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct dns_response_state);
} dns_response_state SEC(".maps");

/* Tail-call jump table for the DNS parser pipeline. */
struct {
	__uint(type, BPF_MAP_TYPE_PROG_ARRAY);
	/* Reserve extra slots for future DNS parser pipeline stages. */
	__uint(max_entries, 16);
	__type(key, __u32);
	__type(value, __u32);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} dns_tail_calls SEC(".maps");

#endif /* __MAP_H */
