// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::{collections::HashMap, fs, path::Path as FsPath, time::Duration};

use axum::{extract::Path, extract::State, http::StatusCode, response::IntoResponse, Json};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::{
    error::{AppError, AppResult},
    models::{
        EgressRule, EgressRuleAction, EgressRuleInject, EgressRuleMatch, NewSandbox,
        RollbackResponse, SandboxNetworkConfig,
    },
    state::AppState,
};

const DEFAULT_DS_OPENCLAW_TEMPLATE: &str = "wecom-ds-openclaw";
const DEEPSEEK_CHAT_MODEL: &str = "deepseek-chat";
const DEEPSEEK_CHAT_MODEL_LABEL: &str = "DeepSeek Chat";
const DEEPSEEK_V4_FLASH_MODEL: &str = "deepseek/deepseek-v4-flash";
const DEEPSEEK_V4_FLASH_MODEL_LABEL: &str = "DeepSeek V4 Flash";
const DEFAULT_OPENCLAW_MODEL: &str = DEEPSEEK_V4_FLASH_MODEL;
const DEEPSEEK_V4_PRO_MODEL: &str = "deepseek/deepseek-v4-pro";
const DEEPSEEK_V4_PRO_MODEL_LABEL: &str = "DeepSeek V4 Pro";
const ENVD_PORT: u16 = 49983;
const LOGIN_ENV_PORT: u16 = 8080;
const OPENCLAW_UI_PORT: u16 = 18789;
const CONNECT_JSON: &str = "application/connect+json";
const SETTING_DEEPSEEK_API_KEY: &str = "deepseek_api_key";
const SETTING_LLM_PROVIDER: &str = "llm_provider";
const SETTING_LLM_BASE_URL: &str = "llm_base_url";
const SETTING_LLM_MODEL: &str = "llm_model";
const SETTING_LLM_API_KEY: &str = "llm_api_key";
const SETTING_LLM_CREDENTIAL_MODE: &str = "llm_credential_mode";
const SETTING_GATEWAY_DOMAIN: &str = "gateway_domain";
const DEFAULT_LLM_PROVIDER: &str = "deepseek";
const DEFAULT_LLM_BASE_URL: &str = "https://api.deepseek.com";
const DEFAULT_LLM_CREDENTIAL_MODE: &str = "egress";
const OPENCLAW_EGRESS_MANAGED_KEY: &str = "CUBE_EGRESS_MANAGED";
const OPENCLAW_NODE_EXTRA_CA_CERTS: &str = "/root/.openclaw/cube-egress-ca.crt";
const OPENCLAW_HOST_STATE_ROOT: &str = "/data/agenthub/openclaw";
const OPENCLAW_HOST_SNAPSHOT_ROOT: &str = "/data/agenthub/openclaw-snapshots";
const OPENCLAW_SANDBOX_STATE_PATH: &str = "/root/.openclaw";
const HOSTDIR_MOUNT_KEY: &str = "host-mount";
const PERSISTENCE_MODE_FULL_SNAPSHOT: &str = "full_snapshot";
const PERSISTENCE_MODE_SHARED_FILES: &str = "shared_files";
const ROOTFS_SOURCE_TEMPLATE: &str = "template";
const ROOTFS_SOURCE_SNAPSHOT: &str = "snapshot";
const SNAPSHOT_KIND_SANDBOX: &str = "sandbox";

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateAgentInstanceRequest {
    pub name: String,
    pub engine: String,
    pub model: Option<String>,
    #[serde(default)]
    pub template_id: Option<String>,
    #[serde(default)]
    pub snapshot_id: Option<String>,
    #[serde(default)]
    pub persistence_mode: Option<String>,
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
    pub persistence_mode: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rootfs_source_type: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rootfs_source_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub openclaw_persist_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub openclaw_state_path: Option<String>,
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
    // Runtime model updates are intentionally disabled this round, but the
    // request shape is kept so the endpoint can still parse client payloads.
    #[allow(dead_code)]
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
pub struct RegisterMarketAgentTemplateRequest {
    pub template_id: String,
    pub name: Option<String>,
    pub model: Option<String>,
    pub version: Option<String>,
    #[serde(default)]
    pub recommended: bool,
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
    #[serde(skip_serializing_if = "Option::is_none")]
    pub persistence_mode: Option<String>,
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
    #[serde(rename = "snapshotKind", skip_serializing_if = "Option::is_none")]
    pub snapshot_kind: Option<String>,
    #[serde(rename = "originSandboxID", skip_serializing_if = "Option::is_none")]
    pub origin_sandbox_id: Option<String>,
    #[serde(
        rename = "publishedTemplateId",
        skip_serializing_if = "Option::is_none"
    )]
    pub published_template_id: Option<String>,
    #[serde(rename = "rootfsSourceType", skip_serializing_if = "Option::is_none")]
    pub rootfs_source_type: Option<String>,
    #[serde(rename = "rootfsSourceId", skip_serializing_if = "Option::is_none")]
    pub rootfs_source_id: Option<String>,
    #[serde(rename = "rootfsSnapshotId", skip_serializing_if = "Option::is_none")]
    pub rootfs_snapshot_id: Option<String>,
    #[serde(
        rename = "openclawStateSnapshotPath",
        skip_serializing_if = "Option::is_none"
    )]
    pub openclaw_state_snapshot_path: Option<String>,
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

    let mut persistence_mode =
        normalize_persistence_mode(body.persistence_mode.as_deref()).to_string();
    let snapshot_id = body
        .snapshot_id
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToString::to_string);
    let mut rootfs_source_type = if snapshot_id.is_some() {
        ROOTFS_SOURCE_SNAPSHOT
    } else {
        ROOTFS_SOURCE_TEMPLATE
    }
    .to_string();
    let mut rootfs_source_id = snapshot_id.clone().unwrap_or_else(|| {
        body.template_id
            .as_deref()
            .map(str::trim)
            .filter(|v| !v.is_empty())
            .map(ToString::to_string)
            .unwrap_or_else(|| {
                std::env::var("AGENTHUB_DS_OPENCLAW_TEMPLATE")
                    .unwrap_or_else(|_| DEFAULT_DS_OPENCLAW_TEMPLATE.to_string())
            })
    });
    let mut template_id = rootfs_source_id.clone();
    let explicit_template_id = body
        .template_id
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToString::to_string);
    let timeout = std::env::var("AGENTHUB_SANDBOX_TIMEOUT_SECS")
        .ok()
        .and_then(|v| v.parse::<i32>().ok())
        .unwrap_or(86_400);

    // 若 template_id 命中已发布的助手模板，则克隆出的沙箱已内置 OpenClaw 配置，
    // 创建时可走快路径，跳过重复装配。
    let agent_template = if rootfs_source_type == ROOTFS_SOURCE_TEMPLATE {
        if let Some(store) = &state.agenthub_store {
            store.get_template(&template_id).await.ok().flatten()
        } else {
            None
        }
    } else {
        None
    };
    if let Some(template_mode) = agent_template
        .as_ref()
        .and_then(|template| template.persistence_mode.as_deref())
        .map(|mode| normalize_persistence_mode(Some(mode)))
    {
        persistence_mode = template_mode.to_string();
    }

    let market_template = agent_template
        .as_ref()
        .is_some_and(|t| t.source_agent_id == "market");
    let template_openclaw_state_source =
        if let (Some(store), Some(template)) = (&state.agenthub_store, agent_template.as_ref()) {
            if !market_template {
                store
                    .get_snapshot(&template.source_agent_id, &template.source_snapshot_id)
                    .await
                    .ok()
                    .flatten()
                    .and_then(|snapshot| {
                        if let Some(rootfs_id) = snapshot.rootfs_snapshot_id.clone() {
                            rootfs_source_type = ROOTFS_SOURCE_SNAPSHOT.to_string();
                            rootfs_source_id = rootfs_id.clone();
                            template_id = rootfs_id;
                        }
                        snapshot.openclaw_state_snapshot_path
                    })
            } else {
                None
            }
        } else {
            None
        };
    // 快照发布的助手模板已内置 OpenClaw 配置，可走快路径；应用市场模板
    // 只是基础镜像，仍需按当前 LLM/企微设置装配。
    let use_template_fastpath = agent_template.is_some() && !market_template && !should_bind_wecom;
    let mut llm_config = resolve_llm_config(&state).await?;
    let default_model: String = body
        .model
        .as_deref()
        .map(str::trim)
        .filter(|m| !m.is_empty())
        .map(ToString::to_string)
        .or_else(|| {
            agent_template
                .as_ref()
                .filter(|_| !market_template)
                .map(|t| t.model.clone())
        })
        .unwrap_or_else(|| llm_config.model.clone());
    let default_model_id = normalize_agent_model(&default_model)
        .map(|(_, model_id)| model_id.to_string())
        .unwrap_or_else(|| default_model.clone());
    llm_config.model = default_model_id;
    let network_config = agenthub_network_config(&llm_config)?;
    let shared_files = persistence_mode == PERSISTENCE_MODE_SHARED_FILES;
    let openclaw_persist = if shared_files {
        let persist_id = new_openclaw_persist_id();
        let state_path = prepare_openclaw_state_dir(&persist_id)?;
        if let Some(source_path) = template_openclaw_state_source
            .as_deref()
            .filter(|path| FsPath::new(path).is_dir())
        {
            copy_openclaw_state_dir(source_path, &state_path)?;
        }
        let mount_metadata = openclaw_host_mount_metadata(&state_path)?;
        Some((persist_id, state_path, mount_metadata))
    } else {
        None
    };
    let mut metadata = HashMap::from([
        ("agenthub".to_string(), "true".to_string()),
        ("agenthub.name".to_string(), name.to_string()),
        ("agenthub.engine".to_string(), "openclaw".to_string()),
        (
            "agenthub.persistence_mode".to_string(),
            persistence_mode.clone(),
        ),
        (
            "agenthub.rootfs_source_type".to_string(),
            rootfs_source_type.clone(),
        ),
        (
            "agenthub.rootfs_source_id".to_string(),
            rootfs_source_id.clone(),
        ),
    ]);
    if let Some((persist_id, _, mount_metadata)) = openclaw_persist.as_ref() {
        metadata.insert(
            "agenthub.openclaw.persist_id".to_string(),
            persist_id.clone(),
        );
        metadata.insert(HOSTDIR_MOUNT_KEY.to_string(), mount_metadata.clone());
    }

    let created = state
        .services
        .sandboxes
        .create_sandbox(NewSandbox {
            template_id: template_id.clone(),
            timeout,
            lifecycle: None,
            secure: None,
            allow_internet_access: Some(true),
            network: network_config,
            metadata: Some(metadata),
            distribution_scope: agenthub_create_distribution_scope(
                &persistence_mode,
                &rootfs_source_type,
            ),
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

    // Published-template sandboxes already carry OpenClaw state, so the fast
    // path only merges LLM settings. Shared-files mounts and app-market
    // templates start empty and still need a full init.
    let has_openclaw_state =
        use_template_fastpath && (!shared_files || template_openclaw_state_source.is_some());
    // Merge mode preserves the bundled gateway token; it is read back from the
    // sandbox/host below. Full init writes a token Rust controls.
    let fixed_gateway_token = if has_openclaw_state {
        None
    } else {
        Some(new_gateway_token())
    };

    let plan = LlmRuntimePlan::resolve(&llm_config, &llm_config.model);
    let apply_options = if should_bind_wecom {
        OpenClawApplyOptions {
            mode: OpenClawApplyMode::FullInit,
            gateway_token: fixed_gateway_token.clone(),
            preserve_gateway_token: false,
            configure_wecom: true,
            bot_id: body.bot_id.clone(),
            bot_secret: body.bot_secret.clone(),
        }
    } else if has_openclaw_state {
        OpenClawApplyOptions::merge_llm()
    } else {
        OpenClawApplyOptions::full_init(fixed_gateway_token.clone())
    };

    let setup =
        match apply_openclaw_runtime(&state, &sandbox_id, &domain, &plan, &apply_options).await {
            Ok(setup) => setup,
            Err(err) => {
                let _ = state.services.sandboxes.kill_sandbox(&sandbox_id).await;
                return Err(err);
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

    // OpenClaw may trigger an in-process reload after the config file changes.
    // Read the gateway token after the reload window so the stored URL matches
    // the token the live gateway actually enforces.
    tokio::time::sleep(Duration::from_secs(5)).await;
    let host_gateway_token = openclaw_persist
        .as_ref()
        .and_then(|(_, state_path, _)| read_openclaw_gateway_token_from_host(state_path));
    let sandbox_gateway_token = read_openclaw_gateway_token(&state, &sandbox_id, &domain)
        .await
        .unwrap_or(None);
    let gateway_token = host_gateway_token
        .or(sandbox_gateway_token)
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
        template_id: explicit_template_id.unwrap_or(template_id),
        gateway_url,
        env_url,
        persistence_mode: Some(persistence_mode.clone()),
        rootfs_source_type: Some(rootfs_source_type.to_string()),
        rootfs_source_id: Some(rootfs_source_id),
        openclaw_persist_id: openclaw_persist
            .as_ref()
            .map(|(persist_id, _, _)| persist_id.clone()),
        openclaw_state_path: openclaw_persist
            .as_ref()
            .map(|(_, state_path, _)| state_path.clone()),
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

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentSettingsResponse {
    /// Backward-compatible alias for the default LLM API key state.
    pub deepseek_api_key_configured: bool,
    /// Backward-compatible masked preview of the active key. Never the full key.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub deepseek_api_key_masked: Option<String>,
    /// Backward-compatible key source: "database" | "env" | "none".
    pub source: String,
    /// LLM provider id, e.g. "deepseek" or "custom".
    pub llm_provider: String,
    /// OpenAI-compatible base URL.
    pub llm_base_url: String,
    /// Default model id injected into OpenClaw.
    pub llm_model: String,
    /// Whether the default LLM API key is configured.
    pub llm_api_key_configured: bool,
    /// Masked preview of the default LLM API key.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub llm_api_key_masked: Option<String>,
    /// Where the LLM API key comes from: "database" | "env" | "none".
    pub llm_api_key_source: String,
    /// How the LLM credential is delivered: "egress" keeps the real key in
    /// CubeEgress policy injection, "env" keeps legacy OpenClaw env/config injection.
    pub llm_credential_mode: String,
    /// Whether settings can be persisted (requires the AgentHub database).
    pub persistence_enabled: bool,
    /// Configured gateway domain for subdomain-origin access (e.g. "cube.app"),
    /// or None when not set. Plaintext value, not a secret. Assistants open their
    /// OpenClaw gateway via `<port>-<sandboxId>.<gateway_domain>` when present.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gateway_domain: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateAgentSettingsRequest {
    /// Legacy alias. Omitted or blank leaves the existing key untouched.
    #[serde(default)]
    pub deepseek_api_key: Option<String>,
    #[serde(default)]
    pub llm_provider: Option<String>,
    #[serde(default)]
    pub llm_base_url: Option<String>,
    #[serde(default)]
    pub llm_model: Option<String>,
    /// New API key for the selected/default LLM. Omitted or blank leaves it untouched.
    #[serde(default)]
    pub llm_api_key: Option<String>,
    /// Credential delivery mode: "egress" (recommended) or "env" (legacy).
    #[serde(default)]
    pub llm_credential_mode: Option<String>,
    /// Gateway domain (e.g. "cube.app"). Empty string clears it. Omitted leaves it untouched.
    #[serde(default)]
    pub gateway_domain: Option<String>,
}

#[derive(Debug, Clone)]
struct LlmConfig {
    provider: String,
    base_url: String,
    model: String,
    api_key: String,
    credential_mode: String,
}

impl LlmConfig {
    fn uses_egress_credentials(&self) -> bool {
        self.credential_mode == "egress"
    }

    fn openclaw_api_key(&self) -> &str {
        if self.uses_egress_credentials() {
            OPENCLAW_EGRESS_MANAGED_KEY
        } else {
            &self.api_key
        }
    }
}

/// OpenClaw resolves a model as `{provider}/{suffix}`. Any caller-supplied
/// prefix is dropped so the suffix is the bare upstream model id, which is also
/// the value sent to the upstream API as the request body `model` field.
fn openclaw_model_suffix(model: &str) -> &str {
    model
        .split_once('/')
        .map(|(_, rest)| rest)
        .filter(|rest| !rest.is_empty())
        .unwrap_or(model)
}

/// Fully resolved view of the LLM for a single sandbox.
///
/// This is the one place that decides how a model string maps onto OpenClaw's
/// provider/model namespace, so the sandbox-side apply script never has to
/// reason about prefixes, providers, or credential modes.
#[derive(Debug, Clone)]
struct LlmRuntimePlan {
    /// Original model string as stored/displayed (e.g. `deepseek/deepseek-v4-flash`).
    public_model: String,
    /// Bare model id sent upstream as the request body `model` field.
    upstream_model_id: String,
    /// Provider literal from settings (e.g. `deepseek`, `openai-compatible`).
    upstream_provider: String,
    upstream_base_url: String,
    /// Namespaced primary OpenClaw resolves against, `{provider}/{model_id}`.
    openclaw_primary: String,
    openclaw_model_name: String,
    /// Key OpenClaw stores. In egress mode this is a managed placeholder; the
    /// real key only lives in the egress rule.
    openclaw_api_key: String,
    credential_mode: String,
}

impl LlmRuntimePlan {
    /// Resolves the runtime plan from the persisted LLM config and the
    /// effective public model chosen by the caller (request / template / clone).
    fn resolve(llm: &LlmConfig, public_model: &str) -> Self {
        let public_model = {
            let trimmed = public_model.trim();
            if trimmed.is_empty() {
                DEFAULT_OPENCLAW_MODEL.to_string()
            } else {
                trimmed.to_string()
            }
        };
        let upstream_model_id = openclaw_model_suffix(&public_model).to_string();
        let openclaw_primary = format!("{}/{}", llm.provider, upstream_model_id);
        Self {
            openclaw_model_name: model_display_name(&public_model),
            upstream_model_id,
            upstream_provider: llm.provider.clone(),
            upstream_base_url: llm.base_url.clone(),
            openclaw_primary,
            openclaw_api_key: llm.openclaw_api_key().to_string(),
            credential_mode: llm.credential_mode.clone(),
            public_model,
        }
    }
}

fn normalize_llm_provider(raw: &str) -> String {
    let value = raw.trim().to_ascii_lowercase();
    if value.is_empty() {
        DEFAULT_LLM_PROVIDER.to_string()
    } else {
        value
    }
}

fn normalize_llm_base_url(raw: &str) -> String {
    let value = raw.trim().trim_end_matches('/').to_string();
    if value.is_empty() {
        DEFAULT_LLM_BASE_URL.to_string()
    } else {
        value
    }
}

fn normalize_llm_model(raw: &str) -> String {
    let value = raw.trim();
    if value.is_empty() {
        DEFAULT_OPENCLAW_MODEL.to_string()
    } else {
        value.to_string()
    }
}

fn normalize_llm_credential_mode(raw: &str) -> String {
    match raw.trim().to_ascii_lowercase().as_str() {
        "env" | "environment" | "legacy" => "env".to_string(),
        "egress" | "hosted" | "managed" | "" => DEFAULT_LLM_CREDENTIAL_MODE.to_string(),
        _ => DEFAULT_LLM_CREDENTIAL_MODE.to_string(),
    }
}

fn egress_ca_pem() -> String {
    std::fs::read_to_string("/etc/cube/ca/cube-root-ca.crt").unwrap_or_default()
}

fn new_openclaw_persist_id() -> String {
    uuid::Uuid::new_v4().simple().to_string()
}

fn new_agenthub_snapshot_id() -> String {
    format!("agenthub-{}", uuid::Uuid::new_v4().simple())
}

fn openclaw_host_state_path(persist_id: &str) -> String {
    format!(
        "{}/{}",
        OPENCLAW_HOST_STATE_ROOT.trim_end_matches('/'),
        persist_id
    )
}

fn openclaw_host_snapshot_path(snapshot_id: &str) -> String {
    format!(
        "{}/{}",
        OPENCLAW_HOST_SNAPSHOT_ROOT.trim_end_matches('/'),
        snapshot_id
    )
}

fn prepare_openclaw_state_dir(persist_id: &str) -> AppResult<String> {
    let path = openclaw_host_state_path(persist_id);
    fs::create_dir_all(&path).map_err(|e| {
        AppError::Internal(anyhow::anyhow!(
            "failed to create OpenClaw state directory {}: {}",
            path,
            e
        ))
    })?;
    Ok(path)
}

fn copy_openclaw_state_dir(source: &str, target: &str) -> AppResult<()> {
    if source.trim().is_empty() || !FsPath::new(source).is_dir() {
        return Ok(());
    }
    fs::create_dir_all(target).map_err(|e| {
        AppError::Internal(anyhow::anyhow!(
            "failed to create cloned OpenClaw state directory {}: {}",
            target,
            e
        ))
    })?;
    let status = std::process::Command::new("rsync")
        .args([
            "-a",
            "--delete",
            &format!("{}/", source.trim_end_matches('/')),
            target,
        ])
        .status()
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!(
                "failed to copy OpenClaw state from {} to {}: {}",
                source,
                target,
                e
            ))
        })?;
    if !status.success() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "failed to copy OpenClaw state from {} to {} with status {}",
            source,
            target,
            status
        )));
    }
    Ok(())
}

