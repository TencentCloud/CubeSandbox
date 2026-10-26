/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   s3_local_dev -- super block and region layout on the local bdev 
 *
 *   === Layout ===
 *
 *   Single bdev (WAL and cache share one device):
 *
 *     offset      region
 *     0           super (4 KiB)
 *     4K          metadata journal
 *     +256MWAL ring
 *     +...        chunk cache (whatever capacity is left)
 *
 *   Dual bdev (caller explicitly passes --wal-bdev + --cache-bdev):
 *
 *     WAL bdev:    super | metadata journal | WAL ring
 *     Cache bdev:  chunk cache
 *
 *   The super block *always records each region's offset/size explicitly*
 *   rather than deriving them from compile-time constants. That way changing
 *   the layout or a region size does not silently invalidate existing disks --
 *   the old layout is still readable and can be recognized.
 *
 *   === Why this is a separate file ===
 *
 *   The journal (s3_journal.c), the WAL (s3_wal.c) and the cache
 *   (s3_chunk_cache.c) all read and write some region of the local bdev, and
 *   all of them need to know "where does my region start and how big is it".
 *   Keeping the layout and the super block I/O here means each of them just
 *   takes a region descriptor and never has to parse the super block itself.
 */

#ifndef S3LVOL_LOCAL_DEV_H
#define S3LVOL_LOCAL_DEV_H

#include "spdk/stdinc.h"
#include "spdk/bdev.h"
#include "spdk/assert.h"
#include "spdk/uuid.h"

/* Super block magic "S3LVOLSB", used to tell whether a disk already carries
 * our layout. */
#define S3_SUPER_MAGIC          0x53334C564F4C5342ULL

/* Version 2 added capacity_bytes and lvs_name, both of which the attach path
 * needs: the geometry it hands to blobstore has to be the geometry the lvstore
 * was created with, and the name is what proves this disk belongs to the
 * lvstore being attached. A v1 disk carries neither, so it is rejected with
 * -EPROTO rather than guessed at. */
#define S3_SUPER_VERSION        2
#define S3_SUPER_SIZE           4096

/* Room for the lvstore name in the super block. Matches SPDK_LVS_NAME_MAX, but
 * spelled out here because lib/ must not depend on the lvol library. */
#define S3_LVS_NAME_MAX         64

/* Default metadata journal size.
 *
 * NOTE: 256 MiB was chosen against the checkpoint threshold ("journal >= 128
 * MiB triggers a checkpoint"), leaving 2x headroom so the journal does not fill
 * up before a checkpoint completes. The threshold should probably become a
 * percentage of capacity instead of an absolute value, otherwise a smaller
 * journal configured by the caller wraps before the absolute threshold is ever
 * reached. */
#define S3_JOURNAL_DEFAULT_SIZE (256ULL * 1024 * 1024)

/* Default WAL ring size. The design suggests 1-4 GiB. */
#define S3_WAL_DEFAULT_SIZE     (1024ULL * 1024 * 1024)

enum s3_region_id {
	S3_REGION_SUPER = 0,
	S3_REGION_JOURNAL,
	S3_REGION_WAL,
	S3_REGION_CACHE,
	S3_REGION_COUNT,
};

/* Where a region lives on some bdev. All I/O is expressed relative to this,
 * so a component never needs to know what precedes it on the device. */
struct s3_region {
	uint64_t         offset;    /* byte offset within the bdev */
	uint64_t         size;      /* byte length */
	bool             valid;
};

/* On-disk super block. *Once this layout ships, field order must not change*;
 * only append at the tail and bump the version. */
struct s3_super_block {
	uint64_t         magic;
	uint32_t         version;
	uint32_t         block_size;        /* always 4096 (P4); recorded so it can be validated */

	/* uuid of the lvstore this disk belongs to.
	 *
	 * All zeroes until the lvstore has actually been created: blobstore
	 * generates the uuid itself and will not accept one from outside, so it
	 * is only known once spdk_lvs_init() has completed and is written back
	 * then (s3_local_dev_set_lvs_uuid). The attach path treats all-zeroes as
	 * "unknown, do not check" -- a create that crashed between formatting the
	 * disk and initialising the lvstore leaves exactly that state. */
	struct spdk_uuid lvs_uuid;

	uint32_t         chunk_size;
	uint32_t         reserved0;

	/* Explicit per-region layout, indexed by enum s3_region_id.
	 * In the dual-bdev case CACHE's offset/size refer to the cache bdev. */
	struct {
		uint64_t offset;
		uint64_t size;
	} regions[S3_REGION_COUNT];

	/* Generation of the most recent checkpoint and the journal LSN it
	 * covers. Recovery uses this to decide where replay starts. */
	uint64_t         checkpoint_gen;
	uint64_t         checkpoint_lsn;

	/* Logical provisioning capacity of the lvstore, in bytes.
	 *
	 * Recorded because the attach path has to hand blobstore the *same*
	 * geometry it was created with. blobstore rejects a device smaller than
	 * the size in its own super block (blobstore.c:1855), and a device that
	 * is larger silently invites a grow. Asking the operator to repeat the
	 * capacity on every attach would make a typo look like corruption. */
	uint64_t         capacity_bytes;

