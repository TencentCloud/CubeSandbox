// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemeta

import (
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

func sampleHostFacts() *node.HostFacts {
	return &node.HostFacts{
		CPUVendor:             "GenuineIntel",
		CPUModel:              "Xeon 8255C",
		CPUIDHash:             "sha256:cpu",
		HostKernelRelease:     "5.15.0",
		HostKernelFingerprint: "sha256:kernel",
		KVMAPIVersion:         12,
	}
}

// TestGetNodeHostFacts_Gating verifies GetNodeHostFacts returns facts only for
// a healthy node with non-zero facts.
func TestGetNodeHostFacts_Gating(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	healthyNode := &node.Node{InsID: "healthy", Healthy: true, HostFacts: sampleHostFacts()}
	noFactsNode := &node.Node{InsID: "no-facts", Healthy: true}
	unhealthyNode := &node.Node{InsID: "unhealthy", Healthy: false, HostFacts: sampleHostFacts()}

	patches.ApplyFunc(localcache.GetNode, func(id string) (*node.Node, bool) {
		switch id {
		case "healthy":
			return healthyNode, true
		case "no-facts":
			return noFactsNode, true
		case "unhealthy":
			return unhealthyNode, true
		default:
			return nil, false
		}
	})

	t.Run("healthy node with facts", func(t *testing.T) {
		got, ok := GetNodeHostFacts(nil, "healthy")
		assert.True(t, ok)
		assert.NotNil(t, got)
		assert.Equal(t, "sha256:kernel", got.HostKernelFingerprint)
	})

	t.Run("unknown node", func(t *testing.T) {
		_, ok := GetNodeHostFacts(nil, "missing")
		assert.False(t, ok)
	})

	t.Run("empty node id", func(t *testing.T) {
		_, ok := GetNodeHostFacts(nil, "  ")
		assert.False(t, ok)
	})

	t.Run("unhealthy node", func(t *testing.T) {
		_, ok := GetNodeHostFacts(nil, "unhealthy")
		assert.False(t, ok)
	})

	t.Run("healthy but no facts reported", func(t *testing.T) {
		_, ok := GetNodeHostFacts(nil, "no-facts")
		assert.False(t, ok)
	})
}

// TestGetPersistedNodeHostFacts verifies GetPersistedNodeHostFacts returns
// facts regardless of health (used by snapshot create as a backfill).
func TestGetPersistedNodeHostFacts(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	healthyNode := &node.Node{InsID: "healthy", Healthy: true, HostFacts: sampleHostFacts()}
	unhealthyNode := &node.Node{InsID: "unhealthy", Healthy: false, HostFacts: sampleHostFacts()}
	noFactsNode := &node.Node{InsID: "no-facts", Healthy: true}

	patches.ApplyFunc(localcache.GetNode, func(id string) (*node.Node, bool) {
		switch id {
		case "healthy":
			return healthyNode, true
		case "unhealthy":
			return unhealthyNode, true
		case "no-facts":
			return noFactsNode, true
		default:
			return nil, false
		}
	})

	t.Run("healthy node with facts", func(t *testing.T) {
		got, ok := GetPersistedNodeHostFacts(nil, "healthy")
		assert.True(t, ok)
		assert.NotNil(t, got)
		assert.Equal(t, "sha256:kernel", got.HostKernelFingerprint)
	})

	t.Run("unhealthy node still returns facts (no health gate)", func(t *testing.T) {
		got, ok := GetPersistedNodeHostFacts(nil, "unhealthy")
		assert.True(t, ok)
		assert.NotNil(t, got)
	})

	t.Run("unknown node", func(t *testing.T) {
		_, ok := GetPersistedNodeHostFacts(nil, "missing")
		assert.False(t, ok)
	})

	t.Run("empty node id", func(t *testing.T) {
		_, ok := GetPersistedNodeHostFacts(nil, "  ")
		assert.False(t, ok)
	})

	t.Run("node without facts", func(t *testing.T) {
		_, ok := GetPersistedNodeHostFacts(nil, "no-facts")
		assert.False(t, ok)
	})
}
