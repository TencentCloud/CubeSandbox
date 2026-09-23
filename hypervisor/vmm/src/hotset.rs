// Copyright © 2026 Tencent Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Consumer side of the template memory hotset profile ("hot-pages.json").
//
// At template-build time the set of memory pages a restored guest touches
// between resume and ready is profiled from /proc/<pid>/pagemap over two
// verification restores (see Cubelet's AppSnapshot profiling phase); their
// intersection is written as an extent list into the template snapshot state
// dir (next to the `memory-ranges` file). Here, at fast-restore time, we read
// the profile and issue posix_fadvise(WILLNEED) per extent so guest
// first-touch faults hit page cache instead of stalling on disk reads.
//
// The profile is a pure optimization. Any absence, corruption, validation
// failure or fadvise error downgrades to the plain restore path — restore
// must never fail or slow down measurably because of profiling.

use std::fs::{self, File};
use std::os::fd::AsRawFd;
use std::path::{Path, PathBuf};

use serde::Deserialize;

/// Profile files live in the template snapshot state dir, next to the
/// `memory-ranges` file cube-runtime writes: `<pkg>/metadata/snapshot/`.
const PROFILE_FILENAME: &str = "hot-pages.json";
const SUPPORTED_VERSION: u32 = 1;
/// Runaway guard: prewarm I/O must stay a small fraction of the memory file
/// (sweeping the whole file competes with concurrent guest faults).
const MAX_TOTAL_RATIO_NUM: u64 = 1;
const MAX_TOTAL_RATIO_DEN: u64 = 2;

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

fn advise_willneed(memory_file: &File, offset: u64, len: u64) -> i32 {
    // SAFETY: plain fadvise on an owned fd; kernel-only side effects.
    unsafe {
        libc::posix_fadvise(
            memory_file.as_raw_fd(),
            offset as libc::off_t,
            len as libc::off_t,
            libc::POSIX_FADV_WILLNEED,
        )
    }
}

/// Kick async prewarm for the guest memory hotset, if a valid profile exists
/// in `snapshot_dir` (the snapshot state dir the restore reads VM state from).
/// `memory_file` is the already-open memory file (volume) whose size the
/// profile is validated against.
///
/// There is deliberately no consumer-side mtime freshness check: the memory
/// backing at restore time is an external volume (device nodes report
/// activation-local mtimes, meaningless against a build-time profile), and
/// the snapshot-dir file shape never carries a profile today. Staleness is
/// owned by the producer — the build flow deletes stale profiles
/// unconditionally before the metadata volume is sealed — with
/// `mem_file_size` equality and the schema version as backstops.
///
/// Fire-and-forget: fadvise is asynchronous, all failures are logged at
/// debug/warn and never propagated.
pub fn restore_prewarm(memory_file: &File, snapshot_dir: &Path) {
    if disabled() {
        debug!("restore memory hotset prewarm disabled");
        return;
    }
    let profile_path = profile_path_for(snapshot_dir);

    let memory_meta = match memory_file.metadata() {
        Ok(m) => m,
        Err(e) => {
            debug!("restore memory hotset: memory file metadata failed: {e}");
            return;
        }
    };

    if let Err(e) = fs::metadata(&profile_path) {
        // Absent profile is the normal case (templates built with the
        // profiling switch off): stay silent at debug level.
        debug!(
            "restore memory hotset: no profile at {}: {e}",
            profile_path.display()
        );
        return;
    }

    let content = match fs::read_to_string(&profile_path) {
        Ok(c) => c,
        Err(e) => {
            debug!("restore memory hotset: profile unreadable: {e}");
            return;
        }
    };
    let profile: HotPagesProfile = match serde_json::from_str(&content) {
        Ok(p) => p,
        Err(e) => {
            debug!("restore memory hotset: profile corrupt: {e}");
            return;
        }
    };

    if let Err(reason) = validate(&profile, memory_meta.len()) {
        debug!(
            "restore memory hotset: profile rejected ({}): {reason}",
            profile_path.display()
        );
        return;
    }

    let total: u64 = profile.extents.iter().map(|e| e[1]).sum();
    let mut skipped = 0usize;
    for extent in &profile.extents {
        if advise_willneed(memory_file, extent[0], extent[1]) != 0 {
            skipped += 1;
        }
    }
    if skipped == 0 {
        info!(
            "restore memory hotset prewarm kicked: {} extents, {} bytes",
            profile.extents.len(),
            total
        );
    } else {
        warn!(
            "restore memory hotset prewarm partial: {} extents, {} bytes, {} fadvise failed",
            profile.extents.len(),
            total,
            skipped
        );
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
