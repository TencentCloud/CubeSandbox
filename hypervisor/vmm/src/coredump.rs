// Copyright © 2022 ZTE Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

#[cfg(target_arch = "x86_64")]
use hypervisor::arch::x86::{DescriptorTable, SegmentRegister};
use linux_loader::elf;
use std::fs::File;
use std::io::Write;
use vm_memory::ByteValued;

#[derive(Clone)]
pub struct CoredumpMemoryRegion {
    pub mem_offset_in_elf: u64,
    pub mem_size: u64,
}

#[derive(Clone)]
pub struct CoredumpMemoryRegions {
    pub ram_maps: std::collections::BTreeMap<u64, CoredumpMemoryRegion>,
}

/// Platform information
#[derive(Default)]
pub struct DumpState {
    pub elf_note_size: isize,
    pub elf_phdr_num: u16,
    pub elf_sh_info: u32,
    pub mem_offset: u64,
    pub mem_info: Option<CoredumpMemoryRegions>,
    pub file: Option<File>,
}

#[derive(Debug)]
pub enum GuestDebuggableError {
    Coredump(anyhow::Error),
    CoredumpFile(std::io::Error),
}

pub trait GuestDebuggable: vm_migration::Pausable {
    fn coredump(
        &mut self,
        _destination_url: &str,
    ) -> std::result::Result<(), GuestDebuggableError> {
        Ok(())
    }
}

#[macro_export]
macro_rules! div_round_up {
    ($n:expr,$d:expr) => {
        ($n + $d - 1) / $d
    };
}

#[repr(C)]
#[derive(Default, Copy, Clone)]
pub struct X86_64UserRegs {
    /// r15, r14, r13, r12, rbp, rbx, r11, r10;
    pub regs1: [u64; 8],
    /// r9, r8, rax, rcx, rdx, rsi, rdi, orig_rax;
    pub regs2: [u64; 8],
    pub rip: u64,
    pub cs: u64,
    pub eflags: u64,
    pub rsp: u64,
    pub ss: u64,
    pub fs_base: u64,
    pub gs_base: u64,
    pub ds: u64,
    pub es: u64,
    pub fs: u64,
    pub gs: u64,
}

unsafe impl ByteValued for X86_64UserRegs {}

#[repr(C)]
pub struct X86_64ElfPrStatus {
    pub pad1: [u8; 32],
    pub pid: u32,
    pub pads2: [u8; 76],
    pub regs: X86_64UserRegs,
    pub pad3: [u8; 8],
}

#[repr(C)]
#[derive(Default, Copy, Clone)]
pub struct CpuSegment {
    pub selector: u32,
    pub limit: u32,
    pub flags: u32,
    pub pad: u32,
    pub base: u64,
}

const DESC_TYPE_SHIFT: u32 = 8;
const DESC_S_SHIFT: u32 = 12;
const DESC_DPL_SHIFT: u32 = 13;
const DESC_P_SHIFT: u32 = 15;
const DESC_P_MASK: u32 = 1 << DESC_P_SHIFT;
const DESC_AVL_SHIFT: u32 = 20;
const DESC_AVL_MASK: u32 = 1 << DESC_AVL_SHIFT;
const DESC_L_SHIFT: u32 = 21;
const DESC_B_SHIFT: u32 = 22;
const DESC_S_MASK: u32 = 1 << DESC_S_SHIFT;
const DESC_G_SHIFT: u32 = 23;
const DESC_G_MASK: u32 = 1 << DESC_G_SHIFT;

impl CpuSegment {
    pub fn new(reg: SegmentRegister) -> Self {
        let p_mask = if (reg.present > 0) && (reg.unusable == 0) {
            DESC_P_MASK
        } else {
            0
        };
        let flags = ((reg.type_ as u32) << DESC_TYPE_SHIFT)
            | p_mask
            | ((reg.dpl as u32) << DESC_DPL_SHIFT)
            | ((reg.db as u32) << DESC_B_SHIFT)
            | ((reg.s as u32) * DESC_S_MASK)
            | ((reg.l as u32) << DESC_L_SHIFT)
            | ((reg.g as u32) * DESC_G_MASK)
            | ((reg.avl as u32) * DESC_AVL_MASK);

        CpuSegment {
            selector: reg.selector as u32,
            limit: reg.limit,
            flags,
            pad: 0,
            base: reg.base,
        }
    }

    pub fn new_from_table(reg: DescriptorTable) -> Self {
        CpuSegment {
            selector: 0,
            limit: reg.limit as u32,
            flags: 0,
            pad: 0,
            base: reg.base,
        }
    }
}

#[repr(C)]
#[derive(Default, Copy, Clone)]
pub struct CpuState {
    pub version: u32,
    pub size: u32,
    /// rax, rbx, rcx, rdx, rsi, rdi, rsp, rbp
    pub regs1: [u64; 8],
    /// r8, r9, r10, r11, r12, r13, r14, r15
    pub regs2: [u64; 8],
    pub rip: u64,
    pub rflags: u64,
    pub cs: CpuSegment,
    pub ds: CpuSegment,
    pub es: CpuSegment,
    pub fs: CpuSegment,
    pub gs: CpuSegment,
    pub ss: CpuSegment,
    pub ldt: CpuSegment,
    pub tr: CpuSegment,
    pub gdt: CpuSegment,
    pub idt: CpuSegment,
    pub cr: [u64; 5],
    pub kernel_gs_base: u64,
}

unsafe impl ByteValued for CpuState {}

pub enum NoteDescType {
    Elf = 0,
    Vmm = 1,
    ElfAndVmm = 2,
}

// "CORE" or "QEMU"
pub const COREDUMP_NAME_SIZE: u32 = 5;
pub const NT_PRSTATUS: u32 = 1;

