// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::{collections::HashMap, time::Duration};

use axum::{extract::Path, extract::State, http::StatusCode, response::IntoResponse, Json};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::{
    error::{AppError, AppResult},
    models::{NewSandbox, RollbackResponse},
    state::AppState,
};

const DEFAULT_DS_OPENCLAW_TEMPLATE: &str = "wecom-ds-openclaw";
const DEFAULT_OPENCLAW_MODEL: &str = "deepseek/deepseek-v4-flash";
const DEFAULT_OPENCLAW_MODEL_LABEL: &str = "DeepSeek V4 Flash";
const DEEPSEEK_V4_PRO_MODEL: &str = "deepseek/deepseek-v4-pro";
const DEEPSEEK_V4_PRO_MODEL_LABEL: &str = "DeepSeek V4 Pro";
const ENVD_PORT: u16 = 49983;
const LOGIN_ENV_PORT: u16 = 8080;
const OPENCLAW_UI_PORT: u16 = 18789;
const CONNECT_JSON: &str = "application/connect+json";

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateAgentInstanceRequest {
    pub name: String,
    pub engine: String,
    pub model: Option<String>,
    #[serde(default)]
    pub template_id: Option<String>,
    #[serde(default)]
    pub bot_id: Option<String>,
    #[serde(default)]
    pub bot_secret: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentInstanceResponse {
    pub id: String,
    pub name: String,
    pub status: String,
    pub engine: String,
    pub env: String,
    pub model: String,
    pub version: String,
    pub bots: Vec<String>,
    pub bots_available: Vec<String>,
    pub avatar: String,
    pub avatar_tone: String,
    pub sandbox_id: String,
    pub template_id: String,
    pub gateway_url: String,
    pub env_url: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub wecom_config: Option<AgentWeComConfig>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub setup: Option<AgentSetupResult>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentWeComConfig {
    pub bot_id: String,
    pub bot_secret: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateWeComConfigRequest {
    pub bot_id: String,
    pub bot_secret: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateAgentModelRequest {
    pub model: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentSetupResult {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateAgentSnapshotRequest {
    pub name: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RollbackAgentRequest {
    pub snapshot_id: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CloneAgentRequest {
    pub name: Option<String>,
    pub snapshot_id: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PublishAgentTemplateRequest {
    pub name: Option<String>,
    pub snapshot_id: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateAgentTemplateRequest {
    pub name: Option<String>,
    pub recommended: Option<bool>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateAgentSnapshotRequest {
    pub name: Option<String>,
    pub is_healthy: Option<bool>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentRecoverResponse {
    pub recovered: bool,
    pub method: String,
    #[serde(rename = "snapshotID", skip_serializing_if = "Option::is_none")]
    pub snapshot_id: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentGatewayHealthResponse {
    pub ready: bool,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PublishAgentTemplateResponse {
    pub template_id: String,
    pub snapshot_id: String,
    pub name: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentTemplateResponse {
    pub template_id: String,
    pub name: String,
    pub source_agent_id: String,
    pub source_snapshot_id: String,
    pub source_sandbox_id: String,
    pub model: String,
    pub version: String,
    pub recommended: bool,
    pub created_at: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentSnapshotResponse {
    #[serde(rename = "snapshotID")]
    pub snapshot_id: String,
    pub names: Vec<String>,
    pub status: String,
    #[serde(rename = "originSandboxID", skip_serializing_if = "Option::is_none")]
    pub origin_sandbox_id: Option<String>,
    #[serde(
        rename = "publishedTemplateId",
        skip_serializing_if = "Option::is_none"
    )]
    pub published_template_id: Option<String>,
    #[serde(rename = "templateReferenced")]
    pub template_referenced: bool,
    #[serde(rename = "isHealthy")]
    pub is_healthy: bool,
    #[serde(rename = "parentSnapshotID", skip_serializing_if = "Option::is_none")]
    pub parent_snapshot_id: Option<String>,
    #[serde(rename = "createdAt", skip_serializing_if = "Option::is_none")]
    pub created_at: Option<String>,
    #[serde(rename = "updatedAt", skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentOperationResponse {
    pub operation_id: String,
    pub agent_id: String,
    pub operation_type: String,
    pub status: String,
    pub target_id: Option<String>,
    pub error_message: Option<String>,
    pub created_at: Option<String>,
    pub updated_at: Option<String>,
}

#[derive(Default)]
struct CommandOutput {
    exit_code: i32,
    stdout: String,
    stderr: String,
}

pub async fn create_agent_instance(
    State(state): State<AppState>,
    Json(body): Json<CreateAgentInstanceRequest>,
) -> AppResult<impl IntoResponse> {
    let name = body.name.trim();
    if name.is_empty() {
        return Err(AppError::BadRequest("agent name is required".to_string()));
    }

    if body.engine != "openclaw" {
        return Err(AppError::BadRequest(
            "only openclaw engine is currently supported".to_string(),
        ));
    }
    let has_bot_id = body.bot_id.as_deref().is_some_and(|v| !v.trim().is_empty());
    let has_bot_secret = body
        .bot_secret
        .as_deref()
        .is_some_and(|v| !v.trim().is_empty());
    if has_bot_id != has_bot_secret {
        return Err(AppError::BadRequest(
            "Bot ID and Secret must be provided together".to_string(),
        ));
    }
    let should_bind_wecom = has_bot_id && has_bot_secret;

    let template_id = body
        .template_id
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToString::to_string)
        .unwrap_or_else(|| {
            std::env::var("AGENTHUB_DS_OPENCLAW_TEMPLATE")
                .unwrap_or_else(|_| DEFAULT_DS_OPENCLAW_TEMPLATE.to_string())
        });
    let timeout = std::env::var("AGENTHUB_SANDBOX_TIMEOUT_SECS")
        .ok()
        .and_then(|v| v.parse::<i32>().ok())
        .unwrap_or(86_400);

    // 若 template_id 命中已发布的助手模板，则克隆出的沙箱已内置 OpenClaw 配置，
    // 创建时可走快路径，跳过重复装配。
    let agent_template = if let Some(store) = &state.agenthub_store {
        store.get_template(&template_id).await.ok().flatten()
    } else {
        None
    };

    let created = state
        .services
        .sandboxes
        .create_sandbox(NewSandbox {
            template_id: template_id.clone(),
            timeout,
            auto_pause: false,
            auto_resume: None,
            secure: None,
            allow_internet_access: Some(true),
            network: None,
            metadata: Some(HashMap::from([
                ("agenthub".to_string(), "true".to_string()),
                ("agenthub.name".to_string(), name.to_string()),
                ("agenthub.engine".to_string(), "openclaw".to_string()),
            ])),
            env_vars: None,
            mcp: None,
            volume_mounts: None,
        })
        .await?;

    let sandbox_id = created.sandbox_id.clone();
    let domain = created
        .domain
        .clone()
        .unwrap_or_else(|| state.config.sandbox_domain.clone());

    // 模板创建快路径：命中助手模板且无需绑定企微时，沙箱已带好 OpenClaw，跳过完整装配。
    let use_template_fastpath = agent_template.is_some() && !should_bind_wecom;

    let default_model: String = body
        .model
        .as_deref()
        .map(str::trim)
        .filter(|m| !m.is_empty())
        .map(ToString::to_string)
        .or_else(|| agent_template.as_ref().map(|t| t.model.clone()))
        .unwrap_or_else(|| DEFAULT_OPENCLAW_MODEL_LABEL.to_string());

    // 快路径不重写配置，沿用快照内置的网关 token（创建后从配置读取）。
    let fixed_gateway_token = if should_bind_wecom || use_template_fastpath {
        None
    } else {
        Some(new_gateway_token())
    };

    let setup = if should_bind_wecom {
        let deepseek_api_key = std::env::var("AGENTHUB_DEEPSEEK_API_KEY")
            .or_else(|_| std::env::var("OPENCLAW_DEEPSEEK_API_KEY"))
            .map_err(|_| {
                AppError::BadRequest(
                    "server env AGENTHUB_DEEPSEEK_API_KEY is required for OpenClaw setup"
                        .to_string(),
                )
            })?;

        match configure_openclaw(
            &state,
            &sandbox_id,
            &domain,
            &deepseek_api_key,
            &body.bot_id,
            &body.bot_secret,
        )
        .await
        {
            Ok(setup) => setup,
            Err(err) => {
                let _ = state.services.sandboxes.kill_sandbox(&sandbox_id).await;
                return Err(err);
            }
        }
    } else if use_template_fastpath {
        // 模板克隆出的沙箱已内置并运行 OpenClaw，无需就绪检查或重新装配。
        AgentSetupResult {
            exit_code: 0,
            stdout: String::new(),
            stderr: String::new(),
        }
    } else {
        let deepseek_api_key = std::env::var("AGENTHUB_DEEPSEEK_API_KEY")
            .or_else(|_| std::env::var("OPENCLAW_DEEPSEEK_API_KEY"))
            .map_err(|_| {
                AppError::BadRequest(
                    "server env AGENTHUB_DEEPSEEK_API_KEY is required for ds-openclaw setup"
                        .to_string(),
                )
            })?;
        let gateway_token = fixed_gateway_token
            .clone()
            .unwrap_or_else(new_gateway_token);

        match configure_ds_openclaw(
            &state,
            &sandbox_id,
            &domain,
            &deepseek_api_key,
            &gateway_token,
        )
        .await
        {
            Ok(setup) => setup,
            Err(err) => {
                let _ = state.services.sandboxes.kill_sandbox(&sandbox_id).await;
                return Err(err);
            }
        }
    };

    let bots = if should_bind_wecom {
        vec!["wecom".to_string()]
    } else {
        vec![]
    };
    let bots_available = ["wecom"]
        .into_iter()
        .filter(|b| !bots.iter().any(|v| v == b))
        .map(ToString::to_string)
        .collect();

    let gateway_token = read_openclaw_gateway_token(&state, &sandbox_id, &domain)
        .await
        .unwrap_or(None)
        .or(fixed_gateway_token);
    let gateway_url = tokenized_gateway_url(
        sandbox_https_url(OPENCLAW_UI_PORT, &sandbox_id, &domain),
        gateway_token.clone(),
    );
    let env_url = sandbox_url(LOGIN_ENV_PORT, &sandbox_id, &domain);

    let response = AgentInstanceResponse {
        id: format!("agent-{}", sandbox_id),
        name: name.to_string(),
        status: "running".to_string(),
        engine: "openclaw".to_string(),
        env: "linux".to_string(),
        model: default_model.to_string(),
        version: "2026.4.5-t.27".to_string(),
        bots,
        bots_available,
        avatar: name.to_string(),
        avatar_tone: "sky".to_string(),
        sandbox_id: sandbox_id.clone(),
        template_id,
        gateway_url,
        env_url,
        wecom_config: if should_bind_wecom {
            match (&body.bot_id, &body.bot_secret) {
                (Some(bot_id), Some(bot_secret)) => Some(AgentWeComConfig {
                    bot_id: bot_id.trim().to_string(),
                    bot_secret: bot_secret.trim().to_string(),
                }),
                _ => None,
            }
        } else {
            None
        },
        setup: Some(setup),
    };

    if let Some(store) = &state.agenthub_store {
        if let Err(err) = store
            .upsert_instance(&response, &domain, gateway_token.as_deref())
            .await
        {
            let _ = state.services.sandboxes.kill_sandbox(&sandbox_id).await;
            return Err(AppError::Internal(anyhow::anyhow!(
                "failed to save AgentHub instance: {}",
                err
            )));
        }
    }

    Ok((StatusCode::CREATED, Json(response)))
}

pub async fn list_agent_instances(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        return Ok((StatusCode::OK, Json(Vec::<AgentInstanceResponse>::new())));
    };
    let instances = store
        .list_instances()
        .await
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!("failed to list AgentHub instances: {}", e))
        })?
        .into_iter()
        .map(|record| record.into_response())
        .collect::<Vec<_>>();
    Ok((StatusCode::OK, Json(instances)))
}

pub async fn get_agent_gateway_health(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let proxy_base = std::env::var("AGENTHUB_SANDBOX_PROXY_URL")
        .unwrap_or_else(|_| "http://127.0.0.1".to_string());
    let url = format!(
        "{}/sandbox/{}/{}/",
        proxy_base.trim_end_matches('/'),
        record.sandbox_id,
        OPENCLAW_UI_PORT
    );
    let ready = match state
        .http_client
        .get(url)
        .timeout(Duration::from_secs(3))
        .send()
        .await
    {
        Ok(resp) => resp.status().is_success(),
        Err(_) => false,
    };

    Ok((StatusCode::OK, Json(AgentGatewayHealthResponse { ready })))
}

pub async fn list_agent_templates(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        return Ok((StatusCode::OK, Json(Vec::<AgentTemplateResponse>::new())));
    };
    let templates = store
        .list_templates()
        .await
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!("failed to list AgentHub templates: {}", e))
        })?
        .into_iter()
        .map(|record| AgentTemplateResponse {
            template_id: record.template_id,
            name: record.name,
            source_agent_id: record.source_agent_id,
            source_snapshot_id: record.source_snapshot_id,
            source_sandbox_id: record.source_sandbox_id,
            model: record.model,
            version: record.version,
            recommended: record.recommended,
            created_at: record.created_at,
        })
        .collect::<Vec<_>>();
    Ok((StatusCode::OK, Json(templates)))
}

pub async fn list_agent_snapshots(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let (items, _) = state
        .services
        .snapshots
        .list(Some(&record.sandbox_id), Some(100), None)
        .await?;
    if let Some(store) = &state.agenthub_store {
        for item in &items {
            let _ = store
                .upsert_snapshot_item(&agent_id, &record.sandbox_id, item)
                .await;
        }
        if let Ok(records) = store.list_snapshots(&agent_id).await {
            let persisted = records
                .into_iter()
                .map(|record| AgentSnapshotResponse {
                    snapshot_id: record.snapshot_id,
                    names: record.name.into_iter().collect(),
                    status: record.status,
                    origin_sandbox_id: record.origin_sandbox_id,
                    published_template_id: record.published_template_id,
                    template_referenced: record.template_referenced,
                    is_healthy: record.is_healthy,
                    parent_snapshot_id: record.parent_snapshot_id,
                    created_at: record.created_at,
                    updated_at: record.updated_at,
                })
                .collect::<Vec<_>>();
            return Ok((StatusCode::OK, Json(persisted)));
        }
    }
    let fallback = items
        .into_iter()
        .map(|item| AgentSnapshotResponse {
            snapshot_id: item.snapshot_id,
            names: item.names,
            status: item.status,
            origin_sandbox_id: item.origin_sandbox_id,
            published_template_id: None,
            template_referenced: false,
            is_healthy: false,
            parent_snapshot_id: None,
            created_at: item.created_at.map(|v| v.to_rfc3339()),
            updated_at: item.updated_at.map(|v| v.to_rfc3339()),
        })
        .collect::<Vec<_>>();
    Ok((StatusCode::OK, Json(fallback)))
}

pub async fn delete_agent_snapshot(
    State(state): State<AppState>,
    Path((agent_id, snapshot_id)): Path<(String, String)>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    if let Some(store) = &state.agenthub_store {
        if let Ok(records) = store.list_snapshots(&agent_id).await {
            if records
                .iter()
                .any(|item| item.snapshot_id == snapshot_id && item.template_referenced)
            {
                return Err(AppError::Conflict(
                    "snapshot is referenced by an assistant template".to_string(),
                ));
            }
        }
    }

    state.services.snapshots.delete(&snapshot_id).await?;
    if let Some(store) = &state.agenthub_store {
        store
            .soft_delete_snapshot(&record.agent_id, &snapshot_id)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to delete AgentHub snapshot: {}", e))
            })?;
    }
    Ok(StatusCode::NO_CONTENT)
}

pub async fn update_agent_snapshot(
    State(state): State<AppState>,
    Path((agent_id, snapshot_id)): Path<(String, String)>,
    Json(body): Json<UpdateAgentSnapshotRequest>,
) -> AppResult<impl IntoResponse> {
    let store = state.agenthub_store.as_ref().ok_or_else(|| {
        AppError::BadRequest("AgentHub database persistence is not configured".to_string())
    })?;

    if let Some(name) = body
        .name
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        store
            .update_snapshot_name(&agent_id, &snapshot_id, name)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to rename AgentHub snapshot: {}", e))
            })?;
    }
    if let Some(is_healthy) = body.is_healthy {
        store
            .set_snapshot_healthy(&agent_id, &snapshot_id, is_healthy)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!(
                    "failed to update AgentHub snapshot health: {}",
                    e
                ))
            })?;
    }

    Ok(StatusCode::NO_CONTENT)
}

