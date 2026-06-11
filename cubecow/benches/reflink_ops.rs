// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// End-to-end control-plane benchmarks for the **xfs-reflink** backend
// of cubecow, driven against a real `dyn Engine` instance.
//
// What this bench measures (cf. `src/engine/reflink.rs`):
//
//   1. **Single-source serial fanout** — same origin volume, N
//      back-to-back `create_snapshot` calls. The reflink backend has
//      no per-origin rate limit, so this exercises the FICLONE hot
//      path under pure metadata pressure.
//
//   2. **Chained snapshots** — snap_i = snap(snap_{i-1}) at depth D.
//      All clones land flattened in the same volume directory (the
//      reflink backend rewrites the link target to the *ultimate
//      origin* on every chain link), so this measures whether deep
//      chains incur extra metadata overhead in the kernel's reflink
//      bookkeeping.
//
//   3. **Multi-worker concurrent fanout** — M workers create M·N
//      snapshots of the same origin in parallel. This is where the
//      backend's `RwLock<name_index>` (write-locked around the name
//      reservation step in `create_snapshot`) and the per-volume
//      `fsync_dir` contend with each other. A reasonable backend
//      should scale near-linearly until disk fsync becomes the
//      bottleneck; the bench reports both per-op latency p50/p99 and
//      aggregate throughput so degradation is visible from either
//      angle.
//
//   4. **Dirty-IO interleave** — between every `create_snapshot`
//      invocation we issue a small random pwrite() against the source
//      volume's main file. This is the regression / jitter test the
//      caller asked for: FICLONE on a file whose pages have been
//      dirtied since the last clone may need to flush dirty extents
//      before sharing them, so the second-and-later snapshots of a
//      "hot" volume can be slower than a clean one. Reporting both
//      the clean-baseline (scenario #1) and the dirty-interleaved
//      latency side by side lets you spot CoW penalties immediately.
//
// Hand-rolled p50/p90/p99 (no criterion) keeps the dependency tree
// slim. Root + xfsprogs + losetup are required; the bench exits early
// with a skip message if the environment can't support it.
//
// Run:
//     sudo cargo bench --bench reflink_ops
//
// Knobs:
//     REFLINK_BENCH_ITERS=<n>         per-scenario iterations (default 1000)
//     REFLINK_BENCH_POOL_GB=<n>       loop-disk size (default 4)
//     REFLINK_BENCH_VOL_BYTES=<n>     source volume size in bytes
//                                     (default 64 MiB; large enough
//                                     that the random pwrite scenario
//                                     dirties more than one page)
//     REFLINK_BENCH_CHAIN_DEPTH=<n>   chain depth (default 200, set 0 to skip)
//     REFLINK_BENCH_CONCURRENCY=a,b,c worker counts to sweep
//                                     (default "1,4,8,16"; "0" skips)
//     REFLINK_BENCH_PER_WORKER=<n>    per-worker snap count for concurrent
//                                     scenarios (default 200)
//     REFLINK_BENCH_DIRTY_BYTES=<n>   bytes of pwrite per snap iteration
//                                     in scenario 4 (default 1 MiB).
//                                     Set 0 to skip the dirty scenario.
//     REFLINK_BENCH_DIRTY_BLOCK=<n>   block size for the random writes
//                                     (default 4096; uses one pwrite()
//                                     per block at a pseudo-random
//                                     aligned offset)
//     REFLINK_BENCH_KEEP=1            skip teardown so artefacts can be
//                                     inspected after the run
//
// All scenarios run inside a single `ReflinkBench` instance, which
// means a single XFS(reflink=1) loop mount serves all four — no
// re-mount cost between scenarios. Each scenario uses a disjoint
// origin-name namespace so they don't collide, and explicit cleanup
// happens at the end of each scenario function.

use std::fs::{File, OpenOptions};
use std::os::unix::fs::FileExt;
use std::path::PathBuf;
use std::process::Command;
use std::sync::{Arc, Barrier, Mutex};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use cubecow::config::AppConfig;
use cubecow::Engine;

// ---------------------------------------------------------------------------
// Knobs
// ---------------------------------------------------------------------------

fn env_usize(key: &str, default: usize) -> usize {
    std::env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default)
}

