// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 OR BSD-3-Clause
//
// Copyright © 2020, Microsoft Corporation
//

pub mod emulator;
pub mod gdt;
#[allow(non_camel_case_types)]
#[allow(non_snake_case)]
#[allow(non_upper_case_globals)]
pub mod msr_index;

use serde::de::Error as SerdeError;
use serde::{Deserialize, Deserializer, Serializer};

// MTRR constants
pub const MTRR_ENABLE: u64 = 0x800; // IA32_MTRR_DEF_TYPE MSR: E (MTRRs enabled) flag, bit 11
pub const MTRR_MEM_TYPE_WB: u64 = 0x6;

// IOAPIC pins
pub const NUM_IOAPIC_PINS: usize = 24;

// X86 Exceptions
#[allow(clippy::upper_case_acronyms)]
#[derive(Clone, Debug)]
pub enum Exception {
    DE = 0,  // Divide Error
    DB = 1,  // Debug Exception
    BP = 3,  // Breakpoint
    OF = 4,  // Overflow
    BR = 5,  // BOUND Range Exceeded
    UD = 6,  // Invalid/Undefined Opcode
    NM = 7,  // No Math Coprocessor
    DF = 8,  // Double Fault
    TS = 10, // Invalid TSS
    NP = 11, // Segment Not Present
    SS = 12, // Stack Segment Fault
    GP = 13, // General Protection
    PF = 14, // Page Fault
    MF = 16, // Math Fault
    AC = 17, // Alignment Check
    MC = 18, // Machine Check
    XM = 19, // SIMD Floating-Point Exception
    VE = 20, // Virtualization Exception
    CP = 21, // Control Protection Exception
}

pub mod regs;

#[derive(Debug, Default, Copy, Clone, PartialEq, Eq)]
#[cfg_attr(feature = "with-serde", derive(Deserialize, Serialize))]
pub struct SegmentRegister {
    pub base: u64,
    pub limit: u32,
    pub selector: u16,
    pub type_: u8,
    pub present: u8,
    pub dpl: u8,
    pub db: u8,
    pub s: u8,
    pub l: u8,
    pub g: u8,
    pub avl: u8,
    pub unusable: u8,
}

impl SegmentRegister {
    pub fn segment_type(&self) -> u8 {
        self.type_
    }
    pub fn set_segment_type(&mut self, val: u8) {
        self.type_ = val;
    }

    pub fn dpl(&self) -> u8 {
        self.dpl
    }

    pub fn set_dpl(&mut self, val: u8) {
        self.dpl = val;
    }

    pub fn present(&self) -> u8 {
        self.present
    }

    pub fn set_present(&mut self, val: u8) {
        self.present = val;
    }

    pub fn long(&self) -> u8 {
        self.l
    }

    pub fn set_long(&mut self, val: u8) {
        self.l = val;
    }

    pub fn avl(&self) -> u8 {
        self.avl
    }

    pub fn set_avl(&mut self, val: u8) {
        self.avl = val;
    }

    pub fn desc_type(&self) -> u8 {
        self.s
    }

    pub fn set_desc_type(&mut self, val: u8) {
        self.s = val;
    }

    pub fn granularity(&self) -> u8 {
        self.g
    }

    pub fn set_granularity(&mut self, val: u8) {
        self.g = val;
    }

    pub fn db(&self) -> u8 {
        self.db
    }

    pub fn set_db(&mut self, val: u8) {
        self.db = val;
    }
}

// Code segment
pub const CODE_SEGMENT_TYPE: u8 = 0x8;

// Read/Write or Read/Exec segment
pub const RWRX_SEGMENT_TYPE: u8 = 0x2;

// Expand down segment
pub const EXPAND_DOWN_SEGMENT_TYPE: u8 = 0x4;

pub fn segment_type_code(t: u8) -> bool {
    t & CODE_SEGMENT_TYPE != 0
}

