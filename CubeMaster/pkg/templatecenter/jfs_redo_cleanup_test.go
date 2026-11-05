// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestJFSTemplateCleanupIncludesEveryAdoptedTarget(t *testing.T) {
	targets := []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
		testNode("node-c", "10.0.0.3"),
	}
	// Only the original builder has a replica row. Nodes B and C model the
	// private volumes/catalogs created by eager adoption.
	replicas := []models.TemplateReplica{{NodeID: "node-a", NodeIP: "10.0.0.1"}}

	locators := templateCleanupLocatorsForNodes(replicas, targets, "jfs")
	if len(locators) != len(targets) {
		t.Fatalf("jfs cleanup locators = %d, want %d targets", len(locators), len(targets))
	}
	for i, target := range targets {
		if locators[i].NodeID != target.ID() || locators[i].NodeIP != target.HostIP() {
			t.Fatalf("locator[%d] = %#v, want node %s/%s", i, locators[i], target.ID(), target.HostIP())
		}
	}
}

func TestNonJFSTemplateCleanupStillUsesReplicaRows(t *testing.T) {
	targets := []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
		testNode("node-c", "10.0.0.3"),
	}
	replicas := []models.TemplateReplica{
		{NodeID: "node-a", NodeIP: "10.0.0.1"},
		{NodeID: "outside", NodeIP: "10.0.0.9"},
	}

	locators := templateCleanupLocatorsForNodes(replicas, targets, "xfs")
	if len(locators) != 1 || locators[0].NodeID != "node-a" {
		t.Fatalf("xfs cleanup locators = %#v, want only node-a replica", locators)
	}
}

func TestJFSNarrowedRedoUsesOriginalAdoptionScope(t *testing.T) {
	original := []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
		testNode("node-c", "10.0.0.3"),
	}
	current := []*node.Node{original[1]}
	origResolve := resolveJFSRedoLifecycleNodes
	t.Cleanup(func() { resolveJFSRedoLifecycleNodes = origResolve })
	resolveJFSRedoLifecycleNodes = func(instanceType string, scope []string) ([]*node.Node, error) {
		if instanceType != "cubebox" || len(scope) != 0 {
			t.Fatalf("resolve original scope: instanceType=%q scope=%v", instanceType, scope)
		}
		return original, nil
	}

	sourceReq := &types.CreateTemplateFromImageReq{InstanceType: "cubebox"}
	for _, redoReq := range []*types.RedoTemplateFromImageReq{
		{DistributionScope: []string{"node-b"}},
		{FailedOnly: true},
	} {
		got, err := jfsRedoLifecycleTargets(sourceReq, redoReq, current)
		if err != nil {
			t.Fatalf("jfsRedoLifecycleTargets() error = %v", err)
		}
		if len(got) != len(original) {
			t.Fatalf("lifecycle targets = %d, want original scope %d", len(got), len(original))
		}
	}
}

func TestJFSFullRedoReusesResolvedFullScope(t *testing.T) {
	current := []*node.Node{testNode("node-a", "10.0.0.1"), testNode("node-b", "10.0.0.2")}
	got, err := jfsRedoLifecycleTargets(
		&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
		&types.RedoTemplateFromImageReq{},
		current,
	)
	if err != nil || len(got) != len(current) {
		t.Fatalf("full redo targets = %v, err=%v", got, err)
	}
}

func TestJFSRedoDropsPreviousBuilderReplicaAfterCleanup(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	cleaned := false
	deletedRows := false
	patches.ApplyFunc(cleanupTemplateReplicasWithLocators, func(context.Context, string, []templateCleanupLocator, string) error {
		cleaned = true
		return nil
	})
	patches.ApplyFunc(deleteReplicasByTemplateID, func(context.Context, string) error {
		if !cleaned {
			t.Fatal("replica rows deleted before physical JFS cleanup")
		}
		deletedRows = true
		return nil
	})

	err := cleanupTemplateReplicasOnNodes(
		context.Background(),
		"tpl-redo",
		[]models.TemplateReplica{{NodeID: "node-a", NodeIP: "10.0.0.1"}},
		[]*node.Node{testNode("node-a", "10.0.0.1"), testNode("node-b", "10.0.0.2")},
		"jfs",
	)
	if err != nil {
		t.Fatalf("cleanupTemplateReplicasOnNodes() error = %v", err)
	}
	if !cleaned || !deletedRows {
		t.Fatalf("cleaned=%v deletedRows=%v, want both true", cleaned, deletedRows)
	}
}
