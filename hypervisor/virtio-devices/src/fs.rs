// Copyright 2019 Intel Corporation. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use super::RateLimiterConfig;
use crate::seccomp_filters::Thread;
use crate::thread_helper::spawn_virtio_thread;
use crate::Error as DeviceError;
use crate::VirtioInterruptType;
use crate::{
    ActivateError, ActivateResult, EpollHelper, EpollHelperError, EpollHelperHandler,
    UserspaceMapping, VirtioCommon, VirtioDevice, VirtioDeviceType, VirtioInterrupt,
    VirtioSharedMemoryList, EPOLL_HELPER_EVENT_LAST, VIRTIO_F_IN_ORDER, VIRTIO_F_IOMMU_PLATFORM,
    VIRTIO_F_NOTIFICATION_DATA, VIRTIO_F_ORDER_PLATFORM, VIRTIO_F_RING_INDIRECT_DESC,
    VIRTIO_F_VERSION_1,
};
use crate::{GuestMemoryMmap, MmapRegion};
use anyhow::anyhow;
use rate_limiter::{BucketReduction, RateLimiter, TokenType};
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use serde_with::{serde_as, Bytes};
use std::fs;
use std::io;
use std::num::Wrapping;
use std::os::unix::io::{AsRawFd, RawFd};
use std::result;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Barrier, Mutex};
use std::time::Instant;
use std::{collections::HashMap, convert::TryInto};
use thiserror::Error;
use virtio_queue::{Queue, QueueOwnedT, QueueT};
use virtiofsd::{
    descriptor_utils::{Error as VufDescriptorError, Reader, Writer},
    filesystem::SerializableFileSystem,
    fs_cache_req_handler::FsCacheReqHandler,
    fuse::RemovemappingOne,
    limits,
    passthrough::{
        self, read_only::PassthroughFsRo, xattrmap::XattrMap, CachePolicy, PassthroughFs,
    },
    server::Server,
    Error as VhostUserFsError,
};
use vm_memory::{ByteValued, GuestAddressSpace, GuestMemoryAtomic};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vmm_sys_util::eventfd::EventFd;

const NUM_QUEUE_OFFSET: usize = 1;
const DEFAULT_QUEUE_NUMBER: usize = 2;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Processing queue failed: {0}")]
    ProcessQueue(VhostUserFsError),
    #[error("Creating a queue reader failed: {0}")]
    QueueReader(VufDescriptorError),
    #[error("Creating a queue writer failed: {0}")]
    QueueWriter(VufDescriptorError),
}

pub type Result<T> = result::Result<T, Error>;

pub const BACKEND_FS_CACHE_AUTO: u8 = 0;
pub const BACKEND_FS_CACHE_ALWAYS: u8 = 1;
pub const BACKEND_FS_CACHE_NEVER: u8 = 2;
pub const BACKEND_FS_CACHE_NONE: u8 = 3;
pub const DEFAULT_FS_LIMIT_NOFILE: u64 = 0;
pub const DEFAULT_FS_THREAD_POOL_SIZE: usize = 0;

pub fn default_fsconfig_cache() -> u8 {
    BACKEND_FS_CACHE_NEVER
}

pub fn default_fsconfig_no_readdirplus() -> bool {
    true
}

pub fn default_fsconfig_read_only() -> bool {
    true
}

pub fn default_fsconfig_rlimit_nofile() -> u64 {
    DEFAULT_FS_LIMIT_NOFILE
}

pub fn default_fsconfig_thread_pool_size() -> usize {
    DEFAULT_FS_THREAD_POOL_SIZE
}

pub fn default_fsconfig_xattr() -> bool {
    true
}

#[derive(Serialize, Deserialize)]
pub struct State {
    pub avail_features: u64,
    pub acked_features: u64,
    pub config: VirtioFsConfig,
    #[serde(default)]
    pub back_state: Vec<u8>,
}

#[serde_as]
#[derive(Copy, Clone, Serialize, Deserialize)]
#[repr(C, packed)]
pub struct VirtioFsConfig {
    #[serde_as(as = "Bytes")]
    pub tag: [u8; 36],
    pub num_request_queues: u32,
}

