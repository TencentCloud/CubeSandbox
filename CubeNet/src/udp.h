// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2025 Cube Authors */
#ifndef __UDP_H
#define __UDP_H

#include <vmlinux.h>
#include "cubevs.h"
#include "session.h"

#define UDP_TIMEOUT_UNREPLIED	(30ULL  * 1000 * 1000 * 1000)
#define UDP_TIMEOUT_REPLIED	(180ULL * 1000 * 1000 * 1000)

static __always_inline void update_udp_session(enum ip_conntrack_dir dir,
					       struct nat_session *sess,
					       __u64 now_ns)
{
	session_lazy_refresh(sess, now_ns);
	session_mark_replied(dir, sess, UDP_CT_UNREPLIED, UDP_CT_REPLIED);
}

static __always_inline bool create_udp_sessions(struct __sk_buff *skb,
						struct session_key *ekey,
						__u64 now_ns, __u32 vm_ifindex,
						struct snat_ip *snat_ip, __u16 snat_port)
{
	return create_nat_session(skb, ekey, now_ns, vm_ifindex, snat_ip, snat_port,
				  UDP_CT_UNREPLIED);
}

#endif /* __UDP_H */
