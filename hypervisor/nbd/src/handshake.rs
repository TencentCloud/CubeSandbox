// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! NBD client handshake (Fixed Newstyle `NBD_OPT_GO` with `NBD_OPT_EXPORT_NAME` fallback).

use std::io::{self, Read, Write};
use std::result;

use log::debug;
use thiserror::Error;

use crate::proto::*;

#[derive(Error, Debug)]
pub enum NbdHandshakeError {
    #[error("I/O error during NBD handshake")]
    Io(#[source] io::Error),
    #[error("Bad NBD magic {0:#x}")]
    BadMagic(u64),
    #[error("Oldstyle NBD negotiation is not supported")]
    Oldstyle,
    #[error("NBD server requires TLS, which is not supported")]
    TlsRequired,
    #[error("NBD export \"{0}\" not found")]
    UnknownExport(String),
    #[error("NBD server rejected option {option} with error {error:#x}: {message}")]
    OptionRejected {
        option: u32,
        error: u32,
        message: String,
    },
    #[error("NBD protocol violation: {0}")]
    Protocol(String),
}

impl From<io::Error> for NbdHandshakeError {
    fn from(e: io::Error) -> Self {
        Self::Io(e)
    }
}

type Result<T> = result::Result<T, NbdHandshakeError>;

/// Export properties negotiated during the handshake.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NbdExportInfo {
    /// Export size in bytes.
    pub size: u64,
    /// Transmission flags (`NBD_FLAG_*`).
    pub flags: u16,
    /// Minimum block size; requests must be aligned to it.
    pub min_block: u32,
    /// Preferred block size.
    pub opt_block: u32,
    /// Maximum payload size for a single READ or WRITE.
    pub max_block: u32,
}

impl NbdExportInfo {
    pub fn has_flag(&self, flag: u16) -> bool {
        self.flags & flag != 0
    }

    /// Returns whether the export supports `connections` concurrent connections.
    pub fn allows_connections(&self, connections: usize) -> bool {
        connections <= 1 || self.has_flag(NBD_FLAG_CAN_MULTI_CONN)
    }
}

fn read_u16(s: &mut impl Read) -> io::Result<u16> {
    let mut b = [0u8; 2];
    s.read_exact(&mut b)?;
    Ok(u16::from_be_bytes(b))
}

fn read_u32(s: &mut impl Read) -> io::Result<u32> {
    let mut b = [0u8; 4];
    s.read_exact(&mut b)?;
    Ok(u32::from_be_bytes(b))
}

fn read_u64(s: &mut impl Read) -> io::Result<u64> {
    let mut b = [0u8; 8];
    s.read_exact(&mut b)?;
    Ok(u64::from_be_bytes(b))
}

fn drop_bytes(s: &mut impl Read, len: u32) -> io::Result<()> {
    io::copy(&mut s.take(len as u64), &mut io::sink()).and_then(|n| {
        if n == len as u64 {
            Ok(())
        } else {
            Err(io::ErrorKind::UnexpectedEof.into())
        }
    })
}

fn send_option<S: Write>(s: &mut S, option: u32, data: &[u8]) -> io::Result<()> {
    let mut buf = Vec::with_capacity(16 + data.len());
    buf.extend_from_slice(&NBD_OPTS_MAGIC.to_be_bytes());
    buf.extend_from_slice(&option.to_be_bytes());
    buf.extend_from_slice(&(data.len() as u32).to_be_bytes());
    buf.extend_from_slice(data);
    s.write_all(&buf)?;
    s.flush()
}

struct OptionReply {
    reply_type: u32,
    length: u32,
}

fn receive_option_reply<S: Read>(s: &mut S, option: u32) -> Result<OptionReply> {
    let magic = read_u64(s)?;
    if magic != NBD_REP_MAGIC {
        return Err(NbdHandshakeError::BadMagic(magic));
    }
    let reply_option = read_u32(s)?;
    if reply_option != option {
        return Err(NbdHandshakeError::Protocol(format!(
            "reply for option {reply_option}, expected {option}"
        )));
    }
    let reply_type = read_u32(s)?;
    let length = read_u32(s)?;
    Ok(OptionReply { reply_type, length })
}

