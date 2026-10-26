// SPDX-License-Identifier: BSD-3-Clause

#[cfg(target_env = "gnu")]
pub use libc::statx as statx_st;

#[cfg(target_env = "gnu")]
pub use libc::{STATX_BASIC_STATS, STATX_MNT_ID};

// musl provides the 'struct statx', but without stx_mnt_id.
// However, the libc crate does not provide libc::statx
// if musl is used. So we add just the required struct and
// constants to make it works.

#[cfg(not(target_env = "gnu"))]
#[repr(C)]
pub struct statx_st_timestamp {
    pub tv_sec: i64,
    pub tv_nsec: u32,
    pub __statx_timestamp_pad1: [i32; 1],
}

#[cfg(not(target_env = "gnu"))]
#[repr(C)]
pub struct statx_st {
    pub stx_mask: u32,
    pub stx_blksize: u32,
    pub stx_attributes: u64,
    pub stx_nlink: u32,
    pub stx_uid: u32,
    pub stx_gid: u32,
    pub stx_mode: u16,
    __statx_pad1: [u16; 1],
    pub stx_ino: u64,
    pub stx_size: u64,
    pub stx_blocks: u64,
    pub stx_attributes_mask: u64,
    pub stx_atime: statx_st_timestamp,
    pub stx_btime: statx_st_timestamp,
    pub stx_ctime: statx_st_timestamp,
    pub stx_mtime: statx_st_timestamp,
    pub stx_rdev_major: u32,
    pub stx_rdev_minor: u32,
    pub stx_dev_major: u32,
    pub stx_dev_minor: u32,
    pub stx_mnt_id: u64,
    __statx_pad2: u64,
    __statx_pad3: [u64; 12],
}

#[cfg(not(target_env = "gnu"))]
pub const STATX_BASIC_STATS: libc::c_uint = 0x07ff;

#[cfg(not(target_env = "gnu"))]
pub const STATX_MNT_ID: libc::c_uint = 0x1000;
