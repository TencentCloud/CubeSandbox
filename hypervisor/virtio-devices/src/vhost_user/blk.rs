// Copyright 2019 Intel Corporation. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use super::super::{ActivateResult, VirtioCommon, VirtioDevice, VirtioDeviceType};
use super::vu_common_ctrl::{VhostUserConfig, VhostUserHandle};
use super::{Error, Result, DEFAULT_VIRTIO_FEATURES};
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::vhost_user::VhostUserCommon;
use crate::{GuestMemoryMmap, GuestRegionMmap};
use crate::{VirtioInterrupt, VIRTIO_F_IOMMU_PLATFORM};
use block_util::VirtioBlockConfig;
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::mem;
use std::result;
use std::sync::{Arc, Barrier, Mutex};
use std::thread;
use std::vec::Vec;
use vhost::vhost_user::message::{
    VhostUserConfigFlags, VhostUserProtocolFeatures, VhostUserVirtioFeatures,
    VHOST_USER_CONFIG_OFFSET,
};
use vhost::vhost_user::{FrontendReqHandler, VhostUserFrontend, VhostUserFrontendReqHandler};
use virtio_bindings::bindings::virtio_blk::{
    VIRTIO_BLK_F_BLK_SIZE, VIRTIO_BLK_F_CONFIG_WCE, VIRTIO_BLK_F_DISCARD, VIRTIO_BLK_F_FLUSH,
    VIRTIO_BLK_F_GEOMETRY, VIRTIO_BLK_F_MQ, VIRTIO_BLK_F_RO, VIRTIO_BLK_F_SEG_MAX,
    VIRTIO_BLK_F_SIZE_MAX, VIRTIO_BLK_F_TOPOLOGY, VIRTIO_BLK_F_WRITE_ZEROES,
};
use virtio_queue::Queue;
use vm_memory::{ByteValued, GuestMemoryAtomic};
use vm_migration::{
    protocol::MemoryRangeTable, Migratable, MigratableError, Pausable, Snapshot, Snapshottable,
    Transportable,
};
use vmm_sys_util::eventfd::EventFd;

const DEFAULT_QUEUE_NUMBER: usize = 1;

#[derive(Serialize, Deserialize)]
pub struct State {
    pub avail_features: u64,
    pub acked_features: u64,
    pub config: VirtioBlockConfig,
    pub acked_protocol_features: u64,
    pub vu_num_queues: usize,
}

struct BackendReqHandler {}
impl VhostUserFrontendReqHandler for BackendReqHandler {}

pub struct Blk {
    common: VirtioCommon,
    vu_common: VhostUserCommon,
    id: String,
    config: VirtioBlockConfig,
    guest_memory: Option<GuestMemoryAtomic<GuestMemoryMmap>>,
    epoll_thread: Option<thread::JoinHandle<()>>,
    seccomp_action: SeccompAction,
    exit_evt: EventFd,
    iommu: bool,
}

