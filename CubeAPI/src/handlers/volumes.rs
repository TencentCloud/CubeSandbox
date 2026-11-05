// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Volume API handlers — aligned with the E2B openapi.yml `/volumes` spec.
// Each handler delegates to VolumeService which in turn calls CubeMaster.

use axum::{
    extract::{rejection::JsonRejection, Path, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};

use crate::{
    error::{AppError, AppResult},
    models::{
        ApiError, NewVolume, NewVolumeV2, Volume, VolumeAndToken, MAX_VOLUME_NAME_LEN,
        MAX_VOLUME_PRIVATE_DATA_LEN,
    },
    state::AppState,
};

// ── GET /volumes ──────────────────────────────────────────────────────────

/// List all volumes.
///
/// Returns `200 OK` with `[Volume]` on success.
#[utoipa::path(
    get,
    path = "/volumes",
    responses(
        (status = 200, description = "Successfully listed all volumes",  body = [Volume]),
        (status = 401, description = "Authentication error",              body = ApiError),
        (status = 500, description = "Server error",                      body = ApiError),
    ),
    security(("apiKeyAuth" = []))
)]
pub async fn list_volumes(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    tracing::debug!("list_volumes");
    let items = state.services.volumes.list().await?;
    Ok((StatusCode::OK, Json(items)))
}

// ── POST /volumes ─────────────────────────────────────────────────────────

/// Create a new volume.
///
/// Returns `201 Created` with `VolumeAndToken` on success.
#[utoipa::path(
    post,
    path = "/volumes",
    request_body = NewVolume,
    responses(
        (status = 201, description = "Successfully created a new volume", body = VolumeAndToken),
        (status = 400, description = "Bad request",                        body = ApiError),
        (status = 401, description = "Authentication error",               body = ApiError),
        (status = 500, description = "Server error",                       body = ApiError),
    ),
    security(("apiKeyAuth" = []))
)]
pub async fn create_volume(
    State(state): State<AppState>,
    Json(body): Json<NewVolume>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(name = %body.name, driver = ?body.driver, "create_volume");
    validate_volume_name(&body)?;
    let vol = state
        .services
        .volumes
        .create(body.name, body.driver)
        .await?;
    tracing::info!(volume_id = %vol.volume_id, "create_volume: success");
    Ok((StatusCode::CREATED, Json(vol)))
}

/// Create a volume with opaque plugin input. Query, delete and mount it through
/// the existing volume APIs using the returned volumeID.
#[utoipa::path(
    post,
    path = "/v2/volumes",
    request_body = NewVolumeV2,
    responses(
        (status = 201, description = "Successfully created a new volume", body = VolumeAndToken),
        (status = 400, description = "Bad request", body = ApiError),
        (status = 401, description = "Authentication error", body = ApiError),
        (status = 500, description = "Server error", body = ApiError),
    ),
    security(("apiKeyAuth" = []))
)]
pub async fn create_volume_v2(
    State(state): State<AppState>,
    body: Result<Json<NewVolumeV2>, JsonRejection>,
) -> AppResult<impl IntoResponse> {
    // Keep malformed private_data values out of parser error responses.
    let Json(body) = match body {
        Ok(body) => body,
        Err(rejection) => {
            let status = match rejection {
                JsonRejection::JsonDataError(_) | JsonRejection::JsonSyntaxError(_) => {
                    StatusCode::BAD_REQUEST
                }
                _ => rejection.status(),
            };
            return Ok((
                status,
                Json(ApiError::new(status.as_u16() as i32, "invalid request")),
            )
                .into_response());
        }
    };
    tracing::debug!(name = %body.base.name, driver = ?body.base.driver, "create_volume_v2");
    validate_volume_name(&body.base)?;

    if let Some(private_data) = &body.private_data {
        if private_data.len() > MAX_VOLUME_PRIVATE_DATA_LEN {
            return Err(AppError::BadRequest(format!(
                "private_data exceeds max length {MAX_VOLUME_PRIVATE_DATA_LEN}"
            )));
        }
        if private_data.contains('\0') {
            return Err(AppError::BadRequest(
                "private_data must not contain NUL".to_string(),
            ));
        }
        if private_data.chars().any(|character| character > '\u{FFFF}') {
            return Err(AppError::BadRequest(
                "private_data must not contain characters outside the BMP".to_string(),
            ));
        }
    }

    let vol = state
        .services
        .volumes
        .create_with_private_data(body.base.name, body.base.driver, body.private_data)
        .await?;

    tracing::info!(volume_id = %vol.volume_id, "create_volume_v2: success");
    Ok((StatusCode::CREATED, Json(vol)).into_response())
}

