// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

#![allow(clippy::undocumented_unsafe_blocks)]

use once_cell::sync::Lazy;
use serde_json::Value;
use ssh2::Session;
use std::env;
use std::ffi::OsStr;
use std::io;
use std::io::{Read, Write};
use std::net::TcpListener;
use std::net::TcpStream;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::path::Path;
use std::process::{Child, Command, ExitStatus, Output, Stdio};
use std::str::FromStr;
use std::sync::Mutex;
use std::thread;
use std::time::Duration;
use std::{fmt, fs};
use vmm_sys_util::tempdir::TempDir;
use wait_timeout::ChildExt;

#[derive(Debug)]
pub enum WaitTimeoutError {
    Timedout,
    ExitStatus,
    General(std::io::Error),
}

#[derive(Debug)]
pub enum Error {
    Parsing(std::num::ParseIntError),
    SshCommand(SshCommandError),
    WaitForBoot(WaitForBootError),
    EthrLogFile(std::io::Error),
    EthrLogParse,
    FioOutputParse,
    Iperf3Parse,
    Spawn(std::io::Error),
    WaitTimeout(WaitTimeoutError),
}

impl From<SshCommandError> for Error {
    fn from(e: SshCommandError) -> Self {
        Self::SshCommand(e)
    }
}

pub struct GuestNetworkConfig {
    pub guest_ip: String,
    pub l2_guest_ip1: String,
    pub l2_guest_ip2: String,
    pub l2_guest_ip3: String,
    pub host_ip: String,
    pub guest_mac: String,
    pub l2_guest_mac1: String,
    pub l2_guest_mac2: String,
    pub l2_guest_mac3: String,
    pub tcp_listener_port: u16,
}

pub const DEFAULT_TCP_LISTENER_MESSAGE: &str = "booted";
pub const DEFAULT_TCP_LISTENER_PORT: u16 = 8000;
pub const DEFAULT_TCP_LISTENER_TIMEOUT: i32 = 120;

#[derive(Debug)]
pub enum WaitForBootError {
    EpollWait(std::io::Error),
    Listen(std::io::Error),
    EpollWaitTimeout,
    WrongGuestAddr,
    Accept(std::io::Error),
}

impl GuestNetworkConfig {
    pub fn wait_vm_boot(&self, custom_timeout: Option<i32>) -> Result<(), WaitForBootError> {
        let start = std::time::Instant::now();
        // The 'port' is unique per 'GUEST' and listening to wild-card ip avoids retrying on 'TcpListener::bind()'
        let listen_addr = format!("0.0.0.0:{}", self.tcp_listener_port);
        let expected_guest_addr = self.guest_ip.as_str();
        let mut s = String::new();
        let timeout = match custom_timeout {
            Some(t) => t,
            None => DEFAULT_TCP_LISTENER_TIMEOUT,
        };

        match (|| -> Result<(), WaitForBootError> {
            let listener =
                TcpListener::bind(listen_addr.as_str()).map_err(WaitForBootError::Listen)?;
            listener
                .set_nonblocking(true)
                .expect("Cannot set non-blocking for tcp listener");

            // Reply on epoll w/ timeout to wait for guest connections faithfully
            let epoll_fd = epoll::create(true).expect("Cannot create epoll fd");
            // Use 'File' to enforce closing on 'epoll_fd'
            let _epoll_file = unsafe { fs::File::from_raw_fd(epoll_fd) };
            epoll::ctl(
                epoll_fd,
                epoll::ControlOptions::EPOLL_CTL_ADD,
                listener.as_raw_fd(),
                epoll::Event::new(epoll::Events::EPOLLIN, 0),
            )
            .expect("Cannot add 'tcp_listener' event to epoll");
            let mut events = vec![epoll::Event::new(epoll::Events::empty(), 0); 1];
            loop {
                let num_events = match epoll::wait(epoll_fd, timeout * 1000_i32, &mut events[..]) {
                    Ok(num_events) => Ok(num_events),
                    Err(e) => match e.raw_os_error() {
                        Some(libc::EAGAIN) | Some(libc::EINTR) => continue,
                        _ => Err(e),
                    },
                }
                .map_err(WaitForBootError::EpollWait)?;
                if num_events == 0 {
                    return Err(WaitForBootError::EpollWaitTimeout);
                }
                break;
            }

            match listener.accept() {
                Ok((_, addr)) => {
                    // Make sure the connection is from the expected 'guest_addr'
                    if addr.ip() != std::net::IpAddr::from_str(expected_guest_addr).unwrap() {
                        s = format!(
                            "Expecting the guest ip '{}' while being connected with ip '{}'",
                            expected_guest_addr,
                            addr.ip()
                        );
                        return Err(WaitForBootError::WrongGuestAddr);
                    }

                    Ok(())
                }
                Err(e) => {
                    s = "TcpListener::accept() failed".to_string();
                    Err(WaitForBootError::Accept(e))
                }
            }
        })() {
            Err(e) => {
                let duration = start.elapsed();
                eprintln!(
                    "\n\n==== Start 'wait_vm_boot' (FAILED) ====\n\n\
                 duration =\"{:?}, timeout = {}s\"\n\
                 listen_addr=\"{}\"\n\
                 expected_guest_addr=\"{}\"\n\
                 message=\"{}\"\n\
                 error=\"{:?}\"\n\
                 \n==== End 'wait_vm_boot' outout ====\n\n",
                    duration, timeout, listen_addr, expected_guest_addr, s, e
                );

                Err(e)
            }
            Ok(_) => Ok(()),
        }
    }
}

pub enum DiskType {
    OperatingSystem,
    CloudInit,
}

pub trait DiskConfig {
    fn prepare_files(&mut self, tmp_dir: &TempDir, network: &GuestNetworkConfig);
    fn prepare_cloudinit(&self, tmp_dir: &TempDir, network: &GuestNetworkConfig) -> String;
    fn disk(&self, disk_type: DiskType) -> Option<String>;
}

#[derive(Clone)]
pub struct UbuntuDiskConfig {
    osdisk_path: String,
    cloudinit_path: String,
    image_name: String,
}

impl UbuntuDiskConfig {
    pub fn new(image_name: String) -> Self {
        UbuntuDiskConfig {
            image_name,
            osdisk_path: String::new(),
            cloudinit_path: String::new(),
        }
    }
}

pub struct WindowsDiskConfig {
    image_name: String,
    osdisk_path: String,
    loopback_device: String,
    windows_snapshot_cow: String,
    windows_snapshot: String,
}

impl WindowsDiskConfig {
    pub fn new(image_name: String) -> Self {
        WindowsDiskConfig {
            image_name,
            osdisk_path: String::new(),
            loopback_device: String::new(),
            windows_snapshot_cow: String::new(),
            windows_snapshot: String::new(),
        }
    }
}

