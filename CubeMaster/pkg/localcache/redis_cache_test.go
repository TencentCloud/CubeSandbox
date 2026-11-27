// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package localcache

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
)

var (
	testMiniredisOnce sync.Once
	testMiniredis     *miniredis.Miniredis
	testMiniredisErr  error
)

// ensureTestMiniredis starts a single process-wide miniredis instance because
// wrapredis.GetRedis() caches its connection pool by process singleton.
func ensureTestMiniredis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	testMiniredisOnce.Do(func() {
		testMiniredis, testMiniredisErr = miniredis.Run()
		if testMiniredisErr != nil {
			return
		}
		cfg := config.GetConfig()
		cfg.RedisConf = &config.RedisConf{
			Nodes:       testMiniredis.Addr(),
			MaxActive:   4,
			MaxIdle:     1,
			MaxRetry:    1,
			DbNo:        0,
			IdleTimeout: 30,
		}
	})
	if testMiniredisErr != nil {
		t.Fatalf("start miniredis: %v", testMiniredisErr)
	}
	testMiniredis.FlushAll()
	return testMiniredis
}

func TestSandboxProxyMapMaskRequestHostRoundTrip(t *testing.T) {
	ensureTestMiniredis(t)

	cache := &local{}
	key := "test:sandbox:proxy:sandbox-1"
	want := &proxytypes.SandboxProxyMap{
		HostIP:             "10.0.0.1",
		SandboxIP:          "192.168.0.2",
		CreatedAt:          "123",
		AllowPublicTraffic: true,
		MaskRequestHost:    "localhost:${PORT}",
		ContainerToHostPorts: map[string]string{
			"3000": "23000",
		},
	}

	if err := cache.setByPassProsyToRedis(context.Background(), key, want); err != nil {
		t.Fatalf("setByPassProsyToRedis failed: %v", err)
	}
	got, err := cache.getByPassProsyFromRedis(context.Background(), key)
	if err != nil {
		t.Fatalf("getByPassProsyFromRedis failed: %v", err)
	}
	if got.MaskRequestHost != want.MaskRequestHost {
		t.Fatalf("MaskRequestHost=%q, want %q", got.MaskRequestHost, want.MaskRequestHost)
	}
	if len(got.ContainerToHostPorts) != 1 || got.ContainerToHostPorts["3000"] != "23000" {
		t.Fatalf("unexpected ContainerToHostPorts: %#v", got.ContainerToHostPorts)
	}
	if _, leaked := got.ContainerToHostPorts["MaskRequestHost"]; leaked {
		t.Fatal("MaskRequestHost leaked into ContainerToHostPorts")
	}
}

func TestSandboxProxyMapHostIDRoundTripAndLiveResolution(t *testing.T) {
	setupIsolatedNodeCache(t)
	ensureTestMiniredis(t)

	UpsertNode(&node.Node{
		InsID:   "k8s-node-proxy-1",
		IP:      "10.3.0.10",
		Healthy: true,
	})

	want := &proxytypes.SandboxProxyMap{
		HostID:             "k8s-node-proxy-1",
		HostIP:             "10.3.0.10",
		SandboxID:          "sandbox-hostid-1",
		SandboxIP:          "192.168.0.2",
		CreatedAt:          "123",
		AllowPublicTraffic: true,
		ContainerToHostPorts: map[string]string{
			"8080": "28080",
		},
	}

	if err := SetSandboxProxyMap(context.Background(), want); err != nil {
		t.Fatalf("SetSandboxProxyMap failed: %v", err)
	}

	// Simulate BigPod recreate with a new Pod IP under the same NodeID.
	UpsertNode(&node.Node{
		InsID:   "k8s-node-proxy-1",
		IP:      "10.3.0.99",
		Healthy: true,
	})

	got, ok := GetSandboxProxyMap(context.Background(), "sandbox-hostid-1")
	if !ok || got == nil {
		t.Fatal("expected GetSandboxProxyMap hit")
	}
	if got.HostID != "k8s-node-proxy-1" {
		t.Fatalf("HostID=%q, want k8s-node-proxy-1", got.HostID)
	}
	if got.HostIP != "10.3.0.99" {
		t.Fatalf("HostIP=%q, want 10.3.0.99 (resolved live IP)", got.HostIP)
	}
	if _, leaked := got.ContainerToHostPorts["HostID"]; leaked {
		t.Fatal("HostID leaked into ContainerToHostPorts")
	}
	if len(got.ContainerToHostPorts) != 1 || got.ContainerToHostPorts["8080"] != "28080" {
		t.Fatalf("unexpected ContainerToHostPorts: %#v", got.ContainerToHostPorts)
	}

	// Rewriting the same sandbox without HostID must clear the old HostID so a
	// cross-node move cannot keep resolving back to the previous node.
	rewritten := &proxytypes.SandboxProxyMap{
		HostIP:             "10.4.0.77",
		SandboxID:          "sandbox-hostid-1",
		SandboxIP:          "192.168.0.3",
		CreatedAt:          "123",
		AllowPublicTraffic: true,
		ContainerToHostPorts: map[string]string{
			"8080": "28081",
		},
	}
	if err := SetSandboxProxyMap(context.Background(), rewritten); err != nil {
		t.Fatalf("rewrite SetSandboxProxyMap failed: %v", err)
	}
	got2, ok := GetSandboxProxyMap(context.Background(), "sandbox-hostid-1")
	if !ok || got2 == nil {
		t.Fatal("expected GetSandboxProxyMap hit after rewrite")
	}
	if got2.HostID != "" {
		t.Fatalf("stale HostID survived rewrite: %q", got2.HostID)
	}
	if got2.HostIP != "10.4.0.77" {
		t.Fatalf("HostIP=%q after rewrite, want 10.4.0.77", got2.HostIP)
	}
}
