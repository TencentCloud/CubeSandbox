// Copyright 2020 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

use crate::{idmap, oslib, util};
use idmap::{GidMap, IdMapSetUpPipeMessage, UidMap};
use std::ffi::CString;
use std::fs::{self, File};
use std::io::{Read, Write};
use std::os::fd::OwnedFd;
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::path::Path;
use std::process::{self, Command};
use std::str::FromStr;
use std::{error, fmt, io};
use vhost::vhost_user::Listener;

#[derive(Debug)]
pub enum Error {
    /// Failed to bind mount `/proc/self/fd` into a temporary directory.
    BindMountProcSelfFd(io::Error),
    /// Failed to bind mount shared directory.
    BindMountSharedDir(io::Error),
    /// Failed to change to the old root directory.
    ChdirOldRoot(io::Error),
    /// Failed to change to the new root directory.
    ChdirNewRoot(io::Error),
    /// Call to libc::chroot returned an error.
    Chroot(io::Error),
    /// Failed to change to the root directory after the chroot call.
    ChrootChdir(io::Error),
    /// Failed to clean the properties of the mount point.
    CleanMount(io::Error),
    /// Failed to create a temporary directory.
    CreateTempDir(io::Error),
    /// Failed to drop supplemental groups.
    DropSupplementalGroups(io::Error),
    /// Call to libc::fork returned an error.
    Fork(io::Error),
    /// Failed to get the number of supplemental groups.
    GetSupplementalGroups(io::Error),
    /// Error bind-mounting a directory.
    MountBind(io::Error),
    /// Failed to mount old root.
    MountOldRoot(io::Error),
    /// Error mounting proc.
    MountProc(io::Error),
    /// Failed to mount new root.
    MountNewRoot(io::Error),
    /// Error mounting target directory.
    MountTarget(io::Error),
    /// Failed to open `/proc/self/mountinfo`.
    OpenMountinfo(io::Error),
    /// Failed to open new root.
    OpenNewRoot(io::Error),
    /// Failed to open old root.
    OpenOldRoot(io::Error),
    /// Failed to open `/proc/self`.
    OpenProcSelf(io::Error),
    /// Failed to open `/proc/self/fd`.
    OpenProcSelfFd(io::Error),
    /// Error switching root directory.
    PivotRoot(io::Error),
    /// Failed to remove temporary directory.
    RmdirTempDir(io::Error),
    /// Failed to lazily unmount old root.
    UmountOldRoot(io::Error),
    /// Failed to lazily unmount temporary directory.
    UmountTempDir(io::Error),
    /// Call to libc::unshare returned an error.
    Unshare(io::Error),
    /// Failed to execute `newgidmap(1)`.
    WriteGidMap(String),
    /// Failed to write to `/proc/self/setgroups`.
    WriteSetGroups(io::Error),
    /// Failed to execute `newuidmap(1)`.
    WriteUidMap(String),
    /// Sandbox mode unavailable for non-privileged users
    SandboxModeInvalidUID,
    /// Setting uid_map is only allowed inside a namespace for non-privileged users
    SandboxModeInvalidUidMap,
    /// Setting gid_map is only allowed inside a namespace for non-privileged users
    SandboxModeInvalidGidMap,
}

impl error::Error for Error {}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        use self::Error::{
            SandboxModeInvalidGidMap, SandboxModeInvalidUID, SandboxModeInvalidUidMap, WriteGidMap,
            WriteUidMap,
        };
        match self {
            SandboxModeInvalidUID => {
                write!(
                    f,
                    "sandbox mode 'chroot' can only be used by \
                    root (Use '--sandbox namespace' instead)"
                )
            }
            SandboxModeInvalidUidMap => {
                write!(
                    f,
                    "uid_map can only be used by unprivileged user where sandbox mod is namespace \
                    (Use '--sandbox namespace' instead)"
                )
            }
            SandboxModeInvalidGidMap => {
                write!(
                    f,
                    "gid_map can only be used by unprivileged user where sandbox mod is namespace \
                    (Use '--sandbox namespace' instead)"
                )
            }
            WriteUidMap(msg) => write!(f, "write to uid map failed: {msg}"),
            WriteGidMap(msg) => write!(f, "write to gid map failed: {msg}"),
            _ => write!(f, "{self:?}"),
        }
    }
}

