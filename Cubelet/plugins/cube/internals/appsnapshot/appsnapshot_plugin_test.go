// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package appsnapshot

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

type failingRunTemplateManager struct{}

func (*failingRunTemplateManager) SetInstanceType(string) {}

func (*failingRunTemplateManager) EnsureCubeRunTemplate(context.Context, string) (*templatetypes.LocalRunTemplate, error) {
	return nil, errors.New("template is not available locally")
}

func (*failingRunTemplateManager) ListLocalTemplates(context.Context) (map[string]*templatetypes.LocalRunTemplate, error) {
	return nil, nil
}

func seedCatalogEntry(t *testing.T, snapshotID string, diskOnly bool) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, storage.WriteSnapshotCatalog(&storage.SnapshotCatalogEntry{
		SnapshotID:   snapshotID,
		InstanceType: "cubebox",
		SpecDir:      "1C1024M",
		SnapshotPath: dir,
		MetaDir:      dir,
		RootfsVol:    "tpl-" + snapshotID + "-rootfs",
		RootfsKind:   storage.CowKindSnapshot,
		DiskOnly:     diskOnly,
		Kind:         storage.CatalogKindTemplate,
	}))
	t.Cleanup(func() { storage.DeleteSnapshotCatalog(snapshotID) })
}

func v2RestoreContext(snapshotID string) *workflow.CreateContext {
	return &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: snapshotID,
				constants.MasterAnnotationAppSnapshotVersion:    "v2",
			},
		},
	}
}

// A disk-only snapshot has no run-template metadata, so the completer must not
// demand one: the nil manager here would panic if EnsureCubeRunTemplate ran.
func TestCreateSkipsRunTemplateForDiskOnlySnapshot(t *testing.T) {
	seedCatalogEntry(t, "snap-disk-only", true)

	completer := &appsnapshotCompleter{}
	flowOpts := v2RestoreContext("snap-disk-only")

	require.NoError(t, completer.Create(context.Background(), flowOpts))
	require.Nil(t, flowOpts.LocalRunTemplate)
}

// Every other v2 restore keeps the run-template requirement, including a
// catalog entry that never declared itself disk-only.
func TestCreateStillRequiresRunTemplateForMemorySnapshot(t *testing.T) {
	seedCatalogEntry(t, "snap-memory", false)

	completer := &appsnapshotCompleter{runtemplateManager: &failingRunTemplateManager{}}

	err := completer.Create(context.Background(), v2RestoreContext("snap-memory"))
	require.ErrorContains(t, err, "ensure cube run template")
}

// An unreadable catalog says nothing about the snapshot, so the requirement
// stands: only a positive disk-only entry may skip it.
func TestCreateStillRequiresRunTemplateWhenCatalogMissing(t *testing.T) {
	completer := &appsnapshotCompleter{runtemplateManager: &failingRunTemplateManager{}}

	err := completer.Create(context.Background(), v2RestoreContext("snap-absent"))
	require.ErrorContains(t, err, "ensure cube run template")
}
