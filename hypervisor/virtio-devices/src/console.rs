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
use std::os::unix::io::{AsRawFd, RawFd};
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
// The output endpoint became writable again after having refused data.
const OUTPUT_FLUSH_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 6;

// Upper bound on how much console output is kept in memory while the consumer
// on the other end of the console is not draining it. Console output is
// best-effort, so past this point the oldest bytes are dropped instead of
// letting the buffer grow without bound. Same policy and size as the
// SerialBuffer used by the PTY path.
const MAX_PENDING_OUT: usize = 1 << 20;

// Trimming happens in bulks of this size. Dropping the oldest bytes of a Vec
// is a memmove of everything that stays behind, so trimming on every descriptor
// would copy the whole buffer once per descriptor while the consumer is stalled.
// The buffer is therefore allowed to overshoot the cap by this much before it is
// brought back down to it.
const PENDING_OUT_TRIM: usize = 64 << 10;

// Upper bound on the size of a single write handed to the output endpoint.
//
// The console output endpoint is an AF_UNIX SOCK_DGRAM socket (the shim points
// the VMM's stdout at a UnixDatagram pair), so one write here is one datagram
// and its size is a contract with the reader at the other end:
//
//  - The shim receives into a fixed 1024-byte buffer
//    (CubeShim/shim/src/log/mod.rs) with a plain recv(), so a larger datagram
//    is truncated and the excess dropped without an error.
//  - On a datagram socket the size check runs before the queue-full check, so
//    a write larger than SO_SNDBUF - 32 (~208 KiB with the default 212992) is
//    refused with EMSGSIZE rather than WouldBlock.
//
// Chunking at the reader's buffer size keeps every write under both limits,
// whatever size the guest's descriptors are.
const MAX_WRITE_CHUNK: usize = 1024;

//Console size feature bit
const VIRTIO_CONSOLE_F_SIZE: u64 = 0;

// The console has exactly one failure left that is worth taking the device - and
// with it the whole VM - down for: failing to hand a descriptor back to the
// guest, which is queue corruption that no amount of retrying repairs. Every
// other failure on this path (a descriptor chain the guest posted malformed, a
// guest buffer that cannot be reached, an output endpoint that is full or gone)
// is reported and skipped, because none of it is worth killing the guest for.
// Upstream cloud-hypervisor reduced this enum to the same single variant.
#[derive(Error, Debug)]
enum Error {
    #[error("Failed to add used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
}

