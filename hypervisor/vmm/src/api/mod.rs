// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

//! The internal VMM API for Cloud Hypervisor.
//!
//! This API is a synchronous, [mpsc](https://doc.rust-lang.org/std/sync/mpsc/)
//! based IPC for sending commands to the VMM thread, from other
//! Cloud Hypervisor threads. The IPC follows a command-response protocol, i.e.
//! each command will receive a response back.
//!
//! The main Cloud Hypervisor thread creates an API event file descriptor
//! to notify the VMM thread about pending API commands, together with an
//! API mpsc channel. The former is the IPC control plane, the latter is the
//! IPC data plane.
//! In order to use the IPC, a Cloud Hypervisor thread needs to have a clone
//! of both the API event file descriptor and the channel Sender. Then it must
//! go through the following steps:
//!
//! 1. The thread creates an mpsc channel for receiving the command response.
//! 2. The thread sends an ApiRequest to the Sender endpoint. The ApiRequest
//!    contains the response channel Sender, for the VMM API server to be able
//!    to send the response back.
//! 3. The thread writes to the API event file descriptor to notify the VMM
//!    API server about a pending command.
//! 4. The thread reads the response back from the VMM API server, from the
//!    response channel Receiver.
//! 5. The thread handles the response and forwards potential errors.

pub use self::http::start_http_fd_thread;
pub use self::http::start_http_path_thread;
use core::fmt;
use std::fmt::Display;

pub mod http;
pub mod service;

use crate::api::service::VMM_SERVICE;
use crate::config::RestoreConfig;
use crate::device_tree::DeviceTree;
use crate::vm::{Error as VmError, VmState};
use crate::vm_config::{
    DeviceConfig, DiskConfig, FsConfig, NetConfig, PmemConfig, UserDeviceConfig, VdpaConfig,
    VmConfig, VsockConfig,
};
use micro_http::Body;
use serde::{Deserialize, Serialize};
use service::Error as VmmServiceError;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use vm_migration::MigratableError;

pub const SNAPSHOT_VERSION: &str = "1.0.3";

/// API errors are sent back from the VMM API server through the ApiResponse.
#[derive(Debug)]
pub enum ApiError {
    /// Wrong response payload type
    ResponsePayloadType,

    /// The VM could not boot.
    VmBoot(VmError),

    /// The VM could not be created.
    VmCreate(VmError),

    /// The VM could not be deleted.
    VmDelete(VmError),

    /// The VM info is not available.
    VmInfo(VmError),

    /// The VM could not be paused.
    VmPause(VmError),

    /// The VM could not be paused or snapshot or deleted.
    VmPauseToSnapshot(VmError),

    /// The VM could not resume.
    VmResume(VmError),

    /// The VM could not be restored or resumed.
    VmResumeFromSnapshot(VmError),

    /// The VM is not booted.
    VmNotBooted,

    /// The VM is not created.
    VmNotCreated,

    /// The VM could not shutdown.
    VmShutdown(VmError),

    /// The VM could not reboot.
    VmReboot(VmError),

    /// The VM could not be snapshotted.
    VmSnapshot(VmError),

    /// The VM could not restored.
    VmRestore(VmError),

    /// The VM could not be coredumped.
    VmCoredump(VmError),

    /// The VMM could not shutdown.
    VmmShutdown(VmError),

    /// The VM could not be resized
    VmResize(VmError),

    /// The memory zone could not be resized.
    VmResizeZone(VmError),

    /// The device could not be added to the VM.
    VmAddDevice(VmError),

    /// The user device could not be added to the VM.
    VmAddUserDevice(VmError),

    /// The device could not be removed from the VM.
    VmRemoveDevice(VmError),

    /// Cannot create seccomp filter
    CreateSeccompFilter(seccompiler::Error),

    /// Cannot apply seccomp filter
    ApplySeccompFilter(seccompiler::Error),

    /// The disk could not be added to the VM.
    VmAddDisk(VmError),

    /// The fs could not be added to the VM.
    VmAddFs(VmError),

    /// The fs could not be added to the VM.
    VmSetFs(VmError),

    /// The pmem device could not be added to the VM.
    VmAddPmem(VmError),

    /// The network device could not be added to the VM.
    VmAddNet(VmError),

    /// The vDPA device could not be added to the VM.
    VmAddVdpa(VmError),

    /// The vsock device could not be added to the VM.
    VmAddVsock(VmError),

    /// Error starting migration receiver
    VmReceiveMigration(MigratableError),

    /// Error starting migration sender
    VmSendMigration(MigratableError),

