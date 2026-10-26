// Copyright 2019 Intel Corporation. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::vhost_user::vu_common_ctrl::{VhostUserConfig, VhostUserHandle};
use crate::vhost_user::{Error, Result, VhostUserCommon};
use crate::{
    ActivateResult, NetCtrlEpollHandler, VirtioCommon, VirtioDevice, VirtioDeviceType,
    VirtioInterrupt, VIRTIO_F_IOMMU_PLATFORM, VIRTIO_F_RING_EVENT_IDX, VIRTIO_F_VERSION_1,
};
use crate::{GuestMemoryMmap, GuestRegionMmap};
use net_util::{build_net_config_space, CtrlQueue, MacAddr, VirtioNetConfig};
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::result;
use std::sync::{Arc, Barrier, Mutex};
use std::thread;
use std::vec::Vec;
use vhost::vhost_user::message::{VhostUserProtocolFeatures, VhostUserVirtioFeatures};
use vhost::vhost_user::{FrontendReqHandler, VhostUserFrontend, VhostUserFrontendReqHandler};
use virtio_bindings::bindings::virtio_net::{
    VIRTIO_NET_F_CSUM, VIRTIO_NET_F_CTRL_VQ, VIRTIO_NET_F_GUEST_CSUM, VIRTIO_NET_F_GUEST_ECN,
    VIRTIO_NET_F_GUEST_TSO4, VIRTIO_NET_F_GUEST_TSO6, VIRTIO_NET_F_GUEST_UFO,
    VIRTIO_NET_F_HOST_ECN, VIRTIO_NET_F_HOST_TSO4, VIRTIO_NET_F_HOST_TSO6, VIRTIO_NET_F_HOST_UFO,
    VIRTIO_NET_F_MAC, VIRTIO_NET_F_MRG_RXBUF, VIRTIO_NET_F_MTU,
};
use virtio_bindings::bindings::virtio_ring::VIRTIO_RING_F_EVENT_IDX;
use virtio_queue::{Queue, QueueT};
use vm_memory::{ByteValued, GuestMemoryAtomic};
use vm_migration::{
    protocol::MemoryRangeTable, Migratable, MigratableError, Pausable, Snapshot, Snapshottable,
    Transportable,
};
use vmm_sys_util::eventfd::EventFd;

const DEFAULT_QUEUE_NUMBER: usize = 2;

#[derive(Serialize, Deserialize)]
pub struct State {
    pub avail_features: u64,
    pub acked_features: u64,
    pub config: VirtioNetConfig,
    pub acked_protocol_features: u64,
    pub vu_num_queues: usize,
}

struct BackendReqHandler {}
impl VhostUserFrontendReqHandler for BackendReqHandler {}

pub struct Net {
    common: VirtioCommon,
    vu_common: VhostUserCommon,
    id: String,
    config: VirtioNetConfig,
    guest_memory: Option<GuestMemoryAtomic<GuestMemoryMmap>>,
    ctrl_queue_epoll_thread: Option<thread::JoinHandle<()>>,
    epoll_thread: Option<thread::JoinHandle<()>>,
    seccomp_action: SeccompAction,
    exit_evt: EventFd,
    iommu: bool,
}

impl Net {
    /// Create a new vhost-user-net device
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        mac_addr: MacAddr,
        mtu: Option<u16>,
        vu_cfg: VhostUserConfig,
        server: bool,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        iommu: bool,
        state: Option<State>,
    ) -> Result<Net> {
        let mut num_queues = vu_cfg.num_queues;

        let mut vu =
            VhostUserHandle::connect_vhost_user(server, &vu_cfg.socket, num_queues as u64, false)?;

        let (avail_features, acked_features, acked_protocol_features, vu_num_queues, config) =
            if let Some(state) = state {
                info!("Restoring vhost-user-net {}", id);

                // The backend acknowledged features must not contain
                // VIRTIO_NET_F_MAC since we don't expect the backend
                // to handle it.
                let backend_acked_features = state.acked_features & !(1 << VIRTIO_NET_F_MAC);

                vu.set_protocol_features_vhost_user(
                    backend_acked_features,
                    state.acked_protocol_features,
                )?;

                // If the control queue feature has been negotiated, let's
                // increase the number of queues.
                if state.acked_features & (1 << VIRTIO_NET_F_CTRL_VQ) != 0 {
                    num_queues += 1;
                }

                (
                    state.avail_features,
                    state.acked_features,
                    state.acked_protocol_features,
                    state.vu_num_queues,
                    state.config,
                )
            } else {
                // Filling device and vring features VMM supports.
                let mut avail_features = 1 << VIRTIO_NET_F_CSUM
                    | 1 << VIRTIO_NET_F_GUEST_CSUM
                    | 1 << VIRTIO_NET_F_GUEST_TSO4
                    | 1 << VIRTIO_NET_F_GUEST_TSO6
                    | 1 << VIRTIO_NET_F_GUEST_ECN
                    | 1 << VIRTIO_NET_F_GUEST_UFO
                    | 1 << VIRTIO_NET_F_HOST_TSO4
                    | 1 << VIRTIO_NET_F_HOST_TSO6
                    | 1 << VIRTIO_NET_F_HOST_ECN
                    | 1 << VIRTIO_NET_F_HOST_UFO
                    | 1 << VIRTIO_NET_F_MRG_RXBUF
                    | 1 << VIRTIO_NET_F_CTRL_VQ
                    | 1 << VIRTIO_F_RING_EVENT_IDX
                    | 1 << VIRTIO_F_VERSION_1
                    | VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits();

                if mtu.is_some() {
                    avail_features |= 1u64 << VIRTIO_NET_F_MTU;
                }

                let mut config = VirtioNetConfig::default();
                build_net_config_space(&mut config, mac_addr, num_queues, mtu, &mut avail_features);

                let avail_protocol_features = VhostUserProtocolFeatures::MQ
                    | VhostUserProtocolFeatures::CONFIGURE_MEM_SLOTS
                    | VhostUserProtocolFeatures::REPLY_ACK
                    | VhostUserProtocolFeatures::INFLIGHT_SHMFD
                    | VhostUserProtocolFeatures::LOG_SHMFD;

                let (mut acked_features, acked_protocol_features) =
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
                    error!("vhost-user-net requested too many queues ({}) since the backend only supports {}\n",
                num_queues, backend_num_queues);
                    return Err(Error::BadQueueNum);
                }

                // If the control queue feature has been negotiated, let's increase
                // the number of queues.
                let vu_num_queues = num_queues;
                if acked_features & (1 << VIRTIO_NET_F_CTRL_VQ) != 0 {
                    num_queues += 1;
                }

                // Make sure the virtio feature to set the MAC address is exposed to
                // the guest, even if it hasn't been negotiated with the backend.
                acked_features |= 1 << VIRTIO_NET_F_MAC;

                (
                    acked_features,
                    // If part of the available features that have been acked,
                    // the PROTOCOL_FEATURES bit must be already set through
                    // the VIRTIO acked features as we know the guest would
                    // never ack it, thus the feature would be lost.
                    acked_features & VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits(),
                    acked_protocol_features,
                    vu_num_queues,
                    config,
                )
            };

        Ok(Net {
            id,
            common: VirtioCommon {
                device_type: VirtioDeviceType::Net as u32,
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
                server,
                ..Default::default()
            },
            config,
            guest_memory: None,
            ctrl_queue_epoll_thread: None,
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

impl Drop for Net {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            if let Err(e) = kill_evt.write(1) {
                error!("failed to kill vhost-user-net: {:?}", e);
            }
        }
    }
}

