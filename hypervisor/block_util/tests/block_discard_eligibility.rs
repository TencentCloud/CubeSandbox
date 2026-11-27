// Block-device DISCARD tests against a parent-owned loop fixture.
//
// Product policy: opt-in discard on an opened block device is advertised only
// when the device is really a block device and its sysfs queue reports a
// positive discard_max_bytes. The parent owns the fixture and exports:
//
//   CUBE_RECLAIM_BLOCK_DEVICE        path to the loop block device
//   CUBE_RECLAIM_BLOCK_BACKING_FILE  path to its loop backing file
//
// Both are required. When either is absent the tests print a clear SKIP and
// return; an absent fixture is never reported as proven integration. When both
// are present the fixture is validated before any write: the device must have
// block metadata, /sys/dev/block/<major>:<minor> must report the same dev_t,
// the sysfs loop/backing_file must resolve to the exact supplied backing path,
// and that path must live under a dedicated cube-reclaim-poc-block-* marker in
// this checkout. No /dev path is guessed and a storage name is never proof.
//
// This validates local block plumbing only. It makes no claim about remote S3
// object release, clone/restart behavior, or production S3 capacity. Tests must
// run serially (--test-threads=1) because they share the owned fixture ranges.

use block_util::async_io::{
    discard_alignment_from_granularity, discard_limits_from_sysfs, AsyncIo, DiskFile,
};
use block_util::raw_async::RawFileDisk;
use block_util::raw_sync::RawFileDiskSync;
use std::fs::{File, OpenOptions};
use std::io;
use std::os::unix::fs::{FileExt, FileTypeExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, Instant};

const DEVICE_ENV: &str = "CUBE_RECLAIM_BLOCK_DEVICE";
const BACKING_ENV: &str = "CUBE_RECLAIM_BLOCK_BACKING_FILE";
const BACKING_MARKER_PREFIX: &str = "cube-reclaim-poc-block-";

const BODY_OFFSET: u64 = 1 << 20; // 1 MiB
const BODY_LEN: usize = 64 * 1024;
const DISCARD_OFFSET: u64 = BODY_OFFSET + 16 * 1024;
const DISCARD_LEN: usize = 16 * 1024;
const MIN_DEVICE_LEN: u64 = 4 * 1024 * 1024;
const COMPLETION_TIMEOUT: Duration = Duration::from_secs(30);

// The tests share the parent-owned copy of the fixture ranges; keep them
// serialized even if the harness is invoked without --test-threads=1.
static SERIAL_FIXTURE: Mutex<()> = Mutex::new(());

struct Fixture {
    device: PathBuf,
    backing: PathBuf,
    sysfs: PathBuf,
}

fn env_path(name: &str) -> Option<PathBuf> {
    match std::env::var_os(name) {
        Some(value) if !value.is_empty() => Some(PathBuf::from(value)),
        _ => None,
    }
}

