// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! NBD URI parsing (including optional transport/timeout/reconnect query parameters).
//!
//! Supported forms:
//! - `nbd://[host][:port][/export][?handshake_timeout=S&socket_timeout=S&reconnect_timeout=S&tcp_keepalive=on|off...]`
//! - `nbd+unix:///[export]?socket=<path>[&handshake_timeout=S&socket_timeout=S&reconnect_timeout=S]`

use std::fmt;
use std::path::{Path, PathBuf};
use std::str::{self, FromStr};
use std::time::Duration;

use thiserror::Error;

use crate::conn::{
    NbdSocketOptions, TcpKeepalive, MAX_TCP_KEEPALIVE_COUNT, MAX_TCP_KEEPALIVE_SECS,
};
use crate::proto::{NBD_DEFAULT_PORT, NBD_MAX_STRING_SIZE};

#[derive(Error, Clone, Debug, PartialEq, Eq)]
pub enum NbdUriError {
    #[error("Unsupported NBD URI scheme \"{0}\"")]
    UnsupportedScheme(String),
    #[error("NBD over TLS (\"{0}\") is not supported")]
    TlsNotSupported(String),
    #[error("Invalid NBD URI: {0}")]
    Invalid(&'static str),
    #[error("Invalid port \"{0}\" in NBD URI")]
    InvalidPort(String),
    #[error("Unsupported or invalid query parameter \"{0}\" in NBD URI")]
    UnsupportedQuery(String),
}

/// Returns `true` when `s` starts with a recognized NBD URI scheme.
pub fn is_nbd_uri(s: &str) -> bool {
    if let Some((scheme, _)) = s.split_once("://") {
        matches!(
            scheme.to_ascii_lowercase().as_str(),
            "nbd" | "nbd+unix" | "nbds" | "nbds+unix"
        )
    } else {
        false
    }
}

/// Endpoint address of an NBD server.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum NbdAddress {
    Tcp { host: String, port: u16 },
    Unix { path: PathBuf },
}

/// Parsed NBD URI together with any socket options encoded in its query string.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NbdUri {
    pub address: NbdAddress,
    pub export: String,
    pub socket_options: NbdSocketOptions,
}

impl NbdUri {
    /// Returns the Unix domain socket path if the URI uses Unix transport.
    pub fn socket_path(&self) -> Option<&Path> {
        match &self.address {
            NbdAddress::Unix { path } => Some(path.as_path()),
            NbdAddress::Tcp { .. } => None,
        }
    }
}

impl fmt::Display for NbdUri {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.address {
            NbdAddress::Tcp { host, port } if host.contains(':') => {
                write!(f, "nbd://[{host}]:{port}/{}", self.export)
            }
            NbdAddress::Tcp { host, port } => write!(f, "nbd://{host}:{port}/{}", self.export),
            NbdAddress::Unix { path } => {
                write!(f, "nbd+unix:///{}?socket={}", self.export, path.display())
            }
        }
    }
}

fn percent_decode(s: &str) -> Result<String, NbdUriError> {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' {
            let hex = bytes
                .get(i + 1..i + 3)
                .and_then(|h| str::from_utf8(h).ok())
                .and_then(|h| u8::from_str_radix(h, 16).ok())
                .ok_or(NbdUriError::Invalid("bad percent encoding"))?;
            out.push(hex);
            i += 3;
        } else {
            out.push(bytes[i]);
            i += 1;
        }
    }
    String::from_utf8(out).map_err(|_| NbdUriError::Invalid("export name is not UTF-8"))
}

fn parse_authority(authority: &str) -> Result<(String, u16), NbdUriError> {
    let (host, port) = if let Some(rest) = authority.strip_prefix('[') {
        let (host, rest) = rest
            .split_once(']')
            .ok_or(NbdUriError::Invalid("unterminated IPv6 address"))?;
        let port = match rest {
            "" => None,
            p => Some(
                p.strip_prefix(':')
                    .ok_or(NbdUriError::Invalid("garbage after IPv6 address"))?,
            ),
        };
        (host, port)
    } else {
        match authority.rsplit_once(':') {
            Some((host, port)) => (host, Some(port)),
            None => (authority, None),
        }
    };

    let host = if host.is_empty() { "localhost" } else { host };
    let port = match port {
        None | Some("") => NBD_DEFAULT_PORT,
        Some(p) => p
            .parse::<u16>()
            .ok()
            .filter(|p| *p != 0)
            .ok_or_else(|| NbdUriError::InvalidPort(p.to_string()))?,
    };
    Ok((host.to_string(), port))
}

