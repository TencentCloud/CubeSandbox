// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::{
    ActivateError, ActivateResult, GuestMemoryMmap, VirtioCommon, VirtioDevice, VirtioInterrupt,
    VirtioInterruptType, DEVICE_ACKNOWLEDGE, DEVICE_DRIVER, DEVICE_DRIVER_OK, DEVICE_FEATURES_OK,
    VIRTIO_F_IOMMU_PLATFORM,
};
use anyhow::anyhow;
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    io, result,
    sync::{atomic::Ordering, Arc, Mutex},
};
use thiserror::Error;
use vhost::{
    vdpa::{VhostVdpa, VhostVdpaIovaRange},
    vhost_kern::VhostKernFeatures,
    vhost_kern::{vdpa::VhostKernVdpa, vhost_binding::VHOST_BACKEND_F_SUSPEND},
    VhostBackend, VringConfigData,
};
use virtio_queue::{Descriptor, Queue, QueueT};
use vm_device::dma_mapping::ExternalDmaMapping;
use vm_memory::{GuestAddress, GuestAddressSpace, GuestMemory, GuestMemoryAtomic};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::{AccessPlatform, Translatable};
use vmm_sys_util::eventfd::EventFd;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Failed to create vhost-vdpa: {0}")]
    CreateVhostVdpa(vhost::Error),
    #[error("Failed to map DMA range: {0}")]
    DmaMap(vhost::Error),
    #[error("Failed to unmap DMA range: {0}")]
    DmaUnmap(vhost::Error),
    #[error("Failed to get address range")]
    GetAddressRange,
    #[error("Failed to get the available index from the virtio queue: {0}")]
    GetAvailableIndex(virtio_queue::Error),
    #[error("Get virtio configuration size: {0}")]
    GetConfigSize(vhost::Error),
    #[error("Get virtio device identifier: {0}")]
    GetDeviceId(vhost::Error),
    #[error("Failed to get backend specific features: {0}")]
    GetBackendFeatures(vhost::Error),
    #[error("Failed to get virtio features: {0}")]
    GetFeatures(vhost::Error),
    #[error("Failed to get the IOVA range: {0}")]
    GetIovaRange(vhost::Error),
    #[error("Failed to get queue size: {0}")]
    GetVringNum(vhost::Error),
    #[error("Invalid IOVA range: {0}-{1}")]
    InvalidIovaRange(u64, u64),
    #[error("Missing VIRTIO_F_ACCESS_PLATFORM feature")]
    MissingAccessPlatformVirtioFeature,
    #[error("Failed to reset owner: {0}")]
    ResetOwner(vhost::Error),
    #[error("Failed to set backend specific features: {0}")]
    SetBackendFeatures(vhost::Error),
    #[error("Failed to set backend configuration: {0}")]
    SetConfig(vhost::Error),
    #[error("Failed to set eventfd notifying about a configuration change: {0}")]
    SetConfigCall(vhost::Error),
    #[error("Failed to set virtio features: {0}")]
    SetFeatures(vhost::Error),
    #[error("Failed to set memory table: {0}")]
    SetMemTable(vhost::Error),
    #[error("Failed to set owner: {0}")]
    SetOwner(vhost::Error),
    #[error("Failed to set virtio status: {0}")]
    SetStatus(vhost::Error),
    #[error("Failed to set vring address: {0}")]
    SetVringAddr(vhost::Error),
    #[error("Failed to set vring base: {0}")]
    SetVringBase(vhost::Error),
    #[error("Failed to set vring eventfd when buffer are used: {0}")]
    SetVringCall(vhost::Error),
    #[error("Failed to enable/disable vring: {0}")]
    SetVringEnable(vhost::Error),
    #[error("Failed to set vring eventfd when new descriptors are available: {0}")]
    SetVringKick(vhost::Error),
    #[error("Failed to set vring size: {0}")]
    SetVringNum(vhost::Error),
}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Serialize, Deserialize)]
pub struct VdpaState {
    pub avail_features: u64,
    pub acked_features: u64,
    pub device_type: u32,
    pub iova_range_first: u64,
    pub iova_range_last: u64,
    pub config: Vec<u8>,
    pub queue_sizes: Vec<u16>,
    pub backend_features: u64,
}

pub struct Vdpa {
    common: VirtioCommon,
    id: String,
    vhost: Option<VhostKernVdpa<GuestMemoryAtomic<GuestMemoryMmap>>>,
    iova_range: VhostVdpaIovaRange,
    enabled_queues: BTreeMap<usize, bool>,
    backend_features: u64,
    migrating: bool,
}

