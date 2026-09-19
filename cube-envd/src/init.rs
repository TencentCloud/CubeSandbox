use crate::defaults::{self, InitDefaults};
use axum::{
    extract::Json,
    http::StatusCode,
    response::{IntoResponse, Response},
};
use serde::Deserialize;
use std::collections::HashMap;

/// `POST /init` request. Upstream envd also sends `volumeMounts`,
/// `timestamp`, and `hyperloopIP`; those are accepted and ignored.
#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct InitRequest {
    #[serde(default)]
    env_vars: HashMap<String, String>,
    #[serde(default)]
    default_user: Option<String>,
    #[serde(default)]
    default_workdir: Option<String>,
    #[serde(default)]
    access_token: Option<String>,
}

pub async fn init(Json(request): Json<InitRequest>) -> Response {
    defaults::update(InitDefaults {
        env_vars: request.env_vars,
        user: request.default_user,
        workdir: request.default_workdir,
        access_token: request.access_token,
    });
    StatusCode::NO_CONTENT.into_response()
}

/// `GET /envs` returns the environment variables established by `/init`.
pub async fn envs() -> Json<HashMap<String, String>> {
    Json(defaults::snapshot().env_vars)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn init_request_decodes_camel_case() {
        let request: InitRequest = serde_json::from_slice(
            br#"{"envVars":{"FOO":"bar"},"defaultWorkdir":"/tmp","accessToken":"secret"}"#,
        )
        .expect("init request decodes");
        assert_eq!(request.env_vars.get("FOO").map(String::as_str), Some("bar"));
        assert_eq!(request.default_workdir.as_deref(), Some("/tmp"));
        assert_eq!(request.access_token.as_deref(), Some("secret"));
    }
}
