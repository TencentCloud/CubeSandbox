// Copyright 2019 Intel Corporation. All Rights Reserved.
//
// SPDX-License-Identifier: (Apache-2.0 AND BSD-3-Clause)

use futures::executor::{ThreadPool, ThreadPoolBuilder};
use libc::EFD_NONBLOCK;
use log::*;
use passthrough::xattrmap::XattrMap;
use std::collections::HashSet;
use std::convert::{self, TryFrom, TryInto};
use std::ffi::CString;
use std::fs::File;
use std::os::unix::io::{FromRawFd, RawFd};
use std::path::Path;
use std::str::FromStr;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::thread::{self, JoinHandle};
use std::time::Duration;
use std::{env, error, fmt, io, process};
use virtiofsd::idmap::{GidMap, UidMap};

use clap::{CommandFactory, Parser};

use vhost::vhost_user::message::*;
use vhost::vhost_user::Error::Disconnected;
use vhost::vhost_user::{Backend, Listener};
use vhost_user_backend::bitmap::BitmapMmapRegion;
use vhost_user_backend::Error::HandleRequest;
use vhost_user_backend::{VhostUserBackend, VhostUserDaemon, VringMutex, VringState, VringT};
use virtio_bindings::bindings::virtio_config::*;
use virtio_bindings::bindings::virtio_ring::{
    VIRTIO_RING_F_EVENT_IDX, VIRTIO_RING_F_INDIRECT_DESC,
};
use virtio_queue::{DescriptorChain, QueueOwnedT};
use virtiofsd::descriptor_utils::{Error as VufDescriptorError, Reader, Writer};
use virtiofsd::filesystem::{FileSystem, SerializableFileSystem};
use virtiofsd::passthrough::{
    self, CachePolicy, InodeFileHandlesMode, MigrationMode, MigrationOnError, PassthroughFs,
};
use virtiofsd::sandbox::{Sandbox, SandboxMode};
use virtiofsd::seccomp::{enable_seccomp, SeccompAction};
use virtiofsd::server::Server;
use virtiofsd::util::{other_io_error, write_pid_file};
use virtiofsd::{limits, oslib, Error as VhostUserFsError};
use vm_memory::{
    ByteValued, GuestAddressSpace, GuestMemoryAtomic, GuestMemoryLoadGuard, GuestMemoryMmap, Le32,
};
use vmm_sys_util::epoll::EventSet;
use vmm_sys_util::eventfd::EventFd;

const QUEUE_SIZE: usize = 32768;
// The spec allows for multiple request queues. We currently only support one.
const REQUEST_QUEUES: u32 = 1;
// In addition to the request queue there is one high-prio queue.
// Since VIRTIO_FS_F_NOTIFICATION is not advertised we do not have a
// notification queue.
const NUM_QUEUES: usize = REQUEST_QUEUES as usize + 1;

type LoggedMemory = GuestMemoryMmap<BitmapMmapRegion>;
type LoggedMemoryAtomic = GuestMemoryAtomic<LoggedMemory>;

// The guest queued an available buffer for the high priority queue.
const HIPRIO_QUEUE_EVENT: u16 = 0;
// The guest queued an available buffer for the request queue.
const REQ_QUEUE_EVENT: u16 = 1;

const MAX_TAG_LEN: usize = 36;

type Result<T> = std::result::Result<T, Error>;
type VhostUserBackendResult<T> = std::result::Result<T, std::io::Error>;

// The compiler warns that some wrapped values are never read, but they are in fact read by
// `<Error as fmt::Display>::fmt()` via the derived `Debug`.
#[allow(dead_code)]
#[derive(Debug)]
enum Error {
    /// Failed to create kill eventfd.
    CreateKillEventFd(io::Error),
    /// Failed to create thread pool.
    CreateThreadPool(io::Error),
    /// Failed to handle event other than input event.
    HandleEventNotEpollIn,
    /// Failed to handle unknown event.
    HandleEventUnknownEvent,
    /// Iterating through the queue failed.
    IterateQueue,
    /// No memory configured.
    NoMemoryConfigured,
    /// Processing queue failed.
    ProcessQueue(VhostUserFsError),
    /// Creating a queue reader failed.
    QueueReader(VufDescriptorError),
    /// Creating a queue writer failed.
    QueueWriter(VufDescriptorError),
    /// The unshare(CLONE_FS) call failed.
    UnshareCloneFs(io::Error),
    /// Invalid tag name
    InvalidTag,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        use self::Error::UnshareCloneFs;
        match self {
            UnshareCloneFs(error) => {
                write!(
                    f,
                    "The unshare(CLONE_FS) syscall failed with '{error}'. \
                    If running in a container please check that the container \
                    runtime seccomp policy allows unshare."
                )
            }
            Self::InvalidTag => write!(
                f,
                "The tag may not be empty or longer than {MAX_TAG_LEN} bytes (encoded as UTF-8)."
            ),
            _ => write!(f, "{self:?}"),
        }
    }
}

impl error::Error for Error {}

impl convert::From<Error> for io::Error {
    fn from(e: Error) -> Self {
        other_io_error(e)
    }
}

struct VhostUserFsThread<F: FileSystem + Send + Sync + 'static> {
    mem: Option<LoggedMemoryAtomic>,
    kill_evt: EventFd,
    server: Arc<Server<F>>,
    // handle request from backend to frontend
    vu_req: Option<Backend>,
    event_idx: bool,
    pool: Option<ThreadPool>,
}

impl<F: FileSystem + Send + Sync + 'static> Clone for VhostUserFsThread<F> {
    fn clone(&self) -> Self {
        VhostUserFsThread {
            mem: self.mem.clone(),
            kill_evt: self.kill_evt.try_clone().unwrap(),
            server: self.server.clone(),
            vu_req: self.vu_req.clone(),
            event_idx: self.event_idx,
            pool: self.pool.clone(),
        }
    }
}

