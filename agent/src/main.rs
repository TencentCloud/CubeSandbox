// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

#[macro_use]
extern crate lazy_static;
extern crate capctl;
extern crate oci;
extern crate prometheus;
extern crate protocols;
extern crate regex;
extern crate scan_fmt;
extern crate serde_json;

#[macro_use]
extern crate scopeguard;

#[macro_use]
extern crate slog;

use anyhow::{anyhow, Context, Result};
use clap::{AppSettings, Parser};
use nix::fcntl::OFlag;
use nix::sys::socket::{self, AddressFamily, SockAddr, SockFlag, SockType};
use nix::unistd::{self, dup, Pid};
use std::env;
use std::ffi::OsStr;
use std::fs::{self, File};
use std::os::unix::fs as unixfs;
use std::os::unix::io::AsRawFd;
use std::path::Path;
use std::process::exit;
use std::process::Command;
use std::sync::atomic::{compiler_fence, Ordering};
use std::sync::Arc;
use std::thread;
use std::time::Duration;
use tracing::{instrument, span};

mod config;
mod console;
mod device;
mod fixes;
mod linux_abi;
mod metrics;
mod mount;
mod namespace;
mod netlink;
mod network;
mod pci;
pub mod random;
mod sandbox;
mod signal;
#[cfg(test)]
mod test_utils;
mod time;
mod uevent;
mod util;
mod version;
mod watcher;

use mount::{cgroups_mount, general_mount};
use sandbox::Sandbox;
use signal::setup_signal_handler;
use slog::{error, o, warn, Logger};
use uevent::watch_uevents;

use futures::future::join_all;
use rustjail::pipestream::PipeStream;
use tokio::{
    io::AsyncWrite,
    sync::{
        watch::{channel, Receiver},
        Mutex, RwLock,
    },
    task::JoinHandle,
};

mod rpc;
mod tracer;

const NAME: &str = "cube-agent";
const ENV_WRAPPER_MODE_K: &str = "wrapper_mode";
lazy_static! {
    static ref AGENT_CONFIG: Arc<RwLock<AgentConfig>> = Arc::new(RwLock::new(
        // Note: We can't do AgentOpts.parse() here to send through the processed arguments to AgentConfig
        // clap::Parser::parse() greedily process all command line input including cargo test parameters,
        // so should only be used inside main.
        AgentConfig::from_cmdline("/proc/cmdline", env::args().collect()).unwrap()
    ));
}

#[derive(Parser)]
// The default clap version info doesn't match our form, so we need to override it
#[clap(global_setting(AppSettings::DisableVersionFlag))]
struct AgentOpts {
    /// Print the version information
    #[clap(short, long)]
    version: bool,
    #[clap(subcommand)]
    subcmd: Option<SubCommand>,
    /// Specify a custom agent config file
    #[clap(short, long)]
    config: Option<String>,
}

#[derive(Parser)]
enum SubCommand {
    Init {},
    Exec {},
}

// Create a thread to handle reading from the logger pipe. The thread will
// output to the vsock port specified, or stdout.
async fn create_logger_task(rfd: RawFd, vsock_port: u32, shutdown: Receiver<bool>) -> Result<()> {
    let mut reader = PipeStream::from_fd(rfd);
    let mut writer: Box<dyn AsyncWrite + Unpin + Send> = if vsock_port > 0 {
        let listenfd = socket::socket(
            AddressFamily::Vsock,
            SockType::Stream,
            SockFlag::SOCK_CLOEXEC,
            None,
        )?;

        let addr = SockAddr::new_vsock(libc::VMADDR_CID_ANY, vsock_port);
        socket::bind(listenfd, &addr)?;
        socket::listen(listenfd, 1)?;

        Box::new(util::get_vsock_stream(listenfd).await?)
    } else {
        Box::new(tokio::io::stdout())
        //Box::new(tokio::io::sink())
    };

    let _ = util::interruptible_io_copier(&mut reader, &mut writer, shutdown).await;

    Ok(())
}