impl Drop for WindowsDiskConfig {
    fn drop(&mut self) {
        // dmsetup remove windows-snapshot-1
        std::process::Command::new("dmsetup")
            .arg("remove")
            .arg(self.windows_snapshot.as_str())
            .output()
            .expect("Expect removing Windows snapshot with 'dmsetup' to succeed");

        // dmsetup remove windows-snapshot-cow-1
        std::process::Command::new("dmsetup")
            .arg("remove")
            .arg(self.windows_snapshot_cow.as_str())
            .output()
            .expect("Expect removing Windows snapshot CoW with 'dmsetup' to succeed");

        // losetup -d <loopback_device>
        std::process::Command::new("losetup")
            .args(["-d", self.loopback_device.as_str()])
            .output()
            .expect("Expect removing loopback device to succeed");
    }
}

impl DiskConfig for UbuntuDiskConfig {
    fn prepare_cloudinit(&self, tmp_dir: &TempDir, network: &GuestNetworkConfig) -> String {
        let cloudinit_file_path =
            String::from(tmp_dir.as_path().join("cloudinit").to_str().unwrap());

        let cloud_init_directory = tmp_dir.as_path().join("cloud-init").join("ubuntu");

        fs::create_dir_all(&cloud_init_directory)
            .expect("Expect creating cloud-init directory to succeed");

        let source_file_dir = std::env::current_dir()
            .unwrap()
            .join("test_data")
            .join("cloud-init")
            .join("ubuntu");

        vec!["meta-data"].iter().for_each(|x| {
            rate_limited_copy(source_file_dir.join(x), cloud_init_directory.join(x))
                .expect("Expect copying cloud-init meta-data to succeed");
        });

        let mut user_data_string = String::new();
        fs::File::open(source_file_dir.join("user-data"))
            .unwrap()
            .read_to_string(&mut user_data_string)
            .expect("Expected reading user-data file in to succeed");
        user_data_string = user_data_string.replace(
            "@DEFAULT_TCP_LISTENER_MESSAGE",
            DEFAULT_TCP_LISTENER_MESSAGE,
        );
        user_data_string = user_data_string.replace("@HOST_IP", &network.host_ip);
        user_data_string =
            user_data_string.replace("@TCP_LISTENER_PORT", &network.tcp_listener_port.to_string());

        fs::File::create(cloud_init_directory.join("user-data"))
            .unwrap()
            .write_all(user_data_string.as_bytes())
            .expect("Expected writing out user-data to succeed");

        let mut network_config_string = String::new();

        fs::File::open(source_file_dir.join("network-config"))
            .unwrap()
            .read_to_string(&mut network_config_string)
            .expect("Expected reading network-config file in to succeed");

        network_config_string = network_config_string.replace("192.168.2.1", &network.host_ip);
        network_config_string = network_config_string.replace("192.168.2.2", &network.guest_ip);
        network_config_string = network_config_string.replace("192.168.2.3", &network.l2_guest_ip1);
        network_config_string = network_config_string.replace("192.168.2.4", &network.l2_guest_ip2);
        network_config_string = network_config_string.replace("192.168.2.5", &network.l2_guest_ip3);
        network_config_string =
            network_config_string.replace("12:34:56:78:90:ab", &network.guest_mac);
        network_config_string =
            network_config_string.replace("de:ad:be:ef:12:34", &network.l2_guest_mac1);
        network_config_string =
            network_config_string.replace("de:ad:be:ef:34:56", &network.l2_guest_mac2);
        network_config_string =
            network_config_string.replace("de:ad:be:ef:56:78", &network.l2_guest_mac3);

        fs::File::create(cloud_init_directory.join("network-config"))
            .unwrap()
            .write_all(network_config_string.as_bytes())
            .expect("Expected writing out network-config to succeed");

        std::process::Command::new("mkdosfs")
            .args(["-n", "cidata"])
            .args(["-C", cloudinit_file_path.as_str()])
            .arg("8192")
            .output()
            .expect("Expect creating disk image to succeed");

        vec!["user-data", "meta-data", "network-config"]
            .iter()
            .for_each(|x| {
                std::process::Command::new("mcopy")
                    .arg("-o")
                    .args(["-i", cloudinit_file_path.as_str()])
                    .args(["-s", cloud_init_directory.join(x).to_str().unwrap(), "::"])
                    .output()
                    .expect("Expect copying files to disk image to succeed");
            });

        cloudinit_file_path
    }

    fn prepare_files(&mut self, tmp_dir: &TempDir, network: &GuestNetworkConfig) {
        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let mut osdisk_base_path = workload_path;
        osdisk_base_path.push(&self.image_name);

        let osdisk_path = String::from(tmp_dir.as_path().join("osdisk.img").to_str().unwrap());
        let cloudinit_path = self.prepare_cloudinit(tmp_dir, network);

        rate_limited_copy(osdisk_base_path, &osdisk_path)
            .expect("copying of OS source disk image failed");

        self.cloudinit_path = cloudinit_path;
        self.osdisk_path = osdisk_path;
    }

    fn disk(&self, disk_type: DiskType) -> Option<String> {
        match disk_type {
            DiskType::OperatingSystem => Some(self.osdisk_path.clone()),
            DiskType::CloudInit => Some(self.cloudinit_path.clone()),
        }
    }
}

impl DiskConfig for WindowsDiskConfig {
    fn prepare_cloudinit(&self, _tmp_dir: &TempDir, _network: &GuestNetworkConfig) -> String {
        String::new()
    }

