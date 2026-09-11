// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::net::SocketAddr;
use std::os::fd::{AsRawFd, RawFd};
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use axum::Router;
use futures::{Future, FutureExt};
use tokio::signal;
use tokio::sync::{watch, Notify};
use tokio::task::JoinSet;
use tracing::{error, info};

use crate::transport;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum ServerPhase {
    Booting = 0,
    Listening = 1,
    Ready = 2,
    Draining = 3,
    Stopped = 4,
    Failed = 5,
}

pub struct LifecycleState {
    state: Mutex<LifecycleStatus>,
    failure_notify: Notify,
}

struct LifecycleStatus {
    phase: ServerPhase,
    failure: Option<EnvdFailure>,
}

impl LifecycleState {
    pub fn new() -> Self {
        Self {
            state: Mutex::new(LifecycleStatus {
                phase: ServerPhase::Booting,
                failure: None,
            }),
            failure_notify: Notify::new(),
        }
    }

    pub fn try_transition(&self, phase: ServerPhase) -> bool {
        let mut state = self.lock_state();
        if !valid_transition(state.phase, phase) {
            return false;
        }
        state.phase = phase;
        true
    }

    pub fn is_ready(&self) -> bool {
        let state = self.lock_state();
        state.failure.is_none() && state.phase == ServerPhase::Ready
    }

    pub fn phase(&self) -> ServerPhase {
        self.lock_state().phase
    }

    pub fn fail(&self, task: impl Into<String>) -> bool {
        self.fail_with(EnvdFailure::fatal_return(task))
    }

    pub fn fail_with(&self, failure: EnvdFailure) -> bool {
        {
            let mut state = self.lock_state();
            if state.failure.is_some() {
                return false;
            }
            state.failure = Some(failure);
            state.phase = ServerPhase::Failed;
        }
        self.failure_notify.notify_waiters();
        true
    }

    pub fn failure(&self) -> Option<EnvdFailure> {
        self.lock_state().failure.clone()
    }

    pub async fn wait_for_failure(&self) -> EnvdFailure {
        loop {
            let notified = self.failure_notify.notified();
            if let Some(failure) = self.failure() {
                return failure;
            }
            notified.await;
        }
    }

    fn lock_state(&self) -> MutexGuard<'_, LifecycleStatus> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

fn valid_transition(current: ServerPhase, next: ServerPhase) -> bool {
    current == next
        || matches!(
            (current, next),
            (ServerPhase::Booting, ServerPhase::Listening)
                | (ServerPhase::Listening, ServerPhase::Ready)
                | (ServerPhase::Listening, ServerPhase::Draining)
                | (ServerPhase::Ready, ServerPhase::Draining)
                | (ServerPhase::Draining, ServerPhase::Stopped)
        )
}

impl Default for LifecycleState {
    fn default() -> Self {
        Self::new()
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EnvdFailureKind {
    FatalReturn,
    Panic,
    RequestPanic,
    UnexpectedCompletion,
    Invariant,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EnvdFailure {
    task: String,
    kind: EnvdFailureKind,
}

impl EnvdFailure {
    pub fn fatal_return(task: impl Into<String>) -> Self {
        Self::new(task, EnvdFailureKind::FatalReturn)
    }

    pub fn panic(task: impl Into<String>) -> Self {
        Self::new(task, EnvdFailureKind::Panic)
    }

    pub fn request_panic() -> Self {
        Self::new("request", EnvdFailureKind::RequestPanic)
    }

    pub fn unexpected_completion(task: impl Into<String>) -> Self {
        Self::new(task, EnvdFailureKind::UnexpectedCompletion)
    }

    pub fn invariant(task: impl Into<String>) -> Self {
        Self::new(task, EnvdFailureKind::Invariant)
    }

    fn new(task: impl Into<String>, kind: EnvdFailureKind) -> Self {
        Self {
            task: task.into(),
            kind,
        }
    }

    pub fn task(&self) -> &str {
        &self.task
    }

    pub fn kind(&self) -> EnvdFailureKind {
        self.kind
    }
}

impl std::fmt::Display for EnvdFailure {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "critical task {} failed", self.task)
    }
}

impl std::error::Error for EnvdFailure {}

pub struct Supervisor {
    tasks: JoinSet<Option<EnvdFailure>>,
    lifecycle: Arc<LifecycleState>,
    stopping: Arc<AtomicBool>,
}

impl Supervisor {
    pub fn new(lifecycle: Arc<LifecycleState>) -> Self {
        Self {
            tasks: JoinSet::new(),
            lifecycle,
            stopping: Arc::new(AtomicBool::new(false)),
        }
    }

