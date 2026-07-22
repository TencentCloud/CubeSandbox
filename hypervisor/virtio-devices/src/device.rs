// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use crate::{
    ActivateError, ActivateResult, Error, GuestMemoryMmap, GuestRegionMmap,
    VIRTIO_F_RING_INDIRECT_DESC,
};
use libc::EFD_NONBLOCK;
use std::collections::HashMap;
use std::io::Write;
use std::num::Wrapping;
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Barrier,
};
use std::thread;

use anyhow::anyhow;
use virtio_queue::Queue;
use vm_memory::{GuestAddress, GuestMemoryAtomic, GuestUsize};
use vm_migration::{MigratableError, Pausable};
use vm_virtio::AccessPlatform;
use vm_virtio::VirtioDeviceType;
use vmm_sys_util::eventfd::EventFd;

pub enum VirtioInterruptType {
    Config,
    Queue(u16),
}

pub trait VirtioInterrupt: Send + Sync {
    fn trigger(&self, int_type: VirtioInterruptType) -> std::result::Result<(), std::io::Error>;
    fn notifier(&self, _int_type: VirtioInterruptType) -> Option<EventFd> {
        None
    }
}

#[derive(Clone)]
pub struct UserspaceMapping {
    pub host_addr: u64,
    pub mem_slot: u32,
    pub addr: GuestAddress,
    pub len: GuestUsize,
    pub mergeable: bool,
}

#[derive(Clone)]
pub struct VirtioSharedMemory {
    pub offset: u64,
    pub len: u64,
}

#[derive(Clone)]
pub struct VirtioSharedMemoryList {
    pub host_addr: u64,
    pub mem_slot: u32,
    pub addr: GuestAddress,
    pub len: GuestUsize,
    pub region_list: Vec<VirtioSharedMemory>,
}

/// Trait for virtio devices to be driven by a virtio transport.
///
/// The lifecycle of a virtio device is to be moved to a virtio transport, which will then query the
/// device. Once the guest driver has configured the device, `VirtioDevice::activate` will be called
/// and all the events, memory, and queues for device operation will be moved into the device.
/// Optionally, a virtio device can implement device reset in which it returns said resources and
/// resets its internal.
pub trait VirtioDevice: Send {
    /// The virtio device type.
    fn device_type(&self) -> u32;

    /// The maximum size of each queue that this device supports.
    fn queue_max_sizes(&self) -> &[u16];

    /// The set of feature bits that this device supports.
    fn features(&self) -> u64 {
        0
    }

    /// Acknowledges that this set of features should be enabled.
    fn ack_features(&mut self, value: u64) {
        let _ = value;
    }

    /// Reads this device configuration space at `offset`.
    fn read_config(&self, _offset: u64, _data: &mut [u8]) {
        warn!(
            "No readable configuration fields for {}",
            VirtioDeviceType::from(self.device_type())
        );
    }

    /// Writes to this device configuration space at `offset`.
    fn write_config(&mut self, _offset: u64, _data: &[u8]) {
        warn!(
            "No writable configuration fields for {}",
            VirtioDeviceType::from(self.device_type())
        );
    }

    /// Activates this device for real usage.
    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_evt: Arc<dyn VirtioInterrupt>,
        queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult;

    /// Optionally deactivates this device and returns ownership of the guest memory map, interrupt
    /// event, and queue events.
    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        None
    }

    /// Returns the list of shared memory regions required by the device.
    fn get_shm_regions(&self) -> Option<VirtioSharedMemoryList> {
        None
    }

    /// Updates the list of shared memory regions required by the device.
    fn set_shm_regions(
        &mut self,
        _shm_regions: VirtioSharedMemoryList,
    ) -> std::result::Result<(), Error> {
        std::unimplemented!()
    }

    /// Some devices may need to do some explicit shutdown work. This method
    /// may be implemented to do this. The VMM should call shutdown() on
    /// every device as part of shutting down the VM. Acting on the device
    /// after a shutdown() can lead to unpredictable results.
    fn shutdown(&mut self) {}

    fn add_memory_region(
        &mut self,
        _region: &Arc<GuestRegionMmap>,
    ) -> std::result::Result<(), Error> {
        Ok(())
    }

    /// Returns the list of userspace mappings associated with this device.
    fn userspace_mappings(&self) -> Vec<UserspaceMapping> {
        Vec::new()
    }

    /// Return the counters that this device exposes
    fn counters(&self) -> Option<HashMap<&'static str, Wrapping<u64>>> {
        None
    }

    /// Helper to allow common implementation of read_config
    fn read_config_from_slice(&self, config: &[u8], offset: u64, mut data: &mut [u8]) {
        let config_len = config.len() as u64;
        let data_len = data.len() as u64;
        if offset + data_len > config_len {
            error!(
                "Out-of-bound access to configuration: config_len = {} offset = {:x} length = {} for {}",
                config_len,
                offset,
                data_len,
                self.device_type()
            );
            return;
        }
        if let Some(end) = offset.checked_add(data.len() as u64) {
            data.write_all(&config[offset as usize..std::cmp::min(end, config_len) as usize])
                .unwrap();
        }
    }

    /// Set the access platform trait to let the device perform address
    /// translations if needed.
    fn set_access_platform(&mut self, _access_platform: Arc<dyn AccessPlatform>) {}
}

