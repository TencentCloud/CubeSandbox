# Virtiofsd Migration Preserialization Speed-up: `StoreOnlyReconstructor` Design

> Author: Songqian Li &lt;sionli@tencent.com&gt;
> 
> Module: `hypervisor/virtiofsd/src/passthrough/device_state/preserialization.rs`

---

## 1. Background

During live migration, virtiofsd must write an `InodeMigrationInfo` **for every inode in the inode store** on the source side: either `RootNode`, or `Path { parent, filename, fullname }`. The destination relies on this information to `openat(2)` the corresponding file in the same shared directory by path.

```rust
pub(in crate::passthrough) enum InodeLocation {
    RootNode,
    Path { parent: StrongInodeReference, filename: String, fullname: String },
}
```

`PathReconstructor` recurses top-down through the **entire shared directory tree** (or, when the filter list is non-empty, down from each filter directory), giving a complexity of **O(shared tree / filter subtree size)**. When the shared directory holds tens of millions of files but the guest has only ever touched a few hundred, the preserialization phase blows the migration SLA.

**Goal**: bring the cost down from `O(on-disk tree)` to `O(inode store size)` while keeping the wire format and the destination-observable semantics identical to `PathReconstructor`.

---

## 2. Core Idea

Instead of digging down through the on-disk tree level by level, **iterate only the inode store itself**:

1. Every inode already holds a `FileOrHandle`, so `readlink("/proc/self/fd/<fd>")` yields its **absolute path** (for `FileOrHandle::File` this is a pure procfs read; for `Handle` it first materializes a temporary FD via `open_by_handle_at(2)`, which still stays bounded by inode-store size rather than on-disk tree size).
2. Sorting the `(inode_id, abs_path)` set **by path byte length, ascending** guarantees that every parent is processed strictly before its children.
3. When processing a child, strip the last component off `abs_path` to get `parent_path`, and reverse-look up the parent inode_id in an **incrementally built** `HashMap<Vec<u8>, Inode>`.

```
  ┌──────────────────┐   per inode      ┌────────────────────┐
  │   inode store    │ ───────────────▶ │  (id, abs_path)    │
  │   (all inodes)   │  readlink        │       list         │
  └──────────────────┘  /proc/self/fd   └────────┬───────────┘
                                                 │ sort by path.len()
                                                 ▼
                                        ┌────────────────────┐
                                        │    sorted list     │
                                        └────────┬───────────┘
                                                 │ stream through +
                                                 │ hash reverse lookup
                                                 ▼
                                        ┌────────────────────┐
                                        │ migration_info::   │
                                        │       Path         │
                                        └────────────────────┘
```

Complexity: **O(inode store size) + O(total depth of missing parent chains)**.

---

## 3. Selecting a Reconstructor

`StoreOnlyReconstructor` is the default; `PathReconstructor` is only used when the DFS switch is explicitly turned on:

```rust
// hypervisor/virtiofsd/src/passthrough/device_state/mod.rs
let result = if self.cfg.migration_dfs_preserialization {
    PathReconstructor::new(self, cancel).execute()
} else {
    StoreOnlyReconstructor::new(self, cancel).execute()
};
```

Configuration (ignored on the destination side):

- CLI: `--migration-dfs-preserialization`, with the compatibility aliases `--migration-legacy-reconstructor` and `--migration-no-store-only`
- `Config.migration_dfs_preserialization`, default `false`

One structural difference between the two: when the filter list is non-empty, `PathReconstructor` calls `recurse_from` on each filter **directory** and walks its whole subtree. `StoreOnlyReconstructor` does **not** perform that DFS — it only writes `Path { parent: root, filename, fullname }` for the filter inodes themselves, and lets descendants be filled in by the inode-store snapshot plus reverse lookup.

---

## 4. `execute()` Pipeline

