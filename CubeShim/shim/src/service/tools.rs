// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use containerd_shim::Error;
use nix::sys::signal::{self, Signal};
use nix::unistd::Pid;
use std::fs;
use std::path::Path;
use std::result::Result;

pub fn read_number_from_file(file: &str) -> Result<i32, Error> {
    let pfile = Path::new(file);
    let num = match fs::read_to_string(pfile) {
        Ok(content) => {
            let n = match content.trim().parse::<i32>() {
                Ok(n) => n,
                Err(e) => {
                    return Err(Error::ParseInt(e));
                }
            };
            n
        }
        Err(e) => {
            return Err(Error::IoError {
                context: format!("read file[{}] failed", pfile.display()),
                err: e,
            });
        }
    };
    Ok(num)
}

pub fn read_address(file: &str) -> Result<String, Error> {
    let pfile = Path::new(file);
    let sk_file = match fs::read_to_string(pfile) {
        Ok(content) => {
            let sk_file = content.strip_prefix("unix://");
            if sk_file.is_none() {
                return Err(Error::InvalidArgument(format!(
                    "read address failed:{}",
                    content
                )));
            }
            sk_file.unwrap().to_string()
        }
        Err(e) => {
            return Err(Error::IoError {
                context: format!("read file[{}] failed", pfile.display()),
                err: e,
            });
        }
    };
    Ok(sk_file)
}

pub fn signal(pid: i32, sig: Option<Signal>) -> Result<(), Error> {
    let p = Pid::from_raw(pid);
    if sig.is_none() {
        signal::kill(p, sig)
            .map_err(|e| Error::Other(format!("signal 0 to {} failed:{}", pid, e)))?;
    } else {
        signal::kill(p, sig).map_err(|e| {
            Error::Other(format!("signal {} to {} failed:{}", sig.unwrap(), pid, e))
        })?;
    }
    Ok(())
}
