// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// Copyright © 2020 Intel Corporation
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

use super::Error as DeviceError;
use super::{
    ActivateError, ActivateResult, EpollHelper, EpollHelperError, EpollHelperHandler, VirtioCommon,
    VirtioDevice, VirtioDeviceType, EPOLL_HELPER_EVENT_LAST, VIRTIO_F_VERSION_1,
};
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::GuestMemoryMmap;
use crate::{VirtioInterrupt, VirtioInterruptType};
use anyhow::anyhow;
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::fs::File;
use std::io::{self, Read};
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::result;
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Barrier, Mutex};
use std::time::Instant;
use thiserror::Error;
use virtio_queue::{Queue, QueueT};
use vm_memory::{Bytes, GuestAddressSpace, GuestMemoryAtomic};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vmm_sys_util::eventfd::EventFd;

const QUEUE_SIZE: u16 = 8;
const QUEUE_SIZES: &[u16] = &[QUEUE_SIZE];

// New descriptors are pending on the virtio queue.
const QUEUE_AVAIL_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
// Timer expired
const TIMER_EXPIRED_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 2;

// Number of seconds to check to see if there has been a ping
// This needs to match what the driver is using.
const WATCHDOG_TIMER_INTERVAL: i64 = 15;

// Number of seconds since last ping to trigger reboot
const WATCHDOG_TIMEOUT: u64 = WATCHDOG_TIMER_INTERVAL as u64 + 5;

#[derive(Error, Debug)]
enum Error {
    #[error("Error programming timer fd: {0}")]
    TimerfdSetup(io::Error),
    #[error("Descriptor chain too short")]
    DescriptorChainTooShort,
    #[error("Failed adding used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
    #[error("Invalid descriptor")]
    InvalidDescriptor,
    #[error("Failed to write to guest memory: {0}")]
    GuestMemoryWrite(vm_memory::guest_memory::Error),
}

struct WatchdogEpollHandler {
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    queue: Queue,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    queue_evt: EventFd,
    kill_evt: EventFd,
    pause_evt: EventFd,
    timer: File,
    last_ping_time: Arc<Mutex<Option<Instant>>>,
    reset_evt: EventFd,
}

impl WatchdogEpollHandler {
    // The main queue is very simple - the driver "pings" the device by passing it a (write-only)
    // descriptor. In response the device writes a 1 into the descriptor and returns it to the driver
    fn process_queue(&mut self) -> result::Result<bool, Error> {
        let queue = &mut self.queue;
        let mut used_descs = false;
        while let Some(mut desc_chain) = queue.pop_descriptor_chain(self.mem.memory()) {
            let desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;

            if !(desc.is_write_only() && desc.len() > 0) {
                return Err(Error::InvalidDescriptor);
            }

            desc_chain
                .memory()
                .write_obj(1u8, desc.addr())
                .map_err(Error::GuestMemoryWrite)?;

            // If this is the first "ping" then setup the timer
            if self.last_ping_time.lock().unwrap().is_none() {
                info!(
                    "First ping received. Starting timer (every {} seconds)",
                    WATCHDOG_TIMER_INTERVAL
                );
                timerfd_setup(&self.timer, WATCHDOG_TIMER_INTERVAL).map_err(Error::TimerfdSetup)?;
            }
            self.last_ping_time.lock().unwrap().replace(Instant::now());

            queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), desc.len())
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;
        }

        Ok(used_descs)
    }

    fn signal_used_queue(&self) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(0))
            .map_err(|e| {
                error!("Failed to signal used queue: {:?}", e);
                DeviceError::FailedSignalingUsedQueue(e)
            })
    }

    fn run(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
    ) -> result::Result<(), EpollHelperError> {
        let mut helper = EpollHelper::new(&self.kill_evt, &self.pause_evt)?;
        helper.add_event(self.queue_evt.as_raw_fd(), QUEUE_AVAIL_EVENT)?;
        helper.add_event(self.timer.as_raw_fd(), TIMER_EXPIRED_EVENT)?;
        helper.run(paused, paused_sync, self)?;

        Ok(())
    }
}

impl EpollHelperHandler for WatchdogEpollHandler {
    fn handle_event(
        &mut self,
        _helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;
        match ev_type {
            QUEUE_AVAIL_EVENT => {
                self.queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get queue event: {:?}", e))
                })?;

                let needs_notification = self.process_queue().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to process queue : {:?}", e))
                })?;
                if needs_notification {
                    self.signal_used_queue().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to signal used queue: {:?}",
                            e
                        ))
                    })?;
                }
            }
            TIMER_EXPIRED_EVENT => {
                // When reading from the timerfd you get 8 bytes indicating
                // the number of times this event has elapsed since the last read.
                let mut buf = vec![0; 8];
                self.timer.read_exact(&mut buf).map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Error reading from timer fd: {:}", e))
                })?;

                if let Some(last_ping_time) = self.last_ping_time.lock().unwrap().as_ref() {
                    let now = Instant::now();
                    let gap = now.duration_since(*last_ping_time).as_secs();
                    if gap > WATCHDOG_TIMEOUT {
                        error!("Watchdog triggered: {} seconds since last ping", gap);
                        self.reset_evt.write(1).ok();
                    }
                }
            }
            _ => {
                return Err(EpollHelperError::HandleEvent(anyhow!(
                    "Unexpected event: {}",
                    ev_type
                )));
            }
        }
        Ok(())
    }
}

