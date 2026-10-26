// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use super::Error as DeviceError;
use super::{
    ActivateResult, EpollHelper, EpollHelperError, EpollHelperHandler, VirtioCommon, VirtioDevice,
    VirtioDeviceType, EPOLL_HELPER_EVENT_LAST, VIRTIO_F_VERSION_1,
};
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::GuestMemoryMmap;
use crate::{DmaRemapping, VirtioInterrupt, VirtioInterruptType};
use anyhow::anyhow;
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::io;
use std::mem::size_of;
use std::ops::Bound::Included;
use std::os::unix::io::AsRawFd;
use std::result;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Barrier, Mutex, RwLock};
use thiserror::Error;
use virtio_queue::{DescriptorChain, Queue, QueueT};
use vm_device::dma_mapping::ExternalDmaMapping;
use vm_memory::{
    Address, ByteValued, Bytes, GuestAddress, GuestAddressSpace, GuestMemoryAtomic,
    GuestMemoryError, GuestMemoryLoadGuard,
};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::AccessPlatform;
use vmm_sys_util::eventfd::EventFd;

/// Queues sizes
const QUEUE_SIZE: u16 = 256;
const NUM_QUEUES: usize = 2;
const QUEUE_SIZES: &[u16] = &[QUEUE_SIZE; NUM_QUEUES];

/// New descriptors are pending on the request queue.
/// "requestq" is meant to be used anytime an action is required to be
/// performed on behalf of the guest driver.
const REQUEST_Q_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
/// New descriptors are pending on the event queue.
/// "eventq" lets the device report any fault or other asynchronous event to
/// the guest driver.
const _EVENT_Q_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 2;

/// PROBE properties size.
/// This is the minimal size to provide at least one RESV_MEM property.
/// Because virtio-iommu expects one MSI reserved region, we must provide it,
/// otherwise the driver in the guest will define a predefined one between
/// 0x8000000 and 0x80FFFFF, which is only relevant for ARM architecture, but
/// will conflict with x86.
const PROBE_PROP_SIZE: u32 =
    (size_of::<VirtioIommuProbeProperty>() + size_of::<VirtioIommuProbeResvMem>()) as u32;

/// Virtio IOMMU features
#[allow(unused)]
const VIRTIO_IOMMU_F_INPUT_RANGE: u32 = 0;
#[allow(unused)]
const VIRTIO_IOMMU_F_DOMAIN_RANGE: u32 = 1;
#[allow(unused)]
const VIRTIO_IOMMU_F_MAP_UNMAP: u32 = 2;
#[allow(unused)]
const VIRTIO_IOMMU_F_BYPASS: u32 = 3;
const VIRTIO_IOMMU_F_PROBE: u32 = 4;
#[allow(unused)]
const VIRTIO_IOMMU_F_MMIO: u32 = 5;
const VIRTIO_IOMMU_F_BYPASS_CONFIG: u32 = 6;

// Support 2MiB and 4KiB page sizes.
const VIRTIO_IOMMU_PAGE_SIZE_MASK: u64 = (2 << 20) | (4 << 10);

#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
#[allow(dead_code)]
struct VirtioIommuRange32 {
    start: u32,
    end: u32,
}

#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
#[allow(dead_code)]
struct VirtioIommuRange64 {
    start: u64,
    end: u64,
}

#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
#[allow(dead_code)]
struct VirtioIommuConfig {
    page_size_mask: u64,
    input_range: VirtioIommuRange64,
    domain_range: VirtioIommuRange32,
    probe_size: u32,
    bypass: u8,
    _reserved: [u8; 7],
}

/// Virtio IOMMU request type
const VIRTIO_IOMMU_T_ATTACH: u8 = 1;
const VIRTIO_IOMMU_T_DETACH: u8 = 2;
const VIRTIO_IOMMU_T_MAP: u8 = 3;
const VIRTIO_IOMMU_T_UNMAP: u8 = 4;
const VIRTIO_IOMMU_T_PROBE: u8 = 5;

#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
struct VirtioIommuReqHead {
    type_: u8,
    _reserved: [u8; 3],
}

