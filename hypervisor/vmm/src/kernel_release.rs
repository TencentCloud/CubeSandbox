// Copyright © 2026 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0

//! Host kernel release (`uname -r`) and the pagemap bit-61 version gate.
//!
//! File-PMD `PM_FILE` reporting was fixed in upstream `3f9f022` (6.6.44 on
//! the 6.6 stable line, about 6.11 on mainline). A naive
//! `(major, minor, patch) >= (6, 6, 44)` compare would treat 6.7–6.10 as
//! new enough; those trees still omit `PM_FILE` on file PMDs.

use log::info;
use once_cell::sync::Lazy;
use std::fmt;

/// Parsed leading `major.minor.patch` from a `uname -r` string.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct KernelRelease {
    pub major: u32,
    pub minor: u32,
    pub patch: u32,
}

impl KernelRelease {
    /// Parse leading `major.minor[.patch]` from a `uname -r` string.
    ///
    /// Distro suffixes are ignored (`6.6.44-1.el9` → `6.6.44`). A missing
    /// patch is `0` (`6.6` → `6.6.0`). Returns `None` when the string does
    /// not start with at least `major.minor`.
    pub(crate) fn parse(release: &str) -> Option<Self> {
        let mut nums = [0u32; 3];
        let mut count = 0usize;
        let bytes = release.as_bytes();
        let mut i = 0usize;
        while count < 3 {
            if i >= bytes.len() || !bytes[i].is_ascii_digit() {
                break;
            }
            let start = i;
            while i < bytes.len() && bytes[i].is_ascii_digit() {
                i += 1;
            }
            nums[count] = release[start..i].parse().ok()?;
            count += 1;
            if i < bytes.len() && bytes[i] == b'.' {
                i += 1;
                continue;
            }
            break;
        }
        (count >= 2).then_some(Self {
            major: nums[0],
            minor: nums[1],
            patch: nums[2],
        })
    }

    /// Host kernel release string (`uname -r`).
    ///
    /// Prefers `/proc/sys/kernel/osrelease` so the gate does not depend on the
    /// `libc` `utsname` layout. Falls back to `uname(2)`. Empty if both fail.
    pub(crate) fn uname_string() -> String {
        if let Ok(s) = std::fs::read_to_string("/proc/sys/kernel/osrelease") {
            let s = s.trim();
            if !s.is_empty() {
                return s.to_string();
            }
        }
        let mut uts = std::mem::MaybeUninit::<libc::utsname>::uninit();
        // SAFETY: `uname` writes a complete `utsname` on success.
        if unsafe { libc::uname(uts.as_mut_ptr()) } != 0 {
            return String::new();
        }
        // SAFETY: `uname` succeeded, so `uts` is initialized and `release` is
        // a NUL-terminated kernel string.
        let uts = unsafe { uts.assume_init() };
        let release = unsafe { std::ffi::CStr::from_ptr(uts.release.as_ptr()) };
        release.to_string_lossy().into_owned()
    }

    /// Whether this kernel is known to set `PM_FILE` on file PMDs.
    pub(crate) fn supports_pm_file_pmd(self) -> bool {
        match (self.major, self.minor, self.patch) {
            (6, 6, patch) => patch >= 44,
            (6, minor, _) => minor >= 11,
            (major, _, _) => major >= 7,
        }
    }
}

impl fmt::Display for KernelRelease {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}.{}.{}", self.major, self.minor, self.patch)
    }
}

/// Cached scan-path decision for `pagemap_anon`.
///
/// Bit 61 (`PM_FILE`) is only used when the kernel is known to set it on
/// file PMDs. Everything else, including an unparseable release, uses
/// `kpageflags`.
pub(crate) struct PagemapScanPath {
    use_bit61: bool,
}

impl PagemapScanPath {
    fn detect() -> Self {
        let release = KernelRelease::uname_string();
        let use_bit61 =
            KernelRelease::parse(&release).is_some_and(KernelRelease::supports_pm_file_pmd);
        info!(
            "pagemap_anon: kernel={} path={}",
            if release.is_empty() {
                "unknown"
            } else {
                release.as_str()
            },
            if use_bit61 { "bit61" } else { "kpageflags" }
        );
        Self { use_bit61 }
    }

    /// Host decision, probed once from `uname`.
    pub(crate) fn cached() -> &'static Self {
        static PATH: Lazy<PagemapScanPath> = Lazy::new(PagemapScanPath::detect);
        &PATH
    }

    pub(crate) fn use_bit61(&self) -> bool {
        self.use_bit61
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_uname_release_strings() {
        assert_eq!(
            KernelRelease::parse("6.6.44"),
            Some(KernelRelease {
                major: 6,
                minor: 6,
                patch: 44
            })
        );
        assert_eq!(
            KernelRelease::parse("6.6.44-1.el9"),
            Some(KernelRelease {
                major: 6,
                minor: 6,
                patch: 44
            })
        );
        assert_eq!(
            KernelRelease::parse("6.6"),
            Some(KernelRelease {
                major: 6,
                minor: 6,
                patch: 0
            })
        );
        assert_eq!(
            KernelRelease::parse("7.0.0-28-generic"),
            Some(KernelRelease {
                major: 7,
                minor: 0,
                patch: 0
            })
        );
        assert_eq!(
            KernelRelease::parse("5.15.0-91-generic"),
            Some(KernelRelease {
                major: 5,
                minor: 15,
                patch: 0
            })
        );
        assert_eq!(
            KernelRelease::parse("6.6.69-opencloudos9.cubesandbox.pvm.host-gb85200d80fa2"),
            Some(KernelRelease {
                major: 6,
                minor: 6,
                patch: 69
            })
        );
        assert_eq!(KernelRelease::parse(""), None);
        assert_eq!(KernelRelease::parse("abc"), None);
        assert_eq!(KernelRelease::parse("6"), None);
        assert_eq!(KernelRelease::parse("linux-6.6.44"), None);
    }

    #[test]
    fn pm_file_pmd_gate() {
        let cases = [
            ("6.6.43", false),
            ("6.6.44", true),
            ("6.6.45", true),
            (
                "6.6.69-opencloudos9.cubesandbox.pvm.host-gb85200d80fa2",
                true,
            ),
            ("6.6.44-1.el9", true),
            ("6.6", false),
            ("6.7.0", false),
            ("6.10.0", false),
            ("6.11.0", true),
            ("6.12.1", true),
            ("7.0.0-28-generic", true),
            ("5.15.0", false),
            ("", false),
            ("not-a-version", false),
        ];
        for (release, expected) in cases {
            let got =
                KernelRelease::parse(release).is_some_and(KernelRelease::supports_pm_file_pmd);
            assert_eq!(got, expected, "release={release}");
        }
    }
}