pub async fn update_agent_template(
    State(state): State<AppState>,
    Path(template_id): Path<String>,
    Json(body): Json<UpdateAgentTemplateRequest>,
) -> AppResult<impl IntoResponse> {
    let store = state.agenthub_store.as_ref().ok_or_else(|| {
        AppError::BadRequest("AgentHub database persistence is not configured".to_string())
    })?;

    if let Some(name) = body
        .name
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        store
            .update_template_name(&template_id, name)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to update AgentHub template: {}", e))
            })?;
    }
    if let Some(recommended) = body.recommended {
        store
            .set_template_recommended(&template_id, recommended)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to update AgentHub template: {}", e))
            })?;
    }

    Ok(StatusCode::NO_CONTENT)
}

pub async fn delete_agent_template(
    State(state): State<AppState>,
    Path(template_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let store = state.agenthub_store.as_ref().ok_or_else(|| {
        AppError::BadRequest("AgentHub database persistence is not configured".to_string())
    })?;
    store
        .soft_delete_template(&template_id)
        .await
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!("failed to delete AgentHub template: {}", e))
        })?;
    Ok(StatusCode::NO_CONTENT)
}

pub async fn list_agent_operations(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let store = state.agenthub_store.as_ref().ok_or_else(|| {
        AppError::BadRequest("AgentHub database persistence is not configured".to_string())
    })?;
    let records = store.list_operations(&agent_id, 50).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to list AgentHub operations: {}", e))
    })?;
    let items = records
        .into_iter()
        .map(|record| AgentOperationResponse {
            operation_id: record.operation_id,
            agent_id: record.agent_id,
            operation_type: record.operation_type,
            status: record.status,
            target_id: record.target_id,
            error_message: record.error_message,
            created_at: record.created_at,
            updated_at: record.updated_at,
        })
        .collect::<Vec<_>>();
    Ok((StatusCode::OK, Json(items)))
}

