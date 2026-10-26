// Copyright 2019 Intel Corporation. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use crate::{
    ActivateError, EpollHelper, EpollHelperError, EpollHelperHandler, GuestMemoryMmap,
    GuestRegionMmap, VirtioInterrupt, EPOLL_HELPER_EVENT_LAST, VIRTIO_F_IN_ORDER,
    VIRTIO_F_NOTIFICATION_DATA, VIRTIO_F_ORDER_PLATFORM, VIRTIO_F_RING_EVENT_IDX,
    VIRTIO_F_RING_INDIRECT_DESC, VIRTIO_F_VERSION_1,
};
use anyhow::anyhow;
use serde::{Deserialize, Serialize};
use std::io;
use std::ops::Deref;
use std::os::unix::io::AsRawFd;
use std::sync::{atomic::AtomicBool, Arc, Barrier, Mutex};
use thiserror::Error;
use vhost::vhost_user::message::{
    VhostUserInflight, VhostUserProtocolFeatures, VhostUserVirtioFeatures,
};
use vhost::vhost_user::{FrontendReqHandler, VhostUserFrontendReqHandler};
use vhost::Error as VhostError;
use virtio_queue::Error as QueueError;
use virtio_queue::Queue;
use vm_memory::{
    mmap::MmapRegionError, Address, Error as MmapError, GuestAddressSpace, GuestMemory,
    GuestMemoryAtomic,
};
use vm_migration::{protocol::MemoryRangeTable, MigratableError, Snapshot};
use vmm_sys_util::eventfd::EventFd;
use vu_common_ctrl::VhostUserHandle;

pub mod blk;
pub mod fs;
pub mod net;
pub mod vu_common_ctrl;

pub use self::blk::Blk;
pub use self::fs::*;
pub use self::net::Net;
pub use self::vu_common_ctrl::VhostUserConfig;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Failed accepting connection: {0}")]
    AcceptConnection(io::Error),
    #[error("Invalid available address")]
    AvailAddress,
    #[error("Queue number  is not correct")]
    BadQueueNum,
    #[error("Failed binding vhost-user socket: {0}")]
    BindSocket(io::Error),
    #[error("Creating kill eventfd failed: {0}")]
    CreateKillEventFd(io::Error),
    #[error("Cloning kill eventfd failed: {0}")]
    CloneKillEventFd(io::Error),
    #[error("Invalid descriptor table address")]
    DescriptorTableAddress,
    #[error("Signal used queue failed: {0}")]
    FailedSignalingUsedQueue(io::Error),
    #[error("Failed to read vhost eventfd: {0}")]
    MemoryRegions(MmapError),
    #[error("Failed removing socket path: {0}")]
    RemoveSocketPath(io::Error),
    #[error("Failed to create frontend: {0}")]
    VhostUserCreateFrontend(VhostError),
    #[error("Failed to open vhost device: {0}")]
    VhostUserOpen(VhostError),
    #[error("Connection to socket failed")]
    VhostUserConnect,
    #[error("Get features failed: {0}")]
    VhostUserGetFeatures(VhostError),
    #[error("Get queue max number failed: {0}")]
    VhostUserGetQueueMaxNum(VhostError),
    #[error("Get protocol features failed: {0}")]
    VhostUserGetProtocolFeatures(VhostError),
    #[error("Get vring base failed: {0}")]
    VhostUserGetVringBase(VhostError),
    #[error("Vhost-user Backend not support vhost-user protocol")]
    VhostUserProtocolNotSupport,
    #[error("Set owner failed: {0}")]
    VhostUserSetOwner(VhostError),
    #[error("Reset owner failed: {0}")]
    VhostUserResetOwner(VhostError),
    #[error("Set features failed: {0}")]
    VhostUserSetFeatures(VhostError),
    #[error("Set protocol features failed: {0}")]
    VhostUserSetProtocolFeatures(VhostError),
    #[error("Set mem table failed: {0}")]
    VhostUserSetMemTable(VhostError),
    #[error("Set vring num failed: {0}")]
    VhostUserSetVringNum(VhostError),
    #[error("Set vring addr failed: {0}")]
    VhostUserSetVringAddr(VhostError),
    #[error("Set vring base failed: {0}")]
    VhostUserSetVringBase(VhostError),
    #[error("Set vring call failed: {0}")]
    VhostUserSetVringCall(VhostError),
    #[error("Set vring kick failed: {0}")]
    VhostUserSetVringKick(VhostError),
    #[error("Set vring enable failed: {0}")]
    VhostUserSetVringEnable(VhostError),
    #[error("Failed to create vhost eventfd: {0}")]
    VhostIrqCreate(io::Error),
    #[error("Failed to read vhost eventfd: {0}")]
    VhostIrqRead(io::Error),
    #[error("Failed to read vhost eventfd: {0}")]
    VhostUserMemoryRegion(MmapError),
    #[error("Failed to create the frontend request handler from backend: {0}")]
    FrontendReqHandlerCreation(vhost::vhost_user::Error),
    #[error("Set backend request fd failed: {0}")]
    VhostUserSetBackendRequestFd(vhost::Error),
    #[error("Add memory region failed: {0}")]
    VhostUserAddMemReg(VhostError),
    #[error("Failed getting the configuration: {0}")]
    VhostUserGetConfig(VhostError),
    #[error("Failed setting the configuration: {0}")]
    VhostUserSetConfig(VhostError),
    #[error("Failed getting inflight shm log: {0}")]
    VhostUserGetInflight(VhostError),
    #[error("Failed setting inflight shm log: {0}")]
    VhostUserSetInflight(VhostError),
    #[error("Failed setting the log base: {0}")]
    VhostUserSetLogBase(VhostError),
    #[error("Invalid used address")]
    UsedAddress,
    #[error("Invalid features provided from vhost-user backe")]
    InvalidFeatures,
    #[error("Missing file descriptor for the region")]
    MissingRegionFd,
    #[error("Missing IrqFd")]
    MissingIrqFd,
    #[error("Failed getting the available index: {0}")]
    GetAvailableIndex(QueueError),
    #[error("Migration is not supported by this vhost-user device")]
    MigrationNotSupported,
    #[error("Failed creating memfd: {0}")]
    MemfdCreate(io::Error),
    #[error("Failed truncating the file size to the expected size: {0}")]
    SetFileSize(io::Error),
    #[error("Failed to set the seals on the file: {0}")]
    SetSeals(io::Error),
    #[error("Failed creating new mmap region: {0}")]
    NewMmapRegion(MmapRegionError),
    #[error("Could not find the shm log region")]
    MissingShmLogRegion,
}
type Result<T> = std::result::Result<T, Error>;