    pub fn spawn<F, E>(&mut self, name: impl Into<String>, task: F)
    where
        F: Future<Output = Result<(), E>> + Send + 'static,
        E: Send + 'static,
    {
        let name = name.into();
        let lifecycle = self.lifecycle.clone();
        let stopping = self.stopping.clone();
        self.tasks.spawn(async move {
            let failure = match std::panic::AssertUnwindSafe(task).catch_unwind().await {
                Ok(Ok(())) => {
                    if stopping.load(Ordering::Acquire) {
                        return None;
                    }
                    EnvdFailure::unexpected_completion(name)
                }
                Ok(Err(_)) => EnvdFailure::fatal_return(name),
                Err(_) => EnvdFailure::panic(name),
            };
            lifecycle.fail_with(failure);
            lifecycle.failure()
        });
    }

    pub async fn next_failure(&mut self) -> EnvdFailure {
        loop {
            match self.tasks.join_next().await {
                Some(Ok(Some(failure))) => return failure,
                Some(Ok(None)) => {}
                Some(Err(_)) => {
                    self.lifecycle
                        .fail_with(EnvdFailure::panic("critical task"));
                    return self.lifecycle.failure().expect("failure just recorded");
                }
                None => return std::future::pending().await,
            }
        }
    }

    fn register(&mut self, task: CriticalTask) {
        self.spawn(task.name, task.future);
    }

    fn begin_shutdown(&self) {
        self.stopping.store(true, Ordering::Release);
    }

    async fn shutdown(&mut self, limit: Duration, force: impl FnOnce()) {
        const FORCE_PERIOD: Duration = Duration::from_millis(250);
        self.begin_shutdown();
        let graceful_period = limit.saturating_sub(FORCE_PERIOD);
        let drained = tokio::time::timeout(graceful_period, async {
            while self.tasks.join_next().await.is_some() {}
        })
        .await;
        if drained.is_ok() {
            return;
        }

        force();
        let forced = tokio::time::timeout(FORCE_PERIOD, async {
            while self.tasks.join_next().await.is_some() {}
        })
        .await;
        if forced.is_err() {
            self.tasks.abort_all();
            while self.tasks.join_next().await.is_some() {}
        }
    }
}

pub struct CriticalTask {
    name: String,
    future: Pin<Box<dyn Future<Output = Result<(), ()>> + Send + 'static>>,
}

impl CriticalTask {
    /// Register daemon-owned critical work only. Managed user processes and their
    /// wait ownership must stay outside this shutdown boundary.
    pub fn new<F, E>(name: impl Into<String>, future: F) -> Self
    where
        F: Future<Output = Result<(), E>> + Send + 'static,
        E: Send + 'static,
    {
        Self {
            name: name.into(),
            future: Box::pin(async move { future.await.map_err(|_| ()) }),
        }
    }
}

pub struct ServerRuntime {
    lifecycle: Arc<LifecycleState>,
    readiness_checks: Vec<Arc<dyn ReadinessCheck>>,
    critical_tasks: Vec<CriticalTask>,
}

impl ServerRuntime {
    pub fn new(lifecycle: Arc<LifecycleState>) -> Self {
        Self {
            lifecycle,
            readiness_checks: Vec::new(),
            critical_tasks: Vec::new(),
        }
    }