fn handle_reply_error<S: Read>(
    s: &mut S,
    option: u32,
    reply: &OptionReply,
    export: &str,
) -> Result<NbdHandshakeError> {
    let mut message = String::new();
    if reply.length as usize <= NBD_MAX_STRING_SIZE {
        let mut buf = vec![0u8; reply.length as usize];
        s.read_exact(&mut buf)?;
        message = String::from_utf8_lossy(&buf).into_owned();
    } else {
        drop_bytes(s, reply.length)?;
    }
    Ok(match reply.reply_type {
        NBD_REP_ERR_TLS_REQD => NbdHandshakeError::TlsRequired,
        NBD_REP_ERR_UNKNOWN => NbdHandshakeError::UnknownExport(export.to_string()),
        error => NbdHandshakeError::OptionRejected {
            option,
            error,
            message,
        },
    })
}

fn opt_go<S: Read + Write>(s: &mut S, export: &str) -> Result<Option<NbdExportInfo>> {
    let name = export.as_bytes();
    let mut data = Vec::with_capacity(4 + name.len() + 4);
    data.extend_from_slice(&(name.len() as u32).to_be_bytes());
    data.extend_from_slice(name);
    data.extend_from_slice(&1u16.to_be_bytes());
    data.extend_from_slice(&NBD_INFO_BLOCK_SIZE.to_be_bytes());
    send_option(s, NBD_OPT_GO, &data)?;

    let mut size_flags = None;
    let mut block_sizes = None;
    loop {
        let reply = receive_option_reply(s, NBD_OPT_GO)?;
        if reply.reply_type & NBD_REP_FLAG_ERROR != 0 {
            if reply.reply_type == NBD_REP_ERR_UNSUP {
                drop_bytes(s, reply.length)?;
                return Ok(None);
            }
            return Err(handle_reply_error(s, NBD_OPT_GO, &reply, export)?);
        }

        match reply.reply_type {
            NBD_REP_ACK => {
                if reply.length != 0 {
                    return Err(NbdHandshakeError::Protocol(
                        "NBD_REP_ACK with payload".to_string(),
                    ));
                }
                let (size, flags) = size_flags.ok_or_else(|| {
                    NbdHandshakeError::Protocol("server omitted NBD_INFO_EXPORT".to_string())
                })?;
                let (min_block, opt_block, max_block) =
                    block_sizes.unwrap_or((1, 4096, NBD_DEFAULT_MAX_PAYLOAD));
                return Ok(Some(NbdExportInfo {
                    size,
                    flags,
                    min_block,
                    opt_block,
                    max_block,
                }));
            }
            NBD_REP_INFO => {
                if reply.length < 2 {
                    return Err(NbdHandshakeError::Protocol(
                        "NBD_REP_INFO too short".to_string(),
                    ));
                }
                let info_type = read_u16(s)?;
                let len = reply.length - 2;
                match info_type {
                    NBD_INFO_EXPORT if len == 10 => {
                        size_flags = Some((read_u64(s)?, read_u16(s)?));
                    }
                    NBD_INFO_BLOCK_SIZE if len == 12 => {
                        block_sizes = Some((read_u32(s)?, read_u32(s)?, read_u32(s)?));
                    }
                    NBD_INFO_EXPORT | NBD_INFO_BLOCK_SIZE => {
                        return Err(NbdHandshakeError::Protocol(format!(
                            "info {info_type} has unexpected length {len}"
                        )));
                    }
                    _ => drop_bytes(s, len)?,
                }
            }
            other => {
                return Err(NbdHandshakeError::Protocol(format!(
                    "unexpected reply type {other} to NBD_OPT_GO"
                )));
            }
        }
    }
}

fn opt_export_name<S: Read + Write>(
    s: &mut S,
    export: &str,
    no_zeroes: bool,
) -> Result<NbdExportInfo> {
    send_option(s, NBD_OPT_EXPORT_NAME, export.as_bytes())?;
    let size = read_u64(s).map_err(|e| {
        if e.kind() == io::ErrorKind::UnexpectedEof {
            NbdHandshakeError::UnknownExport(export.to_string())
        } else {
            NbdHandshakeError::Io(e)
        }
    })?;
    let flags = read_u16(s)?;
    if !no_zeroes {
        drop_bytes(s, 124)?;
    }
    Ok(NbdExportInfo {
        size,
        flags,
        min_block: 1,
        opt_block: 4096,
        max_block: NBD_DEFAULT_MAX_PAYLOAD,
    })
}

