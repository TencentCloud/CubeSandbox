// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::container::Config;
use anyhow::{anyhow, Context, Error, Result};
use oci::{Linux, LinuxIdMapping, LinuxNamespace, Spec};
use std::collections::HashMap;
use std::path::{Component, PathBuf};

fn einval() -> Error {
    anyhow!(nix::Error::EINVAL)
}

fn get_linux(oci: &Spec) -> Result<&Linux> {
    oci.linux.as_ref().ok_or_else(einval)
}

fn contain_namespace(nses: &[LinuxNamespace], key: &str) -> bool {
    for ns in nses {
        if ns.r#type.as_str() == key {
            return true;
        }
    }

    false
}

fn rootfs(root: &str) -> Result<()> {
    let path = PathBuf::from(root);
    // not absolute path or not exists
    if !path.exists() || !path.is_absolute() {
        return Err(einval());
    }

    // symbolic link? ..?
    let mut stack: Vec<String> = Vec::new();
    for c in path.components() {
        if stack.is_empty() && (c == Component::RootDir || c == Component::ParentDir) {
            continue;
        }

        if c == Component::ParentDir {
            stack.pop();
            continue;
        }

        if let Some(v) = c.as_os_str().to_str() {
            stack.push(v.to_string());
        } else {
            return Err(einval());
        }
    }

    let mut cleaned = PathBuf::from("/");
    for e in stack.iter() {
        cleaned.push(e);
    }

    let canon = path.canonicalize().context("canonicalize")?;
    if cleaned != canon {
        // There is symbolic in path
        return Err(einval());
    }

    Ok(())
}

fn hostname(oci: &Spec) -> Result<()> {
    if oci.hostname.is_empty() {
        return Ok(());
    }

    let linux = get_linux(oci)?;
    if !contain_namespace(&linux.namespaces, "uts") {
        return Err(einval());
    }

    Ok(())
}

fn security(oci: &Spec) -> Result<()> {
    let linux = get_linux(oci)?;

    if linux.masked_paths.is_empty() && linux.readonly_paths.is_empty() {
        return Ok(());
    }

    if !contain_namespace(&linux.namespaces, "mount") {
        return Err(einval());
    }

    // don't care about selinux at present

    Ok(())
}

fn idmapping(maps: &[LinuxIdMapping]) -> Result<()> {
    for map in maps {
        if map.size > 0 {
            return Ok(());
        }
    }

    Err(einval())
}

fn usernamespace(oci: &Spec) -> Result<()> {
    let linux = get_linux(oci)?;

    if contain_namespace(&linux.namespaces, "user") {
        let user_ns = PathBuf::from("/proc/self/ns/user");
        if !user_ns.exists() {
            return Err(anyhow!("user namespace not supported!"));
        }
        // check if idmappings is correct, at least I saw idmaps
        // with zero size was passed to agent
        idmapping(&linux.uid_mappings).context("idmapping uid")?;
        idmapping(&linux.gid_mappings).context("idmapping gid")?;
    } else {
        // no user namespace but idmap
        if !linux.uid_mappings.is_empty() || !linux.gid_mappings.is_empty() {
            return Err(einval());
        }
    }

    Ok(())
}

fn cgroupnamespace(oci: &Spec) -> Result<()> {
    let linux = get_linux(oci)?;

    if contain_namespace(&linux.namespaces, "cgroup") {
        let path = PathBuf::from("/proc/self/ns/cgroup");
        if !path.exists() {
            return Err(anyhow!("cgroup unsupported!"));
        }
    }
    Ok(())
}

lazy_static! {
    pub static ref SYSCTLS: HashMap<&'static str, bool> = {
        let mut m = HashMap::new();
        m.insert("kernel.msgmax", true);
        m.insert("kernel.msgmnb", true);
        m.insert("kernel.msgmni", true);
        m.insert("kernel.sem", true);
        m.insert("kernel.shmall", true);
        m.insert("kernel.shmmax", true);
        m.insert("kernel.shmmni", true);
        m.insert("kernel.shm_rmid_forced", true);
        m
    };
}

fn sysctl(oci: &Spec) -> Result<()> {
    let linux = get_linux(oci)?;

    for (key, _) in linux.sysctl.iter() {
        if SYSCTLS.contains_key(key.as_str()) || key.starts_with("fs.mqueue.") {
            if contain_namespace(&linux.namespaces, "ipc") {
                continue;
            } else {
                return Err(einval());
            }
        }

        if key.starts_with("net.") {
            // the network ns is shared with the guest, don't expect to find it in spec
            continue;
        }

        if contain_namespace(&linux.namespaces, "uts") {
            if key == "kernel.domainname" {
                continue;
            }

            if key == "kernel.hostname" {
                return Err(einval());
            }
        }

        return Err(einval());
    }
    Ok(())
}

