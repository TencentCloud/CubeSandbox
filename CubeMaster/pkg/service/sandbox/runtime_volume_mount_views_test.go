// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestConfirmedRuntimeVolumeMountViews(t *testing.T) {
	db, req, volume := newRuntimeAttachFixture(t, 0)
	ctx := CubeLog.WithRequestTrace(t.Context(), &CubeLog.RequestTrace{})
	cfg := &config.Config{Common: &config.CommonConf{}, CubeletConf: &config.CubeletConf{}}
	previous := l
	require.NoError(t, Init(ctx, cfg))
	t.Cleanup(func() { l = previous })

	redisServer := miniredis.RunT(t)
	redis := wrapredis.GetRedisConnPoolWrap("test-confirmed-mount-views", &config.RedisConf{
		Nodes: redisServer.Addr(), MaxActive: 4, MaxIdle: 2, MaxRetry: 1,
	})
	t.Cleanup(func() { require.NoError(t, redis.RedisConnPool.Close()) })
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap { return redis })
	patches.ApplyFunc(config.GetConfig, func() *config.Config { return cfg })

	host := &node.Node{InsID: "test-runtime-mount-host", IP: "127.0.0.1", Healthy: true}
	const legacyID = "sb-legacy-mount-view"
	patches.ApplyFunc(localcache.GetNode, func(id string) (*node.Node, bool) { return host, id == host.ID() })
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) { return host, ip == host.IP })
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList { return node.NodeList{host} })
	patches.ApplyFunc(localcache.SetSandboxCache, func(string, *localcache.SandboxCache) {})
	patches.ApplyFunc(localcache.ListKnownSandboxIDs, func() []string { return []string{req.SandboxID, legacyID} })
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) { return nil, false })
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })

	nodeSandbox := func(id, name, path string) *cubebox.CubeSandbox {
		return &cubebox.CubeSandbox{Id: id, Containers: []*cubebox.Container{{
			Id: id, Type: "sandbox", State: cubebox.ContainerState_CONTAINER_RUNNING,
			VolumeMounts: []*cubebox.VolumeMounts{
				{Name: "rootfs", ContainerPath: "/"},
				{Name: name, ContainerPath: path, HostPath: "/node-only"},
			},
		}}}
	}
	nodeItems := []*cubebox.CubeSandbox{nodeSandbox(req.SandboxID, "work", "/work"), nodeSandbox(legacyID, "cache", "/cache")}
	var listErr error
	patches.ApplyFunc(cubelet.List, func(_ context.Context, _ string, request *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		if listErr != nil {
			return nil, listErr
		}
		reply := &cubebox.ListCubeSandboxResponse{}
		for _, item := range nodeItems {
			if request.GetId() == "" || request.GetId() == item.GetId() {
				reply.Items = append(reply.Items, item)
			}
		}
		return reply, nil
	})

	work := []*types.VolumeMountInfo{{Name: "work", ContainerPath: "/work"}}
	confirmed := []*types.VolumeMountInfo{{Name: "work", ContainerPath: "/work"}, {Name: req.VolumeID, ContainerPath: req.ContainerPath, Readonly: true}}
	legacy := []*types.VolumeMountInfo{{Name: "cache", ContainerPath: "/cache"}}
	checkViews := func(want []*types.VolumeMountInfo, status cubebox.ContainerState) {
		t.Helper()
		info := SandboxInfo(ctx, &types.GetCubeSandboxReq{SandboxID: req.SandboxID, HostID: host.ID()})
		require.Equal(t, int(errorcode.ErrorCode_Success), info.Ret.RetCode)
		require.Len(t, info.Data, 1)
		require.Equal(t, want, info.Data[0].VolumeMounts)
		require.Equal(t, int32(status), info.Data[0].Status)
		list := ListSandbox(ctx, &types.ListCubeSandboxReq{HostID: host.ID(), InstanceType: "cubebox"})
		require.Equal(t, int(errorcode.ErrorCode_Success), list.Ret.RetCode)
		rows := indexBriefs(list.Data)
		require.Contains(t, rows, req.SandboxID)
		require.Equal(t, want, rows[req.SandboxID].VolumeMounts)
		require.Equal(t, int32(status), rows[req.SandboxID].Status)
		if status == cubebox.ContainerState_CONTAINER_RUNNING {
			require.Contains(t, rows, legacyID)
			require.Equal(t, legacy, rows[legacyID].VolumeMounts)
		}
	}
	checkViews(work, cubebox.ContainerState_CONTAINER_RUNNING)

	rejected := req
	rejected.RequestID = "rejected-attach"
	require.ErrorIs(t, AttachRuntimeVolume(ctx, rejected, noActiveRuntimeAttachSnapshot, func(_ context.Context, binding RuntimeAttachBinding, _ models.VolumeRecord) (RuntimeAttachReply, error) {
		return RuntimeAttachReply{Binding: binding, Outcome: RuntimeAttachNotStarted}, nil
	}), ErrRuntimeAttachRejected)
	checkViews(work, cubebox.ContainerState_CONTAINER_RUNNING)

	var settledReply RuntimeAttachReply
	require.ErrorIs(t, AttachRuntimeVolume(ctx, req, noActiveRuntimeAttachSnapshot, func(_ context.Context, binding RuntimeAttachBinding, _ models.VolumeRecord) (RuntimeAttachReply, error) {
		checkViews(work, cubebox.ContainerState_CONTAINER_RUNNING)
		settledReply = RuntimeAttachReply{Binding: binding, Outcome: RuntimeAttachPublished, ExtInfo: runtimeAttachFirstReference(t, req.VolumeID)}
		return RuntimeAttachReply{}, errors.New("fixture lost reply")
	}), ErrRuntimeAttachUnresolved)
	checkViews(work, cubebox.ContainerState_CONTAINER_RUNNING)
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_mount BEFORE UPDATE OF runtime_volume_mounts ON t_cube_sandbox_spec
		BEGIN SELECT RAISE(ABORT, 'mount write failed'); END`).Error)
	require.ErrorIs(t, settleRuntimeAttach(ctx, db, req, settledReply), ErrRuntimeAttachUnresolved)
	assertRuntimeAttachState(t, db, req, volume.ID, 0, 1, false)
	checkViews(work, cubebox.ContainerState_CONTAINER_RUNNING)
	require.NoError(t, db.Exec("DROP TRIGGER reject_mount").Error)
	require.NoError(t, settleRuntimeAttach(ctx, db, req, settledReply))
	assertRuntimeAttachState(t, db, req, volume.ID, 1, 0, true)
	checkViews(confirmed, cubebox.ContainerState_CONTAINER_RUNNING)

	hostInfo := SandboxInfo(ctx, &types.GetCubeSandboxReq{HostID: host.ID()})
	require.Equal(t, int(errorcode.ErrorCode_Success), hostInfo.Ret.RetCode)
	require.Len(t, hostInfo.Data, 2)
	require.Equal(t, confirmed, findSandboxData(hostInfo.Data, req.SandboxID).VolumeMounts)
	require.Equal(t, legacy, findSandboxData(hostInfo.Data, legacyID).VolumeMounts)
	inventory := ListSandboxWithFailOnError(ctx, &types.ListCubeSandboxReq{HostID: host.ID(), InstanceType: "cubebox"})
	require.Equal(t, int(errorcode.ErrorCode_Success), inventory.Ret.RetCode)
	require.Equal(t, confirmed, indexBriefs(inventory.Data)[req.SandboxID].VolumeMounts)
	listErr = errors.New("fixture cubelet unavailable")
	inventory = ListSandboxWithFailOnError(ctx, &types.ListCubeSandboxReq{HostID: host.ID(), InstanceType: "cubebox"})
	require.Equal(t, int(errorcode.ErrorCode_ReqCubeAPIFailed), inventory.Ret.RetCode)
	require.Equal(t, []*types.SandboxBriefData{}, inventory.Data)

	listErr = nil
	nodeItems = nil
	require.NoError(t, db.Create(&models.PauseSnapshotRecord{
		SandboxID: req.SandboxID, SnapshotID: "snap-confirmed-mounts", NodeID: host.ID(), NodeIP: host.IP,
		InstanceType: "cubebox", Status: pausesnap.StatusReady,
	}).Error)
	checkViews(confirmed, cubebox.ContainerState_CONTAINER_PAUSED)
}

func TestMergeConfirmedVolumeMountsKeepsFirstBindingPosition(t *testing.T) {
	work := &types.VolumeMountInfo{Name: "work", ContainerPath: "/work"}
	old := &types.VolumeMountInfo{Name: "dataset", ContainerPath: "/data"}
	otherTarget := &types.VolumeMountInfo{Name: "dataset", ContainerPath: "/other"}
	otherName := &types.VolumeMountInfo{Name: "cache", ContainerPath: "/data"}
	duplicate := &types.VolumeMountInfo{Name: "dataset", ContainerPath: "/data", Readonly: true}
	existing := []*types.VolumeMountInfo{work, old, otherTarget, duplicate, otherName}
	confirmed := []sandboxspec.RuntimeVolumeMount{
		{VolumeID: "dataset", ContainerPath: "/data", Readonly: true},
		{VolumeID: "logs", ContainerPath: "/logs"},
	}
	require.Equal(t, []*types.VolumeMountInfo{work, duplicate, otherTarget, otherName, {Name: "logs", ContainerPath: "/logs"}},
		mergeConfirmedVolumeMounts(existing, confirmed))
	require.False(t, old.Readonly)
	require.Equal(t, existing, mergeConfirmedVolumeMounts(existing, nil))
}
