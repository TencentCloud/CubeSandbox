// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

mod common;
#[macro_use(crate_authors)]
extern crate clap;
extern crate log_json;

use std::env;
use std::fs::File;
use std::io::Write;
use std::os::fd::RawFd;
use std::os::unix::io::FromRawFd;
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex};

use crate::common::{default_coredump_filter, default_coredump_limit, DEFAULT_LOGGER_BUFFER_SIZE};
use chrono::{DateTime, Local};
use clap::{Arg, ArgAction, ArgGroup, ArgMatches, Command};
use event_monitor::event;
use libc::{EFD_NONBLOCK, SIGSYS};
use log::{error, info, LevelFilter};
use logging::START_TM;
use option_parser::OptionParser;
use rlimit::{setrlimit, Resource};
use seccompiler::SeccompAction;
use std::sync::mpsc::channel;
use thiserror::Error;
use vmm::api::service::{Error as VmmServiceError, VMM_SERVICE};
use vmm::config::{RestoreConfig, VmParams};
#[cfg(target_arch = "x86_64")]
use vmm::vm_config::SgxEpcConfig;
use vmm::vm_config::{
    BalloonConfig, DeviceConfig, DiskConfig, FsConfig, IvshmemConfig, NetConfig, NumaConfig,
    PmemConfig, TpmConfig, UserDeviceConfig, VdpaConfig, VmConfig, VsockConfig,
    DEFAULT_MAX_PHYS_BITS, DEFAULT_MEMORY_MB, DEFAULT_RNG_SOURCE, DEFAULT_VCPUS,
};
use vmm_sys_util::eventfd::EventFd;
use vmm_sys_util::signal::block_signal;
use vmm_sys_util::terminal::Terminal;

extern crate slog;
extern crate slog_scope;

