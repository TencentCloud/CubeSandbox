// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestApplyPauseBindingToInfoIncludesLifecycleEndAt(t *testing.T) {
	const sandboxID = "sb-paused-info"
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(_ context.Context, gotSandboxID string) (*basetypes.SandboxProxyMap, bool) {
		require.Equal(t, sandboxID, gotSandboxID)
		return &basetypes.SandboxProxyMap{
			SandboxID: sandboxID,
			HostIP:    "10.0.0.1",
			SandboxIP: "192.168.0.2",
		}, true
	})
	rec := &pausesnap.Record{
		SandboxID:  sandboxID,
		SnapshotID: "snap-paused-info",
		Status:     pausesnap.StatusReady,
	}

	for _, tc := range []struct {
		name         string
		endAt        int64
		cubeletEndAt int64
		wantLookups  int
	}{
		{name: "finite timeout", endAt: 123456789, wantLookups: 1},
		{name: "never timeout", endAt: 0, wantLookups: 1},
		{name: "reuse cubelet deadline", endAt: 123456789, cubeletEndAt: 987654321},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &mockTimeoutProvider{returnEndAt: tc.endAt}
			previousProvider := getTimeoutProvider()
			SetTimeoutProvider(provider)
			t.Cleanup(func() { SetTimeoutProvider(previousProvider) })

			rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
			if tc.cubeletEndAt != 0 {
				rsp.Data = []*types.SandboxData{{SandboxID: sandboxID, EndAt: tc.cubeletEndAt}}
			}
			filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{
				SandboxID: sandboxID,
			}, rsp, rec)

			require.True(t, filled)
			require.Len(t, rsp.Data, 1)
			wantEndAt := tc.endAt
			if tc.cubeletEndAt != 0 {
				wantEndAt = tc.cubeletEndAt
			}
			require.Equal(t, wantEndAt, rsp.Data[0].EndAt)
			require.Equal(t, tc.wantLookups, provider.lookupCalls)
		})
	}
}

func readyInfoRecord(sandboxID string) *pausesnap.Record {
	return &pausesnap.Record{
		SandboxID:  sandboxID,
		SnapshotID: "snap-info",
		Status:     pausesnap.StatusReady,
		NodeID:     "node-a",
		NodeIP:     "10.0.0.1",
	}
}

func patchInfoProxy(t *testing.T, sandboxID string) *gomonkey.Patches {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(_ context.Context, got string) (*basetypes.SandboxProxyMap, bool) {
		if got != sandboxID {
			return nil, false
		}
		return &basetypes.SandboxProxyMap{SandboxID: sandboxID, HostIP: "10.0.0.9", SandboxIP: "192.168.0.9"}, true
	})
	return patches
}

func TestApplyPauseBindingToInfoKeepsTombstoneIdentity(t *testing.T) {
	const sandboxID = "sb-tombstone"
	patchInfoProxy(t, sandboxID)
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}, Data: []*types.SandboxData{{
		SandboxID:  sandboxID,
		Status:     int32(cubebox.ContainerState_CONTAINER_PAUSED),
		Labels:     map[string]string{"app": "demo"},
		TemplateID: "tpl-1",
		NameSpace:  "ns-a",
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1",
		},
		Containers: []*types.ContainerInfo{{
			ContainerID: sandboxID,
			Status:      int32(cubebox.ContainerState_CONTAINER_PAUSED),
			Cpu:         "1",
			Type:        "sandbox",
		}},
	}}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, readyInfoRecord(sandboxID))
	require.True(t, filled)
	got := rsp.Data[0]
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), got.Status)
	require.Equal(t, "demo", got.Labels["app"])
	require.Equal(t, "tpl-1", got.TemplateID)
	require.Equal(t, "ns-a", got.NameSpace)
	require.Equal(t, "1", got.Containers[0].Cpu)
	require.Equal(t, "snap-info", got.Annotations[constants.CubeAnnotationPauseSnapshotID])
	require.Equal(t, "tpl-1", got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	require.Equal(t, "10.0.0.1", got.HostIP)
	require.Equal(t, "192.168.0.9", got.SandboxIP)
}

