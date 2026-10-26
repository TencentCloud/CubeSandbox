// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//

/// `VsockPacket` provides a thin wrapper over the buffers exchanged via virtio queues.
/// There are two components to a vsock packet, each using its own descriptor in a
/// virtio queue:
/// - the packet header; and
/// - the packet data/buffer.
/// There is a 1:1 relation between descriptor chains and packets: the first (chain head) holds
/// the header, and an optional second descriptor holds the data. The second descriptor is only
/// present for data packets (VSOCK_OP_RW).
///
/// `VsockPacket` wraps these two buffers and provides direct access to the data stored
/// in guest memory. This is done to avoid unnecessarily copying data from guest memory
/// to temporary buffers, before passing it on to the vsock backend.
///
use byteorder::{ByteOrder, LittleEndian};
use std::fmt::{Debug, Error, Formatter};
use std::ops::Deref;
use std::result;
use std::sync::Arc;

use super::defs;
use super::{Result, VsockError};
use crate::get_host_address_range;
use crate::vsock::defs::uapi;
use virtio_queue::DescriptorChain;
use vm_memory::{Address, GuestMemory};
use vm_virtio::{AccessPlatform, Translatable};
// The vsock packet header is defined by the C struct:
//
// ```C
// struct virtio_vsock_hdr {
//     le64 src_cid;
//     le64 dst_cid;
//     le32 src_port;
//     le32 dst_port;
//     le32 len;
//     le16 type;
//     le16 op;
//     le32 flags;
//     le32 buf_alloc;
//     le32 fwd_cnt;
// };
// ```
//
// This structed will occupy the buffer pointed to by the head descriptor. We'll be accessing it
// as a byte slice. To that end, we define below the offsets for each field struct, as well as the
// packed struct size, as a bunch of `usize` consts.
// Note that these offsets are only used privately by the `VsockPacket` struct, the public interface
// consisting of getter and setter methods, for each struct field, that will also handle the correct
// endianess.

/// The vsock packet header struct size (when packed).
pub const VSOCK_PKT_HDR_SIZE: usize = 44;

// Source CID.
const HDROFF_SRC_CID: usize = 0;

// Destination CID.
const HDROFF_DST_CID: usize = 8;

// Source port.
const HDROFF_SRC_PORT: usize = 16;

// Destination port.
const HDROFF_DST_PORT: usize = 20;

// Data length (in bytes) - may be 0, if there is no data buffer.
const HDROFF_LEN: usize = 24;

// Socket type. Currently, only connection-oriented streams are defined by the vsock protocol.
const HDROFF_TYPE: usize = 28;

// Operation ID - one of the VSOCK_OP_* values; e.g.
// - VSOCK_OP_RW: a data packet;
// - VSOCK_OP_REQUEST: connection request;
// - VSOCK_OP_RST: forceful connection termination;
// etc (see `super::defs::uapi` for the full list).
const HDROFF_OP: usize = 30;

// Additional options (flags) associated with the current operation (`op`).
// Currently, only used with shutdown requests (VSOCK_OP_SHUTDOWN).
const HDROFF_FLAGS: usize = 32;

// Size (in bytes) of the packet sender receive buffer (for the connection to which this packet
// belongs).
const HDROFF_BUF_ALLOC: usize = 36;

// Number of bytes the sender has received and consumed (for the connection to which this packet
// belongs). For instance, for our Unix backend, this counter would be the total number of bytes
// we have successfully written to a backing Unix socket.
const HDROFF_FWD_CNT: usize = 40;

#[allow(dead_code)]
#[derive(Clone, Copy)]
#[repr(C)]
pub struct VirtioVsockHdr {
    src_cid: u64,
    dst_cid: u64,
    src_port: u32,
    dst_port: u32,
    len: u32,
    type_field: u16,
    op: u16,
    flags: u32,
    buf_alloc: u32,
    fwd_cnt: u32,
}

