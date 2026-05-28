// Copyright 2019 Intel Corporation. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the THIRD-PARTY file.

mod csm;
mod device;
mod packet;
mod unix;

pub use self::device::Vsock;
pub use self::unix::VsockUnixBackend;
pub use self::unix::VsockUnixError;

pub use packet::VsockPacket;
use std::os::unix::io::RawFd;
use vm_migration::Snapshottable;

mod defs {

    /// Max vsock packet data/buffer size.
    pub const MAX_PKT_BUF_SIZE: usize = 64 * 1024;

    pub mod uapi {

        /// Vsock packet operation IDs.
        /// Defined in `/include/uapi/linux/virtio_vsock.h`.
        ///
        /// Connection invalid.
        pub const VSOCK_OP_INVALID: u16 = 0;
        /// Connection request.
        pub const VSOCK_OP_REQUEST: u16 = 1;
        /// Connection response.
        pub const VSOCK_OP_RESPONSE: u16 = 2;
        /// Connection reset.
        pub const VSOCK_OP_RST: u16 = 3;
        /// Connection clean shutdown.
        pub const VSOCK_OP_SHUTDOWN: u16 = 4;
        /// Connection data (read/write).
        pub const VSOCK_OP_RW: u16 = 5;
        /// Flow control credit update.
        pub const VSOCK_OP_CREDIT_UPDATE: u16 = 6;
        /// Flow control credit update request.
        pub const VSOCK_OP_CREDIT_REQUEST: u16 = 7;

        /// Vsock packet flags.
        /// Defined in `/include/uapi/linux/virtio_vsock.h`.
        ///
        /// Valid with a VSOCK_OP_SHUTDOWN packet: the packet sender will receive no more data.
        pub const VSOCK_FLAGS_SHUTDOWN_RCV: u32 = 1;
        /// Valid with a VSOCK_OP_SHUTDOWN packet: the packet sender will send no more data.
        pub const VSOCK_FLAGS_SHUTDOWN_SEND: u32 = 2;

        /// Vsock packet type.
        /// Defined in `/include/uapi/linux/virtio_vsock.h`.
        ///
        /// Stream / connection-oriented packet (the only currently valid type).
        pub const VSOCK_TYPE_STREAM: u16 = 1;

        pub const VSOCK_TYPE_SEQPACKET: u16 = 2;

        pub const VSOCK_HOST_CID: u64 = 2;
    }
}

#[derive(Debug)]
pub enum VsockError {
    /// The vsock data/buffer virtio descriptor length is smaller than expected.
    BufDescTooSmall,
    /// The vsock data/buffer virtio descriptor is expected, but missing.
    BufDescMissing,
    /// Chained GuestMemory error.
    GuestMemory,
    /// Bounds check failed on guest memory pointer.
    GuestMemoryBounds,
    /// The vsock header descriptor length is too small.
    HdrDescTooSmall(u32),
    /// The vsock header descriptor is expected, but missing.
    HdrDescMissing,
    /// The vsock header `len` field holds an invalid value.
    InvalidPktLen(u32),
    /// A data fetch was attempted when no data was available.
    NoData,
    /// A data buffer was expected for the provided packet, but it is missing.
    PktBufMissing,
    /// Encountered an unexpected write-only virtio descriptor.
    UnreadableDescriptor,
    /// Encountered an unexpected read-only virtio descriptor.
    UnwritableDescriptor,
}
type Result<T> = std::result::Result<T, VsockError>;

#[derive(Debug)]
pub enum VsockEpollHandlerError {
    /// The vsock data/buffer virtio descriptor length is smaller than expected.
    BufDescTooSmall,
    /// The vsock data/buffer virtio descriptor is expected, but missing.
    BufDescMissing,
    /// Chained GuestMemory error.
    GuestMemory,
    /// Bounds check failed on guest memory pointer.
    GuestMemoryBounds,
    /// The vsock header descriptor length is too small.
    HdrDescTooSmall(u32),
    /// The vsock header `len` field holds an invalid value.
    InvalidPktLen(u32),
    /// A data fetch was attempted when no data was available.
    NoData,
    /// A data buffer was expected for the provided packet, but it is missing.
    PktBufMissing,
    /// Encountered an unexpected write-only virtio descriptor.
    UnreadableDescriptor,
    /// Encountered an unexpected read-only virtio descriptor.
    UnwritableDescriptor,
}

/// A passive, event-driven object, that needs to be notified whenever an epoll-able event occurs.
/// An event-polling control loop will use `get_polled_fd()` and `get_polled_evset()` to query
/// the listener for the file descriptor and the set of events it's interested in. When such an
/// event occurs, the control loop will route the event to the listener via `notify()`.
///
pub trait VsockEpollListener {
    /// Get the file descriptor the listener needs polled.
    fn get_polled_fd(&self) -> RawFd;

    /// Get the set of events for which the listener wants to be notified.
    fn get_polled_evset(&self) -> epoll::Events;

    /// Notify the listener that one ore more events have occurred.
    fn notify(&mut self, evset: epoll::Events);

    /// If muxer epoll is nested.
    fn muxer_epoll_nested(&self) -> bool {
        true
    }