pub const DEFAULT_VIRTIO_FEATURES: u64 = 1 << VIRTIO_F_RING_INDIRECT_DESC
    | 1 << VIRTIO_F_RING_EVENT_IDX
    | 1 << VIRTIO_F_VERSION_1
    | 1 << VIRTIO_F_IN_ORDER
    | 1 << VIRTIO_F_ORDER_PLATFORM
    | 1 << VIRTIO_F_NOTIFICATION_DATA
    | VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits();

const HUP_CONNECTION_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
const BACKEND_REQ_EVNET: u16 = EPOLL_HELPER_EVENT_LAST + 2;

#[derive(Default)]
pub struct Inflight {
    pub info: VhostUserInflight,
    pub fd: Option<std::fs::File>,
}

pub struct VhostUserEpollHandler<S: VhostUserFrontendReqHandler> {
    pub vu: Arc<Mutex<VhostUserHandle>>,
    pub mem: GuestMemoryAtomic<GuestMemoryMmap>,
    pub kill_evt: EventFd,
    pub pause_evt: EventFd,
    pub queues: Vec<(usize, Queue, EventFd)>,
    pub virtio_interrupt: Arc<dyn VirtioInterrupt>,
    pub acked_features: u64,
    pub acked_protocol_features: u64,
    pub socket_path: String,
    pub server: bool,
    pub backend_req_handler: Option<FrontendReqHandler<S>>,
    pub inflight: Option<Inflight>,
}

impl<S: VhostUserFrontendReqHandler> VhostUserEpollHandler<S> {
    pub fn run(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
    ) -> std::result::Result<(), EpollHelperError> {
        let mut helper = EpollHelper::new(&self.kill_evt, &self.pause_evt)?;
        helper.add_event_custom(
            self.vu.lock().unwrap().socket_handle().as_raw_fd(),
            HUP_CONNECTION_EVENT,
            epoll::Events::EPOLLHUP,
        )?;

        if let Some(backend_req_handler) = &self.backend_req_handler {
            helper.add_event(backend_req_handler.as_raw_fd(), BACKEND_REQ_EVNET)?;
        }

        helper.run(paused, paused_sync, self)?;

        Ok(())
    }