impl VirtioVsockHdr {
    pub fn from_slice(data: &[u8]) -> Option<Self> {
        let raw_hdr: &VirtioVsockHdr;
        // SAFETY: data is a slice of the packet header
        unsafe {
            let ptr = data.as_ptr() as *const VirtioVsockHdr;
            raw_hdr = &*ptr;
        }

        let hdr = Self {
            src_cid: u64::from_le(raw_hdr.src_cid),
            dst_cid: u64::from_le(raw_hdr.dst_cid),
            src_port: u32::from_le(raw_hdr.src_port),
            dst_port: u32::from_le(raw_hdr.dst_port),
            len: u32::from_le(raw_hdr.len),
            type_field: u16::from_le(raw_hdr.type_field),
            op: u16::from_le(raw_hdr.op),
            flags: u32::from_le(raw_hdr.flags),
            buf_alloc: u32::from_le(raw_hdr.buf_alloc),
            fwd_cnt: u32::from_le(raw_hdr.fwd_cnt),
        };
        Some(hdr)
    }
}

impl Debug for VirtioVsockHdr {
    fn fmt(&self, f: &mut Formatter<'_>) -> result::Result<(), Error> {
        let type_field = match self.type_field {
            uapi::VSOCK_TYPE_STREAM => "VSOCK_TYPE_STREAM",
            uapi::VSOCK_TYPE_SEQPACKET => "VSOCK_TYPE_SEQPACKET",
            _ => "UNKNOWN_TYPE",
        };
        let op = match self.op {
            uapi::VSOCK_OP_INVALID => "VIRTIO_VSOCK_OP_INVALID",
            uapi::VSOCK_OP_REQUEST => "VIRTIO_VSOCK_OP_REQUEST",
            uapi::VSOCK_OP_RESPONSE => "VIRTIO_VSOCK_OP_RESPONSE",
            uapi::VSOCK_OP_RST => "VIRTIO_VSOCK_OP_RST",
            uapi::VSOCK_OP_SHUTDOWN => "VIRTIO_VSOCK_OP_SHUTDOWN",
            uapi::VSOCK_OP_RW => "VIRTIO_VSOCK_OP_RW",
            uapi::VSOCK_OP_CREDIT_UPDATE => "VIRTIO_VSOCK_OP_CREDIT_UPDATE",
            uapi::VSOCK_OP_CREDIT_REQUEST => "VIRTIO_VSOCK_OP_CREDIT_REQUEST",
            _ => "UNKNOWN_OP",
        };
        f.debug_struct("VirtioVsockHdr")
            .field("src_cid", &self.src_cid)
            .field("dst_cid", &self.dst_cid)
            .field("src_port", &self.src_port)
            .field("dst_port", &self.dst_port)
            .field("len", &self.len)
            .field("type_field", &type_field)
            .field("op", &op)
            .field("flags", &self.flags)
            .field("buf_alloc", &self.buf_alloc)
            .field("fwd_cnt", &self.fwd_cnt)
            .finish()
    }
}

/// The vsock packet, implemented as a wrapper over a virtq descriptor chain:
/// - the chain head, holding the packet header; and
/// - (an optional) data/buffer descriptor, only present for data packets (VSOCK_OP_RW).
///
pub struct VsockPacket {
    hdr: *mut u8,
    buf: Option<*mut u8>,
    buf_size: usize,
}