    fn prepare_files(&mut self, tmp_dir: &TempDir, _network: &GuestNetworkConfig) {
        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let mut osdisk_path = workload_path;
        osdisk_path.push(&self.image_name);

        let osdisk_blk_size = fs::metadata(osdisk_path)
            .expect("Expect retrieving Windows image metadata")
            .len()
            >> 9;

        let snapshot_cow_path =
            String::from(tmp_dir.as_path().join("snapshot_cow").to_str().unwrap());

        // Create and truncate CoW file for device mapper
        let cow_file_size: u64 = 1 << 30;
        let cow_file_blk_size = cow_file_size >> 9;
        let cow_file = std::fs::File::create(snapshot_cow_path.as_str())
            .expect("Expect creating CoW image to succeed");
        cow_file
            .set_len(cow_file_size)
            .expect("Expect truncating CoW image to succeed");

        // losetup --find --show /tmp/snapshot_cow
        let loopback_device = std::process::Command::new("losetup")
            .arg("--find")
            .arg("--show")
            .arg(snapshot_cow_path.as_str())
            .output()
            .expect("Expect creating loopback device from snapshot CoW image to succeed");

        self.loopback_device = String::from_utf8_lossy(&loopback_device.stdout)
            .trim()
            .to_string();

        let random_extension = tmp_dir.as_path().file_name().unwrap();
        let windows_snapshot_cow = format!(
            "windows-snapshot-cow-{}",
            random_extension.to_str().unwrap()
        );

        // dmsetup create windows-snapshot-cow-1 --table '0 2097152 linear /dev/loop1 0'
        std::process::Command::new("dmsetup")
            .arg("create")
            .arg(windows_snapshot_cow.as_str())
            .args([
                "--table",
                format!("0 {} linear {} 0", cow_file_blk_size, self.loopback_device).as_str(),
            ])
            .output()
            .expect("Expect creating Windows snapshot CoW with 'dmsetup' to succeed");

        let windows_snapshot = format!("windows-snapshot-{}", random_extension.to_str().unwrap());

        // dmsetup mknodes
        std::process::Command::new("dmsetup")
            .arg("mknodes")
            .output()
            .expect("Expect device mapper nodes to be ready");

        // dmsetup create windows-snapshot-1 --table '0 41943040 snapshot /dev/mapper/windows-base /dev/mapper/windows-snapshot-cow-1 P 8'
        std::process::Command::new("dmsetup")
            .arg("create")
            .arg(windows_snapshot.as_str())
            .args([
                "--table",
                format!(
                    "0 {} snapshot /dev/mapper/windows-base /dev/mapper/{} P 8",
                    osdisk_blk_size,
                    windows_snapshot_cow.as_str()
                )
                .as_str(),
            ])
            .output()
            .expect("Expect creating Windows snapshot with 'dmsetup' to succeed");

        // dmsetup mknodes
        std::process::Command::new("dmsetup")
            .arg("mknodes")
            .output()
            .expect("Expect device mapper nodes to be ready");

        self.osdisk_path = format!("/dev/mapper/{}", windows_snapshot);
        self.windows_snapshot_cow = windows_snapshot_cow;
        self.windows_snapshot = windows_snapshot;
    }

    fn disk(&self, disk_type: DiskType) -> Option<String> {
        match disk_type {
            DiskType::OperatingSystem => Some(self.osdisk_path.clone()),
            DiskType::CloudInit => None,
        }
    }
}

pub fn rate_limited_copy<P: AsRef<Path>, Q: AsRef<Path>>(from: P, to: Q) -> io::Result<u64> {
    for i in 0..10 {
        let free_bytes = unsafe {
            let mut stats = std::mem::MaybeUninit::zeroed();
            let fs_name = std::ffi::CString::new("/tmp").unwrap();
            libc::statvfs(fs_name.as_ptr(), stats.as_mut_ptr());

            let free_blocks = stats.assume_init().f_bfree;
            let block_size = stats.assume_init().f_bsize;

            free_blocks * block_size
        };

        // Make sure there is at least 6 GiB of space
        if free_bytes < 6 << 30 {
            eprintln!(
                "Not enough space on disk ({}). Attempt {} of 10. Sleeping.",
                free_bytes, i
            );
            thread::sleep(std::time::Duration::new(60, 0));
            continue;
        }

        match fs::copy(&from, &to) {
            Err(e) => {
                if let Some(errno) = e.raw_os_error() {
                    if errno == libc::ENOSPC {
                        eprintln!("Copy returned ENOSPC. Attempt {} of 10. Sleeping.", i);
                        thread::sleep(std::time::Duration::new(60, 0));
                        continue;
                    }
                }
                return Err(e);
            }
            Ok(i) => return Ok(i),
        }
    }
    Err(io::Error::last_os_error())
}

pub fn handle_child_output(
    r: Result<(), std::boxed::Box<dyn std::any::Any + std::marker::Send>>,
    output: &std::process::Output,
) {
    use std::os::unix::process::ExitStatusExt;
    if r.is_ok() && output.status.success() {
        return;
    }

    match output.status.code() {
        None => {
            // Don't treat child.kill() as a problem
            if output.status.signal() == Some(9) && r.is_ok() {
                return;
            }

            eprintln!(
                "==== child killed by signal: {} ====",
                output.status.signal().unwrap()
            );
        }
        Some(code) => {
            eprintln!("\n\n==== child exit code: {} ====", code);
        }
    }

    eprintln!(
        "\n\n==== Start child stdout ====\n\n{}\n\n==== End child stdout ====",
        String::from_utf8_lossy(&output.stdout)
    );
    eprintln!(
        "\n\n==== Start child stderr ====\n\n{}\n\n==== End child stderr ====",
        String::from_utf8_lossy(&output.stderr)
    );

    panic!("Test failed")
}

#[derive(Debug)]
pub struct PasswordAuth {
    pub username: String,
    pub password: String,
}

pub const DEFAULT_SSH_RETRIES: u8 = 6;
pub const DEFAULT_SSH_TIMEOUT: u8 = 10;

#[derive(Debug)]
pub enum SshCommandError {
    Connection(std::io::Error),
    Handshake(ssh2::Error),
    Authentication(ssh2::Error),
    ChannelSession(ssh2::Error),
    Command(ssh2::Error),
    ExitStatus(ssh2::Error),
    NonZeroExitStatus(i32),
    FileRead(std::io::Error),
    FileMetadata(std::io::Error),
    ScpSend(ssh2::Error),
    WriteAll(std::io::Error),
    SendEof(ssh2::Error),
    WaitEof(ssh2::Error),
}

fn scp_to_guest_with_auth(
    path: &Path,
    remote_path: &Path,
    auth: &PasswordAuth,
    ip: &str,
    retries: u8,
    timeout: u8,
) -> Result<(), SshCommandError> {
    let mut counter = 0;
    loop {
        match (|| -> Result<(), SshCommandError> {
            let tcp =
                TcpStream::connect(format!("{}:22", ip)).map_err(SshCommandError::Connection)?;
            let mut sess = Session::new().unwrap();
            sess.set_tcp_stream(tcp);
            sess.handshake().map_err(SshCommandError::Handshake)?;

            sess.userauth_password(&auth.username, &auth.password)
                .map_err(SshCommandError::Authentication)?;
            assert!(sess.authenticated());

            let content = fs::read(path).map_err(SshCommandError::FileRead)?;
            let mode = fs::metadata(path)
                .map_err(SshCommandError::FileMetadata)?
                .permissions()
                .mode()
                & 0o777;

            let mut channel = sess
                .scp_send(remote_path, mode as i32, content.len() as u64, None)
                .map_err(SshCommandError::ScpSend)?;
            channel
                .write_all(&content)
                .map_err(SshCommandError::WriteAll)?;
            channel.send_eof().map_err(SshCommandError::SendEof)?;
            channel.wait_eof().map_err(SshCommandError::WaitEof)?;

            // Intentionally ignore these results here as their failure
            // does not precipitate a repeat
            let _ = channel.close();
            let _ = channel.wait_close();

            Ok(())
        })() {
            Ok(_) => break,
            Err(e) => {
                counter += 1;
                if counter >= retries {
                    eprintln!(
                        "\n\n==== Start scp command output (FAILED) ====\n\n\
                         path =\"{:?}\"\n\
                         remote_path =\"{:?}\"\n\
                         auth=\"{:#?}\"\n\
                         ip=\"{}\"\n\
                         error=\"{:?}\"\n\
                         \n==== End scp command outout ====\n\n",
                        path, remote_path, auth, ip, e
                    );

                    return Err(e);
                }
            }
        };
        thread::sleep(std::time::Duration::new((timeout * counter).into(), 0));
    }
    Ok(())
}

