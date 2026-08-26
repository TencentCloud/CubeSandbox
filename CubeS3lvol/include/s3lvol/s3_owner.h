/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

/**
 * \file
 * s3_owner -- the lvstore occupancy marker
 *
 * An lvstore can have exactly one owner at any time. This module turns that
 * constraint into something checkable by means of a single object on S3,
 * `<lvs_name>/meta/owner`.
 *
 * === Why it is needed ===
 *
 * Since attach exists, the same lvstore can be attached twice -- two processes
 * each running their own flusher, overwriting each other on the same keys. That
 * does not error, it only **silently corrupts data**: the last writer of each
 * chunk wins, while both chunk maps believe they are right.
 *
 * Before attach existed this could not happen, so the check is the debt attach
 * brought with it.
 *
 * === Deliberately no lease ===
 *
 * The old 30 s renewal scheme has been removed. Do not "helpfully" add it
 * back:
 *
 *   1. Judging liveness by timestamp necessarily misjudges. A process that only
 *      stalled (long GC, network jitter) is declared dead while it is still
 *      writing -- which manufactures real concurrent writes, worse than not
 *      checking at all.
 *   2. A lease without fencing does not stop split brain. The old owner does
 *      not know its lease expired and keeps writing. Real safety needs a fence
 *      token, and the design principle is "one owner per lvstore, forever; do
 *      not invent lease/fence".
 *
 * So a marker left behind by a crash **deliberately does not expire on its
 * own**. Rather than guessing liveness by timeout, a human confirms explicitly
 * and overrides with `rcow_attach_lvstore --force`.
 *
 * === What this mechanism does not stop (important) ===
 *
 * *It does not stop a simultaneous race.* Two processes acquire at the same
 * time, both read "unowned", both write their marker -- the result is two
 * owners. Closing that window for real needs a conditional write
 * (If-None-Match), which COS does not support: the `if_none_match` of `s3_put`
 * silently returns 0 on COS, so it cannot be used to infer "I am the first".
 *
 * acquire **reads the marker back and verifies the nonce** after writing, which
 * narrows the window from "read to write" to "write to read-back", and under
 * most interleavings lets at most one process succeed. But it is **not**
 * mutual exclusion: an interleaving of A writes, A reads back, B writes, B
 * reads back leaves both sides believing they hold it.
 *
 * In other words: this guards against **misoperations that happen in sequence**
 * (another process still alive, or a marker left by a previous crash), not
 * against **concurrent scheduling**. True exclusion needs an external
 * coordinator and does not belong in this layer.
 */

#ifndef S3LVOL_S3_OWNER_H
#define S3LVOL_S3_OWNER_H

#include "spdk/stdinc.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_client.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Upper bound for a host name. Big enough for Linux HOST_NAME_MAX (64). */
#define S3_OWNER_NODE_ID_MAX    64

/* Upper bound for the owner document. The real content is about 200 bytes; this
 * leaves generous headroom. It is also the length of the range GET used to read
 * the document, so it caps how much can be read back. */
#define S3_OWNER_DOC_MAX        1024

/**
 * The marker's content. Handed to the caller on a conflict, to put "who holds
 * it" into the error message -- otherwise an operator sees only -EBUSY and
 * cannot tell whom to go find.
 */
struct s3_owner_info {
	char     node_id[S3_OWNER_NODE_ID_MAX];
	int32_t  pid;

	/* Unix timestamp (seconds) of the acquire. **For display only**; it
	 * takes part in no liveness decision -- see "Deliberately no lease"
	 * in the file header. */
	uint64_t attach_ts;

	/* Which lvstore the holder believes it is attached to. Only has a true
	 * value after create, so it may be all zeroes. */
	struct spdk_uuid lvs_uuid;

	/* Generated randomly on every acquire, to recognise "this marker is
	 * mine" after writing. Not (node_id, pid): pids get reused. */
	struct spdk_uuid nonce;
};

/**
 * \param holder  non-NULL only when status == -EBUSY and the other side's info
 *                parsed successfully
 */
typedef void (*s3_owner_acquire_cb)(void *cb_arg,
				    const struct s3_owner_info *holder,
				    int status);

typedef void (*s3_owner_cb)(void *cb_arg, int status);

/**
 * Claim ownership of \c lvs_name.
 *
 * Flow: GET the existing marker -> if absent, PUT ours -> read it back and
 * verify the nonce.
 *
 * When a marker exists and \c force is false, the callback gets -EBUSY along
 * with the holder's info. With \c force true it is overwritten, and the
 * displaced holder is written to the log (not silently discarded --
 * overwriting somebody's marker is an action that must leave a trace).
 *
 * \param lvs_uuid  the lvstore uuid to record in the marker; pass all-zeroes
 *                  while create has not finished
 *
 * Asynchronous. The callback **may fire before this function returns** (param
 * validation or submission failure).
 */
void s3_owner_acquire(struct s3_client *client, const char *lvs_name,
		      const struct spdk_uuid *lvs_uuid, bool force,
		      s3_owner_acquire_cb cb_fn, void *cb_arg);

/**
 * Release ownership (delete the marker). Called on a clean unload so the next
 * attach does not need --force.
 *
 * Failure **must not block the unload**: a leftover marker only means the next
 * attach needs --force, and the unload is already done -- failing it would buy
 * nothing.
 */
void s3_owner_release(struct s3_client *client, const char *lvs_name,
		      s3_owner_cb cb_fn, void *cb_arg);

/**
 * Format the holder info into one line, for error messages. \c out is always
 * NUL-terminated.
 */
void s3_owner_info_str(const struct s3_owner_info *info, char *out,
		       size_t out_len);

#ifdef __cplusplus
}
#endif

#endif /* S3LVOL_S3_OWNER_H */