/// Virtio IOMMU request status
const VIRTIO_IOMMU_S_OK: u8 = 0;
#[allow(unused)]
const VIRTIO_IOMMU_S_IOERR: u8 = 1;
#[allow(unused)]
const VIRTIO_IOMMU_S_UNSUPP: u8 = 2;
#[allow(unused)]
const VIRTIO_IOMMU_S_DEVERR: u8 = 3;
#[allow(unused)]
const VIRTIO_IOMMU_S_INVAL: u8 = 4;
#[allow(unused)]
const VIRTIO_IOMMU_S_RANGE: u8 = 5;
#[allow(unused)]
const VIRTIO_IOMMU_S_NOENT: u8 = 6;
#[allow(unused)]
const VIRTIO_IOMMU_S_FAULT: u8 = 7;
#[allow(unused)]
const VIRTIO_IOMMU_S_NOMEM: u8 = 8;

#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
#[allow(dead_code)]
struct VirtioIommuReqTail {
    status: u8,
    _reserved: [u8; 3],
}

/// ATTACH request
#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
struct VirtioIommuReqAttach {
    domain: u32,
    endpoint: u32,
    flags: u32,
    _reserved: [u8; 4],
}

const VIRTIO_IOMMU_ATTACH_F_BYPASS: u32 = 1;

/// DETACH request
#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
struct VirtioIommuReqDetach {
    domain: u32,
    endpoint: u32,
    _reserved: [u8; 8],
}

/// Virtio IOMMU request MAP flags
#[allow(unused)]
const VIRTIO_IOMMU_MAP_F_READ: u32 = 1;
#[allow(unused)]
const VIRTIO_IOMMU_MAP_F_WRITE: u32 = 1 << 1;
#[allow(unused)]
const VIRTIO_IOMMU_MAP_F_MMIO: u32 = 1 << 2;
#[allow(unused)]
const VIRTIO_IOMMU_MAP_F_MASK: u32 =
    VIRTIO_IOMMU_MAP_F_READ | VIRTIO_IOMMU_MAP_F_WRITE | VIRTIO_IOMMU_MAP_F_MMIO;

/// MAP request
#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
struct VirtioIommuReqMap {
    domain: u32,
    virt_start: u64,
    virt_end: u64,
    phys_start: u64,
    _flags: u32,
}

/// UNMAP request
#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
struct VirtioIommuReqUnmap {
    domain: u32,
    virt_start: u64,
    virt_end: u64,
    _reserved: [u8; 4],
}

/// Virtio IOMMU request PROBE types
#[allow(unused)]
const VIRTIO_IOMMU_PROBE_T_NONE: u16 = 0;
const VIRTIO_IOMMU_PROBE_T_RESV_MEM: u16 = 1;
#[allow(unused)]
const VIRTIO_IOMMU_PROBE_T_MASK: u16 = 0xfff;

/// PROBE request
#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
#[allow(dead_code)]
struct VirtioIommuReqProbe {
    endpoint: u32,
    _reserved: [u64; 8],
}

#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
#[allow(dead_code)]
struct VirtioIommuProbeProperty {
    type_: u16,
    length: u16,
}

/// Virtio IOMMU request PROBE property RESV_MEM subtypes
#[allow(unused)]
const VIRTIO_IOMMU_RESV_MEM_T_RESERVED: u8 = 0;
const VIRTIO_IOMMU_RESV_MEM_T_MSI: u8 = 1;

#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
#[allow(dead_code)]
struct VirtioIommuProbeResvMem {
    subtype: u8,
    _reserved: [u8; 3],
    start: u64,
    end: u64,
}

/// Virtio IOMMU fault flags
#[allow(unused)]
const VIRTIO_IOMMU_FAULT_F_READ: u32 = 1;
#[allow(unused)]
const VIRTIO_IOMMU_FAULT_F_WRITE: u32 = 1 << 1;
#[allow(unused)]
const VIRTIO_IOMMU_FAULT_F_EXEC: u32 = 1 << 2;
#[allow(unused)]
const VIRTIO_IOMMU_FAULT_F_ADDRESS: u32 = 1 << 8;

/// Virtio IOMMU fault reasons
#[allow(unused)]
const VIRTIO_IOMMU_FAULT_R_UNKNOWN: u32 = 0;
#[allow(unused)]
const VIRTIO_IOMMU_FAULT_R_DOMAIN: u32 = 1;
#[allow(unused)]
const VIRTIO_IOMMU_FAULT_R_MAPPING: u32 = 2;

