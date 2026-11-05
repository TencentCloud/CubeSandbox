// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::ffi::CString;
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use tokio::time::Instant;

use crate::error::DomainError;

#[derive(Clone, Copy)]
pub(crate) enum ProcessClass {
    Pty,
    Socat,
    User,
}

#[derive(Debug, thiserror::Error)]
#[error("{0}")]
pub struct CgroupError(String);

pub(crate) struct ProcessCgroup {
    managed: Option<(std::fs::File, CgroupDirectory)>,
}

impl ProcessCgroup {
    pub(crate) fn file(&self) -> Option<&std::fs::File> {
        self.managed.as_ref().map(|(file, _directory)| file)
    }
}

struct CgroupDirectory {
    path: PathBuf,
    created: bool,
}

impl Drop for CgroupDirectory {
    fn drop(&mut self) {
        if self.created {
            // Never remove a platform-owned class or signal descendants. A
            // class still containing processes remains for the guest to reap.
            let _ = std::fs::remove_dir(&self.path);
        }
    }
}

struct CgroupSet {
    ptys: Arc<ProcessCgroup>,
    socats: Arc<ProcessCgroup>,
    user: Arc<ProcessCgroup>,
}

impl CgroupSet {
    fn noop() -> Self {
        let group = Arc::new(ProcessCgroup { managed: None });
        Self {
            ptys: group.clone(),
            socats: group.clone(),
            user: group,
        }
    }

    fn get(&self, class: ProcessClass) -> Arc<ProcessCgroup> {
        match class {
            ProcessClass::Pty => self.ptys.clone(),
            ProcessClass::Socat => self.socats.clone(),
            ProcessClass::User => self.user.clone(),
        }
    }
}

pub(crate) struct CgroupManager {
    root: PathBuf,
    groups: Arc<tokio::sync::Mutex<Option<Arc<CgroupSet>>>>,
}

impl CgroupManager {
    pub(crate) fn new(root: impl Into<PathBuf>) -> Self {
        let root = root.into();
        Self {
            root: if root.as_os_str().is_empty() {
                PathBuf::from("/sys/fs/cgroup")
            } else {
                root
            },
            groups: Arc::new(tokio::sync::Mutex::new(None)),
        }
    }

    pub(crate) async fn initialize(&self) -> Result<(), CgroupError> {
        let mut lease = self.acquire_inner(ProcessClass::User, None).await?;
        lease.commit();
        Ok(())
    }

    pub(crate) async fn acquire(
        &self,
        class: ProcessClass,
        deadline: Option<Instant>,
    ) -> Result<CgroupLease, DomainError> {
        self.acquire_inner(class, deadline)
            .await
            .map_err(|_| DomainError::Internal)
    }

    pub(crate) async fn group(
        &self,
        class: ProcessClass,
    ) -> Result<Arc<ProcessCgroup>, CgroupError> {
        let mut lease = self.acquire_inner(class, None).await?;
        let group = lease.group.clone();
        lease.commit();
        Ok(group)
    }

    async fn acquire_inner(
        &self,
        class: ProcessClass,
        deadline: Option<Instant>,
    ) -> Result<CgroupLease, CgroupError> {
        let mut slot = if let Some(deadline) = deadline {
            tokio::time::timeout_at(deadline, self.groups.clone().lock_owned())
                .await
                .map_err(|_| CgroupError("cgroup setup deadline exceeded".into()))?
        } else {
            self.groups.clone().lock_owned().await
        };
        if let Some(groups) = slot.as_ref() {
            return Ok(CgroupLease {
                group: groups.get(class),
                initializing: None,
            });
        }

        let root = self.root.clone();
        let mut job = tokio::task::spawn_blocking(move || prepare(&root));
        let groups = if let Some(deadline) = deadline {
            match tokio::time::timeout_at(deadline, &mut job).await {
                Ok(result) => result.map_err(|_| CgroupError("cgroup setup task failed".into()))?,
                Err(_) => {
                    drop(job.await);
                    return Err(CgroupError("cgroup setup deadline exceeded".into()));
                }
            }
        } else {
            job.await
                .map_err(|_| CgroupError("cgroup setup task failed".into()))?
        };
        let groups = match groups {
            Ok(groups) => groups,
            Err(error) => {
                // Match upstream: initialization failure disables only envd's
                // additional process groups for this daemon lifetime. A later
                // placement failure in an enabled group still rejects the child.
                tracing::warn!(cgroup_root = %self.root.display(), %error,
                    "falling back to no-op cgroup manager");
                CgroupSet::noop()
            }
        };
        let groups = Arc::new(groups);
        *slot = Some(groups.clone());
        Ok(CgroupLease {
            group: groups.get(class),
            initializing: Some(slot),
        })
    }
}

