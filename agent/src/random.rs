// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{ensure, Result};
use nix::errno::Errno;
use nix::fcntl::{self, OFlag};
use nix::sys::stat::Mode;
use std::fs;
use std::os::unix::io::{AsRawFd, FromRawFd};
use tracing::instrument;

pub const RNGDEV: &str = "/dev/random";
pub const RNDADDTOENTCNT: libc::c_int = 0x40045201;
pub const RNDRESEEDCRNG: libc::c_int = 0x5207;

// Handle the differing ioctl(2) request types for different targets
#[cfg(target_env = "musl")]
type IoctlRequestType = libc::c_int;
#[cfg(target_env = "gnu")]
type IoctlRequestType = libc::c_ulong;

#[instrument]
pub fn reseed_rng(data: &[u8]) -> Result<()> {
    let len = data.len() as libc::c_long;

    ensure!(len > 0, "missing entropy data");

    fs::write(RNGDEV, data)?;

    let f = {
        let fd = fcntl::open(RNGDEV, OFlag::O_RDWR, Mode::from_bits_truncate(0o022))?;
        // Wrap fd with `File` to properly close descriptor on exit
        unsafe { fs::File::from_raw_fd(fd) }
    };

    let ret = unsafe {
        libc::ioctl(
            f.as_raw_fd(),
            RNDADDTOENTCNT as IoctlRequestType,
            &len as *const libc::c_long,
        )
    };
    Errno::result(ret).map(drop)?;

    let ret = unsafe { libc::ioctl(f.as_raw_fd(), RNDRESEEDCRNG as IoctlRequestType, 0) };
    Errno::result(ret).map(drop)?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{skip_if_no_cap, skip_if_not_root};
    use capctl::caps::Cap;
    use std::fs::File;
    use std::io::prelude::*;

    #[test]
    fn test_reseed_rng() {
        skip_if_not_root!();
        // The RNDADDTOENTCNT/RNDRESEEDCRNG ioctls require CAP_SYS_ADMIN, which
        // root may lack inside a container.
        skip_if_no_cap!(Cap::SYS_ADMIN);
        const POOL_SIZE: usize = 512;
        let mut f = File::open("/dev/urandom").unwrap();
        let mut seed = [0; POOL_SIZE];
        let n = f.read(&mut seed).unwrap();
        // Ensure the buffer was filled.
        assert!(n == POOL_SIZE);
        let ret = reseed_rng(&seed);
        assert!(ret.is_ok());
    }

    #[test]
    fn test_reseed_rng_not_root() {
        const POOL_SIZE: usize = 512;
        let mut f = File::open("/dev/urandom").unwrap();
        let mut seed = [0; POOL_SIZE];
        let n = f.read(&mut seed).unwrap();
        // Ensure the buffer was filled.
        assert!(n == POOL_SIZE);
        let ret = reseed_rng(&seed);
        // Reseeding succeeds only with CAP_SYS_ADMIN; being uid 0 is not enough
        // when the capability has been dropped (e.g. inside a container).
        if crate::test_utils::test_utils::have_effective_cap(Cap::SYS_ADMIN) {
            assert!(ret.is_ok());
        } else {
            assert!(ret.is_err());
        }
    }

    #[test]
    fn test_reseed_rng_zero_data() {
        let seed = [];
        let ret = reseed_rng(&seed);
        assert!(ret.is_err());
    }
}