impl<F: FileSystem + SerializableFileSystem + Send + Sync + 'static> VhostUserFsThread<F> {
    fn new(fs: F, thread_pool_size: usize) -> Result<Self> {
        let pool = if thread_pool_size > 0 {
            // Test that unshare(CLONE_FS) works, it will be called for each thread.
            // It's an unprivileged system call but some Docker/Moby versions are
            // known to reject it via seccomp when CAP_SYS_ADMIN is not given.
            //
            // Note that the program is single-threaded here so this syscall has no
            // visible effect and is safe to make.
            let ret = unsafe { libc::unshare(libc::CLONE_FS) };
            if ret == -1 {
                return Err(Error::UnshareCloneFs(std::io::Error::last_os_error()));
            }

            Some(
                ThreadPoolBuilder::new()
                    .after_start(|_| {
                        // unshare FS for xattr operation
                        let ret = unsafe { libc::unshare(libc::CLONE_FS) };
                        assert_eq!(ret, 0); // Should not fail
                    })
                    .pool_size(thread_pool_size)
                    .create()
                    .map_err(Error::CreateThreadPool)?,
            )
        } else {
            None
        };

        Ok(VhostUserFsThread {
            mem: None,
            kill_evt: EventFd::new(EFD_NONBLOCK).map_err(Error::CreateKillEventFd)?,
            server: Arc::new(Server::new(fs, &None).unwrap()),
            vu_req: None,
            event_idx: false,
            pool,
        })
    }

    fn return_descriptor(
        vring_state: &mut VringState<LoggedMemoryAtomic>,
        head_index: u16,
        event_idx: bool,
        len: usize,
    ) {
        let used_len: u32 = match len.try_into() {
            Ok(l) => l,
            Err(_) => panic!("Invalid used length, can't return used descriptors to the ring"),
        };

        if vring_state.add_used(head_index, used_len).is_err() {
            warn!("Couldn't return used descriptors to the ring");
        }

        if event_idx {
            match vring_state.needs_notification() {
                Err(_) => {
                    warn!("Couldn't check if queue needs to be notified");
                    vring_state.signal_used_queue().unwrap();
                }
                Ok(needs_notification) => {
                    if needs_notification {
                        vring_state.signal_used_queue().unwrap();
                    }
                }
            }
        } else {
            vring_state.signal_used_queue().unwrap();
        }
    }

    fn process_queue_pool(&self, vring: VringMutex<LoggedMemoryAtomic>) -> Result<bool> {
        let mut used_any = false;
        let atomic_mem = match &self.mem {
            Some(m) => m,
            None => return Err(Error::NoMemoryConfigured),
        };

        while let Some(avail_desc) = vring
            .get_mut()
            .get_queue_mut()
            .iter(atomic_mem.memory())
            .map_err(|_| Error::IterateQueue)?
            .next()
        {
            used_any = true;

            // Prepare a set of objects that can be moved to the worker thread.
            let atomic_mem = atomic_mem.clone();
            let server = self.server.clone();
            let mut vu_req = self.vu_req.clone();
            let event_idx = self.event_idx;
            let worker_vring = vring.clone();
            let worker_desc = avail_desc.clone();

            self.pool.as_ref().unwrap().spawn_ok(async move {
                let mem = atomic_mem.memory();
                let head_index = worker_desc.head_index();

                let reader = Reader::new(&mem, worker_desc.clone())
                    .map_err(Error::QueueReader)
                    .unwrap();
                let writer = Writer::new(&mem, worker_desc.clone())
                    .map_err(Error::QueueWriter)
                    .unwrap();

                let len = server
                    .handle_message(reader, writer, vu_req.as_mut())
                    .map_err(Error::ProcessQueue)
                    .unwrap();

                Self::return_descriptor(&mut worker_vring.get_mut(), head_index, event_idx, len);
            });
        }

        Ok(used_any)
    }

    fn process_queue_serial(
        &self,
        vring_state: &mut VringState<LoggedMemoryAtomic>,
    ) -> Result<bool> {
        let mut used_any = false;
        let mem = match &self.mem {
            Some(m) => m.memory(),
            None => return Err(Error::NoMemoryConfigured),
        };
        let mut vu_req = self.vu_req.clone();

        let avail_chains: Vec<DescriptorChain<GuestMemoryLoadGuard<LoggedMemory>>> = vring_state
            .get_queue_mut()
            .iter(mem.clone())
            .map_err(|_| Error::IterateQueue)?
            .collect();

        for chain in avail_chains {
            used_any = true;

            let head_index = chain.head_index();

            let reader = Reader::new(&mem, chain.clone())
                .map_err(Error::QueueReader)
                .unwrap();
            let writer = Writer::new(&mem, chain.clone())
                .map_err(Error::QueueWriter)
                .unwrap();

            let len = self
                .server
                .handle_message(reader, writer, vu_req.as_mut())
                .map_err(Error::ProcessQueue)
                .unwrap();

            Self::return_descriptor(vring_state, head_index, self.event_idx, len);
        }

        Ok(used_any)
    }

    fn handle_event_pool(
        &self,
        device_event: u16,
        vrings: &[VringMutex<LoggedMemoryAtomic>],
    ) -> VhostUserBackendResult<()> {
        let idx = match device_event {
            HIPRIO_QUEUE_EVENT => {
                debug!("HIPRIO_QUEUE_EVENT");
                0
            }
            REQ_QUEUE_EVENT => {
                debug!("QUEUE_EVENT");
                1
            }
            _ => return Err(Error::HandleEventUnknownEvent.into()),
        };

        if self.event_idx {
            // vm-virtio's Queue implementation only checks avail_index
            // once, so to properly support EVENT_IDX we need to keep
            // calling process_queue() until it stops finding new
            // requests on the queue.
            loop {
                vrings[idx].disable_notification().unwrap();
                self.process_queue_pool(vrings[idx].clone())?;
                if !vrings[idx].enable_notification().unwrap() {
                    break;
                }
            }
        } else {
            // Without EVENT_IDX, a single call is enough.
            self.process_queue_pool(vrings[idx].clone())?;
        }

        Ok(())
    }

    fn handle_event_serial(
        &self,
        device_event: u16,
        vrings: &[VringMutex<LoggedMemoryAtomic>],
    ) -> VhostUserBackendResult<()> {
        let mut vring_state = match device_event {
            HIPRIO_QUEUE_EVENT => {
                debug!("HIPRIO_QUEUE_EVENT");
                vrings[0].get_mut()
            }
            REQ_QUEUE_EVENT => {
                debug!("QUEUE_EVENT");
                vrings[1].get_mut()
            }
            _ => return Err(Error::HandleEventUnknownEvent.into()),
        };

        if self.event_idx {
            // vm-virtio's Queue implementation only checks avail_index
            // once, so to properly support EVENT_IDX we need to keep
            // calling process_queue() until it stops finding new
            // requests on the queue.
            loop {
                vring_state.disable_notification().unwrap();
                self.process_queue_serial(&mut vring_state)?;
                if !vring_state.enable_notification().unwrap() {
                    break;
                }
            }
        } else {
            // Without EVENT_IDX, a single call is enough.
            self.process_queue_serial(&mut vring_state)?;
        }

        Ok(())
    }
}

