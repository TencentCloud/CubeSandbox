// Copyright © 2022, Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::anyhow;
#[cfg(target_arch = "aarch64")]
use arch::aarch64::layout::{TPM_SIZE, TPM_START};
#[cfg(target_arch = "x86_64")]
use arch::x86_64::layout::{TPM_SIZE, TPM_START};
use phf::phf_map;
use std::cmp;
use std::sync::{Arc, Barrier};
use thiserror::Error;
use tpm::emulator::{BackendCmd, Emulator};
use tpm::TPM_CRB_BUFFER_MAX;
use tpm::TPM_SUCCESS;
use vm_device::BusDevice;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Emulator doesn't implement min required capabilities: {0}")]
    CheckCaps(#[source] anyhow::Error),
    #[error("Failed to initialize tpm: {0}")]
    Init(#[source] anyhow::Error),
    #[error("Failed to deliver tpm Command: {0}")]
    DeliverRequest(#[source] anyhow::Error),
}
type Result<T> = anyhow::Result<T, Error>;

/* crb 32-bit registers */
const CRB_LOC_STATE: u32 = 0x0;
//Register Fields
// Field => (start, length)
// start: lowest bit in the bit field numbered from 0
// length: length of the bit field
const CRB_LOC_STATE_FIELDS: phf::Map<&str, [u32; 2]> = phf_map! {
    "tpmEstablished" => [0, 1],
    "locAssigned" => [1,1],
    "activeLocality"=> [2, 3],
    "reserved" => [5, 2],
    "tpmRegValidSts" => [7, 1]
};
const CRB_LOC_CTRL: u32 = 0x08;
const CRB_LOC_CTRL_REQUEST_ACCESS: u32 = 1 << 0;
const CRB_LOC_CTRL_RELINQUISH: u32 = 1 << 1;
const CRB_LOC_CTRL_RESET_ESTABLISHMENT_BIT: u32 = 1 << 3;
const CRB_LOC_STS: u32 = 0x0C;
const CRB_LOC_STS_FIELDS: phf::Map<&str, [u32; 2]> = phf_map! {
    "Granted" => [0, 1],
    "beenSeized" => [1,1]
};
const CRB_INTF_ID: u32 = 0x30;
const CRB_INTF_ID_FIELDS: phf::Map<&str, [u32; 2]> = phf_map! {
    "InterfaceType" => [0, 4],
    "InterfaceVersion" => [4, 4],
    "CapLocality" =>  [8, 1],
    "CapCRBIdleBypass" => [9, 1],
    "Reserved1" => [10, 1],
    "CapDataXferSizeSupport" => [11, 2],
    "CapFIFO" =>  [13, 1],
    "CapCRB" => [14, 1],
    "CapIFRes" => [15, 2],
    "InterfaceSelector" => [17, 2],
    "IntfSelLock" =>  [19, 1],
    "Reserved2" => [20, 4],
    "RID" => [24, 8]
};
const CRB_INTF_ID2: u32 = 0x34;
const CRB_INTF_ID2_FIELDS: phf::Map<&str, [u32; 2]> = phf_map! {
    "VID" => [0, 16],
    "DID" => [16, 16]
};
const CRB_CTRL_REQ: u32 = 0x40;
const CRB_CTRL_REQ_CMD_READY: u32 = 1 << 0;
const CRB_CTRL_REQ_GO_IDLE: u32 = 1 << 1;
const CRB_CTRL_STS: u32 = 0x44;
const CRB_CTRL_STS_FIELDS: phf::Map<&str, [u32; 2]> = phf_map! {
    "tpmSts" => [0, 1],
    "tpmIdle" => [1, 1]
};
const CRB_CTRL_CANCEL: u32 = 0x48;
const CRB_CANCEL_INVOKE: u32 = 1 << 0;
const CRB_CTRL_START: u32 = 0x4C;
const CRB_START_INVOKE: u32 = 1 << 0;
const CRB_CTRL_CMD_LADDR: u32 = 0x5C;
const CRB_CTRL_CMD_HADDR: u32 = 0x60;
const CRB_CTRL_RSP_SIZE: u32 = 0x64;
const CRB_CTRL_RSP_ADDR: u32 = 0x68;
const CRB_DATA_BUFFER: u32 = 0x80;

const TPM_CRB_NO_LOCALITY: u32 = 0xff;

const TPM_CRB_ADDR_BASE: u32 = TPM_START.0 as u32;
const TPM_CRB_ADDR_SIZE: usize = TPM_SIZE as usize;

const TPM_CRB_R_MAX: u32 = CRB_DATA_BUFFER;

// CRB Protocol details
const CRB_INTF_TYPE_CRB_ACTIVE: u32 = 0b1;
const CRB_INTF_VERSION_CRB: u32 = 0b1;
const CRB_INTF_CAP_LOCALITY_0_ONLY: u32 = 0b0;
const CRB_INTF_CAP_IDLE_FAST: u32 = 0b0;
const CRB_INTF_CAP_XFER_SIZE_64: u32 = 0b11;
const CRB_INTF_CAP_FIFO_NOT_SUPPORTED: u32 = 0b0;
const CRB_INTF_CAP_CRB_SUPPORTED: u32 = 0b1;
const CRB_INTF_IF_SELECTOR_CRB: u32 = 0b1;
const PCI_VENDOR_ID_IBM: u32 = 0x1014;
const CRB_CTRL_CMD_SIZE_REG: u32 = 0x58;
const CRB_CTRL_CMD_SIZE: usize = TPM_CRB_ADDR_SIZE - CRB_DATA_BUFFER as usize;

fn get_fields_map(reg: u32) -> phf::Map<&'static str, [u32; 2]> {
    match reg {
        CRB_LOC_STATE => CRB_LOC_STATE_FIELDS,
        CRB_LOC_STS => CRB_LOC_STS_FIELDS,
        CRB_INTF_ID => CRB_INTF_ID_FIELDS,
        CRB_INTF_ID2 => CRB_INTF_ID2_FIELDS,
        CRB_CTRL_STS => CRB_CTRL_STS_FIELDS,
        _ => {
            panic!(
                "Fields in '{:?}' register were accessed which are Invalid",
                reg
            );
        }
    }
}

/// Set a particular field in a Register
fn set_reg_field(regs: &mut [u32; TPM_CRB_R_MAX as usize], reg: u32, field: &str, value: u32) {
    let reg_fields = get_fields_map(reg);
    if reg_fields.contains_key(field) {
        let start = reg_fields.get(field).unwrap()[0];
        let len = reg_fields.get(field).unwrap()[1];
        let mask = (!(0_u32) >> (32 - len)) << start;
        regs[reg as usize] = (regs[reg as usize] & !mask) | ((value << start) & mask);
    } else {
        error!(
            "Failed to tpm Register. {:?} is not a valid field in Reg {:#X}",
            field, reg
        )
    }
}

/// Get the value of a particular field in a Register
fn get_reg_field(regs: &[u32; TPM_CRB_R_MAX as usize], reg: u32, field: &str) -> u32 {
    let reg_fields = get_fields_map(reg);
    if reg_fields.contains_key(field) {
        let start = reg_fields.get(field).unwrap()[0];
        let len = reg_fields.get(field).unwrap()[1];
        let mask = (!(0_u32) >> (32 - len)) << start;
        (regs[reg as usize] & mask) >> start
    } else {
        // TODO: Sensible return value if fields do not exist
        0x0
    }
}

fn locality_from_addr(addr: u32) -> u8 {
    (addr >> 12) as u8
}

pub struct Tpm {
    emulator: Emulator,
    cmd: Option<BackendCmd>,
    regs: [u32; TPM_CRB_R_MAX as usize],
    backend_buff_size: usize,
    data_buff: [u8; TPM_CRB_BUFFER_MAX],
    data_buff_len: usize,
}

impl Tpm {
    pub fn new(path: String) -> Result<Self> {
        let emulator = Emulator::new(path)
            .map_err(|e| Error::Init(anyhow!("Failed while initializing tpm Emulator: {:?}", e)))?;
        let mut tpm = Tpm {
            emulator,
            cmd: None,
            regs: [0; TPM_CRB_R_MAX as usize],
            backend_buff_size: TPM_CRB_BUFFER_MAX,
            data_buff: [0; TPM_CRB_BUFFER_MAX],
            data_buff_len: 0,
        };
        tpm.reset()?;
        Ok(tpm)
    }

    fn get_active_locality(&mut self) -> u32 {
        if get_reg_field(&self.regs, CRB_LOC_STATE, "locAssigned") == 0 {
            return TPM_CRB_NO_LOCALITY;
        }
        get_reg_field(&self.regs, CRB_LOC_STATE, "activeLocality")
    }

    fn request_completed(&mut self, result: isize) {
        self.regs[CRB_CTRL_START as usize] = !CRB_START_INVOKE;
        if result != 0 {
            set_reg_field(&mut self.regs, CRB_CTRL_STS, "tpmSts", 1);
        }
    }

    fn reset(&mut self) -> Result<()> {
        let cur_buff_size = self.emulator.get_buffer_size().unwrap();
        self.regs = [0; TPM_CRB_R_MAX as usize];
        set_reg_field(&mut self.regs, CRB_LOC_STATE, "tpmRegValidSts", 1);
        set_reg_field(&mut self.regs, CRB_CTRL_STS, "tpmIdle", 1);
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "InterfaceType",
            CRB_INTF_TYPE_CRB_ACTIVE,
        );
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "InterfaceVersion",
            CRB_INTF_VERSION_CRB,
        );
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "CapLocality",
            CRB_INTF_CAP_LOCALITY_0_ONLY,
        );
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "CapCRBIdleBypass",
            CRB_INTF_CAP_IDLE_FAST,
        );
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "CapDataXferSizeSupport",
            CRB_INTF_CAP_XFER_SIZE_64,
        );
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "CapFIFO",
            CRB_INTF_CAP_FIFO_NOT_SUPPORTED,
        );
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "CapCRB",
            CRB_INTF_CAP_CRB_SUPPORTED,
        );
        set_reg_field(
            &mut self.regs,
            CRB_INTF_ID,
            "InterfaceSelector",
            CRB_INTF_IF_SELECTOR_CRB,
        );
        set_reg_field(&mut self.regs, CRB_INTF_ID, "RID", 0b0000);
        set_reg_field(&mut self.regs, CRB_INTF_ID2, "VID", PCI_VENDOR_ID_IBM);

        self.regs[CRB_CTRL_CMD_SIZE_REG as usize] = CRB_CTRL_CMD_SIZE as u32;
        self.regs[CRB_CTRL_CMD_LADDR as usize] = TPM_CRB_ADDR_BASE + CRB_DATA_BUFFER;
        self.regs[CRB_CTRL_RSP_SIZE as usize] = CRB_CTRL_CMD_SIZE as u32;
        self.regs[CRB_CTRL_RSP_ADDR as usize] = TPM_CRB_ADDR_BASE + CRB_DATA_BUFFER;

        self.backend_buff_size = cmp::min(cur_buff_size, TPM_CRB_BUFFER_MAX);

        if let Err(e) = self.emulator.startup_tpm(self.backend_buff_size) {
            return Err(Error::Init(anyhow!(
                "Failed while running Startup TPM. Error: {:?}",
                e
            )));
        }
        Ok(())
    }
}

