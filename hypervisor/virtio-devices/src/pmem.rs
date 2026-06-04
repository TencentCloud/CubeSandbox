// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use super::Error as DeviceError;
use super::{
    ActivateError, ActivateResult, EpollHelper, EpollHelperError, EpollHelperHandler,
    UserspaceMapping, VirtioCommon, VirtioDevice, VirtioDeviceType, EPOLL_HELPER_EVENT_LAST,
    VIRTIO_F_IOMMU_PLATFORM, VIRTIO_F_VERSION_1,
};
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::{GuestMemoryMmap, GuestRegionMmap};
use crate::{VirtioInterrupt, VirtioInterruptType};
use anyhow::anyhow;
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::fs::File;
use std::io;
use std::mem::size_of;
use std::os::unix::io::AsRawFd;
use std::result;
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Barrier};
use thiserror::Error;
use virtio_queue::{DescriptorChain, Queue, QueueT};
use vm_memory::{
    Address, ByteValued, Bytes, GuestAddress, GuestAddressSpace, GuestMemoryAtomic,
    GuestMemoryError, GuestMemoryLoadGuard, GuestMemoryRegion,
};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::{AccessPlatform, Translatable};
use vmm_sys_util::eventfd::EventFd;

const QUEUE_SIZE: u16 = 256;
const QUEUE_SIZES: &[u16] = &[QUEUE_SIZE];

const VIRTIO_PMEM_REQ_TYPE_FLUSH: u32 = 0;
const VIRTIO_PMEM_RESP_TYPE_OK: u32 = 0;
const VIRTIO_PMEM_RESP_TYPE_EIO: u32 = 1;

// New descriptors are pending on the virtio queue.
const QUEUE_AVAIL_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;

#[derive(Copy, Clone, Debug, Default, Serialize, Deserialize)]
#[repr(C)]
struct VirtioPmemConfig {
    start: u64,
    size: u64,
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioPmemConfig {}

#[derive(Copy, Clone, Debug, Default)]
#[repr(C)]
struct VirtioPmemReq {
    type_: u32,
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioPmemReq {}

#[derive(Copy, Clone, Debug, Default)]
#[repr(C)]
struct VirtioPmemResp {
    ret: u32,
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioPmemResp {}

#[derive(Error, Debug)]
enum Error {
    #[error("Bad guest memory addresses: {0}")]
    GuestMemory(GuestMemoryError),
    #[error("Unexpected write-only descriptor")]
    UnexpectedWriteOnlyDescriptor,
    #[error("Unexpected read-only descriptor")]
    UnexpectedReadOnlyDescriptor,
    #[error("Descriptor chain too short")]
    DescriptorChainTooShort,
    #[error("Buffer length too small")]
    BufferLengthTooSmall,
    #[error("Invalid request")]
    InvalidRequest,
    #[error("Failed adding used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
}

#[derive(Debug, PartialEq, Eq)]
enum RequestType {
    Flush,
}

struct Request {
    type_: RequestType,
    status_addr: GuestAddress,
}

impl Request {
    fn parse(
        desc_chain: &mut DescriptorChain<GuestMemoryLoadGuard<GuestMemoryMmap>>,
        access_platform: Option<&Arc<dyn AccessPlatform>>,
    ) -> result::Result<Request, Error> {
        let desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;
        // The descriptor contains the request type which MUST be readable.
        if desc.is_write_only() {
            return Err(Error::UnexpectedWriteOnlyDescriptor);
        }

        if desc.len() as usize != size_of::<VirtioPmemReq>() {
            return Err(Error::InvalidRequest);
        }

        let request: VirtioPmemReq = desc_chain
            .memory()
            .read_obj(
                desc.addr()
                    .translate_gva(access_platform, desc.len() as usize),
            )
            .map_err(Error::GuestMemory)?;

        let request_type = match request.type_ {
            VIRTIO_PMEM_REQ_TYPE_FLUSH => RequestType::Flush,
            _ => return Err(Error::InvalidRequest),
        };

        let status_desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;

        // The status MUST always be writable
        if !status_desc.is_write_only() {
            return Err(Error::UnexpectedReadOnlyDescriptor);
        }

        if (status_desc.len() as usize) < size_of::<VirtioPmemResp>() {
            return Err(Error::BufferLengthTooSmall);
        }

        Ok(Request {
            type_: request_type,
            status_addr: status_desc
                .addr()
                .translate_gva(access_platform, status_desc.len() as usize),
        })
    }
}

struct PmemEpollHandler {
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    queue: Queue,
    disk: File,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    queue_evt: EventFd,
    kill_evt: EventFd,
    pause_evt: EventFd,
    access_platform: Option<Arc<dyn AccessPlatform>>,
}

impl PmemEpollHandler {
    fn process_queue(&mut self) -> result::Result<bool, Error> {
        let mut used_descs = false;
        while let Some(mut desc_chain) = self.queue.pop_descriptor_chain(self.mem.memory()) {
            let len = match Request::parse(&mut desc_chain, self.access_platform.as_ref()) {
                Ok(ref req) if (req.type_ == RequestType::Flush) => {
                    let status_code = match self.disk.sync_all() {
                        Ok(()) => VIRTIO_PMEM_RESP_TYPE_OK,
                        Err(e) => {
                            error!("failed flushing disk image: {}", e);
                            VIRTIO_PMEM_RESP_TYPE_EIO
                        }
                    };

                    let resp = VirtioPmemResp { ret: status_code };
                    match desc_chain.memory().write_obj(resp, req.status_addr) {
                        Ok(_) => size_of::<VirtioPmemResp>() as u32,
                        Err(e) => {
                            error!("bad guest memory address: {}", e);
                            0
                        }
                    }
                }
                Ok(ref req) => {
                    // Currently, there is only one virtio-pmem request, FLUSH.
                    error!("Invalid virtio request type {:?}", req.type_);
                    0
                }
                Err(e) => {
                    error!("Failed to parse available descriptor chain: {:?}", e);
                    0
                }
            };

            self.queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), len)
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;
        }

