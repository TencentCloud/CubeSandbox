# SPDK patches

This directory holds the patches that **must be applied to SPDK**. s3lvol does
not fork SPDK; it maintains a minimal set of diffs here, each of which can be
explained on its own.

```sh
patches/apply.sh                 # apply (idempotent; already-applied ones are skipped)
patches/apply.sh --check         # check only; non-zero exit if any is missing
patches/apply.sh --reverse       # revert
SPDK_ROOT=/path/to/spdk patches/apply.sh
```

After applying, SPDK must be rebuilt (`make -C ../spdk -j$(nproc)`), otherwise
the headers are new but the libraries are old, which shows up as undefined
symbols at link time.

The patches are `git format-patch` shaped, with full commit messages and
Signed-off-by, so `git am` can consume them too -- use it if you want the
commits kept in the SPDK tree:

```sh
git -C ../spdk am /path/to/s3lvol/patches/0001-*.patch
```

`apply.sh` uses `git apply` (work tree only, no commits) because it has to be
repeatable and idempotent, which `git am` cannot be.

**Baseline**: `v26.09-pre-115-gd64c4fa89`. `apply.sh` does not check the
version; it checks whether the patches apply cleanly. A wrong version whose code
happened not to move still works, and a real conflict is reported plainly.

## Why not fork SPDK

After a fork, "what we changed" is buried in tens of thousands of commits, and
when rebasing to a new version nobody can say which changes are required and
which were convenient at the time. A directory plus a "why" paragraph per patch
is the only form that can still answer that question a year later. So every
patch here must satisfy:

1. **Only add, never modify.** New APIs are fine; changing the behaviour of an
   existing function is not -- such a change makes every other consumer of SPDK
   (our own app also links nvmf, bdev and accel) behave in ways we have not
   tested.
2. **The comments explain why upstream does not have it**, and why our problem
   cannot be solved with the existing API.
3. **It can be proposed upstream on its own.** If a patch is not suitable for
   upstream, that is usually a sign the design went astray.

0004 is the **only patch that modifies an existing function**; the reason for the
exception is written in its own section. Read that section before adding a
second such patch, and make sure the reason is of the same kind.

## 0001-blob-add-spdk_blob_get_io_unit_lba.patch

Adds `spdk_blob_get_io_unit_lba(blob, offset)`: translates an offset inside a
blob into the LBA on the blobstore device. About 20 lines, purely read-only, no
existing path touched.

**Why it is needed.** Our chunk map lives at the bs_dev layer and is keyed on
**device LBAs** (`include/s3lvol/s3_bs_dev.h`, section 5.3.2). Exporting a
snapshot to another node means "list the S3 objects this snapshot occupies and
hand them to the peer", which needs the chain
`blob offset -> device LBA -> object uuid`. The middle step is not in the public
API:

- `spdk_blob_get_next_allocated_io_unit()` only answers "which offsets are
  allocated", not where they land;
- the cluster table `blob->active.clusters[]` lives in `lib/blob/blobstore.h`,
  which is private.

**The cost of not having it, and why that is not acceptable.** The alternative
is `#include "lib/blob/blobstore.h"` and reading that table directly. It would
work, but it depends on the **memory layout** of a private struct: the day
upstream adds a field in the middle of `struct spdk_blob`, we would read wrong
LBAs -> the manifest points at someone else's objects -> the peer reads
**someone else's data**, and everything looks perfectly normal, no error at all.
That kind of bug makes no noise -- the kind we are least willing to carry.

With this API, if upstream changes the signature or removes it, we get a
**compile error**.

## 0002-blob-add-spdk_blob_materialize_cluster.patch

Adds `spdk_blob_materialize_cluster(blob, ch, cluster, cb, arg)`: allocates
**one** cluster of a thin blob and fills it from its back_bs_dev, exactly as a
write to that cluster would. About 160 lines, all additive, no existing path
touched.

**Why it is needed.** We want an imported volume to stop depending on the peer's
export, and the public API only has `spdk_bs_inflate_blob()` for that. For an
esnap clone it is **all-or-nothing**: `bs_inflate_blob_open_cpl()` forces
`allocate_all = true` (`blobstore.c:7287`), and `bs_cluster_needs_allocation()`
returns true for every cluster of an esnap clone (`:7195`), so the cost scales
with the **provisioned size** rather than the amount of data, and the volume
comes out thick. With the default geometry (1 MiB clusters, a 16 TiB lvstore), a
volume that only ever wrote 10 GiB would need 16.7 million cluster allocations
and would fill all 16 TiB of free space, just to stop reading those 10 GiB.