//impl BusDevice for TPM
impl BusDevice for Tpm {
    fn read(&mut self, _base: u64, offset: u64, data: &mut [u8]) {
        let mut offset: u32 = offset as u32;
        let read_len: usize = data.len();

        if offset >= CRB_DATA_BUFFER
            && (offset + read_len as u32) < (CRB_DATA_BUFFER + self.data_buff.len() as u32)
        {
            // Read from Data Buffer
            let start: usize = (offset as usize) - (CRB_DATA_BUFFER as usize);
            let end: usize = start + read_len;
            data[..].clone_from_slice(&self.data_buff[start..end]);
        } else {
            offset &= 0xff;
            let mut val = self.regs[offset as usize];

            if offset == CRB_LOC_STATE && !self.emulator.get_established_flag() {
                val |= 0x1;
            }

            if data.len() <= 4 {
                data.clone_from_slice(val.to_ne_bytes()[0..read_len].as_ref());
            } else {
                error!(
                    "Invalid tpm read: offset {:#X}, data length {:?}",
                    offset,
                    data.len()
                );
            }
        }
        debug!(
            "MMIO Read: offset {:#X} len {:?} val = {:02X?}  ",
            offset,
            data.len(),
            data
        );
    }

    fn write(&mut self, _base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        debug!(
            "MMIO Write: offset {:#X} len {:?} input data {:02X?}",
            offset,
            data.len(),
            data
        );
        let mut offset: u32 = offset as u32;
        if offset < CRB_DATA_BUFFER {
            offset &= 0xff;
        }
        let locality = locality_from_addr(offset) as u32;
        let write_len = data.len();

        if offset >= CRB_DATA_BUFFER
            && (offset + write_len as u32) < (CRB_DATA_BUFFER + self.data_buff.len() as u32)
        {
            let start: usize = (offset as usize) - (CRB_DATA_BUFFER as usize);
            if start == 0 {
                // If filling data_buff at index 0, reset length to 0
                self.data_buff_len = 0;
                self.data_buff.fill(0);
            }
            let end: usize = start + data.len();
            self.data_buff[start..end].clone_from_slice(data);
            self.data_buff_len += data.len();
        } else {
            // Ctrl Commands that take more than 4 bytes as input are not yet supported
            // CTRL_RSP_ADDR usually gets 8 byte write request. Last 4 bytes are zeros.
            if write_len > 4 && offset != CRB_CTRL_RSP_ADDR {
                error!(
                    "Invalid tpm write: offset {:#X}, data length {}",
                    offset,
                    data.len()
                );
                return None;
            }

            let mut input: [u8; 4] = [0; 4];
            input.copy_from_slice(&data[0..4]);
            let v = u32::from_le_bytes(input);

            match offset {
                CRB_CTRL_CMD_SIZE_REG => {
                    self.regs[CRB_CTRL_CMD_SIZE_REG as usize] = v;
                }
                CRB_CTRL_CMD_LADDR => {
                    self.regs[CRB_CTRL_CMD_LADDR as usize] = v;
                }
                CRB_CTRL_CMD_HADDR => {
                    self.regs[CRB_CTRL_CMD_HADDR as usize] = v;
                }
                CRB_CTRL_RSP_SIZE => {
                    self.regs[CRB_CTRL_RSP_SIZE as usize] = v;
                }
                CRB_CTRL_RSP_ADDR => {
                    self.regs[CRB_CTRL_RSP_ADDR as usize] = v;
                }
                CRB_CTRL_REQ => match v {
                    CRB_CTRL_REQ_CMD_READY => {
                        set_reg_field(&mut self.regs, CRB_CTRL_STS, "tpmIdle", 0);
                    }
                    CRB_CTRL_REQ_GO_IDLE => {
                        set_reg_field(&mut self.regs, CRB_CTRL_STS, "tpmIdle", 1);
                    }
                    _ => {
                        error!("Invalid value passed to CRTL_REQ register");
                        return None;
                    }
                },
                CRB_CTRL_CANCEL => {
                    if v == CRB_CANCEL_INVOKE
                        && (self.regs[CRB_CTRL_START as usize] & CRB_START_INVOKE != 0)
                    {
                        if let Err(e) = self.emulator.cancel_cmd() {
                            error!("Failed to run cancel command. Error: {:?}", e);
                        }
                    }
                }
                CRB_CTRL_START => {
                    if v == CRB_START_INVOKE
                        && ((self.regs[CRB_CTRL_START as usize] & CRB_START_INVOKE) == 0)
                        && self.get_active_locality() == locality
                    {
                        self.regs[CRB_CTRL_START as usize] |= CRB_START_INVOKE;

                        self.cmd = Some(BackendCmd {
                            locality: locality as u8,
                            input: self.data_buff[0..self.data_buff_len].to_vec(),
                            input_len: cmp::min(self.data_buff_len, TPM_CRB_BUFFER_MAX),
                            output: self.data_buff.to_vec(),
                            output_len: TPM_CRB_BUFFER_MAX,
                            selftest_done: false,
                        });

                        let mut cmd = self.cmd.as_ref().unwrap().clone();
                        let output = self.emulator.deliver_request(&mut cmd).map_err(|e| {
                            Error::DeliverRequest(anyhow!(
                                "Failed to deliver tpm request. Error :{:?}",
                                e
                            ))
                        });
                        //TODO: drop the copy here
                        self.data_buff.fill(0);
                        self.data_buff.clone_from_slice(output.unwrap().as_slice());

                        self.request_completed(TPM_SUCCESS as isize);
                    }
                }
                CRB_LOC_CTRL => {
                    warn!(
                        "CRB_LOC_CTRL locality to write = {:?} val = {:?}",
                        locality, v
                    );
                    match v {
                        CRB_LOC_CTRL_RESET_ESTABLISHMENT_BIT => {}
                        CRB_LOC_CTRL_RELINQUISH => {
                            set_reg_field(&mut self.regs, CRB_LOC_STATE, "locAssigned", 0);
                            set_reg_field(&mut self.regs, CRB_LOC_STS, "Granted", 0);
                        }
                        CRB_LOC_CTRL_REQUEST_ACCESS => {
                            set_reg_field(&mut self.regs, CRB_LOC_STS, "Granted", 1);
                            set_reg_field(&mut self.regs, CRB_LOC_STS, "beenSeized", 0);
                            set_reg_field(&mut self.regs, CRB_LOC_STATE, "locAssigned", 1);
                        }
                        _ => {
                            error!("Invalid value to write in CRB_LOC_CTRL {:#X} ", v);
                        }
                    }
                }
                _ => {
                    error!(
                        "Invalid tpm write: offset {:#X}, data length {:?}",
                        offset,
                        data.len()
                    );
                }
            }
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_set_get_reg_field() {
        let mut regs: [u32; TPM_CRB_R_MAX as usize] = [0; TPM_CRB_R_MAX as usize];
        set_reg_field(&mut regs, CRB_INTF_ID, "RID", 0xAC);
        assert_eq!(
            get_reg_field(&regs, CRB_INTF_ID, "RID"),
            0xAC,
            concat!("Test: ", stringify!(set_get_reg_field))
        );
    }
}
