// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::convert::Infallible;
use std::future::{ready, Future, Ready};
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::Duration;

use axum::body::Body;
use axum::extract::Request;
use axum::response::Response;
use axum::serve::{IncomingStream, Listener};
use bytes::Bytes;
use futures::future::BoxFuture;
use http_body::{Frame, SizeHint};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::net::{TcpListener, TcpStream};
use tokio::time::{Instant, Sleep};
use tower::Service;

pub(crate) const HTTP_IDLE_TIMEOUT: Duration = Duration::from_secs(640);

pub(crate) struct IdleListener {
    inner: TcpListener,
    timeout: Duration,
}

impl IdleListener {
    pub(crate) fn new(inner: TcpListener, timeout: Duration) -> Self {
        Self { inner, timeout }
    }
}

impl Listener for IdleListener {
    type Io = IdleIo<TcpStream>;
    type Addr = std::net::SocketAddr;

    async fn accept(&mut self) -> (Self::Io, Self::Addr) {
        let (stream, address) = Listener::accept(&mut self.inner).await;
        (IdleIo::new(stream, self.timeout), address)
    }

    fn local_addr(&self) -> std::io::Result<Self::Addr> {
        self.inner.local_addr()
    }
}

#[derive(Clone)]
struct IdleState {
    inner: Arc<IdleStateInner>,
}

struct IdleStateInner {
    status: Mutex<IdleStatus>,
    timeout: Duration,
}

struct IdleStatus {
    active: usize,
    deadline: Option<Instant>,
}

impl IdleState {
    fn new(timeout: Duration) -> Self {
        Self {
            inner: Arc::new(IdleStateInner {
                status: Mutex::new(IdleStatus {
                    active: 0,
                    deadline: None,
                }),
                timeout,
            }),
        }
    }

    fn begin_request(&self) -> ActiveRequest {
        let mut status = self.inner.status.lock().expect("idle status");
        status.active += 1;
        status.deadline = None;
        ActiveRequest(Some(self.clone()))
    }

    fn finish_request(&self) {
        let mut status = self.inner.status.lock().expect("idle status");
        debug_assert!(status.active > 0);
        if status.active == 0 {
            return;
        }
        status.active -= 1;
        if status.active == 0 {
            status.deadline = Some(Instant::now() + self.inner.timeout);
        }
    }

    fn io_progress(&self) {
        let mut status = self.inner.status.lock().expect("idle status");
        if status.active == 0 && status.deadline.is_some() {
            status.deadline = Some(Instant::now() + self.inner.timeout);
        }
    }

    fn deadline(&self) -> Option<Instant> {
        self.inner.status.lock().expect("idle status").deadline
    }
}

struct ActiveRequest(Option<IdleState>);

impl ActiveRequest {
    fn track(mut self, body: Body) -> TrackedBody {
        TrackedBody {
            inner: body,
            state: self.0.take(),
        }
    }
}

impl Drop for ActiveRequest {
    fn drop(&mut self) {
        if let Some(state) = self.0.take() {
            state.finish_request();
        }
    }
}

struct TrackedBody {
    inner: Body,
    state: Option<IdleState>,
}

impl TrackedBody {
    fn finish(&mut self) {
        if let Some(state) = self.state.take() {
            state.finish_request();
        }
    }
}

impl Drop for TrackedBody {
    fn drop(&mut self) {
        self.finish();
    }
}

impl http_body::Body for TrackedBody {
    type Data = Bytes;
    type Error = axum::Error;

    fn poll_frame(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Self::Data>, Self::Error>>> {
        let result = Pin::new(&mut self.inner).poll_frame(cx);
        if matches!(result, Poll::Ready(None) | Poll::Ready(Some(Err(_)))) {
            self.finish();
        }
        result
    }

    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }

    fn size_hint(&self) -> SizeHint {
        self.inner.size_hint()
    }
}

pub(crate) struct IdleRouter {
    router: axum::Router,
}

impl IdleRouter {
    pub(crate) fn new(router: axum::Router) -> Self {
        Self { router }
    }
}

impl<'a> Service<IncomingStream<'a, IdleListener>> for IdleRouter {
    type Response = IdleService;
    type Error = Infallible;
    type Future = Ready<Result<Self::Response, Self::Error>>;

    fn poll_ready(&mut self, _cx: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, stream: IncomingStream<'a, IdleListener>) -> Self::Future {
        ready(Ok(IdleService {
            router: self.router.clone(),
            state: stream.io().state.clone(),
        }))
    }
}

#[derive(Clone)]
pub(crate) struct IdleService {
    router: axum::Router,
    state: IdleState,
}

