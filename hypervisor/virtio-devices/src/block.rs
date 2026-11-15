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
use std::fs::File;
use std::io;
use std::num::Wrapping;
use std::ops::Deref;
use std::os::unix::fs::{FileExt, FileTypeExt, MetadataExt};
use std::os::unix::io::AsRawFd;
use std::path::PathBuf;
use std::result;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Barrier};
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
        RequestType::Out | RequestType::Flush | RequestType::Unsupported(_) => 1,
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
        RequestType::Flush => completed_data_bytes == 0,
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

// How long inflight IO may go without any progress while the device is
// paused before the drain is declared stuck. Pause is the synchronization
// point a snapshot relies on, so the drain does not give up on a backend
// that is merely slow: the guest's last requests can sit behind gigabytes
// of host page cache that drain at the backend's pace, and how long that
// takes depends on how much the guest wrote, not on any fixed bound. The
// window restarts whenever one of this queue's requests completes or the
// host device behind the disk completes any IO. Only a backend that makes
// no progress at all for this long fails the drain, so the snapshot is
// aborted instead of being captured in an inconsistent state.
const PAUSE_DRAIN_TIMEOUT: Duration = Duration::from_secs(20);
// Upper bound on the whole drain, however much progress it is making. The
// progress signal includes the host device's completed IO, which other users
// of a shared device also move, and the layers above give a whole pause about
// 120 s (CubeMaster, CubeAPI and Cubelet), which must also cover the
// pause-time flush and the snapshot that follow the drain. Stopping here
// leaves them half of that budget and fails the pause cleanly, with the VM
// resumed, rather than letting an upper layer time out mid-snapshot.
const PAUSE_DRAIN_LIMIT: Duration = Duration::from_secs(60);
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

/// Total IOs the host block device has completed, from its sysfs `stat`:
/// reads, writes and, where the kernel reports them, discards and flushes.
/// Only ever compared with an earlier reading of the same device.
fn parse_completed_ios(stat: &str) -> Option<u64> {
    let fields: Vec<u64> = stat
        .split_whitespace()
        .map(|f| f.parse().ok())
        .collect::<Option<Vec<u64>>>()?;
    if fields.len() < 11 {
        return None;
    }
    let mut ios = fields[0].wrapping_add(fields[4]);
    if fields.len() >= 15 {
        ios = ios.wrapping_add(fields[11]);
    }
    if fields.len() >= 17 {
        ios = ios.wrapping_add(fields[15]);
    }
    Some(ios)
}

/// Opens the sysfs `stat` of the block device behind `path`, if it is one.
/// Opened once here, on the VMM thread, and shared by every queue, so a
/// drain only has to `pread` it.
fn open_device_stat(path: &std::path::Path) -> Option<Arc<File>> {
    let md = std::fs::metadata(path).ok()?;
    if !md.file_type().is_block_device() {
        return None;
    }
    let rdev = md.rdev();
    // SAFETY: major/minor only split the device number; no memory is touched.
    let (major, minor) = unsafe { (libc::major(rdev), libc::minor(rdev)) };
    let stat = format!("/sys/dev/block/{}:{}/stat", major, minor);
    match File::open(&stat) {
        Ok(f) => Some(Arc::new(f)),
        Err(e) => {
            warn!("cannot open {} for pause drain progress: {}", stat, e);
            None
        }
    }
}

fn read_completed_ios(stat: &File) -> Option<u64> {
    let mut buf = [0u8; 512];
    let n = stat.read_at(&mut buf, 0).ok()?;
    parse_completed_ios(std::str::from_utf8(&buf[..n]).ok()?)
}

/// Tracks whether a pause drain is still getting anywhere. Progress is a
/// drop in this queue's inflight count or any change in the host device's
/// completed-IO count; `expired` is true once neither has moved for
/// `idle`, or once the drain has run for `limit` in total.
struct DrainProgress {
    idle: Duration,
    limit: Duration,
    started: Instant,
    last_progress: Instant,
    inflight: usize,
    device_ios: Option<u64>,
    renewals: u64,
}

impl DrainProgress {
    fn new(
        now: Instant,
        idle: Duration,
        limit: Duration,
        inflight: usize,
        device_ios: Option<u64>,
    ) -> Self {
        DrainProgress {
            idle,
            limit,
            started: now,
            last_progress: now,
            inflight,
            device_ios,
            renewals: 0,
        }
    }

