// Copyright © 2023 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::x86_64::CpuidReg;
use hypervisor::arch::x86::{CpuIdCustomEntry, CpuIdEntry};

/// Intel brand string.
pub const VENDOR_ID_INTEL: &[u8; 12] = b"GenuineIntel";

/// AMD brand string.
pub const VENDOR_ID_AMD: &[u8; 12] = b"AuthenticAMD";

pub fn apply_compatible_template(cpuids: &mut [CpuIdEntry]) {
    if let Some(vendor) = get_vendor_id_from_host() {
        if *VENDOR_ID_INTEL == vendor {
            debug!("Got Intel machine");
            apply_template(cpuids, &t2cl());
        } else if *VENDOR_ID_AMD == vendor {
            debug!("Got AMD machine");
            apply_template(cpuids, &t2a());
        } else {
            warn!("Unsupported vendor");
        }
    }
}

/// Extracts the CPU vendor id from leaf 0x0.
///
/// # Errors
///
/// When CPUID leaf 0 is not supported.
pub fn get_vendor_id_from_host() -> Option<[u8; 12]> {
    // SAFETY: call cpuid with valid leaves
    unsafe {
        let entry = std::arch::x86_64::__cpuid(0);
        // The ordering of the vendor string is ebx,edx,ecx this is not a mistake.
        Some(std::mem::transmute::<[u32; 3], [u8; 12]>([
            entry.ebx, entry.edx, entry.ecx,
        ]))
    }
}

#[derive(Debug, Default, serde::Deserialize, serde::Serialize)]
pub struct CustomCpuId {
    pub cpuids: Vec<CpuIdCustomEntry>,
}

/// Target register to be modified by a bitmap.
#[derive(Debug, Clone)]
pub struct CpuidRegisterModifier {
    /// CPUID register to be modified by the bitmap.
    pub register: CpuidReg,
    /// Bit mapping to be applied as a modifier to the
    /// register's value at the address provided.
    pub bitmap: RegisterValueFilter,
}

/// Composite type that holistically provides
/// the location of a specific register being used
/// in the context of a CPUID tree.
#[derive(Debug, Default, Clone)]
pub struct CpuidLeafModifier {
    /// Leaf value.
    pub leaf: u32,
    /// Sub-Leaf value.
    pub subleaf: u32,
    /// KVM feature flags for this leaf-subleaf.
    pub flags: u32,
    /// All registers to be modified under the sub-leaf.
    pub modifiers: Vec<CpuidRegisterModifier>,
}

/// Bit-mapped value to adjust targeted bits of a register.
#[derive(Debug, Default, Clone, Copy, Eq, PartialEq, Hash)]
pub struct RegisterValueFilter {
    /// Filter to be used when writing the value bits.
    pub filter: u32,
    /// Value to be applied.
    pub value: u32,
}

impl RegisterValueFilter {
    /// Applies filter to the value
    #[inline]
    pub fn apply(&self, value: u32) -> u32 {
        (value & !self.filter) | self.value
    }
}

/// Wrapper type to containing x86_64 CPU config modifiers.
#[derive(Debug, Default, Clone)]
pub struct CustomCpuTemplate {
    /// Modifiers for CPUID configuration.
    pub cpuid_modifiers: Vec<CpuidLeafModifier>,
}

pub fn apply_template(cpuids: &mut [CpuIdEntry], template: &CustomCpuTemplate) {
    // Apply CPUID modifiers
    for mod_leaf in template.cpuid_modifiers.iter() {
        let function = mod_leaf.leaf;
        let index = mod_leaf.subleaf;

        for entry in &mut *cpuids {
            if entry.function == function && entry.index == index {
                entry.flags = mod_leaf.flags;

                for mod_reg in &mod_leaf.modifiers {
                    match mod_reg.register {
                        CpuidReg::EAX => {
                            entry.eax = mod_reg.bitmap.apply(entry.eax);
                        }
                        CpuidReg::EBX => {
                            entry.ebx = mod_reg.bitmap.apply(entry.ebx);
                        }
                        CpuidReg::ECX => {
                            entry.ecx = mod_reg.bitmap.apply(entry.ecx);
                        }
                        CpuidReg::EDX => {
                            entry.edx = mod_reg.bitmap.apply(entry.edx);
                        }
                    }
                }
            }
        }
    }
}

