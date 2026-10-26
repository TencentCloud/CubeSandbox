//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::arch::emulator::{EmulationError, EmulationResult, PlatformEmulator, PlatformError};
use crate::arch::x86::emulator::instructions::*;
use crate::arch::x86::regs::{CR0_PE, EFER_LMA};
use crate::arch::x86::{
    segment_type_expand_down, segment_type_ro, Exception, SegmentRegister, SpecialRegisters,
    StandardRegisters,
};
use anyhow::Context;
use iced_x86::*;

#[macro_use]
mod instructions;

/// x86 CPU modes
#[derive(Debug, PartialEq, Eq)]
pub enum CpuMode {
    /// Real mode
    Real,

    /// Virtual 8086 mode
    Virtual8086,

    /// 16-bit protected mode
    Protected16,

    /// 32-bit protected mode
    Protected,

    /// 64-bit mode, a.k.a. long mode
    Long,
}

/// CpuStateManager manages an x86 CPU state.
///
/// Instruction emulation handlers get a mutable reference to
/// a `CpuStateManager` implementation, representing the current state of the
/// CPU they have to emulate an instruction stream against. Usually those
/// handlers will modify the CPU state by modifying `CpuState` and it is up to
/// the handler caller to commit those changes back by invoking a
/// `PlatformEmulator` implementation `set_state()` method.
///
pub trait CpuStateManager: Clone {
    /// Reads a CPU register.
    ///
    /// # Arguments
    ///
    /// * `reg` - A general purpose, control or debug register.
    fn read_reg(&self, reg: Register) -> Result<u64, PlatformError>;

    /// Write to a CPU register.
    ///
    /// # Arguments
    ///
    /// * `reg` - A general purpose, control or debug register.
    /// * `val` - The value to load.
    fn write_reg(&mut self, reg: Register, val: u64) -> Result<(), PlatformError>;

    /// Reads a segment register.
    ///
    /// # Arguments
    ///
    /// * `reg` - A segment register.
    fn read_segment(&self, reg: Register) -> Result<SegmentRegister, PlatformError>;

    /// Write to a segment register.
    ///
    /// # Arguments
    ///
    /// * `reg` - A segment register.
    /// * `segment_reg` - The segment register value to load.
    fn write_segment(
        &mut self,
        reg: Register,
        segment_reg: SegmentRegister,
    ) -> Result<(), PlatformError>;

    /// Get the CPU instruction pointer.
    fn ip(&self) -> u64;

    /// Set the CPU instruction pointer.
    ///
    /// # Arguments
    ///
    /// * `ip` - The CPU instruction pointer.
    fn set_ip(&mut self, ip: u64);

    /// Get the CPU Extended Feature Enable Register.
    fn efer(&self) -> u64;

    /// Set the CPU Extended Feature Enable Register.
    ///
    /// # Arguments
    ///
    /// * `efer` - The CPU EFER value.
    fn set_efer(&mut self, efer: u64);

    /// Get the CPU flags.
    fn flags(&self) -> u64;

    /// Set the CPU flags.
    ///
    /// # Arguments
    ///
    /// * `flags` - The CPU flags
    fn set_flags(&mut self, flags: u64);

    /// Get the CPU mode.
    fn mode(&self) -> Result<CpuMode, PlatformError>;

