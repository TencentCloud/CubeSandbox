// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use anyhow::{anyhow, Context, Result};
use lazy_static::lazy_static;
use nix::mount::{mount, MsFlags};
use nix::unistd;
use std::collections::HashMap;
use std::ffi::OsStr;
use std::io::{BufRead, BufReader};
use std::os::fd::AsRawFd;
use std::os::unix::fs as unixfs;
use std::time::Instant;
use std::{env, fs, fs::File, path::Path, process::Command};

const ENV_WRAPPER_MODE_K: &str = "wrapper_mode";
const ENV_WRAPPER_MODE_V: &str = "on";
const UNIFIED_CGROUP_HIERARCHY_OPTION: &str = "agent.unified_cgroup_hierarchy";
const PROC_CMDLINE: &str = "/proc/cmdline";
pub const PROC_CGROUPS: &str = "/proc/cgroups";
pub const SYSFS_CGROUPPATH: &str = "/sys/fs/cgroup";

#[derive(Debug, PartialEq)]
pub struct InitMount<'a> {
    fstype: &'a str,
    src: &'a str,
    dest: &'a str,
    flags: MsFlags,
    options: &'a str,
}

lazy_static! {
    static ref CGROUPS: HashMap<&'static str, &'static str> = {
        let mut m = HashMap::new();
        m.insert("cpu", "/sys/fs/cgroup/cpu");
        m.insert("cpuacct", "/sys/fs/cgroup/cpuacct");
        m.insert("blkio", "/sys/fs/cgroup/blkio");
        m.insert("cpuset", "/sys/fs/cgroup/cpuset");
        m.insert("memory", "/sys/fs/cgroup/memory");
        m.insert("devices", "/sys/fs/cgroup/devices");
        m.insert("freezer", "/sys/fs/cgroup/freezer");
        m.insert("net_cls", "/sys/fs/cgroup/net_cls");
        m.insert("perf_event", "/sys/fs/cgroup/perf_event");
        m.insert("net_prio", "/sys/fs/cgroup/net_prio");
        m.insert("hugetlb", "/sys/fs/cgroup/hugetlb");
        m.insert("pids", "/sys/fs/cgroup/pids");
        m.insert("rdma", "/sys/fs/cgroup/rdma");
        m
    };
}

lazy_static! {
    pub static ref INIT_ROOTFS_MOUNTS: Vec<InitMount<'static>> = vec![
        InitMount {
            fstype: "proc",
            src: "proc",
            dest: "/proc",
            flags: MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC,
            options: ""
        },
        InitMount {
            fstype: "sysfs",
            src: "sysfs",
            dest: "/sys",
            flags: MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC,
            options: ""
        },
        InitMount {
            fstype: "tmpfs",
            src: "tmpfs",
            dest: "/dev/shm",
            flags: MsFlags::MS_NOSUID | MsFlags::MS_NODEV,
            options: ""
        },
        InitMount {
            fstype: "devpts",
            src: "devpts",
            dest: "/dev/pts",
            flags: MsFlags::MS_NOSUID | MsFlags::MS_NOEXEC,
            options: ""
        },
        InitMount {
            fstype: "tmpfs",
            src: "tmpfs",
            dest: "/run",
            flags: MsFlags::MS_NOSUID | MsFlags::MS_NODEV,
            options: ""
        },
    ];
}

pub fn init(start: Instant) -> Result<()> {
    mount_sys(start)?;
    let unified = read_unified_cgroup_hierarchy(PROC_CMDLINE);
    mount_cgroup(start, unified)?;
    enable_rc_local(start);
    init_env(start)?;
    println!("init finish at:{}", start.elapsed().as_millis());
    Ok(())
}

fn mount_sys(start: Instant) -> Result<()> {
    for m in INIT_ROOTFS_MOUNTS.iter() {
        fs::create_dir_all(Path::new(m.dest))
            .map_err(|e| anyhow!("mkdir {} failed:{:?}", m.dest, e))?;

        let source = Path::new(m.src);
        let dest = Path::new(m.dest);
        mount(Some(source), dest, Some(m.fstype), m.flags, Some(""))
            .map_err(|e| anyhow!("mount {} to {} failed:{:?}", m.src, m.dest, e))?;
    }
    env::set_var(ENV_WRAPPER_MODE_K, ENV_WRAPPER_MODE_V);
    println!("init mount finish at:{}", start.elapsed().as_millis());
    Ok(())
}

fn enable_rc_local(start: Instant) {
    let output = Command::new("/etc/rc.local").spawn();

    match output {
        Ok(child) => match child.wait_with_output() {
            Ok(child) => {
                if let Some(code) = child.status.code() {
                    println!("/etc/rc.local exit:{}", code)
                }
            }
            Err(e) => {
                println!("Failed to execute rc.local: {}", e);
            }
        },
        Err(e) => {
            println!("Failed to execute rc.local: {}", e);
        }
    }

    println!("rc-local exit at:{}", start.elapsed().as_millis());
}

