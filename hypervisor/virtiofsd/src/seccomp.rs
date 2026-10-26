// Copyright 2020 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

use libseccomp_sys::{
    seccomp_init, seccomp_load, seccomp_release, seccomp_rule_add, SCMP_ACT_ALLOW,
    SCMP_ACT_KILL_PROCESS, SCMP_ACT_LOG, SCMP_ACT_TRAP,
};
use std::convert::TryInto;
use std::{error, fmt};

#[derive(Debug)]
pub enum Error {
    /// Error allowing a syscall
    AllowSeccompSyscall(i32),

    /// Cannot load seccomp filter
    LoadSeccompFilter,

    /// Cannot initialize seccomp context
    InitSeccompContext,
}

impl error::Error for Error {}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "virtiofsd_seccomp_error: {self:?}")
    }
}

#[derive(Copy, Clone, Debug)]
pub enum SeccompAction {
    Allow,
    Kill,
    Log,
    Trap,
}

impl From<SeccompAction> for u32 {
    fn from(action: SeccompAction) -> u32 {
        match action {
            SeccompAction::Allow => SCMP_ACT_ALLOW,
            SeccompAction::Kill => SCMP_ACT_KILL_PROCESS,
            SeccompAction::Log => SCMP_ACT_LOG,
            SeccompAction::Trap => SCMP_ACT_TRAP,
        }
    }
}

macro_rules! allow_syscall {
    ($ctx:ident, $syscall:expr) => {
        let syscall_nr: i32 = $syscall.try_into().unwrap();
        let ret = unsafe { seccomp_rule_add($ctx, SCMP_ACT_ALLOW, syscall_nr, 0) };
        if ret != 0 {
            return Err(Error::AllowSeccompSyscall(syscall_nr));
        }
    };
}