pub fn scp_to_guest(
    path: &Path,
    remote_path: &Path,
    ip: &str,
    retries: u8,
    timeout: u8,
) -> Result<(), SshCommandError> {
    scp_to_guest_with_auth(
        path,
        remote_path,
        &PasswordAuth {
            username: String::from("cloud"),
            password: String::from("cloud123"),
        },
        ip,
        retries,
        timeout,
    )
}

pub fn ssh_command_ip_with_auth(
    command: &str,
    auth: &PasswordAuth,
    ip: &str,
    retries: u8,
    timeout: u8,
) -> Result<String, SshCommandError> {
    let mut s = String::new();

    let mut counter = 0;
    loop {
        match (|| -> Result<(), SshCommandError> {
            let tcp =
                TcpStream::connect(format!("{}:22", ip)).map_err(SshCommandError::Connection)?;
            let mut sess = Session::new().unwrap();
            sess.set_tcp_stream(tcp);
            sess.handshake().map_err(SshCommandError::Handshake)?;

            sess.userauth_password(&auth.username, &auth.password)
                .map_err(SshCommandError::Authentication)?;
            assert!(sess.authenticated());

            let mut channel = sess
                .channel_session()
                .map_err(SshCommandError::ChannelSession)?;
            channel.exec(command).map_err(SshCommandError::Command)?;

            // Intentionally ignore these results here as their failure
            // does not precipitate a repeat
            let _ = channel.read_to_string(&mut s);
            let _ = channel.close();
            let _ = channel.wait_close();

            let status = channel.exit_status().map_err(SshCommandError::ExitStatus)?;

            if status != 0 {
                Err(SshCommandError::NonZeroExitStatus(status))
            } else {
                Ok(())
            }
        })() {
            Ok(_) => break,
            Err(e) => {
                counter += 1;
                if counter >= retries {
                    eprintln!(
                        "\n\n==== Start ssh command output (FAILED) ====\n\n\
                         command=\"{}\"\n\
                         auth=\"{:#?}\"\n\
                         ip=\"{}\"\n\
                         output=\"{}\"\n\
                         error=\"{:?}\"\n\
                         \n==== End ssh command outout ====\n\n",
                        command, auth, ip, s, e
                    );

                    return Err(e);
                }
            }
        };
        thread::sleep(std::time::Duration::new((timeout * counter).into(), 0));
    }
    Ok(s)
}

pub fn ssh_command_ip(
    command: &str,
    ip: &str,
    retries: u8,
    timeout: u8,
) -> Result<String, SshCommandError> {
    ssh_command_ip_with_auth(
        command,
        &PasswordAuth {
            username: String::from("cloud"),
            password: String::from("cloud123"),
        },
        ip,
        retries,
        timeout,
    )
}

pub fn exec_host_command_status(command: &str) -> ExitStatus {
    std::process::Command::new("bash")
        .args(["-c", command])
        .status()
        .unwrap_or_else(|_| panic!("Expected '{}' to run", command))
}

pub fn exec_host_command_output(command: &str) -> Output {
    std::process::Command::new("bash")
        .args(["-c", command])
        .output()
        .unwrap_or_else(|_| panic!("Expected '{}' to run", command))
}

pub fn kill_child(child: &mut Child) {
    let r = unsafe { libc::kill(child.id() as i32, libc::SIGTERM) };
    if r != 0 {
        let e = io::Error::last_os_error();
        if e.raw_os_error().unwrap() == libc::ESRCH {
            return;
        }
        eprintln!("Failed to kill child with SIGTERM: {e:?}");
    }

    // The timeout period elapsed without the child exiting
    if child.wait_timeout(Duration::new(5, 0)).unwrap().is_none() {
        let _ = child.kill();
    }
}

pub const PIPE_SIZE: i32 = 32 << 20;

static NEXT_VM_ID: Lazy<Mutex<u8>> = Lazy::new(|| Mutex::new(1));

pub struct Guest {
    pub tmp_dir: TempDir,
    pub disk_config: Box<dyn DiskConfig>,
    pub network: GuestNetworkConfig,
}

// Safe to implement as we know we have no interior mutability
impl std::panic::RefUnwindSafe for Guest {}

impl Guest {
    pub fn new_from_ip_range(mut disk_config: Box<dyn DiskConfig>, class: &str, id: u8) -> Self {
        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();

        let network = GuestNetworkConfig {
            guest_ip: format!("{}.{}.2", class, id),
            l2_guest_ip1: format!("{}.{}.3", class, id),
            l2_guest_ip2: format!("{}.{}.4", class, id),
            l2_guest_ip3: format!("{}.{}.5", class, id),
            host_ip: format!("{}.{}.1", class, id),
            guest_mac: format!("12:34:56:78:90:{:02x}", id),
            l2_guest_mac1: format!("de:ad:be:ef:12:{:02x}", id),
            l2_guest_mac2: format!("de:ad:be:ef:34:{:02x}", id),
            l2_guest_mac3: format!("de:ad:be:ef:56:{:02x}", id),
            tcp_listener_port: DEFAULT_TCP_LISTENER_PORT + id as u16,
        };

        disk_config.prepare_files(&tmp_dir, &network);

        Guest {
            tmp_dir,
            disk_config,
            network,
        }
    }

    pub fn new(disk_config: Box<dyn DiskConfig>) -> Self {
        let mut guard = NEXT_VM_ID.lock().unwrap();
        let id = *guard;
        *guard = id + 1;

        Self::new_from_ip_range(disk_config, "192.168", id)
    }

    pub fn default_net_string(&self) -> String {
        format!(
            "tap=,mac={},ip={},mask=255.255.255.0",
            self.network.guest_mac, self.network.host_ip
        )
    }

    pub fn default_net_string_w_iommu(&self) -> String {
        format!(
            "tap=,mac={},ip={},mask=255.255.255.0,iommu=on",
            self.network.guest_mac, self.network.host_ip
        )
    }

    pub fn default_net_string_w_mtu(&self, mtu: u16) -> String {
        format!(
            "tap=,mac={},ip={},mask=255.255.255.0,mtu={}",
            self.network.guest_mac, self.network.host_ip, mtu
        )
    }