    fn observe(&mut self, now: Instant, inflight: usize, device_ios: Option<u64>) {
        let progressed =
            inflight < self.inflight || (device_ios.is_some() && device_ios != self.device_ios);
        if progressed {
            self.last_progress = now;
            self.renewals += 1;
        }
        self.inflight = inflight;
        if device_ios.is_some() {
            self.device_ios = device_ios;
        }
    }

    fn stalled(&self, now: Instant) -> bool {
        now.duration_since(self.last_progress) >= self.idle
    }

    fn over_limit(&self, now: Instant) -> bool {
        now.duration_since(self.started) >= self.limit
    }

    fn expired(&self, now: Instant) -> bool {
        self.stalled(now) || self.over_limit(now)
    }
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
    rate_limited: std::sync::Once,
    id: String,
    // Set by `before_pause` when inflight drain or pause-time flush
    // could not be completed within the deadline. Read by
    // `Block::pause` after the pause barrier returns to decide whether
    // the snapshot must be aborted. Shared (not owned) so a multi-queue
    // device aggregates failures from any queue.
    drain_failed: Arc<AtomicBool>,
    // sysfs `stat` of the host block device behind the disk, when it is
    // one; read during a pause drain to tell a slow backend from a
    // stuck one.
    device_stat: Option<Arc<File>>,
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
                    continue;
                }
            };

            // For virtio spec compliance
            // "A device MUST set the status byte to VIRTIO_BLK_S_IOERR for a write request
            // if the VIRTIO_BLK_F_RO feature if offered, and MUST NOT write any data."
            if self.read_only
                && (request.request_type == RequestType::Out
                    || request.request_type == RequestType::Flush)
            {
                desc_chain
                    .memory()
                    .write_obj(VIRTIO_BLK_S_IOERR, request.status_addr)
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
                    | block_util::ExecuteError::TemporaryBufferAllocation(_)),
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
                    continue;
                }
                Err(e) => return Err(Error::RequestExecuting(e)),
            };
            if submitted {
                self.request_list.insert(desc_chain.head_index(), request);
            } else {
                desc_chain
                    .memory()
                    .write_obj(VIRTIO_BLK_S_OK, request.status_addr)
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

            mem.write_obj(status, request.status_addr)
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
        let started = Instant::now();
        let efd = self.disk_image.notifier().as_raw_fd();
        let mut progress = DrainProgress::new(
            started,
            PAUSE_DRAIN_TIMEOUT,
            PAUSE_DRAIN_LIMIT,
            self.request_list.len(),
            self.device_stat.as_deref().and_then(read_completed_ios),
        );

        while !self.request_list.is_empty() {
            let now = Instant::now();
            if progress.expired(now) {
                let why = if progress.stalled(now) {
                    format!("made no progress for {:?}", PAUSE_DRAIN_TIMEOUT)
                } else {
                    format!("reached the {:?} limit", PAUSE_DRAIN_LIMIT)
                };
                error!(
                    "virtio-blk {}: pause drain {}, {} IO still inflight after {:?}",
                    self.id,
                    why,
                    self.request_list.len(),
                    started.elapsed()
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
            let ios = self.device_stat.as_deref().and_then(read_completed_ios);
            progress.observe(Instant::now(), self.request_list.len(), ios);
        }
        if started.elapsed() >= Duration::from_secs(1) {
            info!(
                "virtio-blk {}: pause drain took {:?} ({} progress renewals)",
                self.id,
                started.elapsed(),
                progress.renewals
            );
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
    /// thread when its pause-time drain stalled for `PAUSE_DRAIN_TIMEOUT`
    /// or ran past `PAUSE_DRAIN_LIMIT`, or when its pause-time flush failed;
    /// checked by `pause()` after the pause barrier so the upper layer can
    /// abort the snapshot.
    drain_failed: Arc<AtomicBool>,
    /// sysfs `stat` of the host block device behind `disk_path`, if any.
    device_stat: Option<Arc<File>>,
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
        let (disk_nsectors, avail_features, acked_features, config) = if let Some(state) = state {
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

            if num_queues > 1 {
                avail_features |= 1u64 << VIRTIO_BLK_F_MQ;
                config.num_queues = num_queues as u16;
            }

            (disk_nsectors, avail_features, 0, config)
        };

        let device_stat = open_device_stat(&disk_path);

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
            device_stat,
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
}

impl Drop for Block {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
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
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;

        let disk_image_id = build_disk_image_id(&self.disk_path);
        self.update_writeback();

        let mut epoll_threads = Vec::new();
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

            let mut handler = BlockEpollHandler {
                queue_index: i as u16,
                queue,
                mem: mem.clone(),
                disk_image: self
                    .disk_image
                    .new_async_io(queue_size as u32)
                    .map_err(|e| {
                        error!("failed to create new AsyncIo: {}", e);
                        ActivateError::BadActivate
                    })?,
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
                rate_limited: std::sync::Once::new(),
                id: self.id.clone(),
                drain_failed: self.drain_failed.clone(),
                device_stat: self.device_stat.clone(),
            };

            let paused = self.common.paused.clone();
            let paused_sync = self.common.paused_sync.clone();

            spawn_virtio_thread(
                &format!("{}_q{}", self.id.clone(), i),
                &self.seccomp_action,
                Thread::VirtioBlock,
                &mut epoll_threads,
                &self.exit_evt,
                move || handler.run(paused, paused_sync.unwrap()),
            )?;
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
        // queue whose drain stalled or ran past its limit, or whose
        // pause-time flush failed, will have set this flag. Aborting pause here propagates upward and prevents
        // a snapshot from being captured in an inconsistent state.
        if self.drain_failed.swap(false, Ordering::SeqCst) {
            return Err(MigratableError::Pause(anyhow!(
                "Failed to drain inflight IO for block device {}: stalled or past the drain limit",
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

    #[test]
    fn parse_completed_ios_sums_reads_writes_discards_and_flushes() {
        // 17-field stat (kernel >= 5.5): reads 10, writes 20, discards 3, flushes 4.
        let stat = "10 0 80 5 20 0 160 9 1 7 14 3 0 24 1 4 2\n";
        assert_eq!(parse_completed_ios(stat), Some(37));
        // 11-field stat (old kernels): reads and writes only.
        assert_eq!(parse_completed_ios("10 0 80 5 20 0 160 9 1 7 14"), Some(30));
        assert_eq!(parse_completed_ios("garbage"), None);
        assert_eq!(parse_completed_ios("1 2 3"), None);
    }

    #[test]
    fn drain_progress_renews_while_device_completes_io() {
        let t0 = Instant::now();
        let idle = Duration::from_secs(20);
        let mut p = DrainProgress::new(t0, idle, Duration::from_secs(600), 1, Some(100));
        // The guest's request is still queued, but the host device keeps
        // completing writeback: never expires, however long it takes.
        for s in 1..=60u64 {
            let now = t0 + Duration::from_secs(s);
            p.observe(now, 1, Some(100 + s));
            assert!(!p.expired(now), "expired at {}s despite progress", s);
        }
        assert_eq!(p.renewals, 60);
    }

    #[test]
    fn drain_progress_expires_after_idle_window_without_progress() {
        let t0 = Instant::now();
        let idle = Duration::from_secs(20);
        let mut p = DrainProgress::new(t0, idle, Duration::from_secs(600), 2, Some(500));
        p.observe(t0 + Duration::from_secs(5), 1, Some(500)); // own request done
        p.observe(t0 + Duration::from_secs(24), 1, Some(500));
        assert!(!p.expired(t0 + Duration::from_secs(24)));
        p.observe(t0 + Duration::from_secs(25), 1, Some(500));
        assert!(p.expired(t0 + Duration::from_secs(25)));
    }

    #[test]
    fn drain_progress_without_device_counters_falls_back_to_completions() {
        let t0 = Instant::now();
        let idle = Duration::from_secs(20);
        let mut p = DrainProgress::new(t0, idle, Duration::from_secs(600), 1, None);
        p.observe(t0 + Duration::from_secs(19), 1, None);
        assert!(!p.expired(t0 + Duration::from_secs(19)));
        assert!(p.expired(t0 + Duration::from_secs(20)));
    }

    #[test]
    fn drain_progress_stops_at_the_limit_despite_progress() {
        let t0 = Instant::now();
        let idle = Duration::from_secs(20);
        let limit = Duration::from_secs(60);
        let mut p = DrainProgress::new(t0, idle, limit, 1, Some(0));
        // Unrelated IO on a shared device keeps moving the counter, so the
        // idle window alone would never end the drain.
        for s in 1..60u64 {
            let now = t0 + Duration::from_secs(s);
            p.observe(now, 1, Some(s));
            assert!(!p.expired(now), "expired at {}s", s);
        }
        let now = t0 + limit;
        p.observe(now, 1, Some(60));
        assert!(!p.stalled(now));
        assert!(p.over_limit(now));
        assert!(p.expired(now));
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
}
