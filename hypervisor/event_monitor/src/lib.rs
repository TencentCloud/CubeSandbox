// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use serde::Serialize;
use std::borrow::Cow;
use std::collections::HashMap;
use std::fs::File;
use std::io::Write;
use std::os::unix::io::AsRawFd;
use std::sync::Mutex;
use std::time::{Duration, Instant};

static mut MONITOR: Option<Mutex<(File, Instant)>> = None;

/// This function must only be called once from the main process before any threads
/// are created to avoid race conditions
pub fn set_monitor(file: File) -> Result<(), std::io::Error> {
    assert!(unsafe { MONITOR.is_none() });
    let fd = file.as_raw_fd();
    let ret = unsafe {
        let mut flags = libc::fcntl(fd, libc::F_GETFL);
        flags |= libc::O_NONBLOCK;
        libc::fcntl(fd, libc::F_SETFL, flags)
    };
    if ret < 0 {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: MONITOR is None. Nobody else can hold a reference to it.
    unsafe {
        MONITOR = Some(Mutex::new((file, Instant::now())));
    };
    Ok(())
}

#[derive(Serialize)]
struct Event<'a> {
    timestamp: Duration,
    source: &'a str,
    event: &'a str,
    properties: Option<&'a HashMap<Cow<'a, str>, Cow<'a, str>>>,
}

pub fn event_log(source: &str, event: &str, properties: Option<&HashMap<Cow<str>, Cow<str>>>) {
    // SAFETY: MONITOR is always in a valid state (None or Some).
    if let Some(mutex) = unsafe { MONITOR.as_ref() } {
        let mut guard = mutex.lock().unwrap();
        let e = Event {
            timestamp: guard.1.elapsed(),
            source,
            event,
            properties,
        };
        serde_json::to_writer_pretty(&guard.0, &e).ok();

        guard.0.write_all(b"\n\n").ok();
    }
}

/*
    Through the use of Cow<'a, str> it is possible to use String as well as
    &str as the parameters:
    e.g.
    event!("cpu_manager", "create_vcpu", "id", cpu_id.to_string());
*/
#[macro_export]
macro_rules! event {
    ($source:expr, $event:expr) => {
        $crate::event_log($source, $event, None)
    };
    ($source:expr, $event:expr, $($key:expr, $value:expr),*) => {
        {
            let mut properties = ::std::collections::HashMap::new();
            $(
                properties.insert($key.into(), $value.into());
            )+
            $crate::event_log($source, $event, Some(&properties))
        }
     };

}
