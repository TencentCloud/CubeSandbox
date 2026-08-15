-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Webhook event delivery (CubeOps worker, issue #642).
-- Four tables:
--   t_webhook_subscription           endpoint registry (soft delete via deleted_at)
--   t_webhook_subscription_event     per-subscription event-type allowlist
--   t_webhook_delivery               delivery ledger (claim / retry / terminal states)
--   t_webhook_materialization_failure poison-entry isolation records
--
-- Deliberately NO foreign keys: the repo has no FK precedent (MySQL would
-- auto-create an index per FK while PostgreSQL does not, which breaks the
-- cross-dialect schema alignment test). Referential integrity is enforced by
-- the application: subscriptions are soft-deleted only, delivery rows are
-- keyed by subscription_id and queried through it.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260814120000_webhook_delivery', 60);

CREATE TABLE IF NOT EXISTS `t_webhook_subscription` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `name` varchar(128) NOT NULL COMMENT 'endpoint alias; UNIQUE, renamed on soft delete to release the name',
  `url` varchar(2048) NOT NULL COMMENT 'endpoint URL',
  `enabled` tinyint(1) NOT NULL DEFAULT 1 COMMENT 'manual on/off; soft delete also sets deleted_at',
  `deleted_at` datetime DEFAULT NULL COMMENT 'soft-delete marker; NULL=exists, set=deleted (distinct from disabled)',
  `secret_ciphertext` text DEFAULT NULL COMMENT 'enc:v1: HMAC secret ciphertext',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_webhook_sub_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Webhook endpoint subscriptions';

CREATE TABLE IF NOT EXISTS `t_webhook_subscription_event` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `subscription_id` bigint NOT NULL COMMENT 'references t_webhook_subscription(id); app-level FK',
  `event_type` varchar(64) NOT NULL COMMENT 'sandbox.created/deleted/paused/resumed',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_webhook_sub_event` (`subscription_id`, `event_type`),
  KEY `idx_webhook_event_type` (`event_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Webhook per-subscription event allowlist';

CREATE TABLE IF NOT EXISTS `t_webhook_delivery` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `event_id` varchar(128) NOT NULL COMMENT 'stable event id (redis stream id or test:<uuid>)',
  `subscription_id` bigint NOT NULL COMMENT 'references t_webhook_subscription(id); app-level FK',
  `payload` mediumtext NOT NULL COMMENT 'immutable delivery payload (raw bytes)',
  `status` varchar(16) NOT NULL COMMENT 'pending/in_progress/succeeded/failed/permanent_failed/dead',
  `attempts` int NOT NULL DEFAULT 0 COMMENT 'recorded send attempts',
  `next_retry_at` datetime(6) NOT NULL COMMENT 'next claimable time; set to now() at materialization (never NULL)',
  `first_failed_at` datetime(6) DEFAULT NULL COMMENT 'first entry into failed; keep-pending retry window start',
  `lease_until` datetime(6) DEFAULT NULL COMMENT 'lease expiry',
  `lease_owner` varchar(128) DEFAULT NULL COMMENT 'claiming worker (consumer name)',
  `http_status` int DEFAULT NULL COMMENT 'last HTTP status',
  `last_error` text DEFAULT NULL COMMENT 'last error summary',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_webhook_delivery_event_sub` (`event_id`, `subscription_id`),
  KEY `idx_webhook_delivery_status_retry` (`status`, `next_retry_at`),
  KEY `idx_webhook_delivery_status_lease` (`status`, `lease_until`),
  KEY `idx_webhook_delivery_subscription` (`subscription_id`),
  KEY `idx_webhook_delivery_status_updated` (`status`, `updated_at`),
  KEY `idx_webhook_delivery_status_first_failed` (`status`, `first_failed_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Webhook delivery ledger';

CREATE TABLE IF NOT EXISTS `t_webhook_materialization_failure` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `event_id` varchar(128) NOT NULL COMMENT 'poison entry (redis stream id); UNIQUE',
  `sandbox_id` varchar(128) DEFAULT NULL COMMENT 'context, best-effort',
  `subscription_id` bigint DEFAULT NULL COMMENT 'offending subscription when identifiable',
  `op` varchar(16) NOT NULL COMMENT 'create/delete/state',
  `payload` mediumtext DEFAULT NULL COMMENT 'raw payload snapshot (truncated)',
  `error` text NOT NULL COMMENT 'failure reason (truncated)',
  `attempts` int NOT NULL COMMENT 'materialization failure count (persisted, cross-replica)',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_webhook_matfail_event` (`event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Webhook materialization failure isolation';

SELECT RELEASE_LOCK('cubemaster_migration_20260814120000_webhook_delivery');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260814120000_webhook_delivery', 60);

DROP TABLE IF EXISTS `t_webhook_materialization_failure`;
DROP TABLE IF EXISTS `t_webhook_delivery`;
DROP TABLE IF EXISTS `t_webhook_subscription_event`;
DROP TABLE IF EXISTS `t_webhook_subscription`;

SELECT RELEASE_LOCK('cubemaster_migration_20260814120000_webhook_delivery');
