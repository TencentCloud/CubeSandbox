// Copyright (c) 2020 Intel Corporation. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use super::{register_listener, unregister_listener, vnet_hdr_len, Tap};
use rate_limiter::{BucketReduction, RateLimiter, TokenType};
use std::io;
use std::num::Wrapping;
use std::os::unix::io::{AsRawFd, RawFd};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use thiserror::Error;
use virtio_queue::{Queue, QueueOwnedT, QueueT};
use vm_memory::bitmap::Bitmap;
use vm_memory::{Bytes, GuestMemory};
use vm_virtio::{AccessPlatform, Translatable};

#[derive(Clone)]
pub struct TxVirtio {
    pub counter_bytes: Wrapping<u64>,
    pub counter_frames: Wrapping<u64>,
    pub limit_bytes: Wrapping<u64>,
    pub limit_frames: Wrapping<u64>,
}

impl Default for TxVirtio {
    fn default() -> Self {
        Self::new()
    }
}

impl TxVirtio {
    pub fn new() -> Self {
        TxVirtio {
            counter_bytes: Wrapping(0),
            counter_frames: Wrapping(0),
            limit_bytes: Wrapping(0),
            limit_frames: Wrapping(0),
        }
    }

    pub fn process_desc_chain<B: Bitmap + 'static>(
        &mut self,
        mem: &vm_memory::GuestMemoryMmap<B>,
        tap: &mut Tap,
        queue: &mut Queue,
        rate_limiter: &mut Option<RateLimiter>,
        access_platform: Option<&Arc<dyn AccessPlatform>>,
        rate_limited: &mut std::sync::Once,
    ) -> Result<bool, NetQueuePairError> {
        let mut retry_write = false;
        let mut rate_limit_reached = BucketReduction::Success;

        while let Some(mut desc_chain) = queue.pop_descriptor_chain(mem) {
            match rate_limit_reached {
                BucketReduction::Failure => {
                    queue.go_to_previous_position();
                    break;
                }
                BucketReduction::OverConsumption(_r) => {
                    break;
                }
                BucketReduction::Success => {}
            }

            let mut next_desc = desc_chain.next();

            let mut iovecs = Vec::new();
            while let Some(desc) = next_desc {
                let desc_addr = desc
                    .addr()
                    .translate_gva(access_platform, desc.len() as usize);
                if !desc.is_write_only() && desc.len() > 0 {
                    let buf = desc_chain
                        .memory()
                        .get_slice(desc_addr, desc.len() as usize)
                        .map_err(NetQueuePairError::GuestMemory)?
                        .ptr_guard_mut();
                    let iovec = libc::iovec {
                        iov_base: buf.as_ptr() as *mut libc::c_void,
                        iov_len: desc.len() as libc::size_t,
                    };
                    iovecs.push(iovec);
                } else {
                    error!(
                        "Invalid descriptor chain: address = 0x{:x} length = {} write_only = {}",
                        desc_addr.0,
                        desc.len(),
                        desc.is_write_only()
                    );
                    return Err(NetQueuePairError::DescriptorChainInvalid);
                }
                next_desc = desc_chain.next();
            }

            let len = if !iovecs.is_empty() {
                let result = unsafe {
                    libc::writev(
                        tap.as_raw_fd() as libc::c_int,
                        iovecs.as_ptr(),
                        iovecs.len() as libc::c_int,
                    )
                };

                if result < 0 {
                    let e = std::io::Error::last_os_error();

                    /* EAGAIN */
                    if e.kind() == std::io::ErrorKind::WouldBlock {
                        queue.go_to_previous_position();
                        retry_write = true;
                        break;
                    }
                    error!("net: tx: failed writing to tap: {}", e);
                    return Err(NetQueuePairError::WriteTap(e));
                }

                self.counter_bytes += Wrapping(result as u64 - vnet_hdr_len() as u64);
                self.counter_frames += Wrapping(1);

                result as u32
            } else {
                0
            };

            // For the sake of simplicity (similar to the RX rate limiting), we always
            // let the 'last' descriptor chain go-through even if it was over the rate
            // limit, and simply stop processing oncoming `avail_desc` if any.
            if let Some(rate_limiter) = rate_limiter {
                rate_limit_reached = rate_limiter.consume(1, TokenType::Ops);
                if rate_limit_reached != BucketReduction::Success {
                    self.limit_frames += Wrapping(1);
                    rate_limited.call_once(|| info!("net tx ops ratelimit fired"));
                } else {
                    rate_limit_reached = rate_limiter.consume(len as u64, TokenType::Bytes);
                    if rate_limit_reached != BucketReduction::Success {
                        self.limit_bytes += Wrapping(1);
                        rate_limited.call_once(|| info!("net tx bw ratelimit fired"));
                    }
                }
            }

            queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), len)
                .map_err(NetQueuePairError::QueueAddUsed)?;

            if !queue
                .enable_notification(mem)
                .map_err(NetQueuePairError::QueueEnableNotification)?
            {
                break;
            }
        }

        Ok(retry_write)
    }
}