fn openclaw_host_mount_metadata(host_path: &str) -> AppResult<String> {
    serde_json::to_string(&serde_json::json!([{
        "hostPath": host_path,
        "mountPath": OPENCLAW_SANDBOX_STATE_PATH,
    }]))
    .map_err(|e| {
        AppError::Internal(anyhow::anyhow!(
            "failed to encode OpenClaw host mount metadata: {}",
            e
        ))
    })
}

pub(crate) fn read_openclaw_gateway_token_from_host(state_path: &str) -> Option<String> {
    let path = FsPath::new(state_path).join("openclaw.json");
    let data = fs::read_to_string(path).ok()?;
    let value: Value = serde_json::from_str(&data).ok()?;
    value
        .get("gateway")
        .and_then(|v| v.get("auth"))
        .and_then(|v| v.get("token"))
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToString::to_string)
}

fn normalize_persistence_mode(raw: Option<&str>) -> &'static str {
    match raw.map(str::trim).filter(|v| !v.is_empty()) {
        Some("full_snapshot") | Some("sandbox") | Some("rootfs") => PERSISTENCE_MODE_FULL_SNAPSHOT,
        _ => PERSISTENCE_MODE_SHARED_FILES,
    }
}

fn agenthub_distribution_scope() -> Option<Vec<String>> {
    std::env::var("AGENTHUB_HOST_MOUNT_NODE_ID")
        .or_else(|_| std::env::var("CUBE_SANDBOX_NODE_IP"))
        .ok()
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
        .map(|v| vec![v])
}

