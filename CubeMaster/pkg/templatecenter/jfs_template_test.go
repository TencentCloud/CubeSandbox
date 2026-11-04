// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestTemplateBuildTargetsBuildsJFSOnOneSchedulableNode(t *testing.T) {
	a, b, c := testNode("a", "10.0.0.1"), testNode("b", "10.0.0.2"), testNode("c", "10.0.0.3")
	a.SetSchedulingDisabled(true)
	c.SetSchedulingDisabled(true)
	all := []*node.Node{a, b, c}

	if got := templateBuildTargets("xfs", all); len(got) != 3 {
		t.Fatalf("xfs targets = %d, want every node", len(got))
	}
	for i := 0; i < 20; i++ {
		got := templateBuildTargets("jfs", all)
		if len(got) != 1 || got[0] != b {
			t.Fatalf("jfs targets = %+v, want only the schedulable node", got)
		}
	}
	b.SetSchedulingDisabled(true)
	if got := templateBuildTargets("jfs", all); len(got) != 1 {
		t.Fatalf("jfs targets with every node cordoned = %d, want one", len(got))
	}
	if got := templateBuildTargets("jfs", nil); len(got) != 0 {
		t.Fatalf("jfs targets of no node = %d", len(got))
	}
}

func TestDeleteJFSTemplateCleansUpEveryHealthyNode(t *testing.T) {
	origReplicaCleanup, origArtifactCleanup := runReplicaCleanup, runArtifactCleanup
	origMetadataCleanup, origJobCleanup := runMetadataCleanup, runTemplateJobCleanup
	origInUse, origNodes := runInUseCheck, sharedTemplateNodes
	t.Cleanup(func() {
		runReplicaCleanup, runArtifactCleanup = origReplicaCleanup, origArtifactCleanup
		runMetadataCleanup, runTemplateJobCleanup = origMetadataCleanup, origJobCleanup
		runInUseCheck, sharedTemplateNodes = origInUse, origNodes
	})
	noop := func(context.Context, string) error { return nil }
	runArtifactCleanup = func(context.Context, string, *templateCleanupTargets) error { return nil }
	runMetadataCleanup, runTemplateJobCleanup = noop, noop
	runInUseCheck = func(context.Context, string, string) (bool, error) { return false, nil }
	sharedTemplateNodes = func(string) []*node.Node {
		return []*node.Node{testNode("a", "10.0.0.1"), testNode("b", "10.0.0.2")}
	}

	run := func(backend string) []templateCleanupLocator {
		var got []templateCleanupLocator
		runReplicaCleanup = func(_ context.Context, _ string, locators []templateCleanupLocator, _ string) error {
			got = locators
			return nil
		}
		err := deleteTemplateWithTargets(context.Background(), "tpl-1", &templateCleanupTargets{
			InstanceType: cubeboxv1.InstanceType_cubebox.String(),
			Definition:   &models.TemplateDefinition{TemplateID: "tpl-1", Status: StatusReady, StorageBackend: backend},
			Locators:     []templateCleanupLocator{{NodeID: "a", NodeIP: "10.0.0.1"}},
		}, DeleteTemplateOptions{})
		if err != nil {
			t.Fatalf("delete %s template: %v", backend, err)
		}
		return got
	}
	if got := run("xfs"); len(got) != 1 {
		t.Fatalf("xfs cleanup locators = %+v, want the replica only", got)
	}
	got := run("jfs")
	if len(got) != 2 || got[1].NodeID != "b" {
		t.Fatalf("jfs cleanup locators = %+v, want the replica plus node b", got)
	}
}
