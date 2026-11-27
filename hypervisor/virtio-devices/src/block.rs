// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use super::Error as DeviceError;
use super::{
    ActivateError, ActivateResult, EpollHelper, EpollHelperError, EpollHelperHandler,
    RateLimiterConfig, VirtioCommon, VirtioDevice, VirtioDeviceType, VirtioInterruptType,
    EPOLL_HELPER_EVENT_LAST,
};
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::GuestMemoryMmap;
use crate::VirtioInterrupt;
use anyhow::anyhow;
use block_util::{
    async_io::AsyncIo, async_io::DiskFile, build_disk_image_id, Request, RequestType,
    VirtioBlockConfig,
};
use rate_limiter::{BucketReduction, RateLimiter, TokenType};
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::io;
use std::num::Wrapping;
use std::ops::Deref;
use std::os::unix::io::AsRawFd;
use std::path::PathBuf;
use std::result;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Barrier};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};
use std::{collections::HashMap, convert::TryInto};
use thiserror::Error;
use virtio_bindings::bindings::virtio_blk::*;
use virtio_bindings::bindings::virtio_ring::{
    VIRTIO_RING_F_EVENT_IDX, VIRTIO_RING_F_INDIRECT_DESC,
};
use virtio_queue::{Queue, QueueOwnedT, QueueT};
use vm_memory::{ByteValued, Bytes, GuestAddressSpace, GuestMemoryAtomic, GuestMemoryError};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::AccessPlatform;
use vmm_sys_util::eventfd::EventFd;

const SECTOR_SHIFT: u8 = 9;
pub const SECTOR_SIZE: u64 = 0x01 << SECTOR_SHIFT;

pub const MINIMUM_BLOCK_QUEUE_SIZE: u16 = 2;

// Used-ring len is the number of bytes written by the device into guest memory,
// not the backend operation's result. Every completed request writes status.
fn used_len(request_type: RequestType, guest_data_bytes: u32) -> u32 {
    match request_type {
        RequestType::In | RequestType::GetDeviceId => guest_data_bytes.saturating_add(1),
        RequestType::Out
        | RequestType::Flush
        | RequestType::Discard
        | RequestType::Unsupported(_) => 1,
    }
}

fn async_completion_status_and_used_len(
    request_type: RequestType,
    result: i32,
    expected_data_bytes: u32,
) -> (u32, u32) {
    if result < 0 {
        return (VIRTIO_BLK_S_IOERR, used_len(request_type, 0));
    }

    let completed_data_bytes = result as u32;
    let completed = match request_type {
        RequestType::In | RequestType::Out => completed_data_bytes == expected_data_bytes,
        RequestType::Flush | RequestType::Discard => completed_data_bytes == 0,
        RequestType::GetDeviceId | RequestType::Unsupported(_) => false,
    };
    let status = if completed {
        VIRTIO_BLK_S_OK
    } else {
        VIRTIO_BLK_S_IOERR
    };
    let guest_data_bytes = if request_type == RequestType::In {
        completed_data_bytes.min(expected_data_bytes)
    } else {
        0
    };

    (status, used_len(request_type, guest_data_bytes))
}

// New descriptors are pending on the virtio queue.
const QUEUE_AVAIL_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
// New completed tasks are pending on the completion ring.
const COMPLETION_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 2;
// New 'wake up' event from the rate limiter
const RATE_LIMITER_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 3;

// Maximum time we wait for inflight IO to drain when the device is
// paused. Pause is the synchronization point a snapshot relies on, so
// we err on the side of giving the backend (network or local) enough
// time to settle. If this expires we mark drain as failed so the
// snapshot is aborted by the upper layer instead of being captured in
// an inconsistent state.
const PAUSE_DRAIN_TIMEOUT: Duration = Duration::from_secs(20);
// Per-iteration poll budget while draining; bounds CPU when the
// completion notifier is quiet but we have not yet hit the deadline.
const PAUSE_DRAIN_POLL_MS: i32 = 100;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Failed to execute the request: {0}")]
    RequestExecuting(block_util::ExecuteError),
    #[error("Failed to complete the request: {0}")]
    RequestCompleting(block_util::Error),
    #[error("Missing the expected entry in the list of requests")]
    MissingEntryRequestList,
    #[error("Failed adding used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
    #[error("Failed creating an iterator over the queue: {0}")]
    QueueIterator(virtio_queue::Error),
    #[error("Failed to update request status: {0}")]
    RequestStatus(GuestMemoryError),
    #[error("Can't execute an unsupported request")]
    Unsupported(u32),
    #[error("Failed to enable notification: {0}")]
    QueueEnableNotification(virtio_queue::Error),
}

pub type Result<T> = result::Result<T, Error>;

#[derive(Default, Clone)]
pub struct BlockCounters {
    read_bytes: Arc<AtomicU64>,
    read_ops: Arc<AtomicU64>,
    write_bytes: Arc<AtomicU64>,
    write_ops: Arc<AtomicU64>,
    limit_by_bytes: Arc<AtomicU64>,
    limit_by_ops: Arc<AtomicU64>,
}

struct BlockEpollHandler {
    queue_index: u16,
    queue: Queue,
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    disk_image: Box<dyn AsyncIo>,
    disk_nsectors: u64,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    disk_image_id: Vec<u8>,
    kill_evt: EventFd,
    pause_evt: EventFd,
    writeback: Arc<AtomicBool>,
    counters: BlockCounters,
    queue_evt: EventFd,
    request_list: HashMap<u16, Request>,
    rate_limiter: Option<RateLimiter>,
    access_platform: Option<Arc<dyn AccessPlatform>>,
    read_only: bool,
    discard: bool,
    rate_limited: std::sync::Once,
    id: String,
    // Set by `before_pause` when inflight drain or pause-time flush
    // could not be completed within the deadline. Read by
    // `Block::pause` after the pause barrier returns to decide whether
    // the snapshot must be aborted. Shared (not owned) so a multi-queue
    // device aggregates failures from any queue.
    drain_failed: Arc<AtomicBool>,
}

