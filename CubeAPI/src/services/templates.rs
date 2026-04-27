// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use uuid::Uuid;

use crate::{
    cubemaster::{
        CreateTemplateFromImageReq, CubeMasterClient, CubeMasterError, RedoTemplateReq,
        TemplateDeleteRequest, TemplateJob, TemplateJobResponse,
    },
    error::{AppError, AppResult},
    models::{
        CreateTemplateRequest, RebuildTemplateRequest, TemplateBuildJob, TemplateBuildStatus,
        TemplateDetail, TemplateSummary,
    },
};

#[derive(Clone)]
pub struct TemplateService {
    cubemaster: CubeMasterClient,
    instance_type: String,
}

impl TemplateService {
    pub fn new(cubemaster: CubeMasterClient, instance_type: String) -> Self {
        Self {
            cubemaster,
            instance_type,
        }
    }

    pub async fn list_templates(&self) -> AppResult<Vec<TemplateSummary>> {
        let resp = self
            .cubemaster
            .list_templates(None, false)
            .await
            .map_err(map_err)?;

        Ok(resp
            .data
            .into_iter()
            .map(|s| TemplateSummary {
                template_id: s.template_id,
                instance_type: non_empty(s.instance_type),
                version: non_empty(s.version),
                status: s.status,
                last_error: non_empty(s.last_error),
                created_at: non_empty(s.created_at),
                image_info: non_empty(s.image_info),
            })
            .collect())
    }

    pub async fn get_template(&self, template_id: &str) -> AppResult<TemplateDetail> {
        let resp = self
            .cubemaster
            .get_template(template_id)
            .await
            .map_err(map_err)?;

        if resp.template_id.is_empty() && resp.status.is_empty() {
            return Err(AppError::NotFound(format!(
                "template {} not found",
                template_id
            )));
        }

        Ok(TemplateDetail {
            template_id: string_or(resp.template_id, template_id),
            instance_type: non_empty(resp.instance_type),
            version: non_empty(resp.version),
            status: resp.status,
            last_error: non_empty(resp.last_error),
            replicas: resp.replicas,
            create_request: resp.create_request,
        })
    }

    pub async fn create_template(
        &self,
        body: CreateTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        if body.template_id.trim().is_empty() || body.image.trim().is_empty() {
            return Err(AppError::BadRequest(
                "templateID and image are required".to_string(),
            ));
        }

        let req = CreateTemplateFromImageReq {
            request_id: new_request_id(),
            instance_type: body
                .instance_type
                .unwrap_or_else(|| self.instance_type.clone()),
            template_id: body.template_id,
            image: body.image,
            extra: body.extra,
        };

        let resp = self
            .cubemaster
            .create_template_from_image(&req)
            .await
            .map_err(map_err)?;

        Ok(to_job(resp))
    }

    pub async fn rebuild_template(
        &self,
        template_id: String,
        body: RebuildTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        let req = RedoTemplateReq {
            request_id: new_request_id(),
            template_id,
            extra: body.extra,
        };

        let resp = self.cubemaster.redo_template(&req).await.map_err(map_err)?;

        Ok(to_job(resp))
    }

    pub async fn delete_template(
        &self,
        template_id: String,
        instance_type: Option<String>,
        sync: Option<bool>,
    ) -> AppResult<()> {
        let req = TemplateDeleteRequest {
            request_id: new_request_id(),
            template_id,
            instance_type: instance_type.unwrap_or_else(|| self.instance_type.clone()),
            sync: sync.unwrap_or(false),
        };

        self.cubemaster
            .delete_template(&req)
            .await
            .map_err(map_err)?;

        Ok(())
    }

    pub async fn start_template_build(&self, template_id: String) -> AppResult<TemplateBuildJob> {
        let req = RedoTemplateReq {
            request_id: new_request_id(),
            template_id,
            extra: Default::default(),
        };

        let resp = self.cubemaster.redo_template(&req).await.map_err(map_err)?;

        Ok(to_job(resp))
    }

    pub async fn get_template_build_status(
        &self,
        template_id: &str,
        build_id: &str,
    ) -> AppResult<TemplateBuildStatus> {
        let resp = self
            .cubemaster
            .get_template_build_status(build_id)
            .await
            .map_err(map_err)?;

        Ok(TemplateBuildStatus {
            build_id: string_or(resp.build_id, build_id),
            template_id: string_or(resp.template_id, template_id),
            status: resp.status,
            progress: resp.progress,
            message: resp.message,
        })
    }

    pub async fn get_template_build_logs(&self, build_id: &str) -> AppResult<serde_json::Value> {
        let resp = self
            .cubemaster
            .get_template_build_status(build_id)
            .await
            .map_err(map_err)?;

        let line = build_log_line(&resp.status, resp.progress, &resp.message);

        Ok(serde_json::json!({
            "buildID": build_id,
            "status": resp.status,
            "progress": resp.progress,
            "lines": [line],
        }))
    }
}

fn map_err(e: CubeMasterError) -> AppError {
    if e.is_invalid_path_parameter() {
        AppError::BadRequest(e.to_string())
    } else if e.is_not_found() || e.is_endpoint_missing() {
        AppError::NotFound(e.to_string())
    } else if e.is_conflict() {
        AppError::Conflict(e.to_string())
    } else {
        AppError::Internal(anyhow::anyhow!(e))
    }
}

fn new_request_id() -> String {
    Uuid::new_v4().to_string()
}

fn non_empty(s: String) -> Option<String> {
    if s.trim().is_empty() {
        None
    } else {
        Some(s)
    }
}

fn string_or(value: String, fallback: &str) -> String {
    if value.is_empty() {
        fallback.to_string()
    } else {
        value
    }
}

fn build_log_line(status: &str, progress: i32, message: &str) -> String {
    if message.is_empty() {
        format!("[{}] progress={}%", status, progress)
    } else {
        format!("[{}] {}", status, message)
    }
}

fn to_job(resp: TemplateJobResponse) -> TemplateBuildJob {
    let job = resp.job.unwrap_or_else(default_template_job);
    TemplateBuildJob {
        job_id: job.job_id,
        template_id: job.template_id,
        status: job.status,
        phase: job.phase,
        progress: job.progress,
        error_message: job.error_message,
    }
}

fn default_template_job() -> TemplateJob {
    TemplateJob {
        job_id: String::new(),
        template_id: String::new(),
        status: "accepted".to_string(),
        phase: String::new(),
        progress: 0,
        error_message: String::new(),
        attempt_no: 0,
        retry_of_job_id: String::new(),
    }
}
