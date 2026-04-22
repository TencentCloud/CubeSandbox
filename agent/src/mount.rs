// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::device::{
    get_device_name_by_pci, get_scsi_device_name, get_virtio_blk_pci_device_name, online_device,
    wait_for_pmem_device, DRIVER_BIND_SHARE_TYPE, DRIVER_BLK_CUBE_TYPE, DRIVER_BLK_TYPE,
    DRIVER_CUBE_BIND_TYPE, DRIVER_EPHEMERAL_TYPE, DRIVER_LOCAL_TYPE, DRIVER_MMIO_BLK_TYPE,
    DRIVER_NVDIMM_TYPE, DRIVER_OVERLAYFS_TYPE, DRIVER_SCSI_TYPE, DRIVER_VIRTIOFS_TYPE,
    DRIVER_WATCHABLE_BIND_TYPE, FS_TYPE_HUGETLB,
};
use crate::linux_abi::*;
use crate::pci;
use crate::protocols::agent::Storage;
use crate::protocols::types::FSGroupChangePolicy;
use crate::Sandbox;
use anyhow::{anyhow, Context, Result};
use nix::mount::{self, MsFlags};
use nix::sys::stat;
use nix::unistd::{Gid, Uid};
use regex::Regex;
use slog::Logger;
use std::collections::HashMap;
use std::fs;
use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader, Write};
use std::iter;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::Path;
use std::str::FromStr;
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::Mutex;
use tracing::instrument;

pub const TYPE_ROOTFS: &str = "rootfs";
const SYS_FS_HUGEPAGES_PREFIX: &str = "/sys/kernel/mm/hugepages";
pub const MOUNT_GUEST_TAG: &str = "cubeShared";

const CUBE_BIND_SHARE_DIR: &str = "/run/cube-bind-share/";

// Allocating an FSGroup that owns the pod's volumes
const FS_GID: &str = "fsgid";

const RW_MASK: u32 = 0o660;
const RO_MASK: u32 = 0o440;
const EXEC_MASK: u32 = 0o110;
const MODE_SETGID: u32 = 0o2000;

#[rustfmt::skip]
lazy_static! {
    pub static ref FLAGS: HashMap<&'static str, (bool, MsFlags)> = {
        let mut m = HashMap::new();
        m.insert("defaults",      (false, MsFlags::empty()));
        m.insert("ro",            (false, MsFlags::MS_RDONLY));
        m.insert("rw",            (true,  MsFlags::MS_RDONLY));
        m.insert("suid",          (true,  MsFlags::MS_NOSUID));
        m.insert("nosuid",        (false, MsFlags::MS_NOSUID));
        m.insert("dev",           (true,  MsFlags::MS_NODEV));
        m.insert("nodev",         (false, MsFlags::MS_NODEV));
        m.insert("exec",          (true,  MsFlags::MS_NOEXEC));
        m.insert("noexec",        (false, MsFlags::MS_NOEXEC));
        m.insert("sync",          (false, MsFlags::MS_SYNCHRONOUS));
        m.insert("async",         (true,  MsFlags::MS_SYNCHRONOUS));
        m.insert("dirsync",       (false, MsFlags::MS_DIRSYNC));
        m.insert("remount",       (false, MsFlags::MS_REMOUNT));
        m.insert("mand",          (false, MsFlags::MS_MANDLOCK));
        m.insert("nomand",        (true,  MsFlags::MS_MANDLOCK));
        m.insert("atime",         (true,  MsFlags::MS_NOATIME));
        m.insert("noatime",       (false, MsFlags::MS_NOATIME));
        m.insert("diratime",      (true,  MsFlags::MS_NODIRATIME));
        m.insert("nodiratime",    (false, MsFlags::MS_NODIRATIME));
        m.insert("bind",          (false, MsFlags::MS_BIND));
        m.insert("rbind",         (false, MsFlags::MS_BIND | MsFlags::MS_REC));
        m.insert("unbindable",    (false, MsFlags::MS_UNBINDABLE));
        m.insert("runbindable",   (false, MsFlags::MS_UNBINDABLE | MsFlags::MS_REC));
        m.insert("private",       (false, MsFlags::MS_PRIVATE));
        m.insert("rprivate",      (false, MsFlags::MS_PRIVATE | MsFlags::MS_REC));
        m.insert("shared",        (false, MsFlags::MS_SHARED));
        m.insert("rshared",       (false, MsFlags::MS_SHARED | MsFlags::MS_REC));
        m.insert("slave",         (false, MsFlags::MS_SLAVE));
        m.insert("rslave",        (false, MsFlags::MS_SLAVE | MsFlags::MS_REC));
        m.insert("relatime",      (false, MsFlags::MS_RELATIME));
        m.insert("norelatime",    (true,  MsFlags::MS_RELATIME));
        m.insert("strictatime",   (false, MsFlags::MS_STRICTATIME));
        m.insert("nostrictatime", (true,  MsFlags::MS_STRICTATIME));
        m
    };
}

#[derive(Debug, PartialEq)]
pub struct InitMount<'a> {
    fstype: &'a str,
    src: &'a str,
    dest: &'a str,
    options: Vec<&'a str>,
}

#[rustfmt::skip]
lazy_static!{
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

#[rustfmt::skip]
lazy_static! {
    pub static ref INIT_ROOTFS_MOUNTS: Vec<InitMount<'static>> = vec![
        InitMount{fstype: "proc", src: "proc", dest: "/proc", options: vec!["nosuid", "nodev", "noexec"]},
        InitMount{fstype: "sysfs", src: "sysfs", dest: "/sys", options: vec!["nosuid", "nodev", "noexec"]},
        //InitMount{fstype: "devtmpfs", src: "dev", dest: "/dev", options: vec!["nosuid"]},
        InitMount{fstype: "tmpfs", src: "tmpfs", dest: "/dev/shm", options: vec!["nosuid", "nodev"]},
        InitMount{fstype: "devpts", src: "devpts", dest: "/dev/pts", options: vec!["nosuid", "noexec"]},
        InitMount{fstype: "tmpfs", src: "tmpfs", dest: "/run", options: vec!["nosuid", "nodev"]},
    ];
}

pub const STORAGE_HANDLER_LIST: &[&str] = &[
    DRIVER_BLK_TYPE,
    DRIVER_VIRTIOFS_TYPE,
    DRIVER_EPHEMERAL_TYPE,
    DRIVER_OVERLAYFS_TYPE,
    DRIVER_MMIO_BLK_TYPE,
    DRIVER_LOCAL_TYPE,
    DRIVER_SCSI_TYPE,
    DRIVER_NVDIMM_TYPE,
    DRIVER_WATCHABLE_BIND_TYPE,
];

#[instrument]
pub fn baremount(
    source: &Path,
    destination: &Path,
    fs_type: &str,
    flags: MsFlags,
    options: &str,
    logger: &Logger,
) -> Result<()> {
    if source.as_os_str().is_empty() {
        return Err(anyhow!("need mount source"));
    }

    if destination.as_os_str().is_empty() {
        return Err(anyhow!("need mount destination"));
    }

    if fs_type.is_empty() {
        return Err(anyhow!("need mount FS type"));
    }

    debug!(
        logger,
        "bare mount {} to {} fs:{} flags:{:?}, opt:{}",
        source.display(),
        destination.display(),
        fs_type,
        flags,
        options
    );
    let start = Instant::now();
    let res = nix::mount::mount(
        Some(source),
        destination,
        Some(fs_type),
        flags,
        Some(options),
    )
    .map_err(|e| {
        anyhow!(
            "failed to mount {:?} to {:?}, with error: {}",
            source,
            destination,
            e
        )
    });

    let duration = start.elapsed().as_millis();
    if duration > 10 {
        info!(
            logger,
            "mount {:?} to {:?} cost: {}ms", source, destination, duration
        );
    }

    res
}

