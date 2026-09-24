// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::time::Duration;

use serde::Deserialize;

pub(crate) const MMDS_ADDRESS: &str = "http://169.254.169.254";
const MMDS_TOKEN_TTL_SECONDS: &str = "60";
const MMDS_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Default, Deserialize)]
pub(crate) struct Metadata {
    #[serde(rename = "instanceID", default)]
    pub(crate) sandbox_id: String,
    #[serde(rename = "envID", default)]
    pub(crate) template_id: String,
    #[serde(rename = "address", default)]
    pub(crate) collector_address: String,
    #[serde(rename = "accessTokenHash", default)]
    pub(crate) access_token_hash: zeroize::Zeroizing<String>,
}

pub(crate) fn mmds_client() -> Result<reqwest::Client, reqwest::Error> {
    reqwest::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(MMDS_REQUEST_TIMEOUT)
        .pool_max_idle_per_host(0)
        .build()
}

pub(crate) async fn current_metadata() -> Option<Metadata> {
    let client = mmds_client().ok()?;
    fetch_metadata(&client, MMDS_ADDRESS).await.ok()
}

pub(crate) async fn fetch_metadata(
    client: &reqwest::Client,
    address: &str,
) -> Result<Metadata, ()> {
    let token = bounded(
        client
            .put(format!("{address}/latest/api/token"))
            .header("X-metadata-token-ttl-seconds", MMDS_TOKEN_TTL_SECONDS)
            .send()
            .await
            .map_err(|_| ())?,
    )
    .await?;
    if token.is_empty() {
        return Err(());
    }
    let body = bounded(
        client
            .get(format!("{address}/"))
            .header("X-metadata-token", token.as_slice())
            .header("Accept", "application/json")
            .send()
            .await
            .map_err(|_| ())?,
    )
    .await?;
    serde_json::from_slice(&body).map_err(|_| ())
}

async fn bounded(mut response: reqwest::Response) -> Result<zeroize::Zeroizing<Vec<u8>>, ()> {
    let mut bytes = zeroize::Zeroizing::new(Vec::new());
    while let Some(chunk) = response.chunk().await.map_err(|_| ())? {
        if bytes.len() + chunk.len() > crate::transport::limits::INIT_BODY_LIMIT {
            return Err(());
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        http::HeaderMap,
        routing::{get, put},
        Router,
    };

    #[tokio::test]
    async fn metadata_handshake_uses_token_and_bounds_response() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = format!("http://{}", listener.local_addr().unwrap());
        let router = Router::new()
            .route(
                "/latest/api/token",
                put(|headers: HeaderMap| async move {
                    assert_eq!(headers["x-metadata-token-ttl-seconds"], "60");
                    "metadata-test-token"
                }),
            )
            .route(
                "/",
                get(|headers: HeaderMap| async move {
                    assert_eq!(headers["x-metadata-token"], "metadata-test-token");
                    assert_eq!(headers["accept"], "application/json");
                    r#"{"accessTokenHash":"hash","instanceID":"ignored-for-init"}"#
                }),
            )
            .route(
                "/large",
                get(|| async { vec![b'x'; crate::transport::limits::INIT_BODY_LIMIT + 1] }),
            );
        let server = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        let client = mmds_client().unwrap();
        let metadata = fetch_metadata(&client, &address).await.unwrap();
        assert_eq!(metadata.access_token_hash.as_str(), "hash");
        let response = client.get(format!("{address}/large")).send().await.unwrap();
        assert!(bounded(response).await.is_err());
        server.abort();
    }
}