```
  ┌─────────────────────────────────────────────────────────┐
  │ Phase 0  build_filter_resolved_map                      │
  │          filter_inode -> resolved_path map              │
  ├─────────────────────────────────────────────────────────┤
  │ Phase 1  set_root_info        root -> RootNode          │
  ├─────────────────────────────────────────────────────────┤
  │ Phase 2  set_filter_infos    filter -> Path(parent=root)│
  ├─────────────────────────────────────────────────────────┤
  │ Phase 3  inodes.map { get_path() }  single-pass snapshot│
  ├─────────────────────────────────────────────────────────┤
  │ Phase 4  entries.sort_by(path.len())                    │
  ├─────────────────────────────────────────────────────────┤
  │ Phase 5  seed path_to_inode  (root + filter)            │
  ├─────────────────────────────────────────────────────────┤
  │ Phase 6  main loop: stream reverse-lookup of parents    │
  │          + ensure_ancestor_chain fills missing links    │
  ├─────────────────────────────────────────────────────────┤
  │ Phase 7  drop(synthesized_refs)  release holding pen    │
  └─────────────────────────────────────────────────────────┘
```

---

## 5. Mechanisms in Execution Order

### 5.0 Two Data Structures That Span the Whole Flow

**(1) `path_to_inode: HashMap<Vec<u8>, Inode>`** — the incrementally built "path to inode" reverse lookup table. Keys are `Vec<u8>` rather than `String` so that non-UTF-8 paths are not mangled. Seeded in phase 5; phase 6 both consumes it and inserts newly discovered children into it as it goes.

**(2) `synthesized_refs: Vec<StrongInodeReference>`** — a `Vec` declared in the body of `execute()` whose lifetime **spans the entire main loop**, used to **keep alive the intermediate directory inodes that phase 6 materializes on demand**.

The crux: every time `ensure_ancestor_chain` materializes a new intermediate directory it gets back a `StrongInodeReference` with `refcount = 1`, and **that reference must survive until the main loop reaches `inodes.get_strong(parent_id)` for that inode** — otherwise the intermediate directory is evicted from the store before any descendant can reference it. `synthesized_refs` is the container that holds those references.

The helper `normalize_path_bytes` strips redundant trailing `/` from a path (except for a bare `/`). The parent slice computed in the phase 6 main loop, `abs_path[..rposition('/')]`, never carries a trailing slash, whereas seed sources (a filter `read_link` target, `get_path()` output) legitimately may. Without normalization the two sides never match, which **silently breaks the parent chain**.

---

### 5.1 Phase 0: Build the Migration-Local Filter Resolution Map

The filter table is typed as:

```rust
filter: RwLock<BTreeMap<Inode, (String, String)>>
//                        filename, original_path / fullname
```

**What it does**: build a `HashMap<Inode, Vec<u8>>` mapping each filter inode to the absolute host path bytes usable for seeding `path_to_inode`.

**Why it is needed**: in whitelist / host-dir setups, a filter entry's `original_path` is frequently an absolute path **outside the shared directory** (an NFS mount point, or a symlink pointing at a remote directory). Preserialization must obtain that host path (resolving the symlink target where applicable), otherwise descendants' parent slices land on the guest-visible path and fail to reverse-resolve.

**Implementation** (`build_filter_resolved_map`):

```rust
for (id, (_filename, original_path)) in filter_map.iter() {
    match std::fs::read_link(original_path.as_str()) {
        Ok(path_buf) => out.insert(*id, path_buf.into_os_string().into_vec()),
        Err(_) => out.insert(*id, original_path.as_bytes().to_vec()),
    }
}
```

- Filter entry is a symlink: `read_link` succeeds, and the target's raw OS bytes are used (no `to_string_lossy`, so non-UTF-8 targets are preserved).
- Not a symlink, broken link, or missing target: fall back to the `original_path` bytes, matching the "keep the original path on resolution failure" semantics.
- **Never written back into `fs.filter`**: the map lives only for the duration of this `execute()` call, so a cancelled or failed migration leaves no runtime residue.

---

### 5.2 Phases 1 & 2: Mark the Root and Filter Inodes

- Phase 1: `set_root_info` writes `InodeLocation::RootNode` on the root inode.
- Phase 2: `set_filter_infos` writes `InodeLocation::Path { parent: root, filename, fullname }` for every filter inode that can still be resolved via `get_strong`, where `fullname` preserves the filter entry's full guest-visible name.

Both are wire-format obligations and must complete before phase 5, since they provide the base anchors for seeding.