We happen to hold exactly the information "which clusters are worth
materialising": the `present` bitmap of the export manifest. With this API we
materialise only what the manifest says has data, leave the rest as holes, and
the volume stays thin.

**Why it cannot be assembled from the existing API in the module.** The only
alternative is "read an io_unit, write it back" to trigger CoW. That **loses
data under concurrent writes**: between the read and the write-back the user
writes new data, and we overwrite the old data over it, without a sound. The
blobstore's internal path does not have this problem -- CoW is a queued user op,
the losing side gets `-EEXIST` from the metadata insert, and
`blob_insert_cluster_cpl()` (`:2785`) frees its own cluster and **re-executes
the queued operation**. So this has to be done inside the blobstore; upstream
just never exposed the entry point.

**Why the API stands on its own.** It is the loop body of
`bs_inflate_blob_touch_next()`, made callable on its own. Any backend whose
back_bs_dev contents are described by the caller needs it -- upstream's own
comment (`:7281-7286`) admits that esnap inflate should look at
`back_bs_dev->is_zeroes()`, it is just that "no user has implemented a useful
is_zeroes() yet". We did (`s3_export_bs_dev.c:382`), and this patch is the other
half of putting that information to use.

Two caller-visible properties are documented in the function docs: an already
allocated cluster returns success immediately, so the materialisation loop
**can resume after an interruption**; and the concurrency safety above.

**Why it is longer than the loop body it came from.** Most of the size is one
correction. The operation given to `bs_allocate_and_copy_cluster()` is a
zero-length read -- a placeholder to be queued and re-executed once the cluster is
in place, with nothing left to do by then. But that function queues in a second
case as well: an allocation already in flight on the same channel, for some
*other* cluster (`blobstore.c:2880`). On that path nothing is allocated and
nothing is copied, and re-executing the placeholder does not repair it, since
`blob_request_submit_op()` completes a zero-length operation immediately and a
read of an unallocated cluster is served by the parent rather than allocating. A
write would have re-driven the allocation; a read cannot. So the completion checks
whether the cluster really arrived and offers it again if it did not.

This matters because a channel is **per thread per blobstore**: two volumes being
decoupled from one thread share one, and without the re-offer the second one loses
nearly every cluster to the first -- reported as success, with the parent then
dropped. Measured before the fix: 43 clusters, 43 queued, 43 "materialised", 40 KiB
read from S3 instead of 43 MiB, and an unmountable filesystem.

## 0003-blob-add-spdk_bs_blob_clear_external_parent.patch

Adds `spdk_bs_blob_clear_external_parent(bs, blob_id, cb, arg)`: removes a
blob's external snapshot parent **while staying thin**. About 95 lines of
additions, written in the shape of `spdk_bs_blob_set_external_parent()`,
reusing its cleanup/close callbacks, no existing function modified.

**Why it is needed, and why the existing API is not enough.** This is the other
half of 0002: after materialising the clusters we want to keep, the fact that
"this blob is a clone of some export" has to be removed from the metadata.
Switching the back device with `spdk_blob_set_esnap_bs_dev()` looks like it
would do -- **it does not**. It goes through
`blob_set_back_bs_dev(blob, dev, NULL, NULL, ...)`
(`spdk_blob_set_esnap_bs_dev()` at `blobstore.c:10650`), with `parent_refs_cb_fn`
as NULL, so the `BLOB_EXTERNAL_SNAPSHOT_ID` xattr and the
`SPDK_BLOB_EXTERNAL_SNAPSHOT` flag **stay**. It only swaps the device in memory,
and the next `spdk_lvs_load_ext()` still asks us for a parent by that uuid --
only by then the export has been released, so the lvstore fails to load. That
path is "looks effective, explodes on restart", exactly the kind we least want.

And `spdk_bs_inflate_blob()`, while persistent, also clears
`SPDK_BLOB_THIN_PROV` on the way (`bs_inflate_blob_done()`, `:7134`) -- that is
the source of the all-or-nothing cost described in 0002.

This patch does precisely the **inverse of `bs_set_external_parent_refs()`**:
clear the xattr, clear the flag, `parent_id = SPDK_BLOBID_INVALID`, unlink from
the clone list, swap the back_bs_dev to a zeroes dev, then sync md. **The one
thing it does not touch is THIN_PROV.**