/// Mechanism to be used for setting up the sandbox.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum SandboxMode {
    /// Create the sandbox using Linux namespaces.
    Namespace,
    /// Create the sandbox using chroot.
    Chroot,
    /// Don't attempt to isolate the process inside a sandbox.
    None,
}

impl FromStr for SandboxMode {
    type Err = &'static str;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.to_lowercase().as_str() {
            "namespace" => Ok(SandboxMode::Namespace),
            "chroot" => Ok(SandboxMode::Chroot),
            "none" => Ok(SandboxMode::None),
            _ => Err("Unknown sandbox mode"),
        }
    }
}

/// A helper for creating a sandbox for isolating the service.
pub struct Sandbox {
    /// The directory that is going to be shared with the VM. The sandbox will be constructed on top
    /// of this directory.
    shared_dir: String,
    /// A `File` object for `/proc/self/fd` obtained from the sandboxed context.
    proc_self_fd: Option<File>,
    /// A `File` object for `/proc/self/mountinfo` obtained from the sandboxed context.
    mountinfo_fd: Option<File>,
    /// Mechanism to be used for setting up the sandbox.
    sandbox_mode: SandboxMode,
    /// UidMap to be used for `newuidmap(1)` command line arguments
    uid_map: Vec<UidMap>,
    /// GidMap to be used for `newgidmap(1)` command line arguments
    gid_map: Vec<GidMap>,
}

impl Sandbox {
    pub fn new(
        shared_dir: String,
        sandbox_mode: SandboxMode,
        uid_map: Vec<UidMap>,
        gid_map: Vec<GidMap>,
    ) -> io::Result<Self> {
        let shared_dir_rp = fs::canonicalize(shared_dir)?;
        let shared_dir_rp_str = shared_dir_rp
            .to_str()
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;

        Ok(Sandbox {
            shared_dir: shared_dir_rp_str.into(),
            proc_self_fd: None,
            mountinfo_fd: None,
            sandbox_mode,
            uid_map,
            gid_map,
        })
    }

