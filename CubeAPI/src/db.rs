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
    pub wecom_bot_id: Option<String>,
    pub wecom_bot_secret: Option<String>,
    pub last_error: Option<String>,
    pub setup_exit_code: Option<i32>,
}

pub struct AgentHubSnapshotRecord {
    pub snapshot_id: String,
    pub name: Option<String>,
    pub status: String,
    pub origin_sandbox_id: Option<String>,
    pub published_template_id: Option<String>,
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
        store.migrate().await?;
        Ok(store)
    }

    async fn migrate(&self) -> anyhow::Result<()> {
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
  `origin_sandbox_id` varchar(128) DEFAULT NULL,
  `published_template_id` varchar(128) DEFAULT NULL,
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
       bots, avatar, avatar_tone, domain, gateway_token, wecom_bot_id, wecom_bot_secret,
       last_error, setup_exit_code
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
                    wecom_bot_id: row.try_get("wecom_bot_id")?,
                    wecom_bot_secret: row.try_get("wecom_bot_secret")?,
                    last_error: row.try_get("last_error")?,
                    setup_exit_code: row.try_get("setup_exit_code")?,
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
       bots, avatar, avatar_tone, domain, gateway_token, wecom_bot_id, wecom_bot_secret,
       last_error, setup_exit_code
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
                wecom_bot_id: row.try_get("wecom_bot_id")?,
                wecom_bot_secret: row.try_get("wecom_bot_secret")?,
                last_error: row.try_get("last_error")?,
                setup_exit_code: row.try_get("setup_exit_code")?,
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
        let (wecom_bot_id, wecom_bot_secret) = response
            .wecom_config
            .as_ref()
            .map(|config| {
                (
                    Some(config.bot_id.as_str()),
                    Some(config.bot_secret.as_str()),
                )
            })
            .unwrap_or((None, None));
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
  wecom_bot_id, wecom_bot_secret,
  last_error, setup_exit_code, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
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
        .bind(wecom_bot_id)
        .bind(wecom_bot_secret)
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
        .bind(bot_secret)
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
  snapshot_id, agent_id, sandbox_id, name, status, origin_sandbox_id, deleted_at
) VALUES (?, ?, ?, ?, 'ready', ?, NULL)
ON DUPLICATE KEY UPDATE
  agent_id = VALUES(agent_id),
  sandbox_id = VALUES(sandbox_id),
  status = VALUES(status),
  origin_sandbox_id = VALUES(origin_sandbox_id),
  deleted_at = NULL
"#,
        )
        .bind(&info.snapshot_id)
        .bind(agent_id)
        .bind(sandbox_id)
        .bind(name)
        .bind(sandbox_id)
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
  snapshot_id, agent_id, sandbox_id, name, status, origin_sandbox_id, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, NULL)
ON DUPLICATE KEY UPDATE
  agent_id = VALUES(agent_id),
  sandbox_id = VALUES(sandbox_id),
  status = VALUES(status),
  origin_sandbox_id = VALUES(origin_sandbox_id),
  deleted_at = NULL
"#,
        )
        .bind(&item.snapshot_id)
        .bind(agent_id)
        .bind(sandbox_id)
        .bind(name)
        .bind(&item.status)
        .bind(item.origin_sandbox_id.as_deref().or(Some(sandbox_id)))
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
SELECT s.snapshot_id, s.name, s.status, s.origin_sandbox_id, s.published_template_id,
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
                    origin_sandbox_id: row.try_get("origin_sandbox_id")?,
                    published_template_id: row.try_get("published_template_id")?,
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
  model, version, recommended, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, 0, NULL)
ON DUPLICATE KEY UPDATE
  name = VALUES(name),
  source_agent_id = VALUES(source_agent_id),
  source_snapshot_id = VALUES(source_snapshot_id),
  source_sandbox_id = VALUES(source_sandbox_id),
  model = VALUES(model),
  version = VALUES(version),
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

    pub async fn list_templates(&self) -> anyhow::Result<Vec<AgentHubTemplateRecord>> {
        let rows = sqlx::query(
            r#"
SELECT template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id,
       model, version, recommended, DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ') AS created_at
FROM t_agenthub_template
WHERE deleted_at IS NULL
ORDER BY created_at DESC, id DESC
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
SELECT template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id,
       model, version, recommended, DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ') AS created_at
FROM t_agenthub_template
WHERE template_id = ? AND deleted_at IS NULL
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
}

impl AgentHubInstanceRecord {
    pub fn into_response(self) -> AgentInstanceResponse {
        let bots_available = ["wecom"]
            .into_iter()
            .filter(|b| !self.bots.iter().any(|v| v == b))
            .map(ToString::to_string)
            .collect();

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
                self.gateway_token,
            ),
            env_url: crate::handlers::agenthub::sandbox_url(8080, &self.sandbox_id, &self.domain),
            wecom_config: match (self.wecom_bot_id, self.wecom_bot_secret) {
                (Some(bot_id), Some(bot_secret)) => Some(AgentWeComConfig { bot_id, bot_secret }),
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
