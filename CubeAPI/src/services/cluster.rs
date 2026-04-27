// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::{
    cubemaster::{CubeMasterClient, CubeMasterError, NodeSnapshot},
    error::{AppError, AppResult},
    models::{ClusterOverview, NodeConditionView, NodeResourcesView, NodeView},
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
        Ok(build_overview(&resp.data))
    }

    pub async fn list_nodes(&self) -> AppResult<Vec<NodeView>> {
        let resp = self.cubemaster.list_nodes().await.map_err(map_err)?;
        Ok(resp.data.into_iter().map(to_view).collect())
    }

    pub async fn get_node(&self, node_id: &str) -> AppResult<NodeView> {
        let resp = self.cubemaster.get_node(node_id).await.map_err(map_err)?;
        let snapshot = resp
            .data
            .ok_or_else(|| AppError::NotFound(format!("node {} not found", node_id)))?;
        Ok(to_view(snapshot))
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
    let mut overview = ClusterOverview {
        node_count: nodes.len(),
        ..Default::default()
    };

    for n in nodes {
        if n.healthy {
            overview.healthy_nodes += 1;
        }
        overview.total_cpu_milli += n.capacity.milli_cpu;
        overview.allocatable_cpu_milli += n.allocatable.milli_cpu;
        overview.total_memory_mb += n.capacity.memory_mb;
        overview.allocatable_memory_mb += n.allocatable.memory_mb;
        overview.max_mvm_slots += n.max_mvm_num;
    }

    overview
}

pub(crate) fn to_view(s: NodeSnapshot) -> NodeView {
    let cap_cpu_milli = s.capacity.milli_cpu;
    let alloc_cpu_milli = s.allocatable.milli_cpu;
    let cap_mem = s.capacity.memory_mb;
    let alloc_mem = s.allocatable.memory_mb;

    let cpu_saturation = saturation_pct(cap_cpu_milli, alloc_cpu_milli);
    let memory_saturation = saturation_pct(cap_mem, alloc_mem);

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
            memory_mb: alloc_mem,
        },
        cpu_saturation,
        memory_saturation,
        max_mvm_slots: s.max_mvm_num,
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
