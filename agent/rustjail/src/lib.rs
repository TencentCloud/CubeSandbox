// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

#![allow(non_upper_case_globals)]
#[macro_use]
#[cfg(test)]
extern crate serial_test;
extern crate serde;
extern crate serde_json;
#[macro_use]
extern crate serde_derive;
extern crate caps;
extern crate protocols;
#[macro_use]
extern crate scopeguard;
extern crate capctl;
#[macro_use]
extern crate lazy_static;
extern crate libc;
extern crate protobuf;
#[macro_use]
extern crate slog;
#[macro_use]
extern crate scan_fmt;
extern crate oci;
extern crate path_absolutize;
extern crate regex;

pub mod capabilities;
pub mod cgroups;
#[cfg(feature = "standard-oci-runtime")]
pub mod console;
pub mod container;
pub mod mount;
pub mod pipestream;
pub mod process;
#[cfg(feature = "seccomp")]
pub mod seccomp;
pub mod specconv;
pub mod sync;
pub mod sync_with_async;
pub mod validator;

use std::collections::HashMap;

use protocols::oci as grpc;

// construct ociSpec from grpc::Spec, which is needed for hook
// execution. since hooks read config.json
pub fn process_grpc_to_oci(p: &grpc::Process) -> oci::Process {
    let console_size = if p.ConsoleSize.is_some() {
        let c = p.ConsoleSize.as_ref().unwrap();
        Some(oci::Box {
            height: c.Height,
            width: c.Width,
        })
    } else {
        None
    };

    let user = if p.User.is_some() {
        let u = p.User.as_ref().unwrap();
        oci::User {
            uid: u.UID,
            gid: u.GID,
            additional_gids: u.AdditionalGids.clone(),
            username: u.Username.clone(),
        }
    } else {
        oci::User {
            uid: 0,
            gid: 0,
            additional_gids: vec![],
            username: String::from(""),
        }
    };

    let capabilities = if p.Capabilities.is_some() {
        let cap = p.Capabilities.as_ref().unwrap();

        Some(oci::LinuxCapabilities {
            bounding: cap.Bounding.clone().to_vec(),
            effective: cap.Effective.clone().to_vec(),
            inheritable: cap.Inheritable.clone().to_vec(),
            permitted: cap.Permitted.clone().to_vec(),
            ambient: cap.Ambient.clone().to_vec(),
        })
    } else {
        None
    };

    let rlimits = {
        let mut r = Vec::new();
        for lm in p.Rlimits.iter() {
            r.push(oci::PosixRlimit {
                r#type: lm.Type.clone(),
                hard: lm.Hard,
                soft: lm.Soft,
            });
        }
        r
    };

    oci::Process {
        terminal: p.Terminal,
        console_size,
        user,
        args: p.Args.clone().to_vec(),
        env: p.Env.clone().to_vec(),
        cwd: p.Cwd.clone(),
        capabilities,
        rlimits,
        no_new_privileges: p.NoNewPrivileges,
        apparmor_profile: p.ApparmorProfile.clone(),
        oom_score_adj: Some(p.OOMScoreAdj as i32),
        selinux_label: p.SelinuxLabel.clone(),
    }
}

fn root_grpc_to_oci(root: &grpc::Root) -> oci::Root {
    oci::Root {
        path: root.Path.clone(),
        readonly: root.Readonly,
    }
}

fn mount_grpc_to_oci(m: &grpc::Mount) -> oci::Mount {
    oci::Mount {
        destination: m.destination.clone(),
        r#type: m.type_.clone(),
        source: m.source.clone(),
        options: m.options.clone().to_vec(),
    }
}

use protocols::oci::Hook as grpcHook;

fn hook_grpc_to_oci(h: &[grpcHook]) -> Vec<oci::Hook> {
    let mut r = Vec::new();
    for e in h.iter() {
        r.push(oci::Hook {
            path: e.Path.clone(),
            args: e.Args.clone().to_vec(),
            env: e.Env.clone().to_vec(),
            timeout: Some(e.Timeout as i32),
        });
    }
    r
}

fn hooks_grpc_to_oci(h: &grpc::Hooks) -> oci::Hooks {
    let prestart = hook_grpc_to_oci(h.Prestart.as_ref());

    let poststart = hook_grpc_to_oci(h.Poststart.as_ref());

    let poststop = hook_grpc_to_oci(h.Poststop.as_ref());

    oci::Hooks {
        prestart,
        poststart,
        poststop,
    }
}

