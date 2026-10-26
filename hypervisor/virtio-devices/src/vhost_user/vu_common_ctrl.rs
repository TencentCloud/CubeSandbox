// Copyright 2019 Intel Corporation. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use super::{Error, Result};
use crate::vhost_user::Inflight;
use crate::{
    get_host_address_range, GuestMemoryMmap, GuestRegionMmap, MmapRegion, VirtioInterrupt,
    VirtioInterruptType,
};
use std::convert::TryInto;
use std::ffi;
use std::fs::File;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::os::unix::net::UnixListener;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::thread::sleep;
use std::time::{Duration, Instant};
use std::vec::Vec;
use vhost::vhost_kern::vhost_binding::{VHOST_F_LOG_ALL, VHOST_VRING_F_LOG};
use vhost::vhost_user::message::{
    VhostUserHeaderFlag, VhostUserInflight, VhostUserProtocolFeatures, VhostUserVirtioFeatures,
};
use vhost::vhost_user::{
    Frontend, FrontendReqHandler, VhostUserFrontend, VhostUserFrontendReqHandler,
};
use vhost::{VhostBackend, VhostUserDirtyLogRegion, VhostUserMemoryRegionInfo, VringConfigData};
use virtio_queue::{Descriptor, Queue, QueueT};
use vm_memory::{
    Address, Error as MmapError, FileOffset, GuestAddress, GuestMemory, GuestMemoryRegion,
};
use vm_migration::protocol::MemoryRangeTable;
use vmm_sys_util::eventfd::EventFd;

// Size of a dirty page for vhost-user.
const VHOST_LOG_PAGE: u64 = 0x1000;

#[derive(Debug, Clone)]
pub struct VhostUserConfig {
    pub socket: String,
    pub num_queues: usize,
    pub queue_size: u16,
}

#[derive(Clone)]
struct VringInfo {
    config_data: VringConfigData,
    used_guest_addr: u64,
}

#[derive(Clone)]
pub struct VhostUserHandle {
    vu: Frontend,
    ready: bool,
    supports_migration: bool,
    shm_log: Option<Arc<MmapRegion>>,
    acked_features: u64,
    vrings_info: Option<Vec<VringInfo>>,
    queue_indexes: Vec<usize>,
}

impl VhostUserHandle {
    pub fn update_mem_table(&mut self, mem: &GuestMemoryMmap) -> Result<()> {
        let mut regions: Vec<VhostUserMemoryRegionInfo> = Vec::new();
        for region in mem.iter() {
            let (mmap_handle, mmap_offset) = match region.file_offset() {
                Some(_file_offset) => (_file_offset.file().as_raw_fd(), _file_offset.start()),
                None => return Err(Error::VhostUserMemoryRegion(MmapError::NoMemoryRegion)),
            };

            let vhost_user_net_reg = VhostUserMemoryRegionInfo {
                guest_phys_addr: region.start_addr().raw_value(),
                memory_size: region.len(),
                userspace_addr: region.as_ptr() as u64,
                mmap_offset,
                mmap_handle,
            };

            regions.push(vhost_user_net_reg);
        }

        self.vu
            .set_mem_table(regions.as_slice())
            .map_err(Error::VhostUserSetMemTable)?;

        Ok(())
    }

    pub fn add_memory_region(&mut self, region: &Arc<GuestRegionMmap>) -> Result<()> {
        let (mmap_handle, mmap_offset) = match region.file_offset() {
            Some(file_offset) => (file_offset.file().as_raw_fd(), file_offset.start()),
            None => return Err(Error::MissingRegionFd),
        };

        let region = VhostUserMemoryRegionInfo {
            guest_phys_addr: region.start_addr().raw_value(),
            memory_size: region.len(),
            userspace_addr: region.as_ptr() as u64,
            mmap_offset,
            mmap_handle,
        };

        self.vu
            .add_mem_region(&region)
            .map_err(Error::VhostUserAddMemReg)
    }