/// Fault reporting through eventq
#[allow(unused)]
#[derive(Copy, Clone, Debug, Default)]
#[repr(packed)]
struct VirtioIommuFault {
    reason: u8,
    reserved: [u8; 3],
    flags: u32,
    endpoint: u32,
    reserved2: [u8; 4],
    address: u64,
}

// SAFETY: these data structures only contain integers and have no implicit padding
unsafe impl ByteValued for VirtioIommuRange32 {}
unsafe impl ByteValued for VirtioIommuRange64 {}
unsafe impl ByteValued for VirtioIommuConfig {}
unsafe impl ByteValued for VirtioIommuReqHead {}
unsafe impl ByteValued for VirtioIommuReqTail {}
unsafe impl ByteValued for VirtioIommuReqAttach {}
unsafe impl ByteValued for VirtioIommuReqDetach {}
unsafe impl ByteValued for VirtioIommuReqMap {}
unsafe impl ByteValued for VirtioIommuReqUnmap {}
unsafe impl ByteValued for VirtioIommuReqProbe {}
unsafe impl ByteValued for VirtioIommuProbeProperty {}
unsafe impl ByteValued for VirtioIommuProbeResvMem {}
unsafe impl ByteValued for VirtioIommuFault {}

#[derive(Error, Debug)]
enum Error {
    #[error("Guest gave us bad memory addresses: {0}")]
    GuestMemory(GuestMemoryError),
    #[error("Guest gave us a write only descriptor that protocol says to read from")]
    UnexpectedWriteOnlyDescriptor,
    #[error("Guest gave us a read only descriptor that protocol says to write to")]
    UnexpectedReadOnlyDescriptor,
    #[error("Guest gave us too few descriptors in a descriptor chain")]
    DescriptorChainTooShort,
    #[error("Guest gave us a buffer that was too short to use")]
    BufferLengthTooSmall,
    #[error("Guest sent us invalid request")]
    InvalidRequest,
    #[error("Guest sent us invalid ATTACH request")]
    InvalidAttachRequest,
    #[error("Guest sent us invalid DETACH request")]
    InvalidDetachRequest,
    #[error("Guest sent us invalid MAP request")]
    InvalidMapRequest,
    #[error("Invalid to map because the domain is in bypass mode")]
    InvalidMapRequestBypassDomain,
    #[error("Invalid to map because the domain is missing")]
    InvalidMapRequestMissingDomain,
    #[error("Guest sent us invalid UNMAP request")]
    InvalidUnmapRequest,
    #[error("Invalid to unmap because the domain is in bypass mode")]
    InvalidUnmapRequestBypassDomain,
    #[error("Invalid to unmap because the domain is missing")]
    InvalidUnmapRequestMissingDomain,
    #[error("Guest sent us invalid PROBE request")]
    InvalidProbeRequest,
    #[error("Failed to performing external mapping: {0}")]
    ExternalMapping(io::Error),
    #[error("Failed to performing external unmapping: {0}")]
    ExternalUnmapping(io::Error),
    #[error("Failed adding used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
}

struct Request {}