impl Default for VirtioFsConfig {
    fn default() -> Self {
        VirtioFsConfig {
            tag: [0; 36],
            num_request_queues: 0,
        }
    }
}

#[derive(Default, Clone)]
pub struct FsCounters {
    total_bytes: Arc<AtomicU64>,
    total_ops: Arc<AtomicU64>,
    min_latency: Arc<AtomicU64>,
    max_latency: Arc<AtomicU64>,
    avg_latency: Arc<AtomicU64>,
    limit_by_bytes: Arc<AtomicU64>,
    limit_by_ops: Arc<AtomicU64>,
}

#[derive(Clone)]
pub struct FsEvent {
    pub tx: std::sync::mpsc::Sender<bool>,
    pub backendfs_config: BackendFsConfig,
}

#[derive(Clone)]
struct CacheHandler {
    cache_size: u64,
    mmap_cache_addr: u64,
}

impl CacheHandler {
    // Make sure request is within cache range
    fn is_req_valid(&self, offset: u64, len: u64) -> bool {
        let end = match offset.checked_add(len) {
            Some(n) => n,
            None => return false,
        };

        !(offset >= self.cache_size || end > self.cache_size)
    }
}

impl FsCacheReqHandler for CacheHandler {
    fn map(
        &mut self,
        foffset: u64,
        moffset: u64,
        len: u64,
        flags: u64,
        fd: RawFd,
    ) -> result::Result<(), io::Error> {
        debug!("fs_slave_map");

        // Ignore if the length is 0.
        if len == 0 {
            return Ok(());
        }

        if !self.is_req_valid(moffset, len) {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }

        let addr = self.mmap_cache_addr + moffset;
        // SAFETY: FFI call with valid arguments
        let ret = unsafe {
            libc::mmap(
                addr as *mut libc::c_void,
                len as usize,
                flags as i32,
                libc::MAP_SHARED | libc::MAP_FIXED,
                fd.as_raw_fd(),
                foffset as libc::off_t,
            )
        };
        if ret == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }

        Ok(())
    }

    fn unmap(&mut self, requests: Vec<RemovemappingOne>) -> std::result::Result<(), io::Error> {
        debug!("fs_slave_unmap");

        for req in requests {
            let mut offset = req.moffset;
            let mut len = req.len;

            // Ignore if the length is 0.
            if len == 0 {
                continue;
            }

            // Need to handle a special case where the slave ask for the unmapping
            // of the entire mapping.
            if len == 0xffff_ffff_ffff_ffff {
                len = self.cache_size;
                offset = 0;
            }

            if !self.is_req_valid(offset, len) {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }

            let addr = self.mmap_cache_addr + offset;
            // SAFETY: FFI call with valid arguments
            let ret = unsafe {
                libc::mmap(
                    addr as *mut libc::c_void,
                    len as usize,
                    libc::PROT_NONE,
                    libc::MAP_ANONYMOUS | libc::MAP_PRIVATE | libc::MAP_FIXED,
                    -1,
                    0,
                )
            };
            if ret == libc::MAP_FAILED {
                return Err(io::Error::last_os_error());
            }
        }

        Ok(())
    }
}

struct FsEpollHandler {
    queue_index: u16,
    queue_evt: EventFd,
    queue: Queue,
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    kill_evt: EventFd,
    pause_evt: EventFd,
    server: Arc<ServerType>,
    cache_handler: Option<CacheHandler>,
    dev_evt: EventFd,
    pending_dev_message: Arc<Mutex<Vec<FsEvent>>>,
    rate_limiter: Option<RateLimiter>,
    counters: FsCounters,
    rate_limited: std::sync::Once,
    id: String,
}

// New descriptors are pending on the virtio queue.
const QUEUE_AVAIL_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
// New 'wake up' event from the rate limiter
const RATE_LIMITER_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 3;
// New event to backend device.
const DEVICE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 4;

// latency scale, for reduce precision loss in calculate.
const LATENCY_SCALE: u64 = 10000;

