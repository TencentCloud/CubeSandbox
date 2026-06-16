// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use serde_json::Value;
use sqlx::{mysql::MySqlPoolOptions, MySqlPool, Row};

use crate::handlers::agenthub::{AgentInstanceResponse, AgentSetupResult, AgentWeComConfig};
use crate::models::{SnapshotInfo, SnapshotListItem};

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

// ── Store Template records ──────────────────────────────────────────────────

pub struct StoreTemplateRecord {
    pub item_id: String,
    pub name_key: String,
    pub description_key: String,
    pub image_cn: String,
    pub image_intl: String,
    pub digest: Option<String>,
    pub tags: Vec<String>,
    pub category: String,
    pub size_mb: i32,
    pub expose_ports: Vec<i32>,
    pub probe_port: i32,
    pub probe_path: String,
    pub writable_layer_size: String,
    pub official: bool,
    pub dns: Vec<String>,
    pub sort_order: i32,
}

impl AgentHubStore {
    pub async fn connect(database_url: &str) -> anyhow::Result<Self> {
        let pool = MySqlPoolOptions::new()
            .max_connections(5)
            .connect(database_url)
            .await?;
        let store = Self { pool };
        store.migrate().await?;
        Ok(store)
    }

    async fn migrate(&self) -> anyhow::Result<()> {
        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_store_template` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `item_id` varchar(128) NOT NULL COMMENT 'Unique catalog item ID, e.g. sandbox-code',
  `name_key` varchar(255) NOT NULL COMMENT 'i18n key for display name',
  `description_key` varchar(255) NOT NULL COMMENT 'i18n key for description',
  `image_cn` varchar(512) NOT NULL COMMENT 'Container image for China region',
  `image_intl` varchar(512) NOT NULL COMMENT 'Container image for international region',
  `digest` varchar(255) DEFAULT NULL COMMENT 'Expected sha256 digest',
  `tags` json DEFAULT NULL COMMENT 'Tag array for filtering',
  `category` varchar(32) NOT NULL COMMENT 'code|browser|ai|base',
  `size_mb` int NOT NULL DEFAULT 0 COMMENT 'Approximate uncompressed size in MB',
  `expose_ports` json DEFAULT NULL COMMENT 'Ports to expose',
  `probe_port` int NOT NULL DEFAULT 0,
  `probe_path` varchar(255) NOT NULL DEFAULT '/',
  `writable_layer_size` varchar(32) NOT NULL DEFAULT '1G',
  `official` tinyint(1) NOT NULL DEFAULT 0,
  `sort_order` int NOT NULL DEFAULT 0 COMMENT 'Display order (lower first)',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_store_template_item_id` (`item_id`),
  KEY `idx_store_template_category` (`category`),
  KEY `idx_store_template_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;

        // ensure dns column exists in t_store_template (added after initial schema)
        self.ensure_table_column(
            "t_store_template",
            "dns",
            "`dns` json DEFAULT NULL COMMENT 'DNS servers for sandbox (ordered by priority)'",
        )
        .await?;

        self.seed_store_templates().await?;

        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_agenthub_instance` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `agent_id` varchar(128) NOT NULL,
  `sandbox_id` varchar(128) NOT NULL,
  `template_id` varchar(128) NOT NULL,
  `name` varchar(128) NOT NULL,
  `engine` varchar(32) NOT NULL,
  `env` varchar(32) NOT NULL,
  `model` varchar(128) NOT NULL,
  `version` varchar(64) NOT NULL,
  `status` varchar(32) NOT NULL,
  `bots` json DEFAULT NULL,
  `avatar` varchar(128) NOT NULL,
  `avatar_tone` varchar(32) NOT NULL,
  `domain` varchar(255) NOT NULL DEFAULT '',
  `gateway_port` int NOT NULL DEFAULT 18789,
  `env_port` int NOT NULL DEFAULT 8080,
  `gateway_token` varchar(255) DEFAULT NULL,
  `persistence_mode` varchar(32) DEFAULT NULL,
  `rootfs_source_type` varchar(32) DEFAULT NULL,
  `rootfs_source_id` varchar(128) DEFAULT NULL,
  `openclaw_persist_id` varchar(128) DEFAULT NULL,
  `openclaw_state_path` varchar(512) DEFAULT NULL,
  `wecom_bot_id` varchar(255) DEFAULT NULL,
  `wecom_bot_secret` varchar(255) DEFAULT NULL,
  `last_error` text DEFAULT NULL,
  `setup_exit_code` int DEFAULT NULL,
  `base_snapshot_id` varchar(128) DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_agent_id` (`agent_id`),
  UNIQUE KEY `uk_agenthub_sandbox_id` (`sandbox_id`),
  KEY `idx_agenthub_status` (`status`),
  KEY `idx_agenthub_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;
        self.ensure_column("wecom_bot_id", "`wecom_bot_id` varchar(255) DEFAULT NULL")
            .await?;
        self.ensure_column(
            "persistence_mode",
            "`persistence_mode` varchar(32) DEFAULT NULL",
        )
        .await?;
        self.ensure_column(
            "rootfs_source_type",
            "`rootfs_source_type` varchar(32) DEFAULT NULL",
        )
        .await?;
        self.ensure_column(
            "rootfs_source_id",
            "`rootfs_source_id` varchar(128) DEFAULT NULL",
        )
        .await?;
        self.ensure_column(
            "openclaw_persist_id",
            "`openclaw_persist_id` varchar(128) DEFAULT NULL",
        )
        .await?;
        self.ensure_column(
            "openclaw_state_path",
            "`openclaw_state_path` varchar(512) DEFAULT NULL",
        )
        .await?;
        self.ensure_column(
            "wecom_bot_secret",
            "`wecom_bot_secret` varchar(255) DEFAULT NULL",
        )
        .await?;
        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_agenthub_snapshot` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `snapshot_id` varchar(128) NOT NULL,
  `agent_id` varchar(128) NOT NULL,
  `sandbox_id` varchar(128) NOT NULL,
  `name` varchar(255) DEFAULT NULL,
  `status` varchar(32) NOT NULL DEFAULT 'unknown',
  `snapshot_kind` varchar(32) DEFAULT NULL,
  `origin_sandbox_id` varchar(128) DEFAULT NULL,
  `published_template_id` varchar(128) DEFAULT NULL,
  `rootfs_source_type` varchar(32) DEFAULT NULL,
  `rootfs_source_id` varchar(128) DEFAULT NULL,
  `rootfs_snapshot_id` varchar(128) DEFAULT NULL,
  `openclaw_state_snapshot_path` varchar(512) DEFAULT NULL,
  `parent_snapshot_id` varchar(128) DEFAULT NULL,
  `is_healthy` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_snapshot_id` (`snapshot_id`),
  KEY `idx_agenthub_snapshot_agent` (`agent_id`, `deleted_at`),
  KEY `idx_agenthub_snapshot_sandbox` (`sandbox_id`, `deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;
        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_agenthub_template` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `template_id` varchar(128) NOT NULL,
  `name` varchar(255) NOT NULL,
  `source_agent_id` varchar(128) NOT NULL,
  `source_snapshot_id` varchar(128) NOT NULL,
  `source_sandbox_id` varchar(128) NOT NULL,
  `model` varchar(128) NOT NULL,
  `version` varchar(64) NOT NULL,
  `persistence_mode` varchar(32) DEFAULT NULL,
  `recommended` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_template_id` (`template_id`),
  KEY `idx_agenthub_template_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;
        self.ensure_table_column(
            "t_agenthub_template",
            "recommended",
            "`recommended` tinyint(1) NOT NULL DEFAULT 0",
        )
        .await?;
        self.ensure_table_column(
            "t_agenthub_template",
            "persistence_mode",
            "`persistence_mode` varchar(32) DEFAULT NULL",
        )
        .await?;
        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_agenthub_operation` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `operation_id` varchar(128) NOT NULL,
  `agent_id` varchar(128) NOT NULL,
  `sandbox_id` varchar(128) NOT NULL,
  `operation_type` varchar(32) NOT NULL,
  `status` varchar(32) NOT NULL,
  `target_id` varchar(128) DEFAULT NULL,
  `error_message` text DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_operation_id` (`operation_id`),
  KEY `idx_agenthub_operation_agent` (`agent_id`, `created_at`),
  KEY `idx_agenthub_operation_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;
        self.ensure_table_column(
            "t_agenthub_snapshot",
            "snapshot_kind",
            "`snapshot_kind` varchar(32) DEFAULT NULL",
        )
        .await?;
        self.ensure_table_column(
            "t_agenthub_snapshot",
            "rootfs_source_type",
            "`rootfs_source_type` varchar(32) DEFAULT NULL",
        )
        .await?;
        self.ensure_table_column(
            "t_agenthub_snapshot",
            "rootfs_source_id",
            "`rootfs_source_id` varchar(128) DEFAULT NULL",
        )
        .await?;
        self.ensure_table_column(
            "t_agenthub_snapshot",
            "rootfs_snapshot_id",
            "`rootfs_snapshot_id` varchar(128) DEFAULT NULL",
        )
        .await?;
        self.ensure_table_column(
            "t_agenthub_snapshot",
            "openclaw_state_snapshot_path",
            "`openclaw_state_snapshot_path` varchar(512) DEFAULT NULL",
        )
        .await?;
        self.ensure_table_column(
            "t_agenthub_snapshot",
            "parent_snapshot_id",
            "`parent_snapshot_id` varchar(128) DEFAULT NULL",
        )
        .await?;
        self.ensure_table_column(
            "t_agenthub_snapshot",
            "is_healthy",
            "`is_healthy` tinyint(1) NOT NULL DEFAULT 0",
        )
        .await?;
        self.ensure_column(
            "base_snapshot_id",
            "`base_snapshot_id` varchar(128) DEFAULT NULL",
        )
        .await?;
        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_agenthub_setting` (
  `setting_key` varchar(128) NOT NULL,
  `setting_value` text DEFAULT NULL,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`setting_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;
        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_agenthub_user` (
  `username` varchar(128) NOT NULL,
  `password` varchar(255) NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;
        // Seed the default admin account (admin/admin) on first run. The
        // password is stored as a bcrypt hash. INSERT IGNORE keeps any password
        // the operator has already changed it to.
        let admin_hash = crate::crypto::hash_password("admin")?;
        sqlx::query(
            r#"INSERT IGNORE INTO t_agenthub_user (username, password) VALUES ('admin', ?)"#,
        )
        .bind(&admin_hash)
        .execute(&self.pool)
        .await?;
        sqlx::query(
            r#"
CREATE TABLE IF NOT EXISTS `t_agenthub_session` (
  `token` varchar(128) NOT NULL,
  `username` varchar(128) NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `expires_at` datetime NOT NULL,
  PRIMARY KEY (`token`),
  KEY `idx_agenthub_session_expires` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
"#,
        )
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn ensure_column(
        &self,
        column_name: &str,
        column_definition: &str,
    ) -> anyhow::Result<()> {
        self.ensure_table_column("t_agenthub_instance", column_name, column_definition)
            .await
    }

    async fn ensure_table_column(
        &self,
        table_name: &str,
        column_name: &str,
        column_definition: &str,
    ) -> anyhow::Result<()> {
        let exists: i64 = sqlx::query_scalar(
            r#"
SELECT COUNT(*)
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = DATABASE()
  AND TABLE_NAME = ?
  AND COLUMN_NAME = ?
"#,
        )
        .bind(table_name)
        .bind(column_name)
        .fetch_one(&self.pool)
        .await?;

        if exists == 0 {
            let sql = format!(
                "ALTER TABLE `{}` ADD COLUMN {}",
                table_name, column_definition
            );
            sqlx::query(&sql).execute(&self.pool).await?;
        }

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

    // ── Store Template CRUD ──────────────────────────────────────────────────

    /// Seed default store templates if the table is empty.
    async fn seed_store_templates(&self) -> anyhow::Result<()> {
        let count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM t_store_template WHERE deleted_at IS NULL",
        )
        .fetch_one(&self.pool)
        .await?;

        if count > 0 {
            return Ok(());
        }

        let seeds: Vec<(&str, &str, &str, &str, &str, Option<&str>, &[&str], &str, i32, &[i32], i32, &str, &str, bool, &[&str], i32)> = vec![
            (
                "sandbox-code",
                "items.sandbox-code.name",
                "items.sandbox-code.description",
                "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
                "cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
                Some("sha256:a7b8654aac5b90e241b98e195ae1d8c85d59fe1fb8c282bcccf1071f877db20f"),
                &["python", "jupyter", "official"],
                "code",
                207,
                &[49983, 49999],
                49999,
                "/",
                "1G",
                true,
                &[],
                0,
            ),
            (
                "sandbox-browser",
                "items.sandbox-browser.name",
                "items.sandbox-browser.description",
                "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
                "cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
                Some("sha256:1786786af8510c34eda64ebec5b0a61a98583cb311c3045c0222910ec0680d60"),
                &["browser", "chromium", "official"],
                "browser",
                1530,
                &[49983, 9000, 6080],
                9000,
                "/cdp/json/version",
                "1G",
                true,
                &["183.60.83.19", "114.114.114.114", "223.5.5.5", "8.8.8.8"],
                1,
            ),
            (
                "openclaw",
                "items.openclaw.name",
                "items.openclaw.description",
                "cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest",
                "cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest",
                Some("sha256:47680d7bc13ea7c57aeb88dff59ef2c44b0facb508e8c9066d479d7d458e0a66"),
                &["agent", "openclaw", "browser", "deepseek"],
                "ai",
                6350,
                &[49983, 18789, 8080],
                49983,
                "/health",
                "4G",
                true,
                &[],
                2,
            ),
            (
                "cubesandbox-base",
                "items.cubesandbox-base.name",
                "items.cubesandbox-base.description",
                "ghcr.io/tencentcloud/cubesandbox-base:latest",
                "ghcr.io/tencentcloud/cubesandbox-base:latest",
                None,
                &["base", "envd", "official"],
                "base",
                98,
                &[49983],
                49983,
                "/health",
                "1G",
                true,
                &[],
                3,
            ),
            (
                "sandbox-nginx",
                "items.sandbox-nginx.name",
                "items.sandbox-nginx.description",
                "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-nginx:latest",
                "cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-nginx:latest",
                None,
                &["nginx", "web", "official"],
                "web",
                120,
                &[49983, 80],
                49983,
                "/health",
                "1G",
                true,
                &[],
                4,
            ),
        ];

        for s in &seeds {
            let tags = serde_json::to_value(&s.6)?;
            let ports = serde_json::to_value(&s.9)?;
            let dns = serde_json::to_value(&s.14)?;
            sqlx::query(
                r#"
INSERT INTO t_store_template (
  item_id, name_key, description_key, image_cn, image_intl, digest,
  tags, category, size_mb, expose_ports, probe_port, probe_path,
  writable_layer_size, official, dns, sort_order, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
ON DUPLICATE KEY UPDATE
  name_key = VALUES(name_key),
  description_key = VALUES(description_key),
  image_cn = VALUES(image_cn),
  image_intl = VALUES(image_intl),
  digest = VALUES(digest),
  tags = VALUES(tags),
  category = VALUES(category),
  size_mb = VALUES(size_mb),
  expose_ports = VALUES(expose_ports),
  probe_port = VALUES(probe_port),
  probe_path = VALUES(probe_path),
  writable_layer_size = VALUES(writable_layer_size),
  official = VALUES(official),
  dns = VALUES(dns),
  sort_order = VALUES(sort_order),
  deleted_at = NULL
"#,
            )
            .bind(s.0)
            .bind(s.1)
            .bind(s.2)
            .bind(s.3)
            .bind(s.4)
            .bind(s.5)
            .bind(tags)
            .bind(s.7)
            .bind(s.8)
            .bind(ports)
            .bind(s.10)
            .bind(s.11)
            .bind(s.12)
            .bind(s.13)
            .bind(dns)
            .bind(s.15)
            .execute(&self.pool)
            .await?;
        }

        Ok(())
    }

    pub async fn list_store_templates(&self) -> anyhow::Result<Vec<StoreTemplateRecord>> {
        let rows = sqlx::query(
            r#"
SELECT item_id, name_key, description_key, image_cn, image_intl, digest,
       tags, category, size_mb, expose_ports, probe_port, probe_path,
       writable_layer_size, official, dns, sort_order
FROM t_store_template
WHERE deleted_at IS NULL
ORDER BY sort_order, id
"#,
        )
        .fetch_all(&self.pool)
        .await?;

        rows.into_iter()
            .map(|row| {
                let tags_value: Option<Value> = row.try_get("tags")?;
                let ports_value: Option<Value> = row.try_get("expose_ports")?;
                let dns_value: Option<Value> = row.try_get("dns")?;
                Ok::<StoreTemplateRecord, sqlx::Error>(StoreTemplateRecord {
                    item_id: row.try_get("item_id")?,
                    name_key: row.try_get("name_key")?,
                    description_key: row.try_get("description_key")?,
                    image_cn: row.try_get("image_cn")?,
                    image_intl: row.try_get("image_intl")?,
                    digest: row.try_get("digest")?,
                    tags: tags_value
                        .and_then(|v| serde_json::from_value(v).ok())
                        .unwrap_or_default(),
                    category: row.try_get("category")?,
                    size_mb: row.try_get("size_mb")?,
                    expose_ports: ports_value
                        .and_then(|v| serde_json::from_value(v).ok())
                        .unwrap_or_default(),
                    probe_port: row.try_get("probe_port")?,
                    probe_path: row.try_get("probe_path")?,
                    writable_layer_size: row.try_get("writable_layer_size")?,
                    official: row.try_get::<i8, _>("official")? != 0,
                    dns: dns_value
                        .and_then(|v| serde_json::from_value(v).ok())
                        .unwrap_or_default(),
                    sort_order: row.try_get("sort_order")?,
                })
            })
            .collect::<Result<Vec<_>, sqlx::Error>>()
            .map_err(anyhow::Error::from)
    }

    #[allow(dead_code)]
    pub async fn get_store_template(&self, item_id: &str) -> anyhow::Result<Option<StoreTemplateRecord>> {
        let row = sqlx::query(
            r#"
SELECT item_id, name_key, description_key, image_cn, image_intl, digest,
       tags, category, size_mb, expose_ports, probe_port, probe_path,
       writable_layer_size, official, dns, sort_order
FROM t_store_template
WHERE item_id = ? AND deleted_at IS NULL
LIMIT 1
"#,
        )
        .bind(item_id)
        .fetch_optional(&self.pool)
        .await?;

        row.map(|row| {
            let tags_value: Option<Value> = row.try_get("tags")?;
            let ports_value: Option<Value> = row.try_get("expose_ports")?;
            let dns_value: Option<Value> = row.try_get("dns")?;
            Ok::<StoreTemplateRecord, sqlx::Error>(StoreTemplateRecord {
                item_id: row.try_get("item_id")?,
                name_key: row.try_get("name_key")?,
                description_key: row.try_get("description_key")?,
                image_cn: row.try_get("image_cn")?,
                image_intl: row.try_get("image_intl")?,
                digest: row.try_get("digest")?,
                tags: tags_value
                    .and_then(|v| serde_json::from_value(v).ok())
                    .unwrap_or_default(),
                category: row.try_get("category")?,
                size_mb: row.try_get("size_mb")?,
                expose_ports: ports_value
                    .and_then(|v| serde_json::from_value(v).ok())
                    .unwrap_or_default(),
                probe_port: row.try_get("probe_port")?,
                probe_path: row.try_get("probe_path")?,
                writable_layer_size: row.try_get("writable_layer_size")?,
                official: row.try_get::<i8, _>("official")? != 0,
                dns: dns_value
                    .and_then(|v| serde_json::from_value(v).ok())
                    .unwrap_or_default(),
                sort_order: row.try_get("sort_order")?,
            })
        })
        .transpose()
        .map_err(anyhow::Error::from)
    }

    pub async fn create_store_template(&self, record: &StoreTemplateRecord) -> anyhow::Result<()> {
        let tags = serde_json::to_value(&record.tags)?;
        let ports = serde_json::to_value(&record.expose_ports)?;
        let dns = serde_json::to_value(&record.dns)?;
        sqlx::query(
            r#"
INSERT INTO t_store_template (
  item_id, name_key, description_key, image_cn, image_intl, digest,
  tags, category, size_mb, expose_ports, probe_port, probe_path,
  writable_layer_size, official, dns, sort_order, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
ON DUPLICATE KEY UPDATE
  name_key = VALUES(name_key),
  description_key = VALUES(description_key),
  image_cn = VALUES(image_cn),
  image_intl = VALUES(image_intl),
  digest = VALUES(digest),
  tags = VALUES(tags),
  category = VALUES(category),
  size_mb = VALUES(size_mb),
  expose_ports = VALUES(expose_ports),
  probe_port = VALUES(probe_port),
  probe_path = VALUES(probe_path),
  writable_layer_size = VALUES(writable_layer_size),
  official = VALUES(official),
  dns = VALUES(dns),
  sort_order = VALUES(sort_order),
  deleted_at = NULL
"#,
        )
        .bind(&record.item_id)
        .bind(&record.name_key)
        .bind(&record.description_key)
        .bind(&record.image_cn)
        .bind(&record.image_intl)
        .bind(&record.digest)
        .bind(tags)
        .bind(&record.category)
        .bind(record.size_mb)
        .bind(ports)
        .bind(record.probe_port)
        .bind(&record.probe_path)
        .bind(&record.writable_layer_size)
        .bind(record.official)
        .bind(dns)
        .bind(record.sort_order)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn update_store_template(&self, record: &StoreTemplateRecord) -> anyhow::Result<()> {
        let tags = serde_json::to_value(&record.tags)?;
        let ports = serde_json::to_value(&record.expose_ports)?;
        let dns = serde_json::to_value(&record.dns)?;
        sqlx::query(
            r#"
UPDATE t_store_template SET
  name_key = ?, description_key = ?, image_cn = ?, image_intl = ?,
  digest = ?, tags = ?, category = ?, size_mb = ?, expose_ports = ?,
  probe_port = ?, probe_path = ?, writable_layer_size = ?,
  official = ?, dns = ?, sort_order = ?
WHERE item_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(&record.name_key)
        .bind(&record.description_key)
        .bind(&record.image_cn)
        .bind(&record.image_intl)
        .bind(&record.digest)
        .bind(tags)
        .bind(&record.category)
        .bind(record.size_mb)
        .bind(ports)
        .bind(record.probe_port)
        .bind(&record.probe_path)
        .bind(&record.writable_layer_size)
        .bind(record.official)
        .bind(dns)
        .bind(record.sort_order)
        .bind(&record.item_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn soft_delete_store_template(&self, item_id: &str) -> anyhow::Result<()> {
        sqlx::query(
            r#"
UPDATE t_store_template
SET deleted_at = CURRENT_TIMESTAMP
WHERE item_id = ? AND deleted_at IS NULL
"#,
        )
        .bind(item_id)
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