    /// Error triggering power button
    VmPowerButton(VmError),
    Service(VmmServiceError),
}
pub type ApiResult<T> = std::result::Result<T, ApiError>;

impl Display for ApiError {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        use self::ApiError::*;
        match self {
            ResponsePayloadType => write!(f, "Wrong response payload type"),
            VmBoot(vm_error) => write!(f, "{}", vm_error),
            VmCreate(vm_error) => write!(f, "{}", vm_error),
            VmDelete(vm_error) => write!(f, "{}", vm_error),
            VmInfo(vm_error) => write!(f, "{}", vm_error),
            VmPause(vm_error) => write!(f, "{}", vm_error),
            VmPauseToSnapshot(vm_error) => write!(f, "{}", vm_error),
            VmResume(vm_error) => write!(f, "{}", vm_error),
            VmResumeFromSnapshot(vm_error) => write!(f, "{}", vm_error),
            VmNotBooted => write!(f, "VM is not booted"),
            VmNotCreated => write!(f, "VM is not created"),
            VmShutdown(vm_error) => write!(f, "{}", vm_error),
            VmReboot(vm_error) => write!(f, "{}", vm_error),
            VmSnapshot(vm_error) => write!(f, "{}", vm_error),
            VmRestore(vm_error) => write!(f, "{}", vm_error),
            VmCoredump(vm_error) => write!(f, "{}", vm_error),
            VmmShutdown(vm_error) => write!(f, "{}", vm_error),
            VmResize(vm_error) => write!(f, "{}", vm_error),
            VmResizeZone(vm_error) => write!(f, "{}", vm_error),
            VmAddDevice(vm_error) => write!(f, "{}", vm_error),
            VmAddUserDevice(vm_error) => write!(f, "{}", vm_error),
            VmRemoveDevice(vm_error) => write!(f, "{}", vm_error),
            CreateSeccompFilter(seccomp_error) => write!(f, "{}", seccomp_error),
            ApplySeccompFilter(seccomp_error) => write!(f, "{}", seccomp_error),
            VmAddDisk(vm_error) => write!(f, "{}", vm_error),
            VmAddFs(vm_error) => write!(f, "{}", vm_error),
            VmAddPmem(vm_error) => write!(f, "{}", vm_error),
            VmAddNet(vm_error) => write!(f, "{}", vm_error),
            VmAddVdpa(vm_error) => write!(f, "{}", vm_error),
            VmAddVsock(vm_error) => write!(f, "{}", vm_error),
            VmReceiveMigration(migratable_error) => write!(f, "{}", migratable_error),
            VmSendMigration(migratable_error) => write!(f, "{}", migratable_error),
            VmPowerButton(vm_error) => write!(f, "{}", vm_error),
            VmSetFs(vm_error) => write!(f, "{}", vm_error),
            Service(service_error) => write!(f, "{}", service_error),
        }
    }
}

#[derive(Clone, Deserialize, Serialize)]
pub struct VmInfo {
    pub config: Arc<Mutex<VmConfig>>,
    pub state: VmState,
    pub memory_actual_size: u64,
    pub device_tree: Option<Arc<Mutex<DeviceTree>>>,
}

#[derive(Clone, Deserialize, Serialize)]
pub struct VmmPingResponse {
    pub version: String,
}

#[derive(Clone, Deserialize, Serialize)]
pub struct VmWaitStartResponse {
    pub started: bool,
}

#[derive(Clone, Deserialize, Serialize, Default, Debug)]
pub struct VmResizeData {
    pub desired_vcpus: Option<u8>,
    pub desired_ram: Option<u64>,
    pub desired_balloon: Option<u64>,
}

#[derive(Clone, Deserialize, Serialize, Default, Debug)]
pub struct VmResizeZoneData {
    pub id: String,
    pub desired_ram: u64,
}

#[derive(Clone, Deserialize, Serialize, Default, Debug)]
pub struct VmRemoveDeviceData {
    pub id: String,
}

#[derive(Clone, Deserialize, Serialize, Default, Debug)]
pub struct VmSnapshotConfig {
    /// The snapshot destination URL
    pub destination_url: String,
}

#[derive(Clone, Deserialize, Serialize, Default, Debug)]
pub struct VmCoredumpData {
    /// The coredump destination file
    pub destination_url: String,
}

#[derive(Clone, Deserialize, Serialize, Default, Debug)]
pub struct VmReceiveMigrationData {
    /// URL for the reception of migration state
    pub receiver_url: String,
}