impl Vdpa {
    pub fn new(
        id: String,
        device_path: &str,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        num_queues: u16,
        state: Option<VdpaState>,
    ) -> Result<Self> {
        let mut vhost = VhostKernVdpa::new(device_path, mem).map_err(Error::CreateVhostVdpa)?;
        vhost.set_owner().map_err(Error::SetOwner)?;

        let (
            device_type,
            avail_features,
            acked_features,
            queue_sizes,
            iova_range,
            backend_features,
        ) = if let Some(state) = state {
            info!("Restoring vDPA {}", id);

            vhost.set_backend_features_acked(state.backend_features);
            vhost
                .set_config(0, state.config.as_slice())
                .map_err(Error::SetConfig)?;

            (
                state.device_type,
                state.avail_features,
                state.acked_features,
                state.queue_sizes,
                VhostVdpaIovaRange {
                    first: state.iova_range_first,
                    last: state.iova_range_last,
                },
                state.backend_features,
            )
        } else {
            let device_type = vhost.get_device_id().map_err(Error::GetDeviceId)?;
            let queue_size = vhost.get_vring_num().map_err(Error::GetVringNum)?;
            let avail_features = vhost.get_features().map_err(Error::GetFeatures)?;
            let backend_features = vhost
                .get_backend_features()
                .map_err(Error::GetBackendFeatures)?;
            vhost.set_backend_features_acked(backend_features);

            let iova_range = vhost.get_iova_range().map_err(Error::GetIovaRange)?;

            if avail_features & (1u64 << VIRTIO_F_IOMMU_PLATFORM) == 0 {
                return Err(Error::MissingAccessPlatformVirtioFeature);
            }

            (
                device_type,
                avail_features,
                0,
                vec![queue_size; num_queues as usize],
                iova_range,
                backend_features,
            )
        };

        Ok(Vdpa {
            common: VirtioCommon {
                device_type,
                queue_sizes,
                avail_features,
                acked_features,
                min_queues: num_queues,
                ..Default::default()
            },
            id,
            vhost: Some(vhost),
            iova_range,
            enabled_queues: BTreeMap::new(),
            backend_features,
            migrating: false,
        })
    }

    fn enable_vrings(&mut self, enable: bool) -> Result<()> {
        assert!(self.vhost.is_some());

        for (queue_index, enabled) in self.enabled_queues.iter_mut() {
            if *enabled != enable {
                self.vhost
                    .as_ref()
                    .unwrap()
                    .set_vring_enable(*queue_index, enable)
                    .map_err(Error::SetVringEnable)?;
                *enabled = enable;
            }
        }

        Ok(())
    }

    fn activate_vdpa(
        &mut self,
        mem: &GuestMemoryMmap,
        virtio_interrupt: &Arc<dyn VirtioInterrupt>,
        queues: Vec<(usize, Queue, EventFd)>,
    ) -> Result<()> {
        assert!(self.vhost.is_some());
        self.vhost
            .as_ref()
            .unwrap()
            .set_features(self.common.acked_features)
            .map_err(Error::SetFeatures)?;
        self.vhost
            .as_mut()
            .unwrap()
            .set_backend_features(self.backend_features)
            .map_err(Error::SetBackendFeatures)?;

        for (queue_index, queue, queue_evt) in queues.iter() {
            let queue_max_size = queue.max_size();
            let queue_size = queue.size();
            self.vhost
                .as_ref()
                .unwrap()
                .set_vring_num(*queue_index, queue_size)
                .map_err(Error::SetVringNum)?;

            let config_data = VringConfigData {
                queue_max_size,
                queue_size,
                flags: 0u32,
                desc_table_addr: queue.desc_table().translate_gpa(
                    self.common.access_platform.as_ref(),
                    queue_size as usize * std::mem::size_of::<Descriptor>(),
                ),
                used_ring_addr: queue.used_ring().translate_gpa(
                    self.common.access_platform.as_ref(),
                    4 + queue_size as usize * 8,
                ),
                avail_ring_addr: queue.avail_ring().translate_gpa(
                    self.common.access_platform.as_ref(),
                    4 + queue_size as usize * 2,
                ),
                log_addr: None,
            };

            self.vhost
                .as_ref()
                .unwrap()
                .set_vring_addr(*queue_index, &config_data)
                .map_err(Error::SetVringAddr)?;
            self.vhost
                .as_ref()
                .unwrap()
                .set_vring_base(
                    *queue_index,
                    queue
                        .avail_idx(mem, Ordering::Acquire)
                        .map_err(Error::GetAvailableIndex)?
                        .0,
                )
                .map_err(Error::SetVringBase)?;

            if let Some(eventfd) =
                virtio_interrupt.notifier(VirtioInterruptType::Queue(*queue_index as u16))
            {
                self.vhost
                    .as_ref()
                    .unwrap()
                    .set_vring_call(*queue_index, &eventfd)
                    .map_err(Error::SetVringCall)?;
            }

            self.vhost
                .as_ref()
                .unwrap()
                .set_vring_kick(*queue_index, queue_evt)
                .map_err(Error::SetVringKick)?;

            self.enabled_queues.insert(*queue_index, false);
        }

        // Setup the config eventfd if there is one
        if let Some(eventfd) = virtio_interrupt.notifier(VirtioInterruptType::Config) {
            self.vhost
                .as_ref()
                .unwrap()
                .set_config_call(&eventfd)
                .map_err(Error::SetConfigCall)?;
        }

        self.enable_vrings(true)?;

        self.vhost
            .as_ref()
            .unwrap()
            .set_status(
                (DEVICE_ACKNOWLEDGE | DEVICE_DRIVER | DEVICE_DRIVER_OK | DEVICE_FEATURES_OK) as u8,
            )
            .map_err(Error::SetStatus)
    }