async fn real_main() -> std::result::Result<(), Box<dyn std::error::Error>> {
    env::set_var("RUST_BACKTRACE", "full");
    println!(
        "enter real main at:{}",
        moniclock::Clock::new().elapsed().as_millis()
    );

    // List of tasks that need to be stopped for a clean shutdown
    let mut tasks: Vec<JoinHandle<Result<()>>> = vec![];

    console::initialize();

    // support vsock log
    let (rfd, wfd) = unistd::pipe2(OFlag::O_CLOEXEC)?;

    let (shutdown_tx, shutdown_rx) = channel(true);

    let init_mode = unistd::getpid() == Pid::from_raw(1);

    if init_mode {
        // dup a new file descriptor for this temporary logger writer,
        // since this logger would be dropped and it's writer would
        // be closed out of this code block.
        let newwfd = dup(wfd)?;
        let writer = unsafe { File::from_raw_fd(newwfd) };

        // Init a temporary logger used by init agent as init process
        // since before do the base mount, it wouldn't access "/proc/cmdline"
        // to get the customized debug level.
        let (logger, logger_async_guard) =
            logging::create_logger(NAME, "agent", slog::Level::Info, writer);

        // Must mount proc fs before parsing kernel command line
        if env::var(ENV_WRAPPER_MODE_K).is_err() {
            general_mount(&logger).map_err(|e| {
                error!(logger, "fail general mount: {}", e);
                e
            })?;

            enable_rc_local().await;
            println!(
                "rc-local exit at:{}",
                moniclock::Clock::new().elapsed().as_millis()
            );

            lazy_static::initialize(&AGENT_CONFIG);
            init_agent_as_init(&logger, AGENT_CONFIG.read().await.unified_cgroup_hierarchy)?;
        }
        drop(logger_async_guard);
    } else {
        lazy_static::initialize(&AGENT_CONFIG);
    }

    let config = AGENT_CONFIG.read().await;
    let log_vport = config.log_vport as u32;

    let log_handle = tokio::spawn(create_logger_task(rfd, log_vport, shutdown_rx.clone()));

    tasks.push(log_handle);

    let writer = unsafe { File::from_raw_fd(wfd) };

    // Recreate a logger with the log level get from "/proc/cmdline".
    let (logger, logger_async_guard) =
        logging::create_logger(NAME, "agent", config.log_level, writer);

    // This variable is required as it enables the global (and crucially static) logger,
    // which is required to satisfy the the lifetime constraints of the auto-generated gRPC code.
    let global_logger = slog_scope::set_global_logger(logger.new(o!()));

    // Allow the global logger to be modified later (for shutdown)
    global_logger.cancel_reset();

    let mut ttrpc_log_guard: Result<(), log::SetLoggerError> = Ok(());

    if config.log_level == slog::Level::Trace {
        // Redirect ttrpc log calls to slog iff full debug requested
        ttrpc_log_guard = Ok(slog_stdlog::init()?);
    }

    if config.tracing {
        tracer::setup_tracing(NAME, &logger)?;
    }

    let root_span = span!(tracing::Level::TRACE, "root-span");

    // XXX: Start the root trace transaction.
    //
    // XXX: Note that *ALL* spans needs to start after this point!!
    let span_guard = root_span.enter();
    println!(
        "start sandbox at:{}",
        moniclock::Clock::new().elapsed().as_millis()
    );

    // Start the sandbox and wait for its ttRPC server to end
    start_sandbox(&logger, &config, init_mode, &mut tasks, shutdown_rx.clone()).await?;

    // Install a NOP logger for the remainder of the shutdown sequence
    // to ensure any log calls made by local crates using the scope logger
    // don't fail.
    let global_logger_guard2 =
        slog_scope::set_global_logger(slog::Logger::root(slog::Discard, o!()));
    global_logger_guard2.cancel_reset();

    drop(logger_async_guard);

    drop(ttrpc_log_guard);

    // Trigger a controlled shutdown
    shutdown_tx
        .send(true)
        .map_err(|e| anyhow!(e).context("failed to request shutdown"))?;

    // Wait for all threads to finish
    let results = join_all(tasks).await;

    // force flushing spans
    drop(span_guard);
    drop(root_span);

    if config.tracing {
        tracer::end_tracing();
    }

    eprintln!("{} shutdown complete", NAME);

    let mut wait_errors: Vec<tokio::task::JoinError> = vec![];
    for result in results {
        if let Err(e) = result {
            eprintln!("wait task error: {:#?}", e);
            wait_errors.push(e);
        }
    }

    if wait_errors.is_empty() {
        Ok(())
    } else {
        Err(anyhow!("wait all tasks failed: {:#?}", wait_errors).into())
    }
}

