// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2025 Cube Authors */
#ifndef __SESSION_H
#define __SESSION_H

#include <vmlinux.h>
#include "cubevs.h"
#include "map.h"

enum packet_class {
	SNAT_PACKET = 0,
	L7PROXY_PACKET,
};

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
 * lookup_session - resolve a reverse-flow key to its egress NAT session
 * @ikey: ingress/reply-direction session key
 *
 * ingress_sessions stores the sandbox identity needed to reconstruct the
 * original-direction egress_sessions key. Both keys use network-byte-order
 * addresses and ports. Returns the live map value, or NULL when either side
 * of the session pair is missing.
 */
static __always_inline struct nat_session *lookup_session(const struct session_key *ikey)
{
	struct ingress_session *isess;
	struct session_key ekey = {};

	isess = bpf_map_lookup_elem(&ingress_sessions, ikey);
	if (!isess)
		return NULL;

	ekey.src_ip = isess->vm_ip;
	ekey.dst_ip = ikey->src_ip;
	ekey.src_port = isess->vm_port;
	ekey.dst_port = ikey->src_port;
	ekey.version = isess->version;
	ekey.protocol = ikey->protocol;

	return bpf_map_lookup_elem(&egress_sessions, &ekey);
}

/* Unified egress policy verdict for a candidate flow.
 *
 * Replaces the former pair l7_scheme_for_flow() (allow_out_v3 /48 L7
 * lookup) and session_policy_allowed() (allow_out_v3 /32 + deny_out /32).
 * Callers classify once and act on the verdict:
 *   FLOW_REJECT  - deny  (drop / RST, never create a session)
 *   FLOW_SNAT   - plain SNAT egress is allowed
 *   FLOW_HTTP   - L7 proxy over HTTP is required
 *   FLOW_HTTPS  - L7 proxy over HTTPS is required
 */
enum flow_verdict {
	FLOW_REJECT = 0,
	FLOW_SNAT,
	FLOW_HTTP,
	FLOW_HTTPS,
};

/**
 * classify_egress_flow - single egress policy decision for a candidate flow
 * @ifindex: TAP ifindex of the originating MVM (policy key)
 * @daddr:   destination IP address in network byte order
 * @dport:   destination port in network byte order (0 for port-agnostic)
 *
 * Priority: allow_out_v3 > deny_out > default allow.
 *
 *   1. Look up (daddr, dport)/48 in allow_out_v3. LPM automatically falls
 *      back to a matching /32 or subnet entry. A non-expired L7_REQUIRED
 *      entry returns FLOW_HTTP / FLOW_HTTPS; any other non-expired allow
 *      entry returns FLOW_SNAT.
 *   2. Else if deny_out matches a /32 (or wider) entry, the flow is
 *      rejected (FLOW_REJECT).
 *   3. Otherwise the flow is allowed via SNAT (default allow).
 *
 * Traffic to mvm_gateway_ip is internal (destined for cube-dev) and always
 * allowed regardless of policy.
 *
 * The L7 (/48) lookup is performed FIRST, before the deny check. This is
 * the key fix for DNS-learned L7 entries: those are stored as (ip, port)/48
 * in allow_out_v3, but the old session_policy_allowed() used a hardcoded
 * /32 key and could never match them, so an already-authorized L7 flow fell
 * through to deny_out (e.g. 0.0.0.0/0) and was silently dropped.
 */
static __always_inline __u8 classify_egress_flow(__u32 ifindex, __u32 daddr,
						 __u16 dport)
{
	struct lpm_key_v3 key = {};
	struct net_policy_value_v3 *value;
	void *inner_map;
	__u64 now = bpf_ktime_get_ns();

	/* internal traffic destined for the MVM gateway is always allowed */
	if (daddr == mvm_gateway_ip)
		return FLOW_SNAT;

	/* 1) Allow: the /48 lookup resolves an exact L7 rule or falls back
	 * to a plain /32/subnet rule in the same LPM trie.
	 */
	inner_map = bpf_map_lookup_elem(&allow_out_v3, &ifindex);
	if (inner_map) {
		key.prefixlen = 48;
		key.ip = daddr;
		key.port = dport;
		value = bpf_map_lookup_elem(inner_map, &key);
		if (value && (value->expires_at_ns == 0 ||
			      value->expires_at_ns > now)) {
			if (value->flags & NET_POLICY_FLAG_L7_REQUIRED) {
				if (value->scheme == L7_SCHEME_HTTP)
					return FLOW_HTTP;
				if (value->scheme == L7_SCHEME_HTTPS)
					return FLOW_HTTPS;
				/* L7 required but scheme unknown: fail closed rather
				 * than silently downgrading to plain SNAT, which would
				 * bypass the TPROXY intercept the rule asked for. A
				 * well-formed entry always carries a scheme (userspace
				 * populate and DNS-learn both set it), so reaching this
				 * branch means a corrupt or half-written map value.
				 */
				return FLOW_REJECT;
			}
			return FLOW_SNAT;
		}
	}

	/* 2) Deny: /32 (or wider) lookup in deny_out. deny_out inner maps are
	 * keyed by the 8-byte struct lpm_key (see map.h), so use a dedicated key
	 * rather than reusing the 12-byte lpm_key_v3 above — passing a v3 key to
	 * an 8-byte-key map only works because the kernel reads map->key_size
	 * bytes, which is an accident of struct layout, not a contract.
	 */
	inner_map = bpf_map_lookup_elem(&deny_out, &ifindex);
	if (inner_map) {
		struct lpm_key deny_key = { .prefixlen = 32, .ip = daddr };
		if (bpf_map_lookup_elem(inner_map, &deny_key))
			return FLOW_REJECT;
	}

	/* 3) Default: allow via SNAT */
	return FLOW_SNAT;
}