    pub fn with_readiness_check(mut self, check: Arc<dyn ReadinessCheck>) -> Self {
        self.readiness_checks.push(check);
        self
    }

    /// Register daemon-owned critical work with the fail-closed supervisor.
    /// Managed user processes and their wait handles must not be registered.
    pub fn with_critical_task(mut self, task: CriticalTask) -> Self {
        self.critical_tasks.push(task);
        self
    }
}

impl Default for ServerRuntime {
    fn default() -> Self {
        Self::new(Arc::new(LifecycleState::new()))
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ServerError {
    #[error("server I/O failure")]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    EnvdFailure(EnvdFailure),
}

/// A current, non-destructive health check for a daemon manager.
///
/// Process, filesystem, and watcher managers register here when implemented;
/// cached lifecycle state alone is never sufficient for readiness.
pub trait ReadinessCheck: Send + Sync {
    fn self_check(&self) -> Pin<Box<dyn Future<Output = bool> + Send + '_>>;
}

struct ListenerCheck(AtomicI32);

impl ListenerCheck {
    fn new() -> Self {
        Self(AtomicI32::new(-1))
    }

    fn attach(&self, listener: RawFd) {
        self.0.store(listener, Ordering::Release);
    }
}

impl ReadinessCheck for ListenerCheck {
    fn self_check(&self) -> Pin<Box<dyn Future<Output = bool> + Send + '_>> {
        Box::pin(async move {
            let listener = self.0.load(Ordering::Acquire);
            if listener < 0 {
                return false;
            }
            // Observe the transport's original descriptor without duplicating
            // ownership, accepting a connection, or changing socket state.
            let mut listening: libc::c_int = 0;
            let mut size = std::mem::size_of_val(&listening) as libc::socklen_t;
            let result = unsafe {
                libc::getsockopt(
                    listener,
                    libc::SOL_SOCKET,
                    libc::SO_ACCEPTCONN,
                    (&mut listening as *mut libc::c_int).cast(),
                    &mut size,
                )
            };
            let flags = unsafe { libc::fcntl(listener, libc::F_GETFL) };
            result == 0 && listening == 1 && flags >= 0 && flags & libc::O_NONBLOCK != 0
        })
    }
}

pub struct ReadinessManager {
    lifecycle: Arc<LifecycleState>,
    checks: Vec<Arc<dyn ReadinessCheck>>,
}

impl ReadinessManager {
    pub fn new(lifecycle: Arc<LifecycleState>, checks: Vec<Arc<dyn ReadinessCheck>>) -> Self {
        Self { lifecycle, checks }
    }

