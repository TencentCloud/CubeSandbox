// Regression tests for RAW discard backend eligibility.
//
// Product policy: opt-in RAW discard is only advertised for a regular file on
// an actual XFS filesystem or an actual fuse.juicefs mount; the asynchronous
// backend additionally requires io_uring Fallocate support. Any other opt-in
// fails closed, and the default stays disabled. The filesystem is taken from
// the real temporary file's fstatfs magic, so these tests are meaningful on
// any host volume. The JuiceFS-positive case lives in the env-gated
// juicefs_discard_eligibility integration test, which owns the actual mount.

use block_util::async_io::DiskFile;
use block_util::raw_async::RawFileDisk;
use block_util::raw_sync::RawFileDiskSync;
use io_uring::{opcode, IoUring};
use std::fs::File;
use std::os::unix::io::AsRawFd;
use vmm_sys_util::tempfile::TempFile;

/// Magic reported by fstatfs(2) for XFS.
const XFS_SUPER_MAGIC: u64 = 0x5846_5342;
/// Magic reported for any FUSE mount. Only a resolved fuse.juicefs mount is
/// eligible; this temp volume cannot distinguish it here, so a FUSE temporary
/// directory is not asserted either way.
const FUSE_SUPER_MAGIC: u64 = 0x6573_5546;

fn temp_file() -> (TempFile, File) {
    let temp = TempFile::new().expect("create temporary file");
    let file = temp.as_file().try_clone().expect("clone temporary file");
    (temp, file)
}

fn fs_magic(file: &File) -> u64 {
    let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
    // SAFETY: fstatfs writes into `stat` on success and the fd is valid.
    let ret = unsafe { libc::fstatfs(file.as_raw_fd(), stat.as_mut_ptr()) };
    assert_eq!(ret, 0, "fstatfs: {}", std::io::Error::last_os_error());
    // SAFETY: fstatfs succeeded above.
    unsafe { (*stat.as_ptr()).f_type as u64 }
}

/// Mirrors the capability the asynchronous backend is expected to require.
/// Unavailable io_uring or probe registration is a hard "not supported", not a
/// panic, so the test also covers hosts where io_uring is disabled.
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

#[test]
fn raw_sync_discard_is_off_by_default() {
    let (_temp, file) = temp_file();
    let backend = RawFileDiskSync::new(file);
    assert!(!backend.supports_discard());
    assert!(!backend.supports_discard());

    let (_temp, file) = temp_file();
    let backend = RawFileDiskSync::new(file).with_discard(false);
    assert!(!backend.supports_discard());
    assert!(!backend.supports_discard());
}

#[test]
fn raw_sync_discard_opt_in_requires_supported_regular_file() {
    let (_temp, file) = temp_file();
    let magic = fs_magic(&file);
    let backend = RawFileDiskSync::new(file).with_discard(true);
    let supported = backend.supports_discard();
    assert_eq!(
        supported,
        backend.supports_discard(),
        "sync discard capability must be stable across calls"
    );

    if magic == XFS_SUPER_MAGIC {
        assert!(supported, "XFS regular file must advertise sync discard");
    } else if magic == FUSE_SUPER_MAGIC {
        // This temp directory may itself be an actual fuse.juicefs mount; the
        // env-gated JuiceFS integration test owns that distinction.
    } else {
        assert!(
            !supported,
            "sync discard must fail closed on filesystem magic {magic:#x} \
             (XFS is {XFS_SUPER_MAGIC:#x}, FUSE is {FUSE_SUPER_MAGIC:#x})"
        );
    }
}

#[test]
fn raw_async_discard_is_off_by_default() {
    let (_temp, file) = temp_file();
    let backend = RawFileDisk::new(file);
    assert!(!backend.supports_discard());
    assert!(!backend.supports_discard());

    let (_temp, file) = temp_file();
    let backend = RawFileDisk::new(file).with_discard(false);
    assert!(!backend.supports_discard());
    assert!(!backend.supports_discard());
}

#[test]
fn raw_async_discard_opt_in_requires_supported_file_and_fallocate_opcode() {
    let (_temp, file) = temp_file();
    let magic = fs_magic(&file);
    let opcode_supported = io_uring_fallocate_supported();
    let backend = RawFileDisk::new(file).with_discard(true);
    let supported = backend.supports_discard();
    assert_eq!(
        supported,
        backend.supports_discard(),
        "async discard capability must be stable across calls"
    );

    if magic == XFS_SUPER_MAGIC {
        assert_eq!(
            supported, opcode_supported,
            "XFS async discard capability must follow actual io_uring Fallocate support"
        );
    } else if magic == FUSE_SUPER_MAGIC {
        // This temp directory may itself be an actual fuse.juicefs mount; the
        // env-gated JuiceFS integration test owns that distinction.
    } else {
        assert!(
            !supported,
            "async discard must fail closed on filesystem magic {magic:#x} \
             (XFS is {XFS_SUPER_MAGIC:#x}, FUSE is {FUSE_SUPER_MAGIC:#x}) even \
             though io_uring Fallocate is {opcode_supported}"
        );
    }
}

#[test]
fn raw_sync_reclaim_off_starts_no_discard_worker() {
    let (_temp, file) = temp_file();
    let backend = RawFileDiskSync::new(file).with_discard(false);
    let mut io = backend.new_async_io(8).expect("create AsyncIo");
    assert!(
        io.start_discard_worker().unwrap().is_none(),
        "a sync disk with writable-disk reclaim off must not start a discard worker"
    );
}

#[test]
fn raw_discard_opt_in_rejects_non_regular_files() {
    let dir = File::open(".").expect("open current directory");
    let backend = RawFileDiskSync::new(dir).with_discard(true);
    assert!(!backend.supports_discard());

    let dir = File::open(".").expect("open current directory");
    let backend = RawFileDisk::new(dir).with_discard(true);
    assert!(!backend.supports_discard());
}