impl Service<Request> for IdleService {
    type Response = Response;
    type Error = Infallible;
    type Future = BoxFuture<'static, Result<Self::Response, Self::Error>>;

    fn poll_ready(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        <axum::Router as Service<Request>>::poll_ready(&mut self.router, cx)
    }

    fn call(&mut self, request: Request) -> Self::Future {
        let active = self.state.begin_request();
        let future = self.router.call(request);
        Box::pin(async move {
            let response = future.await?;
            Ok(response.map(|body| Body::new(active.track(body))))
        })
    }
}

pub(crate) struct IdleIo<T> {
    inner: T,
    state: IdleState,
    sleep: Pin<Box<Sleep>>,
    armed: Option<Instant>,
}

impl<T> IdleIo<T> {
    fn new(inner: T, timeout: Duration) -> Self {
        Self {
            inner,
            state: IdleState::new(timeout),
            sleep: Box::pin(tokio::time::sleep(Duration::MAX)),
            armed: None,
        }
    }

    fn poll_timeout(&mut self, cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        let deadline = self.state.deadline();
        if deadline != self.armed {
            if let Some(deadline) = deadline {
                self.sleep.as_mut().reset(deadline);
            }
            self.armed = deadline;
        }
        match deadline {
            Some(_) if self.sleep.as_mut().poll(cx).is_ready() => Poll::Ready(Err(
                std::io::Error::new(std::io::ErrorKind::TimedOut, "HTTP connection idle timeout"),
            )),
            _ => Poll::Pending,
        }
    }

    fn progressed(&mut self) {
        self.state.io_progress();
        self.armed = None;
    }
}

impl<T: AsyncRead + Unpin> AsyncRead for IdleIo<T> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        if let Poll::Ready(result) = self.poll_timeout(cx) {
            return Poll::Ready(result);
        }
        let before = buffer.filled().len();
        let result = Pin::new(&mut self.inner).poll_read(cx, buffer);
        if matches!(result, Poll::Ready(Ok(()))) && buffer.filled().len() > before {
            self.progressed();
        }
        result
    }
}

impl<T: AsyncWrite + Unpin> AsyncWrite for IdleIo<T> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &[u8],
    ) -> Poll<Result<usize, std::io::Error>> {
        if let Poll::Ready(result) = self.poll_timeout(cx) {
            return Poll::Ready(result.map(|()| 0));
        }
        let result = Pin::new(&mut self.inner).poll_write(cx, buffer);
        if matches!(result, Poll::Ready(Ok(size)) if size > 0) {
            self.progressed();
        }
        result
    }

    fn poll_flush(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Result<(), std::io::Error>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Result<(), std::io::Error>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }

    fn is_write_vectored(&self) -> bool {
        self.inner.is_write_vectored()
    }

    fn poll_write_vectored(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffers: &[std::io::IoSlice<'_>],
    ) -> Poll<Result<usize, std::io::Error>> {
        if let Poll::Ready(result) = self.poll_timeout(cx) {
            return Poll::Ready(result.map(|()| 0));
        }
        let result = Pin::new(&mut self.inner).poll_write_vectored(cx, buffers);
        if matches!(result, Poll::Ready(Ok(size)) if size > 0) {
            self.progressed();
        }
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    #[tokio::test(start_paused = true)]
    async fn timeout_begins_after_a_request_and_ignores_active_requests() {
        let (client, mut server) = tokio::io::duplex(16);
        let mut io = IdleIo::new(client, Duration::from_secs(10));

        tokio::time::advance(Duration::from_secs(20)).await;
        server.write_all(b"a").await.unwrap();
        let mut byte = [0];
        io.read_exact(&mut byte).await.unwrap();

        let request = io.state.begin_request();
        tokio::time::advance(Duration::from_secs(20)).await;
        server.write_all(b"b").await.unwrap();
        io.read_exact(&mut byte).await.unwrap();

        drop(request);
        tokio::time::advance(Duration::from_secs(11)).await;
        let error = io.read_exact(&mut byte).await.unwrap_err();
        assert_eq!(error.kind(), std::io::ErrorKind::TimedOut);
    }

    #[tokio::test(start_paused = true)]
    async fn overlapping_requests_arm_only_after_the_last_response() {
        let state = IdleState::new(Duration::from_secs(10));
        let first = state.begin_request();
        let second = state.begin_request();

        drop(first);
        assert_eq!(state.deadline(), None);
        drop(second);
        assert_eq!(
            state.deadline(),
            Some(Instant::now() + Duration::from_secs(10))
        );
    }
}
