// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use serde_json::Value;
use sqlx::{mysql::MySqlPoolOptions, MySqlPool, Row};

use crate::handlers::agenthub::{AgentInstanceResponse, AgentSetupResult, AgentWeComConfig};
use crate::models::{SnapshotInfo, SnapshotListItem};

/// Setting key for the persisted AgentHub master encryption key (base64).
const SETTING_MASTER_KEY: &str = "secret_master_key";

#[derive(Clone)]
pub struct AgentHubStore {
    pool: MySqlPool,
}

pub struct AgentHubInstanceRecord {
    pub agent_id: String,
    pub sandbox_id: String,
    pub template_id: String,
    pub name: String,
    pub engine: String,
    pub env: String,
    pub model: String,
    pub version: String,
    pub status: String,
    pub bots: Vec<String>,
    pub avatar: String,
    pub avatar_tone: String,
    pub domain: String,
    pub gateway_token: Option<String>,
    pub persistence_mode: Option<String>,
    pub rootfs_source_type: Option<String>,
    pub rootfs_source_id: Option<String>,
    pub openclaw_persist_id: Option<String>,
    pub openclaw_state_path: Option<String>,
    pub wecom_bot_id: Option<String>,
    pub wecom_bot_secret: Option<String>,
    pub last_error: Option<String>,
    pub setup_exit_code: Option<i32>,
    pub base_snapshot_id: Option<String>,
}

pub struct AgentHubSnapshotRecord {
    pub snapshot_id: String,
    pub name: Option<String>,
    pub status: String,
    pub snapshot_kind: Option<String>,
    pub origin_sandbox_id: Option<String>,
    pub published_template_id: Option<String>,
    pub rootfs_source_type: Option<String>,
    pub rootfs_source_id: Option<String>,
    pub rootfs_snapshot_id: Option<String>,
    pub openclaw_state_snapshot_path: Option<String>,
    pub template_referenced: bool,
    pub is_healthy: bool,
    pub parent_snapshot_id: Option<String>,
    pub created_at: Option<String>,
    pub updated_at: Option<String>,
}

pub struct AgentHubTemplateRecord {
    pub template_id: String,
    pub name: String,
    pub source_agent_id: String,
    pub source_snapshot_id: String,
    pub source_sandbox_id: String,
    pub model: String,
    pub version: String,
    pub persistence_mode: Option<String>,
    pub recommended: bool,
    pub created_at: Option<String>,
}

pub struct AgentHubOperationRecord {
    pub operation_id: String,
    pub agent_id: String,
    pub operation_type: String,
    pub status: String,
    pub target_id: Option<String>,
    pub error_message: Option<String>,
    pub created_at: Option<String>,
    pub updated_at: Option<String>,
}

impl AgentHubStore {
    pub async fn connect(database_url: &str) -> anyhow::Result<Self> {
        let pool = MySqlPoolOptions::new()
            .max_connections(5)
            .connect(database_url)
            .await?;
        let store = Self { pool };
        store.seed_default_admin().await?;
        store.bootstrap_master_key().await?;
        Ok(store)
    }

