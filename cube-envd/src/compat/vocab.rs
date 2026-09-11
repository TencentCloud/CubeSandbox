// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Go-syscall errno text table — the contract for byte-identical error
//! messages against the Go envd baseline.
//!
//! Why this exists: Go's `syscall.Errno.Error()` uses its own **lowercase**
//! table, while Rust's `io::Error` Display goes through `strerror`, which
//! capitalizes ("Not a directory" vs Go's "not a directory"). That is a
//! systematic per-errno divergence; the conformance harness only exercises
//! the ENOENT path (which used to be hardcoded), so every other errno
//! drifted unnoticed.
//!
//! Source of truth: `/usr/local/go/src/syscall/zerrors_linux_amd64.go`
//! (linux/amd64, go1.26.5). Every entry below was extracted verbatim from
//! that table. Measured examples of the target shapes:
//!   `stat /etc/hosts/x: not a directory`        (os.Stat, ENOTDIR)
//!   `lstat /nope/x/y: no such file or directory` (os.Lstat, ENOENT)
//!   `rename /a /b: no such file or directory`    (os.Rename, *LinkError)
//!   `mkdir /etc/hosts/z: not a directory`        (os.Mkdir, ENOTDIR)
//!   `readdirent {dir}: permission denied`        (os.ReadDir, dir_unix.go:89)

/// Map a Linux errno number to Go's exact error text (lowercase).
pub fn go_errno_text(errno: i32) -> Option<&'static str> {
    let text = match errno {
        libc::EPERM => "operation not permitted",
        libc::ENOENT => "no such file or directory",
        libc::EACCES => "permission denied",
        libc::EBUSY => "device or resource busy",
        libc::EEXIST => "file exists",
        libc::EXDEV => "invalid cross-device link",
        libc::ENOTDIR => "not a directory",
        libc::EISDIR => "is a directory",
        libc::EINVAL => "invalid argument",
        libc::ENFILE => "too many open files in system",
        libc::EMFILE => "too many open files",
        libc::ENOSPC => "no space left on device",
        libc::EROFS => "read-only file system",
        libc::EMLINK => "too many links",
        libc::ENAMETOOLONG => "file name too long",
        libc::ENOTEMPTY => "directory not empty",
        libc::ELOOP => "too many levels of symbolic links",
        libc::EOVERFLOW => "value too large for defined data type",
        libc::ESTALE => "stale file handle",
        _ => return None,
    };
    Some(text)
}

/// The Go text for `err`, falling back to `strerror` (Rust's `Display`) when
/// the table has no entry. The fallback is intentionally loud in debug builds:
/// a capitalized string in an untested branch beats silent drift.
pub fn errno_text(err: &std::io::Error) -> String {
    match err.raw_os_error().and_then(go_errno_text) {
        Some(text) => text.to_string(),
        None => {
            #[cfg(debug_assertions)]
            eprintln!(
                "compat::vocab: no Go text for errno {:?} ({}), falling back to strerror",
                err.raw_os_error(),
                err
            );
            err.to_string()
        }
    }
}

/// Go `*os.PathError`: `"{op} {path}: {errno text}"`.
pub fn go_path_error(op: &str, path: &str, err: &std::io::Error) -> String {
    format!("{op} {path}: {}", errno_text(err))
}

/// Go `*os.LinkError` (rename): `"{op} {old} {new}: {errno text}"`.
pub fn go_link_error(op: &str, old: &str, new: &str, err: &std::io::Error) -> String {
    format!("{op} {old} {new}: {}", errno_text(err))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every table entry must match go1.26.5's `zerrors_linux_amd64.go`
    /// verbatim (verified empirically on this machine; see the plan doc).
    #[test]
    fn go_errno_texts_match_go_baseline() {
        let cases = [
            (libc::EPERM, "operation not permitted"),
            (libc::ENOENT, "no such file or directory"),
            (libc::EACCES, "permission denied"),
            (libc::EBUSY, "device or resource busy"),
            (libc::EEXIST, "file exists"),
            (libc::EXDEV, "invalid cross-device link"),
            (libc::ENOTDIR, "not a directory"),
            (libc::EISDIR, "is a directory"),
            (libc::EINVAL, "invalid argument"),
            (libc::ENFILE, "too many open files in system"),
            (libc::EMFILE, "too many open files"),
            (libc::ENOSPC, "no space left on device"),
            (libc::EROFS, "read-only file system"),
            (libc::EMLINK, "too many links"),
            (libc::ENAMETOOLONG, "file name too long"),
            (libc::ENOTEMPTY, "directory not empty"),
            (libc::ELOOP, "too many levels of symbolic links"),
            (libc::EOVERFLOW, "value too large for defined data type"),
            (libc::ESTALE, "stale file handle"),
        ];
        for (errno, text) in cases {
            assert_eq!(go_errno_text(errno), Some(text), "errno {errno}");
        }
    }

    #[test]
    fn path_error_renders_go_shape() {
        let err = std::io::Error::from_raw_os_error(libc::ENOTDIR);
        assert_eq!(
            go_path_error("stat", "/etc/hosts/x", &err),
            "stat /etc/hosts/x: not a directory"
        );
    }

    #[test]
    fn link_error_renders_go_shape() {
        let err = std::io::Error::from_raw_os_error(libc::ENOENT);
        assert_eq!(
            go_link_error("rename", "/nope/a", "/tmp/b", &err),
            "rename /nope/a /tmp/b: no such file or directory"
        );
    }
}
