// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use std::convert::TryInto;

use seccompiler::{
    BpfProgram, Error, SeccompAction, SeccompCmpArgLen as ArgLen, SeccompCmpOp::Eq,
    SeccompCondition as Cond, SeccompFilter, SeccompRule,
};

pub enum Thread {
    VirtioBalloon,
    VirtioBlock,
    VirtioConsole,
    VirtioIommu,
    VirtioMem,
    VirtioNet,
    VirtioNetCtl,
    VirtioPmem,
    VirtioRng,
    VirtioVhostBlock,
    VirtioVhostFs,
    VirtioVhostNet,
    VirtioVhostNetCtl,
    VirtioVsock,
    VirtioWatchdog,
    VirtioFs,
}

/// Shorthand for chaining `SeccompCondition`s with the `and` operator  in a `SeccompRule`.
/// The rule will take the `Allow` action if _all_ the conditions are true.
///
/// [`SeccompCondition`]: struct.SeccompCondition.html
/// [`SeccompRule`]: struct.SeccompRule.html
macro_rules! and {
    ($($x:expr),*) => (SeccompRule::new(vec![$($x),*]).unwrap())
}

/// Shorthand for chaining `SeccompRule`s with the `or` operator in a `SeccompFilter`.
///
/// [`SeccompFilter`]: struct.SeccompFilter.html
/// [`SeccompRule`]: struct.SeccompRule.html
macro_rules! or {
    ($($x:expr,)*) => (vec![$($x),*]);
    ($($x:expr),*) => (vec![$($x),*])
}

// See include/uapi/asm-generic/ioctls.h in the kernel code.
const TIOCGWINSZ: u64 = 0x5413;
const FIONBIO: u64 = 0x5421;

// See include/uapi/linux/vfio.h in the kernel code.
const VFIO_IOMMU_MAP_DMA: u64 = 0x3b71;
const VFIO_IOMMU_UNMAP_DMA: u64 = 0x3b72;

// See include/uapi/linux/if_tun.h in the kernel code.
const TUNSETOFFLOAD: u64 = 0x4004_54d0;

fn create_virtio_console_ioctl_seccomp_rule() -> Vec<SeccompRule> {
    or![and![Cond::new(1, ArgLen::Dword, Eq, TIOCGWINSZ).unwrap()]]
}

fn create_virtio_iommu_ioctl_seccomp_rule() -> Vec<SeccompRule> {
    or![
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_IOMMU_MAP_DMA).unwrap()],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_IOMMU_UNMAP_DMA).unwrap()],
    ]
}

fn create_virtio_mem_ioctl_seccomp_rule() -> Vec<SeccompRule> {
    or![
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_IOMMU_MAP_DMA).unwrap()],
        and![Cond::new(1, ArgLen::Dword, Eq, VFIO_IOMMU_UNMAP_DMA).unwrap()],
    ]
}

fn virtio_balloon_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![(libc::SYS_fallocate, vec![])]
}

fn virtio_block_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_fallocate, vec![]),
        (libc::SYS_fdatasync, vec![]),
        (libc::SYS_fsync, vec![]),
        (libc::SYS_ftruncate, vec![]),
        (libc::SYS_getrandom, vec![]),
        (libc::SYS_io_uring_enter, vec![]),
        (libc::SYS_lseek, vec![]),
        (libc::SYS_prctl, vec![]),
        (libc::SYS_pread64, vec![]),
        (libc::SYS_preadv, vec![]),
        (libc::SYS_pwritev, vec![]),
        (libc::SYS_pwrite64, vec![]),
        (libc::SYS_sched_getaffinity, vec![]),
        (libc::SYS_set_robust_list, vec![]),
        (libc::SYS_timerfd_settime, vec![]),
    ]
}

fn virtio_console_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_ioctl, create_virtio_console_ioctl_seccomp_rule()),
        (libc::SYS_prctl, vec![]),
        (libc::SYS_sched_getaffinity, vec![]),
        (libc::SYS_set_robust_list, vec![]),
    ]
}

fn virtio_iommu_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![(libc::SYS_ioctl, create_virtio_iommu_ioctl_seccomp_rule())]
}

fn virtio_mem_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_fallocate, vec![]),
        (libc::SYS_ioctl, create_virtio_mem_ioctl_seccomp_rule()),
        (libc::SYS_recvfrom, vec![]),
        (libc::SYS_sendmsg, vec![]),
    ]
}

fn virtio_net_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_readv, vec![]),
        (libc::SYS_timerfd_settime, vec![]),
        (libc::SYS_writev, vec![]),
    ]
}

fn create_virtio_net_ctl_ioctl_seccomp_rule() -> Vec<SeccompRule> {
    or![and![Cond::new(1, ArgLen::Dword, Eq, TUNSETOFFLOAD).unwrap()],]
}

