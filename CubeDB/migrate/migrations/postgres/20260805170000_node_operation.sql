-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Operation audit log for node management (isolate/unisolate/label changes).

-- +goose Up

CREATE TABLE IF NOT EXISTS t_cube_node_operation (
  id BIGSERIAL PRIMARY KEY,
  node_id VARCHAR(128) NOT NULL,
  type VARCHAR(32) NOT NULL DEFAULT '',
  operator VARCHAR(128) NOT NULL DEFAULT '',
  detail TEXT,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_node_operation_node_id ON t_cube_node_operation(node_id);
CREATE INDEX IF NOT EXISTS idx_node_operation_created_at ON t_cube_node_operation(created_at);

-- +goose Down

DROP TABLE IF EXISTS t_cube_node_operation;