fn agenthub_create_distribution_scope(
    persistence_mode: &str,
    rootfs_source_type: &str,
) -> Option<Vec<String>> {
    if rootfs_source_type == ROOTFS_SOURCE_SNAPSHOT
        && persistence_mode != PERSISTENCE_MODE_SHARED_FILES
    {
        return None;
    }
    agenthub_distribution_scope()
}

fn llm_base_url_parts(base_url: &str) -> AppResult<(String, String, String)> {
    let url = reqwest::Url::parse(base_url)
        .map_err(|e| AppError::BadRequest(format!("Invalid LLM Base URL '{}': {}", base_url, e)))?;
    let scheme = url.scheme().to_string();
    if scheme != "http" && scheme != "https" {
        return Err(AppError::BadRequest(
            "LLM Base URL must use http or https".to_string(),
        ));
    }
    let host = url
        .host_str()
        .map(str::to_string)
        .ok_or_else(|| AppError::BadRequest("LLM Base URL must include a host".to_string()))?;
    let base_path = url.path().trim_end_matches('/');
    let path = if base_path.is_empty() {
        "/*".to_string()
    } else {
        format!("{base_path}/*")
    };
    Ok((scheme, host, path))
}

fn llm_egress_rule(llm: &LlmConfig) -> AppResult<EgressRule> {
    let (scheme, host, path) = llm_base_url_parts(&llm.base_url)?;
    Ok(EgressRule {
        name: format!("agenthub-llm-{}", llm.provider),
        r#match: EgressRuleMatch {
            sni: if scheme == "https" {
                Some(host.clone())
            } else {
                None
            },
            host: Some(host),
            method: Some(vec![
                "GET".to_string(),
                "POST".to_string(),
                "PUT".to_string(),
                "PATCH".to_string(),
                "DELETE".to_string(),
            ]),
            path: Some(path),
            scheme: Some(scheme),
        },
        action: EgressRuleAction {
            allow: true,
            audit: Some("metadata".to_string()),
            inject: Some(vec![EgressRuleInject {
                header: "Authorization".to_string(),
                secret: llm.api_key.clone(),
                format: Some("Bearer ${SECRET}".to_string()),
            }]),
        },
    })
}

fn agenthub_network_config(llm: &LlmConfig) -> AppResult<Option<SandboxNetworkConfig>> {
    if !llm.uses_egress_credentials() {
        return Ok(None);
    }
    Ok(Some(SandboxNetworkConfig {
        allow_public_traffic: Some(true),
        allow_out: None,
        deny_out: None,
        mask_request_host: None,
        rules: Some(vec![llm_egress_rule(llm)?]),
    }))
}

/// Normalizes a user-supplied gateway domain: strips scheme, surrounding
/// whitespace, and trailing slashes. An empty result clears the setting.
fn normalize_gateway_domain(raw: &str) -> String {
    let v = raw.trim();
    let v = v
        .strip_prefix("https://")
        .or_else(|| v.strip_prefix("http://"))
        .unwrap_or(v);
    v.trim_end_matches('/').trim().to_string()
}

/// Reads the configured gateway domain from the database, if any.
async fn resolve_gateway_domain(state: &AppState) -> Option<String> {
    let store = state.agenthub_store.as_ref()?;
    let value = store.get_setting(SETTING_GATEWAY_DOMAIN).await.ok()??;
    let trimmed = value.trim().to_string();
    if trimmed.is_empty() {
        None
    } else {
        Some(trimmed)
    }
}

async fn read_setting_trimmed(state: &AppState, key: &str) -> Option<String> {
    let store = state.agenthub_store.as_ref()?;
    let value = store.get_setting(key).await.ok()??;
    let trimmed = value.trim().to_string();
    if trimmed.is_empty() {
        None
    } else {
        Some(trimmed)
    }
}

async fn read_llm_api_key(state: &AppState) -> (Option<String>, String) {
    if let Some(store) = &state.agenthub_store {
        if let Ok(Some(value)) = store.get_setting(SETTING_LLM_API_KEY).await {
            let trimmed = crate::crypto::decrypt_or_passthrough(&value)
                .trim()
                .to_string();
            if !trimmed.is_empty() {
                return (Some(trimmed), "database".to_string());
            }
        }
        // Backward compatibility with existing DeepSeek-only installations.
        if let Ok(Some(value)) = store.get_setting(SETTING_DEEPSEEK_API_KEY).await {
            let trimmed = crate::crypto::decrypt_or_passthrough(&value)
                .trim()
                .to_string();
            if !trimmed.is_empty() {
                return (Some(trimmed), "database".to_string());
            }
        }
    }
    (None, "none".to_string())
}

/// Resolves the full LLM configuration injected into OpenClaw.
async fn resolve_llm_config(state: &AppState) -> AppResult<LlmConfig> {
    let provider = read_setting_trimmed(state, SETTING_LLM_PROVIDER)
        .await
        .map(|v| normalize_llm_provider(&v))
        .unwrap_or_else(|| DEFAULT_LLM_PROVIDER.to_string());
    let base_url = read_setting_trimmed(state, SETTING_LLM_BASE_URL)
        .await
        .map(|v| normalize_llm_base_url(&v))
        .unwrap_or_else(|| DEFAULT_LLM_BASE_URL.to_string());
    let model = read_setting_trimmed(state, SETTING_LLM_MODEL)
        .await
        .map(|v| normalize_llm_model(&v))
        .unwrap_or_else(|| DEFAULT_OPENCLAW_MODEL.to_string());
    let credential_mode = read_setting_trimmed(state, SETTING_LLM_CREDENTIAL_MODE)
        .await
        .map(|v| normalize_llm_credential_mode(&v))
        .unwrap_or_else(|| DEFAULT_LLM_CREDENTIAL_MODE.to_string());
    let (api_key, _) = read_llm_api_key(state).await;
    let Some(api_key) = api_key else {
        return Err(AppError::BadRequest(
            "LLM API key is not configured. Configure it on the AgentHub settings page first."
                .to_string(),
        ));
    };
    Ok(LlmConfig {
        provider,
        base_url,
        model,
        api_key,
        credential_mode,
    })
}

fn mask_secret(secret: &str) -> String {
    let s = secret.trim();
    let count = s.chars().count();
    if count <= 4 {
        "••••".to_string()
    } else {
        let last4: String = s.chars().skip(count - 4).collect();
        format!("••••{}", last4)
    }
}