/// Core file.
const ET_CORE: u16 = 4;
/// 64-bit object.
const ELFCLASS64: u8 = 2;
/// Current ELF version.
const EV_CURRENT: u8 = 1;
/// AMD x86-64 architecture
const EM_X86_64: u16 = 62;

pub trait Elf64Writable {
    fn write_header(
        &mut self,
        dump_state: &DumpState,
    ) -> std::result::Result<(), GuestDebuggableError> {
        let e_ident = [
            elf::ELFMAG0 as u8, // magic
            elf::ELFMAG1,
            elf::ELFMAG2,
            elf::ELFMAG3,
            ELFCLASS64,             // class
            elf::ELFDATA2LSB as u8, //data
            EV_CURRENT,             // version
            0,                      // os_abi
            0,                      // abi_version
            0,                      // padding
            0,
            0,
            0,
            0,
            0,
            0,
        ];
        let elf64_ehdr_size = std::mem::size_of::<elf::Elf64_Ehdr>();
        let elf64_phdr_size = std::mem::size_of::<elf::Elf64_Phdr>();
        let mut elf64_ehdr = elf::Elf64_Ehdr {
            e_ident,
            e_type: ET_CORE,
            e_machine: EM_X86_64,
            e_version: EV_CURRENT as u32,
            e_entry: 0,
            e_phoff: elf64_ehdr_size as u64,
            e_shoff: 0,
            e_flags: 0,
            e_ehsize: 0,
            e_phentsize: elf64_phdr_size as u16,
            e_phnum: dump_state.elf_phdr_num,
            e_shentsize: 0,
            e_shnum: 0,
            e_shstrndx: 0,
        };
        elf64_ehdr.e_ehsize = std::mem::size_of_val(&elf64_ehdr) as u16;

        let mut coredump_file = dump_state.file.as_ref().unwrap();
        let bytes: &[u8] = elf64_ehdr.as_slice();
        coredump_file
            .write(bytes)
            .map_err(GuestDebuggableError::CoredumpFile)?;

        Ok(())
    }

    fn write_note(
        &mut self,
        dump_state: &DumpState,
    ) -> std::result::Result<(), GuestDebuggableError> {
        let begin = dump_state.mem_offset - dump_state.elf_note_size as u64;
        let elf64_phdr = elf::Elf64_Phdr {
            p_type: elf::PT_NOTE,
            p_flags: 0,
            p_offset: begin,
            p_vaddr: 0,
            p_paddr: 0,
            p_filesz: dump_state.elf_note_size as u64,
            p_memsz: dump_state.elf_note_size as u64,
            p_align: 0,
        };

        let mut coredump_file = dump_state.file.as_ref().unwrap();
        let bytes: &[u8] = elf64_phdr.as_slice();
        coredump_file
            .write(bytes)
            .map_err(GuestDebuggableError::CoredumpFile)?;

        Ok(())
    }

    fn write_load(
        &mut self,
        offset: u64,
        phys_addr: u64,
        length: u64,
        virt_addr: u64,
        dump_state: &DumpState,
    ) -> std::result::Result<(), GuestDebuggableError> {
        let elf64_load = elf::Elf64_Phdr {
            p_type: elf::PT_LOAD,
            p_flags: 0,
            p_offset: offset,
            p_vaddr: virt_addr,
            p_paddr: phys_addr,
            p_filesz: length,
            p_memsz: length,
            p_align: 0,
        };

        let mut coredump_file = dump_state.file.as_ref().unwrap();
        let bytes: &[u8] = elf64_load.as_slice();
        coredump_file
            .write(bytes)
            .map_err(GuestDebuggableError::CoredumpFile)?;

        Ok(())
    }

    fn write_loads(
        &mut self,
        dump_state: &DumpState,
    ) -> std::result::Result<(), GuestDebuggableError> {
        let mem_info = dump_state.mem_info.as_ref().unwrap();

        for (gpa, load) in &mem_info.ram_maps {
            self.write_load(load.mem_offset_in_elf, *gpa, load.mem_size, 0, dump_state)?;
        }

        Ok(())
    }

    fn elf_note_size(&self, hdr_size: u32, name_size: u32, desc_size: u32) -> u32 {
        (div_round_up!(hdr_size, 4) + div_round_up!(name_size, 4) + div_round_up!(desc_size, 4)) * 4
    }

    fn get_note_size(&self, desc_type: NoteDescType, nr_cpus: u32) -> u32 {
        let note_head_size = std::mem::size_of::<elf::Elf64_Nhdr>() as u32;
        let elf_desc_size = std::mem::size_of::<X86_64ElfPrStatus>() as u32;
        let cpu_state_desc_size = std::mem::size_of::<CpuState>() as u32;

        let elf_note_size = self.elf_note_size(note_head_size, COREDUMP_NAME_SIZE, elf_desc_size);
        let vmm_note_size =
            self.elf_note_size(note_head_size, COREDUMP_NAME_SIZE, cpu_state_desc_size);

        match desc_type {
            NoteDescType::Elf => elf_note_size * nr_cpus,
            NoteDescType::Vmm => vmm_note_size * nr_cpus,
            NoteDescType::ElfAndVmm => (elf_note_size + vmm_note_size) * nr_cpus,
        }
    }
}

pub trait CpuElf64Writable {
    fn cpu_write_elf64_note(
        &mut self,
        _dump_state: &DumpState,
    ) -> std::result::Result<(), GuestDebuggableError> {
        Ok(())
    }

    fn cpu_write_vmm_note(
        &mut self,
        _dump_state: &DumpState,
    ) -> std::result::Result<(), GuestDebuggableError> {
        Ok(())
    }
}
