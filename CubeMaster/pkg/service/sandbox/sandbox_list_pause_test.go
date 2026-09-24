// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestApplyPauseBindingsEnrichesScannedRow(t *testing.T) {
	pausedAt := time.Now().Add(-time.Minute)
	items := []*types.SandboxBriefData{
		{SandboxID: "sb-1", Status: 5, HostID: "node-a", Backend: constants.SnapshotBackendS3, CreateAt: 42, EndAt: 1234},
	}

	got := applyPauseBindings(t.Context(), items, []*pausesnap.Record{{
		SandboxID:    "sb-1",
		SnapshotID:   "snap-1",
		NodeID:       "node-a",
		Status:       pausesnap.StatusReady,
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
		UpdatedAt:    pausedAt,
	}}, pauseBindingMerge{appendMissing: true})

	require.Len(t, got, 1)
	require.Equal(t, "snap-1", got[0].PauseSnapshotID)
	require.Equal(t, constants.RemoteStatusReady, got[0].RemoteStatus)
	// The node reported this row, so its own timestamps must survive.
	require.Equal(t, int64(42), got[0].CreateAt)
	require.Equal(t, int64(1234), got[0].EndAt)
}

func TestApplyPauseBindingsAppendsRowMissingFromNodeScan(t *testing.T) {
	pausedAt := time.Now().Add(-time.Minute)

	got := applyPauseBindings(t.Context(), nil, []*pausesnap.Record{{
		SandboxID:    "sb-2",
		SnapshotID:   "snap-2",
		NodeID:       "node-b",
		NodeIP:       "10.0.0.2",
		Status:       pausesnap.StatusReady,
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusInProgress,
		UpdatedAt:    pausedAt,
	}}, pauseBindingMerge{appendMissing: true})

	require.Len(t, got, 1)
	require.Equal(t, "sb-2", got[0].SandboxID)
	require.Equal(t, int32(5), got[0].Status)
	require.Equal(t, "node-b", got[0].HostID)
	require.Equal(t, "10.0.0.2", got[0].HostIP)
	require.Equal(t, constants.RemoteStatusInProgress, got[0].RemoteStatus)
	require.Equal(t, pausedAt.UnixNano(), got[0].PauseAt)
	require.Zero(t, got[0].CreateAt)
}

func TestEnrichSandboxListEndAtsUsesOneBatchLookup(t *testing.T) {
	provider := &mockTimeoutProvider{returnEndAts: map[string]int64{
		"sb-node":   1234,
		"sb-paused": 5678,
	}}
	previousProvider := getTimeoutProvider()
	SetTimeoutProvider(provider)
	t.Cleanup(func() { SetTimeoutProvider(previousProvider) })

	items := []*types.SandboxBriefData{
		{SandboxID: "sb-node", EndAt: 1},
		{SandboxID: "sb-paused"},
		{SandboxID: "sb-never", EndAt: 2},
	}
	enrichSandboxListEndAts(t.Context(), items)

	require.Equal(t, int64(1234), items[0].EndAt)
	require.Equal(t, int64(5678), items[1].EndAt)
	require.Equal(t, int64(2), items[2].EndAt)
	require.Equal(t, 1, provider.batchLookupCalls)
	require.Zero(t, provider.lookupCalls)
	require.Equal(t, []string{"sb-node", "sb-paused", "sb-never"}, provider.lastLookupIDs)
}

func TestEnrichSandboxListEndAtsAppliesAuthoritativeZero(t *testing.T) {
	provider := &mockTimeoutProvider{returnEndAts: map[string]int64{"sb-never": 0}}
	previousProvider := getTimeoutProvider()
	SetTimeoutProvider(provider)
	t.Cleanup(func() { SetTimeoutProvider(previousProvider) })

	items := []*types.SandboxBriefData{{SandboxID: "sb-never", EndAt: 1234}}
	enrichSandboxListEndAts(t.Context(), items)

	require.Zero(t, items[0].EndAt)
}