/// Validates the parent-owned fixture. Present-but-wrong env is a hard failure;
/// only absent env is a skip.
fn fixture() -> Option<Fixture> {
    let Some(device) = env_path(DEVICE_ENV) else {
        eprintln!(
            "SKIP {}: set it (and {}) to an owned loop block fixture to run these tests",
            DEVICE_ENV, BACKING_ENV
        );
        return None;
    };
    let Some(backing) = env_path(BACKING_ENV) else {
        eprintln!(
            "SKIP {}: set it (and {}) to an owned loop block fixture to run these tests",
            BACKING_ENV, DEVICE_ENV
        );
        return None;
    };

    let metadata = std::fs::metadata(&device).unwrap_or_else(|error| {
        panic!("{DEVICE_ENV}={device:?} cannot be stat'ed: {error}");
    });
    assert!(
        metadata.file_type().is_block_device(),
        "{DEVICE_ENV}={device:?} is not actually a block device"
    );

    // Derive the sysfs identity from the opened device's own dev_t, never from
    // the /dev name, and require the reported dev_t to match it back.
    let rdev = metadata.rdev();
    let (major, minor) = (libc::major(rdev), libc::minor(rdev));
    let sysfs = std::fs::canonicalize(format!("/sys/dev/block/{major}:{minor}"))
        .unwrap_or_else(|error| panic!("no sysfs entry for block device {major}:{minor}: {error}"));
    let reported = std::fs::read_to_string(sysfs.join("dev"))
        .unwrap_or_else(|error| panic!("read {sysfs:?}/dev: {error}"));
    assert_eq!(
        reported.trim(),
        format!("{major}:{minor}"),
        "sysfs dev_t for {:?} does not match its block metadata",
        device
    );

    // The sysfs loop/backing_file must resolve to the exact second env path.
    let backing_in_sysfs =
        std::fs::read_to_string(sysfs.join("loop/backing_file")).unwrap_or_else(|error| {
            panic!(
                "{:?} is not a loop device with backing_file: {error}",
                device
            )
        });
    let sysfs_backing = std::fs::canonicalize(backing_in_sysfs.trim()).unwrap_or_else(|error| {
        panic!(
            "sysfs loop backing_file {:?} cannot be resolved: {error}",
            backing_in_sysfs.trim()
        )
    });
    let explicit_backing = std::fs::canonicalize(&backing).unwrap_or_else(|error| {
        panic!("{BACKING_ENV}={backing:?} cannot be resolved: {error}");
    });
    assert_eq!(
        sysfs_backing, explicit_backing,
        "{BACKING_ENV} does not match the loop device's actual backing file"
    );

    // Only a dedicated, ignored marker inside this checkout may be written.
    let repo_root = std::fs::canonicalize(env!("CARGO_MANIFEST_DIR"))
        .and_then(|path| {
            path.parent()
                .and_then(Path::parent)
                .map(Path::to_path_buf)
                .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "no repo root"))
        })
        .expect("resolve repository root");
    assert!(
        explicit_backing.starts_with(&repo_root),
        "refusing to write: backing file {:?} is outside the repository root {:?}",
        explicit_backing,
        repo_root
    );
    assert!(
        explicit_backing.components().any(|component| component
            .as_os_str()
            .to_string_lossy()
            .starts_with(BACKING_MARKER_PREFIX)),
        "refusing to write: backing file {:?} is not under a {BACKING_MARKER_PREFIX}* marker",
        explicit_backing
    );

    let sectors: u64 = read_trimmed(&sysfs.join("size"))
        .parse()
        .expect("parse sysfs size");
    let capacity = sectors.saturating_mul(512);
    assert!(
        capacity >= MIN_DEVICE_LEN,
        "fixture capacity {capacity} is smaller than the required {MIN_DEVICE_LEN}"
    );

    let discard_max: u64 = read_trimmed(&sysfs.join("queue/discard_max_bytes"))
        .parse()
        .expect("parse queue/discard_max_bytes");
    assert!(
        discard_max >= DISCARD_LEN as u64,
        "fixture queue/discard_max_bytes={discard_max} cannot hold the {DISCARD_LEN}-byte discard"
    );
    let discard_granularity: u64 = read_trimmed(&sysfs.join("queue/discard_granularity"))
        .parse()
        .expect("parse queue/discard_granularity");
    assert!(
        discard_granularity > 0,
        "fixture queue/discard_granularity must be positive"
    );

    let logical_block_size: u64 = read_trimmed(&sysfs.join("queue/logical_block_size"))
        .parse()
        .expect("parse queue/logical_block_size");
    assert!(
        logical_block_size > 0
            && BODY_OFFSET % logical_block_size == 0
            && DISCARD_OFFSET % logical_block_size == 0
            && DISCARD_LEN as u64 % logical_block_size == 0,
        "fixture logical block size {logical_block_size} does not align the owned ranges"
    );

    Some(Fixture {
        device,
        backing: explicit_backing,
        sysfs,
    })
}

fn read_trimmed(path: &Path) -> String {
    std::fs::read_to_string(path)
        .unwrap_or_else(|error| panic!("read {path:?}: {error}"))
        .trim()
        .to_string()
}

fn device_capacity(fixture: &Fixture) -> u64 {
    read_trimmed(&fixture.sysfs.join("size"))
        .parse::<u64>()
        .expect("parse sysfs size")
        .saturating_mul(512)
}

fn backing_blocks(path: &Path) -> u64 {
    std::fs::metadata(path)
        .unwrap_or_else(|error| panic!("stat loop backing {path:?}: {error}"))
        .blocks()
}

fn open_rw(path: &Path) -> File {
    OpenOptions::new()
        .read(true)
        .write(true)
        .open(path)
        .unwrap_or_else(|error| panic!("open {path:?} read/write: {error}"))
}

fn open_ro(path: &Path) -> File {
    OpenOptions::new()
        .read(true)
        .open(path)
        .unwrap_or_else(|error| panic!("open {path:?} read-only: {error}"))
}