    /// Seeds the default WebUI admin account (admin/admin) on first connect.
    ///
    /// CubeMaster owns AgentHub schema migrations through goose. CubeAPI only
    /// seeds the default WebUI account because the bcrypt hash is generated
    /// by Rust-side crypto helpers. INSERT IGNORE keeps any password the
    /// operator has already changed it to.
    async fn seed_default_admin(&self) -> anyhow::Result<()> {
        let admin_hash = crate::crypto::hash_password("admin")?;
        sqlx::query(
            r#"INSERT IGNORE INTO t_agenthub_user (username, password) VALUES ('admin', ?)"#,
        )
        .bind(&admin_hash)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// Bootstraps the AgentHub master encryption key on first startup.
    ///
    /// Generates a random key only when the row does not yet exist; concurrent
    /// CubeAPI instances converge on the single persisted value via the
    /// `INSERT IGNORE` semantics of [`Self::get_or_create_setting`]. The decoded
    /// key is then installed into process memory for the rest of the lifetime.
    async fn bootstrap_master_key(&self) -> anyhow::Result<()> {
        // Common path (already initialized): a plain read, no key generation.
        let b64 = match self.get_setting(SETTING_MASTER_KEY).await? {
            Some(existing) if !existing.trim().is_empty() => existing,
            // First start: generate a candidate and let the concurrency-safe
            // get-or-create pick the single winning value across processes.
            _ => {
                self.get_or_create_setting(
                    SETTING_MASTER_KEY,
                    &crate::crypto::generate_master_key_b64(),
                )
                .await?
            }
        };
        crate::crypto::install_master_key(&b64)?;
        Ok(())
    }

    pub async fn list_instances(&self) -> anyhow::Result<Vec<AgentHubInstanceRecord>> {
        let rows = sqlx::query(
            r#"
SELECT agent_id, sandbox_id, template_id, name, engine, env, model, version, status,
       bots, avatar, avatar_tone, domain, gateway_token,
       persistence_mode, rootfs_source_type, rootfs_source_id,
       openclaw_persist_id, openclaw_state_path,
       wecom_bot_id, wecom_bot_secret,
       last_error, setup_exit_code, base_snapshot_id
FROM t_agenthub_instance
WHERE deleted_at IS NULL
ORDER BY created_at DESC, id DESC
"#,
        )
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(|row| {
                let bots_value: Option<Value> = row.try_get("bots")?;
                Ok::<AgentHubInstanceRecord, sqlx::Error>(AgentHubInstanceRecord {
                    agent_id: row.try_get("agent_id")?,
                    sandbox_id: row.try_get("sandbox_id")?,
                    template_id: row.try_get("template_id")?,
                    name: row.try_get("name")?,
                    engine: row.try_get("engine")?,
                    env: row.try_get("env")?,
                    model: row.try_get("model")?,
                    version: row.try_get("version")?,
                    status: row.try_get("status")?,
                    bots: bots_value
                        .and_then(|v| serde_json::from_value(v).ok())
                        .unwrap_or_default(),
                    avatar: row.try_get("avatar")?,
                    avatar_tone: row.try_get("avatar_tone")?,
                    domain: row.try_get("domain")?,
                    gateway_token: row.try_get("gateway_token")?,
                    persistence_mode: row.try_get("persistence_mode")?,
                    rootfs_source_type: row.try_get("rootfs_source_type")?,
                    rootfs_source_id: row.try_get("rootfs_source_id")?,
                    openclaw_persist_id: row.try_get("openclaw_persist_id")?,
                    openclaw_state_path: row.try_get("openclaw_state_path")?,
                    wecom_bot_id: row.try_get("wecom_bot_id")?,
                    wecom_bot_secret: row
                        .try_get::<Option<String>, _>("wecom_bot_secret")?
                        .map(|v| crate::crypto::decrypt_or_passthrough(&v)),
                    last_error: row.try_get("last_error")?,
                    setup_exit_code: row.try_get("setup_exit_code")?,
                    base_snapshot_id: row.try_get("base_snapshot_id")?,
                })
            })
            .collect::<Result<Vec<_>, sqlx::Error>>()
            .map_err(anyhow::Error::from)
    }

    pub async fn get_instance(
        &self,
        agent_id: &str,
    ) -> anyhow::Result<Option<AgentHubInstanceRecord>> {
        let row = sqlx::query(
            r#"
SELECT agent_id, sandbox_id, template_id, name, engine, env, model, version, status,
       bots, avatar, avatar_tone, domain, gateway_token,
       persistence_mode, rootfs_source_type, rootfs_source_id,
       openclaw_persist_id, openclaw_state_path,
       wecom_bot_id, wecom_bot_secret,
       last_error, setup_exit_code, base_snapshot_id
FROM t_agenthub_instance
WHERE agent_id = ? AND deleted_at IS NULL
LIMIT 1
"#,
        )
        .bind(agent_id)
        .fetch_optional(&self.pool)
        .await?;

        row.map(|row| {
            let bots_value: Option<Value> = row.try_get("bots")?;
            Ok::<AgentHubInstanceRecord, sqlx::Error>(AgentHubInstanceRecord {
                agent_id: row.try_get("agent_id")?,
                sandbox_id: row.try_get("sandbox_id")?,
                template_id: row.try_get("template_id")?,
                name: row.try_get("name")?,
                engine: row.try_get("engine")?,
                env: row.try_get("env")?,
                model: row.try_get("model")?,
                version: row.try_get("version")?,
                status: row.try_get("status")?,
                bots: bots_value
                    .and_then(|v| serde_json::from_value(v).ok())
                    .unwrap_or_default(),
                avatar: row.try_get("avatar")?,
                avatar_tone: row.try_get("avatar_tone")?,
                domain: row.try_get("domain")?,
                gateway_token: row.try_get("gateway_token")?,
                persistence_mode: row.try_get("persistence_mode")?,
                rootfs_source_type: row.try_get("rootfs_source_type")?,
                rootfs_source_id: row.try_get("rootfs_source_id")?,
                openclaw_persist_id: row.try_get("openclaw_persist_id")?,
                openclaw_state_path: row.try_get("openclaw_state_path")?,
                wecom_bot_id: row.try_get("wecom_bot_id")?,
                wecom_bot_secret: row
                    .try_get::<Option<String>, _>("wecom_bot_secret")?
                    .map(|v| crate::crypto::decrypt_or_passthrough(&v)),
                last_error: row.try_get("last_error")?,
                setup_exit_code: row.try_get("setup_exit_code")?,
                base_snapshot_id: row.try_get("base_snapshot_id")?,
            })
        })
        .transpose()
        .map_err(anyhow::Error::from)
    }

    pub async fn upsert_instance(
        &self,
        response: &AgentInstanceResponse,
        domain: &str,
        gateway_token: Option<&str>,
    ) -> anyhow::Result<()> {
        let bots = serde_json::to_value(&response.bots)?;
        let wecom_bot_id = response
            .wecom_config
            .as_ref()
            .map(|config| config.bot_id.clone());
        let wecom_bot_secret = match response.wecom_config.as_ref() {
            Some(config) => Some(crate::crypto::encrypt_secret(&config.bot_secret)?),
            None => None,
        };
        let setup_exit_code = response.setup.as_ref().map(|setup| setup.exit_code);
        let last_error = response
            .setup
            .as_ref()
            .and_then(|setup| (!setup.stderr.trim().is_empty()).then(|| setup.stderr.clone()));

        sqlx::query(
            r#"
INSERT INTO t_agenthub_instance (
  agent_id, sandbox_id, template_id, name, engine, env, model, version, status,
  bots, avatar, avatar_tone, domain, gateway_port, env_port, gateway_token,
  persistence_mode, rootfs_source_type, rootfs_source_id,
  openclaw_persist_id, openclaw_state_path,
  wecom_bot_id, wecom_bot_secret,
  last_error, setup_exit_code, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
ON DUPLICATE KEY UPDATE
  sandbox_id = VALUES(sandbox_id),
  template_id = VALUES(template_id),
  name = VALUES(name),
  engine = VALUES(engine),
  env = VALUES(env),
  model = VALUES(model),
  version = VALUES(version),
  status = VALUES(status),
  bots = VALUES(bots),
  avatar = VALUES(avatar),
  avatar_tone = VALUES(avatar_tone),
  domain = VALUES(domain),
  gateway_port = VALUES(gateway_port),
  env_port = VALUES(env_port),
  gateway_token = VALUES(gateway_token),
  persistence_mode = VALUES(persistence_mode),
  rootfs_source_type = VALUES(rootfs_source_type),
  rootfs_source_id = VALUES(rootfs_source_id),
  openclaw_persist_id = VALUES(openclaw_persist_id),
  openclaw_state_path = VALUES(openclaw_state_path),
  wecom_bot_id = VALUES(wecom_bot_id),
  wecom_bot_secret = VALUES(wecom_bot_secret),
  last_error = VALUES(last_error),
  setup_exit_code = VALUES(setup_exit_code),
  deleted_at = NULL
"#,
        )
        .bind(&response.id)
        .bind(&response.sandbox_id)
        .bind(&response.template_id)
        .bind(&response.name)
        .bind(&response.engine)
        .bind(&response.env)
        .bind(&response.model)
        .bind(&response.version)
        .bind(&response.status)
        .bind(bots)
        .bind(&response.avatar)
        .bind(&response.avatar_tone)
        .bind(domain)
        .bind(18789_i32)
        .bind(8080_i32)
        .bind(gateway_token)
        .bind(response.persistence_mode.as_deref())
        .bind(response.rootfs_source_type.as_deref())
        .bind(response.rootfs_source_id.as_deref())
        .bind(response.openclaw_persist_id.as_deref())
        .bind(response.openclaw_state_path.as_deref())
        .bind(wecom_bot_id.as_deref())
        .bind(wecom_bot_secret.as_deref())
        .bind(last_error)
        .bind(setup_exit_code)
        .execute(&self.pool)
        .await?;

        Ok(())
    }

    pub async fn update_wecom_config(
        &self,
        agent_id: &str,
        bot_id: &str,
        bot_secret: &str,
        gateway_token: Option<&str>,
        setup: &AgentSetupResult,
    ) -> anyhow::Result<Option<AgentHubInstanceRecord>> {
        let bots = serde_json::to_value(["wecom"])?;
        let last_error = (!setup.stderr.trim().is_empty()).then(|| setup.stderr.clone());
        let encrypted_secret = crate::crypto::encrypt_secret(bot_secret)?;

        sqlx::query(
            r#"
UPDATE t_agenthub_instance
SET bots = ?,
    wecom_bot_id = ?,
    wecom_bot_secret = ?,
    gateway_token = COALESCE(?, gateway_token),
    setup_exit_code = ?,
    last_error = ?
WHERE agent_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(bots)
        .bind(bot_id)
        .bind(&encrypted_secret)
        .bind(gateway_token)
        .bind(setup.exit_code)
        .bind(last_error)
        .bind(agent_id)
        .execute(&self.pool)
        .await?;

        self.get_instance(agent_id).await
    }

    pub async fn update_model(
        &self,
        agent_id: &str,
        model: &str,
        setup: &AgentSetupResult,
    ) -> anyhow::Result<Option<AgentHubInstanceRecord>> {
        let last_error = (!setup.stderr.trim().is_empty()).then(|| setup.stderr.clone());
        sqlx::query(
            r#"
UPDATE t_agenthub_instance
SET model = ?,
    setup_exit_code = ?,
    last_error = ?
WHERE agent_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(model)
        .bind(setup.exit_code)
        .bind(last_error)
        .bind(agent_id)
        .execute(&self.pool)
        .await?;

        self.get_instance(agent_id).await
    }

    pub async fn update_status(
        &self,
        agent_id: &str,
        status: &str,
    ) -> anyhow::Result<Option<AgentHubInstanceRecord>> {
        sqlx::query(
            r#"
UPDATE t_agenthub_instance
SET status = ?
WHERE agent_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(status)
        .bind(agent_id)
        .execute(&self.pool)
        .await?;

        self.get_instance(agent_id).await
    }

    pub async fn soft_delete_instance(&self, agent_id: &str) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_instance
SET status = 'stopped', deleted_at = CURRENT_TIMESTAMP
WHERE agent_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(agent_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn upsert_snapshot_info(
        &self,
        agent_id: &str,
        sandbox_id: &str,
        info: &SnapshotInfo,
    ) -> anyhow::Result<()> {
        let name = info.names.first().map(String::as_str);
        sqlx::query(
            r#"
INSERT INTO t_agenthub_snapshot (
  snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
  rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, openclaw_state_snapshot_path, deleted_at
) VALUES (?, ?, ?, ?, 'ready', 'sandbox', ?, 'snapshot', ?, ?, NULL, NULL)
ON DUPLICATE KEY UPDATE
  agent_id = VALUES(agent_id),
  sandbox_id = VALUES(sandbox_id),
  status = VALUES(status),
  snapshot_kind = VALUES(snapshot_kind),
  origin_sandbox_id = VALUES(origin_sandbox_id),
  rootfs_source_type = VALUES(rootfs_source_type),
  rootfs_source_id = VALUES(rootfs_source_id),
  rootfs_snapshot_id = VALUES(rootfs_snapshot_id),
  deleted_at = NULL
"#,
        )
        .bind(&info.snapshot_id)
        .bind(agent_id)
        .bind(sandbox_id)
        .bind(name)
        .bind(sandbox_id)
        .bind(&info.snapshot_id)
        .bind(&info.snapshot_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn upsert_agenthub_openclaw_snapshot(
        &self,
        agent_id: &str,
        sandbox_id: &str,
        snapshot_id: &str,
        name: Option<&str>,
        rootfs_source_type: &str,
        rootfs_source_id: &str,
        rootfs_snapshot_id: &str,
        openclaw_state_snapshot_path: &str,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
INSERT INTO t_agenthub_snapshot (
  snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
  rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, openclaw_state_snapshot_path, deleted_at
) VALUES (?, ?, ?, ?, 'ready', 'agenthub_state', ?, ?, ?, ?, ?, NULL)
ON DUPLICATE KEY UPDATE
  agent_id = VALUES(agent_id),
  sandbox_id = VALUES(sandbox_id),
  name = VALUES(name),
  status = VALUES(status),
  snapshot_kind = VALUES(snapshot_kind),
  origin_sandbox_id = VALUES(origin_sandbox_id),
  rootfs_source_type = VALUES(rootfs_source_type),
  rootfs_source_id = VALUES(rootfs_source_id),
  rootfs_snapshot_id = VALUES(rootfs_snapshot_id),
  openclaw_state_snapshot_path = VALUES(openclaw_state_snapshot_path),
  deleted_at = NULL
"#,
        )
        .bind(snapshot_id)
        .bind(agent_id)
        .bind(sandbox_id)
        .bind(name)
        .bind(sandbox_id)
        .bind(rootfs_source_type)
        .bind(rootfs_source_id)
        .bind(rootfs_snapshot_id)
        .bind(openclaw_state_snapshot_path)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn upsert_snapshot_item(
        &self,
        agent_id: &str,
        sandbox_id: &str,
        item: &SnapshotListItem,
    ) -> anyhow::Result<()> {
        let name = item.names.first().map(String::as_str);
        sqlx::query(
            r#"
INSERT INTO t_agenthub_snapshot (
  snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
  rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, deleted_at
) VALUES (?, ?, ?, ?, ?, 'sandbox', ?, 'snapshot', ?, ?, NULL)
ON DUPLICATE KEY UPDATE
  agent_id = VALUES(agent_id),
  sandbox_id = VALUES(sandbox_id),
  status = VALUES(status),
  snapshot_kind = VALUES(snapshot_kind),
  origin_sandbox_id = VALUES(origin_sandbox_id),
  rootfs_source_type = VALUES(rootfs_source_type),
  rootfs_source_id = VALUES(rootfs_source_id),
  rootfs_snapshot_id = VALUES(rootfs_snapshot_id),
  deleted_at = NULL
"#,
        )
        .bind(&item.snapshot_id)
        .bind(agent_id)
        .bind(sandbox_id)
        .bind(name)
        .bind(&item.status)
        .bind(item.origin_sandbox_id.as_deref().or(Some(sandbox_id)))
        .bind(&item.snapshot_id)
        .bind(&item.snapshot_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn list_snapshots(
        &self,
        agent_id: &str,
    ) -> anyhow::Result<Vec<AgentHubSnapshotRecord>> {
        let rows = sqlx::query(
            r#"
SELECT s.snapshot_id, s.name, s.status, s.snapshot_kind, s.origin_sandbox_id, s.published_template_id,
       s.rootfs_source_type, s.rootfs_source_id,
       s.rootfs_snapshot_id, s.openclaw_state_snapshot_path,
       s.parent_snapshot_id, s.is_healthy,
       t.template_id IS NOT NULL AS template_referenced,
       DATE_FORMAT(s.created_at, '%Y-%m-%dT%H:%i:%sZ') AS created_at,
       DATE_FORMAT(s.updated_at, '%Y-%m-%dT%H:%i:%sZ') AS updated_at
FROM t_agenthub_snapshot s
LEFT JOIN t_agenthub_template t
  ON t.source_snapshot_id = s.snapshot_id AND t.deleted_at IS NULL
WHERE s.agent_id = ? AND s.deleted_at IS NULL
ORDER BY s.created_at DESC, s.id DESC
"#,
        )
        .bind(agent_id)
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(|row| {
                Ok::<AgentHubSnapshotRecord, sqlx::Error>(AgentHubSnapshotRecord {
                    snapshot_id: row.try_get("snapshot_id")?,
                    name: row.try_get("name")?,
                    status: row.try_get("status")?,
                    snapshot_kind: row.try_get("snapshot_kind")?,
                    origin_sandbox_id: row.try_get("origin_sandbox_id")?,
                    published_template_id: row.try_get("published_template_id")?,
                    rootfs_source_type: row.try_get("rootfs_source_type")?,
                    rootfs_source_id: row.try_get("rootfs_source_id")?,
                    rootfs_snapshot_id: row.try_get("rootfs_snapshot_id")?,
                    openclaw_state_snapshot_path: row.try_get("openclaw_state_snapshot_path")?,
                    template_referenced: row.try_get("template_referenced")?,
                    is_healthy: row.try_get::<i8, _>("is_healthy")? != 0,
                    parent_snapshot_id: row.try_get("parent_snapshot_id")?,
                    created_at: row.try_get("created_at")?,
                    updated_at: row.try_get("updated_at")?,
                })
            })
            .collect::<Result<Vec<_>, sqlx::Error>>()
            .map_err(anyhow::Error::from)
    }

    pub async fn get_snapshot(
        &self,
        agent_id: &str,
        snapshot_id: &str,
    ) -> anyhow::Result<Option<AgentHubSnapshotRecord>> {
        let row = sqlx::query(
            r#"
SELECT s.snapshot_id, s.name, s.status, s.snapshot_kind, s.origin_sandbox_id, s.published_template_id,
       s.rootfs_source_type, s.rootfs_source_id,
       s.rootfs_snapshot_id, s.openclaw_state_snapshot_path,
       s.parent_snapshot_id, s.is_healthy,
       t.template_id IS NOT NULL AS template_referenced,
       DATE_FORMAT(s.created_at, '%Y-%m-%dT%H:%i:%sZ') AS created_at,
       DATE_FORMAT(s.updated_at, '%Y-%m-%dT%H:%i:%sZ') AS updated_at
FROM t_agenthub_snapshot s
LEFT JOIN t_agenthub_template t
  ON t.source_snapshot_id = s.snapshot_id AND t.deleted_at IS NULL
WHERE s.agent_id = ? AND s.snapshot_id = ? AND s.deleted_at IS NULL
LIMIT 1
"#,
        )
        .bind(agent_id)
        .bind(snapshot_id)
        .fetch_optional(&self.pool)
        .await?;

        row.map(|row| {
            Ok::<AgentHubSnapshotRecord, sqlx::Error>(AgentHubSnapshotRecord {
                snapshot_id: row.try_get("snapshot_id")?,
                name: row.try_get("name")?,
                status: row.try_get("status")?,
                snapshot_kind: row.try_get("snapshot_kind")?,
                origin_sandbox_id: row.try_get("origin_sandbox_id")?,
                published_template_id: row.try_get("published_template_id")?,
                rootfs_source_type: row.try_get("rootfs_source_type")?,
                rootfs_source_id: row.try_get("rootfs_source_id")?,
                rootfs_snapshot_id: row.try_get("rootfs_snapshot_id")?,
                openclaw_state_snapshot_path: row.try_get("openclaw_state_snapshot_path")?,
                template_referenced: row.try_get("template_referenced")?,
                is_healthy: row.try_get::<i8, _>("is_healthy")? != 0,
                parent_snapshot_id: row.try_get("parent_snapshot_id")?,
                created_at: row.try_get("created_at")?,
                updated_at: row.try_get("updated_at")?,
            })
        })
        .transpose()
        .map_err(anyhow::Error::from)
    }

    pub async fn publish_template(
        &self,
        template_id: &str,
        name: &str,
        source: &AgentHubInstanceRecord,
        source_snapshot_id: &str,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
INSERT INTO t_agenthub_template (
  template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id,
  model, version, persistence_mode, recommended, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL)
ON DUPLICATE KEY UPDATE
  name = VALUES(name),
  source_agent_id = VALUES(source_agent_id),
  source_snapshot_id = VALUES(source_snapshot_id),
  source_sandbox_id = VALUES(source_sandbox_id),
  model = VALUES(model),
  version = VALUES(version),
  persistence_mode = VALUES(persistence_mode),
  deleted_at = NULL
"#,
        )
        .bind(template_id)
        .bind(name)
        .bind(&source.agent_id)
        .bind(source_snapshot_id)
        .bind(&source.sandbox_id)
        .bind(&source.model)
        .bind(&source.version)
        .bind(source.persistence_mode.as_deref())
        .execute(&self.pool)
        .await?;

        sqlx::query(
            r#"
UPDATE t_agenthub_snapshot
SET published_template_id = ?
WHERE snapshot_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(template_id)
        .bind(source_snapshot_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn register_market_template(
        &self,
        template_id: &str,
        name: &str,
        model: &str,
        version: &str,
        recommended: bool,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
INSERT INTO t_agenthub_template (
  template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id,
  model, version, persistence_mode, recommended, deleted_at
) VALUES (?, ?, 'market', ?, '', ?, ?, NULL, ?, NULL)
ON DUPLICATE KEY UPDATE
  name = VALUES(name),
  source_agent_id = 'market',
  source_snapshot_id = VALUES(source_snapshot_id),
  source_sandbox_id = '',
  model = VALUES(model),
  version = VALUES(version),
  persistence_mode = NULL,
  recommended = VALUES(recommended),
  deleted_at = NULL
"#,
        )
        .bind(template_id)
        .bind(name)
        .bind(template_id)
        .bind(model)
        .bind(version)
        .bind(recommended)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn list_templates(&self) -> anyhow::Result<Vec<AgentHubTemplateRecord>> {
        let rows = sqlx::query(
            r#"
SELECT t.template_id, t.name, t.source_agent_id, t.source_snapshot_id, t.source_sandbox_id,
       t.model, t.version, COALESCE(t.persistence_mode, i.persistence_mode) AS persistence_mode,
       t.recommended, DATE_FORMAT(t.created_at, '%Y-%m-%dT%H:%i:%sZ') AS created_at
FROM t_agenthub_template t
LEFT JOIN t_agenthub_instance i ON i.agent_id = t.source_agent_id AND i.deleted_at IS NULL
WHERE t.deleted_at IS NULL
ORDER BY t.created_at DESC, t.id DESC
"#,
        )
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(|row| {
                Ok::<AgentHubTemplateRecord, sqlx::Error>(AgentHubTemplateRecord {
                    template_id: row.try_get("template_id")?,
                    name: row.try_get("name")?,
                    source_agent_id: row.try_get("source_agent_id")?,
                    source_snapshot_id: row.try_get("source_snapshot_id")?,
                    source_sandbox_id: row.try_get("source_sandbox_id")?,
                    model: row.try_get("model")?,
                    version: row.try_get("version")?,
                    persistence_mode: row.try_get("persistence_mode")?,
                    recommended: row.try_get("recommended")?,
                    created_at: row.try_get("created_at")?,
                })
            })
            .collect::<Result<Vec<_>, sqlx::Error>>()
            .map_err(anyhow::Error::from)
    }

    pub async fn get_template(
        &self,
        template_id: &str,
    ) -> anyhow::Result<Option<AgentHubTemplateRecord>> {
        let row = sqlx::query(
            r#"
SELECT t.template_id, t.name, t.source_agent_id, t.source_snapshot_id, t.source_sandbox_id,
       t.model, t.version, COALESCE(t.persistence_mode, i.persistence_mode) AS persistence_mode,
       t.recommended, DATE_FORMAT(t.created_at, '%Y-%m-%dT%H:%i:%sZ') AS created_at
FROM t_agenthub_template t
LEFT JOIN t_agenthub_instance i ON i.agent_id = t.source_agent_id AND i.deleted_at IS NULL
WHERE t.template_id = ? AND t.deleted_at IS NULL
LIMIT 1
"#,
        )
        .bind(template_id)
        .fetch_optional(&self.pool)
        .await?;

        row.map(|row| {
            Ok::<AgentHubTemplateRecord, sqlx::Error>(AgentHubTemplateRecord {
                template_id: row.try_get("template_id")?,
                name: row.try_get("name")?,
                source_agent_id: row.try_get("source_agent_id")?,
                source_snapshot_id: row.try_get("source_snapshot_id")?,
                source_sandbox_id: row.try_get("source_sandbox_id")?,
                model: row.try_get("model")?,
                version: row.try_get("version")?,
                persistence_mode: row.try_get("persistence_mode")?,
                recommended: row.try_get("recommended")?,
                created_at: row.try_get("created_at")?,
            })
        })
        .transpose()
        .map_err(anyhow::Error::from)
    }

    pub async fn soft_delete_snapshot(
        &self,
        agent_id: &str,
        snapshot_id: &str,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_snapshot
SET deleted_at = CURRENT_TIMESTAMP
WHERE agent_id = ? AND snapshot_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(agent_id)
        .bind(snapshot_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn soft_delete_template(&self, template_id: &str) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_template
SET deleted_at = CURRENT_TIMESTAMP
WHERE template_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(template_id)
        .execute(&self.pool)
        .await?;

        sqlx::query(
            r#"
UPDATE t_agenthub_snapshot
SET published_template_id = NULL
WHERE published_template_id = ?
"#,
        )
        .bind(template_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn update_template_name(&self, template_id: &str, name: &str) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_template
SET name = ?
WHERE template_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(name)
        .bind(template_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn set_template_recommended(
        &self,
        template_id: &str,
        recommended: bool,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_template
SET recommended = ?
WHERE template_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(recommended)
        .bind(template_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn create_operation(
        &self,
        agent_id: &str,
        sandbox_id: &str,
        operation_type: &str,
    ) -> anyhow::Result<String> {
        let operation_id = uuid::Uuid::new_v4().simple().to_string();
        sqlx::query(
            r#"
INSERT INTO t_agenthub_operation (
  operation_id, agent_id, sandbox_id, operation_type, status
) VALUES (?, ?, ?, ?, 'running')
"#,
        )
        .bind(&operation_id)
        .bind(agent_id)
        .bind(sandbox_id)
        .bind(operation_type)
        .execute(&self.pool)
        .await?;
        Ok(operation_id)
    }

    pub async fn finish_operation(
        &self,
        operation_id: &str,
        status: &str,
        target_id: Option<&str>,
        error_message: Option<&str>,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_operation
SET status = ?, target_id = ?, error_message = ?
WHERE operation_id = ?
"#,
        )
        .bind(status)
        .bind(target_id)
        .bind(error_message)
        .bind(operation_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn list_operations(
        &self,
        agent_id: &str,
        limit: i32,
    ) -> anyhow::Result<Vec<AgentHubOperationRecord>> {
        let rows = sqlx::query(
            r#"
SELECT operation_id, agent_id, operation_type, status, target_id, error_message,
       DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ') AS created_at,
       DATE_FORMAT(updated_at, '%Y-%m-%dT%H:%i:%sZ') AS updated_at
FROM t_agenthub_operation
WHERE agent_id = ?
ORDER BY id DESC
LIMIT ?
"#,
        )
        .bind(agent_id)
        .bind(limit.max(1).min(100))
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(|row| {
                Ok::<AgentHubOperationRecord, sqlx::Error>(AgentHubOperationRecord {
                    operation_id: row.try_get("operation_id")?,
                    agent_id: row.try_get("agent_id")?,
                    operation_type: row.try_get("operation_type")?,
                    status: row.try_get("status")?,
                    target_id: row.try_get("target_id")?,
                    error_message: row.try_get("error_message")?,
                    created_at: row.try_get("created_at")?,
                    updated_at: row.try_get("updated_at")?,
                })
            })
            .collect::<Result<Vec<_>, sqlx::Error>>()
            .map_err(anyhow::Error::from)
    }

    pub async fn set_snapshot_healthy(
        &self,
        agent_id: &str,
        snapshot_id: &str,
        healthy: bool,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_snapshot
SET is_healthy = ?
WHERE agent_id = ? AND snapshot_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(healthy)
        .bind(agent_id)
        .bind(snapshot_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn update_snapshot_name(
        &self,
        agent_id: &str,
        snapshot_id: &str,
        name: &str,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_snapshot
SET name = ?
WHERE agent_id = ? AND snapshot_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(name)
        .bind(agent_id)
        .bind(snapshot_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn set_snapshot_parent(
        &self,
        snapshot_id: &str,
        parent_snapshot_id: Option<&str>,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_snapshot
SET parent_snapshot_id = ?
WHERE snapshot_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(parent_snapshot_id)
        .bind(snapshot_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// Returns the most recently created snapshot that has been marked healthy,
    /// used by crash auto-recovery to roll back to a known-good state.
    pub async fn latest_healthy_snapshot(&self, agent_id: &str) -> anyhow::Result<Option<String>> {
        let snapshot_id: Option<String> = sqlx::query_scalar(
            r#"
SELECT snapshot_id
FROM t_agenthub_snapshot
WHERE agent_id = ? AND deleted_at IS NULL AND is_healthy = 1
ORDER BY created_at DESC, id DESC
LIMIT 1
"#,
        )
        .bind(agent_id)
        .fetch_optional(&self.pool)
        .await?;
        Ok(snapshot_id)
    }

    pub async fn get_base_snapshot_id(&self, agent_id: &str) -> anyhow::Result<Option<String>> {
        let base: Option<String> = sqlx::query_scalar(
            r#"
SELECT base_snapshot_id
FROM t_agenthub_instance
WHERE agent_id = ? AND deleted_at IS NULL
LIMIT 1
"#,
        )
        .bind(agent_id)
        .fetch_optional(&self.pool)
        .await?
        .flatten();
        Ok(base)
    }

    pub async fn set_base_snapshot_id(
        &self,
        agent_id: &str,
        snapshot_id: &str,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_agenthub_instance
SET base_snapshot_id = ?
WHERE agent_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(snapshot_id)
        .bind(agent_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// Reads a global AgentHub setting (e.g. the shared DeepSeek API key).
    pub async fn get_setting(&self, key: &str) -> anyhow::Result<Option<String>> {
        let value: Option<String> = sqlx::query_scalar(
            r#"SELECT setting_value FROM t_agenthub_setting WHERE setting_key = ? LIMIT 1"#,
        )
        .bind(key)
        .fetch_optional(&self.pool)
        .await?
        .flatten();
        Ok(value)
    }

    /// Atomically returns the stored value for `key`, inserting `default_value`
    /// only when the row does not exist yet.
    ///
    /// Concurrency-safe across processes: the `INSERT IGNORE` makes only the
    /// first writer's value win at the row level (primary key on `setting_key`),
    /// and the subsequent `SELECT` returns that single persisted value, so all
    /// callers converge on the same result without an application-level lock.
    pub async fn get_or_create_setting(
        &self,
        key: &str,
        default_value: &str,
    ) -> anyhow::Result<String> {
        sqlx::query(
            r#"INSERT IGNORE INTO t_agenthub_setting (setting_key, setting_value) VALUES (?, ?)"#,
        )
        .bind(key)
        .bind(default_value)
        .execute(&self.pool)
        .await?;
        self.get_setting(key)
            .await?
            .ok_or_else(|| anyhow::anyhow!("setting {key} missing after insert"))
    }

    /// Upserts a global AgentHub setting.
    pub async fn set_setting(&self, key: &str, value: &str) -> anyhow::Result<()> {
        sqlx::query(
            r#"
INSERT INTO t_agenthub_setting (setting_key, setting_value)
VALUES (?, ?)
ON DUPLICATE KEY UPDATE setting_value = VALUES(setting_value)
"#,
        )
        .bind(key)
        .bind(value)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// Returns the stored password for a WebUI user, if the user exists.
    pub async fn get_user_password(&self, username: &str) -> anyhow::Result<Option<String>> {
        let value: Option<String> = sqlx::query_scalar(
            r#"SELECT password FROM t_agenthub_user WHERE username = ? LIMIT 1"#,
        )
        .bind(username)
        .fetch_optional(&self.pool)
        .await?;
        Ok(value)
    }

    /// Updates a WebUI user's password.
    pub async fn set_user_password(&self, username: &str, password: &str) -> anyhow::Result<()> {
        sqlx::query(r#"UPDATE t_agenthub_user SET password = ? WHERE username = ?"#)
            .bind(password)
            .bind(username)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    /// Persists a new WebUI session token valid for `ttl_secs` seconds.
    pub async fn create_session(
        &self,
        token: &str,
        username: &str,
        ttl_secs: i64,
    ) -> anyhow::Result<()> {
        sqlx::query(
            r#"
INSERT INTO t_agenthub_session (token, username, expires_at)
VALUES (?, ?, DATE_ADD(CURRENT_TIMESTAMP, INTERVAL ? SECOND))
"#,
        )
        .bind(token)
        .bind(username)
        .bind(ttl_secs)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// Returns the username for a non-expired session token.
    pub async fn validate_session(&self, token: &str) -> anyhow::Result<Option<String>> {
        let username: Option<String> = sqlx::query_scalar(
            r#"
SELECT username FROM t_agenthub_session
WHERE token = ? AND expires_at > CURRENT_TIMESTAMP
LIMIT 1
"#,
        )
        .bind(token)
        .fetch_optional(&self.pool)
        .await?;
        Ok(username)
    }

    /// Deletes a session token (logout).
    pub async fn delete_session(&self, token: &str) -> anyhow::Result<()> {
        sqlx::query(r#"DELETE FROM t_agenthub_session WHERE token = ?"#)
            .bind(token)
            .execute(&self.pool)
            .await?;
        Ok(())
    }
}

impl AgentHubInstanceRecord {
    pub fn into_response(self) -> AgentInstanceResponse {
        let bots_available = ["wecom"]
            .into_iter()
            .filter(|b| !self.bots.iter().any(|v| v == b))
            .map(ToString::to_string)
            .collect();
        let gateway_token = self
            .openclaw_state_path
            .as_deref()
            .and_then(crate::handlers::agenthub::read_openclaw_gateway_token_from_host)
            .or(self.gateway_token);

        AgentInstanceResponse {
            id: self.agent_id,
            name: self.name,
            status: self.status,
            engine: self.engine,
            env: self.env,
            model: self.model,
            version: self.version,
            bots: self.bots,
            bots_available,
            avatar: self.avatar,
            avatar_tone: self.avatar_tone,
            sandbox_id: self.sandbox_id.clone(),
            template_id: self.template_id,
            gateway_url: crate::handlers::agenthub::tokenized_gateway_url(
                crate::handlers::agenthub::sandbox_https_url(18789, &self.sandbox_id, &self.domain),
                gateway_token,
            ),
            env_url: crate::handlers::agenthub::sandbox_url(8080, &self.sandbox_id, &self.domain),
            persistence_mode: self.persistence_mode,
            rootfs_source_type: self.rootfs_source_type,
            rootfs_source_id: self.rootfs_source_id,
            openclaw_persist_id: self.openclaw_persist_id,
            openclaw_state_path: self.openclaw_state_path,
            wecom_config: match (self.wecom_bot_id, self.wecom_bot_secret) {
                (Some(bot_id), Some(bot_secret)) => Some(AgentWeComConfig {
                    bot_id,
                    bot_secret: crate::crypto::decrypt_or_passthrough(&bot_secret),
                }),
                _ => None,
            },
            setup: self.setup_exit_code.map(|exit_code| AgentSetupResult {
                exit_code,
                stdout: String::new(),
                stderr: self.last_error.unwrap_or_default(),
            }),
        }
    }
}
