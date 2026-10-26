// Copyright (c) 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Result};
use futures::StreamExt;
use std::io;
use std::io::ErrorKind;
use std::os::unix::io::{FromRawFd, RawFd};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::watch::Receiver;
use tokio_vsock::{Incoming, VsockListener, VsockStream};
use tracing::instrument;

// Size of I/O read buffer
const BUF_SIZE: usize = 8192;

// Interruptable I/O copy using readers and writers
// (an interruptable version of "io::copy()").
pub async fn interruptable_io_copier<R: Sized, W: Sized>(
    mut reader: R,
    mut writer: W,
    mut shutdown: Receiver<bool>,
) -> io::Result<u64>
where
    R: tokio::io::AsyncRead + Unpin,
    W: tokio::io::AsyncWrite + Unpin,
{
    let mut total_bytes: u64 = 0;

    let mut buf: [u8; BUF_SIZE] = [0; BUF_SIZE];

    loop {
        tokio::select! {
            _ = shutdown.changed() => {
                eprintln!("INFO: interruptable_io_copier: got shutdown request");
                break;
            },

            result = reader.read(&mut buf) => {
                let bytes = match result {
                    Ok(0) => return Ok(total_bytes),
                    Ok(len) => len,
                    Err(ref e) if e.kind() == ErrorKind::Interrupted => continue,
                    Err(e) => return Err(e),
                };

                total_bytes += bytes as u64;

                // Actually copy the data ;)
                writer.write_all(&buf[..bytes]).await?;
            },
        };
    }

    Ok(total_bytes)
}

#[instrument]
pub fn get_vsock_incoming(fd: RawFd) -> Incoming {
    unsafe { VsockListener::from_raw_fd(fd).incoming() }
}

