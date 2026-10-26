// Copyright 2019 Intel Corporation. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use super::Error as DeviceError;
use super::{
    ActivateResult, EpollHelper, EpollHelperError, EpollHelperHandler, VirtioCommon, VirtioDevice,
    VirtioDeviceType, VirtioInterruptType, EPOLL_HELPER_EVENT_LAST, VIRTIO_F_IOMMU_PLATFORM,
    VIRTIO_F_VERSION_1,
};
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::GuestMemoryMmap;
use crate::VirtioInterrupt;
use anyhow::anyhow;
use libc::{EFD_NONBLOCK, TIOCGWINSZ};
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use serial_buffer::SerialBuffer;
use std::cmp;
use std::collections::VecDeque;
use std::fs::File;
use std::io;
use std::io::{Read, Write};
use std::os::unix::io::AsRawFd;
use std::result;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Barrier, Mutex};
use thiserror::Error;
use virtio_queue::{Queue, QueueT};
use vm_memory::{ByteValued, Bytes, GuestAddressSpace, GuestMemory, GuestMemoryAtomic};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::{AccessPlatform, Translatable};
use vmm_sys_util::eventfd::EventFd;

const QUEUE_SIZE: u16 = 256;
const NUM_QUEUES: usize = 2;
const QUEUE_SIZES: &[u16] = &[QUEUE_SIZE; NUM_QUEUES];

// New descriptors are pending on the virtio queue.
const INPUT_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
const OUTPUT_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 2;
// Console configuration change event is triggered.
const CONFIG_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 3;
// File written to (input ready)
const FILE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 4;
// Console resized
const RESIZE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 5;

//Console size feature bit
const VIRTIO_CONSOLE_F_SIZE: u64 = 0;

#[derive(Error, Debug)]
enum Error {
    #[error("Descriptor chain too short")]
    DescriptorChainTooShort,
    #[error("Failed to read from guest memory: {0}")]
    GuestMemoryRead(vm_memory::guest_memory::Error),
    #[error("Failed to write to guest memory: {0}")]
    GuestMemoryWrite(vm_memory::guest_memory::Error),
    #[error("Failed to write_all output: {0}")]
    OutputWriteAll(io::Error),
    #[error("Failed to flush output: {0}")]
    OutputFlush(io::Error),
    #[error("Failed to add used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
}

#[derive(Copy, Clone, Debug, Serialize, Deserialize)]
#[repr(C, packed)]
pub struct VirtioConsoleConfig {
    cols: u16,
    rows: u16,
    max_nr_ports: u32,
    emerg_wr: u32,
}

impl Default for VirtioConsoleConfig {
    fn default() -> Self {
        VirtioConsoleConfig {
            cols: 0,
            rows: 0,
            max_nr_ports: 1,
            emerg_wr: 0,
        }
    }
}

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioConsoleConfig {}

struct ConsoleEpollHandler {
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    input_queue: Queue,
    output_queue: Queue,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    in_buffer: Arc<Mutex<VecDeque<u8>>>,
    resizer: Arc<ConsoleResizer>,
    endpoint: Endpoint,
    input_queue_evt: EventFd,
    output_queue_evt: EventFd,
    config_evt: EventFd,
    resize_pipe: Option<File>,
    kill_evt: EventFd,
    pause_evt: EventFd,
    access_platform: Option<Arc<dyn AccessPlatform>>,
    out: Option<Box<dyn Write + Send>>,
    write_out: Option<Arc<AtomicBool>>,
    file_event_registered: bool,
}

pub enum Endpoint {
    File(File),
    FilePair(File, File),
    PtyPair(File, File),
    Null,
}

impl Endpoint {
    fn out_file(&self) -> Option<&File> {
        match self {
            Self::File(f) => Some(f),
            Self::FilePair(f, _) => Some(f),
            Self::PtyPair(f, _) => Some(f),
            Self::Null => None,
        }
    }

    fn in_file(&self) -> Option<&File> {
        match self {
            Self::File(_) => None,
            Self::FilePair(_, f) => Some(f),
            Self::PtyPair(_, f) => Some(f),
            Self::Null => None,
        }
    }

    fn is_pty(&self) -> bool {
        matches!(self, Self::PtyPair(_, _))
    }
}

impl Clone for Endpoint {
    fn clone(&self) -> Self {
        match self {
            Self::File(f) => Self::File(f.try_clone().unwrap()),
            Self::FilePair(f_out, f_in) => {
                Self::FilePair(f_out.try_clone().unwrap(), f_in.try_clone().unwrap())
            }
            Self::PtyPair(f_out, f_in) => {
                Self::PtyPair(f_out.try_clone().unwrap(), f_in.try_clone().unwrap())
            }
            Self::Null => Self::Null,
        }
    }
}

