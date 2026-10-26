// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use byteorder::{BigEndian, ByteOrder};
use uuid::Uuid;

macro_rules! div_round_up {
    ($n:expr,$d:expr) => {
        ($n + $d - 1) / $d
    };
}

pub mod vhdx;
mod vhdx_bat;
mod vhdx_header;
mod vhdx_io;
mod vhdx_metadata;

pub(crate) fn uuid_from_guid(buf: &[u8]) -> Uuid {
    // The first 3 fields of UUID are stored in Big Endian format, and
    // the last 8 bytes are stored as byte array. Therefore, we read the
    // first 3 fields in Big Endian format instead of Little Endian.
    Uuid::from_fields_le(
        BigEndian::read_u32(&buf[0..4]),
        BigEndian::read_u16(&buf[4..6]),
        BigEndian::read_u16(&buf[6..8]),
        buf[8..16].try_into().unwrap(),
    )
}
