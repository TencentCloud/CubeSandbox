// Copyright 2019 Intel Corporation. All Rights Reserved.
//
// Portions Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
//
// SPDX-License-Identifier: (Apache-2.0 AND BSD-3-Clause)

#![allow(clippy::significant_drop_in_scrutinee)]

use libc::{self, EFD_NONBLOCK};
use log::*;
use net_util::{
    open_tap, MacAddr, NetCounters, NetQueuePair, OpenTapError, RxVirtio, Tap, TxVirtio,
};
use option_parser::Toggle;
use option_parser::{OptionParser, OptionParserError};
use std::fmt;
use std::io;
use std::net::Ipv4Addr;
use std::ops::Deref;
use std::os::unix::io::{AsRawFd, RawFd};
use std::process;
use std::sync::{Arc, Mutex, RwLock};
use std::vec::Vec;
use vhost::vhost_user::message::*;
use vhost::vhost_user::Listener;
use vhost_user_backend::bitmap::BitmapMmapRegion;
use vhost_user_backend::{VhostUserBackendMut, VhostUserDaemon, VringRwLock, VringT};
use virtio_bindings::bindings::virtio_net::*;
use vm_memory::GuestAddressSpace;
use vm_memory::GuestMemoryAtomic;
use vmm_sys_util::{epoll::EventSet, eventfd::EventFd};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<BitmapMmapRegion>;

pub type Result<T> = std::result::Result<T, Error>;
type VhostUserBackendResult<T> = std::result::Result<T, std::io::Error>;

#[derive(Debug)]
pub enum Error {
    /// Failed to create kill eventfd.
    CreateKillEventFd(io::Error),
    /// Failed to parse configuration string.
    FailedConfigParse(OptionParserError),
    /// Failed to signal used queue.
    FailedSignalingUsedQueue(io::Error),
    /// Failed to handle event other than input event.
    HandleEventNotEpollIn,
    /// Failed to handle unknown event.
    HandleEventUnknownEvent,
    /// Failed to open tap device.
    OpenTap(OpenTapError),
    /// No socket provided.
    SocketParameterMissing,
    /// Underlying QueuePair error.
    NetQueuePair(net_util::NetQueuePairError),
    /// Failed to register the TAP listener.
    RegisterTapListener(io::Error),
}

pub const SYNTAX: &str = "vhost-user-net backend parameters \
\"ip=<ip_addr>,mask=<net_mask>,socket=<socket_path>,client=on|off,\
num_queues=<number_of_queues>,queue_size=<size_of_each_queue>,tap=<if_name>\"";

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "vhost_user_net_error: {:?}", self)
    }
}

impl std::error::Error for Error {}

impl std::convert::From<Error> for std::io::Error {
    fn from(e: Error) -> Self {
        std::io::Error::new(io::ErrorKind::Other, e)
    }
}

struct VhostUserNetThread {
    net: NetQueuePair,
    kill_evt: EventFd,
}

impl VhostUserNetThread {
    /// Create a new virtio network device with the given TAP interface.
    fn new(tap: Tap) -> Result<Self> {
        Ok(VhostUserNetThread {
            kill_evt: EventFd::new(EFD_NONBLOCK).map_err(Error::CreateKillEventFd)?,
            net: NetQueuePair {
                tap_for_write_epoll: tap.clone(),
                tap,
                rx: RxVirtio::new(),
                tx: TxVirtio::new(),
                rx_tap_listening: false,
                tx_tap_listening: false,
                epoll_fd: None,
                counters: NetCounters::default(),
                tap_rx_event_id: 3,
                tap_tx_event_id: 4,
                rx_desc_avail: false,
                rx_rate_limiter: None,
                tx_rate_limiter: None,
                access_platform: None,
                rx_rate_limited: std::sync::Once::new(),
                tx_rate_limited: std::sync::Once::new(),
                id: "".to_string(),
            },
        })
    }

    pub fn set_epoll_fd(&mut self, fd: RawFd) {
        self.net.epoll_fd = Some(fd);
    }
}

pub struct VhostUserNetBackend {
    threads: Vec<Mutex<VhostUserNetThread>>,
    num_queues: usize,
    queue_size: u16,
    queues_per_thread: Vec<u64>,
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
}