fn env_u64(key: &str, default: u64) -> u64 {
    std::env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default)
}

fn env_list_usize(key: &str, default: &str) -> Vec<usize> {
    let raw = std::env::var(key).unwrap_or_else(|_| default.to_string());
    raw.split(',')
        .filter_map(|s| s.trim().parse().ok())
        .filter(|n: &usize| *n > 0)
        .collect()
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

fn percentile(mut s: Vec<Duration>, p: f64) -> Duration {
    s.sort();
    if s.is_empty() {
        return Duration::ZERO;
    }
    let idx = ((s.len() as f64 - 1.0) * p).round() as usize;
    s[idx]
}

fn fmt(d: Duration) -> String {
    let us = d.as_secs_f64() * 1_000_000.0;
    if us >= 1000.0 {
        format!("{:.3} ms", us / 1000.0)
    } else {
        format!("{us:.1} µs")
    }
}

/// Single-pass stats line. Not the wall-clock throughput interpreter:
/// `samples.len() / sum(samples)` measures the *latency-implied*
/// ops/sec assuming serial submission. For the concurrent scenario we
/// also print a separate wall-clock throughput at the call site, since
/// per-op latency does not capture parallel speedup.
fn report(label: &str, samples: &[Duration]) {
    if samples.is_empty() {
        return;
    }
    let p50 = percentile(samples.to_vec(), 0.50);
    let p90 = percentile(samples.to_vec(), 0.90);
    let p99 = percentile(samples.to_vec(), 0.99);
    let max = samples.iter().copied().max().unwrap_or_default();
    let mean: Duration = samples.iter().copied().sum::<Duration>() / samples.len() as u32;
    let total_secs = samples.iter().copied().sum::<Duration>().as_secs_f64();
    let ops_per_sec = samples.len() as f64 / total_secs.max(1e-9);
    println!(
        "reflink_ops  {label:<48} iters={:<5} p50={} p90={} p99={} max={} mean={} latency_ops/s={:.1}",
        samples.len(),
        fmt(p50),
        fmt(p90),
        fmt(p99),
        fmt(max),
        fmt(mean),
        ops_per_sec,
    );
}

// ---------------------------------------------------------------------------
// Bench harness — owns the XFS(reflink=1) loop mount and the engine.
//
// Layout:
//
//     /tmp/reflink-bench-<tag>.img      sparse-allocated backing file
//     /dev/loopN                         attached via `losetup -fP`
//     /tmp/reflink-bench-mnt-<tag>/      XFS(reflink=1) mount, also the
//                                        engine's `[backend.reflink].root_dir`
//
// `Drop` makes a best-effort umount + losetup -d + rm. Errors are
// warned, never panicked, so a failed scenario still leaves the host
// clean.
// ---------------------------------------------------------------------------

struct ReflinkBench {
    /// Live engine handle, behind a `dyn Engine` so the same driver
    /// code could later target other reflink-style backends. `None`
    /// only inside `Drop` once the engine has been moved out.
    engine: Option<Arc<dyn Engine>>,
    /// Root dir cubecow uses for the reflink layout.
    /// `<root_dir>/volumes/<vol>/<vol>` is the FICLONE source for snap.
    root_dir: PathBuf,
    /// Loop device backing the XFS mount; needed by Drop's teardown.
    loop_dev: String,
    /// Backing file for the loop device.
    backing_file: PathBuf,
    /// If `keep_on_drop` is true, leave loop+mount+files in place so
    /// the operator can inspect them. Driven by `REFLINK_BENCH_KEEP`.
    keep_on_drop: bool,
}

impl ReflinkBench {
    /// Provision a fresh XFS(reflink=1) loop mount, build a JSON config
    /// pointing the reflink backend at it, and boot the engine through
    /// the public `cubecow::initialize_without_logging` factory.
    ///
    /// Returns `Err` (rather than panicking) on any environmental
    /// failure so `main()` can print a friendly skip message and exit
    /// with success.
    fn setup(pool_gb: u64) -> Result<Self, String> {
        require_root()?;
        require_binaries(&[
            "losetup", "mkfs.xfs", "mount", "umount", "findmnt", "truncate",
        ])?;

        // Unique per-run tag: pid + nanos so artefacts from concurrent
        // runs don't collide.
        let tag = unique_suffix();
        let backing_file = PathBuf::from(format!("/tmp/reflink-bench-{tag}.img"));
        let mount_point = PathBuf::from(format!("/tmp/reflink-bench-mnt-{tag}"));
        std::fs::create_dir_all(&mount_point).map_err(|e| format!("create mount_point: {e}"))?;

        // Sparse `truncate`: a 4 GiB declared size only costs a few
        // MiB of real disk because every block we never touch stays
        // unallocated.
        run(
            "truncate",
            &["-s", &format!("{pool_gb}G"), backing_file.to_str().unwrap()],
        )?;

        let loop_dev = run_stdout(
            "losetup",
            &["-fP", "--show", backing_file.to_str().unwrap()],
        )?
        .trim()
        .to_string();
        if !std::path::Path::new(&loop_dev).exists() {
            return Err(format!("losetup returned non-existent device {loop_dev}"));
        }

        // `-m reflink=1,crc=1` is the canonical FICLONE-capable XFS
        // recipe. crc=1 has been the xfsprogs default since ~4.x but
        // we name it explicitly so the recipe is robust to default
        // drift. -f forces past stale signatures, -q hides the banner.
        run(
            "mkfs.xfs",
            &["-f", "-q", "-m", "reflink=1,crc=1", &loop_dev],
        )?;
        run(
            "mount",
            &[
                "-t",
                "xfs",
                "-o",
                "noatime,nouuid",
                &loop_dev,
                mount_point.to_str().unwrap(),
            ],
        )?;

        // Sanity-probe FICLONE *before* booting the engine — gives a
        // clearer error than the engine's `probe_reflink_support`
        // would when the kernel silently disabled reflink.
        {
            let src = mount_point.join(".reflink-bench-probe-src");
            let dst = mount_point.join(".reflink-bench-probe-dst");
            File::create(&src).map_err(|e| format!("probe create src: {e}"))?;
            let probe = Command::new("cp")
                .args([
                    "--reflink=always",
                    src.to_str().unwrap(),
                    dst.to_str().unwrap(),
                ])
                .status();
            let _ = std::fs::remove_file(&src);
            let _ = std::fs::remove_file(&dst);
            match probe {
                Ok(s) if s.success() => {}
                Ok(s) => {
                    return Err(format!(
                        "FICLONE probe failed on {} (cp --reflink exited {s})",
                        mount_point.display()
                    ));
                }
                Err(e) => return Err(format!("FICLONE probe spawn failed: {e}")),
            }
        }

        // Render the reflink-backend config inline.
        let config_json = format!(
            r#"{{
                "log": {{ "level": "off" }},
                "backend": {{
                    "kind": "reflink",
                    "reflink": {{ "root_dir": "{}" }}
                }}
            }}"#,
            mount_point.display(),
        );

        let config = AppConfig::from_json_str(&config_json)
            .map_err(|e| format!("config build failed: {e}"))?;

        // Public, backend-agnostic factory: returns Box<dyn Engine>
        // selected by `config.backend.kind`. We assert .kind = reflink
        // upstream (we built the JSON ourselves), so this always lands
        // in `ReflinkEngine::initialize_without_logging`.
        let engine_box = cubecow::initialize_without_logging(config)
            .map_err(|e| format!("engine init failed: {e}"))?;
        // Box<dyn Engine> -> Arc<dyn Engine> for cheap cross-thread
        // sharing in the concurrent scenario.
        let engine: Arc<dyn Engine> = Arc::from(engine_box);

        Ok(Self {
            engine: Some(engine),
            root_dir: mount_point,
            loop_dev,
            backing_file,
            keep_on_drop: std::env::var("REFLINK_BENCH_KEEP")
                .map(|v| v != "0" && !v.is_empty())
                .unwrap_or(false),
        })
    }

    fn engine(&self) -> &Arc<dyn Engine> {
        self.engine
            .as_ref()
            .expect("ReflinkBench::engine called after teardown")
    }

    /// Path to the *main file* of a volume, i.e. the FICLONE source.
    /// Mirrors `ReflinkEngine::vol_main_file`.
    fn vol_main_file(&self, vol: &str) -> PathBuf {
        self.root_dir.join("volumes").join(vol).join(vol)
    }
}