fn rootless_euid_mapping(oci: &Spec) -> Result<()> {
    let linux = get_linux(oci)?;

    if !contain_namespace(&linux.namespaces, "user") {
        return Err(einval());
    }

    if linux.uid_mappings.is_empty() || linux.gid_mappings.is_empty() {
        // rootless containers requires at least one UID/GID mapping
        return Err(einval());
    }

    Ok(())
}

fn has_idmapping(maps: &[LinuxIdMapping], id: u32) -> bool {
    for map in maps {
        if id >= map.container_id && id < map.container_id + map.size {
            return true;
        }
    }
    false
}

fn rootless_euid_mount(oci: &Spec) -> Result<()> {
    let linux = get_linux(oci)?;

    for mnt in oci.mounts.iter() {
        for opt in mnt.options.iter() {
            if opt.starts_with("uid=") || opt.starts_with("gid=") {
                let fields: Vec<&str> = opt.split('=').collect();

                if fields.len() != 2 {
                    return Err(einval());
                }

                let id = fields[1]
                    .trim()
                    .parse::<u32>()
                    .context(format!("parse field {}", &fields[1]))?;

                if opt.starts_with("uid=") && !has_idmapping(&linux.uid_mappings, id) {
                    return Err(einval());
                }

                if opt.starts_with("gid=") && !has_idmapping(&linux.gid_mappings, id) {
                    return Err(einval());
                }
            }
        }
    }
    Ok(())
}

fn rootless_euid(oci: &Spec) -> Result<()> {
    rootless_euid_mapping(oci).context("rootless euid mapping")?;
    rootless_euid_mount(oci).context("rotless euid mount")?;
    Ok(())
}