    // Make `self.shared_dir` our root directory, and get isolated file descriptors for
    // `/proc/self/fd` and '/proc/self/mountinfo`.
    //
    // This is based on virtiofsd's setup_namespaces() and setup_mounts(), and it's very similar to
    // the strategy used in containers. Consists on a careful sequence of mounts and bind-mounts to
    // ensure it's not possible to escape the sandbox through `self.shared_dir` nor the file
    // descriptor obtained for `/proc/self/fd`.
    //
    // It's ugly, but it's the only way until Linux implements a proper containerization API.
    fn setup_mounts(&mut self) -> Result<(), Error> {
        // Open an FD to `/proc/self` so we can later open `/proc/self/mountinfo`.
        // (If we opened `/proc/self/mountinfo` now, it would appear empty by the end of this
        // function, which is why we need to defer opening it until then.)
        let c_proc_self = CString::new("/proc/self").unwrap();
        let proc_self_raw = unsafe { libc::open(c_proc_self.as_ptr(), libc::O_PATH) };
        if proc_self_raw < 0 {
            return Err(Error::OpenProcSelf(std::io::Error::last_os_error()));
        }

        // Encapsulate the `/proc/self` FD in a `File` object so it is closed when this function
        // returns
        let proc_self = unsafe { File::from_raw_fd(proc_self_raw) };

        // Ensure our mount changes don't affect the parent mount namespace.

        oslib::mount(None, "/", None, libc::MS_SLAVE | libc::MS_REC).map_err(Error::CleanMount)?;

        // Mount `/proc` in this context.
        oslib::mount(
            "proc".into(),
            "/proc",
            "proc".into(),
            libc::MS_NODEV | libc::MS_NOEXEC | libc::MS_NOSUID | libc::MS_RELATIME,
        )
        .map_err(Error::MountProc)?;

        // Bind-mount `/proc/self/fd` onto /proc preventing access to ancestor
        // directories.
        oslib::mount("/proc/self/fd".into(), "/proc", None, libc::MS_BIND)
            .map_err(Error::BindMountProcSelfFd)?;

        // Obtain a file descriptor to /proc/self/fd/ by opening bind-mounted /proc directory.
        let c_proc_dir = CString::new("/proc").unwrap();
        let proc_self_fd = unsafe { libc::open(c_proc_dir.as_ptr(), libc::O_PATH) };
        if proc_self_fd < 0 {
            return Err(Error::OpenProcSelfFd(std::io::Error::last_os_error()));
        }
        // Safe because we just opened this fd.
        self.proc_self_fd = Some(unsafe { File::from_raw_fd(proc_self_fd) });

        // Bind-mount `self.shared_dir` on itself so we can use as new root on `pivot_root` syscall.
        oslib::mount(
            self.shared_dir.as_str().into(),
            self.shared_dir.as_str(),
            None,
            libc::MS_BIND | libc::MS_REC,
        )
        .map_err(Error::BindMountSharedDir)?;

        // Get a file descriptor to our old root so we can reference it after switching root.
        let c_root_dir = CString::new("/").unwrap();
        let oldroot_fd = unsafe {
            libc::open(
                c_root_dir.as_ptr(),
                libc::O_DIRECTORY | libc::O_RDONLY | libc::O_CLOEXEC,
            )
        };
        if oldroot_fd < 0 {
            return Err(Error::OpenOldRoot(std::io::Error::last_os_error()));
        }

        // Get a file descriptor to the new root so we can reference it after switching root.
        let c_shared_dir = CString::new(self.shared_dir.clone()).unwrap();
        let newroot_fd = unsafe {
            libc::open(
                c_shared_dir.as_ptr(),
                libc::O_DIRECTORY | libc::O_RDONLY | libc::O_CLOEXEC,
            )
        };
        if newroot_fd < 0 {
            return Err(Error::OpenNewRoot(std::io::Error::last_os_error()));
        }

        // Change to new root directory to prepare for `pivot_root` syscall.
        oslib::fchdir(newroot_fd).map_err(Error::ChdirNewRoot)?;

        // Call to `pivot_root` using `.` as both new and old root.
        let c_current_dir = CString::new(".").unwrap();
        let ret = unsafe {
            libc::syscall(
                libc::SYS_pivot_root,
                c_current_dir.as_ptr(),
                c_current_dir.as_ptr(),
            )
        };
        if ret < 0 {
            return Err(Error::PivotRoot(std::io::Error::last_os_error()));
        }

        // Change to old root directory to prepare for cleaning up and unmounting it.
        oslib::fchdir(oldroot_fd).map_err(Error::ChdirOldRoot)?;

        // Clean up old root to avoid mount namespace propagation.
        oslib::mount(None, ".", None, libc::MS_SLAVE | libc::MS_REC).map_err(Error::CleanMount)?;

        // Lazily unmount old root.
        oslib::umount2(".", libc::MNT_DETACH).map_err(Error::UmountOldRoot)?;

        // Change to new root.
        oslib::fchdir(newroot_fd).map_err(Error::ChdirNewRoot)?;

        // We no longer need these file descriptors, so close them.
        unsafe { libc::close(newroot_fd) };
        unsafe { libc::close(oldroot_fd) };

        // Open `/proc/self/mountinfo` now
        let c_mountinfo = CString::new("mountinfo").unwrap();
        let mountinfo_fd =
            unsafe { libc::openat(proc_self.as_raw_fd(), c_mountinfo.as_ptr(), libc::O_RDONLY) };
        if mountinfo_fd < 0 {
            return Err(Error::OpenMountinfo(std::io::Error::last_os_error()));
        }
        // Safe because we just opened this fd.
        self.mountinfo_fd = Some(unsafe { File::from_raw_fd(mountinfo_fd) });

        Ok(())
    }

