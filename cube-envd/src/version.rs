// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! cube-envd 自身的版本标识。
//!
//! 与参考实现 e2b envd 的 `packages/envd/pkg/version.go` 同构：**版本属于构建产物
//! 本身，由一个入库的 semver 常量承载**，而不是由构建渠道（CI / Makefile / one-click /
//! 裸 cargo）各自派生。上游由 release-please 通过 magic comment 自动 bump；这里保留同
//! 形式的标记，未来接入自动化时无需改动取值方式。
//!
//! 为什么必须是 semver 而不是构建标识：
//!
//! - 下游按 `\d+\.\d+\.\d+` 提取该值（`Cubelet/services/cubebox/envd_version.go`、
//!   `CubeMaster/pkg/templatecenter/store.go`），写进模板/快照的
//!   `cube.master.components.envd.version` 注解，并作为公开 SDK 字段
//!   `SandboxInfo.envdVersion` 暴露；`sha-xxxxxxx` 这类构建标识会被直接丢弃。
//! - 上游把 envd 版本当作**能力门禁**比较（`packages/shared/pkg/utils/version.go`：
//!   `MinEnvdVersionForSnapshot` 等，非法格式会被判为 error），因此一个"可解析但错误"
//!   的值（例如把 CubeSandbox 的仓库 tag 当成 envd 版本）比空值更危险。
//!
//! 构建时的 commit 另行通过 `CUBE_ENVD_COMMIT` 注入，由 `-commit` 输出——版本与
//! commit 分工明确，与上游一致。
//!
//! 发布新版本时只需 bump 下面的常量（`Cargo.toml` 的 `package.version` 不参与该输出）。
pub const CUBE_ENVD_VERSION: &str = "0.1.0"; // x-cube-envd-version
