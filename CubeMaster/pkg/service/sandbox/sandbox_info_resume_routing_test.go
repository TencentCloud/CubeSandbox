// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
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

const (
	resumeInfoSourceIP = "10.0.0.92"
	resumeInfoTargetIP = "10.0.0.110"
)

type resumeInfoListResult struct {
	response *cubebox.ListCubeSandboxResponse
	err      error
}

type resumeInfoFixture struct {
	t             *testing.T
	sandboxID     string
	sharedHost    string
	sharedPresent bool
	listResults   map[string]resumeInfoListResult
	listCalls     []string
	proxyReads    int
}

func newResumeInfoFixture(t *testing.T, sandboxID, sharedHost string, sharedPresent bool,
	rec *pausesnap.Record, listResults map[string]resumeInfoListResult) *resumeInfoFixture {
	t.Helper()
	f := &resumeInfoFixture{
		t:             t,
		sandboxID:     sandboxID,
		sharedHost:    sharedHost,
		sharedPresent: sharedPresent,
		listResults:   listResults,
	}
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    resumeInfoSourceIP,
	})
	t.Cleanup(func() { localcache.DeleteSandboxCache(sandboxID) })

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(_ context.Context, got string) (*basetypes.SandboxProxyMap, bool) {
		require.Equal(t, sandboxID, got)
		f.proxyReads++
		if !f.sharedPresent {
			return nil, false
		}
		return &basetypes.SandboxProxyMap{
			SandboxID: sandboxID,
			HostIP:    f.sharedHost,
			SandboxIP: "192.168.0.110",
		}, true
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(_ context.Context, got string) (*pausesnap.Record, error) {
		require.Equal(t, sandboxID, got)
		if rec == nil {
			return nil, pausesnap.ErrNotFound
		}
		return rec, nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		switch ip {
		case resumeInfoSourceIP:
			return &node.Node{IP: ip, InsID: "node-source", Healthy: true}, true
		case resumeInfoTargetIP:
			return &node.Node{IP: ip, InsID: "node-target", Healthy: true}, true
		default:
			return nil, false
		}
	})
	patches.ApplyFunc(localcache.GetNode, func(id string) (*node.Node, bool) {
		if id == "node-source" {
			return &node.Node{IP: resumeInfoSourceIP, InsID: id, Healthy: true}, true
		}
		if id == "node-target" {
			return &node.Node{IP: resumeInfoTargetIP, InsID: id, Healthy: true}, true
		}
		return nil, false
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string {
		return hostIP + ":50051"
	})
	patches.ApplyFunc(cubelet.List, func(_ context.Context, endpoint string, req *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		require.NotNil(t, req)
		require.Equal(t, sandboxID, req.GetId())
		f.listCalls = append(f.listCalls, endpoint)
		if result, ok := f.listResults[endpoint]; ok {
			return result.response, result.err
		}
		return &cubebox.ListCubeSandboxResponse{}, nil
	})
	patches.ApplyFunc(sandboxspec.GetAnnotations, func(context.Context, string) (map[string]string, error) {
		return nil, sandboxspec.ErrSandboxSpecNotFound
	})
	patches.ApplyFunc(LookupSandboxEndAt, func(context.Context, string) int64 { return 0 })

	return f
}

func (f *resumeInfoFixture) info(hostID string) *types.GetCubeSandboxRes {
	f.t.Helper()
	requestID := "req-" + f.sandboxID
	ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: requestID})
	return SandboxInfo(ctx, &types.GetCubeSandboxReq{
		RequestID: requestID,
		SandboxID: f.sandboxID,
		HostID:    hostID,
	})
}

func resumeInfoRunning(sandboxID string) *cubebox.ListCubeSandboxResponse {
	return resumeInfoWithState(sandboxID, cubebox.ContainerState_CONTAINER_RUNNING)
}

func resumeInfoWithState(sandboxID string, state cubebox.ContainerState) *cubebox.ListCubeSandboxResponse {
	return &cubebox.ListCubeSandboxResponse{Items: []*cubebox.CubeSandbox{{
		Id: sandboxID,
		Containers: []*cubebox.Container{{
			Id:    sandboxID,
			State: state,
		}},
	}}}
}