func TestApplyPauseBindingToInfoStaleReadyReportsRunning(t *testing.T) {
	const sandboxID = "sb-stale"
	patches := patchInfoProxy(t, sandboxID)
	var deleted int
	patches.ApplyFunc(pausesnap.Delete, func(context.Context, string) error {
		deleted++
		return nil
	})
	before := pauseStaleCount("info")
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}, Data: []*types.SandboxData{{
		SandboxID: sandboxID,
		Status:    int32(cubebox.ContainerState_CONTAINER_RUNNING),
		Labels:    map[string]string{"app": "live"},
	}}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, readyInfoRecord(sandboxID))
	require.False(t, filled)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_RUNNING), rsp.Data[0].Status)
	require.Equal(t, "live", rsp.Data[0].Labels["app"])
	require.Equal(t, before+1, pauseStaleCount("info"))
	require.Zero(t, deleted)
}

func TestApplyPauseBindingToInfoReadyWithoutNodeRecordUsesSpec(t *testing.T) {
	const sandboxID = "sb-spec"
	patches := patchInfoProxy(t, sandboxID)
	patches.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		return specWithIdentity(), nil
	})
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, readyInfoRecord(sandboxID))
	require.True(t, filled)
	got := rsp.Data[0]
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), got.Status)
	require.Equal(t, "demo", got.Labels["app"])
	require.Equal(t, "tpl-1", got.TemplateID)
	require.Equal(t, "/data", got.VolumeMounts[0].ContainerPath)
	require.Zero(t, got.Containers[0].CreateAt)
}

func TestApplyPauseBindingToInfoReadyWithoutSpecIsMinimal(t *testing.T) {
	const sandboxID = "sb-nospec"
	patches := patchInfoProxy(t, sandboxID)
	patches.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		return nil, sandboxspec.ErrSandboxSpecNotFound
	})
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, readyInfoRecord(sandboxID))
	require.True(t, filled)
	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	require.Equal(t, sandboxID, rsp.Data[0].SandboxID)
	require.Empty(t, rsp.Data[0].Labels)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), rsp.Data[0].Status)
}

func TestApplyPauseBindingToInfoCreatingOverRunningKeepsIdentity(t *testing.T) {
	const sandboxID = "sb-creating"
	patchInfoProxy(t, sandboxID)
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}, Data: []*types.SandboxData{{
		SandboxID: sandboxID,
		Status:    int32(cubebox.ContainerState_CONTAINER_RUNNING),
		Labels:    map[string]string{"app": "demo"},
		Containers: []*types.ContainerInfo{{
			ContainerID: sandboxID,
			Status:      int32(cubebox.ContainerState_CONTAINER_RUNNING),
		}},
	}}}
	rec := readyInfoRecord(sandboxID)
	rec.Status = pausesnap.StatusCreating
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, rec)
	require.True(t, filled)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSING), rsp.Data[0].Status)
	require.Equal(t, "demo", rsp.Data[0].Labels["app"])
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSING), rsp.Data[0].Containers[0].Status)
}

func TestApplyPauseBindingToInfoCreatingOverPausedKeepsNodeView(t *testing.T) {
	const sandboxID = "sb-creating-paused"
	patchInfoProxy(t, sandboxID)
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}, Data: []*types.SandboxData{{
		SandboxID: sandboxID,
		Status:    int32(cubebox.ContainerState_CONTAINER_PAUSED),
		Labels:    map[string]string{"app": "demo"},
	}}}
	rec := readyInfoRecord(sandboxID)
	rec.Status = pausesnap.StatusCreating
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, rec)
	require.False(t, filled)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), rsp.Data[0].Status)
	require.Equal(t, "snap-info", rsp.Data[0].Annotations[constants.CubeAnnotationPauseSnapshotID])
	require.Equal(t, "demo", rsp.Data[0].Labels["app"])
}

