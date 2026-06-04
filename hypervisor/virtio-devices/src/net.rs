// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the THIRD-PARTY file.

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
use net_util::CtrlQueue;
use net_util::{
    build_net_config_space, build_net_config_space_with_mq, open_tap,
    virtio_features_to_tap_offload, MacAddr, NetCounters, NetQueuePair, OpenTapError, RxVirtio,
    Tap, TapError, TxVirtio, VirtioNetConfig,
};
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::net::Ipv4Addr;
use std::num::Wrapping;
use std::ops::Deref;
use std::os::unix::io::{AsRawFd, RawFd};
use std::result;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Barrier};
use std::thread;
use std::vec::Vec;
use std::{collections::HashMap, convert::TryInto};
use thiserror::Error;
use virtio_bindings::bindings::virtio_net::*;
use virtio_bindings::bindings::virtio_ring::VIRTIO_RING_F_EVENT_IDX;
use virtio_queue::{Queue, QueueT};
use vm_memory::{ByteValued, GuestAddressSpace, GuestMemoryAtomic};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::AccessPlatform;
use vmm_sys_util::eventfd::EventFd;

/// Control queue
// Event available on the control queue.
const CTRL_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;

// Following the VIRTIO specification, the MTU should be at least 1280.
pub const MIN_MTU: u16 = 1280;

pub struct NetCtrlEpollHandler {
    pub mem: GuestMemoryAtomic<GuestMemoryMmap>,
    pub kill_evt: EventFd,
    pub pause_evt: EventFd,
    pub ctrl_q: CtrlQueue,
    pub queue_evt: EventFd,
    pub queue: Queue,
    pub access_platform: Option<Arc<dyn AccessPlatform>>,
    pub interrupt_cb: Arc<dyn VirtioInterrupt>,
    pub queue_index: u16,
}

impl NetCtrlEpollHandler {
    fn signal_used_queue(&self, queue_index: u16) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(queue_index))
            .map_err(|e| {
                error!("Failed to signal used queue: {:?}", e);
                DeviceError::FailedSignalingUsedQueue(e)
            })
    }

    pub fn run_ctrl(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
    ) -> std::result::Result<(), EpollHelperError> {
        let mut helper = EpollHelper::new(&self.kill_evt, &self.pause_evt)?;
        helper.add_event(self.queue_evt.as_raw_fd(), CTRL_QUEUE_EVENT)?;
        helper.run(paused, paused_sync, self)?;

        Ok(())
    }
}

impl EpollHelperHandler for NetCtrlEpollHandler {
    fn handle_event(
        &mut self,
        _helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;
        match ev_type {
            CTRL_QUEUE_EVENT => {
                let mem = self.mem.memory();
                self.queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to get control queue event: {:?}",
                        e
                    ))
                })?;
                self.ctrl_q
                    .process(mem.deref(), &mut self.queue, self.access_platform.as_ref())
                    .map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to process control queue: {:?}",
                            e
                        ))
                    })?;
                match self.queue.needs_notification(mem.deref()) {
                    Ok(true) => {
                        self.signal_used_queue(self.queue_index).map_err(|e| {
                            EpollHelperError::HandleEvent(anyhow!(
                                "Error signalling that control queue was used: {:?}",
                                e
                            ))
                        })?;
                    }
                    Ok(false) => {}
                    Err(e) => {
                        return Err(EpollHelperError::HandleEvent(anyhow!(
                            "Error getting notification state of control queue: {}",
                            e
                        )));
                    }
                };
            }
            _ => {
                return Err(EpollHelperError::HandleEvent(anyhow!(
                    "Unknown event for virtio-net control queue"
                )));
            }
        }

        Ok(())
    }
}