/**
 * session_verdict - the flow verdict this session was created under
 * @sess: pointer to the NAT session
 *
 * Reconstructed from packet_class + l7_scheme so a re-check can compare the
 * fresh verdict against the cached one. An L7 session with an unknown scheme
 * is a corrupt value; report REJECT so it fails closed, matching
 * classify_egress_flow().
 */
static __always_inline __u8 session_verdict(const struct nat_session *sess)
{
	if (sess->packet_class != L7PROXY_PACKET)
		return FLOW_SNAT;
	if (sess->l7_scheme == L7_SCHEME_HTTP)
		return FLOW_HTTP;
	if (sess->l7_scheme == L7_SCHEME_HTTPS)
		return FLOW_HTTPS;
	return FLOW_REJECT;
}

/**
 * session_policy_revoked - re-evaluate an established flow after a policy update
 * @sess:      pointer to the NAT session
 * @policy_version: policy generation this packet is judged under, read once by
 *                  the caller before any classification
 * @ifindex:   TAP ifindex of the originating MVM
 * @daddr:     destination IP in network byte order
 * @dport:     destination port in network byte order (0 for ICMP)
 *
 * Returns true when this flow may no longer carry traffic. Callers retire the
 * session pair with del_session() and reject the packet: TCP answers with an RST
 * like every other unreachable packet here, so the guest fails fast instead of
 * stalling on retransmits; UDP and ICMP have nothing to reset and simply drop.
 *
 * Deleting rather than flagging keeps the retirement self-enforcing. A later
 * non-SYN packet on the same tuple finds no session and is reset, so a revoked
 * flow cannot resume even if a subsequent update re-allows the destination,
 * while a SYN legitimately opens a fresh connection under the current policy.
 *
 * A verdict *change* counts as revocation, not just FLOW_REJECT. Once a flow
 * must switch between plain SNAT and L7 interception there is no way to migrate
 * it -- the two paths disagree on both the reply tuple and who terminates the
 * TCP connection -- so the flow is retired and the client reconnects.
 *
 * Called before update_session(): there is no point advancing the conntrack
 * state of a flow that is about to be deleted.
 *
 * Takes the generation by value rather than re-reading mvm_meta, because
 * userspace can bump it while classify_egress_flow() runs. Re-reading would
 * stamp the flow with a generation newer than the maps it was judged against,
 * and since the stamp is what schedules the next re-check, the flow would never
 * be judged under that generation at all -- silently outliving its revocation.
 * A generation only ever moves forward, so a stale value costs one extra
 * re-check, which is the direction we want to err in.
 */
static __always_inline bool session_policy_revoked(struct nat_session *sess,
						   __u32 policy_version,
						   __u32 ifindex, __u32 daddr,
						   __u16 dport)
{
	__u8 verdict;

	if (sess->policy_version == policy_version)
		return false;

	verdict = classify_egress_flow(ifindex, daddr, dport);
	if (verdict == FLOW_REJECT || verdict != session_verdict(sess))
		return true;
	sess->policy_version = policy_version;
	return false;
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
 * @packet_class:  SNAT_PACKET or L7PROXY_PACKET
 * @l7_scheme:     L7_SCHEME_*; NONE for non-L7 sessions
 * @policy_version: generation the caller classified this flow under
 *
 * packet_class and l7_scheme are initialized in the stack value before the
 * single BPF_NOEXIST insertion. This prevents another CPU from observing a
 * partially classified session.
 *
 * Egress network policy is NOT enforced here. Callers must classify the
 * flow with classify_egress_flow() first and reject denied flows (stamping
 * skb->cb with NAT_CB_DENIED_BY_POLICY) before reaching this point. This
 * keeps the policy verdict a single decision taken once per new flow.
 *
 * policy_version comes from the caller for the same reason: it has to be the
 * generation read *before* that classification. Reading mvm_meta here would let
 * a concurrent bump stamp the new flow as already judged under a generation it
 * never saw, and the stamp is what schedules its next re-check.
 *
 * Returns true on success, false otherwise (ingress session cleaned up).
 */
static __always_inline bool create_nat_session(struct __sk_buff *skb,
					       struct session_key *ekey,
					       __u64 now_ns, __u32 vm_ifindex,
					       struct snat_ip *snat_ip, __u16 snat_port,
					       __u8 initial_state, __u8 packet_class,
					       __u8 l7_scheme, __u32 policy_version)
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

	sess.access_time = now_ns;
	sess.node_ifindex = snat_ip->ifindex;
	sess.node_ip = snat_ip->ip;
	sess.vm_ifindex = vm_ifindex;
	sess.vm_ip = ekey->src_ip;
	sess.node_port = snat_port;
	sess.vm_port = ekey->src_port;
	sess.state = initial_state;
	sess.packet_class = packet_class;
	sess.l7_scheme = l7_scheme;
	/* Stamp the generation this verdict was taken under; comparing it against
	 * mvm_meta is what schedules this flow's next re-check.
	 */
	sess.policy_version = policy_version;
	err = bpf_map_update_elem(&egress_sessions, ekey, &sess, BPF_NOEXIST);
	if (err) {
		/* on failure, clean up the ingress slot we reserved earlier */
		bpf_map_delete_elem(&ingress_sessions, &ikey);
		return false;
	}

	return true;
}

#endif /* __SESSION_H */