#[instrument]
async fn ephemeral_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    // hugetlbfs
    if storage.fstype == FS_TYPE_HUGETLB {
        return handle_hugetlbfs_storage(logger, storage).await;
    }

    // normal ephemeral storage
    fs::create_dir_all(Path::new(&storage.mount_point))?;

    // By now we only support one option field: "fsGroup" which
    // isn't an valid mount option, thus we should remove it when
    // do mount.
    if storage.options.len() > 0 {
        // ephemeral_storage didn't support mount options except fsGroup.
        let mut new_storage = storage.clone();
        new_storage.options = vec![];
        common_storage_handler(logger, &new_storage)?;

        let opts_vec: Vec<String> = storage.options.to_vec();

        let opts = parse_options(opts_vec);

        if let Some(fsgid) = opts.get(FS_GID) {
            let gid = fsgid.parse::<u32>()?;

            nix::unistd::chown(storage.mount_point.as_str(), None, Some(Gid::from_raw(gid)))?;

            let meta = fs::metadata(&storage.mount_point)?;
            let mut permission = meta.permissions();

            let o_mode = meta.mode() | MODE_SETGID;
            permission.set_mode(o_mode);
            fs::set_permissions(&storage.mount_point, permission)?;
        }
    } else {
        common_storage_handler(logger, storage)?;
    }

    Ok("".to_string())
}

#[instrument]
async fn cube_bind_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    // normal ephemeral storage
    fs::create_dir_all(Path::new(&storage.mount_point))?;
    common_storage_handler(logger, storage)?;
    Ok("".to_string())
}

#[instrument]
async fn overlayfs_storage_handler(
    logger: &Logger,
    storage: &Storage,
    _sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    common_storage_handler(logger, storage)
}

#[instrument]
async fn bind_share_storage_handler(
    _logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    if let Err(_) = stat::stat(CUBE_BIND_SHARE_DIR) {
        fs::create_dir_all(CUBE_BIND_SHARE_DIR).context(format!(
            "failed to create dir all {:?}",
            CUBE_BIND_SHARE_DIR
        ))?;
        let share_path = Path::new(CUBE_BIND_SHARE_DIR);
        mount::mount(
            Some(share_path),
            share_path,
            Option::<&str>::None,
            MsFlags::MS_BIND,
            Option::<&str>::None,
        )
        .map_err(|e| {
            anyhow!(format!(
                "mount {:?} failed, flags:MS_BIND err:{:}",
                share_path, e
            ))
        })?;
        mount::mount(
            Option::<&Path>::None,
            share_path,
            Option::<&str>::None,
            MsFlags::MS_SHARED,
            Option::<&str>::None,
        )
        .map_err(|e| {
            anyhow!(format!(
                "mount {:?} failed, flags:MS_SHARED err:{:}",
                share_path, e
            ))
        })?;
    }

    fs::create_dir_all(&storage.mount_point).context(format!(
        "failed to create dir all {:?}",
        &storage.mount_point
    ))?;

    let permissions = fs::Permissions::from_mode(0o777);
    fs::set_permissions(&storage.mount_point, permissions)?;

    Ok("".to_string())
}

#[instrument]
async fn local_storage_handler(
    _logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    fs::create_dir_all(&storage.mount_point).context(format!(
        "failed to create dir all {:?}",
        &storage.mount_point
    ))?;

    let opts_vec: Vec<String> = storage.options.to_vec();

    let opts = parse_options(opts_vec);

    let mut need_set_fsgid = false;
    if let Some(fsgid) = opts.get(FS_GID) {
        let gid = fsgid.parse::<u32>()?;

        nix::unistd::chown(storage.mount_point.as_str(), None, Some(Gid::from_raw(gid)))?;
        need_set_fsgid = true;
    }

    if let Some(mode) = opts.get("mode") {
        let mut permission = fs::metadata(&storage.mount_point)?.permissions();

        let mut o_mode = u32::from_str_radix(mode, 8)?;

        if need_set_fsgid {
            // set SetGid mode mask.
            o_mode |= MODE_SETGID;
        }
        permission.set_mode(o_mode);

        fs::set_permissions(&storage.mount_point, permission)?;
    }

    Ok("".to_string())
}

#[instrument]
async fn handle_hugetlbfs_storage(logger: &Logger, storage: &Storage) -> Result<String> {
    info!(logger, "handle hugetlbfs storage");
    // Allocate hugepages before mount
    // /sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages
    // /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages
    // options eg "pagesize=2097152,size=524288000"(2M, 500M)
    allocate_hugepages(logger, &storage.options.to_vec()).context("allocate hugepages")?;

    common_storage_handler(logger, storage)?;

    // hugetlbfs return empty string as ephemeral_storage_handler do.
    // this is a sandbox level storage, but not a container-level mount.
    Ok("".to_string())
}

// Allocate hugepages by writing to sysfs
fn allocate_hugepages(logger: &Logger, options: &[String]) -> Result<()> {
    info!(logger, "mounting hugePages storage options: {:?}", options);

    let (pagesize, size) = get_pagesize_and_size_from_option(options)
        .context(format!("parse mount options: {:?}", &options))?;

    info!(
        logger,
        "allocate hugepages. pageSize: {}, size: {}", pagesize, size
    );

    // sysfs entry is always of the form hugepages-${pagesize}kB
    // Ref: https://www.kernel.org/doc/Documentation/vm/hugetlbpage.txt
    let path = Path::new(SYS_FS_HUGEPAGES_PREFIX)
        .join(format!("hugepages-{}kB", pagesize / 1024))
        .join("nr_hugepages");

    // write numpages to nr_hugepages file.
    let numpages = format!("{}", size / pagesize);
    info!(logger, "write {} pages to {:?}", &numpages, &path);

    let mut file = OpenOptions::new()
        .write(true)
        .open(&path)
        .context(format!("open nr_hugepages directory {:?}", &path))?;

    file.write_all(numpages.as_bytes())
        .context(format!("write nr_hugepages failed: {:?}", &path))?;

    // Even if the write succeeds, the kernel isn't guaranteed to be
    // able to allocate all the pages we requested.  Verify that it
    // did.
    let verify = fs::read_to_string(&path).context(format!("reading {:?}", &path))?;
    let allocated = verify
        .trim_end()
        .parse::<u64>()
        .map_err(|_| anyhow!("Unexpected text {:?} in {:?}", &verify, &path))?;
    if allocated != size / pagesize {
        return Err(anyhow!(
            "Only allocated {} of {} hugepages of size {}",
            allocated,
            numpages,
            pagesize
        ));
    }

    Ok(())
}

// Parse filesystem options string to retrieve hugepage details
// options eg "pagesize=2048,size=107374182"
fn get_pagesize_and_size_from_option(options: &[String]) -> Result<(u64, u64)> {
    let mut pagesize_str: Option<&str> = None;
    let mut size_str: Option<&str> = None;

    for option in options {
        let vars: Vec<&str> = option.trim().split(',').collect();

        for var in vars {
            if let Some(stripped) = var.strip_prefix("pagesize=") {
                pagesize_str = Some(stripped);
            } else if let Some(stripped) = var.strip_prefix("size=") {
                size_str = Some(stripped);
            }

            if pagesize_str.is_some() && size_str.is_some() {
                break;
            }
        }
    }

    if pagesize_str.is_none() || size_str.is_none() {
        return Err(anyhow!("no pagesize/size options found"));
    }

    let pagesize = pagesize_str
        .unwrap()
        .parse::<u64>()
        .context(format!("parse pagesize: {:?}", &pagesize_str))?;
    let size = size_str
        .unwrap()
        .parse::<u64>()
        .context(format!("parse size: {:?}", &pagesize_str))?;

    Ok((pagesize, size))
}

// virtiommio_blk_storage_handler handles the storage for mmio blk driver.
#[instrument]
async fn virtiommio_blk_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    //The source path is VmPath
    common_storage_handler(logger, storage)
}

// virtiofs_storage_handler handles the storage for virtio-fs.
#[instrument]
async fn virtiofs_storage_handler(
    logger: &Logger,
    storage: &Storage,
    _sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    common_storage_handler(logger, storage)
}