pub fn validate(conf: &Config) -> Result<()> {
    lazy_static::initialize(&SYSCTLS);
    let oci = conf.spec.as_ref().ok_or_else(einval)?;

    if oci.linux.is_none() {
        return Err(einval());
    }

    let root = match oci.root.as_ref() {
        Some(v) => v.path.as_str(),
        None => return Err(einval()),
    };

    rootfs(root).context("rootfs")?;
    hostname(oci).context("hostname")?;
    security(oci).context("security")?;
    usernamespace(oci).context("usernamespace")?;
    cgroupnamespace(oci).context("cgroupnamespace")?;
    sysctl(oci).context("sysctl")?;

    if conf.rootless_euid {
        rootless_euid(oci).context("rootless euid")?;
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use oci::Mount;

    #[test]
    fn test_namespace() {
        let namespaces = [
            LinuxNamespace {
                r#type: "net".to_owned(),
                path: "/sys/cgroups/net".to_owned(),
            },
            LinuxNamespace {
                r#type: "uts".to_owned(),
                path: "/sys/cgroups/uts".to_owned(),
            },
        ];

        assert_eq!(contain_namespace(&namespaces, "net"), true);
        assert_eq!(contain_namespace(&namespaces, "uts"), true);

        assert_eq!(contain_namespace(&namespaces, ""), false);
        assert_eq!(contain_namespace(&namespaces, "Net"), false);
        assert_eq!(contain_namespace(&namespaces, "ipc"), false);
    }

    #[test]
    fn test_rootfs() {
        rootfs("/_no_exit_fs_xxxxxxxxxxx").unwrap_err();
        rootfs("sys").unwrap_err();
        rootfs("/proc/self/root").unwrap_err();
        rootfs("/proc/self/root/sys").unwrap_err();

        rootfs("/proc/self").unwrap_err();
        rootfs("/./proc/self").unwrap_err();
        rootfs("/proc/././self").unwrap_err();
        rootfs("/proc/.././self").unwrap_err();

        rootfs("/proc/uptime").unwrap();
        rootfs("/../proc/uptime").unwrap();
        rootfs("/../../proc/uptime").unwrap();
        rootfs("/proc/../proc/uptime").unwrap();
        rootfs("/proc/../../proc/uptime").unwrap();
    }

    #[test]
    fn test_hostname() {
        let mut spec = Spec::default();

        hostname(&spec).unwrap();

        spec.hostname = "a.test.com".to_owned();
        hostname(&spec).unwrap_err();

        let mut linux = Linux::default();
        linux.namespaces = vec![
            LinuxNamespace {
                r#type: "net".to_owned(),
                path: "/sys/cgroups/net".to_owned(),
            },
            LinuxNamespace {
                r#type: "uts".to_owned(),
                path: "/sys/cgroups/uts".to_owned(),
            },
        ];
        spec.linux = Some(linux);
        hostname(&spec).unwrap();
    }

    #[test]
    fn test_security() {
        let mut spec = Spec::default();

        let linux = Linux::default();
        spec.linux = Some(linux);
        security(&spec).unwrap();

        let mut linux = Linux::default();
        linux.masked_paths.push("/test".to_owned());
        linux.namespaces = vec![
            LinuxNamespace {
                r#type: "net".to_owned(),
                path: "/sys/cgroups/net".to_owned(),
            },
            LinuxNamespace {
                r#type: "uts".to_owned(),
                path: "/sys/cgroups/uts".to_owned(),
            },
        ];
        spec.linux = Some(linux);
        security(&spec).unwrap_err();

        let mut linux = Linux::default();
        linux.masked_paths.push("/test".to_owned());
        linux.namespaces = vec![
            LinuxNamespace {
                r#type: "net".to_owned(),
                path: "/sys/cgroups/net".to_owned(),
            },
            LinuxNamespace {
                r#type: "mount".to_owned(),
                path: "/sys/cgroups/mount".to_owned(),
            },
        ];
        spec.linux = Some(linux);
        security(&spec).unwrap();
    }

    #[test]
    fn test_usernamespace() {
        let mut spec = Spec::default();
        usernamespace(&spec).unwrap_err();

        let linux = Linux::default();
        spec.linux = Some(linux);
        usernamespace(&spec).unwrap();

        let mut linux = Linux::default();
        linux.uid_mappings = vec![LinuxIdMapping {
            container_id: 0,
            host_id: 1000,
            size: 0,
        }];
        spec.linux = Some(linux);
        usernamespace(&spec).unwrap_err();

        let mut linux = Linux::default();
        linux.uid_mappings = vec![LinuxIdMapping {
            container_id: 0,
            host_id: 1000,
            size: 100,
        }];
        spec.linux = Some(linux);
        usernamespace(&spec).unwrap_err();
    }

    #[test]
    fn test_rootless_euid() {
        let mut spec = Spec::default();

        // Test case: without linux
        rootless_euid_mapping(&spec).unwrap_err();
        rootless_euid_mount(&spec).unwrap_err();

        // Test case: without user namespace
        let linux = Linux::default();
        spec.linux = Some(linux);
        rootless_euid_mapping(&spec).unwrap_err();

        // Test case: without user namespace
        let linux = spec.linux.as_mut().unwrap();
        linux.namespaces = vec![
            LinuxNamespace {
                r#type: "net".to_owned(),
                path: "/sys/cgroups/net".to_owned(),
            },
            LinuxNamespace {
                r#type: "uts".to_owned(),
                path: "/sys/cgroups/uts".to_owned(),
            },
        ];
        rootless_euid_mapping(&spec).unwrap_err();

        let linux = spec.linux.as_mut().unwrap();
        linux.namespaces = vec![
            LinuxNamespace {
                r#type: "net".to_owned(),
                path: "/sys/cgroups/net".to_owned(),
            },
            LinuxNamespace {
                r#type: "user".to_owned(),
                path: "/sys/cgroups/user".to_owned(),
            },
        ];
        linux.uid_mappings = vec![LinuxIdMapping {
            container_id: 0,
            host_id: 1000,
            size: 1000,
        }];
        linux.gid_mappings = vec![LinuxIdMapping {
            container_id: 0,
            host_id: 1000,
            size: 1000,
        }];
        rootless_euid_mapping(&spec).unwrap();

        spec.mounts.push(Mount {
            destination: "/app".to_owned(),
            r#type: "tmpfs".to_owned(),
            source: "".to_owned(),
            options: vec!["uid=10000".to_owned()],
        });
        rootless_euid_mount(&spec).unwrap_err();

        spec.mounts = vec![
            (Mount {
                destination: "/app".to_owned(),
                r#type: "tmpfs".to_owned(),
                source: "".to_owned(),
                options: vec!["uid=500".to_owned(), "gid=500".to_owned()],
            }),
        ];
        rootless_euid(&spec).unwrap();
    }

    #[test]
    fn test_sysctl() {
        let mut spec = Spec::default();

        let mut linux = Linux::default();
        linux.namespaces = vec![LinuxNamespace {
            r#type: "net".to_owned(),
            path: "/sys/cgroups/net".to_owned(),
        }];
        linux
            .sysctl
            .insert("kernel.domainname".to_owned(), "test.com".to_owned());
        spec.linux = Some(linux);
        sysctl(&spec).unwrap_err();

        spec.linux
            .as_mut()
            .unwrap()
            .namespaces
            .push(LinuxNamespace {
                r#type: "uts".to_owned(),
                path: "/sys/cgroups/uts".to_owned(),
            });
        sysctl(&spec).unwrap();
    }

    #[test]
    fn test_validate() {
        let spec = Spec::default();
        let mut config = Config {
            cgroup_name: "container1".to_owned(),
            use_systemd_cgroup: false,
            no_pivot_root: true,
            no_new_keyring: true,
            rootless_euid: false,
            rootless_cgroup: false,
            spec: Some(spec),
        };

        validate(&config).unwrap_err();

        let linux = Linux::default();
        config.spec.as_mut().unwrap().linux = Some(linux);
        validate(&config).unwrap_err();
    }
}
