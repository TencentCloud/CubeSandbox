// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Cgroup hierarchy mode for templates.
//!
//! A mode selects the cgroup hierarchy a template and every sandbox created
//! from it boot with. The platform default is the unified (v2) hierarchy;
//! guests that require cgroup v1 (Android-based images) select `"legacy"`.

use std::fmt;

/// Stable error identifiers for the cgroup-mode contract.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CgroupModeError {
    Invalid(String),
}

impl CgroupModeError {
    pub fn code(&self) -> &'static str {
        match self {
            Self::Invalid(_) => "cgroup_mode_invalid",
        }
    }

    pub fn message(&self) -> String {
        match self {
            // The sentence after the code prefix matches CubeMaster's
            // `normalizeTemplateImageRequest` rejection so operators can
            // grep one wording across components.
            Self::Invalid(raw) => format!(
                "{}: invalid cgroup_mode {:?}: supported values are \"\" (default, unified v2) and \"legacy\"",
                self.code(),
                raw
            ),
        }
    }
}

impl fmt::Display for CgroupModeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message())
    }
}

impl std::error::Error for CgroupModeError {}

/// Normalize a client-supplied cgroup mode for template creation.
///
/// Mirrors CubeMaster's `normalizeTemplateImageRequest` cgroup handling so
/// invalid values fail fast at the API edge instead of surfacing later in
/// the build job:
///
/// - absent / empty / whitespace-only → `None` (default unified v2 hierarchy)
/// - `"legacy"` → `Some("legacy")` (cgroup v1, required by Android-based guests)
/// - anything else → [`CgroupModeError::Invalid`]
///
/// Matching is exact after trimming, same as CubeMaster; a one-word
/// vocabulary gets no case folding.
pub fn normalize_request(raw: Option<&str>) -> Result<Option<String>, CgroupModeError> {
    match raw.map(str::trim).filter(|mode| !mode.is_empty()) {
        None => Ok(None),
        Some("legacy") => Ok(Some("legacy".to_string())),
        Some(other) => Err(CgroupModeError::Invalid(other.to_string())),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn absent_or_blank_means_default_unified() {
        assert_eq!(normalize_request(None).unwrap(), None);
        assert_eq!(normalize_request(Some("")).unwrap(), None);
        assert_eq!(normalize_request(Some("   ")).unwrap(), None);
    }

    #[test]
    fn legacy_round_trips_after_trim() {
        assert_eq!(
            normalize_request(Some("legacy")).unwrap(),
            Some("legacy".to_string())
        );
        assert_eq!(
            normalize_request(Some(" legacy ")).unwrap(),
            Some("legacy".to_string())
        );
    }

    #[test]
    fn case_is_not_folded_like_cube_master() {
        let err = normalize_request(Some("Legacy")).unwrap_err();
        assert_eq!(err.code(), "cgroup_mode_invalid");
    }

    #[test]
    fn unknown_values_carry_the_stable_code_and_wording() {
        let err = normalize_request(Some("v1")).unwrap_err();
        assert_eq!(err.code(), "cgroup_mode_invalid");
        let message = err.message();
        assert!(message.contains("cgroup_mode_invalid"), "{message}");
        assert!(
            message.contains("invalid cgroup_mode \"v1\": supported values are"),
            "{message}"
        );
    }
}