fn read_range(file: &File, offset: u64, len: usize) -> Vec<u8> {
    let mut buffer = vec![0u8; len];
    let mut done = 0;
    while done < len {
        let count = file
            .read_at(&mut buffer[done..], offset + done as u64)
            .unwrap_or_else(|error| panic!("read {len} bytes at {offset}: {error}"));
        assert!(count > 0, "unexpected EOF reading block range at {offset}");
        done += count;
    }
    buffer
}

fn pattern(offset: u64, seed: u8) -> u8 {
    ((offset
        .wrapping_mul(37)
        .wrapping_add((seed as u64).wrapping_mul(11)))
        % 251) as u8
}

fn patterned(len: usize, seed: u8) -> Vec<u8> {
    (0..len as u64)
        .map(|offset| pattern(offset, seed))
        .collect()
}

#[derive(Clone, Copy)]
enum Kind {
    Sync,
    Async,
}

impl Kind {
    fn name(self) -> &'static str {
        match self {
            Kind::Sync => "raw_sync",
            Kind::Async => "raw_async",
        }
    }
}

fn default_backend(kind: Kind, path: &Path) -> Box<dyn DiskFile> {
    let file = open_rw(path);
    match kind {
        Kind::Sync => Box::new(RawFileDiskSync::new(file)),
        Kind::Async => Box::new(RawFileDisk::new(file)),
    }
}

fn backend(kind: Kind, path: &Path, opt_in: bool) -> Box<dyn DiskFile> {
    let file = open_rw(path);
    match kind {
        Kind::Sync => Box::new(RawFileDiskSync::new(file).with_discard(opt_in)),
        Kind::Async => Box::new(RawFileDisk::new(file).with_discard(opt_in)),
    }
}

/// Bounded poll for the completion notifier. Reading it before `complete` is
/// deliberate: a backend that forgets the immediate eventfd write must fail
/// here instead of passing by having `complete()` polled directly.
fn wait_notifier(io: &mut dyn AsyncIo, what: &str) {
    let deadline = Instant::now() + COMPLETION_TIMEOUT;
    loop {
        match io.notifier().read() {
            Ok(_) => return,
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                assert!(
                    Instant::now() < deadline,
                    "timed out waiting for the {what} notifier after {COMPLETION_TIMEOUT:?}"
                );
                std::thread::sleep(Duration::from_millis(2));
            }
            Err(error) => panic!("{what} notifier failed: {error}"),
        }
    }
}

/// Hand the backend's owned runner to a plain std thread, exactly as the
/// virtio queue does via `spawn_virtio_thread`. Dropping the backend closes the
/// worker; joining after that drop is what proves teardown has no stale work.
fn start_discard_worker(io: &mut dyn AsyncIo, what: &str) -> std::thread::JoinHandle<()> {
    let runner = io
        .start_discard_worker()
        .unwrap_or_else(|error| panic!("{what}: start discard worker: {error}"))
        .unwrap_or_else(|| panic!("{what}: block backend must expose a discard worker"));
    let what = what.to_owned();
    std::thread::spawn(move || {
        runner().unwrap_or_else(|error| panic!("{what}: discard worker exited: {error}"))
    })
}

fn wait_completion(io: &mut dyn AsyncIo, what: &str) -> Vec<(u64, i32)> {
    wait_notifier(io, what);
    let deadline = Instant::now() + COMPLETION_TIMEOUT;
    loop {
        let completions = io.complete();
        if !completions.is_empty() {
            return completions;
        }
        assert!(
            Instant::now() < deadline,
            "the {what} notifier fired but no completion is available"
        );
        std::thread::sleep(Duration::from_millis(1));
    }
}

/// Submit one full write and wait for its completion while the buffer lives.
fn submit_write(io: &mut dyn AsyncIo, offset: u64, data: &[u8], id: u64) {
    let iovec = libc::iovec {
        iov_base: data.as_ptr() as *mut libc::c_void,
        iov_len: data.len(),
    };
    io.write_vectored(offset as libc::off_t, vec![iovec], id)
        .expect("submit write");
    assert_eq!(
        wait_completion(io, "write"),
        vec![(id, data.len() as i32)],
        "write must complete with the full byte count"
    );
}

fn submit_read(io: &mut dyn AsyncIo, offset: u64, buffer: &mut [u8], id: u64) {
    let iovec = libc::iovec {
        iov_base: buffer.as_mut_ptr().cast(),
        iov_len: buffer.len(),
    };
    io.read_vectored(offset as libc::off_t, vec![iovec], id)
        .expect("submit read");
    assert_eq!(
        wait_completion(io, "read"),
        vec![(id, buffer.len() as i32)],
        "read must complete with the full byte count"
    );
}