impl Blk {
    /// Create a new vhost-user-blk device
    pub fn new(
        id: String,
        vu_cfg: VhostUserConfig,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        iommu: bool,
        state: Option<State>,
    ) -> Result<Blk> {
        let num_queues = vu_cfg.num_queues;

        let mut vu =
            VhostUserHandle::connect_vhost_user(false, &vu_cfg.socket, num_queues as u64, false)?;

        let (avail_features, acked_features, acked_protocol_features, vu_num_queues, config) =
            if let Some(state) = state {
                info!("Restoring vhost-user-block {}", id);

                vu.set_protocol_features_vhost_user(
                    state.acked_features,
                    state.acked_protocol_features,
                )?;

                (
                    state.avail_features,
                    state.acked_features,
                    state.acked_protocol_features,
                    state.vu_num_queues,
                    state.config,
                )
            } else {
                // Filling device and vring features VMM supports.
                let mut avail_features = 1 << VIRTIO_BLK_F_SIZE_MAX
                    | 1 << VIRTIO_BLK_F_SEG_MAX
                    | 1 << VIRTIO_BLK_F_GEOMETRY
                    | 1 << VIRTIO_BLK_F_RO
                    | 1 << VIRTIO_BLK_F_BLK_SIZE
                    | 1 << VIRTIO_BLK_F_FLUSH
                    | 1 << VIRTIO_BLK_F_TOPOLOGY
                    | 1 << VIRTIO_BLK_F_CONFIG_WCE
                    | 1 << VIRTIO_BLK_F_DISCARD
                    | 1 << VIRTIO_BLK_F_WRITE_ZEROES
                    | DEFAULT_VIRTIO_FEATURES;

                if num_queues > 1 {
                    avail_features |= 1 << VIRTIO_BLK_F_MQ;
                }

                let avail_protocol_features = VhostUserProtocolFeatures::CONFIG
                    | VhostUserProtocolFeatures::MQ
                    | VhostUserProtocolFeatures::CONFIGURE_MEM_SLOTS
                    | VhostUserProtocolFeatures::REPLY_ACK
                    | VhostUserProtocolFeatures::INFLIGHT_SHMFD
                    | VhostUserProtocolFeatures::LOG_SHMFD;

                let (acked_features, acked_protocol_features) =
                    vu.negotiate_features_vhost_user(avail_features, avail_protocol_features)?;

                let backend_num_queues =
                    if acked_protocol_features & VhostUserProtocolFeatures::MQ.bits() != 0 {
                        vu.socket_handle()
                            .get_queue_num()
                            .map_err(Error::VhostUserGetQueueMaxNum)?
                            as usize
                    } else {
                        DEFAULT_QUEUE_NUMBER
                    };

                if num_queues > backend_num_queues {
                    error!("vhost-user-blk requested too many queues ({}) since the backend only supports {}\n",
                num_queues, backend_num_queues);
                    return Err(Error::BadQueueNum);
                }

                let config_len = mem::size_of::<VirtioBlockConfig>();
                let config_space: Vec<u8> = vec![0u8; config_len];
                let (_, config_space) = vu
                    .socket_handle()
                    .get_config(
                        VHOST_USER_CONFIG_OFFSET,
                        config_len as u32,
                        VhostUserConfigFlags::WRITABLE,
                        config_space.as_slice(),
                    )
                    .map_err(Error::VhostUserGetConfig)?;
                let mut config = VirtioBlockConfig::default();
                if let Some(backend_config) = VirtioBlockConfig::from_slice(config_space.as_slice())
                {
                    config = *backend_config;
                    config.num_queues = num_queues as u16;
                }

                (
                    acked_features,
                    // If part of the available features that have been acked,
                    // the PROTOCOL_FEATURES bit must be already set through
                    // the VIRTIO acked features as we know the guest would
                    // never ack it, thus the feature would be lost.
                    acked_features & VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits(),
                    acked_protocol_features,
                    num_queues,
                    config,
                )
            };

        Ok(Blk {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Block as u32,
                queue_sizes: vec![vu_cfg.queue_size; num_queues],
                avail_features,
                acked_features,
                paused_sync: Some(Arc::new(Barrier::new(2))),
                min_queues: DEFAULT_QUEUE_NUMBER as u16,
                ..Default::default()
            },
            vu_common: VhostUserCommon {
                vu: Some(Arc::new(Mutex::new(vu))),
                acked_protocol_features,
                socket_path: vu_cfg.socket,
                vu_num_queues,
                ..Default::default()
            },
            id,
            config,
            guest_memory: None,
            epoll_thread: None,
            seccomp_action,
            exit_evt,
            iommu,
        })
    }

    fn state(&self) -> State {
        State {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: self.config,
            acked_protocol_features: self.vu_common.acked_protocol_features,
            vu_num_queues: self.vu_common.vu_num_queues,
        }
    }
}

impl Drop for Blk {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            if let Err(e) = kill_evt.write(1) {
                error!("failed to kill vhost-user-blk: {:?}", e);
            }
        }
    }
}

impl VirtioDevice for Blk {
    fn device_type(&self) -> u32 {
        self.common.device_type
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &self.common.queue_sizes
    }

    fn features(&self) -> u64 {
        let mut features = self.common.avail_features;
        if self.iommu {
            features |= 1u64 << VIRTIO_F_IOMMU_PLATFORM;
        }
        features
    }