pub async fn create_agent_snapshot(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
    Json(body): Json<CreateAgentSnapshotRequest>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let operation_id = start_agent_operation(&state, &record, "snapshot").await;
    let name = body
        .name
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToString::to_string)
        .or_else(|| Some(format!("{} 存档", record.name)));
    // 快照本质是提交 rootfs 的 COW 增量，耗时与数据量成正比、可能较长。
    // 这里改为后台执行并立即返回操作 ID，前端用操作流水（operation）轮询完成状态，
    // 避免请求长时间阻塞、转圈。
    let task_state = state.clone();
    let task_agent = agent_id.clone();
    let task_sandbox = record.sandbox_id.clone();
    let task_operation = operation_id.clone();
    tokio::spawn(async move {
        match task_state
            .services
            .snapshots
            .create(&task_sandbox, name)
            .await
        {
            Ok(info) => {
                if let Some(store) = &task_state.agenthub_store {
                    let parent = store.get_base_snapshot_id(&task_agent).await.ok().flatten();
                    let _ = store
                        .upsert_snapshot_info(&task_agent, &task_sandbox, &info)
                        .await;
                    if let Some(parent) = parent.as_deref() {
                        if parent != info.snapshot_id {
                            let _ = store
                                .set_snapshot_parent(&info.snapshot_id, Some(parent))
                                .await;
                        }
                    }
                    let _ = store
                        .set_base_snapshot_id(&task_agent, &info.snapshot_id)
                        .await;
                }
                finish_agent_operation(
                    &task_state,
                    task_operation.as_deref(),
                    "succeeded",
                    Some(&info.snapshot_id),
                    None,
                )
                .await;
            }
            Err(err) => {
                finish_agent_operation(
                    &task_state,
                    task_operation.as_deref(),
                    "failed",
                    None,
                    Some(&err.to_string()),
                )
                .await;
            }
        }
    });

    Ok((
        StatusCode::ACCEPTED,
        Json(serde_json::json!({
            "operationId": operation_id,
            "status": "running",
        })),
    ))
}

pub async fn rollback_agent_to_snapshot(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
    Json(body): Json<RollbackAgentRequest>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let snapshot_id = body.snapshot_id.trim();
    if snapshot_id.is_empty() {
        return Err(AppError::BadRequest("snapshotId is required".to_string()));
    }
    let snapshot_id = snapshot_id.to_string();

    let result: AppResult<RollbackResponse> = state
        .services
        .snapshots
        .rollback(&record.sandbox_id, &snapshot_id)
        .await;

    match result {
        Ok(resp) => {
            let task_state = state.clone();
            let task_record = record;
            let task_agent = agent_id;
            let task_snapshot = snapshot_id;
            tokio::spawn(async move {
                let operation_id =
                    start_agent_operation(&task_state, &task_record, "rollback").await;
                if let Some(store) = &task_state.agenthub_store {
                    let _ = store
                        .set_base_snapshot_id(&task_agent, &task_snapshot)
                        .await;
                    let _ = store
                        .set_snapshot_healthy(&task_agent, &task_snapshot, true)
                        .await;
                }
                finish_agent_operation(
                    &task_state,
                    operation_id.as_deref(),
                    "succeeded",
                    Some(&task_snapshot),
                    None,
                )
                .await;
            });
            Ok((StatusCode::OK, Json(resp)))
        }
        Err(err) => {
            let error_message = err.to_string();
            let task_state = state.clone();
            let task_record = record;
            let task_snapshot = snapshot_id;
            tokio::spawn(async move {
                let operation_id =
                    start_agent_operation(&task_state, &task_record, "rollback").await;
                finish_agent_operation(
                    &task_state,
                    operation_id.as_deref(),
                    "failed",
                    Some(&task_snapshot),
                    Some(&error_message),
                )
                .await;
            });
            Err(err)
        }
    }
}

