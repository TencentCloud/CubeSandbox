// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::{
    epoll_helper::EpollHelperError,
    seccomp_filters::{get_seccomp_filter, Thread},
    ActivateError,
};
use seccompiler::{apply_filter, SeccompAction};
use std::{
    panic::AssertUnwindSafe,
    thread::{self, JoinHandle},
};
use vmm_sys_util::eventfd::EventFd;

pub(crate) fn spawn_virtio_thread<F>(
    name: &str,
    seccomp_action: &SeccompAction,
    thread_type: Thread,
    epoll_threads: &mut Vec<JoinHandle<()>>,
    exit_evt: &EventFd,
    f: F,
) -> Result<(), ActivateError>
where
    F: FnOnce() -> std::result::Result<(), EpollHelperError>,
    F: Send + 'static,
{
    let seccomp_filter = get_seccomp_filter(seccomp_action, thread_type)
        .map_err(ActivateError::CreateSeccompFilter)?;

    let thread_exit_evt = exit_evt
        .try_clone()
        .map_err(ActivateError::CloneExitEventFd)?;
    let thread_name = name.to_string();

    thread::Builder::new()
        .name(name.to_string())
        .spawn(move || {
            if !seccomp_filter.is_empty() {
                if let Err(e) = apply_filter(&seccomp_filter) {
                    error!("Error applying seccomp filter: {:?}", e);
                    thread_exit_evt.write(1).ok();
                    return;
                }
            }
            match std::panic::catch_unwind(AssertUnwindSafe(f)) {
                Err(_) => {
                    error!("{} thread panicked", thread_name);
                    thread_exit_evt.write(1).ok();
                }
                Ok(r) => {
                    if let Err(e) = r {
                        error!("Error running worker: {:?}", e);
                        thread_exit_evt.write(1).ok();
                    }
                }
            };
        })
        .map(|thread| epoll_threads.push(thread))
        .map_err(|e| {
            error!("Failed to spawn thread for {}: {}", name, e);
            ActivateError::ThreadSpawn(e)
        })
}