fn parse_nonzero_u64(key: &str, value: &str) -> Result<u64, NbdUriError> {
    value
        .parse::<u64>()
        .ok()
        .filter(|v| *v > 0)
        .ok_or_else(|| NbdUriError::UnsupportedQuery(format!("{key}={value}")))
}

fn parse_bounded_u32(key: &str, value: &str, max: u32) -> Result<u32, NbdUriError> {
    value
        .parse::<u32>()
        .ok()
        .filter(|v| *v > 0 && *v <= max)
        .ok_or_else(|| NbdUriError::UnsupportedQuery(format!("{key}={value}")))
}

impl FromStr for NbdUri {
    type Err = NbdUriError;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let (scheme, rest) = s
            .split_once("://")
            .ok_or(NbdUriError::Invalid("missing scheme"))?;
        let scheme = scheme.to_ascii_lowercase();
        let unix = match scheme.as_str() {
            "nbd" => false,
            "nbd+unix" => true,
            "nbds" | "nbds+unix" => return Err(NbdUriError::TlsNotSupported(scheme)),
            _ => return Err(NbdUriError::UnsupportedScheme(scheme)),
        };

        if rest.contains('#') {
            return Err(NbdUriError::Invalid("fragments are not supported"));
        }
        let (rest, query) = match rest.split_once('?') {
            Some((r, q)) => (r, Some(q)),
            None => (rest, None),
        };
        let (authority, path) = match rest.find('/') {
            Some(i) => (&rest[..i], &rest[i + 1..]),
            None => (rest, ""),
        };
        let export = percent_decode(path)?;

        let mut socket = None;
        let mut handshake_timeout = None;
        let mut socket_timeout = None;
        let mut reconnect_timeout = None;
        let mut tcp_keepalive = false;
        let mut ka_idle = None;
        let mut ka_interval = None;
        let mut ka_count = None;

        for param in query.into_iter().flat_map(|q| q.split('&')) {
            if param.is_empty() {
                continue;
            }
            let (key, value) = param.split_once('=').unwrap_or((param, ""));
            match key {
                "socket" if unix => socket = Some(PathBuf::from(percent_decode(value)?)),
                "handshake_timeout" => {
                    handshake_timeout = Some(Duration::from_secs(parse_nonzero_u64(key, value)?));
                }
                "socket_timeout" => {
                    socket_timeout = Some(Duration::from_secs(parse_nonzero_u64(key, value)?));
                }
                "reconnect_timeout" => {
                    reconnect_timeout = Some(Duration::from_secs(parse_nonzero_u64(key, value)?));
                }
                "tcp_keepalive" if !unix => match value {
                    "on" | "true" | "1" => tcp_keepalive = true,
                    "off" | "false" | "0" => tcp_keepalive = false,
                    _ => return Err(NbdUriError::UnsupportedQuery(param.to_string())),
                },
                "tcp_keepalive_idle" if !unix => {
                    ka_idle = Some(parse_bounded_u32(key, value, MAX_TCP_KEEPALIVE_SECS)?);
                }
                "tcp_keepalive_interval" if !unix => {
                    ka_interval = Some(parse_bounded_u32(key, value, MAX_TCP_KEEPALIVE_SECS)?);
                }
                "tcp_keepalive_count" if !unix => {
                    ka_count = Some(parse_bounded_u32(key, value, MAX_TCP_KEEPALIVE_COUNT)?);
                }
                _ => return Err(NbdUriError::UnsupportedQuery(key.to_string())),
            }
        }

        if !tcp_keepalive && (ka_idle.is_some() || ka_interval.is_some() || ka_count.is_some()) {
            return Err(NbdUriError::Invalid(
                "tcp_keepalive_* parameters require tcp_keepalive=on",
            ));
        }

