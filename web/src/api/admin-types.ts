// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Admin DTOs served by CubeOps (cluster/nodes/version matrix). They used to
// ride along in the generated schema.ts from a stale openapi.yml that still
// carried the CubeOps surface; openapi.yml now only exports CubeAPI's E2B
// surface and CubeOps has no OpenAPI export of its own, so these types are
// maintained by hand. Keep them in sync with CubeOps/internal/httpapi.
export interface ClusterOverview {
  /** Format: int64. Currently-allocatable CPU in millicpu. */
  allocatableCpuMilli: number;
  /** Format: int64. Currently-allocatable memory in MiB. */
  allocatableMemoryMB: number;
  healthyNodes: number;
  /** Format: int64. Sum of every node's maximum MVM slots. */
  maxMvmSlots: number;
  nodeCount: number;
  /** Format: int64. Total CPU across the cluster, expressed in millicpu. */
  totalCpuMilli: number;
  /** Format: int64. Total memory in MiB. */
  totalMemoryMB: number;
}

/** Per-component aggregation across all nodes. */
export interface ComponentMatrixRowView {
  component: string;
  consistent: boolean;
  declaredVersion?: string;
  declaredVersions?: string[];
  versions: ComponentVersionGroupView[];
}

/** A group of nodes that report the same version of a component. */
export interface ComponentVersionGroupView {
  nodes: string[];
  version: string;
}

/** One component's version on a node. */
export interface ComponentVersionView {
  buildTime?: string;
  commit?: string;
  component: string;
  source?: string;
  version?: string;
}

/** Control-plane reference version (the cluster's target version). */
export interface ControlPlaneVersionView {
  buildTime?: string;
  commit?: string;
  version: string;
}

/** A single component version on a single node, with release declaration membership. */
export interface NodeComponentEntryView {
  component: string;
  declared: boolean;
  version: string;
}

export interface NodeConditionView {
  /** Format: date-time */
  lastHeartbeatTime?: string | null;
  message?: string;
  reason?: string;
  status: string;
  type: string;
}

export interface NodeResourcesView {
  /** Format: int64. CPU capacity or availability expressed in millicpu. */
  cpuMilli: number;
  /** Memory in MiB; the MB suffix is retained for API compatibility. Format: int64 */
  memoryMB: number;
}

/** Per-node view of the version matrix. */
export interface NodeVersionRowView {
  components: NodeComponentEntryView[];
  healthy: boolean;
  nodeID: string;
}

export interface NodeView {
  allocatable: NodeResourcesView;
  capacity: NodeResourcesView;
  conditions?: NodeConditionView[];
  /** Format: float. Percentage (0-100) of CPU currently in use. */
  cpuSaturation: number;
  /** Format: int64. Max concurrent sandbox-create operations on this node. */
  createConcurrentNum: number;
  healthy: boolean;
  /** Format: date-time */
  heartbeatTime?: string | null;
  hostIP: string;
  instanceType?: string;
  /**
   * Node labels as stored by CubeOps, including control-plane-owned keys.
   * Keep in sync with CubeOps/internal/nodemanagement/handler/common.go.
   */
  labels?: Record<string, string>;
  /** Catalog machine type bound to this node. Empty when unassigned. */
  machineType?: string;
  /** Host identity used for cross-node restore. */
  hostFacts?: NodeHostFactsView | null;
  localTemplates?: string[];
  /** Format: int64 */
  maxMvmSlots: number;
  /** Format: float. Percentage (0-100) of memory currently in use. */
  memorySaturation: number;
  nodeID: string;
  /** Format: int64. CPU quota in millicores assigned to this node. */
  quotaCpu: number;
  /** Format: int64. Memory quota in MiB assigned to this node. */
  quotaMemMB: number;
  versions?: ComponentVersionView[];
}

/** Host identity CubeMaster compares when it picks a cross-node restore target. */
export interface NodeHostFactsView {
  cpuVendor?: string;
  cpuModel?: string;
  cpuidHash?: string;
  hostKernelRelease?: string;
  kvmModuleTaint?: string;
}

/** Full node x component version matrix. */
export interface VersionMatrixView {
  components: ComponentMatrixRowView[];
  controlPlane: ControlPlaneVersionView;
  nodes: NodeVersionRowView[];
}