#[derive(Clone)]
pub struct RxVirtio {
    pub counter_bytes: Wrapping<u64>,
    pub counter_frames: Wrapping<u64>,
    pub limit_bytes: Wrapping<u64>,
    pub limit_frames: Wrapping<u64>,
}

impl Default for RxVirtio {
    fn default() -> Self {
        Self::new()
    }
}

impl RxVirtio {
    pub fn new() -> Self {
        RxVirtio {
            counter_bytes: Wrapping(0),
            counter_frames: Wrapping(0),
            limit_bytes: Wrapping(0),
            limit_frames: Wrapping(0),
        }
    }

    pub fn process_desc_chain<B: Bitmap + 'static>(
        &mut self,
        mem: &vm_memory::GuestMemoryMmap<B>,
        tap: &mut Tap,
        queue: &mut Queue,
        rate_limiter: &mut Option<RateLimiter>,
        access_platform: Option<&Arc<dyn AccessPlatform>>,
        rate_limited: &mut std::sync::Once,
    ) -> Result<bool, NetQueuePairError> {
        let mut exhausted_descs = true;
        let mut rate_limit_reached = BucketReduction::Success;

        while let Some(mut desc_chain) = queue.pop_descriptor_chain(mem) {
            match rate_limit_reached {
                BucketReduction::Failure => {
                    exhausted_descs = false;
                    queue.go_to_previous_position();
                    break;
                }
                BucketReduction::OverConsumption(_r) => {
                    break;
                }
                BucketReduction::Success => {}
            }

            let desc = desc_chain
                .next()
                .ok_or(NetQueuePairError::DescriptorChainTooShort)?;

            let num_buffers_addr = desc_chain
                .memory()
                .checked_offset(
                    desc.addr()
                        .translate_gva(access_platform, desc.len() as usize),
                    10,
                )
                .unwrap();
            let mut next_desc = Some(desc);

            let mut iovecs = Vec::new();
            while let Some(desc) = next_desc {
                let desc_addr = desc
                    .addr()
                    .translate_gva(access_platform, desc.len() as usize);
                if desc.is_write_only() && desc.len() > 0 {
                    let buf = desc_chain
                        .memory()
                        .get_slice(desc_addr, desc.len() as usize)
                        .map_err(NetQueuePairError::GuestMemory)?
                        .ptr_guard_mut();
                    let iovec = libc::iovec {
                        iov_base: buf.as_ptr() as *mut libc::c_void,
                        iov_len: desc.len() as libc::size_t,
                    };
                    iovecs.push(iovec);
                } else {
                    error!(
                        "Invalid descriptor chain: address = 0x{:x} length = {} write_only = {}",
                        desc_addr.0,
                        desc.len(),
                        desc.is_write_only()
                    );
                    return Err(NetQueuePairError::DescriptorChainInvalid);
                }
                next_desc = desc_chain.next();
            }

            let len = if !iovecs.is_empty() {
                let result = unsafe {
                    libc::readv(
                        tap.as_raw_fd() as libc::c_int,
                        iovecs.as_ptr(),
                        iovecs.len() as libc::c_int,
                    )
                };
                if result < 0 {
                    let e = std::io::Error::last_os_error();
                    exhausted_descs = false;
                    queue.go_to_previous_position();

                    /* EAGAIN */
                    if e.kind() == std::io::ErrorKind::WouldBlock {
                        break;
                    }

                    error!("net: rx: failed reading from tap: {}", e);
                    return Err(NetQueuePairError::ReadTap(e));
                }

                // Write num_buffers to guest memory. We simply write 1 as we
                // never spread the frame over more than one descriptor chain.
                desc_chain
                    .memory()
                    .write_obj(1u16, num_buffers_addr)
                    .map_err(NetQueuePairError::GuestMemory)?;

                self.counter_bytes += Wrapping(result as u64 - vnet_hdr_len() as u64);
                self.counter_frames += Wrapping(1);

                result as u32
            } else {
                0
            };

            // For the sake of simplicity (keeping the handling of RX_QUEUE_EVENT and
            // RX_TAP_EVENT totally asynchronous), we always let the 'last' descriptor
            // chain go-through even if it was over the rate limit, and simply stop
            // processing oncoming `avail_desc` if any.
            if let Some(rate_limiter) = rate_limiter {
                rate_limit_reached = rate_limiter.consume(1, TokenType::Ops);
                if rate_limit_reached != BucketReduction::Success {
                    self.limit_frames += Wrapping(1);
                    rate_limited.call_once(|| info!("net rx ops ratelimit fired"));
                } else {
                    rate_limit_reached = rate_limiter.consume(len as u64, TokenType::Bytes);
                    if rate_limit_reached != BucketReduction::Success {
                        self.limit_bytes += Wrapping(1);
                        rate_limited.call_once(|| info!("net rx bw ratelimit fired"));
                    }
                }
            }

            queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), len)
                .map_err(NetQueuePairError::QueueAddUsed)?;

            if !queue
                .enable_notification(mem)
                .map_err(NetQueuePairError::QueueEnableNotification)?
            {
                break;
            }
        }

        Ok(exhausted_descs)
    }
}

