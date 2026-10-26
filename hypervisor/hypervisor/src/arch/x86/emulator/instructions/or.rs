//
// Copyright © 2021 Microsoft
//
// SPDX-License-Identifier: Apache-2.0
//

#![allow(non_camel_case_types)]

//
// OR - Logical inclusive OR
//

use crate::arch::emulator::{EmulationError, PlatformEmulator};
use crate::arch::x86::emulator::instructions::*;
use crate::arch::x86::Exception;

macro_rules! or_rm_r {
    ($bound:ty) => {
        fn emulate(
            &self,
            insn: &Instruction,
            state: &mut T,
            platform: &mut dyn PlatformEmulator<CpuState = T>,
        ) -> Result<(), EmulationError<Exception>> {
            let src_reg_value = get_op(&insn, 1, std::mem::size_of::<$bound>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;

            let dst_value = get_op(&insn, 0, std::mem::size_of::<$bound>(), state, platform)
                .map_err(EmulationError::PlatformEmulationError)?;

            let result = src_reg_value | dst_value;

            set_op(
                &insn,
                0,
                std::mem::size_of::<$bound>(),
                state,
                platform,
                result,
            )
            .map_err(EmulationError::PlatformEmulationError)?;

            Ok(())
        }
    };
}

pub struct Or_rm8_r8;
impl<T: CpuStateManager> InstructionHandler<T> for Or_rm8_r8 {
    or_rm_r!(u8);
}

#[cfg(test)]
mod tests {
    #![allow(unused_mut)]
    use super::*;

    use crate::arch::x86::emulator::mock_vmm::*;

    #[test]
    // or byte ptr [rax+1h], sil
    fn test_or_rm8_r8() {
        let rax = 0;
        let insn = [0x40, 0x08, 0x70, 0x1];
        let cpu_id = 0;
        let ip: u64 = 0x1000;
        let sil = 0xaa;
        let memory = [0x0, 0x55];

        let mut vmm = MockVmm::new(
            ip,
            vec![(Register::SIL, sil), (Register::RAX, rax)],
            Some((0, &memory)),
        );

        assert!(vmm.emulate_first_insn(cpu_id, &insn).is_ok());

        let mut out: [u8; 1] = [0; 1];

        vmm.read_memory(rax + 1, &mut out).unwrap();
        assert_eq!(u8::from_le_bytes(out), 0xff);
    }
}
