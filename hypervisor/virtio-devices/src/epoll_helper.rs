// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use std::fs::File;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Barrier};
use std::thread;
use thiserror::Error;
use vmm_sys_util::eventfd::EventFd;

pub struct EpollHelper {
    pause_evt: EventFd,
    epoll_file: File,
}

#[derive(Error, Debug)]
pub enum EpollHelperError {
    #[error("Failed to create Fd: {0}")]
    CreateFd(std::io::Error),
    #[error("Failed to epoll_ctl: {0}")]
    Ctl(std::io::Error),
    #[error("IO error: {0}")]
    IoError(std::io::Error),
    #[error("Failed to epoll_wait: {0}")]
    Wait(std::io::Error),
    #[error("Failed to get virtio-queue index: {0}")]
    QueueRingIndex(virtio_queue::Error),
    #[error("Failed to handle virtio device events: {0}")]
    HandleEvent(anyhow::Error),
    #[error("Failed to handle timeout: {0}")]
    HandleTimeout(anyhow::Error),
}

pub const EPOLL_HELPER_EVENT_PAUSE: u16 = 0;
pub const EPOLL_HELPER_EVENT_KILL: u16 = 1;
pub const EPOLL_HELPER_EVENT_LAST: u16 = 15;

pub trait EpollHelperHandler {
    // Handle one event at a time. The EpollHelper iterates over a list of
    // events that have been returned by epoll_wait(). For each event, the
    // current method is invoked to let the implementation decide how to process
    // the incoming event.
    fn handle_event(
        &mut self,
        helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> Result<(), EpollHelperError>;

    // Called when PAUSE_EVENT is received, before the pause barrier and
    // park(). Implementations can use this hook to drain inflight work
    // and bring device state to a quiescent point so that a subsequent
    // snapshot is consistent. The hook MUST NOT propagate errors via
    // Err here: returning Err would cause the epoll loop to exit early
    // and skip paused_sync.wait(), deadlocking the pausing thread.
    // Implementations should swallow failures internally (e.g. via a
    // shared atomic flag) and always return Ok.
    fn before_pause(&mut self, _helper: &mut EpollHelper) -> Result<(), EpollHelperError> {
        Ok(())
    }

    // This method is only invoked if the EpollHelper was configured to call
    // epoll_wait() with a valid timeout (different from -1), meaning the call
    // won't block forever. When the timeout is reached, and if no even has been
    // triggered, this function will be called to let the implementation decide
    // how to interpret such situation. By default, it provides a no-op
    // implementation.
    fn handle_timeout(&mut self, _helper: &mut EpollHelper) -> Result<(), EpollHelperError> {
        Ok(())
    }

    // In some situations, it might be useful to know the full list of events
    // triggered while waiting on epoll_wait(). And having this list provided
    // prior to the iterations over each event might help make some informed
    // decisions. This function should not replace handle_event(), otherwise it
    // would completely defeat the purpose of having the loop being factorized
    // through the EpollHelper structure.
    fn event_list(
        &mut self,
        _helper: &mut EpollHelper,
        _events: &[epoll::Event],
    ) -> Result<(), EpollHelperError> {
        Ok(())
    }
}

impl EpollHelper {
    pub fn new(
        kill_evt: &EventFd,
        pause_evt: &EventFd,
    ) -> std::result::Result<Self, EpollHelperError> {
        // Create the epoll file descriptor
        let epoll_fd = epoll::create(true).map_err(EpollHelperError::CreateFd)?;
        // Use 'File' to enforce closing on 'epoll_fd'
        let epoll_file = unsafe { File::from_raw_fd(epoll_fd) };

        let mut helper = Self {
            pause_evt: pause_evt.try_clone().unwrap(),
            epoll_file,
        };

        helper.add_event(kill_evt.as_raw_fd(), EPOLL_HELPER_EVENT_KILL)?;
        helper.add_event(pause_evt.as_raw_fd(), EPOLL_HELPER_EVENT_PAUSE)?;
        Ok(helper)
    }

    pub fn add_event(&mut self, fd: RawFd, id: u16) -> std::result::Result<(), EpollHelperError> {
        self.add_event_custom(fd, id, epoll::Events::EPOLLIN)
    }

    pub fn add_event_custom(
        &mut self,
        fd: RawFd,
        id: u16,
        evts: epoll::Events,
    ) -> std::result::Result<(), EpollHelperError> {
        epoll::ctl(
            self.epoll_file.as_raw_fd(),
            epoll::ControlOptions::EPOLL_CTL_ADD,
            fd,
            epoll::Event::new(evts, id.into()),
        )
        .map_err(EpollHelperError::Ctl)
    }

    pub fn mod_event_custom(
        &mut self,
        fd: RawFd,
        id: u16,
        evts: epoll::Events,
    ) -> std::result::Result<(), EpollHelperError> {
        epoll::ctl(
            self.epoll_file.as_raw_fd(),
            epoll::ControlOptions::EPOLL_CTL_MOD,
            fd,
            epoll::Event::new(evts, id.into()),
        )
        .map_err(EpollHelperError::Ctl)
    }

    pub fn del_event_custom(
        &mut self,
        fd: RawFd,
        id: u16,
        evts: epoll::Events,
    ) -> std::result::Result<(), EpollHelperError> {
        epoll::ctl(
            self.epoll_file.as_raw_fd(),
            epoll::ControlOptions::EPOLL_CTL_DEL,
            fd,
            epoll::Event::new(evts, id.into()),
        )
        .map_err(EpollHelperError::Ctl)
    }

