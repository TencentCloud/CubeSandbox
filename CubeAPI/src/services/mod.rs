// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

pub mod cluster;
pub mod sandboxes;
pub mod snapshots;
pub mod templates;

use crate::{
    config::ServerConfig,
    cubemaster::CubeMasterClient,
    error::{AppError, AppResult},
};

const DENY_ALL_IPV4_CIDR: &str = "0.0.0.0/0";
const ALLOW_OUT_DOMAIN_REQUIRES_DENY_ALL: &str =
    "When specifying allowed domains in allow_out, you must disable public outbound traffic or include '0.0.0.0/0' in deny_out to block all other traffic.";

pub(crate) fn validate_allow_out_domains_require_deny_all(
    allow_out: &[String],
    deny_out: &[String],
    default_deny_all: bool,
) -> AppResult<()> {
    if !allow_out
        .iter()
        .any(|target| is_domain_allow_out_target(target))
    {
        return Ok(());
    }
    if default_deny_all
        || deny_out
            .iter()
            .any(|target| target.trim() == DENY_ALL_IPV4_CIDR)
    {
        return Ok(());
    }
    Err(AppError::BadRequest(
        ALLOW_OUT_DOMAIN_REQUIRES_DENY_ALL.to_string(),
    ))
}

fn is_domain_allow_out_target(target: &str) -> bool {
    let target = target.trim();
    if target.is_empty() || target.contains('/') || target.parse::<std::net::IpAddr>().is_ok() {
        return false;
    }
    if is_dotted_decimal_like_target(target) {
        return false;
    }

    let mut domain = target.trim_end_matches('.').to_ascii_lowercase();
    if let Some(stripped) = domain.strip_prefix("*.") {
        domain = stripped.to_string();
    } else if domain.contains('*') {
        return false;
    }
    is_valid_dns_domain_name(&domain)
}

fn is_dotted_decimal_like_target(target: &str) -> bool {
    let parts: Vec<&str> = target.trim_end_matches('.').split('.').collect();
    parts.len() == 4
        && parts
            .iter()
            .all(|part| !part.is_empty() && part.chars().all(|ch| ch.is_ascii_digit()))
}

fn is_valid_dns_domain_name(domain: &str) -> bool {
    !domain.is_empty()
        && domain.len() < 255
        && domain.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && !label.starts_with('-')
                && !label.ends_with('-')
                && label
                    .chars()
                    .all(|ch| ch.is_ascii_lowercase() || ch.is_ascii_digit() || ch == '-')
        })
}

#[derive(Clone)]
pub struct AppServices {
    pub cluster: cluster::ClusterService,
    pub sandboxes: sandboxes::SandboxService,
    pub snapshots: snapshots::SnapshotService,
    pub templates: templates::TemplateService,
}

impl AppServices {
    pub fn new(config: &ServerConfig, cubemaster: CubeMasterClient) -> Self {
        Self {
            cluster: cluster::ClusterService::new(cubemaster.clone()),
            sandboxes: sandboxes::SandboxService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
                config.sandbox_domain.clone(),
            ),
            snapshots: snapshots::SnapshotService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
            ),
            templates: templates::TemplateService::new(cubemaster, config.instance_type.clone()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::validate_allow_out_domains_require_deny_all;

    #[test]
    fn allow_out_domain_requires_deny_all_or_default_deny_all() {
        let err = validate_allow_out_domains_require_deny_all(
            &["api.example.com".to_string()],
            &[],
            false,
        )
        .unwrap_err();
        assert!(err
            .to_string()
            .contains("must disable public outbound traffic or include '0.0.0.0/0' in deny_out"));
    }

    #[test]
    fn allow_out_domain_accepts_explicit_deny_all() {
        validate_allow_out_domains_require_deny_all(
            &["*.example.com".to_string()],
            &["0.0.0.0/0".to_string()],
            false,
        )
        .expect("deny-all should satisfy domain allow_out requirement");
    }

    #[test]
    fn allow_out_domain_accepts_default_deny_all() {
        validate_allow_out_domains_require_deny_all(&["*.example.com".to_string()], &[], true)
            .expect("default deny-all should satisfy domain allow_out requirement");
    }

    #[test]
    fn allow_out_cidr_does_not_require_deny_all() {
        validate_allow_out_domains_require_deny_all(
            &["203.0.113.0/24".to_string(), "8.8.8.8".to_string()],
            &[],
            false,
        )
        .expect("IP/CIDR allow_out targets should not require deny-all");
    }
}
