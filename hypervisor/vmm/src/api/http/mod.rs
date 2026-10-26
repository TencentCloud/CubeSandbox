// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use self::http_endpoint::{VmActionHandler, VmCreate, VmInfo, VmWaitStart, VmmPing, VmmShutdown};
use crate::api::{ApiError, VmAction};
use crate::seccomp_filters::{get_seccomp_filter, Thread};
use crate::{Error as VmmError, Result};
use core::fmt;
use hypervisor::HypervisorType;
use micro_http::{Body, HttpServer, MediaType, Method, Request, Response, StatusCode, Version};
use once_cell::sync::Lazy;
use seccompiler::{apply_filter, SeccompAction};
use serde_json::Error as SerdeError;
use std::collections::BTreeMap;
use std::fmt::Display;
use std::fs::File;
use std::os::unix::io::{IntoRawFd, RawFd};
use std::os::unix::net::UnixListener;
use std::panic::AssertUnwindSafe;
use std::path::PathBuf;
use std::sync::Arc;
use std::thread;
use vmm_sys_util::eventfd::EventFd;

pub mod http_endpoint;

/// Errors associated with VMM management
#[derive(Debug)]
pub enum HttpError {
    /// API request receive error
    SerdeJsonDeserialize(SerdeError),

    /// Attempt to access unsupported HTTP method
    BadRequest,

    /// Undefined endpoints
    NotFound,

    /// Internal Server Error
    InternalServerError,

    /// Error from internal API
    ApiError(ApiError),
}

impl Display for HttpError {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        use self::HttpError::*;
        match self {
            BadRequest => write!(f, "Bad Request"),
            NotFound => write!(f, "Not Found"),
            InternalServerError => write!(f, "Internal Server Error"),
            SerdeJsonDeserialize(serde_error) => write!(f, "{}", serde_error),
            ApiError(api_error) => write!(f, "{}", api_error),
        }
    }
}

impl From<serde_json::Error> for HttpError {
    fn from(e: serde_json::Error) -> Self {
        HttpError::SerdeJsonDeserialize(e)
    }
}

const HTTP_ROOT: &str = "/api/v1";

pub fn error_response(error: HttpError, status: StatusCode) -> Response {
    let mut response = Response::new(Version::Http11, status);
    response.set_body(Body::new(format!("{error}")));

    response
}

/// An HTTP endpoint handler interface
pub trait EndpointHandler {
    /// Handles an HTTP request.
    /// After parsing the request, the handler could decide to send an
    /// associated API request down to the VMM API server to e.g. create
    /// or start a VM. The request will block waiting for an answer from the
    /// API server and translate that into an HTTP response.
    fn handle_request(&self, req: &Request) -> Response {
        // Cloning the files here is very important as it dup() the file
        // descriptors, leaving open the one that was received. This way,
        // rebooting the VM will work since the VM will be created from the
        // original file descriptors.
        let files = req.files.iter().map(|f| f.try_clone().unwrap()).collect();
        let res = match req.method() {
            Method::Put => self.put_handler(&req.body, files),
            Method::Get => self.get_handler(&req.body),
            _ => return Response::new(Version::Http11, StatusCode::BadRequest),
        };

        match res {
            Ok(response_body) => {
                if let Some(body) = response_body {
                    let mut response = Response::new(Version::Http11, StatusCode::OK);
                    response.set_body(body);
                    response
                } else {
                    Response::new(Version::Http11, StatusCode::NoContent)
                }
            }
            Err(e @ HttpError::BadRequest) => error_response(e, StatusCode::BadRequest),
            Err(e @ HttpError::SerdeJsonDeserialize(_)) => {
                error_response(e, StatusCode::BadRequest)
            }
            Err(e) => error_response(e, StatusCode::InternalServerError),
        }
    }

    fn put_handler(
        &self,
        _body: &Option<Body>,
        _files: Vec<File>,
    ) -> std::result::Result<Option<Body>, HttpError> {
        Err(HttpError::BadRequest)
    }

    fn get_handler(&self, _body: &Option<Body>) -> std::result::Result<Option<Body>, HttpError> {
        Err(HttpError::BadRequest)
    }
}