impl Request {
    // Parse the available vring buffer. Based on the hashmap table of external
    // mappings required from various devices such as VFIO or vhost-user ones,
    // this function might update the hashmap table of external mappings per
    // domain.
    // Basically, the VMM knows about the device_id <=> mapping relationship
    // before running the VM, but at runtime, a new domain <=> mapping hashmap
    // is created based on the information provided from the guest driver for
    // virtio-iommu (giving the link device_id <=> domain).
    fn parse(
        desc_chain: &mut DescriptorChain<GuestMemoryLoadGuard<GuestMemoryMmap>>,
        mapping: &Arc<IommuMapping>,
        ext_mapping: &BTreeMap<u32, Arc<dyn ExternalDmaMapping>>,
        msi_iova_space: (u64, u64),
    ) -> result::Result<usize, Error> {
        let desc = desc_chain
            .next()
            .ok_or(Error::DescriptorChainTooShort)
            .map_err(|e| {
                error!("Missing head descriptor");
                e
            })?;

        // The descriptor contains the request type which MUST be readable.
        if desc.is_write_only() {
            return Err(Error::UnexpectedWriteOnlyDescriptor);
        }

        if (desc.len() as usize) < size_of::<VirtioIommuReqHead>() {
            return Err(Error::InvalidRequest);
        }

        let req_head: VirtioIommuReqHead = desc_chain
            .memory()
            .read_obj(desc.addr())
            .map_err(Error::GuestMemory)?;
        let req_offset = size_of::<VirtioIommuReqHead>();
        let desc_size_left = (desc.len() as usize) - req_offset;
        let req_addr = if let Some(addr) = desc.addr().checked_add(req_offset as u64) {
            addr
        } else {
            return Err(Error::InvalidRequest);
        };

        let (msi_iova_start, msi_iova_end) = msi_iova_space;

        // Create the reply
        let mut reply: Vec<u8> = Vec::new();
        let mut status = VIRTIO_IOMMU_S_OK;
        let mut hdr_len = 0;

        let result = (|| {
            match req_head.type_ {
                VIRTIO_IOMMU_T_ATTACH => {
                    if desc_size_left != size_of::<VirtioIommuReqAttach>() {
                        status = VIRTIO_IOMMU_S_INVAL;
                        return Err(Error::InvalidAttachRequest);
                    }

                    let req: VirtioIommuReqAttach = desc_chain
                        .memory()
                        .read_obj(req_addr as GuestAddress)
                        .map_err(Error::GuestMemory)?;
                    debug!("Attach request {:?}", req);

                    // Copy the value to use it as a proper reference.
                    let domain_id = req.domain;
                    let endpoint = req.endpoint;
                    let bypass =
                        (req.flags & VIRTIO_IOMMU_ATTACH_F_BYPASS) == VIRTIO_IOMMU_ATTACH_F_BYPASS;

                    // Add endpoint associated with specific domain
                    mapping
                        .endpoints
                        .write()
                        .unwrap()
                        .insert(endpoint, domain_id);

                    // Add new domain with no mapping if the entry didn't exist yet
                    let mut domains = mapping.domains.write().unwrap();
                    let domain = Domain {
                        mappings: BTreeMap::new(),
                        bypass,
                    };
                    domains.entry(domain_id).or_insert_with(|| domain);
                }
                VIRTIO_IOMMU_T_DETACH => {
                    if desc_size_left != size_of::<VirtioIommuReqDetach>() {
                        status = VIRTIO_IOMMU_S_INVAL;
                        return Err(Error::InvalidDetachRequest);
                    }

                    let req: VirtioIommuReqDetach = desc_chain
                        .memory()
                        .read_obj(req_addr as GuestAddress)
                        .map_err(Error::GuestMemory)?;
                    debug!("Detach request {:?}", req);

                    // Copy the value to use it as a proper reference.
                    let domain_id = req.domain;
                    let endpoint = req.endpoint;

                    // Remove endpoint associated with specific domain
                    mapping.endpoints.write().unwrap().remove(&endpoint);

                    // After all endpoints have been successfully detached from a
                    // domain, the domain can be removed. This means we must remove
                    // the mappings associated with this domain.
                    if mapping
                        .endpoints
                        .write()
                        .unwrap()
                        .iter()
                        .filter(|(_, &d)| d == domain_id)
                        .count()
                        == 0
                    {
                        mapping.domains.write().unwrap().remove(&domain_id);
                    }
                }
                VIRTIO_IOMMU_T_MAP => {
                    if desc_size_left != size_of::<VirtioIommuReqMap>() {
                        status = VIRTIO_IOMMU_S_INVAL;
                        return Err(Error::InvalidMapRequest);
                    }

                    let req: VirtioIommuReqMap = desc_chain
                        .memory()
                        .read_obj(req_addr as GuestAddress)
                        .map_err(Error::GuestMemory)?;
                    debug!("Map request {:?}", req);

                    // Copy the value to use it as a proper reference.
                    let domain_id = req.domain;

                    if let Some(domain) = mapping.domains.read().unwrap().get(&domain_id) {
                        if domain.bypass {
                            status = VIRTIO_IOMMU_S_INVAL;
                            return Err(Error::InvalidMapRequestBypassDomain);
                        }
                    } else {
                        status = VIRTIO_IOMMU_S_INVAL;
                        return Err(Error::InvalidMapRequestMissingDomain);
                    }

                    // Find the list of endpoints attached to the given domain.
                    let endpoints: Vec<u32> = mapping
                        .endpoints
                        .write()
                        .unwrap()
                        .iter()
                        .filter(|(_, &d)| d == domain_id)
                        .map(|(&e, _)| e)
                        .collect();

                    // Trigger external mapping if necessary.
                    for endpoint in endpoints {
                        if let Some(ext_map) = ext_mapping.get(&endpoint) {
                            let size = req.virt_end - req.virt_start + 1;
                            ext_map
                                .map(req.virt_start, req.phys_start, size)
                                .map_err(Error::ExternalMapping)?;
                        }
                    }

                    // Add new mapping associated with the domain
                    mapping
                        .domains
                        .write()
                        .unwrap()
                        .get_mut(&domain_id)
                        .unwrap()
                        .mappings
                        .insert(
                            req.virt_start,
                            Mapping {
                                gpa: req.phys_start,
                                size: req.virt_end - req.virt_start + 1,
                            },
                        );
                }
                VIRTIO_IOMMU_T_UNMAP => {
                    if desc_size_left != size_of::<VirtioIommuReqUnmap>() {
                        status = VIRTIO_IOMMU_S_INVAL;
                        return Err(Error::InvalidUnmapRequest);
                    }

                    let req: VirtioIommuReqUnmap = desc_chain
                        .memory()
                        .read_obj(req_addr as GuestAddress)
                        .map_err(Error::GuestMemory)?;
                    debug!("Unmap request {:?}", req);

                    // Copy the value to use it as a proper reference.
                    let domain_id = req.domain;
                    let virt_start = req.virt_start;

                    if let Some(domain) = mapping.domains.read().unwrap().get(&domain_id) {
                        if domain.bypass {
                            status = VIRTIO_IOMMU_S_INVAL;
                            return Err(Error::InvalidUnmapRequestBypassDomain);
                        }
                    } else {
                        status = VIRTIO_IOMMU_S_INVAL;
                        return Err(Error::InvalidUnmapRequestMissingDomain);
                    }

                    // Find the list of endpoints attached to the given domain.
                    let endpoints: Vec<u32> = mapping
                        .endpoints
                        .write()
                        .unwrap()
                        .iter()
                        .filter(|(_, &d)| d == domain_id)
                        .map(|(&e, _)| e)
                        .collect();

                    // Trigger external unmapping if necessary.
                    for endpoint in endpoints {
                        if let Some(ext_map) = ext_mapping.get(&endpoint) {
                            let size = req.virt_end - virt_start + 1;
                            ext_map
                                .unmap(virt_start, size)
                                .map_err(Error::ExternalUnmapping)?;
                        }
                    }

                    // Remove mapping associated with the domain
                    mapping
                        .domains
                        .write()
                        .unwrap()
                        .get_mut(&domain_id)
                        .unwrap()
                        .mappings
                        .remove(&virt_start);
                }
                VIRTIO_IOMMU_T_PROBE => {
                    if desc_size_left != size_of::<VirtioIommuReqProbe>() {
                        status = VIRTIO_IOMMU_S_INVAL;
                        return Err(Error::InvalidProbeRequest);
                    }

                    let req: VirtioIommuReqProbe = desc_chain
                        .memory()
                        .read_obj(req_addr as GuestAddress)
                        .map_err(Error::GuestMemory)?;
                    debug!("Probe request {:?}", req);

                    let probe_prop = VirtioIommuProbeProperty {
                        type_: VIRTIO_IOMMU_PROBE_T_RESV_MEM,
                        length: size_of::<VirtioIommuProbeResvMem>() as u16,
                    };
                    reply.extend_from_slice(probe_prop.as_slice());

                    let resv_mem = VirtioIommuProbeResvMem {
                        subtype: VIRTIO_IOMMU_RESV_MEM_T_MSI,
                        start: msi_iova_start,
                        end: msi_iova_end,
                        ..Default::default()
                    };
                    reply.extend_from_slice(resv_mem.as_slice());

                    hdr_len = PROBE_PROP_SIZE;
                }
                _ => {
                    status = VIRTIO_IOMMU_S_INVAL;
                    return Err(Error::InvalidRequest);
                }
            }
            Ok(())
        })();

        let status_desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;

        // The status MUST always be writable
        if !status_desc.is_write_only() {
            return Err(Error::UnexpectedReadOnlyDescriptor);
        }

        if status_desc.len() < hdr_len + size_of::<VirtioIommuReqTail>() as u32 {
            return Err(Error::BufferLengthTooSmall);
        }

        let tail = VirtioIommuReqTail {
            status,
            ..Default::default()
        };
        reply.extend_from_slice(tail.as_slice());

        // Make sure we return the result of the request to the guest before
        // we return a potential error internally.
        desc_chain
            .memory()
            .write_slice(reply.as_slice(), status_desc.addr())
            .map_err(Error::GuestMemory)?;

        // Return the error if the result was not Ok().
        result?;

        Ok((hdr_len as usize) + size_of::<VirtioIommuReqTail>())
    }
}