fn main() -> std::result::Result<(), Box<dyn std::error::Error>> {
    let args = AgentOpts::parse();

    if args.version {
        println!(
            "version:{} ,commit: {})",
            version::AGENT_VERSION,
            version::VERSION_COMMIT,
        );
        exit(0);
    }

    if let Some(SubCommand::Init {}) = args.subcmd {
        reset_sigpipe();
        rustjail::container::init_child();
        exit(0);
    }

    if let Some(SubCommand::Exec {}) = args.subcmd {
        cube::rootfs::do_exec_mount();
        exit(0);
    }

    println!(
        "agent start at:{}",
        moniclock::Clock::new().elapsed().as_millis()
    );

    compiler_fence(Ordering::SeqCst);

    let rt: tokio::runtime::Runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;

    rt.block_on(real_main())
}

#[instrument]
async fn enable_rc_local() {
    let _lock = rustjail::container::WAIT_PID_LOCKER.lock().await;
    let output = Command::new("/etc/rc.local").spawn();

    match output {
        Ok(child) => {
            let output = child.wait_with_output();
            match output {
                Ok(child) => {
                    if let Some(code) = child.status.code() {
                        println!("/etc/rc.local exit:{}", code)
                    }
                }
                Err(e) => {
                    println!("Failed to execute rc.local: {}", e);
                }
            }
        }
        Err(e) => {
            println!("Failed to execute rc.local: {}", e);
        }
    }
}

#[instrument]
async fn start_sandbox(
    logger: &Logger,
    config: &AgentConfig,
    init_mode: bool,
    tasks: &mut Vec<JoinHandle<Result<()>>>,
    shutdown: Receiver<bool>,
) -> Result<()> {
    let debug_console_vport = config.debug_console_vport as u32;

    if config.debug_console {
        let debug_console_task = tokio::task::spawn(console::debug_console_handler(
            logger.clone(),
            debug_console_vport,
            shutdown.clone(),
        ));

        tasks.push(debug_console_task);
    }

    // Initialize unique sandbox structure.
    let s = Sandbox::new(logger).context("Failed to create sandbox")?;
    if init_mode {
        s.rtnl.handle_localhost().await?;
    }

    let sandbox = Arc::new(Mutex::new(s));

    let signal_handler_task = tokio::spawn(setup_signal_handler(
        logger.clone(),
        sandbox.clone(),
        shutdown.clone(),
    ));

    tasks.push(signal_handler_task);

    let (ue_tx, ue_rx) = tokio::sync::oneshot::channel();
    let uevents_handler_task =
        tokio::spawn(watch_uevents(sandbox.clone(), shutdown.clone(), ue_tx));
    ue_rx.await.unwrap();
    tasks.push(uevents_handler_task);

    let (tx, rx) = tokio::sync::oneshot::channel();
    sandbox.lock().await.sender = Some(tx);
    println!(
        "rpc start at:{}",
        moniclock::Clock::new().elapsed().as_millis()
    );
    // vsock:///dev/vsock, port
    let mut server = rpc::start(sandbox.clone(), config.server_addr.as_str())?;
    server.start().await?;

    rx.await?;

    thread::sleep(Duration::from_millis(5));

    let ret = unsafe { libc::reboot(libc::LINUX_REBOOT_CMD_POWER_OFF) };
    if ret != 0 {
        println!("shutdown vm failed, ret:{:}", ret);
    }

    Ok(())
}

