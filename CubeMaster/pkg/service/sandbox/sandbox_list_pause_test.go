// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestApplyPauseBindingsEnrichesScannedRow(t *testing.T) {
	pausedAt := time.Now().Add(-time.Minute)
	items := []*types.SandboxBriefData{
		{SandboxID: "sb-1", Status: 5, HostID: "node-a", Backend: constants.SnapshotBackendS3, CreateAt: 42},
	}

	got := applyPauseBindings(items, []*pausesnap.Record{{
		SandboxID:    "sb-1",
		SnapshotID:   "snap-1",
		NodeID:       "node-a",
		Status:       pausesnap.StatusReady,
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
		UpdatedAt:    pausedAt,
	}}, false)

	require.Len(t, got, 1)
	require.Equal(t, "snap-1", got[0].PauseSnapshotID)
	require.Equal(t, constants.RemoteStatusReady, got[0].RemoteStatus)
	// The node reported this row, so its own timestamps must survive.
	require.Equal(t, int64(42), got[0].CreateAt)
}

func TestApplyPauseBindingsAppendsRowMissingFromNodeScan(t *testing.T) {
	pausedAt := time.Now().Add(-time.Minute)

	got := applyPauseBindings(nil, []*pausesnap.Record{{
		SandboxID:    "sb-2",
		SnapshotID:   "snap-2",
		NodeID:       "node-b",
		NodeIP:       "10.0.0.2",
		Status:       pausesnap.StatusReady,
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusInProgress,
		UpdatedAt:    pausedAt,
	}}, false)

	require.Len(t, got, 1)
	require.Equal(t, "sb-2", got[0].SandboxID)
	require.Equal(t, int32(5), got[0].Status)
	require.Equal(t, "node-b", got[0].HostID)
	require.Equal(t, "10.0.0.2", got[0].HostIP)
	require.Equal(t, constants.RemoteStatusInProgress, got[0].RemoteStatus)
	require.Equal(t, pausedAt.UnixNano(), got[0].PauseAt)
	require.Zero(t, got[0].CreateAt)
}

func TestApplyPauseBindingsSkipsUnfinishedPause(t *testing.T) {
	got := applyPauseBindings(nil, []*pausesnap.Record{
		{SandboxID: "sb-3", SnapshotID: "snap-3", Status: "CREATING"},
		{SandboxID: "sb-4", SnapshotID: "snap-4", Status: pausesnap.StatusFailed},
	}, false)

	require.Empty(t, got)
}

func TestApplyPauseBindingsSurfacesDeleteFailedLeftover(t *testing.T) {
	got := applyPauseBindings(nil, []*pausesnap.Record{{
		SandboxID:    "sb-stuck",
		SnapshotID:   "snap-stuck",
		NodeID:       "node-c",
		Status:       pausesnap.StatusDeleteFailed,
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
	}}, false)

	require.Len(t, got, 1, "a pause package the node could not sweep must stay visible")
	require.Equal(t, "sb-stuck", got[0].SandboxID)
	require.Equal(t, pausesnap.StatusDeleteFailed, got[0].PauseStatus)
}

func TestApplyPauseBindingsSkipsAppendWhenLabelFiltered(t *testing.T) {
	records := []*pausesnap.Record{{
		SandboxID:    "sb-5",
		SnapshotID:   "snap-5",
		Status:       pausesnap.StatusReady,
		RemoteStatus: constants.RemoteStatusReady,
	}}

	require.Empty(t, applyPauseBindings(nil, records, true))

	// A row the node did report still gets its pause state, filter or not.
	items := []*types.SandboxBriefData{{SandboxID: "sb-5", Status: 5}}
	got := applyPauseBindings(items, records, true)
	require.Len(t, got, 1)
	require.Equal(t, constants.RemoteStatusReady, got[0].RemoteStatus)
}