impl Drop for ReflinkBench {
    fn drop(&mut self) {
        // Drop engine first so its internal locks / file handles are
        // released before we yank the mount out.
        self.engine.take();

        if self.keep_on_drop {
            eprintln!(
                "REFLINK_BENCH_KEEP=1 — preserving loop={} mount={} backing={}",
                self.loop_dev,
                self.root_dir.display(),
                self.backing_file.display(),
            );
            return;
        }

        // Best-effort umount with a few retries — XFS log can take a
        // moment to drain after the engine releases its file handles.
        // Only fall back to lazy umount when the normal one really
        // failed; otherwise running both prints a confusing
        // `umount: ...: not mounted` to stderr after the first one
        // already succeeded.
        let mut umount_ok = false;
        for _ in 0..10 {
            let st = Command::new("umount").arg(&self.root_dir).status();
            if matches!(st, Ok(s) if s.success()) {
                umount_ok = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        if !umount_ok {
            // Lazy umount as a last resort so the loop device can
            // detach even if something inside is still pinned.
            let _ = Command::new("umount")
                .args(["-l"])
                .arg(&self.root_dir)
                .status();
        }

        // Detach loop device.
        for _ in 0..10 {
            let st = Command::new("losetup")
                .args(["-d", &self.loop_dev])
                .status();
            if matches!(st, Ok(s) if s.success()) {
                break;
            }
            std::thread::sleep(Duration::from_millis(100));
        }

        let _ = std::fs::remove_dir(&self.root_dir);
        let _ = std::fs::remove_file(&self.backing_file);
    }
}

// ---------------------------------------------------------------------------
// Helpers shared across scenarios
// ---------------------------------------------------------------------------

/// Volume size for all bench scenarios. Default 64 MiB: large enough
/// that the dirty-IO scenario can write a meaningful amount of data
/// across multiple pages, small enough that `create_volume` (which
/// `fallocate`s the main file) stays well under 100 ms even on slow
/// loop disks.
fn vol_bytes() -> u64 {
    env_u64("REFLINK_BENCH_VOL_BYTES", 64 * 1024 * 1024)
}

/// Open the source volume's main file for direct pwrite() — used by
/// the dirty-IO scenario. We open with O_RDWR + O_DSYNC so each
/// pwrite is a durable write that actually dirties FICLONE-shared
/// extents, which is what we want to measure. (Without O_DSYNC the
/// kernel can satisfy reads from page-cache without ever splitting
/// shared extents, which would mask the FICLONE-on-dirty cost.)
fn open_volume_for_dirty_io(path: &std::path::Path) -> Result<File, String> {
    use std::os::unix::fs::OpenOptionsExt;
    OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(libc::O_DSYNC)
        .open(path)
        .map_err(|e| format!("open {} for dirty-IO: {e}", path.display()))
}

/// Pseudo-random aligned offset in `[0, vol_size - block)`. We use a
/// linear-congruential generator seeded by the iteration counter so
/// results are reproducible across runs. `xorshift32` would also work;
/// LCG is the smallest snippet that gives non-clustered offsets.
fn pseudo_offset(seed: u64, block: u64, vol_size: u64) -> u64 {
    // Numerical Recipes constants. Bottom bits are well-mixed for our
    // purposes since we mask immediately after.
    let x = seed.wrapping_mul(1_664_525).wrapping_add(1_013_904_223);
    let blocks = (vol_size / block).max(1);
    (x % blocks) * block
}

// ---------------------------------------------------------------------------
// Scenario 1 — single-source serial fanout
// ---------------------------------------------------------------------------

/// Take `n` back-to-back snapshots of a single origin volume. No I/O
/// between snaps: pure FICLONE + name-index + fsync_dir. This is the
/// "ideal world" baseline against which scenarios 3 and 4 are compared.
///
/// Implementation note: reflink has no per-origin rate limit (cf.
/// `src/engine/reflink.rs::create_snapshot`), so we don't need any
/// "fresh origin per iteration" workaround.
fn bench_single_source_serial(bench: &ReflinkBench, n: usize) {
    let engine = bench.engine();
    let origin = "single-src-origin";

    engine
        .create_volume(origin, vol_bytes())
        .expect("scenario 1: create_volume");

    // Warm-up: amortise allocator + first-FICLONE-on-this-fs cost.
    for i in 0..16 {
        let snap = format!("single-src-warm-{i}");
        engine
            .create_snapshot(origin, &snap, false)
            .expect("scenario 1 warm-up create_snapshot");
    }

    let mut create_samples = Vec::with_capacity(n);
    for i in 0..n {
        let snap = format!("single-src-snap-{i}");
        let t = Instant::now();
        engine
            .create_snapshot(origin, &snap, false)
            .expect("scenario 1 create_snapshot");
        create_samples.push(t.elapsed());
    }
    report(
        "create_snapshot single-source serial (clean)",
        &create_samples,
    );

    // Delete latency is also interesting on its own. Same hot loop,
    // same source.
    let mut delete_samples = Vec::with_capacity(n);
    for i in 0..n {
        let snap = format!("single-src-snap-{i}");
        let t = Instant::now();
        engine
            .delete_snapshot(&snap)
            .expect("scenario 1 delete_snapshot");
        delete_samples.push(t.elapsed());
    }
    report("delete_snapshot single-source serial", &delete_samples);

    // Drop warm-ups too, then the origin.
    for i in 0..16 {
        let _ = engine.delete_snapshot(&format!("single-src-warm-{i}"));
    }
    let _ = engine.delete_volume(origin);
}

// ---------------------------------------------------------------------------
// Scenario 2 — chained snapshot creation
// ---------------------------------------------------------------------------

/// `snap_i = snap(snap_{i-1})` for i in 1..=depth. Reflink rewrites
/// the destination path to the *ultimate origin*'s directory on every
/// chain link, so all `chain-N` snapshots end up under
/// `volumes/chain-base/`. Latency should stay flat across the chain
/// because FICLONE is O(extents-shared), not O(chain-depth) — but if
/// the kernel's reflink bookkeeping degrades with depth, this is the
/// scenario where it surfaces.
fn bench_chained(bench: &ReflinkBench, depth: usize) {
    if depth == 0 {
        return;
    }
    let engine = bench.engine();
    let base = "chain-base";

    engine
        .create_volume(base, vol_bytes())
        .expect("scenario 2: create_volume");

    let mut prev = base.to_string();
    let mut samples = Vec::with_capacity(depth);
    for i in 0..depth {
        let name = format!("chain-{i}");
        let t = Instant::now();
        engine
            .create_snapshot(&prev, &name, false)
            .expect("scenario 2 create_snapshot");
        samples.push(t.elapsed());
        prev = name;
    }
    report(
        &format!("create_snapshot chained (depth={depth})"),
        &samples,
    );

    // Tear down in reverse to be safe — although for reflink any
    // order works (snapshots are independent files once cloned).
    for i in (0..depth).rev() {
        let _ = engine.delete_snapshot(&format!("chain-{i}"));
    }
    let _ = engine.delete_volume(base);
}

// ---------------------------------------------------------------------------
// Scenario 3 — concurrent fanout against a shared origin
// ---------------------------------------------------------------------------

/// `threads` workers each take `per_worker` snapshots of the same
/// origin volume in parallel. The bench reports per-op latency
/// p50/p99 over all `threads * per_worker` samples *and* a
/// wall-clock throughput in ops/sec — they tell complementary
/// stories:
///
///   * latency p99 grows when the engine's per-op critical sections
///     (write-locking `name_index`, fsync_dir on the volume directory)
///     start to serialise workers;
///   * wall-clock throughput plateaus / regresses when the underlying
///     filesystem can't keep up with parallel FICLONE+fsync streams.
///
/// Returns `(samples, total_wall_time)` so the caller can compose a
/// throughput-vs-concurrency table after running multiple thread
/// counts.
fn run_concurrent_fanout(
    bench: &ReflinkBench,
    threads: usize,
    per_worker: usize,
) -> (Vec<Duration>, Duration) {
    assert!(threads >= 1);
    let engine = bench.engine();
    let origin = format!("conc-{threads}-origin");

    engine
        .create_volume(&origin, vol_bytes())
        .expect("scenario 3 create_volume");

    let barrier = Arc::new(Barrier::new(threads));
    let samples_shared: Arc<Mutex<Vec<Duration>>> =
        Arc::new(Mutex::new(Vec::with_capacity(threads * per_worker)));

    let mut handles = Vec::with_capacity(threads);

    let wall_start = Instant::now();
    for tid in 0..threads {
        let engine = Arc::clone(engine);
        let barrier = Arc::clone(&barrier);
        let samples_shared = Arc::clone(&samples_shared);
        let origin = origin.clone();

        handles.push(thread::spawn(move || {
            let mut local: Vec<Duration> = Vec::with_capacity(per_worker);
            // Sync the workers so we measure parallel cost, not serial
            // start-up jitter.
            barrier.wait();
            for i in 0..per_worker {
                // Disjoint name namespace per thread so no two workers
                // race for the same `name_index` key (we want
                // contention on the *lock*, not collision on a key —
                // the latter would just turn into AlreadyExists errors
                // and skew the bench).
                let snap = format!("conc-{threads}-t{tid}-s{i}");
                let t = Instant::now();
                engine
                    .create_snapshot(&origin, &snap, false)
                    .expect("scenario 3 create_snapshot");
                local.push(t.elapsed());
            }
            samples_shared.lock().unwrap().extend(local);
        }));
    }
    for h in handles {
        h.join().expect("worker panicked");
    }
    let wall = wall_start.elapsed();

    let samples = Arc::try_unwrap(samples_shared)
        .map(|m| m.into_inner().unwrap())
        .unwrap_or_else(|arc| arc.lock().unwrap().clone());

    // Cleanup: best-effort. Snapshots have unique names so this is
    // straightforward.
    for tid in 0..threads {
        for i in 0..per_worker {
            let _ = engine.delete_snapshot(&format!("conc-{threads}-t{tid}-s{i}"));
        }
    }
    let _ = engine.delete_volume(&origin);

    (samples, wall)
}

/// Run scenario 3 across the configured concurrency sweep and print
/// (a) per-thread-count `report` lines, and (b) a final scaling table
/// that puts wall-clock throughput next to per-op p99 — the two
/// numbers a reviewer needs to decide whether the backend scales.
fn bench_concurrent_sweep(bench: &ReflinkBench, sweep: &[usize], per_worker: usize) {
    if sweep.is_empty() {
        return;
    }
    let mut summary: Vec<(usize, Duration, Duration, f64)> = Vec::new(); // (threads, p50, p99, ops_per_s)
    for &threads in sweep {
        let label = format!("create_snapshot concurrent threads={threads} per_worker={per_worker}");
        let (samples, wall) = run_concurrent_fanout(bench, threads, per_worker);
        report(&label, &samples);
        let total_ops = samples.len() as f64;
        let throughput = total_ops / wall.as_secs_f64().max(1e-9);
        let p50 = percentile(samples.clone(), 0.50);
        let p99 = percentile(samples, 0.99);
        println!(
            "  └─ wall_clock={:.3} s  throughput={:.1} ops/s",
            wall.as_secs_f64(),
            throughput,
        );
        summary.push((threads, p50, p99, throughput));
    }

    if summary.len() <= 1 {
        return;
    }

    // Scaling table — each row references the threads=1 baseline so
    // the marginal effect of more concurrency is obvious. If the user
    // didn't include threads=1 in the sweep, fall back to the smallest
    // configured count.
    println!();
    println!(
        "# concurrency scaling (relative to threads={}):",
        summary[0].0
    );
    println!(
        "  {:<10}  {:<12}  {:<12}  {:<14}  {:<10}",
        "threads", "p50", "p99", "throughput", "speedup"
    );
    let base_throughput = summary[0].3;
    for (threads, p50, p99, throughput) in &summary {
        let speedup = throughput / base_throughput.max(1e-9);
        println!(
            "  {:<10}  {:<12}  {:<12}  {:<14}  {:<10.2}",
            threads,
            fmt(*p50),
            fmt(*p99),
            format!("{:.1} ops/s", throughput),
            speedup,
        );
    }
}

// ---------------------------------------------------------------------------
// Scenario 4 — dirty-IO interleave (jitter / regression detector)
// ---------------------------------------------------------------------------

/// Between each `create_snapshot`, write `dirty_bytes` of pseudo-random
/// pwrite()s to the source volume's main file. This dirties FICLONE-
/// shared extents from prior snapshots, so the next FICLONE may have
/// to flush dirty extents before re-sharing them — exactly the
/// "stability under load" pattern the caller asked us to probe.
///
/// Reports both the latency *distribution* (p50/p90/p99/max) and
/// flags whether the latency drifted upward over the run by computing
/// p99 of the first half of samples vs the second half. A backend
/// that's bookkeeping-stable should have first-half p99 ≈
/// second-half p99; a regressor will show the second half noticeably
/// slower.
fn bench_dirty_io_interleave(bench: &ReflinkBench, n: usize, dirty_bytes: u64, block: u64) {
    if n == 0 || dirty_bytes == 0 {
        return;
    }
    let engine = bench.engine();
    let origin = "dirty-origin";
    let vol_size = vol_bytes();

    engine
        .create_volume(origin, vol_size)
        .expect("scenario 4 create_volume");

    let main_path = bench.vol_main_file(origin);
    let f = open_volume_for_dirty_io(&main_path).expect("scenario 4 open volume for dirty-IO");

    // Pre-fill a single block of payload data once. We pwrite the same
    // buffer at different offsets each iteration; the *content* doesn't
    // matter for the bench, only the act of dirtying extents does.
    let payload = vec![0xa5u8; block as usize];

    // Warm-up: a few clean snaps + a few dirty snaps so allocator and
    // page-cache state are stable before measurement.
    for i in 0..8 {
        engine
            .create_snapshot(origin, &format!("dirty-warm-{i}"), false)
            .expect("scenario 4 warm create_snapshot");
    }

    let mut samples = Vec::with_capacity(n);
    let mut writes_per_iter = (dirty_bytes / block).max(1) as usize;
    if dirty_bytes % block != 0 {
        // Round up so the caller's "dirty_bytes" is a *minimum*, not a
        // floor that's silently truncated.
        writes_per_iter += 1;
    }

    for i in 0..n {
        // Dirty step: writes_per_iter pwrites at pseudo-random aligned
        // offsets. We seed with `i * writes_per_iter + j` so each
        // iteration touches a different set of blocks (some overlap
        // with prior iterations is intentional — it exercises the
        // worst-case "re-clone an already-dirtied extent" path).
        for j in 0..writes_per_iter {
            let seed = (i as u64).wrapping_mul(writes_per_iter as u64) + j as u64;
            let off = pseudo_offset(seed, block, vol_size);
            f.write_all_at(&payload, off)
                .expect("scenario 4 pwrite dirty");
        }
        // Snap step (measured).
        let snap = format!("dirty-snap-{i}");
        let t = Instant::now();
        engine
            .create_snapshot(origin, &snap, false)
            .expect("scenario 4 create_snapshot");
        samples.push(t.elapsed());
    }

    report(
        &format!(
            "create_snapshot dirty-interleave (dirty={} KiB block={} KiB)",
            dirty_bytes / 1024,
            block / 1024,
        ),
        &samples,
    );

    // Stability check: split samples in half by chronological order
    // (NOT by sorted latency) and compare p99s. Drift here = the
    // backend or the kernel's reflink bookkeeping is degrading with
    // each dirty cycle.
    if samples.len() >= 8 {
        let half = samples.len() / 2;
        let first: Vec<_> = samples[..half].to_vec();
        let second: Vec<_> = samples[half..].to_vec();
        let first_p99 = percentile(first.clone(), 0.99);
        let second_p99 = percentile(second.clone(), 0.99);
        let first_mean: Duration = first.iter().copied().sum::<Duration>() / first.len() as u32;
        let second_mean: Duration = second.iter().copied().sum::<Duration>() / second.len() as u32;
        let drift_pct = if first_p99.as_nanos() > 0 {
            (second_p99.as_secs_f64() / first_p99.as_secs_f64() - 1.0) * 100.0
        } else {
            0.0
        };
        println!(
            "  stability: first_half p99={} mean={}  | second_half p99={} mean={}  | drift={:+.1}% (p99)",
            fmt(first_p99),
            fmt(first_mean),
            fmt(second_p99),
            fmt(second_mean),
            drift_pct,
        );
    }

    // Cleanup.
    drop(f);
    for i in 0..8 {
        let _ = engine.delete_snapshot(&format!("dirty-warm-{i}"));
    }
    for i in 0..n {
        let _ = engine.delete_snapshot(&format!("dirty-snap-{i}"));
    }
    let _ = engine.delete_volume(origin);
}

// ---------------------------------------------------------------------------
// Shell-out helpers
// ---------------------------------------------------------------------------

fn run(bin: &str, args: &[&str]) -> Result<(), String> {
    let st = Command::new(bin)
        .args(args)
        .status()
        .map_err(|e| format!("failed to spawn {bin}: {e}"))?;
    if !st.success() {
        return Err(format!("{bin} {args:?} exited with {st}"));
    }
    Ok(())
}

fn run_stdout(bin: &str, args: &[&str]) -> Result<String, String> {
    let out = Command::new(bin)
        .args(args)
        .output()
        .map_err(|e| format!("failed to spawn {bin}: {e}"))?;
    if !out.status.success() {
        return Err(format!(
            "{bin} {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        ));
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

fn require_root() -> Result<(), String> {
    // SAFETY: libc::geteuid is always safe.
    let euid = unsafe { libc::geteuid() };
    if euid != 0 {
        return Err(format!("requires root (euid=0), got euid={euid}"));
    }
    Ok(())
}

fn require_binaries(bins: &[&str]) -> Result<(), String> {
    for b in bins {
        let st = Command::new("sh")
            .args(["-c", &format!("command -v {b} >/dev/null 2>&1")])
            .status()
            .map_err(|e| format!("probe for {b}: {e}"))?;
        if !st.success() {
            return Err(format!("missing prerequisite binary: {b}"));
        }
    }
    Ok(())
}

fn unique_suffix() -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!("{}_{nanos}", std::process::id())
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

fn main() {
    println!("# cubecow reflink-backend control-plane benchmark");
    println!("# Scenarios:");
    println!("#   1. single-source serial (FICLONE clean baseline)");
    println!("#   2. chained snapshots (depth scan)");
    println!("#   3. multi-worker concurrent fanout (lock + fsync contention)");
    println!("#   4. dirty-IO interleave (jitter / drift detector)");
    println!();

    let pool_gb = env_u64("REFLINK_BENCH_POOL_GB", 4);
    let n = env_usize("REFLINK_BENCH_ITERS", 1000);
    let chain_depth = env_usize("REFLINK_BENCH_CHAIN_DEPTH", 200);
    let conc_sweep = env_list_usize("REFLINK_BENCH_CONCURRENCY", "1,4,8,16");
    let per_worker = env_usize("REFLINK_BENCH_PER_WORKER", 200);
    let dirty_bytes = env_u64("REFLINK_BENCH_DIRTY_BYTES", 1024 * 1024);
    let dirty_block = env_u64("REFLINK_BENCH_DIRTY_BLOCK", 4096);

    println!(
        "# knobs: pool_gb={pool_gb} iters={n} vol_bytes={} chain_depth={chain_depth} \
         concurrency={:?} per_worker={per_worker} dirty_bytes={dirty_bytes} \
         dirty_block={dirty_block}",
        vol_bytes(),
        conc_sweep,
    );
    println!();

    let bench = match ReflinkBench::setup(pool_gb) {
        Ok(b) => b,
        Err(e) => {
            // Skip-and-exit-clean: CI hosts without root / xfsprogs /
            // loop-mount support see a friendly message instead of a
            // build failure.
            eprintln!("[skip] {e}");
            return;
        }
    };

    bench_single_source_serial(&bench, n);
    println!();

    if chain_depth > 0 {
        bench_chained(&bench, chain_depth);
        println!();
    }

    if !conc_sweep.is_empty() {
        bench_concurrent_sweep(&bench, &conc_sweep, per_worker);
        println!();
    }

    if dirty_bytes > 0 {
        bench_dirty_io_interleave(&bench, n, dirty_bytes, dirty_block);
        println!();
    }

    // Final sanity: print a metrics snapshot as the closing line so
    // stdout includes a quick "yep, the engine is alive" marker even
    // when every scenario was filtered out by env knobs.
    let metrics = bench.engine().metrics();
    println!("# final metrics:");
    let mut keys: Vec<&String> = metrics.keys().collect();
    keys.sort();
    for k in keys {
        println!("    {k} = {}", metrics[k]);
    }
}