impl FsEpollHandler {
    fn run(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
    ) -> result::Result<(), EpollHelperError> {
        let mut helper = EpollHelper::new(&self.kill_evt, &self.pause_evt)?;

        helper.add_event(self.queue_evt.as_raw_fd(), QUEUE_AVAIL_EVENT)?;
        if let Some(rate_limiter) = &self.rate_limiter {
            helper.add_event(rate_limiter.as_raw_fd(), RATE_LIMITER_EVENT)?;
        }
        // There always multi queue support for virtiofs, we only
        // need one thread to listen the DEVICE_EVENT.
        // Here we just add DEVICE_EVENT to queue which index is 0,
        // because this queue is always exist.
        if self.queue_index == 0 {
            helper.add_event(self.dev_evt.as_raw_fd(), DEVICE_EVENT)?;
        }
        helper.run(paused, paused_sync, self)?;

        Ok(())
    }

    fn signal_used_queue(&self) -> result::Result<(), DeviceError> {
        self.interrupt_cb
            .trigger(VirtioInterruptType::Queue(self.queue_index))
            .map_err(|e| {
                error!("Failed to signal used queue: {:?}", e);
                DeviceError::FailedSignalingUsedQueue(e)
            })
    }

    fn return_descriptor(queue: &mut Queue, mem: &GuestMemoryMmap, head_index: u16, len: usize) {
        let used_len: u32 = match len.try_into() {
            Ok(l) => l,
            Err(_) => panic!("Invalid used length, can't return used descriptors to the ring"),
        };

        if queue.add_used(mem, head_index, used_len).is_err() {
            warn!("Couldn't return used descriptors to the ring");
        }
    }

    fn process_queue_serial(&mut self) -> Result<bool> {
        let queue = &mut self.queue;
        let mut cache_handler = self.cache_handler.clone();
        let mut used_descs = false;
        let mut total_ops = Wrapping(0);
        let mut total_bytes = Wrapping(0);
        let ops_last = self.counters.total_ops.load(Ordering::Relaxed) as i64;
        let mut avg_latency = self.counters.avg_latency.load(Ordering::Relaxed) as i64;
        let mut limit_by_ops = Wrapping(0);
        let mut limit_by_bytes = Wrapping(0);

        while let Some(desc_chain) = queue.pop_descriptor_chain(self.mem.memory()) {

            let head_index = desc_chain.head_index();

            let reader = Reader::new(desc_chain.memory(), desc_chain.clone())
                .map_err(Error::QueueReader)
                .unwrap();
            let writer = Writer::new(desc_chain.memory(), desc_chain.clone())
                .map_err(Error::QueueWriter)
                .unwrap();

            let mut rate_limited = BucketReduction::Success;
            let mut rate_limited_type = TokenType::Ops;
            let req_start = Instant::now();
            let len = self
                .server
                .handle_message(
                    reader,
                    writer,
                    cache_handler.as_mut(),
                    &mut self.rate_limiter,
                    &mut rate_limited,
                    &mut rate_limited_type,
                )
                .map_err(Error::ProcessQueue)?;

            if rate_limited != BucketReduction::Success {
                match rate_limited_type {
                    TokenType::Ops => {
                        limit_by_ops += Wrapping(1);
                        self.rate_limited
                            .call_once(|| info!("{} fs ops ratelimit fired", self.id));
                    }
                    TokenType::Bytes => {
                        limit_by_bytes += Wrapping(1);
                        self.rate_limited
                            .call_once(|| info!("{} fs bw ratelimit fired", self.id));
                    }
                }

                // Stop processing the queue and return this descriptor chain to the
                // avail ring, for later processing.
                if rate_limited == BucketReduction::Failure {
                    queue.go_to_previous_position();
                    break;
                }
            }

            let latency = req_start.elapsed().as_micros() as u64;
            total_bytes += Wrapping(len as u64);
            total_ops += Wrapping(1);
            if (ops_last == 0) || (latency < self.counters.min_latency.load(Ordering::Relaxed)) {
                self.counters.min_latency.store(latency, Ordering::Relaxed);
            }
            if latency > self.counters.max_latency.load(Ordering::Relaxed) {
                self.counters.max_latency.store(latency, Ordering::Relaxed);
            }
            if latency > 20_000 {
                warn!("Virtio-fs IO latency too long {}", latency);
            }
            avg_latency = avg_latency
                + ((latency * LATENCY_SCALE) as i64 - avg_latency)
                    / (ops_last + total_ops.0 as i64);
            Self::return_descriptor(queue, desc_chain.memory(), head_index, len);
            used_descs = true;

            // Process BucketReduction::OverConsumption.
            if let BucketReduction::OverConsumption(_) = rate_limited {
                break;
            }
        }

        self.counters
            .total_bytes
            .fetch_add(total_bytes.0, Ordering::AcqRel);
        self.counters
            .total_ops
            .fetch_add(total_ops.0, Ordering::AcqRel);
        self.counters
            .avg_latency
            .store(avg_latency as u64, Ordering::Relaxed);
        self.counters
            .limit_by_bytes
            .fetch_add(limit_by_bytes.0, Ordering::AcqRel);
        self.counters
            .limit_by_ops
            .fetch_add(limit_by_ops.0, Ordering::AcqRel);

        Ok(used_descs)
    }

