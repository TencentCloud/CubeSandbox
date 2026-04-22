// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Context, Result};
use libc::uid_t;
use nix::fcntl::{self, OFlag};
#[cfg(not(test))]
use nix::mount;
use nix::mount::{MntFlags, MsFlags};
use nix::sys::stat::{self, Mode, SFlag};
use nix::unistd::{self, Gid, Uid};
use nix::NixPath;
use oci::{LinuxDevice, Mount, Process, Spec};
use std::collections::{HashMap, HashSet};
use std::fs::{self, OpenOptions};
use std::mem::MaybeUninit;
use std::os::unix;
use std::os::unix::io::RawFd;
use std::path::{Component, Path, PathBuf};

use path_absolutize::*;
use std::fs::File;
use std::io::{BufRead, BufReader};

use crate::container::DEFAULT_DEVICES;
use crate::log_child;
use crate::sync::write_count;
use cube::rootfs;
use std::string::ToString;

// Info reveals information about a particular mounted filesystem. This
// struct is populated from the content in the /proc/<pid>/mountinfo file.
#[derive(std::fmt::Debug, PartialEq)]
pub struct Info {
    mount_point: String,
    optional: String,
    fstype: String,
}

const MOUNTINFO_FORMAT: &str = "{d} {d} {d}:{d} {} {} {} {}";
const MOUNTINFO_PATH: &str = "/proc/self/mountinfo";
const PROC_PATH: &str = "/proc";

const ERR_FAILED_PARSE_MOUNTINFO: &str = "failed to parse mountinfo file";
const ERR_FAILED_PARSE_MOUNTINFO_FINAL_FIELDS: &str =
    "failed to parse final fields in mountinfo file";

// since libc didn't defined this const for musl, thus redefined it here.
#[cfg(all(target_os = "linux", target_env = "gnu"))]
const PROC_SUPER_MAGIC: libc::c_long = 0x00009fa0;
#[cfg(all(target_os = "linux", target_env = "musl"))]
const PROC_SUPER_MAGIC: libc::c_ulong = 0x00009fa0;

lazy_static! {
    static ref PROPAGATION: HashMap<&'static str, MsFlags> = {
        let mut m = HashMap::new();
        m.insert("private", MsFlags::MS_PRIVATE);
        m.insert("rprivate", MsFlags::MS_PRIVATE | MsFlags::MS_REC);
        m.insert("rshared", MsFlags::MS_SHARED | MsFlags::MS_REC);
        m.insert("rslave", MsFlags::MS_SLAVE | MsFlags::MS_REC);
        m.insert("runbindable", MsFlags::MS_UNBINDABLE | MsFlags::MS_REC);
        m.insert("shared", MsFlags::MS_SHARED);
        m.insert("slave", MsFlags::MS_SLAVE);
        m.insert("unbindable", MsFlags::MS_UNBINDABLE);
        m
    };
    static ref OPTIONS: HashMap<&'static str, (bool, MsFlags)> = {
        let mut m = HashMap::new();
        m.insert("acl", (false, MsFlags::MS_POSIXACL));
        m.insert("async", (true, MsFlags::MS_SYNCHRONOUS));
        m.insert("atime", (true, MsFlags::MS_NOATIME));
        m.insert("bind", (false, MsFlags::MS_BIND));
        m.insert("defaults", (false, MsFlags::empty()));
        m.insert("dev", (true, MsFlags::MS_NODEV));
        m.insert("diratime", (true, MsFlags::MS_NODIRATIME));
        m.insert("dirsync", (false, MsFlags::MS_DIRSYNC));
        m.insert("exec", (true, MsFlags::MS_NOEXEC));
        m.insert("iversion", (false, MsFlags::MS_I_VERSION));
        m.insert("lazytime", (false, MsFlags::MS_LAZYTIME));
        m.insert("loud", (true, MsFlags::MS_SILENT));
        m.insert("mand", (false, MsFlags::MS_MANDLOCK));
        m.insert("noacl", (true, MsFlags::MS_POSIXACL));
        m.insert("noatime", (false, MsFlags::MS_NOATIME));
        m.insert("nodev", (false, MsFlags::MS_NODEV));
        m.insert("nodiratime", (false, MsFlags::MS_NODIRATIME));
        m.insert("noexec", (false, MsFlags::MS_NOEXEC));
        m.insert("noiversion", (true, MsFlags::MS_I_VERSION));
        m.insert("nolazytime", (true, MsFlags::MS_LAZYTIME));
        m.insert("nomand", (true, MsFlags::MS_MANDLOCK));
        m.insert("norelatime", (true, MsFlags::MS_RELATIME));
        m.insert("nostrictatime", (true, MsFlags::MS_STRICTATIME));
        m.insert("nosuid", (false, MsFlags::MS_NOSUID));
        m.insert("rbind", (false, MsFlags::MS_BIND | MsFlags::MS_REC));
        m.insert("relatime", (false, MsFlags::MS_RELATIME));
        m.insert("remount", (false, MsFlags::MS_REMOUNT));
        m.insert("ro", (false, MsFlags::MS_RDONLY));
        m.insert("rw", (true, MsFlags::MS_RDONLY));
        m.insert("silent", (false, MsFlags::MS_SILENT));
        m.insert("strictatime", (false, MsFlags::MS_STRICTATIME));
        m.insert("suid", (true, MsFlags::MS_NOSUID));
        m.insert("sync", (false, MsFlags::MS_SYNCHRONOUS));
        m
    };
}

#[inline(always)]
#[cfg(not(test))]
pub fn mount<
    P1: ?Sized + NixPath,
    P2: ?Sized + NixPath,
    P3: ?Sized + NixPath,
    P4: ?Sized + NixPath,
>(
    source: Option<&P1>,
    target: &P2,
    fstype: Option<&P3>,
    flags: MsFlags,
    data: Option<&P4>,
) -> std::result::Result<(), nix::Error> {
    mount::mount(source, target, fstype, flags, data)
}

#[inline(always)]
#[cfg(test)]
pub fn mount<
    P1: ?Sized + NixPath,
    P2: ?Sized + NixPath,
    P3: ?Sized + NixPath,
    P4: ?Sized + NixPath,
>(
    _source: Option<&P1>,
    _target: &P2,
    _fstype: Option<&P3>,
    _flags: MsFlags,
    _data: Option<&P4>,
) -> std::result::Result<(), nix::Error> {
    Ok(())
}

#[inline(always)]
#[cfg(not(test))]
pub fn umount2<P: ?Sized + NixPath>(
    target: &P,
    flags: MntFlags,
) -> std::result::Result<(), nix::Error> {
    mount::umount2(target, flags)
}