func TestApplyPauseBindingsSkipsUnfinishedPause(t *testing.T) {
	got := applyPauseBindings(t.Context(), nil, []*pausesnap.Record{
		{SandboxID: "sb-3", SnapshotID: "snap-3", Status: pausesnap.StatusCreating},
		{SandboxID: "sb-4", SnapshotID: "snap-4", Status: pausesnap.StatusFailed},
	}, pauseBindingMerge{appendMissing: true})

	require.Empty(t, got)
}

func TestApplyPauseBindingsSurfacesDeleteFailedLeftover(t *testing.T) {
	got := applyPauseBindings(t.Context(), nil, []*pausesnap.Record{{
		SandboxID:    "sb-stuck",
		SnapshotID:   "snap-stuck",
		NodeID:       "node-c",
		Status:       pausesnap.StatusDeleteFailed,
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
	}}, pauseBindingMerge{appendMissing: true})

	require.Len(t, got, 1, "a pause package the node could not sweep must stay visible")
	require.Equal(t, "sb-stuck", got[0].SandboxID)
	require.Equal(t, pausesnap.StatusDeleteFailed, got[0].PauseStatus)
}

func TestApplyPauseBindingsFiltersShimlessRowsBySpecLabels(t *testing.T) {
	records := []*pausesnap.Record{{
		SandboxID:    "sb-5",
		SnapshotID:   "snap-5",
		Status:       pausesnap.StatusReady,
		RemoteStatus: constants.RemoteStatusReady,
	}}
	specs := map[string]*types.CreateCubeSandboxReq{
		"sb-5": {Labels: map[string]string{"app": "keep"}},
	}

	require.Empty(t, applyPauseBindings(t.Context(), nil, records, pauseBindingMerge{
		appendMissing: true,
		labelSelector: map[string]string{"app": "other"},
		specs:         specs,
	}))
	require.Empty(t, applyPauseBindings(t.Context(), nil, records, pauseBindingMerge{
		appendMissing: true,
		labelSelector: map[string]string{"app": "keep"},
	}), "a shimless row with no spec has no labels to match")

	got := applyPauseBindings(t.Context(), nil, records, pauseBindingMerge{
		appendMissing: true,
		labelSelector: map[string]string{"app": "keep"},
		specs:         specs,
	})
	require.Len(t, got, 1)
	require.Equal(t, "keep", got[0].Labels["app"])

	// A row the node did report still gets its pause state, filter or not.
	items := []*types.SandboxBriefData{{SandboxID: "sb-5", Status: 5}}
	got = applyPauseBindings(t.Context(), items, records, pauseBindingMerge{
		appendMissing: true,
		labelSelector: map[string]string{"app": "other"},
	})
	require.Len(t, got, 1)
	require.Equal(t, constants.RemoteStatusReady, got[0].RemoteStatus)
}

func TestApplyPauseBindingsDoesNotAppendOnIntermediatePage(t *testing.T) {
	scanned := []*types.SandboxBriefData{{SandboxID: "sb-run", Status: 1, CreateAt: 99}}
	records := []*pausesnap.Record{
		{
			SandboxID:    "sb-run",
			SnapshotID:   "snap-run",
			Status:       pausesnap.StatusReady,
			RemoteStatus: constants.RemoteStatusReady,
		},
		{
			SandboxID:    "sb-paused",
			SnapshotID:   "snap-paused",
			Status:       pausesnap.StatusReady,
			RemoteStatus: constants.RemoteStatusReady,
		},
	}

	got := applyPauseBindings(t.Context(), scanned, records, pauseBindingMerge{})
	require.Len(t, got, 1)
	require.Equal(t, "sb-run", got[0].SandboxID)
	require.Equal(t, "snap-run", got[0].PauseSnapshotID)
}