fn validate(info: NbdExportInfo) -> Result<NbdExportInfo> {
    let protocol = |m: String| Err(NbdHandshakeError::Protocol(m));
    if info.flags & NBD_FLAG_HAS_FLAGS == 0 {
        return protocol("server did not set NBD_FLAG_HAS_FLAGS".to_string());
    }
    if !info.min_block.is_power_of_two() || info.min_block > 65536 {
        return protocol(format!("invalid minimum block size {}", info.min_block));
    }
    if !info.opt_block.is_power_of_two() || info.opt_block < info.min_block {
        return protocol(format!("invalid preferred block size {}", info.opt_block));
    }
    if info.max_block < info.opt_block
        || (info.max_block != u32::MAX && info.max_block % info.min_block != 0)
    {
        return protocol(format!("invalid maximum block size {}", info.max_block));
    }
    if info.size % (info.min_block as u64) != 0 {
        return protocol(format!(
            "export size {} is not a multiple of minimum block size {}",
            info.size, info.min_block
        ));
    }
    Ok(info)
}

/// Runs the client side of the NBD handshake on a connected stream.
pub fn negotiate<S: Read + Write>(s: &mut S, export: &str) -> Result<NbdExportInfo> {
    let magic = read_u64(s)?;
    if magic != NBD_INIT_MAGIC {
        return Err(NbdHandshakeError::BadMagic(magic));
    }
    let magic = read_u64(s)?;
    match magic {
        NBD_OPTS_MAGIC => {}
        NBD_CLIENT_MAGIC => return Err(NbdHandshakeError::Oldstyle),
        m => return Err(NbdHandshakeError::BadMagic(m)),
    }

    let server_flags = read_u16(s)?;
    let fixed_newstyle = server_flags & NBD_FLAG_FIXED_NEWSTYLE != 0;
    let no_zeroes = server_flags & NBD_FLAG_NO_ZEROES != 0;
    let mut client_flags = 0u32;
    if fixed_newstyle {
        client_flags |= NBD_FLAG_C_FIXED_NEWSTYLE;
    }
    if no_zeroes {
        client_flags |= NBD_FLAG_C_NO_ZEROES;
    }
    s.write_all(&client_flags.to_be_bytes())?;
    s.flush()?;

    let info = if fixed_newstyle {
        match opt_go(s, export)? {
            Some(info) => info,
            None => {
                debug!("NBD server does not support NBD_OPT_GO, using NBD_OPT_EXPORT_NAME");
                opt_export_name(s, export, no_zeroes)?
            }
        }
    } else {
        opt_export_name(s, export, no_zeroes)?
    };

    validate(info)
}

#[cfg(test)]
pub(crate) mod tests {
    use std::convert::TryInto;
    use std::os::unix::net::UnixStream;
    use std::thread;

    use super::*;

    pub(crate) struct ServerHandshake {
        pub flags: u16,
        pub size: u64,
        pub export: &'static str,
        pub support_go: bool,
        pub block_size: Option<(u32, u32, u32)>,
    }

    impl ServerHandshake {
        pub(crate) fn run(&self, s: &mut UnixStream) -> io::Result<()> {
            s.write_all(&NBD_INIT_MAGIC.to_be_bytes())?;
            s.write_all(&NBD_OPTS_MAGIC.to_be_bytes())?;
            s.write_all(&(NBD_FLAG_FIXED_NEWSTYLE | NBD_FLAG_NO_ZEROES).to_be_bytes())?;
            let client_flags = read_u32(s)?;
            assert_eq!(
                client_flags,
                NBD_FLAG_C_FIXED_NEWSTYLE | NBD_FLAG_C_NO_ZEROES
            );

            loop {
                assert_eq!(read_u64(s)?, NBD_OPTS_MAGIC);
                let option = read_u32(s)?;
                let len = read_u32(s)?;
                let mut data = vec![0u8; len as usize];
                s.read_exact(&mut data)?;
                let reply = |s: &mut UnixStream, t: u32, payload: &[u8]| -> io::Result<()> {
                    s.write_all(&NBD_REP_MAGIC.to_be_bytes())?;
                    s.write_all(&option.to_be_bytes())?;
                    s.write_all(&t.to_be_bytes())?;
                    s.write_all(&(payload.len() as u32).to_be_bytes())?;
                    s.write_all(payload)
                };
                match option {
                    NBD_OPT_GO if self.support_go => {
                        let name_len = u32::from_be_bytes(data[0..4].try_into().unwrap());
                        let name = &data[4..4 + name_len as usize];
                        if name != self.export.as_bytes() {
                            reply(s, NBD_REP_ERR_UNKNOWN, b"no such export")?;
                            continue;
                        }
                        let mut info = NBD_INFO_EXPORT.to_be_bytes().to_vec();
                        info.extend_from_slice(&self.size.to_be_bytes());
                        info.extend_from_slice(&self.flags.to_be_bytes());
                        reply(s, NBD_REP_INFO, &info)?;
                        if let Some((min, opt, max)) = self.block_size {
                            let mut info = NBD_INFO_BLOCK_SIZE.to_be_bytes().to_vec();
                            info.extend_from_slice(&min.to_be_bytes());
                            info.extend_from_slice(&opt.to_be_bytes());
                            info.extend_from_slice(&max.to_be_bytes());
                            reply(s, NBD_REP_INFO, &info)?;
                        }
                        return reply(s, NBD_REP_ACK, &[]);
                    }
                    NBD_OPT_EXPORT_NAME => {
                        assert_eq!(data, self.export.as_bytes());
                        s.write_all(&self.size.to_be_bytes())?;
                        return s.write_all(&self.flags.to_be_bytes());
                    }
                    _ => reply(s, NBD_REP_ERR_UNSUP, &[])?,
                }
            }
        }
    }

