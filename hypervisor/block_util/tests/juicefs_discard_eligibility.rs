// JuiceFS backend regression tests for RAW discard.
//
// Product policy: opt-in discard on a regular file is advertised for an actual
// XFS filesystem OR an actual fuse.juicefs mount (no blanket FUSE). These tests
// only exercise a real JuiceFS 1.4.1 mount supplied through
// CUBE_RECLAIM_JUICEFS_DIR, so an unavailable mount is a clear skip and an
// arbitrary FUSE mount is never asserted to be supported. The mount itself is
// verified independently; these tests never treat a storage name as proof.
//
// JuiceFS serves the punched range as zeros from its local view immediately.
// Actual object-store release is governed by JuiceFS background compaction/GC,
// trash retention and shared references, none of which these tests exercise;
// an explicit compact happened to be used in the isolated fixture experiment
// but is not the only mechanism. st_blocks stays at the logical size, so the
// integrity checks assert logical length, the exact zeroed inner range and the
// exact untouched bytes, never host st_blocks or object-store usage.

use block_util::async_io::{AsyncIo, DiskFile};
use block_util::raw_async::RawFileDisk;
use block_util::raw_sync::RawFileDiskSync;
use io_uring::{opcode, IoUring};
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const JUICEFS_DIR_ENV: &str = "CUBE_RECLAIM_JUICEFS_DIR";

const FILE_SIZE: usize = 64 * 1024;
const DISCARD_OFFSET: usize = 16 * 1024;
const DISCARD_LEN: usize = 16 * 1024;
const COMPLETION_TIMEOUT: Duration = Duration::from_secs(30);

static UNIQUE: AtomicU64 = AtomicU64::new(0);

fn juicefs_dir() -> Option<PathBuf> {
    match std::env::var_os(JUICEFS_DIR_ENV) {
        Some(value) if !value.is_empty() => Some(PathBuf::from(value)),
        _ => {
            eprintln!(
                "SKIP {}: set it to a real JuiceFS 1.4.1 mount to run these tests",
                JUICEFS_DIR_ENV
            );
            None
        }
    }
}

fn pattern(offset: usize) -> u8 {
    ((offset.wrapping_mul(37).wrapping_add(11)) % 251) as u8
}

fn patterned_bytes(len: usize) -> Vec<u8> {
    (0..len).map(pattern).collect()
}

/// Removes only the unique files this test created, even on panic.
struct OwnedFiles(Vec<PathBuf>);

impl Drop for OwnedFiles {
    fn drop(&mut self) {
        for path in &self.0 {
            let _ = std::fs::remove_file(path);
        }
    }
}

fn unique_path(dir: &Path, suffix: &str) -> PathBuf {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock before epoch")
        .as_nanos();
    let serial = UNIQUE.fetch_add(1, Ordering::SeqCst);
    dir.join(format!(
        "block-util-jfs-discard-{}-{nanos}-{serial}.{suffix}",
        std::process::id()
    ))
}

fn copy_range(src: &File, dst: &File, len: u64) {
    // Explicit offsets: the parent's cursor is already at EOF after write_all,
    // so relying on file-position offsets would start the copy at EOF.
    let mut src_offset: libc::off_t = 0;
    let mut dst_offset: libc::off_t = 0;
    let mut remaining = len;
    while remaining > 0 {
        // SAFETY: both fds are valid for the call and both offsets point at
        // live stack locals for the duration of the syscall.
        let copied = unsafe {
            libc::copy_file_range(
                src.as_raw_fd(),
                &mut src_offset,
                dst.as_raw_fd(),
                &mut dst_offset,
                remaining as usize,
                0,
            )
        };
        if copied < 0 {
            let error = io::Error::last_os_error();
            if error.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            panic!("copy_file_range failed: {error}");
        }
        assert!(copied > 0, "copy_file_range made no progress");
        remaining -= copied as u64;
    }
}

