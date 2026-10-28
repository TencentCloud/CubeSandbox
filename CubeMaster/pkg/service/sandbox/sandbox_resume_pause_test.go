// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	dbmodels "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/restoreplace"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestValidatePauseResumeVolumesEmptyOK(t *testing.T) {
	t.Parallel()
	require.NoError(t, validatePauseResumeVolumes(nil))
	require.NoError(t, validatePauseResumeVolumes([]string{"", "  "}))
}

func TestPausePackageNodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, record, target, origin, pkg string
	}{
		{"same node", "10.0.0.1", "10.0.0.1", "", "10.0.0.1"},
		{"blank origin", " ", "10.0.0.1", "", "10.0.0.1"},
		{"cross node", " 10.0.0.2 ", "10.0.0.1", "10.0.0.2", "10.0.0.2"},
	} {
		origin, pkg := pausePackageNodes(tc.record, tc.target)
		require.Equal(t, tc.origin, origin, tc.name)
		require.Equal(t, tc.pkg, pkg, tc.name)
	}
}

func TestCleanupPausePackageAfterResumeOutlivesCaller(t *testing.T) {
	type call struct {
		ctxErr                  error
		node, snapshot, backend string
	}
	got := make(chan call, 1)
	patches := gomonkey.ApplyFunc(pausesnap.CleanupPauseSnapshot,
		func(ctx context.Context, _, nodeIP, snapshotID, backend string) error {
			got <- call{ctx.Err(), nodeIP, snapshotID, backend}
			return nil
		})
	defer patches.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanupPausePackageAfterResume(ctx, "req-1", "sb-1", "10.0.0.2", "snap-1", "jfs")
	select {
	case c := <-got:
		require.NoError(t, c.ctxErr)
		require.Equal(t, call{nil, "10.0.0.2", "snap-1", "jfs"}, c)
	case <-time.After(5 * time.Second):
		t.Fatal("pause package cleanup never ran")
	}
}

func TestValidatePauseResumeVolumesMissing(t *testing.T) {
	patches := gomonkey.ApplyFunc(resolveVolumeRecord, func(volumeID string) (*dbmodels.VolumeRecord, error) {
		return nil, fmt.Errorf("volume not found: %s", volumeID)
	})
	defer patches.Reset()

	err := validatePauseResumeVolumes([]string{"vol-gone"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot resume")
	require.Contains(t, err.Error(), "vol-gone")
}

func TestValidatePauseResumeVolumesPresent(t *testing.T) {
	patches := gomonkey.ApplyFunc(resolveVolumeRecord, func(volumeID string) (*dbmodels.VolumeRecord, error) {
		return &dbmodels.VolumeRecord{VolumeID: volumeID}, nil
	})
	defer patches.Reset()

	require.NoError(t, validatePauseResumeVolumes([]string{"vol-ok", "  vol-ok2  "}))
}

func TestPauseResumePlacementInputPinsHostMount(t *testing.T) {
	t.Parallel()
	rec := &pausesnap.Record{
		SnapshotID:   "snap-1",
		Backend:      "s3",
		RemoteStatus: "ready",
		NodeID:       "node-a",
		NodeIP:       "10.0.0.1",
	}
	got := pauseResumePlacementInput(rec, "cubebox", &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationHostDirMount: `[{"hostPath":"/data/shared/a","mountPath":"/mnt"}]`,
		},
	})
	if !got.PinToOrigin {
		t.Fatal("host-mount spec must pin resume to origin")
	}
	plain := pauseResumePlacementInput(rec, "cubebox", &types.CreateCubeSandboxReq{})
	if plain.PinToOrigin {
		t.Fatal("spec without host-mount must not pin")
	}
	missing := pauseResumePlacementInput(rec, "cubebox", nil)
	if missing.PinToOrigin {
		t.Fatal("missing spec must not pin")
	}
}

func TestResumePlacementHomesWhenCannotCrossNode(t *testing.T) {
	orig := decidePauseResumePlacementFn
	t.Cleanup(func() { decidePauseResumePlacementFn = orig })
	decidePauseResumePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		t.Fatal("Decide must not run when resume cannot leave origin")
		return nil, fmt.Errorf("Decide must not run")
	}

	rec := &pausesnap.Record{
		SnapshotID: "snap-1",
		Backend:    "xfs",
		NodeID:     "node-a",
		NodeIP:     "10.0.0.1",
	}
	got, err := resumePlacement(context.Background(), rec, "cubebox", nil)
	require.NoError(t, err)
	require.Equal(t, "node-a", got.NodeID)
	require.Equal(t, "10.0.0.1", got.NodeIP)
	require.False(t, got.CrossNode)

	rec.Backend = "s3"
	rec.RemoteStatus = "inprogress"
	got, err = resumePlacement(context.Background(), rec, "cubebox", nil)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1", got.NodeIP)
	require.False(t, got.CrossNode)
}

func TestResumePlacementCallsDecideWhenS3Ready(t *testing.T) {
	orig := decidePauseResumePlacementFn
	t.Cleanup(func() { decidePauseResumePlacementFn = orig })
	called := false
	decidePauseResumePlacementFn = func(ctx context.Context, in restoreplace.Input) (*restoreplace.Placement, error) {
		called = true
		return &restoreplace.Placement{NodeID: "node-b", NodeIP: "10.0.0.2", CrossNode: true}, nil
	}

	got, err := resumePlacement(context.Background(), &pausesnap.Record{
		SnapshotID:   "snap-1",
		Backend:      "s3",
		RemoteStatus: "ready",
		NodeID:       "node-a",
		NodeIP:       "10.0.0.1",
	}, "cubebox", nil)
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "node-b", got.NodeID)
	require.True(t, got.CrossNode)
}
