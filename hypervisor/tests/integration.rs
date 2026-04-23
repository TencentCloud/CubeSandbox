// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

// When enabling the `mshv` feature, we skip quite some tests and
// hence have known dead-code. This annotation silences dead-code
// related warnings for our quality workflow to pass.
#![allow(dead_code)]

extern crate test_infra;

use net_util::MacAddr;
use std::collections::HashMap;
use std::fs;
use std::io;
use std::io::BufRead;
use std::io::Read;
use std::io::Seek;
use std::io::Write;
use std::os::unix::io::AsRawFd;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::string::String;
use std::sync::mpsc;
use std::sync::mpsc::Receiver;
use std::sync::Mutex;
use std::thread;
use test_infra::*;
use vmm::config::RestoreConfig;
use vmm::vm_config::{DiskConfig, FsConfig, NetConfig, PmemConfig, VsockConfig};
use vmm_sys_util::{tempdir::TempDir, tempfile::TempFile};
use wait_timeout::ChildExt;

#[cfg(target_arch = "x86_64")]
mod x86_64 {
    pub const BIONIC_IMAGE_NAME: &str = "bionic-server-cloudimg-amd64.raw";
    pub const FOCAL_IMAGE_NAME: &str = "focal-server-cloudimg-amd64-custom-20210609-0.raw";
    pub const FOCAL_SGX_IMAGE_NAME: &str = "focal-server-cloudimg-amd64-sgx.raw";
    pub const HIRSUTE_NVIDIA_IMAGE_NAME: &str = "hirsute-server-cloudimg-amd64-nvidia.raw";
    pub const FOCAL_IMAGE_NAME_QCOW2: &str = "focal-server-cloudimg-amd64-custom-20210609-0.qcow2";
    pub const FOCAL_IMAGE_NAME_VHD: &str = "focal-server-cloudimg-amd64-custom-20210609-0.vhd";
    pub const FOCAL_IMAGE_NAME_VHDX: &str = "focal-server-cloudimg-amd64-custom-20210609-0.vhdx";
    pub const JAMMY_IMAGE_NAME: &str = "jammy-server-cloudimg-amd64-custom-20220329-0.raw";
    pub const WINDOWS_IMAGE_NAME: &str = "windows-server-2019.raw";
    pub const OVMF_NAME: &str = "CLOUDHV.fd";
    pub const GREP_SERIAL_IRQ_CMD: &str = "grep -c 'IO-APIC.*ttyS0' /proc/interrupts || true";
}

#[cfg(target_arch = "x86_64")]
use x86_64::*;

#[cfg(target_arch = "aarch64")]
mod aarch64 {
    pub const BIONIC_IMAGE_NAME: &str = "bionic-server-cloudimg-arm64.raw";
    pub const FOCAL_IMAGE_NAME: &str = "focal-server-cloudimg-arm64-custom-20210929-0.raw";
    pub const FOCAL_IMAGE_UPDATE_KERNEL_NAME: &str =
        "focal-server-cloudimg-arm64-custom-20210929-0-update-kernel.raw";
    pub const FOCAL_IMAGE_NAME_QCOW2: &str = "focal-server-cloudimg-arm64-custom-20210929-0.qcow2";
    pub const FOCAL_IMAGE_NAME_VHD: &str = "focal-server-cloudimg-arm64-custom-20210929-0.vhd";
    pub const FOCAL_IMAGE_NAME_VHDX: &str = "focal-server-cloudimg-arm64-custom-20210929-0.vhdx";
    pub const JAMMY_IMAGE_NAME: &str = "jammy-server-cloudimg-arm64-custom-20220329-0.raw";
    pub const WINDOWS_IMAGE_NAME: &str = "windows-11-iot-enterprise-aarch64.raw";
    pub const OVMF_NAME: &str = "CLOUDHV_EFI.fd";
    pub const GREP_SERIAL_IRQ_CMD: &str = "grep -c 'GICv3.*uart-pl011' /proc/interrupts || true";
    pub const GREP_PMU_IRQ_CMD: &str = "grep -c 'GICv3.*arm-pmu' /proc/interrupts || true";
}

#[cfg(target_arch = "aarch64")]
use aarch64::*;

const DIRECT_KERNEL_BOOT_CMDLINE: &str =
    "root=/dev/vda1 console=hvc0 rw systemd.journald.forward_to_console=1";

const CONSOLE_TEST_STRING: &str = "Started OpenBSD Secure Shell server";

fn prepare_virtiofsd(tmp_dir: &TempDir, shared_dir: &str) -> (std::process::Child, String) {
    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");

    let mut virtiofsd_path = workload_path;
    virtiofsd_path.push("virtiofsd");
    let virtiofsd_path = String::from(virtiofsd_path.to_str().unwrap());

    let virtiofsd_socket_path =
        String::from(tmp_dir.as_path().join("virtiofs.sock").to_str().unwrap());

    // Start the daemon
    let child = Command::new(virtiofsd_path.as_str())
        .args(["--shared-dir", shared_dir])
        .args(["--socket-path", virtiofsd_socket_path.as_str()])
        .args(["--cache", "never"])
        .spawn()
        .unwrap();

    thread::sleep(std::time::Duration::new(10, 0));

    (child, virtiofsd_socket_path)
}

fn prepare_vubd(
    tmp_dir: &TempDir,
    blk_img: &str,
    num_queues: usize,
    rdonly: bool,
    direct: bool,
) -> (std::process::Child, String) {
    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");

    let mut blk_file_path = workload_path;
    blk_file_path.push(blk_img);
    let blk_file_path = String::from(blk_file_path.to_str().unwrap());

    let vubd_socket_path = String::from(tmp_dir.as_path().join("vub.sock").to_str().unwrap());

    // Start the daemon
    let child = Command::new(clh_command("vhost_user_block"))
        .args([
            "--block-backend",
            format!(
                "path={},socket={},num_queues={},readonly={},direct={}",
                blk_file_path, vubd_socket_path, num_queues, rdonly, direct
            )
            .as_str(),
        ])
        .spawn()
        .unwrap();

    thread::sleep(std::time::Duration::new(10, 0));

    (child, vubd_socket_path)
}

fn temp_vsock_path(tmp_dir: &TempDir) -> String {
    String::from(tmp_dir.as_path().join("vsock").to_str().unwrap())
}

fn temp_api_path(tmp_dir: &TempDir) -> String {
    String::from(
        tmp_dir
            .as_path()
            .join("cloud-hypervisor.sock")
            .to_str()
            .unwrap(),
    )
}

fn temp_event_monitor_path(tmp_dir: &TempDir) -> String {
    String::from(tmp_dir.as_path().join("event.json").to_str().unwrap())
}

// Creates the directory and returns the path.
fn temp_snapshot_dir_path(tmp_dir: &TempDir) -> String {
    let snapshot_dir = String::from(tmp_dir.as_path().join("snapshot").to_str().unwrap());
    std::fs::create_dir(&snapshot_dir).unwrap();
    snapshot_dir
}

fn temp_vmcore_file_path(tmp_dir: &TempDir) -> String {
    let vmcore_file = String::from(tmp_dir.as_path().join("vmcore").to_str().unwrap());
    vmcore_file
}

// Creates the path for direct kernel boot and return the path.
// For x86_64, this function returns the vmlinux kernel path.
// For AArch64, this function returns the PE kernel path.
fn direct_kernel_boot_path() -> PathBuf {
    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");

    let mut kernel_path = workload_path;
    #[cfg(target_arch = "x86_64")]
    kernel_path.push("vmlinux");
    #[cfg(target_arch = "aarch64")]
    kernel_path.push("Image");

    kernel_path
}

fn edk2_path() -> PathBuf {
    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");
    let mut edk2_path = workload_path;
    edk2_path.push(OVMF_NAME);

    edk2_path
}

fn cloud_hypervisor_release_path() -> String {
    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");

    let mut ch_release_path = workload_path;
    #[cfg(target_arch = "x86_64")]
    ch_release_path.push("cloud-hypervisor-static");
    #[cfg(target_arch = "aarch64")]
    ch_release_path.push("cloud-hypervisor-static-aarch64");

    ch_release_path.into_os_string().into_string().unwrap()
}

fn prepare_vhost_user_net_daemon(
    tmp_dir: &TempDir,
    ip: &str,
    tap: Option<&str>,
    mtu: Option<u16>,
    num_queues: usize,
    client_mode: bool,
) -> (std::process::Command, String) {
    let vunet_socket_path = String::from(tmp_dir.as_path().join("vunet.sock").to_str().unwrap());

    // Start the daemon
    let mut net_params = format!(
        "ip={},mask=255.255.255.0,socket={},num_queues={},queue_size=1024,client={}",
        ip, vunet_socket_path, num_queues, client_mode
    );

    if let Some(tap) = tap {
        net_params.push_str(format!(",tap={}", tap).as_str());
    }

    if let Some(mtu) = mtu {
        net_params.push_str(format!(",mtu={}", mtu).as_str());
    }

    let mut command = Command::new(clh_command("vhost_user_net"));
    command.args(["--net-backend", net_params.as_str()]);

    (command, vunet_socket_path)
}

fn curl_command(api_socket: &str, method: &str, url: &str, http_body: Option<&str>) {
    let mut curl_args: Vec<&str> = ["--unix-socket", api_socket, "-i", "-X", method, url].to_vec();

    if let Some(body) = http_body {
        curl_args.push("-H");
        curl_args.push("Accept: application/json");
        curl_args.push("-H");
        curl_args.push("Content-Type: application/json");
        curl_args.push("-d");
        curl_args.push(body);
    }

    let status = Command::new("curl")
        .args(curl_args)
        .status()
        .expect("Failed to launch curl command");

    assert!(status.success());
}

fn remote_command(api_socket: &str, command: &str, arg: Option<&str>) -> bool {
    let mut cmd = Command::new(clh_command("ch-remote"));
    cmd.args([&format!("--api-socket={}", api_socket), command]);

    if let Some(arg) = arg {
        cmd.arg(arg);
    }
    let output = cmd.output().unwrap();
    if output.status.success() {
        true
    } else {
        eprintln!("Error running ch-remote command: {:?}", &cmd);
        let stderr = String::from_utf8_lossy(&output.stderr);
        eprintln!("stderr: {}", stderr);
        false
    }
}

fn remote_command_w_output(api_socket: &str, command: &str, arg: Option<&str>) -> (bool, Vec<u8>) {
    let mut cmd = Command::new(clh_command("ch-remote"));
    cmd.args([&format!("--api-socket={}", api_socket), command]);

    if let Some(arg) = arg {
        cmd.arg(arg);
    }

    let output = cmd.output().expect("Failed to launch ch-remote");

    (output.status.success(), output.stdout)
}

fn resize_command(
    api_socket: &str,
    desired_vcpus: Option<u8>,
    desired_ram: Option<usize>,
    desired_balloon: Option<usize>,
    event_file: Option<&str>,
) -> bool {
    let mut cmd = Command::new(clh_command("ch-remote"));
    cmd.args([&format!("--api-socket={}", api_socket), "resize"]);

    if let Some(desired_vcpus) = desired_vcpus {
        cmd.arg(format!("--cpus={}", desired_vcpus));
    }

    if let Some(desired_ram) = desired_ram {
        cmd.arg(format!("--memory={}", desired_ram));
    }

    if let Some(desired_balloon) = desired_balloon {
        cmd.arg(format!("--balloon={}", desired_balloon));
    }

    let ret = cmd.status().expect("Failed to launch ch-remote").success();

    if let Some(event_path) = event_file {
        let latest_events = [
            &MetaEvent {
                event: "resizing".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "resized".to_string(),
                device_id: None,
            },
        ];
        assert!(check_latest_events_exact(&latest_events, event_path));
    }

    ret
}

fn resize_zone_command(api_socket: &str, id: &str, desired_size: &str) -> bool {
    let mut cmd = Command::new(clh_command("ch-remote"));
    cmd.args([
        &format!("--api-socket={}", api_socket),
        "resize-zone",
        &format!("--id={}", id),
        &format!("--size={}", desired_size),
    ]);

    cmd.status().expect("Failed to launch ch-remote").success()
}

// setup OVS-DPDK bridge and ports
fn setup_ovs_dpdk() {
    // setup OVS-DPDK
    assert!(exec_host_command_status("service openvswitch-switch start").success());
    assert!(exec_host_command_status("ovs-vsctl init").success());
    assert!(
        exec_host_command_status("ovs-vsctl set Open_vSwitch . other_config:dpdk-init=true")
            .success()
    );
    assert!(exec_host_command_status("service openvswitch-switch restart").success());

    // Create OVS-DPDK bridge and ports
    assert!(exec_host_command_status(
        "ovs-vsctl add-br ovsbr0 -- set bridge ovsbr0 datapath_type=netdev",
    )
    .success());
    assert!(exec_host_command_status("ovs-vsctl add-port ovsbr0 vhost-user1 -- set Interface vhost-user1 type=dpdkvhostuserclient options:vhost-server-path=/tmp/dpdkvhostclient1").success());
    assert!(exec_host_command_status("ovs-vsctl add-port ovsbr0 vhost-user2 -- set Interface vhost-user2 type=dpdkvhostuserclient options:vhost-server-path=/tmp/dpdkvhostclient2").success());
    assert!(exec_host_command_status("ip link set up dev ovsbr0").success());
    assert!(exec_host_command_status("service openvswitch-switch restart").success());
}
fn cleanup_ovs_dpdk() {
    assert!(exec_host_command_status("ovs-vsctl del-br ovsbr0").success());
    exec_host_command_status("rm -f ovs-vsctl /tmp/dpdkvhostclient1 /tmp/dpdkvhostclient2");
}
// Setup two guests and ensure they are connected through ovs-dpdk
fn setup_ovs_dpdk_guests(
    guest1: &Guest,
    guest2: &Guest,
    api_socket: &str,
    release_binary: bool,
) -> (Child, Child) {
    setup_ovs_dpdk();

    let clh_path = if !release_binary {
        clh_command("cloud-hypervisor")
    } else {
        cloud_hypervisor_release_path()
    };

    let mut child1 = GuestCommand::new_with_binary_path(guest1, &clh_path)
                    .args(["--cpus", "boot=2"])
                    .args(["--memory", "size=0,shared=on"])
                    .args(["--memory-zone", "id=mem0,size=1G,shared=on,host_numa_node=0"])
                    .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
                    .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
                    .default_disks()
                    .args(["--net", guest1.default_net_string().as_str(), "vhost_user=true,socket=/tmp/dpdkvhostclient1,num_queues=2,queue_size=256,vhost_mode=server"])
                    .capture_output()
                    .spawn()
                    .unwrap();

    #[cfg(target_arch = "x86_64")]
    let guest_net_iface = "ens5";
    #[cfg(target_arch = "aarch64")]
    let guest_net_iface = "enp0s5";

    let r = std::panic::catch_unwind(|| {
        guest1.wait_vm_boot(None).unwrap();

        guest1
            .ssh_command(&format!(
                "sudo ip addr add 172.100.0.1/24 dev {}",
                guest_net_iface
            ))
            .unwrap();
        guest1
            .ssh_command(&format!("sudo ip link set up dev {}", guest_net_iface))
            .unwrap();

        let guest_ip = guest1.network.guest_ip.clone();
        thread::spawn(move || {
            ssh_command_ip(
                "nc -l 12345",
                &guest_ip,
                DEFAULT_SSH_RETRIES,
                DEFAULT_SSH_TIMEOUT,
            )
            .unwrap();
        });
    });
    if r.is_err() {
        cleanup_ovs_dpdk();

        let _ = child1.kill();
        let output = child1.wait_with_output().unwrap();
        handle_child_output(r, &output);
        panic!("Test should already be failed/panicked"); // To explicitly mark this block never return
    }

    let mut child2 = GuestCommand::new_with_binary_path(guest2, &clh_path)
                    .args(["--api-socket", api_socket])
                    .args(["--cpus", "boot=2"])
                    .args(["--memory", "size=0,shared=on"])
                    .args(["--memory-zone", "id=mem0,size=1G,shared=on,host_numa_node=0"])
                    .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
                    .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
                    .default_disks()
                    .args(["--net", guest2.default_net_string().as_str(), "vhost_user=true,socket=/tmp/dpdkvhostclient2,num_queues=2,queue_size=256,vhost_mode=server"])
                    .capture_output()
                    .spawn()
                    .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest2.wait_vm_boot(None).unwrap();

        guest2
            .ssh_command(&format!(
                "sudo ip addr add 172.100.0.2/24 dev {}",
                guest_net_iface
            ))
            .unwrap();
        guest2
            .ssh_command(&format!("sudo ip link set up dev {}", guest_net_iface))
            .unwrap();

        // Check the connection works properly between the two VMs
        guest2.ssh_command("nc -vz 172.100.0.1 12345").unwrap();
    });
    if r.is_err() {
        cleanup_ovs_dpdk();

        let _ = child1.kill();
        let _ = child2.kill();
        let output = child2.wait_with_output().unwrap();
        handle_child_output(r, &output);
        panic!("Test should already be failed/panicked"); // To explicitly mark this block never return
    }

    (child1, child2)
}

enum FwType {
    Ovmf,
    RustHypervisorFirmware,
}

fn fw_path(_fw_type: FwType) -> String {
    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");

    let mut fw_path = workload_path;
    #[cfg(target_arch = "aarch64")]
    fw_path.push("CLOUDHV_EFI.fd");
    #[cfg(target_arch = "x86_64")]
    {
        match _fw_type {
            FwType::Ovmf => fw_path.push(OVMF_NAME),
            FwType::RustHypervisorFirmware => fw_path.push("hypervisor-fw"),
        }
    }

    fw_path.to_str().unwrap().to_string()
}

struct MetaEvent {
    event: String,
    device_id: Option<String>,
}

impl MetaEvent {
    pub fn match_with_json_event(&self, v: &serde_json::Value) -> bool {
        let mut matched = false;
        if v["event"].as_str().unwrap() == self.event {
            if let Some(device_id) = &self.device_id {
                if v["properties"]["id"].as_str().unwrap() == device_id {
                    matched = true
                }
            } else {
                matched = true;
            }
        }
        matched
    }
}

// Parse the event_monitor file based on the format that each event
// is followed by a double newline
fn parse_event_file(event_file: &str) -> Vec<serde_json::Value> {
    let content = fs::read(event_file).unwrap();
    let mut ret = Vec::new();
    for entry in String::from_utf8_lossy(&content)
        .trim()
        .split("\n\n")
        .collect::<Vec<&str>>()
    {
        ret.push(serde_json::from_str(entry).unwrap());
    }

    ret
}

// Return true if all events from the input 'expected_events' are matched sequentially
// with events from the 'event_file'
fn check_sequential_events(expected_events: &[&MetaEvent], event_file: &str) -> bool {
    let json_events = parse_event_file(event_file);
    let len = expected_events.len();
    let mut idx = 0;
    for e in &json_events {
        if idx == len {
            break;
        }
        if expected_events[idx].match_with_json_event(e) {
            idx += 1;
        }
    }

    idx == len
}

// Return true if all events from the input 'expected_events' are matched exactly
// with events from the 'event_file'
fn check_sequential_events_exact(expected_events: &[&MetaEvent], event_file: &str) -> bool {
    let json_events = parse_event_file(event_file);
    assert!(expected_events.len() <= json_events.len());
    let json_events = &json_events[..expected_events.len()];

    for (idx, e) in json_events.iter().enumerate() {
        if !expected_events[idx].match_with_json_event(e) {
            return false;
        }
    }

    true
}

// Return true if events from the input 'expected_events' are matched exactly
// with the most recent events from the 'event_file'
fn check_latest_events_exact(latest_events: &[&MetaEvent], event_file: &str) -> bool {
    let json_events = parse_event_file(event_file);
    assert!(latest_events.len() <= json_events.len());
    let json_events = &json_events[(json_events.len() - latest_events.len())..];

    for (idx, e) in json_events.iter().enumerate() {
        if !latest_events[idx].match_with_json_event(e) {
            return false;
        }
    }

    true
}

fn test_cpu_topology(threads_per_core: u8, cores_per_package: u8, packages: u8, use_fw: bool) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));
    let total_vcpus = threads_per_core * cores_per_package * packages;
    let direct_kernel_boot_path = direct_kernel_boot_path();
    let mut kernel_path = direct_kernel_boot_path.to_str().unwrap();
    let fw_path = fw_path(FwType::RustHypervisorFirmware);
    if use_fw {
        kernel_path = fw_path.as_str();
    }

    let mut child = GuestCommand::new(&guest)
        .args([
            "--cpus",
            &format!(
                "boot={},topology={}:{}:1:{}",
                total_vcpus, threads_per_core, cores_per_package, packages
            ),
        ])
        .args(["--memory", "size=512M"])
        .args(["--kernel", kernel_path])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .default_net()
        .capture_output()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();
        assert_eq!(
            guest.get_cpu_count().unwrap_or_default(),
            u32::from(total_vcpus)
        );
        assert_eq!(
            guest
                .ssh_command("lscpu | grep \"per core\" | cut -f 2 -d \":\" | sed \"s# *##\"")
                .unwrap()
                .trim()
                .parse::<u8>()
                .unwrap_or(0),
            threads_per_core
        );

        assert_eq!(
            guest
                .ssh_command("lscpu | grep \"per socket\" | cut -f 2 -d \":\" | sed \"s# *##\"")
                .unwrap()
                .trim()
                .parse::<u8>()
                .unwrap_or(0),
            cores_per_package
        );

        assert_eq!(
            guest
                .ssh_command("lscpu | grep \"Socket\" | cut -f 2 -d \":\" | sed \"s# *##\"")
                .unwrap()
                .trim()
                .parse::<u8>()
                .unwrap_or(0),
            packages
        );
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    handle_child_output(r, &output);
}

#[allow(unused_variables)]
fn _test_guest_numa_nodes(acpi: bool) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));
    let api_socket = temp_api_path(&guest.tmp_dir);
    #[cfg(target_arch = "x86_64")]
    let kernel_path = direct_kernel_boot_path();
    #[cfg(target_arch = "aarch64")]
    let kernel_path = if acpi {
        edk2_path()
    } else {
        direct_kernel_boot_path()
    };

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", "boot=6,max=12"])
        .args(["--memory", "size=0,hotplug_method=virtio-mem"])
        .args([
            "--memory-zone",
            "id=mem0,size=1G,hotplug_size=3G",
            "id=mem1,size=2G,hotplug_size=3G",
            "id=mem2,size=3G,hotplug_size=3G",
        ])
        .args([
            "--numa",
            "guest_numa_id=0,cpus=[0-2,9],distances=[1@15,2@20],memory_zones=mem0",
            "guest_numa_id=1,cpus=[3-4,6-8],distances=[0@20,2@25],memory_zones=mem1",
            "guest_numa_id=2,cpus=[5,10-11],distances=[0@25,1@30],memory_zones=mem2",
        ])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .args(["--api-socket", &api_socket])
        .capture_output()
        .default_disks()
        .default_net()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        guest.check_numa_common(
            Some(&[960_000, 1_920_000, 2_880_000]),
            Some(&[vec![0, 1, 2], vec![3, 4], vec![5]]),
            Some(&["10 15 20", "20 10 25", "25 30 10"]),
        );

        // AArch64 currently does not support hotplug, and therefore we only
        // test hotplug-related function on x86_64 here.
        #[cfg(target_arch = "x86_64")]
        {
            guest.enable_memory_hotplug();

            // Resize every memory zone and check each associated NUMA node
            // has been assigned the right amount of memory.
            resize_zone_command(&api_socket, "mem0", "4G");
            resize_zone_command(&api_socket, "mem1", "4G");
            resize_zone_command(&api_socket, "mem2", "4G");
            // Resize to the maximum amount of CPUs and check each NUMA
            // node has been assigned the right CPUs set.
            resize_command(&api_socket, Some(12), None, None, None);
            thread::sleep(std::time::Duration::new(5, 0));

            guest.check_numa_common(
                Some(&[3_840_000, 3_840_000, 3_840_000]),
                Some(&[vec![0, 1, 2, 9], vec![3, 4, 6, 7, 8], vec![5, 10, 11]]),
                None,
            );
        }
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    handle_child_output(r, &output);
}

#[allow(unused_variables)]
fn _test_power_button(acpi: bool) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));
    let mut cmd = GuestCommand::new(&guest);
    let api_socket = temp_api_path(&guest.tmp_dir);

    #[cfg(target_arch = "x86_64")]
    let kernel_path = direct_kernel_boot_path();
    #[cfg(target_arch = "aarch64")]
    let kernel_path = if acpi {
        edk2_path()
    } else {
        direct_kernel_boot_path()
    };

    cmd.args(["--cpus", "boot=1"])
        .args(["--memory", "size=512M"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .capture_output()
        .default_disks()
        .default_net()
        .args(["--api-socket", &api_socket]);

    let child = cmd.spawn().unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();
        assert!(remote_command(&api_socket, "power-button", None));
    });

    let output = child.wait_with_output().unwrap();
    assert!(output.status.success());
    handle_child_output(r, &output);
}

type PrepareNetDaemon = dyn Fn(
    &TempDir,
    &str,
    Option<&str>,
    Option<u16>,
    usize,
    bool,
) -> (std::process::Command, String);

fn test_vhost_user_net(
    tap: Option<&str>,
    num_queues: usize,
    prepare_daemon: &PrepareNetDaemon,
    generate_host_mac: bool,
    client_mode_daemon: bool,
) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));
    let api_socket = temp_api_path(&guest.tmp_dir);

    let kernel_path = direct_kernel_boot_path();

    let host_mac = if generate_host_mac {
        Some(MacAddr::local_random())
    } else {
        None
    };

    let mtu = Some(3000);

    let (mut daemon_command, vunet_socket_path) = prepare_daemon(
        &guest.tmp_dir,
        &guest.network.host_ip,
        tap,
        mtu,
        num_queues,
        client_mode_daemon,
    );

    let net_params = format!(
        "vhost_user=true,mac={},socket={},num_queues={},queue_size=1024{},vhost_mode={},mtu=3000",
        guest.network.guest_mac,
        vunet_socket_path,
        num_queues,
        if let Some(host_mac) = host_mac {
            format!(",host_mac={}", host_mac)
        } else {
            "".to_owned()
        },
        if client_mode_daemon {
            "server"
        } else {
            "client"
        },
    );

    let mut ch_command = GuestCommand::new(&guest);
    ch_command
        .args(["--cpus", format!("boot={}", num_queues / 2).as_str()])
        .args(["--memory", "size=512M,hotplug_size=2048M,shared=on"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .args(["--net", net_params.as_str()])
        .args(["--api-socket", &api_socket])
        .capture_output();

    let mut daemon_child: std::process::Child;
    let mut child: std::process::Child;

    if client_mode_daemon {
        child = ch_command.spawn().unwrap();
        // Make sure the VMM is waiting for the backend to connect
        thread::sleep(std::time::Duration::new(10, 0));
        daemon_child = daemon_command.spawn().unwrap();
    } else {
        daemon_child = daemon_command.spawn().unwrap();
        // Make sure the backend is waiting for the VMM to connect
        thread::sleep(std::time::Duration::new(10, 0));
        child = ch_command.spawn().unwrap();
    }

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        if let Some(tap_name) = tap {
            let tap_count = exec_host_command_output(&format!("ip link | grep -c {}", tap_name));
            assert_eq!(String::from_utf8_lossy(&tap_count.stdout).trim(), "1");
        }

        if let Some(host_mac) = tap {
            let mac_count = exec_host_command_output(&format!("ip link | grep -c {}", host_mac));
            assert_eq!(String::from_utf8_lossy(&mac_count.stdout).trim(), "1");
        }

        #[cfg(target_arch = "aarch64")]
        let iface = "enp0s4";
        #[cfg(target_arch = "x86_64")]
        let iface = "ens4";

        assert_eq!(
            guest
                .ssh_command(format!("cat /sys/class/net/{}/mtu", iface).as_str())
                .unwrap()
                .trim(),
            "3000"
        );

        // 1 network interface + default localhost ==> 2 interfaces
        // It's important to note that this test is fully exercising the
        // vhost-user-net implementation and the associated backend since
        // it does not define any --net network interface. That means all
        // the ssh communication in that test happens through the network
        // interface backed by vhost-user-net.
        assert_eq!(
            guest
                .ssh_command("ip -o link | wc -l")
                .unwrap()
                .trim()
                .parse::<u32>()
                .unwrap_or_default(),
            2
        );

        // The following pci devices will appear on guest with PCI-MSI
        // interrupt vectors assigned.
        // 1 virtio-console with 3 vectors: config, Rx, Tx
        // 1 virtio-blk     with 2 vectors: config, Request
        // 1 virtio-blk     with 2 vectors: config, Request
        // 1 virtio-rng     with 2 vectors: config, Request
        // Since virtio-net has 2 queue pairs, its vectors is as follows:
        // 1 virtio-net     with 5 vectors: config, Rx (2), Tx (2)
        // Based on the above, the total vectors should 14.
        #[cfg(target_arch = "x86_64")]
        let grep_cmd = "grep -c PCI-MSI /proc/interrupts";
        #[cfg(target_arch = "aarch64")]
        let grep_cmd = "grep -c ITS-MSI /proc/interrupts";
        assert_eq!(
            guest
                .ssh_command(grep_cmd)
                .unwrap()
                .trim()
                .parse::<u32>()
                .unwrap_or_default(),
            10 + (num_queues as u32)
        );

        // ACPI feature is needed.
        #[cfg(target_arch = "x86_64")]
        {
            guest.enable_memory_hotplug();

            // Add RAM to the VM
            let desired_ram = 1024 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(10, 0));

            // Here by simply checking the size (through ssh), we validate
            // the connection is still working, which means vhost-user-net
            // keeps working after the resize.
            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);
        }
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    thread::sleep(std::time::Duration::new(5, 0));
    let _ = daemon_child.kill();
    let _ = daemon_child.wait();

    handle_child_output(r, &output);
}

type PrepareBlkDaemon = dyn Fn(&TempDir, &str, usize, bool, bool) -> (std::process::Child, String);

fn test_vhost_user_blk(
    num_queues: usize,
    readonly: bool,
    direct: bool,
    prepare_vhost_user_blk_daemon: Option<&PrepareBlkDaemon>,
) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));
    let api_socket = temp_api_path(&guest.tmp_dir);

    let kernel_path = direct_kernel_boot_path();

    let (blk_params, daemon_child) = {
        let prepare_daemon = prepare_vhost_user_blk_daemon.unwrap();
        // Start the daemon
        let (daemon_child, vubd_socket_path) =
            prepare_daemon(&guest.tmp_dir, "blk.img", num_queues, readonly, direct);

        (
            format!(
                "vhost_user=true,socket={},num_queues={},queue_size=128",
                vubd_socket_path, num_queues,
            ),
            Some(daemon_child),
        )
    };

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", format!("boot={}", num_queues).as_str()])
        .args(["--memory", "size=512M,hotplug_size=2048M,shared=on"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .args([
            "--disk",
            format!(
                "path={}",
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
            )
            .as_str(),
            format!(
                "path={}",
                guest.disk_config.disk(DiskType::CloudInit).unwrap()
            )
            .as_str(),
            blk_params.as_str(),
        ])
        .default_net()
        .args(["--api-socket", &api_socket])
        .capture_output()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        // Check both if /dev/vdc exists and if the block size is 16M.
        assert_eq!(
            guest
                .ssh_command("lsblk | grep vdc | grep -c 16M")
                .unwrap()
                .trim()
                .parse::<u32>()
                .unwrap_or_default(),
            1
        );

        // Check if this block is RO or RW.
        assert_eq!(
            guest
                .ssh_command("lsblk | grep vdc | awk '{print $5}'")
                .unwrap()
                .trim()
                .parse::<u32>()
                .unwrap_or_default(),
            readonly as u32
        );

        // Check if the number of queues in /sys/block/vdc/mq matches the
        // expected num_queues.
        assert_eq!(
            guest
                .ssh_command("ls -ll /sys/block/vdc/mq | grep ^d | wc -l")
                .unwrap()
                .trim()
                .parse::<u32>()
                .unwrap_or_default(),
            num_queues as u32
        );

        // Mount the device
        let mount_ro_rw_flag = if readonly { "ro,noload" } else { "rw" };
        guest.ssh_command("mkdir mount_image").unwrap();
        guest
            .ssh_command(
                format!(
                    "sudo mount -o {} -t ext4 /dev/vdc mount_image/",
                    mount_ro_rw_flag
                )
                .as_str(),
            )
            .unwrap();

        // Check the content of the block device. The file "foo" should
        // contain "bar".
        assert_eq!(
            guest.ssh_command("cat mount_image/foo").unwrap().trim(),
            "bar"
        );

        // ACPI feature is needed.
        #[cfg(target_arch = "x86_64")]
        {
            guest.enable_memory_hotplug();

            // Add RAM to the VM
            let desired_ram = 1024 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(10, 0));

            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);

            // Check again the content of the block device after the resize
            // has been performed.
            assert_eq!(
                guest.ssh_command("cat mount_image/foo").unwrap().trim(),
                "bar"
            );
        }

        // Unmount the device
        guest.ssh_command("sudo umount /dev/vdc").unwrap();
        guest.ssh_command("rm -r mount_image").unwrap();
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    if let Some(mut daemon_child) = daemon_child {
        thread::sleep(std::time::Duration::new(5, 0));
        let _ = daemon_child.kill();
        let _ = daemon_child.wait();
    }

    handle_child_output(r, &output);
}