    /// Translate a logical (segmented) address into a linear (virtual) one.
    ///
    /// # Arguments
    ///
    /// * `segment` - Which segment to use for linearization
    /// * `logical_addr` - The logical address to be translated
    fn linearize(
        &self,
        segment: Register,
        logical_addr: u64,
        write: bool,
    ) -> Result<u64, PlatformError> {
        let segment_register = self.read_segment(segment)?;
        let mode = self.mode()?;

        match mode {
            CpuMode::Long => {
                // TODO Check that we got a canonical address.
                Ok(logical_addr
                    .checked_add(segment_register.base)
                    .ok_or_else(|| {
                        PlatformError::InvalidAddress(anyhow!(
                            "Logical address {:#x} can not be linearized with segment {:#x?}",
                            logical_addr,
                            segment_register
                        ))
                    })?)
            }

            CpuMode::Protected | CpuMode::Real => {
                let segment_type = segment_register.segment_type();

                // Must not write to a read-only segment.
                if segment_type_ro(segment_type) && write {
                    return Err(PlatformError::InvalidAddress(anyhow!(
                        "Can not write to a read-only segment"
                    )));
                }

                let logical_addr = logical_addr & 0xffff_ffffu64;
                let mut segment_limit: u32 = if segment_register.granularity() != 0 {
                    (segment_register.limit << 12) | 0xfff
                } else {
                    segment_register.limit
                };

                // Expand-down segment
                if segment_type_expand_down(segment_type) {
                    if logical_addr >= segment_limit.into() {
                        return Err(PlatformError::InvalidAddress(anyhow!(
                            "{:#x} is off limits {:#x} (expand down)",
                            logical_addr,
                            segment_limit
                        )));
                    }

                    if segment_register.db() != 0 {
                        segment_limit = 0xffffffff
                    } else {
                        segment_limit = 0xffff
                    }
                }

                if logical_addr > segment_limit.into() {
                    return Err(PlatformError::InvalidAddress(anyhow!(
                        "{:#x} is off limits {:#x}",
                        logical_addr,
                        segment_limit
                    )));
                }

                Ok(logical_addr + segment_register.base)
            }

            _ => Err(PlatformError::UnsupportedCpuMode(anyhow!("{:?}", mode))),
        }
    }
}

const REGISTER_MASK_64: u64 = 0xffff_ffff_ffff_ffffu64;
const REGISTER_MASK_32: u64 = 0xffff_ffffu64;
const REGISTER_MASK_16: u64 = 0xffffu64;
const REGISTER_MASK_8: u64 = 0xffu64;

macro_rules! set_reg {
    ($reg:expr, $mask:expr, $value:expr) => {
        $reg = ($reg & $mask) | $value
    };
}

#[derive(Clone, Default, Debug)]
/// A minimal, emulated CPU state.
///
/// Hypervisors needing x86 emulation can choose to either use their own
/// CPU state structures and implement the CpuStateManager interface for it,
/// or use `EmulatorCpuState`. The latter implies creating a new state
/// `EmulatorCpuState` instance for each platform `cpu_state()` call, which
/// might be less efficient.
pub struct EmulatorCpuState {
    pub regs: StandardRegisters,
    pub sregs: SpecialRegisters,
}

impl CpuStateManager for EmulatorCpuState {
    fn read_reg(&self, reg: Register) -> Result<u64, PlatformError> {
        let mut reg_value: u64 = match reg {
            Register::RAX | Register::EAX | Register::AX | Register::AL | Register::AH => {
                self.regs.rax
            }
            Register::RBX | Register::EBX | Register::BX | Register::BL | Register::BH => {
                self.regs.rbx
            }
            Register::RCX | Register::ECX | Register::CX | Register::CL | Register::CH => {
                self.regs.rcx
            }
            Register::RDX | Register::EDX | Register::DX | Register::DL | Register::DH => {
                self.regs.rdx
            }
            Register::RSP | Register::ESP | Register::SP => self.regs.rsp,
            Register::RBP | Register::EBP | Register::BP => self.regs.rbp,
            Register::RSI | Register::ESI | Register::SI | Register::SIL => self.regs.rsi,
            Register::RDI | Register::EDI | Register::DI | Register::DIL => self.regs.rdi,
            Register::R8 | Register::R8D | Register::R8W | Register::R8L => self.regs.r8,
            Register::R9 | Register::R9D | Register::R9W | Register::R9L => self.regs.r9,
            Register::R10 | Register::R10D | Register::R10W | Register::R10L => self.regs.r10,
            Register::R11 | Register::R11D | Register::R11W | Register::R11L => self.regs.r11,
            Register::R12 | Register::R12D | Register::R12W | Register::R12L => self.regs.r12,
            Register::R13 | Register::R13D | Register::R13W | Register::R13L => self.regs.r13,
            Register::R14 | Register::R14D | Register::R14W | Register::R14L => self.regs.r14,
            Register::R15 | Register::R15D | Register::R15W | Register::R15L => self.regs.r15,
            Register::CR0 => self.sregs.cr0,
            Register::CR2 => self.sregs.cr2,
            Register::CR3 => self.sregs.cr3,
            Register::CR4 => self.sregs.cr4,
            Register::CR8 => self.sregs.cr8,

            r => {
                return Err(PlatformError::InvalidRegister(anyhow!(
                    "read_reg invalid GPR {:?}",
                    r
                )))
            }
        };

        reg_value = if reg.is_gpr64() || reg.is_cr() {
            reg_value
        } else if reg.is_gpr32() {
            reg_value & REGISTER_MASK_32
        } else if reg.is_gpr16() {
            reg_value & REGISTER_MASK_16
        } else if reg.is_gpr8() {
            if reg == Register::AH
                || reg == Register::BH
                || reg == Register::CH
                || reg == Register::DH
            {
                (reg_value >> 8) & REGISTER_MASK_8
            } else {
                reg_value & REGISTER_MASK_8
            }
        } else {
            return Err(PlatformError::InvalidRegister(anyhow!(
                "read_reg invalid GPR {:?}",
                reg
            )));
        };

        debug!("Register read: {:#x} from {:?}", reg_value, reg);

        Ok(reg_value)
    }