func resumeInfoEndpoint(hostIP string) string {
	return fmt.Sprintf("%s:50051", hostIP)
}

func TestSandboxInfoRetriesSharedProxyOnlyAfterUnpinnedEmptyResult(t *testing.T) {
	const sandboxID = "sb-info-resumed-routing"
	f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, nil, map[string]resumeInfoListResult{
		resumeInfoEndpoint(resumeInfoSourceIP): {response: &cubebox.ListCubeSandboxResponse{}},
		resumeInfoEndpoint(resumeInfoTargetIP): {response: resumeInfoRunning(sandboxID)},
	})

	rsp := f.info("")
	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	require.Len(t, rsp.Data, 1)
	require.Equal(t, sandboxID, rsp.Data[0].SandboxID)
	require.Equal(t, int32(cubebox.ContainerState_CONTAINER_RUNNING), rsp.Data[0].Status)
	require.Equal(t, resumeInfoTargetIP, rsp.Data[0].HostIP)
	require.Equal(t, "node-target", rsp.Data[0].HostID)
	require.Equal(t, []string{
		resumeInfoEndpoint(resumeInfoSourceIP),
		resumeInfoEndpoint(resumeInfoTargetIP),
	}, f.listCalls, "source and shared proxy must be queried in order, exactly once each")
	require.NotNil(t, localcache.GetSandboxCache(sandboxID))
	require.Equal(t, resumeInfoTargetIP, localcache.GetSandboxCache(sandboxID).HostIP)
}