struct IommuEpollHandler {
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    request_queue: Queue,
    _event_queue: Queue,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    request_queue_evt: EventFd,
    _event_queue_evt: EventFd,
    kill_evt: EventFd,
    pause_evt: EventFd,
    mapping: Arc<IommuMapping>,
    ext_mapping: Arc<Mutex<BTreeMap<u32, Arc<dyn ExternalDmaMapping>>>>,
    msi_iova_space: (u64, u64),
}

impl IommuEpollHandler {
    fn request_queue(&mut self) -> Result<bool, Error> {
        let mut used_descs = false;
        while let Some(mut desc_chain) = self.request_queue.pop_descriptor_chain(self.mem.memory())
        {
            let len = Request::parse(
                &mut desc_chain,
                &self.mapping,
                &self.ext_mapping.lock().unwrap(),
                self.msi_iova_space,
            )?;

            self.request_queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), len as u32)
                .map_err(Error::QueueAddUsed)?;

            used_descs = true;
        }

        Ok(used_descs)
    }

    fn signal_used_queue(&self, queue_index: u16) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(queue_index))
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
        helper.add_event(self.request_queue_evt.as_raw_fd(), REQUEST_Q_EVENT)?;
        helper.run(paused, paused_sync, self)?;

        Ok(())
    }
}