    fn handle_event_impl(&mut self) -> result::Result<(), EpollHelperError> {
        let needs_notification = self.process_queue_serial().map_err(|e| {
            EpollHelperError::HandleEvent(anyhow!("Failed to process queue (submit): {:?}", e))
        })?;

        if needs_notification {
            self.signal_used_queue().map_err(|e| {
                EpollHelperError::HandleEvent(anyhow!("Failed to signal used queue: {:?}", e))
            })?
        };

        Ok(())
    }
}

impl EpollHelperHandler for FsEpollHandler {
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
                let rate_limit_reached =
                    self.rate_limiter.as_ref().map_or(false, |r| r.is_blocked());

                // Process the queue only when the rate limit is not reached
                if !rate_limit_reached {
                    self.handle_event_impl()?
                }
            }
            RATE_LIMITER_EVENT => {
                if let Some(rate_limiter) = &mut self.rate_limiter {
                    // Upon rate limiter event, call the rate limiter handler
                    // and restart processing the queue.
                    rate_limiter.event_handler().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to process rate limiter event: {:?}",
                            e
                        ))
                    })?;

                    self.handle_event_impl()?
                } else {
                    return Err(EpollHelperError::HandleEvent(anyhow!(
                        "Unexpected 'RATE_LIMITER_EVENT' when rate_limiter is not enabled."
                    )));
                }
            }
            DEVICE_EVENT => {
                self.dev_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!("Failed to get queue event: {:?}", e))
                })?;
                for event in self.pending_dev_message.lock().unwrap().drain(..) {
                    info!(
                        "Fs device update with configuration {:?}",
                        event.backendfs_config
                    );
                    let ret = self
                        .server
                        .update_filter(&event.backendfs_config.allowed_dirs)
                        .map_or_else(
                            |e| {
                                info!("Failed to update filter: {:?}", e);
                                false
                            },
                            |_| true,
                        );
                    let _ = event.tx.send(ret);
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

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct BackendFsConfig {
    #[serde(default)]
    pub shared_dir: String,
    #[serde(default = "default_fsconfig_thread_pool_size")]
    pub thread_pool_size: usize,
    #[serde(default = "default_fsconfig_xattr")]
    pub xattr: bool,
    #[serde(default)]
    pub posix_acl: bool,
    #[serde(default)]
    pub xattrmap: Option<String>,
    #[serde(default)]
    pub announce_submounts: bool,
    #[serde(default = "default_fsconfig_cache")]
    pub cache: u8,
    #[serde(default = "default_fsconfig_no_readdirplus")]
    pub no_readdirplus: bool,
    #[serde(default)]
    pub writeback: bool,
    #[serde(default)]
    pub allow_direct_io: bool,
    #[serde(default = "default_fsconfig_read_only")]
    pub read_only: bool,
    #[serde(default = "default_fsconfig_rlimit_nofile")]
    pub rlimit_nofile: u64,
    #[serde(default)]
    pub killpriv_v2: bool,
    #[serde(default)]
    pub security_label: bool,
    #[serde(default)]
    pub allowed_dirs: Option<Vec<String>>,
}

impl Default for BackendFsConfig {
    fn default() -> Self {
        BackendFsConfig {
            shared_dir: "".to_owned(),
            thread_pool_size: default_fsconfig_thread_pool_size(),
            xattr: default_fsconfig_xattr(),
            posix_acl: false,
            xattrmap: None,
            announce_submounts: false,
            cache: default_fsconfig_cache(),
            no_readdirplus: default_fsconfig_no_readdirplus(),
            writeback: false,
            allow_direct_io: false,
            read_only: default_fsconfig_read_only(),
            rlimit_nofile: default_fsconfig_rlimit_nofile(),
            killpriv_v2: false,
            security_label: false,
            allowed_dirs: None,
        }
    }
}

// SAFETY: only a series of integers
unsafe impl ByteValued for VirtioFsConfig {}

enum ServerType {
    PassthroughFs(Server<PassthroughFs>),
    PassthroughFsRo(Server<PassthroughFsRo>),
}

// Macro to forward method calls to the underlying server variant
// Supports both explicit parameter passing and automatic forwarding
macro_rules! forward_to_server {
    // Pattern: forward method with explicit arguments
    ($self:expr, $method:ident $(, $args:expr)*) => {
        match $self {
            ServerType::PassthroughFs(server) => server.$method($($args),*),
            ServerType::PassthroughFsRo(server) => server.$method($($args),*),
        }
    };
}

// Macro to define a method that forwards to the server with automatic parameter passing
macro_rules! impl_forward_method {
    // Pattern: method without generic parameters
    (
        $(#[$attr:meta])*
        $vis:vis fn $method:ident
        ( &$self:ident $(, $param:ident: $param_ty:ty)* ) -> $ret:ty
    ) => {
        $(#[$attr])*
        $vis fn $method ( &$self $(, $param: $param_ty)* ) -> $ret
        {
            forward_to_server!($self, $method $(, $param)*)
        }
    };

    // Pattern: method with generic parameters (using where clause)
    (
        $(#[$attr:meta])*
        $vis:vis fn $method:ident <$($gen:ident),+>
        ( &$self:ident $(, $param:ident: $param_ty:ty)* ) -> $ret:ty
        where $($bounds:tt)+
    ) => {
        $(#[$attr])*
        $vis fn $method <$($gen),+> ( &$self $(, $param: $param_ty)* ) -> $ret
        where $($bounds)+
        {
            forward_to_server!($self, $method $(, $param)*)
        }
    };
}

impl ServerType {
    impl_forward_method! {
        #[allow(dead_code)] // Used by virtiofsd migration (not yet enabled in open branch)
        fn prepare_serialization(&self, cancel: Arc<AtomicBool>) -> io::Result<()>
    }

    impl_forward_method! {
        #[allow(dead_code)] // Used by virtiofsd migration (not yet enabled in open branch)
        fn serialize_data(&self) -> io::Result<Vec<u8>>
    }

    impl_forward_method! {
        fn deserialize_and_apply_data(&self, data: &Vec<u8>) -> io::Result<()>
    }

    impl_forward_method! {
        fn update_filter(&self, whitelist: &Option<Vec<String>>) -> virtiofsd::Result<()>
    }

    impl_forward_method! {
        #[allow(clippy::cognitive_complexity)]
        pub fn handle_message<T>(
            &self,
            r: Reader,
            w: Writer,
            vu_req: Option<&mut T>,
            rl: &mut Option<RateLimiter>,
            rlret: &mut BucketReduction,
            rltyp: &mut TokenType
        ) -> virtiofsd::Result<usize>
        where T: FsCacheReqHandler
    }
}

pub struct Fs {
    common: VirtioCommon,
    id: String,
    config: VirtioFsConfig,
    seccomp_action: SeccompAction,
    exit_evt: EventFd,
    backendfs_config: BackendFsConfig,
    cache: Option<(VirtioSharedMemoryList, MmapRegion)>,
    rate_limiter_config: Option<RateLimiterConfig>,
    dev_evt: EventFd,
    pending_dev_message: Arc<Mutex<Vec<FsEvent>>>,
    counters: FsCounters,
    server: Option<Arc<ServerType>>,
    back_state: Vec<u8>,
    restore: bool,
    #[allow(dead_code)] // Used by virtiofsd migration (not yet enabled in open branch)
    pre_serialization: AtomicBool,
}

impl Fs {
    /// Create a new virtio-fs device.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        tag: &str,
        req_num_queues: usize,
        queue_size: u16,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        iommu: bool,
        state: Option<State>,
        backendfs_config: &BackendFsConfig,
        cache: Option<(VirtioSharedMemoryList, MmapRegion)>,
        rate_limiter_config: Option<RateLimiterConfig>,
        dev_evt: EventFd,
        pending_dev_message: Arc<Mutex<Vec<FsEvent>>>,
    ) -> io::Result<Fs> {
        let mut back_state = Vec::new();
        // Calculate the actual number of queues needed.
        let num_queues = NUM_QUEUE_OFFSET + req_num_queues;
        if num_queues > DEFAULT_QUEUE_NUMBER {
            error!(
                "virtio-fs requested too many queues ({}) since the backend only supports {}\n",
                num_queues, DEFAULT_QUEUE_NUMBER
            );
            return Err(io::Error::new(
                io::ErrorKind::Other,
                format!("requested too many queues"),
            ));
        }

        let (avail_features, acked_features, config, paused) = if let Some(state) = state {
            debug!("Restoring virtio-fs {}", id);
            back_state = state.back_state;

            (
                state.avail_features,
                state.acked_features,
                state.config,
                true,
            )
        } else {
            // Filling device and vring features VMM supports.
            let mut avail_features: u64 = 1 << VIRTIO_F_RING_INDIRECT_DESC
                | 1 << VIRTIO_F_VERSION_1
                | 1 << VIRTIO_F_IN_ORDER
                | 1 << VIRTIO_F_ORDER_PLATFORM
                | 1 << VIRTIO_F_NOTIFICATION_DATA;

            if iommu {
                avail_features |= 1 << VIRTIO_F_IOMMU_PLATFORM;
            }

            // Create virtio-fs device configuration.
            let mut config = VirtioFsConfig::default();
            let tag_bytes_vec = tag.to_string().into_bytes();
            config.tag[..tag_bytes_vec.len()].copy_from_slice(tag_bytes_vec.as_slice());
            config.num_request_queues = req_num_queues as u32;

            (avail_features, 0, config, false)
        };
        Ok(Fs {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Fs as u32,
                avail_features,
                acked_features,
                queue_sizes: vec![queue_size; num_queues],
                paused_sync: Some(Arc::new(Barrier::new(num_queues + 1))),
                min_queues: 1,
                paused: Arc::new(AtomicBool::new(paused)),
                ..Default::default()
            },
            id,
            config,
            seccomp_action,
            exit_evt,
            backendfs_config: backendfs_config.clone(),
            cache,
            rate_limiter_config,
            dev_evt,
            pending_dev_message,
            counters: FsCounters::default(),
            server: None,
            back_state,
            restore: paused,
            pre_serialization: AtomicBool::new(false),
        })
    }

    fn state(&self) -> State {
        State {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: self.config,
            back_state: Vec::new(),
        }
    }

    fn init_backend_fs_server(
        &self,
        backendfs_config: &BackendFsConfig,
    ) -> result::Result<Arc<ServerType>, ActivateError> {
        let shared_dir = &backendfs_config.shared_dir;
        let shared_dir_rp =
            fs::canonicalize(shared_dir).map_err(ActivateError::ActivateVirtioFs)?;
        let shared_dir_rp_str = shared_dir_rp
            .to_str()
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))
            .map_err(ActivateError::ActivateVirtioFs)?;

        let xattrmap = backendfs_config
            .xattrmap
            .as_ref()
            .map(|s| XattrMap::try_from(s.as_str()).unwrap());

        let cache = match backendfs_config.cache {
            BACKEND_FS_CACHE_AUTO => CachePolicy::Auto,
            BACKEND_FS_CACHE_ALWAYS => CachePolicy::Always,
            BACKEND_FS_CACHE_NEVER => CachePolicy::Never,
            BACKEND_FS_CACHE_NONE => CachePolicy::None,
            num => {
                return Err(ActivateError::ActivateVirtioFs(io::Error::other(format!(
                    "unknown cache policy: {}, valid input: 0(auto), 1(always), 2(never), 3(none)",
                    num
                ))))
            }
        };
        let readdirplus = match cache {
            CachePolicy::Never => false,
            CachePolicy::None => false,
            _ => !backendfs_config.no_readdirplus,
        };

        limits::setup_rlimit_nofile(Some(backendfs_config.rlimit_nofile))
            .map_err(|error| io::Error::other(format!("setup rlimit nofile error {:?}", error)))
            .map_err(ActivateError::ActivateVirtioFs)?;

        let fs_cfg = passthrough::Config {
            cache_policy: cache,
            root_dir: shared_dir_rp_str.into(),
            mountinfo_prefix: None,
            xattr: backendfs_config.xattr,
            xattrmap,
            proc_sfd_rawfd: None,
            proc_mountinfo_rawfd: None,
            announce_submounts: backendfs_config.announce_submounts,
            readdirplus,
            writeback: backendfs_config.writeback,
            allow_direct_io: backendfs_config.allow_direct_io,
            killpriv_v2: backendfs_config.killpriv_v2,
            security_label: backendfs_config.security_label,
            posix_acl: backendfs_config.posix_acl,
            ..Default::default()
        };

        if backendfs_config.read_only {
            let fs = PassthroughFsRo::new(fs_cfg).map_err(|e| {
                ActivateError::ActivateVirtioFs(io::Error::other(format!(
                    "Failed to create internal ro filesystem representation: {e:?}",
                )))
            })?;
            Ok(Arc::new(ServerType::PassthroughFsRo(
                Server::new(fs, &self.backendfs_config.allowed_dirs)
                    .map_err(ActivateError::InvalidServerConfig)?,
            )))
        } else {
            let fs = PassthroughFs::new(fs_cfg).map_err(|e| {
                ActivateError::ActivateVirtioFs(io::Error::other(format!(
                    "Failed to create internal filesystem representation: {e:?}",
                )))
            })?;
            Ok(Arc::new(ServerType::PassthroughFs(
                Server::new(fs, &self.backendfs_config.allowed_dirs)
                    .map_err(ActivateError::InvalidServerConfig)?,
            )))
        }
    }
}

impl Drop for Fs {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
    }
}

