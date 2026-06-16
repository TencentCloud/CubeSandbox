// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;

use crate::{
    cubemaster::{CubeMasterClient, CubeMasterError, ListSandboxRequest, NodeSnapshot},
    error::{AppError, AppResult},
    models::{
        ClusterOverview, ComponentMatrixRowView, ComponentVersionGroupView, ComponentVersionView,
        ControlPlaneVersionView, NodeComponentEntryView, NodeConditionView, NodeResourcesView,
        NodeVersionRowView, NodeView, VersionMatrixView,
    },
};

#[derive(Clone)]
pub struct ClusterService {
    cubemaster: CubeMasterClient,
}

impl ClusterService {
    pub fn new(cubemaster: CubeMasterClient) -> Self {
        Self { cubemaster }
    }

    pub async fn cluster_overview(&self) -> AppResult<ClusterOverview> {
        let resp = self.cubemaster.list_nodes().await.map_err(map_err)?;
        let used_map = self.fetch_used_resources().await;
        Ok(build_overview_with_used(&resp.data, &used_map))
    }

    pub async fn list_nodes(&self) -> AppResult<Vec<NodeView>> {
        let resp = self.cubemaster.list_nodes().await.map_err(map_err)?;
        let used_map = self.fetch_used_resources().await;
        Ok(resp
            .data
            .into_iter()
            .map(|s| to_view_with_used(s, &used_map))
            .collect())
    }

    pub async fn get_node(&self, node_id: &str) -> AppResult<NodeView> {
        let resp = self.cubemaster.get_node(node_id).await.map_err(map_err)?;
        let snapshot = resp
            .data
            .ok_or_else(|| AppError::NotFound(format!("node {} not found", node_id)))?;
        let used_map = self.fetch_used_resources().await;
        Ok(to_view_with_used(snapshot, &used_map))
    }

    /// Fetch the cluster-wide component version matrix. When the underlying
    /// CubeMaster predates the endpoint (404), an empty matrix is returned so
    /// the UI degrades gracefully rather than surfacing an error.
    pub async fn version_matrix(&self) -> AppResult<VersionMatrixView> {
        match self.cubemaster.get_version_matrix().await {
            Ok(resp) => Ok(to_version_matrix_view(resp.data.unwrap_or_default())),
            Err(e) if e.is_endpoint_missing() => Ok(VersionMatrixView::default()),
            Err(e) => Err(map_err(e)),
        }
    }

    /// Fetch all running sandboxes and aggregate cpu_milli / memory_mb used per host IP.
    /// Returns a map of host_ip -> (used_cpu_milli, used_memory_mb).
    /// On any error the map is empty and saturation falls back to CubeMaster values.
    async fn fetch_used_resources(&self) -> HashMap<String, (i64, i64)> {
        let req = ListSandboxRequest {
            request_id: uuid::Uuid::new_v4().to_string(),
            instance_type: String::new(),
            host_id: None,
            start_idx: Some(1),
            size: Some(500),
            filter: None,
        };
        let Ok(resp) = self.cubemaster.list_sandboxes(&req).await else {
            return HashMap::new();
        };
        let mut map: HashMap<String, (i64, i64)> = HashMap::new();
        for sb in &resp.sandboxes {
            // only count running sandboxes
            if sb.status.to_lowercase() != "running" {
                continue;
            }
            let entry = map.entry(sb.host_id.clone()).or_default();
            // cpu_count is in cores; convert to millicores
            entry.0 += sb.cpu_count as i64 * 1000;
            entry.1 += sb.memory_mb as i64;
        }
        map
    }
}

fn map_err(e: CubeMasterError) -> AppError {
    if e.is_not_found() || e.is_endpoint_missing() {
        AppError::NotFound(e.to_string())
    } else {
        AppError::Internal(anyhow::anyhow!(e))
    }
}

pub(crate) fn build_overview(nodes: &[NodeSnapshot]) -> ClusterOverview {
    build_overview_with_used(nodes, &HashMap::new())
}

fn build_overview_with_used(
    nodes: &[NodeSnapshot],
    used_map: &HashMap<String, (i64, i64)>,
) -> ClusterOverview {
    let mut overview = ClusterOverview {
        node_count: nodes.len(),
        ..Default::default()
    };

    for n in nodes {
        if n.healthy {
            overview.healthy_nodes += 1;
        }
        overview.total_cpu_milli += n.capacity.milli_cpu;
        overview.total_memory_mb += n.capacity.memory_mb;
        overview.max_mvm_slots += n.max_mvm_num;

        // Use sandbox-aggregated used resources if available; fall back to CubeMaster allocatable.
        if let Some(&(used_cpu, used_mem)) = used_map.get(&n.host_ip) {
            let alloc_cpu = (n.capacity.milli_cpu - used_cpu).max(0);
            let alloc_mem = (n.capacity.memory_mb - used_mem).max(0);
            overview.allocatable_cpu_milli += alloc_cpu;
            overview.allocatable_memory_mb += alloc_mem;
        } else {
            overview.allocatable_cpu_milli += n.allocatable.milli_cpu;
            overview.allocatable_memory_mb += n.allocatable.memory_mb;
        }
    }

    overview
}

