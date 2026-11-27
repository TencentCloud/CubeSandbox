// Copyright © 2026 Tencent Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Consumer side of the template memory hotset profile ("hot-pages.json").
//
// At template-build time Cubelet records the memory pages a restored guest
// touches between resume and ready, and writes the extent list into the
// template snapshot state dir (next to the `memory-ranges` file). Here, at
// fast-restore time, we read the profile and pread(2) each extent into the
// page cache from a bounded worker pool, so guest first-touch faults hit
// cache instead of disk. pread is used rather than a readahead hint
// (fadvise/madvise) so delivery is complete on every kernel. Workers pull
// chunk-sized units from a shared queue and are cancelled when the VM goes
// away.
//
// The profile is a pure optimization. Any absence, corruption, validation
// failure or pread error downgrades to the plain restore path — restore
// must never fail or slow down measurably because of profiling.

use std::fs::{self, File};
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::FileTypeExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::Arc;

use serde::Deserialize;

/// Profile files live in the template snapshot state dir, next to the
/// `memory-ranges` file cube-runtime writes: `<pkg>/metadata/snapshot/`.
const PROFILE_FILENAME: &str = "hot-pages.json";
const SUPPORTED_VERSION: u32 = 1;
/// Runaway guard: prewarm reads at most 1/2 of the memory file. The worker
/// count bounds the IO depth; concurrent creates on one device multiply the
/// aggregate load.
const MAX_TOTAL_RATIO_NUM: u64 = 1;
const MAX_TOTAL_RATIO_DEN: u64 = 2;
/// Per-pread chunk: also the work unit handed to the workers, so a giant
/// extent cannot serialize behind one thread.
const PREAD_CHUNK_BYTES: usize = 1 << 20;
/// Worker threads when `CUBE_VMM_RESTORE_HOTSET_QD` is unset or unparsable.
const DEFAULT_WORKERS: usize = 8;
const MAX_WORKERS: usize = 64;
/// Number of prewarm worker threads; 1 restores the sequential behavior.
const ENV_HOTSET_WORKERS: &str = "CUBE_VMM_RESTORE_HOTSET_QD";

fn worker_count() -> usize {
    match std::env::var(ENV_HOTSET_WORKERS) {
        Ok(v) => v
            .trim()
            .parse::<usize>()
            .map(|n| n.clamp(1, MAX_WORKERS))
            .unwrap_or(DEFAULT_WORKERS),
        Err(_) => DEFAULT_WORKERS,
    }
}

/// Cancels the prewarm workers: set when the VM owning the restored memory
/// goes away (MemoryManager teardown, failed restore setup), so a short-lived
/// guest — e.g. the template-build verification sandboxes — does not keep
/// workers reading a memory volume nobody maps anymore.
pub struct PrewarmGuard {
    cancel: Arc<AtomicBool>,
}

impl PrewarmGuard {
    pub fn cancel(&self) {
        self.cancel.store(true, Ordering::Relaxed);
    }
}

/// `CUBE_VMM_RESTORE_HOTSET_DISABLE`: consume nothing, behave exactly like
/// the plain restore path.
fn disabled() -> bool {
    match std::env::var("CUBE_VMM_RESTORE_HOTSET_DISABLE") {
        Ok(v) => v == "1" || v.eq_ignore_ascii_case("true"),
        Err(_) => false,
    }
}

/// Profile path inside a snapshot state dir (sibling of `memory-ranges`).
fn profile_path_for(snapshot_dir: &Path) -> PathBuf {
    snapshot_dir.join(PROFILE_FILENAME)
}

#[derive(Debug, Deserialize)]
struct HotPagesProfile {
    version: u32,
    #[serde(default)]
    #[allow(dead_code)]
    template_id: String,
    /// Memory file (volume) size the profile was taken against.
    mem_file_size: u64,
    #[serde(default)]
    #[allow(dead_code)]
    profiled_at: String,
    /// [offset, len] byte ranges into the memory file.
    #[serde(default)]
    extents: Vec<[u64; 2]>,
}