    pub fn ssh_command(&self, command: &str) -> Result<String, SshCommandError> {
        ssh_command_ip(
            command,
            &self.network.guest_ip,
            DEFAULT_SSH_RETRIES,
            DEFAULT_SSH_TIMEOUT,
        )
    }

    #[cfg(target_arch = "x86_64")]
    pub fn ssh_command_l1(&self, command: &str) -> Result<String, SshCommandError> {
        ssh_command_ip(
            command,
            &self.network.guest_ip,
            DEFAULT_SSH_RETRIES,
            DEFAULT_SSH_TIMEOUT,
        )
    }

    #[cfg(target_arch = "x86_64")]
    pub fn ssh_command_l2_1(&self, command: &str) -> Result<String, SshCommandError> {
        ssh_command_ip(
            command,
            &self.network.l2_guest_ip1,
            DEFAULT_SSH_RETRIES,
            DEFAULT_SSH_TIMEOUT,
        )
    }

    #[cfg(target_arch = "x86_64")]
    pub fn ssh_command_l2_2(&self, command: &str) -> Result<String, SshCommandError> {
        ssh_command_ip(
            command,
            &self.network.l2_guest_ip2,
            DEFAULT_SSH_RETRIES,
            DEFAULT_SSH_TIMEOUT,
        )
    }

    #[cfg(target_arch = "x86_64")]
    pub fn ssh_command_l2_3(&self, command: &str) -> Result<String, SshCommandError> {
        ssh_command_ip(
            command,
            &self.network.l2_guest_ip3,
            DEFAULT_SSH_RETRIES,
            DEFAULT_SSH_TIMEOUT,
        )
    }

    pub fn api_create_body(&self, cpu_count: u8, kernel_path: &str, kernel_cmd: &str) -> String {
        format! {"{{\"cpus\":{{\"boot_vcpus\":{},\"max_vcpus\":{}}},\"payload\":{{\"kernel\":\"{}\",\"cmdline\": \"{}\"}},\"net\":[{{\"ip\":\"{}\", \"mask\":\"255.255.255.0\", \"mac\":\"{}\"}}], \"disks\":[{{\"path\":\"{}\"}}, {{\"path\":\"{}\"}}]}}",
                 cpu_count,
                 cpu_count,
                 kernel_path,
                 kernel_cmd,
                 self.network.host_ip,
                 self.network.guest_mac,
                 self.disk_config.disk(DiskType::OperatingSystem).unwrap().as_str(),
                 self.disk_config.disk(DiskType::CloudInit).unwrap().as_str(),
        }
    }

    pub fn get_cpu_count(&self) -> Result<u32, Error> {
        self.ssh_command("grep -c processor /proc/cpuinfo")?
            .trim()
            .parse()
            .map_err(Error::Parsing)
    }

    #[cfg(target_arch = "x86_64")]
    pub fn get_initial_apicid(&self) -> Result<u32, Error> {
        self.ssh_command("grep \"initial apicid\" /proc/cpuinfo | grep -o \"[0-9]*\"")?
            .trim()
            .parse()
            .map_err(Error::Parsing)
    }

    pub fn get_total_memory(&self) -> Result<u32, Error> {
        self.ssh_command("grep MemTotal /proc/meminfo | grep -o \"[0-9]*\"")?
            .trim()
            .parse()
            .map_err(Error::Parsing)
    }

    #[cfg(target_arch = "x86_64")]
    pub fn get_total_memory_l2(&self) -> Result<u32, Error> {
        self.ssh_command_l2_1("grep MemTotal /proc/meminfo | grep -o \"[0-9]*\"")?
            .trim()
            .parse()
            .map_err(Error::Parsing)
    }

    pub fn get_numa_node_memory(&self, node_id: usize) -> Result<u32, Error> {
        self.ssh_command(
            format!(
                "grep MemTotal /sys/devices/system/node/node{}/meminfo \
                        | cut -d \":\" -f 2 | grep -o \"[0-9]*\"",
                node_id
            )
            .as_str(),
        )?
        .trim()
        .parse()
        .map_err(Error::Parsing)
    }

    pub fn wait_vm_boot(&self, custom_timeout: Option<i32>) -> Result<(), Error> {
        self.network
            .wait_vm_boot(custom_timeout)
            .map_err(Error::WaitForBoot)
    }

    pub fn check_numa_node_cpus(&self, node_id: usize, cpus: Vec<usize>) -> Result<(), Error> {
        for cpu in cpus.iter() {
            let cmd = format!(
                "[ -d \"/sys/devices/system/node/node{}/cpu{}\" ]",
                node_id, cpu
            );
            self.ssh_command(cmd.as_str())?;
        }

        Ok(())
    }

    pub fn check_numa_node_distances(
        &self,
        node_id: usize,
        distances: &str,
    ) -> Result<bool, Error> {
        let cmd = format!("cat /sys/devices/system/node/node{}/distance", node_id);
        if self.ssh_command(cmd.as_str())?.trim() == distances {
            Ok(true)
        } else {
            Ok(false)
        }
    }

    pub fn check_numa_common(
        &self,
        mem_ref: Option<&[u32]>,
        node_ref: Option<&[Vec<usize>]>,
        distance_ref: Option<&[&str]>,
    ) {
        if let Some(mem_ref) = mem_ref {
            // Check each NUMA node has been assigned the right amount of
            // memory.
            for (i, &m) in mem_ref.iter().enumerate() {
                assert!(self.get_numa_node_memory(i).unwrap_or_default() > m);
            }
        }

        if let Some(node_ref) = node_ref {
            // Check each NUMA node has been assigned the right CPUs set.
            for (i, n) in node_ref.iter().enumerate() {
                self.check_numa_node_cpus(i, n.clone()).unwrap();
            }
        }

        if let Some(distance_ref) = distance_ref {
            // Check each NUMA node has been assigned the right distances.
            for (i, &d) in distance_ref.iter().enumerate() {
                assert!(self.check_numa_node_distances(i, d).unwrap());
            }
        }
    }

    #[cfg(target_arch = "x86_64")]
    pub fn check_sgx_support(&self) -> Result<(), Error> {
        self.ssh_command(
            "cpuid -l 0x7 -s 0 | tr -s [:space:] | grep -q 'SGX: \
                    Software Guard Extensions supported = true'",
        )?;
        self.ssh_command(
            "cpuid -l 0x7 -s 0 | tr -s [:space:] | grep -q 'SGX_LC: \
                    SGX launch config supported = true'",
        )?;
        self.ssh_command(
            "cpuid -l 0x12 -s 0 | tr -s [:space:] | grep -q 'SGX1 \
                    supported = true'",
        )?;

        Ok(())
    }