fn inner_start() -> usize {
    (DISCARD_OFFSET - BODY_OFFSET) as usize
}

fn inner_end() -> usize {
    inner_start() + DISCARD_LEN
}

#[test]
fn block_loop_opt_in_discards_inner_range_and_reuses() {
    let _serial = SERIAL_FIXTURE
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let Some(fixture) = fixture() else {
        return;
    };

    let capacity_before = device_capacity(&fixture);
    let limits = discard_limits_from_sysfs(
        read_trimmed(&fixture.sysfs.join("queue/discard_max_bytes"))
            .parse()
            .expect("parse queue/discard_max_bytes"),
        read_trimmed(&fixture.sysfs.join("queue/discard_granularity"))
            .parse()
            .expect("parse queue/discard_granularity"),
    )
    .expect("fixture discard limits must be representable");
    let body = patterned(BODY_LEN, 7);
    assert!(
        body[inner_start()..inner_end()]
            .iter()
            .any(|byte| *byte != 0),
        "the discarded range must contain non-zero data so zeroed reads are meaningful"
    );

    for kind in [Kind::Sync, Kind::Async] {
        let name = kind.name();

        assert!(
            !default_backend(kind, &fixture.device).supports_discard(),
            "{name}: discard must stay off by default"
        );
        assert!(
            !backend(kind, &fixture.device, false).supports_discard(),
            "{name}: explicit opt-out must stay off"
        );

        let disk = backend(kind, &fixture.device, true);
        assert!(
            disk.supports_discard(),
            "{name}: an actual block device with a positive sysfs discard_max_bytes must advertise discard"
        );
        assert!(
            disk.supports_discard(),
            "{name}: discard capability must be cached and stable across calls"
        );
        assert_eq!(
            disk.max_discard_sectors(),
            limits.max_sectors,
            "{name}: advertised max must match the cached sysfs limit"
        );
        assert_eq!(
            disk.discard_sector_alignment(),
            limits.alignment_sectors,
            "{name}: alignment must match the cached sysfs granularity"
        );

        let mut io = disk.new_async_io(8).expect("create AsyncIo");
        let worker = start_discard_worker(io.as_mut(), name);
        submit_write(io.as_mut(), BODY_OFFSET, &body, 1);
        // Flush the loop write into the backing file so the st_blocks check
        // below observes real allocation rather than delayed allocation.
        io.fsync(None).expect("fsync after write");

        let mut written = vec![0u8; BODY_LEN];
        submit_read(io.as_mut(), BODY_OFFSET, &mut written, 2);
        assert_eq!(written, body, "{name}: bytes written through AsyncIo");

        let blocks_before = backing_blocks(&fixture.backing);
        io.discard(DISCARD_OFFSET as libc::off_t, DISCARD_LEN as libc::off_t, 3)
            .expect("submit discard");
        assert_eq!(
            wait_completion(io.as_mut(), "discard"),
            vec![(3, 0)],
            "{name}: discard must complete successfully"
        );

        assert_eq!(
            device_capacity(&fixture),
            capacity_before,
            "{name}: discard must not change device capacity"
        );
        let blocks_after = backing_blocks(&fixture.backing);
        assert!(
            blocks_after + (DISCARD_LEN as u64 / 512) <= blocks_before,
            "{name}: punching {DISCARD_LEN} bytes must release at least {} backing-file blocks \
             (before={blocks_before}, after={blocks_after}); this is local ext4 loop plumbing \
             evidence only, not a remote S3 reclaim claim",
            DISCARD_LEN / 512
        );

        let mut discarded = vec![0u8; BODY_LEN];
        submit_read(io.as_mut(), BODY_OFFSET, &mut discarded, 4);
        assert_eq!(
            &discarded[..inner_start()],
            &body[..inner_start()],
            "{name}: bytes before the discarded range must be untouched"
        );
        assert!(
            discarded[inner_start()..inner_end()]
                .iter()
                .all(|byte| *byte == 0),
            "{name}: the discarded inner {DISCARD_LEN} bytes must read back as exact zeros"
        );
        assert_eq!(
            &discarded[inner_end()..],
            &body[inner_end()..],
            "{name}: bytes after the discarded range must be untouched"
        );

        let replacement = patterned(DISCARD_LEN, 9);
        submit_write(io.as_mut(), DISCARD_OFFSET, &replacement, 5);
        let mut reused = vec![0u8; BODY_LEN];
        submit_read(io.as_mut(), BODY_OFFSET, &mut reused, 6);
        assert_eq!(
            &reused[inner_start()..inner_end()],
            &replacement[..],
            "{name}: the discarded range must be reusable"
        );
        assert_eq!(
            &reused[..inner_start()],
            &body[..inner_start()],
            "{name}: reuse must not disturb bytes before the range"
        );
        assert_eq!(
            &reused[inner_end()..],
            &body[inner_end()..],
            "{name}: reuse must not disturb bytes after the range"
        );

        // A second queue backend must complete an explicit fsync request, and
        // a synchronous fsync must succeed with no stray completions.
        let mut second = disk.new_async_io(4).expect("create second AsyncIo");
        second.fsync(Some(99)).expect("submit fsync");
        assert_eq!(
            wait_completion(second.as_mut(), "fsync"),
            vec![(99, 0)],
            "{name}: fsync completion"
        );
        second.fsync(None).expect("synchronous fsync must succeed");
        assert!(
            second.complete().is_empty(),
            "{name}: all completions must be drained"
        );

        drop(io);
        worker.join().expect("join discard worker");
        drop(second);
        drop(disk);
    }
}

