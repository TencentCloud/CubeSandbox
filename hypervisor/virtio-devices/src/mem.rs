// Copyright (c) 2020 Ant Financial
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

use super::Error as DeviceError;
use super::{
    ActivateError, ActivateResult, EpollHelper, EpollHelperError, EpollHelperHandler, VirtioCommon,
    VirtioDevice, VirtioDeviceType, EPOLL_HELPER_EVENT_LAST, VIRTIO_F_VERSION_1,
};
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::{GuestMemoryMmap, GuestRegionMmap};
use crate::{VirtioInterrupt, VirtioInterruptType};
use anyhow::anyhow;
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::io;
use std::mem::size_of;
use std::os::unix::io::{AsRawFd, RawFd};
use std::result;
use std::sync::atomic::AtomicBool;
use std::sync::mpsc;
use std::sync::{Arc, Barrier, Mutex};
use thiserror::Error;
use virtio_queue::{DescriptorChain, Queue, QueueT};
use vm_device::dma_mapping::ExternalDmaMapping;
use vm_memory::{
    Address, ByteValued, Bytes, GuestAddress, GuestAddressSpace, GuestMemoryAtomic,
    GuestMemoryError, GuestMemoryLoadGuard, GuestMemoryRegion,
};
use vm_migration::protocol::MemoryRangeTable;
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vmm_sys_util::eventfd::EventFd;

const QUEUE_SIZE: u16 = 128;
const QUEUE_SIZES: &[u16] = &[QUEUE_SIZE];

// 128MiB is the standard memory block size in Linux. A virtio-mem region must
// be aligned on this size, and the region size must be a multiple of it.
pub const VIRTIO_MEM_ALIGN_SIZE: u64 = 128 << 20;
// Use 2 MiB alignment so transparent hugepages can be used by KVM.
const VIRTIO_MEM_DEFAULT_BLOCK_SIZE: u64 = 2 << 20;

// Request processed successfully, applicable for
// - VIRTIO_MEM_REQ_PLUG
// - VIRTIO_MEM_REQ_UNPLUG
// - VIRTIO_MEM_REQ_UNPLUG_ALL
// - VIRTIO_MEM_REQ_STATE
const VIRTIO_MEM_RESP_ACK: u16 = 0;

// Request denied - e.g. trying to plug more than requested, applicable for
// - VIRTIO_MEM_REQ_PLUG
const VIRTIO_MEM_RESP_NACK: u16 = 1;

// Request cannot be processed right now, try again later, applicable for
// - VIRTIO_MEM_REQ_PLUG
// - VIRTIO_MEM_REQ_UNPLUG
// - VIRTIO_MEM_REQ_UNPLUG_ALL
#[allow(unused)]
const VIRTIO_MEM_RESP_BUSY: u16 = 2;

// Error in request (e.g. addresses/alignment), applicable for
// - VIRTIO_MEM_REQ_PLUG
// - VIRTIO_MEM_REQ_UNPLUG
// - VIRTIO_MEM_REQ_STATE
const VIRTIO_MEM_RESP_ERROR: u16 = 3;

// State of memory blocks is "plugged"
const VIRTIO_MEM_STATE_PLUGGED: u16 = 0;
// State of memory blocks is "unplugged"
const VIRTIO_MEM_STATE_UNPLUGGED: u16 = 1;
// State of memory blocks is "mixed"
const VIRTIO_MEM_STATE_MIXED: u16 = 2;

// request to plug memory blocks
const VIRTIO_MEM_REQ_PLUG: u16 = 0;
// request to unplug memory blocks
const VIRTIO_MEM_REQ_UNPLUG: u16 = 1;
// request to unplug all blocks and shrink the usable size
const VIRTIO_MEM_REQ_UNPLUG_ALL: u16 = 2;
// request information about the plugged state of memory blocks
const VIRTIO_MEM_REQ_STATE: u16 = 3;

// New descriptors are pending on the virtio queue.
const QUEUE_AVAIL_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;