    fn reconnect(&mut self, helper: &mut EpollHelper) -> std::result::Result<(), EpollHelperError> {
        helper.del_event_custom(
            self.vu.lock().unwrap().socket_handle().as_raw_fd(),
            HUP_CONNECTION_EVENT,
            epoll::Events::EPOLLHUP,
        )?;

        let mut vhost_user = VhostUserHandle::connect_vhost_user(
            self.server,
            &self.socket_path,
            self.queues.len() as u64,
            true,
        )
        .map_err(|e| {
            EpollHelperError::IoError(std::io::Error::new(
                std::io::ErrorKind::Other,
                format!("failed connecting vhost-user backend {:?}", e),
            ))
        })?;

        // Initialize the backend
        vhost_user
            .reinitialize_vhost_user(
                self.mem.memory().deref(),
                self.queues
                    .iter()
                    .map(|(i, q, e)| (*i, vm_virtio::clone_queue(q), e.try_clone().unwrap()))
                    .collect(),
                &self.virtio_interrupt,
                self.acked_features,
                self.acked_protocol_features,
                &self.backend_req_handler,
                self.inflight.as_mut(),
            )
            .map_err(|e| {
                EpollHelperError::IoError(std::io::Error::new(
                    std::io::ErrorKind::Other,
                    format!("failed reconnecting vhost-user backend{:?}", e),
                ))
            })?;

        helper.add_event_custom(
            vhost_user.socket_handle().as_raw_fd(),
            HUP_CONNECTION_EVENT,
            epoll::Events::EPOLLHUP,
        )?;

        // Update vhost-user reference
        let mut vu = self.vu.lock().unwrap();
        *vu = vhost_user;

        Ok(())
    }
}

impl<S: VhostUserFrontendReqHandler> EpollHelperHandler for VhostUserEpollHandler<S> {
    fn handle_event(
        &mut self,
        helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> std::result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;
        match ev_type {
            HUP_CONNECTION_EVENT => {
                self.reconnect(helper).map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "failed to reconnect vhost-user backend: {:?}",
                        e
                    ))
                })?;
            }
            BACKEND_REQ_EVNET => {
                if let Some(backend_req_handler) = self.backend_req_handler.as_mut() {
                    backend_req_handler.handle_request().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to handle request from vhost-user backend: {:?}",
                            e
                        ))
                    })?;
                }
            }
            _ => {
                return Err(EpollHelperError::HandleEvent(anyhow!(
                    "Unknown event for vhost-user thread"
                )));
            }
        }

        Ok(())
    }
}

#[derive(Default)]
pub struct VhostUserCommon {
    pub vu: Option<Arc<Mutex<VhostUserHandle>>>,
    pub acked_protocol_features: u64,
    pub socket_path: String,
    pub vu_num_queues: usize,
    pub migration_started: bool,
    pub server: bool,
}

impl VhostUserCommon {
    #[allow(clippy::too_many_arguments)]
    pub fn activate<T: VhostUserFrontendReqHandler>(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        queues: Vec<(usize, Queue, EventFd)>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        acked_features: u64,
        backend_req_handler: Option<FrontendReqHandler<T>>,
        kill_evt: EventFd,
        pause_evt: EventFd,
    ) -> std::result::Result<VhostUserEpollHandler<T>, ActivateError> {
        let mut inflight: Option<Inflight> =
            if self.acked_protocol_features & VhostUserProtocolFeatures::INFLIGHT_SHMFD.bits() != 0
            {
                Some(Inflight::default())
            } else {
                None
            };

        if self.vu.is_none() {
            error!("Missing vhost-user handle");
            return Err(ActivateError::BadActivate);
        }
        let vu = self.vu.as_ref().unwrap();
        vu.lock()
            .unwrap()
            .setup_vhost_user(
                &mem.memory(),
                queues
                    .iter()
                    .map(|(i, q, e)| (*i, vm_virtio::clone_queue(q), e.try_clone().unwrap()))
                    .collect(),
                &interrupt_cb,
                acked_features,
                &backend_req_handler,
                inflight.as_mut(),
            )
            .map_err(ActivateError::VhostUserBlkSetup)?;

        Ok(VhostUserEpollHandler {
            vu: vu.clone(),
            mem,
            kill_evt,
            pause_evt,
            queues,
            virtio_interrupt: interrupt_cb,
            acked_features,
            acked_protocol_features: self.acked_protocol_features,
            socket_path: self.socket_path.clone(),
            server: self.server,
            backend_req_handler,
            inflight,
        })
    }

    pub fn restore_backend_connection(&mut self, acked_features: u64) -> Result<()> {
        let mut vu = VhostUserHandle::connect_vhost_user(
            self.server,
            &self.socket_path,
            self.vu_num_queues as u64,
            false,
        )?;

        vu.set_protocol_features_vhost_user(acked_features, self.acked_protocol_features)?;

        self.vu = Some(Arc::new(Mutex::new(vu)));

        Ok(())
    }

    pub fn shutdown(&mut self) {
        if let Some(vu) = &self.vu {
            let _ = unsafe { libc::close(vu.lock().unwrap().socket_handle().as_raw_fd()) };
        }

        // Remove socket path if needed
        if self.server {
            let _ = std::fs::remove_file(&self.socket_path);
        }
    }

