-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Refresh token rotation: persist token state so that used refresh tokens
-- can be revoked, preventing token replay after refresh.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260718160000_refresh_token', 60);

CREATE TABLE IF NOT EXISTS `t_refresh_token` (
  `token_id` varchar(64) NOT NULL,
  `username` varchar(128) NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `revoked_at` datetime DEFAULT NULL,
  PRIMARY KEY (`token_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20260718160000_refresh_token');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260718160000_refresh_token', 60);

DROP TABLE IF EXISTS `t_refresh_token`;

SELECT RELEASE_LOCK('cubemaster_migration_20260718160000_refresh_token');