fn virtio_net_ctl_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![(libc::SYS_ioctl, create_virtio_net_ctl_ioctl_seccomp_rule())]
}

fn virtio_pmem_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![(libc::SYS_fsync, vec![])]
}

fn virtio_rng_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_prctl, vec![]),
        (libc::SYS_sched_getaffinity, vec![]),
        (libc::SYS_set_robust_list, vec![]),
    ]
}

fn virtio_vhost_fs_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_connect, vec![]),
        (libc::SYS_nanosleep, vec![]),
        (libc::SYS_pread64, vec![]),
        (libc::SYS_pwrite64, vec![]),
        (libc::SYS_recvmsg, vec![]),
        (libc::SYS_sendmsg, vec![]),
        (libc::SYS_sendto, vec![]),
        (libc::SYS_socket, vec![]),
    ]
}

fn virtio_vhost_net_ctl_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![]
}

fn virtio_vhost_net_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_accept4, vec![]),
        (libc::SYS_bind, vec![]),
        (libc::SYS_getcwd, vec![]),
        (libc::SYS_listen, vec![]),
        (libc::SYS_recvmsg, vec![]),
        (libc::SYS_sendmsg, vec![]),
        (libc::SYS_sendto, vec![]),
        (libc::SYS_socket, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_unlink, vec![]),
        #[cfg(target_arch = "aarch64")]
        (libc::SYS_unlinkat, vec![]),
    ]
}

fn virtio_vhost_block_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![]
}

fn create_vsock_ioctl_seccomp_rule() -> Vec<SeccompRule> {
    or![and![Cond::new(1, ArgLen::Dword, Eq, FIONBIO,).unwrap()],]
}

fn create_vsock_fcntl_seccomp_rule() -> Vec<SeccompRule> {
    or![
        and![Cond::new(1, ArgLen::Dword, Eq, libc::F_GETFD as u64).unwrap()],
        and![Cond::new(1, ArgLen::Dword, Eq, libc::F_GETFL as u64).unwrap()],
        and![Cond::new(1, ArgLen::Dword, Eq, libc::F_SETFL as u64).unwrap()],
        and![Cond::new(1, ArgLen::Dword, Eq, libc::F_DUPFD_CLOEXEC as u64).unwrap()],
    ]
}

fn create_vsock_getsockopt_seccomp_rule() -> Vec<SeccompRule> {
    vec![and![
        Cond::new(1, ArgLen::Dword, Eq, libc::SOL_SOCKET as u64).unwrap(),
        Cond::new(2, ArgLen::Dword, Eq, libc::SO_PEERCRED as u64).unwrap()
    ]]
}

fn create_vsock_recvmsg_seccomp_rule() -> Vec<SeccompRule> {
    vec![and![Cond::new(2, ArgLen::Dword, Eq, 0).unwrap()]]
}

fn virtio_vsock_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_accept4, vec![]),
        (libc::SYS_connect, vec![]),
        // Passfd validates received SCM_RIGHTS descriptors and sets O_NONBLOCK.
        (libc::SYS_fcntl, create_vsock_fcntl_seccomp_rule()),
        (libc::SYS_fstat, vec![]),
        // VsockMuxer validates every host-side UDS peer with
        // getsockopt(SO_PEERCRED) before accepting its connect/passfd command.
        (libc::SYS_geteuid, vec![]),
        (libc::SYS_getsockopt, create_vsock_getsockopt_seccomp_rule()),
        (libc::SYS_ioctl, create_vsock_ioctl_seccomp_rule()),
        (libc::SYS_recvfrom, vec![]),
        // recv_with_fd() receives passfd commands and SCM_RIGHTS.
        (libc::SYS_recvmsg, create_vsock_recvmsg_seccomp_rule()),
        (libc::SYS_socket, vec![]),
    ]
}

fn virtio_watchdog_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_prctl, vec![]),
        (libc::SYS_sched_getaffinity, vec![]),
        (libc::SYS_set_robust_list, vec![]),
        (libc::SYS_timerfd_settime, vec![]),
    ]
}

