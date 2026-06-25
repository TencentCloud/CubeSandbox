-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Store template catalog table and seed data. CubeMaster owns shared database
-- schema migration through embedded goose migrations; CubeAPI only reads and
-- writes these rows.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260625015405_store_template', 60);

CREATE TABLE IF NOT EXISTS `t_store_template` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `item_id` varchar(128) NOT NULL,
  `name_key` varchar(255) NOT NULL DEFAULT '',
  `description_key` varchar(255) NOT NULL DEFAULT '',
  `image_cn` varchar(512) NOT NULL DEFAULT '',
  `image_intl` varchar(512) NOT NULL DEFAULT '',
  `digest` varchar(128) DEFAULT NULL,
  `tags` json DEFAULT NULL,
  `category` varchar(64) NOT NULL DEFAULT '',
  `size_mb` int NOT NULL DEFAULT 0,
  `expose_ports` json DEFAULT NULL,
  `probe_port` int NOT NULL DEFAULT 0,
  `probe_path` varchar(255) NOT NULL DEFAULT '/',
  `writable_layer_size` varchar(32) NOT NULL DEFAULT '1G',
  `official` tinyint(1) NOT NULL DEFAULT 0,
  `dns` json DEFAULT NULL,
  `sort_order` int NOT NULL DEFAULT 0,
  `deleted_at` datetime DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_item_id` (`item_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO `t_store_template` (
  `item_id`, `name_key`, `description_key`, `image_cn`, `image_intl`, `digest`,
  `tags`, `category`, `size_mb`, `expose_ports`, `probe_port`, `probe_path`,
  `writable_layer_size`, `official`, `dns`, `sort_order`, `deleted_at`
) VALUES
  ('sandbox-code', 'items.sandbox-code.name', 'items.sandbox-code.description',
   'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
   'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
   'sha256:a7b8654aac5b90e241b98e195ae1d8c85d59fe1fb8c282bcccf1071f877db20f',
   '["python", "jupyter", "official"]', 'code', 207, '[49983, 49999]', 49999, '/',
   '1G', 1, '[]', 0, NULL)
ON DUPLICATE KEY UPDATE
  `name_key` = VALUES(`name_key`),
  `description_key` = VALUES(`description_key`),
  `image_cn` = VALUES(`image_cn`),
  `image_intl` = VALUES(`image_intl`),
  `digest` = VALUES(`digest`),
  `tags` = VALUES(`tags`),
  `category` = VALUES(`category`),
  `size_mb` = VALUES(`size_mb`),
  `expose_ports` = VALUES(`expose_ports`),
  `probe_port` = VALUES(`probe_port`),
  `probe_path` = VALUES(`probe_path`),
  `writable_layer_size` = VALUES(`writable_layer_size`),
  `official` = VALUES(`official`),
  `dns` = VALUES(`dns`),
  `sort_order` = VALUES(`sort_order`),
  `deleted_at` = NULL;

INSERT INTO `t_store_template` (
  `item_id`, `name_key`, `description_key`, `image_cn`, `image_intl`, `digest`,
  `tags`, `category`, `size_mb`, `expose_ports`, `probe_port`, `probe_path`,
  `writable_layer_size`, `official`, `dns`, `sort_order`, `deleted_at`
) VALUES
  ('sandbox-browser', 'items.sandbox-browser.name', 'items.sandbox-browser.description',
   'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
   'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
   'sha256:1786786af8510c34eda64ebec5b0a61a98583cb311c3045c0222910ec0680d60',
   '["browser", "chromium", "official"]', 'browser', 1530, '[49983, 9000, 6080]', 9000, '/cdp/json/version',
   '1G', 1, '["183.60.83.19", "114.114.114.114", "223.5.5.5", "8.8.8.8"]', 1, NULL)
ON DUPLICATE KEY UPDATE
  `name_key` = VALUES(`name_key`),
  `description_key` = VALUES(`description_key`),
  `image_cn` = VALUES(`image_cn`),
  `image_intl` = VALUES(`image_intl`),
  `digest` = VALUES(`digest`),
  `tags` = VALUES(`tags`),
  `category` = VALUES(`category`),
  `size_mb` = VALUES(`size_mb`),
  `expose_ports` = VALUES(`expose_ports`),
  `probe_port` = VALUES(`probe_port`),
  `probe_path` = VALUES(`probe_path`),
  `writable_layer_size` = VALUES(`writable_layer_size`),
  `official` = VALUES(`official`),
  `dns` = VALUES(`dns`),
  `sort_order` = VALUES(`sort_order`),
  `deleted_at` = NULL;

-- Soft-delete the legacy single openclaw entry in case it was seeded by
-- a previous run of the CubeAPI Rust seed_store_templates() function.
UPDATE `t_store_template`
  SET `deleted_at` = NOW()
WHERE `item_id` = 'openclaw' AND `deleted_at` IS NULL;

INSERT INTO `t_store_template` (
  `item_id`, `name_key`, `description_key`, `image_cn`, `image_intl`, `digest`,
  `tags`, `category`, `size_mb`, `expose_ports`, `probe_port`, `probe_path`,
  `writable_layer_size`, `official`, `dns`, `sort_order`, `deleted_at`
) VALUES
  ('openclaw-lite', 'items.openclaw-lite.name', 'items.openclaw-lite.description',
   'cube-sandbox-image.tencentcloudcr.com/demo/lightweight-openclaw-deepseek-wecom:latest',
   'cube-sandbox-image.tencentcloudcr.com/demo/lightweight-openclaw-deepseek-wecom:latest',
   NULL,
   '["agent", "openclaw", "lite", "deepseek"]', 'ai', 2600, '[49983, 18789, 49999]', 49983, '/health',
   '2G', 1, '[]', 2, NULL)