/// An HTTP routes structure.
pub struct HttpRoutes {
    /// routes is a hash table mapping endpoint URIs to their endpoint handlers.
    pub routes: BTreeMap<String, Box<dyn EndpointHandler + Sync + Send>>,
}

macro_rules! endpoint {
    ($path:expr) => {
        format!("{}{}", HTTP_ROOT, $path)
    };
}

/// HTTP_ROUTES contain all the cloud-hypervisor HTTP routes.
pub static HTTP_ROUTES: Lazy<HttpRoutes> = Lazy::new(|| {
    let mut r = HttpRoutes {
        routes: BTreeMap::new(),
    };

    r.routes.insert(
        endpoint!("/vm.add-device"),
        Box::new(VmActionHandler::new(VmAction::AddDevice(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.add-user-device"),
        Box::new(VmActionHandler::new(
            VmAction::AddUserDevice(Arc::default()),
        )),
    );
    r.routes.insert(
        endpoint!("/vm.add-disk"),
        Box::new(VmActionHandler::new(VmAction::AddDisk(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.add-fs"),
        Box::new(VmActionHandler::new(VmAction::AddFs(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.set-fs"),
        Box::new(VmActionHandler::new(VmAction::SetFs(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.add-net"),
        Box::new(VmActionHandler::new(VmAction::AddNet(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.add-pmem"),
        Box::new(VmActionHandler::new(VmAction::AddPmem(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.add-vdpa"),
        Box::new(VmActionHandler::new(VmAction::AddVdpa(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.add-vsock"),
        Box::new(VmActionHandler::new(VmAction::AddVsock(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.boot"),
        Box::new(VmActionHandler::new(VmAction::Boot)),
    );
    r.routes.insert(
        endpoint!("/vm.counters"),
        Box::new(VmActionHandler::new(VmAction::Counters)),
    );
    r.routes
        .insert(endpoint!("/vm.create"), Box::new(VmCreate {}));
    r.routes.insert(
        endpoint!("/vm.delete"),
        Box::new(VmActionHandler::new(VmAction::Delete)),
    );
    r.routes.insert(endpoint!("/vm.info"), Box::new(VmInfo {}));
    r.routes.insert(
        endpoint!("/vm.pause"),
        Box::new(VmActionHandler::new(VmAction::Pause)),
    );
    r.routes.insert(
        endpoint!("/vm.pause2snapshot"),
        Box::new(VmActionHandler::new(VmAction::PauseToSnapshot(
            Arc::default(),
        ))),
    );
    r.routes.insert(
        endpoint!("/vm.power-button"),
        Box::new(VmActionHandler::new(VmAction::PowerButton)),
    );
    r.routes.insert(
        endpoint!("/vm.reboot"),
        Box::new(VmActionHandler::new(VmAction::Reboot)),
    );
    r.routes.insert(
        endpoint!("/vm.receive-migration"),
        Box::new(VmActionHandler::new(VmAction::ReceiveMigration(
            Arc::default(),
        ))),
    );
    r.routes.insert(
        endpoint!("/vm.remove-device"),
        Box::new(VmActionHandler::new(VmAction::RemoveDevice(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.resize"),
        Box::new(VmActionHandler::new(VmAction::Resize(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.resize-zone"),
        Box::new(VmActionHandler::new(VmAction::ResizeZone(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.restore"),
        Box::new(VmActionHandler::new(VmAction::Restore(Arc::default()))),
    );
    r.routes.insert(
        endpoint!("/vm.resume"),
        Box::new(VmActionHandler::new(VmAction::Resume)),
    );
    r.routes.insert(
        endpoint!("/vm.resume-from-snapshot"),
        Box::new(VmActionHandler::new(VmAction::ResumeFromSnapshot(
            Arc::default(),
        ))),
    );
    r.routes.insert(
        endpoint!("/vm.send-migration"),
        Box::new(VmActionHandler::new(
            VmAction::SendMigration(Arc::default()),
        )),
    );
    r.routes.insert(
        endpoint!("/vm.shutdown"),
        Box::new(VmActionHandler::new(VmAction::Shutdown)),
    );
    r.routes.insert(
        endpoint!("/vm.snapshot"),
        Box::new(VmActionHandler::new(VmAction::Snapshot(Arc::default()))),
    );
    #[cfg(feature = "guest_debug")]
    r.routes.insert(
        endpoint!("/vm.coredump"),
        Box::new(VmActionHandler::new(VmAction::Coredump(Arc::default()))),
    );
    r.routes
        .insert(endpoint!("/vmm.ping"), Box::new(VmmPing {}));
    r.routes
        .insert(endpoint!("/vmm.shutdown"), Box::new(VmmShutdown {}));
    r.routes
        .insert(endpoint!("/vm.waitstart"), Box::new(VmWaitStart {}));

    r
});

fn handle_http_request(request: &Request) -> Response {
    let path = request.uri().get_abs_path().to_string();
    let mut response = match HTTP_ROUTES.routes.get(&path) {
        Some(route) => route.handle_request(request),
        None => error_response(HttpError::NotFound, StatusCode::NotFound),
    };

    response.set_server("Cloud Hypervisor API");
    response.set_content_type(MediaType::ApplicationJson);
    response
}

fn start_http_thread(
    mut server: HttpServer,
    seccomp_action: &SeccompAction,
    exit_evt: EventFd,
    hypervisor_type: HypervisorType,
) -> Result<thread::JoinHandle<Result<()>>> {
    // Retrieve seccomp filter for API thread
    let api_seccomp_filter = get_seccomp_filter(seccomp_action, Thread::Api, hypervisor_type)
        .map_err(VmmError::CreateSeccompFilter)?;

    thread::Builder::new()
        .name("http-server".to_string())
        .spawn(move || {
            // Apply seccomp filter for API thread.
            if !api_seccomp_filter.is_empty() {
                apply_filter(&api_seccomp_filter)
                    .map_err(VmmError::ApplySeccompFilter)
                    .map_err(|e| {
                        error!("Error applying seccomp filter: {:?}", e);
                        exit_evt.write(1).ok();
                        e
                    })?;
            }

            std::panic::catch_unwind(AssertUnwindSafe(move || {
                server.start_server().unwrap();
                loop {
                    match server.requests() {
                        Ok(request_vec) => {
                            for server_request in request_vec {
                                if let Err(e) =
                                    server.respond(server_request.process(handle_http_request))
                                {
                                    error!("HTTP server error on response: {}", e);
                                }
                            }
                        }
                        Err(e) => {
                            error!(
                                "HTTP server error on retrieving incoming request. Error: {}",
                                e
                            );
                        }
                    }
                }
            }))
            .map_err(|_| {
                error!("http-server thread panicked");
                exit_evt.write(1).ok()
            })
            .ok();

            Ok(())
        })
        .map_err(VmmError::HttpThreadSpawn)
}

pub fn start_http_path_thread(
    path: &str,

    seccomp_action: &SeccompAction,
    exit_evt: EventFd,
    hypervisor_type: HypervisorType,
) -> Result<thread::JoinHandle<Result<()>>> {
    let socket_path = PathBuf::from(path);
    let socket_fd = UnixListener::bind(socket_path).map_err(VmmError::CreateApiServerSocket)?;
    // SAFETY: Valid FD just opened
    let server = unsafe { HttpServer::new_from_fd(socket_fd.into_raw_fd()) }
        .map_err(VmmError::CreateApiServer)?;
    start_http_thread(server, seccomp_action, exit_evt, hypervisor_type)
}

pub fn start_http_fd_thread(
    fd: RawFd,
    seccomp_action: &SeccompAction,
    exit_evt: EventFd,
    hypervisor_type: HypervisorType,
) -> Result<thread::JoinHandle<Result<()>>> {
    // SAFETY: Valid FD
    let server = unsafe { HttpServer::new_from_fd(fd) }.map_err(VmmError::CreateApiServer)?;
    start_http_thread(server, seccomp_action, exit_evt, hypervisor_type)
}
