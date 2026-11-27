// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! NBD wire protocol definitions.
//!
//! Constants and fixed-size message layouts from the NBD protocol
//! specification (<https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md>).
//! All integers on the wire are big endian.

use std::convert::TryInto;

/// Default TCP port of an NBD server.
pub const NBD_DEFAULT_PORT: u16 = 10809;

/// Largest payload a client may send or request unless the server
/// advertises a different maximum via `NBD_INFO_BLOCK_SIZE`.
pub const NBD_DEFAULT_MAX_PAYLOAD: u32 = 32 * 1024 * 1024;

/// Upper bound for strings exchanged during the handshake.
pub const NBD_MAX_STRING_SIZE: usize = 4096;

// Handshake magics.
pub const NBD_INIT_MAGIC: u64 = 0x4e42_444d_4147_4943; // "NBDMAGIC"
pub const NBD_OPTS_MAGIC: u64 = 0x4948_4156_454f_5054; // "IHAVEOPT"
pub const NBD_CLIENT_MAGIC: u64 = 0x0000_4202_8186_1253; // oldstyle
pub const NBD_REP_MAGIC: u64 = 0x0003_e889_0455_65a9;

// Handshake flags (server) and client flags.
pub const NBD_FLAG_FIXED_NEWSTYLE: u16 = 1 << 0;
pub const NBD_FLAG_NO_ZEROES: u16 = 1 << 1;
pub const NBD_FLAG_C_FIXED_NEWSTYLE: u32 = 1 << 0;
pub const NBD_FLAG_C_NO_ZEROES: u32 = 1 << 1;

// Transmission flags.
pub const NBD_FLAG_HAS_FLAGS: u16 = 1 << 0;
pub const NBD_FLAG_READ_ONLY: u16 = 1 << 1;
pub const NBD_FLAG_SEND_FLUSH: u16 = 1 << 2;
pub const NBD_FLAG_SEND_FUA: u16 = 1 << 3;
pub const NBD_FLAG_SEND_TRIM: u16 = 1 << 5;
pub const NBD_FLAG_SEND_WRITE_ZEROES: u16 = 1 << 6;
pub const NBD_FLAG_CAN_MULTI_CONN: u16 = 1 << 8;

// Options.
pub const NBD_OPT_EXPORT_NAME: u32 = 1;
pub const NBD_OPT_GO: u32 = 7;

// Option reply types.
pub const NBD_REP_ACK: u32 = 1;
pub const NBD_REP_INFO: u32 = 3;
pub const NBD_REP_FLAG_ERROR: u32 = 1 << 31;
pub const NBD_REP_ERR_UNSUP: u32 = NBD_REP_FLAG_ERROR | 1;
pub const NBD_REP_ERR_TLS_REQD: u32 = NBD_REP_FLAG_ERROR | 5;
pub const NBD_REP_ERR_UNKNOWN: u32 = NBD_REP_FLAG_ERROR | 6;

// Info types for NBD_OPT_GO.
pub const NBD_INFO_EXPORT: u16 = 0;
pub const NBD_INFO_BLOCK_SIZE: u16 = 3;

// Transmission phase magics.
pub const NBD_REQUEST_MAGIC: u32 = 0x2560_9513;
pub const NBD_SIMPLE_REPLY_MAGIC: u32 = 0x6744_6698;
pub const NBD_STRUCTURED_REPLY_MAGIC: u32 = 0x668e_33ef;

// Transmission commands.
pub const NBD_CMD_READ: u16 = 0;
pub const NBD_CMD_WRITE: u16 = 1;
pub const NBD_CMD_DISC: u16 = 2;
pub const NBD_CMD_FLUSH: u16 = 3;
pub const NBD_CMD_TRIM: u16 = 4;
pub const NBD_CMD_WRITE_ZEROES: u16 = 6;

// Error values carried in NBD replies.
pub const NBD_SUCCESS: u32 = 0;
pub const NBD_EPERM: u32 = 1;
pub const NBD_EIO: u32 = 5;
pub const NBD_ENOMEM: u32 = 12;
pub const NBD_EINVAL: u32 = 22;
pub const NBD_ENOSPC: u32 = 28;
pub const NBD_EOVERFLOW: u32 = 75;
pub const NBD_ENOTSUP: u32 = 95;
pub const NBD_ESHUTDOWN: u32 = 108;

/// Size of a transmission request header in bytes.
pub const NBD_REQUEST_SIZE: usize = 28;
/// Size of a simple reply header in bytes.
pub const NBD_SIMPLE_REPLY_SIZE: usize = 16;