// Virtio features
const VIRTIO_MEM_F_ACPI_PXM: u8 = 0;

#[derive(Error, Debug)]
pub enum Error {
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
    #[error("Failed to EventFd write: {0}")]
    EventFdWriteFail(std::io::Error),
    #[error("Failed to EventFd try_clone: {0}")]
    EventFdTryCloneFail(std::io::Error),
    #[error("Failed to MpscRecv: {0}")]
    MpscRecvFail(mpsc::RecvError),
    #[error("Resize invalid argument: {0}")]
    ResizeError(anyhow::Error),
    #[error("Fail to resize trigger: {0}")]
    ResizeTriggerFail(DeviceError),
    #[error("Invalid configuration: {0}")]
    ValidateError(anyhow::Error),
    #[error("Failed discarding memory range: {0}")]
    DiscardMemoryRange(std::io::Error),
    #[error("Failed DMA mapping: {0}")]
    DmaMap(std::io::Error),
    #[error("Failed DMA unmapping: {0}")]
    DmaUnmap(std::io::Error),
    #[error("Invalid DMA mapping handler")]
    InvalidDmaMappingHandler,
    #[error("Not activated by the guest")]
    NotActivatedByGuest,
    #[error("Unknown request type: {0}")]
    UnknownRequestType(u16),
    #[error("Failed adding used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
}

#[repr(C)]
#[derive(Copy, Clone, Debug, Default)]
struct VirtioMemReq {
    req_type: u16,
    padding: [u16; 3],
    addr: u64,
    nb_blocks: u16,
    padding_1: [u16; 3],
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioMemReq {}

#[repr(C)]
#[derive(Copy, Clone, Debug, Default)]
struct VirtioMemResp {
    resp_type: u16,
    padding: [u16; 3],
    state: u16,
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioMemResp {}

#[repr(C)]
#[derive(Copy, Clone, Debug, Default, Serialize, Deserialize)]
pub struct VirtioMemConfig {
    // Block size and alignment. Cannot change.
    block_size: u64,
    // Valid with VIRTIO_MEM_F_ACPI_PXM. Cannot change.
    node_id: u16,
    padding: [u8; 6],
    // Start address of the memory region. Cannot change.
    addr: u64,
    // Region size (maximum). Cannot change.
    region_size: u64,
    // Currently usable region size. Can grow up to region_size. Can
    // shrink due to VIRTIO_MEM_REQ_UNPLUG_ALL (in which case no config
    // update will be sent).
    usable_region_size: u64,
    // Currently used size. Changes due to plug/unplug requests, but no
    // config updates will be sent.
    plugged_size: u64,
    // Requested size. New plug requests cannot exceed it. Can change.
    requested_size: u64,
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioMemConfig {}

impl VirtioMemConfig {
    fn validate(&self) -> result::Result<(), Error> {
        if self.addr % self.block_size != 0 {
            return Err(Error::ValidateError(anyhow!(
                "addr 0x{:x} is not aligned on block_size 0x{:x}",
                self.addr,
                self.block_size
            )));
        }
        if self.region_size % self.block_size != 0 {
            return Err(Error::ValidateError(anyhow!(
                "region_size 0x{:x} is not aligned on block_size 0x{:x}",
                self.region_size,
                self.block_size
            )));
        }
        if self.usable_region_size % self.block_size != 0 {
            return Err(Error::ValidateError(anyhow!(
                "usable_region_size 0x{:x} is not aligned on block_size 0x{:x}",
                self.usable_region_size,
                self.block_size
            )));
        }
        if self.plugged_size % self.block_size != 0 {
            return Err(Error::ValidateError(anyhow!(
                "plugged_size 0x{:x} is not aligned on block_size 0x{:x}",
                self.plugged_size,
                self.block_size
            )));
        }
        if self.requested_size % self.block_size != 0 {
            return Err(Error::ValidateError(anyhow!(
                "requested_size 0x{:x} is not aligned on block_size 0x{:x}",
                self.requested_size,
                self.block_size
            )));
        }

        Ok(())
    }