pub async fn clone_agent_instance(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
    Json(body): Json<CloneAgentRequest>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let operation_id = start_agent_operation(&state, &record, "clone").await;
    let snapshot_id = match body
        .snapshot_id
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        Some(snapshot_id) => snapshot_id.to_string(),
        None => {
            let snapshot = state
                .services
                .snapshots
                .create(&record.sandbox_id, Some(format!("{} 分身源", record.name)))
                .await
                .map_err(|err| {
                    finish_agent_operation_blocking(
                        &state,
                        operation_id.as_deref(),
                        "failed",
                        None,
                        Some(&err.to_string()),
                    );
                    err
                })?;
            if let Some(store) = &state.agenthub_store {
                let _ = store
                    .upsert_snapshot_info(&agent_id, &record.sandbox_id, &snapshot)
                    .await;
            }
            snapshot.snapshot_id
        }
    };

    let clone_name = body
        .name
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToString::to_string)
        .unwrap_or_else(|| format!("{} 临时助手", record.name));
    let timeout = std::env::var("AGENTHUB_SANDBOX_TIMEOUT_SECS")
        .ok()
        .and_then(|v| v.parse::<i32>().ok())
        .unwrap_or(86_400);
    let created = state
        .services
        .sandboxes
        .create_sandbox(NewSandbox {
            template_id: snapshot_id.clone(),
            timeout,
            auto_pause: false,
            auto_resume: None,
            secure: None,
            allow_internet_access: Some(true),
            network: None,
            metadata: Some(HashMap::from([
                ("agenthub".to_string(), "true".to_string()),
                ("agenthub.name".to_string(), clone_name.clone()),
                ("agenthub.engine".to_string(), record.engine.clone()),
                (
                    "agenthub.clone.source".to_string(),
                    record.sandbox_id.clone(),
                ),
            ])),
            env_vars: None,
            mcp: None,
            volume_mounts: None,
        })
        .await
        .map_err(|err| {
            finish_agent_operation_blocking(
                &state,
                operation_id.as_deref(),
                "failed",
                Some(&snapshot_id),
                Some(&err.to_string()),
            );
            err
        })?;

    let sandbox_id = created.sandbox_id.clone();
    let domain = created
        .domain
        .clone()
        .unwrap_or_else(|| state.config.sandbox_domain.clone());
    let gateway_token = record.gateway_token.clone();
    let gateway_url = tokenized_gateway_url(
        sandbox_https_url(OPENCLAW_UI_PORT, &sandbox_id, &domain),
        gateway_token.clone(),
    );
    let env_url = sandbox_url(LOGIN_ENV_PORT, &sandbox_id, &domain);
    let bots_available = ["wecom"]
        .into_iter()
        .filter(|b| !record.bots.iter().any(|v| v == b))
        .map(ToString::to_string)
        .collect();
    let response = AgentInstanceResponse {
        id: format!("agent-{}", sandbox_id),
        name: clone_name,
        status: "running".to_string(),
        engine: record.engine.clone(),
        env: record.env.clone(),
        model: record.model.clone(),
        version: record.version.clone(),
        bots: record.bots.clone(),
        bots_available,
        avatar: record.avatar.clone(),
        avatar_tone: record.avatar_tone.clone(),
        sandbox_id: sandbox_id.clone(),
        template_id: snapshot_id,
        gateway_url,
        env_url,
        wecom_config: match (record.wecom_bot_id.clone(), record.wecom_bot_secret.clone()) {
            (Some(bot_id), Some(bot_secret)) => Some(AgentWeComConfig { bot_id, bot_secret }),
            _ => None,
        },
        setup: None,
    };

    if let Some(store) = &state.agenthub_store {
        store
            .upsert_instance(&response, &domain, gateway_token.as_deref())
            .await
            .map_err(|e| {
                finish_agent_operation_blocking(
                    &state,
                    operation_id.as_deref(),
                    "failed",
                    Some(&sandbox_id),
                    Some(&e.to_string()),
                );
                AppError::Internal(anyhow::anyhow!(
                    "failed to save cloned AgentHub instance: {}",
                    e
                ))
            })?;
    }

    finish_agent_operation(
        &state,
        operation_id.as_deref(),
        "succeeded",
        Some(&sandbox_id),
        None,
    )
    .await;
    Ok((StatusCode::CREATED, Json(response)))
}

pub async fn publish_agent_template(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
    Json(body): Json<PublishAgentTemplateRequest>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let operation_id = start_agent_operation(&state, &record, "publish_template").await;
    let snapshot_id = match body
        .snapshot_id
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        Some(snapshot_id) => snapshot_id.to_string(),
        None => {
            // 直接对当前状态打快照后发布：不脱敏、不还原。
            // 模板保留可用配置，配合「从模板秒级孵化」克隆即用，无需重新装配。
            let snapshot = state
                .services
                .snapshots
                .create(&record.sandbox_id, body.name.clone())
                .await
                .map_err(|err| {
                    finish_agent_operation_blocking(
                        &state,
                        operation_id.as_deref(),
                        "failed",
                        None,
                        Some(&err.to_string()),
                    );
                    err
                })?;
            if let Some(store) = &state.agenthub_store {
                let _ = store
                    .upsert_snapshot_info(&agent_id, &record.sandbox_id, &snapshot)
                    .await;
            }
            snapshot.snapshot_id
        }
    };
    let template_id = snapshot_id.clone();
    let name = body
        .name
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToString::to_string)
        .unwrap_or_else(|| format!("{} 模板", record.name));
    if let Some(store) = &state.agenthub_store {
        store
            .publish_template(&template_id, &name, &record, &snapshot_id)
            .await
            .map_err(|e| {
                finish_agent_operation_blocking(
                    &state,
                    operation_id.as_deref(),
                    "failed",
                    Some(&snapshot_id),
                    Some(&e.to_string()),
                );
                AppError::Internal(anyhow::anyhow!(
                    "failed to publish AgentHub template: {}",
                    e
                ))
            })?;
    }
    finish_agent_operation(
        &state,
        operation_id.as_deref(),
        "succeeded",
        Some(&template_id),
        None,
    )
    .await;
    Ok((
        StatusCode::OK,
        Json(PublishAgentTemplateResponse {
            template_id,
            snapshot_id,
            name: Some(name),
        }),
    ))
}

pub async fn delete_agent_instance(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    if let Some(record) = store.get_instance(&agent_id).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to read AgentHub instance: {}", e))
    })? {
        let _ = state
            .services
            .sandboxes
            .kill_sandbox(&record.sandbox_id)
            .await;
        store.soft_delete_instance(&agent_id).await.map_err(|e| {
            AppError::Internal(anyhow::anyhow!("failed to delete AgentHub instance: {}", e))
        })?;
    }

    Ok(StatusCode::NO_CONTENT)
}

pub async fn restart_agent_openclaw(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    let Some(record) = store.get_instance(&agent_id).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to read AgentHub instance: {}", e))
    })?
    else {
        return Err(AppError::BadRequest(format!(
            "AgentHub instance {} not found",
            agent_id
        )));
    };

    let output = restart_openclaw_for_record(&state, &record).await?;
    Ok((
        StatusCode::OK,
        Json(AgentSetupResult {
            exit_code: output.exit_code,
            stdout: output.stdout,
            stderr: output.stderr,
        }),
    ))
}

/// Crash auto-recovery: attempt to bring OpenClaw back to a healthy state.
///
/// First tries a plain restart; if OpenClaw still does not come up healthy,
/// rolls back to the most recent snapshot marked as healthy and restarts again.
pub async fn recover_agent_openclaw(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let operation_id = start_agent_operation(&state, &record, "recover").await;

    // Step 1: a plain restart is enough for transient failures.
    if restart_openclaw_for_record(&state, &record).await.is_ok() {
        if let Some(store) = &state.agenthub_store {
            let _ = store.update_status(&agent_id, "running").await;
        }
        finish_agent_operation(&state, operation_id.as_deref(), "succeeded", None, None).await;
        return Ok((
            StatusCode::OK,
            Json(AgentRecoverResponse {
                recovered: true,
                method: "restart".to_string(),
                snapshot_id: None,
            }),
        ));
    }

    // Step 2: restart failed — roll back to the latest known-healthy snapshot.
    let Some(store) = &state.agenthub_store else {
        finish_agent_operation_blocking(
            &state,
            operation_id.as_deref(),
            "failed",
            None,
            Some("OpenClaw restart failed and persistence is not configured"),
        );
        return Err(AppError::Internal(anyhow::anyhow!(
            "OpenClaw restart failed and no healthy snapshot is available"
        )));
    };

    let healthy = store
        .latest_healthy_snapshot(&agent_id)
        .await
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!("failed to look up healthy snapshot: {}", e))
        })?;
    let Some(snapshot_id) = healthy else {
        let _ = store.update_status(&agent_id, "error").await;
        finish_agent_operation(
            &state,
            operation_id.as_deref(),
            "failed",
            None,
            Some("no healthy snapshot available"),
        )
        .await;
        return Err(AppError::Conflict(
            "OpenClaw is unhealthy and no healthy snapshot is available to recover from"
                .to_string(),
        ));
    };

    let _resp: RollbackResponse = state
        .services
        .snapshots
        .rollback(&record.sandbox_id, &snapshot_id)
        .await
        .map_err(|err| {
            finish_agent_operation_blocking(
                &state,
                operation_id.as_deref(),
                "failed",
                Some(&snapshot_id),
                Some(&err.to_string()),
            );
            err
        })?;

    if let Err(err) = restart_openclaw_for_record(&state, &record).await {
        let _ = store.update_status(&agent_id, "error").await;
        finish_agent_operation(
            &state,
            operation_id.as_deref(),
            "failed",
            Some(&snapshot_id),
            Some(&err.to_string()),
        )
        .await;
        return Err(err);
    }

    let _ = store.set_base_snapshot_id(&agent_id, &snapshot_id).await;
    let _ = store.update_status(&agent_id, "running").await;
    finish_agent_operation(
        &state,
        operation_id.as_deref(),
        "succeeded",
        Some(&snapshot_id),
        None,
    )
    .await;
    Ok((
        StatusCode::OK,
        Json(AgentRecoverResponse {
            recovered: true,
            method: "rollback".to_string(),
            snapshot_id: Some(snapshot_id),
        }),
    ))
}