#[instrument]
pub async fn get_vsock_stream(fd: RawFd) -> Result<VsockStream> {
    let stream = get_vsock_incoming(fd)
        .next()
        .await
        .ok_or_else(|| anyhow!("cannot handle incoming vsock connection"))?;

    Ok(stream?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io;
    use std::io::Cursor;
    use std::io::Write;
    use std::pin::Pin;
    use std::sync::{Arc, Mutex};
    use std::task::{Context, Poll, Poll::Ready};
    use tokio::pin;
    use tokio::select;
    use tokio::sync::watch::channel;
    use tokio::task::JoinError;
    use tokio::time::Duration;

    #[derive(Debug, Default, Clone)]
    struct BufWriter {
        data: Arc<Mutex<Vec<u8>>>,
        write_delay: Duration,
    }

    impl BufWriter {
        fn new() -> Self {
            BufWriter {
                data: Arc::new(Mutex::new(Vec::<u8>::new())),
                write_delay: Duration::new(0, 0),
            }
        }

        fn write_vec(&mut self, buf: &[u8]) -> io::Result<usize> {
            let vec_ref = self.data.clone();

            let mut vec_locked = vec_ref.lock();

            let mut v = vec_locked.as_deref_mut().unwrap();

            if self.write_delay.as_nanos() > 0 {
                std::thread::sleep(self.write_delay);
            }

            std::io::Write::write(&mut v, buf)
        }
    }

    impl Write for BufWriter {
        fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
            self.write_vec(buf)
        }

        fn flush(&mut self) -> io::Result<()> {
            let vec_ref = self.data.clone();

            let mut vec_locked = vec_ref.lock();

            let v = vec_locked
                .as_deref_mut()
                .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e.to_string()))?;

            std::io::Write::flush(v)
        }
    }

    impl tokio::io::AsyncWrite for BufWriter {
        fn poll_write(
            mut self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
            buf: &[u8],
        ) -> Poll<Result<usize, io::Error>> {
            let result = self.write_vec(buf);

            Ready(result)
        }

        fn poll_flush(
            self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Result<(), std::io::Error>> {
            // NOP
            Ready(Ok(()))
        }

        fn poll_shutdown(
            self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Result<(), std::io::Error>> {
            // NOP
            Ready(Ok(()))
        }
    }

    impl ToString for BufWriter {
        fn to_string(&self) -> String {
            let data_ref = self.data.clone();
            let output = data_ref.lock().unwrap();
            let s = (*output).clone();

            String::from_utf8(s).unwrap()
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn test_interruptable_io_copier_reader() {
        #[derive(Debug)]
        struct TestData {
            reader_value: String,
        }

        let tests = &[
            TestData {
                reader_value: "".into(),
            },
            TestData {
                reader_value: "a".into(),
            },
            TestData {
                reader_value: "foo".into(),
            },
            TestData {
                reader_value: "b".repeat(BUF_SIZE - 1),
            },
            TestData {
                reader_value: "c".repeat(BUF_SIZE),
            },
            TestData {
                reader_value: "d".repeat(BUF_SIZE + 1),
            },
            TestData {
                reader_value: "e".repeat((2 * BUF_SIZE) - 1),
            },
            TestData {
                reader_value: "f".repeat(2 * BUF_SIZE),
            },
            TestData {
                reader_value: "g".repeat((2 * BUF_SIZE) + 1),
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            // Create a string containing details of the test
            let msg = format!("test[{}]: {:?}", i, d);

            let (tx, rx) = channel(true);
            let reader = Cursor::new(d.reader_value.clone());
            let writer = BufWriter::new();

            // XXX: Pass a copy of the writer to the copier to allow the
            // result of the write operation to be checked below.
            let handle = tokio::spawn(interruptable_io_copier(reader, writer.clone(), rx));

            // Allow time for the thread to be spawned.
            tokio::time::sleep(Duration::from_secs(1)).await;

            let timeout = tokio::time::sleep(Duration::from_secs(1));
            pin!(timeout);

            // Since the readers only specify a small number of bytes, the
            // copier will quickly read zero and kill the task, closing the
            // Receiver.
            assert!(tx.is_closed(), "{}", msg);

            let spawn_result: std::result::Result<
                std::result::Result<u64, std::io::Error>,
                JoinError,
            >;

            select! {
                res = handle => spawn_result = res,
                _ = &mut timeout => panic!("timed out"),
            }

            assert!(spawn_result.is_ok());

            let result: std::result::Result<u64, std::io::Error> = spawn_result.unwrap();

            assert!(result.is_ok());

            let byte_count = result.unwrap() as usize;
            assert_eq!(byte_count, d.reader_value.len(), "{}", msg);

            let value = writer.to_string();
            assert_eq!(value, d.reader_value, "{}", msg);
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn test_interruptable_io_copier_eof() {
        // Create an async reader that always returns EOF
        let reader = tokio::io::empty();

        let (tx, rx) = channel(true);
        let writer = BufWriter::new();

        let handle = tokio::spawn(interruptable_io_copier(reader, writer.clone(), rx));

        // Allow time for the thread to be spawned.
        tokio::time::sleep(Duration::from_secs(1)).await;

        let timeout = tokio::time::sleep(Duration::from_secs(1));
        pin!(timeout);

        assert!(tx.is_closed());

        let spawn_result: std::result::Result<std::result::Result<u64, std::io::Error>, JoinError>;

        select! {
            res = handle => spawn_result = res,
            _ = &mut timeout => panic!("timed out"),
        }

        assert!(spawn_result.is_ok());

        let result: std::result::Result<u64, std::io::Error> = spawn_result.unwrap();

        assert!(result.is_ok());

        let byte_count = result.unwrap();
        assert_eq!(byte_count, 0);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn test_interruptable_io_copier_shutdown() {
        // Create an async reader that creates an infinite stream of bytes
        // (which allows us to interrupt it, since we know it is always busy ;)
        const REPEAT_CHAR: u8 = b'r';

        let reader = tokio::io::repeat(REPEAT_CHAR);

        let (tx, rx) = channel(true);
        let writer = BufWriter::new();

        let handle = tokio::spawn(interruptable_io_copier(reader, writer.clone(), rx));

        // Allow time for the thread to be spawned.
        tokio::time::sleep(Duration::from_secs(1)).await;

        let timeout = tokio::time::sleep(Duration::from_secs(1));
        pin!(timeout);

        assert!(!tx.is_closed());

        tx.send(true).expect("failed to request shutdown");

        let spawn_result: std::result::Result<std::result::Result<u64, std::io::Error>, JoinError>;

        select! {
            res = handle => spawn_result = res,
            _ = &mut timeout => panic!("timed out"),
        }

        assert!(spawn_result.is_ok());

        let result: std::result::Result<u64, std::io::Error> = spawn_result.unwrap();

        assert!(result.is_ok());

        let byte_count = result.unwrap();

        let value = writer.to_string();

        let writer_byte_count = value.len() as u64;

        assert_eq!(byte_count, writer_byte_count);

        // Remove the char used as a payload. If anything else remins,
        // something went wrong.
        let mut remainder = value;

        remainder.retain(|c| c != REPEAT_CHAR as char);

        assert_eq!(remainder.len(), 0);
    }
}