fn test_boot_from_vhost_user_blk(
    num_queues: usize,
    readonly: bool,
    direct: bool,
    prepare_vhost_user_blk_daemon: Option<&PrepareBlkDaemon>,
) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));

    let kernel_path = direct_kernel_boot_path();

    let disk_path = guest.disk_config.disk(DiskType::OperatingSystem).unwrap();

    let (blk_boot_params, daemon_child) = {
        let prepare_daemon = prepare_vhost_user_blk_daemon.unwrap();
        // Start the daemon
        let (daemon_child, vubd_socket_path) = prepare_daemon(
            &guest.tmp_dir,
            disk_path.as_str(),
            num_queues,
            readonly,
            direct,
        );

        (
            format!(
                "vhost_user=true,socket={},num_queues={},queue_size=128",
                vubd_socket_path, num_queues,
            ),
            Some(daemon_child),
        )
    };

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", format!("boot={}", num_queues).as_str()])
        .args(["--memory", "size=512M,shared=on"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .args([
            "--disk",
            blk_boot_params.as_str(),
            format!(
                "path={}",
                guest.disk_config.disk(DiskType::CloudInit).unwrap()
            )
            .as_str(),
        ])
        .default_net()
        .capture_output()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        // Just check the VM booted correctly.
        assert_eq!(guest.get_cpu_count().unwrap_or_default(), num_queues as u32);
        assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
    });
    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    if let Some(mut daemon_child) = daemon_child {
        thread::sleep(std::time::Duration::new(5, 0));
        let _ = daemon_child.kill();
        let _ = daemon_child.wait();
    }

    handle_child_output(r, &output);
}

fn _test_native_virtio_fs(hotplug: bool, pci_segment: Option<u16>) {
    #[cfg(target_arch = "x86_64")]
    let focal_image = FOCAL_IMAGE_NAME.to_string();
    let focal = UbuntuDiskConfig::new(focal_image);
    let guest = Guest::new(Box::new(focal));
    let api_socket = temp_api_path(&guest.tmp_dir);

    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");

    let mut shared_dir = workload_path;
    shared_dir.push("shared_dir");

    #[cfg(target_arch = "x86_64")]
    let kernel_path = direct_kernel_boot_path();

    let mut guest_command = GuestCommand::new(&guest);
    guest_command
        .args(["--cpus", "boot=1"])
        .args(["--memory", "size=512M,hotplug_size=2048M,shared=on"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .default_net()
        .args(["--api-socket", &api_socket]);
    if pci_segment.is_some() {
        guest_command.args(["--platform", "num_pci_segments=16"]);
    }

    let fs_params = format!(
        "id=myfs0,tag=myfs,native=true,shared_dir={},cache={},num_queues=1,queue_size=1024{}",
        shared_dir.to_str().unwrap(),
        "always".to_owned(),
        if let Some(pci_segment) = pci_segment {
            format!(",pci_segment={}", pci_segment)
        } else {
            "".to_owned()
        }
    );

    if !hotplug {
        guest_command.args(["--fs", fs_params.as_str()]);
    }

    let mut child = guest_command.capture_output().spawn().unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        if hotplug {
            // Add fs to the VM
            let (cmd_success, cmd_output) =
                remote_command_w_output(&api_socket, "add-fs", Some(&fs_params));
            assert!(cmd_success);

            if let Some(pci_segment) = pci_segment {
                assert!(String::from_utf8_lossy(&cmd_output).contains(&format!(
                    "{{\"id\":\"myfs0\",\"bdf\":\"{:04x}:00:01.0\"}}",
                    pci_segment
                )));
            } else {
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains("{\"id\":\"myfs0\",\"bdf\":\"0000:00:06.0\"}"));
            }

            thread::sleep(std::time::Duration::new(10, 0));
        }

        // Mount shared directory through virtio_fs filesystem
        guest
            .ssh_command("mkdir -p mount_dir && sudo mount -t virtiofs myfs mount_dir/")
            .unwrap();

        // Check file1 exists and its content is "foo"
        assert_eq!(
            guest.ssh_command("cat mount_dir/file1").unwrap().trim(),
            "foo"
        );
        // Check file2 does not exist
        guest
            .ssh_command("[ ! -f 'mount_dir/file2' ] || true")
            .unwrap();

        // Check file3 exists and its content is "bar"
        assert_eq!(
            guest.ssh_command("cat mount_dir/file3").unwrap().trim(),
            "bar"
        );

        // ACPI feature is needed.
        #[cfg(target_arch = "x86_64")]
        {
            guest.enable_memory_hotplug();

            // Add RAM to the VM
            let desired_ram = 1024 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(30, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);

            // After the resize, check again that file1 exists and its
            // content is "foo".
            assert_eq!(
                guest.ssh_command("cat mount_dir/file1").unwrap().trim(),
                "foo"
            );
        }

        if hotplug {
            // Remove from VM
            guest.ssh_command("sudo umount mount_dir").unwrap();
            assert!(remote_command(&api_socket, "remove-device", Some("myfs0")));
        }
    });

    let r = if r.is_ok() && hotplug {
        std::panic::catch_unwind(|| {
            thread::sleep(std::time::Duration::new(10, 0));
            let fs_params = format!(
                "id=myfs0,tag=myfs,native=true,shared_dir={},cache={},num_queues=1,queue_size=1024{}",
                    shared_dir.to_str().unwrap(),
                    "always".to_owned(),
                if let Some(pci_segment) = pci_segment {
                    format!(",pci_segment={}", pci_segment)
                } else {
                    "".to_owned()
                }
            );

            // Add back and check it works
            let (cmd_success, cmd_output) =
                remote_command_w_output(&api_socket, "add-fs", Some(&fs_params));
            assert!(cmd_success);
            if let Some(pci_segment) = pci_segment {
                assert!(String::from_utf8_lossy(&cmd_output).contains(&format!(
                    "{{\"id\":\"myfs0\",\"bdf\":\"{:04x}:00:01.0\"}}",
                    pci_segment
                )));
            } else {
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains("{\"id\":\"myfs0\",\"bdf\":\"0000:00:06.0\"}"));
            }

            thread::sleep(std::time::Duration::new(10, 0));
            // Mount shared directory through virtio_fs filesystem
            guest
                .ssh_command("mkdir -p mount_dir && sudo mount -t virtiofs myfs mount_dir/")
                .unwrap();

            // Check file1 exists and its content is "foo"
            assert_eq!(
                guest.ssh_command("cat mount_dir/file1").unwrap().trim(),
                "foo"
            );
        })
    } else {
        r
    };

    kill_child(&mut child);
    let output = child.wait_with_output().unwrap();

    handle_child_output(r, &output);
}

fn _test_virtio_fs(
    prepare_daemon: &dyn Fn(&TempDir, &str) -> (std::process::Child, String),
    hotplug: bool,
    pci_segment: Option<u16>,
) {
    #[cfg(target_arch = "aarch64")]
    let focal_image = if hotplug {
        FOCAL_IMAGE_UPDATE_KERNEL_NAME.to_string()
    } else {
        FOCAL_IMAGE_NAME.to_string()
    };
    #[cfg(target_arch = "x86_64")]
    let focal_image = FOCAL_IMAGE_NAME.to_string();
    let focal = UbuntuDiskConfig::new(focal_image);
    let guest = Guest::new(Box::new(focal));
    let api_socket = temp_api_path(&guest.tmp_dir);

    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");

    let mut shared_dir = workload_path;
    shared_dir.push("shared_dir");

    #[cfg(target_arch = "x86_64")]
    let kernel_path = direct_kernel_boot_path();
    #[cfg(target_arch = "aarch64")]
    let kernel_path = if hotplug {
        edk2_path()
    } else {
        direct_kernel_boot_path()
    };

    let (mut daemon_child, virtiofsd_socket_path) =
        prepare_daemon(&guest.tmp_dir, shared_dir.to_str().unwrap());

    let mut guest_command = GuestCommand::new(&guest);
    guest_command
        .args(["--cpus", "boot=1"])
        .args(["--memory", "size=512M,hotplug_size=2048M,shared=on"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .default_net()
        .args(["--api-socket", &api_socket]);
    if pci_segment.is_some() {
        guest_command.args(["--platform", "num_pci_segments=16"]);
    }

    let fs_params = format!(
        "id=myfs0,tag=myfs,socket={},num_queues=1,queue_size=1024{}",
        virtiofsd_socket_path,
        if let Some(pci_segment) = pci_segment {
            format!(",pci_segment={}", pci_segment)
        } else {
            "".to_owned()
        }
    );

    if !hotplug {
        guest_command.args(["--fs", fs_params.as_str()]);
    }

    let mut child = guest_command.capture_output().spawn().unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        if hotplug {
            // Add fs to the VM
            let (cmd_success, cmd_output) =
                remote_command_w_output(&api_socket, "add-fs", Some(&fs_params));
            assert!(cmd_success);

            if let Some(pci_segment) = pci_segment {
                assert!(String::from_utf8_lossy(&cmd_output).contains(&format!(
                    "{{\"id\":\"myfs0\",\"bdf\":\"{:04x}:00:01.0\"}}",
                    pci_segment
                )));
            } else {
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains("{\"id\":\"myfs0\",\"bdf\":\"0000:00:06.0\"}"));
            }

            thread::sleep(std::time::Duration::new(10, 0));
        }

        // Mount shared directory through virtio_fs filesystem
        guest
            .ssh_command("mkdir -p mount_dir && sudo mount -t virtiofs myfs mount_dir/")
            .unwrap();

        // Check file1 exists and its content is "foo"
        assert_eq!(
            guest.ssh_command("cat mount_dir/file1").unwrap().trim(),
            "foo"
        );
        // Check file2 does not exist
        guest
            .ssh_command("[ ! -f 'mount_dir/file2' ] || true")
            .unwrap();

        // Check file3 exists and its content is "bar"
        assert_eq!(
            guest.ssh_command("cat mount_dir/file3").unwrap().trim(),
            "bar"
        );

        // ACPI feature is needed.
        #[cfg(target_arch = "x86_64")]
        {
            guest.enable_memory_hotplug();

            // Add RAM to the VM
            let desired_ram = 1024 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(30, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);

            // After the resize, check again that file1 exists and its
            // content is "foo".
            assert_eq!(
                guest.ssh_command("cat mount_dir/file1").unwrap().trim(),
                "foo"
            );
        }

        if hotplug {
            // Remove from VM
            guest.ssh_command("sudo umount mount_dir").unwrap();
            assert!(remote_command(&api_socket, "remove-device", Some("myfs0")));
        }
    });

    let (r, hotplug_daemon_child) = if r.is_ok() && hotplug {
        thread::sleep(std::time::Duration::new(10, 0));
        let (daemon_child, virtiofsd_socket_path) =
            prepare_daemon(&guest.tmp_dir, shared_dir.to_str().unwrap());

        let r = std::panic::catch_unwind(|| {
            thread::sleep(std::time::Duration::new(10, 0));
            let fs_params = format!(
                "id=myfs0,tag=myfs,socket={},num_queues=1,queue_size=1024{}",
                virtiofsd_socket_path,
                if let Some(pci_segment) = pci_segment {
                    format!(",pci_segment={}", pci_segment)
                } else {
                    "".to_owned()
                }
            );

            // Add back and check it works
            let (cmd_success, cmd_output) =
                remote_command_w_output(&api_socket, "add-fs", Some(&fs_params));
            assert!(cmd_success);
            if let Some(pci_segment) = pci_segment {
                assert!(String::from_utf8_lossy(&cmd_output).contains(&format!(
                    "{{\"id\":\"myfs0\",\"bdf\":\"{:04x}:00:01.0\"}}",
                    pci_segment
                )));
            } else {
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains("{\"id\":\"myfs0\",\"bdf\":\"0000:00:06.0\"}"));
            }

            thread::sleep(std::time::Duration::new(10, 0));
            // Mount shared directory through virtio_fs filesystem
            guest
                .ssh_command("mkdir -p mount_dir && sudo mount -t virtiofs myfs mount_dir/")
                .unwrap();

            // Check file1 exists and its content is "foo"
            assert_eq!(
                guest.ssh_command("cat mount_dir/file1").unwrap().trim(),
                "foo"
            );
        });

        (r, Some(daemon_child))
    } else {
        (r, None)
    };

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    let _ = daemon_child.kill();
    let _ = daemon_child.wait();

    if let Some(mut daemon_child) = hotplug_daemon_child {
        let _ = daemon_child.kill();
        let _ = daemon_child.wait();
    }

    handle_child_output(r, &output);
}

fn test_virtio_pmem(discard_writes: bool, specify_size: bool) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));

    let kernel_path = direct_kernel_boot_path();

    let pmem_temp_file = TempFile::new().unwrap();
    pmem_temp_file.as_file().set_len(128 << 20).unwrap();

    std::process::Command::new("mkfs.ext4")
        .arg(pmem_temp_file.as_path())
        .output()
        .expect("Expect creating disk image to succeed");

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", "boot=1"])
        .args(["--memory", "size=512M"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .default_net()
        .args([
            "--pmem",
            format!(
                "file={}{}{}",
                pmem_temp_file.as_path().to_str().unwrap(),
                if specify_size { ",size=128M" } else { "" },
                if discard_writes {
                    ",discard_writes=on"
                } else {
                    ""
                }
            )
            .as_str(),
        ])
        .capture_output()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        // Check for the presence of /dev/pmem0
        assert_eq!(
            guest.ssh_command("ls /dev/pmem0").unwrap().trim(),
            "/dev/pmem0"
        );

        // Check changes persist after reboot
        assert_eq!(guest.ssh_command("sudo mount /dev/pmem0 /mnt").unwrap(), "");
        assert_eq!(guest.ssh_command("ls /mnt").unwrap(), "lost+found\n");
        guest
            .ssh_command("echo test123 | sudo tee /mnt/test")
            .unwrap();
        assert_eq!(guest.ssh_command("sudo umount /mnt").unwrap(), "");
        assert_eq!(guest.ssh_command("ls /mnt").unwrap(), "");

        // guest.reboot_linux(0, None);
        // assert_eq!(guest.ssh_command("sudo mount /dev/pmem0 /mnt").unwrap(), "");
        // assert_eq!(
        //     guest
        //         .ssh_command("sudo cat /mnt/test || true")
        //         .unwrap()
        //         .trim(),
        //     if discard_writes { "" } else { "test123" }
        // );
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    handle_child_output(r, &output);
}

fn get_fd_count(pid: u32) -> usize {
    fs::read_dir(format!("/proc/{}/fd", pid)).unwrap().count()
}

fn _test_virtio_vsock(hotplug: bool) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));

    #[cfg(target_arch = "x86_64")]
    let kernel_path = direct_kernel_boot_path();
    #[cfg(target_arch = "aarch64")]
    let kernel_path = if hotplug {
        edk2_path()
    } else {
        direct_kernel_boot_path()
    };

    let socket = temp_vsock_path(&guest.tmp_dir);
    let api_socket = temp_api_path(&guest.tmp_dir);

    let mut cmd = GuestCommand::new(&guest);
    cmd.args(["--api-socket", &api_socket]);
    cmd.args(["--cpus", "boot=1"]);
    cmd.args(["--memory", "size=512M"]);
    cmd.args(["--kernel", kernel_path.to_str().unwrap()]);
    cmd.args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE]);
    cmd.default_disks();
    cmd.default_net();

    if !hotplug {
        cmd.args(["--vsock", format!("cid=3,socket={}", socket).as_str()]);
    }

    let mut child = cmd.capture_output().spawn().unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        if hotplug {
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-vsock",
                Some(format!("cid=3,socket={},id=test0", socket).as_str()),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"test0\",\"bdf\":\"0000:00:06.0\"}"));
            thread::sleep(std::time::Duration::new(10, 0));
            // Check adding a second one fails
            assert!(!remote_command(
                &api_socket,
                "add-vsock",
                Some("cid=1234,socket=/tmp/fail")
            ));
        }

        // Validate vsock works as expected.
        guest.check_vsock(socket.as_str());
        // guest.reboot_linux(0, None);
        // Validate vsock still works after a reboot.
        // guest.check_vsock(socket.as_str());

        if hotplug {
            assert!(remote_command(&api_socket, "remove-device", Some("test0")));
        }
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    handle_child_output(r, &output);
}

fn get_ksm_pages_shared() -> u32 {
    fs::read_to_string("/sys/kernel/mm/ksm/pages_shared")
        .unwrap()
        .trim()
        .parse::<u32>()
        .unwrap()
}

fn test_memory_mergeable(mergeable: bool) {
    let memory_param = if mergeable {
        "mergeable=on"
    } else {
        "mergeable=off"
    };

    // We are assuming the rest of the system in our CI is not using mergeable memeory
    let ksm_ps_init = get_ksm_pages_shared();
    assert!(ksm_ps_init == 0);

    let focal1 = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest1 = Guest::new(Box::new(focal1));
    let mut child1 = GuestCommand::new(&guest1)
        .args(["--cpus", "boot=1"])
        .args(["--memory", format!("size=512M,{}", memory_param).as_str()])
        .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .args(["--net", guest1.default_net_string().as_str()])
        .args(["--serial", "tty", "--console", "off"])
        .capture_output()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest1.wait_vm_boot(None).unwrap();
    });
    if r.is_err() {
        let _ = child1.kill();
        let output = child1.wait_with_output().unwrap();
        handle_child_output(r, &output);
        panic!("Test should already be failed/panicked"); // To explicitly mark this block never return
    }

    let ksm_ps_guest1 = get_ksm_pages_shared();

    let focal2 = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest2 = Guest::new(Box::new(focal2));
    let mut child2 = GuestCommand::new(&guest2)
        .args(["--cpus", "boot=1"])
        .args(["--memory", format!("size=512M,{}", memory_param).as_str()])
        .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .args(["--net", guest2.default_net_string().as_str()])
        .args(["--serial", "tty", "--console", "off"])
        .capture_output()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest2.wait_vm_boot(None).unwrap();
        let ksm_ps_guest2 = get_ksm_pages_shared();

        if mergeable {
            println!(
                "ksm pages_shared after vm1 booted '{}', ksm pages_shared after vm2 booted '{}'",
                ksm_ps_guest1, ksm_ps_guest2
            );
            // We are expecting the number of shared pages to increase as the number of VM increases
            assert!(ksm_ps_guest1 < ksm_ps_guest2);
        } else {
            assert!(ksm_ps_guest1 == 0);
            assert!(ksm_ps_guest2 == 0);
        }
    });

    let _ = child1.kill();
    let _ = child2.kill();

    let output = child1.wait_with_output().unwrap();
    child2.wait().unwrap();

    handle_child_output(r, &output);
}

fn _get_vmm_overhead(pid: u32, guest_memory_size: u32) -> HashMap<String, u32> {
    let smaps = fs::File::open(format!("/proc/{}/smaps", pid)).unwrap();
    let reader = io::BufReader::new(smaps);

    let mut skip_map: bool = false;
    let mut region_name: String = "".to_string();
    let mut region_maps = HashMap::new();
    for line in reader.lines() {
        let l = line.unwrap();

        if l.contains('-') {
            let values: Vec<&str> = l.split_whitespace().collect();
            region_name = values.last().unwrap().trim().to_string();
            if region_name == "0" {
                region_name = "anonymous".to_string()
            }
        }

        // Each section begins with something that looks like:
        // Size:               2184 kB
        if l.starts_with("Size:") {
            let values: Vec<&str> = l.split_whitespace().collect();
            let map_size = values[1].parse::<u32>().unwrap();
            // We skip the assigned guest RAM map, its RSS is only
            // dependent on the guest actual memory usage.
            // Everything else can be added to the VMM overhead.
            skip_map = map_size >= guest_memory_size;
            continue;
        }

        // If this is a map we're taking into account, then we only
        // count the RSS. The sum of all counted RSS is the VMM overhead.
        if !skip_map && l.starts_with("Rss:") {
            let values: Vec<&str> = l.split_whitespace().collect();
            let value = values[1].trim().parse::<u32>().unwrap();
            *region_maps.entry(region_name.clone()).or_insert(0) += value;
        }
    }

    region_maps
}

fn get_vmm_overhead(pid: u32, guest_memory_size: u32) -> u32 {
    let mut total = 0;

    for (region_name, value) in &_get_vmm_overhead(pid, guest_memory_size) {
        eprintln!("{}: {}", region_name, value);
        total += value;
    }

    total
}

fn process_rss_kib(pid: u32) -> usize {
    let command = format!("ps -q {} -o rss=", pid);
    let rss = exec_host_command_output(&command);
    String::from_utf8_lossy(&rss.stdout).trim().parse().unwrap()
}

// 10MB is our maximum accepted overhead.
const MAXIMUM_VMM_OVERHEAD_KB: u32 = 10 * 1024;

#[derive(PartialEq, Eq, PartialOrd)]
struct Counters {
    rx_bytes: u64,
    rx_frames: u64,
    tx_bytes: u64,
    tx_frames: u64,
    read_bytes: u64,
    write_bytes: u64,
    read_ops: u64,
    write_ops: u64,
}

fn get_counters(api_socket: &str) -> Counters {
    // Get counters
    let (cmd_success, cmd_output) = remote_command_w_output(api_socket, "counters", None);
    assert!(cmd_success);

    let counters: HashMap<&str, HashMap<&str, u64>> =
        serde_json::from_slice(&cmd_output).unwrap_or_default();

    let rx_bytes = *counters.get("_net2").unwrap().get("rx_bytes").unwrap();
    let rx_frames = *counters.get("_net2").unwrap().get("rx_frames").unwrap();
    let tx_bytes = *counters.get("_net2").unwrap().get("tx_bytes").unwrap();
    let tx_frames = *counters.get("_net2").unwrap().get("tx_frames").unwrap();

    let read_bytes = *counters.get("_disk0").unwrap().get("read_bytes").unwrap();
    let write_bytes = *counters.get("_disk0").unwrap().get("write_bytes").unwrap();
    let read_ops = *counters.get("_disk0").unwrap().get("read_ops").unwrap();
    let write_ops = *counters.get("_disk0").unwrap().get("write_ops").unwrap();

    Counters {
        rx_bytes,
        rx_frames,
        tx_bytes,
        tx_frames,
        read_bytes,
        write_bytes,
        read_ops,
        write_ops,
    }
}

fn pty_read(mut pty: std::fs::File) -> Receiver<String> {
    let (tx, rx) = mpsc::channel::<String>();
    thread::spawn(move || loop {
        thread::sleep(std::time::Duration::new(1, 0));
        let mut buf = [0; 512];
        match pty.read(&mut buf) {
            Ok(_) => {
                let output = std::str::from_utf8(&buf).unwrap().to_string();
                match tx.send(output) {
                    Ok(_) => (),
                    Err(_) => break,
                }
            }
            Err(_) => break,
        }
    });
    rx
}

fn get_pty_path(api_socket: &str, pty_type: &str) -> PathBuf {
    let (cmd_success, cmd_output) = remote_command_w_output(api_socket, "info", None);
    assert!(cmd_success);
    let info: serde_json::Value = serde_json::from_slice(&cmd_output).unwrap_or_default();
    assert_eq!("Pty", info["config"][pty_type]["mode"]);
    PathBuf::from(
        info["config"][pty_type]["file"]
            .as_str()
            .expect("Missing pty path"),
    )
}

// VFIO test network setup.
// We reserve a different IP class for it: 172.18.0.0/24.
#[cfg(target_arch = "x86_64")]
fn setup_vfio_network_interfaces() {
    // 'vfio-br0'
    assert!(exec_host_command_status("sudo ip link add name vfio-br0 type bridge").success());
    assert!(exec_host_command_status("sudo ip link set vfio-br0 up").success());
    assert!(exec_host_command_status("sudo ip addr add 172.18.0.1/24 dev vfio-br0").success());
    // 'vfio-tap0'
    assert!(exec_host_command_status("sudo ip tuntap add vfio-tap0 mode tap").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap0 master vfio-br0").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap0 up").success());
    // 'vfio-tap1'
    assert!(exec_host_command_status("sudo ip tuntap add vfio-tap1 mode tap").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap1 master vfio-br0").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap1 up").success());
    // 'vfio-tap2'
    assert!(exec_host_command_status("sudo ip tuntap add vfio-tap2 mode tap").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap2 master vfio-br0").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap2 up").success());
    // 'vfio-tap3'
    assert!(exec_host_command_status("sudo ip tuntap add vfio-tap3 mode tap").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap3 master vfio-br0").success());
    assert!(exec_host_command_status("sudo ip link set vfio-tap3 up").success());
}

// Tear VFIO test network down
#[cfg(target_arch = "x86_64")]
fn cleanup_vfio_network_interfaces() {
    assert!(exec_host_command_status("sudo ip link del vfio-br0").success());
    assert!(exec_host_command_status("sudo ip link del vfio-tap0").success());
    assert!(exec_host_command_status("sudo ip link del vfio-tap1").success());
    assert!(exec_host_command_status("sudo ip link del vfio-tap2").success());
    assert!(exec_host_command_status("sudo ip link del vfio-tap3").success());
}

fn balloon_size(api_socket: &str) -> u64 {
    let (cmd_success, cmd_output) = remote_command_w_output(api_socket, "info", None);
    assert!(cmd_success);

    let info: serde_json::Value = serde_json::from_slice(&cmd_output).unwrap_or_default();
    let total_mem = &info["config"]["memory"]["size"]
        .to_string()
        .parse::<u64>()
        .unwrap();
    let actual_mem = &info["memory_actual_size"]
        .to_string()
        .parse::<u64>()
        .unwrap();
    total_mem - actual_mem
}

// This test validates that it can find the virtio-iommu device at first.
// It also verifies that both disks and the network card are attached to
// the virtual IOMMU by looking at /sys/kernel/iommu_groups directory.
// The last interesting part of this test is that it exercises the network
// interface attached to the virtual IOMMU since this is the one used to
// send all commands through SSH.
fn _test_virtio_iommu(acpi: bool) {
    // Virtio-iommu support is ready in recent kernel (v5.14). But the kernel in
    // Focal image is still old.
    // So if ACPI is enabled on AArch64, we use a modified Focal image in which
    // the kernel binary has been updated.
    #[cfg(target_arch = "aarch64")]
    let focal_image = FOCAL_IMAGE_UPDATE_KERNEL_NAME.to_string();
    #[cfg(target_arch = "x86_64")]
    let focal_image = FOCAL_IMAGE_NAME.to_string();
    let focal = UbuntuDiskConfig::new(focal_image);
    let guest = Guest::new(Box::new(focal));

    #[cfg(target_arch = "x86_64")]
    let kernel_path = direct_kernel_boot_path();
    #[cfg(target_arch = "aarch64")]
    let kernel_path = if acpi {
        edk2_path()
    } else {
        direct_kernel_boot_path()
    };

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", "boot=1"])
        .args(["--memory", "size=512M"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .args([
            "--disk",
            format!(
                "path={},iommu=on",
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
            )
            .as_str(),
            format!(
                "path={},iommu=on",
                guest.disk_config.disk(DiskType::CloudInit).unwrap()
            )
            .as_str(),
        ])
        .args(["--net", guest.default_net_string_w_iommu().as_str()])
        .capture_output()
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        // Verify the virtio-iommu device is present.
        assert!(guest
            .does_device_vendor_pair_match("0x1057", "0x1af4")
            .unwrap_or_default());

        // On AArch64, if the guest system boots from FDT, the behavior of IOMMU is a bit
        // different with ACPI.
        // All devices on the PCI bus will be attached to the virtual IOMMU, except the
        // virtio-iommu device itself. So these devices will all be added to IOMMU groups,
        // and appear under folder '/sys/kernel/iommu_groups/'.
        // The result is, in the case of FDT, IOMMU group '0' contains "0000:00:01.0"
        // which is the console. The first disk "0000:00:02.0" is in group '1'.
        // While on ACPI, console device is not attached to IOMMU. So the IOMMU group '0'
        // contains "0000:00:02.0" which is the first disk.
        //
        // Verify the iommu group of the first disk.
        let iommu_group = !acpi as i32;
        assert_eq!(
            guest
                .ssh_command(
                    format!("ls /sys/kernel/iommu_groups/{}/devices", iommu_group).as_str()
                )
                .unwrap()
                .trim(),
            "0000:00:02.0"
        );

        // Verify the iommu group of the second disk.
        let iommu_group = if acpi { 1 } else { 2 };
        assert_eq!(
            guest
                .ssh_command(
                    format!("ls /sys/kernel/iommu_groups/{}/devices", iommu_group).as_str()
                )
                .unwrap()
                .trim(),
            "0000:00:03.0"
        );

        // Verify the iommu group of the network card.
        let iommu_group = if acpi { 2 } else { 3 };
        assert_eq!(
            guest
                .ssh_command(
                    format!("ls /sys/kernel/iommu_groups/{}/devices", iommu_group).as_str()
                )
                .unwrap()
                .trim(),
            "0000:00:04.0"
        );
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    handle_child_output(r, &output);
}

fn get_reboot_count(guest: &Guest) -> u32 {
    guest
        .ssh_command("sudo last | grep -c reboot")
        .unwrap()
        .trim()
        .parse::<u32>()
        .unwrap_or_default()
}

fn enable_guest_watchdog(guest: &Guest, watchdog_sec: u32) {
    // Check for PCI device
    assert!(guest
        .does_device_vendor_pair_match("0x1063", "0x1af4")
        .unwrap_or_default());

    // Enable systemd watchdog
    guest
        .ssh_command(&format!(
            "echo RuntimeWatchdogSec={}s | sudo tee -a /etc/systemd/system.conf",
            watchdog_sec
        ))
        .unwrap();
}

fn snapshot_and_check_events(api_socket: &str, snapshot_dir: &str, event_path: &str) {
    // Pause the VM
    assert!(remote_command(api_socket, "pause", None));
    let latest_events = [
        &MetaEvent {
            event: "pausing".to_string(),
            device_id: None,
        },
        &MetaEvent {
            event: "paused".to_string(),
            device_id: None,
        },
    ];
    assert!(check_latest_events_exact(&latest_events, event_path));

    // Take a snapshot from the VM
    assert!(remote_command(
        api_socket,
        "snapshot",
        Some(format!("file://{}", snapshot_dir).as_str()),
    ));

    // Wait to make sure the snapshot is completed
    thread::sleep(std::time::Duration::new(10, 0));

    let latest_events = [
        &MetaEvent {
            event: "snapshotting".to_string(),
            device_id: None,
        },
        &MetaEvent {
            event: "snapshotted".to_string(),
            device_id: None,
        },
    ];
    assert!(check_latest_events_exact(&latest_events, event_path));
}

#[allow(clippy::too_many_arguments)]
fn _test_restore_from_config(
    binary_path: &str,
    guest: &Guest,
    snapshot_dir: String,
    vsock_conf: Option<VsockConfig>,
    net_conf: Option<Vec<NetConfig>>,
    disk_conf: Option<Vec<DiskConfig>>,
    pmem_conf: Option<Vec<PmemConfig>>,
    fs_conf: Option<Vec<FsConfig>>,
) {
    let api_socket_restored = format!("{}.2", temp_api_path(&guest.tmp_dir));
    let event_path_restored = format!("{}.2", temp_event_monitor_path(&guest.tmp_dir));
    let console_text = String::from("On a branch floating down river a cricket, singing.");

    let restore_config = RestoreConfig {
        source_url: PathBuf::from(format!("file://{}", snapshot_dir)),
        vsock: vsock_conf.clone(),
        net: net_conf.clone(),
        disks: disk_conf.clone(),
        pmem: pmem_conf.clone(),
        fs: fs_conf.clone(),
        ..Default::default()
    };

    // Start the hypervisor
    let mut child = GuestCommand::new_with_binary_path(guest, binary_path)
        .args(["--api-socket", &api_socket_restored])
        .args([
            "--event-monitor",
            format!("path={}", event_path_restored).as_str(),
        ])
        .capture_output()
        .spawn()
        .unwrap();

    thread::sleep(std::time::Duration::new(1, 0));

    // Restore the VM from the snapshot
    let body = serde_json::to_string(&restore_config).unwrap();
    curl_command(
        api_socket_restored.as_str(),
        "PUT",
        "http://localhost/api/v1/vm.restore",
        Some(body.as_str()),
    );
    // Wait for the VM to be restored
    thread::sleep(std::time::Duration::new(10, 0));
    let r = std::panic::catch_unwind(|| {
        // Automatically restart the VM after it has been restored
        let latest_events = [
            &MetaEvent {
                event: "restored".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "resuming".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "resumed".to_string(),
                device_id: None,
            },
        ];
        assert!(check_latest_events_exact(
            &latest_events,
            &event_path_restored
        ));

        let (cmd_success, cmd_output) =
            remote_command_w_output(api_socket_restored.as_str(), "info", None);
        assert!(cmd_success);
        println!(
            "cmd_output: {}",
            String::from_utf8_lossy(&cmd_output).trim()
        );

        guest.check_devices_common(None, Some(&console_text), None);
        if let Some(config) = vsock_conf {
            let socket = String::from(config.socket.to_str().unwrap());
            assert!(String::from_utf8_lossy(&cmd_output)
                .trim()
                .contains(format!("\"socket\":\"{}\"", socket.as_str()).as_str()));
            // Check vsock
            guest.check_devices_common(Some(&socket), None, None);
        }
        if let Some(config_list) = net_conf {
            for config in config_list {
                if let Some(tap_name) = config.tap {
                    // Check tap from RestoreConfig
                    let tap_count = exec_host_command_output(&format!(
                        "ip link | grep -c {}",
                        tap_name.as_str()
                    ));
                    assert_eq!(String::from_utf8_lossy(&tap_count.stdout).trim(), "1");
                    assert!(String::from_utf8_lossy(&cmd_output)
                        .trim()
                        .contains(format!("\"tap\":\"{}\"", tap_name.as_str()).as_str()));
                }
            }
        }
        if let Some(config_list) = disk_conf {
            for config in config_list {
                if let Some(path) = config.path {
                    assert!(String::from_utf8_lossy(&cmd_output)
                        .trim()
                        .contains(format!("\"path\":\"{}\"", path.to_str().unwrap()).as_str()));
                }
            }
        }
        if let Some(config_list) = pmem_conf {
            for (index, config) in config_list.iter().enumerate() {
                let pmem_path = format!("/dev/pmem{}", index);

                let pmem_file = String::from(config.file.to_str().unwrap());
                assert!(String::from_utf8_lossy(&cmd_output)
                    .trim()
                    .contains(format!("\"file\":\"{}\"", pmem_file.as_str()).as_str()));
                // Check pmem
                guest.check_devices_common(None, None, Some(&pmem_path));
            }
        }
        if let Some(config_list) = fs_conf {
            for config in config_list {
                if let Some(backend) = config.backendfs_config.as_ref() {
                    assert!(String::from_utf8_lossy(&cmd_output).trim().contains(
                        format!("\"shared_dir\":\"{}\"", backend.shared_dir.as_str()).as_str()
                    ));
                }
            }
        }
    });
    // Shutdown the target VM and check console output
    kill_child(&mut child);
    let output = child.wait_with_output().unwrap();
    handle_child_output(r, &output);

    let r = std::panic::catch_unwind(|| {
        assert!(String::from_utf8_lossy(&output.stdout).contains(&console_text));
    });

    handle_child_output(r, &output);
}

fn _test_snapshot_restore_from_different_binary(
    snapshot_binary_path: &str,
    restore_binary_path: &str,
) {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = Guest::new(Box::new(focal));
    let kernel_path = direct_kernel_boot_path();

    let api_socket_source = format!("{}.1", temp_api_path(&guest.tmp_dir));
    let vsock_id = "_vsock0";

    let net_id = "net123";
    let net_params = format!(
        "id={},tap=,mac={},ip={},mask=255.255.255.0",
        net_id, guest.network.guest_mac, guest.network.host_ip
    );

    let socket = temp_vsock_path(&guest.tmp_dir);
    let event_path = temp_event_monitor_path(&guest.tmp_dir);

    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");
    // Prepare native virtiofs
    let mut shared_dir = workload_path.clone();
    shared_dir.push("shared_dir");
    let fs_params = format!(
        "id=myfs0,tag=myfs,native=true,shared_dir={},cache=always,num_queues=1,queue_size=1024",
        shared_dir.to_str().unwrap()
    );

    // check_devices_common() needs two disks, we need to mount a empty disk here.
    let mut blk_file_path = workload_path.clone();
    blk_file_path.push("blk.img");

    let mut child = GuestCommand::new_with_binary_path(&guest, snapshot_binary_path)
        .args(["--api-socket", &api_socket_source])
        .args(["--event-monitor", format!("path={}", event_path).as_str()])
        .args(["--cpus", "boot=1"])
        .args(["--memory", "size=1024M,hotplug_method=acpi,hotplug_size=4G"])
        .args(["--kernel", kernel_path.to_str().unwrap()])
        .args([
            "--disk",
            format!(
                "id=cloudinit,path={}",
                guest.disk_config.disk(DiskType::CloudInit).unwrap()
            )
            .as_str(),
            format!("id=blk-0,path={}", blk_file_path.to_str().unwrap()).as_str(),
        ])
        .args([
            "--pmem",
            format!(
                "file={},size={}",
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
                fs::metadata(guest.disk_config.disk(DiskType::OperatingSystem).unwrap())
                    .unwrap()
                    .len()
            )
            .as_str(),
        ])
        .args([
            "--cmdline",
            DIRECT_KERNEL_BOOT_CMDLINE
                .replace("vda1", "pmem0p1")
                .as_str(),
        ])
        .args(["--fs", fs_params.as_str()])
        .args(["--serial", "tty", "--console", "tty"])
        .args(["--net", net_params.as_str()])
        .args([
            "--vsock",
            format!("cid=3,id={},socket={}", vsock_id, socket).as_str(),
        ])
        .capture_output()
        .spawn()
        .unwrap();

    let console_text = String::from("On a branch floating down river a cricket, singing.");
    // Create the snapshot directory
    let snapshot_dir = temp_snapshot_dir_path(&guest.tmp_dir);

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        // Check the number of vCPUs
        assert_eq!(guest.get_cpu_count().unwrap_or_default(), 1);

        // Check the guest virtio-devices, e.g. block, rng, vsock, console, and net
        guest.check_devices_common(Some(&socket), Some(&console_text), None);

        snapshot_and_check_events(
            api_socket_source.as_str(),
            snapshot_dir.as_str(),
            event_path.as_str(),
        );
    });

    // Shutdown the source VM and check console output
    kill_child(&mut child);
    let output = child.wait_with_output().unwrap();
    handle_child_output(r, &output);

    let r = std::panic::catch_unwind(|| {
        assert!(String::from_utf8_lossy(&output.stdout).contains(&console_text));
    });

    handle_child_output(r, &output);

    // Remove the vsock socket file.
    Command::new("rm")
        .arg("-f")
        .arg(socket.as_str())
        .output()
        .unwrap();

    // Prepare restore config
    // vsock config
    let socket_restored = format!("{}.2", temp_vsock_path(&guest.tmp_dir));
    let vsock_config =
        VsockConfig::parse(format!("cid=3,id={},socket={}", vsock_id, socket_restored).as_str())
            .unwrap();

    // net config
    let tap_name_restored = "restore-tap";
    let net_params_restored = format!(
        "id={},tap={},mac={},ip={},mask=255.255.255.0",
        net_id, tap_name_restored, guest.network.guest_mac, guest.network.host_ip
    );
    let net_config = NetConfig::parse(net_params_restored.as_str()).unwrap();

    // Pre-create restore-tap since v1.0.9 can't create a named tap
    assert!(exec_host_command_status(
        format!("sudo ip tuntap add {} mode tap", tap_name_restored).as_str()
    )
    .success());
    assert!(exec_host_command_status(
        format!(
            "sudo ip link set dev {} address {}",
            tap_name_restored, guest.network.guest_mac
        )
        .as_str()
    )
    .success());
    assert!(exec_host_command_status(
        format!(
            "sudo ip addr add {}/24 dev {}",
            guest.network.host_ip, tap_name_restored
        )
        .as_str()
    )
    .success());
    assert!(exec_host_command_status(
        format!("sudo ip link set {} up", tap_name_restored).as_str()
    )
    .success());

    // disk config
    let mut workload_path = dirs::home_dir().unwrap();
    workload_path.push("workloads");
    let mut blk_file_path = workload_path.clone();
    blk_file_path.push("blk.img");

    let disk_path_restored =
        String::from(guest.tmp_dir.as_path().join("blk2.img").to_str().unwrap());
    rate_limited_copy(blk_file_path, &disk_path_restored)
        .expect("copying of OS source disk image failed");
    let disk_params_restored = format!("id=blk-0,path={}", disk_path_restored);
    let disk_config = DiskConfig::parse(disk_params_restored.as_str()).unwrap();

    // fs config
    let mut shared_dir_restored = workload_path.clone();
    shared_dir_restored.push("restored_shared_dir");
    let fs_params = format!(
        "id=myfs0,tag=myfs,native=true,shared_dir={},cache=always,num_queues=1,queue_size=1024",
        shared_dir_restored.to_str().unwrap()
    );
    let fs_config = FsConfig::parse(fs_params.as_str()).unwrap();

    _test_restore_from_config(
        restore_binary_path,
        &guest,
        snapshot_dir,
        Some(vsock_config),
        Some(vec![net_config]),
        Some(vec![disk_config]),
        None,
        Some(vec![fs_config]),
    );
    assert!(
        exec_host_command_status(format!("sudo ip link del {}", tap_name_restored).as_str())
            .success()
    );
}