/// Creates a patterned 64 KiB parent and an exact copy_file_range clone on the
/// JuiceFS mount, returning the cleanup guard and both owned paths.
fn prepare_pair(dir: &Path) -> (OwnedFiles, PathBuf, PathBuf) {
    assert!(dir.is_dir(), "{JUICEFS_DIR_ENV}={dir:?} is not a directory");

    let parent_path = unique_path(dir, "parent");
    let clone_path = unique_path(dir, "clone");
    let owned = OwnedFiles(vec![parent_path.clone(), clone_path.clone()]);
    let expected = patterned_bytes(FILE_SIZE);
    assert!(
        expected[DISCARD_OFFSET..DISCARD_OFFSET + DISCARD_LEN]
            .iter()
            .any(|byte| *byte != 0),
        "the discarded range must contain non-zero data so zeroed reads are meaningful"
    );

    let mut parent = OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .open(&parent_path)
        .expect("create parent file on JuiceFS");
    parent.write_all(&expected).expect("write parent pattern");
    parent.sync_all().expect("sync parent");

    let clone = OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .open(&clone_path)
        .expect("create clone file on JuiceFS");
    copy_range(&parent, &clone, FILE_SIZE as u64);
    clone.sync_all().expect("sync clone");

    let mut copied = Vec::new();
    File::open(&clone_path)
        .expect("reopen clone")
        .read_to_end(&mut copied)
        .expect("read clone");
    assert_eq!(copied, expected, "clone must start as an exact copy");

    (owned, parent_path, clone_path)
}

fn open_rw(path: &Path) -> File {
    OpenOptions::new()
        .read(true)
        .write(true)
        .open(path)
        .expect("open JuiceFS file")
}

/// Bounded wait: the backends must deliver a completion, not hang or fake one.
fn wait_for_completion(io: &mut dyn AsyncIo) -> Vec<(u64, i32)> {
    let deadline = Instant::now() + COMPLETION_TIMEOUT;
    loop {
        let completions = io.complete();
        if !completions.is_empty() {
            return completions;
        }
        match io.notifier().read() {
            Ok(_) => {}
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                std::thread::sleep(Duration::from_millis(5));
            }
            Err(error) => panic!("discard notifier failed: {error}"),
        }
        assert!(
            Instant::now() < deadline,
            "timed out waiting for discard completion after {COMPLETION_TIMEOUT:?}"
        );
    }
}

fn assert_discarded(parent_path: &Path, clone_path: &Path) {
    let expected = patterned_bytes(FILE_SIZE);

    assert_eq!(
        std::fs::metadata(clone_path).unwrap().len(),
        FILE_SIZE as u64,
        "discard must keep the clone's logical length"
    );
    assert_eq!(
        std::fs::metadata(parent_path).unwrap().len(),
        FILE_SIZE as u64,
        "the parent must not be touched"
    );

    let clone = std::fs::read(clone_path).expect("read clone after discard");
    assert_eq!(clone.len(), FILE_SIZE);
    assert_eq!(
        &clone[..DISCARD_OFFSET],
        &expected[..DISCARD_OFFSET],
        "bytes before the discarded range must be exact"
    );
    assert!(
        clone[DISCARD_OFFSET..DISCARD_OFFSET + DISCARD_LEN]
            .iter()
            .all(|byte| *byte == 0),
        "the discarded inner 16 KiB must read back as exact zeros"
    );
    assert_eq!(
        &clone[DISCARD_OFFSET + DISCARD_LEN..],
        &expected[DISCARD_OFFSET + DISCARD_LEN..],
        "bytes after the discarded range must be exact"
    );

    let parent = std::fs::read(parent_path).expect("read parent after discard");
    assert_eq!(
        parent, expected,
        "discard on the clone must not change the parent"
    );

    // Reuse: rewriting the punched range must return the replacement bytes
    // through JuiceFS COW, not stale data from the shared parent.
    let replacement = vec![0x5a; DISCARD_LEN];
    let mut clone = open_rw(clone_path);
    clone
        .seek(SeekFrom::Start(DISCARD_OFFSET as u64))
        .expect("seek to punched range");
    clone
        .write_all(&replacement)
        .expect("rewrite punched range");
    clone.sync_all().expect("sync rewritten clone");
    drop(clone);

    let mut patched = expected.clone();
    patched[DISCARD_OFFSET..DISCARD_OFFSET + DISCARD_LEN].copy_from_slice(&replacement);
    let mut reread = Vec::new();
    File::open(clone_path)
        .expect("reopen rewritten clone")
        .read_to_end(&mut reread)
        .expect("read rewritten clone");
    assert_eq!(
        reread, patched,
        "the punched range must read back exactly after reuse"
    );

    let parent = std::fs::read(parent_path).expect("read parent after reuse");
    assert_eq!(
        parent, expected,
        "reuse on the clone must not change the parent"
    );
}

