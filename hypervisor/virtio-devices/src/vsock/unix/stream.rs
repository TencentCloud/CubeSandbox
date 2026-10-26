// SPDX-License-Identifier: Apache-2.0
//
// Stream backends for vsock host-side connections.
//
// `VsockBackendStream` unifies two concrete stream types behind a single
// `Read + Write + AsRawFd` enum so that `VsockConnection<VsockBackendStream>`
// can handle both transparently:
//
// - `Unix(UnixStream)`: the classic path: each vsock connection maps to a
//   host-side Unix domain socket.
// - `PassFd(PassFdStream)`: the high-performance path: the Shim passes
//   container stdio pipe FDs to the VMM via `SCM_RIGHTS`, and data flows
//   directly through those pipes instead of traversing a Unix socket.
//

use std::fs::File;
use std::io::{Read, Write};
use std::os::unix::io::{AsRawFd, RawFd};
use std::os::unix::net::UnixStream;

pub enum VsockBackendStream {
    Unix(UnixStream),
    PassFd(PassFdStream),
}

impl Read for VsockBackendStream {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        match self {
            Self::Unix(s) => s.read(buf),
            Self::PassFd(s) => s.read(buf),
        }
    }
}

impl Write for VsockBackendStream {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        match self {
            Self::Unix(s) => s.write(buf),
            Self::PassFd(s) => s.write(buf),
        }
    }

    fn flush(&mut self) -> std::io::Result<()> {
        match self {
            Self::Unix(s) => s.flush(),
            Self::PassFd(s) => s.flush(),
        }
    }
}

impl AsRawFd for VsockBackendStream {
    fn as_raw_fd(&self) -> RawFd {
        match self {
            Self::Unix(s) => s.as_raw_fd(),
            Self::PassFd(s) => s.as_raw_fd(),
        }
    }
}

impl VsockBackendStream {
    pub fn send_connect_ack(&mut self, local_port: u32) -> std::io::Result<()> {
        match self {
            Self::Unix(s) => s.write_all(format!("OK {}\n", local_port).as_bytes()),
            Self::PassFd(s) => s.write_ack(local_port),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PassFdLabel {
    Stdin,
    Stdout,
    Stderr,
    Stream,
    Other(String),
}

impl PassFdLabel {
    pub fn as_str(&self) -> &str {
        match self {
            Self::Stdin => "stdin",
            Self::Stdout => "stdout",
            Self::Stderr => "stderr",
            Self::Stream => "stream",
            Self::Other(s) => s.as_str(),
        }
    }
}

impl std::fmt::Display for PassFdLabel {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.as_str())
    }
}

impl From<&str> for PassFdLabel {
    fn from(s: &str) -> Self {
        match s {
            "stdin" => Self::Stdin,
            "stdout" => Self::Stdout,
            "stderr" => Self::Stderr,
            "stream" => Self::Stream,
            other => Self::Other(other.to_string()),
        }
    }
}

pub struct PassFdStream {
    pub file: File,
    pub control: UnixStream,
    pub label: PassFdLabel,
}

impl PassFdStream {
    pub fn new(file: File, control: UnixStream, label: PassFdLabel) -> Self {
        Self {
            file,
            control,
            label,
        }
    }

    fn write_ack(&mut self, local_port: u32) -> std::io::Result<()> {
        self.control
            .write_all(format!("OK {} {}\n", self.label, local_port).as_bytes())
    }
}

impl Read for PassFdStream {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        self.file.read(buf)
    }
}

impl Write for PassFdStream {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.file.write(buf)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.file.flush()
    }
}

impl AsRawFd for PassFdStream {
    fn as_raw_fd(&self) -> RawFd {
        self.file.as_raw_fd()
    }
}

#[cfg(test)]
mod tests {
    use std::fs::OpenOptions;
    use std::io::{BufRead, BufReader, Read, Seek, SeekFrom, Write};
    use std::os::unix::net::UnixStream;

    use super::{PassFdLabel, PassFdStream, VsockBackendStream};

    fn temp_file() -> std::fs::File {
        let mut path = std::env::temp_dir();
        path.push(format!(
            "passfd-stream-test-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));

        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(&path)
            .unwrap();
        std::fs::remove_file(path).unwrap();
        file
    }

    #[test]
    fn unix_stream_sends_plain_connect_ack() {
        let (stream, mut peer) = UnixStream::pair().unwrap();
        let mut backend = VsockBackendStream::Unix(stream);

        backend.send_connect_ack(1024).unwrap();

        let mut ack = String::new();
        let mut reader = BufReader::new(peer);
        reader.read_line(&mut ack).unwrap();
        assert_eq!(ack, "OK 1024\n");
    }

    #[test]
    fn passfd_stream_sends_labeled_connect_ack() {
        let (control, mut peer) = UnixStream::pair().unwrap();
        let file = temp_file();
        let mut backend =
            VsockBackendStream::PassFd(PassFdStream::new(file, control, PassFdLabel::Stdout));

        backend.send_connect_ack(1025).unwrap();

        let mut ack = String::new();
        let mut reader = BufReader::new(peer);
        reader.read_line(&mut ack).unwrap();
        assert_eq!(ack, "OK stdout 1025\n");
    }

    #[test]
    fn passfd_stream_proxies_file_io() {
        let (control, _peer) = UnixStream::pair().unwrap();
        let file = temp_file();
        let mut stream = PassFdStream::new(file, control, PassFdLabel::Stdin);

        stream.write_all(b"payload").unwrap();
        stream.flush().unwrap();
        stream.file.seek(SeekFrom::Start(0)).unwrap();

        let mut buf = [0; 7];
        stream.read_exact(&mut buf).unwrap();
        assert_eq!(&buf, b"payload");
    }
}