mod common_parallel {
    use std::{fs::OpenOptions, io::SeekFrom};

    use crate::*;

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_bionic_hypervisor_fw() {
        test_simple_launch(fw_path(FwType::RustHypervisorFirmware), BIONIC_IMAGE_NAME)
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_focal_hypervisor_fw() {
        test_simple_launch(fw_path(FwType::RustHypervisorFirmware), FOCAL_IMAGE_NAME)
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_bionic_ovmf() {
        test_simple_launch(fw_path(FwType::Ovmf), BIONIC_IMAGE_NAME)
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_focal_ovmf() {
        test_simple_launch(fw_path(FwType::Ovmf), FOCAL_IMAGE_NAME)
    }

    #[cfg(target_arch = "x86_64")]
    fn test_simple_launch(fw_path: String, disk_path: &str) {
        let disk_config = Box::new(UbuntuDiskConfig::new(disk_path.to_string()));
        let guest = Guest::new(disk_config);
        let event_path = temp_event_monitor_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", fw_path.as_str()])
            .default_disks()
            .default_net()
            .args(["--serial", "tty", "--console", "off"])
            .args(["--event-monitor", format!("path={}", event_path).as_str()])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(Some(120)).unwrap();

            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 1);
            assert_eq!(guest.get_initial_apicid().unwrap_or(1), 0);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
            assert_eq!(guest.get_pci_bridge_class().unwrap_or_default(), "0x060000");

            let expected_sequential_events = [
                &MetaEvent {
                    event: "starting".to_string(),
                    device_id: None,
                },
                &MetaEvent {
                    event: "booting".to_string(),
                    device_id: None,
                },
                &MetaEvent {
                    event: "booted".to_string(),
                    device_id: None,
                },
                &MetaEvent {
                    event: "activated".to_string(),
                    device_id: Some("_disk0".to_string()),
                },
                &MetaEvent {
                    event: "reset".to_string(),
                    device_id: Some("_disk0".to_string()),
                },
            ];
            assert!(check_sequential_events(
                &expected_sequential_events,
                &event_path
            ));

            // It's been observed on the Bionic image that udev and snapd
            // services can cause some delay in the VM's shutdown. Disabling
            // them improves the reliability of this test.
            let _ = guest.ssh_command("sudo systemctl disable udev");
            let _ = guest.ssh_command("sudo systemctl stop udev");
            let _ = guest.ssh_command("sudo systemctl disable snapd");
            let _ = guest.ssh_command("sudo systemctl stop snapd");

            guest.ssh_command("sudo poweroff").unwrap();
            thread::sleep(std::time::Duration::new(10, 0));
            let latest_events = [
                &MetaEvent {
                    event: "shutdown".to_string(),
                    device_id: None,
                },
                &MetaEvent {
                    event: "deleted".to_string(),
                    device_id: None,
                },
                &MetaEvent {
                    event: "shutdown".to_string(),
                    device_id: None,
                },
            ];
            assert!(check_latest_events_exact(&latest_events, &event_path));
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_multi_cpu() {
        let bionic = UbuntuDiskConfig::new(BIONIC_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(bionic));
        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=2,max=4"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .capture_output()
            .default_disks()
            .default_net();

        let mut child = cmd.spawn().unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(Some(120)).unwrap();

            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 2);

            #[cfg(target_arch = "x86_64")]
            assert_eq!(
                guest
                    .ssh_command(r#"dmesg | grep "smpboot: Allowing" | sed "s/\[\ *[0-9.]*\] //""#)
                    .unwrap()
                    .trim(),
                "smpboot: Allowing 4 CPUs, 2 hotplug CPUs"
            );
            #[cfg(target_arch = "aarch64")]
            assert_eq!(
                guest
                    .ssh_command(r#"dmesg | grep "smp: Brought up" | sed "s/\[\ *[0-9.]*\] //""#)
                    .unwrap()
                    .trim(),
                "smp: Brought up 1 node, 2 CPUs"
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_cpu_topology_421() {
        test_cpu_topology(4, 2, 1, false);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_cpu_topology_142() {
        test_cpu_topology(1, 4, 2, false);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_cpu_topology_262() {
        test_cpu_topology(2, 6, 2, false);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    #[cfg(not(feature = "mshv"))]
    fn test_cpu_physical_bits() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let max_phys_bits: u8 = 36;
        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", &format!("max_phys_bits={}", max_phys_bits)])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert!(
                    guest
                        .ssh_command("lscpu | grep \"Address sizes:\" | cut -f 2 -d \":\" | sed \"s# *##\" | cut -f 1 -d \" \"")
                        .unwrap()
                        .trim()
                        .parse::<u8>()
                        .unwrap_or(max_phys_bits + 1) <= max_phys_bits,
                );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_cpu_affinity() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        // We need the host to have at least 4 CPUs if we want to be able
        // to run this test.
        let host_cpus_count = exec_host_command_output("nproc");
        assert!(
            String::from_utf8_lossy(&host_cpus_count.stdout)
                .trim()
                .parse::<u8>()
                .unwrap_or(0)
                >= 4
        );

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=2,affinity=[0@[0,2],1@[1,3]]"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();
            let pid = child.id();
            let taskset_vcpu0 = exec_host_command_output(format!("taskset -pc $(ps -T -p {} | grep vcpu0 | xargs | cut -f 2 -d \" \") | cut -f 6 -d \" \"", pid).as_str());
            assert_eq!(String::from_utf8_lossy(&taskset_vcpu0.stdout).trim(), "0,2");
            let taskset_vcpu1 = exec_host_command_output(format!("taskset -pc $(ps -T -p {} | grep vcpu1 | xargs | cut -f 2 -d \" \") | cut -f 6 -d \" \"", pid).as_str());
            assert_eq!(String::from_utf8_lossy(&taskset_vcpu1.stdout).trim(), "1,3");
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_large_vm() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=48"])
            .args(["--memory", "size=5120M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .capture_output()
            .default_disks()
            .default_net();

        let mut child = cmd.spawn().unwrap();

        guest.wait_vm_boot(None).unwrap();

        let r = std::panic::catch_unwind(|| {
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 48);
            assert_eq!(
                guest
                    .ssh_command("lscpu | grep \"On-line\" | cut -f 2 -d \":\" | sed \"s# *##\"")
                    .unwrap()
                    .trim(),
                "0-47"
            );

            assert!(guest.get_total_memory().unwrap_or_default() > 5_000_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_huge_memory() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=1"])
            .args(["--memory", "size=128G"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .capture_output()
            .default_disks()
            .default_net();

        let mut child = cmd.spawn().unwrap();

        guest.wait_vm_boot(Some(120)).unwrap();

        let r = std::panic::catch_unwind(|| {
            assert!(guest.get_total_memory().unwrap_or_default() > 128_000_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_power_button() {
        _test_power_button(false);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_user_defined_memory_regions() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=0,hotplug_method=virtio-mem"])
            .args([
                "--memory-zone",
                "id=mem0,size=1G,hotplug_size=2G",
                "id=mem1,size=1G,file=/dev/shm",
                "id=mem2,size=1G,host_numa_node=0,hotplug_size=2G",
            ])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--api-socket", &api_socket])
            .capture_output()
            .default_disks()
            .default_net()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert!(guest.get_total_memory().unwrap_or_default() > 2_880_000);

            guest.enable_memory_hotplug();

            resize_zone_command(&api_socket, "mem0", "3G");
            thread::sleep(std::time::Duration::new(5, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 4_800_000);
            resize_zone_command(&api_socket, "mem2", "3G");
            thread::sleep(std::time::Duration::new(5, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 6_720_000);
            resize_zone_command(&api_socket, "mem0", "2G");
            thread::sleep(std::time::Duration::new(5, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 5_760_000);
            resize_zone_command(&api_socket, "mem2", "2G");
            thread::sleep(std::time::Duration::new(5, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 4_800_000);

            // guest.reboot_linux(0, None);
            //
            // // Check the amount of RAM after reboot
            // assert!(guest.get_total_memory().unwrap_or_default() > 4_800_000);
            // assert!(guest.get_total_memory().unwrap_or_default() < 5_760_000);
            //
            // // Check if we can still resize down to the initial 'boot'size
            // resize_zone_command(&api_socket, "mem0", "1G");
            // thread::sleep(std::time::Duration::new(5, 0));
            // assert!(guest.get_total_memory().unwrap_or_default() < 4_800_000);
            // resize_zone_command(&api_socket, "mem2", "1G");
            // thread::sleep(std::time::Duration::new(5, 0));
            // assert!(guest.get_total_memory().unwrap_or_default() < 3_840_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_guest_numa_nodes() {
        _test_guest_numa_nodes(false);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_iommu_segments() {
        let focal_image = FOCAL_IMAGE_NAME.to_string();
        let focal = UbuntuDiskConfig::new(focal_image);
        let guest = Guest::new(Box::new(focal));

        // Prepare another disk file for the virtio-disk device
        let test_disk_path = String::from(
            guest
                .tmp_dir
                .as_path()
                .join("test-disk.raw")
                .to_str()
                .unwrap(),
        );
        assert!(
            exec_host_command_status(format!("truncate {} -s 4M", test_disk_path).as_str())
                .success()
        );
        assert!(
            exec_host_command_status(format!("mkfs.ext4 {}", test_disk_path).as_str()).success()
        );

        let api_socket = temp_api_path(&guest.tmp_dir);
        let mut cmd = GuestCommand::new(&guest);

        cmd.args(["--cpus", "boot=1"])
            .args(["--api-socket", &api_socket])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--platform", "num_pci_segments=16,iommu_segments=[1]"])
            .default_disks()
            .capture_output()
            .default_net();

        let mut child = cmd.spawn().unwrap();

        guest.wait_vm_boot(None).unwrap();

        let r = std::panic::catch_unwind(|| {
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-disk",
                Some(
                    format!(
                        "path={},id=test0,pci_segment=1,iommu=on",
                        test_disk_path.as_str()
                    )
                    .as_str(),
                ),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"test0\",\"bdf\":\"0001:00:01.0\"}"));

            // Check IOMMU setup
            assert!(guest
                .does_device_vendor_pair_match("0x1057", "0x1af4")
                .unwrap_or_default());
            assert_eq!(
                guest
                    .ssh_command("ls /sys/kernel/iommu_groups/0/devices")
                    .unwrap()
                    .trim(),
                "0001:00:01.0"
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_pci_msi() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .capture_output()
            .default_disks()
            .default_net();

        let mut child = cmd.spawn().unwrap();

        guest.wait_vm_boot(None).unwrap();

        #[cfg(target_arch = "x86_64")]
        let grep_cmd = "grep -c PCI-MSI /proc/interrupts";
        #[cfg(target_arch = "aarch64")]
        let grep_cmd = "grep -c ITS-MSI /proc/interrupts";

        let r = std::panic::catch_unwind(|| {
            assert_eq!(
                guest
                    .ssh_command(grep_cmd)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                12
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_virtio_net_ctrl_queue() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--net", guest.default_net_string_w_mtu(3000).as_str()])
            .capture_output()
            .default_disks();

        let mut child = cmd.spawn().unwrap();

        guest.wait_vm_boot(None).unwrap();

        #[cfg(target_arch = "aarch64")]
        let iface = "enp0s4";
        #[cfg(target_arch = "x86_64")]
        let iface = "ens4";

        let r = std::panic::catch_unwind(|| {
            assert_eq!(
                guest
                    .ssh_command(
                        format!("sudo ethtool -K {} rx-gro-hw off && echo success", iface).as_str()
                    )
                    .unwrap()
                    .trim(),
                "success"
            );
            assert_eq!(
                guest
                    .ssh_command(format!("cat /sys/class/net/{}/mtu", iface).as_str())
                    .unwrap()
                    .trim(),
                "3000"
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_pci_multiple_segments() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        // Prepare another disk file for the virtio-disk device
        let test_disk_path = String::from(
            guest
                .tmp_dir
                .as_path()
                .join("test-disk.raw")
                .to_str()
                .unwrap(),
        );
        assert!(
            exec_host_command_status(format!("truncate {} -s 4M", test_disk_path).as_str())
                .success()
        );
        assert!(
            exec_host_command_status(format!("mkfs.ext4 {}", test_disk_path).as_str()).success()
        );

        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--platform", "num_pci_segments=16"])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
                format!("path={},pci_segment=15", test_disk_path).as_str(),
            ])
            .capture_output()
            .default_net();

        let mut child = cmd.spawn().unwrap();

        guest.wait_vm_boot(None).unwrap();

        let grep_cmd = "lspci | grep \"Host bridge\" | wc -l";

        let r = std::panic::catch_unwind(|| {
            // There should be 16 PCI host bridges in the guest.
            assert_eq!(
                guest
                    .ssh_command(grep_cmd)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                16
            );

            // Check both if /dev/vdc exists and if the block size is 4M.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdc | grep -c 4M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Mount the device.
            guest.ssh_command("mkdir mount_image").unwrap();
            guest
                .ssh_command("sudo mount -o rw -t ext4 /dev/vdc mount_image/")
                .unwrap();
            // Grant all users with write permission.
            guest.ssh_command("sudo chmod a+w mount_image/").unwrap();

            // Write something to the device.
            guest
                .ssh_command("sudo echo \"bar\" >> mount_image/foo")
                .unwrap();

            // Check the content of the block device. The file "foo" should
            // contain "bar".
            assert_eq!(
                guest
                    .ssh_command("sudo cat mount_image/foo")
                    .unwrap()
                    .trim(),
                "bar"
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_direct_kernel_boot() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 1);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

            let grep_cmd = if cfg!(target_arch = "x86_64") {
                "grep -c PCI-MSI /proc/interrupts"
            } else {
                "grep -c ITS-MSI /proc/interrupts"
            };
            assert_eq!(
                guest
                    .ssh_command(grep_cmd)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                12
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    fn _test_virtio_block(image_name: &str, disable_io_uring: bool) {
        let focal = UbuntuDiskConfig::new(image_name.to_string());
        let guest = Guest::new(Box::new(focal));

        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let mut blk_file_path = workload_path;
        blk_file_path.push("blk.img");

        let kernel_path = direct_kernel_boot_path();

        let mut cloud_child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=4"])
            .args(["--memory", "size=512M,shared=on"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
                format!(
                    "path={},readonly=on,direct=on,num_queues=4,_disable_io_uring={}",
                    blk_file_path.to_str().unwrap(),
                    disable_io_uring
                )
                .as_str(),
            ])
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check both if /dev/vdc exists and if the block size is 16M.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdc | grep -c 16M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check both if /dev/vdc exists and if this block is RO.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdc | awk '{print $5}'")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check if the number of queues is 4.
            assert_eq!(
                guest
                    .ssh_command("ls -ll /sys/block/vdc/mq | grep ^d | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                4
            );
        });

        let _ = cloud_child.kill();
        let output = cloud_child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_virtio_block() {
        _test_virtio_block(FOCAL_IMAGE_NAME, false)
    }

    #[test]
    fn test_virtio_block_disable_io_uring() {
        _test_virtio_block(FOCAL_IMAGE_NAME, true)
    }

    #[test]
    fn test_virtio_block_qcow2() {
        _test_virtio_block(FOCAL_IMAGE_NAME_QCOW2, false)
    }

    #[test]
    fn test_virtio_block_vhd() {
        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let mut raw_file_path = workload_path.clone();
        let mut vhd_file_path = workload_path;
        raw_file_path.push(FOCAL_IMAGE_NAME);
        vhd_file_path.push(FOCAL_IMAGE_NAME_VHD);

        // Generate VHD file from RAW file
        std::process::Command::new("qemu-img")
            .arg("convert")
            .arg("-p")
            .args(["-f", "raw"])
            .args(["-O", "vpc"])
            .args(["-o", "subformat=fixed"])
            .arg(raw_file_path.to_str().unwrap())
            .arg(vhd_file_path.to_str().unwrap())
            .output()
            .expect("Expect generating VHD image from RAW image");

        _test_virtio_block(FOCAL_IMAGE_NAME_VHD, false)
    }

    #[test]
    fn test_virtio_block_vhdx() {
        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let mut raw_file_path = workload_path.clone();
        let mut vhdx_file_path = workload_path;
        raw_file_path.push(FOCAL_IMAGE_NAME);
        vhdx_file_path.push(FOCAL_IMAGE_NAME_VHDX);

        // Generate dynamic VHDX file from RAW file
        std::process::Command::new("qemu-img")
            .arg("convert")
            .arg("-p")
            .args(["-f", "raw"])
            .args(["-O", "vhdx"])
            .arg(raw_file_path.to_str().unwrap())
            .arg(vhdx_file_path.to_str().unwrap())
            .output()
            .expect("Expect generating dynamic VHDx image from RAW image");

        _test_virtio_block(FOCAL_IMAGE_NAME_VHDX, false)
    }

    #[test]
    fn test_virtio_block_dynamic_vhdx_expand() {
        const VIRTUAL_DISK_SIZE: u64 = 100 << 20;
        const EMPTY_VHDX_FILE_SIZE: u64 = 8 << 20;
        const FULL_VHDX_FILE_SIZE: u64 = 112 << 20;
        const DYNAMIC_VHDX_NAME: &str = "dynamic.vhdx";

        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let mut vhdx_file_path = workload_path;
        vhdx_file_path.push(DYNAMIC_VHDX_NAME);
        let vhdx_path = vhdx_file_path.to_str().unwrap();

        // Generate a 100 MiB dynamic VHDX file
        std::process::Command::new("qemu-img")
            .arg("create")
            .args(["-f", "vhdx"])
            .arg(vhdx_path)
            .arg(VIRTUAL_DISK_SIZE.to_string())
            .output()
            .expect("Expect generating dynamic VHDx image from RAW image");

        // Check if the size matches with empty VHDx file size
        assert_eq!(vhdx_image_size(vhdx_path), EMPTY_VHDX_FILE_SIZE);

        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();

        let mut cloud_child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
                format!("path={}", vhdx_path).as_str(),
            ])
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check both if /dev/vdc exists and if the block size is 100 MiB.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdc | grep -c 100M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Write 100 MB of data to the VHDx disk
            guest
                .ssh_command("sudo dd if=/dev/urandom of=/dev/vdc bs=1M count=100")
                .unwrap();
        });

        // Check if the size matches with expected expanded VHDx file size
        assert_eq!(vhdx_image_size(vhdx_path), FULL_VHDX_FILE_SIZE);

        let _ = cloud_child.kill();
        let output = cloud_child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    fn vhdx_image_size(disk_name: &str) -> u64 {
        std::fs::File::open(disk_name)
            .unwrap()
            .seek(SeekFrom::End(0))
            .unwrap()
    }

    #[test]
    fn test_virtio_block_direct_and_firmware() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        // The OS disk must be copied to a location that is not backed by
        // tmpfs, otherwise the syscall openat(2) with O_DIRECT simply fails
        // with EINVAL because tmpfs doesn't support this flag.
        let mut workloads_path = dirs::home_dir().unwrap();
        workloads_path.push("workloads");
        let os_dir = TempDir::new_in(workloads_path.as_path()).unwrap();
        let mut os_path = os_dir.as_path().to_path_buf();
        os_path.push("osdisk.img");
        rate_limited_copy(
            guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
            os_path.as_path(),
        )
        .expect("copying of OS disk failed");

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", fw_path(FwType::RustHypervisorFirmware).as_str()])
            .args([
                "--disk",
                format!("path={},direct=on", os_path.as_path().to_str().unwrap()).as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
            ])
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(Some(120)).unwrap();
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_vhost_user_net_default() {
        test_vhost_user_net(None, 2, &prepare_vhost_user_net_daemon, false, false)
    }

    #[test]
    fn test_vhost_user_net_named_tap() {
        test_vhost_user_net(
            Some("mytap0"),
            2,
            &prepare_vhost_user_net_daemon,
            false,
            false,
        )
    }

    #[test]
    fn test_vhost_user_net_existing_tap() {
        test_vhost_user_net(
            Some("vunet-tap0"),
            2,
            &prepare_vhost_user_net_daemon,
            false,
            false,
        )
    }

    #[test]
    fn test_vhost_user_net_multiple_queues() {
        test_vhost_user_net(None, 4, &prepare_vhost_user_net_daemon, false, false)
    }

    #[test]
    fn test_vhost_user_net_tap_multiple_queues() {
        test_vhost_user_net(
            Some("vunet-tap1"),
            4,
            &prepare_vhost_user_net_daemon,
            false,
            false,
        )
    }

    #[test]
    fn test_vhost_user_net_host_mac() {
        test_vhost_user_net(None, 2, &prepare_vhost_user_net_daemon, true, false)
    }

    #[test]
    fn test_vhost_user_net_client_mode() {
        test_vhost_user_net(None, 2, &prepare_vhost_user_net_daemon, false, true)
    }

    #[test]
    fn test_vhost_user_blk_default() {
        test_vhost_user_blk(2, false, false, Some(&prepare_vubd))
    }

    #[test]
    fn test_vhost_user_blk_readonly() {
        test_vhost_user_blk(1, true, false, Some(&prepare_vubd))
    }

    #[test]
    fn test_vhost_user_blk_direct() {
        test_vhost_user_blk(1, false, true, Some(&prepare_vubd))
    }

    #[test]
    fn test_boot_from_vhost_user_blk_default() {
        test_boot_from_vhost_user_blk(1, false, false, Some(&prepare_vubd))
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_split_irqchip() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(
                guest
                    .ssh_command("grep -c IO-APIC.*timer /proc/interrupts || true")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(1),
                0
            );
            assert_eq!(
                guest
                    .ssh_command("grep -c IO-APIC.*cascade /proc/interrupts || true")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(1),
                0
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_dmi_serial_number() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--platform", "serial_number=a=b;c=d"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(
                guest
                    .ssh_command("sudo cat /sys/class/dmi/id/product_serial")
                    .unwrap()
                    .trim(),
                "a=b;c=d"
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_dmi_uuid() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--platform", "uuid=1e8aa28a-435d-4027-87f4-40dceff1fa0a"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(
                guest
                    .ssh_command("sudo cat /sys/class/dmi/id/product_uuid")
                    .unwrap()
                    .trim(),
                "1e8aa28a-435d-4027-87f4-40dceff1fa0a"
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_dmi_oem_strings() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let s1 = "io.systemd.credential:xx=yy";
        let s2 = "This is a test string";

        let oem_strings = format!("oem_strings=[{},{}]", s1, s2);

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--platform", &oem_strings])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(
                guest
                    .ssh_command("sudo dmidecode --oem-string count")
                    .unwrap()
                    .trim(),
                "2"
            );

            assert_eq!(
                guest
                    .ssh_command("sudo dmidecode --oem-string 1")
                    .unwrap()
                    .trim(),
                s1
            );

            assert_eq!(
                guest
                    .ssh_command("sudo dmidecode --oem-string 2")
                    .unwrap()
                    .trim(),
                s2
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_virtio_fs() {
        _test_virtio_fs(&prepare_virtiofsd, false, None)
    }

    #[test]
    fn test_virtio_fs_hotplug() {
        _test_virtio_fs(&prepare_virtiofsd, true, None)
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_virtio_fs_multi_segment_hotplug() {
        _test_virtio_fs(&prepare_virtiofsd, true, Some(15))
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_virtio_fs_multi_segment() {
        _test_virtio_fs(&prepare_virtiofsd, false, Some(15))
    }

    #[test]
    fn test_native_virtio_fs() {
        _test_native_virtio_fs(false, None)
    }

    #[test]
    fn test_native_virtio_fs_hotplug() {
        _test_native_virtio_fs(true, None)
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_native_virtio_fs_multi_segment() {
        _test_native_virtio_fs(false, Some(15))
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_native_virtio_fs_multi_segment_hotplug() {
        _test_native_virtio_fs(true, Some(15))
    }

    #[test]
    fn test_virtio_pmem_persist_writes() {
        test_virtio_pmem(false, false)
    }

    #[test]
    fn test_virtio_pmem_discard_writes() {
        test_virtio_pmem(true, false)
    }

    #[test]
    fn test_virtio_pmem_with_size() {
        test_virtio_pmem(true, true)
    }

    #[test]
    fn test_boot_from_virtio_pmem() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
            ])
            .default_net()
            .args([
                "--pmem",
                format!(
                    "file={},size={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
                    fs::metadata(guest.disk_config.disk(DiskType::OperatingSystem).unwrap())
                        .unwrap()
                        .len()
                )
                .as_str(),
            ])
            .args([
                "--cmdline",
                DIRECT_KERNEL_BOOT_CMDLINE
                    .replace("vda1", "pmem0p1")
                    .as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Simple checks to validate the VM booted properly
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 1);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_multiple_network_interfaces() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args([
                "--net",
                guest.default_net_string().as_str(),
                "tap=,mac=8a:6b:6f:5a:de:ac,ip=192.168.3.1,mask=255.255.255.0",
                "tap=mytap1,mac=fe:1f:9e:e1:60:f2,ip=192.168.4.1,mask=255.255.255.0",
            ])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            let tap_count = exec_host_command_output("ip link | grep -c mytap1");
            assert_eq!(String::from_utf8_lossy(&tap_count.stdout).trim(), "1");

            // 3 network interfaces + default localhost ==> 4 interfaces
            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                4
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "aarch64")]
    fn test_pmu_on() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Test that PMU exists.
            assert_eq!(
                guest
                    .ssh_command(GREP_PMU_IRQ_CMD)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_serial_off() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--serial", "off"])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Test that there is no ttyS0
            assert_eq!(
                guest
                    .ssh_command(GREP_SERIAL_IRQ_CMD)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(1),
                0
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_serial_null() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut cmd = GuestCommand::new(&guest);
        #[cfg(target_arch = "x86_64")]
        let console_str: &str = "console=ttyS0";
        #[cfg(target_arch = "aarch64")]
        let console_str: &str = "console=ttyAMA0";

        cmd.args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args([
                "--cmdline",
                DIRECT_KERNEL_BOOT_CMDLINE
                    .replace("console=hvc0 ", console_str)
                    .as_str(),
            ])
            .default_disks()
            .default_net()
            .args(["--serial", "null"])
            .args(["--console", "off"])
            .capture_output();

        let mut child = cmd.spawn().unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Test that there is a ttyS0
            assert_eq!(
                guest
                    .ssh_command(GREP_SERIAL_IRQ_CMD)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        let r = std::panic::catch_unwind(|| {
            assert!(!String::from_utf8_lossy(&output.stdout).contains(CONSOLE_TEST_STRING));
        });

        handle_child_output(r, &output);
    }

    #[test]
    fn test_serial_tty() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        #[cfg(target_arch = "x86_64")]
        let console_str: &str = "console=ttyS0";
        #[cfg(target_arch = "aarch64")]
        let console_str: &str = "console=ttyAMA0";

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args([
                "--cmdline",
                DIRECT_KERNEL_BOOT_CMDLINE
                    .replace("console=hvc0 ", console_str)
                    .as_str(),
            ])
            .default_disks()
            .default_net()
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Test that there is a ttyS0
            assert_eq!(
                guest
                    .ssh_command(GREP_SERIAL_IRQ_CMD)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );
        });

        // This sleep is needed to wait for the login prompt
        thread::sleep(std::time::Duration::new(2, 0));

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&output.stdout).contains(CONSOLE_TEST_STRING));
        });

        handle_child_output(r, &output);
    }

    #[test]
    fn test_serial_file() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let serial_path = guest.tmp_dir.as_path().join("serial-output");
        #[cfg(target_arch = "x86_64")]
        let console_str: &str = "console=ttyS0";
        #[cfg(target_arch = "aarch64")]
        let console_str: &str = "console=ttyAMA0";

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args([
                "--cmdline",
                DIRECT_KERNEL_BOOT_CMDLINE
                    .replace("console=hvc0 ", console_str)
                    .as_str(),
            ])
            .default_disks()
            .default_net()
            .args([
                "--serial",
                format!("file={}", serial_path.to_str().unwrap()).as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Test that there is a ttyS0
            assert_eq!(
                guest
                    .ssh_command(GREP_SERIAL_IRQ_CMD)
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            guest.ssh_command("sudo shutdown -h now").unwrap();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(20));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        let r = std::panic::catch_unwind(|| {
            // Check that the cloud-hypervisor binary actually terminated
            assert!(output.status.success());

            // Do this check after shutdown of the VM as an easy way to ensure
            // all writes are flushed to disk
            let mut f = std::fs::File::open(serial_path).unwrap();
            let mut buf = String::new();
            f.read_to_string(&mut buf).unwrap();
            assert!(buf.contains(CONSOLE_TEST_STRING));
        });

        handle_child_output(r, &output);
    }

    #[test]
    fn test_pty_interaction() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);
        let serial_option = if cfg!(target_arch = "x86_64") {
            " console=ttyS0"
        } else {
            " console=ttyAMA0"
        };
        let cmdline = DIRECT_KERNEL_BOOT_CMDLINE.to_owned() + serial_option;

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", &cmdline])
            .default_disks()
            .default_net()
            .args(["--serial", "null"])
            .args(["--console", "pty"])
            .args(["--api-socket", &api_socket])
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();
            // Get pty fd for console
            let console_path = get_pty_path(&api_socket, "console");
            // TODO: Get serial pty test working
            let mut cf = std::fs::OpenOptions::new()
                .write(true)
                .read(true)
                .open(console_path)
                .unwrap();

            // Some dumb sleeps but we don't want to write
            // before the console is up and we don't want
            // to try and write the next line before the
            // login process is ready.
            thread::sleep(std::time::Duration::new(5, 0));
            assert_eq!(cf.write(b"cloud\n").unwrap(), 6);
            thread::sleep(std::time::Duration::new(2, 0));
            assert_eq!(cf.write(b"cloud123\n").unwrap(), 9);
            thread::sleep(std::time::Duration::new(2, 0));
            assert_eq!(cf.write(b"echo test_pty_console\n").unwrap(), 22);
            thread::sleep(std::time::Duration::new(2, 0));

            // read pty and ensure they have a login shell
            // some fairly hacky workarounds to avoid looping
            // forever in case the channel is blocked getting output
            let ptyc = pty_read(cf);
            let mut empty = 0;
            let mut prev = String::new();
            loop {
                thread::sleep(std::time::Duration::new(2, 0));
                match ptyc.try_recv() {
                    Ok(line) => {
                        empty = 0;
                        prev = prev + &line;
                        if prev.contains("test_pty_console") {
                            break;
                        }
                    }
                    Err(mpsc::TryRecvError::Empty) => {
                        empty += 1;
                        assert!(empty <= 5, "No login on pty");
                    }
                    _ => panic!("No login on pty"),
                }
            }

            guest.ssh_command("sudo shutdown -h now").unwrap();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(20));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        let r = std::panic::catch_unwind(|| {
            // Check that the cloud-hypervisor binary actually terminated
            assert!(output.status.success())
        });
        handle_child_output(r, &output);
    }

    #[test]
    fn test_virtio_console() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--console", "tty"])
            .args(["--serial", "null"])
            .capture_output()
            .spawn()
            .unwrap();

        let text = String::from("On a branch floating down river a cricket, singing.");
        let cmd = format!("echo {} | sudo tee /dev/hvc0", text);

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert!(guest
                .does_device_vendor_pair_match("0x1043", "0x1af4")
                .unwrap_or_default());

            guest.ssh_command(&cmd).unwrap();
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&output.stdout).contains(&text));
        });

        handle_child_output(r, &output);
    }

    #[test]
    fn test_console_file() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let console_path = guest.tmp_dir.as_path().join("console-output");
        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args([
                "--console",
                format!("file={}", console_path.to_str().unwrap()).as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        guest.wait_vm_boot(None).unwrap();

        guest.ssh_command("sudo shutdown -h now").unwrap();

        let _ = child.wait_timeout(std::time::Duration::from_secs(20));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let r = std::panic::catch_unwind(|| {
            // Check that the cloud-hypervisor binary actually terminated
            assert!(output.status.success());

            // Do this check after shutdown of the VM as an easy way to ensure
            // all writes are flushed to disk
            let mut f = std::fs::File::open(console_path).unwrap();
            let mut buf = String::new();
            f.read_to_string(&mut buf).unwrap();

            if !buf.contains(CONSOLE_TEST_STRING) {
                eprintln!(
                    "\n\n==== Console file output ====\n\n{}\n\n==== End console file output ====",
                    buf
                );
            }
            assert!(buf.contains(CONSOLE_TEST_STRING));
        });

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    #[cfg(not(feature = "mshv"))]
    #[ignore = "See #4324"]
    // The VFIO integration test starts cloud-hypervisor guest with 3 TAP
    // backed networking interfaces, bound through a simple bridge on the host.
    // So if the nested cloud-hypervisor succeeds in getting a directly
    // assigned interface from its cloud-hypervisor host, we should be able to
    // ssh into it, and verify that it's running with the right kernel command
    // line (We tag the command line from cloud-hypervisor for that purpose).
    // The third device is added to validate that hotplug works correctly since
    // it is being added to the L2 VM through hotplugging mechanism.
    // Also, we pass-through a vitio-blk device to the L2 VM to test the 32-bit
    // vfio device support
    fn test_vfio() {
        setup_vfio_network_interfaces();

        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new_from_ip_range(Box::new(focal), "172.18", 0);

        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let kernel_path = direct_kernel_boot_path();

        let mut vfio_path = workload_path.clone();
        vfio_path.push("vfio");

        let mut cloud_init_vfio_base_path = vfio_path.clone();
        cloud_init_vfio_base_path.push("cloudinit.img");

        // We copy our cloudinit into the vfio mount point, for the nested
        // cloud-hypervisor guest to use.
        rate_limited_copy(
            guest.disk_config.disk(DiskType::CloudInit).unwrap(),
            &cloud_init_vfio_base_path,
        )
        .expect("copying of cloud-init disk failed");

        let mut vfio_disk_path = workload_path.clone();
        vfio_disk_path.push("vfio.img");

        // Create the vfio disk image
        let output = Command::new("mkfs.ext4")
            .arg("-d")
            .arg(vfio_path.to_str().unwrap())
            .arg(vfio_disk_path.to_str().unwrap())
            .arg("2g")
            .output()
            .unwrap();
        if !output.status.success() {
            eprintln!("{}", String::from_utf8_lossy(&output.stderr));
            panic!("mkfs.ext4 command generated an error");
        }

        let mut blk_file_path = workload_path;
        blk_file_path.push("blk.img");

        let vfio_tap0 = "vfio-tap0";
        let vfio_tap1 = "vfio-tap1";
        let vfio_tap2 = "vfio-tap2";
        let vfio_tap3 = "vfio-tap3";

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=4"])
            .args(["--memory", "size=2G,hugepages=on,shared=on"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
                format!("path={}", vfio_disk_path.to_str().unwrap()).as_str(),
                format!("path={},iommu=on", blk_file_path.to_str().unwrap()).as_str(),
            ])
            .args([
                "--cmdline",
                format!(
                    "{} kvm-intel.nested=1 vfio_iommu_type1.allow_unsafe_interrupts",
                    DIRECT_KERNEL_BOOT_CMDLINE
                )
                .as_str(),
            ])
            .args([
                "--net",
                format!("tap={},mac={}", vfio_tap0, guest.network.guest_mac).as_str(),
                format!(
                    "tap={},mac={},iommu=on",
                    vfio_tap1, guest.network.l2_guest_mac1
                )
                .as_str(),
                format!(
                    "tap={},mac={},iommu=on",
                    vfio_tap2, guest.network.l2_guest_mac2
                )
                .as_str(),
                format!(
                    "tap={},mac={},iommu=on",
                    vfio_tap3, guest.network.l2_guest_mac3
                )
                .as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        thread::sleep(std::time::Duration::new(30, 0));

        let r = std::panic::catch_unwind(|| {
            guest.ssh_command_l1("sudo systemctl start vfio").unwrap();
            thread::sleep(std::time::Duration::new(120, 0));

            // We booted our cloud hypervisor L2 guest with a "VFIOTAG" tag
            // added to its kernel command line.
            // Let's ssh into it and verify that it's there. If it is it means
            // we're in the right guest (The L2 one) because the QEMU L1 guest
            // does not have this command line tag.
            assert_eq!(
                guest
                    .ssh_command_l2_1("grep -c VFIOTAG /proc/cmdline")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Let's also verify from the second virtio-net device passed to
            // the L2 VM.
            assert_eq!(
                guest
                    .ssh_command_l2_2("grep -c VFIOTAG /proc/cmdline")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check the amount of PCI devices appearing in L2 VM.
            assert_eq!(
                guest
                    .ssh_command_l2_1("ls /sys/bus/pci/devices | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                8,
            );

            // Check both if /dev/vdc exists and if the block size is 16M in L2 VM
            assert_eq!(
                guest
                    .ssh_command_l2_1("lsblk | grep vdc | grep -c 16M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Hotplug an extra virtio-net device through L2 VM.
            guest
                .ssh_command_l1(
                    "echo 0000:00:09.0 | sudo tee /sys/bus/pci/devices/0000:00:09.0/driver/unbind",
                )
                .unwrap();
            guest
                .ssh_command_l1("echo 0000:00:09.0 | sudo tee /sys/bus/pci/drivers/vfio-pci/bind")
                .unwrap();
            let vfio_hotplug_output = guest
                .ssh_command_l1(
                    "sudo /mnt/ch-remote \
                 --api-socket=/tmp/ch_api.sock \
                 add-device path=/sys/bus/pci/devices/0000:00:09.0,id=vfio123",
                )
                .unwrap();
            assert!(vfio_hotplug_output.contains("{\"id\":\"vfio123\",\"bdf\":\"0000:00:08.0\"}"));

            thread::sleep(std::time::Duration::new(10, 0));

            // Let's also verify from the third virtio-net device passed to
            // the L2 VM. This third device has been hotplugged through the L2
            // VM, so this is our way to validate hotplug works for VFIO PCI.
            assert_eq!(
                guest
                    .ssh_command_l2_3("grep -c VFIOTAG /proc/cmdline")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check the amount of PCI devices appearing in L2 VM.
            // There should be one more device than before, raising the count
            // up to 9 PCI devices.
            assert_eq!(
                guest
                    .ssh_command_l2_1("ls /sys/bus/pci/devices | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                9,
            );

            // Let's now verify that we can correctly remove the virtio-net
            // device through the "remove-device" command responsible for
            // unplugging VFIO devices.
            guest
                .ssh_command_l1(
                    "sudo /mnt/ch-remote \
                 --api-socket=/tmp/ch_api.sock \
                 remove-device vfio123",
                )
                .unwrap();
            thread::sleep(std::time::Duration::new(10, 0));

            // Check the amount of PCI devices appearing in L2 VM is back down
            // to 8 devices.
            assert_eq!(
                guest
                    .ssh_command_l2_1("ls /sys/bus/pci/devices | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                8,
            );

            // Perform memory hotplug in L2 and validate the memory is showing
            // up as expected. In order to check, we will use the virtio-net
            // device already passed through L2 as a VFIO device, this will
            // verify that VFIO devices are functional with memory hotplug.
            assert!(guest.get_total_memory_l2().unwrap_or_default() > 480_000);
            guest
                .ssh_command_l2_1(
                    "sudo bash -c 'echo online > /sys/devices/system/memory/auto_online_blocks'",
                )
                .unwrap();
            guest
                .ssh_command_l1(
                    "sudo /mnt/ch-remote \
                 --api-socket=/tmp/ch_api.sock \
                 resize --memory=1073741824",
                )
                .unwrap();
            assert!(guest.get_total_memory_l2().unwrap_or_default() > 960_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        cleanup_vfio_network_interfaces();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_direct_kernel_boot_noacpi() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args([
                "--cmdline",
                format!("{} acpi=off", DIRECT_KERNEL_BOOT_CMDLINE).as_str(),
            ])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 1);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_virtio_vsock() {
        _test_virtio_vsock(false)
    }

    #[test]
    fn test_virtio_vsock_hotplug() {
        _test_virtio_vsock(true);
    }

    #[test]
    // Start cloud-hypervisor with no VM parameters, only the API server running.
    // From the API: Create a VM, boot it and check that it looks as expected.
    fn test_api_create_boot() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        thread::sleep(std::time::Duration::new(1, 0));

        // Verify API server is running
        curl_command(&api_socket, "GET", "http://localhost/api/v1/vmm.ping", None);

        // Create the VM first
        let cpu_count: u8 = 4;
        let http_body = guest.api_create_body(
            cpu_count,
            direct_kernel_boot_path().to_str().unwrap(),
            DIRECT_KERNEL_BOOT_CMDLINE,
        );

        let temp_config_path = guest.tmp_dir.as_path().join("config");
        std::fs::write(&temp_config_path, http_body).unwrap();

        remote_command(
            &api_socket,
            "create",
            Some(temp_config_path.as_os_str().to_str().unwrap()),
        );

        // Then boot it
        remote_command(&api_socket, "boot", None);
        thread::sleep(std::time::Duration::new(20, 0));

        let r = std::panic::catch_unwind(|| {
            // Check that the VM booted as expected
            assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    // Start cloud-hypervisor with no VM parameters, only the API server running.
    // From the API: Create a VM, boot it and check it can be shutdown and then
    // booted again
    fn test_api_shutdown() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        thread::sleep(std::time::Duration::new(1, 0));

        // Verify API server is running
        curl_command(&api_socket, "GET", "http://localhost/api/v1/vmm.ping", None);

        // Create the VM first
        let cpu_count: u8 = 4;
        let http_body = guest.api_create_body(
            cpu_count,
            direct_kernel_boot_path().to_str().unwrap(),
            DIRECT_KERNEL_BOOT_CMDLINE,
        );

        let r = std::panic::catch_unwind(|| {
            curl_command(
                &api_socket,
                "PUT",
                "http://localhost/api/v1/vm.create",
                Some(&http_body),
            );

            // Then boot it
            curl_command(&api_socket, "PUT", "http://localhost/api/v1/vm.boot", None);

            guest.wait_vm_boot(None).unwrap();

            // Check that the VM booted as expected
            assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

            // Sync and shutdown without powering off to prevent filesystem
            // corruption.
            guest.ssh_command("sync").unwrap();
            guest.ssh_command("sudo shutdown -H now").unwrap();

            // Wait for the guest to be fully shutdown
            thread::sleep(std::time::Duration::new(20, 0));

            // Then shut it down
            curl_command(
                &api_socket,
                "PUT",
                "http://localhost/api/v1/vm.shutdown",
                None,
            );

            // Then boot it again
            curl_command(&api_socket, "PUT", "http://localhost/api/v1/vm.boot", None);

            guest.wait_vm_boot(None).unwrap();

            // Check that the VM booted as expected
            assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    // Start cloud-hypervisor with no VM parameters, only the API server running.
    // From the API: Create a VM, boot it and check it can be deleted and then recreated
    // booted again.
    fn test_api_delete() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        thread::sleep(std::time::Duration::new(1, 0));

        // Verify API server is running
        curl_command(&api_socket, "GET", "http://localhost/api/v1/vmm.ping", None);

        // Create the VM first
        let cpu_count: u8 = 4;
        let http_body = guest.api_create_body(
            cpu_count,
            direct_kernel_boot_path().to_str().unwrap(),
            DIRECT_KERNEL_BOOT_CMDLINE,
        );

        let r = std::panic::catch_unwind(|| {
            curl_command(
                &api_socket,
                "PUT",
                "http://localhost/api/v1/vm.create",
                Some(&http_body),
            );

            // Then boot it
            curl_command(&api_socket, "PUT", "http://localhost/api/v1/vm.boot", None);

            guest.wait_vm_boot(None).unwrap();

            // Check that the VM booted as expected
            assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

            // Sync and shutdown without powering off to prevent filesystem
            // corruption.
            guest.ssh_command("sync").unwrap();
            guest.ssh_command("sudo shutdown -H now").unwrap();

            // Wait for the guest to be fully shutdown
            thread::sleep(std::time::Duration::new(20, 0));

            // Then delete it
            curl_command(
                &api_socket,
                "PUT",
                "http://localhost/api/v1/vm.delete",
                None,
            );

            curl_command(
                &api_socket,
                "PUT",
                "http://localhost/api/v1/vm.create",
                Some(&http_body),
            );

            // Then boot it again
            curl_command(&api_socket, "PUT", "http://localhost/api/v1/vm.boot", None);

            guest.wait_vm_boot(None).unwrap();

            // Check that the VM booted as expected
            assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    // Start cloud-hypervisor with no VM parameters, only the API server running.
    // From the API: Create a VM, boot it and check that it looks as expected.
    // Then we pause the VM, check that it's no longer available.
    // Finally we resume the VM and check that it's available.
    fn test_api_pause_resume() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        thread::sleep(std::time::Duration::new(1, 0));

        // Verify API server is running
        curl_command(&api_socket, "GET", "http://localhost/api/v1/vmm.ping", None);

        // Create the VM first
        let cpu_count: u8 = 4;
        let http_body = guest.api_create_body(
            cpu_count,
            direct_kernel_boot_path().to_str().unwrap(),
            DIRECT_KERNEL_BOOT_CMDLINE,
        );
        curl_command(
            &api_socket,
            "PUT",
            "http://localhost/api/v1/vm.create",
            Some(&http_body),
        );

        // Then boot it
        curl_command(&api_socket, "PUT", "http://localhost/api/v1/vm.boot", None);
        thread::sleep(std::time::Duration::new(20, 0));

        let r = std::panic::catch_unwind(|| {
            // Check that the VM booted as expected
            assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

            // We now pause the VM
            assert!(remote_command(&api_socket, "pause", None));

            // Check pausing again fails
            assert!(!remote_command(&api_socket, "pause", None));

            thread::sleep(std::time::Duration::new(2, 0));

            // SSH into the VM should fail
            assert!(ssh_command_ip(
                "grep -c processor /proc/cpuinfo",
                &guest.network.guest_ip,
                2,
                5
            )
            .is_err());

            // Resume the VM
            assert!(remote_command(&api_socket, "resume", None));

            // Check resuming again fails
            assert!(!remote_command(&api_socket, "resume", None));

            thread::sleep(std::time::Duration::new(2, 0));

            // Now we should be able to SSH back in and get the right number of CPUs
            assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_virtio_iommu() {
        _test_virtio_iommu(cfg!(target_arch = "x86_64"))
    }

    #[test]
    // We cannot force the software running in the guest to reprogram the BAR
    // with some different addresses, but we have a reliable way of testing it
    // with a standard Linux kernel.
    // By removing a device from the PCI tree, and then rescanning the tree,
    // Linux consistently chooses to reorganize the PCI device BARs to other
    // locations in the guest address space.
    // This test creates a dedicated PCI network device to be checked as being
    // properly probed first, then removing it, and adding it again by doing a
    // rescan.
    fn test_pci_bar_reprogramming() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        #[cfg(target_arch = "x86_64")]
        let kernel_path = direct_kernel_boot_path();
        #[cfg(target_arch = "aarch64")]
        let kernel_path = edk2_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args([
                "--net",
                guest.default_net_string().as_str(),
                "tap=,mac=8a:6b:6f:5a:de:ac,ip=192.168.3.1,mask=255.255.255.0",
            ])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // 2 network interfaces + default localhost ==> 3 interfaces
            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                3
            );

            let init_bar_addr = guest
                .ssh_command(
                    "sudo awk '{print $1; exit}' /sys/bus/pci/devices/0000:00:05.0/resource",
                )
                .unwrap();

            // Remove the PCI device
            guest
                .ssh_command("echo 1 | sudo tee /sys/bus/pci/devices/0000:00:05.0/remove")
                .unwrap();

            // Only 1 network interface left + default localhost ==> 2 interfaces
            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                2
            );

            // Remove the PCI device
            guest
                .ssh_command("echo 1 | sudo tee /sys/bus/pci/rescan")
                .unwrap();

            // Back to 2 network interface + default localhost ==> 3 interfaces
            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                3
            );

            let new_bar_addr = guest
                .ssh_command(
                    "sudo awk '{print $1; exit}' /sys/bus/pci/devices/0000:00:05.0/resource",
                )
                .unwrap();

            // Let's compare the BAR addresses for our virtio-net device.
            // They should be different as we expect the BAR reprogramming
            // to have happened.
            assert_ne!(init_bar_addr, new_bar_addr);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_memory_mergeable_off() {
        test_memory_mergeable(false)
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_cpu_hotplug() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=2,max=4"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 2);

            // Resize the VM
            let desired_vcpus = 4;
            resize_command(&api_socket, Some(desired_vcpus), None, None, None);

            guest
                .ssh_command("echo 1 | sudo tee /sys/bus/cpu/devices/cpu2/online")
                .unwrap();
            guest
                .ssh_command("echo 1 | sudo tee /sys/bus/cpu/devices/cpu3/online")
                .unwrap();
            thread::sleep(std::time::Duration::new(10, 0));
            assert_eq!(
                guest.get_cpu_count().unwrap_or_default(),
                u32::from(desired_vcpus)
            );

            // guest.reboot_linux(0, None);
            //
            // assert_eq!(
            //     guest.get_cpu_count().unwrap_or_default(),
            //     u32::from(desired_vcpus)
            // );
            //
            // // Resize the VM
            // let desired_vcpus = 2;
            // resize_command(&api_socket, Some(desired_vcpus), None, None, None);
            //
            // thread::sleep(std::time::Duration::new(10, 0));
            // assert_eq!(
            //     guest.get_cpu_count().unwrap_or_default(),
            //     u32::from(desired_vcpus)
            // );
            //
            // // Resize the VM back up to 4
            // let desired_vcpus = 4;
            // resize_command(&api_socket, Some(desired_vcpus), None, None, None);
            //
            // guest
            //     .ssh_command("echo 1 | sudo tee /sys/bus/cpu/devices/cpu2/online")
            //     .unwrap();
            // guest
            //     .ssh_command("echo 1 | sudo tee /sys/bus/cpu/devices/cpu3/online")
            //     .unwrap();
            // thread::sleep(std::time::Duration::new(10, 0));
            // assert_eq!(
            //     guest.get_cpu_count().unwrap_or_default(),
            //     u32::from(desired_vcpus)
            // );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_memory_hotplug() {
        #[cfg(target_arch = "aarch64")]
        let focal_image = FOCAL_IMAGE_UPDATE_KERNEL_NAME.to_string();
        #[cfg(target_arch = "x86_64")]
        let focal_image = FOCAL_IMAGE_NAME.to_string();
        let focal = UbuntuDiskConfig::new(focal_image);
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        #[cfg(target_arch = "aarch64")]
        let kernel_path = edk2_path();
        #[cfg(target_arch = "x86_64")]
        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=2,max=4"])
            .args(["--memory", "size=512M,hotplug_size=8192M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--balloon", "size=0"])
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

            guest.enable_memory_hotplug();

            // Add RAM to the VM
            let desired_ram = 1024 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(10, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);

            // Use balloon to remove RAM from the VM
            let desired_balloon = 512 << 20;
            resize_command(&api_socket, None, None, Some(desired_balloon), None);

            thread::sleep(std::time::Duration::new(10, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
            assert!(guest.get_total_memory().unwrap_or_default() < 960_000);

            // guest.reboot_linux(0, None);
            //
            // assert!(guest.get_total_memory().unwrap_or_default() < 960_000);
            //
            // // Use balloon add RAM to the VM
            // let desired_balloon = 0;
            // resize_command(&api_socket, None, None, Some(desired_balloon), None);
            //
            // thread::sleep(std::time::Duration::new(10, 0));
            //
            // assert!(guest.get_total_memory().unwrap_or_default() > 960_000);
            //
            // guest.enable_memory_hotplug();
            //
            // // Add RAM to the VM
            // let desired_ram = 2048 << 20;
            // resize_command(&api_socket, None, Some(desired_ram), None, None);
            //
            // thread::sleep(std::time::Duration::new(10, 0));
            // assert!(guest.get_total_memory().unwrap_or_default() > 1_920_000);
            //
            // // Remove RAM to the VM (only applies after reboot)
            // let desired_ram = 1024 << 20;
            // resize_command(&api_socket, None, Some(desired_ram), None, None);
            //
            // guest.reboot_linux(1, None);
            //
            // assert!(guest.get_total_memory().unwrap_or_default() > 960_000);
            // assert!(guest.get_total_memory().unwrap_or_default() < 1_920_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_virtio_mem() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=2,max=4"])
            .args([
                "--memory",
                "size=512M,hotplug_method=virtio-mem,hotplug_size=8192M",
            ])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

            guest.enable_memory_hotplug();

            // Add RAM to the VM
            let desired_ram = 1024 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(10, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);

            // Add RAM to the VM
            let desired_ram = 2048 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(10, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 1_920_000);

            // Remove RAM from the VM
            let desired_ram = 1024 << 20;
            resize_command(&api_socket, None, Some(desired_ram), None, None);

            thread::sleep(std::time::Duration::new(10, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);
            assert!(guest.get_total_memory().unwrap_or_default() < 1_920_000);

            // guest.reboot_linux(0, None);
            //
            // // Check the amount of memory after reboot is 1GiB
            // assert!(guest.get_total_memory().unwrap_or_default() > 960_000);
            // assert!(guest.get_total_memory().unwrap_or_default() < 1_920_000);
            //
            // // Check we can still resize to 512MiB
            // let desired_ram = 512 << 20;
            // resize_command(&api_socket, None, Some(desired_ram), None, None);
            // thread::sleep(std::time::Duration::new(10, 0));
            // assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
            // assert!(guest.get_total_memory().unwrap_or_default() < 960_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    #[cfg(not(feature = "mshv"))]
    // Test both vCPU and memory resizing together
    fn test_resize() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=2,max=4"])
            .args(["--memory", "size=512M,hotplug_size=8192M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 2);
            assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

            guest.enable_memory_hotplug();

            // Resize the VM
            let desired_vcpus = 4;
            let desired_ram = 1024 << 20;
            resize_command(
                &api_socket,
                Some(desired_vcpus),
                Some(desired_ram),
                None,
                None,
            );

            guest
                .ssh_command("echo 1 | sudo tee /sys/bus/cpu/devices/cpu2/online")
                .unwrap();
            guest
                .ssh_command("echo 1 | sudo tee /sys/bus/cpu/devices/cpu3/online")
                .unwrap();
            thread::sleep(std::time::Duration::new(10, 0));
            assert_eq!(
                guest.get_cpu_count().unwrap_or_default(),
                u32::from(desired_vcpus)
            );

            assert!(guest.get_total_memory().unwrap_or_default() > 960_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_memory_overhead() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let guest_memory_size_kb = 512 * 1024;

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args([
                "--memory",
                format!("size={}K", guest_memory_size_kb).as_str(),
            ])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .capture_output()
            .spawn()
            .unwrap();

        thread::sleep(std::time::Duration::new(20, 0));

        let r = std::panic::catch_unwind(|| {
            let overhead = get_vmm_overhead(child.id(), guest_memory_size_kb);
            eprintln!(
                "Guest memory overhead: {} vs {}",
                overhead, MAXIMUM_VMM_OVERHEAD_KB
            );
            assert!(overhead <= MAXIMUM_VMM_OVERHEAD_KB);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_disk_hotplug() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        #[cfg(target_arch = "x86_64")]
        let kernel_path = direct_kernel_boot_path();
        #[cfg(target_arch = "aarch64")]
        let kernel_path = edk2_path();

        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check /dev/vdc is not there
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep -c vdc.*16M || true")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(1),
                0
            );

            // Now let's add the extra disk.
            let mut blk_file_path = dirs::home_dir().unwrap();
            blk_file_path.push("workloads");
            blk_file_path.push("blk.img");
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-disk",
                Some(format!("path={},id=test0", blk_file_path.to_str().unwrap()).as_str()),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"test0\",\"bdf\":\"0000:00:06.0\"}"));

            thread::sleep(std::time::Duration::new(10, 0));

            // Check that /dev/vdc exists and the block size is 16M.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdc | grep -c 16M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );
            // And check the block device can be read.
            guest
                .ssh_command("sudo dd if=/dev/vdc of=/dev/null bs=1M iflag=direct count=16")
                .unwrap();

            // Let's remove it the extra disk.
            assert!(remote_command(&api_socket, "remove-device", Some("test0")));
            thread::sleep(std::time::Duration::new(5, 0));
            // And check /dev/vdc is not there
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep -c vdc.*16M || true")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(1),
                0
            );

            // And add it back to validate unplug did work correctly.
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-disk",
                Some(format!("path={},id=test0", blk_file_path.to_str().unwrap()).as_str()),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"test0\",\"bdf\":\"0000:00:06.0\"}"));

            thread::sleep(std::time::Duration::new(10, 0));

            // Check that /dev/vdc exists and the block size is 16M.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdc | grep -c 16M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );
            // And check the block device can be read.
            guest
                .ssh_command("sudo dd if=/dev/vdc of=/dev/null bs=1M iflag=direct count=16")
                .unwrap();

            // Reboot the VM.
            // guest.reboot_linux(0, None);

            // Check still there after reboot
            // assert_eq!(
            //     guest
            //         .ssh_command("lsblk | grep vdc | grep -c 16M")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or_default(),
            //     1
            // );
            //
            // assert!(remote_command(&api_socket, "remove-device", Some("test0")));
            //
            // thread::sleep(std::time::Duration::new(20, 0));
            //
            // // Check device has gone away
            // assert_eq!(
            //     guest
            //         .ssh_command("lsblk | grep -c vdc.*16M || true")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or(1),
            //     0
            // );
            //
            // guest.reboot_linux(1, None);
            //
            // // Check device still absent
            // assert_eq!(
            //     guest
            //         .ssh_command("lsblk | grep -c vdc.*16M || true")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or(1),
            //     0
            // );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[allow(clippy::useless_conversion)]
    fn create_loop_device(backing_file_path: &str, block_size: u32, num_retries: usize) -> String {
        const LOOP_CONFIGURE: u64 = 0x4c0a;
        const LOOP_CTL_GET_FREE: u64 = 0x4c82;
        const LOOP_CTL_PATH: &str = "/dev/loop-control";
        const LOOP_DEVICE_PREFIX: &str = "/dev/loop";

        #[repr(C)]
        struct LoopInfo64 {
            lo_device: u64,
            lo_inode: u64,
            lo_rdevice: u64,
            lo_offset: u64,
            lo_sizelimit: u64,
            lo_number: u32,
            lo_encrypt_type: u32,
            lo_encrypt_key_size: u32,
            lo_flags: u32,
            lo_file_name: [u8; 64],
            lo_crypt_name: [u8; 64],
            lo_encrypt_key: [u8; 32],
            lo_init: [u64; 2],
        }

        impl Default for LoopInfo64 {
            fn default() -> Self {
                LoopInfo64 {
                    lo_device: 0,
                    lo_inode: 0,
                    lo_rdevice: 0,
                    lo_offset: 0,
                    lo_sizelimit: 0,
                    lo_number: 0,
                    lo_encrypt_type: 0,
                    lo_encrypt_key_size: 0,
                    lo_flags: 0,
                    lo_file_name: [0; 64],
                    lo_crypt_name: [0; 64],
                    lo_encrypt_key: [0; 32],
                    lo_init: [0; 2],
                }
            }
        }

        #[derive(Default)]
        #[repr(C)]
        struct LoopConfig {
            fd: u32,
            block_size: u32,
            info: LoopInfo64,
            _reserved: [u64; 8],
        }

        // Open loop-control device
        let loop_ctl_file = OpenOptions::new()
            .read(true)
            .write(true)
            .open(LOOP_CTL_PATH)
            .unwrap();

        // Request a free loop device
        let loop_device_number = unsafe {
            libc::ioctl(
                loop_ctl_file.as_raw_fd(),
                LOOP_CTL_GET_FREE.try_into().unwrap(),
            )
        };
        if loop_device_number < 0 {
            panic!("Couldn't find a free loop device");
        }

        // Create loop device path
        let loop_device_path = format!("{}{}", LOOP_DEVICE_PREFIX, loop_device_number);

        // Open loop device
        let loop_device_file = OpenOptions::new()
            .read(true)
            .write(true)
            .open(&loop_device_path)
            .unwrap();

        // Open backing file
        let backing_file = OpenOptions::new()
            .read(true)
            .write(true)
            .open(backing_file_path)
            .unwrap();

        let loop_config = LoopConfig {
            fd: backing_file.as_raw_fd() as u32,
            block_size,
            ..Default::default()
        };

        for i in 0..num_retries {
            let ret = unsafe {
                libc::ioctl(
                    loop_device_file.as_raw_fd(),
                    LOOP_CONFIGURE.try_into().unwrap(),
                    &loop_config,
                )
            };
            if ret != 0 {
                if i < num_retries - 1 {
                    println!(
                        "Iteration {}: Failed to configure the loop device {}: {}",
                        i,
                        loop_device_path,
                        std::io::Error::last_os_error()
                    );
                } else {
                    panic!(
                        "Failed {} times trying to configure the loop device {}: {}",
                        num_retries,
                        loop_device_path,
                        std::io::Error::last_os_error()
                    );
                }
            } else {
                break;
            }

            // Wait for a bit before retrying
            thread::sleep(std::time::Duration::new(5, 0));
        }

        loop_device_path
    }

    #[test]
    fn test_virtio_block_topology() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();
        let test_disk_path = guest.tmp_dir.as_path().join("test.img");

        let output = exec_host_command_output(
            format!(
                "qemu-img create -f raw {} 16M",
                test_disk_path.to_str().unwrap()
            )
            .as_str(),
        );
        if !output.status.success() {
            let stdout = String::from_utf8_lossy(&output.stdout);
            let stderr = String::from_utf8_lossy(&output.stderr);
            panic!(
                "qemu-img command failed\nstdout\n{}\nstderr\n{}",
                stdout, stderr
            );
        }

        let loop_dev = create_loop_device(test_disk_path.to_str().unwrap(), 4096, 5);

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
                format!("path={}", &loop_dev).as_str(),
            ])
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // MIN-IO column
            assert_eq!(
                guest
                    .ssh_command("lsblk -t| grep vdc | awk '{print $3}'")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                4096
            );
            // PHY-SEC column
            assert_eq!(
                guest
                    .ssh_command("lsblk -t| grep vdc | awk '{print $5}'")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                4096
            );
            // LOG-SEC column
            assert_eq!(
                guest
                    .ssh_command("lsblk -t| grep vdc | awk '{print $6}'")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                4096
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);

        Command::new("losetup")
            .args(["-d", &loop_dev])
            .output()
            .expect("loop device not found");
    }

    #[test]
    fn test_virtio_balloon_deflate_on_oom() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let api_socket = temp_api_path(&guest.tmp_dir);

        //Let's start a 4G guest with balloon occupied 2G memory
        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--balloon", "size=2G,deflate_on_oom=on"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Wait for balloon memory's initialization and check its size.
            // The virtio-balloon driver might take a few seconds to report the
            // balloon effective size back to the VMM.
            thread::sleep(std::time::Duration::new(20, 0));

            let orig_balloon = balloon_size(&api_socket);
            println!("The original balloon memory size is {} bytes", orig_balloon);
            assert!(orig_balloon == 2147483648);

            // Two steps to verify if the 'deflate_on_oom' parameter works.
            // 1st: run a command to trigger an OOM in the guest.
            guest
                .ssh_command("echo f | sudo tee /proc/sysrq-trigger")
                .unwrap();

            // Give some time for the OOM to happen in the guest and be reported
            // back to the host.
            thread::sleep(std::time::Duration::new(20, 0));

            // 2nd: check balloon_mem's value to verify balloon has been automatically deflated
            let deflated_balloon = balloon_size(&api_socket);
            println!(
                "After deflating, balloon memory size is {} bytes",
                deflated_balloon
            );
            // Verify the balloon size deflated
            assert!(deflated_balloon < 2147483648);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_virtio_balloon_free_page_reporting() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        //Let's start a 4G guest with balloon occupied 2G memory
        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--balloon", "size=0,free_page_reporting=on"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let pid = child.id();
        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check the initial RSS is less than 1GiB
            let rss = process_rss_kib(pid);
            println!("RSS {} < 1048576", rss);
            assert!(rss < 1048576);

            // Spawn a command inside the guest to consume 2GiB of RAM for 60
            // seconds
            let guest_ip = guest.network.guest_ip.clone();
            thread::spawn(move || {
                ssh_command_ip(
                    "stress --vm 1 --vm-bytes 2G --vm-keep --timeout 60",
                    &guest_ip,
                    DEFAULT_SSH_RETRIES,
                    DEFAULT_SSH_TIMEOUT,
                )
                .unwrap();
            });

            // Wait for 50 seconds to make sure the stress command is consuming
            // the expected amount of memory.
            thread::sleep(std::time::Duration::new(50, 0));
            let rss = process_rss_kib(pid);
            println!("RSS {} >= 2097152", rss);
            assert!(rss >= 2097152);

            // Wait for an extra minute to make sure the stress command has
            // completed and that the guest reported the free pages to the VMM
            // through the virtio-balloon device. We expect the RSS to be under
            // 2GiB.
            thread::sleep(std::time::Duration::new(60, 0));
            let rss = process_rss_kib(pid);
            println!("RSS {} < 2097152", rss);
            assert!(rss < 2097152);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_pmem_hotplug() {
        _test_pmem_hotplug(None)
    }

    #[test]
    fn test_pmem_multi_segment_hotplug() {
        _test_pmem_hotplug(Some(15))
    }

    fn _test_pmem_hotplug(pci_segment: Option<u16>) {
        #[cfg(target_arch = "aarch64")]
        let focal_image = FOCAL_IMAGE_UPDATE_KERNEL_NAME.to_string();
        #[cfg(target_arch = "x86_64")]
        let focal_image = FOCAL_IMAGE_NAME.to_string();
        let focal = UbuntuDiskConfig::new(focal_image);
        let guest = Guest::new(Box::new(focal));

        #[cfg(target_arch = "x86_64")]
        let kernel_path = direct_kernel_boot_path();
        #[cfg(target_arch = "aarch64")]
        let kernel_path = edk2_path();

        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut cmd = GuestCommand::new(&guest);

        cmd.args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .capture_output();

        if pci_segment.is_some() {
            cmd.args(["--platform", "num_pci_segments=16"]);
        }

        let mut child = cmd.spawn().unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check /dev/pmem0 is not there
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep -c pmem0 || true")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(1),
                0
            );

            let pmem_temp_file = TempFile::new().unwrap();
            pmem_temp_file.as_file().set_len(128 << 20).unwrap();
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-pmem",
                Some(&format!(
                    "file={},id=test0{}",
                    pmem_temp_file.as_path().to_str().unwrap(),
                    if let Some(pci_segment) = pci_segment {
                        format!(",pci_segment={}", pci_segment)
                    } else {
                        "".to_owned()
                    }
                )),
            );
            assert!(cmd_success);
            if let Some(pci_segment) = pci_segment {
                assert!(String::from_utf8_lossy(&cmd_output).contains(&format!(
                    "{{\"id\":\"test0\",\"bdf\":\"{:04x}:00:01.0\"}}",
                    pci_segment
                )));
            } else {
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains("{\"id\":\"test0\",\"bdf\":\"0000:00:06.0\"}"));
            }

            // Check that /dev/pmem0 exists and the block size is 128M
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep pmem0 | grep -c 128M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // guest.reboot_linux(0, None);
            //
            // // Check still there after reboot
            // assert_eq!(
            //     guest
            //         .ssh_command("lsblk | grep pmem0 | grep -c 128M")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or_default(),
            //     1
            // );
            //
            // assert!(remote_command(&api_socket, "remove-device", Some("test0")));
            //
            // thread::sleep(std::time::Duration::new(20, 0));
            //
            // // Check device has gone away
            // assert_eq!(
            //     guest
            //         .ssh_command("lsblk | grep -c pmem0.*128M || true")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or(1),
            //     0
            // );
            //
            // guest.reboot_linux(1, None);
            //
            // // Check still absent after reboot
            // assert_eq!(
            //     guest
            //         .ssh_command("lsblk | grep -c pmem0.*128M || true")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or(1),
            //     0
            // );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_net_hotplug() {
        _test_net_hotplug(None)
    }

    #[test]
    fn test_net_multi_segment_hotplug() {
        _test_net_hotplug(Some(15))
    }

    fn _test_net_hotplug(pci_segment: Option<u16>) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        #[cfg(target_arch = "x86_64")]
        let kernel_path = direct_kernel_boot_path();
        #[cfg(target_arch = "aarch64")]
        let kernel_path = edk2_path();

        let api_socket = temp_api_path(&guest.tmp_dir);

        // Boot without network
        let mut cmd = GuestCommand::new(&guest);

        cmd.args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .capture_output();

        if pci_segment.is_some() {
            cmd.args(["--platform", "num_pci_segments=16"]);
        }

        let mut child = cmd.spawn().unwrap();

        thread::sleep(std::time::Duration::new(20, 0));

        let r = std::panic::catch_unwind(|| {
            // Add network
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-net",
                Some(
                    format!(
                        "{}{},id=test0",
                        guest.default_net_string(),
                        if let Some(pci_segment) = pci_segment {
                            format!(",pci_segment={}", pci_segment)
                        } else {
                            "".to_owned()
                        }
                    )
                    .as_str(),
                ),
            );
            assert!(cmd_success);

            if let Some(pci_segment) = pci_segment {
                assert!(String::from_utf8_lossy(&cmd_output).contains(&format!(
                    "{{\"id\":\"test0\",\"bdf\":\"{:04x}:00:01.0\"}}",
                    pci_segment
                )));
            } else {
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains("{\"id\":\"test0\",\"bdf\":\"0000:00:05.0\"}"));
            }

            thread::sleep(std::time::Duration::new(5, 0));

            // 1 network interfaces + default localhost ==> 2 interfaces
            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                2
            );

            // Remove network
            assert!(remote_command(&api_socket, "remove-device", Some("test0"),));
            thread::sleep(std::time::Duration::new(5, 0));

            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-net",
                Some(
                    format!(
                        "{}{},id=test1",
                        guest.default_net_string(),
                        if let Some(pci_segment) = pci_segment {
                            format!(",pci_segment={}", pci_segment)
                        } else {
                            "".to_owned()
                        }
                    )
                    .as_str(),
                ),
            );
            assert!(cmd_success);

            if let Some(pci_segment) = pci_segment {
                assert!(String::from_utf8_lossy(&cmd_output).contains(&format!(
                    "{{\"id\":\"test1\",\"bdf\":\"{:04x}:00:01.0\"}}",
                    pci_segment
                )));
            } else {
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains("{\"id\":\"test1\",\"bdf\":\"0000:00:05.0\"}"));
            }

            thread::sleep(std::time::Duration::new(5, 0));

            // 1 network interfaces + default localhost ==> 2 interfaces
            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                2
            );

            // guest.reboot_linux(0, None);
            //
            // // Check still there after reboot
            // // 1 network interfaces + default localhost ==> 2 interfaces
            // assert_eq!(
            //     guest
            //         .ssh_command("ip -o link | wc -l")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or_default(),
            //     2
            // );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_initramfs() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        #[cfg(target_arch = "x86_64")]
        let mut kernels = vec![direct_kernel_boot_path()];
        #[cfg(target_arch = "aarch64")]
        let kernels = vec![direct_kernel_boot_path()];

        #[cfg(target_arch = "x86_64")]
        {
            let mut pvh_kernel_path = workload_path.clone();
            pvh_kernel_path.push("vmlinux");
            kernels.push(pvh_kernel_path);
        }

        let mut initramfs_path = workload_path;
        initramfs_path.push("alpine_initramfs.img");

        let test_string = String::from("axz34i9rylotd8n50wbv6kcj7f2qushme1pg");
        let cmdline = format!("console=hvc0 quiet TEST_STRING={}", test_string);

        kernels.iter().for_each(|k_path| {
            let mut child = GuestCommand::new(&guest)
                .args(["--kernel", k_path.to_str().unwrap()])
                .args(["--initramfs", initramfs_path.to_str().unwrap()])
                .args(["--cmdline", &cmdline])
                .capture_output()
                .spawn()
                .unwrap();

            thread::sleep(std::time::Duration::new(20, 0));

            let _ = child.kill();
            let output = child.wait_with_output().unwrap();

            let r = std::panic::catch_unwind(|| {
                let s = String::from_utf8_lossy(&output.stdout);

                assert_ne!(s.lines().position(|line| line == test_string), None);
            });

            handle_child_output(r, &output);
        });
    }

    // One thing to note about this test. The virtio-net device is heavily used
    // through each ssh command. There's no need to perform a dedicated test to
    // verify the migration went well for virtio-net.
    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_snapshot_restore_hotplug_virtiomem() {
        _test_snapshot_restore(true);
    }

    #[test]
    fn test_snapshot_restore_basic() {
        _test_snapshot_restore(false);
    }

    fn _test_snapshot_restore(use_hotplug: bool) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();

        let api_socket_source = format!("{}.1", temp_api_path(&guest.tmp_dir));

        let net_id = "net123";
        let net_params = format!(
            "id={},tap=,mac={},ip={},mask=255.255.255.0",
            net_id, guest.network.guest_mac, guest.network.host_ip
        );
        let mut mem_params = "size=4G";

        if use_hotplug {
            mem_params = "size=4G,hotplug_method=virtio-mem,hotplug_size=32G"
        }

        let cloudinit_params = format!(
            "path={},iommu=on",
            guest.disk_config.disk(DiskType::CloudInit).unwrap()
        );

        let socket = temp_vsock_path(&guest.tmp_dir);
        let event_path = temp_event_monitor_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket_source])
            .args(["--event-monitor", format!("path={}", event_path).as_str()])
            .args(["--cpus", "boot=4"])
            .args(["--memory", mem_params])
            .args(["--balloon", "size=0"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                cloudinit_params.as_str(),
            ])
            .args(["--net", net_params.as_str()])
            .args(["--vsock", format!("cid=3,socket={}", socket).as_str()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .capture_output()
            .spawn()
            .unwrap();

        let console_text = String::from("On a branch floating down river a cricket, singing.");
        // Create the snapshot directory
        let snapshot_dir = temp_snapshot_dir_path(&guest.tmp_dir);

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check the number of vCPUs
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 4);
            // Check the guest RAM
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);
            if use_hotplug {
                // Increase guest RAM with virtio-mem
                resize_command(
                    &api_socket_source,
                    None,
                    Some(6 << 30),
                    None,
                    Some(&event_path),
                );
                thread::sleep(std::time::Duration::new(5, 0));
                assert!(guest.get_total_memory().unwrap_or_default() > 5_760_000);
                // Use balloon to remove RAM from the VM
                resize_command(
                    &api_socket_source,
                    None,
                    None,
                    Some(1 << 30),
                    Some(&event_path),
                );
                thread::sleep(std::time::Duration::new(5, 0));
                let total_memory = guest.get_total_memory().unwrap_or_default();
                assert!(total_memory > 4_800_000);
                assert!(total_memory < 5_760_000);
            }
            // Check the guest virtio-devices, e.g. block, rng, vsock, console, and net
            guest.check_devices_common(Some(&socket), Some(&console_text), None);

            // x86_64: We check that removing and adding back the virtio-net device
            // does not break the snapshot/restore support for virtio-pci.
            // This is an important thing to test as the hotplug will
            // trigger a PCI BAR reprogramming, which is a good way of
            // checking if the stored resources are correctly restored.
            // Unplug the virtio-net device
            // AArch64: Device hotplug is currently not supported, skipping here.
            #[cfg(target_arch = "x86_64")]
            {
                assert!(remote_command(
                    &api_socket_source,
                    "remove-device",
                    Some(net_id),
                ));
                thread::sleep(std::time::Duration::new(10, 0));
                let latest_events = [&MetaEvent {
                    event: "device-removed".to_string(),
                    device_id: Some(net_id.to_string()),
                }];
                assert!(check_latest_events_exact(&latest_events, &event_path));

                // Plug the virtio-net device again
                assert!(remote_command(
                    &api_socket_source,
                    "add-net",
                    Some(net_params.as_str()),
                ));
                thread::sleep(std::time::Duration::new(10, 0));
            }
            snapshot_and_check_events(
                api_socket_source.as_str(),
                snapshot_dir.as_str(),
                event_path.as_str(),
            );
        });

        // Shutdown the source VM and check console output
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&output.stdout).contains(&console_text));
        });

        handle_child_output(r, &output);

        // Remove the vsock socket file.
        Command::new("rm")
            .arg("-f")
            .arg(socket.as_str())
            .output()
            .unwrap();

        let api_socket_restored = format!("{}.2", temp_api_path(&guest.tmp_dir));
        let event_path_restored = format!("{}.2", temp_event_monitor_path(&guest.tmp_dir));

        // Restore the VM from the snapshot
        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket_restored])
            .args([
                "--event-monitor",
                format!("path={}", event_path_restored).as_str(),
            ])
            .args([
                "--restore",
                format!("source_url=file://{}", snapshot_dir).as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        // Wait for the VM to be restored
        thread::sleep(std::time::Duration::new(10, 0));
        let expected_events = [
            &MetaEvent {
                event: "starting".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "restoring".to_string(),
                device_id: None,
            },
        ];
        assert!(check_sequential_events_exact(
            &expected_events,
            &event_path_restored
        ));
        let latest_events = [&MetaEvent {
            event: "restored".to_string(),
            device_id: None,
        }];
        assert!(check_latest_events_exact(
            &latest_events,
            &event_path_restored
        ));

        let r = std::panic::catch_unwind(|| {
            // Resume the VM
            assert!(remote_command(&api_socket_restored, "resume", None));
            let latest_events = [
                &MetaEvent {
                    event: "resuming".to_string(),
                    device_id: None,
                },
                &MetaEvent {
                    event: "resumed".to_string(),
                    device_id: None,
                },
            ];
            assert!(check_latest_events_exact(
                &latest_events,
                &event_path_restored
            ));

            // Perform same checks to validate VM has been properly restored
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), 4);
            let total_memory = guest.get_total_memory().unwrap_or_default();
            if !use_hotplug {
                assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);
            } else {
                assert!(total_memory > 4_800_000);
                assert!(total_memory < 5_760_000);
                // Deflate balloon to restore entire RAM to the VM
                resize_command(&api_socket_restored, None, None, Some(0), None);
                thread::sleep(std::time::Duration::new(5, 0));
                assert!(guest.get_total_memory().unwrap_or_default() > 5_760_000);
                // Decrease guest RAM with virtio-mem
                resize_command(&api_socket_restored, None, Some(5 << 30), None, None);
                thread::sleep(std::time::Duration::new(5, 0));
                let total_memory = guest.get_total_memory().unwrap_or_default();
                assert!(total_memory > 4_800_000);
                assert!(total_memory < 5_760_000);
            }

            guest.check_devices_common(Some(&socket), Some(&console_text), None);
        });
        // Shutdown the target VM and check console output
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&output.stdout).contains(&console_text));
        });

        handle_child_output(r, &output);
    }

    #[test]
    fn test_counters() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--net", guest.default_net_string().as_str()])
            .args(["--api-socket", &api_socket])
            .capture_output();

        let mut child = cmd.spawn().unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            let orig_counters = get_counters(&api_socket);
            guest
                .ssh_command("dd if=/dev/zero of=test count=8 bs=1M")
                .unwrap();

            let new_counters = get_counters(&api_socket);

            // Check that all the counters have increased
            assert!(new_counters > orig_counters);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(feature = "guest_debug")]
    fn test_coredump() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=4"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", fw_path(FwType::RustHypervisorFirmware).as_str()])
            .default_disks()
            .args(["--net", guest.default_net_string().as_str()])
            .args(["--api-socket", &api_socket])
            .capture_output();

        let mut child = cmd.spawn().unwrap();
        let vmcore_file = temp_vmcore_file_path(&guest.tmp_dir);

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert!(remote_command(&api_socket, "pause", None));

            assert!(remote_command(
                &api_socket,
                "coredump",
                Some(format!("file://{}", vmcore_file).as_str()),
            ));

            // the num of CORE notes should equals to vcpu
            let readelf_core_num_cmd = format!(
                "readelf --all {} |grep CORE |grep -v Type |wc -l",
                vmcore_file
            );
            let core_num_in_elf = exec_host_command_output(&readelf_core_num_cmd);
            assert_eq!(String::from_utf8_lossy(&core_num_in_elf.stdout).trim(), "4");

            // the num of QEMU notes should equals to vcpu
            let readelf_vmm_num_cmd = format!("readelf --all {} |grep QEMU |wc -l", vmcore_file);
            let vmm_num_in_elf = exec_host_command_output(&readelf_vmm_num_cmd);
            assert_eq!(String::from_utf8_lossy(&vmm_num_in_elf.stdout).trim(), "4");
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_watchdog() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let kernel_path = direct_kernel_boot_path();

        let mut cmd = GuestCommand::new(&guest);
        cmd.args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--net", guest.default_net_string().as_str()])
            .args(["--watchdog"])
            .args(["--api-socket", &api_socket])
            .capture_output();

        let mut child = cmd.spawn().unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            let mut expected_reboot_count = 1;

            // Enable the watchdog with a 15s timeout
            enable_guest_watchdog(&guest, 15);

            // Reboot and check that systemd has activated the watchdog
            guest.ssh_command("sudo reboot").unwrap();
            guest.wait_vm_boot(None).unwrap();
            expected_reboot_count += 1;
            assert_eq!(get_reboot_count(&guest), expected_reboot_count);
            assert_eq!(
                guest
                    .ssh_command("sudo journalctl | grep -c -- \"Watchdog started\"")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                2
            );

            // Allow some normal time to elapse to check we don't get spurious reboots
            thread::sleep(std::time::Duration::new(40, 0));
            // Check no reboot
            assert_eq!(get_reboot_count(&guest), expected_reboot_count);

            // Trigger a panic (sync first). We need to do this inside a screen with a delay so the SSH command returns.
            guest.ssh_command("screen -dmS reboot sh -c \"sleep 5; echo s | tee /proc/sysrq-trigger; echo c | sudo tee /proc/sysrq-trigger\"").unwrap();
            // Allow some time for the watchdog to trigger (max 30s) and reboot to happen
            guest.wait_vm_boot(Some(50)).unwrap();
            // Check a reboot is triggerred by the watchdog
            expected_reboot_count += 1;
            assert_eq!(get_reboot_count(&guest), expected_reboot_count);

            #[cfg(target_arch = "x86_64")]
            {
                // Now pause the VM and remain offline for 30s
                assert!(remote_command(&api_socket, "pause", None));
                thread::sleep(std::time::Duration::new(30, 0));
                assert!(remote_command(&api_socket, "resume", None));

                // Check no reboot
                assert_eq!(get_reboot_count(&guest), expected_reboot_count);
            }
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_tap_from_fd() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();

        // Create a TAP interface with multi-queue enabled
        let num_queue_pairs: usize = 2;

        use std::str::FromStr;
        let taps = net_util::open_tap(
            Some("chtap0"),
            Some(std::net::Ipv4Addr::from_str(&guest.network.host_ip).unwrap()),
            None,
            &mut None,
            None,
            num_queue_pairs,
            Some(libc::O_RDWR | libc::O_NONBLOCK),
            None,
        )
        .unwrap();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", &format!("boot={}", num_queue_pairs)])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args([
                "--net",
                &format!(
                    "fd=[{},{}],mac={},num_queues={}",
                    taps[0].as_raw_fd(),
                    taps[1].as_raw_fd(),
                    guest.network.guest_mac,
                    num_queue_pairs * 2
                ),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                2
            );

            // guest.reboot_linux(0, None);
            //
            // assert_eq!(
            //     guest
            //         .ssh_command("ip -o link | wc -l")
            //         .unwrap()
            //         .trim()
            //         .parse::<u32>()
            //         .unwrap_or_default(),
            //     2
            // );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    // By design, a guest VM won't be able to connect to the host
    // machine when using a macvtap network interface (while it can
    // communicate externally). As a workaround, this integration
    // test creates two macvtap interfaces in 'bridge' mode on the
    // same physical net interface, one for the guest and one for
    // the host. With additional setup on the IP address and the
    // routing table, it enables the communications between the
    // guest VM and the host machine.
    // Details: https://wiki.libvirt.org/page/TroubleshootMacvtapHostFail
    fn _test_macvtap(hotplug: bool, guest_macvtap_name: &str, host_macvtap_name: &str) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        #[cfg(target_arch = "x86_64")]
        let kernel_path = direct_kernel_boot_path();
        #[cfg(target_arch = "aarch64")]
        let kernel_path = edk2_path();

        let phy_net = "eth0";

        // Create a macvtap interface for the guest VM to use
        assert!(exec_host_command_status(&format!(
            "sudo ip link add link {} name {} type macvtap mod bridge",
            phy_net, guest_macvtap_name
        ))
        .success());
        assert!(exec_host_command_status(&format!(
            "sudo ip link set {} address {} up",
            guest_macvtap_name, guest.network.guest_mac
        ))
        .success());
        assert!(
            exec_host_command_status(&format!("sudo ip link show {}", guest_macvtap_name))
                .success()
        );

        let tap_index =
            fs::read_to_string(format!("/sys/class/net/{}/ifindex", guest_macvtap_name)).unwrap();
        let tap_device = format!("/dev/tap{}", tap_index.trim());

        assert!(
            exec_host_command_status(&format!("sudo chown $UID.$UID {}", tap_device)).success()
        );

        let cstr_tap_device = std::ffi::CString::new(tap_device).unwrap();
        let tap_fd1 = unsafe { libc::open(cstr_tap_device.as_ptr(), libc::O_RDWR) };
        assert!(tap_fd1 > 0);
        let tap_fd2 = unsafe { libc::open(cstr_tap_device.as_ptr(), libc::O_RDWR) };
        assert!(tap_fd2 > 0);

        // Create a macvtap on the same physical net interface for
        // the host machine to use
        assert!(exec_host_command_status(&format!(
            "sudo ip link add link {} name {} type macvtap mod bridge",
            phy_net, host_macvtap_name
        ))
        .success());
        // Use default mask "255.255.255.0"
        assert!(exec_host_command_status(&format!(
            "sudo ip address add {}/24 dev {}",
            guest.network.host_ip, host_macvtap_name
        ))
        .success());
        assert!(exec_host_command_status(&format!(
            "sudo ip link set dev {} up",
            host_macvtap_name
        ))
        .success());

        let mut guest_command = GuestCommand::new(&guest);
        guest_command
            .args(["--cpus", "boot=2"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--api-socket", &api_socket]);

        let net_params = format!(
            "fd=[{},{}],mac={},num_queues=4",
            tap_fd1, tap_fd2, guest.network.guest_mac
        );

        if !hotplug {
            guest_command.args(["--net", &net_params]);
        }

        let mut child = guest_command.capture_output().spawn().unwrap();

        if hotplug {
            // Give some time to the VMM process to listen to the API
            // socket. This is the only requirement to avoid the following
            // call to ch-remote from failing.
            thread::sleep(std::time::Duration::new(10, 0));
            // Hotplug the virtio-net device
            let (cmd_success, cmd_output) =
                remote_command_w_output(&api_socket, "add-net", Some(&net_params));
            assert!(cmd_success);
            #[cfg(target_arch = "x86_64")]
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"_net2\",\"bdf\":\"0000:00:05.0\"}"));
            #[cfg(target_arch = "aarch64")]
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"_net0\",\"bdf\":\"0000:00:05.0\"}"));
        }

        // The functional connectivity provided by the virtio-net device
        // gets tested through wait_vm_boot() as it expects to receive a
        // HTTP request, and through the SSH command as well.
        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert_eq!(
                guest
                    .ssh_command("ip -o link | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                2
            );
        });

        let _ = child.kill();

        exec_host_command_status(&format!("sudo ip link del {}", guest_macvtap_name));
        exec_host_command_status(&format!("sudo ip link del {}", host_macvtap_name));

        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_macvtap() {
        _test_macvtap(false, "guestmacvtap0", "hostmacvtap0")
    }

    #[test]
    fn test_macvtap_hotplug() {
        _test_macvtap(true, "guestmacvtap1", "hostmacvtap1")
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_ovs_dpdk() {
        let focal1 = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest1 = Guest::new(Box::new(focal1));

        let focal2 = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest2 = Guest::new(Box::new(focal2));
        let api_socket_source = format!("{}.1", temp_api_path(&guest2.tmp_dir));

        let (mut child1, mut child2) =
            setup_ovs_dpdk_guests(&guest1, &guest2, &api_socket_source, false);

        // Create the snapshot directory
        let snapshot_dir = temp_snapshot_dir_path(&guest2.tmp_dir);

        let r = std::panic::catch_unwind(|| {
            // Remove one of the two ports from the OVS bridge
            assert!(exec_host_command_status("ovs-vsctl del-port vhost-user1").success());

            // Spawn a new netcat listener in the first VM
            let guest_ip = guest1.network.guest_ip.clone();
            thread::spawn(move || {
                ssh_command_ip(
                    "nc -l 12345",
                    &guest_ip,
                    DEFAULT_SSH_RETRIES,
                    DEFAULT_SSH_TIMEOUT,
                )
                .unwrap();
            });

            // Wait for the server to be listening
            thread::sleep(std::time::Duration::new(5, 0));

            // Check the connection fails this time
            assert!(guest2.ssh_command("nc -vz 172.100.0.1 12345").is_err());

            // Add the OVS port back
            assert!(exec_host_command_status("ovs-vsctl add-port ovsbr0 vhost-user1 -- set Interface vhost-user1 type=dpdkvhostuserclient options:vhost-server-path=/tmp/dpdkvhostclient1").success());

            // And finally check the connection is functional again
            guest2.ssh_command("nc -vz 172.100.0.1 12345").unwrap();

            // Pause the VM
            assert!(remote_command(&api_socket_source, "pause", None));

            // Take a snapshot from the VM
            assert!(remote_command(
                &api_socket_source,
                "snapshot",
                Some(format!("file://{}", snapshot_dir).as_str()),
            ));

            // Wait to make sure the snapshot is completed
            thread::sleep(std::time::Duration::new(10, 0));
        });

        // Shutdown the source VM
        let _ = child2.kill();
        let output = child2.wait_with_output().unwrap();
        handle_child_output(r, &output);

        // Remove the vhost-user socket file.
        Command::new("rm")
            .arg("-f")
            .arg("/tmp/dpdkvhostclient2")
            .output()
            .unwrap();

        let api_socket_restored = format!("{}.2", temp_api_path(&guest2.tmp_dir));
        // Restore the VM from the snapshot
        let mut child2 = GuestCommand::new(&guest2)
            .args(["--api-socket", &api_socket_restored])
            .args([
                "--restore",
                format!("source_url=file://{}", snapshot_dir).as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        // Wait for the VM to be restored
        thread::sleep(std::time::Duration::new(10, 0));

        let r = std::panic::catch_unwind(|| {
            // Resume the VM
            assert!(remote_command(&api_socket_restored, "resume", None));

            // Spawn a new netcat listener in the first VM
            let guest_ip = guest1.network.guest_ip.clone();
            thread::spawn(move || {
                ssh_command_ip(
                    "nc -l 12345",
                    &guest_ip,
                    DEFAULT_SSH_RETRIES,
                    DEFAULT_SSH_TIMEOUT,
                )
                .unwrap();
            });

            // Wait for the server to be listening
            thread::sleep(std::time::Duration::new(5, 0));

            // And check the connection is still functional after restore
            guest2.ssh_command("nc -vz 172.100.0.1 12345").unwrap();
        });

        let _ = child1.kill();
        let _ = child2.kill();

        let output = child1.wait_with_output().unwrap();
        child2.wait().unwrap();

        cleanup_ovs_dpdk();

        handle_child_output(r, &output);
    }

    fn setup_spdk_nvme(nvme_dir: &std::path::Path) {
        cleanup_spdk_nvme();

        assert!(exec_host_command_status(&format!(
            "mkdir -p {}",
            nvme_dir.join("nvme-vfio-user").to_str().unwrap()
        ))
        .success());
        assert!(exec_host_command_status(&format!(
            "truncate {} -s 128M",
            nvme_dir.join("test-disk.raw").to_str().unwrap()
        ))
        .success());
        assert!(exec_host_command_status(&format!(
            "mkfs.ext4 {}",
            nvme_dir.join("test-disk.raw").to_str().unwrap()
        ))
        .success());

        // Start the SPDK nvmf_tgt daemon to present NVMe device as a VFIO user device
        Command::new("/usr/local/bin/spdk-nvme/nvmf_tgt")
            .args(["-i", "0", "-m", "0x1"])
            .spawn()
            .unwrap();
        thread::sleep(std::time::Duration::new(2, 0));

        assert!(exec_host_command_status(
            "/usr/local/bin/spdk-nvme/rpc.py nvmf_create_transport -t VFIOUSER"
        )
        .success());
        assert!(exec_host_command_status(&format!(
            "/usr/local/bin/spdk-nvme/rpc.py bdev_aio_create {} test 512",
            nvme_dir.join("test-disk.raw").to_str().unwrap()
        ))
        .success());
        assert!(exec_host_command_status(
                "/usr/local/bin/spdk-nvme/rpc.py nvmf_create_subsystem nqn.2019-07.io.spdk:cnode -a -s test"
            )
            .success());
        assert!(exec_host_command_status(
            "/usr/local/bin/spdk-nvme/rpc.py nvmf_subsystem_add_ns nqn.2019-07.io.spdk:cnode test"
        )
        .success());
        assert!(exec_host_command_status(&format!(
                "/usr/local/bin/spdk-nvme/rpc.py nvmf_subsystem_add_listener nqn.2019-07.io.spdk:cnode -t VFIOUSER -a {} -s 0",
                nvme_dir.join("nvme-vfio-user").to_str().unwrap()
            ))
            .success());
    }

    fn cleanup_spdk_nvme() {
        exec_host_command_status("pkill -f nvmf_tgt");
    }

    #[test]
    fn test_vfio_user() {
        let jammy_image = JAMMY_IMAGE_NAME.to_string();
        let jammy = UbuntuDiskConfig::new(jammy_image);
        let guest = Guest::new(Box::new(jammy));

        let spdk_nvme_dir = guest.tmp_dir.as_path().join("test-vfio-user");
        setup_spdk_nvme(spdk_nvme_dir.as_path());

        let api_socket = temp_api_path(&guest.tmp_dir);
        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M,shared=on,hugepages=on"])
            .args(["--kernel", fw_path(FwType::RustHypervisorFirmware).as_str()])
            .args(["--serial", "tty", "--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Hotplug the SPDK-NVMe device to the VM
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-user-device",
                Some(&format!(
                    "socket={},id=vfio_user0",
                    spdk_nvme_dir
                        .as_path()
                        .join("nvme-vfio-user/cntrl")
                        .to_str()
                        .unwrap(),
                )),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"vfio_user0\",\"bdf\":\"0000:00:05.0\"}"));

            thread::sleep(std::time::Duration::new(10, 0));

            // Check both if /dev/nvme exists and if the block size is 128M.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep nvme0n1 | grep -c 128M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check changes persist after reboot
            assert_eq!(
                guest.ssh_command("sudo mount /dev/nvme0n1 /mnt").unwrap(),
                ""
            );
            assert_eq!(guest.ssh_command("ls /mnt").unwrap(), "lost+found\n");
            guest
                .ssh_command("echo test123 | sudo tee /mnt/test")
                .unwrap();
            assert_eq!(guest.ssh_command("sudo umount /mnt").unwrap(), "");
            assert_eq!(guest.ssh_command("ls /mnt").unwrap(), "");

            // guest.reboot_linux(0, None);
            // assert_eq!(
            //     guest.ssh_command("sudo mount /dev/nvme0n1 /mnt").unwrap(),
            //     ""
            // );
            // assert_eq!(
            //     guest.ssh_command("sudo cat /mnt/test").unwrap().trim(),
            //     "test123"
            // );
        });

        cleanup_spdk_nvme();

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_vdpa_block() {
        // Before trying to run the test, verify the vdpa_sim_blk module is correctly loaded.
        if !exec_host_command_status("lsmod | grep vdpa_sim_blk").success() {
            return;
        }

        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=2"])
            .args(["--memory", "size=512M,hugepages=on"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--vdpa", "path=/dev/vhost-vdpa-0,num_queues=1"])
            .args(["--platform", "num_pci_segments=2,iommu_segments=1"])
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check both if /dev/vdc exists and if the block size is 128M.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdc | grep -c 128M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check the content of the block device after we wrote to it.
            // The vpda-sim-blk should let us read what we previously wrote.
            guest
                .ssh_command("sudo bash -c 'echo foobar > /dev/vdc'")
                .unwrap();
            assert_eq!(
                guest.ssh_command("sudo head -1 /dev/vdc").unwrap().trim(),
                "foobar"
            );

            // Hotplug an extra vDPA block device behind the vIOMMU
            // Add a new vDPA device to the VM
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-vdpa",
                Some("id=myvdpa0,path=/dev/vhost-vdpa-1,num_queues=1,pci_segment=1,iommu=on"),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"myvdpa0\",\"bdf\":\"0001:00:01.0\"}"));

            thread::sleep(std::time::Duration::new(10, 0));

            // Check IOMMU setup
            assert!(guest
                .does_device_vendor_pair_match("0x1057", "0x1af4")
                .unwrap_or_default());
            assert_eq!(
                guest
                    .ssh_command("ls /sys/kernel/iommu_groups/0/devices")
                    .unwrap()
                    .trim(),
                "0001:00:01.0"
            );

            // Check both if /dev/vdd exists and if the block size is 128M.
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep vdd | grep -c 128M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Write some content to the block device we've just plugged.
            guest
                .ssh_command("sudo bash -c 'echo foobar > /dev/vdd'")
                .unwrap();

            // Check we can read the content back.
            assert_eq!(
                guest.ssh_command("sudo head -1 /dev/vdd").unwrap().trim(),
                "foobar"
            );

            // Unplug the device
            let cmd_success = remote_command(&api_socket, "remove-device", Some("myvdpa0"));
            assert!(cmd_success);
            thread::sleep(std::time::Duration::new(10, 0));

            // Check /dev/vdd doesn't exist anymore
            assert_eq!(
                guest
                    .ssh_command("lsblk | grep -c vdd || true")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(1),
                0
            );
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_vdpa_net() {
        // Before trying to run the test, verify the vdpa_sim_net module is correctly loaded.
        if !exec_host_command_status("lsmod | grep vdpa_sim_net").success() {
            return;
        }

        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let kernel_path = direct_kernel_boot_path();

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=2"])
            .args(["--memory", "size=512M,hugepages=on"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--vdpa", "path=/dev/vhost-vdpa-2,num_queues=2"])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check we can find network interface related to vDPA device
            assert_eq!(
                guest
                    .ssh_command("ip -o link | grep -c ens6")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(0),
                1
            );

            guest
                .ssh_command("sudo ip addr add 172.16.1.2/24 dev ens6")
                .unwrap();
            guest.ssh_command("sudo ip link set up dev ens6").unwrap();

            // Check there is no packet yet on both TX/RX of the network interface
            assert_eq!(
                guest
                    .ssh_command("ip -j -p -s link show ens6 | grep -c '\"packets\": 0'")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(0),
                2
            );

            // Send 6 packets with ping command
            guest.ssh_command("ping 172.16.1.10 -c 6 || true").unwrap();

            // Check we can find 6 packets on both TX/RX of the network interface
            assert_eq!(
                guest
                    .ssh_command("ip -j -p -s link show ens6 | grep -c '\"packets\": 6'")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or(0),
                2
            );

            // No need to check for hotplug as we already tested it through
            // test_vdpa_block()
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }
}

mod common_sequential {
    use vmm::vm_config::{DiskConfig, FsConfig, NetConfig, PmemConfig, VsockConfig};

    use crate::*;

    #[test]
    fn test_memory_mergeable_on() {
        test_memory_mergeable(true)
    }

    #[test]
    fn test_snapshot_restore_from_config() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();

        let api_socket_source = format!("{}.1", temp_api_path(&guest.tmp_dir));
        let vsock_id = "_vsock0";

        let net_id = "net123";
        let tap_name = "vmtap0";
        let net_params = format!(
            "id={},tap={},mac={},ip={},mask=255.255.255.0",
            net_id, tap_name, guest.network.guest_mac, guest.network.host_ip
        );

        let socket = temp_vsock_path(&guest.tmp_dir);
        let event_path = temp_event_monitor_path(&guest.tmp_dir);

        let pmem_id = "pmem0";
        let pmem_temp_file = TempFile::new().unwrap();
        pmem_temp_file.as_file().set_len(128 << 20).unwrap();
        Command::new("mkfs.ext4")
            .arg(pmem_temp_file.as_path())
            .output()
            .expect("Expect creating disk image to succeed");

        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");
        // Prepare native virtiofs
        let mut shared_dir = workload_path.clone();
        shared_dir.push("shared_dir");
        let fs_params = format!(
            "id=myfs0,tag=myfs,native=true,shared_dir={},cache=always,num_queues=1,queue_size=1024",
            shared_dir.to_str().unwrap()
        );

        let mut child = GuestCommand::new(&guest)
            .args(["--api-socket", &api_socket_source])
            .args(["--event-monitor", format!("path={}", event_path).as_str()])
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=1G"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--net", net_params.as_str()])
            .args([
                "--disk",
                format!(
                    "id=osdisk,path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "id=cloudinit,path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
            ])
            .args([
                "--pmem",
                format!(
                    "id={},file={}",
                    pmem_id,
                    pmem_temp_file.as_path().to_str().unwrap(),
                )
                .as_str(),
            ])
            .args(["--fs", fs_params.as_str()])
            .args([
                "--vsock",
                format!("cid=3,id={},socket={}", vsock_id, socket).as_str(),
            ])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .capture_output()
            .spawn()
            .unwrap();

        // Create the snapshot directory
        let snapshot_dir = temp_snapshot_dir_path(&guest.tmp_dir);

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check tap device name
            let tap_count = exec_host_command_output(&format!("ip link | grep -c {}", tap_name));
            assert_eq!(String::from_utf8_lossy(&tap_count.stdout).trim(), "1");

            snapshot_and_check_events(
                api_socket_source.as_str(),
                snapshot_dir.as_str(),
                event_path.as_str(),
            );
        });

        // Shutdown the source VM and check console output
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);

        // Remove the vsock socket file.
        Command::new("rm")
            .arg("-f")
            .arg(socket.as_str())
            .output()
            .unwrap();

        // Prepare restore config
        // vsock config
        let socket_restored = format!("{}.2", temp_vsock_path(&guest.tmp_dir));
        let vsock_config = VsockConfig::parse(
            format!("cid=3,id={},socket={}", vsock_id, socket_restored).as_str(),
        )
        .unwrap();

        // net config
        let tap_name_restored = "restore-tap";
        let net_params_restored = format!(
            "id={},tap={},mac={},ip={},mask=255.255.255.0",
            net_id, tap_name_restored, guest.network.guest_mac, guest.network.host_ip
        );
        let net_config = NetConfig::parse(net_params_restored.as_str()).unwrap();

        // disk config
        let mut osdisk_base_path = workload_path.clone();
        osdisk_base_path.push(FOCAL_IMAGE_NAME);
        let osdisk_path_restored = String::from(
            guest
                .tmp_dir
                .as_path()
                .join("osdisk2.img")
                .to_str()
                .unwrap(),
        );
        rate_limited_copy(osdisk_base_path, &osdisk_path_restored)
            .expect("copying of OS source disk image failed");
        let disk_params_restored = format!("id=osdisk,path={}", osdisk_path_restored);
        let disk_config = DiskConfig::parse(disk_params_restored.as_str()).unwrap();

        // pmem config
        let pmem_temp_file_retored = TempFile::new().unwrap();
        pmem_temp_file_retored.as_file().set_len(128 << 20).unwrap();
        std::process::Command::new("mkfs.ext4")
            .arg(pmem_temp_file_retored.as_path())
            .output()
            .expect("Expect creating disk image to succeed");
        let pmem_config = PmemConfig::parse(
            format!(
                "id={},file={}",
                pmem_id,
                pmem_temp_file_retored.as_path().to_str().unwrap()
            )
            .as_str(),
        )
        .unwrap();

        // fs config
        let mut shared_dir_restored = workload_path.clone();
        shared_dir_restored.push("restored_shared_dir");
        let fs_params = format!(
            "id=myfs0,tag=myfs,native=true,shared_dir={},cache=always,num_queues=1,queue_size=1024",
            shared_dir_restored.to_str().unwrap()
        );
        let fs_config = FsConfig::parse(fs_params.as_str()).unwrap();

        _test_restore_from_config(
            clh_command("cube-hypervisor").as_str(),
            &guest,
            snapshot_dir,
            Some(vsock_config),
            Some(vec![net_config]),
            Some(vec![disk_config]),
            Some(vec![pmem_config]),
            Some(vec![fs_config]),
        );
    }
}

mod compatibility {
    use crate::_test_snapshot_restore_from_different_binary;
    use test_infra::clh_command;

    #[test]
    fn test_snapshot_from_release_restore_on_head() {
        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");
        let mut release_path = workload_path.clone();
        release_path.push("cube-release");
        release_path.push("cube-hypervisor");

        let head_path = clh_command("cube-hypervisor");
        _test_snapshot_restore_from_different_binary(
            release_path.to_str().unwrap(),
            head_path.as_str(),
        );
    }
}

mod vmm_instance {
    use std::fs::File;
    use std::io::Write;
    use std::path::PathBuf;
    #[cfg(feature = "lib_support")]
    use std::sync::mpsc::channel;
    use std::sync::Arc;
    use std::thread;
    use std::time::Duration;

    #[cfg(feature = "lib_support")]
    use cube_hypervisor::vmm_config::EventNotifyConfig;
    use cube_hypervisor::vmm_config::{EventMonitorConfig, VmmConfig};
    #[cfg(feature = "lib_support")]
    use cube_hypervisor::NotifyEvent;
    use cube_hypervisor::VmmInstance;
    use log::{info, LevelFilter};
    use rlimit::{setrlimit, Resource};
    use test_infra::{ssh_command_ip, DiskType, Guest, UbuntuDiskConfig};
    use vmm::api::{ApiRequest, ApiResponsePayload, VmSnapshotConfig};
    use vmm::config::RestoreConfig;
    use vmm::vm_config::{
        ConsoleConfig, ConsoleOutputMode, CpusConfig, DiskConfig, NetConfig, PayloadConfig,
        VmConfig,
    };

    use crate::x86_64::FOCAL_IMAGE_NAME;
    use crate::{
        check_latest_events_exact, check_sequential_events, direct_kernel_boot_path,
        temp_event_monitor_path, temp_snapshot_dir_path, MetaEvent, DIRECT_KERNEL_BOOT_CMDLINE,
    };

    struct DummyVmm {
        vmm: VmmInstance,
    }

    impl DummyVmm {
        pub fn new(config: VmmConfig) -> Self {
            let vmm = VmmInstance::new(config).unwrap();
            Self { vmm }
        }

        pub fn send_request(&self, req: ApiRequest) -> bool {
            let req_info = format!("req {req:?}");
            let response = self.vmm.send_request(req).expect("send request error");
            match response {
                Ok(body) => match body {
                    ApiResponsePayload::Empty => true,
                    ApiResponsePayload::VmInfo(_vm_info) => true,
                    ApiResponsePayload::VmmPing(_ping_res) => true,
                    ApiResponsePayload::VmWaitStart(_wait_start_res) => true,
                    ApiResponsePayload::VmAction(_) => true,
                },
                Err(_) => {
                    println!("error: {req_info}");
                    false
                }
            }
        }

        pub fn join(&mut self) {
            self.vmm.join().unwrap();
        }

        pub fn join_timeout(&mut self, timeout: Option<Duration>) {
            self.vmm.join_timeout(timeout).unwrap();
        }
    }

    fn default_vmm_config() -> VmmConfig {
        VmmConfig {
            log_stderr: true,
            log_level: LevelFilter::Info,
            ..Default::default()
        }
    }

    fn default_vm_config() -> VmConfig {
        VmConfig {
            payload: Some(PayloadConfig {
                kernel: Some(direct_kernel_boot_path()),
                cmdline: Some(String::from(DIRECT_KERNEL_BOOT_CMDLINE)),
                ..Default::default()
            }),
            serial: ConsoleConfig {
                mode: ConsoleOutputMode::Tty,
                ..Default::default()
            },
            console: ConsoleConfig {
                mode: ConsoleOutputMode::Off,
                ..Default::default()
            },
            ..Default::default()
        }
    }

    fn dummy_vmm_setup() {
        // Ensure all created files (.e.g sockets) are only accessible by this user
        // SAFETY: trivially safe
        let _ = unsafe { libc::umask(0o077) };

        // Create dummy socket and dup it to large fd, so that we could trigger
        // expand_files in kernel. And this tricky work will avoid later multi
        // thread try to invoke expand_files at same time, there will be lock
        // contention in expand_files.
        // SAFETY: FFI call
        unsafe {
            let dummy = libc::socket(libc::AF_UNIX, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0);
            if dummy > 0 {
                libc::dup2(dummy, 512);
            }
        }

        std::panic::set_hook(Box::new(|info| {
            info!("{info:?}");
            log::logger().flush();
        }));

        let coredump_limit: u64 = 2 * 1024 * 1024 * 1024;
        let coredump_filter = "0x33".to_string();
        let mut filter_file = File::options()
            .write(true)
            .open("/proc/self/coredump_filter")
            .unwrap();
        filter_file.write_all(coredump_filter.as_bytes()).unwrap();
        setrlimit(Resource::CORE, coredump_limit, coredump_limit).unwrap();

        cube_hypervisor::set_runtime_seccomp_rules(vec![
            (libc::SYS_sysinfo, vec![]),
            (libc::SYS_getcwd, vec![]),
        ]);
    }

    #[test]
    // Start cube-hypervisor with no VM parameters, only the API server running.
    // From the API: Create a VM, boot it and check that it looks as expected.
    fn test_api_create_boot_and_shutdown() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        dummy_vmm_setup();
        let vmm_config = default_vmm_config();
        let mut vmm = DummyVmm::new(vmm_config);

        // Verify VMM service is running
        assert!(vmm.send_request(ApiRequest::VmmPing));

        // Create the VM first
        let mut vm_config = default_vm_config();
        let cpu_count = 4;
        vm_config.cpus = CpusConfig {
            boot_vcpus: cpu_count,
            max_vcpus: cpu_count,
            ..Default::default()
        };
        let mut disk_config = vec![];
        disk_config.push(DiskConfig {
            path: Some(PathBuf::from(
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
            )),
            ..Default::default()
        });
        if guest.disk_config.disk(DiskType::CloudInit).is_some() {
            disk_config.push(DiskConfig {
                path: Some(PathBuf::from(
                    guest.disk_config.disk(DiskType::CloudInit).unwrap(),
                )),
                ..Default::default()
            });
        }

        vm_config.disks = Some(disk_config);

        let net_string = guest.default_net_string();
        vm_config.net = Some(vec![NetConfig::parse(net_string.as_str()).unwrap()]);

        assert!(vmm.send_request(ApiRequest::VmCreate(Box::new(vm_config))));

        // Then boot it
        assert!(vmm.send_request(ApiRequest::VmBoot));

        guest.wait_vm_boot(None).unwrap();
        // Check that the VM booted as expected
        assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
        assert!(guest.get_total_memory().unwrap_or_default() > 480_000);
        assert!(vmm.send_request(ApiRequest::VmmShutdown));
        vmm.join_timeout(Some(Duration::from_secs(5)));
    }

    #[test]
    fn test_api_delete() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        dummy_vmm_setup();
        let vmm_config = default_vmm_config();
        let mut vmm = DummyVmm::new(vmm_config);

        // Verify VMM service is running
        assert!(vmm.send_request(ApiRequest::VmmPing));

        // Create the VM first
        let mut vm_config = default_vm_config();
        let cpu_count = 4;
        vm_config.cpus = CpusConfig {
            boot_vcpus: cpu_count,
            max_vcpus: cpu_count,
            ..Default::default()
        };
        let mut disk_config = vec![];
        disk_config.push(DiskConfig {
            path: Some(PathBuf::from(
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
            )),
            ..Default::default()
        });
        if guest.disk_config.disk(DiskType::CloudInit).is_some() {
            disk_config.push(DiskConfig {
                path: Some(PathBuf::from(
                    guest.disk_config.disk(DiskType::CloudInit).unwrap(),
                )),
                ..Default::default()
            });
        }

        vm_config.disks = Some(disk_config);

        let net_string = guest.default_net_string();
        vm_config.net = Some(vec![NetConfig::parse(net_string.as_str()).unwrap()]);

        assert!(vmm.send_request(ApiRequest::VmCreate(Box::new(vm_config.clone()))));

        // Then boot it
        assert!(vmm.send_request(ApiRequest::VmBoot));

        guest.wait_vm_boot(None).unwrap();
        // Check that the VM booted as expected
        assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
        assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

        // Waiting for ssh
        thread::sleep(std::time::Duration::new(5, 0));

        assert!(vmm.send_request(ApiRequest::VmDelete));

        // Waiting for vm shuwdown
        thread::sleep(std::time::Duration::new(5, 0));

        assert!(vmm.send_request(ApiRequest::VmCreate(Box::new(vm_config))));
        assert!(vmm.send_request(ApiRequest::VmBoot));

        guest.wait_vm_boot(None).unwrap();
        // Check that the VM booted as expected
        assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
        assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

        assert!(vmm.send_request(ApiRequest::VmmShutdown));
        vmm.join_timeout(Some(Duration::from_secs(5)));
    }

    #[test]
    fn test_api_pause_resume() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        dummy_vmm_setup();
        let vmm_config = default_vmm_config();
        let mut vmm = DummyVmm::new(vmm_config);

        // Verify VMM service is running
        assert!(vmm.send_request(ApiRequest::VmmPing));

        // Create the VM first
        let mut vm_config = default_vm_config();
        let cpu_count = 4;
        vm_config.cpus = CpusConfig {
            boot_vcpus: cpu_count,
            max_vcpus: cpu_count,
            ..Default::default()
        };
        let mut disk_config = vec![];
        disk_config.push(DiskConfig {
            path: Some(PathBuf::from(
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
            )),
            ..Default::default()
        });
        if guest.disk_config.disk(DiskType::CloudInit).is_some() {
            disk_config.push(DiskConfig {
                path: Some(PathBuf::from(
                    guest.disk_config.disk(DiskType::CloudInit).unwrap(),
                )),
                ..Default::default()
            });
        }

        vm_config.disks = Some(disk_config);

        let net_string = guest.default_net_string();
        vm_config.net = Some(vec![NetConfig::parse(net_string.as_str()).unwrap()]);

        assert!(vmm.send_request(ApiRequest::VmCreate(Box::new(vm_config))));

        // Then boot it
        assert!(vmm.send_request(ApiRequest::VmBoot));

        guest.wait_vm_boot(None).unwrap();
        // Check that the VM booted as expected
        assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
        assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

        // We now pause the VM
        assert!(vmm.send_request(ApiRequest::VmPause));
        // Check pausing again fails
        assert!(!vmm.send_request(ApiRequest::VmPause));

        thread::sleep(std::time::Duration::new(1, 0));
        // SSH into the VM should fail
        assert!(ssh_command_ip(
            "grep -c processor /proc/cpuinfo",
            &guest.network.guest_ip,
            2,
            5
        )
        .is_err());

        // Resume the VM
        assert!(vmm.send_request(ApiRequest::VmResume));
        // Check resuming again fails
        assert!(!vmm.send_request(ApiRequest::VmResume));

        thread::sleep(std::time::Duration::new(1, 0));
        // Now we should be able to SSH back in and get the right number of CPUs
        assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);

        assert!(vmm.send_request(ApiRequest::VmmShutdown));
        vmm.join_timeout(Some(Duration::from_secs(5)));
    }

    #[test]
    fn test_api_snapshot_restore() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let event_path = temp_event_monitor_path(&guest.tmp_dir);
        // Create the snapshot directory
        let snapshot_dir = temp_snapshot_dir_path(&guest.tmp_dir);

        dummy_vmm_setup();
        let mut vmm_config = default_vmm_config();
        vmm_config.event_monitor = Some(EventMonitorConfig {
            path: Some(event_path.clone()),
            fd: None,
        });
        let mut vmm = DummyVmm::new(vmm_config);
        // Verify VMM service is running
        assert!(vmm.send_request(ApiRequest::VmmPing));

        // Create the VM first
        let mut vm_config = default_vm_config();
        let cpu_count = 4;
        vm_config.cpus = CpusConfig {
            boot_vcpus: cpu_count,
            max_vcpus: cpu_count,
            ..Default::default()
        };
        let mut disk_config = vec![];
        disk_config.push(DiskConfig {
            path: Some(PathBuf::from(
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
            )),
            ..Default::default()
        });
        if guest.disk_config.disk(DiskType::CloudInit).is_some() {
            disk_config.push(DiskConfig {
                path: Some(PathBuf::from(
                    guest.disk_config.disk(DiskType::CloudInit).unwrap(),
                )),
                ..Default::default()
            });
        }

        vm_config.disks = Some(disk_config);

        let net_string = guest.default_net_string();
        vm_config.net = Some(vec![NetConfig::parse(net_string.as_str()).unwrap()]);

        assert!(vmm.send_request(ApiRequest::VmCreate(Box::new(vm_config))));

        // Then boot it
        assert!(vmm.send_request(ApiRequest::VmBoot));

        guest.wait_vm_boot(None).unwrap();
        // Check that the VM booted as expected
        assert_eq!(guest.get_cpu_count().unwrap_or_default() as u8, cpu_count);
        assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

        assert!(vmm.send_request(ApiRequest::VmPause));
        let latest_events = [
            &MetaEvent {
                event: "pausing".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "paused".to_string(),
                device_id: None,
            },
        ];
        assert!(check_latest_events_exact(&latest_events, &event_path));

        let snapshot_config = VmSnapshotConfig {
            destination_url: format!("file://{snapshot_dir}"),
        };
        assert!(vmm.send_request(ApiRequest::VmSnapshot(Arc::new(snapshot_config))));
        let latest_events = [
            &MetaEvent {
                event: "snapshotting".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "snapshotted".to_string(),
                device_id: None,
            },
        ];
        assert!(check_latest_events_exact(&latest_events, &event_path));

        assert!(vmm.send_request(ApiRequest::VmDelete));

        let restore_config = RestoreConfig {
            source_url: PathBuf::from(format!("file://{snapshot_dir}")),
            ..Default::default()
        };
        assert!(vmm.send_request(ApiRequest::VmRestore(Arc::new(restore_config))));
        // Wait for the VM to be restored
        thread::sleep(std::time::Duration::new(10, 0));
        let expected_events = [&MetaEvent {
            event: "restoring".to_string(),
            device_id: None,
        }];
        assert!(check_sequential_events(&expected_events, &event_path));

        // Automatically restart the VM after it has been restored
        let latest_events = [
            &MetaEvent {
                event: "restored".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "resuming".to_string(),
                device_id: None,
            },
            &MetaEvent {
                event: "resumed".to_string(),
                device_id: None,
            },
        ];
        assert!(check_latest_events_exact(&latest_events, &event_path));
        // Perform same checks to validate VM has been properly restored
        assert_eq!(guest.get_cpu_count().unwrap_or_default(), 4);
        assert!(guest.get_total_memory().unwrap_or_default() > 480_000);

        assert!(vmm.send_request(ApiRequest::VmmShutdown));
        vmm.join_timeout(Some(Duration::from_secs(5)));
    }

    #[cfg(feature = "lib_support")]
    #[test]
    fn test_get_shutdown_notifier() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        dummy_vmm_setup();
        let mut vmm_config = default_vmm_config();
        let (sender, receiver) = channel();
        vmm_config.event_notifier = Some(EventNotifyConfig { notifier: sender });
        let vmm = DummyVmm::new(vmm_config);

        // Verify VMM service is running
        assert!(vmm.send_request(ApiRequest::VmmPing));

        // Create the VM first
        let mut vm_config = default_vm_config();
        let cpu_count = 4;
        vm_config.cpus = CpusConfig {
            boot_vcpus: cpu_count,
            max_vcpus: cpu_count,
            ..Default::default()
        };
        let mut disk_config = vec![];
        disk_config.push(DiskConfig {
            path: Some(PathBuf::from(
                guest.disk_config.disk(DiskType::OperatingSystem).unwrap(),
            )),
            ..Default::default()
        });
        if guest.disk_config.disk(DiskType::CloudInit).is_some() {
            disk_config.push(DiskConfig {
                path: Some(PathBuf::from(
                    guest.disk_config.disk(DiskType::CloudInit).unwrap(),
                )),
                ..Default::default()
            });
        }
        vm_config.disks = Some(disk_config);
        let net_string = guest.default_net_string();
        vm_config.net = Some(vec![NetConfig::parse(net_string.as_str()).unwrap()]);
        assert!(vmm.send_request(ApiRequest::VmCreate(Box::new(vm_config))));

        // Then boot it
        assert!(vmm.send_request(ApiRequest::VmBoot));

        guest.ssh_command("sudo reboot").unwrap();

        let mut recv_evt_flag = false;
        for _ in 0..3 {
            let data = receiver.recv().unwrap();
            if data == NotifyEvent::VmShutdown {
                recv_evt_flag = true;
                break;
            }
        }
        assert!(recv_evt_flag);
    }
}

mod windows {
    use crate::*;
    use once_cell::sync::Lazy;

    static NEXT_DISK_ID: Lazy<Mutex<u8>> = Lazy::new(|| Mutex::new(1));

    struct WindowsGuest {
        guest: Guest,
        auth: PasswordAuth,
    }

    trait FsType {
        const FS_FAT: u8;
        const FS_NTFS: u8;
    }
    impl FsType for WindowsGuest {
        const FS_FAT: u8 = 0;
        const FS_NTFS: u8 = 1;
    }

    impl WindowsGuest {
        fn new() -> Self {
            let disk = WindowsDiskConfig::new(WINDOWS_IMAGE_NAME.to_string());
            let guest = Guest::new(Box::new(disk));
            let auth = PasswordAuth {
                username: String::from("administrator"),
                password: String::from("Admin123"),
            };

            WindowsGuest { guest, auth }
        }

        fn guest(&self) -> &Guest {
            &self.guest
        }

        fn ssh_cmd(&self, cmd: &str) -> String {
            ssh_command_ip_with_auth(
                cmd,
                &self.auth,
                &self.guest.network.guest_ip,
                DEFAULT_SSH_RETRIES,
                DEFAULT_SSH_TIMEOUT,
            )
            .unwrap()
        }

        fn cpu_count(&self) -> u8 {
            self.ssh_cmd("powershell -Command \"(Get-CimInstance win32_computersystem).NumberOfLogicalProcessors\"")
                .trim()
                .parse::<u8>()
                .unwrap_or(0)
        }

        fn ram_size(&self) -> usize {
            self.ssh_cmd("powershell -Command \"(Get-CimInstance win32_computersystem).TotalPhysicalMemory\"")
                .trim()
                .parse::<usize>()
                .unwrap_or(0)
        }

        fn netdev_count(&self) -> u8 {
            self.ssh_cmd("powershell -Command \"netsh int ipv4 show interfaces | Select-String ethernet | Measure-Object -Line | Format-Table -HideTableHeaders\"")
                .trim()
                .parse::<u8>()
                .unwrap_or(0)
        }

        fn disk_count(&self) -> u8 {
            self.ssh_cmd("powershell -Command \"Get-Disk | Measure-Object -Line | Format-Table -HideTableHeaders\"")
                .trim()
                .parse::<u8>()
                .unwrap_or(0)
        }

        fn reboot(&self) {
            let _ = self.ssh_cmd("shutdown /r /t 0");
        }

        fn shutdown(&self) {
            let _ = self.ssh_cmd("shutdown /s /t 0");
        }

        fn run_dnsmasq(&self) -> std::process::Child {
            let listen_address = format!("--listen-address={}", self.guest.network.host_ip);
            let dhcp_host = format!(
                "--dhcp-host={},{}",
                self.guest.network.guest_mac, self.guest.network.guest_ip
            );
            let dhcp_range = format!(
                "--dhcp-range=eth,{},{}",
                self.guest.network.guest_ip, self.guest.network.guest_ip
            );

            Command::new("dnsmasq")
                .arg("--no-daemon")
                .arg("--log-queries")
                .arg(listen_address.as_str())
                .arg("--except-interface=lo")
                .arg("--bind-dynamic") // Allow listening to host_ip while the interface is not ready yet.
                .arg("--conf-file=/dev/null")
                .arg(dhcp_host.as_str())
                .arg(dhcp_range.as_str())
                .spawn()
                .unwrap()
        }

        // TODO Cleanup image file explicitly after test, if there's some space issues.
        fn disk_new(&self, fs: u8, sz: usize) -> String {
            let mut guard = NEXT_DISK_ID.lock().unwrap();
            let id = *guard;
            *guard = id + 1;

            let img = PathBuf::from(format!("/tmp/test-hotplug-{}.raw", id));
            let _ = fs::remove_file(&img);

            // Create an image file
            let out = Command::new("qemu-img")
                .args([
                    "create",
                    "-f",
                    "raw",
                    img.to_str().unwrap(),
                    format!("{}m", sz).as_str(),
                ])
                .output()
                .expect("qemu-img command failed")
                .stdout;
            println!("{:?}", out);

            // Associate image to a loop device
            let out = Command::new("losetup")
                .args(["--show", "-f", img.to_str().unwrap()])
                .output()
                .expect("failed to create loop device")
                .stdout;
            let _tmp = String::from_utf8_lossy(&out);
            let loop_dev = _tmp.trim();
            println!("{:?}", out);

            // Create a partition table
            // echo 'type=7' | sudo sfdisk "${LOOP}"
            let mut child = Command::new("sfdisk")
                .args([loop_dev])
                .stdin(Stdio::piped())
                .spawn()
                .unwrap();
            let stdin = child.stdin.as_mut().expect("failed to open stdin");
            stdin
                .write_all("type=7".as_bytes())
                .expect("failed to write stdin");
            let out = child.wait_with_output().expect("sfdisk failed").stdout;
            println!("{:?}", out);

            // Disengage the loop device
            let out = Command::new("losetup")
                .args(["-d", loop_dev])
                .output()
                .expect("loop device not found")
                .stdout;
            println!("{:?}", out);

            // Re-associate loop device pointing to the partition only
            let out = Command::new("losetup")
                .args([
                    "--show",
                    "--offset",
                    (512 * 2048).to_string().as_str(),
                    "-f",
                    img.to_str().unwrap(),
                ])
                .output()
                .expect("failed to create loop device")
                .stdout;
            let _tmp = String::from_utf8_lossy(&out);
            let loop_dev = _tmp.trim();
            println!("{:?}", out);

            // Create filesystem.
            let fs_cmd = match fs {
                WindowsGuest::FS_FAT => "mkfs.msdos",
                WindowsGuest::FS_NTFS => "mkfs.ntfs",
                _ => panic!("Unknown filesystem type '{}'", fs),
            };
            let out = Command::new(fs_cmd)
                .args([&loop_dev])
                .output()
                .unwrap_or_else(|_| panic!("{} failed", fs_cmd))
                .stdout;
            println!("{:?}", out);

            // Disengage the loop device
            let out = Command::new("losetup")
                .args(["-d", loop_dev])
                .output()
                .unwrap_or_else(|_| panic!("loop device '{}' not found", loop_dev))
                .stdout;
            println!("{:?}", out);

            img.to_str().unwrap().to_string()
        }

        fn disks_set_rw(&self) {
            let _ = self.ssh_cmd("powershell -Command \"Get-Disk | Where-Object IsOffline -eq $True | Set-Disk -IsReadOnly $False\"");
        }

        fn disks_online(&self) {
            let _ = self.ssh_cmd("powershell -Command \"Get-Disk | Where-Object IsOffline -eq $True | Set-Disk -IsOffline $False\"");
        }

        fn disk_file_put(&self, fname: &str, data: &str) {
            let _ = self.ssh_cmd(&format!(
                "powershell -Command \"'{}' | Set-Content -Path {}\"",
                data, fname
            ));
        }

        fn disk_file_read(&self, fname: &str) -> String {
            self.ssh_cmd(&format!(
                "powershell -Command \"Get-Content -Path {}\"",
                fname
            ))
        }

        fn wait_for_boot(&self) -> bool {
            let cmd = "dir /b c:\\ | find \"Windows\"";
            let tmo_max = 180;
            // The timeout increase by n*1+n*2+n*3+..., therefore the initial
            // interval must be small.
            let tmo_int = 2;
            let out = ssh_command_ip_with_auth(
                cmd,
                &self.auth,
                &self.guest.network.guest_ip,
                {
                    let mut ret = 1;
                    let mut tmo_acc = 0;
                    loop {
                        tmo_acc += tmo_int * ret;
                        if tmo_acc >= tmo_max {
                            break;
                        }
                        ret += 1;
                    }
                    ret
                },
                tmo_int,
            )
            .unwrap();

            if "Windows" == out.trim() {
                return true;
            }

            false
        }
    }

    fn vcpu_threads_count(pid: u32) -> u8 {
        // ps -T -p 12345 | grep vcpu | wc -l
        let out = Command::new("ps")
            .args(["-T", "-p", format!("{}", pid).as_str()])
            .output()
            .expect("ps command failed")
            .stdout;
        return String::from_utf8_lossy(&out).matches("vcpu").count() as u8;
    }

    fn netdev_ctrl_threads_count(pid: u32) -> u8 {
        // ps -T -p 12345 | grep "_net[0-9]*_ctrl" | wc -l
        let out = Command::new("ps")
            .args(["-T", "-p", format!("{}", pid).as_str()])
            .output()
            .expect("ps command failed")
            .stdout;
        let mut n = 0;
        String::from_utf8_lossy(&out)
            .split_whitespace()
            .for_each(|s| n += (s.starts_with("_net") && s.ends_with("_ctrl")) as u8); // _net1_ctrl
        n
    }

    fn disk_ctrl_threads_count(pid: u32) -> u8 {
        // ps -T -p 15782  | grep "_disk[0-9]*_q0" | wc -l
        let out = Command::new("ps")
            .args(["-T", "-p", format!("{}", pid).as_str()])
            .output()
            .expect("ps command failed")
            .stdout;
        let mut n = 0;
        String::from_utf8_lossy(&out)
            .split_whitespace()
            .for_each(|s| n += (s.starts_with("_disk") && s.ends_with("_q0")) as u8); // _disk0_q0, don't care about multiple queues as they're related to the same hdd
        n
    }

    #[test]
    fn test_windows_guest() {
        let windows_guest = WindowsGuest::new();

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--cpus", "boot=2,kvm_hyperv=on"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", edk2_path().to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let fd = child.stdout.as_ref().unwrap().as_raw_fd();
        let pipesize = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };
        let fd = child.stderr.as_ref().unwrap().as_raw_fd();
        let pipesize1 = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };

        assert!(pipesize >= PIPE_SIZE && pipesize1 >= PIPE_SIZE);

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_windows_guest_multiple_queues() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--cpus", "boot=4,kvm_hyperv=on"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .args([
                "--disk",
                format!(
                    "path={},num_queues=4",
                    windows_guest
                        .guest()
                        .disk_config
                        .disk(DiskType::OperatingSystem)
                        .unwrap()
                )
                .as_str(),
            ])
            .args([
                "--net",
                format!(
                    "tap=,mac={},ip={},mask=255.255.255.0,num_queues=8",
                    windows_guest.guest().network.guest_mac,
                    windows_guest.guest().network.host_ip
                )
                .as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        let fd = child.stdout.as_ref().unwrap().as_raw_fd();
        let pipesize = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };
        let fd = child.stderr.as_ref().unwrap().as_raw_fd();
        let pipesize1 = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };

        assert!(pipesize >= PIPE_SIZE && pipesize1 >= PIPE_SIZE);

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    #[ignore = "See #4327"]
    fn test_windows_guest_snapshot_restore() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();
        let api_socket_source = format!("{}.1", temp_api_path(&tmp_dir));

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket_source])
            .args(["--cpus", "boot=2,kvm_hyperv=on"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let fd = child.stdout.as_ref().unwrap().as_raw_fd();
        let pipesize = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };
        let fd = child.stderr.as_ref().unwrap().as_raw_fd();
        let pipesize1 = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, PIPE_SIZE) };

        assert!(pipesize >= PIPE_SIZE && pipesize1 >= PIPE_SIZE);

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        // Wait to make sure Windows boots up
        assert!(windows_guest.wait_for_boot());

        let snapshot_dir = temp_snapshot_dir_path(&tmp_dir);

        // Pause the VM
        assert!(remote_command(&api_socket_source, "pause", None));

        // Take a snapshot from the VM
        assert!(remote_command(
            &api_socket_source,
            "snapshot",
            Some(format!("file://{}", snapshot_dir).as_str()),
        ));

        // Wait to make sure the snapshot is completed
        thread::sleep(std::time::Duration::new(30, 0));

        let _ = child.kill();
        child.wait().unwrap();

        let api_socket_restored = format!("{}.2", temp_api_path(&tmp_dir));

        // Restore the VM from the snapshot
        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket_restored])
            .args([
                "--restore",
                format!("source_url=file://{}", snapshot_dir).as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        // Wait for the VM to be restored
        thread::sleep(std::time::Duration::new(20, 0));

        let r = std::panic::catch_unwind(|| {
            // Resume the VM
            assert!(remote_command(&api_socket_restored, "resume", None));

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    #[cfg(not(target_arch = "aarch64"))]
    fn test_windows_guest_cpu_hotplug() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();
        let api_socket = temp_api_path(&tmp_dir);

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=2,max=8,kvm_hyperv=on"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            let vcpu_num = 2;
            // Check the initial number of CPUs the guest sees
            assert_eq!(windows_guest.cpu_count(), vcpu_num);
            // Check the initial number of vcpu threads in the CH process
            assert_eq!(vcpu_threads_count(child.id()), vcpu_num);

            let vcpu_num = 6;
            // Hotplug some CPUs
            resize_command(&api_socket, Some(vcpu_num), None, None, None);
            // Wait to make sure CPUs are added
            thread::sleep(std::time::Duration::new(10, 0));
            // Check the guest sees the correct number
            assert_eq!(windows_guest.cpu_count(), vcpu_num);
            // Check the CH process has the correct number of vcpu threads
            assert_eq!(vcpu_threads_count(child.id()), vcpu_num);

            let vcpu_num = 4;
            // Remove some CPUs. Note that Windows doesn't support hot-remove.
            resize_command(&api_socket, Some(vcpu_num), None, None, None);
            // Wait to make sure CPUs are removed
            thread::sleep(std::time::Duration::new(10, 0));
            // Reboot to let Windows catch up
            windows_guest.reboot();
            // Wait to make sure Windows completely rebooted
            thread::sleep(std::time::Duration::new(60, 0));
            // Check the guest sees the correct number
            assert_eq!(windows_guest.cpu_count(), vcpu_num);
            // Check the CH process has the correct number of vcpu threads
            assert_eq!(vcpu_threads_count(child.id()), vcpu_num);

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    #[cfg(not(target_arch = "aarch64"))]
    fn test_windows_guest_ram_hotplug() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();
        let api_socket = temp_api_path(&tmp_dir);

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=2,kvm_hyperv=on"])
            .args(["--memory", "size=2G,hotplug_size=5G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            let ram_size = 2 * 1024 * 1024 * 1024;
            // Check the initial number of RAM the guest sees
            let current_ram_size = windows_guest.ram_size();
            // This size seems to be reserved by the system and thus the
            // reported amount differs by this constant value.
            let reserved_ram_size = ram_size - current_ram_size;
            // Verify that there's not more than 4mb constant diff wasted
            // by the reserved ram.
            assert!(reserved_ram_size < 4 * 1024 * 1024);

            let ram_size = 4 * 1024 * 1024 * 1024;
            // Hotplug some RAM
            resize_command(&api_socket, None, Some(ram_size), None, None);
            // Wait to make sure RAM has been added
            thread::sleep(std::time::Duration::new(10, 0));
            // Check the guest sees the correct number
            assert_eq!(windows_guest.ram_size(), ram_size - reserved_ram_size);

            let ram_size = 3 * 1024 * 1024 * 1024;
            // Unplug some RAM. Note that hot-remove most likely won't work.
            resize_command(&api_socket, None, Some(ram_size), None, None);
            // Wait to make sure RAM has been added
            thread::sleep(std::time::Duration::new(10, 0));
            // Reboot to let Windows catch up
            windows_guest.reboot();
            // Wait to make sure guest completely rebooted
            thread::sleep(std::time::Duration::new(60, 0));
            // Check the guest sees the correct number
            assert_eq!(windows_guest.ram_size(), ram_size - reserved_ram_size);

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    fn test_windows_guest_netdev_hotplug() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();
        let api_socket = temp_api_path(&tmp_dir);

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=2,kvm_hyperv=on"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            // Initially present network device
            let netdev_num = 1;
            assert_eq!(windows_guest.netdev_count(), netdev_num);
            assert_eq!(netdev_ctrl_threads_count(child.id()), netdev_num);

            // Hotplug network device
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-net",
                Some(windows_guest.guest().default_net_string().as_str()),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output).contains("\"id\":\"_net2\""));
            thread::sleep(std::time::Duration::new(5, 0));
            // Verify the device  is on the system
            let netdev_num = 2;
            assert_eq!(windows_guest.netdev_count(), netdev_num);
            assert_eq!(netdev_ctrl_threads_count(child.id()), netdev_num);

            // Remove network device
            let cmd_success = remote_command(&api_socket, "remove-device", Some("_net2"));
            assert!(cmd_success);
            thread::sleep(std::time::Duration::new(5, 0));
            // Verify the device has been removed
            let netdev_num = 1;
            assert_eq!(windows_guest.netdev_count(), netdev_num);
            assert_eq!(netdev_ctrl_threads_count(child.id()), netdev_num);

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    #[cfg(not(target_arch = "aarch64"))]
    fn test_windows_guest_disk_hotplug() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();
        let api_socket = temp_api_path(&tmp_dir);

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=2,kvm_hyperv=on"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        let disk = windows_guest.disk_new(WindowsGuest::FS_FAT, 100);

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            // Initially present disk device
            let disk_num = 1;
            assert_eq!(windows_guest.disk_count(), disk_num);
            assert_eq!(disk_ctrl_threads_count(child.id()), disk_num);

            // Hotplug disk device
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-disk",
                Some(format!("path={},readonly=off", disk).as_str()),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output).contains("\"id\":\"_disk2\""));
            thread::sleep(std::time::Duration::new(5, 0));
            // Online disk device
            windows_guest.disks_set_rw();
            windows_guest.disks_online();
            // Verify the device is on the system
            let disk_num = 2;
            assert_eq!(windows_guest.disk_count(), disk_num);
            assert_eq!(disk_ctrl_threads_count(child.id()), disk_num);

            let data = "hello";
            let fname = "d:\\world";
            windows_guest.disk_file_put(fname, data);

            // Unmount disk device
            let cmd_success = remote_command(&api_socket, "remove-device", Some("_disk2"));
            assert!(cmd_success);
            thread::sleep(std::time::Duration::new(5, 0));
            // Verify the device has been removed
            let disk_num = 1;
            assert_eq!(windows_guest.disk_count(), disk_num);
            assert_eq!(disk_ctrl_threads_count(child.id()), disk_num);

            // Remount and check the file exists with the expected contents
            let (cmd_success, _cmd_output) = remote_command_w_output(
                &api_socket,
                "add-disk",
                Some(format!("path={},readonly=off", disk).as_str()),
            );
            assert!(cmd_success);
            thread::sleep(std::time::Duration::new(5, 0));
            let out = windows_guest.disk_file_read(fname);
            assert_eq!(data, out.trim());

            // Intentionally no unmount, it'll happen at shutdown.

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    #[cfg(not(target_arch = "aarch64"))]
    fn test_windows_guest_disk_hotplug_multi() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();
        let api_socket = temp_api_path(&tmp_dir);

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=2,kvm_hyperv=on"])
            .args(["--memory", "size=2G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        // Predefined data to used at various test stages
        let disk_test_data: [[String; 4]; 2] = [
            [
                "_disk2".to_string(),
                windows_guest.disk_new(WindowsGuest::FS_FAT, 123),
                "d:\\world".to_string(),
                "hello".to_string(),
            ],
            [
                "_disk3".to_string(),
                windows_guest.disk_new(WindowsGuest::FS_NTFS, 333),
                "e:\\hello".to_string(),
                "world".to_string(),
            ],
        ];

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            // Initially present disk device
            let disk_num = 1;
            assert_eq!(windows_guest.disk_count(), disk_num);
            assert_eq!(disk_ctrl_threads_count(child.id()), disk_num);

            for it in &disk_test_data {
                let disk_id = it[0].as_str();
                let disk = it[1].as_str();
                // Hotplug disk device
                let (cmd_success, cmd_output) = remote_command_w_output(
                    &api_socket,
                    "add-disk",
                    Some(format!("path={},readonly=off", disk).as_str()),
                );
                assert!(cmd_success);
                assert!(String::from_utf8_lossy(&cmd_output)
                    .contains(format!("\"id\":\"{}\"", disk_id).as_str()));
                thread::sleep(std::time::Duration::new(5, 0));
                // Online disk devices
                windows_guest.disks_set_rw();
                windows_guest.disks_online();
            }
            // Verify the devices are on the system
            let disk_num = (disk_test_data.len() + 1) as u8;
            assert_eq!(windows_guest.disk_count(), disk_num);
            assert_eq!(disk_ctrl_threads_count(child.id()), disk_num);

            // Put test data
            for it in &disk_test_data {
                let fname = it[2].as_str();
                let data = it[3].as_str();
                windows_guest.disk_file_put(fname, data);
            }

            // Unmount disk devices
            for it in &disk_test_data {
                let disk_id = it[0].as_str();
                let cmd_success = remote_command(&api_socket, "remove-device", Some(disk_id));
                assert!(cmd_success);
                thread::sleep(std::time::Duration::new(5, 0));
            }

            // Verify the devices have been removed
            let disk_num = 1;
            assert_eq!(windows_guest.disk_count(), disk_num);
            assert_eq!(disk_ctrl_threads_count(child.id()), disk_num);

            // Remount
            for it in &disk_test_data {
                let disk = it[1].as_str();
                let (cmd_success, _cmd_output) = remote_command_w_output(
                    &api_socket,
                    "add-disk",
                    Some(format!("path={},readonly=off", disk).as_str()),
                );
                assert!(cmd_success);
                thread::sleep(std::time::Duration::new(5, 0));
            }

            // Check the files exists with the expected contents
            for it in &disk_test_data {
                let fname = it[2].as_str();
                let data = it[3].as_str();
                let out = windows_guest.disk_file_read(fname);
                assert_eq!(data, out.trim());
            }

            // Intentionally no unmount, it'll happen at shutdown.

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }

    #[test]
    #[cfg(not(feature = "mshv"))]
    #[cfg(not(target_arch = "aarch64"))]
    fn test_windows_guest_netdev_multi() {
        let windows_guest = WindowsGuest::new();

        let mut ovmf_path = dirs::home_dir().unwrap();
        ovmf_path.push("workloads");
        ovmf_path.push(OVMF_NAME);

        let tmp_dir = TempDir::new_with_prefix("/tmp/ch").unwrap();
        let api_socket = temp_api_path(&tmp_dir);

        let mut child = GuestCommand::new(windows_guest.guest())
            .args(["--api-socket", &api_socket])
            .args(["--cpus", "boot=2,kvm_hyperv=on"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", ovmf_path.to_str().unwrap()])
            .args(["--serial", "tty"])
            .args(["--console", "off"])
            .default_disks()
            // The multi net dev config is borrowed from test_multiple_network_interfaces
            .args([
                "--net",
                windows_guest.guest().default_net_string().as_str(),
                "tap=,mac=8a:6b:6f:5a:de:ac,ip=192.168.3.1,mask=255.255.255.0",
                "tap=mytap42,mac=fe:1f:9e:e1:60:f2,ip=192.168.4.1,mask=255.255.255.0",
            ])
            .capture_output()
            .spawn()
            .unwrap();

        let mut child_dnsmasq = windows_guest.run_dnsmasq();

        let r = std::panic::catch_unwind(|| {
            // Wait to make sure Windows boots up
            assert!(windows_guest.wait_for_boot());

            let netdev_num = 3;
            assert_eq!(windows_guest.netdev_count(), netdev_num);
            assert_eq!(netdev_ctrl_threads_count(child.id()), netdev_num);

            let tap_count = exec_host_command_output("ip link | grep -c mytap42");
            assert_eq!(String::from_utf8_lossy(&tap_count.stdout).trim(), "1");

            windows_guest.shutdown();
        });

        let _ = child.wait_timeout(std::time::Duration::from_secs(60));
        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        let _ = child_dnsmasq.kill();
        let _ = child_dnsmasq.wait();

        handle_child_output(r, &output);
    }
}

#[cfg(target_arch = "x86_64")]
mod sgx {
    use crate::*;

    #[test]
    fn test_sgx() {
        let focal = UbuntuDiskConfig::new(FOCAL_SGX_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let mut kernel_path = workload_path;
        kernel_path.push("vmlinux_w_sgx");

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=1"])
            .args(["--memory", "size=512M"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .default_net()
            .args(["--sgx-epc", "id=epc0,size=64M"])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check if SGX is correctly detected in the guest.
            guest.check_sgx_support().unwrap();

            // Validate the SGX EPC section is 64MiB.
            assert_eq!(
                guest
                    .ssh_command("cpuid -l 0x12 -s 2 | grep 'section size' | cut -d '=' -f 2")
                    .unwrap()
                    .trim(),
                "0x0000000004000000"
            );

            // Run a test relying on SGX enclaves and check if it runs
            // successfully.
            assert!(guest
                .ssh_command("cd /linux-sgx/SampleCode/LocalAttestation/bin/ && sudo ./app")
                .unwrap()
                .trim()
                .contains(
                    "succeed to load enclaves.\nsucceed to \
                        establish secure channel.\nSucceed to exchange \
                        secure message...\nSucceed to close Session..."
                ));
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }
}

#[cfg(target_arch = "x86_64")]
mod vfio {
    use crate::*;

    #[test]
    // The VFIO integration test starts cloud-hypervisor guest with 3 TAP
    // backed networking interfaces, bound through a simple bridge on the host.
    // So if the nested cloud-hypervisor succeeds in getting a directly
    // assigned interface from its cloud-hypervisor host, we should be able to
    // ssh into it, and verify that it's running with the right kernel command
    // line (We tag the command line from cloud-hypervisor for that purpose).
    // The third device is added to validate that hotplug works correctly since
    // it is being added to the L2 VM through hotplugging mechanism.
    // Also, we pass-through a vitio-blk device to the L2 VM to test the 32-bit
    // vfio device support
    fn test_vfio() {
        setup_vfio_network_interfaces();

        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new_from_ip_range(Box::new(focal), "172.18", 0);

        let mut workload_path = dirs::home_dir().unwrap();
        workload_path.push("workloads");

        let kernel_path = direct_kernel_boot_path();

        let mut vfio_path = workload_path.clone();
        vfio_path.push("vfio");

        let mut cloud_init_vfio_base_path = vfio_path.clone();
        cloud_init_vfio_base_path.push("cloudinit.img");

        // We copy our cloudinit into the vfio mount point, for the nested
        // cloud-hypervisor guest to use.
        rate_limited_copy(
            guest.disk_config.disk(DiskType::CloudInit).unwrap(),
            &cloud_init_vfio_base_path,
        )
        .expect("copying of cloud-init disk failed");

        let mut vfio_disk_path = workload_path.clone();
        vfio_disk_path.push("vfio.img");

        // Create the vfio disk image
        let output = Command::new("mkfs.ext4")
            .arg("-d")
            .arg(vfio_path.to_str().unwrap())
            .arg(vfio_disk_path.to_str().unwrap())
            .arg("2g")
            .output()
            .unwrap();
        if !output.status.success() {
            eprintln!("{}", String::from_utf8_lossy(&output.stderr));
            panic!("mkfs.ext4 command generated an error");
        }

        let mut blk_file_path = workload_path;
        blk_file_path.push("blk.img");

        let vfio_tap0 = "vfio-tap0";
        let vfio_tap1 = "vfio-tap1";
        let vfio_tap2 = "vfio-tap2";
        let vfio_tap3 = "vfio-tap3";

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=4"])
            .args(["--memory", "size=2G,hugepages=on,shared=on"])
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
                format!("path={}", vfio_disk_path.to_str().unwrap()).as_str(),
                format!("path={},iommu=on", blk_file_path.to_str().unwrap()).as_str(),
            ])
            .args([
                "--cmdline",
                format!(
                    "{} kvm-intel.nested=1 vfio_iommu_type1.allow_unsafe_interrupts",
                    DIRECT_KERNEL_BOOT_CMDLINE
                )
                .as_str(),
            ])
            .args([
                "--net",
                format!("tap={},mac={}", vfio_tap0, guest.network.guest_mac).as_str(),
                format!(
                    "tap={},mac={},iommu=on",
                    vfio_tap1, guest.network.l2_guest_mac1
                )
                .as_str(),
                format!(
                    "tap={},mac={},iommu=on",
                    vfio_tap2, guest.network.l2_guest_mac2
                )
                .as_str(),
                format!(
                    "tap={},mac={},iommu=on",
                    vfio_tap3, guest.network.l2_guest_mac3
                )
                .as_str(),
            ])
            .capture_output()
            .spawn()
            .unwrap();

        thread::sleep(std::time::Duration::new(30, 0));

        let r = std::panic::catch_unwind(|| {
            guest.ssh_command_l1("sudo systemctl start vfio").unwrap();
            thread::sleep(std::time::Duration::new(120, 0));

            // We booted our cloud hypervisor L2 guest with a "VFIOTAG" tag
            // added to its kernel command line.
            // Let's ssh into it and verify that it's there. If it is it means
            // we're in the right guest (The L2 one) because the QEMU L1 guest
            // does not have this command line tag.
            assert_eq!(
                guest
                    .ssh_command_l2_1("grep -c VFIOTAG /proc/cmdline")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Let's also verify from the second virtio-net device passed to
            // the L2 VM.
            assert_eq!(
                guest
                    .ssh_command_l2_2("grep -c VFIOTAG /proc/cmdline")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check the amount of PCI devices appearing in L2 VM.
            assert_eq!(
                guest
                    .ssh_command_l2_1("ls /sys/bus/pci/devices | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                8,
            );

            // Check both if /dev/vdc exists and if the block size is 16M in L2 VM
            assert_eq!(
                guest
                    .ssh_command_l2_1("lsblk | grep vdc | grep -c 16M")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Hotplug an extra virtio-net device through L2 VM.
            guest
                .ssh_command_l1(
                    "echo 0000:00:09.0 | sudo tee /sys/bus/pci/devices/0000:00:09.0/driver/unbind",
                )
                .unwrap();
            guest
                .ssh_command_l1("echo 0000:00:09.0 | sudo tee /sys/bus/pci/drivers/vfio-pci/bind")
                .unwrap();
            let vfio_hotplug_output = guest
                .ssh_command_l1(
                    "sudo /mnt/ch-remote \
                 --api-socket=/tmp/ch_api.sock \
                 add-device path=/sys/bus/pci/devices/0000:00:09.0,id=vfio123",
                )
                .unwrap();
            assert!(vfio_hotplug_output.contains("{\"id\":\"vfio123\",\"bdf\":\"0000:00:08.0\"}"));

            thread::sleep(std::time::Duration::new(10, 0));

            // Let's also verify from the third virtio-net device passed to
            // the L2 VM. This third device has been hotplugged through the L2
            // VM, so this is our way to validate hotplug works for VFIO PCI.
            assert_eq!(
                guest
                    .ssh_command_l2_3("grep -c VFIOTAG /proc/cmdline")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                1
            );

            // Check the amount of PCI devices appearing in L2 VM.
            // There should be one more device than before, raising the count
            // up to 9 PCI devices.
            assert_eq!(
                guest
                    .ssh_command_l2_1("ls /sys/bus/pci/devices | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                9,
            );

            // Let's now verify that we can correctly remove the virtio-net
            // device through the "remove-device" command responsible for
            // unplugging VFIO devices.
            guest
                .ssh_command_l1(
                    "sudo /mnt/ch-remote \
                 --api-socket=/tmp/ch_api.sock \
                 remove-device vfio123",
                )
                .unwrap();
            thread::sleep(std::time::Duration::new(10, 0));

            // Check the amount of PCI devices appearing in L2 VM is back down
            // to 8 devices.
            assert_eq!(
                guest
                    .ssh_command_l2_1("ls /sys/bus/pci/devices | wc -l")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                8,
            );

            // Perform memory hotplug in L2 and validate the memory is showing
            // up as expected. In order to check, we will use the virtio-net
            // device already passed through L2 as a VFIO device, this will
            // verify that VFIO devices are functional with memory hotplug.
            assert!(guest.get_total_memory_l2().unwrap_or_default() > 480_000);
            guest
                .ssh_command_l2_1(
                    "sudo bash -c 'echo online > /sys/devices/system/memory/auto_online_blocks'",
                )
                .unwrap();
            guest
                .ssh_command_l1(
                    "sudo /mnt/ch-remote \
                 --api-socket=/tmp/ch_api.sock \
                 resize --memory=1073741824",
                )
                .unwrap();
            assert!(guest.get_total_memory_l2().unwrap_or_default() > 960_000);
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        cleanup_vfio_network_interfaces();

        handle_child_output(r, &output);
    }

    fn test_nvidia_card_memory_hotplug(hotplug_method: &str) {
        let hirsute = UbuntuDiskConfig::new(HIRSUTE_NVIDIA_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(hirsute));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=4"])
            .args([
                "--memory",
                format!("size=4G,hotplug_size=4G,hotplug_method={}", hotplug_method).as_str(),
            ])
            .args(["--kernel", fw_path(FwType::RustHypervisorFirmware).as_str()])
            .args(["--device", "path=/sys/bus/pci/devices/0000:31:00.0/"])
            .args(["--api-socket", &api_socket])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);

            guest.enable_memory_hotplug();

            // Add RAM to the VM
            let desired_ram = 6 << 30;
            resize_command(&api_socket, None, Some(desired_ram), None, None);
            thread::sleep(std::time::Duration::new(30, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 5_760_000);

            // Check the VFIO device works when RAM is increased to 6GiB
            guest.check_nvidia_gpu();
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_nvidia_card_memory_hotplug_acpi() {
        test_nvidia_card_memory_hotplug("acpi")
    }

    #[test]
    fn test_nvidia_card_memory_hotplug_virtio_mem() {
        test_nvidia_card_memory_hotplug("virtio-mem")
    }

    #[test]
    fn test_nvidia_card_pci_hotplug() {
        let hirsute = UbuntuDiskConfig::new(HIRSUTE_NVIDIA_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(hirsute));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=4"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", fw_path(FwType::RustHypervisorFirmware).as_str()])
            .args(["--api-socket", &api_socket])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Hotplug the card to the VM
            let (cmd_success, cmd_output) = remote_command_w_output(
                &api_socket,
                "add-device",
                Some("id=vfio0,path=/sys/bus/pci/devices/0000:31:00.0/"),
            );
            assert!(cmd_success);
            assert!(String::from_utf8_lossy(&cmd_output)
                .contains("{\"id\":\"vfio0\",\"bdf\":\"0000:00:06.0\"}"));

            thread::sleep(std::time::Duration::new(10, 0));

            // Check the VFIO device works after hotplug
            guest.check_nvidia_gpu();
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }

    #[test]
    fn test_nvidia_card_reboot() {
        let hirsute = UbuntuDiskConfig::new(HIRSUTE_NVIDIA_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(hirsute));
        let api_socket = temp_api_path(&guest.tmp_dir);

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", "boot=4"])
            .args(["--memory", "size=4G"])
            .args(["--kernel", fw_path(FwType::RustHypervisorFirmware).as_str()])
            .args(["--device", "path=/sys/bus/pci/devices/0000:31:00.0/"])
            .args(["--api-socket", &api_socket])
            .default_disks()
            .default_net()
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Check the VFIO device works after boot
            guest.check_nvidia_gpu();

            // guest.reboot_linux(0, None);
            //
            // // Check the VFIO device works after reboot
            // guest.check_nvidia_gpu();
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();

        handle_child_output(r, &output);
    }
}

mod live_migration {
    use crate::*;

    fn start_live_migration(
        migration_socket: &str,
        src_api_socket: &str,
        dest_api_socket: &str,
        local: bool,
    ) -> bool {
        // Start to receive migration from the destintion VM
        let mut receive_migration = Command::new(clh_command("ch-remote"))
            .args([
                &format!("--api-socket={}", dest_api_socket),
                "receive-migration",
                &format! {"unix:{}", migration_socket},
            ])
            .stderr(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        // Give it '1s' to make sure the 'migration_socket' file is properly created
        thread::sleep(std::time::Duration::new(1, 0));
        // Start to send migration from the source VM

        let mut args = [
            format!("--api-socket={}", &src_api_socket),
            "send-migration".to_string(),
            format! {"unix:{}", migration_socket},
        ]
        .to_vec();

        if local {
            args.insert(2, "--local".to_string());
        }

        let mut send_migration = Command::new(clh_command("ch-remote"))
            .args(&args)
            .stderr(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();

        // The 'send-migration' command should be executed successfully within the given timeout
        let send_success = if let Some(status) = send_migration
            .wait_timeout(std::time::Duration::from_secs(30))
            .unwrap()
        {
            status.success()
        } else {
            false
        };

        if !send_success {
            let _ = send_migration.kill();
            let output = send_migration.wait_with_output().unwrap();
            eprintln!("\n\n==== Start 'send_migration' output ====\n\n---stdout---\n{}\n\n---stderr---\n{}\n\n==== End 'send_migration' output ====\n\n",
                    String::from_utf8_lossy(&output.stdout), String::from_utf8_lossy(&output.stderr));
        }

        // The 'receive-migration' command should be executed successfully within the given timeout
        let receive_success = if let Some(status) = receive_migration
            .wait_timeout(std::time::Duration::from_secs(30))
            .unwrap()
        {
            status.success()
        } else {
            false
        };

        if !receive_success {
            let _ = receive_migration.kill();
            let output = receive_migration.wait_with_output().unwrap();
            eprintln!("\n\n==== Start 'receive_migration' output ====\n\n---stdout---\n{}\n\n---stderr---\n{}\n\n==== End 'receive_migration' output ====\n\n",
                    String::from_utf8_lossy(&output.stdout), String::from_utf8_lossy(&output.stderr));
        }

        send_success && receive_success
    }

    fn print_and_panic(src_vm: Child, dest_vm: Child, ovs_vm: Option<Child>, message: &str) -> ! {
        let mut src_vm = src_vm;
        let mut dest_vm = dest_vm;

        let _ = src_vm.kill();
        let src_output = src_vm.wait_with_output().unwrap();
        eprintln!(
            "\n\n==== Start 'source_vm' stdout ====\n\n{}\n\n==== End 'source_vm' stdout ====",
            String::from_utf8_lossy(&src_output.stdout)
        );
        eprintln!(
            "\n\n==== Start 'source_vm' stderr ====\n\n{}\n\n==== End 'source_vm' stderr ====",
            String::from_utf8_lossy(&src_output.stderr)
        );
        let _ = dest_vm.kill();
        let dest_output = dest_vm.wait_with_output().unwrap();
        eprintln!(
                "\n\n==== Start 'destination_vm' stdout ====\n\n{}\n\n==== End 'destination_vm' stdout ====",
                String::from_utf8_lossy(&dest_output.stdout)
            );
        eprintln!(
                "\n\n==== Start 'destination_vm' stderr ====\n\n{}\n\n==== End 'destination_vm' stderr ====",
                String::from_utf8_lossy(&dest_output.stderr)
            );

        if let Some(ovs_vm) = ovs_vm {
            let mut ovs_vm = ovs_vm;
            let _ = ovs_vm.kill();
            let ovs_output = ovs_vm.wait_with_output().unwrap();
            eprintln!(
                "\n\n==== Start 'ovs_vm' stdout ====\n\n{}\n\n==== End 'ovs_vm' stdout ====",
                String::from_utf8_lossy(&ovs_output.stdout)
            );
            eprintln!(
                "\n\n==== Start 'ovs_vm' stderr ====\n\n{}\n\n==== End 'ovs_vm' stderr ====",
                String::from_utf8_lossy(&ovs_output.stderr)
            );

            cleanup_ovs_dpdk();
        }

        panic!("Test failed: {}", message)
    }

    // This test exercises the local live-migration between two Cloud Hypervisor VMs on the
    // same host. It ensures the following behaviors:
    // 1. The source VM is up and functional (including various virtio-devices are working properly);
    // 2. The 'send-migration' and 'receive-migration' command finished successfully;
    // 3. The source VM terminated gracefully after live migration;
    // 4. The destination VM is functional (including various virtio-devices are working properly) after
    //    live migration;
    // Note: This test does not use vsock as we can't create two identical vsock on the same host.
    fn _test_live_migration(upgrade_test: bool, local: bool) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();
        let console_text = String::from("On a branch floating down river a cricket, singing.");
        let net_id = "net123";
        let net_params = format!(
            "id={},tap=,mac={},ip={},mask=255.255.255.0",
            net_id, guest.network.guest_mac, guest.network.host_ip
        );

        let memory_param: &[&str] = if local {
            &["--memory", "size=4G,shared=on"]
        } else {
            &["--memory", "size=4G"]
        };

        let boot_vcpus = 2;
        let max_vcpus = 4;

        let pmem_temp_file = TempFile::new().unwrap();
        pmem_temp_file.as_file().set_len(128 << 20).unwrap();
        std::process::Command::new("mkfs.ext4")
            .arg(pmem_temp_file.as_path())
            .output()
            .expect("Expect creating disk image to succeed");
        let pmem_path = String::from("/dev/pmem0");

        // Start the source VM
        let src_vm_path = if !upgrade_test {
            clh_command("cloud-hypervisor")
        } else {
            cloud_hypervisor_release_path()
        };
        let src_api_socket = temp_api_path(&guest.tmp_dir);
        let mut src_vm_cmd = GuestCommand::new_with_binary_path(&guest, &src_vm_path);
        src_vm_cmd
            .args([
                "--cpus",
                format!("boot={},max={}", boot_vcpus, max_vcpus).as_str(),
            ])
            .args(memory_param)
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--net", net_params.as_str()])
            .args(["--api-socket", &src_api_socket])
            .args([
                "--pmem",
                format!("file={}", pmem_temp_file.as_path().to_str().unwrap(),).as_str(),
            ]);
        let mut src_child = src_vm_cmd.capture_output().spawn().unwrap();

        // Start the destination VM
        let mut dest_api_socket = temp_api_path(&guest.tmp_dir);
        dest_api_socket.push_str(".dest");
        let mut dest_child = GuestCommand::new(&guest)
            .args(["--api-socket", &dest_api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Make sure the source VM is functaionl
            // Check the number of vCPUs
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);

            // Check the guest RAM
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);

            // Check the guest virtio-devices, e.g. block, rng, console, and net
            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));

            // x86_64: Following what's done in the `test_snapshot_restore`, we need
            // to make sure that removing and adding back the virtio-net device does
            // not break the live-migration support for virtio-pci.
            #[cfg(target_arch = "x86_64")]
            {
                assert!(remote_command(
                    &src_api_socket,
                    "remove-device",
                    Some(net_id),
                ));
                thread::sleep(std::time::Duration::new(10, 0));

                // Plug the virtio-net device again
                assert!(remote_command(
                    &src_api_socket,
                    "add-net",
                    Some(net_params.as_str()),
                ));
                thread::sleep(std::time::Duration::new(10, 0));
            }

            // Start the live-migration
            let migration_socket = String::from(
                guest
                    .tmp_dir
                    .as_path()
                    .join("live-migration.sock")
                    .to_str()
                    .unwrap(),
            );

            assert!(
                start_live_migration(&migration_socket, &src_api_socket, &dest_api_socket, local),
                "Unsuccessful command: 'send-migration' or 'receive-migration'."
            );
        });

        // Check and report any errors occured during the live-migration
        if r.is_err() {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "Error occured during live-migration",
            );
        }

        // Check the source vm has been terminated successful (give it '3s' to settle)
        thread::sleep(std::time::Duration::new(3, 0));
        if !src_child.try_wait().unwrap().map_or(false, |s| s.success()) {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "source VM was not terminated successfully.",
            );
        };

        // Post live-migration check to make sure the destination VM is funcational
        let r = std::panic::catch_unwind(|| {
            // Perform same checks to validate VM has been properly migrated
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);

            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));
        });

        // Clean-up the destination VM and make sure it terminated correctly
        let _ = dest_child.kill();
        let dest_output = dest_child.wait_with_output().unwrap();
        handle_child_output(r, &dest_output);

        // Check the destination VM has the expected 'concole_text' from its output
        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&dest_output.stdout).contains(&console_text));
        });
        handle_child_output(r, &dest_output);
    }

    fn _test_live_migration_balloon(upgrade_test: bool, local: bool) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();
        let console_text = String::from("On a branch floating down river a cricket, singing.");
        let net_id = "net123";
        let net_params = format!(
            "id={},tap=,mac={},ip={},mask=255.255.255.0",
            net_id, guest.network.guest_mac, guest.network.host_ip
        );

        let memory_param: &[&str] = if local {
            &[
                "--memory",
                "size=4G,hotplug_method=virtio-mem,hotplug_size=8G,shared=on",
                "--balloon",
                "size=0",
            ]
        } else {
            &[
                "--memory",
                "size=4G,hotplug_method=virtio-mem,hotplug_size=8G",
                "--balloon",
                "size=0",
            ]
        };

        let boot_vcpus = 2;
        let max_vcpus = 4;

        let pmem_temp_file = TempFile::new().unwrap();
        pmem_temp_file.as_file().set_len(128 << 20).unwrap();
        std::process::Command::new("mkfs.ext4")
            .arg(pmem_temp_file.as_path())
            .output()
            .expect("Expect creating disk image to succeed");
        let pmem_path = String::from("/dev/pmem0");

        // Start the source VM
        let src_vm_path = if !upgrade_test {
            clh_command("cloud-hypervisor")
        } else {
            cloud_hypervisor_release_path()
        };
        let src_api_socket = temp_api_path(&guest.tmp_dir);
        let mut src_vm_cmd = GuestCommand::new_with_binary_path(&guest, &src_vm_path);
        src_vm_cmd
            .args([
                "--cpus",
                format!("boot={},max={}", boot_vcpus, max_vcpus).as_str(),
            ])
            .args(memory_param)
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--net", net_params.as_str()])
            .args(["--api-socket", &src_api_socket])
            .args([
                "--pmem",
                format!("file={}", pmem_temp_file.as_path().to_str().unwrap(),).as_str(),
            ]);
        let mut src_child = src_vm_cmd.capture_output().spawn().unwrap();

        // Start the destination VM
        let mut dest_api_socket = temp_api_path(&guest.tmp_dir);
        dest_api_socket.push_str(".dest");
        let mut dest_child = GuestCommand::new(&guest)
            .args(["--api-socket", &dest_api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Make sure the source VM is functaionl
            // Check the number of vCPUs
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);

            // Check the guest RAM
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);
            // Increase the guest RAM
            resize_command(&src_api_socket, None, Some(6 << 30), None, None);
            thread::sleep(std::time::Duration::new(5, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 5_760_000);
            // Use balloon to remove RAM from the VM
            resize_command(&src_api_socket, None, None, Some(1 << 30), None);
            thread::sleep(std::time::Duration::new(5, 0));
            let total_memory = guest.get_total_memory().unwrap_or_default();
            assert!(total_memory > 4_800_000);
            assert!(total_memory < 5_760_000);

            // Check the guest virtio-devices, e.g. block, rng, console, and net
            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));

            // x86_64: Following what's done in the `test_snapshot_restore`, we need
            // to make sure that removing and adding back the virtio-net device does
            // not break the live-migration support for virtio-pci.
            #[cfg(target_arch = "x86_64")]
            {
                assert!(remote_command(
                    &src_api_socket,
                    "remove-device",
                    Some(net_id),
                ));
                thread::sleep(std::time::Duration::new(10, 0));

                // Plug the virtio-net device again
                assert!(remote_command(
                    &src_api_socket,
                    "add-net",
                    Some(net_params.as_str()),
                ));
                thread::sleep(std::time::Duration::new(10, 0));
            }

            // Start the live-migration
            let migration_socket = String::from(
                guest
                    .tmp_dir
                    .as_path()
                    .join("live-migration.sock")
                    .to_str()
                    .unwrap(),
            );

            assert!(
                start_live_migration(&migration_socket, &src_api_socket, &dest_api_socket, local),
                "Unsuccessful command: 'send-migration' or 'receive-migration'."
            );
        });

        // Check and report any errors occured during the live-migration
        if r.is_err() {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "Error occured during live-migration",
            );
        }

        // Check the source vm has been terminated successful (give it '3s' to settle)
        thread::sleep(std::time::Duration::new(3, 0));
        if !src_child.try_wait().unwrap().map_or(false, |s| s.success()) {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "source VM was not terminated successfully.",
            );
        };

        // Post live-migration check to make sure the destination VM is funcational
        let r = std::panic::catch_unwind(|| {
            // Perform same checks to validate VM has been properly migrated
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);

            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));

            // Perform checks on guest RAM using balloon
            let total_memory = guest.get_total_memory().unwrap_or_default();
            assert!(total_memory > 4_800_000);
            assert!(total_memory < 5_760_000);
            // Deflate balloon to restore entire RAM to the VM
            resize_command(&dest_api_socket, None, None, Some(0), None);
            thread::sleep(std::time::Duration::new(5, 0));
            assert!(guest.get_total_memory().unwrap_or_default() > 5_760_000);
            // Decrease guest RAM with virtio-mem
            resize_command(&dest_api_socket, None, Some(5 << 30), None, None);
            thread::sleep(std::time::Duration::new(5, 0));
            let total_memory = guest.get_total_memory().unwrap_or_default();
            assert!(total_memory > 4_800_000);
            assert!(total_memory < 5_760_000);
        });

        // Clean-up the destination VM and make sure it terminated correctly
        let _ = dest_child.kill();
        let dest_output = dest_child.wait_with_output().unwrap();
        handle_child_output(r, &dest_output);

        // Check the destination VM has the expected 'concole_text' from its output
        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&dest_output.stdout).contains(&console_text));
        });
        handle_child_output(r, &dest_output);
    }

    fn _test_live_migration_numa(upgrade_test: bool, local: bool) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();
        let console_text = String::from("On a branch floating down river a cricket, singing.");
        let net_id = "net123";
        let net_params = format!(
            "id={},tap=,mac={},ip={},mask=255.255.255.0",
            net_id, guest.network.guest_mac, guest.network.host_ip
        );

        let memory_param: &[&str] = if local {
            &[
                "--memory",
                "size=0,hotplug_method=virtio-mem,shared=on",
                "--memory-zone",
                "id=mem0,size=1G,hotplug_size=4G,shared=on",
                "id=mem1,size=1G,hotplug_size=4G,shared=on",
                "id=mem2,size=2G,hotplug_size=4G,shared=on",
                "--numa",
                "guest_numa_id=0,cpus=[0-2,9],distances=[1@15,2@20],memory_zones=mem0",
                "guest_numa_id=1,cpus=[3-4,6-8],distances=[0@20,2@25],memory_zones=mem1",
                "guest_numa_id=2,cpus=[5,10-11],distances=[0@25,1@30],memory_zones=mem2",
            ]
        } else {
            &[
                "--memory",
                "size=0,hotplug_method=virtio-mem",
                "--memory-zone",
                "id=mem0,size=1G,hotplug_size=4G",
                "id=mem1,size=1G,hotplug_size=4G",
                "id=mem2,size=2G,hotplug_size=4G",
                "--numa",
                "guest_numa_id=0,cpus=[0-2,9],distances=[1@15,2@20],memory_zones=mem0",
                "guest_numa_id=1,cpus=[3-4,6-8],distances=[0@20,2@25],memory_zones=mem1",
                "guest_numa_id=2,cpus=[5,10-11],distances=[0@25,1@30],memory_zones=mem2",
            ]
        };

        let boot_vcpus = 6;
        let max_vcpus = 12;

        let pmem_temp_file = TempFile::new().unwrap();
        pmem_temp_file.as_file().set_len(128 << 20).unwrap();
        std::process::Command::new("mkfs.ext4")
            .arg(pmem_temp_file.as_path())
            .output()
            .expect("Expect creating disk image to succeed");
        let pmem_path = String::from("/dev/pmem0");

        // Start the source VM
        let src_vm_path = if !upgrade_test {
            clh_command("cloud-hypervisor")
        } else {
            cloud_hypervisor_release_path()
        };
        let src_api_socket = temp_api_path(&guest.tmp_dir);
        let mut src_vm_cmd = GuestCommand::new_with_binary_path(&guest, &src_vm_path);
        src_vm_cmd
            .args([
                "--cpus",
                format!("boot={},max={}", boot_vcpus, max_vcpus).as_str(),
            ])
            .args(memory_param)
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--net", net_params.as_str()])
            .args(["--api-socket", &src_api_socket])
            .args([
                "--pmem",
                format!("file={}", pmem_temp_file.as_path().to_str().unwrap(),).as_str(),
            ]);
        let mut src_child = src_vm_cmd.capture_output().spawn().unwrap();

        // Start the destination VM
        let mut dest_api_socket = temp_api_path(&guest.tmp_dir);
        dest_api_socket.push_str(".dest");
        let mut dest_child = GuestCommand::new(&guest)
            .args(["--api-socket", &dest_api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Make sure the source VM is functaionl
            // Check the number of vCPUs
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);

            // Check the guest RAM
            assert!(guest.get_total_memory().unwrap_or_default() > 2_880_000);

            // Check the guest virtio-devices, e.g. block, rng, console, and net
            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));

            // Check the NUMA parameters are applied correctly and resize
            // each zone to test the case where we migrate a VM with the
            // virtio-mem regions being used.
            {
                guest.check_numa_common(
                    Some(&[960_000, 960_000, 1_920_000]),
                    Some(&[vec![0, 1, 2], vec![3, 4], vec![5]]),
                    Some(&["10 15 20", "20 10 25", "25 30 10"]),
                );

                // AArch64 currently does not support hotplug, and therefore we only
                // test hotplug-related function on x86_64 here.
                #[cfg(target_arch = "x86_64")]
                {
                    guest.enable_memory_hotplug();

                    // Resize every memory zone and check each associated NUMA node
                    // has been assigned the right amount of memory.
                    resize_zone_command(&src_api_socket, "mem0", "2G");
                    resize_zone_command(&src_api_socket, "mem1", "2G");
                    resize_zone_command(&src_api_socket, "mem2", "3G");
                    thread::sleep(std::time::Duration::new(5, 0));

                    guest.check_numa_common(Some(&[1_920_000, 1_920_000, 1_920_000]), None, None);
                }
            }

            // x86_64: Following what's done in the `test_snapshot_restore`, we need
            // to make sure that removing and adding back the virtio-net device does
            // not break the live-migration support for virtio-pci.
            #[cfg(target_arch = "x86_64")]
            {
                assert!(remote_command(
                    &src_api_socket,
                    "remove-device",
                    Some(net_id),
                ));
                thread::sleep(std::time::Duration::new(10, 0));

                // Plug the virtio-net device again
                assert!(remote_command(
                    &src_api_socket,
                    "add-net",
                    Some(net_params.as_str()),
                ));
                thread::sleep(std::time::Duration::new(10, 0));
            }

            // Start the live-migration
            let migration_socket = String::from(
                guest
                    .tmp_dir
                    .as_path()
                    .join("live-migration.sock")
                    .to_str()
                    .unwrap(),
            );

            assert!(
                start_live_migration(&migration_socket, &src_api_socket, &dest_api_socket, local),
                "Unsuccessful command: 'send-migration' or 'receive-migration'."
            );
        });

        // Check and report any errors occured during the live-migration
        if r.is_err() {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "Error occured during live-migration",
            );
        }

        // Check the source vm has been terminated successful (give it '3s' to settle)
        thread::sleep(std::time::Duration::new(3, 0));
        if !src_child.try_wait().unwrap().map_or(false, |s| s.success()) {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "source VM was not terminated successfully.",
            );
        };

        // Post live-migration check to make sure the destination VM is funcational
        let r = std::panic::catch_unwind(|| {
            // Perform same checks to validate VM has been properly migrated
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);
            #[cfg(target_arch = "x86_64")]
            assert!(guest.get_total_memory().unwrap_or_default() > 6_720_000);
            #[cfg(target_arch = "aarch64")]
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);

            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));

            // Perform NUMA related checks
            {
                #[cfg(target_arch = "aarch64")]
                {
                    guest.check_numa_common(
                        Some(&[960_000, 960_000, 1_920_000]),
                        Some(&[vec![0, 1, 2], vec![3, 4], vec![5]]),
                        Some(&["10 15 20", "20 10 25", "25 30 10"]),
                    );
                }

                // AArch64 currently does not support hotplug, and therefore we only
                // test hotplug-related function on x86_64 here.
                #[cfg(target_arch = "x86_64")]
                {
                    guest.check_numa_common(
                        Some(&[1_920_000, 1_920_000, 2_880_000]),
                        Some(&[vec![0, 1, 2], vec![3, 4], vec![5]]),
                        Some(&["10 15 20", "20 10 25", "25 30 10"]),
                    );

                    guest.enable_memory_hotplug();

                    // Resize every memory zone and check each associated NUMA node
                    // has been assigned the right amount of memory.
                    resize_zone_command(&dest_api_socket, "mem0", "4G");
                    resize_zone_command(&dest_api_socket, "mem1", "4G");
                    resize_zone_command(&dest_api_socket, "mem2", "4G");
                    // Resize to the maximum amount of CPUs and check each NUMA
                    // node has been assigned the right CPUs set.
                    resize_command(&dest_api_socket, Some(max_vcpus), None, None, None);
                    thread::sleep(std::time::Duration::new(5, 0));

                    guest.check_numa_common(
                        Some(&[3_840_000, 3_840_000, 3_840_000]),
                        Some(&[vec![0, 1, 2, 9], vec![3, 4, 6, 7, 8], vec![5, 10, 11]]),
                        None,
                    );
                }
            }
        });

        // Clean-up the destination VM and make sure it terminated correctly
        let _ = dest_child.kill();
        let dest_output = dest_child.wait_with_output().unwrap();
        handle_child_output(r, &dest_output);

        // Check the destination VM has the expected 'concole_text' from its output
        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&dest_output.stdout).contains(&console_text));
        });
        handle_child_output(r, &dest_output);
    }

    fn _test_live_migration_watchdog(upgrade_test: bool, local: bool) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let kernel_path = direct_kernel_boot_path();
        let console_text = String::from("On a branch floating down river a cricket, singing.");
        let net_id = "net123";
        let net_params = format!(
            "id={},tap=,mac={},ip={},mask=255.255.255.0",
            net_id, guest.network.guest_mac, guest.network.host_ip
        );

        let memory_param: &[&str] = if local {
            &["--memory", "size=4G,shared=on"]
        } else {
            &["--memory", "size=4G"]
        };

        let boot_vcpus = 2;
        let max_vcpus = 4;

        let pmem_temp_file = TempFile::new().unwrap();
        pmem_temp_file.as_file().set_len(128 << 20).unwrap();
        std::process::Command::new("mkfs.ext4")
            .arg(pmem_temp_file.as_path())
            .output()
            .expect("Expect creating disk image to succeed");
        let pmem_path = String::from("/dev/pmem0");

        // Start the source VM
        let src_vm_path = if !upgrade_test {
            clh_command("cloud-hypervisor")
        } else {
            cloud_hypervisor_release_path()
        };
        let src_api_socket = temp_api_path(&guest.tmp_dir);
        let mut src_vm_cmd = GuestCommand::new_with_binary_path(&guest, &src_vm_path);
        src_vm_cmd
            .args([
                "--cpus",
                format!("boot={},max={}", boot_vcpus, max_vcpus).as_str(),
            ])
            .args(memory_param)
            .args(["--kernel", kernel_path.to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--net", net_params.as_str()])
            .args(["--api-socket", &src_api_socket])
            .args([
                "--pmem",
                format!("file={}", pmem_temp_file.as_path().to_str().unwrap(),).as_str(),
            ])
            .args(["--watchdog"]);
        let mut src_child = src_vm_cmd.capture_output().spawn().unwrap();

        // Start the destination VM
        let mut dest_api_socket = temp_api_path(&guest.tmp_dir);
        dest_api_socket.push_str(".dest");
        let mut dest_child = GuestCommand::new(&guest)
            .args(["--api-socket", &dest_api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            // Make sure the source VM is functaionl
            // Check the number of vCPUs
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);
            // Check the guest RAM
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);
            // Check the guest virtio-devices, e.g. block, rng, console, and net
            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));
            // x86_64: Following what's done in the `test_snapshot_restore`, we need
            // to make sure that removing and adding back the virtio-net device does
            // not break the live-migration support for virtio-pci.
            #[cfg(target_arch = "x86_64")]
            {
                assert!(remote_command(
                    &src_api_socket,
                    "remove-device",
                    Some(net_id),
                ));
                thread::sleep(std::time::Duration::new(10, 0));

                // Plug the virtio-net device again
                assert!(remote_command(
                    &src_api_socket,
                    "add-net",
                    Some(net_params.as_str()),
                ));
                thread::sleep(std::time::Duration::new(10, 0));
            }

            // Enable watchdog and ensure its functional
            let mut expected_reboot_count = 1;
            // Enable the watchdog with a 15s timeout
            enable_guest_watchdog(&guest, 15);
            // Reboot and check that systemd has activated the watchdog
            guest.ssh_command("sudo reboot").unwrap();
            guest.wait_vm_boot(None).unwrap();
            expected_reboot_count += 1;
            assert_eq!(get_reboot_count(&guest), expected_reboot_count);
            assert_eq!(
                guest
                    .ssh_command("sudo journalctl | grep -c -- \"Watchdog started\"")
                    .unwrap()
                    .trim()
                    .parse::<u32>()
                    .unwrap_or_default(),
                2
            );
            // Allow some normal time to elapse to check we don't get spurious reboots
            thread::sleep(std::time::Duration::new(40, 0));
            // Check no reboot
            assert_eq!(get_reboot_count(&guest), expected_reboot_count);

            // Start the live-migration
            let migration_socket = String::from(
                guest
                    .tmp_dir
                    .as_path()
                    .join("live-migration.sock")
                    .to_str()
                    .unwrap(),
            );

            assert!(
                start_live_migration(&migration_socket, &src_api_socket, &dest_api_socket, local),
                "Unsuccessful command: 'send-migration' or 'receive-migration'."
            );
        });

        // Check and report any errors occured during the live-migration
        if r.is_err() {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "Error occured during live-migration",
            );
        }

        // Check the source vm has been terminated successful (give it '3s' to settle)
        thread::sleep(std::time::Duration::new(3, 0));
        if !src_child.try_wait().unwrap().map_or(false, |s| s.success()) {
            print_and_panic(
                src_child,
                dest_child,
                None,
                "source VM was not terminated successfully.",
            );
        };

        // Post live-migration check to make sure the destination VM is funcational
        let r = std::panic::catch_unwind(|| {
            // Perform same checks to validate VM has been properly migrated
            assert_eq!(guest.get_cpu_count().unwrap_or_default(), boot_vcpus);
            assert!(guest.get_total_memory().unwrap_or_default() > 3_840_000);

            guest.check_devices_common(None, Some(&console_text), Some(&pmem_path));

            // Perform checks on watchdog
            let mut expected_reboot_count = 2;

            // Allow some normal time to elapse to check we don't get spurious reboots
            thread::sleep(std::time::Duration::new(40, 0));
            // Check no reboot
            assert_eq!(get_reboot_count(&guest), expected_reboot_count);

            // Trigger a panic (sync first). We need to do this inside a screen with a delay so the SSH command returns.
            guest.ssh_command("screen -dmS reboot sh -c \"sleep 5; echo s | tee /proc/sysrq-trigger; echo c | sudo tee /proc/sysrq-trigger\"").unwrap();
            // Allow some time for the watchdog to trigger (max 30s) and reboot to happen
            guest.wait_vm_boot(Some(50)).unwrap();
            // Check a reboot is triggerred by the watchdog
            expected_reboot_count += 1;
            assert_eq!(get_reboot_count(&guest), expected_reboot_count);

            #[cfg(target_arch = "x86_64")]
            {
                // Now pause the VM and remain offline for 30s
                assert!(remote_command(&dest_api_socket, "pause", None));
                thread::sleep(std::time::Duration::new(30, 0));
                assert!(remote_command(&dest_api_socket, "resume", None));

                // Check no reboot
                assert_eq!(get_reboot_count(&guest), expected_reboot_count);
            }
        });

        // Clean-up the destination VM and make sure it terminated correctly
        let _ = dest_child.kill();
        let dest_output = dest_child.wait_with_output().unwrap();
        handle_child_output(r, &dest_output);

        // Check the destination VM has the expected 'concole_text' from its output
        let r = std::panic::catch_unwind(|| {
            assert!(String::from_utf8_lossy(&dest_output.stdout).contains(&console_text));
        });
        handle_child_output(r, &dest_output);
    }

    fn _test_live_migration_ovs_dpdk(upgrade_test: bool, local: bool) {
        let ovs_focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let ovs_guest = Guest::new(Box::new(ovs_focal));

        let migration_focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let migration_guest = Guest::new(Box::new(migration_focal));
        let src_api_socket = temp_api_path(&migration_guest.tmp_dir);

        // Start two VMs that are connected through ovs-dpdk and one of the VMs is the source VM for live-migration
        let (mut ovs_child, mut src_child) =
            setup_ovs_dpdk_guests(&ovs_guest, &migration_guest, &src_api_socket, upgrade_test);

        // Start the destination VM
        let mut dest_api_socket = temp_api_path(&migration_guest.tmp_dir);
        dest_api_socket.push_str(".dest");
        let mut dest_child = GuestCommand::new(&migration_guest)
            .args(["--api-socket", &dest_api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            // Give it '1s' to make sure the 'dest_api_socket' file is properly created
            thread::sleep(std::time::Duration::new(1, 0));

            // Start the live-migration
            let migration_socket = String::from(
                migration_guest
                    .tmp_dir
                    .as_path()
                    .join("live-migration.sock")
                    .to_str()
                    .unwrap(),
            );

            assert!(
                start_live_migration(&migration_socket, &src_api_socket, &dest_api_socket, local),
                "Unsuccessful command: 'send-migration' or 'receive-migration'."
            );
        });

        // Check and report any errors occured during the live-migration
        if r.is_err() {
            print_and_panic(
                src_child,
                dest_child,
                Some(ovs_child),
                "Error occured during live-migration",
            );
        }

        // Check the source vm has been terminated successful (give it '3s' to settle)
        thread::sleep(std::time::Duration::new(3, 0));
        if !src_child.try_wait().unwrap().map_or(false, |s| s.success()) {
            print_and_panic(
                src_child,
                dest_child,
                Some(ovs_child),
                "source VM was not terminated successfully.",
            );
        };

        // Post live-migration check to make sure the destination VM is funcational
        let r = std::panic::catch_unwind(|| {
            // Perform same checks to validate VM has been properly migrated
            // Spawn a new netcat listener in the OVS VM
            let guest_ip = ovs_guest.network.guest_ip.clone();
            thread::spawn(move || {
                ssh_command_ip(
                    "nc -l 12345",
                    &guest_ip,
                    DEFAULT_SSH_RETRIES,
                    DEFAULT_SSH_TIMEOUT,
                )
                .unwrap();
            });

            // Wait for the server to be listening
            thread::sleep(std::time::Duration::new(5, 0));

            // And check the connection is still functional after live-migration
            migration_guest
                .ssh_command("nc -vz 172.100.0.1 12345")
                .unwrap();
        });

        // Clean-up the destination VM and OVS VM, and make sure they terminated correctly
        let _ = dest_child.kill();
        let _ = ovs_child.kill();
        let dest_output = dest_child.wait_with_output().unwrap();
        let ovs_output = ovs_child.wait_with_output().unwrap();

        cleanup_ovs_dpdk();

        handle_child_output(r, &dest_output);
        handle_child_output(Ok(()), &ovs_output);
    }

    mod live_migration_parallel {
        use super::*;
        #[test]
        fn test_live_migration_basic() {
            _test_live_migration(false, false)
        }

        #[test]
        fn test_live_migration_local() {
            _test_live_migration(false, true)
        }

        #[test]
        #[cfg(not(feature = "mshv"))]
        fn test_live_migration_numa() {
            _test_live_migration_numa(false, false)
        }

        #[test]
        #[cfg(not(feature = "mshv"))]
        fn test_live_migration_numa_local() {
            _test_live_migration_numa(false, true)
        }

        #[test]
        fn test_live_migration_watchdog() {
            _test_live_migration_watchdog(false, false)
        }

        #[test]
        fn test_live_migration_watchdog_local() {
            _test_live_migration_watchdog(false, true)
        }

        #[test]
        fn test_live_migration_balloon() {
            _test_live_migration_balloon(false, false)
        }

        #[test]
        fn test_live_migration_balloon_local() {
            _test_live_migration_balloon(false, true)
        }

        #[test]
        fn test_live_upgrade_basic() {
            _test_live_migration(true, false)
        }

        #[test]
        fn test_live_upgrade_local() {
            _test_live_migration(true, true)
        }

        #[test]
        #[cfg(not(feature = "mshv"))]
        fn test_live_upgrade_numa() {
            _test_live_migration_numa(true, false)
        }

        #[test]
        #[cfg(not(feature = "mshv"))]
        fn test_live_upgrade_numa_local() {
            _test_live_migration_numa(true, true)
        }

        #[test]
        fn test_live_upgrade_watchdog() {
            _test_live_migration_watchdog(true, false)
        }

        #[test]
        fn test_live_upgrade_watchdog_local() {
            _test_live_migration_watchdog(true, true)
        }

        #[test]
        fn test_live_upgrade_balloon() {
            _test_live_migration_balloon(true, false)
        }

        #[test]
        fn test_live_upgrade_balloon_local() {
            _test_live_migration_balloon(true, true)
        }
    }

    mod live_migration_sequential {
        #[cfg(target_arch = "x86_64")]
        #[cfg(not(feature = "mshv"))]
        use super::*;

        // Require to run ovs-dpdk tests sequentially because they rely on the same ovs-dpdk setup
        #[test]
        #[cfg(target_arch = "x86_64")]
        #[cfg(not(feature = "mshv"))]
        fn test_live_migration_ovs_dpdk() {
            _test_live_migration_ovs_dpdk(false, false);
        }

        #[test]
        #[cfg(target_arch = "x86_64")]
        #[cfg(not(feature = "mshv"))]
        fn test_live_migration_ovs_dpdk_local() {
            _test_live_migration_ovs_dpdk(false, true);
        }

        #[test]
        #[cfg(target_arch = "x86_64")]
        #[cfg(not(feature = "mshv"))]
        fn test_live_upgrade_ovs_dpdk() {
            _test_live_migration_ovs_dpdk(true, false);
        }

        #[test]
        #[cfg(target_arch = "x86_64")]
        #[cfg(not(feature = "mshv"))]
        fn test_live_upgrade_ovs_dpdk_local() {
            _test_live_migration_ovs_dpdk(true, true);
        }
    }
}

