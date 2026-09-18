/* SPDX-License-Identifier: GPL-2.0 */
#ifndef _CUBE_GAUGE_H
#define _CUBE_GAUGE_H

#include <linux/types.h>
#include <linux/build_bug.h>

/*
 * layout=2 (whole-only): header + single whole record.
 * One sandbox == one container == one machine-wide metric line.
 * No per-cgroup slots, no idmap.
 *
 * layout was bumped from 1 when cpu_permille was replaced by the raw
 * cpu_usage_ns counter. The two occupy the same offset but mean entirely
 * different things, so a stale reader must be rejected outright rather
 * than left to print nanoseconds as a permille figure.
 */
#define CUBE_GAUGE_MAGIC		0x47554745u	/* "GAUG" */
#define CUBE_GAUGE_VERSION		2
#define CUBE_GAUGE_LAYOUT		2
#define CUBE_GAUGE_HEADER_SIZE		64
#define CUBE_GAUGE_WHOLE_SIZE		256
#define CUBE_GAUGE_WHOLE_OFFSET		64
#define CUBE_GAUGE_MIN_SIZE		(CUBE_GAUGE_WHOLE_OFFSET + CUBE_GAUGE_WHOLE_SIZE)

/*
 * Each source has its own validity bit: cgroup, system disk and NIC fail
 * independently (e.g. a sandbox with no NIC still has valid cpu/mem/disk),
 * and the reader must tell "absent" from "zero".
 */
#define CUBE_GAUGE_FLAG_VALID		(1ULL << 0)	/* cpu_usage_ns + mem_bytes */
#define CUBE_GAUGE_FLAG_SYSDISK_VALID	(1ULL << 1)	/* sysdisk_* */
#define CUBE_GAUGE_FLAG_NET_VALID	(1ULL << 2)	/* net_* */

/* cgroup accounting mode (header.cgroup_mode) */
#define CUBE_GAUGE_CG_V2		1
#define CUBE_GAUGE_CG_V1		2

struct cube_gauge_header {
	u32 magic;
	u16 version;
	u16 layout;
	u32 header_size;
	u32 whole_size;
	u64 total_size;
	u32 cgroup_mode;	/* 1=v2, 2=v1 (self-describing, for diagnostics) */
	u8  reserved[36];
} __packed;

/*
 * Everything except mem_bytes is a raw monotonic counter, not a rate: the
 * reader differentiates them against timestamp_ns. That keeps the numbers
 * correct across a changed sample interval, dropped samples and snapshot
 * restore, none of which the writer can compensate for on its own. mem is
 * a point-in-time value, so it is published as-is.
 */
struct cube_gauge_global {
	u64 flags;
	u64 timestamp_ns;
	u64 sequence;
	u64 cpu_usage_ns;	/* cgroup cumulative CPU time */
	u64 mem_bytes;
	u64 sysdisk_read_bytes;
	u64 sysdisk_write_bytes;
	u64 sysdisk_read_ios;
	u64 sysdisk_write_ios;
	u64 net_rx_bytes;
	u64 net_tx_bytes;
	u64 net_rx_packets;
	u64 net_tx_packets;
	u8  reserved[152];
} __packed;

/* The record size is ABI: shrink reserved[] when adding fields. */
static_assert(sizeof(struct cube_gauge_header) == CUBE_GAUGE_HEADER_SIZE);
static_assert(sizeof(struct cube_gauge_global) == CUBE_GAUGE_WHOLE_SIZE);

#endif /* _CUBE_GAUGE_H */