impl BlockEpollHandler {
    fn process_queue_submit(&mut self) -> Result<()> {
        let queue = &mut self.queue;

        let mut limit_by_bytes = Wrapping(0);
        let mut limit_by_ops = Wrapping(0);

        while let Some(mut desc_chain) = queue.pop_descriptor_chain(self.mem.memory()) {
            let mut request = match Request::parse(&mut desc_chain, self.access_platform.as_ref()) {
                Ok(request) => request,
                Err(e) => {
                    error!("Failed parsing block request: {:?}", e);
                    // A malformed chain may not contain a valid status descriptor.
                    // Consume only that chain rather than terminating the VM.
                    queue
                        .add_used(desc_chain.memory(), desc_chain.head_index(), 0)
                        .map_err(Error::QueueAddUsed)?;
                    queue
                        .enable_notification(self.mem.memory().deref())
                        .map_err(Error::QueueEnableNotification)?;
                    continue;
                }
            };

            if request.request_type == RequestType::Discard && !self.discard {
                desc_chain
                    .memory()
                    .write_obj(VIRTIO_BLK_S_UNSUPP as u8, request.status_addr)
                    .map_err(Error::RequestStatus)?;
                queue
                    .add_used(desc_chain.memory(), desc_chain.head_index(), 1)
                    .map_err(Error::QueueAddUsed)?;
                queue
                    .enable_notification(self.mem.memory().deref())
                    .map_err(Error::QueueEnableNotification)?;
                continue;
            }

            // For virtio spec compliance
            // "A device MUST set the status byte to VIRTIO_BLK_S_IOERR for a write request
            // if the VIRTIO_BLK_F_RO feature if offered, and MUST NOT write any data."
            if self.read_only
                && (request.request_type == RequestType::Out
                    || request.request_type == RequestType::Flush
                    || request.request_type == RequestType::Discard)
            {
                desc_chain
                    .memory()
                    .write_obj(VIRTIO_BLK_S_IOERR as u8, request.status_addr)
                    .map_err(Error::RequestStatus)?;

                // If no asynchronous operation has been submitted, we can
                // simply return the used descriptor.
                queue
                    .add_used(
                        desc_chain.memory(),
                        desc_chain.head_index(),
                        used_len(request.request_type, 0),
                    )
                    .map_err(Error::QueueAddUsed)?;
                queue
                    .enable_notification(self.mem.memory().deref())
                    .map_err(Error::QueueEnableNotification)?;
                continue;
            }

            if let Some(rate_limiter) = &mut self.rate_limiter {
                // If limiter.consume() fails it means there is no more TokenType::Ops
                // budget and rate limiting is in effect.
                let rate_limit_reached = rate_limiter.consume(1, TokenType::Ops);
                if rate_limit_reached != BucketReduction::Success {
                    // Stop processing the queue and return this descriptor chain to the
                    // avail ring, for later processing.
                    queue.go_to_previous_position();
                    limit_by_ops = Wrapping(1);
                    self.rate_limited
                        .call_once(|| info!("{} block ops ratelimit fired", self.id));
                    break;
                }
                // Exercise the rate limiter only if this request is of data transfer type.
                if request.request_type == RequestType::In
                    || request.request_type == RequestType::Out
                {
                    let mut bytes = Wrapping(0);
                    for (_, data_len) in &request.data_descriptors {
                        bytes += Wrapping(*data_len as u64);
                    }

                    // If limiter.consume() fails it means there is no more TokenType::Bytes
                    // budget and rate limiting is in effect.
                    let rate_limit_reached = rate_limiter.consume(bytes.0, TokenType::Bytes);
                    match rate_limit_reached {
                        BucketReduction::Failure => {
                            // Revert the OPS consume().
                            rate_limiter.manual_replenish(1, TokenType::Ops);
                            // Stop processing the queue and return this descriptor chain to the
                            // avail ring, for later processing.
                            queue.go_to_previous_position();
                            limit_by_bytes = Wrapping(1);
                            self.rate_limited
                                .call_once(|| info!("{} block bw ratelimit fired", self.id));
                            break;
                        }
                        BucketReduction::OverConsumption(_r) => {
                            // The oversized request consumed its debt and armed the
                            // limiter timer. It must still be submitted once; dropping
                            // it here would advance avail without adding a used entry.
                            limit_by_bytes = Wrapping(1);
                            self.rate_limited
                                .call_once(|| info!("{} block bw ratelimit fired", self.id));
                        }
                        BucketReduction::Success => {}
                    }
                };
            }

            request.set_writeback(self.writeback.load(Ordering::Acquire));

            let submitted = match request.execute_async(
                desc_chain.memory(),
                self.disk_nsectors,
                self.disk_image.as_mut(),
                &self.disk_image_id,
                desc_chain.head_index() as u64,
            ) {
                Ok(submitted) => submitted,
                Err(
                    e @ (block_util::ExecuteError::BadRequest(_)
                    | block_util::ExecuteError::GetHostAddress(_)
                    | block_util::ExecuteError::Write(_)
                    | block_util::ExecuteError::Unsupported(_)
                    | block_util::ExecuteError::TemporaryBufferAllocation(_)
                    | block_util::ExecuteError::AsyncDiscard(
                        block_util::async_io::AsyncIoError::DiscardRejected(_),
                    )),
                ) => {
                    error!("Rejected block request: {:?}", e);
                    request
                        .complete_async(0)
                        .map_err(Error::RequestCompleting)?;
                    desc_chain
                        .memory()
                        .write_obj(e.status() as u8, request.status_addr)
                        .map_err(Error::RequestStatus)?;
                    queue
                        .add_used(
                            desc_chain.memory(),
                            desc_chain.head_index(),
                            used_len(request.request_type, 0),
                        )
                        .map_err(Error::QueueAddUsed)?;
                    queue
                        .enable_notification(self.mem.memory().deref())
                        .map_err(Error::QueueEnableNotification)?;
                    continue;
                }
                Err(e) => return Err(Error::RequestExecuting(e)),
            };
            if submitted {
                self.request_list.insert(desc_chain.head_index(), request);
            } else {
                desc_chain
                    .memory()
                    .write_obj(VIRTIO_BLK_S_OK as u8, request.status_addr)
                    .map_err(Error::RequestStatus)?;

                // If no asynchronous operation has been submitted, we can
                // simply return the used descriptor. GetDeviceId writes the ID
                // bytes as well as the status byte.
                queue
                    .add_used(
                        desc_chain.memory(),
                        desc_chain.head_index(),
                        used_len(request.request_type, self.disk_image_id.len() as u32),
                    )
                    .map_err(Error::QueueAddUsed)?;
                queue
                    .enable_notification(self.mem.memory().deref())
                    .map_err(Error::QueueEnableNotification)?;
            }
        }

        self.counters
            .limit_by_bytes
            .fetch_add(limit_by_bytes.0, Ordering::AcqRel);
        self.counters
            .limit_by_ops
            .fetch_add(limit_by_ops.0, Ordering::AcqRel);
        Ok(())
    }

    fn try_signal_used_queue(&mut self) -> result::Result<(), EpollHelperError> {
        if self
            .queue
            .needs_notification(self.mem.memory().deref())
            .map_err(|e| {
                EpollHelperError::HandleEvent(anyhow!(
                    "Failed to check needs_notification: {:?}",
                    e
                ))
            })?
        {
            self.signal_used_queue().map_err(|e| {
                EpollHelperError::HandleEvent(anyhow!("Failed to signal used queue: {:?}", e))
            })?;
        }

        Ok(())
    }

    fn process_queue_submit_and_signal(&mut self) -> result::Result<(), EpollHelperError> {
        self.process_queue_submit().map_err(|e| {
            EpollHelperError::HandleEvent(anyhow!("Failed to process queue (submit): {:?}", e))
        })?;

        self.try_signal_used_queue()
    }

    fn process_queue_complete(&mut self) -> Result<()> {
        let queue = &mut self.queue;

        let mem = self.mem.memory();
        let mut read_bytes = Wrapping(0);
        let mut write_bytes = Wrapping(0);
        let mut read_ops = Wrapping(0);
        let mut write_ops = Wrapping(0);

        let completion_list = self.disk_image.complete();
        for (user_data, result) in completion_list {
            let desc_index = user_data as u16;
            let mut request = self
                .request_list
                .remove(&desc_index)
                .ok_or(Error::MissingEntryRequestList)?;
            request
                .complete_async(result.max(0) as usize)
                .map_err(Error::RequestCompleting)?;

            let expected_data_bytes = request
                .data_descriptors
                .iter()
                .fold(0u32, |total, (_, len)| total.saturating_add(*len));
            let (mut status, len) = async_completion_status_and_used_len(
                request.request_type,
                result,
                expected_data_bytes,
            );

            if status == VIRTIO_BLK_S_OK {
                match request.request_type {
                    RequestType::In => {
                        read_bytes += Wrapping(result as u64);
                        read_ops += Wrapping(1);
                    }
                    RequestType::Out => {
                        if !request.writeback {
                            if let Err(e) = self.disk_image.fsync(None) {
                                error!("Request flush failed: {:x?} {:?}", request, e);
                                status = VIRTIO_BLK_S_IOERR;
                            }
                        }
                        if status == VIRTIO_BLK_S_OK {
                            write_bytes += Wrapping(result as u64);
                            write_ops += Wrapping(1);
                        }
                    }
                    _ => {}
                }
            } else if result < 0 {
                error!(
                    "Request failed: {:x?} {:?}",
                    request,
                    io::Error::from_raw_os_error(-result)
                );
            } else {
                error!(
                    "Request completed partially: {:x?}, expected {} bytes, completed {}",
                    request, expected_data_bytes, result
                );
            }

            mem.write_obj(status as u8, request.status_addr)
                .map_err(Error::RequestStatus)?;

            queue
                .add_used(mem.deref(), desc_index, len)
                .map_err(Error::QueueAddUsed)?;
            queue
                .enable_notification(mem.deref())
                .map_err(Error::QueueEnableNotification)?;
        }

        self.counters
            .write_bytes
            .fetch_add(write_bytes.0, Ordering::AcqRel);
        self.counters
            .write_ops
            .fetch_add(write_ops.0, Ordering::AcqRel);

        self.counters
            .read_bytes
            .fetch_add(read_bytes.0, Ordering::AcqRel);
        self.counters
            .read_ops
            .fetch_add(read_ops.0, Ordering::AcqRel);

        Ok(())
    }

    fn signal_used_queue(&self) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(self.queue_index))
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
        helper.add_event(self.disk_image.notifier().as_raw_fd(), COMPLETION_EVENT)?;
        if let Some(rate_limiter) = &self.rate_limiter {
            helper.add_event(rate_limiter.as_raw_fd(), RATE_LIMITER_EVENT)?;
        }
        helper.run(paused, paused_sync, self)?;

        Ok(())
    }
}

impl EpollHelperHandler for BlockEpollHandler {
    // Drain inflight IO and (in writeback mode) issue a synchronous
    // FLUSH before the pause barrier. Any failure is reported via
    // `drain_failed` rather than `Err`, because returning Err here
    // would skip `paused_sync.wait()` and deadlock the pausing thread.
    fn before_pause(&mut self, _helper: &mut EpollHelper) -> result::Result<(), EpollHelperError> {
        let deadline = Instant::now() + PAUSE_DRAIN_TIMEOUT;
        let efd = self.disk_image.notifier().as_raw_fd();

        while !self.request_list.is_empty() {
            if Instant::now() >= deadline {
                error!(
                    "virtio-blk {}: pause drain timed out, {} IO still inflight",
                    self.id,
                    self.request_list.len()
                );
                self.drain_failed.store(true, Ordering::SeqCst);
                return Ok(());
            }

            // Wait briefly for the backend completion notifier. We do
            // not consume the eventfd counter here; `process_queue_complete`
            // pulls completions directly from the AsyncIo queue, which
            // is the source of truth.
            let mut pfd = libc::pollfd {
                fd: efd,
                events: libc::POLLIN,
                revents: 0,
            };
            // SAFETY: pfd is initialised; libc::poll only writes revents.
            let _ = unsafe { libc::poll(&mut pfd, 1, PAUSE_DRAIN_POLL_MS) };

            if let Err(e) = self.process_queue_complete() {
                error!(
                    "virtio-blk {}: drain process_queue_complete failed: {:?}",
                    self.id, e
                );
                self.drain_failed.store(true, Ordering::SeqCst);
                return Ok(());
            }
        }

        // Make the freshly-added used entries visible to the guest.
        // Failures here only affect interrupt delivery (state is
        // already correct in memory); log and continue.
        if let Err(e) = self.try_signal_used_queue() {
            warn!(
                "virtio-blk {}: pre-pause used-queue signal failed: {:?}",
                self.id, e
            );
        }

        // Write barrier: once data is fully drained, force a synchronous
        // FLUSH so the backend's completed writes are durable before the
        // snapshot is taken. Skipped in writethrough because each write
        // already implies a flush.
        if self.writeback.load(Ordering::Acquire) {
            if let Err(e) = self.disk_image.fsync(None) {
                error!("virtio-blk {}: pause-time flush failed: {:?}", self.id, e);
                self.drain_failed.store(true, Ordering::SeqCst);
            }
        }

        Ok(())
    }

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

