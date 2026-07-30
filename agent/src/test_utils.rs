// Copyright (c) 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//
#![allow(clippy::module_inception)]

#[cfg(test)]
pub mod test_utils {
    #[derive(Debug, PartialEq)]
    pub enum TestUserType {
        RootOnly,
        NonRootOnly,
        Any,
    }

    #[macro_export]
    macro_rules! skip_if_root {
        () => {
            if nix::unistd::Uid::effective().is_root() {
                println!("INFO: skipping {} which needs non-root", module_path!());
                return;
            }
        };
    }

    #[macro_export]
    macro_rules! skip_if_not_root {
        () => {
            if !nix::unistd::Uid::effective().is_root() {
                println!("INFO: skipping {} which needs root", module_path!());
                return;
            }
        };
    }

    // Returns true when the effective capability set holds `cap`. Running as
    // uid 0 is not sufficient: container/CI environments frequently drop
    // CAP_SYS_ADMIN or CAP_NET_ADMIN from root, and the affected syscalls
    // (mount, unshare, netlink link changes, ...) then fail with EPERM.
    pub fn have_effective_cap(cap: capctl::caps::Cap) -> bool {
        match capctl::caps::CapState::get_current() {
            Ok(state) => state.effective.has(cap),
            Err(_) => false,
        }
    }

    // NOTE: rustjail (a separate crate that agent depends on) carries a sibling
    // `skip_if_no_cap!` in agent/rustjail/src/lib.rs. That crate can't import
    // this test-util, so the two are intentionally duplicated — keep them
    // behaviourally in sync.
    #[macro_export]
    macro_rules! skip_if_no_cap {
        ($cap:expr) => {
            if !$crate::test_utils::test_utils::have_effective_cap($cap) {
                println!(
                    "INFO: skipping {} which needs capability {:?}",
                    module_path!(),
                    $cap
                );
                return;
            }
        };
    }

    // Probe whether the cgroup filesystem is writable, i.e. whether we can
    // actually create a cgroup. Root inside the builder container sees a
    // read-only /sys/fs/cgroup, so LinuxContainer::new fails even though the
    // uid is 0.
    pub fn cgroupfs_writable() -> bool {
        if !have_effective_cap(capctl::caps::Cap::SYS_ADMIN) {
            return false;
        }
        let base = if cgroups::hierarchies::is_cgroup2_unified_mode() {
            "/sys/fs/cgroup".to_string()
        } else {
            "/sys/fs/cgroup/pids".to_string()
        };
        let probe = format!("{}/cube_agent_test_probe_{}", base, std::process::id());
        match std::fs::create_dir(&probe) {
            Ok(_) => {
                let _ = std::fs::remove_dir(&probe);
                true
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                let _ = std::fs::remove_dir(&probe);
                true
            }
            Err(_) => false,
        }
    }

    #[macro_export]
    macro_rules! skip_loop_if_no_cap {
        ($msg:expr, $cap:expr) => {
            if !$crate::test_utils::test_utils::have_effective_cap($cap) {
                println!(
                    "INFO: skipping loop {} in {} which needs capability {:?}",
                    $msg,
                    module_path!(),
                    $cap
                );
                continue;
            }
        };
    }

    #[macro_export]
    macro_rules! skip_if_no_cgroupfs {
        () => {
            if !$crate::test_utils::test_utils::cgroupfs_writable() {
                println!(
                    "INFO: skipping {} which needs a writable cgroup filesystem",
                    module_path!()
                );
                return;
            }
        };
    }

    #[macro_export]
    macro_rules! skip_loop_if_root {
        ($msg:expr) => {
            if nix::unistd::Uid::effective().is_root() {
                println!(
                    "INFO: skipping loop {} in {} which needs non-root",
                    $msg,
                    module_path!()
                );
                continue;
            }
        };
    }

    #[macro_export]
    macro_rules! skip_loop_if_not_root {
        ($msg:expr) => {
            if !nix::unistd::Uid::effective().is_root() {
                println!(
                    "INFO: skipping loop {} in {} which needs root",
                    $msg,
                    module_path!()
                );
                continue;
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

    #[macro_export]
    macro_rules! skip_loop_by_user {
        ($msg:expr, $user:expr) => {
            if $user == TestUserType::RootOnly {
                skip_loop_if_not_root!($msg);
            } else if $user == TestUserType::NonRootOnly {
                skip_loop_if_root!($msg);
            }
        };
    }
}