// virtio_blk_storage_handler handles the storage for blk driver.
#[instrument]
async fn virtio_blk_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    let mut storage = storage.clone();
    // If hot-plugged, get the device node path based on the PCI path
    // otherwise use the virt path provided in Storage Source
    if storage.source.starts_with("/dev") {
        let metadata = fs::metadata(&storage.source)
            .context(format!("get metadata on file {:?}", &storage.source))?;

        let mode = metadata.permissions().mode();
        if mode & libc::S_IFBLK == 0 {
            return Err(anyhow!("Invalid device {}", &storage.source));
        }
    } else {
        let pcipath = pci::Path::from_str(&storage.source)?;
        let dev_path = get_virtio_blk_pci_device_name(&sandbox, &pcipath).await?;
        storage.source = dev_path;
    }

    common_storage_handler(logger, &storage)
}

async fn do_format_ext4_blk_cube(storage: &Storage) -> Result<()> {
    let lock = rustjail::container::WAIT_PID_LOCKER.lock().await;
    // 调用mkfs.ext4命令格式化blk设备
    let output = std::process::Command::new("/run/support/sbin/mkfs.ext4")
        .arg("-E")
        .arg("lazy_itable_init=1,lazy_journal_init=1")
        .arg(&storage.source)
        .output()
        .map_err(|e| anyhow!("Failed to execute mkfs.ext4: {}", e))?;
    drop(lock);
    if !output.status.success() {
        return Err(anyhow!(
            "mkfs.ext4 failed with exit code: {}",
            output.status
        ));
    }

    Ok(())
}

async fn format_blk_cube(logger: &Logger, storage: &mut Storage) -> Result<()> {
    info!(logger, "Formatting blk cube '{}'", storage.source);
    let start = tokio::time::Instant::now();
    match storage.fstype.to_ascii_lowercase().as_str() {
        "ext4" => do_format_ext4_blk_cube(storage).await,
        _ => Err(anyhow!("Unsupported fstype: {}", storage.fstype)),
    }
    .map_err(|e| anyhow!("Failed to format blk cube '{}': {}", storage.source, e))?;

    let duration = start.elapsed().as_millis();
    info!(
        logger,
        "Formatted blk cube '{}' in {}ms", storage.source, duration
    );

    Ok(())
}

async fn do_check_ext4_blk_cube(storage: &Storage) -> Result<()> {
    let lock = rustjail::container::WAIT_PID_LOCKER.lock().await;
    // Check filesystem integrity with e2fsck
    let output = std::process::Command::new("/run/support/sbin/e2fsck")
        .arg("-f") // Force check even if clean
        .arg("-y") // Automatically repair
        .arg(&storage.source)
        .output()
        .map_err(|e| anyhow!("Failed to execute e2fsck: {}", e))?;
    drop(lock);
    if !output.status.success() {
        return Err(anyhow!("e2fsck failed with exit code: {}", output.status));
    }

    Ok(())
}

async fn do_resize_ext4_blk_cube(storage: &Storage) -> Result<()> {
    let lock = rustjail::container::WAIT_PID_LOCKER.lock().await;
    // Resize filesystem to use all available space
    let output = std::process::Command::new("/run/support/sbin/resize2fs")
        .arg("-f") // Force resize
        .arg(&storage.source)
        .output()
        .map_err(|e| anyhow!("Failed to execute resize2fs: {}", e))?;
    drop(lock);
    if !output.status.success() {
        return Err(anyhow!(
            "resize2fs failed with exit code: {}",
            output.status
        ));
    }

    Ok(())
}

async fn resize_blk_cube(logger: &Logger, storage: &mut Storage) -> Result<()> {
    info!(logger, "Resizing blk cube '{}'", storage.source);
    let start = tokio::time::Instant::now();

    // First check filesystem
    do_check_ext4_blk_cube(storage)
        .await
        .map_err(|e| anyhow!("Filesystem check failed: {}", e))?;
    let fsck_ms = start.elapsed().as_millis();
    // Then resize
    do_resize_ext4_blk_cube(storage)
        .await
        .map_err(|e| anyhow!("Filesystem resize failed: {}", e))?;
    let resize_ms = start.elapsed().as_millis() - fsck_ms;
    info!(
        logger,
        "Resized blk cube '{}' done in {:?}ms (fsck: {:?}ms, resize: {:?}ms)",
        storage.source,
        start.elapsed().as_millis(),
        fsck_ms,
        resize_ms
    );
    Ok(())
}

// virtio_blk_cube_storage_handler handles the storage for blk driver.
#[instrument]
async fn virtio_blk_cube_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    debug!(logger, "cube storage:{:?}", &storage);
    let mut storage = storage.clone();

    //get devname by bdf(eg:0000:00:02.0)
    if storage.source.as_str().starts_with("0000:") {
        let dev_path = get_device_name_by_pci(&sandbox, &storage.source)
            .await
            .map_err(|e| {
                anyhow!(
                    "Failed to get virtio blk cube pci device name for '{}': {}",
                    storage.source,
                    e
                )
            })?;
        info!(logger, "cube dev_path:{}", dev_path);
        storage.source = dev_path.clone();
    }

    // 检查是否需要agent来格式化cbs
    if storage.need_format {
        format_blk_cube(logger, &mut storage).await?
    }

    if storage.need_resize {
        resize_blk_cube(logger, &mut storage).await?;
    }

    let res = common_storage_handler(logger, &storage);
    if res.is_err() {
        return res;
    }
    res
}

#[instrument]
async fn virtio_scsi_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    let mut storage = storage.clone();

    // Retrieve the device path from SCSI address.
    let dev_path = get_scsi_device_name(&sandbox, &storage.source).await?;
    storage.source = dev_path;

    common_storage_handler(logger, &storage)
}

#[instrument]
fn common_storage_handler(logger: &Logger, storage: &Storage) -> Result<String> {
    // Mount the storage device.
    let mount_point = storage.mount_point.to_string();

    mount_storage(logger, storage)?;
    set_ownership(logger, storage)?;
    Ok(mount_point)
}

// nvdimm_storage_handler handles the storage for NVDIMM driver.
#[instrument]
async fn nvdimm_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
) -> Result<String> {
    let storage = storage.clone();
    if let Err(_) = fs::metadata(&storage.source) {
        warn!(
            logger,
            "Not found {}, watch uevent wait it", &storage.source
        );
        wait_for_pmem_device(&sandbox, &storage.source).await?;
    }
    common_storage_handler(logger, &storage)
}

async fn bind_watcher_storage_handler(
    logger: &Logger,
    storage: &Storage,
    sandbox: Arc<Mutex<Sandbox>>,
    cid: Option<String>,
) -> Result<()> {
    let mut locked = sandbox.lock().await;

    if let Some(cid) = cid {
        locked
            .bind_watcher
            .add_container(cid, iter::once(storage.clone()), logger)
            .await
    } else {
        Ok(())
    }
}

// mount_storage performs the mount described by the storage structure.
#[instrument]
fn mount_storage(logger: &Logger, storage: &Storage) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "mount"));

    // Check share before attempting to mount to see if the destination is already a mount point.
    // If so, skip doing the mount. This facilitates mounting the sharedfs automatically
    // in the guest before the agent service starts.
    if storage.source == MOUNT_GUEST_TAG && is_mounted(&storage.mount_point)? {
        warn!(
            logger,
            "{} already mounted on {}, ignoring...", MOUNT_GUEST_TAG, &storage.mount_point
        );
        return Ok(());
    }

    let mount_path = Path::new(&storage.mount_point);
    let src_path = Path::new(&storage.source);
    if storage.fstype == "bind" && !src_path.is_dir() {
        ensure_destination_file_exists(mount_path)
    } else {
        fs::create_dir_all(mount_path).map_err(anyhow::Error::from)
    }
    .context("Could not create mountpoint")?;

    let options_vec = storage.options.to_vec();
    let options_vec = options_vec.iter().map(String::as_str).collect();
    let (flags, options) = parse_mount_flags_and_options(options_vec);

    let source = Path::new(&storage.source);

    baremount(
        source,
        mount_path,
        storage.fstype.as_str(),
        flags,
        options.as_str(),
        &logger,
    )
}