#[repr(C)]
#[derive(Clone, Copy)]
struct VirtioFsConfig {
    tag: [u8; MAX_TAG_LEN],
    num_request_queues: Le32,
}

// vm-memory needs a Default implementation even though these values are never
// used anywhere...
impl Default for VirtioFsConfig {
    fn default() -> Self {
        Self {
            tag: [0; MAX_TAG_LEN],
            num_request_queues: Le32::default(),
        }
    }
}

unsafe impl ByteValued for VirtioFsConfig {}

struct PremigrationThread {
    handle: JoinHandle<io::Result<()>>,
    cancel: Arc<AtomicBool>,
}

struct VhostUserFsBackend<F: FileSystem + SerializableFileSystem + Send + Sync + 'static> {
    thread: RwLock<VhostUserFsThread<F>>,
    premigration_thread: Mutex<Option<PremigrationThread>>,
    migration_thread: Mutex<Option<JoinHandle<io::Result<()>>>>,
    tag: Option<String>,
}

impl<F: FileSystem + SerializableFileSystem + Send + Sync + 'static> VhostUserFsBackend<F> {
    fn new(fs: F, thread_pool_size: usize, tag: Option<String>) -> Result<Self> {
        let thread = RwLock::new(VhostUserFsThread::new(fs, thread_pool_size)?);
        Ok(VhostUserFsBackend {
            thread,
            premigration_thread: None.into(),
            migration_thread: None.into(),
            tag,
        })
    }
}

impl<F: FileSystem + SerializableFileSystem + Send + Sync + 'static> VhostUserBackend
    for VhostUserFsBackend<F>
{
    type Bitmap = BitmapMmapRegion;
    type Vring = VringMutex<LoggedMemoryAtomic>;

    fn num_queues(&self) -> usize {
        NUM_QUEUES
    }

    fn max_queue_size(&self) -> usize {
        QUEUE_SIZE
    }

    fn features(&self) -> u64 {
        1 << VIRTIO_F_VERSION_1
            | 1 << VIRTIO_RING_F_INDIRECT_DESC
            | 1 << VIRTIO_RING_F_EVENT_IDX
            | VhostUserVirtioFeatures::PROTOCOL_FEATURES.bits()
            | VhostUserVirtioFeatures::LOG_ALL.bits()
    }

    fn protocol_features(&self) -> VhostUserProtocolFeatures {
        let mut protocol_features = VhostUserProtocolFeatures::MQ
            | VhostUserProtocolFeatures::BACKEND_REQ
            | VhostUserProtocolFeatures::BACKEND_SEND_FD
            | VhostUserProtocolFeatures::REPLY_ACK
            | VhostUserProtocolFeatures::CONFIGURE_MEM_SLOTS
            | VhostUserProtocolFeatures::LOG_SHMFD
            | VhostUserProtocolFeatures::DEVICE_STATE;

        if self.tag.is_some() {
            protocol_features |= VhostUserProtocolFeatures::CONFIG;
        }

        protocol_features
    }

    fn get_config(&self, offset: u32, size: u32) -> Vec<u8> {
        // virtio spec 1.2, 5.11.4:
        //   The tag is encoded in UTF-8 and padded with NUL bytes if shorter than
        //   the available space. This field is not NUL-terminated if the encoded
        //   bytes take up the entire field.
        // The length was already checked when parsing the arguments. Hence, we
        // only assert that everything looks sane and pad with NUL bytes to the
        // fixed length.
        let tag = self.tag.as_ref().expect("Did not expect read of config if tag is not set. We do not advertise F_CONFIG in that case!");
        assert!(tag.len() <= MAX_TAG_LEN, "too long tag length");
        assert!(!tag.is_empty(), "tag should not be empty");
        let mut fixed_len_tag = [0; MAX_TAG_LEN];
        fixed_len_tag[0..tag.len()].copy_from_slice(tag.as_bytes());

        let config = VirtioFsConfig {
            tag: fixed_len_tag,
            num_request_queues: Le32::from(REQUEST_QUEUES),
        };

        let offset = offset as usize;
        let size = size as usize;
        let mut result: Vec<_> = config
            .as_slice()
            .iter()
            .skip(offset)
            .take(size)
            .copied()
            .collect();
        // pad with 0s up to `size`
        result.resize(size, 0);
        result
    }

    fn acked_features(&self, features: u64) {
        if features & VhostUserVirtioFeatures::LOG_ALL.bits() != 0 {
            // F_LOG_ALL set: Prepare for migration (unless we're already doing that)
            let mut premigration_thread = self.premigration_thread.lock().unwrap();
            if premigration_thread.is_none() {
                let cancel = Arc::new(AtomicBool::new(false));
                let cloned_server = Arc::clone(&self.thread.read().unwrap().server);
                let cloned_cancel = Arc::clone(&cancel);
                let handle =
                    thread::spawn(move || cloned_server.prepare_serialization(cloned_cancel));
                *premigration_thread = Some(PremigrationThread { handle, cancel });
            }
        } else {
            // F_LOG_ALL cleared: Migration cancelled, if any was ongoing
            // (Note that this is our interpretation, and not said by the specification.  The back
            // end might clear this flag also on the source side once the VM has been stopped, even
            // before we receive SET_DEVICE_STATE_FD.  QEMU will clear F_LOG_ALL only when the VM
            // is running, i.e. when the source resumes after a cancelled migration, which is
            // exactly what we want, but it would be better if we had a more reliable way that is
            // backed up by the spec.  We could delay cancelling until we receive a guest request
            // while F_LOG_ALL is cleared, but that can take an indefinite amount of time.)
            if let Some(premigration_thread) = self.premigration_thread.lock().unwrap().take() {
                premigration_thread.cancel.store(true, Ordering::Relaxed);
                // Ignore the result, we are cancelling anyway
                let _ = premigration_thread.handle.join();
            }
        }
    }

    fn set_event_idx(&self, enabled: bool) {
        self.thread.write().unwrap().event_idx = enabled;
    }

    fn update_memory(&self, mem: LoggedMemoryAtomic) -> VhostUserBackendResult<()> {
        self.thread.write().unwrap().mem = Some(mem);
        Ok(())
    }

    fn handle_event(
        &self,
        device_event: u16,
        evset: EventSet,
        vrings: &[VringMutex<LoggedMemoryAtomic>],
        _thread_id: usize,
    ) -> VhostUserBackendResult<()> {
        if evset != EventSet::IN {
            return Err(Error::HandleEventNotEpollIn.into());
        }

        let thread = self.thread.read().unwrap();

        if thread.pool.is_some() {
            thread.handle_event_pool(device_event, vrings)
        } else {
            thread.handle_event_serial(device_event, vrings)
        }
    }

    fn exit_event(&self, _thread_index: usize) -> Option<EventFd> {
        Some(self.thread.read().unwrap().kill_evt.try_clone().unwrap())
    }

    fn set_backend_req_fd(&self, vu_req: Backend) {
        self.thread.write().unwrap().vu_req = Some(vu_req);
    }

    fn set_device_state_fd(
        &self,
        direction: VhostTransferStateDirection,
        phase: VhostTransferStatePhase,
        file: File,
    ) -> io::Result<Option<File>> {
        if phase != VhostTransferStatePhase::STOPPED {
            return Err(io::Error::new(
                io::ErrorKind::Unsupported,
                format!("Transfer in phase {:?} is not supported", phase),
            ));
        }

        let server = Arc::clone(&self.thread.read().unwrap().server);
        let join_handle = match direction {
            VhostTransferStateDirection::SAVE => {
                if let Some(premigration_thread) = self.premigration_thread.lock().unwrap().take() {
                    // It isn't great to have this vhost-user message block, but if the
                    // preparations are still ongoing, we have no choice
                    premigration_thread.handle.join().map_err(|_| {
                        other_io_error("Failed to finalize serialization preparation".to_string())
                    })??;
                }

                thread::spawn(move || {
                    server.serialize(file).map_err(|e| {
                        io::Error::new(e.kind(), format!("Failed to save state: {}", e))
                    })
                })
            }

            VhostTransferStateDirection::LOAD => {
                if let Some(premigration_thread) = self.premigration_thread.lock().unwrap().take() {
                    // Strange, but OK
                    premigration_thread.cancel.store(true, Ordering::Relaxed);
                    warn!("Cancelling serialization preparation because of incoming migration");
                    let _ = premigration_thread.handle.join();
                }

                thread::spawn(move || {
                    server.deserialize_and_apply(file).map_err(|e| {
                        io::Error::new(e.kind(), format!("Failed to load state: {}", e))
                    })
                })
            }
        };

        *self.migration_thread.lock().unwrap() = Some(join_handle);

        Ok(None)
    }

    fn check_device_state(&self) -> io::Result<()> {
        let result = if let Some(migration_thread) = self.migration_thread.lock().unwrap().take() {
            // `Result::flatten()` is not stable yet, so no `.join().map_err(...).flatten()`
            match migration_thread.join() {
                Ok(x) => x,
                Err(_) => Err(other_io_error("Failed to join the migration thread")),
            }
        } else {
            // `check_device_state()` must follow a successful `set_device_state_fd()`, so this is
            // a protocol violation
            Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "Front-end attempts to check migration state, but no migration has been done",
            ))
        };

        // Note that just like any other vhost-user message implementation, the error object that
        // we return is not forwarded to the front end (it only receives an error flag), so if we
        // want users to see some diagnostics, we have to print them ourselves
        if let Err(e) = &result {
            error!("Migration failed: {e}");
        }
        result
    }
}