    /// Sets mappings for the given uid and gid.
    fn setup_id_mappings(
        &self,
        uid_map: &[UidMap],
        gid_map: &[GidMap],
        pid: i32,
    ) -> Result<(), Error> {
        let current_uid = unsafe { libc::geteuid() };
        let current_gid = unsafe { libc::getegid() };

        // Take uid map or set up a 1-to-1 mapping for our current euid.
        let default_uid_map = vec![UidMap {
            outside_uid: current_uid,
            inside_uid: current_uid,
            count: 1,
        }];
        let uid_map = if uid_map.is_empty() {
            &default_uid_map
        } else {
            uid_map
        };

        // Take gid map or set up a 1-to-1 mapping for our current gid.
        let default_gid_map = vec![GidMap {
            outside_gid: current_gid,
            inside_gid: current_gid,
            count: 1,
        }];
        let gid_map = if gid_map.is_empty() {
            &default_gid_map
        } else {
            gid_map
        };

        // Unprivileged user can not set any mapping without any restriction.
        // Therefore, newuidmap/newgidmap is used instead of writing directly
        // into proc/[pid]/{uid,gid}_map if a potentially privileged action is
        // requested (outside {u,g}id != e{u,g}id or count > 1).
        if uid_map.len() != 1 || uid_map[0].outside_uid != current_uid || uid_map[0].count > 1 {
            let mut newuidmap = Command::new("newuidmap");
            newuidmap.arg(pid.to_string());
            for entry in uid_map.iter() {
                newuidmap.arg(entry.inside_uid.to_string());
                newuidmap.arg(entry.outside_uid.to_string());
                newuidmap.arg(entry.count.to_string());
            }
            let output = newuidmap.output().map_err(|_| {
                Error::WriteUidMap(format!(
                    "failed to execute newuidmap: {}",
                    io::Error::last_os_error()
                ))
            })?;
            if !output.status.success() {
                return Err(Error::WriteUidMap(
                    String::from_utf8_lossy(&output.stderr).to_string(),
                ));
            }
        } else {
            // Unprivileged part, we can driectly write to /proc/[pid]/uid_map.
            std::fs::write(
                format!("/proc/{pid}/uid_map"),
                format!("{} {} 1", uid_map[0].inside_uid, uid_map[0].outside_uid),
            )
            .map_err(|e| Error::WriteUidMap(e.to_string()))?;
        }

        if gid_map.len() != 1 || gid_map[0].outside_gid != current_gid || gid_map[0].count > 1 {
            let mut newgidmap = Command::new("newgidmap");
            newgidmap.arg(pid.to_string());
            for entry in gid_map.iter() {
                newgidmap.arg(entry.inside_gid.to_string());
                newgidmap.arg(entry.outside_gid.to_string());
                newgidmap.arg(entry.count.to_string());
            }
            let output = newgidmap.output().map_err(|_| {
                Error::WriteGidMap(format!(
                    "failed to execute newgidmap: {}",
                    io::Error::last_os_error()
                ))
            })?;
            if !output.status.success() {
                return Err(Error::WriteGidMap(
                    String::from_utf8_lossy(&output.stderr).to_string(),
                ));
            }
        } else {
            // Unprivileged part, we can driectly write to /proc/[pid]/gid_map.
            std::fs::write(format!("/proc/{pid}/setgroups"), b"deny")
                .map_err(|e| Error::WriteGidMap(e.to_string()))?;
            std::fs::write(
                format!("/proc/{pid}/gid_map"),
                format!("{} {} 1", gid_map[0].inside_gid, gid_map[0].outside_gid),
            )
            .map_err(|e| Error::WriteGidMap(e.to_string()))?;
        }
        Ok(())
    }

