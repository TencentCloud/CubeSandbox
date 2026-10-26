// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

#[macro_use(crate_authors)]
extern crate clap;

use api_client::simple_api_command;
use api_client::simple_api_command_with_fds;
use api_client::Error as ApiClientError;
use clap::{Arg, ArgAction, ArgMatches, Command};
use option_parser::{ByteSized, ByteSizedParseError};
use std::fmt;
use std::io::Read;
use std::os::unix::net::UnixStream;
use std::process;
use vmm::config::RestoreConfig;
use vmm::vm_config::{
    DeviceConfig, DiskConfig, FsConfig, NetConfig, PmemConfig, UserDeviceConfig, VdpaConfig,
    VsockConfig,
};
use vmm::{SnapshotConfig, SnapshotType};

const MEMORY_VOL_URL_HELP: &str =
    "Optional existing memory blob path for storing memory range data on a separate volume \
     (for example /dev/vdb or file:///dev/vdb)";

#[derive(Debug)]
enum Error {
    Connect(std::io::Error),
    ApiClient(ApiClientError),
    InvalidCpuCount(std::num::ParseIntError),
    InvalidMemorySize(ByteSizedParseError),
    InvalidBalloonSize(ByteSizedParseError),
    AddDeviceConfig(vmm::config::Error),
    AddDiskConfig(vmm::config::Error),
    AddFsConfig(vmm::config::Error),
    AddPmemConfig(vmm::config::Error),
    AddNetConfig(vmm::config::Error),
    AddUserDeviceConfig(vmm::config::Error),
    AddVdpaConfig(vmm::config::Error),
    AddVsockConfig(vmm::config::Error),
    Restore(vmm::config::Error),
    ReadingStdin(std::io::Error),
    ReadingFile(std::io::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        use Error::*;
        match self {
            ApiClient(e) => e.fmt(f),
            Connect(e) => write!(f, "Error opening HTTP socket: {}", e),
            InvalidCpuCount(e) => write!(f, "Error parsing CPU count: {}", e),
            InvalidMemorySize(e) => write!(f, "Error parsing memory size: {:?}", e),
            InvalidBalloonSize(e) => write!(f, "Error parsing balloon size: {:?}", e),
            AddDeviceConfig(e) => write!(f, "Error parsing device syntax: {}", e),
            AddDiskConfig(e) => write!(f, "Error parsing disk syntax: {}", e),
            AddFsConfig(e) => write!(f, "Error parsing filesystem syntax: {}", e),
            AddPmemConfig(e) => write!(f, "Error parsing persistent memory syntax: {}", e),
            AddNetConfig(e) => write!(f, "Error parsing network syntax: {}", e),
            AddUserDeviceConfig(e) => write!(f, "Error parsing user device syntax: {}", e),
            AddVdpaConfig(e) => write!(f, "Error parsing vDPA device syntax: {}", e),
            AddVsockConfig(e) => write!(f, "Error parsing vsock syntax: {}", e),
            Restore(e) => write!(f, "Error parsing restore syntax: {}", e),
            ReadingStdin(e) => write!(f, "Error reading from stdin: {}", e),
            ReadingFile(e) => write!(f, "Error reading from file: {}", e),
        }
    }
}

