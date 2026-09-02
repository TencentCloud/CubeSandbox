// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::utils::{ADDRESS_FILE, SHIM_PID_FILE};
use crate::service::tools;
use crate::{common::utils, service::task_srv::TaskService};
use async_trait::async_trait;
use containerd_shim::{
    asynchronous::{publisher::RemotePublisher, spawn, ExitSignal, Shim},
    protos::api,
    Config, Error, Flags, StartOpts,
};

use nix::sys::signal::Signal;
use std::{fs, sync::Arc};

#[derive(Clone)]
pub struct Service {
    id: String,
    ns: String,
    exit: Arc<ExitSignal>,
    debug: bool,
}

#[async_trait]
impl Shim for Service {
    type T = TaskService;

    async fn new(_runtime: &str, flags: &Flags, _config: &mut Config) -> Self {
        Service {
            id: flags.id.clone(),
            ns: flags.namespace.clone(),
            exit: Arc::new(ExitSignal::default()),
            debug: flags.debug,
        }
    }

    async fn start_shim(&mut self, opts: StartOpts) -> Result<String, Error> {
        let grouping = opts.id.clone();
        let address: String = spawn(opts, &grouping, Vec::new()).await?;
        fs::write(ADDRESS_FILE, address.as_bytes()).map_err(|e| Error::IoError {
            context: "write address file failed".to_string(),
            err: e,
        })?;

        /*
        fs::write(SHIM_PID_FILE, format!("{}", "0")).map_err(|e| Error::IoError {
            context: "write pid file failed".to_string(),
            err: e,
        })?;
        */
        Ok(address)
    }

    async fn delete_shim(&mut self) -> Result<api::DeleteResponse, Error> {
        //return Ok(api::DeleteResponse::new());
        //kill shim
        if let Ok(shim_pid) = tools::read_number_from_file(SHIM_PID_FILE) {
            if let Ok(()) = tools::signal(shim_pid, None) {
                let _ = tools::signal(shim_pid, Some(Signal::SIGKILL));
            }
        }

        if let Ok(sk_file) = tools::read_address(ADDRESS_FILE) {
            let _ = fs::remove_file(sk_file.as_str());
        }

        // Binary delete: also removes /data/cubelet/log/<id> so a crashed
        // shim does not leak host log files until a later live Task.Delete.
        utils::Utils::clean_sandbox_resource(&self.id).map_err(Error::Other)?;

        Ok(api::DeleteResponse::new())
    }

    async fn wait(&mut self) {
        self.exit.wait().await;
    }

    async fn create_task_service(&self, publisher: RemotePublisher) -> Self::T {
        TaskService::new(
            self.id.clone(),
            self.ns.clone(),
            self.debug,
            self.exit.clone(),
            publisher,
        )
        .await
    }
}
