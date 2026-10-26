// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::api::http::{error_response, EndpointHandler, HttpError};
#[cfg(feature = "guest_debug")]
use crate::api::vm_coredump;
use crate::api::{
    vm_add_device, vm_add_disk, vm_add_fs, vm_add_net, vm_add_pmem, vm_add_user_device,
    vm_add_vdpa, vm_add_vsock, vm_boot, vm_counters, vm_create, vm_delete, vm_info, vm_pause,
    vm_power_button, vm_reboot, vm_receive_migration, vm_remove_device, vm_resize, vm_resize_zone,
    vm_restore, vm_resume, vm_send_migration, vm_shutdown, vm_snapshot, vm_wait_start, vmm_ping,
    vmm_shutdown, VmAction, VmConfig,
};
use crate::api::{vm_pause2snapshot, vm_resume_from_snapshot, vm_set_fs};
use crate::vm_config::NetConfig;
use micro_http::{Body, Method, Request, Response, StatusCode, Version};
use std::fs::File;
use std::os::unix::io::IntoRawFd;
use std::sync::Arc;

// /api/v1/vm.create handler
pub struct VmCreate {}

impl EndpointHandler for VmCreate {
    fn handle_request(&self, req: &Request) -> Response {
        match req.method() {
            Method::Put => {
                match &req.body {
                    Some(body) => {
                        // Deserialize into a VmConfig
                        let vm_config: Box<VmConfig> = match serde_json::from_slice(body.raw())
                            .map_err(HttpError::SerdeJsonDeserialize)
                        {
                            Ok(config) => config,
                            Err(e) => return error_response(e, StatusCode::BadRequest),
                        };

                        // Call vm_create()
                        match vm_create(vm_config).map_err(HttpError::ApiError) {
                            Ok(_) => Response::new(Version::Http11, StatusCode::NoContent),
                            Err(e) => error_response(e, StatusCode::InternalServerError),
                        }
                    }

                    None => Response::new(Version::Http11, StatusCode::BadRequest),
                }
            }

            _ => error_response(HttpError::BadRequest, StatusCode::BadRequest),
        }
    }
}

// Common handler for boot, shutdown and reboot
pub struct VmActionHandler {
    action: VmAction,
}

impl VmActionHandler {
    pub fn new(action: VmAction) -> Self {
        VmActionHandler { action }
    }
}

impl EndpointHandler for VmActionHandler {
    fn put_handler(
        &self,
        body: &Option<Body>,
        mut files: Vec<File>,
    ) -> std::result::Result<Option<Body>, HttpError> {
        use VmAction::*;
        if let Some(body) = body {
            match self.action {
                AddDevice(_) => vm_add_device(Arc::new(serde_json::from_slice(body.raw())?)),
                AddDisk(_) => vm_add_disk(Arc::new(serde_json::from_slice(body.raw())?)),
                AddFs(_) => vm_add_fs(Arc::new(serde_json::from_slice(body.raw())?)),
                SetFs(_) => vm_set_fs(Arc::new(serde_json::from_slice(body.raw())?)),
                AddPmem(_) => vm_add_pmem(Arc::new(serde_json::from_slice(body.raw())?)),
                AddNet(_) => {
                    let mut net_cfg: NetConfig = serde_json::from_slice(body.raw())?;
                    // Update network config with optional files that might have
                    // been sent through control message.
                    if !files.is_empty() {
                        let fds = files.drain(..).map(|f| f.into_raw_fd()).collect();
                        net_cfg.fds = Some(fds);
                    }
                    vm_add_net(Arc::new(net_cfg))
                }
                AddVdpa(_) => vm_add_vdpa(Arc::new(serde_json::from_slice(body.raw())?)),
                AddVsock(_) => vm_add_vsock(Arc::new(serde_json::from_slice(body.raw())?)),
                AddUserDevice(_) => {
                    vm_add_user_device(Arc::new(serde_json::from_slice(body.raw())?))
                }
                RemoveDevice(_) => vm_remove_device(Arc::new(serde_json::from_slice(body.raw())?)),
                Resize(_) => vm_resize(Arc::new(serde_json::from_slice(body.raw())?)),
                ResizeZone(_) => vm_resize_zone(Arc::new(serde_json::from_slice(body.raw())?)),
                Restore(_) => vm_restore(Arc::new(serde_json::from_slice(body.raw())?)),
                Snapshot(_) => vm_snapshot(Arc::new(serde_json::from_slice(body.raw())?)),
                PauseToSnapshot(_) => {
                    vm_pause2snapshot(Arc::new(serde_json::from_slice(body.raw())?))
                }
                ResumeFromSnapshot(_) => {
                    vm_resume_from_snapshot(Arc::new(serde_json::from_slice(body.raw())?))
                }
                #[cfg(all(target_arch = "x86_64", feature = "guest_debug"))]
                Coredump(_) => vm_coredump(Arc::new(serde_json::from_slice(body.raw())?)),
                ReceiveMigration(_) => {
                    vm_receive_migration(Arc::new(serde_json::from_slice(body.raw())?))
                }
                SendMigration(_) => {
                    vm_send_migration(Arc::new(serde_json::from_slice(body.raw())?))
                }

                _ => return Err(HttpError::BadRequest),
            }
        } else {
            match self.action {
                Boot => vm_boot(),
                Delete => vm_delete(),
                Shutdown => vm_shutdown(),
                Reboot => vm_reboot(),
                Pause => vm_pause(),
                Resume => vm_resume(),
                PowerButton => vm_power_button(),
                _ => return Err(HttpError::BadRequest),
            }
        }
        .map_err(HttpError::ApiError)
    }

