// Copyright 2019 Red Hat, Inc. All Rights Reserved.
//
// Portions Copyright 2019 Intel Corporation. All Rights Reserved.
//
// Portions Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
//
// SPDX-License-Identifier: (Apache-2.0 AND BSD-3-Clause)

#![allow(clippy::significant_drop_in_scrutinee)]

use block_util::{build_disk_image_id, Request, RequestType, VirtioBlockConfig};
use libc::EFD_NONBLOCK;
use log::*;
use option_parser::{OptionParser, OptionParserError, Toggle};
use qcow::{self, ImageType, QcowFile};
use std::fs::File;
use std::fs::OpenOptions;
use std::io::Read;
use std::io::{Seek, SeekFrom, Write};
use std::ops::Deref;
use std::ops::DerefMut;
use std::os::unix::fs::OpenOptionsExt;
use std::path::PathBuf;
use std::process;
use std::result;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, RwLock, RwLockWriteGuard};
use std::time::Instant;
use std::vec::Vec;
use std::{convert, error, fmt, io};
use vhost::vhost_user::message::*;
use vhost::vhost_user::Listener;
use vhost_user_backend::{
    bitmap::BitmapMmapRegion, VhostUserBackendMut, VhostUserDaemon, VringRwLock, VringState, VringT,
};
use virtio_bindings::bindings::virtio_blk::*;
use virtio_bindings::bindings::virtio_ring::VIRTIO_RING_F_EVENT_IDX;
use virtio_queue::QueueT;
use vm_memory::GuestAddressSpace;
use vm_memory::{ByteValued, Bytes, GuestMemoryAtomic};
use vmm_sys_util::{epoll::EventSet, eventfd::EventFd};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<BitmapMmapRegion>;

const SECTOR_SHIFT: u8 = 9;
const SECTOR_SIZE: u64 = 0x01 << SECTOR_SHIFT;
const BLK_SIZE: u32 = 512;
// Current (2020) enterprise SSDs have a latency lower than 30us.
// Polling for 50us should be enough to cover for the device latency
// and the overhead of the emulation layer.
const POLL_QUEUE_US: u128 = 50;

fn used_len(request_type: RequestType, guest_data_bytes: u32) -> u32 {
    match request_type {
        RequestType::In | RequestType::GetDeviceId => guest_data_bytes.saturating_add(1),
        RequestType::Out | RequestType::Flush | RequestType::Unsupported(_) => 1,
    }
}

trait DiskFile: Read + Seek + Write + Send {}
impl<D: Read + Seek + Write + Send> DiskFile for D {}

type Result<T> = std::result::Result<T, Error>;
type VhostUserBackendResult<T> = std::result::Result<T, std::io::Error>;

#[allow(dead_code)]
#[derive(Debug)]
enum Error {
    /// Failed to create kill eventfd
    CreateKillEventFd(io::Error),
    /// Failed to parse configuration string
    FailedConfigParse(OptionParserError),
    /// Failed to handle event other than input event.
    HandleEventNotEpollIn,
    /// Failed to handle unknown event.
    HandleEventUnknownEvent,
    /// No path provided
    PathParameterMissing,
    /// No socket provided
    SocketParameterMissing,
}

pub const SYNTAX: &str = "vhost-user-block backend parameters \
 \"path=<image_path>,socket=<socket_path>,num_queues=<number_of_queues>,\
 queue_size=<size_of_each_queue>,readonly=true|false,direct=true|false,\
 poll_queue=true|false\"";

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "vhost_user_block_error: {:?}", self)
    }
}

impl error::Error for Error {}

impl convert::From<Error> for io::Error {
    fn from(e: Error) -> Self {
        io::Error::new(io::ErrorKind::Other, e)
    }
}

struct VhostUserBlkThread {
    disk_image: Arc<Mutex<dyn DiskFile>>,
    disk_image_id: Vec<u8>,
    disk_nsectors: u64,
    event_idx: bool,
    kill_evt: EventFd,
    writeback: Arc<AtomicBool>,
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
}

impl VhostUserBlkThread {
    fn new(
        disk_image: Arc<Mutex<dyn DiskFile>>,
        disk_image_id: Vec<u8>,
        disk_nsectors: u64,
        writeback: Arc<AtomicBool>,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
    ) -> Result<Self> {
        Ok(VhostUserBlkThread {
            disk_image,
            disk_image_id,
            disk_nsectors,
            event_idx: false,
            kill_evt: EventFd::new(EFD_NONBLOCK).map_err(Error::CreateKillEventFd)?,
            writeback,
            mem,
        })
    }