fn resize_api_command(
    socket: &mut UnixStream,
    cpus: Option<&str>,
    memory: Option<&str>,
    balloon: Option<&str>,
) -> Result<(), Error> {
    let desired_vcpus: Option<u8> = if let Some(cpus) = cpus {
        Some(cpus.parse().map_err(Error::InvalidCpuCount)?)
    } else {
        None
    };

    let desired_ram: Option<u64> = if let Some(memory) = memory {
        Some(
            memory
                .parse::<ByteSized>()
                .map_err(Error::InvalidMemorySize)?
                .0,
        )
    } else {
        None
    };

    let desired_balloon: Option<u64> = if let Some(balloon) = balloon {
        Some(
            balloon
                .parse::<ByteSized>()
                .map_err(Error::InvalidBalloonSize)?
                .0,
        )
    } else {
        None
    };

    let resize = vmm::api::VmResizeData {
        desired_vcpus,
        desired_ram,
        desired_balloon,
    };

    simple_api_command(
        socket,
        "PUT",
        "resize",
        Some(&serde_json::to_string(&resize).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn resize_zone_api_command(socket: &mut UnixStream, id: &str, size: &str) -> Result<(), Error> {
    let resize_zone = vmm::api::VmResizeZoneData {
        id: id.to_owned(),
        desired_ram: size
            .parse::<ByteSized>()
            .map_err(Error::InvalidMemorySize)?
            .0,
    };

    simple_api_command(
        socket,
        "PUT",
        "resize-zone",
        Some(&serde_json::to_string(&resize_zone).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn add_device_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let device_config = DeviceConfig::parse(config).map_err(Error::AddDeviceConfig)?;

    simple_api_command(
        socket,
        "PUT",
        "add-device",
        Some(&serde_json::to_string(&device_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn add_user_device_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let device_config = UserDeviceConfig::parse(config).map_err(Error::AddUserDeviceConfig)?;

    simple_api_command(
        socket,
        "PUT",
        "add-user-device",
        Some(&serde_json::to_string(&device_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn remove_device_api_command(socket: &mut UnixStream, id: &str) -> Result<(), Error> {
    let remove_device_data = vmm::api::VmRemoveDeviceData { id: id.to_owned() };

    simple_api_command(
        socket,
        "PUT",
        "remove-device",
        Some(&serde_json::to_string(&remove_device_data).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn add_disk_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let disk_config = DiskConfig::parse(config).map_err(Error::AddDiskConfig)?;

    simple_api_command(
        socket,
        "PUT",
        "add-disk",
        Some(&serde_json::to_string(&disk_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn add_fs_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let fs_config = FsConfig::parse(config).map_err(Error::AddFsConfig)?;

    simple_api_command(
        socket,
        "PUT",
        "add-fs",
        Some(&serde_json::to_string(&fs_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn add_pmem_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let pmem_config = PmemConfig::parse(config).map_err(Error::AddPmemConfig)?;

    simple_api_command(
        socket,
        "PUT",
        "add-pmem",
        Some(&serde_json::to_string(&pmem_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn add_net_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let mut net_config = NetConfig::parse(config).map_err(Error::AddNetConfig)?;

    // NetConfig is modified on purpose here by taking the list of file
    // descriptors out. Keeping the list and send it to the server side
    // process would not make any sense since the file descriptor may be
    // represented with different values.
    let fds = net_config.fds.take().unwrap_or_default();

    simple_api_command_with_fds(
        socket,
        "PUT",
        "add-net",
        Some(&serde_json::to_string(&net_config).unwrap()),
        fds,
    )
    .map_err(Error::ApiClient)
}

fn add_vdpa_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let vdpa_config = VdpaConfig::parse(config).map_err(Error::AddVdpaConfig)?;

    simple_api_command(
        socket,
        "PUT",
        "add-vdpa",
        Some(&serde_json::to_string(&vdpa_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn add_vsock_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let vsock_config = VsockConfig::parse(config).map_err(Error::AddVsockConfig)?;

    simple_api_command(
        socket,
        "PUT",
        "add-vsock",
        Some(&serde_json::to_string(&vsock_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn snapshot_api_command(
    socket: &mut UnixStream,
    url: &str,
    snapshot_type: SnapshotType,
    memory_vol_url: Option<String>,
) -> Result<(), Error> {
    snapshot_command(socket, "snapshot", url, snapshot_type, memory_vol_url)
}

fn snapshot_command(
    socket: &mut UnixStream,
    endpoint: &str,
    url: &str,
    snapshot_type: SnapshotType,
    memory_vol_url: Option<String>,
) -> Result<(), Error> {
    let snapshot_config = SnapshotConfig {
        destination_url: String::from(url),
        snapshot_type,
        memory_vol_url,
    };

    simple_api_command(
        socket,
        "PUT",
        endpoint,
        Some(&serde_json::to_string(&snapshot_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn parse_snapshot_command_args(sub_matches: &ArgMatches) -> (&str, SnapshotType, Option<String>) {
    let snapshot_type = sub_matches
        .get_one::<String>("snapshot_type")
        .map(|s| s.parse::<SnapshotType>().unwrap_or_default())
        .unwrap_or_default();
    let memory_vol_url = sub_matches.get_one::<String>("memory_vol_url").cloned();

    (
        sub_matches.get_one::<String>("snapshot_config").unwrap(),
        snapshot_type,
        memory_vol_url,
    )
}

fn restore_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let restore_config = RestoreConfig::parse(config).map_err(Error::Restore)?;

    simple_api_command(
        socket,
        "PUT",
        "restore",
        Some(&serde_json::to_string(&restore_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn pause2snapshot_api_command(
    socket: &mut UnixStream,
    url: &str,
    snapshot_type: SnapshotType,
    memory_vol_url: Option<String>,
) -> Result<(), Error> {
    snapshot_command(socket, "pause2snapshot", url, snapshot_type, memory_vol_url)
}

fn resume_from_snap_api_command(socket: &mut UnixStream, config: &str) -> Result<(), Error> {
    let restore_config = RestoreConfig::parse(config).map_err(Error::Restore)?;

    simple_api_command(
        socket,
        "PUT",
        "resume-from-snapshot",
        Some(&serde_json::to_string(&restore_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn coredump_api_command(socket: &mut UnixStream, destination_url: &str) -> Result<(), Error> {
    let coredump_config = vmm::api::VmCoredumpData {
        destination_url: String::from(destination_url),
    };

    simple_api_command(
        socket,
        "PUT",
        "coredump",
        Some(&serde_json::to_string(&coredump_config).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn receive_migration_api_command(socket: &mut UnixStream, url: &str) -> Result<(), Error> {
    let receive_migration_data = vmm::api::VmReceiveMigrationData {
        receiver_url: url.to_owned(),
    };
    simple_api_command(
        socket,
        "PUT",
        "receive-migration",
        Some(&serde_json::to_string(&receive_migration_data).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn send_migration_api_command(
    socket: &mut UnixStream,
    url: &str,
    local: bool,
) -> Result<(), Error> {
    let send_migration_data = vmm::api::VmSendMigrationData {
        destination_url: url.to_owned(),
        local,
    };
    simple_api_command(
        socket,
        "PUT",
        "send-migration",
        Some(&serde_json::to_string(&send_migration_data).unwrap()),
    )
    .map_err(Error::ApiClient)
}

fn create_api_command(socket: &mut UnixStream, path: &str) -> Result<(), Error> {
    let mut data = String::default();
    if path == "-" {
        std::io::stdin()
            .read_to_string(&mut data)
            .map_err(Error::ReadingStdin)?;
    } else {
        data = std::fs::read_to_string(path).map_err(Error::ReadingFile)?;
    }

    simple_api_command(socket, "PUT", "create", Some(&data)).map_err(Error::ApiClient)
}

fn do_command(matches: &ArgMatches) -> Result<(), Error> {
    let mut socket = UnixStream::connect(matches.get_one::<String>("api-socket").unwrap())
        .map_err(Error::Connect)?;

    match matches.subcommand_name() {
        Some("info") => {
            simple_api_command(&mut socket, "GET", "info", None).map_err(Error::ApiClient)
        }
        Some("counters") => {
            simple_api_command(&mut socket, "GET", "counters", None).map_err(Error::ApiClient)
        }
        Some("resize") => resize_api_command(
            &mut socket,
            matches
                .subcommand_matches("resize")
                .unwrap()
                .get_one::<String>("cpus")
                .map(|x| x as &str),
            matches
                .subcommand_matches("resize")
                .unwrap()
                .get_one::<String>("memory")
                .map(|x| x as &str),
            matches
                .subcommand_matches("resize")
                .unwrap()
                .get_one::<String>("balloon")
                .map(|x| x as &str),
        ),
        Some("resize-zone") => resize_zone_api_command(
            &mut socket,
            matches
                .subcommand_matches("resize-zone")
                .unwrap()
                .get_one::<String>("id")
                .unwrap(),
            matches
                .subcommand_matches("resize-zone")
                .unwrap()
                .get_one::<String>("size")
                .unwrap(),
        ),
        Some("add-device") => add_device_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-device")
                .unwrap()
                .get_one::<String>("device_config")
                .unwrap(),
        ),
        Some("remove-device") => remove_device_api_command(
            &mut socket,
            matches
                .subcommand_matches("remove-device")
                .unwrap()
                .get_one::<String>("id")
                .unwrap(),
        ),
        Some("add-disk") => add_disk_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-disk")
                .unwrap()
                .get_one::<String>("disk_config")
                .unwrap(),
        ),
        Some("add-fs") => add_fs_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-fs")
                .unwrap()
                .get_one::<String>("fs_config")
                .unwrap(),
        ),
        Some("add-pmem") => add_pmem_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-pmem")
                .unwrap()
                .get_one::<String>("pmem_config")
                .unwrap(),
        ),
        Some("add-net") => add_net_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-net")
                .unwrap()
                .get_one::<String>("net_config")
                .unwrap(),
        ),
        Some("add-user-device") => add_user_device_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-user-device")
                .unwrap()
                .get_one::<String>("device_config")
                .unwrap(),
        ),
        Some("add-vdpa") => add_vdpa_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-vdpa")
                .unwrap()
                .get_one::<String>("vdpa_config")
                .unwrap(),
        ),
        Some("add-vsock") => add_vsock_api_command(
            &mut socket,
            matches
                .subcommand_matches("add-vsock")
                .unwrap()
                .get_one::<String>("vsock_config")
                .unwrap(),
        ),
        Some("snapshot") => {
            let sub_matches = matches.subcommand_matches("snapshot").unwrap();
            let (snapshot_config, snapshot_type, memory_vol_url) =
                parse_snapshot_command_args(sub_matches);
            snapshot_api_command(&mut socket, snapshot_config, snapshot_type, memory_vol_url)
        }
        Some("restore") => restore_api_command(
            &mut socket,
            matches
                .subcommand_matches("restore")
                .unwrap()
                .get_one::<String>("restore_config")
                .unwrap(),
        ),
        Some("pause2snapshot") => {
            let sub_matches = matches.subcommand_matches("pause2snapshot").unwrap();
            let (snapshot_config, snapshot_type, memory_vol_url) =
                parse_snapshot_command_args(sub_matches);
            pause2snapshot_api_command(&mut socket, snapshot_config, snapshot_type, memory_vol_url)
        }
        Some("resume-from-snapshot") => resume_from_snap_api_command(
            &mut socket,
            matches
                .subcommand_matches("resume-from-snapshot")
                .unwrap()
                .get_one::<String>("restore_config")
                .unwrap(),
        ),
        Some("coredump") => coredump_api_command(
            &mut socket,
            matches
                .subcommand_matches("coredump")
                .unwrap()
                .get_one::<String>("coredump_config")
                .unwrap(),
        ),
        Some("send-migration") => send_migration_api_command(
            &mut socket,
            matches
                .subcommand_matches("send-migration")
                .unwrap()
                .get_one::<String>("send_migration_config")
                .unwrap(),
            matches
                .subcommand_matches("send-migration")
                .unwrap()
                .get_flag("send_migration_local"),
        ),
        Some("receive-migration") => receive_migration_api_command(
            &mut socket,
            matches
                .subcommand_matches("receive-migration")
                .unwrap()
                .get_one::<String>("receive_migration_config")
                .unwrap(),
        ),
        Some("create") => create_api_command(
            &mut socket,
            matches
                .subcommand_matches("create")
                .unwrap()
                .get_one::<String>("path")
                .unwrap(),
        ),
        Some(c) => simple_api_command(&mut socket, "PUT", c, None).map_err(Error::ApiClient),
        None => unreachable!(),
    }
}

fn main() {
    let app = Command::new("ch-remote")
        .author(crate_authors!())
        .subcommand_required(true)
        .about("Remotely control a cloud-hypervisor VMM.")
        .arg(
            Arg::new("api-socket")
                .long("api-socket")
                .help("HTTP API socket path (UNIX domain socket).")
                .num_args(1)
                .required(true),
        )
        .subcommand(
            Command::new("add-device").about("Add VFIO device").arg(
                Arg::new("device_config")
                    .index(1)
                    .help(DeviceConfig::SYNTAX),
            ),
        )
        .subcommand(
            Command::new("add-disk")
                .about("Add block device")
                .arg(Arg::new("disk_config").index(1).help(DiskConfig::SYNTAX)),
        )
        .subcommand(
            Command::new("add-fs")
                .about("Add virtio-fs backed fs device")
                .arg(Arg::new("fs_config").index(1).help(FsConfig::SYNTAX)),
        )
        .subcommand(
            Command::new("add-pmem")
                .about("Add persistent memory device")
                .arg(Arg::new("pmem_config").index(1).help(PmemConfig::SYNTAX)),
        )
        .subcommand(
            Command::new("add-net")
                .about("Add network device")
                .arg(Arg::new("net_config").index(1).help(NetConfig::SYNTAX)),
        )
        .subcommand(
            Command::new("add-user-device")
                .about("Add userspace device")
                .arg(
                    Arg::new("device_config")
                        .index(1)
                        .help(UserDeviceConfig::SYNTAX),
                ),
        )
        .subcommand(
            Command::new("add-vdpa")
                .about("Add vDPA device")
                .arg(Arg::new("vdpa_config").index(1).help(VdpaConfig::SYNTAX)),
        )
        .subcommand(
            Command::new("add-vsock")
                .about("Add vsock device")
                .arg(Arg::new("vsock_config").index(1).help(VsockConfig::SYNTAX)),
        )
        .subcommand(
            Command::new("remove-device")
                .about("Remove VFIO device")
                .arg(Arg::new("id").index(1).help("<device_id>")),
        )
        .subcommand(Command::new("info").about("Info on the VM"))
        .subcommand(Command::new("counters").about("Counters from the VM"))
        .subcommand(Command::new("pause").about("Pause the VM"))
        .subcommand(Command::new("reboot").about("Reboot the VM"))
        .subcommand(Command::new("power-button").about("Trigger a power button in the VM"))
        .subcommand(
            Command::new("resize")
                .about("Resize the VM")
                .arg(
                    Arg::new("cpus")
                        .long("cpus")
                        .help("New vCPUs count")
                        .num_args(1),
                )
                .arg(
                    Arg::new("memory")
                        .long("memory")
                        .help("New memory size in bytes (supports K/M/G suffix)")
                        .num_args(1),
                )
                .arg(
                    Arg::new("balloon")
                        .long("balloon")
                        .help("New balloon size in bytes (supports K/M/G suffix)")
                        .num_args(1),
                ),
        )
        .subcommand(
            Command::new("resize-zone")
                .about("Resize a memory zone")
                .arg(
                    Arg::new("id")
                        .long("id")
                        .help("Memory zone identifier")
                        .num_args(1),
                )
                .arg(
                    Arg::new("size")
                        .long("size")
                        .help("New memory zone size in bytes (supports K/M/G suffix)")
                        .num_args(1),
                ),
        )
        .subcommand(Command::new("resume").about("Resume the VM"))
        .subcommand(Command::new("boot").about("Boot a created VM"))
        .subcommand(Command::new("delete").about("Delete a VM"))
        .subcommand(Command::new("shutdown").about("Shutdown the VM"))
        .subcommand(
        Command::new("snapshot")
                .about("Create a snapshot from VM")
                .arg(
                    Arg::new("snapshot_config")
                        .index(1)
                        .help("<destination_url>"),
                )
                .arg(
                    Arg::new("snapshot_type")
                        .long("snapshot-type")
                        .help("Snapshot type: 'full', 'incremental' (saves only CoW anonymous pages) or 'soft-dirty' (true delta of pages written since the previous soft-dirty snapshot; falls back to 'incremental' on kernels without CONFIG_MEM_SOFT_DIRTY)")
                        .num_args(1)
                        .default_value("full"),
                )
                .arg(
                    Arg::new("memory_vol_url")
                        .long("memory-vol-url")
                        .help(MEMORY_VOL_URL_HELP)
                        .num_args(1),
                ),
        )
        .subcommand(
            Command::new("restore")
                .about("Restore VM from a snapshot")
                .arg(
                    Arg::new("restore_config")
                        .index(1)
                        .help(RestoreConfig::SYNTAX),
                ),
        )
        .subcommand(
        Command::new("pause2snapshot")
                .about("Pause and create a snapshot from VM")
                .arg(
                    Arg::new("snapshot_config")
                        .index(1)
                        .help("<destination_url>"),
                )
                .arg(
                    Arg::new("snapshot_type")
                        .long("snapshot-type")
                        .help("Snapshot type: 'full', 'incremental' (saves only CoW anonymous pages) or 'soft-dirty' (true delta of pages written since the previous soft-dirty snapshot; falls back to 'incremental' on kernels without CONFIG_MEM_SOFT_DIRTY)")
                        .num_args(1)
                        .default_value("full"),
                )
                .arg(
                    Arg::new("memory_vol_url")
                        .long("memory-vol-url")
                        .help(MEMORY_VOL_URL_HELP)
                        .num_args(1),
                ),
        )
        .subcommand(
            Command::new("resume-from-snapshot")
                .about("Resume VM from a snapshot")
                .arg(
                    Arg::new("restore_config")
                        .index(1)
                        .help(RestoreConfig::SYNTAX),
                ),
        )
        .subcommand(
            Command::new("coredump")
                .about("Create a coredump from VM")
                .arg(Arg::new("coredump_config").index(1).help("<file_path>")),
        )
        .subcommand(
            Command::new("send-migration")
                .about("Initiate a VM migration")
                .arg(
                    Arg::new("send_migration_config")
                        .index(1)
                        .help("<destination_url>"),
                )
                .arg(
                    Arg::new("send_migration_local")
                        .long("local")
                        .num_args(0)
                        .action(ArgAction::SetTrue),
                ),
        )
        .subcommand(
            Command::new("receive-migration")
                .about("Receive a VM migration")
                .arg(
                    Arg::new("receive_migration_config")
                        .index(1)
                        .help("<receiver_url>"),
                ),
        )
        .subcommand(
            Command::new("create")
                .about("Create VM from a JSON configuration")
                .arg(Arg::new("path").index(1).default_value("-")),
        );

    let matches = app.get_matches();

    if let Err(e) = do_command(&matches) {
        eprintln!("Error running command: {}", e);
        process::exit(1)
    };
}