    fn get_handler(&self, _body: &Option<Body>) -> std::result::Result<Option<Body>, HttpError> {
        use VmAction::*;
        match self.action {
            Counters => vm_counters().map_err(HttpError::ApiError),
            _ => Err(HttpError::BadRequest),
        }
    }
}

// /api/v1/vm.info handler
pub struct VmInfo {}

impl EndpointHandler for VmInfo {
    fn handle_request(&self, req: &Request) -> Response {
        match req.method() {
            Method::Get => match vm_info().map_err(HttpError::ApiError) {
                Ok(info) => {
                    let mut response = Response::new(Version::Http11, StatusCode::OK);
                    let info_serialized = serde_json::to_string(&info).unwrap();

                    response.set_body(Body::new(info_serialized));
                    response
                }
                Err(e) => error_response(e, StatusCode::InternalServerError),
            },
            _ => error_response(HttpError::BadRequest, StatusCode::BadRequest),
        }
    }
}

// /api/v1/vmm.info handler
pub struct VmmPing {}

impl EndpointHandler for VmmPing {
    fn handle_request(&self, req: &Request) -> Response {
        match req.method() {
            Method::Get => match vmm_ping().map_err(HttpError::ApiError) {
                Ok(pong) => {
                    let mut response = Response::new(Version::Http11, StatusCode::OK);
                    let info_serialized = serde_json::to_string(&pong).unwrap();

                    response.set_body(Body::new(info_serialized));
                    response
                }
                Err(e) => error_response(e, StatusCode::InternalServerError),
            },

            _ => error_response(HttpError::BadRequest, StatusCode::BadRequest),
        }
    }
}

// /api/v1/vmm.shutdown handler
pub struct VmmShutdown {}

impl EndpointHandler for VmmShutdown {
    fn handle_request(&self, req: &Request) -> Response {
        match req.method() {
            Method::Put => match vmm_shutdown().map_err(HttpError::ApiError) {
                Ok(_) => Response::new(Version::Http11, StatusCode::OK),
                Err(e) => error_response(e, StatusCode::InternalServerError),
            },
            _ => error_response(HttpError::BadRequest, StatusCode::BadRequest),
        }
    }
}

// /api/v1/vm.waitstart handler
pub struct VmWaitStart {}

impl EndpointHandler for VmWaitStart {
    fn handle_request(&self, req: &Request) -> Response {
        match req.method() {
            Method::Get => match vm_wait_start().map_err(HttpError::ApiError) {
                Ok(wait) => {
                    let mut response = Response::new(Version::Http11, StatusCode::OK);
                    let info_serialized = serde_json::to_string(&wait).unwrap();

                    response.set_body(Body::new(info_serialized));
                    response
                }
                Err(e) => error_response(e, StatusCode::InternalServerError),
            },

            _ => error_response(HttpError::BadRequest, StatusCode::BadRequest),
        }
    }
}
