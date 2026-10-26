// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use lazy_static::lazy_static;
use slog::{info, warn, Logger};
use tokio_vsock::{VsockAddr, VsockListener, VsockStream};

pub const PASSFD_LISTENER_PORT: u32 = 1027;
const PASSFD_STREAM_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_PENDING_STREAMS: usize = 256;
const ACCEPT_ERROR_INITIAL_BACKOFF: Duration = Duration::from_millis(10);
const ACCEPT_ERROR_MAX_BACKOFF: Duration = Duration::from_secs(1);
static NEXT_WAITER_ID: AtomicU64 = AtomicU64::new(1);

fn next_accept_backoff(current: Duration) -> Duration {
    current.saturating_mul(2).min(ACCEPT_ERROR_MAX_BACKOFF)
}

struct PendingStream {
    stream: VsockStream,
    arrived_at: Instant,
}

struct Waiter {
    id: u64,
    tx: tokio::sync::oneshot::Sender<VsockStream>,
}

struct PassfdState {
    streams: HashMap<u32, PendingStream>,
    waiters: HashMap<u32, Waiter>,
    expired_ports: HashMap<u32, Instant>,
}

impl PassfdState {
    fn new() -> Self {
        Self {
            streams: HashMap::new(),
            waiters: HashMap::new(),
            expired_ports: HashMap::new(),
        }
    }

    fn purge_expired(&mut self, now: Instant) {
        self.streams
            .retain(|_, stream| now.duration_since(stream.arrived_at) < PASSFD_STREAM_TIMEOUT);
        self.expired_ports.retain(|_, deadline| *deadline > now);
    }

    fn insert_stream(&mut self, port: u32, stream: VsockStream, now: Instant) -> Option<u32> {
        let evicted_port =
            if self.streams.len() >= MAX_PENDING_STREAMS && !self.streams.contains_key(&port) {
                self.streams
                    .iter()
                    .min_by_key(|(_, pending)| pending.arrived_at)
                    .map(|(port, _)| *port)
            } else {
                None
            };

        if let Some(port) = evicted_port {
            self.streams.remove(&port);
        }
        self.streams.insert(
            port,
            PendingStream {
                stream,
                arrived_at: now,
            },
        );
        evicted_port
    }
}

lazy_static! {
    static ref PASSFD_STATE: Mutex<PassfdState> = Mutex::new(PassfdState::new());
}

pub async fn start_passfd_listener(logger: Logger) -> anyhow::Result<()> {
    let addr = VsockAddr::new(libc::VMADDR_CID_ANY, PASSFD_LISTENER_PORT);
    let listener = VsockListener::bind(addr)?;

    info!(
        logger,
        "Listening for passfd connections on port {}", PASSFD_LISTENER_PORT
    );

    tokio::spawn(async move {
        let mut error_backoff = ACCEPT_ERROR_INITIAL_BACKOFF;
        loop {
            match listener.accept().await {
                Ok((stream, addr)) => {
                    error_backoff = ACCEPT_ERROR_INITIAL_BACKOFF;
                    if addr.cid() != libc::VMADDR_CID_HOST {
                        warn!(
                            logger,
                            "Rejected passfd connection from untrusted CID: {}",
                            addr.cid()
                        );
                        continue;
                    }
                    let port = addr.port();
                    let (accepted, evicted_port) = {
                        let mut state = PASSFD_STATE.lock().unwrap();
                        let now = Instant::now();
                        state.purge_expired(now);
                        if state.expired_ports.contains_key(&port) {
                            (false, None)
                        } else if let Some(waiter) = state.waiters.remove(&port) {
                            let _ = waiter.tx.send(stream);
                            (true, None)
                        } else {
                            let evicted_port = state.insert_stream(port, stream, now);
                            (true, evicted_port)
                        }
                    };
                    if let Some(evicted_port) = evicted_port {
                        warn!(
                            logger,
                            "Evicted oldest pending passfd connection on port {} after reaching capacity {}",
                            evicted_port,
                            MAX_PENDING_STREAMS
                        );
                    }
                    if accepted {
                        info!(logger, "Accepted passfd connection on port {}", port);
                    } else {
                        warn!(
                            logger,
                            "Rejected late passfd connection on expired port {}", port
                        );
                    }
                }
                Err(e) => {
                    warn!(logger, "Error accepting passfd connection: {:?}", e);
                    tokio::time::sleep(error_backoff).await;
                    error_backoff = next_accept_backoff(error_backoff);
                }
            }
        }
    });

    Ok(())
}