    fn run_pair(server: ServerHandshake, export: &str) -> Result<NbdExportInfo> {
        let (mut client, mut srv) = UnixStream::pair().unwrap();
        let handle = thread::spawn(move || {
            let _ = server.run(&mut srv);
        });
        let r = negotiate(&mut client, export);
        drop(client);
        handle.join().unwrap();
        r
    }

    fn server(support_go: bool) -> ServerHandshake {
        ServerHandshake {
            flags: NBD_FLAG_HAS_FLAGS | NBD_FLAG_SEND_FLUSH,
            size: 1 << 20,
            export: "vol",
            support_go,
            block_size: Some((512, 4096, 1 << 20)),
        }
    }

    #[test]
    fn go_success() {
        let info = run_pair(server(true), "vol").unwrap();
        assert_eq!(
            info,
            NbdExportInfo {
                size: 1 << 20,
                flags: NBD_FLAG_HAS_FLAGS | NBD_FLAG_SEND_FLUSH,
                min_block: 512,
                opt_block: 4096,
                max_block: 1 << 20,
            }
        );
    }

    #[test]
    fn multiple_connections_need_multi_conn() {
        let mut info = run_pair(server(true), "vol").unwrap();
        assert!(info.allows_connections(1));
        assert!(!info.allows_connections(2));
        info.flags |= NBD_FLAG_CAN_MULTI_CONN;
        assert!(info.allows_connections(1));
        assert!(info.allows_connections(4));
    }

    #[test]
    fn export_name_fallback() {
        let info = run_pair(server(false), "vol").unwrap();
        assert_eq!(info.size, 1 << 20);
        assert_eq!(info.min_block, 1);
        assert_eq!(info.max_block, NBD_DEFAULT_MAX_PAYLOAD);
    }

    #[test]
    fn unknown_export() {
        assert!(matches!(
            run_pair(server(true), "other"),
            Err(NbdHandshakeError::UnknownExport(_))
        ));
    }

    #[test]
    fn bad_block_size_rejected() {
        let mut srv = server(true);
        srv.block_size = Some((3, 4096, 1 << 20));
        assert!(matches!(
            run_pair(srv, "vol"),
            Err(NbdHandshakeError::Protocol(_))
        ));
    }

    #[test]
    fn oldstyle_rejected() {
        let (mut client, mut srv) = UnixStream::pair().unwrap();
        srv.write_all(&NBD_INIT_MAGIC.to_be_bytes()).unwrap();
        srv.write_all(&NBD_CLIENT_MAGIC.to_be_bytes()).unwrap();
        assert!(matches!(
            negotiate(&mut client, ""),
            Err(NbdHandshakeError::Oldstyle)
        ));
    }

    #[test]
    fn bad_magic_rejected() {
        let (mut client, mut srv) = UnixStream::pair().unwrap();
        srv.write_all(&0u64.to_be_bytes()).unwrap();
        assert!(matches!(
            negotiate(&mut client, ""),
            Err(NbdHandshakeError::BadMagic(0))
        ));
    }
}