#[instrument]
pub fn set_ownership(logger: &Logger, storage: &Storage) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "mount", "fn" => "set_ownership"));

    // If fsGroup is not set, skip performing ownership change
    if storage.fs_group.is_none() {
        return Ok(());
    }
    let fs_group = storage.fs_group();

    let mut read_only = false;
    let opts_vec: Vec<String> = storage.options.to_vec();
    if opts_vec.contains(&String::from("ro")) {
        read_only = true;
    }

    let mount_path = Path::new(&storage.mount_point);
    let metadata = mount_path.metadata().map_err(|err| {
        error!(logger, "failed to obtain metadata for mount path";
            "mount-path" => mount_path.to_str(),
            "error" => err.to_string(),
        );
        err
    })?;

    if fs_group.group_change_policy == FSGroupChangePolicy::OnRootMismatch.into()
        && metadata.gid() == fs_group.group_id
    {
        let mut mask = if read_only { RO_MASK } else { RW_MASK };
        mask |= EXEC_MASK;

        // With fsGroup change policy to OnRootMismatch, if the current
        // gid of the mount path root directory matches the desired gid
        // and the current permission of mount path root directory is correct,
        // then ownership change will be skipped.
        let current_mode = metadata.permissions().mode();
        if (mask & current_mode == mask) && (current_mode & MODE_SETGID != 0) {
            info!(logger, "skipping ownership change for volume";
                "mount-path" => mount_path.to_str(),
                "fs-group" => fs_group.group_id.to_string(),
            );
            return Ok(());
        }
    }

    info!(logger, "performing recursive ownership change";
        "mount-path" => mount_path.to_str(),
        "fs-group" => fs_group.group_id.to_string(),
    );
    recursive_ownership_change(
        mount_path,
        None,
        Some(Gid::from_raw(fs_group.group_id)),
        read_only,
    )
}

#[instrument]
pub fn recursive_ownership_change(
    path: &Path,
    uid: Option<Uid>,
    gid: Option<Gid>,
    read_only: bool,
) -> Result<()> {
    let mut mask = if read_only { RO_MASK } else { RW_MASK };
    if path.is_dir() {
        for entry in fs::read_dir(&path)? {
            recursive_ownership_change(entry?.path().as_path(), uid, gid, read_only)?;
        }
        mask |= EXEC_MASK;
        mask |= MODE_SETGID;
    }
    nix::unistd::chown(path, uid, gid)?;

    if gid.is_some() {
        let metadata = path.metadata()?;
        let mut permission = metadata.permissions();
        let target_mode = metadata.mode() | mask;
        permission.set_mode(target_mode);
        fs::set_permissions(path, permission)?;
    }

    Ok(())
}

/// Looks for `mount_point` entry in the /proc/mounts.
#[instrument]
pub fn is_mounted(mount_point: &str) -> Result<bool> {
    let mount_point = mount_point.trim_end_matches('/');
    let found = fs::metadata(mount_point).is_ok()
        // Looks through /proc/mounts and check if the mount exists
        && fs::read_to_string("/proc/mounts")?
            .lines()
            .any(|line| {
                // The 2nd column reveals the mount point.
                line.split_whitespace()
                    .nth(1)
                    .map(|target| mount_point.eq(target))
                    .unwrap_or(false)
            });

    Ok(found)
}

#[instrument]
fn parse_mount_flags_and_options(options_vec: Vec<&str>) -> (MsFlags, String) {
    let mut flags = MsFlags::empty();
    let mut options: String = "".to_string();

    for opt in options_vec {
        if !opt.is_empty() {
            match FLAGS.get(opt) {
                Some(x) => {
                    let (clear, f) = *x;
                    if clear {
                        flags &= !f;
                    } else {
                        flags |= f;
                    }
                }
                None => {
                    if !options.is_empty() {
                        options.push_str(format!(",{}", opt).as_str());
                    } else {
                        options.push_str(opt.to_string().as_str());
                    }
                }
            };
        }
    }
    (flags, options)
}

pub async fn add_virtiofs_storages(logger: Logger, storages: Vec<Storage>) -> Result<()> {
    for storage in storages {
        let handler_name = storage.driver.clone();
        debug!(logger, "handler_name:{}", handler_name);
        if handler_name != DRIVER_VIRTIOFS_TYPE {
            continue;
        }
        mount_storage(&logger, &storage)?;
    }
    Ok(())
}

// add_storages takes a list of storages passed by the caller, and perform the
// associated operations such as waiting for the device to show up, and mount
// it to a specific location, according to the type of handler chosen, and for
// each storage.
#[instrument]
pub async fn add_storages(
    logger: Logger,
    storages: Vec<Storage>,
    sandbox: Arc<Mutex<Sandbox>>,
    cid: Option<String>,
) -> Result<Vec<String>> {
    let mut mount_list = Vec::new();
    for storage in storages {
        let handler_name = storage.driver.clone();
        let logger = logger.new(o!(
            "subsystem" => "storage",
            "storage-type" => handler_name.to_owned()));

        {
            let mut sb = sandbox.lock().await;
            let new_storage = sb.set_sandbox_storage(&storage.mount_point);
            if !new_storage {
                info!(
                    logger,
                    "storage already exists, skip mount{}", &storage.mount_point
                );
                continue;
            }
        }

        let start = Instant::now();
        let res = match handler_name.as_str() {
            DRIVER_BLK_TYPE => virtio_blk_storage_handler(&logger, &storage, sandbox.clone()).await,
            DRIVER_BLK_CUBE_TYPE => {
                virtio_blk_cube_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_CUBE_BIND_TYPE => {
                cube_bind_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_BIND_SHARE_TYPE => {
                bind_share_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_VIRTIOFS_TYPE => {
                virtiofs_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_EPHEMERAL_TYPE => {
                ephemeral_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_OVERLAYFS_TYPE => {
                overlayfs_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_MMIO_BLK_TYPE => {
                virtiommio_blk_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_LOCAL_TYPE => local_storage_handler(&logger, &storage, sandbox.clone()).await,
            DRIVER_SCSI_TYPE => {
                virtio_scsi_storage_handler(&logger, &storage, sandbox.clone()).await
            }
            DRIVER_NVDIMM_TYPE => nvdimm_storage_handler(&logger, &storage, sandbox.clone()).await,
            DRIVER_WATCHABLE_BIND_TYPE => {
                bind_watcher_storage_handler(&logger, &storage, sandbox.clone(), cid.clone())
                    .await?;
                // Don't register watch mounts, they're handled separately by the watcher.
                Ok(String::new())
            }
            _ => {
                return Err(anyhow!(
                    "Failed to find the storage handler {}",
                    storage.driver.to_owned()
                ));
            }
        };
        let duration = start.elapsed().as_millis();
        if duration > 10 {
            info!(
                logger,
                "handle {} storage cost: {}ms",
                handler_name.as_str(),
                duration
            );
        }
        // Todo need to rollback the mounted storage if err met.
        let mount_point = res?;

        if !mount_point.is_empty() {
            mount_list.push(mount_point);
        }
    }

    Ok(mount_list)
}

#[instrument]
fn mount_to_rootfs(logger: &Logger, m: &InitMount) -> Result<()> {
    let options_vec: Vec<&str> = m.options.clone();

    let (flags, options) = parse_mount_flags_and_options(options_vec);

    fs::create_dir_all(Path::new(m.dest)).context("could not create directory")?;

    let source = Path::new(m.src);
    let dest = Path::new(m.dest);

    baremount(source, dest, m.fstype, flags, &options, logger).or_else(|e| {
        if m.src != "dev" {
            return Err(e);
        }

        Ok(())
    })?;

    Ok(())
}

#[instrument]
pub fn general_mount(logger: &Logger) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "mount"));

    for m in INIT_ROOTFS_MOUNTS.iter() {
        mount_to_rootfs(&logger, m)?;
    }

    Ok(())
}

#[inline]
pub fn get_mount_fs_type(mount_point: &str) -> Result<String> {
    get_mount_fs_type_from_file(PROC_MOUNTSTATS, mount_point)
}

// get_mount_fs_type_from_file returns the FS type corresponding to the passed mount point and
// any error encountered.
#[instrument]
pub fn get_mount_fs_type_from_file(mount_file: &str, mount_point: &str) -> Result<String> {
    if mount_point.is_empty() {
        return Err(anyhow!("Invalid mount point {}", mount_point));
    }

    let content = fs::read_to_string(mount_file)?;

    let re = Regex::new(format!("device .+ mounted on {} with fstype (.+)", mount_point).as_str())?;

    // Read the file line by line using the lines() iterator from std::io::BufRead.
    for (_index, line) in content.lines().enumerate() {
        let capes = match re.captures(line) {
            Some(c) => c,
            None => continue,
        };

        if capes.len() > 1 {
            return Ok(capes[1].to_string());
        }
    }

    Err(anyhow!(
        "failed to find FS type for mount point {}, mount file content: {:?}",
        mount_point,
        content
    ))
}

#[instrument]
pub fn get_cgroup_mounts(
    logger: &Logger,
    cg_path: &str,
    unified_cgroup_hierarchy: bool,
) -> Result<Vec<InitMount<'static>>> {
    // cgroup v2
    // https://github.com/kata-containers/agent/blob/8c9bbadcd448c9a67690fbe11a860aaacc69813c/agent.go#L1249
    if unified_cgroup_hierarchy {
        return Ok(vec![InitMount {
            fstype: "cgroup2",
            src: "cgroup2",
            dest: "/sys/fs/cgroup",
            options: vec!["nosuid", "nodev", "noexec", "relatime", "nsdelegate"],
        }]);
    }

    let file = File::open(&cg_path)?;
    let reader = BufReader::new(file);

    let mut has_device_cgroup = false;
    let mut cg_mounts: Vec<InitMount> = vec![InitMount {
        fstype: "tmpfs",
        src: "tmpfs",
        dest: SYSFS_CGROUPPATH,
        options: vec!["nosuid", "nodev", "noexec", "mode=755"],
    }];

    // #subsys_name    hierarchy       num_cgroups     enabled
    // fields[0]       fields[1]       fields[2]       fields[3]
    'outer: for (_, line) in reader.lines().enumerate() {
        let line = line?;

        let fields: Vec<&str> = line.split('\t').collect();

        // Ignore comment header
        if fields[0].starts_with('#') {
            continue;
        }

        // Ignore truncated lines
        if fields.len() < 4 {
            continue;
        }

        // Ignore disabled cgroups
        if fields[3] == "0" {
            continue;
        }

        // Ignore fields containing invalid numerics
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
                options: vec!["nosuid", "nodev", "noexec", "relatime", key],
            });
        }
    }

    if !has_device_cgroup {
        warn!(logger, "The system didn't support device cgroup, which is dangerous, thus agent initialized without cgroup support!\n");
        return Ok(Vec::new());
    }

    cg_mounts.push(InitMount {
        fstype: "tmpfs",
        src: "tmpfs",
        dest: SYSFS_CGROUPPATH,
        options: vec!["remount", "ro", "nosuid", "nodev", "noexec", "mode=755"],
    });

    Ok(cg_mounts)
}