impl VsockPacket {
    /// Create the packet wrapper from a TX virtq chain head.
    ///
    /// The chain head is expected to hold valid packet header data. A following packet buffer
    /// descriptor can optionally end the chain. Bounds and pointer checks are performed when
    /// creating the wrapper.
    ///
    pub fn from_tx_virtq_head<M>(
        desc_chain: &mut DescriptorChain<M>,
        access_platform: Option<&Arc<dyn AccessPlatform>>,
    ) -> Result<Self>
    where
        M: Clone + Deref,
        M::Target: GuestMemory,
    {
        let head = desc_chain.next().ok_or(VsockError::HdrDescMissing)?;

        // All buffers in the TX queue must be readable.
        //
        if head.is_write_only() {
            return Err(VsockError::UnreadableDescriptor);
        }

        // The packet header should fit inside the head descriptor.
        if head.len() < VSOCK_PKT_HDR_SIZE as u32 {
            return Err(VsockError::HdrDescTooSmall(head.len()));
        }

        let mut pkt = Self {
            hdr: get_host_address_range(
                desc_chain.memory(),
                head.addr()
                    .translate_gva(access_platform, VSOCK_PKT_HDR_SIZE),
                VSOCK_PKT_HDR_SIZE,
            )
            .ok_or(VsockError::GuestMemory)?,
            buf: None,
            buf_size: 0,
        };

        // No point looking for a data/buffer descriptor, if the packet is zero-lengthed.
        if pkt.is_empty() {
            return Ok(pkt);
        }

        // Reject weirdly-sized packets.
        //
        if pkt.len() > defs::MAX_PKT_BUF_SIZE as u32 {
            return Err(VsockError::InvalidPktLen(pkt.len()));
        }

        // Prior to Linux v6.3 there are two descriptors
        if head.has_next() {
            let buf_desc = desc_chain.next().ok_or(VsockError::BufDescMissing)?;

            // TX data should be read-only.
            if buf_desc.is_write_only() {
                return Err(VsockError::UnreadableDescriptor);
            }

            // The data buffer should be large enough to fit the size of the data, as described by
            // the header descriptor.
            if buf_desc.len() < pkt.len() {
                return Err(VsockError::BufDescTooSmall);
            }
            let buf_size = buf_desc.len() as usize;
            pkt.buf_size = buf_size;
            pkt.buf = Some(
                get_host_address_range(
                    desc_chain.memory(),
                    buf_desc.addr().translate_gva(access_platform, buf_size),
                    pkt.buf_size,
                )
                .ok_or(VsockError::GuestMemory)?,
            );
        } else {
            let buf_size: usize = head.len() as usize - VSOCK_PKT_HDR_SIZE;
            pkt.buf_size = buf_size;
            pkt.buf = Some(
                get_host_address_range(
                    desc_chain.memory(),
                    head.addr()
                        .checked_add(VSOCK_PKT_HDR_SIZE as u64)
                        .unwrap()
                        .translate_gva(access_platform, buf_size),
                    buf_size,
                )
                .ok_or(VsockError::GuestMemory)?,
            );
        }

        Ok(pkt)
    }

    /// Create the packet wrapper from an RX virtq chain head.
    ///
    /// There must be two descriptors in the chain, both writable: a header descriptor and a data
    /// descriptor. Bounds and pointer checks are performed when creating the wrapper.
    ///
    pub fn from_rx_virtq_head<M>(
        desc_chain: &mut DescriptorChain<M>,
        access_platform: Option<&Arc<dyn AccessPlatform>>,
    ) -> Result<Self>
    where
        M: Clone + Deref,
        M::Target: GuestMemory,
    {
        let head = desc_chain.next().ok_or(VsockError::HdrDescMissing)?;

        // All RX buffers must be writable.
        //
        if !head.is_write_only() {
            return Err(VsockError::UnwritableDescriptor);
        }

        // The packet header should fit inside the head descriptor.
        if head.len() < VSOCK_PKT_HDR_SIZE as u32 {
            return Err(VsockError::HdrDescTooSmall(head.len()));
        }

        // Prior to Linux v6.3 there are two descriptors
        if head.has_next() {
            let buf_desc = desc_chain.next().ok_or(VsockError::BufDescMissing)?;
            let buf_size = buf_desc.len() as usize;

            Ok(Self {
                hdr: get_host_address_range(
                    desc_chain.memory(),
                    head.addr()
                        .translate_gva(access_platform, VSOCK_PKT_HDR_SIZE),
                    VSOCK_PKT_HDR_SIZE,
                )
                .ok_or(VsockError::GuestMemory)?,
                buf: Some(
                    get_host_address_range(
                        desc_chain.memory(),
                        buf_desc.addr().translate_gva(access_platform, buf_size),
                        buf_size,
                    )
                    .ok_or(VsockError::GuestMemory)?,
                ),
                buf_size,
            })
        } else {
            let buf_size: usize = head.len() as usize - VSOCK_PKT_HDR_SIZE;
            Ok(Self {
                hdr: get_host_address_range(
                    desc_chain.memory(),
                    head.addr()
                        .translate_gva(access_platform, VSOCK_PKT_HDR_SIZE),
                    VSOCK_PKT_HDR_SIZE,
                )
                .ok_or(VsockError::GuestMemory)?,
                buf: Some(
                    get_host_address_range(
                        desc_chain.memory(),
                        head.addr()
                            .checked_add(VSOCK_PKT_HDR_SIZE as u64)
                            .unwrap()
                            .translate_gva(access_platform, buf_size),
                        buf_size,
                    )
                    .ok_or(VsockError::GuestMemory)?,
                ),
                buf_size,
            })
        }
    }

