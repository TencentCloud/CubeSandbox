/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   TODO: not implemented yet.
 *
 *   GC is a full scan that deletes orphans: list the whole prefix and delete
 *   every object not in the live set. No reference counting and no
 *   mark-and-sweep are needed -- the chunk map lives at the bs_dev layer and
 *   covers the whole device address space, so "an object is live iff it appears
 *   in the chunk map".
 *
 *   === The complete definition of the live set ===
 *
 *   Read all four items before implementing. The first three are what intuition
 *   suggests; the fourth is not, and missing it silently deletes data another
 *   node is reading right now.
 *
 *   1. everything under <lvs>/meta/      -- super block, checkpoint, owner,
 *                                           imports.json, exports.json
 *   2. the unreclaimed segments under <lvs>/wal/
 *   3. <lvs>/data/<uuid>                  -- the ones present in the chunk map
 *   4. when <lvs>/exports/<uuid>.json exists:
 *        a. dense layout: everything under <lvs>/exports/<uuid>/ is live
 *        b. **ref layout: every <lvs>/data/<uuid> that manifest references is
 *           live, even if it is no longer in the chunk map**
 *
 *   === Why 4b has to exist ===
 *
 *   The zero-copy export manifest references this lvstore's live objects,
 *   not copies. In the normal case those objects are also in the chunk map (the
 *   exported read-only snapshot holds the corresponding clusters), so rule 3
 *   alone would be enough -- 4b looks redundant.
 *
 *   It is not redundant in these two cases:
 *
 *     - The exported snapshot is deleted. The snapshot's clusters merge into its
 *       only clone and end up on the writable origin volume; once the origin
 *       overwrites those clusters, create-once produces a new uuid and the old
 *       uuid leaves the chunk map. The B-side's manifest still points at the old
 *       uuid.
 *     - The snapshot is deleted after the export TTL expires
 *       (`s3lvol_export_pinning()` returns "do not pin" for an expired export,
 *       so this path happens by default; nobody has to enable it).
 *
 *   In both cases a GC judging by rule 3 alone deletes those objects as
 *   orphans. B receives no notice: its imports registry caches the manifest
 *   text and loads it on attach without re-GETting (see the imports_serialize
 *   comment in vbdev_s3lvol_xfer.c). B then reads 404s, returns zeroes, and
 *   only a checksum can catch it.
 *
 *   === Why not "materialise" instead of 4b ===
 *
 *   Materialising works like this: when
 *   side A wants to delete a referenced snapshot, it first CopyObjects the
 *   objects into the exports prefix, rewrites the manifest atomically into
 *   dense form (generation+1), then allows the delete. The live set would then
 *   need only 4a.
 *
 *   It founders on the manifest rewrite: side B caches the manifest text, so
 *   after the rewrite B still holds the old ref manifest pointing at the
 *   data/<uuid> about to be reclaimed. Fixing that requires B to refresh
 *   manifests, and s3_export_bs_dev.c's lock-free multithreaded reads are
 *   explicitly built on "the parsed manifest is immutable" (see the Threading
 *   section at the head of that file). The price is an atomic swap, deferred
 *   freeing of the old manifest, and internal retry after 404 (otherwise the
 *   I/O error would propagate to the application) -- all to alter a correct
 *   design for a feature that is not needed.
 *
 *   4b needs none of that: no CopyObject, no extra storage, nothing for B to
 *   change, and it is isomorphic to 4a -- a manifest in place makes the objects
 *   it references live. The only difference is that "references" in the ref
 *   layout has to be learned from the manifest's ref table, while the dense
 *   layout says so by the prefix.
 *
 *   The dense engine stays anyway, but its purpose is cross-bucket /
 *   cross-region (B cannot even read A's bucket), not materialisation.
 *
 *   === Hard prerequisite in the implementation order ===
 *
 *   The only infrastructure GC lacks is `s3_list_objects()` (paged list;
 *   currently -ENOTSUP). Note 4b requires GC to read the ref tables of all ref
 *   manifests -- but GC has to list the exports prefix and read the manifests
 *   anyway (to tell which exports/<uuid>/ prefixes are orphans), so this part
 *   comes for free rather than costing extra.
 *
 *   Until 4b is implemented, do not lift the "-EBUSY on deleting a snapshot
 *   referenced by a ref export" guard in vbdev_s3lvol_lvstore.c. The TTL
 *   expiry path has not caused trouble only because GC does not exist yet and
 *   nobody deletes orphans -- that is accidental safety, not a design
 *   guarantee.
 */

#include "spdk/stdinc.h"