async fn restart_openclaw_for_record(
    state: &AppState,
    record: &crate::db::AgentHubInstanceRecord,
) -> AppResult<CommandOutput> {
    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": [
                "-l",
                "-c",
                "supervisorctl restart openclaw && supervisorctl status openclaw | grep -q RUNNING && supervisorctl status openclaw"
            ],
            "envs": {},
            "cwd": "/root"
        },
        "stdin": false
    });

    let output = run_envd_command(state, &record.sandbox_id, &record.domain, req).await?;
    if output.exit_code != 0 {
        return Err(AppError::Internal(anyhow::anyhow!(
            "OpenClaw restart failed with exit code {}: {}",
            output.exit_code,
            output.stderr
        )));
    }

    Ok(output)
}

async fn start_agent_operation(
    state: &AppState,
    record: &crate::db::AgentHubInstanceRecord,
    operation_type: &str,
) -> Option<String> {
    let store = state.agenthub_store.as_ref()?;
    match store
        .create_operation(&record.agent_id, &record.sandbox_id, operation_type)
        .await
    {
        Ok(operation_id) => Some(operation_id),
        Err(err) => {
            tracing::warn!(
                "failed to create AgentHub operation {} for {}: {}",
                operation_type,
                record.agent_id,
                err
            );
            None
        }
    }
}

async fn finish_agent_operation(
    state: &AppState,
    operation_id: Option<&str>,
    status: &str,
    target_id: Option<&str>,
    error_message: Option<&str>,
) {
    let (Some(store), Some(operation_id)) = (&state.agenthub_store, operation_id) else {
        return;
    };
    if let Err(err) = store
        .finish_operation(operation_id, status, target_id, error_message)
        .await
    {
        tracing::warn!(
            "failed to finish AgentHub operation {}: {}",
            operation_id,
            err
        );
    }
}

fn finish_agent_operation_blocking(
    state: &AppState,
    operation_id: Option<&str>,
    status: &'static str,
    target_id: Option<&str>,
    error_message: Option<&str>,
) {
    let Some(store) = state.agenthub_store.clone() else {
        return;
    };
    let Some(operation_id) = operation_id.map(ToString::to_string) else {
        return;
    };
    let target_id = target_id.map(ToString::to_string);
    let error_message = error_message.map(ToString::to_string);
    tokio::spawn(async move {
        let _ = store
            .finish_operation(
                &operation_id,
                status,
                target_id.as_deref(),
                error_message.as_deref(),
            )
            .await;
    });
}

pub async fn pause_agent_openclaw(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    state
        .services
        .sandboxes
        .pause_sandbox(&record.sandbox_id)
        .await?;
    let store = state.agenthub_store.as_ref().ok_or_else(|| {
        AppError::BadRequest("AgentHub database persistence is not configured".to_string())
    })?;
    let updated = store
        .update_status(&agent_id, "stopped")
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to update status: {}", e)))?
        .ok_or_else(|| AppError::BadRequest(format!("AgentHub instance {} not found", agent_id)))?;
    Ok((StatusCode::OK, Json(updated.into_response())))
}

pub async fn resume_agent_openclaw(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let record = read_agenthub_instance(&state, &agent_id).await?;
    let timeout = std::env::var("AGENTHUB_SANDBOX_TIMEOUT_SECS")
        .ok()
        .and_then(|v| v.parse::<i32>().ok())
        .unwrap_or(86_400);
    state
        .services
        .sandboxes
        .connect_sandbox(&record.sandbox_id, timeout)
        .await?;
    let store = state.agenthub_store.as_ref().ok_or_else(|| {
        AppError::BadRequest("AgentHub database persistence is not configured".to_string())
    })?;
    let updated = store
        .update_status(&agent_id, "running")
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to update status: {}", e)))?
        .ok_or_else(|| AppError::BadRequest(format!("AgentHub instance {} not found", agent_id)))?;
    Ok((StatusCode::OK, Json(updated.into_response())))
}

pub async fn upgrade_agent_openclaw(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    let Some(record) = store.get_instance(&agent_id).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to read AgentHub instance: {}", e))
    })?
    else {
        return Err(AppError::BadRequest(format!(
            "AgentHub instance {} not found",
            agent_id
        )));
    };

    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": [
                "-l",
                "-c",
                r#"set -e
upgraded=0
openclaw_bin="$(command -v openclaw || true)"

if command -v npm >/dev/null 2>&1; then
  npm_json="$(npm ls -g --depth=0 --json 2>/dev/null || true)"
  npm_packages="$(printf '%s' "$npm_json" | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    data = {}
for name in (data.get("dependencies") or {}):
    if "openclaw" in name.lower():
        print(name)
' || true)"
  if [ -n "$npm_packages" ]; then
    for pkg in $npm_packages; do
      npm install -g "${pkg}@latest"
      upgraded=1
    done
  fi
fi

if [ "$upgraded" != "1" ] && command -v pnpm >/dev/null 2>&1; then
  pnpm_root="$(pnpm root -g 2>/dev/null || true)"
  if [ -n "$pnpm_root" ]; then
    for pkg_dir in "$pnpm_root"/*openclaw* "$pnpm_root"/@*/*openclaw*; do
      [ -e "$pkg_dir/package.json" ] || continue
      pkg="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("name",""))' "$pkg_dir/package.json")"
      [ -n "$pkg" ] || continue
      pnpm add -g "${pkg}@latest"
      upgraded=1
    done
  fi
fi

if [ "$upgraded" != "1" ]; then
  if python3 -m pip show openclaw >/dev/null 2>&1; then
    python3 -m pip install -U openclaw
    upgraded=1
  elif command -v pip3 >/dev/null 2>&1 && pip3 show openclaw >/dev/null 2>&1; then
    pip3 install -U openclaw
    upgraded=1
  elif command -v pip >/dev/null 2>&1 && pip show openclaw >/dev/null 2>&1; then
    pip install -U openclaw
    upgraded=1
  elif command -v uv >/dev/null 2>&1 && uv pip show openclaw >/dev/null 2>&1; then
    uv pip install -U openclaw
    upgraded=1
  fi
fi

if [ "$upgraded" != "1" ]; then
  echo "OpenClaw upgrade source was not detected; refreshing existing OpenClaw service." >&2
fi
supervisorctl restart openclaw
supervisorctl status openclaw | grep -q RUNNING
supervisorctl status openclaw
[ -n "$openclaw_bin" ] && "$openclaw_bin" --version || true"#
            ],
            "envs": {},
            "cwd": "/root"
        },
        "stdin": false
    });

    let output = run_envd_command(&state, &record.sandbox_id, &record.domain, req).await?;
    if output.exit_code != 0 {
        return Err(AppError::Internal(anyhow::anyhow!(
            "OpenClaw upgrade failed with exit code {}: {}",
            output.exit_code,
            output.stderr
        )));
    }

    Ok((
        StatusCode::OK,
        Json(AgentSetupResult {
            exit_code: output.exit_code,
            stdout: output.stdout,
            stderr: output.stderr,
        }),
    ))
}