    /// Set epoll helper fd to muxer
    fn set_epoll_helper_fd(&mut self, _epfd: RawFd) {}

    /// add host_sock fd to listener map
    fn add_host_sock(&mut self) {}

    /// Handle host socket event
    fn dispatch_muxer_event(&mut self, _fd: RawFd, _event_set: epoll::Events) {}
}

/// Any channel that handles vsock packet traffic: sending and receiving packets. Since we're
/// implementing the device model here, our responsibility is to always process the sending of
/// packets (i.e. the TX queue). So, any locally generated data, addressed to the driver (e.g.
/// a connection response or RST), will have to be queued, until we get to processing the RX queue.
///
/// Note: `recv_pkt()` and `send_pkt()` are named analogous to `Read::read()` and `Write::write()`,
///       respectively. I.e.
///       - `recv_pkt(&mut pkt)` will read data from the channel, and place it into `pkt`; and
///       - `send_pkt(&pkt)` will fetch data from `pkt`, and place it into the channel.
pub trait VsockChannel {
    /// Read/receive an incoming packet from the channel.
    fn recv_pkt(&mut self, pkt: &mut VsockPacket) -> Result<()>;

    /// Write/send a packet through the channel.
    fn send_pkt(&mut self, pkt: &VsockPacket) -> Result<()>;

    /// Checks whether there is pending incoming data inside the channel, meaning that a subsequent
    /// call to `recv_pkt()` won't fail.
    fn has_pending_rx(&self) -> bool;
}

/// The vsock backend, which is basically an epoll-event-driven vsock channel, that needs to be
/// sendable through a mpsc channel (the latter due to how `vmm::EpollContext` works).
/// Currently, the only implementation we have is `crate::virtio::unix::muxer::VsockMuxer`, which
/// translates guest-side vsock connections to host-side Unix domain socket connections.
pub trait VsockBackend: VsockChannel + Snapshottable + VsockEpollListener + Send {
    fn connections(&self) -> Vec<(u32, u32)> {
        Vec::new()
    }
    fn queue_rst_for_connections(&mut self, _conns: Vec<(u32, u32)>) {}
}

#[cfg(test)]
mod tests {
    use super::device::{VsockEpollHandler, RX_QUEUE_EVENT, TX_QUEUE_EVENT};
    use super::packet::VSOCK_PKT_HDR_SIZE;
    use super::*;
    use crate::device::{VirtioInterrupt, VirtioInterruptType};
    use crate::epoll_helper::EpollHelperHandler;
    use crate::EpollHelper;
    use crate::GuestMemoryMmap;
    use libc::EFD_NONBLOCK;
    use std::os::unix::io::AsRawFd;
    use std::path::PathBuf;
    use std::sync::{Arc, RwLock};
    use virtio_bindings::bindings::virtio_ring::{VRING_DESC_F_NEXT, VRING_DESC_F_WRITE};
    use vm_memory::{GuestAddress, GuestMemoryAtomic};
    use vm_migration::Pausable;
    use vm_virtio::queue::testing::VirtQueue as GuestQ;
    use vmm_sys_util::eventfd::EventFd;

    pub struct NoopVirtioInterrupt {}

    impl VirtioInterrupt for NoopVirtioInterrupt {
        fn trigger(
            &self,
            _int_type: VirtioInterruptType,
        ) -> std::result::Result<(), std::io::Error> {
            Ok(())
        }
    }

    pub struct TestBackend {
        pub evfd: EventFd,
        pub rx_err: Option<VsockError>,
        pub tx_err: Option<VsockError>,
        pub pending_rx: bool,
        pub rx_ok_cnt: usize,
        pub tx_ok_cnt: usize,
        pub evset: Option<epoll::Events>,
    }
    impl TestBackend {
        pub fn new() -> Self {
            Self {
                evfd: EventFd::new(EFD_NONBLOCK).unwrap(),
                rx_err: None,
                tx_err: None,
                pending_rx: false,
                rx_ok_cnt: 0,
                tx_ok_cnt: 0,
                evset: None,
            }
        }
        pub fn set_rx_err(&mut self, err: Option<VsockError>) {
            self.rx_err = err;
        }
        pub fn set_tx_err(&mut self, err: Option<VsockError>) {
            self.tx_err = err;
        }
        pub fn set_pending_rx(&mut self, prx: bool) {
            self.pending_rx = prx;
        }
    }
    impl VsockChannel for TestBackend {
        fn recv_pkt(&mut self, _pkt: &mut VsockPacket) -> Result<()> {
            match self.rx_err.take() {
                None => {
                    self.rx_ok_cnt += 1;
                    Ok(())
                }
                Some(e) => Err(e),
            }
        }
        fn send_pkt(&mut self, _pkt: &VsockPacket) -> Result<()> {
            match self.tx_err.take() {
                None => {
                    self.tx_ok_cnt += 1;
                    Ok(())
                }
                Some(e) => Err(e),
            }
        }
        fn has_pending_rx(&self) -> bool {
            self.pending_rx
        }
    }
    impl VsockEpollListener for TestBackend {
        fn get_polled_fd(&self) -> RawFd {
            self.evfd.as_raw_fd()
        }
        fn get_polled_evset(&self) -> epoll::Events {
            epoll::Events::EPOLLIN
        }
        fn notify(&mut self, evset: epoll::Events) {
            self.evset = Some(evset);
        }
    }

