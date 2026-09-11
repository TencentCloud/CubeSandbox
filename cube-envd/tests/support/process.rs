// SPDX-License-Identifier: Apache-2.0

// Each integration binary uses the subset needed by its public-wire scenarios.
#![allow(dead_code)]

use reqwest::{Client, Response};
use serde_json::Value;
use std::time::Duration;

pub struct Stream {
    response: Response,
    bytes: Vec<u8>,
    protobuf: bool,
}
impl Stream {
    pub async fn open(client: &Client, port: u16, method: &str, request: Value) -> Self {
        Self::encoded(client, port, method, request, false).await
    }
    pub async fn encoded(
        client: &Client,
        port: u16,
        method: &str,
        request: Value,
        protobuf: bool,
    ) -> Self {
        Self::timed(client, port, method, request, protobuf, None).await
    }
    pub async fn timed(
        client: &Client,
        port: u16,
        method: &str,
        request: Value,
        protobuf: bool,
        timeout: Option<&str>,
    ) -> Self {
        use buffa::Message;
        use cube_envd::proto::process::{ConnectRequest, StartRequest};
        let payload = if protobuf {
            if method == "Start" {
                serde_json::from_value::<StartRequest>(request)
                    .unwrap()
                    .encode_to_vec()
            } else {
                serde_json::from_value::<ConnectRequest>(request)
                    .unwrap()
                    .encode_to_vec()
            }
        } else {
            serde_json::to_vec(&request).unwrap()
        };
        let mut body = vec![0];
        body.extend_from_slice(&(payload.len() as u32).to_be_bytes());
        body.extend(payload);
        let mut builder = client
            .post(format!("http://127.0.0.1:{port}/process.Process/{method}"))
            .header(
                "content-type",
                if protobuf {
                    "application/connect+proto"
                } else {
                    "application/connect+json"
                },
            )
            .header("connect-protocol-version", "1")
            .body(body);
        if let Some(timeout) = timeout {
            builder = builder.header("connect-timeout-ms", timeout);
        }
        let response = builder.send().await.unwrap();
        assert_eq!(response.status(), 200);
        Self {
            response,
            bytes: vec![],
            protobuf,
        }
    }
    pub async fn next(&mut self) -> (u8, Value) {
        self.next_with_timeout(5).await
    }
    pub async fn next_with_timeout(&mut self, seconds: u64) -> (u8, Value) {
        tokio::time::timeout(Duration::from_secs(seconds), async {
            loop {
                if self.bytes.len() >= 5 {
                    let len = u32::from_be_bytes(self.bytes[1..5].try_into().unwrap()) as usize;
                    if self.bytes.len() >= len + 5 {
                        let frame = (
                            self.bytes[0],
                            if self.protobuf && self.bytes[0] == 0 {
                                use buffa::Message;
                                serde_json::to_value(
                                    cube_envd::proto::process::ConnectResponse::decode(
                                        &mut &self.bytes[5..5 + len],
                                    )
                                    .unwrap(),
                                )
                                .unwrap()
                            } else {
                                serde_json::from_slice(&self.bytes[5..5 + len]).unwrap()
                            },
                        );
                        self.bytes.drain(..5 + len);
                        return frame;
                    }
                }
                self.bytes.extend(
                    self.response
                        .chunk()
                        .await
                        .unwrap()
                        .expect("stream ended before expected event"),
                );
            }
        })
        .await
        .expect("stream progress")
    }
}

pub async fn unary(client: &Client, port: u16, method: &str, request: Value) -> (u16, Value) {
    let response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/{method}"))
        .header("connect-protocol-version", "1")
        .json(&request)
        .send()
        .await
        .unwrap();
    (response.status().as_u16(), response.json().await.unwrap())
}

pub async fn unary_proto<Req, Res>(
    client: &Client,
    port: u16,
    method: &str,
    request: Value,
) -> (u16, Value)
where
    Req: buffa::Message + serde::de::DeserializeOwned,
    Res: buffa::Message + serde::Serialize,
{
    let response = client
        .post(format!("http://127.0.0.1:{port}/process.Process/{method}"))
        .header("connect-protocol-version", "1")
        .header("content-type", "application/proto")
        .body(
            serde_json::from_value::<Req>(request)
                .unwrap()
                .encode_to_vec(),
        )
        .send()
        .await
        .unwrap();
    let status = response.status().as_u16();
    if status != 200 {
        return (status, response.json().await.unwrap());
    }
    assert_eq!(response.headers()["content-type"], "application/proto");
    let bytes = response.bytes().await.unwrap();
    let decoded = Res::decode(&mut bytes.as_ref()).unwrap();
    (status, serde_json::to_value(decoded).unwrap())
}