// Whether a failed console write is worth retrying once the endpoint drains.
//
// Console output is best-effort, so this is not about which errors are grave:
// none of them are allowed to take the VM down. It only decides whether the
// bytes are kept queued (the endpoint is momentarily full and EPOLLOUT will
// bring us back) or dropped (the endpoint will not accept them however long we
// wait, so holding on to them would just stall the buffer until it overruns).
fn is_retryable(err: &io::Error) -> bool {
    if matches!(
        err.kind(),
        io::ErrorKind::WouldBlock        // EAGAIN: the consumer's queue is full
            | io::ErrorKind::Interrupted // EINTR: the write never happened
            | io::ErrorKind::OutOfMemory // ENOMEM
    ) {
        return true;
    }
    // std does not give ENOBUFS its own ErrorKind, but it is a transient kernel
    // memory shortage and belongs with the retryable errors.
    err.raw_os_error() == Some(libc::ENOBUFS)
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
    // Raw fd of the output endpoint. Used to subscribe to EPOLLOUT while the
    // endpoint is refusing data, so the retry is event driven rather than
    // relying on the guest to kick the queue again.
    out_fd: Option<RawFd>,
    // Console output the endpoint refused to accept because its buffer was
    // full. Drained by OUTPUT_FLUSH_EVENT.
    pending_out: Vec<u8>,
    // Whether OUTPUT_FLUSH_EVENT is currently registered with the epoll loop.
    out_flush_registered: bool,
    // Whether the current overrun episode has already been reported. The guest
    // can keep producing while the consumer is stalled, so without this the
    // drop path would log once per descriptor.
    out_overrun_warned: bool,
    // Whether the current endpoint-rejection episode has already been reported.
    // Same reasoning as `out_overrun_warned`: a permanently broken endpoint
    // would otherwise produce one log line per descriptor.
    out_drop_warned: bool,
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

        // Take the fd before `endpoint` is moved into the handler below.
        let out_fd = out_file.map(|f| f.as_raw_fd());

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
            out_fd,
            pending_out: Vec::new(),
            out_flush_registered: false,
            out_overrun_warned: false,
            out_drop_warned: false,
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
            // A malformed chain, or one whose buffer has gone away, is reported
            // and skipped rather than failing the queue: the chain is still
            // returned to the guest below, so the driver is not left waiting on
            // it forever. Input the guest did not get is simply delivered on the
            // next pass, since it is only taken out of the buffer once written.
            let mut len = 0;
            if let Some(desc) = desc_chain.next() {
                len = cmp::min(desc.len(), in_buffer.len() as u32);
                let source_slice = in_buffer
                    .range(..len as usize)
                    .copied()
                    .collect::<Vec<u8>>();

                if let Err(e) = desc_chain.memory().write_slice(
                    &source_slice[..],
                    desc.addr()
                        .translate_gva(self.access_platform.as_ref(), desc.len() as usize),
                ) {
                    warn!("Failed to write to receiveq descriptor: {e}");
                    len = 0;
                } else {
                    in_buffer.drain(..len as usize);
                }
            } else {
                warn!("Skipping empty descriptor chain on receiveq");
            }

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
        // Split the borrow explicitly: the output endpoint is touched from
        // inside the loop, so it cannot be reached through `self` while the
        // transmit queue is borrowed.
        let Self {
            mem,
            output_queue,
            out,
            pending_out,
            out_overrun_warned,
            out_drop_warned,
            access_platform,
            ..
        } = self;
        let mut used_descs = false;

        while let Some(mut desc_chain) = output_queue.pop_descriptor_chain(mem.memory()) {
            // A malformed chain, or one whose buffer has gone away, costs the
            // guest that line of output and nothing else: the chain is still
            // returned to the guest below, so the driver keeps running.
            let mut desc_len = 0;
            if let Some(desc) = desc_chain.next() {
                desc_len = desc.len();
                if out.is_some() {
                    let mut buf: Vec<u8> = Vec::new();
                    match desc_chain.memory().write_volatile_to(
                        desc.addr()
                            .translate_gva(access_platform.as_ref(), desc.len() as usize),
                        &mut buf,
                        desc.len() as usize,
                    ) {
                        Ok(_written) => Self::write_output(
                            out,
                            pending_out,
                            out_overrun_warned,
                            out_drop_warned,
                            &buf,
                        ),
                        Err(e) => warn!("Failed to read from transmitq descriptor: {e}"),
                    }
                }
            } else {
                warn!("Skipping empty descriptor chain on transmitq");
            }

            output_queue
                .add_used(desc_chain.memory(), desc_chain.head_index(), desc_len)
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;
        }

        Ok(used_descs)
    }

    // Queue console output and hand as much of it as possible to the
    // (non-blocking) endpoint.
    //
    // Console output is best-effort: a full output buffer only means the
    // consumer on the other end is momentarily slow. Treating that as a device
    // error used to abort the console epoll loop and shut the whole VM down,
    // so anything the endpoint refuses is kept in `pending_out` instead and
    // written once OUTPUT_FLUSH_EVENT reports the fd is writable again.
    //
    // This deliberately has no error return: there is no console output failure
    // worth taking a guest down for, so every failure is either retried or
    // dropped. See `drain_pending_output`.
    fn write_output(
        out: &mut Option<Box<dyn Write + Send>>,
        pending_out: &mut Vec<u8>,
        overrun_warned: &mut bool,
        drop_warned: &mut bool,
        data: &[u8],
    ) {
        pending_out.extend_from_slice(data);

        if pending_out.len() > MAX_PENDING_OUT + PENDING_OUT_TRIM {
            let dropped = pending_out.len() - MAX_PENDING_OUT;
            pending_out.drain(..dropped);
            // Report the first drop of an episode only: a consumer that stays
            // stalled would otherwise produce one log line per descriptor.
            if !*overrun_warned {
                warn!(
                    "Console output overrun, dropped {} bytes of console output",
                    dropped
                );
                *overrun_warned = true;
            }
        } else if pending_out.len() <= MAX_PENDING_OUT {
            // The buffer is back within its nominal size, so the next overrun is
            // a new episode and deserves its own line.
            *overrun_warned = false;
        }

        Self::drain_pending_output(out, pending_out, drop_warned);
    }

    // Write `pending_out` to the endpoint, stopping at the first byte the
    // endpoint refuses. Returns whether the buffer ended up empty.
    //
    // No outcome here is fatal. A failure is either retryable, in which case
    // the bytes stay queued and EPOLLOUT brings us back, or it is terminal for
    // the endpoint, in which case the bytes are dropped and the guest keeps
    // running. Returning an error would abort the console worker and shut the
    // whole VM down, which is how a slow log consumer used to kill guests.
    fn drain_pending_output(
        out: &mut Option<Box<dyn Write + Send>>,
        pending_out: &mut Vec<u8>,
        drop_warned: &mut bool,
    ) -> bool {
        let Some(writer) = out.as_mut() else {
            pending_out.clear();
            return true;
        };

        while !pending_out.is_empty() {
            // Bounded so that no single write can exceed what the datagram
            // endpoint is willing to accept; see MAX_WRITE_CHUNK.
            let chunk = pending_out.len().min(MAX_WRITE_CHUNK);
            match writer.write(&pending_out[..chunk]) {
                Ok(0) => {
                    // The endpoint accepts nothing right now; wait for EPOLLOUT
                    // rather than spinning on it.
                    return false;
                }
                Ok(written) => {
                    pending_out.drain(..written);
                }
                Err(e) if is_retryable(&e) => return false,
                Err(e) => {
                    // EMSGSIZE, EPIPE, ECONNREFUSED, EIO...: the endpoint will
                    // not take these bytes however long we wait, so drop them
                    // and carry on rather than killing the guest.
                    if !*drop_warned {
                        warn!("Dropping console output, endpoint refused it: {}", e);
                        *drop_warned = true;
                    }
                    pending_out.clear();
                    return true;
                }
            }
        }

        // A failed flush is not worth taking the guest down for either.
        if let Err(e) = writer.flush() {
            warn!("Failed to flush console output: {}", e);
        }
        // The buffer is empty again, so the next rejection is a new episode.
        *drop_warned = false;
        true
    }

    // Keep the EPOLLOUT subscription in sync with whether console output is
    // still pending. An idle datagram socket reports EPOLLOUT continuously, so
    // subscribing only while the endpoint is refusing data is what keeps this
    // from becoming a busy loop.
    //
    // For an AF_UNIX datagram socket, unix_dgram_poll() clears EPOLLOUT while
    // the peer's receive queue is full -- the same condition that makes the
    // write return EAGAIN -- and wakes the sender when the peer next reads, so
    // the subscription tracks exactly what blocked the write.
    //
    // A failed epoll_ctl is logged and ignored: without the subscription a
    // blocked write is retried on the next descriptor from the guest instead.
    fn update_output_flush_event(&mut self, helper: &mut EpollHelper, drained: bool) {
        let Some(fd) = self.out_fd else {
            return;
        };

        let result = if drained {
            if !self.out_flush_registered {
                return;
            }
            helper.del_event_custom(fd, OUTPUT_FLUSH_EVENT, epoll::Events::EPOLLOUT)
        } else {
            if self.out_flush_registered {
                return;
            }
            helper.add_event_custom(fd, OUTPUT_FLUSH_EVENT, epoll::Events::EPOLLOUT)
        };

        match result {
            Ok(()) => self.out_flush_registered = !drained,
            Err(e) => warn!("Failed to update console output flush event: {:?}", e),
        }
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
                let drained = self.pending_out.is_empty();
                self.update_output_flush_event(helper, drained);
            }
            OUTPUT_FLUSH_EVENT => {
                let drained = ConsoleEpollHandler::drain_pending_output(
                    &mut self.out,
                    &mut self.pending_out,
                    &mut self.out_drop_warned,
                );
                self.update_output_flush_event(helper, drained);
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