#[inline(always)]
#[cfg(test)]
pub fn umount2<P: ?Sized + NixPath>(
    _target: &P,
    _flags: MntFlags,
) -> std::result::Result<(), nix::Error> {
    Ok(())
}

pub fn init_rootfs(
    cfd_log: RawFd,
    spec: &Spec,
    cpath: &HashMap<String, String>,
    mounts: &HashMap<String, String>,
    bind_device: bool,
) -> Result<()> {
    lazy_static::initialize(&OPTIONS);
    lazy_static::initialize(&PROPAGATION);
    lazy_static::initialize(&LINUXDEVICETYPE);

    let linux = &spec
        .linux
        .as_ref()
        .ok_or_else(|| anyhow!("Could not get linux configuration from spec"))?;

    let mut flags = MsFlags::MS_REC;
    match PROPAGATION.get(&linux.rootfs_propagation.as_str()) {
        Some(fl) => flags |= *fl,
        None => flags |= MsFlags::MS_SLAVE,
    }

    let root = spec
        .root
        .as_ref()
        .ok_or_else(|| anyhow!("Could not get rootfs path from spec"))
        .and_then(|r| {
            fs::canonicalize(r.path.as_str()).context("Could not canonicalize rootfs path")
        })?;

    let rootfs = (*root)
        .to_str()
        .ok_or_else(|| anyhow!("Could not convert rootfs path to string"))?;

    mount(None::<&str>, "/", None::<&str>, flags, None::<&str>)
        .map_err(|e| anyhow!("mount / failed.{:}", e))?;

    rootfs_parent_mount_private(rootfs)
        .map_err(|e| anyhow!("rootfs_parent_mount_private {:} failed.{:}", rootfs, e))?;

    mount(
        Some(rootfs),
        rootfs,
        None::<&str>,
        MsFlags::MS_BIND | MsFlags::MS_REC,
        None::<&str>,
    )
    .map_err(|e| anyhow!("mount rootfs {:} failed.{:}", rootfs, e))?;

    let mut bind_mount_dev = false;
    for m in &spec.mounts {
        let (mut flags, pgflags, data) = parse_mount(m);
        if !m.destination.starts_with('/') || m.destination.contains("..") {
            return Err(anyhow!(
                "the mount destination {} is invalid",
                m.destination
            ));
        }

        if m.r#type == "cgroup" {
            mount_cgroups(cfd_log, m, rootfs, flags, &data, cpath, mounts)
                .map_err(|e| anyhow!("mount cgroup {:} failed:{:}", m.source.as_str(), e))?;
        } else {
            if m.destination == "/dev" {
                if m.r#type == "bind" {
                    bind_mount_dev = true;
                }
                flags &= !MsFlags::MS_RDONLY;
            }

            if m.r#type == "bind" {
                check_proc_mount(m).map_err(|e| anyhow!("check proc mount failed:{:}", e))?;
            }

            // If the destination already exists and is not a directory, we bail
            // out This is to avoid mounting through a symlink or similar -- which
            // has been a "fun" attack scenario in the past.
            if m.r#type == "proc" || m.r#type == "sysfs" {
                if let Ok(meta) = fs::symlink_metadata(&m.destination) {
                    if !meta.is_dir() {
                        return Err(anyhow!(
                            "Mount point {} must be ordinary directory: got {:?}",
                            m.destination,
                            meta.file_type()
                        ));
                    }
                }
            }

            mount_from(cfd_log, m, rootfs, flags, &data, "").map_err(|e| {
                anyhow!(
                    "mount from {:} {:} failed:{:}",
                    m.source.as_str(),
                    m.destination.as_str(),
                    e
                )
            })?;

            if m.r#type == "bind" && !pgflags.is_empty() {
                let dest = secure_join(rootfs, &m.destination);
                mount(
                    None::<&str>,
                    dest.as_str(),
                    None::<&str>,
                    pgflags,
                    None::<&str>,
                )
                .map_err(|e| anyhow!("bind mount to {:} failed.{:}", dest.as_str(), e))?;
            }
        }
    }

    if let Some(ri_str) = spec.annotations.get(rootfs::ANNOTATION_K_ROOTFS_INFO) {
        let ri = rootfs::RootfsInfo::new(ri_str).map_err(|e| anyhow!("{}", e))?;
        if ri.mounts.is_some() {
            for mi in ri.mounts.unwrap() {
                let source = Path::new(&mi.virtiofs_source);
                let source_str = source.to_str().unwrap().to_string();
                let m = Mount {
                    destination: mi.container_dest,
                    r#type: mi.r#type,
                    source: source_str,
                    options: mi.options,
                };
                let (flags, _, data) = parse_mount(&m);
                mount_from(cfd_log, &m, rootfs, flags, &data, "").map_err(|e| {
                    anyhow!(
                        "mount from {:} {:} failed:{:}",
                        m.source.as_str(),
                        m.destination.as_str(),
                        e
                    )
                })?;
            }
        }
    }

    let olddir = unistd::getcwd()?;
    unistd::chdir(rootfs).map_err(|e| anyhow!("chdir {:} failed:{:}", rootfs, e))?;

    // in case the /dev directory was binded mount from guest,
    // then there's no need to create devices nodes and symlinks
    // in /dev.
    if !bind_mount_dev {
        default_symlinks().map_err(|e| anyhow!("default_symlinks failed:{:}", e))?;
        create_devices(&linux.devices, bind_device)
            .map_err(|e| anyhow!("create_devices failed:{:}", e))?;
        ensure_ptmx()?;
    }

    unistd::chdir(&olddir)?;

    Ok(())
}

fn check_proc_mount(m: &Mount) -> Result<()> {
    // White list, it should be sub directories of invalid destinations
    // These entries can be bind mounted by files emulated by fuse,
    // so commands like top, free displays stats in container.
    let valid_destinations = [
        "/proc/cpuinfo",
        "/proc/diskstats",
        "/proc/meminfo",
        "/proc/stat",
        "/proc/swaps",
        "/proc/uptime",
        "/proc/loadavg",
        "/proc/net/dev",
    ];

    for i in valid_destinations.iter() {
        if m.destination.as_str() == *i {
            return Ok(());
        }
    }

    if m.destination == PROC_PATH {
        // only allow a mount on-top of proc if it's source is "proc"
        unsafe {
            let mut stats = MaybeUninit::<libc::statfs>::uninit();
            if m.source
                .with_nix_path(|path| libc::statfs(path.as_ptr(), stats.as_mut_ptr()))
                .is_ok()
            {
                if stats.assume_init().f_type == PROC_SUPER_MAGIC {
                    return Ok(());
                }
            } else {
                return Ok(());
            }

            return Err(anyhow!(format!(
                "{} cannot be mounted to {} because it is not of type proc",
                m.source, m.destination
            )));
        }
    }

    if m.destination.starts_with(PROC_PATH) {
        return Err(anyhow!(format!(
            "{} cannot be mounted because it is inside /proc",
            m.destination
        )));
    }

    Ok(())
}

