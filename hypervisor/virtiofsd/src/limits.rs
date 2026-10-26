// SPDX-License-Identifier: BSD-3-Clause

use libc::{getrlimit, rlim_t, rlimit, setrlimit, RLIMIT_NOFILE};
use std::mem::MaybeUninit;
use std::{cmp, fs, io};

// Default number of open files (RLIMIT_NOFILE)
const DEFAULT_NOFILE: rlim_t = 1_000_000;

/// Gets the maximum number of open files.
fn get_max_nofile() -> Result<rlim_t, String> {
    let path = "/proc/sys/fs/nr_open";
    let max_str = fs::read_to_string(path).map_err(|error| format!("Reading {path}: {error:?}"))?;

    max_str
        .trim()
        .parse()
        .map_err(|error| format!("Parsing {path}: {error:?}"))
}

/// Gets the hard limit of open files.
fn get_nofile_limits() -> Result<rlimit, String> {
    let mut limits = MaybeUninit::<rlimit>::zeroed();
    let ret = unsafe { getrlimit(RLIMIT_NOFILE, limits.as_mut_ptr()) };
    if ret != 0 {
        return Err(format!("getrlimit: {}", io::Error::last_os_error()));
    }

    Ok(unsafe { limits.assume_init() })
}

/// Sets the limit of open files to the given value.
fn setup_rlimit_nofile_to(nofile: rlim_t) -> Result<(), String> {
    let rlimit = rlimit {
        rlim_cur: nofile,
        rlim_max: nofile,
    };
    let ret = unsafe { setrlimit(RLIMIT_NOFILE, &rlimit) };
    if ret < 0 {
        Err(format!(
            "Failed to increase the limit: {:?}",
            io::Error::last_os_error()
        ))
    } else {
        Ok(())
    }
}

pub fn setup_rlimit_nofile(nofile: Option<u64>) -> Result<(), String> {
    let max_nofile = get_max_nofile()?;
    let rlimit { rlim_cur, rlim_max } = get_nofile_limits()?;

    let target_limit = if let Some(nofile) = nofile {
        if nofile == 0 {
            return Ok(()); // '--rlimit-nofile=0' leaves the resource limit unchanged
        }
        nofile
    } else {
        if DEFAULT_NOFILE <= rlim_cur {
            return Ok(()); // the user has already setup the soft limit higher than the target
        }
        cmp::min(DEFAULT_NOFILE, max_nofile)
    };

    if target_limit > max_nofile {
        return Err(format!("It cannot be increased above {max_nofile}"));
    }

    if let Err(error) = setup_rlimit_nofile_to(target_limit) {
        if nofile.is_some() {
            // Error attempting to setup user-supplied value
            return Err(error);
        } else {
            warn!(
                "Failure when trying to set the limit to {}, \
                the hard limit ({}) of open file descriptors is used instead.",
                target_limit, rlim_max
            );
            setup_rlimit_nofile_to(rlim_max).map_err(|error| {
                format!("Cannot increase the soft limit to the hard limit: {error}")
            })?
        }
    }

    Ok(())
}