                let rate_limit_reached =
                    self.rate_limiter.as_ref().map_or(false, |r| r.is_blocked());

                // Process the queue only when the rate limit is not reached
                if !rate_limit_reached {
                    self.process_queue_submit_and_signal()?
                }
            }
            COMPLETION_EVENT => {
                self.disk_image.notifier().read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get queue event: {:?}", e))
                })?;

                self.process_queue_complete().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to process queue (complete): {:?}",
                        e
                    ))
                })?;

                let rate_limit_reached =
                    self.rate_limiter.as_ref().map_or(false, |r| r.is_blocked());

                // Process the queue only when the rate limit is not reached
                if !rate_limit_reached {
                    self.process_queue_submit().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to process queue (submit): {:?}",
                            e
                        ))
                    })?;
                }
                self.try_signal_used_queue()?;
            }
            RATE_LIMITER_EVENT => {
                if let Some(rate_limiter) = &mut self.rate_limiter {
                    // Upon rate limiter event, call the rate limiter handler
                    // and restart processing the queue.
                    rate_limiter.event_handler().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to process rate limiter event: {:?}",
                            e
                        ))
                    })?;

                    self.process_queue_submit_and_signal()?
                } else {
                    return Err(EpollHelperError::HandleEvent(anyhow!(
                        "Unexpected 'RATE_LIMITER_EVENT' when rate_limiter is not enabled."
                    )));
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

/// Virtio device for exposing block level read/write operations on a host file.
pub struct Block {
    common: VirtioCommon,
    id: String,
    disk_image: Box<dyn DiskFile>,
    disk_path: PathBuf,
    disk_nsectors: u64,
    config: VirtioBlockConfig,
    writeback: Arc<AtomicBool>,
    counters: BlockCounters,
    seccomp_action: SeccompAction,
    rate_limiter_config: Option<RateLimiterConfig>,
    exit_evt: EventFd,
    read_only: bool,
    /// Shared with each per-queue `BlockEpollHandler`. Set by an epoll
    /// thread when its pause-time drain or flush could not complete
    /// within `PAUSE_DRAIN_TIMEOUT`; checked by `pause()` after the
    /// pause barrier so the upper layer can abort the snapshot.
    drain_failed: Arc<AtomicBool>,
}

#[derive(Serialize, Deserialize)]
pub struct BlockState {
    pub disk_path: String,
    pub disk_nsectors: u64,
    pub avail_features: u64,
    pub acked_features: u64,
    pub config: VirtioBlockConfig,
}

impl Block {
    /// Create a new virtio block device that operates on the given file.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        mut disk_image: Box<dyn DiskFile>,
        disk_path: PathBuf,
        read_only: bool,
        iommu: bool,
        num_queues: usize,
        queue_size: u16,
        seccomp_action: SeccompAction,
        rate_limiter_config: Option<RateLimiterConfig>,
        exit_evt: EventFd,
        state: Option<BlockState>,
    ) -> io::Result<Self> {
        let discard_supported = !read_only && disk_image.supports_discard();
        let (disk_nsectors, avail_features, acked_features, config) = if let Some(state) = state {
            if state.avail_features & (1 << VIRTIO_BLK_F_DISCARD) != 0 {
                if !discard_supported {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidInput,
                        "saved DISCARD capability is unsupported by this backend",
                    ));
                }
                // Never silently shrink a saved guest config: reject a restore
                // whose advertised maximum this backend can no longer satisfy.
                let saved_max = state.config.max_discard_sectors;
                let backend_max = disk_image.max_discard_sectors();
                if saved_max > backend_max {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidInput,
                        format!(
                            "saved DISCARD maximum {saved_max} exceeds this backend's maximum {backend_max}"
                        ),
                    ));
                }
            }
            debug!("Restoring virtio-block {}", id);
            (
                state.disk_nsectors,
                state.avail_features,
                state.acked_features,
                state.config,
            )
        } else {
            let disk_size = disk_image.size().map_err(|e| {
                io::Error::new(
                    io::ErrorKind::Other,
                    format!("Failed getting disk size: {}", e),
                )
            })?;
            if disk_size % SECTOR_SIZE != 0 {
                warn!(
                    "Disk size {} is not a multiple of sector size {}; \
                 the remainder will not be visible to the guest.",
                    disk_size, SECTOR_SIZE
                );
            }

            let mut avail_features = (1u64 << VIRTIO_F_VERSION_1)
                | (1u64 << VIRTIO_BLK_F_FLUSH)
                | (1u64 << VIRTIO_BLK_F_CONFIG_WCE)
                | (1u64 << VIRTIO_BLK_F_BLK_SIZE)
                | (1u64 << VIRTIO_BLK_F_TOPOLOGY)
                | (1u64 << VIRTIO_BLK_F_SEG_MAX)
                | (1u64 << VIRTIO_RING_F_EVENT_IDX)
                | (1u64 << VIRTIO_RING_F_INDIRECT_DESC);

            if iommu {
                avail_features |= 1u64 << VIRTIO_F_IOMMU_PLATFORM;
            }

            if read_only {
                avail_features |= 1u64 << VIRTIO_BLK_F_RO;
            }

            let topology = disk_image.topology();
            info!("Disk topology: {:?}", topology);

            let logical_block_size = if topology.logical_block_size > 512 {
                topology.logical_block_size
            } else {
                512
            };

            // Calculate the exponent that maps physical block to logical block
            let mut physical_block_exp = 0;
            let mut size = logical_block_size;
            while size < topology.physical_block_size {
                physical_block_exp += 1;
                size <<= 1;
            }

            let disk_nsectors = disk_size / SECTOR_SIZE;
            let mut config = VirtioBlockConfig {
                capacity: disk_nsectors,
                writeback: 1,
                blk_size: topology.logical_block_size as u32,
                physical_block_exp,
                min_io_size: (topology.minimum_io_size / logical_block_size) as u16,
                opt_io_size: (topology.optimal_io_size / logical_block_size) as u32,
                seg_max: (queue_size - MINIMUM_BLOCK_QUEUE_SIZE) as u32,
                ..Default::default()
            };

            if discard_supported {
                avail_features |= 1 << VIRTIO_BLK_F_DISCARD;
                // Advertise what this backend really accepts so a guest can
                // never submit a range the backend would reject.
                config.max_discard_sectors = disk_image.max_discard_sectors();
                config.max_discard_seg = 1;
                config.discard_sector_alignment = disk_image.discard_sector_alignment();
            }
            if num_queues > 1 {
                avail_features |= 1u64 << VIRTIO_BLK_F_MQ;
                config.num_queues = num_queues as u16;
            }

            (disk_nsectors, avail_features, 0, config)
        };

        Ok(Block {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Block as u32,
                avail_features,
                acked_features,
                paused_sync: Some(Arc::new(Barrier::new(num_queues + 1))),
                queue_sizes: vec![queue_size; num_queues],
                min_queues: 1,
                ..Default::default()
            },
            id,
            disk_image,
            disk_path,
            disk_nsectors,
            config,
            writeback: Arc::new(AtomicBool::new(true)),
            counters: BlockCounters::default(),
            seccomp_action,
            rate_limiter_config,
            exit_evt,
            read_only,
            drain_failed: Arc::new(AtomicBool::new(false)),
        })
    }

    fn state(&self) -> BlockState {
        BlockState {
            disk_path: self.disk_path.to_str().unwrap().to_owned(),
            disk_nsectors: self.disk_nsectors,
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: self.config,
        }
    }

    fn update_writeback(&mut self) {
        // Use writeback from config if VIRTIO_BLK_F_CONFIG_WCE
        let writeback = if self.common.feature_acked(VIRTIO_BLK_F_CONFIG_WCE.into()) {
            self.config.writeback == 1
        } else {
            // Else check if VIRTIO_BLK_F_FLUSH negotiated
            self.common.feature_acked(VIRTIO_BLK_F_FLUSH.into())
        };

        info!(
            "Changing cache mode to {}",
            if writeback {
                "writeback"
            } else {
                "writethrough"
            }
        );
        self.writeback.store(writeback, Ordering::Release);
    }

    #[cfg(fuzzing)]
    pub fn wait_for_epoll_threads(&mut self) {
        self.common.wait_for_epoll_threads();
    }

    fn spawn_queue_threads(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
        epoll_threads: &mut Vec<JoinHandle<()>>,
    ) -> ActivateResult {
        let disk_image_id = build_disk_image_id(&self.disk_path);
        let event_idx = self.common.feature_acked(VIRTIO_RING_F_EVENT_IDX.into());

        for i in 0..queues.len() {
            let (_, mut queue, queue_evt) = queues.remove(0);
            queue.set_event_idx(event_idx);

            let queue_size = queue.size();
            let (kill_evt, pause_evt) = self.common.dup_eventfds();

            let rate_limiter: Option<RateLimiter> = self
                .rate_limiter_config
                .map(RateLimiterConfig::try_into)
                .transpose()
                .map_err(ActivateError::CreateRateLimiter)?;

            let mut disk_image = self
                .disk_image
                .new_async_io(queue_size as u32)
                .map_err(|e| {
                    error!("failed to create new AsyncIo: {}", e);
                    ActivateError::BadActivate
                })?;
            // The runner owns its fd, so it is created before the backend moves
            // into the handler and before any seccomp filter is installed.
            let discard_worker = disk_image.start_discard_worker().map_err(|e| {
                error!("failed to start discard worker: {}", e);
                ActivateError::BadActivate
            })?;

            let mut handler = BlockEpollHandler {
                queue_index: i as u16,
                queue,
                mem: mem.clone(),
                disk_image,
                disk_nsectors: self.disk_nsectors,
                interrupt_cb: interrupt_cb.clone(),
                disk_image_id: disk_image_id.clone(),
                kill_evt,
                pause_evt,
                writeback: self.writeback.clone(),
                counters: self.counters.clone(),
                queue_evt,
                request_list: HashMap::with_capacity(queue_size.into()),
                rate_limiter,
                access_platform: self.common.access_platform.clone(),
                read_only: self.read_only,
                discard: self.common.acked_features & (1 << VIRTIO_BLK_F_DISCARD) != 0,
                rate_limited: std::sync::Once::new(),
                id: self.id.clone(),
                drain_failed: self.drain_failed.clone(),
            };

            let paused = self.common.paused.clone();
            let paused_sync = self.common.paused_sync.clone();

            // Queue thread first: reset joins in order, so dropping the queue
            // backend (and its sender) happens before the worker is joined.
            spawn_virtio_thread(
                &format!("{}_q{}", self.id.clone(), i),
                &self.seccomp_action,
                Thread::VirtioBlock,
                epoll_threads,
                &self.exit_evt,
                move || handler.run(paused, paused_sync.unwrap()),
            )?;

            if let Some(runner) = discard_worker {
                spawn_virtio_thread(
                    &format!("{}_q{}_discard", self.id.clone(), i),
                    &self.seccomp_action,
                    Thread::VirtioBlock,
                    epoll_threads,
                    &self.exit_evt,
                    move || {
                        runner().map_err(|e| {
                            EpollHelperError::HandleEvent(anyhow!("discard worker failed: {e}"))
                        })
                    },
                )?;
            }
        }

        Ok(())
    }
}