fn validate_volume_name(body: &NewVolume) -> AppResult<()> {
    if !body.name.is_empty() && !body.name_is_valid() {
        return Err(AppError::BadRequest(format!(
            "volume name must match ^[a-zA-Z0-9_-]+$ and be at most {MAX_VOLUME_NAME_LEN} characters"
        )));
    }
    Ok(())
}

// ── GET /volumes/{volumeID} ───────────────────────────────────────────────

/// Get info for a single volume (includes auth token).
///
/// Returns `200 OK` with `VolumeAndToken` on success.
#[utoipa::path(
    get,
    path = "/volumes/{volumeID}",
    params(
        ("volumeID" = String, Path, description = "Identifier of the volume")
    ),
    responses(
        (status = 200, description = "Successfully retrieved a volume", body = VolumeAndToken),
        (status = 401, description = "Authentication error",            body = ApiError),
        (status = 404, description = "Not found",                       body = ApiError),
        (status = 500, description = "Server error",                    body = ApiError),
    ),
    security(("apiKeyAuth" = []))
)]
pub async fn get_volume(
    State(state): State<AppState>,
    Path(volume_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(volume_id = %volume_id, "get_volume");
    let vol = state.services.volumes.get(&volume_id).await?;
    Ok((StatusCode::OK, Json(vol)))
}

// ── DELETE /volumes/{volumeID} ────────────────────────────────────────────

/// Delete a volume.
///
/// Returns `204 No Content` on success.
#[utoipa::path(
    delete,
    path = "/volumes/{volumeID}",
    params(
        ("volumeID" = String, Path, description = "Identifier of the volume")
    ),
    responses(
        (status = 204, description = "Successfully deleted a volume"),
        (status = 401, description = "Authentication error", body = ApiError),
        (status = 404, description = "Not found",            body = ApiError),
        (status = 500, description = "Server error",         body = ApiError),
    ),
    security(("apiKeyAuth" = []))
)]
pub async fn delete_volume(
    State(state): State<AppState>,
    Path(volume_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(volume_id = %volume_id, "delete_volume");
    state.services.volumes.delete(&volume_id).await?;
    Ok(StatusCode::NO_CONTENT)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
    };
    use axum::{routing::post, Router};
    use axum_test::TestServer;
    use serde_json::{json, Value};
    use std::sync::{Arc, Mutex};

    type Requests = Arc<Mutex<Vec<Value>>>;

    #[tokio::test]
    async fn legacy_create_ignores_plugin_input() {
        let (server, requests) = volume_server().await;
        for input in [
            json!({"marker": "private-data-sentinel"}),
            json!("x".repeat(1025)),
        ] {
            let response = server
                .post("/volumes")
                .json(&json!({"name": "vol-private-data", "driver": "s3", "private_data": input}))
                .await;
            assert_eq!(response.status_code(), StatusCode::CREATED);
            let request = requests.lock().unwrap().pop().unwrap();
            assert_eq!(request["name"], "vol-private-data");
            assert_eq!(request["driver"], "s3");
            assert!(request.get("private_data").is_none());
        }
    }

    #[test]
    fn volume_openapi_preserves_legacy_schema() {
        let document = serde_json::to_value(crate::openapi::build_openapi()).unwrap();
        let properties = document["components"]["schemas"]["NewVolume"]["properties"]
            .as_object()
            .unwrap();
        assert_eq!(properties.len(), 2);
        assert!(properties.contains_key("name"));
        assert!(properties.contains_key("driver"));
        assert_eq!(
            document["paths"]["/volumes"]["post"]["requestBody"]["content"]["application/json"]
                ["schema"]["$ref"],
            "#/components/schemas/NewVolume"
        );
        assert_eq!(
            document["paths"]["/v2/volumes"]["post"]["requestBody"]["content"]["application/json"]
                ["schema"]["$ref"],
            "#/components/schemas/NewVolumeV2"
        );
        assert_eq!(
            document["paths"]["/volumes"]["post"]["responses"]["201"],
            document["paths"]["/v2/volumes"]["post"]["responses"]["201"]
        );
    }

    // Exercise the public route, service, HTTP client and serialized Master
    // request together, without provisioning any external storage.
    async fn volume_server() -> (TestServer, Requests) {
        volume_server_with_key(None).await
    }

    async fn volume_server_with_key(key: Option<String>) -> (TestServer, Requests) {
        volume_server_with_master_response(
            key,
            StatusCode::OK,
            json!({
                "ret": {"ret_code": 0, "ret_msg": "ok"},
                "volume": {
                    "volumeID": "vol-private-data",
                    "name": "vol-private-data",
                    "token": "test-token",
                    "private_data": "plugin-state-B-sentinel"
                }
            }),
        )
        .await
    }

    async fn volume_server_with_master_response(
        key: Option<String>,
        status: StatusCode,
        response: Value,
    ) -> (TestServer, Requests) {
        async fn create(
            State((requests, status, response)): State<(Requests, StatusCode, Value)>,
            Json(body): Json<Value>,
        ) -> (StatusCode, Json<Value>) {
            requests.lock().unwrap().push(body);
            (status, Json(response))
        }

        let requests = Requests::default();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let master = Router::new()
            .route("/cube/volume", post(create))
            .with_state((requests.clone(), status, response));
        tokio::spawn(async move { axum::serve(listener, master).await.unwrap() });
        let config = ServerConfig {
            cubemaster_url: format!("http://{address}"),
            cube_api_key: key,
            ..ServerConfig::default()
        };
        let state = AppState::new(config, arc(NoopLogger)).await;
        let server = TestServer::new(crate::routes::build_router(state)).unwrap();
        (server, requests)
    }

    #[tokio::test]
    async fn create_volume_preserves_master_private_data_error_for_both_http_statuses() {
        let message = "private_data must not contain characters outside the BMP";
        for master_status in [StatusCode::OK, StatusCode::BAD_REQUEST] {
            let (server, requests) = volume_server_with_master_response(
                None,
                master_status,
                json!({"ret": {"ret_code": 130400, "ret_msg": message}}),
            )
            .await;
            for path in ["/volumes", "/v2/volumes"] {
                // Valid A reaches Master. Its rejection can represent invalid
                // plugin-returned B; the API must preserve the parameter error.
                let response = server
                    .post(path)
                    .json(&json!({"name": "vol-private-data", "private_data": "valid-input-A"}))
                    .await;
                assert_eq!(response.status_code(), StatusCode::BAD_REQUEST);
                let error: ApiError = response.json();
                assert_eq!(error.code, 400);
                assert_eq!(
                    error.message,
                    format!("CubeMaster returned error code 130400: {message}")
                );
                let mut captured = requests.lock().unwrap();
                assert_eq!(captured.len(), 1);
                let request = captured.pop().unwrap();
                if path == "/v2/volumes" {
                    assert_eq!(request["private_data"], "valid-input-A");
                } else {
                    assert!(request.get("private_data").is_none());
                }
            }
        }
    }

    #[tokio::test]
    async fn both_create_routes_use_existing_auth() {
        let (server, requests) = volume_server_with_key(Some("test-key".into())).await;
        for path in ["/volumes", "/v2/volumes"] {
            let body = json!({"name": "vol-private-data", "private_data": "input-A"});
            let response = server.post(path).json(&body).await;
            assert_eq!(response.status_code(), StatusCode::UNAUTHORIZED);
            assert!(requests.lock().unwrap().is_empty());
            let response = server
                .post(path)
                .add_header(
                    axum::http::header::AUTHORIZATION,
                    axum::http::HeaderValue::from_static("Bearer test-key"),
                )
                .json(&body)
                .await;
            assert_eq!(response.status_code(), StatusCode::CREATED);
            let request = requests.lock().unwrap().pop().unwrap();
            assert_eq!(request.get("private_data").is_some(), path == "/v2/volumes");
        }
    }

    #[tokio::test]
    async fn legacy_create_keeps_json_type_rejection_status() {
        let (server, requests) = volume_server().await;
        let response = server.post("/volumes").json(&json!({"name": 123})).await;
        assert_eq!(response.status_code(), StatusCode::UNPROCESSABLE_ENTITY);
        assert!(requests.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn create_volume_forwards_private_data_unchanged() {
        let (server, requests) = volume_server().await;
        for input in [
            "  opaque text 桶 '\" \\ $()\n ".to_string(),
            "a".repeat(1024),
            format!("{}a", "界".repeat(341)),
        ] {
            let response = server
                .post("/v2/volumes")
                .json(&json!({"name": "vol-private-data", "driver": "s3", "private_data": input}))
                .await;
            assert_eq!(response.status_code(), StatusCode::CREATED);
            assert_eq!(
                response.json::<Value>(),
                json!({
                    "volumeID": "vol-private-data", "name": "vol-private-data", "token": "test-token"
                })
            );
            let mut captured = requests.lock().unwrap();
            assert_eq!(captured.len(), 1);
            let request = captured.pop().unwrap();
            assert_eq!(request["private_data"], input);
            assert_eq!(request["name"], "vol-private-data");
            assert_eq!(request["driver"], "s3");
            assert!(request["request_id"]
                .as_str()
                .is_some_and(|id| !id.is_empty()));
        }
    }

    #[tokio::test]
    async fn create_volume_preserves_optional_private_data_behavior() {
        let (server, requests) = volume_server().await;
        for input in [None, Some(Value::Null), Some(json!(""))] {
            let mut body = json!({});
            if let Some(value) = input {
                body["private_data"] = value;
            }
            let response = server.post("/v2/volumes").json(&body).await;
            assert_eq!(response.status_code(), StatusCode::CREATED);
            let request = requests.lock().unwrap().pop().unwrap();
            assert_eq!(request["name"], "");
            assert!(request.get("driver").is_none());
            if body["private_data"] == "" {
                assert_eq!(request["private_data"], "");
            } else {
                assert!(request.get("private_data").is_none());
            }
        }
    }

    #[tokio::test]
    async fn create_volume_rejects_invalid_private_data_before_master() {
        let (server, requests) = volume_server().await;
        for input in [
            "a".repeat(1025),
            "界".repeat(342),
            "😀".to_string(),
            "𠀀".to_string(),
            "private-data-sentinel\0tail".to_string(),
        ] {
            let response = server
                .post("/v2/volumes")
                .json(&json!({"name": "vol-private-data", "private_data": input.clone()}))
                .await;
            assert_eq!(response.status_code(), StatusCode::BAD_REQUEST);
            let error: ApiError = response.json();
            assert_eq!(error.code, 400);
            assert!(!error.message.contains(&input));
            assert!(!error.message.contains("private-data-sentinel"));
            assert!(requests.lock().unwrap().is_empty());
        }
    }

    #[tokio::test]
    async fn create_volume_accepts_bmp_private_data_without_rewriting() {
        let (server, requests) = volume_server().await;
        for input in [
            "é".to_string(),
            "中".to_string(),
            "\u{2028}".to_string(),
            "\u{FFFF}".to_string(),
            "\u{FFFD}".to_string(),
            "a".repeat(1024),
            r"literal \ud83d\ude00".to_string(),
        ] {
            let response = server
                .post("/v2/volumes")
                .json(&json!({"name": "vol-private-data", "private_data": input.clone()}))
                .await;
            assert_eq!(response.status_code(), StatusCode::CREATED);
            let request = requests.lock().unwrap().pop().unwrap();
            assert_eq!(request["private_data"], input);
        }
    }

    #[tokio::test]
    async fn create_volume_rejects_escaped_supplementary_private_data_before_master() {
        let (server, requests) = volume_server().await;
        let response = server
            .post("/v2/volumes")
            .text(r#"{"name":"vol-private-data","private_data":"\ud83d\ude00"}"#)
            .content_type("application/json")
            .await;
        assert_eq!(response.status_code(), StatusCode::BAD_REQUEST);
        let error: ApiError = response.json();
        assert_eq!(
            error.message,
            "private_data must not contain characters outside the BMP"
        );
        assert!(requests.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn create_volume_rejects_invalid_private_data_type_without_echo() {
        let (server, requests) = volume_server().await;
        for input in [
            json!({"marker": "private-data-sentinel"}),
            json!(["private-data-sentinel"]),
            json!(987654321012345_u64),
        ] {
            let response = server
                .post("/v2/volumes")
                .json(&json!({"private_data": input}))
                .await;
            assert_eq!(response.status_code(), StatusCode::BAD_REQUEST);
            let error: ApiError = response.json();
            assert_eq!(error.message, "invalid request");
            assert!(requests.lock().unwrap().is_empty());
        }
    }

    #[tokio::test]
    async fn create_volume_rejects_malformed_json_without_echo() {
        let (server, requests) = volume_server().await;
        for body in [
            r#"{"private_data":"private-data-sentinel\q"}"#,
            r#"{"private_data":"private-data-sentinel""#,
        ] {
            let response = server
                .post("/v2/volumes")
                .text(body)
                .content_type("application/json")
                .await;
            assert_eq!(response.status_code(), StatusCode::BAD_REQUEST);
            let error: ApiError = response.json();
            assert_eq!(error.message, "invalid request");
            assert!(requests.lock().unwrap().is_empty());
        }
    }

    #[tokio::test]
    async fn create_volume_preserves_transport_rejection_status() {
        let (server, requests) = volume_server().await;
        let response = server.post("/v2/volumes").text("{}").await;
        assert_eq!(response.status_code(), StatusCode::UNSUPPORTED_MEDIA_TYPE);
        let response = server
            .post("/v2/volumes")
            .json(&json!({"private_data": "x".repeat(3 * 1024 * 1024)}))
            .await;
        assert_eq!(response.status_code(), StatusCode::PAYLOAD_TOO_LARGE);
        assert!(requests.lock().unwrap().is_empty());
    }
}
