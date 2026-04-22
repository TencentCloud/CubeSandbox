// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::linux_abi::*;
use crate::mount::{get_mount_fs_type, remove_mounts, TYPE_ROOTFS};
use crate::namespace::Namespace;
use crate::netlink::Handle;
use crate::network::Network;
use crate::pci;
use crate::uevent::{Uevent, UeventMatcher};
use crate::watcher::BindWatcher;
use anyhow::{anyhow, Context, Result};
use libc::pid_t;
use oci::Hooks;
use protocols::agent::OnlineCPUMemRequest;
use regex::Regex;
use rustjail::cgroups as rustjail_cgroups;
use rustjail::container::BaseContainer;
use rustjail::container::LinuxContainer;
use rustjail::process::Process;
use slog::Logger;
use std::collections::HashMap;
use std::fs;
use std::{thread, time};
use tokio::sync::broadcast::{channel, Sender as BroadcastSender};
use tokio::sync::mpsc::Receiver;
use tokio::sync::oneshot;
use tracing::instrument;

pub const ERR_INVALID_CONTAINER_ID: &str = "Invalid container id";

type UeventWatcher = (Box<dyn UeventMatcher>, oneshot::Sender<Uevent>);

#[derive(Debug)]
pub struct Sandbox {
    pub logger: Logger,
    pub id: String,
    pub hostname: String,
    pub containers: HashMap<String, LinuxContainer>,
    pub network: Network,
    pub mounts: Vec<String>,
    pub container_mounts: HashMap<String, Vec<String>>,
    pub uevent_map: HashMap<String, Uevent>,
    pub uevent_watchers: Vec<Option<UeventWatcher>>,
    pub shared_utsns: Namespace,
    pub shared_ipcns: Namespace,
    pub sandbox_pidns: Option<Namespace>,
    pub storages: HashMap<String, u32>,
    pub running: bool,
    pub no_pivot_root: bool,
    pub sender: Option<tokio::sync::oneshot::Sender<i32>>,
    pub rtnl: Handle,
    pub hooks: Option<Hooks>,
    pub event_tx: Option<BroadcastSender<String>>,
    pub bind_watcher: BindWatcher,
    pub pcimap: HashMap<pci::Address, pci::Address>,
}

impl Sandbox {
    #[instrument]
    pub fn new(logger: &Logger) -> Result<Self> {
        let fs_type = get_mount_fs_type("/")?;
        let logger = logger.new(o!("subsystem" => "sandbox"));
        let (tx, _) = channel::<String>(100);
        Ok(Sandbox {
            logger: logger.clone(),
            id: String::new(),
            hostname: String::new(),
            network: Network::new(),
            containers: HashMap::new(),
            mounts: Vec::new(),
            container_mounts: HashMap::new(),
            uevent_map: HashMap::new(),
            uevent_watchers: Vec::new(),
            shared_utsns: Namespace::new(&logger),
            shared_ipcns: Namespace::new(&logger),
            sandbox_pidns: None,
            storages: HashMap::new(),
            running: false,
            no_pivot_root: fs_type.eq(TYPE_ROOTFS),
            sender: None,
            rtnl: Handle::new()?,
            hooks: None,
            event_tx: Some(tx),
            bind_watcher: BindWatcher::new(),
            pcimap: HashMap::new(),
        })
    }

    // set_sandbox_storage sets the sandbox level reference
    // counter for the sandbox storage.
    // This method also returns a boolean to let
    // callers know if the storage already existed or not.
    // It will return true if storage is new.
    //
    // It's assumed that caller is calling this method after
    // acquiring a lock on sandbox.
    #[instrument]
    pub fn set_sandbox_storage(&mut self, path: &str) -> bool {
        match self.storages.get_mut(path) {
            None => {
                self.storages.insert(path.to_string(), 1);
                true
            }
            Some(count) => {
                *count += 1;
                false
            }
        }
    }

