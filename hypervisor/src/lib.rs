mod common;
pub mod vmm_config;

pub use event_notifier::NotifyEvent;
pub use vmm::api::*;
pub use vmm::config;
pub use vmm::seccomp_filters::*;
pub use vmm::vm_config;
pub use vmm::{SnapshotConfig, SnapshotType};

use chrono::Local;
use libc::{EFD_NONBLOCK, SIGSYS, SIGTERM};
use log::{error, info, LevelFilter};
use logging::START_TM;
use seccompiler::SeccompAction;
use std::fs::File;
use std::os::fd::FromRawFd;
use std::sync::atomic::AtomicBool;
use std::sync::mpsc::{channel, RecvError, SendError};
use std::sync::{Arc, Barrier, Mutex};
use std::thread;
use std::thread::JoinHandle;
use std::time::{Duration, Instant};
use thiserror::Error;
use vmm::api::service::{Error as VmmServiceError, VMM_SERVICE};
use vmm::Error as VmmError;
use vmm_sys_util::eventfd::EventFd;
use vmm_sys_util::signal::{block_signal, Killable};
use vmm_sys_util::terminal::Terminal;

use crate::common::DEFAULT_LOGGER_BUFFER_SIZE;
use crate::vmm_config::VmmConfig;

#[derive(Debug, Error)]
pub enum Error {
    #[error("Failed to recv ApiResponse by channel: {0}")]
    ReviverChannel(#[source] RecvError),
    #[error("Failed to send ApiRequest by channel: {0}")]
    SenderChannel(#[source] SendError<ApiRequest>),
    #[error("Failed to start vmm: {0}")]
    StartVmm(#[source] VmmError),
    #[error("Failed to create EventFd: {0}")]
    CreateEventFd(#[source] std::io::Error),
    #[error("Failed to create Hypervisor: {0}")]
    CreateHypervisor(#[source] hypervisor::HypervisorError),
    #[error("Error setting up logger: {0}")]
    LoggerSetup(#[source] log::SetLoggerError),
    #[error("Error setting up coredump: {0}")]
    CoredumpSetup(#[source] std::io::Error),
    #[error("Error parsing --event-monitor: path or fd required")]
    BareEventMonitor,
    #[error("Error doing event monitor I/O: {0}")]
    EventMonitorIo(#[source] std::io::Error),
    #[error("Error doing log json I/O: {0}")]
    LogJsonIo(#[source] std::io::Error),
    #[error("Error init vmm service: {0}")]
    InitVmmService(#[source] VmmServiceError),
    #[error("Failed to send request: {0}")]
    SendRequest(#[source] VmmServiceError),
    #[error("Failed to join on VMM thread: {0:?}")]
    ThreadJoin(Box<dyn std::any::Any + Send>),
    #[error("VMM thread exited with error: {0}")]
    VmmThread(#[source] vmm::Error),
}

/// # Vmm instance for CH
///
/// VmmInstance provides vmm management interfaces for upper layer access.
pub struct VmmInstance {
    vmm_thread: Option<JoinHandle<Result<(), VmmError>>>,
}

impl VmmInstance {
    /// Create a vmm instance
    ///
    /// This function will prepare the vmm resources and start a vmm thread, the vmm
    /// thread is ready when this function returns.
    ///
    /// # Examples:
    /// ```
    /// use cube_hypervisor::vmm_config::VmmConfig;
    /// use cube_hypervisor::VmmInstance;
    ///
    /// let vmm = VmmInstance::new(VmmConfig::default()).expect("Failed to create vmm");
    /// ```
    pub fn new(vmm_config: VmmConfig) -> Result<Self, Error> {
        let now = std::time::Instant::now();
        let local = Local::now();
        let tm1 = std::time::Instant::now().duration_since(now);

        let defer_logger_thread = vmm_config.log_level <= LevelFilter::Info;

        // Init log file and log async mode.
        let (log_file, log_async) = {
            if !vmm_config.log_stderr {
                // Open async log file later in cube-log
                // thread. Let cube-log thread handle the
                // log file rotate(reopen) logic.
                (None, true)
            } else {
                let file: Box<dyn std::io::Write + Send> = Box::new(std::io::stderr());
                (Some(file), false)
            }
        };

        #[cfg(feature = "logger_debug")]
        let mut logger_dbg_file_name = vmm_config.log_file.clone();
        #[cfg(feature = "logger_debug")]
        logger_dbg_file_name.push_str(".dbg");
        #[cfg(feature = "logger_debug")]
        let logger_dbg_file = Arc::new(Mutex::new(
            std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .append(true)
                .open(logger_dbg_file_name)
                .unwrap(),
        ));

        let wait_vcpu_started = Arc::new(AtomicBool::new(false));
        *START_TM.lock().unwrap() = now;

        // set_boxed_logger only return an error when initialized multiple times。
        if log::set_boxed_logger(Box::new(common::Logger::new(
            Mutex::new(log_file),
            now,
            vmm_config.sandbox_id.clone(),
            log_async,
            defer_logger_thread,
            Arc::new(Mutex::new(Vec::with_capacity(DEFAULT_LOGGER_BUFFER_SIZE))),
            Arc::new(Mutex::new(AtomicBool::new(false))),
            vmm_config.log_file,
            wait_vcpu_started.clone(),
            Arc::new(AtomicBool::new(false)),
            #[cfg(feature = "logger_debug")]
            logger_dbg_file,
        )))
        .map(|()| log::set_max_level(vmm_config.log_level))
        .is_err()
        {
            error!(
                "Logger has been initialized, please check if VMM has been created multiple times!"
            );
        }

        let tm2 = std::time::Instant::now().duration_since(now);
        info!(
            "Cube-Hypervisor version {}, prepare main {}ms, prepare vmm {}ms, start {}",
            env!("BUILT_VERSION"),
            tm1.as_millis(),
            tm2.as_millis(),
            local
        );

        if let Some(monitor_config) = vmm_config.event_monitor {
            let file = if let Some(fd) = monitor_config.fd {
                // SAFETY: fd is valid
                unsafe { File::from_raw_fd(fd) }
            } else if let Some(path) = monitor_config.path {
                std::fs::OpenOptions::new()
                    .write(true)
                    .create(true)
                    .open(path.as_str())
                    .map_err(Error::EventMonitorIo)?
            } else {
                return Err(Error::BareEventMonitor);
            };
            event_monitor::set_monitor(file).map_err(Error::EventMonitorIo)?;
        }

        if let Some(notifier_config) = vmm_config.event_notifier {
            event_notifier::setup_notifier(notifier_config.notifier);
        }

        if let Some(file) = vmm_config.log_json_file {
            log_json::set_log_file(file).map_err(Error::LogJsonIo)?;
        }

        let (req_sender, req_receiver) = channel();
        let (res_sender, res_receiver) = channel();
        let api_evt = EventFd::new(EFD_NONBLOCK).map_err(Error::CreateEventFd)?;

        VMM_SERVICE
            .lock()
            .unwrap()
            .init(req_sender, api_evt.try_clone().unwrap(), res_receiver)
            .map_err(Error::InitVmmService)?;

        let hypervisor = hypervisor::new().map_err(Error::CreateHypervisor)?;

        #[cfg(feature = "guest_debug")]
        let gdb_socket_path = None;
        #[cfg(feature = "guest_debug")]
        let debug_evt = EventFd::new(EFD_NONBLOCK).map_err(Error::CreateEventFd)?;
        #[cfg(feature = "guest_debug")]
        let vm_debug_evt = EventFd::new(EFD_NONBLOCK).map_err(Error::CreateEventFd)?;

        if vmm_config.seccomp == SeccompAction::Trap
            || vmm_config.seccomp == SeccompAction::KillProcess
        {
            // SAFETY: We only using signal_hook for managing signals and only execute signal
            // handler safe functions (writing to stderr) and manipulating signals.
            unsafe {
                signal_hook::low_level::register(SIGSYS, || {
                    eprint!("\nError signal: SIGSYS, possible seccomp violation.\n");
                    error!("Error signal: SIGSYS, possible seccomp violation.");
                })
            }
            .map_err(|e| error!("Error adding SIGSYS signal handler: {}", e))
            .ok();
        }

        // Before we start any threads, mask the signals we'll be
        // installing handlers for, to make sure they only ever run on the
        // dedicated signal handling thread we'll start in a bit.
        for sig in &vmm::vm::Vm::HANDLED_SIGNALS {
            if let Err(e) = block_signal(*sig) {
                error!("Error blocking signals: {}", e);
            }
        }

        for sig in &vmm::Vmm::HANDLED_SIGNALS {
            if let Err(e) = block_signal(*sig) {
                error!("Error blocking signals: {}", e);
            }
        }

        // waiting for vmm thread
        let barrier = Arc::new(Barrier::new(2));

        let vmm_thread = vmm::start_vmm_thread(
            env!("CARGO_PKG_VERSION").to_string(),
            &vmm_config.http_path,
            None,
            api_evt,
            req_receiver,
            res_sender.clone(),
            #[cfg(feature = "guest_debug")]
            gdb_socket_path,
            #[cfg(feature = "guest_debug")]
            debug_evt.try_clone().unwrap(),
            #[cfg(feature = "guest_debug")]
            vm_debug_evt.try_clone().unwrap(),
            &vmm_config.seccomp,
            hypervisor,
            vmm_config.sandbox_id,
            wait_vcpu_started,
            Some(barrier.clone()),
        )
        .map_err(Error::StartVmm)?;

        barrier.wait();

        Ok(Self {
            vmm_thread: Some(vmm_thread),
        })
    }

    /// Send api request to vmm
    ///
    /// This function provides a interface for up layer to send request to vmm.
    ///
    /// # Examples:
    /// ```
    /// use cube_hypervisor::vmm_config::VmmConfig;
    /// use cube_hypervisor::VmmInstance;
    /// use vmm::api::ApiRequest;
    ///
    /// let vmm = VmmInstance::new(VmmConfig::default()).expect("Failed to create vmm");
    /// let response = vmm.send_request(ApiRequest::VmmPing)
    ///     .expect("Failed to send vmm ping request");
    /// ```
    pub fn send_request(&self, request: ApiRequest) -> Result<ApiResponse, Error> {
        VMM_SERVICE
            .lock()
            .unwrap()
            .send_request(request)
            .map_err(Error::SendRequest)
    }

    /// Wait for the vmm thread to finish.
    ///
    /// This function will return immediately if the associated thread has already finished.
    ///
    /// # Examples:
    /// ```
    /// use cube_hypervisor::vmm_config::VmmConfig;
    /// use cube_hypervisor::VmmInstance;
    /// use vmm::api::ApiRequest;
    ///
    /// let mut vmm = VmmInstance::new(VmmConfig::default()).expect("Failed to create vmm");
    /// vmm.join().unwrap();
    /// ```
    pub fn join(&mut self) -> Result<(), Error> {
        if let Some(thread) = self.vmm_thread.take() {
            thread
                .join()
                .map_err(Error::ThreadJoin)?
                .map_err(Error::VmmThread)?;
        }
        Ok(())
    }

    /// Wait timeout for the vmm thread to finish.
    ///
    /// This function will return immediately if the associated thread has already finished. The
    /// vmm thread will be terminated after timeout. The default timeout is 5s.
    ///
    /// # Examples:
    /// ```
    /// use std::time::Duration;
    /// use cube_hypervisor::vmm_config::VmmConfig;
    /// use cube_hypervisor::VmmInstance;
    /// use vmm::api::ApiRequest;
    ///
    /// let mut vmm = VmmInstance::new(VmmConfig::default()).expect("Failed to create vmm");
    /// vmm.join_timeout(Some(Duration::from_secs(5))).unwrap();
    /// ```
    pub fn join_timeout(&mut self, timeout: Option<Duration>) -> Result<(), Error> {
        let timeout_duration = if let Some(duration) = timeout {
            duration
        } else {
            Duration::from_secs(5)
        };
        let start_time = Instant::now();

        if let Some(thread) = self.vmm_thread.take() {
            while !thread.is_finished() {
                if start_time.elapsed() >= timeout_duration {
                    error!("Vmm thread join timeout, kill with SIGTERM");
                    thread.kill(SIGTERM).expect("vmm thread kill err");
                }
                thread::sleep(Duration::from_millis(100));
            }
        }
        Ok(())
    }
}

impl Drop for VmmInstance {
    fn drop(&mut self) {
        if let Some(thread) = self.vmm_thread.as_ref() {
            if !thread.is_finished() && self.send_request(ApiRequest::VmmShutdown).is_err() {
                error!("Failed to shutdown vmm, kill with SIGTERM");
                thread.kill(SIGTERM).expect("vmm thread kill err");
            }
        }

        self.join().unwrap();
        log::logger().flush();
        // SAFETY: trivially safe
        let on_tty = unsafe { libc::isatty(libc::STDIN_FILENO) } != 0;
        if on_tty {
            // Don't forget to set the terminal in canonical mode
            // before to exit.
            std::io::stdin().lock().set_canon_mode().unwrap();
        }
    }
}
