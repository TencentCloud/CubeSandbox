// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::ffi::OsString;
use std::io;
use std::net::SocketAddr;
use std::os::fd::RawFd;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use crate::common::utils::Utils;
use crate::log::Log;
use crate::{infof, warnf};

pub const TRACE_FORWARDER_GUARD_ARG: &str = "--internal-trace-forwarder-guard";

// Guest (cube-agent) tracing: the agent exports spans over vsock to a
// per-sandbox host forwarder, which ships them to a Jaeger thrift endpoint.
const ENV_TRACE_FORWARDER: &str = "CUBE_TRACE_FORWARDER";
const ENV_TRACE_JAEGER_ENDPOINT: &str = "CUBE_TRACE_JAEGER_ENDPOINT";
const DEFAULT_JAEGER_ENDPOINT: &str = "127.0.0.1:6831";
const FORWARDER_VSOCK_PORT: u16 = 10240;
const FORWARDER_TRACE_NAME: &str = "cube-agent";
const CMDLINE_AGENT_TRACE: &str = "agent.trace";

struct GuestTraceConfig {
    binary: PathBuf,
    jaeger_endpoint: String,
}

/// Guest tracing needs both shim tracing and a deployed forwarder binary.
fn guest_trace_config() -> Option<GuestTraceConfig> {
    if !crate::telemetry::tracing_enabled() {
        return None;
    }
    let raw = std::env::var(ENV_TRACE_FORWARDER).unwrap_or_default();
    if raw.trim().is_empty() {
        return None;
    }
    let binary = PathBuf::from(raw.trim());
    if !binary.is_file() {
        log::warn!(
            "trace forwarder {} is not a file; guest tracing disabled",
            binary.display()
        );
        return None;
    }
    let jaeger = std::env::var(ENV_TRACE_JAEGER_ENDPOINT).unwrap_or_default();
    let jaeger_endpoint = if jaeger.trim().is_empty() {
        DEFAULT_JAEGER_ENDPOINT.to_string()
    } else {
        jaeger.trim().to_string()
    };
    Some(GuestTraceConfig {
        binary,
        jaeger_endpoint,
    })
}

/// Kernel cmdline switch enabling tracing inside the guest agent. An explicit
/// `agent.trace` entry in extra kernel params wins over the automatic switch.
pub(crate) fn agent_trace_cmdline(extra_kernel_params: &[String]) -> Option<String> {
    let user_set = extra_kernel_params
        .iter()
        .flat_map(|p| p.split_whitespace())
        .any(|param| {
            param == CMDLINE_AGENT_TRACE
                || param
                    .split_once('=')
                    .is_some_and(|(key, _)| key == CMDLINE_AGENT_TRACE)
        });
    (!user_set).then(|| format!("{CMDLINE_AGENT_TRACE}=1"))
}

fn parse_jaeger_endpoint(endpoint: &str) -> Option<SocketAddr> {
    let addr = endpoint.parse::<SocketAddr>().ok()?;
    (addr.port() != 0).then_some(addr)
}

fn forwarder_args(socket_path: &Path, jaeger_endpoint: SocketAddr) -> Vec<String> {
    // Kata joins the two flags with ":" and parses them as a SocketAddr.
    // Keep the brackets (and any scope ID) of a formatted IPv6 address.
    let endpoint = jaeger_endpoint.to_string();
    let (host, port) = endpoint
        .rsplit_once(':')
        .expect("SocketAddr includes a port");
    vec![
        "--socket-path".to_string(),
        socket_path.to_string_lossy().into_owned(),
        "--vsock-port".to_string(),
        FORWARDER_VSOCK_PORT.to_string(),
        "--jaeger-host".to_string(),
        host.to_string(),
        "--jaeger-port".to_string(),
        port.to_string(),
        "--trace-name".to_string(),
        FORWARDER_TRACE_NAME.to_string(),
        "--log-level".to_string(),
        "warn".to_string(),
    ]
}

/// Host-side span forwarder for one sandbox. The guest agent connects to host
/// vsock port 10240; the hypervisor bridges it to the hybrid socket
/// `{VM_PATH}/{sandbox_id}/cube.sock_10240` that this process listens on.
pub struct TraceForwarder {
    child: tokio::process::Child,
}

impl TraceForwarder {
    /// Starts the local forwarder without resolving DNS or waiting for its backend.
    /// Setup failures disable guest tracing for this sandbox.
    pub fn spawn(sandbox_id: &str, log: &Log) -> Option<TraceForwarder> {
        let cfg = guest_trace_config()?;
        let Some(endpoint) = parse_jaeger_endpoint(&cfg.jaeger_endpoint) else {
            warnf!(
                log,
                "invalid {}={}; expected a literal IP and nonzero port, guest tracing disabled",
                ENV_TRACE_JAEGER_ENDPOINT,
                cfg.jaeger_endpoint
            );
            return None;
        };
        let socket_path = Utils::vsock_path(sandbox_id);
        if let Some(dir) = socket_path.parent() {
            if let Err(e) = std::fs::create_dir_all(dir) {
                warnf!(
                    log,
                    "cannot create {}: {}; guest tracing disabled",
                    dir.display(),
                    e
                );
                return None;
            }
        }
        let shim_binary = match std::env::current_exe() {
            Ok(path) => path,
            Err(err) => {
                warnf!(log, "cannot locate shim for trace forwarder: {}", err);
                return None;
            }
        };
        // The supervisor owns the forwarder. Its stdin is a pipe whose writer
        // lives in this shim, so SIGKILL/OOM closes it even without Rust Drop.
        match tokio::process::Command::new(shim_binary)
            .arg(TRACE_FORWARDER_GUARD_ARG)
            .arg(&cfg.binary)
            .args(forwarder_args(&socket_path, endpoint))
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::inherit())
            .spawn()
        {
            Ok(child) => {
                infof!(log, "trace forwarder started for sandbox {}", sandbox_id);
                Some(TraceForwarder { child })
            }
            Err(e) => {
                warnf!(
                    log,
                    "cannot spawn trace forwarder {}: {}; guest tracing disabled",
                    cfg.binary.display(),
                    e
                );
                None
            }
        }
    }

    pub fn is_running(&mut self) -> bool {
        matches!(self.child.try_wait(), Ok(None))
    }

    pub async fn stop(mut self) {
        drop(self.child.stdin.take());
        // The guard kills and reaps its child on EOF. Give it time to exit
        // without letting an external process hold up sandbox deletion.
        let _ = tokio::time::timeout(Duration::from_millis(500), self.child.wait()).await;
    }
}

