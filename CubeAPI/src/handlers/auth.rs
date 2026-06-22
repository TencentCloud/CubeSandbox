// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// WebUI username/password login for the digital assistant console.
//
// This is a lightweight, DB-backed credential check that issues an opaque
// session token. The default account is `admin`/`admin` (seeded on first
// migration) and the password can be changed. Token enforcement is performed
// client-side by the WebUI route guard; the CubeAPI request-level gate remains
// the optional `auth_callback_url` middleware.

use axum::{extract::State, http::HeaderMap, http::StatusCode, response::IntoResponse, Json};
use serde::{Deserialize, Serialize};

use crate::{
    error::{AppError, AppResult},
    state::AppState,
};

const SESSION_HEADER: &str = "x-session-token";
const SESSION_TTL_SECS: i64 = 24 * 60 * 60;
const INVALID_CURRENT_PASSWORD: &str = "current password is incorrect or user not found";

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct LoginRequest {
    pub username: String,
    pub password: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LoginResponse {
    pub token: String,
    pub username: String,
    pub expires_in_secs: i64,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SessionResponse {
    /// Whether login is enforced at all (only when the database is configured).
    pub auth_required: bool,
    pub authenticated: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub username: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ChangePasswordRequest {
    pub username: String,
    pub old_password: String,
    pub new_password: String,
}

fn session_token(headers: &HeaderMap) -> Option<String> {
    headers
        .get(SESSION_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
}

fn password_matches(stored: Option<&str>, candidate: &str) -> bool {
    stored
        .map(|expected| crate::crypto::verify_password(expected, candidate))
        .unwrap_or(false)
}

pub async fn login(
    State(state): State<AppState>,
    Json(body): Json<LoginRequest>,
) -> AppResult<impl IntoResponse> {
    let username = body.username.trim();
    let password = body.password;
    if username.is_empty() || password.is_empty() {
        return Err(AppError::BadRequest(
            "username and password are required".to_string(),
        ));
    }

    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "login is unavailable because the database is not configured".to_string(),
        ));
    };

    let stored = store
        .get_user_password(username)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to read user: {}", e)))?;

    if !password_matches(stored.as_deref(), &password) {
        return Err(AppError::Unauthorized("invalid credentials".to_string()));
    }

    let token = uuid::Uuid::new_v4().simple().to_string();
    store
        .create_session(&token, username, SESSION_TTL_SECS)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to create session: {}", e)))?;

    Ok((
        StatusCode::OK,
        Json(LoginResponse {
            token,
            username: username.to_string(),
            expires_in_secs: SESSION_TTL_SECS,
        }),
    ))
}

pub async fn session(
    State(state): State<AppState>,
    headers: HeaderMap,
) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        // No database → login is not enforced; the WebUI runs open.
        return Ok((
            StatusCode::OK,
            Json(SessionResponse {
                auth_required: false,
                authenticated: false,
                username: None,
            }),
        ));
    };

    let username = match session_token(&headers) {
        Some(token) => store.validate_session(&token).await.map_err(|e| {
            AppError::Internal(anyhow::anyhow!("failed to validate session: {}", e))
        })?,
        None => None,
    };

    Ok((
        StatusCode::OK,
        Json(SessionResponse {
            auth_required: true,
            authenticated: username.is_some(),
            username,
        }),
    ))
}

pub async fn logout(
    State(state): State<AppState>,
    headers: HeaderMap,
) -> AppResult<impl IntoResponse> {
    if let (Some(store), Some(token)) = (&state.agenthub_store, session_token(&headers)) {
        store
            .delete_session(&token)
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to delete session: {}", e)))?;
    }
    Ok(StatusCode::NO_CONTENT)
}

pub async fn change_password(
    State(state): State<AppState>,
    Json(body): Json<ChangePasswordRequest>,
) -> AppResult<impl IntoResponse> {
    let username = body.username.trim();
    if username.is_empty() || body.old_password.is_empty() || body.new_password.is_empty() {
        return Err(AppError::BadRequest(
            "username, oldPassword and newPassword are required".to_string(),
        ));
    }
    if body.new_password.len() < 4 {
        return Err(AppError::BadRequest(
            "new password must be at least 4 characters".to_string(),
        ));
    }

    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "the database is not configured".to_string(),
        ));
    };

    let stored = store
        .get_user_password(username)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to read user: {}", e)))?;
    if !password_matches(stored.as_deref(), &body.old_password) {
        return Err(AppError::Unauthorized(INVALID_CURRENT_PASSWORD.to_string()));
    }

    let new_hash = crate::crypto::hash_password(&body.new_password)
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to hash password: {}", e)))?;
    store
        .set_user_password(username, &new_hash)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to update password: {}", e)))?;

    Ok(StatusCode::NO_CONTENT)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn session_ttl_is_limited_to_one_day() {
        assert_eq!(SESSION_TTL_SECS, 24 * 60 * 60);
    }

    #[test]
    fn password_matches_rejects_missing_user() {
        assert!(!password_matches(None, "password"));
    }

    #[test]
    fn password_matches_supports_hashed_and_legacy_passwords() {
        let hash = crate::crypto::hash_password("secret").expect("hash should succeed");

        assert!(password_matches(Some(&hash), "secret"));
        assert!(!password_matches(Some(&hash), "wrong"));
        assert!(password_matches(Some("legacy-secret"), "legacy-secret"));
        assert!(!password_matches(Some("legacy-secret"), "wrong"));
    }

    #[test]
    fn change_password_uses_non_enumerating_error_message() {
        assert_eq!(
            INVALID_CURRENT_PASSWORD,
            "current password is incorrect or user not found"
        );
    }
}