fn mount_cgroups_v2(cfd_log: RawFd, m: &Mount, rootfs: &str, flags: MsFlags) -> Result<()> {
    let olddir = unistd::getcwd()?;
    unistd::chdir(rootfs)?;

    // https://github.com/opencontainers/runc/blob/09ddc63afdde16d5fb859a1d3ab010bd45f08497/libcontainer/rootfs_linux.go#L287
    let bm = Mount {
        source: "cgroup".to_string(),
        r#type: "cgroup2".to_string(),
        destination: m.destination.clone(),
        options: Vec::new(),
    };

    let mount_flags: MsFlags = flags;

    mount_from(cfd_log, &bm, rootfs, mount_flags, "", "")?;

    unistd::chdir(&olddir)?;

    if flags.contains(MsFlags::MS_RDONLY) {
        let dest = format!("{}{}", rootfs, m.destination.as_str());
        mount(
            Some(dest.as_str()),
            dest.as_str(),
            None::<&str>,
            flags | MsFlags::MS_BIND | MsFlags::MS_REMOUNT,
            None::<&str>,
        )?;
    }

    Ok(())
}

fn mount_cgroups(
    cfd_log: RawFd,
    m: &Mount,
    rootfs: &str,
    flags: MsFlags,
    _data: &str,
    cpath: &HashMap<String, String>,
    mounts: &HashMap<String, String>,
) -> Result<()> {
    if cgroups::hierarchies::is_cgroup2_unified_mode() {
        return mount_cgroups_v2(cfd_log, m, rootfs, flags);
    }
    // mount tmpfs
    let ctm = Mount {
        source: "tmpfs".to_string(),
        r#type: "tmpfs".to_string(),
        destination: m.destination.clone(),
        options: Vec::new(),
    };

    let cflags = MsFlags::MS_NOEXEC | MsFlags::MS_NOSUID | MsFlags::MS_NODEV;
    mount_from(cfd_log, &ctm, rootfs, cflags, "", "")?;
    let olddir = unistd::getcwd()?;

    unistd::chdir(rootfs)?;

    let mut srcs: HashSet<String> = HashSet::new();

    // bind mount cgroups
    for (key, mount) in mounts.iter() {
        let source = if cpath.get(key).is_some() {
            cpath.get(key).unwrap()
        } else {
            continue;
        };

        let base = if let Some(o) = mount.rfind('/') {
            &mount[o + 1..]
        } else {
            &mount[..]
        };

        let destination = format!("{}/{}", m.destination.as_str(), base);

        if srcs.contains(source) {
            // already mounted, xxx,yyy style cgroup
            if key != base {
                let src = format!("{}/{}", m.destination.as_str(), key);
                unix::fs::symlink(destination.as_str(), &src[1..])?;
            }

            continue;
        }

        srcs.insert(source.to_string());

        let bm = Mount {
            source: source.to_string(),
            r#type: "bind".to_string(),
            destination: destination.clone(),
            options: Vec::new(),
        };

        let mut mount_flags: MsFlags = flags | MsFlags::MS_REC | MsFlags::MS_BIND;
        if key.contains("systemd") {
            mount_flags &= !MsFlags::MS_RDONLY;
        }
        mount_from(cfd_log, &bm, rootfs, mount_flags, "", "")?;

        if key != base {
            let src = format!("{}/{}", m.destination.as_str(), key);
            unix::fs::symlink(destination.as_str(), &src[1..]).map_err(|e| {
                log_child!(
                    cfd_log,
                    "symlink: {} {} err: {}",
                    key,
                    destination.as_str(),
                    e.to_string()
                );

                e
            })?;
        }
    }

    unistd::chdir(&olddir)?;

    if flags.contains(MsFlags::MS_RDONLY) {
        let dest = format!("{}{}", rootfs, m.destination.as_str());
        mount(
            Some(dest.as_str()),
            dest.as_str(),
            None::<&str>,
            flags | MsFlags::MS_BIND | MsFlags::MS_REMOUNT,
            None::<&str>,
        )?;
    }

    Ok(())
}

#[cfg(not(test))]
fn pivot_root<P1: ?Sized + NixPath, P2: ?Sized + NixPath>(
    new_root: &P1,
    put_old: &P2,
) -> anyhow::Result<(), nix::Error> {
    unistd::pivot_root(new_root, put_old)
}

#[cfg(test)]
fn pivot_root<P1: ?Sized + NixPath, P2: ?Sized + NixPath>(
    _new_root: &P1,
    _put_old: &P2,
) -> anyhow::Result<(), nix::Error> {
    Ok(())
}

pub fn pivot_rootfs<P: ?Sized + NixPath + std::fmt::Debug>(path: &P) -> Result<()> {
    let oldroot = fcntl::open("/", OFlag::O_DIRECTORY | OFlag::O_RDONLY, Mode::empty())?;
    defer!(unistd::close(oldroot).unwrap());
    let newroot = fcntl::open(path, OFlag::O_DIRECTORY | OFlag::O_RDONLY, Mode::empty())?;
    defer!(unistd::close(newroot).unwrap());

    // Change to the new root so that the pivot_root actually acts on it.
    unistd::fchdir(newroot)?;
    pivot_root(".", ".").context(format!("failed to pivot_root on {:?}", path))?;

    // Currently our "." is oldroot (according to the current kernel code).
    // However, purely for safety, we will fchdir(oldroot) since there isn't
    // really any guarantee from the kernel what /proc/self/cwd will be after a
    // pivot_root(2).
    unistd::fchdir(oldroot)?;

    // Make oldroot rslave to make sure our unmounts don't propagate to the
    // host. We don't use rprivate because this is known to cause issues due
    // to races where we still have a reference to a mount while a process in
    // the host namespace are trying to operate on something they think has no
    // mounts (devicemapper in particular).
    mount(
        Some("none"),
        ".",
        Some(""),
        MsFlags::MS_SLAVE | MsFlags::MS_REC,
        Some(""),
    )?;

    // Preform the unmount. MNT_DETACH allows us to unmount /proc/self/cwd.
    umount2(".", MntFlags::MNT_DETACH).context("failed to do umount2")?;

    // Switch back to our shiny new root.
    unistd::chdir("/")?;
    stat::umask(Mode::from_bits_truncate(0o022));
    Ok(())
}