impl ConsoleEpollHandler {
    #[allow(clippy::too_many_arguments)]
    fn new(
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        input_queue: Queue,
        output_queue: Queue,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        in_buffer: Arc<Mutex<VecDeque<u8>>>,
        resizer: Arc<ConsoleResizer>,
        endpoint: Endpoint,
        input_queue_evt: EventFd,
        output_queue_evt: EventFd,
        config_evt: EventFd,
        resize_pipe: Option<File>,
        kill_evt: EventFd,
        pause_evt: EventFd,
        access_platform: Option<Arc<dyn AccessPlatform>>,
    ) -> Self {
        let out_file = endpoint.out_file();
        let (out, write_out) = if let Some(out_file) = out_file {
            let writer = out_file.try_clone().unwrap();
            if endpoint.is_pty() {
                let pty_write_out = Arc::new(AtomicBool::new(false));
                let write_out = Some(pty_write_out.clone());
                let buffer = SerialBuffer::new(Box::new(writer), pty_write_out);
                (Some(Box::new(buffer) as Box<dyn Write + Send>), write_out)
            } else {
                (Some(Box::new(writer) as Box<dyn Write + Send>), None)
            }
        } else {
            (None, None)
        };

        ConsoleEpollHandler {
            mem,
            input_queue,
            output_queue,
            interrupt_cb,
            in_buffer,
            resizer,
            endpoint,
            input_queue_evt,
            output_queue_evt,
            config_evt,
            resize_pipe,
            kill_evt,
            pause_evt,
            access_platform,
            out,
            write_out,
            file_event_registered: false,
        }
    }

    /*
     * Each port of virtio console device has one receive
     * queue. One or more empty buffers are placed by the
     * driver in the receive queue for incoming data. Here,
     * we place the input data to these empty buffers.
     */
    fn process_input_queue(&mut self) -> Result<bool, Error> {
        let mut in_buffer = self.in_buffer.lock().unwrap();
        let recv_queue = &mut self.input_queue; //receiveq
        let mut used_descs = false;

        if in_buffer.is_empty() {
            return Ok(false);
        }

        while let Some(mut desc_chain) = recv_queue.pop_descriptor_chain(self.mem.memory()) {
            let desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;
            let len = cmp::min(desc.len(), in_buffer.len() as u32);
            let source_slice = in_buffer.drain(..len as usize).collect::<Vec<u8>>();

            desc_chain
                .memory()
                .write_slice(
                    &source_slice[..],
                    desc.addr()
                        .translate_gva(self.access_platform.as_ref(), desc.len() as usize),
                )
                .map_err(Error::GuestMemoryWrite)?;

            recv_queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), len)
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;