    fn process_queue(
        &mut self,
        vring: &mut RwLockWriteGuard<VringState<GuestMemoryAtomic<GuestMemoryMmap>>>,
    ) -> bool {
        let mut used_descs = false;

        while let Some(mut desc_chain) = vring
            .get_queue_mut()
            .pop_descriptor_chain(self.mem.memory())
        {
            debug!("got an element in the queue");
            let len;
            match Request::parse(&mut desc_chain, None) {
                Ok(mut request) => {
                    debug!("element is a valid request");
                    request.set_writeback(self.writeback.load(Ordering::Acquire));
                    let status = match request.execute(
                        &mut self.disk_image.lock().unwrap().deref_mut(),
                        self.disk_nsectors,
                        desc_chain.memory(),
                        &self.disk_image_id,
                    ) {
                        Ok(l) => {
                            // `execute` only reports the byte count for `In`;
                            // `GetDeviceId` writes the ID to guest memory but
                            // returns 0, so account for it explicitly to mirror
                            // the async block backend.
                            let guest_data_bytes = match request.request_type {
                                RequestType::GetDeviceId => self.disk_image_id.len() as u32,
                                _ => l,
                            };
                            len = used_len(request.request_type, guest_data_bytes);
                            VIRTIO_BLK_S_OK
                        }
                        Err(e) => {
                            len = 1;
                            e.status()
                        }
                    };
                    desc_chain
                        .memory()
                        .write_obj(status, request.status_addr)
                        .unwrap();
                }
                Err(err) => {
                    error!("failed to parse available descriptor chain: {:?}", err);
                    len = 0;
                }
            }

            vring
                .get_queue_mut()
                .add_used(desc_chain.memory(), desc_chain.head_index(), len)
                .unwrap();
            used_descs = true;
        }

        let mut needs_signalling = false;
        if self.event_idx {
            if vring
                .get_queue_mut()
                .needs_notification(self.mem.memory().deref())
                .unwrap()
            {
                debug!("signalling queue");
                needs_signalling = true;
            } else {
                debug!("omitting signal (event_idx)");
            }
        } else {
            debug!("signalling queue");
            needs_signalling = true;
        }

        if needs_signalling {
            vring.signal_used_queue().unwrap();
        }

        used_descs
    }
}

struct VhostUserBlkBackend {
    threads: Vec<Mutex<VhostUserBlkThread>>,
    config: VirtioBlockConfig,
    rdonly: bool,
    poll_queue: bool,
    queues_per_thread: Vec<u64>,
    queue_size: usize,
    acked_features: u64,
    writeback: Arc<AtomicBool>,
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
}

impl VhostUserBlkBackend {
    fn new(
        image_path: String,
        num_queues: usize,
        rdonly: bool,
        direct: bool,
        poll_queue: bool,
        queue_size: usize,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
    ) -> Result<Self> {
        let mut options = OpenOptions::new();
        options.read(true);
        options.write(!rdonly);
        if direct {
            options.custom_flags(libc::O_DIRECT);
        }
        let image: File = options.open(&image_path).unwrap();
        let mut raw_img: qcow::RawFile = qcow::RawFile::new(image, direct);

        let image_id = build_disk_image_id(&PathBuf::from(&image_path));
        let image_type = qcow::detect_image_type(&mut raw_img).unwrap();
        let image = match image_type {
            ImageType::Raw => Arc::new(Mutex::new(raw_img)) as Arc<Mutex<dyn DiskFile>>,
            ImageType::Qcow2 => {
                Arc::new(Mutex::new(QcowFile::from(raw_img).unwrap())) as Arc<Mutex<dyn DiskFile>>
            }
        };

        let nsectors = (image.lock().unwrap().seek(SeekFrom::End(0)).unwrap()) / SECTOR_SIZE;
        let config = VirtioBlockConfig {
            capacity: nsectors,
            blk_size: BLK_SIZE,
            size_max: 65535,
            seg_max: 128 - 2,
            min_io_size: 1,
            opt_io_size: 1,
            num_queues: num_queues as u16,
            writeback: 1,
            ..Default::default()
        };

        let mut queues_per_thread = Vec::new();
        let mut threads = Vec::new();
        let writeback = Arc::new(AtomicBool::new(true));
        for i in 0..num_queues {
            let thread = Mutex::new(VhostUserBlkThread::new(
                image.clone(),
                image_id.clone(),
                nsectors,
                writeback.clone(),
                mem.clone(),
            )?);
            threads.push(thread);
            queues_per_thread.push(0b1 << i);
        }

        Ok(VhostUserBlkBackend {
            threads,
            config,
            rdonly,
            poll_queue,
            queues_per_thread,
            queue_size,
            acked_features: 0,
            writeback,
            mem,
        })
    }

