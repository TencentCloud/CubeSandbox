// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the THIRD-PARTY file.

#[macro_use]
extern crate log;

mod ctrl_queue;
mod mac;
mod open_tap;
mod queue_pair;
mod tap;

use serde::{Deserialize, Serialize};
use std::io::Error as IoError;
use std::os::raw::c_uint;
use std::os::unix::io::{FromRawFd, RawFd};
use std::{io, mem, net};
use thiserror::Error;
use virtio_bindings::bindings::virtio_net::{
    virtio_net_hdr_v1, VIRTIO_NET_CTRL_MQ_VQ_PAIRS_MAX, VIRTIO_NET_CTRL_MQ_VQ_PAIRS_MIN,
    VIRTIO_NET_F_GUEST_CSUM, VIRTIO_NET_F_GUEST_ECN, VIRTIO_NET_F_GUEST_TSO4,
    VIRTIO_NET_F_GUEST_TSO6, VIRTIO_NET_F_GUEST_UFO, VIRTIO_NET_F_MAC, VIRTIO_NET_F_MQ,
};
use vm_memory::{bitmap::AtomicBitmap, ByteValued};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<AtomicBitmap>;

pub use ctrl_queue::{CtrlQueue, Error as CtrlQueueError};
pub use mac::{MacAddr, MAC_ADDR_LEN};
pub use open_tap::{open_tap, Error as OpenTapError};
pub use queue_pair::{NetCounters, NetQueuePair, NetQueuePairError, RxVirtio, TxVirtio};
pub use tap::{Error as TapError, Tap};

#[derive(Error, Debug)]
pub enum Error {
    #[error("Failed to create a socket: {0}")]
    CreateSocket(IoError),
}

pub type Result<T> = std::result::Result<T, Error>;

#[repr(C, packed)]
#[derive(Copy, Clone, Debug, Default, Serialize, Deserialize)]
pub struct VirtioNetConfig {
    pub mac: [u8; 6],
    pub status: u16,
    pub max_virtqueue_pairs: u16,
    pub mtu: u16,
    pub speed: u32,
    pub duplex: u8,
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioNetConfig {}

/// Create a sockaddr_in from an IPv4 address, and expose it as
/// an opaque sockaddr suitable for usage by socket ioctls.
fn create_sockaddr(ip_addr: net::Ipv4Addr) -> net_gen::sockaddr {
    // IPv4 addresses big-endian (network order), but Ipv4Addr will give us
    // a view of those bytes directly so we can avoid any endian trickiness.
    let addr_in = net_gen::sockaddr_in {
        sin_family: net_gen::AF_INET as u16,
        sin_port: 0,
        sin_addr: unsafe { mem::transmute(ip_addr.octets()) },
        __pad: [0; 8usize],
    };

    unsafe { mem::transmute(addr_in) }
}

fn create_inet_socket() -> Result<net::UdpSocket> {
    // This is safe since we check the return value.
    let sock = unsafe { libc::socket(libc::AF_INET, libc::SOCK_DGRAM, 0) };
    if sock < 0 {
        return Err(Error::CreateSocket(IoError::last_os_error()));
    }

    // This is safe; nothing else will use or hold onto the raw sock fd.
    Ok(unsafe { net::UdpSocket::from_raw_fd(sock) })
}

fn create_unix_socket() -> Result<net::UdpSocket> {
    // This is safe since we check the return value.
    let sock = unsafe { libc::socket(libc::AF_UNIX, libc::SOCK_DGRAM, 0) };
    if sock < 0 {
        return Err(Error::CreateSocket(IoError::last_os_error()));
    }

    // This is safe; nothing else will use or hold onto the raw sock fd.
    Ok(unsafe { net::UdpSocket::from_raw_fd(sock) })
}

fn vnet_hdr_len() -> usize {
    std::mem::size_of::<virtio_net_hdr_v1>()
}

pub fn register_listener(
    epoll_fd: RawFd,
    fd: RawFd,
    ev_type: epoll::Events,
    data: u64,
) -> std::result::Result<(), io::Error> {
    epoll::ctl(
        epoll_fd,
        epoll::ControlOptions::EPOLL_CTL_ADD,
        fd,
        epoll::Event::new(ev_type, data),
    )
}

pub fn unregister_listener(
    epoll_fd: RawFd,
    fd: RawFd,
    ev_type: epoll::Events,
    data: u64,
) -> std::result::Result<(), io::Error> {
    epoll::ctl(
        epoll_fd,
        epoll::ControlOptions::EPOLL_CTL_DEL,
        fd,
        epoll::Event::new(ev_type, data),
    )
}

pub fn build_net_config_space(
    config: &mut VirtioNetConfig,
    mac: MacAddr,
    num_queues: usize,
    mtu: Option<u16>,
    avail_features: &mut u64,
) {
    config.mac.copy_from_slice(mac.get_bytes());
    *avail_features |= 1 << VIRTIO_NET_F_MAC;

    build_net_config_space_with_mq(config, num_queues, mtu, avail_features);
}

pub fn build_net_config_space_with_mq(
    config: &mut VirtioNetConfig,
    num_queues: usize,
    mtu: Option<u16>,
    avail_features: &mut u64,
) {
    let num_queue_pairs = (num_queues / 2) as u16;
    if (num_queue_pairs >= VIRTIO_NET_CTRL_MQ_VQ_PAIRS_MIN as u16)
        && (num_queue_pairs <= VIRTIO_NET_CTRL_MQ_VQ_PAIRS_MAX as u16)
    {
        config.max_virtqueue_pairs = num_queue_pairs;
        *avail_features |= 1 << VIRTIO_NET_F_MQ;
    }
    if let Some(mtu) = mtu {
        config.mtu = mtu;
    }
}

pub fn virtio_features_to_tap_offload(features: u64) -> c_uint {
    let mut tap_offloads: c_uint = 0;
    if features & (1 << VIRTIO_NET_F_GUEST_CSUM) != 0 {
        tap_offloads |= net_gen::TUN_F_CSUM;
    }
    if features & (1 << VIRTIO_NET_F_GUEST_TSO4) != 0 {
        tap_offloads |= net_gen::TUN_F_TSO4;
    }
    if features & (1 << VIRTIO_NET_F_GUEST_TSO6) != 0 {
        tap_offloads |= net_gen::TUN_F_TSO6;
    }
    if features & (1 << VIRTIO_NET_F_GUEST_ECN) != 0 {
        tap_offloads |= net_gen::TUN_F_TSO_ECN;
    }
    if features & (1 << VIRTIO_NET_F_GUEST_UFO) != 0 {
        tap_offloads |= net_gen::TUN_F_UFO;
    }

    tap_offloads
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_create_sockaddr() {
        let addr: net::Ipv4Addr = "10.0.0.1".parse().unwrap();
        let sockaddr = create_sockaddr(addr);

        assert_eq!(sockaddr.sa_family, net_gen::AF_INET as u16);

        let data = &sockaddr.sa_data[..];

        // The first two bytes should represent the port, which is 0.
        assert_eq!(data[0], 0);
        assert_eq!(data[1], 0);

        // The next four bytes should represent the actual IPv4 address, in network order.
        assert_eq!(data[2], 10);
        assert_eq!(data[3], 0);
        assert_eq!(data[4], 0);
        assert_eq!(data[5], 1);
    }
}
