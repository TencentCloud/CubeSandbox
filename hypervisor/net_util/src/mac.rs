// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the THIRD-PARTY file.

use std::fmt;
use std::io;
use std::result::Result;
use std::str::FromStr;

use serde::de::{Deserialize, Deserializer, Error};
use serde::ser::{Serialize, Serializer};

pub const MAC_ADDR_LEN: usize = 6;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct MacAddr {
    bytes: [u8; MAC_ADDR_LEN],
}

impl MacAddr {
    pub fn parse_str<S>(s: &S) -> Result<MacAddr, io::Error>
    where
        S: AsRef<str> + ?Sized,
    {
        let v: Vec<&str> = s.as_ref().split(':').collect();
        let mut bytes = [0u8; MAC_ADDR_LEN];
        let common_err = Err(io::Error::new(
            io::ErrorKind::Other,
            format!("parsing of {} into a MAC address failed", s.as_ref()),
        ));

        if v.len() != MAC_ADDR_LEN {
            return common_err;
        }

        for i in 0..MAC_ADDR_LEN {
            if v[i].len() != 2 {
                return common_err;
            }
            bytes[i] = u8::from_str_radix(v[i], 16).map_err(|e| {
                io::Error::new(
                    io::ErrorKind::Other,
                    format!("parsing of {} into a MAC address failed: {}", s.as_ref(), e),
                )
            })?;
        }

        Ok(MacAddr { bytes })
    }

    // Does not check whether src.len() == MAC_ADDR_LEN.
    #[inline]
    pub fn from_bytes_unchecked(src: &[u8]) -> MacAddr {
        // TODO: using something like std::mem::uninitialized could avoid the extra initialization,
        // if this ever becomes a performance bottleneck.
        let mut bytes = [0u8; MAC_ADDR_LEN];
        bytes[..].copy_from_slice(src);

        MacAddr { bytes }
    }

    // An error can only occur if the slice length is different from MAC_ADDR_LEN.
    #[inline]
    pub fn from_bytes(src: &[u8]) -> Result<MacAddr, io::Error> {
        if src.len() != MAC_ADDR_LEN {
            return Err(io::Error::new(
                io::ErrorKind::Other,
                format!("invalid length of slice: {} vs {}", src.len(), MAC_ADDR_LEN),
            ));
        }
        Ok(MacAddr::from_bytes_unchecked(src))
    }

    #[inline]
    pub fn get_bytes(&self) -> &[u8] {
        &self.bytes
    }

    pub fn local_random() -> MacAddr {
        // Generate a fully random MAC
        let mut random_bytes = [0u8; MAC_ADDR_LEN];
        if let Err(e) = getrandom::getrandom(&mut random_bytes) {
            error!(
                "Error populating MAC address with random data: {}",
                e.to_string()
            );
        }

        // Set the first byte to make the OUI a locally administered OUI
        random_bytes[0] = 0x2e;

        MacAddr {
            bytes: random_bytes,
        }
    }
}

impl fmt::Display for MacAddr {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        let b = &self.bytes;
        write!(
            f,
            "{:02x}:{:02x}:{:02x}:{:02x}:{:02x}:{:02x}",
            b[0], b[1], b[2], b[3], b[4], b[5]
        )
    }
}

impl Serialize for MacAddr {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        self.to_string().serialize(serializer)
    }
}

impl<'de> Deserialize<'de> for MacAddr {
    fn deserialize<D>(deserializer: D) -> Result<MacAddr, D::Error>
    where
        D: Deserializer<'de>,
    {
        let s = String::deserialize(deserializer)?;
        MacAddr::parse_str(&s)
            .map_err(|e| D::Error::custom(format!("The provided MAC address is invalid: {}", e)))
    }
}

pub enum MacAddrParseError {
    InvalidValue(String),
}

impl FromStr for MacAddr {
    type Err = MacAddrParseError;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        MacAddr::parse_str(s).map_err(|_| MacAddrParseError::InvalidValue(s.to_owned()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_mac_addr() {
        // too long
        assert!(MacAddr::parse_str("aa:aa:aa:aa:aa:aa:aa").is_err());

        // invalid hex
        assert!(MacAddr::parse_str("aa:aa:aa:aa:aa:ax").is_err());

        // single digit mac address component should be invalid
        assert!(MacAddr::parse_str("aa:aa:aa:aa:aa:b").is_err());

        // components with more than two digits should also be invalid
        assert!(MacAddr::parse_str("aa:aa:aa:aa:aa:bbb").is_err());

        let mac = MacAddr::parse_str("12:34:56:78:9a:BC").unwrap();

        println!("parsed MAC address: {}", mac);

        let bytes = mac.get_bytes();
        assert_eq!(bytes, [0x12u8, 0x34, 0x56, 0x78, 0x9a, 0xbc]);
    }

    #[test]
    fn test_from_bytes() {
        let src1 = [0x01, 0x02, 0x03, 0x04, 0x05];
        let src2 = [0x01, 0x02, 0x03, 0x04, 0x05, 0x06];
        let src3 = [0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07];

        assert!(MacAddr::from_bytes(&src1[..]).is_err());

        let x = MacAddr::from_bytes(&src2[..]).unwrap();
        assert_eq!(x.to_string(), String::from("01:02:03:04:05:06"));

        assert!(MacAddr::from_bytes(&src3[..]).is_err());
    }

    #[test]
    fn test_mac_addr_serialization_and_deserialization() {
        let mac: MacAddr =
            serde_json::from_str("\"12:34:56:78:9a:bc\"").expect("MacAddr deserialization failed.");

        let bytes = mac.get_bytes();
        assert_eq!(bytes, [0x12u8, 0x34, 0x56, 0x78, 0x9a, 0xbc]);

        let s = serde_json::to_string(&mac).expect("MacAddr serialization failed.");
        assert_eq!(s, "\"12:34:56:78:9a:bc\"");
    }
}