impl VhostUserNetBackend {
    #[allow(clippy::too_many_arguments)]
    fn new(
        ip_addr: Ipv4Addr,
        host_mac: MacAddr,
        netmask: Ipv4Addr,
        mtu: Option<u16>,
        num_queues: usize,
        queue_size: u16,
        ifname: Option<&str>,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
    ) -> Result<Self> {
        let mut taps = open_tap(
            ifname,
            Some(ip_addr),
            Some(netmask),
            &mut Some(host_mac),
            mtu,
            num_queues / 2,
            None,
            None,
        )
        .map_err(Error::OpenTap)?;

        let mut queues_per_thread = Vec::new();
        let mut threads = Vec::new();
        for (i, tap) in taps.drain(..).enumerate() {
            let thread = Mutex::new(VhostUserNetThread::new(tap)?);
            threads.push(thread);
            queues_per_thread.push(0b11 << (i * 2));
        }

        Ok(VhostUserNetBackend {
            threads,
            num_queues,
            queue_size,
            queues_per_thread,
            mem,
        })
    }
}

impl VhostUserBackendMut for VhostUserNetBackend {
    type Bitmap = BitmapMmapRegion;
    type Vring = VringRwLock<GuestMemoryAtomic<GuestMemoryMmap>>;
    fn num_queues(&self) -> usize {
        self.num_queues
    }

    fn max_queue_size(&self) -> usize {
        self.queue_size as usize
    }

    fn features(&self) -> u64 {
        1 << VIRTIO_NET_F_GUEST_CSUM
            | 1 << VIRTIO_NET_F_CSUM
            | 1 << VIRTIO_NET_F_GUEST_TSO4
            | 1 << VIRTIO_NET_F_GUEST_TSO6
            | 1 << VIRTIO_NET_F_GUEST_ECN
            | 1 << VIRTIO_NET_F_GUEST_UFO
            | 1 << VIRTIO_NET_F_HOST_TSO4
            | 1 << VIRTIO_NET_F_HOST_TSO6
            | 1 << VIRTIO_NET_F_HOST_ECN
            | 1 << VIRTIO_NET_F_HOST_UFO
            | 1 << VIRTIO_NET_F_CTRL_VQ
            | 1 << VIRTIO_NET_F_MQ
            | 1 << VIRTIO_NET_F_MAC
            | 1 << VIRTIO_NET_F_MTU
            | 1 << VIRTIO_F_NOTIFY_ON_EMPTY
            | 1 << VIRTIO_F_VERSION_1
            | VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits()
    }

    fn protocol_features(&self) -> VhostUserProtocolFeatures {
        VhostUserProtocolFeatures::MQ
            | VhostUserProtocolFeatures::REPLY_ACK
            | VhostUserProtocolFeatures::CONFIGURE_MEM_SLOTS
    }

    fn set_event_idx(&mut self, _enabled: bool) {}