fn virtio_fs_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_accept4, vec![]),
        (libc::SYS_brk, vec![]),
        (libc::SYS_capget, vec![]), // For CAP_FSETID
        (libc::SYS_capset, vec![]),
        (libc::SYS_clock_gettime, vec![]),
        (libc::SYS_clone, vec![]),
        (libc::SYS_clone3, vec![]),
        (libc::SYS_close, vec![]),
        (libc::SYS_copy_file_range, vec![]),
        (libc::SYS_dup, vec![]),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_epoll_create, vec![]),
        (libc::SYS_epoll_create1, vec![]),
        (libc::SYS_epoll_ctl, vec![]),
        (libc::SYS_epoll_pwait, vec![]),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_epoll_wait, vec![]),
        (libc::SYS_eventfd2, vec![]),
        (libc::SYS_exit, vec![]),
        (libc::SYS_exit_group, vec![]),
        (libc::SYS_fallocate, vec![]),
        (libc::SYS_fchdir, vec![]),
        (libc::SYS_fchmod, vec![]),
        (libc::SYS_fchmodat, vec![]),
        (libc::SYS_fchownat, vec![]),
        (libc::SYS_fcntl, vec![]),
        (libc::SYS_fdatasync, vec![]),
        (libc::SYS_fgetxattr, vec![]),
        (libc::SYS_flistxattr, vec![]),
        (libc::SYS_flock, vec![]),
        (libc::SYS_fremovexattr, vec![]),
        (libc::SYS_fsetxattr, vec![]),
        (libc::SYS_fstat, vec![]),
        #[cfg(target_arch = "s390x")]
        (libc::SYS_fstatfs64, vec![]),
        (libc::SYS_fstatfs, vec![]),
        (libc::SYS_fsync, vec![]),
        (libc::SYS_ftruncate, vec![]),
        (libc::SYS_futex, vec![]),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_getdents, vec![]),
        (libc::SYS_getdents64, vec![]),
        (libc::SYS_getegid, vec![]),
        (libc::SYS_geteuid, vec![]),
        (libc::SYS_getpid, vec![]),
        (libc::SYS_gettid, vec![]),
        (libc::SYS_gettimeofday, vec![]),
        (libc::SYS_getxattr, vec![]),
        (libc::SYS_linkat, vec![]),
        (libc::SYS_listxattr, vec![]),
        (libc::SYS_lseek, vec![]),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_lstat, vec![]),
        (libc::SYS_madvise, vec![]),
        (libc::SYS_mkdirat, vec![]),
        (libc::SYS_mknodat, vec![]),
        (libc::SYS_mmap, vec![]),
        (libc::SYS_mprotect, vec![]),
        (libc::SYS_mremap, vec![]),
        (libc::SYS_munmap, vec![]),
        (libc::SYS_name_to_handle_at, vec![]),
        (libc::SYS_newfstatat, vec![]),
        #[cfg(any(
            target_arch = "x86_64",
            target_arch = "s390x",
            target_arch = "powerpc64le"
        ))]
        (libc::SYS_open, vec![]),
        (libc::SYS_openat, vec![]),
        (libc::SYS_openat2, vec![]),
        (libc::SYS_open_by_handle_at, vec![]),
        (libc::SYS_prctl, vec![]),
        (libc::SYS_preadv, vec![]),
        (libc::SYS_prlimit64, vec![]),
        (libc::SYS_pwritev, vec![]),
        (libc::SYS_pwritev2, vec![]),
        (libc::SYS_read, vec![]),
        (libc::SYS_readlinkat, vec![]),
        (libc::SYS_recvmsg, vec![]),
        (libc::SYS_renameat, vec![]),
        (libc::SYS_renameat2, vec![]),
        (libc::SYS_removexattr, vec![]),
        (libc::SYS_rt_sigaction, vec![]),
        (libc::SYS_rt_sigprocmask, vec![]),
        (libc::SYS_rt_sigreturn, vec![]),
        (libc::SYS_sched_getaffinity, vec![]),
        (libc::SYS_sendmsg, vec![]),
        (libc::SYS_set_robust_list, vec![]),
        (libc::SYS_setresgid, vec![]),
        (libc::SYS_setresuid, vec![]),
        //(libc::SYS_setresgid32);  Needed on some platforms,
        //(libc::SYS_setresuid32);  Needed on some platforms
        (libc::SYS_setxattr, vec![]),
        (libc::SYS_sigaltstack, vec![]),
        #[cfg(target_arch = "s390x")]
        (libc::SYS_sigreturn, vec![]),
        (libc::SYS_statx, vec![]),
        (libc::SYS_symlinkat, vec![]),
        (libc::SYS_syncfs, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_time, vec![]), // Rarely needed, except on static builds
        (libc::SYS_umask, vec![]),
        (libc::SYS_unshare, vec![]),
        (libc::SYS_utimensat, vec![]),
        (libc::SYS_write, vec![]),
        (libc::SYS_writev, vec![]),
        (libc::SYS_timerfd_create, vec![]),
        (libc::SYS_timerfd_settime, vec![]),
    ]
}

pub fn create_virtio_device_ioctl_seccomp_rule() -> Vec<SeccompRule> {
    let mut common_rules = vec![];
    let rules = create_virtio_console_ioctl_seccomp_rule();
    common_rules.extend(rules);
    let rules = create_virtio_net_ctl_ioctl_seccomp_rule();
    common_rules.extend(rules);
    let rules = create_vsock_ioctl_seccomp_rule();
    common_rules.extend(rules);
    common_rules
}