    fn write_reg(&mut self, reg: Register, val: u64) -> Result<(), PlatformError> {
        debug!("Register write: {:#x} to {:?}", val, reg);

        // SDM Vol 1 - 3.4.1.1
        //
        // 8-bit and 16-bit operands generate an 8-bit or 16-bit result.
        // The upper 56 bits or 48 bits (respectively) of the destination
        // general-purpose register are not modified by the operation.
        let (reg_value, mask): (u64, u64) = if reg.is_gpr64() || reg.is_cr() {
            (val, !REGISTER_MASK_64)
        } else if reg.is_gpr32() {
            (val & REGISTER_MASK_32, !REGISTER_MASK_64)
        } else if reg.is_gpr16() {
            (val & REGISTER_MASK_16, !REGISTER_MASK_16)
        } else if reg.is_gpr8() {
            if reg == Register::AH
                || reg == Register::BH
                || reg == Register::CH
                || reg == Register::DH
            {
                ((val & REGISTER_MASK_8) << 8, !(REGISTER_MASK_8 << 8))
            } else {
                (val & REGISTER_MASK_8, !REGISTER_MASK_8)
            }
        } else {
            return Err(PlatformError::InvalidRegister(anyhow!(
                "write_reg invalid register {:?}",
                reg
            )));
        };

        match reg {
            Register::RAX | Register::EAX | Register::AX | Register::AL | Register::AH => {
                set_reg!(self.regs.rax, mask, reg_value);
            }
            Register::RBX | Register::EBX | Register::BX | Register::BL | Register::BH => {
                set_reg!(self.regs.rbx, mask, reg_value);
            }
            Register::RCX | Register::ECX | Register::CX | Register::CL | Register::CH => {
                set_reg!(self.regs.rcx, mask, reg_value);
            }
            Register::RDX | Register::EDX | Register::DX | Register::DL | Register::DH => {
                set_reg!(self.regs.rdx, mask, reg_value);
            }
            Register::RSP | Register::ESP | Register::SP => {
                set_reg!(self.regs.rsp, mask, reg_value)
            }
            Register::RBP | Register::EBP | Register::BP => {
                set_reg!(self.regs.rbp, mask, reg_value)
            }
            Register::RSI | Register::ESI | Register::SI | Register::SIL => {
                set_reg!(self.regs.rsi, mask, reg_value)
            }
            Register::RDI | Register::EDI | Register::DI | Register::DIL => {
                set_reg!(self.regs.rdi, mask, reg_value)
            }
            Register::R8 | Register::R8D | Register::R8W | Register::R8L => {
                set_reg!(self.regs.r8, mask, reg_value)
            }
            Register::R9 | Register::R9D | Register::R9W | Register::R9L => {
                set_reg!(self.regs.r9, mask, reg_value)
            }
            Register::R10 | Register::R10D | Register::R10W | Register::R10L => {
                set_reg!(self.regs.r10, mask, reg_value)
            }
            Register::R11 | Register::R11D | Register::R11W | Register::R11L => {
                set_reg!(self.regs.r11, mask, reg_value)
            }
            Register::R12 | Register::R12D | Register::R12W | Register::R12L => {
                set_reg!(self.regs.r12, mask, reg_value)
            }
            Register::R13 | Register::R13D | Register::R13W | Register::R13L => {
                set_reg!(self.regs.r13, mask, reg_value)
            }
            Register::R14 | Register::R14D | Register::R14W | Register::R14L => {
                set_reg!(self.regs.r14, mask, reg_value)
            }
            Register::R15 | Register::R15D | Register::R15W | Register::R15L => {
                set_reg!(self.regs.r15, mask, reg_value)
            }
            Register::CR0 => set_reg!(self.sregs.cr0, mask, reg_value),
            Register::CR2 => set_reg!(self.sregs.cr2, mask, reg_value),
            Register::CR3 => set_reg!(self.sregs.cr3, mask, reg_value),
            Register::CR4 => set_reg!(self.sregs.cr4, mask, reg_value),
            Register::CR8 => set_reg!(self.sregs.cr8, mask, reg_value),
            _ => {
                return Err(PlatformError::InvalidRegister(anyhow!(
                    "write_reg invalid register {:?}",
                    reg
                )))
            }
        }

        Ok(())
    }