    pub fn add_memory_region(
        &mut self,
        guest_memory: &Option<GuestMemoryAtomic<GuestMemoryMmap>>,
        region: &Arc<GuestRegionMmap>,
    ) -> std::result::Result<(), crate::Error> {
        if let Some(vu) = &self.vu {
            if self.acked_protocol_features & VhostUserProtocolFeatures::CONFIGURE_MEM_SLOTS.bits()
                != 0
            {
                return vu
                    .lock()
                    .unwrap()
                    .add_memory_region(region)
                    .map_err(crate::Error::VhostUserAddMemoryRegion);
            } else if let Some(guest_memory) = guest_memory {
                return vu
                    .lock()
                    .unwrap()
                    .update_mem_table(guest_memory.memory().deref())
                    .map_err(crate::Error::VhostUserUpdateMemory);
            }
        }
        Ok(())
    }

    pub fn pause(&mut self) -> std::result::Result<(), MigratableError> {
        if let Some(vu) = &self.vu {
            vu.lock().unwrap().pause_vhost_user().map_err(|e| {
                MigratableError::Pause(anyhow!("Error pausing vhost-user-blk backend: {:?}", e))
            })
        } else {
            Ok(())
        }
    }

    pub fn resume(&mut self) -> std::result::Result<(), MigratableError> {
        if let Some(vu) = &self.vu {
            vu.lock().unwrap().resume_vhost_user().map_err(|e| {
                MigratableError::Resume(anyhow!("Error resuming vhost-user-blk backend: {:?}", e))
            })
        } else {
            Ok(())
        }
    }

    pub fn snapshot<'a, T>(
        &mut self,
        id: &str,
        state: &T,
    ) -> std::result::Result<Snapshot, MigratableError>
    where
        T: Serialize + Deserialize<'a>,
    {
        let snapshot = Snapshot::new_from_state(id, state)?;

        if self.migration_started {
            self.shutdown();
        }

        Ok(snapshot)
    }

    pub fn start_dirty_log(
        &mut self,
        guest_memory: &Option<GuestMemoryAtomic<GuestMemoryMmap>>,
    ) -> std::result::Result<(), MigratableError> {
        if let Some(vu) = &self.vu {
            if let Some(guest_memory) = guest_memory {
                let last_ram_addr = guest_memory.memory().last_addr().raw_value();
                vu.lock()
                    .unwrap()
                    .start_dirty_log(last_ram_addr)
                    .map_err(|e| {
                        MigratableError::StartDirtyLog(anyhow!(
                            "Error starting migration for vhost-user backend: {:?}",
                            e
                        ))
                    })
            } else {
                Err(MigratableError::StartDirtyLog(anyhow!(
                    "Missing guest memory"
                )))
            }
        } else {
            Ok(())
        }
    }

    pub fn stop_dirty_log(&mut self) -> std::result::Result<(), MigratableError> {
        if let Some(vu) = &self.vu {
            vu.lock().unwrap().stop_dirty_log().map_err(|e| {
                MigratableError::StopDirtyLog(anyhow!(
                    "Error stopping migration for vhost-user backend: {:?}",
                    e
                ))
            })
        } else {
            Ok(())
        }
    }

    pub fn dirty_log(
        &mut self,
        guest_memory: &Option<GuestMemoryAtomic<GuestMemoryMmap>>,
    ) -> std::result::Result<MemoryRangeTable, MigratableError> {
        if let Some(vu) = &self.vu {
            if let Some(guest_memory) = guest_memory {
                let last_ram_addr = guest_memory.memory().last_addr().raw_value();
                vu.lock().unwrap().dirty_log(last_ram_addr).map_err(|e| {
                    MigratableError::DirtyLog(anyhow!(
                        "Error retrieving dirty ranges from vhost-user backend: {:?}",
                        e
                    ))
                })
            } else {
                Err(MigratableError::DirtyLog(anyhow!("Missing guest memory")))
            }
        } else {
            Ok(MemoryRangeTable::default())
        }
    }

    pub fn start_migration(&mut self) -> std::result::Result<(), MigratableError> {
        self.migration_started = true;
        Ok(())
    }

    pub fn complete_migration(
        &mut self,
        kill_evt: Option<EventFd>,
    ) -> std::result::Result<(), MigratableError> {
        self.migration_started = false;

        // Make sure the device thread is killed in order to prevent from
        // reconnections to the socket.
        if let Some(kill_evt) = kill_evt {
            kill_evt.write(1).map_err(|e| {
                MigratableError::CompleteMigration(anyhow!(
                    "Error killing vhost-user thread: {:?}",
                    e
                ))
            })?;
        }

        // Drop the vhost-user handler to avoid further calls to fail because
        // the connection with the backend has been closed.
        self.vu = None;

        Ok(())
    }
}
