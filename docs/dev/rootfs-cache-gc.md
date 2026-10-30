# Worker rootfs cache GC

Cubelet's opt-in `artifact_cache_gc` evicts only `images.image_base_path/<artifact_id>/<artifact_id>.ext4`.
It is independent of containerd image GC and does not change template replica counts.

## Eligibility and safety

Only explicitly S3-backed artifacts qualify. CubeMaster adds `cube.master.rootfs.artifact.cacheable=true` to distribution and create requests. After successful use, Cubelet stores the stable download route, token, SHA256, size and last-use timestamp in an owner-readable local index.

Above the high watermark, GC processes idle files in LRU order until the low watermark or per-cycle byte limit. It probes the stable `/cube/template/artifact/download` route with a one-byte range request before eviction. Missing sources, unreadable remote data, size mismatches and unsafe paths are skipped. Old presigned S3 URLs are never used as proof of recoverability.

Runtime references protect running and paused sandboxes. Artifact leases cover creation, downloads and restoration through the create workflow. GC stays disabled until runtime recovery completes successfully. A new borrower waits for eviction, then checks the actual file and downloads with SHA256 verification if absent.

GC unlinks only `.ext4`, keeping `.vm`, `version`, recovery metadata, template snapshots, COW volumes, template definitions, S3 objects and artifact/placement rows. READY replica records remain unchanged, so eviction alone does not trigger replica backfill.

Real GC enables capacity reservations for concurrent downloads. A download must fit within the high watermark after existing reservations. Pressure triggers a sweep, then rejects the download if space remains insufficient. Unknown sizes without Content-Length are refused. Reservations conservatively count the entire file until download completion, including already allocated blocks.

Coordination is limited to one Cubelet process. Do not share its cache directory between writers or replace directories externally. Open files and reflink sharing may prevent blocks from being freed immediately. GC checks actual filesystem space, reporting planned bytes separately from the observed increase in available bytes. Other writers can still consume free space.

## Configuration

```toml
[plugins."io.cubelet.images-service.v1.images-service".artifact_cache_gc]
enabled = false
dry_run = true
interval = "1m"
high_percent = 80
low_percent = 70
idle_time = "1h"
max_bytes_per_cycle = 107374182400 # 100 GiB
source_manifest = ""
```

`enabled=true, dry_run=true` probes and reports candidates without unlinking or enabling download reservations. Source metadata is still written. Omitting `dry_run` keeps dry-run enabled. Set `dry_run=false` explicitly for actual eviction. Files larger than the cycle limit are skipped. Restart Cubelet after configuration changes.

## Adopting existing caches

Legacy files without recovery metadata are never automatically eligible. On a trusted management host with kubectl access, export current sources using a read-only query:

```bash
python3 scripts/export_rootfs_cache_sources.py \
  --namespace cube-system --pod cubesandbox-mysql-0 \
  --master-url http://MASTER_ADDRESS:8089 \
  --output /root/rootfs-cache-sources.json
```

Replace the address and pod information. The MySQL pod must already provide `MYSQL_ROOT_PASSWORD` and `MYSQL_DATABASE`. Only READY, non-deleted S3 rows are exported. The output contains credentials and has mode `0600`; never commit or print its contents. Copy it through your existing secure operations channel into a root-only worker directory and configure `source_manifest`.

Cubelet imports changed manifests, adopting only matching-size regular files, without downloading or rebuilding templates. First adoption starts the idle clock at the current time; subsequent imports preserve it. New requests refresh it. New artifacts register automatically after CubeMaster is upgraded; legacy artifacts can retain the manifest. Unregistered files remain protected.

## Rollout and limits

Start with one upgraded worker in dry-run mode. Check candidates, runtime protection and remote readability, then wait an idle period before enabling real eviction. Verify a cold artifact can download, create, pause and resume before expanding the rollout.

`rootfs cache GC` logs include `files`, `planned_bytes`, `freed_bytes`, `available_bytes` and `target_bytes`. Incomplete recovery blocks eviction until the underlying recovery errors are repaired. Unreadable sources or no eligible candidates can leave usage above the watermark. New creates may be rejected under pressure, and cold downloads increase creation latency.

TC build files, MySQL binlogs, snapshots and COW volumes need separate lifecycle management.
