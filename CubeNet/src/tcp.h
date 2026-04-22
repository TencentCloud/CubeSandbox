// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2023 Cube Authors */
#ifndef __TCP_H
#define __TCP_H

#include <vmlinux.h>
#include "cubevs.h"
#include "session.h"

/* What TCP flags are set from RST/SYN/FIN/ACK. */
enum tcp_bit_set {
	TCP_SYN_SET,
	TCP_SYNACK_SET,
	TCP_FIN_SET,
	TCP_ACK_SET,
	TCP_RST_SET,
	TCP_NONE_SET,
};

#define TCP_CONNTRACK_SYN_SENT2	TCP_CONNTRACK_LISTEN

#define sNO TCP_CONNTRACK_NONE
#define sSS TCP_CONNTRACK_SYN_SENT
#define sSR TCP_CONNTRACK_SYN_RECV
#define sES TCP_CONNTRACK_ESTABLISHED
#define sFW TCP_CONNTRACK_FIN_WAIT
#define sCW TCP_CONNTRACK_CLOSE_WAIT
#define sLA TCP_CONNTRACK_LAST_ACK
#define sTW TCP_CONNTRACK_TIME_WAIT
#define sCL TCP_CONNTRACK_CLOSE
#define sS2 TCP_CONNTRACK_SYN_SENT2
#define sIV TCP_CONNTRACK_MAX
#define sIG TCP_CONNTRACK_IGNORE

/*
 * The TCP state transition table needs a few words...
 *
 * We are the man in the middle. All the packets go through us
 * but might get lost in transit to the destination.
 * It is assumed that the destinations can't receive segments
 * we haven't seen.
 *
 * The checked segment is in window, but our windows are *not*
 * equivalent with the ones of the sender/receiver. We always
 * try to guess the state of the current sender.
 *
 * The meaning of the states are:
 *
 * NONE:	initial state
 * SYN_SENT:	SYN-only packet seen
 * SYN_SENT2:	SYN-only packet seen from reply dir, simultaneous open
 * SYN_RECV:	SYN-ACK packet seen
 * ESTABLISHED:	ACK packet seen
 * FIN_WAIT:	FIN packet seen
 * CLOSE_WAIT:	ACK seen (after FIN)
 * LAST_ACK:	FIN seen (after FIN)
 * TIME_WAIT:	last ACK seen
 * CLOSE:	closed connection (RST)
 *
 * Packets marked as IGNORED (sIG):
 *	if they may be either invalid or valid
 *	and the receiver may send back a connection
 *	closing RST or a SYN/ACK.
 *
 * Packets marked as INVALID (sIV):
 *	if we regard them as truly invalid packets
 */