    /// Provides in-place, byte-slice, access to the vsock packet header.
    ///
    pub fn hdr(&self) -> &[u8] {
        // This is safe since bound checks have already been performed when creating the packet
        // from the virtq descriptor.
        unsafe { std::slice::from_raw_parts(self.hdr as *const u8, VSOCK_PKT_HDR_SIZE) }
    }

    /// Provides in-place, byte-slice, mutable access to the vsock packet header.
    ///
    pub fn hdr_mut(&mut self) -> &mut [u8] {
        // This is safe since bound checks have already been performed when creating the packet
        // from the virtq descriptor.
        unsafe { std::slice::from_raw_parts_mut(self.hdr, VSOCK_PKT_HDR_SIZE) }
    }

    /// Provides in-place, byte-slice access to the vsock packet data buffer.
    ///
    /// Note: control packets (e.g. connection request or reset) have no data buffer associated.
    ///       For those packets, this method will return `None`.
    /// Also note: calling `len()` on the returned slice will yield the buffer size, which may be
    ///            (and often is) larger than the length of the packet data. The packet data length
    ///            is stored in the packet header, and accessible via `VsockPacket::len()`.
    pub fn buf(&self) -> Option<&[u8]> {
        self.buf.map(|ptr| {
            // This is safe since bound checks have already been performed when creating the packet
            // from the virtq descriptor.
            unsafe { std::slice::from_raw_parts(ptr as *const u8, self.buf_size) }
        })
    }

    /// Provides in-place, byte-slice, mutable access to the vsock packet data buffer.
    ///
    /// Note: control packets (e.g. connection request or reset) have no data buffer associated.
    ///       For those packets, this method will return `None`.
    /// Also note: calling `len()` on the returned slice will yield the buffer size, which may be
    ///            (and often is) larger than the length of the packet data. The packet data length
    ///            is stored in the packet header, and accessible via `VsockPacket::len()`.
    pub fn buf_mut(&mut self) -> Option<&mut [u8]> {
        self.buf.map(|ptr| {
            // This is safe since bound checks have already been performed when creating the packet
            // from the virtq descriptor.
            unsafe { std::slice::from_raw_parts_mut(ptr, self.buf_size) }
        })
    }

    pub fn src_cid(&self) -> u64 {
        LittleEndian::read_u64(&self.hdr()[HDROFF_SRC_CID..])
    }

    pub fn set_src_cid(&mut self, cid: u64) -> &mut Self {
        LittleEndian::write_u64(&mut self.hdr_mut()[HDROFF_SRC_CID..], cid);
        self
    }