#[instrument]
pub fn cgroups_mount(logger: &Logger, unified_cgroup_hierarchy: bool) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "mount"));

    let cgroups = get_cgroup_mounts(&logger, PROC_CGROUPS, unified_cgroup_hierarchy)?;

    for cg in cgroups.iter() {
        mount_to_rootfs(&logger, cg)?;
    }

    if !unified_cgroup_hierarchy {
        // Enable memory hierarchical account.
        // For more information see https://www.kernel.org/doc/Documentation/cgroup-v1/memory.txt
        online_device("/sys/fs/cgroup/memory/memory.use_hierarchy")?;
    }
    Ok(())
}

#[instrument]
pub fn remove_mounts(mounts: &[String]) -> Result<()> {
    for m in mounts.iter() {
        nix::mount::umount(m.as_str()).context(format!("failed to umount {:?}", m))?;
    }
    Ok(())
}

#[instrument]
fn ensure_destination_file_exists(path: &Path) -> Result<()> {
    if path.is_file() {
        return Ok(());
    } else if path.exists() {
        return Err(anyhow!("{:?} exists but is not a regular file", path));
    }

    let dir = path
        .parent()
        .ok_or_else(|| anyhow!("failed to find parent path for {:?}", path))?;

    fs::create_dir_all(dir).context(format!("create_dir_all {:?}", dir))?;

    fs::File::create(path).context(format!("create empty file {:?}", path))?;

    Ok(())
}