#[test]
fn block_loop_read_only_discard_reports_negative_completion() {
    let _serial = SERIAL_FIXTURE
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let Some(fixture) = fixture() else {
        return;
    };

    for kind in [Kind::Sync, Kind::Async] {
        let name = kind.name();
        let ro = open_ro(&fixture.device);
        let before = read_range(&ro, BODY_OFFSET, BODY_LEN);

        // The duplicated fd is owned by the DiskFile; the AsyncIo borrows from
        // the DiskFile, not from `ro`.
        let duplicate = ro.try_clone().expect("duplicate read-only fd");
        let disk: Box<dyn DiskFile> = match kind {
            Kind::Sync => Box::new(RawFileDiskSync::new(duplicate).with_discard(true)),
            Kind::Async => Box::new(RawFileDisk::new(duplicate).with_discard(true)),
        };

        let mut io = disk.new_async_io(8).expect("create AsyncIo");
        let worker = start_discard_worker(io.as_mut(), name);
        io.discard(
            DISCARD_OFFSET as libc::off_t,
            DISCARD_LEN as libc::off_t,
            77,
        )
        .expect("a read-only discard failure must be reported via completion, not as Err");
        let completions = wait_completion(io.as_mut(), "read-only discard");
        assert_eq!(
            completions.len(),
            1,
            "{name}: a read-only discard must produce exactly one completion"
        );
        assert_eq!(completions[0].0, 77, "{name}: completion user_data");
        assert!(
            completions[0].1 < 0,
            "{name}: a read-only discard must not fake success: {completions:?}"
        );

        let after = read_range(&ro, BODY_OFFSET, BODY_LEN);
        assert_eq!(
            after, before,
            "{name}: a failed read-only discard must leave the device data unchanged"
        );

        drop(io);
        worker.join().expect("join discard worker");
        drop(disk);
        drop(ro);
    }
}

// Read-only on purpose: the alignment hint is derived from sysfs and must be
// observable without writing to the parent-owned device.
#[test]
fn block_loop_backends_report_sysfs_discard_alignment() {
    let _serial = SERIAL_FIXTURE
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let Some(fixture) = fixture() else {
        return;
    };

    let granularity: u64 = read_trimmed(&fixture.sysfs.join("queue/discard_granularity"))
        .parse()
        .expect("parse queue/discard_granularity");
    let expected = discard_alignment_from_granularity(granularity)
        .expect("fixture discard_granularity must be a representable sector multiple");
    let limits = discard_limits_from_sysfs(
        read_trimmed(&fixture.sysfs.join("queue/discard_max_bytes"))
            .parse()
            .expect("parse queue/discard_max_bytes"),
        granularity,
    )
    .expect("fixture discard limits must be representable");

    for kind in [Kind::Sync, Kind::Async] {
        let name = kind.name();
        let file = open_ro(&fixture.device);
        let disk: Box<dyn DiskFile> = match kind {
            Kind::Sync => Box::new(RawFileDiskSync::new(file).with_discard(true)),
            Kind::Async => Box::new(RawFileDisk::new(file).with_discard(true)),
        };
        assert!(
            disk.supports_discard(),
            "{name}: fixture must advertise discard"
        );
        assert_eq!(
            disk.discard_sector_alignment(),
            expected,
            "{name}: hint must be sysfs discard_granularity {granularity} bytes / 512"
        );
        assert_eq!(
            disk.max_discard_sectors(),
            limits.max_sectors,
            "{name}: max must be sysfs discard_max_bytes capped and rounded down"
        );
    }
}