impl VirtioDevice for Fs {
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
        self.read_config_from_slice(self.config.as_slice(), offset, data);
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        let server = self.init_backend_fs_server(&self.backendfs_config)?;
        if self.restore {
            if !self.back_state.is_empty() {
                let start = Instant::now();
                server
                    .deserialize_and_apply_data(self.back_state.as_ref())
                    .map_err(|e| {
                        warn!("{} deserialization failed ({:?})", self.id, e);
                        ActivateError::InvalidVirtioFsState(e)
                    })?;
                info!(
                    "{} deserialization success after {}(us)",
                    self.id,
                    start.elapsed().as_micros()
                );
            }
            self.restore = false;
        }
        self.server = Some(server.clone());
        let mut epoll_threads = Vec::new();
        for i in 0..queues.len() {
            let (_, queue, queue_evt) = queues.remove(0);
            let (kill_evt, pause_evt) = self.common.dup_eventfds();

            let rate_limiter: Option<RateLimiter> = self
                .rate_limiter_config
                .map(RateLimiterConfig::try_into)
                .transpose()
                .map_err(ActivateError::CreateRateLimiter)?;

            let cache_handler = if let Some(cache) = self.cache.as_ref() {
                let handler = CacheHandler {
                    cache_size: cache.0.len,
                    mmap_cache_addr: cache.0.host_addr,
                };

                Some(handler)
            } else {
                None
            };
            let mut handler = FsEpollHandler {
                queue_index: i as u16,
                queue_evt,
                queue,
                mem: mem.clone(),
                interrupt_cb: interrupt_cb.clone(),
                kill_evt,
                pause_evt,
                server: server.clone(),
                cache_handler,
                dev_evt: self.dev_evt.try_clone().unwrap(),
                pending_dev_message: self.pending_dev_message.clone(),
                rate_limiter,
                counters: self.counters.clone(),
                rate_limited: std::sync::Once::new(),
                id: self.id.clone(),
            };

            let paused = self.common.paused.clone();
            let paused_sync = self.common.paused_sync.clone();

            spawn_virtio_thread(
                &format!("{}_q{}", self.id.clone(), i),
                &self.seccomp_action,
                Thread::VirtioFs,
                &mut epoll_threads,
                &self.exit_evt,
                move || handler.run(paused, paused_sync.unwrap()),
            )?;
        }