    // unset_sandbox_storage will decrement the sandbox storage
    // reference counter. If there aren't any containers using
    // that sandbox storage, this method will remove the
    // storage reference from the sandbox and return 'true' to
    // let the caller know that they can clean up the storage
    // related directories by calling remove_sandbox_storage
    //
    // It's assumed that caller is calling this method after
    // acquiring a lock on sandbox.
    #[instrument]
    pub fn unset_sandbox_storage(&mut self, path: &str) -> Result<bool> {
        match self.storages.get_mut(path) {
            None => Err(anyhow!("Sandbox storage with path {} not found", path)),
            Some(count) => {
                *count -= 1;
                if *count < 1 {
                    self.storages.remove(path);
                    return Ok(true);
                }
                Ok(false)
            }
        }
    }

    // remove_sandbox_storage removes the sandbox storage if no
    // containers are using that storage.
    //
    // It's assumed that caller is calling this method after
    // acquiring a lock on sandbox.
    #[instrument]
    pub fn remove_sandbox_storage(&self, path: &str) -> Result<()> {
        let mounts = vec![path.to_string()];
        remove_mounts(&mounts)?;
        // "remove_dir" will fail if the mount point is backed by a read-only filesystem.
        // This is the case with the device mapper snapshotter, where we mount the block device directly
        // at the underlying sandbox path which was provided from the base RO kataShared path from the host.
        if let Err(err) = fs::remove_dir(path) {
            warn!(self.logger, "failed to remove dir {}, {:?}", path, err);
        }
        Ok(())
    }

    // unset_and_remove_sandbox_storage unsets the storage from sandbox
    // and if there are no containers using this storage it will
    // remove it from the sandbox.
    //
    // It's assumed that caller is calling this method after
    // acquiring a lock on sandbox.
    #[instrument]
    pub fn unset_and_remove_sandbox_storage(&mut self, path: &str) -> Result<()> {
        if self.unset_sandbox_storage(path)? {
            return self.remove_sandbox_storage(path);
        }

        Ok(())
    }

    #[instrument]
    pub async fn setup_shared_namespaces(&mut self) -> Result<bool> {
        // Set up shared IPC namespace
        self.shared_ipcns = Namespace::new(&self.logger)
            .get_ipc()
            .setup()
            .await
            .context("Failed to setup persistent IPC namespace")?;

        // // Set up shared UTS namespace
        self.shared_utsns = Namespace::new(&self.logger)
            .get_uts(self.hostname.as_str())
            .setup()
            .await
            .context("Failed to setup persistent UTS namespace")?;

        Ok(true)
    }

    pub fn add_container(&mut self, c: LinuxContainer) {
        self.containers.insert(c.id.clone(), c);
    }

    #[instrument]
    pub fn update_shared_pidns(&mut self, c: &LinuxContainer) -> Result<()> {
        // Populate the shared pid path only if this is an infra container and
        // sandbox_pidns has not been passed in the create_sandbox request.
        // This means a separate pause process has not been created. We treat the
        // first container created as the infra container in that case
        // and use its pid namespace in case pid namespace needs to be shared.
        if self.sandbox_pidns.is_none() && self.containers.is_empty() {
            let init_pid = c.init_process_pid;
            if init_pid == -1 {
                return Err(anyhow!(
                    "Failed to setup pid namespace: init container pid is -1"
                ));
            }

            let mut pid_ns = Namespace::new(&self.logger).get_pid();
            pid_ns.path = format!("/proc/{}/ns/pid", init_pid);

            self.sandbox_pidns = Some(pid_ns);
        }

        Ok(())
    }

    pub fn get_container(&mut self, id: &str) -> Option<&mut LinuxContainer> {
        self.containers.get_mut(id)
    }

    pub fn find_process(&mut self, pid: pid_t) -> Option<&mut Process> {
        for (_, c) in self.containers.iter_mut() {
            if c.processes.get(&pid).is_some() {
                return c.processes.get_mut(&pid);
            }
        }

        None
    }

