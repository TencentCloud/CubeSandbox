// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

//! Implements virtio devices, queues, and transport mechanisms.

#![allow(clippy::significant_drop_in_scrutinee)]

#[macro_use]
extern crate event_monitor;
#[macro_use]
extern crate log;

use serde::{Deserialize, Serialize};
use std::convert::TryInto;
use std::io;
use thiserror::Error;

#[macro_use]
mod device;
pub mod balloon;
pub mod block;
mod console;
pub mod epoll_helper;
pub mod fs;
mod iommu;
pub mod mem;
pub mod net;
mod pmem;
mod rng;
pub mod seccomp_filters;
mod thread_helper;
pub mod transport;
pub mod vdpa;
pub mod vhost_user;
pub mod vsock;
pub mod watchdog;

pub use self::balloon::Balloon;
pub use self::block::{Block, BlockState};
pub use self::console::{Console, ConsoleResizer, Endpoint};
pub use self::device::{
    DmaRemapping, UserspaceMapping, VirtioCommon, VirtioDevice, VirtioInterrupt,
    VirtioInterruptType, VirtioSharedMemoryList,
};
pub use self::epoll_helper::{
    EpollHelper, EpollHelperError, EpollHelperHandler, EPOLL_HELPER_EVENT_LAST,
};
pub use self::fs::*;
pub use self::iommu::{AccessPlatformMapping, Iommu, IommuMapping};
pub use self::mem::{BlocksState, Mem, VirtioMemMappingSource, VIRTIO_MEM_ALIGN_SIZE};
pub use self::net::{Net, NetCtrlEpollHandler};
pub use self::pmem::Pmem;
pub use self::rng::Rng;
pub use self::vdpa::{Vdpa, VdpaDmaMapping};
pub use self::vsock::Vsock;
pub use self::watchdog::Watchdog;

use vm_memory::{bitmap::AtomicBitmap, GuestAddress, GuestMemory};
use vm_virtio::VirtioDeviceType;

use virtiofsd::Error as VirtioFsError;

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<AtomicBitmap>;
type GuestRegionMmap = vm_memory::GuestRegionMmap<AtomicBitmap>;
type MmapRegion = vm_memory::MmapRegion<AtomicBitmap>;

const DEVICE_INIT: u32 = 0x00;
const DEVICE_ACKNOWLEDGE: u32 = 0x01;
const DEVICE_DRIVER: u32 = 0x02;
const DEVICE_DRIVER_OK: u32 = 0x04;
const DEVICE_FEATURES_OK: u32 = 0x08;
const DEVICE_FAILED: u32 = 0x80;

const VIRTIO_F_RING_INDIRECT_DESC: u32 = 28;
const VIRTIO_F_RING_EVENT_IDX: u32 = 29;
const VIRTIO_F_VERSION_1: u32 = 32;
const VIRTIO_F_IOMMU_PLATFORM: u32 = 33;
const VIRTIO_F_IN_ORDER: u32 = 35;
const VIRTIO_F_ORDER_PLATFORM: u32 = 36;
#[allow(dead_code)]
const VIRTIO_F_SR_IOV: u32 = 37;
const VIRTIO_F_NOTIFICATION_DATA: u32 = 38;

#[derive(Error, Debug)]
pub enum ActivateError {
    #[error("Failed to activate virtio device")]
    BadActivate,
    #[error("Failed to clone exit event fd: {0}")]
    CloneExitEventFd(std::io::Error),
    #[error("Failed to spawn thread: {0}")]
    ThreadSpawn(std::io::Error),
    #[error("Failed to setup vhost-user-fs daemon: {0}")]
    VhostUserFsSetup(vhost_user::Error),
    #[error("Failed to setup vhost-user-blk daemon: {0}")]
    VhostUserBlkSetup(vhost_user::Error),
    #[error("Failed to create seccomp filter: {0}")]
    CreateSeccompFilter(seccompiler::Error),
    #[error("Failed to create rate limiter: {0}")]
    CreateRateLimiter(std::io::Error),
    #[error("Failed to activate the vDPA device: {0}")]
    ActivateVdpa(vdpa::Error),
    #[error("Failed to activate the virtio-fs device: {0}")]
    ActivateVirtioFs(std::io::Error),
    #[error("Failed to create virtiofs server: {0}")]
    InvalidServerConfig(VirtioFsError),
    #[error("Failed to restore virtiofs state: {0}")]
    InvalidVirtioFsState(std::io::Error),
}

pub type ActivateResult = std::result::Result<(), ActivateError>;

pub type DeviceEventT = u16;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Failed to single used queue: {0}")]
    FailedSignalingUsedQueue(io::Error),
    #[error("I/O Error: {0}")]
    IoError(io::Error),
    #[error("Failed to update memory vhost-user: {0}")]
    VhostUserUpdateMemory(vhost_user::Error),
    #[error("Failed to add memory region vhost-user: {0}")]
    VhostUserAddMemoryRegion(vhost_user::Error),
    #[error("Failed to set shared memory region")]
    SetShmRegionsNotSupported,
    #[error("Failed to process net queue: {0}")]
    NetQueuePair(::net_util::NetQueuePairError),
    #[error("Failed to : {0}")]
    QueueAddUsed(virtio_queue::Error),
    #[error("Failed to : {0}")]
    QueueIterator(virtio_queue::Error),
    #[error("Failed to enable queue notification: {0}")]
    QueueEnableNotification(virtio_queue::Error),
    #[error("Failed to check queue notification: {0}")]
    QueueNeedsNotification(virtio_queue::Error),
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
pub struct TokenBucketConfig {
    pub size: u64,
    pub one_time_burst: Option<u64>,
    pub refill_time: u64,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct RateLimiterConfig {
    pub bandwidth: Option<TokenBucketConfig>,
    pub ops: Option<TokenBucketConfig>,
}

impl TryInto<rate_limiter::RateLimiter> for RateLimiterConfig {
    type Error = io::Error;

    fn try_into(self) -> std::result::Result<rate_limiter::RateLimiter, Self::Error> {
        let bw = self.bandwidth.unwrap_or_default();
        let ops = self.ops.unwrap_or_default();
        rate_limiter::RateLimiter::new(
            bw.size,
            bw.one_time_burst.unwrap_or(0),
            bw.refill_time,
            ops.size,
            ops.one_time_burst.unwrap_or(0),
            ops.refill_time,
        )
    }
}

/// Convert an absolute address into an address space (GuestMemory)
/// to a host pointer and verify that the provided size define a valid
/// range within a single memory region.
/// Return None if it is out of bounds or if addr+size overlaps a single region.
pub fn get_host_address_range<M: GuestMemory + ?Sized>(
    mem: &M,
    addr: GuestAddress,
    size: usize,
) -> Option<*mut u8> {
    if mem.check_range(addr, size) {
        Some(mem.get_host_address(addr).unwrap())
    } else {
        None
    }
}