fn parse_seccomp(src: &str) -> std::result::Result<SeccompAction, &'static str> {
    Ok(match src {
        "none" => SeccompAction::Allow, // i.e. no seccomp
        "kill" => SeccompAction::Kill,
        "log" => SeccompAction::Log,
        "trap" => SeccompAction::Trap,
        _ => return Err("Matching variant not found"),
    })
}

/// On the command line, we want to allow aliases for `InodeFileHandlesMode` values.  This enum has
/// all values allowed on the command line, and with `From`/`Into`, it can be translated into the
/// internally used `InodeFileHandlesMode` enum.
#[derive(Debug, Copy, Clone, PartialEq, Eq)]
enum InodeFileHandlesCommandLineMode {
    /// `InodeFileHandlesMode::Never`
    Never,
    /// Alias for `InodeFileHandlesMode::Prefer`
    Fallback,
    /// `InodeFileHandlesMode::Prefer`
    Prefer,
    /// `InodeFileHandlesMode::Mandatory`
    Mandatory,
}

impl From<InodeFileHandlesCommandLineMode> for InodeFileHandlesMode {
    fn from(clm: InodeFileHandlesCommandLineMode) -> Self {
        match clm {
            InodeFileHandlesCommandLineMode::Never => InodeFileHandlesMode::Never,
            InodeFileHandlesCommandLineMode::Fallback => InodeFileHandlesMode::Prefer,
            InodeFileHandlesCommandLineMode::Prefer => InodeFileHandlesMode::Prefer,
            InodeFileHandlesCommandLineMode::Mandatory => InodeFileHandlesMode::Mandatory,
        }
    }
}

impl FromStr for InodeFileHandlesCommandLineMode {
    type Err = &'static str;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        match s {
            "never" => Ok(InodeFileHandlesCommandLineMode::Never),
            "fallback" => Ok(InodeFileHandlesCommandLineMode::Fallback),
            "prefer" => Ok(InodeFileHandlesCommandLineMode::Prefer),
            "mandatory" => Ok(InodeFileHandlesCommandLineMode::Mandatory),

            _ => Err("invalid inode file handles mode"),
        }
    }
}

fn parse_tag(tag: &str) -> Result<String> {
    if !tag.is_empty() && tag.len() <= MAX_TAG_LEN {
        Ok(tag.into())
    } else {
        Err(Error::InvalidTag)
    }
}

#[derive(Clone, Debug, Parser)]
#[command(
    name = "virtiofsd",
    about = "Launch a virtiofsd backend.",
    version,
    args_override_self = true
)]
struct Opt {
    /// Shared directory path
    #[arg(long, required_unless_present("compat_options"))]
    shared_dir: Option<String>,

