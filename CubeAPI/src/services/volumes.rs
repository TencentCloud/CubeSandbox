// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// VolumeService — thin adapter between the CubeAPI handler layer and the
// CubeMaster /cube/volume* endpoints.  Each method maps 1:1 to one
// CubeMaster call; all error translation lives here so handlers stay clean.

use uuid::Uuid;

use crate::{
    cubemaster::{CreateVolumeRequest, CubeMasterClient, CubeMasterError, ListVolumesRequest},
    error::{AppError, AppResult},
    models::{Volume, VolumeAndToken},
};

fn new_request_id() -> String {
    Uuid::new_v4().to_string()
}

fn cm_err_to_app(e: CubeMasterError) -> AppError {
    // Map CubeMaster's standard error codes to HTTP semantics:
    //   NotFound (130404) → 404, Conflict (130409) → 409,
    //   MasterParamsError (130400) → 400; everything else → 500.
    match &e {
        CubeMasterError::Api { ret_code, .. } if *ret_code == 404 || *ret_code == 130404 => {
            AppError::NotFound(e.to_string())
        }
        CubeMasterError::Api { ret_code, .. } if *ret_code == 409 || *ret_code == 130409 => {
            AppError::Conflict(e.to_string())
        }
        CubeMasterError::Api { ret_code, .. } if *ret_code == 400 || *ret_code == 130400 => {
            AppError::BadRequest(e.to_string())
        }
        _ => AppError::Internal(anyhow::anyhow!("{e}")),
    }
}

/// Business-logic facade for volume management.
#[derive(Clone)]
pub struct VolumeService {
    cubemaster: CubeMasterClient,
}

impl VolumeService {
    pub fn new(cubemaster: CubeMasterClient) -> Self {
        Self { cubemaster }
    }

    // ── GET /volumes ──────────────────────────────────────────────────────

    /// List all volumes. Returns a flat list without auth tokens
    /// (tokens are only surfaced on create / get-single).
    pub async fn list(&self) -> AppResult<Vec<Volume>> {
        let req = ListVolumesRequest {
            request_id: new_request_id(),
        };

        let resp = self
            .cubemaster
            .list_volumes(&req)
            .await
            .map_err(cm_err_to_app)?;

        resp.ret.into_result().map_err(cm_err_to_app)?;

        Ok(resp
            .items
            .into_iter()
            .map(|v| Volume {
                volume_id: v.volume_id,
                name: v.name,
            })
            .collect())
    }

    // ── POST /volumes ─────────────────────────────────────────────────────

    /// Create a new volume. Returns `VolumeAndToken` so the caller can
    /// hand the token to the volume-content service immediately.
    pub async fn create(&self, name: String, driver: Option<String>) -> AppResult<VolumeAndToken> {
        let req = CreateVolumeRequest {
            request_id: new_request_id(),
            name,
            driver,
        };

        let resp = self
            .cubemaster
            .create_volume(&req)
            .await
            .map_err(cm_err_to_app)?;

        resp.ret.into_result().map_err(cm_err_to_app)?;

        Ok(VolumeAndToken {
            volume_id: resp.volume.volume_id,
            name: resp.volume.name,
            token: resp.volume.token,
        })
    }

    // ── GET /volumes/{volumeID} ───────────────────────────────────────────

    /// Fetch a single volume by ID, including its auth token.
    pub async fn get(&self, volume_id: &str) -> AppResult<VolumeAndToken> {
        let resp = self
            .cubemaster
            .get_volume(volume_id)
            .await
            .map_err(cm_err_to_app)?;

        resp.ret.into_result().map_err(cm_err_to_app)?;

        Ok(VolumeAndToken {
            volume_id: resp.volume.volume_id,
            name: resp.volume.name,
            token: resp.volume.token,
        })
    }

    // ── DELETE /volumes/{volumeID} ────────────────────────────────────────

    /// Delete a volume. Returns `Ok(())` on success; maps 404 → `AppError::NotFound`.
    pub async fn delete(&self, volume_id: &str) -> AppResult<()> {
        let resp = self
            .cubemaster
            .delete_volume(volume_id)
            .await
            .map_err(cm_err_to_app)?;

        resp.ret.into_result().map_err(cm_err_to_app)?;
        Ok(())
    }
}