#[derive(Default, Clone)]
pub struct NetCounters {
    pub tx_bytes: Arc<AtomicU64>,
    pub tx_frames: Arc<AtomicU64>,
    pub rx_bytes: Arc<AtomicU64>,
    pub rx_frames: Arc<AtomicU64>,
    pub rx_limit_bytes: Arc<AtomicU64>,
    pub rx_limit_frames: Arc<AtomicU64>,
    pub tx_limit_bytes: Arc<AtomicU64>,
    pub tx_limit_frames: Arc<AtomicU64>,
}

#[derive(Error, Debug)]
pub enum NetQueuePairError {
    #[error("No memory configured")]
    NoMemoryConfigured,
    #[error("Error registering listener: {0}")]
    RegisterListener(io::Error),
    #[error("Error unregistering listener: {0}")]
    UnregisterListener(io::Error),
    #[error("Error writing to the TAP device: {0}")]
    WriteTap(io::Error),
    #[error("Error reading from the TAP device: {0}")]
    ReadTap(io::Error),
    #[error("Error related to guest memory: {0}")]
    GuestMemory(vm_memory::GuestMemoryError),
    #[error("Returned an error while iterating through the queue: {0}")]
    QueueIteratorFailed(virtio_queue::Error),
    #[error("Descriptor chain is too short")]
    DescriptorChainTooShort,
    #[error("Descriptor chain does not contain valid descriptors")]
    DescriptorChainInvalid,
    #[error("Failed to determine if queue needed notification: {0}")]
    QueueNeedsNotification(virtio_queue::Error),
    #[error("Failed to enable notification on the queue: {0}")]
    QueueEnableNotification(virtio_queue::Error),
    #[error("Failed to add used index to the queue: {0}")]
    QueueAddUsed(virtio_queue::Error),
}

pub struct NetQueuePair {
    pub tap: Tap,
    // With epoll each FD must be unique. So in order to filter the
    // events we need to get a second FD responding to the original
    // device so that we can send EPOLLOUT and EPOLLIN to separate
    // events.
    pub tap_for_write_epoll: Tap,
    pub rx: RxVirtio,
    pub tx: TxVirtio,
    pub epoll_fd: Option<RawFd>,
    pub rx_tap_listening: bool,
    pub tx_tap_listening: bool,
    pub counters: NetCounters,
    pub tap_rx_event_id: u16,
    pub tap_tx_event_id: u16,
    pub rx_desc_avail: bool,
    pub rx_rate_limiter: Option<RateLimiter>,
    pub tx_rate_limiter: Option<RateLimiter>,
    pub access_platform: Option<Arc<dyn AccessPlatform>>,
    pub tx_rate_limited: std::sync::Once,
    pub rx_rate_limited: std::sync::Once,
    pub id: String,
}