async fn read_agenthub_instance(
    state: &AppState,
    agent_id: &str,
) -> AppResult<crate::db::AgentHubInstanceRecord> {
    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    let Some(record) = store.get_instance(agent_id).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to read AgentHub instance: {}", e))
    })?
    else {
        return Err(AppError::BadRequest(format!(
            "AgentHub instance {} not found",
            agent_id
        )));
    };
    Ok(record)
}

pub async fn update_agent_model(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
    Json(body): Json<UpdateAgentModelRequest>,
) -> AppResult<impl IntoResponse> {
    let requested_model = body.model.trim();
    let Some((model_label, model_id)) = normalize_agent_model(requested_model) else {
        return Err(AppError::BadRequest(format!(
            "unsupported AgentHub model: {}",
            requested_model
        )));
    };

    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    let Some(record) = store.get_instance(&agent_id).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to read AgentHub instance: {}", e))
    })?
    else {
        return Err(AppError::BadRequest(format!(
            "AgentHub instance {} not found",
            agent_id
        )));
    };

    let setup =
        configure_openclaw_model(&state, &record.sandbox_id, &record.domain, model_id).await?;
    let updated = store
        .update_model(&agent_id, model_label, &setup)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to update model: {}", e)))?
        .ok_or_else(|| AppError::BadRequest(format!("AgentHub instance {} not found", agent_id)))?;

    Ok((StatusCode::OK, Json(updated.into_response())))
}

pub async fn update_agent_wecom_config(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
    Json(body): Json<UpdateWeComConfigRequest>,
) -> AppResult<impl IntoResponse> {
    let bot_id = body.bot_id.trim();
    let bot_secret = body.bot_secret.trim();
    if bot_id.is_empty() || bot_secret.is_empty() {
        return Err(AppError::BadRequest(
            "Bot ID and Secret must be provided together".to_string(),
        ));
    }

    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    let Some(record) = store.get_instance(&agent_id).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to read AgentHub instance: {}", e))
    })?
    else {
        return Err(AppError::BadRequest(format!(
            "AgentHub instance {} not found",
            agent_id
        )));
    };

    let deepseek_api_key = std::env::var("AGENTHUB_DEEPSEEK_API_KEY")
        .or_else(|_| std::env::var("OPENCLAW_DEEPSEEK_API_KEY"))
        .map_err(|_| {
            AppError::BadRequest(
                "server env AGENTHUB_DEEPSEEK_API_KEY is required for OpenClaw setup".to_string(),
            )
        })?;

    let bot_id_value = Some(bot_id.to_string());
    let bot_secret_value = Some(bot_secret.to_string());
    let setup = configure_openclaw(
        &state,
        &record.sandbox_id,
        &record.domain,
        &deepseek_api_key,
        &bot_id_value,
        &bot_secret_value,
    )
    .await?;
    let gateway_token = read_openclaw_gateway_token(&state, &record.sandbox_id, &record.domain)
        .await
        .unwrap_or(None);

    let updated = store
        .update_wecom_config(
            &agent_id,
            bot_id,
            bot_secret,
            gateway_token.as_deref(),
            &setup,
        )
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to update WeCom config: {}", e)))?
        .ok_or_else(|| AppError::BadRequest(format!("AgentHub instance {} not found", agent_id)))?;

    Ok((StatusCode::OK, Json(updated.into_response())))
}

pub async fn get_agent_wecom_config(
    State(state): State<AppState>,
    Path(agent_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    let Some(record) = store.get_instance(&agent_id).await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed to read AgentHub instance: {}", e))
    })?
    else {
        return Err(AppError::BadRequest(format!(
            "AgentHub instance {} not found",
            agent_id
        )));
    };

    let config = match (record.wecom_bot_id.clone(), record.wecom_bot_secret.clone()) {
        (Some(bot_id), Some(bot_secret)) if !bot_id.is_empty() && !bot_secret.is_empty() => {
            Some(AgentWeComConfig { bot_id, bot_secret })
        }
        _ => read_openclaw_wecom_config(&state, &record.sandbox_id, &record.domain)
            .await
            .unwrap_or(None),
    };

    Ok((StatusCode::OK, Json(config)))
}

async fn configure_openclaw(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    deepseek_api_key: &str,
    bot_id: &Option<String>,
    bot_secret: &Option<String>,
) -> AppResult<AgentSetupResult> {
    let mut envs = serde_json::Map::from_iter([
        (
            "OPENCLAW_DEEPSEEK_API_KEY".to_string(),
            Value::String(deepseek_api_key.to_string()),
        ),
        (
            "OPENCLAW_DEFAULT_MODEL".to_string(),
            Value::String(DEFAULT_OPENCLAW_MODEL.to_string()),
        ),
        (
            "OPENCLAW_ALLOWED_ORIGINS".to_string(),
            Value::String("*".to_string()),
        ),
    ]);
    if let Some(v) = bot_id.as_deref().filter(|v| !v.trim().is_empty()) {
        envs.insert("OPENCLAW_BOT_ID".to_string(), Value::String(v.to_string()));
    }
    if let Some(v) = bot_secret.as_deref().filter(|v| !v.trim().is_empty()) {
        envs.insert(
            "OPENCLAW_BOT_SECRET".to_string(),
            Value::String(v.to_string()),
        );
    }

    let setup_script = openclaw_setup_script(
        bot_id.as_deref().is_some_and(|v| !v.trim().is_empty())
            && bot_secret.as_deref().is_some_and(|v| !v.trim().is_empty()),
    );

    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": ["-l", "-c", setup_script],
            "envs": envs,
            "cwd": "/root"
        },
        "stdin": false
    });

    let mut output = run_envd_command(state, sandbox_id, domain, req.clone()).await?;
    for _ in 0..2 {
        if output.exit_code == 0 || !is_openclaw_config_conflict(&output) {
            break;
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
        output = run_envd_command(state, sandbox_id, domain, req.clone()).await?;
    }
    if output.exit_code != 0 {
        return Err(AppError::Internal(anyhow::anyhow!(
            "OpenClaw setup failed with exit code {}: {}",
            output.exit_code,
            output.stderr
        )));
    }

    Ok(AgentSetupResult {
        exit_code: output.exit_code,
        stdout: output.stdout,
        stderr: output.stderr,
    })
}

async fn configure_openclaw_model(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    model: &str,
) -> AppResult<AgentSetupResult> {
    let model_name = model_display_name(model);
    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": [
                "-l",
                "-c",
                r#"python3 - <<'PY'
import json, os
from pathlib import Path

model = os.environ["OPENCLAW_DEFAULT_MODEL"]
model_name = os.environ["OPENCLAW_DEFAULT_MODEL_NAME"]
model_id = model.split("/", 1)[-1]
config_path = Path("/root/.openclaw/openclaw.json")
agent_dir = Path("/root/.openclaw/agents/main/agent")

data = json.loads(config_path.read_text())
agents = data.setdefault("agents", {}).setdefault("defaults", {})
agents["model"] = {"primary": model}
agents["models"] = {model: {"alias": "DeepSeek"}}
models = data.setdefault("models", {})
if not isinstance(models, dict):
    models = {}
    data["models"] = models
models["mode"] = models.get("mode") or "merge"
models.pop("deepseek", None)
providers = models.setdefault("providers", {})
deepseek = providers.setdefault("deepseek", {})
deepseek.update({
    "baseUrl": "https://api.deepseek.com",
    "api": "openai-completions",
})
deepseek["models"] = [{
    "id": model_id,
    "name": model_name,
    "reasoning": True,
    "input": ["text"],
    "contextWindow": 1000000,
    "maxTokens": 384000,
    "compat": {
        "supportsReasoningEffort": True,
        "supportsUsageInStreaming": True,
        "maxTokensField": "max_tokens",
    },
    "api": "openai-completions",
}]
config_path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n")
agent_dir.mkdir(parents=True, exist_ok=True)
(agent_dir / "models.json").write_text(json.dumps(models, ensure_ascii=False, indent=2) + "\n")

supervisor_conf = Path("/opt/gem/supervisord/openclaw.conf")
if supervisor_conf.exists():
    lines = supervisor_conf.read_text().splitlines()
    env_line = f'environment=NODE_ENV="production",OPENCLAW_DEFAULT_MODEL="{model}"'
    for idx, line in enumerate(lines):
        if line.startswith("environment="):
            lines[idx] = env_line
            break
    else:
        lines.append(env_line)
    supervisor_conf.write_text("\n".join(lines) + "\n")
PY
supervisorctl reread || true
supervisorctl update openclaw || true
supervisorctl restart openclaw
supervisorctl status openclaw | grep -q RUNNING
supervisorctl status openclaw"#
            ],
            "envs": {
                "OPENCLAW_DEFAULT_MODEL": model,
                "OPENCLAW_DEFAULT_MODEL_NAME": model_name,
            },
            "cwd": "/root"
        },
        "stdin": false
    });

    let output = run_envd_command(state, sandbox_id, domain, req).await?;
    if output.exit_code != 0 {
        return Err(AppError::Internal(anyhow::anyhow!(
            "OpenClaw model update failed with exit code {}: {}",
            output.exit_code,
            output.stderr
        )));
    }

    Ok(AgentSetupResult {
        exit_code: output.exit_code,
        stdout: output.stdout,
        stderr: output.stderr,
    })
}