impl Default for CgroupManager {
    fn default() -> Self {
        Self::new("/sys/fs/cgroup")
    }
}

pub(crate) struct CgroupLease {
    pub(crate) group: Arc<ProcessCgroup>,
    initializing: Option<tokio::sync::OwnedMutexGuard<Option<Arc<CgroupSet>>>>,
}

impl CgroupLease {
    pub(crate) fn commit(&mut self) {
        self.initializing.take();
    }
}

impl Drop for CgroupLease {
    fn drop(&mut self) {
        if let Some(slot) = self.initializing.as_mut() {
            slot.take();
        }
    }
}

fn prepare(root: &Path) -> Result<CgroupSet, CgroupError> {
    verify_cgroup2(root)?;
    let memory = std::fs::read_to_string("/proc/meminfo")
        .map_err(|error| io_error("read memory metrics", Path::new("/proc/meminfo"), error))?;
    let total = memory
        .lines()
        .find_map(|line| line.strip_prefix("MemTotal:"))
        .and_then(|line| line.split_whitespace().next())
        .and_then(|value| value.parse::<u64>().ok())
        .and_then(|value| value.checked_mul(1024))
        .ok_or_else(|| CgroupError("failed to calculate host memory".into()))?;
    let available = total
        .checked_sub((total / 8).min(128 * 1024 * 1024))
        .ok_or_else(|| CgroupError("failed to calculate cgroup memory limit".into()))?;

    let ptys = create(
        root,
        "ptys",
        &[
            ("cpu.weight", "200".into()),
            ("memory.high", available.to_string()),
            ("memory.max", available.to_string()),
        ],
    )?;
    let socats = create(
        root,
        "socats",
        &[
            ("cpu.weight", "150".into()),
            ("memory.min", (5 * 1024 * 1024).to_string()),
            ("memory.low", (8 * 1024 * 1024).to_string()),
        ],
    )?;
    let user = create(
        root,
        "user",
        &[
            ("cpu.weight", "50".into()),
            ("memory.high", available.to_string()),
            ("memory.max", available.to_string()),
        ],
    )?;
    Ok(CgroupSet {
        ptys: Arc::new(ptys),
        socats: Arc::new(socats),
        user: Arc::new(user),
    })
}

fn verify_cgroup2(root: &Path) -> Result<(), CgroupError> {
    let path = CString::new(root.as_os_str().as_bytes())
        .map_err(|_| CgroupError("cgroup root contains NUL".into()))?;
    let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
    let result = unsafe { libc::statfs(path.as_ptr(), stat.as_mut_ptr()) };
    if result != 0 {
        return Err(io_error(
            "inspect cgroup root",
            root,
            std::io::Error::last_os_error(),
        ));
    }
    let stat = unsafe { stat.assume_init() };
    if stat.f_type as u64 != libc::CGROUP2_SUPER_MAGIC as u64 {
        return Err(CgroupError(format!(
            "cgroup root {} is not a cgroup v2 filesystem",
            root.display()
        )));
    }
    Ok(())
}

fn create(
    root: &Path,
    name: &str,
    properties: &[(&str, String)],
) -> Result<ProcessCgroup, CgroupError> {
    let path = root.join(name);
    let created = match std::fs::create_dir(&path) {
        Ok(()) => true,
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => false,
        Err(error) => return Err(io_error("create cgroup", &path, error)),
    };
    let directory = CgroupDirectory {
        path: path.clone(),
        created,
    };
    for (property, value) in properties {
        let property = path.join(property);
        std::fs::write(&property, value)
            .map_err(|error| io_error("configure cgroup", &property, error))?;
    }
    let file = std::fs::OpenOptions::new()
        .write(true)
        .open(path.join("cgroup.procs"))
        .map_err(|error| io_error("open cgroup.procs", &path, error))?;
    Ok(ProcessCgroup {
        managed: Some((file, directory)),
    })
}

fn io_error(operation: &str, path: &Path, error: std::io::Error) -> CgroupError {
    CgroupError(format!("{operation} {}: {error}", path.display()))
}