// init_agent_as_init will do the initializations such as setting up the rootfs
// when this agent has been run as the init process.
fn init_agent_as_init(logger: &Logger, unified_cgroup_hierarchy: bool) -> Result<()> {
    cgroups_mount(logger, unified_cgroup_hierarchy).map_err(|e| {
        error!(
            logger,
            "fail cgroups mount, unified_cgroup_hierarchy {}: {}", unified_cgroup_hierarchy, e
        );
        e
    })?;

    fs::remove_file(Path::new("/dev/ptmx"))?;
    unixfs::symlink(Path::new("/dev/pts/ptmx"), Path::new("/dev/ptmx"))?;

    unistd::setsid()?;

    unsafe {
        libc::ioctl(std::io::stdin().as_raw_fd(), libc::TIOCSCTTY, 1);
    }

    env::set_var("PATH", "/bin:/sbin/:/usr/bin/:/usr/sbin/");

    let contents =
        std::fs::read_to_string("/etc/hostname").unwrap_or_else(|_| String::from("localhost"));
    let contents_array: Vec<&str> = contents.split(' ').collect();
    let hostname = contents_array[0].trim();

    if unistd::sethostname(OsStr::new(hostname)).is_err() {
        warn!(logger, "failed to set hostname");
    }

    Ok(())
}

// The Rust standard library had suppressed the default SIGPIPE behavior,
// see https://github.com/rust-lang/rust/pull/13158.
// Since the parent's signal handler would be inherited by it's child process,
// thus we should re-enable the standard SIGPIPE behavior as a workaround to
// fix the issue of https://github.com/kata-containers/kata-containers/issues/1887.
fn reset_sigpipe() {
    unsafe {
        libc::signal(libc::SIGPIPE, libc::SIG_DFL);
    }
}

use crate::config::AgentConfig;
use std::os::unix::io::{FromRawFd, RawFd};

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_utils::test_utils::TestUserType;

    #[tokio::test]
    async fn test_create_logger_task() {
        #[derive(Debug)]
        struct TestData {
            vsock_port: u32,
            test_user: TestUserType,
            result: Result<()>,
        }

        let tests = &[
            TestData {
                // non-root user cannot use privileged vsock port
                vsock_port: 1,
                test_user: TestUserType::NonRootOnly,
                result: Err(anyhow!(nix::errno::Errno::from_i32(libc::EACCES))),
            },
            TestData {
                // passing vsock_port 0 causes logger task to write to stdout
                vsock_port: 0,
                test_user: TestUserType::Any,
                result: Ok(()),
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            if d.test_user == TestUserType::RootOnly {
                skip_if_not_root!();
            } else if d.test_user == TestUserType::NonRootOnly {
                skip_if_root!();
            }

            let msg = format!("test[{}]: {:?}", i, d);
            let (rfd, wfd) = unistd::pipe2(OFlag::O_CLOEXEC).unwrap();
            defer!({
                // rfd is closed by the use of PipeStream in the crate_logger_task function,
                // but we will attempt to close in case of a failure
                let _ = unistd::close(rfd);
                unistd::close(wfd).unwrap();
            });

            let (shutdown_tx, shutdown_rx) = channel(true);

            shutdown_tx.send(true).unwrap();
            let result = create_logger_task(rfd, d.vsock_port, shutdown_rx).await;

            let msg = format!("{}, result: {:?}", msg, result);
            assert_result!(d.result, result, msg);
        }
    }
}