#[cfg(target_arch = "aarch64")]
mod aarch64_acpi {
    use crate::*;

    #[test]
    fn test_simple_launch_acpi() {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());

        vec![Box::new(focal)].drain(..).for_each(|disk_config| {
            let guest = Guest::new(disk_config);

            let mut child = GuestCommand::new(&guest)
                .args(["--cpus", "boot=1"])
                .args(["--memory", "size=512M"])
                .args(["--kernel", edk2_path().to_str().unwrap()])
                .default_disks()
                .default_net()
                .args(["--serial", "tty", "--console", "off"])
                .capture_output()
                .spawn()
                .unwrap();

            let r = std::panic::catch_unwind(|| {
                guest.wait_vm_boot(Some(120)).unwrap();

                assert_eq!(guest.get_cpu_count().unwrap_or_default(), 1);
                assert!(guest.get_total_memory().unwrap_or_default() > 400_000);
                assert_eq!(guest.get_pci_bridge_class().unwrap_or_default(), "0x060000");
            });

            let _ = child.kill();
            let output = child.wait_with_output().unwrap();

            handle_child_output(r, &output);
        });
    }

    #[test]
    fn test_guest_numa_nodes_acpi() {
        _test_guest_numa_nodes(true);
    }

    #[test]
    fn test_cpu_topology_421_acpi() {
        test_cpu_topology(4, 2, 1, true);
    }

    #[test]
    fn test_cpu_topology_142_acpi() {
        test_cpu_topology(1, 4, 2, true);
    }

    #[test]
    fn test_cpu_topology_262_acpi() {
        test_cpu_topology(2, 6, 2, true);
    }

    #[test]
    fn test_power_button_acpi() {
        _test_power_button(true);
    }

    #[test]
    fn test_virtio_iommu() {
        _test_virtio_iommu(true)
    }
}