fn mount_cgroup(start: Instant, unified_cgroup_hierarchy: bool) -> Result<()> {
    let cgroups = get_cgroup_mounts(PROC_CGROUPS, unified_cgroup_hierarchy)?;
    for m in cgroups.iter() {
        fs::create_dir_all(Path::new(m.dest)).context("could not create directory")?;
        let source = Path::new(m.src);
        let dest = Path::new(m.dest);
        nix::mount::mount(Some(source), dest, Some(m.fstype), m.flags, Some(m.options)).map_err(
            |e| {
                anyhow!(
                    "failed to mount {:?} to {:?}, with error: {}",
                    source,
                    m.dest,
                    e
                )
            },
        )?;
    }

    // Enable memory hierarchical account (cgroup v1 only).
    // For more information see https://www.kernel.org/doc/Documentation/cgroup-v1/memory.txt
    if !unified_cgroup_hierarchy && !cgroups.is_empty() {
        let _ = fs::write("/sys/fs/cgroup/memory/memory.use_hierarchy", "1");
    }

    println!("mount cgroup finish at:{}", start.elapsed().as_millis());
    Ok(())
}

fn init_env(start: Instant) -> Result<()> {
    let _ = fs::remove_file(Path::new("/dev/ptmx"));
    unixfs::symlink(Path::new("/dev/pts/ptmx"), Path::new("/dev/ptmx"))
        .map_err(|e| anyhow!("symlink /dev/ptmx failed:{}", e))?;

    unistd::setsid().map_err(|e| anyhow!("setsid failed:{}", e))?;

    unsafe {
        libc::ioctl(std::io::stdin().as_raw_fd(), libc::TIOCSCTTY, 1);
    }

    env::set_var("PATH", "/bin:/sbin/:/usr/bin/:/usr/sbin/");

    let contents =
        std::fs::read_to_string("/etc/hostname").unwrap_or_else(|_| String::from("localhost"));
    let contents_array: Vec<&str> = contents.split(' ').collect();
    let hostname = contents_array[0].trim();

    if unistd::sethostname(OsStr::new(hostname)).is_err() {
        println!("failed to set hostname");
    }
    println!("init env finish at:{}", start.elapsed().as_millis());
    Ok(())
}

/// Parse `agent.unified_cgroup_hierarchy` from kernel cmdline.
/// Defaults to false when absent (matches cube-agent AgentConfig default).
/// Production CubeShim injects `agent.unified_cgroup_hierarchy=true`.
fn read_unified_cgroup_hierarchy(cmdline_path: &str) -> bool {
    let Ok(contents) = fs::read_to_string(cmdline_path) else {
        return false;
    };
    parse_unified_cgroup_hierarchy_from_cmdline(&contents)
}

fn parse_unified_cgroup_hierarchy_from_cmdline(cmdline: &str) -> bool {
    let prefix = concat!("agent.unified_cgroup_hierarchy", "=");
    for param in cmdline.split_whitespace() {
        if let Some(value) = param.strip_prefix(prefix) {
            return parse_bool_value(value);
        }
        // Flag without `=value` is treated as false (agent get_bool_value).
        if param == UNIFIED_CGROUP_HIERARCHY_OPTION {
            return false;
        }
    }
    false
}

/// Match agent `get_bool_value` value-side semantics:
/// try `bool` parse, else treat non-zero u64 as true, else false.
fn parse_bool_value(v: &str) -> bool {
    v.parse::<bool>()
        .unwrap_or_else(|_| v.parse::<u64>().map(|n| n != 0).unwrap_or(false))
}