impl Drop for Block {
    fn drop(&mut self) {
        // A paused queue is parked and would never observe kill_evt. resume()
        // is safe on a never-activated device (unlike reset(), which unwraps
        // interrupt_cb), so use it to guarantee both threads can exit before
        // the backing file is reused.
        let _ = self.common.resume();
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
        self.common.wait_for_epoll_threads();
    }
}

impl VirtioDevice for Block {
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
        self.update_writeback();
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        self.update_writeback();

        let mut epoll_threads = Vec::new();
        if let Err(e) = self.spawn_queue_threads(mem, interrupt_cb, queues, &mut epoll_threads) {
            // Roll back a partially started activation. A queue thread spawned
            // while the device is paused parks before its epoll loop and would
            // never observe kill_evt. Its handles are still local here, so
            // `common.resume()` cannot wake them: clear the paused state and
            // wake the local handles directly before killing and joining.
            // Joining a queue thread drops its backend, which closes its
            // discard worker so that thread exits before the failure returns.
            self.common.paused.store(false, Ordering::SeqCst);
            if let Some(kill_evt) = &self.common.kill_evt {
                let _ = kill_evt.write(1);
            }
            for thread in &epoll_threads {
                thread.thread().unpark();
            }
            for thread in epoll_threads.drain(..) {
                if let Err(e) = thread.join() {
                    error!("Error joining thread after failed activation: {:?}", e);
                }
            }
            return Err(e);
        }

        self.common.epoll_threads = Some(epoll_threads);
        event!("virtio-device", "activated", "id", &self.id);

        Ok(())
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        let result = self.common.reset();
        event!("virtio-device", "reset", "id", &self.id);
        result
    }

    fn counters(&self) -> Option<HashMap<&'static str, Wrapping<u64>>> {
        let mut counters = HashMap::new();

        counters.insert(
            "read_bytes",
            Wrapping(self.counters.read_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "write_bytes",
            Wrapping(self.counters.write_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "read_ops",
            Wrapping(self.counters.read_ops.load(Ordering::Acquire)),
        );
        counters.insert(
            "write_ops",
            Wrapping(self.counters.write_ops.load(Ordering::Acquire)),
        );
        counters.insert(
            "limit_by_bytes",
            Wrapping(self.counters.limit_by_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "limit_by_ops",
            Wrapping(self.counters.limit_by_ops.load(Ordering::Acquire)),
        );

        Some(counters)
    }

    fn set_access_platform(&mut self, access_platform: Arc<dyn AccessPlatform>) {
        self.common.set_access_platform(access_platform)
    }
}

impl Pausable for Block {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()?;

        // After common.pause() returns, every per-queue epoll thread has
        // executed its `before_pause` hook and crossed the barrier. Any
        // queue that failed to drain inflight IO or to issue the
        // pause-time flush within `PAUSE_DRAIN_TIMEOUT` will have set
        // this flag. Aborting pause here propagates upward and prevents
        // a snapshot from being captured in an inconsistent state.
        if self.drain_failed.swap(false, Ordering::SeqCst) {
            return Err(MigratableError::Pause(anyhow!(
                "Failed to drain inflight IO for block device {} within timeout",
                self.id
            )));
        }

        Ok(())
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()
    }
}

impl Snapshottable for Block {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id(), &self.state())
    }
}
impl Transportable for Block {}
impl Migratable for Block {}

#[cfg(test)]
mod tests {
    use super::*;
    use block_util::async_io::{AsyncIoError, AsyncIoResult};
    use std::collections::VecDeque;
    use std::sync::atomic::AtomicUsize;
    use std::sync::mpsc;
    use std::sync::Mutex;
    use vm_memory::GuestAddress;
    use vm_virtio::queue::testing::VirtQueue as GuestQ;

    #[test]
    fn test_discard_completion_accepts_one_byte_status_at_memory_end() {
        use std::io::Write;
        use vm_memory::GuestAddress;
        use vm_virtio::queue::testing::VirtQueue as GuestQ;
        struct NoopInterrupt;
        impl VirtioInterrupt for NoopInterrupt {
            fn trigger(&self, _: VirtioInterruptType) -> io::Result<()> {
                Ok(())
            }
        }
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
        let guest_queue = GuestQ::new(GuestAddress(0x1000), &memory, 16);
        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let mut file = temp.as_file().try_clone().unwrap();
        file.write_all(&[0xa5; 4096]).unwrap();
        let mut disk = block_util::raw_sync::RawFileSync::new(file.as_raw_fd());
        disk.discard(0, 4096, 0).unwrap();
        let request = Request {
            request_type: RequestType::Discard,
            sector: 0,
            data_descriptors: vec![(GuestAddress(0x3000), 16)],
            status_addr: GuestAddress(0xffff),
            writeback: true,
            aligned_operations: Vec::new(),
        };
        memory.write_obj(0xffu8, request.status_addr).unwrap();
        let mut handler = BlockEpollHandler {
            queue_index: 0,
            queue: guest_queue.create_queue(),
            mem: GuestMemoryAtomic::new(memory.clone()),
            disk_image: Box::new(disk),
            disk_nsectors: 8,
            interrupt_cb: Arc::new(NoopInterrupt),
            disk_image_id: vec![0; 20],
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
            writeback: Arc::new(AtomicBool::new(true)),
            counters: BlockCounters::default(),
            queue_evt: EventFd::new(0).unwrap(),
            request_list: HashMap::from([(0, request)]),
            rate_limiter: None,
            access_platform: None,
            read_only: false,
            discard: true,
            rate_limited: std::sync::Once::new(),
            id: "status-test".to_string(),
            drain_failed: Arc::new(AtomicBool::new(false)),
        };
        handler.process_queue_complete().unwrap();
        assert_eq!(
            memory.read_obj::<u8>(GuestAddress(0xffff)).unwrap(),
            VIRTIO_BLK_S_OK as u8
        );
        assert_eq!(guest_queue.used.idx.get(), 1);
        assert!(handler.request_list.is_empty());
    }