    /// The tag that the virtio device advertises
    ///
    /// Setting this option will enable advertising of
    /// VHOST_USER_PROTOCOL_F_CONFIG. However, the vhost-user frontend of your
    /// hypervisor may not negotiate this feature and (or) ignore this value.
    /// Notably, QEMU currently (as of 8.1) ignores the CONFIG feature. QEMU
    /// versions from 7.1 to 8.0 will crash while attempting to log a warning
    /// about not supporting the feature.
    #[arg(long, value_parser = parse_tag)]
    tag: Option<String>,

    /// vhost-user socket path [deprecated]
    #[arg(long, required_unless_present_any = &["fd", "socket_path", "print_capabilities"])]
    socket: Option<String>,

    /// vhost-user socket path
    #[arg(long = "socket-path", required_unless_present_any = &["fd", "socket", "print_capabilities"])]
    socket_path: Option<String>,

    /// Name of group for the vhost-user socket
    #[arg(long = "socket-group", conflicts_with_all = &["fd", "print_capabilities"])]
    socket_group: Option<String>,

    /// File descriptor for the listening socket
    #[arg(long, required_unless_present_any = &["socket", "socket_path", "print_capabilities"], conflicts_with_all = &["socket_path", "socket"])]
    fd: Option<RawFd>,

    /// Maximum thread pool size. A value of "0" disables the pool
    #[arg(long, default_value = "0")]
    thread_pool_size: usize,

    /// Enable support for extended attributes
    #[arg(long)]
    xattr: bool,

    /// Enable support for posix ACLs (implies --xattr)
    #[arg(long)]
    posix_acl: bool,

    /// Add custom rules for translating extended attributes between host and guest
    /// (e.g. :map::user.virtiofs.:)
    #[arg(long, value_parser = |s: &_| XattrMap::try_from(s))]
    xattrmap: Option<XattrMap>,

    /// Sandbox mechanism to isolate the daemon process (namespace, chroot, none)
    #[arg(long, default_value = "namespace")]
    sandbox: SandboxMode,

    /// Action to take when seccomp finds a not allowed syscall (none, kill, log, trap)
    #[arg(long, value_parser = parse_seccomp, default_value = "kill")]
    seccomp: SeccompAction,

    /// Tell the guest which directories are mount points [default]
    #[arg(long)]
    announce_submounts: bool,

    /// Do not tell the guest which directories are mount points
    #[arg(long, overrides_with("announce_submounts"))]
    no_announce_submounts: bool,

    /// When to use file handles to reference inodes instead of O_PATH file descriptors (never,
    /// prefer, mandatory)
    ///
    /// - never: Never use file handles, always use O_PATH file descriptors.
    ///
    /// - prefer: Attempt to generate file handles, but fall back to O_PATH file descriptors where
    /// the underlying filesystem does not support file handles.  Useful when there are various
    /// different filesystems under the shared directory and some of them do not support file
    /// handles.  ("fallback" is a deprecated alias for "prefer".)
    ///
    /// - mandatory: Always use file handles, never fall back to O_PATH file descriptors.
    ///
    /// Using file handles reduces the number of file descriptors virtiofsd keeps open, which is
    /// not only helpful with resources, but may also be important in cases where virtiofsd should
    /// only have file descriptors open for files that are open in the guest, e.g. to get around
    /// bad interactions with NFS's silly renaming.
    #[arg(long, require_equals = true, default_value = "never")]
    inode_file_handles: InodeFileHandlesCommandLineMode,

    /// The caching policy the file system should use (auto, always, never, metadata)
    #[arg(long, default_value = "auto")]
    cache: CachePolicy,

    #[arg(long)]
    allow_mmap: bool,

    /// Disable support for READDIRPLUS operations
    #[arg(long)]
    no_readdirplus: bool,

    /// Enable writeback cache
    #[arg(long)]
    writeback: bool,

    /// Honor the O_DIRECT flag passed down by guest applications
    #[arg(long)]
    allow_direct_io: bool,

    /// Print vhost-user.json backend program capabilities and exit
    #[arg(long = "print-capabilities")]
    print_capabilities: bool,

    /// Modify the list of capabilities, e.g., --modcaps=+sys_admin:-chown
    #[arg(long)]
    modcaps: Option<String>,

    /// Log level (error, warn, info, debug, trace, off)
    #[arg(long = "log-level", default_value = "info")]
    log_level: LevelFilter,

    /// Log to syslog [default: stderr]
    #[arg(long)]
    syslog: bool,

    /// Set maximum number of file descriptors (0 leaves rlimit unchanged)
    /// [default: min(1000000, '/proc/sys/fs/nr_open')]
    #[arg(long = "rlimit-nofile")]
    rlimit_nofile: Option<u64>,

    /// Options in a format compatible with the legacy implementation [deprecated]
    #[arg(short = 'o')]
    compat_options: Option<Vec<String>>,

    /// Set log level to "debug" [deprecated]
    #[arg(short = 'd')]
    compat_debug: bool,

    /// Disable KILLPRIV V2 support [default]
    #[arg(long)]
    _no_killpriv_v2: bool,

    /// Enable KILLPRIV V2 support
    #[arg(long, overrides_with("_no_killpriv_v2"))]
    killpriv_v2: bool,

    /// Compatibility option that has no effect [deprecated]
    #[arg(short = 'f')]
    compat_foreground: bool,

    /// Enable security label support. Expects SELinux xattr on file creation
    /// from client and stores it in the newly created file.
    #[arg(long = "security-label")]
    security_label: bool,

    /// Map a range of UIDs from the host into the namespace, given as
    /// :namespace_uid:host_uid:count:
    ///
    /// For example, :0:100000:65536: will map the 65536 host UIDs [100000, 165535]
    /// into the namespace as [0, 65535].
    ///
    /// Provide this argument multiple times to map multiple UID ranges.
    #[arg(long)]
    uid_map: Vec<UidMap>,

    /// Map a range of GIDs from the host into the namespace, given as
    /// :namespace_gid:host_gid:count:
    ///
    /// For example, :0:100000:65536: will map the 65536 host GIDs [100000, 165535]
    /// into the namespace as [0, 65535].
    ///
    /// Provide this argument multiple times to map multiple GID ranges.
    #[arg(long)]
    gid_map: Vec<GidMap>,

    /// Preserve O_NOATIME behavior, otherwise automatically clean up O_NOATIME flag to prevent
    /// potential permission errors when running in unprivileged mode (e.g., when accessing files
    /// without having ownership/capability to use O_NOATIME).
    #[arg(long = "preserve-noatime")]
    preserve_noatime: bool,