fn rootfs_parent_mount_private(path: &str) -> Result<()> {
    let mount_infos = parse_mount_table(MOUNTINFO_PATH)?;

    let mut max_len = 0;
    let mut mount_point = String::from("");
    let mut options = String::from("");
    for i in mount_infos {
        if path.starts_with(&i.mount_point) && i.mount_point.len() > max_len {
            max_len = i.mount_point.len();
            mount_point = i.mount_point;
            options = i.optional;
        }
    }

    if options.contains("shared:") {
        mount(
            None::<&str>,
            mount_point.as_str(),
            None::<&str>,
            MsFlags::MS_PRIVATE,
            None::<&str>,
        )?;
    }

    Ok(())
}

// Parse /proc/self/mountinfo because comparing Dev and ino does not work from
// bind mounts
fn parse_mount_table(mountinfo_path: &str) -> Result<Vec<Info>> {
    let file = File::open(mountinfo_path)?;
    let reader = BufReader::new(file);
    let mut infos = Vec::new();

    for (_index, line) in reader.lines().enumerate() {
        let line = line?;

        //Example mountinfo format:
        // id
        // |  / parent
        // |  |   / major:minor
        // |  |   |   / root
        // |  |   |   |  / mount_point
        // |  |   |   |  |        / opts
        // |  |   |   |  |        |                           / optional
        // |  |   |   |  |        |                           |          / fstype
        // |  |   |   |  |        |                           |          |     / source
        // |  |   |   |  |        |                           |          |     |      / vfs_opts
        // 22 96 0:21 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw,seclabel

        let (_id, _parent, _major, _minor, _root, mount_point, _opts, optional) = scan_fmt!(
            &line,
            MOUNTINFO_FORMAT,
            i32,
            i32,
            i32,
            i32,
            String,
            String,
            String,
            String
        )
        .map_err(|_| anyhow!(ERR_FAILED_PARSE_MOUNTINFO))?;

        let fields: Vec<&str> = line.split(" - ").collect();
        if fields.len() == 2 {
            let final_fields: Vec<&str> = fields[1].split_whitespace().collect();

            if final_fields.len() != 3 {
                return Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO_FINAL_FIELDS));
            }
            let fstype = final_fields[0].to_string();

            let mut optional_new = String::new();
            if optional != "-" {
                optional_new = optional;
            }

            let info = Info {
                mount_point,
                optional: optional_new,
                fstype,
            };

            infos.push(info);
        } else {
            return Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO));
        }
    }

    Ok(infos)
}

#[inline(always)]
#[cfg(not(test))]
fn chroot<P: ?Sized + NixPath>(path: &P) -> Result<(), nix::Error> {
    unistd::chroot(path)
}

#[inline(always)]
#[cfg(test)]
fn chroot<P: ?Sized + NixPath>(_path: &P) -> Result<(), nix::Error> {
    Ok(())
}

pub fn ms_move_root(rootfs: &str) -> Result<bool> {
    unistd::chdir(rootfs)?;
    let mount_infos = parse_mount_table(MOUNTINFO_PATH)?;

    let root_path = Path::new(rootfs);
    let abs_root_buf = root_path.absolutize()?;
    let abs_root = abs_root_buf
        .to_str()
        .ok_or_else(|| anyhow!("failed to parse {} to absolute path", rootfs))?;

    for info in mount_infos.iter() {
        let mount_point = Path::new(&info.mount_point);
        let abs_mount_buf = mount_point.absolutize()?;
        let abs_mount_point = abs_mount_buf
            .to_str()
            .ok_or_else(|| anyhow!("failed to parse {} to absolute path", info.mount_point))?;
        let abs_mount_point_string = String::from(abs_mount_point);

        // Umount every sysfs and proc file systems, except those under the container rootfs
        if (info.fstype != "proc" && info.fstype != "sysfs")
            || abs_mount_point_string.starts_with(abs_root)
        {
            continue;
        }

        // Be sure umount events are not propagated to the host.
        mount(
            None::<&str>,
            abs_mount_point,
            None::<&str>,
            MsFlags::MS_SLAVE | MsFlags::MS_REC,
            None::<&str>,
        )?;
        umount2(abs_mount_point, MntFlags::MNT_DETACH).or_else(|e| {
            if e.ne(&nix::Error::EINVAL) && e.ne(&nix::Error::EPERM) {
                return Err(anyhow!(e));
            }

            // If we have not privileges for umounting (e.g. rootless), then
            // cover the path.
            mount(
                Some("tmpfs"),
                abs_mount_point,
                Some("tmpfs"),
                MsFlags::empty(),
                None::<&str>,
            )?;

            Ok(())
        })?;
    }

    mount(
        Some(abs_root),
        "/",
        None::<&str>,
        MsFlags::MS_MOVE,
        None::<&str>,
    )?;
    chroot(".")?;
    unistd::chdir("/")?;

    Ok(true)
}

fn parse_mount(m: &Mount) -> (MsFlags, MsFlags, String) {
    let mut flags = MsFlags::empty();
    let mut pgflags = MsFlags::empty();
    let mut data = Vec::new();

    for o in &m.options {
        if let Some(v) = OPTIONS.get(o.as_str()) {
            let (clear, fl) = *v;
            if clear {
                flags &= !fl;
            } else {
                flags |= fl;
            }
        } else if let Some(fl) = PROPAGATION.get(o.as_str()) {
            pgflags |= *fl;
        } else {
            data.push(o.clone());
        }
    }

    (flags, pgflags, data.join(","))
}

// This function constructs a canonicalized path by combining the `rootfs` and `unsafe_path` elements.
// The resulting path is guaranteed to be ("below" / "in a directory under") the `rootfs` directory.
//
// Parameters:
//
// - `rootfs` is the absolute path to the root of the containers root filesystem directory.
// - `unsafe_path` is path inside a container. It is unsafe since it may try to "escape" from the containers
//    rootfs by using one or more "../" path elements or is its a symlink to path.
fn secure_join(rootfs: &str, unsafe_path: &str) -> String {
    let mut path = PathBuf::from(format!("{}/", rootfs));
    let unsafe_p = Path::new(&unsafe_path);

    for it in unsafe_p.iter() {
        let it_p = Path::new(&it);

        // if it_p leads with "/", path.push(it) will be replace as it, so ignore "/"
        if it_p.has_root() {
            continue;
        };

        path.push(it);
        if let Ok(v) = path.read_link() {
            if v.is_absolute() {
                path = PathBuf::from(format!("{}{}", rootfs, v.to_str().unwrap()));
            } else {
                path.pop();
                for it in v.iter() {
                    path.push(it);
                    if path.exists() {
                        path = path.canonicalize().unwrap();
                        if !path.starts_with(rootfs) {
                            path = PathBuf::from(rootfs.to_string());
                        }
                    }
                }
            }
        }
        // skip any ".."
        if path.ends_with("..") {
            path.pop();
        }
    }

    path.to_str().unwrap().to_string()
}