    pub fn get_pci_bridge_class(&self) -> Result<String, Error> {
        Ok(self
            .ssh_command("cat /sys/bus/pci/devices/0000:00:00.0/class")?
            .trim()
            .to_string())
    }

    pub fn get_pci_device_ids(&self) -> Result<String, Error> {
        Ok(self
            .ssh_command("cat /sys/bus/pci/devices/*/device")?
            .trim()
            .to_string())
    }

    pub fn get_pci_vendor_ids(&self) -> Result<String, Error> {
        Ok(self
            .ssh_command("cat /sys/bus/pci/devices/*/vendor")?
            .trim()
            .to_string())
    }

    pub fn does_device_vendor_pair_match(
        &self,
        device_id: &str,
        vendor_id: &str,
    ) -> Result<bool, Error> {
        // We are checking if console device's device id and vendor id pair matches
        let devices = self.get_pci_device_ids()?;
        let devices: Vec<&str> = devices.split('\n').collect();
        let vendors = self.get_pci_vendor_ids()?;
        let vendors: Vec<&str> = vendors.split('\n').collect();

        for (index, d_id) in devices.iter().enumerate() {
            if *d_id == device_id {
                if let Some(v_id) = vendors.get(index) {
                    if *v_id == vendor_id {
                        return Ok(true);
                    }
                }
            }
        }

        Ok(false)
    }

    pub fn check_vsock(&self, socket: &str) {
        // Listen from guest on vsock CID=3 PORT=16
        // SOCKET-LISTEN:<domain>:<protocol>:<local-address>
        let guest_ip = self.network.guest_ip.clone();
        let listen_socat = thread::spawn(move || {
            ssh_command_ip("sudo socat - SOCKET-LISTEN:40:0:x00x00x10x00x00x00x03x00x00x00x00x00x00x00 > vsock_log", &guest_ip, DEFAULT_SSH_RETRIES, DEFAULT_SSH_TIMEOUT).unwrap();
        });

        // Make sure socat is listening, which might take a few second on slow systems
        thread::sleep(std::time::Duration::new(10, 0));

        // Write something to vsock from the host
        assert!(exec_host_command_status(&format!(
            "echo -e \"CONNECT 16\\nHelloWorld!\" | socat - UNIX-CONNECT:{}",
            socket
        ))
        .success());

        // Wait for the thread to terminate.
        listen_socat.join().unwrap();

        assert_eq!(
            self.ssh_command("cat vsock_log").unwrap().trim(),
            "HelloWorld!"
        );
    }

    #[cfg(target_arch = "x86_64")]
    pub fn check_nvidia_gpu(&self) {
        // Run CUDA sample to validate it can find the device
        let device_query_result = self
            .ssh_command("sudo /root/NVIDIA_CUDA-11.3_Samples/bin/x86_64/linux/release/deviceQuery")
            .unwrap();
        assert!(device_query_result.contains("Detected 1 CUDA Capable device"));
        assert!(device_query_result.contains("Device 0: \"NVIDIA Tesla T4\""));
        assert!(device_query_result.contains("Result = PASS"));

        // Run NVIDIA DCGM Diagnostics to validate the device is functional
        self.ssh_command("sudo nv-hostengine").unwrap();

        assert!(self
            .ssh_command("sudo dcgmi discovery -l")
            .unwrap()
            .contains("Name: NVIDIA Tesla T4"));
        assert_eq!(
            self.ssh_command("sudo dcgmi diag -r 'diagnostic' | grep Pass | wc -l")
                .unwrap()
                .trim(),
            "10"
        );
    }

    pub fn reboot_linux(&self, current_reboot_count: u32, custom_timeout: Option<i32>) {
        let list_boots_cmd = "sudo last | grep -c reboot";
        let boot_count = self
            .ssh_command(list_boots_cmd)
            .unwrap()
            .trim()
            .parse::<u32>()
            .unwrap_or_default();

        assert_eq!(boot_count, current_reboot_count + 1);
        self.ssh_command("sudo reboot").unwrap();

        self.wait_vm_boot(custom_timeout).unwrap();
        let boot_count = self
            .ssh_command(list_boots_cmd)
            .unwrap()
            .trim()
            .parse::<u32>()
            .unwrap_or_default();
        assert_eq!(boot_count, current_reboot_count + 2);
    }

    pub fn enable_memory_hotplug(&self) {
        self.ssh_command("echo online | sudo tee /sys/devices/system/memory/auto_online_blocks")
            .unwrap();
    }

    pub fn check_devices_common(
        &self,
        socket: Option<&String>,
        console_text: Option<&String>,
        pmem_path: Option<&String>,
    ) {
        // Check block devices are readable
        self.ssh_command("sudo dd if=/dev/vda of=/dev/null bs=1M iflag=direct count=1024")
            .unwrap();
        self.ssh_command("sudo dd if=/dev/vdb of=/dev/null bs=1M iflag=direct count=8")
            .unwrap();
        // Check if the rng device is readable
        self.ssh_command("sudo head -c 1000 /dev/hwrng > /dev/null")
            .unwrap();
        // Check vsock
        if let Some(socket) = socket {
            self.check_vsock(socket.as_str());
        }
        // Check if the console is usable
        if let Some(console_text) = console_text {
            let console_cmd = format!("echo {} | sudo tee /dev/hvc0", console_text);
            self.ssh_command(&console_cmd).unwrap();
        }
        // The net device is 'automatically' exercised through the above 'ssh' commands

        // Check if the pmem device is usable
        if let Some(pmem_path) = pmem_path {
            assert_eq!(
                self.ssh_command(&format!("ls {}", pmem_path))
                    .unwrap()
                    .trim(),
                pmem_path
            );
            assert_eq!(
                self.ssh_command(&format!("sudo mount {} /mnt", pmem_path))
                    .unwrap(),
                ""
            );
            assert_eq!(self.ssh_command("ls /mnt").unwrap(), "lost+found\n");
            self.ssh_command("echo test123 | sudo tee /mnt/test")
                .unwrap();
            assert_eq!(self.ssh_command("sudo umount /mnt").unwrap(), "");
            assert_eq!(self.ssh_command("ls /mnt").unwrap(), "");

            assert_eq!(
                self.ssh_command(&format!("sudo mount {} /mnt", pmem_path))
                    .unwrap(),
                ""
            );
            assert_eq!(
                self.ssh_command("sudo cat /mnt/test || true")
                    .unwrap()
                    .trim(),
                "test123"
            );
            self.ssh_command("sudo rm /mnt/test").unwrap();
            assert_eq!(self.ssh_command("sudo umount /mnt").unwrap(), "");
        }
    }
}

pub enum VerbosityLevel {
    Warn,
    Info,
    Debug,
}

impl Default for VerbosityLevel {
    fn default() -> Self {
        Self::Warn
    }
}