    fn handle_event(
        &mut self,
        device_event: u16,
        _evset: EventSet,
        vrings: &[VringRwLock<GuestMemoryAtomic<GuestMemoryMmap>>],
        thread_id: usize,
    ) -> VhostUserBackendResult<()> {
        let mut thread = self.threads[thread_id].lock().unwrap();
        match device_event {
            0 => {
                if !thread.net.rx_tap_listening {
                    net_util::register_listener(
                        thread.net.epoll_fd.unwrap(),
                        thread.net.tap.as_raw_fd(),
                        epoll::Events::EPOLLIN,
                        u64::from(thread.net.tap_rx_event_id),
                    )
                    .map_err(Error::RegisterTapListener)?;
                    thread.net.rx_tap_listening = true;
                }
            }
            1 | 4 => {
                let mut vring = vrings[1].get_mut();
                if thread
                    .net
                    .process_tx(self.mem.memory().deref(), vring.get_queue_mut())
                    .map_err(Error::NetQueuePair)?
                {
                    vring
                        .signal_used_queue()
                        .map_err(Error::FailedSignalingUsedQueue)?
                }
            }
            3 => {
                let mut vring = vrings[0].get_mut();
                if thread
                    .net
                    .process_rx(self.mem.memory().deref(), vring.get_queue_mut())
                    .map_err(Error::NetQueuePair)?
                {
                    vring
                        .signal_used_queue()
                        .map_err(Error::FailedSignalingUsedQueue)?
                }
            }
            _ => return Err(Error::HandleEventUnknownEvent.into()),
        }

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

pub struct VhostUserNetBackendConfig {
    pub ip: Ipv4Addr,
    pub host_mac: MacAddr,
    pub mask: Ipv4Addr,
    pub mtu: Option<u16>,
    pub socket: String,
    pub num_queues: usize,
    pub queue_size: u16,
    pub tap: Option<String>,
    pub client: bool,
}

impl VhostUserNetBackendConfig {
    pub fn parse(backend: &str) -> Result<Self> {
        let mut parser = OptionParser::new();

        parser
            .add("tap")
            .add("ip")
            .add("host_mac")
            .add("mask")
            .add("mtu")
            .add("queue_size")
            .add("num_queues")
            .add("socket")
            .add("client");

        parser.parse(backend).map_err(Error::FailedConfigParse)?;

        let tap = parser.get("tap");
        let ip = parser
            .convert("ip")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or_else(|| Ipv4Addr::new(192, 168, 100, 1));
        let host_mac = parser
            .convert("host_mac")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or_else(MacAddr::local_random);
        let mask = parser
            .convert("mask")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or_else(|| Ipv4Addr::new(255, 255, 255, 0));
        let mtu = parser.convert("mtu").map_err(Error::FailedConfigParse)?;
        let queue_size = parser
            .convert("queue_size")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(256);
        let num_queues = parser
            .convert("num_queues")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(2);
        let socket = parser.get("socket").ok_or(Error::SocketParameterMissing)?;
        let client = parser
            .convert::<Toggle>("client")
            .map_err(Error::FailedConfigParse)?
            .unwrap_or(Toggle(false))
            .0;

        Ok(VhostUserNetBackendConfig {
            ip,
            host_mac,
            mask,
            mtu,
            socket,
            num_queues,
            queue_size,
            tap,
            client,
        })
    }
}

pub fn start_net_backend(backend_command: &str) {
    let backend_config = match VhostUserNetBackendConfig::parse(backend_command) {
        Ok(config) => config,
        Err(e) => {
            eprintln!("Failed parsing parameters {:?}", e);
            process::exit(1);
        }
    };

    let tap = backend_config.tap.as_deref();

    let mem = GuestMemoryAtomic::new(GuestMemoryMmap::new());

    let net_backend = Arc::new(RwLock::new(
        VhostUserNetBackend::new(
            backend_config.ip,
            backend_config.host_mac,
            backend_config.mask,
            backend_config.mtu,
            backend_config.num_queues,
            backend_config.queue_size,
            tap,
            mem.clone(),
        )
        .unwrap(),
    ));

    let mut net_daemon = VhostUserDaemon::new(
        "vhost-user-net-backend".to_string(),
        net_backend.clone(),
        mem,
    )
    .unwrap();

    let epoll_handlers = net_daemon.get_epoll_handlers();
    if epoll_handlers.len() != net_backend.read().unwrap().threads.len() {
        error!("Number of vring workers must be identical to the number of backend threads");
        process::exit(1);
    }

    for (index, thread) in net_backend.read().unwrap().threads.iter().enumerate() {
        thread
            .lock()
            .unwrap()
            .set_epoll_fd(epoll_handlers[index].as_raw_fd());
    }

    if let Err(e) = if backend_config.client {
        net_daemon.start_client(&backend_config.socket)
    } else {
        net_daemon.start(Listener::new(&backend_config.socket, true).unwrap())
    } {
        error!(
            "failed to start daemon for vhost-user-net with error: {:?}",
            e
        );
        process::exit(1);
    }

    if let Err(e) = net_daemon.wait() {
        error!("Error from the main thread: {:?}", e);
    }

    for thread in net_backend.read().unwrap().threads.iter() {
        if let Err(e) = thread.lock().unwrap().kill_evt.write(1) {
            error!("Error shutting down worker thread: {:?}", e)
        }
    }
}