pub fn enable_seccomp(action: SeccompAction, allow_remote_logging: bool) -> Result<(), Error> {
    let ctx = unsafe { seccomp_init(action.into()) };
    if ctx.is_null() {
        return Err(Error::InitSeccompContext);
    }

    allow_syscall!(ctx, libc::SYS_accept4);
    allow_syscall!(ctx, libc::SYS_brk);
    allow_syscall!(ctx, libc::SYS_capget); // For CAP_FSETID
    allow_syscall!(ctx, libc::SYS_capset);
    allow_syscall!(ctx, libc::SYS_clock_gettime);
    allow_syscall!(ctx, libc::SYS_clone);
    allow_syscall!(ctx, libc::SYS_clone3);
    allow_syscall!(ctx, libc::SYS_close);
    allow_syscall!(ctx, libc::SYS_copy_file_range);
    allow_syscall!(ctx, libc::SYS_dup);
    #[cfg(any(
        target_arch = "x86_64",
        target_arch = "s390x",
        target_arch = "powerpc64"
    ))]
    allow_syscall!(ctx, libc::SYS_epoll_create);
    allow_syscall!(ctx, libc::SYS_epoll_create1);
    allow_syscall!(ctx, libc::SYS_epoll_ctl);
    allow_syscall!(ctx, libc::SYS_epoll_pwait);
    #[cfg(any(
        target_arch = "x86_64",
        target_arch = "s390x",
        target_arch = "powerpc64"
    ))]
    allow_syscall!(ctx, libc::SYS_epoll_wait);
    allow_syscall!(ctx, libc::SYS_eventfd2);
    allow_syscall!(ctx, libc::SYS_exit);
    allow_syscall!(ctx, libc::SYS_exit_group);
    allow_syscall!(ctx, libc::SYS_fallocate);
    allow_syscall!(ctx, libc::SYS_fchdir);
    allow_syscall!(ctx, libc::SYS_fchmod);
    allow_syscall!(ctx, libc::SYS_fchmodat);
    allow_syscall!(ctx, libc::SYS_fchownat);
    allow_syscall!(ctx, libc::SYS_fcntl);
    allow_syscall!(ctx, libc::SYS_fdatasync);
    allow_syscall!(ctx, libc::SYS_fgetxattr);
    allow_syscall!(ctx, libc::SYS_flistxattr);
    allow_syscall!(ctx, libc::SYS_flock);
    allow_syscall!(ctx, libc::SYS_fremovexattr);
    allow_syscall!(ctx, libc::SYS_fsetxattr);
    #[cfg(not(target_arch = "loongarch64"))]
    allow_syscall!(ctx, libc::SYS_fstat);
    #[cfg(any(target_arch = "s390x", target_arch = "powerpc64"))]
    allow_syscall!(ctx, libc::SYS_fstatfs64);
    allow_syscall!(ctx, libc::SYS_fstatfs);
    allow_syscall!(ctx, libc::SYS_fsync);
    allow_syscall!(ctx, libc::SYS_ftruncate);
    allow_syscall!(ctx, libc::SYS_futex);
    #[cfg(any(
        target_arch = "x86_64",
        target_arch = "s390x",
        target_arch = "powerpc64"
    ))]
    allow_syscall!(ctx, libc::SYS_getdents);
    allow_syscall!(ctx, libc::SYS_getdents64);
    allow_syscall!(ctx, libc::SYS_getegid);
    allow_syscall!(ctx, libc::SYS_geteuid);
    allow_syscall!(ctx, libc::SYS_getpid);
    allow_syscall!(ctx, libc::SYS_getrandom);
    allow_syscall!(ctx, libc::SYS_gettid);
    allow_syscall!(ctx, libc::SYS_gettimeofday);
    allow_syscall!(ctx, libc::SYS_getxattr);
    allow_syscall!(ctx, libc::SYS_linkat);
    allow_syscall!(ctx, libc::SYS_listxattr);
    allow_syscall!(ctx, libc::SYS_lseek);
    allow_syscall!(ctx, libc::SYS_madvise);
    allow_syscall!(ctx, libc::SYS_membarrier);
    allow_syscall!(ctx, libc::SYS_mkdirat);
    allow_syscall!(ctx, libc::SYS_mknodat);
    allow_syscall!(ctx, libc::SYS_mmap);
    allow_syscall!(ctx, libc::SYS_mprotect);
    allow_syscall!(ctx, libc::SYS_mremap);
    allow_syscall!(ctx, libc::SYS_munmap);
    allow_syscall!(ctx, libc::SYS_name_to_handle_at);
    #[cfg(not(target_arch = "loongarch64"))]
    allow_syscall!(ctx, libc::SYS_newfstatat);
    #[cfg(target_arch = "powerpc64")]
    allow_syscall!(ctx, libc::SYS__llseek);
    #[cfg(any(
        target_arch = "x86_64",
        target_arch = "s390x",
        target_arch = "powerpc64"
    ))]
    allow_syscall!(ctx, libc::SYS_open);
    allow_syscall!(ctx, libc::SYS_openat);
    allow_syscall!(ctx, libc::SYS_openat2);
    allow_syscall!(ctx, libc::SYS_open_by_handle_at);
    allow_syscall!(ctx, libc::SYS_prctl); // TODO restrict to just PR_SET_NAME?
    allow_syscall!(ctx, libc::SYS_preadv);
    allow_syscall!(ctx, libc::SYS_pread64);
    allow_syscall!(ctx, libc::SYS_pwritev2);
    allow_syscall!(ctx, libc::SYS_pwrite64);
    allow_syscall!(ctx, libc::SYS_read);
    allow_syscall!(ctx, libc::SYS_readlinkat);
    allow_syscall!(ctx, libc::SYS_recvmsg);
    #[cfg(not(any(target_arch = "loongarch64", target_arch = "riscv64")))]
    allow_syscall!(ctx, libc::SYS_renameat);
    allow_syscall!(ctx, libc::SYS_renameat2);
    allow_syscall!(ctx, libc::SYS_removexattr);
    #[cfg(target_env = "gnu")]
    allow_syscall!(ctx, libc::SYS_rseq);
    allow_syscall!(ctx, libc::SYS_rt_sigaction);
    allow_syscall!(ctx, libc::SYS_rt_sigprocmask);
    allow_syscall!(ctx, libc::SYS_rt_sigreturn);
    allow_syscall!(ctx, libc::SYS_sched_getaffinity); // used by thread_pool
    allow_syscall!(ctx, libc::SYS_sched_yield);
    allow_syscall!(ctx, libc::SYS_sendmsg);
    allow_syscall!(ctx, libc::SYS_setgroups);
    allow_syscall!(ctx, libc::SYS_setresgid);
    allow_syscall!(ctx, libc::SYS_setresuid);
    //allow_syscall!(ctx, libc::SYS_setresgid32);  Needed on some platforms,
    //allow_syscall!(ctx, libc::SYS_setresuid32);  Needed on some platforms
    allow_syscall!(ctx, libc::SYS_set_robust_list);
    allow_syscall!(ctx, libc::SYS_setxattr);
    allow_syscall!(ctx, libc::SYS_sigaltstack);
    #[cfg(target_arch = "s390x")]
    allow_syscall!(ctx, libc::SYS_sigreturn);
    allow_syscall!(ctx, libc::SYS_statx);
    allow_syscall!(ctx, libc::SYS_symlinkat);
    allow_syscall!(ctx, libc::SYS_syncfs);
    #[cfg(target_arch = "x86_64")]
    allow_syscall!(ctx, libc::SYS_time); // Rarely needed, except on static builds
    allow_syscall!(ctx, libc::SYS_tgkill);
    allow_syscall!(ctx, libc::SYS_umask);
    #[cfg(any(
        target_arch = "x86_64",
        target_arch = "s390x",
        target_arch = "powerpc64"
    ))]
    allow_syscall!(ctx, libc::SYS_unlink);
    allow_syscall!(ctx, libc::SYS_unlinkat);
    allow_syscall!(ctx, libc::SYS_unshare);
    allow_syscall!(ctx, libc::SYS_utimensat);
    allow_syscall!(ctx, libc::SYS_write);
    allow_syscall!(ctx, libc::SYS_writev);

    if allow_remote_logging {
        allow_syscall!(ctx, libc::SYS_sendto); // Required by syslog
    }

    let ret = unsafe { seccomp_load(ctx) };
    if ret != 0 {
        return Err(Error::LoadSeccompFilter);
    }

    unsafe { seccomp_release(ctx) };

    Ok(())
}