fn idmap_grpc_to_oci(im: &grpc::LinuxIDMapping) -> oci::LinuxIdMapping {
    oci::LinuxIdMapping {
        container_id: im.ContainerID,
        host_id: im.HostID,
        size: im.Size,
    }
}

fn idmaps_grpc_to_oci(ims: &[grpc::LinuxIDMapping]) -> Vec<oci::LinuxIdMapping> {
    let mut r = Vec::new();
    for im in ims.iter() {
        r.push(idmap_grpc_to_oci(im));
    }
    r
}

fn throttle_devices_grpc_to_oci(
    tds: &[grpc::LinuxThrottleDevice],
) -> Vec<oci::LinuxThrottleDevice> {
    let mut r = Vec::new();
    for td in tds.iter() {
        r.push(oci::LinuxThrottleDevice {
            blk: oci::LinuxBlockIoDevice {
                major: td.Major,
                minor: td.Minor,
            },
            rate: td.Rate,
        });
    }
    r
}

fn weight_devices_grpc_to_oci(wds: &[grpc::LinuxWeightDevice]) -> Vec<oci::LinuxWeightDevice> {
    let mut r = Vec::new();
    for wd in wds.iter() {
        r.push(oci::LinuxWeightDevice {
            blk: oci::LinuxBlockIoDevice {
                major: wd.Major,
                minor: wd.Minor,
            },
            weight: Some(wd.Weight as u16),
            leaf_weight: Some(wd.LeafWeight as u16),
        });
    }
    r
}

fn blockio_grpc_to_oci(blk: &grpc::LinuxBlockIO) -> oci::LinuxBlockIo {
    let weight_device = weight_devices_grpc_to_oci(blk.WeightDevice.as_ref());
    let throttle_read_bps_device = throttle_devices_grpc_to_oci(blk.ThrottleReadBpsDevice.as_ref());
    let throttle_write_bps_device =
        throttle_devices_grpc_to_oci(blk.ThrottleWriteBpsDevice.as_ref());
    let throttle_read_iops_device =
        throttle_devices_grpc_to_oci(blk.ThrottleReadIOPSDevice.as_ref());
    let throttle_write_iops_device =
        throttle_devices_grpc_to_oci(blk.ThrottleWriteIOPSDevice.as_ref());

    oci::LinuxBlockIo {
        weight: Some(blk.Weight as u16),
        leaf_weight: Some(blk.LeafWeight as u16),
        weight_device,
        throttle_read_bps_device,
        throttle_write_bps_device,
        throttle_read_iops_device,
        throttle_write_iops_device,
    }
}

pub fn resources_grpc_to_oci(res: &grpc::LinuxResources) -> oci::LinuxResources {
    let devices = {
        let mut d = Vec::new();
        for dev in res.Devices.iter() {
            let major = if dev.Major == -1 {
                None
            } else {
                Some(dev.Major)
            };

            let minor = if dev.Minor == -1 {
                None
            } else {
                Some(dev.Minor)
            };
            d.push(oci::LinuxDeviceCgroup {
                allow: dev.Allow,
                r#type: dev.Type.clone(),
                major,
                minor,
                access: dev.Access.clone(),
            });
        }
        d
    };

    let memory = if res.Memory.is_some() {
        let mem = res.Memory.as_ref().unwrap();
        Some(oci::LinuxMemory {
            limit: Some(mem.Limit),
            reservation: Some(mem.Reservation),
            swap: Some(mem.Swap),
            kernel: Some(mem.Kernel),
            kernel_tcp: Some(mem.KernelTCP),
            swappiness: Some(mem.Swappiness),
            disable_oom_killer: Some(mem.DisableOOMKiller),
        })
    } else {
        None
    };

    let cpu = if res.CPU.is_some() {
        let c = res.CPU.as_ref().unwrap();
        Some(oci::LinuxCpu {
            shares: Some(c.Shares),
            quota: Some(c.Quota),
            period: Some(c.Period),
            realtime_runtime: Some(c.RealtimeRuntime),
            realtime_period: Some(c.RealtimePeriod),
            cpus: c.Cpus.clone(),
            mems: c.Mems.clone(),
        })
    } else {
        None
    };

    let pids = if res.Pids.is_some() {
        let p = res.Pids.as_ref().unwrap();
        Some(oci::LinuxPids { limit: p.Limit })
    } else {
        None
    };

    let block_io = if res.BlockIO.is_some() {
        let blk = res.BlockIO.as_ref().unwrap();
        // copy LinuxBlockIO
        Some(blockio_grpc_to_oci(blk))
    } else {
        None
    };

    let hugepage_limits = {
        let mut r = Vec::new();
        for hl in res.HugepageLimits.iter() {
            r.push(oci::LinuxHugepageLimit {
                page_size: hl.Pagesize.clone(),
                limit: hl.Limit,
            });
        }
        r
    };

    let network = if res.Network.is_some() {
        let net = res.Network.as_ref().unwrap();
        let priorities = {
            let mut r = Vec::new();
            for pr in net.Priorities.iter() {
                r.push(oci::LinuxInterfacePriority {
                    name: pr.Name.clone(),
                    priority: pr.Priority,
                });
            }
            r
        };
        Some(oci::LinuxNetwork {
            class_id: Some(net.ClassID),
            priorities,
        })
    } else {
        None
    };

    oci::LinuxResources {
        devices,
        memory,
        cpu,
        pids,
        block_io,
        hugepage_limits,
        network,
        rdma: HashMap::new(),
    }
}

