/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * When a lease-aware export may be treated as "nobody imported it".
 *
 * A 404 only means the last check missed. An importer can PUT the lease after
 * that HEAD and still before expires_at; combining "ever saw 404" with
 * "now past the deadline" would reap a live import's manifest. The miss has
 * to have been observed on a check submitted at or after the deadline.
 */

#ifndef EXPORT_LEASE_TERM_H
#define EXPORT_LEASE_TERM_H

#include <stdbool.h>
#include <stdint.h>

static inline bool
export_lease_miss_confirms_gone(bool lease_absent, uint64_t absent_at,
				uint64_t expires_at, uint64_t now)
{
	if (!lease_absent || expires_at == 0) {
		return false;
	}
	if (now < expires_at) {
		return false;
	}
	return absent_at >= expires_at;
}

#endif
