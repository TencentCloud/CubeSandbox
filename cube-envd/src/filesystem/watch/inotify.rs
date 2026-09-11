// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! The kernel boundary: inotify FFI + event-buffer parsing.
//!
//! Carries over the FFI part of `services/watch.rs`: the raw `libc::inotify_*`
//! calls and the `read()` buffer parser. This layer depends on no other module
//! in this directory, so it can be unit-tested in isolation.

use std::os::unix::io::{AsRawFd, RawFd};
use std::path::Path;

use crate::compat::vocab::errno_text;
use crate::protocol::{ConnectCode, ConnectError};

/// The exact mask fsnotify requests for its default op set
/// Create|Write|Remove|Rename|Chmod (`fsnotify.go:424-426` expanding through
/// `backend_inotify.go:195-223`): nothing more, nothing less — no IN_ONLYDIR,
/// no IN_MASK_ADD.
const WATCH_MASK: u32 = libc::IN_CREATE
    | libc::IN_MODIFY
    | libc::IN_DELETE
    | libc::IN_DELETE_SELF
    | libc::IN_MOVED_TO
    | libc::IN_MOVED_FROM
    | libc::IN_MOVE_SELF
    | libc::IN_ATTRIB;

// ---------- inotify (raw libc — no inotify crate, see plan) ----------

pub(super) struct Inotify {
    fd: RawFd,
}

impl Inotify {
    /// `inotify_init1(IN_NONBLOCK|IN_CLOEXEC)`: non-blocking so the pump can
    /// select between readability, keepalive ticks and client disconnect
    /// (upstream relies on `ctx.Done()` responsiveness, `watch.go:89-90`).
    pub(super) fn new() -> Result<Self, ConnectError> {
        // SAFETY: plain syscall, no pointers.
        let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
        if fd < 0 {
            let e = std::io::Error::last_os_error();
            return Err(ConnectError::new(
                ConnectCode::Internal,
                format!("error creating watcher: inotify_init1: {}", errno_text(&e)),
            ));
        }
        Ok(Self { fd })
    }

    pub(super) fn read(&self, buf: &mut [u8]) -> std::io::Result<usize> {
        // SAFETY: buf is valid for writes of buf.len().
        let n = unsafe { libc::read(self.fd, buf.as_mut_ptr().cast(), buf.len()) };
        if n < 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
}

impl AsRawFd for Inotify {
    fn as_raw_fd(&self) -> RawFd {
        self.fd
    }
}

impl Drop for Inotify {
    fn drop(&mut self) {
        // SAFETY: fd is owned.
        unsafe { libc::close(self.fd) };
    }
}

/// `inotify_add_watch` on a raw fd (free function so `WatchState` can add
/// watches while the `Inotify` itself lives inside an `AsyncFd`).
pub(super) fn add_watch_raw(fd: RawFd, path: &Path) -> std::io::Result<i32> {
    let c = std::ffi::CString::new(path.as_os_str().as_encoded_bytes())?;
    // SAFETY: c outlives the call; fd is owned.
    let wd = unsafe { libc::inotify_add_watch(fd, c.as_ptr(), WATCH_MASK) };
    if wd < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(wd)
    }
}

pub(super) fn rm_watch_raw(fd: RawFd, wd: i32) {
    // SAFETY: fd is owned; wd was returned by add_watch on it.
    unsafe { libc::inotify_rm_watch(fd, wd) };
}

/// One parsed `inotify_event`. `name` is None for events on the watched
/// directory itself (kernel sends no name).
pub(super) struct RawEvent {
    pub(super) wd: i32,
    pub(super) mask: u32,
    pub(super) cookie: u32,
    pub(super) name: Option<String>,
}