func TestSandboxInfoResumeRoutingBoundaries(t *testing.T) {
	t.Run("exact target sandbox preserves its reported non-running state", func(t *testing.T) {
		const sandboxID = "sb-info-target-exited"
		f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, nil, map[string]resumeInfoListResult{
			resumeInfoEndpoint(resumeInfoSourceIP): {response: &cubebox.ListCubeSandboxResponse{}},
			resumeInfoEndpoint(resumeInfoTargetIP): {response: resumeInfoWithState(sandboxID, cubebox.ContainerState_CONTAINER_EXITED)},
		})

		rsp := f.info("")
		require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
		require.Len(t, rsp.Data, 1)
		require.Equal(t, int32(cubebox.ContainerState_CONTAINER_EXITED), rsp.Data[0].Status)
		require.Equal(t, []string{
			resumeInfoEndpoint(resumeInfoSourceIP),
			resumeInfoEndpoint(resumeInfoTargetIP),
		}, f.listCalls)
		require.Equal(t, resumeInfoTargetIP, localcache.GetSandboxCache(sandboxID).HostIP)
	})

	t.Run("explicit source host is never rerouted", func(t *testing.T) {
		const sandboxID = "sb-info-pinned-source"
		f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, nil, nil)
		rsp := f.info("node-source")
		require.Equal(t, int(errorcode.ErrorCode_NotFoundAtCubelet), rsp.Ret.RetCode)
		require.Equal(t, []string{resumeInfoEndpoint(resumeInfoSourceIP)}, f.listCalls)
		require.Equal(t, resumeInfoSourceIP, localcache.GetSandboxCache(sandboxID).HostIP)
	})

	t.Run("empty source and target do not correct cache", func(t *testing.T) {
		const sandboxID = "sb-info-both-empty"
		f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, nil, nil)
		rsp := f.info("")
		require.Equal(t, int(errorcode.ErrorCode_NotFoundAtCubelet), rsp.Ret.RetCode)
		require.Empty(t, rsp.Data)
		require.Equal(t, []string{
			resumeInfoEndpoint(resumeInfoSourceIP),
			resumeInfoEndpoint(resumeInfoTargetIP),
		}, f.listCalls)
		require.Equal(t, resumeInfoSourceIP, localcache.GetSandboxCache(sandboxID).HostIP)
	})

	for _, tc := range []struct {
		name          string
		sharedHost    string
		sharedPresent bool
	}{
		{name: "shared proxy names same host", sharedHost: resumeInfoSourceIP, sharedPresent: true},
		{name: "shared proxy missing", sharedPresent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandboxID := "sb-info-no-extra-" + tc.name
			f := newResumeInfoFixture(t, sandboxID, tc.sharedHost, tc.sharedPresent, nil, nil)
			rsp := f.info("")
			require.Equal(t, int(errorcode.ErrorCode_NotFoundAtCubelet), rsp.Ret.RetCode)
			require.Equal(t, []string{resumeInfoEndpoint(resumeInfoSourceIP)}, f.listCalls)
			require.Equal(t, 1, f.proxyReads)
			require.Equal(t, resumeInfoSourceIP, localcache.GetSandboxCache(sandboxID).HostIP)
		})
	}

	t.Run("initial transport error is preserved", func(t *testing.T) {
		const sandboxID = "sb-info-source-error"
		f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, nil, map[string]resumeInfoListResult{
			resumeInfoEndpoint(resumeInfoSourceIP): {err: errors.New("source transport failed")},
		})
		rsp := f.info("")
		require.Equal(t, int(errorcode.ErrorCode_ReqCubeAPIFailed), rsp.Ret.RetCode)
		require.Equal(t, []string{resumeInfoEndpoint(resumeInfoSourceIP)}, f.listCalls)
		require.Equal(t, resumeInfoSourceIP, localcache.GetSandboxCache(sandboxID).HostIP)
	})

	t.Run("target transport error is not converted to not found", func(t *testing.T) {
		const sandboxID = "sb-info-target-error"
		f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, nil, map[string]resumeInfoListResult{
			resumeInfoEndpoint(resumeInfoSourceIP): {response: &cubebox.ListCubeSandboxResponse{}},
			resumeInfoEndpoint(resumeInfoTargetIP): {err: errors.New("target transport failed")},
		})
		rsp := f.info("")
		require.Equal(t, int(errorcode.ErrorCode_ReqCubeAPIFailed), rsp.Ret.RetCode)
		require.Equal(t, []string{
			resumeInfoEndpoint(resumeInfoSourceIP),
			resumeInfoEndpoint(resumeInfoTargetIP),
		}, f.listCalls)
		require.Equal(t, resumeInfoSourceIP, localcache.GetSandboxCache(sandboxID).HostIP)
	})

	t.Run("other sandbox id on target is not accepted", func(t *testing.T) {
		const sandboxID = "sb-info-target-wrong-id"
		f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, nil, map[string]resumeInfoListResult{
			resumeInfoEndpoint(resumeInfoSourceIP): {response: &cubebox.ListCubeSandboxResponse{}},
			resumeInfoEndpoint(resumeInfoTargetIP): {response: resumeInfoRunning("sb-unrelated")},
		})
		rsp := f.info("")
		require.Equal(t, int(errorcode.ErrorCode_NotFoundAtCubelet), rsp.Ret.RetCode)
		require.Empty(t, rsp.Data)
		require.Equal(t, []string{
			resumeInfoEndpoint(resumeInfoSourceIP),
			resumeInfoEndpoint(resumeInfoTargetIP),
		}, f.listCalls)
		require.Equal(t, resumeInfoSourceIP, localcache.GetSandboxCache(sandboxID).HostIP)
	})

	t.Run("ready pause binding keeps source view", func(t *testing.T) {
		const sandboxID = "sb-info-ready-binding"
		rec := readyInfoRecord(sandboxID)
		rec.NodeID = "node-source"
		rec.NodeIP = resumeInfoSourceIP
		f := newResumeInfoFixture(t, sandboxID, resumeInfoTargetIP, true, rec, nil)
		patches := gomonkey.NewPatches()
		t.Cleanup(patches.Reset)
		patches.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
			return nil, sandboxspec.ErrSandboxSpecNotFound
		})
		patches.ApplyFunc(sandboxspec.GetCreatedAt, func(context.Context, string) (time.Time, error) {
			return time.Time{}, sandboxspec.ErrSandboxSpecNotFound
		})

		rsp := f.info("")
		require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
		require.Len(t, rsp.Data, 1)
		require.Equal(t, int32(cubebox.ContainerState_CONTAINER_PAUSED), rsp.Data[0].Status)
		require.Equal(t, []string{resumeInfoEndpoint(resumeInfoSourceIP)}, f.listCalls)
		require.Equal(t, resumeInfoSourceIP, localcache.GetSandboxCache(sandboxID).HostIP)
	})
}