impl ToString for VerbosityLevel {
    fn to_string(&self) -> String {
        use VerbosityLevel::*;
        match self {
            Warn => "".to_string(),
            Info => "-v".to_string(),
            Debug => "-vv".to_string(),
        }
    }
}

pub struct GuestCommand<'a> {
    command: Command,
    guest: &'a Guest,
    capture_output: bool,
    print_cmd: bool,
    verbosity: VerbosityLevel,
}

impl<'a> GuestCommand<'a> {
    pub fn new(guest: &'a Guest) -> Self {
        Self::new_with_binary_path(guest, &clh_command("cube-hypervisor"))
    }

    pub fn new_with_binary_path(guest: &'a Guest, binary_path: &str) -> Self {
        Self {
            command: Command::new(binary_path),
            guest,
            capture_output: false,
            print_cmd: true,
            verbosity: VerbosityLevel::Info,
        }
    }

    pub fn verbosity(&mut self, verbosity: VerbosityLevel) -> &mut Self {
        self.verbosity = verbosity;
        self
    }

    pub fn capture_output(&mut self) -> &mut Self {
        self.capture_output = true;
        self
    }

    pub fn set_print_cmd(&mut self, print_cmd: bool) -> &mut Self {
        self.print_cmd = print_cmd;
        self
    }

    pub fn spawn(&mut self) -> io::Result<Child> {
        use VerbosityLevel::*;
        match &self.verbosity {
            Warn => {}
            Info => {
                self.command.arg("-v");
            }
            Debug => {
                self.command.arg("-vv");
            }
        };

        if self.print_cmd {
            println!(
                "\n\n==== Start cube-hypervisor command-line ====\n\n\
                     {:?}\n\
                     \n==== End cube-hypervisor command-line ====\n\n",
                self.command
            );
        }

        if self.capture_output {
            let child = self
                .command
                .stderr(Stdio::piped())
                .stdout(Stdio::piped())
                .spawn()
                .unwrap();

            let fd = child.stdout.as_ref().unwrap().as_raw_fd();
            let pipesize = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };
            if pipesize == -1 {
                return Err(io::Error::last_os_error());
            }
            let fd = child.stderr.as_ref().unwrap().as_raw_fd();
            let pipesize1 = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };
            if pipesize1 == -1 {
                return Err(io::Error::last_os_error());
            }

            if pipesize >= PIPE_SIZE && pipesize1 >= PIPE_SIZE {
                Ok(child)
            } else {
                Err(std::io::Error::new(
                    std::io::ErrorKind::Other,
                    format!(
                        "resizing pipe w/ 'fnctl' failed: stdout pipesize {}, stderr pipesize {}",
                        pipesize, pipesize1
                    ),
                ))
            }
        } else {
            self.command.spawn()
        }
    }

    pub fn args<I, S>(&mut self, args: I) -> &mut Self
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        self.command.args(args);
        self
    }

    pub fn default_disks(&mut self) -> &mut Self {
        if self.guest.disk_config.disk(DiskType::CloudInit).is_some() {
            self.args([
                "--disk",
                format!(
                    "path={}",
                    self.guest
                        .disk_config
                        .disk(DiskType::OperatingSystem)
                        .unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    self.guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
            ])
        } else {
            self.args([
                "--disk",
                format!(
                    "path={}",
                    self.guest
                        .disk_config
                        .disk(DiskType::OperatingSystem)
                        .unwrap()
                )
                .as_str(),
            ])
        }
    }

    pub fn default_net(&mut self) -> &mut Self {
        self.args(["--net", self.guest.default_net_string().as_str()])
    }
}

pub fn clh_command(cmd: &str) -> String {
    env::var("BUILD_TARGET").map_or(
        format!("target/x86_64-unknown-linux-gnu/release/{}", cmd),
        |target| format!("target/{}/release/{}", target, cmd),
    )
}

pub fn parse_iperf3_output(output: &[u8], sender: bool) -> Result<f64, Error> {
    std::panic::catch_unwind(|| {
        let s = String::from_utf8_lossy(output);
        let v: Value = serde_json::from_str(&s).expect("'iperf3' parse error: invalid json output");

        let bps: f64 = if sender {
            v["end"]["sum_sent"]["bits_per_second"]
                .as_f64()
                .expect("'iperf3' parse error: missing entry 'end.sum_sent.bits_per_second'")
        } else {
            v["end"]["sum_received"]["bits_per_second"]
                .as_f64()
                .expect("'iperf3' parse error: missing entry 'end.sum_received.bits_per_second'")
        };

        bps
    })
    .map_err(|_| {
        eprintln!(
            "=============== iperf3 output ===============\n\n{}\n\n===========end============\n\n",
            String::from_utf8_lossy(output)
        );
        Error::Iperf3Parse
    })
}

pub enum FioOps {
    Read,
    RandomRead,
    Write,
    RandomWrite,
    ReadWrite,
    RandRW,
}

impl fmt::Display for FioOps {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            FioOps::Read => write!(f, "read"),
            FioOps::RandomRead => write!(f, "randread"),
            FioOps::Write => write!(f, "write"),
            FioOps::RandomWrite => write!(f, "randwrite"),
            FioOps::ReadWrite => write!(f, "rw"),
            FioOps::RandRW => write!(f, "randrw"),
        }
    }
}