    #[test]
    fn test_discard_parser_requires_readable_range_descriptor() {
        use vm_memory::GuestAddress;
        use vm_virtio::queue::testing::VirtQueue as GuestQ;
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
        memory.write_obj(11u32, GuestAddress(0x3000)).unwrap();
        let guest_queue = GuestQ::new(GuestAddress(0x1000), &memory, 16);
        guest_queue.dtable[0].set(0x3000, 16, 1, 1);
        guest_queue.dtable[1].set(0x4000, 16, 3, 2);
        guest_queue.dtable[2].set(0x5000, 1, 2, 0);
        guest_queue.avail.ring[0].set(0);
        guest_queue.avail.idx.set(1);
        let atomic = GuestMemoryAtomic::new(memory.clone());
        let mut queue = guest_queue.create_queue();
        let mut chain = queue.pop_descriptor_chain(atomic.memory()).unwrap();
        assert!(matches!(
            Request::parse(&mut chain, None),
            Err(block_util::Error::UnexpectedWriteOnlyDescriptor)
        ));
    }

    struct DiscardTestDisk(bool);
    impl DiskFile for DiscardTestDisk {
        fn size(&mut self) -> block_util::async_io::DiskFileResult<u64> {
            Ok(64 * 1024 * 1024)
        }
        fn new_async_io(&self, _: u32) -> block_util::async_io::DiskFileResult<Box<dyn AsyncIo>> {
            Err(block_util::async_io::DiskFileError::NewAsyncIo(
                io::Error::from(io::ErrorKind::Unsupported),
            ))
        }
        fn supports_discard(&self) -> bool {
            self.0
        }
    }
    fn discard_test_device(
        supported: bool,
        readonly: bool,
        state: Option<BlockState>,
    ) -> io::Result<Block> {
        Block::new(
            "discard-test".to_owned(),
            Box::new(DiscardTestDisk(supported)),
            PathBuf::from("unused-test-path"),
            readonly,
            false,
            1,
            128,
            SeccompAction::Allow,
            None,
            EventFd::new(libc::EFD_NONBLOCK).unwrap(),
            state,
        )
    }
    #[test]
    fn test_discard_capability_is_backend_and_readonly_gated() {
        let block = discard_test_device(true, false, None).unwrap();
        assert_ne!(block.common.avail_features & (1 << VIRTIO_BLK_F_DISCARD), 0);
        let seg = block.config.max_discard_seg;
        assert_eq!(seg, 1);
        for (supported, readonly) in [(false, false), (true, true)] {
            let block = discard_test_device(supported, readonly, None).unwrap();
            assert_eq!(block.common.avail_features & (1 << VIRTIO_BLK_F_DISCARD), 0);
        }
    }
    #[test]
    fn test_discard_restore_rejects_incompatible_backend() {
        let block = discard_test_device(true, false, None).unwrap();
        assert!(discard_test_device(false, false, Some(block.state())).is_err());
    }

    struct MaxTestDisk {
        max_sectors: u32,
    }

    impl DiskFile for MaxTestDisk {
        fn size(&mut self) -> block_util::async_io::DiskFileResult<u64> {
            Ok(64 * 1024 * 1024)
        }
        fn new_async_io(&self, _: u32) -> block_util::async_io::DiskFileResult<Box<dyn AsyncIo>> {
            Err(block_util::async_io::DiskFileError::NewAsyncIo(
                io::Error::from(io::ErrorKind::Unsupported),
            ))
        }
        fn supports_discard(&self) -> bool {
            true
        }
        fn max_discard_sectors(&self) -> u32 {
            self.max_sectors
        }
    }

    fn max_test_device(max_sectors: u32, state: Option<BlockState>) -> io::Result<Block> {
        Block::new(
            "max-test".to_owned(),
            Box::new(MaxTestDisk { max_sectors }),
            PathBuf::from("unused-test-path"),
            false,
            false,
            1,
            128,
            SeccompAction::Allow,
            None,
            EventFd::new(libc::EFD_NONBLOCK).unwrap(),
            state,
        )
    }

    #[test]
    fn test_discard_max_is_advertised_from_backend() {
        let block = max_test_device(128, None).unwrap();
        assert_ne!(block.common.avail_features & (1 << VIRTIO_BLK_F_DISCARD), 0);
        // Copy out of the packed config before taking a reference.
        let advertised = block.config.max_discard_sectors;
        assert_eq!(advertised, 128);
    }

    #[test]
    fn test_discard_restore_rejects_saved_max_above_backend_max() {
        let block = max_test_device(4096, None).unwrap();
        assert!(
            max_test_device(1024, Some(block.state())).is_err(),
            "a saved max larger than the current backend must fail explicitly"
        );

        // A saved max the backend can still satisfy is preserved untouched.
        let restored = max_test_device(4096, Some(block.state())).unwrap();
        let restored_max = restored.config.max_discard_sectors;
        assert_eq!(restored_max, 4096);
    }

    #[test]
    fn test_block_drop_without_activation_is_safe() {
        drop(discard_test_device(true, false, None).unwrap());
    }

    // Backend double that exercises the real activation lifecycle. Each
    // successful `new_async_io` yields a queue backend whose discard worker
    // blocks until that backend is dropped (mirroring the real worker, which
    // only returns after its sender is dropped). `fail_async_io_at` makes the
    // n-th `new_async_io` call fail, producing a partial activation.
    struct LifecycleDisk {
        async_io_calls: AtomicUsize,
        fail_async_io_at: usize,
        workers_started: Arc<AtomicUsize>,
        workers_stopped: Arc<AtomicUsize>,
    }

    impl DiskFile for LifecycleDisk {
        fn size(&mut self) -> block_util::async_io::DiskFileResult<u64> {
            Ok(64 * 1024 * 1024)
        }
        fn new_async_io(&self, _: u32) -> block_util::async_io::DiskFileResult<Box<dyn AsyncIo>> {
            let call = self.async_io_calls.fetch_add(1, Ordering::SeqCst);
            if call == self.fail_async_io_at {
                return Err(block_util::async_io::DiskFileError::NewAsyncIo(
                    io::Error::from_raw_os_error(libc::EIO),
                ));
            }
            let (close_tx, close_rx) = mpsc::channel();
            Ok(Box::new(LifecycleAsyncIo {
                notifier: EventFd::new(libc::EFD_NONBLOCK).unwrap(),
                _close_tx: close_tx,
                close_rx: Some(close_rx),
                workers_started: self.workers_started.clone(),
                workers_stopped: self.workers_stopped.clone(),
            }))
        }
        fn supports_discard(&self) -> bool {
            true
        }
    }

    struct LifecycleAsyncIo {
        notifier: EventFd,
        // Dropped together with the backend, which is how the worker learns it
        // must return (the real sender closes the same way).
        _close_tx: mpsc::Sender<()>,
        close_rx: Option<mpsc::Receiver<()>>,
        workers_started: Arc<AtomicUsize>,
        workers_stopped: Arc<AtomicUsize>,
    }

    impl AsyncIo for LifecycleAsyncIo {
        fn notifier(&self) -> &EventFd {
            &self.notifier
        }
        fn read_vectored(
            &mut self,
            _: libc::off_t,
            _: Vec<libc::iovec>,
            _: u64,
        ) -> AsyncIoResult<()> {
            panic!("unexpected read in lifecycle test")
        }
        fn write_vectored(
            &mut self,
            _: libc::off_t,
            _: Vec<libc::iovec>,
            _: u64,
        ) -> AsyncIoResult<()> {
            panic!("unexpected write in lifecycle test")
        }
        fn fsync(&mut self, _: Option<u64>) -> AsyncIoResult<()> {
            panic!("unexpected fsync in lifecycle test")
        }
        fn start_discard_worker(
            &mut self,
        ) -> io::Result<Option<Box<dyn FnOnce() -> io::Result<()> + Send>>> {
            let close_rx = self.close_rx.take().expect("worker started twice");
            let started = self.workers_started.clone();
            let stopped = self.workers_stopped.clone();
            Ok(Some(Box::new(move || {
                started.fetch_add(1, Ordering::SeqCst);
                let _ = close_rx.recv();
                stopped.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })))
        }
        fn complete(&mut self) -> Vec<(u64, i32)> {
            Vec::new()
        }
    }

    fn lifecycle_queues(memory: &GuestMemoryMmap, count: usize) -> Vec<(usize, Queue, EventFd)> {
        (0..count)
            .map(|i| {
                let guest = GuestQ::new(GuestAddress(0x1000 + (i as u64) * 0x1000), memory, 16);
                (
                    i,
                    guest.create_queue(),
                    EventFd::new(libc::EFD_NONBLOCK).unwrap(),
                )
            })
            .collect()
    }

    fn lifecycle_device(
        num_queues: usize,
        fail_async_io_at: usize,
        started: Arc<AtomicUsize>,
        stopped: Arc<AtomicUsize>,
    ) -> Block {
        Block::new(
            "lifecycle-test".to_owned(),
            Box::new(LifecycleDisk {
                async_io_calls: AtomicUsize::new(0),
                fail_async_io_at,
                workers_started: started,
                workers_stopped: stopped,
            }),
            PathBuf::from("unused-test-path"),
            false,
            false,
            num_queues,
            16,
            SeccompAction::Allow,
            None,
            EventFd::new(libc::EFD_NONBLOCK).unwrap(),
            None,
        )
        .unwrap()
    }