impl EpollHelperHandler for IommuEpollHandler {
    fn handle_event(
        &mut self,
        _helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;
        match ev_type {
            REQUEST_Q_EVENT => {
                self.request_queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get queue event: {:?}", e))
                })?;

                let needs_notification = self.request_queue().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to process request queue : {:?}",
                        e
                    ))
                })?;
                if needs_notification {
                    self.signal_used_queue(0).map_err(|e| {
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

#[derive(Clone, Copy, Debug, Serialize, Deserialize)]
struct Mapping {
    gpa: u64,
    size: u64,
}

#[derive(Clone, Debug)]
struct Domain {
    mappings: BTreeMap<u64, Mapping>,
    bypass: bool,
}

#[derive(Debug)]
pub struct IommuMapping {
    // Domain related to an endpoint.
    endpoints: Arc<RwLock<BTreeMap<u32, u32>>>,
    // Information related to each domain.
    domains: Arc<RwLock<BTreeMap<u32, Domain>>>,
    // Global flag indicating if endpoints that are not attached to any domain
    // are in bypass mode.
    bypass: AtomicBool,
}

impl DmaRemapping for IommuMapping {
    fn translate_gva(&self, id: u32, addr: u64) -> std::result::Result<u64, std::io::Error> {
        debug!("Translate GVA addr 0x{:x}", addr);
        if let Some(domain_id) = self.endpoints.read().unwrap().get(&id) {
            if let Some(domain) = self.domains.read().unwrap().get(domain_id) {
                // Directly return identity mapping in case the domain is in
                // bypass mode.
                if domain.bypass {
                    return Ok(addr);
                }

                let range_start = if VIRTIO_IOMMU_PAGE_SIZE_MASK > addr {
                    0
                } else {
                    addr - VIRTIO_IOMMU_PAGE_SIZE_MASK
                };
                for (&key, &value) in domain
                    .mappings
                    .range((Included(&range_start), Included(&addr)))
                {
                    if addr >= key && addr < key + value.size {
                        let new_addr = addr - key + value.gpa;
                        debug!("Into GPA addr 0x{:x}", new_addr);
                        return Ok(new_addr);
                    }
                }
            }
        } else if self.bypass.load(Ordering::Acquire) {
            return Ok(addr);
        }

        Err(io::Error::new(
            io::ErrorKind::Other,
            format!("failed to translate GVA addr 0x{:x}", addr),
        ))
    }

    fn translate_gpa(&self, id: u32, addr: u64) -> std::result::Result<u64, std::io::Error> {
        debug!("Translate GPA addr 0x{:x}", addr);
        if let Some(domain_id) = self.endpoints.read().unwrap().get(&id) {
            if let Some(domain) = self.domains.read().unwrap().get(domain_id) {
                // Directly return identity mapping in case the domain is in
                // bypass mode.
                if domain.bypass {
                    return Ok(addr);
                }

                for (&key, &value) in domain.mappings.iter() {
                    if addr >= value.gpa && addr < value.gpa + value.size {
                        let new_addr = addr - value.gpa + key;
                        debug!("Into GVA addr 0x{:x}", new_addr);
                        return Ok(new_addr);
                    }
                }
            }
        } else if self.bypass.load(Ordering::Acquire) {
            return Ok(addr);
        }

        Err(io::Error::new(
            io::ErrorKind::Other,
            format!("failed to translate GPA addr 0x{:x}", addr),
        ))
    }
}

#[derive(Debug)]
pub struct AccessPlatformMapping {
    id: u32,
    mapping: Arc<IommuMapping>,
}

impl AccessPlatformMapping {
    pub fn new(id: u32, mapping: Arc<IommuMapping>) -> Self {
        AccessPlatformMapping { id, mapping }
    }
}

impl AccessPlatform for AccessPlatformMapping {
    fn translate_gva(&self, base: u64, _size: u64) -> std::result::Result<u64, std::io::Error> {
        self.mapping.translate_gva(self.id, base)
    }
    fn translate_gpa(&self, base: u64, _size: u64) -> std::result::Result<u64, std::io::Error> {
        self.mapping.translate_gpa(self.id, base)
    }
}

pub struct Iommu {
    common: VirtioCommon,
    id: String,
    config: VirtioIommuConfig,
    mapping: Arc<IommuMapping>,
    ext_mapping: Arc<Mutex<BTreeMap<u32, Arc<dyn ExternalDmaMapping>>>>,
    seccomp_action: SeccompAction,
    exit_evt: EventFd,
    msi_iova_space: (u64, u64),
}

type EndpointsState = Vec<(u32, u32)>;
type DomainsState = Vec<(u32, (Vec<(u64, Mapping)>, bool))>;

#[derive(Serialize, Deserialize)]
pub struct IommuState {
    avail_features: u64,
    acked_features: u64,
    endpoints: EndpointsState,
    domains: DomainsState,
}

impl Iommu {
    pub fn new(
        id: String,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        msi_iova_space: (u64, u64),
        state: Option<IommuState>,
    ) -> io::Result<(Self, Arc<IommuMapping>)> {
        let (avail_features, acked_features, endpoints, domains) = if let Some(state) = state {
            info!("Restoring virtio-iommu {}", id);
            (
                state.avail_features,
                state.acked_features,
                state.endpoints.into_iter().collect(),
                state
                    .domains
                    .into_iter()
                    .map(|(k, v)| {
                        (
                            k,
                            Domain {
                                mappings: v.0.into_iter().collect(),
                                bypass: v.1,
                            },
                        )
                    })
                    .collect(),
            )
        } else {
            let avail_features = 1u64 << VIRTIO_F_VERSION_1
                | 1u64 << VIRTIO_IOMMU_F_MAP_UNMAP
                | 1u64 << VIRTIO_IOMMU_F_PROBE
                | 1u64 << VIRTIO_IOMMU_F_BYPASS_CONFIG;

            (avail_features, 0, BTreeMap::new(), BTreeMap::new())
        };

        let config = VirtioIommuConfig {
            page_size_mask: VIRTIO_IOMMU_PAGE_SIZE_MASK,
            probe_size: PROBE_PROP_SIZE,
            ..Default::default()
        };

        let mapping = Arc::new(IommuMapping {
            endpoints: Arc::new(RwLock::new(endpoints)),
            domains: Arc::new(RwLock::new(domains)),
            bypass: AtomicBool::new(true),
        });

        Ok((
            Iommu {
                id,
                common: VirtioCommon {
                    device_type: VirtioDeviceType::Iommu as u32,
                    queue_sizes: QUEUE_SIZES.to_vec(),
                    avail_features,
                    acked_features,
                    paused_sync: Some(Arc::new(Barrier::new(2))),
                    min_queues: NUM_QUEUES as u16,
                    ..Default::default()
                },
                config,
                mapping: mapping.clone(),
                ext_mapping: Arc::new(Mutex::new(BTreeMap::new())),
                seccomp_action,
                exit_evt,
                msi_iova_space,
            },
            mapping,
        ))
    }

    fn state(&self) -> IommuState {
        IommuState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            endpoints: self
                .mapping
                .endpoints
                .read()
                .unwrap()
                .clone()
                .into_iter()
                .collect(),
            domains: self
                .mapping
                .domains
                .read()
                .unwrap()
                .clone()
                .into_iter()
                .map(|(k, v)| (k, (v.mappings.into_iter().collect(), v.bypass)))
                .collect(),
        }
    }

    fn update_bypass(&mut self) {
        // Use bypass from config if VIRTIO_IOMMU_F_BYPASS_CONFIG has been negotiated
        if !self
            .common
            .feature_acked(VIRTIO_IOMMU_F_BYPASS_CONFIG.into())
        {
            return;
        }

        let bypass = self.config.bypass == 1;
        info!("Updating bypass mode to {}", bypass);
        self.mapping.bypass.store(bypass, Ordering::Release);
    }

    pub fn add_external_mapping(&mut self, device_id: u32, mapping: Arc<dyn ExternalDmaMapping>) {
        self.ext_mapping.lock().unwrap().insert(device_id, mapping);
    }

    #[cfg(fuzzing)]
    pub fn wait_for_epoll_threads(&mut self) {
        self.common.wait_for_epoll_threads();
    }
}

impl Drop for Iommu {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
    }
}

impl VirtioDevice for Iommu {
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

    fn write_config(&mut self, offset: u64, data: &[u8]) {
        // The "bypass" field is the only mutable field
        let bypass_offset =
            (&self.config.bypass as *const _ as u64) - (&self.config as *const _ as u64);
        if offset != bypass_offset || data.len() != std::mem::size_of_val(&self.config.bypass) {
            error!(
                "Attempt to write to read-only field: offset {:x} length {}",
                offset,
                data.len()
            );
            return;
        }

        self.config.bypass = data[0];

        self.update_bypass();
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        let (kill_evt, pause_evt) = self.common.dup_eventfds();

        let (_, request_queue, request_queue_evt) = queues.remove(0);
        let (_, _event_queue, _event_queue_evt) = queues.remove(0);

        let mut handler = IommuEpollHandler {
            mem,
            request_queue,
            _event_queue,
            interrupt_cb,
            request_queue_evt,
            _event_queue_evt,
            kill_evt,
            pause_evt,
            mapping: self.mapping.clone(),
            ext_mapping: self.ext_mapping.clone(),
            msi_iova_space: self.msi_iova_space,
        };

        let paused = self.common.paused.clone();
        let paused_sync = self.common.paused_sync.clone();
        let mut epoll_threads = Vec::new();
        spawn_virtio_thread(
            &self.id,
            &self.seccomp_action,
            Thread::VirtioIommu,
            &mut epoll_threads,
            &self.exit_evt,
            move || handler.run(paused, paused_sync.unwrap()),
        )?;

        self.common.epoll_threads = Some(epoll_threads);

        event!("virtio-device", "activated", "id", &self.id);
        Ok(())
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        let result = self.common.reset();
        event!("virtio-device", "reset", "id", &self.id);
        result
    }
}

impl Pausable for Iommu {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()
    }
}

impl Snapshottable for Iommu {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id, &self.state())
    }
}
impl Transportable for Iommu {}
impl Migratable for Iommu {}
