// Copyright Red Hat.
//
// SPDX-License-Identifier: Apache-2.0
//
use std::convert::TryInto;
use std::fmt;
use std::ops::Deref;
use std::str::FromStr;

use anyhow::anyhow;

// The PCI spec reserves 5 bits (0..31) for slot number (a.k.a. device
// number)
const SLOT_BITS: u8 = 5;
const SLOT_MAX: u8 = (1 << SLOT_BITS) - 1;

// The PCI spec reserves 3 bits (0..7) for function number
const FUNCTION_BITS: u8 = 3;
const FUNCTION_MAX: u8 = (1 << FUNCTION_BITS) - 1;

// Represents a PCI function's slot (a.k.a. device) and function
// numbers, giving its location on a single logical bus
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct SlotFn(u8);

impl SlotFn {
    pub fn new<T, U>(ss: T, f: U) -> anyhow::Result<Self>
    where
        T: TryInto<u8> + fmt::Display + Copy,
        U: TryInto<u8> + fmt::Display + Copy,
    {
        let ss8 = match ss.try_into() {
            Ok(ss8) if ss8 <= SLOT_MAX => ss8,
            _ => {
                return Err(anyhow!(
                    "PCI slot {} should be in range [0..{:#x}]",
                    ss,
                    SLOT_MAX
                ));
            }
        };

        let f8 = match f.try_into() {
            Ok(f8) if f8 <= FUNCTION_MAX => f8,
            _ => {
                return Err(anyhow!(
                    "PCI function {} should be in range [0..{:#x}]",
                    f,
                    FUNCTION_MAX
                ));
            }
        };

        Ok(SlotFn(ss8 << FUNCTION_BITS | f8))
    }

    pub fn slot(self) -> u8 {
        self.0 >> FUNCTION_BITS
    }

    pub fn function(self) -> u8 {
        self.0 & FUNCTION_MAX
    }
}

impl FromStr for SlotFn {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> anyhow::Result<Self> {
        let mut tokens = s.split('.').fuse();
        let slot = tokens.next();
        let func = tokens.next();

        if slot.is_none() || tokens.next().is_some() {
            return Err(anyhow!(
                "PCI slot/function {} should have the format SS.F",
                s
            ));
        }

        let slot = isize::from_str_radix(slot.unwrap(), 16)?;
        let func = match func {
            Some(func) => isize::from_str_radix(func, 16)?,
            None => 0,
        };

        SlotFn::new(slot, func)
    }
}

impl fmt::Display for SlotFn {
    fn fmt(&self, f: &mut fmt::Formatter) -> Result<(), fmt::Error> {
        write!(f, "{:02x}.{:01x}", self.slot(), self.function())
    }
}

#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct Address {
    domain: u16,
    bus: u8,
    slotfn: SlotFn,
}

impl Address {
    pub fn new(domain: u16, bus: u8, slotfn: SlotFn) -> Self {
        Address {
            domain,
            bus,
            slotfn,
        }
    }
}

impl FromStr for Address {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> anyhow::Result<Self> {
        let mut tokens = s.split(':').fuse();
        let domain = tokens.next();
        let bus = tokens.next();
        let slotfn = tokens.next();

        if domain.is_none() || bus.is_none() || slotfn.is_none() || tokens.next().is_some() {
            return Err(anyhow!(
                "PCI address {} should have the format DDDD:BB:SS.F",
                s
            ));
        }

        let domain = u16::from_str_radix(domain.unwrap(), 16)?;
        let bus = u8::from_str_radix(bus.unwrap(), 16)?;
        let slotfn = SlotFn::from_str(slotfn.unwrap())?;

        Ok(Address::new(domain, bus, slotfn))
    }
}