ON DUPLICATE KEY UPDATE
  `name_key` = VALUES(`name_key`),
  `description_key` = VALUES(`description_key`),
  `image_cn` = VALUES(`image_cn`),
  `image_intl` = VALUES(`image_intl`),
  `digest` = VALUES(`digest`),
  `tags` = VALUES(`tags`),
  `category` = VALUES(`category`),
  `size_mb` = VALUES(`size_mb`),
  `expose_ports` = VALUES(`expose_ports`),
  `probe_port` = VALUES(`probe_port`),
  `probe_path` = VALUES(`probe_path`),
  `writable_layer_size` = VALUES(`writable_layer_size`),
  `official` = VALUES(`official`),
  `dns` = VALUES(`dns`),
  `sort_order` = VALUES(`sort_order`),
  `deleted_at` = NULL;

INSERT INTO `t_store_template` (
  `item_id`, `name_key`, `description_key`, `image_cn`, `image_intl`, `digest`,
  `tags`, `category`, `size_mb`, `expose_ports`, `probe_port`, `probe_path`,
  `writable_layer_size`, `official`, `dns`, `sort_order`, `deleted_at`
) VALUES
  ('openclaw-aio', 'items.openclaw-aio.name', 'items.openclaw-aio.description',
   'cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest',
   'cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest',
   'sha256:47680d7bc13ea7c57aeb88dff59ef2c44b0facb508e8c9066d479d7d458e0a66',
   '["agent", "openclaw", "aio", "browser", "deepseek"]', 'ai', 6350, '[49983, 18789, 8080]', 49983, '/health',
   '4G', 1, '[]', 3, NULL)
ON DUPLICATE KEY UPDATE
  `name_key` = VALUES(`name_key`),
  `description_key` = VALUES(`description_key`),
  `image_cn` = VALUES(`image_cn`),
  `image_intl` = VALUES(`image_intl`),
  `digest` = VALUES(`digest`),
  `tags` = VALUES(`tags`),
  `category` = VALUES(`category`),
  `size_mb` = VALUES(`size_mb`),
  `expose_ports` = VALUES(`expose_ports`),
  `probe_port` = VALUES(`probe_port`),
  `probe_path` = VALUES(`probe_path`),
  `writable_layer_size` = VALUES(`writable_layer_size`),
  `official` = VALUES(`official`),
  `dns` = VALUES(`dns`),
  `sort_order` = VALUES(`sort_order`),
  `deleted_at` = NULL;

INSERT INTO `t_store_template` (
  `item_id`, `name_key`, `description_key`, `image_cn`, `image_intl`, `digest`,
  `tags`, `category`, `size_mb`, `expose_ports`, `probe_port`, `probe_path`,
  `writable_layer_size`, `official`, `dns`, `sort_order`, `deleted_at`
) VALUES
  ('cubesandbox-base', 'items.cubesandbox-base.name', 'items.cubesandbox-base.description',
   'ghcr.io/tencentcloud/cubesandbox-base:latest',
   'ghcr.io/tencentcloud/cubesandbox-base:latest',
   NULL,
   '["base", "envd", "official"]', 'base', 98, '[49983]', 49983, '/health',
  '1G', 1, '[]', 4, NULL)
ON DUPLICATE KEY UPDATE
  `name_key` = VALUES(`name_key`),
  `description_key` = VALUES(`description_key`),
  `image_cn` = VALUES(`image_cn`),
  `image_intl` = VALUES(`image_intl`),
  `digest` = VALUES(`digest`),
  `tags` = VALUES(`tags`),
  `category` = VALUES(`category`),
  `size_mb` = VALUES(`size_mb`),
  `expose_ports` = VALUES(`expose_ports`),
  `probe_port` = VALUES(`probe_port`),
  `probe_path` = VALUES(`probe_path`),
  `writable_layer_size` = VALUES(`writable_layer_size`),
  `official` = VALUES(`official`),
  `dns` = VALUES(`dns`),
  `sort_order` = VALUES(`sort_order`),
  `deleted_at` = NULL;

INSERT INTO `t_store_template` (
  `item_id`, `name_key`, `description_key`, `image_cn`, `image_intl`, `digest`,
  `tags`, `category`, `size_mb`, `expose_ports`, `probe_port`, `probe_path`,
  `writable_layer_size`, `official`, `dns`, `sort_order`, `deleted_at`
) VALUES
  ('sandbox-nginx', 'items.sandbox-nginx.name', 'items.sandbox-nginx.description',
   'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-nginx:latest',
   'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-nginx:latest',
   NULL,
   '["nginx", "web", "official"]', 'web', 120, '[49983, 80]', 49983, '/health',
  '1G', 1, '[]', 5, NULL)
ON DUPLICATE KEY UPDATE
  `name_key` = VALUES(`name_key`),
  `description_key` = VALUES(`description_key`),
  `image_cn` = VALUES(`image_cn`),
  `image_intl` = VALUES(`image_intl`),
  `digest` = VALUES(`digest`),
  `tags` = VALUES(`tags`),
  `category` = VALUES(`category`),
  `size_mb` = VALUES(`size_mb`),
  `expose_ports` = VALUES(`expose_ports`),
  `probe_port` = VALUES(`probe_port`),
  `probe_path` = VALUES(`probe_path`),
  `writable_layer_size` = VALUES(`writable_layer_size`),
  `official` = VALUES(`official`),
  `dns` = VALUES(`dns`),
  `sort_order` = VALUES(`sort_order`),
  `deleted_at` = NULL;

SELECT RELEASE_LOCK('cubemaster_migration_20260625015405_store_template');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260625015405_store_template', 60);

DROP TABLE IF EXISTS `t_store_template`;

SELECT RELEASE_LOCK('cubemaster_migration_20260625015405_store_template');
