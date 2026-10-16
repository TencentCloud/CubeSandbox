// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! The accept loop for the data plane.
//!
//! `axum::serve` owns its accept loop and offers no hook between accepting a
//! socket and serving it, so this walks the listener itself for one reason:
//! `TCP_NODELAY`. Nagle's algorithm delays a small write until the previous
//! segment is acknowledged, and every request here answers with framed or
//! chunked bytes that a client may not have anything to piggyback on — a
//! streaming consumer pays a stall per write, and a large download pays it per
//! chunk. The Go baseline turns Nagle off on every accepted connection (its
//! `net` package sets `TCP_NODELAY` for TCP by default); leaving it on is a
//! measurable throughput loss, so the socket is tuned here before it is served.
//!
//! Everything else matches `axum::serve`: one task per connection, HTTP/1.1
//! with upgrade support, and connection errors are per-connection, never fatal
//! to the loop.

use std::convert::Infallible;
use std::io;
use std::time::Duration;

use axum::body::Body;
use axum::Router;
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper::{Request, Response};
use hyper_util::rt::TokioIo;
use tokio::net::TcpListener;
use tower::Service;

/// Accept a failure this long to avoid spinning while descriptors are short
/// (`EMFILE`), the way the Go baseline backs off on a temporary accept error.
const ACCEPT_RETRY_DELAY: Duration = Duration::from_millis(10);

/// Serve `app` on `listener` until the process ends.
///
/// Returns only if the listener itself can no longer be polled; a single failed
/// accept is retried.
pub async fn serve(listener: TcpListener, app: Router) -> io::Result<()> {
    loop {
        let (stream, _remote) = match listener.accept().await {
            Ok(accepted) => accepted,
            Err(e) => {
                tracing::warn!("accept failed: {e}");
                tokio::time::sleep(ACCEPT_RETRY_DELAY).await;
                continue;
            }
        };

        // Failure here is not fatal to the connection: it only means this
        // socket keeps Nagle's algorithm.
        if let Err(e) = stream.set_nodelay(true) {
            tracing::debug!("could not disable Nagle: {e}");
        }

        let app = app.clone();
        tokio::spawn(async move {
            let service = service_fn(move |req: Request<Incoming>| {
                let mut app = app.clone();
                async move {
                    let (parts, body) = req.into_parts();
                    let req = Request::from_parts(parts, Body::new(body));
                    match Service::call(&mut app, req).await {
                        Ok(response) => Ok::<Response<Body>, Infallible>(response),
                        // `Router`'s error type is `Infallible`.
                        Err(never) => match never {},
                    }
                }
            });

            // Upgrades are part of the surface `axum::serve` provides, so they
            // stay available here even though no route uses them today.
            // `half_close(true)`: a Connect client may end its request stream
            // (FIN) before the response is written, and hyper's default is to
            // treat that as a disconnect -- the request then gets no answer at
            // all, where the Go daemon answers normally.
            if let Err(e) = hyper::server::conn::http1::Builder::new()
                .half_close(true)
                .serve_connection(TokioIo::new(stream), service)
                .with_upgrades()
                .await
            {
                tracing::debug!("connection ended: {e}");
            }
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    /// The accept loop replaces `axum::serve`, so a request has to come back
    /// through it: bind an ephemeral port, send a request by hand, and read the
    /// response off the same socket.
    #[tokio::test]
    async fn a_request_is_served_over_the_accept_loop() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let app = Router::new().route("/", axum::routing::get(|| async { "pong" }));
        let server = tokio::spawn(serve(listener, app));

        let mut client = tokio::net::TcpStream::connect(addr).await.unwrap();
        client
            .write_all(b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
            .await
            .unwrap();
        let mut response = Vec::new();
        client.read_to_end(&mut response).await.unwrap();
        server.abort();

        let response = String::from_utf8(response).unwrap();
        assert!(response.starts_with("HTTP/1.1 200 OK"), "{response}");
        assert!(response.ends_with("pong"), "{response}");
    }

    /// A client may end its request stream before the response is written --
    /// Connect clients are allowed to half-close, and some do. hyper's default
    /// treats that FIN as a disconnect, which would leave the request
    /// unanswered; `half_close(true)` is what keeps the two equivalent.
    #[tokio::test]
    async fn a_client_that_half_closes_its_request_still_gets_a_response() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        // Respond only after the client has closed its write side, so the FIN
        // is guaranteed to arrive before the response is written.
        let app = Router::new().route(
            "/",
            axum::routing::get(|| async {
                tokio::time::sleep(Duration::from_millis(150)).await;
                "pong"
            }),
        );
        let server = tokio::spawn(serve(listener, app));

        let mut client = tokio::net::TcpStream::connect(addr).await.unwrap();
        client
            .write_all(b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
            .await
            .unwrap();
        client.shutdown().await.unwrap();

        let mut response = Vec::new();
        tokio::time::timeout(Duration::from_secs(5), client.read_to_end(&mut response))
            .await
            .expect("the server must answer a half-closed request")
            .unwrap();
        server.abort();

        let response = String::from_utf8(response).unwrap();
        assert!(response.starts_with("HTTP/1.1 200 OK"), "{response}");
        assert!(response.ends_with("pong"), "{response}");
    }
}