    fn read_segment(&self, reg: Register) -> Result<SegmentRegister, PlatformError> {
        if !reg.is_segment_register() {
            return Err(PlatformError::InvalidRegister(anyhow!(
                "read_segment {:?} is not a segment register",
                reg
            )));
        }

        match reg {
            Register::CS => Ok(self.sregs.cs),
            Register::DS => Ok(self.sregs.ds),
            Register::ES => Ok(self.sregs.es),
            Register::FS => Ok(self.sregs.fs),
            Register::GS => Ok(self.sregs.gs),
            Register::SS => Ok(self.sregs.ss),
            r => Err(PlatformError::InvalidRegister(anyhow!(
                "read_segment invalid register {:?}",
                r
            ))),
        }
    }

    fn write_segment(
        &mut self,
        reg: Register,
        segment_register: SegmentRegister,
    ) -> Result<(), PlatformError> {
        if !reg.is_segment_register() {
            return Err(PlatformError::InvalidRegister(anyhow!("{:?}", reg)));
        }

        match reg {
            Register::CS => self.sregs.cs = segment_register,
            Register::DS => self.sregs.ds = segment_register,
            Register::ES => self.sregs.es = segment_register,
            Register::FS => self.sregs.fs = segment_register,
            Register::GS => self.sregs.gs = segment_register,
            Register::SS => self.sregs.ss = segment_register,
            r => return Err(PlatformError::InvalidRegister(anyhow!("{:?}", r))),
        }

        Ok(())
    }

    fn ip(&self) -> u64 {
        self.regs.rip
    }

    fn set_ip(&mut self, ip: u64) {
        self.regs.rip = ip;
    }

    fn efer(&self) -> u64 {
        self.sregs.efer
    }

    fn set_efer(&mut self, efer: u64) {
        self.sregs.efer = efer
    }

    fn flags(&self) -> u64 {
        self.regs.rflags
    }

    fn set_flags(&mut self, flags: u64) {
        self.regs.rflags = flags;
    }

    fn mode(&self) -> Result<CpuMode, PlatformError> {
        let efer = self.efer();
        let cr0 = self.read_reg(Register::CR0)?;
        let mut mode = CpuMode::Real;

        if (cr0 & CR0_PE) == CR0_PE {
            mode = CpuMode::Protected;
        }

        if (efer & EFER_LMA) == EFER_LMA {
            if mode != CpuMode::Protected {
                return Err(PlatformError::InvalidState(anyhow!(
                    "Protection must be enabled in long mode"
                )));
            }

            mode = CpuMode::Long;
        }

        Ok(mode)
    }
}

pub struct Emulator<'a, T: CpuStateManager> {
    platform: &'a mut dyn PlatformEmulator<CpuState = T>,
}

// Reduce repetition, see its invocation in get_handler().
macro_rules! gen_handler_match {
    ($value: ident, $( ($module:ident, $code:ident) ),* ) => {
        match $value {
            $(
                Code::$code => Some(Box::new($module::$code)),
            )*
            _ => None,
        }
    };
}

impl<'a, T: CpuStateManager> Emulator<'a, T> {
    pub fn new(platform: &mut dyn PlatformEmulator<CpuState = T>) -> Emulator<T> {
        Emulator { platform }
    }