fn get_cgroup_mounts(
    cg_path: &str,
    unified_cgroup_hierarchy: bool,
) -> Result<Vec<InitMount<'static>>> {
    // cgroup v2 — aligned with agent/src/mount.rs get_cgroup_mounts(..., true)
    if unified_cgroup_hierarchy {
        return Ok(vec![InitMount {
            fstype: "cgroup2",
            src: "cgroup2",
            dest: SYSFS_CGROUPPATH,
            flags: MsFlags::MS_NOSUID
                | MsFlags::MS_NODEV
                | MsFlags::MS_NOEXEC
                | MsFlags::MS_RELATIME,
            options: "nsdelegate",
        }]);
    }

    let file = File::open(cg_path)?;
    let reader = BufReader::new(file);

    let mut has_device_cgroup = false;
    let mut cg_mounts: Vec<InitMount> = vec![InitMount {
        fstype: "tmpfs",
        src: "tmpfs",
        dest: SYSFS_CGROUPPATH,
        flags: MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC,
        options: "mode=755",
    }];

    'outer: for line in reader.lines() {
        let line = line?;
        let fields: Vec<&str> = line.split('\t').collect();

        if fields[0].starts_with('#') {
            continue;
        }
        if fields.len() < 4 {
            continue;
        }
        if fields[3] == "0" {
            continue;
        }
        for f in [fields[1], fields[2], fields[3]].iter() {
            if f.parse::<u64>().is_err() {
                continue 'outer;
            }
        }

        let subsystem_name = fields[0];
        if subsystem_name.is_empty() {
            continue;
        }
        if subsystem_name == "devices" {
            has_device_cgroup = true;
        }

        if let Some((key, value)) = CGROUPS.get_key_value(subsystem_name) {
            cg_mounts.push(InitMount {
                fstype: "cgroup",
                src: "cgroup",
                dest: value,
                flags: MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC,
                options: key,
            });
        }
    }

    if !has_device_cgroup {
        return Ok(Vec::new());
    }

    cg_mounts.push(InitMount {
        fstype: "tmpfs",
        src: "tmpfs",
        dest: SYSFS_CGROUPPATH,
        flags: MsFlags::MS_REMOUNT | MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC,
        options: "mode=755",
    });

    Ok(cg_mounts)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn scratch_file(name: &str) -> std::path::PathBuf {
        let dir = env::temp_dir().join(format!("cube-init-test-{}-{}", std::process::id(), name));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).expect("create scratch dir");
        dir.join(name)
    }

    #[test]
    fn init_rootfs_mounts_include_proc_sys_run() {
        let dests: Vec<&str> = INIT_ROOTFS_MOUNTS.iter().map(|m| m.dest).collect();
        assert!(dests.contains(&"/proc"));
        assert!(dests.contains(&"/sys"));
        assert!(dests.contains(&"/run") || dests.iter().any(|d| d.starts_with("/run")));
    }

    #[test]
    fn wrapper_mode_constants() {
        assert_eq!(ENV_WRAPPER_MODE_K, "wrapper_mode");
        assert_eq!(ENV_WRAPPER_MODE_V, "on");
    }

    #[test]
    fn parse_bool_value_matches_agent() {
        assert!(parse_bool_value("true"));
        assert!(!parse_bool_value("false"));
        assert!(parse_bool_value("1"));
        assert!(!parse_bool_value("0"));
        assert!(parse_bool_value("11"));
        // Non-bool / non-integer → false (agent maps parse failure to 0)
        assert!(!parse_bool_value("a"));
    }

    #[test]
    fn parse_unified_from_cmdline() {
        assert!(!parse_unified_cgroup_hierarchy_from_cmdline("quiet"));
        assert!(parse_unified_cgroup_hierarchy_from_cmdline(
            "quiet agent.unified_cgroup_hierarchy=true"
        ));
        assert!(parse_unified_cgroup_hierarchy_from_cmdline(
            "agent.unified_cgroup_hierarchy=1 highres=off"
        ));
        assert!(!parse_unified_cgroup_hierarchy_from_cmdline(
            "agent.unified_cgroup_hierarchy=false"
        ));
        assert!(!parse_unified_cgroup_hierarchy_from_cmdline(
            "agent.unified_cgroup_hierarchy=0"
        ));
        assert!(!parse_unified_cgroup_hierarchy_from_cmdline(
            "agent.unified_cgroup_hierarchy"
        ));
    }

    #[test]
    fn get_cgroup_mounts_unified_returns_cgroup2() {
        let mounts = get_cgroup_mounts("", true).expect("unified mounts");
        assert_eq!(mounts.len(), 1);
        assert_eq!(mounts[0].fstype, "cgroup2");
        assert_eq!(mounts[0].src, "cgroup2");
        assert_eq!(mounts[0].dest, SYSFS_CGROUPPATH);
        assert_eq!(mounts[0].options, "nsdelegate");
    }

    #[test]
    fn get_cgroup_mounts_v1_with_devices() {
        let path = scratch_file("cgroups-devices");
        {
            let mut f = File::create(&path).unwrap();
            writeln!(f, "devices\t1\t1\t1").unwrap();
            writeln!(f, "memory\t2\t1\t1").unwrap();
        }
        let mounts = get_cgroup_mounts(path.to_str().unwrap(), false).expect("v1 mounts");
        assert!(mounts.len() >= 3);
        assert_eq!(mounts[0].fstype, "tmpfs");
        assert_eq!(mounts[0].dest, SYSFS_CGROUPPATH);
        assert!(mounts.iter().any(|m| m.dest == "/sys/fs/cgroup/devices"));
        assert_eq!(
            mounts.last().unwrap().flags & MsFlags::MS_REMOUNT,
            MsFlags::MS_REMOUNT
        );
        let _ = fs::remove_dir_all(path.parent().unwrap());
    }

    #[test]
    fn get_cgroup_mounts_v1_without_devices_empty() {
        let path = scratch_file("cgroups-memory");
        {
            let mut f = File::create(&path).unwrap();
            writeln!(f, "memory\t2\t1\t1").unwrap();
        }
        let mounts = get_cgroup_mounts(path.to_str().unwrap(), false).expect("ok");
        assert!(mounts.is_empty());
        let _ = fs::remove_dir_all(path.parent().unwrap());
    }

    #[test]
    fn read_unified_from_file() {
        let path = scratch_file("cmdline");
        fs::write(&path, "quiet agent.unified_cgroup_hierarchy=true\n").unwrap();
        assert!(read_unified_cgroup_hierarchy(path.to_str().unwrap()));

        fs::write(&path, "quiet\n").unwrap();
        assert!(!read_unified_cgroup_hierarchy(path.to_str().unwrap()));

        assert!(!read_unified_cgroup_hierarchy("/no/such/cmdline"));
        let _ = fs::remove_dir_all(path.parent().unwrap());
    }
}