pub fn segment_type_ro(t: u8) -> bool {
    t & !RWRX_SEGMENT_TYPE == 0
}

pub fn segment_type_expand_down(t: u8) -> bool {
    !segment_type_code(t) && (t & EXPAND_DOWN_SEGMENT_TYPE != 0)
}
#[macro_export]
macro_rules! msr {
    ($msr:expr) => {
        MsrEntry {
            index: $msr,
            data: 0x0,
        }
    };
}
#[macro_export]
macro_rules! msr_data {
    ($msr:expr, $data:expr) => {
        MsrEntry {
            index: $msr,
            data: $data,
        }
    };
}

#[derive(Debug, Default, Copy, Clone, PartialEq, Eq)]
#[cfg_attr(feature = "with-serde", derive(Deserialize, Serialize))]
pub struct StandardRegisters {
    pub rax: u64,
    pub rbx: u64,
    pub rcx: u64,
    pub rdx: u64,
    pub rsi: u64,
    pub rdi: u64,
    pub rsp: u64,
    pub rbp: u64,
    pub r8: u64,
    pub r9: u64,
    pub r10: u64,
    pub r11: u64,
    pub r12: u64,
    pub r13: u64,
    pub r14: u64,
    pub r15: u64,
    pub rip: u64,
    pub rflags: u64,
}

#[derive(Debug, Default, Copy, Clone, PartialEq, Eq)]
#[cfg_attr(feature = "with-serde", derive(Deserialize, Serialize))]
pub struct DescriptorTable {
    pub base: u64,
    pub limit: u16,
}

#[derive(Debug, Default, Copy, Clone, PartialEq, Eq)]
#[cfg_attr(feature = "with-serde", derive(Deserialize, Serialize))]
pub struct SpecialRegisters {
    pub cs: SegmentRegister,
    pub ds: SegmentRegister,
    pub es: SegmentRegister,
    pub fs: SegmentRegister,
    pub gs: SegmentRegister,
    pub ss: SegmentRegister,
    pub tr: SegmentRegister,
    pub ldt: SegmentRegister,
    pub gdt: DescriptorTable,
    pub idt: DescriptorTable,
    pub cr0: u64,
    pub cr2: u64,
    pub cr3: u64,
    pub cr4: u64,
    pub cr8: u64,
    pub efer: u64,
    pub apic_base: u64,
    pub interrupt_bitmap: [u64; 4usize],
}

/// Serializes number to hex
pub fn serialize_to_hex_str<S, N>(number: &N, serializer: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
    N: std::fmt::LowerHex,
{
    serializer.serialize_str(format!("{:#x}", number).as_str())
}

macro_rules! deserialize_from_str {
    ($name:ident, $type:tt) => {
        /// Deserializes number from string.
        /// Number can be in binary, hex or dec formats.
        pub fn $name<'de, D>(deserializer: D) -> Result<$type, D::Error>
        where
            D: Deserializer<'de>,
        {
            let number_str = String::deserialize(deserializer)?;
            let deserialized_number = if let Some(s) = number_str.strip_prefix("0b") {
                $type::from_str_radix(s, 2)
            } else if let Some(s) = number_str.strip_prefix("0x") {
                $type::from_str_radix(s, 16)
            } else {
                return Err(D::Error::custom(format!(
                    "No supported number system prefix found in value [{}]. Make sure to prefix \
                     the number with '0x' for hexadecimal numbers or '0b' for binary numbers.",
                    number_str,
                )));
            }
            .map_err(|err| {
                D::Error::custom(format!(
                    "Failed to parse string [{}] as a number for CPU template - {:?}",
                    number_str, err
                ))
            })?;
            Ok(deserialized_number)
        }
    };
}

deserialize_from_str!(deserialize_from_str_u32, u32);
deserialize_from_str!(deserialize_from_str_u64, u64);

#[derive(Debug, Default, Copy, Clone, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
pub struct CpuIdEntry {
    pub function: u32,
    pub index: u32,
    pub flags: u32,
    pub eax: u32,
    pub ebx: u32,
    pub ecx: u32,
    pub edx: u32,
}

// To keep compatibility of old snaps, use a new structure.
// Old snaps member are u32, and we use string here, in order
// to get them from json file.
#[derive(Debug, Default, Copy, Clone, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
pub struct CpuIdCustomEntry {
    #[serde(
        deserialize_with = "deserialize_from_str_u32",
        serialize_with = "serialize_to_hex_str"
    )]
    pub function: u32,
    pub index: u32,
    pub flags: u32,
    pub eax: u32,
    pub ebx: u32,
    pub ecx: u32,
    pub edx: u32,
}