#[instrument]
fn parse_options(option_list: Vec<String>) -> HashMap<String, String> {
    let mut options = HashMap::new();
    for opt in option_list.iter() {
        let fields: Vec<&str> = opt.split('=').collect();
        if fields.len() != 2 {
            continue;
        }

        options.insert(fields[0].to_string(), fields[1].to_string());
    }

    options
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_utils::test_utils::TestUserType;
    use crate::{skip_if_not_root, skip_loop_by_user, skip_loop_if_not_root, skip_loop_if_root};
    use protocols::agent::FSGroup;
    use std::fs::File;
    use std::fs::OpenOptions;
    use std::io::Write;
    use std::path::PathBuf;
    use tempfile::tempdir;

    #[test]
    fn test_mount() {
        #[derive(Debug)]
        struct TestData<'a> {
            // User(s) who can run this test
            test_user: TestUserType,

            src: &'a str,
            dest: &'a str,
            fs_type: &'a str,
            flags: MsFlags,
            options: &'a str,

            // If set, assume an error will be generated,
            // else assume no error.
            //
            // If not set, assume root required to perform a
            // successful mount.
            error_contains: &'a str,
        }

        let dir = tempdir().expect("failed to create tmpdir");
        let drain = slog::Discard;
        let logger = slog::Logger::root(drain, o!());

        let tests = &[
            TestData {
                test_user: TestUserType::Any,
                src: "",
                dest: "",
                fs_type: "",
                flags: MsFlags::empty(),
                options: "",
                error_contains: "need mount source",
            },
            TestData {
                test_user: TestUserType::Any,
                src: "from",
                dest: "",
                fs_type: "",
                flags: MsFlags::empty(),
                options: "",
                error_contains: "need mount destination",
            },
            TestData {
                test_user: TestUserType::Any,
                src: "from",
                dest: "to",
                fs_type: "",
                flags: MsFlags::empty(),
                options: "",
                error_contains: "need mount FS type",
            },
            TestData {
                test_user: TestUserType::NonRootOnly,
                src: "from",
                dest: "to",
                fs_type: "bind",
                flags: MsFlags::empty(),
                options: "bind",
                error_contains: "Operation not permitted",
            },
            TestData {
                test_user: TestUserType::NonRootOnly,
                src: "from",
                dest: "to",
                fs_type: "bind",
                flags: MsFlags::MS_BIND,
                options: "",
                error_contains: "Operation not permitted",
            },
            TestData {
                test_user: TestUserType::RootOnly,
                src: "from",
                dest: "to",
                fs_type: "bind",
                flags: MsFlags::MS_BIND,
                options: "",
                error_contains: "",
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            skip_loop_by_user!(msg, d.test_user);

            let src: PathBuf;
            let dest: PathBuf;

            let src_filename: String;
            let dest_filename: String;

            if !d.src.is_empty() {
                src = dir.path().join(d.src);
                src_filename = src
                    .to_str()
                    .expect("failed to convert src to filename")
                    .to_string();
            } else {
                src_filename = "".to_owned();
            }

            if !d.dest.is_empty() {
                dest = dir.path().join(d.dest);
                dest_filename = dest
                    .to_str()
                    .expect("failed to convert dest to filename")
                    .to_string();
            } else {
                dest_filename = "".to_owned();
            }

            // Create the mount directories
            for d in [src_filename.clone(), dest_filename.clone()].iter() {
                if d.is_empty() {
                    continue;
                }

                std::fs::create_dir_all(d).expect("failed to created directory");
            }

            let src = Path::new(&src_filename);
            let dest = Path::new(&dest_filename);

            let result = baremount(src, dest, d.fs_type, d.flags, d.options, &logger);

            let msg = format!("{}: result: {:?}", msg, result);

            if d.error_contains.is_empty() {
                assert!(result.is_ok(), "{}", msg);

                // Cleanup
                nix::mount::umount(dest_filename.as_str()).unwrap();

                continue;
            }

            let err = result.unwrap_err();
            let error_msg = format!("{}", err);
            assert!(error_msg.contains(d.error_contains), "{}", msg);
        }
    }

    #[test]
    fn test_is_mounted() {
        assert!(is_mounted("/proc").unwrap());
        assert!(!is_mounted("").unwrap());
        assert!(!is_mounted("!").unwrap());
        assert!(!is_mounted("/not_existing_path").unwrap());
    }

    #[test]
    fn test_remove_mounts() {
        skip_if_not_root!();

        #[derive(Debug)]
        struct TestData<'a> {
            mounts: Vec<String>,

            // If set, assume an error will be generated,
            // else assume no error.
            error_contains: &'a str,
        }

        let dir = tempdir().expect("failed to create tmpdir");
        let drain = slog::Discard;
        let logger = slog::Logger::root(drain, o!());

        let test_dir_path = dir.path().join("dir");
        let test_dir_filename = test_dir_path
            .to_str()
            .expect("failed to create mount dir filename");

        let test_file_path = dir.path().join("file");
        let test_file_filename = test_file_path
            .to_str()
            .expect("failed to create mount file filename");

        OpenOptions::new()
            .create(true)
            .write(true)
            .open(test_file_filename)
            .expect("failed to create test file");

        std::fs::create_dir_all(test_dir_filename).expect("failed to create dir");

        let mnt_src = dir.path().join("mnt-src");
        let mnt_src_filename = mnt_src
            .to_str()
            .expect("failed to create mount source filename");

        let mnt_dest = dir.path().join("mnt-dest");
        let mnt_dest_filename = mnt_dest
            .to_str()
            .expect("failed to create mount destination filename");

        for d in [test_dir_filename, mnt_src_filename, mnt_dest_filename].iter() {
            std::fs::create_dir_all(d)
                .unwrap_or_else(|_| panic!("failed to create directory {}", d));
        }

        let src = Path::new(mnt_src_filename);
        let dest = Path::new(mnt_dest_filename);

        // Create an actual mount
        let result = baremount(src, dest, "bind", MsFlags::MS_BIND, "", &logger);
        assert!(result.is_ok(), "mount for test setup failed");

        let tests = &[
            TestData {
                mounts: vec![],
                error_contains: "",
            },
            TestData {
                mounts: vec!["".to_string()],
                error_contains: "ENOENT: No such file or directory",
            },
            TestData {
                mounts: vec![test_file_filename.to_string()],
                error_contains: "EINVAL: Invalid argument",
            },
            TestData {
                mounts: vec![test_dir_filename.to_string()],
                error_contains: "EINVAL: Invalid argument",
            },
            TestData {
                mounts: vec![mnt_dest_filename.to_string()],
                error_contains: "",
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let result = remove_mounts(&d.mounts);

            let msg = format!("{}: result: {:?}", msg, result);

            if d.error_contains.is_empty() {
                assert!(result.is_ok(), "{}", msg);
                continue;
            }

            let error_msg = format!("{:#}", result.unwrap_err());

            assert!(error_msg.contains(d.error_contains), "{}", msg);
        }
    }

    #[test]
    fn test_get_mount_fs_type_from_file() {
        #[derive(Debug)]
        struct TestData<'a> {
            // Create file with the specified contents
            // (even if a nul string is specified).
            contents: &'a str,
            mount_point: &'a str,

            // If set, assume an error will be generated,
            // else assume no error.
            error_contains: &'a str,

            // successful return value
            fs_type: &'a str,
        }

        let dir = tempdir().expect("failed to create tmpdir");

        let tests = &[
            TestData {
                contents: "",
                mount_point: "",
                error_contains: "Invalid mount point",
                fs_type: "",
            },
            TestData {
                contents: "foo",
                mount_point: "",
                error_contains: "Invalid mount point",
                fs_type: "",
            },
            TestData {
                contents: "foo",
                mount_point: "/",
                error_contains: "failed to find FS type for mount point /",
                fs_type: "",
            },
            TestData {
                // contents missing fields
                contents: "device /dev/mapper/root mounted on /",
                mount_point: "/",
                error_contains: "failed to find FS type for mount point /",
                fs_type: "",
            },
            TestData {
                contents: "device /dev/mapper/root mounted on / with fstype ext4",
                mount_point: "/",
                error_contains: "",
                fs_type: "ext4",
            },
        ];

        let enoent_file_path = dir.path().join("enoent");
        let enoent_filename = enoent_file_path
            .to_str()
            .expect("failed to create enoent filename");

        // First, test that an empty mount file is handled
        for (i, mp) in ["/", "/somewhere", "/tmp", enoent_filename]
            .iter()
            .enumerate()
        {
            let msg = format!("missing mount file test[{}] with mountpoint: {}", i, mp);

            let result = get_mount_fs_type_from_file("", mp);
            let err = result.unwrap_err();

            let msg = format!("{}: error: {}", msg, err);

            assert!(
                format!("{}", err).contains("No such file or directory"),
                "{}",
                msg
            );
        }

        // Now, test various combinations of file contents
        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let file_path = dir.path().join("mount_stats");

            let filename = file_path
                .to_str()
                .unwrap_or_else(|| panic!("{}: failed to create filename", msg));

            let mut file =
                File::create(filename).unwrap_or_else(|_| panic!("{}: failed to create file", msg));

            file.write_all(d.contents.as_bytes())
                .unwrap_or_else(|_| panic!("{}: failed to write file contents", msg));

            let result = get_mount_fs_type_from_file(filename, d.mount_point);

            // add more details if an assertion fails
            let msg = format!("{}: result: {:?}", msg, result);

            if d.error_contains.is_empty() {
                let fs_type = result.unwrap();

                assert!(d.fs_type == fs_type, "{}", msg);

                continue;
            }

            let error_msg = format!("{}", result.unwrap_err());
            assert!(error_msg.contains(d.error_contains), "{}", msg);
        }
    }

    #[test]
    fn test_get_cgroup_v2_mounts() {
        let _ = tempdir().expect("failed to create tmpdir");
        let drain = slog::Discard;
        let logger = slog::Logger::root(drain, o!());
        let result = get_cgroup_mounts(&logger, "", true);

        assert!(result.is_ok());
        let result = result.unwrap();
        assert_eq!(1, result.len());
        assert_eq!(result[0].fstype, "cgroup2");
        assert_eq!(result[0].src, "cgroup2");
    }

    #[test]
    fn test_get_cgroup_mounts() {
        #[derive(Debug)]
        struct TestData<'a> {
            // Create file with the specified contents
            // (even if a nul string is specified).
            contents: &'a str,

            // If set, assume an error will be generated,
            // else assume no error.
            error_contains: &'a str,

            // Set if the devices cgroup is expected to be found
            devices_cgroup: bool,
        }

        let dir = tempdir().expect("failed to create tmpdir");
        let drain = slog::Discard;
        let logger = slog::Logger::root(drain, o!());

        let first_mount = InitMount {
            fstype: "tmpfs",
            src: "tmpfs",
            dest: SYSFS_CGROUPPATH,
            options: vec!["nosuid", "nodev", "noexec", "mode=755"],
        };

        let last_mount = InitMount {
            fstype: "tmpfs",
            src: "tmpfs",
            dest: SYSFS_CGROUPPATH,
            options: vec!["remount", "ro", "nosuid", "nodev", "noexec", "mode=755"],
        };

        let cg_devices_mount = InitMount {
            fstype: "cgroup",
            src: "cgroup",
            dest: "/sys/fs/cgroup/devices",
            options: vec!["nosuid", "nodev", "noexec", "relatime", "devices"],
        };

        let enoent_file_path = dir.path().join("enoent");
        let enoent_filename = enoent_file_path
            .to_str()
            .expect("failed to create enoent filename");

        let tests = &[
            TestData {
                // Empty file
                contents: "",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Only a comment line
                contents: "#subsys_name	hierarchy	num_cgroups	enabled",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Single (invalid) field
                contents: "foo",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Multiple (invalid) fields
                contents: "this\tis\tinvalid\tdata\n",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Valid first field, but other fields missing
                contents: "devices\n",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Valid first field, but invalid others fields
                contents: "devices\tinvalid\tinvalid\tinvalid\n",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Valid first field, but lots of invalid others fields
                contents: "devices\tinvalid\tinvalid\tinvalid\tinvalid\tinvalid\n",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Valid, but disabled
                contents: "devices\t1\t1\t0\n",
                error_contains: "",
                devices_cgroup: false,
            },
            TestData {
                // Valid
                contents: "devices\t1\t1\t1\n",
                error_contains: "",
                devices_cgroup: true,
            },
        ];

        // First, test a missing file
        let result = get_cgroup_mounts(&logger, enoent_filename, false);

        assert!(result.is_err());
        let error_msg = format!("{}", result.unwrap_err());
        assert!(
            error_msg.contains("No such file or directory"),
            "enoent test"
        );

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let file_path = dir.path().join("cgroups");
            let filename = file_path
                .to_str()
                .expect("failed to create cgroup file filename");

            let mut file =
                File::create(filename).unwrap_or_else(|_| panic!("{}: failed to create file", msg));

            file.write_all(d.contents.as_bytes())
                .unwrap_or_else(|_| panic!("{}: failed to write file contents", msg));

            let result = get_cgroup_mounts(&logger, filename, false);
            let msg = format!("{}: result: {:?}", msg, result);

            if !d.error_contains.is_empty() {
                assert!(result.is_err(), "{}", msg);

                let error_msg = format!("{}", result.unwrap_err());
                assert!(error_msg.contains(d.error_contains), "{}", msg);
                continue;
            }

            assert!(result.is_ok(), "{}", msg);

            let mounts = result.unwrap();
            let count = mounts.len();

            if !d.devices_cgroup {
                assert!(count == 0, "{}", msg);
                continue;
            }

            // get_cgroup_mounts() adds the device cgroup plus two other mounts.
            assert!(count == (1 + 2), "{}", msg);

            // First mount
            assert!(mounts[0].eq(&first_mount), "{}", msg);

            // Last mount
            assert!(mounts[2].eq(&last_mount), "{}", msg);

            // Devices cgroup
            assert!(mounts[1].eq(&cg_devices_mount), "{}", msg);
        }
    }

    #[test]
    fn test_ensure_destination_file_exists() {
        let dir = tempdir().expect("failed to create tmpdir");

        let mut testfile = dir.into_path();
        testfile.push("testfile");

        let result = ensure_destination_file_exists(&testfile);

        assert!(result.is_ok());
        assert!(testfile.exists());

        let result = ensure_destination_file_exists(&testfile);
        assert!(result.is_ok());

        assert!(testfile.is_file());
    }

    #[test]
    fn test_mount_storage() {
        #[derive(Debug)]
        struct TestData<'a> {
            test_user: TestUserType,
            storage: Storage,
            error_contains: &'a str,

            make_source_dir: bool,
            make_mount_dir: bool,
            deny_mount_permission: bool,
        }

        impl Default for TestData<'_> {
            fn default() -> Self {
                TestData {
                    test_user: TestUserType::Any,
                    storage: Storage {
                        mount_point: "mnt".to_string(),
                        source: "src".to_string(),
                        fstype: "tmpfs".to_string(),
                        ..Default::default()
                    },
                    make_source_dir: true,
                    make_mount_dir: false,
                    deny_mount_permission: false,
                    error_contains: "",
                }
            }
        }

        let tests = &[
            TestData {
                test_user: TestUserType::NonRootOnly,
                error_contains: "EPERM: Operation not permitted",
                ..Default::default()
            },
            TestData {
                test_user: TestUserType::RootOnly,
                ..Default::default()
            },
            TestData {
                storage: Storage {
                    mount_point: "mnt".to_string(),
                    source: "src".to_string(),
                    fstype: "bind".to_string(),
                    ..Default::default()
                },
                make_source_dir: false,
                make_mount_dir: true,
                error_contains: "Could not create mountpoint",
                ..Default::default()
            },
            TestData {
                test_user: TestUserType::NonRootOnly,
                deny_mount_permission: true,
                error_contains: "Could not create mountpoint",
                ..Default::default()
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            skip_loop_by_user!(msg, d.test_user);

            let drain = slog::Discard;
            let logger = slog::Logger::root(drain, o!());

            let tempdir = tempdir().unwrap();

            let source = tempdir.path().join(&d.storage.source);
            let mount_point = tempdir.path().join(&d.storage.mount_point);

            let storage = Storage {
                source: source.to_str().unwrap().to_string(),
                mount_point: mount_point.to_str().unwrap().to_string(),
                ..d.storage.clone()
            };

            if d.make_source_dir {
                fs::create_dir_all(&storage.source).unwrap();
            }
            if d.make_mount_dir {
                fs::create_dir_all(&storage.mount_point).unwrap();
            }

            if d.deny_mount_permission {
                fs::set_permissions(
                    mount_point.parent().unwrap(),
                    fs::Permissions::from_mode(0o000),
                )
                .unwrap();
            }

            let result = mount_storage(&logger, &storage);

            // restore permissions so tempdir can be cleaned up
            if d.deny_mount_permission {
                fs::set_permissions(
                    mount_point.parent().unwrap(),
                    fs::Permissions::from_mode(0o755),
                )
                .unwrap();
            }

            if result.is_ok() {
                nix::mount::umount(&mount_point).unwrap();
            }

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
    fn test_mount_to_rootfs() {
        #[derive(Debug)]
        struct TestData<'a> {
            test_user: TestUserType,
            src: &'a str,
            options: Vec<&'a str>,
            error_contains: &'a str,
            deny_mount_dir_permission: bool,
            // if true src will be prepended with a temporary directory
            mask_src: bool,
        }

        impl Default for TestData<'_> {
            fn default() -> Self {
                TestData {
                    test_user: TestUserType::Any,
                    src: "src",
                    options: vec![],
                    error_contains: "",
                    deny_mount_dir_permission: false,
                    mask_src: true,
                }
            }
        }

        let tests = &[
            TestData {
                test_user: TestUserType::NonRootOnly,
                error_contains: "EPERM: Operation not permitted",
                ..Default::default()
            },
            TestData {
                test_user: TestUserType::NonRootOnly,
                src: "dev",
                mask_src: false,
                ..Default::default()
            },
            TestData {
                test_user: TestUserType::RootOnly,
                ..Default::default()
            },
            TestData {
                test_user: TestUserType::NonRootOnly,
                deny_mount_dir_permission: true,
                error_contains: "could not create directory",
                ..Default::default()
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);
            skip_loop_by_user!(msg, d.test_user);

            let drain = slog::Discard;
            let logger = slog::Logger::root(drain, o!());
            let tempdir = tempdir().unwrap();

            let src = if d.mask_src {
                tempdir.path().join(&d.src)
            } else {
                Path::new(d.src).to_path_buf()
            };
            let dest = tempdir.path().join("mnt");
            let init_mount = InitMount {
                fstype: "tmpfs",
                src: src.to_str().unwrap(),
                dest: dest.to_str().unwrap(),
                options: d.options.clone(),
            };

            if d.deny_mount_dir_permission {
                fs::set_permissions(dest.parent().unwrap(), fs::Permissions::from_mode(0o000))
                    .unwrap();
            }

            let result = mount_to_rootfs(&logger, &init_mount);

            // restore permissions so tempdir can be cleaned up
            if d.deny_mount_dir_permission {
                fs::set_permissions(dest.parent().unwrap(), fs::Permissions::from_mode(0o755))
                    .unwrap();
            }

            if result.is_ok() && d.mask_src {
                nix::mount::umount(&dest).unwrap();
            }

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
    fn test_get_pagesize_and_size_from_option() {
        let expected_pagesize = 2048;
        let expected_size = 107374182;
        let expected = (expected_pagesize, expected_size);

        let data = vec![
            // (input, expected, is_ok)
            ("size-1=107374182,pagesize-1=2048", expected, false),
            ("size-1=107374182,pagesize=2048", expected, false),
            ("size=107374182,pagesize-1=2048", expected, false),
            ("size=107374182,pagesize=abc", expected, false),
            ("size=abc,pagesize=2048", expected, false),
            ("size=,pagesize=2048", expected, false),
            ("size=107374182,pagesize=", expected, false),
            ("size=107374182,pagesize=2048", expected, true),
            ("pagesize=2048,size=107374182", expected, true),
            ("foo=bar,pagesize=2048,size=107374182", expected, true),
            (
                "foo=bar,pagesize=2048,foo1=bar1,size=107374182",
                expected,
                true,
            ),
            (
                "pagesize=2048,foo1=bar1,foo=bar,size=107374182",
                expected,
                true,
            ),
            (
                "foo=bar,pagesize=2048,foo1=bar1,size=107374182,foo2=bar2",
                expected,
                true,
            ),
            (
                "foo=bar,size=107374182,foo1=bar1,pagesize=2048",
                expected,
                true,
            ),
        ];

        for case in data {
            let input = case.0;
            let r = get_pagesize_and_size_from_option(&[input.to_string()]);

            let is_ok = case.2;
            if is_ok {
                let expected = case.1;
                let (pagesize, size) = r.unwrap();
                assert_eq!(expected.0, pagesize);
                assert_eq!(expected.1, size);
            } else {
                assert!(r.is_err());
            }
        }
    }

    #[test]
    fn test_parse_mount_flags_and_options() {
        #[derive(Debug)]
        struct TestData<'a> {
            options_vec: Vec<&'a str>,
            result: (MsFlags, &'a str),
        }

        let tests = &[
            TestData {
                options_vec: vec![],
                result: (MsFlags::empty(), ""),
            },
            TestData {
                options_vec: vec!["ro"],
                result: (MsFlags::MS_RDONLY, ""),
            },
            TestData {
                options_vec: vec!["rw"],
                result: (MsFlags::empty(), ""),
            },
            TestData {
                options_vec: vec!["ro", "rw"],
                result: (MsFlags::empty(), ""),
            },
            TestData {
                options_vec: vec!["ro", "nodev"],
                result: (MsFlags::MS_RDONLY | MsFlags::MS_NODEV, ""),
            },
            TestData {
                options_vec: vec!["option1", "nodev", "option2"],
                result: (MsFlags::MS_NODEV, "option1,option2"),
            },
            TestData {
                options_vec: vec!["rbind", "", "ro"],
                result: (MsFlags::MS_BIND | MsFlags::MS_REC | MsFlags::MS_RDONLY, ""),
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let result = parse_mount_flags_and_options(d.options_vec.clone());

            let msg = format!("{}: result: {:?}", msg, result);

            let expected_result = (d.result.0, d.result.1.to_owned());
            assert_eq!(expected_result, result, "{}", msg);
        }
    }

    #[test]
    fn test_set_ownership() {
        skip_if_not_root!();

        let logger = slog::Logger::root(slog::Discard, o!());

        #[derive(Debug)]
        struct TestData<'a> {
            mount_path: &'a str,
            fs_group: Option<FSGroup>,
            read_only: bool,
            expected_group_id: u32,
            expected_permission: u32,
        }

        let tests = &[
            TestData {
                mount_path: "foo",
                fs_group: None,
                read_only: false,
                expected_group_id: 0,
                expected_permission: 0,
            },
            TestData {
                mount_path: "rw_mount",
                fs_group: Some(FSGroup {
                    group_id: 3000,
                    group_change_policy: FSGroupChangePolicy::Always.into(),
                    ..Default::default()
                }),
                read_only: false,
                expected_group_id: 3000,
                expected_permission: RW_MASK | EXEC_MASK | MODE_SETGID,
            },
            TestData {
                mount_path: "ro_mount",
                fs_group: Some(FSGroup {
                    group_id: 3000,
                    group_change_policy: FSGroupChangePolicy::OnRootMismatch.into(),
                    ..Default::default()
                }),
                read_only: true,
                expected_group_id: 3000,
                expected_permission: RO_MASK | EXEC_MASK | MODE_SETGID,
            },
        ];

        let tempdir = tempdir().expect("failed to create tmpdir");

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let mount_dir = tempdir.path().join(d.mount_path);
            fs::create_dir(&mount_dir)
                .unwrap_or_else(|_| panic!("{}: failed to create root directory", msg));

            let directory_mode = mount_dir.as_path().metadata().unwrap().permissions().mode();
            let mut storage_data = Storage::new();
            if d.read_only {
                storage_data.set_options(Vec::from(&["foo".to_string(), "ro".to_string()]));
            }
            if let Some(fs_group) = d.fs_group.clone() {
                storage_data.set_fs_group(fs_group);
            }
            storage_data.mount_point = mount_dir.clone().into_os_string().into_string().unwrap();

            let result = set_ownership(&logger, &storage_data);
            assert!(result.is_ok());

            assert_eq!(
                mount_dir.as_path().metadata().unwrap().gid(),
                d.expected_group_id
            );
            assert_eq!(
                mount_dir.as_path().metadata().unwrap().permissions().mode(),
                (directory_mode | d.expected_permission)
            );
        }
    }

    #[test]
    fn test_recursive_ownership_change() {
        skip_if_not_root!();

        const COUNT: usize = 5;

        #[derive(Debug)]
        struct TestData<'a> {
            // Directory where the recursive ownership change should be performed on
            path: &'a str,

            // User ID for ownership change
            uid: u32,

            // Group ID for ownership change
            gid: u32,

            // Set when the permission should be read-only
            read_only: bool,

            // The expected permission of all directories after ownership change
            expected_permission_directory: u32,

            // The expected permission of all files after ownership change
            expected_permission_file: u32,
        }

        let tests = &[
            TestData {
                path: "no_gid_change",
                uid: 0,
                gid: 0,
                read_only: false,
                expected_permission_directory: 0,
                expected_permission_file: 0,
            },
            TestData {
                path: "rw_gid_change",
                uid: 0,
                gid: 3000,
                read_only: false,
                expected_permission_directory: RW_MASK | EXEC_MASK | MODE_SETGID,
                expected_permission_file: RW_MASK,
            },
            TestData {
                path: "ro_gid_change",
                uid: 0,
                gid: 3000,
                read_only: true,
                expected_permission_directory: RO_MASK | EXEC_MASK | MODE_SETGID,
                expected_permission_file: RO_MASK,
            },
        ];

        let tempdir = tempdir().expect("failed to create tmpdir");

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let mount_dir = tempdir.path().join(d.path);
            fs::create_dir(&mount_dir)
                .unwrap_or_else(|_| panic!("{}: failed to create root directory", msg));

            let directory_mode = mount_dir.as_path().metadata().unwrap().permissions().mode();
            let mut file_mode: u32 = 0;

            // create testing directories and files
            for n in 1..COUNT {
                let nest_dir = mount_dir.join(format!("nested{}", n));
                fs::create_dir(&nest_dir)
                    .unwrap_or_else(|_| panic!("{}: failed to create nest directory", msg));

                for f in 1..COUNT {
                    let filename = nest_dir.join(format!("file{}", f));
                    File::create(&filename)
                        .unwrap_or_else(|_| panic!("{}: failed to create file", msg));
                    file_mode = filename.as_path().metadata().unwrap().permissions().mode();
                }
            }

            let uid = if d.uid > 0 {
                Some(Uid::from_raw(d.uid))
            } else {
                None
            };
            let gid = if d.gid > 0 {
                Some(Gid::from_raw(d.gid))
            } else {
                None
            };
            let result = recursive_ownership_change(&mount_dir, uid, gid, d.read_only);

            assert!(result.is_ok());

            assert_eq!(mount_dir.as_path().metadata().unwrap().gid(), d.gid);
            assert_eq!(
                mount_dir.as_path().metadata().unwrap().permissions().mode(),
                (directory_mode | d.expected_permission_directory)
            );

            for n in 1..COUNT {
                let nest_dir = mount_dir.join(format!("nested{}", n));
                for f in 1..COUNT {
                    let filename = nest_dir.join(format!("file{}", f));
                    let file = Path::new(&filename);

                    assert_eq!(file.metadata().unwrap().gid(), d.gid);
                    assert_eq!(
                        file.metadata().unwrap().permissions().mode(),
                        (file_mode | d.expected_permission_file)
                    );
                }

                let dir = Path::new(&nest_dir);
                assert_eq!(dir.metadata().unwrap().gid(), d.gid);
                assert_eq!(
                    dir.metadata().unwrap().permissions().mode(),
                    (directory_mode | d.expected_permission_directory)
                );
            }
        }
    }
}