            if in_buffer.is_empty() {
                break;
            }
        }

        Ok(used_descs)
    }

    /*
     * Each port of virtio console device has one transmit
     * queue. For outgoing data, characters are placed in
     * the transmit queue by the driver. Therefore, here
     * we read data from the transmit queue and flush them
     * to the referenced address.
     */
    fn process_output_queue(&mut self) -> Result<bool, Error> {
        let trans_queue = &mut self.output_queue; //transmitq
        let mut used_descs = false;

        while let Some(mut desc_chain) = trans_queue.pop_descriptor_chain(self.mem.memory()) {
            let desc = desc_chain.next().ok_or(Error::DescriptorChainTooShort)?;
            if let Some(out) = &mut self.out {
                let mut buf: Vec<u8> = Vec::new();
                desc_chain
                    .memory()
                    .write_volatile_to(
                        desc.addr()
                            .translate_gva(self.access_platform.as_ref(), desc.len() as usize),
                        &mut buf,
                        desc.len() as usize,
                    )
                    .map_err(Error::GuestMemoryRead)?;

                out.write_all(&buf).map_err(Error::OutputWriteAll)?;
                out.flush().map_err(Error::OutputFlush)?;
            }
            trans_queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), desc.len())
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;
        }

        Ok(used_descs)
    }

    fn signal_used_queue(&self, queue_index: u16) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(queue_index))
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
        helper.add_event(self.input_queue_evt.as_raw_fd(), INPUT_QUEUE_EVENT)?;
        helper.add_event(self.output_queue_evt.as_raw_fd(), OUTPUT_QUEUE_EVENT)?;
        helper.add_event(self.config_evt.as_raw_fd(), CONFIG_EVENT)?;
        if let Some(resize_pipe) = self.resize_pipe.as_ref() {
            helper.add_event(resize_pipe.as_raw_fd(), RESIZE_EVENT)?;
        }
        if let Some(in_file) = self.endpoint.in_file() {
            let mut events = epoll::Events::EPOLLIN;
            if self.endpoint.is_pty() {
                events |= epoll::Events::EPOLLONESHOT;
            }
            helper.add_event_custom(in_file.as_raw_fd(), FILE_EVENT, events)?;
            self.file_event_registered = true;
        }

        // In case of PTY, we want to be able to detect a connection on the
        // other end of the PTY. This is done by detecting there's no event
        // triggered on the epoll, which is the reason why we want the
        // epoll_wait() function to return after the timeout expired.
        // In case of TTY, we don't expect to detect such behavior, which is
        // why we can afford to block until an actual event is triggered.
        let (timeout, enable_event_list) = if self.endpoint.is_pty() {
            (500, true)
        } else {
            (-1, false)
        };
        helper.run_with_timeout(paused, paused_sync, self, timeout, enable_event_list)?;

        Ok(())
    }

    // This function should be called when the other end of the PTY is
    // connected. It verifies if this is the first time it's been invoked
    // after the connection happened, and if that's the case it flushes
    // all output from the console to the PTY. Otherwise, it's a no-op.
    fn trigger_pty_flush(&mut self) -> result::Result<(), anyhow::Error> {
        if let (Some(pty_write_out), Some(out)) = (&self.write_out, &mut self.out) {
            if pty_write_out.load(Ordering::Acquire) {
                return Ok(());
            }
            pty_write_out.store(true, Ordering::Release);
            out.flush()
                .map_err(|e| anyhow!("Failed to flush PTY: {:?}", e))
        } else {
            Ok(())
        }
    }

    fn register_file_event(
        &mut self,
        helper: &mut EpollHelper,
    ) -> result::Result<(), EpollHelperError> {
        if self.file_event_registered {
            return Ok(());
        }

        // Re-arm the file event.
        helper.mod_event_custom(
            self.endpoint.in_file().unwrap().as_raw_fd(),
            FILE_EVENT,
            epoll::Events::EPOLLIN | epoll::Events::EPOLLONESHOT,
        )?;
        self.file_event_registered = true;

        Ok(())
    }
}