    fn reset_vdpa(&mut self) -> Result<()> {
        self.enable_vrings(false)?;

        assert!(self.vhost.is_some());
        self.vhost
            .as_ref()
            .unwrap()
            .set_status(0)
            .map_err(Error::SetStatus)
    }

    fn dma_map(
        &mut self,
        iova: u64,
        size: u64,
        host_vaddr: *const u8,
        readonly: bool,
    ) -> Result<()> {
        let iova_last = iova + size - 1;
        if iova < self.iova_range.first || iova_last > self.iova_range.last {
            return Err(Error::InvalidIovaRange(iova, iova_last));
        }

        assert!(self.vhost.is_some());
        self.vhost
            .as_ref()
            .unwrap()
            .dma_map(iova, size, host_vaddr, readonly)
            .map_err(Error::DmaMap)
    }

    fn dma_unmap(&self, iova: u64, size: u64) -> Result<()> {
        let iova_last = iova + size - 1;
        if iova < self.iova_range.first || iova_last > self.iova_range.last {
            return Err(Error::InvalidIovaRange(iova, iova_last));
        }

        assert!(self.vhost.is_some());
        self.vhost
            .as_ref()
            .unwrap()
            .dma_unmap(iova, size)
            .map_err(Error::DmaUnmap)
    }

    fn state(&self) -> Result<VdpaState> {
        assert!(self.vhost.is_some());
        let config_size = self
            .vhost
            .as_ref()
            .unwrap()
            .get_config_size()
            .map_err(Error::GetConfigSize)?;
        let mut config = vec![0; config_size as usize];
        self.read_config(0, config.as_mut_slice());

        Ok(VdpaState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            device_type: self.common.device_type,
            queue_sizes: self.common.queue_sizes.clone(),
            iova_range_first: self.iova_range.first,
            iova_range_last: self.iova_range.last,
            config,
            backend_features: self.backend_features,
        })
    }
}

impl VirtioDevice for Vdpa {
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
        assert!(self.vhost.is_some());
        if let Err(e) = self.vhost.as_ref().unwrap().get_config(offset as u32, data) {
            error!("Failed reading virtio config: {}", e);
        }
    }

    fn write_config(&mut self, offset: u64, data: &[u8]) {
        assert!(self.vhost.is_some());
        if let Err(e) = self.vhost.as_ref().unwrap().set_config(offset as u32, data) {
            error!("Failed writing virtio config: {}", e);
        }
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        virtio_interrupt: Arc<dyn VirtioInterrupt>,
        queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.activate_vdpa(&mem.memory(), &virtio_interrupt, queues)
            .map_err(ActivateError::ActivateVdpa)?;

        // Store the virtio interrupt handler as we need to return it on reset
        self.common.interrupt_cb = Some(virtio_interrupt);

        event!("vdpa", "activated", "id", &self.id);
        Ok(())
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        if let Err(e) = self.reset_vdpa() {
            error!("Failed to reset vhost-vdpa: {:?}", e);
            return None;
        }

        event!("vdpa", "reset", "id", &self.id);

        // Return the virtio interrupt handler
        self.common.interrupt_cb.take()
    }

    fn set_access_platform(&mut self, access_platform: Arc<dyn AccessPlatform>) {
        self.common.set_access_platform(access_platform)
    }
}