    /// Defines how to perform migration, i.e. how to represent the internal state to the
    /// destination, and how to obtain that representation.
    ///
    /// - find-paths: Iterate through the shared directory (exhaustive search) to find paths for
    ///   all inodes indexed and opened by the guest, and transfer these paths to the destination.
    ///
    /// This parameter is ignored on the destination side.
    #[arg(long = "migration-mode", default_value = "find-paths")]
    migration_mode: MigrationMode,

    /// Controls how to respond to errors during migration.
    ///
    /// If any inode turns out not to be migratable (either the source cannot serialize it, or the
    /// destination cannot opened the serialized representation), the destination can react in
    /// different ways:
    ///
    /// - abort: Whenever any error occurs, return a hard error to the vhost-user front-end (e.g.
    ///          QEMU), aborting migration.
    ///
    /// - guest-error: Let migration finish, but the guest will be unable to access any of the
    ///                affected inodes, receiving only errors.
    ///
    /// This parameter is ignored on the source side.
    #[arg(long = "migration-on-error", default_value = "abort")]
    migration_on_error: MigrationOnError,

    /// Ensure that the migration destination opens the very same inodes as the source (only works
    /// if source and destination use the same shared directory on the same filesystem).
    ///
    /// This option makes the source attach the respective file handle to each inode transferred
    /// during migration.  Once the destination has (re-)opened the inode, it will generate the
    /// file handle on its end, and compare, ensuring that it has opened the very same inode.
    ///
    /// (File handles are per-filesystem unique identifiers for inodes that, besides the inode ID,
    /// also include a generation ID to protect against inode ID reuse.)
    ///
    /// Using this option protects against external parties renaming or replacing inodes
    /// while migration is ongoing, which, without this option, can lead to data loss or
    /// corruption, so it should always be used when other processes besides virtiofsd have write
    /// access to the shared directory.  However, again, it only works if both source and
    /// destination use the same shared directory.
    ///
    /// This parameter is ignored on the destination side.
    #[arg(long = "migration-verify-handles")]
    migration_verify_handles: bool,

    /// Double-check the identity of inodes right before switching over to the destination,
    /// potentially making migration more resilient when third parties have write access to the
    /// shared directory.
    ///
    /// When representing migrated inodes using their paths relative to the shared directory,
    /// double-check during switch-over to the destination that each path still matches the
    /// respective inode, and on mismatch, try to correct it via the respective symlink in
    /// /proc/self/fd.
    ///
    /// Because this option requires accessing each inode indexed or opened by the guest, it can
    /// prolong the switch-over phase of migration (when both source and destination are paused)
    /// for an indeterminate amount of time.
    ///
    /// This parameter is ignored on the destination side.
    #[arg(long = "migration-confirm-paths")]
    migration_confirm_paths: bool,
}

fn parse_compat(opt: Opt) -> Opt {
    use clap::error::ErrorKind;
    fn value_error(arg: &str, value: &str) -> ! {
        <Opt as CommandFactory>::command()
            .error(
                ErrorKind::InvalidValue,
                format!("Invalid compat value '{value}' for '-o {arg}'"),
            )
            .exit()
    }
    fn argument_error(arg: &str) -> ! {
        <Opt as CommandFactory>::command()
            .error(
                ErrorKind::UnknownArgument,
                format!("Invalid compat argument '-o {arg}'"),
            )
            .exit()
    }

    fn parse_tuple(opt: &mut Opt, tuple: &str) {
        match tuple.split('=').collect::<Vec<&str>>()[..] {
            ["xattrmap", value] => {
                opt.xattrmap = Some(
                    XattrMap::try_from(value).unwrap_or_else(|_| value_error("xattrmap", value)),
                )
            }
            ["cache", value] => match value {
                "auto" => opt.cache = CachePolicy::Auto,
                "always" => opt.cache = CachePolicy::Always,
                "none" => opt.cache = CachePolicy::Never,
                "metadata" => opt.cache = CachePolicy::Metadata,
                _ => value_error("cache", value),
            },
            ["loglevel", value] => match value {
                "debug" => opt.log_level = LevelFilter::Debug,
                "info" => opt.log_level = LevelFilter::Info,
                "warn" => opt.log_level = LevelFilter::Warn,
                "err" => opt.log_level = LevelFilter::Error,
                _ => value_error("loglevel", value),
            },
            ["sandbox", value] => match value {
                "namespace" => opt.sandbox = SandboxMode::Namespace,
                "chroot" => opt.sandbox = SandboxMode::Chroot,
                _ => value_error("sandbox", value),
            },
            ["source", value] => opt.shared_dir = Some(value.to_string()),
            ["modcaps", value] => opt.modcaps = Some(value.to_string()),
            _ => argument_error(tuple),
        }
    }

    fn parse_single(opt: &mut Opt, option: &str) {
        match option {
            "xattr" => opt.xattr = true,
            "no_xattr" => opt.xattr = false,
            "readdirplus" => opt.no_readdirplus = false,
            "no_readdirplus" => opt.no_readdirplus = true,
            "writeback" => opt.writeback = true,
            "no_writeback" => opt.writeback = false,
            "allow_direct_io" => opt.allow_direct_io = true,
            "no_allow_direct_io" => opt.allow_direct_io = false,
            "announce_submounts" => opt.announce_submounts = true,
            "killpriv_v2" => opt.killpriv_v2 = true,
            "no_killpriv_v2" => opt.killpriv_v2 = false,
            "posix_acl" => opt.posix_acl = true,
            "no_posix_acl" => opt.posix_acl = false,
            "security_label" => opt.security_label = true,
            "no_security_label" => opt.security_label = false,
            "no_posix_lock" | "no_flock" => (),
            _ => argument_error(option),
        }
    }

    let mut clean_opt = opt.clone();

    if let Some(compat_options) = opt.compat_options.as_ref() {
        for line in compat_options {
            for option in line.split(',') {
                if option.contains('=') {
                    parse_tuple(&mut clean_opt, option);
                } else {
                    parse_single(&mut clean_opt, option);
                }
            }
        }
    }

    clean_opt
}

fn print_capabilities() {
    println!("{{");
    println!("  \"type\": \"fs\",");
    println!("  \"features\": [");
    println!("    \"migrate-precopy\"");
    println!("  ]");
    println!("}}");
}

fn set_default_logger(log_level: LevelFilter) {
    if env::var("RUST_LOG").is_err() {
        env::set_var("RUST_LOG", log_level.to_string());
    }
    env_logger::init();
}