pub fn run_trace_forwarder_guard(args: &[OsString]) -> io::Result<()> {
    let (binary, args) = args.split_first().ok_or_else(|| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            "missing trace forwarder binary",
        )
    })?;
    let child = std::process::Command::new(binary)
        .args(args)
        .stdin(Stdio::null())
        .spawn()?;
    let status = wait_for_forwarder_or_shim(child, libc::STDIN_FILENO)?;
    if status.success()
        || std::os::unix::process::ExitStatusExt::signal(&status) == Some(libc::SIGKILL)
    {
        return Ok(());
    }
    Err(io::Error::other(format!(
        "trace forwarder exited with {status}"
    )))
}

fn wait_for_forwarder_or_shim(
    mut child: std::process::Child,
    lifetime_fd: RawFd,
) -> io::Result<std::process::ExitStatus> {
    let mut fd = libc::pollfd {
        fd: lifetime_fd,
        events: libc::POLLIN,
        revents: 0,
    };
    let result = loop {
        match child.try_wait() {
            Ok(Some(status)) => return Ok(status),
            Ok(None) => {}
            Err(err) => break Err(err),
        }
        // Poll wakes immediately on EOF. The timeout also lets us reap a
        // forwarder that exits independently while the shim is still alive.
        let ready = unsafe { libc::poll(&mut fd, 1, 1000) };
        if ready > 0 {
            break Ok(());
        }
        if ready < 0 {
            let err = io::Error::last_os_error();
            if err.kind() != io::ErrorKind::Interrupted {
                break Err(err);
            }
        }
    };
    let _ = child.kill();
    let status = child.wait();
    result.and(status)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn forwarder_args_bind_the_hybrid_socket_and_name_the_guest_service() {
        let args = forwarder_args(
            Path::new("/run/vc/vm/sb1/cube.sock"),
            SocketAddr::from(([10, 0, 0, 1], 6831)),
        );
        fn value_of<'a>(args: &'a [String], flag: &str) -> &'a str {
            args.iter()
                .position(|a| a == flag)
                .map(|i| args[i + 1].as_str())
                .unwrap()
        }
        assert_eq!(value_of(&args, "--socket-path"), "/run/vc/vm/sb1/cube.sock");
        assert_eq!(value_of(&args, "--vsock-port"), "10240");
        assert_eq!(value_of(&args, "--jaeger-host"), "10.0.0.1");
        assert_eq!(value_of(&args, "--jaeger-port"), "6831");
        assert_eq!(value_of(&args, "--trace-name"), "cube-agent");

        let args = forwarder_args(
            Path::new("/run/vc/vm/sb1/cube.sock"),
            "[::1]:6831".parse().unwrap(),
        );
        assert_eq!(value_of(&args, "--jaeger-host"), "[::1]");
        assert_eq!(value_of(&args, "--jaeger-port"), "6831");
    }

    #[test]
    fn jaeger_endpoint_requires_a_literal_ip_and_nonzero_port() {
        assert_eq!(
            parse_jaeger_endpoint("127.0.0.1:6831"),
            Some(SocketAddr::from(([127, 0, 0, 1], 6831)))
        );
        assert!(parse_jaeger_endpoint("[::1]:6831").is_some());
        assert!(parse_jaeger_endpoint("tempo.internal:6831").is_none());
        assert!(parse_jaeger_endpoint("127.0.0.1").is_none());
        assert!(parse_jaeger_endpoint(":6831").is_none());
        assert!(parse_jaeger_endpoint("127.0.0.1:0").is_none());
        assert!(parse_jaeger_endpoint("127.0.0.1:99999").is_none());
    }

    #[test]
    fn forwarder_is_killed_and_reaped_when_shim_lifetime_pipe_closes() {
        use std::os::fd::AsRawFd;
        use std::os::unix::{net::UnixStream, process::ExitStatusExt};

        let (guard_end, shim_end) = UnixStream::pair().unwrap();
        let forwarder = std::process::Command::new("/bin/sleep")
            .arg("5")
            .spawn()
            .unwrap();
        drop(shim_end);
        let status = wait_for_forwarder_or_shim(forwarder, guard_end.as_raw_fd()).unwrap();
        assert_eq!(status.signal(), Some(libc::SIGKILL));
    }

    #[test]
    fn preserves_an_explicit_agent_trace_kernel_option() {
        assert_eq!(agent_trace_cmdline(&[]).as_deref(), Some("agent.trace=1"));
        assert_eq!(agent_trace_cmdline(&["agent.trace=0".into()]), None);
        assert_eq!(
            agent_trace_cmdline(&["quiet agent.trace=false".into()]),
            None
        );
        assert_eq!(agent_trace_cmdline(&["agent.trace".into()]), None);
    }
}