/// Rx/Tx queue pair
// The guest has made a buffer available to receive a frame into.
pub const RX_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
// The transmit queue has a frame that is ready to send from the guest.
pub const TX_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 2;
// A frame is available for reading from the tap device to receive in the guest.
pub const RX_TAP_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 3;
// The TAP can be written to. Used after an EAGAIN error to retry TX.
pub const TX_TAP_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 4;
// New 'wake up' event from the rx rate limiter
pub const RX_RATE_LIMITER_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 5;
// New 'wake up' event from the tx rate limiter
pub const TX_RATE_LIMITER_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 6;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Failed to open taps: {0}")]
    OpenTap(OpenTapError),
    #[error("Using existing tap: {0}")]
    TapError(TapError),
    #[error("Error calling dup() on tap fd: {0}")]
    DuplicateTapFd(std::io::Error),
}

pub type Result<T> = result::Result<T, Error>;

struct NetEpollHandler {
    net: NetQueuePair,
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    kill_evt: EventFd,
    pause_evt: EventFd,
    queue_index_base: u16,
    queue_pair: (Queue, Queue),
    queue_evt_pair: (EventFd, EventFd),
    // Always generate interrupts until the driver has signalled to the device.
    // This mitigates a problem with interrupts from tap events being "lost" upon
    // a restore as the vCPU thread isn't ready to handle the interrupt. This causes
    // issues when combined with VIRTIO_RING_F_EVENT_IDX interrupt suppression.
    driver_awake: bool,
}

impl NetEpollHandler {
    fn signal_used_queue(&self, queue_index: u16) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(queue_index))
            .map_err(|e| {
                error!("Failed to signal used queue: {:?}", e);
                DeviceError::FailedSignalingUsedQueue(e)
            })
    }

    fn handle_rx_event(&mut self) -> result::Result<(), DeviceError> {
        let queue_evt = &self.queue_evt_pair.0;
        if let Err(e) = queue_evt.read() {
            error!("Failed to get rx queue event: {:?}", e);
        }

        self.net.rx_desc_avail = true;

        let rate_limit_reached = self
            .net
            .rx_rate_limiter
            .as_ref()
            .map_or(false, |r| r.is_blocked());

        // Start to listen on RX_TAP_EVENT only when the rate limit is not reached
        if !self.net.rx_tap_listening && !rate_limit_reached {
            net_util::register_listener(
                self.net.epoll_fd.unwrap(),
                self.net.tap.as_raw_fd(),
                epoll::Events::EPOLLIN,
                u64::from(self.net.tap_rx_event_id),
            )
            .map_err(DeviceError::IoError)?;
            self.net.rx_tap_listening = true;
        }

        Ok(())
    }

    fn process_tx(&mut self) -> result::Result<(), DeviceError> {
        if self
            .net
            .process_tx(&self.mem.memory(), &mut self.queue_pair.1)
            .map_err(DeviceError::NetQueuePair)?
            || !self.driver_awake
        {
            self.signal_used_queue(self.queue_index_base + 1)?;
            debug!("Signalling TX queue");
        } else {
            debug!("Not signalling TX queue");
        }
        Ok(())
    }

    fn handle_tx_event(&mut self) -> result::Result<(), DeviceError> {
        let rate_limit_reached = self
            .net
            .tx_rate_limiter
            .as_ref()
            .map_or(false, |r| r.is_blocked());

        if !rate_limit_reached {
            self.process_tx()?;
        }

        Ok(())
    }

    fn handle_rx_tap_event(&mut self) -> result::Result<(), DeviceError> {
        if self
            .net
            .process_rx(&self.mem.memory(), &mut self.queue_pair.0)
            .map_err(DeviceError::NetQueuePair)?
            || !self.driver_awake
        {
            self.signal_used_queue(self.queue_index_base)?;
            debug!("Signalling RX queue");
        } else {
            debug!("Not signalling RX queue");
        }
        Ok(())
    }

    fn run(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
    ) -> result::Result<(), EpollHelperError> {
        let mut helper = EpollHelper::new(&self.kill_evt, &self.pause_evt)?;
        helper.add_event(self.queue_evt_pair.0.as_raw_fd(), RX_QUEUE_EVENT)?;
        helper.add_event(self.queue_evt_pair.1.as_raw_fd(), TX_QUEUE_EVENT)?;
        if let Some(rate_limiter) = &self.net.rx_rate_limiter {
            helper.add_event(rate_limiter.as_raw_fd(), RX_RATE_LIMITER_EVENT)?;
        }
        if let Some(rate_limiter) = &self.net.tx_rate_limiter {
            helper.add_event(rate_limiter.as_raw_fd(), TX_RATE_LIMITER_EVENT)?;
        }

        let mem = self.mem.memory();
        // If there are some already available descriptors on the RX queue,
        // then we can start the thread while listening onto the TAP.
        if self
            .queue_pair
            .0
            .used_idx(mem.deref(), Ordering::Acquire)
            .map_err(EpollHelperError::QueueRingIndex)?
            < self
                .queue_pair
                .0
                .avail_idx(mem.deref(), Ordering::Acquire)
                .map_err(EpollHelperError::QueueRingIndex)?
        {
            helper.add_event(self.net.tap.as_raw_fd(), RX_TAP_EVENT)?;
            self.net.rx_tap_listening = true;
            info!("Listener registered at start");
        }

        // The NetQueuePair needs the epoll fd.
        self.net.epoll_fd = Some(helper.as_raw_fd());

        helper.run(paused, paused_sync, self)?;

        Ok(())
    }
}

