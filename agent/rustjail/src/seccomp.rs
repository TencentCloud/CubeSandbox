// Copyright 2021 Sony Group Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Result};
use libseccomp::*;
use oci::{LinuxSeccomp, LinuxSeccompArg};
use std::str::FromStr;

fn get_filter_attr_from_flag(flag: &str) -> Result<ScmpFilterAttr> {
    match flag {
        "SECCOMP_FILTER_FLAG_TSYNC" => Ok(ScmpFilterAttr::CtlTsync),
        "SECCOMP_FILTER_FLAG_LOG" => Ok(ScmpFilterAttr::CtlLog),
        "SECCOMP_FILTER_FLAG_SPEC_ALLOW" => Ok(ScmpFilterAttr::CtlSsb),
        _ => Err(anyhow!("Invalid seccomp flag")),
    }
}

// get_rule_conditions gets rule conditions for a system call from the args.
fn get_rule_conditions(args: &[LinuxSeccompArg]) -> Result<Vec<ScmpArgCompare>> {
    let mut conditions: Vec<ScmpArgCompare> = Vec::new();

    for arg in args {
        if arg.op.is_empty() {
            return Err(anyhow!("seccomp operator is required"));
        }

        let cond = ScmpArgCompare::new(
            arg.index,
            ScmpCompareOp::from_str(&arg.op)?,
            arg.value,
            Some(arg.value_two),
        );

        conditions.push(cond);
    }

    Ok(conditions)
}

pub fn get_unknown_syscalls(scmp: &LinuxSeccomp) -> Option<Vec<String>> {
    let mut unknown_syscalls: Vec<String> = Vec::new();

    for syscall in &scmp.syscalls {
        for name in &syscall.names {
            if get_syscall_from_name(name, None).is_err() {
                unknown_syscalls.push(name.to_string());
            }
        }
    }

    if unknown_syscalls.is_empty() {
        None
    } else {
        Some(unknown_syscalls)
    }
}