async fn current_settings(state: &AppState) -> AgentSettingsResponse {
    let persistence_enabled = state.agenthub_store.is_some();
    let llm_provider = read_setting_trimmed(state, SETTING_LLM_PROVIDER)
        .await
        .map(|v| normalize_llm_provider(&v))
        .unwrap_or_else(|| DEFAULT_LLM_PROVIDER.to_string());
    let llm_base_url = read_setting_trimmed(state, SETTING_LLM_BASE_URL)
        .await
        .map(|v| normalize_llm_base_url(&v))
        .unwrap_or_else(|| DEFAULT_LLM_BASE_URL.to_string());
    let llm_model = read_setting_trimmed(state, SETTING_LLM_MODEL)
        .await
        .map(|v| normalize_llm_model(&v))
        .unwrap_or_else(|| DEFAULT_OPENCLAW_MODEL.to_string());
    let llm_credential_mode = read_setting_trimmed(state, SETTING_LLM_CREDENTIAL_MODE)
        .await
        .map(|v| normalize_llm_credential_mode(&v))
        .unwrap_or_else(|| DEFAULT_LLM_CREDENTIAL_MODE.to_string());
    let (active, source) = read_llm_api_key(state).await;
    let gateway_domain = resolve_gateway_domain(state).await;

    AgentSettingsResponse {
        deepseek_api_key_configured: active.is_some(),
        deepseek_api_key_masked: active.as_deref().map(mask_secret),
        source: source.clone(),
        llm_provider,
        llm_base_url,
        llm_model,
        llm_api_key_configured: active.is_some(),
        llm_api_key_masked: active.as_deref().map(mask_secret),
        llm_api_key_source: source,
        llm_credential_mode,
        persistence_enabled,
        gateway_domain,
    }
}

pub async fn get_agent_settings(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    Ok((StatusCode::OK, Json(current_settings(&state).await)))
}

pub async fn update_agent_settings(
    State(state): State<AppState>,
    Json(body): Json<UpdateAgentSettingsRequest>,
) -> AppResult<impl IntoResponse> {
    let Some(store) = &state.agenthub_store else {
        return Err(AppError::BadRequest(
            "AgentHub database persistence is not configured".to_string(),
        ));
    };

    let mut changed = false;

    if let Some(raw) = body.llm_provider.as_deref() {
        let provider = normalize_llm_provider(raw);
        store
            .set_setting(SETTING_LLM_PROVIDER, &provider)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to save AgentHub settings: {}", e))
            })?;
        changed = true;
    }

    if let Some(raw) = body.llm_base_url.as_deref() {
        let base_url = normalize_llm_base_url(raw);
        store
            .set_setting(SETTING_LLM_BASE_URL, &base_url)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to save AgentHub settings: {}", e))
            })?;
        changed = true;
    }

    if let Some(raw) = body.llm_model.as_deref() {
        let model = normalize_llm_model(raw);
        store
            .set_setting(SETTING_LLM_MODEL, &model)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to save AgentHub settings: {}", e))
            })?;
        changed = true;
    }

    if let Some(raw) = body.llm_credential_mode.as_deref() {
        let mode = normalize_llm_credential_mode(raw);
        store
            .set_setting(SETTING_LLM_CREDENTIAL_MODE, &mode)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to save AgentHub settings: {}", e))
            })?;
        changed = true;
    }

    // LLM API key: only update when a non-blank value is provided so that
    // saving other settings (e.g. base URL or gateway domain) doesn't wipe it.
    let api_key_input = body
        .llm_api_key
        .as_deref()
        .or(body.deepseek_api_key.as_deref());
    if let Some(raw) = api_key_input {
        let key = raw.trim();
        if !key.is_empty() {
            let encrypted = crate::crypto::encrypt_secret(key).map_err(|e| {
                AppError::Internal(anyhow::anyhow!(
                    "failed to encrypt AgentHub settings: {}",
                    e
                ))
            })?;
            store
                .set_setting(SETTING_LLM_API_KEY, &encrypted)
                .await
                .map_err(|e| {
                    AppError::Internal(anyhow::anyhow!("failed to save AgentHub settings: {}", e))
                })?;
            changed = true;
        }
    }

    // Gateway domain: stored in plaintext. An empty value clears it.
    if let Some(raw) = body.gateway_domain.as_ref() {
        let domain = normalize_gateway_domain(raw);
        store
            .set_setting(SETTING_GATEWAY_DOMAIN, &domain)
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("failed to save AgentHub settings: {}", e))
            })?;
        changed = true;
    }

    if !changed {
        return Err(AppError::BadRequest(
            "no settings provided to update".to_string(),
        ));
    }

    Ok((StatusCode::OK, Json(current_settings(&state).await)))
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
            persistence_mode: record.persistence_mode,
            recommended: record.recommended,
            created_at: record.created_at,
        })
        .collect::<Vec<_>>();
    Ok((StatusCode::OK, Json(templates)))
}

