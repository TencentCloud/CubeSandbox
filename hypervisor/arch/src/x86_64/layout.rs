// Copyright © 2020, Oracle and/or its affiliates.
//
// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

use vm_memory::GuestAddress;

/*

Memory layout documentation and constants
~~~~~~ ~~~~~~ ~~~~~~~~~~~~~ ~~~ ~~~~~~~~~

Constants are in order and grouped by range. Take care to update all references
when making changes and keep them in order.

*/

// ** Low RAM (start: 0, length: 640KiB) **
pub const LOW_RAM_START: GuestAddress = GuestAddress(0x0);

// == Fixed addresses within the "Low RAM" range: ==

// Location of EBDA address
pub const EBDA_POINTER: GuestAddress = GuestAddress(0x40e);

// Initial GDT/IDT needed to boot kernel
pub const BOOT_GDT_START: GuestAddress = GuestAddress(0x500);
pub const BOOT_IDT_START: GuestAddress = GuestAddress(0x520);

/// Address for the hvm_start_info struct used in PVH boot
pub const PVH_INFO_START: GuestAddress = GuestAddress(0x6000);

/// Starting address of array of modules of hvm_modlist_entry type.
/// Used to enable initrd support using the PVH boot ABI.
pub const MODLIST_START: GuestAddress = GuestAddress(0x6040);

/// Address of memory map table used in PVH boot. Can overlap
/// with the zero page address since they are mutually exclusive.
pub const MEMMAP_START: GuestAddress = GuestAddress(0x7000);

/// The 'zero page', a.k.a linux kernel bootparams.
pub const ZERO_PAGE_START: GuestAddress = GuestAddress(0x7000);

/// Initial stack for the boot CPU.
pub const BOOT_STACK_START: GuestAddress = GuestAddress(0x8000);
pub const BOOT_STACK_POINTER: GuestAddress = GuestAddress(0x8ff0);

// Initial pagetables.
pub const PML5_START: GuestAddress = GuestAddress(0x9000);
pub const PML4_START: GuestAddress = GuestAddress(0xa000);
pub const PDPTE_START: GuestAddress = GuestAddress(0xb000);
pub const PDE_START: GuestAddress = GuestAddress(0xc000);

/// Kernel command line start address.
pub const CMDLINE_START: GuestAddress = GuestAddress(0x20000);
/// Kernel command line start address maximum size.
pub const CMDLINE_MAX_SIZE: usize = 0x10000;

// MPTABLE, describing VCPUS.
pub const MPTABLE_START: GuestAddress = GuestAddress(0x9fc00);

// == End of "Low RAM" range. ==

// ** EBDA reserved area (start: 640KiB, length: 384KiB) **
pub const EBDA_START: GuestAddress = GuestAddress(0xa0000);

// == Fixed constants within the "EBDA" range ==

// ACPI RSDP table
pub const RSDP_POINTER: GuestAddress = EBDA_START;

pub const SMBIOS_START: u64 = 0xf0000; // First possible location per the spec.

// == End of "EBDA" range ==

// ** High RAM (start: 1MiB, length: 3071MiB) **
pub const HIGH_RAM_START: GuestAddress = GuestAddress(0x100000);

// == No fixed addresses in the "High RAM" range ==

// ** 32-bit reserved area (start: 3GiB, length: 896MiB) **
pub const MEM_32BIT_RESERVED_START: GuestAddress = GuestAddress(0xc000_0000);
pub const MEM_32BIT_RESERVED_SIZE: u64 = PCI_MMCONFIG_SIZE + MEM_32BIT_DEVICES_SIZE;

// == Fixed constants within the "32-bit reserved" range ==

// Sub range: 32-bit PCI devices (start: 3GiB, length: 640Mib)
pub const MEM_32BIT_DEVICES_START: GuestAddress = MEM_32BIT_RESERVED_START;
pub const MEM_32BIT_DEVICES_SIZE: u64 = 640 << 20;

// PCI MMCONFIG space (start: after the device space, length: 256MiB)
pub const PCI_MMCONFIG_START: GuestAddress =
    GuestAddress(MEM_32BIT_DEVICES_START.0 + MEM_32BIT_DEVICES_SIZE);
pub const PCI_MMCONFIG_SIZE: u64 = 256 << 20;
// One bus with potentially 256 devices (32 slots x 8 functions).
pub const PCI_MMIO_CONFIG_SIZE_PER_SEGMENT: u64 = 4096 * 256;

// TSS is 3 pages after the PCI MMCONFIG space
pub const KVM_TSS_START: GuestAddress = GuestAddress(PCI_MMCONFIG_START.0 + PCI_MMCONFIG_SIZE);
pub const KVM_TSS_SIZE: u64 = (3 * 4) << 10;

// Identity map is a one page region after the TSS
pub const KVM_IDENTITY_MAP_START: GuestAddress = GuestAddress(KVM_TSS_START.0 + KVM_TSS_SIZE);
pub const KVM_IDENTITY_MAP_SIZE: u64 = 4 << 10;

/// TPM Address Range
/// This Address range is specific to CRB Interface
pub const TPM_START: GuestAddress = GuestAddress(0xfed4_0000);
pub const TPM_SIZE: u64 = 0x1000;

// IOAPIC
pub const IOAPIC_START: GuestAddress = GuestAddress(0xfec0_0000);
pub const IOAPIC_SIZE: u64 = 0x20;

// APIC
pub const APIC_START: GuestAddress = GuestAddress(0xfee0_0000);

// == End of "32-bit reserved" range. ==

// ** 64-bit RAM start (start: 4GiB, length: varies) **
pub const RAM_64BIT_START: GuestAddress = GuestAddress(0x1_0000_0000);
