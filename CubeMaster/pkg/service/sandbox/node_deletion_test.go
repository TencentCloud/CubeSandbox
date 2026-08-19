// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
)

func TestDeleteNodeRejectsNonEmptyNode(t *testing.T) {
	patch := gomonkey.ApplyFunc(countNodeSandboxes, func(context.Context, string) (int, error) {
		return 2, nil
	})
	defer patch.Reset()

	assert.ErrorIs(t, DeleteNode(context.Background(), "node-a", false), ErrNodeHasSandboxes)
}

func TestDeleteNodeFailsClosedWhenInventoryUnavailable(t *testing.T) {
	sentinel := errors.New("cubelet unavailable")
	patch := gomonkey.ApplyFunc(countNodeSandboxes, func(context.Context, string) (int, error) {
		return 0, sentinel
	})
	defer patch.Reset()

	err := DeleteNode(context.Background(), "node-a", false)
	assert.ErrorIs(t, err, sentinel)
	assert.ErrorContains(t, err, "retry with force=true")
}

func TestCountNodeSandboxesDoesNotTrustCachedZeroWhenCubeletUnavailable(t *testing.T) {
	sentinel := errors.New("cubelet unavailable")
	origLocal := l
	l = &local{CubeletListLock: utils.NewResourceLocks()}
	t.Cleanup(func() { l = origLocal })
	nodePatch := gomonkey.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) {
		return &node.Node{
			InsID:        "node-a",
			IP:           "10.0.0.1",
			Healthy:      false,
			MvmNum:       0,
			MetricUpdate: time.Now(),
		}, true
	})
	defer nodePatch.Reset()
	listPatch := gomonkey.ApplyFunc(cubelet.List,
		func(context.Context, string, *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
			return nil, sentinel
		})
	defer listPatch.Reset()

	_, err := countNodeSandboxes(context.Background(), "node-a")
	assert.ErrorIs(t, err, sentinel)
}

func TestCountNodeSandboxesReturnsNotFoundWhenNodeIsAbsent(t *testing.T) {
	nodePatch := gomonkey.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) {
		return nil, false
	})
	defer nodePatch.Reset()
	metaPatch := gomonkey.ApplyFunc(nodemeta.GetNode,
		func(context.Context, string) (*nodemeta.NodeSnapshot, error) {
			return nil, nodemeta.ErrNodeNotFound
		})
	defer metaPatch.Reset()

	_, err := countNodeSandboxes(context.Background(), "missing-node")
	assert.ErrorIs(t, err, nodemeta.ErrNodeNotFound)
}

func TestCountNodeSandboxesDoesNotMisreportSchedulerCacheMissAsNotFound(t *testing.T) {
	nodePatch := gomonkey.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) {
		return nil, false
	})
	defer nodePatch.Reset()
	metaPatch := gomonkey.ApplyFunc(nodemeta.GetNode,
		func(_ context.Context, nodeID string) (*nodemeta.NodeSnapshot, error) {
			return &nodemeta.NodeSnapshot{NodeID: nodeID}, nil
		})
	defer metaPatch.Reset()

	_, err := countNodeSandboxes(context.Background(), "uncached-node")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, nodemeta.ErrNodeNotFound)
	assert.ErrorContains(t, err, "not present in the scheduler cache")
}

func TestDeleteNodeRemovesMetadataAfterEmptyCheck(t *testing.T) {
	checkPatch := gomonkey.ApplyFunc(countNodeSandboxes, func(context.Context, string) (int, error) {
		return 0, nil
	})
	defer checkPatch.Reset()

	var gotNodeID string
	deletePatch := gomonkey.ApplyFunc(nodemeta.DeleteNode, func(_ context.Context, nodeID string) error {
		gotNodeID = nodeID
		return nil
	})
	defer deletePatch.Reset()

	require.NoError(t, DeleteNode(context.Background(), "node-a", false))
	assert.Equal(t, "node-a", gotNodeID)
}

func TestForceDeleteBypassesInventoryCheck(t *testing.T) {
	checkPatch := gomonkey.ApplyFunc(countNodeSandboxes, func(context.Context, string) (int, error) {
		t.Fatal("force deletion must not query sandbox inventory")
		return 0, nil
	})
	defer checkPatch.Reset()

	var gotNodeID string
	deletePatch := gomonkey.ApplyFunc(nodemeta.DeleteNode, func(_ context.Context, nodeID string) error {
		gotNodeID = nodeID
		return nil
	})
	defer deletePatch.Reset()

	require.NoError(t, DeleteNode(context.Background(), "node-a", true))
	assert.Equal(t, "node-a", gotNodeID)
}

func TestForceDeleteStillRequiresIsolation(t *testing.T) {
	checkPatch := gomonkey.ApplyFunc(countNodeSandboxes, func(context.Context, string) (int, error) {
		t.Fatal("force deletion must not query sandbox inventory")
		return 0, nil
	})
	defer checkPatch.Reset()

	deletePatch := gomonkey.ApplyFunc(nodemeta.DeleteNode, func(context.Context, string) error {
		return nodemeta.ErrNodeNotIsolated
	})
	defer deletePatch.Reset()

	assert.ErrorIs(t, DeleteNode(context.Background(), "node-a", true), nodemeta.ErrNodeNotIsolated)
}