    fn resize(&mut self, size: u64) -> result::Result<(), Error> {
        if self.requested_size == size {
            return Err(Error::ResizeError(anyhow!(
                "new size 0x{:x} and requested_size are identical",
                size
            )));
        } else if size > self.region_size {
            return Err(Error::ResizeError(anyhow!(
                "new size 0x{:x} is bigger than region_size 0x{:x}",
                size,
                self.region_size
            )));
        } else if size % self.block_size != 0 {
            return Err(Error::ResizeError(anyhow!(
                "new size 0x{:x} is not aligned on block_size 0x{:x}",
                size,
                self.block_size
            )));
        }

        self.requested_size = size;

        Ok(())
    }

    fn is_valid_range(&self, addr: u64, size: u64) -> bool {
        // Ensure no overflow from adding 'addr' and 'size' whose value are both
        // controlled by the guest driver
        if addr.checked_add(size).is_none() {
            return false;
        }

        // Start address must be aligned on block_size, the size must be
        // greater than 0, and all blocks covered by the request must be
        // in the usable region.
        if addr % self.block_size != 0
            || size == 0
            || (addr < self.addr || addr + size >= self.addr + self.usable_region_size)
        {
            return false;
        }

        true
    }
}

struct Request {
    req: VirtioMemReq,
    status_addr: GuestAddress,
}

impl Request {
    fn parse(
        desc_chain: &mut DescriptorChain<GuestMemoryLoadGuard<GuestMemoryMmap>>,
    ) -> result::Result<Request, Error> {
        let desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;
        // The descriptor contains the request type which MUST be readable.
        if desc.is_write_only() {
            return Err(Error::UnexpectedWriteOnlyDescriptor);
        }
        if desc.len() as usize != size_of::<VirtioMemReq>() {
            return Err(Error::InvalidRequest);
        }
        let req: VirtioMemReq = desc_chain
            .memory()
            .read_obj(desc.addr())
            .map_err(Error::GuestMemory)?;

        let status_desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;

        // The status MUST always be writable
        if !status_desc.is_write_only() {
            return Err(Error::UnexpectedReadOnlyDescriptor);
        }

        if (status_desc.len() as usize) < size_of::<VirtioMemResp>() {
            return Err(Error::BufferLengthTooSmall);
        }

        Ok(Request {
            req,
            status_addr: status_desc.addr(),
        })
    }

    fn send_response(
        &self,
        mem: &GuestMemoryMmap,
        resp_type: u16,
        state: u16,
    ) -> Result<u32, Error> {
        let resp = VirtioMemResp {
            resp_type,
            state,
            ..Default::default()
        };
        mem.write_obj(resp, self.status_addr)
            .map_err(Error::GuestMemory)?;
        Ok(size_of::<VirtioMemResp>() as u32)
    }
}

#[derive(Clone, Serialize, Deserialize)]
pub struct BlocksState {
    bitmap: Vec<bool>,
}

impl BlocksState {
    pub fn new(region_size: u64) -> Self {
        BlocksState {
            bitmap: vec![false; (region_size / VIRTIO_MEM_DEFAULT_BLOCK_SIZE) as usize],
        }
    }

    fn is_range_state(&self, first_block_index: usize, nb_blocks: u16, plug: bool) -> bool {
        for state in self
            .bitmap
            .iter()
            .skip(first_block_index)
            .take(nb_blocks as usize)
        {
            if *state != plug {
                return false;
            }
        }
        true
    }

    fn set_range(&mut self, first_block_index: usize, nb_blocks: u16, plug: bool) {
        for state in self
            .bitmap
            .iter_mut()
            .skip(first_block_index)
            .take(nb_blocks as usize)
        {
            *state = plug;
        }
    }

    fn inner(&self) -> &Vec<bool> {
        &self.bitmap
    }

