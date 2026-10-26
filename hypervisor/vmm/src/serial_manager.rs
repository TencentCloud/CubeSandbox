// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::device_manager::PtyPair;
use crate::vm_config::ConsoleOutputMode;
#[cfg(target_arch = "aarch64")]
use devices::legacy::Pl011;
#[cfg(target_arch = "x86_64")]
use devices::legacy::Serial;
use libc::EFD_NONBLOCK;
use serial_buffer::SerialBuffer;
use std::fs::{File, OpenOptions};
use std::io::Read;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::panic::AssertUnwindSafe;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::{io, result, thread};
use thiserror::Error;
use vmm_sys_util::eventfd::EventFd;

#[derive(Debug, Error)]
pub enum Error {
    /// Cannot clone File.
    #[error("Error cloning File: {0}")]
    FileClone(#[source] io::Error),

    /// Cannot create epoll context.
    #[error("Error creating epoll context: {0}")]
    Epoll(#[source] io::Error),

    /// Cannot handle the VM input stream.
    #[error("Error handling VM input: {0:?}")]
    ReadInput(#[source] io::Error),

    /// Cannot queue input to the serial device.
    #[error("Error queuing input to the serial device: {0}")]
    QueueInput(#[source] vmm_sys_util::errno::Error),

    /// Cannot flush output on the serial buffer.
    #[error("Error flushing serial device's output buffer: {0}")]
    FlushOutput(#[source] io::Error),

    /// Cannot open the terminal input file descriptor.
    #[error("Error opening input file descriptor: {0}")]
    OpenInput(#[source] io::Error),

    /// Cannot create EventFd.
    #[error("Error creating EventFd: {0}")]
    EventFd(#[source] io::Error),

    /// Cannot spawn SerialManager thread.
    #[error("Error spawning SerialManager thread: {0}")]
    SpawnSerialManager(#[source] io::Error),
}
pub type Result<T> = result::Result<T, Error>;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u64)]
pub enum EpollDispatch {
    File = 0,
    Kill = 1,
    Unknown,
}

impl From<u64> for EpollDispatch {
    fn from(v: u64) -> Self {
        use EpollDispatch::*;
        match v {
            0 => File,
            1 => Kill,
            _ => Unknown,
        }
    }
}

pub struct SerialManager {
    #[cfg(target_arch = "x86_64")]
    serial: Arc<Mutex<Serial>>,
    #[cfg(target_arch = "aarch64")]
    serial: Arc<Mutex<Pl011>>,
    epoll_file: File,
    in_file: File,
    kill_evt: EventFd,
    handle: Option<thread::JoinHandle<()>>,
    pty_write_out: Option<Arc<AtomicBool>>,
}

impl SerialManager {
    pub fn new(
        #[cfg(target_arch = "x86_64")] serial: Arc<Mutex<Serial>>,
        #[cfg(target_arch = "aarch64")] serial: Arc<Mutex<Pl011>>,
        pty_pair: Option<Arc<Mutex<PtyPair>>>,
        mode: ConsoleOutputMode,
    ) -> Result<Option<Self>> {
        let in_file = match mode {
            ConsoleOutputMode::Pty => {
                if let Some(pty_pair) = pty_pair {
                    pty_pair
                        .lock()
                        .unwrap()
                        .main
                        .try_clone()
                        .map_err(Error::FileClone)?
                } else {
                    return Ok(None);
                }
            }
            ConsoleOutputMode::Tty => {
                // If running on an interactive TTY then accept input
                if unsafe { libc::isatty(libc::STDIN_FILENO) == 1 } {
                    // Reopen stdin so setting O_NONBLOCK does not change the
                    // file status flags shared with stdout.
                    OpenOptions::new()
                        .read(true)
                        .custom_flags(libc::O_NONBLOCK)
                        .open("/proc/self/fd/0")
                        .map_err(Error::OpenInput)?
                } else {
                    return Ok(None);
                }
            }
            _ => return Ok(None),
        };

        let epoll_fd = epoll::create(true).map_err(Error::Epoll)?;
        let kill_evt = EventFd::new(EFD_NONBLOCK).map_err(Error::EventFd)?;

        epoll::ctl(
            epoll_fd,
            epoll::ControlOptions::EPOLL_CTL_ADD,
            kill_evt.as_raw_fd(),
            epoll::Event::new(epoll::Events::EPOLLIN, EpollDispatch::Kill as u64),
        )
        .map_err(Error::Epoll)?;

        epoll::ctl(
            epoll_fd,
            epoll::ControlOptions::EPOLL_CTL_ADD,
            in_file.as_raw_fd(),
            epoll::Event::new(epoll::Events::EPOLLIN, EpollDispatch::File as u64),
        )
        .map_err(Error::Epoll)?;

        let mut pty_write_out = None;
        if mode == ConsoleOutputMode::Pty {
            let write_out = Arc::new(AtomicBool::new(false));
            pty_write_out = Some(write_out.clone());
            let writer = in_file.try_clone().map_err(Error::FileClone)?;
            let buffer = SerialBuffer::new(Box::new(writer), write_out);
            serial.as_ref().lock().unwrap().set_out(Box::new(buffer));
        }

        // Use 'File' to enforce closing on 'epoll_fd'
        let epoll_file = unsafe { File::from_raw_fd(epoll_fd) };

        Ok(Some(SerialManager {
            serial,
            epoll_file,
            in_file,
            kill_evt,
            handle: None,
            pty_write_out,
        }))
    }

    // This function should be called when the other end of the PTY is
    // connected. It verifies if this is the first time it's been invoked
    // after the connection happened, and if that's the case it flushes
    // all output from the serial to the PTY. Otherwise, it's a no-op.
    fn trigger_pty_flush(
        #[cfg(target_arch = "x86_64")] serial: &Arc<Mutex<Serial>>,
        #[cfg(target_arch = "aarch64")] serial: &Arc<Mutex<Pl011>>,
        pty_write_out: Option<&Arc<AtomicBool>>,
    ) -> Result<()> {
        if let Some(pty_write_out) = &pty_write_out {
            if pty_write_out.load(Ordering::Acquire) {
                return Ok(());
            }

            pty_write_out.store(true, Ordering::Release);

            serial
                .lock()
                .unwrap()
                .flush_output()
                .map_err(Error::FlushOutput)?;
        }

        Ok(())
    }

    pub fn start_thread(&mut self, exit_evt: EventFd) -> Result<()> {
        // Don't allow this to be run if the handle exists
        if self.handle.is_some() {
            warn!("Tried to start multiple SerialManager threads, ignoring");
            return Ok(());
        }

        let epoll_fd = self.epoll_file.as_raw_fd();
        let mut in_file = self.in_file.try_clone().map_err(Error::FileClone)?;
        let serial = self.serial.clone();
        let pty_write_out = self.pty_write_out.clone();

        // In case of PTY, we want to be able to detect a connection on the
        // other end of the PTY. This is done by detecting there's no event
        // triggered on the epoll, which is the reason why we want the
        // epoll_wait() function to return after the timeout expired.
        // In case of TTY, we don't expect to detect such behavior, which is
        // why we can afford to block until an actual event is triggered.
        let timeout = if pty_write_out.is_some() { 500 } else { -1 };

        let thread = thread::Builder::new()
            .name("serial-manager".to_string())
            .spawn(move || {
                std::panic::catch_unwind(AssertUnwindSafe(move || {
                    // 3 for File, Kill, and Unknown
                    const EPOLL_EVENTS_LEN: usize = 3;

                    let mut events =
                        vec![epoll::Event::new(epoll::Events::empty(), 0); EPOLL_EVENTS_LEN];

                    loop {
                        let num_events = match epoll::wait(epoll_fd, timeout, &mut events[..]) {
                            Ok(res) => res,
                            Err(e) => {
                                if e.kind() == io::ErrorKind::Interrupted {
                                    // It's well defined from the epoll_wait() syscall
                                    // documentation that the epoll loop can be interrupted
                                    // before any of the requested events occurred or the
                                    // timeout expired. In both those cases, epoll_wait()
                                    // returns an error of type EINTR, but this should not
                                    // be considered as a regular error. Instead it is more
                                    // appropriate to retry, by calling into epoll_wait().
                                    continue;
                                } else {
                                    return Err(Error::Epoll(e));
                                }
                            }
                        };

                        if num_events == 0 {
                            // This very specific case happens when the serial is connected
                            // to a PTY. We know EPOLLHUP is always present when there's nothing
                            // connected at the other end of the PTY. That's why getting no event
                            // means we can flush the output of the serial through the PTY.
                            Self::trigger_pty_flush(&serial, pty_write_out.as_ref())?;
                            continue;
                        }

                        for event in events.iter().take(num_events) {
                            let dispatch_event: EpollDispatch = event.data.into();
                            match dispatch_event {
                                EpollDispatch::Unknown => {
                                    let event = event.data;
                                    warn!("Unknown serial manager loop event: {}", event);
                                }
                                EpollDispatch::File => {
                                    if event.events & libc::EPOLLIN as u32 != 0 {
                                        let mut input = [0u8; 64];
                                        let count =
                                            in_file.read(&mut input).map_err(Error::ReadInput)?;

                                        // Replace "\n" with "\r" to deal with Windows SAC (#1170)
                                        if count == 1 && input[0] == 0x0a {
                                            input[0] = 0x0d;
                                        }

                                        serial
                                            .as_ref()
                                            .lock()
                                            .unwrap()
                                            .queue_input_bytes(&input[..count])
                                            .map_err(Error::QueueInput)?;
                                    }
                                    if event.events & libc::EPOLLHUP as u32 != 0 {
                                        if let Some(pty_write_out) = &pty_write_out {
                                            pty_write_out.store(false, Ordering::Release);
                                        }
                                        // It's really important to sleep here as this will prevent
                                        // the current thread from consuming 100% of the CPU cycles
                                        // when waiting for someone to connect to the PTY.
                                        std::thread::sleep(std::time::Duration::from_millis(500));
                                    } else {
                                        // If the EPOLLHUP flag is not up on the associated event, we
                                        // can assume the other end of the PTY is connected and therefore
                                        // we can flush the output of the serial to it.
                                        Self::trigger_pty_flush(&serial, pty_write_out.as_ref())?;
                                    }
                                }
                                EpollDispatch::Kill => {
                                    info!("KILL event received, stopping epoll loop");
                                    return Ok(());
                                }
                            }
                        }
                    }
                }))
                .map_err(|_| {
                    error!("serial-manager thread panicked");
                    exit_evt.write(1).ok()
                })
                .ok();
            })
            .map_err(Error::SpawnSerialManager)?;
        self.handle = Some(thread);
        Ok(())
    }
}

impl Drop for SerialManager {
    fn drop(&mut self) {
        self.kill_evt.write(1).ok();
        if let Some(handle) = self.handle.take() {
            handle.join().ok();
        }
    }
}