    fn update_writeback(&mut self) {
        // Use writeback from config if VIRTIO_BLK_F_CONFIG_WCE
        let writeback =
            if self.acked_features & 1 << VIRTIO_BLK_F_CONFIG_WCE == 1 << VIRTIO_BLK_F_CONFIG_WCE {
                self.config.writeback == 1
            } else {
                // Else check if VIRTIO_BLK_F_FLUSH negotiated
                self.acked_features & 1 << VIRTIO_BLK_F_FLUSH == 1 << VIRTIO_BLK_F_FLUSH
            };

        info!(
            "Changing cache mode to {}",
            if writeback {
                "writeback"
            } else {
                "writethrough"
            }
        );
        self.writeback.store(writeback, Ordering::Release);
    }
}

impl VhostUserBackendMut for VhostUserBlkBackend {
    type Bitmap = BitmapMmapRegion;
    type Vring = VringRwLock<GuestMemoryAtomic<GuestMemoryMmap>>;
    fn num_queues(&self) -> usize {
        self.config.num_queues as usize
    }

    fn max_queue_size(&self) -> usize {
        self.queue_size
    }

    fn features(&self) -> u64 {
        let mut avail_features = 1 << VIRTIO_BLK_F_SEG_MAX
            | 1 << VIRTIO_BLK_F_BLK_SIZE
            | 1 << VIRTIO_BLK_F_FLUSH
            | 1 << VIRTIO_BLK_F_TOPOLOGY
            | 1 << VIRTIO_BLK_F_MQ
            | 1 << VIRTIO_BLK_F_CONFIG_WCE
            | 1 << VIRTIO_RING_F_EVENT_IDX
            | 1 << VIRTIO_F_VERSION_1
            | VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits();

        if self.rdonly {
            avail_features |= 1 << VIRTIO_BLK_F_RO;
        }
        avail_features
    }

    fn acked_features(&mut self, features: u64) {
        self.acked_features = features;
        self.update_writeback();
    }

    fn protocol_features(&self) -> VhostUserProtocolFeatures {
        VhostUserProtocolFeatures::CONFIG
            | VhostUserProtocolFeatures::MQ
            | VhostUserProtocolFeatures::CONFIGURE_MEM_SLOTS
    }

    fn set_event_idx(&mut self, enabled: bool) {
        for thread in self.threads.iter() {
            thread.lock().unwrap().event_idx = enabled;
        }
    }

    fn handle_event(
        &mut self,
        device_event: u16,
        evset: EventSet,
        vrings: &[VringRwLock<GuestMemoryAtomic<GuestMemoryMmap>>],
        thread_id: usize,
    ) -> VhostUserBackendResult<()> {
        if evset != EventSet::IN {
            return Err(Error::HandleEventNotEpollIn.into());
        }

        debug!("event received: {:?}", device_event);

        let mut thread = self.threads[thread_id].lock().unwrap();
        match device_event {
            0 => {
                let mut vring = vrings[0].get_mut();

                if self.poll_queue {
                    // Actively poll the queue until POLL_QUEUE_US has passed
                    // without seeing a new request.
                    let mut now = Instant::now();
                    loop {
                        if thread.process_queue(&mut vring) {
                            now = Instant::now();
                        } else if now.elapsed().as_micros() > POLL_QUEUE_US {
                            break;
                        }
                    }
                }

                if thread.event_idx {
                    // vm-virtio's Queue implementation only checks avail_index
                    // once, so to properly support EVENT_IDX we need to keep
                    // calling process_queue() until it stops finding new
                    // requests on the queue.
                    loop {
                        vring
                            .get_queue_mut()
                            .enable_notification(self.mem.memory().deref())
                            .unwrap();
                        if !thread.process_queue(&mut vring) {
                            break;
                        }
                    }
                } else {
                    // Without EVENT_IDX, a single call is enough.
                    thread.process_queue(&mut vring);
                }

                Ok(())
            }
            _ => Err(Error::HandleEventUnknownEvent.into()),
        }
    }