    pub fn memory_ranges(&self, start_addr: u64, plugged: bool) -> MemoryRangeTable {
        let mut bitmap: Vec<u64> = Vec::new();
        let mut i = 0;
        for (j, bit) in self.bitmap.iter().enumerate() {
            if j % 64 == 0 {
                bitmap.push(0);

                if j != 0 {
                    i += 1;
                }
            }

            if *bit == plugged {
                bitmap[i] |= 1 << (j % 64);
            }
        }

        MemoryRangeTable::from_bitmap(bitmap, start_addr, VIRTIO_MEM_DEFAULT_BLOCK_SIZE)
    }
}

struct MemEpollHandler {
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    host_addr: u64,
    host_fd: Option<RawFd>,
    blocks_state: Arc<Mutex<BlocksState>>,
    config: Arc<Mutex<VirtioMemConfig>>,
    queue: Queue,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    queue_evt: EventFd,
    kill_evt: EventFd,
    pause_evt: EventFd,
    hugepages: bool,
    dma_mapping_handlers: Arc<Mutex<BTreeMap<VirtioMemMappingSource, Arc<dyn ExternalDmaMapping>>>>,
}

impl MemEpollHandler {
    fn discard_memory_range(&self, offset: u64, size: u64) -> Result<(), Error> {
        // Use fallocate if the memory region is backed by a file.
        if let Some(fd) = self.host_fd {
            let res = unsafe {
                libc::fallocate64(
                    fd,
                    libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE,
                    offset as libc::off64_t,
                    size as libc::off64_t,
                )
            };
            if res != 0 {
                let err = io::Error::last_os_error();
                error!("Deallocating file space failed: {}", err);
                return Err(Error::DiscardMemoryRange(err));
            }
        }

        // Only use madvise if the memory region is not allocated with
        // hugepages.
        if !self.hugepages {
            let res = unsafe {
                libc::madvise(
                    (self.host_addr + offset) as *mut libc::c_void,
                    size as libc::size_t,
                    libc::MADV_DONTNEED,
                )
            };
            if res != 0 {
                let err = io::Error::last_os_error();
                error!("Advising kernel about pages range failed: {}", err);
                return Err(Error::DiscardMemoryRange(err));
            }
        }

        Ok(())
    }

    fn state_change_request(&mut self, addr: u64, nb_blocks: u16, plug: bool) -> u16 {
        let mut config = self.config.lock().unwrap();
        let size: u64 = nb_blocks as u64 * config.block_size;

        if plug && (config.plugged_size + size > config.requested_size) {
            return VIRTIO_MEM_RESP_NACK;
        }
        if !config.is_valid_range(addr, size) {
            return VIRTIO_MEM_RESP_ERROR;
        }

        let offset = addr - config.addr;

        let first_block_index = (offset / config.block_size) as usize;
        if !self
            .blocks_state
            .lock()
            .unwrap()
            .is_range_state(first_block_index, nb_blocks, !plug)
        {
            return VIRTIO_MEM_RESP_ERROR;
        }

        if !plug {
            if let Err(e) = self.discard_memory_range(offset, size) {
                error!("failed discarding memory range: {:?}", e);
                return VIRTIO_MEM_RESP_ERROR;
            }
        }

        self.blocks_state
            .lock()
            .unwrap()
            .set_range(first_block_index, nb_blocks, plug);

        let handlers = self.dma_mapping_handlers.lock().unwrap();
        if plug {
            let mut gpa = addr;
            for _ in 0..nb_blocks {
                for (_, handler) in handlers.iter() {
                    if let Err(e) = handler.map(gpa, gpa, config.block_size) {
                        error!(
                            "failed DMA mapping addr 0x{:x} size 0x{:x}: {}",
                            gpa, config.block_size, e
                        );
                        return VIRTIO_MEM_RESP_ERROR;
                    }
                }

                gpa += config.block_size;
            }

            config.plugged_size += size;
        } else {
            for (_, handler) in handlers.iter() {
                if let Err(e) = handler.unmap(addr, size) {
                    error!(
                        "failed DMA unmapping addr 0x{:x} size 0x{:x}: {}",
                        addr, size, e
                    );
                    return VIRTIO_MEM_RESP_ERROR;
                }
            }

            config.plugged_size -= size;
        }

        VIRTIO_MEM_RESP_ACK
    }