fn seccomp_grpc_to_oci(sec: &grpc::LinuxSeccomp) -> oci::LinuxSeccomp {
    let syscalls = {
        let mut r = Vec::new();

        for sys in sec.Syscalls.iter() {
            let mut args = Vec::new();

            let errno_ret: u32 = if sys.has_errnoret() {
                sys.errnoret()
            } else {
                libc::EPERM as u32
            };

            for arg in sys.Args.iter() {
                args.push(oci::LinuxSeccompArg {
                    index: arg.Index as u32,
                    value: arg.Value,
                    value_two: arg.ValueTwo,
                    op: arg.Op.clone(),
                });
            }

            r.push(oci::LinuxSyscall {
                names: sys.Names.clone().to_vec(),
                action: sys.Action.clone(),
                errno_ret,
                args,
            });
        }
        r
    };

    oci::LinuxSeccomp {
        default_action: sec.DefaultAction.clone(),
        architectures: sec.Architectures.clone().to_vec(),
        flags: sec.Flags.clone().to_vec(),
        syscalls,
    }
}

fn linux_grpc_to_oci(l: &grpc::Linux) -> oci::Linux {
    let uid_mappings = idmaps_grpc_to_oci(l.UIDMappings.as_ref());
    let gid_mappings = idmaps_grpc_to_oci(l.GIDMappings.as_ref());

    let resources = if l.Resources.is_some() {
        Some(resources_grpc_to_oci(l.Resources.as_ref().unwrap()))
    } else {
        None
    };

    let seccomp = if l.Seccomp.is_some() {
        Some(seccomp_grpc_to_oci(l.Seccomp.as_ref().unwrap()))
    } else {
        None
    };

    let namespaces = {
        let mut r = Vec::new();

        for ns in l.Namespaces.iter() {
            r.push(oci::LinuxNamespace {
                r#type: ns.Type.clone(),
                path: ns.Path.clone(),
            });
        }
        r
    };

    let devices = {
        let mut r = Vec::new();

        for d in l.Devices.iter() {
            r.push(oci::LinuxDevice {
                path: d.Path.clone(),
                r#type: d.Type.clone(),
                major: d.Major,
                minor: d.Minor,
                file_mode: Some(d.FileMode),
                uid: Some(d.UID),
                gid: Some(d.GID),
            });
        }
        r
    };

    let intel_rdt = if l.IntelRdt.is_some() {
        let rdt = l.IntelRdt.as_ref().unwrap();

        Some(oci::LinuxIntelRdt {
            l3_cache_schema: rdt.L3CacheSchema.clone(),
        })
    } else {
        None
    };

    oci::Linux {
        uid_mappings,
        gid_mappings,
        sysctl: l.Sysctl.clone(),
        resources,
        cgroups_path: l.CgroupsPath.clone(),
        namespaces,
        devices,
        seccomp,
        rootfs_propagation: l.RootfsPropagation.clone(),
        masked_paths: l.MaskedPaths.clone().to_vec(),
        readonly_paths: l.ReadonlyPaths.clone().to_vec(),
        mount_label: l.MountLabel.clone(),
        intel_rdt,
    }
}