    pub fn negotiate_features_vhost_user(
        &mut self,
        avail_features: u64,
        avail_protocol_features: VhostUserProtocolFeatures,
    ) -> Result<(u64, u64)> {
        // Set vhost-user owner.
        self.vu.set_owner().map_err(Error::VhostUserSetOwner)?;

        // Get features from backend, do negotiation to get a feature collection which
        // both VMM and backend support.
        let backend_features = self
            .vu
            .get_features()
            .map_err(Error::VhostUserGetFeatures)?;
        let acked_features = avail_features & backend_features;

        let acked_protocol_features =
            if acked_features & VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits() != 0 {
                let backend_protocol_features = self
                    .vu
                    .get_protocol_features()
                    .map_err(Error::VhostUserGetProtocolFeatures)?;

                let acked_protocol_features = avail_protocol_features & backend_protocol_features;

                self.vu
                    .set_protocol_features(acked_protocol_features)
                    .map_err(Error::VhostUserSetProtocolFeatures)?;

                acked_protocol_features
            } else {
                VhostUserProtocolFeatures::empty()
            };

        if avail_protocol_features.contains(VhostUserProtocolFeatures::REPLY_ACK)
            && acked_protocol_features.contains(VhostUserProtocolFeatures::REPLY_ACK)
        {
            self.vu.set_hdr_flags(VhostUserHeaderFlag::NEED_REPLY);
        }

        self.update_supports_migration(acked_features, acked_protocol_features.bits());

        Ok((acked_features, acked_protocol_features.bits()))
    }

    #[allow(clippy::too_many_arguments)]
    pub fn setup_vhost_user<S: VhostUserFrontendReqHandler>(
        &mut self,
        mem: &GuestMemoryMmap,
        queues: Vec<(usize, Queue, EventFd)>,
        virtio_interrupt: &Arc<dyn VirtioInterrupt>,
        acked_features: u64,
        backend_req_handler: &Option<FrontendReqHandler<S>>,
        inflight: Option<&mut Inflight>,
    ) -> Result<()> {
        self.vu
            .set_features(acked_features)
            .map_err(Error::VhostUserSetFeatures)?;

        // Update internal value after it's been sent to the backend.
        self.acked_features = acked_features;

        // Let's first provide the memory table to the backend.
        self.update_mem_table(mem)?;

        // Send set_vring_num here, since it could tell backends, like SPDK,
        // how many virt queues to be handled, which backend required to know
        // at early stage.
        for (queue_index, queue, _) in queues.iter() {
            self.vu
                .set_vring_num(*queue_index, queue.size())
                .map_err(Error::VhostUserSetVringNum)?;
        }

        // Setup for inflight I/O tracking shared memory.
        if let Some(inflight) = inflight {
            if inflight.fd.is_none() {
                let inflight_req_info = VhostUserInflight {
                    mmap_size: 0,
                    mmap_offset: 0,
                    num_queues: queues.len() as u16,
                    queue_size: queues[0].1.size(),
                };
                let (info, fd) = self
                    .vu
                    .get_inflight_fd(&inflight_req_info)
                    .map_err(Error::VhostUserGetInflight)?;
                inflight.info = info;
                inflight.fd = Some(fd);
            }
            // Unwrapping the inflight fd is safe here since we know it can't be None.
            self.vu
                .set_inflight_fd(&inflight.info, inflight.fd.as_ref().unwrap().as_raw_fd())
                .map_err(Error::VhostUserSetInflight)?;
        }

        let mut vrings_info = Vec::new();
        for (queue_index, queue, queue_evt) in queues.iter() {
            let actual_size: usize = queue.size().try_into().unwrap();

            let config_data = VringConfigData {
                queue_max_size: queue.max_size(),
                queue_size: queue.size(),
                flags: 0u32,
                desc_table_addr: get_host_address_range(
                    mem,
                    GuestAddress(queue.desc_table()),
                    actual_size * std::mem::size_of::<Descriptor>(),
                )
                .ok_or(Error::DescriptorTableAddress)? as u64,
                // The used ring is {flags: u16; idx: u16; virtq_used_elem [{id: u16, len: u16}; actual_size]},
                // i.e. 4 + (4 + 4) * actual_size.
                used_ring_addr: get_host_address_range(
                    mem,
                    GuestAddress(queue.used_ring()),
                    4 + actual_size * 8,
                )
                .ok_or(Error::UsedAddress)? as u64,
                // The used ring is {flags: u16; idx: u16; elem [u16; actual_size]},
                // i.e. 4 + (2) * actual_size.
                avail_ring_addr: get_host_address_range(
                    mem,
                    GuestAddress(queue.avail_ring()),
                    4 + actual_size * 2,
                )
                .ok_or(Error::AvailAddress)? as u64,
                log_addr: None,
            };

            vrings_info.push(VringInfo {
                config_data,
                used_guest_addr: queue.used_ring(),
            });

            self.vu
                .set_vring_addr(*queue_index, &config_data)
                .map_err(Error::VhostUserSetVringAddr)?;
            self.vu
                .set_vring_base(
                    *queue_index,
                    queue
                        .avail_idx(mem, Ordering::Acquire)
                        .map_err(Error::GetAvailableIndex)?
                        .0,
                )
                .map_err(Error::VhostUserSetVringBase)?;

            if let Some(eventfd) =
                virtio_interrupt.notifier(VirtioInterruptType::Queue(*queue_index as u16))
            {
                self.vu
                    .set_vring_call(*queue_index, &eventfd)
                    .map_err(Error::VhostUserSetVringCall)?;
            }

            self.vu
                .set_vring_kick(*queue_index, queue_evt)
                .map_err(Error::VhostUserSetVringKick)?;

            self.queue_indexes.push(*queue_index);
        }

        self.enable_vhost_user_vrings(self.queue_indexes.clone(), true)?;

        if let Some(backend_req_handler) = backend_req_handler {
            self.vu
                .set_backend_request_fd(&backend_req_handler.get_tx_raw_fd())
                .map_err(Error::VhostUserSetBackendRequestFd)?;
        }

        self.vrings_info = Some(vrings_info);
        self.ready = true;

        Ok(())
    }