    fn unplug_all(&mut self) -> u16 {
        let mut config = self.config.lock().unwrap();
        if let Err(e) = self.discard_memory_range(0, config.region_size) {
            error!("failed discarding memory range: {:?}", e);
            return VIRTIO_MEM_RESP_ERROR;
        }

        // Remaining plugged blocks are unmapped.
        if config.plugged_size > 0 {
            let handlers = self.dma_mapping_handlers.lock().unwrap();
            for (idx, plugged) in self.blocks_state.lock().unwrap().inner().iter().enumerate() {
                if *plugged {
                    let gpa = config.addr + (idx as u64 * config.block_size);
                    for (_, handler) in handlers.iter() {
                        if let Err(e) = handler.unmap(gpa, config.block_size) {
                            error!(
                                "failed DMA unmapping addr 0x{:x} size 0x{:x}: {}",
                                gpa, config.block_size, e
                            );
                            return VIRTIO_MEM_RESP_ERROR;
                        }
                    }
                }
            }
        }

        self.blocks_state.lock().unwrap().set_range(
            0,
            (config.region_size / config.block_size) as u16,
            false,
        );

        config.plugged_size = 0;

        VIRTIO_MEM_RESP_ACK
    }

    fn state_request(&self, addr: u64, nb_blocks: u16) -> (u16, u16) {
        let config = self.config.lock().unwrap();
        let size: u64 = nb_blocks as u64 * config.block_size;

        let resp_type = if config.is_valid_range(addr, size) {
            VIRTIO_MEM_RESP_ACK
        } else {
            VIRTIO_MEM_RESP_ERROR
        };

        let offset = addr - config.addr;
        let first_block_index = (offset / config.block_size) as usize;
        let resp_state =
            if self
                .blocks_state
                .lock()
                .unwrap()
                .is_range_state(first_block_index, nb_blocks, true)
            {
                VIRTIO_MEM_STATE_PLUGGED
            } else if self.blocks_state.lock().unwrap().is_range_state(
                first_block_index,
                nb_blocks,
                false,
            ) {
                VIRTIO_MEM_STATE_UNPLUGGED
            } else {
                VIRTIO_MEM_STATE_MIXED
            };

        (resp_type, resp_state)
    }

    fn signal(&self, int_type: VirtioInterruptType) -> result::Result<(), DeviceError> {
        self.interrupt_cb.trigger(int_type).map_err(|e| {
            error!("Failed to signal used queue: {:?}", e);
            DeviceError::FailedSignalingUsedQueue(e)
        })
    }

    fn process_queue(&mut self) -> Result<bool, Error> {
        let mut used_descs = false;

        while let Some(mut desc_chain) = self.queue.pop_descriptor_chain(self.mem.memory()) {
            let r = Request::parse(&mut desc_chain)?;
            let (resp_type, resp_state) = match r.req.req_type {
                VIRTIO_MEM_REQ_PLUG => (
                    self.state_change_request(r.req.addr, r.req.nb_blocks, true),
                    0u16,
                ),
                VIRTIO_MEM_REQ_UNPLUG => (
                    self.state_change_request(r.req.addr, r.req.nb_blocks, false),
                    0u16,
                ),
                VIRTIO_MEM_REQ_UNPLUG_ALL => (self.unplug_all(), 0u16),
                VIRTIO_MEM_REQ_STATE => self.state_request(r.req.addr, r.req.nb_blocks),
                _ => {
                    return Err(Error::UnknownRequestType(r.req.req_type));
                }
            };
            let len = r.send_response(desc_chain.memory(), resp_type, resp_state)?;
            self.queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), len)
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;
        }

