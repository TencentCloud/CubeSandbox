/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   Zero-copy export: describe a snapshot without reading it
 *
 *   === What this is ===
 *
 *   The whole of a cross-node handoff on the sending side, once the volume has
 *   been snapshotted and drained. It walks the snapshot's clone chain, turns each
 *   allocated cluster into the object the chunk map already has it in, and uploads
 *   one manifest naming them. No data is read; nothing is copied.
 *
 *   That is the difference between a handoff measured in round trips and one
 *   measured in gigabytes. The dense engine next door (s3_export.c) reads the
 *   snapshot and re-uploads it, which is the only option across buckets or
 *   regions, and is also what a ref export degrades into when the source
 *   eventually wants the snapshot back (see materialise, module side).
 *
 *   === The three-step translation, and where each step comes from ===
 *
 *     blob offset -> device LBA      spdk_blob_get_io_unit_lba(), which is the
 *                                    patch in patches/. Nothing else in the
 *                                    public API answers it.
 *     device LBA  -> chunk index     arithmetic, s3_lba_to_chunk_index()
 *     chunk index -> object uuid     the chunk map
 *
 *   The first step is why this file needs a patched SPDK. Reading blobstore's
 *   private cluster table would work too, right up until a field is added to
 *   struct spdk_blob and every LBA comes out wrong -- naming, with no error
 *   anywhere, objects that belong to another volume.
 *
 *   === Why a chain and not a blob ===
 *
 *   spdk_blob_get_next_allocated_io_unit() and spdk_blob_get_io_unit_lba() both
 *   read blob->active.clusters and stop there; neither follows a parent. That is
 *   exactly what this needs, but it means one blob is never the whole picture:
 *   taking a second snapshot of a volume hands the new snapshot only the clusters
 *   written since the first, because blobstore moves the cluster map across and
 *   leaves the older data in the older snapshot. So `lvol -> snap1 -> snap2`, the
 *   ordinary way to use this, produces a snap2 that owns almost nothing.
 *
 *   Hence the layers are walked nearest first, and a chunk already named is never
 *   revisited. Every layer's clusters live in the same bs_dev address space and
 *   resolve through the same chunk map, so a parent's object is not somebody
 *   else's -- it is in the same <prefix>/data/ as everything else.
 *
 *   === What the caller has to have done ===
 *
 *   Snapshotted, so the clusters cannot move; and drained, so every one of them
 *   has a committed mapping. A cluster blobstore has allocated whose data is
 *   still in the WAL has no object yet, and this refuses to continue rather than
 *   quietly leaving it out of the bitmap -- a chunk left out reads as zeroes on
 *   the importing node, which is data loss that announces itself nowhere.
 *
 *   Allocated does not imply an object, though, and the difference is not an
 *   error. write_zeroes on this device drops the mapping and deletes the object,
 *   since an unmapped chunk already reads back as zeroes -- so a cluster that
 *   was written and then zeroed stays allocated in blobstore while having
 *   nothing in S3. Filesystems do this in bulk: ext4's lazy inode table init
 *   zeroes megabytes of a freshly made volume, and every one of those clusters
 *   arrives here allocated and unmapped. Refusing them, which is what this used
 *   to do, turned an ordinary sparse snapshot into a full copy.
 *
 *   is_zeroes() separates the two, because it consults the overlay as well as
 *   the chunk map. A hole is left out of the bitmap, which is exactly how the
 *   manifest spells "reads as zeroes"; anything else still stops the walk.
 */

#include "spdk/stdinc.h"
#include "spdk/blob.h"
#include "spdk/log.h"
#include "spdk/string.h"
#include "spdk/util.h"
#include "spdk/uuid.h"

#include "s3lvol/s3_bs_dev.h"
#include "s3lvol/s3_chunk_map.h"
#include "s3lvol/s3_export.h"

struct ref_walk {
	struct s3_export_manifest *m;
	struct s3_chunk_map       *map;

	/* Needed for is_zeroes(): a cluster with no mapping is either a hole or
	 * data that has not been flushed, and only the device can tell which. */
	struct spdk_bs_dev        *bs_dev;

	uint32_t                   chunk_shift;
	uint64_t                   io_units_per_chunk;

	/* Of the snapshot, i.e. of chain[0]. Ancestors are never larger -- a volume
	 * can only grow -- but they can be smaller, and a layer's own length is what
	 * bounds its walk. This bounds the manifest. */
	uint64_t                   num_io_units;

	uint64_t                   named;
	uint64_t                   bytes_named;