func TestApplyPauseBindingToInfoFailedOverUnknownKeepsIdentity(t *testing.T) {
	const sandboxID = "sb-failed"
	patchInfoProxy(t, sandboxID)
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}, Data: []*types.SandboxData{{
		SandboxID: sandboxID,
		Status:    int32(cubebox.ContainerState_CONTAINER_UNKNOWN),
		Labels:    map[string]string{"app": "demo"},
		Containers: []*types.ContainerInfo{{
			ContainerID: sandboxID,
			Status:      int32(cubebox.ContainerState_CONTAINER_UNKNOWN),
		}},
	}}}
	rec := readyInfoRecord(sandboxID)
	rec.Status = pausesnap.StatusFailed
	rec.LastError = "freeze failed"
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, rec)
	require.True(t, filled)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_UNKNOWN), rsp.Data[0].Status)
	require.Equal(t, "demo", rsp.Data[0].Labels["app"])
	require.Equal(t, "freeze failed", rsp.Data[0].Annotations[constants.CubeAnnotationPauseError])
}

func TestApplyPauseBindingToInfoDeleteFailedWithoutNodeRecordIsPaused(t *testing.T) {
	const sandboxID = "sb-delete-failed"
	patchInfoProxy(t, sandboxID)
	rec := readyInfoRecord(sandboxID)
	rec.Status = pausesnap.StatusDeleteFailed
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, rec)
	require.True(t, filled)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), rsp.Data[0].Status)
}

func TestApplyPauseBindingToInfoPinnedWrongHostDoesNotSynthesize(t *testing.T) {
	const sandboxID = "sb-wrong-host"
	rec := readyInfoRecord(sandboxID)
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{
		SandboxID: sandboxID,
		HostID:    "node-other",
	}, rsp, rec)
	require.False(t, filled)
	require.Empty(t, rsp.Data)
}

func TestApplyPauseBindingToInfoPinnedMatchingHostSynthesizes(t *testing.T) {
	const sandboxID = "sb-right-host"
	patchInfoProxy(t, sandboxID)
	rec := readyInfoRecord(sandboxID)
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{
		SandboxID: sandboxID,
		HostID:    rec.NodeID,
	}, rsp, rec)
	require.True(t, filled)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), rsp.Data[0].Status)
	require.Equal(t, rec.NodeID, rsp.Data[0].HostID)
}

func TestApplyPauseBindingToInfoDoesNotRequireProxyMap(t *testing.T) {
	const sandboxID = "sb-noproxy"
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return nil, false
	})
	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := applyPauseBindingToInfo(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, rsp, readyInfoRecord(sandboxID))
	require.True(t, filled)
	require.Equal(t, "10.0.0.1", rsp.Data[0].HostIP)
	require.Equal(t, "node-a", rsp.Data[0].HostID)
}

func TestCheckValidAndGetReqFallsBackToPauseBinding(t *testing.T) {
	const sandboxID = "sb-locate"
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return nil, false
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return readyInfoRecord(sandboxID), nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{IP: ip, InsID: "node-a", Healthy: true}, true
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP + ":50051" })

	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	endpoint, rec, ok := checkValidAndGetReq(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, &cubebox.ListCubeSandboxRequest{}, rsp)
	require.True(t, ok)
	require.Equal(t, "10.0.0.1:50051", endpoint)
	require.Equal(t, sandboxID, rec.SandboxID)
}

func TestCheckValidAndGetReqPrefersCacheOverBinding(t *testing.T) {
	const sandboxID = "sb-cache-wins"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "10.2.0.2"})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		rec := readyInfoRecord(sandboxID)
		rec.NodeIP = "10.1.0.1"
		return rec, nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{IP: ip, InsID: "node", Healthy: true}, true
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP + ":50051" })

	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	endpoint, _, ok := checkValidAndGetReq(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, &cubebox.ListCubeSandboxRequest{}, rsp)
	require.True(t, ok)
	require.Equal(t, "10.2.0.2:50051", endpoint)
}

