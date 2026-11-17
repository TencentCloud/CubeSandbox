/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * Single-flight sequencing for whole-object registries.
 *
 * A mutation receives a monotonically increasing sequence.  A PUT captures
 * every mutation through flight_seq; callers at or below that boundary finish
 * with that PUT's status.  Mutations arriving during the PUT remain beyond the
 * boundary and require one coalesced follow-up.
 *
 * Header-only so ordering and waiter boundaries can be tested without S3 or
 * SPDK.  Callback ownership remains with each registry implementation.
 */

#ifndef S3LVOL_REGISTRY_PERSIST_CTL_H
#define S3LVOL_REGISTRY_PERSIST_CTL_H

#include <stdbool.h>
#include <stdint.h>

struct registry_persist_ctl {
	uint64_t mutation_seq;
	uint64_t settled_seq;
	uint64_t flight_seq;
	bool     in_flight;
};

static inline uint64_t
registry_persist_mutate(struct registry_persist_ctl *ctl)
{
	return ++ctl->mutation_seq;
}

/* Start a snapshot of all mutations known at this instant. */
static inline bool
registry_persist_start(struct registry_persist_ctl *ctl)
{
	if (ctl->in_flight || ctl->mutation_seq == ctl->settled_seq) {
		return false;
	}
	ctl->in_flight = true;
	ctl->flight_seq = ctl->mutation_seq;
	return true;
}

/*
 * Close the current snapshot and return its waiter boundary.  A failed PUT is
 * settled too: its covered callers receive the failure rather than being
 * silently retried.  A later mutation (including a caller's rollback) gets a
 * new sequence and therefore a new PUT.
 */
static inline uint64_t
registry_persist_complete(struct registry_persist_ctl *ctl)
{
	uint64_t completed = ctl->flight_seq;

	ctl->in_flight = false;
	ctl->settled_seq = completed;
	return completed;
}

static inline bool
registry_persist_pending(const struct registry_persist_ctl *ctl)
{
	return ctl->mutation_seq != ctl->settled_seq;
}

#endif /* S3LVOL_REGISTRY_PERSIST_CTL_H */
