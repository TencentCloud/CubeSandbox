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

/* Direct-egress on-link neighbor trigger/cache.
 *
 * key:   destination IPv4 address in packet-byte layout
 * value: struct direct_neighbor — a TTL-bounded cache of the last
 *        bpf_fib_lookup() result (MAC + valid_until + fib_ok + last_used) plus
 *        the userspace scanner's trigger scheduling fields (step +
 *        next_attempt/next_refresh). The MAC always comes from fib; this map
 *        only caches it briefly and drives scanner scheduling.
 */
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__type(value, struct direct_neighbor);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} direct_neigh SEC(".maps");

/* Egress allow list v3 (hash of maps)
 *
 * key:   ifindex of the TAP device
 * value: fd of inner LPM trie map (destination ip[:port] allow list)
 *
 * Inner keys use lpm_key_v3 so a single longest-prefix lookup resolves
 * exact (ip, port) (prefixlen 48), ip-only / any-port (prefixlen 32),
 * or ip/mask subnet (prefixlen < 32) rules. Inner values use
 * net_policy_value_v3, which marks the L7 scheme directly (the port is
 * now part of the key, so no per-packet (port, scheme) array scan).
 * A zero expires_at_ns means a static entry; a non-zero expires_at_ns
 * means a temporary DNS-learned entry.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__array(values, struct {
		__uint(type, BPF_MAP_TYPE_LPM_TRIE);
		__uint(max_entries, MAX_IP_RULE_ENTRIES);
		__type(key, struct lpm_key_v3);
		__type(value, struct net_policy_value_v3);
		__uint(map_flags, BPF_F_NO_PREALLOC);
	});
} allow_out_v3 SEC(".maps");

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
 * stored in ifindex_to_mvmmeta, while dns_allow_v2 stores only domain rules.
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
} dns_allow_v2 SEC(".maps");

/* Pending DNS queries waiting for responses.
 *
 * key:   sandbox ifindex + DNS server IP + sandbox UDP source port + DNS id
 *        + raw DNS QNAME hash
 * value: L7 flags inherited from dns_allow_v2 and pending expiration time
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

/* Per-sandbox rate limit on DNS query tracking.
 *
 * key:   TAP ifindex
 * value: fixed-window counter guarded by a spin lock
 *
 * A tracked query is what authorizes a response to be uploaded to user space,
 * where learning costs a full desired-state recomputation per response. This
 * caps how fast a sandbox can drive that path. A missing entry means "no
 * limit", so a sandbox whose entry has not been installed yet still learns.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, __u32);
	__type(value, struct dns_track_rl_state);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} dns_track_rl SEC(".maps");

/* DNS responses handed to user space for learning.
 *
 * Each record is a struct dns_event_prefix followed by the post-NAT Ethernet
 * frame verbatim. max_entries is left at 0; both libbpf and cilium/ebpf fix it
 * up to the number of possible CPUs at load time.
 *
 * The pin is removed on every Init (see miscs.go): a perf event array's slots
 * hold references to the ring buffers of whichever process installed them, so
 * reusing a stale pin would send output to rings nobody reads.
 */
struct {
	__uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
	__uint(max_entries, 0);
	__type(key, __u32);
	__type(value, __u32);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} dns_events SEC(".maps");

#endif /* __MAP_H */