fn validate(profile: &HotPagesProfile, actual_size: u64) -> Result<(), String> {
    if profile.version != SUPPORTED_VERSION {
        return Err(format!("unsupported version {}", profile.version));
    }
    if profile.mem_file_size != actual_size {
        return Err(format!(
            "mem_file_size {} != actual {}",
            profile.mem_file_size, actual_size
        ));
    }
    let mut total = 0u64;
    for extent in &profile.extents {
        let [offset, len] = *extent;
        if len == 0 {
            return Err("zero-length extent".to_string());
        }
        let end = offset
            .checked_add(len)
            .ok_or_else(|| "extent offset overflow".to_string())?;
        if end > actual_size {
            return Err(format!("extent [{offset},{len}) out of bounds"));
        }
        total = total
            .checked_add(len)
            .ok_or_else(|| "extent total overflow".to_string())?;
    }
    if total * MAX_TOTAL_RATIO_DEN > actual_size * MAX_TOTAL_RATIO_NUM {
        return Err(format!(
            "extent total {total} exceeds half of memory size {actual_size}"
        ));
    }
    Ok(())
}

/// One pread64, retried on EINTR and short reads. `Ok(bytes)` with
/// `bytes < buf.len()` means EOF was reached mid-request.
fn pread_once(file: &File, offset: u64, buf: &mut [u8]) -> io::Result<usize> {
    let mut filled = 0usize;
    while filled < buf.len() {
        // SAFETY: plain pread64 into a caller-owned buffer of the matching
        // length; kernel-only side effects.
        let n = unsafe {
            libc::pread64(
                file.as_raw_fd(),
                buf[filled..].as_mut_ptr().cast(),
                buf.len() - filled,
                offset.saturating_add(filled as u64) as libc::off64_t,
            )
        };
        if n < 0 {
            let e = io::Error::last_os_error();
            if e.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return Err(e);
        }
        if n == 0 {
            break; // EOF
        }
        filled += n as usize;
    }
    Ok(filled)
}

/// Split extents into chunk-sized work units. Extents stay the unit of the
/// profile (and of validation); chunks are only the workers' queue items.
fn split_chunks(extents: &[[u64; 2]]) -> Vec<[u64; 2]> {
    let mut chunks = Vec::new();
    for &[offset, len] in extents {
        let mut pos = offset;
        let mut left = len;
        while left > 0 {
            let n = (left as usize).min(PREAD_CHUNK_BYTES) as u64;
            chunks.push([pos, n]);
            pos += n;
            left -= n;
        }
    }
    chunks
}

/// Run the prewarm walk on a pool of worker threads pulling chunk-sized work
/// units from a shared queue. Returns the cancel guard and the coordinator's
/// join handle (dropped by the caller; joined by tests). The coordinator
/// joins the workers and logs the aggregate outcome, so the caller stays
/// fire-and-forget.
fn spawn_prewarm(
    file: File,
    extents: Vec<[u64; 2]>,
    total: u64,
) -> io::Result<(PrewarmGuard, std::thread::JoinHandle<()>)> {
    let chunks = Arc::new(split_chunks(&extents));
    let nchunks = chunks.len();
    let next = Arc::new(AtomicUsize::new(0));
    let cancel = Arc::new(AtomicBool::new(false));
    let done = Arc::new(AtomicU64::new(0));
    let incomplete = Arc::new(AtomicBool::new(false));

    let mut joins = Vec::new();
    for i in 0..worker_count() {
        let worker_file = match file.try_clone() {
            Ok(f) => f,
            Err(e) => {
                if joins.is_empty() {
                    return Err(e);
                }
                debug!("restore memory hotset: worker {i} fd clone failed: {e}");
                break;
            }
        };
        let worker = {
            let chunks = Arc::clone(&chunks);
            let next = Arc::clone(&next);
            let cancel = Arc::clone(&cancel);
            let done = Arc::clone(&done);
            let incomplete = Arc::clone(&incomplete);
            move || {
                let mut scratch = vec![0u8; PREAD_CHUNK_BYTES];
                loop {
                    if cancel.load(Ordering::Relaxed) {
                        return;
                    }
                    let idx = next.fetch_add(1, Ordering::Relaxed);
                    if idx >= chunks.len() {
                        return;
                    }
                    let [off, len] = chunks[idx];
                    let mut pos = off;
                    let mut left = len;
                    while left > 0 {
                        if cancel.load(Ordering::Relaxed) {
                            return;
                        }
                        let want = (left as usize).min(scratch.len());
                        match pread_once(&worker_file, pos, &mut scratch[..want]) {
                            Ok(n) if n == want => {
                                done.fetch_add(n as u64, Ordering::Relaxed);
                                pos += n as u64;
                                left -= n as u64;
                            }
                            Ok(_) => {
                                // EOF (should not happen: validate() bounds
                                // extents by the file size). Give up; the
                                // plain fault path covers the rest.
                                incomplete.store(true, Ordering::Relaxed);
                                return;
                            }
                            Err(e) => {
                                debug!("restore memory hotset: pread at {pos} failed: {e}");
                                incomplete.store(true, Ordering::Relaxed);
                                return;
                            }
                        }
                    }
                }
            }
        };
        match std::thread::Builder::new()
            .name(format!("hotset-prewarm{i}"))
            .spawn(worker)
        {
            Ok(join) => joins.push(join),
            Err(e) => {
                if joins.is_empty() {
                    return Err(e);
                }
                debug!("restore memory hotset: worker {i} spawn failed: {e}");
                break;
            }
        }
    }

    let coord_cancel = Arc::clone(&cancel);
    let coord_done = Arc::clone(&done);
    let coord_incomplete = Arc::clone(&incomplete);
    let guard_cancel = Arc::clone(&cancel);
    let coordinator = std::thread::Builder::new()
        .name("hotset-prewarm".to_string())
        .spawn(move || {
            let started = std::time::Instant::now();
            for join in joins {
                let _ = join.join();
            }
            let got = coord_done.load(Ordering::Relaxed);
            let ms = started.elapsed().as_millis();
            if coord_incomplete.load(Ordering::Relaxed) {
                warn!(
                    "restore memory hotset prewarm incomplete: {got} of {total} bytes in {nchunks} chunks, {ms} ms"
                );
            } else if coord_cancel.load(Ordering::Relaxed) {
                debug!(
                    "restore memory hotset prewarm cancelled after {got} of {total} bytes, {ms} ms"
                );
            } else {
                info!(
                    "restore memory hotset prewarm done: {got} bytes in {nchunks} chunks, {ms} ms"
                );
            }
        })
        .map_err(|e| {
            // The worker joins moved into the closure are dropped with it:
            // cancel so the already-spawned workers stop instead of draining
            // the whole profile uncancelled.
            guard_cancel.store(true, Ordering::Relaxed);
            e
        })?;

    Ok((PrewarmGuard { cancel }, coordinator))
}