/// Build a NodeView, overriding allocatable with sandbox-based actual usage when available.
pub(crate) fn to_view_with_used(
    s: NodeSnapshot,
    used_map: &HashMap<String, (i64, i64)>,
) -> NodeView {
    let cap_cpu_milli = s.capacity.milli_cpu;
    let cap_mem = s.capacity.memory_mb;

    // Use sandbox-aggregated usage if available; fall back to CubeMaster allocatable diff.
    let (used_cpu_milli, used_mem_mb) = if let Some(&(cpu, mem)) = used_map.get(&s.host_ip) {
        (cpu, mem)
    } else {
        (
            (cap_cpu_milli - s.allocatable.milli_cpu).max(0),
            (cap_mem - s.allocatable.memory_mb).max(0),
        )
    };

    let alloc_cpu_milli = (cap_cpu_milli - used_cpu_milli).max(0);
    let alloc_mem_mb = (cap_mem - used_mem_mb).max(0);

    let cpu_saturation = saturation_pct(cap_cpu_milli, alloc_cpu_milli);
    let memory_saturation = saturation_pct(cap_mem, alloc_mem_mb);

    NodeView {
        node_id: s.node_id,
        host_ip: s.host_ip,
        instance_type: s.instance_type,
        healthy: s.healthy,
        capacity: NodeResourcesView {
            cpu_milli: cap_cpu_milli,
            memory_mb: cap_mem,
        },
        allocatable: NodeResourcesView {
            cpu_milli: alloc_cpu_milli,
            memory_mb: alloc_mem_mb,
        },
        cpu_saturation,
        memory_saturation,
        max_mvm_slots: s.max_mvm_num,
        quota_cpu: s.quota_cpu,
        quota_mem_mb: s.quota_mem_mb,
        create_concurrent_num: s.create_concurrent_num,
        heartbeat_time: s.heartbeat_time,
        conditions: s
            .conditions
            .into_iter()
            .map(|c| NodeConditionView {
                kind: c.kind,
                status: c.status,
                last_heartbeat_time: c.last_heartbeat_time,
                reason: c.reason,
                message: c.message,
            })
            .collect(),
        local_templates: s
            .local_templates
            .into_iter()
            .map(|t| t.template_id)
            .collect(),
        versions: s
            .versions
            .into_iter()
            .map(|v| ComponentVersionView {
                component: v.component,
                version: v.version,
                commit: v.commit,
                build_time: v.build_time,
                source: v.source,
            })
            .collect(),
    }
}

/// Keep the old signature for tests (no sandbox data, pure CubeMaster values).
#[cfg(test)]
pub(crate) fn to_view(s: NodeSnapshot) -> NodeView {
    to_view_with_used(s, &HashMap::new())
}

fn to_version_matrix_view(m: crate::cubemaster::VersionMatrix) -> VersionMatrixView {
    VersionMatrixView {
        control_plane: ControlPlaneVersionView {
            version: m.control_plane.version,
            commit: m.control_plane.commit,
            build_time: m.control_plane.build_time,
        },
        components: m
            .components
            .into_iter()
            .map(|c| ComponentMatrixRowView {
                component: c.component,
                declared_version: c.declared_version,
                declared_versions: c.declared_versions,
                consistent: c.consistent,
                versions: c
                    .versions
                    .into_iter()
                    .map(|g| ComponentVersionGroupView {
                        version: g.version,
                        nodes: g.nodes,
                    })
                    .collect(),
            })
            .collect(),
        nodes: m
            .nodes
            .into_iter()
            .map(|n| NodeVersionRowView {
                node_id: n.node_id,
                healthy: n.healthy,
                components: n
                    .components
                    .into_iter()
                    .map(|e| NodeComponentEntryView {
                        component: e.component,
                        version: e.version,
                        declared: e.declared,
                    })
                    .collect(),
            })
            .collect(),
    }
}

pub(crate) fn saturation_pct(total: i64, allocatable: i64) -> f32 {
    if total <= 0 {
        return 0.0;
    }

    let used = (total - allocatable).max(0) as f32;
    ((used / total as f32) * 100.0).clamp(0.0, 100.0)
}

#[cfg(test)]
mod tests {
    use super::{build_overview, saturation_pct, to_view};
    use crate::cubemaster::{LocalTemplate, NodeCondition, NodeResources, NodeSnapshot};

    #[test]
    fn saturation_is_clamped() {
        assert_eq!(saturation_pct(0, 0), 0.0);
        assert_eq!(saturation_pct(10, 15), 0.0);
        assert_eq!(saturation_pct(10, 0), 100.0);
    }

    #[test]
    fn builds_views_and_overview_from_snapshots() {
        let snapshot = NodeSnapshot {
            node_id: "node-a".to_string(),
            host_ip: "10.0.0.1".to_string(),
            instance_type: "cubebox".to_string(),
            healthy: true,
            capacity: NodeResources {
                milli_cpu: 2200,
                memory_mb: 4096,
            },
            allocatable: NodeResources {
                milli_cpu: 1000,
                memory_mb: 2048,
            },
            max_mvm_num: 3,
            heartbeat_time: None,
            conditions: vec![NodeCondition {
                kind: "Ready".to_string(),
                status: "True".to_string(),
                last_heartbeat_time: None,
                last_transition_time: None,
                reason: String::new(),
                message: String::new(),
            }],
            local_templates: vec![LocalTemplate {
                template_id: "tmpl-1".to_string(),
                ..Default::default()
            }],
            ..Default::default()
        };

        let view = to_view(snapshot.clone());
        assert_eq!(view.node_id, "node-a");
        assert_eq!(view.capacity.cpu_milli, 2200);
        assert_eq!(view.allocatable.cpu_milli, 1000);
        assert_eq!(view.local_templates, vec!["tmpl-1".to_string()]);

        let overview = build_overview(&[snapshot]);
        assert_eq!(overview.node_count, 1);
        assert_eq!(overview.healthy_nodes, 1);
        assert_eq!(overview.total_cpu_milli, 2200);
        assert_eq!(overview.allocatable_cpu_milli, 1000);
        assert_eq!(overview.max_mvm_slots, 3);
    }
}
