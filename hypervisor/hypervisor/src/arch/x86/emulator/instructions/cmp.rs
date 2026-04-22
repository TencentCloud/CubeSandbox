//
// Copyright © 2020 Microsoft
//
// SPDX-License-Identifier: Apache-2.0
//

#![allow(non_camel_case_types, clippy::upper_case_acronyms)]

//
// CMP-Compare Two Operands
//

use crate::arch::emulator::{EmulationError, PlatformEmulator};
use crate::arch::x86::emulator::instructions::*;
use crate::arch::x86::regs::*;
use crate::arch::x86::Exception;

// CMP affects OF, SF, ZF, AF, PF and CF
const FLAGS_MASK: u64 = CF | PF | AF | ZF | SF | OF;

// TODO: Switch to inline asm when that's stable. Executing CMP (or any arithmetic instructions)
// natively and extracting RFLAGS will be much faster and make the code simpler.
fn calc_rflags_cpazso(op0: u64, op1: u64, op_size: usize) -> u64 {
    let op_bits = op_size * 8;
    let msb_shift = op_bits - 1;
    // CMP is the same as SUB.
    let result = op0.wrapping_sub(op1);

    // Carry-out vector for SUB.
    let cout = (!op0 & op1) | ((!op0 ^ op1) & result);

    let cf = ((cout >> msb_shift) & 0x1) << CF_SHIFT;

    // PF only needs the least significant byte. XOR its higher 4 bits with its lower 4 bits then
    // use the value directly.
    let pf = ((0x9669 >> ((result ^ (result >> 4)) & 0xf)) & 0x1) << PF_SHIFT;

    // AF cares about the lowest 4 bits (nibble). msb_shift is 3 in this case.
    let af = ((cout >> 3) & 0x1) << AF_SHIFT;

    let zf = u64::from(result & (!0u64 >> (63 - msb_shift)) == 0) << ZF_SHIFT;

    let sf = ((result >> msb_shift) & 0x1) << SF_SHIFT;

    // Overflow happens when two operands have the same sign but the result has a different sign.
    let of = ((((op0 ^ op1) & (op0 ^ result)) >> msb_shift) & 0x1) << OF_SHIFT;

    cf | pf | af | zf | sf | of
}