If `get_strong(*id)` fails, that filter is skipped and never enters `filter_inodes` — which is precisely why filter inodes must be pinned against guest FORGET; see section 6.

---

### 5.3 Phase 3: Single-Pass Snapshot of the Inode Store

Snapshot the absolute paths of **all inodes** in the store into a `Vec<(Inode, Vec<u8>)>` in one go:

```rust
let snapshots = self.fs.inodes.map(|inode_data| {
    if inode == ROOT || filter_inode_set.contains(inode) || FileOrHandle::Invalid {
        return Ok((inode, Vec::new())); // empty paths are dropped afterwards
    }
    match inode_data.get_path(proc_self_fd) {
        Ok(path) => Ok((inode_data.inode, path.into_bytes())),
        Err(err) => { /* empty path -> marked Invalid at serialization */ }
    }
});
```

**Key design points**:

1. **`inodes.map` takes the snapshot under a single read lock**, guaranteeing consistency: no inode is added or removed midway.
2. **Inodes that already have `migration_info` still participate in the snapshot** (their migration_info is not overwritten, but they must be seeded into `path_to_inode`). Otherwise their in-store descendants cannot find a parent in phase 6.
3. Entries unlinked on the host, for which procfs reports `"Inode deleted"`, are logged at `warn` level (so operators can correlate post-migration "missing inode" reports with on-host unlink activity); other `get_path` failures are logged at `debug`. Both become empty paths and are ultimately serialized as `Invalid`.
4. Cancellation is the only hard error this phase propagates upwards.

---

### 5.4 Phase 4: Sort by Path Length

`entries.sort_by(|a, b| a.1.len().cmp(&b.1.len()))`.

**Proof of the parent-before-child invariant**:

- Under POSIX, `child = parent + '/' + name` with a non-empty `name`;
- therefore `len(child) >= len(parent) + 2`;
- paths of equal length cannot be in an ancestor/descendant relationship, so ties may be ordered arbitrarily.

A single sort turns "parents before children" into a deterministic order with no topological sort required. Should mount-namespace aliasing surfacing through `/proc/self/fd` ever break this invariant, the affected descendants simply fall through to the "parent not found" branch and degrade into on-demand synthesis or a skip.

---

### 5.5 Phase 5: Seed `path_to_inode`

Load the root and all filter inodes into `path_to_inode` via **two overlaid seeding channels**:

1. The `filter_resolved_map` from phase 0 (the `read_link` result, or the `original_path` fallback);
2. **Additionally**, each filter inode's own `/proc/self/fd` path, as a fallback that covers cases where the "logical" path and the kernel's path disagree, e.g. bind mounts. This second channel uses `entry().or_insert`, so it never overwrites a key already written by the first.

Every key is normalized through `normalize_path_bytes` first.

---

### 5.6 Phase 6: Main Loop — Streaming Reverse Lookup With On-Demand Parent Synthesis

The heart of the algorithm.

#### 5.6.1 Main Loop Skeleton

```rust
for (inode_id, abs_path) in entries.iter() {
    let (parent_bytes, filename_bytes) = split_at_last_slash(abs_path);
    let parent_id = match path_to_inode.get(parent_bytes) {
        Some(id) => *id,
        None => {
            match ensure_ancestor_chain(parent_bytes, &mut path_to_inode, &mut synthesized_refs)? {
                true => *path_to_inode.get(parent_bytes).unwrap(),
                false => { path_to_inode.insert(abs_path.clone(), *inode_id); continue; }
            }
        }
    };
    let parent_ref = self.fs.inodes.get_strong(parent_id)?;
    // if migration_info already exists, do not overwrite it; just seed our own path
    // otherwise write Path { parent, filename, fullname: "" }
    path_to_inode.insert(abs_path.clone(), *inode_id);
}
```