    pub fn find_container_process(&mut self, cid: &str, eid: &str) -> Result<&mut Process> {
        let ctr = self
            .get_container(cid)
            .ok_or_else(|| anyhow!(ERR_INVALID_CONTAINER_ID))?;

        if eid.is_empty() {
            return ctr
                .processes
                .get_mut(&ctr.init_process_pid)
                .ok_or_else(|| anyhow!("cannot find init process!"));
        }

        ctr.get_process(eid).map_err(|_| anyhow!("Invalid exec id"))
    }

    /*
    #[instrument]
    pub async fn destroy(&mut self) -> Result<()> {
        for ctr in self.containers.values_mut() {
            ctr.destroy().await?;
        }
        Ok(())
    }*/

    #[instrument]
    pub fn online_cpu_memory(&self, req: &OnlineCPUMemRequest) -> Result<()> {
        if req.nb_cpus > 0 {
            // online cpus
            online_cpus(&self.logger, req.nb_cpus as i32)?;
        }

        if !req.cpu_only {
            // online memory
            online_memory(&self.logger)?;
        }

        if req.nb_cpus == 0 {
            return Ok(());
        }

        let guest_cpuset = rustjail_cgroups::fs::get_guest_cpuset()?;

        for (_, ctr) in self.containers.iter() {
            let cpu = ctr
                .config
                .spec
                .as_ref()
                .unwrap()
                .linux
                .as_ref()
                .unwrap()
                .resources
                .as_ref()
                .unwrap()
                .cpu
                .as_ref();
            let container_cpust = if let Some(c) = cpu { &c.cpus } else { "" };

            info!(self.logger, "updating {}", ctr.id.as_str());
            ctr.cgroup_manager
                .as_ref()
                .unwrap()
                .update_cpuset_path(guest_cpuset.as_str(), container_cpust)?;
        }

        Ok(())
    }

    #[instrument]
    pub async fn run_oom_event_monitor(&self, mut rx: Receiver<String>, container_id: String) {
        let logger = self.logger.clone();

        if self.event_tx.is_none() {
            error!(
                logger,
                "sandbox.event_tx not found in run_oom_event_monitor"
            );
            return;
        }

        let tx = self.event_tx.as_ref().unwrap().clone();

        tokio::spawn(async move {
            loop {
                let event = rx.recv().await;
                // None means the container has exited,
                // and sender in OOM notifier is dropped.
                if event.is_none() {
                    return;
                }
                info!(logger, "got an OOM event {:?}", event);

                let _ = tx
                    .send(container_id.clone())
                    .map_err(|e| error!(logger, "failed to send message: {:?}", e));
            }
        });
    }
}

#[instrument]
fn online_resources(logger: &Logger, path: &str, pattern: &str, num: i32) -> Result<i32> {
    let mut count = 0;
    let re = Regex::new(pattern)?;

    for e in fs::read_dir(path)? {
        let entry = e?;
        let tmpname = entry.file_name();
        let name = tmpname.to_str().unwrap();
        let p = entry.path();

        if re.is_match(name) {
            let file = format!("{}/{}", p.to_str().unwrap(), SYSFS_ONLINE_FILE);
            info!(logger, "{}", file.as_str());

            let c = fs::read_to_string(file.as_str());
            if c.is_err() {
                continue;
            }
            let c = c.unwrap();

            if c.trim().contains('0') {
                let r = fs::write(file.as_str(), "1");
                if r.is_err() {
                    continue;
                }
                count += 1;

                if num > 0 && count == num {
                    break;
                }
            }
        }
    }

    if num > 0 {
        return Ok(count);
    }

    Ok(0)
}

// max wait for all CPUs to online will use 50 * 100 = 5 seconds.
const ONLINE_CPUMEM_WATI_MILLIS: u64 = 50;
const ONLINE_CPUMEM_MAX_RETRIES: u32 = 100;