fn mount_from(
    cfd_log: RawFd,
    m: &Mount,
    rootfs: &str,
    flags: MsFlags,
    data: &str,
    _label: &str,
) -> Result<()> {
    let d = String::from(data);
    let dest = secure_join(rootfs, &m.destination);

    let src = if m.r#type.as_str() == "bind" {
        //If the mountpoint path already exists, do not overwrite it
        if m.options.iter().any(|o| o == "propagation-mnt") {
            if let Ok(_) = stat::stat(dest.as_str()) {
                return Err(anyhow!(format!(
                    "dest:{} exist in propagation mounts",
                    dest.as_str()
                )));
            }
        }

        let src = fs::canonicalize(m.source.as_str())?;
        let dir = if src.is_dir() {
            Path::new(&dest)
        } else {
            Path::new(&dest).parent().unwrap()
        };

        let _ = fs::create_dir_all(dir).map_err(|e| {
            log_child!(
                cfd_log,
                "create dir {}: {}",
                dir.to_str().unwrap(),
                e.to_string()
            )
        });

        // make sure file exists so we can bind over it
        if !src.is_dir() {
            let _ = OpenOptions::new().create(true).write(true).open(&dest);
        }
        src.to_str().unwrap().to_string()
    } else {
        fs::create_dir_all(&dest)
            .map_err(|e| anyhow!(format!("create dest error, dest:{:?} err{:?}", dest, e)))?;
        if m.r#type.as_str() == "cgroup2" {
            "cgroup2".to_string()
        } else {
            let tmp = PathBuf::from(&m.source);
            tmp.to_str().unwrap().to_string()
        }
    };

    stat::stat(dest.as_str()).map_err(|e| anyhow!(format!("mount dest:{:?} err{:?}", dest, e)))?;

    //stat::stat(dest.as_str())
    //    .map_err(|e| log_child!(cfd_log, "dest stat error. {}: {:?}", dest.as_str(), e))?;

    mount(
        Some(src.as_str()),
        dest.as_str(),
        Some(m.r#type.as_str()),
        flags,
        Some(d.as_str()),
    )
    .map_err(|e| anyhow!(format!("mount error, dest:{:?} err{:?}", dest, e)))?;

    if flags.contains(MsFlags::MS_BIND)
        && flags.intersects(
            !(MsFlags::MS_REC
                | MsFlags::MS_REMOUNT
                | MsFlags::MS_BIND
                | MsFlags::MS_PRIVATE
                | MsFlags::MS_SHARED
                | MsFlags::MS_SLAVE),
        )
    {
        mount(
            Some(dest.as_str()),
            dest.as_str(),
            None::<&str>,
            flags | MsFlags::MS_REMOUNT,
            None::<&str>,
        )
        .map_err(|e| {
            log_child!(cfd_log, "remout {}: {:?}", dest.as_str(), e);
            e
        })?;
    }
    Ok(())
}

static SYMLINKS: &[(&str, &str)] = &[
    ("/proc/self/fd", "dev/fd"),
    ("/proc/self/fd/0", "dev/stdin"),
    ("/proc/self/fd/1", "dev/stdout"),
    ("/proc/self/fd/2", "dev/stderr"),
];

fn default_symlinks() -> Result<()> {
    if Path::new("/proc/kcore").exists() {
        unix::fs::symlink("/proc/kcore", "dev/kcore")?;
    }
    for &(src, dst) in SYMLINKS {
        unix::fs::symlink(src, dst)?;
    }
    Ok(())
}

fn dev_rel_path(path: &str) -> Option<&Path> {
    let path = Path::new(path);

    if !path.starts_with("/dev")
        || path == Path::new("/dev")
        || path.components().any(|c| c == Component::ParentDir)
    {
        return None;
    }
    path.strip_prefix("/").ok()
}

fn create_devices(devices: &[LinuxDevice], bind: bool) -> Result<()> {
    let op: fn(&LinuxDevice, &Path) -> Result<()> = if bind { bind_dev } else { mknod_dev };
    let old = stat::umask(Mode::from_bits_truncate(0o000));
    for dev in DEFAULT_DEVICES.iter() {
        let path = Path::new(&dev.path[1..]);
        op(dev, path).context(format!("Creating container device {:?}", dev))?;
    }
    for dev in devices {
        let path = dev_rel_path(&dev.path).ok_or_else(|| {
            let msg = format!("{} is not a valid device path", dev.path);
            anyhow!(msg)
        })?;
        if let Some(dir) = path.parent() {
            fs::create_dir_all(dir).context(format!("Creating container device {:?}", dev))?;
        }
        op(dev, path).context(format!("Creating container device {:?}", dev))?;
    }
    stat::umask(old);
    Ok(())
}

fn ensure_ptmx() -> Result<()> {
    let _ = fs::remove_file("dev/ptmx");
    unix::fs::symlink("pts/ptmx", "dev/ptmx")?;
    Ok(())
}

lazy_static! {
    static ref LINUXDEVICETYPE: HashMap<&'static str, SFlag> = {
        let mut m = HashMap::new();
        m.insert("c", SFlag::S_IFCHR);
        m.insert("b", SFlag::S_IFBLK);
        m.insert("p", SFlag::S_IFIFO);
        m
    };
}

fn mknod_dev(dev: &LinuxDevice, relpath: &Path) -> Result<()> {
    let f = match LINUXDEVICETYPE.get(dev.r#type.as_str()) {
        Some(v) => v,
        None => return Err(anyhow!("invalid spec".to_string())),
    };

    stat::mknod(
        relpath,
        *f,
        Mode::from_bits_truncate(dev.file_mode.unwrap_or(0)),
        nix::sys::stat::makedev(dev.major as u64, dev.minor as u64),
    )?;

    unistd::chown(
        relpath,
        Some(Uid::from_raw(dev.uid.unwrap_or(0) as uid_t)),
        Some(Gid::from_raw(dev.gid.unwrap_or(0) as uid_t)),
    )?;

    Ok(())
}

fn bind_dev(dev: &LinuxDevice, relpath: &Path) -> Result<()> {
    let fd = fcntl::open(
        relpath,
        OFlag::O_RDWR | OFlag::O_CREAT,
        Mode::from_bits_truncate(0o644),
    )?;

    unistd::close(fd)?;

    mount(
        Some(&*dev.path),
        relpath,
        None::<&str>,
        MsFlags::MS_BIND,
        None::<&str>,
    )?;
    Ok(())
}

