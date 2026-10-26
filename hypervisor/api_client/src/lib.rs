// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::fmt;
use std::io::{Read, Write};
use std::os::unix::io::RawFd;
use vmm_sys_util::sock_ctrl_msg::ScmSocket;

#[derive(Debug)]
pub enum Error {
    Socket(std::io::Error),
    SocketSendFds(vmm_sys_util::errno::Error),
    StatusCodeParsing(std::num::ParseIntError),
    MissingProtocol,
    ContentLengthParsing(std::num::ParseIntError),
    ServerResponse(StatusCode, Option<String>),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        use Error::*;
        match self {
            Socket(e) => write!(f, "Error writing to or reading from HTTP socket: {}", e),
            SocketSendFds(e) => write!(f, "Error writing to or reading from HTTP socket: {}", e),
            StatusCodeParsing(e) => write!(f, "Error parsing HTTP status code: {}", e),
            MissingProtocol => write!(f, "HTTP output is missing protocol statement"),
            ContentLengthParsing(e) => write!(f, "Error parsing HTTP Content-Length field: {}", e),
            ServerResponse(s, o) => {
                if let Some(o) = o {
                    write!(f, "Server responded with an error: {:?}: {}", s, o)
                } else {
                    write!(f, "Server responded with an error: {:?}", s)
                }
            }
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub enum StatusCode {
    Continue,
    Ok,
    NoContent,
    BadRequest,
    NotFound,
    InternalServerError,
    NotImplemented,
    Unknown,
}

impl StatusCode {
    fn from_raw(code: usize) -> StatusCode {
        match code {
            100 => StatusCode::Continue,
            200 => StatusCode::Ok,
            204 => StatusCode::NoContent,
            400 => StatusCode::BadRequest,
            404 => StatusCode::NotFound,
            500 => StatusCode::InternalServerError,
            501 => StatusCode::NotImplemented,
            _ => StatusCode::Unknown,
        }
    }

    fn parse(code: &str) -> Result<StatusCode, Error> {
        Ok(StatusCode::from_raw(
            code.trim().parse().map_err(Error::StatusCodeParsing)?,
        ))
    }

    fn is_server_error(self) -> bool {
        !matches!(
            self,
            StatusCode::Ok | StatusCode::Continue | StatusCode::NoContent
        )
    }
}

fn get_header<'a>(res: &'a str, header: &'a str) -> Option<&'a str> {
    let header_str = format!("{}: ", header);
    res.find(&header_str)
        .map(|o| &res[o + header_str.len()..o + res[o..].find('\r').unwrap()])
}

fn get_status_code(res: &str) -> Result<StatusCode, Error> {
    if let Some(o) = res.find("HTTP/1.1") {
        Ok(StatusCode::parse(
            &res[o + "HTTP/1.1 ".len()..res[o..].find('\r').unwrap()],
        )?)
    } else {
        Err(Error::MissingProtocol)
    }
}

fn parse_http_response(socket: &mut dyn Read) -> Result<Option<String>, Error> {
    let mut res = String::new();
    let mut body_offset = None;
    let mut content_length: Option<usize> = None;
    loop {
        let mut bytes = vec![0; 256];
        let count = socket.read(&mut bytes).map_err(Error::Socket)?;
        // If the return value is 0, the peer has performed an orderly shutdown.
        if count == 0 {
            break;
        }
        res.push_str(std::str::from_utf8(&bytes[0..count]).unwrap());

        // End of headers
        if let Some(o) = res.find("\r\n\r\n") {
            body_offset = Some(o + "\r\n\r\n".len());

            // With all headers available we can see if there is any body
            content_length = if let Some(length) = get_header(&res, "Content-Length") {
                Some(length.trim().parse().map_err(Error::ContentLengthParsing)?)
            } else {
                None
            };

            if content_length.is_none() {
                break;
            }
        }

        if let Some(body_offset) = body_offset {
            if let Some(content_length) = content_length {
                if res.len() >= content_length + body_offset {
                    break;
                }
            }
        }
    }
    let body_string = content_length.and(body_offset.map(|o| String::from(&res[o..])));
    let status_code = get_status_code(&res)?;

    if status_code.is_server_error() {
        Err(Error::ServerResponse(status_code, body_string))
    } else {
        Ok(body_string)
    }
}

/// Make an API request using the fully qualified command name.
/// For example, full_command could be "vm.create" or "vmm.ping".
pub fn simple_api_full_command_with_fds_and_response<T: Read + Write + ScmSocket>(
    socket: &mut T,
    method: &str,
    full_command: &str,
    request_body: Option<&str>,
    request_fds: Vec<RawFd>,
) -> Result<Option<String>, Error> {
    socket
        .send_with_fds(
            &[format!(
                "{} /api/v1/{} HTTP/1.1\r\nHost: localhost\r\nAccept: */*\r\n",
                method, full_command
            )
            .as_bytes()],
            &request_fds,
        )
        .map_err(Error::SocketSendFds)?;

    if let Some(request_body) = request_body {
        socket
            .write_all(format!("Content-Length: {}\r\n", request_body.len()).as_bytes())
            .map_err(Error::Socket)?;
    }

    socket.write_all(b"\r\n").map_err(Error::Socket)?;

    if let Some(request_body) = request_body {
        socket
            .write_all(request_body.as_bytes())
            .map_err(Error::Socket)?;
    }

    socket.flush().map_err(Error::Socket)?;

    parse_http_response(socket)
}

pub fn simple_api_full_command_with_fds<T: Read + Write + ScmSocket>(
    socket: &mut T,
    method: &str,
    full_command: &str,
    request_body: Option<&str>,
    request_fds: Vec<RawFd>,
) -> Result<(), Error> {
    let response = simple_api_full_command_with_fds_and_response(
        socket,
        method,
        full_command,
        request_body,
        request_fds,
    )?;

    if response.is_some() {
        println!("{}", response.unwrap());
    }

    Ok(())
}

pub fn simple_api_full_command<T: Read + Write + ScmSocket>(
    socket: &mut T,
    method: &str,
    full_command: &str,
    request_body: Option<&str>,
) -> Result<(), Error> {
    simple_api_full_command_with_fds(socket, method, full_command, request_body, Vec::new())
}

pub fn simple_api_full_command_and_response<T: Read + Write + ScmSocket>(
    socket: &mut T,
    method: &str,
    full_command: &str,
    request_body: Option<&str>,
) -> Result<Option<String>, Error> {
    simple_api_full_command_with_fds_and_response(
        socket,
        method,
        full_command,
        request_body,
        Vec::new(),
    )
}

pub fn simple_api_command_with_fds<T: Read + Write + ScmSocket>(
    socket: &mut T,
    method: &str,
    c: &str,
    request_body: Option<&str>,
    request_fds: Vec<RawFd>,
) -> Result<(), Error> {
    // Create the full VM command. For VMM commands, use
    // simple_api_full_command().
    let full_command = format!("vm.{}", c);

    simple_api_full_command_with_fds(socket, method, &full_command, request_body, request_fds)
}

pub fn simple_api_command<T: Read + Write + ScmSocket>(
    socket: &mut T,
    method: &str,
    c: &str,
    request_body: Option<&str>,
) -> Result<(), Error> {
    simple_api_command_with_fds(socket, method, c, request_body, Vec::new())
}