        self.common.epoll_threads = Some(epoll_threads);
        event!("virtio-device", "activated", "id", &self.id);

        Ok(())
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        // We first must resume the virtio thread if it was paused.
        if self.common.pause_evt.take().is_some() {
            self.common.resume().ok()?;
        }

        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }

        event!("virtio-device", "reset", "id", &self.id);

        // Return the interrupt
        Some(self.common.interrupt_cb.take().unwrap())
    }

    fn get_shm_regions(&self) -> Option<VirtioSharedMemoryList> {
        self.cache.as_ref().map(|cache| cache.0.clone())
    }

    fn set_shm_regions(
        &mut self,
        shm_regions: VirtioSharedMemoryList,
    ) -> std::result::Result<(), crate::Error> {
        if let Some(cache) = self.cache.as_mut() {
            cache.0 = shm_regions;
            Ok(())
        } else {
            Err(crate::Error::SetShmRegionsNotSupported)
        }
    }

    fn userspace_mappings(&self) -> Vec<UserspaceMapping> {
        let mut mappings = Vec::new();
        if let Some(cache) = self.cache.as_ref() {
            mappings.push(UserspaceMapping {
                host_addr: cache.0.host_addr,
                mem_slot: cache.0.mem_slot,
                addr: cache.0.addr,
                len: cache.0.len,
                mergeable: false,
            })
        }

        mappings
    }

    fn counters(&self) -> Option<HashMap<&'static str, Wrapping<u64>>> {
        let mut counters = HashMap::new();

        counters.insert(
            "total_bytes",
            Wrapping(self.counters.total_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "total_ops",
            Wrapping(self.counters.total_ops.load(Ordering::Acquire)),
        );
        counters.insert(
            "min_latency",
            Wrapping(self.counters.min_latency.load(Ordering::Acquire)),
        );
        counters.insert(
            "max_latency",
            Wrapping(self.counters.max_latency.load(Ordering::Acquire)),
        );
        counters.insert(
            "avg_latency",
            Wrapping(self.counters.avg_latency.load(Ordering::Acquire) / LATENCY_SCALE),
        );
        counters.insert(
            "limit_by_bytes",
            Wrapping(self.counters.limit_by_bytes.load(Ordering::Acquire)),
        );
        counters.insert(
            "limit_by_ops",
            Wrapping(self.counters.limit_by_ops.load(Ordering::Acquire)),
        );

        Some(counters)
    }
}

