// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use crate::logging::LogEvent;
use reqwest::{StatusCode, Url};
use serde::Serialize;
use std::time::Duration;

#[derive(Debug, Serialize)]
pub(crate) struct InternalBatch {
    schema_version: u8,
    events: Vec<LogEvent>,
}

impl InternalBatch {
    pub(crate) fn new(events: Vec<LogEvent>) -> Self {
        Self {
            schema_version: 1,
            events,
        }
    }

    pub(crate) fn event_count(&self) -> usize {
        self.events.len()
    }
}

#[derive(Clone)]
pub(crate) struct OpsClient {
    client: reqwest::Client,
    endpoint: Url,
}

impl OpsClient {
    pub(crate) fn new(base_url: &str, timeout: Duration) -> anyhow::Result<Self> {
        let base = Url::parse(base_url)?;
        let endpoint = base.join("/internal/webhook/events/batch")?;
        let client = reqwest::Client::builder().timeout(timeout).build()?;
        Ok(Self { client, endpoint })
    }

    pub(crate) async fn send_once(
        &self,
        batch: &InternalBatch,
    ) -> Result<StatusCode, reqwest::Error> {
        self.client
            .post(self.endpoint.clone())
            .json(batch)
            .send()
            .await
            .map(|response| response.status())
    }
}