    impl Pausable for TestBackend {}
    impl Snapshottable for TestBackend {}
    impl VsockBackend for TestBackend {}

    pub struct TestContext {
        pub cid: u64,
        pub mem: GuestMemoryMmap,
        pub mem_size: usize,
        pub device: Vsock<TestBackend>,
    }

    impl TestContext {
        pub fn new() -> Self {
            const CID: u64 = 52;
            const MEM_SIZE: usize = 1024 * 1024 * 128;
            Self {
                cid: CID,
                mem: GuestMemoryMmap::from_ranges(&[(GuestAddress(0), MEM_SIZE)]).unwrap(),
                mem_size: MEM_SIZE,
                device: Vsock::new(
                    String::from("vsock"),
                    CID,
                    PathBuf::from("/test/sock"),
                    TestBackend::new(),
                    false,
                    seccompiler::SeccompAction::Trap,
                    EventFd::new(EFD_NONBLOCK).unwrap(),
                    None,
                )
                .unwrap(),
            }
        }

        pub fn create_epoll_handler_context(&self) -> EpollHandlerContext {
            const QSIZE: u16 = 2;

            let guest_rxvq = GuestQ::new(GuestAddress(0x0010_0000), &self.mem, QSIZE);
            let guest_txvq = GuestQ::new(GuestAddress(0x0020_0000), &self.mem, QSIZE);
            let guest_evvq = GuestQ::new(GuestAddress(0x0030_0000), &self.mem, QSIZE);
            let rxvq = guest_rxvq.create_queue();
            let txvq = guest_txvq.create_queue();
            let evvq = guest_evvq.create_queue();

            // Set up one available descriptor in the RX queue.
            guest_rxvq.dtable[0].set(
                0x0040_0000,
                VSOCK_PKT_HDR_SIZE as u32,
                (VRING_DESC_F_WRITE | VRING_DESC_F_NEXT).try_into().unwrap(),
                1,
            );
            guest_rxvq.dtable[1].set(0x0040_1000, 4096, VRING_DESC_F_WRITE.try_into().unwrap(), 0);
            guest_rxvq.avail.ring[0].set(0);
            guest_rxvq.avail.idx.set(1);

            // Set up one available descriptor in the TX queue.
            guest_txvq.dtable[0].set(
                0x0050_0000,
                VSOCK_PKT_HDR_SIZE as u32,
                VRING_DESC_F_NEXT.try_into().unwrap(),
                1,
            );
            guest_txvq.dtable[1].set(0x0050_1000, 4096, 0, 0);
            guest_txvq.avail.ring[0].set(0);
            guest_txvq.avail.idx.set(1);

            let queues = vec![rxvq, txvq, evvq];
            let queue_evts = vec![
                EventFd::new(EFD_NONBLOCK).unwrap(),
                EventFd::new(EFD_NONBLOCK).unwrap(),
                EventFd::new(EFD_NONBLOCK).unwrap(),
            ];
            let interrupt_cb = Arc::new(NoopVirtioInterrupt {});

            EpollHandlerContext {
                guest_rxvq,
                guest_txvq,
                guest_evvq,
                handler: VsockEpollHandler {
                    mem: GuestMemoryAtomic::new(self.mem.clone()),
                    queues,
                    queue_evts,
                    kill_evt: EventFd::new(EFD_NONBLOCK).unwrap(),
                    pause_evt: EventFd::new(EFD_NONBLOCK).unwrap(),
                    interrupt_cb,
                    backend: Arc::new(RwLock::new(TestBackend::new())),
                    access_platform: None,
                },
            }
        }
    }

    pub struct EpollHandlerContext<'a> {
        pub handler: VsockEpollHandler<TestBackend>,
        pub guest_rxvq: GuestQ<'a>,
        pub guest_txvq: GuestQ<'a>,
        pub guest_evvq: GuestQ<'a>,
    }

    impl<'a> EpollHandlerContext<'a> {
        pub fn signal_txq_event(&mut self) {
            self.handler.queue_evts[1].write(1).unwrap();
            let events = epoll::Events::EPOLLIN;
            let event = epoll::Event::new(events, TX_QUEUE_EVENT as u64);
            let mut epoll_helper =
                EpollHelper::new(&self.handler.kill_evt, &self.handler.pause_evt).unwrap();
            self.handler.handle_event(&mut epoll_helper, &event).ok();
        }
        pub fn signal_rxq_event(&mut self) {
            self.handler.queue_evts[0].write(1).unwrap();
            let events = epoll::Events::EPOLLIN;
            let event = epoll::Event::new(events, RX_QUEUE_EVENT as u64);
            let mut epoll_helper =
                EpollHelper::new(&self.handler.kill_evt, &self.handler.pause_evt).unwrap();
            self.handler.handle_event(&mut epoll_helper, &event).ok();
        }
    }
}