static const u8 tcp_conntracks[2][6][TCP_CONNTRACK_MAX] = {
	{
/* ORIGINAL */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*syn*/	   { sSS, sSS, sIG, sIG, sIG, sIG, sIG, sSS, sSS, sS2 },
/*
 *	sNO -> sSS	Initialize a new connection
 *	sSS -> sSS	Retransmitted SYN
 *	sS2 -> sS2	Late retransmitted SYN
 *	sSR -> sIG
 *	sES -> sIG	Error: SYNs in window outside the SYN_SENT state
 *			are errors. Receiver will reply with RST
 *			and close the connection.
 *			Or we are not in sync and hold a dead connection.
 *	sFW -> sIG
 *	sCW -> sIG
 *	sLA -> sIG
 *	sTW -> sSS	Reopened connection (RFC 1122).
 *	sCL -> sSS
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*synack*/ { sIV, sIV, sSR, sIV, sIV, sIV, sIV, sIV, sIV, sSR },
/*
 *	sNO -> sIV	Too late and no reason to do anything
 *	sSS -> sIV	Client can't send SYN and then SYN/ACK
 *	sS2 -> sSR	SYN/ACK sent to SYN2 in simultaneous open
 *	sSR -> sSR	Late retransmitted SYN/ACK in simultaneous open
 *	sES -> sIV	Invalid SYN/ACK packets sent by the client
 *	sFW -> sIV
 *	sCW -> sIV
 *	sLA -> sIV
 *	sTW -> sIV
 *	sCL -> sIV
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*fin*/    { sIV, sIV, sFW, sFW, sLA, sLA, sLA, sTW, sCL, sIV },
/*
 *	sNO -> sIV	Too late and no reason to do anything...
 *	sSS -> sIV	Client might not send FIN in this state:
 *			we enforce waiting for a SYN/ACK reply first.
 *	sS2 -> sIV
 *	sSR -> sFW	Close started.
 *	sES -> sFW
 *	sFW -> sLA	FIN seen in both directions, waiting for
 *			the last ACK.
 *			Might be a retransmitted FIN as well...
 *	sCW -> sLA
 *	sLA -> sLA	Retransmitted FIN. Remain in the same state.
 *	sTW -> sTW
 *	sCL -> sCL
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*ack*/	   { sES, sIV, sES, sES, sCW, sCW, sTW, sTW, sCL, sIV },
/*
 *	sNO -> sES	Assumed.
 *	sSS -> sIV	ACK is invalid: we haven't seen a SYN/ACK yet.
 *	sS2 -> sIV
 *	sSR -> sES	Established state is reached.
 *	sES -> sES	:-)
 *	sFW -> sCW	Normal close request answered by ACK.
 *	sCW -> sCW
 *	sLA -> sTW	Last ACK detected (RFC5961 challenged)
 *	sTW -> sTW	Retransmitted last ACK. Remain in the same state.
 *	sCL -> sCL
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*rst*/    { sIV, sCL, sCL, sCL, sCL, sCL, sCL, sCL, sCL, sCL },
/*none*/   { sIV, sIV, sIV, sIV, sIV, sIV, sIV, sIV, sIV, sIV }
	},
	{
/* REPLY */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*syn*/	   { sIV, sS2, sIV, sIV, sIV, sIV, sIV, sSS, sIV, sS2 },
/*
 *	sNO -> sIV	Never reached.
 *	sSS -> sS2	Simultaneous open
 *	sS2 -> sS2	Retransmitted simultaneous SYN
 *	sSR -> sIV	Invalid SYN packets sent by the server
 *	sES -> sIV
 *	sFW -> sIV
 *	sCW -> sIV
 *	sLA -> sIV
 *	sTW -> sSS	Reopened connection, but server may have switched role
 *	sCL -> sIV
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*synack*/ { sIV, sSR, sIG, sIG, sIG, sIG, sIG, sIG, sIG, sSR },
/*
 *	sSS -> sSR	Standard open.
 *	sS2 -> sSR	Simultaneous open
 *	sSR -> sIG	Retransmitted SYN/ACK, ignore it.
 *	sES -> sIG	Late retransmitted SYN/ACK?
 *	sFW -> sIG	Might be SYN/ACK answering ignored SYN
 *	sCW -> sIG
 *	sLA -> sIG
 *	sTW -> sIG
 *	sCL -> sIG
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*fin*/    { sIV, sIV, sFW, sFW, sLA, sLA, sLA, sTW, sCL, sIV },
/*
 *	sSS -> sIV	Server might not send FIN in this state.
 *	sS2 -> sIV
 *	sSR -> sFW	Close started.
 *	sES -> sFW
 *	sFW -> sLA	FIN seen in both directions.
 *	sCW -> sLA
 *	sLA -> sLA	Retransmitted FIN.
 *	sTW -> sTW
 *	sCL -> sCL
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*ack*/	   { sIV, sIG, sSR, sES, sCW, sCW, sTW, sTW, sCL, sIG },
/*
 *	sSS -> sIG	Might be a half-open connection.
 *	sS2 -> sIG
 *	sSR -> sSR	Might answer late resent SYN.
 *	sES -> sES	:-)
 *	sFW -> sCW	Normal close request answered by ACK.
 *	sCW -> sCW
 *	sLA -> sTW	Last ACK detected (RFC5961 challenged)
 *	sTW -> sTW	Retransmitted last ACK.
 *	sCL -> sCL
 */
/* 	     sNO, sSS, sSR, sES, sFW, sCW, sLA, sTW, sCL, sS2	*/
/*rst*/    { sIV, sCL, sCL, sCL, sCL, sCL, sCL, sCL, sCL, sCL },
/*none*/   { sIV, sIV, sIV, sIV, sIV, sIV, sIV, sIV, sIV, sIV }
	}
};