// init_seccomp creates a seccomp filter and loads it for the current process
// including all the child processes.
pub fn init_seccomp(scmp: &LinuxSeccomp) -> Result<()> {
    let def_action = ScmpAction::from_str(scmp.default_action.as_str(), Some(libc::EPERM as u32))?;

    // Create a new filter context
    let mut filter = ScmpFilterContext::new_filter(def_action)?;

    // Add extra architectures
    for arch in &scmp.architectures {
        let scmp_arch = ScmpArch::from_str(arch)?;
        filter.add_arch(scmp_arch)?;
    }

    // Unset no new privileges bit
    filter.set_no_new_privs_bit(false)?;

    // Add a rule for each system call
    for syscall in &scmp.syscalls {
        if syscall.names.is_empty() {
            return Err(anyhow!("syscall name is required"));
        }

        let action = ScmpAction::from_str(&syscall.action, Some(syscall.errno_ret))?;
        if action == def_action {
            continue;
        }

        for name in &syscall.names {
            let syscall_num = match get_syscall_from_name(name, None) {
                Ok(num) => num,
                Err(_) => {
                    // If we cannot resolve the given system call, we assume it is not supported
                    // by the kernel. Hence, we skip it without generating an error.
                    continue;
                }
            };

            if syscall.args.is_empty() {
                filter.add_rule(action, syscall_num, None)?;
            } else {
                let conditions = get_rule_conditions(&syscall.args)?;
                filter.add_rule(action, syscall_num, Some(&conditions))?;
            }
        }
    }

    // Set filter attributes for each seccomp flag
    for flag in &scmp.flags {
        let scmp_attr = get_filter_attr_from_flag(flag)?;
        filter.set_filter_attr(scmp_attr, 1)?;
    }

    // Load the filter
    filter.load()?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::skip_if_not_root;
    use libc::{dup3, process_vm_readv, EPERM, O_CLOEXEC};
    use std::io::Error;
    use std::ptr::null;

    macro_rules! syscall_assert {
        ($e1: expr, $e2: expr) => {
            let mut errno: i32 = 0;
            if $e1 < 0 {
                errno = -Error::last_os_error().raw_os_error().unwrap();
            }
            assert_eq!(errno, $e2);
        };
    }

    const TEST_DATA: &str = r#"{
          "defaultAction": "SCMP_ACT_ALLOW",
          "architectures": [
          ],
          "flags": [
              "SECCOMP_FILTER_FLAG_LOG"
          ],
          "syscalls": [
              {
                 "names": [
                      "dup3",
                      "invalid_syscall1",
                      "invalid_syscall2"
                  ],
                  "action": "SCMP_ACT_ERRNO"
              },
              {
                 "names": [
                      "process_vm_readv"
                  ],
                  "action": "SCMP_ACT_ERRNO",
                  "errnoRet": 111,
                  "args": [
                      {
                          "index": 0,
                          "value": 10,
                          "op": "SCMP_CMP_EQ"
                      }
                  ]
              },
              {
                 "names": [
                      "process_vm_readv"
                  ],
                  "action": "SCMP_ACT_ERRNO",
                  "errnoRet": 111,
                  "args": [
                      {
                          "index": 0,
                          "value": 20,
                          "op": "SCMP_CMP_EQ"
                      }
                  ]
              },
              {
                 "names": [
                      "process_vm_readv"
                  ],
                  "action": "SCMP_ACT_ERRNO",
                  "errnoRet": 222,
                  "args": [
                      {
                          "index": 0,
                          "value": 30,
                          "op": "SCMP_CMP_EQ"
                      },
                      {
                          "index": 2,
                          "value": 40,
                          "op": "SCMP_CMP_EQ"
                      }
                  ]
              }
          ]
      }"#;

    #[test]
    fn test_get_filter_attr_from_flag() {
        skip_if_not_root!();

        assert_eq!(
            get_filter_attr_from_flag("SECCOMP_FILTER_FLAG_TSYNC").unwrap(),
            ScmpFilterAttr::CtlTsync
        );

        assert_eq!(get_filter_attr_from_flag("ERROR").is_err(), true);
    }

    #[test]
    fn test_get_unknown_syscalls() {
        let scmp: oci::LinuxSeccomp = serde_json::from_str(TEST_DATA).unwrap();
        let syscalls = get_unknown_syscalls(&scmp).unwrap();

        assert_eq!(syscalls, vec!["invalid_syscall1", "invalid_syscall2"]);
    }

    #[test]
    fn test_init_seccomp() {
        skip_if_not_root!();

        let mut scmp: oci::LinuxSeccomp = serde_json::from_str(TEST_DATA).unwrap();
        let mut arch: Vec<oci::Arch>;

        if cfg!(target_endian = "little") {
            // For little-endian architectures
            arch = vec![
                "SCMP_ARCH_X86".to_string(),
                "SCMP_ARCH_X32".to_string(),
                "SCMP_ARCH_X86_64".to_string(),
                "SCMP_ARCH_AARCH64".to_string(),
                "SCMP_ARCH_ARM".to_string(),
                "SCMP_ARCH_PPC64LE".to_string(),
            ];
        } else {
            // For big-endian architectures
            arch = vec!["SCMP_ARCH_S390X".to_string()];
        }

        scmp.architectures.append(&mut arch);

        init_seccomp(&scmp).unwrap();

        // Basic syscall with simple rule
        syscall_assert!(unsafe { dup3(0, 1, O_CLOEXEC) }, -EPERM);

        // Syscall with permitted arguments
        syscall_assert!(unsafe { process_vm_readv(1, null(), 0, null(), 0, 0) }, 0);

        // Multiple arguments with OR rules with ERRNO
        syscall_assert!(
            unsafe { process_vm_readv(10, null(), 0, null(), 0, 0) },
            -111
        );
        syscall_assert!(
            unsafe { process_vm_readv(20, null(), 0, null(), 0, 0) },
            -111
        );

        // Multiple arguments with AND rules with ERRNO
        syscall_assert!(unsafe { process_vm_readv(30, null(), 0, null(), 0, 0) }, 0);
        syscall_assert!(
            unsafe { process_vm_readv(30, null(), 40, null(), 0, 0) },
            -222
        );
    }
}