impl Pausable for Fs {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()
    }
}

impl Snapshottable for Fs {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        let mut state = self.state();
        if let Some(server) = &self.server {
            let start = Instant::now();
            server
                .serialize_data()
                .map_err(|e| {
                    warn!("{} serialization failed ({:?})", self.id, e);
                    MigratableError::Snapshot(anyhow!("serialize process failed: {:?}", e))
                })
                .map(|data| {
                    state.back_state = data;
                })?;
            info!(
                "{} serialization success after {}(us)",
                self.id,
                start.elapsed().as_micros()
            );
        }
        Snapshot::new_from_state(&self.id(), &state)
    }
}

impl Transportable for Fs {}

impl Migratable for Fs {
    fn start_migration(&mut self) -> std::result::Result<(), MigratableError> {
        if let Some(server) = &self.server {
            let start = Instant::now();
            let cancel = Arc::new(AtomicBool::new(false));
            debug!("serialize prepare");
            server.prepare_serialization(cancel).map_err(|e| {
                warn!("{} preserialization failed ({:?})", self.id, e);
                MigratableError::Snapshot(anyhow!("serialize prepare failed: {:?}", e))
            })?;
            info!(
                "{} preserialization success after {}(us)",
                self.id,
                start.elapsed().as_micros()
            );
        }

        Ok(())
    }
}