    pub fn enter_namespace(&mut self, listener: Listener) -> Result<Listener, Error> {
        let uid = unsafe { libc::geteuid() };

        let flags = if uid == 0 {
            libc::CLONE_NEWPID | libc::CLONE_NEWNS | libc::CLONE_NEWNET
        } else {
            // If running as an unprivileged user, rely on user_namespaces(7) for isolation.
            libc::CLONE_NEWPID | libc::CLONE_NEWNS | libc::CLONE_NEWNET | libc::CLONE_NEWUSER
        };

        let (mut x_reader, mut x_writer) = oslib::pipe().unwrap();
        let (mut y_reader, mut y_writer) = oslib::pipe().unwrap();

        let pid = util::sfork().map_err(Error::Fork)?;
        let mut output = [0];

        // First child is only responsible to setup id mapping
        // from outside of the main thread's namespace.
        // Pipe is used for synchronization between the main thread and the first child.
        // That will guarantee the mapping is done before the main thread gets running.
        if pid == 0 {
            // First child
            // Dropping the other end of the pipes
            drop(x_writer);
            drop(y_reader);

            // This is waiting until unshare() returns
            x_reader.read_exact(&mut output).unwrap();
            assert_eq!(output[0], IdMapSetUpPipeMessage::Request as u8);

            // Setup uid/gid mappings
            if uid != 0 {
                let ppid = unsafe { libc::getppid() };
                if let Err(error) = self.setup_id_mappings(&self.uid_map, &self.gid_map, ppid) {
                    // We don't really need to close the pipes here, since the OS will close the FDs
                    // after the process exits. But let's do it explicitly to signal an error to the
                    // other end of the pipe.
                    drop(x_reader);
                    drop(y_writer);
                    error!("sandbox: couldn't setup id mappings: {}", error);
                    process::exit(1);
                };
            }

            // Signal that mapping is done
            y_writer
                .write_all(&[IdMapSetUpPipeMessage::Done as u8])
                .unwrap_or_else(|_| process::exit(1));

            // Terminate this child
            process::exit(0);
        } else {
            // This is the parent
            let ret = unsafe { libc::unshare(flags) };
            if ret != 0 {
                return Err(Error::Unshare(std::io::Error::last_os_error()));
            }

            // Dropping the other end of the pipes
            drop(x_reader);
            drop(y_writer);

            // Signal the first child to go ahead and setup the id mappings
            x_writer
                .write_all(&[IdMapSetUpPipeMessage::Request as u8])
                .unwrap();

            // Receive the signal that mapping is done. If the child process exits
            // before setting up the mapping, closing the pipe before sending the
            // message, `read_exact()` will fail with `UnexpectedEof`.
            y_reader
                .read_exact(&mut output)
                .unwrap_or_else(|_| process::exit(1));
            assert_eq!(output[0], IdMapSetUpPipeMessage::Done as u8);

            let mut status = 0_i32;
            let _ = unsafe { libc::waitpid(pid, &mut status, 0) };

            // Set the process inside the user namespace as root
            let mut ret = unsafe { libc::setresuid(0, 0, 0) };
            if ret != 0 {
                warn!("Couldn't set the process uid as root: {}", ret);
            }
            ret = unsafe { libc::setresgid(0, 0, 0) };
            if ret != 0 {
                warn!("Couldn't set the process gid as root: {}", ret);
            }

            let child = util::sfork().map_err(Error::Fork)?;
            if child == 0 {
                // Second child
                self.setup_mounts()?;
                Ok(listener)
            } else {
                // This is the parent

                // The child process drops the `vhost::Listener` after the first
                // `accept()`. However, since the parent just waits until the child
                // ends, keeping all the FDs open, as well as the socket's FD in a
                // listen state. This is problematic because nothing prevents a
                // miss-configured VMM to try to connect twice to the same socket
                // leaving the VMM waiting forever. So, let's close the listener
                // before waiting for the child.
                let fd = listener.as_raw_fd();

                // `vhost::Listener` beside closing the FD, it will remove the socket, if dropped
                std::mem::forget(listener);

                // Let's close the FD without removing the socket file
                // SAFETY: `fd` is open and nobody owns it
                let fd = unsafe { OwnedFd::from_raw_fd(fd) };
                drop(fd);

                util::wait_for_child(child); // This never returns.
            }
        }
    }

    pub fn enter_chroot(&mut self) -> Result<(), Error> {
        let c_proc_self_fd = CString::new("/proc/self/fd").unwrap();
        let proc_self_fd = unsafe { libc::open(c_proc_self_fd.as_ptr(), libc::O_PATH) };
        if proc_self_fd < 0 {
            return Err(Error::OpenProcSelfFd(std::io::Error::last_os_error()));
        }
        // Safe because we just opened this fd.
        self.proc_self_fd = Some(unsafe { File::from_raw_fd(proc_self_fd) });

        let c_mountinfo = CString::new("/proc/self/mountinfo").unwrap();
        let mountinfo_fd = unsafe { libc::open(c_mountinfo.as_ptr(), libc::O_RDONLY) };
        if mountinfo_fd < 0 {
            return Err(Error::OpenMountinfo(std::io::Error::last_os_error()));
        }
        // Safe because we just opened this fd.
        self.mountinfo_fd = Some(unsafe { File::from_raw_fd(mountinfo_fd) });

        let c_shared_dir = CString::new(self.shared_dir.clone()).unwrap();
        let ret = unsafe { libc::chroot(c_shared_dir.as_ptr()) };
        if ret != 0 {
            return Err(Error::Chroot(std::io::Error::last_os_error()));
        }

        let c_root_dir = CString::new("/").unwrap();
        let ret = unsafe { libc::chdir(c_root_dir.as_ptr()) };
        if ret != 0 {
            return Err(Error::ChrootChdir(std::io::Error::last_os_error()));
        }

        Ok(())
    }