impl fmt::Display for Address {
    fn fmt(&self, f: &mut fmt::Formatter) -> Result<(), fmt::Error> {
        write!(f, "{:04x}:{:02x}:{}", self.domain, self.bus, self.slotfn)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Path(Vec<SlotFn>);

impl Path {
    pub fn new(slots: Vec<SlotFn>) -> anyhow::Result<Self> {
        if slots.is_empty() {
            return Err(anyhow!("PCI path must have at least one element"));
        }
        Ok(Path(slots))
    }
}

// Let Path be treated as a slice of Slots
impl Deref for Path {
    type Target = [SlotFn];

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl fmt::Display for Path {
    fn fmt(&self, f: &mut fmt::Formatter) -> Result<(), fmt::Error> {
        let sslots: Vec<String> = self
            .0
            .iter()
            .map(std::string::ToString::to_string)
            .collect();
        write!(f, "{}", sslots.join("/"))
    }
}

impl FromStr for Path {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> anyhow::Result<Self> {
        let rslots: anyhow::Result<Vec<SlotFn>> = s.split('/').map(SlotFn::from_str).collect();
        Path::new(rslots?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::str::FromStr;

    #[test]
    fn test_slotfn() {
        // Valid slots
        let sf = SlotFn::new(0x00, 0x0).unwrap();
        assert_eq!(format!("{}", sf), "00.0");

        let sf = SlotFn::from_str("00.0").unwrap();
        assert_eq!(format!("{}", sf), "00.0");

        let sf = SlotFn::from_str("00").unwrap();
        assert_eq!(format!("{}", sf), "00.0");

        let sf = SlotFn::new(31, 7).unwrap();
        let sf2 = SlotFn::from_str("1f.7").unwrap();
        assert_eq!(sf, sf2);

        // Bad slots
        let sf = SlotFn::new(-1, 0);
        assert!(sf.is_err());

        let sf = SlotFn::new(32, 0);
        assert!(sf.is_err());

        let sf = SlotFn::from_str("20.0");
        assert!(sf.is_err());

        let sf = SlotFn::from_str("20");
        assert!(sf.is_err());

        let sf = SlotFn::from_str("xy.0");
        assert!(sf.is_err());

        let sf = SlotFn::from_str("xy");
        assert!(sf.is_err());

        // Bad functions
        let sf = SlotFn::new(0, -1);
        assert!(sf.is_err());

        let sf = SlotFn::new(0, 8);
        assert!(sf.is_err());

        let sf = SlotFn::from_str("00.8");
        assert!(sf.is_err());

        let sf = SlotFn::from_str("00.x");
        assert!(sf.is_err());

        // Bad formats
        let sf = SlotFn::from_str("");
        assert!(sf.is_err());

        let sf = SlotFn::from_str("00.0.0");
        assert!(sf.is_err());

        let sf = SlotFn::from_str("00.0/");
        assert!(sf.is_err());

        let sf = SlotFn::from_str("00/");
        assert!(sf.is_err());
    }

    #[test]
    fn test_address() {
        // Valid addresses
        let sf0_0 = SlotFn::new(0, 0).unwrap();
        let sf1f_7 = SlotFn::new(0x1f, 7).unwrap();

        let addr = Address::new(0, 0, sf0_0);
        assert_eq!(format!("{}", addr), "0000:00:00.0");
        let addr2 = Address::from_str("0000:00:00.0").unwrap();
        assert_eq!(addr, addr2);

        let addr = Address::new(0xffff, 0xff, sf1f_7);
        assert_eq!(format!("{}", addr), "ffff:ff:1f.7");
        let addr2 = Address::from_str("ffff:ff:1f.7").unwrap();
        assert_eq!(addr, addr2);

        // Bad addresses
        let addr = Address::from_str("10000:00:00.0");
        assert!(addr.is_err());

        let addr = Address::from_str("0000:100:00.0");
        assert!(addr.is_err());

        let addr = Address::from_str("0000:00:20.0");
        assert!(addr.is_err());

        let addr = Address::from_str("0000:00:00.8");
        assert!(addr.is_err());

        let addr = Address::from_str("xyz");
        assert!(addr.is_err());

        let addr = Address::from_str("xyxy:xy:xy.z");
        assert!(addr.is_err());

        let addr = Address::from_str("0000:00:00.0:00");
        assert!(addr.is_err());
    }

    #[test]
    fn test_path() {
        let sf3_0 = SlotFn::new(0x03, 0).unwrap();
        let sf4_0 = SlotFn::new(0x04, 0).unwrap();
        let sf5_0 = SlotFn::new(0x05, 0).unwrap();
        let sfa_5 = SlotFn::new(0x0a, 5).unwrap();
        let sfb_6 = SlotFn::new(0x0b, 6).unwrap();
        let sfc_7 = SlotFn::new(0x0c, 7).unwrap();

        // Valid paths
        let pcipath = Path::new(vec![sf3_0]).unwrap();
        assert_eq!(format!("{}", pcipath), "03.0");
        let pcipath2 = Path::from_str("03.0").unwrap();
        assert_eq!(pcipath, pcipath2);
        let pcipath2 = Path::from_str("03").unwrap();
        assert_eq!(pcipath, pcipath2);
        assert_eq!(pcipath.len(), 1);
        assert_eq!(pcipath[0], sf3_0);

        let pcipath = Path::new(vec![sf3_0, sf4_0]).unwrap();
        assert_eq!(format!("{}", pcipath), "03.0/04.0");
        let pcipath2 = Path::from_str("03.0/04.0").unwrap();
        assert_eq!(pcipath, pcipath2);
        let pcipath2 = Path::from_str("03/04").unwrap();
        assert_eq!(pcipath, pcipath2);
        assert_eq!(pcipath.len(), 2);
        assert_eq!(pcipath[0], sf3_0);
        assert_eq!(pcipath[1], sf4_0);

        let pcipath = Path::new(vec![sf3_0, sf4_0, sf5_0]).unwrap();
        assert_eq!(format!("{}", pcipath), "03.0/04.0/05.0");
        let pcipath2 = Path::from_str("03.0/04.0/05.0").unwrap();
        assert_eq!(pcipath, pcipath2);
        let pcipath2 = Path::from_str("03/04/05").unwrap();
        assert_eq!(pcipath, pcipath2);
        assert_eq!(pcipath.len(), 3);
        assert_eq!(pcipath[0], sf3_0);
        assert_eq!(pcipath[1], sf4_0);
        assert_eq!(pcipath[2], sf5_0);

        let pcipath = Path::new(vec![sfa_5, sfb_6, sfc_7]).unwrap();
        assert_eq!(format!("{}", pcipath), "0a.5/0b.6/0c.7");
        let pcipath2 = Path::from_str("0a.5/0b.6/0c.7").unwrap();
        assert_eq!(pcipath, pcipath2);
        assert_eq!(pcipath.len(), 3);
        assert_eq!(pcipath[0], sfa_5);
        assert_eq!(pcipath[1], sfb_6);
        assert_eq!(pcipath[2], sfc_7);

        // Bad paths
        assert!(Path::new(vec!()).is_err());
        assert!(Path::from_str("20").is_err());
        assert!(Path::from_str("00.8").is_err());
        assert!(Path::from_str("//").is_err());
        assert!(Path::from_str("xyz").is_err());
    }
}
