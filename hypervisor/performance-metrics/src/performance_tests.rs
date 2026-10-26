// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

// Performance tests

use crate::{mean, PerformanceTestControl};
use std::fs;
use std::path::PathBuf;
use std::string::String;
use std::thread;
use std::time::Duration;
use test_infra::Error as InfraError;
use test_infra::*;

#[cfg(target_arch = "x86_64")]
pub const FOCAL_IMAGE_NAME: &str = "focal-server-cloudimg-amd64-custom-20210609-0.raw";
#[cfg(target_arch = "aarch64")]
pub const FOCAL_IMAGE_NAME: &str = "focal-server-cloudimg-arm64-custom-20210929-0-update-tool.raw";

#[allow(dead_code)]
#[derive(Debug)]
enum Error {
    BootTimeParse,
    Infra(InfraError),
}

impl From<InfraError> for Error {
    fn from(e: InfraError) -> Self {
        Self::Infra(e)
    }
}

const BLK_IO_TEST_IMG: &str = "/var/tmp/ch-blk-io-test.img";

pub fn init_tests() {
    // The test image can not be created on tmpfs (e.g. /tmp) filesystem,
    // as tmpfs does not support O_DIRECT
    assert!(exec_host_command_output(&format!(
        "dd if=/dev/zero of={} bs=1M count=4096",
        BLK_IO_TEST_IMG
    ))
    .status
    .success());
}

pub fn cleanup_tests() {
    fs::remove_file(BLK_IO_TEST_IMG)
        .unwrap_or_else(|_| panic!("Failed to remove file '{}'.", BLK_IO_TEST_IMG));
}

// Performance tests are expected to be executed sequentially, so we can
// start VM guests with the same IP while putting them on a different
// private network. The default constructor "Guest::new()" does not work
// well, as we can easily create more than 256 VMs from repeating various
// performance tests dozens times in a single run.
fn performance_test_new_guest(disk_config: Box<dyn DiskConfig>) -> Guest {
    Guest::new_from_ip_range(disk_config, "172.19", 0)
}

const DIRECT_KERNEL_BOOT_CMDLINE: &str =
    "root=/dev/vda1 console=hvc0 rw systemd.journald.forward_to_console=1";

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

pub fn performance_net_throughput(control: &PerformanceTestControl) -> f64 {
    let test_timeout = control.test_timeout;
    let rx = control.net_rx.unwrap();

    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = performance_test_new_guest(Box::new(focal));

    let num_queues = control.num_queues.unwrap();
    let queue_size = control.queue_size.unwrap();
    let net_params = format!(
        "tap=,mac={},ip={},mask=255.255.255.0,num_queues={},queue_size={}",
        guest.network.guest_mac, guest.network.host_ip, num_queues, queue_size,
    );

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", &format!("boot={}", num_queues)])
        .args(["--memory", "size=4G"])
        .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .args(["--net", net_params.as_str()])
        .capture_output()
        .verbosity(VerbosityLevel::Warn)
        .set_print_cmd(false)
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();
        measure_virtio_net_throughput(test_timeout, num_queues / 2, &guest, rx).unwrap()
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    match r {
        Ok(r) => r,
        Err(e) => {
            handle_child_output(Err(e), &output);
            panic!("test failed!");
        }
    }
}

pub fn performance_vsock_echo_throughput(_control: &PerformanceTestControl) -> f64 {
    const ECHO_BYTES: usize = 256 << 20;

    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = performance_test_new_guest(Box::new(focal));
    let socket = String::from(guest.tmp_dir.as_path().join("vsock").to_str().unwrap());

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", "boot=2"])
        .args(["--memory", "size=1G"])
        .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .default_net()
        .args(["--vsock", format!("cid=3,socket={}", socket).as_str()])
        .capture_output()
        .verbosity(VerbosityLevel::Warn)
        .set_print_cmd(false)
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();
        guest.start_vsock_echo_listener();
        // Warm up the connection path before the measured run.
        guest.vsock_echo(socket.as_str(), 1 << 20);
        let elapsed = guest.vsock_echo(socket.as_str(), ECHO_BYTES);
        ECHO_BYTES as f64 / elapsed.as_secs_f64()
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    match r {
        Ok(r) => r,
        Err(e) => {
            handle_child_output(Err(e), &output);
            panic!("test failed!");
        }
    }
}