        Ok(used_descs)
    }

    fn signal_used_queue(&self) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(0))
            .map_err(|e| {
                error!("Failed to signal used queue: {:?}", e);
                DeviceError::FailedSignalingUsedQueue(e)
            })
    }

    fn run(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
    ) -> result::Result<(), EpollHelperError> {
        let mut helper = EpollHelper::new(&self.kill_evt, &self.pause_evt)?;
        helper.add_event(self.queue_evt.as_raw_fd(), QUEUE_AVAIL_EVENT)?;
        helper.run(paused, paused_sync, self)?;

        Ok(())
    }
}

impl EpollHelperHandler for PmemEpollHandler {
    fn handle_event(
        &mut self,
        _helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;
        match ev_type {
            QUEUE_AVAIL_EVENT => {
                self.queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get queue event: {:?}", e))
                })?;

                let needs_notification = self.process_queue().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to process queue : {:?}", e))
                })?;

                if needs_notification {
                    self.signal_used_queue().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to signal used queue: {:?}",
                            e
                        ))
                    })?;
                }
            }
            _ => {
                return Err(EpollHelperError::HandleEvent(anyhow!(
                    "Unexpected event: {}",
                    ev_type
                )));
            }
        }
        Ok(())
    }
}

pub struct Pmem {
    common: VirtioCommon,
    id: String,
    disk: Option<File>,
    config: VirtioPmemConfig,
    mapping: UserspaceMapping,
    seccomp_action: SeccompAction,
    exit_evt: EventFd,

    // Hold ownership of the memory that is allocated for the device
    // which will be automatically dropped when the device is dropped.
    // The same Arc is also held by MemoryManager::device_memory so that
    // virtio backends can dereference GPAs that fall inside this PMEM
    // region.
    _region: Arc<GuestRegionMmap>,
}