/// Trait providing address translation the same way a physical DMA remapping
/// table would provide translation between an IOVA and a physical address.
/// The goal of this trait is to be used by virtio devices to perform the
/// address translation before they try to read from the guest physical address.
/// On the other side, the implementation itself should be provided by the code
/// emulating the IOMMU for the guest.
pub trait DmaRemapping {
    /// Provide a way to translate GVA address ranges into GPAs.
    fn translate_gva(&self, id: u32, addr: u64) -> std::result::Result<u64, std::io::Error>;
    /// Provide a way to translate GPA address ranges into GVAs.
    fn translate_gpa(&self, id: u32, addr: u64) -> std::result::Result<u64, std::io::Error>;
}

/// Structure to handle device state common to all devices
#[derive(Default)]
pub struct VirtioCommon {
    pub avail_features: u64,
    pub acked_features: u64,
    pub kill_evt: Option<EventFd>,
    pub interrupt_cb: Option<Arc<dyn VirtioInterrupt>>,
    pub pause_evt: Option<EventFd>,
    pub paused: Arc<AtomicBool>,
    pub paused_sync: Option<Arc<Barrier>>,
    pub epoll_threads: Option<Vec<thread::JoinHandle<()>>>,
    pub queue_sizes: Vec<u16>,
    pub queue_evts: Vec<(u16, EventFd)>,
    pub device_type: u32,
    pub min_queues: u16,
    pub access_platform: Option<Arc<dyn AccessPlatform>>,
}

impl VirtioCommon {
    pub fn feature_acked(&self, feature: u64) -> bool {
        self.acked_features & 1 << feature == 1 << feature
    }

    pub fn ack_features(&mut self, value: u64) {
        let mut v = value;
        // Check if the guest is ACK'ing a feature that we didn't claim to have.
        let unrequested_features = v & !self.avail_features;
        if unrequested_features != 0 {
            warn!("Received acknowledge request for unknown feature.");

            // Don't count these features as acked.
            v &= !unrequested_features;
        }
        self.acked_features |= v;
    }

    pub fn activate(
        &mut self,
        queues: &[(usize, Queue, EventFd)],
        interrupt_cb: &Arc<dyn VirtioInterrupt>,
    ) -> ActivateResult {
        if queues.len() < self.min_queues.into() {
            error!(
                "Number of enabled queues lower than min: {} vs {}",
                queues.len(),
                self.min_queues
            );
            return Err(ActivateError::BadActivate);
        }

        self.queue_evts = queues
            .iter()
            .map(|(queue_index, _, queue_evt)| {
                queue_evt
                    .try_clone()
                    .map(|evt| (*queue_index as u16, evt))
                    .map_err(|e| {
                        error!("failed cloning queue EventFd: {e}");
                        ActivateError::BadActivate
                    })
            })
            .collect::<Result<Vec<_>, _>>()?;

        let kill_evt = EventFd::new(EFD_NONBLOCK).map_err(|e| {
            error!("failed creating kill EventFd: {}", e);
            ActivateError::BadActivate
        })?;
        self.kill_evt = Some(kill_evt);

        let pause_evt = EventFd::new(EFD_NONBLOCK).map_err(|e| {
            error!("failed creating pause EventFd: {}", e);
            ActivateError::BadActivate
        })?;
        self.pause_evt = Some(pause_evt);

        // Save the interrupt EventFD as we need to return it on reset
        // but clone it to pass into the thread.
        self.interrupt_cb = Some(interrupt_cb.clone());

        Ok(())
    }