pub fn virtio_device_thread_rules() -> Vec<(i64, Vec<SeccompRule>)> {
    let mut rules = vec![
        (libc::SYS_gettid, vec![]),
        (libc::SYS_gettimeofday, vec![]),
        (libc::SYS_getuid, vec![]),
        // to support operations in init_backendfs()
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_lstat, vec![]),
        (libc::SYS_prlimit64, vec![]),
    ];

    rules.append(&mut virtio_block_thread_rules());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_net_thread_rules());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_net_ctl_thread_rules());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_pmem_thread_rules());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_rng_thread_rules());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_vsock_thread_rules());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_fs_thread_rules());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);
    rules.append(&mut virtio_thread_common());
    rules.retain(|(num, _)| *num != libc::SYS_ioctl);

    let mut ioctl_rules = vec![(libc::SYS_ioctl, create_virtio_device_ioctl_seccomp_rule())];
    rules.append(&mut ioctl_rules);

    rules
}

fn get_seccomp_rules(thread_type: Thread) -> Vec<(i64, Vec<SeccompRule>)> {
    let mut rules = match thread_type {
        Thread::VirtioBalloon => virtio_balloon_thread_rules(),
        Thread::VirtioBlock => virtio_block_thread_rules(),
        Thread::VirtioConsole => virtio_console_thread_rules(),
        Thread::VirtioIommu => virtio_iommu_thread_rules(),
        Thread::VirtioMem => virtio_mem_thread_rules(),
        Thread::VirtioNet => virtio_net_thread_rules(),
        Thread::VirtioNetCtl => virtio_net_ctl_thread_rules(),
        Thread::VirtioPmem => virtio_pmem_thread_rules(),
        Thread::VirtioRng => virtio_rng_thread_rules(),
        Thread::VirtioVhostBlock => virtio_vhost_block_thread_rules(),
        Thread::VirtioVhostFs => virtio_vhost_fs_thread_rules(),
        Thread::VirtioVhostNet => virtio_vhost_net_thread_rules(),
        Thread::VirtioVhostNetCtl => virtio_vhost_net_ctl_thread_rules(),
        Thread::VirtioVsock => virtio_vsock_thread_rules(),
        Thread::VirtioWatchdog => virtio_watchdog_thread_rules(),
        Thread::VirtioFs => virtio_fs_thread_rules(),
    };
    rules.append(&mut virtio_thread_common());
    rules
}

fn virtio_thread_common() -> Vec<(i64, Vec<SeccompRule>)> {
    vec![
        (libc::SYS_brk, vec![]),
        (libc::SYS_clock_gettime, vec![]),
        (libc::SYS_close, vec![]),
        (libc::SYS_dup, vec![]),
        (libc::SYS_epoll_create1, vec![]),
        (libc::SYS_epoll_ctl, vec![]),
        (libc::SYS_epoll_pwait, vec![]),
        #[cfg(target_arch = "x86_64")]
        (libc::SYS_epoll_wait, vec![]),
        (libc::SYS_exit, vec![]),
        (libc::SYS_futex, vec![]),
        (libc::SYS_madvise, vec![]),
        (libc::SYS_mmap, vec![]),
        (libc::SYS_mprotect, vec![]),
        (libc::SYS_munmap, vec![]),
        (libc::SYS_openat, vec![]),
        (libc::SYS_read, vec![]),
        (libc::SYS_rt_sigprocmask, vec![]),
        (libc::SYS_rt_sigreturn, vec![]),
        (libc::SYS_sigaltstack, vec![]),
        (libc::SYS_write, vec![]),
    ]
}

/// Generate a BPF program based on the seccomp_action value
pub fn get_seccomp_filter(
    seccomp_action: &SeccompAction,
    thread_type: Thread,
) -> Result<BpfProgram, Error> {
    match seccomp_action {
        SeccompAction::Allow => Ok(vec![]),
        SeccompAction::KillProcess => Ok(vec![]),
        SeccompAction::Log => SeccompFilter::new(
            get_seccomp_rules(thread_type).into_iter().collect(),
            SeccompAction::Log,
            SeccompAction::Allow,
            std::env::consts::ARCH.try_into().unwrap(),
        )
        .and_then(|filter| filter.try_into())
        .map_err(Error::Backend),
        _ => SeccompFilter::new(
            get_seccomp_rules(thread_type).into_iter().collect(),
            SeccompAction::Trap,
            SeccompAction::Allow,
            std::env::consts::ARCH.try_into().unwrap(),
        )
        .and_then(|filter| filter.try_into())
        .map_err(Error::Backend),
    }
}
