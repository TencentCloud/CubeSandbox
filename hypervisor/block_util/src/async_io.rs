// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use crate::{MAX_DISCARD_SECTORS, SECTOR_SIZE};
use libc::{ioctl, S_IFBLK, S_IFMT};
use std::convert::TryInto;
use std::fs::File;
use std::os::unix::io::{AsRawFd, RawFd};
use thiserror::Error;
use vmm_sys_util::eventfd::EventFd;
use vmm_sys_util::{ioctl_io_nr, ioctl_ioc_nr};

#[derive(Error, Debug)]
pub enum DiskFileError {
    /// Failed getting disk file size.
    #[error("Failed getting disk file size: {0}")]
    Size(#[source] std::io::Error),
    /// Failed creating a new AsyncIo.
    #[error("Failed creating a new AsyncIo: {0}")]
    NewAsyncIo(#[source] std::io::Error),
}

#[derive(Debug)]
pub struct DiskTopology {
    pub logical_block_size: u64,
    pub physical_block_size: u64,
    pub minimum_io_size: u64,
    pub optimal_io_size: u64,
}

impl Default for DiskTopology {
    fn default() -> Self {
        Self {
            logical_block_size: 512,
            physical_block_size: 512,
            minimum_io_size: 512,
            optimal_io_size: 0,
        }
    }
}

ioctl_io_nr!(BLKSSZGET, 0x12, 104);
ioctl_io_nr!(BLKPBSZGET, 0x12, 123);
ioctl_io_nr!(BLKIOMIN, 0x12, 120);
ioctl_io_nr!(BLKIOOPT, 0x12, 121);

enum BlockSize {
    LogicalBlock,
    PhysicalBlock,
    MinimumIo,
    OptimalIo,
}

impl DiskTopology {
    fn is_block_device(f: &mut File) -> std::io::Result<bool> {
        let mut stat = std::mem::MaybeUninit::<libc::stat>::uninit();
        let ret = unsafe { libc::fstat(f.as_raw_fd(), stat.as_mut_ptr()) };
        if ret != 0 {
            return Err(std::io::Error::last_os_error());
        }

        let is_block = unsafe { (*stat.as_ptr()).st_mode & S_IFMT == S_IFBLK };
        Ok(is_block)
    }

    // libc::ioctl() takes different types on different architectures
    #[allow(clippy::useless_conversion)]
    fn query_block_size(f: &mut File, block_size_type: BlockSize) -> std::io::Result<u64> {
        let mut block_size = 0;
        let ret = unsafe {
            ioctl(
                f.as_raw_fd(),
                match block_size_type {
                    BlockSize::LogicalBlock => BLKSSZGET(),
                    BlockSize::PhysicalBlock => BLKPBSZGET(),
                    BlockSize::MinimumIo => BLKIOMIN(),
                    BlockSize::OptimalIo => BLKIOOPT(),
                }
                .try_into()
                .unwrap(),
                &mut block_size,
            )
        };
        if ret != 0 {
            return Err(std::io::Error::last_os_error());
        };

        Ok(block_size)
    }

    pub fn probe(f: &mut File) -> std::io::Result<Self> {
        if !Self::is_block_device(f)? {
            return Ok(DiskTopology::default());
        }

        Ok(DiskTopology {
            logical_block_size: Self::query_block_size(f, BlockSize::LogicalBlock)?,
            physical_block_size: Self::query_block_size(f, BlockSize::PhysicalBlock)?,
            minimum_io_size: Self::query_block_size(f, BlockSize::MinimumIo)?,
            optimal_io_size: Self::query_block_size(f, BlockSize::OptimalIo)?,
        })
    }
}

/// Filesystem magic for XFS as reported by fstatfs(2)'s f_type.
const XFS_SUPER_MAGIC: u64 = 0x5846_5342;
/// Filesystem magic for any FUSE mount. Only an exact fuse.juicefs mount is
/// eligible; the magic alone is never enough.
const FUSE_SUPER_MAGIC: u64 = 0x6573_5546;

fn fs_magic(file: &File) -> Option<u64> {
    let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
    // SAFETY: fstatfs initializes the struct on success and the fd is valid.
    if unsafe { libc::fstatfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
        return None;
    }
    // SAFETY: fstatfs succeeded above.
    Some(unsafe { (*stat.as_ptr()).f_type as u64 })
}

/// Parses `mnt_id:` from /proc/self/fdinfo/<fd>. Pure for unit tests.
fn parse_fdinfo_mount_id(contents: &str) -> Option<u64> {
    contents.lines().find_map(|line| {
        let mut fields = line.split_whitespace();
        if fields.next()? != "mnt_id:" {
            return None;
        }
        fields.next()?.parse().ok()
    })
}

/// Returns the fstype of the mountinfo line whose first field equals
/// `mount_id`, without assuming how many optional fields precede the " - "
/// separator. Pure for unit tests.
fn mountinfo_fstype_for_mount_id(contents: &str, mount_id: u64) -> Option<&str> {
    for line in contents.lines() {
        let mut fields = line.split_whitespace();
        let Some(id) = fields.next() else {
            continue;
        };
        if id.parse::<u64>().ok() != Some(mount_id) {
            continue;
        }
        let mut after_separator = false;
        for field in fields {
            if after_separator {
                return Some(field);
            }
            if field == "-" {
                after_separator = true;
            }
        }
        return None;
    }
    None
}

fn mountinfo_is_juicefs(contents: &str, mount_id: u64) -> bool {
    mountinfo_fstype_for_mount_id(contents, mount_id) == Some("fuse.juicefs")
}

/// True only when the opened file's mount ID resolves, in /proc/self/mountinfo,
/// to fstype `fuse.juicefs`. Any read or parse failure is unsupported.
fn is_actual_juicefs_file(file: &File) -> bool {
    let Ok(fdinfo) = std::fs::read_to_string(format!("/proc/self/fdinfo/{}", file.as_raw_fd()))
    else {
        return false;
    };
    let Some(mount_id) = parse_fdinfo_mount_id(&fdinfo) else {
        return false;
    };
    let Ok(mountinfo) = std::fs::read_to_string("/proc/self/mountinfo") else {
        return false;
    };
    mountinfo_is_juicefs(&mountinfo, mount_id)
}

/// Returns true only for a regular file on an actual XFS filesystem or an
/// actual fuse.juicefs mount. Other FUSE filesystems (and every probing or
/// parsing failure) are unsupported.
pub fn is_regular_xfs_or_juicefs_file(file: &File) -> bool {
    if !file.metadata().map(|m| m.is_file()).unwrap_or(false) {
        return false;
    }

    match fs_magic(file) {
        Some(XFS_SUPER_MAGIC) => true,
        Some(FUSE_SUPER_MAGIC) => is_actual_juicefs_file(file),
        _ => false,
    }
}

/// See include/uapi/linux/fs.h: `_IO(0x12, 119)`.
const BLKDISCARD: libc::c_ulong = 0x1277;

/// Parses a sysfs integer and requires it to be positive. Pure for unit tests.
fn parse_positive_u64(contents: &str) -> Option<u64> {
    contents
        .trim()
        .parse::<u64>()
        .ok()
        .filter(|value| *value > 0)
}

/// Converts a sysfs `discard_granularity` byte count into the alignment hint
/// advertised to the guest, expressed in 512-byte sectors. Fails closed (no
/// outward rounding) for zero, non-sector multiples, and values that cannot be
/// represented in the u32 config field.
pub fn discard_alignment_from_granularity(granularity_bytes: u64) -> Option<u32> {
    if granularity_bytes == 0 || granularity_bytes % SECTOR_SIZE != 0 {
        return None;
    }
    u32::try_from(granularity_bytes / SECTOR_SIZE).ok()
}

/// Discard capabilities cached from a block device's sysfs queue.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BlockDiscardLimits {
    /// The device's discard granularity in 512-byte sectors.
    pub alignment_sectors: u32,
    /// Largest single discard the backend accepts, in 512-byte sectors,
    /// capped by both the device (rounded down to the granularity) and the
    /// global `MAX_DISCARD_SECTORS` bound.
    pub max_sectors: u32,
}

/// Derives cached discard limits from the raw sysfs counters. `max_sectors` is
/// the smaller of the device limit and `MAX_DISCARD_SECTORS`, rounded down to a
/// whole granularity. Zero or unrepresentable values fail closed.
pub fn discard_limits_from_sysfs(
    discard_max_bytes: u64,
    discard_granularity_bytes: u64,
) -> Option<BlockDiscardLimits> {
    let alignment_sectors = discard_alignment_from_granularity(discard_granularity_bytes)?;
    if discard_max_bytes == 0 {
        return None;
    }
    let device_sectors = discard_max_bytes / SECTOR_SIZE;
    let capped_sectors = device_sectors.min(u64::from(MAX_DISCARD_SECTORS));
    let max_sectors = capped_sectors - capped_sectors % u64::from(alignment_sectors);
    u32::try_from(max_sectors)
        .ok()
        .filter(|max_sectors| *max_sectors > 0)
        .map(|max_sectors| BlockDiscardLimits {
            alignment_sectors,
            max_sectors,
        })
}

/// Probes the opened descriptor's own sysfs queue once and returns the cached
/// discard limits. Eligible only for an actual block device whose queue reports
/// a positive `discard_max_bytes` and a positive, representable
/// `discard_granularity`. Partitions without a queue of their own (and every
/// stat/read/parse failure) are unsupported: there is no parent device
/// fallback. The device name is never inspected and nothing is probed
/// destructively.
pub fn block_discard_limits(file: &File) -> Option<BlockDiscardLimits> {
    let mut stat = std::mem::MaybeUninit::<libc::stat>::uninit();
    // SAFETY: fstat initializes the struct on success and the fd is valid.
    if unsafe { libc::fstat(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
        return None;
    }
    // SAFETY: fstat succeeded above.
    let stat = unsafe { stat.assume_init() };
    if stat.st_mode & S_IFMT != S_IFBLK {
        return None;
    }

    // libc's `f!` macro emits `major()`/`minor()` as unsafe fns in the older
    // version pinned by CubeShim and as safe fns in the newer version used
    // here. The unsafe block is required by the former and redundant (hence
    // the allow) on the latter.
    #[allow(unused_unsafe)]
    let (major, minor) = unsafe { (libc::major(stat.st_rdev), libc::minor(stat.st_rdev)) };
    // A partition has no queue of its own; canonicalizing would land on the
    // partition directory and the queue lookup below fails closed.
    let sysfs = std::fs::canonicalize(format!("/sys/dev/block/{major}:{minor}")).ok()?;
    let read_sysfs = |name: &str| {
        std::fs::read_to_string(sysfs.join("queue").join(name))
            .ok()
            .and_then(|contents| parse_positive_u64(&contents))
    };
    discard_limits_from_sysfs(
        read_sysfs("discard_max_bytes")?,
        read_sysfs("discard_granularity")?,
    )
}

/// Compatibility wrapper: the descriptor advertises discard support only when
/// `block_discard_limits` succeeds.
pub fn block_discard_alignment(file: &File) -> Option<u32> {
    block_discard_limits(file).map(|limits| limits.alignment_sectors)
}

/// Compatibility wrapper: the descriptor advertises discard support only when
/// `block_discard_alignment` succeeds.
pub fn is_discard_eligible_block_device(file: &File) -> bool {
    block_discard_alignment(file).is_some()
}

/// Issue BLKDISCARD with EINTR retry. `offset` is a byte offset and `len` the
/// byte length; callers only pass ranges already validated by the request
/// parser. The fd stays owned by the `DiskFile` for the whole call.
pub(crate) fn block_discard(fd: RawFd, offset: u64, len: u64) -> std::io::Result<()> {
    let mut range = [offset, len];
    loop {
        // SAFETY: `range` is a live [u64; 2] and the fd is open for this call.
        let result = unsafe { ioctl(fd, BLKDISCARD, range.as_mut_ptr()) };
        if result == 0 {
            return Ok(());
        }
        let error = std::io::Error::last_os_error();
        if error.kind() != std::io::ErrorKind::Interrupted {
            return Err(error);
        }
    }
}

pub type DiskFileResult<T> = std::result::Result<T, DiskFileError>;

pub trait DiskFile: Send {
    fn size(&mut self) -> DiskFileResult<u64>;
    fn new_async_io(&self, ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>>;
    fn supports_discard(&self) -> bool {
        false
    }

    /// Discard alignment hint advertised to the guest, in 512-byte sectors.
    /// The default (8 sectors == 4 KiB) preserves the historical hint for the
    /// regular-file backends; block backends are expected to override it with
    /// the value derived from their sysfs discard granularity.
    fn discard_sector_alignment(&self) -> u32 {
        8
    }

    /// Largest single discard the backend accepts, in 512-byte sectors.
    /// Regular-file backends keep the historical global bound; a block backend
    /// overrides it with the value cached from its sysfs queue.
    fn max_discard_sectors(&self) -> u32 {
        crate::MAX_DISCARD_SECTORS
    }

    fn topology(&mut self) -> DiskTopology {
        DiskTopology::default()
    }
}

#[derive(Error, Debug)]
pub enum AsyncIoError {
    /// Failed vectored reading from file.
    #[error("Failed vectored reading from file: {0}")]
    ReadVectored(#[source] std::io::Error),
    /// Failed vectored writing to file.
    #[error("Failed vectored writing to file: {0}")]
    WriteVectored(#[source] std::io::Error),
    /// Failed synchronizing file.
    #[error("Failed synchronizing file: {0}")]
    Fsync(#[source] std::io::Error),
    #[error("Failed discarding file range: {0}")]
    Discard(#[source] std::io::Error),
    /// The discard request was rejected without retaining anything for a later
    /// completion: either the submission-queue push failed before publishing an
    /// SQE, or an immediate completion could not arm the notifier. No outstanding
    /// SQE/CQ entry or later completion exists for this request, so the queue
    /// handler may fail it immediately. This is not a rollback guarantee: the
    /// immediate operation (for example a synchronous fallocate or BLKDISCARD)
    /// may already have taken effect. Every other discard failure stays
    /// ambiguous: an SQE may already have been published, so it must remain
    /// fatal.
    #[error("Discard request was safely rejected with no completion pending: {0}")]
    DiscardRejected(#[source] std::io::Error),
}

pub type AsyncIoResult<T> = std::result::Result<T, AsyncIoError>;

#[cfg(test)]
mod discard_mount_parser_tests {
    use super::{
        discard_alignment_from_granularity, discard_limits_from_sysfs,
        is_discard_eligible_block_device, mountinfo_fstype_for_mount_id, mountinfo_is_juicefs,
        parse_fdinfo_mount_id, parse_positive_u64, BlockDiscardLimits,
    };
    use crate::{MAX_DISCARD_SECTORS, SECTOR_SIZE};

    #[test]
    fn fdinfo_mount_id_parses_present_value() {
        let fdinfo = "pos:\t0\nflags:\t0100002\nmnt_id:\t25\nino:\t99\n";
        assert_eq!(parse_fdinfo_mount_id(fdinfo), Some(25));
    }

    #[test]
    fn fdinfo_mount_id_rejects_absent_or_malformed_value() {
        assert_eq!(parse_fdinfo_mount_id("pos:\t0\nflags:\t0100002\n"), None);
        assert_eq!(parse_fdinfo_mount_id("mnt_id:\n"), None);
        assert_eq!(parse_fdinfo_mount_id("mnt_id:\tnot-a-number\n"), None);
    }

    #[test]
    fn mountinfo_matches_exact_mount_id_despite_optional_fields() {
        let mountinfo = "\
24 1 0:21 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
25 1 0:22 / /srv/jfs rw shared:2 master:1 propagate_from:2 unbindable - fuse.juicefs JuiceFS:store rw,user_id=0
26 1 0:23 / /other rw - fuse.juicefs JuiceFS:other rw
";
        assert!(mountinfo_is_juicefs(mountinfo, 25));
        assert_eq!(
            mountinfo_fstype_for_mount_id(mountinfo, 25),
            Some("fuse.juicefs")
        );
        assert_eq!(mountinfo_fstype_for_mount_id(mountinfo, 24), Some("ext4"));
    }

    #[test]
    fn mountinfo_rejects_mismatching_mount_id_and_other_fuse() {
        let mountinfo = "\
25 1 0:22 / /srv/jfs rw shared:2 - fuse.sshfs user@host:/ rw
26 1 0:23 / /store rw - fuse.juicefs JuiceFS:store rw
";
        assert!(!mountinfo_is_juicefs(mountinfo, 25));
        assert!(mountinfo_is_juicefs(mountinfo, 26));
        assert_eq!(mountinfo_fstype_for_mount_id(mountinfo, 99), None);
    }

    #[test]
    fn mountinfo_without_separator_or_fstype_is_unsupported() {
        assert_eq!(
            mountinfo_fstype_for_mount_id("25 1 0:22 / / rw\n", 25),
            None
        );
        assert!(!mountinfo_is_juicefs("25 1 0:22 / / rw\n", 25));
        assert!(!mountinfo_is_juicefs("not mountinfo\n", 25));
    }

    #[test]
    fn positive_u64_requires_a_present_positive_value() {
        assert_eq!(parse_positive_u64("0\n"), None);
        assert_eq!(parse_positive_u64(" 16384\n"), Some(16384));
        assert_eq!(parse_positive_u64("not-a-number"), None);
        assert_eq!(parse_positive_u64(""), None);
    }

    #[test]
    fn regular_file_is_not_a_discard_eligible_block_device() {
        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        assert!(!is_discard_eligible_block_device(temp.as_file()));
    }

    #[test]
    fn discard_alignment_conversion_maps_sector_multiples() {
        assert_eq!(discard_alignment_from_granularity(512), Some(1));
        assert_eq!(discard_alignment_from_granularity(4096), Some(8));
        assert_eq!(discard_alignment_from_granularity(1024 * 1024), Some(2048));
    }

    #[test]
    fn discard_alignment_conversion_fails_closed() {
        // Zero, non-sector multiples and unrepresentable values must never be
        // rounded outward; callers disable discard support instead.
        assert_eq!(discard_alignment_from_granularity(0), None);
        assert_eq!(discard_alignment_from_granularity(1), None);
        assert_eq!(discard_alignment_from_granularity(513), None);
        assert_eq!(discard_alignment_from_granularity(4095), None);
        assert_eq!(discard_alignment_from_granularity(u64::MAX), None);
        assert_eq!(
            discard_alignment_from_granularity(u64::from(u32::MAX) * 512),
            Some(u32::MAX)
        );
        // One sector past u32::MAX sectors: sector-aligned but unrepresentable.
        assert_eq!(
            discard_alignment_from_granularity((u64::from(u32::MAX) + 1) * 512),
            None
        );
    }

    #[test]
    fn discard_limits_round_down_and_cap() {
        let sectors = |count: u64| count * SECTOR_SIZE;

        // Device max is below the global cap and alignment-dividing: kept as-is.
        assert_eq!(
            discard_limits_from_sysfs(sectors(1024), sectors(8)),
            Some(BlockDiscardLimits {
                alignment_sectors: 8,
                max_sectors: 1024,
            })
        );
        // Partial trailing granularity is rounded down, never up.
        assert_eq!(
            discard_limits_from_sysfs(sectors(2050), sectors(8)),
            Some(BlockDiscardLimits {
                alignment_sectors: 8,
                max_sectors: 2048,
            })
        );
        // Huge device max is capped at the global bound (exactly divisible).
        assert_eq!(
            discard_limits_from_sysfs(u64::MAX, sectors(8)),
            Some(BlockDiscardLimits {
                alignment_sectors: 8,
                max_sectors: MAX_DISCARD_SECTORS,
            })
        );
        // The cap is rounded down to a non-power-of-two granularity.
        assert_eq!(
            discard_limits_from_sysfs(u64::MAX, sectors(3)),
            Some(BlockDiscardLimits {
                alignment_sectors: 3,
                max_sectors: MAX_DISCARD_SECTORS - 1,
            })
        );
    }

    #[test]
    fn discard_limits_fail_closed() {
        let sectors = |count: u64| count * SECTOR_SIZE;

        // A zero max or granularity cannot bound discard safely.
        assert_eq!(discard_limits_from_sysfs(0, sectors(8)), None);
        assert_eq!(discard_limits_from_sysfs(sectors(16), 0), None);
        // Non-sector-multiple granularity.
        assert_eq!(discard_limits_from_sysfs(sectors(16), 513), None);
        // Device max smaller than one granularity rounds down to zero.
        assert_eq!(discard_limits_from_sysfs(sectors(7), sectors(8)), None);
        // Unrepresentable alignment.
        assert_eq!(
            discard_limits_from_sysfs(sectors(16), (u64::from(u32::MAX) + 1) * SECTOR_SIZE),
            None
        );
    }
}

pub trait AsyncIo: Send {
    fn notifier(&self) -> &EventFd;
    fn read_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()>;
    fn write_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()>;
    fn fsync(&mut self, user_data: Option<u64>) -> AsyncIoResult<()>;
    fn discard(
        &mut self,
        _offset: libc::off_t,
        _len: libc::off_t,
        _user_data: u64,
    ) -> AsyncIoResult<()> {
        Err(AsyncIoError::Discard(std::io::Error::from_raw_os_error(
            libc::EOPNOTSUPP,
        )))
    }

    /// Hands out the owned runner for this backend's discard worker, if any.
    /// The caller is expected to run it on a dedicated thread and treat an
    /// `Err` return as a fatal thread failure. The runner owns everything it
    /// needs (including its duplicated fd), so it can outlive the backend's
    /// borrow; the backend must be dropped to stop it.
    fn start_discard_worker(
        &mut self,
    ) -> std::io::Result<Option<Box<dyn FnOnce() -> std::io::Result<()> + Send>>> {
        Ok(None)
    }

    fn complete(&mut self) -> Vec<(u64, i32)>;
}
