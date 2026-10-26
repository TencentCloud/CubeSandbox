// Copyright © 2023 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::api::{ApiRequest, ApiResponse};
use lazy_static::lazy_static;
use std::io;
use std::sync::mpsc::{Receiver, RecvError, SendError, Sender};
use std::sync::Mutex;
use thiserror::Error;
use vmm_sys_util::eventfd::EventFd;

lazy_static! {
    pub static ref VMM_SERVICE: Mutex<VmmService> = Mutex::new(VmmService::new());
}

#[derive(Error, Debug)]
pub enum Error {
    #[error("Cannot write to EventFd: {0}")]
    EventFdWrite(#[source] io::Error),
    #[error("API request send error: {0}")]
    RequestSend(#[source] SendError<ApiRequest>),
    #[error("API response receive error: {0}")]
    ResponseRecv(#[source] RecvError),
    /// Vmm Service not init
    #[error("Vmm Service not init")]
    NotInit,
}

/// Vmm Api Service
/// A global static Vmm Api Sender to Vmm thread
pub struct VmmService {
    inner: Option<VmmServiceInner>,
}

impl VmmService {
    pub fn new() -> Self {
        Self { inner: None }
    }

    pub fn init(
        &mut self,
        api_sender: Sender<ApiRequest>,
        api_evt: EventFd,
        res_receiver: Receiver<ApiResponse>,
    ) -> Result<(), Error> {
        self.inner = Some(VmmServiceInner::new(api_sender, api_evt, res_receiver));
        Ok(())
    }

    pub fn send_request(&self, request: ApiRequest) -> Result<ApiResponse, Error> {
        if let Some(instance) = self.inner.as_ref() {
            return instance.send_request(request);
        }
        Err(Error::NotInit)
    }
}

impl Default for VmmService {
    fn default() -> Self {
        Self::new()
    }
}

struct VmmServiceInner {
    api_sender: Sender<ApiRequest>,
    api_evt: EventFd,
    res_receiver: Receiver<ApiResponse>,
}

impl VmmServiceInner {
    pub fn new(
        api_sender: Sender<ApiRequest>,
        api_evt: EventFd,
        res_receiver: Receiver<ApiResponse>,
    ) -> Self {
        Self {
            api_sender,
            api_evt,
            res_receiver,
        }
    }

    pub fn send_request(&self, request: ApiRequest) -> Result<ApiResponse, Error> {
        self.api_sender.send(request).map_err(Error::RequestSend)?;
        self.api_evt.write(1).map_err(Error::EventFdWrite)?;
        self.res_receiver.recv().map_err(Error::ResponseRecv)
    }
}