    fn get_config(&self, _offset: u32, _size: u32) -> Vec<u8> {
        self.config.as_slice().to_vec()
    }

    fn set_config(&mut self, offset: u32, data: &[u8]) -> result::Result<(), io::Error> {
        let config_slice = self.config.as_mut_slice();
        let data_len = data.len() as u32;
        let config_len = config_slice.len() as u32;
        if offset + data_len > config_len {
            error!("Failed to write config space");
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        let (_, right) = config_slice.split_at_mut(offset as usize);
        right.copy_from_slice(data);
        self.update_writeback();
        Ok(())
    }

    fn exit_event(&self, thread_index: usize) -> Option<EventFd> {
        Some(
            self.threads[thread_index]
                .lock()
                .unwrap()
                .kill_evt
                .try_clone()
                .unwrap(),
        )
    }

    fn queues_per_thread(&self) -> Vec<u64> {
        self.queues_per_thread.clone()
    }

    fn update_memory(
        &mut self,
        _mem: GuestMemoryAtomic<GuestMemoryMmap>,
    ) -> VhostUserBackendResult<()> {
        Ok(())
    }
}

struct VhostUserBlkBackendConfig {
    path: String,
    socket: String,
    num_queues: usize,
    queue_size: usize,
    readonly: bool,
    direct: bool,
    poll_queue: bool,
}

impl VhostUserBlkBackendConfig {
    fn parse(backend: &str) -> Result<Self> {
        let mut parser = OptionParser::new();
        parser
            .add("path")
            .add("readonly")
            .add("direct")
            .add("num_queues")
            .add("queue_size")
            .add("socket")
            .add("poll_queue");
        parser.parse(backend).map_err(Error::FailedConfigParse)?;

        let path = parser.get("path").ok_or(Error::PathParameterMissing)?;
        let readonly = parser
            .convert::<Toggle>("readonly")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(Toggle(false))
            .0;
        let direct = parser
            .convert::<Toggle>("direct")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(Toggle(false))
            .0;
        let num_queues = parser
            .convert("num_queues")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(1);
        let socket = parser.get("socket").ok_or(Error::SocketParameterMissing)?;
        let poll_queue = parser
            .convert::<Toggle>("poll_queue")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(Toggle(true))
            .0;
        let queue_size = parser
            .convert("queue_size")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(1024);

        Ok(VhostUserBlkBackendConfig {
            path,
            socket,
            num_queues,
            queue_size,
            readonly,
            direct,
            poll_queue,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn used_len_reports_bytes_written_to_guest() {
        assert_eq!(used_len(RequestType::In, 512), 513);
        assert_eq!(used_len(RequestType::Out, 0), 1);
        assert_eq!(used_len(RequestType::Flush, 0), 1);
        assert_eq!(used_len(RequestType::GetDeviceId, 20), 21);
        assert_eq!(used_len(RequestType::Unsupported(42), 0), 1);
    }
}

pub fn start_block_backend(backend_command: &str) {
    let backend_config = match VhostUserBlkBackendConfig::parse(backend_command) {
        Ok(config) => config,
        Err(e) => {
            println!("Failed parsing parameters {:?}", e);
            process::exit(1);
        }
    };

    let mem = GuestMemoryAtomic::new(GuestMemoryMmap::new());

    let blk_backend = Arc::new(RwLock::new(
        VhostUserBlkBackend::new(
            backend_config.path,
            backend_config.num_queues,
            backend_config.readonly,
            backend_config.direct,
            backend_config.poll_queue,
            backend_config.queue_size,
            mem.clone(),
        )
        .unwrap(),
    ));

    debug!("blk_backend is created!\n");

    let listener = Listener::new(&backend_config.socket, true).unwrap();

    let name = "vhost-user-blk-backend";
    let mut blk_daemon = VhostUserDaemon::new(name.to_string(), blk_backend.clone(), mem).unwrap();

    debug!("blk_daemon is created!\n");

    if let Err(e) = blk_daemon.start(listener) {
        error!(
            "Failed to start daemon for vhost-user-block with error: {:?}\n",
            e
        );
        process::exit(1);
    }

    if let Err(e) = blk_daemon.wait() {
        error!("Error from the main thread: {:?}", e);
    }

    for thread in blk_backend.read().unwrap().threads.iter() {
        if let Err(e) = thread.lock().unwrap().kill_evt.write(1) {
            error!("Error shutting down worker thread: {:?}", e)
        }
    }
}