        Ok(used_descs)
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

impl EpollHelperHandler for MemEpollHandler {
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
                    self.signal(VirtioInterruptType::Queue(0)).map_err(|e| {
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

#[derive(PartialEq, Eq, PartialOrd, Ord)]
pub enum VirtioMemMappingSource {
    Container,
    Device(u32),
}

#[derive(Serialize, Deserialize)]
pub struct MemState {
    pub avail_features: u64,
    pub acked_features: u64,
    pub config: VirtioMemConfig,
    pub blocks_state: BlocksState,
}

pub struct Mem {
    common: VirtioCommon,
    id: String,
    host_addr: u64,
    host_fd: Option<RawFd>,
    config: Arc<Mutex<VirtioMemConfig>>,
    seccomp_action: SeccompAction,
    hugepages: bool,
    dma_mapping_handlers: Arc<Mutex<BTreeMap<VirtioMemMappingSource, Arc<dyn ExternalDmaMapping>>>>,
    blocks_state: Arc<Mutex<BlocksState>>,
    exit_evt: EventFd,
    interrupt_cb: Option<Arc<dyn VirtioInterrupt>>,
}

impl Mem {
    // Create a new virtio-mem device.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        region: &Arc<GuestRegionMmap>,
        seccomp_action: SeccompAction,
        numa_node_id: Option<u16>,
        initial_size: u64,
        hugepages: bool,
        exit_evt: EventFd,
        blocks_state: Arc<Mutex<BlocksState>>,
        state: Option<MemState>,
    ) -> io::Result<Mem> {
        let region_len = region.len();

        if region_len != region_len / VIRTIO_MEM_ALIGN_SIZE * VIRTIO_MEM_ALIGN_SIZE {
            return Err(io::Error::new(
                io::ErrorKind::Other,
                format!(
                    "Virtio-mem size is not aligned with {}",
                    VIRTIO_MEM_ALIGN_SIZE
                ),
            ));
        }

        let (avail_features, acked_features, config) = if let Some(state) = state {
            info!("Restoring virtio-mem {}", id);
            *(blocks_state.lock().unwrap()) = state.blocks_state.clone();
            (state.avail_features, state.acked_features, state.config)
        } else {
            let mut avail_features = 1u64 << VIRTIO_F_VERSION_1;

            let mut config = VirtioMemConfig {
                block_size: VIRTIO_MEM_DEFAULT_BLOCK_SIZE,
                addr: region.start_addr().raw_value(),
                region_size: region.len(),
                usable_region_size: region.len(),
                plugged_size: 0,
                requested_size: 0,
                ..Default::default()
            };

            if initial_size != 0 {
                config.resize(initial_size).map_err(|e| {
                    io::Error::new(
                        io::ErrorKind::Other,
                        format!(
                            "Failed to resize virtio-mem configuration to {}: {:?}",
                            initial_size, e
                        ),
                    )
                })?;
            }

            if let Some(node_id) = numa_node_id {
                avail_features |= 1u64 << VIRTIO_MEM_F_ACPI_PXM;
                config.node_id = node_id;
            }

            // Make sure the virtio-mem configuration complies with the
            // specification.
            config.validate().map_err(|e| {
                io::Error::new(
                    io::ErrorKind::Other,
                    format!("Invalid virtio-mem configuration: {:?}", e),
                )
            })?;

            (avail_features, 0, config)
        };

        let host_fd = region
            .file_offset()
            .map(|f_offset| f_offset.file().as_raw_fd());

        Ok(Mem {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Mem as u32,
                avail_features,
                acked_features,
                paused_sync: Some(Arc::new(Barrier::new(2))),
                queue_sizes: QUEUE_SIZES.to_vec(),
                min_queues: 1,
                ..Default::default()
            },
            id,
            host_addr: region.as_ptr() as u64,
            host_fd,
            config: Arc::new(Mutex::new(config)),
            seccomp_action,
            hugepages,
            dma_mapping_handlers: Arc::new(Mutex::new(BTreeMap::new())),
            blocks_state,
            exit_evt,
            interrupt_cb: None,
        })
    }

    pub fn resize(&mut self, size: u64) -> result::Result<(), Error> {
        let mut config = self.config.lock().unwrap();
        config.resize(size).map_err(|e| {
            Error::ResizeError(anyhow!("Failed to update virtio configuration: {:?}", e))
        })?;

        if let Some(interrupt_cb) = self.interrupt_cb.as_ref() {
            interrupt_cb
                .trigger(VirtioInterruptType::Config)
                .map_err(|e| {
                    Error::ResizeError(anyhow!("Failed to signal the guest about resize: {:?}", e))
                })
        } else {
            Ok(())
        }
    }

    pub fn add_dma_mapping_handler(
        &mut self,
        source: VirtioMemMappingSource,
        handler: Arc<dyn ExternalDmaMapping>,
    ) -> result::Result<(), Error> {
        let config = self.config.lock().unwrap();

        if config.plugged_size > 0 {
            for (idx, plugged) in self.blocks_state.lock().unwrap().inner().iter().enumerate() {
                if *plugged {
                    let gpa = config.addr + (idx as u64 * config.block_size);
                    handler
                        .map(gpa, gpa, config.block_size)
                        .map_err(Error::DmaMap)?;
                }
            }
        }

        self.dma_mapping_handlers
            .lock()
            .unwrap()
            .insert(source, handler);

        Ok(())
    }

    pub fn remove_dma_mapping_handler(
        &mut self,
        source: VirtioMemMappingSource,
    ) -> result::Result<(), Error> {
        let handler = self
            .dma_mapping_handlers
            .lock()
            .unwrap()
            .remove(&source)
            .ok_or(Error::InvalidDmaMappingHandler)?;

        let config = self.config.lock().unwrap();

        if config.plugged_size > 0 {
            for (idx, plugged) in self.blocks_state.lock().unwrap().inner().iter().enumerate() {
                if *plugged {
                    let gpa = config.addr + (idx as u64 * config.block_size);
                    handler
                        .unmap(gpa, config.block_size)
                        .map_err(Error::DmaUnmap)?;
                }
            }
        }

        Ok(())
    }

    fn state(&self) -> MemState {
        MemState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: *(self.config.lock().unwrap()),
            blocks_state: self.blocks_state.lock().unwrap().clone(),
        }
    }