pub fn grpc_to_oci(grpc: &grpc::Spec) -> oci::Spec {
    // process
    let process = if grpc.Process.is_some() {
        Some(process_grpc_to_oci(grpc.Process.as_ref().unwrap()))
    } else {
        None
    };

    // root
    let root = if grpc.Root.is_some() {
        Some(root_grpc_to_oci(grpc.Root.as_ref().unwrap()))
    } else {
        None
    };

    // mounts
    let mounts = {
        let mut r = Vec::new();
        for m in grpc.Mounts.iter() {
            r.push(mount_grpc_to_oci(m));
        }
        r
    };

    // hooks
    let hooks = if grpc.Hooks.is_some() {
        Some(hooks_grpc_to_oci(grpc.Hooks.as_ref().unwrap()))
    } else {
        None
    };

    // Linux
    let linux = if grpc.Linux.is_some() {
        Some(linux_grpc_to_oci(grpc.Linux.as_ref().unwrap()))
    } else {
        None
    };

    let mut version = grpc.Version.clone();
    if version.is_empty() {
        version = "1.0.2-dev".to_string();
    }

    oci::Spec {
        version,
        process,
        root,
        hostname: grpc.Hostname.clone(),
        mounts,
        hooks,
        annotations: grpc.Annotations.clone(),
        linux,
        solaris: None,
        windows: None,
        vm: None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[macro_export]
    macro_rules! skip_if_not_root {
        () => {
            if !nix::unistd::Uid::effective().is_root() {
                println!("INFO: skipping {} which needs root", module_path!());
                return;
            }
        };
    }

    // Skip a test unless the given capability is present in the effective set.
    // Being uid 0 is not sufficient: container/CI environments frequently drop
    // capabilities such as CAP_MKNOD or CAP_SYS_ADMIN from root, and the
    // corresponding syscalls then fail with EPERM.
    //
    // NOTE: the agent crate has a sibling `skip_if_no_cap!` in
    // agent/src/test_utils.rs. rustjail can't depend on agent's test-utils, so
    // the two are intentionally duplicated — keep them behaviourally in sync.
    #[macro_export]
    macro_rules! skip_if_no_cap {
        ($cap:expr) => {
            match caps::has_cap(None, caps::CapSet::Effective, $cap) {
                Ok(true) => {}
                _ => {
                    println!(
                        "INFO: skipping {} which needs capability {:?}",
                        module_path!(),
                        $cap
                    );
                    return;
                }
            }
        };
    }

    // Parameters:
    //
    // 1: expected Result
    // 2: actual Result
    // 3: string used to identify the test on error
    #[macro_export]
    macro_rules! assert_result {
        ($expected_result:expr, $actual_result:expr, $msg:expr) => {
            if $expected_result.is_ok() {
                let expected_value = $expected_result.as_ref().unwrap();
                let actual_value = $actual_result.unwrap();
                assert!(*expected_value == actual_value, "{}", $msg);
            } else {
                assert!($actual_result.is_err(), "{}", $msg);

                let expected_error = $expected_result.as_ref().unwrap_err();
                let expected_error_msg = format!("{:?}", expected_error);

                let actual_error_msg = format!("{:?}", $actual_result.unwrap_err());

                assert!(expected_error_msg == actual_error_msg, "{}", $msg);
            }
        };
    }

    #[test]
    fn test_process_grpc_to_oci() {
        #[derive(Debug)]
        struct TestData {
            grpcproc: grpc::Process,
            result: oci::Process,
        }

        let tests = &[
            TestData {
                // All fields specified
                grpcproc: grpc::Process {
                    Terminal: true,
                    ConsoleSize: protobuf::MessageField::<grpc::Box>::some(grpc::Box {
                        Height: 123,
                        Width: 456,
                        ..Default::default()
                    }),
                    User: protobuf::MessageField::<grpc::User>::some(grpc::User {
                        UID: 1234,
                        GID: 5678,
                        AdditionalGids: Vec::from([910, 1112]),
                        Username: String::from("username"),
                        ..Default::default()
                    }),
                    Args: Vec::from([String::from("arg1"), String::from("arg2")]),
                    Env: Vec::from([String::from("env")]),
                    Cwd: String::from("cwd"),
                    Capabilities: protobuf::MessageField::some(grpc::LinuxCapabilities {
                        Bounding: Vec::from([String::from("bnd")]),
                        Effective: Vec::from([String::from("eff")]),
                        Inheritable: Vec::from([String::from("inher")]),
                        Permitted: Vec::from([String::from("perm")]),
                        Ambient: Vec::from([String::from("amb")]),
                        ..Default::default()
                    }),
                    Rlimits: Vec::from([
                        grpc::POSIXRlimit {
                            Type: String::from("r#type"),
                            Hard: 123,
                            Soft: 456,
                            ..Default::default()
                        },
                        grpc::POSIXRlimit {
                            Type: String::from("r#type2"),
                            Hard: 789,
                            Soft: 1011,
                            ..Default::default()
                        },
                    ]),
                    NoNewPrivileges: true,
                    ApparmorProfile: String::from("apparmor profile"),
                    OOMScoreAdj: 123456,
                    SelinuxLabel: String::from("Selinux Label"),
                    ..Default::default()
                },
                result: oci::Process {
                    terminal: true,
                    console_size: Some(oci::Box {
                        height: 123,
                        width: 456,
                    }),
                    user: oci::User {
                        uid: 1234,
                        gid: 5678,
                        additional_gids: Vec::from([910, 1112]),
                        username: String::from("username"),
                    },
                    args: Vec::from([String::from("arg1"), String::from("arg2")]),
                    env: Vec::from([String::from("env")]),
                    cwd: String::from("cwd"),
                    capabilities: Some(oci::LinuxCapabilities {
                        bounding: Vec::from([String::from("bnd")]),
                        effective: Vec::from([String::from("eff")]),
                        inheritable: Vec::from([String::from("inher")]),
                        permitted: Vec::from([String::from("perm")]),
                        ambient: Vec::from([String::from("amb")]),
                    }),
                    rlimits: Vec::from([
                        oci::PosixRlimit {
                            r#type: String::from("r#type"),
                            hard: 123,
                            soft: 456,
                        },
                        oci::PosixRlimit {
                            r#type: String::from("r#type2"),
                            hard: 789,
                            soft: 1011,
                        },
                    ]),
                    no_new_privileges: true,
                    apparmor_profile: String::from("apparmor profile"),
                    oom_score_adj: Some(123456),
                    selinux_label: String::from("Selinux Label"),
                },
            },
            TestData {
                // None ConsoleSize
                grpcproc: grpc::Process {
                    ConsoleSize: protobuf::MessageField::<grpc::Box>::none(),
                    OOMScoreAdj: 0,
                    ..Default::default()
                },
                result: oci::Process {
                    console_size: None,
                    oom_score_adj: Some(0),
                    ..Default::default()
                },
            },
            TestData {
                // None User
                grpcproc: grpc::Process {
                    User: protobuf::MessageField::<grpc::User>::none(),
                    OOMScoreAdj: 0,
                    ..Default::default()
                },
                result: oci::Process {
                    user: oci::User {
                        uid: 0,
                        gid: 0,
                        additional_gids: vec![],
                        username: String::from(""),
                    },
                    oom_score_adj: Some(0),
                    ..Default::default()
                },
            },
            TestData {
                // None Capabilities
                grpcproc: grpc::Process {
                    Capabilities: protobuf::MessageField::none(),
                    OOMScoreAdj: 0,
                    ..Default::default()
                },
                result: oci::Process {
                    capabilities: None,
                    oom_score_adj: Some(0),
                    ..Default::default()
                },
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let result = process_grpc_to_oci(&d.grpcproc);

            let msg = format!("{}, result: {:?}", msg, result);

            assert_eq!(d.result, result, "{}", msg);
        }
    }

    #[test]
    fn test_root_grpc_to_oci() {
        #[derive(Debug)]
        struct TestData {
            grpcroot: grpc::Root,
            result: oci::Root,
        }

        let tests = &[
            TestData {
                // Default fields
                grpcroot: grpc::Root {
                    ..Default::default()
                },
                result: oci::Root {
                    ..Default::default()
                },
            },
            TestData {
                // Specified fields, readonly false
                grpcroot: grpc::Root {
                    Path: String::from("path"),
                    Readonly: false,
                    ..Default::default()
                },
                result: oci::Root {
                    path: String::from("path"),
                    readonly: false,
                    ..Default::default()
                },
            },
            TestData {
                // Specified fields, readonly true
                grpcroot: grpc::Root {
                    Path: String::from("path"),
                    Readonly: true,
                    ..Default::default()
                },
                result: oci::Root {
                    path: String::from("path"),
                    readonly: true,
                    ..Default::default()
                },
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let result = root_grpc_to_oci(&d.grpcroot);

            let msg = format!("{}, result: {:?}", msg, result);

            assert_eq!(d.result, result, "{}", msg);
        }
    }

    #[test]
    fn test_hooks_grpc_to_oci() {
        #[derive(Debug)]
        struct TestData {
            grpchooks: grpc::Hooks,
            result: oci::Hooks,
        }

        let tests = &[
            TestData {
                // Default fields
                grpchooks: grpc::Hooks {
                    ..Default::default()
                },
                result: oci::Hooks {
                    ..Default::default()
                },
            },
            TestData {
                // All specified
                grpchooks: grpc::Hooks {
                    Prestart: Vec::from([
                        grpc::Hook {
                            Path: String::from("prestartpath"),
                            Args: Vec::from([String::from("arg1"), String::from("arg2")]),
                            Env: Vec::from([String::from("env1"), String::from("env2")]),
                            Timeout: 10,
                            ..Default::default()
                        },
                        grpc::Hook {
                            Path: String::from("prestartpath2"),
                            Args: Vec::from([String::from("arg3"), String::from("arg4")]),
                            Env: Vec::from([String::from("env3"), String::from("env4")]),
                            Timeout: 25,
                            ..Default::default()
                        },
                    ]),
                    Poststart: Vec::from([grpc::Hook {
                        Path: String::from("poststartpath"),
                        Args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        Env: Vec::from([String::from("env1"), String::from("env2")]),
                        Timeout: 10,
                        ..Default::default()
                    }]),
                    Poststop: Vec::from([grpc::Hook {
                        Path: String::from("poststoppath"),
                        Args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        Env: Vec::from([String::from("env1"), String::from("env2")]),
                        Timeout: 10,
                        ..Default::default()
                    }]),
                    ..Default::default()
                },
                result: oci::Hooks {
                    prestart: Vec::from([
                        oci::Hook {
                            path: String::from("prestartpath"),
                            args: Vec::from([String::from("arg1"), String::from("arg2")]),
                            env: Vec::from([String::from("env1"), String::from("env2")]),
                            timeout: Some(10),
                        },
                        oci::Hook {
                            path: String::from("prestartpath2"),
                            args: Vec::from([String::from("arg3"), String::from("arg4")]),
                            env: Vec::from([String::from("env3"), String::from("env4")]),
                            timeout: Some(25),
                        },
                    ]),
                    poststart: Vec::from([oci::Hook {
                        path: String::from("poststartpath"),
                        args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        env: Vec::from([String::from("env1"), String::from("env2")]),
                        timeout: Some(10),
                    }]),
                    poststop: Vec::from([oci::Hook {
                        path: String::from("poststoppath"),
                        args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        env: Vec::from([String::from("env1"), String::from("env2")]),
                        timeout: Some(10),
                    }]),
                },
            },
            TestData {
                // Prestart empty
                grpchooks: grpc::Hooks {
                    Prestart: Vec::from([]),
                    Poststart: Vec::from([grpc::Hook {
                        Path: String::from("poststartpath"),
                        Args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        Env: Vec::from([String::from("env1"), String::from("env2")]),
                        Timeout: 10,
                        ..Default::default()
                    }]),
                    Poststop: Vec::from([grpc::Hook {
                        Path: String::from("poststoppath"),
                        Args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        Env: Vec::from([String::from("env1"), String::from("env2")]),
                        Timeout: 10,
                        ..Default::default()
                    }]),
                    ..Default::default()
                },
                result: oci::Hooks {
                    prestart: Vec::from([]),
                    poststart: Vec::from([oci::Hook {
                        path: String::from("poststartpath"),
                        args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        env: Vec::from([String::from("env1"), String::from("env2")]),
                        timeout: Some(10),
                    }]),
                    poststop: Vec::from([oci::Hook {
                        path: String::from("poststoppath"),
                        args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        env: Vec::from([String::from("env1"), String::from("env2")]),
                        timeout: Some(10),
                    }]),
                },
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let result = hooks_grpc_to_oci(&d.grpchooks);

            let msg = format!("{}, result: {:?}", msg, result);

            assert_eq!(d.result, result, "{}", msg);
        }
    }

    #[test]
    fn test_mount_grpc_to_oci() {
        #[derive(Debug)]
        struct TestData {
            grpcmount: grpc::Mount,
            result: oci::Mount,
        }

        let tests = &[
            TestData {
                // Default fields
                grpcmount: grpc::Mount {
                    ..Default::default()
                },
                result: oci::Mount {
                    ..Default::default()
                },
            },
            TestData {
                grpcmount: grpc::Mount {
                    destination: String::from("destination"),
                    source: String::from("source"),
                    type_: String::from("fieldtype"),
                    options: Vec::from([String::from("option1"), String::from("option2")]),
                    ..Default::default()
                },
                result: oci::Mount {
                    destination: String::from("destination"),
                    source: String::from("source"),
                    r#type: String::from("fieldtype"),
                    options: Vec::from([String::from("option1"), String::from("option2")]),
                },
            },
            TestData {
                grpcmount: grpc::Mount {
                    destination: String::from("destination"),
                    source: String::from("source"),
                    type_: String::from("fieldtype"),
                    options: Vec::new(),
                    ..Default::default()
                },
                result: oci::Mount {
                    destination: String::from("destination"),
                    source: String::from("source"),
                    r#type: String::from("fieldtype"),
                    options: Vec::new(),
                },
            },
            TestData {
                grpcmount: grpc::Mount {
                    destination: String::new(),
                    source: String::from("source"),
                    type_: String::from("fieldtype"),
                    options: Vec::from([String::from("option1")]),
                    ..Default::default()
                },
                result: oci::Mount {
                    destination: String::new(),
                    source: String::from("source"),
                    r#type: String::from("fieldtype"),
                    options: Vec::from([String::from("option1")]),
                },
            },
            TestData {
                grpcmount: grpc::Mount {
                    destination: String::from("destination"),
                    source: String::from("source"),
                    type_: String::new(),
                    options: Vec::from([String::from("option1")]),
                    ..Default::default()
                },
                result: oci::Mount {
                    destination: String::from("destination"),
                    source: String::from("source"),
                    r#type: String::new(),
                    options: Vec::from([String::from("option1")]),
                },
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let result = mount_grpc_to_oci(&d.grpcmount);

            let msg = format!("{}, result: {:?}", msg, result);

            assert_eq!(d.result, result, "{}", msg);
        }
    }

    #[test]
    fn test_hook_grpc_to_oci<'a>() {
        #[derive(Debug)]
        struct TestData<'a> {
            grpchook: &'a [grpc::Hook],
            result: Vec<oci::Hook>,
        }

        let tests = &[
            TestData {
                // Default fields
                grpchook: &[
                    grpc::Hook {
                        Timeout: 0,
                        ..Default::default()
                    },
                    grpc::Hook {
                        Timeout: 0,
                        ..Default::default()
                    },
                ],
                result: vec![
                    oci::Hook {
                        timeout: Some(0),
                        ..Default::default()
                    },
                    oci::Hook {
                        timeout: Some(0),
                        ..Default::default()
                    },
                ],
            },
            TestData {
                // Specified fields
                grpchook: &[
                    grpc::Hook {
                        Path: String::from("path"),
                        Args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        Env: Vec::from([String::from("env1"), String::from("env2")]),
                        Timeout: 10,
                        ..Default::default()
                    },
                    grpc::Hook {
                        Path: String::from("path2"),
                        Args: Vec::from([String::from("arg3"), String::from("arg4")]),
                        Env: Vec::from([String::from("env3"), String::from("env4")]),
                        Timeout: 20,
                        ..Default::default()
                    },
                ],
                result: vec![
                    oci::Hook {
                        path: String::from("path"),
                        args: Vec::from([String::from("arg1"), String::from("arg2")]),
                        env: Vec::from([String::from("env1"), String::from("env2")]),
                        timeout: Some(10),
                    },
                    oci::Hook {
                        path: String::from("path2"),
                        args: Vec::from([String::from("arg3"), String::from("arg4")]),
                        env: Vec::from([String::from("env3"), String::from("env4")]),
                        timeout: Some(20),
                    },
                ],
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let result = hook_grpc_to_oci(d.grpchook);

            let msg = format!("{}, result: {:?}", msg, result);

            assert_eq!(d.result, result, "{}", msg);
        }
    }
}
