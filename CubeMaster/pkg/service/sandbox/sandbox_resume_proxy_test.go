// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

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
