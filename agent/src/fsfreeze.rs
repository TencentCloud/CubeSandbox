// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Filesystem freeze for filesystem-only sandbox snapshots.
//!
//! `FIFREEZE` flushes the journal, marks it empty and blocks new writers, so a
//! raw copy of the block device is a filesystem-consistent image. `sync` is
//! not enough: it leaves the journal non-empty and the image is only
//! crash-consistent.
//!
//! Only one filesystem may be frozen at a time. The agent owns the record of
//! what it froze so a thaw can never be redirected to another mount.

use std::fs::File;
use std::io::{BufRead, BufReader, Error, ErrorKind};
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use anyhow::{anyhow, ensure, Context, Result};
use lazy_static::lazy_static;

#[cfg(target_env = "musl")]
type IoctlRequestType = libc::c_int;
#[cfg(target_env = "gnu")]
type IoctlRequestType = libc::c_ulong;

// _IOWR('X', 119, int) / _IOWR('X', 120, int) from linux/fs.h. Defined here
// because not every libc release exports both names on every target.
const FIFREEZE_IOCTL: libc::c_ulong = 0xC004_5877;
const FITHAW_IOCTL: libc::c_ulong = 0xC004_5878;

/// A mount entry from /proc/self/mountinfo. Only the fields needed to prove
/// that an opened descriptor belongs to this mount are kept.
#[derive(Debug, Clone, PartialEq, Eq)]
struct MountEntry {
    mount_point: PathBuf,
    fstype: String,
    device: libc::dev_t,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FrozenMount {
    mount_point: PathBuf,
    device: libc::dev_t,
}

lazy_static! {
    static ref FROZEN: Mutex<Option<FrozenMount>> = Mutex::new(None);
}

/// Freeze `mount_point` and return the frozen device number.
///
/// The caller passes the mount point explicitly; it is never inferred from
/// host paths or from the overlay upperdir.
pub fn freeze(mount_point: &Path) -> Result<u64> {
    ensure!(
        mount_point.is_absolute(),
        "freeze target must be an absolute path"
    );
    ensure!(
        mount_point != Path::new("/"),
        "refusing to freeze the root filesystem"
    );

    let mut frozen = FROZEN
        .lock()
        .map_err(|_| anyhow!("filesystem freeze state is poisoned"))?;
    if let Some(current) = frozen.as_ref() {
        return Err(Error::new(
            ErrorKind::AlreadyExists,
            format!(
                "filesystem {} is already frozen",
                current.mount_point.display()
            ),
        )
        .into());
    }

    let entry = lookup_mount(mount_point)?;
    ensure!(
        entry.fstype == "ext4",
        "unsupported filesystem {} on {}",
        entry.fstype,
        mount_point.display()
    );

    // A second open could resolve to a different mount after an unmount.
    let file = File::open(mount_point)
        .with_context(|| format!("open freeze target {}", mount_point.display()))?;
    let fd = file.as_raw_fd();

    let target = fstat(fd)?;
    ensure!(
        target.st_dev != root_device()?,
        "refusing to freeze the root filesystem"
    );
    ensure!(
        target.st_dev == entry.device,
        "mount entry {} does not match the opened descriptor",
        mount_point.display()
    );

    let rc = unsafe { libc::ioctl(fd, FIFREEZE_IOCTL as IoctlRequestType, 0) };
    if rc < 0 {
        return Err(Error::last_os_error())
            .with_context(|| format!("FIFREEZE {}", mount_point.display()));
    }

    *frozen = Some(FrozenMount {
        mount_point: entry.mount_point,
        device: target.st_dev,
    });
    Ok(target.st_dev as u64)
}

/// Thaw `mount_point`.
///
/// Thawing is idempotent: the caller releases a disk it is not sure about
/// (a freeze RPC whose reply was lost), and the agent forgets its record when
/// it restarts, so a missing record is not an error. The path still has to be
/// a mount point, and when a record exists it must name the same mount and the
/// same device — a thaw can never be redirected to another filesystem.
pub fn thaw(mount_point: &Path) -> Result<()> {
    ensure!(
        mount_point.is_absolute(),
        "thaw target must be an absolute path"
    );
    ensure!(
        mount_point != Path::new("/"),
        "refusing to thaw the root filesystem"
    );

    let mut frozen = FROZEN
        .lock()
        .map_err(|_| anyhow!("filesystem freeze state is poisoned"))?;
    let recorded_device = match frozen.as_ref() {
        Some(record) => {
            ensure!(
                record.mount_point == mount_point,
                "thaw target {} does not match frozen mount {}",
                mount_point.display(),
                record.mount_point.display()
            );
            Some(record.device)
        }
        None => {
            // The mount still has to exist: FITHAW on a path that names no
            // filesystem would report EINVAL and mask a caller bug.
            let entry = lookup_mount(mount_point)?;
            ensure!(
                entry.fstype == "ext4",
                "unsupported filesystem {} on {}",
                entry.fstype,
                mount_point.display()
            );
            None
        }
    };

    let file = File::open(mount_point)
        .with_context(|| format!("open thaw target {}", mount_point.display()))?;
    let fd = file.as_raw_fd();
    let target = fstat(fd)?;
    ensure!(
        target.st_dev != root_device()?,
        "refusing to thaw the root filesystem"
    );
    if let Some(device) = recorded_device {
        ensure!(
            target.st_dev == device,
            "device changed since freeze; refusing to thaw {}",
            mount_point.display()
        );
    }

    let rc = unsafe { libc::ioctl(fd, FITHAW_IOCTL as IoctlRequestType, 0) };
    if rc < 0 {
        let err = Error::last_os_error();
        // A filesystem that is no longer frozen needs no further action. The
        // kernel returns EINVAL when the freeze count is already zero.
        if err.raw_os_error() == Some(libc::EINVAL) {
            *frozen = None;
            return Ok(());
        }
        return Err(err).with_context(|| format!("FITHAW {}", mount_point.display()));
    }

    *frozen = None;
    Ok(())
}

fn fstat(fd: libc::c_int) -> Result<libc::stat> {
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::fstat(fd, &mut stat) };
    if rc < 0 {
        return Err(Error::last_os_error()).context("fstat");
    }
    Ok(stat)
}

