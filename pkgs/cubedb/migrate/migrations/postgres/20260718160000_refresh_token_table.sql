-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Refresh token rotation: persist token state so that used refresh tokens
-- can be revoked, preventing token replay after refresh.

-- +goose Up

CREATE TABLE IF NOT EXISTS t_refresh_token (
  token_id   VARCHAR(64) PRIMARY KEY,
  username   VARCHAR(128) NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  revoked_at TIMESTAMPTZ
);

-- +goose Down

DROP TABLE IF EXISTS t_refresh_token;