fn initialize_logging(opt: &Opt) {
    let log_level = if opt.compat_debug {
        LevelFilter::Debug
    } else {
        opt.log_level
    };

    if opt.syslog {
        if let Err(e) = syslog::init(syslog::Facility::LOG_USER, log_level, None) {
            set_default_logger(log_level);
            warn!("can't enable syslog: {}", e);
        }
    } else {
        set_default_logger(log_level);
    }
}

fn set_signal_handlers() {
    use vmm_sys_util::signal;

    extern "C" fn handle_signal(_: libc::c_int, _: *mut libc::siginfo_t, _: *mut libc::c_void) {
        unsafe { libc::_exit(1) };
    }
    let signals = vec![libc::SIGHUP, libc::SIGTERM];
    for s in signals {
        if let Err(e) = signal::register_signal_handler(s, handle_signal) {
            error!("Setting signal handlers: {}", e);
            process::exit(1);
        }
    }
}

fn parse_modcaps(
    default_caps: Vec<&str>,
    modcaps: Option<String>,
) -> (HashSet<String>, HashSet<String>) {
    let mut required_caps: HashSet<String> = default_caps.iter().map(|&s| s.into()).collect();
    let mut disabled_caps = HashSet::new();

    if let Some(modcaps) = modcaps {
        for modcap in modcaps.split(':').map(str::to_string) {
            if modcap.is_empty() {
                error!("empty modcap found: expected (+|-)capability:...");
                process::exit(1);
            }
            let (action, cap_name) = modcap.split_at(1);
            let cap_name = cap_name.to_uppercase();
            if !matches!(action, "+" | "-") {
                error!(
                    "invalid modcap action: expecting '+'|'-' but found '{}'",
                    action
                );
                process::exit(1);
            }
            if let Err(error) = capng::name_to_capability(&cap_name) {
                error!("invalid capability '{}': {}", &cap_name, error);
                process::exit(1);
            }

            match action {
                "+" => {
                    disabled_caps.remove(&cap_name);
                    required_caps.insert(cap_name);
                }
                "-" => {
                    required_caps.remove(&cap_name);
                    disabled_caps.insert(cap_name);
                }
                _ => unreachable!(),
            }
        }
    }
    (required_caps, disabled_caps)
}

fn drop_capabilities(inode_file_handles: InodeFileHandlesMode, modcaps: Option<String>) {
    let default_caps = vec![
        "CHOWN",
        "DAC_OVERRIDE",
        "FOWNER",
        "FSETID",
        "SETGID",
        "SETUID",
        "MKNOD",
        "SETFCAP",
    ];
    let (mut required_caps, disabled_caps) = parse_modcaps(default_caps, modcaps);

    if inode_file_handles != InodeFileHandlesMode::Never {
        let required_cap = "DAC_READ_SEARCH".to_owned();
        if disabled_caps.contains(&required_cap) {
            error!(
                "can't disable {} when using --inode-file-handles={:?}",
                &required_cap, inode_file_handles
            );
            process::exit(1);
        }
        required_caps.insert(required_cap);
    }

    capng::clear(capng::Set::BOTH);
    // Configure the required set of capabilities for the child, and leave the
    // parent with none.
    if let Err(e) = capng::updatev(
        capng::Action::ADD,
        capng::Type::PERMITTED | capng::Type::EFFECTIVE,
        required_caps.iter().map(String::as_str).collect(),
    ) {
        error!("can't set up the child capabilities: {}", e);
        process::exit(1);
    }
    if let Err(e) = capng::apply(capng::Set::BOTH) {
        error!("can't apply the child capabilities: {}", e);
        process::exit(1);
    }
}

fn has_noatime_capability() -> bool {
    // We may not have all permissions/capabilities to use O_NOATIME with all the exported files if
    // we are running as unprivileged user and without any sandbox (e.g., --sandbox=none).
    //
    // Provide this helper function to check this particular case.
    let uid = unsafe { libc::geteuid() };
    let cap = capng::name_to_capability("FOWNER").unwrap_or_else(|err| {
        error!("could not get capability FOWNER: {}", err);
        process::exit(1);
    });

    uid == 0 || capng::have_capability(capng::Type::EFFECTIVE, cap)
}

