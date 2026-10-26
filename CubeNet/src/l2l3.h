// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright (c) 2025 Cube Authors */
#ifndef __L2L3_H
#define __L2L3_H

#include <vmlinux.h>
#include "cubevs.h"

/**
 * set_mac_pair - overwrite both source and destination MAC addresses in L2 header
 * @l2:     pointer to Ethernet header
 * @src_p1: first 4 bytes of source MAC
 * @src_p2: last 2 bytes of source MAC
 * @dst_p1: first 4 bytes of destination MAC
 * @dst_p2: last 2 bytes of destination MAC
 */
static __always_inline void set_mac_pair(struct ethhdr *l2,
					 __u32 src_p1, __u16 src_p2,
					 __u32 dst_p1, __u16 dst_p2)
{
	union macaddr *macaddr;

	macaddr = (union macaddr *)l2->h_source;
	macaddr->p1 = src_p1;
	macaddr->p2 = src_p2;
	macaddr = (union macaddr *)l2->h_dest;
	macaddr->p1 = dst_p1;
	macaddr->p2 = dst_p2;
}

#endif /* __L2L3_H */