func TestCheckValidAndGetReqServesShimlessPauseWhenNodeUnhealthy(t *testing.T) {
	const sandboxID = "sb-unhealthy-bind"
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return &basetypes.SandboxProxyMap{HostIP: "10.0.0.1"}, true
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return readyInfoRecord(sandboxID), nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{IP: ip, Healthy: false}, true
	})

	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	endpoint, rec, ok := checkValidAndGetReq(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, &cubebox.ListCubeSandboxRequest{}, rsp)
	require.True(t, ok)
	require.Empty(t, endpoint)
	require.NotNil(t, rec)
}

func TestCheckValidAndGetReqKeepsUnhealthyErrorOtherwise(t *testing.T) {
	const sandboxID = "sb-unhealthy-live"
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return &basetypes.SandboxProxyMap{HostIP: "10.0.0.1"}, true
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		rec := readyInfoRecord(sandboxID)
		rec.Status = pausesnap.StatusCreating
		return rec, nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{IP: ip, Healthy: false}, true
	})

	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	_, _, ok := checkValidAndGetReq(context.Background(), &types.GetCubeSandboxReq{SandboxID: sandboxID}, &cubebox.ListCubeSandboxRequest{}, rsp)
	require.False(t, ok)
	require.Equal(t, int(errorcode.ErrorCode_CubeletUnHealthy), rsp.Ret.RetCode)

	rsp = &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	patches.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) {
		return &node.Node{IP: "10.0.0.1", Healthy: false}, true
	})
	_, _, ok = checkValidAndGetReq(context.Background(), &types.GetCubeSandboxReq{
		SandboxID: sandboxID,
		HostID:    "node-a",
	}, &cubebox.ListCubeSandboxRequest{}, rsp)
	require.False(t, ok)
	require.Equal(t, int(errorcode.ErrorCode_CubeletUnHealthy), rsp.Ret.RetCode)
}

func TestSandboxInfoUnhealthyNodeServesSpecView(t *testing.T) {
	const sandboxID = "sb-info-unhealthy"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.8"})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })
	var listed int
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return &basetypes.SandboxProxyMap{HostIP: "10.0.0.8", SandboxIP: "192.168.0.8"}, true
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return readyInfoRecord(sandboxID), nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) {
		return &node.Node{IP: "10.0.0.1", InsID: "node-a", Healthy: false}, true
	})
	patches.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		return specWithIdentity(), nil
	})
	patches.ApplyFunc(cubelet.List, func(context.Context, string, *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		listed++
		return nil, errors.New("should not be called")
	})

	ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: "req-unhealthy"})
	rsp := SandboxInfo(ctx, &types.GetCubeSandboxReq{RequestID: "req-unhealthy", SandboxID: sandboxID})
	require.Zero(t, listed)
	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), rsp.Data[0].Status)
	require.Equal(t, "demo", rsp.Data[0].Labels["app"])
}

func TestSandboxInfoCubeletErrorStillFails(t *testing.T) {
	const sandboxID = "sb-info-rpc"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.3"})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return &basetypes.SandboxProxyMap{HostIP: "10.0.0.3"}, true
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return readyInfoRecord(sandboxID), nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{IP: ip, InsID: "node", Healthy: true}, true
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP + ":50051" })
	patches.ApplyFunc(cubelet.List, func(context.Context, string, *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		return nil, errors.New("cubelet down")
	})

	ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: "req-rpc"})
	rsp := SandboxInfo(ctx, &types.GetCubeSandboxReq{RequestID: "req-rpc", SandboxID: sandboxID})
	require.Equal(t, int(errorcode.ErrorCode_ReqCubeAPIFailed), rsp.Ret.RetCode)
}