	/* Clusters blobstore has allocated that hold no data at all. Counted
	 * rather than recorded: the manifest says "not present" for them, which
	 * is what the importer needs, and the count is worth a line in the log. */
	uint64_t                   holes;
};

/* One layer. Returns the number of chunks it contributed, or negative on error.
 *
 * Chunks already named are skipped rather than overwritten: the layers arrive
 * nearest first, so whoever got there first is the closest layer that has the
 * cluster, which is the one blobstore would reach by reading through the chain.
 */
static int64_t
walk_layer(struct ref_walk *w, struct spdk_blob *blob, const char *uuid_str,
	   uint32_t depth)
{
	uint64_t layer_io_units = spdk_blob_get_num_io_units(blob);
	uint64_t offset = 0;
	int64_t contributed = 0;
	int rc;

	/* A layer longer than the snapshot would be describing chunks the manifest
	 * has no room for. Cannot happen from a resize -- ancestors are the older,
	 * smaller versions -- but the loop below indexes the ref table with it. */
	if (layer_io_units > w->num_io_units) {
		layer_io_units = w->num_io_units;
	}

	while (offset < layer_io_units) {
		struct spdk_uuid uuid;
		uint32_t valid_bytes = 0;
		uint64_t lba;
		uint64_t device_chunk;
		uint64_t export_chunk;

		offset = spdk_blob_get_next_allocated_io_unit(blob, offset);
		if (offset == UINT64_MAX || offset >= layer_io_units) {
			break;
		}

		export_chunk = offset / w->io_units_per_chunk;

		if (s3_export_manifest_is_present(w->m, export_chunk)) {
			/* A nearer layer already owns this chunk. */
			offset += w->io_units_per_chunk;
			continue;
		}

		lba = spdk_blob_get_io_unit_lba(blob, offset);
		if (lba == UINT64_MAX) {
			/* Allocated a moment ago according to the query above. Only a
			 * concurrent modification explains this, which means the blob is
			 * not the read-only snapshot it was supposed to be. */
			SPDK_ERRLOG("Export %s: cluster at io unit %" PRIu64 " of layer "
				    "%u went away mid-walk. Is the blob really "
				    "read-only?\n", uuid_str, offset, depth);
			return -EAGAIN;
		}

		device_chunk = s3_lba_to_chunk_index(lba, w->chunk_shift);

		rc = s3_chunk_map_lookup(w->map, device_chunk, &uuid, &valid_bytes);
		if (rc != 0) {
			/* No object holds this cluster. Two very different things look
			 * identical here, and treating them alike is what made this walk
			 * refuse whole snapshots it could have described:
			 *
			 *   a hole -- blobstore allocated the cluster and something then
			 *   zeroed it. write_zeroes on this device drops the mapping and
			 *   deletes the object, because an unmapped chunk reads back as
			 *   zeroes, so "allocated" and "has an object" stop agreeing. A
			 *   filesystem does this in bulk: ext4's lazy inode table init
			 *   zeroes megabytes of a freshly made volume.
			 *
			 *   data that is not in S3 yet -- still in the WAL or the overlay.
			 *   Naming no object for it would hand the importer zeroes in
			 *   place of real data, silently, which is the one outcome that
			 *   must not happen.
			 *
			 * is_zeroes() is the device's own answer to exactly this question
			 * -- it checks the overlay as well as the chunk map -- so it is
			 * asked rather than reimplemented. A hole is left absent from the
			 * bitmap, which is how the manifest spells "reads as zeroes".
			 */
			if (w->bs_dev->is_zeroes(w->bs_dev, lba,
						 w->io_units_per_chunk)) {
				w->holes++;
				offset += w->io_units_per_chunk;
				continue;
			}

			/* Not a hole: the caller is expected to drain and walk again, and
			 * normally does. It is only fatal if it happens a second time,
			 * which the caller reports. */
			SPDK_NOTICELOG("Export %s: chunk %" PRIu64 " of layer %u (device "
				       "chunk %" PRIu64 ", LBA %" PRIu64 ") has no committed "
				       "mapping yet (%s); the lvstore needs a drain\n",
				       uuid_str, export_chunk, depth, device_chunk, lba,
				       spdk_strerror(-rc));
			return -EIO;
		}

		rc = s3_export_manifest_set_ref(w->m, export_chunk, &uuid, valid_bytes);
		if (rc != 0) {
			SPDK_ERRLOG("Export %s: chunk %" PRIu64 " could not be recorded: "
				    "%s\n", uuid_str, export_chunk, spdk_strerror(-rc));
			return rc;
		}

		w->named++;
		w->bytes_named += valid_bytes;
		contributed++;
		offset += w->io_units_per_chunk;
	}

	return contributed;
}

