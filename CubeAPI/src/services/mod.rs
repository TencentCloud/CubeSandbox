// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

pub mod sandboxes;
pub mod snapshots;
pub mod templates;
pub mod volumes;

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
    pub sandboxes: sandboxes::SandboxService,
    pub snapshots: snapshots::SnapshotService,
    pub templates: templates::TemplateService,
    pub volumes: volumes::VolumeService,
}

impl AppServices {
    pub fn new(config: &ServerConfig, cubemaster: CubeMasterClient) -> Self {
        Self {
            sandboxes: sandboxes::SandboxService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
                config.sandbox_domain.clone(),
            ),
            snapshots: snapshots::SnapshotService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
            ),
            templates: templates::TemplateService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
            ),
            volumes: volumes::VolumeService::new(cubemaster),
        }
    }

    /// Decide whether `DELETE /templates/{id}` targets a snapshot. `snap-*` is a
    /// snapshot, `tpl-*` is never looked up by snapshot name, and a name that
    /// matches both a template and a snapshot (or several snapshots) is a 409.
    pub async fn resolve_snapshot_delete_target(&self, id: &str) -> AppResult<Option<String>> {
        let id = id.trim();
        if id.is_empty() || id.starts_with("tpl-") {
            return Ok(None);
        }
        if snapshots::is_snapshot_identity(id) {
            return Ok(Some(id.to_string()));
        }
        let mut ids = self.snapshots.ids_by_name(id).await?;
        if ids.is_empty() {
            return Ok(None);
        }
        match self.templates.get_template(id).await {
            Ok(_) => {
                return Err(AppError::Conflict(format!(
                "{id:?} matches both a template alias and a snapshot name; use a tpl- or snap- id"
            )))
            }
            Err(AppError::NotFound(_)) => {}
            Err(e) => return Err(e),
        }
        if ids.len() > 1 {
            return Err(snapshots::ambiguous_name_conflict(id));
        }
        Ok(ids.pop())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{extract::State, routing::get, Json, Router};
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };

    #[derive(Clone, Default)]
    struct Counters {
        snapshot_lists: Arc<AtomicUsize>,
        template_gets: Arc<AtomicUsize>,
    }

    /// Fake CubeMaster: `snapshots` is returned for any snapshot list, and
    /// `template` is `Some(json)` for a template GET response (None => 404 ret).
    async fn spawn_master(
        snapshots: Vec<&'static str>,
        template: Option<serde_json::Value>,
    ) -> (AppServices, Counters) {
        let counters = Counters::default();
        let state = (counters.clone(), snapshots, template);

        async fn list_snapshots(
            State((c, ids, _)): State<(Counters, Vec<&'static str>, Option<serde_json::Value>)>,
        ) -> Json<serde_json::Value> {
            c.snapshot_lists.fetch_add(1, Ordering::SeqCst);
            let items: Vec<_> = ids
                .iter()
                .map(|id| {
                    serde_json::json!({
                        "snapshot_id": id,
                        "display_name": "prod",
                        "status": "READY",
                    })
                })
                .collect();
            Json(serde_json::json!({
                "RequestID": "req-1",
                "ret": { "ret_code": 0, "ret_msg": "success" },
                "data": items,
                "next_token": ""
            }))
        }

        async fn get_template(
            State((c, _, tpl)): State<(Counters, Vec<&'static str>, Option<serde_json::Value>)>,
        ) -> Json<serde_json::Value> {
            c.template_gets.fetch_add(1, Ordering::SeqCst);
            Json(tpl.unwrap_or_else(|| {
                serde_json::json!({
                    "RequestID": "req-1",
                    "ret": { "ret_code": 130404, "ret_msg": "template not found" },
                })
            }))
        }

        let app = Router::new()
            .route("/cube/snapshot", get(list_snapshots))
            .route("/cube/template", get(get_template))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let addr = listener.local_addr().expect("listener addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server should run");
        });

        let client = CubeMasterClient::new(format!("http://{addr}"), reqwest::Client::new());
        (AppServices::new(&ServerConfig::default(), client), counters)
    }

    fn existing_template() -> Option<serde_json::Value> {
        Some(serde_json::json!({
            "RequestID": "req-1",
            "ret": { "ret_code": 0, "ret_msg": "success" },
            "template_id": "tpl-abc",
            "display_name": "prod",
            "status": "ready",
            "replicas": []
        }))
    }

    fn calls(counters: &Counters) -> (usize, usize) {
        (
            counters.snapshot_lists.load(Ordering::SeqCst),
            counters.template_gets.load(Ordering::SeqCst),
        )
    }

    #[tokio::test]
    async fn delete_target_empty_is_none() {
        let (svc, counters) = spawn_master(vec!["snap-a"], None).await;
        assert_eq!(
            svc.resolve_snapshot_delete_target("  ").await.unwrap(),
            None
        );
        assert_eq!(calls(&counters), (0, 0));
    }

    #[tokio::test]
    async fn delete_target_tpl_id_never_queries_snapshot_names() {
        let (svc, counters) = spawn_master(vec!["snap-a"], existing_template()).await;
        assert_eq!(
            svc.resolve_snapshot_delete_target("tpl-abc").await.unwrap(),
            None
        );
        assert_eq!(calls(&counters), (0, 0));
    }

    #[tokio::test]
    async fn delete_target_snap_id_resolves_to_itself_without_requests() {
        let (svc, counters) = spawn_master(vec![], existing_template()).await;
        assert_eq!(
            svc.resolve_snapshot_delete_target("  snap-abc ")
                .await
                .unwrap(),
            Some("snap-abc".to_string())
        );
        assert_eq!(calls(&counters), (0, 0));
    }

    #[tokio::test]
    async fn delete_target_name_without_snapshot_skips_template_probe() {
        let (svc, counters) = spawn_master(vec![], existing_template()).await;
        assert_eq!(
            svc.resolve_snapshot_delete_target("prod").await.unwrap(),
            None
        );
        assert_eq!(calls(&counters), (1, 0));
    }

    #[tokio::test]
    async fn delete_target_snapshot_only_name_returns_snapshot_id() {
        let (svc, counters) = spawn_master(vec!["snap-x"], None).await;
        assert_eq!(
            svc.resolve_snapshot_delete_target("prod").await.unwrap(),
            Some("snap-x".to_string())
        );
        assert_eq!(calls(&counters), (1, 1));
    }

    #[tokio::test]
    async fn delete_target_name_shared_with_template_is_conflict() {
        let (svc, _) = spawn_master(vec!["snap-x"], existing_template()).await;
        match svc.resolve_snapshot_delete_target("prod").await {
            Err(AppError::Conflict(msg)) => {
                assert!(msg.contains("template alias and a snapshot name"), "{msg}");
            }
            other => panic!("expected Conflict, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn delete_target_duplicate_snapshots_without_template_is_conflict() {
        let (svc, _) = spawn_master(vec!["snap-a", "snap-b"], None).await;
        match svc.resolve_snapshot_delete_target("prod").await {
            Err(AppError::Conflict(msg)) => assert!(msg.contains("ambiguous"), "{msg}"),
            other => panic!("expected Conflict, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn delete_target_template_probe_error_is_propagated() {
        let broken = Some(serde_json::json!({
            "RequestID": "req-1",
            "ret": { "ret_code": 500, "ret_msg": "boom" },
        }));
        let (svc, _) = spawn_master(vec!["snap-x"], broken).await;
        match svc.resolve_snapshot_delete_target("prod").await {
            Err(AppError::Internal(_)) => {}
            other => panic!("expected Internal, got {other:?}"),
        }
    }

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
