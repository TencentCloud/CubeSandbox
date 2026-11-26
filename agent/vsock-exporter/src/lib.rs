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

#[async_trait]
impl SpanExporter for Exporter {
    async fn export(&mut self, batch: Vec<SpanData>) -> ExportResult {
        if self.conn.is_none() {
            let conn = connect_vsock(self.cid, self.port).await.map(|e| {
                error!(self.logger, "failed to obtain connection"; "error" => format!("{:?}", e));
                e
            })?;

            self.conn = Some(Arc::new(Mutex::new(conn)));
        }

        handle_batch(self.conn.as_ref().unwrap().clone(), batch)
            .await
            .map_err(|e| {
                error!(self.logger, "handle_batch error: {:?}", e);
                if e.kind() == ErrorKind::NotConnected {
                    info!(self.logger, "drop connection");
                    self.conn.take();
                }

                Error::IOError(e)
            })?;

        Ok(())
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
