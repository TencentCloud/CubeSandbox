// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubeproxy"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/restoreplace"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	protoret "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestResumeProxyMapFailureStillCompletesBookkeeping(t *testing.T) {
	for _, target := range []string{"10.0.0.1", "10.0.0.2"} {
		t.Run(target, func(t *testing.T) {
			const sid = "sb-resume-map-failure"
			const origin = "10.0.0.1"
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			localcache.SetSandboxCache(sid, &localcache.SandboxCache{SandboxID: sid, HostIP: origin})
			defer localcache.DeleteSandboxCache(sid)
			patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
				return &pausesnap.Record{SnapshotID: "snap", Status: "READY", NodeIP: origin}, nil
			})
			patches.ApplyFunc(loadResumeSandboxSpec, func(context.Context, string) *types.CreateCubeSandboxReq { return nil })
			patches.ApplyFunc(resumePlacement, func(context.Context, *pausesnap.Record, string, *types.CreateCubeSandboxReq) (*restoreplace.Placement, error) {
				return &restoreplace.Placement{NodeIP: target, CrossNode: target != origin}, nil
			})
			patches.ApplyFunc(cubelet.Create, func(_ context.Context, endpoint string, _ *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
				if !strings.Contains(endpoint, target) {
					t.Fatalf("restore sent to wrong node: %s", endpoint)
				}
				return &cubebox.RunCubeSandboxResponse{SandboxID: sid, SandboxIP: "192.168.1.62", Ret: &protoret.Ret{RetCode: 200}}, nil
			})
			origGet, origSet := getSandboxProxyMapFn, setSandboxProxyMapFn
			defer func() { getSandboxProxyMapFn, setSandboxProxyMapFn = origGet, origSet }()
			getSandboxProxyMapFn = func(context.Context, string) (*proxytypes.SandboxProxyMap, bool) { return nil, false }
			setSandboxProxyMapFn = func(_ context.Context, proxy *proxytypes.SandboxProxyMap) error {
				if proxy.HostIP != target {
					t.Fatalf("wrong mapping target: %s", proxy.HostIP)
				}
				return errors.New("injected mapping write failure")
			}
			var purged, cleaned, deleted, marked, published bool
			patches.ApplyFunc(cubeproxy.InvalidateBackendCache, func(_ context.Context, id, host string) error {
				purged = id == sid && host == target
				return nil
			})
			patches.ApplyFunc(pausesnap.DropOriginTombstone, func(_ context.Context, _, id, host, _, _ string) error {
				cleaned = target != origin && id == sid && host == origin
				return nil
			})
			patches.ApplyFunc(pausesnap.CleanupPauseSnapshot, func(_ context.Context, _, host, _, _ string) error {
				cleaned = target == origin && host == target
				return nil
			})
			patches.ApplyFunc(pausesnap.Delete, func(_ context.Context, id string) error { deleted = id == "snap"; return nil })
			patches.ApplyFunc(markResumedLifecycleState, func(id string) error { marked = id == sid; return nil })
			patches.ApplyFunc(runAfterUpdateSandboxSuccessHook, func(_ context.Context, id, _, action, _ string) {
				published = marked && id == sid && action == "resume"
			})
			rsp := resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: sid, InstanceType: "cubebox", Action: "resume"}, origin)
			if !rsp.ResumeCompleted || rsp.Ret.RetCode != int(errorcode.ErrorCode_DBError) || !strings.Contains(rsp.Ret.RetMsg, "injected mapping write failure") {
				t.Fatalf("lost partial completion or mapping error: %+v ret=%+v", rsp, rsp.Ret)
			}
			if !purged || !cleaned || !deleted || !marked || !published {
				t.Fatalf("bookkeeping skipped: purge=%v cleanup=%v delete=%v marker=%v event=%v", purged, cleaned, deleted, marked, published)
			}
			if cache := localcache.GetSandboxCache(sid); cache == nil || cache.HostIP != target {
				t.Fatalf("local placement still points at pre-resume node: %+v", cache)
			}
		})
	}
}