    // A restore can pause a device before it is activated, so every spawned
    // queue thread parks before its epoll loop. If activation then fails
    // partway (after a queue thread and its discard worker already started),
    // rollback must still wake and join those local threads instead of
    // deadlocking on a parked thread that never sees kill_evt.
    #[test]
    fn test_paused_partial_activation_failure_joins_started_threads() {
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
        let started = Arc::new(AtomicUsize::new(0));
        let stopped = Arc::new(AtomicUsize::new(0));
        let mut block = lifecycle_device(2, 1, started.clone(), stopped.clone());

        block.pause().unwrap();
        assert!(block.common.paused.load(Ordering::SeqCst));

        let mem = GuestMemoryAtomic::new(memory.clone());
        let queues = lifecycle_queues(&memory, 2);
        let (tx, rx) = mpsc::channel();
        // Run activation off the test thread so a cleanup deadlock fails the
        // test after a bounded wait instead of hanging the suite.
        let activation = std::thread::spawn(move || {
            let failed = block
                .activate(mem, Arc::new(TestInterrupt), queues)
                .is_err();
            tx.send(failed).unwrap();
            block
        });

        let failed = rx
            .recv_timeout(Duration::from_secs(5))
            .expect("partial activation rollback blocked on a paused queue thread");
        assert!(
            failed,
            "the second queue's backend failure must fail activation"
        );

        // Dropping the (now un-activated) device must also return promptly.
        drop(activation.join().unwrap());

        assert_eq!(
            started.load(Ordering::SeqCst),
            1,
            "the first queue's discard worker must have started before the failure"
        );
        assert_eq!(
            stopped.load(Ordering::SeqCst),
            1,
            "rollback must close and join the started discard worker"
        );
    }

    // Successful activation publishes its handles into `common`, so a normal
    // drop must resume/join the queue and its delayed discard worker rather
    // than leaking either thread.
    #[test]
    fn test_drop_joins_started_queue_and_discard_worker() {
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
        let started = Arc::new(AtomicUsize::new(0));
        let stopped = Arc::new(AtomicUsize::new(0));
        let mut block = lifecycle_device(1, usize::MAX, started.clone(), stopped.clone());

        let mem = GuestMemoryAtomic::new(memory.clone());
        let queues = lifecycle_queues(&memory, 1);
        block
            .activate(mem, Arc::new(TestInterrupt), queues)
            .unwrap();

        let deadline = Instant::now() + Duration::from_secs(5);
        while started.load(Ordering::SeqCst) == 0 {
            assert!(
                Instant::now() < deadline,
                "discard worker never started after activation"
            );
            std::thread::sleep(Duration::from_millis(1));
        }

        let (tx, rx) = mpsc::channel();
        let dropper = std::thread::spawn(move || {
            drop(block);
            tx.send(()).unwrap();
        });
        rx.recv_timeout(Duration::from_secs(5))
            .expect("drop blocked joining the activated queue/worker threads");
        dropper.join().unwrap();

        assert_eq!(
            stopped.load(Ordering::SeqCst),
            1,
            "drop must stop the discard worker"
        );
    }
    #[test]
    fn test_discard_completion_reports_status_only() {
        assert_eq!(
            async_completion_status_and_used_len(RequestType::Discard, 0, 16),
            (VIRTIO_BLK_S_OK, 1)
        );
        assert_eq!(
            async_completion_status_and_used_len(RequestType::Discard, -libc::EIO, 16),
            (VIRTIO_BLK_S_IOERR, 1)
        );
    }

    #[test]
    fn test_used_len_reports_bytes_written_to_guest() {
        assert_eq!(used_len(RequestType::In, 512), 513);
        assert_eq!(used_len(RequestType::Out, 512), 1);
        assert_eq!(used_len(RequestType::Flush, 0), 1);
        assert_eq!(used_len(RequestType::GetDeviceId, 20), 21);
        assert_eq!(used_len(RequestType::Unsupported(42), 0), 1);
    }

    #[test]
    fn test_async_completion_used_len() {
        assert_eq!(
            async_completion_status_and_used_len(RequestType::In, 512, 512),
            (VIRTIO_BLK_S_OK, 513)
        );
        assert_eq!(
            async_completion_status_and_used_len(RequestType::Out, 512, 512),
            (VIRTIO_BLK_S_OK, 1)
        );
        assert_eq!(
            async_completion_status_and_used_len(RequestType::Flush, 0, 0),
            (VIRTIO_BLK_S_OK, 1)
        );
    }

    #[test]
    fn test_async_completion_errors_include_status_and_partial_read() {
        assert_eq!(
            async_completion_status_and_used_len(RequestType::In, -libc::EIO, 512),
            (VIRTIO_BLK_S_IOERR, 1)
        );
        assert_eq!(
            async_completion_status_and_used_len(RequestType::Out, -libc::EIO, 512),
            (VIRTIO_BLK_S_IOERR, 1)
        );
        assert_eq!(
            async_completion_status_and_used_len(RequestType::Flush, -libc::EIO, 0),
            (VIRTIO_BLK_S_IOERR, 1)
        );
        assert_eq!(
            async_completion_status_and_used_len(RequestType::In, 256, 512),
            (VIRTIO_BLK_S_IOERR, 257)
        );
        assert_eq!(
            async_completion_status_and_used_len(RequestType::Out, 256, 512),
            (VIRTIO_BLK_S_IOERR, 1)
        );
    }

    struct TestInterrupt;
    impl VirtioInterrupt for TestInterrupt {
        fn trigger(&self, _: VirtioInterruptType) -> io::Result<()> {
            Ok(())
        }
    }

    #[derive(Clone, Copy)]
    enum DiscardResponse {
        Complete,
        // Safely rejected before submission: no completion will ever arrive.
        Reject(i32),
        // Ambiguous failure: an SQE may already be published.
        Fatal(i32),
    }

    // Backend double for queue-handler tests. Completions are handed out by
    // `complete()`, optionally delayed by one call so a `before_pause` drain
    // has to poll more than once.
    struct FakeAsyncIo {
        notifier: EventFd,
        completions: VecDeque<(u64, i32)>,
        delay_completions: usize,
        fail_fsync: bool,
        discard_response: DiscardResponse,
        events: Arc<Mutex<Vec<&'static str>>>,
        discards: Arc<Mutex<Vec<(libc::off_t, libc::off_t)>>>,
    }

    impl FakeAsyncIo {
        fn new() -> Self {
            Self {
                notifier: EventFd::new(libc::EFD_NONBLOCK).unwrap(),
                completions: VecDeque::new(),
                delay_completions: 0,
                fail_fsync: false,
                discard_response: DiscardResponse::Complete,
                events: Arc::new(Mutex::new(Vec::new())),
                discards: Arc::new(Mutex::new(Vec::new())),
            }
        }
    }

    impl AsyncIo for FakeAsyncIo {
        fn notifier(&self) -> &EventFd {
            &self.notifier
        }

        fn read_vectored(
            &mut self,
            _: libc::off_t,
            _: Vec<libc::iovec>,
            _: u64,
        ) -> AsyncIoResult<()> {
            panic!("unexpected read in discard test")
        }

        fn write_vectored(
            &mut self,
            _: libc::off_t,
            _: Vec<libc::iovec>,
            _: u64,
        ) -> AsyncIoResult<()> {
            panic!("unexpected write in discard test")
        }

        fn fsync(&mut self, _: Option<u64>) -> AsyncIoResult<()> {
            self.events.lock().unwrap().push("fsync");
            if self.fail_fsync {
                Err(AsyncIoError::Fsync(io::Error::from_raw_os_error(libc::EIO)))
            } else {
                Ok(())
            }
        }

        fn discard(
            &mut self,
            offset: libc::off_t,
            len: libc::off_t,
            user_data: u64,
        ) -> AsyncIoResult<()> {
            self.discards.lock().unwrap().push((offset, len));
            match self.discard_response {
                DiscardResponse::Complete => {
                    self.completions.push_back((user_data, 0));
                    Ok(())
                }
                DiscardResponse::Reject(errno) => Err(AsyncIoError::DiscardRejected(
                    io::Error::from_raw_os_error(errno),
                )),
                DiscardResponse::Fatal(errno) => {
                    Err(AsyncIoError::Discard(io::Error::from_raw_os_error(errno)))
                }
            }
        }

        fn complete(&mut self) -> Vec<(u64, i32)> {
            if self.delay_completions > 0 {
                self.delay_completions -= 1;
                return Vec::new();
            }
            let completions: Vec<_> = self.completions.drain(..).collect();
            if !completions.is_empty() {
                self.events.lock().unwrap().push("complete");
            }
            completions
        }
    }

    fn test_handler(
        memory: GuestMemoryMmap,
        queue: Queue,
        disk_image: Box<dyn AsyncIo>,
        request_list: HashMap<u16, Request>,
        read_only: bool,
        discard: bool,
    ) -> BlockEpollHandler {
        BlockEpollHandler {
            queue_index: 0,
            queue,
            mem: GuestMemoryAtomic::new(memory),
            disk_image,
            disk_nsectors: 64,
            interrupt_cb: Arc::new(TestInterrupt),
            disk_image_id: vec![0; 20],
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
            writeback: Arc::new(AtomicBool::new(true)),
            counters: BlockCounters::default(),
            queue_evt: EventFd::new(0).unwrap(),
            request_list,
            rate_limiter: None,
            access_platform: None,
            read_only,
            discard,
            rate_limited: std::sync::Once::new(),
            id: "test-discard".to_owned(),
            drain_failed: Arc::new(AtomicBool::new(false)),
        }
    }