pub fn performance_net_latency(control: &PerformanceTestControl) -> f64 {
    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = performance_test_new_guest(Box::new(focal));

    let num_queues = control.num_queues.unwrap();
    let queue_size = control.queue_size.unwrap();
    let net_params = format!(
        "tap=,mac={},ip={},mask=255.255.255.0,num_queues={},queue_size={}",
        guest.network.guest_mac, guest.network.host_ip, num_queues, queue_size,
    );

    let mut child = GuestCommand::new(&guest)
        .args(["--cpus", &format!("boot={}", num_queues)])
        .args(["--memory", "size=4G"])
        .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
        .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
        .default_disks()
        .args(["--net", net_params.as_str()])
        .capture_output()
        .verbosity(VerbosityLevel::Warn)
        .set_print_cmd(false)
        .spawn()
        .unwrap();

    let r = std::panic::catch_unwind(|| {
        guest.wait_vm_boot(None).unwrap();

        // 'ethr' tool will measure the latency multiple times with provided test time
        let latency = measure_virtio_net_latency(&guest, control.test_timeout).unwrap();
        mean(&latency).unwrap()
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    match r {
        Ok(r) => r,
        Err(e) => {
            handle_child_output(Err(e), &output);
            panic!("test failed!");
        }
    }
}

fn parse_boot_time_output(output: &[u8]) -> Result<f64, Error> {
    std::panic::catch_unwind(|| {
        let l: Vec<String> = String::from_utf8_lossy(output)
            .lines()
            .into_iter()
            .filter(|l| l.contains("Debug I/O port: Kernel code"))
            .map(|l| l.to_string())
            .collect();

        assert_eq!(
            l.len(),
            2,
            "Expecting two matching lines for 'Debug I/O port: Kernel code'"
        );

        let time_stamp_kernel_start = {
            let s = l[0].split("--").collect::<Vec<&str>>();
            assert_eq!(
                s.len(),
                2,
                "Expecting '--' for the matching line of 'Debug I/O port' output"
            );

            // Sample output: "[Debug I/O port: Kernel code 0x40] 0.096537 seconds"
            assert!(
                s[1].contains("0x40"),
                "Expecting kernel code '0x40' for 'linux_kernel_start' time stamp output"
            );
            let t = s[1].split_whitespace().collect::<Vec<&str>>();
            assert_eq!(
                t.len(),
                8,
                "Expecting exact '8' words from the 'Debug I/O port' output"
            );
            assert!(
                t[7].eq("seconds"),
                "Expecting 'seconds' as the the last word of the 'Debug I/O port' output"
            );

            t[6].parse::<f64>().unwrap()
        };

        let time_stamp_user_start = {
            let s = l[1].split("--").collect::<Vec<&str>>();
            assert_eq!(
                s.len(),
                2,
                "Expecting '--' for the matching line of 'Debug I/O port' output"
            );

            // Sample output: "Debug I/O port: Kernel code 0x41] 0.198980 seconds"
            assert!(
                s[1].contains("0x41"),
                "Expecting kernel code '0x41' for 'linux_kernel_start' time stamp output"
            );
            let t = s[1].split_whitespace().collect::<Vec<&str>>();
            assert_eq!(
                t.len(),
                8,
                "Expecting exact '8' words from the 'Debug I/O port' output"
            );
            assert!(
                t[7].eq("seconds"),
                "Expecting 'seconds' as the the last word of the 'Debug I/O port' output"
            );

            t[6].parse::<f64>().unwrap()
        };

        time_stamp_user_start - time_stamp_kernel_start
    })
    .map_err(|_| {
        eprintln!(
            "=============== boot-time output ===============\n\n{}\n\n===========end============\n\n",
            String::from_utf8_lossy(output)
        );
        Error::BootTimeParse
    })
}

fn measure_boot_time(cmd: &mut GuestCommand, test_timeout: u32) -> Result<f64, Error> {
    let mut child = cmd
        .capture_output()
        .verbosity(VerbosityLevel::Warn)
        .set_print_cmd(false)
        .spawn()
        .unwrap();

    thread::sleep(Duration::new(test_timeout as u64, 0));
    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    parse_boot_time_output(&output.stderr).map_err(|e| {
        eprintln!(
            "\n\n==== Start child stdout ====\n\n{}\n\n==== End child stdout ====",
            String::from_utf8_lossy(&output.stdout)
        );
        eprintln!(
            "\n\n==== Start child stderr ====\n\n{}\n\n==== End child stderr ====",
            String::from_utf8_lossy(&output.stderr)
        );

        e
    })
}

pub fn performance_boot_time(control: &PerformanceTestControl) -> f64 {
    let r = std::panic::catch_unwind(|| {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = performance_test_new_guest(Box::new(focal));
        let mut cmd = GuestCommand::new(&guest);

        let c = cmd
            .args([
                "--cpus",
                &format!("boot={}", control.num_boot_vcpus.unwrap_or(1)),
            ])
            .args(["--memory", "size=1G"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", DIRECT_KERNEL_BOOT_CMDLINE])
            .args(["--console", "off"])
            .default_disks();

        measure_boot_time(c, control.test_timeout).unwrap()
    });

    match r {
        Ok(r) => r,
        Err(_) => {
            panic!("test failed!");
        }
    }
}

pub fn performance_boot_time_pmem(control: &PerformanceTestControl) -> f64 {
    let r = std::panic::catch_unwind(|| {
        let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
        let guest = performance_test_new_guest(Box::new(focal));
        let mut cmd = GuestCommand::new(&guest);
        let c = cmd
            .args([
                "--cpus",
                &format!("boot={}", control.num_boot_vcpus.unwrap_or(1)),
            ])
            .args(["--memory", "size=1G,hugepages=on"])
            .args(["--kernel", direct_kernel_boot_path().to_str().unwrap()])
            .args(["--cmdline", "root=/dev/pmem0p1 console=ttyS0 quiet rw"])
            .args(["--console", "off"])
            .args([
                "--pmem",
                format!(
                    "file={}",
                    guest.disk_config.disk(DiskType::OperatingSystem).unwrap()
                )
                .as_str(),
            ]);

        measure_boot_time(c, control.test_timeout).unwrap()
    });

    match r {
        Ok(r) => r,
        Err(_) => {
            panic!("test failed!");
        }
    }
}

pub fn performance_block_io(control: &PerformanceTestControl) -> f64 {
    let test_timeout = control.test_timeout;
    let num_queues = control.num_queues.unwrap();
    let fio_ops = control.fio_ops.as_ref().unwrap();

    let focal = UbuntuDiskConfig::new(FOCAL_IMAGE_NAME.to_string());
    let guest = performance_test_new_guest(Box::new(focal));
    let api_socket = guest
        .tmp_dir
        .as_path()
        .join("cloud-hypervisor.sock")
        .to_str()
        .unwrap()
        .to_string();

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
            format!("path={}", BLK_IO_TEST_IMG).as_str(),
        ])
        .default_net()
        .args(["--api-socket", &api_socket])
        .capture_output()
        .verbosity(VerbosityLevel::Warn)
        .set_print_cmd(false)
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
        let output = guest
            .ssh_command(&fio_command)
            .map_err(InfraError::SshCommand)
            .unwrap();

        // Parse fio output
        parse_fio_output(&output, fio_ops, num_queues).unwrap()
    });

    let _ = child.kill();
    let output = child.wait_with_output().unwrap();

    match r {
        Ok(r) => r,
        Err(e) => {
            handle_child_output(Err(e), &output);
            panic!("test failed!");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_iperf3_output() {
        let output = r#"
{
	"end":	{
		"sum_sent":	{
			"start":	0,
			"end":	5.000196,
			"seconds":	5.000196,
			"bytes":	14973836248,
			"bits_per_second":	23957198874.604115,
			"retransmits":	0,
			"sender":	false
		}
	}
}
       "#;
        assert_eq!(
            parse_iperf3_output(output.as_bytes(), true).unwrap(),
            23957198874.604115
        );

        let output = r#"
{
	"end":	{
		"sum_received":	{
			"start":	0,
			"end":	5.000626,
			"seconds":	5.000626,
			"bytes":	24703557800,
			"bits_per_second":	39520744482.79,
			"sender":	true
		}
	}
}
              "#;
        assert_eq!(
            parse_iperf3_output(output.as_bytes(), false).unwrap(),
            39520744482.79
        );
    }

    #[test]
    fn test_parse_ethr_latency_output() {
        let output = r#"{"Time":"2022-02-08T03:52:50Z","Title":"","Type":"INFO","Message":"Using destination: 192.168.249.2, ip: 192.168.249.2, port: 8888"}
{"Time":"2022-02-08T03:52:51Z","Title":"","Type":"INFO","Message":"Running latency test: 1000, 1"}
{"Time":"2022-02-08T03:52:51Z","Title":"","Type":"LatencyResult","RemoteAddr":"192.168.249.2","Protocol":"TCP","Avg":"80.712us","Min":"61.677us","P50":"257.014us","P90":"74.418us","P95":"107.283us","P99":"119.309us","P999":"142.100us","P9999":"216.341us","Max":"216.341us"}
{"Time":"2022-02-08T03:52:52Z","Title":"","Type":"LatencyResult","RemoteAddr":"192.168.249.2","Protocol":"TCP","Avg":"79.826us","Min":"55.129us","P50":"598.996us","P90":"73.849us","P95":"106.552us","P99":"122.152us","P999":"142.459us","P9999":"474.280us","Max":"474.280us"}
{"Time":"2022-02-08T03:52:53Z","Title":"","Type":"LatencyResult","RemoteAddr":"192.168.249.2","Protocol":"TCP","Avg":"78.239us","Min":"56.999us","P50":"396.820us","P90":"69.469us","P95":"115.421us","P99":"119.404us","P999":"130.158us","P9999":"258.686us","Max":"258.686us"}"#;

        let ret = parse_ethr_latency_output(output.as_bytes()).unwrap();
        let reference = vec![80.712_f64, 79.826_f64, 78.239_f64];
        assert_eq!(ret, reference);
    }

    #[test]
    fn test_parse_boot_time_output() {
        let output = r#"
cloud-hypervisor: 161.167103ms: <vcpu0> INFO:vmm/src/vm.rs:392 -- [Debug I/O port: Kernel code 0x40] 0.132 seconds
cloud-hypervisor: 613.57361ms: <vcpu0> INFO:vmm/src/vm.rs:392 -- [Debug I/O port: Kernel code 0x41] 0.5845 seconds
        "#;

        assert_eq!(parse_boot_time_output(output.as_bytes()).unwrap(), 0.4525);
    }

    #[test]
    fn test_parse_fio_output() {
        let output = r#"
{
  "jobs" : [
    {
      "read" : {
        "io_bytes" : 1965273088,
        "io_kbytes" : 1919212,
        "bw_bytes" : 392976022,
        "bw" : 383765,
        "iops" : 95941.411718,
        "runtime" : 5001,
        "total_ios" : 479803,
        "short_ios" : 0,
        "drop_ios" : 0
      }
    }
  ]
}
"#;

        let bps = 1965273088_f64 / (5001_f64 / 1000_f64);
        assert_eq!(
            parse_fio_output(output, &FioOps::RandomRead, 1).unwrap(),
            bps
        );
        assert_eq!(parse_fio_output(output, &FioOps::Read, 1).unwrap(), bps);

        let output = r#"
{
  "jobs" : [
    {
      "write" : {
        "io_bytes" : 1172783104,
        "io_kbytes" : 1145296,
        "bw_bytes" : 234462835,
        "bw" : 228967,
        "iops" : 57241.903239,
        "runtime" : 5002,
        "total_ios" : 286324,
        "short_ios" : 0,
        "drop_ios" : 0
      }
    },
    {
      "write" : {
        "io_bytes" : 1172234240,
        "io_kbytes" : 1144760,
        "bw_bytes" : 234353106,
        "bw" : 228860,
        "iops" : 57215.113954,
        "runtime" : 5002,
        "total_ios" : 286190,
        "short_ios" : 0,
        "drop_ios" : 0
      }
    }
  ]
}
"#;

        let bps = 1172783104_f64 / (5002_f64 / 1000_f64) + 1172234240_f64 / (5002_f64 / 1000_f64);
        assert_eq!(
            parse_fio_output(output, &FioOps::RandomWrite, 2).unwrap(),
            bps
        );
        assert_eq!(parse_fio_output(output, &FioOps::Write, 2).unwrap(), bps);
    }
}