    fn enable_vhost_user_vrings(&mut self, queue_indexes: Vec<usize>, enable: bool) -> Result<()> {
        for queue_index in queue_indexes {
            self.vu
                .set_vring_enable(queue_index, enable)
                .map_err(Error::VhostUserSetVringEnable)?;
        }

        Ok(())
    }

    pub fn reset_vhost_user(&mut self) -> Result<()> {
        for queue_index in self.queue_indexes.drain(..) {
            self.vu
                .set_vring_enable(queue_index, false)
                .map_err(Error::VhostUserSetVringEnable)?;

            let _ = self
                .vu
                .get_vring_base(queue_index)
                .map_err(Error::VhostUserGetVringBase)?;
        }

        Ok(())
    }

    pub fn set_protocol_features_vhost_user(
        &mut self,
        acked_features: u64,
        acked_protocol_features: u64,
    ) -> Result<()> {
        self.vu.set_owner().map_err(Error::VhostUserSetOwner)?;
        self.vu
            .get_features()
            .map_err(Error::VhostUserGetFeatures)?;

        if acked_features & VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits() != 0 {
            if let Some(acked_protocol_features) =
                VhostUserProtocolFeatures::from_bits(acked_protocol_features)
            {
                self.vu
                    .set_protocol_features(acked_protocol_features)
                    .map_err(Error::VhostUserSetProtocolFeatures)?;

                if acked_protocol_features.contains(VhostUserProtocolFeatures::REPLY_ACK) {
                    self.vu.set_hdr_flags(VhostUserHeaderFlag::NEED_REPLY);
                }
            }
        }

        self.update_supports_migration(acked_features, acked_protocol_features);

        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    pub fn reinitialize_vhost_user<S: VhostUserFrontendReqHandler>(
        &mut self,
        mem: &GuestMemoryMmap,
        queues: Vec<(usize, Queue, EventFd)>,
        virtio_interrupt: &Arc<dyn VirtioInterrupt>,
        acked_features: u64,
        acked_protocol_features: u64,
        backend_req_handler: &Option<FrontendReqHandler<S>>,
        inflight: Option<&mut Inflight>,
    ) -> Result<()> {
        self.set_protocol_features_vhost_user(acked_features, acked_protocol_features)?;

        self.setup_vhost_user(
            mem,
            queues,
            virtio_interrupt,
            acked_features,
            backend_req_handler,
            inflight,
        )
    }

    pub fn connect_vhost_user(
        server: bool,
        socket_path: &str,
        num_queues: u64,
        unlink_socket: bool,
    ) -> Result<Self> {
        if server {
            if unlink_socket {
                std::fs::remove_file(socket_path).map_err(Error::RemoveSocketPath)?;
            }

            info!("Binding vhost-user listener...");
            let listener = UnixListener::bind(socket_path).map_err(Error::BindSocket)?;
            info!("Waiting for incoming vhost-user connection...");
            let (stream, _) = listener.accept().map_err(Error::AcceptConnection)?;

            Ok(VhostUserHandle {
                vu: Frontend::from_stream(stream, num_queues),
                ready: false,
                supports_migration: false,
                shm_log: None,
                acked_features: 0,
                vrings_info: None,
                queue_indexes: Vec::new(),
            })
        } else {
            let now = Instant::now();

            // Retry connecting for a full minute
            let err = loop {
                let err = match Frontend::connect(socket_path, num_queues) {
                    Ok(m) => {
                        return Ok(VhostUserHandle {
                            vu: m,
                            ready: false,
                            supports_migration: false,
                            shm_log: None,
                            acked_features: 0,
                            vrings_info: None,
                            queue_indexes: Vec::new(),
                        })
                    }
                    Err(e) => e,
                };
                sleep(Duration::from_millis(100));

                if now.elapsed().as_secs() >= 60 {
                    break err;
                }
            };

            error!(
                "Failed connecting the backend after trying for 1 minute: {:?}",
                err
            );
            Err(Error::VhostUserConnect)
        }
    }

    pub fn socket_handle(&mut self) -> &mut Frontend {
        &mut self.vu
    }

    pub fn pause_vhost_user(&mut self) -> Result<()> {
        if self.ready {
            self.enable_vhost_user_vrings(self.queue_indexes.clone(), false)?;
        }

        Ok(())
    }

    pub fn resume_vhost_user(&mut self) -> Result<()> {
        if self.ready {
            self.enable_vhost_user_vrings(self.queue_indexes.clone(), true)?;
        }

        Ok(())
    }

    fn update_supports_migration(&mut self, acked_features: u64, acked_protocol_features: u64) {
        if (acked_features & u64::from(vhost::vhost_kern::vhost_binding::VHOST_F_LOG_ALL) != 0)
            && (acked_protocol_features & VhostUserProtocolFeatures::LOG_SHMFD.bits() != 0)
        {
            self.supports_migration = true;
        }
    }

    fn update_log_base(&mut self, last_ram_addr: u64) -> Result<Option<Arc<MmapRegion>>> {
        // Create the memfd
        let fd = memfd_create(
            &ffi::CString::new("vhost_user_dirty_log").unwrap(),
            libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
        )
        .map_err(Error::MemfdCreate)?;

        // Safe because we checked the file descriptor is valid
        let file = unsafe { File::from_raw_fd(fd) };
        // The size of the memory mapping corresponds to the size of a bitmap
        // covering all guest pages for addresses from 0 to the last physical
        // address in guest RAM.
        // A page is always 4kiB from a vhost-user perspective, and each bit is
        // a page. That's how we can compute mmap_size from the last address.
        let mmap_size = (last_ram_addr / (VHOST_LOG_PAGE * 8)) + 1;
        let mmap_handle = file.as_raw_fd();

        // Set shm_log region size
        file.set_len(mmap_size).map_err(Error::SetFileSize)?;

        // Set the seals
        let res = unsafe {
            libc::fcntl(
                file.as_raw_fd(),
                libc::F_ADD_SEALS,
                libc::F_SEAL_GROW | libc::F_SEAL_SHRINK | libc::F_SEAL_SEAL,
            )
        };
        if res < 0 {
            return Err(Error::SetSeals(std::io::Error::last_os_error()));
        }

        // Mmap shm_log region
        let region = MmapRegion::build(
            Some(FileOffset::new(file, 0)),
            mmap_size as usize,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_SHARED,
        )
        .map_err(Error::NewMmapRegion)?;

        // Make sure we hold onto the region to prevent the mapping from being
        // released.
        let old_region = self.shm_log.replace(Arc::new(region));

        // Send the shm_log fd over to the backend
        let log = VhostUserDirtyLogRegion {
            mmap_size,
            mmap_offset: 0,
            mmap_handle,
        };
        self.vu
            .set_log_base(0, Some(log))
            .map_err(Error::VhostUserSetLogBase)?;

        Ok(old_region)
    }

    fn set_vring_logging(&mut self, enable: bool) -> Result<()> {
        if let Some(vrings_info) = &self.vrings_info {
            for (i, vring_info) in vrings_info.iter().enumerate() {
                let mut config_data = vring_info.config_data;
                config_data.flags = if enable { 1 << VHOST_VRING_F_LOG } else { 0 };
                config_data.log_addr = if enable {
                    Some(vring_info.used_guest_addr)
                } else {
                    None
                };

                self.vu
                    .set_vring_addr(i, &config_data)
                    .map_err(Error::VhostUserSetVringAddr)?;
            }
        }

        Ok(())
    }

    pub fn start_dirty_log(&mut self, last_ram_addr: u64) -> Result<()> {
        if !self.supports_migration {
            return Err(Error::MigrationNotSupported);
        }

        // Set the shm log region
        self.update_log_base(last_ram_addr)?;

        // Enable VHOST_F_LOG_ALL feature
        let features = self.acked_features | (1 << VHOST_F_LOG_ALL);
        self.vu
            .set_features(features)
            .map_err(Error::VhostUserSetFeatures)?;

        // Enable dirty page logging of used ring for all queues
        self.set_vring_logging(true)
    }

    pub fn stop_dirty_log(&mut self) -> Result<()> {
        if !self.supports_migration {
            return Err(Error::MigrationNotSupported);
        }

        // Disable dirty page logging of used ring for all queues
        self.set_vring_logging(false)?;

        // Disable VHOST_F_LOG_ALL feature
        self.vu
            .set_features(self.acked_features)
            .map_err(Error::VhostUserSetFeatures)?;

        // This is important here since the log region goes out of scope,
        // invoking the Drop trait, hence unmapping the memory.
        self.shm_log = None;

        Ok(())
    }

    pub fn dirty_log(&mut self, last_ram_addr: u64) -> Result<MemoryRangeTable> {
        // The log region is updated by creating a new region that is sent to
        // the backend. This ensures the backend stops logging to the previous
        // region. The previous region is returned and processed to create the
        // bitmap representing the dirty pages.
        if let Some(region) = self.update_log_base(last_ram_addr)? {
            // Be careful with the size, as it was based on u8, meaning we must
            // divide it by 8.
            let len = region.size() / 8;
            let bitmap = unsafe {
                // Cast the pointer to u64
                let ptr = region.as_ptr() as *const u64;
                std::slice::from_raw_parts(ptr, len).to_vec()
            };
            Ok(MemoryRangeTable::from_bitmap(bitmap, 0, 4096))
        } else {
            Err(Error::MissingShmLogRegion)
        }
    }
}

fn memfd_create(name: &ffi::CStr, flags: u32) -> std::result::Result<RawFd, std::io::Error> {
    let res = unsafe { libc::syscall(libc::SYS_memfd_create, name.as_ptr(), flags) };

    if res < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(res as RawFd)
    }
}