#[derive(Error, Debug)]
enum Error {
    #[error("Failed to create API EventFd: {0}")]
    CreateApiEventFd(#[source] std::io::Error),
    #[cfg(feature = "guest_debug")]
    #[error("Failed to create Debug EventFd: {0}")]
    CreateDebugEventFd(#[source] std::io::Error),
    #[error("Failed to open hypervisor interface (is hypervisor interface available?): {0}")]
    CreateHypervisor(#[source] hypervisor::HypervisorError),
    #[error("Failed to start the VMM thread: {0}")]
    StartVmmThread(#[source] vmm::Error),
    #[error("Error parsing config: {0}")]
    ParsingConfig(vmm::config::Error),
    #[error("Error creating VM: {0:?}")]
    VmCreate(vmm::api::ApiError),
    #[error("Error booting VM: {0:?}")]
    VmBoot(vmm::api::ApiError),
    #[error("Error restoring VM: {0:?}")]
    VmRestore(vmm::api::ApiError),
    #[error("Error parsing restore: {0}")]
    ParsingRestore(vmm::config::Error),
    #[error("Failed to join on VMM thread: {0:?}")]
    ThreadJoin(std::boxed::Box<dyn std::any::Any + std::marker::Send>),
    #[error("VMM thread exited with error: {0}")]
    VmmThread(#[source] vmm::Error),
    #[error("Error parsing --api-socket: {0}")]
    ParsingApiSocket(std::num::ParseIntError),
    #[error("Error parsing --event-monitor: {0}")]
    ParsingEventMonitor(option_parser::OptionParserError),
    #[error("Error parsing --event-monitor: path or fd required")]
    BareEventMonitor,
    #[error("Error doing event monitor I/O: {0}")]
    EventMonitorIo(std::io::Error),
    #[cfg(feature = "guest_debug")]
    #[error("Error parsing --gdb: {0}")]
    ParsingGdb(option_parser::OptionParserError),
    #[cfg(feature = "guest_debug")]
    #[error("Error parsing --gdb: path required")]
    BareGdb,
    #[error("Error creating log file: {0}")]
    LogFileCreation(std::io::Error),
    #[error("Error setting up logger: {0}")]
    LoggerSetup(log::SetLoggerError),
    #[error("Error setting up coredump: {0}")]
    CoredumpSetup(std::io::Error),
    #[error("Error init vmm service: {0}")]
    InitVmmService(VmmServiceError),
}

fn prepare_default_values() -> (String, String, String) {
    let default_vcpus = format! {"boot={},max_phys_bits={}", DEFAULT_VCPUS,DEFAULT_MAX_PHYS_BITS};
    let default_memory = format! {"size={}M", DEFAULT_MEMORY_MB};
    let default_rng = format! {"src={}", DEFAULT_RNG_SOURCE};

    (default_vcpus, default_memory, default_rng)
}

fn create_app(default_vcpus: String, default_memory: String, default_rng: String) -> Command {
    let app = Command::new("cube-hypervisor")
        // 'BUILT_VERSION' is set by the build script 'build.rs' at
        // compile time
        .version(env!("BUILT_VERSION"))
        .author(crate_authors!())
        .about("Launch a cloud-hypervisor VMM.")
        .group(ArgGroup::new("vm-config").multiple(true))
        .group(ArgGroup::new("vmm-config").multiple(true))
        .group(ArgGroup::new("logging").multiple(true))
        .arg(
            Arg::new("cpus")
                .long("cpus")
                .help(
                    "boot=<boot_vcpus>,max=<max_vcpus>,\
                    topology=<threads_per_core>:<cores_per_die>:<dies_per_package>:<packages>,\
                    kvm_hyperv=on|off,max_phys_bits=<maximum_number_of_physical_bits>,\
                    affinity=<list_of_vcpus_with_their_associated_cpuset>,\
                    features=<list_of_features_to_enable>,compatible=vendor|max|ignore",
                )
                .default_value(default_vcpus)
                .group("vm-config"),
        )
        .arg(
            Arg::new("platform")
                .long("platform")
                .help(
                    "num_pci_segments=<num_pci_segments>,iommu_segments=<list_of_segments>,serial_number=<dmi_device_serial_number>,uuid=<dmi_device_uuid>,oem_strings=<list_of_strings>",
                )
                .num_args(1)
                .group("vm-config"),
        )
        .arg(
            Arg::new("memory")
                .long("memory")
                .help(
                    "Memory parameters \
                     \"size=<guest_memory_size>,mergeable=on|off,shared=on|off,\
                     hugepages=on|off,hugepage_size=<hugepage_size>,\
                     hotplug_method=acpi|virtio-mem,\
                     hotplug_size=<hotpluggable_memory_size>,\
                     hotplugged_size=<hotplugged_memory_size>,\
                     prefault=on|off,thp=on|off\"",
                )
                .default_value(default_memory)
                .group("vm-config"),
        )
        .arg(
            Arg::new("memory-zone")
                .long("memory-zone")
                .help(
                    "User defined memory zone parameters \
                     \"size=<guest_memory_region_size>,file=<backing_file>,\
                     shared=on|off,\
                     hugepages=on|off,hugepage_size=<hugepage_size>,\
                     host_numa_node=<node_id>,\
                     id=<zone_identifier>,hotplug_size=<hotpluggable_memory_size>,\
                     hotplugged_size=<hotplugged_memory_size>,\
                     prefault=on|off\"",
                )
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("firmware")
                .long("firmware")
                .help("Path to firmware that is loaded in an architectural specific way")
                .num_args(1)
                .group("vm-config"),
        )
        .arg(
            Arg::new("kernel")
                .long("kernel")
                .help(
                    "Path to kernel to load. This may be a kernel or firmware that supports a PVH \
                entry point (e.g. vmlinux) or architecture equivalent",
                )
                .num_args(1)
                .group("vm-config"),
        )
        .arg(
            Arg::new("initramfs")
                .long("initramfs")
                .help("Path to initramfs image")
                .num_args(1)
                .group("vm-config"),
        )
        .arg(
            Arg::new("cmdline")
                .long("cmdline")
                .help("Kernel command line")
                .num_args(1)
                .group("vm-config"),
        )
        .arg(
            Arg::new("disk")
                .long("disk")
                .help(DiskConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("net")
                .long("net")
                .help(NetConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("rng")
                .long("rng")
                .help(
                    "Random number generator parameters \"src=<entropy_source_path>,iommu=on|off\"",
                )
                .default_value(default_rng)
                .group("vm-config"),
        )
        .arg(
            Arg::new("balloon")
                .long("balloon")
                .help(BalloonConfig::SYNTAX)
                .num_args(1)
                .group("vm-config"),
        )
        .arg(
            Arg::new("fs")
                .long("fs")
                .help(FsConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("pmem")
                .long("pmem")
                .help(PmemConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("serial")
                .long("serial")
                .help("Control serial port: off|null|pty|tty|file=/path/to/a/file")
                .default_value("null")
                .group("vm-config"),
        )
        .arg(
            Arg::new("console")
                .long("console")
                .help(
                    "Control (virtio) console: \"off|null|pty|tty|file=/path/to/a/file,iommu=on|off\"",
                )
                .default_value("tty")
                .group("vm-config"),
        )
        .arg(
            Arg::new("device")
                .long("device")
                .help(DeviceConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("user-device")
                .long("user-device")
                .help(UserDeviceConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("vdpa")
                .long("vdpa")
                .help(VdpaConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("vsock")
                .long("vsock")
                .help(VsockConfig::SYNTAX)
                .num_args(1)
                .group("vm-config"),
        )
        .arg(
            Arg::new("numa")
                .long("numa")
                .help(NumaConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        )
        .arg(
            Arg::new("watchdog")
                .long("watchdog")
                .help("Enable virtio-watchdog")
                .num_args(0)
                .action(ArgAction::SetTrue)
                .group("vm-config"),
        )
        .arg(
            Arg::new("v")
                .short('v')
                .action(ArgAction::Count)
                .help("Sets the level of debugging output")
                .group("logging"),
        )
        .arg(
            Arg::new("log-file")
                .long("log-file")
                .help("Log file. Standard error is used if not specified")
                .num_args(1)
                .group("logging"),
        )
        .arg(
            Arg::new("sandbox-id")
                .long("sandbox-id")
                .help("Ssandbox ID, will show in logs")
                .num_args(1)
                .group("logging"),
        )
        .arg(
            Arg::new("log-stderr")
                .long("log-stderr")
                .help("Logging to stderr, disable log-file.")
                .num_args(0)
                .action(ArgAction::SetTrue)
                .group("logging"),
        )
        .arg(
            Arg::new("sys-ctrl")
                .long("sys-ctrl")
                .help("Enable system controller")
                .num_args(0)
                .action(ArgAction::SetTrue)
                .group("vm-config"),
        )
        .arg(
            Arg::new("api-socket")
                .long("api-socket")
                .help("HTTP API socket (UNIX domain socket): path=</path/to/a/file> or fd=<fd>.")
                .num_args(1)
                .group("vmm-config"),
        )
        .arg(
            Arg::new("event-monitor")
                .long("event-monitor")
                .help("File to report events on: path=</path/to/a/file> or fd=<fd>")
                .num_args(1)
                .group("vmm-config"),
        )
        .arg(
            Arg::new("restore")
                .long("restore")
                .help(RestoreConfig::SYNTAX)
                .num_args(1)
                .group("vmm-config"),
        )
        .arg(
            Arg::new("seccomp")
                .long("seccomp")
                .num_args(1)
                .value_parser(["true", "false", "log", "process"])
                .default_value("process"),
        )
        .arg(
            Arg::new("tpm")
                .long("tpm")
                .num_args(1)
                .help(TpmConfig::SYNTAX)
                .group("vm-config"),

        )
        .arg(
            Arg::new("coredump")
                .long("coredump")
                .help("filter=<filter>,limit=<limit>")
                .num_args(1..)
                .group("vmm-config"),
        )
        .arg(
            Arg::new("pvpanic")
                .long("pvpanic")
                .help("Enable pvpanic-pci")
                .num_args(0)
                .action(ArgAction::SetTrue)
                .group("vm-config"),
        )
        .arg(
            Arg::new("ivshmem")
                .long("ivshmem")
                .help(IvshmemConfig::SYNTAX)
                .num_args(1..)
                .group("vm-config"),
        );

    #[cfg(target_arch = "x86_64")]
    let app = app.arg(
        Arg::new("sgx-epc")
            .long("sgx-epc")
            .help(SgxEpcConfig::SYNTAX)
            .num_args(1..)
            .group("vm-config"),
    );

    #[cfg(feature = "guest_debug")]
    let app = app.arg(
        Arg::new("gdb")
            .long("gdb")
            .help("GDB socket (UNIX domain socket): path=</path/to/a/file>")
            .num_args(1)
            .group("vmm-config"),
    );

    let app = app.arg(
        Arg::new("snapshot-version")
            .short('D')
            .long("snapshot-version")
            .exclusive(true)
            .action(clap::ArgAction::SetTrue)
            .help("snapshot version, restore from different snapshot version is not supported"),
    );

    app
}

fn start_vmm(
    cmd_arguments: ArgMatches,
    now: std::time::Instant,
    local: DateTime<Local>,
) -> Result<Option<String>, Error> {
    let tm1 = std::time::Instant::now().duration_since(now);

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

    let mut defer_logger_thread: bool = true;
    let log_level = match cmd_arguments.get_count("v") {
        0 => LevelFilter::Warn,
        1 => LevelFilter::Info,
        2 => LevelFilter::Debug,
        _ => LevelFilter::Trace,
    };

    if log_level > LevelFilter::Info {
        defer_logger_thread = false;
    }

    // Init log file name.
    let log_file_name = if let Some(file) = cmd_arguments.get_one::<String>("log-file") {
        file.clone()
    } else {
        String::from(crate::common::DEFAULT_LOG_FILE)
    };

    // Init log file and log async mode.
    let (log_file, log_async) = {
        if !cmd_arguments.get_flag("log-stderr") {
            // Create directories. We need this when we add new machine in
            // Cube environment.
            let dir = std::path::Path::new(&log_file_name).parent().unwrap();
            std::fs::create_dir_all(dir).map_err(Error::LogFileCreation)?;

            // Open async log file later in cube-log
            // thread. Let cube-log thread handle the
            // log file rotate(reopen) logic.
            (None, true)
        } else {
            let file: Box<dyn std::io::Write + Send> = Box::new(std::io::stderr());
            (Some(file), false)
        }
    };

    let sandbox_id: String = if let Some(sandboxid) = cmd_arguments.get_one::<String>("sandbox-id")
    {
        sandboxid.to_string()
    } else {
        String::from("cube-hypervisor")
    };

    #[cfg(feature = "logger_debug")]
    let mut logger_dbg_file_name = log_file_name.clone();
    #[cfg(feature = "logger_debug")]
    logger_dbg_file_name.push_str(".dbg");
    #[cfg(feature = "logger_debug")]
    let logger_dbg_file = Arc::new(Mutex::new(
        std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .append(true)
            .open(logger_dbg_file_name)
            .unwrap(),
    ));

    let wait_vcpu_started = Arc::new(AtomicBool::new(false));
    *START_TM.lock().unwrap() = now;

    log::set_boxed_logger(Box::new(crate::common::Logger::new(
        Mutex::new(log_file),
        now,
        sandbox_id.clone(),
        log_async,
        defer_logger_thread,
        Arc::new(Mutex::new(Vec::with_capacity(DEFAULT_LOGGER_BUFFER_SIZE))),
        Arc::new(Mutex::new(AtomicBool::new(false))),
        log_file_name,
        wait_vcpu_started.clone(),
        Arc::new(AtomicBool::new(false)),
        #[cfg(feature = "logger_debug")]
        logger_dbg_file,
    )))
    .map(|()| log::set_max_level(log_level))
    .map_err(Error::LoggerSetup)?;

    #[cfg(not(feature = "lib_support"))]
    std::panic::set_hook(Box::new(|info| {
        info!("{info:?}");
        log::logger().flush();
    }));

    let tm2 = std::time::Instant::now().duration_since(now);

    info!(
            "Cube-Hypervisor version {} started with PID: {}, prepare main {}ms, prepare vmm {}ms, start {}",
            env!("BUILT_VERSION"),
            std::process::id(),
            tm1.as_millis(),
            tm2.as_millis(),
            local
    );

    let mut coredump_limit = default_coredump_limit();
    let mut coredump_filter = default_coredump_filter();
    if let Some(coredump_config) = cmd_arguments.get_one::<String>("coredump") {
        let mut parser = OptionParser::new();

        parser.add("filter").add("limit");
        parser.parse(coredump_config).unwrap_or_default();

        if let Some(filter) = parser.get("filter") {
            coredump_filter = filter;
        }

        if let Some(limit) = parser.get("limit") {
            coredump_limit = limit.parse::<u64>().unwrap();
        }
    }

    let mut filter_file = File::options()
        .write(true)
        .open("/proc/self/coredump_filter")
        .map_err(Error::CoredumpSetup)?;
    filter_file
        .write(coredump_filter.as_bytes())
        .map_err(Error::CoredumpSetup)?;
    setrlimit(Resource::CORE, coredump_limit, coredump_limit).map_err(Error::CoredumpSetup)?;

    let (api_socket_path, api_socket_fd) =
        if let Some(socket_config) = cmd_arguments.get_one::<String>("api-socket") {
            let mut parser = OptionParser::new();
            parser.add("path").add("fd");
            parser.parse(socket_config).unwrap_or_default();

            if let Some(fd) = parser.get("fd") {
                (
                    None,
                    Some(fd.parse::<RawFd>().map_err(Error::ParsingApiSocket)?),
                )
            } else if let Some(path) = parser.get("path") {
                (Some(path), None)
            } else {
                (
                    cmd_arguments
                        .get_one::<String>("api-socket")
                        .map(|s| s.to_string()),
                    None,
                )
            }
        } else {
            (None, None)
        };

    if let Some(monitor_config) = cmd_arguments.get_one::<String>("event-monitor") {
        let mut parser = OptionParser::new();
        parser.add("path").add("fd");
        parser
            .parse(monitor_config)
            .map_err(Error::ParsingEventMonitor)?;

        let file = if parser.is_set("fd") {
            let fd = parser
                .convert("fd")
                .map_err(Error::ParsingEventMonitor)?
                .unwrap();
            unsafe { File::from_raw_fd(fd) }
        } else if parser.is_set("path") {
            std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .open(parser.get("path").unwrap())
                .map_err(Error::EventMonitorIo)?
        } else {
            return Err(Error::BareEventMonitor);
        };
        event_monitor::set_monitor(file).map_err(Error::EventMonitorIo)?;
    }

    let (api_request_sender, api_request_receiver) = channel();
    let (res_sender, res_receiver) = channel();
    let api_evt = EventFd::new(EFD_NONBLOCK).map_err(Error::CreateApiEventFd)?;

    VMM_SERVICE
        .lock()
        .unwrap()
        .init(
            api_request_sender,
            api_evt.try_clone().unwrap(),
            res_receiver,
        )
        .map_err(Error::InitVmmService)?;

    let seccomp_action = if let Some(seccomp_value) = cmd_arguments.get_one::<String>("seccomp") {
        match seccomp_value as &str {
            "true" => SeccompAction::Trap,
            "false" => SeccompAction::Allow,
            "log" => SeccompAction::Log,
            "process" => SeccompAction::KillProcess,
            _ => {
                // The user providing an invalid value will be rejected by clap
                panic!("Invalid parameter {} for \"--seccomp\" flag", seccomp_value);
            }
        }
    } else {
        SeccompAction::KillProcess
    };

    if seccomp_action == SeccompAction::Trap || seccomp_action == SeccompAction::KillProcess {
        // SAFETY: We only using signal_hook for managing signals and only execute signal
        // handler safe functions (writing to stderr) and manipulating signals.
        unsafe {
            signal_hook::low_level::register(SIGSYS, || {
                eprint!("\nError signal: SIGSYS, possible seccomp violation.\n");
                error!("Error signal: SIGSYS, possible seccomp violation.");
                signal_hook::low_level::emulate_default_handler(SIGSYS).unwrap();
            })
        }
        .map_err(|e| error!("Error adding SIGSYS signal handler: {e}"))
        .ok();
    }

    // Before we start any threads, mask the signals we'll be
    // installing handlers for, to make sure they only ever run on the
    // dedicated signal handling thread we'll start in a bit.
    for sig in &vmm::vm::Vm::HANDLED_SIGNALS {
        if let Err(e) = block_signal(*sig) {
            error!("Error blocking signals: {e}");
        }
    }

    for sig in &vmm::Vmm::HANDLED_SIGNALS {
        if let Err(e) = block_signal(*sig) {
            error!("Error blocking signals: {e}");
        }
    }

    event!("vmm", "starting");

    let hypervisor = hypervisor::new().map_err(Error::CreateHypervisor)?;

    #[cfg(feature = "guest_debug")]
    let gdb_socket_path = if let Some(gdb_config) = cmd_arguments.get_one::<String>("gdb") {
        let mut parser = OptionParser::new();
        parser.add("path");
        parser.parse(gdb_config).map_err(Error::ParsingGdb)?;

        if parser.is_set("path") {
            Some(std::path::PathBuf::from(parser.get("path").unwrap()))
        } else {
            return Err(Error::BareGdb);
        }
    } else {
        None
    };
    #[cfg(feature = "guest_debug")]
    let debug_evt = EventFd::new(EFD_NONBLOCK).map_err(Error::CreateDebugEventFd)?;
    #[cfg(feature = "guest_debug")]
    let vm_debug_evt = EventFd::new(EFD_NONBLOCK).map_err(Error::CreateDebugEventFd)?;

    let vmm_thread = vmm::start_vmm_thread(
        env!("CARGO_PKG_VERSION").to_string(),
        &api_socket_path,
        api_socket_fd,
        api_evt,
        api_request_receiver,
        res_sender,
        #[cfg(feature = "guest_debug")]
        gdb_socket_path,
        #[cfg(feature = "guest_debug")]
        debug_evt.try_clone().unwrap(),
        #[cfg(feature = "guest_debug")]
        vm_debug_evt.try_clone().unwrap(),
        &seccomp_action,
        hypervisor,
        sandbox_id,
        wait_vcpu_started,
        None,
    )
    .map_err(Error::StartVmmThread)?;

    let payload_present =
        cmd_arguments.contains_id("kernel") || cmd_arguments.contains_id("firmware");

    if payload_present {
        let vm_params = VmParams::from_arg_matches(&cmd_arguments);
        let vm_config = VmConfig::parse(vm_params).map_err(Error::ParsingConfig)?;

        // Create and boot the VM based off the VM config we just built.
        vmm::api::vm_create(Box::new(vm_config)).map_err(Error::VmCreate)?;
        vmm::api::vm_boot().map_err(Error::VmBoot)?;
    } else if let Some(restore_params) = cmd_arguments.get_one::<String>("restore") {
        vmm::api::vm_restore(Arc::new(
            RestoreConfig::parse(restore_params).map_err(Error::ParsingRestore)?,
        ))
        .map_err(Error::VmRestore)?;
    }

    vmm_thread
        .join()
        .map_err(Error::ThreadJoin)?
        .map_err(Error::VmmThread)?;

    Ok(api_socket_path)
}

fn main() {
    let now = std::time::Instant::now();
    let local = Local::now();
    // Ensure all created files (.e.g sockets) are only accessible by this user
    let _ = unsafe { libc::umask(0o077) };

    let (default_vcpus, default_memory, default_rng) = prepare_default_values();
    let cmd_arguments = create_app(default_vcpus, default_memory, default_rng).get_matches();
    if cmd_arguments.get_flag("snapshot-version") {
        println!("Sanpshot Version: {}", vmm::api::SNAPSHOT_VERSION);
        return;
    }
    let exit_code = match start_vmm(cmd_arguments, now, local) {
        Ok(path) => {
            path.map(|s| std::fs::remove_file(s).ok());
            0
        }
        Err(e) => {
            eprintln!("{}", e);
            log::logger().flush();
            1
        }
    };

    let on_tty = unsafe { libc::isatty(libc::STDIN_FILENO) } != 0;
    if on_tty {
        // Don't forget to set the terminal in canonical mode
        // before to exit.
        std::io::stdin().lock().set_canon_mode().unwrap();
    }

    std::process::exit(exit_code);
}

#[cfg(test)]
mod unit_tests {
    use crate::{create_app, prepare_default_values};
    use std::path::PathBuf;
    use vmm::config::VmParams;
    use vmm::vm_config::{
        CompatibleMode, ConsoleConfig, ConsoleOutputMode, CpuFeatures, CpusConfig, HotplugMethod,
        MemoryConfig, PayloadConfig, RngConfig, VmConfig,
    };

    fn get_vm_config_from_vec(args: &[&str]) -> VmConfig {
        let (default_vcpus, default_memory, default_rng) = prepare_default_values();
        let cmd_arguments =
            create_app(default_vcpus, default_memory, default_rng).get_matches_from(args);

        let vm_params = VmParams::from_arg_matches(&cmd_arguments);

        VmConfig::parse(vm_params).unwrap()
    }

    fn compare_vm_config_cli_vs_json(
        cli: &[&str],
        openapi: &str,
        equal: bool,
    ) -> (VmConfig, VmConfig) {
        let cli_vm_config = get_vm_config_from_vec(cli);
        let openapi_vm_config: VmConfig = serde_json::from_str(openapi).unwrap();

        if equal {
            assert_eq!(cli_vm_config, openapi_vm_config);
        } else {
            assert_ne!(cli_vm_config, openapi_vm_config);
        }

        (cli_vm_config, openapi_vm_config)
    }

    #[test]
    fn test_valid_vm_config_default() {
        let cli = vec!["cloud-hypervisor", "--kernel", "/path/to/kernel"];
        let openapi = r#"{ "payload": {"kernel": "/path/to/kernel"} }"#;

        // First we check we get identical VmConfig structures.
        let (result_vm_config, _) = compare_vm_config_cli_vs_json(&cli, openapi, true);

        // As a second step, we validate all the default values.
        let expected_vm_config = VmConfig {
            cpus: CpusConfig {
                boot_vcpus: 1,
                max_vcpus: 1,
                topology: None,
                kvm_hyperv: false,
                max_phys_bits: 46,
                affinity: None,
                features: CpuFeatures::default(),
                compatible: CompatibleMode::Vendor,
            },
            memory: MemoryConfig {
                size: 536_870_912,
                mergeable: false,
                hotplug_method: HotplugMethod::Acpi,
                hotplug_size: None,
                hotplugged_size: None,
                shared: false,
                hugepages: false,
                hugepage_size: None,
                prefault: false,
                zones: None,
                thp: true,
                dirty_log: false,
            },
            payload: Some(PayloadConfig {
                kernel: Some(PathBuf::from("/path/to/kernel")),
                ..Default::default()
            }),
            disks: None,
            net: None,
            rng: RngConfig {
                src: PathBuf::from("/dev/urandom"),
                iommu: false,
            },
            balloon: None,
            fs: None,
            pmem: None,
            serial: ConsoleConfig {
                file: None,
                mode: ConsoleOutputMode::Null,
                sigwinch: false,
                iommu: false,
            },
            console: ConsoleConfig {
                file: None,
                mode: ConsoleOutputMode::Tty,
                sigwinch: false,
                iommu: false,
            },
            devices: None,
            user_devices: None,
            vdpa: None,
            vsock: None,
            pvpanic: false,
            iommu: false,
            #[cfg(target_arch = "x86_64")]
            sgx_epc: None,
            numa: None,
            watchdog: false,
            #[cfg(feature = "guest_debug")]
            gdb: false,
            platform: None,
            tpm: None,
            sys_ctrl: false,
            ivshmem: None,
        };

        assert_eq!(expected_vm_config, result_vm_config);
    }

    #[test]
    fn test_valid_vm_config_cpus() {
        vec![
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--cpus",
                    "boot=1",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "cpus": {"boot_vcpus": 1, "max_vcpus": 1}
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--cpus",
                    "boot=1,max=3",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "cpus": {"boot_vcpus": 1, "max_vcpus": 3}
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--cpus",
                    "boot=2,max=4",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "cpus": {"boot_vcpus": 1, "max_vcpus": 3}
                }"#,
                false,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_memory() {
        vec![
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "size=1073741824"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory": {"size": 1073741824}
                }"#,
                true,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "size=1G"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory": {"size": 1073741824}
                }"#,
                true,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "size=1G,mergeable=on"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory": {"size": 1073741824, "mergeable": true}
                }"#,
                true,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "size=1G,mergeable=off"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory": {"size": 1073741824, "mergeable": false}
                }"#,
                true,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "size=1G,mergeable=on"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory": {"size": 1073741824, "mergeable": false}
                }"#,
                false,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "size=1G,hotplug_size=1G"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory": {"size": 1073741824, "hotplug_method": "Acpi", "hotplug_size": 1073741824}
                }"#,
                true,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "size=1G,hotplug_method=virtio-mem,hotplug_size=1G"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory": {"size": 1073741824, "hotplug_method": "VirtioMem", "hotplug_size": 1073741824}
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_kernel() {
        vec![(
            vec!["cloud-hypervisor", "--kernel", "/path/to/kernel"],
            r#"{
                "payload": {"kernel": "/path/to/kernel"}
            }"#,
            true,
        )]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_cmdline() {
        vec![(
            vec![
                "cloud-hypervisor",
                "--kernel",
                "/path/to/kernel",
                "--cmdline",
                "arg1=foo arg2=bar",
            ],
            r#"{
                "payload": {"kernel": "/path/to/kernel", "cmdline": "arg1=foo arg2=bar"}
            }"#,
            true,
        )]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_disks() {
        vec![
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--disk",
                    "path=/path/to/disk/1",
                    "path=/path/to/disk/2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "disks": [
                        {"path": "/path/to/disk/1"},
                        {"path": "/path/to/disk/2"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--disk",
                    "path=/path/to/disk/1",
                    "path=/path/to/disk/2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "disks": [
                        {"path": "/path/to/disk/1"}
                    ]
                }"#,
                false,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--memory",
                    "shared=true",
                    "--disk",
                    "vhost_user=true,socket=/tmp/sock1",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory" : { "shared": true, "size": 536870912 },
                    "disks": [
                        {"vhost_user":true, "vhost_socket":"/tmp/sock1"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--memory",
                    "shared=true",
                    "--disk",
                    "vhost_user=true,socket=/tmp/sock1",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory" : { "shared": true, "size": 536870912 },
                    "disks": [
                        {"vhost_user":true, "vhost_socket":"/tmp/sock1"}
                    ]
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_net() {
        vec![
            // This test is expected to fail because the default MAC address is
            // randomly generated. There's no way we can have twice the same
            // default value.
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--net", "mac="],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": []
                }"#,
                false,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--net", "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--cpus", "boot=2",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8,num_queues=4",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "cpus": {"boot_vcpus": 2, "max_vcpus": 2},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8", "num_queues": 4}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--cpus", "boot=2",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8,num_queues=4,queue_size=128",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "cpus": {"boot_vcpus": 2, "max_vcpus": 2},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8", "num_queues": 4, "queue_size": 128}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8,num_queues=2,queue_size=256",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8", "num_queues": 2, "queue_size": 256}
                    ]
                }"#,
                true,
            ),
            #[cfg(target_arch = "x86_64")]
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8,num_queues=2,queue_size=256,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8", "num_queues": 2, "queue_size": 256, "iommu": true}
                    ]
                }"#,
                false,
            ),
            #[cfg(target_arch = "x86_64")]
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8,num_queues=2,queue_size=256,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8", "num_queues": 2, "queue_size": 256, "iommu": true}
                    ],
                    "iommu": true
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--net",
                    "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,tap=tap0,ip=1.2.3.4,mask=5.6.7.8,num_queues=2,queue_size=256,iommu=off",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "tap": "tap0", "ip": "1.2.3.4", "mask": "5.6.7.8", "num_queues": 2, "queue_size": 256, "iommu": false}
                    ]
                }"#,
                true,
            ),
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel", "--memory", "shared=true", "--net", "mac=12:34:56:78:90:ab,host_mac=34:56:78:90:ab:cd,vhost_user=true,socket=/tmp/sock"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory" : { "shared": true, "size": 536870912 },
                    "net": [
                        {"mac": "12:34:56:78:90:ab", "host_mac": "34:56:78:90:ab:cd", "vhost_user": true, "vhost_socket": "/tmp/sock"}
                    ]
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_rng() {
        vec![(
            vec![
                "cloud-hypervisor",
                "--kernel",
                "/path/to/kernel",
                "--rng",
                "src=/path/to/entropy/source",
            ],
            r#"{
                "payload": {"kernel": "/path/to/kernel"},
                "rng": {"src": "/path/to/entropy/source"}
            }"#,
            true,
        )]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_fs() {
        vec![
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--memory", "shared=true",
                    "--fs",
                    "tag=virtiofs1,socket=/path/to/sock1",
                    "tag=virtiofs2,socket=/path/to/sock2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory" : { "shared": true, "size": 536870912 },
                    "fs": [
                        {"tag": "virtiofs1", "socket": "/path/to/sock1"},
                        {"tag": "virtiofs2", "socket": "/path/to/sock2"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--memory", "shared=true",
                    "--fs",
                    "tag=virtiofs1,socket=/path/to/sock1",
                    "tag=virtiofs2,socket=/path/to/sock2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory" : { "shared": true, "size": 536870912 },
                    "fs": [
                        {"tag": "virtiofs1", "socket": "/path/to/sock1"}
                    ]
                }"#,
                false,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--memory", "shared=true", "--cpus", "boot=4",
                    "--fs",
                    "tag=virtiofs1,socket=/path/to/sock1,num_queues=4",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory" : { "shared": true, "size": 536870912 },
                    "cpus": {"boot_vcpus": 4, "max_vcpus": 4},
                    "fs": [
                        {"tag": "virtiofs1", "socket": "/path/to/sock1", "num_queues": 4}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor", "--kernel", "/path/to/kernel",
                    "--memory", "shared=true", "--cpus", "boot=4",
                    "--fs",
                    "tag=virtiofs1,socket=/path/to/sock1,num_queues=4,queue_size=128"
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "memory" : { "shared": true, "size": 536870912 },
                    "cpus": {"boot_vcpus": 4, "max_vcpus": 4},
                    "fs": [
                        {"tag": "virtiofs1", "socket": "/path/to/sock1", "num_queues": 4, "queue_size": 128}
                    ]
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_pmem() {
        vec![
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--pmem",
                    "file=/path/to/img/1,size=1G",
                    "file=/path/to/img/2,size=2G",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "pmem": [
                        {"file": "/path/to/img/1", "size": 1073741824},
                        {"file": "/path/to/img/2", "size": 2147483648}
                    ]
                }"#,
                true,
            ),
            #[cfg(target_arch = "x86_64")]
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--pmem",
                    "file=/path/to/img/1,size=1G,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "pmem": [
                        {"file": "/path/to/img/1", "size": 1073741824, "iommu": true}
                    ],
                    "iommu": true
                }"#,
                true,
            ),
            #[cfg(target_arch = "x86_64")]
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--pmem",
                    "file=/path/to/img/1,size=1G,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "pmem": [
                        {"file": "/path/to/img/1", "size": 1073741824, "iommu": true}
                    ]
                }"#,
                false,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_serial_console() {
        vec![
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "serial": {"mode": "Null"},
                    "console": {"mode": "Tty"}
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--serial",
                    "null",
                    "--console",
                    "tty",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"}
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--serial",
                    "tty",
                    "--console",
                    "off",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "serial": {"mode": "Tty"},
                    "console": {"mode": "Off"}
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_serial_pty_console_pty() {
        vec![
            (
                vec!["cloud-hypervisor", "--kernel", "/path/to/kernel"],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "serial": {"mode": "Null"},
                    "console": {"mode": "Tty"}
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--serial",
                    "null",
                    "--console",
                    "tty",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"}
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--serial",
                    "pty",
                    "--console",
                    "pty",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "serial": {"mode": "Pty"},
                    "console": {"mode": "Pty"}
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    #[cfg(target_arch = "x86_64")]
    fn test_valid_vm_config_devices() {
        vec![
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--device",
                    "path=/path/to/device/1",
                    "path=/path/to/device/2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "devices": [
                        {"path": "/path/to/device/1"},
                        {"path": "/path/to/device/2"}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--device",
                    "path=/path/to/device/1",
                    "path=/path/to/device/2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "devices": [
                        {"path": "/path/to/device/1"}
                    ]
                }"#,
                false,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--device",
                    "path=/path/to/device,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "devices": [
                        {"path": "/path/to/device", "iommu": true}
                    ],
                    "iommu": true
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--device",
                    "path=/path/to/device,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "devices": [
                        {"path": "/path/to/device", "iommu": true}
                    ]
                }"#,
                false,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--device",
                    "path=/path/to/device,iommu=off",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "devices": [
                        {"path": "/path/to/device", "iommu": false}
                    ]
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_vdpa() {
        vec![
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--vdpa",
                    "path=/path/to/device/1",
                    "path=/path/to/device/2,num_queues=2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "vdpa": [
                        {"path": "/path/to/device/1", "num_queues": 1},
                        {"path": "/path/to/device/2", "num_queues": 2}
                    ]
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--vdpa",
                    "path=/path/to/device/1",
                    "path=/path/to/device/2",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "vdpa": [
                        {"path": "/path/to/device/1"}
                    ]
                }"#,
                false,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_vsock() {
        vec![
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--vsock",
                    "cid=123,socket=/path/to/sock/1",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "vsock": {"cid": 123, "socket": "/path/to/sock/1"}
                }"#,
                true,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--vsock",
                    "cid=124,socket=/path/to/sock/1",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "vsock": {"cid": 123, "socket": "/path/to/sock/1"}
                }"#,
                false,
            ),
            #[cfg(target_arch = "x86_64")]
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--vsock",
                    "cid=123,socket=/path/to/sock/1,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "vsock": {"cid": 123, "socket": "/path/to/sock/1", "iommu": true},
                    "iommu": true
                }"#,
                true,
            ),
            #[cfg(target_arch = "x86_64")]
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--vsock",
                    "cid=123,socket=/path/to/sock/1,iommu=on",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "vsock": {"cid": 123, "socket": "/path/to/sock/1", "iommu": true}
                }"#,
                false,
            ),
            (
                vec![
                    "cloud-hypervisor",
                    "--kernel",
                    "/path/to/kernel",
                    "--vsock",
                    "cid=123,socket=/path/to/sock/1,iommu=off",
                ],
                r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "vsock": {"cid": 123, "socket": "/path/to/sock/1", "iommu": false}
                }"#,
                true,
            ),
        ]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }

    #[test]
    fn test_valid_vm_config_tpm_socket() {
        vec![(
            vec![
                "cloud-hypervisor",
                "--kernel",
                "/path/to/kernel",
                "--tpm",
                "socket=/path/to/tpm/sock",
            ],
            r#"{
                    "payload": {"kernel": "/path/to/kernel"},
                    "tpm": {"socket": "/path/to/tpm/sock"}
                }"#,
            true,
        )]
        .iter()
        .for_each(|(cli, openapi, equal)| {
            compare_vm_config_cli_vs_json(cli, openapi, *equal);
        });
    }
}
