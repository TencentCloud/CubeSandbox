// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func TestTemplateLocalityFilterSelect(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetImageStateByNode, func(templateID string, nodeID string) *fwk.ImageStateSummary {
		if templateID == "tpl-1" && nodeID == "node-a" {
			return fwk.NewImageStateSummary(1, "", nodeID)
		}
		return nil
	})

	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.ReqRes = &selctx.RequestResource{TemplateID: "tpl-1"}
	ctx.SetNodes(node.NodeList{
		&node.Node{InsID: "node-a", IP: "10.0.0.1"},
		&node.Node{InsID: "node-b", IP: "10.0.0.2"},
	})

	got, err := NewTemplateLocalityFilter().Select(ctx)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 node after locality filter, got %d", len(got))
	}
	if got[0].ID() != "node-a" {
		t.Fatalf("expected node-a to remain after locality filter, got %s", got[0].ID())
	}
}

func TestTemplateLocalityFilterIgnoresNonTemplateRequests(t *testing.T) {
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.ReqRes = &selctx.RequestResource{}
	ctx.SetNodes(node.NodeList{
		&node.Node{InsID: "node-a", IP: "10.0.0.1"},
		&node.Node{InsID: "node-b", IP: "10.0.0.2"},
	})

	got, err := NewTemplateLocalityFilter().Select(ctx)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected filter to keep all nodes for non-template requests, got %d", len(got))
	}
}

func TestTemplateLocalityFilterHonorsTemplateNodeScopeAndStorageState(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetImageStateByNode, func(templateID string, nodeID string) *fwk.ImageStateSummary {
		if templateID == "snap-1" && (nodeID == "node-a" || nodeID == "node-b") {
			return fwk.NewImageStateSummary(1, "", nodeID)
		}
		return nil
	})
	localcache.SetSnapshotStorageState(localcache.SnapshotStorageState{
		NodeID: "node-a",
		NodeIP: "10.0.0.1",
		Mode:   "warn",
	})
	localcache.SetSnapshotStorageState(localcache.SnapshotStorageState{
		NodeID: "node-b",
		NodeIP: "10.0.0.2",
		Mode:   "reject",
	})

	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.ReqRes = &selctx.RequestResource{
		TemplateID:             "snap-1",
		TemplateNodeScope:      []string{"node-a", "node-b"},
		EnforceSnapshotStorage: true,
	}
	ctx.SetNodes(node.NodeList{
		&node.Node{InsID: "node-a", IP: "10.0.0.1"},
		&node.Node{InsID: "node-b", IP: "10.0.0.2"},
		&node.Node{InsID: "node-c", IP: "10.0.0.3"},
	})

	got, err := NewTemplateLocalityFilter().Select(ctx)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if len(got) != 1 || got[0].ID() != "node-a" {
		t.Fatalf("expected only node-a to remain, got %v", got)
	}
}