    fn ack_features(&mut self, value: u64) {
        self.common.ack_features(value)
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        self.read_config_from_slice(self.config.as_slice(), offset, data);
    }

    fn write_config(&mut self, offset: u64, data: &[u8]) {
        // The "writeback" field is the only mutable field
        let writeback_offset =
            (&self.config.writeback as *const _ as u64) - (&self.config as *const _ as u64);
        if offset != writeback_offset || data.len() != std::mem::size_of_val(&self.config.writeback)
        {
            error!(
                "Attempt to write to read-only field: offset {:x} length {}",
                offset,
                data.len()
            );
            return;
        }

        self.config.writeback = data[0];
        if let Some(vu) = &self.vu_common.vu {
            if let Err(e) = vu
                .lock()
                .unwrap()
                .socket_handle()
                .set_config(offset as u32, VhostUserConfigFlags::WRITABLE, data)
                .map_err(Error::VhostUserSetConfig)
            {
                error!("Failed setting vhost-user-blk configuration: {:?}", e);
            }
        }
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        self.guest_memory = Some(mem.clone());

        let backend_req_handler: Option<FrontendReqHandler<BackendReqHandler>> = None;

        // Run a dedicated thread for handling potential reconnections with
        // the backend.
        let (kill_evt, pause_evt) = self.common.dup_eventfds();

        let mut handler = self.vu_common.activate(
            mem,
            queues,
            interrupt_cb,
            self.common.acked_features,
            backend_req_handler,
            kill_evt,
            pause_evt,
        )?;

        let paused = self.common.paused.clone();
        let paused_sync = self.common.paused_sync.clone();

        let mut epoll_threads = Vec::new();

        spawn_virtio_thread(
            &self.id,
            &self.seccomp_action,
            Thread::VirtioVhostBlock,
            &mut epoll_threads,
            &self.exit_evt,
            move || handler.run(paused, paused_sync.unwrap()),
        )?;
        self.epoll_thread = Some(epoll_threads.remove(0));

        Ok(())
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        // We first must resume the virtio thread if it was paused.
        if self.common.pause_evt.take().is_some() {
            self.common.resume().ok()?;
        }

        if let Some(vu) = &self.vu_common.vu {
            if let Err(e) = vu.lock().unwrap().reset_vhost_user() {
                error!("Failed to reset vhost-user daemon: {:?}", e);
                return None;
            }
        }

        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }

        event!("virtio-device", "reset", "id", &self.id);

        // Return the interrupt
        Some(self.common.interrupt_cb.take().unwrap())
    }

    fn shutdown(&mut self) {
        self.vu_common.shutdown()
    }

    fn add_memory_region(
        &mut self,
        region: &Arc<GuestRegionMmap>,
    ) -> std::result::Result<(), crate::Error> {
        self.vu_common.add_memory_region(&self.guest_memory, region)
    }
}

impl Pausable for Blk {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.vu_common.pause()?;
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()?;

        if let Some(epoll_thread) = &self.epoll_thread {
            epoll_thread.thread().unpark();
        }

        self.vu_common.resume()
    }
}

impl Snapshottable for Blk {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        self.vu_common.snapshot(&self.id(), &self.state())
    }
}
impl Transportable for Blk {}

impl Migratable for Blk {
    fn start_dirty_log(&mut self) -> std::result::Result<(), MigratableError> {
        self.vu_common.start_dirty_log(&self.guest_memory)
    }

    fn stop_dirty_log(&mut self) -> std::result::Result<(), MigratableError> {
        self.vu_common.stop_dirty_log()
    }

    fn dirty_log(&mut self) -> std::result::Result<MemoryRangeTable, MigratableError> {
        self.vu_common.dirty_log(&self.guest_memory)
    }

    fn start_migration(&mut self) -> std::result::Result<(), MigratableError> {
        self.vu_common.start_migration()
    }

    fn complete_migration(&mut self) -> std::result::Result<(), MigratableError> {
        self.vu_common
            .complete_migration(self.common.kill_evt.take())
    }
}