#[derive(Clone, Deserialize, Serialize, Default, Debug)]
pub struct VmSendMigrationData {
    /// URL to migrate the VM to
    pub destination_url: String,
    /// Send memory across socket without copying
    #[serde(default)]
    pub local: bool,
}

pub enum ApiResponsePayload {
    /// No data is sent on the channel.
    Empty,

    /// Virtual machine information
    VmInfo(VmInfo),

    /// Vmm ping response
    VmmPing(VmmPingResponse),

    /// Vmm wait response
    VmWaitStart(VmWaitStartResponse),

    /// Vm action response
    VmAction(Option<Vec<u8>>),
}

/// This is the response sent by the VMM API server through the mpsc channel.
pub type ApiResponse = std::result::Result<ApiResponsePayload, ApiError>;

#[allow(clippy::large_enum_variant)]
#[derive(Debug)]
pub enum ApiRequest {
    /// Create the virtual machine. This request payload is a VM configuration
    /// (VmConfig).
    /// If the VMM API server could not create the VM, it will send a VmCreate
    /// error back.
    VmCreate(Box<VmConfig>),

    /// Boot the previously created virtual machine.
    /// If the VM was not previously created, the VMM API server will send a
    /// VmBoot error back.
    VmBoot,

    /// Delete the previously created virtual machine.
    /// If the VM was not previously created, the VMM API server will send a
    /// VmDelete error back.
    /// If the VM is booted, we shut it down first.
    VmDelete,

    /// Request the VM information.
    VmInfo,

    /// Request the VMM API server status
    VmmPing,

    /// Pause a VM.
    VmPause,

    /// Pause a VM to snapshot
    VmPauseToSnapshot(Arc<VmSnapshotConfig>),

    /// Resume a VM from snapshot
    VmResumeFromSnapshot(Arc<RestoreConfig>),

    /// Resume a VM.
    VmResume,

    /// Wait vm started.
    VmWaitStart,

    /// Get counters for a VM.
    VmCounters,

    /// Shut the previously booted virtual machine down.
    /// If the VM was not previously booted or created, the VMM API server
    /// will send a VmShutdown error back.
    VmShutdown,

    /// Reboot the previously booted virtual machine.
    /// If the VM was not previously booted or created, the VMM API server
    /// will send a VmReboot error back.
    VmReboot,

    /// Shut the VMM down.
    /// This will shutdown and delete the current VM, if any, and then exit the
    /// VMM process.
    VmmShutdown,

    /// Resize the VM.
    VmResize(Arc<VmResizeData>),

    /// Resize the memory zone.
    VmResizeZone(Arc<VmResizeZoneData>),

    /// Add a device to the VM.
    VmAddDevice(Arc<DeviceConfig>),

    /// Add a user device to the VM.
    VmAddUserDevice(Arc<UserDeviceConfig>),

    /// Remove a device from the VM.
    VmRemoveDevice(Arc<VmRemoveDeviceData>),

    /// Add a disk to the VM.
    VmAddDisk(Arc<DiskConfig>),

    /// Add a fs to the VM.
    VmAddFs(Arc<FsConfig>),

    /// Add a vsock device to the VM.
    VmSetFs(Arc<FsConfig>),

    /// Add a pmem device to the VM.
    VmAddPmem(Arc<PmemConfig>),

    /// Add a network device to the VM.
    VmAddNet(Arc<NetConfig>),

    /// Add a vDPA device to the VM.
    VmAddVdpa(Arc<VdpaConfig>),

    /// Add a vsock device to the VM.
    VmAddVsock(Arc<VsockConfig>),

    /// Take a VM snapshot
    VmSnapshot(Arc<VmSnapshotConfig>),

    /// Restore from a VM snapshot
    VmRestore(Arc<RestoreConfig>),

    /// Take a VM coredump
    #[cfg(all(target_arch = "x86_64", feature = "guest_debug"))]
    VmCoredump(Arc<VmCoredumpData>),

    /// Incoming migration
    VmReceiveMigration(Arc<VmReceiveMigrationData>),

    /// Outgoing migration
    VmSendMigration(Arc<VmSendMigrationData>),

    // Trigger power button
    VmPowerButton,
}

pub fn vm_create(config: Box<VmConfig>) -> ApiResult<()> {
    // Send the VM creation request.
    VMM_SERVICE
        .lock()
        .unwrap()
        .send_request(ApiRequest::VmCreate(config))
        .map_err(ApiError::Service)??;

    Ok(())
}