/// Virtio device for exposing a watchdog to the guest
pub struct Watchdog {
    common: VirtioCommon,
    id: String,
    seccomp_action: SeccompAction,
    reset_evt: EventFd,
    last_ping_time: Arc<Mutex<Option<Instant>>>,
    timer: File,
    exit_evt: EventFd,
}

#[derive(Serialize, Deserialize)]
pub struct WatchdogState {
    pub avail_features: u64,
    pub acked_features: u64,
    pub enabled: bool,
}

impl Watchdog {
    /// Create a new virtio watchdog device that will reboot VM if the guest hangs
    pub fn new(
        id: String,
        reset_evt: EventFd,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        state: Option<WatchdogState>,
    ) -> io::Result<Watchdog> {
        let mut last_ping_time = None;
        let (avail_features, acked_features) = if let Some(state) = state {
            info!("Restoring virtio-watchdog {}", id);

            // When restoring enable the watchdog if it was previously enabled.
            // We reset the timer to ensure that we don't unnecessarily reboot
            // due to the offline time.
            if state.enabled {
                last_ping_time = Some(Instant::now());
            }

            (state.avail_features, state.acked_features)
        } else {
            (1u64 << VIRTIO_F_VERSION_1, 0)
        };

        let timer_fd = timerfd_create().map_err(|e| {
            error!("Failed to create timer fd {}", e);
            e
        })?;
        let timer = unsafe { File::from_raw_fd(timer_fd) };

        Ok(Watchdog {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Watchdog as u32,
                queue_sizes: QUEUE_SIZES.to_vec(),
                paused_sync: Some(Arc::new(Barrier::new(2))),
                avail_features,
                acked_features,
                min_queues: 1,
                ..Default::default()
            },
            id,
            seccomp_action,
            reset_evt,
            last_ping_time: Arc::new(Mutex::new(last_ping_time)),
            timer,
            exit_evt,
        })
    }

    fn state(&self) -> WatchdogState {
        WatchdogState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            enabled: self.last_ping_time.lock().unwrap().is_some(),
        }
    }

    #[cfg(fuzzing)]
    pub fn wait_for_epoll_threads(&mut self) {
        self.common.wait_for_epoll_threads();
    }
}

impl Drop for Watchdog {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
    }
}

fn timerfd_create() -> Result<RawFd, io::Error> {
    let res = unsafe { libc::timerfd_create(libc::CLOCK_MONOTONIC, 0) };
    if res < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(res as RawFd)
    }
}

fn timerfd_setup(timer: &File, secs: i64) -> Result<(), io::Error> {
    let periodic = libc::itimerspec {
        it_interval: libc::timespec {
            tv_sec: secs,
            tv_nsec: 0,
        },
        it_value: libc::timespec {
            tv_sec: secs,
            tv_nsec: 0,
        },
    };

    let res =
        unsafe { libc::timerfd_settime(timer.as_raw_fd(), 0, &periodic, std::ptr::null_mut()) };

    if res < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}

impl VirtioDevice for Watchdog {
    fn device_type(&self) -> u32 {
        self.common.device_type
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &self.common.queue_sizes
    }

    fn features(&self) -> u64 {
        self.common.avail_features
    }

    fn ack_features(&mut self, value: u64) {
        self.common.ack_features(value)
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        let (kill_evt, pause_evt) = self.common.dup_eventfds();

        let reset_evt = self.reset_evt.try_clone().map_err(|e| {
            error!("Failed to clone reset_evt eventfd: {}", e);
            ActivateError::BadActivate
        })?;

        let timer = self.timer.try_clone().map_err(|e| {
            error!("Failed to clone timer fd: {}", e);
            ActivateError::BadActivate
        })?;

        let (_, queue, queue_evt) = queues.remove(0);

        let mut handler = WatchdogEpollHandler {
            mem,
            queue,
            interrupt_cb,
            queue_evt,
            kill_evt,
            pause_evt,
            timer,
            last_ping_time: self.last_ping_time.clone(),
            reset_evt,
        };

        let paused = self.common.paused.clone();
        let paused_sync = self.common.paused_sync.clone();
        let mut epoll_threads = Vec::new();

        spawn_virtio_thread(
            &self.id,
            &self.seccomp_action,
            Thread::VirtioWatchdog,
            &mut epoll_threads,
            &self.exit_evt,
            move || handler.run(paused, paused_sync.unwrap()),
        )?;

        self.common.epoll_threads = Some(epoll_threads);

        event!("virtio-device", "activated", "id", &self.id);
        Ok(())
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        let result = self.common.reset();
        event!("virtio-device", "reset", "id", &self.id);
        result
    }
}

impl Pausable for Watchdog {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        info!("Watchdog paused - disabling timer");
        timerfd_setup(&self.timer, 0)
            .map_err(|e| MigratableError::Pause(anyhow!("Error clearing timer: {:?}", e)))?;
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        // Reset the timer on pause if it was previously used
        if self.last_ping_time.lock().unwrap().is_some() {
            info!(
                "Watchdog resumed - enabling timer (every {} seconds)",
                WATCHDOG_TIMER_INTERVAL
            );
            self.last_ping_time.lock().unwrap().replace(Instant::now());
            timerfd_setup(&self.timer, WATCHDOG_TIMER_INTERVAL)
                .map_err(|e| MigratableError::Resume(anyhow!("Error setting timer: {:?}", e)))?;
        }
        self.common.resume()
    }
}

impl Snapshottable for Watchdog {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id, &self.state())
    }
}

impl Transportable for Watchdog {}
impl Migratable for Watchdog {}