/// Parse a read() buffer into events. The kernel only ever returns whole
/// events per read, but headers are 4-byte aligned while names vary in
/// length, so every header read must be unaligned-safe.
pub(super) fn parse_events(buf: &[u8]) -> Vec<RawEvent> {
    const HDR: usize = std::mem::size_of::<libc::inotify_event>();
    let mut out = Vec::new();
    let mut off = 0usize;
    while off + HDR <= buf.len() {
        // SAFETY: off+HDR <= buf.len(); unaligned by design (see above).
        let ev = unsafe {
            std::ptr::read_unaligned(buf.as_ptr().add(off) as *const libc::inotify_event)
        };
        let name_len = ev.len as usize;
        let name = if name_len > 0 && off + HDR + name_len <= buf.len() {
            let bytes = &buf[off + HDR..off + HDR + name_len];
            let end = bytes.iter().position(|&b| b == 0).unwrap_or(bytes.len());
            let s = String::from_utf8_lossy(&bytes[..end]).into_owned();
            (!s.is_empty()).then_some(s)
        } else {
            None
        };
        out.push(RawEvent {
            wd: ev.wd,
            mask: ev.mask,
            cookie: ev.cookie,
            name,
        });
        // Advance by header + name. A zero name_len is a NORMAL event (the
        // watched directory itself: IN_ATTRIB / IN_DELETE_SELF / ...) — the
        // only stop condition is a truncated tail, which the kernel never
        // produces for a single read (it returns whole events only).
        let total = HDR + name_len;
        if off + total > buf.len() {
            break;
        }
        off += total;
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- parse_events ----

    #[test]
    fn parse_roundtrips_named_and_anonymous_events() {
        let mut buf: Vec<u8> = Vec::new();
        let push = |buf: &mut Vec<u8>, wd: i32, mask: u32, cookie: u32, name: &str| {
            buf.extend_from_slice(&wd.to_ne_bytes());
            buf.extend_from_slice(&mask.to_ne_bytes());
            buf.extend_from_slice(&cookie.to_ne_bytes());
            let name_bytes = name.as_bytes();
            let len = name_bytes.len() + 1; // NUL-terminated, kernel-padded
            buf.extend_from_slice(&(len as u32).to_ne_bytes());
            buf.extend_from_slice(name_bytes);
            buf.push(0);
        };
        push(&mut buf, 3, libc::IN_CREATE, 0, "hello.txt");
        push(&mut buf, 5, libc::IN_DELETE_SELF, 0, "");

        let evs = parse_events(&buf);
        assert_eq!(evs.len(), 2);
        assert_eq!(evs[0].wd, 3);
        assert_eq!(evs[0].mask, libc::IN_CREATE);
        assert_eq!(evs[0].name.as_deref(), Some("hello.txt"));
        assert_eq!(evs[1].wd, 5);
        assert_eq!(evs[1].name, None);
    }

    /// Regression: an anonymous event (name_len == 0 — the watched directory
    /// itself) must NOT truncate the rest of the batch. A read commonly
    /// carries [ATTRIB-on-dir, CREATE-on-entry, …].
    #[test]
    fn anonymous_event_does_not_truncate_batch() {
        let mut buf: Vec<u8> = Vec::new();
        let push = |buf: &mut Vec<u8>, wd: i32, mask: u32, name: &str| {
            buf.extend_from_slice(&wd.to_ne_bytes());
            buf.extend_from_slice(&mask.to_ne_bytes());
            buf.extend_from_slice(&0u32.to_ne_bytes());
            let name_bytes = name.as_bytes();
            buf.extend_from_slice(&((name_bytes.len() + 1) as u32).to_ne_bytes());
            buf.extend_from_slice(name_bytes);
            buf.push(0);
        };
        push(&mut buf, 3, libc::IN_ATTRIB, ""); // anonymous: dir itself
        push(&mut buf, 3, libc::IN_CREATE, "after.txt");
        let evs = parse_events(&buf);
        assert_eq!(evs.len(), 2, "anonymous event must not stop parsing");
        assert_eq!(evs[0].name, None);
        assert_eq!(evs[1].name.as_deref(), Some("after.txt"));
    }
}