func TestShouldAppendShimlessPauseRows(t *testing.T) {
	cases := []struct {
		name string
		req  *types.ListCubeSandboxReq
		rsp  *types.ListCubeSandboxRes
		want bool
	}{
		{
			name: "hostid always appends",
			req:  &types.ListCubeSandboxReq{HostID: "node-a"},
			rsp:  &types.ListCubeSandboxRes{Total: 4, Size: 1, EndIdx: 1},
			want: true,
		},
		{
			name: "last page",
			req:  &types.ListCubeSandboxReq{},
			rsp:  &types.ListCubeSandboxRes{Total: 4, Size: 2, EndIdx: 4},
			want: true,
		},
		{
			name: "mid page",
			req:  &types.ListCubeSandboxReq{},
			rsp:  &types.ListCubeSandboxRes{Total: 4, Size: 2, EndIdx: 2},
			want: false,
		},
		{
			name: "empty window is not last page",
			req:  &types.ListCubeSandboxReq{},
			rsp:  &types.ListCubeSandboxRes{Total: 2, Size: 0, EndIdx: -1},
			want: false,
		},
		{
			name: "no healthy nodes",
			req:  &types.ListCubeSandboxReq{},
			rsp:  &types.ListCubeSandboxRes{Total: 0, Size: 0},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shouldAppendShimlessPauseRows(tc.req, tc.rsp))
		})
	}
}

func TestApplyPauseBindingsAppendsRowWithSpecIdentity(t *testing.T) {
	rec := &pausesnap.Record{
		SandboxID:    "sb-spec",
		SnapshotID:   "snap-spec",
		NodeID:       "node-a",
		Status:       pausesnap.StatusReady,
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
	}
	got := applyPauseBindings(t.Context(), nil, []*pausesnap.Record{rec}, pauseBindingMerge{
		appendMissing: true,
		specs:         map[string]*types.CreateCubeSandboxReq{"sb-spec": specWithIdentity()},
	})
	require.Len(t, got, 1)
	require.Equal(t, "demo", got[0].Labels["app"])
	require.Equal(t, "tpl-1", got[0].TemplateID)
	require.Equal(t, int32(500), got[0].CPUMilli)
	require.NotEmpty(t, got[0].VolumeMounts)
	require.Zero(t, got[0].CreateAt)
	require.Equal(t, constants.SnapshotBackendS3, got[0].Backend)
	require.Equal(t, "snap-spec", got[0].Annotations[constants.CubeAnnotationPauseSnapshotID])
}

func TestApplyPauseBindingsDecidesScannedRowStatus(t *testing.T) {
	var (
		running = int32(cubebox.ContainerState_CONTAINER_RUNNING)
		exited  = int32(cubebox.ContainerState_CONTAINER_EXITED)
		pausing = int32(cubebox.ContainerState_CONTAINER_PAUSING)
		paused  = int32(cubebox.ContainerState_CONTAINER_PAUSED)
		unknown = int32(cubebox.ContainerState_CONTAINER_UNKNOWN)
	)
	cases := []struct {
		name   string
		status string
		obs    int32
		want   int32
		stale  bool
		errAnn string
	}{
		{name: "creating over running", status: pausesnap.StatusCreating, obs: running, want: pausing},
		{name: "failed over running", status: pausesnap.StatusFailed, obs: running, want: unknown, errAnn: "boom"},
		{name: "ready over exited", status: pausesnap.StatusReady, obs: exited, want: paused},
		{name: "ready over running", status: pausesnap.StatusReady, obs: running, want: running, stale: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := pauseStaleCount("list")
			items := []*types.SandboxBriefData{{SandboxID: "sb", Status: tc.obs, Labels: map[string]string{"app": "demo"}}}
			got := applyPauseBindings(t.Context(), items, []*pausesnap.Record{{
				SandboxID:  "sb",
				SnapshotID: "snap",
				Status:     tc.status,
				LastError:  "boom",
			}}, pauseBindingMerge{})
			require.Equal(t, tc.want, got[0].Status)
			require.Equal(t, "demo", got[0].Labels["app"])
			if tc.errAnn != "" {
				require.Equal(t, tc.errAnn, got[0].Annotations[constants.CubeAnnotationPauseError])
			}
			delta := pauseStaleCount("list") - before
			if tc.stale {
				require.Equal(t, float64(1), delta)
			} else {
				require.Zero(t, delta)
			}
		})
	}
}

