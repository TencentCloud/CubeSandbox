// Copyright (c) 2020-2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

// The VSOCK Exporter sends trace spans "out" to the forwarder running on the
// host (which then forwards them on to a trace collector). The data is sent
// via a VSOCK socket that the forwarder process is listening on. To allow the
// forwarder to know how much data to each for each trace span the simplest
// protocol is employed which uses a header packet and the payload (trace
// span) data. The header packet is a simple count of the number of bytes in the
// payload, which allows the forwarder to know how many bytes it must read to
// consume the trace span. The payload is a serialised version of the trace span.

#![allow(unknown_lints)]

use std::io::ErrorKind;
use std::sync::Arc;

use async_trait::async_trait;
use byteorder::{ByteOrder, NetworkEndian};
use opentelemetry::sdk::export::trace::{ExportResult, SpanData, SpanExporter};
use opentelemetry::sdk::export::ExportError;
use slog::{error, info, o, Logger};
use thiserror::Error;
use tokio::io::AsyncWriteExt;
use tokio::sync::Mutex;
use tokio_vsock::VsockAddr;
use tokio_vsock::VsockStream;

const ANY_CID: &str = "any";

// Must match the value of the variable of the same name in the trace forwarder.
const HEADER_SIZE_BYTES: u64 = std::mem::size_of::<u64>() as u64;

// By default, the VSOCK exporter should talk "out" to the host where the
// forwarder is running.
const DEFAULT_CID: u32 = libc::VMADDR_CID_HOST;

// The VSOCK port the forwarders listens on by default
const DEFAULT_PORT: u32 = 10240;

#[derive(Debug)]
pub struct Exporter {
    port: u32,
    cid: u32,
    conn: Option<Arc<Mutex<VsockStream>>>,
    logger: Logger,
}

impl Exporter {
    /// Create a new exporter builder.
    pub fn builder() -> Builder {
        Builder::default()
    }
}

#[derive(Error, Debug)]
pub enum Error {
    #[error("connection error: {0}")]
    ConnectionError(String),
    #[error("serialisation error: {0}")]
    SerialisationError(#[from] bincode::Error),
    #[error("I/O error: {0}")]
    IOError(#[from] std::io::Error),
}

impl ExportError for Error {
    fn exporter_name(&self) -> &'static str {
        "vsock-exporter"
    }
}

fn make_io_error(desc: String) -> std::io::Error {
    std::io::Error::new(ErrorKind::Other, desc)
}

// Send a trace span to the forwarder running on the host.
async fn write_span(
    writer: Arc<Mutex<VsockStream>>,
    span: &SpanData,
) -> Result<(), std::io::Error> {
    let mut writer = writer.lock().await;

    let encoded_payload: Vec<u8> =
        bincode::serialize(&span).map_err(|e| make_io_error(e.to_string()))?;

    let payload_len: u64 = encoded_payload.len() as u64;

    let mut payload_len_as_bytes: [u8; HEADER_SIZE_BYTES as usize] =
        [0; HEADER_SIZE_BYTES as usize];

    // Encode the header
    NetworkEndian::write_u64(&mut payload_len_as_bytes, payload_len);

    // Send the header
    writer.write_all(&payload_len_as_bytes).await?;

    writer.write_all(&encoded_payload).await
}

async fn handle_batch(
    writer: Arc<Mutex<VsockStream>>,
    batch: Vec<SpanData>,
) -> Result<(), std::io::Error> {
    for span_data in batch {
        write_span(writer.clone(), &span_data).await?;
    }

    Ok(())
}

/// Destination for exported spans. Production writes over vsock; tests
/// substitute an in-memory fake to drive reconnect behaviour.
#[async_trait]
pub(crate) trait SpanSink: Send {
    async fn send(&mut self, batch: Vec<SpanData>) -> Result<(), std::io::Error>;
}

#[async_trait]
impl SpanSink for Arc<Mutex<VsockStream>> {
    async fn send(&mut self, batch: Vec<SpanData>) -> Result<(), std::io::Error> {
        handle_batch(self.clone(), batch).await
    }
}

/// Lazy-connect + batch export state machine shared by the production
/// exporter and its regression tests.
async fn export_with_sink<S, F, Fut>(
    conn: &mut Option<S>,
    connect: F,
    batch: Vec<SpanData>,
    logger: &Logger,
) -> ExportResult
where
    S: SpanSink,
    F: FnOnce() -> Fut,
    Fut: std::future::Future<Output = Result<S, Error>>,
{
    if conn.is_none() {
        let sink = connect().await.map_err(|e| {
            error!(logger, "failed to obtain connection"; "error" => format!("{:?}", e));
            e
        })?;

        *conn = Some(sink);
    }

    let mut sink = conn.take().unwrap();
    match sink.send(batch).await {
        Ok(()) => {
            *conn = Some(sink);
            Ok(())
        }
        Err(e) => {
            error!(logger, "handle_batch error: {:?}", e);
            // Drop the cached connection on any write failure: after a VM
            // snapshot/restore it is dead but reports BrokenPipe or
            // ConnectionReset, not NotConnected, and the next export must
            // reconnect to the new sandbox's host forwarder.
            info!(logger, "drop connection");

            Err(Error::IOError(e).into())
        }
    }
}

#[async_trait]
impl SpanExporter for Exporter {
    async fn export(&mut self, batch: Vec<SpanData>) -> ExportResult {
        let cid = self.cid;
        let port = self.port;

        export_with_sink(
            &mut self.conn,
            move || async move {
                let stream = connect_vsock(cid, port).await?;

                Ok(Arc::new(Mutex::new(stream)))
            },
            batch,
            &self.logger,
        )
        .await
    }