impl EpollHelperHandler for ConsoleEpollHandler {
    fn handle_event(
        &mut self,
        helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;

        match ev_type {
            INPUT_QUEUE_EVENT => {
                self.input_queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get queue event: {:?}", e))
                })?;
                let needs_notification = self.process_input_queue().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to process input queue : {:?}",
                        e
                    ))
                })?;
                if needs_notification {
                    self.signal_used_queue(0).map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to signal used queue: {:?}",
                            e
                        ))
                    })?;
                }
            }
            OUTPUT_QUEUE_EVENT => {
                self.output_queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get queue event: {:?}", e))
                })?;
                let needs_notification = self.process_output_queue().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to process output queue : {:?}",
                        e
                    ))
                })?;
                if needs_notification {
                    self.signal_used_queue(1).map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to signal used queue: {:?}",
                            e
                        ))
                    })?;
                }
            }
            CONFIG_EVENT => {
                self.config_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get config event: {:?}", e))
                })?;
                self.interrupt_cb
                    .trigger(VirtioInterruptType::Config)
                    .map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to signal console driver: {:?}",
                            e
                        ))
                    })?;
            }
            RESIZE_EVENT => {
                self.resize_pipe
                    .as_ref()
                    .unwrap()
                    .read_exact(&mut [0])
                    .map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to get resize event: {:?}",
                            e
                        ))
                    })?;
                self.resizer.update_console_size();
            }
            FILE_EVENT => {
                if event.events & libc::EPOLLIN as u32 != 0 {
                    let mut input = [0u8; 64];
                    if let Some(ref mut in_file) = self.endpoint.in_file() {
                        if let Ok(count) = in_file.read(&mut input) {
                            let mut in_buffer = self.in_buffer.lock().unwrap();
                            in_buffer.extend(&input[..count]);
                        }

                        let needs_notification = self.process_input_queue().map_err(|e| {
                            EpollHelperError::HandleEvent(anyhow!(
                                "Failed to process input queue : {:?}",
                                e
                            ))
                        })?;
                        if needs_notification {
                            self.signal_used_queue(0).map_err(|e| {
                                EpollHelperError::HandleEvent(anyhow!(
                                    "Failed to signal used queue: {:?}",
                                    e
                                ))
                            })?;
                        }
                    }
                }
                if self.endpoint.is_pty() {
                    self.file_event_registered = false;
                    if event.events & libc::EPOLLHUP as u32 != 0 {
                        if let Some(pty_write_out) = &self.write_out {
                            if pty_write_out.load(Ordering::Acquire) {
                                pty_write_out.store(false, Ordering::Release);
                            }
                        }
                    } else {
                        // If the EPOLLHUP flag is not up on the associated event, we
                        // can assume the other end of the PTY is connected and therefore
                        // we can flush the output of the serial to it.
                        self.trigger_pty_flush()
                            .map_err(EpollHelperError::HandleTimeout)?;

                        self.register_file_event(helper)?;
                    }
                }
            }
            _ => {
                return Err(EpollHelperError::HandleEvent(anyhow!(
                    "Unknown event for virtio-console"
                )));
            }
        }
        Ok(())
    }

    // This function will be invoked whenever the timeout is reached before
    // any other event was triggered while waiting for the epoll.
    fn handle_timeout(&mut self, helper: &mut EpollHelper) -> Result<(), EpollHelperError> {
        if !self.endpoint.is_pty() {
            return Ok(());
        }

        if self.file_event_registered {
            // This very specific case happens when the console is connected
            // to a PTY. We know EPOLLHUP is always present when there's nothing
            // connected at the other end of the PTY. That's why getting no event
            // means we can flush the output of the console through the PTY.
            self.trigger_pty_flush()
                .map_err(EpollHelperError::HandleTimeout)?;
        }

        // Every time we hit the timeout, let's register the FILE_EVENT to give
        // us a chance to catch a possible event that might have been triggered.
        self.register_file_event(helper)
    }

    // This function returns the full list of events found on the epoll before
    // iterating through it calling handle_event(). It allows the detection of
    // the PTY connection even when the timeout is not being triggered, which
    // happens when there are other events preventing the timeout from being
    // reached. This is an additional way of detecting a PTY connection.
    fn event_list(
        &mut self,
        helper: &mut EpollHelper,
        events: &[epoll::Event],
    ) -> Result<(), EpollHelperError> {
        if self.file_event_registered {
            for event in events {
                if event.data as u16 == FILE_EVENT && (event.events & libc::EPOLLHUP as u32) != 0 {
                    return Ok(());
                }
            }

            // This very specific case happens when the console is connected
            // to a PTY. We know EPOLLHUP is always present when there's nothing
            // connected at the other end of the PTY. That's why getting no event
            // means we can flush the output of the console through the PTY.
            self.trigger_pty_flush()
                .map_err(EpollHelperError::HandleTimeout)?;
        }

        self.register_file_event(helper)
    }
}

/// Resize handler
pub struct ConsoleResizer {
    config_evt: EventFd,
    tty: Option<File>,
    config: Arc<Mutex<VirtioConsoleConfig>>,
    acked_features: AtomicU64,
}

impl ConsoleResizer {
    pub fn update_console_size(&self) {
        if let Some(tty) = self.tty.as_ref() {
            let (cols, rows) = get_win_size(tty);
            self.config.lock().unwrap().update_console_size(cols, rows);
            if self
                .acked_features
                .fetch_and(1u64 << VIRTIO_CONSOLE_F_SIZE, Ordering::AcqRel)
                != 0
            {
                // Send the interrupt to the driver
                let _ = self.config_evt.write(1);
            }
        }
    }
}

impl VirtioConsoleConfig {
    pub fn update_console_size(&mut self, cols: u16, rows: u16) {
        self.cols = cols;
        self.rows = rows;
    }
}

/// Virtio device for exposing console to the guest OS through virtio.
pub struct Console {
    common: VirtioCommon,
    id: String,
    config: Arc<Mutex<VirtioConsoleConfig>>,
    resizer: Arc<ConsoleResizer>,
    resize_pipe: Option<File>,
    endpoint: Endpoint,
    seccomp_action: SeccompAction,
    in_buffer: Arc<Mutex<VecDeque<u8>>>,
    exit_evt: EventFd,
}

#[derive(Serialize, Deserialize)]
pub struct ConsoleState {
    avail_features: u64,
    acked_features: u64,
    config: VirtioConsoleConfig,
    in_buffer: Vec<u8>,
}