/// Transmission request header (28 bytes on wire).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NbdRequest {
    pub flags: u16,
    pub command: u16,
    pub cookie: u64,
    pub offset: u64,
    pub length: u32,
}

impl NbdRequest {
    pub fn encode(&self) -> [u8; NBD_REQUEST_SIZE] {
        let mut buf = [0u8; NBD_REQUEST_SIZE];
        buf[0..4].copy_from_slice(&NBD_REQUEST_MAGIC.to_be_bytes());
        buf[4..6].copy_from_slice(&self.flags.to_be_bytes());
        buf[6..8].copy_from_slice(&self.command.to_be_bytes());
        buf[8..16].copy_from_slice(&self.cookie.to_be_bytes());
        buf[16..24].copy_from_slice(&self.offset.to_be_bytes());
        buf[24..28].copy_from_slice(&self.length.to_be_bytes());
        buf
    }

    pub fn decode(buf: &[u8; NBD_REQUEST_SIZE]) -> Option<Self> {
        if u32::from_be_bytes(buf[0..4].try_into().unwrap()) != NBD_REQUEST_MAGIC {
            return None;
        }
        Some(Self {
            flags: u16::from_be_bytes(buf[4..6].try_into().unwrap()),
            command: u16::from_be_bytes(buf[6..8].try_into().unwrap()),
            cookie: u64::from_be_bytes(buf[8..16].try_into().unwrap()),
            offset: u64::from_be_bytes(buf[16..24].try_into().unwrap()),
            length: u32::from_be_bytes(buf[24..28].try_into().unwrap()),
        })
    }
}

/// Simple reply header (16 bytes on wire).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NbdSimpleReply {
    pub error: u32,
    pub cookie: u64,
}

impl NbdSimpleReply {
    /// Decodes a simple reply header, returning `None` on a bad magic.
    pub fn decode(buf: &[u8; NBD_SIMPLE_REPLY_SIZE]) -> Option<Self> {
        if u32::from_be_bytes(buf[0..4].try_into().unwrap()) != NBD_SIMPLE_REPLY_MAGIC {
            return None;
        }
        Some(Self {
            error: u32::from_be_bytes(buf[4..8].try_into().unwrap()),
            cookie: u64::from_be_bytes(buf[8..16].try_into().unwrap()),
        })
    }

    pub fn encode(&self) -> [u8; NBD_SIMPLE_REPLY_SIZE] {
        let mut buf = [0u8; NBD_SIMPLE_REPLY_SIZE];
        buf[0..4].copy_from_slice(&NBD_SIMPLE_REPLY_MAGIC.to_be_bytes());
        buf[4..8].copy_from_slice(&self.error.to_be_bytes());
        buf[8..16].copy_from_slice(&self.cookie.to_be_bytes());
        buf
    }
}

/// Maps an NBD reply error code to a positive `libc` errno.
pub fn nbd_errno_to_errno(error: u32) -> i32 {
    match error {
        NBD_SUCCESS => 0,
        NBD_EPERM => libc::EPERM,
        NBD_EIO => libc::EIO,
        NBD_ENOMEM => libc::ENOMEM,
        NBD_EINVAL => libc::EINVAL,
        NBD_ENOSPC => libc::ENOSPC,
        NBD_EOVERFLOW => libc::EOVERFLOW,
        NBD_ENOTSUP => libc::ENOTSUP,
        NBD_ESHUTDOWN => libc::ESHUTDOWN,
        _ => libc::EINVAL,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_round_trip() {
        let req = NbdRequest {
            flags: 0,
            command: NBD_CMD_WRITE,
            cookie: 0x0102_0304_0506_0708,
            offset: 4096,
            length: 512,
        };
        let buf = req.encode();
        assert_eq!(&buf[0..4], &[0x25, 0x60, 0x95, 0x13]);
        assert_eq!(NbdRequest::decode(&buf), Some(req));
    }

    #[test]
    fn reply_round_trip_and_bad_magic() {
        let reply = NbdSimpleReply {
            error: NBD_EIO,
            cookie: 42,
        };
        let mut buf = reply.encode();
        assert_eq!(NbdSimpleReply::decode(&buf), Some(reply));
        buf[0..4].copy_from_slice(&NBD_STRUCTURED_REPLY_MAGIC.to_be_bytes());
        assert_eq!(NbdSimpleReply::decode(&buf), None);
    }

    #[test]
    fn errno_mapping() {
        assert_eq!(nbd_errno_to_errno(NBD_SUCCESS), 0);
        assert_eq!(nbd_errno_to_errno(NBD_ENOSPC), libc::ENOSPC);
        assert_eq!(nbd_errno_to_errno(12345), libc::EINVAL);
    }
}