#[derive(Serialize, Deserialize)]
pub struct PmemState {
    avail_features: u64,
    acked_features: u64,
    config: VirtioPmemConfig,
}

impl Pmem {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        disk: File,
        addr: GuestAddress,
        mapping: UserspaceMapping,
        _region: Arc<GuestRegionMmap>,
        iommu: bool,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        state: Option<PmemState>,
    ) -> io::Result<Pmem> {
        let (avail_features, acked_features, config) = if let Some(state) = state {
            debug!("Restoring virtio-pmem {}", id);
            (state.avail_features, state.acked_features, state.config)
        } else {
            let config = VirtioPmemConfig {
                start: addr.raw_value().to_le(),
                size: _region.len().to_le(),
            };

            let mut avail_features = 1u64 << VIRTIO_F_VERSION_1;

            if iommu {
                avail_features |= 1u64 << VIRTIO_F_IOMMU_PLATFORM;
            }
            (avail_features, 0, config)
        };

        Ok(Pmem {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Pmem as u32,
                queue_sizes: QUEUE_SIZES.to_vec(),
                paused_sync: Some(Arc::new(Barrier::new(2))),
                avail_features,
                acked_features,
                min_queues: 1,
                ..Default::default()
            },
            id,
            disk: Some(disk),
            config,
            mapping,
            seccomp_action,
            _region,
            exit_evt,
        })
    }

    fn state(&self) -> PmemState {
        PmemState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: self.config,
        }
    }

    #[cfg(fuzzing)]
    pub fn wait_for_epoll_threads(&mut self) {
        self.common.wait_for_epoll_threads();
    }
}

impl Drop for Pmem {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
    }
}

impl VirtioDevice for Pmem {
    fn device_type(&self) -> u32 {
        self.common.device_type
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &self.common.queue_sizes
    }

    fn features(&self) -> u64 {
        self.common.avail_features
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
        let (kill_evt, pause_evt) = self.common.dup_eventfds();
        if let Some(disk) = self.disk.as_ref() {
            let disk = disk.try_clone().map_err(|e| {
                error!("failed cloning pmem disk: {}", e);
                ActivateError::BadActivate
            })?;

            let (_, queue, queue_evt) = queues.remove(0);

            let mut handler = PmemEpollHandler {
                mem,
                queue,
                disk,
                interrupt_cb,
                queue_evt,
                kill_evt,
                pause_evt,
                access_platform: self.common.access_platform.clone(),
            };

            let paused = self.common.paused.clone();
            let paused_sync = self.common.paused_sync.clone();
            let mut epoll_threads = Vec::new();

            spawn_virtio_thread(
                &self.id,
                &self.seccomp_action,
                Thread::VirtioPmem,
                &mut epoll_threads,
                &self.exit_evt,
                move || handler.run(paused, paused_sync.unwrap()),
            )?;

            self.common.epoll_threads = Some(epoll_threads);

            event!("virtio-device", "activated", "id", &self.id);
            return Ok(());
        }
        Err(ActivateError::BadActivate)
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        let result = self.common.reset();
        event!("virtio-device", "reset", "id", &self.id);
        result
    }

    fn userspace_mappings(&self) -> Vec<UserspaceMapping> {
        vec![self.mapping.clone()]
    }

    fn set_access_platform(&mut self, access_platform: Arc<dyn AccessPlatform>) {
        self.common.set_access_platform(access_platform)
    }
}

impl Pausable for Pmem {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()?;

        // Flush pmem backend file data to disk for cross-machine pause-snapshot
        // scenarios, ensuring all written data is persisted before the
        // snapshot is transferred to a remote host.
        if let Some(disk) = self.disk.as_ref() {
            disk.sync_all().map_err(|e| {
                MigratableError::Pause(anyhow!("Failed to fsync pmem device {}: {}", self.id, e))
            })?;
        }

        Ok(())
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()
    }
}

impl Snapshottable for Pmem {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id, &self.state())
    }
}

impl Transportable for Pmem {}
impl Migratable for Pmem {}