pub async fn take_stream(port: u32) -> anyhow::Result<VsockStream> {
    let id = NEXT_WAITER_ID.fetch_add(1, Ordering::Relaxed);
    let rx = {
        let mut state = PASSFD_STATE.lock().unwrap();
        let now = Instant::now();
        state.purge_expired(now);
        if state.expired_ports.contains_key(&port) {
            return Err(anyhow::anyhow!(
                "Passfd port {} is quarantined after a timed out request",
                port
            ));
        }
        if let Some(stream) = state.streams.remove(&port) {
            return Ok(stream.stream);
        }

        let (tx, rx) = tokio::sync::oneshot::channel();
        if state.waiters.contains_key(&port) {
            return Err(anyhow::anyhow!(
                "Another passfd request is already waiting on port {}",
                port
            ));
        }
        state.waiters.insert(port, Waiter { id, tx });
        rx
    };

    let mut guard = WaiterGuard { port, id };
    match tokio::time::timeout(PASSFD_STREAM_TIMEOUT, rx).await {
        Ok(Ok(stream)) => {
            guard.disarm();
            Ok(stream)
        }
        Ok(Err(_)) => Err(anyhow::anyhow!(
            "Passfd stream waiter was cancelled for port {}",
            port
        )),
        Err(_) => {
            guard.expire();
            Err(anyhow::anyhow!(
                "Timeout waiting for passfd stream on port {}",
                port
            ))
        }
    }
}

struct WaiterGuard {
    port: u32,
    id: u64,
}

impl WaiterGuard {
    fn remove(&self, expire: bool) {
        let mut state = PASSFD_STATE.lock().unwrap();
        if state.waiters.get(&self.port).map(|waiter| waiter.id) == Some(self.id) {
            state.waiters.remove(&self.port);
            if expire {
                state
                    .expired_ports
                    .insert(self.port, Instant::now() + PASSFD_STREAM_TIMEOUT);
                state.streams.remove(&self.port);
            }
        }
    }

    fn disarm(&mut self) {
        self.id = 0;
    }

    fn expire(&mut self) {
        self.remove(true);
        self.disarm();
    }
}

impl Drop for WaiterGuard {
    fn drop(&mut self) {
        if self.id != 0 {
            self.remove(true);
        }
    }
}

pub fn has_passfd_ports(stdin_port: u32, stdout_port: u32, stderr_port: u32) -> bool {
    stdin_port > 0 || stdout_port > 0 || stderr_port > 0
}

async fn take_optional_stream(port: u32) -> anyhow::Result<Option<VsockStream>> {
    if port > 0 {
        take_stream(port).await.map(Some)
    } else {
        Ok(None)
    }
}

/// Helper function to create ProcessIo by connecting to the passed vsock ports
pub async fn create_process_io(
    stdin_port: u32,
    stdout_port: u32,
    stderr_port: u32,
) -> anyhow::Result<rustjail::process::ProcessIo> {
    let (stdin, stdout, stderr) = tokio::try_join!(
        take_optional_stream(stdin_port),
        take_optional_stream(stdout_port),
        take_optional_stream(stderr_port),
    )?;

    Ok(rustjail::process::ProcessIo::new(stdin, stdout, stderr))
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;

    #[tokio::test]
    async fn test_take_stream_timeout() {
        let port = 9999;

        let start = std::time::Instant::now();
        let result = take_stream(port).await;
        let elapsed = start.elapsed();

        assert!(result.is_err());
        assert_eq!(
            result.err().unwrap().to_string(),
            format!("Timeout waiting for passfd stream on port {}", port)
        );

        // Timeout is 5 seconds, should be roughly around 5s
        assert!(elapsed >= Duration::from_secs(5));

        // Ensure waiter is removed after timeout
        let state = PASSFD_STATE.lock().unwrap();
        assert!(!state.waiters.contains_key(&port));
        assert!(state.expired_ports.contains_key(&port));
    }

    #[tokio::test]
    async fn test_cancelled_take_stream_removes_waiter() {
        let port = 9998;
        let task = tokio::spawn(take_stream(port));
        tokio::task::yield_now().await;
        task.abort();
        let _ = task.await;

        let state = PASSFD_STATE.lock().unwrap();
        assert!(!state.waiters.contains_key(&port));
        assert!(state.expired_ports.contains_key(&port));
    }

    #[test]
    fn test_accept_error_backoff_is_bounded() {
        let mut backoff = ACCEPT_ERROR_INITIAL_BACKOFF;
        assert_eq!(next_accept_backoff(backoff), Duration::from_millis(20));
        for _ in 0..16 {
            backoff = next_accept_backoff(backoff);
        }
        assert_eq!(backoff, ACCEPT_ERROR_MAX_BACKOFF);
        assert_eq!(next_accept_backoff(backoff), ACCEPT_ERROR_MAX_BACKOFF);
    }
}