	/* Name of the lvstore, NUL terminated.
	 *
	 * This is the one identity check the attach path can make *before* it
	 * replays anything, which is why it exists: the S3 key prefix is derived
	 * from the name, so a mismatch means the local device belongs to a
	 * different lvstore and replaying its log would write foreign data into
	 * this one. */
	char             lvs_name[S3_LVS_NAME_MAX];

	/* Dual-bdev marker. When single, the cache shares the journal/WAL
	 * device. */
	uint8_t          dual_bdev;
	uint8_t          reserved1[7];

	/* CRC32C over every field above. Kept last, and treated as zero while
	 * computing itself. */
	uint32_t         crc;
} __attribute__((packed));

SPDK_STATIC_ASSERT(sizeof(struct s3_super_block) <= S3_SUPER_SIZE,
		   "super block must fit in one 4 KiB block");

struct s3_local_dev;

typedef void (*s3_local_dev_cb)(void *cb_arg, int status);

typedef void (*s3_local_dev_open_cb)(void *cb_arg, struct s3_local_dev *dev,
				     int status);

/**
 * What to record when formatting a fresh layout.
 *
 * A struct rather than a parameter list because most of these end up verbatim in
 * the super block, and a call with seven positional arguments of which four are
 * integers is one transposition away from a silently wrong disk.
 */
struct s3_local_dev_format_opts {
	/* bdev holding super + journal + WAL. Required. */
	const char *wal_bdev_name;

	/* bdev holding the chunk cache; NULL means it shares the WAL device
	 * (single-bdev layout). */
	const char *cache_bdev_name;

	/* Which lvstore this disk belongs to. Required: the attach path has
	 * nothing else to check before it starts replaying. */
	const char *lvs_name;

	/* Logical provisioning capacity of the lvstore. Required, because the
	 * attach path reads it back instead of asking the operator again. */
	uint64_t    capacity_bytes;

	uint32_t    chunk_size;

	uint64_t    journal_size;   /* 0 for the default */
	uint64_t    wal_size;       /* 0 for the default */
};

/**
 * Open the local bdev(s) and *format* a fresh layout (used when creating a
 * new lvstore).
 *
 * Writes the super block and zeroes the journal head. *This overwrites
 * whatever was on the disk.*
 *
 * lvs_uuid is left all-zeroes: blobstore assigns it and will not take one from
 * outside, so it is written back with s3_local_dev_set_lvs_uuid() once
 * spdk_lvs_init() has completed.
 *
 * Asynchronous: when the callback fires the super block is durable and
 * \c dev is usable (NULL on failure). The callback *may run before this
 * function returns* (argument validation or submission failure).
 */
void s3_local_dev_format(const struct s3_local_dev_format_opts *opts,
			 s3_local_dev_open_cb cb_fn, void *cb_arg);

/**
 * Open the local bdev(s) and *read* the existing layout (used when attaching
 * an existing lvstore).
 *
 * Asynchronous. status: -ENODEV bdev not found; -EILSEQ bad super block magic
 * or CRC; -EPROTO unsupported version.
 */
void s3_local_dev_open(const char *wal_bdev_name, const char *cache_bdev_name,
		       s3_local_dev_open_cb cb_fn, void *cb_arg);

void s3_local_dev_close(struct s3_local_dev *dev);

/**
 * Look up where a region lives.
 */
const struct s3_region *s3_local_dev_get_region(struct s3_local_dev *dev,
						enum s3_region_id id);

/**
 * Get the bdev descriptor / channel backing a region.
 *
 * In a single-bdev layout every region returns the same one; in a dual-bdev
 * layout the CACHE region returns the cache bdev.
 *
 * Channels are per-thread, so this must be called on the thread that will use
 * them.
 */
struct spdk_bdev_desc *s3_local_dev_get_desc(struct s3_local_dev *dev,
					     enum s3_region_id id);
struct spdk_io_channel *s3_local_dev_get_channel(struct s3_local_dev *dev,
						 enum s3_region_id id);

const struct s3_super_block *s3_local_dev_get_super(struct s3_local_dev *dev);

/**
 * Record the lvstore uuid in the super block and persist it.
 *
 * Called once on the create path, right after spdk_lvs_init() has handed out the
 * uuid. Nothing on the write path depends on it; it exists so that a later
 * attach can refuse a local device that belongs to a *different incarnation* of
 * an lvstore with the same name -- the case where the lvstore was destroyed and
 * recreated while this disk was kept. Without it that pairing looks valid and
 * replay writes stale data into the new lvstore.
 *
 * A failure here is not fatal to the create: the lvstore works, only the attach
 * check degrades to name-only. The caller should log it and carry on rather than
 * unwind a working lvstore.
 */
void s3_local_dev_set_lvs_uuid(struct s3_local_dev *dev,
			       const struct spdk_uuid *lvs_uuid,
			       s3_local_dev_cb cb_fn, void *cb_arg);

/**
 * Update the checkpoint pointer in the super block and persist it.
 *
 * Called once a checkpoint completes. It doubles as the proof that "the
 * journal may be truncated up to lsn", so the caller must wait for this
 * callback (i.e. for durability) before truncating the journal. Otherwise a
 * crash would leave an old checkpoint paired with an already-truncated
 * journal, which loses metadata.
 */
void s3_local_dev_update_checkpoint(struct s3_local_dev *dev,
				    uint64_t gen, uint64_t lsn,
				    s3_local_dev_cb cb_fn, void *cb_arg);

#endif /* S3LVOL_LOCAL_DEV_H */
