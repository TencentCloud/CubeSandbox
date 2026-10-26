// Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

pub const MP_PROCESSOR: ::std::os::raw::c_uint = 0;
pub const MP_BUS: ::std::os::raw::c_uint = 1;
pub const MP_IOAPIC: ::std::os::raw::c_uint = 2;
pub const MP_INTSRC: ::std::os::raw::c_uint = 3;
pub const MP_LINTSRC: ::std::os::raw::c_uint = 4;
pub const CPU_ENABLED: ::std::os::raw::c_uint = 1;
pub const CPU_BOOTPROCESSOR: ::std::os::raw::c_uint = 2;
pub const MPC_APIC_USABLE: ::std::os::raw::c_uint = 1;
pub const MP_IRQDIR_DEFAULT: ::std::os::raw::c_uint = 0;

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpf_intel {
    pub signature: [::std::os::raw::c_char; 4usize],
    pub physptr: ::std::os::raw::c_uint,
    pub length: ::std::os::raw::c_uchar,
    pub specification: ::std::os::raw::c_uchar,
    pub checksum: ::std::os::raw::c_uchar,
    pub feature1: ::std::os::raw::c_uchar,
    pub feature2: ::std::os::raw::c_uchar,
    pub feature3: ::std::os::raw::c_uchar,
    pub feature4: ::std::os::raw::c_uchar,
    pub feature5: ::std::os::raw::c_uchar,
}

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpc_table {
    pub signature: [::std::os::raw::c_char; 4usize],
    pub length: ::std::os::raw::c_ushort,
    pub spec: ::std::os::raw::c_char,
    pub checksum: ::std::os::raw::c_char,
    pub oem: [::std::os::raw::c_char; 8usize],
    pub productid: [::std::os::raw::c_char; 12usize],
    pub oemptr: ::std::os::raw::c_uint,
    pub oemsize: ::std::os::raw::c_ushort,
    pub oemcount: ::std::os::raw::c_ushort,
    pub lapic: ::std::os::raw::c_uint,
    pub reserved: ::std::os::raw::c_uint,
}

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpc_cpu {
    pub type_: ::std::os::raw::c_uchar,
    pub apicid: ::std::os::raw::c_uchar,
    pub apicver: ::std::os::raw::c_uchar,
    pub cpuflag: ::std::os::raw::c_uchar,
    pub cpufeature: ::std::os::raw::c_uint,
    pub featureflag: ::std::os::raw::c_uint,
    pub reserved: [::std::os::raw::c_uint; 2usize],
}

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpc_bus {
    pub type_: ::std::os::raw::c_uchar,
    pub busid: ::std::os::raw::c_uchar,
    pub bustype: [::std::os::raw::c_uchar; 6usize],
}

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpc_ioapic {
    pub type_: ::std::os::raw::c_uchar,
    pub apicid: ::std::os::raw::c_uchar,
    pub apicver: ::std::os::raw::c_uchar,
    pub flags: ::std::os::raw::c_uchar,
    pub apicaddr: ::std::os::raw::c_uint,
}

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpc_intsrc {
    pub type_: ::std::os::raw::c_uchar,
    pub irqtype: ::std::os::raw::c_uchar,
    pub irqflag: ::std::os::raw::c_ushort,
    pub srcbus: ::std::os::raw::c_uchar,
    pub srcbusirq: ::std::os::raw::c_uchar,
    pub dstapic: ::std::os::raw::c_uchar,
    pub dstirq: ::std::os::raw::c_uchar,
}

pub const MP_IRQ_SOURCE_TYPES_MP_INT: ::std::os::raw::c_uint = 0;
pub const MP_IRQ_SOURCE_TYPES_MP_NMI: ::std::os::raw::c_uint = 1;
pub const MP_IRQ_SOURCE_TYPES_MP_EXT_INT: ::std::os::raw::c_uint = 3;

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpc_lintsrc {
    pub type_: ::std::os::raw::c_uchar,
    pub irqtype: ::std::os::raw::c_uchar,
    pub irqflag: ::std::os::raw::c_ushort,
    pub srcbusid: ::std::os::raw::c_uchar,
    pub srcbusirq: ::std::os::raw::c_uchar,
    pub destapic: ::std::os::raw::c_uchar,
    pub destapiclint: ::std::os::raw::c_uchar,
}

#[repr(C)]
#[derive(Debug, Default, Copy, Clone)]
pub struct mpc_oemtable {
    pub signature: [::std::os::raw::c_char; 4usize],
    pub length: ::std::os::raw::c_ushort,
    pub rev: ::std::os::raw::c_char,
    pub checksum: ::std::os::raw::c_char,
    pub mpc: [::std::os::raw::c_char; 8usize],
}