pub fn finish_rootfs(_cfd_log: RawFd, spec: &Spec, process: &Process) -> Result<()> {
    let olddir = unistd::getcwd()?;
    unistd::chdir("/")?;

    if !process.cwd.is_empty() {
        // Although the process.cwd string can be unclean/malicious (../../dev, etc),
        // we are running on our own mount namespace and we just chrooted into the
        // container's root. It's safe to create CWD from there.
        // Unconditionally try to create CWD, create_dir_all will not fail if
        // it already exists.
        fs::create_dir_all(process.cwd.as_str())?;
    }

    if spec.linux.is_some() {
        let linux = spec.linux.as_ref().unwrap();

        for path in linux.masked_paths.iter() {
            mask_path(path)?;
        }

        for path in linux.readonly_paths.iter() {
            readonly_path(path)?;
        }
    }

    for m in spec.mounts.iter() {
        if m.destination == "/dev" {
            let (flags, _, _) = parse_mount(m);
            if flags.contains(MsFlags::MS_RDONLY) {
                mount(
                    Some("/dev"),
                    "/dev",
                    None::<&str>,
                    flags | MsFlags::MS_REMOUNT,
                    None::<&str>,
                )?;
            }
        }
    }

    if spec.root.as_ref().unwrap().readonly {
        let flags = MsFlags::MS_BIND | MsFlags::MS_RDONLY | MsFlags::MS_NODEV | MsFlags::MS_REMOUNT;

        mount(Some("/"), "/", None::<&str>, flags, None::<&str>)?;
    }
    stat::umask(Mode::from_bits_truncate(0o022));
    unistd::chdir(&olddir)?;

    Ok(())
}

fn mask_path(path: &str) -> Result<()> {
    if !path.starts_with('/') || path.contains("..") {
        return Err(anyhow!(nix::Error::EINVAL));
    }

    match mount(
        Some("/dev/null"),
        path,
        None::<&str>,
        MsFlags::MS_BIND,
        None::<&str>,
    ) {
        Err(e) => match e {
            nix::Error::ENOENT | nix::Error::ENOTDIR => Ok(()),
            _ => Err(e.into()),
        },
        Ok(_) => Ok(()),
    }
}