impl From<CpuIdCustomEntry> for CpuIdEntry {
    fn from(s: CpuIdCustomEntry) -> Self {
        Self {
            function: s.function,
            index: s.index,
            flags: s.flags,
            eax: s.eax,
            ebx: s.ebx,
            ecx: s.ecx,
            edx: s.edx,
        }
    }
}

pub const CPUID_FLAG_VALID_INDEX: u32 = 1;

#[derive(Default, Debug, Clone, serde::Deserialize, serde::Serialize)]
pub struct FpuState {
    pub fpr: [[u8; 16usize]; 8usize],
    pub fcw: u16,
    pub fsw: u16,
    pub ftwx: u8,
    pub last_opcode: u16,
    pub last_ip: u64,
    pub last_dp: u64,
    pub xmm: [[u8; 16usize]; 16usize],
    pub mxcsr: u32,
}

#[serde_with::serde_as]
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct LapicState {
    #[serde_as(as = "[_; 1024usize]")]
    pub(crate) regs: [::std::os::raw::c_char; 1024usize],
}

impl Default for LapicState {
    fn default() -> Self {
        // SAFETY: this is plain old data structure
        unsafe { ::std::mem::zeroed() }
    }
}

impl LapicState {
    pub fn get_klapic_reg(&self, reg_offset: usize) -> u32 {
        use byteorder::{LittleEndian, ReadBytesExt};
        use std::io::Cursor;
        use std::mem;

        let sliceu8 = unsafe {
            // This array is only accessed as parts of a u32 word, so interpret it as a u8 array.
            // Cursors are only readable on arrays of u8, not i8(c_char).
            mem::transmute::<&[i8], &[u8]>(&self.regs[reg_offset..])
        };

        let mut reader = Cursor::new(sliceu8);
        // Following call can't fail if the offsets defined above are correct.
        reader
            .read_u32::<LittleEndian>()
            .expect("Failed to read klapic register")
    }

    pub fn set_klapic_reg(&mut self, reg_offset: usize, value: u32) {
        use byteorder::{LittleEndian, WriteBytesExt};
        use std::io::Cursor;
        use std::mem;

        let sliceu8 = unsafe {
            // This array is only accessed as parts of a u32 word, so interpret it as a u8 array.
            // Cursors are only readable on arrays of u8, not i8(c_char).
            mem::transmute::<&mut [i8], &mut [u8]>(&mut self.regs[reg_offset..])
        };

        let mut writer = Cursor::new(sliceu8);
        // Following call can't fail if the offsets defined above are correct.
        writer
            .write_u32::<LittleEndian>(value)
            .expect("Failed to write klapic register")
    }
}

#[derive(Debug, Default, Copy, Clone, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
pub struct MsrEntry {
    pub index: u32,
    pub data: u64,
}

#[serde_with::serde_as]
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct XsaveState {
    #[serde_as(as = "[_; 1024usize]")]
    pub region: [u32; 1024usize],
}

impl Default for XsaveState {
    fn default() -> Self {
        // SAFETY: this is plain old data structure
        unsafe { ::std::mem::zeroed() }
    }
}