    fn discard_request(status_addr: GuestAddress) -> Request {
        Request {
            request_type: RequestType::Discard,
            sector: 0,
            data_descriptors: vec![(GuestAddress(0x3000), 16)],
            status_addr,
            writeback: true,
            aligned_operations: Vec::new(),
        }
    }

    // Submit one valid DISCARD descriptor chain through the real queue
    // handler and report the guest-visible status, used index, whether a
    // request was left pending, and whether the backend was asked to discard.
    fn submit_discard(discard: bool, read_only: bool) -> (u8, u16, usize, usize) {
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
        memory
            .write_obj(VIRTIO_BLK_T_DISCARD, GuestAddress(0x3000))
            .unwrap();
        let guest_queue = GuestQ::new(GuestAddress(0x1000), &memory, 16);
        guest_queue.dtable[0].set(0x3000, 16, 1, 1); // header, readable
        guest_queue.dtable[1].set(0x4000, 16, 1, 2); // range, readable
        guest_queue.dtable[2].set(0x5000, 1, 2, 0); // status, writable
        guest_queue.avail.ring[0].set(0);
        guest_queue.avail.idx.set(1);

        let fake = FakeAsyncIo::new();
        let discards = fake.discards.clone();
        let mut handler = test_handler(
            memory.clone(),
            guest_queue.create_queue(),
            Box::new(fake),
            HashMap::new(),
            read_only,
            discard,
        );
        handler.process_queue_submit().unwrap();
        let backend_discards = discards.lock().unwrap().len();
        (
            memory.read_obj::<u8>(GuestAddress(0x5000)).unwrap(),
            guest_queue.used.idx.get(),
            handler.request_list.len(),
            backend_discards,
        )
    }