impl VirtioDevice for Net {
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

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        self.guest_memory = Some(mem.clone());

        let num_queues = queues.len();
        let event_idx = self.common.feature_acked(VIRTIO_RING_F_EVENT_IDX.into());
        if self.common.feature_acked(VIRTIO_NET_F_CTRL_VQ.into()) && num_queues % 2 != 0 {
            let ctrl_queue_index = num_queues - 1;
            let (_, mut ctrl_queue, ctrl_queue_evt) = queues.remove(ctrl_queue_index);

            ctrl_queue.set_event_idx(event_idx);

            let (kill_evt, pause_evt) = self.common.dup_eventfds();

            let mut ctrl_handler = NetCtrlEpollHandler {
                mem: mem.clone(),
                kill_evt,
                pause_evt,
                ctrl_q: CtrlQueue::new(Vec::new()),
                queue: ctrl_queue,
                queue_evt: ctrl_queue_evt,
                access_platform: None,
                interrupt_cb: interrupt_cb.clone(),
                queue_index: ctrl_queue_index as u16,
            };

            let paused = self.common.paused.clone();
            // Let's update the barrier as we need 1 for the control queue
            // thread + 1 for the common vhost-user thread + 1 for the main
            // thread signalling the pause.
            self.common.paused_sync = Some(Arc::new(Barrier::new(3)));
            let paused_sync = self.common.paused_sync.clone();

            let mut epoll_threads = Vec::new();
            spawn_virtio_thread(
                &format!("{}_ctrl", &self.id),
                &self.seccomp_action,
                Thread::VirtioVhostNetCtl,
                &mut epoll_threads,
                &self.exit_evt,
                move || ctrl_handler.run_ctrl(paused, paused_sync.unwrap()),
            )?;
            self.ctrl_queue_epoll_thread = Some(epoll_threads.remove(0));
        }

        let backend_req_handler: Option<FrontendReqHandler<BackendReqHandler>> = None;

        // The backend acknowledged features must not contain VIRTIO_NET_F_MAC
        // since we don't expect the backend to handle it.
        let backend_acked_features = self.common.acked_features & !(1 << VIRTIO_NET_F_MAC);

        // Run a dedicated thread for handling potential reconnections with
        // the backend.
        let (kill_evt, pause_evt) = self.common.dup_eventfds();

        let mut handler = self.vu_common.activate(
            mem,
            queues,
            interrupt_cb,
            backend_acked_features,
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
            Thread::VirtioVhostNet,
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
        self.vu_common.shutdown();
    }

    fn add_memory_region(
        &mut self,
        region: &Arc<GuestRegionMmap>,
    ) -> std::result::Result<(), crate::Error> {
        self.vu_common.add_memory_region(&self.guest_memory, region)
    }
}

impl Pausable for Net {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.vu_common.pause()?;
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()?;

        if let Some(epoll_thread) = &self.epoll_thread {
            epoll_thread.thread().unpark();
        }

        if let Some(ctrl_queue_epoll_thread) = &self.ctrl_queue_epoll_thread {
            ctrl_queue_epoll_thread.thread().unpark();
        }

        self.vu_common.resume()
    }
}

impl Snapshottable for Net {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        self.vu_common.snapshot(&self.id(), &self.state())
    }
}
impl Transportable for Net {}

impl Migratable for Net {
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
