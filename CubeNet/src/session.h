// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2025 Cube Authors */
#ifndef __SESSION_H
#define __SESSION_H

#include <vmlinux.h>
#include "cubevs.h"
#include "map.h"

/* Lazy refresh threshold: 1 second in nanoseconds */
#define SESSION_REFRESH_INTERVAL_NS (1000 * 1000 * 1000UL)

/**
 * session_lazy_refresh - refresh session access time if stale
 * @sess:   pointer to the NAT session
 * @now_ns: current monotonic time in nanoseconds
 */
static __always_inline void session_lazy_refresh(struct nat_session *sess, __u64 now_ns)
{
	if (now_ns - sess->access_time > SESSION_REFRESH_INTERVAL_NS)
		sess->access_time = now_ns;
}

/**
 * session_mark_replied - transition simple UNREPLIED -> REPLIED state
 * @dir:             IP_CT_DIR_ORIGINAL or IP_CT_DIR_REPLY
 * @sess:            pointer to the NAT session
 * @unreplied_state: the protocol-specific UNREPLIED state value
 * @replied_state:   the protocol-specific REPLIED state value
 */
static __always_inline void session_mark_replied(enum ip_conntrack_dir dir,
						 struct nat_session *sess,
						 __u8 unreplied_state,
						 __u8 replied_state)
{
	if (dir == IP_CT_DIR_REPLY && sess->state == unreplied_state)
		sess->state = replied_state;
}

/**
 * session_policy_allowed - check egress network policy for a candidate flow
 * @vm_ifindex: TAP ifindex of the originating MVM (policy key)
 * @daddr:      destination IP address in network byte order
 *
 * Priority: allow_out_v2 > deny_out > default allow.
 *
 *   1. If allow_out_v2 has an inner map for this ifindex and daddr matches
 *      a non-expired entry, the flow is explicitly allowed.
 *   2. If deny_out has an inner map for this ifindex and daddr matches,
 *      the flow is denied.
 *   3. Otherwise the flow is allowed.
 *
 * Traffic to mvm_gateway_ip is internal (destined for cube-dev) and always
 * allowed regardless of policy.
 */
static __always_inline bool session_policy_allowed(__u32 vm_ifindex, __u32 daddr)
{
	struct lpm_key key = { .prefixlen = 32, .ip = daddr };
	struct net_policy_value_v2 *value;
	void *inner_map;

	if (daddr == mvm_gateway_ip)
		return true;

	inner_map = bpf_map_lookup_elem(&allow_out_v2, &vm_ifindex);
	if (inner_map) {
		value = bpf_map_lookup_elem(inner_map, &key);
		if (value && (value->expires_at_ns == 0 ||
			      value->expires_at_ns > bpf_ktime_get_ns()))
			return true;
	}

	inner_map = bpf_map_lookup_elem(&deny_out, &vm_ifindex);
	if (inner_map && bpf_map_lookup_elem(inner_map, &key))
		return false;

	return true;
}

/**
 * create_nat_session - create egress session with rollback on failure
 * @skb:           packet skb, used to signal deny reason via skb->cb[]
 * @ekey:          egress session key
 * @now_ns:        current monotonic time
 * @vm_ifindex:    TAP ifindex of the originating MVM
 * @snat_ip:       selected SNAT IP entry
 * @snat_port:     selected SNAT port/identifier in network byte order
 * @initial_state: protocol-specific initial conntrack state
 *
 * Enforces egress network policy before creating the session. On policy
 * deny, stamps skb->cb with NAT_CB_DENIED_BY_POLICY so the caller can
 * distinguish "deny" from "resource exhaustion" (e.g. TCP callers use it
 * to trigger tcp_reply_reset).
 *
 * Returns true on success, false otherwise (ingress session cleaned up).
 */
static __always_inline bool create_nat_session(struct __sk_buff *skb,
					       struct session_key *ekey,
					       __u64 now_ns, __u32 vm_ifindex,
					       struct snat_ip *snat_ip, __u16 snat_port,
					       __u8 initial_state)
{
	struct nat_session sess = {};
	struct session_key ikey = {};
	long err;

	ikey.src_ip = ekey->dst_ip;
	ikey.dst_ip = snat_ip->ip;
	ikey.src_port = ekey->dst_port;
	ikey.dst_port = snat_port;
	ikey.version = 0;
	ikey.protocol = ekey->protocol;

	/* Clear the status word so callers only see a fresh value written by
	 * this invocation. skb->cb[] can carry state from earlier tc filters
	 * in the chain, so we cannot assume it starts at zero.
	 */
	nat_cb_set(skb, NAT_CB_OK);

	if (!session_policy_allowed(vm_ifindex, ekey->dst_ip)) {
		nat_cb_set(skb, NAT_CB_DENIED_BY_POLICY);
		/* release the ingress slot reserved in pick_snat_ip_port */
		bpf_map_delete_elem(&ingress_sessions, &ikey);
		return false;
	}

	sess.access_time = now_ns;
	sess.node_ifindex = snat_ip->ifindex;
	sess.node_ip = snat_ip->ip;
	sess.vm_ifindex = vm_ifindex;
	sess.vm_ip = ekey->src_ip;
	sess.node_port = snat_port;
	sess.vm_port = ekey->src_port;
	sess.state = initial_state;
	err = bpf_map_update_elem(&egress_sessions, ekey, &sess, BPF_NOEXIST);
	if (err) {
		/* on failure, clean up the ingress slot we reserved earlier */
		bpf_map_delete_elem(&ingress_sessions, &ikey);
		return false;
	}

	return true;
}

#endif /* __SESSION_H */