    pub fn dst_cid(&self) -> u64 {
        LittleEndian::read_u64(&self.hdr()[HDROFF_DST_CID..])
    }

    pub fn set_dst_cid(&mut self, cid: u64) -> &mut Self {
        LittleEndian::write_u64(&mut self.hdr_mut()[HDROFF_DST_CID..], cid);
        self
    }

    pub fn src_port(&self) -> u32 {
        LittleEndian::read_u32(&self.hdr()[HDROFF_SRC_PORT..])
    }

    pub fn set_src_port(&mut self, port: u32) -> &mut Self {
        LittleEndian::write_u32(&mut self.hdr_mut()[HDROFF_SRC_PORT..], port);
        self
    }

    pub fn dst_port(&self) -> u32 {
        LittleEndian::read_u32(&self.hdr()[HDROFF_DST_PORT..])
    }

    pub fn set_dst_port(&mut self, port: u32) -> &mut Self {
        LittleEndian::write_u32(&mut self.hdr_mut()[HDROFF_DST_PORT..], port);
        self
    }

    pub fn len(&self) -> u32 {
        LittleEndian::read_u32(&self.hdr()[HDROFF_LEN..])
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    pub fn set_len(&mut self, len: u32) -> &mut Self {
        LittleEndian::write_u32(&mut self.hdr_mut()[HDROFF_LEN..], len);
        self
    }

    pub fn type_(&self) -> u16 {
        LittleEndian::read_u16(&self.hdr()[HDROFF_TYPE..])
    }

    pub fn set_type(&mut self, type_: u16) -> &mut Self {
        LittleEndian::write_u16(&mut self.hdr_mut()[HDROFF_TYPE..], type_);
        self
    }

    pub fn op(&self) -> u16 {
        LittleEndian::read_u16(&self.hdr()[HDROFF_OP..])
    }

    pub fn set_op(&mut self, op: u16) -> &mut Self {
        LittleEndian::write_u16(&mut self.hdr_mut()[HDROFF_OP..], op);
        self
    }

    pub fn flags(&self) -> u32 {
        LittleEndian::read_u32(&self.hdr()[HDROFF_FLAGS..])
    }

    pub fn set_flags(&mut self, flags: u32) -> &mut Self {
        LittleEndian::write_u32(&mut self.hdr_mut()[HDROFF_FLAGS..], flags);
        self
    }

    pub fn set_flag(&mut self, flag: u32) -> &mut Self {
        self.set_flags(self.flags() | flag);
        self
    }

    pub fn buf_alloc(&self) -> u32 {
        LittleEndian::read_u32(&self.hdr()[HDROFF_BUF_ALLOC..])
    }

    pub fn set_buf_alloc(&mut self, buf_alloc: u32) -> &mut Self {
        LittleEndian::write_u32(&mut self.hdr_mut()[HDROFF_BUF_ALLOC..], buf_alloc);
        self
    }

    pub fn fwd_cnt(&self) -> u32 {
        LittleEndian::read_u32(&self.hdr()[HDROFF_FWD_CNT..])
    }

    pub fn set_fwd_cnt(&mut self, fwd_cnt: u32) -> &mut Self {
        LittleEndian::write_u32(&mut self.hdr_mut()[HDROFF_FWD_CNT..], fwd_cnt);
        self
    }
}

#[cfg(test)]
mod tests {
    use super::super::tests::TestContext;
    use super::*;
    use crate::vsock::defs::MAX_PKT_BUF_SIZE;
    use crate::GuestMemoryMmap;
    use virtio_bindings::bindings::virtio_ring::VRING_DESC_F_WRITE;
    use virtio_queue::QueueOwnedT;
    use vm_memory::GuestAddress;
    use vm_virtio::queue::testing::VirtqDesc as GuestQDesc;

