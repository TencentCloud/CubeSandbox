// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! API-facing constants shared across sandbox responses.

/// Conservative fallback `envdVersion` used when a sandbox carries no collected
/// version annotation (e.g. legacy templates). Kept at the historical value so
/// e2b SDK feature gating degrades safely.
pub const ENVD_VERSION_FALLBACK: &str = "0.2.0";

/// Sandbox annotation key carrying the real envd version, collected at template
/// creation time by CubeMaster/Cubelet and propagated to sandbox instances.
pub const ENVD_VERSION_ANNOTATION: &str = "cube.master.components.envd.version";

/// Sandbox annotation key carrying the template alias (display_name) or a named
/// snapshot's name. Absent for unnamed snapshots and legacy sandboxes.
pub const TEMPLATE_ALIAS_ANNOTATION: &str = "cube.master.appsnapshot.template.alias";