    pub async fn is_ready(&self) -> bool {
        if !self.lifecycle.is_ready() {
            return false;
        }
        for check in &self.checks {
            if !check.self_check().await {
                return false;
            }
        }
        self.lifecycle.is_ready()
    }
}

#[derive(Debug, Clone)]
pub struct ServerConfig {
    pub port: u16,
    pub is_not_fc: bool,
}

pub async fn run_server(config: ServerConfig) -> Result<(), ServerError> {
    run_server_with_runtime(config, ServerRuntime::default()).await
}

const FAILURE_REJECTION_PERIOD: Duration = Duration::from_millis(50);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(1);

fn transition_or_failure(
    lifecycle: &LifecycleState,
    phase: ServerPhase,
) -> Result<(), EnvdFailure> {
    if lifecycle.try_transition(phase) {
        return Ok(());
    }

    let failure = lifecycle
        .failure()
        .unwrap_or_else(|| EnvdFailure::invariant("lifecycle transition"));
    lifecycle.fail_with(failure);
    Err(lifecycle.failure().expect("failure just recorded"))
}

fn log_failure(failure: &EnvdFailure) {
    error!(
        task = failure.task(),
        kind = ?failure.kind(),
        "critical task failed; shutting down"
    );
}

pub async fn run_server_with_runtime(
    config: ServerConfig,
    mut runtime: ServerRuntime,
) -> Result<(), ServerError> {
    let lifecycle = runtime.lifecycle;
    let listener_check = Arc::new(ListenerCheck::new());
    runtime.readiness_checks.push(listener_check.clone());
    let app_state = transport::new_server_state(
        lifecycle.clone(),
        runtime.readiness_checks,
        !config.is_not_fc,
    );
    let addr = SocketAddr::from(([0, 0, 0, 0], config.port));
    let listener = tokio::net::TcpListener::bind(addr).await?;
    listener_check.attach(listener.as_raw_fd());
    let addr = listener.local_addr()?;

    let router: Router = transport::build_router(app_state.clone());
    let listener = crate::transport::idle::IdleListener::new(
        listener,
        crate::transport::idle::HTTP_IDLE_TIMEOUT,
    );

    transition_or_failure(&lifecycle, ServerPhase::Listening).map_err(ServerError::EnvdFailure)?;
    let (shutdown, mut shutdown_requested) = watch::channel(false);
    let mut supervisor = Supervisor::new(lifecycle.clone());
    supervisor.spawn("transport", async move {
        axum::serve(listener, crate::transport::idle::IdleRouter::new(router))
            .with_graceful_shutdown(async move {
                while !*shutdown_requested.borrow_and_update() {
                    if shutdown_requested.changed().await.is_err() {
                        break;
                    }
                }
            })
            .await
    });
    for task in runtime.critical_tasks {
        supervisor.register(task);
    }

    let selected_failure = match transition_or_failure(&lifecycle, ServerPhase::Ready) {
        Ok(()) => {
            info!(%addr, is_not_fc = config.is_not_fc, "envd listening");
            tokio::select! {
                failure = supervisor.next_failure() => Some(failure),
                failure = lifecycle.wait_for_failure() => Some(failure),
                _ = shutdown_signal() => None,
            }
        }
        Err(failure) => Some(failure),
    };

    if let Some(failure) = selected_failure {
        lifecycle.fail_with(failure);
    }
    let failure_before_shutdown = match lifecycle.failure() {
        Some(failure) => Some(failure),
        None => transition_or_failure(&lifecycle, ServerPhase::Draining).err(),
    };
    if let Some(failure) = failure_before_shutdown.as_ref() {
        log_failure(failure);
        tokio::time::sleep(FAILURE_REJECTION_PERIOD).await;
    }

    supervisor.begin_shutdown();
    let _ = shutdown.send(true);
    supervisor
        .shutdown(SHUTDOWN_TIMEOUT, || app_state.force_request_shutdown())
        .await;

    match lifecycle.failure() {
        Some(failure) => {
            if failure_before_shutdown.is_none() {
                log_failure(&failure);
            }
            Err(ServerError::EnvdFailure(failure))
        }
        None => transition_or_failure(&lifecycle, ServerPhase::Stopped)
            .map_err(ServerError::EnvdFailure),
    }
}

async fn shutdown_signal() {
    match signal::unix::signal(signal::unix::SignalKind::terminate()) {
        Ok(mut terminate) => {
            tokio::select! {
                _ = signal::ctrl_c() => {}
                _ = terminate.recv() => {}
            }
        }
        Err(_) => {
            let _ = signal::ctrl_c().await;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lifecycle_transitions() {
        let lc = LifecycleState::new();
        assert_eq!(lc.phase(), ServerPhase::Booting);
        assert!(lc.try_transition(ServerPhase::Listening));
        assert!(lc.try_transition(ServerPhase::Ready));
        assert!(lc.is_ready());
        assert!(lc.try_transition(ServerPhase::Draining));
        assert!(!lc.is_ready());
    }
}