/// Size of the opened memory target: `metadata().len()` for regular files,
/// lseek(SEEK_END) for block devices, whose st_size is always 0. lseek is
/// already on the VMM seccomp allowlist; a BLKGETSIZE64 ioctl is not.
fn memory_target_size(file: &File, meta: &fs::Metadata) -> u64 {
    if !meta.file_type().is_block_device() {
        return meta.len();
    }
    // SAFETY: plain lseek on an owned fd, no pointer arguments.
    let end = unsafe { libc::lseek(file.as_raw_fd(), 0, libc::SEEK_END) };
    if end > 0 {
        end as u64
    } else {
        0
    }
}

/// Kick page-cache prewarm for the guest memory hotset, if a valid profile
/// exists in `snapshot_dir` (the snapshot state dir the restore reads VM state
/// from). `memory_file` is the already-open memory file (volume) whose size
/// the profile is validated against.
///
/// Returns a guard cancelling the workers; the caller ties it to the restored
/// VM's lifetime. Fire-and-forget otherwise: failures are logged at
/// debug/warn and never propagated.
pub fn restore_prewarm(memory_file: &File, snapshot_dir: &Path) -> Option<PrewarmGuard> {
    if disabled() {
        debug!("restore memory hotset prewarm disabled");
        return None;
    }
    let profile_path = profile_path_for(snapshot_dir);

    let memory_meta = match memory_file.metadata() {
        Ok(m) => m,
        Err(e) => {
            debug!("restore memory hotset: memory file metadata failed: {e}");
            return None;
        }
    };

    if let Err(e) = fs::metadata(&profile_path) {
        // Absent profile is the normal case (templates built with the
        // profiling switch off): stay silent at debug level.
        debug!(
            "restore memory hotset: no profile at {}: {e}",
            profile_path.display()
        );
        return None;
    }

    let content = match fs::read_to_string(&profile_path) {
        Ok(c) => c,
        Err(e) => {
            debug!("restore memory hotset: profile unreadable: {e}");
            return None;
        }
    };
    let profile: HotPagesProfile = match serde_json::from_str(&content) {
        Ok(p) => p,
        Err(e) => {
            debug!("restore memory hotset: profile corrupt: {e}");
            return None;
        }
    };

    if let Err(reason) = validate(&profile, memory_target_size(&memory_file, &memory_meta)) {
        warn!(
            "restore memory hotset: profile rejected ({}): {reason}",
            profile_path.display()
        );
        return None;
    }

    if profile.extents.is_empty() {
        debug!("restore memory hotset: empty profile, nothing to warm");
        return None;
    }

    let total: u64 = profile.extents.iter().map(|e| e[1]).sum();
    let worker_file = match memory_file.try_clone() {
        Ok(f) => f,
        Err(e) => {
            debug!("restore memory hotset: cannot clone memory fd: {e}");
            return None;
        }
    };
    match spawn_prewarm(worker_file, profile.extents.clone(), total) {
        Ok((guard, _coordinator)) => {
            info!(
                "restore memory hotset prewarm kicked: {} extents, {} workers, {total} bytes",
                profile.extents.len(),
                worker_count()
            );
            Some(guard)
        }
        Err(e) => {
            debug!("restore memory hotset: cannot spawn prewarm workers: {e}");
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::time::SystemTime;

    // serialize tests that touch the process env or the shared temp dir
    static LOCK: Mutex<()> = Mutex::new(());

    fn profile(extents: &[[u64; 2]], mem_file_size: u64) -> HotPagesProfile {
        HotPagesProfile {
            version: SUPPORTED_VERSION,
            template_id: "tpl-x".to_string(),
            mem_file_size,
            profiled_at: "2026-09-20T03:00:00Z".to_string(),
            extents: extents.to_vec(),
        }
    }

    #[test]
    fn test_validate_accepts_good_profile() {
        let _g = LOCK.lock().unwrap();
        let p = profile(&[[0, 4096], [8192, 4096]], 1 << 20);
        assert_eq!(validate(&p, 1 << 20), Ok(()));
    }

    #[test]
    fn test_validate_rejects_bad_version() {
        let _g = LOCK.lock().unwrap();
        let mut p = profile(&[[0, 4096]], 1 << 20);
        p.version = 99;
        assert!(validate(&p, 1 << 20).is_err());
    }

    #[test]
    fn test_validate_rejects_size_mismatch() {
        let _g = LOCK.lock().unwrap();
        let p = profile(&[[0, 4096]], 1 << 20);
        assert!(validate(&p, (1 << 20) + 1).is_err());
    }

    #[test]
    fn test_validate_rejects_out_of_bounds_and_zero_len() {
        let _g = LOCK.lock().unwrap();
        let p = profile(&[[0, 4096], [(1 << 20) - 100, 4096]], 1 << 20);
        assert!(validate(&p, 1 << 20).is_err());
        let p = profile(&[[0, 0]], 1 << 20);
        assert!(validate(&p, 1 << 20).is_err());
        let p = profile(&[[u64::MAX - 10, 4096]], 1 << 20);
        assert!(validate(&p, 1 << 20).is_err());
    }

    #[test]
    fn test_validate_rejects_over_half_total() {
        let _g = LOCK.lock().unwrap();
        let half = 1 << 20;
        let p = profile(&[[0, half / 2 + 1]], half);
        assert!(validate(&p, half).is_err());
        // exactly half is allowed
        let p = profile(&[[0, half / 2]], half);
        assert_eq!(validate(&p, half), Ok(()));
    }

    #[test]
    fn test_memory_target_size_regular_file_is_st_size() {
        let _g = LOCK.lock().unwrap();
        let dir = temp_dir_case("size");
        let mem = write_file(&dir.join("mem.bin"), &vec![0u8; 12345]);
        assert_eq!(memory_target_size(&mem, &mem.metadata().unwrap()), 12345);
        fs::remove_dir_all(&dir).ok();
    }

    fn temp_dir_case(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "vmm-hotset-{tag}-{}",
            SystemTime::now()
                .duration_since(SystemTime::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn write_file(path: &Path, bytes: &[u8]) -> File {
        fs::write(path, bytes).unwrap();
        File::options().read(true).open(path).unwrap()
    }

    #[test]
    fn test_profile_path_sibling_naming() {
        let p = profile_path_for(Path::new("/data/tpl/tpl-a/metadata/snapshot"));
        assert_eq!(
            p,
            Path::new("/data/tpl/tpl-a/metadata/snapshot/hot-pages.json")
        );
    }

    #[test]
    fn test_restore_prewarm_missing_profile_is_noop() {
        let _g = LOCK.lock().unwrap();
        let dir = temp_dir_case("missing");
        let mem = write_file(&dir.join("mem.bin"), &vec![0u8; 8192]);
        // no profile file: must not panic, must not log errors
        restore_prewarm(&mem, &dir);
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_restore_prewarm_valid_profile_kicks() {
        let _g = LOCK.lock().unwrap();
        let dir = temp_dir_case("valid");
        let mem_path = dir.join("mem.bin");
        let mem = write_file(&mem_path, &vec![0xABu8; 1 << 20]);
        let profile_path = profile_path_for(&dir);
        fs::write(&profile_path, r#"{"version":1,"template_id":"t","mem_file_size":1048576,"profiled_at":"x","extents":[[0,4096],[8192,4096]]}"#).unwrap();
        restore_prewarm(&mem, &dir);
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_split_chunks_covers_all_bytes() {
        let extents = vec![[0, 2 * PREAD_CHUNK_BYTES as u64 + 123], [10 << 20, 4096]];
        let chunks = split_chunks(&extents);
        let total: u64 = extents.iter().map(|e| e[1]).sum();
        assert_eq!(chunks.iter().map(|c| c[1]).sum::<u64>(), total);
        assert!(chunks.iter().all(|c| c[1] <= PREAD_CHUNK_BYTES as u64));
        // chunk count: 3 from the split extent + 1 small one
        assert_eq!(chunks.len(), 4);
        // contiguity within each source extent
        assert_eq!(chunks[0][0] + chunks[0][1], chunks[1][0]);
        assert_eq!(chunks[1][0] + chunks[1][1], chunks[2][0]);
    }

    #[test]
    fn test_worker_count_env_clamped() {
        let _g = LOCK.lock().unwrap();
        for (raw, want) in [("0", 1), ("1", 1), ("8", 8), ("999", MAX_WORKERS)] {
            std::env::set_var(ENV_HOTSET_WORKERS, raw);
            assert_eq!(worker_count(), want, "env {raw}");
        }
        std::env::set_var(ENV_HOTSET_WORKERS, "notanumber");
        assert_eq!(worker_count(), DEFAULT_WORKERS);
        std::env::remove_var(ENV_HOTSET_WORKERS);
        assert_eq!(worker_count(), DEFAULT_WORKERS);
    }

    #[test]
    fn test_spawn_prewarm_walks_whole_profile() {
        let _g = LOCK.lock().unwrap();
        let dir = temp_dir_case("spawn");
        let mem = write_file(&dir.join("mem.bin"), &vec![0u8; 1 << 20]);
        let (guard, coordinator) =
            spawn_prewarm(mem.try_clone().unwrap(), vec![[0, 1 << 20]], 1 << 20).unwrap();
        coordinator.join().unwrap();
        assert!(!guard.cancel.load(Ordering::Relaxed));
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_spawn_prewarm_cancel_stops_the_walk() {
        let _g = LOCK.lock().unwrap();
        let dir = temp_dir_case("spawn-cancel");
        // 64 MiB, 64 chunks: far more than a worker can finish before the
        // cancel lands on the next scheduling quantum
        let mem = write_file(&dir.join("mem.bin"), &vec![0u8; 64 << 20]);
        let (guard, coordinator) = spawn_prewarm(
            mem.try_clone().unwrap(),
            vec![[0, 64 << 20]],
            64 << 20,
        )
        .unwrap();
        guard.cancel();
        coordinator.join().unwrap();
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_restore_prewarm_corrupt_is_noop() {
        let _g = LOCK.lock().unwrap();
        let dir = temp_dir_case("corrupt");
        let mem_path = dir.join("mem.bin");
        let mem = write_file(&mem_path, &vec![0u8; 8192]);
        fs::write(profile_path_for(&dir), "{not json").unwrap();
        restore_prewarm(&mem, &dir);
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_restore_prewarm_disabled_by_env() {
        let _g = LOCK.lock().unwrap();
        std::env::set_var("CUBE_VMM_RESTORE_HOTSET_DISABLE", "1");
        let dir = temp_dir_case("disabled");
        let mem_path = dir.join("mem.bin");
        let mem = write_file(&mem_path, &vec![0u8; 8192]);
        fs::write(
            profile_path_for(&dir),
            r#"{"version":1,"mem_file_size":8192,"extents":[[0,4096]]}"#,
        )
        .unwrap();
        // must return early without reading/validating anything
        restore_prewarm(&mem, &dir);
        std::env::remove_var("CUBE_VMM_RESTORE_HOTSET_DISABLE");
        fs::remove_dir_all(&dir).ok();
    }
}