/// T2CL template
///
/// Mask CPUID to make exposed CPU features as close as possbile to Intel Cascade Lake and provide
/// instruction set feature partity with AMD Milan using T2A template.
///
/// References:
/// - Intel SDM: https://cdrdv2.intel.com/v1/dl/getContent/671200
/// - AMD APM: https://www.amd.com/system/files/TechDocs/40332.pdf
/// - CPUID Enumeration and Architectural MSRs: https://www.intel.com/content/www/us/en/developer/articles/technical/software-security-guidance/technical-documentation/cpuid-enumeration-and-architectural-msrs.html
#[allow(clippy::unusual_byte_groupings)]
pub fn t2cl() -> CustomCpuTemplate {
    CustomCpuTemplate {
        cpuid_modifiers: vec![
            CpuidLeafModifier {
                leaf: 0x1,
                subleaf: 0x0,
                flags: 0,
                modifiers: vec![
                    // EAX: Version Information
                    // - Bits 03-00: Stepping ID (Intel SDM) / Stepping (AMD APM)
                    // - Bits 07-04: Model (Intel SDM) / BaseModel (AMD APM)
                    // - Bits 11-08: Family (Intel SDM) / BaseFamily (AMD APM)
                    // - Bits 13-12: Processor Type (Intel SDM) / Reserved (AMD APM)
                    // - Bits 19-16: Extended Model ID (Intel SDM) / ExtModel (AMD APM)
                    // - Bits 27-20: Extended Family ID (Intel SDM) / ExtFamily (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EAX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_11111111_1111_00_11_1111_1111_1111,
                            value: 0b0000_00000000_0011_00_00_0110_1111_0010,
                        },
                    },
                    // ECX: Feature Information
                    // - Bit 02: DTES64 (Intel SDM) / Reserved (AMD APM)
                    // - Bit 03: MONITOR (Intel SDM) / MONITOR (AMD APM)
                    // - Bit 04: DS-CPL (Intel SDM) / Reserved (AMD APM)
                    // - Bit 05: VMX (Intel SDM) / Reserved (AMD APM)
                    // - Bit 06: SMX (Intel SDM) / Reserved (AMD APM)
                    // - Bit 07: EIST (Intel SDM) / Reserved (AMD APM)
                    // - Bit 08: TM2 (Intel SDM) / Reserved (AMD APM)
                    // - Bit 10: CNXT-ID (Intel SDM) / Reserved (AMD APM)
                    // - Bit 11: SDBG (Intel SDM) / Reserved (AMD APM)
                    // - Bit 14: xTPR Update Control (Intel SDM) / Reserved (AMD APM)
                    // - Bit 15: PDCM (Intel SDM) / Reserved (AMD APM)
                    // - Bit 18: DCA (Intel SDM) / Reserevd (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::ECX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0100_1100_1101_1111_1100,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                    // EDX: Feature Information
                    // - Bit 07: MCE (Intel SDM) / MCE (AMD APM)
                    // - Bit 12: MTRR (Intel SDM) / MTRR (AMD APM)
                    // - Bit 18: PSN (Intel SDM) / Reserved (AMD APM)
                    // - Bit 21: DS (Intel SDM) / Reserved (AMD APM)PC
                    // - Bit 22: ACPI (Intel SDM) / Reserved (AMD APM)
                    // - Bit 27: SS (Intel SDM) / Reserved (AMD APM)
                    // - Bit 29: TM (Intel SDM) / Reserved (AMD APM)
                    // - Bit 30: IA64 (deprecated) / Reserved (AMD APM) https://www.intel.com/content/dam/www/public/us/en/documents/manuals/itanium-architecture-vol-4-manual.pdf
                    // - Bit 31: PBE (Intel SDM) / Reserved (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EDX,
                        bitmap: RegisterValueFilter {
                            filter: 0b1110_1000_0110_0100_0001_0000_1000_0000,
                            value: 0b0000_0000_0000_0000_0001_0000_1000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0x7,
                subleaf: 0x0,
                flags: 1,
                modifiers: vec![
                    // EBX:
                    // - Bit 02: SGX (Intel SDM) / Reserved (AMD APM)
                    // - Bit 04: HLE (Intel SDM) / Reserved (AMD APM)
                    // - Bit 09: Enhanced REP MOVSB/STOSB (Intel SDM) / Reserved (AMD APM)
                    // - Bit 11: RTM (Intel SDM) / Reserved (AMD APM)
                    // - Bit 12: RDT-M (Intel SDM) / PQM (AMD APM)
                    // - Bit 14: MPX (Intel SDM) / Reserved (AMD APM)
                    // - Bit 15: RDT-A (Intel SDM) / PQE (AMD APM)
                    // - Bit 16: AVX512F (Intel SDM) / Reserved (AMD APM)
                    // - Bit 17: AVX512DQ (Intel SDM) / Reserved (AMD APM)
                    // - Bit 18: RDSEED (Intel SDM) / RDSEED (AMD APM)
                    // - Bit 19: ADX (Intel SDM) / ADX (AMD APM)
                    // - Bit 21: AVX512_IFMA (Intel SDM) / Reserved (AMD APM)
                    // - Bit 22: Reserved (Intel SDM) / RDPID (AMD APM)
                    //   On kernel codebase and Intel SDM, RDPID is enumerated at CPUID.07h:ECX.RDPID[bit 22].
                    //   https://elixir.bootlin.com/linux/v6.3.8/source/arch/x86/include/asm/cpufeatures.h#L389
                    // - Bit 23: CLFLUSHOPT (Intel SDM) / CLFLUSHOPT (AMD APM)
                    // - Bit 24: CLWB (Intel SDM) / CLWB (AMD APM)
                    // - Bit 25: Intel Processor Trace (Intel SDM) / Reserved (AMD APM)
                    // - Bit 26: AVX512PF (Intel SDM) / Reserved (AMD APM)
                    // - Bit 27: AVX512ER (Intel SDM) / Reserved (AMD APM)
                    // - Bit 28: AVX512CD (Intel SDM) / Reserved (AMD APM)
                    // - Bit 29: SHA (Intel SDM) / SHA (AMD APM)
                    // - Bit 30: AVX512BW (Intel SDM) / Reserved (AMD APM)
                    // - Bit 31: AVX512VL (Intel SDM) / Reserved (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EBX,
                        bitmap: RegisterValueFilter {
                            filter: 0b1111_1111_1110_1111_1101_1010_0001_0100,
                            value: 0b0000_0000_0000_0000_0000_0010_0000_0000,
                        },
                    },
                    // ECX:
                    // - Bit 01: AVX512_VBMI (Intel SDM) / Reserved (AMD APM)
                    // - Bit 02: UMIP (Intel SDM) / UMIP (AMD APM)
                    // - Bit 03: PKU (Intel SDM) / PKU (AMD APM)
                    // - Bit 04: OSPKE (Intel SDM) / OSPKE (AMD APM)
                    // - Bit 06: AVX512_VBMI2 (Intel SDM) / Reserved (AMD APM)
                    // - Bit 08: GFNI (Intel SDM) / Reserved (AMD APM)
                    // - Bit 09: VAES (Intel SDM) / VAES (AMD APM)
                    // - Bit 10: VPCLMULQDQ (Intel SDM) / VPCLMULQDQ (AMD APM)
                    // - Bit 11: AVX512_VNNI (Intel SDM) / Reserved (AMD APM)
                    // - Bit 12: AVX512_BITALG (Intel SDM) / Reserved (AMD APM)
                    // - Bit 14: AVX512_VPOPCNTDQ (Intel SDM) / Reserved (AMD APM)
                    // - Bit 16: LA57 (Intel SDM) / LA57 (AMD APM)
                    // - Bit 22: RDPID and IA32_TSC_AUX (Intel SDM) / Reserved (AMD APM)
                    // - Bit 30: SGX_LC (Intel SDM) / Reserved (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::ECX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0100_0000_0100_0001_0101_1111_0101_1110,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                    // EDX:
                    // - Bit 02: AVX512_4VNNIW (Intel SDM) / Reserved (AMD APM)
                    // - Bit 03: AVX512_4FMAPS (Intel SDM) / Reserved (AMD APM)
                    // - Bit 04: Fast Short REP MOV (Intel SDM) / Reserved (AMD APM)
                    // - Bit 08: AVX512_VP2INTERSECT (Intel SDM) / Reserved (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EDX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0000_0000_0001_0001_1100,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0xd,
                subleaf: 0x0,
                flags: 1,
                modifiers: vec![
                    // EAX:
                    // - Bits 04-03: MPX state (Intel SDM) / Reserved (AMD APM)
                    // - Bits 07-05: AVX-512 state (Intel SDM) / Reserved (AMD APM)
                    // - Bit 09: PKRU state (Intel SDM) / MPK (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EAX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0000_0000_00_1_0_111_11_000,
                            value: 0b0000_0000_0000_0000_0000_00_0_0_000_00_000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0xd,
                subleaf: 0x1,
                flags: 1,
                modifiers: vec![
                    // EAX:
                    // - Bit 01: Supports XSAVEC and the compacted form of XRSTOR (Intel SDM) /
                    //   XSAVEC (AMD APM)
                    // - Bit 02: Supports XGETBV (Intel SDM) / XGETBV (AMD APM)
                    // - Bit 03: Supports XSAVES/XRSTORS and IA32_XSS (Intel SDM) / XSAVES (AMD
                    //   APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EAX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0000_0000_0000_0000_1110,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0x80000001,
                subleaf: 0x0,
                flags: 0,
                modifiers: vec![
                    // ECX:
                    // - Bit 06: Reserved (Intel SDM) / SSE4A (AMD APM)
                    // - Bit 07: Reserved (Intel SDM) / MisAlignSse (AMD APM)
                    // - Bit 08: PREFETCHW (Intel SDM) / 3DNowPrefetch (AMD APM)
                    // - Bit 29: MONITORX and MWAITX (Intel SDM) / MONITORX (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::ECX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0010_0000_0000_0000_0000_0001_1100_0000,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                    // EDX:
                    // - Bit 22: Reserved (Intel SDM) / MmxExt (AMD APM)
                    // - Bit 23: Reserved (Intel SDM) / MMX (AMD APM)
                    // - Bit 24: Reserved (Intel SDM) / FSXR (AMD APM)
                    // - Bit 25: Reserved (Intel SDM) / FFXSR (AMD APM)
                    // - Bit 26: 1-GByte pages (Intel SDM) / Page1GB (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EDX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0111_1100_0000_0000_0000_0000_0000,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0x80000008,
                subleaf: 0x0,
                flags: 0,
                modifiers: vec![
                    // EBX:
                    // - Bit 09: WBNOINVD (Intel SDM) / WBNOINVD (AMD APM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EBX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0000_0000_0010_0000_0000,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                ],
            },
        ],
    }
}

/// T2A template
///
/// Provide instruction set feature partity with Intel Cascade Lake or later using T2CL template.
///
/// References:
/// - Intel SDM: https://cdrdv2.intel.com/v1/dl/getContent/671200
/// - AMD APM: https://www.amd.com/system/files/TechDocs/40332.pdf
/// - CPUID Enumeration and Architectural MSRs: https://www.intel.com/content/www/us/en/developer/articles/technical/software-security-guidance/technical-documentation/cpuid-enumeration-and-architectural-msrs.html
#[allow(clippy::unusual_byte_groupings)]
pub fn t2a() -> CustomCpuTemplate {
    CustomCpuTemplate {
        cpuid_modifiers: vec![
            CpuidLeafModifier {
                leaf: 0x1,
                subleaf: 0x0,
                flags: 0,
                modifiers: vec![
                    // EAX: Version Information
                    // - Bits 03-00: Stepping (AMD APM) / Stepping ID (Intel SDM)
                    // - Bits 07-04: BaseModel (AMD APM) / Model (Intel SDM)
                    // - Bits 11-08: BaseFamily (AMD APM) / Family (Intel SDM)
                    // - Bits 13-12: Reserved (AMD APM) / Processor Type (Intel SDM)
                    // - Bits 19-16: ExtModel (AMD APM) / Extended Model ID (Intel SDM)
                    // - Bits 27-20: ExtFamily (AMD APM) / Extended Family ID (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EAX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_11111111_1111_00_11_1111_1111_1111,
                            value: 0b0000_00000000_0011_00_00_0110_1111_0010,
                        },
                    },
                    // ECX: Feature Information
                    // - Bit 02: Reserved (AMD APM) / DTES64 (Intel SDM)
                    // - Bit 03: MONITOR (AMD APM) / MONITOR (Intel SDM)
                    // - Bit 04: Reserved (AMD APM) / DS-CPL (Intel SDM)
                    // - Bit 05: Reserved (AMD APM) / VMX (Intel SDM)
                    // - Bit 06: Reserved (AMD APM) / SMX (Intel SDM)
                    // - Bit 07: Reserved (AMD APM) / EIST (Intel SDM)
                    // - Bit 08: Reserved (AMD APM) / TM2 (Intel SDM)
                    // - Bit 10: Reserved (AMD APM) / CNXT-ID (Intel SDM)
                    // - Bit 11: Reserved (AMD APM) / SDBG (Intel SDM)
                    // - Bit 14: Reserved (AMD APM) / xTPR Update Control (Intel SDM)
                    // - Bit 15: Reserved (AMD APM) / PDCM (Intel SDM)
                    // - Bit 18: Reserevd (AMD APM) / DCA (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::ECX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0100_1100_1101_1111_1100,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                    // EDX: Feature Information
                    // - Bit 07: MCE (AMD APM) / MCE (Intel SDM)
                    // - Bit 12: MTRR (AMD APM) / MTRR (Intel SDM)
                    // - Bit 18: Reserved (AMD APM) / PSN (Intel SDM)
                    // - Bit 21: Reserved (AMD APM) / DS (Intel SDM)
                    // - Bit 22: Reserved (AMD APM) / ACPI (Intel SDM)
                    // - Bit 27: Reserved (AMD APM) / SS (Intel SDM)
                    // - Bit 29: Reserved (AMD APM) / TM (Intel SDM)
                    // - Bit 30: Reserved (AMD APM) / IA-64 (deprecated) https://www.intel.com/content/dam/www/public/us/en/documents/manuals/itanium-architecture-vol-4-manual.pdf
                    // - Bit 31: Reserved (AMD APM) / PBE (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EDX,
                        bitmap: RegisterValueFilter {
                            filter: 0b1110_1000_0110_0100_0001_0000_1000_0000,
                            value: 0b0000_0000_0000_0000_0001_0000_1000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0x7,
                subleaf: 0x0,
                flags: 1,
                modifiers: vec![
                    // EBX:
                    // - Bit 02: Reserved (AMD APM) / SGX (Intel SDM)
                    // - Bit 04: Reserved (AMD APM) / HLE (Intel SDM)
                    // - Bit 09: Reserved (AMD APM) / Enhanced REP MOVSB/STOSB (Intel SDM)
                    // - Bit 11: Reserved (AMD APM) / RTM (Intel SDM)
                    // - Bit 12: PQM (AMD APM) / RDT-M (Intel SDM)
                    // - Bit 14: Reserved (AMD APM) / MPX (Intel SDM)
                    // - Bit 15: PQE (AMD APM) / RDT-A (Intel SDM)
                    // - Bit 16: Reserved (AMD APM) / AVX512F (Intel SDM)
                    // - Bit 17: Reserved (AMD APM) / AVX512DQ (Intel SDM)
                    // - Bit 18: RDSEED (AMD APM) / RDSEED (Intel SDM)
                    // - Bit 19: ADX (AMD APM) / ADX (Intel SDM)
                    // - Bit 21: Reserved (AMD APM) / AVX512_IFMA (Intel SDM)
                    // - Bit 22: RDPID (AMD APM) / Reserved (Intel SDM)
                    //   On kernel codebase and Intel SDM, RDPID is enumerated at CPUID.07h:ECX.RDPID[bit 22].
                    //   https://elixir.bootlin.com/linux/v6.3.8/source/arch/x86/include/asm/cpufeatures.h#L389
                    // - Bit 23: CLFLUSHOPT (AMD APM) / CLFLUSHOPT (Intel SDM)
                    // - Bit 24: CLWB (AMD APM) / CLWB (Intel SDM)
                    // - Bit 25: Reserved (AMD APM) / Intel Processor Trace (Intel SDM)
                    // - Bit 26: Reserved (AMD APM) / AVX512PF (Intel SDM)
                    // - Bit 27: Reserved (AMD APM) / AVX512ER (Intel SDM)
                    // - Bit 28: Reserved (AMD APM) / AVX512CD (Intel SDM)
                    // - Bit 29: SHA (AMD APM) / SHA (Intel SDM)
                    // - Bit 30: Reserved (AMD APM) / AVX512BW (Intel SDM)
                    // - Bit 31: Reserved (AMD APM) / AVX512VL (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EBX,
                        bitmap: RegisterValueFilter {
                            filter: 0b1111_1111_1110_1111_1101_1010_0001_0100,
                            value: 0b0000_0000_0000_0000_0000_0010_0000_0000,
                        },
                    },
                    // ECX:
                    // - Bit 01: Reserved (AMD APM) / AVX512_VBMI (Intel SDM)
                    // - Bit 02: UMIP (AMD APM) / UMIP (Intel SDM)
                    // - Bit 03: PKU (AMD APM) / PKU (Intel SDM)
                    // - Bit 04: OSPKE (AMD APM) / OSPKE (Intel SDM)
                    // - Bit 06: Reserved (AMD APM) / AVX512_VBMI2 (Intel SDM)
                    // - Bit 08: Reserved (AMD APM) / GFNI (Intel SDM)
                    // - Bit 09: VAES (AMD APM) / VAES (Intel SDM)
                    // - Bit 10: VPCLMULQDQ (AMD APM) / VPCLMULQDQ (Intel SDM)
                    // - Bit 11: Reserved (AMD APM) / AVX512_VNNI (Intel SDM)
                    // - Bit 12: Reserved (AMD APM) / AVX512_BITALG (Intel SDM)
                    // - Bit 14: Reserved (AMD APM) / AVX512_VPOPCNTDQ (Intel SDM)
                    // - Bit 16: LA57 (AMD APM) / LA57 (Intel SDM)
                    // - Bit 22: Reserved (AMD APM) / RDPID and IA32_TSC_AUX (Intel SDM)
                    // - Bit 30: Reserved (AMD APM) / SGX_LC (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::ECX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0100_0000_0100_0001_0101_1111_0101_1110,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                    // EDX:
                    // - Bit 02: Reserved (AMD APM) / AVX512_4VNNIW (Intel SDM)
                    // - Bit 03: Reserved (AMD APM) / AVX512_4FMAPS (Intel SDM)
                    // - Bit 04: Reserved (AMD APM) / Fast Short REP MOV (Intel SDM)
                    // - Bit 08: Reserved (AMD APM) / AVX512_VP2INTERSECT (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EDX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0000_0000_0001_0001_1100,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0xd,
                subleaf: 0x0,
                flags: 1,
                modifiers: vec![
                    // EAX:
                    // - Bits 04-03: Reserved (AMD APM) / MPX state (Intel SDM)
                    // - Bits 07-05: Reserved (AMD APM) / AVX-512 state (Intel SDM)
                    // - Bit 09: MPK (AMD APM) / PKRU state (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EAX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0000_0000_00_1_0_111_11_000,
                            value: 0b0000_0000_0000_0000_0000_00_0_0_000_00_000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0xd,
                subleaf: 0x1,
                flags: 1,
                modifiers: vec![
                    // EAX:
                    // - Bit 01: XSAVEC (AMD APM) / Supports XSAVEC and the compacted form of
                    //   XRSTOR (Intel SDM)
                    // - Bit 02: XGETBV (AMD APM) / Supports XGETBV (Intel SDM)
                    // - Bit 03: XSAVES (AMD APM) / Supports XSAVES/XRSTORS and IA32_XSS (Intel
                    //   SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EAX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0000_0000_0000_0000_0000_1110,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0x80000001,
                subleaf: 0x0,
                flags: 0,
                modifiers: vec![
                    // ECX:
                    // - Bit 02: SVM (AMD APM) / Reserved (Intel SDM)
                    // - Bit 06: SSE4A (AMD APM) / Reserved (Intel SDM)
                    // - Bit 07: MisAlignSse (AMD APM) / Reserved (Intel SDM)
                    // - Bit 08: 3DNowPrefetch (AMD APM) / PREFETCHW (Intel SDM)
                    // - Bit 29: MONITORX (AMD APM) / MONITORX and MWAITX (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::ECX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0010_0000_0000_0000_0000_0001_1100_0100,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                    // EDX:
                    // - Bit 22: MmxExt (AMD APM) / Reserved (Intel SDM)
                    // - Bit 25: FFXSR (AMD APM) / Reserved (Intel SDM)
                    // - Bit 26: Page1GB (AMD APM) / 1-GByte pages (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EDX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0110_0100_0000_0000_0000_0000_0000,
                            value: 0b0000_0000_0000_0000_0000_0000_0000_0000,
                        },
                    },
                ],
            },
            CpuidLeafModifier {
                leaf: 0x80000008,
                subleaf: 0x0,
                flags: 0,
                modifiers: vec![
                    // EBX:
                    // - Bit 00: CLZERO (AMD APM) / Reserved (Intel SDM)
                    // - Bit 02: RstrFpErrPtrs (AMD APM) / Reserved (Intel SDM)
                    // - Bit 09: WBNOINVD (AMD APM) / WBNOINVD (Intel SDM)
                    // - Bit 18: IbrsPreferred (ADM APM) / Reserved (Intel SDm)
                    // - Bit 19: IbrsSameMode (AMD APM) / Reserved (Intel SDM)
                    // - Bit 20: EferLmsleUnsupported (AMD APM) / Reserved (Intel SDM)
                    CpuidRegisterModifier {
                        register: CpuidReg::EBX,
                        bitmap: RegisterValueFilter {
                            filter: 0b0000_0000_0001_1100_0000_0010_0000_0101,
                            value: 0b0000_0000_0001_1100_0000_0000_0000_0100,
                        },
                    },
                ],
            },
        ],
    }
}