During it, I/O is frozen (`blob_set_back_bs_dev()` does that itself), but this
is a metadata write, not a copy. A non-esnap clone returns `-EINVAL`, and
another locked operation in flight returns `-EBUSY` -- both before anything is
changed.

**Why this API stands on its own.** The blobstore already has `set_parent` /
`set_external_parent`, two public "change the parent" operations, but no "drop
the parent while staying thin". For local snapshots it is meaningless
(`decouple_parent` is that), but for external snapshots it is the only
operation that can express "I no longer need this external dependency" -- and
the data of an external snapshot lives in someone else's storage, so "fully
materialise" and "keep depending" should have had a third option between them.

(If the justification above reads like it repeats, the two "why" paragraphs
come from the patch's own commit message, which is deliberately kept close to
the code. The `spdk_blob_get_io_unit_lba` addition is the natural complement of
the bs_dev abstraction: the blobstore lets a backend supply custom storage
(`spdk_bs_dev`) but never tells it "which of your LBAs does this piece of data
land on", so any LBA-addressed backend (object store, dedup layer, tiering)
cannot describe a blob's contents without moving data. Its doc comment notes it
only makes sense for read-only blobs -- a writable blob's clusters can be
reallocated at any time.)

## 0004-bdev_aio-flush-with-fdatasync-rather-than-fsync.patch

Changes the `fsync()` in `bdev_aio_flush()` to `fdatasync()`. One line of code,
plus a comment.

**This is the only patch that violates rule 1 above**, so the reason has to be
solid.

**Why change it.** Measured (ext4 over a write-through virtio disk, 4 KiB
O_DIRECT writes, extents already in the written state):

| | p50 |
|---|---|
| write only, no sync | 647 us |
| write + `fdatasync` | **674 us** |
| write + `fsync` | **2177 us** |

The 1.5 ms is one jbd2 journal commit ext4 makes for the **inode timestamps**.
Making the data itself durable costs nothing (the device is write-through, and
`O_DSYNC` measures the same 674 us).

The cost is multiplied by two more factors:

1. The WAL treats write+flush as **one commit unit** (`s3_wal.c:361`; callers
   are acknowledged only from the flush completion), so it is paid on every
   commit;
2. `SPDK_BDEV_IO_TYPE_FLUSH` is a **synchronous syscall** in the aio module, not
   something that goes through the aio ring
   (`_bdev_aio_submit_request` in `bdev_aio.c`), so it is paid by **blocking
   the calling reactor** -- the poller sits in the kernel for the whole time.

For s3lvol that is the difference between about 440 and about 1480 WAL commits
per second, and the owner thread's blocking share dropping from about 68% to
about 4%.

**Why this is not a "behaviour change" in the sense of rule 1.** A bdev FLUSH
means "make written data readable after a crash", and `fdatasync` guarantees
exactly that, **including** block allocation and the unwritten-extent
conversion (that is a precondition for reading the data back; measured,
`fdatasync` on an unwritten extent still takes 2.2 ms, so it is definitely not
skipped). The extra part `fsync` does -- the inode timestamps -- **is not part
of the bdev abstraction**: a bdev's size does not change after creation, and no
caller can see an aio bdev's mtime through the bdev API. In other words, the
old code implemented block semantics with file semantics, and the extra flush
was invisible to every bdev user.

`bdev_aio.c` itself positions an aio bdev as a block device: it unconditionally
sets `fdisk->disk.write_cache = 1` ("I have a volatile write cache, flush me").
A block device flush never includes filesystem timestamps.

**Why not make it an option.** That was the first choice, but
`bdev_aio_create`'s RPC arguments are **auto-generated** from
`schema/schema.json` + `scripts/genrpc.py`, and `genrpc.py`'s `lint_c_code()`
cross-checks the C decoders against the schema. Adding a parameter would touch
six files -- `schema.json`, `bdev_aio.{c,h}`, `bdev_aio_rpc.c`,
`python/spdk/rpc/bdev.py`, `scripts/rpc.py` -- and `schema.json` is a huge file
whose diff collides easily, which directly conflicts with "a minimal set of
diffs, each explainable on its own". And the default would still be `fsync`,
which just makes the next person step on the same rake.

**Rule 3 still holds**: this change can be proposed upstream on its own; it
reads more like a bug fix.