fn readonly_path(path: &str) -> Result<()> {
    if !path.starts_with('/') || path.contains("..") {
        return Err(anyhow!(nix::Error::EINVAL));
    }

    if let Err(e) = mount(
        Some(&path[1..]),
        path,
        None::<&str>,
        MsFlags::MS_BIND | MsFlags::MS_REC,
        None::<&str>,
    ) {
        match e {
            nix::Error::ENOENT => return Ok(()),
            _ => return Err(e.into()),
        };
    }

    mount(
        Some(&path[1..]),
        &path[1..],
        None::<&str>,
        MsFlags::MS_BIND | MsFlags::MS_REC | MsFlags::MS_RDONLY | MsFlags::MS_REMOUNT,
        None::<&str>,
    )?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::assert_result;
    use crate::skip_if_not_root;
    use std::fs::create_dir;
    use std::fs::create_dir_all;
    use std::fs::remove_dir_all;
    use std::io;
    use std::os::unix::fs;
    use std::os::unix::io::AsRawFd;
    use tempfile::tempdir;

    #[test]
    #[serial(chdir)]
    fn test_init_rootfs() {
        let stdout_fd = std::io::stdout().as_raw_fd();
        let mut spec = oci::Spec::default();
        let cpath = HashMap::new();
        let mounts = HashMap::new();

        // there is no spec.linux, should fail
        let ret = init_rootfs(stdout_fd, &spec, &cpath, &mounts, true);
        assert!(
            ret.is_err(),
            "Should fail: there is no spec.linux. Got: {:?}",
            ret
        );

        // there is no spec.Root, should fail
        spec.linux = Some(oci::Linux::default());
        let ret = init_rootfs(stdout_fd, &spec, &cpath, &mounts, true);
        assert!(
            ret.is_err(),
            "should fail: there is no spec.Root. Got: {:?}",
            ret
        );

        let rootfs = tempdir().unwrap();
        let ret = create_dir(rootfs.path().join("dev"));
        assert!(ret.is_ok(), "Got: {:?}", ret);

        spec.root = Some(oci::Root {
            path: rootfs.path().to_str().unwrap().to_string(),
            readonly: false,
        });

        // there is no spec.mounts, but should pass
        let ret = init_rootfs(stdout_fd, &spec, &cpath, &mounts, true);
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
        let _ = remove_dir_all(rootfs.path().join("dev"));
        let _ = create_dir(rootfs.path().join("dev"));

        // Adding bad mount point to spec.mounts
        spec.mounts.push(oci::Mount {
            destination: "error".into(),
            r#type: "bind".into(),
            source: "error".into(),
            options: vec!["shared".into(), "rw".into(), "dev".into()],
        });

        // destination doesn't start with /, should fail
        let ret = init_rootfs(stdout_fd, &spec, &cpath, &mounts, true);
        assert!(
            ret.is_err(),
            "Should fail: destination doesn't start with '/'. Got: {:?}",
            ret
        );
        spec.mounts.pop();
        let _ = remove_dir_all(rootfs.path().join("dev"));
        let _ = create_dir(rootfs.path().join("dev"));

        // mounting a cgroup
        spec.mounts.push(oci::Mount {
            destination: "/cgroup".into(),
            r#type: "cgroup".into(),
            source: "/cgroup".into(),
            options: vec!["shared".into()],
        });

        let ret = init_rootfs(stdout_fd, &spec, &cpath, &mounts, true);
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
        spec.mounts.pop();
        let _ = remove_dir_all(rootfs.path().join("dev"));
        let _ = create_dir(rootfs.path().join("dev"));

        // mounting /dev
        spec.mounts.push(oci::Mount {
            destination: "/dev".into(),
            r#type: "bind".into(),
            source: "/dev".into(),
            options: vec!["shared".into()],
        });

        let ret = init_rootfs(stdout_fd, &spec, &cpath, &mounts, true);
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    #[serial(chdir)]
    fn test_mount_cgroups() {
        let stdout_fd = std::io::stdout().as_raw_fd();
        let mount = oci::Mount {
            destination: "/cgroups".to_string(),
            r#type: "cgroup".to_string(),
            source: "/cgroups".to_string(),
            options: vec!["shared".to_string()],
        };
        let tempdir = tempdir().unwrap();
        let rootfs = tempdir.path().to_str().unwrap().to_string();
        let flags = MsFlags::MS_RDONLY;
        let mut cpath = HashMap::new();
        let mut cgroup_mounts = HashMap::new();

        cpath.insert("cpu".to_string(), "cpu".to_string());
        cpath.insert("memory".to_string(), "memory".to_string());

        cgroup_mounts.insert("default".to_string(), "default".to_string());
        cgroup_mounts.insert("cpu".to_string(), "cpu".to_string());
        cgroup_mounts.insert("memory".to_string(), "memory".to_string());

        let ret = create_dir_all(tempdir.path().join("cgroups"));
        assert!(ret.is_ok(), "Should pass. Got {:?}", ret);
        let ret = create_dir_all(tempdir.path().join("cpu"));
        assert!(ret.is_ok(), "Should pass. Got {:?}", ret);
        let ret = create_dir_all(tempdir.path().join("memory"));
        assert!(ret.is_ok(), "Should pass. Got {:?}", ret);

        let ret = mount_cgroups(
            stdout_fd,
            &mount,
            &rootfs,
            flags,
            "",
            &cpath,
            &cgroup_mounts,
        );
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    #[serial(chdir)]
    fn test_pivot_root() {
        let ret = pivot_rootfs("/tmp");
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    #[serial(chdir)]
    fn test_ms_move_rootfs() {
        let ret = ms_move_root("/abc");
        assert!(
            ret.is_err(),
            "Should fail. path doesn't exist. Got: {:?}",
            ret
        );

        let ret = ms_move_root("/tmp");
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    fn test_mask_path() {
        let ret = mask_path("abc");
        assert!(
            ret.is_err(),
            "Should fail: path doesn't start with '/'. Got: {:?}",
            ret
        );

        let ret = mask_path("abc/../");
        assert!(
            ret.is_err(),
            "Should fail: path contains '..'. Got: {:?}",
            ret
        );

        let ret = mask_path("/tmp");
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    #[serial(chdir)]
    fn test_finish_rootfs() {
        let stdout_fd = std::io::stdout().as_raw_fd();
        let mut spec = oci::Spec::default();

        spec.linux = Some(oci::Linux::default());
        spec.linux.as_mut().unwrap().masked_paths = vec!["/tmp".to_string()];
        spec.linux.as_mut().unwrap().readonly_paths = vec!["/tmp".to_string()];
        spec.root = Some(oci::Root {
            path: "/tmp".to_string(),
            readonly: true,
        });
        spec.mounts = vec![oci::Mount {
            destination: "/dev".to_string(),
            r#type: "bind".to_string(),
            source: "/dev".to_string(),
            options: vec!["ro".to_string(), "shared".to_string()],
        }];

        let ret = finish_rootfs(stdout_fd, &spec, &oci::Process::default());
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    fn test_readonly_path() {
        let ret = readonly_path("abc");
        assert!(ret.is_err(), "Should fail. Got: {:?}", ret);

        let ret = readonly_path("../../");
        assert!(ret.is_err(), "Should fail. Got: {:?}", ret);

        let ret = readonly_path("/tmp");
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    #[serial(chdir)]
    fn test_mknod_dev() {
        skip_if_not_root!();

        let tempdir = tempdir().unwrap();

        let olddir = unistd::getcwd().unwrap();
        defer!(let _ = unistd::chdir(&olddir););
        let _ = unistd::chdir(tempdir.path());

        let dev = oci::LinuxDevice {
            path: "/fifo".to_string(),
            r#type: "c".to_string(),
            major: 0,
            minor: 0,
            file_mode: Some(0660),
            uid: Some(unistd::getuid().as_raw()),
            gid: Some(unistd::getgid().as_raw()),
        };
        let path = Path::new("fifo");

        let ret = mknod_dev(&dev, path);
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);

        let ret = stat::stat(path);
        assert!(ret.is_ok(), "Should pass. Got: {:?}", ret);
    }

    #[test]
    fn test_mount_from() {
        #[derive(Debug)]
        struct TestData<'a> {
            source: &'a str,
            destination: &'a str,
            r#type: &'a str,
            flags: MsFlags,
            error_contains: &'a str,

            // if true, a directory will be created at path in source
            make_source_directory: bool,
            // if true, a file will be created at path in source
            make_source_file: bool,
        }

        impl Default for TestData<'_> {
            fn default() -> Self {
                TestData {
                    source: "tmp",
                    destination: "dest",
                    r#type: "tmpfs",
                    flags: MsFlags::empty(),
                    error_contains: "",
                    make_source_directory: true,
                    make_source_file: false,
                }
            }
        }

        let tests = &[
            TestData {
                ..Default::default()
            },
            TestData {
                flags: MsFlags::MS_BIND,
                ..Default::default()
            },
            TestData {
                r#type: "bind",
                ..Default::default()
            },
            TestData {
                r#type: "cgroup2",
                ..Default::default()
            },
            TestData {
                r#type: "bind",
                make_source_directory: false,
                error_contains: &format!("{}", std::io::Error::from_raw_os_error(libc::ENOENT)),
                ..Default::default()
            },
            TestData {
                r#type: "bind",
                make_source_directory: false,
                make_source_file: true,
                ..Default::default()
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);
            let tempdir = tempdir().unwrap();

            let (rfd, wfd) = unistd::pipe2(OFlag::O_CLOEXEC).unwrap();
            defer!({
                unistd::close(rfd).unwrap();
                unistd::close(wfd).unwrap();
            });

            let source_path = tempdir.path().join(d.source).to_str().unwrap().to_string();
            if d.make_source_directory {
                std::fs::create_dir_all(&source_path).unwrap();
            } else if d.make_source_file {
                std::fs::write(&source_path, []).unwrap();
            }

            let mount = Mount {
                source: source_path,
                destination: d.destination.to_string(),
                r#type: d.r#type.to_string(),
                options: vec![],
            };

            let result = mount_from(
                wfd,
                &mount,
                tempdir.path().to_str().unwrap(),
                d.flags,
                "",
                "",
            );

            let msg = format!("{}: result: {:?}", msg, result);

            if d.error_contains.is_empty() {
                assert!(result.is_ok(), "{}", msg);
            } else {
                assert!(result.is_err(), "{}", msg);

                let error_msg = format!("{}", result.unwrap_err());
                assert!(error_msg.contains(d.error_contains), "{}", msg);
            }
        }
    }

    #[test]
    fn test_check_proc_mount() {
        let mount = oci::Mount {
            destination: "/proc".to_string(),
            r#type: "bind".to_string(),
            source: "/test".to_string(),
            options: vec!["shared".to_string()],
        };

        assert!(check_proc_mount(&mount).is_err());

        let mount = oci::Mount {
            destination: "/proc/cpuinfo".to_string(),
            r#type: "bind".to_string(),
            source: "/test".to_string(),
            options: vec!["shared".to_string()],
        };

        assert!(check_proc_mount(&mount).is_ok());

        let mount = oci::Mount {
            destination: "/proc/test".to_string(),
            r#type: "bind".to_string(),
            source: "/test".to_string(),
            options: vec!["shared".to_string()],
        };

        assert!(check_proc_mount(&mount).is_err());
    }

    #[test]
    fn test_secure_join() {
        #[derive(Debug)]
        struct TestData<'a> {
            name: &'a str,
            rootfs: &'a str,
            unsafe_path: &'a str,
            symlink_path: &'a str,
            result: &'a str,
        }

        // create temporary directory to simulate container rootfs with symlink
        let rootfs_dir = tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path().to_str().unwrap();

        let tests = &[
            TestData {
                name: "rootfs_not_exist",
                rootfs: "/home/rootfs",
                unsafe_path: "a/b/c",
                symlink_path: "",
                result: "/home/rootfs/a/b/c",
            },
            TestData {
                name: "relative_path",
                rootfs: "/home/rootfs",
                unsafe_path: "../../../a/b/c",
                symlink_path: "",
                result: "/home/rootfs/a/b/c",
            },
            TestData {
                name: "skip any ..",
                rootfs: "/home/rootfs",
                unsafe_path: "../../../a/../../b/../../c",
                symlink_path: "",
                result: "/home/rootfs/a/b/c",
            },
            TestData {
                name: "rootfs is null",
                rootfs: "",
                unsafe_path: "",
                symlink_path: "",
                result: "/",
            },
            TestData {
                name: "relative softlink beyond container rootfs",
                rootfs: rootfs_path,
                unsafe_path: "1",
                symlink_path: "../../../",
                result: rootfs_path,
            },
            TestData {
                name: "abs softlink points to the non-exist directory",
                rootfs: rootfs_path,
                unsafe_path: "2",
                symlink_path: "/dddd",
                result: &format!("{}/dddd", rootfs_path).as_str().to_owned(),
            },
            TestData {
                name: "abs softlink points to the root",
                rootfs: rootfs_path,
                unsafe_path: "3",
                symlink_path: "/",
                result: &format!("{}/", rootfs_path).as_str().to_owned(),
            },
        ];

        for (i, t) in tests.iter().enumerate() {
            // Create a string containing details of the test
            let msg = format!("test[{}]: {:?}", i, t.name);

            // if is_symlink, then should be prepare the softlink environment
            if t.symlink_path != "" {
                fs::symlink(t.symlink_path, format!("{}/{}", t.rootfs, t.unsafe_path)).unwrap();
            }
            let result = secure_join(t.rootfs, t.unsafe_path);

            // Update the test details string with the results of the call
            let msg = format!("{}, result: {:?}", msg, result);

            // Perform the checks
            assert!(result == t.result, "{}", msg);
        }
    }

    #[test]
    fn test_parse_mount_table() {
        #[derive(Debug)]
        struct TestData<'a> {
            mountinfo_data: Option<&'a str>,
            result: Result<Vec<Info>>,
        }

        let tests = &[
            TestData {
                mountinfo_data: Some(
                    "22 933 0:20 / /sys rw,nodev shared:2 - sysfs sysfs rw,noexec",
                ),
                result: Ok(vec![Info {
                    mount_point: "/sys".to_string(),
                    optional: "shared:2".to_string(),
                    fstype: "sysfs".to_string(),
                }]),
            },
            TestData {
                mountinfo_data: Some(
                    r#"22 933 0:20 / /sys rw,nodev - sysfs sysfs rw,noexec
                       81 13 1:2 / /tmp/dir rw shared:2 - tmpfs tmpfs rw"#,
                ),
                result: Ok(vec![
                    Info {
                        mount_point: "/sys".to_string(),
                        optional: "".to_string(),
                        fstype: "sysfs".to_string(),
                    },
                    Info {
                        mount_point: "/tmp/dir".to_string(),
                        optional: "shared:2".to_string(),
                        fstype: "tmpfs".to_string(),
                    },
                ]),
            },
            TestData {
                mountinfo_data: Some(
                    "22 933 0:20 /foo\040-\040bar /sys rw,nodev shared:2 - sysfs sysfs rw,noexec",
                ),
                result: Ok(vec![Info {
                    mount_point: "/sys".to_string(),
                    optional: "shared:2".to_string(),
                    fstype: "sysfs".to_string(),
                }]),
            },
            TestData {
                mountinfo_data: Some(""),
                result: Ok(vec![]),
            },
            TestData {
                mountinfo_data: Some("invalid line data - sysfs sysfs rw"),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO)),
            },
            TestData {
                mountinfo_data: Some("22 96 0:21 / /sys rw,noexec - sysfs"),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO_FINAL_FIELDS)),
            },
            TestData {
                mountinfo_data: Some("22 96 0:21 / /sys rw,noexec - sysfs sysfs rw rw"),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO_FINAL_FIELDS)),
            },
            TestData {
                mountinfo_data: Some("22 96 0:21 / /sys rw,noexec shared:2 - x - x"),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO)),
            },
            TestData {
                mountinfo_data: Some("-"),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO)),
            },
            TestData {
                mountinfo_data: Some("--"),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO)),
            },
            TestData {
                mountinfo_data: Some("- -"),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO)),
            },
            TestData {
                mountinfo_data: Some(" - "),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO)),
            },
            TestData {
                mountinfo_data: Some(
                    r#"22 933 0:20 / /sys rw,nodev - sysfs sysfs rw,noexec
                       invalid line
                       81 13 1:2 / /tmp/dir rw shared:2 - tmpfs tmpfs rw"#,
                ),
                result: Err(anyhow!(ERR_FAILED_PARSE_MOUNTINFO)),
            },
            TestData {
                mountinfo_data: None,
                result: Err(anyhow!(io::Error::from_raw_os_error(libc::ENOENT))),
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let tempdir = tempdir().unwrap();
            let mountinfo_path = tempdir.path().join("mountinfo");

            if let Some(mountinfo_data) = d.mountinfo_data {
                std::fs::write(&mountinfo_path, mountinfo_data).unwrap();
            }

            let result = parse_mount_table(mountinfo_path.to_str().unwrap());

            let msg = format!("{}: result: {:?}", msg, result);

            assert_result!(d.result, result, msg);
        }
    }

    #[test]
    fn test_dev_rel_path() {
        // Valid device paths
        assert_eq!(dev_rel_path("/dev/sda").unwrap(), Path::new("dev/sda"));
        assert_eq!(dev_rel_path("//dev/sda").unwrap(), Path::new("dev/sda"));
        assert_eq!(
            dev_rel_path("/dev/vfio/99").unwrap(),
            Path::new("dev/vfio/99")
        );
        assert_eq!(dev_rel_path("/dev/...").unwrap(), Path::new("dev/..."));
        assert_eq!(dev_rel_path("/dev/a..b").unwrap(), Path::new("dev/a..b"));
        assert_eq!(dev_rel_path("/dev//foo").unwrap(), Path::new("dev/foo"));

        // Bad device paths
        assert!(dev_rel_path("/devfoo").is_none());
        assert!(dev_rel_path("/etc/passwd").is_none());
        assert!(dev_rel_path("/dev/../etc/passwd").is_none());
        assert!(dev_rel_path("dev/foo").is_none());
        assert!(dev_rel_path("").is_none());
        assert!(dev_rel_path("/dev").is_none());
    }
}