pub async fn register_market_agent_template(
    State(state): State<AppState>,
    Json(body): Json<RegisterMarketAgentTemplateRequest>,
) -> AppResult<impl IntoResponse> {
    let store = state.agenthub_store.as_ref().ok_or_else(|| {
        AppError::BadRequest("AgentHub database persistence is not configured".to_string())
    })?;
    let template_id = body.template_id.trim();
    if template_id.is_empty() {
        return Err(AppError::BadRequest("templateId is required".to_string()));
    }
    let name = body
        .name
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or(template_id);
    let model = body
        .model
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or(DEFAULT_OPENCLAW_MODEL);
    let version = body
        .version
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or("market");
    store
        .register_market_template(template_id, name, model, version, body.recommended)
        .await
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!("failed to register market template: {}", e))
        })?;

    let record = store
        .get_template(template_id)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to read template: {}", e)))?
        .ok_or_else(|| AppError::Internal(anyhow::anyhow!("registered template not found")))?;

    Ok((
        StatusCode::OK,
        Json(AgentTemplateResponse {
            template_id: record.template_id,
            name: record.name,
            source_agent_id: record.source_agent_id,
            source_snapshot_id: record.source_snapshot_id,
            source_sandbox_id: record.source_sandbox_id,
            model: record.model,
            version: record.version,
            persistence_mode: record.persistence_mode,
            recommended: record.recommended,
            created_at: record.created_at,
        }),
    ))
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
                    snapshot_kind: record.snapshot_kind,
                    origin_sandbox_id: record.origin_sandbox_id,
                    published_template_id: record.published_template_id,
                    rootfs_source_type: record.rootfs_source_type,
                    rootfs_source_id: record.rootfs_source_id,
                    rootfs_snapshot_id: record.rootfs_snapshot_id,
                    openclaw_state_snapshot_path: record.openclaw_state_snapshot_path,
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
            snapshot_id: item.snapshot_id.clone(),
            names: item.names,
            status: item.status,
            snapshot_kind: Some(SNAPSHOT_KIND_SANDBOX.to_string()),
            origin_sandbox_id: item.origin_sandbox_id,
            published_template_id: None,
            rootfs_source_type: Some(ROOTFS_SOURCE_SNAPSHOT.to_string()),
            rootfs_source_id: Some(item.snapshot_id.clone()),
            rootfs_snapshot_id: Some(item.snapshot_id.clone()),
            openclaw_state_snapshot_path: None,
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
    let task_template = record.template_id.clone();
    let task_rootfs_source_type = record.rootfs_source_type.clone();
    let task_rootfs_source_id = record.rootfs_source_id.clone();
    let task_base_snapshot_id = record.base_snapshot_id.clone();
    let task_openclaw_state_path = record.openclaw_state_path.clone();
    let task_operation = operation_id.clone();
    tokio::spawn(async move {
        if let Some(source_openclaw_path) = task_openclaw_state_path
            .as_deref()
            .filter(|path| FsPath::new(path).is_dir())
        {
            let snapshot_id = new_agenthub_snapshot_id();
            let snapshot_path = openclaw_host_snapshot_path(&snapshot_id);
            let result = async {
                copy_openclaw_state_dir(source_openclaw_path, &snapshot_path)?;
                let Some(store) = &task_state.agenthub_store else {
                    return Err(AppError::BadRequest(
                        "AgentHub database persistence is not configured".to_string(),
                    ));
                };
                let rootfs_snapshot_id = task_base_snapshot_id
                    .clone()
                    .or_else(|| task_rootfs_source_id.clone())
                    .unwrap_or_else(|| task_template.clone());
                let rootfs_source_type = task_rootfs_source_type
                    .as_deref()
                    .unwrap_or(ROOTFS_SOURCE_TEMPLATE);
                store
                    .upsert_agenthub_openclaw_snapshot(
                        &task_agent,
                        &task_sandbox,
                        &snapshot_id,
                        name.as_deref(),
                        rootfs_source_type,
                        &rootfs_snapshot_id,
                        &rootfs_snapshot_id,
                        &snapshot_path,
                    )
                    .await
                    .map_err(|e| {
                        AppError::Internal(anyhow::anyhow!(
                            "failed to save AgentHub OpenClaw snapshot: {}",
                            e
                        ))
                    })?;
                Ok::<String, AppError>(snapshot_id)
            }
            .await;

            match result {
                Ok(snapshot_id) => {
                    finish_agent_operation(
                        &task_state,
                        task_operation.as_deref(),
                        "succeeded",
                        Some(&snapshot_id),
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
            return;
        }

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

    let has_openclaw_host_mount = record
        .openclaw_state_path
        .as_deref()
        .is_some_and(|source_path| FsPath::new(source_path).is_dir());
    let persistence_mode =
        record
            .persistence_mode
            .as_deref()
            .unwrap_or(if has_openclaw_host_mount {
                PERSISTENCE_MODE_SHARED_FILES
            } else {
                PERSISTENCE_MODE_FULL_SNAPSHOT
            });
    if persistence_mode == PERSISTENCE_MODE_SHARED_FILES {
        let operation_id = start_agent_operation(&state, &record, "rollback").await;
        let store = state.agenthub_store.as_ref().ok_or_else(|| {
            AppError::BadRequest("AgentHub database persistence is not configured".to_string())
        })?;
        let snapshot = store
            .get_snapshot(&agent_id, &snapshot_id)
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to load snapshot: {}", e)))?
            .ok_or_else(|| AppError::BadRequest("snapshot not found".to_string()))?;
        let source_path = snapshot
            .openclaw_state_snapshot_path
            .as_deref()
            .filter(|path| FsPath::new(path).is_dir())
            .ok_or_else(|| {
                AppError::BadRequest(
                    "selected snapshot does not contain OpenClaw host state".to_string(),
                )
            })?;
        let target_path = record
            .openclaw_state_path
            .as_deref()
            .filter(|path| FsPath::new(path).is_dir())
            .ok_or_else(|| {
                AppError::BadRequest(
                    "current assistant does not have an OpenClaw host state directory".to_string(),
                )
            })?;
        if let Err(err) = copy_openclaw_state_dir(source_path, target_path) {
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
        let rootfs_snapshot_id = snapshot
            .rootfs_snapshot_id
            .clone()
            .or(snapshot.rootfs_source_id.clone())
            .unwrap_or_else(|| record.template_id.clone());
        let _ = store
            .set_base_snapshot_id(&agent_id, &rootfs_snapshot_id)
            .await;
        let _ = store
            .set_snapshot_healthy(&agent_id, &snapshot_id, true)
            .await;
        finish_agent_operation(
            &state,
            operation_id.as_deref(),
            "succeeded",
            Some(&snapshot_id),
            None,
        )
        .await;
        return Ok((
            StatusCode::OK,
            Json(RollbackResponse {
                sandbox_id: record.sandbox_id,
                snapshot_id,
                operation_id: operation_id.unwrap_or_default(),
                status: "succeeded".to_string(),
            }),
        ));
    }

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
    let has_openclaw_host_mount = record
        .openclaw_state_path
        .as_deref()
        .is_some_and(|source_path| FsPath::new(source_path).is_dir());
    let persistence_mode =
        record
            .persistence_mode
            .as_deref()
            .unwrap_or(if has_openclaw_host_mount {
                PERSISTENCE_MODE_SHARED_FILES
            } else {
                PERSISTENCE_MODE_FULL_SNAPSHOT
            });
    let shared_files = persistence_mode == PERSISTENCE_MODE_SHARED_FILES;
    let requested_snapshot_id = body
        .snapshot_id
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty());
    let requested_snapshot_record =
        if let (Some(store), Some(snapshot_id)) = (&state.agenthub_store, requested_snapshot_id) {
            store
                .get_snapshot(&agent_id, snapshot_id)
                .await
                .ok()
                .flatten()
        } else {
            None
        };
    let snapshot_id = match requested_snapshot_id {
        Some(snapshot_id) => requested_snapshot_record
            .as_ref()
            .and_then(|snapshot| snapshot.rootfs_snapshot_id.clone())
            .unwrap_or_else(|| snapshot_id.to_string()),
        None if shared_files => record
            .base_snapshot_id
            .clone()
            .or_else(|| record.rootfs_source_id.clone())
            .unwrap_or_else(|| record.template_id.clone()),
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
    let mut llm_config = resolve_llm_config(&state).await?;
    llm_config.model = normalize_agent_model(&record.model)
        .map(|(_, model_id)| model_id.to_string())
        .unwrap_or_else(|| record.model.clone());
    let network_config = agenthub_network_config(&llm_config)?;
    let openclaw_persist = if shared_files {
        let persist_id = new_openclaw_persist_id();
        let state_path = prepare_openclaw_state_dir(&persist_id)?;
        let source_openclaw_state_path = requested_snapshot_record
            .as_ref()
            .and_then(|snapshot| snapshot.openclaw_state_snapshot_path.as_deref())
            .or(record.openclaw_state_path.as_deref());
        let copied_state = source_openclaw_state_path
            .as_ref()
            .is_some_and(|source_path| FsPath::new(source_path).is_dir());
        if let Some(source_path) = source_openclaw_state_path.filter(|_| copied_state) {
            copy_openclaw_state_dir(source_path, &state_path)?;
        }
        let mount_metadata = openclaw_host_mount_metadata(&state_path)?;
        Some((persist_id, state_path, mount_metadata, copied_state))
    } else {
        None
    };
    let mut metadata = HashMap::from([
        ("agenthub".to_string(), "true".to_string()),
        ("agenthub.name".to_string(), clone_name.clone()),
        ("agenthub.engine".to_string(), record.engine.clone()),
        (
            "agenthub.persistence_mode".to_string(),
            persistence_mode.to_string(),
        ),
        (
            "agenthub.rootfs_source_type".to_string(),
            ROOTFS_SOURCE_SNAPSHOT.to_string(),
        ),
        ("agenthub.rootfs_source_id".to_string(), snapshot_id.clone()),
        (
            "agenthub.clone.source".to_string(),
            record.sandbox_id.clone(),
        ),
    ]);
    if let Some((persist_id, _, mount_metadata, _)) = openclaw_persist.as_ref() {
        metadata.insert(
            "agenthub.openclaw.persist_id".to_string(),
            persist_id.clone(),
        );
        metadata.insert(HOSTDIR_MOUNT_KEY.to_string(), mount_metadata.clone());
    }
    let created = state
        .services
        .sandboxes
        .create_sandbox(NewSandbox {
            template_id: snapshot_id.clone(),
            timeout,
            lifecycle: None,
            secure: None,
            allow_internet_access: Some(true),
            network: network_config,
            metadata: Some(metadata),
            distribution_scope: agenthub_create_distribution_scope(
                persistence_mode,
                ROOTFS_SOURCE_SNAPSHOT,
            ),
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
    let copied_openclaw_state = openclaw_persist
        .as_ref()
        .is_some_and(|(_, _, _, copied)| *copied);
    // A copied/full-snapshot sandbox already carries OpenClaw state, so we only
    // merge LLM settings and keep its gateway token; a fresh shared-files mount
    // needs a full init with a new token.
    let has_openclaw_state = copied_openclaw_state || !shared_files;
    let gateway_token = if has_openclaw_state {
        record.gateway_token.clone()
    } else {
        Some(new_gateway_token())
    };
    let plan = LlmRuntimePlan::resolve(&llm_config, &llm_config.model);
    let apply_options = if has_openclaw_state {
        OpenClawApplyOptions::merge_llm()
    } else {
        OpenClawApplyOptions::full_init(gateway_token.clone())
    };
    let setup_result =
        apply_openclaw_runtime(&state, &sandbox_id, &domain, &plan, &apply_options).await;
    let setup = match setup_result {
        Ok(setup) => setup,
        Err(err) => {
            let _ = state.services.sandboxes.kill_sandbox(&sandbox_id).await;
            finish_agent_operation_blocking(
                &state,
                operation_id.as_deref(),
                "failed",
                Some(&sandbox_id),
                Some(&err.to_string()),
            );
            return Err(err);
        }
    };
    tokio::time::sleep(Duration::from_secs(5)).await;
    let host_gateway_token = openclaw_persist
        .as_ref()
        .and_then(|(_, state_path, _, _)| read_openclaw_gateway_token_from_host(state_path));
    let sandbox_gateway_token = read_openclaw_gateway_token(&state, &sandbox_id, &domain)
        .await
        .unwrap_or(None);
    let gateway_token = host_gateway_token
        .or(sandbox_gateway_token)
        .or(gateway_token);
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
        template_id: snapshot_id.clone(),
        gateway_url,
        env_url,
        persistence_mode: Some(persistence_mode.to_string()),
        rootfs_source_type: Some(ROOTFS_SOURCE_SNAPSHOT.to_string()),
        rootfs_source_id: Some(snapshot_id),
        openclaw_persist_id: openclaw_persist
            .as_ref()
            .map(|(persist_id, _, _, _)| persist_id.clone()),
        openclaw_state_path: openclaw_persist
            .as_ref()
            .map(|(_, state_path, _, _)| state_path.clone()),
        wecom_config: match (record.wecom_bot_id.clone(), record.wecom_bot_secret.clone()) {
            (Some(bot_id), Some(bot_secret)) => Some(AgentWeComConfig { bot_id, bot_secret }),
            _ => None,
        },
        setup: Some(setup),
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
            let has_openclaw_host_mount = record
                .openclaw_state_path
                .as_deref()
                .is_some_and(|source_path| FsPath::new(source_path).is_dir());
            let persistence_mode =
                record
                    .persistence_mode
                    .as_deref()
                    .unwrap_or(if has_openclaw_host_mount {
                        PERSISTENCE_MODE_SHARED_FILES
                    } else {
                        PERSISTENCE_MODE_FULL_SNAPSHOT
                    });
            if persistence_mode == PERSISTENCE_MODE_SHARED_FILES {
                let store = state.agenthub_store.as_ref().ok_or_else(|| {
                    AppError::BadRequest(
                        "AgentHub database persistence is not configured".to_string(),
                    )
                })?;
                let source_openclaw_path = record
                    .openclaw_state_path
                    .as_deref()
                    .filter(|path| FsPath::new(path).is_dir())
                    .ok_or_else(|| {
                        AppError::BadRequest(
                            "current assistant does not have an OpenClaw host state directory"
                                .to_string(),
                        )
                    })?;
                let snapshot_id = new_agenthub_snapshot_id();
                let snapshot_path = openclaw_host_snapshot_path(&snapshot_id);
                copy_openclaw_state_dir(source_openclaw_path, &snapshot_path)?;
                let rootfs_snapshot_id = record
                    .base_snapshot_id
                    .clone()
                    .or_else(|| record.rootfs_source_id.clone())
                    .unwrap_or_else(|| record.template_id.clone());
                let rootfs_source_type = record
                    .rootfs_source_type
                    .as_deref()
                    .unwrap_or(ROOTFS_SOURCE_TEMPLATE);
                store
                    .upsert_agenthub_openclaw_snapshot(
                        &agent_id,
                        &record.sandbox_id,
                        &snapshot_id,
                        body.name.as_deref(),
                        rootfs_source_type,
                        &rootfs_snapshot_id,
                        &rootfs_snapshot_id,
                        &snapshot_path,
                    )
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
                            "failed to save AgentHub OpenClaw snapshot: {}",
                            e
                        ))
                    })?;
                snapshot_id
            } else {
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
                r#"set -e
kill_openclaw_listeners() {
  python3 - <<'PY'
import os, pathlib, signal, time
port = int(os.environ.get("OPENCLAW_PORT", "18789"))
port_hex = f"{port:04X}"
inodes = set()
for name in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        for line in pathlib.Path(name).read_text().splitlines()[1:]:
            cols = line.split()
            if cols[1].rsplit(":", 1)[-1].upper() == port_hex and cols[3] == "0A":
                inodes.add(cols[9])
    except Exception:
        pass
pids = set()
for pid in filter(str.isdigit, os.listdir("/proc")):
    fd_dir = f"/proc/{pid}/fd"
    try:
        for fd in os.listdir(fd_dir):
            try:
                target = os.readlink(f"{fd_dir}/{fd}")
            except Exception:
                continue
            if target.startswith("socket:[") and target[8:-1] in inodes:
                pids.add(int(pid))
    except Exception:
        pass
for sig in (signal.SIGTERM, signal.SIGKILL):
    for pid in sorted(pids):
        if pid == os.getpid():
            continue
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            pass
        except Exception:
            pass
    time.sleep(0.5)
PY
}
restart_openclaw_service() {
  if [ -n "${OPENCLAW_NODE_EXTRA_CA_CERTS:-}" ] && [ -f "${OPENCLAW_NODE_EXTRA_CA_CERTS}" ]; then
    export NODE_EXTRA_CA_CERTS="${OPENCLAW_NODE_EXTRA_CA_CERTS}"
  elif [ -f "/root/.openclaw/cube-egress-ca.crt" ]; then
    export NODE_EXTRA_CA_CERTS="/root/.openclaw/cube-egress-ca.crt"
  fi
  if command -v supervisorctl >/dev/null 2>&1; then
    supervisorctl restart openclaw
  else
    pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
    pkill -f 'node .*openclaw' 2>/dev/null || true
    kill_openclaw_listeners
    mkdir -p /var/log
    if command -v openclaw >/dev/null 2>&1; then
      nohup openclaw gateway run >/var/log/openclaw.log 2>&1 &
    elif [ -x /opt/openclaw/openclaw ]; then
      nohup /opt/openclaw/openclaw gateway run >/var/log/openclaw.log 2>&1 &
    elif [ -f /opt/openclaw/package.json ] && command -v npm >/dev/null 2>&1; then
      (cd /opt/openclaw && nohup npm start >/var/log/openclaw.log 2>&1 &)
    elif [ -f /app/package.json ] && command -v npm >/dev/null 2>&1; then
      (cd /app && nohup npm start >/var/log/openclaw.log 2>&1 &)
    elif [ -f /opt/openclaw/package.json ] && command -v pnpm >/dev/null 2>&1; then
      (cd /opt/openclaw && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
    elif [ -f /app/package.json ] && command -v pnpm >/dev/null 2>&1; then
      (cd /app && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
    else
      echo "Neither supervisorctl nor a direct OpenClaw startup command was found" >&2
      return 127
    fi
  fi
}
openclaw_ready() {
  python3 - <<'PY'
import json, os, socket, sys
try:
    token = json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", "")
    port = int(os.environ.get("OPENCLAW_PORT", "18789"))
    if not token:
        sys.exit(1)
    s = socket.create_connection(("127.0.0.1", port), timeout=0.5)
    s.close()
except Exception:
    sys.exit(1)
PY
}
restart_openclaw_service
for i in $(seq 1 30); do
  if openclaw_ready; then
    if command -v supervisorctl >/dev/null 2>&1; then
      supervisorctl status openclaw
    elif command -v ps >/dev/null 2>&1; then
      ps -ef | grep -E '[o]penclaw|node .*openclaw' || true
    fi
    exit 0
  fi
  sleep 0.5
done
[ -f /var/log/openclaw.log ] && tail -80 /var/log/openclaw.log >&2 || true
exit 1"#
            ],
            "envs": {
                "NODE_EXTRA_CA_CERTS": OPENCLAW_NODE_EXTRA_CA_CERTS,
                "OPENCLAW_NODE_EXTRA_CA_CERTS": OPENCLAW_NODE_EXTRA_CA_CERTS,
            },
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
if command -v supervisorctl >/dev/null 2>&1; then
  supervisorctl restart openclaw
else
  pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
  pkill -f 'node .*openclaw' 2>/dev/null || true
  mkdir -p /var/log
  if command -v openclaw >/dev/null 2>&1; then
    nohup openclaw gateway run >/var/log/openclaw.log 2>&1 &
  elif [ -x /opt/openclaw/openclaw ]; then
    nohup /opt/openclaw/openclaw gateway run >/var/log/openclaw.log 2>&1 &
  elif [ -f /opt/openclaw/package.json ] && command -v npm >/dev/null 2>&1; then
    (cd /opt/openclaw && nohup npm start >/var/log/openclaw.log 2>&1 &)
  elif [ -f /app/package.json ] && command -v npm >/dev/null 2>&1; then
    (cd /app && nohup npm start >/var/log/openclaw.log 2>&1 &)
  else
    echo "Neither supervisorctl nor a direct OpenClaw startup command was found" >&2
    exit 127
  fi
fi
for i in $(seq 1 30); do
  if python3 - <<'PY'
import json, os, socket, sys
try:
    token = json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", "")
    port = int(os.environ.get("OPENCLAW_PORT", "18789"))
    if not token:
        sys.exit(1)
    s = socket.create_connection(("127.0.0.1", port), timeout=0.5)
    s.close()
except Exception:
    sys.exit(1)
PY
  then
    if command -v supervisorctl >/dev/null 2>&1; then supervisorctl status openclaw; else ps -ef | grep -E '[o]penclaw|node .*openclaw' || true; fi
    break
  fi
  sleep 0.5
done
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
    State(_state): State<AppState>,
    Path(_agent_id): Path<String>,
    Json(_body): Json<UpdateAgentModelRequest>,
) -> AppResult<impl IntoResponse> {
    // Changing a model on a running instance is intentionally not supported in
    // this round: the model namespace is resolved once when the sandbox is
    // provisioned. Create a new instance (or clone) to switch models.
    Err::<axum::Json<()>, _>(AppError::NotImplemented(
        "updating an AgentHub instance's model at runtime is not supported; create or clone an instance instead".to_string(),
    ))
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

    let llm_config = resolve_llm_config(&state).await?;
    let plan = LlmRuntimePlan::resolve(&llm_config, &llm_config.model);
    // Reconfiguring the WeCom channel rewrites the full OpenClaw config, but the
    // existing gateway token MUST be preserved or the stored gateway URL breaks.
    let apply_options = OpenClawApplyOptions {
        mode: OpenClawApplyMode::FullInit,
        gateway_token: record.gateway_token.clone(),
        preserve_gateway_token: true,
        configure_wecom: true,
        bot_id: Some(bot_id.to_string()),
        bot_secret: Some(bot_secret.to_string()),
    };
    let setup = apply_openclaw_runtime(
        &state,
        &record.sandbox_id,
        &record.domain,
        &plan,
        &apply_options,
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
            Some(AgentWeComConfig {
                bot_id,
                bot_secret: crate::crypto::decrypt_or_passthrough(&bot_secret),
            })
        }
        _ => read_openclaw_wecom_config(&state, &record.sandbox_id, &record.domain)
            .await
            .unwrap_or(None),
    };

    Ok((StatusCode::OK, Json(config)))
}

/// How `apply_openclaw_runtime` should treat the existing on-disk OpenClaw
/// state in the sandbox.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum OpenClawApplyMode {
    /// Write the full OpenClaw structure (gateway/workspace/plugins + LLM).
    /// Used for fresh sandboxes, app-market templates and shared-files mounts
    /// without bundled state.
    FullInit,
    /// Only merge the LLM-related blocks into existing config, preserving the
    /// gateway (and its token). Used when the sandbox already carries OpenClaw
    /// state (published template fast path / clone from snapshot).
    MergeLlm,
}

/// Per-call knobs for the unified OpenClaw apply path. Everything the sandbox
/// script needs to decide behaviour is captured here in Rust.
struct OpenClawApplyOptions {
    mode: OpenClawApplyMode,
    /// Explicit gateway token to write. When `None` in `FullInit`, the script
    /// reuses the existing token if `preserve_gateway_token` is set, otherwise
    /// it generates a fresh one.
    gateway_token: Option<String>,
    preserve_gateway_token: bool,
    configure_wecom: bool,
    bot_id: Option<String>,
    bot_secret: Option<String>,
}

impl OpenClawApplyOptions {
    fn full_init(gateway_token: Option<String>) -> Self {
        Self {
            mode: OpenClawApplyMode::FullInit,
            gateway_token,
            preserve_gateway_token: false,
            configure_wecom: false,
            bot_id: None,
            bot_secret: None,
        }
    }

    fn merge_llm() -> Self {
        Self {
            mode: OpenClawApplyMode::MergeLlm,
            gateway_token: None,
            preserve_gateway_token: true,
            configure_wecom: false,
            bot_id: None,
            bot_secret: None,
        }
    }
}

/// Renders the structured config patch handed to the sandbox apply script.
/// This is pure so it can be unit-tested without a sandbox.
fn openclaw_apply_spec(plan: &LlmRuntimePlan, options: &OpenClawApplyOptions) -> Value {
    let mode = match options.mode {
        OpenClawApplyMode::FullInit => "full_init",
        OpenClawApplyMode::MergeLlm => "merge_llm",
    };
    serde_json::json!({
        "mode": mode,
        "provider": plan.upstream_provider,
        "baseUrl": plan.upstream_base_url,
        "apiKey": plan.openclaw_api_key,
        "openclawPrimary": plan.openclaw_primary,
        "upstreamModelId": plan.upstream_model_id,
        "modelName": plan.openclaw_model_name,
        "credentialMode": plan.credential_mode,
        "configureWecom": options.configure_wecom,
        "gateway": {
            "manage": options.mode == OpenClawApplyMode::FullInit,
            "token": options.gateway_token,
            "preserveExisting": options.preserve_gateway_token,
        },
    })
}

/// Single entry point for writing an OpenClaw runtime into a sandbox.
///
/// Rust resolves the entire configuration (`LlmRuntimePlan` + options), encodes
/// it as a JSON patch and hands it to a thin, branch-free sandbox script that
/// only writes files, restarts the gateway and probes readiness.
async fn apply_openclaw_runtime(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    plan: &LlmRuntimePlan,
    options: &OpenClawApplyOptions,
) -> AppResult<AgentSetupResult> {
    let spec = openclaw_apply_spec(plan, options);
    let spec_b64 = BASE64.encode(serde_json::to_vec(&spec).map_err(anyhow::Error::from)?);

    let mut envs = serde_json::Map::from_iter([
        ("OPENCLAW_APPLY_SPEC".to_string(), Value::String(spec_b64)),
        (
            "OPENCLAW_ALLOWED_ORIGINS".to_string(),
            Value::String("*".to_string()),
        ),
        (
            "CUBE_EGRESS_CA_PEM".to_string(),
            Value::String(egress_ca_pem()),
        ),
        (
            "NODE_EXTRA_CA_CERTS".to_string(),
            Value::String(OPENCLAW_NODE_EXTRA_CA_CERTS.to_string()),
        ),
        (
            "OPENCLAW_NODE_EXTRA_CA_CERTS".to_string(),
            Value::String(OPENCLAW_NODE_EXTRA_CA_CERTS.to_string()),
        ),
        (
            "CUBE_SANDBOX_NODE_IP".to_string(),
            Value::String(std::env::var("CUBE_SANDBOX_NODE_IP").unwrap_or_default()),
        ),
    ]);
    if options.configure_wecom {
        if let Some(v) = options.bot_id.as_deref().filter(|v| !v.trim().is_empty()) {
            envs.insert("OPENCLAW_BOT_ID".to_string(), Value::String(v.to_string()));
        }
        if let Some(v) = options
            .bot_secret
            .as_deref()
            .filter(|v| !v.trim().is_empty())
        {
            envs.insert(
                "OPENCLAW_BOT_SECRET".to_string(),
                Value::String(v.to_string()),
            );
        }
    }

    let req = serde_json::json!({
        "process": {
            "cmd": "/bin/bash",
            "args": ["-l", "-c", openclaw_apply_script()],
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
            "OpenClaw runtime apply failed with exit code {}: {}",
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
                "python3 - <<'PY'\nimport json\nfrom pathlib import Path\nfor path in (Path('/root/.openclaw/agenthub-wecom.json'), Path('/root/.openclaw/openclaw.json')):\n    try:\n        data = json.load(open(path))\n        wecom = data if path.name == 'agenthub-wecom.json' else (data.get('channels', {}).get('wecom') or {})\n        bot_id = wecom.get('botId') or ''\n        secret = wecom.get('secret') or ''\n        if bot_id and secret:\n            print(json.dumps({'botId': bot_id, 'botSecret': secret}))\n            break\n    except Exception:\n        pass\nPY"
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
        DEEPSEEK_CHAT_MODEL | DEEPSEEK_CHAT_MODEL_LABEL => {
            Some((DEEPSEEK_CHAT_MODEL_LABEL, DEEPSEEK_CHAT_MODEL))
        }
        DEEPSEEK_V4_FLASH_MODEL | DEEPSEEK_V4_FLASH_MODEL_LABEL => {
            Some((DEEPSEEK_V4_FLASH_MODEL_LABEL, DEEPSEEK_V4_FLASH_MODEL))
        }
        DEEPSEEK_V4_PRO_MODEL | DEEPSEEK_V4_PRO_MODEL_LABEL => {
            Some((DEEPSEEK_V4_PRO_MODEL_LABEL, DEEPSEEK_V4_PRO_MODEL))
        }
        _ => None,
    }
}

fn model_display_name(model: &str) -> String {
    match model {
        DEEPSEEK_V4_PRO_MODEL => DEEPSEEK_V4_PRO_MODEL_LABEL.to_string(),
        DEEPSEEK_V4_FLASH_MODEL => DEEPSEEK_V4_FLASH_MODEL_LABEL.to_string(),
        DEEPSEEK_CHAT_MODEL => DEEPSEEK_CHAT_MODEL_LABEL.to_string(),
        other => other
            .rsplit('/')
            .next()
            .filter(|v| !v.is_empty())
            .unwrap_or(other)
            .to_string(),
    }
}

/// Thin, branch-free OpenClaw apply script run inside the sandbox.
///
/// All business logic (model namespace, provider, credential mode, gateway
/// token policy, wecom binding) is decided in Rust and handed in as a base64
/// JSON patch via `OPENCLAW_APPLY_SPEC`. This script only writes files,
/// restarts the gateway and probes readiness.
fn openclaw_apply_script() -> &'static str {
    r#"kill_openclaw_listeners() {
           python3 - <<'PY'
import os, pathlib, signal, time
port = int(os.environ.get("OPENCLAW_PORT", "18789"))
port_hex = f"{port:04X}"
inodes = set()
for name in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        for line in pathlib.Path(name).read_text().splitlines()[1:]:
            cols = line.split()
            if cols[1].rsplit(":", 1)[-1].upper() == port_hex and cols[3] == "0A":
                inodes.add(cols[9])
    except Exception:
        pass
pids = set()
for pid in filter(str.isdigit, os.listdir("/proc")):
    fd_dir = f"/proc/{pid}/fd"
    try:
        for fd in os.listdir(fd_dir):
            try:
                target = os.readlink(f"{fd_dir}/{fd}")
            except Exception:
                continue
            if target.startswith("socket:[") and target[8:-1] in inodes:
                pids.add(int(pid))
    except Exception:
        pass
for sig in (signal.SIGTERM, signal.SIGKILL):
    for pid in sorted(pids):
        if pid == os.getpid():
            continue
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            pass
        except Exception:
            pass
    time.sleep(0.5)
PY
         }
         restart_openclaw_service() {
           kill_openclaw_listeners || true
           if command -v supervisorctl >/dev/null 2>&1; then
             supervisorctl reread || true
             supervisorctl update openclaw || true
             (supervisorctl restart openclaw || supervisorctl start openclaw) || return $?
           else
             pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
             pkill -f 'node .*openclaw' 2>/dev/null || true
             mkdir -p /var/log
             if command -v openclaw >/dev/null 2>&1; then
               nohup openclaw gateway run >/var/log/openclaw.log 2>&1 &
             elif [ -x /opt/openclaw/openclaw ]; then
               nohup /opt/openclaw/openclaw gateway run >/var/log/openclaw.log 2>&1 &
             elif [ -f /opt/openclaw/package.json ] && command -v npm >/dev/null 2>&1; then
               (cd /opt/openclaw && nohup npm start >/var/log/openclaw.log 2>&1 &)
             elif [ -f /app/package.json ] && command -v npm >/dev/null 2>&1; then
               (cd /app && nohup npm start >/var/log/openclaw.log 2>&1 &)
             elif [ -f /opt/openclaw/package.json ] && command -v pnpm >/dev/null 2>&1; then
               (cd /opt/openclaw && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
             elif [ -f /app/package.json ] && command -v pnpm >/dev/null 2>&1; then
               (cd /app && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
             else
               echo "Neither supervisorctl nor a direct OpenClaw startup command was found" >&2
               return 127
             fi
           fi
         }
         openclaw_ready() {
           python3 - <<'PY'
import json, os, socket, sys
try:
    token = json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", "")
    port = int(os.environ.get("OPENCLAW_PORT", "18789"))
    if not token:
        sys.exit(1)
    s = socket.create_connection(("127.0.0.1", port), timeout=0.5)
    s.close()
except Exception:
    sys.exit(1)
PY
         }
         openclaw_status() {
           if command -v supervisorctl >/dev/null 2>&1; then
             supervisorctl status openclaw || true
           else
             ps -ef | grep -E '[o]penclaw|node .*openclaw' || true
             [ -f /var/log/openclaw.log ] && tail -40 /var/log/openclaw.log || true
           fi
         }
         install_wecom_plugin_if_needed() {
           if [ -n "${OPENCLAW_BOT_ID:-}" ] && [ -n "${OPENCLAW_BOT_SECRET:-}" ]; then
             if command -v openclaw >/dev/null 2>&1; then
               export NODE_EXTRA_CA_CERTS="${NODE_EXTRA_CA_CERTS:-/root/.openclaw/cube-egress-ca.crt}"
               openclaw plugins inspect wecom-openclaw-plugin >/dev/null 2>&1 || \
                 openclaw plugins install @wecom/wecom-openclaw-plugin@2026.5.7
             fi
           fi
         }
         (command -v supervisorctl >/dev/null 2>&1 && supervisorctl stop openclaw || true) && \
         install_wecom_plugin_if_needed && \
         cat >/tmp/agenthub-openclaw-apply.py <<'PY'
import base64, json, os, secrets
from datetime import datetime, timezone
from pathlib import Path

spec = json.loads(base64.b64decode(os.environ["OPENCLAW_APPLY_SPEC"]))
mode = spec["mode"]
provider = spec["provider"]
base_url = spec["baseUrl"].strip().rstrip("/")
api_key = spec["apiKey"]
openclaw_primary = spec["openclawPrimary"]
model_id = spec["upstreamModelId"]
model_name = spec["modelName"]
configure_wecom = bool(spec.get("configureWecom"))
gateway_spec = spec.get("gateway", {})
auth_profile = f"{provider}:default"

config_path = Path("/root/.openclaw/openclaw.json")
agent_dir = Path("/root/.openclaw/agents/main/agent")
workspace = Path("/root/.openclaw/workspace")
sessions = Path("/root/.openclaw/agents/main/sessions")
config_path.parent.mkdir(parents=True, exist_ok=True)
agent_dir.mkdir(parents=True, exist_ok=True)

ca_pem = os.environ.get("CUBE_EGRESS_CA_PEM", "").strip()
ca_path = Path(os.environ.get("OPENCLAW_NODE_EXTRA_CA_CERTS", "/root/.openclaw/cube-egress-ca.crt"))
if ca_pem:
    ca_path.parent.mkdir(parents=True, exist_ok=True)
    ca_path.write_text(ca_pem + ("\n" if not ca_pem.endswith("\n") else ""))
    os.environ["NODE_EXTRA_CA_CERTS"] = str(ca_path)

try:
    data = json.loads(config_path.read_text())
except Exception:
    data = {}
if not isinstance(data, dict):
    data = {}

# LLM blocks are written identically in both modes. Rebuilding models from
# scratch drops stale provider namespaces left by earlier configurations.
data["models"] = {
    "mode": "merge",
    "providers": {
        provider: {
            "baseUrl": base_url,
            "api": "openai-completions",
            "models": [{
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
            }],
        }
    },
}

agents = data.setdefault("agents", {}).setdefault("defaults", {})
agents["model"] = {"primary": openclaw_primary}
agents["models"] = {openclaw_primary: {"alias": model_name}}

plugins = data.setdefault("plugins", {}).setdefault("entries", {})
# A provider is not a plugin. Older builds registered the provider name here,
# which OpenClaw reports as "plugin not found"; drop that stale entry.
plugins.pop(provider, None)
data["auth"] = {"profiles": {auth_profile: {"provider": provider, "mode": "api_key"}}}

if mode == "full_init":
    workspace.mkdir(parents=True, exist_ok=True)
    sessions.mkdir(parents=True, exist_ok=True)
    agents["workspace"] = str(workspace)
    if gateway_spec.get("manage"):
        gateway = data.setdefault("gateway", {})
        existing = gateway.get("auth", {}).get("token", "") or ""
        token = (gateway_spec.get("token") or "").strip()
        if not token and gateway_spec.get("preserveExisting") and existing:
            token = existing
        if not token:
            token = secrets.token_hex(16)
        gateway["bind"] = os.environ.get("OPENCLAW_BIND", "auto")
        gateway["port"] = int(os.environ.get("OPENCLAW_PORT", "18789"))
        gateway["mode"] = "local"
        gateway["tailscale"] = {"mode": "off", "resetOnExit": False}
        gateway["auth"] = {"mode": "token", "token": token}
        trusted_proxies = [
            "169.254.68.5",
            "169.254.68.0/24",
            os.environ.get("CUBE_SANDBOX_NODE_IP", "").strip(),
            "127.0.0.1",
            "::1",
        ]
        gateway["trustedProxies"] = [v for v in trusted_proxies if v]
        origins = os.environ.get("OPENCLAW_ALLOWED_ORIGINS", "*")
        gateway["controlUi"] = {
            "allowedOrigins": [o.strip() for o in origins.split(",") if o.strip()],
            "dangerouslyDisableDeviceAuth": os.environ.get("OPENCLAW_DISABLE_DEVICE_AUTH", "true").lower() == "true",
            "allowInsecureAuth": os.environ.get("OPENCLAW_ALLOW_INSECURE_AUTH", "true").lower() == "true",
            "dangerouslyAllowHostHeaderOriginFallback": os.environ.get("OPENCLAW_ALLOW_HOST_HEADER_ORIGIN_FALLBACK", "true").lower() == "true",
        }
        token_file = Path(os.environ.get("OPENCLAW_TOKEN_FILE", "/var/log/openclaw.token"))
        token_file.parent.mkdir(parents=True, exist_ok=True)
        token_file.write_text(token + "\n")
    data["session"] = {"dmScope": "per-channel-peer"}
    tools = data.setdefault("tools", {})
    tools["profile"] = "full"
    data["skills"] = {"install": {"nodeManager": "npm"}}
    data["meta"] = {
        "lastTouchedVersion": data.get("meta", {}).get("lastTouchedVersion", "2026.5.7"),
        "lastTouchedAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    if configure_wecom:
        plugins["wecom-openclaw-plugin"] = {"enabled": True}
        tools["alsoAllow"] = sorted(set(tools.get("alsoAllow", []) + ["wecom_mcp"]))
        channels = data.setdefault("channels", {})
        channels["wecom"] = {
            "enabled": True,
            "connectionMode": "websocket",
            "botId": os.environ["OPENCLAW_BOT_ID"],
            "secret": os.environ["OPENCLAW_BOT_SECRET"],
            "name": "企业微信",
        }
        # Keep a small AgentHub-owned copy so the backend can return/edit the
        # binding without parsing plugin-specific channel config.
        wecom_path = config_path.parent / "agenthub-wecom.json"
        wecom_path.write_text(json.dumps({
            "botId": os.environ["OPENCLAW_BOT_ID"],
            "secret": os.environ["OPENCLAW_BOT_SECRET"],
            "enabled": True,
        }, ensure_ascii=False, indent=2) + "\n")

tmp = config_path.with_suffix(".json.tmp")
tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n")
tmp.replace(config_path)

(agent_dir / "auth-profiles.json").write_text(json.dumps({
    "version": 1,
    "profiles": {
        auth_profile: {
            "type": "api_key",
            "provider": provider,
            "key": api_key,
        }
    },
}, ensure_ascii=False, indent=2) + "\n")
(agent_dir / "models.json").write_text(json.dumps(data["models"], ensure_ascii=False, indent=2) + "\n")

supervisor_conf = Path("/opt/gem/supervisord/openclaw.conf")
if supervisor_conf.exists():
    lines = supervisor_conf.read_text().splitlines()
    ca_env = f',NODE_EXTRA_CA_CERTS="{ca_path}"' if ca_pem else ""
    env_line = f'environment=NODE_ENV="production",OPENCLAW_DEFAULT_MODEL="{openclaw_primary}"{ca_env}'
    for idx, line in enumerate(lines):
        if line.startswith("environment="):
            lines[idx] = env_line
            break
    else:
        lines.append(env_line)
    supervisor_conf.write_text("\n".join(lines) + "\n")

print("Applied ~/.openclaw/openclaw.json")
PY
         python3 /tmp/agenthub-openclaw-apply.py && \
         restart_openclaw_service && \
         for i in $(seq 1 30); do \
           if openclaw_ready; then \
             openclaw_status; \
             break; \
           fi; \
           sleep 0.5; \
         done && \
         openclaw_ready
"#
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

#[cfg(test)]
mod tests {
    use super::*;

    fn llm(provider: &str, credential_mode: &str) -> LlmConfig {
        LlmConfig {
            provider: provider.to_string(),
            base_url: "https://upstream.example.com".to_string(),
            model: "deepseek/deepseek-v4-flash".to_string(),
            api_key: "sk-real-secret".to_string(),
            credential_mode: credential_mode.to_string(),
        }
    }

    #[test]
    fn model_suffix_strips_only_the_first_prefix() {
        assert_eq!(
            openclaw_model_suffix("deepseek-v4-flash"),
            "deepseek-v4-flash"
        );
        assert_eq!(
            openclaw_model_suffix("deepseek/deepseek-v4-flash"),
            "deepseek-v4-flash"
        );
        assert_eq!(openclaw_model_suffix("a/b/c"), "b/c");
        // A trailing slash leaves nothing useful, so fall back to the input.
        assert_eq!(openclaw_model_suffix("deepseek/"), "deepseek/");
    }

    #[test]
    fn custom_upstream_namespaces_primary_under_its_provider() {
        // Reproduces the reported bug: a custom provider with a prefixed model
        // must resolve to `{provider}/{suffix}`, never `openai/...`.
        let plan =
            LlmRuntimePlan::resolve(&llm("openai-compatible", "egress"), "deepseek-v4-flash");
        assert_eq!(plan.upstream_model_id, "deepseek-v4-flash");
        assert_eq!(plan.openclaw_primary, "openai-compatible/deepseek-v4-flash");
        assert_eq!(plan.upstream_provider, "openai-compatible");

        // A stale prefix on the input is dropped and re-namespaced.
        let plan = LlmRuntimePlan::resolve(
            &llm("openai-compatible", "egress"),
            "deepseek/deepseek-v4-flash",
        );
        assert_eq!(plan.upstream_model_id, "deepseek-v4-flash");
        assert_eq!(plan.openclaw_primary, "openai-compatible/deepseek-v4-flash");
        assert_eq!(plan.public_model, "deepseek/deepseek-v4-flash");
    }

    #[test]
    fn official_deepseek_model_keeps_its_namespace() {
        let plan =
            LlmRuntimePlan::resolve(&llm("deepseek", "egress"), "deepseek/deepseek-v4-flash");
        assert_eq!(plan.openclaw_primary, "deepseek/deepseek-v4-flash");
        assert_eq!(plan.openclaw_model_name, DEEPSEEK_V4_FLASH_MODEL_LABEL);
    }

    #[test]
    fn egress_mode_hides_the_real_key_from_openclaw() {
        let plan = LlmRuntimePlan::resolve(&llm("deepseek", "egress"), "deepseek-v4-flash");
        assert_eq!(plan.openclaw_api_key, OPENCLAW_EGRESS_MANAGED_KEY);
        assert_eq!(plan.credential_mode, "egress");

        let plan = LlmRuntimePlan::resolve(&llm("deepseek", "env"), "deepseek-v4-flash");
        assert_eq!(plan.openclaw_api_key, "sk-real-secret");
    }

    #[test]
    fn full_init_spec_manages_gateway_with_explicit_token() {
        let plan =
            LlmRuntimePlan::resolve(&llm("openai-compatible", "egress"), "deepseek-v4-flash");
        let options = OpenClawApplyOptions::full_init(Some("tok-123".to_string()));
        let spec = openclaw_apply_spec(&plan, &options);
        assert_eq!(spec["mode"], "full_init");
        assert_eq!(spec["gateway"]["manage"], true);
        assert_eq!(spec["gateway"]["token"], "tok-123");
        assert_eq!(
            spec["openclawPrimary"],
            "openai-compatible/deepseek-v4-flash"
        );
        assert_eq!(spec["upstreamModelId"], "deepseek-v4-flash");
        // egress: the real key never reaches the sandbox patch.
        assert_eq!(spec["apiKey"], OPENCLAW_EGRESS_MANAGED_KEY);
    }

    #[test]
    fn merge_llm_spec_leaves_the_gateway_alone() {
        let plan = LlmRuntimePlan::resolve(&llm("deepseek", "env"), "deepseek-v4-flash");
        let options = OpenClawApplyOptions::merge_llm();
        let spec = openclaw_apply_spec(&plan, &options);
        assert_eq!(spec["mode"], "merge_llm");
        assert_eq!(spec["gateway"]["manage"], false);
        assert_eq!(spec["gateway"]["preserveExisting"], true);
        assert_eq!(spec["configureWecom"], false);
    }
}
