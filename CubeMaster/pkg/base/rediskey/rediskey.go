// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package rediskey centralizes Redis key construction so every key shares a
// unified namespace ("cube:{ver}:{scope}:{resource}[:{sub}]:{id}") instead of
// scattered string literals.
//
// Writes use the new namespaced keys only. Reads try the new key first and
// fall back to the legacy key so a simultaneous CubeMaster/CubeProxy upgrade
// can still see data written before the cutover.
//
// See docs/architecture/redis-key-spec.md for the full convention.
package rediskey

import "strings"

const (
	// Prefix is the fixed product namespace guarding against collisions with
	// third-party keys in the shared Redis instance.
	Prefix = "cube"
	// Version is the schema version; bump only on breaking value-structure
	// changes (adding fields / Hash members does not require a bump).
	Version = "v1"

	// ScopeMaster marks data read/written only by CubeMaster.
	ScopeMaster = "master"
	// ScopeProxy marks data private to CubeProxy.
	ScopeProxy = "proxy"
	// ScopeAPI marks data private to CubeAPI (reserved, not yet used).
	ScopeAPI = "api"
	// ScopeShared marks cross-service contracts (renaming needs coordination).
	ScopeShared = "shared"
)

func join(segs ...string) string { return strings.Join(segs, ":") }

// ---- standard key builders ----

// NodeMetric is the per-node resource metric Hash key.
func NodeMetric(nodeID string) string {
	return join(Prefix, Version, ScopeMaster, "node", "metric", nodeID)
}

// SandboxProxy is the sandbox proxy routing Hash key, shared with CubeProxy.
func SandboxProxy(sandboxID string) string {
	return join(Prefix, Version, ScopeShared, "sandbox", "proxy", sandboxID)
}

// InstanceInfo is the instance metadata Hash key.
func InstanceInfo(insID string) string {
	return join(Prefix, Version, ScopeMaster, "instance", "info", insID)
}

// DescribeTask is the async describe-task result Hash key.
func DescribeTask(taskID string) string {
	return join(Prefix, Version, ScopeMaster, "task", "describe", taskID)
}

// InstanceMeta is the generic instance metadata key (string/list).
func InstanceMeta(objs ...string) string {
	return join(append([]string{Prefix, Version, ScopeMaster, "instance", "meta"}, objs...)...)
}

// SandboxLifecycleMeta is the global HSet of sandbox lifecycle snapshots.
func SandboxLifecycleMeta() string {
	return join(Prefix, Version, ScopeShared, "sandbox", "lifecycle", "meta")
}

// SandboxLifecycleEvents is the append-only create/delete event stream.
func SandboxLifecycleEvents() string {
	return join(Prefix, Version, ScopeShared, "sandbox", "lifecycle", "events")
}

// SandboxLifecycleState is the per-sandbox pause/resume coordination key.
func SandboxLifecycleState(sandboxID string) string {
	return join(Prefix, Version, ScopeShared, "sandbox", "lifecycle", "state", sandboxID)
}

// ---- legacy key builders (read fallback / delete cleanup only) ----

// LegacyNodeMetric is the bare node ID used before namespacing.
func LegacyNodeMetric(nodeID string) string { return nodeID }

// LegacySandboxProxy is the pre-standardization sandbox proxy key.
func LegacySandboxProxy(sandboxID string) string { return "bypass_host_proxy:" + sandboxID }

// LegacyInstanceInfo is the pre-standardization instance info key.
func LegacyInstanceInfo(insID string) string { return "cube_instance_info:" + insID }

// LegacyDescribeTask is the pre-standardization describe-task key.
func LegacyDescribeTask(taskID string) string { return "describetask:" + taskID }

// LegacyInstanceMeta is the pre-standardization metadata key.
func LegacyInstanceMeta(objs ...string) string {
	return join(append([]string{"instance", "metadata"}, objs...)...)
}

// ReadKeysWithFallback returns the key try-order for reads: new first, legacy
// second. Used while legacy keys may still exist in Redis after upgrade.
func ReadKeysWithFallback(newKey, legacyKey string) []string {
	return []string{newKey, legacyKey}
}

// DeleteKeys returns both the new and legacy keys so teardown removes residue
// from either naming generation.
func DeleteKeys(newKey, legacyKey string) []string {
	return []string{newKey, legacyKey}
}