impl Pausable for Vdpa {
    fn pause(&mut self) -> std::result::Result<(), MigratableError> {
        if !self.migrating {
            Err(MigratableError::Pause(anyhow!(
                "Can't pause a vDPA device outside live migration"
            )))
        } else {
            Ok(())
        }
    }

    fn resume(&mut self) -> std::result::Result<(), MigratableError> {
        if !self.migrating {
            Err(MigratableError::Resume(anyhow!(
                "Can't resume a vDPA device outside live migration"
            )))
        } else {
            Ok(())
        }
    }
}

impl Snapshottable for Vdpa {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        if !self.migrating {
            return Err(MigratableError::Snapshot(anyhow!(
                "Can't snapshot a vDPA device outside live migration"
            )));
        }

        let snapshot = Snapshot::new_from_state(
            &self.id(),
            &self.state().map_err(|e| {
                MigratableError::Snapshot(anyhow!("Error snapshotting vDPA device: {:?}", e))
            })?,
        )?;

        // Force the vhost handler to be dropped in order to close the vDPA
        // file. This will ensure the device can be accessed if the VM is
        // migrated on the same host machine.
        self.vhost.take();

        Ok(snapshot)
    }
}

impl Transportable for Vdpa {}

impl Migratable for Vdpa {
    fn start_migration(&mut self) -> std::result::Result<(), MigratableError> {
        self.migrating = true;
        // Given there's no way to track dirty pages, we must suspend the
        // device as soon as the migration process starts.
        if self.backend_features & (1 << VHOST_BACKEND_F_SUSPEND) != 0 {
            assert!(self.vhost.is_some());
            self.vhost.as_ref().unwrap().suspend().map_err(|e| {
                MigratableError::StartMigration(anyhow!("Error suspending vDPA device: {:?}", e))
            })
        } else {
            Err(MigratableError::StartMigration(anyhow!(
                "vDPA device can't be suspended"
            )))
        }
    }

    fn complete_migration(&mut self) -> std::result::Result<(), MigratableError> {
        self.migrating = false;
        Ok(())
    }
}

pub struct VdpaDmaMapping<M: GuestAddressSpace> {
    device: Arc<Mutex<Vdpa>>,
    memory: Arc<M>,
}

impl<M: GuestAddressSpace> VdpaDmaMapping<M> {
    pub fn new(device: Arc<Mutex<Vdpa>>, memory: Arc<M>) -> Self {
        Self { device, memory }
    }
}

impl<M: GuestAddressSpace + Sync + Send> ExternalDmaMapping for VdpaDmaMapping<M> {
    fn map(&self, iova: u64, gpa: u64, size: u64) -> result::Result<(), io::Error> {
        let mem = self.memory.memory();
        let guest_addr = GuestAddress(gpa);
        let user_addr = if mem.check_range(guest_addr, size as usize) {
            mem.get_host_address(guest_addr).unwrap() as *const u8
        } else {
            return Err(io::Error::new(
                io::ErrorKind::Other,
                format!(
                    "failed to convert guest address 0x{:x} into \
                     host user virtual address",
                    gpa
                ),
            ));
        };

        debug!(
            "DMA map iova 0x{:x}, gpa 0x{:x}, size 0x{:x}, host_addr 0x{:x}",
            iova, gpa, size, user_addr as u64
        );
        self.device
            .lock()
            .unwrap()
            .dma_map(iova, size, user_addr, false)
            .map_err(|e| {
                io::Error::new(
                    io::ErrorKind::Other,
                    format!(
                        "failed to map memory for vDPA device, \
                         iova 0x{:x}, gpa 0x{:x}, size 0x{:x}: {:?}",
                        iova, gpa, size, e
                    ),
                )
            })
    }

    fn unmap(&self, iova: u64, size: u64) -> std::result::Result<(), std::io::Error> {
        debug!("DMA unmap iova 0x{:x} size 0x{:x}", iova, size);
        self.device
            .lock()
            .unwrap()
            .dma_unmap(iova, size)
            .map_err(|e| {
                io::Error::new(
                    io::ErrorKind::Other,
                    format!(
                        "failed to unmap memory for vDPA device, \
                     iova 0x{:x}, size 0x{:x}: {:?}",
                        iova, size, e
                    ),
                )
            })
    }
}