    fn must_drop_supplemental_groups(&self) -> Result<bool, Error> {
        let uid = unsafe { libc::geteuid() };
        if uid != 0 {
            return Ok(false);
        }

        // If we are running as root and the system does not support user namespaces,
        // we must drop supplemental groups.
        if !Path::new("/proc/self/ns/user").exists() {
            return Ok(true);
        }

        let uid_mmap_data =
            fs::read_to_string("/proc/self/uid_map").map_err(Error::DropSupplementalGroups)?;
        let uid_map: Vec<_> = uid_mmap_data.split_whitespace().collect();

        let gid_map_data =
            fs::read_to_string("/proc/self/gid_map").map_err(Error::DropSupplementalGroups)?;
        let gid_map: Vec<_> = gid_map_data.split_whitespace().collect();

        let setgroups =
            fs::read_to_string("/proc/self/setgroups").map_err(Error::DropSupplementalGroups)?;

        // A single line mapping only has 3 fields, and the 'count' field should
        // be 1.
        let single_uid_mapping = uid_map.len() == 3 && uid_map[2] == "1";
        let single_gid_mapping = gid_map.len() == 3 && gid_map[2] == "1";

        Ok(setgroups.trim() != "deny" || !single_uid_mapping || !single_gid_mapping)
    }

    fn drop_supplemental_groups(&self) -> Result<(), Error> {
        let ngroups = unsafe { libc::getgroups(0, std::ptr::null_mut()) };
        if ngroups < 0 {
            return Err(Error::GetSupplementalGroups(std::io::Error::last_os_error()));
        } else if ngroups != 0 {
            let ret = unsafe { libc::setgroups(0, std::ptr::null()) };
            if ret != 0 {
                return Err(Error::DropSupplementalGroups(
                    std::io::Error::last_os_error(),
                ));
            }
        }

        Ok(())
    }

    /// Set up sandbox,
    pub fn enter(&mut self, listener: Listener) -> Result<Listener, Error> {
        let uid = unsafe { libc::geteuid() };
        if uid != 0 && self.sandbox_mode == SandboxMode::Chroot {
            return Err(Error::SandboxModeInvalidUID);
        }

        if !self.uid_map.is_empty() && (uid == 0 || self.sandbox_mode != SandboxMode::Namespace) {
            return Err(Error::SandboxModeInvalidUidMap);
        }

        if !self.gid_map.is_empty() && (uid == 0 || self.sandbox_mode != SandboxMode::Namespace) {
            return Err(Error::SandboxModeInvalidGidMap);
        }

        // We must drop supplemental groups membership if we support switching
        // between arbitrary uids/gids, unless the following conditions are met:
        // we're not running as root or we are inside a user namespace with only
        // one uid and gid mapping and '/proc/self/setgroups' is equal to
        // "deny". In both of these cases, no arbitrary uid/gid switching is
        // possible and thus there's no need to drop supplemental groups. In
        // both of these scenarios calling setgroups() is also not allowed so we
        // avoid calling it since we know it will return a privilege error.
        let must_drop_supplemental_groups = match self.must_drop_supplemental_groups() {
            Ok(must_drop) => must_drop,
            Err(error) => {
                warn!(
                    "Failed to determine whether supplemental groups must be dropped: {error}; \
                    defaulting to trying to drop supplemental groups"
                );
                true
            }
        };

        if must_drop_supplemental_groups {
            self.drop_supplemental_groups()?;
        }

        match self.sandbox_mode {
            SandboxMode::Namespace => self.enter_namespace(listener),
            SandboxMode::Chroot => self.enter_chroot().and(Ok(listener)),
            SandboxMode::None => Ok(listener),
        }
    }

    pub fn get_proc_self_fd(&mut self) -> Option<File> {
        self.proc_self_fd.take()
    }

    pub fn get_mountinfo_fd(&mut self) -> Option<File> {
        self.mountinfo_fd.take()
    }

    pub fn get_root_dir(&self) -> String {
        match self.sandbox_mode {
            SandboxMode::Namespace | SandboxMode::Chroot => "/".to_string(),
            SandboxMode::None => self.shared_dir.clone(),
        }
    }

    /// Return the prefix to strip from /proc/self/mountinfo entries to get paths that are actually
    /// accessible in our sandbox
    pub fn get_mountinfo_prefix(&self) -> Option<String> {
        match self.sandbox_mode {
            SandboxMode::Namespace | SandboxMode::None => None,
            SandboxMode::Chroot => Some(self.shared_dir.clone()),
        }
    }
}