    macro_rules! create_context {
        ($test_ctx:ident, $handler_ctx:ident) => {
            let $test_ctx = TestContext::new();
            let mut $handler_ctx = $test_ctx.create_epoll_handler_context();
            // For TX packets, hdr.len should be set to a valid value.
            set_pkt_len(1024, &$handler_ctx.guest_txvq.dtable[0], &$test_ctx.mem);
        };
    }

    macro_rules! expect_asm_error {
        (tx, $test_ctx:expr, $handler_ctx:expr, $err:pat) => {
            expect_asm_error!($test_ctx, $handler_ctx, $err, from_tx_virtq_head, 1);
        };
        (rx, $test_ctx:expr, $handler_ctx:expr, $err:pat) => {
            expect_asm_error!($test_ctx, $handler_ctx, $err, from_rx_virtq_head, 0);
        };
        ($test_ctx:expr, $handler_ctx:expr, $err:pat, $ctor:ident, $vq:expr) => {
            match VsockPacket::$ctor(
                &mut $handler_ctx.handler.queues[$vq]
                    .iter(&$test_ctx.mem)
                    .unwrap()
                    .next()
                    .unwrap(),
                None,
            ) {
                Err($err) => (),
                Ok(_) => panic!("Packet assembly should've failed!"),
                Err(other) => panic!("Packet assembly failed with: {:?}", other),
            }
        };
    }

    fn set_pkt_len(len: u32, guest_desc: &GuestQDesc, mem: &GuestMemoryMmap) {
        let hdr_gpa = guest_desc.addr.get();
        let hdr_ptr =
            get_host_address_range(mem, GuestAddress(hdr_gpa), VSOCK_PKT_HDR_SIZE).unwrap();
        let len_ptr = unsafe { hdr_ptr.add(HDROFF_LEN) };

        LittleEndian::write_u32(unsafe { std::slice::from_raw_parts_mut(len_ptr, 4) }, len);
    }

    #[test]
    #[allow(clippy::cognitive_complexity)]
    fn test_tx_packet_assembly() {
        // Test case: successful TX packet assembly.
        {
            create_context!(test_ctx, handler_ctx);

            let pkt = VsockPacket::from_tx_virtq_head(
                &mut handler_ctx.handler.queues[1]
                    .iter(&test_ctx.mem)
                    .unwrap()
                    .next()
                    .unwrap(),
                None,
            )
            .unwrap();
            assert_eq!(pkt.hdr().len(), VSOCK_PKT_HDR_SIZE);
            assert_eq!(
                pkt.buf().unwrap().len(),
                handler_ctx.guest_txvq.dtable[1].len.get() as usize
            );
        }

        // Test case: error on write-only hdr descriptor.
        {
            create_context!(test_ctx, handler_ctx);
            handler_ctx.guest_txvq.dtable[0]
                .flags
                .set(VRING_DESC_F_WRITE.try_into().unwrap());
            expect_asm_error!(tx, test_ctx, handler_ctx, VsockError::UnreadableDescriptor);
        }

        // Test case: header descriptor has insufficient space to hold the packet header.
        {
            create_context!(test_ctx, handler_ctx);
            handler_ctx.guest_txvq.dtable[0]
                .len
                .set(VSOCK_PKT_HDR_SIZE as u32 - 1);
            expect_asm_error!(tx, test_ctx, handler_ctx, VsockError::HdrDescTooSmall(_));
        }

        // Test case: zero-length TX packet.
        {
            create_context!(test_ctx, handler_ctx);
            set_pkt_len(0, &handler_ctx.guest_txvq.dtable[0], &test_ctx.mem);
            let mut pkt = VsockPacket::from_tx_virtq_head(
                &mut handler_ctx.handler.queues[1]
                    .iter(&test_ctx.mem)
                    .unwrap()
                    .next()
                    .unwrap(),
                None,
            )
            .unwrap();
            assert!(pkt.buf().is_none());
            assert!(pkt.buf_mut().is_none());
        }

        // Test case: TX packet has more data than we can handle.
        {
            create_context!(test_ctx, handler_ctx);
            set_pkt_len(
                MAX_PKT_BUF_SIZE as u32 + 1,
                &handler_ctx.guest_txvq.dtable[0],
                &test_ctx.mem,
            );
            expect_asm_error!(tx, test_ctx, handler_ctx, VsockError::InvalidPktLen(_));
        }

        // Test case: error on write-only buf descriptor.
        {
            create_context!(test_ctx, handler_ctx);
            handler_ctx.guest_txvq.dtable[1]
                .flags
                .set(VRING_DESC_F_WRITE.try_into().unwrap());
            expect_asm_error!(tx, test_ctx, handler_ctx, VsockError::UnreadableDescriptor);
        }

        // Test case: the buffer descriptor cannot fit all the data advertised by the
        // packet header `len` field.
        {
            create_context!(test_ctx, handler_ctx);
            set_pkt_len(8 * 1024, &handler_ctx.guest_txvq.dtable[0], &test_ctx.mem);
            handler_ctx.guest_txvq.dtable[1].len.set(4 * 1024);
            expect_asm_error!(tx, test_ctx, handler_ctx, VsockError::BufDescTooSmall);
        }
    }