    fn get_handler(code: Code) -> Option<Box<dyn InstructionHandler<T>>> {
        let handler: Option<Box<dyn InstructionHandler<T>>> = gen_handler_match!(
            code,
            // CMP
            (cmp, Cmp_rm32_r32),
            (cmp, Cmp_rm8_r8),
            (cmp, Cmp_rm32_imm8),
            (cmp, Cmp_rm64_r64),
            // MOV
            (mov, Mov_r8_rm8),
            (mov, Mov_r8_imm8),
            (mov, Mov_r16_imm16),
            (mov, Mov_r16_rm16),
            (mov, Mov_r32_imm32),
            (mov, Mov_r32_rm32),
            (mov, Mov_r64_imm64),
            (mov, Mov_r64_rm64),
            (mov, Mov_rm8_imm8),
            (mov, Mov_rm8_r8),
            (mov, Mov_rm16_imm16),
            (mov, Mov_rm16_r16),
            (mov, Mov_rm32_imm32),
            (mov, Mov_rm32_r32),
            (mov, Mov_rm64_imm32),
            (mov, Mov_rm64_r64),
            // MOVZX
            (mov, Movzx_r16_rm8),
            (mov, Movzx_r32_rm8),
            (mov, Movzx_r64_rm8),
            (mov, Movzx_r32_rm16),
            (mov, Movzx_r64_rm16),
            // MOVS
            (movs, Movsd_m32_m32),
            // OR
            (or, Or_rm8_r8)
        );

        handler
    }

    fn emulate_insn_stream(
        &mut self,
        cpu_id: usize,
        insn_stream: &[u8],
        num_insn: Option<usize>,
    ) -> EmulationResult<T, Exception> {
        let mut state = self
            .platform
            .cpu_state(cpu_id)
            .map_err(EmulationError::PlatformEmulationError)?;
        let mut decoder = Decoder::new(64, insn_stream, DecoderOptions::NONE);
        let mut insn = Instruction::default();
        let mut num_insn_emulated: usize = 0;
        let mut fetched_insn_stream: [u8; 16] = [0; 16];
        let mut last_decoded_ip: u64 = state.ip();
        let mut stop_emulation: bool = false;

        decoder.set_ip(state.ip());

        while decoder.can_decode() && !stop_emulation {
            decoder.decode_out(&mut insn);

            if decoder.last_error() == DecoderError::NoMoreBytes {
                // The decoder is missing some bytes to decode the current
                // instruction, for example because the instruction stream
                // crosses a page boundary.
                // We fetch 16 more bytes from the instruction segment,
                // decode and emulate the failing instruction and terminate
                // the emulation loop.
                debug!(
                    "Fetching {} bytes from {:#x}",
                    fetched_insn_stream.len(),
                    last_decoded_ip
                );

                // fetched_insn_stream is 16 bytes long, enough to contain
                // any complete x86 instruction.
                self.platform
                    .fetch(last_decoded_ip, &mut fetched_insn_stream)
                    .map_err(EmulationError::PlatformEmulationError)?;

                debug!("Fetched {:x?}", fetched_insn_stream);

                // Once we have the new stream, we must create a new decoder
                // and emulate one last instruction from the last decoded IP.
                decoder = Decoder::new(64, &fetched_insn_stream, DecoderOptions::NONE);
                decoder.decode_out(&mut insn);
                if decoder.last_error() != DecoderError::None {
                    return Err(EmulationError::InstructionFetchingError(anyhow!(
                        "{:#x?}",
                        insn_format!(insn)
                    )));
                }

                stop_emulation = true;
            }

            // Emulate the decoded instruction
            Emulator::get_handler(insn.code())
                .ok_or_else(|| {
                    EmulationError::UnsupportedInstruction(anyhow!(
                        "{:#x?} {:?} {:?}",
                        insn_format!(insn),
                        insn.mnemonic(),
                        insn.code()
                    ))
                })?
                .emulate(&insn, &mut state, self.platform)
                .context(anyhow!("Failed to emulate {:#x?}", insn_format!(insn)))?;

            last_decoded_ip = decoder.ip();
            num_insn_emulated += 1;

            if let Some(num_insn) = num_insn {
                if num_insn_emulated >= num_insn {
                    // Exit the decoding loop, do not decode the next instruction.
                    stop_emulation = true;
                }
            }
        }

        state.set_ip(decoder.ip());
        Ok(state)
    }

    /// Emulate all instructions from the instructions stream.
    pub fn emulate(&mut self, cpu_id: usize, insn_stream: &[u8]) -> EmulationResult<T, Exception> {
        self.emulate_insn_stream(cpu_id, insn_stream, None)
    }