fn main() {
    let opt = parse_compat(Opt::parse());

    // Enable killpriv_v2 only if user explicitly asked for it by using
    // --killpriv-v2 or -o killpriv_v2. Otherwise disable it by default.
    let killpriv_v2 = opt.killpriv_v2;

    // Disable announce submounts if the user asked for it
    let announce_submounts = !opt.no_announce_submounts;

    if opt.print_capabilities {
        print_capabilities();
        return;
    }

    initialize_logging(&opt);
    set_signal_handlers();

    let shared_dir = match opt.shared_dir.as_ref() {
        Some(s) => s,
        None => {
            error!("missing \"--shared-dir\" or \"-o source\" option");
            process::exit(1);
        }
    };

    let shadir_path = Path::new(shared_dir);
    if !shadir_path.is_dir() && !shadir_path.is_file() {
        error!("{shared_dir} does not exist");
        process::exit(1);
    }

    if opt.compat_foreground {
        warn!("Use of deprecated flag '-f': This flag has no effect, please remove it");
    }
    if opt.compat_debug {
        warn!("Use of deprecated flag '-d': Please use the '--log-level debug' option instead");
    }
    if opt.compat_options.is_some() {
        warn!("Use of deprecated option format '-o': Please specify options without it (e.g., '--cache auto' instead of '-o cache=auto')");
    }
    if opt.inode_file_handles == InodeFileHandlesCommandLineMode::Fallback {
        warn!("Use of deprecated value 'fallback' for '--inode-file-handles': Please use 'prefer' instead");
    }

    let xattrmap = opt.xattrmap.clone();
    let xattr = xattrmap.is_some() || opt.posix_acl || opt.xattr;
    let thread_pool_size = opt.thread_pool_size;
    let readdirplus = match opt.cache {
        CachePolicy::Never => false,
        _ => !opt.no_readdirplus,
    };

    let timeout = match opt.cache {
        CachePolicy::Never => Duration::from_secs(0),
        CachePolicy::Metadata => Duration::from_secs(86400),
        CachePolicy::Auto => Duration::from_secs(1),
        CachePolicy::Always => Duration::from_secs(86400),
    };

    let umask = if opt.socket_group.is_some() {
        libc::S_IROTH | libc::S_IWOTH | libc::S_IXOTH
    } else {
        libc::S_IRGRP
            | libc::S_IWGRP
            | libc::S_IXGRP
            | libc::S_IROTH
            | libc::S_IWOTH
            | libc::S_IXOTH
    };

    // We need to keep _pid_file around because it maintains a lock on the pid file
    // that prevents another daemon from using the same pid file.
    let (listener, socket_path, _pid_file) = match opt.fd.as_ref() {
        Some(fd) => unsafe { (Listener::from_raw_fd(*fd), None, None) },
        None => {
            // Set umask to ensure the socket is created with the right permissions
            let _umask_guard = oslib::ScopedUmask::new(umask);

            let socket = opt.socket_path.as_ref().unwrap_or_else(|| {
                warn!("use of deprecated parameter '--socket': Please use the '--socket-path' option instead");
                opt.socket.as_ref().unwrap() // safe to unwrap because clap ensures either --socket or --socket-path are passed
            });

            let socket_parent_dir = Path::new(socket).parent().unwrap_or_else(|| {
                error!("Invalid socket file name");
                process::exit(1);
            });

            if !socket_parent_dir.as_os_str().is_empty() && !socket_parent_dir.exists() {
                error!(
                    "{} does not exist or is not a directory",
                    socket_parent_dir.to_string_lossy()
                );
                process::exit(1);
            }

            let pid_file_name = socket.to_owned() + ".pid";
            let pid_file_path = Path::new(pid_file_name.as_str());
            let pid_file = write_pid_file(pid_file_path).unwrap_or_else(|error| {
                error!("Error creating pid file '{}': {}", pid_file_name, error);
                process::exit(1);
            });

            let listener = Listener::new(socket, true).unwrap_or_else(|error| {
                error!("Error creating listener: {}", error);
                process::exit(1);
            });

            (listener, Some(socket.clone()), Some(pid_file))
        }
    };

    if let Some(group_name) = opt.socket_group {
        let c_name = CString::new(group_name).expect("invalid group name");
        let group = unsafe { libc::getgrnam(c_name.as_ptr()) };
        if group.is_null() {
            error!("Couldn't resolve the group name specified for the socket path");
            process::exit(1);
        }

        // safe to unwrap because clap ensures --socket-group can't be specified alongside --fd
        let c_socket_path = CString::new(socket_path.unwrap()).expect("invalid socket path");
        let ret = unsafe { libc::chown(c_socket_path.as_ptr(), u32::MAX, (*group).gr_gid) };
        if ret != 0 {
            error!(
                "Couldn't set up the group for the socket path: {}",
                std::io::Error::last_os_error()
            );
            process::exit(1);
        }
    }

    limits::setup_rlimit_nofile(opt.rlimit_nofile).unwrap_or_else(|error| {
        error!("Error increasing number of open files: {}", error);
        process::exit(1)
    });

    let mut sandbox = Sandbox::new(
        shared_dir.to_string(),
        opt.sandbox,
        opt.uid_map,
        opt.gid_map,
    )
    .unwrap_or_else(|error| {
        error!("Error creating sandbox: {}", error);
        process::exit(1)
    });

    // Enter the sandbox, from this point the process will be isolated (or not)
    // as chosen in '--sandbox'.
    let listener = sandbox.enter(listener).unwrap_or_else(|error| {
        error!("Error entering sandbox: {}", error);
        process::exit(1)
    });

    let fs_cfg = passthrough::Config {
        entry_timeout: timeout,
        attr_timeout: timeout,
        cache_policy: opt.cache,
        root_dir: sandbox.get_root_dir(),
        mountinfo_prefix: sandbox.get_mountinfo_prefix(),
        xattr,
        xattrmap,
        proc_sfd_rawfd: sandbox.get_proc_self_fd(),
        proc_mountinfo_rawfd: sandbox.get_mountinfo_fd(),
        announce_submounts,
        inode_file_handles: opt.inode_file_handles.into(),
        readdirplus,
        writeback: opt.writeback,
        allow_direct_io: opt.allow_direct_io,
        killpriv_v2,
        security_label: opt.security_label,
        posix_acl: opt.posix_acl,
        clean_noatime: !opt.preserve_noatime && !has_noatime_capability(),
        allow_mmap: opt.allow_mmap,
        migration_on_error: opt.migration_on_error,
        migration_verify_handles: opt.migration_verify_handles,
        migration_confirm_paths: opt.migration_confirm_paths,
        migration_mode: opt.migration_mode,
        ..Default::default()
    };

    // Must happen before we start the thread pool
    match opt.seccomp {
        SeccompAction::Allow => {}
        _ => enable_seccomp(opt.seccomp, opt.syslog).unwrap(),
    }

    // We don't modify the capabilities if the user call us without
    // any sandbox (i.e. --sandbox=none) as unprivileged user
    let uid = unsafe { libc::geteuid() };
    if uid == 0 {
        drop_capabilities(fs_cfg.inode_file_handles, opt.modcaps);
    }

    let fs = match PassthroughFs::new(fs_cfg) {
        Ok(fs) => fs,
        Err(e) => {
            error!(
                "Failed to create internal filesystem representation: {:?}",
                e
            );
            process::exit(1);
        }
    };

    let fs_backend = Arc::new(
        VhostUserFsBackend::new(fs, thread_pool_size, opt.tag).unwrap_or_else(|error| {
            error!("Error creating vhost-user backend: {}", error);
            process::exit(1)
        }),
    );

    let mut daemon = VhostUserDaemon::new(
        String::from("virtiofsd-backend"),
        fs_backend.clone(),
        GuestMemoryAtomic::new(GuestMemoryMmap::new()),
    )
    .unwrap();

    info!("Waiting for vhost-user socket connection...");

    if let Err(e) = daemon.start(listener) {
        error!("Failed to start daemon: {:?}", e);
        process::exit(1);
    }

    info!("Client connected, servicing requests");

    if let Err(e) = daemon.wait() {
        match e {
            HandleRequest(Disconnected) => info!("Client disconnected, shutting down"),
            _ => error!("Waiting for daemon failed: {:?}", e),
        }
    }

    let kill_evt = fs_backend
        .thread
        .read()
        .unwrap()
        .kill_evt
        .try_clone()
        .unwrap();
    if let Err(e) = kill_evt.write(1) {
        error!("Error shutting down worker thread: {:?}", e)
    }
}