    #[test]
    fn test_rx_packet_assembly() {
        // Test case: successful RX packet assembly.
        {
            create_context!(test_ctx, handler_ctx);
            let pkt = VsockPacket::from_rx_virtq_head(
                &mut handler_ctx.handler.queues[0]
                    .iter(&test_ctx.mem)
                    .unwrap()
                    .next()
                    .unwrap(),
                None,
            )
            .unwrap();
            assert_eq!(pkt.hdr().len(), VSOCK_PKT_HDR_SIZE);
            assert_eq!(
                pkt.buf().unwrap().len(),
                handler_ctx.guest_rxvq.dtable[1].len.get() as usize
            );
        }

        // Test case: read-only RX packet header.
        {
            create_context!(test_ctx, handler_ctx);
            handler_ctx.guest_rxvq.dtable[0].flags.set(0);
            expect_asm_error!(rx, test_ctx, handler_ctx, VsockError::UnwritableDescriptor);
        }

        // Test case: RX descriptor head cannot fit the entire packet header.
        {
            create_context!(test_ctx, handler_ctx);
            handler_ctx.guest_rxvq.dtable[0]
                .len
                .set(VSOCK_PKT_HDR_SIZE as u32 - 1);
            expect_asm_error!(rx, test_ctx, handler_ctx, VsockError::HdrDescTooSmall(_));
        }
    }