macro_rules! cmp_rm_r {
    ($bound:ty) => {
        fn emulate(
            &self,
            insn: &Instruction,
            state: &mut T,
            platform: &mut dyn PlatformEmulator<CpuState = T>,
        ) -> Result<(), EmulationError<Exception>> {
            let op0_value = get_op(&insn, 0, std::mem::size_of::<$bound>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;
            let op1_value = get_op(&insn, 1, std::mem::size_of::<$bound>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;

            let cpazso = calc_rflags_cpazso(op0_value, op1_value, std::mem::size_of::<$bound>());

            state.set_flags((state.flags() & !FLAGS_MASK) | cpazso);

            Ok(())
        }
    };
}

macro_rules! cmp_r_rm {
    ($bound:ty) => {
        fn emulate(
            &self,
            insn: &Instruction,
            state: &mut T,
            platform: &mut dyn PlatformEmulator<CpuState = T>,
        ) -> Result<(), EmulationError<Exception>> {
            let op0_value = get_op(&insn, 0, std::mem::size_of::<$bound>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;
            let op1_value = get_op(&insn, 1, std::mem::size_of::<$bound>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;

            let cpazso = calc_rflags_cpazso(op0_value, op1_value, std::mem::size_of::<$bound>());

            state.set_flags((state.flags() & !FLAGS_MASK) | cpazso);

            Ok(())
        }
    };
}

macro_rules! cmp_rm_imm {
    ($imm:ty, $bound:ty) => {
        fn emulate(
            &self,
            insn: &Instruction,
            state: &mut T,
            platform: &mut dyn PlatformEmulator<CpuState = T>,
        ) -> Result<(), EmulationError<Exception>> {
            let op0_value = get_op(&insn, 0, std::mem::size_of::<$bound>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;
            let op1_value = get_op(&insn, 1, std::mem::size_of::<$imm>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;

            let cpazso = calc_rflags_cpazso(op0_value, op1_value, std::mem::size_of::<$bound>());

            state.set_flags((state.flags() & !FLAGS_MASK) | cpazso);

            Ok(())
        }
    };
}

pub struct Cmp_rm64_r64;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm64_r64 {
    cmp_rm_r!(u64);
}

pub struct Cmp_rm32_r32;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm32_r32 {
    cmp_rm_r!(u32);
}

pub struct Cmp_rm16_r16;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm16_r16 {
    cmp_rm_r!(u16);
}

pub struct Cmp_rm8_r8;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm8_r8 {
    cmp_rm_r!(u8);
}

pub struct Cmp_r64_rm64;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_r64_rm64 {
    cmp_r_rm!(u64);
}

pub struct Cmp_r32_rm32;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_r32_rm32 {
    cmp_r_rm!(u32);
}

pub struct Cmp_r16_rm16;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_r16_rm16 {
    cmp_r_rm!(u16);
}

pub struct Cmp_r8_rm8;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_r8_rm8 {
    cmp_r_rm!(u8);
}

pub struct Cmp_AL_imm8;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_AL_imm8 {
    cmp_rm_imm!(u8, u8);
}

pub struct Cmp_AX_imm16;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_AX_imm16 {
    cmp_rm_imm!(u16, u16);
}

pub struct Cmp_EAX_imm32;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_EAX_imm32 {
    cmp_rm_imm!(u32, u32);
}

pub struct Cmp_RAX_imm32;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_RAX_imm32 {
    cmp_rm_imm!(u32, u64);
}

pub struct Cmp_rm8_imm8;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm8_imm8 {
    cmp_rm_imm!(u8, u8);
}

pub struct Cmp_rm16_imm16;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm16_imm16 {
    cmp_rm_imm!(u16, u16);
}

pub struct Cmp_rm32_imm32;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm32_imm32 {
    cmp_rm_imm!(u32, u32);
}

pub struct Cmp_rm64_imm32;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm64_imm32 {
    cmp_rm_imm!(u32, u64);
}

pub struct Cmp_rm16_imm8;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm16_imm8 {
    cmp_rm_imm!(u8, u16);
}

pub struct Cmp_rm32_imm8;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm32_imm8 {
    cmp_rm_imm!(u8, u32);
}

pub struct Cmp_rm64_imm8;
impl<T: CpuStateManager> InstructionHandler<T> for Cmp_rm64_imm8 {
    cmp_rm_imm!(u8, u64);
}

#[cfg(test)]
mod tests {
    #![allow(unused_mut)]

    use super::*;
    use crate::arch::x86::emulator::mock_vmm::*;

    #[test]
    // cmp ah,al
    fn test_cmp_rm8_r8_1() {
        let rax: u64 = 0x0;
        let ip: u64 = 0x1000;
        let cpu_id = 0;
        let insn = [0x38, 0xc4]; // cmp ah,al
        let mut vmm = MockVmm::new(ip, vec![(Register::RAX, rax)], None);
        assert!(vmm.emulate_first_insn(cpu_id, &insn).is_ok());

        let rflags: u64 = vmm.cpu_state(cpu_id).unwrap().flags() & FLAGS_MASK;
        assert_eq!(0b1000100, rflags);
    }

    #[test]
    // cmp eax,100
    fn test_cmp_rm32_imm8_1() {
        let rax: u64 = 0xabcdef;
        let ip: u64 = 0x1000;
        let cpu_id = 0;
        let insn = [0x83, 0xf8, 0x64]; // cmp eax,100
        let mut vmm = MockVmm::new(ip, vec![(Register::RAX, rax)], None);
        assert!(vmm.emulate_first_insn(cpu_id, &insn).is_ok());

        let rflags: u64 = vmm.cpu_state(cpu_id).unwrap().flags() & FLAGS_MASK;
        assert_eq!(0b100, rflags);
    }

    #[test]
    // cmp eax,-1
    fn test_cmp_rm32_imm8_2() {
        let rax: u64 = 0xabcdef;
        let ip: u64 = 0x1000;
        let cpu_id = 0;
        let insn = [0x83, 0xf8, 0xff]; // cmp eax,-1
        let mut vmm = MockVmm::new(ip, vec![(Register::RAX, rax)], None);
        assert!(vmm.emulate_first_insn(cpu_id, &insn).is_ok());

        let rflags: u64 = vmm.cpu_state(cpu_id).unwrap().flags() & FLAGS_MASK;
        assert_eq!(0b101, rflags);
    }

    #[test]
    // cmp rax,rbx
    fn test_cmp_rm64_r64() {
        let rax: u64 = 0xabcdef;
        let rbx: u64 = 0x1234;
        let ip: u64 = 0x1000;
        let cpu_id = 0;
        let insn = [0x48, 0x39, 0xd8, 0x00, 0xc3]; // cmp rax,rbx + two bytes garbage
        let mut vmm = MockVmm::new(ip, vec![(Register::RAX, rax), (Register::RBX, rbx)], None);
        assert!(vmm.emulate_first_insn(cpu_id, &insn).is_ok());

        let rflags: u64 = vmm.cpu_state(cpu_id).unwrap().flags() & FLAGS_MASK;
        assert_eq!(0b100, rflags);
    }

    #[test]
    fn test_cmp_64() {
        let data = [
            (0xabcdef, 0x1234, 0b100),
            (0x0, 0x101, 0b1001_0101),
            (0x0, 0x8000_0000_0000_0000, 0b1000_1000_0101),
            (0x1234abcd, 0x1234abcd, 0b100_0100),
            (0x1234abcd, 0xdeadbeef, 0b1001_0101),
            (0xffff_ffff_ffff_ffff, 0xdeadbeef, 0b1000_0000),
            (0xffff_ffff_ffff_ffff, 0x0, 0b1000_0100),
        ];

        for d in data.iter() {
            let rax = d.0;
            let rbx = d.1;
            let insn = [0x48, 0x39, 0xd8]; // cmp rax,rbx
            let mut vmm = MockVmm::new(
                0x1000,
                vec![(Register::RAX, rax), (Register::RBX, rbx)],
                None,
            );
            assert!(vmm.emulate_first_insn(0, &insn).is_ok());

            let rflags: u64 = vmm.cpu_state(0).unwrap().flags() & FLAGS_MASK;
            assert_eq!(d.2, rflags);
        }
    }

    #[test]
    fn test_cmp_32() {
        let data = [
            (0xabcdef, 0x1234, 0b100),
            (0x0, 0x101, 0b1001_0101),
            (0x0, 0x8000_0000_0000_0000, 0b100_0100), // Same as cmp 0,0 due to truncation
            (0x1234abcd, 0x1234abcd, 0b100_0100),
            (0x1234abcd, 0xdeadbeef, 0b1_0101),
            (0xffff_ffff_ffff_ffff, 0xdeadbeef, 0b0), // Same as cmp 0xffffffff,0xdeadbeef
            (0xffff_ffff, 0x0, 0b1000_0100),
        ];

        for d in data.iter() {
            let rax = d.0;
            let rbx = d.1;
            let insn = [0x39, 0xd8]; // cmp eax,ebx
            let mut vmm = MockVmm::new(
                0x1000,
                vec![(Register::RAX, rax), (Register::RBX, rbx)],
                None,
            );
            assert!(vmm.emulate_first_insn(0, &insn).is_ok());

            let rflags: u64 = vmm.cpu_state(0).unwrap().flags() & FLAGS_MASK;
            assert_eq!(d.2, rflags);
        }
    }
}
