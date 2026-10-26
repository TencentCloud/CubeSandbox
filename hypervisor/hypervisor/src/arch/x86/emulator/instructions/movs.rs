//
// Copyright © 2021 Microsoft
//
// SPDX-License-Identifier: Apache-2.0
//

#![allow(non_camel_case_types)]

//
// MOVS - Move Data from String to String
//

use crate::arch::emulator::{EmulationError, PlatformEmulator};
use crate::arch::x86::emulator::instructions::*;
use crate::arch::x86::regs::DF;
use crate::arch::x86::Exception;

pub struct Movsd_m32_m32;
impl<T: CpuStateManager> InstructionHandler<T> for Movsd_m32_m32 {
    fn emulate(
        &self,
        insn: &Instruction,
        state: &mut T,
        platform: &mut dyn PlatformEmulator<CpuState = T>,
    ) -> Result<(), EmulationError<Exception>> {
        let mut count: u64 = if insn.has_rep_prefix() {
            state
                .read_reg(Register::ECX)
                .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?
        } else {
            1
        };

        let mut rsi = state
            .read_reg(Register::RSI)
            .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?;
        let mut rdi = state
            .read_reg(Register::RDI)
            .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?;

        let df = (state.flags() & DF) != 0;
        let len = std::mem::size_of::<u32>();

        while count > 0 {
            let mut memory: [u8; 4] = [0; 4];

            let src = state
                .linearize(Register::DS, rsi, false)
                .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?;
            let dst = state
                .linearize(Register::ES, rdi, true)
                .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?;

            platform
                .read_memory(src, &mut memory[0..len])
                .map_err(EmulationError::PlatformEmulationError)?;
            platform
                .write_memory(dst, &memory[0..len])
                .map_err(EmulationError::PlatformEmulationError)?;

            if df {
                rsi = rsi.wrapping_sub(len as u64);
                rdi = rdi.wrapping_sub(len as u64);
            } else {
                rsi = rsi.wrapping_add(len as u64);
                rdi = rdi.wrapping_add(len as u64);
            }
            count -= 1;
        }

        state
            .write_reg(Register::RSI, rsi)
            .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?;
        state
            .write_reg(Register::RDI, rdi)
            .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?;
        if insn.has_rep_prefix() {
            state
                .write_reg(Register::ECX, 0)
                .map_err(|e| EmulationError::InvalidOperand(anyhow!(e)))?;
        }

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    #![allow(unused_mut)]
    use super::*;
    use crate::arch::x86::emulator::mock_vmm::*;

    #[test]
    fn test_rep_movsd_m32_m32() {
        let ip: u64 = 0x1000;
        let memory: [u8; 24] = [
            0x78, 0x56, 0x34, 0x12, // 0x12345678
            0xdd, 0xcc, 0xbb, 0xaa, // 0xaabbccdd
            0xa5, 0x5a, 0xa5, 0x5a, // 0x5aa55aa5
            0x00, 0x00, 0x00, 0x00, // 0x00000000
            0x00, 0x00, 0x00, 0x00, // 0x00000000
            0x00, 0x00, 0x00, 0x00, // 0x00000000
        ];
        let insn = [0xf3, 0xa5]; // rep movsd
        let regs = vec![(Register::ECX, 3), (Register::ESI, 0), (Register::EDI, 0xc)];
        let mut data = [0u8; 4];

        let mut vmm = MockVmm::new(ip, regs, Some((0, &memory)));

        assert!(vmm.emulate_first_insn(0, &insn).is_ok());

        vmm.read_memory(0xc, &mut data).unwrap();
        assert_eq!(0x12345678, <u32>::from_le_bytes(data));
        vmm.read_memory(0xc + 4, &mut data).unwrap();
        assert_eq!(0xaabbccdd, <u32>::from_le_bytes(data));
        vmm.read_memory(0xc + 8, &mut data).unwrap();
        assert_eq!(0x5aa55aa5, <u32>::from_le_bytes(data));
        // The rest should be default value 0 from MockVmm
        vmm.read_memory(0xc + 12, &mut data).unwrap();
        assert_eq!(0x0, <u32>::from_le_bytes(data));
    }

    #[test]
    fn test_movsd_m32_m32() {
        let ip: u64 = 0x1000;
        let memory: [u8; 4] = [
            0x78, 0x56, 0x34, 0x12, // 0x12345678
        ];
        let insn = [0xa5]; // movsd
        let regs = vec![(Register::ESI, 0), (Register::EDI, 0x8)];
        let mut data = [0u8; 4];

        let mut vmm = MockVmm::new(ip, regs, Some((0, &memory)));

        assert!(vmm.emulate_first_insn(0, &insn).is_ok());

        vmm.read_memory(0x8, &mut data).unwrap();
        assert_eq!(0x12345678, <u32>::from_le_bytes(data));
        // The rest should be default value 0 from MockVmm
        vmm.read_memory(0x4, &mut data).unwrap();
        assert_eq!(0x0, <u32>::from_le_bytes(data));
        vmm.read_memory(0x8 + 8, &mut data).unwrap();
        assert_eq!(0x0, <u32>::from_le_bytes(data));
    }
}