async fn configure_ds_openclaw(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    deepseek_api_key: &str,
    gateway_token: &str,
) -> AppResult<AgentSetupResult> {
    let envs = serde_json::Map::from_iter([
        (
            "OPENCLAW_DEEPSEEK_API_KEY".to_string(),
            Value::String(deepseek_api_key.to_string()),
        ),
        (
            "OPENCLAW_DEFAULT_MODEL".to_string(),
            Value::String(DEFAULT_OPENCLAW_MODEL.to_string()),
        ),
        (
            "OPENCLAW_ALLOWED_ORIGINS".to_string(),
            Value::String("*".to_string()),
        ),
        (
            "OPENCLAW_GATEWAY_TOKEN".to_string(),
            Value::String(gateway_token.to_string()),
        ),
    ]);

    let setup_script = openclaw_setup_script(false);

    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": ["-l", "-c", setup_script],
            "envs": envs,
            "cwd": "/root"
        },
        "stdin": false
    });

    let mut output = run_envd_command(state, sandbox_id, domain, req.clone()).await?;
    for _ in 0..2 {
        if output.exit_code == 0 || !is_openclaw_config_conflict(&output) {
            break;
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
        output = run_envd_command(state, sandbox_id, domain, req.clone()).await?;
    }

    if output.exit_code != 0 {
        return Err(AppError::Internal(anyhow::anyhow!(
            "ds-openclaw async setup failed with exit code {}: {}",
            output.exit_code,
            output.stderr
        )));
    }

    Ok(AgentSetupResult {
        exit_code: output.exit_code,
        stdout: output.stdout,
        stderr: output.stderr,
    })
}

async fn read_openclaw_wecom_config(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
) -> AppResult<Option<AgentWeComConfig>> {
    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": [
                "-l",
                "-c",
                "python3 - <<'PY'\nimport json\ntry:\n    channels = json.load(open('/root/.openclaw/openclaw.json')).get('channels', {})\n    wecom = channels.get('wecom') or {}\n    bot_id = wecom.get('botId') or ''\n    secret = wecom.get('secret') or ''\n    if bot_id and secret:\n        print(json.dumps({'botId': bot_id, 'botSecret': secret}))\nexcept Exception:\n    pass\nPY"
            ],
            "envs": {},
            "cwd": "/root"
        },
        "stdin": false
    });

    let output = run_envd_command(state, sandbox_id, domain, req).await?;
    if output.exit_code != 0 {
        return Ok(None);
    }

    let Some(line) = output.stdout.lines().map(str::trim).find(|v| !v.is_empty()) else {
        return Ok(None);
    };

    Ok(serde_json::from_str::<AgentWeComConfig>(line).ok())
}

async fn read_openclaw_gateway_token(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
) -> AppResult<Option<String>> {
    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": [
                "-l",
                "-c",
                "python3 - <<'PY'\nimport json\ntry:\n    token = json.load(open('/root/.openclaw/openclaw.json')).get('gateway', {}).get('auth', {}).get('token')\n    if token:\n        print(token)\nexcept Exception:\n    pass\nPY"
            ],
            "envs": {},
            "cwd": "/root"
        },
        "stdin": false
    });

    let output = run_envd_command(state, sandbox_id, domain, req).await?;
    if output.exit_code != 0 {
        return Ok(None);
    }

    Ok(output
        .stdout
        .lines()
        .map(str::trim)
        .find(|v| !v.is_empty())
        .map(ToString::to_string))
}

fn new_gateway_token() -> String {
    uuid::Uuid::new_v4().simple().to_string()
}

fn normalize_agent_model(model: &str) -> Option<(&'static str, &'static str)> {
    match model {
        DEFAULT_OPENCLAW_MODEL | DEFAULT_OPENCLAW_MODEL_LABEL => {
            Some((DEFAULT_OPENCLAW_MODEL_LABEL, DEFAULT_OPENCLAW_MODEL))
        }
        DEEPSEEK_V4_PRO_MODEL | DEEPSEEK_V4_PRO_MODEL_LABEL => {
            Some((DEEPSEEK_V4_PRO_MODEL_LABEL, DEEPSEEK_V4_PRO_MODEL))
        }
        _ => None,
    }
}

fn model_display_name(model: &str) -> &'static str {
    match model {
        DEEPSEEK_V4_PRO_MODEL => DEEPSEEK_V4_PRO_MODEL_LABEL,
        _ => DEFAULT_OPENCLAW_MODEL_LABEL,
    }
}