impl EpollHelperHandler for NetEpollHandler {
    fn handle_event(
        &mut self,
        _helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;
        match ev_type {
            RX_QUEUE_EVENT => {
                self.driver_awake = true;
                self.handle_rx_event().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Error processing RX queue: {:?}", e))
                })?;
            }
            TX_QUEUE_EVENT => {
                let queue_evt = &self.queue_evt_pair.1;
                if let Err(e) = queue_evt.read() {
                    error!("Failed to get tx queue event: {:?}", e);
                }
                self.driver_awake = true;
                self.handle_tx_event().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Error processing TX queue: {:?}", e))
                })?;
            }
            TX_TAP_EVENT => {
                self.handle_tx_event().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Error processing TX queue (TAP event): {:?}",
                        e
                    ))
                })?;
            }
            RX_TAP_EVENT => {
                self.handle_rx_tap_event().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Error processing tap queue: {:?}", e))
                })?;
            }
            RX_RATE_LIMITER_EVENT => {
                if let Some(rate_limiter) = &mut self.net.rx_rate_limiter {
                    // Upon rate limiter event, call the rate limiter handler and register the
                    // TAP fd for further processing if some RX buffers are available
                    rate_limiter.event_handler().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Error from 'rate_limiter.event_handler()': {:?}",
                            e
                        ))
                    })?;

                    if !self.net.rx_tap_listening && self.net.rx_desc_avail {
                        net_util::register_listener(
                            self.net.epoll_fd.unwrap(),
                            self.net.tap.as_raw_fd(),
                            epoll::Events::EPOLLIN,
                            u64::from(self.net.tap_rx_event_id),
                        )
                        .map_err(|e| {
                            EpollHelperError::HandleEvent(anyhow!(
                                "Error register_listener with `RX_RATE_LIMITER_EVENT`: {:?}",
                                e
                            ))
                        })?;

                        self.net.rx_tap_listening = true;
                    }
                } else {
                    return Err(EpollHelperError::HandleEvent(anyhow!(
                        "Unexpected RX_RATE_LIMITER_EVENT"
                    )));
                }
            }
            TX_RATE_LIMITER_EVENT => {
                if let Some(rate_limiter) = &mut self.net.tx_rate_limiter {
                    // Upon rate limiter event, call the rate limiter handler
                    // and restart processing the queue.
                    rate_limiter.event_handler().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Error from 'rate_limiter.event_handler()': {:?}",
                            e
                        ))
                    })?;

                    self.driver_awake = true;
                    self.process_tx().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!("Error processing TX queue: {:?}", e))
                    })?;
                } else {
                    return Err(EpollHelperError::HandleEvent(anyhow!(
                        "Unexpected TX_RATE_LIMITER_EVENT"
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

pub struct Net {
    common: VirtioCommon,
    id: String,
    taps: Vec<Tap>,
    config: VirtioNetConfig,
    ctrl_queue_epoll_thread: Option<thread::JoinHandle<()>>,
    counters: NetCounters,
    seccomp_action: SeccompAction,
    rate_limiter_config: Option<RateLimiterConfig>,
    exit_evt: EventFd,
}

#[derive(Serialize, Deserialize)]
pub struct NetState {
    pub avail_features: u64,
    pub acked_features: u64,
    pub config: VirtioNetConfig,
    pub queue_size: Vec<u16>,
}

impl Net {
    /// Create a new virtio network device with the given TAP interface.
    #[allow(clippy::too_many_arguments)]
    fn new_with_tap(
        id: String,
        taps: Vec<Tap>,
        guest_mac: Option<MacAddr>,
        iommu: bool,
        num_queues: usize,
        queue_size: u16,
        seccomp_action: SeccompAction,
        rate_limiter_config: Option<RateLimiterConfig>,
        exit_evt: EventFd,
        state: Option<NetState>,
    ) -> Result<Self> {
        assert!(!taps.is_empty());

        let mtu = taps[0].mtu().map_err(Error::TapError)? as u16;

        let (avail_features, acked_features, config, queue_sizes) = if let Some(state) = state {
            debug!("Restoring virtio-net {}", id);
            (
                state.avail_features,
                state.acked_features,
                state.config,
                state.queue_size,
            )
        } else {
            let mut avail_features = 1 << VIRTIO_NET_F_CSUM
                | 1 << VIRTIO_NET_F_CTRL_GUEST_OFFLOADS
                | 1 << VIRTIO_NET_F_GUEST_CSUM
                | 1 << VIRTIO_NET_F_GUEST_ECN
                | 1 << VIRTIO_NET_F_GUEST_TSO4
                | 1 << VIRTIO_NET_F_GUEST_TSO6
                | 1 << VIRTIO_NET_F_GUEST_UFO
                | 1 << VIRTIO_NET_F_HOST_ECN
                | 1 << VIRTIO_NET_F_HOST_TSO4
                | 1 << VIRTIO_NET_F_HOST_TSO6
                | 1 << VIRTIO_NET_F_HOST_UFO
                | 1 << VIRTIO_NET_F_MTU
                | 1 << VIRTIO_RING_F_EVENT_IDX
                | 1 << VIRTIO_F_VERSION_1;

            if iommu {
                avail_features |= 1u64 << VIRTIO_F_IOMMU_PLATFORM;
            }

            avail_features |= 1 << VIRTIO_NET_F_CTRL_VQ;
            let queue_num = num_queues + 1;

            let mut config = VirtioNetConfig::default();
            if let Some(mac) = guest_mac {
                build_net_config_space(
                    &mut config,
                    mac,
                    num_queues,
                    Some(mtu),
                    &mut avail_features,
                );
            } else {
                build_net_config_space_with_mq(
                    &mut config,
                    num_queues,
                    Some(mtu),
                    &mut avail_features,
                );
            }

            (avail_features, 0, config, vec![queue_size; queue_num])
        };

        Ok(Net {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Net as u32,
                avail_features,
                acked_features,
                queue_sizes,
                paused_sync: Some(Arc::new(Barrier::new((num_queues / 2) + 1))),
                min_queues: 2,
                ..Default::default()
            },
            id,
            taps,
            config,
            ctrl_queue_epoll_thread: None,
            counters: NetCounters::default(),
            seccomp_action,
            rate_limiter_config,
            exit_evt,
        })
    }

    /// Create a new virtio network device with the given IP address and
    /// netmask.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        if_name: Option<&str>,
        ip_addr: Option<Ipv4Addr>,
        netmask: Option<Ipv4Addr>,
        guest_mac: Option<MacAddr>,
        host_mac: &mut Option<MacAddr>,
        mtu: Option<u16>,
        iommu: bool,
        num_queues: usize,
        queue_size: u16,
        seccomp_action: SeccompAction,
        rate_limiter_config: Option<RateLimiterConfig>,
        exit_evt: EventFd,
        state: Option<NetState>,
        #[allow(unused_variables)] sandbox_id: Option<String>,
    ) -> Result<Self> {
        let taps = open_tap(
            if_name,
            ip_addr,
            netmask,
            host_mac,
            mtu,
            num_queues / 2,
            None,
            sandbox_id,
        )
        .map_err(Error::OpenTap)?;

        Self::new_with_tap(
            id,
            taps,
            guest_mac,
            iommu,
            num_queues,
            queue_size,
            seccomp_action,
            rate_limiter_config,
            exit_evt,
            state,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn from_tap_fds(
        id: String,
        fds: &[RawFd],
        guest_mac: Option<MacAddr>,
        mtu: Option<u16>,
        iommu: bool,
        queue_size: u16,
        seccomp_action: SeccompAction,
        rate_limiter_config: Option<RateLimiterConfig>,
        exit_evt: EventFd,
        state: Option<NetState>,
    ) -> Result<Self> {
        let mut taps: Vec<Tap> = Vec::new();
        let num_queue_pairs = fds.len();

        for fd in fds.iter() {
            // Duplicate so that it can survive reboots
            // SAFETY: FFI call to dup. Trivially safe.
            let fd = unsafe { libc::dup(*fd) };
            if fd < 0 {
                return Err(Error::DuplicateTapFd(std::io::Error::last_os_error()));
            }
            let tap = Tap::from_tap_fd(fd, num_queue_pairs).map_err(Error::TapError)?;
            taps.push(tap);
        }

        assert!(!taps.is_empty());

        if let Some(mtu) = mtu {
            taps[0].set_mtu(mtu as i32).map_err(Error::TapError)?;
        }

        Self::new_with_tap(
            id,
            taps,
            guest_mac,
            iommu,
            num_queue_pairs * 2,
            queue_size,
            seccomp_action,
            rate_limiter_config,
            exit_evt,
            state,
        )
    }

    fn state(&self) -> NetState {
        NetState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: self.config,
            queue_size: self.common.queue_sizes.clone(),
        }
    }
}

impl Drop for Net {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
        // Needed to ensure all references to tap FDs are dropped (#4868)
        self.common.wait_for_epoll_threads();
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
                ctrl_q: CtrlQueue::new(self.taps.clone()),
                queue: ctrl_queue,
                queue_evt: ctrl_queue_evt,
                access_platform: self.common.access_platform.clone(),
                queue_index: ctrl_queue_index as u16,
                interrupt_cb: interrupt_cb.clone(),
            };

            let paused = self.common.paused.clone();
            // Let's update the barrier as we need 1 for each RX/TX pair +
            // 1 for the control queue + 1 for the main thread signalling
            // the pause.
            self.common.paused_sync = Some(Arc::new(Barrier::new(self.taps.len() + 2)));
            let paused_sync = self.common.paused_sync.clone();

            let mut epoll_threads = Vec::new();
            spawn_virtio_thread(
                &format!("{}_ctrl", &self.id),
                &self.seccomp_action,
                Thread::VirtioNetCtl,
                &mut epoll_threads,
                &self.exit_evt,
                move || ctrl_handler.run_ctrl(paused, paused_sync.unwrap()),
            )?;
            self.ctrl_queue_epoll_thread = Some(epoll_threads.remove(0));
        }

        let mut epoll_threads = Vec::new();
        let mut taps = self.taps.clone();
        for i in 0..queues.len() / 2 {
            let rx = RxVirtio::new();
            let tx = TxVirtio::new();
            let rx_tap_listening = false;

            let (_, queue_0, queue_evt_0) = queues.remove(0);
            let (_, queue_1, queue_evt_1) = queues.remove(0);
            let mut queue_pair = (queue_0, queue_1);
            queue_pair.0.set_event_idx(event_idx);
            queue_pair.1.set_event_idx(event_idx);

            let queue_evt_pair = (queue_evt_0, queue_evt_1);

            let (kill_evt, pause_evt) = self.common.dup_eventfds();

            let rx_rate_limiter: Option<rate_limiter::RateLimiter> = self
                .rate_limiter_config
                .map(RateLimiterConfig::try_into)
                .transpose()
                .map_err(ActivateError::CreateRateLimiter)?;

            let tx_rate_limiter: Option<rate_limiter::RateLimiter> = self
                .rate_limiter_config
                .map(RateLimiterConfig::try_into)
                .transpose()
                .map_err(ActivateError::CreateRateLimiter)?;

            let tap = taps.remove(0);

            if !tap.is_donated() {
                tap.set_offload(virtio_features_to_tap_offload(self.common.acked_features))
                    .map_err(|e| {
                        error!("Error programming tap offload: {:?}", e);
                        ActivateError::BadActivate
                    })?;
            }

            let mut handler = NetEpollHandler {
                net: NetQueuePair {
                    tap_for_write_epoll: tap.clone(),
                    tap,
                    rx,
                    tx,
                    epoll_fd: None,
                    rx_tap_listening,
                    tx_tap_listening: false,
                    counters: self.counters.clone(),
                    tap_rx_event_id: RX_TAP_EVENT,
                    tap_tx_event_id: TX_TAP_EVENT,
                    rx_desc_avail: false,
                    rx_rate_limiter,
                    tx_rate_limiter,
                    access_platform: self.common.access_platform.clone(),
                    rx_rate_limited: std::sync::Once::new(),
                    tx_rate_limited: std::sync::Once::new(),
                    id: self.id.clone(),
                },
                mem: mem.clone(),
                queue_index_base: (i * 2) as u16,
                queue_pair,
                queue_evt_pair,
                interrupt_cb: interrupt_cb.clone(),
                kill_evt,
                pause_evt,
                driver_awake: false,
            };

            let paused = self.common.paused.clone();
            let paused_sync = self.common.paused_sync.clone();

            spawn_virtio_thread(
                &format!("{}_qp{}", self.id.clone(), i),
                &self.seccomp_action,
                Thread::VirtioNet,
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
            "rx_bytes",
            Wrapping(self.counters.rx_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "rx_frames",
            Wrapping(self.counters.rx_frames.load(Ordering::Acquire)),
        );
        counters.insert(
            "rx_limit_bytes",
            Wrapping(self.counters.rx_limit_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "rx_limit_frames",
            Wrapping(self.counters.rx_limit_frames.load(Ordering::Acquire)),
        );
        counters.insert(
            "tx_bytes",
            Wrapping(self.counters.tx_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "tx_frames",
            Wrapping(self.counters.tx_frames.load(Ordering::Acquire)),
        );
        counters.insert(
            "tx_limit_bytes",
            Wrapping(self.counters.tx_limit_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "tx_limit_frames",
            Wrapping(self.counters.tx_limit_frames.load(Ordering::Acquire)),
        );

        Some(counters)
    }

    fn set_access_platform(&mut self, access_platform: Arc<dyn AccessPlatform>) {
        self.common.set_access_platform(access_platform)
    }
}

impl Pausable for Net {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()?;

        if let Some(ctrl_queue_epoll_thread) = &self.ctrl_queue_epoll_thread {
            ctrl_queue_epoll_thread.thread().unpark();
        }
        Ok(())
    }
}

impl Snapshottable for Net {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id, &self.state())
    }
}
impl Transportable for Net {}
impl Migratable for Net {}