        let address = if unix {
            if !authority.is_empty() {
                return Err(NbdUriError::Invalid(
                    "nbd+unix URIs must not contain a host",
                ));
            }
            let path = socket
                .filter(|p| !p.as_os_str().is_empty())
                .ok_or(NbdUriError::Invalid("nbd+unix URIs require ?socket=<path>"))?;
            NbdAddress::Unix { path }
        } else {
            let (host, port) = parse_authority(authority)?;
            NbdAddress::Tcp { host, port }
        };

        if export.len() > NBD_MAX_STRING_SIZE {
            return Err(NbdUriError::Invalid("export name too long"));
        }

        let keepalive = if tcp_keepalive {
            Some(TcpKeepalive::new(ka_idle, ka_interval, ka_count))
        } else {
            None
        };
        let socket_options = NbdSocketOptions::new(handshake_timeout, socket_timeout, keepalive)
            .with_reconnect_timeout(reconnect_timeout);

        Ok(NbdUri {
            address,
            export,
            socket_options,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tcp(host: &str, port: u16, export: &str) -> NbdUri {
        NbdUri {
            address: NbdAddress::Tcp {
                host: host.to_string(),
                port,
            },
            export: export.to_string(),
            socket_options: NbdSocketOptions::default(),
        }
    }

    #[test]
    fn parse_tcp() {
        assert_eq!(
            "nbd://10.0.0.1:1234/vol0".parse(),
            Ok(tcp("10.0.0.1", 1234, "vol0"))
        );
        assert_eq!(
            "nbd://example.com".parse(),
            Ok(tcp("example.com", NBD_DEFAULT_PORT, ""))
        );
        assert_eq!(
            "nbd:///".parse(),
            Ok(tcp("localhost", NBD_DEFAULT_PORT, ""))
        );
        assert_eq!("nbd://[::1]:99/a%20b".parse(), Ok(tcp("::1", 99, "a b")));
        assert_eq!(
            "nbd://host//abs".parse(),
            Ok(tcp("host", NBD_DEFAULT_PORT, "/abs"))
        );
    }

    #[test]
    fn parse_unix_and_query_options() {
        let uri: NbdUri =
            "nbd+unix:///exp?socket=/run/nbd.sock&reconnect_timeout=10&socket_timeout=30"
                .parse()
                .unwrap();
        assert_eq!(uri.socket_path(), Some(Path::new("/run/nbd.sock")));
        assert_eq!(uri.export, "exp");
        assert_eq!(
            uri.socket_options.reconnect_timeout,
            Some(Duration::from_secs(10))
        );
        assert_eq!(
            uri.socket_options.socket_timeout,
            Some(Duration::from_secs(30))
        );
    }

    #[test]
    fn display_round_trip() {
        for s in [
            "nbd://host:10809/x",
            "nbd://[::1]:10809/x",
            "nbd+unix:///x?socket=/tmp/s",
        ] {
            let uri: NbdUri = s.parse().unwrap();
            assert_eq!(uri.to_string(), s);
        }
    }

    #[test]
    fn parse_errors() {
        assert!(matches!(
            "nbds://host/x".parse::<NbdUri>(),
            Err(NbdUriError::TlsNotSupported(_))
        ));
        assert!(matches!(
            "http://host/x".parse::<NbdUri>(),
            Err(NbdUriError::UnsupportedScheme(_))
        ));
        assert!(matches!(
            "nbd://host:0/x".parse::<NbdUri>(),
            Err(NbdUriError::InvalidPort(_))
        ));
        assert!("nbd+unix:///x".parse::<NbdUri>().is_err());
        assert!("nbd+unix://host/x?socket=/s".parse::<NbdUri>().is_err());
        assert!(matches!(
            "nbd://host/x?socket=/s".parse::<NbdUri>(),
            Err(NbdUriError::UnsupportedQuery(_))
        ));
        assert!("nbd://host/%zz".parse::<NbdUri>().is_err());
        assert!("/path/to/file".parse::<NbdUri>().is_err());
    }
}