impl NetQueuePair {
    pub fn process_tx<B: Bitmap + 'static>(
        &mut self,
        mem: &vm_memory::GuestMemoryMmap<B>,
        queue: &mut Queue,
    ) -> Result<bool, NetQueuePairError> {
        let tx_tap_retry = self.tx.process_desc_chain(
            mem,
            &mut self.tap,
            queue,
            &mut self.tx_rate_limiter,
            self.access_platform.as_ref(),
            &mut self.tx_rate_limited,
        )?;

        // We got told to try again when writing to the tap. Wait for the TAP to be writable
        if tx_tap_retry && !self.tx_tap_listening {
            register_listener(
                self.epoll_fd.unwrap(),
                self.tap_for_write_epoll.as_raw_fd(),
                epoll::Events::EPOLLOUT,
                u64::from(self.tap_tx_event_id),
            )
            .map_err(NetQueuePairError::RegisterListener)?;
            self.tx_tap_listening = true;
            info!("Writing to TAP returned EAGAIN. Listening for TAP to become writable.");
        } else if !tx_tap_retry && self.tx_tap_listening {
            unregister_listener(
                self.epoll_fd.unwrap(),
                self.tap_for_write_epoll.as_raw_fd(),
                epoll::Events::EPOLLOUT,
                u64::from(self.tap_tx_event_id),
            )
            .map_err(NetQueuePairError::UnregisterListener)?;
            self.tx_tap_listening = false;
            info!("Writing to TAP succeeded. No longer listening for TAP to become writable.");
        }

        self.counters
            .tx_bytes
            .fetch_add(self.tx.counter_bytes.0, Ordering::AcqRel);
        self.counters
            .tx_frames
            .fetch_add(self.tx.counter_frames.0, Ordering::AcqRel);
        self.counters
            .tx_limit_bytes
            .fetch_add(self.tx.limit_bytes.0, Ordering::AcqRel);
        self.counters
            .tx_limit_frames
            .fetch_add(self.tx.limit_frames.0, Ordering::AcqRel);
        self.tx.counter_bytes = Wrapping(0);
        self.tx.counter_frames = Wrapping(0);
        self.tx.limit_bytes = Wrapping(0);
        self.tx.limit_frames = Wrapping(0);

        queue
            .needs_notification(mem)
            .map_err(NetQueuePairError::QueueNeedsNotification)
    }

    pub fn process_rx<B: Bitmap + 'static>(
        &mut self,
        mem: &vm_memory::GuestMemoryMmap<B>,
        queue: &mut Queue,
    ) -> Result<bool, NetQueuePairError> {
        self.rx_desc_avail = !self.rx.process_desc_chain(
            mem,
            &mut self.tap,
            queue,
            &mut self.rx_rate_limiter,
            self.access_platform.as_ref(),
            &mut self.rx_rate_limited,
        )?;
        let rate_limit_reached = self
            .rx_rate_limiter
            .as_ref()
            .map_or(false, |r| r.is_blocked());

        // Stop listening on the `RX_TAP_EVENT` when:
        // 1) there is no available describles, or
        // 2) the RX rate limit is reached.
        if self.rx_tap_listening && (!self.rx_desc_avail || rate_limit_reached) {
            unregister_listener(
                self.epoll_fd.unwrap(),
                self.tap.as_raw_fd(),
                epoll::Events::EPOLLIN,
                u64::from(self.tap_rx_event_id),
            )
            .map_err(NetQueuePairError::UnregisterListener)?;
            self.rx_tap_listening = false;
        }

        self.counters
            .rx_bytes
            .fetch_add(self.rx.counter_bytes.0, Ordering::AcqRel);
        self.counters
            .rx_frames
            .fetch_add(self.rx.counter_frames.0, Ordering::AcqRel);
        self.counters
            .rx_limit_bytes
            .fetch_add(self.rx.limit_bytes.0, Ordering::AcqRel);
        self.counters
            .rx_limit_frames
            .fetch_add(self.rx.limit_frames.0, Ordering::AcqRel);
        self.rx.counter_bytes = Wrapping(0);
        self.rx.counter_frames = Wrapping(0);
        self.rx.limit_bytes = Wrapping(0);
        self.rx.limit_frames = Wrapping(0);

        queue
            .needs_notification(mem)
            .map_err(NetQueuePairError::QueueNeedsNotification)
    }
}