/// Represents a VM related action.
/// This is mostly used to factorize code between VM routines
/// that only differ by the IPC command they send.
pub enum VmAction {
    /// Boot a VM
    Boot,

    /// Delete a VM
    Delete,

    /// Shut a VM down
    Shutdown,

    /// Reboot a VM
    Reboot,

    /// Pause a VM
    Pause,

    /// Resume a VM
    Resume,

    /// Snapshot VM
    PauseToSnapshot(Arc<VmSnapshotConfig>),

    /// Restore VM
    ResumeFromSnapshot(Arc<RestoreConfig>),

    /// Return VM counters
    Counters,

    /// Add VFIO device
    AddDevice(Arc<DeviceConfig>),

    /// Add disk
    AddDisk(Arc<DiskConfig>),

    /// Add filesystem
    AddFs(Arc<FsConfig>),

    /// Add filesystem
    SetFs(Arc<FsConfig>),

    /// Add pmem
    AddPmem(Arc<PmemConfig>),

    /// Add network
    AddNet(Arc<NetConfig>),

    /// Add vdpa
    AddVdpa(Arc<VdpaConfig>),

    /// Add vsock
    AddVsock(Arc<VsockConfig>),

    /// Add user  device
    AddUserDevice(Arc<UserDeviceConfig>),

    /// Remove VFIO device
    RemoveDevice(Arc<VmRemoveDeviceData>),

    /// Resize VM
    Resize(Arc<VmResizeData>),

    /// Resize memory zone
    ResizeZone(Arc<VmResizeZoneData>),

    /// Restore VM
    Restore(Arc<RestoreConfig>),

    /// Snapshot VM
    Snapshot(Arc<VmSnapshotConfig>),

    /// Coredump VM
    #[cfg(feature = "guest_debug")]
    Coredump(Arc<VmCoredumpData>),

    /// Incoming migration
    ReceiveMigration(Arc<VmReceiveMigrationData>),

    /// Outgoing migration
    SendMigration(Arc<VmSendMigrationData>),

    /// Power Button for clean shutdown
    PowerButton,
}

fn vm_action(action: VmAction) -> ApiResult<Option<Body>> {
    use VmAction::*;
    let request = match action {
        Boot => ApiRequest::VmBoot,
        Delete => ApiRequest::VmDelete,
        Shutdown => ApiRequest::VmShutdown,
        Reboot => ApiRequest::VmReboot,
        Pause => ApiRequest::VmPause,
        Resume => ApiRequest::VmResume,
        PauseToSnapshot(v) => ApiRequest::VmPauseToSnapshot(v),
        ResumeFromSnapshot(v) => ApiRequest::VmResumeFromSnapshot(v),
        Counters => ApiRequest::VmCounters,
        AddDevice(v) => ApiRequest::VmAddDevice(v),
        AddDisk(v) => ApiRequest::VmAddDisk(v),
        AddFs(v) => ApiRequest::VmAddFs(v),
        SetFs(v) => ApiRequest::VmSetFs(v),
        AddPmem(v) => ApiRequest::VmAddPmem(v),
        AddNet(v) => ApiRequest::VmAddNet(v),
        AddVdpa(v) => ApiRequest::VmAddVdpa(v),
        AddVsock(v) => ApiRequest::VmAddVsock(v),
        AddUserDevice(v) => ApiRequest::VmAddUserDevice(v),
        RemoveDevice(v) => ApiRequest::VmRemoveDevice(v),
        Resize(v) => ApiRequest::VmResize(v),
        ResizeZone(v) => ApiRequest::VmResizeZone(v),
        Restore(v) => ApiRequest::VmRestore(v),
        Snapshot(v) => ApiRequest::VmSnapshot(v),
        #[cfg(all(target_arch = "x86_64", feature = "guest_debug"))]
        Coredump(v) => ApiRequest::VmCoredump(v),
        ReceiveMigration(v) => ApiRequest::VmReceiveMigration(v),
        SendMigration(v) => ApiRequest::VmSendMigration(v),
        PowerButton => ApiRequest::VmPowerButton,
    };

    let body = match VMM_SERVICE
        .lock()
        .unwrap()
        .send_request(request)
        .map_err(ApiError::Service)??
    {
        ApiResponsePayload::VmAction(response) => response.map(Body::new),
        ApiResponsePayload::Empty => None,
        _ => return Err(ApiError::ResponsePayloadType),
    };

    Ok(body)
}

pub fn vm_boot() -> ApiResult<Option<Body>> {
    vm_action(VmAction::Boot)
}

