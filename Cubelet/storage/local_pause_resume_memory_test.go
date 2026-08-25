// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func pauseResumeCreateContext(sandboxID string, ann map[string]string) *workflow.CreateContext {
	return &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations:  ann,
		},
	}
}

func TestPauseResumeMemoryOwnerID(t *testing.T) {
	t.Parallel()

	pauseAnn := map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
	}
	require.Equal(t, "sb-1", pauseResumeMemoryOwnerID(pauseResumeCreateContext("sb-1", pauseAnn)))
	require.Equal(t, "sb-desired", pauseResumeMemoryOwnerID(pauseResumeCreateContext("", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
		constants.MasterAnnotationDesiredSandboxID:  "sb-desired",
	})))
	require.Empty(t, pauseResumeMemoryOwnerID(pauseResumeCreateContext("", pauseAnn)))
	require.Empty(t, pauseResumeMemoryOwnerID(nil))
	require.Empty(t, pauseResumeMemoryOwnerID(pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: "snap-customer",
	})))
	cross := crossNodeAnnotations()
	cross[constants.MasterAnnotationPauseSnapshotID] = "snap-pause1"
	cross[constants.MasterAnnotationRuntimeSnapshotID] = "snap-pause1"
	require.Empty(t, pauseResumeMemoryOwnerID(pauseResumeCreateContext("sb-1", cross)))
}

func TestPrefetchRestoreMemoryVolURLFailsClosedWithoutSandboxID(t *testing.T) {
	l := &local{config: &Config{StorageBackend: "cubecow"}}
	_, _, err := l.prefetchRestoreMemoryVolURL(context.Background(), pauseResumeCreateContext("", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "sandbox id")
}

func TestPrefetchRestoreMemoryVolURLFromSnapDoesNotClone(t *testing.T) {
	l := &local{config: &Config{StorageBackend: "cubecow"}}
	_, imported, err := l.prefetchRestoreMemoryVolURL(context.Background(), pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: "snap-customer",
	}))
	// Catalog miss is expected; the point is FromSnap must not fail closed
	// as a pause resume, and must not record a cloned memory disk.
	if err != nil {
		require.NotContains(t, err.Error(), "sandbox id")
	}
	require.Empty(t, imported)
}