pub fn parse_fio_output(output: &str, fio_ops: &FioOps, num_jobs: u32) -> Result<f64, Error> {
    std::panic::catch_unwind(|| {
        let v: Value =
            serde_json::from_str(output).expect("'fio' parse error: invalid json output");
        let jobs = v["jobs"]
            .as_array()
            .expect("'fio' parse error: missing entry 'jobs'");
        assert_eq!(
            jobs.len(),
            num_jobs as usize,
            "'fio' parse error: Unexpected number of 'fio' jobs."
        );

        let (read, write) = match fio_ops {
            FioOps::Read | FioOps::RandomRead => (true, false),
            FioOps::Write | FioOps::RandomWrite => (false, true),
            FioOps::ReadWrite | FioOps::RandRW => (true, true),
        };

        let mut total_bps = 0_f64;
        for j in jobs {
            if read {
                let bytes = j["read"]["io_bytes"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'read.io_bytes'");
                let runtime = j["read"]["runtime"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'read.runtime'")
                    as f64
                    / 1000_f64;
                total_bps += bytes as f64 / runtime;
            }
            if write {
                let bytes = j["write"]["io_bytes"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'write.io_bytes'");
                let runtime = j["write"]["runtime"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'write.runtime'")
                    as f64
                    / 1000_f64;
                total_bps += bytes as f64 / runtime;
            }
        }

        total_bps
    })
    .map_err(|_| {
        eprintln!(
            "=============== Fio output ===============\n\n{}\n\n===========end============\n\n",
            output
        );
        Error::FioOutputParse
    })
}

pub fn parse_fio_output_iops(output: &str, fio_ops: &FioOps, num_jobs: u32) -> Result<f64, Error> {
    std::panic::catch_unwind(|| {
        let v: Value =
            serde_json::from_str(output).expect("'fio' parse error: invalid json output");
        let jobs = v["jobs"]
            .as_array()
            .expect("'fio' parse error: missing entry 'jobs'");
        assert_eq!(
            jobs.len(),
            num_jobs as usize,
            "'fio' parse error: Unexpected number of 'fio' jobs."
        );

        let (read, write) = match fio_ops {
            FioOps::Read | FioOps::RandomRead => (true, false),
            FioOps::Write | FioOps::RandomWrite => (false, true),
            FioOps::ReadWrite | FioOps::RandRW => (true, true),
        };

        let mut total_iops = 0_f64;
        for j in jobs {
            if read {
                let ios = j["read"]["total_ios"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'read.total_ios'");
                let runtime = j["read"]["runtime"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'read.runtime'")
                    as f64
                    / 1000_f64;
                total_iops += ios as f64 / runtime;
            }
            if write {
                let ios = j["write"]["total_ios"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'write.total_ios'");
                let runtime = j["write"]["runtime"]
                    .as_u64()
                    .expect("'fio' parse error: missing entry 'write.runtime'")
                    as f64
                    / 1000_f64;
                total_iops += ios as f64 / runtime;
            }
        }

        total_iops
    })
    .map_err(|_| {
        eprintln!(
            "=============== Fio output ===============\n\n{}\n\n===========end============\n\n",
            output
        );
        Error::FioOutputParse
    })
}

// Wait the child process for a given timeout
fn child_wait_timeout(child: &mut Child, timeout: u64) -> Result<(), WaitTimeoutError> {
    match child.wait_timeout(Duration::from_secs(timeout)) {
        Err(e) => {
            return Err(WaitTimeoutError::General(e));
        }
        Ok(s) => match s {
            None => {
                return Err(WaitTimeoutError::Timedout);
            }
            Some(s) => {
                if !s.success() {
                    return Err(WaitTimeoutError::ExitStatus);
                }
            }
        },
    }

    Ok(())
}

pub fn measure_virtio_net_throughput(
    test_timeout: u32,
    queue_pairs: u32,
    guest: &Guest,
    receive: bool,
) -> Result<f64, Error> {
    let default_port = 5201;

    // 1. start the iperf3 server on the guest
    for n in 0..queue_pairs {
        guest.ssh_command(&format!("iperf3 -s -p {} -D", default_port + n))?;
    }

    thread::sleep(Duration::new(1, 0));

    // 2. start the iperf3 client on host to measure RX through-put
    let mut clients = Vec::new();
    for n in 0..queue_pairs {
        let mut cmd = Command::new("iperf3");
        cmd.args([
            "-J", // Output in JSON format
            "-c",
            &guest.network.guest_ip,
            "-p",
            &format!("{}", default_port + n),
            "-t",
            &format!("{}", test_timeout),
        ]);
        // For measuring the guest transmit throughput (as a sender),
        // use reverse mode of the iperf3 client on the host
        if !receive {
            cmd.args(["-R"]);
        }
        let client = cmd
            .stderr(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .map_err(Error::Spawn)?;

        clients.push(client);
    }

    let mut err: Option<Error> = None;
    let mut bps = Vec::new();
    let mut failed = false;
    for c in clients {
        let mut c = c;
        if let Err(e) = child_wait_timeout(&mut c, test_timeout as u64 + 5) {
            err = Some(Error::WaitTimeout(e));
            failed = true;
        }

        if !failed {
            // Safe to unwrap as we know the child has terminated succesffully
            let output = c.wait_with_output().unwrap();
            bps.push(parse_iperf3_output(&output.stdout, receive)?);
        } else {
            let _ = c.kill();
            let output = c.wait_with_output().unwrap();
            println!(
                "=============== Client output [Error] ===============\n\n{}\n\n===========end============\n\n",
                String::from_utf8_lossy(&output.stdout)
            );
        }
    }

    if let Some(e) = err {
        Err(e)
    } else {
        Ok(bps.iter().sum())
    }
}

pub fn parse_ethr_latency_output(output: &[u8]) -> Result<Vec<f64>, Error> {
    std::panic::catch_unwind(|| {
        let s = String::from_utf8_lossy(output);
        let mut latency = Vec::new();
        for l in s.lines() {
            let v: Value = serde_json::from_str(l).expect("'ethr' parse error: invalid json line");
            // Skip header/summary lines
            if let Some(avg) = v["Avg"].as_str() {
                // Assume the latency unit is always "us"
                latency.push(
                    avg.split("us").collect::<Vec<&str>>()[0]
                        .parse::<f64>()
                        .expect("'ethr' parse error: invalid 'Avg' entry"),
                );
            }
        }

        assert!(
            !latency.is_empty(),
            "'ethr' parse error: no valid latency data found"
        );

        latency
    })
    .map_err(|_| {
        eprintln!(
            "=============== ethr output ===============\n\n{}\n\n===========end============\n\n",
            String::from_utf8_lossy(output)
        );
        Error::EthrLogParse
    })
}

pub fn measure_virtio_net_latency(guest: &Guest, test_timeout: u32) -> Result<Vec<f64>, Error> {
    // copy the 'ethr' tool to the guest image
    let ethr_path = "/usr/local/bin/ethr";
    let ethr_remote_path = "/tmp/ethr";
    scp_to_guest(
        Path::new(ethr_path),
        Path::new(ethr_remote_path),
        &guest.network.guest_ip,
        //DEFAULT_SSH_RETRIES,
        1,
        DEFAULT_SSH_TIMEOUT,
    )?;

    // Start the ethr server on the guest
    guest.ssh_command(&format!("{} -s &> /dev/null &", ethr_remote_path))?;

    thread::sleep(Duration::new(10, 0));

    // Start the ethr client on the host
    let log_file = guest
        .tmp_dir
        .as_path()
        .join("ethr.client.log")
        .to_str()
        .unwrap()
        .to_string();
    let mut c = Command::new(ethr_path)
        .args([
            "-c",
            &guest.network.guest_ip,
            "-t",
            "l",
            "-o",
            &log_file, // file output is JSON format
            "-d",
            &format!("{}s", test_timeout),
        ])
        .stderr(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .map_err(Error::Spawn)?;

    if let Err(e) = child_wait_timeout(&mut c, test_timeout as u64 + 5).map_err(Error::WaitTimeout)
    {
        let _ = c.kill();
        return Err(e);
    }

    // Parse the ethr latency test output
    let content = fs::read(log_file).map_err(Error::EthrLogFile)?;
    parse_ethr_latency_output(&content)
}