static unsigned int get_conntrack_index(bool syn, bool ack, bool fin, bool rst)
{
	if (rst) return TCP_RST_SET;
	else if (syn) return (ack ? TCP_SYNACK_SET : TCP_SYN_SET);
	else if (fin) return TCP_FIN_SET;
	else if (ack) return TCP_ACK_SET;
	else return TCP_NONE_SET;
}

static __always_inline long snat_tcp(struct __sk_buff *skb,
				     __u32 ifindex, struct ethhdr *l2, struct iphdr *l3, struct tcphdr *l4,
				     __u16 listen_port, __u16 host_port)
{
	__u32 saddr, offset, node_ip = nodenic_ip;
	union macaddr *macaddr;
	__u16 ip_hlen;
	__u64 flags;
	long err;

	saddr = l3->saddr;
	node_ip = nodenic_ip;
	ip_hlen = BPF_CORE_READ_BITFIELD(l3, ihl);
	ip_hlen <<= 2;

	/* Update L2 addrs */
	macaddr = (union macaddr *)l2->h_dest;
	macaddr->p1 = nodegw_macaddr_p1;
	macaddr->p2 = nodegw_macaddr_p2;
	macaddr = (union macaddr *)l2->h_source;
	macaddr->p1 = nodenic_macaddr_p1;
	macaddr->p2 = nodenic_macaddr_p2;

	/* Update L4 csum and source port */
	offset = TCP_CSUM_OFF(ip_hlen);
	flags = BPF_F_PSEUDO_HDR | sizeof(saddr);
	err = bpf_l4_csum_replace(skb, offset, saddr, node_ip, flags);
	if (err)
		return err;

	flags = BPF_F_PSEUDO_HDR | sizeof(listen_port);
	err = bpf_l4_csum_replace(skb, offset, listen_port, host_port, flags);
	if (err)
		return err;

	flags = 0;
	err = bpf_skb_store_bytes(skb, TCP_SRC_OFF(ip_hlen), &host_port, sizeof(host_port), flags);
	if (err)
		return err;

	/* Update L3 csum and source addr */
	err = bpf_l3_csum_replace(skb, IP_CSUM_OFF, saddr, node_ip, sizeof(saddr));
	if (err)
		return err;

	flags = 0;
	err = bpf_skb_store_bytes(skb, IP_SADDR_OFF, &node_ip, sizeof(node_ip), flags);
	if (err)
		return err;

	return 0;
}

static __always_inline void update_session(enum ip_conntrack_dir dir, struct nat_session *sess,
					   __u64 now_ns, bool syn, bool ack, bool fin, bool rst)
{
	enum tcp_conntrack old_state, new_state;
	unsigned int index;

	session_lazy_refresh(sess, now_ns);

	/* update CT state */
	if (dir > IP_CT_DIR_REPLY) {
		/* dir should be either IP_CT_DIR_ORIGINAL or IP_CT_DIR_REPLY */
		return;
	}

	index = get_conntrack_index(syn, ack, fin, rst);
	if (index > TCP_NONE_SET) {
		/* see enum tcp_bit_set */
		return;
	}

	old_state = sess->state;
	if (old_state > TCP_CONNTRACK_SYN_SENT2) {
		/* TCP_CONNTRACK_SYN_SENT2 = TCP_CONNTRACK_LISTEN = 9
		 * If we reach here, the state should be either
		 *   - sIG (IGNORED)
		 *   - sIV (INVALID)
		 * Proceed with state intact
		 */
		return;
	}

	new_state = tcp_conntracks[dir][index][old_state];
	/* no store if state remain unchanged */
	if (new_state != old_state)
		sess->state = new_state;
}

static __always_inline bool create_new_sessions(struct session_key *ekey,
						__u64 now_ns, __u32 vm_ifindex,
						struct snat_ip *snat_ip, __u16 snat_port)
{
	return create_nat_session(ekey, now_ns, vm_ifindex, snat_ip, snat_port,
				  TCP_CONNTRACK_SYN_SENT);
}

#endif /* __TCP_H */