Three things per iteration: **split the path**, **reverse-look up or synthesize the parent**, then **write migration_info and insert the current inode into the table** (it may itself become some descendant's parent later).

Error handling matches `PathReconstructor`'s fail-hard semantics:

- Non-UTF-8 filename: hard error under `migration_verify_handles`, otherwise `warn` + skip (serialized as `Invalid`).
- Inside `ensure_ancestor_chain`, a failed `openat` or a component containing NUL or invalid UTF-8: likewise a hard error under verify_handles, otherwise degraded to a skip.
- A non-directory encountered mid-path: abort that chain; the caller then marks the child as Invalid.

#### 5.6.2 Why `ensure_ancestor_chain` Is Needed

The guest may have forgotten a parent directory while still holding an fd on a file underneath it. In that case:

- the child inode is still in the store and `readlink` returns its full absolute path;
- the parent inode has been evicted from the store and is absent from `path_to_inode`;
- `PathReconstructor` would create the intermediate directory inode on the spot during its DFS;
- simply skipping would serialize the child as `Invalid`, so **the destination would permanently lose that inode** — a functional regression.

#### 5.6.3 The `ensure_ancestor_chain` Algorithm

Strategy: **walk upwards to locate the nearest anchor, then walk downwards materializing each level with `openat(O_PATH | O_NOFOLLOW)`**.

```
  Input: parent_bytes = "/share/a/b/c" (not in path_to_inode)
         nearest known anchor in path_to_inode = "/share/a"

  ┌─ Step A: walk upwards to locate the nearest anchor ────┐
  │   try "/share/a/b/c"  -> miss                          │
  │   try "/share/a/b"    -> miss                          │
  │   try "/share/a"      -> HIT (anchor)                  │
  │   collected missing = ["b", "c"]                       │
  └────────────────────────────────────────────────────────┘
                            │
                            ▼
  ┌─ Step B: walk downwards, materializing each component ─┐
  │   anchor_fd = open("/share/a", O_PATH|O_NOFOLLOW)      │
  │                                                        │
  │   for component in ["b", "c"]:                         │
  │       fd  = openat(anchor_fd, component, O_PATH)       │
  │       statx(fd) must be a directory                    │
  │       handle = name_to_handle_at(fd)                   │
  │       ┌─ claim_inode(handle, ids) ─┐                   │
  │       │  hit  -> reuse InodeData   │                   │
  │       │  miss -> get_or_insert(    │                   │
  │       │            full InodeData) │                   │
  │       └─────────────┬──────────────┘                   │
  │                     ▼                                  │
  │       write migration_info { parent = previous level } │
  │       holding_pen.push(strong_ref)   <- keeps it alive │
  │       path_to_inode.insert(next_path -> new_id)        │
  │       anchor_fd = fd                                   │
  └────────────────────────────────────────────────────────┘
                            │
                            ▼
  back in main loop: parent_bytes is now in path_to_inode
```

**Key design points**:

1. **`claim_inode` first**: if the same physical inode is already in the store via another path, reuse it; never create a duplicate `InodeData`, or the destination would see two inodes referencing the same on-disk object.
2. **Complete `InodeData`**: a synthesized node carries `file_or_handle` + `ids` + `mode` + `migration_info`, so its wire format is identical to an intermediate node created on the fly by `PathReconstructor`.
3. **Idempotent**: if a path has already been populated by another chain or by the main loop, rebase onto the existing inode and continue downwards.
4. **Cost O(missing_chain_depth)**, far below the DFS cost of O(subtree size).

#### 5.6.4 Why the Caller Must Own `synthesized_refs`

This is the least obvious lifetime constraint in the design, and it follows from the inode store's refcount semantics.

**Fact 1**: the `StrongInodeReference` returned by `claim_inode` / `get_or_insert` is the **only live strong reference** to that inode in the store — freshly materialized it has `refcount = 1`, so dropping it takes the refcount to zero and the inode is evicted immediately.

**Fact 2**: for the main loop to use that synthesized inode as some child's parent, it must call `inodes.get_strong(parent_id)` to obtain **its own** strong reference and store it in the child's `migration_info.location.parent`. That step is what actually makes the intermediate directory referenced by the serialization output.

**Counter-example (what happens without a holding pen)**:

```
inside ensure_ancestor_chain, for each component {
    let new_ref = claim_or_insert(...)?;   // refcount: 0 -> 1
    path_to_inode.insert(next_path, new_id);
    // function returns, new_ref goes out of scope
}                                           // <- drop(new_ref): refcount 1 -> 0
                                            //    inode is evicted from the store
back in the main loop:
let parent_ref = inodes.get_strong(parent_id)?;  // NotFound
                                                 // child skipped -> serialized as Invalid
```

**The fix**: park `new_ref` in the caller-provided `holding_pen: &mut Vec<StrongInodeReference>`:

```rust
holding_pen.push(new_ref);   // refcount stays at 1, but the lifetime moves to the caller
```

`synthesized_refs` is declared in the body of `execute()` with a scope that **strictly covers the entire main loop**, yielding this invariant:

| Point in time | Sources of the synthesized inode's refcount | Main loop `get_strong(parent_id)` |
|---|---|---|
| When `ensure_ancestor_chain` returns | `synthesized_refs` × 1 | — |
| While the main loop visits its descendants | `synthesized_refs` × 1 + `migration_info.parent` × k from descendants created so far | **always succeeds** |
| After the main loop, once phase 7 has dropped the pen | only `migration_info.parent` × k (survives if it has descendants; evicted at zero otherwise) | — |

**In one sentence**: `synthesized_refs` is a **temporary custody area** that holds synthesized inodes which have just been born but are not yet held by any serialization object, keeping them alive until the main loop finishes — so the main loop can always resolve them as parents, while phase 7 can flush in one go those that never gained a descendant.

---

### 5.7 Phase 7: `drop(synthesized_refs)`

Release the holding pen explicitly. **The timing is the other half of the design**: dropping too early makes `get_strong(parent_id)` fail, dropping too late wrongly serializes intermediate directories that have no descendants.

```
during the main loop    synthesized_refs holds N strong refs to synthesized inodes
        │
        ▼
main loop ends          each synthesized inode with descendants:
                          refcount = 1 (synthesized_refs)
                                   + k (migration_info.parent of k descendants)
                        each synthesized inode without descendants:
                          refcount = 1 (synthesized_refs) only
        │
        ▼
drop(synthesized_refs)
        │
        ▼
        each synthesized inode's refcount drops by 1:
          has descendants -> still >= 1, stays in the store, gets serialized
          no descendants  -> reaches 0, evicted, excluded from the output
```

This way, at serialization time `StoreOnlyReconstructor` only sees synthesized intermediate directories that were **genuinely referenced by some descendant through `migration_info.location.parent`** — semantically equivalent to `PathReconstructor`, where an intermediate node not held by anything simply disappears as the DFS unwinds.

---

## 6. Filter Inodes Must Be Pinned Against Guest FORGET

Phases 2 and 5 of `StoreOnlyReconstructor` rely on one invariant: every inode id in `self.filter` is **also** still alive in `inode_store`.

If a guest `FORGET` (including a bulk forget triggered by `drop_caches`) evicts a whitelisted directory or symlink from the store while `self.filter` retains the old id:

1. the next lookup allocates a fresh inode number, so the filter table ends up with two ids pointing at the same on-disk object;
2. the phase 5 `path_to_inode` seed is overwritten by the stale id;
3. `get_strong` fails for descendants, the source marks them `Invalid`, the destination reports `Migration source has lost inode N`, and every open handle on those children is rejected.

`PassthroughFs::forget` / `batch_forget` therefore **skip keys present in `self.filter` instead of forwarding them to `inodes.forget_one` / `forget_many`**. The whitelist is bounded (a few dozen entries at most) and each pin costs only an `O_PATH` fd plus one `InodeData`; guest semantics are unaffected because refcount bookkeeping is host-private anyway.

---

## 7. Summary

`StoreOnlyReconstructor` follows a single line of reasoning — **iterate only the inode store, let procfs report each path, sort and stream the reverse lookup, synthesize missing parents on demand** — to bring preserialization down from `O(shared directory tree)` to `O(inode store)`.

The details most worth remembering:

- **Strictly monotonic path length** is the foundation of the parent-before-child invariant.
- A **caller-owned holding pen** resolves the non-obvious lifetime constraint that strong references to on-demand synthesized inodes must survive across the main loop.
- **Phase 0 resolves filters locally for the migration only**, never writing back into the runtime filter table, so cancellation or failure leaves no residue.
- **Filter inodes must be pinned against FORGET**, or the `path_to_inode` seed gets poisoned by stale ids.