    #[test]
    fn test_discard_completion_error_writes_one_ioerr_byte_and_clears_pending() {
        for error in [libc::EOPNOTSUPP, libc::EIO] {
            let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
            let guest_queue = GuestQ::new(GuestAddress(0x1000), &memory, 16);
            let status_addr = GuestAddress(0x8000);
            memory.write_obj(0x5au8, GuestAddress(0x7fff)).unwrap();
            memory.write_obj(0xffu8, status_addr).unwrap();
            memory.write_obj(0x5au8, GuestAddress(0x8001)).unwrap();

            let mut fake = FakeAsyncIo::new();
            fake.completions.push_back((0, -error));
            let mut handler = test_handler(
                memory.clone(),
                guest_queue.create_queue(),
                Box::new(fake),
                HashMap::from([(0, discard_request(status_addr))]),
                false,
                true,
            );

            handler.process_queue_complete().unwrap();

            assert_eq!(
                memory.read_obj::<u8>(status_addr).unwrap(),
                VIRTIO_BLK_S_IOERR as u8,
                "error {error} must map to IOERR"
            );
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x7fff)).unwrap(),
                0x5a,
                "byte before status must be untouched"
            );
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x8001)).unwrap(),
                0x5a,
                "byte after status must be untouched"
            );
            assert_eq!(guest_queue.used.idx.get(), 1);
            assert!(handler.request_list.is_empty());
        }
    }

    #[test]
    fn test_discard_submit_without_negotiation_writes_unsupp_without_pending() {
        let (status, used, pending, backend_discards) = submit_discard(false, false);
        assert_eq!(status, VIRTIO_BLK_S_UNSUPP as u8);
        assert_eq!(used, 1);
        assert_eq!(pending, 0);
        assert_eq!(backend_discards, 0);
    }

    #[test]
    fn test_discard_submit_on_read_only_device_writes_ioerr_without_pending() {
        let (status, used, pending, backend_discards) = submit_discard(true, true);
        assert_eq!(status, VIRTIO_BLK_S_IOERR as u8);
        assert_eq!(used, 1);
        assert_eq!(pending, 0);
        assert_eq!(backend_discards, 0);
    }

    // Lay out a DISCARD chain at heads 0..=2 and a GET_ID chain at heads
    // 3..=5. Only the DISCARD chain is exposed through the avail ring; tests
    // reveal the GET_ID chain later to prove the queue can be re-armed.
    fn prepare_discard_and_get_id(memory: &GuestMemoryMmap) -> GuestQ<'_> {
        memory
            .write_obj(VIRTIO_BLK_T_DISCARD, GuestAddress(0x3000))
            .unwrap();
        memory
            .write_obj(VIRTIO_BLK_T_GET_ID, GuestAddress(0x6000))
            .unwrap();
        let mut payload = Vec::new();
        payload.extend_from_slice(&0u64.to_le_bytes()); // sector
        payload.extend_from_slice(&8u32.to_le_bytes()); // sector count
        payload.extend_from_slice(&0u32.to_le_bytes()); // flags
        memory.write_slice(&payload, GuestAddress(0x4000)).unwrap();

        let guest_queue = GuestQ::new(GuestAddress(0x1000), memory, 16);
        guest_queue.dtable[0].set(0x3000, 16, 1, 1); // header, readable, next
        guest_queue.dtable[1].set(0x4000, 16, 1, 2); // range, readable, next -> status
        guest_queue.dtable[2].set(0x5000, 1, 2, 0); // status, writable
        guest_queue.dtable[3].set(0x6000, 16, 1, 4); // header, readable, next
        guest_queue.dtable[4].set(0x7000, 20, 3, 5); // id, writable, next
        guest_queue.dtable[5].set(0x8000, 1, 2, 0); // status, writable
        guest_queue.avail.ring[0].set(0);
        guest_queue.avail.idx.set(1);
        guest_queue
    }

    // The guest's interrupt condition, wrapping u16 arithmetic included.
    fn vring_need_event(event: u16, new: u16, old: u16) -> bool {
        new.wrapping_sub(event).wrapping_sub(1) < new.wrapping_sub(old)
    }

    // Used-ring layout as virtio-queue 0.14 computes it: 4-byte header then
    // 8-byte elements, with avail_event last under EVENT_IDX.
    fn used_ring_addresses(queue: &virtio_queue::Queue) -> (GuestAddress, GuestAddress) {
        let used = GuestAddress(queue.used_ring());
        let avail_event = GuestAddress(queue.used_ring() + 4 + 8 * u64::from(queue.size()));
        (used, avail_event)
    }

    // After consuming avail idx 1 the device must leave the queue armed so a
    // guest that publishes idx 2 still gets notified. Under EVENT_IDX that
    // means a fresh avail_event; without it, the plain used flags must stay
    // unsuppressed.
    fn assert_queue_rearmed(
        memory: &GuestMemoryMmap,
        event_idx: bool,
        used: GuestAddress,
        avail_event_addr: GuestAddress,
    ) {
        if event_idx {
            let avail_event = u16::from_le(memory.read_obj::<u16>(avail_event_addr).unwrap());
            assert!(
                vring_need_event(avail_event, 2, 1),
                "event_idx: stale avail_event={avail_event} would never wake the guest for avail idx 2"
            );
        } else {
            let used_flags = u16::from_le(memory.read_obj::<u16>(used).unwrap());
            assert_eq!(
                used_flags & 1,
                0,
                "non-event_idx: used-ring notifications must remain enabled"
            );
        }
    }

    #[test]
    fn test_discard_safely_rejected_leaves_one_ioerr_and_queue_usable() {
        for event_idx in [false, true] {
            let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
            memory.write_obj(0x5au8, GuestAddress(0x4fff)).unwrap();
            memory.write_obj(0x5au8, GuestAddress(0x5001)).unwrap();
            let guest_queue = prepare_discard_and_get_id(&memory);

            let mut fake = FakeAsyncIo::new();
            fake.discard_response = DiscardResponse::Reject(libc::EAGAIN);
            let mut queue = guest_queue.create_queue();
            queue.set_event_idx(event_idx);
            let (used, avail_event_addr) = used_ring_addresses(&queue);
            let mut handler = test_handler(
                memory.clone(),
                queue,
                Box::new(fake),
                HashMap::new(),
                false,
                true,
            );

            handler.process_queue_submit().unwrap();

            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x5000)).unwrap(),
                VIRTIO_BLK_S_IOERR as u8,
                "event_idx={event_idx}: safely rejected discard must report IOERR"
            );
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x4fff)).unwrap(),
                0x5a,
                "event_idx={event_idx}: byte before status must be untouched"
            );
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x5001)).unwrap(),
                0x5a,
                "event_idx={event_idx}: byte after status must be untouched"
            );
            assert_eq!(guest_queue.used.idx.get(), 1);
            assert!(handler.request_list.is_empty());
            assert!(handler.disk_image.complete().is_empty());

            // The rejection must rearm the queue before the guest can publish
            // the next chain; a manual second invocation would hide a lost
            // notification.
            assert_queue_rearmed(&memory, event_idx, used, avail_event_addr);

            // The rejection must not wedge the queue: a later chain is still
            // consumed.
            guest_queue.avail.ring[1].set(3);
            guest_queue.avail.idx.set(2);
            handler.process_queue_submit().unwrap();
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x8000)).unwrap(),
                VIRTIO_BLK_S_OK as u8,
                "event_idx={event_idx}: GetDeviceId after the rejection must succeed"
            );
            assert_eq!(guest_queue.used.idx.get(), 2);
            assert!(handler.request_list.is_empty());
        }
    }

    #[test]
    fn test_ambiguous_discard_error_remains_fatal() {
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
        let guest_queue = prepare_discard_and_get_id(&memory);
        let mut fake = FakeAsyncIo::new();
        fake.discard_response = DiscardResponse::Fatal(libc::EIO);
        let mut handler = test_handler(
            memory.clone(),
            guest_queue.create_queue(),
            Box::new(fake),
            HashMap::new(),
            false,
            true,
        );

        let error = handler.process_queue_submit().unwrap_err();
        assert!(
            matches!(
                &error,
                Error::RequestExecuting(block_util::ExecuteError::AsyncDiscard(_))
            ),
            "generic discard error must stay fatal, got {error:?}"
        );
        assert_eq!(guest_queue.used.idx.get(), 0);
        assert!(handler.request_list.is_empty());
    }

    #[test]
    fn test_unnegotiated_discard_does_not_block_later_chains() {
        for event_idx in [false, true] {
            let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
            let guest_queue = prepare_discard_and_get_id(&memory);
            let mut queue = guest_queue.create_queue();
            queue.set_event_idx(event_idx);
            let (used, avail_event_addr) = used_ring_addresses(&queue);
            let mut handler = test_handler(
                memory.clone(),
                queue,
                Box::new(FakeAsyncIo::new()),
                HashMap::new(),
                false,
                false,
            );

            handler.process_queue_submit().unwrap();
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x5000)).unwrap(),
                VIRTIO_BLK_S_UNSUPP as u8,
                "event_idx={event_idx}: unnegotiated discard must report UNSUPP"
            );
            assert_eq!(guest_queue.used.idx.get(), 1);
            assert!(handler.request_list.is_empty());

            assert_queue_rearmed(&memory, event_idx, used, avail_event_addr);

            guest_queue.avail.ring[1].set(3);
            guest_queue.avail.idx.set(2);
            handler.process_queue_submit().unwrap();
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x8000)).unwrap(),
                VIRTIO_BLK_S_OK as u8,
                "event_idx={event_idx}: later chain must still be processed"
            );
            assert_eq!(guest_queue.used.idx.get(), 2);
        }
    }

    #[test]
    fn test_malformed_descriptor_chain_rearms_event_idx_before_get_id() {
        for event_idx in [false, true] {
            let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
            // Neither a malformed chain nor the later GetId may leave these
            // uninitialized-looking success bytes behind.
            memory.write_obj(0xffu8, GuestAddress(0x5000)).unwrap();
            memory.write_obj(0xffu8, GuestAddress(0x8000)).unwrap();
            let guest_queue = prepare_discard_and_get_id(&memory);
            // Consume only the header: without a next descriptor parsing
            // fails, and this chain has no valid status descriptor.
            guest_queue.dtable[0].set(0x3000, 16, 0, 0);

            let mut queue = guest_queue.create_queue();
            queue.set_event_idx(event_idx);
            let (used, avail_event_addr) = used_ring_addresses(&queue);
            let mut handler = test_handler(
                memory.clone(),
                queue,
                Box::new(FakeAsyncIo::new()),
                HashMap::new(),
                false,
                true,
            );

            handler.process_queue_submit().unwrap();
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x5000)).unwrap(),
                0xff,
                "event_idx={event_idx}: malformed chain must not write a status byte"
            );
            assert_eq!(guest_queue.used.idx.get(), 1);
            assert!(handler.request_list.is_empty());
            assert!(handler.disk_image.complete().is_empty());

            assert_queue_rearmed(&memory, event_idx, used, avail_event_addr);

            guest_queue.avail.ring[1].set(3);
            guest_queue.avail.idx.set(2);
            handler.process_queue_submit().unwrap();
            assert_eq!(
                memory.read_obj::<u8>(GuestAddress(0x8000)).unwrap(),
                VIRTIO_BLK_S_OK as u8,
                "event_idx={event_idx}: GetId after the malformed chain must succeed"
            );
            assert_eq!(guest_queue.used.idx.get(), 2);
            assert!(handler.request_list.is_empty());
        }
    }

    #[test]
    fn test_regular_file_backends_report_default_discard_alignment() {
        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let file = temp.as_file().try_clone().unwrap();
        let sync_disk = block_util::raw_sync::RawFileDiskSync::new(file.try_clone().unwrap())
            .with_discard(true);
        assert_eq!(sync_disk.discard_sector_alignment(), 8);
        let async_disk = block_util::raw_async::RawFileDisk::new(file).with_discard(true);
        assert_eq!(async_disk.discard_sector_alignment(), 8);
    }

    struct AlignmentTestDisk(u32);

    impl DiskFile for AlignmentTestDisk {
        fn size(&mut self) -> block_util::async_io::DiskFileResult<u64> {
            Ok(64 * 1024 * 1024)
        }
        fn new_async_io(&self, _: u32) -> block_util::async_io::DiskFileResult<Box<dyn AsyncIo>> {
            Err(block_util::async_io::DiskFileError::NewAsyncIo(
                io::Error::from(io::ErrorKind::Unsupported),
            ))
        }
        fn supports_discard(&self) -> bool {
            true
        }
        fn discard_sector_alignment(&self) -> u32 {
            self.0
        }
    }

    fn alignment_test_device(alignment: u32, state: Option<BlockState>) -> io::Result<Block> {
        Block::new(
            "alignment-test".to_owned(),
            Box::new(AlignmentTestDisk(alignment)),
            PathBuf::from("unused-test-path"),
            false,
            false,
            1,
            128,
            SeccompAction::Allow,
            None,
            EventFd::new(libc::EFD_NONBLOCK).unwrap(),
            state,
        )
    }

    #[test]
    fn test_discard_alignment_is_advertised_from_backend() {
        for alignment in [32u32, 2048] {
            let block = alignment_test_device(alignment, None).unwrap();
            assert_ne!(block.common.avail_features & (1 << VIRTIO_BLK_F_DISCARD), 0);
            // Copy out of the packed config before taking a reference.
            let advertised = block.config.discard_sector_alignment;
            assert_eq!(
                advertised, alignment,
                "backend alignment {alignment} must be advertised to the guest"
            );
        }
    }

    #[test]
    fn test_discard_alignment_survives_snapshot_restore() {
        let block = alignment_test_device(2048, None).unwrap();
        let state = block.state();
        let saved = state.config.discard_sector_alignment;
        assert_eq!(saved, 2048);

        // A backend that would report a different alignment must not rewrite
        // the saved config on restore.
        let restored = alignment_test_device(8, Some(state)).unwrap();
        let restored_alignment = restored.config.discard_sector_alignment;
        assert_eq!(restored_alignment, 2048);
        assert_ne!(
            restored.common.avail_features & (1 << VIRTIO_BLK_F_DISCARD),
            0
        );
    }

    // Drive the real `before_pause` hook. The notifier is made readable
    // before the call so the drain loop never blocks on its poll timeout,
    // even when `complete()` deliberately returns empty on the first call.
    fn run_before_pause(
        delay_completions: usize,
        fail_fsync: bool,
    ) -> (u8, u16, usize, bool, Vec<&'static str>) {
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap();
        let guest_queue = GuestQ::new(GuestAddress(0x1000), &memory, 16);
        let status_addr = GuestAddress(0x8000);
        memory.write_obj(0xffu8, status_addr).unwrap();

        let mut fake = FakeAsyncIo::new();
        fake.completions.push_back((0, 0));
        fake.delay_completions = delay_completions;
        fake.fail_fsync = fail_fsync;
        let events = fake.events.clone();
        let mut handler = test_handler(
            memory.clone(),
            guest_queue.create_queue(),
            Box::new(fake),
            HashMap::from([(0, discard_request(status_addr))]),
            false,
            true,
        );

        handler.disk_image.notifier().write(1).unwrap();
        let mut helper = EpollHelper::new(&handler.kill_evt, &handler.pause_evt).unwrap();
        handler.before_pause(&mut helper).unwrap();

        let events = events.lock().unwrap().clone();
        (
            memory.read_obj::<u8>(status_addr).unwrap(),
            guest_queue.used.idx.get(),
            handler.request_list.len(),
            handler.drain_failed.load(Ordering::SeqCst),
            events,
        )
    }

    #[test]
    fn test_before_pause_drains_delayed_discard_before_flush() {
        let (status, used, pending, drain_failed, events) = run_before_pause(1, false);
        assert_eq!(status, VIRTIO_BLK_S_OK as u8);
        assert_eq!(used, 1);
        assert_eq!(pending, 0);
        assert!(!drain_failed);
        assert_eq!(events, vec!["complete", "fsync"]);
    }

    #[test]
    fn test_before_pause_flush_failure_sets_drain_failed_after_drain() {
        let (status, used, pending, drain_failed, events) = run_before_pause(0, true);
        assert_eq!(status, VIRTIO_BLK_S_OK as u8);
        assert_eq!(used, 1);
        assert_eq!(pending, 0);
        assert!(drain_failed);
        assert_eq!(events, vec!["complete", "fsync"]);
    }
}