int
s3_export_run_ref(const struct s3_export_ref_opts *opts, s3_export_cb cb, void *cb_arg)
{
	struct s3_export_manifest *m = NULL;
	struct ref_walk w = {0};
	uint64_t size_bytes;
	uint32_t chunk_size;
	uint32_t i;
	int64_t contributed;
	int rc;

	if (!opts || !opts->bs_dev || !opts->chain || opts->chain_len == 0 ||
	    !opts->client || !opts->prefix || !opts->uuid_str) {
		return -EINVAL;
	}
	for (i = 0; i < opts->chain_len; i++) {
		if (!opts->chain[i]) {
			return -EINVAL;
		}
	}

	w.map = s3_bs_dev_get_chunk_map(opts->bs_dev);
	if (!w.map) {
		SPDK_ERRLOG("The source device has no chunk map, so there is nothing to "
			    "reference\n");
		return -EINVAL;
	}

	/* Asked per unmapped cluster, to tell a hole from data that is still local.
	 * A device without it cannot be walked safely: every unmapped cluster would
	 * have to be assumed to hold data, which is the old behaviour and refuses
	 * snapshots that are merely sparse.
	 *
	 * Not an error, and deliberately not logged as one: -ENOTSUP routes the
	 * caller to the copying engine, which needs none of this and produces a
	 * correct export. s3_bs_dev always provides is_zeroes, so this is here for
	 * some future device rather than for anything that exists. */
	if (!opts->bs_dev->is_zeroes) {
		SPDK_NOTICELOG("The source device cannot report zeroed ranges, so a "
			       "hole cannot be told from unflushed data. Exporting by "
			       "copying instead.\n");
		return -ENOTSUP;
	}
	w.bs_dev = opts->bs_dev;

	chunk_size = s3_bs_dev_get_chunk_size(opts->bs_dev);
	if (chunk_size == 0) {
		return -EINVAL;
	}

	/* One blob cluster has to be exactly one chunk map entry, which holds when
	 * the two sizes agree: clusters are allocated at cluster granularity from
	 * the start of the device, so cluster N begins at N * cluster_size, which is
	 * chunk N. Without that equality a cluster would span several chunks or
	 * share one, and neither the bitmap nor the ref table can express it. The
	 * caller falls back to the copying engine, which references nothing and so
	 * does not care. */
	if (opts->cluster_size != chunk_size) {
		SPDK_ERRLOG("A zero-copy export needs cluster_size == chunk_size, but "
			    "the lvstore has %u and %u. Export by copying instead.\n",
			    opts->cluster_size, chunk_size);
		return -ENOTSUP;
	}

	w.chunk_shift = spdk_u32log2(chunk_size);
	w.io_units_per_chunk = chunk_size / S3LVOL_BLOCK_SIZE;
	w.num_io_units = spdk_blob_get_num_io_units(opts->chain[0]);
	size_bytes = w.num_io_units * S3LVOL_BLOCK_SIZE;

	rc = s3_export_manifest_create(opts->uuid_str, size_bytes, chunk_size,
				       S3_EXPORT_LAYOUT_REF, &m);
	if (rc != 0) {
		return rc;
	}
	w.m = m;

	m->src = opts->src;
	m->cluster_size = opts->cluster_size;
	m->created_at = (uint64_t)time(NULL);
	m->expires_at = opts->expires_at;

	/* The walk. Every step is memory: blobstore is asked which cluster is next,
	 * not to read it. */
	for (i = 0; i < opts->chain_len; i++) {
		contributed = walk_layer(&w, opts->chain[i], opts->uuid_str, i);
		if (contributed < 0) {
			rc = (int)contributed;
			goto err;
		}
		SPDK_DEBUGLOG(s3lvol_export, "Export %s: layer %u named %" PRId64
			      " chunk(s)\n", opts->uuid_str, i, contributed);
	}

	SPDK_NOTICELOG("Export %s references %" PRIu64 " object(s) holding %" PRIu64
		       " byte(s) of a %" PRIu64 "-byte snapshot across %u layer(s), "
		       "skipping %" PRIu64 " zeroed cluster(s); nothing was copied\n",
		       opts->uuid_str, w.named, w.bytes_named, size_bytes,
		       opts->chain_len, w.holes);

	/* Hands its reference to the callback. */
	rc = s3_export_manifest_publish(opts->client, m, cb, cb_arg);
	if (rc != 0) {
		goto err;
	}

	s3_export_manifest_unref(m);
	return 0;
err:
	s3_export_manifest_unref(m);
	return rc;
}
