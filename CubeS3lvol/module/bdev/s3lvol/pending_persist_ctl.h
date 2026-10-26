/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   One in-flight pending-delete registry PUT per lvstore
 *
 *   Every set/clear serialises the whole registry to the same key. Independent
 *   PUTs can complete out of mutation order: a slow set(A) landing after
 *   cancel(A) brings A back after restart.
 *
 *   One PUT at a time, with a dirty flag so a completion always writes the
 *   newest in-memory snapshot rather than the bytes captured at submit.
 *   Header-only so the state machine is testable without S3 or SPDK.
 */

#ifndef S3LVOL_PENDING_PERSIST_CTL_H
#define S3LVOL_PENDING_PERSIST_CTL_H

#include <stdbool.h>

struct pending_persist_ctl {
	bool in_flight;
	bool dirty;
};

/* True: caller should serialise now and PUT. False: a PUT is already in
 * flight; this mutation will be written when that PUT completes. */
static inline bool
pending_persist_begin(struct pending_persist_ctl *c)
{
	if (c->in_flight) {
		c->dirty = true;
		return false;
	}
	c->in_flight = true;
	return true;
}

/* True: caller should serialise the current marks and PUT again.
 *
 * Does not set in_flight. The follow-up PUT is submitted on this thread by
 * begin(), which takes the flag; pre-setting it made that begin() coalesce
 * and skip the write. */
static inline bool
pending_persist_end(struct pending_persist_ctl *c)
{
	c->in_flight = false;
	if (!c->dirty) {
		return false;
	}
	c->dirty = false;
	return true;
}

#endif /* S3LVOL_PENDING_PERSIST_CTL_H */