mod rate_limiter {
    use super::*;

    // Check if the 'measured' rate is within the expected 'difference' (in percentage)
    // compared to given 'limit' rate.
    fn check_rate_limit(measured: f64, limit: f64, difference: f64) -> bool {
        let upper_limit = limit * (1_f64 + difference);
        let lower_limit = limit * (1_f64 - difference);

        if measured > lower_limit && measured < upper_limit {
            return true;
        }

        eprintln!(
            "\n\n==== check_rate_limit failed! ====\n\nmeasured={}, , lower_limit={}, upper_limit={}\n\n",
            measured, lower_limit, upper_limit
        );

        false
    }

    fn _test_rate_limiter_net(rx: bool) {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));

        let test_timeout = 10;
        let num_queues = 2;
        let queue_size = 256;
        let bw_size = 10485760_u64; // bytes
        let bw_refill_time = 100; // ms
        let limit_bps = (bw_size * 8 * 1000) as f64 / bw_refill_time as f64;

        let net_params = format!(
            "tap=,mac={},ip={},mask=255.255.255.0,num_queues={},queue_size={},bw_size={},bw_refill_time={}",
            guest.network.guest_mac,
            guest.network.host_ip,
            num_queues,
            queue_size,
            bw_size,
            bw_refill_time,
        );

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", &format!("boot={}", num_queues / 2)])
            .args(["--memory", "size=4G"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .default_disks()
            .args(["--net", net_params.as_str()])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();
            let measured_bps =
                measure_virtio_net_throughput(test_timeout, num_queues / 2, &guest, rx).unwrap();
            assert!(check_rate_limit(measured_bps, limit_bps, 0.1));
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);
    }

    #[test]
    fn test_rate_limiter_net_rx() {
        _test_rate_limiter_net(true);
    }

    #[test]
    fn test_rate_limiter_net_tx() {
        _test_rate_limiter_net(false);
    }

    fn _test_rate_limiter_block(bandwidth: bool) {
        let test_timeout = 10;
        let num_queues = 1;
        let fio_ops = FioOps::RandRW;

        let bw_size = if bandwidth {
            10485760_u64 // bytes
        } else {
            100_u64 // I/O
        };
        let bw_refill_time = 100; // ms
        let limit_rate = (bw_size * 1000) as f64 / bw_refill_time as f64;

        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = Guest::new(Box::new(focal));
        let api_socket = temp_api_path(&guest.tmp_dir);
        let test_img_dir = TempDir::new_with_prefix("/var/tmp/ch").unwrap();
        let blk_rate_limiter_test_img =
            String::from(test_img_dir.as_path().join("blk.img").to_str().unwrap());

        // Create the test block image
        assert!(exec_host_command_output(&format!(
            "dd if=/dev/zero of={} bs=1M count=1024",
            blk_rate_limiter_test_img
        ))
        .status
        .success());

        let test_blk_params = if bandwidth {
            format!(
                "path={},bw_size={},bw_refill_time={}",
                blk_rate_limiter_test_img, bw_size, bw_refill_time
            )
        } else {
            format!(
                "path={},ops_size={},ops_refill_time={}",
                blk_rate_limiter_test_img, bw_size, bw_refill_time
            )
        };

        let mut child = GuestCommand::new(&guest)
            .args(["--cpus", &format!("boot={}", num_queues)])
            .args(["--memory", "size=4G"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args([
                "--disk",
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
                format!(
                    "path={}",
                    guest.disk_config.disk(DiskType::CloudInit).unwrap()
                )
                .as_str(),
                test_blk_params.as_str(),
            ])
            .default_net()
            .args(["--api-socket", &api_socket])
            .capture_output()
            .spawn()
            .unwrap();

        let r = std::panic::catch_unwind(|| {
            guest.wait_vm_boot(None).unwrap();

            let fio_command = format!(
                "sudo fio --filename=/dev/vdc --name=test --output-format=json \
                --direct=1 --bs=4k --ioengine=io_uring --iodepth=64 \
                --rw={} --runtime={} --numjobs={}",
                fio_ops, test_timeout, num_queues
            );
            let output = guest.ssh_command(&fio_command).unwrap();

            // Parse fio output
            let measured_rate = if bandwidth {
                parse_fio_output(&output, &fio_ops, num_queues).unwrap()
            } else {
                parse_fio_output_iops(&output, &fio_ops, num_queues).unwrap()
            };
            assert!(check_rate_limit(measured_rate, limit_rate, 0.1));
        });

        kill_child(&mut child);
        let output = child.wait_with_output().unwrap();
        handle_child_output(r, &output);
    }

    #[test]
    fn test_rate_limiter_block_bandwidth() {
        _test_rate_limiter_block(true)
    }

    #[test]
    fn test_rate_limiter_block_iops() {
        _test_rate_limiter_block(false)
    }
}