#[instrument]
fn online_cpus(logger: &Logger, num: i32) -> Result<i32> {
    let mut onlined_count: i32 = 0;

    for i in 0..ONLINE_CPUMEM_MAX_RETRIES {
        let r = online_resources(
            logger,
            SYSFS_CPU_ONLINE_PATH,
            r"cpu[0-9]+",
            num - onlined_count,
        );

        onlined_count += r?;
        if onlined_count == num {
            info!(logger, "online {} CPU(s) after {} retries", num, i);
            return Ok(num);
        }
        thread::sleep(time::Duration::from_millis(ONLINE_CPUMEM_WATI_MILLIS));
    }

    Err(anyhow!(
        "failed to online {} CPU(s) after {} retries",
        num,
        ONLINE_CPUMEM_MAX_RETRIES
    ))
}

#[instrument]
fn online_memory(logger: &Logger) -> Result<()> {
    online_resources(logger, SYSFS_MEMORY_ONLINE_PATH, r"memory[0-9]+", -1)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{mount::baremount, skip_if_not_root};
    use anyhow::{anyhow, Error};
    use nix::mount::MsFlags;
    use oci::{Linux, Root, Spec};
    use rustjail::container::LinuxContainer;
    use rustjail::process::Process;
    use rustjail::specconv::CreateOpts;
    use slog::Logger;
    use std::fs::{self, File};
    use std::io::prelude::*;
    use std::os::unix::fs::PermissionsExt;
    use std::path::Path;
    use tempfile::{tempdir, Builder, TempDir};

    fn bind_mount(src: &str, dst: &str, logger: &Logger) -> Result<(), Error> {
        let src_path = Path::new(src);
        let dst_path = Path::new(dst);

        baremount(src_path, dst_path, "bind", MsFlags::MS_BIND, "", logger)
    }

    use serial_test::serial;

    #[tokio::test]
    #[serial]
    async fn set_sandbox_storage() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();

        let tmpdir = Builder::new().tempdir().unwrap();
        let tmpdir_path = tmpdir.path().to_str().unwrap();

        // Add a new sandbox storage
        let new_storage = s.set_sandbox_storage(tmpdir_path);

        // Check the reference counter
        let ref_count = s.storages[tmpdir_path];
        assert_eq!(
            ref_count, 1,
            "Invalid refcount, got {} expected 1.",
            ref_count
        );
        assert!(new_storage);

        // Use the existing sandbox storage
        let new_storage = s.set_sandbox_storage(tmpdir_path);
        assert!(!new_storage, "Should be false as already exists.");

        // Since we are using existing storage, the reference counter
        // should be 2 by now.
        let ref_count = s.storages[tmpdir_path];
        assert_eq!(
            ref_count, 2,
            "Invalid refcount, got {} expected 2.",
            ref_count
        );
    }

    #[tokio::test]
    #[serial]
    async fn remove_sandbox_storage() {
        skip_if_not_root!();

        let logger = slog::Logger::root(slog::Discard, o!());
        let s = Sandbox::new(&logger).unwrap();

        let tmpdir = Builder::new().tempdir().unwrap();
        let tmpdir_path = tmpdir.path().to_str().unwrap();

        let srcdir = Builder::new()
            .prefix("src")
            .tempdir_in(tmpdir_path)
            .unwrap();
        let srcdir_path = srcdir.path().to_str().unwrap();

        let destdir = Builder::new()
            .prefix("dest")
            .tempdir_in(tmpdir_path)
            .unwrap();
        let destdir_path = destdir.path().to_str().unwrap();

        let emptydir = Builder::new()
            .prefix("empty")
            .tempdir_in(tmpdir_path)
            .unwrap();

        assert!(
            s.remove_sandbox_storage(srcdir_path).is_err(),
            "Expect Err as the directory is not a mountpoint"
        );

        assert!(s.remove_sandbox_storage("").is_err());

        let invalid_dir = emptydir.path().join("invalid");

        assert!(s
            .remove_sandbox_storage(invalid_dir.to_str().unwrap())
            .is_err());

        assert!(bind_mount(srcdir_path, destdir_path, &logger).is_ok());

        assert!(s.remove_sandbox_storage(destdir_path).is_ok());
    }

    #[tokio::test]
    #[serial]
    async fn unset_and_remove_sandbox_storage() {
        skip_if_not_root!();

        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();

        assert!(
            s.unset_and_remove_sandbox_storage("/tmp/testEphePath")
                .is_err(),
            "Should fail because sandbox storage doesn't exist"
        );

        let tmpdir = Builder::new().tempdir().unwrap();
        let tmpdir_path = tmpdir.path().to_str().unwrap();

        let srcdir = Builder::new()
            .prefix("src")
            .tempdir_in(tmpdir_path)
            .unwrap();
        let srcdir_path = srcdir.path().to_str().unwrap();

        let destdir = Builder::new()
            .prefix("dest")
            .tempdir_in(tmpdir_path)
            .unwrap();
        let destdir_path = destdir.path().to_str().unwrap();

        assert!(bind_mount(srcdir_path, destdir_path, &logger).is_ok());

        assert!(s.set_sandbox_storage(destdir_path));
        assert!(s.unset_and_remove_sandbox_storage(destdir_path).is_ok());

        let other_dir_str;
        {
            // Create another folder in a separate scope to ensure that is
            // deleted
            let other_dir = Builder::new()
                .prefix("dir")
                .tempdir_in(tmpdir_path)
                .unwrap();
            let other_dir_path = other_dir.path().to_str().unwrap();
            other_dir_str = other_dir_path.to_string();

            assert!(s.set_sandbox_storage(other_dir_path));
        }

        assert!(s.unset_and_remove_sandbox_storage(&other_dir_str).is_err());
    }

    #[tokio::test]
    #[serial]
    async fn unset_sandbox_storage() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();

        let storage_path = "/tmp/testEphe";

        // Add a new sandbox storage
        assert!(s.set_sandbox_storage(storage_path));
        // Use the existing sandbox storage
        assert!(
            !s.set_sandbox_storage(storage_path),
            "Expects false as the storage is not new."
        );

        assert!(
            !s.unset_sandbox_storage(storage_path).unwrap(),
            "Expects false as there is still a storage."
        );

        // Reference counter should decrement to 1.
        let ref_count = s.storages[storage_path];
        assert_eq!(
            ref_count, 1,
            "Invalid refcount, got {} expected 1.",
            ref_count
        );

        assert!(
            s.unset_sandbox_storage(storage_path).unwrap(),
            "Expects true as there is still a storage."
        );

        // Since no container is using this sandbox storage anymore
        // there should not be any reference in sandbox struct
        // for the given storage
        assert!(
            !s.storages.contains_key(storage_path),
            "The storages map should not contain the key {}",
            storage_path
        );

        // If no container is using the sandbox storage, the reference
        // counter for it should not exist.
        assert!(
            s.unset_sandbox_storage(storage_path).is_err(),
            "Expects false as the reference counter should no exist."
        );
    }

    fn create_dummy_opts() -> CreateOpts {
        let root = Root {
            path: String::from("/"),
            ..Default::default()
        };

        let spec = Spec {
            linux: Some(Linux::default()),
            root: Some(root),
            ..Default::default()
        };

        CreateOpts {
            cgroup_name: "".to_string(),
            use_systemd_cgroup: false,
            no_pivot_root: false,
            no_new_keyring: false,
            spec: Some(spec),
            rootless_euid: false,
            rootless_cgroup: false,
        }
    }

    fn create_linuxcontainer() -> (LinuxContainer, TempDir) {
        // Create a temporal directory
        let dir = tempdir()
            .map_err(|e| anyhow!(e).context("tempdir failed"))
            .unwrap();

        // Create a new container
        (
            LinuxContainer::new(
                "some_id",
                dir.path().join("rootfs").to_str().unwrap(),
                create_dummy_opts(),
                &slog_scope::logger(),
            )
            .unwrap(),
            dir,
        )
    }

    #[tokio::test]
    #[serial]
    async fn get_container_entry_exist() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();
        let (linux_container, _root) = create_linuxcontainer();

        s.containers
            .insert("testContainerID".to_string(), linux_container);
        let cnt = s.get_container("testContainerID");
        assert!(cnt.is_some());
    }

    #[tokio::test]
    #[serial]
    async fn get_container_no_entry() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();

        let cnt = s.get_container("testContainerID");
        assert!(cnt.is_none());
    }

    #[tokio::test]
    #[serial]
    async fn add_and_get_container() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();
        let (linux_container, _root) = create_linuxcontainer();

        s.add_container(linux_container);
        assert!(s.get_container("some_id").is_some());
    }

    #[tokio::test]
    #[serial]
    async fn update_shared_pidns() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();
        let test_pid = 9999;

        let (mut linux_container, _root) = create_linuxcontainer();
        linux_container.init_process_pid = test_pid;

        s.update_shared_pidns(&linux_container).unwrap();

        assert!(s.sandbox_pidns.is_some());

        let ns_path = format!("/proc/{}/ns/pid", test_pid);
        assert_eq!(s.sandbox_pidns.unwrap().path, ns_path);
    }

    #[tokio::test]
    #[serial]
    async fn add_guest_hooks() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let s = Sandbox::new(&logger).unwrap();
        let tmpdir = Builder::new().tempdir().unwrap();
        let _tmpdir_path = tmpdir.path().to_str().unwrap();

        assert!(fs::create_dir_all(tmpdir.path().join("prestart")).is_ok());
        assert!(fs::create_dir_all(tmpdir.path().join("poststop")).is_ok());

        let file = File::create(tmpdir.path().join("prestart").join("prestart.sh")).unwrap();
        let mut perm = file.metadata().unwrap().permissions();
        perm.set_mode(0o777);
        assert!(file.set_permissions(perm).is_ok());
        assert!(File::create(tmpdir.path().join("poststop").join("poststop.sh")).is_ok());

        //assert!(s.add_hooks(tmpdir_path).is_ok());
        assert!(s.hooks.is_some());
        assert!(s.hooks.as_ref().unwrap().prestart.len() == 1);
        assert!(s.hooks.as_ref().unwrap().poststart.is_empty());
        assert!(s.hooks.as_ref().unwrap().poststop.is_empty());
    }

    #[tokio::test]
    async fn test_find_container_process() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();
        let cid = "container-123";

        let (mut linux_container, _root) = create_linuxcontainer();
        linux_container.init_process_pid = 1;
        linux_container.id = cid.to_string();
        // add init process
        linux_container.processes.insert(
            1,
            Process::new(&logger, &oci::Process::default(), "1", true, 1).unwrap(),
        );
        // add exec process
        linux_container.processes.insert(
            123,
            Process::new(&logger, &oci::Process::default(), "exec-123", false, 1).unwrap(),
        );

        s.add_container(linux_container);

        // empty exec-id will return init process
        let p = s.find_container_process(cid, "");
        assert!(p.is_ok(), "Expecting Ok, Got {:?}", p);
        let p = p.unwrap();
        assert_eq!("1", p.exec_id, "exec_id should be 1");
        assert!(p.init, "init flag should be true");

        // get exist exec-id will return the exec process
        let p = s.find_container_process(cid, "exec-123");
        assert!(p.is_ok(), "Expecting Ok, Got {:?}", p);
        let p = p.unwrap();
        assert_eq!("exec-123", p.exec_id, "exec_id should be exec-123");
        assert!(!p.init, "init flag should be false");

        // get not exist exec-id will return error
        let p = s.find_container_process(cid, "exec-456");
        assert!(p.is_err(), "Expecting Error, Got {:?}", p);

        // container does not exist
        let p = s.find_container_process("not-exist-cid", "");
        assert!(p.is_err(), "Expecting Error, Got {:?}", p);
    }

    #[tokio::test]
    async fn test_find_process() {
        let logger = slog::Logger::root(slog::Discard, o!());

        let test_pids = [std::i32::MIN, -1, 0, 1, std::i32::MAX];

        for test_pid in test_pids {
            let mut s = Sandbox::new(&logger).unwrap();
            let (mut linux_container, _root) = create_linuxcontainer();

            let mut test_process = Process::new(
                &logger,
                &oci::Process::default(),
                "this_is_a_test_process",
                true,
                1,
            )
            .unwrap();
            // processes internally only have pids when manually set
            test_process.pid = test_pid;

            linux_container.processes.insert(test_pid, test_process);

            s.add_container(linux_container);

            let find_result = s.find_process(test_pid);

            // test first if it finds anything
            assert!(find_result.is_some(), "Should be able to find a process");

            let found_process = find_result.unwrap();

            // then test if it founds the correct process
            assert_eq!(
                found_process.pid, test_pid,
                "Should be able to find correct process"
            );
        }

        // to test for nonexistent pids, any pid that isn't the one set
        // above should work, as linuxcontainer starts with no processes
        let mut s = Sandbox::new(&logger).unwrap();

        let nonexistent_test_pid = 1234;

        let find_result = s.find_process(nonexistent_test_pid);

        assert!(
            find_result.is_none(),
            "Shouldn't find a process for non existent pid"
        );
    }

    #[tokio::test]
    async fn test_online_resources() {
        #[derive(Debug, Default)]
        struct TestFile {
            name: String,
            content: String,
        }

        #[derive(Debug, Default)]
        struct TestDirectory<'a> {
            name: String,
            files: &'a [TestFile],
        }

        #[derive(Debug)]
        struct TestData<'a> {
            directory_autogen_name: String,
            number_autogen_directories: u32,

            extra_directories: &'a [TestDirectory<'a>],
            pattern: String,
            to_enable: i32,

            result: Result<i32>,
        }

        impl Default for TestData<'_> {
            fn default() -> Self {
                TestData {
                    directory_autogen_name: Default::default(),
                    number_autogen_directories: Default::default(),
                    extra_directories: Default::default(),
                    pattern: Default::default(),
                    to_enable: Default::default(),
                    result: Ok(Default::default()),
                }
            }
        }

        let tests = &[
            // 4 well formed directories, request enabled 4,
            // correct result 4 enabled, should pass
            TestData {
                directory_autogen_name: String::from("cpu"),
                number_autogen_directories: 4,
                pattern: String::from(r"cpu[0-9]+"),
                to_enable: 4,
                result: Ok(4),
                ..Default::default()
            },
            // 0 well formed directories, request enabled 4,
            // correct result 0 enabled, should pass
            TestData {
                number_autogen_directories: 0,
                to_enable: 4,
                result: Ok(0),
                ..Default::default()
            },
            // 10 well formed directories, request enabled 4,
            // correct result 4 enabled, should pass
            TestData {
                directory_autogen_name: String::from("cpu"),
                number_autogen_directories: 10,
                pattern: String::from(r"cpu[0-9]+"),
                to_enable: 4,
                result: Ok(4),
                ..Default::default()
            },
            // 0 well formed directories, request enabled 0,
            // correct result 0 enabled, should pass
            TestData {
                number_autogen_directories: 0,
                pattern: String::from(r"cpu[0-9]+"),
                to_enable: 0,
                result: Ok(0),
                ..Default::default()
            },
            // 4 well formed directories, 1 malformed (no online file),
            // request enable 5, correct result 4
            TestData {
                directory_autogen_name: String::from("cpu"),
                number_autogen_directories: 4,
                pattern: String::from(r"cpu[0-9]+"),
                extra_directories: &[TestDirectory {
                    name: String::from("cpu4"),
                    files: &[],
                }],
                to_enable: 5,
                result: Ok(4),
            },
            // 3 malformed directories (no online files),
            // request enable 3, correct result 0
            TestData {
                pattern: String::from(r"cpu[0-9]+"),
                extra_directories: &[
                    TestDirectory {
                        name: String::from("cpu0"),
                        files: &[],
                    },
                    TestDirectory {
                        name: String::from("cpu1"),
                        files: &[],
                    },
                    TestDirectory {
                        name: String::from("cpu2"),
                        files: &[],
                    },
                ],
                to_enable: 3,
                result: Ok(0),
                ..Default::default()
            },
            // 1 malformed directories (online file with content "1"),
            // request enable 1, correct result 0
            TestData {
                pattern: String::from(r"cpu[0-9]+"),
                extra_directories: &[TestDirectory {
                    name: String::from("cpu0"),
                    files: &[TestFile {
                        name: SYSFS_ONLINE_FILE.to_string(),
                        content: String::from("1"),
                    }],
                }],
                to_enable: 1,
                result: Ok(0),
                ..Default::default()
            },
            // 2 well formed directories, 1 malformed (online file with content "1"),
            // request enable 3, correct result 2
            TestData {
                directory_autogen_name: String::from("cpu"),
                number_autogen_directories: 2,
                pattern: String::from(r"cpu[0-9]+"),
                extra_directories: &[TestDirectory {
                    name: String::from("cpu2"),
                    files: &[TestFile {
                        name: SYSFS_ONLINE_FILE.to_string(),
                        content: String::from("1"),
                    }],
                }],
                to_enable: 3,
                result: Ok(2),
            },
        ];

        let logger = slog::Logger::root(slog::Discard, o!());
        let tmpdir = Builder::new().tempdir().unwrap();
        let tmpdir_path = tmpdir.path().to_str().unwrap();

        for (i, d) in tests.iter().enumerate() {
            let current_test_dir_path = format!("{}/test_{}", tmpdir_path, i);
            fs::create_dir(&current_test_dir_path).unwrap();

            // create numbered directories and fill using root name
            for j in 0..d.number_autogen_directories {
                let subdir_path = format!(
                    "{}/{}{}",
                    current_test_dir_path, d.directory_autogen_name, j
                );
                let subfile_path = format!("{}/{}", subdir_path, SYSFS_ONLINE_FILE);
                fs::create_dir(&subdir_path).unwrap();
                let mut subfile = File::create(subfile_path).unwrap();
                subfile.write_all(b"0").unwrap();
            }
            // create extra directories and fill to specification
            for j in d.extra_directories {
                let subdir_path = format!("{}/{}", current_test_dir_path, j.name);
                fs::create_dir(&subdir_path).unwrap();
                for file in j.files {
                    let subfile_path = format!("{}/{}", subdir_path, file.name);
                    let mut subfile = File::create(&subfile_path).unwrap();
                    subfile.write_all(file.content.as_bytes()).unwrap();
                }
            }

            // run created directory structure against online_resources
            let result = online_resources(&logger, &current_test_dir_path, &d.pattern, d.to_enable);

            let mut msg = format!(
                "test[{}]: {:?}, expected {}, actual {}",
                i,
                d,
                d.result.is_ok(),
                result.is_ok()
            );

            assert_eq!(result.is_ok(), d.result.is_ok(), "{}", msg);

            if d.result.is_ok() {
                let test_result_val = *d.result.as_ref().ok().unwrap();
                let result_val = result.ok().unwrap();

                msg = format!(
                    "test[{}]: {:?}, expected {}, actual {}",
                    i, d, test_result_val, result_val
                );

                assert_eq!(test_result_val, result_val, "{}", msg);
            }
        }
    }
}
