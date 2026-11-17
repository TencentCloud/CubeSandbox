// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/hostcompat"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// jfs keeps replica rows meaning "built the shared package": distribution
// only caches the ext4 and must not fabricate replica rows, on any alias
// or spelling of the backend.
func TestDistributionTracksReplicas(t *testing.T) {
	for _, backend := range []string{"jfs", "JuiceFS", "juicefs"} {
		if distributionTracksReplicas(backend) {
			t.Fatalf("backend %q must not track replica rows during distribution", backend)
		}
	}
	for _, backend := range []string{"xfs", "s3", "cubecow", ""} {
		if !distributionTracksReplicas(backend) {
			t.Fatalf("backend %q must track replica rows during distribution", backend)
		}
	}
}

func TestTemplateBuildTargetsBuildsJFSOnOneSchedulableNode(t *testing.T) {
	a, b, c := testNode("a", "10.0.0.1"), testNode("b", "10.0.0.2"), testNode("c", "10.0.0.3")
	for _, n := range []*node.Node{a, b, c} {
		n.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	}
	a.SetSchedulingDisabled(true)
	c.SetSchedulingDisabled(true)
	all := []*node.Node{a, b, c}

	if got := templateBuildTargets("xfs", true, all); len(got) != 3 {
		t.Fatalf("xfs targets = %d, want every node", len(got))
	}
	if got := templateBuildTargets("jfs", false, all); len(got) != 3 {
		t.Fatalf("non-template JFS targets = %d, want every node", len(got))
	}
	for i := 0; i < 20; i++ {
		got := templateBuildTargets("jfs", true, all)
		if len(got) != 1 || got[0] != b {
			t.Fatalf("jfs targets = %+v, want only the schedulable node", got)
		}
	}
	b.SetSchedulingDisabled(true)
	if got := templateBuildTargets("jfs", true, all); len(got) != 1 {
		t.Fatalf("jfs targets with every node cordoned = %d, want one", len(got))
	}
	if got := templateBuildTargets("jfs", true, nil); len(got) != 0 {
		t.Fatalf("jfs targets of no node = %d", len(got))
	}
}

func TestTemplateBuildTargetsBuildsOneJFSVariantPerHostFactGroup(t *testing.T) {
	a, b := testNode("a", "10.0.0.1"), testNode("b", "10.0.0.2")
	c, unknown := testNode("c", "10.0.0.3"), testNode("unknown", "10.0.0.4")
	a.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	b.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	c.HostFacts = &node.HostFacts{CPUIDHash: "cpu-b", HostKernelRelease: "6.6.0"}

	got := templateBuildTargets("jfs", true, []*node.Node{a, b, c, unknown})
	if len(got) != 2 {
		t.Fatalf("jfs targets = %d, want one per known group", len(got))
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n.HostFacts.CPUIDHash+"|"+n.HostFacts.HostKernelRelease] = true
	}
	if !seen["cpu-a|5.15.0"] || !seen["cpu-b|6.6.0"] {
		t.Fatalf("selected groups = %v", seen)
	}
}

func TestJFSRedoCleanupLocatorsUseSelectedHostFactGroups(t *testing.T) {
	a, b := testNode("a", "10.0.0.1"), testNode("b", "10.0.0.2")
	c := testNode("c", "10.0.0.3")
	a.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	b.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	c.HostFacts = &node.HostFacts{CPUIDHash: "cpu-b", HostKernelRelease: "6.6.0"}
	variantA, _, ok := hostcompat.TemplateVariantID("tpl-a", "cpu-a", "5.15.0")
	if !ok {
		t.Fatal("derive existing variant")
	}

	locators := jfsRedoCleanupLocators("tpl-a", []models.TemplateReplica{{
		NodeID:            "old-owner",
		NodeIP:            "10.0.0.9",
		TemplateVariantID: variantA,
		Status:            ReplicaStatusReady,
	}}, []*node.Node{a, b, c})

	if len(locators) != 1 {
		t.Fatalf("cleanup locators = %+v, want one existing HostFact variant", locators)
	}
	if locators[0].NodeID != "a" || locators[0].TemplateVariantID != variantA {
		t.Fatalf("cleanup locator = %+v, want selected group representative and physical variant", locators[0])
	}
}

func TestJFSRedoReplicaRowsBecomeUnavailableBeforeDelete(t *testing.T) {
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateReplica{}))
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })

	target := testNode("new-owner", "10.0.0.2")
	target.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	variantID, _, ok := hostcompat.TemplateVariantID("tpl-a", "cpu-a", "5.15.0")
	require.True(t, ok)
	require.NoError(t, db.Table(constants.TemplateReplicaTableName).Create(&models.TemplateReplica{
		TemplateID:        "tpl-a",
		NodeID:            "old-owner",
		TemplateVariantID: variantID,
		Status:            ReplicaStatusReady,
	}).Error)

	require.NoError(t, markJFSRedoReplicaRowsUnavailable(context.Background(), "tpl-a", []*node.Node{target}))
	var marked models.TemplateReplica
	require.NoError(t, db.Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND template_variant_id = ?", "tpl-a", variantID).
		First(&marked).Error)
	require.Equal(t, ReplicaStatusFailed, marked.Status)

	require.NoError(t, deleteJFSRedoReplicaRows(context.Background(), "tpl-a", []*node.Node{target}))
	var count int64
	require.NoError(t, db.Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND template_variant_id = ?", "tpl-a", variantID).
		Count(&count).Error)
	require.Zero(t, count)
}

func TestPrepareJFSRedoRejectsTemplateInUseBeforeCleanup(t *testing.T) {
	oldCheck := redoTemplateInUse
	redoTemplateInUse = func(_ context.Context, templateID, instanceType string) (bool, error) {
		require.Equal(t, "tpl-a", templateID)
		require.Equal(t, "cubebox", instanceType)
		return true, nil
	}
	t.Cleanup(func() { redoTemplateInUse = oldCheck })

	err := prepareRedoTemplateReplicas(
		context.Background(), "tpl-a", "cubebox", nil, nil, "jfs",
	)
	require.ErrorIs(t, err, ErrTemplateInUse)
}

func TestDeleteJFSTemplateCleansUpOwningLocatorOnly(t *testing.T) {
	origReplicaCleanup, origArtifactCleanup := runReplicaCleanup, runArtifactCleanup
	origMetadataCleanup, origJobCleanup := runMetadataCleanup, runTemplateJobCleanup
	origInUse := runInUseCheck
	t.Cleanup(func() {
		runReplicaCleanup, runArtifactCleanup = origReplicaCleanup, origArtifactCleanup
		runMetadataCleanup, runTemplateJobCleanup = origMetadataCleanup, origJobCleanup
		runInUseCheck = origInUse
	})
	noop := func(context.Context, string) error { return nil }
	runArtifactCleanup = func(context.Context, string, *templateCleanupTargets) error { return nil }
	runMetadataCleanup, runTemplateJobCleanup = noop, noop
	runInUseCheck = func(context.Context, string, string) (bool, error) { return false, nil }

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
	if len(got) != 1 || got[0].NodeID != "a" {
		t.Fatalf("jfs cleanup locators = %+v, want only the owning locator", got)
	}
}