    pub fn run(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
        handler: &mut dyn EpollHelperHandler,
    ) -> std::result::Result<(), EpollHelperError> {
        self.run_with_timeout(paused, paused_sync, handler, -1, false)
    }

    #[cfg(not(fuzzing))]
    pub fn run_with_timeout(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
        handler: &mut dyn EpollHelperHandler,
        timeout: i32,
        enable_event_list: bool,
    ) -> std::result::Result<(), EpollHelperError> {
        const EPOLL_EVENTS_LEN: usize = 100;
        let mut events = vec![epoll::Event::new(epoll::Events::empty(), 0); EPOLL_EVENTS_LEN];

        // Before jumping into the epoll loop, check if the device is expected
        // to be in a paused state. This is helpful for the restore code path
        // as the device thread should not start processing anything before the
        // device has been resumed.
        while paused.load(Ordering::SeqCst) {
            thread::park();
        }

        loop {
            let num_events =
                match epoll::wait(self.epoll_file.as_raw_fd(), timeout, &mut events[..]) {
                    Ok(res) => res,
                    Err(e) => {
                        if e.kind() == std::io::ErrorKind::Interrupted {
                            // It's well defined from the epoll_wait() syscall
                            // documentation that the epoll loop can be interrupted
                            // before any of the requested events occurred or the
                            // timeout expired. In both those cases, epoll_wait()
                            // returns an error of type EINTR, but this should not
                            // be considered as a regular error. Instead it is more
                            // appropriate to retry, by calling into epoll_wait().
                            continue;
                        }
                        return Err(EpollHelperError::Wait(e));
                    }
                };

            if num_events == 0 {
                // This case happens when the timeout is reached before any of
                // the registered events is triggered.
                handler.handle_timeout(self)?;
                continue;
            }

            if enable_event_list {
                handler.event_list(self, &events[..num_events])?;
            }

            for event in events.iter().take(num_events) {
                let ev_type = event.data as u16;

                match ev_type {
                    EPOLL_HELPER_EVENT_KILL => {
                        info!("KILL_EVENT received, stopping epoll loop");
                        return Ok(());
                    }
                    EPOLL_HELPER_EVENT_PAUSE => {
                        info!("PAUSE_EVENT received, pausing epoll loop");

                        // Per-device pre-pause hook (e.g. drain inflight IO
                        // for virtio-blk). Contract: hook always returns Ok
                        // so we never skip the barrier below.
                        handler.before_pause(self)?;

                        // Acknowledge the pause is effective by using the
                        // paused_sync barrier.
                        paused_sync.wait();

                        // We loop here to handle spurious park() returns.
                        // Until we have not resumed, the paused boolean will
                        // be true.
                        while paused.load(Ordering::SeqCst) {
                            thread::park();
                        }

                        // Drain pause event after the device has been resumed.
                        // This ensures the pause event has been seen by each
                        // thread related to this virtio device.
                        let _ = self.pause_evt.read();
                    }
                    _ => {
                        handler.handle_event(self, event)?;
                    }
                }
            }
        }
    }

    #[cfg(fuzzing)]
    // Require to have a 'queue_evt' being kicked before calling
    // and return when no epoll events are active
    pub fn run_with_timeout(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
        handler: &mut dyn EpollHelperHandler,
        _timeout: i32,
        _enable_event_list: bool,
    ) -> std::result::Result<(), EpollHelperError> {
        const EPOLL_EVENTS_LEN: usize = 100;
        let mut events = vec![epoll::Event::new(epoll::Events::empty(), 0); EPOLL_EVENTS_LEN];

        loop {
            let num_events = match epoll::wait(self.epoll_file.as_raw_fd(), 0, &mut events[..]) {
                Ok(res) => res,
                Err(e) => {
                    if e.kind() == std::io::ErrorKind::Interrupted {
                        // It's well defined from the epoll_wait() syscall
                        // documentation that the epoll loop can be interrupted
                        // before any of the requested events occurred or the
                        // timeout expired. In both those cases, epoll_wait()
                        // returns an error of type EINTR, but this should not
                        // be considered as a regular error. Instead it is more
                        // appropriate to retry, by calling into epoll_wait().
                        continue;
                    }
                    return Err(EpollHelperError::Wait(e));
                }
            };

            // Return when no epoll events are active
            if num_events == 0 {
                return Ok(());
            }

            for event in events.iter().take(num_events) {
                let ev_type = event.data as u16;

                match ev_type {
                    EPOLL_HELPER_EVENT_KILL => {
                        info!("KILL_EVENT received, stopping epoll loop");
                        return Ok(());
                    }
                    EPOLL_HELPER_EVENT_PAUSE => {
                        info!("PAUSE_EVENT received, pausing epoll loop");

                        // Per-device pre-pause hook. See the non-fuzzing
                        // branch for the contract.
                        handler.before_pause(self)?;

                        // Acknowledge the pause is effective by using the
                        // paused_sync barrier.
                        paused_sync.wait();

                        // We loop here to handle spurious park() returns.
                        // Until we have not resumed, the paused boolean will
                        // be true.
                        while paused.load(Ordering::SeqCst) {
                            thread::park();
                        }

                        // Drain pause event after the device has been resumed.
                        // This ensures the pause event has been seen by each
                        // thread related to this virtio device.
                        let _ = self.pause_evt.read();
                    }
                    _ => {
                        handler.handle_event(self, event)?;
                    }
                }
            }
        }
    }
}

impl AsRawFd for EpollHelper {
    fn as_raw_fd(&self) -> RawFd {
        self.epoll_file.as_raw_fd()
    }
}