    #[test]
    #[allow(clippy::cognitive_complexity)]
    fn test_packet_hdr_accessors() {
        const SRC_CID: u64 = 1;
        const DST_CID: u64 = 2;
        const SRC_PORT: u32 = 3;
        const DST_PORT: u32 = 4;
        const LEN: u32 = 5;
        const TYPE: u16 = 6;
        const OP: u16 = 7;
        const FLAGS: u32 = 8;
        const BUF_ALLOC: u32 = 9;
        const FWD_CNT: u32 = 10;

        create_context!(test_ctx, handler_ctx);
        let mut pkt = VsockPacket::from_rx_virtq_head(
            &mut handler_ctx.handler.queues[0]
                .iter(&test_ctx.mem)
                .unwrap()
                .next()
                .unwrap(),
            None,
        )
        .unwrap();

        // Test field accessors.
        pkt.set_src_cid(SRC_CID)
            .set_dst_cid(DST_CID)
            .set_src_port(SRC_PORT)
            .set_dst_port(DST_PORT)
            .set_len(LEN)
            .set_type(TYPE)
            .set_op(OP)
            .set_flags(FLAGS)
            .set_buf_alloc(BUF_ALLOC)
            .set_fwd_cnt(FWD_CNT);

        assert_eq!(pkt.src_cid(), SRC_CID);
        assert_eq!(pkt.dst_cid(), DST_CID);
        assert_eq!(pkt.src_port(), SRC_PORT);
        assert_eq!(pkt.dst_port(), DST_PORT);
        assert_eq!(pkt.len(), LEN);
        assert_eq!(pkt.type_(), TYPE);
        assert_eq!(pkt.op(), OP);
        assert_eq!(pkt.flags(), FLAGS);
        assert_eq!(pkt.buf_alloc(), BUF_ALLOC);
        assert_eq!(pkt.fwd_cnt(), FWD_CNT);

        // Test individual flag setting.
        let flags = pkt.flags() | 0b1000;
        pkt.set_flag(0b1000);
        assert_eq!(pkt.flags(), flags);

        // Test packet header as-slice access.
        //

        assert_eq!(pkt.hdr().len(), VSOCK_PKT_HDR_SIZE);

        assert_eq!(
            SRC_CID,
            LittleEndian::read_u64(&pkt.hdr()[HDROFF_SRC_CID..])
        );
        assert_eq!(
            DST_CID,
            LittleEndian::read_u64(&pkt.hdr()[HDROFF_DST_CID..])
        );
        assert_eq!(
            SRC_PORT,
            LittleEndian::read_u32(&pkt.hdr()[HDROFF_SRC_PORT..])
        );
        assert_eq!(
            DST_PORT,
            LittleEndian::read_u32(&pkt.hdr()[HDROFF_DST_PORT..])
        );
        assert_eq!(LEN, LittleEndian::read_u32(&pkt.hdr()[HDROFF_LEN..]));
        assert_eq!(TYPE, LittleEndian::read_u16(&pkt.hdr()[HDROFF_TYPE..]));
        assert_eq!(OP, LittleEndian::read_u16(&pkt.hdr()[HDROFF_OP..]));
        assert_eq!(FLAGS, LittleEndian::read_u32(&pkt.hdr()[HDROFF_FLAGS..]));
        assert_eq!(
            BUF_ALLOC,
            LittleEndian::read_u32(&pkt.hdr()[HDROFF_BUF_ALLOC..])
        );
        assert_eq!(
            FWD_CNT,
            LittleEndian::read_u32(&pkt.hdr()[HDROFF_FWD_CNT..])
        );

        assert_eq!(pkt.hdr_mut().len(), VSOCK_PKT_HDR_SIZE);
        for b in pkt.hdr_mut() {
            *b = 0;
        }
        assert_eq!(pkt.src_cid(), 0);
        assert_eq!(pkt.dst_cid(), 0);
        assert_eq!(pkt.src_port(), 0);
        assert_eq!(pkt.dst_port(), 0);
        assert_eq!(pkt.len(), 0);
        assert_eq!(pkt.type_(), 0);
        assert_eq!(pkt.op(), 0);
        assert_eq!(pkt.flags(), 0);
        assert_eq!(pkt.buf_alloc(), 0);
        assert_eq!(pkt.fwd_cnt(), 0);
    }

    #[test]
    fn test_packet_buf() {
        create_context!(test_ctx, handler_ctx);
        let mut pkt = VsockPacket::from_rx_virtq_head(
            &mut handler_ctx.handler.queues[0]
                .iter(&test_ctx.mem)
                .unwrap()
                .next()
                .unwrap(),
            None,
        )
        .unwrap();

        assert_eq!(
            pkt.buf().unwrap().len(),
            handler_ctx.guest_rxvq.dtable[1].len.get() as usize
        );
        assert_eq!(
            pkt.buf_mut().unwrap().len(),
            handler_ctx.guest_rxvq.dtable[1].len.get() as usize
        );

        for i in 0..pkt.buf().unwrap().len() {
            pkt.buf_mut().unwrap()[i] = (i % 0x100) as u8;
            assert_eq!(pkt.buf().unwrap()[i], (i % 0x100) as u8);
        }
    }
}