    #[cfg(fuzzing)]
    pub fn wait_for_epoll_threads(&mut self) {
        self.common.wait_for_epoll_threads();
    }
}

impl Drop for Mem {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
    }
}

impl VirtioDevice for Mem {
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
        self.read_config_from_slice(self.config.lock().unwrap().as_slice(), offset, data);
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        let (kill_evt, pause_evt) = self.common.dup_eventfds();

        let (_, queue, queue_evt) = queues.remove(0);

        self.interrupt_cb = Some(interrupt_cb.clone());

        let mut handler = MemEpollHandler {
            mem,
            host_addr: self.host_addr,
            host_fd: self.host_fd,
            blocks_state: Arc::clone(&self.blocks_state),
            config: self.config.clone(),
            queue,
            interrupt_cb,
            queue_evt,
            kill_evt,
            pause_evt,
            hugepages: self.hugepages,
            dma_mapping_handlers: Arc::clone(&self.dma_mapping_handlers),
        };

        let unplugged_memory_ranges = self.blocks_state.lock().unwrap().memory_ranges(0, false);
        for range in unplugged_memory_ranges.regions() {
            handler
                .discard_memory_range(range.gpa, range.length)
                .map_err(|e| {
                    error!(
                        "failed discarding memory range [0x{:x}-0x{:x}]: {:?}",
                        range.gpa,
                        range.gpa + range.length - 1,
                        e
                    );
                    ActivateError::BadActivate
                })?;
        }

        let paused = self.common.paused.clone();
        let paused_sync = self.common.paused_sync.clone();
        let mut epoll_threads = Vec::new();

        spawn_virtio_thread(
            &self.id,
            &self.seccomp_action,
            Thread::VirtioMem,
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

impl Pausable for Mem {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()
    }
}

impl Snapshottable for Mem {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id(), &self.state())
    }
}
impl Transportable for Mem {}
impl Migratable for Mem {}