    /// Only emulate the first instruction from the stream.
    ///
    /// This is useful for cases where we get readahead instruction stream
    /// but implicitly must only emulate the first instruction, and then return
    /// to the guest.
    pub fn emulate_first_insn(
        &mut self,
        cpu_id: usize,
        insn_stream: &[u8],
    ) -> EmulationResult<T, Exception> {
        self.emulate_insn_stream(cpu_id, insn_stream, Some(1))
    }
}

#[cfg(test)]
mod mock_vmm {
    #![allow(unused_mut)]

    use super::*;
    use crate::arch::emulator::{EmulationError, PlatformEmulator};
    use crate::arch::x86::emulator::{Emulator, EmulatorCpuState as CpuState};
    use crate::arch::x86::gdt::{gdt_entry, segment_from_gdt};
    use crate::arch::x86::Exception;
    use std::sync::{Arc, Mutex};

    #[derive(Debug, Clone)]
    pub struct MockVmm {
        memory: Vec<u8>,
        state: Arc<Mutex<CpuState>>,
    }

    pub type MockResult = Result<(), EmulationError<Exception>>;

    impl MockVmm {
        pub fn new(ip: u64, regs: Vec<(Register, u64)>, memory: Option<(u64, &[u8])>) -> MockVmm {
            let _ = env_logger::try_init();
            let cs_reg = segment_from_gdt(gdt_entry(0xc09b, 0, 0xffffffff), 1);
            let ds_reg = segment_from_gdt(gdt_entry(0xc093, 0, 0xffffffff), 2);
            let es_reg = segment_from_gdt(gdt_entry(0xc093, 0, 0xffffffff), 3);
            let mut initial_state = CpuState::default();
            initial_state.set_ip(ip);
            initial_state.write_segment(Register::CS, cs_reg).unwrap();
            initial_state.write_segment(Register::DS, ds_reg).unwrap();
            initial_state.write_segment(Register::ES, es_reg).unwrap();
            for (reg, value) in regs {
                initial_state.write_reg(reg, value).unwrap();
            }

            let mut vmm = MockVmm {
                memory: vec![0; 8192],
                state: Arc::new(Mutex::new(initial_state)),
            };

            if let Some(mem) = memory {
                vmm.write_memory(mem.0, mem.1).unwrap();
            }

            vmm
        }

        pub fn emulate_insn(
            &mut self,
            cpu_id: usize,
            insn: &[u8],
            num_insn: Option<usize>,
        ) -> MockResult {
            let ip = self.cpu_state(cpu_id).unwrap().ip();
            let mut emulator = Emulator::new(self);

            let new_state = emulator.emulate_insn_stream(cpu_id, insn, num_insn)?;
            if num_insn.is_none() {
                assert_eq!(ip + insn.len() as u64, new_state.ip());
            }

            self.set_cpu_state(cpu_id, new_state).unwrap();

            Ok(())
        }

        pub fn emulate_first_insn(&mut self, cpu_id: usize, insn: &[u8]) -> MockResult {
            self.emulate_insn(cpu_id, insn, Some(1))
        }
    }

    impl PlatformEmulator for MockVmm {
        type CpuState = CpuState;

        fn read_memory(&self, gva: u64, data: &mut [u8]) -> Result<(), PlatformError> {
            debug!(
                "Memory read {} bytes from [{:#x} -> {:#x}]",
                data.len(),
                gva,
                gva + data.len() as u64 - 1
            );
            data.copy_from_slice(&self.memory[gva as usize..gva as usize + data.len()]);
            Ok(())
        }

        fn write_memory(&mut self, gva: u64, data: &[u8]) -> Result<(), PlatformError> {
            debug!(
                "Memory write {} bytes at [{:#x} -> {:#x}]",
                data.len(),
                gva,
                gva + data.len() as u64 - 1
            );
            self.memory[gva as usize..gva as usize + data.len()].copy_from_slice(data);

            Ok(())
        }

        fn cpu_state(&self, _cpu_id: usize) -> Result<CpuState, PlatformError> {
            Ok(self.state.lock().unwrap().clone())
        }

        fn set_cpu_state(
            &self,
            _cpu_id: usize,
            state: Self::CpuState,
        ) -> Result<(), PlatformError> {
            *self.state.lock().unwrap() = state;
            Ok(())
        }

        fn gva_to_gpa(&self, gva: u64) -> Result<u64, PlatformError> {
            Ok(gva)
        }

