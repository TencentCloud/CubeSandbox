// Copyright © 2023 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use clap::{Arg, ArgAction, ArgMatches, Command};
#[cfg(all(feature = "kvm", target_arch = "x86_64"))]
use hypervisor::kvm::kvm_bindings;
#[cfg(all(feature = "kvm", target_arch = "x86_64"))]
use hypervisor::kvm::kvm_ioctls::Kvm;
use serde::de::Error as SerdeError;
use serde::{Deserialize, Deserializer, Serializer};
use std::fs::File;
use std::io::Write;
use std::path::Path;
use std::process;

#[cfg(all(feature = "kvm", target_arch = "x86_64"))]
pub use {kvm_bindings::kvm_cpuid_entry2, kvm_bindings::KVM_CPUID_FLAG_SIGNIFCANT_INDEX};

pub const CPUID_FLAG_VALID_INDEX: u32 = 1;

#[derive(Debug)]
enum Error {
    //    Connect(std::io::Error),
    //    ReadingStdin(std::io::Error),
    //    ReadingFile(std::io::Error),
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
    #[serde(
        deserialize_with = "deserialize_from_str_u32",
        serialize_with = "serialize_to_hex_str"
    )]
    pub function: u32,
    #[serde(
        deserialize_with = "deserialize_from_str_u32",
        serialize_with = "serialize_to_hex_str"
    )]
    pub index: u32,
    pub flags: u32,
    #[serde(
        deserialize_with = "deserialize_from_str_u32",
        serialize_with = "serialize_to_hex_str"
    )]
    pub eax: u32,
    #[serde(
        deserialize_with = "deserialize_from_str_u32",
        serialize_with = "serialize_to_hex_str"
    )]
    pub ebx: u32,
    #[serde(
        deserialize_with = "deserialize_from_str_u32",
        serialize_with = "serialize_to_hex_str"
    )]
    pub ecx: u32,
    #[serde(
        deserialize_with = "deserialize_from_str_u32",
        serialize_with = "serialize_to_hex_str"
    )]
    pub edx: u32,
}

#[cfg(all(feature = "kvm", target_arch = "x86_64"))]
impl From<CpuIdEntry> for kvm_cpuid_entry2 {
    fn from(e: CpuIdEntry) -> Self {
        let flags = if e.flags & CPUID_FLAG_VALID_INDEX != 0 {
            KVM_CPUID_FLAG_SIGNIFCANT_INDEX
        } else {
            0
        };
        Self {
            function: e.function,
            index: e.index,
            flags,
            eax: e.eax,
            ebx: e.ebx,
            ecx: e.ecx,
            edx: e.edx,
            ..Default::default()
        }
    }
}

#[cfg(all(feature = "kvm", target_arch = "x86_64"))]
impl From<kvm_cpuid_entry2> for CpuIdEntry {
    fn from(e: kvm_cpuid_entry2) -> Self {
        let flags = if e.flags & KVM_CPUID_FLAG_SIGNIFCANT_INDEX != 0 {
            CPUID_FLAG_VALID_INDEX
        } else {
            0
        };
        Self {
            function: e.function,
            index: e.index,
            flags,
            eax: e.eax,
            ebx: e.ebx,
            ecx: e.ecx,
            edx: e.edx,
        }
    }
}

pub fn intersect_cpuid(cpuid: &mut [CpuIdEntry], patches: Vec<CpuIdEntry>) {
    for entry in cpuid {
        for patch in patches.iter() {
            if entry.function == patch.function && entry.index == patch.index {
                entry.eax &= patch.eax;
                entry.ebx &= patch.ebx;
                entry.ecx &= patch.ecx;
                entry.edx &= patch.edx;
            }
        }
    }
}

fn get_cpuid_from_file(file: &str) -> Vec<CpuIdEntry> {
    serde_json::from_str(&std::fs::read_to_string(Path::new(file)).unwrap()).unwrap()
}

#[cfg(all(feature = "kvm", target_arch = "x86_64"))]
fn get_local_host_cpuid() -> Vec<CpuIdEntry> {
    let kvm = Kvm::new().unwrap();
    let kvm_cpuid = kvm
        .get_supported_cpuid(kvm_bindings::KVM_MAX_CPUID_ENTRIES)
        .unwrap();

    let v = kvm_cpuid.as_slice().iter().map(|e| (*e).into()).collect();

    let mut f = File::create(Path::new("cpuid.json")).unwrap();
    f.write_all(serde_json::to_string_pretty(&v).unwrap().as_bytes())
        .unwrap();

    v
}

fn intersect_json_files(files: Vec<&String>) {
    let mut compatible_cpuid = get_cpuid_from_file(files[0]);

    for f in files.iter() {
        let cpuid = get_cpuid_from_file(f);

        intersect_cpuid(&mut compatible_cpuid, cpuid);
    }

    println!("{:x?}", compatible_cpuid);
    let mut compatible_file = File::create(Path::new("compatible.json")).unwrap();
    compatible_file
        .write_all(
            serde_json::to_string_pretty(&compatible_cpuid)
                .unwrap()
                .as_bytes(),
        )
        .unwrap();
}

fn do_command(matches: &ArgMatches) -> Result<(), Error> {
    match matches.subcommand_name() {
        Some("get") => {
            println!("Get cpuid from host machine to cpuid.json");
            #[cfg(all(feature = "kvm", target_arch = "x86_64"))]
            let _ = get_local_host_cpuid();
        }
        Some("intersect") => {
            let files: Vec<_> = matches
                .subcommand_matches("intersect")
                .unwrap()
                .get_many::<String>("file")
                .unwrap()
                .collect();
            intersect_json_files(files);
        }
        None => unreachable!(),
        Some(&_) => todo!(),
    }

    Ok(())
}

fn main() {
    let app = Command::new("cube-cpuid")
        .subcommand_required(true)
        .about("Get or Intersect of cpuid")
        .subcommand(Command::new("get").about("Get local machine cpuid"))
        .subcommand(
            Command::new("intersect")
                .about("intersect cpuid json files")
                .arg(
                    Arg::new("file")
                        .action(ArgAction::Append)
                        .num_args(2..)
                        .short('F')
                        .help("<cpuid json files>"),
                ),
        );

    let matches = app.get_matches();

    if do_command(&matches).is_err() {
        println!("Error running command");
        process::exit(1)
    };
}