fn root_device() -> Result<libc::dev_t> {
    let file = File::open("/").context("open /")?;
    Ok(fstat(file.as_raw_fd())?.st_dev)
}

/// Reads /proc/self/mountinfo and returns the entry whose mount point is
/// exactly `mount_point`. mountinfo is the only source that proves the
/// filesystem type and the device number of a mount.
fn lookup_mount(mount_point: &Path) -> Result<MountEntry> {
    let entries = read_mountinfo("/proc/self/mountinfo")?;
    entries
        .into_iter()
        .find(|entry| entry.mount_point == mount_point)
        .ok_or_else(|| anyhow!("{} is not a mount point", mount_point.display()))
}

fn read_mountinfo(path: &str) -> Result<Vec<MountEntry>> {
    let file = File::open(path).with_context(|| format!("open {path}"))?;
    let mut entries = Vec::new();
    for line in BufReader::new(file).lines() {
        let line = line.with_context(|| format!("read {path}"))?;
        if let Some(entry) = parse_mountinfo_line(&line) {
            entries.push(entry);
        }
    }
    Ok(entries)
}

/// Parses one mountinfo line:
/// `36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw`
fn parse_mountinfo_line(line: &str) -> Option<MountEntry> {
    let fields: Vec<&str> = line.split(' ').collect();
    if fields.len() < 7 {
        return None;
    }
    let separator = fields.iter().position(|field| *field == "-")?;
    if separator + 1 >= fields.len() || separator < 5 {
        return None;
    }
    let device = parse_device(fields[2])?;
    Some(MountEntry {
        mount_point: PathBuf::from(unescape_mountinfo(fields[4])),
        fstype: fields[separator + 1].to_string(),
        device,
    })
}

fn parse_device(raw: &str) -> Option<libc::dev_t> {
    let (major, minor) = raw.split_once(':')?;
    let major: u32 = major.parse().ok()?;
    let minor: u32 = minor.parse().ok()?;
    Some(libc::makedev(major, minor))
}

/// mountinfo escapes space, tab, newline and backslash as `\ooo`.
fn unescape_mountinfo(raw: &str) -> String {
    let bytes = raw.as_bytes();
    let mut out = String::with_capacity(raw.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'\\' && i + 3 < bytes.len() {
            let octal = &raw[i + 1..i + 4];
            if let Ok(value) = u8::from_str_radix(octal, 8) {
                out.push(value as char);
                i += 4;
                continue;
            }
        }
        out.push(bytes[i] as char);
        i += 1;
    }
    out
}

/// Parses major:minor pairs for tests without touching the real mount table.
#[cfg(test)]
fn parse_mountinfo_for_test(contents: &str) -> Vec<MountEntry> {
    contents.lines().filter_map(parse_mountinfo_line).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_ext4_mount_entry() {
        let entries = parse_mountinfo_for_test(
            "36 35 98:0 /mnt1 /run/blk-cube/vdb rw,noatime master:1 - ext4 /dev/vdb rw\n",
        );
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].mount_point, PathBuf::from("/run/blk-cube/vdb"));
        assert_eq!(entries[0].fstype, "ext4");
        assert_eq!(entries[0].device, libc::makedev(98, 0));
    }

    #[test]
    fn decodes_escaped_mount_point() {
        let entries =
            parse_mountinfo_for_test("36 35 98:0 / /run/block\\040cube rw - ext4 /dev/vdb rw\n");
        assert_eq!(
            entries[0].mount_point,
            PathBuf::from("/run/block cube"),
            "octal escape must be decoded"
        );
    }

    #[test]
    fn ignores_malformed_lines() {
        assert!(parse_mountinfo_for_test("garbage\n\n").is_empty());
        assert!(parse_mountinfo_for_test("1 2 notadev /a /b rw - ext4 x rw\n").is_empty());
    }

    #[test]
    fn refuses_root_and_relative_paths() {
        assert!(freeze(Path::new("/")).is_err());
        assert!(freeze(Path::new("relative")).is_err());
    }

    #[test]
    fn thaw_rejects_paths_that_are_not_mounts() {
        assert!(thaw(Path::new("/run/not-frozen")).is_err());
        assert!(thaw(Path::new("relative")).is_err());
        assert!(thaw(Path::new("/")).is_err());
    }
}
