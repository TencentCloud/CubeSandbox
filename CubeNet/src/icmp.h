// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2025 Cube Authors */
#ifndef __ICMP_H
#define __ICMP_H

#include <vmlinux.h>
#include "cubevs.h"
#include "map.h"
#include "session.h"

#define ICMP_ECHOREPLY		0	/* Echo Reply			*/
#define ICMP_ECHO		8	/* Echo Request			*/

/* ICMP session timeout: 30 seconds */
#define ICMP_TIMEOUT		(30ULL * 1000 * 1000 * 1000)

/* ICMP conntrack states reuse UDP states for simplicity */
#define ICMP_CT_UNREPLIED	0
#define ICMP_CT_REPLIED		1

/* 4-byte aligned buffer for ICMP identifier incremental checksum update.
 * The identifier occupies the high 16 bits; the low 16 bits are zero-padded.
 */
struct icmp_id_buff {
	__u16 id;
	__u16 reserved;
};

static __always_inline void update_icmp_session(enum ip_conntrack_dir dir,
						struct nat_session *sess,
						__u64 now_ns)
{
	session_lazy_refresh(sess, now_ns);
	session_mark_replied(dir, sess, ICMP_CT_UNREPLIED, ICMP_CT_REPLIED);
}

static __always_inline bool create_icmp_sessions(struct __sk_buff *skb,
						 struct session_key *ekey,
						 __u64 now_ns, __u32 vm_ifindex,
						 struct snat_ip *snat_ip, __u16 snat_id)
{
	return create_nat_session(skb, ekey, now_ns, vm_ifindex, snat_ip, snat_id,
				  ICMP_CT_UNREPLIED);
}

#endif /* __ICMP_H */