func TestRefreshProxyMapAfterResumeRewritesSandboxIP(t *testing.T) {
	origGet := getSandboxProxyMapFn
	origSet := setSandboxProxyMapFn
	defer func() {
		getSandboxProxyMapFn = origGet
		setSandboxProxyMapFn = origSet
	}()

	prev := &proxytypes.SandboxProxyMap{
		HostIP:             "10.0.0.1",
		SandboxID:          "sb-1",
		SandboxIP:          "192.168.1.10",
		SandboxPort:        "8080",
		CreatedAt:          "111",
		AllowPublicTraffic: false,
		TrafficAccessToken: "tok-keep",
		MaskRequestHost:    "localhost:${PORT}",
		ContainerToHostPorts: map[string]string{
			"49999": "20002",
		},
	}
	getSandboxProxyMapFn = func(_ context.Context, sandboxID string) (*proxytypes.SandboxProxyMap, bool) {
		if sandboxID != "sb-1" {
			return nil, false
		}
		return prev, true
	}

	var stored *proxytypes.SandboxProxyMap
	setSandboxProxyMapFn = func(_ context.Context, proxy *proxytypes.SandboxProxyMap) error {
		stored = proxy
		return nil
	}

	cfg := config.GetConfig()
	if cfg == nil {
		t.Fatal("config not initialized")
	}
	prevEnable := false
	if cfg.CubeletConf != nil {
		prevEnable = cfg.CubeletConf.EnableExposedPort
		cfg.CubeletConf.EnableExposedPort = true
		defer func() { cfg.CubeletConf.EnableExposedPort = prevEnable }()
	}

	err := refreshProxyMapAfterResume(context.Background(), "sb-1", "10.0.0.1", &cubebox.RunCubeSandboxResponse{
		SandboxIP: "192.168.1.62",
		PortMappings: []*cubebox.PortMapping{
			{ContainerPort: 49999, HostPort: 20010},
		},
	})
	if err != nil {
		t.Fatalf("refreshProxyMapAfterResume: %v", err)
	}
	if stored == nil {
		t.Fatal("expected proxy rewrite")
	}
	if stored.SandboxIP != "192.168.1.62" {
		t.Fatalf("SandboxIP=%q, want 192.168.1.62", stored.SandboxIP)
	}
	if stored.HostIP != "10.0.0.1" {
		t.Fatalf("HostIP=%q, want 10.0.0.1", stored.HostIP)
	}
	if stored.TrafficAccessToken != "tok-keep" || stored.MaskRequestHost != "localhost:${PORT}" || stored.AllowPublicTraffic {
		t.Fatalf("traffic policy not preserved: %+v", stored)
	}
	if stored.CreatedAt != "111" {
		t.Fatalf("CreatedAt=%q, want 111", stored.CreatedAt)
	}
	if stored.ContainerToHostPorts["49999"] != "20010" {
		t.Fatalf("ports=%v, want 49999->20010", stored.ContainerToHostPorts)
	}
}

func TestRefreshProxyMapAfterResumeRequiresSandboxIP(t *testing.T) {
	origGet := getSandboxProxyMapFn
	origSet := setSandboxProxyMapFn
	defer func() {
		getSandboxProxyMapFn = origGet
		setSandboxProxyMapFn = origSet
	}()
	getSandboxProxyMapFn = func(context.Context, string) (*proxytypes.SandboxProxyMap, bool) { return nil, false }
	setSandboxProxyMapFn = func(context.Context, *proxytypes.SandboxProxyMap) error {
		t.Fatal("must not write without SandboxIP")
		return nil
	}
	err := refreshProxyMapAfterResume(context.Background(), "sb-1", "10.0.0.1", &cubebox.RunCubeSandboxResponse{})
	if err == nil {
		t.Fatal("expected error for missing SandboxIP")
	}
}
