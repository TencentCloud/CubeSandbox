// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::sync::{Arc, Mutex};

use axum::extract::State as AxumState;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Serialize;

use crate::{Error, Fault, Outcome, State, Transfer, Worker};

pub trait Terminator: Send + Sync + 'static {
    fn terminate(&self);
}

pub struct Abort;

impl Terminator for Abort {
    fn terminate(&self) {
        std::process::abort();
    }
}

#[derive(Clone)]
struct Ctx {
    terminator: Arc<dyn Terminator>,
    worker: Arc<Mutex<Worker>>,
}

#[derive(Serialize)]
struct Health {
    status: &'static str,
}

pub fn app(worker: Worker, terminator: Arc<dyn Terminator>) -> Router {
    let ctx = Ctx {
        terminator,
        worker: Arc::new(Mutex::new(worker)),
    };

    Router::new()
        .route("/health", get(health))
        .route("/state", get(state))
        .route("/transfers", post(transfer))
        .with_state(ctx)
}

async fn health() -> Json<Health> {
    Json(Health { status: "ok" })
}

async fn state(AxumState(ctx): AxumState<Ctx>) -> Result<Json<State>, StatusCode> {
    let worker = ctx
        .worker
        .lock()
        .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    Ok(Json(worker.state().clone()))
}

#[derive(Serialize)]
struct TransferResult {
    outcome: Outcome,
}

struct ApiError {
    code: &'static str,
    message: String,
    status: StatusCode,
}

#[derive(Serialize)]
struct ErrorBody {
    code: &'static str,
    message: String,
}

#[derive(Serialize)]
struct ErrorEnvelope {
    error: ErrorBody,
}

impl ApiError {
    fn internal(message: impl Into<String>) -> Self {
        Self {
            code: "internal_error",
            message: message.into(),
            status: StatusCode::INTERNAL_SERVER_ERROR,
        }
    }
}

impl From<Error> for ApiError {
    fn from(error: Error) -> Self {
        let (status, code) = match error {
            Error::AccountNotFound(_) => (StatusCode::BAD_REQUEST, "account_not_found"),
            Error::InsufficientBalance { .. } => (StatusCode::BAD_REQUEST, "insufficient_balance"),
            Error::InvalidAmount => (StatusCode::BAD_REQUEST, "invalid_amount"),
            Error::InvalidId => (StatusCode::BAD_REQUEST, "invalid_id"),
            Error::Io(_) | Error::Json(_) => {
                (StatusCode::INTERNAL_SERVER_ERROR, "persistence_error")
            }
            Error::RequestConflict(_) => (StatusCode::CONFLICT, "request_conflict"),
            Error::SameAccount => (StatusCode::BAD_REQUEST, "same_account"),
        };

        Self {
            code,
            message: error.to_string(),
            status,
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (
            self.status,
            Json(ErrorEnvelope {
                error: ErrorBody {
                    code: self.code,
                    message: self.message,
                },
            }),
        )
            .into_response()
    }
}

async fn transfer(
    AxumState(ctx): AxumState<Ctx>,
    headers: HeaderMap,
    Json(request): Json<Transfer>,
) -> Result<(StatusCode, Json<TransferResult>), ApiError> {
    let fault = match headers
        .get("x-fault-point")
        .and_then(|value| value.to_str().ok())
    {
        None => Fault::None,
        Some("after_debit") => Fault::AfterDebit,
        Some(_) => {
            return Err(ApiError {
                code: "invalid_fault_point",
                message: "x-fault-point must be after_debit".to_owned(),
                status: StatusCode::BAD_REQUEST,
            });
        }
    };

    let mut worker = ctx
        .worker
        .lock()
        .map_err(|_| ApiError::internal("worker state lock is poisoned"))?;
    let outcome = worker.transfer(request, fault)?;
    drop(worker);

    if outcome == Outcome::FaultInjected {
        ctx.terminator.terminate();

        return Err(ApiError::internal("fault terminator returned unexpectedly"));
    }

    let status = match outcome {
        Outcome::Committed => StatusCode::CREATED,
        Outcome::Duplicate => StatusCode::OK,
        Outcome::FaultInjected => StatusCode::INTERNAL_SERVER_ERROR,
    };

    Ok((status, Json(TransferResult { outcome })))
}