pub fn vm_delete() -> ApiResult<Option<Body>> {
    vm_action(VmAction::Delete)
}

pub fn vm_shutdown() -> ApiResult<Option<Body>> {
    vm_action(VmAction::Shutdown)
}

pub fn vm_reboot() -> ApiResult<Option<Body>> {
    vm_action(VmAction::Reboot)
}

pub fn vm_pause() -> ApiResult<Option<Body>> {
    vm_action(VmAction::Pause)
}

pub fn vm_resume() -> ApiResult<Option<Body>> {
    vm_action(VmAction::Resume)
}

pub fn vm_pause2snapshot(data: Arc<VmSnapshotConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::PauseToSnapshot(data))
}

pub fn vm_resume_from_snapshot(data: Arc<RestoreConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::ResumeFromSnapshot(data))
}

pub fn vm_counters() -> ApiResult<Option<Body>> {
    vm_action(VmAction::Counters)
}

pub fn vm_power_button() -> ApiResult<Option<Body>> {
    vm_action(VmAction::PowerButton)
}

pub fn vm_receive_migration(data: Arc<VmReceiveMigrationData>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::ReceiveMigration(data))
}

pub fn vm_send_migration(data: Arc<VmSendMigrationData>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::SendMigration(data))
}

pub fn vm_snapshot(data: Arc<VmSnapshotConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::Snapshot(data))
}

pub fn vm_restore(data: Arc<RestoreConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::Restore(data))
}

#[cfg(all(target_arch = "x86_64", feature = "guest_debug"))]
pub fn vm_coredump(data: Arc<VmCoredumpData>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::Coredump(data))
}

pub fn vm_info() -> ApiResult<VmInfo> {
    // Send the VM request.
    let vm_info = VMM_SERVICE
        .lock()
        .unwrap()
        .send_request(ApiRequest::VmInfo)
        .map_err(ApiError::Service)??;

    match vm_info {
        ApiResponsePayload::VmInfo(info) => Ok(info),
        _ => Err(ApiError::ResponsePayloadType),
    }
}

pub fn vmm_ping() -> ApiResult<VmmPingResponse> {
    let vmm_pong = VMM_SERVICE
        .lock()
        .unwrap()
        .send_request(ApiRequest::VmmPing)
        .map_err(ApiError::Service)??;

    match vmm_pong {
        ApiResponsePayload::VmmPing(pong) => Ok(pong),
        _ => Err(ApiError::ResponsePayloadType),
    }
}

pub fn vmm_shutdown() -> ApiResult<()> {
    // Send the VMM shutdown request.
    VMM_SERVICE
        .lock()
        .unwrap()
        .send_request(ApiRequest::VmmShutdown)
        .map_err(ApiError::Service)??;
    Ok(())
}

pub fn vm_wait_start() -> ApiResult<VmWaitStartResponse> {
    loop {
        let vmm_wait = VMM_SERVICE
            .lock()
            .unwrap()
            .send_request(ApiRequest::VmWaitStart)
            .map_err(ApiError::Service)??;

        match vmm_wait {
            ApiResponsePayload::VmWaitStart(wait) => {
                if wait.started {
                    return Ok(wait);
                } else {
                    std::thread::sleep(Duration::from_millis(100));
                    continue;
                }
            }
            _ => return Err(ApiError::ResponsePayloadType),
        }
    }
}

pub fn vm_resize(data: Arc<VmResizeData>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::Resize(data))
}

pub fn vm_resize_zone(data: Arc<VmResizeZoneData>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::ResizeZone(data))
}

pub fn vm_add_device(data: Arc<DeviceConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddDevice(data))
}

pub fn vm_add_user_device(data: Arc<UserDeviceConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddUserDevice(data))
}

pub fn vm_remove_device(data: Arc<VmRemoveDeviceData>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::RemoveDevice(data))
}

pub fn vm_add_disk(data: Arc<DiskConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddDisk(data))
}

pub fn vm_add_fs(data: Arc<FsConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddFs(data))
}

pub fn vm_set_fs(data: Arc<FsConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::SetFs(data))
}

pub fn vm_add_pmem(data: Arc<PmemConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddPmem(data))
}

pub fn vm_add_net(data: Arc<NetConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddNet(data))
}

pub fn vm_add_vdpa(data: Arc<VdpaConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddVdpa(data))
}

pub fn vm_add_vsock(data: Arc<VsockConfig>) -> ApiResult<Option<Body>> {
    vm_action(VmAction::AddVsock(data))
}