        fn fetch(&self, ip: u64, instruction_bytes: &mut [u8]) -> Result<(), PlatformError> {
            let rip = self
                .state
                .lock()
                .unwrap()
                .linearize(Register::CS, ip, false)?;
            self.read_memory(rip, instruction_bytes)
        }
    }
}

#[cfg(test)]
mod tests {
    #![allow(unused_mut)]
    use super::*;
    use crate::arch::x86::emulator::mock_vmm::*;

    #[test]
    // Emulate truncated instruction stream, which should cause a fetch.
    //
    // mov rax, 0x1000
    // Test with a first instruction truncated.
    fn test_fetch_first_instruction() {
        let ip: u64 = 0x1000;
        let cpu_id = 0;
        let memory = [
            // Code at IP
            0x48, 0xc7, 0xc0, 0x00, 0x10, 0x00, 0x00, // mov rax, 0x1000
            0x48, 0x8b, 0x58, 0x10, // mov rbx, qword ptr [rax+10h]
            // Padding
            0x00, 0x00, 0x00, 0x00, 0x00, // Padding is all zeroes
            // Data at IP + 0x10 (0x1234567812345678 in LE)
            0x78, 0x56, 0x34, 0x12, 0x78, 0x56, 0x34, 0x12,
        ];
        let insn = [
            // First instruction is truncated
            0x48, 0xc7, 0xc0, 0x00, // mov rax, 0x1000 -- Missing bytes: 0x00, 0x10, 0x00, 0x00,
        ];

        let mut vmm = MockVmm::new(ip, vec![], Some((ip, &memory)));
        assert!(vmm.emulate_insn(cpu_id, &insn, Some(2)).is_ok());

        let rax: u64 = vmm
            .cpu_state(cpu_id)
            .unwrap()
            .read_reg(Register::RAX)
            .unwrap();
        assert_eq!(rax, ip);
    }

    #[test]
    // Emulate truncated instruction stream, which should cause a fetch.
    //
    // mov rax, 0x1000
    // mov rbx, qword ptr [rax+10h]
    // Test with a 2nd instruction truncated.
    fn test_fetch_second_instruction() {
        let target_rax: u64 = 0x1234567812345678;
        let ip: u64 = 0x1000;
        let cpu_id = 0;
        let memory = [
            // Code at IP
            0x48, 0xc7, 0xc0, 0x00, 0x10, 0x00, 0x00, // mov rax, 0x1000
            0x48, 0x8b, 0x58, 0x10, // mov rbx, qword ptr [rax+10h]
            // Padding
            0x00, 0x00, 0x00, 0x00, 0x00, // Padding is all zeroes
            // Data at IP + 0x10 (0x1234567812345678 in LE)
            0x78, 0x56, 0x34, 0x12, 0x78, 0x56, 0x34, 0x12,
        ];
        let insn = [
            0x48, 0xc7, 0xc0, 0x00, 0x10, 0x00, 0x00, // mov rax, 0x1000
            0x48, 0x8b, // Truncated mov rbx, qword ptr [rax+10h] -- missing [0x58, 0x10]
        ];

        let mut vmm = MockVmm::new(ip, vec![], Some((ip, &memory)));
        assert!(vmm.emulate_insn(cpu_id, &insn, Some(2)).is_ok());

        let rbx: u64 = vmm
            .cpu_state(cpu_id)
            .unwrap()
            .read_reg(Register::RBX)
            .unwrap();
        assert_eq!(rbx, target_rax);
    }

    #[test]
    // Emulate truncated instruction stream, which should cause a fetch.
    //
    // mov rax, 0x1000
    // Test with a first instruction truncated and a bad fetched instruction.
    // Verify that the instruction emulation returns an error.
    fn test_fetch_bad_insn() {
        let ip: u64 = 0x1000;
        let cpu_id = 0;
        let memory = [
            // Code at IP
            0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
            0xff, 0xff,
        ];
        let insn = [
            // First instruction is truncated
            0x48, 0xc7, 0xc0, 0x00, // mov rax, 0x1000 -- Missing bytes: 0x00, 0x10, 0x00, 0x00,
        ];

        let mut vmm = MockVmm::new(ip, vec![], Some((ip, &memory)));
        assert!(vmm.emulate_first_insn(cpu_id, &insn).is_err());
    }
}