fn get_win_size(tty: &dyn AsRawFd) -> (u16, u16) {
    #[repr(C)]
    #[derive(Default)]
    struct WindowSize {
        rows: u16,
        cols: u16,
        xpixel: u16,
        ypixel: u16,
    }
    let ws: WindowSize = WindowSize::default();

    unsafe {
        libc::ioctl(tty.as_raw_fd(), TIOCGWINSZ, &ws);
    }

    (ws.cols, ws.rows)
}
impl Console {
    /// Create a new virtio console device
    pub fn new(
        id: String,
        endpoint: Endpoint,
        resize_pipe: Option<File>,
        iommu: bool,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        state: Option<ConsoleState>,
    ) -> io::Result<(Console, Arc<ConsoleResizer>)> {
        let (avail_features, acked_features, config, in_buffer) = if let Some(state) = state {
            info!("Restoring virtio-console {}", id);
            (
                state.avail_features,
                state.acked_features,
                state.config,
                state.in_buffer.into(),
            )
        } else {
            let mut avail_features = 1u64 << VIRTIO_F_VERSION_1 | 1u64 << VIRTIO_CONSOLE_F_SIZE;
            if iommu {
                avail_features |= 1u64 << VIRTIO_F_IOMMU_PLATFORM;
            }

            (
                avail_features,
                0,
                VirtioConsoleConfig::default(),
                VecDeque::new(),
            )
        };

        let config_evt = EventFd::new(EFD_NONBLOCK).unwrap();
        let console_config = Arc::new(Mutex::new(config));
        let resizer = Arc::new(ConsoleResizer {
            config_evt,
            config: console_config.clone(),
            tty: endpoint.out_file().as_ref().map(|t| t.try_clone().unwrap()),
            acked_features: AtomicU64::new(acked_features),
        });

        resizer.update_console_size();

        Ok((
            Console {
                common: VirtioCommon {
                    device_type: VirtioDeviceType::Console as u32,
                    queue_sizes: QUEUE_SIZES.to_vec(),
                    avail_features,
                    acked_features,
                    paused_sync: Some(Arc::new(Barrier::new(2))),
                    min_queues: NUM_QUEUES as u16,
                    ..Default::default()
                },
                id,
                config: console_config,
                resizer: resizer.clone(),
                resize_pipe,
                endpoint,
                seccomp_action,
                in_buffer: Arc::new(Mutex::new(in_buffer)),
                exit_evt,
            },
            resizer,
        ))
    }

    fn state(&self) -> ConsoleState {
        ConsoleState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: *(self.config.lock().unwrap()),
            in_buffer: self.in_buffer.lock().unwrap().clone().into(),
        }
    }

    #[cfg(fuzzing)]
    pub fn wait_for_epoll_threads(&mut self) {
        self.common.wait_for_epoll_threads();
    }
}

impl Drop for Console {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
    }
}

impl VirtioDevice for Console {
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

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        self.read_config_from_slice(self.config.lock().unwrap().as_slice(), offset, data);
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        self.resizer
            .acked_features
            .store(self.common.acked_features, Ordering::Relaxed);

        if self.common.feature_acked(VIRTIO_CONSOLE_F_SIZE) {
            if let Err(e) = interrupt_cb.trigger(VirtioInterruptType::Config) {
                error!("Failed to signal console driver: {:?}", e);
            }
        }

        let (kill_evt, pause_evt) = self.common.dup_eventfds();

        let (_, input_queue, input_queue_evt) = queues.remove(0);
        let (_, output_queue, output_queue_evt) = queues.remove(0);

        let mut handler = ConsoleEpollHandler::new(
            mem,
            input_queue,
            output_queue,
            interrupt_cb,
            self.in_buffer.clone(),
            Arc::clone(&self.resizer),
            self.endpoint.clone(),
            input_queue_evt,
            output_queue_evt,
            self.resizer.config_evt.try_clone().unwrap(),
            self.resize_pipe.as_ref().map(|p| p.try_clone().unwrap()),
            kill_evt,
            pause_evt,
            self.common.access_platform.clone(),
        );

        let paused = self.common.paused.clone();
        let paused_sync = self.common.paused_sync.clone();
        let mut epoll_threads = Vec::new();

        spawn_virtio_thread(
            &self.id,
            &self.seccomp_action,
            Thread::VirtioConsole,
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

    fn set_access_platform(&mut self, access_platform: Arc<dyn AccessPlatform>) {
        self.common.set_access_platform(access_platform)
    }
}

impl Pausable for Console {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()
    }
}

impl Snapshottable for Console {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id, &self.state())
    }
}
impl Transportable for Console {}
impl Migratable for Console {}
