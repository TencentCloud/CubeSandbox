-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Webhook event delivery (CubeOps worker, issue #642).
-- PostgreSQL counterpart of mysql/20260814120000_webhook_delivery.sql.
-- See the mysql file header for the design notes; schema must stay logically
-- aligned with the mysql migration (migrate_alignment_test).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260814120000_webhook_delivery', 60);

CREATE TABLE IF NOT EXISTS t_webhook_subscription (
  id bigserial,
  name varchar(128) NOT NULL,
  url varchar(2048) NOT NULL,
  enabled boolean NOT NULL DEFAULT TRUE,
  deleted_at timestamp DEFAULT NULL,
  secret_ciphertext text DEFAULT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT uk_webhook_sub_name UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS t_webhook_subscription_event (
  id bigserial,
  subscription_id bigint NOT NULL,
  event_type varchar(64) NOT NULL,
  PRIMARY KEY (id),
  CONSTRAINT uk_webhook_sub_event UNIQUE (subscription_id, event_type)
);
CREATE INDEX IF NOT EXISTS idx_webhook_event_type ON t_webhook_subscription_event (event_type);

CREATE TABLE IF NOT EXISTS t_webhook_delivery (
  id bigserial,
  event_id varchar(128) NOT NULL,
  subscription_id bigint NOT NULL,
  payload text NOT NULL,
  status varchar(16) NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  next_retry_at timestamp NOT NULL,
  first_failed_at timestamp DEFAULT NULL,
  lease_until timestamp DEFAULT NULL,
  lease_owner varchar(128) DEFAULT NULL,
  http_status int DEFAULT NULL,
  last_error text DEFAULT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT uk_webhook_delivery_event_sub UNIQUE (event_id, subscription_id)
);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_status_retry ON t_webhook_delivery (status, next_retry_at);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_status_lease ON t_webhook_delivery (status, lease_until);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_subscription ON t_webhook_delivery (subscription_id);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_status_updated ON t_webhook_delivery (status, updated_at);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_status_first_failed ON t_webhook_delivery (status, first_failed_at);

CREATE TABLE IF NOT EXISTS t_webhook_materialization_failure (
  id bigserial,
  event_id varchar(128) NOT NULL,
  sandbox_id varchar(128) DEFAULT NULL,
  subscription_id bigint DEFAULT NULL,
  op varchar(16) NOT NULL,
  payload text DEFAULT NULL,
  error text NOT NULL,
  attempts int NOT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT uk_webhook_matfail_event UNIQUE (event_id)
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260814120000_webhook_delivery'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260814120000_webhook_delivery', 60);

DROP TABLE IF EXISTS t_webhook_materialization_failure;
DROP TABLE IF EXISTS t_webhook_delivery;
DROP TABLE IF EXISTS t_webhook_subscription_event;
DROP TABLE IF EXISTS t_webhook_subscription;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260814120000_webhook_delivery'));
