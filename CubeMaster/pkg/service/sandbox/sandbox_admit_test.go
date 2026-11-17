// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/hostcompat"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestAdmitSelectedHost(t *testing.T) {
	mk := func(id string, disabled bool) *node.Node {
		n := &node.Node{InsID: id, IP: "10.0.0.1"}
		n.SetSchedulingDisabled(disabled)
		return n
	}
	cacheOf := func(nodes ...*node.Node) func(string) (*node.Node, bool) {
		m := make(map[string]*node.Node, len(nodes))
		for _, n := range nodes {
			m[n.ID()] = n
		}
		return func(id string) (*node.Node, bool) {
			n, ok := m[id]
			return n, ok
		}
	}

	t.Run("nil host", func(t *testing.T) {
		got, err := admitSelectedHost(nil, cacheOf())
		assert.Nil(t, got)
		assertAdmitFailed(t, err, "no selected host")
	})

	t.Run("cache miss", func(t *testing.T) {
		got, err := admitSelectedHost(mk("missing", false), cacheOf())
		assert.Nil(t, got)
		assertAdmitFailed(t, err, "missing from cache")
	})

	t.Run("cordoned", func(t *testing.T) {
		host := mk("n1", true)
		got, err := admitSelectedHost(mk("n1", false), cacheOf(host))
		assert.Nil(t, got)
		assertAdmitFailed(t, err, "scheduling-disabled")
	})

	t.Run("schedulable refreshes host", func(t *testing.T) {
		fresh := mk("n1", false)
		fresh.IP = "10.0.0.9"
		selected := mk("n1", false)
		selected.IP = "10.0.0.1"

		got, err := admitSelectedHost(selected, cacheOf(fresh))
		assert.NoError(t, err)
		assert.Same(t, fresh, got)
		assert.Equal(t, "10.0.0.9", got.HostIP())
	})
}

func TestRefreshAndAdmitHostUsesLocalcache(t *testing.T) {
	c := &createSandboxContext{}
	err := c.refreshAndAdmitHost()
	assertAdmitFailed(t, err, "no selected host")
}

func TestBindJFSTemplateVariant(t *testing.T) {
	SetJFSTemplateVariantResolver(nil)
	t.Cleanup(func() { SetJFSTemplateVariantResolver(nil) })
	host := &node.Node{
		InsID: "n1",
		HostFacts: &node.HostFacts{
			CPUIDHash:         "sha256:cpu",
			HostKernelRelease: "5.15.0",
		},
	}
	c := &createSandboxContext{
		selectHost: host,
		cubeletReq: &cubeboxv1.RunCubeSandboxRequest{Annotations: map[string]string{
			constants.CubeAnnotationStorageBackend:           "jfs",
			constants.CubeAnnotationAppSnapshotTemplateID:    "tpl-a",
			constants.CubeAnnotationJFSTemplateVariantPolicy: hostcompat.VariantPolicyV1,
		}},
	}
	SetJFSTemplateVariantResolver(func(_ context.Context, logicalID, compatKey string) (string, error) {
		got, _, ok := hostcompat.TemplateVariantID(logicalID, "sha256:cpu", "5.15.0")
		if !ok || compatKey == "" {
			t.Fatalf("resolver received logical=%q compat=%q", logicalID, compatKey)
		}
		return got, nil
	})
	assert.NoError(t, c.bindJFSTemplateVariant())
	want, _, ok := hostcompat.TemplateVariantID("tpl-a", "sha256:cpu", "5.15.0")
	assert.True(t, ok)
	assert.Equal(t, want, c.cubeletReq.Annotations[constants.CubeAnnotationJFSTemplateVariantID])
	assert.Equal(t, "tpl-a", c.cubeletReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
}

func TestBindJFSTemplateVariantLeavesLegacyTemplateLogical(t *testing.T) {
	SetJFSTemplateVariantResolver(nil)
	t.Cleanup(func() { SetJFSTemplateVariantResolver(nil) })
	c := &createSandboxContext{
		selectHost: &node.Node{InsID: "n1"},
		cubeletReq: &cubeboxv1.RunCubeSandboxRequest{Annotations: map[string]string{
			constants.CubeAnnotationStorageBackend:        "jfs",
			constants.CubeAnnotationAppSnapshotTemplateID: "tpl-old",
			constants.CubeAnnotationJFSTemplateVariantID:  "forged",
		}},
	}
	assert.NoError(t, c.bindJFSTemplateVariant())
	assert.Empty(t, c.cubeletReq.Annotations[constants.CubeAnnotationJFSTemplateVariantID])
}

func TestBindJFSTemplateVariantFailsClosedWithoutResolver(t *testing.T) {
	SetJFSTemplateVariantResolver(nil)
	t.Cleanup(func() { SetJFSTemplateVariantResolver(nil) })
	c := &createSandboxContext{
		selectHost: &node.Node{
			InsID: "n1",
			HostFacts: &node.HostFacts{
				CPUIDHash:         "sha256:cpu",
				HostKernelRelease: "5.15.0",
			},
		},
		cubeletReq: &cubeboxv1.RunCubeSandboxRequest{Annotations: map[string]string{
			constants.CubeAnnotationStorageBackend:           "jfs",
			constants.CubeAnnotationAppSnapshotTemplateID:    "tpl-a",
			constants.CubeAnnotationJFSTemplateVariantPolicy: hostcompat.VariantPolicyV1,
		}},
	}
	err := c.bindJFSTemplateVariant()
	assertAdmitFailed(t, err, "resolver is not initialized")
}

func assertAdmitFailed(t *testing.T, err error, msgSubstr string) {
	t.Helper()
	assert.Error(t, err)
	status, ok := ret.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_SelectNodesFailed, status.Code())
	assert.True(t, strings.Contains(status.Message(), msgSubstr), "msg=%q want substr %q", status.Message(), msgSubstr)
}