func TestApplyPauseBindingsHonorsListFilterOutLabels(t *testing.T) {
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Common: &config.CommonConf{ListFilterOutLables: map[string]string{"hide": "yes"}}}
	})
	got := applyPauseBindings(t.Context(), nil, []*pausesnap.Record{{
		SandboxID:  "sb-hide",
		SnapshotID: "snap-hide",
		Status:     pausesnap.StatusReady,
	}}, pauseBindingMerge{
		appendMissing: true,
		specs: map[string]*types.CreateCubeSandboxReq{
			"sb-hide": {Labels: map[string]string{"hide": "yes", "app": "demo"}},
		},
	})
	require.Empty(t, got)
}

func TestMissingShimlessBindingIDs(t *testing.T) {
	items := []*types.SandboxBriefData{{SandboxID: "sb-seen"}}
	records := []*pausesnap.Record{
		{SandboxID: "sb-seen", Status: pausesnap.StatusReady},
		{SandboxID: "sb-ready", Status: pausesnap.StatusReady},
		{SandboxID: "sb-stuck", Status: pausesnap.StatusDeleteFailed},
		{SandboxID: "sb-creating", Status: pausesnap.StatusCreating},
	}
	require.Equal(t, []string{"sb-ready", "sb-stuck"}, missingShimlessBindingIDs(items, records))
}

func TestMergePauseBindingsSkipsSpecReadOnIntermediatePage(t *testing.T) {
	var gets int
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(pausesnap.List, func(context.Context, pausesnap.ListOptions) ([]*pausesnap.Record, error) {
		return []*pausesnap.Record{{
			SandboxID:  "sb-paused",
			SnapshotID: "snap",
			Status:     pausesnap.StatusReady,
		}}, nil
	})
	patches.ApplyFunc(sandboxspec.GetMany, func(context.Context, []string) (map[string]*types.CreateCubeSandboxReq, error) {
		gets++
		return map[string]*types.CreateCubeSandboxReq{}, nil
	})

	mergePauseBindings(t.Context(), &types.ListCubeSandboxReq{}, &types.ListCubeSandboxRes{Total: 4, Size: 2, EndIdx: 2})
	require.Zero(t, gets)
	mergePauseBindings(t.Context(), &types.ListCubeSandboxReq{}, &types.ListCubeSandboxRes{Total: 4, Size: 2, EndIdx: 4})
	require.Equal(t, 1, gets)
}

func TestListAndInfoAgreeOnPausedSandbox(t *testing.T) {
	const sandboxID = "sb-agree"
	spec := specWithIdentity()
	rec := readyInfoRecord(sandboxID)
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return nil, false
	})
	patches.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		return spec, nil
	})

	infoRsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, infoRsp, rec)
	require.True(t, filled)

	listed := applyPauseBindings(t.Context(), nil, []*pausesnap.Record{rec}, pauseBindingMerge{
		appendMissing: true,
		specs:         map[string]*types.CreateCubeSandboxReq{sandboxID: spec},
	})
	require.Len(t, listed, 1)
	require.Equal(t, infoRsp.Data[0].Status, listed[0].Status)
	require.Equal(t, infoRsp.Data[0].Labels, listed[0].Labels)
	require.Equal(t, infoRsp.Data[0].TemplateID, listed[0].TemplateID)
	require.Equal(t, infoRsp.Data[0].Annotations[constants.CubeAnnotationPauseSnapshotID], listed[0].Annotations[constants.CubeAnnotationPauseSnapshotID])
}