fn openclaw_setup_script(configure_wecom: bool) -> String {
    let script = r#"(supervisorctl stop openclaw || true) && \
         gateway_token="${OPENCLAW_GATEWAY_TOKEN:-$(openssl rand -hex 16)}" && \
         export OPENCLAW_GATEWAY_TOKEN="$gateway_token" && \
         cat >/tmp/agenthub-openclaw-setup.py <<'PY'
import json, os
from datetime import datetime, timezone
from pathlib import Path

configure_wecom = __CONFIGURE_WECOM__
model = os.environ.get("OPENCLAW_DEFAULT_MODEL", "deepseek/deepseek-v4-flash")
model_id = model.split("/", 1)[-1]
token = os.environ["OPENCLAW_GATEWAY_TOKEN"]
config_path = Path("/root/.openclaw/openclaw.json")
workspace = Path("/root/.openclaw/workspace")
sessions = Path("/root/.openclaw/agents/main/sessions")
agent_dir = Path("/root/.openclaw/agents/main/agent")

workspace.mkdir(parents=True, exist_ok=True)
sessions.mkdir(parents=True, exist_ok=True)
agent_dir.mkdir(parents=True, exist_ok=True)
config_path.parent.mkdir(parents=True, exist_ok=True)

try:
    data = json.loads(config_path.read_text())
except Exception:
    data = {}

gateway = data.setdefault("gateway", {})
gateway["bind"] = os.environ.get("OPENCLAW_BIND", "auto")
gateway["port"] = int(os.environ.get("OPENCLAW_PORT", "18789"))
gateway["mode"] = "local"
gateway["tailscale"] = {"mode": "off", "resetOnExit": False}
gateway["auth"] = {"mode": "token", "token": token}
origins = os.environ.get("OPENCLAW_ALLOWED_ORIGINS", "*")
gateway["controlUi"] = {
    "allowedOrigins": [o.strip() for o in origins.split(",") if o.strip()],
    "dangerouslyDisableDeviceAuth": os.environ.get("OPENCLAW_DISABLE_DEVICE_AUTH", "true").lower() == "true",
    "allowInsecureAuth": os.environ.get("OPENCLAW_ALLOW_INSECURE_AUTH", "true").lower() == "true",
    "dangerouslyAllowHostHeaderOriginFallback": os.environ.get("OPENCLAW_ALLOW_HOST_HEADER_ORIGIN_FALLBACK", "true").lower() == "true",
}

agents = data.setdefault("agents", {}).setdefault("defaults", {})
agents["model"] = {"primary": model}
agents["models"] = {model: {"alias": "DeepSeek"}}
agents["workspace"] = str(workspace)

data["session"] = {"dmScope": "per-channel-peer"}
tools = data.setdefault("tools", {})
tools["profile"] = "full"
plugins = data.setdefault("plugins", {}).setdefault("entries", {})
plugins["deepseek"] = {"enabled": True}
data["auth"] = {"profiles": {"deepseek:default": {"provider": "deepseek", "mode": "api_key"}}}
data["models"] = {
    "mode": "merge",
    "providers": {
        "deepseek": {
            "baseUrl": "https://api.deepseek.com",
            "api": "openai-completions",
            "models": [{
                "id": model_id,
                "name": "DeepSeek V4 Flash",
                "reasoning": True,
                "input": ["text"],
                "contextWindow": 1000000,
                "maxTokens": 384000,
                "compat": {
                    "supportsReasoningEffort": True,
                    "supportsUsageInStreaming": True,
                    "maxTokensField": "max_tokens",
                },
                "api": "openai-completions",
            }],
        }
    },
}
data["skills"] = {"install": {"nodeManager": "npm"}}
data["meta"] = {
    "lastTouchedVersion": data.get("meta", {}).get("lastTouchedVersion", "2026.5.7"),
    "lastTouchedAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
}

if configure_wecom:
    allow = tools.setdefault("alsoAllow", [])
    if "wecom_mcp" not in allow:
        allow.append("wecom_mcp")
    plugins["wecom-openclaw-plugin"] = {"enabled": True}
    wecom = data.setdefault("channels", {}).setdefault("wecom", {})
    wecom["botId"] = os.environ["OPENCLAW_BOT_ID"]
    wecom["secret"] = os.environ["OPENCLAW_BOT_SECRET"]
    wecom["enabled"] = True

tmp = config_path.with_suffix(".json.tmp")
tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n")
tmp.replace(config_path)

(agent_dir / "auth-profiles.json").write_text(json.dumps({
    "version": 1,
    "profiles": {
        "deepseek:default": {
            "type": "api_key",
            "provider": "deepseek",
            "key": os.environ["OPENCLAW_DEEPSEEK_API_KEY"],
        }
    },
}, ensure_ascii=False, indent=2) + "\n")
(agent_dir / "models.json").write_text(json.dumps(data["models"], ensure_ascii=False, indent=2) + "\n")

token_file = Path(os.environ.get("OPENCLAW_TOKEN_FILE", "/var/log/openclaw.token"))
token_file.parent.mkdir(parents=True, exist_ok=True)
token_file.write_text(token + "\n")

print("Updated ~/.openclaw/openclaw.json")
print("Workspace OK: ~/.openclaw/workspace")
print("Sessions OK: ~/.openclaw/agents/main/sessions")
PY
         python3 /tmp/agenthub-openclaw-setup.py && \
         (supervisorctl start openclaw || supervisorctl restart openclaw) && \
         for i in $(seq 1 30); do \
           new_openclaw_pid=$(pgrep -xo openclaw || true); \
           status="$(supervisorctl status openclaw || true)"; \
           token_ready="$(python3 -c 'import json; print(json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", ""))' 2>/dev/null || true)"; \
           if [ -n "$new_openclaw_pid" ] && [ -n "$token_ready" ] && printf '%s\n' "$status" | grep -q RUNNING; then \
             printf '%s\n' "$status"; \
             break; \
           fi; \
           sleep 0.5; \
         done && \
         test "$(python3 -c 'import json; print(json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("mode", ""))')" = token && \
         test -n "$(python3 -c 'import json; print(json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", ""))')" && \
         test -n "$(pgrep -xo openclaw || true)" && \
         supervisorctl status openclaw | grep -q RUNNING
"#;

    script.replace(
        "__CONFIGURE_WECOM__",
        if configure_wecom { "True" } else { "False" },
    )
}

fn is_openclaw_config_conflict(output: &CommandOutput) -> bool {
    output.stdout.contains("ConfigMutationConflictError")
        || output.stderr.contains("ConfigMutationConflictError")
        || output.stdout.contains("Config overwrite:")
        || output.stderr.contains("Config overwrite:")
}

async fn run_envd_command(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    req: Value,
) -> AppResult<CommandOutput> {
    let host = format!("{}-{}.{}", ENVD_PORT, sandbox_id, domain);
    let url = std::env::var("AGENTHUB_SANDBOX_PROXY_URL")
        .unwrap_or_else(|_| "http://127.0.0.1".to_string());
    let url = format!("{}/process.Process/Start", url.trim_end_matches('/'));

    let body = connect_envelope(&serde_json::to_vec(&req).map_err(anyhow::Error::from)?);
    let resp = state
        .http_client
        .post(url)
        .header("Host", host)
        .header("Content-Type", CONNECT_JSON)
        .header("Authorization", "Basic cm9vdDo=")
        .body(body)
        .send()
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("envd command request failed: {}", e)))?;

    if !resp.status().is_success() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "envd command request returned HTTP {}",
            resp.status()
        )));
    }

    let bytes = resp.bytes().await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed reading envd command stream: {}", e))
    })?;
    parse_connect_stream(&bytes)
}

fn connect_envelope(payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(payload.len() + 5);
    out.push(0);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

fn parse_connect_stream(bytes: &[u8]) -> AppResult<CommandOutput> {
    let mut out = CommandOutput::default();
    let mut i = 0usize;

    while i + 5 <= bytes.len() {
        let flags = bytes[i];
        let len =
            u32::from_be_bytes([bytes[i + 1], bytes[i + 2], bytes[i + 3], bytes[i + 4]]) as usize;
        i += 5;
        if i + len > bytes.len() {
            return Err(AppError::Internal(anyhow::anyhow!(
                "truncated envd command stream"
            )));
        }
        let payload = &bytes[i..i + len];
        i += len;

        let v: Value = serde_json::from_slice(payload)
            .map_err(|e| AppError::Internal(anyhow::anyhow!("invalid envd JSON event: {}", e)))?;

        if flags & 0b10 != 0 {
            if v.get("error").is_some() {
                return Err(AppError::Internal(anyhow::anyhow!(
                    "envd command error: {}",
                    v
                )));
            }
            continue;
        }

        let Some(event) = v.get("event") else {
            continue;
        };
        if let Some(data) = event.get("data") {
            if let Some(stdout) = data.get("stdout").and_then(Value::as_str) {
                out.stdout.push_str(&decode_b64_lossy(stdout));
            }
            if let Some(stderr) = data.get("stderr").and_then(Value::as_str) {
                out.stderr.push_str(&decode_b64_lossy(stderr));
            }
        }
        if let Some(end) = event.get("end") {
            out.exit_code = end
                .get("exitCode")
                .and_then(Value::as_i64)
                .or_else(|| parse_exit_status(end.get("status").and_then(Value::as_str)))
                .unwrap_or_default() as i32;
        }
    }

    Ok(out)
}

fn decode_b64_lossy(s: &str) -> String {
    BASE64
        .decode(s)
        .map(|b| String::from_utf8_lossy(&b).into_owned())
        .unwrap_or_default()
}

fn parse_exit_status(status: Option<&str>) -> Option<i64> {
    let status = status?;
    status
        .strip_prefix("exit status ")
        .and_then(|v| v.trim().parse::<i64>().ok())
}

pub(crate) fn sandbox_url(port: u16, sandbox_id: &str, domain: &str) -> String {
    format!("http://{}-{}.{}", port, sandbox_id, domain)
}

pub(crate) fn sandbox_https_url(port: u16, sandbox_id: &str, domain: &str) -> String {
    format!("https://{}-{}.{}", port, sandbox_id, domain)
}

pub(crate) fn tokenized_gateway_url(url: String, token: Option<String>) -> String {
    let Some(token) = token.filter(|v| !v.trim().is_empty()) else {
        return url;
    };
    format!("{}#token={}", url, token.trim())
}