    pub fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        self.queue_evts.clear();

        // We first must resume the virtio thread if it was paused.
        if self.pause_evt.take().is_some() {
            self.resume().ok()?;
        }

        if let Some(kill_evt) = self.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }

        if let Some(mut threads) = self.epoll_threads.take() {
            for t in threads.drain(..) {
                if let Err(e) = t.join() {
                    error!("Error joining thread: {:?}", e);
                }
            }
        }

        // Return the interrupt
        Some(self.interrupt_cb.take().unwrap())
    }

    // Wait for the worker thread to finish and return
    pub fn wait_for_epoll_threads(&mut self) {
        if let Some(mut threads) = self.epoll_threads.take() {
            for t in threads.drain(..) {
                if let Err(e) = t.join() {
                    error!("Error joining thread: {:?}", e);
                }
            }
        }
    }

    pub fn dup_eventfds(&self) -> (EventFd, EventFd) {
        (
            self.kill_evt.as_ref().unwrap().try_clone().unwrap(),
            self.pause_evt.as_ref().unwrap().try_clone().unwrap(),
        )
    }

    pub fn set_access_platform(&mut self, access_platform: Arc<dyn AccessPlatform>) {
        self.access_platform = Some(access_platform);
        // Indirect descriptors feature is not supported when the device
        // requires the addresses held by the descriptors to be translated.
        self.avail_features &= !(1 << VIRTIO_F_RING_INDIRECT_DESC);
    }
}

impl Pausable for VirtioCommon {
    fn pause(&mut self) -> std::result::Result<(), MigratableError> {
        debug!(
            "Pausing virtio-{}",
            VirtioDeviceType::from(self.device_type)
        );
        self.paused.store(true, Ordering::SeqCst);
        if let Some(pause_evt) = &self.pause_evt {
            pause_evt
                .write(1)
                .map_err(|e| MigratableError::Pause(e.into()))?;

            // Wait for all threads to acknowledge the pause before going
            // any further. This is exclusively performed when pause_evt
            // eventfd is Some(), as this means the virtio device has been
            // activated. One specific case where the device can be paused
            // while it hasn't been yet activated is snapshot/restore.
            self.paused_sync.as_ref().unwrap().wait();
        }

        Ok(())
    }

    fn resume(&mut self) -> std::result::Result<(), MigratableError> {
        debug!(
            "Resuming virtio-{}",
            VirtioDeviceType::from(self.device_type)
        );
        self.paused.store(false, Ordering::SeqCst);
        if let Some(epoll_threads) = &self.epoll_threads {
            for t in epoll_threads.iter() {
                t.thread().unpark();
            }
        }

        // Signal each activated queue eventfd so workers process restored queues
        // that may already contain pending requests.
        for (_, queue_evt) in &self.queue_evts {
            queue_evt.write(1).map_err(|e| {
                MigratableError::Resume(anyhow!(
                    "Could not notify restored virtio worker on resume: {e}"
                ))
            })?;
        }

        // Also trigger interrupts into the guest to wake up the driver to avoid a "livelock"
        // if a used-ring completion interrupt was lost across restore.
        //
        // Use the real virtio queue index rather than the position in
        // `queue_evts`. `VirtioInterruptType::Queue(x)` is defined to carry
        // the real queue index, and `VirtioInterruptMsix` indexes
        // `queues_vectors` (whose length is `total_queues`, filled by the
        // guest driver at real-index positions) with it. When ready queues
        // are non-contiguous (e.g. virtio-net control queue at index 2N when
        // only a subset of queue pairs are enabled) the position in
        // `queue_evts` differs from the real queue index, and using the
        // position would either land on a `VIRTQ_MSI_NO_VECTOR` slot
        // (silently dropping the interrupt) or on an unrelated queue's vector.
        if let Some(interrupt_cb) = &self.interrupt_cb {
            for &(queue_index, _) in &self.queue_evts {
                interrupt_cb
                    .trigger(crate::VirtioInterruptType::Queue(queue_index))
                    .ok();
            }
        }

        Ok(())
    }
}