    fn shutdown(&mut self) {
        self.conn.take();
    }
}

#[derive(Debug)]
pub struct Builder {
    port: u32,
    cid: u32,
    logger: Logger,
}

impl Default for Builder {
    fn default() -> Self {
        let logger = Logger::root(slog::Discard, o!());

        Builder {
            cid: DEFAULT_CID,
            port: DEFAULT_PORT,
            logger,
        }
    }
}

impl Builder {
    pub fn with_cid(self, cid: u32) -> Self {
        Builder { cid, ..self }
    }

    pub fn with_port(self, port: u32) -> Self {
        Builder { port, ..self }
    }

    pub fn with_logger(self, logger: &Logger) -> Self {
        Builder {
            logger: logger.new(o!()),
            ..self
        }
    }

    pub fn init(self) -> Exporter {
        let Builder { port, cid, logger } = self;

        let cid_str: String = if self.cid == libc::VMADDR_CID_ANY {
            ANY_CID.to_string()
        } else {
            format!("{}", self.cid)
        };

        Exporter {
            port,
            cid,
            conn: None,
            logger: logger.new(o!("cid" => cid_str, "port" => port)),
        }
    }
}

async fn connect_vsock(cid: u32, port: u32) -> Result<VsockStream, Error> {
    match VsockStream::connect(VsockAddr::new(cid, port)).await {
        Ok(conn) => Ok(conn),
        Err(e) => Err(Error::ConnectionError(e.to_string())),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use opentelemetry::sdk::trace::{EvictedHashMap, EvictedQueue};
    use opentelemetry::sdk::InstrumentationLibrary;
    use opentelemetry::trace::{SpanContext, SpanId, SpanKind, StatusCode, TraceId, TraceState};
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::SystemTime;

    struct FakeSink {
        fail_with: Option<ErrorKind>,
        sent: Arc<AtomicUsize>,
    }

    #[async_trait]
    impl SpanSink for FakeSink {
        async fn send(&mut self, batch: Vec<SpanData>) -> Result<(), std::io::Error> {
            match self.fail_with {
                Some(kind) => Err(std::io::Error::new(kind, "stale vsock connection")),
                None => {
                    self.sent.fetch_add(batch.len(), Ordering::SeqCst);
                    Ok(())
                }
            }
        }
    }

    fn make_span() -> SpanData {
        SpanData {
            span_context: SpanContext::new(
                TraceId::from_u128(1),
                SpanId::from_u64(1),
                0,
                false,
                TraceState::default(),
            ),
            parent_span_id: SpanId::invalid(),
            span_kind: SpanKind::Client,
            name: "test-span".into(),
            start_time: SystemTime::UNIX_EPOCH,
            end_time: SystemTime::UNIX_EPOCH,
            attributes: EvictedHashMap::new(128, 0),
            events: EvictedQueue::new(128),
            links: EvictedQueue::new(128),
            status_code: StatusCode::Unset,
            status_message: "".into(),
            resource: None,
            instrumentation_lib: InstrumentationLibrary::default(),
        }
    }

    fn discard_logger() -> Logger {
        Logger::root(slog::Discard, o!())
    }

    /// Regression test: a snapshot/restored VM reports BrokenPipe or
    /// ConnectionReset on the stale connection (not NotConnected). The
    /// exporter must drop it so the next batch reconnects to the new
    /// sandbox's host forwarder.
    #[tokio::test]
    async fn drops_stale_connection_and_reconnects_on_next_export() {
        let logger = discard_logger();
        let connects = Arc::new(AtomicUsize::new(0));
        let sent = Arc::new(AtomicUsize::new(0));
        let mut conn: Option<FakeSink> = None;

        // Export #1: connect succeeds, write fails with BrokenPipe.
        let (c1, s1) = (connects.clone(), sent.clone());
        let r1 = export_with_sink(
            &mut conn,
            move || {
                c1.fetch_add(1, Ordering::SeqCst);
                async move {
                    Ok(FakeSink {
                        fail_with: Some(ErrorKind::BrokenPipe),
                        sent: s1,
                    })
                }
            },
            vec![make_span()],
            &logger,
        )
        .await;
        assert!(r1.is_err());
        assert!(
            conn.is_none(),
            "stale connection must be dropped after a write failure"
        );
        assert_eq!(connects.load(Ordering::SeqCst), 1);
        assert_eq!(sent.load(Ordering::SeqCst), 0);

        // Export #2: reconnects and succeeds.
        let (c2, s2) = (connects.clone(), sent.clone());
        let r2 = export_with_sink(
            &mut conn,
            move || {
                c2.fetch_add(1, Ordering::SeqCst);
                async move {
                    Ok(FakeSink {
                        fail_with: None,
                        sent: s2,
                    })
                }
            },
            vec![make_span()],
            &logger,
        )
        .await;
        assert!(r2.is_ok());
        assert_eq!(
            connects.load(Ordering::SeqCst),
            2,
            "second export must reconnect"
        );
        assert_eq!(sent.load(Ordering::SeqCst), 1);
        assert!(conn.is_some(), "healthy connection is cached for reuse");

        // Export #3: cached connection is reused, no extra connect.
        let (c3, s3) = (connects.clone(), sent.clone());
        let r3 = export_with_sink(
            &mut conn,
            move || {
                c3.fetch_add(1, Ordering::SeqCst);
                async move {
                    Ok(FakeSink {
                        fail_with: None,
                        sent: s3,
                    })
                }
            },
            vec![make_span()],
            &logger,
        )
        .await;
        assert!(r3.is_ok());
        assert_eq!(
            connects.load(Ordering::SeqCst),
            2,
            "cached connection must be reused without reconnecting"
        );
        assert_eq!(sent.load(Ordering::SeqCst), 2);
    }

    /// Guards against regressing to the old NotConnected-only drop: every
    /// write failure kind must invalidate the cached connection.
    #[tokio::test]
    async fn any_write_failure_kind_drops_the_connection() {
        let logger = discard_logger();

        for kind in [
            ErrorKind::NotConnected,
            ErrorKind::BrokenPipe,
            ErrorKind::ConnectionReset,
            ErrorKind::UnexpectedEof,
        ]
        .iter()
        {
            let mut conn = Some(FakeSink {
                fail_with: Some(*kind),
                sent: Arc::new(AtomicUsize::new(0)),
            });
            let r = export_with_sink(
                &mut conn,
                || async { unreachable!("connect must not be called") },
                vec![make_span()],
                &logger,
            )
            .await;
            assert!(r.is_err());
            assert!(conn.is_none(), "{:?} must drop the cached connection", kind);
        }
    }

    /// A failed lazy connect must propagate and leave no connection cached.
    #[tokio::test]
    async fn connect_failure_propagates_and_caches_nothing() {
        let logger = discard_logger();
        let mut conn: Option<FakeSink> = None;

        let r = export_with_sink(
            &mut conn,
            || async { Err(Error::ConnectionError("vsock down".into())) },
            vec![make_span()],
            &logger,
        )
        .await;

        assert!(r.is_err());
        assert!(conn.is_none());
    }
}