#[test]
fn juicefs_raw_sync_opt_in_advertises_and_discards_inner_range() {
    let Some(dir) = juicefs_dir() else {
        return;
    };
    let (_owned, parent_path, clone_path) = prepare_pair(&dir);

    // Default off and explicit false stay off on JuiceFS too.
    assert!(!RawFileDiskSync::new(open_rw(&clone_path)).supports_discard());
    assert!(!RawFileDiskSync::new(open_rw(&clone_path))
        .with_discard(false)
        .supports_discard());

    let backend = RawFileDiskSync::new(open_rw(&clone_path)).with_discard(true);
    assert!(
        backend.supports_discard(),
        "an actual JuiceFS regular file must advertise sync discard after opt-in"
    );

    let mut io = backend.new_async_io(1).expect("create sync AsyncIo");
    let runner = io
        .start_discard_worker()
        .expect("start sync discard worker")
        .expect("JuiceFS sync backend must expose a discard worker");
    let worker = std::thread::spawn(move || runner().unwrap());
    io.discard(
        DISCARD_OFFSET as libc::off_t,
        DISCARD_LEN as libc::off_t,
        42,
    )
    .expect("submit sync discard");
    let completions = wait_for_completion(io.as_mut());
    assert_eq!(
        completions,
        vec![(42, 0)],
        "sync discard must complete successfully"
    );

    drop(io);
    worker.join().unwrap();
    drop(backend);
    assert_discarded(&parent_path, &clone_path);
}

#[test]
fn juicefs_raw_async_opt_in_advertises_and_discards_inner_range() {
    let Some(dir) = juicefs_dir() else {
        return;
    };
    let (_owned, parent_path, clone_path) = prepare_pair(&dir);

    assert!(
        io_uring_fallocate_supported(),
        "host io_uring must support IORING_OP_FALLOCATE; an unavailable opcode would \
         fake success if the async backend still advertised discard"
    );

    // Default off and explicit false stay off on JuiceFS too.
    assert!(!RawFileDisk::new(open_rw(&clone_path)).supports_discard());
    assert!(!RawFileDisk::new(open_rw(&clone_path))
        .with_discard(false)
        .supports_discard());

    let backend = RawFileDisk::new(open_rw(&clone_path)).with_discard(true);
    assert!(
        backend.supports_discard(),
        "an actual JuiceFS regular file must advertise async discard after opt-in \
         when io_uring Fallocate is supported"
    );

    let mut io = backend
        .new_async_io(8)
        .expect("create async io_uring backend");
    io.discard(
        DISCARD_OFFSET as libc::off_t,
        DISCARD_LEN as libc::off_t,
        43,
    )
    .expect("submit async discard");
    let completions = wait_for_completion(io.as_mut());
    assert_eq!(
        completions,
        vec![(43, 0)],
        "async discard must complete successfully"
    );

    drop(io);
    drop(backend);
    assert_discarded(&parent_path, &clone_path);
}

#[test]
fn juicefs_raw_discard_opt_in_rejects_directories() {
    let Some(dir) = juicefs_dir() else {
        return;
    };
    assert!(!RawFileDiskSync::new(File::open(&dir).unwrap())
        .with_discard(true)
        .supports_discard());
    assert!(!RawFileDisk::new(File::open(&dir).unwrap())
        .with_discard(true)
        .supports_discard());
}

/// Mirrors the capability the asynchronous backend is expected to require.
fn io_uring_fallocate_supported() -> bool {
    let Ok(ring) = IoUring::new(1) else {
        return false;
    };
    let mut probe = io_uring::Probe::new();
    if ring.submitter().register_probe(&mut probe).is_err() {
        return false;
    }
    probe.is_supported(opcode::Fallocate::CODE)
}
