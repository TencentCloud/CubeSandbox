// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestEffectiveWritableLayerPrefersTheStoredDirectory(t *testing.T) {
	box := createTestCubeBox("sb-1", "")
	box.RootfsWritableLayer = "disk/tpl-stored_0"
	box.Annotations = map[string]string{
		constants.MasterAnnotationAppSnapshotTemplateID: "snap-should-not-win",
	}
	require.Equal(t, "disk/tpl-stored_0", box.EffectiveWritableLayer(context.Background()))
}

func TestEffectiveWritableLayerUsesTheTemplateWhenNothingElseIsRecorded(t *testing.T) {
	box := createTestCubeBox("sb-1", "")
	box.SandboxID = "sb-1"
	box.Annotations = map[string]string{
		constants.MasterAnnotationJFSTemplateVariantID:  "tpl-variant",
		constants.MasterAnnotationAppSnapshotTemplateID: "tpl-plain",
	}
	require.Equal(t, "disk/tpl-variant_0", box.EffectiveWritableLayer(context.Background()))
}

func TestEffectiveWritableLayerFallsBackToTheSandboxID(t *testing.T) {
	box := createTestCubeBox("sb-1", "")
	box.SandboxID = "sb-1"
	require.Equal(t, "disk/sb-1", box.EffectiveWritableLayer(context.Background()))
}

func TestEffectiveWritableLayerSkipsAReadOnlyRoot(t *testing.T) {
	box := createTestCubeBox("sb-1", "")
	box.RootfsWritableLayer = "disk/tpl-stored_0"
	box.FirstContainer().Config = &cubeboxv1.ContainerConfig{
		SecurityContext: &cubeboxv1.ContainerSecurityContext{ReadonlyRootfs: true},
	}
	require.Empty(t, box.EffectiveWritableLayer(context.Background()))
}

func TestReconcileRecoveredWritableLayerFillsAnEmptyRow(t *testing.T) {
	box := createTestCubeBox("sb-1", "")
	box.Annotations = map[string]string{
		constants.MasterAnnotationAppSnapshotTemplateID: "tpl-recovered",
	}
	box.ReconcileRecoveredWritableLayer(context.Background())
	require.Equal(t, "disk/tpl-recovered_0", box.RootfsWritableLayer)

	box.ReconcileRecoveredWritableLayer(context.Background())
	require.Equal(t, "disk/tpl-recovered_0", box.RootfsWritableLayer)
}

func TestNormalizeWritableLayerRejectsPathsOutsideDisk(t *testing.T) {
	require.Empty(t, NormalizeWritableLayer("disk/../etc"))
	require.Empty(t, NormalizeWritableLayer("/disk/tpl_0"))
	require.Equal(t, "disk/tpl_0", NormalizeWritableLayer("disk/tpl_0"))
}
